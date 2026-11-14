import json
from pathlib import Path
import socket
import tempfile
import threading
import time
import unittest
from unittest.mock import patch

from scripts.ops.core import OpsError, validate_selection
from scripts.ops.engine import Engine
from scripts.ops.plans import routing_fingerprint
from scripts.ops.report import write as report_write
from scripts.ops.security import Vault
from scripts.ops.store import Lease, ProcessLock, Store
from scripts.ops.tests.fakes import COMMIT, Clock, Driver, Releases, Routes, Adapter, baseline


class EngineTests(unittest.TestCase):
    def setUp(self):
        self.network = patch.object(socket, 'create_connection', side_effect=AssertionError('Offline tests must not connect'))
        self.network.start()
        self.temporary = tempfile.TemporaryDirectory()
        self.root = Path(self.temporary.name)
        self.store = Store(self.root, Vault(lambda value, decrypt: value))
        self.lease = Lease(self.root / 'global-lease.json', self.root / 'hot-lease.json')
        self.driver, self.clock, self.releases = Driver(), Clock(), Releases()

    def tearDown(self):
        self.network.stop()
        self.temporary.cleanup()

    def start(self, *, deferrals=None, fast_model_tests=None, acceptance_mode='full', target_options=None):
        targets = validate_selection(self.driver.inventory, list(self.driver.inventory['targets']))
        exceptions = {'target_options': target_options} if target_options is not None else {
            'operator_deferrals': deferrals, 'fast_model_tests': fast_model_tests}
        plan = self.store.plan({'schema': 2 if target_options is not None else 1,
                               'targets': targets, 'release': {'commit': COMMIT, 'page_sha256': 'new-page'},
                               'inventory_fingerprint': self.driver.inventory_fingerprint(),
                               **exceptions,
                               **({'acceptance_mode': acceptance_mode} if acceptance_mode is not None else {})})
        self.run_id = self.store.begin(plan)
        self.engine = Engine(self.store, self.lease, self.driver, self.releases,
                             clock=self.clock, monotonic=self.clock, sleep=self.clock.sleep)
        return self.engine.run(self.run_id)

    def test_success_is_serial_and_preserves_disabled_channels(self):
        run = self.start()
        self.assertEqual(run['status'], 'complete')
        self.assertEqual(self.releases.calls, 1)
        self.assertEqual(self.driver.channels, {'one': {1: 1, 2: 2, 3: 3}, 'two': {1: 1, 2: 2, 3: 3}})
        self.assertLess(self.driver.calls.index(('complete', 'one', False)), self.driver.calls.index(('stop', 'two')))
        self.assertFalse(self.lease.path.exists())
        self.assertEqual(run['targets'][0]['data']['drain']['criterion'], 'fallback_wait_limit_180_seconds')
        self.assertFalse(run['targets'][0]['data']['drain']['completion_verified'])

    def test_next_only_is_prepared_while_current_drains_and_is_rechecked_after_release(self):
        self.driver = Driver(('one', 'two', 'three'))
        calls = self.driver.calls
        original_wait, original_preflight = self.clock.sleep, Adapter.preflight
        prepared = threading.Event()

        def prepare(target, release, options):
            label = target['label']
            calls.append(('prefetch', label))
            return {'target': baseline(label), 'backup': {'config': 'synthetic'}, 'prepared_at': time.time()}

        def wait(seconds):
            if self.driver.channels['one'][1] == 2 and not prepared.is_set():
                self.assertTrue(self.engine.lookahead.future.result(timeout=5))
                self.assertIn(('prefetch', 'two'), calls)
                self.assertNotIn(('prefetch', 'three'), calls)
                prepared.set()
            original_wait(seconds)

        def preflight(adapter, release, **options):
            if adapter.label == 'two':
                self.assertTrue(options['reuse'])
                self.assertIn(('complete', 'one', False), calls)
            return original_preflight(adapter, release, **options)

        self.driver.prepare, self.clock.sleep = prepare, wait
        with patch.object(Adapter, 'preflight', preflight):
            run = self.start(acceptance_mode='availability', target_options={})
        self.assertEqual(run['status'], 'complete')
        self.assertLess(calls.index(('prefetch', 'two')), calls.index(('deploy', 'one')))
        self.assertLess(calls.index(('complete', 'one', False)), calls.index(('stage', 'two')))
        self.assertLess(calls.index(('stop', 'two')), calls.index(('prefetch', 'three')))
        self.assertLess(calls.index(('complete', 'two', False)), calls.index(('stage', 'three')))

    def test_next_preflight_failure_stops_queue_only_after_restoring_current(self):
        def failure(target, release, options):
            self.driver.calls.append(('prefetch', target['label']))
            raise OpsError('oauth_application_secret_missing', category='authorization')

        self.driver.prepare = failure
        run = self.start(acceptance_mode='availability', target_options={})
        self.assertEqual(run['status'], 'blocked')
        self.assertEqual(run['data']['reason'], 'background_preflight_failed')
        self.assertEqual([item['phase'] for item in run['targets']], ['complete', 'pending'])
        self.assertIn(('restore', 'one'), self.driver.calls)
        self.assertNotIn(('stage', 'two'), self.driver.calls)
        self.assertEqual(self.driver.channels['one'], {1: 1, 2: 2, 3: 3})
        self.assertFalse(self.lease.path.exists())
        report = json.loads(Path(report_write(self.store, self.run_id)['json']).read_text(encoding='utf-8'))
        self.assertEqual(report['preparation_failures'], [{'target': 'two', 'code': 'oauth_application_secret_missing'}])

    def test_schema_two_exceptions_stay_with_their_target_and_preserve_current_disabled_status(self):
        # Keep a second live channel so the fake can exercise models on both targets.
        self.driver.channels['one'][4] = 1
        routing = self.driver.routing(lambda: None)
        options = {'one': {'hold_channels': ['main:4'], 'routing_fingerprint': routing_fingerprint(
            routing.snapshot(['one.example.test']), include_status=False)}}
        self.driver.channels['one'][2] = 3  # Manual change before maintenance becomes the new baseline.
        run = self.start(acceptance_mode='availability', target_options=options)
        first, second = (row['data'] for row in run['targets'])
        self.assertEqual(run['status'], 'complete_with_holds')
        self.assertEqual(first['original_statuses']['main']['2'], 3)
        self.assertEqual(list(first['channel_results']), ['main:1'])
        self.assertEqual(first['held_channels'], ['main:4'])
        self.assertEqual(list(second['channel_results']), ['main:1'])
        self.assertEqual(second['held_channels'], [])
        self.assertEqual(self.driver.channels['one'], {1: 1, 2: 3, 3: 3, 4: 2})
        self.assertEqual(self.driver.channels['two'], {1: 1, 2: 2, 3: 3})

    def test_external_disable_before_first_write_becomes_the_baseline_on_resume(self):
        self.driver = Driver(('one',))
        self.driver.channels['one'][4] = 1
        original = Adapter.stage

        def stage(adapter, *args):
            result = original(adapter, *args)
            self.driver.channels['one'][4] = 2
            return result

        with patch.object(Adapter, 'stage', stage):
            run = self.start(acceptance_mode='availability', target_options={})
        self.assertEqual(run['status'], 'blocked')
        self.assertEqual(run['targets'][0]['phase'], 'pending')
        self.assertNotIn(('stop', 'one'), self.driver.calls)
        self.assertFalse(self.lease.path.exists())
        run = self.engine.run(self.run_id)
        self.assertEqual(run['status'], 'complete')
        self.assertEqual(run['targets'][0]['data']['original_statuses']['main']['4'], 2)
        self.assertEqual(self.driver.channels['one'][4], 2)

    def test_availability_mode_keeps_single_server_order_and_disabled_channels(self):
        call = self.driver.model_call
        def check(adapter, job, snapshot):
            self.assertTrue(job['availability_only'])
            return call(adapter, job, snapshot)
        self.driver.model_call = check
        run = self.start(acceptance_mode='availability')
        self.assertEqual(run['status'], 'complete')
        self.assertEqual(self.driver.channels, {'one': {1: 1, 2: 2, 3: 3}, 'two': {1: 1, 2: 2, 3: 3}})
        self.assertLess(self.driver.calls.index(('complete', 'one', False)), self.driver.calls.index(('stop', 'two')))
        report = json.loads(Path(report_write(self.store, self.run_id)['json']).read_text(encoding='utf-8'))
        self.assertEqual(report['acceptance_mode'], 'availability')
        self.assertIn('attachment', report['unverified'][0])

    def test_backup_and_routing_precheck_overlap_before_stoppage(self):
        self.driver = Driver(('one',))
        barrier = threading.Barrier(2)
        snapshot, backup = Routes.snapshot, Adapter.backup
        def route_read(routes, domains):
            barrier.wait(timeout=5)
            return snapshot(routes, domains)
        def backup_read(adapter, before, **kwargs):
            barrier.wait(timeout=5)
            return backup(adapter, before, **kwargs)
        with patch.object(Routes, 'snapshot', route_read), patch.object(Adapter, 'backup', backup_read):
            run = self.start(acceptance_mode='availability')
        self.assertEqual(run['status'], 'complete')

    def test_existing_plan_without_a_mode_uses_availability_on_execution(self):
        self.driver = Driver(('one',))
        run = self.start(acceptance_mode=None)
        self.assertEqual(run['status'], 'complete')
        self.assertEqual(run['targets'][0]['data']['acceptance_mode'], 'availability')
        report = json.loads(Path(report_write(self.store, self.run_id)['json']).read_text(encoding='utf-8'))
        self.assertEqual(report['acceptance_mode'], 'availability')

    def test_availability_resume_only_retries_failed_channels(self):
        self.driver = Driver(('one',))
        self.driver.channels['one'][4] = 1
        original = Routes.test_channel
        failed = {'value': True}
        calls = []
        def channel_call(routes, snapshot, site, row, exclusions, *, verify_snapshot=True):
            calls.append(row['id'])
            result = original(routes, snapshot, site, row, exclusions, verify_snapshot=verify_snapshot)
            if row['id'] == 4 and failed['value']:
                result.update(success=False, http=401, category='authorization')
            return result
        with patch.object(Routes, 'test_channel', channel_call):
            run = self.start(acceptance_mode='availability')
            self.assertEqual(run['status'], 'blocked')
            self.assertEqual(self.driver.channels['one'], {1: 2, 2: 2, 3: 3, 4: 2})
            failed['value'] = False
            run = self.engine.run(self.run_id)
        self.assertEqual(run['status'], 'complete')
        self.assertEqual(calls, [1, 4, 4])
        self.assertEqual(self.driver.channels['one'], {1: 1, 2: 2, 3: 3, 4: 1})

    def test_no_side_effects_on_second_execution(self):
        self.start()
        before = list(self.driver.calls)
        self.engine.run(self.run_id)
        self.assertEqual(self.driver.calls, before)

    def test_web_revision_change_before_restore_retests_calls_without_redeployment(self):
        self.driver = Driver(('one',))
        revision = {'value': 1, 'changed': False}
        original_verify = Adapter.verify
        def with_web(label):
            value = baseline(label)
            model = value['models'][0]['id']
            value['credentials'].append({'name': 'synthetic-web', 'type': 'gemini-enterprise-web', 'auth_index': 'web',
                                         'gemini_enterprise_web': {'cookie_present': True, 'revision': revision['value']}})
            value['catalogs']['synthetic-web'] = [{'id': model}]
            value['web'] = {'enabled': True}
            return value
        def verify(adapter, *args, **kwargs):
            if self.store.target(self.run_id, adapter.label)['phase'] == 'restoring' and not revision['changed']:
                revision.update(value=2, changed=True)
            return original_verify(adapter, *args, **kwargs)
        with patch('scripts.ops.tests.fakes.baseline', side_effect=with_web), patch.object(Adapter, 'verify', verify):
            run = self.start(acceptance_mode='availability')
        self.assertEqual(run['status'], 'complete')
        self.assertEqual(self.driver.deploys, {'one': 1})
        self.assertEqual(self.driver.calls.count(('channel', 'one')), 2)
        self.assertEqual(self.driver.channels['one'], {1: 1, 2: 2, 3: 3})

    def test_preflight_authorization_failure_never_stops_routing(self):
        self.driver.failure = 'preflight_auth'
        run = self.start()
        self.assertEqual(run['status'], 'blocked')
        self.assertFalse(any(call[0] == 'stop' for call in self.driver.calls))
        self.assertFalse(self.lease.path.exists())

    def test_deferring_web_does_not_skip_same_named_enterprise_and_holds_original_channel(self):
        self.driver = Driver(('one',))
        model = 'gemini-3.1-pro-preview'
        called = []
        original_call = self.driver.model_call

        def shared_model(label):
            value = baseline(label)
            value['web'] = {'enabled': True}
            value['credentials'].append({'name': 'synthetic-web', 'type': 'gemini-enterprise-web', 'auth_index': 'web',
                                         'gemini_enterprise_web': {'cookie_present': True, 'revision': 1,
                                                                   'cookie_check_status': 'relogin_required'}})
            value['catalogs']['synthetic-web'] = [{'id': model}]
            return value

        def model_call(adapter, job, snapshot):
            called.append(job['id'])
            return original_call(adapter, job, snapshot)

        self.driver.model_call = model_call
        with patch('scripts.ops.tests.fakes.baseline', side_effect=shared_model):
            routing = self.driver.routing(lambda: None).snapshot(['one.example.test'])
            run = self.start(deferrals={'web_model_tests': ['gemini-enterprise-web:' + model],
                                        'hold_channels': ['main:1'], 'routing_fingerprint': routing_fingerprint(routing)})
        data = run['targets'][0]['data']
        self.assertEqual(run['status'], 'complete_with_holds')
        self.assertEqual(called, ['gemini-enterprise:' + model])
        self.assertNotIn('main:1', data['channel_results'])
        self.assertFalse(data['original_routing_restored'])
        self.assertEqual(data['original_statuses']['main'], {'1': 1, '2': 2, '3': 3})
        self.assertEqual(self.driver.channels['one'], {1: 2, 2: 2, 3: 3})
        self.assertFalse(self.lease.path.exists())

    def test_fast_acceptance_marks_manual_models_and_tests_only_channels_to_restore(self):
        self.driver = Driver(('one',))
        self.driver.channels['one'][4] = 1
        smoke_model = 'gemini-3.1-pro-preview'

        def models(label):
            value = baseline(label)
            extra = {'id': 'gemini-3.8-flash'}
            value['models'].append(extra)
            value['catalogs']['synthetic-' + label].append(extra)
            value['web'] = {'enabled': True}
            value['credentials'].append({'name': 'synthetic-web', 'type': 'gemini-enterprise-web', 'auth_index': 'web',
                                         'gemini_enterprise_web': {'cookie_present': True, 'revision': 1,
                                                                   'cookie_check_status': 'relogin_required'}})
            value['catalogs']['synthetic-web'] = [{'id': smoke_model}]
            return value

        routing = self.driver.routing(lambda: None).snapshot(['one.example.test'])
        with patch('scripts.ops.tests.fakes.baseline', side_effect=models):
            run = self.start(deferrals={'web_model_tests': ['gemini-enterprise-web:' + smoke_model],
                                        'hold_channels': ['main:1'], 'routing_fingerprint': routing_fingerprint(routing)},
                             fast_model_tests=['gemini-enterprise:' + smoke_model])
        data = run['targets'][0]['data']
        self.assertEqual(run['status'], 'complete_with_holds')
        self.assertEqual(list(data['model_results']), ['gemini-enterprise:' + smoke_model])
        self.assertEqual(list(data['channel_results']), ['main:4'])
        self.assertIn({'model': 'gemini-3.8-flash', 'provider': 'gemini-enterprise', 'reason': 'manual_validation_pending'},
                      data['excluded_tests'])
        self.assertEqual(self.driver.channels['one'], {1: 2, 2: 2, 3: 3, 4: 1})

    def test_fast_acceptance_preserves_authorized_hold_when_web_is_disabled(self):
        self.driver = Driver(('one',))
        self.driver.channels['one'][4] = 1
        routing = self.driver.routing(lambda: None).snapshot(['one.example.test'])
        run = self.start(deferrals={'hold_channels': ['main:1'], 'routing_fingerprint': routing_fingerprint(routing)},
                         fast_model_tests=['gemini-enterprise:gemini-3.1-pro-preview'])
        data = run['targets'][0]['data']
        self.assertEqual(run['status'], 'complete_with_holds')
        self.assertEqual(list(data['channel_results']), ['main:4'])
        self.assertEqual(self.driver.channels['one'], {1: 2, 2: 2, 3: 3, 4: 1})

    def test_stop_propagation_failure_blocks_restart_and_next_target(self):
        self.driver.failure = 'stop_propagation'
        run = self.start()
        self.assertEqual(run['status'], 'blocked')
        self.assertEqual(self.driver.deploys, {})
        self.assertTrue(self.lease.path.exists())
        self.assertEqual(run['targets'][1]['phase'], 'pending')

    def test_failed_logs_use_time_fallback_not_silence(self):
        self.driver.failure = 'log_failure'
        run = self.start()
        result = run['targets'][0]['data']['drain']
        self.assertEqual(result['criterion'], 'fallback_wait_limit_180_seconds')
        self.assertEqual(result['elapsed_seconds'], 180)

    def test_verified_all_provider_zero_baseline_is_immediate(self):
        self.driver.counts = {'reliable': True, 'baseline': 0, 'remaining': 0}
        run = self.start()
        result = run['targets'][0]['data']['drain']
        self.assertTrue(result['completion_verified'])
        self.assertEqual(result['elapsed_seconds'], 0)

    def test_quota_failure_keeps_routes_disabled_without_rollback(self):
        self.driver.failure = 'quota'
        run = self.start()
        self.assertEqual(run['status'], 'blocked')
        self.assertEqual(self.driver.channels['one'][1], 2)
        self.assertEqual(self.driver.rollbacks, {})
        self.assertEqual(run['targets'][1]['phase'], 'pending')
        self.assertTrue(self.lease.path.exists())

    def test_program_regression_rolls_back_once_and_stops_batch(self):
        self.driver.failure = 'program_regression'
        run = self.start()
        self.assertEqual(run['status'], 'rolled_back_stopped')
        self.assertEqual(self.driver.rollbacks, {'one': 1})
        self.assertEqual(self.driver.channels['one'][1], 1)
        self.assertEqual(run['targets'][1]['phase'], 'pending')
        self.assertIn(('model', 'one', 'old'), self.driver.calls)
        self.assertFalse(self.lease.path.exists())

    def test_platform_deployment_failure_rolls_back(self):
        self.driver.failure = 'deployment'
        run = self.start()
        self.assertEqual(run['status'], 'rolled_back_stopped')
        self.assertEqual(self.driver.rollbacks, {'one': 1})

    def test_failed_rollback_retains_lease_and_disabled_routes(self):
        self.driver.failure = 'program_regression'
        original = self.driver.adapter
        def factory(target, guard):
            adapter = original(target, guard)
            def failed(*args, **kwargs):
                self.driver.rollbacks['one'] = self.driver.rollbacks.get('one', 0) + 1
                raise OpsError('rollback_failed')
            adapter.rollback = failed
            return adapter
        self.driver.adapter = factory
        run = self.start()
        self.assertEqual(run['status'], 'blocked')
        self.assertEqual(self.driver.channels['one'][1], 2)
        self.assertTrue(self.lease.path.exists())
        self.assertEqual(self.driver.rollbacks, {'one': 1})

    def test_lost_deploy_ack_resume_does_not_recreate_twice(self):
        self.driver.failure = 'lost_deploy_ack'
        run = self.start()
        self.assertEqual(run['status'], 'blocked')
        self.driver.failure = None
        run = self.engine.run(self.run_id)
        self.assertEqual(run['status'], 'complete')
        self.assertEqual(self.driver.deploys, {'one': 1, 'two': 1})

    def test_failed_channel_is_not_restored(self):
        self.driver.failure = 'channel'
        run = self.start()
        self.assertEqual(run['status'], 'blocked')
        self.assertEqual(self.driver.channels['one'][1], 2)
        self.assertNotIn(('restore', 'one'), self.driver.calls)

    def test_explicit_http_400_exclusion_persists_and_other_tests_continue(self):
        self.driver = Driver(('one',))
        excluded_model = {'id': 'gemini-2.0-preview'}
        excluded_job = 'gemini-enterprise:' + excluded_model['id']
        original_call = self.driver.model_call
        called = []

        def catalog_with_excluded_model(label):
            value = baseline(label)
            value['models'].append(excluded_model)
            value['catalogs']['synthetic-' + label].append(excluded_model)
            return value

        def model_call(adapter, job, snapshot):
            called.append(job['id'])
            if job['id'] == excluded_job:
                return {'success': False, 'http': 400, 'category': 'functional'}
            return original_call(adapter, job, snapshot)

        self.driver.model_call = model_call
        with patch('scripts.ops.tests.fakes.baseline', side_effect=catalog_with_excluded_model):
            self.assertEqual(self.start()['status'], 'blocked')
            self.assertEqual(self.driver.channels['one'][1], 2)
            with self.assertRaisesRegex(OpsError, 'model_test_exclusion_requires_recorded_400'):
                self.engine.run(self.run_id, exclude_model_test='gemini-enterprise:another-model')
            self.driver.failure = 'channel'
            self.assertEqual(self.engine.run(self.run_id, exclude_model_test=excluded_job)['status'], 'blocked')
            self.assertEqual(self.driver.channels['one'][1], 2)
            self.driver.failure = None
            run = self.engine.run(self.run_id)

        self.assertEqual(run['status'], 'complete')
        self.assertEqual(called, [excluded_job, 'gemini-enterprise:gemini-3.1-pro-preview'])
        self.assertEqual(run['targets'][0]['data']['excluded_tests'][0]['http'], 400)
        self.assertEqual(self.driver.channels['one'], {1: 1, 2: 2, 3: 3})

    def test_resume_after_model_429_requires_fresh_scoped_quota_before_another_call(self):
        self.driver.failure = 'quota'
        self.assertEqual(self.start()['status'], 'blocked')
        self.driver.failure = None
        original = self.driver.adapter
        quota_reads, available = [], False

        def factory(target, guard):
            adapter = original(target, guard)
            verify = adapter.verify

            def check(*args, quota_models=(), **kwargs):
                value = verify(*args, **kwargs)
                if quota_models:
                    from datetime import datetime, timedelta, timezone
                    quota_reads.append(quota_models)
                    row = value['credentials'][0]
                    binding = row['gemini_enterprise']['collections'][0]
                    value['binding_observations'] = {row['name']: {binding['binding_id']: {
                        'models': value['models'], 'quota': {**binding,
                            'reset_at': (datetime.now(timezone.utc) + timedelta(hours=1)).isoformat(),
                            'selected_pool': {'status': 'ok', 'remaining': 800 if available else 0}}}}}
                return value

            adapter.verify = check
            return adapter

        self.driver.adapter = factory
        self.assertEqual(self.engine.run(self.run_id)['status'], 'blocked')
        self.assertEqual(sum(call == ('model', 'one', 'new') for call in self.driver.calls), 1)
        self.assertEqual(self.driver.channels['one'][1], 2)
        available = True
        self.assertEqual(self.engine.run(self.run_id)['status'], 'complete')
        self.assertEqual(quota_reads, [{'gemini-3.1-pro-preview'}, {'gemini-3.1-pro-preview'}])
        self.assertEqual(self.driver.deploys['one'], 1)
        self.assertEqual(self.driver.channels['one'][1], 1)

    def test_resume_reuses_matching_models_but_retests_channels(self):
        self.driver.failure = 'channel'
        self.start()
        self.driver.failure = None
        self.engine.run(self.run_id)
        self.assertEqual(sum(call == ('model', 'one', 'new') for call in self.driver.calls), 1)
        self.assertEqual(sum(call == ('channel', 'one') for call in self.driver.calls), 2)

    def test_expired_model_cache_is_retested_before_restore(self):
        self.driver.failure = 'channel'
        self.start()
        self.driver.failure = None
        self.clock.sleep(601)
        run = self.engine.run(self.run_id)
        self.assertEqual(run['status'], 'complete')
        self.assertEqual(sum(call == ('model', 'one', 'new') for call in self.driver.calls), 2)

    def test_pause_finishes_current_server_only(self):
        self.driver.on_model = lambda: self.store.pause(self.run_id)
        run = self.start()
        self.assertEqual(run['status'], 'paused_after_current')
        self.assertEqual(run['targets'][0]['phase'], 'complete')
        self.assertEqual(run['targets'][1]['phase'], 'pending')
        self.assertFalse(self.lease.path.exists())

    def test_config_drift_never_overwrites_external_change(self):
        self.driver.on_model = lambda: setattr(self.driver, 'config_drift', True)
        run = self.start()
        self.assertEqual(run['status'], 'blocked')
        self.assertEqual(self.driver.rollbacks, {})
        self.assertEqual(self.driver.channels['one'][1], 2)

    def test_foreign_maintenance_lease_is_not_deleted(self):
        self.lease.path.write_text(json.dumps({'lease_id': 'foreign', 'run_id': 'another'}))
        with self.assertRaises(OpsError):
            self.start()
        self.assertEqual(json.loads(self.lease.path.read_text())['lease_id'], 'foreign')
        self.assertEqual(self.driver.calls, [])

    def test_process_lock_excludes_another_controller(self):
        with ProcessLock(self.root / 'process.lock'):
            with self.assertRaises(OpsError):
                with ProcessLock(self.root / 'process.lock'):
                    pass

    def test_report_contains_no_secret_snapshot(self):
        self.start()
        paths = report_write(self.store, self.run_id, offline=True)
        text = Path(paths['json']).read_text() + Path(paths['html']).read_text(encoding='utf-8')
        self.assertNotIn('SECRET-NEVER-REPORT', text)
        self.assertIn('offline_simulation', text)
        self.assertFalse(json.loads(Path(paths['json']).read_text())['production_verified'])

    def test_sql_transaction_rolls_back_on_exception(self):
        with self.assertRaises(RuntimeError):
            with self.store.connect() as db:
                db.execute('INSERT INTO plans VALUES(?,?,?)', ('incomplete', '{}', 'x'))
                raise RuntimeError('interrupt')
        with self.assertRaises(OpsError):
            self.store.get_plan('incomplete')

    def test_build_failure_precedes_channel_stoppage(self):
        self.releases.prepare = lambda release: (_ for _ in ()).throw(OpsError('release_qualification_failed'))
        run = self.start()
        self.assertEqual(run['status'], 'blocked')
        self.assertFalse(any(call[0] == 'stop' for call in self.driver.calls))
        self.assertFalse(self.lease.path.exists())

    def test_reliable_incomplete_counts_never_take_time_fallback(self):
        self.driver.counts = {'reliable': True, 'baseline': 100, 'remaining': 6}
        original = self.clock.sleep
        def sleep(seconds):
            original(seconds)
            if self.clock() >= 1400:
                raise KeyboardInterrupt()
        self.clock.sleep = sleep
        run = self.start()
        self.assertEqual(run['status'], 'blocked')
        self.assertEqual(self.driver.deploys, {})
        self.assertEqual(run['targets'][0]['data']['drain']['criterion'], 'waiting_for_request_completion')

    def test_lost_reliable_counter_does_not_become_time_fallback(self):
        self.driver.counts = {'reliable': True, 'baseline': 100, 'remaining': 6}
        readings = iter([self.driver.counts, None])
        self.driver.live_counts = lambda handle: next(readings)
        run = self.start()
        self.assertEqual(run['status'], 'blocked')
        self.assertEqual(run['targets'][0]['data']['last_error']['code'], 'reliable_request_counter_became_unavailable')
        self.assertEqual(self.driver.deploys, {})
        self.driver.live_counts = lambda handle: None
        result = self.engine.run(self.run_id)
        self.assertEqual(result['status'], 'blocked')
        self.assertEqual(self.driver.deploys, {})

    def test_reliable_baseline_is_frozen_across_observations(self):
        self.driver.counts = {'reliable': True, 'baseline': 100, 'remaining': 6}
        values = iter([self.driver.counts, {'reliable': True, 'baseline': 1000, 'remaining': 6}])
        self.driver.live_counts = lambda handle: next(values)
        result = self.start()
        self.assertEqual(result['status'], 'blocked')
        self.assertEqual(result['targets'][0]['data']['last_error']['code'], 'request_baseline_changed')
        self.assertEqual(self.driver.deploys, {})

    def test_uncertain_rollback_is_not_repeated_by_resume(self):
        self.driver.failure = 'program_regression'
        original = self.driver.adapter
        def factory(target, guard):
            adapter = original(target, guard)
            def fail(*args, **kwargs):
                self.driver.rollbacks['one'] = self.driver.rollbacks.get('one', 0) + 1
                raise OpsError('lost_rollback_ack')
            adapter.rollback = fail
            return adapter
        self.driver.adapter = factory
        self.start()
        self.clock.sleep(1900)
        result = self.engine.run(self.run_id)
        self.assertEqual(result['status'], 'blocked')
        self.assertEqual(self.driver.rollbacks, {'one': 1})
        self.assertTrue(self.lease.path.exists())

    def test_complete_commit_before_lease_release_recovers_without_redeploy(self):
        self.start()
        self.lease.claim(self.run_id, 'one', self.driver.inventory['targets']['one']['origin'], self.root / self.run_id / 'one')
        previous = list(self.driver.calls)
        self.engine.run(self.run_id)
        self.assertFalse(self.lease.path.exists())
        self.assertEqual(self.driver.calls, previous)


if __name__ == '__main__':
    unittest.main()

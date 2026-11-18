import contextlib
from datetime import datetime, timezone
import json
from pathlib import Path
import tempfile
import threading
import unittest
from unittest.mock import MagicMock, patch
from urllib.parse import parse_qs, urlsplit

from scripts.ops.application import Application
from scripts.ops.cache_observer import SyncEvents, timestamp_ns
from scripts.ops.cache_sync import CacheObserver
from scripts.ops.concurrency import ReadLimiter, TaskExecutor, background_reads
from scripts.ops.core import OpsError, sha_file
from scripts.ops.plans import execution_options, load_target_options
from scripts.ops.preparation import inspect_plan
from scripts.ops.release import Releases
from scripts.ops.routing import channel_accepted, channel_test_model


class PreparationTests(unittest.TestCase):
    def test_selected_model_uses_only_matching_channel_alias(self):
        row = {'models': 'gemini-2.5-pro,neon-channel', 'test_model': 'gemini-2.5-pro',
               'model_mapping': {'neon-channel': 'gemini-4-neon'}}
        self.assertEqual(channel_test_model(row, set(), acceptance_model='gemini-4-neon'),
                         ('neon-channel', 'gemini-4-neon'))
        with self.assertRaisesRegex(OpsError, 'acceptance_model_not_configured'):
            channel_test_model(row, set(), acceptance_model='gemini-4-argon')

    def test_health_checks_need_explicit_model_scope_and_unavailability_evidence(self):
        receipt = {'acceptance_mode': 'health_only', 'reason': 'model_price_error', 'success': False}
        self.assertFalse(channel_accepted(receipt))
        self.assertTrue(channel_accepted(receipt, allow_health_only=True))
        self.assertFalse(channel_accepted({**receipt, 'reason': 'quota'}, allow_health_only=True))

    def test_release_qualification_overlaps_read_only_preflight(self):
        release = {'commit': 'commit'}
        driver, releases = MagicMock(), MagicMock()
        build_started, read_done = threading.Event(), threading.Event()

        def qualify(description):
            self.assertEqual(description, release)
            build_started.set()
            self.assertTrue(read_done.wait(5))
            return {'package': 'qualified'}

        def inspect(targets, description, *, options):
            try:
                self.assertTrue(build_started.wait(5))
                self.assertEqual(targets, ['vps', 'zeabur'])
                self.assertEqual(description, release)
                self.assertEqual(options, {})
                return ['vps-ready', 'zeabur-ready']
            finally:
                read_done.set()

        releases.prepare.side_effect = qualify
        driver.inspect.side_effect = inspect
        self.assertEqual(
            inspect_plan(driver, ['vps', 'zeabur'], release, {}, releases=releases),
            (['vps-ready', 'zeabur-ready'], {'package': 'qualified'}),
        )

    def test_qualified_release_reuses_the_same_verified_artifacts(self):
        release = {'commit': 'commit'}
        with tempfile.TemporaryDirectory() as temporary:
            releases = Releases('repository', temporary, go='go')

            def build(description, directory, receipt, version, identity):
                binary = directory / 'binary'
                package = directory / 'package'
                binary.write_bytes(b'binary')
                package.write_bytes(b'package')
                result = {'identity': identity, 'binary': str(binary), 'package': str(package),
                          'binary_sha256': sha_file(binary), 'package_sha256': sha_file(package)}
                receipt.write_text(json.dumps(result), encoding='utf-8')
                return result

            with patch('scripts.ops.release.describe', return_value=release), \
                    patch('scripts.ops.release.subprocess.check_output', return_value='go version'), \
                    patch.object(releases, '_build', side_effect=build) as rebuild:
                qualified = releases.prepare(release)
                self.assertEqual(releases.prepare(release), qualified)
                rebuild.assert_called_once()
                Path(qualified['package']).write_bytes(b'corrupt')
                with self.assertRaisesRegex(OpsError, 'cached_release_corrupt'):
                    releases.prepare(release)

    def test_background_reads_are_capped_and_a_waiting_foreground_read_goes_first(self):
        limiter = ReadLimiter(4, 2)
        waiting = {name: threading.Event() for name in ['background-3', 'foreground-3']}
        entered = {name: threading.Event() for name in ['background-1', 'background-2', 'background-3',
                                                         'foreground-1', 'foreground-2', 'foreground-3']}
        release = {name: threading.Event() for name in entered}
        condition_wait = limiter.condition.wait

        def waiting_read(timeout=None):
            name = threading.current_thread().read_name
            if name in waiting:
                waiting[name].set()
            return condition_wait(timeout)

        limiter.condition.wait = waiting_read
        def read(name):
            threading.current_thread().read_name = name
            with limiter:
                entered[name].set()
                self.assertTrue(release[name].wait(5))

        with TaskExecutor(max_workers=6) as executor, contextlib.ExitStack() as cleanup:
            for event in release.values():
                cleanup.callback(event.set)
            with background_reads():
                jobs = [executor.submit(read, f'background-{i}') for i in range(1, 4)]
            self.assertTrue(entered['background-1'].wait(5))
            self.assertTrue(entered['background-2'].wait(5))
            self.assertTrue(waiting['background-3'].wait(5))
            jobs.extend(executor.submit(read, f'foreground-{i}') for i in range(1, 4))
            self.assertTrue(entered['foreground-1'].wait(5))
            self.assertTrue(entered['foreground-2'].wait(5))
            self.assertTrue(waiting['foreground-3'].wait(5))
            self.assertEqual((limiter.active, limiter.background_active), (4, 2))
            release['background-1'].set()
            self.assertTrue(entered['foreground-3'].wait(5))
            self.assertFalse(entered['background-3'].is_set())
            release['foreground-1'].set()
            self.assertTrue(entered['background-3'].wait(5))
            for event in release.values():
                event.set()
            for job in jobs:
                job.result(timeout=5)

    def test_legacy_exception_must_resolve_to_one_target(self):
        options = {'hold_channels': ['vip:22'], 'routing_fingerprint': 'recorded'}
        plan = {'schema': 1, 'operator_deferrals': options, 'targets': [{'label': 'one'}]}
        self.assertEqual(execution_options(plan)['one']['held_channels'], {'vip:22'})
        with self.assertRaisesRegex(OpsError, 'ambiguous_legacy_exceptions'):
            execution_options({**plan, 'targets': [*plan['targets'], {'label': 'two'}]})

    def test_target_options_reject_other_targets_and_duplicate_json_keys(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'options.json'
            path.write_text('{"one":{"hold_channels":["vip:22"]},"one":{}}', encoding='utf-8')
            with self.assertRaisesRegex(OpsError, 'duplicate_target_option_key'):
                load_target_options(path, ['one'])
            path.write_text('{"two":{"hold_channels":["vip:22"]}}', encoding='utf-8')
            with self.assertRaisesRegex(OpsError, 'target_options_outside_selection'):
                load_target_options(path, ['one'])

    def test_backup_reuses_only_files_with_unchanged_version_evidence(self):
        rows = [{'name': 'same', 'size': 100, 'modtime': '2026-11-01T00:00:00Z'},
                {'name': 'changed', 'size': 100, 'modtime': '2026-11-01T00:00:00Z'},
                {'name': 'unknown'}]
        app = Application(MagicMock())
        app.read = MagicMock(return_value={'data': 'first'})
        snapshot = {'legacy': False, 'raw': 'first config', 'credentials': rows}
        original = app.backup(snapshot)
        rows[1] = {**rows[1], 'modtime': '2026-11-01T01:00:00Z'}
        app.read = MagicMock(return_value={'data': 'fresh'})
        backup = app.backup({**snapshot, 'raw': 'current config'}, reuse=original)
        self.assertEqual(backup['config'], 'current config')
        self.assertEqual(backup['credential_files'], {'same': 'first', 'changed': 'fresh', 'unknown': 'fresh'})
        self.assertEqual({parse_qs(urlsplit(call.args[0]).query)['name'][0] for call in app.read.call_args_list},
                         {'changed', 'unknown'})

    def test_catalog_age_does_not_slide_and_quota_is_checked_per_binding(self):
        from datetime import timedelta

        now = datetime.now(timezone.utc)
        binding = {'binding_id': 'b', 'auth_index': 'b-auth', 'project_id': 'p', 'location': 'global'}
        credentials = [{'name': 'test', 'type': 'gemini-enterprise', 'gemini_enterprise': {'collections': [binding]}}]
        quota = {'binding_id': 'b', 'project_id': 'p', 'location': 'global',
                 'observed_at': now.isoformat(), 'reset_at': (now + timedelta(hours=1)).isoformat(),
                 'selected_pool': {'status': 'ok', 'remaining': 100}}
        model = {'id': 'synthetic-model'}
        responses = {'/v8/management/config': {'api-keys': ['synthetic']}, '/v8/management/config.yaml': '{}',
                     '/v8/management/credentials': {'files': credentials}, '/v1/models': {'data': [model]},
                     '/v8/management/credentials/models': {'models': [model]}, '/healthz': {}, '/management.html': '<html>',
                     '/v8/management/credentials/gemini-enterprise-web/status': {},
                     '/v8/management/quota/gemini-enterprise': quota}
        app = Application(MagicMock())
        app.read = MagicMock(side_effect=lambda path, **kwargs: {
            'data': responses[urlsplit(path).path], 'http': 200, 'headers': {}})
        with patch('scripts.ops.application.time.time', return_value=1000):
            first = app.snapshot(quotas=True)
        app.read.reset_mock()
        with patch('scripts.ops.application.time.time', return_value=1500):
            second = app.snapshot(quotas=True, reuse=first)
        self.assertEqual(second['catalog_observed_at'], 1000)
        self.assertFalse(any('credentials/models' in call.args[0] or '/quota/' in call.args[0] for call in app.read.call_args_list))
        stale = second['binding_observations']['test']['b']
        stale['quota'] = {**stale['quota'], 'reset_at': (now - timedelta(seconds=1)).isoformat()}
        app.read.reset_mock()
        with patch('scripts.ops.application.time.time', return_value=1501):
            third = app.snapshot(quotas=True, reuse=second)
        self.assertTrue(any('/quota/' in call.args[0] for call in app.read.call_args_list))
        self.assertFalse(any('credentials/models' in call.args[0] for call in app.read.call_args_list))
        app.read.reset_mock()
        with patch('scripts.ops.application.time.time', return_value=1601):
            app.snapshot(quotas=True, reuse=third)
        self.assertTrue(any('credentials/models' in call.args[0] for call in app.read.call_args_list))


class CacheSyncTests(unittest.TestCase):
    def test_old_start_cannot_pair_with_finish_after_the_barrier(self):
        events = SyncEvents('container')
        def stamp(second):
            return f'2026-11-12T00:00:{second:02}Z'

        events.ingest(stamp(1) + ' | syncing channels from database')
        events.barrier('write', now_ns=timestamp_ns(stamp(2)))
        events.ingest(stamp(3) + ' | channels synced from database')
        events.ingest(stamp(2) + ' | syncing channels from database')
        events.ingest(stamp(3) + ' | channels synced from database')
        self.assertIsNone(events.proof('write'))
        events.ingest(stamp(4) + ' | syncing channels from database')
        self.assertIsNone(events.proof('write'))
        events.ingest(stamp(5) + ' | channels synced from database')
        self.assertEqual(events.proof('write')['sync_started_at'], stamp(4))
        events.barrier('write', now_ns=timestamp_ns(stamp(5)))
        self.assertIsNone(events.proof('write'))

    def test_sync_receipt_is_reused_only_for_its_write_and_current_process(self):
        import json

        stamp = '2026-11-12T00:00:00Z'
        proof = {'container_id': 'a' * 64, 'started_at': stamp, 'sync_seconds': 60}
        start = timestamp_ns(stamp)
        barrier = {'container_id': proof['container_id'], 'started_at': stamp, 'operation_id': 'b' * 32,
                   'cursor': 5, 'after_ns': start}
        receipt = {**barrier, 'sync_started_at': '2026-11-12T00:00:01Z',
                   'sync_finished_at': '2026-11-12T00:00:02Z', 'after_status_writes': True}
        factory = MagicMock()
        client = factory.connect.return_value
        stdin, stdout = MagicMock(), MagicMock()
        client.exec_command.return_value = (stdin, stdout, MagicMock())
        stdout.readline.side_effect = map(json.dumps, [{**proof, 'ready': True}, barrier, receipt,
                                                       {**barrier, 'after_ns': start + 2_000_000_000}, receipt,
                                                       {**barrier, 'started_at': receipt['sync_started_at']}])
        observer = CacheObserver(factory, 'synthetic', proof)
        try:
            observer.barrier(barrier['operation_id'])
            self.assertEqual(observer.wait(barrier['operation_id']), receipt)
            self.assertEqual(observer.wait(barrier['operation_id']), receipt)
            self.assertEqual(stdout.readline.call_count, 3)
            observer.barrier(barrier['operation_id'])
            with self.assertRaisesRegex(OpsError, 'shadow_sync_not_observed'):
                observer.wait(barrier['operation_id'])
            with self.assertRaisesRegex(OpsError, 'shadow_sync_observation_identity_changed'):
                observer.barrier(barrier['operation_id'])
        finally:
            observer.close()

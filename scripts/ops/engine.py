"""Durable single-target state machine. Every production mutation follows a lease guard."""
from __future__ import annotations

from .concurrency import TaskExecutor as ThreadPoolExecutor
import copy
import contextlib
import time

from scripts.vps.maintenance import FALLBACK_WAIT_SECONDS, evaluate_drain
from . import catalog, testing
from .core import EXCLUDED_MODELS, OpsError, TEST_TTL, channel_fingerprint, deployment_identity, digest, is_claude_model, require, validate_selection
from .store import ProcessLock
from .routing import channel_test_model
from .plans import execution_options, validate_observed_options
from .preparation import Lookahead
from .telemetry import PHASE, recording, operation, note


TERMINAL = {'complete', 'complete_with_holds', 'rolled_back', 'already_current'}


def restoration_routes(snapshot, held_channels):
    if not held_channels:
        return snapshot
    known = {f"{site}:{row['id']}" for site, rows in snapshot['channels'].items() for row in rows}
    require(held_channels <= known, 'held_channel_not_found')
    channels = {site: [{**row, 'status': 2} if row['status'] == 1 and f"{site}:{row['id']}" in held_channels else row for row in rows]
                for site, rows in snapshot['channels'].items()}
    return {**snapshot, 'channels': channels}


class Engine:
    def __init__(self, store, lease, driver, releases, *, emit=lambda event, **values: None,
                 clock=time.time, monotonic=time.monotonic, sleep=time.sleep):
        self.store, self.lease, self.driver, self.releases = store, lease, driver, releases
        self.emit, self.clock, self.monotonic, self.sleep = emit, clock, monotonic, sleep
        self.token = None
        self.lookahead, self.next_preparation, self.prepared = None, None, {}

    def guard(self):
        require(self.token is not None, 'mutation_outside_maintenance')
        self.lease.check(self.token)

    def save(self, run, target, phase, **values):
        PHASE.set(phase)
        data = self.store.save(run, target, phase, **values)
        self.emit('stage', run=run, target=target, phase=phase)
        return data

    def saved(self, run, label):
        return self.store.target(run, label)

    def snapshot(self, run, label, name):
        return self.store.unseal(run, label + ':' + name)

    def seal(self, run, label, name, value):
        self.store.seal(run, label + ':' + name, value)

    def preparation(self, run, label):
        candidate = self.prepared.get(label)
        if candidate is None and self.store.has_snapshot(run, label + ':prefetched'):
            candidate = self.store.unseal(run, label + ':prefetched')
        if candidate is None:
            plan_id = self.store.run(run)['plan']
            if self.store.has_snapshot(plan_id, label + ':preflight'):
                candidate = self.store.unseal(plan_id, label + ':preflight')
        if candidate and 0 <= time.time() - candidate.get('prepared_at', 0) <= TEST_TTL:
            note('cache_hit', scope='preparation')
            return candidate
        return {}

    def run(self, run_id, *, exclude_model_test=None):
        run = self.store.run(run_id)
        plan = self.store.get_plan(run['plan'])
        require(plan['inventory_fingerprint'] == self.driver.inventory_fingerprint(), 'inventory_changed_since_plan')
        validate_selection(self.driver.inventory, [target['label'] for target in plan['targets']])
        options = execution_options(plan)
        session = self.driver.session() if hasattr(self.driver, 'session') else contextlib.nullcontext()
        with ProcessLock(self.lease.path.with_suffix('.controller.lock')), session, contextlib.ExitStack() as cleanup:
            self.prepared.clear()
            self.lookahead = cleanup.enter_context(Lookahead(self.driver, self.store, run_id, plan['release']))
            if exclude_model_test:
                run = self.store.run(run_id)
                require(run['status'] == 'blocked' and len(run['targets']) == 1, 'model_test_exclusion_requires_blocked_single_target')
                item = run['targets'][0]
                data = item['data']
                result = data.get('model_results', {}).get(exclude_model_test) or {}
                require(item['phase'] == 'model_testing' and exclude_model_test in data.get('required_models', [])
                        and result.get('success') is False and result.get('http') == 400,
                        'model_test_exclusion_requires_recorded_400')
                exclusion = {key: result[key] for key in ('model', 'provider', 'http', 'upstream_error_codes',
                                                          'upstream_google_status') if key in result}
                exclusion['reason'] = 'user_excluded_model_after_http_400'
                self.save(run_id, item['target'], item['phase'], model_test_exclusions={
                    **data.get('model_test_exclusions', {}), exclude_model_test: exclusion})
            if self.lease.path.exists():
                import json
                lease_record = json.loads(self.lease.path.read_text(encoding='utf-8'))
                if lease_record.get('run_id') == run_id:
                    completed = next((item for item in run['targets'] if item['target'] == lease_record.get('target')
                                      and item['phase'] in TERMINAL), None)
                    if completed:
                        self.lease.release(lease_record['lease_id'])
            if run['status'] in {'complete', 'complete_with_holds', 'rolled_back_stopped'}:
                return run
            self.store.status(run_id, 'running')
            for index, target in enumerate(plan['targets']):
                label = target['label']
                state = self.saved(run_id, label)
                if state['phase'] in TERMINAL:
                    continue
                try:
                    prefetched = self.lookahead.take(label)
                    if prefetched is not None:
                        self.prepared[label] = prefetched
                except Exception as error:
                    self.store.status(run_id, 'blocked', reason='background_preflight_failed')
                    self.store.event(run_id, label, 'preparation_failed', {'code': getattr(error, 'code', type(error).__name__)})
                    return self.store.run(run_id)
                if self.store.run(run_id)['pause'] and state['phase'] == 'pending':
                    self.store.status(run_id, 'paused_after_current')
                    return self.store.run(run_id)
                following = plan['targets'][index + 1] if index + 1 < len(plan['targets']) else None
                self.next_preparation = None
                if following:
                    self.next_preparation = (following, {**options[following['label']], 'routing_states_frozen': plan['schema'] == 1})
                selected = {**options[label], 'routing_states_frozen': plan['schema'] == 1}
                self.token = self.lease.claim(run_id, label, target['origin'], self.store.root / run_id / label)
                self.driver.guard = self.guard
                adapter = self.driver.adapter(target, self.guard)
                if hasattr(adapter, 'close'):
                    cleanup.callback(adapter.close)
                try:
                    with recording(self.store, run_id, label):
                        self.target(run_id, target, adapter, plan['release'], **selected)
                except BaseException as error:
                    state = self.saved(run_id, label)
                    category = error.category if isinstance(error, OpsError) else 'interrupted' if isinstance(error, KeyboardInterrupt) else 'blocked'
                    code = error.code if isinstance(error, OpsError) else type(error).__name__
                    self.seal(run_id, label, 'last-error', {'code': code, 'type': type(error).__name__, 'message': str(error)})
                    self.save(run_id, label, state['phase'], last_error={'code': code, 'category': category})
                    deployed_phases = {'deploying', 'deployed', 'verified', 'model_testing', 'models_passed',
                                       'channel_testing', 'tested', 'restoring', 'restored', 'postflight'}
                    if category == 'regression' and state['phase'] in deployed_phases and not state['data'].get('rollback_attempted'):
                        self.save(run_id, label, 'rollback_stopping', rollback_attempted=True, regression_code=code)
                        try:
                            with recording(self.store, run_id, label):
                                self.target(run_id, target, adapter, plan['release'], **selected)
                        except BaseException as recovery_error:
                            self.store.status(run_id, 'blocked', reason='rollback_not_verified')
                            self.emit('blocked', target=label, code=getattr(recovery_error, 'code', type(recovery_error).__name__),
                                      lease_retained=True, next_server_started=False)
                            return self.store.run(run_id)
                    else:
                        # No routing changes have occurred before the stopping phase.
                        if state['phase'] in {'pending', 'prepared', 'staging', 'staged'}:
                            self.lease.release(self.token)
                            self.token = None
                        self.store.status(run_id, 'blocked', reason=code)
                        self.emit('blocked', target=label, code=code, lease_retained=self.token is not None,
                                  next_server_started=False)
                        return self.store.run(run_id)
                state = self.saved(run_id, label)
                require(state['phase'] in TERMINAL, 'target_did_not_finish')
                # State is committed before lease release; a crash is recoverable.
                self.lease.release(self.token)
                self.token = None
                if hasattr(adapter, 'close'):
                    adapter.close()
                if state['phase'] == 'rolled_back':
                    self.store.status(run_id, 'rolled_back_stopped')
                    return self.store.run(run_id)
            holds = any(item['phase'] == 'complete_with_holds' for item in self.store.run(run_id)['targets'])
            self.store.status(run_id, 'complete_with_holds' if holds else 'complete', completed_at=self.clock())
        return self.store.run(run_id)

    def drain(self, run, label, routing, baseline, *, rollback=False):
        if not rollback and self.next_preparation is not None and self.lookahead is not None:
            target, options = self.next_preparation
            self.lookahead.start(target, options)
        phase = 'rollback_draining' if rollback else 'draining'
        record = self.saved(run, label)['data']
        # Wall-clock jumps or controller restarts cannot manufacture a quiet interval.
        started = self.monotonic()
        quiet_since, last_marker = None, None
        handle = record.get('drain_handle') if not rollback else record.get('rollback_drain_handle')
        previous_evidence = record.get('rollback_drain' if rollback else 'drain') or {}
        had_reliable_counts = bool(previous_evidence.get('provider_wide_counts_available'))
        frozen_baseline = previous_evidence.get('baseline') if had_reliable_counts else None
        while True:
            self.guard()
            routing.confirm(baseline)
            counts = self.driver.live_counts(handle) if handle else None
            reliable = bool(counts and counts.get('reliable'))
            require(not had_reliable_counts or reliable, 'reliable_request_counter_became_unavailable')
            if reliable:
                require(type(counts.get('baseline')) is int and type(counts.get('remaining')) is int,
                        'reliable_request_counter_incomplete')
                require(frozen_baseline is None or frozen_baseline == counts['baseline'], 'request_baseline_changed')
                frozen_baseline = counts['baseline']
                had_reliable_counts = True
            now = self.monotonic()
            elapsed = now - started
            quiet = None
            if not reliable and elapsed < FALLBACK_WAIT_SECONDS:
                try:
                    marker = routing.log_markers(max_seconds=min(10, FALLBACK_WAIT_SECONDS - elapsed))
                    now = self.monotonic()
                    if marker != last_marker or quiet_since is None:
                        quiet_since = now
                    last_marker = marker
                    quiet = now - quiet_since
                except Exception:
                    last_marker, quiet_since = None, None
            elapsed = self.monotonic() - started
            decision = evaluate_drain(channels_stopped=True, elapsed_seconds=elapsed,
                                      counts_reliable=bool(counts and counts.get('reliable')),
                                      baseline=counts.get('baseline') if counts else None,
                                      remaining=counts.get('remaining') if counts else None,
                                      log_quiet_seconds=quiet)
            evidence = {'criterion': decision.criterion, 'elapsed_seconds': round(elapsed, 3),
                        'completion_verified': decision.completion_verified,
                        'provider_wide_counts_available': reliable,
                        'baseline': counts['baseline'] if reliable else None,
                        'remaining': counts['remaining'] if reliable else None}
            self.save(run, label, phase, **{'rollback_drain' if rollback else 'drain': evidence})
            if decision.ready:
                return evidence
            delay = 10 if reliable else min(10, max(0, FALLBACK_WAIT_SECONDS - elapsed))
            with operation('drain_wait', kind='drain'):
                self.sleep(delay)

    def target(self, run, target, adapter, release_description, **options):
        routing = self.driver.routing(self.guard)
        routing.record_event = lambda event, data: self.store.event(run, target['label'], event, data)
        session = routing.session() if hasattr(routing, 'session') else contextlib.nullcontext()
        with session:
            return self._target(run, target, adapter, release_description, routing, **options)

    def _target(self, run, target, adapter, release_description, routing, *, deferred_web_tests=(), held_channels=frozenset(),
                approved_routing=None, fast_model_tests=(), acceptance_mode='full', routing_states_frozen=True):
        label = target['label']
        deferred_rows = [{'provider': 'gemini-enterprise-web', 'model': job.split(':', 1)[1],
                          'reason': 'user_deferred_web_authorization'} for job in sorted(deferred_web_tests)]
        while True:
            self.guard()
            state = self.saved(run, label)
            phase, data = state['phase'], state['data']
            if phase in TERMINAL:
                return
            if phase == 'pending':
                started_at = data.get('started_at', self.clock())
                self.save(run, label, 'pending', started_at=started_at)
                prepared = self.preparation(run, label)
                with ThreadPoolExecutor(max_workers=1) as executor:
                    release_future = None
                    if not self.store.run(run)['data'].get('release'):
                        release_future = executor.submit(self.releases.prepare, release_description)
                    current = adapter.prepare(routing, release_description, deferred_web_tests=deferred_web_tests, reuse=prepared)
                    before, routes, backup = (current[key] for key in ('target', 'routing', 'backup'))
                    if release_future is not None:
                        self.store.status(run, 'running', release=release_future.result())
                validate_observed_options(before, routes, deferred_web_tests, held_channels, fast_model_tests,
                                          approved_routing=approved_routing, include_status=routing_states_frozen)
                self.seal(run, label, 'before', before)
                self.seal(run, label, 'routing', routes)
                self.seal(run, label, 'backup', backup)
                self.save(run, label, 'prepared', started_at=started_at,
                          original_statuses=routing.expected(routes, restored=True),
                          warnings=before.get('warnings', []), excluded_tests=[*before.get('excluded_tests', []), *deferred_rows],
                          held_channels=sorted(held_channels), fast_model_tests=sorted(fast_model_tests),
                          acceptance_mode=acceptance_mode)
                continue
            before = self.snapshot(run, label, 'before')
            routes = self.snapshot(run, label, 'routing')
            if phase in {'prepared', 'staging'}:
                routing.confirm(routes, restored=True)
                if not self.store.run(run)['data'].get('release'):
                    qualified = self.releases.prepare(release_description)
                    self.store.status(run, 'running', release=qualified)
                release = self.store.run(run)['data']['release']
                self.save(run, label, 'staging')
                with operation('stage_release', kind='deployment'):
                    staged = adapter.stage(before, release, run)
                self.seal(run, label, 'staged', staged)
                self.save(run, label, 'staged')
                continue
            release = self.store.run(run)['data']['release']
            staged = self.snapshot(run, label, 'staged')
            if phase in {'staged', 'stopping'}:
                def stop_intent():
                    self.save(run, label, 'stopping')

                try:
                    propagation = routing.set_stopped(routes, initial=phase == 'staged', record_intent=stop_intent)
                except OpsError as error:
                    changed = {'channel_status_changed', 'channel_set_changed', 'channel_configuration_changed'}
                    if error.code in changed and self.saved(run, label)['phase'] == 'staged':
                        # With no write intent, resume from a fresh preflight after an external edit.
                        self.save(run, label, 'pending')
                    raise
                handle = self.driver.capture_drain()
                self.save(run, label, 'stopped', stopped_at=self.clock(), stop_propagation=propagation,
                          drain_handle=handle)
                continue
            if phase in {'stopped', 'draining'}:
                evidence = self.drain(run, label, routing, routes)
                self.save(run, label, 'drained', drain=evidence)
                continue
            if phase in {'drained', 'pinning'}:
                routing.confirm(routes)
                self.save(run, label, 'pinning')
                expected = adapter.pin(before)
                self.seal(run, label, 'expected-config', expected)
                adjustments = [] if expected == before['config_semantics'] else [
                    {'path': 'management.disable-auto-update-panel', 'value': True, 'purpose': 'pin_release_frontend'}]
                self.save(run, label, 'deploying', deploy_started_at=self.clock(), deploy_command_intent=False,
                          configuration_adjustments=adjustments)
                continue
            expected = self.snapshot(run, label, 'expected-config')
            if phase == 'deploying':
                routing.confirm(routes)
                current = adapter.deployment_state(before, release, staged)
                if current['state'] == 'old' and not data.get('deploy_command_acknowledged'):
                    # Retry an unacknowledged command only after the old runtime is confirmed.
                    adapter.confirm_before_deploy(before, expected)
                    self.save(run, label, phase, deploy_command_intent=True)
                    with operation('deploy_release', kind='deployment'):
                        adapter.deploy(before, release, staged, run)
                    self.save(run, label, phase, deploy_command_acknowledged=True)
                    continue
                if current['state'] == 'running':
                    self.save(run, label, 'deployed', deployment=current)
                    continue
                if current['state'] == 'failed':
                    raise OpsError('platform_deployment_failed', category='regression')
                elapsed = self.clock() - data['deploy_started_at']
                if current['state'] == 'old' and target['kind'] == 'zeabur' and elapsed >= 120 and not data.get('redeploy_intent'):
                    self.save(run, label, phase, redeploy_intent=True)
                    adapter.redeploy(staged)
                require(elapsed < 1800, 'deployment_outcome_unconfirmed')
                with operation('deployment_poll_wait', kind='wait'):
                    self.sleep(10)
                continue
            if phase in {'deployed', 'rollback_deployed'}:
                rollback = phase.startswith('rollback')
                routing.confirm(routes)
                # Startup probes may temporarily fail while the runtime is starting.
                deadline = self.monotonic() + 180
                while True:
                    try:
                        after = adapter.verify(before, release, staged, expected, rollback=rollback)
                        break
                    except OpsError as error:
                        if error.code not in {'application_read_failed', 'http_transport_failed'}:
                            raise
                        if self.monotonic() >= deadline:
                            raise OpsError('new_runtime_health_failed', category='regression') from None
                        note('retry', scope='runtime_startup', code=error.code)
                        with operation('runtime_startup_wait', kind='wait'):
                            self.sleep(5)
                self.seal(run, label, 'verified', after)
                self.save(run, label, 'rollback_verified' if rollback else 'verified',
                          verified_fingerprint=catalog.fingerprint(after))
                continue
            if phase in {'verified', 'model_testing', 'rollback_verified', 'rollback_model_testing'}:
                rollback = phase.startswith('rollback')
                routing.confirm(routes)
                current_phase = 'rollback_model_testing' if rollback else 'model_testing'
                key = 'rollback_model_results' if rollback else 'model_results'
                exclusions = data.get('model_test_exclusions', {})
                results = {job_id: result for job_id, result in (data.get(key) or {}).items() if job_id not in exclusions}
                failed_quota_models = {result['model'] for result in results.values()
                                       if result.get('success') is False and result.get('category') == 'quota'
                                       and not is_claude_model(result['model'])}
                if phase == current_phase:
                    after = adapter.verify(before, release, staged, expected, rollback=rollback, quota_models=failed_quota_models)
                    self.seal(run, label, 'verified', after)
                else:
                    after = self.snapshot(run, label, 'verified')
                jobs, excluded = catalog.jobs(after)
                jobs = [job for job in jobs if job['id'] not in exclusions and job['id'] not in deferred_web_tests]
                excluded = [entry for entry in excluded if f"{entry.get('provider')}:{entry['model']}" not in exclusions]
                excluded.extend(exclusions.values())
                excluded.extend(deferred_rows)
                if fast_model_tests:
                    require(set(fast_model_tests) <= {job['id'] for job in jobs}, 'fast_smoke_model_unavailable')
                    excluded.extend({'provider': job['provider'], 'model': job['model'], 'reason': 'manual_validation_pending'}
                                    for job in jobs if job['id'] not in fast_model_tests)
                    jobs = [job for job in jobs if job['id'] in fast_model_tests]
                require(jobs, 'no_eligible_models_for_verification')
                if failed_quota_models and acceptance_mode == 'full':
                    catalog.quota_check(after, [job for job in jobs if job['model'] in failed_quota_models],
                                        require_positive=True)
                    # Prioritize the models that blocked the previous run.
                    jobs.sort(key=lambda job: job['model'] not in failed_quota_models)
                identity = {'runtime': deployment_identity(after), 'configuration': catalog.fingerprint(after),
                            'rollback': rollback}
                for job in jobs:
                    if acceptance_mode == 'availability':
                        job['availability_only'] = True
                    job['fingerprint'] = digest([identity, job])
                required_models = [job['id'] for job in jobs]
                results = {job_id: result for job_id, result in results.items() if job_id in required_models}
                self.save(run, label, current_phase, required_models=required_models, excluded_tests=excluded,
                          acceptance_mode=acceptance_mode, **{key: results})

                def save_result(job_id, result):
                    results[job_id] = result
                    self.save(run, label, current_phase, **{key: results})
                    self.emit('model_test', target=label, model=result['model'], success=result.get('success'))

                tested = testing.run_jobs(jobs, lambda job: testing.retry_call(
                    lambda: self.driver.model_call(adapter, job, after), sleep=self.sleep), save_result,
                    results, clock=self.clock, ttl=TEST_TTL)
                failures = [result for result in tested.values() if result.get('success') is not True]
                if failures:
                    category = failures[0].get('category', 'functional')
                    raise OpsError('required_model_call_failed', category=category)
                require(all(tested.get(job['id'], {}).get('success') is True for job in jobs), 'required_models_not_all_tested')
                self.save(run, label, 'rollback_models_passed' if rollback else 'models_passed', **{key: tested})
                continue
            if phase in {'models_passed', 'channel_testing', 'rollback_models_passed', 'rollback_channel_testing'}:
                rollback = phase.startswith('rollback')
                current_phase = 'rollback_channel_testing' if rollback else 'channel_testing'
                self.save(run, label, current_phase, acceptance_mode=acceptance_mode)
                results = {}
                channel_exclusions = EXCLUDED_MODELS | {value['model'] for value in data.get('model_test_exclusions', {}).values()}
                channel_exclusions |= {entry['model'] for entry in data.get('excluded_tests', [])
                                       if entry.get('reason') == 'user_excluded_claude_model'}
                if acceptance_mode == 'availability':
                    routing.confirm(routes)
                    verified = self.snapshot(run, label, 'verified')
                    model_jobs, _ = catalog.jobs(verified)
                    verification_identity = [deployment_identity(verified), catalog.fingerprint(verified), digest(model_jobs)]
                    jobs, rows_by_id = [], {}
                    for site, rows in routes['channels'].items():
                        for row in rows:
                            channel_id = f"{site}:{row['id']}"
                            if row['status'] != 1 or channel_id in held_channels:
                                continue
                            chosen, mapped = channel_test_model(row, channel_exclusions)
                            matches = [job for job in model_jobs if chosen == job['model'] or mapped in job['upstream_models']]
                            resources = sorted({resource for job in matches for resource in job['resources']}) or ['*']
                            jobs.append({'id': channel_id, 'model': chosen, 'provider': 'new-api', 'resources': resources,
                                         'fingerprint': digest([verification_identity, site, channel_fingerprint(row), chosen,
                                                                acceptance_mode, rollback, sorted(channel_exclusions)])})
                            rows_by_id[channel_id] = site, row

                    def save_channel(job_id, result):
                        results[job_id] = result
                        self.save(run, label, current_phase, channel_results=results)

                    def call_channel(job):
                        site, row = rows_by_id[job['id']]
                        return testing.retry_call(lambda: routing.test_channel(
                            routes, site, row, channel_exclusions, verify_snapshot=False), sleep=self.sleep)

                    results = testing.run_jobs(jobs, call_channel, save_channel,
                                               data.get('channel_results', {}), clock=self.clock, ttl=TEST_TTL)
                    failures = [result for result in results.values() if result.get('success') is not True]
                    if failures:
                        raise OpsError('channel_functional_test_failed', category=failures[0].get('category', 'functional'))
                    require(all(job['id'] in results for job in jobs), 'required_channels_not_all_tested')
                    checked_at = min((result['checked_at'] for result in results.values()), default=self.clock())
                    self.save(run, label, 'rollback_tested' if rollback else 'tested', channel_results=results,
                              channel_tests_at=checked_at)
                    continue
                # Channels are always retested before restoration after a resume.
                for site, rows in routes['channels'].items():
                    for row in rows:
                        if row['status'] != 1 or f"{site}:{row['id']}" in held_channels:
                            continue
                        result = testing.retry_call(lambda: routing.test_channel(routes, site, row, channel_exclusions), sleep=self.sleep)
                        results[site + ':' + str(row['id'])] = result
                        self.save(run, label, current_phase, channel_results=results)
                        require(result.get('success') is True, 'channel_functional_test_failed', category=result.get('category', 'functional'))
                self.save(run, label, 'rollback_tested' if rollback else 'tested', channel_results=results,
                          channel_tests_at=self.clock())
                continue
            if phase in {'tested', 'rollback_tested'}:
                self.save(run, label, 'rollback_restoring' if phase.startswith('rollback') else 'restoring')
                continue
            if phase in {'restoring', 'rollback_restoring'}:
                rollback = phase.startswith('rollback')
                key = 'rollback_model_results' if rollback else 'model_results'
                checked_at = [data['channel_tests_at'], *(result.get('checked_at', 0) for result in data.get(key, {}).values())]
                if any(not 0 <= self.clock() - timestamp <= TEST_TTL for timestamp in checked_at):
                    routing.set_stopped(routes)
                    self.save(run, label, 'rollback_verified' if rollback else 'verified')
                    continue
                fresh = adapter.verify(before, release, staged, expected, rollback=rollback)
                verified = self.snapshot(run, label, 'verified')
                current_jobs, _ = catalog.jobs(fresh)
                previous_jobs, _ = catalog.jobs(verified)
                if (deployment_identity(fresh) != deployment_identity(verified)
                        or catalog.fingerprint(fresh) != catalog.fingerprint(verified) or digest(current_jobs) != digest(previous_jobs)):
                    routing.set_stopped(routes)
                    self.seal(run, label, 'verified', fresh)
                    self.save(run, label, 'rollback_verified' if rollback else 'verified')
                    continue
                restore = restoration_routes(routes, held_channels)
                restoring_started = any(event['event'] == 'propagation_intent' and event['data'].get('status') == 1
                                        for event in self.store.events(run) if event['target'] == label)
                def still_fresh():
                    require(all(0 <= self.clock() - timestamp <= TEST_TTL for timestamp in checked_at),
                            'acceptance_expired_before_restore')

                try:
                    propagation = routing.set_stopped(restore, restore=True, initial=not restoring_started, passed_channels=data['channel_results'],
                                                      validate_before_write=still_fresh)
                except OpsError as error:
                    if error.code != 'acceptance_expired_before_restore':
                        raise
                    routing.set_stopped(routes)
                    self.save(run, label, 'rollback_verified' if rollback else 'verified')
                    continue
                for site, stopped in data['stop_propagation'].items():
                    if stopped.get('startups') and any(row['status'] == 1 for row in restore['channels'][site]):
                        require(propagation[site].get('startups') == stopped['startups'], 'newapi_restarted_during_maintenance')
                self.save(run, label, 'rollback_restored' if rollback else 'restored', restore_propagation=propagation)
                continue
            if phase in {'restored', 'rollback_restored'}:
                rollback = phase.startswith('rollback')
                routing.confirm(restoration_routes(routes, held_channels), restored=True)
                require(routing.topology() == routes['topology'], 'newapi_process_or_topology_changed')
                with operation('postflight', kind='verification'):
                    result = adapter.postflight(before, release, staged, expected, rollback=rollback)
                active_holds = any(row['status'] == 1 and f"{site}:{row['id']}" in held_channels
                                   for site, rows in routes['channels'].items() for row in rows)
                finished = 'complete_with_holds' if active_holds else 'complete'
                self.save(run, label, 'rolled_back' if rollback else finished, completed_at=self.clock(),
                          postflight=result, newapi_not_restarted=True, original_routing_restored=not active_holds,
                          **({} if rollback else {'last_error': None}))
                continue
            if phase in {'rollback_stopping', 'rollback_draining'}:
                if phase == 'rollback_stopping':
                    routing.set_stopped(routes)
                    self.save(run, label, 'rollback_draining', rollback_drain_handle=self.driver.capture_drain())
                evidence = self.drain(run, label, routing, routes, rollback=True)
                self.save(run, label, 'rollback_ready', rollback_drain=evidence)
                continue
            if phase == 'rollback_ready':
                routing.confirm(routes)
                if adapter.rollback_state(before, staged):
                    self.save(run, label, 'rollback_deployed')
                    continue
                self.save(run, label, 'rollback_waiting', rollback_command_intent=True, rollback_started_at=self.clock())
                with operation('rollback_release', kind='deployment'):
                    adapter.rollback(before, staged, run)
                continue
            if phase == 'rollback_waiting':
                routing.confirm(routes)
                if adapter.rollback_state(before, staged):
                    self.save(run, label, 'rollback_deployed')
                    continue
                require(self.clock() - data['rollback_started_at'] < 1800, 'rollback_outcome_unconfirmed')
                with operation('rollback_poll_wait', kind='wait'):
                    self.sleep(10)
                continue
            raise OpsError('unsupported_recorded_phase')

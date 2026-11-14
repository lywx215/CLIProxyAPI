"""Deterministic in-memory production substitutes, for tests only."""
import copy
from pathlib import Path

from scripts.ops import catalog
from scripts.ops.adapters import Adapter as LiveAdapter
from scripts.ops.core import OpsError, digest, require
from scripts.ops.routing import Routing


COMMIT = 'a' * 40


class Clock:
    def __init__(self):
        self.now = 1000.0

    def __call__(self):
        return self.now

    def sleep(self, seconds):
        self.now += seconds


def baseline(label):
    model = 'gemini-3.1-pro-preview'
    return {'config_semantics': {'management': {'disable-auto-update-panel': True}, 'payload': {'untouched': True}},
            'credentials': [{'name': 'synthetic-' + label, 'type': 'gemini-enterprise', 'disabled': False,
                             'gemini_enterprise': {'collections': [{'binding_id': 'b', 'project_id': 'synthetic',
                                                                    'location': 'global', 'disabled': False}]}}],
            'models': [{'id': model}], 'model_key': 'SECRET-NEVER-REPORT',
            'catalogs': {'synthetic-' + label: [{'id': model}]}, 'web': {'enabled': False},
            'runtime': {'binary_sha256': 'old-' + label}, 'page_sha256': 'old-page',
            'domains': [label + '.example.test'], 'legacy': False, 'warnings': [], 'excluded_tests': []}


class Releases:
    def __init__(self):
        self.calls = 0

    def prepare(self, release):
        self.calls += 1
        return {**release, 'package_sha256': 'package', 'binary_sha256': 'new-binary'}


class Driver:
    def __init__(self, labels=('one', 'two')):
        self.inventory = {'schema': 1, 'targets': {label: {'kind': 'vps', 'host': '192.0.2.' + str(index + 1),
            'origin': 'http://192.0.2.' + str(index + 1) + ':8317', 'ssh': 'fixture',
            'base': '/opt/cliproxyapi', 'project': 'cliproxyapi-vps'} for index, label in enumerate(labels)}}
        self.runtimes = {label: 'old' for label in labels}
        self.channels = {label: {1: 1, 2: 2, 3: 3} for label in labels}
        self.calls, self.stages, self.deploys, self.rollbacks = [], {}, {}, {}
        self.failure = None
        self.fail_target = labels[0]
        self.fired = False
        self.on_model = None
        self.counts = None
        self.guard = lambda: None
        self.config_drift = False

    def inventory_fingerprint(self):
        return digest(self.inventory)

    def adapter(self, target, guard):
        return Adapter(self, target, guard)

    def routing(self, guard):
        return Routes(self, guard)

    def capture_drain(self):
        return {'scope': 'all-providers'} if self.counts else None

    def live_counts(self, handle):
        return self.counts

    def model_call(self, adapter, job, snapshot):
        adapter.guard()
        label = adapter.target['label']
        require(self.channels[label][1] == 2, 'model_test_while_enabled')
        self.calls.append(('model', label, self.runtimes[label]))
        if self.on_model:
            self.on_model()
        if label == self.fail_target and self.failure == 'quota':
            return {'success': False, 'http': 429, 'category': 'quota'}
        return {'success': True, 'http': 200, 'seconds': .1, 'first_text_seconds': .05}


class Adapter:
    def __init__(self, driver, target, guard):
        self.driver, self.target, self.guard = driver, target, guard
        self.label = target['label']

    def preflight(self, release, *, deferred_web_tests=(), reuse=None, on_identity=None):
        self.driver.calls.append(('preflight', self.label))
        if self.driver.failure == 'preflight_auth' and self.label == self.driver.fail_target:
            raise OpsError('oauth_application_secret_missing', category='authorization')
        value = baseline(self.label)
        if on_identity:
            on_identity(value)
        return value

    def backup(self, before, *, reuse=None):
        return {'token': 'SECRET-NEVER-REPORT', 'config': before['config_semantics']}

    prepare = LiveAdapter.prepare

    def stage(self, before, release, run):
        self.guard()
        if self.label not in self.driver.stages:
            self.driver.stages[self.label] = 1
            self.driver.calls.append(('stage', self.label))
        return {'old_image': 'old', 'image_id': 'new', 'old_container': 'original'}

    def pin(self, before):
        self.guard()
        return before['config_semantics']

    def deployment_state(self, before, release, staged):
        self.guard()
        value = self.driver.runtimes[self.label]
        if self.driver.failure == 'deployment' and value == 'new' and self.label == self.driver.fail_target:
            return {'state': 'failed'}
        return {'state': 'running' if value == 'new' else 'old'}

    def confirm_before_deploy(self, before, expected):
        self.guard()
        require(not self.driver.config_drift, 'configuration_changed_outside_update')

    def deploy(self, before, release, staged, run):
        self.guard()
        require(self.driver.channels[self.label][1] == 2, 'deploy_while_enabled')
        if self.driver.runtimes[self.label] != 'new':
            self.driver.deploys[self.label] = self.driver.deploys.get(self.label, 0) + 1
            self.driver.calls.append(('deploy', self.label))
            self.driver.runtimes[self.label] = 'new'
        if self.driver.failure == 'lost_deploy_ack' and not self.driver.fired:
            self.driver.fired = True
            raise OpsError('http_transport_failed', category='network')

    def redeploy(self, staged):
        raise AssertionError('VPS must not request cloud redeploy')

    def verify(self, before, release, staged, expected, rollback=False, quota_models=()):
        self.guard()
        if self.driver.failure == 'program_regression' and self.driver.runtimes[self.label] == 'new':
            raise OpsError('runtime_binary_mismatch', category='regression')
        if self.driver.config_drift:
            raise OpsError('configuration_changed_outside_update')
        value = baseline(self.label)
        value['runtime'] = {'binary_sha256': 'old-' + self.label if rollback else 'new-binary'}
        value['page_sha256'] = before['page_sha256'] if rollback else release['page_sha256']
        catalog.preserve(before, value, expected_config=expected)
        return value

    def rollback(self, before, staged, run):
        self.guard()
        require(self.driver.channels[self.label][1] == 2, 'rollback_while_enabled')
        self.driver.rollbacks[self.label] = self.driver.rollbacks.get(self.label, 0) + 1
        if self.driver.failure == 'rollback_failure':
            raise OpsError('rollback_failed')
        self.driver.runtimes[self.label] = 'old'
        self.driver.calls.append(('rollback', self.label))

    def rollback_state(self, before, staged):
        return self.driver.runtimes[self.label] == 'old'

    def postflight(self, before, release, staged, expected, rollback=False):
        self.verify(before, release, staged, expected, rollback=rollback)
        self.driver.calls.append(('complete', self.label, rollback))
        return {'health_http': 200, 'configuration_preserved': True, 'runtime': {'version': self.driver.runtimes[self.label]}}


class Routes:
    expected = staticmethod(Routing.expected)

    def __init__(self, driver, guard):
        self.driver, self.guard = driver, guard

    def snapshot(self, domains):
        label = domains[0].split('.')[0]
        return {'domains': domains, 'channels': {'main': [{'id': cid, 'status': status, 'models': 'gemini-3.1-pro-preview',
                                                           'name': 'synthetic', 'label': label}
                                                          for cid, status in self.driver.channels[label].items()]},
                'topology': {'stable_process': 'original'}}

    def topology(self):
        return {'stable_process': 'original'}

    def confirm(self, snapshot, restored=False, transitional=False):
        label = snapshot['domains'][0].split('.')[0]
        expected = self.expected(snapshot, restored)
        for row in snapshot['channels']['main']:
            allowed = {1, 2} if transitional and row['status'] == 1 else {expected['main'][str(row['id'])]}
            require(self.driver.channels[label][row['id']] in allowed, 'channel_status_changed')
        return snapshot['channels']

    def set_stopped(self, snapshot, restore=False, passed_channels=None, initial=False, validate_before_write=None,
                    record_intent=None):
        self.guard()
        self.confirm(snapshot, restored=initial and not restore, transitional=not initial)
        if validate_before_write:
            validate_before_write()
        if record_intent:
            record_intent()
        label = snapshot['domains'][0].split('.')[0]
        if self.driver.failure == 'stop_propagation' and not restore:
            self.driver.channels[label][1] = 2
            raise OpsError('channel_stop_or_restore_propagation_incomplete')
        for row in snapshot['channels']['main']:
            if row['status'] == 1:
                require(not restore or passed_channels.get('main:' + str(row['id']), {}).get('success') is True,
                        'restore_requires_successful_channel_calls')
                self.driver.channels[label][row['id']] = 1 if restore else 2
        self.driver.calls.append(('restore' if restore else 'stop', label))
        return {'main': {'startups': {'backend': 123}}}

    def log_markers(self, max_seconds=10):
        if self.driver.failure == 'log_failure':
            raise OpsError('global_logs_unavailable')
        self.driver.log_count = getattr(self.driver, 'log_count', 0) + 1
        return {'main': self.driver.log_count, 'vip': self.driver.log_count}

    def test_channel(self, snapshot, site, row, exclusions, *, verify_snapshot=True):
        self.guard()
        self.confirm(snapshot)
        label = row['label']
        require(self.driver.channels[label][row['id']] == 2, 'channel_test_while_enabled')
        self.driver.calls.append(('channel', label))
        return {'success': self.driver.failure != 'channel', 'model': 'gemini-3.1-pro-preview',
                'site': site, 'channel': row['id'], 'seconds': .1, 'category': 'functional'}

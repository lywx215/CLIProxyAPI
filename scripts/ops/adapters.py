"""Production deployment adapters with explicit mutation guards and reconciliation."""
from __future__ import annotations

import contextlib
from .concurrency import TaskExecutor as ThreadPoolExecutor
import copy
import json
from pathlib import Path
import re
import shlex
import time
import threading
from urllib.parse import urlsplit

from .application import Application
from . import catalog
from .core import READ_SLOTS, OpsError, deployment_identity, digest, require, sha_file
from .release import git
from .transport import HTTP, remote_json
from .telemetry import operation, note


def oauth_reference(config):
    return (((config.get('oauth') or {}).get('providers') or {}).get('gemini-enterprise') or {}).get('client-secret-env') or \
        (config.get('gemini-enterprise') or {}).get('client-secret-env')


class Adapter:
    def __init__(self, target, guard, ssh_factory, zeabur=None, repository=None, repository_url=None, cloud_peers=()):
        self.target, self.guard, self.ssh = target, guard, ssh_factory
        self.z, self.repository, self.repository_url = zeabur, repository, repository_url
        self.cloud_peers = cloud_peers
        self.clients, self.client_lock = {}, threading.Lock()
        self.cloud_access, self.previous_snapshot = None, None

    def client(self, origin, key, ssh=None):
        identity = (origin, digest(key), id(ssh.get_transport()) if ssh is not None else None)
        with self.client_lock:
            if identity not in self.clients:
                self.clients[identity] = HTTP(origin, key=key, ssh=ssh)
            return self.clients[identity]

    def close(self):
        for client in self.clients.values():
            client.close()
        self.clients.clear()

    def remote(self, action, **spec):
        require(action in {'snapshot', 'runtime', 'stage', 'deploy', 'rollback'}, 'remote_action_rejected')
        if action in {'stage', 'deploy', 'rollback'}:
            self.guard()
        with (READ_SLOTS if action in {'snapshot', 'runtime'} else contextlib.nullcontext()), operation(
                'vps_' + action, kind='read' if action in {'snapshot', 'runtime'} else 'deployment'):
            client = self.ssh.connect(self.target['ssh'])
            try:
                return remote_json(client, Path(__file__).with_name('remote_vps.py').read_text(encoding='utf-8'),
                                   {**self.target, 'action': action, **spec})
            finally:
                client.close()

    @contextlib.contextmanager
    def application(self, host=None, *, cloud=None):
        if self.target['kind'] == 'zeabur':
            variables, domains = cloud if cloud is not None else (self.cloud_access or self.z.variables(self.target))
            require(urlsplit(self.target['origin']).hostname in domains, 'cloud_origin_binding_changed')
            key = variables.get('MANAGEMENT_PASSWORD')
            require(key, 'cloud_management_key_missing')
            yield Application(self.client(self.target['origin'], key), self.guard)
        else:
            host = host or self.remote('snapshot')
            with READ_SLOTS, operation('vps_application_transport'):
                client = self.ssh.connect(self.target['ssh'])
            try:
                yield Application(self.client('http://127.0.0.1:8317', host['management_key'], client), self.guard)
            finally:
                client.close()

    def vps_domains(self):
        return sorted(set([urlsplit(self.target['origin']).hostname, *self.target.get('addresses', [])]))

    def snapshot(self, *, quotas=False, quota_models=None, legacy_raw=False, reuse=None, on_identity=None, refresh_catalog=False):
        reuse = reuse or self.previous_snapshot
        def read(app, domains, host=None):
            return app.snapshot(quotas=quotas, quota_models=quota_models, legacy_raw=legacy_raw, reuse=reuse,
                                on_identity=(lambda value: on_identity({**value, 'domains': domains, 'host': host}))
                                if on_identity else None, refresh_catalog=refresh_catalog)
        if self.target['kind'] == 'vps':
            host = self.remote('snapshot')
            domains = self.vps_domains()
            with self.application(host) as app:
                value = read(app, domains, host)
            ref = oauth_reference(value['config'])
            require(not ref or host['environment'].get(ref), 'oauth_application_secret_missing', category='authorization')
            require(host['container']['State']['Running'], 'cli_container_not_running', category='regression')
            value.update(host=host, runtime={'binary_sha256': host['binary_sha256'], 'image_id': host['container']['Image'],
                                           'deployment_id': host['container']['Id'],
                                           'oauth_secret_present': not ref or bool(host['environment'].get(ref))},
                         domains=domains)
        else:
            with ThreadPoolExecutor(max_workers=2) as executor:
                platform_future = executor.submit(self.z.snapshot, self.target)
                variables_future = executor.submit(self.z.variables, self.target)
                platform = platform_future.result()
                variables, domains = variables_future.result()
            self.cloud_access = variables, domains
            require(platform['service']['gitTrigger']['branchName'] == self.target['branch'], 'deployment_branch_changed')
            with self.application(cloud=(variables, domains)) as app:
                value = read(app, domains)
            ref = oauth_reference(value['config'])
            runtime = self.z.runtime(self.target, ref)
            require(runtime['oauth_secret_present'], 'oauth_application_secret_missing', category='authorization')
            if platform['recent_deployments']:
                runtime['deployment_id'] = platform['recent_deployments'][0]['_id']
            value.update(platform=platform, runtime=runtime, domains=domains)
            store_keys = ['OBJECTSTORE_ENDPOINT', 'OBJECTSTORE_BUCKET', 'OBJECTSTORE_ACCESS_KEY', 'OBJECTSTORE_SECRET_KEY']
            value['persistent_store_ready'] = all(variables.get(key) for key in store_keys) if variables.get('OBJECTSTORE_ENDPOINT') else None
        self.previous_snapshot = value
        return value

    def preflight(self, release, *, deferred_web_tests=(), reuse=None, on_identity=None):
        before = self.snapshot(quotas=True, reuse=reuse, on_identity=on_identity)
        jobs, excluded = catalog.jobs(before)
        require(jobs, 'no_eligible_credentials_or_models')
        web_jobs = {job['id']: set(job['credentials']) for job in jobs if job['provider'] == 'gemini-enterprise-web'}
        rejected = {row['name'] for row in before['credentials']
                    if (row.get('gemini_enterprise_web') or {}).get('cookie_check_status') == 'relogin_required'}
        deferred = set(deferred_web_tests)
        unavailable = {job_id for job_id, credentials in web_jobs.items() if credentials and credentials <= rejected}
        require(deferred <= unavailable, 'web_deferral_does_not_match_known_auth_failure', category='authorization')
        require(not any(credentials & rejected for job_id, credentials in web_jobs.items() if job_id not in deferred),
                'required_web_credential_relogin', category='authorization')
        require(any(job['id'] not in deferred for job in jobs), 'no_eligible_models_for_verification')
        warnings = catalog.quota_check(before, jobs)
        section = 'remote-management' if before['legacy'] else 'management'
        require(not before['legacy'] or (before['config_semantics'].get(section) or {}).get('disable-auto-update-panel') is True,
                'legacy_panel_pin_requires_separate_preparation')
        if self.target['kind'] == 'zeabur':
            platform = before['platform']
            require(platform['service']['status'] == 'RUNNING' and platform['recent_deployments'], 'cloud_old_runtime_unhealthy')
            require(platform['recent_deployments'][0]['status'] == 'RUNNING', 'cloud_deployment_already_in_progress')
            cached = reuse or {}
            binding_checked_at = cached.get('binding_checked_at', 0)
            deployment_id = before['runtime'].get('deployment_id')
            same_deployment = deployment_id and cached.get('runtime', {}).get('deployment_id') == deployment_id
            if not same_deployment or not 0 <= time.time() - binding_checked_at <= 120:
                self.z.unique_binding(self.target, self.repository_url)
                binding_checked_at = time.time()
            else:
                note('cache_hit', scope='zeabur_preflight_binding')
            before['binding_checked_at'] = binding_checked_at
            with READ_SLOTS, operation('git_read_branch'):
                tip = git(self.repository, 'ls-remote', 'origin', 'refs/heads/' + self.target['branch']).decode().split()[0]
            git(self.repository, 'merge-base', '--is-ancestor', tip, release['commit'])
            require(before['persistent_store_ready'] is True or self.target.get('persistent_mount_verified') is True,
                    'persistent_store_not_verified')
            before['branch_tip'] = tip
            runtime_page = before['runtime']['disk_page_sha256']
            require(runtime_page == before['page_sha256'], 'served_panel_differs_from_runtime_file')
        else:
            host = before['host']
            require(host['resources']['architecture'] in {'x86_64', 'amd64'}, 'unsupported_vps_architecture')
            require(host['resources']['disk_free_bytes'] >= 1024 ** 3, 'insufficient_disk_space')
            require(host['resources']['memory']['MemAvailable'] >= 256 * 1024 ** 2, 'insufficient_memory')
            forbidden = {'/CLIProxyAPI/CLIProxyAPI', '/CLIProxyAPI/static', '/CLIProxyAPI/static/management.html'}
            require(not any(mount['Destination'] in forbidden for mount in host['container']['Mounts']), 'runtime_override_mount')
        before['warnings'], before['excluded_tests'] = warnings, excluded
        return before

    def backup(self, before, *, reuse=None):
        with self.application(before.get('host')) as app:
            return app.backup(before, reuse=reuse)

    def prepare(self, routing, release, *, deferred_web_tests=(), reuse=None):
        """Overlap routing and backup reads with catalog and quota requests."""
        reuse = reuse or {}
        with ThreadPoolExecutor(max_workers=2) as executor, operation('preflight', kind='phase'):
            def identified(identity):
                pending.append(executor.submit(routing.snapshot, identity['domains']))
                pending.append(executor.submit(self.backup, identity, reuse=reuse.get('backup')))
            pending = []
            before = self.preflight(release, deferred_web_tests=deferred_web_tests, reuse=reuse.get('target'),
                                    on_identity=identified)
            routes, backup = (future.result() for future in pending)
        return {'target': before, 'routing': routes, 'backup': backup, 'prepared_at': time.time()}

    def stage(self, before, release, run_id):
        self.guard()
        if self.target['kind'] == 'zeabur':
            peers = self.z.deployment_ids(item for item in self.cloud_peers if item['label'] != self.target['label'])
            return {'previous_deployment': before['platform']['recent_deployments'][0],
                    'previous_branch_commit': before['branch_tip'], 'peer_deployments': peers}
        require(sha_file(release['package']) == release['package_sha256'], 'release_package_changed')
        host = before['host']
        directory = self.target['base'] + '/releases/ops-' + run_id
        client = self.ssh.connect(self.target['ssh'])
        try:
            # Only the dedicated, validated deployment directory can be created.
            source = 'import json,pathlib,sys\ns=json.load(sys.stdin)\nr=pathlib.Path(s["base"]).resolve()\np=r/"releases"/("ops-"+s["run"])\nif str(r) not in ["/opt/cliproxyapi","/opt/cliproxyapi-netcup"] or not p.resolve().is_relative_to(r/"releases") or p.is_symlink(): raise ValueError("unsafe_stage_path")\np.mkdir(parents=True,exist_ok=True,mode=0o700)\nprint(json.dumps({"path":str(p)}))'
            value = remote_json(client, source, {'base': self.target['base'], 'run': run_id})
            require(value['path'] == directory, 'stage_path_changed')
            with client.open_sftp() as sftp:
                sftp.put(release['package'], directory + '/runtime.tar.gz')
                sftp.chmod(directory + '/runtime.tar.gz', 0o600)
        finally:
            client.close()
        self.guard()
        return self.remote('stage', run_id=run_id, expected_container=host['container']['Id'],
                           compose_sha256=host['compose_sha256'], **{key: release[key] for key in
                           ['commit', 'package_sha256', 'binary_sha256', 'page_sha256']})

    def pin(self, before):
        self.guard()
        with self.application() as app:
            return app.panel_pin(before)

    def confirm_before_deploy(self, before, expected_config):
        self.guard()
        fresh = self.snapshot(legacy_raw=before['legacy'])
        catalog.preserve(before, fresh, expected_config=expected_config)
        require(fresh['runtime']['binary_sha256'] == before['runtime']['binary_sha256'],
                'runtime_changed_before_deploy')
        if self.target['kind'] == 'vps':
            old, new = before['host'], fresh['host']
            require(old['container']['Id'] == new['container']['Id'] and old['processes'] == new['processes'],
                    'process_changed_before_deploy')
            require(old['protected_files'] == new['protected_files'] and old['branch'] == new['branch'],
                    'vps_configuration_changed_before_deploy')
        else:
            require(fresh['platform']['recent_deployments'][0]['_id'] == before['platform']['recent_deployments'][0]['_id'],
                    'cloud_deployment_changed_before_deploy')

    def deployment_state(self, before, release, staged):
        if self.target['kind'] == 'vps':
            value = self.remote('runtime')
            if value['image'] == staged['image_id']:
                state = 'running' if value['running'] else ('building' if value.get('status') == 'created' else 'failed')
                return {'state': state,
                        'identity': value['id'], 'image_id': staged['image_id']}
            require(value['image'] == staged['old_image'], 'unexpected_deployment_image')
            require(value['id'] == staged['old_container'], 'external_container_replacement')
            return {'state': 'old'}
        platform = self.z.snapshot(self.target)
        require(platform['service']['gitTrigger']['branchName'] == self.target['branch'], 'deployment_branch_changed')
        latest = platform['recent_deployments'][0]
        previous = staged['previous_deployment']
        if latest['_id'] == previous['_id']:
            return {'state': 'old'}
        require(latest.get('commitSHA') == release['commit'], 'unexpected_cloud_deployment')
        state = 'running' if latest['status'] == 'RUNNING' else ('failed' if latest['status'] in
                {'FAILED', 'CRASHED', 'CANCELED', 'CANCELLED', 'REMOVED'} else 'building')
        return {'state': state, 'identity': latest['_id'], 'deployment': latest}

    def deploy(self, before, release, staged, run_id):
        self.guard()
        if self.target['kind'] == 'vps':
            host = before['host']
            try:
                return self.remote('deploy', run_id=run_id, compose_sha256=host['compose_sha256'], protected_files=host['protected_files'])
            except OpsError as error:
                if error.code in {'remote_command_failed', 'remote_container_image_mismatch'}:
                    raise OpsError('cli_container_recreation_failed', category='regression') from None
                raise
        require(git(self.repository, 'remote', 'get-url', '--push', 'origin').decode().strip() == self.repository_url,
                'git_push_origin_changed')
        self.z.unique_binding(self.target, self.repository_url)
        branch = 'refs/heads/' + self.target['branch']
        head = git(self.repository, 'ls-remote', 'origin', branch).decode().split()[0]
        if head == release['commit']:
            return {'branch_already_pushed': True}
        require(head == staged['previous_branch_commit'], 'branch_changed_since_preflight')
        git(self.repository, 'merge-base', '--is-ancestor', head, release['commit'])
        git(self.repository, 'push', 'origin', release['commit'] + ':' + branch)
        require(git(self.repository, 'ls-remote', 'origin', branch).decode().split()[0] == release['commit'], 'push_unconfirmed')
        return {'branch_pushed': True}

    def redeploy(self, staged):
        self.guard()
        require(self.target['kind'] == 'zeabur', 'redeploy_not_cloud')
        self.z.unique_binding(self.target, self.repository_url)
        latest = self.z.snapshot(self.target)['recent_deployments'][0]
        require(latest['_id'] == staged['previous_deployment']['_id'], 'cloud_deployment_already_started')
        self.z.redeploy(self.target)

    def rollback(self, before, staged, run_id):
        self.guard()
        if self.target['kind'] == 'vps':
            host = before['host']
            return self.remote('rollback', run_id=run_id, compose_sha256=host['compose_sha256'], protected_files=host['protected_files'])
        self.z.unique_binding(self.target, self.repository_url)
        self.z.rollback(staged['previous_deployment']['_id'])
        return {'requested': True}

    def rollback_state(self, before, staged):
        if self.target['kind'] == 'vps':
            host = self.remote('runtime')
            return host['image'] == staged['old_image'] and host['running']
        platform = self.z.snapshot(self.target)
        require(platform['service']['gitTrigger']['branchName'] == self.target['branch'], 'deployment_branch_changed')
        latest = platform['recent_deployments'][0]
        if latest['status'] != 'RUNNING':
            return False
        runtime = self.z.runtime(self.target, oauth_reference(before['config']))
        return runtime['binary_sha256'] == before['runtime']['binary_sha256'] and runtime['disk_page_sha256'] == before['page_sha256']

    def verify(self, before, release, staged, expected_config, *, rollback=False, quota_models=()):
        self.guard()
        previous = self.previous_snapshot
        refresh = previous is None or previous.get('catalog_release') != (release['commit'], rollback)
        after = self.snapshot(quotas=bool(quota_models), quota_models=quota_models, legacy_raw=before['legacy'], refresh_catalog=refresh)
        if not refresh and deployment_identity(previous) != deployment_identity(after):
            after = self.snapshot(quotas=bool(quota_models), quota_models=quota_models, legacy_raw=before['legacy'], refresh_catalog=True)
        after['catalog_release'] = (release['commit'], rollback)
        catalog.preserve(before, after, expected_config=expected_config)
        expected_page = before['page_sha256'] if rollback else release['page_sha256']
        require(after['page_sha256'] == expected_page, 'deployed_frontend_mismatch', category='regression')
        if self.target['kind'] == 'vps':
            old, new = before['host'], after['host']
            other = lambda host: [row for row in host['processes'] if row['id'] != host['container']['Id']]
            require(other(old) == other(new), 'other_container_changed')
            require(old['branch'] == new['branch'] and old['protected_files'] == new['protected_files'], 'vps_deployment_configuration_changed')
            environment = lambda host: {key: value for key, value in host['environment'].items() if key != 'CPA_IMAGE'}
            require(environment(old) == environment(new), 'vps_runtime_environment_changed')
            desired = staged['old_image'] if rollback else staged['image_id']
            require(new['container']['Image'] == desired, 'runtime_image_mismatch', category='regression')
            expected_binary = old['binary_sha256'] if rollback else release['binary_sha256']
            require(new['binary_sha256'] == expected_binary, 'runtime_binary_mismatch', category='regression')
        elif not rollback:
            latest = after['platform']['recent_deployments'][0]
            require(latest['status'] == 'RUNNING' and latest.get('commitSHA') == release['commit'], 'cloud_source_not_deployed', category='regression')
        else:
            require(after['runtime']['binary_sha256'] == before['runtime']['binary_sha256'],
                    'rollback_binary_mismatch', category='regression')
        return after

    def postflight(self, before, release, staged, expected_config, *, rollback=False):
        after = self.verify(before, release, staged, expected_config, rollback=rollback)
        with self.application(after.get('host')) as app:
            logs = app.log_review()
        summary = {'health_http': after['health_http'], 'management_http': after['management_http'],
                   'runtime': after['runtime'], 'page_sha256': after['page_sha256'],
                   'version_header': after['version_header'], 'commit_header': after['commit_header'],
                   'credential_count': len(after['credentials']), 'configuration_preserved': True, 'log_review': logs}
        if self.target['kind'] == 'zeabur':
            deployment = after['platform']['recent_deployments'][0]
            summary['deployment'] = deployment
            summary['image_manifest_digests'] = self.z.image_digests(self.target, deployment['_id'])
            expected_peers = staged.get('peer_deployments', {})
            actual_peers = self.z.deployment_ids(peer for peer in self.cloud_peers if peer['label'] in expected_peers)
            require(actual_peers == expected_peers, 'other_cli_deployment_changed')
            summary['other_cloud_deployments_unchanged'] = True
        return summary

import copy
import hashlib
import json
from pathlib import Path
import tarfile
import tempfile
import threading
import time
import unittest
from unittest.mock import MagicMock, patch
from urllib.parse import parse_qs, urlsplit

import yaml

from scripts.ops.adapters import Adapter
from scripts.ops.application import Application
from scripts.ops.core import OpsError, digest
from scripts.ops import catalog, remote_vps
from scripts.ops.testing import model_call
from scripts.ops.tests.fakes import baseline


class AdapterTests(unittest.TestCase):
    def test_enterprise_verification_hashes_fresh_file_content_and_ignores_only_catalog_check_time(self):
        name = 'enterprise + account.json'
        row = {'name': name, 'type': 'gemini-enterprise', 'source': 'file', 'size': 128, 'modtime': 'first',
               'auth_index': 'enterprise', 'last_refresh': 'first', 'gemini_enterprise': {'revision': 1}}
        other_rows = [dict(row, name='disabled.json', disabled=True), dict(row, name='runtime.json', runtime_only=True),
                      dict(row, name='web.json', type='gemini-enterprise-web')]
        document = {'type': 'gemini-enterprise', 'access_token': 'synthetic-access', 'refresh_token': 'synthetic-refresh',
                    'enterprise_catalog_cache': {'binding': {'checked_at': 'first', 'models': ['gemini-4-carbon']}}}
        model = {'id': 'gemini-4-carbon'}
        fixture = {'/v8/management/config': {'api-keys': ['synthetic']}, '/v8/management/config.yaml': '{}',
                   '/v1/models': {'data': [model]}, '/healthz': {}, '/management.html': '<html>',
                   '/v8/management/credentials/models': {'models': [model]},
                   '/v8/management/credentials/gemini-enterprise-web/status': {'enabled': False}}

        def read(path, **kwargs):
            url = urlsplit(path)
            if url.path == '/v8/management/credentials':
                data = {'files': copy.deepcopy([row, *other_rows])}
            elif url.path == '/v8/management/credentials/download':
                self.assertEqual(parse_qs(url.query), {'name': [name]})
                data = copy.deepcopy(document)
            else:
                data = fixture[url.path]
            return {'data': data, 'http': 200, 'headers': {}}

        app = Application(MagicMock())
        app.read = MagicMock(side_effect=read)
        first = app.snapshot(verify_credential_content=True)
        self.assertEqual(set(first['enterprise_file_versions']), {name})
        self.assertNotIn('synthetic-refresh', json.dumps(first))
        row.update(modtime='second', size=129)
        document['enterprise_catalog_cache']['binding']['checked_at'] = 'second'
        second = app.snapshot(verify_credential_content=True, reuse=first)
        self.assertEqual(catalog.fingerprint(first), catalog.fingerprint(second))
        for path, value in [('access_token', 'changed-access'), ('refresh_token', 'changed-refresh'),
                            ('checked_at', 'outside-cache'), ('enterprise_catalog_cache', {
                                'binding': {'checked_at': 'second', 'models': ['gemini-4-argon']}})]:
            with self.subTest(field=path), patch.dict(document, {path: value}):
                changed = app.snapshot(verify_credential_content=True, reuse=second)
                self.assertNotEqual(catalog.fingerprint(second), catalog.fingerprint(changed))
        with patch.dict(document, {'type': 'wrong-provider'}):
            with self.assertRaisesRegex(OpsError, 'credential_content_identity_unavailable'):
                app.snapshot(verify_credential_content=True)

    def test_failed_catalog_verification_never_reuses_that_catalog(self):
        adapter = Adapter({'kind': 'zeabur'}, lambda: None, None)
        before = baseline('one')
        ready = baseline('one')
        ready.update(page_sha256='new-page', platform={
            'recent_deployments': [{'status': 'RUNNING', 'commitSHA': 'new-commit'}]})
        missing = {**ready, 'catalogs': {before['credentials'][0]['name']: []}}
        snapshots = iter((missing, ready))

        def snapshot(**kwargs):
            value = next(snapshots)
            adapter.previous_snapshot = value
            return value

        adapter.snapshot = MagicMock(side_effect=snapshot)
        verify_args = (before, {'commit': 'new-commit', 'page_sha256': 'new-page'}, {}, before['config_semantics'])
        with self.assertRaisesRegex(OpsError, 'required_text_model_disappeared'):
            adapter.verify(*verify_args)
        self.assertIs(adapter.verify(*verify_args), ready)
        self.assertEqual([call.kwargs['refresh_catalog'] for call in adapter.snapshot.call_args_list], [True, True])
        self.assertTrue(all(call.kwargs['verify_credential_content'] for call in adapter.snapshot.call_args_list))

    def test_preflight_rejects_present_web_cookie_marked_for_relogin(self):
        adapter = Adapter({'kind': 'vps'}, lambda: None, None)
        before = baseline('one')
        before['web'] = {'enabled': True}
        before['credentials'].append({'name': 'web', 'type': 'gemini-enterprise-web', 'auth_index': 'web',
                                     'gemini_enterprise_web': {'cookie_present': True, 'revision': 1,
                                                               'cookie_check_status': 'relogin_required'}})
        before['catalogs']['web'] = [{'id': 'gemini-4-carbon'}]
        before['host'] = {'resources': {'architecture': 'amd64', 'disk_free_bytes': 2 ** 31,
                                        'memory': {'MemAvailable': 2 ** 30}}, 'container': {'Mounts': []}}
        adapter.snapshot = MagicMock(return_value=before)
        with self.assertRaisesRegex(OpsError, 'required_web_credential_relogin') as raised:
            adapter.preflight({})
        self.assertEqual(raised.exception.category, 'authorization')
        deferred = ('gemini-enterprise-web:gemini-4-carbon',)
        self.assertIs(adapter.preflight({}, deferred_web_tests=deferred), before)
        before['credentials'][-1]['gemini_enterprise_web']['cookie_check_status'] = 'verified'
        with self.assertRaisesRegex(OpsError, 'web_deferral_does_not_match_known_auth_failure'):
            adapter.preflight({}, deferred_web_tests=deferred)

    def test_vps_failed_new_container_is_detected_without_docker_exec(self):
        adapter = Adapter({'kind': 'vps'}, lambda: None, None)
        adapter.remote = MagicMock(return_value={'image': 'new', 'id': 'container', 'running': False, 'status': 'exited'})
        result = adapter.deployment_state({}, {}, {'image_id': 'new', 'old_image': 'old', 'old_container': 'original'})
        self.assertEqual(result['state'], 'failed')
        adapter.remote.assert_called_once_with('runtime')

    def test_created_container_is_not_prematurely_rolled_back(self):
        adapter = Adapter({'kind': 'vps'}, lambda: None, None)
        adapter.remote = MagicMock(return_value={'image': 'new', 'id': 'container', 'running': False, 'status': 'created'})
        self.assertEqual(adapter.deployment_state({}, {}, {'image_id': 'new'})['state'], 'building')

    def test_external_old_container_replacement_blocks_deploy(self):
        adapter = Adapter({'kind': 'vps'}, lambda: None, None)
        adapter.remote = MagicMock(return_value={'image': 'old', 'id': 'external', 'running': True})
        with self.assertRaisesRegex(OpsError, 'external_container_replacement'):
            adapter.deployment_state({}, {}, {'image_id': 'new', 'old_image': 'old', 'old_container': 'original'})

    def test_panel_pin_lost_ack_is_idempotent_and_does_not_put_whole_config(self):
        app = Application(MagicMock())
        before = baseline('one')
        before['config_semantics']['management']['disable-auto-update-panel'] = False
        expected = copy.deepcopy(before['config_semantics'])
        expected['management']['disable-auto-update-panel'] = True
        app.read = MagicMock(return_value={'data': yaml.safe_dump(expected), 'headers': {'etag': 'v2'}})
        self.assertEqual(app.panel_pin(before), expected)
        app.http.request.assert_not_called()

    def test_panel_pin_rejects_external_config_change(self):
        before = baseline('one')
        before['config_semantics']['management']['disable-auto-update-panel'] = False
        app = Application(MagicMock())
        app.read = MagicMock(return_value={'data': 'management: {}\nchanged: true\n', 'headers': {'etag': 'v2'}})
        with self.assertRaisesRegex(OpsError, 'config_changed_before_panel_pin'):
            app.panel_pin(before)
        app.http.request.assert_not_called()

    def test_multicollection_reads_keep_quota_and_catalog_bound_to_each_enabled_collection(self):
        row = {'name': 'synthetic-account', 'type': 'gemini-enterprise', 'auth_index': 'account',
               'gemini_enterprise': {'collections': [
                   {'binding_id': 'one', 'auth_index': 'child-1'},
                   {'binding_id': 'two', 'auth_index': 'child-2'},
                   {'binding_id': 'disabled', 'auth_index': 'child-3', 'disabled': True}]}}
        requested = []
        barriers = {name: threading.Barrier(2) for name in ('one', 'two')}
        def read(path, **kwargs):
            requested.append(path)
            route, params = urlsplit(path).path, parse_qs(urlsplit(path).query)
            if route.endswith('/config.yaml'):
                data = 'config-version: 8\n'
            elif route.endswith('/config'):
                data = {'access': {'api-keys': ['synthetic']}}
            elif route.endswith('/credentials'):
                data = {'files': [row]}
            elif route.endswith('/credentials/models'):
                self.assertIn(params['binding_id'][0], {'one', 'two'})
                self.assertIn(params['auth_index'][0], {'child-1', 'child-2'})
                barriers[params['binding_id'][0]].wait(timeout=5)
                data = {'models': [{'id': params['binding_id'][0]}]}
            elif '/quota/' in route:
                barriers[params['binding_id'][0]].wait(timeout=5)
                data = {'binding_id': params['binding_id'][0],
                        'selected_pool': {'tier': 'enterprise_standard', 'remaining': 100},
                        'pools': [{'tier': 'enterprise_standard', 'remaining': 100},
                                  {'tier': 'enterprise_plus', 'remaining': 0}]}
            elif route == '/v1/models':
                data = {'data': [{'id': 'one'}, {'id': 'two'}]}
            elif route.endswith('/management.html'):
                data = '<html>synthetic</html>'
            else:
                data = {}
            return {'http': 200, 'data': data, 'headers': {}}
        app = Application(MagicMock())
        app.read = read
        value = app.snapshot(quotas=True)
        self.assertEqual({model['id'] for model in value['catalogs'][row['name']]}, {'one', 'two'})
        scope = value['binding_observations'][row['name']]
        self.assertEqual({key: (entry['models'][0]['id'], entry['quota']['binding_id']) for key, entry in scope.items()},
                         {'one': ('one', 'one'), 'two': ('two', 'two')})
        self.assertEqual(value['quotas'], {})
        self.assertFalse(any('child-3' in path for path in requested))

    def test_credential_backup_reads_files_concurrently_and_ignores_runtime_only_entries(self):
        barrier = threading.Barrier(2)
        app = Application(MagicMock())
        def read(path, **kwargs):
            name = parse_qs(urlsplit(path).query)['name'][0]
            self.assertIn(name, {'one.json', 'two.json'})
            barrier.wait(timeout=5)
            return {'data': name}
        app.read = read
        result = app.backup({'legacy': False, 'raw': 'synthetic config', 'credentials': [
            {'name': 'one.json'}, {'name': 'two.json'}, {'name': 'virtual', 'runtime_only': True}]})
        self.assertEqual(result['config'], 'synthetic config')
        self.assertEqual(result['credential_files'], {'one.json': 'one.json', 'two.json': 'two.json'})
        self.assertEqual(set(result['fingerprints']), {'one.json', 'two.json'})

    def test_route_and_backup_readers_start_before_preflight_finishes(self):
        adapter = Adapter({'kind': 'vps'}, lambda: None, None)
        routing = MagicMock()
        barrier = threading.Barrier(3)
        target = {'domains': ['test.example']}

        def preflight(release, *, on_identity, **kwargs):
            on_identity(target)
            barrier.wait(timeout=5)
            return target

        def read(value):
            barrier.wait(timeout=5)
            return value

        adapter.preflight = preflight
        adapter.backup = lambda before, **kwargs: read('backup')
        routing.snapshot.side_effect = read
        prepared = adapter.prepare(routing, {})
        self.assertEqual(prepared['routing'], target['domains'])
        self.assertEqual(prepared['backup'], 'backup')

    def test_cloud_preflight_reuses_binding_scan_only_for_same_recent_deployment(self):
        value = baseline('one')
        value.update(platform={'service': {'status': 'RUNNING'}, 'recent_deployments': [{'status': 'RUNNING'}]},
                     persistent_store_ready=True)
        value['runtime'].update(deployment_id='current', disk_page_sha256=value['page_sha256'])
        cloud = MagicMock()
        adapter = Adapter({'kind': 'zeabur', 'branch': 'dev'}, lambda: None, None, cloud)
        adapter.snapshot = MagicMock(return_value=value)
        previous = {'runtime': {'deployment_id': 'current'}, 'binding_checked_at': time.time()}
        with patch('scripts.ops.adapters.git', return_value=b'synthetic\trefs/heads/dev'):
            adapter.preflight({'commit': 'synthetic'}, reuse=previous)
            cloud.unique_binding.assert_not_called()
            value['runtime']['deployment_id'] = 'different'
            adapter.preflight({'commit': 'synthetic'}, reuse=previous)
        cloud.unique_binding.assert_called_once()

    def test_web_probe_cannot_pass_using_a_different_provider_or_only_http_200(self):
        for response in [{'status': 'error'}, {'status': 'ok', 'http_status': 200, 'provider': 'gemini-enterprise',
                                             'test_kind': 'inline_media', 'upstream_model': 'synthetic'}]:
            adapter = MagicMock()
            app = adapter.application.return_value.__enter__.return_value
            app.http.request.return_value = {'http': 200, 'data': response}
            job = {'provider': 'gemini-enterprise-web', 'probe': {'expected_upstream_model': 'synthetic'}}
            with patch('scripts.ops.testing.stream_call') as stream:
                result = model_call(adapter, job, {'model_key': 'synthetic'})
            self.assertFalse(result['success'])
            stream.assert_not_called()

    def test_remote_deploy_requires_live_guard_even_when_called_directly(self):
        adapter = Adapter({'kind': 'vps'}, MagicMock(side_effect=OpsError('no_lease')), MagicMock())
        with self.assertRaisesRegex(OpsError, 'no_lease'):
            adapter.remote('deploy')
        adapter.ssh.connect.assert_not_called()


class RemoteDockerTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.root = Path(self.temporary.name)
        self.directory = self.root / 'operation'
        self.directory.mkdir()
        self.compose = self.root / 'compose.yaml'
        self.compose.write_text('services: synthetic\n')
        (self.root / 'current.env').write_text('CPA_IMAGE=old-tag\nOTHER=preserved\n')
        (self.directory / 'current.env.before').write_text('CPA_IMAGE=old-tag\nOTHER=preserved\n')
        (self.directory / 'stage.json').write_text(json.dumps({'old_image': 'old', 'image_id': 'new'}))
        self.old = {'Id': 'old-container', 'Image': 'old', 'State': {'Running': True}}
        self.new = {'Id': 'new-container', 'Image': 'new', 'State': {'Running': True}}
        self.spec = {'project': 'cliproxyapi-vps', 'compose_sha256': remote_vps.sha(self.compose), 'protected_files': {}}

    def tearDown(self):
        self.temporary.cleanup()

    def test_stage_uses_a_local_tag_for_the_exact_running_image(self):
        operation = self.root / 'stage'
        operation.mkdir()
        package = operation / 'runtime.tar.gz'
        with tarfile.open(package, 'w:gz') as archive:
            for name in ('CLIProxyAPI', 'management.html', 'Dockerfile'):
                path = self.root / name
                path.write_bytes(name.encode())
                archive.add(path, arcname=name)
        old_image = 'sha256:' + '1' * 64
        old = {**self.old, 'Image': old_image}
        spec = {**self.spec, 'run_id': 'run-1234567890abcdef', 'expected_container': old['Id'], 'commit': 'a' * 40,
                'package_sha256': remote_vps.sha(package), 'binary_sha256': remote_vps.sha(self.root / 'CLIProxyAPI'),
                'page_sha256': remote_vps.sha(self.root / 'management.html')}
        images = {old_image: {'Id': old_image, 'RootFS': {'Layers': ['base-layer']}}}

        def docker(args, error='command_failed'):
            if args[:3] == ['docker', 'image', 'inspect']:
                return json.dumps([images[args[3]]])
            if args[:2] == ['docker', 'tag']:
                images[args[3]] = images[args[2]]
            elif args[:2] == ['docker', 'build']:
                base = args[args.index('--build-arg') + 1].removeprefix('BASE=')
                self.assertFalse(base.startswith('sha256:'))
                self.assertEqual(images[base]['Id'], old_image)
                images[args[args.index('-t') + 1]] = {'Id': 'staged-image',
                    'RootFS': {'Layers': ['base-layer', 'release-layer']},
                    'Config': {'Labels': {'org.opencontainers.image.revision': spec['commit']}}}
            return ''

        with patch.object(remote_vps, 'locate', return_value=(self.root, old, self.compose)), \
                patch.object(remote_vps, 'operation_dir', return_value=operation), patch.object(remote_vps, 'run', side_effect=docker):
            result = remote_vps.stage(spec)
        self.assertEqual(result['image_id'], 'staged-image')
        self.assertEqual(result['old_image'], old_image)

    def test_compose_mutation_targets_only_cli_and_preserves_environment(self):
        with patch.object(remote_vps, 'locate', side_effect=[(self.root, self.old, self.compose), (self.root, self.new, self.compose)]), \
                patch.object(remote_vps, 'operation_dir', return_value=self.directory), patch.object(remote_vps, 'run') as run:
            remote_vps.apply(self.spec)
        commands = [call.args[0] for call in run.call_args_list]
        self.assertEqual(len([cmd for cmd in commands if cmd[:2] == ['docker', 'compose']]), 1)
        mutation = next(cmd for cmd in commands if cmd[:2] == ['docker', 'compose'])
        self.assertEqual(mutation[-5:], ['up', '-d', '--no-deps', '--force-recreate', 'cli-proxy-api'])
        self.assertEqual((self.root / 'current.env').read_text(), 'CPA_IMAGE=new\nOTHER=preserved\n')

    def test_lost_deploy_ack_does_not_recreate_again(self):
        (self.root / 'current.env').write_text('CPA_IMAGE=new\nOTHER=preserved\n')
        with patch.object(remote_vps, 'locate', return_value=(self.root, self.new, self.compose)), \
                patch.object(remote_vps, 'operation_dir', return_value=self.directory), patch.object(remote_vps, 'run') as run:
            self.assertTrue(remote_vps.apply(self.spec)['already_applied'])
        self.assertFalse(any(call.args[0][:2] == ['docker', 'compose'] for call in run.call_args_list))

    def test_external_environment_or_compose_change_prevents_recreation(self):
        for file, value in [(self.root / 'current.env', 'CPA_IMAGE=old-tag\nOTHER=external\n'),
                            (self.compose, 'external-compose')]:
            file.write_text(value)
            with patch.object(remote_vps, 'locate', return_value=(self.root, self.old, self.compose)), \
                    patch.object(remote_vps, 'operation_dir', return_value=self.directory), patch.object(remote_vps, 'run') as run:
                with self.assertRaises(RuntimeError):
                    remote_vps.apply(self.spec)
                run.assert_not_called()

    def test_rollback_preserves_new_account_data(self):
        account = self.root / 'new-token.json'
        account.write_text('synthetic-new-token')
        (self.root / 'current.env').write_text('CPA_IMAGE=new\nOTHER=preserved\n')
        with patch.object(remote_vps, 'locate', side_effect=[(self.root, self.new, self.compose), (self.root, self.old, self.compose)]), \
                patch.object(remote_vps, 'operation_dir', return_value=self.directory), patch.object(remote_vps, 'run'):
            remote_vps.apply(self.spec, rollback=True)
        self.assertEqual(account.read_text(), 'synthetic-new-token')
        self.assertEqual((self.root / 'current.env').read_text(), 'CPA_IMAGE=old\nOTHER=preserved\n')


if __name__ == '__main__':
    unittest.main()

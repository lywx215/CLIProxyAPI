import ast
import copy
from datetime import datetime, timezone
from pathlib import Path
import unittest

from scripts.ops import catalog
from scripts.ops.cli import no_inline_secrets
from scripts.ops.core import OpsError, credential_settings, deployment_identity, failure_category, validate_selection
from scripts.ops.tests.fakes import Driver, baseline
from scripts.ops.zeabur import Zeabur


NOW = datetime(2026, 11, 9, 7, tzinfo=timezone.utc)


def binding_observation(data, binding, models, remaining):
    scope = data.setdefault('binding_observations', {}).setdefault('synthetic-one', {})
    scope[binding['binding_id']] = {'models': models, 'quota': {
        **{key: binding[key] for key in ('binding_id', 'project_id', 'location')},
        'reset_at': '2026-11-09T08:00:00Z',
        'selected_pool': {'status': 'ok' if remaining is not None else 'unavailable', 'tier': 'enterprise_plus',
                          'remaining': remaining, 'limit': 10000}}}


class PolicyTests(unittest.TestCase):
    def test_deployment_identity_ignores_free_space_but_tracks_program_and_page(self):
        before = baseline('one')
        after = copy.deepcopy(before)
        before['runtime']['disk_free_bytes'] = 1000
        after['runtime']['disk_free_bytes'] = 900
        self.assertEqual(deployment_identity(before), deployment_identity(after))
        after['runtime']['binary_sha256'] = 'different-binary'
        self.assertNotEqual(deployment_identity(before), deployment_identity(after))
        after['runtime']['binary_sha256'] = before['runtime']['binary_sha256']
        after['page_sha256'] = 'different-page'
        self.assertNotEqual(deployment_identity(before), deployment_identity(after))

    def test_retired_hosts_rejected_even_with_new_alias(self):
        for address in ['152.53.208.245', '159.195.16.180']:
            inventory = Driver(('renamed',)).inventory
            inventory['targets']['renamed']['host'] = address
            with self.assertRaisesRegex(OpsError, 'retired_target'):
                validate_selection(inventory, ['renamed'])

    def test_unknown_target_and_empty_selection_are_rejected(self):
        inventory = Driver().inventory
        for selection in [[], ['other'], ['one', 'one']]:
            with self.assertRaises(OpsError):
                validate_selection(inventory, selection)

    def test_inline_password_is_rejected(self):
        with self.assertRaises(OpsError):
            no_inline_secrets({'connections': {'one': {'password': 'fake'}}})

    def test_shared_deployment_branch_is_rejected(self):
        z = object.__new__(Zeabur)
        z.profile = lambda target: {'service_id': 'one', 'environment_id': 'prod'}
        z.bindings = lambda repository: [{'service_id': 'one', 'environment_id': 'prod', 'branch': 'dev'},
                                          {'service_id': 'two', 'environment_id': 'prod', 'branch': 'dev'}]
        with self.assertRaisesRegex(OpsError, 'shared_or_changed_deployment_branch'):
            z.unique_binding({'branch': 'dev'}, 'https://example.test/repository')

    def test_aliased_inventory_never_treats_a_missing_environment_as_unbound(self):
        z = object.__new__(Zeabur)
        z.graphql = lambda query, variables: {'r0': {'_id': 'same-service', 'gitTrigger': None}}
        with self.assertRaisesRegex(OpsError, 'zeabur_binding_inventory_incomplete'):
            z.batch_read([('same-service', 'prod'), ('same-service', 'other')], 'service(_id:$id{i})',
                         '{{_id gitTrigger(environmentID:$env{i}){{repoURL branchName}}}}', True)

    def test_peer_deployment_identity_uses_history_without_cluster_status(self):
        z = object.__new__(Zeabur)
        z.profile = lambda target: {'service_id': target['label'], 'environment_id': 'prod'}

        def history(query, variables):
            self.assertNotIn('status', query)
            self.assertEqual(variables['env'], 'prod')
            rows = [{'node': {'_id': variables['id'] + '-current'}}] if variables['id'] != 'missing' else []
            return {'deployments': {'edges': rows}}

        z.graphql = history
        self.assertEqual(z.deployment_ids([{'label': 'p1'}, {'label': 'p3'}]),
                         {'p1': 'p1-current', 'p3': 'p3-current'})
        with self.assertRaisesRegex(OpsError, 'zeabur_deployment_identity_unavailable'):
            z.deployment_ids([{'label': 'missing'}])

    def test_disabled_collection_settings_are_preserved(self):
        before = baseline('one')
        after = copy.deepcopy(before)
        after['credentials'][0]['gemini_enterprise']['collections'][0]['disabled'] = True
        with self.assertRaisesRegex(OpsError, 'credential_settings_changed'):
            catalog.preserve(before, after)

    def test_token_refresh_does_not_count_as_configuration_change(self):
        first = baseline('one')['credentials']
        second = copy.deepcopy(first)
        second[0].update(last_refresh='new', success=100, failed=2, access_token='synthetic-token')
        self.assertEqual(credential_settings(first), credential_settings(second))

    def test_missing_required_text_model_is_not_silently_excluded(self):
        first = baseline('one')
        second = copy.deepcopy(first)
        second['models'] = []
        with self.assertRaisesRegex(OpsError, 'required_text_model_disappeared'):
            catalog.preserve(first, second)

    def test_agents_are_not_misclassified_as_enterprise_web(self):
        data = baseline('one')
        data['models'].append({'id': 'antigravity-preview-latest'})
        data['credentials'].append({'name': 'agent', 'provider': 'interactions'})
        data['catalogs']['agent'] = [{'id': 'antigravity-preview-latest'}]
        jobs, excluded = catalog.jobs(data)
        self.assertEqual([job['provider'] for job in jobs], ['gemini-enterprise'])
        self.assertIn({'model': 'antigravity-preview-latest', 'reason': 'outside_enterprise_provider_scope'}, excluded)

    def test_two_user_exclusions_apply_even_when_advertised(self):
        data = baseline('one')
        for name in ['gemini-2.5-flash', 'gemini-2.5-flash-lite']:
            data['models'].append({'id': name})
            data['catalogs']['synthetic-one'].append({'id': name})
        jobs, excluded = catalog.jobs(data)
        self.assertEqual(len(jobs), 1)
        self.assertEqual(len([row for row in excluded if row['reason'] == 'user_excluded_404_model']), 2)

    def test_claude_family_excluded_by_public_name_and_catalog_upstream_model(self):
        data = baseline('one')
        claude_models = [{'id': 'anthropic/Claude-Opus-4-6'},
                         {'id': 'claude-sonnet-5-5'},
                         {'id': 'premium-alias', 'upstream_model': 'us.anthropic.claude-haiku-4-5-v1:0'}]
        data['models'].extend(claude_models + [{'id': 'claude-3-haiku'}])
        data['catalogs']['synthetic-one'].extend(claude_models)
        data['catalogs']['synthetic-one'].append({'id': 'premium-alias', 'upstream_model': 'gemini-4-carbon'})
        jobs, excluded = catalog.jobs(data)
        self.assertEqual([job['model'] for job in jobs], ['gemini-3.1-pro-preview'])
        self.assertEqual({row['model'] for row in excluded if row['reason'] == 'user_excluded_claude_model'},
                         {row['id'] for row in claude_models} | {'claude-3-haiku'})

    def test_enabled_web_requires_real_media_jobs(self):
        data = baseline('one')
        data['web'] = {'enabled': True}
        data['credentials'].append({'name': 'web', 'type': 'gemini-enterprise-web', 'auth_index': 'w',
                                    'gemini_enterprise_web': {'revision': 2, 'cookie_present': True}})
        data['catalogs']['web'] = [{'id': 'gemini-4-carbon', 'upstream_model': 'gemini-4-carbon'}]
        jobs, _ = catalog.jobs(data)
        web = next(job for job in jobs if job['provider'] == 'gemini-enterprise-web')
        self.assertEqual(web['probe']['expected_revision'], 2)
        self.assertEqual(web['resources'], ['*'])

    def test_zero_known_quota_blocks_but_unknown_does_not_become_zero(self):
        data = baseline('one')
        binding = catalog.bindings(data['credentials'][0])[0]
        binding_observation(data, binding, data['models'], 0)
        jobs, _ = catalog.jobs(data)
        with self.assertRaisesRegex(OpsError, 'gemini-3.1-pro-preview') as raised:
            catalog.quota_check(data, jobs, now=NOW)
        self.assertEqual(raised.exception.code, 'required_model_quota_exhausted')
        binding_observation(data, binding, data['models'], None)
        self.assertEqual(catalog.quota_check(data, jobs, now=NOW)[0]['code'], 'quota_unknown_not_zero')

    def test_failure_classification_does_not_confuse_quota_and_regression(self):
        self.assertEqual(failure_category(429), 'quota')
        self.assertEqual(failure_category(403), 'authorization')
        self.assertEqual(failure_category(500), 'upstream')
        self.assertEqual(failure_category(200, {'error': 'RESOURCE_EXHAUSTED'}), 'quota')

    def test_same_tier_free_collection_cannot_mask_argon_pool_exhaustion(self):
        data = baseline('one')
        argon = {'id': 'alias-argon', 'upstream_model': 'gemini-4-argon'}
        carbon = {'id': 'gemini-4-carbon'}
        data['models'] = [argon, carbon]
        data['catalogs']['synthetic-one'] = data['models']
        bindings = catalog.bindings(data['credentials'][0])
        bindings[0]['auth_index'] = 'paid'
        bindings.append({'binding_id': 'trial', 'auth_index': 'free', 'project_id': 'different', 'location': 'global'})
        binding_observation(data, bindings[0], [argon], 0)
        binding_observation(data, bindings[1], [carbon], 9000)
        jobs, _ = catalog.jobs(data)
        self.assertEqual(jobs[0]['auth_indexes'], ['paid'])
        self.assertNotEqual(jobs[0]['resources'], jobs[1]['resources'])
        with self.assertRaises(OpsError) as raised:
            catalog.quota_check(data, jobs, now=NOW)
        self.assertEqual((raised.exception.code, str(raised.exception)), ('required_model_quota_exhausted', 'alias-argon'))
        self.assertEqual(catalog.quota_check(data, jobs[1:], now=NOW), [])

    def test_active_model_cooldown_blocks_even_when_monitoring_shows_remaining(self):
        data = baseline('one')
        binding = catalog.bindings(data['credentials'][0])[0]
        model = data['models'][0]['id']
        binding_observation(data, binding, data['models'], 38)
        data['credential_observed_at'] = '2026-11-09T06:58:00Z'
        binding['cooldowns'] = [{'scope': 'model', 'model_key': model, 'http_status': 429, 'reason': 'quota',
                                 'retry_at': '2026-11-09T06:59:00Z'}]
        jobs, _ = catalog.jobs(data)
        with self.assertRaises(OpsError) as raised:
            catalog.quota_check(data, jobs, now=NOW)
        self.assertEqual(raised.exception.code, 'required_model_quota_cooldown')
        binding['cooldowns'][0]['model_key'] = 'unrelated-model'
        self.assertEqual(catalog.quota_check(data, jobs, now=NOW)[0]['code'], 'quota_below_10_percent')

    def test_wrong_binding_or_previous_quota_period_is_unknown(self):
        data = baseline('one')
        binding = catalog.bindings(data['credentials'][0])[0]
        binding_observation(data, binding, data['models'], 0)
        value = data['binding_observations']['synthetic-one']['b']['quota']
        jobs, _ = catalog.jobs(data)
        value['project_id'] = 'unrelated'
        self.assertEqual(catalog.quota_check(data, jobs, now=NOW)[0]['code'], 'quota_unknown_not_zero')
        value['project_id'] = binding['project_id']
        value['reset_at'] = NOW.isoformat()
        self.assertEqual(catalog.quota_check(data, jobs, now=NOW)[0]['code'], 'quota_unknown_not_zero')

    def test_same_filename_with_different_account_invalidates_preservation(self):
        first = baseline('one')
        first['credentials'][0]['email'] = 'one@example.test'
        second = copy.deepcopy(first)
        second['credentials'][0]['email'] = 'two@example.test'
        with self.assertRaisesRegex(OpsError, 'credential_settings_changed'):
            catalog.preserve(first, second)

    def test_explicit_auth_failure_blocks_preflight_before_routing_changes(self):
        data = baseline('one')
        data['credentials'][0]['status_message'] = 'invalid_grant'
        with self.assertRaises(OpsError) as raised:
            catalog.quota_check(data, catalog.jobs(data)[0])
        self.assertEqual(raised.exception.code, 'required_model_credentials_unauthorized')

    def test_production_guards_do_not_use_assert(self):
        directory = Path(__file__).resolve().parents[1]
        for path in directory.glob('*.py'):
            tree = ast.parse(path.read_text(encoding='utf-8'))
            self.assertFalse(any(isinstance(node, ast.Assert) for node in ast.walk(tree)), str(path))

    def test_no_legacy_task_code_or_fixed_release_import(self):
        directory = Path(__file__).resolve().parents[1]
        for path in directory.glob('*.py'):
            source = path.read_text(encoding='utf-8')
            self.assertNotIn('fleet-release-ed55', source)
            self.assertNotIn('ast.get_source_segment', source)
            self.assertNotIn('ed55f508fabf98011c74979b77e45c2d49da1ff5', source)


if __name__ == '__main__':
    unittest.main()

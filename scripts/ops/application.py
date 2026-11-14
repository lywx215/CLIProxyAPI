"""Management reads, encrypted backups, and an explicitly scoped panel pin."""
from __future__ import annotations

import copy
import time
from .concurrency import TaskExecutor as ThreadPoolExecutor
from datetime import datetime, timezone
from urllib.parse import urlencode
import yaml

from . import catalog
from .core import MAX_READ_WORKERS, PROVIDERS, READ_SLOTS, credential_settings, digest, require
from .telemetry import note


def catalog_identity(config, files, models):
    def stable(value):
        if isinstance(value, dict):
            return {key: stable(child) for key, child in value.items()
                    if key not in {'checked_at', 'last_success_at', 'observed_at', 'expiry', 'updated_at'}}
        if isinstance(value, list):
            return [stable(child) for child in value]
        return value
    identities = []
    for row in files:
        enterprise = row.get('gemini_enterprise') or {}
        web = row.get('gemini_enterprise_web') or {}
        identities.append([row['name'], row.get('auth_index'), row.get('last_refresh'),
                           row.get('modtime') if row.get('size') else None, enterprise.get('revision'),
                           [[binding.get('binding_id'), binding.get('revision'), binding.get('auth_index'),
                             stable(binding.get('model_policy_state'))] for binding in catalog.bindings(row)],
                           web.get('revision'), web.get('cookie_check_status'), web.get('invalid')])
    return digest([config, credential_settings(files), sorted(models, key=digest), sorted(identities, key=lambda row: row[0])])


def backup_identity(row):
    if type(row.get('size')) is not int or row['size'] <= 0 or not catalog.parse_timestamp(row.get('modtime')):
        return None
    return digest([credential_settings([row]), {key: row.get(key) for key in ('modtime', 'size', 'updated_at', 'auth_index')}])


class Application:
    def __init__(self, client, guard=lambda: None):
        self.http, self.guard = client, guard

    def read(self, path, *, raw=False, optional=False, **kwargs):
        with READ_SLOTS:
            response = self.http.request(path, raw=raw, read_only=True, **kwargs)
        if not optional:
            require(response['http'] == 200 and response['data'] is not None, 'application_read_failed')
        return response

    def snapshot(self, *, quotas=False, quota_models=None, legacy_raw=False, reuse=None, on_identity=None, refresh_catalog=False):
        config = self.read('/v8/management/config', optional=True)
        legacy = config['http'] == 404
        prefix = '/v0/management' if legacy else '/v8/management'
        if legacy:
            config = self.read(prefix + '/config')
        require(config['http'] == 200, 'management_unavailable')
        cfg = config['data']
        keys = (cfg.get('access') or {}).get('api-keys') or cfg.get('api-keys') or []
        model_key = next((value for value in keys if isinstance(value, str) and value), None)
        require(model_key, 'model_key_missing')
        with ThreadPoolExecutor(max_workers=MAX_READ_WORKERS) as executor:
            raw_future = executor.submit(self.read, ('/v0/management' if legacy_raw else prefix) + '/config.yaml', raw=True)
            files_future = executor.submit(self.read, prefix + ('/auth-files' if legacy else '/credentials'))
            models_future = executor.submit(self.read, '/v1/models', key=model_key)
            health_future = executor.submit(self.read, '/healthz', optional=True)
            page_future = executor.submit(self.read, '/management.html', raw=True)
            web_future = executor.submit(self.read, '/v8/management/credentials/gemini-enterprise-web/status', optional=True)
            raw = raw_future.result()
            files = files_future.result()['data'].get('files')
            credential_observed_at = datetime.now(timezone.utc).isoformat()
            models = models_future.result()['data'].get('data')
            health = health_future.result()
            if health['http'] == 404:
                health = executor.submit(self.read, '/').result()
            page, web = page_future.result(), web_future.result()
        require(isinstance(files, list), 'credentials_unavailable')
        require(isinstance(models, list), 'model_list_invalid')
        require(health['http'] == 200, 'application_unhealthy', category='regression')
        semantics = yaml.safe_load(raw['data'])
        identity = catalog_identity(semantics, files, models)
        if on_identity is not None:
            on_identity({'legacy': legacy, 'raw': raw['data'], 'credentials': files})
        reuse = reuse or {}
        catalog_observed_at = time.time()
        reusable = (reuse.get('catalog_identity') == identity
                    and 0 <= catalog_observed_at - reuse.get('catalog_observed_at', 0) <= 600)
        catalogs, observations, binding_observations = {}, {}, {}
        reads = []
        def read_catalog(row, binding, path):
            prior = (reuse.get('binding_observations', {}).get(row['name'], {}).get(binding.get('binding_id'), {})
                     if binding else {'models': reuse.get('catalogs', {}).get(row['name'])})
            if reusable and not refresh_catalog and isinstance(prior.get('models'), list):
                note('cache_hit', scope='model_catalog')
                return {'data': {'models': prior['models']}}
            return self.read(path)
        def read_quota(row, binding, path):
            prior = reuse.get('binding_observations', {}).get(row['name'], {}).get(binding.get('binding_id'), {}).get('quota') or {}
            observed, reset = catalog.parse_timestamp(prior.get('observed_at')), catalog.parse_timestamp(prior.get('reset_at'))
            now = datetime.now(timezone.utc)
            same_binding = all(prior.get(key) == binding.get(key) for key in ('binding_id', 'project_id', 'location'))
            pool = catalog.selected_pool(binding, prior, now)
            if reusable and observed and reset and same_binding and catalog.known_pool(pool) and (
                    pool['remaining'] > 0 and 0 <= (now - observed).total_seconds() <= 120 and reset > now):
                note('cache_hit', scope='quota')
                return {'http': 200, 'data': prior}
            return self.read(path, optional=True)
        with ThreadPoolExecutor(max_workers=MAX_READ_WORKERS) as executor:
            for row in files:
                if row.get('disabled') or catalog.provider(row) not in PROVIDERS:
                    continue
                endpoint = prefix + ('/auth-files/models' if legacy else '/credentials/models')
                collections = catalog.enabled_bindings(row)
                choices = collections if catalog.bindings(row) and not legacy else [{}]
                catalogs[row['name']] = {}
                if catalog.bindings(row) and not legacy:
                    binding_observations[row['name']] = {}
                for binding in choices:
                    params = {'name': row['name'], 'auth_index': binding.get('auth_index') or row.get('auth_index', '')}
                    if binding:
                        require(binding.get('binding_id') and binding.get('auth_index'), 'collection_identity_unavailable')
                        params['binding_id'] = binding['binding_id']
                    quota_path = None
                    if quotas and catalog.provider(row) == 'gemini-enterprise' and not legacy:
                        query = {key: value for key, value in params.items() if key != 'name'}
                        quota_path = '/v8/management/quota/gemini-enterprise?' + urlencode(query)
                    models_future = executor.submit(read_catalog, row, binding, endpoint + '?' + urlencode(params))
                    quota_future = executor.submit(read_quota, row, binding, quota_path) if quota_path and quota_models is None else None
                    reads.append((row, binding, models_future, quota_future, quota_path))
            for row, binding, models_future, quota_future, quota_path in reads:
                models_for_binding = models_future.result()['data'].get('models')
                require(isinstance(models_for_binding, list), 'credential_model_catalog_invalid')
                entries = catalogs.setdefault(row['name'], {})
                for model in models_for_binding:
                    entries[digest(model)] = model
                quota = None
                if quota_future is None and quota_path and quota_models is not None and any(
                        model['id'] in quota_models for model in models_for_binding):
                    quota_future = executor.submit(read_quota, row, binding, quota_path)
                if quota_future is not None:
                    result = quota_future.result()
                    if result['http'] == 200 and isinstance(result['data'], dict):
                        quota = result['data']
                if binding:
                    details = binding_observations.setdefault(row['name'], {})
                    details[binding['binding_id']] = {'models': models_for_binding, 'quota': quota}
                elif quota:
                    observations.setdefault(row['name'], []).append(quota.get('selected_pool') or {'status': 'unknown'})
        catalogs = {name: list(entries.values()) for name, entries in catalogs.items()}
        return {'config': cfg, 'config_semantics': semantics, 'raw': raw['data'],
                'catalog_identity': identity,
                'catalog_observed_at': reuse['catalog_observed_at'] if reusable and not refresh_catalog else catalog_observed_at,
                'etag': raw['headers'].get('etag'), 'legacy': legacy, 'credentials': files,
                'models': models, 'model_key': model_key, 'catalogs': catalogs, 'quotas': observations,
                'binding_observations': binding_observations, 'credential_observed_at': credential_observed_at,
                'health_http': health['http'], 'management_http': config['http'], 'page_sha256': digest(page['data'].encode()),
                'version_header': config['headers'].get('x-cpa-version'),
                'commit_header': config['headers'].get('x-cpa-commit'),
                'web': web['data'] if web['http'] == 200 else {'enabled': False, 'unavailable': True}}

    def backup(self, snapshot, *, reuse=None):
        prefix = '/v0/management/auth-files' if snapshot['legacy'] else '/v8/management/credentials'
        reuse = reuse or {}
        fingerprints = {row['name']: backup_identity(row) for row in snapshot['credentials'] if not row.get('runtime_only')}
        def read_file(row):
            name = row['name']
            if fingerprints[name] and reuse.get('fingerprints', {}).get(name) == fingerprints[name] and name in reuse.get('credential_files', {}):
                note('cache_hit', scope='backup')
                return {'data': reuse['credential_files'][name]}
            return self.read(prefix + '/download?' + urlencode({'name': name}), raw=True)
        with ThreadPoolExecutor(max_workers=MAX_READ_WORKERS) as executor:
            reads = {row['name']: executor.submit(read_file, row)
                     for row in snapshot['credentials'] if not row.get('runtime_only')}
            values = {name: future.result()['data'] for name, future in reads.items()}
        return {'config': snapshot['raw'], 'credential_files': values, 'fingerprints': fingerprints}

    def panel_pin(self, before):
        self.guard()
        original = before['config_semantics']
        section = 'remote-management' if before['legacy'] else 'management'
        if (original.get(section) or {}).get('disable-auto-update-panel') is True:
            return original
        # Do not write a legacy full-file config without transactional v8 support.
        require(not before['legacy'], 'legacy_panel_pin_requires_separate_preparation')
        fresh = self.read('/v8/management/config.yaml', raw=True)
        expected = copy.deepcopy(original)
        expected.setdefault(section, {})['disable-auto-update-panel'] = True
        if yaml.safe_load(fresh['data']) == expected:
            return expected
        require(yaml.safe_load(fresh['data']) == original and fresh['headers'].get('etag'), 'config_changed_before_panel_pin')
        response = self.http.request('/v8/management/config/management/disable-auto-update-panel', 'PATCH', True,
                                     headers={'If-Match': fresh['headers']['etag']})
        require(response['http'] == 200, 'panel_pin_failed')
        after = self.read('/v8/management/config.yaml', raw=True)
        require(yaml.safe_load(after['data']) == expected, 'unexpected_panel_config_change')
        return expected

    def log_review(self):
        import re
        result = self.read('/v8/management/observability/logs?limit=300', optional=True)
        lines = (result.get('data') or {}).get('lines') or []
        text = [line if isinstance(line, str) else __import__('json').dumps(line) for line in lines]
        patterns = {'missing_oauth_secret': r'configured client secret environment variable is empty',
                    'invalid_oauth': r'\b(invalid_client|invalid_grant)\b',
                    'panic_or_fatal': r'\b(panic|fatal)\b', 'http_5xx': r'\|\s*5[0-9]{2}\s*\|'}
        return {'http': result['http'], 'bounded_sample_lines': len(lines),
                'counts': {key: sum(bool(re.search(pattern, line, re.I)) for line in text)
                           for key, pattern in patterns.items()}}

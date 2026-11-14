"""Provider-derived test inventory and preservation checks."""
from __future__ import annotations

from datetime import datetime, timezone

from .core import EXCLUDED_MODELS, PROVIDERS, credential_settings, digest, failure_category, is_claude_model, require, text_capability


def provider(row):
    return row.get('provider') or row.get('type')


def bindings(row):
    return (row.get('gemini_enterprise') or {}).get('collections') or []


def enabled_bindings(row):
    return [binding for binding in bindings(row) if not (binding.get('disabled') or binding.get('effective_disabled')
            or binding.get('missing'))]


def matching_bindings(snapshot, credential, model):
    scope = snapshot.get('binding_observations', {}).get(credential['name'])
    if scope is None:
        return None
    return [(binding, scope[binding['binding_id']]) for binding in enabled_bindings(credential)
            if binding.get('binding_id') in scope
            and any(entry['id'] == model for entry in scope[binding['binding_id']]['models'])]


def jobs(snapshot):
    public = {row['id'] for row in snapshot['models']}
    claude_models = {model['id'] for entries in [snapshot['models'], *snapshot['catalogs'].values()]
                     for model in entries
                     if is_claude_model(model['id']) or is_claude_model(model.get('upstream_model'))}
    by_key = {}
    excluded = []
    enabled_web = (snapshot.get('web') or {}).get('enabled') is True
    for credential in snapshot['credentials']:
        kind = provider(credential)
        if credential.get('disabled') or kind not in PROVIDERS:
            continue
        if kind == 'gemini-enterprise-web' and not enabled_web:
            continue
        entries = snapshot['catalogs'].get(credential['name'], [])
        for model in entries:
            name = model['id']
            if name in EXCLUDED_MODELS:
                excluded.append({'model': name, 'reason': 'user_excluded_404_model'})
                continue
            if name in claude_models:
                excluded.append({'model': name, 'provider': kind, 'reason': 'user_excluded_claude_model'})
                continue
            if not text_capability(model):
                excluded.append({'model': name, 'reason': 'nontext_output_capability'})
                continue
            # Web catalogs may intentionally be separate from the OpenAI model listing.
            if kind == 'gemini-enterprise' and name not in public:
                continue
            scoped = matching_bindings(snapshot, credential, name) if kind == 'gemini-enterprise' else None
            if scoped == []:
                continue
            eligible = enabled_bindings(credential) if scoped is None else [binding for binding, _ in scoped]
            key = kind + ':' + name
            if key not in by_key:
                by_key[key] = {'id': key, 'model': name, 'provider': kind, 'resources': [], 'credentials': [],
                               'upstream_models': [], 'auth_indexes': []}
            job = by_key[key]
            job['credentials'].append(credential['name'])
            job['upstream_models'].append(model.get('upstream_model') or name)
            if scoped is None and credential.get('auth_index'):
                job['auth_indexes'].append(credential['auth_index'])
            job['auth_indexes'].extend(row['auth_index'] for row in eligible if row.get('auth_index'))
            if kind == 'gemini-enterprise-web':
                view = credential.get('gemini_enterprise_web') or {}
                require(view.get('cookie_present') is True and not view.get('invalid'), 'web_credential_unusable', category='authorization')
                job.setdefault('probe', {'name': credential['name'], 'auth_index': credential['auth_index'],
                                         'model': name, 'expected_upstream_model': model.get('upstream_model') or name,
                                         'expected_revision': view.get('revision')})
                # Cookies can be duplicated between files; unknown sharing always serializes.
                job['resources'].append('*')
            else:
                if not eligible:
                    job['resources'].append('*')
                for row in eligible:
                    if row.get('project_id') and row.get('location'):
                        job['resources'].append(digest([row['project_id'], row['location']]))
                    else:
                        job['resources'].append('*')
    for job in by_key.values():
        job['resources'] = sorted(set(job['resources']))
        job['credentials'] = sorted(set(job['credentials']))
        job['upstream_models'] = sorted(set(job['upstream_models']))
        job['auth_indexes'] = sorted(set(job['auth_indexes']))
        if job['provider'] == 'gemini-enterprise-web':
            require(job['probe']['expected_revision'], 'web_revision_missing')
    covered = {job['model'] for job in by_key.values()} | {entry['model'] for entry in excluded if entry['model'] in claude_models}
    excluded.extend({'model': name, 'reason': ('user_excluded_claude_model' if name in claude_models
                                              else 'outside_enterprise_provider_scope')}
                    for name in sorted(public - covered - EXCLUDED_MODELS))
    return sorted(by_key.values(), key=lambda job: job['id']), excluded


def fingerprint(snapshot):
    versions = []
    for row in snapshot['credentials']:
        if row.get('disabled') or provider(row) not in PROVIDERS:
            continue
        enterprise, web = row.get('gemini_enterprise') or {}, row.get('gemini_enterprise_web') or {}
        versions.append([row['name'], row.get('auth_index'), row.get('last_refresh'),
                         row.get('modtime') if row.get('size') else None,
                         enterprise.get('revision'), web.get('revision'),
                         sorted((item['binding_id'], item.get('auth_index'), item.get('revision'))
                                for item in enabled_bindings(row) if item.get('binding_id'))])
    return digest({'configuration': snapshot['config_semantics'],
                   'credentials': credential_settings(snapshot['credentials']),
                   'versions': sorted(versions, key=lambda row: row[0])})


def preserve(before, after, *, expected_config=None):
    require(after['config_semantics'] == (expected_config if expected_config is not None else before['config_semantics']),
            'configuration_changed_outside_update')
    require(credential_settings(before['credentials']) == credential_settings(after['credentials']),
            'credential_settings_changed_outside_update')
    old_jobs, _ = jobs(before)
    new_jobs, _ = jobs(after)
    missing = {job['id'] for job in old_jobs} - {job['id'] for job in new_jobs}
    require(not missing, 'required_text_model_disappeared', category='functional')
    return new_jobs


def parse_timestamp(value):
    try:
        parsed = datetime.fromisoformat(value)
    except (TypeError, ValueError):
        return None
    return parsed if parsed.tzinfo is not None else None


def active_quota_cooldown(row, models, now):
    times = [parse_timestamp(item.get('retry_at')) for item in row.get('cooldowns') or []
             if item.get('reason') in {'quota', 'credential_quota'} or item.get('http_status') == 429
             if item.get('scope') == 'credential' or (item.get('scope') == 'model' and item.get('model_key') in models)]
    return max((when for when in times if when and when > now), default=None)


def selected_pool(binding, quota, now):
    if not quota or quota.get('binding_id') != binding.get('binding_id') \
            or quota.get('project_id') != binding.get('project_id') or quota.get('location') != binding.get('location'):
        return {}
    pool = quota.get('selected_pool') or {}
    reset_at = parse_timestamp(quota.get('reset_at'))
    if reset_at is None or reset_at <= now:
        return {}
    return pool


def known_pool(pool):
    return pool.get('status') == 'ok' and type(pool.get('remaining')) in {int, float} and pool['remaining'] >= 0


def quota_check(snapshot, test_jobs, *, require_positive=False, now=None):
    """Only the selected pool and cooldown of a route that can serve the model count."""
    now = now or datetime.now(timezone.utc)
    # The quota reads can outlast short cooldowns. A timer active when the
    # credentials were read is not evidence of recovery; require a new snapshot.
    cooldown_now = parse_timestamp(snapshot.get('credential_observed_at')) or now
    warnings = []
    for job in test_jobs:
        if job['provider'] != 'gemini-enterprise':
            continue
        evidence, unauthorized = [], 0
        for credential in snapshot['credentials']:
            if credential['name'] not in job['credentials']:
                continue
            scoped = matching_bindings(snapshot, credential, job['model'])
            if scoped is None:
                if failure_category(payload=credential.get('status_message')) == 'authorization':
                    unauthorized += 1
                    continue
                # Older snapshots do not link collection pools to their model catalogs.
                # Their observations are usable only for a single credential without collections.
                pools = [] if bindings(credential) else snapshot.get('quotas', {}).get(credential['name'], [])
                pools = pools if len(pools) == 1 else [{}]
                evidence.extend((pool, active_quota_cooldown(credential, {job['model'], *job['upstream_models']}, cooldown_now))
                                for pool in pools)
                continue
            for binding, observation in scoped:
                if failure_category(payload=binding.get('status_message')) == 'authorization':
                    unauthorized += 1
                    continue
                models = {job['model'], *(model.get('upstream_model') or model['id'] for model in observation['models']
                                          if model['id'] == job['model'])}
                cooldown = active_quota_cooldown(binding, models, cooldown_now)
                evidence.append((selected_pool(binding, observation.get('quota'), now), cooldown))
        require(evidence or not unauthorized, 'required_model_credentials_unauthorized', job['model'], category='authorization')
        known = [pool for pool, _ in evidence if known_pool(pool)]
        if any(cooldown for _, cooldown in evidence):
            require(not all(cooldown or (known_pool(pool) and pool['remaining'] == 0)
                            for pool, cooldown in evidence), 'required_model_quota_cooldown', job['model'], category='quota')
            warnings.append({'model': job['model'], 'code': 'quota_cooldown_on_some_routes'})
        require(not (evidence and len(known) == len(evidence) and all(pool['remaining'] == 0 for pool in known)),
                'required_model_quota_exhausted', job['model'], category='quota')
        if require_positive:
            require(any(not cooldown and known_pool(pool) and pool['remaining'] > 0 for pool, cooldown in evidence),
                    'quota_recovery_not_verified', job['model'], category='quota')
        if any(type(pool.get('limit')) in {int, float} and pool['limit'] > 0 and pool['remaining'] / pool['limit'] < .1
               for pool in known):
            warnings.append({'model': job['model'], 'code': 'quota_below_10_percent'})
        if not known:
            warnings.append({'model': job['model'], 'code': 'quota_unknown_not_zero'})
    return warnings

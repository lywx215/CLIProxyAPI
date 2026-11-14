"""Per-target exceptions and backwards-compatible execution plan normalization."""
import json
import re

from . import catalog
from .core import channel_fingerprint, digest, require


def load_target_options(path, labels):
    def unique(pairs):
        result = dict(pairs)
        require(len(result) == len(pairs), 'duplicate_target_option_key')
        return result
    value = json.loads(path.read_text(encoding='utf-8-sig'), object_pairs_hook=unique) if path else {}
    return target_options(value, labels, external=True)


def target_options(value, labels, *, external=False):
    require(isinstance(value, dict) and set(value) <= set(labels), 'target_options_outside_selection')
    result = {}
    allowed = {'web_model_tests', 'hold_channels', 'fast_model_tests'}
    if not external:
        allowed.add('routing_fingerprint')
    for label, options in value.items():
        require(isinstance(options, dict) and set(options) <= allowed, 'invalid_target_options')
        normalized = {}
        for key in ('web_model_tests', 'hold_channels', 'fast_model_tests'):
            items = options.get(key, [])
            require(isinstance(items, list) and all(isinstance(item, str) for item in items)
                    and len(items) == len(set(items)), 'invalid_target_option_values')
            normalized[key] = sorted(items)
        require(all(item.startswith('gemini-enterprise-web:') and item.split(':', 1)[1]
                    for item in normalized['web_model_tests']), 'invalid_web_deferral')
        require(all(re.fullmatch(r'(main|vip):[1-9][0-9]*', item) for item in normalized['hold_channels']),
                'invalid_held_channel')
        require(all(item.startswith(('gemini-enterprise:', 'gemini-enterprise-web:')) and item.split(':', 1)[1]
                    for item in normalized['fast_model_tests']), 'invalid_fast_model_tests')
        require(not set(normalized['web_model_tests']) & set(normalized['fast_model_tests']), 'invalid_fast_model_tests')
        normalized['routing_fingerprint'] = options.get('routing_fingerprint')
        if not external and (normalized['web_model_tests'] or normalized['hold_channels']):
            require(isinstance(normalized['routing_fingerprint'], str) and normalized['routing_fingerprint'],
                    'invalid_operator_deferrals')
        result[label] = normalized
    return result


def execution_options(plan):
    labels = [target['label'] for target in plan['targets']]
    schema = plan.get('schema')
    require(type(schema) is int and schema in {1, 2}, 'unsupported_plan_schema')
    if schema == 2:
        require(not plan.get('operator_deferrals') and not plan.get('fast_model_tests'), 'ambiguous_plan_exceptions')
        options = target_options(plan.get('target_options', {}), labels)
    else:
        require(not plan.get('target_options'), 'unsupported_legacy_target_options')
        legacy = plan.get('operator_deferrals') or {}
        fast = plan.get('fast_model_tests') or []
        require(isinstance(legacy, dict), 'invalid_operator_deferrals')
        require(not legacy and not fast or len(labels) == 1, 'ambiguous_legacy_exceptions')
        options = target_options({labels[0]: {**legacy, 'fast_model_tests': fast}} if legacy or fast else {}, labels)
    mode = plan.get('acceptance_mode', 'availability')
    require(mode in {'full', 'availability'}, 'invalid_acceptance_mode')
    return {label: {'deferred_web_tests': frozenset(options.get(label, {}).get('web_model_tests', [])),
                    'held_channels': frozenset(options.get(label, {}).get('hold_channels', [])),
                    'fast_model_tests': frozenset(options.get(label, {}).get('fast_model_tests', [])),
                    'approved_routing': options.get(label, {}).get('routing_fingerprint'),
                    'acceptance_mode': mode} for label in labels}


def routing_fingerprint(snapshot, *, include_status=True):
    return digest({site: {str(row['id']): [row['status'] if include_status else None, channel_fingerprint(row)]
                          for row in rows} for site, rows in snapshot['channels'].items()})


def validate_observed_options(before, routes, deferred, held, fast, *, approved_routing=None, include_status=False):
    known = {f"{site}:{row['id']}" for site, rows in routes['channels'].items() for row in rows}
    require(set(held) <= known, 'held_channel_not_found')
    if held and not deferred:
        web = before.get('web') or {}
        require(web.get('enabled') is False and not web.get('unavailable'), 'held_web_path_no_longer_disabled')
    if fast:
        jobs, _ = catalog.jobs(before)
        require(set(fast) <= {job['id'] for job in jobs}, 'fast_smoke_model_unavailable_before_stop')
    require(not approved_routing or routing_fingerprint(routes, include_status=include_status) == approved_routing,
            'channel_configuration_changed_since_deferral_approval')

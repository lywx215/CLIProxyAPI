"""Shared policy and stable fingerprints. No network or filesystem side effects."""
from __future__ import annotations

import hashlib
import json
import re
from pathlib import Path
from urllib.parse import urlsplit

from .concurrency import ReadLimiter


RETIRED_HOSTS = frozenset({'152.53.208.245', '159.195.16.180'})
EXCLUDED_MODELS = frozenset({'gemini-2.5-flash', 'gemini-2.5-flash-lite'})
PROVIDERS = frozenset({'gemini-enterprise', 'gemini-enterprise-web'})
MAX_READ_WORKERS = 4
READ_SLOTS = ReadLimiter(MAX_READ_WORKERS, 2)
MAX_TEST_WORKERS = 2
TEST_TTL = 600
REPO = Path(__file__).resolve().parents[2]


class OpsError(Exception):
    """Only a safe code and a deliberately public message may escape a client."""

    def __init__(self, code, message=None, *, category='blocked'):
        self.code = code
        self.category = category
        super().__init__(message or code)


def require(condition, code, message=None, *, category='blocked'):
    if not condition:
        raise OpsError(code, message, category=category)


def digest(value):
    if not isinstance(value, bytes):
        value = json.dumps(value, sort_keys=True, ensure_ascii=True, separators=(',', ':')).encode()
    return hashlib.sha256(value).hexdigest()


def sha_file(path):
    result = hashlib.sha256()
    with Path(path).open('rb') as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b''):
            result.update(block)
    return result.hexdigest()


def identifier(value):
    require(isinstance(value, str) and re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_.-]{0,95}', value),
            'invalid_identifier')
    return value


def channel_fingerprint(row):
    volatile = {'status', 'response_time', 'test_time', 'used_quota', 'other_info'}
    return digest({key: value for key, value in row.items() if key not in volatile})


def credential_settings(files):
    """Compare durable intent, excluding refreshable tokens and usage counters."""
    keys = ('name', 'type', 'provider', 'disabled', 'prefix', 'priority', 'proxy_url',
            'model_policy', 'subscription_tier', 'client_id', 'email', 'account',
            'account_id', 'account_type', 'project_id')
    binding_keys = ('id', 'binding_id', 'project_id', 'location', 'collection_id',
                    'engine_id', 'assistant_id', 'disabled', 'settings')
    result = []
    for row in files:
        item = {key: row.get(key) for key in keys if key in row}
        enterprise = row.get('gemini_enterprise') or {}
        collections = row.get('collections') or row.get('gemini_enterprise_collections') or enterprise.get('collections') or []
        if isinstance(collections, list):
            item['collections'] = sorted(
                [{key: binding.get(key) for key in binding_keys if key in binding}
                 for binding in collections], key=lambda value: json.dumps(value, sort_keys=True))
        result.append(item)
        item['enterprise_settings'] = {key: enterprise.get(key) for key in
                                       ('client_id', 'client_secret_env', 'model_policy', 'project_id', 'location')
                                       if key in enterprise}
        web = row.get('gemini_enterprise_web') or {}
        item['web_settings'] = {key: web.get(key) for key in
                               ('page_url', 'location', 'auth_user', 'user_agent', 'model_policy') if key in web}
    return sorted(result, key=lambda value: json.dumps(value, sort_keys=True))


def deployment_identity(snapshot):
    runtime = snapshot['runtime']
    keys = ('binary_sha256', 'image_id', 'deployment_id', 'disk_page_sha256', 'oauth_secret_present')
    return {'runtime': {key: runtime[key] for key in keys if key in runtime},
            'page_sha256': snapshot['page_sha256']}


def validate_target(label, target):
    identifier(label)
    require(not target.get('retired') and label not in {'A', 'B'}, 'retired_target')
    origin = urlsplit(target.get('origin', ''))
    addresses = {target.get('host'), origin.hostname, *(target.get('addresses') or [])}
    require(not addresses.intersection(RETIRED_HOSTS), 'retired_target')
    require(origin.scheme in {'http', 'https'} and origin.hostname and not origin.username
            and not origin.password and origin.path in {'', '/'} and not origin.query
            and not origin.fragment, 'invalid_target_origin')
    require(target.get('kind') in {'zeabur', 'vps'}, 'invalid_target_kind')
    if target['kind'] == 'zeabur':
        require(origin.scheme == 'https', 'cloud_requires_https')
        require(target.get('branch') and target.get('alias'), 'missing_deployment_binding')
    else:
        require(target.get('host') == origin.hostname and target.get('ssh'), 'missing_vps_identity')
        require(target.get('base') in {'/opt/cliproxyapi', '/opt/cliproxyapi-netcup'}, 'invalid_vps_root')
        require(re.fullmatch(r'[a-z][a-z0-9_-]{0,47}', target.get('project', '')), 'invalid_compose_project')


def validate_selection(inventory, labels):
    require(labels and len(labels) == len(set(labels)), 'empty_or_duplicate_targets')
    targets = inventory.get('targets', {})
    for label in labels:
        require(label in targets, 'unknown_target')
        validate_target(label, targets[label])
    return [dict(targets[label], label=label) for label in labels]


def is_claude_model(name):
    return isinstance(name, str) and re.search(r'(?:^|[./:\]])claude(?:[-_]|$)', name.strip(), re.IGNORECASE) is not None


def text_capability(model):
    """Prefer an explicit catalog capability; use the ID only as an exclusion guard."""
    methods = model.get('supportedGenerationMethods') or model.get('supported_generation_methods')
    if methods and not any(method in methods for method in ('generateContent', 'streamGenerateContent')):
        return False
    outputs = model.get('supportedOutputModalities') or model.get('supported_output_modalities')
    if outputs and 'text' not in {str(value).lower() for value in outputs}:
        return False
    return not re.search(r'(?i)(image|imagen|video|\bveo(?:-|\b)|tts|whisper|speech|embed|rerank|audio|sora)', model['id'])


def failure_category(http=None, payload=None):
    """Quota/auth failures must never trigger a speculative code rollback."""
    text = json.dumps(payload or {}, ensure_ascii=True).lower()
    http = http if type(http) is int else None
    if http in {401, 403} or any(word in text for word in ('invalid_grant', 'invalid_client', 'unauthenticated')):
        return 'authorization'
    if http == 429 or any(word in text for word in ('resource_exhausted', 'quota_exceeded', 'model_cooldown')):
        return 'quota'
    if http and 500 <= http <= 599:
        return 'upstream'
    return 'functional'

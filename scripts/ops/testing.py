"""Small real calls, protocol-aware validation and bounded, resource-aware scheduling."""
from __future__ import annotations

from concurrent.futures import FIRST_COMPLETED, wait
from .concurrency import TaskExecutor as ThreadPoolExecutor
import base64
import json
import re
import struct
import time
from urllib.parse import quote
import uuid
import zlib

from .core import MAX_TEST_WORKERS, OpsError, digest, failure_category, require
from .telemetry import operation, note


def upstream_error_evidence(payload, retry_after=None):
    """Keep only fixed status codes and retry seconds; never preserve upstream messages."""
    allowed = {'resource_exhausted', 'quota_exceeded', 'model_cooldown', 'rate_limit_exceeded',
               'insufficient_quota', 'daily_limit_exceeded', 'invalid_grant', 'invalid_client',
               'permission_denied', 'unauthenticated', 'model_not_found', 'failed_precondition',
               'invalid_argument', 'upstream_invalid_request', 'model_not_enabled'}
    error = payload.get('error', payload) if isinstance(payload, dict) else {}
    evidence = {}
    if isinstance(error, dict):
        fields = [error.get(key) for key in ('code', 'status', 'type', 'reason')]
        upstream = error.get('upstream')
        if isinstance(upstream, dict):
            fields.extend(upstream.get(key) for key in ('status', 'reason'))
        details = error.get('details')
        if isinstance(details, list):
            fields.extend(item.get('reason') for item in details if isinstance(item, dict))
        codes = sorted({value.lower() for value in fields if isinstance(value, str) and value.lower() in allowed})
        if codes:
            evidence['upstream_error_codes'] = codes
        code = error.get('code')
        if type(code) is int and 400 <= code <= 599:
            evidence['upstream_http_status'] = code
    if isinstance(retry_after, str) and re.fullmatch(r'[0-9]{1,9}', retry_after.strip()):
        evidence['retry_after_seconds'] = int(retry_after)
    return evidence


def events(lines):
    data = []
    for line in lines:
        line = line.decode('utf-8', 'strict').rstrip('\r\n') if isinstance(line, bytes) else line.rstrip('\r\n')
        if not line:
            if data:
                yield '\n'.join(data)
                data = []
        elif line.startswith('data:'):
            data.append(line[5:].lstrip(' '))
    if data:
        yield '\n'.join(data)


def parse_stream(lines, marker=None, *, native=False, clock=time.monotonic):
    started = clock()
    text, reasoning, finishes, count, first, done, error, usage = '', 0, [], 0, None, False, None, None
    evidence = {}
    for payload in events(lines):
        if payload == '[DONE]':
            done = True
            break
        item = json.loads(payload)
        require(isinstance(item, dict), 'invalid_sse_event')
        count += 1
        if item.get('error'):
            evidence = upstream_error_evidence(item)
            error = failure_category(evidence.get('upstream_http_status'), item['error'])
            break
        if native:
            usage = item.get('usageMetadata', usage)
            candidates = item.get('candidates') or []
            for candidate in candidates:
                if candidate.get('finishReason'):
                    finishes.append(candidate['finishReason'])
                for part in (candidate.get('content') or {}).get('parts') or []:
                    value = part.get('text') or ''
                    if part.get('thought'):
                        reasoning += len(value)
                    else:
                        if value and first is None:
                            first = clock() - started
                        text += value
        else:
            usage = item.get('usage', usage)
            for choice in item.get('choices') or []:
                delta = choice.get('delta') or {}
                value = delta.get('content') or ''
                if isinstance(value, list):
                    value = ''.join(part.get('text', '') for part in value if isinstance(part, dict))
                require(isinstance(value, str), 'invalid_stream_text')
                if value and first is None:
                    first = clock() - started
                text += value
                thought = delta.get('reasoning_content') or delta.get('reasoning') or ''
                if isinstance(thought, str):
                    reasoning += len(thought)
                if choice.get('finish_reason'):
                    finishes.append(choice['finish_reason'])
        require(len(text) <= 1024 * 1024 and count <= 100000, 'test_response_limit')
    availability = marker is None
    if native:
        finished = 'STOP' in finishes or (availability and 'MAX_TOKENS' in finishes)
    else:
        finished = done and ('stop' in finishes or (availability and 'length' in finishes))
    def numeric_usage(value):
        if not isinstance(value, dict):
            return None
        return {key: (numeric_usage(child) if isinstance(child, dict) else child)
                for key, child in value.items() if isinstance(child, dict) or type(child) in {int, float}}
    return {'success': error is None and bool(text.strip()) and (availability or marker in text) and finished,
            'first_text_seconds': round(first, 3) if first is not None else None,
            'content_chars': len(text), 'reasoning_chars': reasoning, 'events': count,
            'done': done, 'finish_reasons': finishes, 'usage': numeric_usage(usage), 'category': error or 'functional',
            **evidence}


def tiny_png():
    """A deterministic 32x16 red/blue fixture generated without external libraries."""
    def chunk(name, value):
        return struct.pack('!I', len(value)) + name + value + struct.pack('!I', zlib.crc32(name + value) & 0xffffffff)
    row = b'\x00' + b'\xff\x00\x00' * 16 + b'\x00\x00\xff' * 16
    return b'\x89PNG\r\n\x1a\n' + chunk(b'IHDR', struct.pack('!IIBBBBB', 32, 16, 8, 2, 0, 0, 0)) + \
        chunk(b'IDAT', zlib.compress(row * 16)) + chunk(b'IEND', b'')


def verify_route(trace, indexes):
    """Validate the existing trace-header identity without disclosing it in reports."""
    if not trace or not indexes:
        return False
    return any(re.fullmatch(r'\d{14}-' + re.escape(index) + r'-[A-Za-z0-9_.:-]+', trace)
               for index in indexes if isinstance(index, str) and index)


def stream_call(app, job, model_key):
    availability = job.get('availability_only', False)
    marker = None if availability else 'CPA_OK_' + f'{uuid.uuid4().int % 100_000_000:08d}'
    native = job['provider'] == 'gemini-enterprise-web'
    if native:
        path = '/v1beta/models/' + quote(job['model'], safe='') + ':streamGenerateContent?alt=sse'
        if availability:
            parts = [{'text': 'Reply briefly with OK.'}]
        else:
            parts = [
                {'text': 'Read the attached text and output only its verification code.'},
                {'inlineData': {'mimeType': 'image/png', 'data': base64.b64encode(tiny_png()).decode()}},
                {'inlineData': {'mimeType': 'text/plain', 'data': base64.b64encode(('Verification code: ' + marker).encode()).decode()}}
            ]
        body = {'contents': [{'role': 'user', 'parts': parts}],
                'generationConfig': {'maxOutputTokens': 128 if availability else 2048}}
    else:
        path = '/v1/chat/completions'
        prompt = 'Reply briefly with OK.' if availability else (
            'Copy this verification code exactly as your entire reply. No Markdown or explanation.\n' + marker)
        body = {'model': job['model'], 'messages': [{'role': 'user', 'content': prompt}],
                'stream': True, 'max_tokens': 128 if availability else 2048, 'stream_options': {'include_usage': True}}
    start = time.monotonic()
    connection, response = app.http.open(path, 'POST', body, key=model_key,
                                          headers={'Accept': 'text/event-stream'}, timeout=240)
    try:
        if response.status != 200:
            value = response.read(65536)
            try:
                payload = json.loads(value)
            except ValueError:
                payload = {}
            return {'success': False, 'http': response.status,
                    'category': failure_category(response.status, payload), 'seconds': round(time.monotonic() - start, 3),
                    **upstream_error_evidence(payload, response.getheader('Retry-After'))}
        require('text/event-stream' in (response.getheader('Content-Type') or '').lower(), 'expected_sse_response')
        headers_time = time.monotonic() - start
        result = parse_stream(response, marker, native=native)
        route_verified = verify_route(response.getheader('X-CPA-TRACE-ID'), job.get('auth_indexes', []))
        result['provider_route_verified'] = route_verified
        if not route_verified:
            if result.get('success'):
                result.update(category='functional', code='provider_route_unverified')
            result['success'] = False
        if result['first_text_seconds'] is not None:
            result['first_text_seconds'] = round(result['first_text_seconds'] + headers_time, 3)
        return {**result, 'http': 200, 'seconds': round(time.monotonic() - start, 3),
                'acceptance_mode': 'availability' if availability else 'full'}
    finally:
        response.close()
        connection.close()


def model_call(adapter, job, snapshot):
    adapter.guard()
    availability = job.get('availability_only', False)
    with adapter.application(snapshot.get('host')) as app:
        if job['provider'] == 'gemini-enterprise-web' and not availability:
            # The existing native-format probe pins both provider and credential and
            # checks image colors plus a generated attachment nonce on the server.
            result = app.http.request('/v8/management/credentials/gemini-enterprise-web/models/test',
                                      'POST', job['probe'], timeout=240)
            value = result['data'] or {}
            verified = result['http'] == 200 and value.get('status') == 'ok' and value.get('http_status') == 200 \
                and value.get('provider') == 'gemini-enterprise-web' and value.get('test_kind') == 'inline_media' \
                and value.get('upstream_model') == job['probe']['expected_upstream_model']
            if not verified:
                return {'success': False, 'http': result['http'], 'category': failure_category(value.get('http_status'), value),
                        'code': 'web_pinned_media_probe_failed'}
        result = stream_call(app, job, snapshot['model_key'])
        if job['provider'] == 'gemini-enterprise-web':
            result['provider_pinned_media_probe_passed'] = not availability
            result['native_public_stream_tested'] = True
        return result


def retry_call(call, *, sleep=time.sleep):
    result = None
    for attempt in range(1, 3):
        try:
            with operation('functional_call', kind='call'):
                result = call()
        except OpsError as error:
            result = {'success': False, 'category': error.category, 'code': error.code}
        except (OSError, TimeoutError):
            result = {'success': False, 'category': 'network', 'code': 'model_connection_failed'}
        except (ValueError, UnicodeError):
            result = {'success': False, 'category': 'functional', 'code': 'invalid_model_response'}
        result['attempts'] = attempt
        if result.get('success') or result.get('category') not in {'network', 'upstream'}:
            break
        if attempt == 1:
            note('retry', scope='call', attempt=2, category=result.get('category'))
            with operation('call_retry_wait', kind='wait'):
                sleep(2)
    return result


def conflicts(resources, active):
    if not active:
        return False
    return '*' in resources or '*' in active or bool(set(resources) & active)


def run_jobs(jobs, call, save, cached=None, *, clock=time.time, ttl=600, workers=MAX_TEST_WORKERS):
    """At most two jobs, and never overlap possibly shared quota/cookie resources."""
    require(1 <= workers <= MAX_TEST_WORKERS, 'test_concurrency_exceeded')
    pending = list(jobs)
    results = {}
    cached = cached or {}
    for job in list(pending):
        old = cached.get(job['id'], {})
        age = clock() - old.get('checked_at', 0)
        if old.get('success') is True and old.get('fingerprint') == job['fingerprint'] and 0 <= age <= ttl:
            note('cache_hit', scope='channel' if job.get('provider') == 'new-api' else 'model', job=job['id'])
            results[job['id']] = old
            pending.remove(job)
    active = {}
    with ThreadPoolExecutor(max_workers=workers) as executor:
        while pending or active:
            resources = {resource for job in active.values() for resource in job['resources']}
            for job in list(pending):
                if len(active) >= workers:
                    break
                if conflicts(job['resources'], resources):
                    continue
                future = executor.submit(call, job)
                active[future] = job
                resources.update(job['resources'])
                pending.remove(job)
            require(active, 'test_scheduler_stalled')
            completed, _ = wait(active, return_when=FIRST_COMPLETED)
            for future in completed:
                job = active.pop(future)
                value = future.result()
                result = {**value, 'model': job['model'], 'provider': job['provider'],
                          'fingerprint': job['fingerprint'], 'checked_at': clock()}
                results[job['id']] = result
                save(job['id'], result)
            # Finish already-issued calls naturally, but do not issue new calls after failure.
            if any(not value.get('success') for value in results.values()):
                pending.clear()
    return results

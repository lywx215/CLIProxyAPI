import io
import json
import threading
import unittest

from scripts.ops.testing import events, parse_stream, retry_call, run_jobs, tiny_png, upstream_error_evidence, verify_route
from scripts.ops.testing import model_call
from unittest.mock import MagicMock


def sse(*values):
    return io.BytesIO(''.join('data: ' + (value if isinstance(value, str) else json.dumps(value)) + '\n\n'
                            for value in values).encode())


class StreamingTests(unittest.TestCase):
    def test_availability_accepts_a_completed_text_reply_without_an_exact_marker(self):
        result = parse_stream(sse({'choices': [{'delta': {'content': 'Available'}, 'finish_reason': 'stop'}]}, '[DONE]'))
        self.assertTrue(result['success'])

    def test_availability_rejects_error_events_truncation_and_reasoning_only(self):
        cases = [sse({'error': {'code': 429}}),
                 sse({'choices': [{'delta': {'content': 'OK'}}]}),
                 sse({'choices': [{'delta': {'reasoning_content': 'hidden'}, 'finish_reason': 'stop'}]}, '[DONE]')]
        for response in cases:
            self.assertFalse(parse_stream(response)['success'])

    def test_availability_accepts_completed_output_at_its_small_token_limit(self):
        response = sse({'choices': [{'delta': {'content': 'OK'}, 'finish_reason': 'length'}]}, '[DONE]')
        self.assertTrue(parse_stream(response)['success'])
        native = sse({'candidates': [{'content': {'parts': [{'text': 'OK'}]}, 'finishReason': 'MAX_TOKENS'}]})
        self.assertTrue(parse_stream(native, native=True)['success'])

    def test_web_availability_uses_one_text_call_and_reuses_the_verified_host(self):
        adapter = MagicMock()
        app = adapter.application.return_value.__enter__.return_value
        response = sse({'candidates': [{'content': {'parts': [{'text': 'OK'}]}, 'finishReason': 'STOP'}]})
        response.status = 200
        response.getheader = lambda name: 'text/event-stream' if name == 'Content-Type' else '20261112010203-auth-request'
        connection = MagicMock()
        app.http.open.return_value = connection, response
        host = {'management_key': 'synthetic-key'}
        result = model_call(adapter, {'model': 'gemini-4-argon', 'provider': 'gemini-enterprise-web',
                                     'availability_only': True, 'auth_indexes': ['auth']},
                            {'host': host, 'model_key': 'synthetic-api-key'})
        self.assertTrue(result['success'])
        adapter.application.assert_called_once_with(host)
        app.http.request.assert_not_called()
        payload = app.http.open.call_args.args[2]
        self.assertEqual(payload['generationConfig']['maxOutputTokens'], 128)
        self.assertEqual(payload['contents'][0]['parts'], [{'text': 'Reply briefly with OK.'}])
        self.assertFalse(result['provider_pinned_media_probe_passed'])
        connection.close.assert_called_once()

    def test_actual_route_must_match_provider_credential_identity(self):
        self.assertTrue(verify_route('20261108120100-child-abc-request123', ['child-abc']))
        self.assertFalse(verify_route('20261108120100-web-other-request123', ['enterprise-child']))
        self.assertFalse(verify_route(None, ['enterprise-child']))
        self.assertFalse(verify_route('20261108120100-child-abc-request123', []))

    def test_complete_openai_stream_passes(self):
        result = parse_stream(sse({'choices': [{'delta': {'reasoning_content': 'hidden', 'content': 'CPA_OK'}, 'finish_reason': 'stop'}]}, '[DONE]'), 'CPA_OK')
        self.assertTrue(result['success'])
        self.assertEqual(result['reasoning_chars'], 6)
        self.assertNotIn('hidden', json.dumps(result))

    def test_http_200_error_event_does_not_pass(self):
        self.assertFalse(parse_stream(sse({'error': {'message': 'failed'}}, '[DONE]'), 'CPA_OK')['success'])

    def test_quota_evidence_is_retained_without_the_error_message_or_metadata(self):
        payload = {'error': {'status': 'RESOURCE_EXHAUSTED', 'code': 429, 'message': 'SECRET',
                             'details': [{'reason': 'RATE_LIMIT_EXCEEDED', 'metadata': {'key': 'SECRET'}},
                                         {'reason': 'SECRET'}]}}
        expected = {'upstream_error_codes': ['rate_limit_exceeded', 'resource_exhausted'], 'upstream_http_status': 429}
        self.assertEqual(upstream_error_evidence(payload, '12'), {**expected, 'retry_after_seconds': 12})
        self.assertEqual(upstream_error_evidence(payload, 'SECRET'), expected)
        result = parse_stream(sse(payload, '[DONE]'), 'CPA_OK')
        self.assertEqual(result['category'], 'quota')
        self.assertEqual(result['upstream_error_codes'], expected['upstream_error_codes'])
        self.assertNotIn('SECRET', json.dumps(result))

    def test_truncated_stream_does_not_pass(self):
        stream = sse({'choices': [{'delta': {'content': 'CPA_OK'}, 'finish_reason': 'stop'}]})
        self.assertFalse(parse_stream(stream, 'CPA_OK')['success'])

    def test_google_precondition_diagnostic_is_retained_without_freeform_values(self):
        payload = {'error': {'code': 'upstream_invalid_request', 'message': 'SECRET', 'upstream': {
            'status': 'FAILED_PRECONDITION', 'reason': 'SECRET', 'field_violations': [{'field': 'SECRET'}]}}}
        self.assertEqual(upstream_error_evidence(payload), {
            'upstream_error_codes': ['failed_precondition', 'upstream_invalid_request']})

    def test_length_finish_does_not_pass(self):
        stream = sse({'choices': [{'delta': {'content': 'CPA_OK'}, 'finish_reason': 'length'}]}, '[DONE]')
        self.assertFalse(parse_stream(stream, 'CPA_OK')['success'])

    def test_reasoning_alone_is_not_a_text_answer(self):
        stream = sse({'choices': [{'delta': {'reasoning_content': 'CPA_OK'}, 'finish_reason': 'stop'}]}, '[DONE]')
        self.assertFalse(parse_stream(stream, 'CPA_OK')['success'])

    def test_native_sse_uses_finish_reason_without_openai_done(self):
        stream = sse({'candidates': [{'content': {'parts': [{'text': 'CPA_OK'}]}, 'finishReason': 'STOP'}]})
        self.assertTrue(parse_stream(stream, 'CPA_OK', native=True)['success'])

    def test_multiline_sse_and_comments(self):
        values = list(events(io.BytesIO(b':keepalive\n\ndata: {"one":\ndata: 1}\n\n')))
        self.assertEqual(json.loads(values[0]), {'one': 1})

    def test_unknown_usage_text_is_not_reported(self):
        stream = sse({'usage': {'prompt_tokens': 1, 'secret': 'do-not-report'}, 'choices': [
            {'delta': {'content': 'CPA_OK'}, 'finish_reason': 'stop'}]}, '[DONE]')
        result = parse_stream(stream, 'CPA_OK')
        self.assertEqual(result['usage'], {'prompt_tokens': 1})

    def test_transient_failure_gets_only_one_retry(self):
        calls = []
        def call():
            calls.append(1)
            return {'success': False, 'category': 'upstream'}
        self.assertEqual(retry_call(call, sleep=lambda seconds: None)['attempts'], 2)
        self.assertEqual(len(calls), 2)

    def test_auth_and_quota_failures_are_not_retried(self):
        for category in ['authorization', 'quota', 'functional']:
            result = retry_call(lambda: {'success': False, 'category': category}, sleep=lambda seconds: self.fail())
            self.assertEqual(result['attempts'], 1)

    def test_fixture_is_small_and_deterministic(self):
        self.assertEqual(tiny_png(), tiny_png())
        self.assertTrue(tiny_png().startswith(b'\x89PNG'))
        self.assertLess(len(tiny_png()), 1024)

    def test_independent_resources_can_run_two_tests(self):
        barrier = threading.Barrier(2)
        jobs = [{'id': str(i), 'model': str(i), 'provider': 'gemini-enterprise',
                 'resources': [str(i)], 'fingerprint': str(i)} for i in range(2)]
        def call(job):
            barrier.wait(timeout=5)
            return {'success': True}
        result = run_jobs(jobs, call, lambda *args: None)
        self.assertTrue(all(row['success'] for row in result.values()))

    def test_shared_or_unknown_resources_never_overlap(self):
        for resources in [['pool'], ['*']]:
            state, lock = {'active': 0, 'max': 0}, threading.Lock()
            jobs = [{'id': str(i), 'model': str(i), 'provider': 'gemini-enterprise',
                     'resources': resources, 'fingerprint': str(i)} for i in range(4)]
            def call(job):
                with lock:
                    state['active'] += 1
                    state['max'] = max(state['active'], state['max'])
                with lock:
                    state['active'] -= 1
                return {'success': True}
            run_jobs(jobs, call, lambda *args: None)
            self.assertEqual(state['max'], 1)

    def test_cache_requires_matching_fingerprint_and_age(self):
        jobs = [{'id': 'one', 'model': 'm', 'provider': 'gemini-enterprise', 'resources': ['*'], 'fingerprint': 'new'}]
        calls = []
        call = lambda job: (calls.append(job['id']) or {'success': True})
        cached = {'one': {'success': True, 'fingerprint': 'new', 'checked_at': 100}}
        run_jobs(jobs, call, lambda *args: None, cached, clock=lambda: 699)
        self.assertEqual(calls, [])
        run_jobs(jobs, call, lambda *args: None, cached, clock=lambda: 701)
        self.assertEqual(calls, ['one'])


if __name__ == '__main__':
    unittest.main()

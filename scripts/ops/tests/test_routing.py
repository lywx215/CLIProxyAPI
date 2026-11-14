import copy
import json
import threading
import uuid
import unittest
from unittest.mock import MagicMock, patch

from scripts.ops.core import OpsError
from scripts.ops.routing import Observer, Routing


class RoutingTests(unittest.TestCase):
    @staticmethod
    def observer():
        observer = MagicMock()
        observer.nonce.side_effect = lambda: uuid.uuid4().hex
        observer.read_many.side_effect = lambda nonces: {nonce: observer.read(nonce) for nonce in nonces}
        return observer

    def setUp(self):
        self.routing = Routing({'main': {'origin': 'https://gemini30.pro', 'propagation': 'upstream',
                                       'entry_addresses': ['192.0.2.1'], 'observers': {'entry': {}}}}, None, None)
        self.topology = {'entry': {'backends': [{'address': '10.0.0.1:3000', 'down': False, 'backup': False},
                                               {'address': '10.0.0.2:3000', 'down': False, 'backup': False}],
                                  'apps': [{'ips': ['172.20.0.2']}]}}

    def test_forwarded_packet_does_not_prove_local_vip_app_receipt(self):
        self.assertIsNone(Routing.observed_destination({'propagation': 'app'}, self.topology,
                                                       {'host': 'entry', 'backend': '10.0.0.2:3000'}))
        self.assertEqual(Routing.observed_destination({'propagation': 'app'}, self.topology,
                                                     {'host': 'entry', 'backend': '172.20.0.2:3000'}), ('entry', 'app'))

    def test_one_backend_receipt_cannot_satisfy_full_propagation(self):
        self.routing.request = MagicMock(side_effect=lambda site, path, **kwargs: {'success': True, 'data': 1 if kwargs.get('body') else {'start_time': 10}})
        observer = self.observer()
        observer.read.return_value = [{'host': 'entry', 'backend': '10.0.0.1:3000'}]
        with patch('scripts.ops.routing.Observer', return_value=observer):
            with self.assertRaisesRegex(OpsError, 'propagation_incomplete'):
                self.routing.propagate('main', [1], 2, self.topology)
        observer.close.assert_called_once()

    def test_every_observed_backend_and_startup_is_required(self):
        self.routing.request = MagicMock(side_effect=lambda site, path, **kwargs: {'success': True, 'data': 1 if kwargs.get('body') else {'start_time': 10}})
        observer = self.observer()
        observer.read.side_effect = [[{'host': 'entry', 'backend': backend}]
                                     for backend in ['10.0.0.1:3000', '10.0.0.2:3000'] * 2]
        with patch('scripts.ops.routing.Observer', return_value=observer):
            evidence = self.routing.propagate('main', [1], 2, self.topology)
        self.assertEqual(len(evidence['backends']), 2)
        self.assertEqual(len(evidence['startups']), 2)

    def test_additional_public_ip_backend_cannot_be_ignored(self):
        self.routing.sites['main']['observers']['entry']['extra_active_backends'] = ['127.0.0.1:3003']
        self.routing.request = MagicMock(return_value={'success': True, 'data': 1})
        observer = self.observer()
        observer.read.return_value = [{'host': 'entry', 'backend': backend} for backend in ['10.0.0.1:3000', '10.0.0.2:3000']]
        with patch('scripts.ops.routing.Observer', return_value=observer):
            with self.assertRaisesRegex(OpsError, 'propagation_incomplete'):
                self.routing.propagate('main', [1], 2, self.topology)

    def test_unreachable_public_ip_backend_blocks_readonly_preflight_before_stop(self):
        self.routing.sites['main']['observers']['entry']['extra_active_backends'] = ['127.0.0.1:3003']
        self.routing.channels = lambda domains: {'main': [{'id': 1, 'status': 1}]}
        self.routing.topology = lambda: {'main': self.topology}
        self.routing.request = MagicMock(return_value={'success': True, 'data': {'start_time': 10}})
        observer = self.observer()
        observer.read.return_value = [{'host': 'entry', 'backend': backend} for backend in ['10.0.0.1:3000', '10.0.0.2:3000']]
        with patch('scripts.ops.routing.Observer', return_value=observer):
            with self.assertRaisesRegex(OpsError, 'newapi_startup_observation_incomplete'):
                self.routing.snapshot(['example.test'])
        self.assertTrue(all(call.args[1].startswith('/api/status?') and not call.kwargs.get('body')
                            for call in self.routing.request.call_args_list))

    def test_external_channel_change_is_not_overwritten(self):
        snapshot = {'domains': ['example.test'], 'channels': {'main': [{'id': 1, 'status': 1, 'models': 'old'}]}}
        self.routing.channels = lambda domains: {'main': [{'id': 1, 'status': 2, 'models': 'externally-edited'}]}
        with self.assertRaisesRegex(OpsError, 'channel_configuration_changed'):
            self.routing.confirm(snapshot)

    def test_ambiguous_retry_packet_cannot_identify_response_backend(self):
        events = [{'host': 'entry', 'backend': address} for address in ['10.0.0.1:3000', '10.0.0.2:3000']]
        self.assertIsNone(self.routing.attributed('main', self.topology, events, self.routing.wanted('main', self.topology)))

    def test_parallel_batches_preserve_all_events_for_each_marker(self):
        observer = Observer(None, ())
        now = [0]
        observer.clock = lambda: now[0]
        first, second, third = observer.nonce(), observer.nonce(), observer.nonce()
        a, b = '10.0.0.1:3000', '10.0.0.2:3000'
        for nonce, address in [(first, a), (third, b), (second, a), (second, b), (first, a)]:
            observer.record('entry', {'nonce': nonce, 'backend': address, 'host': 'ignored'})
        for nonce in [first, second, third]:
            observer.complete(nonce)
        now[0] = 2
        batch = observer.read_many([first, second])
        self.assertEqual([event['backend'] for event in batch[first]], [a, a])
        self.assertEqual([event['backend'] for event in batch[second]], [a, b])
        self.assertIsNone(self.routing.attributed('main', self.topology, batch[second], self.routing.wanted('main', self.topology)))
        self.assertEqual(observer.read_many([third]), {third: [{'host': 'entry', 'backend': b}]})

    def test_batched_probes_rotate_every_entry(self):
        config = {'entry_addresses': ['entry-a', 'entry-b', 'entry-c', 'entry-d']}
        batches = list(Routing.public_batches(config, {('a', 'x'), ('b', 'y')}))
        self.assertEqual(batches[:2], [[(False, 'entry-a'), (False, 'entry-b')], [(False, 'entry-c'), (False, 'entry-d')]])
        self.assertEqual(sum(map(len, batches)), 32)

    def test_two_sites_write_concurrently_after_durable_intent(self):
        self.routing.sites['vip'] = self.routing.sites['main']
        topology = {site: self.topology for site in self.routing.sites}
        snapshot = {'domains': ['example.test'], 'topology': topology,
                    'startups': {site: {'entry/backend': 10} for site in self.routing.sites},
                    'channels': {site: [{'id': 1, 'status': 1}, {'id': 2, 'status': 2}] for site in self.routing.sites}}
        self.routing.prepare_session = MagicMock()
        self.routing.topology = MagicMock(return_value=topology)
        self.routing.confirm = MagicMock(return_value=snapshot['channels'])
        self.routing.observe_startups = MagicMock(return_value={'entry/backend': 10})
        self.routing.confirm_shadow_rows = MagicMock()
        self.routing.record_event = MagicMock()
        barrier = threading.Barrier(2)

        def propagate(site, ids, status, topology, *, intent):
            self.routing.record_event.assert_called_once_with('propagation_intent', {
                'operation_id': intent, 'status': 2, 'sites': {'main': [1], 'vip': [1]}})
            self.assertEqual(ids, [1])
            barrier.wait(timeout=5)
            return {'startups': {'entry/backend': 10}}

        self.routing.propagate = propagate
        self.assertEqual(set(self.routing.set_stopped(snapshot, initial=True)), {'main', 'vip'})

    def test_process_identity_change_after_write_blocks_completion(self):
        topology = {'main': self.topology}
        snapshot = {'domains': ['example.test'], 'topology': topology, 'startups': {'main': {'entry/backend': 10}},
                    'channels': {'main': [{'id': 1, 'status': 1}]}}
        self.routing.prepare_session = MagicMock()
        self.routing.topology = MagicMock(return_value=topology)
        self.routing.confirm = MagicMock(return_value=snapshot['channels'])
        self.routing.observe_startups = MagicMock(return_value={'entry/backend': 10})
        self.routing.confirm_shadow_rows = MagicMock()
        self.routing.propagate = MagicMock(return_value={'startups': {'entry/backend': 11}})
        with self.assertRaisesRegex(OpsError, 'newapi_startup_changed'):
            self.routing.set_stopped(snapshot, initial=True)

    def test_extra_backend_status_is_proved_over_its_own_tls_relay(self):
        self.routing.sites['main']['observers']['entry'].update(shadow_admin_relay=True, extra_active_backends=['127.0.0.1:3003'])
        self.topology['entry']['shadow'] = {'address': '127.0.0.1:3003', 'container': 'newapi-slave1-shadow'}
        self.routing.request = MagicMock(return_value={'success': True, 'data': {'start_time': 10}})
        self.routing.shadow_request = MagicMock(return_value={'success': True, 'data': {'start_time': 20}})
        observer = self.observer()
        observer.read.side_effect = [[{'host': 'entry', 'backend': backend}]
                                     for backend in ['10.0.0.1:3000', '10.0.0.2:3000', '127.0.0.1:3003']]
        starts = self.routing.observe_startups('main', self.topology, observer)
        self.assertEqual(starts['entry/127.0.0.1:3003'], 20)
        self.routing.shadow_request.assert_called_once()

    def test_first_stop_rejects_a_new_external_disable(self):
        snapshot = {'domains': ['example.test'], 'topology': {'main': self.topology},
                    'channels': {'main': [{'id': 1, 'status': 1, 'models': 'same'}]}}
        self.routing.topology = lambda: snapshot['topology']
        self.routing.channels = lambda domains: {'main': [{'id': 1, 'status': 2, 'models': 'same'}]}
        self.routing.propagate = MagicMock()
        with self.assertRaisesRegex(OpsError, 'channel_status_changed'):
            self.routing.set_stopped(snapshot, initial=True)
        self.routing.propagate.assert_not_called()

    def test_status_writes_request_fresh_connections_and_safe_reads_still_reuse(self):
        self.routing.guard = MagicMock()
        self.routing.client = MagicMock()
        request = self.routing.client.return_value.request
        request.return_value = {'http': 200, 'data': {'success': True}}
        for status in (2, 1):
            with self.subTest(status=status):
                body = {'ids': [7], 'status': status}
                self.routing.request('vip', '/api/channel/status/batch', address='192.0.2.10', body=body)
                self.routing.client.assert_called_with('vip', '192.0.2.10')
                request.assert_called_with('/api/channel/status/batch', 'POST', body,
                                           timeout=45, read_only=False, fresh_connection=True)
        self.assertEqual(self.routing.guard.call_count, 2)
        self.routing.request('vip', '/api/status')
        self.assertFalse(request.call_args.kwargs['fresh_connection'])
        self.assertTrue(request.call_args.kwargs['read_only'])


    def test_functional_channel_failure_even_with_http_success(self):
        self.routing.confirm = MagicMock()
        self.routing.request = MagicMock(return_value={'success': False, 'message': 'RESOURCE_EXHAUSTED'})
        result = self.routing.test_channel({}, 'main', {'id': 1, 'status': 1, 'models': 'gemini-4-carbon'}, set())
        self.assertFalse(result['success'])
        self.assertEqual(result['category'], 'quota')

    def test_channel_test_skips_alias_mapped_to_excluded_model(self):
        self.routing.confirm = MagicMock()
        for upstream, excluded in [('gemini-2.5-flash', {'gemini-2.5-flash'}), ('anthropic/Claude-Opus-4-6', set())]:
            with self.subTest(upstream=upstream):
                self.routing.request = MagicMock(return_value={'success': True})
                row = {'id': 1, 'status': 1, 'models': 'premium-alias,claude-sonnet-5-5,gemini-4-carbon',
                       'test_model': 'premium-alias', 'model_mapping': json.dumps({'premium-alias': upstream})}
                self.assertEqual(self.routing.test_channel({}, 'main', row, excluded)['model'], 'gemini-4-carbon')
                self.routing.request.reset_mock()
                with self.assertRaisesRegex(OpsError, 'channel_has_no_eligible_test_model'):
                    self.routing.test_channel({}, 'main', {**row, 'models': 'premium-alias,claude-sonnet-5-5'}, excluded)
                self.routing.request.assert_not_called()


if __name__ == '__main__':
    unittest.main()

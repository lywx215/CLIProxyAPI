import io
from http.client import RemoteDisconnected
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import MagicMock, patch

from scripts.ops.core import OpsError
from scripts.ops.security import Vault, dpapi
from scripts.ops.transport import HTTP, SSHFactory


class SecurityTests(unittest.TestCase):
    @staticmethod
    def http_connection():
        connection = MagicMock()
        response = connection.getresponse.return_value
        response.status, response.will_close = 200, False
        response.read.return_value = b'{}'
        response.getheaders.return_value = []
        return connection

    def test_http_reuses_a_drained_connection_with_the_new_request_timeout(self):
        client = HTTP('https://example.test', key='synthetic')
        client.connection = MagicMock(return_value=self.http_connection())
        client.request('/api/status', read_only=True, timeout=45)
        client.request('/api/log/', read_only=True, timeout=5)
        connection = client.connection.return_value
        client.connection.assert_called_once_with(45)
        connection.sock.settimeout.assert_called_once_with(5)
        self.assertEqual(connection.request.call_count, 2)
        client.close()
        connection.close.assert_called_once()

    def test_stale_idle_http_connection_reopens_only_for_a_read(self):
        for read_only, method in [(True, 'GET'), (False, 'GET'), (False, 'POST')]:
            with self.subTest(read_only=read_only, method=method):
                old, fresh = self.http_connection(), self.http_connection()
                client = HTTP('https://example.test')
                client.connection = MagicMock(side_effect=[old, fresh])
                client.request('/api/status', read_only=True)
                old.request.side_effect = RemoteDisconnected()
                if read_only:
                    client.request('/api/status', method, read_only=True)
                    fresh.request.assert_called_once()
                else:
                    with self.assertRaisesRegex(OpsError, 'http_transport_failed'):
                        client.request('/api/channel/test/1', method)
                    fresh.request.assert_not_called()
                client.close()

    def test_ssh_http_connection_close_preserves_the_complete_chunked_response(self):
        from paramiko.channel import ChannelFile

        body = json.dumps({'files': ['x' * 128000]}).encode()
        wire = io.BytesIO(b'HTTP/1.1 200 OK\r\nConnection: close\r\nTransfer-Encoding: chunked\r\n\r\n'
                          + f'{len(body):x}\r\n'.encode() + body + b'\r\n0\r\n\r\n')
        ssh = MagicMock()
        channel = ssh.get_transport.return_value.open_channel.return_value
        channel.recv.side_effect = lambda size: b'' if channel.close.called else wire.read(size)
        channel.makefile.side_effect = lambda mode: ChannelFile(channel, mode)

        result = HTTP('http://127.0.0.1:8317', key='synthetic', ssh=ssh).request('/v8/management/credentials')

        self.assertEqual(result['data'], json.loads(body))
        channel.close.assert_called_once()

    def test_redirect_is_rejected_without_forwarding_authorization(self):
        connection = MagicMock()
        connection.getresponse.return_value.status = 302
        with patch('http.client.HTTPSConnection', return_value=connection):
            with self.assertRaisesRegex(OpsError, 'redirect_rejected'):
                HTTP('https://example.test', key='synthetic').request('/v1/models')
        self.assertEqual(connection.request.call_count, 1)
        connection.close.assert_called()

    def test_cross_origin_and_plain_http_are_rejected(self):
        with self.assertRaises(OpsError):
            HTTP('http://example.test', key='synthetic')
        client = HTTP('https://example.test', key='synthetic')
        for path in ['https://evil.test/', '//evil.test/', '/x#fragment']:
            with self.assertRaises(OpsError):
                client.open(path)

    def test_401_opens_circuit_before_second_network_request(self):
        connection = MagicMock()
        response = connection.getresponse.return_value
        response.status = 401
        response.read.return_value = b'{}'
        response.getheaders.return_value = []
        client = HTTP('https://example.test', key='synthetic')
        with patch('http.client.HTTPSConnection', return_value=connection):
            with self.assertRaises(OpsError):
                client.request('/first')
            with self.assertRaisesRegex(OpsError, 'authentication_circuit_open'):
                client.request('/second')
        self.assertEqual(connection.request.call_count, 1)

    @unittest.skipUnless(os.name == 'nt', 'Windows current-user DPAPI')
    def test_real_dpapi_encrypts_authenticates_and_roundtrips(self):
        value = b'synthetic-offline-secret-9eb410'
        encrypted = dpapi(value)
        self.assertNotIn(value, encrypted)
        self.assertEqual(dpapi(encrypted, True), value)
        broken = bytearray(encrypted)
        broken[-1] ^= 1
        with self.assertRaises(OpsError):
            dpapi(bytes(broken), True)

    def test_reference_rejects_wrong_origin_and_wrong_host(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'synthetic.dpapi'
            path.write_text(json.dumps({'host': '192.0.2.1', 'origin': 'https://example.test', 'token': 'synthetic'}))
            vault = Vault(lambda value, decrypt: value)
            for kwargs in [{'host': '192.0.2.2'}, {'origin': 'https://other.test'}]:
                with self.assertRaises(OpsError):
                    vault.reference({'path': str(path)}, **kwargs)

    def test_ssh_rejects_unknown_host_and_disables_implicit_key_search(self):
        import paramiko
        with tempfile.TemporaryDirectory() as directory:
            known = Path(directory) / 'known_hosts'
            known.write_text('synthetic')
            client = MagicMock()
            client.connect.side_effect = paramiko.SSHException('unknown host')
            factory = SSHFactory({'node': {'host': '192.0.2.1', 'user': 'root', 'port': 22,
                                          'known_hosts': str(known), 'identity': 'synthetic-key'}}, MagicMock())
            with patch.object(paramiko, 'SSHClient', return_value=client):
                with self.assertRaisesRegex(OpsError, 'verified_ssh_connection_failed'):
                    factory.connect('node')
            self.assertIsInstance(client.set_missing_host_key_policy.call_args.args[0], paramiko.RejectPolicy)
            self.assertFalse(client.connect.call_args.kwargs['look_for_keys'])
            self.assertFalse(client.connect.call_args.kwargs['allow_agent'])
            client.close.assert_called_once()

    def test_ssh_reuses_verified_transport_and_reconnects_through_host_key_policy(self):
        import paramiko
        with tempfile.TemporaryDirectory() as directory:
            known = Path(directory) / 'known_hosts'
            known.write_text('synthetic')
            old, new = MagicMock(), MagicMock()
            old.get_transport.return_value.is_active.side_effect = [True, False]
            factory = SSHFactory({'node': {'host': '192.0.2.1', 'user': 'root', 'port': 22,
                                          'known_hosts': str(known), 'identity': 'synthetic-key'}}, MagicMock())
            with patch.object(paramiko, 'SSHClient', side_effect=[old, new]), factory.session():
                factory.connect('node').close()
                self.assertIs(factory.connect('node').client, old)
                old.close.assert_not_called()
                self.assertIs(factory.connect('node').client, new)
                self.assertIsInstance(new.set_missing_host_key_policy.call_args.args[0], paramiko.RejectPolicy)
                new.load_host_keys.assert_called_once_with(str(known))
            old.close.assert_called_once()
            new.close.assert_called_once()

    def test_ssh_credentials_cannot_silently_change_user_or_port(self):
        with tempfile.TemporaryDirectory() as directory:
            known = Path(directory) / 'known_hosts'
            known.write_text('')
            vault = MagicMock()
            vault.reference.return_value = {'password': 'synthetic', 'user': 'another', 'port': 22}
            factory = SSHFactory({'node': {'host': '192.0.2.1', 'user': 'root', 'port': 22,
                                          'known_hosts': str(known), 'credential': {}}}, vault)
            factory.connections['node']['credential'] = {'path': 'synthetic'}
            with patch('paramiko.SSHClient') as connect:
                with self.assertRaisesRegex(OpsError, 'ssh_credential_identity_mismatch'):
                    factory.connect('node')
                connect.return_value.connect.assert_not_called()


if __name__ == '__main__':
    unittest.main()

import datetime as dt
from pathlib import Path
from queue import Queue
import ssl
import tempfile
import threading
import unittest

from cryptography import x509
from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import rsa

from scripts.ops.shadow_relay import allowed
from scripts.ops.tls_pipe import TLSPipe


class ShadowRelayTests(unittest.TestCase):
    def test_scope_rejects_other_paths_and_unsound_status_payloads(self):
        suffix = '?_cpa_ops_nonce=' + '0' * 32
        self.assertTrue(allowed('/api/status' + suffix, None))
        self.assertTrue(allowed('/api/channel/280' + suffix, None))
        self.assertTrue(allowed('/api/channel/status/batch' + suffix, {'ids': [280], 'status': 2}))
        for path, body in [('/v1/chat/completions' + suffix, {}), ('/api/channel/status/batch' + suffix, None),
                           ('/api/channel/status/batch' + suffix, {'ids': [280, 280], 'status': 1}),
                           ('/api/channel/status/batch' + suffix, {'ids': [True], 'status': 2}),
                           ('/api/channel/status/batch' + suffix, {'ids': [280], 'status': 3}),
                           ('/api/channel/280/key' + suffix, None), ('/api/status' + suffix + '&other=1', None)]:
            self.assertFalse(allowed(path, body))

    def test_tls_pipe_authenticates_host_and_carries_payload_in_encrypted_records(self):
        key = rsa.generate_private_key(public_exponent=65537, key_size=2048)
        name = x509.Name([x509.NameAttribute(x509.NameOID.COMMON_NAME, 'fixture.invalid')])
        now = dt.datetime.now(dt.timezone.utc)
        cert = (x509.CertificateBuilder().subject_name(name).issuer_name(name).public_key(key.public_key())
                .serial_number(x509.random_serial_number()).not_valid_before(now - dt.timedelta(minutes=1))
                .not_valid_after(now + dt.timedelta(minutes=5))
                .add_extension(x509.SubjectAlternativeName([x509.DNSName('fixture.invalid')]), critical=False)
                .sign(key, hashes.SHA256()))
        with tempfile.TemporaryDirectory() as work:
            certfile, keyfile = Path(work) / 'test.pem', Path(work) / 'test-key.pem'
            certfile.write_bytes(cert.public_bytes(serialization.Encoding.PEM))
            keyfile.write_bytes(key.private_bytes(serialization.Encoding.PEM, serialization.PrivateFormat.PKCS8,
                                                  serialization.NoEncryption()))
            server = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
            server.load_cert_chain(certfile, keyfile)
            client = ssl.create_default_context(cafile=str(certfile))
            left, right, ciphertext, received = Queue(), Queue(), [], []

            def encrypted_write(data):
                ciphertext.append(data)
                left.put(data)

            def serve():
                pipe = TLSPipe(lambda _: left.get(timeout=5), right.put, server, server_side=True)
                received.append(pipe.makefile('rb').readline())
                pipe.sendall(b'ok\n')

            worker = threading.Thread(target=serve)
            worker.start()
            pipe = TLSPipe(lambda _: right.get(timeout=5), encrypted_write, client, hostname='fixture.invalid')
            pipe.sendall(b'synthetic-management-authorization\n')
            self.assertEqual(pipe.makefile('rb').readline(), b'ok\n')
            worker.join(timeout=5)
            self.assertFalse(worker.is_alive())
            self.assertEqual(received, [b'synthetic-management-authorization\n'])
            self.assertNotIn(b'synthetic-management-authorization', b''.join(ciphertext))


if __name__ == '__main__':
    unittest.main()

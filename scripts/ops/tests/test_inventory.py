import copy
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from scripts.ops.core import OpsError
from scripts.ops.inventory import install, validate
from scripts.ops.security import Vault


class InventoryTests(unittest.TestCase):
    def setUp(self):
        self.data = json.loads((Path(__file__).parents[1] / 'inventory.example.json').read_text(encoding='utf-8'))

    def test_shipped_inventory_retires_ab_and_preserves_bindings(self):
        validate(self.data)
        self.assertEqual(self.data['targets']['p1']['branch'], 'main')
        self.assertEqual(self.data['targets']['c05']['branch'], 'dev-c05')
        self.assertTrue(self.data['targets']['A']['retired'])
        self.assertTrue(self.data['targets']['B']['retired'])

    def test_target_cannot_point_ssh_at_a_retired_or_different_host(self):
        self.data['targets']['nc73']['ssh'] = 'newapi-vip-a'
        with self.assertRaisesRegex(OpsError, 'vps_ssh_binding_mismatch'):
            validate(self.data)

    def test_missing_main_or_vip_is_not_an_empty_routing_inventory(self):
        del self.data['sites']['vip']
        with self.assertRaisesRegex(OpsError, 'required_newapi_sites_missing'):
            validate(self.data)

    def test_init_normalizes_encrypted_references_without_network_or_plaintext(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            known = root / 'known_hosts'
            known.write_text('')
            vault = Vault(lambda value, decrypt: value[::-1])
            for name, conn in self.data['connections'].items():
                conn['known_hosts'] = str(known)
                path = root / (name + '.dpapi')
                path.write_bytes(vault.seal({'host': conn['host'], 'user': 'root', 'port': 22, 'password': 'synthetic-secret'}))
                conn['credential']['path'] = str(path)
            for name, site in self.data['sites'].items():
                path = root / (name + '.dpapi')
                path.write_bytes(vault.seal({'origin': site['origin'], 'token': 'synthetic-token'}))
                site['credential']['path'] = str(path)
            with patch('socket.create_connection', side_effect=AssertionError('No network during init')):
                path = install(self.data, root / 'installed', vault)
            text = path.read_text(encoding='utf-8')
            self.assertNotIn('synthetic-secret', text)
            self.assertNotIn('synthetic-token', text)
            stored = json.loads(text)
            ref = stored['connections']['nc73']['credential']
            self.assertEqual(vault.reference(ref, host='152.53.208.73')['password'], 'synthetic-secret')
            with self.assertRaisesRegex(OpsError, 'inventory_already_exists'):
                install(self.data, root / 'installed', vault)


if __name__ == '__main__':
    unittest.main()

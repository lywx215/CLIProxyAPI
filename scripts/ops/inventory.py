"""Local inventory validation and DPAPI reference installation. Never contacts hosts."""
from __future__ import annotations

import copy
import json
import os
from pathlib import Path

from .core import RETIRED_HOSTS, digest, require, validate_selection
from .security import Vault


def validate(inventory):
    require(inventory.get('schema') == 1, 'inventory_schema_unsupported')
    connections = inventory.get('connections', {})
    for label, target in inventory['targets'].items():
        if target.get('retired'):
            continue
        validate_selection(inventory, [label])
        if target['kind'] == 'vps':
            profile = connections.get(target['ssh'], {})
            require(profile.get('host') == target['host'], 'vps_ssh_binding_mismatch')
            require(profile.get('host') not in RETIRED_HOSTS, 'retired_target')
    for site in inventory.get('sites', {}).values():
        require(site['origin'] in {'https://gemini30.pro', 'https://vip.gemini30.pro'}, 'newapi_origin_changed')
        require(site.get('entry_addresses') and site.get('observers'), 'newapi_topology_missing')
        require(site.get('propagation') in {'upstream', 'app'}, 'unknown_propagation_strategy')
        require(set(site['observers']).issubset(connections), 'newapi_ssh_profile_missing')
        for name, probe in site['observers'].items():
            if probe.get('shadow_admin_relay'):
                require(site['origin'] == 'https://gemini30.pro' and site['propagation'] == 'upstream'
                        and connections[name]['host'] == '43.162.127.208'
                        and '127.0.0.1:3003' in probe.get('extra_active_backends', [])
                        and 'newapi-slave1-shadow' in probe.get('apps', []), 'shadow_relay_binding_rejected')
    require(set(inventory.get('sites', {})) == {'main', 'vip'}, 'required_newapi_sites_missing')


def install(inventory, root, vault=None):
    """Normalize existing encrypted references into this controller's stable custody."""
    validate(inventory)
    vault = vault or Vault()
    root = Path(root).resolve()
    require(not (root / 'inventory.json').exists(), 'inventory_already_exists_edit_explicitly')
    result = copy.deepcopy(inventory)
    pending = {}
    for name, connection in result['connections'].items():
        require(Path(connection['known_hosts']).expanduser().is_file(), 'known_hosts_missing')
        if not connection.get('credential'):
            require(connection.get('identity') or connection.get('agent'), 'ssh_authentication_reference_missing')
            continue
        host = connection['host']
        record = vault.reference(connection['credential'], host=host)
        if isinstance(record, dict):
            require(record.get('user', connection['user']) == connection['user']
                    and record.get('port', connection['port']) == connection['port'], 'ssh_credential_identity_mismatch')
        password = record if isinstance(record, str) else record['password']
        require(isinstance(password, str) and password, 'empty_ssh_credential')
        record = {'host': host, 'user': connection['user'], 'port': connection['port'], 'password': password}
        path = root / 'credentials' / ('ssh-' + digest([host, connection['port'], connection['user']])[:16] + '.dpapi')
        pending[path] = record
        connection['credential'] = {'path': str(path), 'host': host, 'checks': {'host': host}}
    for name, site in result['sites'].items():
        record = vault.reference(site['credential'], origin=site['origin'])
        token = record if isinstance(record, str) else record[site.get('token_field', 'token')]
        require(isinstance(token, str) and token, 'empty_newapi_credential')
        path = root / 'credentials' / ('newapi-' + name + '.dpapi')
        pending[path] = {'origin': site['origin'], 'token': token}
        site['credential'] = {'path': str(path), 'origin': site['origin'], 'checks': {'origin': site['origin']}}
        site['token_field'] = 'token'
    root.mkdir(parents=True, exist_ok=True)
    for path, record in pending.items():
        path.parent.mkdir(parents=True, exist_ok=True)
        if path.exists():
            require(vault.open(path.read_bytes()) == record, 'existing_encrypted_reference_differs')
        else:
            with path.open('xb') as stream:
                stream.write(vault.seal(record))
                stream.flush()
                os.fsync(stream.fileno())
    with (root / 'inventory.json').open('x', encoding='utf-8') as stream:
        json.dump(result, stream, ensure_ascii=False, indent=2)
        stream.flush()
        os.fsync(stream.fileno())
    return root / 'inventory.json'

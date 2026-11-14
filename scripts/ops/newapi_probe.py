"""Read-only topology probe, sent over SSH; never changes New API or Nginx."""
import json
import hashlib
import pathlib
import re
import subprocess
import sys


def run(arguments):
    result = subprocess.run(arguments, capture_output=True, text=True)
    if result.returncode:
        raise RuntimeError('topology_command_failed')
    return result.stdout


def main(spec):
    nginx = run([spec['nginx'], '-T'])
    if spec['hostname'] not in nginx:
        raise RuntimeError('nginx_origin_not_found')
    ids = run(['docker', 'ps', '-aq']).split()
    objects = json.loads(run(['docker', 'inspect', *ids])) if ids else []
    apps = []
    shadow = None
    for item in objects:
        name = item['Name'].lstrip('/')
        if name not in spec['apps']:
            continue
        apps.append({'name': name, 'id': item['Id'], 'running': item['State']['Running'],
                     'started_at': item['State']['StartedAt'], 'restart_count': item['RestartCount'],
                     'ips': sorted(network['IPAddress'] for network in item['NetworkSettings']['Networks'].values()
                                   if network.get('IPAddress'))})
        if spec.get('shadow_admin_relay') and name == 'newapi-slave1-shadow':
            ports = (item['HostConfig'].get('PortBindings') or {}).get('3000/tcp') or []
            if ports != [{'HostIp': '127.0.0.1', 'HostPort': '3003'}]:
                raise RuntimeError('shadow_loopback_binding_changed')
            env = dict(field.split('=', 1) for field in item['Config'].get('Env', []) if '=' in field)
            seconds = int(env.get('SYNC_FREQUENCY', '60'))
            if not 1 <= seconds <= 120:
                raise RuntimeError('shadow_sync_interval_unsupported')
            destinations = {ip + ':3000' for ip in apps[-1]['ips']}
            nat = run(['iptables-save', '-t', 'nat'])
            rules = [line for line in nat.splitlines() if line.startswith('-A PREROUTING ')
                     and '--dport 3000 ' in line and 'newapi-slave1-cutover' in line
                     and ' -j DNAT --to-destination ' in line]
            if len(rules) != 1 or rules[0].rsplit(' ', 1)[-1] not in destinations:
                raise RuntimeError('shadow_public_dnat_binding_changed')
            shadow = {'address': '127.0.0.1:3003', 'container': name, 'container_id': item['Id'],
                      'public_dnat_target': rules[0].rsplit(' ', 1)[-1], 'sync_seconds': seconds,
                      'nat_sha256': hashlib.sha256(rules[0].encode()).hexdigest()}
    if set(spec['apps']) != {item['name'] for item in apps} or not all(item['running'] for item in apps):
        raise RuntimeError('newapi_app_inventory_incomplete')
    backends = []
    for body in re.findall(r'upstream\s+' + re.escape(spec['upstream']) + r'\s*\{([^}]+)\}', nginx):
        for line in re.findall(r'(?m)^\s*server\s+([^;]+);', body):
            words = line.split()
            backends.append({'address': words[0], 'backup': 'backup' in words, 'down': 'down' in words})
    if not backends:
        # An explicit loopback proxy_pass is common on the VIP hosts.
        if spec.get('direct_upstream') and ('http://' + spec['direct_upstream']) in nginx:
            backends = [{'address': spec['direct_upstream'], 'backup': False, 'down': False}]
        else:
            raise RuntimeError('newapi_upstream_not_found')
    if spec.get('shadow_admin_relay') and not shadow:
        raise RuntimeError('shadow_container_missing')
    return {'apps': sorted(apps, key=lambda item: item['name']), 'backends': backends, 'shadow': shadow,
            'nginx_config_sha256': hashlib.sha256(nginx.encode()).hexdigest()}


if __name__ == '__main__':
    print(json.dumps(main(json.load(sys.stdin))))

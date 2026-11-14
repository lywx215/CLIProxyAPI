"""Target-only Docker operations. Executed over verified SSH with JSON on stdin."""
from __future__ import annotations

import hashlib
import json
import os
import platform
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tarfile


def check(condition, code):
    if not condition:
        raise RuntimeError(code)


def run(args, error='command_failed'):
    result = subprocess.run(args, capture_output=True, text=True)
    check(result.returncode == 0, error)
    return result.stdout


def sha(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def atomic(path, data, mode=0o600):
    temporary = path.with_name(path.name + '.ops-new')
    check(not temporary.is_symlink(), 'temporary_symlink')
    with temporary.open('wb') as stream:
        stream.write(data)
        stream.flush()
        os.fsync(stream.fileno())
    os.chmod(temporary, mode)
    os.replace(temporary, path)


def locate(spec):
    root = Path(spec['base']).resolve()
    check(str(root) in {'/opt/cliproxyapi', '/opt/cliproxyapi-netcup'}, 'invalid_root')
    check(re.fullmatch(r'[a-z][a-z0-9_-]{0,47}', spec['project']), 'invalid_project')
    ids = run(['docker', 'ps', '-aq', '--filter', 'label=com.docker.compose.project=' + spec['project'],
               '--filter', 'label=com.docker.compose.service=cli-proxy-api']).split()
    check(len(ids) == 1, 'container_ambiguous')
    obj = json.loads(run(['docker', 'inspect', ids[0]]))[0]
    files = obj['Config']['Labels'].get('com.docker.compose.project.config_files', '').split(',')
    check(len(files) == 1 and files[0], 'compose_file_ambiguous')
    compose = Path(files[0]).resolve()
    check(compose.is_relative_to(root) and compose.is_file(), 'compose_path_invalid')
    return root, obj, compose


def processes():
    ids = run(['docker', 'ps', '-aq']).split()
    objects = json.loads(run(['docker', 'inspect', *ids])) if ids else []
    return sorted([{'id': item['Id'], 'name': item['Name'].lstrip('/'), 'image': item['Image'],
                    'started_at': item['State']['StartedAt'], 'running': item['State']['Running'],
                    'restart_count': item['RestartCount']} for item in objects], key=lambda item: item['name'])


def normalized_env(value):
    return re.sub(r'^CPA_IMAGE=.*$', 'CPA_IMAGE=<runtime>', value, flags=re.M)


def snapshot(spec):
    root, obj, compose = locate(spec)
    env = dict(item.split('=', 1) for item in obj['Config']['Env'] if '=' in item)
    file = root / 'shared/initial-credentials.txt'
    values = dict(line.split(': ', 1) for line in file.read_text().splitlines() if ': ' in line) if file.is_file() else {}
    management = env.get('MANAGEMENT_PASSWORD') or values.get('Management key')
    check(management, 'management_key_unavailable')
    binary = run(['docker', 'exec', obj['Id'], 'sha256sum', '/CLIProxyAPI/CLIProxyAPI']).split()[0]
    page = run(['docker', 'exec', obj['Id'], 'sha256sum', '/CLIProxyAPI/static/management.html']).split()[0]
    protected = {}
    for path in [compose, root / 'current.env', root / 'shared/provider.env']:
        if path.is_file():
            content = path.read_text()
            protected[str(path.relative_to(root))] = {
                'sha256': hashlib.sha256((normalized_env(content) if path.name == 'current.env' else content).encode()).hexdigest(),
                'mode': path.stat().st_mode & 0o777}
    memory = {key: int(value.split()[0]) * 1024 for key, value in
              (line.split(':', 1) for line in Path('/proc/meminfo').read_text().splitlines())
              if key in {'MemTotal', 'MemAvailable'}}
    branch = run(['git', '-C', str(root), 'branch', '--show-current']).strip() if (root / '.git').exists() else None
    return {'container': obj, 'management_key': management, 'environment': env,
            'compose': str(compose), 'compose_sha256': sha(compose), 'protected_files': protected,
            'processes': processes(), 'branch': branch, 'binary_sha256': binary, 'disk_page_sha256': page,
            'resources': {'disk_free_bytes': shutil.disk_usage(root).free, 'memory': memory,
                          'cpu_count': os.cpu_count(), 'load_average': os.getloadavg(), 'architecture': platform.machine()},
            'current_env': (root / 'current.env').read_text()}


def operation_dir(root, spec):
    check(re.fullmatch(r'run-[a-f0-9]{16}', spec['run_id']), 'invalid_operation_id')
    directory = root / 'releases' / ('ops-' + spec['run_id'])
    check(not directory.is_symlink() and directory.resolve().is_relative_to(root / 'releases'), 'unsafe_operation_path')
    return directory


def stage(spec):
    root, obj, compose = locate(spec)
    check(obj['Id'] == spec['expected_container'], 'container_changed_before_stage')
    check(sha(compose) == spec['compose_sha256'], 'compose_changed')
    directory = operation_dir(root, spec)
    check(directory.is_dir(), 'staging_directory_missing')
    receipt = directory / 'stage.json'
    if receipt.exists():
        value = json.loads(receipt.read_text())
        check(value['package_sha256'] == spec['package_sha256'], 'stage_identity_changed')
        run(['docker', 'image', 'inspect', value['image_id']])
        return value
    package = directory / 'runtime.tar.gz'
    check(sha(package) == spec['package_sha256'], 'package_corrupt')
    with tarfile.open(package, 'r:gz') as archive:
        members = archive.getmembers()
        check(len(members) == 3 and {member.name for member in members} == {'CLIProxyAPI', 'management.html', 'Dockerfile'}
              and all(member.isfile() for member in members), 'unsafe_package_members')
        for member in members:
            dest = directory / member.name
            check(not dest.is_symlink(), 'unsafe_package_destination')
            atomic(dest, archive.extractfile(member).read(), 0o755 if member.name == 'CLIProxyAPI' else 0o600)
    check(sha(directory / 'CLIProxyAPI') == spec['binary_sha256']
          and sha(directory / 'management.html') == spec['page_sha256'], 'artifact_mismatch')
    base_image = json.loads(run(['docker', 'image', 'inspect', obj['Image']]))[0]
    base_layers = base_image['RootFS']['Layers']
    check(base_image['Id'] == obj['Image'] and base_layers, 'base_image_identity_changed')
    # BuildKit parses a bare image ID in FROM as a repository and tag.
    base_tag = 'cliproxyapi-ops-base:' + spec['run_id']
    run(['docker', 'tag', obj['Image'], base_tag], 'base_image_tag_failed')
    tag = 'cliproxyapi-ops:' + spec['run_id']
    run(['docker', 'build', '--pull=false', '--network=none', '--build-arg', 'BASE=' + base_tag,
         '-t', tag, str(directory)], 'image_assembly_failed')
    image = json.loads(run(['docker', 'image', 'inspect', tag]))[0]
    check(image['Config']['Labels']['org.opencontainers.image.revision'] == spec['commit'], 'image_revision_mismatch')
    check(image['RootFS']['Layers'][:len(base_layers)] == base_layers, 'staged_base_layers_mismatch')
    value = {'image_id': image['Id'], 'old_image': obj['Image'], 'old_container': obj['Id'],
             'package_sha256': spec['package_sha256'], 'commit': spec['commit'], 'directory': str(directory)}
    atomic(directory / 'current.env.before', (root / 'current.env').read_bytes())
    atomic(receipt, json.dumps(value).encode())
    return value


def apply(spec, rollback=False):
    root, obj, compose = locate(spec)
    directory = operation_dir(root, spec)
    value = json.loads((directory / 'stage.json').read_text())
    desired = value['old_image'] if rollback else value['image_id']
    check(obj['Image'] in {value['old_image'], value['image_id']}, 'unexpected_runtime_image')
    check(sha(compose) == spec['compose_sha256'], 'compose_changed')
    for name, expected in spec['protected_files'].items():
        path = (root / name).resolve()
        check(path.is_relative_to(root) and path.is_file(), 'protected_file_missing')
        content = path.read_text()
        current = hashlib.sha256((normalized_env(content) if path.name == 'current.env' else content).encode()).hexdigest()
        check(current == expected['sha256'] and path.stat().st_mode & 0o777 == expected['mode'], 'protected_file_changed')
    current = root / 'current.env'
    text = current.read_text()
    check(len(re.findall(r'^CPA_IMAGE=.*$', text, re.M)) == 1, 'image_variable_ambiguous')
    original = (directory / 'current.env.before').read_text()
    check(normalized_env(text) == normalized_env(original), 'environment_changed')
    run(['docker', 'image', 'inspect', desired])
    if obj['Image'] == desired and obj['State']['Running']:
        # Reconcile a completed mutation whose acknowledgement was lost.
        if rollback and text == original:
            return {'image_id': desired, 'container_id': obj['Id'], 'already_applied': True}
        check(re.search(r'^CPA_IMAGE=' + re.escape(desired) + r'$', text, re.M), 'running_image_env_mismatch')
        return {'image_id': desired, 'container_id': obj['Id'], 'already_applied': True}
    atomic(current, re.sub(r'^CPA_IMAGE=.*$', 'CPA_IMAGE=' + desired, text, flags=re.M).encode(),
           current.stat().st_mode & 0o777)
    run(['docker', 'compose', '--project-name', spec['project'], '--env-file', str(current), '-f', str(compose),
         'up', '-d', '--no-deps', '--force-recreate', 'cli-proxy-api'])
    _, after, _ = locate(spec)
    check(after['Image'] == desired, 'container_image_mismatch')
    result = {'image_id': desired, 'container_id': after['Id'], 'already_applied': False}
    atomic(directory / ('rollback.json' if rollback else 'deploy.json'), json.dumps(result).encode())
    return result


def main(spec):
    action = spec.get('action')
    if action == 'runtime':
        root, obj, _ = locate(spec)
        return {'id': obj['Id'], 'image': obj['Image'], 'running': obj['State']['Running'],
                'status': obj['State'].get('Status'), 'exit_code': obj['State'].get('ExitCode')}
    if action == 'snapshot':
        return snapshot(spec)
    if action == 'stage':
        return stage(spec)
    if action in {'deploy', 'rollback'}:
        return apply(spec, action == 'rollback')
    raise RuntimeError('remote_action_rejected')


if __name__ == '__main__':
    try:
        print(json.dumps(main(json.load(sys.stdin))))
    except Exception as error:
        print(json.dumps({'error': str(error) if isinstance(error, RuntimeError) else type(error).__name__}))
        sys.exit(1)

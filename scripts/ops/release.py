"""Build and qualify immutable Git commits, independent of dirty working files."""
from __future__ import annotations

import datetime as dt
import http.client
import json
import os
from pathlib import Path
import re
import shutil
import socket
import subprocess
import tarfile
import time
import uuid

from .core import OpsError, digest, require, sha_file
from .telemetry import operation, note


def git(repository, *arguments):
    result = subprocess.run(['git', '-C', str(repository), *arguments], capture_output=True)
    require(result.returncode == 0, 'git_operation_failed')
    return result.stdout


def describe(repository, commit):
    require(re.fullmatch(r'[0-9a-f]{40}', commit or ''), 'full_commit_required')
    actual = git(repository, 'rev-parse', commit + '^{commit}').decode().strip()
    require(actual == commit, 'commit_identity_mismatch')
    page = git(repository, 'show', commit + ':static/management.html')
    require(b'<html' in page.lower() and len(page) > 1024, 'embedded_management_page_missing')
    return {'commit': commit, 'tree': git(repository, 'rev-parse', commit + '^{tree}').decode().strip(),
            'page_sha256': digest(page), 'page_bytes': len(page)}


def environment(directory):
    env = {key: value for key, value in os.environ.items()
           if not re.search(r'(?i)(secret|token|password|cookie|api_key|credential|cliproxy|gemini_|objectstore|pgstore|gitstore)', key)}
    env.update(CGO_ENABLED='0', GOCACHE=str(directory / 'go-cache'), GOTMPDIR=str(directory / 'go-tmp'))
    for name in ['go-cache', 'go-tmp']:
        (directory / name).mkdir(parents=True, exist_ok=True)
    return env


def execute(arguments, directory, env, logfile):
    started = time.monotonic()
    with logfile.open('wb') as stream:
        result = subprocess.run(arguments, cwd=directory, env=env, stdout=stream, stderr=subprocess.STDOUT,
                                creationflags=getattr(subprocess, 'CREATE_NO_WINDOW', 0))
    require(result.returncode == 0, 'release_qualification_failed')
    return {'command': arguments, 'exit': 0, 'seconds': round(time.monotonic() - started, 3)}


def loopback_smoke(executable, directory, env, expected_page):
    """Empty account store, no upstreams, pinned local panel; no live model calls."""
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0))
        port = sock.getsockname()[1]
    key = 'local-smoke-' + uuid.uuid4().hex
    config = {'config-version': 8, 'server': {'host': '127.0.0.1', 'port': port},
              'management': {'allow-remote': False, 'secret-key': key, 'disable-auto-update-panel': True},
              'access': {'api-keys': [key]}, 'oauth': {'auth-dir': str(directory / 'empty-auths')},
              'models': {'catalog': '', 'codex-catalog': '', 'devin-catalog': ''}}
    import yaml
    path = directory / 'smoke-config.yaml'
    path.write_text(yaml.safe_dump(config), encoding='utf-8')
    isolated_env = dict(env, MANAGEMENT_STATIC_PATH=str(directory / 'static/management.html'))
    log = (directory / 'smoke.log').open('wb')
    process = subprocess.Popen([str(executable), '--config', str(path), '--local-model', '--no-browser'],
                               cwd=directory, env=isolated_env, stdout=log, stderr=subprocess.STDOUT,
                               creationflags=getattr(subprocess, 'CREATE_NO_WINDOW', 0))
    try:
        deadline = time.monotonic() + 45
        while True:
            require(process.poll() is None, 'isolated_server_failed')
            try:
                conn = http.client.HTTPConnection('127.0.0.1', port, timeout=2)
                conn.request('GET', '/healthz')
                response = conn.getresponse()
                response.read()
                conn.close()
                if response.status == 200:
                    break
            except OSError:
                pass
            require(time.monotonic() < deadline, 'isolated_server_not_ready')
            time.sleep(.2)
        checks = []
        for endpoint in ['/healthz', '/management.html', '/v8/management/config', '/v1/models']:
            conn = http.client.HTTPConnection('127.0.0.1', port, timeout=5)
            try:
                conn.request('GET', endpoint, headers={'Authorization': 'Bearer ' + key})
                response = conn.getresponse()
                data = response.read()
                require(response.status == 200, 'isolated_http_check_failed')
                if endpoint == '/management.html':
                    require(digest(data) == expected_page, 'isolated_panel_mismatch')
                if endpoint == '/v1/models':
                    require(json.loads(data).get('data') == [], 'isolated_model_store_not_empty')
                checks.append({'path': endpoint, 'http': 200})
            finally:
                conn.close()
        return checks
    finally:
        process.terminate()
        try:
            process.wait(timeout=10)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait()
        log.close()


class Releases:
    def __init__(self, repository, root, go=None, emit=lambda *args, **kwargs: None):
        self.repository, self.root = Path(repository), Path(root)
        self.go, self.emit = go or shutil.which('go'), emit

    def prepare(self, release):
        with operation('release_qualification', kind='build'):
            return self._prepare(release)

    def _prepare(self, release):
        require(self.go, 'go_toolchain_missing')
        require(describe(self.repository, release['commit']) == release, 'source_release_changed')
        version = subprocess.check_output([self.go, 'version'], text=True).strip()
        identity = digest({'release': release, 'go': version, 'builder': sha_file(__file__)})
        directory = self.root / identity
        receipt = directory / 'release.json'
        if receipt.is_file():
            data = json.loads(receipt.read_text(encoding='utf-8'))
            require(data['identity'] == identity and sha_file(data['package']) == data['package_sha256']
                    and sha_file(data['binary']) == data['binary_sha256'], 'cached_release_corrupt')
            note('cache_hit', scope='release')
            return data
        directory.mkdir(parents=True, exist_ok=True)
        source = directory / ('source-' + uuid.uuid4().hex[:8])
        source.mkdir()
        archive = directory / 'source.tar'
        git(self.repository, 'archive', '--format=tar', '--output=' + str(archive), release['commit'])
        with tarfile.open(archive, 'r:') as package:
            for member in package.getmembers():
                path = (source / member.name).resolve()
                require(path.is_relative_to(source.resolve()) and not member.issym() and not member.islnk(), 'unsafe_source_archive')
                if member.isdir():
                    path.mkdir(parents=True, exist_ok=True)
                elif member.isfile():
                    path.parent.mkdir(parents=True, exist_ok=True)
                    path.write_bytes(package.extractfile(member).read())
        env = environment(directory)
        self.emit('release_qualification_started', commit=release['commit'])
        checks = [execute([self.go, 'test', '-count=1', '-p', '4', './...'], source, env, directory / 'go-test.log')]
        executable = source / ('smoke-server.exe' if os.name == 'nt' else 'smoke-server')
        checks.append(execute([self.go, 'build', '-o', str(executable), './cmd/server'], source, env, directory / 'go-build.log'))
        http_checks = loopback_smoke(executable, source, env, release['page_sha256'])
        runtime = directory / 'runtime'
        runtime.mkdir(exist_ok=True)
        binary = runtime / 'CLIProxyAPI'
        build_date = dt.datetime.now(dt.timezone.utc).isoformat()
        flags = '-s -w -X main.Version=ops-' + release['commit'][:12] + ' -X main.Commit=' + release['commit'] + ' -X main.BuildDate=' + build_date
        checks.append(execute([self.go, 'build', '-buildvcs=false', '-ldflags', flags, '-o', str(binary), './cmd/server'],
                              source, dict(env, GOOS='linux', GOARCH='amd64'), directory / 'linux-build.log'))
        require(binary.read_bytes()[:4] == b'\x7fELF', 'linux_binary_invalid')
        page = source / 'static/management.html'
        require(sha_file(page) == release['page_sha256'], 'release_page_changed')
        shutil.copyfile(page, runtime / 'management.html')
        (runtime / 'Dockerfile').write_text('ARG BASE\nFROM ${BASE}\nLABEL org.opencontainers.image.revision="' + release['commit']
                                            + '"\nCOPY CLIProxyAPI /CLIProxyAPI/CLIProxyAPI\nCOPY management.html /CLIProxyAPI/static/management.html\n', encoding='utf-8')
        output = directory / 'runtime.tar.gz'
        with tarfile.open(output, 'w:gz') as package:
            for name in ['CLIProxyAPI', 'management.html', 'Dockerfile']:
                package.add(runtime / name, arcname=name)
        data = {**release, 'identity': identity, 'toolchain': version, 'binary': str(binary),
                'binary_sha256': sha_file(binary), 'package': str(output), 'package_sha256': sha_file(output),
                'package_bytes': output.stat().st_size, 'qualification': checks, 'isolated_http_checks': http_checks,
                'built_at': build_date, 'frontend_validation': 'embedded Git artifact hash and isolated served page'}
        temporary = receipt.with_suffix('.new')
        temporary.write_text(json.dumps(data, indent=2), encoding='utf-8')
        temporary.replace(receipt)
        return data

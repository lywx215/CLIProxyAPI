#!/usr/bin/env python3
"""Deploy a local working-tree snapshot to an existing Linux Docker host."""

from __future__ import annotations

import argparse
import base64
import getpass
import hashlib
import io
import json
import os
from pathlib import Path, PurePosixPath
import re
import shlex
import stat
import subprocess
import sys
import tarfile
import tempfile
from datetime import datetime, timezone


REPOSITORY = Path(__file__).resolve().parents[2]
PROFILE = REPOSITORY / '.codex' / 'vps-deploy.json'
EXCLUDED = {
    '.git', '.codex', '.claude', '.gemini', '.ssh', '.venv', 'venv',
    'node_modules', '__pycache__', 'auths', 'logs', 'plugins', 'temp',
    'pgstore', 'gitstore', 'objectstore', 'conv', 'bin', 'refs', 'sshcli',
    '.worktrees', '.codex-worktrees', 'zeaburcli',
}


def git(root: Path, *arguments: str) -> bytes:
    return subprocess.check_output(['git', '-C', str(root), *arguments], stderr=subprocess.PIPE)


def allowed_file(name: str) -> bool:
    path = PurePosixPath(name)
    if path.is_absolute() or '..' in path.parts or not path.parts:
        return False
    if any(part in EXCLUDED for part in path.parts):
        return False
    if path.name.startswith('.env') or path.suffix.lower() in {'.pem', '.key', '.pfx', '.p12'}:
        return False
    if path.name.startswith('config') and path.suffix in {'.yaml', '.yml'}:
        return name == 'config.example.yaml'
    return name not in {'.build-info', 'deploy-manifest.json'}


def make_snapshot(root: Path, output: Path) -> dict:
    names = {os.fsdecode(name).replace('\\', '/') for name in
             git(root, 'ls-files', '-z', '--cached', '--others', '--exclude-standard').split(b'\0') if name}
    # The management page can be ignored locally, but is required by our Dockerfile.
    static_root = root / 'static'
    if static_root.is_dir():
        names.update(path.relative_to(root).as_posix() for path in static_root.rglob('*') if path.is_file())
    names = {name for name in names if allowed_file(name)}
    required = {'Dockerfile', 'go.mod', 'go.sum', 'config.example.yaml', 'static/management.html'}
    if not required.issubset(names) or any(not (root / name).is_file() for name in required):
        raise ValueError('Required source or static/management.html is missing; build the management page first.')
    commit = git(root, 'rev-parse', 'HEAD').decode().strip()
    branch = git(root, 'branch', '--show-current').decode().strip()
    dirty = bool(git(root, 'status', '--porcelain', '-z'))
    digest = hashlib.sha256()
    count = 0
    created = datetime.now(timezone.utc)
    output.parent.mkdir(parents=True, exist_ok=True)
    with tarfile.open(output, 'w:gz') as archive:
        for name in sorted(names):
            path = root / name
            if path.is_symlink() or not path.resolve().is_relative_to(root.resolve()):
                raise ValueError(f'Symlinks are not supported in deployment snapshots: {name}')
            if not path.exists():
                continue  # Respect deletions in the working tree.
            if not path.is_file():
                raise ValueError(f'Unsupported source entry (possibly a submodule): {name}')
            # Archive and hash the same bytes even if another editor changes the file.
            data = path.read_bytes()
            mode = 0o755 if path.stat().st_mode & stat.S_IXUSR else 0o644
            digest.update(name.encode('utf-8') + b'\0' + str(mode).encode() + b'\0')
            digest.update(hashlib.sha256(data).digest())
            entry = tarfile.TarInfo(name)
            entry.size, entry.mode, entry.mtime = len(data), mode, 0
            archive.addfile(entry, io.BytesIO(data))
            count += 1
        snapshot_hash = digest.hexdigest()
        manifest = {
            'schema_version': 1, 'commit': commit, 'branch': branch, 'dirty': dirty,
            'snapshot_sha256': snapshot_hash, 'files': count,
            'created_at': created.strftime('%Y-%m-%dT%H:%M:%SZ'),
            'release': created.strftime('%Y%m%dT%H%M%S%fZ') + '-' + snapshot_hash[:12],
            'version': 'local-' + commit[:12] + ('-dirty' if dirty else '') + '-' + snapshot_hash[:12],
        }
        data = (json.dumps(manifest, indent=2) + '\n').encode()
        entry = tarfile.TarInfo('deploy-manifest.json')
        entry.size, entry.mode = len(data), 0o644
        archive.addfile(entry, io.BytesIO(data))
    return manifest


def validate_arguments(args: argparse.Namespace) -> None:
    root = PurePosixPath(args.directory)
    if (not re.fullmatch(r'/[A-Za-z0-9_./-]+', args.directory) or len(root.parts) < 3
            or '..' in root.parts):
        raise ValueError('Deployment directory must be a dedicated absolute Linux path, e.g. /opt/cliproxyapi.')
    if not re.fullmatch(r'[a-z][a-z0-9_-]{0,47}', args.project):
        raise ValueError('Project must start with a lowercase letter and use letters, digits, - or _.')
    if not 1 <= args.port <= 65535 or not 1 <= args.ssh_port <= 65535:
        raise ValueError('Ports must be between 1 and 65535.')
    if not 1 <= args.lines <= 100000:
        raise ValueError('Log lines must be between 1 and 100000.')
    if args.action != 'package' and not args.host:
        raise ValueError('Specify --host or create .codex/vps-deploy.json (see scripts/vps/README.md).')
    if getattr(args, 'domain', None) and not re.fullmatch(r'[a-z0-9](?:[a-z0-9.-]{0,251}[a-z0-9])?', args.domain):
        raise ValueError('Domain must be a lowercase DNS hostname.')


class SSH:
    def __init__(self, args: argparse.Namespace):
        try:
            import paramiko
        except ImportError as exc:
            raise ValueError('Install the SSH dependency: python -m pip install -r scripts/vps/requirements.txt') from exc
        self.client = paramiko.SSHClient()
        known_hosts = Path.home() / '.ssh' / 'known_hosts'
        known_hosts.parent.mkdir(parents=True, exist_ok=True)
        if not known_hosts.exists():
            known_hosts.touch(mode=0o600)
        self.client.load_host_keys(str(known_hosts))
        if args.accept_new_host_key:
            class AcceptNew(paramiko.MissingHostKeyPolicy):
                def missing_host_key(self, client, hostname, key):
                    fingerprint = base64.b64encode(hashlib.sha256(key.asbytes()).digest()).decode().rstrip('=')
                    print(f'New SSH host key for {hostname}: SHA256:{fingerprint}', flush=True)
                    client.get_host_keys().add(hostname, key.get_name(), key)
                    client.save_host_keys(str(known_hosts))
            self.client.set_missing_host_key_policy(AcceptNew())
        password = None
        if args.password_stdin:
            print('Reading SSH password from standard input (not echoed).', flush=True)
            password = sys.stdin.readline().rstrip('\r\n')
        elif args.password:
            password = getpass.getpass('SSH password: ')
        if (args.password_stdin or args.password) and not password:
            self.client.close()
            raise ValueError('No SSH password was supplied.')
        try:
            self.client.connect(
                args.host, port=args.ssh_port, username=args.user, password=password,
                key_filename=args.identity, allow_agent=password is None, look_for_keys=password is None,
                timeout=20, banner_timeout=20, auth_timeout=30,
            )
            # Do not impose operation timeouts after the SSH connection is established.
            self.client.get_transport().sock.settimeout(None)
        except Exception:
            self.client.close()
            raise
        finally:
            password = None

    def run(self, arguments: list[str], capture: bool = False) -> str:
        channel = self.client.get_transport().open_session()
        channel.set_combine_stderr(True)
        channel.exec_command(shlex.join(arguments))
        chunks = []
        with channel.makefile('rb') as stream:
            for line in stream:
                text = line.decode('utf-8', errors='replace')
                if capture:
                    chunks.append(text)
                else:
                    print(text, end='', flush=True)
        result = channel.recv_exit_status()
        channel.close()
        if result:
            raise RuntimeError(f'Remote operation failed (exit {result}).' + (''.join(chunks) if capture else ''))
        return ''.join(chunks).strip()

    def close(self):
        self.client.close()


def run(args: argparse.Namespace) -> None:
    validate_arguments(args)
    with tempfile.TemporaryDirectory(prefix='cliproxyapi-vps-') as temporary:
        local = Path(temporary)
        bundle = local / 'source.tar.gz'
        manifest = None
        if args.action in {'deploy', 'update', 'package'}:
            destination = Path(args.output).resolve() if args.action == 'package' and args.output else bundle
            if destination.is_relative_to(REPOSITORY) and '.codex' not in destination.relative_to(REPOSITORY).parts:
                raise ValueError('Put snapshot archives outside the repository or under .codex/.')
            manifest = make_snapshot(REPOSITORY, destination)
            print(json.dumps(manifest, indent=2), flush=True)
            if args.action == 'package':
                if not args.output:
                    destination = REPOSITORY / '.codex' / 'vps-source.tar.gz'
                    destination.parent.mkdir(parents=True, exist_ok=True)
                    import shutil
                    shutil.copyfile(bundle, destination)
                print(f'Snapshot: {destination}')
                return
            archive_hash = hashlib.sha256(bundle.read_bytes()).hexdigest()
            (local / 'source.sha256').write_text(archive_hash + '  source.tar.gz\n', encoding='ascii', newline='\n')
            (local / 'release.env').write_text(''.join(f'{key}={shlex.quote(value)}\n' for key, value in {
                'RELEASE': manifest['release'], 'COMMIT': manifest['commit'],
                'VERSION': manifest['version'], 'BUILD_DATE': manifest['created_at'],
                'SNAPSHOT': manifest['snapshot_sha256'],
            }.items()), encoding='ascii', newline='\n')
        connection = SSH(args)
        upload = None
        uploaded = []
        try:
            upload = connection.run(['mktemp', '-d', '/tmp/cliproxyapi-upload.XXXXXXXX'], capture=True)
            if not re.fullmatch(r'/tmp/cliproxyapi-upload\.[A-Za-z0-9]+', upload):
                raise ValueError('Unexpected SSH temporary directory response.')
            with connection.client.open_sftp() as sftp:
                files = []
                for name in ['manage.sh', 'proxy.sh']:
                    script = local / name
                    script.write_bytes(Path(__file__).with_name(name).read_bytes().replace(b'\r\n', b'\n'))
                    files.append(script)
                if manifest:
                    files += [bundle, local / 'source.sha256', local / 'release.env']
                for path in files:
                    remote = upload + '/' + path.name
                    sftp.put(str(path), remote)
                    sftp.chmod(remote, 0o600)
                    uploaded.append(remote)
            command = ['bash', upload + '/manage.sh', args.action, args.directory,
                       args.project, str(args.port), upload, str(args.lines), '1' if args.follow else '0', args.domain or '']
            if args.sudo:
                command = ['sudo', '-n', '--', *command]
            connection.run(command)
        finally:
            if upload and re.fullmatch(r'/tmp/cliproxyapi-upload\.[A-Za-z0-9]+', upload):
                try:
                    with connection.client.open_sftp() as sftp:
                        for remote in uploaded:
                            sftp.remove(remote)
                        sftp.rmdir(upload)
                except Exception:
                    print('Temporary upload cleanup failed; remove the reported upload directory on the server.', file=sys.stderr)
            connection.close()


def main() -> int:
    initial = argparse.ArgumentParser(add_help=False)
    initial.add_argument('--profile', default=str(PROFILE))
    selected, _ = initial.parse_known_args()
    defaults = json.loads(Path(selected.profile).read_text(encoding='utf-8-sig')) if Path(selected.profile).exists() else {}
    fields = {'host', 'user', 'ssh_port', 'directory', 'project', 'port', 'identity', 'sudo', 'domain'}
    if defaults.keys() - fields:
        raise ValueError('Profile contains unsupported fields. Do not store passwords in profiles.')
    parser = argparse.ArgumentParser(description=__doc__, parents=[initial])
    parser.add_argument('action', choices=['deploy', 'update', 'restart', 'logs', 'status', 'probe', 'package'])
    parser.add_argument('--host', default=defaults.get('host'))
    parser.add_argument('--user', default=defaults.get('user', 'root'))
    parser.add_argument('--ssh-port', type=int, default=defaults.get('ssh_port', 22))
    parser.add_argument('--directory', default=defaults.get('directory', '/opt/cliproxyapi'))
    parser.add_argument('--project', default=defaults.get('project', 'cliproxyapi-vps'))
    parser.add_argument('--domain', default=defaults.get('domain'), help='Configure HTTPS through an existing BaoTa Nginx installation.')
    parser.add_argument('--port', type=int, default=defaults.get('port', 8317))
    parser.add_argument('--identity', default=defaults.get('identity'))
    authentication = parser.add_mutually_exclusive_group()
    authentication.add_argument('--password', action='store_true', help='Prompt for the SSH password without echo.')
    authentication.add_argument('--password-stdin', action='store_true', help=argparse.SUPPRESS)
    parser.add_argument('--accept-new-host-key', action='store_true', help='Trust and save a new SSH host key; changed keys are rejected.')
    parser.add_argument('--sudo', action='store_true', default=defaults.get('sudo', False), help='Use passwordless sudo for server operations.')
    parser.add_argument('--lines', type=int, default=100)
    parser.add_argument('--follow', action='store_true')
    parser.add_argument('--output', help='Destination archive for the offline package action.')
    run(parser.parse_args())
    return 0


if __name__ == '__main__':
    try:
        sys.exit(main())
    except KeyboardInterrupt:
        print('\nInterrupted.', file=sys.stderr)
        sys.exit(130)
    except Exception as error:
        print(f'Error: {error}', file=sys.stderr)
        sys.exit(1)

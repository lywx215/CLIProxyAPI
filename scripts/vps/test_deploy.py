"""Offline regression tests for snapshot contents and remote update safety."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import tarfile
import tempfile
import unittest

import deploy


class SnapshotTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix='vps-snapshot-test-')
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name) / 'repo'
        self.root.mkdir()
        self.write('.gitignore', 'static/\n.env\nauths/\nlogs/\n.codex/\n')
        for name in ('Dockerfile', 'go.mod', 'go.sum', 'config.example.yaml', 'main.go', 'deleted.go'):
            self.write(name, 'original\n')
        self.write('static/management.html', 'local panel\n')
        self.git('init', '-q')
        self.git('add', '.')
        self.git('-c', 'user.name=VPS Test', '-c', 'user.email=test@example.invalid', 'commit', '-qm', 'initial')

    def write(self, name, data):
        path = self.root / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(data, encoding='utf-8', newline='\n')

    def git(self, *args):
        return subprocess.check_output(['git', '-C', str(self.root), *args], stderr=subprocess.PIPE)

    def snapshot(self, name='snapshot.tar.gz'):
        output = Path(self.temporary.name) / name
        manifest = deploy.make_snapshot(self.root, output)
        with tarfile.open(output) as archive:
            files = {item.name: archive.extractfile(item).read() for item in archive}
        return manifest, files

    def test_current_files_and_ignored_panel_are_included_but_secrets_are_not(self):
        self.write('main.go', 'modified\n')
        self.write('new.go', 'untracked\n')
        (self.root / 'deleted.go').unlink()
        for name in ('config.yaml', 'config.private.yaml', '.env', 'auths/token.json',
                     'logs/request.log', '.codex/vps-deploy.json', 'private.pem'):
            self.write(name, 'private content\n')
            self.git('add', '-f', name)
        manifest, files = self.snapshot()
        self.assertTrue(manifest['dirty'])
        self.assertEqual(files['main.go'], b'modified\n')
        self.assertEqual(files['new.go'], b'untracked\n')
        self.assertEqual(files['static/management.html'], b'local panel\n')
        self.assertNotIn('deleted.go', files)
        self.assertFalse(any(b'private content' in value for value in files.values()))
        self.assertEqual(json.loads(files['deploy-manifest.json']), manifest)

    def test_panel_changes_affect_the_snapshot_hash_even_when_git_ignores_them(self):
        first, _ = self.snapshot('one.tar.gz')
        self.write('static/management.html', 'updated panel\n')
        second, _ = self.snapshot('two.tar.gz')
        self.assertFalse(first['dirty'])
        self.assertFalse(second['dirty'])
        self.assertEqual(first['commit'], second['commit'])
        self.assertNotEqual(first['snapshot_sha256'], second['snapshot_sha256'])

    def test_missing_panel_fails_before_any_upload(self):
        (self.root / 'static/management.html').unlink()
        with self.assertRaisesRegex(ValueError, 'management page'):
            self.snapshot()

    def test_archive_hash_describes_archived_bytes(self):
        manifest, files = self.snapshot()
        digest = hashlib.sha256()
        for name in sorted(set(files) - {'deploy-manifest.json'}):
            mode = 0o755 if (self.root / name).stat().st_mode & 0o100 else 0o644
            digest.update(name.encode() + b'\0' + str(mode).encode() + b'\0')
            digest.update(hashlib.sha256(files[name]).digest())
        self.assertEqual(manifest['snapshot_sha256'], digest.hexdigest())

    def test_unsafe_deployment_arguments_are_rejected(self):
        args = argparse.Namespace(directory='/opt/../root', project='cpa', port=8317,
                                  ssh_port=22, lines=100, action='deploy', host='example.invalid')
        with self.assertRaises(ValueError):
            deploy.validate_arguments(args)
        args.directory = '/opt/cpa'
        args.project = 'cpa; command'
        with self.assertRaises(ValueError):
            deploy.validate_arguments(args)


BASH = shutil.which('bash') or ('C:/Program Files/Git/bin/bash.exe' if os.name == 'nt' else None)


@unittest.skipUnless(BASH and Path(BASH).exists(), 'Bash is unavailable')
class ManagerTests(unittest.TestCase):
    def run_fixture(self, scenario):
        fixture = Path(__file__).with_name('test_manager.sh')
        manager = Path(__file__).with_name('manage.sh')
        completed = subprocess.run([BASH, str(fixture), str(manager), scenario],
                                   text=True, capture_output=True)
        self.assertEqual(completed.returncode, 0, completed.stdout + completed.stderr)

    def test_failed_build_keeps_the_current_deployment(self):
        self.run_fixture('build-failure')

    def test_failed_health_check_restores_previous_image(self):
        self.run_fixture('health-failure')

    def test_successful_update_preserves_credentials_data_and_port(self):
        self.run_fixture('success')


if __name__ == '__main__':
    unittest.main()

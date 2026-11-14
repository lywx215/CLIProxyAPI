"""Transactional state and a persistent maintenance lease shared with old tooling."""
from __future__ import annotations

import contextlib
import json
import os
import sqlite3
import time
import uuid
from pathlib import Path

from .core import digest, identifier, require
from .security import Vault


class Store:
    def __init__(self, directory, vault=None):
        self.root = Path(directory)
        self.root.mkdir(parents=True, exist_ok=True)
        self.vault = vault or Vault()
        self.db = self.root / 'operations.sqlite3'
        with self.connect() as db:
            db.executescript('''
                CREATE TABLE IF NOT EXISTS plans(id TEXT PRIMARY KEY, body TEXT NOT NULL, hash TEXT NOT NULL);
                CREATE TABLE IF NOT EXISTS runs(id TEXT PRIMARY KEY, plan TEXT NOT NULL UNIQUE,
                    status TEXT NOT NULL, pause INTEGER NOT NULL DEFAULT 0, data TEXT NOT NULL);
                CREATE TABLE IF NOT EXISTS targets(run TEXT, target TEXT, phase TEXT, data TEXT NOT NULL,
                    PRIMARY KEY(run,target));
                CREATE TABLE IF NOT EXISTS events(seq INTEGER PRIMARY KEY AUTOINCREMENT, run TEXT,
                    target TEXT, at REAL, event TEXT, data TEXT);
                CREATE TABLE IF NOT EXISTS secrets(run TEXT, name TEXT, data BLOB,
                    PRIMARY KEY(run,name));
            ''')

    @contextlib.contextmanager
    def connect(self):
        db = sqlite3.connect(self.db, timeout=10)
        db.row_factory = sqlite3.Row
        db.execute('PRAGMA journal_mode=WAL')
        db.execute('PRAGMA synchronous=FULL')
        try:
            yield db
            db.commit()
        except BaseException:
            db.rollback()
            raise
        finally:
            db.close()

    def plan(self, body, *, snapshots=None):
        plan_id = 'plan-' + uuid.uuid4().hex[:16]
        blobs = [(plan_id, name, self.vault.seal(value)) for name, value in (snapshots or {}).items()]
        with self.connect() as db:
            db.execute('INSERT INTO plans VALUES(?,?,?)', (plan_id, json.dumps(body), digest(body)))
            db.executemany('INSERT INTO secrets VALUES(?,?,?)', blobs)
        return plan_id

    def get_plan(self, plan_id):
        with self.connect() as db:
            row = db.execute('SELECT * FROM plans WHERE id=?', (identifier(plan_id),)).fetchone()
        require(row is not None, 'plan_not_found')
        body = json.loads(row['body'])
        require(digest(body) == row['hash'], 'plan_modified')
        return body

    def begin(self, plan_id):
        plan = self.get_plan(plan_id)
        with self.connect() as db:
            db.execute('BEGIN IMMEDIATE')
            row = db.execute('SELECT id FROM runs WHERE plan=?', (plan_id,)).fetchone()
            if row:
                return row['id']
            run_id = 'run-' + uuid.uuid4().hex[:16]
            db.execute('INSERT INTO runs VALUES(?,?,?,0,?)',
                       (run_id, plan_id, 'ready', json.dumps({'created_at': time.time()})))
            for target in plan['targets']:
                db.execute('INSERT INTO targets VALUES(?,?,?,?)', (run_id, target['label'], 'pending', '{}'))
        return run_id

    def run(self, run_id):
        with self.connect() as db:
            row = db.execute('SELECT * FROM runs WHERE id=?', (identifier(run_id),)).fetchone()
            targets = db.execute('SELECT * FROM targets WHERE run=? ORDER BY rowid', (run_id,)).fetchall()
        require(row is not None, 'run_not_found')
        return {**dict(row), 'data': json.loads(row['data']),
                'targets': [{**dict(item), 'data': json.loads(item['data'])} for item in targets]}

    def target(self, run_id, label):
        return next(item for item in self.run(run_id)['targets'] if item['target'] == label)

    def status(self, run_id, status, **values):
        data = {**self.run(run_id)['data'], **values}
        with self.connect() as db:
            db.execute('UPDATE runs SET status=?,data=? WHERE id=?', (status, json.dumps(data), run_id))
            db.execute('INSERT INTO events(run,target,at,event,data) VALUES(?,?,?,?,?)',
                       (run_id, None, time.time(), 'run_status', json.dumps({'status': status, 'reason': values.get('reason')})))

    def pause(self, run_id, value=True):
        self.run(run_id)
        with self.connect() as db:
            db.execute('UPDATE runs SET pause=? WHERE id=?', (int(value), run_id))

    def event(self, run_id, label, event, data):
        with self.connect() as db:
            db.execute('INSERT INTO events(run,target,at,event,data) VALUES(?,?,?,?,?)',
                       (run_id, label, time.time(), event, json.dumps(data)))

    def has_snapshot(self, run_id, name):
        with self.connect() as db:
            return db.execute('SELECT 1 FROM secrets WHERE run=? AND name=?', (run_id, name)).fetchone() is not None

    def save(self, run_id, label, phase, **values):
        old = self.target(run_id, label)
        data = {**old['data'], **values}
        now = time.time()
        with self.connect() as db:
            db.execute('UPDATE targets SET phase=?,data=? WHERE run=? AND target=?',
                       (phase, json.dumps(data), run_id, label))
            db.execute('INSERT INTO events(run,target,at,event,data) VALUES(?,?,?,?,?)',
                       (run_id, label, now, phase, json.dumps(values)))
        return data

    def seal(self, run_id, name, value):
        blob = self.vault.seal(value)
        with self.connect() as db:
            db.execute('INSERT OR REPLACE INTO secrets VALUES(?,?,?)', (run_id, name, blob))

    def unseal(self, run_id, name):
        with self.connect() as db:
            row = db.execute('SELECT data FROM secrets WHERE run=? AND name=?', (run_id, name)).fetchone()
        require(row is not None, 'encrypted_snapshot_missing')
        return self.vault.open(row['data'])

    def events(self, run_id):
        with self.connect() as db:
            rows = db.execute('SELECT * FROM events WHERE run=? ORDER BY seq', (run_id,)).fetchall()
        return [{**dict(row), 'data': json.loads(row['data'])} for row in rows]


class ProcessLock:
    """OS-owned lock; crashes release it, unlike the persistent production lease."""
    def __init__(self, path):
        self.path = Path(path)

    def __enter__(self):
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self.stream = self.path.open('a+b')
        self.stream.seek(0, 2)
        if self.stream.tell() == 0:
            self.stream.write(b'0')
            self.stream.flush()
        self.stream.seek(0)
        try:
            if os.name == 'nt':
                import msvcrt
                msvcrt.locking(self.stream.fileno(), msvcrt.LK_NBLCK, 1)
            else:
                import fcntl
                fcntl.flock(self.stream.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
        except OSError:
            self.stream.close()
            require(False, 'another_controller_running')
        return self

    def __exit__(self, *args):
        self.stream.close()


class Lease:
    def __init__(self, path, hot_path):
        self.path, self.hot_path = Path(path), Path(hot_path)

    def claim(self, run_id, target, origin, operation_dir):
        require(not self.hot_path.exists(), 'hot_configuration_window_active')
        identity = {'run_id': run_id, 'target': target, 'origin': origin,
                    'operation_dir': str(Path(operation_dir).resolve())}
        if self.path.exists():
            row = json.loads(self.path.read_text(encoding='utf-8'))
            require(all(row.get(key) == value for key, value in identity.items()), 'foreign_maintenance_lease')
            return row['lease_id']
        self.path.parent.mkdir(parents=True, exist_ok=True)
        row = {**identity, 'lease_id': uuid.uuid4().hex, 'pid': os.getpid(),
               'created_at': time.time(), 'operation': 'cliproxy-ops-update'}
        try:
            with self.path.open('x', encoding='utf-8') as stream:
                json.dump(row, stream)
                stream.flush()
                os.fsync(stream.fileno())
        except FileExistsError:
            require(False, 'foreign_maintenance_lease')
        return row['lease_id']

    def check(self, token):
        require(self.path.exists(), 'maintenance_lease_missing')
        row = json.loads(self.path.read_text(encoding='utf-8'))
        require(row.get('lease_id') == token and not self.hot_path.exists(), 'maintenance_lease_changed')

    def release(self, token):
        self.check(token)
        self.path.unlink()

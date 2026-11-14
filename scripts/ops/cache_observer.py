"""Read-only cache synchronization observer executed over verified SSH stdio."""
from collections import deque
import calendar
import datetime
import json
import re
import subprocess
import sys
import threading
import time


def timestamp_ns(value):
    match = re.fullmatch(r'(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d)(?:\.(\d{1,9}))?Z', value or '')
    if not match:
        raise ValueError('invalid_sync_timestamp')
    seconds = calendar.timegm(datetime.datetime.strptime(match[1], '%Y-%m-%dT%H:%M:%S').timetuple())
    return seconds * 1_000_000_000 + int((match[2] or '').ljust(9, '0'))


class SyncEvents:
    def __init__(self, container_id):
        self.container_id = container_id
        self.events, self.sequence, self.barriers = deque(maxlen=4096), 0, {}
        self.condition = threading.Condition()
        self.failed = False

    def ingest(self, line):
        kind = 'start' if ' | syncing channels from database' in line else (
            'finish' if ' | channels synced from database' in line else None)
        if kind is None:
            return
        stamp = line.split()[0]
        try:
            nanos = timestamp_ns(stamp)
        except ValueError:
            with self.condition:
                self.failed = True
                self.condition.notify_all()
            return
        with self.condition:
            self.sequence += 1
            self.events.append((self.sequence, kind, nanos, stamp))
            self.condition.notify_all()

    def barrier(self, operation_id, *, now_ns=None):
        with self.condition:
            value = {'operation_id': operation_id, 'container_id': self.container_id,
                     'after_ns': time.time_ns() if now_ns is None else now_ns, 'cursor': self.sequence}
            self.barriers[operation_id] = value
            return dict(value)

    def proof(self, operation_id):
        barrier = self.barriers.get(operation_id)
        if barrier is None:
            raise ValueError('sync_barrier_missing')
        start = None
        for sequence, kind, nanos, stamp in self.events:
            if sequence <= barrier['cursor'] or nanos <= barrier['after_ns']:
                continue
            if kind == 'start':
                start = (nanos, stamp)
            elif start and nanos >= start[0]:
                return {**barrier, 'sync_started_at': start[1], 'sync_finished_at': stamp,
                        'after_status_writes': True}
        return None


def main(spec):
    container_id, started_at = spec['container_id'], spec['started_at']
    if not re.fullmatch(r'[a-f0-9]{64}', container_id) or type(spec['sync_seconds']) is not int or not 1 <= spec['sync_seconds'] <= 600:
        raise ValueError('invalid_sync_observer_identity')
    timestamp_ns(started_at)

    def inspect():
        result = subprocess.run(['docker', 'inspect', '--format', '{{.Id}} {{.State.Running}} {{.State.StartedAt}}',
                                 'newapi-slave1-shadow'], capture_output=True, text=True, timeout=10)
        if result.returncode or result.stdout.split() != [container_id, 'true', started_at]:
            raise ValueError('shadow_process_changed')
    inspect()
    events = SyncEvents(container_id)
    process = subprocess.Popen(['docker', 'logs', '--follow', '--timestamps', '--since', str(time.time()), container_id],
                               stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, bufsize=1)
    def consume():
        try:
            for line in process.stdout:
                events.ingest(line)
        finally:
            with events.condition:
                events.failed = True
                events.condition.notify_all()
    threading.Thread(target=consume, daemon=True).start()
    print(json.dumps({'ready': True, 'container_id': container_id, 'started_at': started_at}), flush=True)
    try:
        for line in sys.stdin:
            command = json.loads(line)
            if command.get('action') == 'stop':
                break
            identity = command.get('operation_id', '')
            if not re.fullmatch(r'[a-f0-9]{32}', identity):
                raise ValueError('invalid_sync_operation')
            inspect()
            if command['action'] == 'barrier':
                result = events.barrier(identity)
            elif command['action'] == 'wait':
                deadline = time.monotonic() + 2 * spec['sync_seconds'] + 15
                while True:
                    with events.condition:
                        if events.failed:
                            raise ValueError('shadow_sync_log_unavailable')
                        result = events.proof(identity)
                        if result:
                            break
                        remaining = deadline - time.monotonic()
                        if remaining <= 0:
                            raise ValueError('shadow_sync_not_observed')
                        events.condition.wait(min(2, remaining))
                    inspect()
                inspect()
            else:
                raise ValueError('invalid_sync_command')
            print(json.dumps({**result, 'started_at': started_at}), flush=True)
    finally:
        process.terminate()
        try:
            process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait()


if __name__ == '__main__':
    try:
        main(json.loads(sys.argv[1]))
    except Exception as error:
        code = str(error)
        print(json.dumps({'error': code if re.fullmatch(r'[a-z_]{1,64}', code) else 'sync_observer_failed'}), flush=True)
        sys.exit(2)

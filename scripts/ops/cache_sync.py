"""Client for a current-window, read-only remote cache observer."""
import json
from pathlib import Path
import shlex
import threading

from .core import READ_SLOTS, OpsError, require
from .telemetry import operation, note
from .cache_observer import timestamp_ns


class CacheObserver:
    REMOTE_ERRORS = {'invalid_sync_observer_identity', 'shadow_process_changed', 'invalid_sync_operation',
                     'shadow_sync_log_unavailable', 'shadow_sync_not_observed', 'sync_barrier_missing',
                     'invalid_sync_command', 'sync_observer_failed'}

    def __init__(self, factory, host, proof):
        self.proof, self.lock = proof, threading.RLock()
        self.barriers, self.receipts = {}, {}
        self.client = self.stdin = self.stdout = None
        try:
            source = Path(__file__).with_name('cache_observer.py').read_text(encoding='utf-8')
            spec = json.dumps({key: proof[key] for key in ('container_id', 'started_at', 'sync_seconds')})
            with READ_SLOTS, operation('cache_observer_setup'):
                self.client = factory.connect(host)
                self.stdin, self.stdout, _ = self.client.exec_command(shlex.join(['python3', '-u', '-c', source, spec]), timeout=20)
                ready = self.response()
                require(ready.get('ready') is True, 'sync_observer_unavailable')
        except BaseException:
            self.close()
            raise

    def response(self):
        value = json.loads(self.stdout.readline())
        require(isinstance(value, dict), 'sync_observer_invalid_response')
        error = value.get('error')
        if error:
            require(isinstance(error, str) and error in self.REMOTE_ERRORS, 'sync_observer_invalid_response')
            raise OpsError(error)
        require(all(value.get(key) == self.proof[key] for key in ('container_id', 'started_at')),
                'shadow_sync_observation_identity_changed')
        return value

    def command(self, action, operation_id):
        try:
            self.stdout.channel.settimeout(2 * self.proof['sync_seconds'] + 35 if action == 'wait' else 20)
            self.stdin.write(json.dumps({'action': action, 'operation_id': operation_id}) + '\n')
            self.stdin.flush()
            value = self.response()
        except (OSError, EOFError, ValueError):
            raise OpsError('sync_observer_transport_failed', category='network') from None
        require(value.get('operation_id') == operation_id, 'shadow_sync_observation_identity_changed')
        return value

    def barrier(self, operation_id):
        with self.lock, READ_SLOTS, operation('cache_sync_barrier'):
            self.receipts.pop(operation_id, None)
            self.barriers.pop(operation_id, None)
            value = self.command('barrier', operation_id)
            require(type(value.get('after_ns')) is int and value['after_ns'] > 0 and type(value.get('cursor')) is int
                    and value['cursor'] >= 0, 'sync_barrier_missing')
            self.barriers[operation_id] = value
            return value

    def wait(self, operation_id):
        with self.lock, operation('cache_sync', kind='wait'):
            barrier = self.barriers.get(operation_id)
            require(barrier is not None, 'sync_barrier_missing')
            if operation_id in self.receipts:
                note('cache_hit', scope='cache_sync')
                return self.receipts[operation_id]
            value = self.command('wait', operation_id)
            try:
                start, finish = (timestamp_ns(value.get(key)) for key in ('sync_started_at', 'sync_finished_at'))
            except (ValueError, TypeError):
                raise OpsError('shadow_sync_not_observed') from None
            require(value.get('after_status_writes') is True and value.get('after_ns') == barrier['after_ns']
                    and value.get('cursor') == barrier['cursor'] and barrier['after_ns'] < start <= finish,
                    'shadow_sync_not_observed')
            self.receipts[operation_id] = value
            return value

    def close(self):
        if self.stdin is not None:
            try:
                self.stdin.write('{"action":"stop"}\n')
                self.stdin.flush()
                self.stdout.channel.close()
            except Exception:
                pass
        if self.client is not None:
            self.client.close()

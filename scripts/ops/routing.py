"""New API routing snapshots and observed per-backend cache propagation."""
from __future__ import annotations

import json
import contextlib
from .concurrency import TaskExecutor as ThreadPoolExecutor
import re
import shlex
import threading
import time
import uuid
from pathlib import Path
from urllib.parse import urlencode, urlsplit

from scripts.vps.maintenance import request_log_marker
from .core import MAX_READ_WORKERS, READ_SLOTS, OpsError, channel_fingerprint, failure_category, is_claude_model, require
from .transport import HTTP, remote_json
from . import shadow_relay
from .cache_sync import CacheObserver
from .telemetry import operation, note


def channel_test_model(row, exclusions):
    models = [name.strip() for name in row.get('models', '').split(',') if name.strip()]
    mapping = row.get('model_mapping') or {}
    if isinstance(mapping, str):
        try:
            mapping = json.loads(mapping) if mapping.strip() else {}
        except ValueError:
            raise OpsError('channel_model_mapping_invalid') from None
    if mapping is None:
        mapping = {}
    require(isinstance(mapping, dict) and all(value is None or isinstance(value, str) for value in mapping.values()),
            'channel_model_mapping_invalid')
    eligible = []
    for name in models:
        mapped = (mapping.get(name) or name).strip()
        if name in exclusions or mapped in exclusions or is_claude_model(name) or is_claude_model(mapped):
            continue
        eligible.append(name)
    chosen = row.get('test_model')
    if not chosen or chosen in exclusions or is_claude_model(chosen) or (chosen in models and chosen not in eligible):
        chosen = next(iter(eligible), None)
    require(chosen in eligible, 'channel_has_no_eligible_test_model')
    return chosen, (mapping.get(chosen) or chosen).strip()


class Observer:
    def __init__(self, factory, hosts):
        self.clients, self.streams = [], []
        self.pending, self.condition, self.failed = {}, threading.Condition(), False
        self.clock = time.monotonic
        self.prefix = uuid.uuid4().hex[:8]
        script = Path(__file__).with_name('route_observer.py').read_text(encoding='utf-8')
        try:
            for name in hosts:
                with READ_SLOTS, operation('route_observer_setup'):
                    ssh = factory.connect(name)
                    self.clients.append(ssh)
                    stdin, stdout, _ = ssh.exec_command(shlex.join(['python3', '-u', '-c', script, self.prefix]), timeout=20)
                    self.streams.append((stdin, stdout))
                    ready = json.loads(stdout.readline())
                    require(isinstance(ready, dict) and ready.get('ready') is True, 'route_observer_unavailable')
                    stdout.channel.settimeout(None)

                def consume(name=name, stdout=stdout):
                    try:
                        for line in stdout:
                            self.record(name, json.loads(line))
                    except (OSError, ValueError):
                        pass
                    finally:
                        with self.condition:
                            self.failed = True
                            self.condition.notify_all()

                threading.Thread(target=consume, daemon=True).start()
        except BaseException:
            self.close()
            raise

    def nonce(self):
        identity = self.prefix + uuid.uuid4().hex[8:]
        with self.condition:
            self.pending[identity] = {'events': [], 'deadline': None}
        return identity

    def record(self, host, row):
        if not isinstance(row, dict) or not isinstance(row.get('nonce'), str) or not isinstance(row.get('backend'), str):
            return
        with self.condition:
            current = self.pending.get(row.get('nonce'))
            if current is not None:
                current['events'].append({'host': host, 'backend': row['backend']})
                self.condition.notify_all()

    def complete(self, nonce):
        with self.condition:
            require(nonce in self.pending, 'unregistered_observer_nonce')
            self.pending[nonce]['deadline'] = self.clock() + 2
            self.condition.notify_all()

    def read_many(self, nonces):
        with self.condition:
            require(len(nonces) == len(set(nonces)) and
                    all(nonce in self.pending and self.pending[nonce]['deadline'] is not None for nonce in nonces),
                    'incomplete_observer_batch')
            while nonces:
                require(not self.failed, 'route_observer_disconnected')
                remaining = max(self.pending[nonce]['deadline'] for nonce in nonces) - self.clock()
                if remaining <= 0:
                    break
                self.condition.wait(remaining)
            return {nonce: self.pending.pop(nonce)['events'] for nonce in nonces}

    def read(self, nonce):
        self.complete(nonce)
        return self.read_many([nonce])[nonce]

    def close(self):
        for stdin, stdout in self.streams:
            try:
                stdin.write('stop\n')
                stdin.flush()
                stdout.channel.close()
            except Exception:
                pass
        for ssh in self.clients:
            ssh.close()


class Routing:
    def __init__(self, sites, ssh_factory, vault, guard=lambda: None):
        self.sites, self.ssh, self.vault, self.guard = sites, ssh_factory, vault, guard
        self.clients = {}
        self.client_lock = threading.Lock()
        self.observers, self.sync_observers = {}, {}
        self.sync_executor = None
        self.active_session = False
        self.record_event = lambda event, data: None

    @contextlib.contextmanager
    def session(self):
        self.active_session = True
        try:
            yield self
        finally:
            self.close()

    def close(self):
        if self.sync_executor is not None:
            self.sync_executor.shutdown(wait=True, cancel_futures=True)
            self.sync_executor = None
        for observer in [*self.observers.values(), *self.sync_observers.values()]:
            observer.close()
        for client in self.clients.values():
            client.close()
        self.observers.clear()
        self.sync_observers.clear()
        self.clients.clear()
        self.active_session = False

    def observer(self, site):
        if site not in self.observers:
            self.observers[site] = Observer(self.ssh, self.sites[site]['observers'])
        return self.observers[site]

    def observe_cache(self, site, topology):
        for (host, address), proof in self.shadows(site, topology).items():
            key = (site, host, address)
            if key not in self.sync_observers:
                matching = [app for app in topology[host]['apps'] if app.get('id') == proof['container_id']]
                require(len(matching) == 1 and matching[0].get('started_at'), 'shadow_topology_unverified')
                self.sync_observers[key] = CacheObserver(self.ssh, host, {**proof, 'started_at': matching[0]['started_at']})
                if self.sync_executor is None:
                    self.sync_executor = ThreadPoolExecutor(max_workers=2)

    def prepare_session(self, snapshot):
        for site, rows in snapshot['channels'].items():
            if not any(row['status'] == 1 for row in rows):
                continue
            self.observer(site)
            self.observe_cache(site, snapshot['topology'][site])

    def probe_batch(self, site, observer, addresses, *, ids=None, status=None, intent=None):
        requests = [(observer.nonce(), shadow, address) for shadow, address in addresses]
        if not requests:
            return []
        sync = {}
        def send(item):
            nonce, shadow, address = item
            body = {'ids': ids, 'status': status} if ids is not None else None
            path = ('/api/channel/status/batch' if body is not None else '/api/status') + '?_cpa_ops_nonce=' + nonce
            if body is not None:
                self.guard()
            result = self.shadow_request(site, address, path, body) if shadow else self.request(
                site, path, address=address, body=body)
            observer.complete(nonce)
            barrier = None
            if body is not None:
                require(type(result.get('data')) is int and 0 <= result['data'] <= len(ids), 'channel_status_ack_invalid')
                if shadow:
                    monitor = self.sync_observers[(site, *address)]
                    barrier = monitor.barrier(nonce)
                self.record_event('propagation_receipt', {'site': site, 'operation_id': nonce, 'intent': intent,
                                                          'status': status, 'shadow': shadow, 'sync_barrier': barrier})
                if barrier is not None:
                    sync[nonce] = self.sync_executor.submit(monitor.wait, nonce)
            return result
        with ThreadPoolExecutor(max_workers=MAX_READ_WORKERS) as executor:
            futures = [executor.submit(send, item) for item in requests]
            results = [future.result() for future in futures]
        with operation('route_observation_window', kind='wait'):
            observed = observer.read_many([nonce for nonce, _, _ in requests])
        return [(item, result, observed[item[0]], sync.get(item[0])) for item, result in zip(requests, results)]

    def client(self, site, address=None):
        config = self.sites[site]
        address = address or config['entry_addresses'][0]
        key = (site, address)
        with self.client_lock:
            if key not in self.clients:
                require(config['origin'] in {'https://gemini30.pro', 'https://vip.gemini30.pro'}, 'newapi_origin_changed')
                value = self.vault.reference(config['credential'], origin=config['origin'])
                token = value if isinstance(value, str) else value[config.get('token_field', 'token')]
                self.clients[key] = HTTP(config['origin'], key=token, user=config.get('user_id', 1), address=address)
        return self.clients[key]

    def request(self, site, path, *, address=None, body=None, model=False, timeout=None):
        route = urlsplit(path).path
        if body is not None:
            self.guard()
            require(route == '/api/channel/status/batch' and set(body) == {'ids', 'status'}
                    and body['status'] in {1, 2}, 'routing_mutation_rejected')
        elif model:
            self.guard()
            require(route.startswith('/api/channel/test/'), 'channel_test_path_rejected')
        else:
            require(route in {'/api/channel/', '/api/log/', '/api/status'}, 'newapi_read_path_rejected')
        with READ_SLOTS if not model and body is None else contextlib.nullcontext():
            response = self.client(site, address).request(path, 'POST' if body is not None else 'GET', body,
                                                         timeout=timeout or (240 if model else 45),
                                                         read_only=not model and body is None)
        require(response['http'] == 200 and isinstance(response['data'], dict), 'newapi_response_invalid')
        data = response['data']
        if not model:
            require(data.get('success') is True, 'newapi_operation_not_confirmed')
        return data

    def channels(self, domains):
        def read_site(site):
            rows = {}
            for page in range(1, 1001):
                data = self.request(site, '/api/channel/?' + urlencode({'p': page, 'page_size': 100,
                                                                      '_cpa_ops': time.time_ns()}))['data']
                items = data['items']
                previous = len(rows)
                rows.update((row['id'], row) for row in items)
                if len(rows) >= data['total']:
                    break
                require(len(rows) > previous, 'channel_pagination_stalled')
            else:
                require(False, 'channel_inventory_incomplete')
            return site, sorted([row for row in rows.values()
                                 if urlsplit(row.get('base_url') or '').hostname in set(domains)],
                                key=lambda row: row['id'])
        with ThreadPoolExecutor(max_workers=MAX_READ_WORKERS) as executor:
            return dict(executor.map(read_site, self.sites))

    def topology(self):
        script = Path(__file__).with_name('newapi_probe.py').read_text(encoding='utf-8')
        result = {site: {} for site in self.sites}
        def read_host(item):
            site, name, probe = item
            with READ_SLOTS, operation('newapi_topology'):
                ssh = self.ssh.connect(name)
                try:
                    value = remote_json(ssh, script, {**probe, 'hostname': urlsplit(self.sites[site]['origin']).hostname})
                    return site, name, value
                finally:
                    ssh.close()
        tasks = [(site, name, probe) for site, config in self.sites.items() for name, probe in config['observers'].items()]
        with ThreadPoolExecutor(max_workers=MAX_READ_WORKERS) as executor:
            for site, name, value in executor.map(read_host, tasks):
                result[site][name] = value
        return result

    def snapshot(self, domains):
        with ThreadPoolExecutor(max_workers=2) as executor:
            channels_future = executor.submit(self.channels, domains)
            topology_future = executor.submit(self.topology)
            channels, topology = channels_future.result(), topology_future.result()
            startup_futures = {site: executor.submit(self.observe_startups, site, topology[site])
                               for site, rows in channels.items() if any(row['status'] == 1 for row in rows)}
            startups = {site: future.result() for site, future in startup_futures.items()}
        self.confirm_shadow_rows(channels, topology)
        return {'domains': sorted(domains), 'channels': channels, 'topology': topology, 'startups': startups}

    @staticmethod
    def expected(snapshot, restored=False):
        return {site: {str(row['id']): row['status'] if restored or row['status'] != 1 else 2
                       for row in rows} for site, rows in snapshot['channels'].items()}

    def confirm(self, snapshot, *, restored=False, transitional=False):
        rows = self.channels(snapshot['domains'])
        expected = self.expected(snapshot, restored)
        for site, old_rows in snapshot['channels'].items():
            require({row['id'] for row in rows[site]} == {row['id'] for row in old_rows}, 'channel_set_changed')
            old = {row['id']: row for row in old_rows}
            for row in rows[site]:
                before = old[row['id']]
                require(channel_fingerprint(row) == channel_fingerprint(before), 'channel_configuration_changed')
                allowed = {expected[site][str(row['id'])]}
                if transitional and before['status'] == 1:
                    allowed = {1, 2}
                require(row['status'] in allowed, 'channel_status_changed')
        return rows

    def wanted(self, site, topology):
        config = self.sites[site]
        observers = config['observers']
        wanted = set()
        for name, host in topology.items():
            active = [row['address'] for row in host['backends'] if not row['down'] and not row['backup']]
            active.extend(observers[name].get('extra_active_backends', []))
            require(active, 'newapi_active_backends_missing')
            if config['propagation'] == 'upstream':
                wanted.update((name, address) for address in active)
            else:
                wanted.add((name, 'app'))
        return wanted

    def shadows(self, site, topology):
        result = {}
        config = self.sites[site]
        for name, probe in config['observers'].items():
            if not probe.get('shadow_admin_relay'):
                continue
            value = topology[name].get('shadow')
            require(config['origin'] == shadow_relay.ORIGIN and value and value['address'] == shadow_relay.ADDRESS
                    and value['container'] == 'newapi-slave1-shadow', 'shadow_topology_unverified')
            result[(name, value['address'])] = value
        return result

    def shadow_request(self, site, target, path, body=None):
        require(self.sites[site]['origin'] == shadow_relay.ORIGIN and target[1] == shadow_relay.ADDRESS,
                'shadow_origin_binding_rejected')
        if body is not None:
            self.guard()
        with (READ_SLOTS if body is None else contextlib.nullcontext()), operation(
                'shadow_backend_request', kind='read' if body is None else 'propagation'):
            if path.startswith('/api/status?'):
                return shadow_relay.request(self.ssh, target[0], path)
            client = self.client(site)
            require(not client.blocked, 'authentication_circuit_open', category='authorization')
            try:
                return shadow_relay.request(self.ssh, target[0], path, body=body, credential=client.key, user=client.user)
            except OpsError as error:
                if error.category == 'authorization':
                    client.block_authentication()
                raise

    def confirm_shadow_rows(self, channels, topology):
        tasks = []
        for site, rows in channels.items():
            if not any(row['status'] == 1 for row in rows) and not any(row['status'] == 2 for row in rows):
                continue
            for target in self.shadows(site, topology[site]):
                for row in rows:
                    tasks.append((site, target, row))
        def read_channel(item):
            site, target, row = item
            path = '/api/channel/' + str(row['id']) + '?_cpa_ops_nonce=' + uuid.uuid4().hex
            fresh = self.shadow_request(site, target, path)['data']
            require(all(fresh.get(key) == row.get(key) for key in ('id', 'status', 'base_url', 'models')),
                    'shadow_channel_readback_mismatch')
        with ThreadPoolExecutor(max_workers=MAX_READ_WORKERS) as executor:
            list(executor.map(read_channel, tasks))

    def attributed(self, site, topology, events, wanted):
        items = {self.observed_destination(self.sites[site], topology, event) for event in events}
        matched = items & wanted
        return next(iter(matched)) if len(matched) == 1 else None

    @staticmethod
    def public_batches(config, public):
        if public:
            entries = config['entry_addresses']
            size = min(MAX_READ_WORKERS, len(public))
            for index in range(0, 32, size):
                yield [(False, entries[offset % len(entries)]) for offset in range(index, min(index + size, 32))]

    def observe_startups(self, site, topology, observer=None):
        config, wanted = self.sites[site], self.wanted(site, topology)
        shadows = self.shadows(site, topology)
        public = wanted - set(shadows)
        owned = observer is None and not self.active_session
        observer = observer or (self.observer(site) if self.active_session else Observer(self.ssh, config['observers']))
        starts = {}
        try:
            for addresses in self.public_batches(config, public):
                for _, result, events, _ in self.probe_batch(site, observer, addresses):
                    started = (result.get('data') or {}).get('start_time')
                    item = self.attributed(site, topology, events, wanted)
                    if item in public and type(started) is int and started > 0:
                        require(starts.get('/'.join(item), started) == started, 'newapi_startup_changed')
                        starts['/'.join(item)] = started
                if set(starts) >= {'/'.join(item) for item in public}:
                    break
            for (_, _, item), result, events, _ in self.probe_batch(
                    site, observer, [(True, item) for item in shadows]):
                started = (result.get('data') or {}).get('start_time')
                require(self.attributed(site, topology, events, wanted) == item
                        and type(started) is int and started > 0, 'shadow_startup_observation_incomplete')
                starts['/'.join(item)] = started
            require(set(starts) == {'/'.join(item) for item in wanted}, 'newapi_startup_observation_incomplete')
            return starts
        finally:
            if owned:
                observer.close()

    def propagate(self, site, ids, status, topology, *, intent=None):
        if not ids:
            return {'no_originally_enabled_channels': True}
        self.guard()
        config, wanted = self.sites[site], self.wanted(site, topology)
        shadows = self.shadows(site, topology)
        public = wanted - set(shadows)
        owned = not self.active_session
        observer = Observer(self.ssh, config['observers']) if owned else self.observer(site)
        seen = set()
        try:
            if owned:
                self.observe_cache(site, topology)
            for addresses in self.public_batches(config, public):
                self.guard()
                for _, _, events, _ in self.probe_batch(site, observer, addresses, ids=ids, status=status, intent=intent):
                    item = self.attributed(site, topology, events, wanted)
                    if item in public:
                        seen.add(item)
                if seen >= public:
                    break
            # Public probes can reach a shadow backend. Take its barrier after all public writes.
            pending_sync = []
            for (_, _, item), _, events, future in self.probe_batch(
                    site, observer, [(True, item) for item in shadows], ids=ids, status=status, intent=intent):
                require(self.attributed(site, topology, events, wanted) == item,
                        'shadow_status_destination_unverified')
                seen.add(item)
                pending_sync.append((item, future))
            require(seen >= wanted, 'channel_stop_or_restore_propagation_incomplete')
            # Check application startup identity through the same public TLS origins.
            starts = self.observe_startups(site, topology, observer)
            sync_evidence = {item[1]: future.result() for item, future in pending_sync}
        finally:
            if owned:
                observer.close()
                if self.sync_executor is not None:
                    self.sync_executor.shutdown(wait=True, cancel_futures=True)
                    self.sync_executor = None
                for key in list(self.sync_observers):
                    if key[0] == site:
                        self.sync_observers.pop(key).close()
        return {'backends': sorted('/'.join(item) for item in seen), 'startups': starts,
                'shadow_tls_relay': sorted('/'.join(item) for item in shadows), 'shadow_cache_sync': sync_evidence}

    @staticmethod
    def observed_destination(config, topology, event):
        if event.get('host') not in topology:
            return None
        if config['propagation'] == 'upstream':
            return event['host'], event['backend']
        ips = {ip for app in topology[event['host']]['apps'] for ip in app['ips']}
        if event['backend'].rsplit(':', 1)[0] in ips:
            return event['host'], 'app'
        return None

    def set_stopped(self, snapshot, *, restore=False, passed_channels=None, initial=False, validate_before_write=None,
                    record_intent=None):
        if not self.active_session:
            with self.session():
                return self.set_stopped(snapshot, restore=restore, passed_channels=passed_channels, initial=initial,
                                        validate_before_write=validate_before_write, record_intent=record_intent)
        self.guard()
        with ThreadPoolExecutor(max_workers=2) as executor:
            topology = executor.submit(self.topology)
            current = executor.submit(self.confirm, snapshot, restored=initial and not restore, transitional=not initial)
            require(topology.result() == snapshot['topology'], 'newapi_topology_or_process_changed')
            before = current.result()
        if initial and restore:
            require(all(row['status'] != 1 for rows in before.values() for row in rows), 'channel_status_changed')
        self.prepare_session(snapshot)
        with ThreadPoolExecutor(max_workers=3) as executor:
            shadow_rows = executor.submit(self.confirm_shadow_rows, before, snapshot['topology'])
            startups = {site: executor.submit(self.observe_startups, site, snapshot['topology'][site])
                        for site in snapshot.get('startups', {})}
            for site, future in startups.items():
                require(future.result() == snapshot['startups'][site], 'newapi_startup_changed')
            shadow_rows.result()
        result = {}
        tasks = {}
        for site, rows in snapshot['channels'].items():
            ids = [row['id'] for row in rows if row['status'] == 1]
            if restore:
                require(all((passed_channels or {}).get(site + ':' + str(cid), {}).get('success') is True
                            for cid in ids), 'restore_requires_successful_channel_calls')
            require(not ids or site in snapshot.get('startups', {}), 'newapi_startup_observation_incomplete')
            tasks[site] = ids
        intent = uuid.uuid4().hex
        if validate_before_write is not None:
            validate_before_write()
        if record_intent is not None:
            record_intent()
        self.record_event('propagation_intent', {'operation_id': intent,
                                               'status': 1 if restore else 2, 'sites': tasks})
        def propagate(site, ids):
            self.guard()
            with operation('restore_propagation' if restore else 'stop_propagation', kind='phase'):
                proof = self.propagate(site, ids, 1 if restore else 2, snapshot['topology'][site], intent=intent)
                require(not ids or proof.get('startups') == snapshot['startups'][site], 'newapi_startup_changed')
                return proof
        with ThreadPoolExecutor(max_workers=2) as executor:
            pending = {site: executor.submit(propagate, site, ids) for site, ids in tasks.items()}
            for site, future in pending.items():
                result[site] = future.result()
        with ThreadPoolExecutor(max_workers=2) as executor:
            topology = executor.submit(self.topology)
            rows = self.confirm(snapshot, restored=restore)
            self.confirm_shadow_rows(rows, snapshot['topology'])
            require(topology.result() == snapshot['topology'], 'newapi_topology_or_process_changed')
        return result

    def log_markers(self, max_seconds=10):
        deadline = time.monotonic() + max_seconds
        def read(site):
            remaining = deadline - time.monotonic()
            require(remaining > 0, 'global_log_observation_deadline')
            response = self.request(site, '/api/log/?' + urlencode({'p': 1, 'page_size': 1,
                                                                   '_cpa_ops': time.time_ns()}), timeout=min(5, remaining))['data']
            total = response.get('total')
            items = response.get('items')
            require(isinstance(items, list), 'global_logs_unavailable')
            return site, request_log_marker(items[0] if items else None, total)
        with ThreadPoolExecutor(max_workers=2) as executor:
            return dict(executor.map(read, self.sites))

    def test_channel(self, snapshot, site, row, exclusions, *, verify_snapshot=True):
        self.guard()
        if verify_snapshot:
            self.confirm(snapshot)
        require(row['status'] == 1, 'cannot_test_restore_of_originally_disabled_channel')
        chosen, _ = channel_test_model(row, exclusions)
        query = {'model': chosen, 'stream': 'true'}
        if row.get('type') == 24:
            query['endpoint_type'] = 'gemini'
        response = self.request(site, '/api/channel/test/' + str(row['id']) + '?' + urlencode(query), model=True)
        code = response.get('error_code')
        code = code if isinstance(code, str) and re.fullmatch(r'[a-zA-Z0-9_.-]{1,80}', code) else None
        seconds = response.get('time')
        return {'site': site, 'channel': row['id'], 'model': chosen, 'stream': True,
                'success': response.get('success') is True, 'seconds': seconds if type(seconds) in {int, float} else None,
                'category': failure_category(response.get('status_code'), response), 'error_code': code}

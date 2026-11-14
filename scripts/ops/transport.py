"""Exact-origin HTTP and verified SSH. Secrets never enter shell arguments."""
from __future__ import annotations

import http.client
from contextlib import contextmanager
import io
import json
import shlex
import socket
import ssl
import threading
from pathlib import Path
from urllib.parse import urlsplit

from .core import OpsError, require
from .telemetry import operation, note


class _SSHResponseReader(io.RawIOBase):
    def __init__(self, tunnel):
        self.tunnel = tunnel

    def readable(self):
        return True

    def readinto(self, buffer):
        value = self.tunnel.channel.recv(len(buffer))
        buffer[:len(value)] = value
        return len(value)

    def close(self):
        if not self.closed:
            self.tunnel.reader_closed()
        super().close()


class _SSHSocket:
    """Keep the channel alive while http.client owns an open response reader."""

    def __init__(self, channel):
        self.channel = channel
        self.readers = 0
        self.closing = False

    def sendall(self, value):
        self.channel.sendall(value)

    def settimeout(self, timeout):
        self.channel.settimeout(timeout)

    def makefile(self, mode):
        require(mode == 'rb', 'ssh_response_read_mode_required')
        self.readers += 1
        return io.BufferedReader(_SSHResponseReader(self))

    def reader_closed(self):
        self.readers -= 1
        if self.closing and not self.readers:
            self.channel.close()

    def close(self):
        if not self.closing:
            self.closing = True
            if not self.readers:
                self.channel.close()


class HTTP:
    def __init__(self, origin, *, key=None, user=None, address=None, ssh=None):
        parsed = urlsplit(origin)
        require(parsed.scheme in {'http', 'https'} and parsed.hostname and not parsed.username
                and parsed.path in {'', '/'} and not parsed.query and not parsed.fragment, 'invalid_http_origin')
        require(parsed.scheme == 'https' or ssh is not None, 'authenticated_http_requires_ssh')
        self.origin = origin.rstrip('/')
        self.parsed, self.key, self.user = parsed, key, user
        self.address, self.ssh = address, ssh
        self.blocked = False
        self.lock = threading.Lock()
        self.pool = []
        self.closed = False

    def connection(self, timeout):
        hostname = self.parsed.hostname
        port = self.parsed.port or (443 if self.parsed.scheme == 'https' else 80)
        if self.ssh:
            conn = http.client.HTTPConnection(hostname, port, timeout=timeout)
            channel = self.ssh.get_transport().open_channel('direct-tcpip', ('127.0.0.1', port), ('127.0.0.1', 0),
                                                            timeout=timeout)
            channel.settimeout(timeout)
            conn.sock = _SSHSocket(channel)
        elif self.address:
            address = self.address

            class Pinned(http.client.HTTPSConnection):
                def connect(inner):
                    raw = socket.create_connection((address, port), inner.timeout)
                    inner.sock = inner._context.wrap_socket(raw, server_hostname=hostname)

            conn = Pinned(hostname, port, timeout=timeout, context=ssl.create_default_context())
        else:
            conn = http.client.HTTPSConnection(hostname, port, timeout=timeout)
        return conn

    def open(self, path, method='GET', body=None, *, key=None, headers=None, timeout=60,
             connection=None, keep_alive=False):
        part = urlsplit(path)
        require(path.startswith('/') and not path.startswith('//') and not part.scheme
                and not part.netloc and not part.fragment, 'cross_origin_request_rejected')
        with self.lock:
            require(not self.blocked, 'authentication_circuit_open', category='authorization')
            require(not self.closed, 'http_session_closed')
        outgoing = {'Accept': 'application/json', 'Cache-Control': 'no-store',
                    'Connection': 'keep-alive' if keep_alive else 'close'}
        credential = key if key is not None else self.key
        if credential:
            outgoing['Authorization'] = 'Bearer ' + credential
        if self.user is not None:
            outgoing['New-Api-User'] = str(self.user)
        outgoing.update(headers or {})
        if body is not None and not isinstance(body, bytes):
            body = json.dumps(body).encode()
            outgoing['Content-Type'] = 'application/json'
        conn = connection
        try:
            if conn is None:
                conn = self.connection(timeout)
            else:
                conn.timeout = timeout
                if conn.sock is not None:
                    conn.sock.settimeout(timeout)
            conn.request(method, path, body=body, headers=outgoing)
            response = conn.getresponse()
            if 300 <= response.status < 400:
                response.close()
                raise OpsError('redirect_rejected')
            if response.status in {401, 403}:
                self.block_authentication()
            return conn, response
        except (OpsError, KeyboardInterrupt):
            if conn is not None:
                conn.close()
            raise
        except Exception:
            if conn is not None:
                conn.close()
            raise OpsError('http_transport_failed', category='network') from None

    def request(self, path, method='GET', body=None, *, raw=False, read_only=False, **kwargs):
        with operation(urlsplit(path).path, kind='read' if read_only else 'call'):
            return self._request(path, method, body, raw=raw, read_only=read_only, **kwargs)

    def _request(self, path, method, body, *, raw, read_only, **kwargs):
        with self.lock:
            cached = self.pool.pop() if self.pool else None
        try:
            conn, response = self.open(path, method, body, connection=cached, keep_alive=True, **kwargs)
        except OpsError as error:
            if cached is not None:
                cached.close()
            if cached is None or not read_only or method not in {'GET', 'HEAD'} or error.code != 'http_transport_failed':
                raise
            note('retry', scope='read', code=error.code, attempt=2)
            conn, response = self.open(path, method, body, keep_alive=True, **kwargs)
        reusable = False
        try:
            value = response.read(32 * 1024 * 1024 + 1)
            require(len(value) <= 32 * 1024 * 1024, 'http_response_too_large')
            headers = {key.lower(): value for key, value in response.getheaders()}
            if raw:
                data = value.decode('utf-8', 'replace')
            else:
                try:
                    data = json.loads(value)
                except ValueError:
                    data = None
            if response.status in {401, 403}:
                raise OpsError('management_authentication_failed', category='authorization')
            reusable = not response.will_close
            return {'http': response.status, 'data': data, 'headers': headers}
        except (http.client.HTTPException, OSError):
            raise OpsError('http_transport_failed', category='network') from None
        finally:
            response.close()
            with self.lock:
                if reusable and not self.closed and not self.blocked and len(self.pool) < 4:
                    self.pool.append(conn)
                    conn = None
            if conn is not None:
                conn.close()

    def close(self):
        with self.lock:
            self.closed = True
            clients, self.pool = self.pool, []
        for client in clients:
            client.close()

    def block_authentication(self):
        with self.lock:
            self.blocked = True
            clients, self.pool = self.pool, []
        for client in clients:
            client.close()


class BorrowedSSH:
    def __init__(self, client):
        self.client = client

    def __getattr__(self, name):
        return getattr(self.client, name)

    def close(self):
        pass


class SSHFactory:
    def __init__(self, connections, vault):
        self.connections, self.vault = connections, vault
        self.pool = None
        self.lock = threading.RLock()
        self.profile_locks = {}

    @contextmanager
    def session(self):
        with self.lock:
            require(self.pool is None, 'ssh_session_already_active')
            self.pool = {}
        try:
            yield
        finally:
            with self.lock:
                clients, self.pool = list(self.pool.values()), None
            for client in clients:
                client.close()

    def connect(self, name):
        with self.lock:
            pooled = self.pool is not None
            profile_lock = self.profile_locks.setdefault(name, threading.Lock())
        if not pooled:
            return self._connect(name)
        with profile_lock:
            with self.lock:
                require(self.pool is not None, 'ssh_session_closed')
                current = self.pool.get(name)
            if current is not None and current.get_transport() is not None and current.get_transport().is_active():
                return BorrowedSSH(current)
            if current is not None:
                current.close()
                note('connection_rebuilt', profile=name)
            client = self._connect(name)
            with self.lock:
                self.pool[name] = client
            return BorrowedSSH(client)

    def _connect(self, name):
        import paramiko
        require(name in self.connections, 'ssh_profile_missing')
        config = self.connections[name]
        known = Path(config['known_hosts']).expanduser()
        require(known.is_file(), 'known_hosts_missing')
        client = paramiko.SSHClient()
        client.load_host_keys(str(known))
        client.set_missing_host_key_policy(paramiko.RejectPolicy())
        password = None
        if config.get('credential'):
            record = self.vault.reference(config['credential'], host=config['host'])
            password = record if isinstance(record, str) else record['password']
            if isinstance(record, dict):
                require(record.get('user', config['user']) == config['user']
                        and record.get('port', config['port']) == config['port'], 'ssh_credential_identity_mismatch')
        try:
            client.connect(config['host'], port=config['port'], username=config['user'], password=password,
                           key_filename=config.get('identity'), look_for_keys=False,
                           allow_agent=bool(config.get('agent')), timeout=20, banner_timeout=20, auth_timeout=30)
        except Exception:
            client.close()
            raise OpsError('verified_ssh_connection_failed') from None
        finally:
            password = None
        client.get_transport().sock.settimeout(None)
        return client


def remote_json(ssh, script, spec):
    stdin, stdout, stderr = ssh.exec_command(shlex.join(['python3', '-u', '-c', script]))
    stdin.write(json.dumps(spec) + '\n')
    stdin.flush()
    stdin.channel.shutdown_write()
    # Drain both streams concurrently so a full stderr pipe cannot deadlock a command.
    errors = []
    thread = threading.Thread(target=lambda: errors.append(stderr.read()), daemon=True)
    thread.start()
    data = stdout.read(48 * 1024 * 1024 + 1)
    code = stdout.channel.recv_exit_status()
    thread.join()
    require(len(data) <= 48 * 1024 * 1024, 'remote_response_too_large')
    if code:
        try:
            result = json.loads(data)
            safe_code = result.get('error')
        except ValueError:
            safe_code = None
        if safe_code and __import__('re').fullmatch(r'[a-z_]{1,80}', safe_code):
            raise OpsError('remote_' + safe_code)
        raise OpsError('remote_operation_failed')
    try:
        return json.loads(data)
    except ValueError:
        raise OpsError('remote_response_invalid') from None

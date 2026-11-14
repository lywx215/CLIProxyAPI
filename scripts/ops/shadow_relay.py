"""One bounded HTTPS request to the main origin's verified shadow instance.

The server source runs on SSH stdio, opens no listener, and writes no files.
"""

import http.client
import http.server
import json
import os
from pathlib import Path
import re
import shlex
import signal
import ssl
import sys


HOSTNAME = 'gemini30.pro'
ADDRESS = '127.0.0.1:3003'
ORIGIN = 'https://' + HOSTNAME
CERT_DIRECTORY = '/www/server/panel/vhost/cert/gemini30.pro/'
MAX_BODY = 16 * 1024
MAX_RESPONSE = 4 * 1024 * 1024
NONCE = r'\?_cpa_ops_nonce=[a-f0-9]{32}'


def allowed(path, body):
    if body is None:
        return bool(re.fullmatch(r'/api/(status|channel/[1-9][0-9]*)' + NONCE, path))
    if not re.fullmatch(r'/api/channel/status/batch' + NONCE, path) or not isinstance(body, dict):
        return False
    ids = body.get('ids')
    return (set(body) == {'ids', 'status'} and type(body['status']) is int and body['status'] in {1, 2}
            and isinstance(ids, list) and 0 < len(ids) <= 500
            and all(type(value) is int and value > 0 for value in ids) and len(ids) == len(set(ids)))


def serve(spec):
    path, body = spec['path'], spec['body']
    if not allowed(path, body):
        raise ValueError('shadow_request_outside_scope')
    method = 'GET' if body is None else 'POST'
    signal.alarm(45)
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    context.minimum_version = ssl.TLSVersion.TLSv1_2
    context.load_cert_chain(CERT_DIRECTORY + 'fullchain.pem', CERT_DIRECTORY + 'privkey.pem')

    def verify_sni(_stream, name, _context):
        if name != HOSTNAME:
            return ssl.ALERT_DESCRIPTION_UNRECOGNIZED_NAME
        return None

    context.set_servername_callback(verify_sni)

    def write_all(value):
        value = memoryview(value)
        while value:
            value = value[os.write(1, value):]

    class Handler(http.server.BaseHTTPRequestHandler):
        def log_message(self, *_args):
            pass

        def relay(self):
            if (self.command != method or self.path != path or self.headers.get_all('Host') != [HOSTNAME]
                    or self.headers.get('Transfer-Encoding')):
                self.send_error(400)
                return
            sizes = self.headers.get_all('Content-Length', [])
            if (body is None and sizes) or (body is not None and (len(sizes) != 1 or not sizes[0].isdigit()
                                                                  or not 0 < int(sizes[0]) <= MAX_BODY)):
                self.send_error(400)
                return
            raw = self.rfile.read(int(sizes[0])) if sizes else None
            if raw is not None and json.loads(raw) != body:
                self.send_error(400)
                return
            headers = {'Host': HOSTNAME, 'Connection': 'close', 'Accept': 'application/json', 'Cache-Control': 'no-store'}
            if path.startswith('/api/channel/'):
                for key in ('Authorization', 'New-Api-User'):
                    values = self.headers.get_all(key)
                    if not values or len(values) != 1:
                        self.send_error(400)
                        return
                    headers[key] = values[0]
            if raw is not None:
                headers['Content-Type'] = 'application/json'
            conn = http.client.HTTPConnection('127.0.0.1', 3003, timeout=25)
            try:
                conn.request(method, path, body=raw, headers=headers)
                upstream = conn.getresponse()
                data = upstream.read(MAX_RESPONSE + 1)
                if len(data) > MAX_RESPONSE or 300 <= upstream.status < 400:
                    self.send_error(502)
                    return
                self.send_response(upstream.status)
                self.send_header('Content-Type', 'application/json')
                self.send_header('Content-Length', str(len(data)))
                self.send_header('Connection', 'close')
                self.end_headers()
                self.wfile.write(data)
            finally:
                conn.close()

        do_GET = do_POST = relay

    pipe = TLSPipe(lambda size: os.read(0, size), write_all, context, server_side=True)
    Handler(pipe, ('127.0.0.1', 0), None)


def request(factory, host, path, *, body=None, credential=None, user=None):
    """Authenticate the exact origin before writing headers into the TLS pipe."""
    from .core import OpsError, require
    from .tls_pipe import TLSPipe

    require(allowed(path, body), 'shadow_request_outside_scope')
    require(not path.startswith('/api/channel/') or (credential and type(user) is int), 'shadow_authorization_missing')
    source = Path(__file__).with_name('tls_pipe.py').read_text(encoding='utf-8') + '\n' + Path(__file__).read_text(encoding='utf-8')
    spec = json.dumps({'path': path, 'body': body}, separators=(',', ':'))
    client = factory.connect(host)
    connection = http.client.HTTPConnection(HOSTNAME, timeout=35)
    try:
        # Retain stdin for the lifetime of the request; dropping it half-closes SSH.
        stdin, stdout, stderr = client.exec_command(shlex.join(['python3', '-u', '-c', source, spec]), timeout=35)
        channel = stdout.channel
        context = ssl.create_default_context()
        connection.sock = TLSPipe(channel.recv, channel.sendall, context, hostname=HOSTNAME)
        headers = {'Host': HOSTNAME, 'Connection': 'close', 'Accept': 'application/json'}
        if path.startswith('/api/channel/'):
            headers.update({'Authorization': 'Bearer ' + credential, 'New-Api-User': str(user)})
        if body is not None:
            headers['Content-Type'] = 'application/json'
        connection.request('GET' if body is None else 'POST', path,
                           body=None if body is None else json.dumps(body).encode(), headers=headers)
        response = connection.getresponse()
        require(not 300 <= response.status < 400, 'shadow_redirect_rejected')
        require(response.status not in {401, 403}, 'shadow_authentication_failed', category='authorization')
        require(response.status == 200, 'shadow_http_not_ok')
        raw = response.read(MAX_RESPONSE + 1)
        require(len(raw) <= MAX_RESPONSE, 'shadow_response_too_large')
        value = json.loads(raw)
        require(isinstance(value, dict) and value.get('success') is True, 'shadow_operation_not_confirmed')
        return value
    except OpsError:
        raise
    except ssl.SSLCertVerificationError:
        raise OpsError('shadow_origin_certificate_unverified') from None
    except (OSError, EOFError, ValueError, http.client.HTTPException):
        raise OpsError('shadow_transport_failed', category='network') from None
    finally:
        connection.close()
        client.close()


def wait_for_sync(factory, host, proof):
    """Observe a fresh cache rebuild after the site's status writes returned."""
    from .core import require
    from .transport import remote_json

    source = r'''
import json,subprocess,sys,time
s=json.load(sys.stdin)
def inspect():
    p=subprocess.run(['docker','inspect','--format','{{.Id}} {{.State.Running}}','newapi-slave1-shadow'],capture_output=True,text=True,timeout=10)
    return p.returncode == 0 and p.stdout.strip() == s['container_id']+' true'
if not inspect(): raise RuntimeError('shadow_process_changed')
since=str(int(time.time())+1)
deadline=time.monotonic()+2*s['sync_seconds']+15
while time.monotonic()<deadline:
    p=subprocess.run(['docker','logs','--timestamps','--since',since,'--tail','12000','newapi-slave1-shadow'],capture_output=True,text=True,timeout=10)
    if p.returncode: raise RuntimeError('shadow_sync_log_unavailable')
    start=None
    for line in p.stdout.splitlines():
        if ' | syncing channels from database' in line: start=line.split()[0]
        if start and ' | channels synced from database' in line:
            if not inspect(): raise RuntimeError('shadow_process_changed')
            print(json.dumps({'container_id':s['container_id'],'sync_started_at':start,'sync_finished_at':line.split()[0],'after_status_writes':True}))
            sys.exit(0)
    time.sleep(2)
raise RuntimeError('shadow_sync_not_observed')
'''
    client = factory.connect(host)
    try:
        value = remote_json(client, source, proof)
        require(value.get('container_id') == proof['container_id'] and value.get('after_status_writes') is True,
                'shadow_sync_not_observed')
        return value
    finally:
        client.close()


if __name__ == '__main__':
    try:
        serve(json.loads(sys.argv[1]))
    except Exception:
        sys.exit(2)

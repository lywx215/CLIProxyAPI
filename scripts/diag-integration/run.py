"""Six real processes, synthetic caller/provider/browser, frozen source verification.

Exit 0: executed assertions passed AND no required gaps; 1: assertion failure;
2: setup/preflight failure; 3: executed assertions passed, acceptance gaps remain.
All artifacts are for synthetic traffic. Raw process output stays in a new temp root.
"""
import argparse
import concurrent.futures
import hashlib
import http.client
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import queue
import socket
import subprocess
import tempfile
import threading
import time
from urllib.parse import urlsplit

BASE = '9422a853a222aef0dbf67815888c53ef6f1ede77'
GCLI = '0b3a07e003ead2ba7a9f7827426c09f8ff996813'
AITO = 'a2d51383bc91751f23bc0f8927c19ef736593cea'
CONTRACT = 'ddb202238cfdaabef1af11575dbfcac788fdbc5a457aea5e72b913afc5b478d4'
REPO = Path(__file__).resolve().parents[2]
HERE = Path(__file__).resolve().parent
CREATE_FLAGS = subprocess.CREATE_NO_WINDOW if os.name == 'nt' else 0


def sha(data):
    return hashlib.sha256(data).hexdigest()


def write_json(path, value):
    path.write_text(json.dumps(value, ensure_ascii=False, indent=2)+'\n', encoding='utf-8')


def responses_equivalent(on, off):
    def stable(body):
        value = json.loads(body)
        for key in ('id', 'created'): value.pop(key, None)
        return value
    return on['status'] == off['status'] and stable(on['body']) == stable(off['body'])


def git(root, *args):
    return subprocess.check_output(['git', '-C', str(root), *args], creationflags=CREATE_FLAGS).decode().strip()


def binary_identity(path, attestation=None):
    # go version -m reads the binary; it does not execute the target program.
    metadata = subprocess.run(['go', 'version', '-m', str(path)], capture_output=True, creationflags=CREATE_FLAGS)
    lines = metadata.stdout.decode('utf-8', errors='replace').splitlines()
    identity = {'file': path.name, 'sha256': sha(path.read_bytes()),
                'embeddedBuildMetadata': lines[1:], 'metadataReadExitCode': metadata.returncode,
                'buildSource': None, 'qualification': 'Unknown unless a matching build attestation is supplied; invocation HEAD is not build HEAD.'}
    if attestation:
        if not attestation.get('baseRevision') or not attestation.get('sourceQualification'):
            raise RuntimeError('binary build attestation must identify source revision and qualification')
        if attestation['sha256'] != identity['sha256']:
            raise RuntimeError('binary build attestation digest mismatch')
        identity['buildSource'] = attestation
        identity['qualification'] = 'Hash-matched operator build attestation; embedded metadata is recorded separately.'
    return identity


def verify(root, revision, external=True):
    head = git(root, 'rev-parse', 'HEAD')
    if external and (head != revision or git(root, 'status', '--porcelain', '--untracked-files=normal')):
        raise RuntimeError('frozen dependency SHA/clean check failed')
    if not external:
        subprocess.run(['git', '-C', str(root), 'merge-base', '--is-ancestor', revision, 'HEAD'], check=True, creationflags=CREATE_FLAGS)
        changed = git(root, 'diff', '--name-only', revision).splitlines()
        allowed = ('cmd/diag-integration/', 'scripts/diag-integration/', 'coordination/diagnostics/20260924/DIAG-07-')
        if any(not name.startswith(allowed) for name in changed):
            raise RuntimeError('existing production source changed')
    contract = root/'contracts/diagnostics/v1'
    manifest = (contract/'SHA256SUMS').read_bytes()
    if sha(manifest) != CONTRACT:
        raise RuntimeError('contract manifest digest differs')
    members = {'SHA256SUMS'}
    for line in manifest.decode().splitlines():
        digest, relative = line.split('  ', 1)
        members.add(relative)
        if sha((contract/relative).read_bytes()) != digest:
            raise RuntimeError('contract byte mismatch')
    if {p.relative_to(contract).as_posix() for p in contract.rglob('*') if p.is_file()} != members or len(members) != 73:
        raise RuntimeError('contract member set differs')
    return {'head': head, 'contractSha256': CONTRACT, 'contractFiles': len(members), 'trackedProductionClean': True}


def environment(cwd, instance, debug=True):
    # Start from an allowlist, never inherit user service configuration/secrets.
    env = {key: os.environ[key] for key in ('SystemRoot', 'WINDIR', 'PATH', 'COMSPEC', 'PATHEXT') if key in os.environ}
    env.update(TEMP=str(cwd), TMP=str(cwd), HOME=str(cwd), USERPROFILE=str(cwd),
               PYTHONDONTWRITEBYTECODE='1', PYTHONUNBUFFERED='1', NO_PROXY='*',
               DIAG_ENVIRONMENT='test', DIAG_DEPLOYMENT_ID='diag07-local', DIAG_INSTANCE_ID=instance,
               LOG_LEVEL='DEBUG' if debug else 'INFO', ACCESS_LOG_ENABLED='true')
    return env


def request(address, path, payload=None, headers=(), cancel=False):
    u = urlsplit(address)
    if u.hostname != '127.0.0.1':
        raise RuntimeError('caller destination denied')
    c = http.client.HTTPConnection('127.0.0.1', u.port)
    data = json.dumps(payload).encode() if payload is not None else b''
    start = time.monotonic()
    c.putrequest('POST' if payload is not None else 'GET', path)
    c.putheader('Content-Type', 'application/json')
    c.putheader('Content-Length', str(len(data)))
    c.putheader('Authorization', 'Bearer synthetic-local-password')
    for k, v in headers:
        c.putheader(k, v)
    c.endheaders(data)
    r = c.getresponse()
    committed = (time.monotonic()-start)*1000
    read_error = None
    try:
        body = r.read(1) if cancel else r.read()
    except http.client.IncompleteRead as error:
        body = error.partial
        read_error = 'incomplete_read'
    finally:
        c.close()
    return {'status': r.status, 'headers': {k.lower(): v for k, v in r.getheaders() if k.lower().startswith('x-diag-')},
            'body': body.decode('utf-8', errors='replace'), 'headersMs': committed,
            'completeMs': (time.monotonic()-start)*1000, 'cancelledAfterByte': cancel, 'readError': read_error}


class Provider:
    def __init__(self, label):
        self.label, self.calls = label, []
        self.lock = threading.Lock()
        self.release = threading.Event()
        self.entered = threading.Event()
        provider = self

        class Handler(BaseHTTPRequestHandler):
            protocol_version = 'HTTP/1.1'
            def log_message(self, *args):
                pass

            def do_POST(self):
                raw = self.rfile.read(int(self.headers['Content-Length']))
                obj = json.loads(raw)
                encoded = json.dumps(obj)
                scenario = next((s for s in ('anti_nested','nested', 'retry', 'readerror', 'errorframe', 'incomplete', 'blocked', 'missing', 'thought13', 'tail13', 'zero', 'tool', 'media', 'empty', 'hold', 'clean400') if 'case-'+s in encoded), 'success')
                with provider.lock:
                    n = sum(c['scenario'] == scenario for c in provider.calls)+1
                    provider.calls.append({'scenario': scenario, 'ordinal': n, 'path': self.path, 'request': obj,
                                           'traceparent': self.headers.get('traceparent'), 'provider': provider.label})
                contents = obj.get('request',{}).get('contents')
                if not contents:
                    self.send_json({'error': {'code':400,'message':'synthetic empty contents rejected'}},400)
                    return
                if scenario == 'clean400' and obj.get('model') == 'gemini-3.7-flash' and contents[-1].get('role') == 'model':
                    self.send_json({'error': {'code':400,'message':'synthetic prefill rejected'}},400)
                    return
                if scenario == 'nested' or (scenario == 'retry' and n <= 2) or (scenario=='anti_nested' and n%2==1):
                    self.send_json({'error': {'code': 429 if provider.label == 'p1' else 503, 'message': 'synthetic retry'}}, 429 if provider.label == 'p1' else 503)
                    return
                parts = [{'text': 'synthetic answer'}]
                if scenario=='anti_nested' and n>=4: parts=[{'text':'synthetic continuation\n[done]'}]
                if scenario == 'tool': parts = [{'functionCall': {'name': 'synthetic_tool', 'args': {}}}]
                if scenario == 'media': parts = [{'inlineData': {'mimeType': 'image/png', 'data': 'AA=='}}]
                if scenario == 'empty': parts = [{'text': '   '}]
                candidate = {'content': {'role': 'model', 'parts': parts}}
                if scenario != 'incomplete': candidate['finishReason'] = 'SAFETY' if scenario == 'blocked' else 'STOP'
                usage = {'promptTokenCount': 10, 'candidatesTokenCount': 0 if scenario == 'zero' else 87,
                         'thoughtsTokenCount': 13 if scenario in ('thought13','tail13') else 2}
                first = {'response': {'candidates': [candidate]}}
                if scenario=='thought13': first['response']['usageMetadata']=usage
                if 'streamGenerateContent' not in self.path:
                    if scenario != 'missing': first['response']['usageMetadata'] = usage
                    self.send_json(first)
                    return
                self.send_response(200)
                self.send_header('Content-Type', 'text/event-stream')
                self.send_header('Transfer-Encoding', 'chunked')
                self.end_headers()
                try:
                    self.chunk(first)
                    if scenario == 'hold':
                        provider.entered.set()
                        provider.release.wait()
                    if scenario == 'readerror':
                        self.close_connection = True
                        self.connection.shutdown(socket.SHUT_RDWR)
                        return
                    if scenario == 'errorframe': self.chunk({'error': {'code': 503, 'message': 'synthetic frame'}})
                    elif scenario != 'missing':
                        self.chunk({'response': {'usageMetadata': usage}})
                        self.chunk({'response': {'usageMetadata': usage}})
                    if scenario != 'incomplete': self.chunk('[DONE]')
                    self.wfile.write(b'0\r\n\r\n')
                    self.wfile.flush()
                except (BrokenPipeError, ConnectionResetError, ConnectionAbortedError):
                    pass

            def chunk(self, value):
                data = ('data: '+(value if isinstance(value, str) else json.dumps(value))+'\n\n').encode()
                self.wfile.write(f'{len(data):x}\r\n'.encode()+data+b'\r\n')
                self.wfile.flush()

            def send_json(self, obj, status=200):
                data = json.dumps(obj).encode()
                self.send_response(status)
                self.send_header('Content-Type', 'application/json')
                self.send_header('Content-Length', str(len(data)))
                self.end_headers()
                self.wfile.write(data)

        self.server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.address = 'http://127.0.0.1:'+str(self.server.server_port)

    def close(self):
        self.release.set()
        self.server.shutdown()
        self.server.server_close()


class Process:
    def __init__(self, label, argv, cwd, env):
        self.label, self.cwd = label, cwd
        self.messages = queue.Queue()
        self.raw = cwd/'stdout.txt'
        self.proc = subprocess.Popen([str(x) for x in argv], cwd=cwd, env=env, stdin=subprocess.PIPE,
                                     stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, encoding='utf-8', errors='replace', creationflags=CREATE_FLAGS)
        def drain():
            with self.raw.open('w', encoding='utf-8') as f:
                for line in self.proc.stdout:
                    f.write(line)
                    f.flush()
                    try:
                        obj = json.loads(line)
                        if isinstance(obj, dict): self.messages.put(obj)
                    except ValueError:
                        pass
        self.thread = threading.Thread(target=drain, daemon=True)
        self.thread.start()
        self.address, self.root = None, cwd

    def ready(self):
        deadline = time.monotonic()+60
        while time.monotonic() < deadline:
            if self.proc.poll() is not None: raise RuntimeError(self.label+' exited before readiness')
            try:
                value = self.messages.get(timeout=.2)
                if value.get('address'):
                    self.address = value['address']
                    if value.get('root'): self.root = Path(value['root'])
                    return
            except queue.Empty:
                pass
        raise RuntimeError(self.label+' readiness deadline')

    def command(self, value):
        self.proc.stdin.write(json.dumps(value)+'\n')
        self.proc.stdin.flush()
        return self.messages.get(timeout=30)

    def close(self, hard=False):
        result = {'label': self.label, 'pid': self.proc.pid, 'hardKill': hard}
        if self.proc.poll() is None:
            if hard:
                self.proc.kill()
            elif self.label.startswith('aito'):
                result['ack'] = self.command({'op': 'close'})
            else:
                result['response'] = request(self.address, '/__shutdown', {})['status']
        try:
            result['exitCode'] = self.proc.wait(timeout=45)
        except subprocess.TimeoutExpired:
            result['forcedAfterFailedClose'] = True
            self.proc.kill()
            result['exitCode'] = self.proc.wait()
        self.thread.join()
        return result

    def export(self, output):
        if self.label.startswith('gcli'):
            paths = list((self.cwd/'state').glob('*.diag.*.jsonl'))
        elif self.label.startswith('aito'):
            paths = [self.root/'diagnostics.jsonl']
        else:
            paths = [self.raw]
        data = b''
        for path in paths:
            for line in path.read_bytes().splitlines(keepends=True):
                if line.startswith(b'@diag '): data += line[6:]
                elif self.label.startswith(('gcli', 'aito')): data += line
        target = output/(self.label+'.jsonl')
        target.write_bytes(data)
        return {'file': target.name, 'bytes': len(data), 'sha256': sha(data), 'lines': len(data.splitlines())}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--gcli', type=Path, required=True)
    parser.add_argument('--aito', type=Path, required=True)
    parser.add_argument('--python', type=Path, required=True)
    parser.add_argument('--node', type=Path, required=True)
    parser.add_argument('--driver', type=Path, required=True)
    parser.add_argument('--analyzer', type=Path, required=True)
    parser.add_argument('--binary-provenance', type=Path, required=True, help='JSON driver/analyzer build attestations, each bound to the actual binary sha256')
    parser.add_argument('--qa-deps', type=Path)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--downstream-only', action='store_true', help='run independent gcli/Aito coverage; explicitly leaves six-instance acceptance incomplete')
    args = parser.parse_args()
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=False)
    root = Path(tempfile.mkdtemp(prefix='diag07-'))
    # The absolute root is local-only; published artifacts use instance labels.
    print('Local scratch:', root, flush=True)
    processes, providers, matrix, closes, exports = [], [], [], [], []
    status = 2
    try:
        revisions = {'cpa': verify(REPO, BASE, False), 'gcli': verify(args.gcli, GCLI), 'aito': verify(args.aito, AITO)}
        provenance = json.loads(args.binary_provenance.read_text(encoding='utf-8'))
        revisions['binaries'] = {key: binary_identity(path, provenance[key]) for key, path in (('driver', args.driver), ('analyzer', args.analyzer))}
        revisions['supportSources'] = {p.name: sha(p.read_bytes()) for p in sorted(HERE.iterdir()) if p.suffix in ('.py', '.cjs')}
        write_json(output/'revisions.json', revisions)
        write_json(output/'run-mode.json', {'mode': 'downstream-only' if args.downstream_only else 'full', 'requiredServices': [] if args.downstream_only else ['gcli2api', 'aitoapi']})
        if not subprocess.check_output([args.node, '--version'], creationflags=CREATE_FLAGS).decode().startswith('v24.'):
            raise RuntimeError('Node24 required')
        providers = [Provider('p1'), Provider('p2')]

        def launch(kind, index, debug=True, throttle=False, instance=None):
            label = f'{kind}{index}'
            cwd = root/label
            cwd.mkdir()
            env = environment(cwd, instance or label, debug)
            if kind == 'gcli':
                verify(args.gcli, GCLI)
                if args.qa_deps: env['PYTHONPATH'] = str(args.qa_deps)
                with socket.socket() as s:
                    s.bind(('127.0.0.1', 0))
                    port = s.getsockname()[1]
                argv = [args.python, HERE/'gcli-wrapper.py', args.gcli, 'gcli', '--root', cwd/'state', '--port', str(port), '--upstream', providers[(index-1)%2].address]
                if debug: argv += ['--debug']
                if index == 2: argv += ['--nonstream-upstream']
            elif kind == 'aito':
                verify(args.aito, AITO)
                argv = [args.node, HERE/'aito-wrapper.cjs', args.aito]
            else:
                verify(REPO, BASE, False)
                targets = gclis if index in (1,3) else aitos
                upstreams = [p.address+('/antigravity' if index in (1,3) else '') for p in targets]
                service = 'gcli2api' if index in (1,3) else 'aitoapi'
                env['DIAG_PEERS'] = json.dumps([{'alias': p.label, 'origin': p.address, 'pathPrefix': '/', 'service': service, 'deploymentId': 'diag07-local'} for p in targets])
                models = ['gemini-2.5-flash'] if index in (1,3) else ['diag-'+s for s in ('usage87','reasoning','missing','zero','empty','blocked','tool','retry','http429','http503','network','truncated','slow','thought')]
                argv = [args.driver, '--upstreams', ','.join(upstreams), '--models', ','.join(models)]
                if debug: argv += ['--debug']
                if throttle: argv += ['--throttle']
                if index == 3: argv += ['--rate','100','--first-delay','10']
            p = Process(label, argv, cwd, env)
            if kind == 'cpa':
                p.expected_peers = {target.label: {'service': service, 'deploymentId': 'diag07-local', 'environment': 'test', 'instanceId': target.label} for target in targets}
            processes.append(p)
            if kind == 'gcli':
                p.address = f'http://127.0.0.1:{port}'
                deadline = time.monotonic()+60
                while True:
                    try:
                        if request(p.address, '/__health')['status'] == 200: break
                    except OSError:
                        if p.proc.poll() is not None or time.monotonic() > deadline: raise RuntimeError(label+' readiness failed')
                        time.sleep(.1)
            else:
                p.ready()
            return p

        gclis = [launch('gcli', i) for i in (1,2)]
        aitos = [launch('aito', i) for i in (1,2)]
        cpas = [] if args.downstream_only else [launch('cpa', 1), launch('cpa', 2, throttle=True)]
        write_json(output/'process-overlap.json', {'sixInstances':len(processes)>=6,'processes':[{'label': p.label, 'pid': p.proc.pid, 'port': urlsplit(p.address).port, 'alive': p.proc.poll() is None} for p in processes]})
        if not all(p.proc.poll() is None for p in processes): raise RuntimeError('process overlap failed')

        def case(name, p, scenario='success', stream=False, headers=(), contents=None, expected=(200,), cancel=False, native=False, model_override=None):
            is_aito = p.label.startswith('aito') or p.label == 'cpa2'
            model = 'diag-'+('usage87' if scenario == 'success' else scenario) if is_aito else 'gemini-2.5-flash'
            if model_override: model=model_override
            path = ('/antigravity' if p.label.startswith('gcli') else '')+'/v1/chat/completions'
            payload = {'model': model, 'messages': [{'role': 'user', 'content': 'synthetic case-'+scenario}], 'stream': stream}
            if contents is not None or native:
                path = ('/antigravity' if p.label.startswith('gcli') else '')+'/v1beta/models/'+model+(':'+('streamGenerateContent?alt=sse' if stream else 'generateContent'))
                payload = {'contents': contents if contents is not None else [{'role':'user','parts':[{'text':'synthetic case-'+scenario}]}]}
            result = request(p.address, path, payload, headers, cancel)
            entry = {'id': name, 'level': 'live', 'instance': p.label, 'scenario': scenario, 'stream': stream,
                     'expectedStatuses': list(expected), 'pass': result['status'] in expected, **result}
            if p.label.startswith('cpa'):
                entry.update(expectedPeers=p.expected_peers, graphPolicy='cancellation' if cancel else 'complete')
            matrix.append(entry)
            write_json(output/'requests.json', matrix)
            print(name, result['status'], flush=True)
            return entry

        for p in gclis+aitos+cpas:
            for stream in (False, True):
                case('topology-'+p.label+('-stream' if stream else '-nonstream'), p, stream=stream)
                case('native-'+p.label+('-stream' if stream else '-nonstream'), p, stream=stream,native=True)
        tp = '00-'+'1'*32+'-'+'2'*16+'-01'
        for p in (gclis[0], aitos[0], *cpas):
            for name, h in [('request-only',[('X-Request-Id','shared-caller')]), ('standard',[('traceparent',tp)]), ('invalid',[('traceparent','bad')]), ('duplicate',[('traceparent',tp),('Traceparent',tp)]), ('reuse-a',[('X-Request-Id','shared-caller'),('traceparent',tp)]), ('reuse-b',[('X-Request-Id','shared-caller'),('traceparent',tp)]), ('duplicate-id',[('X-Request-Id','a'),('x-request-id','b')])]:
                case('headers-'+p.label+'-'+name, p, headers=h)
        for p in (gclis[0], *cpas[:1]):
            for scenario in ('thought13','tail13','zero','missing','tool','media','empty','blocked','incomplete','errorframe','readerror','retry','nested'):
                case('semantics-'+p.label+'-'+scenario, p, scenario, stream=True, expected=(200,429,503,500))
        for p in (aitos[0], *cpas[1:2]):
            for scenario in ('reasoning','zero','missing','tool','empty','blocked','truncated','retry','http429','http503','network','thought'):
                case('semantics-'+p.label+'-'+scenario, p, scenario, stream=True, expected=(200,429,503,502,504,500))
        case('semantics-gcli1-anti_nested',gclis[0],'anti_nested',stream=True,model_override='抗截断/gemini-3.7-flash')
        def message(role, text): return {'role': role, 'parts': [{'text': text}]}
        cleaning = {
            'trailing-model': [message('user','case-clean400'),message('model','synthetic'),message('user','')],
            'multiple-empty': [message('user','case-clean400'),message('user',''),message('model',''),message('user','  ')],
            'whitespace': [message('user','case-clean400'),message('model',' \n '),message('user','synthetic')],
            'tool': [message('user','case-clean400'),{'role':'model','parts':[{'functionCall':{'name':'synthetic_tool','args':{}}}]},{'role':'user','parts':[{'functionResponse':{'name':'synthetic_tool','response':{'value':1}}}]}],
            'all-empty': [message('user',''),message('model','  ')]}
        for name, contents in cleaning.items():
            case('cleaning-'+name, gclis[0], contents=contents, model_override='gemini-3.7-flash',expected=(400,) if name=='all-empty' else (200,))
        # Compare stable response fields separately from generated protocol IDs/time.
        for p in (aitos[0], *cpas):
            on = case('debug-on-'+p.label, p)
            if p.label.startswith('aito'): p.command({'op':'debug','enabled':False})
            else: request(p.address,'/__debug/off',{})
            off = case('debug-off-'+p.label,p)
            if p.label.startswith('aito'): p.command({'op':'debug','enabled':True})
            else: request(p.address,'/__debug/on',{})
            matrix.append({'id':'equivalence-'+p.label,'level':'live','pass':responses_equivalent(on,off), 'evidence':[on['id'],off['id']], 'scope':'equal HTTP status and response JSON excluding generated id/created'})
        # Cancellation and DEBUG interruption are held by a provider handshake.
        if cpas:
          with concurrent.futures.ThreadPoolExecutor() as pool:
            for provider in providers:
                provider.release.clear(); provider.entered.clear()
            future = pool.submit(case, 'dynamic-debug-cpa1', cpas[0], 'hold', True)
            while not any(p.entered.wait(.05) for p in providers):
                if future.done(): raise RuntimeError('hold fixture returned before handshake')
            request(cpas[0].address,'/__debug/off',{})
            for p in providers: p.release.set()
            future.result()
            request(cpas[0].address,'/__debug/on',{})
        for p in (gclis[0], *cpas[:1]):
            for provider in providers:
                provider.release.clear(); provider.entered.clear()
            case('cancel-'+p.label,p,'hold',stream=True,cancel=True)
            for provider in providers: provider.release.set()
        case('cancel-aito1',aitos[0],'slow',stream=True,cancel=True)
        if cpas:
            throttle_cpa = launch('cpa',3,throttle=True)
            case('throttle-collected-gcli',throttle_cpa,native=True)
            case('throttle-native-aito',cpas[1],native=True)
        quiet_gcli = launch('gcli',3,debug=False)
        case('debug-off-gcli',quiet_gcli)
        aitos[0].command({'op':'reconnect'})
        case('aito-browser-reconnect',aitos[0])
        # Restart with the same configured instance, a fresh cwd and a fresh boot.
        closes.append(aitos[1].close())
        exports.append(aitos[1].export(output))
        restarted = launch('aito',3,instance='aito2')
        case('aito-restart',restarted)
        # Same-instance multiple processes are worker evidence, never POSIX fork.
        worker = launch('aito',4,instance='aito1')
        case('aito-multiworker',worker)
        gcli_worker = launch('gcli',4,instance='gcli1')
        case('gcli-multiworker',gcli_worker)
        closes.append(gclis[1].close())
        exports.append(gclis[1].export(output))
        gcli_restart = launch('gcli',5,instance='gcli2')
        case('gcli-restart',gcli_restart)
        # Observe a dispatch count before killing; this does not prove the
        # response is still active or identify when unflushed records vanished.
        conn = http.client.HTTPConnection('127.0.0.1',urlsplit(worker.address).port)
        conn.request('POST','/v1/chat/completions',json.dumps({'model':'diag-slow','messages':[{'role':'user','content':'synthetic'}],'stream':False}),{'Content-Type':'application/json','X-Request-Id':'hard-kill-dispatch-observed'})
        snapshot = worker.command({'op':'snapshot'})
        for _ in range(100):
            if snapshot['dispatches'] >= 2: break
            snapshot = worker.command({'op':'snapshot'})
        matrix.append({'id':'hard-kill-dispatch-observed','level':'live','pass':snapshot['dispatches']>=2,'snapshot':snapshot,'evidence':'aito4.jsonl','limit':'dispatch count observed before kill; response activity at kill is unproven; unflushed events may be wholly absent'})
        closes.append(worker.close(hard=True))
        conn.close()
        exports.append({**worker.export(output), 'knownLoss':True})
        status = 3 if all(item['pass'] for item in matrix) else 1
    except Exception as error:
        write_json(output/'failure.json', {'type': type(error).__name__, 'winerror':getattr(error,'winerror',None), 'reason': str(error) if isinstance(error, RuntimeError) else 'see local scratch for setup failure'})
        print('SETUP/EXECUTION FAILURE', type(error).__name__, str(error), flush=True)
    finally:
        for provider in providers: provider.release.set()
        for p in reversed(processes):
            if not any(c['label']==p.label for c in closes):
                try: closes.append(p.close())
                except Exception as error:
                    if p.proc.poll() is None: p.proc.kill(); p.proc.wait()
                    closes.append({'label':p.label,'forcedAfterFailedClose':True,'error':type(error).__name__})
            if not any(e['file']==p.label+'.jsonl' for e in exports):
                try: exports.append(p.export(output))
                except OSError: exports.append({'file':p.label+'.jsonl','missing':True})
        for provider in providers:
            write_json(output/(provider.label+'-requests.json'),provider.calls)
            provider.close()
        write_json(output/'close.json',closes)
        write_json(output/'exports.json',exports)
        write_json(output/'requests.json',matrix)
        if any(c.get('forcedAfterFailedClose') for c in closes): status=1
        if exports:
            from bilateral import analyze_scopes
            if any(e.get('missing') for e in exports): status=1
            scopes = analyze_scopes(args.analyzer, [dict(e, alias=Path(e['file']).stem, path=output/e['file'], trusted=True) for e in exports if not e.get('missing')], output)
            if any(s['exitCode'] != 0 for s in scopes): status=1
        if status in (1,3) and matrix:
            from check_evidence import check_run
            semantic=check_run(output, full=not args.downstream_only)
            if semantic['failed']: status=1
        write_json(output/'result.json',{'exitCode':status,'acceptanceComplete':False,'liveAssertions':len(matrix), 'failedAssertions':[x['id'] for x in matrix if not x['pass']], 'requiredGaps':['POSIX fork and container multiworker','CGO race','actual historical service rolling version','full deterministic end-to-end backpressure/proxy equivalence','see matrix report for remaining component-only and unexecuted items']})
    return status


if __name__ == '__main__':
    raise SystemExit(main())

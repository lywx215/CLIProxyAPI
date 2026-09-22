import argparse, json, os, socket, subprocess, time, hashlib, tempfile, shutil
from datetime import datetime, timezone
from pathlib import Path
from urllib.request import Request, urlopen
from urllib.error import HTTPError, URLError

root = Path(__file__).resolve().parents[2]
parser = argparse.ArgumentParser()
group = parser.add_mutually_exclusive_group(required=True)
group.add_argument('--binary')
group.add_argument('--image')
args = parser.parse_args()
runtime_dir = tempfile.TemporaryDirectory(prefix='cliproxy-release-smoke-')
runtime = Path(runtime_dir.name)
management_path = Path(os.environ.get('SMOKE_MANAGEMENT_PATH', str(root/'static/management.html')))
source_page = management_path
management_path = runtime/'management.html'
shutil.copyfile(source_page, management_path)
runtime.mkdir(exist_ok=True)
(runtime/'auths').mkdir(exist_ok=True)
sock=socket.socket(); sock.bind(('127.0.0.1',0)); port=sock.getsockname()[1]; sock.close()
key='isolated-sync-management-key'
config=f'''host: 127.0.0.1
port: {port}
auth-dir: {runtime.as_posix()}/auths
remote-management:
  allow-remote: false
  secret-key: {key}
  disable-auto-update-panel: true
api-keys: [isolated-sync-client-key]
usage-statistics-enabled: true
logging-to-file: false
plugins:
  enabled: false
api-key-rate-limit:
  enabled: false
  default-rpm: 60
speed-throttle:
  enabled: false
  min-tokens-per-second: 70
  max-tokens-per-second: 100
enable-gemini-cli-endpoint: false
quota-exceeded:
  antigravity-credits: false
  antigravity-credits-force: false
antigravity:
  sensitive-words: [original]
  connection-pool:
    enabled: false
    idle-conn-timeout: 30s
codex:
  response-steering: false
  stream-bootstrap-timeout: 30s
  identity-confuse: true
  orphan-delegation-compatibility: true
routing:
  session-affinity-subagents: false
'''
(runtime/'isolated.yaml').write_text(config,encoding='utf-8')
env={k:v for k,v in os.environ.items() if k.upper() in {'SYSTEMROOT','WINDIR','PATH','TEMP','TMP','COMSPEC','PATHEXT','SYSTEMDRIVE','USERPROFILE','APPDATA','LOCALAPPDATA'}}
env['MANAGEMENT_STATIC_PATH']=str(management_path)
env['GIN_MODE']='release'
log=open(runtime/'server.log','wb')
command = [str(Path(args.binary).resolve()), '--config', str(runtime/'isolated.yaml'), '--local-model'] if args.binary else [
    'docker', 'run', '--rm', '--network', 'host', '--name', 'cliproxy-smoke-'+str(port),
    '-v', f'{runtime}:{runtime}', '-e', 'MANAGEMENT_STATIC_PATH=/CLIProxyAPI/static/management.html',
    '--entrypoint', '/CLIProxyAPI/CLIProxyAPI', args.image,
    '--config', str(runtime/'isolated.yaml'), '--local-model']
proc=subprocess.Popen(command,cwd=runtime,env=env,stdout=log,stderr=subprocess.STDOUT,creationflags=getattr(subprocess,'CREATE_NO_WINDOW',0))
results=[]
base=f'http://127.0.0.1:{port}'
def request(path,method='GET',body=None,content_type='application/json',expected=200,auth=True):
    headers={'Content-Type':content_type}
    if auth: headers['Authorization']='Bearer '+key
    if isinstance(body,(dict,list)): body=json.dumps(body).encode()
    if isinstance(body,str): body=body.encode()
    req=Request(base+path,data=body,headers=headers,method=method)
    try:
        with urlopen(req,timeout=8) as resp: code,raw=resp.status,resp.read()
    except HTTPError as e: code,raw=e.code,e.read()
    assert code==expected,(method,path,code,raw[:300])
    results.append({'method':method,'path':path,'status':code})
    try: return json.loads(raw)
    except (json.JSONDecodeError,UnicodeDecodeError): return raw
try:
    for _ in range(80):
        if proc.poll() is not None: raise RuntimeError('isolated server exited; see server.log')
        try:
            with urlopen(base+'/healthz',timeout=1) as response: response.read()
            break
        except (URLError,ConnectionError): time.sleep(.1)
    else: raise RuntimeError('isolated server did not become ready')
    prefix='/v0/management'
    request(prefix+'/config',auth=False,expected=401)
    cfg=request(prefix+'/config')
    assert cfg['codex']['identity-confuse'] is True
    raw=request(prefix+'/config.yaml',content_type='text/plain')
    assert b'orphan-delegation-compatibility: true' in raw
    changed=raw.decode().replace('sensitive-words: [original]','sensitive-words: [Hermes]')
    changed += '\n# Sync round-trip marker\nunknown-sync-field: preserved\n'
    request(prefix+'/config.yaml','PUT',changed,'application/yaml')
    reread=request(prefix+'/config.yaml',content_type='text/plain')
    for fragment in [b'unknown-sync-field: preserved',b'connection-pool:',b'session-affinity-subagents: false',b'identity-confuse: true',b'speed-throttle:',b'api-key-rate-limit:',b'response-steering: false',b'stream-bootstrap-timeout: 30s']:
        assert fragment in reread,fragment
    request(prefix+'/config.yaml','PUT','invalid: [','application/yaml',expected=400)
    snapshot={'version':1,'usage':{'apis':{'synthetic-key':{'models':{'synthetic-model':{'details':[{'timestamp':datetime.now(timezone.utc).isoformat(),'latency_ms':12,'source':'isolated','auth_index':'synthetic-index','failed':False,'credits_used':True,'tokens':{'input_tokens':3,'output_tokens':2,'reasoning_tokens':0,'cached_tokens':0,'total_tokens':5}}]}}}}}}
    imported=request(prefix+'/usage/import','POST',snapshot)
    assert imported['added']==1,imported
    duplicate=request(prefix+'/usage/import','POST',snapshot)
    assert duplicate['added']==0 and duplicate['skipped']==1,duplicate
    usage=request(prefix+'/usage')
    assert usage['usage']['total_tokens']==5 and usage['usage']['total_requests']==1,usage
    exported=request(prefix+'/usage/export')
    detail=exported['usage']['apis']['synthetic-key']['models']['synthetic-model']['details'][0]
    assert detail['credits_used'] and detail['tokens']['total_tokens']==5,detail
    for view in ['auth','model','detail']:
        stats=request(prefix+'/antigravity-stats?view='+view)
        assert stats['view']==view and stats['summary']['total_requests']==0
    cleared=request(prefix+'/antigravity-stats?auth_id=synthetic','DELETE')
    assert cleared['cleared_entries']==0
    assert request(prefix+'/auth-files')['files']==[]
    request(prefix+'/usage-queue?count=0',expected=400)
    assert request(prefix+'/usage-queue')==[]
    assert request(prefix+'/get-auth-status?state=isolated-sync-state')['status']=='error'
    assert request(prefix+'/oauth-session?state=isolated-sync-state','DELETE')['cancelled'] is False
    request(prefix+'/oauth-callback','POST',{'provider':'codex','redirect_url':'http://localhost/?state=isolated-sync-state&code=synthetic'},expected=404)
    assert request(prefix+'/meta-api-key')['meta-api-key']==[]
    assert request(prefix+'/quota/providers')['providers']==[]
    request(prefix+'/quota/fetch','POST',{'auth_index':'synthetic'},expected=404)
    request(prefix+'/quota/reset','POST',{},expected=400)
    request(prefix+'/api-key-usage')
    request(prefix+'/auth-files/refresh','POST',{},expected=400)
    request(prefix+'/vertex/import','POST',b'',expected=400)
    # The Gemini CLI login route is plugin-provided; no plugin/real credentials are loaded here.
    request(prefix+'/gemini-cli-auth-url?project_id=synthetic',expected=404)
    html=request('/management.html',auth=False)
    assert hashlib.sha256(html).hexdigest()==hashlib.sha256(management_path.read_bytes()).hexdigest()
    assert not list((runtime/'auths').iterdir()),'smoke checks wrote credentials'
    print(f'{len(results)} isolated HTTP checks passed; config, usage, stats, OAuth validation, auth files and served HTML verified.')
    print(f'Browser URL: {base}/management.html; evidence: {runtime}', flush=True)
finally:
    if args.image:
        subprocess.run(['docker','stop','cliproxy-smoke-'+str(port)],check=False,stdout=subprocess.DEVNULL)
    proc.terminate()
    try: proc.wait(timeout=8)
    except subprocess.TimeoutExpired: proc.kill(); proc.wait()
    log.close()
    (runtime/'http-results.json').write_text(json.dumps(results,indent=2),encoding='utf-8')
    runtime_dir.cleanup()

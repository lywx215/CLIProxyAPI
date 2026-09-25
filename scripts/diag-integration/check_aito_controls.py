"""Independent live fake-stream and in-flight DEBUG checks via approved fixture."""
import argparse
import http.client
import json
from pathlib import Path
import tempfile
from urllib.parse import urlsplit

from run import AITO, HERE, Process, environment, request, verify, write_json


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--aito',type=Path,required=True)
    parser.add_argument('--node',type=Path,required=True)
    parser.add_argument('--output',type=Path,required=True)
    args=parser.parse_args()
    verify(args.aito,AITO)
    args.output.mkdir(parents=True,exist_ok=False)
    cwd=Path(tempfile.mkdtemp(prefix='diag07-aito-controls-'))
    p=Process('aito-controls',[args.node,HERE/'aito-wrapper.cjs',args.aito],cwd,environment(cwd,'aito-controls'))
    responses=[]
    try:
        p.ready()
        payload={'model':'diag-reasoning-fake','messages':[{'role':'user','content':'synthetic'}],'stream':True}
        responses.append({'id':'fake-stream-reasoning',**request(p.address,'/v1/chat/completions',payload)})
        before=p.command({'op':'snapshot'})['dispatches']
        conn=http.client.HTTPConnection('127.0.0.1',urlsplit(p.address).port)
        payload['model']='diag-slow'
        conn.request('POST','/v1/chat/completions',json.dumps(payload),{'Content-Type':'application/json'})
        for _ in range(100):
            if p.command({'op':'snapshot'})['dispatches']>before:break
        else:raise RuntimeError('browser dispatch handshake missing')
        p.command({'op':'debug','enabled':False})
        response=conn.getresponse()
        responses.append({'id':'inflight-debug-disabled','status':response.status,
                          'headers':{k.lower():v for k,v in response.getheaders() if k.lower().startswith('x-diag-')},
                          'body':response.read().decode()})
        conn.close()
        p.command({'op':'debug','enabled':True})
    finally:
        closed=p.close()
        exported=p.export(args.output)
        write_json(args.output/'close.json',closed)
        write_json(args.output/'exports.json',exported)
        write_json(args.output/'requests.json',responses)
    records=[json.loads(s) for s in (args.output/exported['file']).read_text(encoding='utf-8').splitlines()]
    checks=[]
    for response in responses:
        selected=[(i+1,r) for i,r in enumerate(records) if r['requestId']==response['headers']['x-diag-request-id']]
        if response['id']=='fake-stream-reasoning':
            converted=[r for _,r in selected if r['event']=='response.converted']
            ok=bool(converted) and converted[-1]['data']['deliveredUsage']['outputTotal']['value']==100
            observed=[r['data'] for r in converted]
        else:
            terminals=[r for _,r in selected if r['event']=='diag.server']
            ok=len(terminals)==1 and terminals[0]['data']['coverage']['debugCapture']=='interrupted'
            observed=[r['data'] for r in terminals]
        checks.append({'id':response['id'],'level':'live','pass':ok and response['status']==200,
                       'observed':observed,'evidence':[exported['file']+':'+str(i) for i,_ in selected]})
    write_json(args.output/'checks.json',{'checks':checks,'sixInstances':False})
    print(json.dumps({'checks':len(checks),'failed':[r['id'] for r in checks if not r['pass']]}))
    return 1 if len(checks)!=2 or any(not r['pass'] for r in checks) or closed['exitCode']!=0 else 0


if __name__=='__main__':raise SystemExit(main())

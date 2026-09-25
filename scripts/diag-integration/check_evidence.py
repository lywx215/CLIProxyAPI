"""Assert producer semantics from actual response IDs and immutable JSONL exports.

This complements HTTP reachability. Missing evidence fails a check; it is never
filled using expected output. Exit 0 means these scoped checks passed, not full
DIAG-07 acceptance. Every check retains precise source/line evidence.
"""
import argparse
import json
from pathlib import Path


def read_error_evidence(instance, response, attempts):
    """Upstream read failure and downstream framing are independent facts."""
    diagnostic = any(r['data']['resultClass']=='error' and r['data']['failureStage']=='read' and not r['data']['eofSeen'] for _,r in attempts)
    if instance.startswith('gcli'):
        wire = response.get('readError')=='incomplete_read'
    elif instance.startswith('cpa'):
        # Approved Gemini handlers can finish HTTP normally with an error frame
        # or the existing Responses completed tail; this is not upstream EOF.
        wire = response.get('readError') is None and ('"error"' in response['body'] or 'response.completed' in response['body'])
    else:
        wire = False
    return wire, diagnostic


def check_run(root):
    requests = json.loads((root/'requests.json').read_text(encoding='utf-8'))
    records = []
    for path in sorted(root.glob('*.jsonl')):
        for line, raw in enumerate(path.read_text(encoding='utf-8').splitlines(),1):
            records.append((f'{path.name}:{line}', json.loads(raw)))
    checks = []

    def check(name, ok, evidence, detail=None):
        checks.append({'id':name,'level':'live','pass':bool(ok),'evidence':[x[0] for x in evidence],'observed':detail})

    for q in requests:
        if 'headers' not in q: continue
        rid, trace = q['headers'].get('x-diag-request-id'),q['headers'].get('x-diag-trace-id')
        selected = [(loc,r) for loc,r in records if r['requestId']==rid and r['traceId']==trace and loc.startswith(q['instance']+'.jsonl:')]
        servers = [(loc,r) for loc,r in selected if r['event']=='diag.server']
        attempts = [(loc,r) for loc,r in selected if r['event']=='upstream.attempt_finished']
        converted = [(loc,r) for loc,r in selected if r['event']=='response.converted']
        calls = [(loc,r) for loc,r in selected if r['event']=='diag.call']
        name = q['id']
        check(name+'/identity',rid and trace and len(servers)==1,servers,{'responseRequestId':rid,'responseTraceId':trace})
        check(name+'/anonymous',all(r['callerAlias'] is None and r['callerAliasScope']=='unknown' for _,r in selected),selected[:1])
        if not servers: continue
        server=servers[0][1]
        check(name+'/call-count',server['data']['callCount']==len(calls),servers+calls,{'declared':server['data']['callCount'],'observed':len(calls)})
        if '-standard' in name or '-reuse-' in name:
            check(name+'/accepted-parent',server['traceId']=='1'*32 and server['parentSpanId']=='2'*16,servers)
        if '-invalid' in name or name.endswith('-duplicate'):
            check(name+'/invalid-replaced',server['contextSource']=='invalid_replaced' and server['traceId']!='1'*32,servers)
        if name.endswith('duplicate-id'):
            check(name+'/duplicate-caller-rejected',server['callerRequestId'] is None,servers)
        if 'debug-off' in name:
            check(name+'/basic-only',not any(r['recordKind']=='debug' for _,r in selected),selected)
        if name.startswith('dynamic-debug-'):
            check(name+'/interrupted',server['data']['coverage']['debugCapture']=='interrupted',servers)
        if name.startswith('throttle-'):
            throttles=[(loc,r) for loc,r in selected if r['event']=='throttle.finished']
            check(name+'/observed',len(throttles)==1,throttles)
            if len(throttles)==1:
                data=throttles[0][1]['data']
                token_dominated=name=='throttle-collected-gcli'
                tokens,rate,delay=(89,100,10) if token_dominated else (87,1000,100)
                check(name+'/provenance',data['tokenCount']==tokens and data['tokenSource']=='provider_output',throttles,data)
                check(name+'/configuration',data['enabled'] and data['targetTokensPerSecond']==rate and data['selectedFirstTokenDelayMs']==delay,throttles,data)
                target=max(tokens/rate*1000,delay)
                check(name+'/whole-response-duration',q['completeMs']>=target-1,throttles,{'targetMs':target,'completeMs':q['completeMs']})
                check(name+'/actual-wait',not data['cancelled'] and data['actualWaitMs']>=data['plannedWaitMs']-1 and data['actualWaitMs']<=q['completeMs'],throttles,data)
        if not name.startswith('semantics-'): continue
        scenario=q['scenario']
        check(name+'/attempt-evidence',bool(attempts),attempts)
        states=[r['data']['resultClass'] for _,r in attempts]
        is_gcli=q['instance'].startswith('gcli')
        is_cpa=q['instance'].startswith('cpa')
        aito_path=q['instance'].startswith('aito') or q['instance']=='cpa2'
        expected_status=429 if scenario in ('nested','http429') else 503 if scenario=='http503' else 504 if scenario=='network' else 502 if aito_path and scenario in ('empty','thought') else 200
        allowed_statuses={429,503} if is_cpa and scenario=='nested' else {expected_status}
        check(name+'/wire-status',q['status'] in allowed_statuses,servers,{'status':q['status'],'expected':sorted(allowed_statuses)})
        if scenario in ('thought13','tail13','reasoning','zero','missing','tool','media'):
            # DONE without HTTP EOF is an approved local-read incomplete state.
            valid = all(r['data']['resultClass'] == ('incomplete' if is_gcli and not r['data']['eofSeen'] else 'success') for _,r in attempts)
            check(name+'/classification',valid,attempts,states)
        if scenario in ('empty','thought','blocked','incomplete','truncated','errorframe','readerror','nested','http429','http503','network'):
            expected={'empty':'empty','thought':'empty','blocked':'blocked','incomplete':'incomplete','truncated':'incomplete'}.get(scenario,'error')
            if is_cpa and q['status']>=400: expected='error'
            check(name+'/classification',bool(states) and all(s==expected for s in states),attempts,states)
        if scenario=='readerror':
            wire, diagnostic=read_error_evidence(q['instance'],q,attempts)
            check(name+'/wire-read-error',wire,attempts, {'readError':q.get('readError'),'protocolErrorOrCompletionFrame':'"error"' in q['body'] or 'response.completed' in q['body']})
            check(name+'/diagnostic-read-error',diagnostic,attempts)
        if scenario in ('zero','missing','thought13','tail13','reasoning'):
            for loc,r in attempts:
                u=r['data']['usage'];candidate=u['candidate']
                expected=None if scenario=='missing' else 0 if scenario=='zero' else 87
                check(name+'/candidate-'+str(r['attemptNo']),candidate['value']==expected and candidate['present']==(expected is not None),[(loc,r)],u)
                if scenario in ('thought13','tail13','reasoning'):
                    check(name+'/reasoning',u['reasoning']['value']==13,[(loc,r)],u)
            if scenario in ('thought13','tail13','reasoning'):
                totals=[r['data'].get('deliveredUsage',{}).get('outputTotal',{}).get('value') for _,r in converted]
                expected_total=None if scenario=='tail13' else 87 if is_gcli else 100
                if is_cpa:
                    # Compare the emitted OpenAI usage, not another producer's
                    # candidate/reasoning formula or its metadata placement.
                    wire_totals=[]
                    for line in q['body'].splitlines():
                        if not line.startswith('data: ') or line[6:]=='[DONE]': continue
                        frame=json.loads(line[6:])
                        usage=frame.get('usage') or {}
                        if 'completion_tokens' in usage: wire_totals.append(usage['completion_tokens'])
                    expected_total=wire_totals[-1] if wire_totals else None
                check(name+'/converted-total',bool(totals) and totals[-1]==expected_total,converted,totals)
        if scenario in ('tool','media'):
            key='validToolCalls' if scenario=='tool' else 'mediaParts'
            check(name+'/upstream-effective-output',any((r['data']['output'].get(key) or 0)>0 for _,r in attempts),attempts)
        if scenario in ('retry','nested','anti_nested'):
            count=4 if scenario=='anti_nested' else 3 if is_gcli else 2
            if is_cpa: count=2 if scenario=='nested' else 1
            ids={r['attemptId'] for _,r in attempts}
            check(name+'/attempt-identity',len(ids)==count and None not in ids,attempts,{'ids':len(ids),'attemptNo':[r['attemptNo'] for _,r in attempts]})
            check(name+'/http-or-dispatch-count',len(calls)==count,calls)
            if scenario=='anti_nested': check(name+'/reset-no-preserved',[r['attemptNo'] for _,r in attempts]==[1,2,1,2],attempts)
        if is_cpa:
            check(name+'/local-owner-scope',all(r['retryScope']=='conductor_gemini_family' for _,r in attempts),attempts)

    for service in ('gcli','aito'):
        for first, second, label in ((service+'1',service+'4','workers'),(service+'2',service+('5' if service=='gcli' else '3'),'restart')):
            def process(name): return [(loc,r) for loc,r in records if loc.startswith(name+'.jsonl:') and r['event']=='diag.process']
            a,b=process(first),process(second)
            if a and b:
                check(service+'/'+label,a[0][1]['instanceId']==b[0][1]['instanceId'] and a[0][1]['bootId']!=b[0][1]['bootId'],a+b, 'real independent processes; not POSIX fork')
    for service in ('gcli1','aito1'):
        same=[(loc,r) for loc,r in records if loc.startswith(service+'.jsonl:') and r['event']=='diag.server' and r['callerRequestId']=='shared-caller']
        check(service+'/reused-id-candidates',len(same)>=3 and len({r['spanId'] for _,r in same})==len(same),same)
    for name in ('trailing-model','multiple-empty','whitespace','tool','all-empty'):
        q=next((q for q in requests if q['id']=='cleaning-'+name),None)
        if not q: continue
        selected=[(loc,r) for loc,r in records if r['requestId']==q['headers'].get('x-diag-request-id') and r['event']=='request.normalized']
        if not selected: check('cleaning/'+name,False,[]);continue
        data=selected[0][1]['data']
        operations=[x['operation'] for x in data['transformations']]
        if name=='trailing-model': ok=data['after']['messageCount']==1 and 'remove_trailing_model' in operations
        elif name=='all-empty': ok=data['after']['messageCount']==0 and q['status']==400
        elif name=='tool': ok=data['after']['toolCallCount']==1 and data['after']['toolResponseCount']==1
        else: ok=data['after']['emptyMessageCount']==0
        check('cleaning/'+name,ok,selected,data)
    report={'checks':checks,'failed':[x['id'] for x in checks if not x['pass']],'acceptanceComplete':False,
            'qualification':'Scoped live producer semantics only; component/synthetic/unexecuted matrix remains separate.'}
    (root/'semantic-checks.json').write_text(json.dumps(report,ensure_ascii=False,indent=2)+'\n',encoding='utf-8')
    return report


if __name__=='__main__':
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('run',type=Path)
    args=parser.parse_args()
    report=check_run(args.run)
    print(json.dumps({'checks':len(report['checks']),'failed':report['failed']}))
    raise SystemExit(1 if report['failed'] else 0)

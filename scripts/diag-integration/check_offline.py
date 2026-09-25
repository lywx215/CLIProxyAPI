"""Replay live exports plus explicitly synthetic log faults through frozen DIAG-06."""
import argparse
import copy
import json
from pathlib import Path
import subprocess
import tempfile

from run import CREATE_FLAGS, write_json, sha
from bilateral import check_bilateral


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--analyzer',type=Path,required=True)
    parser.add_argument('--live',type=Path,required=True)
    parser.add_argument('--pair',type=Path,required=True)
    parser.add_argument('--output',type=Path,required=True)
    parser.add_argument('--peer-plan',type=Path, help='explicit instance -> expectedPeers mapping for historical requests without runner metadata')
    parser.add_argument('--required-service',action='append', help='default: both gcli2api and aitoapi; use aitoapi explicitly for the historical smoke subset')
    parser.add_argument('--live-known-loss',action='append',default=[], help='additional known-loss source alias for historical live exports whose manifest omitted the flag')
    args=parser.parse_args()
    args.output.mkdir(parents=True,exist_ok=False)
    rows=[]
    known_loss_paths = set()
    for root in (args.pair, args.live):
        manifest=root/'exports.json'
        if manifest.exists():
            known_loss_paths.update((root/e['file']).resolve() for e in json.loads(manifest.read_text(encoding='utf-8')) if e.get('knownLoss'))
    known_loss_paths.update((args.live/(alias+'.jsonl')).resolve() for alias in args.live_known_loss)
    def analyze(name, inputs, options=(), expected_exit=0):
        argv=[str(args.analyzer)]
        for alias,path,trusted in inputs:
            argv+=['-input',alias+'='+str(path)]
            if trusted:argv+=['-trust',alias]
            if path.resolve() in known_loss_paths:argv+=['-known-loss',alias]
        argv+=['-format','json',*options]
        result=subprocess.run(argv,capture_output=True,creationflags=CREATE_FLAGS)
        if result.returncode not in (0,1):raise RuntimeError('analyzer invocation failed')
        report=json.loads(result.stdout)
        (args.output/(name+'.json')).write_bytes(result.stdout)
        row={'id':name,'level':'synthetic-log','exitCode':result.returncode,'pass':result.returncode==expected_exit,
             'evidence':name+'.json','counts':report['counts'],'verified':sum(e['verified'] for e in report['edges'] or []),
             'quarantined':len(report['quarantined'] or []),'sourceFindings':[s['findings'] for s in report['sources']]}
        rows.append(row)
        return report,row
    lost={p.name for p in args.pair.glob('*.jsonl') if p.resolve() in known_loss_paths}
    pair=[(p.stem,p,True) for p in sorted(args.pair.glob('*.jsonl')) if p.name not in lost]
    requests=json.loads((args.pair/'requests.json').read_text(encoding='utf-8'))
    controlled=[q for q in requests if q.get('instance','').startswith('cpa') and 'headers' in q]
    peer_plan=json.loads(args.peer_plan.read_text(encoding='utf-8')) if args.peer_plan else {}
    for index,q in enumerate(controlled):
        q.setdefault('id','historical-controlled-'+str(index))
        if 'expectedPeers' not in q: q['expectedPeers']=peer_plan[q['instance']]
    required=args.required_service or ['gcli2api','aitoapi']
    base,row=analyze('controlled-pair',pair)
    graph=check_bilateral(base,controlled,required)
    write_json(args.output/'controlled-bilateral.json',graph)
    row.update(level='live-export-replay',controlledCalls=graph['controlledCalls'],excludedKnownLoss=sorted(lost))
    row['pass'] &= not graph['failed']
    if lost:
        _,row=analyze('pair-all-sources',[(p.stem,p,True) for p in sorted(args.pair.glob('*.jsonl'))])
        row.update(level='live-export-replay',qualification='All pair sources with their real known-loss declarations; normal-subset verification does not apply globally.')
        row['pass'] &= row['verified']==0
    repeated,row=analyze('duplicate-import',pair+[(alias+'copy',path,True) for alias,path,_ in pair])
    row['pass'] &= repeated['counts']==base['counts'] and len(repeated['evidence'])==len(base['evidence'])
    _,row=analyze('untrusted-pair',[(alias,path,False) for alias,path,_ in pair])
    row['pass'] &= row['verified']==0
    _,row=analyze('known-loss',pair,['-known-loss',pair[0][0]])
    row['pass'] &= row['verified']==0
    live=[(p.stem,p,True) for p in sorted(args.live.glob('*.jsonl'))]
    report,row=analyze('anonymous-alias',live,['-caller-request-id','shared-caller','-caller-scope','unknown','-environment','test','-deployment','diag07-local','-service','gcli2api'])
    row['level']='live';row['pass'] &= len(report['aliasCandidates'] or [])>=3
    fixtures=args.output/'fixtures';fixtures.mkdir()
    receiver=next(path for _,path,_ in pair if any(r['event']=='diag.server' and r['parentSpanId'] is not None and r['service']!='cliproxyapi' for r in map(json.loads,path.read_text(encoding='utf-8').splitlines())))
    original=[json.loads(x) for x in receiver.read_text(encoding='utf-8').splitlines()]
    def fixture(name,records):
        path=fixtures/(name+'.jsonl')
        path.write_text(''.join(json.dumps(r,separators=(',',':'))+'\n' for r in records),encoding='utf-8')
        return path
    skewed=copy.deepcopy(original)
    for r in skewed:r['ts']='2000-01-01T00:00:00.000Z'
    skew=fixture('clock-skew',skewed)
    report,row=analyze('clock-skew',[(alias,skew if path==receiver else path,trust) for alias,path,trust in pair])
    skew_graph=check_bilateral(report,controlled,required)
    row['pass'] &= not skew_graph['failed'] and skew_graph['controlledCalls']==graph['controlledCalls']
    chosen=next(r for r in original if r['event']=='diag.server' and r['parentSpanId'] is not None)
    duplicate=copy.deepcopy(chosen)
    duplicate['spanId']=duplicate['serverSpanId']='f'*16
    duplicate['requestId']='synthetic-duplicate-parent'
    fault=fixture('duplicate-parent',[duplicate])
    report,row=analyze('duplicate-parent',pair+[('fault',fault,True)])
    row['pass'] &= any('ambiguous_parent' in (e['findings'] or []) for e in report['edges'] or [])
    mixed=fixtures/'mixed.jsonl'
    mixed.write_bytes(receiver.read_bytes()+b'legacy synthetic text\n'+b'{"ordinary":"synthetic"}\n'+b'{broken\n'+b'{"diagnosticSchema":"ai-proxy-diagnostics/99"}\n'+b'x'*5000+b'\n')
    report,row=analyze('mixed-framing',[(alias,mixed if path==receiver else path,trust) for alias,path,trust in pair],expected_exit=1)
    row['pass'] &= row['quarantined']==4
    # This is real historical exported data, not a running historical service.
    history=Path(__file__).resolve().parents[2]/'coordination/diagnostics/20260924/DIAG-05-R1-cancel-synthetic.jsonl'
    report,row=analyze('historical-log',live+[('history',history,True)])
    row['qualification']='historical candidate-stage export retained in the approved baseline, mixed with current exports; no historical process executed'
    with tempfile.TemporaryDirectory(prefix='diag07-capacity-') as tmp:
        near=Path(tmp)/'near.jsonl'
        process=next(r for _,path,_ in live for r in map(json.loads,path.read_text(encoding='utf-8').splitlines()) if r['event']=='diag.process' and r['service']=='gcli2api')
        line=(json.dumps(process,separators=(',',':'))+'\n').encode()
        padding=b'x'*511+b'\n'
        data=line+padding*((16773120-len(line)+511)//512)
        near.write_bytes(data)
        report,row=analyze('near-capacity',[('near',near,True)])
        row['inputSha256']=sha(data);row['inputBytes']=len(data)
        row['pass'] &= any('suspected_tail_gap_near_producer_limit' in (s['findings'] or []) for s in report['sources'])
        row['qualification']='synthetic size clue only; not proof of actual producer stoppage or complete export'
    write_json(args.output/'checks.json',{'checks':rows,'failed':[r['id'] for r in rows if not r['pass']]})
    print(json.dumps({'checks':len(rows),'failed':[r['id'] for r in rows if not r['pass']]}))
    return 1 if any(not r['pass'] for r in rows) else 0


if __name__=='__main__':raise SystemExit(main())

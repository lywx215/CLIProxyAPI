"""R1 regression using immutable live exports and labeled synthetic log faults.

Only the offline analyzer executes. No CPA/gcli/Aito process is started.
"""
import argparse
import copy
import json
from pathlib import Path
import subprocess
import tempfile

from bilateral import analyze_scopes, check_bilateral, check_scope_loss
from run import CREATE_FLAGS, REPO, sha, write_json


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--analyzer', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    args.output.mkdir(parents=True, exist_ok=False)
    evidence_root = REPO/'coordination/diagnostics/20260924'
    smoke, live = evidence_root/'DIAG-07-aito-smoke', evidence_root/'DIAG-07-run-06'
    peers = {alias: {'service': 'aitoapi', 'deploymentId': 'diag07-local', 'environment': 'test', 'instanceId': alias}
             for alias in ('aito1', 'aito2')}
    # The historical smoke deliberately configured both CPA instances to Aito.
    # This plan is an explicit test input, not inferred from diagnostic claims.
    peer_plan = {'cpa1': peers, 'cpa2': peers}
    write_json(args.output/'historical-peer-plan.json', peer_plan)
    requests = [q for q in json.loads((smoke/'requests.json').read_text(encoding='utf-8')) if q['instance'].startswith('cpa')]
    for i, q in enumerate(requests):
        q.update(id='smoke-'+str(i), expectedPeers=peer_plan[q['instance']], graphPolicy='complete')
    inputs = [{'alias': prefix+p.stem, 'path': p, 'trusted': True, 'knownLoss': prefix == 'run06-' and p.stem == 'aito4'}
              for prefix, root in (('smoke-', smoke), ('run06-', live)) for p in sorted(root.glob('*.jsonl'))]
    scopes = analyze_scopes(args.analyzer, inputs, args.output)
    normal = json.loads((args.output/'analysis.json').read_text(encoding='utf-8'))
    all_sources = json.loads((args.output/'analysis-known-loss.json').read_text(encoding='utf-8'))
    rows = []

    def check(name, ok, **detail):
        rows.append({'id': name, 'pass': bool(ok), **detail})

    graph = check_bilateral(normal, requests, ['aitoapi'])
    write_json(args.output/'normal-bilateral.json', graph)
    check('normal-subset-real-smoke', not graph['failed'] and all(s['exitCode'] == 0 for s in scopes),
          level='live-export-replay', controlledCalls=graph['controlledCalls'], failed=graph['failed'])
    check('all-sources-explicit-known-loss', check_scope_loss(all_sources), level='live-export-replay',
          verified=sum(e['verified'] for e in all_sources['edges']), knownLossAliases=[s['alias'] for s in all_sources['sources'] if s['knownLoss']])
    full = check_bilateral(normal, requests, ['gcli2api', 'aitoapi'])
    check('aito-only-cannot-pass-full-mode', 'bilateral/required-service/gcli2api' in full['failed'], level='regression', failed=full['failed'])
    no_cpa = json.loads((live/'analysis.json').read_text(encoding='utf-8'))
    empty = check_bilateral(no_cpa, [], ['gcli2api', 'aitoapi'])
    check('zero-cpa-cannot-pass-full-mode', set(empty['failed']) == {'bilateral/required-service/gcli2api', 'bilateral/required-service/aitoapi'}, level='regression', failed=empty['failed'])

    source_records = {p.stem: [json.loads(line) for line in p.read_text(encoding='utf-8').splitlines()] for p in sorted(smoke.glob('*.jsonl'))}
    first = next(r for r in source_records['cpa1'] if r['event'] == 'diag.call')
    call_span = first['spanId']
    receiver = next(r for values in source_records.values() for r in values if r['event'] == 'diag.server' and r['parentSpanId'] == call_span)

    with tempfile.TemporaryDirectory(prefix='diag07-r1-replay-') as scratch:
        def analyze(name, records, untrusted=()):
            folder = Path(scratch)/name
            folder.mkdir()
            argv = [str(args.analyzer)]
            hashes = {}
            for alias, values in records.items():
                path = folder/(alias+'.jsonl')
                path.write_text(''.join(json.dumps(r, separators=(',', ':'))+'\n' for r in values), encoding='utf-8')
                hashes[alias] = sha(path.read_bytes())
                argv += ['-input', alias+'='+str(path)]
                if alias not in untrusted: argv += ['-trust', alias]
            result = subprocess.run(argv+['-format', 'json'], capture_output=True, creationflags=CREATE_FLAGS)
            if result.returncode != 0: raise RuntimeError('synthetic analyzer failed: '+name)
            report = json.loads(result.stdout)
            write_json(args.output/(name+'-inputs.json'), {'level': 'synthetic-log', 'sourceSha256': hashes, 'untrusted': list(untrusted)})
            return report

        def affected(report):
            call = next(n for n in report['nodes'] if n['spanId'] == call_span and n['kind'] == 'call')
            return next(e for e in report['edges'] if e['kind'] == 'remote' and call['id'] in (e['senders'] or []))

        half = {alias: values for alias, values in source_records.items() if alias != 'cpa2'}
        report = analyze('two-controlled-calls', half)
        two = check_bilateral(report, [q for q in requests if q['instance'] == 'cpa1'], ['aitoapi'])
        verified = sum(e['verified'] for e in report['edges'])
        check('dynamic-expectation-not-four', not two['failed'] and two['controlledCalls'] == 2 and verified == 2,
              level='synthetic-log-subset', oldFixedFourWouldPass=verified == 4, controlledCalls=two['controlledCalls'], verified=verified)

        def fault(name, values, finding, untrusted=()):
            report = analyze(name, values, untrusted)
            edge = affected(report)
            result = check_bilateral(report, requests, ['aitoapi'])
            check(name, finding in edge['findings'] and not edge['verified'] and bool(result['failed']),
                  level='synthetic-log', edge=edge, rejectedByChecker=result['failed'])
            return report

        missing = copy.deepcopy(source_records)
        # Remove all records for exactly the receiver server and its local calls.
        for alias in missing:
            missing[alias] = [r for r in missing[alias] if not (r['bootId'] == receiver['bootId'] and r['serverSpanId'] == receiver['spanId'])]
        report = fault('missing-receiver', missing, 'missing_peer')
        cancellation = copy.deepcopy(requests)
        cancellation[0].update(graphPolicy='cancellation', cancelledAfterByte=True)
        result = check_bilateral(report, cancellation, ['aitoapi'])
        check('cancellation-specific-missing-peer', not result['failed'], level='synthetic-log', failed=result['failed'])
        result = check_bilateral(normal, cancellation, ['aitoapi'])
        check('cancellation-with-complete-bilateral-evidence', not result['failed'], level='synthetic-test-input', failed=result['failed'],
              qualification='Synthetic caller cancellation flag over complete historical graph; no new live cancellation claim.')

        missing_terminal = copy.deepcopy(source_records)
        for alias in missing_terminal:
            missing_terminal[alias] = [r for r in missing_terminal[alias] if not (r['event'] == 'diag.server' and r['spanId'] == receiver['spanId'])]
        report = fault('missing-receiver-terminal', missing_terminal, 'terminal_evidence_incomplete')
        result = check_bilateral(report, cancellation, ['aitoapi'])
        check('cancellation-specific-missing-terminal', not result['failed'], level='synthetic-log', failed=result['failed'])

        duplicated = copy.deepcopy(source_records)
        duplicate = copy.deepcopy(receiver)
        duplicate.update(spanId='f'*16, serverSpanId='f'*16, requestId='synthetic-duplicate-parent')
        duplicated['duplicate'] = [duplicate]
        report = fault('duplicate-parent', duplicated, 'ambiguous_parent')
        check('cancellation-cannot-excuse-conflict', bool(check_bilateral(report, cancellation, ['aitoapi'])['failed']), level='synthetic-log')
        fault('untrusted-receiver', source_records, 'untrusted_source', (first['data']['targetAlias'],))
        for field, value, finding in (('peerRequestId', 'synthetic-wrong-id', 'peer_conflict'),
                                      ('peerTraceId', 'e'*32, 'context_mismatch'),
                                      ('peerService', 'gcli2api', 'peer_conflict'),
                                      ('peerDeploymentId', 'synthetic-other-deployment', 'peer_conflict')):
            mutated = copy.deepcopy(source_records)
            call = next(r for r in mutated['cpa1'] if r['event'] == 'diag.call' and r['spanId'] == call_span)
            call['data'][field] = value
            fault(field+'-mismatch', mutated, finding)
        alias = copy.deepcopy(source_records)
        call = next(r for r in alias['cpa1'] if r['event'] == 'diag.call' and r['spanId'] == call_span)
        call['data']['targetAlias'] = 'synthetic-unknown-alias'
        report = analyze('alias-mismatch', alias)
        result = check_bilateral(report, requests, ['aitoapi'])
        check('alias-checked-against-configured-plan', affected(report)['verified'] and bool(result['failed']),
              level='synthetic-log', rejectedByChecker=result['failed'], qualification='Frozen analyzer does not know driver alias configuration; driver must check it.')
        call['data']['targetAlias'] = 'aito2'
        report = analyze('alias-wrong-receiver', alias)
        result = check_bilateral(report, requests, ['aitoapi'])
        check('known-alias-must-match-receiver-instance', affected(report)['verified'] and bool(result['failed']), level='synthetic-log', rejectedByChecker=result['failed'])
        absent = copy.deepcopy(source_records)
        call = next(r for r in absent['cpa1'] if r['event'] == 'diag.call' and r['spanId'] == call_span)
        call['data'].update(peerRequestId=None, peerTraceId=None)
        report = analyze('optional-peer-ids-absent', absent)
        result = check_bilateral(report, requests, ['aitoapi'])
        check('optional-peer-ids-absent', not result['failed'], level='synthetic-log', failed=result['failed'])
        both = copy.deepcopy(source_records)
        for values in both.values():
            for r in values:
                if r['service'] == 'aitoapi' and r['instanceId'] == 'aito2': r['service'] = 'gcli2api'
                if r['event'] == 'diag.call' and r['service'] == 'cliproxyapi' and r['data']['targetAlias'] == 'aito2': r['data']['peerService'] = 'gcli2api'
        synthetic_requests = copy.deepcopy(requests)
        for q in synthetic_requests: q['expectedPeers']['aito2']['service'] = 'gcli2api'
        report = analyze('synthetic-both-services', both)
        result = check_bilateral(report, synthetic_requests, ['gcli2api', 'aitoapi'])
        check('both-service-gate-positive-control', not result['failed'], level='synthetic-log', failed=result['failed'],
              qualification='Explicit service-label mutation tests checker branching only; never CPA-to-gcli live evidence.')
    write_json(args.output/'checks.json', {'checks': rows, 'failed': [r['id'] for r in rows if not r['pass']], 'acceptanceComplete': False})
    print(json.dumps({'checks': len(rows), 'failed': [r['id'] for r in rows if not r['pass']]}))
    return int(any(not r['pass'] for r in rows))


if __name__ == '__main__':
    raise SystemExit(main())

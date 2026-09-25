"""Scoped graph assertions against frozen DIAG-06 output, not timestamp joins."""
import json
from pathlib import Path
import subprocess

from run import CREATE_FLAGS, sha, write_json


def analyze_scopes(analyzer, exports, output):
    """Inputs contain explicit path, alias, trust and knownLoss declarations."""
    scopes = []
    for name, selected in (
        ('analysis.json', [e for e in exports if not e.get('knownLoss')]),
        ('analysis-known-loss.json', exports),
    ):
        argv = [str(analyzer)]
        for e in selected:
            argv += ['-input', e['alias']+'='+str(e['path'])]
            if e.get('trusted'): argv += ['-trust', e['alias']]
            if e.get('knownLoss'): argv += ['-known-loss', e['alias']]
        argv += ['-format', 'json']
        result = subprocess.run(argv, capture_output=True, creationflags=CREATE_FLAGS)
        (output/name).write_bytes(result.stdout)
        scopes.append({'report': name, 'exitCode': result.returncode,
                       'inputs': [{'alias': e['alias'], 'sha256': sha(Path(e['path']).read_bytes()),
                                   'trusted': bool(e.get('trusted')), 'knownLoss': bool(e.get('knownLoss'))}
                                  for e in selected],
                       'excludedKnownLoss': [e['alias'] for e in exports if e not in selected],
                       'qualification': 'Verification is limited to these imported sources; never global completeness.'})
    write_json(output/'analysis-scopes.json', {'scopes': scopes, 'acceptanceComplete': False})
    return scopes


def check_bilateral(report, requests, required_services):
    """Each request carries an independently configured expectedPeers map.

    Complete cases require verified edges. Cancellation may allow the specific
    missing-receiver/terminal gap, but cannot turn a conflict into a passing
    result. A complete cancellation edge still proves graph identity only.
    """
    checks, observed_services, observed_targets, assigned = [], set(), set(), set()
    evidence = {e['id']: e for e in report.get('evidence') or []}
    nodes = report.get('nodes') or []
    edges = report.get('edges') or []

    def records(node, event):
        return [evidence[i] for i in node['events'] if i in evidence and evidence[i]['record']['event'] == event]

    def add(name, ok, events=(), **observed):
        checks.append({'id': name, 'level': 'export-replay', 'pass': bool(ok),
                       'evidence': [p for e in events for p in e['provenance']], 'observed': observed})

    cpa_calls = [n for n in nodes if n['kind'] == 'call' and n['resource']['service'] == 'cliproxyapi']
    for q in requests:
        name = q['id']+'/bilateral'
        rid, trace = q['headers'].get('x-diag-request-id'), q['headers'].get('x-diag-trace-id')
        owner = [n for n in nodes if n['kind'] == 'server' and n['resource']['service'] == 'cliproxyapi'
                 and n['resource']['instanceId'] == q['instance'] and n['requestId'] == rid and n['traceId'] == trace]
        calls = [n for n in cpa_calls if n['requestId'] == rid and n['traceId'] == trace
                 and n['resource']['instanceId'] == q['instance']]
        add(name+'/owner-and-calls', len(owner) == 1 and bool(calls),
            ownerCount=len(owner), callCount=len(calls))
        owner_terminals = [r for n in owner for r in records(n, 'diag.server')]
        add(name+'/declared-call-count', len(owner_terminals) == 1 and owner_terminals[0]['record']['data']['callCount'] == len(calls),
            owner_terminals, observedCalls=len(calls))
        for call in calls:
            assigned.add(call['id'])
            prefix = name+'/'+call['spanId']
            terminal = records(call, 'diag.call')
            add(prefix+'/terminal', len(terminal) == 1, terminal)
            if len(terminal) != 1: continue
            event = terminal[0]
            data = event['record']['data']
            peer = q['expectedPeers'].get(data.get('targetAlias'))
            configured = bool(peer) and data.get('peerConfigured') is True
            configured = configured and data.get('peerService') == peer['service'] and data.get('peerDeploymentId') == peer['deploymentId']
            add(prefix+'/configured-peer', configured, terminal, declared=data, expectedPeers=q['expectedPeers'])
            owned = len(owner) == 1 and call['serverSpanId'] == owner[0]['spanId'] and call['resource'] == owner[0]['resource']
            add(prefix+'/local-owner', owned, terminal)
            remote = [e for e in edges if e['kind'] == 'remote' and call['id'] in (e['senders'] or [])]
            candidates = [n for n in nodes if n['kind'] == 'server' and n['traceId'] == call['traceId'] and n['parentSpanId'] == call['spanId']]
            add(prefix+'/unique-remote-edge', len(remote) == 1, terminal, edges=remote)
            if len(remote) != 1: continue
            edge = remote[0]
            receiver_events = [r for n in candidates for r in records(n, 'diag.server')]
            unique = len(candidates) == 1 and len(receiver_events) == 1
            identity = False
            if unique and peer:
                receiver = receiver_events[0]['record']
                identity = all(receiver[key] == peer[key] for key in ('service', 'deploymentId', 'instanceId', 'environment'))
                identity = identity and all(data.get(key) is None or data[key] == receiver[other]
                                            for key, other in (('peerRequestId', 'requestId'), ('peerTraceId', 'traceId')))
            verified = (configured and owned and unique and identity and edge['verified']
                        and edge['senders'] == [call['id']] and edge['receivers'] == [candidates[0]['id']]
                        and set(edge['findings'] or []) == {'verified'})
            # This is the only live fault allowance. It is based on missing
            # observed endpoints, not merely on a failed HTTP/business result.
            findings = set(edge['findings'] or [])
            cancellation_gap = q.get('cancelledAfterByte') is True and q.get('graphPolicy') == 'cancellation'
            precise_gap = (not candidates and findings == {'missing_peer'}) or (
                len(candidates) == 1 and not receiver_events and findings == {'terminal_evidence_incomplete'})
            allowed_gap = cancellation_gap and precise_gap and not edge['verified'] and edge['senders'] == [call['id']]
            add(prefix+'/identity-and-verification', verified or (configured and owned and allowed_gap),
                terminal+receiver_events, receiverCount=len(candidates), findings=sorted(findings),
                verified=bool(verified), acceptedCancellationGap=bool(allowed_gap),
                qualification='Graph identity is independent of business success and diagnostic coverage.')
            if verified:
                observed_services.add(peer['service'])
                observed_targets.add(peer['service']+'/'+data['targetAlias'])
    add('bilateral/all-cpa-calls-assigned', assigned == {n['id'] for n in cpa_calls},
        expectedCallCount=len(cpa_calls), assignedCallCount=len(assigned))
    for service in required_services:
        add('bilateral/required-service/'+service, service in observed_services,
            verifiedServices=sorted(observed_services))
    return {'checks': checks, 'failed': [c['id'] for c in checks if not c['pass']],
            'controlledCalls': len(assigned), 'verifiedServices': sorted(observed_services),
            'verifiedTargets': sorted(observed_targets), 'acceptanceComplete': False}


def check_scope_loss(report):
    """Known-loss import must retain its declaration and cannot verify edges."""
    edges = report.get('edges') or []
    return (any(s['knownLoss'] for s in report['sources']) and bool(edges)
            and all(not e['verified'] and 'export_known_loss' in (e['findings'] or []) for e in edges))

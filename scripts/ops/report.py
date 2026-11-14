"""Sanitized HTML/JSON reports; no raw snapshots, keys or generated reasoning."""
from __future__ import annotations

import datetime as dt
import html
import json
from collections import Counter, defaultdict
from pathlib import Path


TERMINAL = {'complete', 'complete_with_holds', 'rolled_back', 'already_current'}
METADATA_EVENTS = {'operation_started', 'operation_finished', 'cache_hit', 'retry', 'run_status',
                   'connection_rebuilt', 'preparation_ready', 'preparation_failed',
                   'propagation_intent', 'propagation_receipt'}


def union_seconds(intervals):
    end, total = None, 0.0
    for start, stop in sorted(intervals):
        stop = max(start, stop)
        total += max(0, stop - max(start, end if end is not None else start))
        end = max(end if end is not None else stop, stop)
    return round(total, 3)


def exception_waits(events, now):
    waits, blocked_at = [], None
    for event in events:
        if event['event'] != 'run_status':
            continue
        if event['data']['status'] == 'blocked':
            blocked_at = event['at'] if blocked_at is None else blocked_at
        elif blocked_at is not None:
            waits.append((blocked_at, event['at']))
            blocked_at = None
    if blocked_at is not None:
        waits.append((blocked_at, now))
    return waits


def activity(events, target=None):
    selected = [event for event in events if target is None or event['target'] == target]
    operations = [event['data'] for event in selected if event['event'] == 'operation_finished']
    def spans(rows):
        return [(row['started_at'], row['started_at'] + row['seconds']) for row in rows]
    execution = spans([row for row in operations if row['kind'] not in {'wait', 'drain', 'queue', 'phase'}])
    waits = spans([row for row in operations if row['kind'] in {'wait', 'drain'}])
    execution_seconds = union_seconds(execution)
    return {'execution_seconds': execution_seconds,
            'wait_seconds': round(max(0, union_seconds(execution + waits) - execution_seconds), 3),
            'queue_wait_seconds': union_seconds(spans([row for row in operations if row['kind'] == 'queue'])),
            'operations': operations,
            'cache_hits': dict(Counter(event['data'].get('scope', 'unknown') for event in selected if event['event'] == 'cache_hit')),
            'retries': [event['data'] for event in selected if event['event'] == 'retry']}


def build(store, run_id, *, offline=False):
    run = store.run(run_id)
    plan = store.get_plan(run['plan'])
    offline = offline or plan.get('offline_simulation', False)
    events = store.events(run_id)
    activity_all = activity(events)
    waits = exception_waits(events, dt.datetime.now(dt.timezone.utc).timestamp())
    records = []
    for item in run['targets']:
        data = item['data']
        target = next(row for row in plan['targets'] if row['label'] == item['target'])
        models = data.get('model_results', {})
        channels = data.get('channel_results', {})
        statuses = data.get('original_statuses', {})
        required_models = data.get('required_models', list(models))
        required_channels = sorted(f'{site}:{channel_id}' for site, rows in statuses.items()
                                   for channel_id, status in rows.items() if status == 1 and
                                   f'{site}:{channel_id}' not in data.get('held_channels', []))
        deployment = data.get('deployment') or {}
        platform = deployment.get('deployment') or {}
        elapsed = None
        if data.get('completed_at') and data.get('started_at'):
            elapsed = round(data['completed_at'] - data['started_at'], 3)
        start_event = next((event['at'] for event in events if event['target'] == item['target'] and 'started_at' in event['data']
                            and event['event'] not in METADATA_EVENTS), None)
        finish_event = next((event['at'] for event in events if event['target'] == item['target'] and event['event'] in TERMINAL), None)
        blocked = [(max(start_event, left), min(finish_event, right)) for left, right in waits
                   if start_event is not None and finish_event is not None and left < finish_event and right > start_event]
        blocked_seconds = union_seconds(blocked)
        records.append({'target': item['target'], 'origin': target['origin'], 'kind': target['kind'],
                        'branch': target.get('branch'), 'phase': item['phase'], 'seconds': elapsed,
                        'exception_wait_seconds': blocked_seconds,
                        'maintenance_seconds': round(max(0, elapsed - blocked_seconds), 3) if elapsed is not None else None,
                        'drain': data.get('drain'), 'rollback_drain': data.get('rollback_drain'),
                        'deployment': {'state': deployment.get('state'), 'identity': deployment.get('identity'),
                                       'commit': platform.get('commitSHA'), 'image_id': deployment.get('image_id')}
                        if deployment else None,
                        'models': models, 'rollback_models': data.get('rollback_model_results', {}),
                        'required_models': required_models, 'models_not_called': sorted(set(required_models) - models.keys()),
                        'channels': channels, 'original_statuses': statuses, 'required_channels': required_channels,
                        'channels_not_called': sorted(set(required_channels) - channels.keys()),
                        'restored': data.get('original_routing_restored', False),
                        'held_channels': data.get('held_channels', []),
                        'fast_model_tests': data.get('fast_model_tests', []),
                        'acceptance_mode': data.get('acceptance_mode', plan.get('acceptance_mode', 'full')),
                        'stop_propagation': data.get('stop_propagation'),
                        'restore_propagation': data.get('restore_propagation'),
                        'newapi_not_restarted': data.get('newapi_not_restarted'),
                        'rollback_attempted': data.get('rollback_attempted', False),
                        'error': data.get('last_error'), 'warnings': data.get('warnings', []),
                        'configuration_fingerprint': data.get('verified_fingerprint'),
                        'configuration_adjustments': data.get('configuration_adjustments', []),
                        'excluded': data.get('excluded_tests', []), 'postflight': data.get('postflight'),
                        'activity': activity(events, item['target']), 'started_at': data.get('started_at'),
                        'completed_at': data.get('completed_at')})
    # State transitions have timestamps, so interrupted stages also remain auditable.
    durations = {}
    previous_by_target = {}
    for current in events:
        if current['event'] in METADATA_EVENTS or current['target'] is None:
            continue
        previous = previous_by_target.get(current['target'])
        previous_by_target[current['target']] = current
        if previous and previous['event'] not in TERMINAL:
            key = previous['target'] + ':' + previous['event']
            start, end = previous['at'], current['at']
            blocked = [(max(start, left), min(end, right)) for left, right in waits if left < end and right > start]
            durations[key] = round(durations.get(key, 0) + max(0, end - start - union_seconds(blocked)), 3)
    grouped = defaultdict(list)
    for event in events:
        if event['event'] == 'operation_finished':
            data = event['data']
            grouped[f"{event['target']}:{data.get('phase', 'preparation')}"].append((data['started_at'], data['started_at'] + data['seconds']))
    measured = {key: union_seconds(intervals) for key, intervals in grouped.items()}
    gaps = []
    for previous, current in zip(records, records[1:]):
        if previous['completed_at'] is not None and current['started_at'] is not None:
            gaps.append({'after': previous['target'], 'before': current['target'],
                         'seconds': round(max(0, current['started_at'] - previous['completed_at']), 3)})
    release = {key: value for key, value in run['data'].get('release', plan['release']).items()
               if key not in {'binary', 'package'}}
    modes = {row['acceptance_mode'] for row in records}
    acceptance_mode = next(iter(modes)) if len(modes) == 1 else 'mixed'
    return {'schema': 1, 'run': run_id, 'status': run['status'], 'offline_simulation': offline,
            'acceptance_mode': acceptance_mode,
            'production_verified': not offline and run['status'] == 'complete' and not any(row['fast_model_tests'] for row in records),
            'release': release, 'generated_at': dt.datetime.now(dt.timezone(dt.timedelta(hours=8))).isoformat(),
            'records': records, 'stage_seconds': durations,
            'measured_stage_seconds': measured, 'activity': activity_all, 'queue_gaps': gaps,
            'preparation_failures': [{'target': event['target'], **event['data']} for event in events
                                     if event['event'] == 'preparation_failed'],
            'exception_wait_seconds': union_seconds(waits),
            'unverified': ['Offline simulation only; no production server was updated or called.'] if offline else
            ['Text model availability only; attachment behavior and performance were not tested.']
            if 'availability' in modes else
            ['Excluded capabilities are not verified; smoke timings are not capacity or performance benchmarks.']}


def write(store, run_id, *, offline=False):
    value = build(store, run_id, offline=offline)
    destination = store.root / 'reports' / run_id
    destination.mkdir(parents=True, exist_ok=True)
    json_path, html_path = destination / 'REPORT.json', destination / 'REPORT_CN.html'
    json_path.write_text(json.dumps(value, ensure_ascii=False, indent=2), encoding='utf-8')
    escape = lambda data: html.escape(str(data))
    rows = []
    for row in value['records']:
        passed = sum(item.get('success') is True for item in row['models'].values())
        channels = sum(item.get('success') is True for item in row['channels'].values())
        rows.append('<tr>' + ''.join('<td>' + escape(cell) + '</td>' for cell in
                                    [row['target'], row['origin'], row['branch'] or 'Docker', row['phase'],
                                     f'{passed}/{len(row["required_models"])}', f'{channels}/{len(row["required_channels"])}',
                                     row['restored'], row['maintenance_seconds']]) + '</tr>')
    details = ''.join('<details><summary>' + escape(row['target']) + '：验收证据</summary><pre>' +
                      escape(json.dumps(row, ensure_ascii=False, indent=2)) + '</pre></details>' for row in value['records'])
    offline = value['offline_simulation']
    title = 'CLIProxyAPI 更新离线演练报告' if offline else 'CLIProxyAPI 更新验收报告'
    acceptance = {'availability': '文本模型可用性', 'full': '完整功能验收',
                  'mixed': '各台按记录中的验收方式执行'}[value['acceptance_mode']]
    body = f'''<!doctype html><html lang="zh-CN"><meta charset="utf-8"><title>{title}</title>
<style>body{{font:15px system-ui;max-width:1250px;margin:32px auto;padding:0 20px;color:#183049}}table{{border-collapse:collapse;width:100%}}td,th{{border:1px solid #ccd7e0;padding:9px;text-align:left}}th{{background:#edf3f8}}pre{{white-space:pre-wrap;overflow-wrap:anywhere}}summary{{cursor:pointer;padding:14px 0}}.note{{padding:16px;background:#fff1cc}}</style>
<h1>{title}</h1><p class="note">{escape('仅离线模拟，未更新、停流、重启或调用任何生产服务器。' if offline else '依据实际执行记录；排除项和未完成项目不代表已验证。')}</p>
<p>状态：{escape(value['status'])}；提交：{escape(value['release']['commit'])}；生成时间：{escape(value['generated_at'])}</p>
<p>验收方式：{escape(acceptance)}</p>
<p>操作执行 {escape(value['activity']['execution_seconds'])} 秒 · 未与操作重叠的观察及排空等待 {escape(value['activity']['wait_seconds'])} 秒 · 预取交接等待 {escape(value['activity']['queue_wait_seconds'])} 秒 · 异常等待 {escape(value['exception_wait_seconds'])} 秒</p>
<table><thead><tr><th>服务器</th><th>地址</th><th>分支／方式</th><th>阶段</th><th>模型成功</th><th>渠道成功</th><th>原路由恢复</th><th>维护秒数</th></tr></thead><tbody>{''.join(rows)}</tbody></table>
{details}<h2>发布清单</h2><pre>{escape(json.dumps(value['release'], ensure_ascii=False, indent=2))}</pre><h2>操作区间合并后的阶段耗时</h2><pre>{escape(json.dumps(value['measured_stage_seconds'], ensure_ascii=False, indent=2))}</pre><h2>队列间隔与预检失败</h2><pre>{escape(json.dumps({'gaps': value['queue_gaps'], 'preparation_failures': value['preparation_failures']}, ensure_ascii=False, indent=2))}</pre><h2>缓存命中与重试</h2><pre>{escape(json.dumps({'cache_hits': value['activity']['cache_hits'], 'retries': value['activity']['retries']}, ensure_ascii=False, indent=2))}</pre></html>'''
    html_path.write_text(body, encoding='utf-8')
    return {'html': str(html_path), 'json': str(json_path)}

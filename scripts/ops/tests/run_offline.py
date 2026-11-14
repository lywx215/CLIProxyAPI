"""Offline regression suite and reproducible scenario reports; outbound connects denied."""
import argparse
import datetime as dt
import html
import json
from pathlib import Path
import socket
import sys
import tempfile
import time
import unittest
import uuid
from unittest.mock import patch

from scripts.ops.core import OpsError, validate_selection
from scripts.ops.engine import Engine
from scripts.ops.report import write as write_report
from scripts.ops.security import Vault
from scripts.ops.store import Lease, Store
from scripts.ops.tests.fakes import COMMIT, Clock, Driver, Releases
from scripts.vps import maintenance


class Result(unittest.TextTestResult):
    def __init__(self, *args, **kwargs):
        super().__init__(*args, **kwargs)
        self.records = []

    def startTest(self, test):
        self.started = time.monotonic()
        super().startTest(test)

    def addSuccess(self, test):
        self.records.append({'test': test.id(), 'success': True, 'seconds': round(time.monotonic() - self.started, 3)})
        super().addSuccess(test)

    def addFailure(self, test, err):
        self.records.append({'test': test.id(), 'success': False, 'error_type': err[0].__name__})
        super().addFailure(test, err)

    def addError(self, test, err):
        self.records.append({'test': test.id(), 'success': False, 'error_type': err[0].__name__})
        super().addError(test, err)

    def addSkip(self, test, reason):
        self.records.append({'test': test.id(), 'skipped': True})
        super().addSkip(test, reason)


def scenario(directory, failure=None):
    # Blocked scenarios intentionally retain leases. Never delete or reuse them.
    directory = directory / uuid.uuid4().hex[:12]
    store = Store(directory)
    driver, releases, clock = Driver(), Releases(), Clock()
    if failure == 'next_preflight':
        def prefetch(target, release, options):
            raise OpsError('oauth_application_secret_missing', category='authorization')
        driver.prepare = prefetch
    else:
        driver.failure = failure
    targets = validate_selection(driver.inventory, list(driver.inventory['targets']))
    plan = store.plan({'schema': 2, 'offline_simulation': True, 'targets': targets,
                       'acceptance_mode': 'availability',
                       'release': {'commit': COMMIT, 'page_sha256': 'synthetic-frontend'},
                       'inventory_fingerprint': driver.inventory_fingerprint()})
    run = store.begin(plan)
    engine = Engine(store, Lease(directory / 'fixture-lease.json', directory / 'fixture-hot.json'), driver, releases,
                    clock=clock, monotonic=clock, sleep=clock.sleep)
    result = engine.run(run)
    return {'run': run, 'status': result['status'], **write_report(store, run)}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output', type=Path, default=Path(__file__).resolve().parents[3] / 'temp/ops-offline-acceptance')
    args = parser.parse_args()
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=True)
    scratch = output / 'scratch'
    scratch.mkdir(exist_ok=True)
    # Some Windows sandboxes virtualize system TEMP; use the explicit workspace directory.
    tempfile.tempdir = str(scratch)
    sys.modules['maintenance'] = maintenance
    loader = unittest.TestLoader()
    suite = loader.discover(str(Path(__file__).parent), top_level_dir=str(Path(__file__).resolve().parents[3]))
    suite.addTests(loader.loadTestsFromName('scripts.vps.test_maintenance'))
    with patch.object(socket, 'create_connection', side_effect=AssertionError('Offline suite forbids outbound connects')), \
            patch.object(socket.socket, 'connect', side_effect=AssertionError('Offline suite forbids outbound connects')):
        result = unittest.TextTestRunner(verbosity=2, resultclass=Result).run(suite)
        scenarios = {}
        if result.wasSuccessful():
            for name, failure in [('success', None), ('regression_rollback', 'program_regression'),
                                  ('quota_blocked', 'quota'), ('next_preflight_blocked', 'next_preflight')]:
                scenarios[name] = scenario(output / 'scenarios' / name, failure)
    evidence = {'offline_simulation': True, 'production_operations': 0, 'tests_run': result.testsRun,
                'failures': len(result.failures), 'errors': len(result.errors), 'skipped': len(result.skipped),
                'passed': result.wasSuccessful(), 'tests': result.records, 'scenarios': scenarios,
                'generated_at': dt.datetime.now(dt.timezone.utc).isoformat()}
    (output / 'ACCEPTANCE.json').write_text(json.dumps(evidence, ensure_ascii=False, indent=2), encoding='utf-8')
    links = ''.join('<li><a href="' + html.escape(Path(row['html']).relative_to(output).as_posix()) + '">' +
                    html.escape(name) + '</a> — ' + html.escape(row['status']) + '</li>' for name, row in scenarios.items())
    text = f'''<!doctype html><html lang="zh-CN"><meta charset="utf-8"><title>CLIProxyAPI 更新工具离线验收</title>
<style>body{{font:16px system-ui;max-width:1100px;margin:40px auto;padding:0 24px;color:#183049}}.note{{padding:16px;background:#fff1cc}}pre{{white-space:pre-wrap;overflow-wrap:anywhere}}</style>
<h1>CLIProxyAPI 更新工具离线验收</h1><p class="note">仅本机程序和模拟验收；未停流、更新、重启或调用任何生产服务器。模拟目标使用保留测试地址，模拟提交不是生产版本。</p>
<p>测试 {result.testsRun} 项；失败 {len(result.failures)}；错误 {len(result.errors)}；跳过 {len(result.skipped)}。</p>
<h2>状态机演练</h2><ul>{links}</ul><h2>离线测试明细</h2><pre>{html.escape(json.dumps(result.records, ensure_ascii=False, indent=2))}</pre>
<p>真实生产端到端验收仍需下一次明确授权的单台维护。</p></html>'''
    (output / 'ACCEPTANCE_CN.html').write_text(text, encoding='utf-8')
    print(json.dumps({'report': str(output / 'ACCEPTANCE_CN.html'), 'passed': result.wasSuccessful(),
                      'tests_run': result.testsRun, 'production_operations': 0}))
    return 0 if result.wasSuccessful() else 1


if __name__ == '__main__':
    sys.exit(main())

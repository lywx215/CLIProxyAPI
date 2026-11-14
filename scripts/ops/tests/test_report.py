import unittest
from unittest.mock import MagicMock

from scripts.ops.report import activity, build


def event(name, timestamp, data, target='one'):
    return {'target': target, 'event': name, 'at': timestamp, 'data': data}


class ReportTests(unittest.TestCase):
    def test_intervals_merge_and_overlapped_wait_is_not_counted_twice(self):
        def operation(start, seconds, kind='read'):
            return event('operation_finished', start + seconds, {'started_at': start, 'seconds': seconds, 'kind': kind})

        result = activity([operation(10, 10), operation(14, 9), operation(20, 10, 'wait'),
                           operation(30, 2, 'queue'), event('cache_hit', 32, {'scope': 'quota'}),
                           event('retry', 33, {'scope': 'call', 'category': 'upstream'})])
        self.assertEqual(result['execution_seconds'], 13)
        self.assertEqual(result['wait_seconds'], 7)
        self.assertEqual(result['queue_wait_seconds'], 2)
        self.assertEqual(result['cache_hits'], {'quota': 1})
        self.assertEqual(result['retries'][0]['category'], 'upstream')

    def test_terminal_and_interrupted_gaps_are_not_counted_as_active_maintenance(self):
        store = MagicMock()
        store.get_plan.return_value = {'release': {'commit': 'synthetic'}, 'targets': [
            {'label': label, 'origin': 'https://' + label + '.example.test', 'kind': 'zeabur'} for label in ('one', 'two')]}
        store.run.return_value = {'plan': 'plan', 'data': {}, 'status': 'complete', 'targets': [
            {'target': 'one', 'phase': 'complete', 'data': {'started_at': 10, 'completed_at': 30}},
            {'target': 'two', 'phase': 'complete', 'data': {'started_at': 50, 'completed_at': 60}}]}
        store.events.return_value = [event('pending', 10, {'started_at': 10}), event('staged', 12, {}),
                                     event('run_status', 15, {'status': 'blocked'}, target=None),
                                     event('run_status', 25, {'status': 'running'}, target=None),
                                     event('complete', 30, {'completed_at': 30}),
                                     event('pending', 50, {'started_at': 50}, 'two'),
                                     event('complete', 60, {'completed_at': 60}, 'two')]
        result = build(store, 'run')
        self.assertEqual(result['exception_wait_seconds'], 10)
        self.assertEqual(result['records'][0]['maintenance_seconds'], 10)
        self.assertEqual(result['stage_seconds'], {'one:pending': 2, 'one:staged': 8, 'two:pending': 10})
        self.assertEqual(result['queue_gaps'], [{'after': 'one', 'before': 'two', 'seconds': 20}])

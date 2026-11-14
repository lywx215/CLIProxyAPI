"""A single read-only lookahead task, joined before its maintenance may begin."""
from .concurrency import TaskExecutor, background_reads
from .telemetry import recording, operation, note
from .plans import validate_observed_options


class Lookahead:
    def __init__(self, driver, store, run_id, release):
        self.driver, self.store, self.run_id, self.release = driver, store, run_id, release
        self.executor = TaskExecutor(max_workers=1)
        self.future, self.label = None, None

    def __enter__(self):
        return self

    def __exit__(self, *_args):
        self.executor.shutdown(wait=True, cancel_futures=True)

    def start(self, target, options):
        if not hasattr(self.driver, 'prepare') or self.future is not None:
            return
        self.label = target['label']
        def read():
            with background_reads(), recording(self.store, self.run_id, self.label):
                prepared = self.driver.prepare(target, self.release, {'web_model_tests': list(options['deferred_web_tests'])})
                if options['held_channels'] or options['fast_model_tests'] or options['approved_routing']:
                    validate_observed_options(prepared['target'], prepared['routing'], options['deferred_web_tests'],
                                              options['held_channels'], options['fast_model_tests'],
                                              approved_routing=options['approved_routing'],
                                              include_status=options['routing_states_frozen'])
                self.store.seal(self.run_id, self.label + ':prefetched', prepared)
                note('preparation_ready', background=True)
                return prepared
        self.future = self.executor.submit(read)

    def take(self, label):
        if self.future is None or self.label != label:
            return None
        try:
            with recording(self.store, self.run_id, label), operation('queue_handoff', kind='queue'):
                return self.future.result()
        finally:
            self.future, self.label = None, None

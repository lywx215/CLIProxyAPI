"""Shared read capacity and context propagation for foreground and lookahead work."""
from concurrent.futures import ThreadPoolExecutor
from contextlib import contextmanager
from contextvars import ContextVar, copy_context
import threading


BACKGROUND_READS = ContextVar('cpa_background_reads', default=False)


class TaskExecutor(ThreadPoolExecutor):
    def submit(self, function, /, *args, **kwargs):
        context = copy_context()
        return super().submit(context.run, function, *args, **kwargs)


@contextmanager
def background_reads():
    token = BACKGROUND_READS.set(True)
    try:
        yield
    finally:
        BACKGROUND_READS.reset(token)


class ReadLimiter:
    def __init__(self, capacity=4, background_capacity=2):
        self.capacity, self.background_capacity = capacity, background_capacity
        self.active = self.background_active = self.foreground_waiters = 0
        self.condition = threading.Condition()
        self.local = threading.local()

    def __enter__(self):
        depth = getattr(self.local, 'depth', 0)
        if depth:
            self.local.depth += 1
            return self
        background = BACKGROUND_READS.get()
        with self.condition:
            if not background:
                self.foreground_waiters += 1
            try:
                while self.active >= self.capacity or (background and (
                        self.background_active >= self.background_capacity or self.foreground_waiters)):
                    self.condition.wait()
                self.active += 1
                self.background_active += int(background)
                self.local.depth, self.local.background = 1, background
            finally:
                if not background:
                    self.foreground_waiters -= 1
        return self

    def __exit__(self, *_args):
        self.local.depth -= 1
        if self.local.depth:
            return
        with self.condition:
            self.active -= 1
            self.background_active -= int(self.local.background)
            self.condition.notify_all()

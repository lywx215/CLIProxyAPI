"""Operation timings and safe cache/retry events inherited by worker contexts."""
from contextlib import contextmanager
from contextvars import ContextVar
import re
import time
import uuid


SINK = ContextVar('cpa_operation_sink', default=None)
PHASE = ContextVar('cpa_operation_phase', default='preparation')


@contextmanager
def recording(store, run_id, target):
    token = SINK.set(lambda event, data: store.event(run_id, target, event, data))
    phase = PHASE.set('preparation')
    try:
        yield
    finally:
        SINK.reset(token)
        PHASE.reset(phase)


def note(event, **data):
    sink = SINK.get()
    if sink:
        sink(event, {'phase': PHASE.get(), **data})


@contextmanager
def operation(name, *, kind='read'):
    identity, start, monotonic = uuid.uuid4().hex, time.time(), time.monotonic()
    note('operation_started', operation_id=identity, name=name, kind=kind, started_at=start)
    success, code = False, None
    try:
        yield
        success = True
    except BaseException as error:
        candidate = getattr(error, 'code', type(error).__name__)
        code = candidate if isinstance(candidate, str) and re.fullmatch(r'[A-Za-z0-9_]{1,96}', candidate) else 'operation_failed'
        raise
    finally:
        note('operation_finished', operation_id=identity, name=name, kind=kind, started_at=start,
             ended_at=time.time(), seconds=max(0, time.monotonic() - monotonic), success=success, code=code)

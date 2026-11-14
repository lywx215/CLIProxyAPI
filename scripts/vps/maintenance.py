"""Pure maintenance decisions; this module never changes routing or services."""
from dataclasses import dataclass
import hashlib
import json
import math


FALLBACK_WAIT_SECONDS = 180


@dataclass(frozen=True)
class DrainDecision:
    ready: bool
    criterion: str
    completion_verified: bool = False


def request_log_marker(record, total_count):
    """Fingerprint a log observation without relying on a page display ID."""
    if type(total_count) is not int or total_count < 0:
        raise ValueError('A successful log read with a valid count is required.')
    if record is None:
        if total_count != 0:
            raise ValueError('Nonempty log history has no latest record.')
        return 'empty'
    if not isinstance(record, dict) or not record.get('created_at'):
        raise ValueError('Log identity is unavailable.')
    # Some New API log backends renumber IDs on every result page.
    stable = {key: value for key, value in record.items() if key != 'id'}
    data = json.dumps([total_count, stable], sort_keys=True, ensure_ascii=True, separators=(',', ':')).encode()
    return hashlib.sha256(data).hexdigest()


def evaluate_drain(*, channels_stopped, elapsed_seconds, counts_reliable=False,
                   baseline=None, remaining=None, log_quiet_seconds=None):
    """Evaluate the user's 95% rule and authorized 60s/180s fallback.

    Log quiet time must describe observations after confirmed channel stoppage;
    historical timestamps alone are not a continuous observation window. Callers
    pass None when logs cannot be observed reliably. A decision grants no API or
    service permission and does not implement stop, restart, health, or restore.
    """
    if not math.isfinite(elapsed_seconds) or elapsed_seconds < 0:
        raise ValueError('Elapsed time must be finite and nonnegative.')
    if not channels_stopped:
        return DrainDecision(False, 'channels_not_stopped')
    if counts_reliable and baseline is not None and remaining is not None:
        if (type(baseline) is not int or type(remaining) is not int
                or baseline < 0 or remaining < 0 or remaining > baseline):
            raise ValueError('Invalid live-request counts.')
        if remaining * 100 <= baseline * 5:
            return DrainDecision(True, 'request_completion_95_percent', True)
        return DrainDecision(False, 'waiting_for_request_completion')
    if log_quiet_seconds is not None:
        if (not math.isfinite(log_quiet_seconds) or log_quiet_seconds < 0
                or log_quiet_seconds > elapsed_seconds):
            raise ValueError('Log quiet time must be observed after channel stoppage.')
        if log_quiet_seconds >= 60:
            return DrainDecision(True, 'newapi_logs_quiet_60_seconds')
    if elapsed_seconds >= FALLBACK_WAIT_SECONDS:
        return DrainDecision(True, 'fallback_wait_limit_180_seconds')
    return DrainDecision(False, 'waiting_for_log_or_time_fallback')

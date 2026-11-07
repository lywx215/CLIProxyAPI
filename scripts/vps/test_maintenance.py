"""Deterministic tests for restart admission; no clocks, API calls, or sleeps."""
import unittest
from maintenance import evaluate_drain, request_log_marker


class DrainDecisionTests(unittest.TestCase):
    def test_confirmed_channel_stoppage_is_always_required(self):
        result = evaluate_drain(channels_stopped=False, elapsed_seconds=600,
                                log_quiet_seconds=120, counts_reliable=True, baseline=0, remaining=0)
        self.assertFalse(result.ready)

    def test_exact_95_percent_boundary(self):
        self.assertTrue(evaluate_drain(channels_stopped=True, elapsed_seconds=0,
                                      counts_reliable=True, baseline=20, remaining=1).ready)
        self.assertFalse(evaluate_drain(channels_stopped=True, elapsed_seconds=0,
                                       counts_reliable=True, baseline=19, remaining=1).ready)

    def test_verified_zero_baseline_can_proceed(self):
        result = evaluate_drain(channels_stopped=True, elapsed_seconds=0,
                                counts_reliable=True, baseline=0, remaining=0)
        self.assertTrue(result.ready)
        self.assertTrue(result.completion_verified)

    def test_unreliable_zero_counter_does_not_prove_completion(self):
        result = evaluate_drain(channels_stopped=True, elapsed_seconds=0,
                                baseline=0, remaining=0)
        self.assertFalse(result.ready)

    def test_60_seconds_of_observed_log_silence_allows_fallback(self):
        before = evaluate_drain(channels_stopped=True, elapsed_seconds=59, log_quiet_seconds=59)
        after = evaluate_drain(channels_stopped=True, elapsed_seconds=60, log_quiet_seconds=60)
        self.assertFalse(before.ready)
        self.assertTrue(after.ready)
        self.assertFalse(after.completion_verified)

    def test_new_logs_restart_the_quiet_window(self):
        result = evaluate_drain(channels_stopped=True, elapsed_seconds=120, log_quiet_seconds=3)
        self.assertFalse(result.ready)

    def test_failed_log_observation_uses_the_300_second_limit(self):
        self.assertFalse(evaluate_drain(channels_stopped=True, elapsed_seconds=299).ready)
        result = evaluate_drain(channels_stopped=True, elapsed_seconds=300)
        self.assertTrue(result.ready)
        self.assertFalse(result.completion_verified)

    def test_busy_logs_cannot_extend_fallback_past_five_minutes(self):
        result = evaluate_drain(channels_stopped=True, elapsed_seconds=300, log_quiet_seconds=0)
        self.assertTrue(result.ready)

    def test_reliable_incomplete_count_does_not_use_unavailable_count_fallback(self):
        result = evaluate_drain(channels_stopped=True, elapsed_seconds=600,
                                counts_reliable=True, baseline=100, remaining=6, log_quiet_seconds=120)
        self.assertFalse(result.ready)

    def test_old_historical_logs_do_not_instantly_satisfy_the_quiet_window(self):
        with self.assertRaises(ValueError):
            evaluate_drain(channels_stopped=True, elapsed_seconds=1, log_quiet_seconds=300)

    def test_renumbered_page_id_cannot_hide_a_new_request(self):
        first = request_log_marker({'id': 1, 'created_at': 100, 'request_id': 'request-a'}, 20)
        second = request_log_marker({'id': 1, 'created_at': 100, 'request_id': 'request-b'}, 21)
        self.assertNotEqual(first, second)

    def test_display_id_changes_do_not_create_false_activity(self):
        first = request_log_marker({'id': 1, 'created_at': 100, 'request_id': 'request-a'}, 20)
        second = request_log_marker({'id': 999, 'created_at': 100, 'request_id': 'request-a'}, 20)
        self.assertEqual(first, second)

    def test_missing_log_evidence_cannot_be_treated_as_empty_history(self):
        with self.assertRaises(ValueError):
            request_log_marker(None, 5)
        with self.assertRaises(ValueError):
            request_log_marker({}, 0)


if __name__ == '__main__':
    unittest.main()

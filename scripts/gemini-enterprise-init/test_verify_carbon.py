"""Offline checks for real-call scope, saved proof, and no-replay journals."""

import copy
from contextlib import contextmanager
from pathlib import Path
import tempfile
import unittest

import initialize as init
import verify_carbon as live
from test_initialize import EMAIL, LOCATION, PROJECT
from test_proxy_carbon import FakeManagement
import test_proxy_carbon as proxy_fixtures


@contextmanager
def journal_directory():
    root = Path(__file__).resolve().parents[2] / "temp"
    root.mkdir(exist_ok=True)
    with tempfile.TemporaryDirectory(dir=root, prefix="carbon-verification-test-") as directory:
        assert Path(directory).resolve().is_relative_to(root.resolve())
        yield directory


class FakeTests(FakeManagement):
    def __init__(self, app):
        super().__init__(app)
        self.root["gemini_enterprise"]["collections"][0]["carbon_admission"]["can_test"] = True
        self.failure = False
        self.unknown = False

    def test(self, payload):
        self.events.append({"method": "POST", "path": "/v8/management/credentials/models/test", "http": 200})
        if self.unknown:
            raise init.InitError("carbon_test_transport_unknown_no_retry")
        proof = {"test_id": "synthetic-test", "status": "verified", "upstream_model": init.CARBON}
        return 200, {"status": "failed" if self.failure else "ok", "code": "MODEL_NOT_ENABLED" if self.failure else "ok", "http_status": 403 if self.failure else 200, "model": init.CARBON, "upstream_model": init.CARBON, "binding_id": payload["binding_id"], "auth_index": payload["auth_index"], "resources": copy.deepcopy(payload["expected"]), "expected_revision": payload["expected_revision"], "revision": payload["expected_revision"], "carbon_test_id": "synthetic-test", "carbon_admission": {"enabled": False, "available": False, "test": proof}}


class CarbonVerificationTests(unittest.TestCase):
    def fixture(self):
        initializer, management = proxy_fixtures.ProxyCarbonTests().setup_trial()
        return initializer, FakeTests(management.root["gemini_enterprise"]["collections"][0])

    def run_journal(self, api, initializer, path):
        journal = live.Journal(path, {"email": EMAIL, "model": init.CARBON})
        return live.verify(api, initializer, [PROJECT], [LOCATION], journal, 1)

    def test_success_is_verified_once_and_reused_without_another_call(self):
        initializer, api = self.fixture()
        with journal_directory() as directory:
            path = Path(directory) / "journal.jsonl"
            first = self.run_journal(api, initializer, path)
            self.assertTrue(first["complete"])
            self.assertEqual(first["verified"], 1)
            second = self.run_journal(api, initializer, path)
            self.assertTrue(second["complete"])
            self.assertEqual(second["calls_submitted_this_run"], 0)
            self.assertEqual(len(api.events), 1)

    def test_paid_and_mixed_regions_never_submit_a_model_test(self):
        for mixed in (False, True):
            initializer, api = self.fixture()
            initializer.api.config_items = ([{"freeTrial": True}] if mixed else []) + [{"state": "ACTIVE"}]
            with journal_directory() as directory:
                result = self.run_journal(api, initializer, Path(directory) / "journal.jsonl")
            self.assertEqual(result["paid_skipped"], 1)
            self.assertFalse(api.events)

    def test_http_200_with_a_failed_generation_is_not_a_success(self):
        initializer, api = self.fixture()
        api.failure = True
        with journal_directory() as directory:
            result = self.run_journal(api, initializer, Path(directory) / "journal.jsonl")
        self.assertFalse(result["complete"])
        self.assertEqual(result["verified"], 0)
        self.assertEqual(result["results"][0]["code"], "MODEL_NOT_ENABLED")

    def test_unknown_submission_is_never_automatically_replayed(self):
        initializer, api = self.fixture()
        api.unknown = True
        with journal_directory() as directory:
            path = Path(directory) / "journal.jsonl"
            self.assertFalse(self.run_journal(api, initializer, path)["complete"])
            self.assertFalse(self.run_journal(api, initializer, path)["complete"])
            self.assertEqual(len(api.events), 1)

    def test_actual_model_and_binding_must_match_saved_proof(self):
        initializer, api = self.fixture()
        app = api.root["gemini_enterprise"]["collections"][0]
        payload = {"name": "synthetic.json", "auth_index": app["auth_index"], "binding_id": app["binding_id"], "expected_revision": app["revision"], "expected": {k:app[k] for k in live.RESOURCE_KEYS}, "model": init.CARBON}
        http, value = api.test(payload)
        for field, replacement in (("upstream_model", "gemini-4-argon"), ("binding_id", "other-binding"), ("revision", 999)):
            altered = {**value, field: replacement}
            self.assertFalse(live.outcome(http, altered, app)["generation_verified"])
        self.assertEqual(live.outcome(http, {**value, "upstream_model": "gemini-4-argon"}, app)["upstream_model"], "gemini-4-argon")
        transport = live.TestManagement("http://127.0.0.1:8317", "synthetic", False)
        with self.assertRaises(init.InitError):
            transport.test(payload)


if __name__ == "__main__":
    unittest.main()

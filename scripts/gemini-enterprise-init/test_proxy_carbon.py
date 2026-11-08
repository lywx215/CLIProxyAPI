"""Offline checks for paid protection and application-level Carbon admission."""

import copy
import unittest

import initialize as init
import proxy_carbon as proxy
from test_initialize import EMAIL, LOCATION, PROJECT, REGION, FakeGoogle


class FakeManagement:
    def __init__(self, app, apply=True):
        self.apply, self.events = apply, []
        self.root = {"name": "synthetic-account.json", "email": EMAIL, "auth_index": "root-index", "disabled": False, "gemini_enterprise": {"account_id": "synthetic-account", "carbon_admission_supported": True, "collections": [app]}}
        self.reads = 0
        self.race = False
        self.change_other_setting = False

    def account(self, email):
        self.reads += 1
        if self.race and self.reads == 3:
            self.root["gemini_enterprise"]["collections"][0]["revision"] += 1
        return copy.deepcopy(self.root)

    def request(self, method, path, body):
        assert method == "PATCH" and body["fields"] == {"carbon_enabled": True}
        self.events.append({"method": method, "path": path, "http": 200})
        app = next(app for app in self.root["gemini_enterprise"]["collections"] if app["binding_id"] == body["binding_id"])
        assert app["revision"] == body["expected_revision"]
        app["settings"]["carbon_enabled"] = True
        app["carbon_admission"].update(enabled=True, available=True)
        app["revision"] += 1
        if self.change_other_setting:
            app["settings"]["priority"] = 99
        return {"status": "ok", "carbon_admission": copy.deepcopy(app["carbon_admission"])}


class ProxyCarbonTests(unittest.TestCase):
    def setup_trial(self, apply=True):
        google = FakeGoogle()
        initializer = init.Initializer(google, EMAIL, sleep=lambda _: None)
        self.assertEqual(initializer.run_region(PROJECT, LOCATION)["status"], "complete")
        name = next(iter(google.engine_items))
        app = {"project_id": PROJECT, "location": LOCATION, "collection_id": "default_collection", "engine_id": name.rsplit("/", 1)[1], "assistant_id": "default_assistant", "binding_id": "synthetic-binding", "auth_index": "synthetic-index", "revision": 1, "disabled": False, "effective_disabled": False, "missing": False, "settings": {"priority": 3}, "model_policy_state": {"subscription": {"status": "ok", "free_trial": True, "state": "ACTIVE"}}, "carbon_admission": {"enabled": False, "available": False, "can_enable": True, "verification_required": False}}
        return initializer, FakeManagement(app, apply)

    def run_scope(self, management, initializer):
        return proxy.reconcile(management, initializer, [PROJECT], [LOCATION])

    def test_trial_enable_readback_and_second_run_have_no_duplicate_write(self):
        initializer, management = self.setup_trial()
        first = self.run_scope(management, initializer)
        self.assertTrue(first["complete"])
        app = first["regions"][0]["applications"][0]
        self.assertEqual(app["status"], "enabled")
        self.assertTrue(app["enabled_after"] and app["business_eligible"])
        self.assertFalse(app["generation_verified"])
        self.assertEqual(len(management.events), 1)
        self.assertEqual(management.root["gemini_enterprise"]["collections"][0]["settings"]["priority"], 3)
        self.assertTrue(self.run_scope(management, initializer)["complete"])
        self.assertEqual(len(management.events), 1)

    def test_paid_and_mixed_scopes_do_not_enable_proxy_carbon(self):
        for mixed in (False, True):
            initializer, management = self.setup_trial()
            initializer.api.config_items = ([{"freeTrial": True}] if mixed else []) + [{"state": "ACTIVE"}]
            report = self.run_scope(management, initializer)
            self.assertEqual(report["regions"][0]["status"], "paid_skipped")
            self.assertFalse(management.events)
            self.assertFalse(management.root["gemini_enterprise"]["collections"][0]["carbon_admission"]["enabled"])

    def test_paid_recheck_and_concurrent_edit_stop_before_patch(self):
        initializer, management = self.setup_trial()
        initializer.api.config_reads = 0
        initializer.api.paid_on_recheck = True
        result = self.run_scope(management, initializer)
        self.assertEqual(result["regions"][0]["error"], "paid_region_write_stopped")
        self.assertFalse(management.events)
        initializer, management = self.setup_trial()
        management.race = True
        result = self.run_scope(management, initializer)
        self.assertEqual(result["regions"][0]["error"], "proxy_application_changed_before_write")
        self.assertFalse(management.events)

    def test_unverified_admission_or_missing_import_does_not_write(self):
        initializer, management = self.setup_trial()
        app = management.root["gemini_enterprise"]["collections"][0]
        app["carbon_admission"]["verification_required"] = True
        self.assertFalse(self.run_scope(management, initializer)["complete"])
        self.assertFalse(management.events)
        management.root["gemini_enterprise"]["collections"] = []
        result = self.run_scope(management, initializer)
        self.assertEqual(result["regions"][0]["error"], "proxy_application_not_imported")

    def test_plan_and_unknown_google_state_do_not_write(self):
        initializer, management = self.setup_trial(apply=False)
        result = self.run_scope(management, initializer)
        self.assertEqual(result["regions"][0]["status"], "planned")
        self.assertFalse(management.events)
        initializer, management = self.setup_trial()
        next(iter(initializer.api.engine_items.values()))["modelConfigs"][init.CARBON] = "MODEL_DISABLED"
        self.assertFalse(self.run_scope(management, initializer)["complete"])
        self.assertFalse(management.events)

    def test_unrelated_setting_change_is_reported(self):
        initializer, management = self.setup_trial()
        management.change_other_setting = True
        result = self.run_scope(management, initializer)
        self.assertEqual(result["regions"][0]["error"], "proxy_unrelated_settings_changed")

    def test_transport_and_generation_are_outside_scope(self):
        for origin in ("http://example.test", "https://example.test/path", "https://user:password@example.test", "https://example.test/?key=value"):
            with self.subTest(origin=origin), self.assertRaises(init.InitError):
                proxy.Management(origin, "synthetic", True)
        api = proxy.Management("http://127.0.0.1:8317", "synthetic", True)
        with self.assertRaisesRegex(init.InitError, "management_operation_outside_scope"):
            api.request("POST", "/v8/management/credentials/models/test", {})
        with self.assertRaisesRegex(init.InitError, "management_write_outside_scope"):
            api.request("PATCH", "/v8/management/credentials/gemini-enterprise/collections", {"fields": {"carbon_enabled": False}})


if __name__ == "__main__":
    unittest.main()

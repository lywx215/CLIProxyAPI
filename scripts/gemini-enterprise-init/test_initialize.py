"""Offline regression tests for initialization scope and resource preservation."""

import copy
import json
from pathlib import Path
import unittest
from unittest.mock import patch
import urllib.parse

import initialize as init


PROJECT = "123456789"
LOCATION = "eu"
REGION = init.region_name(PROJECT, LOCATION)
EMAIL = "example@example.test"


class FakeGoogle:
    def __init__(self, apply=True):
        self.apply = apply
        self.events = []
        self.config_items = []
        self.user_items = []
        self.engine_items = {}
        self.assistant_items = {}
        self.config_reads = 0
        self.paid_on_recheck = False
        self.fail_users = False
        self.race_engine = False

    def pages(self, url, field):
        path = urllib.parse.urlsplit(url).path
        if field == "licenseConfigs":
            self.config_reads += 1
            if self.paid_on_recheck and self.config_reads > 1:
                return [{"name": REGION + "/licenseConfigs/paid", "state": "ACTIVE"}]
            return copy.deepcopy(self.config_items)
        if field == "userLicenses":
            if self.fail_users:
                raise init.APIError(403)
            return copy.deepcopy(self.user_items)
        if field == "collections":
            return [{"name": REGION + "/collections/default_collection"}]
        if field == "engines":
            return copy.deepcopy(list(self.engine_items.values()))
        raise AssertionError((path, field))

    def request(self, method, url, body=None):
        path = urllib.parse.urlsplit(url).path
        resource = path.split("/", 2)[2]
        self.events.append({"method": method, "path": path, "http": 200})
        if method == "POST" and path.endswith("/licenseConfigs"):
            value = {**copy.deepcopy(body), "name": REGION + "/licenseConfigs/" + init.TRIAL_ID, "state": "ACTIVE"}
            self.config_items.append(value)
            return copy.deepcopy(value)
        if method == "GET" and "/licenseConfigs/" in path:
            return copy.deepcopy(next(item for item in self.config_items if item["name"] == resource))
        if method == "POST" and path.endswith("/engines"):
            app_id = urllib.parse.parse_qs(urllib.parse.urlsplit(url).query)["engineId"][0]
            name = REGION + "/collections/default_collection/engines/" + app_id
            self.engine_items[name] = {**copy.deepcopy(body), "name": name, "features": {"keep": "FEATURE_STATE_ON"}}
            self.assistant_items[name + "/assistants/default_assistant"] = {"name": name + "/assistants/default_assistant", "generationConfig": {"defaultModelId": "keep-model", "systemInstruction": {"additionalSystemInstruction": "keep"}}, "defaultWebGroundingToggleOff": True}
            return {"done": True, "response": {"name": name}}
        if method == "POST" and ":batchUpdateUserLicenses" in path:
            incoming = copy.deepcopy(body["inlineSource"]["userLicenses"][0])
            incoming["licenseAssignmentState"] = "ASSIGNED"
            self.user_items = [item for item in self.user_items if item.get("userPrincipal") != incoming["userPrincipal"]] + [incoming]
            return {"done": True, "response": {}}
        if method == "GET" and "/widgetConfigs/" in path:
            engine_name = resource.split("/widgetConfigs/")[0]
            engine = self.engine_items[engine_name]
            assistant = self.assistant_items[engine_name + "/assistants/default_assistant"]
            view = urllib.parse.parse_qs(urllib.parse.urlsplit(url).query)["modelInfoView"][0]
            models = [{"modelId": "gemini-3.8-flash"}, {"modelId": "gemini-4-argon"}]
            return {"uiSettings": {"features": copy.deepcopy(engine.get("features", {})), "modelConfigInfo": {"resolvedModels": models}}, "assistantSettings": {"webGroundingType": assistant.get("webGroundingType"), "defaultWebGroundingToggleOff": assistant.get("defaultWebGroundingToggleOff", False)}}
        collection = self.assistant_items if "/assistants/" in path else self.engine_items
        if resource not in collection:
            raise init.APIError(404)
        if method == "GET":
            value = copy.deepcopy(collection[resource])
            if self.race_engine and collection is self.engine_items:
                collection[resource]["displayName"] = "changed concurrently"
                self.race_engine = False
            return value
        if method == "PATCH":
            for key, value in body.items():
                if key == "generationConfig":
                    collection[resource].setdefault(key, {}).update(copy.deepcopy(value))
                else:
                    collection[resource][key] = copy.deepcopy(value)
            return copy.deepcopy(collection[resource])
        raise AssertionError((method, path))


class InitializationTests(unittest.TestCase):
    def initializer(self, api):
        return init.Initializer(api, EMAIL, sleep=lambda _: None)

    def test_new_region_and_second_run_are_idempotent(self):
        api = FakeGoogle()
        api.user_items = [{"userPrincipal": "other@example.test", "licenseAssignmentState": "ASSIGNED", "licenseConfig": "other"}]
        tool = self.initializer(api)
        first = tool.run_region(PROJECT, LOCATION)
        self.assertEqual(first["status"], "complete")
        self.assertEqual(len(api.config_items), 1)
        self.assertTrue(api.config_items[0]["freeTrial"])
        self.assertFalse(api.config_items[0]["autoRenew"])
        self.assertEqual(len(api.engine_items), 1)
        self.assertTrue(any(item["userPrincipal"] == "other@example.test" for item in api.user_items))
        offset = len(api.events)
        second = tool.run_region(PROJECT, LOCATION)
        self.assertEqual(second["status"], "complete")
        self.assertFalse(any(event["method"] != "GET" for event in api.events[offset:]))

    def test_paid_region_and_mixed_paid_trial_are_never_written(self):
        for configs in ([{"state": "ACTIVE"}], [{"freeTrial": True}, {"freeTrial": False}]):
            with self.subTest(configs=configs):
                api = FakeGoogle()
                api.config_items = configs
                result = self.initializer(api).run_region(PROJECT, LOCATION)
                self.assertEqual(result["status"], "paid_skipped")
                self.assertFalse(any(event["method"] != "GET" for event in api.events))

    def test_paid_status_is_rechecked_before_first_write(self):
        api = FakeGoogle()
        api.paid_on_recheck = True
        result = self.initializer(api).run_region(PROJECT, LOCATION)
        self.assertEqual(result["error"], "paid_region_write_stopped")
        self.assertFalse(any(event["method"] != "GET" for event in api.events))

    def test_unknown_metadata_is_not_an_empty_region(self):
        api = FakeGoogle()
        api.fail_users = True
        result = self.initializer(api).run_region(PROJECT, LOCATION)
        self.assertEqual(result["status"], "failed")
        self.assertEqual(result["http"], 403)
        self.assertFalse(any(event["method"] != "GET" for event in api.events))

    def test_plan_is_read_only(self):
        api = FakeGoogle(apply=False)
        result = self.initializer(api).run_region(PROJECT, LOCATION)
        self.assertEqual(result["status"], "planned")
        self.assertFalse(any(event["method"] != "GET" for event in api.events))

    def test_explicit_range_and_unrelated_settings_survive(self):
        api = FakeGoogle()
        tool = self.initializer(api)
        self.assertEqual(tool.run_region(PROJECT, LOCATION)["status"], "complete")
        assistant = next(iter(api.assistant_items.values()))
        assistant["generationConfig"]["allowedModelIds"] = ["custom-model"]
        engine = next(iter(api.engine_items.values()))
        engine["features"]["model-selector"] = "FEATURE_STATE_OFF"
        engine["modelConfigs"][init.CARBON] = "MODEL_DISABLED"
        result = tool.run_region(PROJECT, LOCATION)
        self.assertEqual(result["status"], "complete")
        generation = assistant["generationConfig"]
        self.assertEqual(generation["defaultModelId"], "keep-model")
        self.assertEqual(generation["systemInstruction"]["additionalSystemInstruction"], "keep")
        self.assertTrue({"custom-model", init.CARBON} <= set(generation["allowedModelIds"]))
        self.assertEqual(engine["features"]["keep"], "FEATURE_STATE_ON")
        self.assertEqual(engine["features"]["model-selector"], "FEATURE_STATE_OFF")
        self.assertFalse(result["applications"][0]["model_selector"])

    def test_absent_and_empty_ranges_stay_implicit(self):
        for empty in (False, True):
            api = FakeGoogle()
            tool = self.initializer(api)
            self.assertEqual(tool.run_region(PROJECT, LOCATION)["status"], "complete")
            assistant = next(iter(api.assistant_items.values()))
            if empty:
                assistant["generationConfig"]["allowedModelIds"] = []
            tool.run_region(PROJECT, LOCATION)
            self.assertEqual(assistant["generationConfig"].get("allowedModelIds"), [] if empty else None)

    def test_concurrent_engine_edit_prevents_patch(self):
        api = FakeGoogle()
        tool = self.initializer(api)
        self.assertEqual(tool.run_region(PROJECT, LOCATION)["status"], "complete")
        api.race_engine = True
        offset = len(api.events)
        result = tool.run_region(PROJECT, LOCATION)
        self.assertEqual(result["error"], "resource_changed_before_write")
        self.assertFalse(any(event["method"] == "PATCH" for event in api.events[offset:]))

    def test_generation_is_outside_mutation_scope(self):
        api = FakeGoogle()
        with self.assertRaisesRegex(init.InitError, "operation_outside_initialization_scope"):
            self.initializer(api).mutate(PROJECT, LOCATION, "POST", REGION + "/collections/default_collection/engines/test/assistants/default_assistant:streamAssist", {})

    def test_remote_carbon_only_preserves_other_models_search_and_selector(self):
        api = FakeGoogle()
        self.assertEqual(self.initializer(api).run_region(PROJECT, LOCATION)["status"], "complete")
        engine = next(iter(api.engine_items.values()))
        assistant = next(iter(api.assistant_items.values()))
        engine["modelConfigs"][init.CARBON] = "MODEL_DISABLED"
        engine["modelConfigs"]["gemini-3.8-flash"] = "MODEL_DISABLED"
        engine["features"]["model-selector"] = "FEATURE_STATE_OFF"
        assistant["generationConfig"]["allowedModelIds"] = ["existing-custom"]
        assistant["webGroundingType"] = "WEB_GROUNDING_TYPE_DISABLED"
        assistant["defaultWebGroundingToggleOff"] = True
        offset = len(api.events)
        tool = init.Initializer(api, EMAIL, sleep=lambda _: None, carbon_only=True)
        result = tool.run_region(PROJECT, LOCATION)
        self.assertEqual(result["status"], "complete")
        remote = result["applications"][0]["carbon_remote"]
        self.assertEqual(remote["status"], "configured")
        self.assertEqual(remote["patches"], 2)
        self.assertEqual(engine["modelConfigs"][init.CARBON], "MODEL_ENABLED")
        self.assertEqual(engine["modelConfigs"]["gemini-3.8-flash"], "MODEL_DISABLED")
        self.assertEqual(engine["features"]["model-selector"], "FEATURE_STATE_OFF")
        self.assertEqual(assistant["generationConfig"]["allowedModelIds"], ["existing-custom", init.CARBON])
        self.assertEqual(assistant["generationConfig"]["defaultModelId"], "keep-model")
        self.assertEqual(assistant["webGroundingType"], "WEB_GROUNDING_TYPE_DISABLED")
        self.assertTrue(assistant["defaultWebGroundingToggleOff"])
        self.assertTrue(all(event["method"] in ("GET", "PATCH") for event in api.events[offset:]))

    def test_remote_carbon_keeps_implicit_range_and_is_idempotent(self):
        api = FakeGoogle()
        self.assertEqual(self.initializer(api).run_region(PROJECT, LOCATION)["status"], "complete")
        engine = next(iter(api.engine_items.values()))
        engine["modelConfigs"][init.CARBON] = "MODEL_DISABLED"
        tool = init.Initializer(api, EMAIL, sleep=lambda _: None, carbon_only=True)
        first = tool.run_region(PROJECT, LOCATION)
        remote = first["applications"][0]["carbon_remote"]
        self.assertEqual(remote["status"], "verification_required")
        self.assertTrue(remote["configuration_verified"])
        self.assertFalse(remote["generation_verified"])
        self.assertNotIn("allowedModelIds", next(iter(api.assistant_items.values()))["generationConfig"])
        offset = len(api.events)
        self.assertEqual(tool.run_region(PROJECT, LOCATION)["status"], "complete")
        self.assertFalse(any(event["method"] != "GET" for event in api.events[offset:]))

    def test_remote_carbon_paid_and_mixed_regions_remain_untouched(self):
        api = FakeGoogle()
        self.assertEqual(self.initializer(api).run_region(PROJECT, LOCATION)["status"], "complete")
        engine = next(iter(api.engine_items.values()))
        engine["modelConfigs"][init.CARBON] = "MODEL_DISABLED"
        api.config_items.append({"name": REGION + "/licenseConfigs/paid", "state": "ACTIVE"})
        offset = len(api.events)
        result = init.Initializer(api, EMAIL, carbon_only=True).run_region(PROJECT, LOCATION)
        self.assertEqual(result["status"], "paid_skipped")
        self.assertEqual(engine["modelConfigs"][init.CARBON], "MODEL_DISABLED")
        self.assertFalse(any(event["method"] != "GET" for event in api.events[offset:]))

    def test_remote_carbon_checks_paid_status_again(self):
        api = FakeGoogle()
        self.assertEqual(self.initializer(api).run_region(PROJECT, LOCATION)["status"], "complete")
        next(iter(api.engine_items.values()))["modelConfigs"][init.CARBON] = "MODEL_DISABLED"
        api.config_reads = 0
        api.paid_on_recheck = True
        offset = len(api.events)
        result = init.Initializer(api, EMAIL, carbon_only=True).run_region(PROJECT, LOCATION)
        self.assertEqual(result["error"], "paid_region_write_stopped")
        self.assertFalse(any(event["method"] != "GET" for event in api.events[offset:]))

    def test_remote_carbon_does_not_create_applications_or_assign_users(self):
        api = FakeGoogle()
        tool = init.Initializer(api, EMAIL, carbon_only=True)
        self.assertEqual(tool.run_region(PROJECT, LOCATION)["status"], "no_applications")
        self.assertFalse(api.events)
        self.assertEqual(self.initializer(api).run_region(PROJECT, LOCATION)["status"], "complete")
        api.user_items = []
        offset = len(api.events)
        result = tool.run_region(PROJECT, LOCATION)
        self.assertEqual(result["error"], "carbon_trial_assignment_unverified")
        self.assertFalse(any(event["method"] != "GET" for event in api.events[offset:]))


class CredentialTests(unittest.TestCase):
    def test_apply_requires_scope_before_reading_a_credential(self):
        with self.assertRaisesRegex(init.InitError, "apply_requires_project_scope"):
            init.main(["--auth-file", "synthetic-account.json", "--apply"])

    def test_project_discovery_is_dynamic_and_filters_inactive_projects(self):
        class DiscoveryAPI:
            class Credential:
                expected_email = EMAIL
                pinned_email = None
                def token(self):
                    return "synthetic"
            credential = Credential()
            def request(self, method, url):
                return {"email": EMAIL}
            def pages(self, url, field):
                return [{"name": "projects/" + str(number), "projectId": "project-" + str(number), "displayName": "Project", "state": "ACTIVE"} for number in range(1, 6)] + [{"name": "projects/99", "state": "DELETE_REQUESTED"}]
        email, projects = init.identity_and_projects(DiscoveryAPI(), [])
        self.assertEqual(email, EMAIL)
        self.assertEqual([item["number"] for item in projects], ["1", "2", "3", "4", "5"])

    def test_account_replacement_stops_credential_loading(self):
        records = [
            json.dumps({"type": "gemini-enterprise", "email": EMAIL, "access_token": "synthetic"}),
            json.dumps({"type": "gemini-enterprise", "email": "different@example.test", "access_token": "synthetic-other"}),
        ]
        with patch.object(Path, "read_text", side_effect=records):
            path = Path("synthetic-account.json")
            credential = init.Credential(path, EMAIL)
            self.assertEqual(credential.token(), "synthetic")
            with self.assertRaisesRegex(init.InitError, "credential_identity_changed"):
                credential.token()

    def test_url_and_dry_run_guards_run_before_credentials(self):
        class NoCredentialRead:
            def token(self):
                raise AssertionError("Guard leaked into credential loading")
        api = init.Google(NoCredentialRead(), False)
        for method, url in [("GET", "http://discoveryengine.googleapis.com/v1/test"), ("GET", "https://example.test/v1/test"), ("POST", init.ORIGINS["eu"] + "/v1/" + REGION + "/licenseConfigs")]:
            with self.subTest(url=url), self.assertRaises(init.InitError):
                api.request(method, url, {})


if __name__ == "__main__":
    unittest.main()

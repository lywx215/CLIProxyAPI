#!/usr/bin/env python3
"""Plan or initialize Gemini Enterprise trial applications using an OAuth file."""

from __future__ import annotations

import argparse
import copy
import datetime as dt
import hashlib
import json
import os
from pathlib import Path
import re
import sys
import tempfile
import time
from typing import Any
import urllib.error
import urllib.parse
import urllib.request


ORIGINS = {
    "global": "https://discoveryengine.googleapis.com",
    "us": "https://us-discoveryengine.googleapis.com",
    "eu": "https://eu-discoveryengine.googleapis.com",
    "ca": "https://ca-discoveryengine.googleapis.com",
    "au": "https://au-discoveryengine.googleapis.com",
    "in": "https://in-discoveryengine.googleapis.com",
}
PART = re.compile(r"^[A-Za-z0-9_-]+$")
TIER = "SUBSCRIPTION_TIER_SEARCH_AND_ASSISTANT"
SEARCH = "WEB_GROUNDING_TYPE_GOOGLE_SEARCH"
TRIAL_ID = "free_trial_gemini"
CARBON = "gemini-4-carbon"


class InitError(Exception):
    """An error code safe to include in reports without provider response bodies."""


class APIError(InitError):
    def __init__(self, status: int, code: str = "request_failed"):
        super().__init__(f"{code}_http_{status}")
        self.status = status


def fingerprint(value: Any) -> str:
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(",", ":")).encode()).hexdigest()


def paid_present(configs: list[dict]) -> bool:
    # Protobuf JSON omits false fields; absent freeTrial therefore means paid.
    for item in configs:
        trial = item.get("freeTrial", False)
        if not isinstance(trial, bool):
            raise InitError("invalid_trial_classification")
        if not trial:
            return True
    return False


def region_name(project: str, location: str) -> str:
    if not project.isascii() or not project.isdigit() or location not in ORIGINS:
        raise InitError("invalid_region_binding")
    return f"projects/{project}/locations/{location}"


class Credential:
    def __init__(self, path: Path, expected_email: str | None):
        self.path = path
        self.expected_email = expected_email.lower() if expected_email else None
        self.pinned_email: str | None = None

    def token(self) -> str:
        try:
            value = json.loads(self.path.read_text(encoding="utf-8-sig"))
        except (OSError, ValueError):
            raise InitError("credential_file_unreadable") from None
        if not isinstance(value, dict) or value.get("type", value.get("provider")) != "gemini-enterprise":
            raise InitError("wrong_credential_provider")
        email, token = value.get("email"), value.get("access_token")
        if not isinstance(email, str) or not isinstance(token, str) or not token:
            raise InitError("credential_identity_or_token_missing")
        expected = self.pinned_email or self.expected_email
        if expected and email.lower() != expected:
            raise InitError("credential_identity_changed")
        return token


class RejectRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None


class Google:
    def __init__(self, credential: Credential, apply: bool):
        self.credential = credential
        self.apply = apply
        self.events: list[dict] = []
        self.opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), RejectRedirect())

    def request(self, method: str, url: str, body: dict | None = None) -> dict:
        parsed = urllib.parse.urlsplit(url)
        if parsed.scheme != "https" or parsed.port not in (None, 443) or parsed.username or parsed.password or parsed.fragment:
            raise InitError("unsafe_google_url")
        origin = "https://" + parsed.netloc
        if origin not in {*ORIGINS.values(), "https://www.googleapis.com", "https://cloudresourcemanager.googleapis.com"}:
            raise InitError("unapproved_google_origin")
        project = None
        if origin in ORIGINS.values():
            pieces = parsed.path.split("/")
            if len(pieces) < 6 or pieces[1] not in ("v1", "v1alpha") or pieces[2] != "projects" or pieces[4] != "locations":
                raise InitError("invalid_google_resource")
            project, location = pieces[3], pieces[5]
            region_name(project, location)
            if ORIGINS[location] != origin:
                raise InitError("region_origin_mismatch")
        elif method != "GET":
            raise InitError("metadata_origin_is_read_only")
        return self._request_json(method, url, parsed, project, body)

    def _request_json(self, method: str, url: str, parsed: urllib.parse.SplitResult, project: str | None, body: dict | None) -> dict:
        if method != "GET" and not self.apply:
            raise InitError("write_requires_apply")
        headers = {"Authorization": "Bearer " + self.credential.token(), "Accept": "application/json"}
        if project:
            headers["X-Goog-User-Project"] = project
        data = None
        if body is not None:
            headers["Content-Type"] = "application/json"
            data = json.dumps(body).encode()
        event = {"method": method, "origin": "https://" + parsed.netloc, "path": parsed.path, "http": None}
        self.events.append(event)
        request = urllib.request.Request(url, data=data, headers=headers, method=method)
        try:
            # No post-connection network deadline; authorization redirects fail closed.
            with self.opener.open(request) as response:
                event["http"] = response.status
                return json.loads(response.read())
        except urllib.error.HTTPError as error:
            event["http"] = error.code
            raise APIError(error.code) from None
        except (OSError, ValueError):
            raise InitError("google_transport_or_response_unavailable") from None

    def pages(self, url: str, field: str) -> list[dict]:
        result, seen = [], set()
        for _ in range(100):
            value = self.request("GET", url)
            items = value.get(field, [])
            if not isinstance(items, list) or any(not isinstance(item, dict) for item in items):
                raise InitError("invalid_google_list")
            result.extend(items)
            token = value.get("nextPageToken")
            if not token:
                return result
            if not isinstance(token, str) or token in seen:
                raise InitError("pagination_cycle")
            seen.add(token)
            parsed = urllib.parse.urlsplit(url)
            query = urllib.parse.parse_qs(parsed.query)
            query["pageToken"] = [token]
            url = urllib.parse.urlunsplit(parsed._replace(query=urllib.parse.urlencode(query, doseq=True)))
        raise InitError("pagination_incomplete")


def identity_and_projects(api: Google, selectors: list[str]) -> tuple[str, list[dict]]:
    identity = api.request("GET", "https://www.googleapis.com/oauth2/v2/userinfo")
    email = identity.get("email")
    if not isinstance(email, str) or not email:
        raise InitError("identity_unverified")
    expected = api.credential.expected_email
    if expected and email.lower() != expected:
        raise InitError("credential_identity_mismatch")
    api.credential.pinned_email = email.lower()
    # Recheck the credential file against Google's verified identity.
    api.credential.token()
    if selectors:
        projects = []
        for selector in dict.fromkeys(selectors):
            if not PART.fullmatch(selector):
                raise InitError("invalid_project_selector")
            projects.append(api.request("GET", "https://cloudresourcemanager.googleapis.com/v3/projects/" + selector))
    else:
        projects = api.pages("https://cloudresourcemanager.googleapis.com/v3/projects:search?pageSize=100", "projects")
    result, seen = [], set()
    for project in projects:
        if project.get("state") != "ACTIVE":
            continue
        name = project.get("name", "")
        number = name.removeprefix("projects/")
        region_name(number, "global")
        if name != "projects/" + number:
            raise InitError("project_identity_unverified")
        if number not in seen:
            result.append({"number": number, "project_id": project.get("projectId"), "display_name": project.get("displayName")})
            seen.add(number)
    if not result:
        raise InitError("no_active_projects")
    return email, sorted(result, key=lambda project: project["number"])


class Initializer:
    def __init__(self, api: Google, email: str, app_prefix: str = "gemini-enterprise", exclude: set[str] | None = None, sleep=time.sleep, carbon_only: bool = False):
        self.api, self.email = api, email.lower()
        self.app_prefix, self.exclude, self.sleep = app_prefix, exclude or set(), sleep
        self.carbon_only = carbon_only

    def url(self, project: str, location: str, resource: str, version: str = "v1", query: str = "") -> str:
        region = region_name(project, location)
        if not resource.startswith(region + "/"):
            raise InitError("resource_binding_mismatch")
        return ORIGINS[location] + "/" + version + "/" + resource + ("?" + query if query else "")

    def configs(self, project: str, location: str) -> list[dict]:
        return self.api.pages(self.url(project, location, region_name(project, location) + "/licenseConfigs", query="pageSize=100"), "licenseConfigs")

    def users(self, project: str, location: str) -> list[dict]:
        values = self.api.pages(self.url(project, location, region_name(project, location) + "/userStores/default_user_store/userLicenses", query="pageSize=100"), "userLicenses")
        return [item for item in values if str(item.get("userPrincipal", "")).lower() == self.email]

    def engines(self, project: str, location: str) -> list[dict]:
        region = region_name(project, location)
        collections = self.api.pages(self.url(project, location, region + "/collections", "v1alpha", "pageSize=100"), "collections")
        result = []
        for collection in collections:
            name = collection.get("name", "")
            if not name.startswith(region + "/collections/"):
                raise InitError("collection_binding_mismatch")
            result.extend(self.api.pages(self.url(project, location, name + "/engines", "v1alpha", "pageSize=100"), "engines"))
        for engine in result:
            if not isinstance(engine.get("name"), str) or not engine["name"].startswith(region + "/collections/") or "/engines/" not in engine["name"]:
                raise InitError("engine_binding_mismatch")
        return result

    def mutate(self, project: str, location: str, method: str, resource: str, body: dict, version: str = "v1", query: str = "") -> dict:
        if not self.api.apply:
            raise InitError("write_requires_apply")
        if paid_present(self.configs(project, location)):
            raise InitError("paid_region_write_stopped")
        region = region_name(project, location)
        params = urllib.parse.parse_qs(query)
        if method == "POST" and resource == region + "/licenseConfigs":
            if body.get("freeTrial") is not True or body.get("autoRenew") is not False or params != {"licenseConfigId": [TRIAL_ID]}:
                raise InitError("trial_creation_only")
        elif method == "POST" and resource == region + "/userStores/default_user_store:batchUpdateUserLicenses":
            entries = body.get("inlineSource", {}).get("userLicenses", [])
            if len(entries) != 1 or entries[0].get("userPrincipal", "").lower() != self.email or body.get("deleteUnassignedUserLicenses") is not False:
                raise InitError("assignment_scope_mismatch")
        elif method == "POST" and resource == region + "/collections/default_collection/engines":
            if body.get("appType") != "APP_TYPE_INTRANET" or set(params) != {"engineId"}:
                raise InitError("application_scope_mismatch")
        elif method == "PATCH" and "/engines/" in resource and body.get("name") == resource:
            if "/assistants/" in resource:
                if set(body) - {"name", "generationConfig", "webGroundingType", "defaultWebGroundingToggleOff"}:
                    raise InitError("assistant_fields_outside_scope")
            elif set(body) != {"name", "modelConfigs"} or params != {"updateMask": ["modelConfigs"]}:
                raise InitError("engine_fields_outside_scope")
        else:
            raise InitError("operation_outside_initialization_scope")
        return self.api.request(method, self.url(project, location, resource, version, query), body)

    def wait(self, project: str, location: str, operation: dict) -> None:
        while not operation.get("done"):
            name = operation.get("name", "")
            if not name.startswith(region_name(project, location) + "/") or "/operations/" not in name:
                raise InitError("invalid_operation_name")
            self.sleep(2)
            operation = self.api.request("GET", self.url(project, location, name))
        response = operation.get("response", {})
        if operation.get("error") or response.get("errorSamples") or response.get("failedUserPrincipals") or int(response.get("failureCount", 0)):
            raise InitError("operation_failed")

    def inspect(self, project: str, location: str) -> dict:
        configs = self.configs(project, location)
        engines = self.engines(project, location)
        users = self.users(project, location)
        return {"project": project, "location": location, "paid": paid_present(configs), "configs": configs, "engines": engines, "users": users}

    def trial(self, project: str, location: str, before: dict, actions: list[str]) -> tuple[str, dict]:
        region = region_name(project, location)
        trials = [item for item in before["configs"] if item.get("freeTrial") is True]
        active = [item for item in trials if item.get("state") == "ACTIVE"]
        if active:
            assigned_names = {item.get("licenseConfig") for item in before["users"] if item.get("licenseAssignmentState") == "ASSIGNED"}
            candidates = [item for item in active if item.get("name") in assigned_names]
            if not candidates:
                candidates = [item for item in active if item.get("name") == region + "/licenseConfigs/" + TRIAL_ID]
            if not candidates and len(active) == 1:
                candidates = active
            if len(candidates) != 1:
                raise InitError("ambiguous_trial_configs")
            name = candidates[0]["name"]
            actions.append("trial_reused")
        elif trials:
            raise InitError("existing_trial_not_active")
        else:
            today = dt.datetime.now(dt.timezone.utc).date()
            payload = {"licenseCount": "50", "subscriptionTier": TIER, "subscriptionTerm": "SUBSCRIPTION_TERM_ONE_MONTH", "freeTrial": True, "autoRenew": False, "startDate": {"year": today.year, "month": today.month, "day": today.day}}
            name = region + "/licenseConfigs/" + TRIAL_ID
            self.mutate(project, location, "POST", region + "/licenseConfigs", payload, query="licenseConfigId=" + TRIAL_ID)
            actions.append("trial_created")
        value = self.api.request("GET", self.url(project, location, name))
        if value.get("name") != name or value.get("freeTrial") is not True or value.get("state") != "ACTIVE":
            raise InitError("trial_readback_failed")
        summary = {key: value[key] for key in ("name", "freeTrial", "state", "licenseCount", "subscriptionTier", "startDate", "endDate") if key in value}
        return name, summary

    def application(self, project: str, location: str, before: dict, actions: list[str]) -> list[str]:
        names = [engine["name"] for engine in before["engines"] if engine["name"] not in self.exclude]
        if names:
            actions.append("applications_reused")
            return names
        if before["engines"]:
            raise InitError("all_existing_applications_excluded")
        region = region_name(project, location)
        app_id = self.app_prefix + "-" + location + "-init"
        name = region + "/collections/default_collection/engines/" + app_id
        payload = {"displayName": "Gemini Enterprise " + location.upper(), "solutionType": "SOLUTION_TYPE_SEARCH", "industryVertical": "GENERIC", "appType": "APP_TYPE_INTRANET", "searchEngineConfig": {"searchTier": "SEARCH_TIER_ENTERPRISE", "searchAddOns": ["SEARCH_ADD_ON_LLM"], "requiredSubscriptionTier": TIER}}
        operation = self.mutate(project, location, "POST", region + "/collections/default_collection/engines", payload, "v1alpha", "engineId=" + app_id)
        self.wait(project, location, operation)
        value = self.api.request("GET", self.url(project, location, name, "v1alpha"))
        if value.get("name") != name:
            raise InitError("application_readback_failed")
        actions.append("application_created")
        return [name]

    def assign(self, project: str, location: str, trial: str, actions: list[str]) -> dict:
        existing = self.users(project, location)
        assigned = [item for item in existing if item.get("licenseAssignmentState") == "ASSIGNED"]
        if assigned:
            if any(item.get("licenseConfig") != trial for item in assigned):
                raise InitError("existing_assignment_conflict")
            actions.append("assignment_reused")
        else:
            payload = {"inlineSource": {"userLicenses": [{"userPrincipal": self.email, "licenseConfig": trial}], "updateMask": "userPrincipal,licenseConfig"}, "deleteUnassignedUserLicenses": False}
            operation = self.mutate(project, location, "POST", region_name(project, location) + "/userStores/default_user_store:batchUpdateUserLicenses", payload)
            self.wait(project, location, operation)
            actions.append("account_assigned")
        verified = [item for item in self.users(project, location) if item.get("licenseAssignmentState") == "ASSIGNED" and item.get("licenseConfig") == trial]
        if not verified:
            raise InitError("assignment_readback_failed")
        return {key: verified[0][key] for key in ("userPrincipal", "licenseAssignmentState", "licenseConfig")}

    @staticmethod
    def retained(value: dict, assistant: bool, models: list[str], explicit: bool) -> dict:
        result = copy.deepcopy(value)
        result.pop("updateTime", None)
        if assistant:
            for key in ("webGroundingType", "defaultWebGroundingToggleOff", "googleSearchGroundingEnabled"):
                result.pop(key, None)
            if explicit:
                result.get("generationConfig", {}).pop("allowedModelIds", None)
                if result.get("generationConfig") == {}:
                    result.pop("generationConfig")
        else:
            result.pop("effectiveFeatures", None)
            for model in models:
                result.get("modelConfigs", {}).pop(model, None)
            if result.get("modelConfigs") == {}:
                result.pop("modelConfigs")
        return result

    @staticmethod
    def carbon_retained(value: dict, assistant: bool) -> dict:
        result = copy.deepcopy(value)
        result.pop("updateTime", None)
        if assistant:
            result.get("generationConfig", {}).pop("allowedModelIds", None)
            if result.get("generationConfig") == {}:
                result.pop("generationConfig")
        else:
            result.get("modelConfigs", {}).pop(CARBON, None)
            if result.get("modelConfigs") == {}:
                result.pop("modelConfigs")
        return result

    def configure_carbon_remote(self, project: str, location: str, name: str) -> dict:
        # Match the project's ConfigureCarbonGoogle semantics without local admission changes.
        if paid_present(self.configs(project, location)):
            raise InitError("paid_region_write_stopped")
        engine = self.api.request("GET", self.url(project, location, name, "v1alpha"))
        assistant_name = name + "/assistants/default_assistant"
        assistant = self.api.request("GET", self.url(project, location, assistant_name, "v1alpha"))
        if engine.get("name") != name or assistant.get("name") != assistant_name:
            raise InitError("carbon_resource_identity_mismatch")
        allowed = assistant.get("generationConfig", {}).get("allowedModelIds")
        if allowed is not None and (not isinstance(allowed, list) or any(not isinstance(item, str) for item in allowed)):
            raise InitError("invalid_assistant_model_range")
        explicit = bool(allowed)
        models = dict(engine.get("modelConfigs", {}))
        models[CARBON] = "MODEL_ENABLED"
        targets = [(False, engine, {"name": name, "modelConfigs": models}, "modelConfigs", engine.get("modelConfigs", {}).get(CARBON) != "MODEL_ENABLED")]
        if explicit:
            targets.append((True, assistant, {"name": assistant_name, "generationConfig": {"allowedModelIds": list(dict.fromkeys(allowed + [CARBON]))}}, "generationConfig.allowedModelIds", CARBON not in allowed))
        patches = 0
        for is_assistant, old, payload, mask, changed in targets:
            url = self.url(project, location, old["name"], "v1alpha")
            latest = self.api.request("GET", url)
            if fingerprint(latest) != fingerprint(old):
                raise InitError("carbon_resource_changed_before_write")
            if changed:
                self.mutate(project, location, "PATCH", old["name"], payload, "v1alpha", "updateMask=" + urllib.parse.quote(mask, safe=""))
                patches += 1
            current = self.api.request("GET", url)
            if self.carbon_retained(old, is_assistant) != self.carbon_retained(current, is_assistant):
                raise InitError("carbon_unrelated_fields_changed")
            if is_assistant:
                if set(current.get("generationConfig", {}).get("allowedModelIds", [])) != set(allowed) | {CARBON}:
                    raise InitError("carbon_assistant_readback_failed")
            elif current.get("modelConfigs", {}).get(CARBON) != "MODEL_ENABLED":
                raise InitError("carbon_engine_readback_failed")
        confirmed = self.api.request("GET", self.url(project, location, assistant_name, "v1alpha"))
        if not explicit and confirmed.get("generationConfig", {}).get("allowedModelIds") != allowed:
            raise InitError("carbon_implicit_range_changed")
        if self.carbon_retained(assistant, True) != self.carbon_retained(confirmed, True):
            raise InitError("carbon_unrelated_fields_changed")
        return {"status": "verification_required" if not explicit else "configured" if patches else "unchanged", "reason": "assistant_defaults_preserved" if not explicit else None, "engine_enabled": True, "assistant_model_range": "explicit" if explicit else "implicit", "patches": patches, "configuration_verified": True, "generation_verified": False}

    def configure(self, project: str, location: str, name: str) -> dict:
        engine = self.api.request("GET", self.url(project, location, name, "v1alpha"))
        assistant_name = name + "/assistants/default_assistant"
        assistant = self.api.request("GET", self.url(project, location, assistant_name, "v1alpha"))
        widget_name = name + "/widgetConfigs/default_search_widget_config"
        admin = self.api.request("GET", self.url(project, location, widget_name, "v1alpha", "modelInfoView=ADMIN&languageCode=en"))
        values = admin.get("uiSettings", {}).get("modelConfigInfo", {}).get("resolvedModels")
        if not isinstance(values, list):
            raise InitError("admin_model_catalog_unknown")
        models = sorted({item["modelId"] for item in values if isinstance(item, dict) and item.get("modelId")} | {CARBON})
        if any(not isinstance(model, str) or len(model) > 256 or not re.fullmatch(r"[A-Za-z0-9_.-]+", model) for model in models):
            raise InitError("invalid_model_id")
        allowed = assistant.get("generationConfig", {}).get("allowedModelIds")
        if allowed is not None and (not isinstance(allowed, list) or any(not isinstance(item, str) for item in allowed)):
            raise InitError("invalid_assistant_model_range")
        explicit = bool(allowed)
        engine_payload = {"name": name, "modelConfigs": dict(engine.get("modelConfigs", {}))}
        engine_payload["modelConfigs"].update({model: "MODEL_ENABLED" for model in models})
        assistant_payload = {"name": assistant_name, "webGroundingType": SEARCH, "defaultWebGroundingToggleOff": False}
        assistant_mask = "webGroundingType,defaultWebGroundingToggleOff"
        if explicit:
            assistant_payload["generationConfig"] = {"allowedModelIds": list(dict.fromkeys(allowed + models))}
            assistant_mask += ",generationConfig.allowedModelIds"
        writes = 0
        for is_assistant, old, payload, mask in ((False, engine, engine_payload, "modelConfigs"), (True, assistant, assistant_payload, assistant_mask)):
            url = self.url(project, location, old["name"], "v1alpha")
            latest = self.api.request("GET", url)
            if fingerprint(latest) != fingerprint(old):
                raise InitError("resource_changed_before_write")
            if any(old.get(key, False if isinstance(value, bool) else None) != value for key, value in payload.items() if key != "name"):
                self.mutate(project, location, "PATCH", old["name"], payload, "v1alpha", "updateMask=" + urllib.parse.quote(mask, safe=""))
                writes += 1
            current = self.api.request("GET", url)
            if self.retained(old, is_assistant, models, explicit) != self.retained(current, is_assistant, models, explicit):
                raise InitError("unrelated_resource_fields_changed")
            if is_assistant:
                if current.get("webGroundingType") != SEARCH or current.get("defaultWebGroundingToggleOff", False):
                    raise InitError("search_readback_failed")
                current_allowed = current.get("generationConfig", {}).get("allowedModelIds")
                if (explicit and not set(allowed + models) <= set(current_allowed or [])) or (not explicit and current_allowed != allowed):
                    raise InitError("assistant_model_range_changed")
            elif any(current.get("modelConfigs", {}).get(model) != "MODEL_ENABLED" for model in models):
                raise InitError("model_settings_readback_failed")
        widget = self.api.request("GET", self.url(project, location, widget_name, "v1alpha", "modelInfoView=END_USER_WEB&languageCode=en"))
        ui, settings = widget.get("uiSettings", {}), widget.get("assistantSettings", {})
        if settings.get("webGroundingType") != SEARCH or settings.get("defaultWebGroundingToggleOff", False):
            raise InitError("widget_settings_readback_failed")
        visible = [item["modelId"] for item in ui.get("modelConfigInfo", {}).get("resolvedModels", []) if item.get("modelId")]
        carbon = self.configure_carbon_remote(project, location, name)
        return {"name": name, "configured_models": models, "user_model_ids": visible, "patches": writes, "model_selector": ui.get("features", {}).get("model-selector") == "FEATURE_STATE_ON", "google_search": True, "default_search": True, "implicit_model_range_preserved": not explicit, "carbon_remote": carbon}

    def run_region(self, project: str, location: str) -> dict:
        result = {"project": project, "location": location, "actions": []}
        start = len(self.api.events)
        try:
            before = self.inspect(project, location)
            result["application_count_before"] = len(before["engines"])
            result["excluded_engines"] = [item["name"] for item in before["engines"] if item["name"] in self.exclude]
            if before["paid"]:
                result["status"] = "paid_skipped"
            elif self.carbon_only and not before["engines"]:
                result["status"] = "no_applications"
            elif not self.api.apply:
                result["status"] = "planned"
                result["actions"] = ["configure_remote_carbon_existing_trial_applications"] if self.carbon_only else ["verify_or_create_trial", "reuse_and_configure_applications" if before["engines"] else "create_and_configure_application", "verify_or_assign_current_account", "verify_models_carbon_and_search", "configure_remote_carbon"]
            elif self.carbon_only:
                active = {item["name"] for item in before["configs"] if item.get("freeTrial") is True and item.get("state") == "ACTIVE"}
                if not any(item.get("licenseAssignmentState") == "ASSIGNED" and item.get("licenseConfig") in active for item in before["users"]):
                    raise InitError("carbon_trial_assignment_unverified")
                names = [item["name"] for item in before["engines"] if item["name"] not in self.exclude]
                if not names:
                    raise InitError("all_existing_applications_excluded")
                result["applications"] = [{"name": name, "carbon_remote": self.configure_carbon_remote(project, location, name)} for name in names]
                result["actions"] = ["remote_carbon_reconciled"]
                result["status"] = "complete"
            else:
                trial, result["trial"] = self.trial(project, location, before, result["actions"])
                names = self.application(project, location, before, result["actions"])
                result["account_assignment"] = self.assign(project, location, trial, result["actions"])
                result["license_config"] = trial
                result["applications"] = [self.configure(project, location, name) for name in names]
                result["status"] = "complete"
        except APIError as error:
            result.update(status="failed", error=str(error), http=error.status)
        except InitError as error:
            result.update(status="failed", error=str(error))
        result["requests"] = self.api.events[start:]
        return result


def parser() -> argparse.ArgumentParser:
    command = argparse.ArgumentParser(description=__doc__)
    command.add_argument("--auth-file", type=Path, required=True, help="Existing Gemini Enterprise OAuth account JSON; never a token value")
    command.add_argument("--email", help="Expected Google account email")
    command.add_argument("--project", action="append", default=[], help="Project ID or number; repeat to scope discovery")
    command.add_argument("--all-projects", action="store_true", help="Explicitly include every visible active project when applying")
    command.add_argument("--location", action="append", choices=ORIGINS, help="Defaults to all six supported locations")
    command.add_argument("--app-prefix", default="gemini-enterprise", help="Prefix for new application IDs")
    command.add_argument("--exclude-engine", action="append", default=[], help="Full resource name to leave untouched; repeat as needed")
    command.add_argument("--apply", action="store_true", help="Enable writes; default is a read-only plan")
    command.add_argument("--carbon-only", action="store_true", help="Only reconcile remote Carbon on existing assigned trial applications; paid regions remain skipped")
    command.add_argument("--enable-proxy-carbon", action="store_true", help="Also enable CLIProxyAPI Carbon admission for imported trial applications after Google configuration")
    command.add_argument("--management-url", help="Exact CLIProxyAPI origin: HTTPS or HTTP loopback over verified SSH")
    command.add_argument("--management-key-env", default="CLIPROXY_MANAGEMENT_KEY", help="Environment variable containing the management key; never pass the key itself")
    command.add_argument("--report", type=Path, help="Write a sanitized JSON report, outside credential storage")
    return command


def main(argv: list[str] | None = None) -> int:
    args = parser().parse_args(argv)
    if not re.fullmatch(r"[a-z][a-z0-9-]{0,39}", args.app_prefix):
        raise InitError("invalid_app_prefix")
    if args.project and args.all_projects:
        raise InitError("choose_projects_or_all_projects")
    if args.apply and not args.project and not args.all_projects:
        raise InitError("apply_requires_project_scope")
    if args.report and args.report.resolve() == args.auth_file.resolve():
        raise InitError("report_must_not_overwrite_credential")
    management = None
    if args.enable_proxy_carbon:
        if not args.management_url or not re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", args.management_key_env):
            raise InitError("proxy_carbon_requires_management_connection")
        from proxy_carbon import Management
        management = Management(args.management_url, os.environ.get(args.management_key_env, ""), args.apply)
    elif args.management_url:
        raise InitError("management_connection_requires_enable_proxy_carbon")
    credential = Credential(args.auth_file, args.email)
    api = Google(credential, args.apply)
    email, projects = identity_and_projects(api, args.project)
    initializer = Initializer(api, email, args.app_prefix, set(args.exclude_engine), carbon_only=args.carbon_only)
    report = {"mode": "apply" if args.apply else "plan", "operation": "remote_carbon" if args.carbon_only else "initialization", "email": email, "projects": projects, "regions": [], "generation_tests": "not_performed"}
    for project in projects:
        for location in dict.fromkeys(args.location or ORIGINS):
            result = initializer.run_region(project["number"], location)
            report["regions"].append(result)
            print(json.dumps({"project": project["number"], "location": location, "status": result["status"]}), file=sys.stderr, flush=True)
            if result.get("http") == 401:
                break
        if report["regions"][-1].get("http") == 401:
            break
    report["complete"] = len(report["regions"]) == len(projects) * len(set(args.location or ORIGINS)) and all(item["status"] in ("complete", "planned", "paid_skipped", "no_applications") for item in report["regions"])
    report["proxy_carbon"] = {"stage": "proxy_carbon_admission", "status": "not_requested", "configuration_verified": False}
    if management is not None:
        if report["complete"]:
            from proxy_carbon import reconcile
            report["proxy_carbon"] = reconcile(management, initializer, [project["number"] for project in projects], list(dict.fromkeys(args.location or ORIGINS)))
            report["complete"] = report["complete"] and report["proxy_carbon"]["complete"]
        else:
            report["proxy_carbon"]["status"] = "blocked_by_google_stage_failure"
    output = json.dumps(report, ensure_ascii=False, indent=2) + "\n"
    if args.report:
        args.report.parent.mkdir(parents=True, exist_ok=True)
        temporary = None
        try:
            with tempfile.NamedTemporaryFile(mode="w", encoding="utf-8", dir=args.report.parent, prefix=args.report.name + ".", suffix=".tmp", delete=False) as stream:
                temporary = Path(stream.name)
                stream.write(output)
            os.replace(temporary, args.report)
        finally:
            if temporary is not None and temporary.exists():
                temporary.unlink()
    print(output, end="")
    return 0 if report["complete"] else 1


if __name__ == "__main__":
    sys.modules.setdefault("initialize", sys.modules[__name__])
    try:
        raise SystemExit(main())
    except (InitError, OSError, ValueError) as error:
        print(json.dumps({"status": "failed", "error": str(error) if isinstance(error, InitError) else type(error).__name__}), file=sys.stderr)
        raise SystemExit(1)

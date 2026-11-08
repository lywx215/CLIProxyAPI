"""Enable CLIProxyAPI Carbon admission only for verified imported trial apps."""

from __future__ import annotations

import copy
import json
import urllib.error
import urllib.parse
import urllib.request

from initialize import CARBON, InitError, RejectRedirect, fingerprint, paid_present, region_name


def management_origin(value: str) -> str:
    parsed = urllib.parse.urlsplit(value)
    if parsed.username or parsed.password or parsed.query or parsed.fragment or parsed.path not in ("", "/"):
        raise InitError("unsafe_management_origin")
    if parsed.scheme != "https" and not (parsed.scheme == "http" and parsed.hostname in ("127.0.0.1", "::1")):
        raise InitError("management_requires_https_or_loopback")
    if not parsed.hostname:
        raise InitError("unsafe_management_origin")
    return value.rstrip("/")


class Management:
    def __init__(self, origin: str, key: str, apply: bool):
        self.origin = management_origin(origin)
        if not isinstance(key, str) or not key or any(ch in key for ch in "\r\n"):
            raise InitError("management_key_missing_or_invalid")
        self.key, self.apply, self.events = key, apply, []
        self.opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), RejectRedirect())

    def request(self, method: str, path: str, body: dict | None = None) -> dict:
        if method == "GET":
            if not path.startswith("/v8/management/credentials?") or body is not None:
                raise InitError("management_read_outside_scope")
        elif method == "PATCH":
            if not self.apply or path != "/v8/management/credentials/gemini-enterprise/collections" or not isinstance(body, dict) or set(body) != {"name", "auth_index", "binding_id", "expected_revision", "fields"} or body["fields"] != {"carbon_enabled": True}:
                raise InitError("management_write_outside_scope")
        else:
            raise InitError("management_operation_outside_scope")
        event = {"method": method, "path": path.split("?")[0], "http": None}
        self.events.append(event)
        headers = {"Authorization": "Bearer " + self.key, "Accept": "application/json"}
        data = None
        if body is not None:
            headers["Content-Type"] = "application/json"
            data = json.dumps(body).encode()
        request = urllib.request.Request(self.origin + path, data=data, headers=headers, method=method)
        try:
            with self.opener.open(request) as response:
                event["http"] = response.status
                value = json.loads(response.read())
                if not isinstance(value, dict):
                    raise InitError("invalid_management_response")
                return value
        except urllib.error.HTTPError as error:
            event["http"] = error.code
            raise InitError(f"management_request_failed_http_{error.code}") from None
        except (OSError, ValueError):
            raise InitError("management_transport_or_response_unavailable") from None

    def account(self, email: str) -> dict:
        matches = []
        for page in range(1, 101):
            value = self.request("GET", f"/v8/management/credentials?page={page}&page_size=100")
            files = value.get("files")
            if not isinstance(files, list) or any(not isinstance(item, dict) for item in files):
                raise InitError("invalid_management_credential_list")
            matches.extend(item for item in files if item.get("provider", item.get("type")) == "gemini-enterprise" and str(item.get("email", "")).lower() == email.lower())
            if value.get("has_more") is False:
                break
            if not files or value.get("has_more") is not True:
                raise InitError("management_pagination_unverified")
        else:
            raise InitError("management_pagination_incomplete")
        if len(matches) != 1:
            raise InitError("management_account_not_unique")
        root = matches[0]
        account = root.get("gemini_enterprise", {})
        if root.get("disabled") is not False or not account.get("account_id") or account.get("carbon_admission_supported") is not True or not isinstance(account.get("collections"), list):
            raise InitError("management_account_unavailable")
        return root


def engine_name(app: dict) -> str:
    values = [app.get(key) for key in ("project_id", "location", "collection_id", "engine_id")]
    if any(not isinstance(value, str) or not value or "/" in value for value in values):
        raise InitError("invalid_imported_application_resources")
    return region_name(values[0], values[1]) + "/collections/" + values[2] + "/engines/" + values[3]


def binding_stamp(root: dict, app: dict) -> str:
    return fingerprint({"name": root.get("name"), "auth_index": root.get("auth_index"), "account_id": root["gemini_enterprise"].get("account_id"), "binding": {key: app.get(key) for key in ("binding_id", "auth_index", "revision", "project_id", "location", "collection_id", "engine_id", "assistant_id", "settings", "disabled", "effective_disabled", "missing")}})


def find_current(api: Management, email: str, root: dict, app: dict) -> tuple[dict, dict]:
    current_root = api.account(email)
    matches = [item for item in current_root["gemini_enterprise"]["collections"] if item.get("binding_id") == app.get("binding_id")]
    if len(matches) != 1 or binding_stamp(current_root, matches[0]) != binding_stamp(root, app):
        raise InitError("proxy_application_changed_before_write")
    return current_root, matches[0]


def validate_admission(app: dict) -> None:
    subscription = app.get("model_policy_state", {}).get("subscription", {})
    admission = app.get("carbon_admission", {})
    if subscription.get("status") != "ok" or subscription.get("free_trial") is not True or subscription.get("state") != "ACTIVE":
        raise InitError("proxy_trial_subscription_unverified")
    if app.get("effective_disabled") is not False or app.get("missing") is not False or admission.get("can_enable") is not True or admission.get("verification_required") is not False:
        raise InitError("proxy_carbon_admission_unavailable")
    if not isinstance(admission.get("enabled"), bool):
        raise InitError("proxy_carbon_state_unverified")
    revision = app.get("revision")
    if not app.get("binding_id") or not app.get("auth_index") or not isinstance(revision, int) or isinstance(revision, bool) or revision < 1:
        raise InitError("invalid_proxy_application_revision")


def retained_settings(app: dict) -> dict:
    settings = copy.deepcopy(app.get("settings", {}))
    if not isinstance(settings, dict):
        raise InitError("invalid_proxy_application_settings")
    settings.pop("carbon_enabled", None)
    return settings


def reconcile(api: Management, initializer, projects: list[str], locations: list[str]) -> dict:
    email = initializer.email
    initial_root = api.account(email)
    frozen = copy.deepcopy(initial_root)
    results = []
    for project in projects:
        for location in locations:
            result = {"project": project, "location": location, "applications": []}
            try:
                configs = initializer.configs(project, location)
                if paid_present(configs):
                    result["status"] = "paid_skipped"
                    results.append(result)
                    continue
                active = {item["name"] for item in configs if item.get("freeTrial") is True and item.get("state") == "ACTIVE"}
                if not any(item.get("licenseAssignmentState") == "ASSIGNED" and item.get("licenseConfig") in active for item in initializer.users(project, location)):
                    raise InitError("proxy_trial_assignment_unverified")
                apps = [app for app in frozen["gemini_enterprise"]["collections"] if app.get("project_id") == project and app.get("location") == location and engine_name(app) not in initializer.exclude]
                if not apps:
                    raise InitError("proxy_application_not_imported")
                for old in apps:
                    current_root, app = find_current(api, email, frozen, old)
                    validate_admission(app)
                    name = engine_name(app)
                    engine = initializer.api.request("GET", initializer.url(project, location, name, "v1alpha"))
                    assistant_id = app.get("assistant_id")
                    if not isinstance(assistant_id, str) or not assistant_id or "/" in assistant_id:
                        raise InitError("invalid_proxy_assistant_binding")
                    assistant_name = name + "/assistants/" + assistant_id
                    assistant = initializer.api.request("GET", initializer.url(project, location, assistant_name, "v1alpha"))
                    allowed = assistant.get("generationConfig", {}).get("allowedModelIds")
                    if engine.get("name") != name or engine.get("modelConfigs", {}).get(CARBON) != "MODEL_ENABLED" or assistant.get("name") != assistant_name or allowed is not None and (not isinstance(allowed, list) or allowed and CARBON not in allowed):
                        raise InitError("google_carbon_configuration_unverified")
                    summary = {"name": name, "binding_id": app["binding_id"], "google_configuration_verified": True, "enabled_before": app["carbon_admission"]["enabled"], "generation_verified": False}
                    if not api.apply:
                        summary.update(status="planned", enabled_after=None)
                    else:
                        current_root, app = find_current(api, email, frozen, old)
                        validate_admission(app)
                        if paid_present(initializer.configs(project, location)):
                            raise InitError("paid_region_write_stopped")
                        if app["carbon_admission"]["enabled"]:
                            summary["status"] = "unchanged"
                        else:
                            payload = {"name": current_root["name"], "auth_index": app["auth_index"], "binding_id": app["binding_id"], "expected_revision": app["revision"], "fields": {"carbon_enabled": True}}
                            response = api.request("PATCH", "/v8/management/credentials/gemini-enterprise/collections", payload)
                            if response.get("status") != "ok" or response.get("carbon_admission", {}).get("enabled") is not True:
                                raise InitError("proxy_carbon_save_unconfirmed")
                            summary["status"] = "enabled"
                        confirmed_root = api.account(email)
                        if confirmed_root.get("name") != current_root.get("name") or confirmed_root["gemini_enterprise"].get("account_id") != current_root["gemini_enterprise"].get("account_id"):
                            raise InitError("proxy_account_changed_after_write")
                        confirmed = [item for item in confirmed_root["gemini_enterprise"]["collections"] if item.get("binding_id") == app["binding_id"]]
                        if len(confirmed) != 1 or engine_name(confirmed[0]) != name or retained_settings(confirmed[0]) != retained_settings(old):
                            raise InitError("proxy_unrelated_settings_changed")
                        admission = confirmed[0].get("carbon_admission", {})
                        if admission.get("enabled") is not True or confirmed[0].get("settings", {}).get("carbon_enabled") is not True:
                            raise InitError("proxy_carbon_readback_failed")
                        summary.update(enabled_after=True, business_eligible=admission.get("available"), configuration_verified=True)
                    result["applications"].append(summary)
                result["status"] = "complete" if api.apply else "planned"
            except InitError as error:
                result.update(status="failed", error=str(error))
            results.append(result)
    final_root = api.account(email)
    final_apps = final_root["gemini_enterprise"]["collections"]
    paid_scopes = {(item["project"], item["location"]) for item in results if item["status"] == "paid_skipped"}
    for old in frozen["gemini_enterprise"]["collections"]:
        if (old.get("project_id"), old.get("location")) in paid_scopes:
            current = [item for item in final_apps if item.get("binding_id") == old.get("binding_id")]
            if len(current) != 1 or binding_stamp(final_root, current[0]) != binding_stamp(frozen, old):
                raise InitError("paid_proxy_application_changed")
    return {"stage": "proxy_carbon_admission", "mode": "apply" if api.apply else "plan", "regions": results, "complete": all(item["status"] in ("complete", "planned", "paid_skipped") for item in results), "management_requests": api.events, "generation_tests": "not_performed"}

"""Explicitly verify native Carbon once per selected imported trial application."""

from __future__ import annotations

import argparse
import copy
import datetime as dt
import json
import os
from pathlib import Path
import re
import sys
import urllib.error
import urllib.request

from initialize import CARBON, ORIGINS, Credential, Google, InitError, Initializer, fingerprint, identity_and_projects, paid_present
from proxy_carbon import Management, binding_stamp, engine_name, find_current, validate_admission


RESOURCE_KEYS = ("project_id", "location", "collection_id", "engine_id", "assistant_id")


def safe_code(value, default="unclassified_result") -> str:
    return value if isinstance(value, str) and re.fullmatch(r"[A-Za-z0-9_-]{1,96}", value) else default


class TestManagement(Management):
    def test(self, payload: dict) -> tuple[int, dict]:
        required = {"name", "auth_index", "binding_id", "expected_revision", "expected", "model"}
        if not self.apply or set(payload) != required or payload["model"] != CARBON or set(payload["expected"]) != set(RESOURCE_KEYS):
            raise InitError("carbon_test_outside_authorized_scope")
        path = "/v8/management/credentials/models/test"
        event = {"method": "POST", "path": path, "http": None, "binding_id": payload["binding_id"]}
        self.events.append(event)
        request = urllib.request.Request(self.origin + path, data=json.dumps(payload).encode(), headers={"Authorization": "Bearer " + self.key, "Content-Type": "application/json", "Accept": "application/json"}, method="POST")
        try:
            with self.opener.open(request) as response:
                event["http"] = response.status
                value = json.loads(response.read())
                if not isinstance(value, dict):
                    raise InitError("carbon_test_response_unavailable_no_retry")
                return response.status, value
        except urllib.error.HTTPError as error:
            event["http"] = error.code
            try:
                value = json.loads(error.read())
            except (OSError, ValueError):
                value = {}
            return error.code, value if isinstance(value, dict) else {}
        except (OSError, ValueError):
            raise InitError("carbon_test_transport_unknown_no_retry") from None


class Journal:
    def __init__(self, path: Path, scope: dict):
        self.path, self.entries = path, {}
        scope_hash = fingerprint(scope)
        if path.exists():
            rows = [json.loads(line) for line in path.read_text(encoding="utf-8").splitlines() if line]
            if not rows or rows[0] != {"event": "scope", "scope_hash": scope_hash}:
                raise InitError("carbon_test_journal_scope_mismatch")
            for row in rows[1:]:
                self.entries[row["binding_id"]] = row
        else:
            path.parent.mkdir(parents=True, exist_ok=True)
            descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
            with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
                stream.write(json.dumps({"event": "scope", "scope_hash": scope_hash}) + "\n")
                stream.flush()
                os.fsync(stream.fileno())

    def append(self, event: str, binding_id: str, result: dict) -> None:
        row = {"event": event, "binding_id": binding_id, "result": result}
        with self.path.open("a", encoding="utf-8") as stream:
            stream.write(json.dumps(row, ensure_ascii=False) + "\n")
            stream.flush()
            os.fsync(stream.fileno())
        self.entries[binding_id] = row


def outcome(http: int, value: dict, app: dict) -> dict:
    expected = {key: app[key] for key in RESOURCE_KEYS}
    admission = value.get("carbon_admission", {})
    proof = admission.get("test") or {}
    same = value.get("binding_id") == app["binding_id"] and value.get("auth_index") == app["auth_index"] and value.get("resources") == expected and value.get("expected_revision") == app["revision"] and value.get("revision") == app["revision"]
    verified = http == 200 and value.get("status") == "ok" and value.get("code") == "ok" and value.get("http_status") == 200 and value.get("model") == CARBON and value.get("upstream_model") == CARBON and same and proof.get("status") == "verified" and proof.get("upstream_model") == CARBON and proof.get("test_id") and proof.get("test_id") == value.get("carbon_test_id")
    actual = value.get("upstream_model")
    actual = actual if isinstance(actual, str) and re.fullmatch(r"[A-Za-z0-9_.-]{1,128}", actual) else None
    return {"status": "verified" if verified else "failed", "management_http": http, "generation_http": value.get("http_status"), "code": safe_code(value.get("code")), "upstream_model": actual, "binding_matched": same, "elapsed_ms": value.get("elapsed_ms"), "checked_at": value.get("checked_at"), "test_id": proof.get("test_id") if isinstance(proof.get("test_id"), str) else None, "proof_status": safe_code(proof.get("status"), "missing"), "generation_verified": bool(verified), "enabled_after": admission.get("enabled"), "available_after": admission.get("available")}


def verify(api: TestManagement, initializer: Initializer, projects: list[str], locations: list[str], journal: Journal, max_calls: int, progress=None) -> dict:
    frozen = copy.deepcopy(api.account(initializer.email))
    results, calls = [], 0
    for old in frozen["gemini_enterprise"]["collections"]:
        project, location = old.get("project_id"), old.get("location")
        if project not in projects or location not in locations:
            continue
        name = engine_name(old)
        if name in initializer.exclude:
            continue
        result = {"project": project, "location": location, "name": name, "binding_id": old["binding_id"], "generation_verified": False, "submitted": False}
        try:
            configs = initializer.configs(project, location)
            if paid_present(configs):
                result["status"] = "paid_skipped"
            elif old["binding_id"] in journal.entries:
                prior = journal.entries[old["binding_id"]]
                result.update(prior["result"], replayed=False, journal_reused=True)
                if prior["event"] == "started":
                    result.update(status="unknown_no_retry", generation_verified=False)
            else:
                if calls >= max_calls:
                    raise InitError("authorized_carbon_call_limit_reached")
                active = {item["name"] for item in configs if item.get("freeTrial") is True and item.get("state") == "ACTIVE"}
                if not any(item.get("licenseAssignmentState") == "ASSIGNED" and item.get("licenseConfig") in active for item in initializer.users(project, location)):
                    raise InitError("carbon_test_trial_assignment_unverified")
                root, app = find_current(api, initializer.email, frozen, old)
                validate_admission(app)
                if app.get("carbon_admission", {}).get("can_test") is not True:
                    raise InitError("carbon_test_not_available")
                if paid_present(initializer.configs(project, location)):
                    result["status"] = "paid_skipped"
                else:
                    payload = {"name": root["name"], "auth_index": app["auth_index"], "binding_id": app["binding_id"], "expected_revision": app["revision"], "expected": {key: app[key] for key in RESOURCE_KEYS}, "model": CARBON}
                    result.update(status="started", submitted=True)
                    journal.append("started", app["binding_id"], result)
                    if progress:
                        progress(result)
                    calls += 1
                    http, value = api.test(payload)
                    result.update(outcome(http, value, app))
                    journal.append("result", app["binding_id"], result)
        except InitError as error:
            result.update(status="unknown_no_retry" if result["submitted"] else "not_tested", code=str(error))
            if result["submitted"]:
                journal.append("result", old["binding_id"], result)
        results.append(result)
        if progress:
            progress(result)
    final = api.account(initializer.email)
    final_apps = {app["binding_id"]: app for app in final["gemini_enterprise"]["collections"]}
    unchanged = final["gemini_enterprise"].get("account_id") == frozen["gemini_enterprise"].get("account_id") and all(app["binding_id"] in final_apps and binding_stamp(frozen, app) == binding_stamp(final, final_apps[app["binding_id"]]) for app in frozen["gemini_enterprise"]["collections"])
    verified = [item for item in results if item["status"] == "verified"]
    paid = [item for item in results if item["status"] == "paid_skipped"]
    return {"email": initializer.email, "model": CARBON, "results": results, "calls_submitted_this_run": calls, "verified": len(verified), "paid_skipped": len(paid), "application_settings_unchanged": unchanged, "complete": unchanged and bool(results) and all(item["status"] in ("verified", "paid_skipped") for item in results), "finished_at": dt.datetime.now(dt.timezone.utc).isoformat(), "management_requests": api.events, "google_requests": initializer.api.events}


def main(argv=None) -> int:
    command = argparse.ArgumentParser(description=__doc__)
    command.add_argument("--auth-file", type=Path, required=True)
    command.add_argument("--email", required=True)
    command.add_argument("--project", action="append", required=True)
    command.add_argument("--location", action="append", choices=ORIGINS)
    command.add_argument("--exclude-engine", action="append", default=[])
    command.add_argument("--management-url", required=True)
    command.add_argument("--management-key-env", default="CLIPROXY_MANAGEMENT_KEY")
    command.add_argument("--run-real-calls", action="store_true", required=True, help="Requires explicit current authorization for actual model calls")
    command.add_argument("--max-calls", type=int, required=True)
    command.add_argument("--journal", type=Path, required=True, help="Durable per-run journal; submitted tests are never automatically replayed")
    command.add_argument("--report", type=Path, required=True)
    args = command.parse_args(argv)
    if args.max_calls < 1 or len({path.resolve() for path in (args.auth_file, args.journal, args.report)}) != 3:
        raise InitError("invalid_carbon_verification_paths_or_limit")
    google = Google(Credential(args.auth_file, args.email), False)
    email, projects = identity_and_projects(google, args.project)
    initializer = Initializer(google, email, exclude=set(args.exclude_engine))
    api = TestManagement(args.management_url, os.environ.get(args.management_key_env, ""), True)
    numbers, locations = [item["number"] for item in projects], list(dict.fromkeys(args.location or ORIGINS))
    journal = Journal(args.journal, {"origin": api.origin, "email": email, "projects": numbers, "locations": locations, "excluded": sorted(initializer.exclude), "model": CARBON})
    result = verify(api, initializer, numbers, locations, journal, args.max_calls, lambda value: print(json.dumps(value), file=sys.stderr, flush=True))
    output = json.dumps(result, ensure_ascii=False, indent=2) + "\n"
    args.report.parent.mkdir(parents=True, exist_ok=True)
    args.report.write_text(output, encoding="utf-8")
    print(output, end="")
    return 0 if result["complete"] else 1


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (InitError, OSError, ValueError) as error:
        print(json.dumps({"status": "failed", "error": str(error) if isinstance(error, InitError) else type(error).__name__}), file=sys.stderr)
        raise SystemExit(1)

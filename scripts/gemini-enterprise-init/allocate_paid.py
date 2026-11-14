#!/usr/bin/env python3
"""Plan and apply approved Gemini Enterprise paid-seat allocations in batches."""

from __future__ import annotations

import argparse
import concurrent.futures
from contextlib import ExitStack
from dataclasses import asdict, dataclass
import json
from pathlib import Path
import re
import sys
import time
import urllib.parse

from initialize import APIError, Credential, Google, InitError, ORIGINS, PART, fingerprint, region_name

if sys.platform == "win32":
    import msvcrt
else:
    import fcntl

PARENT = re.compile(r"billingAccounts/[A-Za-z0-9-]+/billingAccountLicenseConfigs/[A-Za-z0-9_-]+")
RESERVED_PAID_STATES = {"ACTIVE", "NOT_STARTED", "DEACTIVATING"}
UNCERTAIN_WRITE_STATUSES = {408, 429, 500, 502, 503, 504}
REQUIRED_SERVICES = ("discoveryengine.googleapis.com", "aiplatform.googleapis.com", "businessaicode.googleapis.com",
                     "cloudresourcemanager.googleapis.com", "monitoring.googleapis.com")
SERVICE_USAGE = "https://serviceusage.googleapis.com"
SERVICE_OPERATION = re.compile(r"operations/[A-Za-z0-9_.-]{1,300}")


def check(condition: bool, error: str) -> None:
    if not condition:
        raise InitError(error)


def count(value: object) -> int:
    check(type(value) is int or isinstance(value, str) and value.isascii() and value.isdigit(), "invalid_seat_count")
    result = int(value)
    check(result >= 0, "invalid_seat_count")
    return result


def read_json(path: Path) -> dict:
    value = json.loads(path.read_text(encoding="utf-8-sig"))
    check(isinstance(value, dict), "invalid_json_object")
    return value


def error_code(error: Exception) -> str:
    return str(error) if isinstance(error, InitError) else type(error).__name__


@dataclass(frozen=True)
class Region:
    project: str
    location: str

    @classmethod
    def parse(cls, row: dict) -> Region:
        project, location = row["project"], row["location"]
        check(isinstance(project, str) and isinstance(location, str), "invalid_project_or_location")
        region_name(project, location)
        return cls(project, location)

    @property
    def resource(self) -> str:
        return region_name(self.project, self.location)

    def url(self, suffix: str, version: str = "v1") -> str:
        return f"{ORIGINS[self.location]}/{version}/{self.resource}/{suffix}"

    def contains_license(self, name: str) -> bool:
        prefix = self.resource + "/licenseConfigs/"
        return name.startswith(prefix) and bool(PART.fullmatch(name.removeprefix(prefix)))


@dataclass(frozen=True)
class Destination:
    region: Region
    seats: int


@dataclass(frozen=True)
class Allocation:
    auth_file: Path
    email: str
    source: Region
    source_license: str
    keep_seats: int
    destinations: tuple[Destination, ...]
    retain_trial: tuple[Region, ...]
    server: str | None = None

    @classmethod
    def parse(cls, row: dict, folder: Path) -> Allocation:
        email = row["email"]
        check(isinstance(email, str) and bool(re.fullmatch(r"[^@\s]+@[^@\s]+\.[^@\s]+", email)), "invalid_email")
        source = Region.parse(row["source"])
        license_id = row["source"]["license_id"]
        check(isinstance(license_id, str) and bool(PART.fullmatch(license_id)), "invalid_source_license_id")
        keep = count(row["source"]["keep_seats"])
        check(keep > 0, "source_must_retain_seats")
        destinations = tuple(Destination(Region.parse(item), count(item["seats"])) for item in row["destinations"])
        check(bool(destinations) and all(item.seats > 0 for item in destinations), "destinations_require_positive_seats")
        trial = tuple(Region.parse(item) for item in row.get("retain_trial", []))
        positions = (source, *(item.region for item in destinations), *trial)
        check(len(set(positions)) == len(positions), "duplicate_project_location")
        auth_file = (folder / row["auth_file"]).resolve()
        server = row.get("server")
        check(server is None or isinstance(server, str) and re.fullmatch(r"[A-Za-z0-9_.-]{1,100}", server) is not None,
              "invalid_credential_server_label")
        return cls(auth_file, email.casefold(), source, source.resource + "/licenseConfigs/" + license_id, keep, destinations, trial, server)

    @property
    def paid_regions(self) -> tuple[Region, ...]:
        return (self.source, *(item.region for item in self.destinations))

    @property
    def scope(self) -> dict:
        return {"email": self.email, "server": self.server, "source_license": self.source_license, "retain_seats": self.keep_seats,
                "destinations": [{"region": asdict(item.region), "seats": item.seats} for item in self.destinations],
                "retain_trial": [asdict(region) for region in self.retain_trial]}


class BillingGoogle(Google):
    def billing(self, region: Region, parent: str, action: str = "", body: dict | None = None) -> dict:
        check(PARENT.fullmatch(parent) is not None, "invalid_billing_subscription")
        check(action in ("", ":retractLicenseConfig", ":distributeLicenseConfig"), "invalid_billing_action")
        origin = ORIGINS[region.location] if action else ORIGINS["global"]
        url = f"{origin}/v1alpha/{parent}{action}"
        return self._request_json("POST" if action else "GET", url, urllib.parse.urlsplit(url), region.project, body)

    def service(self, project: str, service_id: str, enable: bool = False) -> dict:
        region_name(project, "global")
        check(service_id in REQUIRED_SERVICES, "service_not_in_initialization_scope")
        resource = f"projects/{project}/services/{service_id}"
        url = f"{SERVICE_USAGE}/v1/{resource}" + (":enable" if enable else "")
        data = self._request_json("POST" if enable else "GET", url, urllib.parse.urlsplit(url), project, {} if enable else None)
        if not enable:
            check(data.get("name") == resource and data.get("state") in {"ENABLED", "DISABLED"}, "service_state_unverified")
        return data

    def service_operation(self, project: str, name: str) -> dict:
        region_name(project, "global")
        check(SERVICE_OPERATION.fullmatch(name) is not None, "invalid_service_operation")
        url = f"{SERVICE_USAGE}/v1/{name}"
        return self._request_json("GET", url, urllib.parse.urlsplit(url), project, None)


class Job:
    def __init__(self, spec: Allocation, apply: bool, api: BillingGoogle | None = None):
        self.spec = spec
        self.api = api if api is not None else BillingGoogle(Credential(spec.auth_file, spec.email), apply)

    def verify_identity(self) -> None:
        value = self.api.request("GET", "https://www.googleapis.com/oauth2/v2/userinfo")
        email = value.get("email")
        check(isinstance(email, str) and email.casefold() == self.spec.email
              and value.get("verified_email") is not False, "credential_identity_mismatch")
        self.api.credential.pinned_email = self.spec.email
        self.api.credential.token()

    def services(self) -> dict:
        projects = sorted({region.project for region in self.spec.paid_regions + self.spec.retain_trial})
        return {project: {service_id: self.api.service(project, service_id)["state"] for service_id in REQUIRED_SERVICES}
                for project in projects}

    def prepare_services(self, approved: dict) -> None:
        for project, services in approved.items():
            for service_id, before in services.items():
                check(before in ("ENABLED", "DISABLED"), "approved_service_state_invalid")
                state = self.api.service(project, service_id)["state"]
                if state == "ENABLED":
                    continue
                check(before == "DISABLED", "required_service_disabled_after_plan")
                self.enable_required_service(project, service_id)

    def enable_required_service(self, project: str, service_id: str) -> None:
        op = self.api.service(project, service_id, enable=True)
        if op.get("done") is not True:
            name = op.get("name")
            check(isinstance(name, str), "invalid_service_operation")
            op = self.await_verified(lambda: self.api.service_operation(project, name),
                                     lambda value: value.get("done") is True, "service_enable_pending")
        check(not op.get("error"), "service_enable_failed")
        self.await_verified(lambda: self.api.service(project, service_id),
                            lambda value: value["state"] == "ENABLED", "service_enable_readback_failed")

    def license(self, region: Region, name: str) -> dict:
        check(region.contains_license(name), "license_outside_target_region")
        leaf = name.rsplit("/", 1)[-1]
        value = self.api.request("GET", region.url("licenseConfigs/" + leaf, "v1alpha"))
        check(value.get("name") == name, "license_readback_identity_mismatch")
        return value

    def region_state(self, region: Region) -> dict:
        configs = self.api.pages(region.url("licenseConfigs?pageSize=100"), "licenseConfigs")
        users = self.api.pages(region.url("userStores/default_user_store/userLicenses?pageSize=100"), "userLicenses")
        licenses = {}
        for item in configs:
            name = item.get("name")
            free = item.get("freeTrial", False)
            check(isinstance(name, str) and region.contains_license(name) and name not in licenses
                  and type(free) is bool and isinstance(item.get("state"), str), "invalid_regional_license")
            licenses[name] = {
                "state": item["state"], "free_trial": free, "seats": count(item.get("licenseCount", 0)),
                "tier": item.get("subscriptionTier"), "end": item.get("endDate"),
                "occupied": 0,
            }
        assigned = []
        for user in users:
            if user.get("licenseAssignmentState") != "ASSIGNED":
                continue
            name, principal = user.get("licenseConfig"), user.get("userPrincipal")
            check(name in licenses and isinstance(principal, str) and bool(principal), "invalid_user_license")
            licenses[name]["occupied"] += 1
            if principal.casefold() == self.spec.email:
                assigned.append(name)
        check(len(assigned) <= 1, "multiple_assignments_for_account")
        return {"licenses": licenses, "assigned": assigned[0] if assigned else None}

    def billing(self, parent: str) -> dict:
        value = self.api.billing(self.spec.source, parent)
        distributed = value.get("licenseConfigDistributions", {})
        check(value.get("name") == parent and value.get("state") == "ACTIVE"
              and isinstance(distributed, dict) and isinstance(value.get("subscriptionTier"), str)
              and bool(value["subscriptionTier"]), "active_billing_subscription_not_verified")
        total = count(value.get("licenseCount"))
        allocations = {}
        for name, seats in distributed.items():
            check(re.fullmatch(r"projects/[0-9]+/locations/[A-Za-z0-9-]+/licenseConfigs/[A-Za-z0-9_-]+", name) is not None,
                  "invalid_billing_allocation")
            amount = count(seats)
            if amount:
                allocations[name] = amount
        free = total - sum(allocations.values())
        check(free >= 0, "billing_seat_balance_invalid")
        return {"parent": parent, "state": value["state"], "tier": value["subscriptionTier"],
                "total": total, "allocated": allocations, "available": free, "end": value.get("endDate")}

    @staticmethod
    def paid(region: dict) -> list[str]:
        return [name for name, item in region["licenses"].items()
                if not item["free_trial"] and item["state"] in RESERVED_PAID_STATES]

    @staticmethod
    def trial_assignment(state: dict) -> bool:
        license = state["licenses"].get(state["assigned"], {})
        return license.get("free_trial") is True and license.get("state") == "ACTIVE"

    def inspect(self) -> dict:
        self.verify_identity()
        services = self.services()
        spec = self.spec
        source = self.license(spec.source, spec.source_license)
        parent = source.get("billingAccountLicenseConfig")
        check(isinstance(parent, str) and PARENT.fullmatch(parent) is not None and
              source.get("freeTrial", False) is False and source.get("state") == "ACTIVE",
              "source_is_not_an_active_paid_license")
        billing = self.billing(parent)
        current_seats = billing["allocated"].get(spec.source_license, 0)
        check(current_seats >= spec.keep_seats and count(source.get("licenseCount")) == current_seats
              and source.get("subscriptionTier") == billing["tier"], "source_and_billing_seats_do_not_match")
        source_state = self.region_state(spec.source)
        check(self.paid(source_state) == [spec.source_license], "source_has_another_active_paid_license")
        seat = source_state["licenses"][spec.source_license]
        check(seat["state"] == "ACTIVE" and seat["seats"] == current_seats
              and seat["occupied"] + int(source_state["assigned"] != spec.source_license)
              <= spec.keep_seats, "source_seats_are_occupied")
        check(source_state["assigned"] in (None, spec.source_license) or self.trial_assignment(source_state),
              "source_user_has_a_conflicting_assignment")

        rows = [{"region": asdict(spec.source), "retain_seats": spec.keep_seats, "state": source_state}]
        for target in spec.destinations:
            state = self.region_state(target.region)
            existing = self.paid(state)
            check(len(existing) <= 1, "destination_has_multiple_active_paid_licenses")
            name = existing[0] if existing else None
            if name:
                config = self.license(target.region, name)
                check(config.get("billingAccountLicenseConfig") == parent and config.get("state") == "ACTIVE"
                      and config.get("subscriptionTier") == billing["tier"]
                      and billing["allocated"].get(name) == state["licenses"][name]["seats"] == target.seats
                      and count(config.get("licenseCount")) == target.seats,
                      "destination_has_a_conflicting_paid_license")
                used = state["licenses"][name]["occupied"]
                check(used + int(state["assigned"] != name) <= target.seats, "destination_seats_are_occupied")
            check(state["assigned"] in (None, name) or self.trial_assignment(state),
                  "destination_user_has_a_conflicting_assignment")
            rows.append({"region": asdict(target.region), "license": name, "target_seats": target.seats, "state": state})

        trial_rows = []
        for region in spec.retain_trial:
            state = self.region_state(region)
            check(not self.paid(state) and self.trial_assignment(state), "retained_trial_assignment_not_verified")
            trial_rows.append({"region": asdict(region), "state": state})

        reclaim = current_seats - spec.keep_seats
        additional = sum(row["target_seats"] for row in rows[1:] if row["license"] is None)
        final_available = billing["available"] + reclaim - additional
        check(final_available >= 0, "insufficient_seats_for_plan")
        return {"scope": spec.scope, "email": spec.email, "source_license": spec.source_license,
                "billing": billing, "reclaim_seats": reclaim, "expected_available": final_available,
                "paid": rows, "retain_trial": trial_rows, "services": services}

    def await_verified(self, getter, condition, error: str):
        for delay in (0, 1, 2, 4, 6):
            if delay:
                time.sleep(delay)
            try:
                value = getter()
                if condition(value):
                    return value
            except APIError as issue:
                if issue.status not in (404, 429, 500, 502, 503, 504):
                    raise
        raise InitError(error)

    def send_write(self, send, report: dict, region: Region, operation: str) -> dict:
        try:
            return send()
        except InitError as issue:
            status = getattr(issue, "status", None)
            if status not in UNCERTAIN_WRITE_STATUSES and str(issue) != "google_transport_or_response_unavailable":
                raise
            report.setdefault("uncertain_responses", []).append({"operation": operation, "region": asdict(region),
                                                                  "error": error_code(issue)})
            return {}

    def confirm_allocation(self, region: Region, name: str, seats: int, parent: str, tier: str) -> dict:
        def read_state():
            return self.license(region, name), self.region_state(region)

        def matches(value):
            cfg, state = value
            return (cfg.get("freeTrial", False) is False and cfg.get("state") == "ACTIVE"
                    and cfg.get("billingAccountLicenseConfig") == parent and cfg.get("subscriptionTier") == tier
                    and count(cfg.get("licenseCount")) == seats and self.paid(state) == [name]
                    and state["licenses"][name]["seats"] == seats)

        _, state = self.await_verified(read_state, matches, "paid_allocation_readback_failed")
        return state

    def wait_assignment(self, region: Region, op: dict) -> None:
        if op.get("done") is not True:
            name = op.get("name")
            scope = re.escape(region.resource)
            check(isinstance(name, str) and re.fullmatch(
                scope + r"(?:/userStores/default_user_store)?/operations/[A-Za-z0-9_.-]{1,240}", name) is not None,
                "invalid_assignment_operation")
            url = f"{ORIGINS[region.location]}/v1/{name}"
            op = self.await_verified(lambda: self.api.request("GET", url), lambda value: value.get("done") is True,
                                     "assignment_operation_pending")
        response = op.get("response") or {}
        check(not (op.get("error") or response.get("errorSamples") or response.get("failedUserPrincipals")
                   or response.get("partialFailures") or count(response.get("failureCount", 0))), "assignment_operation_failed")

    def assign(self, region: Region, name: str, seats: int, parent: str, tier: str, previous: str | None, report: dict) -> None:
        state = self.confirm_allocation(region, name, seats, parent, tier)
        if state["assigned"] == name:
            return
        check(state["assigned"] == previous and (previous is None or self.trial_assignment(state)),
              "user_assignment_changed_after_plan")
        check(state["licenses"][name]["occupied"] < seats, "paid_seat_occupied_after_plan")
        request = {
            "inlineSource": {"userLicenses": [{"userPrincipal": self.spec.email, "licenseConfig": name}],
                             "updateMask": "userPrincipal,licenseConfig"},
            "deleteUnassignedUserLicenses": False,
        }
        op = self.send_write(lambda: self.api.request("POST", region.url("userStores/default_user_store:batchUpdateUserLicenses"), request),
                             report, region, "batchUpdateUserLicenses")
        if op:
            self.wait_assignment(region, op)
        self.await_verified(lambda: self.region_state(region), lambda now: now["assigned"] == name,
                            "paid_user_assignment_readback_failed")

    def execute(self, plan: dict, report: dict) -> None:
        spec = self.spec
        parent = plan["billing"]["parent"]
        tier = plan["billing"]["tier"]
        allocations = dict(plan["billing"]["allocated"])
        source = spec.source_license
        paid = [(spec.source, source, spec.keep_seats)]

        def billing_matches(value):
            return value["allocated"] == allocations and value["total"] == plan["billing"]["total"] and value["tier"] == tier

        report["stage"] = "reclaim"
        if plan["reclaim_seats"]:
            check(billing_matches(self.billing(parent)), "billing_changed_before_reclaim")
            check(self.region_state(spec.source) == plan["paid"][0]["state"], "source_changed_before_reclaim")
            payload = {"licenseConfig": source, "licenseCount": str(plan["reclaim_seats"]), "fullRetract": False}
            reply = self.send_write(lambda: self.api.billing(spec.source, parent, ":retractLicenseConfig", payload),
                                    report, spec.source, "retractLicenseConfig")
            receipt = reply.get("licenseConfig") or {}
            check(receipt.get("name", source) == source, "reclaim_receipt_mismatch")
            allocations[source] = spec.keep_seats
            self.await_verified(lambda: self.billing(parent), billing_matches, "reclaim_billing_readback_failed")
        self.confirm_allocation(spec.source, source, spec.keep_seats, parent, tier)
        report["reclaimed"] = plan["reclaim_seats"]

        report["stage"] = "distribution"
        report["distributions"] = []
        for row, target in zip(plan["paid"][1:], spec.destinations):
            region, seats = target.region, target.seats
            name = row["license"]
            if name is None:
                check(billing_matches(self.billing(parent)), "billing_changed_before_distribution")
                check(self.region_state(region) == row["state"], "destination_changed_before_distribution")
                payload = {"projectNumber": region.project, "location": region.location, "licenseCount": str(seats)}
                answer = self.send_write(lambda: self.api.billing(region, parent, ":distributeLicenseConfig", payload),
                                         report, region, "distributeLicenseConfig")
                receipt = (answer.get("licenseConfig") or {}).get("name")

                def distribution_matches(billing):
                    new = set(billing["allocated"]) - set(allocations)
                    if len(new) != 1:
                        return False
                    candidate = next(iter(new))
                    return (region.contains_license(candidate) and (not receipt or candidate == receipt)
                            and billing["total"] == plan["billing"]["total"] and billing["tier"] == tier
                            and billing["allocated"] == {**allocations, candidate: seats})

                read = self.await_verified(lambda: self.billing(parent), distribution_matches, "distribution_billing_readback_failed")
                name = next(iter(set(read["allocated"]) - set(allocations)))
                allocations[name] = seats
            self.confirm_allocation(region, name, seats, parent, tier)
            paid.append((region, name, seats))
            report["distributions"].append({"region": asdict(region), "license": name, "seats": seats, "active": True})

        check(billing_matches(self.billing(parent)), "billing_changed_before_assignment")
        report["stage"] = "assignments"
        report["assigned"] = []
        for (region, name, seats), before in zip(paid, plan["paid"]):
            self.assign(region, name, seats, parent, tier, before["state"]["assigned"], report)
            report["assigned"].append({"region": asdict(region), "license": name, "state": "ACTIVE", "assignment": "ASSIGNED"})

        report["stage"] = "final_readback"
        for region, name, seats in paid:
            state = self.confirm_allocation(region, name, seats, parent, tier)
            check(state["assigned"] == name, "final_paid_assignment_not_verified")
        retained = []
        for region, before in zip(spec.retain_trial, plan["retain_trial"]):
            state = self.region_state(region)
            check(not self.paid(state) and self.trial_assignment(state) and
                  state["assigned"] == before["state"]["assigned"], "retained_trial_changed")
            trial = state["licenses"][state["assigned"]]
            retained.append({"region": asdict(region), "license": state["assigned"], "state": trial["state"],
                             "assignment": "ASSIGNED", "end": trial["end"]})
        final = self.billing(parent)
        check(billing_matches(final) and final["available"] == plan["expected_available"], "final_billing_not_verified")
        report.update(stage="complete", complete=True, billing=final, retained_trial=retained)


def lock_resources(root: Path, parent: str, regions: tuple[Region, ...]):
    stack = ExitStack()
    keys = {"subscription/" + parent, *(region.resource for region in regions), *("services/" + region.project for region in regions)}
    try:
        for key in sorted(keys):
            stream = stack.enter_context((root / (fingerprint(key) + ".lock")).open("a+b"))
            stream.seek(0)
            try:
                if sys.platform == "win32":
                    msvcrt.locking(stream.fileno(), msvcrt.LK_NBLCK, 1)
                else:
                    fcntl.flock(stream.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
            except OSError:
                raise InitError("subscription_or_region_has_an_active_local_writer") from None
    except BaseException:
        stack.close()
        raise
    return stack


def apply_job(job: Job, approved: dict, lock_root: Path) -> dict:
    report = {"email": job.spec.email, "server": job.spec.server, "stage": "confirm_plan", "complete": False}
    try:
        check(job.spec.scope == approved.get("scope"), "manifest_does_not_match_approved_plan")
        with lock_resources(lock_root, approved["billing"]["parent"], job.spec.paid_regions + job.spec.retain_trial):
            current = job.inspect()
            comparison = dict(current)
            comparison["services"] = {
                project: {service_id: approved["services"][project][service_id] if state == "ENABLED" else state
                          for service_id, state in services.items()} for project, services in current["services"].items()
            }
            check(comparison == approved, "approved_plan_is_stale")
            report["stage"] = "required_apis"
            job.prepare_services(approved["services"])
            report["services"] = job.services()
            check(all(state == "ENABLED" for services in report["services"].values() for state in services.values()),
                  "required_google_services_not_verified")
            job.execute(approved, report)
    except (InitError, OSError, ValueError, KeyError, TypeError) as error:
        report["error"] = error_code(error)
    return report


def plan_job(job: Job) -> dict:
    try:
        return job.inspect()
    except (InitError, OSError, ValueError, KeyError, TypeError) as error:
        return {"scope": job.spec.scope, "email": job.spec.email, "error": error_code(error)}


def shared_parents(plans: list[dict]) -> set[str]:
    seen = set()
    shared = set()
    for plan in plans:
        if "error" in plan:
            continue
        parent = plan["billing"]["parent"]
        if parent in seen:
            shared.add(parent)
        seen.add(parent)
    return shared


def planning(jobs: list[Job], workers: int) -> dict:
    with concurrent.futures.ThreadPoolExecutor(max_workers=min(workers, len(jobs))) as executor:
        plans = list(executor.map(plan_job, jobs))
    shared = shared_parents(plans)
    for item in plans:
        if "billing" in item and item["billing"]["parent"] in shared:
            item["error"] = "shared_subscription_requires_one_confirmed_plan_at_a_time"
    plan = {"version": 1, "allocations": plans}
    if all("error" in item for item in plans):
        return {**plan, "complete": False}
    return {**plan, "complete": all("error" not in item for item in plans), "approval_id": fingerprint(plan)}


def execution(jobs: list[Job], workers: int, approved: list[dict], lock_root: Path) -> dict:
    check(len(jobs) == len(approved), "approved_plan_job_count_invalid")
    check(all(job.spec.scope == item.get("scope") for job, item in zip(jobs, approved)),
          "manifest_does_not_match_approved_plan")
    check(not shared_parents(approved), "shared_subscription_requires_one_confirmed_plan_at_a_time")
    queue = []
    active = {}
    results = [None] * len(jobs)
    for position, (job, item) in enumerate(zip(jobs, approved)):
        if "error" in item:
            results[position] = {"email": job.spec.email, "server": job.spec.server, "stage": "plan_skipped",
                                 "complete": False, "error": item["error"]}
        else:
            queue.append((position, (job, item)))
    with concurrent.futures.ThreadPoolExecutor(max_workers=min(workers, len(jobs))) as executor:
        while queue or active:
            index = 0
            reserved = set().union(*(projects for _, projects in active.values()))
            while index < len(queue) and len(active) < workers:
                position, (job, plan) = queue[index]
                projects = {region.project for region in job.spec.paid_regions + job.spec.retain_trial}
                if projects.isdisjoint(reserved):
                    task = executor.submit(apply_job, job, plan, lock_root)
                    active[task] = position, projects
                    reserved.update(projects)
                    queue.pop(index)
                else:
                    index += 1
            done, _ = concurrent.futures.wait(active, return_when=concurrent.futures.FIRST_COMPLETED)
            for task in done:
                position, _ = active.pop(task)
                results[position] = task.result()
    return {"complete": all(result["complete"] for result in results), "results": results}


def parser() -> argparse.ArgumentParser:
    cli = argparse.ArgumentParser(description=__doc__)
    cli.add_argument("--manifest", type=Path, required=True, help="Local JSON defining credential files and desired seat placement")
    cli.add_argument("--workers", type=int, default=8, help="Maximum number of independent credentials in flight")
    cli.add_argument("--apply-plan", type=Path, help="Apply the exact saved read-only plan after human confirmation")
    cli.add_argument("--approved-digest", help="Copy the approval_id of the plan confirmed by the human")
    cli.add_argument("--lock-dir", type=Path, default=Path.home() / ".codex" / "gemini-enterprise-allocation-locks",
                     help="Shared local lock directory for concurrent executions of this command")
    return cli


def main() -> int:
    args = parser().parse_args()
    check(args.workers > 0, "workers_must_be_positive")
    check(bool(args.apply_plan) == bool(args.approved_digest), "apply_requires_a_confirmed_plan_and_digest")
    manifest = read_json(args.manifest)
    allocations = manifest.get("accounts")
    check(isinstance(allocations, list) and bool(allocations), "manifest_has_no_accounts")
    specs = [Allocation.parse(row, args.manifest.resolve().parent) for row in allocations]
    plans = None
    if args.apply_plan:
        saved = read_json(args.apply_plan)
        plan = {"version": saved.get("version"), "allocations": saved.get("allocations")}
        check(plan["version"] == 1 and isinstance(plan["allocations"], list)
              and saved.get("approval_id") == args.approved_digest == fingerprint(plan),
              "approval_digest_does_not_match_saved_plan")
        plans = plan["allocations"]
    jobs = [Job(item, apply=plans is not None) for item in specs]
    if plans is None:
        result = planning(jobs, args.workers)
    else:
        args.lock_dir.mkdir(parents=True, exist_ok=True)
        result = execution(jobs, args.workers, plans, args.lock_dir)
    print(json.dumps(result, ensure_ascii=True, indent=2))
    return 0 if result.get("complete", True) else 1


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (InitError, OSError, ValueError, KeyError, TypeError) as error:
        print(json.dumps({"error": str(error) if isinstance(error, InitError) else type(error).__name__}), file=sys.stderr)
        raise SystemExit(1)

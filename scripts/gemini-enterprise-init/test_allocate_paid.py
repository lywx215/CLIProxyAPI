"""Offline coverage for approval, seat reconciliation, and concurrent paid allocations."""

import copy
import json
from pathlib import Path
import tempfile
import threading
import unittest
from unittest.mock import patch
import urllib.parse

import allocate_paid as paid
import initialize as init


class MemoryGoogle:
    def __init__(self, spec, subscription_suffix, barrier=None):
        self.credential = type("MemoryCredential", (), {"token": lambda _: "synthetic"})()
        self.spec = spec
        self.parent = f"billingAccounts/account/billingAccountLicenseConfigs/{subscription_suffix}"
        self.placements = {spec.source_license: 5}
        self.names = {}
        for region in spec.paid_regions + spec.retain_trial:
            self.names[region] = region.resource + "/licenseConfigs/free_trial_gemini"
        self.assignments = dict(self.names)
        self.events = []
        self.disabled = {(spec.source.project, "cloudresourcemanager.googleapis.com")}
        self.fail_distribution = False
        self.lose_reclaim_receipt = False
        self.barrier = barrier

    def descriptions(self, region):
        trial = {"name": self.names[region], "freeTrial": True, "licenseCount": "50",
                 "state": "ACTIVE", "subscriptionTier": init.TIER, "endDate": {"year": 2026, "month": 12, "day": 14}}
        configs = [trial]
        for name, seats in self.placements.items():
            if region.contains_license(name):
                configs.append({"name": name, "state": "ACTIVE", "licenseCount": str(seats),
                                "billingAccountLicenseConfig": self.parent, "subscriptionTier": init.TIER})
        return configs

    def details(self, url):
        path = urllib.parse.urlsplit(url).path
        parts = path.split("/")
        return paid.Region(parts[3], parts[5]), "/".join(parts[2:])

    def pages(self, url, field):
        region, _ = self.details(url)
        if field == "licenseConfigs":
            return self.descriptions(region)
        if field == "userLicenses":
            return [{"userPrincipal": self.spec.email, "licenseAssignmentState": "ASSIGNED",
                     "licenseConfig": self.assignments[region]}]
        raise AssertionError(field)

    def request(self, method, url, body=None):
        if url.endswith("/userinfo"):
            return {"email": self.spec.email, "verified_email": True}
        region, name = self.details(url)
        if method == "GET":
            return next(item for item in self.descriptions(region) if item["name"] == name)
        if name.endswith(":batchUpdateUserLicenses"):
            self.assert_scope_assignment(body)
            self.assignments[region] = body["inlineSource"]["userLicenses"][0]["licenseConfig"]
            self.events.append(("assign", region))
            return {"done": True, "response": {}}
        raise AssertionError((method, name))

    def assert_scope_assignment(self, body):
        entries = body["inlineSource"]["userLicenses"]
        assert len(entries) == 1 and entries[0]["userPrincipal"] == self.spec.email
        assert body["deleteUnassignedUserLicenses"] is False

    def billing(self, region, parent, action="", body=None):
        assert parent == self.parent
        if not action:
            return {"name": parent, "state": "ACTIVE", "subscriptionTier": init.TIER, "licenseCount": "5",
                    "licenseConfigDistributions": copy.deepcopy(self.placements)}
        self.events.append((action, region))
        if action == ":retractLicenseConfig":
            if self.barrier:
                self.barrier.wait(timeout=5)
            assert body["fullRetract"] is False
            self.placements[body["licenseConfig"]] -= int(body["licenseCount"])
            if self.lose_reclaim_receipt:
                raise init.InitError("google_transport_or_response_unavailable")
            return {"licenseConfig": {"name": body["licenseConfig"]}}
        assert action == ":distributeLicenseConfig"
        if self.fail_distribution:
            raise init.APIError(412)
        assert body["projectNumber"] == region.project and body["location"] == region.location
        name = region.resource + "/licenseConfigs/paid"
        self.placements[name] = int(body["licenseCount"])
        return {"licenseConfig": {"name": name}}

    def service(self, project, service_id, enable=False):
        if enable:
            self.events.append(("service", (project, service_id)))
            self.disabled.remove((project, service_id))
            return {"done": True}
        return {"state": "DISABLED" if (project, service_id) in self.disabled else "ENABLED"}


def fake_job(index, barrier=None):
    project, second = str(index * 10 + 1), str(index * 10 + 2)
    spec = paid.Allocation.parse({
        "auth_file": "synthetic.json", "email": f"user{index}@example.test",
        "source": {"project": project, "location": "global", "license_id": "source", "keep_seats": 1},
        "destinations": [
            {"project": project, "location": "us", "seats": 1},
            {"project": project, "location": "eu", "seats": 1},
            {"project": second, "location": "global", "seats": 1},
            {"project": second, "location": "us", "seats": 1},
        ],
        "retain_trial": [{"project": second, "location": "eu"}],
    }, Path("."))
    return paid.Job(spec, apply=True, api=MemoryGoogle(spec, str(index), barrier))


class PaidAllocationTests(unittest.TestCase):
    def test_cli_rejects_a_changed_confirmed_digest_before_credentials_are_read(self):
        with tempfile.TemporaryDirectory() as folder:
            folder = Path(folder)
            manifest, approved = folder / "input.json", folder / "plan.json"
            job = fake_job(1)
            row = job.spec.scope
            manifest.write_text(json.dumps({"accounts": [{
                "auth_file": "inaccessible.json", "email": row["email"],
                "source": {**paid.asdict(job.spec.source), "license_id": "source", "keep_seats": 1},
                "destinations": [{**item["region"], "seats": item["seats"]} for item in row["destinations"]],
                "retain_trial": row["retain_trial"],
            }]}), encoding="utf-8")
            saved = paid.planning([job], 8)
            saved["allocations"][0]["reclaim_seats"] = 2
            approved.write_text(json.dumps(saved), encoding="utf-8")
            arguments = ["allocate_paid.py", "--manifest", str(manifest), "--apply-plan", str(approved),
                         "--approved-digest", saved["approval_id"]]
            with patch("sys.argv", arguments), self.assertRaisesRegex(init.InitError, "approval_digest_does_not_match"):
                paid.main()

    def test_independent_credentials_reclaim_concurrently_and_finish(self):
        barrier = threading.Barrier(2)
        jobs = [fake_job(1, barrier), fake_job(2, barrier)]
        jobs[0].api.lose_reclaim_receipt = True
        before = paid.planning(jobs, 8)
        self.assertTrue(all(not item.api.events for item in jobs))
        self.assertTrue(all(row["reclaim_seats"] == 4 for row in before["allocations"]))
        with tempfile.TemporaryDirectory() as folder:
            result = paid.execution(jobs, 8, before["allocations"], Path(folder))
        self.assertTrue(result["complete"])
        for row, job in zip(result["results"], jobs):
            self.assertEqual(row["reclaimed"], 4)
            self.assertEqual(row["billing"]["available"], 0)
            self.assertEqual(len(row["assigned"]), 5)
            self.assertEqual(row["retained_trial"][0]["assignment"], "ASSIGNED")
            self.assertEqual([action for action, _ in job.api.events].count(":retractLicenseConfig"), 1)
            self.assertEqual(job.api.events[0][0], "service")
        self.assertEqual(result["results"][0]["uncertain_responses"][0]["operation"], "retractLicenseConfig")

    def test_stale_plan_rejects_seat_changes_and_failed_job_does_not_block_others(self):
        jobs = [fake_job(1), fake_job(2)]
        before = paid.planning(jobs, 8)
        jobs[0].api.placements[jobs[0].spec.source_license] = 4
        with tempfile.TemporaryDirectory() as folder:
            result = paid.execution(jobs, 8, before["allocations"], Path(folder))
        self.assertEqual(result["results"][0]["error"], "approved_plan_is_stale")
        self.assertFalse(jobs[0].api.events)
        self.assertTrue(result["results"][1]["complete"])

    def test_partial_reclaim_resumes_from_new_plan_without_reclaiming_twice(self):
        job = fake_job(1)
        before = paid.planning([job], 8)
        job.api.fail_distribution = True
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            first = paid.execution([job], 8, before["allocations"], root)
            self.assertFalse(first["complete"])
            self.assertEqual(first["results"][0]["reclaimed"], 4)
            job.api.fail_distribution = False
            refreshed = paid.planning([job], 8)
            self.assertEqual(refreshed["allocations"][0]["reclaim_seats"], 0)
            second = paid.execution([job], 8, refreshed["allocations"], root)
        self.assertTrue(second["complete"])
        self.assertEqual([action for action, _ in job.api.events].count(":retractLicenseConfig"), 1)

    def test_shared_parent_rows_do_not_block_an_independent_subscription(self):
        jobs = [fake_job(1), fake_job(2), fake_job(3)]
        jobs[1].api.parent = jobs[0].api.parent
        before = paid.planning(jobs, 8)
        self.assertFalse(before["complete"])
        with tempfile.TemporaryDirectory() as folder:
            result = paid.execution(jobs, 8, before["allocations"], Path(folder))
        self.assertEqual([row["stage"] for row in result["results"]], ["plan_skipped", "plan_skipped", "complete"])
        self.assertFalse(jobs[0].api.events or jobs[1].api.events)

    def test_project_lock_blocks_a_second_writer_and_releases_on_close(self):
        one, two = fake_job(1), fake_job(2)
        same_project = paid.Region(one.spec.source.project, "us")
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            with paid.lock_resources(root, one.api.parent, (one.spec.source,)):
                with self.assertRaisesRegex(init.InitError, "active_local_writer"):
                    with paid.lock_resources(root, two.api.parent, (same_project,)):
                        self.fail("same project lock was acquired twice")
            with paid.lock_resources(root, two.api.parent, (same_project,)):
                self.assertTrue(any(root.iterdir()))


if __name__ == "__main__":
    unittest.main()

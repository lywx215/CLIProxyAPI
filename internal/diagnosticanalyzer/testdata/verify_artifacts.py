"""Offline byte verification; standard library only, no producer imports."""

import hashlib
import json
import subprocess
from pathlib import Path

ROOT = Path(__file__).resolve().parents[3]
DATA = Path(__file__).resolve().parent
CONTRACT = ROOT / "contracts/diagnostics/v1"
BASELINE = "4996e7ae12b2af38f3bdc490eedf32e1887aeed6"
DIGEST = "ddb202238cfdaabef1af11575dbfcac788fdbc5a457aea5e72b913afc5b478d4"


def check(ok, reason):
    if not ok:
        raise RuntimeError(reason)


def sha(data):
    return hashlib.sha256(data).hexdigest()


manifest = (CONTRACT / "SHA256SUMS").read_bytes()
check(sha(manifest) == DIGEST, "contract manifest changed")
members = {"SHA256SUMS"}
for line in manifest.decode("utf-8").splitlines():
    digest, relative = line.split("  ", 1)
    members.add(relative)
    check(sha((CONTRACT / relative).read_bytes()) == digest, "contract member changed")
check(members == {p.relative_to(CONTRACT).as_posix() for p in CONTRACT.rglob("*") if p.is_file()},
      "contract member set changed")
paths = subprocess.check_output(["git", "ls-tree", "-r", "--name-only", BASELINE,
                                 "contracts/diagnostics/v1"], cwd=ROOT).decode().splitlines()
check(len(paths) == len(members) == 73, "frozen git member count")
for path in paths:
    original = subprocess.check_output(["git", "show", f"{BASELINE}:{path}"], cwd=ROOT)
    indexed = subprocess.check_output(["git", "show", f":{path}"], cwd=ROOT)
    check(original == indexed == (ROOT / path).read_bytes(), "frozen byte mismatch")
check((DATA.parent / "record.schema.json").read_bytes() ==
      (CONTRACT / "record.schema.json").read_bytes(), "embedded schema mismatch")

sources = json.loads((DATA / "sources.json").read_text(encoding="utf-8"))
records = 0
for source in sources["fixtures"]:
    data = (DATA / source["file"]).read_bytes()
    check(sha(data) == source["sha256"], "producer sample bytes changed")
    check(len(data.splitlines()) == source["records"], "producer record count changed")
    check(max(map(len, data.splitlines(keepends=True))) <= 4096, "producer line limit")
    records += source["records"]
print(f"PASS: 73 frozen Git/worktree/index files; embedded schema; {records} producer records")
print(f"Contract SHA-256: {DIGEST}")

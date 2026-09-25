"""Apply the frozen offline oracle to both approved producer line framings."""

import json
import runpy
from pathlib import Path

ROOT = Path(__file__).resolve().parents[3]
DATA = Path(__file__).resolve().parent
oracle = runpy.run_path(str(ROOT / "contracts/diagnostics/v1/validate.py"))
sources = json.loads((DATA / "sources.json").read_text(encoding="utf-8"))
count = 0
for source in sources["fixtures"]:
    for raw in (DATA / source["file"]).read_bytes().splitlines(keepends=True):
        if len(raw) > 4096:
            raise RuntimeError("producer line limit")
        payload = raw[6:] if raw.startswith(b"@diag ") else raw
        record = json.loads(payload)
        oracle["SCHEMA"].validate(record)
        if oracle["semantic_issues"](record):
            raise RuntimeError("producer semantics")
        count += 1
print(f"PASS: {count} producer records against frozen schema and semantic oracle")

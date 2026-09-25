"""Offline validation of synthetic records produced by the real Go boundary tests."""

import json
import runpy
import sys
from pathlib import Path

import jsonschema


root = Path(__file__).resolve().parents[3]
contract = root / "contracts/diagnostics/v1"
schema = json.loads((contract / "record.schema.json").read_text(encoding="utf-8"))
validator = jsonschema.Draft202012Validator(schema, format_checker=jsonschema.FormatChecker())
semantic_issues = runpy.run_path(str(contract / "validate.py"))["semantic_issues"]
count = 0
for source in sys.argv[1:]:
    for number, raw in enumerate(Path(source).read_bytes().splitlines(keepends=True), 1):
        assert len(raw) <= 4096, (source, number, "line_limit")
        assert raw.startswith(b"@diag "), (source, number, "framing")
        record = json.loads(raw[6:])
        validator.validate(record)
        assert not semantic_issues(record), (source, number, "semantic_invariant")
        count += 1
assert count, "no records"
print(f"Validated {count} Go-generated records against the frozen schema and semantics")

"""Convert saved v8 credential listings into a local Enterprise history baseline."""

import argparse
from datetime import datetime, timedelta, timezone
import json
import os
from pathlib import Path
import re
import sys
from zoneinfo import ZoneInfo, ZoneInfoNotFoundError


IDENTITY = re.compile(r"[0-9a-f]{64}")
BUCKET_SECONDS = 600
MAX_GO_INT = (1 << 63) - 1


def parse_time(value: str) -> datetime:
    at = datetime.fromisoformat(value)
    if at.tzinfo is None:
        raise ValueError("timestamps must include their UTC offset")
    return at.astimezone(timezone.utc)


def source_zone(value: str):
    if value == "UTC":
        return timezone.utc
    offset = re.fullmatch(r"([+-])(\d\d):(\d\d)", value)
    if offset:
        sign, hours, minutes = offset.groups()
        if int(minutes) >= 60:
            raise ValueError("invalid source timezone offset")
        delta = timedelta(hours=int(hours), minutes=int(minutes))
        return timezone(-delta if sign == "-" else delta)
    return ZoneInfo(value)


def count(row: dict, field: str) -> int:
    value = row.get(field, 0)
    if type(value) is not int or not 0 <= value <= MAX_GO_INT:
        raise ValueError(f"invalid {field} counter")
    return value


def bucket_time(index: int) -> datetime:
    return datetime.fromtimestamp(index * BUCKET_SECONDS, timezone.utc)


def label(index: int, zone) -> str:
    return "-".join(bucket_time(i).astimezone(zone).strftime("%H:%M") for i in (index, index + 1))


def recent_requests(row: dict, captured_at: datetime, zone) -> list:
    recent = row.get("recent_requests") or []
    if not recent:
        return []
    if len(recent) > 20:
        raise ValueError("unexpected recent-request window size")
    current = int(captured_at.timestamp()) // BUCKET_SECONDS
    for latest in (current, current - 1):
        first = latest - len(recent) + 1
        if all(item.get("time") == label(first + i, zone) for i, item in enumerate(recent)):
            return [
                {"at": bucket_time(first + i).isoformat(), "success": count(item, "success"), "failed": count(item, "failed")}
                for i, item in enumerate(recent)
                if count(item, "success") or count(item, "failed")
            ]
    raise ValueError("recent request labels do not match the capture time and source timezone")


def build_baseline(documents: list, captured_at: datetime, zone, trusted_times: dict) -> dict:
    accounts = {}
    sources = set()
    for document in documents:
        for row in document["files"]:
            enterprise = row.get("gemini_enterprise")
            if not isinstance(enterprise, dict) or not IDENTITY.fullmatch(enterprise.get("account_id") or ""):
                continue
            source = row.get("auth_index") or row.get("name")
            if not source or source in sources:
                raise ValueError("missing or repeated source credential in supplied listing pages")
            sources.add(source)
            account = accounts.setdefault(enterprise["account_id"], {})
            for collection in enterprise.get("collections", []):
                binding = collection.get("binding_id") or ""
                if not IDENTITY.fullmatch(binding):
                    raise ValueError("invalid Enterprise binding identity")
                saved = account.setdefault(binding, {"binding_id": binding, "success": 0, "failed": 0, "recent_requests": {}})
                saved["success"] += count(collection, "success")
                saved["failed"] += count(collection, "failed")
                for bucket in recent_requests(collection, captured_at, zone):
                    at = bucket["at"]
                    aggregate = saved["recent_requests"].setdefault(at, {"at": at, "success": 0, "failed": 0})
                    aggregate["success"] += bucket["success"]
                    aggregate["failed"] += bucket["failed"]
    if set(trusted_times) - accounts.keys():
        raise ValueError("trusted upload times must reference accounts in the listing")
    output = []
    for key, collections in sorted(accounts.items()):
        account = {"account_id": key, "collections": []}
        if key in trusted_times:
            uploaded_at = parse_time(trusted_times[key])
            if uploaded_at > captured_at:
                raise ValueError("first upload time is after the capture time")
            account["first_uploaded_at"] = uploaded_at.isoformat()
        for _, collection in sorted(collections.items()):
            count(collection, "success")
            count(collection, "failed")
            recent = collection["recent_requests"]
            collection["recent_requests"] = [recent[key] for key in sorted(recent)[-20:]]
            account["collections"].append(collection)
        output.append(account)
    return {"version": 1, "captured_at": captured_at.isoformat(), "accounts": output}


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("pages", type=Path, nargs="+", help="saved /v8/management/credentials response pages")
    parser.add_argument("--captured-at", type=parse_time, required=True, help="UTC timestamp when the idle old server was read")
    parser.add_argument("--source-timezone", type=source_zone, required=True, help="old server timezone such as UTC, +08:00 or Asia/Shanghai")
    parser.add_argument("--trusted-first-uploads", type=Path, help="optional JSON mapping from account IDs to verified first upload timestamps")
    parser.add_argument("--output", type=Path, help="create a new baseline file using UTF-8, or omit for stdout")
    args = parser.parse_args()
    pages = [json.loads(path.read_text(encoding="utf-8-sig")) for path in args.pages]
    trusted_times = json.loads(args.trusted_first_uploads.read_text(encoding="utf-8-sig")) if args.trusted_first_uploads else {}
    data = json.dumps(build_baseline(pages, args.captured_at, args.source_timezone, trusted_times), separators=(",", ":")) + "\n"
    if args.output:
        fd = os.open(args.output, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
        with os.fdopen(fd, "w", encoding="utf-8") as output:
            output.write(data)
    else:
        sys.stdout.write(data)


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, KeyError, TypeError, ZoneInfoNotFoundError) as error:
        raise SystemExit(f"Baseline conversion failed: {error}") from None

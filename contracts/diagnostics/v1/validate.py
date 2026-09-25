"""Offline contract fixture validation; never import this into a service."""

import argparse
import hashlib
import ipaddress
import json
import re
from datetime import datetime
from pathlib import Path
from urllib.parse import urlsplit

from jsonschema import Draft202012Validator, FormatChecker, ValidationError

ROOT = Path(__file__).resolve().parent
ID = re.compile(r"[A-Za-z0-9._:/-]{1,128}\Z", re.ASCII)
TOKEN = re.compile(r"[A-Za-z0-9._-]{1,64}\Z", re.ASCII)
TRACE = re.compile(r"(?!0{32}\Z)[0-9a-f]{32}\Z", re.ASCII)
FORMATS = FormatChecker()


@FORMATS.checks("date-time", raises=ValueError)
def valid_datetime(value):
    if not isinstance(value, str):
        return True
    datetime.fromisoformat(value.replace("Z", "+00:00"))
    return True


SCHEMA = Draft202012Validator(
    json.loads((ROOT / "record.schema.json").read_text(encoding="utf-8")),
    format_checker=FORMATS,
)
PEERS_SCHEMA = Draft202012Validator(json.loads((ROOT / "peers.schema.json").read_text(encoding="utf-8")))


def require(ok, message):
    if not ok:
        raise AssertionError(message)


def read_json(path):
    def unique_object(pairs):
        result = {}
        for key, value in pairs:
            require(key not in result, f"Duplicate JSON key: {path.name}")
            result[key] = value
        return result

    return json.loads(path.read_text(encoding="utf-8"), object_pairs_hook=unique_object,
                      parse_constant=lambda _: require(False, "Nonfinite JSON number"))


def values(headers, key):
    return [v for k, v in headers if k.lower() == key]


def custom_id(raw):
    if not raw:
        return None, "missing"
    if len(raw) != 1 or "," in raw[0]:
        return None, "duplicate"
    if len(raw[0]) > 128:
        return None, "too_long"
    return (raw[0], "none") if ID.fullmatch(raw[0]) else (None, "invalid")


def parse_tracestate(raw):
    combined = ",".join(raw)
    if len(combined) > 512 or not combined.isascii():
        return None
    pairs = [v.strip(" \t") for v in combined.split(",") if v.strip(" \t")]
    if len(pairs) > 32:
        return None
    seen = set()
    for pair in pairs:
        key, separator, value = pair.partition("=")
        if not separator or key in seen or len(key) > 256 or not 1 <= len(value) <= 256:
            return None
        simple = re.fullmatch(r"[a-z][a-z0-9_*/-]{0,255}", key)
        multi = re.fullmatch(r"[a-z0-9][a-z0-9_*/-]{0,240}@[a-z][a-z0-9_*/-]{0,13}", key)
        if not (simple or multi) or not re.fullmatch(r"[\x20-\x2b\x2d-\x3c\x3e-\x7e]*[\x21-\x2b\x2d-\x3c\x3e-\x7e]", value):
            return None
        seen.add(key)
    return ",".join(pairs) or None


def header_oracle(case):
    headers = case["headers"]
    parents = values(headers, "traceparent")
    context = "generated" if not parents else "invalid_replaced"
    trace, parent, flags, state = "<generated>", None, "00", None
    if len(parents) == 1:
        value = parents[0]
        match = re.fullmatch(r"([0-9a-f]{2})-([0-9a-f]{32})-([0-9a-f]{16})-([0-9a-f]{2})(.*)", value, flags=re.ASCII)
        if match and len(value) <= 512 and "," not in value and all(32 <= ord(c) <= 126 for c in value):
            version, tid, sid, incoming_flags, rest = match.groups()
            valid_version = version != "ff" and (not rest or (version != "00" and rest.startswith("-")))
            if valid_version and tid != "0" * 32 and sid != "0" * 16:
                context, trace, parent = "accepted", tid, sid
                flags = f"{int(incoming_flags, 16) & 1:02x}"
                state = parse_tracestate(values(headers, "tracestate"))
    authenticated = case.get("authenticated", False)
    peer = authenticated and case.get("configuredInboundAdapter", False)
    candidates = ["x-diag-request-id", "x-request-id"] if peer else ["x-request-id"]
    caller, source, rejection = None, None, "none"
    for name in candidates:
        candidate, why = custom_id(values(headers, name))
        if why not in ("none", "missing") and rejection == "none":
            rejection = why
        if candidate is not None:
            caller, source = candidate, name
            break
    trust = "none" if caller is None else ("configured_peer" if peer and source == "x-diag-request-id" else "authenticated" if authenticated else "unverified")
    return {"contextSource": context, "traceId": trace, "parentSpanId": parent,
            "outputFlags": flags, "tracestate": state, "callerRequestId": caller,
            "callerHeader": source, "callerTrust": trust, "rejected": rejection}


def outbound_oracle(case):
    result = {}
    response = case.get("response", False)
    owned = case.get("diagnosticOwned", False)
    for key, value in case["headers"]:
        name = key.lower()
        if name.startswith("x-diag-") or (not response and (case["allowed"] or owned) and name in ("traceparent", "tracestate")):
            continue
        result.setdefault(name, []).append(value)
    if response:
        result["x-diag-request-id"] = [case["requestId"]]
        result["x-diag-trace-id"] = [case["traceId"]]
    elif case["allowed"]:
        result["traceparent"] = [f"00-{case['traceId']}-{case['callSpanId']}-{case['flags']}"]
        if case.get("tracestate"):
            result["tracestate"] = [case["tracestate"]]
        result["x-diag-request-id"] = [case["requestId"]]
    return {"headers": result, "diagnosticOwned": not response and case["allowed"]}


def copy_source_oracle(case):
    return [[key, value] for key, value in case["headers"]
            if key.lower() not in ("traceparent", "tracestate") and not key.lower().startswith("x-diag-")]


def redirect_oracle(case):
    current_headers = case["headers"]
    owned = False
    results = []
    for step in case["steps"]:
        peer_result = peers_oracle({"config": case["peers"], "target": step["target"]})
        require((peer_result["match"] is not None) == step["allowed"], "Redirect peer policy fixture disagrees with actual target")
        # Remove our own pair before a provider constructs new-hop headers.
        current_headers = [[key, value] for key, value in current_headers
                           if not key.lower().startswith("x-diag-") and
                           not (owned and key.lower() in ("traceparent", "tracestate"))]
        owned = False
        current_headers += step.get("businessHeaders", [])
        result = outbound_oracle({**step, "headers": current_headers, "diagnosticOwned": owned})
        results.append(result)
        current_headers = [[key, value] for key, vals in result["headers"].items() for value in vals]
        owned = result["diagnosticOwned"]
    return results


def http_ingress_oracle(case):
    # Exercise a real HTTP parser in memory, without sockets or service code.
    import h11

    conn = h11.Connection(h11.SERVER)
    wire = "GET /fixture HTTP/1.1\r\nHost: fixture.test\r\n" + "\r\n".join(case["wireHeaderLines"]) + "\r\n\r\n"
    conn.receive_data(wire.encode("ascii"))
    request = conn.next_event()
    require(isinstance(request, h11.Request), "HTTP fixture did not produce a request")
    observed = [[key.decode("ascii"), value.decode("ascii")] for key, value in request.headers if key != b"host"]
    require(observed == case["headers"], "HTTP framework field values differ from the fixture")
    return header_oracle(case)


def peer_response_oracle(case):
    if not case["peerConfigured"]:
        return {"peerRequestId": None, "peerTraceId": None, "peerIdRejected": "none"}
    request_id, request_reason = custom_id(values(case["headers"], "x-diag-request-id"))
    trace_id, trace_reason = custom_id(values(case["headers"], "x-diag-trace-id"))
    if trace_id is not None and not TRACE.fullmatch(trace_id):
        trace_id, trace_reason = None, "invalid"
    rejection = next((reason for reason in (request_reason, trace_reason) if reason not in ("none", "missing")), "none")
    return {"peerRequestId": request_id, "peerTraceId": trace_id, "peerIdRejected": rejection}


def origin(url, configured=False):
    if not isinstance(url, str) or not url.isascii() or any(ord(c) <= 32 or ord(c) >= 127 for c in url) or "\\" in url:
        raise ValueError("origin")
    parsed = urlsplit(url)
    if parsed.scheme.lower() not in ("http", "https") or not parsed.netloc or parsed.username is not None or parsed.password is not None:
        raise ValueError("origin")
    if "@" in parsed.netloc or "%" in parsed.netloc or "#" in url:
        raise ValueError("origin")
    if configured and (parsed.path not in ("", "/") or "?" in url):
        raise ValueError("origin")
    host = parsed.hostname
    if not host or host.endswith("."):
        raise ValueError("host")
    if ":" in host:
        host = "[" + ipaddress.IPv6Address(host).compressed + "]"
    elif re.fullmatch(r"[0-9.]+", host):
        host = str(ipaddress.IPv4Address(host))
    elif not re.fullmatch(r"(?=.{1,253}\Z)[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)*", host.lower()):
        raise ValueError("host")
    # A trailing colon or an alternate numeric representation is never a default port.
    alternate_numeric = all(re.fullmatch(r"(?:0x[0-9a-f]+|[0-9]+)", label, re.I) for label in host.split(".")) and not re.fullmatch(r"[0-9.]+", host)
    if parsed.netloc.endswith(":") or alternate_numeric:
        raise ValueError("port_or_numeric_host")
    port = parsed.port
    if port is not None and not 1 <= port <= 65535:
        raise ValueError("port")
    return (parsed.scheme.lower(), host.lower(), port or (443 if parsed.scheme.lower() == "https" else 80)), parsed.path or "/"


def safe_path(path):
    if not path.startswith("/") or not path.isascii() or any(ord(c) < 33 or ord(c) > 126 for c in path):
        return False
    if "%" in path or "\\" in path:
        return False
    segments = path.split("/")[1:]
    # A single trailing slash is allowed on a target URL, not in configured prefixes.
    if segments and segments[-1] == "":
        segments.pop()
    return not any(segment in ("", ".", "..") for segment in segments)


def prefix_matches(prefix, path):
    return prefix == "/" or path == prefix or path.startswith(prefix + "/")


def peers_oracle(case):
    raw = case["config"]
    try:
        def config_object(pairs):
            result = {}
            for key, value in pairs:
                if key in result:
                    raise ValueError("duplicate_config_key")
                result[key] = value
            return result

        peers = [] if raw is None or raw == "" else json.loads(raw, object_pairs_hook=config_object) if isinstance(raw, str) else raw
        PEERS_SCHEMA.validate(peers)
        parsed = []
        aliases = set()
        for peer in peers:
            ori, _ = origin(peer["origin"], configured=True)
            path = peer["pathPrefix"]
            if not safe_path(path) or (path != "/" and path.endswith("/")) or peer["alias"] in aliases:
                raise ValueError("peer")
            for other_ori, other in parsed:
                if other_ori == ori and (prefix_matches(path, other["pathPrefix"]) or prefix_matches(other["pathPrefix"], path)):
                    raise ValueError("overlap")
            aliases.add(peer["alias"])
            parsed.append((ori, peer))
    except (ValueError, TypeError, ValidationError) as exc:
        # Schema errors and malformed inputs produce one stable classification.
        # No rejected input or exception text is copied to the result.
        del exc
        return {"configStatus": "config_invalid", "match": None}
    try:
        target_origin, path = origin(case["target"])
        if not safe_path(path):
            raise ValueError("path")
    except ValueError:
        return {"configStatus": "valid", "match": None}
    match = next((p["alias"] for ori, p in parsed if ori == target_origin and prefix_matches(p["pathPrefix"], path)), None)
    return {"configStatus": "valid", "match": match}


def source_oracle(case):
    def scope(value):
        keys = ["environment", "deploymentId", "service", "callerAliasScope", "callerAlias"]
        if value["callerAliasScope"] == "boot":
            keys += ["instanceId", "bootId"]
        return tuple(value.get(key) for key in keys)

    left, right = case["left"], case["right"]
    unknown = any(v.get("callerAlias") is None or v["callerAliasScope"] == "unknown" or v["deploymentId"] == "unassigned" for v in (left, right))
    return {"sameLookupScope": not unknown and scope(left) == scope(right),
            "candidateAliasMatch": left["callerRequestId"] is not None and left["callerRequestId"] == right["callerRequestId"],
            "merge": False, "scopeUnknown": unknown}


def resource_oracle(case):
    config = case["config"]
    result = {}
    for key, default in (("environment", "unassigned"), ("deploymentId", "unassigned"), ("nodeLabel", None)):
        value = config.get(key)
        result[key] = value if isinstance(value, str) and TOKEN.fullmatch(value) else default
    identity = re.compile(r"[A-Za-z0-9._-]{1,128}\Z", re.ASCII)
    for source, value in (("platform", case.get("platformId")), ("configured", config.get("instanceId"))):
        if isinstance(value, str) and identity.fullmatch(value):
            result.update(instanceId=value, instanceIdentitySource=source)
            break
    else:
        result.update(instanceId="<random-uuid>", instanceIdentitySource="ephemeral")
    result.update(service=case["localService"], bootId="<new-uuid-per-worker>")
    return result


def counts_oracle(case):
    calls = case["calls"]
    owners, call_ids, attempts, unknown = set(), set(), set(), set()
    for call in calls:
        owner = tuple(call[key] for key in ("service", "instanceId", "bootId", "serverSpanId"))
        call_key = owner + (call["spanId"],)
        owners.add(owner)
        call_ids.add(call_key)
        if call["retryScope"] is not None and (call["attemptId"] is not None or call["attemptNoUnique"] and call["attemptNo"] is not None):
            attempt = ("id", call["attemptId"]) if call["attemptId"] is not None else ("no", call["attemptNo"])
            attempts.add(owner + (call["retryScope"],) + attempt)
        else:
            unknown.add(call_key)
    return {"observedServerOwners": len(owners), "callCount": len(call_ids),
            "knownAttemptCount": len(attempts), "unknownAttemptCallCount": len(unknown)}


def graph_oracle(case):
    """Only the minimal record projection specified in vectors/README.md."""
    records = case["records"]
    unique = {}
    findings = set()
    for record in records:
        key = tuple(record[k] for k in ("service", "instanceId", "bootId", "spanId", "logSeq"))
        comparable = {k: v for k, v in record.items() if k != "sourceTrusted"}
        if key in unique:
            prior = {k: v for k, v in unique[key].items() if k != "sourceTrusted"}
            if prior != comparable:
                findings.add("event_conflict")
            unique[key]["sourceTrusted"] = unique[key]["sourceTrusted"] and record["sourceTrusted"]
        else:
            unique[key] = dict(record)
    nodes = list(unique.values())
    identities = {}
    for node in nodes:
        identities.setdefault((node["traceId"], node["spanId"]), []).append(node)
    if any(len(group) > 1 for group in identities.values()):
        findings.add("span_conflict")
    calls = [r for r in nodes if r["kind"] == "call"]
    servers = [r for r in nodes if r["kind"] == "server"]
    pairs = []
    for call in calls:
        if call["peerTraceId"] is not None and call["peerTraceId"] != call["traceId"]:
            findings.add("context_mismatch")
        receivers = [r for r in servers if r["traceId"] == call["traceId"] and r["parentSpanId"] == call["spanId"]]
        senders = [r for r in calls if r["traceId"] == call["traceId"] and r["spanId"] == call["spanId"]]
        if not call["peerConfigured"]:
            findings.add("not_participating")
            if receivers:
                findings.add("peer_conflict")
            continue
        if not receivers:
            findings.add("missing_peer")
        elif len(receivers) != 1 or len(senders) != 1:
            findings.add("ambiguous_parent")
        else:
            receiver = receivers[0]
            mismatch = (call["peerService"] != receiver["service"] or
                        (call["peerDeploymentId"] is not None and call["peerDeploymentId"] != receiver["deploymentId"]) or
                        (call["peerRequestId"] is not None and call["peerRequestId"] != receiver["requestId"]) or
                        (receiver["callerHeader"] == "x-diag-request-id" and receiver["callerTrust"] == "configured_peer" and receiver["callerRequestId"] != call["requestId"]))
            if mismatch:
                findings.add("peer_conflict")
            if not call["sourceTrusted"] or not receiver["sourceTrusted"]:
                findings.add("untrusted_source")
            pairs.append([call["spanId"], receiver["spanId"]])
    for server in servers:
        if server["parentSpanId"] is None:
            findings.add("root")
        elif not any(c["traceId"] == server["traceId"] and c["spanId"] == server["parentSpanId"] for c in calls):
            findings.add("external_parent_unverified")
    # This oracle intentionally covers one candidate edge per vector; DIAG-06
    # must classify each edge independently in a complete graph.
    conflicts = {"event_conflict", "span_conflict", "context_mismatch", "peer_conflict", "untrusted_source", "ambiguous_parent"}
    verified = pairs if not findings.intersection(conflicts) else []
    if verified:
        findings.add("verified")
    return {"findings": sorted(findings), "verifiedEdges": verified, "distinctEvents": len(nodes)}


def coverage_oracle(case):
    seq = case["sequences"]
    expected = case["expectedLastLogSeq"]
    gaps = expected is not None and set(seq) != set(range(1, expected + 1))
    known_loss = (case["debugCapture"] == "interrupted" or case["accessCapture"] == "interrupted" or
                  (case["droppedForSpan"] or 0) > 0 or (case["truncatedEvents"] or 0) > 0 or
                  case["terminalStubCount"] > 0 or case["conflict"] or gaps or case["exportKnownLoss"])
    if known_loss:
        coverage = "partial"
    elif (case["terminalCount"] != 1 or expected is None or case["terminalLogSeq"] != expected or
          case["debugCapture"] == "unknown" or case["accessCapture"] in ("none", "unknown") or
          case["droppedForSpan"] is None or case["truncatedEvents"] is None):
        coverage = "unknown"
    elif case["debugCapture"] == "none":
        coverage = "none"
    elif case["accessCapture"] != "enabled_throughout":
        coverage = "unknown"
    else:
        coverage = "full"
    return {"debugCoverage": coverage, "terminalMissing": case["terminalCount"] + case["terminalStubCount"] == 0}


def mapping_oracle(case):
    legacy = case["legacy"]
    state = "unknown"
    if legacy.get("deliveryOutcome") == "success" and case["localFinishObserved"]:
        state = "local_finished"
    elif case.get("localCancelObserved"):
        state = "cancelled"
    elif case.get("localFailureObserved"):
        state = "failed"
    timing = {key: case.get("monotonicMeasurements", {}).get(key) for key in
              ("firstUpstreamByteMs", "firstEffectiveOutputMs", "responseCommitMs", "firstDownstreamEffectiveOutputMs")}
    timing["timingSource"] = "server_monotonic"
    return {"requestId": legacy.get("request_id", legacy.get("requestId")),
            "attemptId": legacy.get("request_attempt_id", legacy.get("attemptId")),
            "deliveryState": state, "logSeq": case["allocatedLogSeq"],
            "sinkDroppedTotal": legacy.get("logsDropped"), "droppedForSpan": None,
            "timing": timing, "legacyPreserved": legacy}


def semantic_issues(record):
    issues = []
    if record["spanKind"] == "server" and record["serverSpanId"] != record["spanId"]:
        issues.append("server_owner")
    if record["spanKind"] == "call" and record["parentSpanId"] != record["serverSpanId"]:
        issues.append("call_owner")
    if record["spanId"] is not None and record["spanId"] == record["parentSpanId"]:
        issues.append("self_parent")
    if record["event"] in ("diag.server", "diag.call") and record["data"]["coverage"]["expectedLastLogSeq"] != record["logSeq"]:
        issues.append("terminal_sequence")
    if record["spanKind"] == "server":
        if (record["contextSource"] == "accepted") != (record["parentSpanId"] is not None):
            issues.append("context_parent")
    if record["event"] == "upstream.attempt_finished" and record["data"]["resultClass"] == "success":
        data = record["data"]
        effective = sum(data["output"][k] or 0 for k in ("ordinaryTextNonWhitespaceChars", "validToolCalls", "mediaParts"))
        if not data["terminalSeen"] or not data["eofSeen"] or not data["parserFinishOk"] or not effective:
            issues.append("success_evidence")
    return sorted(issues)


def manifest_bytes():
    files = sorted((path for path in ROOT.rglob("*") if path.is_file() and
                    path.name != "SHA256SUMS" and "__pycache__" not in path.parts and path.suffix != ".pyc"),
                   key=lambda path: path.relative_to(ROOT).as_posix())
    result = []
    for path in files:
        data = path.read_bytes()
        require(not data.startswith(b"\xef\xbb\xbf") and b"\r" not in data and data.endswith(b"\n"), f"Noncanonical text: {path.relative_to(ROOT)}")
        data.decode("utf-8")
        result.append(f"{hashlib.sha256(data).hexdigest()}  {path.relative_to(ROOT).as_posix()}\n")
    return "".join(result).encode("utf-8")


def validate():
    schemas = list(ROOT.glob("*.schema.json"))
    for path in schemas:
        Draft202012Validator.check_schema(read_json(path))
    schema_cases = read_json(ROOT / "fixtures/schema-cases.json")
    for case in schema_cases:
        validator = Draft202012Validator(read_json(ROOT / case["schema"]), format_checker=FORMATS)
        require(validator.is_valid(case["input"]) == case["valid"], f"Schema case failed: {case['id']}")
    index = read_json(ROOT / "fixtures/index.json")
    count = 0
    for group in ("valid", "invalid"):
        actual = sorted(path.stem for path in (ROOT / "fixtures" / group).glob("*.json"))
        require(actual == index[group], f"Fixture index mismatch: {group}")
        for name in actual:
            record = read_json(ROOT / "fixtures" / group / (name + ".json"))
            errors = list(SCHEMA.iter_errors(record))
            require(bool(errors) == (group == "invalid"), f"Unexpected schema result: {group}/{name}")
            if group == "valid":
                require(not semantic_issues(record), f"Invalid semantic fixture: {name}")
            count += 1
    example_lines = 0
    for path in (ROOT / "examples").glob("*.jsonl"):
        for line in path.read_text(encoding="utf-8").splitlines():
            record = json.loads(line)
            SCHEMA.validate(record)
            require(not semantic_issues(record), f"Invalid example semantics: {path.name}")
            require(len((line + "\n").encode("utf-8")) <= 4096, f"Oversized example: {path.name}")
            example_lines += 1
    bundle_path = ROOT / "examples/bundle.json"
    if bundle_path.exists():
        bundle = read_json(bundle_path)
        Draft202012Validator(read_json(ROOT / "bundle.schema.json"), format_checker=FORMATS).validate(bundle)
        for entry in bundle["files"]:
            require(hashlib.sha256((ROOT / "examples" / entry["path"]).read_bytes()).hexdigest() == entry["sha256"], "Bundle file digest mismatch")
    oracles = {"headers": header_oracle, "outbound": outbound_oracle, "peer-response": peer_response_oracle, "peers": peers_oracle,
               "source-scope": source_oracle, "graph": graph_oracle, "coverage": coverage_oracle,
               "aito-mapping": mapping_oracle, "resources": resource_oracle, "counts": counts_oracle,
               "semantic": lambda case: semantic_issues(case["record"]), "copy-source": copy_source_oracle,
               "redirects": redirect_oracle, "http-ingress": http_ingress_oracle}
    vector_count = 0
    for name, oracle in oracles.items():
        vectors = read_json(ROOT / "vectors" / (name + ".json"))
        seen = set()
        for vector in vectors:
            require(vector["id"] not in seen, f"Duplicate vector ID: {name}")
            seen.add(vector["id"])
            if name == "headers":
                require(vector["boundary"] in ("framework_fields", "parser_unit"), "Missing header input boundary")
            if name == "peers":
                require(vector["boundary"] == "final_request_target", "Missing final URL input boundary")
            if name == "semantic":
                SCHEMA.validate(vector["input"]["record"])
            actual = oracle(vector["input"])
            require(actual == vector["expected"], f"Vector mismatch {name}/{vector['id']}: expected {vector['expected']!r}, got {actual!r}")
            vector_count += 1
    return len(schemas), count + len(schema_cases), example_lines, vector_count


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--write-manifest", action="store_true")
    args = parser.parse_args()
    schemas, fixtures, lines, vectors = validate()
    data = manifest_bytes()
    manifest = ROOT / "SHA256SUMS"
    if args.write_manifest:
        manifest.write_bytes(data)
    require(manifest.read_bytes() == data, "SHA256SUMS differs; inspect changes before --write-manifest")
    print(f"PASS: {schemas} schemas, {fixtures} fixtures, {lines} example lines, {vectors} vectors")
    print(f"Contract SHA-256: {hashlib.sha256(data).hexdigest()}")


if __name__ == "__main__":
    main()

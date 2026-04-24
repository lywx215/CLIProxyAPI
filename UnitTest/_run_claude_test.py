import urllib.request, json, time, sys

BASE = "https://ultra-cli.zeabur.app"
HEADERS = {
    "Content-Type": "application/json",
    "x-api-key": "gemini123!!!",
    "anthropic-version": "2023-06-01"
}

def test(model, extra=None):
    url = BASE + "/v1/messages"
    payload = {
        "model": model,
        "max_tokens": 800,
        "messages": [{"role": "user", "content": "What is 2+2? Think step by step and explain your reasoning."}]
    }
    if extra:
        payload.update(extra)
    body = json.dumps(payload).encode()
    req = urllib.request.Request(url, data=body, headers=HEADERS)
    print("\n" + "="*60)
    print(f"Model: {model}")
    print("="*60)
    try:
        resp = json.loads(urllib.request.urlopen(req, timeout=90).read().decode())
        print(f"[OK] stop_reason={resp.get('stop_reason')}")
        for block in resp.get("content", []):
            btype = block.get("type", "")
            if btype == "thinking":
                thinking_text = block.get("thinking", "")
                print(f"  [thinking] length={len(thinking_text)} chars")
                print(f"  preview: {thinking_text[:300]}...")
            elif btype == "text":
                print(f"  [text]: {block.get('text', '')[:400]}")
        usage = resp.get("usage", {})
        print(f"  usage: input={usage.get('input_tokens')} output={usage.get('output_tokens')}")
        return True
    except urllib.error.HTTPError as e:
        err = e.read().decode()[:400]
        print(f"[FAIL] HTTP {e.code}: {err}")
        return False
    except Exception as ex:
        print(f"[FAIL] Error: {ex}")
        return False

# Test 1: standard claude-opus-4-6
test("claude-opus-4-6")

time.sleep(2)

# Test 2: claude-opus-4-6-thinking with extended thinking enabled
test("claude-opus-4-6-thinking", {
    "thinking": {"type": "enabled", "budget_tokens": 400}
})

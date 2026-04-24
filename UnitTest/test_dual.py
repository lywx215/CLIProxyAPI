import urllib.request, json, sys, io
sys.stdout = io.TextIOWrapper(sys.stdout.buffer, encoding='utf-8')

# Test 1: Claude format - claude-opus-4-6-thinking
print("=" * 60)
print("Test 1: Claude format - claude-opus-4-6-thinking")
print("=" * 60)

url = "http://localhost:8317/v1/messages"
body = json.dumps({
    "model": "claude-opus-4-6-thinking",
    "max_tokens": 200,
    "messages": [{"role": "user", "content": "What is 2+2? Answer in one word."}]
}).encode()
req = urllib.request.Request(url, data=body, headers={
    "Content-Type": "application/json",
    "x-api-key": "gemini123!!!",
    "anthropic-version": "2023-06-01"
})
try:
    resp = json.loads(urllib.request.urlopen(req, timeout=60).read().decode())
    print(json.dumps(resp, indent=2, ensure_ascii=False)[:800])
except urllib.error.HTTPError as e:
    print(f"HTTP {e.code}: {e.read().decode()[:300]}")
except Exception as e:
    print(f"Error: {e}")

print()

# Test 2: Gemini format - gemini-3.1-flash-image
print("=" * 60)
print("Test 2: Gemini format - gemini-3.1-flash-image")
print("=" * 60)

import base64

url2 = "http://localhost:8317/v1beta/models/gemini-3.1-flash-image:generateContent"
body2 = json.dumps({
    "contents": [{"parts": [{"text": "Generate a simple cute cat emoji-style drawing"}]}],
    "generationConfig": {"responseModalities": ["IMAGE", "TEXT"]}
}).encode()
req2 = urllib.request.Request(url2, data=body2, headers={
    "Content-Type": "application/json",
    "x-goog-api-key": "gemini123!!!"
})
try:
    resp2 = json.loads(urllib.request.urlopen(req2, timeout=60).read().decode())
    parts = resp2["candidates"][0]["content"]["parts"]
    for i, p in enumerate(parts):
        if "inlineData" in p:
            mime = p["inlineData"]["mimeType"]
            data_len = len(p["inlineData"]["data"])
            img_bytes = base64.b64decode(p["inlineData"]["data"])
            ext = mime.split("/")[-1]
            fname = f"test_cat.{ext}"
            with open(fname, "wb") as f:
                f.write(img_bytes)
            print(f"Part {i}: IMAGE | {mime} | base64={data_len} chars | saved {fname} ({len(img_bytes)} bytes)")
        elif "text" in p:
            print(f"Part {i}: TEXT  | {p['text'].strip()[:100]}")
    usage = resp2.get("usageMetadata", {})
    print(f"Usage: {json.dumps(usage)}")
except urllib.error.HTTPError as e:
    print(f"HTTP {e.code}: {e.read().decode()[:300]}")
except Exception as e:
    print(f"Error: {e}")

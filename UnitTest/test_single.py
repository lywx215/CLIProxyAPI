import urllib.request, json, sys, io
sys.stdout = io.TextIOWrapper(sys.stdout.buffer, encoding='utf-8')

url = "http://localhost:8317/v1beta/models/gemini-3-pro-high:generateContent"
body = json.dumps({"contents": [{"parts": [{"text": "What is 2+2? Answer in one word."}]}]}).encode()
req = urllib.request.Request(url, data=body, headers={
    "Content-Type": "application/json",
    "x-goog-api-key": "gemini123!!!"
})
try:
    resp = urllib.request.urlopen(req, timeout=30).read().decode()
    data = json.loads(resp)
    text = data["candidates"][0]["content"]["parts"][-1].get("text", "")
    usage = data.get("usageMetadata", {})
    print(f"Model: gemini-3-pro-high")
    print(f"Answer: {text.strip()}")
    print(f"Usage: {json.dumps(usage)}")
except urllib.error.HTTPError as e:
    print(f"HTTP {e.code}: {e.read().decode()[:300]}")

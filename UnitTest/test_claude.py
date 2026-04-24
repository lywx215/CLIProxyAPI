import urllib.request, json, sys, io
sys.stdout = io.TextIOWrapper(sys.stdout.buffer, encoding='utf-8')

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
    print(json.dumps(resp, indent=2, ensure_ascii=False)[:1000])
except urllib.error.HTTPError as e:
    print(f"HTTP {e.code}: {e.read().decode()[:400]}")
except Exception as e:
    print(f"Error: {e}")

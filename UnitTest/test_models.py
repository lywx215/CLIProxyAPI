import urllib.request, json, sys, time, io

sys.stdout = io.TextIOWrapper(sys.stdout.buffer, encoding='utf-8')

API_URL = "http://localhost:8317/v1beta/models/{model}:generateContent"
API_KEY = "gemini123!!!"
PROMPT = "What is 2+2? Answer in one word."

models = [
    "gemini-2.5-flash",
    "gemini-2.5-pro",
    "gemini-3-pro-preview",
    "gemini-3-flash-preview",
    "gemini-3.1-pro-preview",
]

for model in models:
    url = API_URL.format(model=model)
    body = json.dumps({
        "contents": [{"parts": [{"text": PROMPT}]}]
    }).encode()
    req = urllib.request.Request(url, data=body, headers={
        "Content-Type": "application/json",
        "x-goog-api-key": API_KEY,
    })
    t0 = time.time()
    try:
        resp_raw = urllib.request.urlopen(req, timeout=30).read().decode()
        resp = json.loads(resp_raw)
        elapsed = time.time() - t0
        text = resp["candidates"][0]["content"]["parts"][-1].get("text", "(no text)")
        usage = resp.get("usageMetadata", {})
        total = usage.get("totalTokenCount", "?")
        print(f"OK  {model:30s} | {elapsed:5.1f}s | tokens={total:>4} | {text.strip()[:60]}")
    except urllib.error.HTTPError as e:
        elapsed = time.time() - t0
        body_err = e.read().decode()[:100] if hasattr(e, 'read') else ""
        print(f"ERR {model:30s} | {elapsed:5.1f}s | HTTP {e.code} | {body_err}")
    except Exception as e:
        elapsed = time.time() - t0
        print(f"ERR {model:30s} | {elapsed:5.1f}s | {type(e).__name__}: {e}")

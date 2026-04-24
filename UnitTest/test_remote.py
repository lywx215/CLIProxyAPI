import urllib.request, json, sys, io, time
sys.stdout = io.TextIOWrapper(sys.stdout.buffer, encoding='utf-8')

BASE_URL = "https://ultra-cli3.zeabur.app"
API_KEY = "gemini123!!!"
MODEL = "gemini-3-flash-preview"
ROUNDS = 10

print(f"Testing {BASE_URL} | model={MODEL} | rounds={ROUNDS}")
print("=" * 70)
print(f"{'#':>3}  {'Status':>6}  {'Time':>6}  {'Tokens':>6}  Answer")
print("-" * 70)

ok_count = 0
total_time = 0

for i in range(1, ROUNDS + 1):
    url = f"{BASE_URL}/v1beta/models/{MODEL}:generateContent"
    body = json.dumps({
        "contents": [{"parts": [{"text": "What is 2+2? Answer in one word."}]}]
    }).encode()
    req = urllib.request.Request(url, data=body, headers={
        "Content-Type": "application/json",
        "x-goog-api-key": API_KEY,
    })
    t0 = time.time()
    try:
        resp = json.loads(urllib.request.urlopen(req, timeout=30).read().decode())
        elapsed = time.time() - t0
        text = resp["candidates"][0]["content"]["parts"][-1].get("text", "").strip()
        total_tokens = resp.get("usageMetadata", {}).get("totalTokenCount", "?")
        print(f"{i:>3}  {'OK':>6}  {elapsed:5.1f}s  {total_tokens:>6}  {text[:40]}")
        ok_count += 1
        total_time += elapsed
    except urllib.error.HTTPError as e:
        elapsed = time.time() - t0
        total_time += elapsed
        err_body = e.read().decode()[:80]
        print(f"{i:>3}  {'E'+str(e.code):>6}  {elapsed:5.1f}s  {'--':>6}  {err_body}")
    except Exception as e:
        elapsed = time.time() - t0
        total_time += elapsed
        print(f"{i:>3}  {'ERR':>6}  {elapsed:5.1f}s  {'--':>6}  {type(e).__name__}")

print("-" * 70)
print(f"Result: {ok_count}/{ROUNDS} OK | avg={total_time/ROUNDS:.1f}s | total={total_time:.1f}s")

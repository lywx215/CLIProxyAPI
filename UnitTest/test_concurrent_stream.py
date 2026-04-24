import urllib.request, json, sys, io, time, threading
sys.stdout = io.TextIOWrapper(sys.stdout.buffer, encoding='utf-8')

BASE_URL = "https://ultra-cli3.zeabur.app"
API_KEY = "gemini123!!!"
MODEL = "gemini-3.1-pro-high"
CONCURRENCY = 5

PROMPT = "Write a 500-word essay about the future of artificial intelligence."

results = [None] * CONCURRENCY
lock = threading.Lock()

def stream_request(idx):
    url = f"{BASE_URL}/v1beta/models/{MODEL}:streamGenerateContent?alt=sse"
    body = json.dumps({
        "contents": [{"parts": [{"text": PROMPT}]}],
    }).encode()
    req = urllib.request.Request(url, data=body, headers={
        "Content-Type": "application/json",
        "x-goog-api-key": API_KEY,
    })

    t0 = time.time()
    try:
        resp = urllib.request.urlopen(req, timeout=120)
        chunk_count = 0
        total_text_len = 0
        first_byte_time = None
        last_usage = None

        for line in resp:
            line = line.decode("utf-8").strip()
            if not line or not line.startswith("data: "):
                continue
            if first_byte_time is None:
                first_byte_time = time.time() - t0
            data_str = line[6:]
            try:
                data = json.loads(data_str)
            except json.JSONDecodeError:
                continue
            chunk_count += 1
            parts = data.get("candidates", [{}])[0].get("content", {}).get("parts", [])
            for p in parts:
                if "text" in p:
                    total_text_len += len(p["text"])
            usage = data.get("usageMetadata")
            if usage and usage.get("totalTokenCount"):
                last_usage = usage

        elapsed = time.time() - t0
        output_t = last_usage.get('candidatesTokenCount', 0) if last_usage else 0
        total_t = last_usage.get('totalTokenCount', 0) if last_usage else 0
        think_t = last_usage.get('thoughtsTokenCount', 0) if last_usage else 0
        results[idx] = {
            "status": "OK", "elapsed": elapsed, "ttfb": first_byte_time,
            "chunks": chunk_count, "chars": total_text_len,
            "output_tokens": output_t, "think_tokens": think_t, "total_tokens": total_t,
        }
        with lock:
            print(f"  [T{idx+1}] OK  | {elapsed:5.1f}s | ttfb={first_byte_time:.1f}s | chunks={chunk_count} | output={output_t} tok | total={total_t} tok")

    except urllib.error.HTTPError as e:
        elapsed = time.time() - t0
        err_body = e.read().decode()[:120]
        results[idx] = {"status": f"E{e.code}", "elapsed": elapsed, "error": err_body}
        with lock:
            print(f"  [T{idx+1}] E{e.code} | {elapsed:5.1f}s | {err_body[:80]}")

    except Exception as e:
        elapsed = time.time() - t0
        results[idx] = {"status": "ERR", "elapsed": elapsed, "error": str(e)}
        with lock:
            print(f"  [T{idx+1}] ERR | {elapsed:5.1f}s | {type(e).__name__}: {e}")


print(f"Concurrent streaming test: {BASE_URL}")
print(f"Model: {MODEL} | Concurrency: {CONCURRENCY}")
print("=" * 80)

t_start = time.time()
threads = []
for i in range(CONCURRENCY):
    t = threading.Thread(target=stream_request, args=(i,))
    threads.append(t)
    t.start()

for t in threads:
    t.join()

t_total = time.time() - t_start
print("=" * 80)

ok = sum(1 for r in results if r and r["status"] == "OK")
fail = CONCURRENCY - ok
print(f"Results:    {ok}/{CONCURRENCY} OK, {fail} failed")
print(f"Wall time:  {t_total:.1f}s")

if ok > 0:
    ok_results = [r for r in results if r and r["status"] == "OK"]
    avg_elapsed = sum(r["elapsed"] for r in ok_results) / len(ok_results)
    avg_ttfb = sum(r["ttfb"] for r in ok_results) / len(ok_results)
    total_output = sum(r["output_tokens"] for r in ok_results)
    print(f"Avg time:   {avg_elapsed:.1f}s")
    print(f"Avg TTFB:   {avg_ttfb:.1f}s")
    print(f"Total output tokens: {total_output}")

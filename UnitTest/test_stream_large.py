import urllib.request, json, sys, io, time
sys.stdout = io.TextIOWrapper(sys.stdout.buffer, encoding='utf-8')

BASE_URL = "https://ultra-cli3.zeabur.app"
API_KEY = "gemini123!!!"
MODEL = "gemini-3.1-pro-high"

PROMPT = """Write a detailed technical tutorial about building a REST API with Go (Golang).
Cover the following topics in depth with code examples:
1. Project setup and directory structure
2. Setting up the HTTP router with gin
3. Creating CRUD endpoints for a "User" resource
4. Adding middleware for logging and authentication
5. Database integration with PostgreSQL
6. Error handling best practices
7. Testing the API endpoints
8. Deploying to production

Please write at least 2000 words with complete, runnable code examples for each section."""

print(f"Large token streaming test: {BASE_URL} | model={MODEL}")
print("=" * 70)

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
    resp = urllib.request.urlopen(req, timeout=180)
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
                # Print progress every 10 chunks
                if chunk_count % 10 == 0:
                    elapsed = time.time() - t0
                    print(f"  [chunk {chunk_count:>4}] chars={total_text_len:>6} | {elapsed:.1f}s", flush=True)

        usage = data.get("usageMetadata")
        if usage and usage.get("totalTokenCount"):
            last_usage = usage

    elapsed = time.time() - t0
    print()
    print("=" * 70)
    print(f"Chunks:       {chunk_count}")
    print(f"Text length:  {total_text_len} chars")
    print(f"TTFB:         {first_byte_time:.2f}s" if first_byte_time else "TTFB: N/A")
    print(f"Total time:   {elapsed:.2f}s")
    if last_usage:
        prompt_t = last_usage.get('promptTokenCount', '?')
        output_t = last_usage.get('candidatesTokenCount', '?')
        think_t = last_usage.get('thoughtsTokenCount', '?')
        total_t = last_usage.get('totalTokenCount', '?')
        print(f"Tokens:       prompt={prompt_t}, output={output_t}, thoughts={think_t}, total={total_t}")
        if isinstance(output_t, int) and elapsed > 0 and first_byte_time:
            gen_time = elapsed - first_byte_time
            tps = output_t / gen_time if gen_time > 0 else 0
            print(f"Output speed: {tps:.1f} tokens/s")

except urllib.error.HTTPError as e:
    elapsed = time.time() - t0
    print(f"HTTP {e.code} ({elapsed:.1f}s): {e.read().decode()[:300]}")
except Exception as e:
    elapsed = time.time() - t0
    print(f"Error ({elapsed:.1f}s): {e}")

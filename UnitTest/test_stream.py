import urllib.request, json, sys, io, time
sys.stdout = io.TextIOWrapper(sys.stdout.buffer, encoding='utf-8')

BASE_URL = "https://ultra-cli3.zeabur.app"
API_KEY = "gemini123!!!"
MODEL = "gemini-3.1-pro-high"

print(f"Streaming test: {BASE_URL} | model={MODEL}")
print("=" * 70)

url = f"{BASE_URL}/v1beta/models/{MODEL}:streamGenerateContent?alt=sse"
body = json.dumps({
    "contents": [{"parts": [{"text": "Write a haiku about coding."}]}]
}).encode()
req = urllib.request.Request(url, data=body, headers={
    "Content-Type": "application/json",
    "x-goog-api-key": API_KEY,
})

t0 = time.time()
try:
    resp = urllib.request.urlopen(req, timeout=60)
    chunk_count = 0
    full_text = ""
    first_byte_time = None

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
                full_text += p["text"]
                print(p["text"], end="", flush=True)

        # Print usage if present (last chunk)
        usage = data.get("usageMetadata")
        if usage and usage.get("totalTokenCount"):
            print()
            print("-" * 70)
            print(f"Usage: prompt={usage.get('promptTokenCount','?')}, "
                  f"output={usage.get('candidatesTokenCount','?')}, "
                  f"thoughts={usage.get('thoughtsTokenCount','?')}, "
                  f"total={usage.get('totalTokenCount','?')}")

    elapsed = time.time() - t0
    print(f"\nChunks:     {chunk_count}")
    print(f"TTFB:       {first_byte_time:.2f}s" if first_byte_time else "TTFB: N/A")
    print(f"Total time: {elapsed:.2f}s")

except urllib.error.HTTPError as e:
    elapsed = time.time() - t0
    print(f"HTTP {e.code} ({elapsed:.1f}s): {e.read().decode()[:300]}")
except Exception as e:
    elapsed = time.time() - t0
    print(f"Error ({elapsed:.1f}s): {e}")

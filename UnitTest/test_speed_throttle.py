import requests
import time
import json
import statistics
import concurrent.futures

BASE_URL = "http://149.248.20.234:8317"
API_KEY = "gemini123!!!"

HEADERS = {
    "Authorization": f"Bearer {API_KEY}",
    "Content-Type": "application/json"
}

LONG_INPUT = "The following is a detailed context for the essay: " + "history AI " * 600

def build_payload(thinking=False):
    payload = {
        "contents": [
            {"role": "user", "parts": [{"text": LONG_INPUT + "\n\nBased on the above context and your knowledge, please write a comprehensive and extremely detailed 2000-word essay about the history, evolution, and future of artificial intelligence. It is critical that your response is very long, at least 2000 words, to ensure we generate more than 1500 tokens."}]}
        ]
    }
    if thinking:
        payload["generationConfig"] = {
            "thinkingConfig": {
                "thinkingBudget": 64000
            }
        }
    return payload

def run_single_non_streaming(req_id, model, use_thinking=False):
    url = f"{BASE_URL}/v1beta/models/{model}:generateContent"
    start_time = time.time()
    try:
        response = requests.post(url, headers=HEADERS, json=build_payload(use_thinking), timeout=300)
        end_time = time.time()
        elapsed = end_time - start_time
        
        if response.status_code != 200:
            return {"id": req_id, "error": f"HTTP {response.status_code}: {response.text[:100]}"}
            
        data = response.json()
        tokens = 0
        input_tokens = 0
        thoughts = 0
        model_version = data.get("modelVersion", "Unknown")
        if "usageMetadata" in data:
            tokens = data["usageMetadata"].get("candidatesTokenCount", 0)
            input_tokens = data["usageMetadata"].get("promptTokenCount", 0)
            thoughts = data["usageMetadata"].get("thoughtsTokenCount", 0)
            
        rate = tokens / elapsed if elapsed > 0 else 0
        return {
            "id": req_id,
            "elapsed": elapsed,
            "tokens": tokens,
            "input_tokens": input_tokens,
            "thoughts": thoughts,
            "rate": rate,
            "model_version": model_version,
            "error": None
        }
    except Exception as e:
        return {"id": req_id, "error": str(e)}

def test_non_streaming_concurrent(model, concurrency=10, use_thinking=False):
    title = f"Testing Non-Streaming (Model: {model}, Thinking: {use_thinking}, Concurrency: {concurrency})"
    print("\n" + "="*80)
    print(f"  {title}")
    print("="*80)
    
    results = []
    with concurrent.futures.ThreadPoolExecutor(max_workers=concurrency) as executor:
        futures = {executor.submit(run_single_non_streaming, i+1, model, use_thinking): i+1 for i in range(concurrency)}
        for future in concurrent.futures.as_completed(futures):
            req_id = futures[future]
            res = future.result()
            if res.get("error"):
                print(f"[Req {req_id}] Error: {res['error']}")
            else:
                print(f"[Req {req_id}] Model: {res['model_version']} | Elapsed: {res['elapsed']:.2f}s | In: {res['input_tokens']} | Out: {res['tokens']} | Thoughts: {res['thoughts']} | Rate: {res['rate']:.2f} t/s")
                results.append(res)
    return results

def run_single_streaming(req_id, model, use_thinking=False):
    url = f"{BASE_URL}/v1beta/models/{model}:streamGenerateContent?alt=sse"
    start_time = time.time()
    ttft = 0
    first_chunk_time = 0
    tokens = 0
    input_tokens = 0
    thoughts = 0
    model_version = "Unknown"
    
    try:
        with requests.post(url, headers=HEADERS, json=build_payload(use_thinking), timeout=300, stream=True) as response:
            if response.status_code != 200:
                return {"id": req_id, "error": f"HTTP {response.status_code}: {response.text[:100]}"}
            
            for line in response.iter_lines():
                if not line:
                    continue
                
                line = line.decode('utf-8')
                if line.startswith("data: "):
                    data_str = line[6:]
                    if data_str == "[DONE]":
                        break
                        
                    if first_chunk_time == 0:
                        first_chunk_time = time.time()
                        ttft = first_chunk_time - start_time
                        
                    try:
                        data = json.loads(data_str)
                        if "modelVersion" in data:
                            model_version = data["modelVersion"]
                        if "usageMetadata" in data:
                            if "candidatesTokenCount" in data["usageMetadata"]:
                                tokens = data["usageMetadata"]["candidatesTokenCount"]
                            if "promptTokenCount" in data["usageMetadata"]:
                                input_tokens = data["usageMetadata"]["promptTokenCount"]
                            if "thoughtsTokenCount" in data["usageMetadata"]:
                                thoughts = data["usageMetadata"]["thoughtsTokenCount"]
                    except json.JSONDecodeError:
                        pass
                        
        end_time = time.time()
        elapsed = end_time - start_time
        gen_time = end_time - first_chunk_time
        rate = tokens / gen_time if gen_time > 0 and tokens > 0 else 0
        
        return {
            "id": req_id,
            "ttft": ttft,
            "elapsed": elapsed,
            "gen_time": gen_time,
            "tokens": tokens,
            "input_tokens": input_tokens,
            "thoughts": thoughts,
            "rate": rate,
            "model_version": model_version,
            "error": None
        }
    except Exception as e:
        return {"id": req_id, "error": str(e)}

def test_streaming_concurrent(model, concurrency=10, use_thinking=False):
    title = f"Testing Streaming (Model: {model}, Thinking: {use_thinking}, Concurrency: {concurrency})"
    print("\n" + "="*80)
    print(f"  {title}")
    print("="*80)
    
    results = []
    with concurrent.futures.ThreadPoolExecutor(max_workers=concurrency) as executor:
        futures = {executor.submit(run_single_streaming, i+1, model, use_thinking): i+1 for i in range(concurrency)}
        for future in concurrent.futures.as_completed(futures):
            req_id = futures[future]
            res = future.result()
            if res.get("error"):
                print(f"[Req {req_id}] Error: {res['error']}")
            else:
                print(f"[Req {req_id}] Model: {res['model_version']} | TTFT: {res['ttft']:.2f}s | Gen: {res['gen_time']:.2f}s | In: {res['input_tokens']} | Out: {res['tokens']} | Thoughts: {res['thoughts']} | Rate: {res['rate']:.2f} t/s")
                results.append(res)
    return results

def print_summary(name, results, is_stream):
    if not results:
        return
    print(f"\n{name}:")
    rates = [r["rate"] for r in results if r["rate"] > 0]
    out_tokens = [r["tokens"] for r in results]
    in_tokens = [r["input_tokens"] for r in results]
    thoughts = [r["thoughts"] for r in results]
    models_returned = list(set([r["model_version"] for r in results if "model_version" in r]))
    
    if is_stream:
        ttfts = [r["ttft"] for r in results]
        print(f"  TTFT           : {min(ttfts):.2f}s - {max(ttfts):.2f}s (Avg: {statistics.mean(ttfts):.2f}s)")
    else:
        elapsed = [r["elapsed"] for r in results]
        print(f"  Total Elapsed  : {min(elapsed):.2f}s - {max(elapsed):.2f}s (Avg: {statistics.mean(elapsed):.2f}s)")
        
    if rates:
        print(f"  Tokens/s       : {min(rates):.2f} - {max(rates):.2f} (Avg: {statistics.mean(rates):.2f})")
    if out_tokens:
        print(f"  Output Tokens  : {min(out_tokens)} - {max(out_tokens)} (Avg: {statistics.mean(out_tokens):.0f})")
    if in_tokens:
        print(f"  Input Tokens   : {min(in_tokens)} - {max(in_tokens)} (Avg: {statistics.mean(in_tokens):.0f})")
    if sum(thoughts) > 0:
        print(f"  Thoughts Tokens: {min(thoughts)} - {max(thoughts)} (Avg: {statistics.mean(thoughts):.0f})")
    print(f"  Models Returned: {', '.join(models_returned)}")

def main():
    print("Starting Multi-Model Speed Throttle CONCURRENT Validation Test...")
    
    # 1. Test gemini-2.5-pro (No thinking)
    print("\n" + "#"*80)
    print("  Testing gemini-2.5-pro")
    print("#"*80)
    s25 = test_streaming_concurrent("gemini-2.5-pro", 10, False)
    ns25 = test_non_streaming_concurrent("gemini-2.5-pro", 10, False)
    
    # 2. Test gemini-3.1-pro-preview (Normal)
    print("\n" + "#"*80)
    print("  Testing gemini-3.1-pro-preview (Normal)")
    print("#"*80)
    s31 = test_streaming_concurrent("gemini-3.1-pro-preview", 10, False)
    ns31 = test_non_streaming_concurrent("gemini-3.1-pro-preview", 10, False)

    # 3. Test gemini-3.1-pro-preview (Max Thinking)
    print("\n" + "#"*80)
    print("  Testing gemini-3.1-pro-preview (Max Thinking)")
    print("#"*80)
    s31_think = test_streaming_concurrent("gemini-3.1-pro-preview", 5, True) # lower concurrency for heavy thinking test
    ns31_think = test_non_streaming_concurrent("gemini-3.1-pro-preview", 5, True)
    
    print("\n" + "="*80)
    print("  FINAL SUMMARY")
    print("="*80)
    
    print_summary("gemini-2.5-pro Streaming", s25, True)
    print_summary("gemini-2.5-pro Non-Streaming", ns25, False)
    print_summary("gemini-3.1-pro-preview Normal Streaming", s31, True)
    print_summary("gemini-3.1-pro-preview Normal Non-Streaming", ns31, False)
    print_summary("gemini-3.1-pro-preview Thinking Streaming", s31_think, True)
    print_summary("gemini-3.1-pro-preview Thinking Non-Streaming", ns31_think, False)

if __name__ == "__main__":
    main()

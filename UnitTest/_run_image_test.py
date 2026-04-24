import urllib.request, json, base64, sys

MODELS_TO_TRY = [
    "gemini-3.1-flash-image-preview",
    "gemini-3.1-flash-image",
]
BASE_URL = "https://ultra-cli.zeabur.app"

def test_model(model_id):
    url = f"{BASE_URL}/v1beta/models/{model_id}:generateContent"
    print(f"\n{'='*60}")
    print(f"Testing model: {model_id}")
    print(f"URL: {url}")
    print('='*60)
    body = json.dumps({
        "contents": [{"parts": [{"text": "Draw a cute cat in a simple style. Just respond with the image."}]}],
        "generationConfig": {"responseModalities": ["IMAGE", "TEXT"]}
    }).encode()
    req = urllib.request.Request(url, data=body, headers={
        "Content-Type": "application/json",
        "x-goog-api-key": "gemini123!!!"
    })
    try:
        resp_raw = urllib.request.urlopen(req, timeout=120).read().decode()
        resp = json.loads(resp_raw)
        parts = resp["candidates"][0]["content"]["parts"]
        for i, p in enumerate(parts):
            if "inlineData" in p:
                mime = p["inlineData"]["mimeType"]
                data_len = len(p["inlineData"]["data"])
                print(f"Part {i}: IMAGE, mimeType={mime}, base64_len={data_len}")
                img_data = base64.b64decode(p["inlineData"]["data"])
                ext = mime.split("/")[-1]
                safe_name = model_id.replace("/", "_").replace(":", "_")
                out_path = f"test_output_{safe_name}.{ext}"
                with open(out_path, "wb") as f:
                    f.write(img_data)
                print(f"  -> Saved to {out_path} ({len(img_data)} bytes)")
            elif "text" in p:
                text_val = p["text"]
                print(f"Part {i}: TEXT = {text_val}")
        print("Usage:", json.dumps(resp.get("usageMetadata", {})))
        return True
    except urllib.error.HTTPError as e:
        body_err = e.read().decode()
        print(f"HTTP Error {e.code}: {body_err}", file=sys.stderr)
        return False
    except Exception as ex:
        print(f"Error: {ex}", file=sys.stderr)
        return False

for model in MODELS_TO_TRY:
    ok = test_model(model)
    if ok:
        break

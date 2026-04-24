import urllib.request, json, base64

url = "http://localhost:8317/v1beta/models/gemini-3.1-flash-image:generateContent"
body = json.dumps({
    "contents": [{"parts": [{"text": "Draw a cute cat in a simple style. Just respond with the image."}]}],
    "generationConfig": {"responseModalities": ["IMAGE", "TEXT"]}
}).encode()

req = urllib.request.Request(url, data=body, headers={
    "Content-Type": "application/json",
    "x-goog-api-key": "gemini123!!!"
})

resp = json.loads(urllib.request.urlopen(req, timeout=60).read().decode())
parts = resp["candidates"][0]["content"]["parts"]

for i, p in enumerate(parts):
    if "inlineData" in p:
        mime = p["inlineData"]["mimeType"]
        data_len = len(p["inlineData"]["data"])
        print(f"Part {i}: IMAGE, mimeType={mime}, base64_len={data_len}")
        # Save image
        img_data = base64.b64decode(p["inlineData"]["data"])
        ext = mime.split("/")[-1]
        with open(f"test_output.{ext}", "wb") as f:
            f.write(img_data)
        print(f"  -> Saved to test_output.{ext} ({len(img_data)} bytes)")
    elif "text" in p:
        print(f"Part {i}: TEXT = {p['text']}")

print("Usage:", json.dumps(resp.get("usageMetadata", {})))

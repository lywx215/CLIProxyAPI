"""Exercise actual Python SDKs against the opt-in local Go fixture harness.

This command uses no Google credentials and performs no model generation calls.
Install openai and google-genai in an isolated environment before running it.
"""

import importlib.metadata
import json
import os
from pathlib import Path
import subprocess
import tempfile
import time
import urllib.request

import httpx
from google import genai
from google.genai import types
from openai import OpenAI


def main():
    repo = Path(__file__).resolve().parents[1]
    with tempfile.TemporaryDirectory(prefix="enterprise-sdk-") as scratch:
        ready = Path(scratch) / "ready.txt"
        env = dict(os.environ, CLIPROXY_ENTERPRISE_SDK_READY_FILE=str(ready))
        log_path = Path(scratch) / "go-test.log"
        with log_path.open("w+", encoding="utf-8") as log:
            process = subprocess.Popen(
                ["go", "test", "-count=1", "-timeout", "180s", "-run", "^TestEnterpriseSDKHarness$", "./test"],
                cwd=repo, env=env, stdout=log, stderr=subprocess.STDOUT,
                creationflags=subprocess.CREATE_NO_WINDOW if os.name == "nt" else 0,
            )
            base = None
            try:
                deadline = time.monotonic() + 150
                while not ready.exists():
                    if process.poll() is not None or time.monotonic() > deadline:
                        log.seek(0)
                        raise RuntimeError("local Go harness did not start:\n" + log.read())
                    time.sleep(0.1)
                base = ready.read_text(encoding="utf-8")
                results = []
                with OpenAI(api_key="offline-fixture-key", base_url=base + "/v1",
                            http_client=httpx.Client(trust_env=False)) as chat:
                    catalog = list(chat.models.list())
                    assert any(m.id == "gemini-4-argon" for m in catalog)
                    args = dict(model="gemini-4-argon", messages=[{"role": "user", "content": "offline fixture"}])
                    response = chat.chat.completions.create(**args, extra_body={"tools": [{"google_search": {}}]})
                    assert "offline" in response.choices[0].message.content
                    assert response.choices[0].message.annotations
                    assert response.usage.prompt_tokens > 0
                    assert response.usage.completion_tokens > 0
                    assert response.usage.total_tokens == response.usage.prompt_tokens + response.usage.completion_tokens
                    assert response.usage.prompt_tokens_details is None
                    assert response.usage.completion_tokens_details is None
                    results.append("OpenAI full + annotations + token usage")
                    chunks = list(chat.chat.completions.create(**args, stream=True))
                    assert chunks[0].choices[0].delta.content
                    assert "offline" in "".join(c.choices[0].delta.content or "" for c in chunks)
                    assert chunks[-1].choices[0].finish_reason == "stop"
                    results.append("OpenAI SSE + visible first chunk + terminal finish")
                    chunks = list(chat.chat.completions.create(**args, stream=True, stream_options={"include_usage": True}))
                    assert chunks[-1].choices == [] and chunks[-1].usage.total_tokens > 0
                    assert chunks[-2].choices[0].finish_reason == "stop"
                    results.append("OpenAI SSE include_usage tail")
                    response = chat.chat.completions.create(**args, response_format={"type": "json_object"})
                    assert json.loads(response.choices[0].message.content) == {"ok": True}
                    results.append("OpenAI JSON object validation")
                    chunks = list(chat.chat.completions.create(**args, stream=True, response_format={"type": "json_object"}))
                    assert json.loads(chunks[0].choices[0].delta.content) == {"ok": True}
                    assert chunks[-1].choices[0].finish_reason == "stop"
                    results.append("OpenAI JSON SSE buffering")
                    long_messages = [{"role": "user" if i % 2 == 0 else "assistant",
                        "content": f"history turn {i} start" + " " * 2200 + "end"} for i in range(514)]
                    assert len(json.dumps(long_messages).encode()) > 1024 * 1024
                    long_args = dict(model="gemini-4-argon", messages=long_messages)
                    response = chat.chat.completions.create(**long_args)
                    assert "offline" in response.choices[0].message.content
                    results.append("OpenAI >1 MiB 514-turn assistant continuation")
                    chunks = list(chat.chat.completions.create(**long_args, stream=True))
                    assert "offline" in "".join(c.choices[0].delta.content or "" for c in chunks)
                    assert chunks[-1].choices[0].finish_reason == "stop"
                    results.append("OpenAI >1 MiB assistant continuation SSE")
                    function_args = dict(model="gemini-4-argon", messages=[{"role": "user", "content": "offline fixture"}],
                        tools=[{"type": "function", "function": {"name": "unused_sdk_function", "parameters": {"type": "object", "properties": {}}}}],
                        tool_choice="required", parallel_tool_calls=True)
                    response = chat.chat.completions.create(**function_args)
                    assert "offline" in response.choices[0].message.content and not response.choices[0].message.tool_calls
                    results.append("OpenAI unsupported function declarations ignored as text")
                    function_args["messages"] = [{"role": "user", "content": "old question"},
                        {"role": "assistant", "content": None, "tool_calls": [{"id": "sdk-call-17", "type": "function", "function": {"name": "historical_add", "arguments": '{"a":2,"b":3}'}}]},
                        {"role": "tool", "tool_call_id": "sdk-call-17", "content": '{"result":5}'}]
                    response = chat.chat.completions.create(**function_args)
                    assert "offline" in response.choices[0].message.content and not response.choices[0].message.tool_calls
                    results.append("OpenAI historical tool call and result preserved as text")
                    chunks = list(chat.chat.completions.create(**function_args, stream=True))
                    assert "offline" in "".join(c.choices[0].delta.content or "" for c in chunks)
                    assert all(not c.choices[0].delta.tool_calls for c in chunks) and chunks[-1].choices[0].finish_reason == "stop"
                    results.append("OpenAI ignored function tools SSE without fabricated calls")
                gemini = genai.Client(api_key="offline-fixture-key", http_options=types.HttpOptions(
                    base_url=base, api_version="v1beta", client_args={"trust_env": False}))
                try:
                    assert any(m.name.endswith("gemini-4-argon") for m in gemini.models.list())
                    response = gemini.models.generate_content(model="gemini-4-argon", contents="offline fixture")
                    parts = response.candidates[0].content.parts
                    assert any(p.executable_code for p in parts)
                    assert any(p.code_execution_result for p in parts)
                    assert response.candidates[0].grounding_metadata.grounding_chunks
                    assert response.usage_metadata.prompt_token_count > 0
                    assert response.usage_metadata.candidates_token_count > 0
                    assert response.usage_metadata.thoughts_token_count is None
                    assert response.usage_metadata.cached_content_token_count is None
                    results.append("Google GenAI full + code + execution + grounding + token usage")
                    history = [types.Content(role="user", parts=[types.Part(text="offline fixture")]),
                        response.candidates[0].content,
                        types.Content(role="user", parts=[types.Part(text="continue previous calculation")])]
                    replay = gemini.models.generate_content(model="gemini-4-argon", contents=history)
                    assert "offline" in "".join(p.text or "" for p in replay.candidates[0].content.parts)
                    results.append("Google GenAI own code/result history replay")
                    signature_history = [types.Content(role="user", parts=[types.Part(text="previous question")]),
                        types.Content(role="model", parts=[types.Part(text="previous answer", thought_signature=b"opaque-signature")]),
                        types.Content(role="user", parts=[types.Part(text="continue")])]
                    replay = gemini.models.generate_content(model="gemini-4-argon", contents=signature_history)
                    assert "offline" in "".join(p.text or "" for p in replay.candidates[0].content.parts)
                    results.append("Google GenAI text history with thought signature")
                    long_history = [types.Content(role="user" if i % 2 == 0 else "model",
                        parts=[types.Part(text=f"history turn {i}")]) for i in range(513)]
                    replay = gemini.models.generate_content(model="gemini-4-argon", contents=long_history)
                    assert "offline" in "".join(p.text or "" for p in replay.candidates[0].content.parts)
                    results.append("Google GenAI 513-turn history")
                    long_history = [types.Content(role="user" if i % 2 == 0 else "model",
                        parts=[types.Part(text=f"history turn {i} start" + " " * 2200 + "end")]) for i in range(514)]
                    assert sum(len(c.parts[0].text.encode()) for c in long_history) > 1024 * 1024
                    response = gemini.models.generate_content(model="gemini-4-argon", contents=long_history)
                    assert "offline" in "".join(p.text or "" for p in response.candidates[0].content.parts)
                    results.append("Google GenAI >1 MiB 514-turn model continuation")
                    chunks = list(gemini.models.generate_content_stream(model="gemini-4-argon", contents=long_history))
                    assert "offline" in "".join(p.text or "" for c in chunks for p in c.candidates[0].content.parts)
                    assert chunks[-1].candidates[0].finish_reason == "STOP"
                    results.append("Google GenAI >1 MiB model continuation SSE")
                    function_config = types.GenerateContentConfig(
                        tools=[types.Tool(function_declarations=[types.FunctionDeclaration(name="unused_sdk_function", parameters={"type": "OBJECT", "properties": {}})])],
                        tool_config=types.ToolConfig(function_calling_config=types.FunctionCallingConfig(mode="ANY", allowed_function_names=["unused_sdk_function"])),
                        automatic_function_calling=types.AutomaticFunctionCallingConfig(disable=True))
                    response = gemini.models.generate_content(model="gemini-4-argon", contents="offline fixture", config=function_config)
                    assert "offline" in "".join(p.text or "" for p in response.candidates[0].content.parts) and not response.function_calls
                    results.append("Google GenAI unsupported function declarations ignored as text")
                    function_history = [types.Content(role="model", parts=[types.Part.from_function_call(name="historical_add", args={"a": 2, "b": 3})]),
                        types.Content(role="user", parts=[types.Part.from_function_response(name="historical_add", response={"result": 5})])]
                    response = gemini.models.generate_content(model="gemini-4-argon", contents=function_history, config=function_config)
                    assert "offline" in "".join(p.text or "" for p in response.candidates[0].content.parts) and not response.function_calls
                    results.append("Google GenAI historical function call and result preserved as text")
                    chunks = list(gemini.models.generate_content_stream(model="gemini-4-argon", contents=function_history, config=function_config))
                    assert "offline" in "".join(p.text or "" for c in chunks for p in c.candidates[0].content.parts)
                    assert all(not c.function_calls for c in chunks) and chunks[-1].candidates[0].finish_reason == "STOP"
                    results.append("Google GenAI ignored function tools SSE without fabricated calls")
                    chunks = list(gemini.models.generate_content_stream(model="gemini-4-argon", contents="offline fixture"))
                    assert "offline" in "".join(p.text or "" for c in chunks for p in c.candidates[0].content.parts)
                    assert chunks[-1].candidates[0].finish_reason == "STOP"
                    assert chunks[-1].usage_metadata.total_token_count > 0
                    results.append("Google GenAI SSE + terminal finish")
                    response = gemini.models.generate_content(model="gemini-4-argon", contents="offline fixture",
                        config=types.GenerateContentConfig(response_mime_type="application/json"))
                    assert json.loads(response.candidates[0].content.parts[0].text) == {"ok": True}
                    results.append("Google GenAI JSON validation")
                    chunks = list(gemini.models.generate_content_stream(model="gemini-4-argon", contents="offline fixture",
                        config=types.GenerateContentConfig(response_mime_type="application/json")))
                    assert json.loads(chunks[0].candidates[0].content.parts[0].text) == {"ok": True}
                    assert chunks[-1].candidates[0].finish_reason == "STOP"
                    results.append("Google GenAI JSON SSE buffering")
                finally:
                    gemini.close()
                print(json.dumps({"mode": "local synthetic fixtures; no Google/model calls",
                    "openai": importlib.metadata.version("openai"),
                    "google-genai": importlib.metadata.version("google-genai"), "passed": results}, indent=2))
            finally:
                if base:
                    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
                    opener.open(urllib.request.Request(base + "/__stop", data=b"", method="POST")).close()
                try:
                    process.wait(timeout=15)
                except subprocess.TimeoutExpired:
                    process.terminate()
                    process.wait(timeout=15)
            if process.returncode:
                log.seek(0)
                raise RuntimeError(log.read())


if __name__ == "__main__":
    main()

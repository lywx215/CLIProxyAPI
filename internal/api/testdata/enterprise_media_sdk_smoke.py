"""Offline SDK smoke test against a Go httptest server with fixture video bytes.

Run via TestEnterpriseVideoPythonSDKSmoke. This script never discovers credentials
and accepts only the loopback HTTP origin supplied by the Go fixture.
"""

import hashlib
import importlib.metadata
import json
import os
from pathlib import Path
import sys
import time
import urllib.parse
import warnings

warnings.filterwarnings("ignore", category=DeprecationWarning)
if deps := os.environ.get("CLIPROXY_ENTERPRISE_SDK_DEPS"):
    sys.path.insert(0, deps)

import httpx
from openai import OpenAI
from google import genai
from google.genai import types


def main():
    base = os.environ["CLIPROXY_ENTERPRISE_SDK_BASE_URL"].rstrip("/")
    parsed = urllib.parse.urlparse(base)
    assert parsed.scheme == "http" and parsed.hostname == "127.0.0.1", "fixture must use loopback HTTP"
    model = os.environ["CLIPROXY_ENTERPRISE_SDK_MODEL"]
    fixture = Path(os.environ["CLIPROXY_ENTERPRISE_SDK_MEDIA"]).read_bytes()
    expected = hashlib.sha256(fixture).hexdigest()
    key = "enterprise-sdk-fixture-alice"
    downloads = {}

    def verify(label, data):
        assert data == fixture, f"{label}: downloaded fixture bytes differ"
        digest = hashlib.sha256(data).hexdigest()
        assert digest == expected
        downloads[label] = {"bytes": len(data), "sha256": digest}

    with OpenAI(
        api_key=key,
        base_url=base + "/openai/v1",
        max_retries=0,
        timeout=None,
        http_client=httpx.Client(trust_env=False, timeout=None),
    ) as client:
        job = client.videos.create(model=model, prompt="Offline video fixture")
        deadline = time.monotonic() + 10
        while True:
            job = client.videos.retrieve(job.id)
            assert job.status != "failed", "OpenAI fixture task failed"
            if job.status == "completed":
                break
            assert time.monotonic() < deadline, "OpenAI fixture task did not complete"
        verify("openai_sdk_download_content", client.videos.download_content(job.id).read())

    with genai.Client(
        vertexai=False,
        api_key=key,
        http_options=types.HttpOptions(
            base_url=base,
            api_version="v1beta",
            client_args={"trust_env": False},
            retry_options=types.HttpRetryOptions(attempts=1),
        ),
    ) as client:
        operation = client.models.generate_videos(
            model=model,
            prompt="Offline video fixture",
            config=types.GenerateVideosConfig(number_of_videos=1),
        )
        assert operation.name.startswith(f"models/{model}/operations/")
        deadline = time.monotonic() + 10
        while True:
            operation = client.operations.get(operation)
            assert not operation.error, "Google fixture task failed"
            if operation.done:
                break
            assert time.monotonic() < deadline, "Google fixture task did not complete"
        generated = operation.response.generated_videos
        assert len(generated) == 1
        video = generated[0].video
        assert video.uri.startswith("files/") and video.mime_type == "video/mp4"
        verify("google_sdk_files_download_v1beta", client.files.download(file=video))
        assert video.video_bytes == fixture
        verify(
            "google_sdk_files_download_download_v1beta",
            client.files.download(
                file=types.Video(uri=video.uri),
                config=types.DownloadFileConfig(
                    http_options=types.HttpOptions(base_url=base + "/download")
                ),
            ),
        )
        identifier = operation.name.rsplit("/", 1)[1]
        alias = client.operations.get(types.GenerateVideosOperation(name="operations/" + identifier))
        assert alias.done and alias.name == operation.name
        assert alias.response.generated_videos[0].video.uri == video.uri
        verify("google_sdk_global_operation_download", client.files.download(file=alias.response.generated_videos[0]))

    result = {
        "kind": "offline_sdk_fixture",
        "status": "passed",
        "versions": {
            "openai": importlib.metadata.version("openai"),
            "google-genai": importlib.metadata.version("google-genai"),
        },
        "methods": [
            "OpenAI.videos.create (multipart)",
            "OpenAI.videos.retrieve",
            "OpenAI.videos.download_content",
            "Google.models.generate_videos",
            "Google.operations.get (model-scoped and global)",
            "Google.files.download (v1beta and download/v1beta)",
        ],
        "downloads": downloads,
    }
    Path(os.environ["CLIPROXY_ENTERPRISE_SDK_REPORT"]).write_text(
        json.dumps(result, ensure_ascii=False, indent=2) + "\n", encoding="utf-8"
    )
    print(json.dumps({"status": "passed", "downloads": len(downloads), "bytes": len(fixture)}))


if __name__ == "__main__":
    main()

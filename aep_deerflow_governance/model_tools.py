"""Governed model tools — multimodal capabilities over the AEP model gateway.

Enabled iff the runtime env carries ``MODEL_TOOLS_ENABLED`` plus a
``MODEL_TOOLS_GATEWAY_URL`` (the gateway-authorizer address). Each capability
is independently configured with the catalog model id that serves it
(``MODEL_TOOLS_IMAGE`` / ``MODEL_TOOLS_SPEECH`` / ``MODEL_TOOLS_VIDEO``); an
unconfigured capability registers no tool — the model simply never sees it.

Execution never uses shared credentials. Every call:

- resolves the requester from the trusted runtime context (injected as the
  reserved ``model_requester_user_id`` argument in ``wrap_tool_call`` — a
  model-supplied value is overwritten, never trusted; an unresolved requester
  fails closed);
- obtains a short-lived data-plane token for that user via the M2M issuance
  endpoint (scopes derive from the user's own assignments, so a user without
  the image model assigned receives a clear 403, not a silent bypass);
- calls the gateway, which enforces scope and writes the access audit.

Generated artifacts are written to ``MODEL_TOOLS_OUTPUT_DIR`` (default
``/tmp/aep-model-outputs``) and the tool returns the file path; presentation
of artifacts in chat is the frontend's concern.
"""
from __future__ import annotations

import base64
import json
import os
import time
import urllib.error
import urllib.request
import uuid
from typing import Any, Callable

from langchain_core.tools import BaseTool, tool

MODEL_REQUESTER_ARG = "model_requester_user_id"
IMAGE_TOOL = "image_generation"
SPEECH_TOOL = "speech_synthesis"
VIDEO_TOOL = "video_generation"
VIDEO_POLL_INTERVAL_S = 5
VIDEO_POLL_TIMEOUT_S = 300
MODEL_TOOL_NAMES = frozenset({IMAGE_TOOL, SPEECH_TOOL, VIDEO_TOOL})


class _ModelTokenProvider:
    """Per-user model token cache in front of the M2M issuance endpoint."""

    def __init__(self, client: Any) -> None:
        self._client = client
        self._tokens: dict[str, tuple[str, float]] = {}
        from threading import Lock
        self._lock = Lock()

    def token(self, user_id: str) -> str:
        now = time.monotonic()
        with self._lock:
            cached = self._tokens.get(user_id)
            if cached and cached[1] > now + 30:
                return cached[0]
        issued = self._client.issue_model_token(user_id)
        raw = str(issued.get("modelAccessToken") or "")
        expires_in = float(issued.get("expiresIn") or 60)
        if not raw:
            raise RuntimeError("empty model token issued")
        with self._lock:
            self._tokens[user_id] = (raw, now + max(30.0, expires_in - 30.0))
        return raw


class _ModelGateway:
    """Gateway caller; substituted in tests."""

    def __init__(self, base_url: str, provider: _ModelTokenProvider) -> None:
        self._base_url = base_url.rstrip("/")
        self._provider = provider

    def post_json(self, user_id: str, model: str, path: str, body: dict[str, Any], timeout: int = 300) -> dict[str, Any]:
        return self._call(user_id, model, "POST", path, json.dumps(body).encode(), timeout)

    def post_raw(self, user_id: str, model: str, path: str, body: dict[str, Any], timeout: int = 300) -> bytes:
        return self._call(user_id, model, "POST", path, json.dumps(body).encode(), timeout)

    def get(self, user_id: str, model: str, path: str, timeout: int = 120) -> bytes:
        return self._call(user_id, model, "GET", path, None, timeout)

    def _call(self, user_id: str, model: str, method: str, path: str, payload: bytes | None, timeout: int) -> Any:
        headers = {"Authorization": f"Bearer {self._provider.token(user_id)}"}
        if payload is not None:
            headers["Content-Type"] = "application/json"
        if method == "GET":
            # Resource polling (video jobs) names the model via header; the
            # authorizer validates it against the token's model scopes.
            headers["X-AEP-Model-ID"] = model
        req = urllib.request.Request(self._base_url + path, data=payload, headers=headers, method=method)
        try:
            with urllib.request.urlopen(req, timeout=timeout) as resp:
                raw = resp.read()
        except urllib.error.HTTPError as error:
            detail = error.read()[:300].decode("utf-8", "replace")
            if error.code == 403:
                raise PermissionError(f"the current user is not assigned model {model}") from error
            raise RuntimeError(f"model gateway returned {error.code}: {detail}") from error
        if method == "GET":
            return raw
        return json.loads(raw or b"{}")


def _save(output_dir: str, name: str, data: bytes) -> str:
    os.makedirs(output_dir, exist_ok=True)
    path = os.path.join(output_dir, name)
    with open(path, "wb") as handle:
        handle.write(data)
    return path


def build_model_tools(environ: dict[str, str] | None = None,
                      client: Any = None,
                      gateway_factory: Callable[[], _ModelGateway] | None = None) -> list[BaseTool]:
    """Build multimodal model tools from the rendered env.

    ``client`` is the middleware's own _AepClient (single credential source);
    ``gateway_factory`` substitutes the gateway-carrying caller in tests.
    """
    env = dict(os.environ if environ is None else environ)
    if not env.get("MODEL_TOOLS_ENABLED", "").strip() or client is None and gateway_factory is None:
        return []
    gateway_url = env.get("MODEL_TOOLS_GATEWAY_URL", "").strip()
    if not gateway_url and gateway_factory is None:
        print("[aep-governance] model tools disabled: MODEL_TOOLS_GATEWAY_URL not set", flush=True)
        return []
    output_dir = env.get("MODEL_TOOLS_OUTPUT_DIR", "/tmp/aep-model-outputs").strip() or "/tmp/aep-model-outputs"
    provider = _ModelTokenProvider(client)

    def make_gateway() -> _ModelGateway:
        if gateway_factory is not None:
            return gateway_factory()
        return _ModelGateway(gateway_url, provider)

    tools: list[BaseTool] = []

    image_model = env.get("MODEL_TOOLS_IMAGE", "").strip()
    if image_model:
        @tool
        def image_generation(prompt: str, size: str = "1024x1024",
                             model_requester_user_id: str = "") -> str:
            """生成一张图像并保存为 PNG 文件，返回文件路径。
            prompt 用英文或中文描述画面；size 形如 1024x1024（宽x高）。"""
            requester = model_requester_user_id.strip()
            if not requester:
                return "DENIED: no trusted requester on this conversation; generation requires a resolved AEP user."
            try:
                result = make_gateway().post_json(requester, image_model, "/v1/images/generations",
                                                  {"model": image_model, "prompt": prompt, "size": size})
            except PermissionError as error:
                return f"DENIED: {error}"
            except RuntimeError as error:
                return f"ERROR: {error}"
            items = result.get("data") or []
            b64 = items[0].get("b64_json") if items else None
            if not b64:
                return f"ERROR: gateway returned no image data: {str(result)[:200]}"
            data = base64.b64decode(b64)
            path = _save(output_dir, f"image-{int(time.time())}-{uuid.uuid4().hex[:8]}.png", data)
            return f"image saved: {path} ({len(data)} bytes, {size})"

        tools.append(image_generation)

    speech_model = env.get("MODEL_TOOLS_SPEECH", "").strip()
    if speech_model:
        @tool
        def speech_synthesis(text: str, model_requester_user_id: str = "") -> str:
            """把一段中文/英文文本合成为语音 WAV 文件，返回文件路径。"""
            requester = model_requester_user_id.strip()
            if not requester:
                return "DENIED: no trusted requester on this conversation; synthesis requires a resolved AEP user."
            try:
                data = make_gateway().post_raw(requester, speech_model, "/v1/audio/speech",
                                               {"model": speech_model, "input": text}, timeout=180)
            except PermissionError as error:
                return f"DENIED: {error}"
            except RuntimeError as error:
                return f"ERROR: {error}"
            path = _save(output_dir, f"speech-{int(time.time())}-{uuid.uuid4().hex[:8]}.wav", data)
            return f"speech saved: {path} ({len(data)} bytes)"

        tools.append(speech_synthesis)

    video_model = env.get("MODEL_TOOLS_VIDEO", "").strip()
    if video_model:
        @tool
        def video_generation(prompt: str, seconds: int = 5,
                             model_requester_user_id: str = "") -> str:
            """生成一段短视频（异步任务，本工具会等待完成）并保存为 MP4，返回文件路径。
            seconds 为 2-10；生成耗时约 1-3 分钟。"""
            requester = model_requester_user_id.strip()
            if not requester:
                return "DENIED: no trusted requester on this conversation; generation requires a resolved AEP user."
            gateway = make_gateway()
            try:
                created = gateway.post_json(requester, video_model, "/v1/videos",
                                            {"model": video_model, "prompt": prompt, "seconds": max(2, min(int(seconds), 10))})
            except PermissionError as error:
                return f"DENIED: {error}"
            except RuntimeError as error:
                return f"ERROR: {error}"
            job_id = str(created.get("id") or "")
            if not job_id:
                return f"ERROR: gateway returned no video job id: {str(created)[:200]}"
            deadline = time.monotonic() + VIDEO_POLL_TIMEOUT_S
            while time.monotonic() < deadline:
                time.sleep(VIDEO_POLL_INTERVAL_S)
                try:
                    status = json.loads(gateway.get(requester, video_model, f"/v1/videos/{job_id}"))
                except PermissionError as error:
                    return f"DENIED: {error}"
                except RuntimeError as error:
                    return f"ERROR: {error}"
                state = str(status.get("status") or "")
                if state == "completed":
                    try:
                        content = gateway.get(requester, video_model, f"/v1/videos/{job_id}/content")
                    except RuntimeError as error:
                        return f"ERROR: {error}"
                    path = _save(output_dir, f"video-{job_id}.mp4", content)
                    return f"video saved: {path} ({len(content)} bytes)"
                if state == "failed":
                    return f"ERROR: video job {job_id} failed: {status.get('error', 'unknown')}"
            return (f"video job {job_id} is still running after {VIDEO_POLL_TIMEOUT_S}s; "
                    f"tell the user it will finish in the background and can be retrieved by job id later.")

        tools.append(video_generation)

    return tools

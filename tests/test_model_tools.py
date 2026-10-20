"""Model tools: build gating, requester injection, fail-closed gateway calls.

Run (governance repo has no python env of its own; the DeerFlow backend
venv provides langchain):

    cd DeerFlow/backend && \
    PYTHONPATH=../../aep-deerflow-governance .venv/bin/python -m pytest \
        ../../aep-deerflow-governance/tests/test_model_tools.py -q
"""
from __future__ import annotations

import base64
import json
from types import SimpleNamespace
from typing import Any

from aep_deerflow_governance import AepGovernanceMiddleware
from aep_deerflow_governance.model_tools import (
    MODEL_REQUESTER_ARG,
    MODEL_TOOL_NAMES,
    _ModelGateway,
    build_model_tools,
)

ENABLED_ENV = {
    "MODEL_TOOLS_ENABLED": "1",
    "MODEL_TOOLS_GATEWAY_URL": "http://authorizer.invalid",
    "MODEL_TOOLS_IMAGE": "flux-2-klein-4b",
    "MODEL_TOOLS_SPEECH": "cosyvoice3-tts",
    "MODEL_TOOLS_VIDEO": "wan2-2-ti2v-5b",
    "MODEL_TOOLS_OUTPUT_DIR": "/tmp/test-model-tools-outputs",
}


class FakeClient:
    def __init__(self) -> None:
        self.issued: list[str] = []

    def issue_model_token(self, user_id: str) -> dict[str, Any]:
        self.issued.append(user_id)
        return {"modelAccessToken": f"tok-{user_id}", "expiresIn": 900, "sessionId": "s1", "modelScopes": []}


class FakeGateway(_ModelGateway):
    def __init__(self) -> None:
        super().__init__("http://invalid", None)
        self.calls: list[tuple[str, str, str, dict[str, Any] | None]] = []
        self.stage = "completed"

    def post_json(self, user_id, model, path, body, timeout=300):
        self.calls.append((user_id, model, "POST", body))
        if path == "/v1/images/generations":
            return {"data": [{"b64_json": base64.b64encode(b"png-bytes").decode()}]}
        if path == "/v1/videos":
            return {"id": "video-1", "status": "queued"}
        raise AssertionError(path)

    def post_raw(self, user_id, model, path, body, timeout=300):
        self.calls.append((user_id, model, "POST", body))
        return b"wav-bytes"

    def get(self, user_id, model, path, timeout=120):
        self.calls.append((user_id, model, "GET", None))
        if path.endswith("/content"):
            return b"mp4-bytes"
        return json.dumps({"id": "video-1", "status": self.stage}).encode()


def _build(gateway: FakeGateway):
    import aep_deerflow_governance.model_tools as mt
    original_sleep = mt.time.sleep
    mt.time.sleep = lambda seconds: None  # polling test must not really sleep
    try:
        tools = build_model_tools(environ=ENABLED_ENV, client=FakeClient(),
                                  gateway_factory=lambda: gateway)
    finally:
        mt.time.sleep = original_sleep
    return {tool.name: tool for tool in tools}


def test_build_gating() -> None:
    assert build_model_tools(environ={}) == []
    assert build_model_tools(environ={"MODEL_TOOLS_ENABLED": "1"}) == []
    missing_url = {k: v for k, v in ENABLED_ENV.items() if k != "MODEL_TOOLS_GATEWAY_URL"}
    assert build_model_tools(environ=missing_url, client=FakeClient()) == []
    only_image = {k: v for k, v in ENABLED_ENV.items() if not k.startswith("MODEL_TOOLS_SPEECH") and not k.startswith("MODEL_TOOLS_VIDEO")}
    tools = {tool.name for tool in build_model_tools(environ=only_image, client=FakeClient())}
    assert tools == {"image_generation"}


def test_image_generation_saves_file_and_uses_requester_token() -> None:
    gateway = FakeGateway()
    tools = _build(gateway)
    output = tools["image_generation"].invoke({"prompt": "a cat", MODEL_REQUESTER_ARG: "user-a"})
    assert output.startswith("image saved:") and "png" in output and "png-bytes" not in output
    assert gateway.calls == [("user-a", "flux-2-klein-4b", "POST",
                              {"model": "flux-2-klein-4b", "prompt": "a cat", "size": "1024x1024"})]


def test_speech_and_video_round_trip() -> None:
    gateway = FakeGateway()
    tools = _build(gateway)
    speech = tools["speech_synthesis"].invoke({"text": "你好", MODEL_REQUESTER_ARG: "user-a"})
    assert speech.startswith("speech saved:")
    video = tools["video_generation"].invoke({"prompt": "waves", "seconds": 2, MODEL_REQUESTER_ARG: "user-a"})
    assert video.startswith("video saved:") and "mp4" in video
    # polling carries the model header via GET + content download
    gets = [call for call in gateway.calls if call[2] == "GET"]
    assert gets and all(call[1] == "wan2-2-ti2v-5b" for call in gets)


def test_tools_fail_closed_without_requester() -> None:
    tools = _build(FakeGateway())
    for name in ("image_generation", "speech_synthesis", "video_generation"):
        assert tools[name].invoke({"prompt": "x"} if "image" in name or "video" in name else {"text": "x"}).startswith("DENIED:")


def test_wrap_tool_call_injects_trusted_requester() -> None:
    middleware = AepGovernanceMiddleware(
        "https://unused.invalid", "svc", "test", "must-not-fallback",
        governed_tools=[],
    )
    middleware.identities = SimpleNamespace(resolve=lambda external: "user-a")
    executed: list[Any] = []

    def handler(request: Any) -> str:
        executed.append(request.tool_call["args"])
        return "done"

    result = middleware.wrap_tool_call(SimpleNamespace(
        tool_call={"name": "image_generation", "args": {"prompt": "cat", MODEL_REQUESTER_ARG: "forged"}, "id": "c1"},
        runtime=SimpleNamespace(context={"channel_user_id": "external"}),
    ), handler)
    assert result == "done"
    assert executed == [{"prompt": "cat", MODEL_REQUESTER_ARG: "user-a"}]


def test_wrap_tool_call_fails_closed_without_requester() -> None:
    middleware = AepGovernanceMiddleware(
        "https://unused.invalid", "svc", "test", "must-not-fallback",
        governed_tools=[],
    )
    middleware.identities = SimpleNamespace(resolve=lambda external: None)
    result = middleware.wrap_tool_call(SimpleNamespace(
        tool_call={"name": "speech_synthesis", "args": {"text": "hi", MODEL_REQUESTER_ARG: "forged"}, "id": "c2"},
        runtime=SimpleNamespace(context={"channel_user_id": "external"}),
    ), lambda request: "must not run")
    assert str(result.content).startswith("DENIED:")


def test_model_tool_names_cover_registered_tools() -> None:
    tools = _build(FakeGateway())
    assert MODEL_TOOL_NAMES >= set(tools)

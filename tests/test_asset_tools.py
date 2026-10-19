"""Asset tools: build gating, requester injection, fail-closed execution.

Run (governance repo has no python env of its own; the DeerFlow backend
venv provides langchain):

    cd DeerFlow/backend && \
    PYTHONPATH=../../aep-deerflow-governance .venv/bin/python -m pytest \
        ../../aep-deerflow-governance/tests/test_asset_tools.py -q
"""
from __future__ import annotations

from types import SimpleNamespace
from typing import Any

from aep_deerflow_governance import AepGovernanceMiddleware
from aep_deerflow_governance.asset_tools import ASSET_REQUESTER_ARG, _AssetGateway, build_asset_tools

ENABLED_ENV = {"ASSET_TOOLS_ENABLED": "1", "ASSET_SUBJECT_ID": "admin-helper"}


class FakeGateway(_AssetGateway):
    def __init__(self, assets: list[dict[str, Any]], executed: list[dict[str, Any]], result: dict[str, Any]) -> None:
        super().__init__(client=None, subject_id="admin-helper")
        self.assets = assets
        self.executed = executed
        self.result = result

    def list_assets(self) -> list[dict[str, Any]]:
        return list(self.assets)

    def execute(self, asset: str, command: str, requester: str, timeout_seconds: int) -> dict[str, Any]:
        from aep_deerflow_governance.asset_tools import _unknown_asset_message
        match = next((record for record in self.assets if asset in (record.get("assetId"), record.get("name"))), None)
        if match is None:
            raise ValueError(_unknown_asset_message(asset, self.assets))
        self.executed.append({
            "assetId": match["assetId"], "command": command,
            "requester": requester, "timeout": timeout_seconds,
        })
        return dict(self.result)


ASSETS = [
    {"assetId": "asset-a", "name": "Bastion", "capabilities": [{"key": "exec", "risk": "medium"}]},
    {"assetId": "asset-b", "name": "Lab Host", "capabilities": [{"key": "exec", "risk": "medium"}]},
]


def _build(gateway: FakeGateway):
    return build_asset_tools(
        environ=ENABLED_ENV,
        gateway_factory=lambda subject: gateway,
    )


def test_build_gating() -> None:
    assert build_asset_tools(environ={}) == []
    assert build_asset_tools(environ={"ASSET_TOOLS_ENABLED": "1"}) == []
    assert build_asset_tools(environ={"ASSET_SUBJECT_ID": "admin-helper"}) == []
    tools = build_asset_tools(environ=ENABLED_ENV, gateway_factory=lambda subject: FakeGateway([], [], {}))
    assert sorted(tool.name for tool in tools) == ["asset_list", "asset_ssh_exec"]


def test_asset_list_formats_bound_assets() -> None:
    tools = {tool.name: tool for tool in _build(FakeGateway(ASSETS, [], {}))}
    listing = tools["asset_list"].invoke({})
    assert "Bastion" in listing and "exec(medium)" in listing
    empty = {tool.name: tool for tool in _build(FakeGateway([], [], {}))}
    assert "没有绑定" in empty["asset_list"].invoke({})


def test_ssh_exec_requires_trusted_requester() -> None:
    tools = {tool.name: tool for tool in _build(FakeGateway(ASSETS, [], {}))}
    output = tools["asset_ssh_exec"].invoke({"asset": "Bastion", "command": "uptime"})
    assert output.startswith("DENIED:")


def test_ssh_exec_runs_and_formats_output() -> None:
    gateway = FakeGateway(ASSETS, [], {"exitCode": 0, "output": "up 42 days\n", "truncated": False, "durationMs": 120})
    tools = {tool.name: tool for tool in _build(gateway)}
    output = tools["asset_ssh_exec"].invoke({
        "asset": "Bastion", "command": "uptime", "timeout_seconds": 999,
        ASSET_REQUESTER_ARG: "user-a",
    })
    assert "exit code: 0" in output and "up 42 days" in output
    assert gateway.executed == [{"assetId": "asset-a", "command": "uptime", "requester": "user-a", "timeout": 120}]


def test_ssh_exec_unknown_asset_lists_bound() -> None:
    gateway = FakeGateway(ASSETS, [], {})
    tools = {tool.name: tool for tool in _build(gateway)}
    output = tools["asset_ssh_exec"].invoke({"asset": "nope", "command": "uptime", ASSET_REQUESTER_ARG: "user-a"})
    assert output.startswith("ERROR:") and "Bastion" in output and "Lab Host" in output


def _middleware(requester: str | None) -> AepGovernanceMiddleware:
    middleware = AepGovernanceMiddleware(
        "https://unused.invalid", "svc", "test", "must-not-fallback",
        governed_tools=[],
    )
    middleware.identities = SimpleNamespace(resolve=lambda external: requester)
    return middleware


def _asset_request(requester_arg: Any) -> SimpleNamespace:
    return SimpleNamespace(
        tool_call={"name": "asset_ssh_exec", "args": {"asset": "Bastion", "command": "uptime", ASSET_REQUESTER_ARG: requester_arg}, "id": "asset-call"},
        runtime=SimpleNamespace(context={"channel_user_id": "external"}),
    )


def test_wrap_tool_call_injects_trusted_requester() -> None:
    middleware = _middleware("user-a")
    executed: list[Any] = []

    def handler(request: Any) -> str:
        executed.append(request.tool_call["args"])
        return "done"

    result = middleware.wrap_tool_call(_asset_request("forged-by-model"), handler)
    assert result == "done"
    assert executed == [{"asset": "Bastion", "command": "uptime", ASSET_REQUESTER_ARG: "user-a"}]


def test_wrap_tool_call_fails_closed_without_requester() -> None:
    middleware = _middleware(None)
    result = middleware.wrap_tool_call(_asset_request("forged-by-model"), lambda request: "must not run")
    assert str(result.content).startswith("DENIED:")

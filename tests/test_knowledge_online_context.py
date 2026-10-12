from __future__ import annotations

import copy
from typing import Any

from langchain_core.messages import ToolMessage

from aep_deerflow_governance import AepGovernanceMiddleware


class Runtime:
    def __init__(self, context: dict[str, Any] | None):
        self.context = context


class Request:
    def __init__(self, name: str, args: dict[str, Any], context: dict[str, Any] | None):
        self.tool_call = {"name": name, "args": args, "id": "call-1"}
        self.runtime = Runtime(context)

    def override(self, **kwargs):
        result = copy.copy(self)
        for key, value in kwargs.items():
            setattr(result, key, value)
        return result


def middleware() -> AepGovernanceMiddleware:
    return AepGovernanceMiddleware(
        aep_base_url="http://unused.invalid",
        service_username="svc",
        service_password="pw",
        default_aep_user="must-not-fallback",
        governed_tools=[],
        knowledge_bases={"kb-1": "one"},
        knowledge_online_authorization=True,
    )


def test_online_policy_injects_server_context_and_overwrites_model_arguments() -> None:
    mw = middleware()
    req = Request(
        "mcp__weknora__hybrid_search",
        {"kb_id": "kb-1", "query": "q", "knowledge_context_handle": "forged", "knowledge_employee_name": "forged"},
        {"zhiyuan_knowledge_context": "A" * 42 + "_", "zhiyuan_employee_name": "sales-helper"},
    )
    observed: dict[str, Any] = {}
    result = mw.wrap_tool_call(req, handler=lambda enriched: observed.update(enriched.tool_call["args"]) or "OK")
    assert result == "OK"
    assert observed["knowledge_context_handle"] == "A" * 42 + "_"
    assert observed["knowledge_employee_name"] == "sales-helper"


def test_online_policy_fails_closed_without_portal_run_context() -> None:
    mw = middleware()
    reached: list[bool] = []
    result = mw.wrap_tool_call(
        Request("weknora_hybrid_search", {"kb_id": "kb-1", "query": "q"}, None),
        handler=lambda _: reached.append(True) or "should-not-run",
    )
    assert isinstance(result, ToolMessage)
    assert not reached
    assert "context unavailable" in result.content


def test_online_policy_rejects_read_alias_without_portal_gate() -> None:
    mw = middleware()
    result = mw.wrap_tool_call(
        Request(
            "weknora_knowledge_search",
            {"knowledge_base_ids": ["kb-1"]},
            {"zhiyuan_knowledge_context": "A" * 42 + "_", "zhiyuan_employee_name": "sales-helper"},
        ),
        handler=lambda _: "should-not-run",
    )
    assert isinstance(result, ToolMessage)
    assert "not protected" in result.content

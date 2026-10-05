"""Knowledge-base whitelist enforcement in AepGovernanceMiddleware.

Run (the governance repo has no python env of its own; the DeerFlow backend
venv provides langchain):

    cd DeerFlow/backend && \
    PYTHONPATH=../../aep-deerflow-governance .venv/bin/python -m pytest \
        ../../aep-deerflow-governance/tests/test_knowledge_whitelist.py -q

The whitelist is per-employee static policy: these tests construct the
middleware with ``governed_tools=[]`` so the allow path never touches the
network, and drive ``_check``/``wrap_tool_call`` with minimal fakes.
"""

from __future__ import annotations

from typing import Any

from langchain_core.messages import ToolMessage

from aep_deerflow_governance import AepGovernanceMiddleware

WHITELIST = {"kb-1": "销售知识库", "kb-2": "产品知识库"}


class FakeCall:
    def __init__(self, name: str, args: dict[str, Any], call_id: str = "call-1"):
        self.name = name
        self.args = args
        self.id = call_id


class FakeRequest:
    def __init__(self, call: FakeCall):
        self.tool_call = call


def middleware(knowledge_bases: dict[str, str] | None) -> AepGovernanceMiddleware:
    return AepGovernanceMiddleware(
        aep_base_url="http://unused.invalid",
        service_username="svc",
        service_password="pw",
        default_aep_user="u1",
        governed_tools=[],
        knowledge_bases=knowledge_bases,
    )


def result_of(mw: AepGovernanceMiddleware, call: FakeCall) -> ToolMessage | str:
    return mw.wrap_tool_call(FakeRequest(call), handler=lambda request: "OK")


def test_absent_kwarg_keeps_legacy_unrestricted() -> None:
    mw = middleware(None)
    # No knowledge_base_ids: legacy behavior must not deny.
    assert result_of(mw, FakeCall("knowledge_search", {"query": "q"})) == "OK"
    assert result_of(mw, FakeCall("weknora__knowledge_search", {"query": "q", "knowledge_base_ids": ["any"]})) == "OK"


def test_missing_ids_is_denied_when_whitelist_configured() -> None:
    mw = middleware(WHITELIST)
    for name in ("knowledge_search", "weknora__knowledge_search"):
        outcome = result_of(mw, FakeCall(name, {"query": "q"}))
        assert isinstance(outcome, ToolMessage)
        assert "knowledge_base_ids" in outcome.content


def test_empty_or_non_list_ids_is_denied() -> None:
    mw = middleware(WHITELIST)
    for args in ({"query": "q", "knowledge_base_ids": []}, {"query": "q", "knowledge_base_ids": "kb-1"}):
        outcome = result_of(mw, FakeCall("knowledge_search", args))
        assert isinstance(outcome, ToolMessage)


def test_out_of_whitelist_ids_is_denied() -> None:
    mw = middleware(WHITELIST)
    outcome = result_of(mw, FakeCall("knowledge_search", {"query": "q", "knowledge_base_ids": ["kb-1", "kb-x"]}))
    assert isinstance(outcome, ToolMessage)
    assert "kb-x" not in outcome.content or "可用知识库" in outcome.content


def test_subset_of_whitelist_is_allowed() -> None:
    mw = middleware(WHITELIST)
    assert result_of(mw, FakeCall("knowledge_search", {"query": "q", "knowledge_base_ids": ["kb-2"]})) == "OK"
    assert result_of(mw, FakeCall("knowledge_search", {"query": "q", "knowledge_base_ids": ["kb-1", "kb-2"]})) == "OK"


def test_empty_whitelist_denies_everything() -> None:
    mw = middleware({})
    outcome = result_of(mw, FakeCall("knowledge_search", {"query": "q", "knowledge_base_ids": ["kb-1"]}))
    assert isinstance(outcome, ToolMessage)
    assert "未连接任何知识库" in outcome.content


def test_non_knowledge_tools_are_untouched() -> None:
    mw = middleware(WHITELIST)
    assert result_of(mw, FakeCall("web_search", {"query": "q"})) == "OK"


def test_governance_message_lists_bases_iff_configured() -> None:
    configured = middleware(WHITELIST)
    message = configured._governance_message("u1", frozenset())
    assert "销售知识库" in str(message.content)
    guest = configured._governance_message(None, frozenset())
    assert "kb-1" in str(guest.content)
    legacy = middleware(None)
    assert "知识库" not in str(legacy._governance_message("u1", frozenset()).content)

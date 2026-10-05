"""Knowledge-scope enforcement in AepGovernanceMiddleware.

Run (the governance repo has no python env of its own; the DeerFlow backend
venv provides langchain):

    cd DeerFlow/backend && \
    PYTHONPATH=../../aep-deerflow-governance .venv/bin/python -m pytest \
        ../../aep-deerflow-governance/tests/test_knowledge_whitelist.py -q

Two deployed weknora MCP surfaces are covered: the PyPI bridge's kb_id
tools plus its admin surface (default-deny under an explicit whitelist),
and the dsh bridge's knowledge_search(knowledge_base_ids). Legacy
employees (no whitelist) keep the unrestricted bridge. These tests
construct the middleware with ``governed_tools=[]`` so the allow path
never touches the network, and drive ``wrap_tool_call`` with minimal
fakes.
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


def outcome(mw: AepGovernanceMiddleware, call: FakeCall) -> ToolMessage | str:
    return mw.wrap_tool_call(FakeRequest(call), handler=lambda request: "OK")


def denied(result: ToolMessage | str) -> bool:
    return isinstance(result, ToolMessage)


# ---------------------------------------------------------------- legacy
def test_absent_kwapp_keeps_legacy_unrestricted() -> None:
    mw = middleware(None)
    assert outcome(mw, FakeCall("weknora_hybrid_search", {"kb_id": "any"})) == "OK"
    assert outcome(mw, FakeCall("weknora_delete_knowledge_base", {"kb_id": "any"})) == "OK"
    assert outcome(mw, FakeCall("knowledge_search", {"query": "q"})) == "OK"


# ------------------------------------------------------------- kb_id read
def test_kb_id_read_tools_pass_with_in_whitelist_id() -> None:
    mw = middleware(WHITELIST)
    for name in ("weknora_hybrid_search", "weknora__hybrid_search", "hybrid_search",
                 "weknora_wiki_search", "weknora_list_knowledge", "weknora_get_knowledge_base"):
        assert outcome(mw, FakeCall(name, {"kb_id": "kb-2", "query": "q"})) == "OK", name


def test_kb_id_missing_or_out_of_whitelist_is_denied() -> None:
    mw = middleware(WHITELIST)
    assert denied(outcome(mw, FakeCall("weknora_hybrid_search", {"query": "q"})))
    assert denied(outcome(mw, FakeCall("weknora_hybrid_search", {"kb_id": "kb-x", "query": "q"})))
    assert denied(outcome(mw, FakeCall("hybrid_search", {"kb_id": ""})))


def test_empty_whitelist_denies_every_read() -> None:
    mw = middleware({})
    result = outcome(mw, FakeCall("weknora_hybrid_search", {"kb_id": "kb-1", "query": "q"}))
    assert denied(result)
    assert "未连接任何知识库" in result.content


# ----------------------------------------------- knowledge_base_ids (dsh)
def test_knowledge_search_list_ids_still_enforced() -> None:
    mw = middleware(WHITELIST)
    assert outcome(mw, FakeCall("knowledge_search", {"query": "q", "knowledge_base_ids": ["kb-1", "kb-2"]})) == "OK"
    assert outcome(mw, FakeCall("weknora__knowledge_search", {"query": "q", "knowledge_base_ids": ["kb-1"]})) == "OK"
    assert denied(outcome(mw, FakeCall("knowledge_search", {"query": "q"})))
    assert denied(outcome(mw, FakeCall("knowledge_search", {"query": "q", "knowledge_base_ids": []})))
    assert denied(outcome(mw, FakeCall("knowledge_search", {"query": "q", "knowledge_base_ids": ["kb-1", "kb-x"]})))


# ------------------------------------------------- admin surface default-deny
def test_admin_and_session_tools_are_denied_under_policy() -> None:
    mw = middleware(WHITELIST)
    for name, args in [
        ("weknora_delete_knowledge_base", {"kb_id": "kb-1"}),
        ("weknora_create_tenant", {"name": "x"}),
        ("weknora_create_model", {"name": "m"}),
        ("weknora_list_knowledge_bases", {}),
        ("weknora_chat", {"session_id": "s", "query": "q"}),
        ("delete_knowledge_base", {"kb_id": "kb-1"}),
        ("weknora_get_knowledge", {"knowledge_id": "k1"}),
    ]:
        result = outcome(mw, FakeCall(name, args))
        assert denied(result), name
        assert "只读检索类" in result.content or "不在本数字员工可用范围" in result.content, name


def test_non_weknora_tools_are_untouched() -> None:
    mw = middleware(WHITELIST)
    assert outcome(mw, FakeCall("web_search", {"query": "q"})) == "OK"
    assert outcome(mw, FakeCall("other__hybrid_search", {"kb_id": "kb-1"})) == "OK"


def test_governance_message_lists_bases_iff_configured() -> None:
    configured = middleware(WHITELIST)
    assert "销售知识库" in str(configured._governance_message("u1", frozenset()))
    guest = configured._governance_message(None, frozenset())
    assert "kb-1" in str(guest.content)
    legacy = middleware(None)
    assert "知识库" not in str(legacy._governance_message("u1", frozenset()).content)

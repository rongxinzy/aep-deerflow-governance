"""Knowledge-scope enforcement in AepGovernanceMiddleware.

Run (the governance repo has no python env of its own; the DeerFlow backend
venv provides langchain):

    cd DeerFlow/backend && \
    PYTHONPATH=../../aep-deerflow-governance .venv/bin/python -m pytest \
        ../../aep-deerflow-governance/tests/test_knowledge_whitelist.py -q

Two deployed weknora MCP surfaces are covered: the PyPI bridge's kb_id
tools plus its admin surface (default-deny under an explicit whitelist),
and the dsh bridge's knowledge_search(knowledge_base_ids). A missing
policy (no ``knowledge_bases`` kwarg at all) is a configuration error and
denies every knowledge tool with migration guidance for the administrator
(the previous "absent means unrestricted" behavior was removed). These
tests construct the middleware with ``governed_tools=[]`` so the allow
path never touches the network, and drive ``wrap_tool_call`` with minimal
fakes.
"""

from __future__ import annotations

import copy
from typing import Any

from langchain_core.messages import ToolMessage

from aep_deerflow_governance import AepGovernanceMiddleware

NATIVE_MCP_TOOL_NAMES = {
    "create_tenant", "list_tenants", "create_knowledge_base", "list_knowledge_bases",
    "list_shared_knowledge_bases", "get_knowledge_base", "delete_knowledge_base",
    "hybrid_search", "create_knowledge_from_file", "create_knowledge_from_url",
    "create_knowledge_from_text", "update_knowledge_from_text", "list_knowledge",
    "get_knowledge", "delete_knowledge", "create_model", "list_models", "get_model",
    "create_session", "get_session", "list_sessions", "delete_session", "chat",
    "agent_chat", "list_agents", "get_agent", "list_chunks", "delete_chunk",
    "wiki_search", "wiki_read_page", "wiki_index_view",
}

WHITELIST = {"kb-1": "销售知识库", "kb-2": "产品知识库"}


class FakeCall:
    def __init__(self, name: str, args: dict[str, Any], call_id: str = "call-1"):
        self.name = name
        self.args = args
        self.id = call_id


class FakeRuntime:
    def __init__(self, context: dict[str, Any] | None = None):
        self.context = context


class FakeRequest:
    def __init__(self, call: FakeCall, context: dict[str, Any] | None = None):
        self.tool_call = call
        self.runtime = FakeRuntime(context)

    def override(self, **kwargs):
        cloned = copy.copy(self)
        for key, value in kwargs.items():
            setattr(cloned, key, value)
        return cloned


class FakeRequestDict:
    """langgraph shape: tool_call is a TypedDict, not an object."""

    def __init__(self, call: dict[str, Any]):
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


# ------------------------------------------- missing policy (default-deny)
def test_missing_policy_denies_every_knowledge_tool_with_actionable_config_error() -> None:
    # Intentional behavior change (was: absent policy = legacy unrestricted
    # bridge credential). A missing policy is a configuration error: every
    # weknora tool is denied before the handler runs, whatever its shape.
    mw = middleware(None)
    reached: list[str] = []

    def call(name: str, args: dict[str, Any]) -> ToolMessage | str:
        return mw.wrap_tool_call(
            FakeRequest(FakeCall(name, args)),
            handler=lambda request: reached.append(name) or "OK",
        )

    for name, args in (
        ("weknora_hybrid_search", {"kb_id": "kb-1", "query": "q"}),
        ("weknora__hybrid_search", {"kb_id": "kb-1", "query": "q"}),
        ("mcp__weknora__hybrid_search", {"kb_id": "kb-1", "query": "q"}),
        ("hybrid_search", {"kb_id": "kb-1", "query": "q"}),
        ("knowledge_search", {"query": "q", "knowledge_base_ids": ["kb-1"]}),
        ("mcp__weknora__delete_knowledge_base", {"kb_id": "kb-1"}),
        ("weknora_create_knowledge_from_text", {"kb_id": "kb-1", "content": "x"}),
        ("weknora_list_knowledge_bases", {}),
        ("mcp__weknora__get_knowledge", {"knowledge_id": "k1"}),
        ("mcp__weknora__wiki_read_page", {"kb_id": "kb-1", "slug": "entity/a"}),
        ("weknora_some_future_admin_tool", {"kb_id": "kb-1"}),  # unknown sensitive tool
    ):
        result = call(name, args)
        assert denied(result), name
        assert result.content.startswith(f"DENIED {name}:"), name
        assert "知识库访问策略缺失" in result.content, name
        assert "spec.knowledge.bases" in result.content, name
    assert reached == []


def test_missing_policy_leaves_non_knowledge_tools_untouched() -> None:
    mw = middleware(None)
    assert outcome(mw, FakeCall("web_search", {"query": "q"})) == "OK"
    assert outcome(mw, FakeCall("other__hybrid_search", {"kb_id": "kb-1"})) == "OK"
    assert outcome(mw, FakeCall("mcp__weather__hybrid_search", {})) == "OK"
    assert outcome(mw, FakeCall("knowledge_benchmark_runner", {"kb_id": "kb-1"})) == "OK"


def test_missing_policy_denies_before_the_online_context_gate() -> None:
    # Default-deny is config-driven and must win over the Portal run-context
    # check: the run has no knowledge context here, yet the administrator
    # gets the configuration error, not "context unavailable".
    mw = middleware(None)
    mw.knowledge_online_authorization = True
    reached: list[bool] = []
    result = mw.wrap_tool_call(FakeRequest(FakeCall("mcp__weknora__hybrid_search", {"kb_id": "kb-1"})),
                               handler=lambda request: reached.append(True) or "OK")
    assert denied(result)
    assert "知识库访问策略缺失" in result.content
    assert "context unavailable" not in result.content
    assert not reached


def test_missing_policy_is_enforced_for_dict_shaped_tool_calls() -> None:
    mw = middleware(None)
    request = FakeRequestDict({"name": "mcp__weknora__hybrid_search", "args": {"kb_id": "kb-1"}, "id": "c1"})
    result = mw.wrap_tool_call(request, handler=lambda req: "OK")
    assert denied(result)
    assert "spec.knowledge.bases" in result.content


def test_missing_policy_note_forbids_attempts_and_names_the_config_key() -> None:
    mw = middleware(None)
    note = mw._knowledge_note()
    assert note != ""
    assert "知识库" in note
    assert "spec.knowledge.bases" in note
    assert "可用的知识库" not in note  # never advertise a usable set
    prompt = str(mw._governance_message("u1", frozenset()).content)
    assert "spec.knowledge.bases" in prompt


# ------------------------------------------------------------- kb_id read
def test_kb_id_read_tools_pass_with_in_whitelist_id() -> None:
    mw = middleware(WHITELIST)
    for name in ("weknora_hybrid_search", "weknora__hybrid_search", "hybrid_search",
                 "mcp__weknora__hybrid_search",  # ToolNode execution name
                 "weknora_wiki_search", "weknora_list_knowledge", "weknora_get_knowledge_base"):
        assert outcome(mw, FakeCall(name, {"kb_id": "kb-2", "query": "q"})) == "OK", name


def test_registry_covers_every_native_mcp_tool_and_denies_bare_writes() -> None:
    assert NATIVE_MCP_TOOL_NAMES <= set(AepGovernanceMiddleware._WEKNORA_TOOL_REGISTRY)
    mw = middleware(WHITELIST)
    for name in ("list_shared_knowledge_bases", "create_knowledge_from_text", "update_knowledge_from_text"):
        assert denied(outcome(mw, FakeCall(name, {"kb_id": "kb-1", "content": "x"}))), name


def test_standard_server_aliases_of_unclassified_or_write_tools_are_denied() -> None:
    mw = middleware(WHITELIST)
    for name in (
        "weknora__list_shared_knowledge_bases",
        "mcp__weknora__list_shared_knowledge_bases",
        "weknora_create_knowledge_from_text",
        "mcp__weknora__create_knowledge_from_text",
        "weknora__update_knowledge_from_text",
        "mcp__weknora__update_knowledge_from_text",
    ):
        assert denied(outcome(mw, FakeCall(name, {"kb_id": "kb-1", "content": "x"}))), name


def test_kb_id_missing_or_out_of_whitelist_is_denied() -> None:
    mw = middleware(WHITELIST)
    assert denied(outcome(mw, FakeCall("weknora_hybrid_search", {"query": "q"})))
    assert denied(outcome(mw, FakeCall("weknora_hybrid_search", {"kb_id": "kb-x", "query": "q"})))
    assert denied(outcome(mw, FakeCall("weknora_hybrid_search", {"knowledge_base_id": "kb-x", "query": "q"})))
    assert denied(outcome(mw, FakeCall("hybrid_search", {"kb_id": ""})))


def test_wiki_read_page_keeps_and_checks_kb_scope() -> None:
    mw = middleware(WHITELIST)
    assert outcome(mw, FakeCall("mcp__weknora__wiki_read_page", {"kb_id": "kb-1", "slug": "entity/a"})) == "OK"
    assert denied(outcome(mw, FakeCall("mcp__weknora__wiki_read_page", {"kb_id": "kb-x", "slug": "entity/a"})))
    assert denied(outcome(mw, FakeCall("mcp__weknora__wiki_read_page", {"slug": "entity/a"})))


def test_empty_whitelist_denies_every_read() -> None:
    mw = middleware({})
    result = outcome(mw, FakeCall("weknora_hybrid_search", {"kb_id": "kb-1", "query": "q"}))
    assert denied(result)
    assert "未连接任何知识库" in result.content


def test_empty_whitelist_denies_opaque_reads_even_with_online_context() -> None:
    mw = middleware({})
    mw.knowledge_online_authorization = True
    for name in ("get_knowledge", "list_chunks"):
        result = outcome(mw, FakeCall(name, {"knowledge_id": "doc-1"}))
        assert denied(result), name
        assert "deny-all whitelist" in result.content


def test_opaque_document_reads_require_online_portal_attribution() -> None:
    mw = middleware(WHITELIST)
    for name, args in (
        ("get_knowledge", {"knowledge_id": "doc-1"}),
        ("list_chunks", {"knowledge_id": "doc-1"}),
        ("read_document", {"document_id": "doc-1"}),
    ):
        result = outcome(mw, FakeCall(name, args))
        assert denied(result), name
        assert "opaque document handles require online Portal authorization" in result.content


# ----------------------------------------------- knowledge_base_ids (dsh)
def test_knowledge_search_list_ids_still_enforced() -> None:
    mw = middleware(WHITELIST)
    assert outcome(mw, FakeCall("knowledge_search", {"query": "q", "knowledge_base_ids": ["kb-1", "kb-2"]})) == "OK"
    assert outcome(mw, FakeCall("weknora__knowledge_search", {"query": "q", "knowledge_base_ids": ["kb-1"]})) == "OK"
    assert denied(outcome(mw, FakeCall("knowledge_search", {"query": "q"})))
    assert denied(outcome(mw, FakeCall("knowledge_search", {"query": "q", "knowledge_base_ids": []})))
    assert denied(outcome(mw, FakeCall("knowledge_search", {"query": "q", "knowledge_base_ids": ["kb-1", "kb-x"]})))


# ------------------------------------------------- admin surface default-deny
def test_read_write_and_delete_are_separately_authorized() -> None:
    # Under an explicit policy the read tools are the only allow path; writes
    # and deletes on the very same in-whitelist kb stay denied.
    mw = middleware(WHITELIST)
    for name in ("weknora_hybrid_search", "weknora_wiki_search", "weknora_list_knowledge",
                 "weknora_get_knowledge_base"):
        assert outcome(mw, FakeCall(name, {"kb_id": "kb-1", "query": "q"})) == "OK", name
    for name in ("weknora_create_knowledge_from_text", "weknora_create_knowledge_from_file",
                 "weknora_create_knowledge_from_url", "weknora_update_knowledge_from_text",
                 "weknora_delete_knowledge", "weknora_delete_knowledge_base", "weknora_delete_chunk"):
        assert denied(outcome(mw, FakeCall(name, {"kb_id": "kb-1", "content": "x"}))), name


def test_revoked_base_denies_new_calls_immediately() -> None:
    # After a reconcile drops a kb from the whitelist, new calls on it are
    # denied at once (the model-visible prompt note is refreshed per run).
    mw = middleware(WHITELIST)
    assert outcome(mw, FakeCall("weknora_hybrid_search", {"kb_id": "kb-1", "query": "q"})) == "OK"
    mw.knowledge_bases = {"kb-2": "产品知识库"}
    assert denied(outcome(mw, FakeCall("weknora_hybrid_search", {"kb_id": "kb-1", "query": "q"})))
    assert denied(outcome(mw, FakeCall("knowledge_search", {"query": "q", "knowledge_base_ids": ["kb-1"]})))
    assert "kb-1" not in mw._knowledge_note()


def test_unknown_weknora_tools_are_denied_under_explicit_and_deny_all_policies() -> None:
    for policy in (WHITELIST, {}):
        mw = middleware(policy)
        for name in ("weknora_future_tool", "mcp__weknora__future_tool", "weknora__privileged_export"):
            result = outcome(mw, FakeCall(name, {"kb_id": "kb-1"}))
            assert denied(result), (policy, name)
            assert result.content.startswith(f"DENIED {name}:"), name


def test_admin_and_session_tools_are_denied_under_policy() -> None:
    mw = middleware(WHITELIST)
    for name, args in [
        ("weknora_delete_knowledge_base", {"kb_id": "kb-1"}),
        ("weknora_create_tenant", {"name": "x"}),
        ("weknora_create_model", {"name": "m"}),
        ("weknora_list_knowledge_bases", {}),
        ("weknora_chat", {"session_id": "s", "query": "q"}),
        ("delete_knowledge_base", {"kb_id": "kb-1"}),
        ("mcp__weknora__delete_knowledge_base", {"kb_id": "kb-1"}),
        ("mcp__weknora__list_knowledge_bases", {}),
        ("weknora_get_knowledge", {"knowledge_id": "k1"}),
    ]:
        result = outcome(mw, FakeCall(name, args))
        assert denied(result), name
        if "list_knowledge_bases" in name:
            assert "tenant-wide catalog reads are denied" in result.content
            continue
        if "get_knowledge" in name:
            assert result.content.startswith(f"DENIED {name}:")
            continue
        assert "只读检索类" in result.content or "不在本数字员工可用范围" in result.content, name


def test_non_weknora_tools_are_untouched() -> None:
    mw = middleware(WHITELIST)
    assert outcome(mw, FakeCall("web_search", {"query": "q"})) == "OK"
    assert outcome(mw, FakeCall("other__hybrid_search", {"kb_id": "kb-1"})) == "OK"
    assert outcome(mw, FakeCall("mcp__weather__hybrid_search", {})) == "OK"


def test_langgraph_dict_shaped_tool_calls_are_enforced() -> None:
    # langgraph hands ToolCallRequest.tool_call as a TypedDict, not an
    # object — the live runtime bypass that shipped before this regression
    # test read getattr(call, "name") off the dict and matched nothing.
    mw = middleware(WHITELIST)

    def dict_outcome(name: str, args: dict[str, Any]) -> ToolMessage | str:
        request = FakeRequestDict({"name": name, "args": args, "id": "c1"})
        return mw.wrap_tool_call(request, handler=lambda req: "OK")

    assert dict_outcome("weknora_hybrid_search", {"kb_id": "kb-1", "query": "q"}) == "OK"
    denied_call = dict_outcome("weknora_hybrid_search", {"kb_id": "kb-x", "query": "q"})
    assert denied(denied_call)
    assert denied(dict_outcome("mcp__weknora__delete_knowledge_base", {"kb_id": "kb-1"}))


def test_context_injection_drops_model_selected_knowledge_action() -> None:
    mw = middleware(WHITELIST)
    mw.knowledge_online_authorization = True
    seen: dict[str, Any] = {}
    request = FakeRequest(
        FakeCall("mcp__weknora__hybrid_search", {
            "kb_id": "kb-1", "query": "q", "knowledge_action": "delete_knowledge_base",
        }),
        {"zhiyuan_knowledge_context": "A" * 42 + "_", "zhiyuan_employee_name": "employee-1"},
    )
    result = mw.wrap_tool_call(request, handler=lambda enriched: seen.update(enriched.tool_call["args"]) or "OK")
    assert result == "OK"
    assert seen["knowledge_employee_name"] == "employee-1"
    assert seen["knowledge_context_handle"] == "A" * 42 + "_"
    assert "knowledge_action" not in seen


def test_governance_message_lists_bases_iff_configured() -> None:
    configured = middleware(WHITELIST)
    assert "销售知识库" in str(configured._governance_message("u1", frozenset()))
    guest = configured._governance_message(None, frozenset())
    assert "kb-1" in str(guest.content)
    deny_all = middleware({})
    assert "未连接任何知识库" in str(deny_all._governance_message("u1", frozenset()).content)
    # Missing policy used to render no note at all (legacy unrestricted); it
    # now renders the missing-policy notice (intentional behavior change).
    missing = middleware(None)
    prompt = str(missing._governance_message("u1", frozenset()).content)
    assert "知识库" in prompt
    assert "spec.knowledge.bases" in prompt
    guest_prompt = str(missing._governance_message(None, frozenset()).content)
    assert "spec.knowledge.bases" in guest_prompt

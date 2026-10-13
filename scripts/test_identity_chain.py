"""Offline sanity test for the governance middleware identity chain.

Run:  python3 scripts/test_identity_chain.py

Stubs langchain + deerflow, then exercises resolve_aep_user:
  no requester -> fail closed (no default fallback) -> valid-AEP-id passthrough
  (web chat path) -> positive cache -> unknown id fails closed -> user_map
  priority -> IM fail closed without identity source -> resolved requester is
  published on the run context for downstream (memory) consumers.
"""

import os
import sys
import threading
import types

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

for name, attrs in {
    "langchain": {},
    "langchain.agents": {},
    "langchain.agents.middleware": {"AgentMiddleware": type("AgentMiddleware", (), {"__init__": lambda self, **k: None})},
    "langchain_core": {},
    "langchain_core.messages": {
        "SystemMessage": type("SystemMessage", (), {"__init__": lambda self, content: setattr(self, "content", content)}),
        "ToolMessage": type("ToolMessage", (), {"__init__": lambda self, content=None, tool_call_id=None: None}),
    },
    "deerflow": {},
    "deerflow.runtime": {},
    "deerflow.runtime.user_context": {},
    "deerflow.agents": {},
    "deerflow.agents.middlewares": {},
    "deerflow.agents.middlewares.dynamic_context_middleware": {
        "DynamicContextMiddleware": type("DynamicContextMiddleware", (), {}),
    },
}.items():
    m = types.ModuleType(name)
    for k, v in attrs.items():
        setattr(m, k, v)
    sys.modules[name] = m

_holder = {"user": None}
_requester_published: list = []
sys.modules["deerflow.runtime.user_context"].get_current_user = lambda: _holder["user"]
sys.modules["deerflow.runtime.user_context"].set_requester_id = _requester_published.append

from aep_deerflow_governance import AepGovernanceMiddleware  # noqa: E402


class FakeClient:
    def __init__(self):
        self.calls = []

    def token(self):
        return "t"

    def data_scope(self, uid):
        self.calls.append(uid)
        if uid == "aep-real":
            return {"orgScope": ["t1"]}
        raise RuntimeError("no such user")


class User:
    def __init__(self, uid):
        self.id = uid


class RuntimeCtx:
    def __init__(self, **context):
        self.context = dict(context)


def make_mw() -> AepGovernanceMiddleware:
    mw = AepGovernanceMiddleware.__new__(AepGovernanceMiddleware)
    mw.client = FakeClient()
    mw.default_aep_user = "default-uid"
    mw.user_map = {}
    mw.identities = None
    mw.cache_seconds = 60
    mw._scope_cache = {}
    mw._aep_user_cache = {}
    mw._cache_lock = threading.Lock()
    return mw


def main() -> None:
    mw = make_mw()

    _holder["user"] = None
    assert mw.resolve_aep_user(None) is None, "no requester -> fail closed (no default fallback)"

    _holder["user"] = User("aep-real")
    assert mw.resolve_aep_user(None) == "aep-real", "valid aep id passthrough"
    assert mw.client.calls.count("aep-real") == 1

    mw.resolve_aep_user(None)
    assert mw.client.calls.count("aep-real") == 1, "positive cache hit"

    _holder["user"] = User("ghost")
    assert mw.resolve_aep_user(None) is None, "unknown user -> fail closed (no default fallback)"

    mw.user_map = {"local-7": "aep-real"}
    _holder["user"] = User("local-7")
    assert mw.resolve_aep_user(None) == "aep-real", "user_map priority"
    assert mw.client.calls.count("aep-real") == 1, "user_map needs no api call"

    assert mw.resolve_aep_user(RuntimeCtx(channel_user_id="wx_123")) is None, "IM fail closed without identity source"

    # The resolved requester is published on the run context for downstream
    # consumers (memory); "unresolved" is published too, not silently dropped.
    _holder["user"] = User("aep-real")
    rt = RuntimeCtx()
    assert mw.resolve_aep_user(rt) == "aep-real"
    assert rt.context.get("requester_id") == "aep-real", "resolved requester published"

    _holder["user"] = None
    rt2 = RuntimeCtx()
    assert mw.resolve_aep_user(rt2) is None
    assert rt2.context.get("requester_id") is None, "unresolved requester published as None"
    assert _requester_published[-1] is None, "unresolved requester published on the contextvar too"

    # before_agent must resolve + publish (memory's first recall runs in another
    # middleware's before_agent, i.e. earlier than the model call).
    _holder["user"] = User("aep-real")
    rt3 = RuntimeCtx()
    assert mw.before_agent({}, rt3) is None, "before_agent resolves without mutating state"
    assert rt3.context.get("requester_id") == "aep-real", "before_agent published the requester"

    # Ordering: governance runs before the middleware that performs the first
    # memory recall, so the requester exists by then.
    anchor = getattr(AepGovernanceMiddleware, "_prev_anchor", None)
    assert anchor is not None, "governance declares a before-anchor"
    assert getattr(anchor, "__name__", "") == "DynamicContextMiddleware", f"before DynamicContextMiddleware, got {anchor}"

    print("ALL-IDENTITY-CHAIN-TESTS-PASS")


if __name__ == "__main__":
    main()

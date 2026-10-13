"""Offline sanity test for the governance middleware identity chain.

Run:  python3 scripts/test_identity_chain.py

Stubs langchain + deerflow, then exercises resolve_aep_user:
  default fallback -> valid-AEP-id passthrough (web chat path) ->
  positive cache -> invalid id falls back -> user_map priority ->
  IM fail-closed without identity source.
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
}.items():
    m = types.ModuleType(name)
    for k, v in attrs.items():
        setattr(m, k, v)
    sys.modules[name] = m

_holder = {"user": None}
sys.modules["deerflow.runtime.user_context"].get_current_user = lambda: _holder["user"]

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
    context = {"channel_user_id": "wx_123"}


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

    assert mw.resolve_aep_user(RuntimeCtx()) is None, "IM fail closed without identity source"

    print("ALL-IDENTITY-CHAIN-TESTS-PASS")


if __name__ == "__main__":
    main()

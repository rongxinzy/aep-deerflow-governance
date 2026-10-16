"""C1 regression: the governance identity chain fails closed, and the
requester it hands to the memory chain is never the instance default identity.

``docs/identity-fail-closed-inventory.md`` records the dependency inventory
this file pins:

* gov#63 removed the last-line fallback to ``default_aep_user``
  (``aep_deerflow_governance/__init__.py`` ``resolve_aep_user``): every branch
  must return ``None`` when the requester cannot be resolved;
* ``_publish_requester()`` must publish that ``None`` -- the memory chain's
  input -- instead of substituting an identity (deer-flow's
  ``openviking_manager._resolve_scope`` treats a missing requester as
  fail-closed; it never sees the default user because governance does not
  invent one);
* an unresolved requester must be denied by the governed-tool gate and must
  carry an empty scope.

The memory chain itself lives in ``rongxinzy/deer-flow``; this file covers the
boundary the governance package owns.

Run: python -m pytest tests/test_identity_fail_closed.py -q
"""

from __future__ import annotations

import sys
import types
import urllib.error
from typing import Any

import pytest
from langchain_core.messages import ToolMessage

from aep_deerflow_governance import AepGovernanceMiddleware, _IdentityResolver

DEFAULT_AEP_USER = "owner-aep"
"""The instance default identity: the employee owner's AEP user id
(``governance.defaultAEPUser`` is set to the owner by the portal, so it is a
real, resolvable AEP user -- which is exactly why it must never be reachable
as a requester by accident)."""


class FakeAepClient:
    """Stands in for ``_AepClient``: ``data_scope`` succeeds only for the
    users the test declares known."""

    def __init__(self, known: dict[str, list[str]] | None = None):
        self.known = known if known is not None else {"aep-real": ["t1"]}
        self.calls: list[str] = []

    def token(self) -> str:
        return "t"

    def data_scope(self, user_id: str) -> dict[str, Any]:
        self.calls.append(user_id)
        if user_id in self.known:
            return {"orgScope": list(self.known[user_id])}
        raise RuntimeError(f"AEP does not know {user_id!r}")


class User:
    def __init__(self, uid: str):
        self.id = uid


class Runtime:
    def __init__(self, **context: Any):
        self.context = dict(context)


class ToolRequest:
    def __init__(self, name: str, args: dict[str, Any], **context: Any):
        self.tool_call = {"name": name, "args": args, "id": "call-1"}
        self.runtime = Runtime(**context)


@pytest.fixture()
def user_context(monkeypatch: pytest.MonkeyPatch):
    """Stand in for ``deerflow.runtime.user_context`` (not installed here).

    It is the second half of the chain: ``get_current_user()`` feeds the
    runtime-user branch and ``set_requester_id()`` is how governance publishes
    the trusted requester to the memory read/write path.
    """
    published: list[str | None] = []
    holder: dict[str, Any] = {"user": None}
    for name in ("deerflow", "deerflow.runtime", "deerflow.runtime.user_context"):
        monkeypatch.setitem(sys.modules, name, types.ModuleType(name))
    context_module = sys.modules["deerflow.runtime.user_context"]
    context_module.get_current_user = lambda: holder["user"]
    context_module.set_requester_id = published.append
    return holder, published


def middleware(client: FakeAepClient | None = None) -> AepGovernanceMiddleware:
    instance = AepGovernanceMiddleware(
        aep_base_url="http://unused.invalid",
        service_username="svc",
        service_password="pw",
        default_aep_user=DEFAULT_AEP_USER,
        governed_tools=["dept_revenue"],
        team_names={"t1": "研发部"},
    )
    instance.client = client or FakeAepClient()
    instance.identities = None
    return instance


# --------------------------------------------------------------- governance chain
def test_missing_runtime_user_fails_closed(user_context) -> None:
    holder, published = user_context
    instance = middleware()
    runtime = Runtime()

    holder["user"] = None
    assert instance.resolve_aep_user(runtime) is None
    assert runtime.context["requester_id"] is None
    assert published[-1] is None, "unresolved requester must reach memory as None"


def test_unknown_runtime_user_fails_closed(user_context) -> None:
    holder, published = user_context
    instance = middleware()
    runtime = Runtime()

    holder["user"] = User("ghost")
    assert instance.resolve_aep_user(runtime) is None
    assert runtime.context["requester_id"] is None
    assert published[-1] is None
    assert DEFAULT_AEP_USER not in published, "the default identity must never be substituted"


@pytest.mark.parametrize("user_id", [None, "", "ghost", "default"])
def test_default_identity_is_never_returned_on_failure(user_context, user_id: str | None) -> None:
    holder, published = user_context
    instance = middleware()
    runtime = Runtime()

    holder["user"] = User(user_id) if user_id else None
    assert instance.resolve_aep_user(runtime) is None
    assert runtime.context["requester_id"] is None
    assert published[-1] is None


def test_im_sender_without_identity_source_fails_closed(user_context) -> None:
    _, published = user_context
    instance = middleware()
    runtime = Runtime(channel_user_id="wx_123")

    assert instance.identities is None
    assert instance.resolve_aep_user(runtime) is None
    assert runtime.context["requester_id"] is None
    assert published[-1] is None


def test_im_identity_source_outage_fails_closed(user_context, monkeypatch: pytest.MonkeyPatch) -> None:
    """An identity-mapping load failure must resolve nobody, not everybody."""
    _, published = user_context
    instance = middleware()

    def unavailable(*_args: Any, **_kwargs: Any):
        raise urllib.error.URLError("identity source unavailable")

    monkeypatch.setattr("urllib.request.urlopen", unavailable)
    instance.identities = _IdentityResolver(instance.client, "gov-source", 30)
    runtime = Runtime(channel_user_id="wx_123")

    assert instance.identities.resolve("wx_123") is None
    assert instance.resolve_aep_user(runtime) is None
    assert runtime.context["requester_id"] is None
    assert published[-1] is None


def test_before_agent_publishes_the_unresolved_requester(user_context) -> None:
    """Memory's first recall happens in another middleware's ``before_agent``:
    the unpublished/None distinction has to be in place by then."""
    holder, published = user_context
    instance = middleware()
    runtime = Runtime()

    holder["user"] = User("ghost")
    assert instance.before_agent({}, runtime) is None
    assert runtime.context["requester_id"] is None
    assert published[-1] is None


def test_valid_requester_is_still_resolved_and_published(user_context) -> None:
    """Positive control: fail-closed must not break the working paths."""
    holder, published = user_context
    instance = middleware()
    runtime = Runtime()

    holder["user"] = User("aep-real")
    assert instance.resolve_aep_user(runtime) == "aep-real"
    assert runtime.context["requester_id"] == "aep-real"
    assert published[-1] == "aep-real"
    assert instance.client.calls.count("aep-real") == 1

    # Second call is served by the positive cache (no new AEP round-trip).
    assert instance.resolve_aep_user(runtime) == "aep-real"
    assert instance.client.calls.count("aep-real") == 1


def test_user_map_hit_is_resolved(user_context) -> None:
    holder, published = user_context
    instance = middleware()
    instance.user_map = {"local-7": "aep-real"}
    runtime = Runtime()

    holder["user"] = User("local-7")
    assert instance.resolve_aep_user(runtime) == "aep-real"
    assert runtime.context["requester_id"] == "aep-real"
    assert published[-1] == "aep-real"
    assert instance.client.calls == [], "a configured mapping needs no AEP round-trip"


def test_unresolved_requester_is_denied_by_governed_tools(user_context) -> None:
    holder, _ = user_context
    instance = middleware()
    reached: list[bool] = []

    holder["user"] = User("ghost")
    result = instance.wrap_tool_call(
        ToolRequest("dept_revenue", {"dept": "研发部"}),
        handler=lambda _request: reached.append(True) or "should-not-run",
    )
    assert isinstance(result, ToolMessage)
    assert not reached
    assert "访客" in result.content


def test_unresolved_requester_has_an_empty_scope(user_context) -> None:
    holder, _ = user_context
    instance = middleware()

    assert instance.visible_teams(None) == frozenset()
    holder["user"] = User("ghost")
    assert instance.resolve_aep_user(Runtime()) is None
    assert instance.visible_teams("ghost") == frozenset(), "an unresolvable scope sees nothing"


# ------------------------------------------------------------------ residual (A)
def test_residual_runtime_user_equal_to_instance_default_is_trusted(user_context) -> None:
    """Records, and deliberately pins, the residual found by the C1 inventory.

    ``governance.defaultAEPUser`` is the employee owner's AEP user id, and the
    runtime-user branch trusts any id AEP accepts -- so the removed fallback is
    observable again whenever a server-side path stamps that id as the run's
    user. Fixed? No: rejecting the id here would also deny the owner's own
    portal chat (the portal stamps the caller's AEP user id). The safe fix
    needs a cross-repo carrier that separates a portal-stamped AEP identity
    from a legacy/default one; see ``docs/identity-fail-closed-inventory.md``
    section 4. If that fix lands, this test flips to ``is None``.
    """
    holder, published = user_context
    instance = middleware(FakeAepClient({DEFAULT_AEP_USER: ["t1"]}))
    runtime = Runtime()

    holder["user"] = User(DEFAULT_AEP_USER)
    assert instance.resolve_aep_user(runtime) == DEFAULT_AEP_USER
    assert runtime.context["requester_id"] == DEFAULT_AEP_USER
    assert published[-1] == DEFAULT_AEP_USER

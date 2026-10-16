"""Guest email denials happen before either synchronous or asynchronous handlers."""

import asyncio
from types import SimpleNamespace

import pytest
from langchain_core.messages import ToolMessage

from aep_deerflow_governance import AepGovernanceMiddleware


@pytest.mark.parametrize("name", ["email_list", "email_read", "email_send"])
@pytest.mark.parametrize("asynchronous", [False, True])
@pytest.mark.parametrize("requester", [None, "mapped-user"])
def test_employee_mailbox_requires_resolved_requester(monkeypatch, name, asynchronous, requester):
    monkeypatch.delenv("EMAIL_MAILBOXES", raising=False)
    mw = AepGovernanceMiddleware("https://unused.invalid", "svc", "test", "owner")
    mw.identities = SimpleNamespace(resolve=lambda external: requester)
    request = SimpleNamespace(
        tool_call={"name": name, "args": {}, "id": "mail-call"},
        runtime=SimpleNamespace(context={"channel_user_id": "external"}),
    )
    executed = []

    def handler(req):
        executed.append(req)
        return "MAIL SENT OR READ"

    async def ahandler(req):
        return handler(req)

    result = (asyncio.run(mw.awrap_tool_call(request, ahandler)) if asynchronous
              else mw.wrap_tool_call(request, handler))
    if requester is None:
        assert isinstance(result, ToolMessage)
        assert result.tool_call_id == "mail-call"
        assert "DENIED" in result.content
        assert executed == []
    else:
        assert result == "MAIL SENT OR READ"
        assert executed == [request]


def test_unmapped_sender_can_still_use_general_tools(monkeypatch):
    monkeypatch.delenv("EMAIL_MAILBOXES", raising=False)
    mw = AepGovernanceMiddleware("https://unused.invalid", "svc", "test", "owner")
    request = SimpleNamespace(tool_call={"name": "calculator", "id": "general", "args": {}})
    assert mw.wrap_tool_call(request, lambda req: "42") == "42"


def test_mailbox_identity_gate_preserves_additional_configured_scope(monkeypatch):
    monkeypatch.delenv("EMAIL_MAILBOXES", raising=False)
    mw = AepGovernanceMiddleware("https://unused.invalid", "svc", "test", "owner",
                                 governed_tools=["email_send"])
    mw.identities = SimpleNamespace(resolve=lambda external: "mapped-user")
    monkeypatch.setattr(mw, "visible_teams", lambda user: frozenset())
    request = SimpleNamespace(
        tool_call={"name": "email_send", "args": {"dept": "outside"}, "id": "mail-call"},
        runtime=SimpleNamespace(context={"channel_user_id": "external"}),
    )
    assert isinstance(mw.wrap_tool_call(request, lambda req: pytest.fail("executed")), ToolMessage)

"""Employee mailbox tools: build gating, list/read/send behavior, security pins.

Run (governance repo has no python env of its own; the DeerFlow backend
venv provides langchain):

    cd DeerFlow/backend && \
    PYTHONPATH=../../aep-deerflow-governance .venv/bin/python -m pytest \
        ../../aep-deerflow-governance/tests/test_email_tools.py -q
"""
from __future__ import annotations

import base64
import json
from email.message import EmailMessage
from typing import Any

from aep_deerflow_governance.email_tools import build_email_tools

GOOD = json.dumps([{
    "label": "工作邮箱", "address": "bot@corp.com",
    "imap": {"host": "imap.corp.com", "port": 993, "tls": True},
    "smtp": {"host": "smtp.corp.com", "port": 465, "tls": True},
    "username": "bot", "password": "secret-pass",
}, {
    "label": "个人邮箱", "address": "bot@per.com",
    "imap": {"host": "imap.per.com", "port": 993, "tls": True},
    "smtp": {"host": "smtp.per.com", "port": 587, "tls": False},
    "username": "bot2", "password": "secret-pass-2",
}])


def _message(subject: str, from_addr: str, body: str) -> bytes:
    msg = EmailMessage()
    msg["From"] = from_addr
    msg["To"] = "bot@corp.com"
    msg["Subject"] = subject
    msg.set_content(body)
    return bytes(msg)


class FakeImap:
    instances: list["FakeImap"] = []

    def __init__(self) -> None:
        self.mailbox: dict[int, tuple[bytes, bool]] = {
            1: (_message("Q4 目标", "boss@corp.com", "明年 Q4 的目标如下"), False),
            2: (_message("周报提醒", "pm@corp.com", "请提交本周周报，截至周五"), True),
        }
        self.readonly = False
        self.logged_in_as: tuple[str, str] | None = None
        self.fetched_non_peek: list[int] = []
        FakeImap.instances.append(self)

    def login(self, user: str, password: str) -> None:
        self.logged_in_as = (user, password)

    def select(self, box: str, readonly: bool = False) -> None:
        self.readonly = readonly

    def uid(self, command: str, *args: Any) -> tuple[bytes, Any]:
        if command == "search":
            return b"OK", [b" ".join(str(uid).encode() for uid in self.mailbox)]
        if command == "fetch":
            uid = int(args[0])
            if uid not in self.mailbox:
                return b"OK", [None]
            raw, seen = self.mailbox[uid]
            flags = b"(\\Seen)" if seen else b"()"
            if "BODY.PEEK" in args[1]:
                return b"OK", [(flags + b" UID " + str(uid).encode(), raw)]
            self.fetched_non_peek.append(uid)
            return b"OK", [(flags + b" UID " + str(uid).encode(), raw)]
        raise AssertionError(f"unexpected uid command {command}")

    def logout(self) -> None: ...


class FakeSmtp:
    instances: list["FakeSmtp"] = []

    def __init__(self) -> None:
        self.sent: list[Any] = []
        self.logged_in_as: tuple[str, str] | None = None
        FakeSmtp.instances.append(self)

    def login(self, user: str, password: str) -> None:
        self.logged_in_as = (user, password)

    def send_message(self, message: Any) -> None:
        self.sent.append(message)

    def quit(self) -> None: ...


def build(env_value: str | None):
    return build_email_tools(
        {"EMAIL_MAILBOXES": env_value} if env_value is not None else {},
        factories={"imap": lambda box: FakeImap(), "smtp": lambda box: FakeSmtp()},
    )


def names(tools) -> set[str]:
    return {t.name for t in tools}


def test_no_env_no_tools():
    assert build_email_tools({}, factories={"imap": lambda b: FakeImap(), "smtp": lambda b: FakeSmtp()}) == []


def test_malformed_env_disables_tools_without_crashing():
    assert names(build('{"not":"a list"}')) == set()
    assert names(build('[{"label":"x"}]')) == set()


def test_valid_env_registers_three_tools():
    tools = build(GOOD)
    assert names(tools) == {"email_list", "email_read", "email_send"}


def test_port_allowlist():
    bad = json.dumps([{**json.loads(GOOD)[0], "imap": {"host": "i", "port": 1430, "tls": True}}])
    assert names(build(bad)) == set()


def test_list_newest_first_with_flags_and_mailbox_label():
    tools = build(GOOD)
    email_list = next(t for t in tools if t.name == "email_list")
    out = json.loads(email_list.invoke({"mailbox": "工作邮箱"}))
    assert out["mailbox"] == "工作邮箱"
    assert [m["subject"] for m in out["messages"]] == ["周报提醒", "Q4 目标"]
    assert out["messages"][0]["seen"] is True
    assert out["messages"][1]["seen"] is False
    assert "Q4" in out["messages"][1]["snippet"]
    # PEEK 语义：list 不置已读
    assert FakeImap.instances[-1].fetched_non_peek == []


def test_list_unread_only_and_unknown_mailbox():
    tools = build(GOOD)
    email_list = next(t for t in tools if t.name == "email_list")
    out = json.loads(email_list.invoke({"mailbox": "工作邮箱", "unread_only": True}))
    assert [m["subject"] for m in out["messages"]] == ["Q4 目标"]
    error = json.loads(email_list.invoke({"mailbox": "不存在"})) if False else None
    try:
        email_list.invoke({"mailbox": "不存在"})
        raised = False
    except Exception:
        raised = True
    assert raised, "未知信箱必须报错（并列出已配置标签）"


def test_read_returns_body_and_marks_seen():
    tools = build(GOOD)
    email_read = next(t for t in tools if t.name == "email_read")
    out = json.loads(email_read.invoke({"mailbox": "工作邮箱", "uid": 1}))
    assert out["subject"] == "Q4 目标"
    assert "Q4 的目标" in out["bodyText"]
    assert FakeImap.instances[-1].fetched_non_peek == [1], "email_read 非 PEEK：读取即置已读"
    missing = json.loads(email_read.invoke({"mailbox": "工作邮箱", "uid": 99}))
    assert "error" in missing


def test_send_pins_from_and_uses_default_mailbox_credentials():
    tools = build(GOOD)
    email_send = next(t for t in tools if t.name == "email_send")
    out = json.loads(email_send.invoke({"to": "client@ext.com", "subject": "合作确认", "text": "您好"}))
    assert out == {"sent": True, "from": "bot@corp.com", "to": "client@ext.com", "subject": "合作确认"}
    smtp = FakeSmtp.instances[-1]
    assert smtp.logged_in_as == ("bot", "secret-pass"), "缺省=第一个信箱的凭证"
    assert smtp.sent[0]["From"] == "bot@corp.com", "from 锁配置地址"
    # 指定第二信箱
    out2 = json.loads(email_send.invoke({"to": "x@y.com", "subject": "s", "text": "t", "mailbox": "个人邮箱"}))
    assert out2["from"] == "bot@per.com"
    assert FakeSmtp.instances[-1].logged_in_as == ("bot2", "secret-pass-2")

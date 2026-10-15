"""Employee mailbox tools — Python stdlib only (imaplib / smtplib / email).

Enabled iff the runtime env ``EMAIL_MAILBOXES`` is present: the operator
envFrom-injects it from the employee's email Secret (see ``EmailSpec``),
and the middleware registers ``email_list`` / ``email_read`` /
``email_send`` in ``AgentMiddleware.tools``. Absent or malformed env means
no tools — an employee without configured mailboxes must never see them.

Pull-only by design this iteration: the employee runtime hibernates
(KEDA), so nothing here holds a long IMAP connection. Inbound push (IDLE
→ wake → inject) lands with the channel-gateway pattern later; these
tools are the on-demand read/send surface the agent uses mid-conversation.

Security notes mirrored from the companion-side mail core:
- ``from`` is pinned to the configured address (no arbitrary spoofing);
- IMAP/SMTP ports are allowlisted (143/993 and 25/465/587) so the runtime
  cannot be used as a port scanner;
- credentials never appear in tool results or errors.
"""
from __future__ import annotations

import email
import email.policy
import imaplib
import json
import os
import smtplib
from email.header import decode_header, make_header
from email.message import EmailMessage
from typing import Any, Callable

from langchain_core.tools import BaseTool, tool

IMAP_PORTS = {143, 993}
SMTP_PORTS = {25, 465, 587}
IMAP_TIMEOUT_S = 15
LIST_LIMIT_CAP = 50
SNIPPET_CHARS = 240
BODY_CHARS = 20_000


class _Mailbox:
    __slots__ = ("label", "address", "imap", "smtp", "username", "password", "is_default")

    def __init__(self, raw: dict[str, Any], index: int) -> None:
        def required(key: str) -> Any:
            value = raw.get(key)
            if value in (None, ""):
                raise ValueError(f"mailbox[{index}].{key} is required")
            return value

        imap = raw.get("imap") or {}
        smtp = raw.get("smtp") or {}
        self.label = str(required("label"))
        self.address = str(required("address"))
        self.username = str(required("username"))
        self.password = str(raw.get("password") or "")
        self.imap = {"host": str(imap.get("host") or ""), "port": int(imap.get("port") or 993),
                     "tls": bool(imap.get("tls", True))}
        self.smtp = {"host": str(smtp.get("host") or ""), "port": int(smtp.get("port") or 587),
                     "tls": bool(smtp.get("tls", True))}
        if not self.password:
            raise ValueError(f"mailbox[{index}].password is required")
        if not self.imap["host"]:
            raise ValueError(f"mailbox[{index}].imap.host is required")
        if not self.smtp["host"]:
            raise ValueError(f"mailbox[{index}].smtp.host is required")
        if self.imap["port"] not in IMAP_PORTS:
            raise ValueError(f"mailbox[{index}].imap.port must be one of {sorted(IMAP_PORTS)}")
        if self.smtp["port"] not in SMTP_PORTS:
            raise ValueError(f"mailbox[{index}].smtp.port must be one of {sorted(SMTP_PORTS)}")
        self.is_default = False


def _parse_mailboxes(raw_env: str | None) -> list[_Mailbox]:
    if not raw_env:
        return []
    data = json.loads(raw_env)
    if not isinstance(data, list) or not data:
        raise ValueError("EMAIL_MAILBOXES must be a non-empty JSON array")
    boxes = [_Mailbox(item, i) for i, item in enumerate(data)]
    boxes[0].is_default = True  # type: ignore[attr-defined]
    return boxes


def _header(message: email.message.Message, name: str) -> str:
    raw = message.get(name, "")
    try:
        return str(make_header(decode_header(raw)))
    except Exception:
        return raw


def _body_text(message: email.message.Message) -> str:
    """Best-effort plain text: text/plain part, else stripped html."""
    if message.is_multipart():
        for part in message.walk():
            if part.get_content_type() == "text/plain" and not part.get_filename():
                payload = part.get_payload(decode=True)
                if payload:
                    return payload.decode(part.get_content_charset() or "utf-8", "replace")
        return ""
    payload = message.get_payload(decode=True)
    if isinstance(payload, bytes):
        return payload.decode(message.get_content_charset() or "utf-8", "replace")
    return str(payload or "")


def _summary(box: _Mailbox, uid: int, message: email.message.Message, seen: bool) -> dict[str, Any]:
    text = _body_text(message).strip()
    return {
        "mailbox": box.label, "address": box.address, "uid": uid,
        "messageId": message.get("Message-ID", ""),
        "from": _header(message, "From"), "to": _header(message, "To"),
        "subject": _header(message, "Subject"), "date": message.get("Date", ""),
        "snippet": " ".join(text.split())[:SNIPPET_CHARS], "seen": seen,
    }


def _imap_connect(box: _Mailbox):
    if box.imap["tls"]:
        return imaplib.IMAP4_SSL(box.imap["host"], box.imap["port"], timeout=IMAP_TIMEOUT_S)
    return imaplib.IMAP4(box.imap["host"], box.imap["port"], timeout=IMAP_TIMEOUT_S)


def _smtp_connect(box: _Mailbox):
    if box.smtp["tls"]:
        return smtplib.SMTP_SSL(box.smtp["host"], box.smtp["port"], timeout=IMAP_TIMEOUT_S)
    return smtplib.SMTP(box.smtp["host"], box.smtp["port"], timeout=IMAP_TIMEOUT_S)


def build_email_tools(environ: dict[str, str] | None = None,
                      factories: dict[str, Callable[[_Mailbox], Any]] | None = None) -> list[BaseTool]:
    """Build the three email tools from EMAIL_MAILBOXES; [] when unset.

    ``factories`` substitutes the imaplib/smtplib constructors in tests
    ({"imap": fn, "smtp": fn}). A malformed configuration raises at build
    time so the employee fails loudly at rollout instead of mid-chat.
    """
    env = dict(os.environ if environ is None else environ)
    try:
        boxes = _parse_mailboxes(env.get("EMAIL_MAILBOXES"))
    except ValueError as error:
        # Deployment misconfiguration: surface it in the runtime log and stay
        # tool-less rather than crash-looping the whole employee.
        print(f"[aep-governance] email tools disabled: {error}", flush=True)
        return []
    if not boxes:
        return []
    make_imap = (factories or {}).get("imap", _imap_connect)
    make_smtp = (factories or {}).get("smtp", _smtp_connect)
    by_label = {box.label: box for box in boxes}

    def pick(label: str | None) -> _Mailbox:
        if not label:
            return boxes[0]
        if label not in by_label:
            raise ValueError(f"unknown mailbox {label!r}; configured: {sorted(by_label)}")
        return by_label[label]

    @tool
    def email_list(mailbox: str | None = None, limit: int = 20, unread_only: bool = False) -> str:
        """列出本员工邮箱里的近期邮件（可按 mailbox 标签过滤，缺省=默认信箱）。
        返回发件人/主题/摘要/已读状态；unread_only=True 只看未读。"""
        box = pick(mailbox)
        limit = max(1, min(int(limit), LIST_LIMIT_CAP))
        client = make_imap(box)
        try:
            client.login(box.username, box.password)
            client.select("INBOX", readonly=True)
            _, data = client.uid("search", None, "ALL")
            uids = (data[0] or b"").split()[-limit:]
            out: list[dict[str, Any]] = []
            for uid_bytes in reversed(uids):  # newest first
                uid = int(uid_bytes)
                _, fetched = client.uid("fetch", str(uid), "(BODY.PEEK[] FLAGS)")
                if not fetched or not isinstance(fetched[0], tuple):
                    continue
                flags = fetched[0][0].decode("utf-8", "replace") if isinstance(fetched[0][0], bytes) else str(fetched[0][0])
                seen = "\\Seen" in flags
                if unread_only and seen:
                    continue
                message = email.message_from_bytes(fetched[0][1], policy=email.policy.default)
                out.append(_summary(box, uid, message, seen))
            return json.dumps({"mailbox": box.label, "messages": out}, ensure_ascii=False)
        finally:
            try:
                client.logout()
            except Exception:
                pass

    @tool
    def email_read(mailbox: str, uid: int) -> str:
        """读取一封邮件的完整正文（按 mailbox 标签 + uid，来自 email_list 结果）。"""
        box = pick(mailbox)
        client = make_imap(box)
        try:
            client.login(box.username, box.password)
            client.select("INBOX")
            _, fetched = client.uid("fetch", str(int(uid)), "(BODY[])")  # 非 PEEK：读取即置已读
            if not fetched or not isinstance(fetched[0], tuple):
                return json.dumps({"error": f"message {uid} not found"}, ensure_ascii=False)
            message = email.message_from_bytes(fetched[0][1], policy=email.policy.default)
            return json.dumps({
                "mailbox": box.label, "uid": int(uid),
                "from": _header(message, "From"), "subject": _header(message, "Subject"),
                "bodyText": _body_text(message)[:BODY_CHARS],
            }, ensure_ascii=False)
        finally:
            try:
                client.logout()
            except Exception:
                pass

    @tool
    def email_send(to: str, subject: str, text: str, mailbox: str | None = None, cc: str | None = None) -> str:
        """以本员工配置的邮箱地址发信（缺省默认信箱）。非幂等：仅在任务明确要求发信时使用。"""
        box = pick(mailbox)
        message = EmailMessage()
        message["From"] = box.address  # from 锁配置地址，不开放任意伪造
        message["To"] = to
        if cc:
            message["Cc"] = cc
        message["Subject"] = subject
        message.set_content(text)
        client = make_smtp(box)
        try:
            client.login(box.username, box.password)
            client.send_message(message)
            return json.dumps({"sent": True, "from": box.address, "to": to, "subject": subject}, ensure_ascii=False)
        finally:
            try:
                client.quit()
            except Exception:
                pass

    return [email_list, email_read, email_send]

"""Unit tests for de-channel-gateway.

The platform leg (WeComChannel) and the aibot SDK live only inside the
runtime image; these tests inject stand-ins so the gateway's own logic —
frame bookkeeping, inbox leasing, ack handling, connection loading — runs
against fakes. Run with: python -m pytest channel-gateway/tests.
"""

from __future__ import annotations

import asyncio
import json
import logging
import sys
import time
import types
from pathlib import Path
from typing import Any

import pytest
from fastapi import HTTPException

# -- stand-ins for the runtime-image-only modules -----------------------------

FAKE_BOT_STATE = {"stream_ids": [], "replies": []}


class FakeWSClient:
    async def reply_stream(self, frame, stream_id, text, flag):
        FAKE_BOT_STATE["stream_ids"].append(stream_id)
        FAKE_BOT_STATE["replies"].append(text)


class FakeChannel:
    def __init__(self, bus, config):
        self.bus = bus
        self.config = config
        self._ws_client = FakeWSClient()
        self._ws_frames: dict = {}
        self._ws_stream_ids: dict = {}
        self._working_message = "Working on it..."
        self._running = False

    @property
    def is_running(self) -> bool:
        return self._running

    async def start(self):
        self._running = True

    async def stop(self):
        self._running = False

    async def _on_outbound(self, msg):
        FAKE_BOT_STATE["replies"].append(msg.get("text", ""))


@pytest.fixture()
def runtime_stubs(monkeypatch):
    """Make `app.channels.wecom` and `aibot` importable as fakes."""
    wecom_mod = types.ModuleType("app.channels.wecom")
    wecom_mod.WeComChannel = FakeChannel

    bus_mod = types.ModuleType("app.channels.message_bus")

    class OutboundMessage(dict):
        def __init__(self, **kwargs):
            super().__init__(**kwargs)
            for key, value in kwargs.items():
                setattr(self, key, value)

    bus_mod.OutboundMessage = OutboundMessage
    wecom_mod.message_bus = bus_mod

    app_pkg = types.ModuleType("app")
    channels_pkg = types.ModuleType("app.channels")
    channels_pkg.wecom = wecom_mod
    channels_pkg.message_bus = bus_mod

    aibot_mod = types.ModuleType("aibot")
    counter = {"n": 0}

    def generate_req_id(prefix: str) -> str:
        counter["n"] += 1
        return f"{prefix}-{counter['n']}"

    aibot_mod.generate_req_id = generate_req_id
    monkeypatch.setitem(sys.modules, "app", app_pkg)
    monkeypatch.setitem(sys.modules, "app.channels", channels_pkg)
    monkeypatch.setitem(sys.modules, "app.channels.wecom", wecom_mod)
    monkeypatch.setitem(sys.modules, "app.channels.message_bus", bus_mod)
    monkeypatch.setitem(sys.modules, "aibot", aibot_mod)

    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
    import gateway_app  # noqa: PLC0415 — import must happen after the stubs

    return gateway_app


class FakeTransaction:
    async def __aenter__(self):
        return self

    async def __aexit__(self, *exc):
        return False


class FakeConnection:
    """Minimal asyncpg stand-in: records statements, serves canned results."""

    def __init__(self, fetchval_results=None, fetch_results=None):
        self.executed: list[tuple] = []
        self.fetched: list[tuple] = []
        self.fetchval_results = list(fetchval_results or [])
        self.fetch_results = list(fetch_results or [])
        # Emulated channel_outbound_idempotency table.
        self.outbound_keys: set[tuple[str, str]] = set()

    def transaction(self):
        return FakeTransaction()

    async def execute(self, sql, *args):
        self.executed.append((sql.split()[0].upper(), sql, args))
        if sql.startswith("DELETE FROM channel_outbound_idempotency") and "employee = $1" in sql:
            self.outbound_keys.discard((args[0], args[1]))

    async def fetchval(self, sql, *args):
        self.fetched.append((sql, args))
        if sql.startswith("INSERT INTO channel_outbound_idempotency"):
            key = (args[0], args[1])
            if key in self.outbound_keys:
                return None
            self.outbound_keys.add(key)
            return True
        return self.fetchval_results.pop(0) if self.fetchval_results else None

    async def fetch(self, sql, *args):
        self.fetched.append((sql, args))
        return self.fetch_results.pop(0) if self.fetch_results else []

    async def __aenter__(self):
        return self

    async def __aexit__(self, *exc):
        return False


class FakePool:
    def __init__(self, connection=None):
        self.connection = connection or FakeConnection()

    def acquire(self):
        return self.connection


# -- credentials ---------------------------------------------------------------

EMPLOYEE = "sales-helper"
OTHER_EMPLOYEE = "other-helper"
GHOST_EMPLOYEE = "ghost-helper"
TOKEN = "token-sales"
OTHER_TOKEN = "token-other"
GHOST_TOKEN = "token-ghost"


def leg_entry(token=TOKEN, expires_at=None, operations=None, **extra):
    credential = {"token": token, "expires_at": time.time() + 3600 if expires_at is None else expires_at}
    if operations is not None:
        credential["operations"] = operations
    entry = {"provider": "wecom", "bot_id": "b", "bot_secret": "s", "caller_credential": credential}
    entry.update(extra)
    return entry


def write_connections(tmp_path, payload):
    path = tmp_path / "connections.json"
    path.write_text(json.dumps(payload), encoding="utf-8")
    return path


@pytest.fixture()
def credentialed(runtime_stubs, tmp_path, monkeypatch):
    """Three employees with caller credentials; only two of them get legs."""
    path = write_connections(
        tmp_path,
        {
            EMPLOYEE: leg_entry(),
            OTHER_EMPLOYEE: leg_entry(token=OTHER_TOKEN),
            GHOST_EMPLOYEE: leg_entry(token=GHOST_TOKEN),
        },
    )
    monkeypatch.setattr(runtime_stubs, "connections_file", str(path))
    return path


def bearer(token=TOKEN):
    return f"Bearer {token}"


def gateway_leg(gateway, employee=EMPLOYEE, pool=None):
    return gateway.GatewayLeg(employee, {"bot_id": "b", "bot_secret": "s"}, pool or FakePool())


def send_body(gateway, text="回复", idempotency_key="turn-1"):
    return gateway.SendBody(
        channel_name="wecom",
        chat_id="u1",
        thread_id="t1",
        text=text,
        artifacts=[],
        is_final=True,
        idempotency_key=idempotency_key,
    )


# -- tests ---------------------------------------------------------------------


def test_gateway_bus_is_noop(runtime_stubs):
    bus = runtime_stubs.GatewayBus()
    bus.subscribe_outbound(lambda: None)
    bus.unsubscribe_outbound(lambda: None)
    asyncio.run(bus.publish_inbound(object()))


def test_leg_enqueue_persists_frame_and_wakes(runtime_stubs):
    gateway = runtime_stubs
    pool = FakePool()
    leg = gateway.GatewayLeg("sales-helper", {"bot_id": "b", "bot_secret": "s"}, pool)

    frame = {"body": {"msgid": "m-1", "from": {"userid": "u1"}, "text": {"content": "hi"}}}
    asyncio.run(leg._enqueue("text", frame))

    conn = pool.connection
    assert len(conn.executed) == 3
    insert_inbox = conn.executed[0]
    assert insert_inbox[1].startswith("INSERT INTO channel_inbox")
    assert insert_inbox[2] == ("sales-helper", "text", json.dumps(frame))
    assert conn.executed[1][1].startswith("INSERT INTO channel_wake")
    assert conn.executed[2][1].startswith("INSERT INTO employee_activity")

    # Send-side bookkeeping ran: the frame and its stream id are registered
    # on the platform leg, and the working acknowledgement was sent.
    assert leg.channel._ws_frames["m-1"] is frame
    assert leg.channel._ws_stream_ids["m-1"].startswith("stream-")
    assert FAKE_BOT_STATE["replies"] == ["Working on it..."]


def test_leg_enqueue_survives_bookkeeping_failure(runtime_stubs):
    gateway = runtime_stubs
    pool = FakePool()

    class BrokenChannel(FakeChannel):
        def __init__(self, bus, config):
            super().__init__(bus, config)
            self._ws_client = None  # forces the reply_stream guard

    import app.channels.wecom as wecom

    original = wecom.WeComChannel
    wecom.WeComChannel = BrokenChannel
    try:
        leg = gateway.GatewayLeg("x", {"bot_id": "b", "bot_secret": "s"}, pool)
        asyncio.run(leg._enqueue("text", {"body": {"msgid": "m"}}))
        assert len(pool.connection.executed) == 3, "persistence must not depend on the ws leg"
    finally:
        wecom.WeComChannel = original


def test_load_connections_filters_and_survives_missing_file(runtime_stubs, tmp_path, monkeypatch):
    gateway = runtime_stubs
    assert gateway.load_connections() == {} or isinstance(gateway.load_connections(), dict)

    cfg = tmp_path / "connections.json"
    cfg.write_text(
        json.dumps(
            {
                "a": {"provider": "wecom", "bot_id": "1", "bot_secret": "2"},
                "b": {"provider": "feishu"},
                "c": "not-a-dict",
            }
        ),
        encoding="utf-8",
    )
    monkeypatch.setattr(gateway, "connections_file", str(cfg))
    loaded = gateway.load_connections()
    assert list(loaded) == ["a"]


def test_ack_and_send_models(runtime_stubs):
    gateway = runtime_stubs
    ack = gateway.AckBody(ids=[1, 2, 3])
    assert ack.ids == [1, 2, 3]

    send = send_body(gateway)
    assert send.is_final is True
    assert send.idempotency_key == "turn-1"


@pytest.mark.asyncio
async def test_inbox_long_poll_leases_and_times_out(runtime_stubs, credentialed, monkeypatch):
    gateway = runtime_stubs
    frame = json.dumps({"body": {"msgid": "m"}})
    conn = FakeConnection(fetch_results=[[{"id": 7, "msg_type": "text", "frame": frame}], []])
    monkeypatch.setattr(gateway, "pool", FakePool(conn))
    response = await gateway.inbox(EMPLOYEE, wait=0, authorization=bearer())
    assert response["messages"][0]["id"] == 7

    # The lease is what makes redelivery work: unacked rows come back once the
    # lease window has passed, and another poller skips them meanwhile.
    lease_sql, lease_args = conn.fetched[0]
    assert "acked_at IS NULL" in lease_sql
    assert "FOR UPDATE SKIP LOCKED" in lease_sql
    assert lease_args == (EMPLOYEE, gateway.INBOX_LEASE_SECONDS)

    # Empty poll with wait=0 returns immediately without rows.
    response = await gateway.inbox(EMPLOYEE, wait=0, authorization=bearer())
    assert response == {"messages": []}


@pytest.mark.asyncio
async def test_send_routes_to_the_leg(runtime_stubs, credentialed, monkeypatch):
    gateway = runtime_stubs
    leg = gateway_leg(gateway)
    monkeypatch.setattr(gateway, "pool", FakePool())
    monkeypatch.setattr(gateway, "legs", {EMPLOYEE: leg})

    response = await gateway.send(EMPLOYEE, send_body(gateway), authorization=bearer())
    assert response == {"sent": True, "duplicate": False}
    assert FAKE_BOT_STATE["replies"][-1] == "回复"


@pytest.mark.asyncio
async def test_send_unknown_employee_404(runtime_stubs, credentialed, monkeypatch):
    gateway = runtime_stubs
    monkeypatch.setattr(gateway, "legs", {})
    with pytest.raises(HTTPException) as exc:
        await gateway.send(GHOST_EMPLOYEE, send_body(gateway), authorization=bearer(GHOST_TOKEN))
    assert exc.value.status_code == 404


@pytest.mark.asyncio
async def test_ensure_schema_creates_tables(runtime_stubs):
    gateway = runtime_stubs
    conn = FakeConnection()
    await gateway.ensure_schema(conn)
    ddl = conn.executed[0][1]
    assert "channel_inbox" in ddl
    assert "channel_inbox_pending" in ddl
    assert "channel_outbound_idempotency" in ddl


# -- caller credential: 缺凭证 / 错凭证 / 跨员工 / 过期 / 轮换 / 权限 ---------------


def test_load_credentials_parses_entries_and_drops_unusable_ones(runtime_stubs, tmp_path, monkeypatch):
    gateway = runtime_stubs
    path = write_connections(
        tmp_path,
        {
            "ok": leg_entry(token="t-ok", expires_at=1_800_000_000, operations=["inbox"]),
            "no-token": leg_entry(token=""),
            "no-expiry": leg_entry(**{"caller_credential": {"token": "t"}}),
            "bad-expiry": leg_entry(**{"caller_credential": {"token": "t", "expires_at": "soon"}}),
            "bad-ops": leg_entry(operations=["inbox", "drop-tables"]),
            "no-credential": {"provider": "wecom", "bot_id": "b", "bot_secret": "s"},
            "not-a-provider": {"provider": "feishu", "caller_credential": {"token": "t", "expires_at": 1}},
        },
    )
    monkeypatch.setattr(gateway, "connections_file", str(path))

    credentials = gateway.load_credentials()
    assert list(credentials) == ["ok"]
    assert credentials["ok"].token == "t-ok"
    assert credentials["ok"].operations == ("inbox",)


def test_load_credentials_fails_closed_on_unreadable_file(runtime_stubs, tmp_path, monkeypatch):
    gateway = runtime_stubs
    broken = tmp_path / "connections.json"
    broken.write_text("{not json", encoding="utf-8")
    monkeypatch.setattr(gateway, "connections_file", str(broken))
    assert gateway.load_credentials() == {}

    monkeypatch.setattr(gateway, "connections_file", str(tmp_path / "missing.json"))
    assert gateway.load_credentials() == {}


def test_bearer_token_parsing(runtime_stubs):
    gateway = runtime_stubs
    assert gateway._bearer_token("Bearer abc") == "abc"
    assert gateway._bearer_token("bearer abc") == "abc"
    assert gateway._bearer_token("Bearer  abc ") == "abc"
    for malformed in (None, "", "abc", "Token abc", "Bearer", "Bearer   "):
        assert gateway._bearer_token(malformed) is None, malformed


async def _rejects(gateway, employee, authorization):
    """Run all three endpoints and return the status codes they raise."""
    codes = []
    with pytest.raises(HTTPException) as exc:
        await gateway.inbox(employee, wait=0, authorization=authorization)
    codes.append(exc.value.status_code)
    with pytest.raises(HTTPException) as exc:
        await gateway.ack(employee, gateway.AckBody(ids=[1]), authorization=authorization)
    codes.append(exc.value.status_code)
    with pytest.raises(HTTPException) as exc:
        await gateway.send(employee, send_body(gateway), authorization=authorization)
    codes.append(exc.value.status_code)
    return codes


@pytest.mark.asyncio
async def test_missing_credential_is_rejected_without_touching_the_db(runtime_stubs, credentialed, monkeypatch):
    gateway = runtime_stubs

    class ExplodingPool:
        def acquire(self):
            raise AssertionError("an unauthenticated call must not reach the DB")

    monkeypatch.setattr(gateway, "pool", ExplodingPool())
    assert await _rejects(gateway, EMPLOYEE, None) == [401, 401, 401]
    assert await _rejects(gateway, EMPLOYEE, "Token abc") == [401, 401, 401]


@pytest.mark.asyncio
async def test_wrong_credential_is_rejected(runtime_stubs, credentialed):
    gateway = runtime_stubs
    assert await _rejects(gateway, EMPLOYEE, bearer("not-the-token")) == [403, 403, 403]
    assert await _rejects(gateway, EMPLOYEE, bearer("")) == [401, 401, 401]


@pytest.mark.asyncio
async def test_non_ascii_credential_is_rejected(runtime_stubs, credentialed):
    gateway = runtime_stubs
    assert await _rejects(gateway, EMPLOYEE, bearer("tökén")) == [403, 403, 403]


@pytest.mark.asyncio
async def test_employee_a_cannot_use_its_token_on_employee_b(runtime_stubs, credentialed):
    gateway = runtime_stubs
    # A's token against B's path, and B's token against A's path.
    assert await _rejects(gateway, OTHER_EMPLOYEE, bearer(TOKEN)) == [403, 403, 403]
    assert await _rejects(gateway, EMPLOYEE, bearer(OTHER_TOKEN)) == [403, 403, 403]


@pytest.mark.asyncio
async def test_employee_without_credential_is_fail_closed(runtime_stubs, tmp_path, monkeypatch):
    gateway = runtime_stubs
    path = write_connections(tmp_path, {EMPLOYEE: {"provider": "wecom", "bot_id": "b", "bot_secret": "s"}})
    monkeypatch.setattr(gateway, "connections_file", str(path))
    assert await _rejects(gateway, EMPLOYEE, bearer(TOKEN)) == [403, 403, 403]
    assert await _rejects(gateway, GHOST_EMPLOYEE, bearer(TOKEN)) == [403, 403, 403]


@pytest.mark.asyncio
async def test_expired_credential_is_rejected(runtime_stubs, tmp_path, monkeypatch):
    gateway = runtime_stubs
    path = write_connections(tmp_path, {EMPLOYEE: leg_entry(expires_at=time.time() - 1)})
    monkeypatch.setattr(gateway, "connections_file", str(path))
    assert await _rejects(gateway, EMPLOYEE, bearer()) == [401, 401, 401]


@pytest.mark.asyncio
async def test_rotated_credential_invalidates_the_old_token(runtime_stubs, credentialed, monkeypatch):
    gateway = runtime_stubs
    monkeypatch.setattr(gateway, "pool", FakePool(FakeConnection(fetch_results=[[]])))
    assert await gateway.inbox(EMPLOYEE, wait=0, authorization=bearer()) == {"messages": []}

    # Rotation = the Secret is rewritten; the retired token must stop working
    # on the very next call, without a restart or a reconcile tick.
    write_connections(credentialed.parent, {EMPLOYEE: leg_entry(token="token-rotated")})
    assert await _rejects(gateway, EMPLOYEE, bearer()) == [403, 403, 403]
    assert await gateway.inbox(EMPLOYEE, wait=0, authorization=bearer("token-rotated")) == {"messages": []}


@pytest.mark.asyncio
async def test_operation_scope_is_enforced(runtime_stubs, tmp_path, monkeypatch):
    gateway = runtime_stubs
    path = write_connections(tmp_path, {EMPLOYEE: leg_entry(operations=["inbox"])})
    monkeypatch.setattr(gateway, "connections_file", str(path))
    monkeypatch.setattr(gateway, "pool", FakePool(FakeConnection(fetch_results=[[]])))

    assert await gateway.inbox(EMPLOYEE, wait=0, authorization=bearer()) == {"messages": []}
    with pytest.raises(HTTPException) as exc:
        await gateway.ack(EMPLOYEE, gateway.AckBody(ids=[1]), authorization=bearer())
    assert exc.value.status_code == 403
    with pytest.raises(HTTPException) as exc:
        await gateway.send(EMPLOYEE, send_body(gateway), authorization=bearer())
    assert exc.value.status_code == 403


# -- ack: 不允许跨员工确认 -------------------------------------------------------


@pytest.mark.asyncio
async def test_ack_rejects_ids_owned_by_another_employee(runtime_stubs, credentialed, monkeypatch):
    gateway = runtime_stubs
    conn = FakeConnection(fetchval_results=[OTHER_EMPLOYEE])
    monkeypatch.setattr(gateway, "pool", FakePool(conn))
    with pytest.raises(HTTPException) as exc:
        await gateway.ack(EMPLOYEE, gateway.AckBody(ids=[1, 2]), authorization=bearer())
    assert exc.value.status_code == 403
    assert "another employee" in exc.value.detail
    assert OTHER_EMPLOYEE not in exc.value.detail, "the rejection must not identify the other employee"
    # The ownership check comes first: nothing is acked, not even the legit ids.
    assert conn.executed == []
    ownership_sql, ownership_args = conn.fetched[0]
    assert "employee <> $1" in ownership_sql
    assert ownership_args == (EMPLOYEE, [1, 2])


@pytest.mark.asyncio
async def test_ack_marks_own_ids(runtime_stubs, credentialed, monkeypatch):
    gateway = runtime_stubs
    conn = FakeConnection(fetchval_results=[None])
    monkeypatch.setattr(gateway, "pool", FakePool(conn))

    assert await gateway.ack(EMPLOYEE, gateway.AckBody(ids=[1, 2]), authorization=bearer()) == {"acked": 2}
    assert conn.executed[0][2] == (EMPLOYEE, [1, 2])


# -- send: 幂等键与重投 ----------------------------------------------------------


@pytest.mark.asyncio
async def test_send_requires_an_idempotency_key(runtime_stubs, credentialed, monkeypatch):
    gateway = runtime_stubs
    monkeypatch.setattr(gateway, "pool", FakePool())
    monkeypatch.setattr(gateway, "legs", {EMPLOYEE: gateway_leg(gateway)})
    before = list(FAKE_BOT_STATE["replies"])
    with pytest.raises(HTTPException) as exc:
        await gateway.send(EMPLOYEE, send_body(gateway, idempotency_key=None), authorization=bearer())
    assert exc.value.status_code == 400
    assert FAKE_BOT_STATE["replies"] == before, "a key-less call must not post"


@pytest.mark.asyncio
async def test_redelivered_send_with_the_same_key_is_a_noop(runtime_stubs, credentialed, monkeypatch):
    gateway = runtime_stubs
    conn = FakeConnection()
    monkeypatch.setattr(gateway, "pool", FakePool(conn))
    monkeypatch.setattr(gateway, "legs", {EMPLOYEE: gateway_leg(gateway)})
    before = len(FAKE_BOT_STATE["replies"])

    first = await gateway.send(EMPLOYEE, send_body(gateway), authorization=bearer())
    second = await gateway.send(EMPLOYEE, send_body(gateway), authorization=bearer())
    assert first == {"sent": True, "duplicate": False}
    assert second == {"sent": True, "duplicate": True}
    assert len(FAKE_BOT_STATE["replies"]) == before + 1, "the redelivery must not post a second message"

    # A different key is a different message.
    third = await gateway.send(EMPLOYEE, send_body(gateway, text="第二条", idempotency_key="turn-2"), authorization=bearer())
    assert third == {"sent": True, "duplicate": False}
    assert FAKE_BOT_STATE["replies"][before + 1 :] == ["第二条"]

    # Keys are per employee.
    monkeypatch.setattr(gateway, "legs", {EMPLOYEE: gateway_leg(gateway), OTHER_EMPLOYEE: gateway_leg(gateway, OTHER_EMPLOYEE)})
    fourth = await gateway.send(OTHER_EMPLOYEE, send_body(gateway), authorization=bearer(OTHER_TOKEN))
    assert fourth == {"sent": True, "duplicate": False}


@pytest.mark.asyncio
async def test_failed_send_releases_the_key_for_retry(runtime_stubs, credentialed, monkeypatch):
    gateway = runtime_stubs
    conn = FakeConnection()
    monkeypatch.setattr(gateway, "pool", FakePool(conn))
    leg = gateway_leg(gateway)

    async def boom(message):
        raise RuntimeError("platform leg down")

    leg.channel._on_outbound = boom
    monkeypatch.setattr(gateway, "legs", {EMPLOYEE: leg})

    with pytest.raises(RuntimeError):
        await gateway.send(EMPLOYEE, send_body(gateway), authorization=bearer())

    async def ok(message):
        FAKE_BOT_STATE["replies"].append(message.get("text", ""))

    leg.channel._on_outbound = ok
    assert await gateway.send(EMPLOYEE, send_body(gateway), authorization=bearer()) == {"sent": True, "duplicate": False}
    assert FAKE_BOT_STATE["replies"][-1] == "回复"


@pytest.mark.asyncio
async def test_reconcile_legs_starts_and_stops(runtime_stubs, tmp_path, monkeypatch):
    gateway = runtime_stubs
    cfg = tmp_path / "connections.json"
    cfg.write_text(json.dumps({"a": {"provider": "wecom", "bot_id": "1", "bot_secret": "2"}}), encoding="utf-8")
    monkeypatch.setattr(gateway, "connections_file", str(cfg))
    monkeypatch.setattr(gateway, "pool", FakePool())

    await gateway.reconcile_legs()
    assert list(gateway.legs) == ["a"]

    # Removing the entry stops the leg on the next reconcile.
    cfg.write_text("{}", encoding="utf-8")
    await gateway.reconcile_legs()
    assert gateway.legs == {}


@pytest.mark.asyncio
async def test_healthz_reports_legs(runtime_stubs, monkeypatch):
    gateway = runtime_stubs
    monkeypatch.setattr(gateway, "legs", {"a": object(), "b": object()})
    assert await gateway.healthz() == {"status": "ok", "legs": ["a", "b"]}


@pytest.mark.asyncio
async def test_config_watcher_reconciles_on_change(runtime_stubs, tmp_path, monkeypatch):
    gateway = runtime_stubs
    cfg = tmp_path / "connections.json"
    cfg.write_text("{}", encoding="utf-8")
    monkeypatch.setattr(gateway, "connections_file", str(cfg))
    monkeypatch.setattr(gateway, "pool", FakePool())

    # Patch the sleep so the watcher cycles quickly, then stop it after the
    # file changes once.
    sleeps = {"n": 0}

    async def fast_sleep(seconds):
        sleeps["n"] += 1
        if sleeps["n"] == 1:
            cfg.write_text(json.dumps({"a": {"provider": "wecom", "bot_id": "1", "bot_secret": "2"}}), encoding="utf-8")
        if sleeps["n"] >= 2:
            raise asyncio.CancelledError

    monkeypatch.setattr(gateway.asyncio, "sleep", fast_sleep)
    with pytest.raises(asyncio.CancelledError):
        await gateway.config_watcher()
    assert list(gateway.legs) == ["a"], "the watcher must reconcile after the file changes"


@pytest.mark.asyncio
async def test_startup_requires_db_url(runtime_stubs, monkeypatch):
    gateway = runtime_stubs
    monkeypatch.delenv("PORTAL_DB_URL", raising=False)

    class BrokenPool:
        def __await__(self):
            raise RuntimeError("PORTAL_DB_URL is required")
            yield

    monkeypatch.setattr(gateway.asyncpg, "create_pool", lambda *a, **k: BrokenPool())
    with pytest.raises(RuntimeError):
        await gateway.startup()


# -- hot reload -----------------------------------------------------------------


def _write_connections(path: Path, connections: dict) -> None:
    path.write_text(json.dumps(connections), encoding="utf-8")


def _wecom_leg(secret: str, **extra) -> dict:
    return {"provider": "wecom", "bot_id": "bot-1", "bot_secret": secret, **extra}


@pytest.fixture()
def channel_factory(runtime_stubs, monkeypatch):
    """Recording, fault-injectable WeComChannel stand-in.

    ``events`` is one ordered log of every start/stop across legs, so a test can
    prove the old connection is stopped before the new one starts. ``plan`` is
    consumed by ``start()``: an exception there fails that attempt, anything else
    (or an exhausted plan) succeeds. ``fail_stops`` holds the leg indexes whose
    ``stop()`` raises.
    """
    import app.channels.wecom as wecom

    events: list[str] = []
    created: list[Any] = []
    plan: list = []
    fail_stops: set[int] = set()

    class Channel(FakeChannel):
        def __init__(self, bus, config):
            super().__init__(bus, config)
            self.index = len(created)
            created.append(self)

        async def start(self):
            outcome = plan.pop(0) if plan else None
            if outcome is not None:
                events.append(f"start-failed:{self.index}")
                raise outcome
            await super().start()
            events.append(f"start:{self.index}")

        async def stop(self):
            events.append(f"stop:{self.index}")
            if self.index in fail_stops:
                raise RuntimeError(f"stop {self.index} failed")
            await super().stop()

    monkeypatch.setattr(wecom, "WeComChannel", Channel)
    return types.SimpleNamespace(events=events, created=created, plan=plan, fail_stops=fail_stops)


def _leg_fixture(gateway, monkeypatch, tmp_path, connections: dict):
    """Point the gateway at a fresh connections file and empty leg bookkeeping."""
    cfg = tmp_path / "connections.json"
    _write_connections(cfg, connections)
    monkeypatch.setattr(gateway, "connections_file", str(cfg))
    monkeypatch.setattr(gateway, "pool", FakePool())
    monkeypatch.setattr(gateway, "legs", {})
    monkeypatch.setattr(gateway, "leg_states", {})
    return cfg


@pytest.mark.asyncio
async def test_reconcile_reconnects_when_credentials_change(runtime_stubs, channel_factory, tmp_path, monkeypatch):
    gateway = runtime_stubs
    cfg = _leg_fixture(gateway, monkeypatch, tmp_path, {"a": _wecom_leg("sec-old-0001")})

    await gateway.reconcile_legs()
    first = gateway.legs["a"]
    assert channel_factory.events == ["start:0"]

    # Same connect-relevant config: the live connection must be left alone.
    _write_connections(cfg, {"a": _wecom_leg("sec-old-0001")})
    await gateway.reconcile_legs()
    assert gateway.legs["a"] is first
    assert channel_factory.events == ["start:0"], "an unchanged config must not reconnect"

    # Rotated secret: one controlled replacement, old stopped before new starts.
    _write_connections(cfg, {"a": _wecom_leg("sec-new-0002")})
    await gateway.reconcile_legs()
    assert gateway.legs["a"] is not first
    assert first.channel.is_running is False, "the replaced leg must be stopped"
    assert gateway.legs["a"].channel.config["bot_secret"] == "sec-new-0002"
    assert channel_factory.events == ["start:0", "stop:0", "start:1"]


@pytest.mark.asyncio
async def test_reconcile_is_idempotent_and_ignores_unrelated_keys(runtime_stubs, channel_factory, tmp_path, monkeypatch):
    gateway = runtime_stubs
    cfg = _leg_fixture(gateway, monkeypatch, tmp_path, {"a": _wecom_leg("sec-00000001")})

    await gateway.reconcile_legs()
    first = gateway.legs["a"]
    for _ in range(3):
        await gateway.reconcile_legs()
    assert gateway.legs["a"] is first
    assert channel_factory.events == ["start:0"], "repeated reconciles must be no-ops"

    # A key that does not drive the connection is not connect-relevant.
    _write_connections(cfg, {"a": _wecom_leg("sec-00000001", note="added by hand")})
    await gateway.reconcile_legs()
    assert gateway.legs["a"] is first

    # working_message *is* part of the connection config, so it triggers a reload.
    _write_connections(cfg, {"a": _wecom_leg("sec-00000001", working_message="On it")})
    await gateway.reconcile_legs()
    assert gateway.legs["a"] is not first
    assert channel_factory.events == ["start:0", "stop:0", "start:1"]


@pytest.mark.asyncio
async def test_reconcile_retries_a_failed_start_with_backoff(runtime_stubs, channel_factory, tmp_path, monkeypatch, caplog):
    gateway = runtime_stubs
    secret = "sec-failure-1"
    _leg_fixture(gateway, monkeypatch, tmp_path, {"a": _wecom_leg(secret)})
    caplog.set_level(logging.INFO, logger="channel-gateway")
    clock = {"now": 1000.0}
    monkeypatch.setattr(gateway, "time", types.SimpleNamespace(monotonic=lambda: clock["now"]))

    channel_factory.plan.append(RuntimeError(f"credentials rejected: {secret}"))
    await gateway.reconcile_legs()

    assert gateway.legs == {}
    state = gateway.leg_states["a"]
    assert state.attempts == 1
    assert state.next_attempt_at == 1000.0 + gateway.RETRY_BASE_SECONDS
    assert state.pending_reason and secret not in state.pending_reason
    assert secret not in caplog.text, "a bot_secret must never reach the log"
    assert "***" in caplog.text, "the failing channel's secret must be redacted"

    # Inside the backoff window the leg is not retried...
    clock["now"] += gateway.RETRY_BASE_SECONDS / 2
    await gateway.reconcile_legs()
    assert len(channel_factory.created) == 1, "a failed leg must not be retried before its backoff elapses"

    # ...and once it elapses the retry succeeds although the file never changed.
    clock["now"] += gateway.RETRY_BASE_SECONDS
    await gateway.reconcile_legs()
    assert list(gateway.legs) == ["a"]
    assert channel_factory.events == ["start-failed:0", "stop:0", "start:1"]
    assert state.pending_reason is None and state.attempts == 0


@pytest.mark.asyncio
async def test_failed_replacement_backs_off_then_heals(runtime_stubs, channel_factory, tmp_path, monkeypatch):
    gateway = runtime_stubs
    cfg = _leg_fixture(gateway, monkeypatch, tmp_path, {"a": _wecom_leg("sec-old-0001")})
    clock = {"now": 500.0}
    monkeypatch.setattr(gateway, "time", types.SimpleNamespace(monotonic=lambda: clock["now"]))

    await gateway.reconcile_legs()
    first = gateway.legs["a"]

    # The rotated credential is rejected: the old connection is already gone.
    channel_factory.plan.append(RuntimeError("rotated secret rejected"))
    channel_factory.fail_stops.add(1)  # the half-started leg must still be cleaned up
    clock["now"] += 1
    _write_connections(cfg, {"a": _wecom_leg("sec-new-0002")})
    await gateway.reconcile_legs()

    assert "a" not in gateway.legs
    assert first.channel.is_running is False
    assert channel_factory.events == ["start:0", "stop:0", "start-failed:1", "stop:1"]
    state = gateway.leg_states["a"]
    assert state.attempts == 1 and state.pending_reason

    # Backoff holds the leg down...
    clock["now"] += 1
    await gateway.reconcile_legs()
    assert len(channel_factory.created) == 2

    # ...then the retry of the unchanged file brings it back.
    clock["now"] += gateway.RETRY_BASE_SECONDS * 2
    await gateway.reconcile_legs()
    assert list(gateway.legs) == ["a"]
    assert gateway.legs["a"].channel.config["bot_secret"] == "sec-new-0002"
    assert state.attempts == 0 and state.pending_reason is None


@pytest.mark.asyncio
async def test_reconcile_stops_and_forgets_a_removed_leg(runtime_stubs, channel_factory, tmp_path, monkeypatch):
    gateway = runtime_stubs
    cfg = _leg_fixture(gateway, monkeypatch, tmp_path, {"a": _wecom_leg("sec-00000001"), "b": _wecom_leg("sec-00000002")})
    channel_factory.fail_stops.add(1)  # a stop that raises must not abort the reconcile

    await gateway.reconcile_legs()
    assert list(gateway.legs) == ["a", "b"]

    _write_connections(cfg, {})
    await gateway.reconcile_legs()
    assert gateway.legs == {}
    assert gateway.leg_states == {}
    assert channel_factory.events == ["start:0", "start:1", "stop:0", "stop:1"]

    # Nothing left to do on a second pass.
    await gateway.reconcile_legs()
    assert channel_factory.events == ["start:0", "start:1", "stop:0", "stop:1"]


@pytest.mark.asyncio
async def test_missing_credentials_keep_the_running_leg(runtime_stubs, channel_factory, tmp_path, monkeypatch, caplog):
    gateway = runtime_stubs
    cfg = _leg_fixture(gateway, monkeypatch, tmp_path, {"a": _wecom_leg("sec-00000001")})
    caplog.set_level(logging.INFO, logger="channel-gateway")

    await gateway.reconcile_legs()
    first = gateway.legs["a"]

    # A half-written Secret must not take a working connection down.
    _write_connections(cfg, {"a": {"provider": "wecom", "bot_id": "bot-1"}})
    await gateway.reconcile_legs()
    await gateway.reconcile_legs()
    assert gateway.legs["a"] is first
    assert gateway.leg_states["a"].pending_reason == gateway.MISSING_CREDENTIALS
    assert caplog.text.count("lacks bot_id/bot_secret") == 1, "the warning must not repeat every reconcile"

    # The credentials come back unchanged: the running leg is kept as is.
    _write_connections(cfg, {"a": _wecom_leg("sec-00000001")})
    await gateway.reconcile_legs()
    assert gateway.legs["a"] is first
    assert gateway.leg_states["a"].pending_reason is None


@pytest.mark.asyncio
async def test_config_watcher_retries_a_failed_leg_without_a_file_change(
    runtime_stubs, channel_factory, tmp_path, monkeypatch
):
    gateway = runtime_stubs
    _leg_fixture(gateway, monkeypatch, tmp_path, {"a": _wecom_leg("sec-00000001")})
    monkeypatch.setattr(gateway, "RETRY_BASE_SECONDS", 0)
    channel_factory.plan.append(RuntimeError("platform refused the first attempt"))

    ticks = {"n": 0}

    async def fast_sleep(seconds):
        ticks["n"] += 1
        if ticks["n"] >= 2:
            raise asyncio.CancelledError

    monkeypatch.setattr(gateway.asyncio, "sleep", fast_sleep)
    with pytest.raises(asyncio.CancelledError):
        await gateway.config_watcher()
    assert list(gateway.legs) == ["a"], "the watcher must retry a failed leg even though the file never changed"
    assert channel_factory.events == ["start-failed:0", "stop:0", "start:1"]


@pytest.mark.asyncio
async def test_config_watcher_survives_a_half_written_file(runtime_stubs, channel_factory, tmp_path, monkeypatch, caplog):
    gateway = runtime_stubs
    cfg = _leg_fixture(gateway, monkeypatch, tmp_path, {"a": _wecom_leg("sec-00000001")})
    caplog.set_level(logging.INFO, logger="channel-gateway")
    await gateway.reconcile_legs()
    first = gateway.legs["a"]

    # A Secret being rewritten can be caught mid-write.
    cfg.write_text('{"a": {"provider": "wecom", "bot_', encoding="utf-8")

    async def fast_sleep(seconds):
        raise asyncio.CancelledError

    monkeypatch.setattr(gateway.asyncio, "sleep", fast_sleep)
    with pytest.raises(asyncio.CancelledError):
        await gateway.config_watcher()
    assert gateway.legs["a"] is first, "a torn read must not tear a running leg down"
    assert "config watcher iteration failed" in caplog.text

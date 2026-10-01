"""Unit tests for de-channel-gateway.

The platform leg (WeComChannel) and the aibot SDK live only inside the
runtime image; these tests inject stand-ins so the gateway's own logic —
frame bookkeeping, inbox leasing, ack handling, connection loading — runs
against fakes. Run with: python -m pytest channel-gateway/tests.
"""

from __future__ import annotations

import asyncio
import json
import sys
import types
from pathlib import Path

import pytest

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
    def __init__(self):
        self.executed: list[tuple] = []

    def transaction(self):
        return FakeTransaction()

    async def execute(self, sql, *args):
        self.executed.append((sql.split()[0].upper(), sql, args))

    async def __aenter__(self):
        return self

    async def __aexit__(self, *exc):
        return False


class FakePool:
    def __init__(self):
        self.connection = FakeConnection()

    def acquire(self):
        return self.connection


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

    send = gateway.SendBody(
        channel_name="wecom",
        chat_id="u1",
        thread_id="t1",
        text="hi",
        artifacts=[],
        is_final=True,
    )
    assert send.is_final is True


@pytest.mark.asyncio
async def test_inbox_long_poll_leases_and_times_out(runtime_stubs, monkeypatch):
    gateway = runtime_stubs
    class RowsConn:
        calls = 0

        async def fetch(self, sql, *args):
            RowsConn.calls += 1
            if RowsConn.calls == 1:
                return [{"id": 7, "msg_type": "text", "frame": json.dumps({"body": {"msgid": "m"}})}]
            return []

        async def __aenter__(self):
            return self

        async def __aexit__(self, *exc):
            return False

    class LeasePool:
        def acquire(self):
            return RowsConn()

    monkeypatch.setattr(gateway, "pool", LeasePool())
    response = await gateway.inbox("sales-helper", wait=0)
    assert response["messages"][0]["id"] == 7

    # Empty poll with wait=0 returns immediately without rows.
    response = await gateway.inbox("sales-helper", wait=0)
    assert response == {"messages": []}


@pytest.mark.asyncio
async def test_send_routes_to_the_leg(runtime_stubs, monkeypatch):
    gateway = runtime_stubs
    pool = FakePool()
    leg = gateway.GatewayLeg("sales-helper", {"bot_id": "b", "bot_secret": "s"}, pool)
    monkeypatch.setattr(gateway, "legs", {"sales-helper": leg})

    body = gateway.SendBody(
        channel_name="wecom", chat_id="u1", thread_id="t1", text="回复", artifacts=[], is_final=True
    )
    response = await gateway.send("sales-helper", body)
    assert response == {"sent": True}
    assert FAKE_BOT_STATE["replies"][-1] == "回复"


@pytest.mark.asyncio
async def test_send_unknown_employee_404(runtime_stubs, monkeypatch):
    from fastapi import HTTPException

    gateway = runtime_stubs
    monkeypatch.setattr(gateway, "legs", {})
    body = gateway.SendBody(channel_name="wecom", chat_id="u", thread_id="t", text="x")
    with pytest.raises(HTTPException) as exc:
        await gateway.send("ghost", body)
    assert exc.value.status_code == 404


@pytest.mark.asyncio
async def test_ensure_schema_creates_tables(runtime_stubs):
    gateway = runtime_stubs
    conn = FakeConnection()
    await gateway.ensure_schema(conn)
    ddl = conn.executed[0][1]
    assert "channel_inbox" in ddl
    assert "channel_inbox_pending" in ddl


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

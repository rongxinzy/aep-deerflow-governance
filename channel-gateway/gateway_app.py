"""de-channel-gateway: the always-on platform leg for IM channels.

Hibernatable employees scale their runtime to zero; someone must keep the
outbound websocket to the messaging platform alive. This service holds one
platform leg per employee (reusing the runtime image's own channel code, so
protocol handling never drifts), buffers inbound frames in the portal
database, and wakes the employee with the same channel_wake rows the portal
uses for chat. The runtime's gateway-mode WeCom channel long-polls
/inbox/{employee}, replays frames through its unchanged handlers, and posts
outbound messages to /send/{employee}.

Configuration:
  PORTAL_DB_URL               postgres DSN (same database as the portal)
  CONNECTIONS_FILE            JSON {employee: {provider, bot_id, bot_secret}}
                              (mounted from the de-channel-connections Secret)
  GATEWAY_LISTEN              uvicorn bind, default 0.0.0.0:8899
"""

from __future__ import annotations

import asyncio
import json
import logging
import os
from pathlib import Path
from typing import Any

import asyncpg
import uvicorn
from fastapi import FastAPI, HTTPException, Request
from pydantic import BaseModel

logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(name)s %(message)s")
logger = logging.getLogger("channel-gateway")

INBOX_LEASE_SECONDS = 60
POLL_WAIT_SECONDS = 20

app = FastAPI(title="de-channel-gateway")


class GatewayBus:
    """No-op stand-in: gateway legs never dispatch to an agent bus."""

    def subscribe_outbound(self, callback: Any) -> None:
        pass

    def unsubscribe_outbound(self, callback: Any) -> None:
        pass

    async def publish_inbound(self, message: Any) -> None:
        pass


class GatewayLeg:
    """One employee's platform websocket leg.

    Wraps the runtime image's WeComChannel: its start() wires the real
    websocket client; we override the four frame handlers so raw frames are
    persisted (and the employee woken) instead of dispatched to an agent.
    Send-side state (_ws_frames/_ws_stream_ids) lives here, which is exactly
    what the runtime-side send delegate needs.
    """

    def __init__(self, employee: str, config: dict[str, Any], pool: asyncpg.Pool) -> None:
        self.employee = employee
        self.pool = pool
        # Imported lazily so the module also loads outside the runtime image.
        from app.channels.wecom import WeComChannel

        outer = self

        class Leg(WeComChannel):
            async def _on_ws_text(self, frame: dict[str, Any]) -> None:
                await outer._enqueue("text", frame)

            async def _on_ws_mixed(self, frame: dict[str, Any]) -> None:
                await outer._enqueue("mixed", frame)

            async def _on_ws_image(self, frame: dict[str, Any]) -> None:
                await outer._enqueue("image", frame)

            async def _on_ws_file(self, frame: dict[str, Any]) -> None:
                await outer._enqueue("file", frame)

        self.channel = Leg(bus=GatewayBus(), config=config)

    async def start(self) -> None:
        await self.channel.start()
        if not self.channel.is_running:
            raise RuntimeError(f"leg for {self.employee} failed to start (check credentials)")

    async def stop(self) -> None:
        await self.channel.stop()

    async def _enqueue(self, msg_type: str, frame: dict[str, Any]) -> None:
        """Persist one raw frame and wake the employee.

        Also performs the bookkeeping the native _publish_ws_inbound would
        have done on the send side (frame/stream-id registry plus the
        immediate working acknowledgement), because replies are streamed
        from this process.
        """
        try:
            from aibot import generate_req_id

            body = frame.get("body") or {}
            msg_id = body.get("msgid")
            if msg_id:
                self.channel._ws_frames[msg_id] = frame
                self.channel._ws_stream_ids[msg_id] = generate_req_id("stream")
            try:
                await self.channel._ws_client.reply_stream(
                    frame, self.channel._ws_stream_ids.get(msg_id) or generate_req_id("stream"), self.channel._working_message, False
                )
            except Exception:
                pass
        except Exception:
            logger.exception("frame bookkeeping failed for %s", self.employee)

        async with self.pool.acquire() as conn:
            async with conn.transaction():
                await conn.execute(
                    "INSERT INTO channel_inbox (employee, msg_type, frame) VALUES ($1, $2, $3)",
                    self.employee, msg_type, json.dumps(frame),
                )
                await conn.execute("INSERT INTO channel_wake (employee) VALUES ($1)", self.employee)
                await conn.execute(
                    "INSERT INTO employee_activity (employee, last_active) VALUES ($1, now()) "
                    "ON CONFLICT (employee) DO UPDATE SET last_active = now()",
                    self.employee,
                )
        logger.info("enqueued %s frame for %s", msg_type, self.employee)


pool: asyncpg.Pool | None = None
legs: dict[str, GatewayLeg] = {}
connections_file = os.getenv("CONNECTIONS_FILE", "/etc/channel-gateway/connections.json")


async def ensure_schema(conn: asyncpg.Connection) -> None:
    await conn.execute(
        """
        CREATE TABLE IF NOT EXISTS channel_inbox (
            id         BIGSERIAL PRIMARY KEY,
            employee   TEXT NOT NULL,
            msg_type   TEXT NOT NULL,
            frame      JSONB NOT NULL,
            created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
            leased_at  TIMESTAMPTZ,
            acked_at   TIMESTAMPTZ
        );
        CREATE INDEX IF NOT EXISTS channel_inbox_pending ON channel_inbox (employee, id) WHERE acked_at IS NULL;
        """
    )


def load_connections() -> dict[str, dict[str, Any]]:
    path = Path(connections_file)
    if not path.exists():
        logger.warning("connections file %s missing; gateway runs with no legs", path)
        return {}
    data = json.loads(path.read_text(encoding="utf-8"))
    return {name: cfg for name, cfg in data.items() if isinstance(cfg, dict) and cfg.get("provider") == "wecom"}


async def reconcile_legs() -> None:
    """Match running legs to the connections file (hot reload)."""
    assert pool is not None
    desired = load_connections()
    for name in list(legs):
        if name not in desired:
            logger.info("stopping leg %s (removed from connections)", name)
            await legs.pop(name).stop()
    for name, cfg in desired.items():
        if name in legs:
            continue
        bot_id, bot_secret = cfg.get("bot_id"), cfg.get("bot_secret")
        if not bot_id or not bot_secret:
            logger.warning("leg %s lacks bot_id/bot_secret; skipped", name)
            continue
        leg = GatewayLeg(name, {"bot_id": bot_id, "bot_secret": bot_secret, "working_message": cfg.get("working_message")}, pool)
        try:
            await leg.start()
            legs[name] = leg
            logger.info("leg %s connected", name)
        except Exception:
            logger.exception("leg %s failed to start", name)


async def config_watcher() -> None:
    last_mtime: float | None = None
    while True:
        try:
            mtime = Path(connections_file).stat().st_mtime if Path(connections_file).exists() else None
            if mtime != last_mtime:
                last_mtime = mtime
                await reconcile_legs()
        except asyncio.CancelledError:
            raise
        except Exception:
            logger.exception("config watcher iteration failed")
        await asyncio.sleep(15)


@app.on_event("startup")
async def startup() -> None:
    global pool
    dsn = os.getenv("PORTAL_DB_URL")
    if not dsn:
        raise RuntimeError("PORTAL_DB_URL is required")
    pool = asyncpg.create_pool(dsn, min_size=1, max_size=4)
    await pool  # pool lazily initializes
    async with pool.acquire() as conn:
        await ensure_schema(conn)
    asyncio.create_task(config_watcher())


@app.get("/healthz")
async def healthz() -> dict[str, Any]:
    return {"status": "ok", "legs": sorted(legs)}


class AckBody(BaseModel):
    ids: list[int]


@app.get("/inbox/{employee}")
async def inbox(employee: str, wait: int = POLL_WAIT_SECONDS) -> dict[str, Any]:
    """Lease pending frames for one employee.

    Long-polls up to `wait` seconds for new rows; leased rows are invisible
    to other pollers for INBOX_LEASE_SECONDS, then redeliver (at-least-once).
    """
    assert pool is not None
    deadline = asyncio.get_event_loop().time() + max(0, min(wait, 55))
    while True:
        async with pool.acquire() as conn:
            rows = await conn.fetch(
                """
                UPDATE channel_inbox SET leased_at = now()
                WHERE id IN (
                    SELECT id FROM channel_inbox
                    WHERE employee = $1 AND acked_at IS NULL
                      AND (leased_at IS NULL OR leased_at < now() - make_interval(secs => $2))
                    ORDER BY id LIMIT 20
                    FOR UPDATE SKIP LOCKED
                )
                RETURNING id, msg_type, frame
                """,
                employee, INBOX_LEASE_SECONDS,
            )
        if rows:
            return {"messages": [{"id": r["id"], "msg_type": r["msg_type"], "frame": json.loads(r["frame"])} for r in rows]}
        if asyncio.get_event_loop().time() >= deadline:
            return {"messages": []}
        await asyncio.sleep(1.5)


@app.post("/ack/{employee}")
async def ack(employee: str, body: AckBody) -> dict[str, Any]:
    assert pool is not None
    async with pool.acquire() as conn:
        await conn.execute(
            "UPDATE channel_inbox SET acked_at = now() WHERE employee = $1 AND id = ANY($2::bigint[])",
            employee, body.ids,
        )
    return {"acked": len(body.ids)}


class SendBody(BaseModel):
    channel_name: str
    chat_id: str
    thread_id: str
    text: str
    artifacts: list[str] = []
    is_final: bool = True
    thread_ts: str | None = None
    connection_id: str | None = None
    owner_user_id: str | None = None
    metadata: dict[str, Any] = {}
    created_at: float = 0.0


@app.post("/send/{employee}")
async def send(employee: str, body: SendBody) -> dict[str, Any]:
    """Deliver one outbound message over the employee's platform leg.

    The runtime serializes its OutboundMessage; the leg's _on_outbound runs
    the full native send path (streaming chunks, attachments registry,
    context cleanup).
    """
    leg = legs.get(employee)
    if leg is None:
        raise HTTPException(status_code=404, detail=f"no platform leg for {employee}")
    from app.channels.message_bus import OutboundMessage

    msg = OutboundMessage(
        channel_name=body.channel_name,
        chat_id=body.chat_id,
        thread_id=body.thread_id,
        text=body.text,
        artifacts=body.artifacts,
        is_final=body.is_final,
        thread_ts=body.thread_ts,
        connection_id=body.connection_id,
        owner_user_id=body.owner_user_id,
        metadata=body.metadata,
        created_at=body.created_at,
    )
    await leg.channel._on_outbound(msg)
    return {"sent": True}


if __name__ == "__main__":
    uvicorn.run(app, host="0.0.0.0", port=int(os.getenv("GATEWAY_PORT", "8899")), log_level="info")

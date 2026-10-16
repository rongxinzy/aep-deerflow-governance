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
  CONNECTIONS_FILE            JSON {employee: {provider, bot_id, bot_secret,
                              caller_credential: {token, expires_at, operations}}}
                              (mounted from the de-channel-connections Secret)
  GATEWAY_LISTEN              uvicorn bind, default 0.0.0.0:8899

Every /inbox, /ack and /send call must carry the caller credential of the
employee named in the path (see PROTOCOL.md). Employees without a credential
in the connections file are rejected outright: the gateway stays useful only
as far as its callers are authenticated, and the sibling client mode in the
runtime image is NOT implemented yet (see PROTOCOL.md "Status").
"""

from __future__ import annotations

import asyncio
import hashlib
import hmac
import json
import logging
import os
import time
import traceback
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import asyncpg
import uvicorn
from fastapi import FastAPI, Header, HTTPException, Request
from pydantic import BaseModel

logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(name)s %(message)s")
logger = logging.getLogger("channel-gateway")

INBOX_LEASE_SECONDS = 60
POLL_WAIT_SECONDS = 20
RECONCILE_INTERVAL_SECONDS = 15
# Start attempts for one leg back off exponentially from here (capped), so a
# wrong secret or a flapping platform endpoint is retried without busy polling.
RETRY_BASE_SECONDS = 15
RETRY_MAX_SECONDS = 300

# Every bot_secret that enters this process, so no log record can carry one:
# our own lines never interpolate a config, but a failing channel may put the
# secret into its exception text. Values shorter than 4 characters cannot be
# real credentials, and redacting them would mangle unrelated log text.
_SECRETS: set[str] = set()


def _register_secret(config: dict[str, Any]) -> None:
    secret = config.get("bot_secret")
    if isinstance(secret, str) and len(secret) >= 4:
        _SECRETS.add(secret)


def _redact(text: str) -> str:
    for secret in _SECRETS:
        text = text.replace(secret, "***")
    return text


class _SecretScrubFilter(logging.Filter):
    """Last line of defence: a bot_secret must never reach a log handler."""

    def filter(self, record: logging.LogRecord) -> bool:
        if not _SECRETS:
            return True
        try:
            message = record.getMessage()
            if record.exc_info:
                message += "\n" + "".join(traceback.format_exception(*record.exc_info))
                record.exc_info = None
                record.exc_text = None
            record.msg = _redact(message)
            record.args = ()
        except Exception:
            pass
        return True


logger.addFilter(_SecretScrubFilter())
OUTBOUND_IDEMPOTENCY_TTL_SECONDS = 24 * 3600
GATEWAY_OPERATIONS = ("inbox", "ack", "send")

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


@dataclass
class LegState:
    """Desired vs. observed state of one employee's leg, kept across reconciles.

    ``fingerprint`` digests the connect-relevant config the *running* leg was
    built from (a digest, so it is safe to log or compare). ``pending_reason``
    says why the currently desired config is not being served — a failed start,
    or a file entry missing its credentials — and ``attempts``/``next_attempt_at``
    drive the retry backoff that gets it serving once the file is fixed.
    """

    fingerprint: str | None = None
    pending_reason: str | None = None
    attempts: int = 0
    next_attempt_at: float = 0.0


leg_states: dict[str, LegState] = {}
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

        CREATE TABLE IF NOT EXISTS channel_outbound_idempotency (
            employee        TEXT NOT NULL,
            idempotency_key TEXT NOT NULL,
            created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
            PRIMARY KEY (employee, idempotency_key)
        );
        """
    )


def load_connections() -> dict[str, dict[str, Any]]:
    path = Path(connections_file)
    if not path.exists():
        logger.warning("connections file %s missing; gateway runs with no legs", path)
        return {}
    data = json.loads(path.read_text(encoding="utf-8"))
    return {name: cfg for name, cfg in data.items() if isinstance(cfg, dict) and cfg.get("provider") == "wecom"}


# Only these fields drive the websocket leg; anything else in the Secret is
# ignored, so editing an unrelated key must not force a reconnect.
LEG_CONFIG_KEYS = ("bot_id", "bot_secret", "working_message")
MISSING_CREDENTIALS = "connections entry lacks bot_id/bot_secret"


def _leg_config(cfg: dict[str, Any]) -> dict[str, Any] | None:
    """The connect-relevant subset of a connections entry, or None if unusable."""
    bot_id, bot_secret = cfg.get("bot_id"), cfg.get("bot_secret")
    if not bot_id or not bot_secret:
        return None
    return {"bot_id": bot_id, "bot_secret": bot_secret, "working_message": cfg.get("working_message")}


def _fingerprint(config: dict[str, Any]) -> str:
    """Stable digest of a connect-relevant config (the secret never leaves it)."""
    payload = json.dumps([config.get(key) for key in LEG_CONFIG_KEYS], ensure_ascii=False)
    return hashlib.sha256(payload.encode("utf-8")).hexdigest()


def _retry_delay(attempts: int) -> float:
    """Exponential backoff between failed start attempts (the exponent is capped
    because the delay itself is capped, and a leg may fail for days)."""
    return float(min(RETRY_MAX_SECONDS, RETRY_BASE_SECONDS * 2 ** min(attempts - 1, 8)))


async def _stop_leg(name: str) -> None:
    """Tear one leg down; a failing stop must not abort the whole reconcile."""
    leg = legs.pop(name, None)
    if leg is None:
        return
    try:
        await leg.stop()
    except Exception:
        logger.exception("leg %s failed to stop cleanly", name)


async def _swap_leg(name: str, config: dict[str, Any], fingerprint: str, state: LegState) -> None:
    """Bring one leg to the desired config: stop the old connection, start the new one.

    The old leg is stopped before the new one starts, so an employee never has
    two long connections consuming its messages. If the new connection fails,
    the leg stays down (the old config is not a usable path once it changed)
    and the backoff in ``state`` makes the watcher retry it.
    """
    assert pool is not None
    if name in legs:
        logger.info("leg %s config changed; reconnecting", name)
        await _stop_leg(name)
    _register_secret(config)
    now = time.monotonic()
    leg: GatewayLeg | None = None
    try:
        leg = GatewayLeg(name, config, pool)
        await leg.start()
    except Exception as exc:
        if leg is not None:
            try:
                await leg.stop()
            except Exception:
                logger.exception("leg %s failed to clean up after a failed start", name)
        state.attempts += 1
        state.pending_reason = _redact(f"{type(exc).__name__}: {exc}")
        delay = _retry_delay(state.attempts)
        state.next_attempt_at = now + delay
        logger.exception("leg %s failed to start (attempt %d); retrying in %.0fs", name, state.attempts, delay)
        return
    legs[name] = leg
    state.pending_reason = None
    state.attempts = 0
    state.next_attempt_at = 0.0
    logger.info("leg %s connected (config %s)", name, fingerprint[:12])

@dataclass(frozen=True)
class CallerCredential:
    """One employee's caller credential, rendered into the connections file.

    The token IS the subject: it is looked up under the employee named in the
    request path, so a token can only ever speak for the employee it was
    issued to.
    """

    token: str
    expires_at: float
    operations: tuple[str, ...] = GATEWAY_OPERATIONS


def load_credentials() -> dict[str, CallerCredential]:
    """Per-employee caller credentials from the connections file.

    Unusable entries (no token, no readable expiry, unknown operation) are
    dropped, which leaves that employee with no credential and therefore with
    no way to call the gateway at all — the intended fail-closed behaviour.
    """
    try:
        connections = load_connections()
    except (OSError, ValueError):
        logger.exception("cannot read caller credentials from %s", connections_file)
        return {}
    credentials: dict[str, CallerCredential] = {}
    for employee, cfg in connections.items():
        raw = cfg.get("caller_credential")
        if not isinstance(raw, dict):
            continue
        token = raw.get("token")
        if not isinstance(token, str) or not token:
            logger.warning("leg %s has a caller credential without a token; ignored", employee)
            continue
        try:
            expires_at = float(raw.get("expires_at"))
        except (TypeError, ValueError):
            logger.warning("leg %s has a caller credential without a usable expires_at; ignored", employee)
            continue
        operations = raw.get("operations")
        if operations is None:
            operations = list(GATEWAY_OPERATIONS)
        if not isinstance(operations, list) or not all(op in GATEWAY_OPERATIONS for op in operations):
            logger.warning("leg %s has an invalid caller credential operations list; ignored", employee)
            continue
        credentials[employee] = CallerCredential(token=token, expires_at=expires_at, operations=tuple(operations))
    return credentials


def _bearer_token(authorization: str | None) -> str | None:
    if not authorization:
        return None
    scheme, _, token = authorization.partition(" ")
    if scheme.lower() != "bearer" or not token.strip():
        return None
    return token.strip()


def authorize(employee: str, authorization: str | None, operation: str) -> None:
    """Validate the caller against the employee in the path.

    Re-reads the connections file on every call: a rotated Secret must stop
    accepting the retired token immediately, not at the next watcher tick.
    The file is a few KB, polled about once per second per employee.
    """
    token = _bearer_token(authorization)
    if token is None:
        raise HTTPException(
            status_code=401, detail="missing caller credential", headers={"WWW-Authenticate": "Bearer"}
        )
    credential = load_credentials().get(employee)
    if credential is None:
        raise HTTPException(status_code=403, detail=f"no caller credential configured for {employee}")
    try:
        matches = hmac.compare_digest(credential.token, token)
    except TypeError:  # non-ASCII header value cannot be a credential
        matches = False
    if not matches:
        raise HTTPException(status_code=403, detail=f"caller credential rejected for {employee}")
    if credential.expires_at <= time.time():
        raise HTTPException(
            status_code=401, detail="caller credential expired", headers={"WWW-Authenticate": "Bearer"}
        )
    if operation not in credential.operations:
        raise HTTPException(status_code=403, detail=f"caller credential may not perform {operation}")


async def reconcile_legs() -> None:
    """Match running legs to the connections file (hot reload).

    A leg whose connect-relevant config changed is replaced; a leg whose
    desired config is not serving (failed start, missing credentials) is
    retried with a backoff, so fixing the file, or the platform coming back,
    heals the leg without waiting for another file change.
    """
    assert pool is not None
    desired = load_connections()
    credentialed = load_credentials()
    for name in desired:
        if name not in credentialed:
            logger.warning("leg %s has no caller credential; its endpoints reject every caller", name)
    for name in list(legs):
        if name not in desired:
            logger.info("stopping leg %s (removed from connections)", name)
            await _stop_leg(name)
    for name in list(leg_states):
        if name not in desired:
            leg_states.pop(name)

    for name, cfg in desired.items():
        state = leg_states.setdefault(name, LegState())
        config = _leg_config(cfg)
        if config is None:
            # Leave any running leg alone: a truncated or half-written Secret
            # must not take a working connection down.
            if state.pending_reason != MISSING_CREDENTIALS:
                logger.warning("leg %s lacks bot_id/bot_secret; skipped", name)
                state.pending_reason = MISSING_CREDENTIALS
            continue
        fingerprint = _fingerprint(config)
        if name in legs and state.fingerprint == fingerprint:
            state.pending_reason = None
            continue
        if state.fingerprint != fingerprint:
            # First sight, or the config changed: replace, with a fresh budget.
            state.fingerprint = fingerprint
            state.pending_reason = None
            state.attempts = 0
            state.next_attempt_at = 0.0
        elif time.monotonic() < state.next_attempt_at:
            continue
        await _swap_leg(name, config, fingerprint, state)


async def config_watcher() -> None:
    """Reconcile on a fixed cadence rather than only on a file mtime change.

    An mtime-only trigger could never retry a leg whose start failed while the
    file stayed put; ``reconcile_legs`` is cheap and a no-op when nothing is
    desired, changed or due for a retry.
    """
    while True:
        try:
            await reconcile_legs()
        except asyncio.CancelledError:
            raise
        except Exception:
            logger.exception("config watcher iteration failed")
        await asyncio.sleep(RECONCILE_INTERVAL_SECONDS)


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
async def inbox(
    employee: str, wait: int = POLL_WAIT_SECONDS, authorization: str | None = Header(default=None)
) -> dict[str, Any]:
    """Lease pending frames for one employee.

    Long-polls up to `wait` seconds for new rows; leased rows are invisible
    to other pollers for INBOX_LEASE_SECONDS, then redeliver (at-least-once).
    """
    authorize(employee, authorization, "inbox")
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
async def ack(
    employee: str, body: AckBody, authorization: str | None = Header(default=None)
) -> dict[str, Any]:
    """Mark frames as processed.

    Acking is scoped to the credential's own employee: the ownership check is
    explicit (and rejects the whole batch) rather than left to the WHERE
    clause, so a caller cannot consume another employee's rows even by id.
    """
    authorize(employee, authorization, "ack")
    assert pool is not None
    async with pool.acquire() as conn:
        foreign = await conn.fetchval(
            "SELECT employee FROM channel_inbox WHERE employee <> $1 AND id = ANY($2::bigint[]) LIMIT 1",
            employee, body.ids,
        )
        if foreign is not None:
            # Deliberately unnamed: an authenticated employee must not be able
            # to probe other employees out of the rejection.
            raise HTTPException(status_code=403, detail="cannot ack messages that belong to another employee")
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
    idempotency_key: str | None = None


async def _reserve_outbound(employee: str, idempotency_key: str) -> bool:
    """Claim one outbound idempotency key; False means it was already used.

    The claim is taken before the send so two concurrent redeliveries of the
    same turn cannot both reach the platform, and dropped again if the send
    fails so the client's retry is not swallowed. Keys expire after
    OUTBOUND_IDEMPOTENCY_TTL_SECONDS.
    """
    assert pool is not None
    async with pool.acquire() as conn:
        await conn.execute(
            "DELETE FROM channel_outbound_idempotency WHERE created_at < now() - make_interval(secs => $1)",
            OUTBOUND_IDEMPOTENCY_TTL_SECONDS,
        )
        reserved = await conn.fetchval(
            "INSERT INTO channel_outbound_idempotency (employee, idempotency_key) VALUES ($1, $2) "
            "ON CONFLICT (employee, idempotency_key) DO NOTHING RETURNING TRUE",
            employee, idempotency_key,
        )
    return reserved is not None


async def _release_outbound(employee: str, idempotency_key: str) -> None:
    assert pool is not None
    async with pool.acquire() as conn:
        await conn.execute(
            "DELETE FROM channel_outbound_idempotency WHERE employee = $1 AND idempotency_key = $2",
            employee, idempotency_key,
        )


@app.post("/send/{employee}")
async def send(
    employee: str, body: SendBody, authorization: str | None = Header(default=None)
) -> dict[str, Any]:
    """Deliver one outbound message over the employee's platform leg.

    The runtime serializes its OutboundMessage; the leg's _on_outbound runs
    the full native send path (streaming chunks, attachments registry,
    context cleanup). `idempotency_key` (the client's turn/message id) makes
    redelivery of the same turn a no-op: a repeat returns duplicate=True
    instead of posting a second message.
    """
    authorize(employee, authorization, "send")
    idempotency_key = body.idempotency_key
    if not idempotency_key:
        raise HTTPException(status_code=400, detail="idempotency_key is required")
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
    if not await _reserve_outbound(employee, idempotency_key):
        logger.info("outbound %s/%s already sent; ignoring redelivery", employee, idempotency_key)
        return {"sent": True, "duplicate": True}
    try:
        await leg.channel._on_outbound(msg)
    except BaseException:
        await _release_outbound(employee, idempotency_key)
        raise
    return {"sent": True, "duplicate": False}


if __name__ == "__main__":
    uvicorn.run(app, host="0.0.0.0", port=int(os.getenv("GATEWAY_PORT", "8899")), log_level="info")

# de-channel-gateway protocol

Wire contract between `gateway_app.py` and the runtime-side client that polls it.
Implemented and enforced by the server; the client described in "Client contract"
does **not** exist yet (see "Status").

## Credential carrier

Per-employee caller credentials live in the same connections file the gateway
already mounts (`CONNECTIONS_FILE`, from the `de-channel-connections` Secret),
next to the platform bot credentials:

```json
{
  "sales-helper": {
    "provider": "wecom",
    "bot_id": "...",
    "bot_secret": "...",
    "caller_credential": {
      "token": "<random, per employee>",
      "expires_at": 1798761600,
      "operations": ["inbox", "ack", "send"]
    }
  }
}
```

- `token` — opaque bearer token, unique per employee. Rotate by writing a new
  value.
- `expires_at` — unix seconds. No expiry configured = entry unusable.
- `operations` — optional subset of `inbox`/`ack`/`send`; defaults to all three.
- A leg without a usable `caller_credential` has **no** credential and every
  call for it is rejected (fail closed).
- The Secret is provisioned out of band (the chart deliberately does not carry
  it); the operator does not render it today.

## Caller credential header

```
Authorization: Bearer <token>
```

`/healthz` is unauthenticated. Everything else requires the credential of the
employee named in the path.

## Validation order

| Check | Failure |
|---|---|
| Header present and `Bearer <token>` | `401 missing caller credential` |
| Employee has a credential in the connections file | `403 no caller credential configured for {employee}` |
| Token equals that employee's token | `403 caller credential rejected for {employee}` |
| Token not expired | `401 caller credential expired` |
| Operation (`inbox`/`ack`/`send`) allowed | `403 caller credential may not perform {operation}` |

The credential is bound to the path employee, so a valid token used against
another employee's path (employee A reading/writing employee B) is a `403`, not
a filtered empty result. 401 responses carry `WWW-Authenticate: Bearer`.

The connections file is re-read per request, so a rewritten Secret invalidates
the retired token immediately — no restart, no reconcile window.

`/send` additionally returns `400 idempotency_key is required` before any DB
work, and `404 no platform leg for {employee}` when the employee is credentialed
but its leg is not connected.

## Endpoints

- `GET /inbox/{employee}?wait=<sec>` → `{"messages": [{"id", "msg_type", "frame"}]}`
- `POST /ack/{employee}` with `{"ids": [<int>]}` → `{"acked": <n>}`
- `POST /send/{employee}` with the serialized outbound message → `{"sent": true, "duplicate": <bool>}`

## Lease and ack semantics

- `/inbox` leases up to 20 pending rows per call for `INBOX_LEASE_SECONDS` (60s);
  leased rows are skipped by other pollers, and are redelivered once the lease
  expires. Delivery is **at-least-once**.
- Acking is per employee and explicit: ids owned by another employee reject the
  whole batch with `403 cannot ack messages that belong to another employee`
  (unnamed on purpose) and nothing is acked. Acking unknown ids is a silent
  no-op (`acked` counts the requested ids, as before).
- Ack is not required for delivery to progress: an unacked lease simply expires
  and the frame is redelivered. Clients must therefore be idempotent per frame id.

## Outbound idempotency

- `idempotency_key` is required and must be stable for the turn/message being
  sent (the client's turn id or message id).
- Keys are stored per `(employee, idempotency_key)` in
  `channel_outbound_idempotency` and expire after 24h.
- The key is claimed **before** the send, so two concurrent redeliveries cannot
  both post. A repeat returns `{"sent": true, "duplicate": true}` without
  touching the platform leg.
- If the send raises, the claim is released so a retry still delivers.

## Accepted duplicate semantics

- Crash after the platform accepted the message but before the ack/response:
  the key stays claimed, the retry returns `duplicate: true`, and the client
  loses the response — the message is delivered exactly once but the caller may
  not learn it. This is accepted (single-replica gateway, DB-backed claim).
- Frame handling is at-least-once: a client that crashes mid-processing gets the
  frame again after the lease expires and must dedupe on frame id.
- No exactly-once claim is made anywhere in this protocol.

## Client contract (not implemented here)

A client must, per employee: send `Authorization: Bearer <its own token>`, never
call another employee's path, long-poll `/inbox/{self}`, ack only ids it was
handed, and pass a stable `idempotency_key` per outbound turn. The token is
delivered to the client through deployment wiring that does not exist yet (the
runtime Deployment does not mount the connections Secret, and the runtime fork
has no gateway consumption mode).

## Status

Server-side enforcement only. **This PR does not claim an end-to-end working
chain**: `DEERFLOW_EMPLOYEE_NAME`/gateway client mode is absent from the runtime
fork, the gateway leg is not deployed, and `channelGateway.enabled` stays
`false`. Until the client exists and is verified, treat the gateway as
default-off.

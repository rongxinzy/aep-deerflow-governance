# Managed knowledge continuation contract

This documents the implementation in Draft PR #45, paired with
[AaaS #165](https://github.com/rongxinzy/zhiyuanAaaS/pull/165) and
[WeKnora #3](https://github.com/rongxinzy/WeKnora/pull/3). It does not establish
deployment or business acceptance. Product scope remains in
[FR-KB-03](https://github.com/rongxinzy/zhiyuan-docs/blob/main/product/README.md#fr-kb-03).

## Base creation

These fixed administrator-only routes do not adopt pre-existing upstream bases
or expose the generic proxy. Every request requires a verified AEP
`/aep/v1/user/me` response whose top-level `deploymentId` matches
`PORTAL_AEP_DEPLOYMENT_ID`, explicit `PORTAL_KNOWLEDGE_TENANT_ID`, and the key
selected by `PORTAL_KNOWLEDGE_API_KEY_SECRET_FIELD` (default
`WEKNORA_KNOWLEDGE_API_KEY`) in `PORTAL_CREDENTIALS_SECRET`. There is no fallback
to `WEKNORA_API_KEY`. Existing upstream bases are not backfilled into the registry.

`PORTAL_KNOWLEDGE_EMBEDDING_MODEL_ID` must identify an upstream Embedding model.

The stack chart exposes these identifiers under `portal.managedKnowledge`.
Enable it explicitly and configure `tenantID`, `embeddingModelID`, and the
API-key field in the out-of-band `secrets.platformCreds` Secret. Missing tenant
or model identifiers fail rendering. The chart passes the Secret name to
`PORTAL_CREDENTIALS_SECRET`; it never places the managed API key in values or
the Portal environment. Local native acceptance may instead set the same
environment identifiers and use an isolated Kubernetes API server.
Employee apply/update references use this same managed credential selector and
verified tenant, and list only active registered bases in the configured
deployment. They do not require an additional legacy `WEKNORA_API_KEY` field.
Readiness reports `configured`, with inference availability, storage, and parser
still `unverified`: successful API authentication does not prove those systems
are ready. Missing configuration, mismatched tenants, and unavailable
dependencies fail closed.

`POST /api/v1/knowledge/managed/bases` accepts `name`, `description`, and
optional `type`: `document` (default, preserving existing clients) or `faq`.
Other types and unknown fields are rejected before upstream writes. Creation
uses the server's managed embedding configuration. FAQ responses must confirm
the requested type, tenant, name, and generated resource ID before registration.
Existing bases cannot change type through the update endpoint.

Existing routes include readiness; registered base listing/detail/update/delete;
paginated document listing; and multipart file upload. Upload accepts exactly
one field named `file`, capped at 64 MiB; upstream deployment limits may be lower.

Registered base listing and detail also carry the statistics the console list
needs: `knowledge_count`, `chunk_count`, `processing_count`, and `updated_at`
come from the upstream base read that already runs for every listed entry (the
counters are pointers, so a counter upstream omits or reports unreadably is left
out of the response rather than sent as `0`). `updated_at` is normalized to a
UTC RFC 3339 timestamp and dropped when it is not parseable. `scope` is Portal's
own summary of `knowledge_base_grants` for that base (`{"users":n,"teams":m}`)
and is computed in one grouped query per request; a failure to read it omits
`scope` from the response and is logged, because the listing itself is already
validated against the registry and upstream and must not fail for a
display-only aggregate. The console renders any absent field as "not counted"
instead of a zero. `POST` responses (creation) carry whatever the upstream
create call returned for the same fields.

Before each mutation, Portal stores a `pending` audit intent with the real actor
and known resource/parent ID. Failure to persist intent prevents the upstream
write. Network errors are `uncertain`. Outcome persistence failures return the
operation ID, and creation/upload also return a known upstream resource ID.
Successful creation audit and registry insertion share a PostgreSQL transaction;
failure retains the pending intent for reconciliation. Startup metadata DDL is
additive and idempotent; merely merging code does not migrate a running database.

Managed, retrieval and runtime-authorization knowledge routes return one error
envelope: `{"error":{"code":"UPPER_SNAKE","message":"面向管理员的中文"}}`, with the
HTTP status unchanged. The admin console branches on `code` and renders
`message`, so no raw upstream English sentence reaches the UI. A deterministic
upstream rejection keeps its own status and code instead of collapsing into the
generic 502: a content-duplicate upload (WeKnora `409 duplicate_file`) returns
`409 KNOWLEDGE_DUPLICATE_FILE`, an oversize upload returns
`413 KNOWLEDGE_INPUT_TOO_LARGE`, and other upstream 4xx format/type rejections
return `422 KNOWLEDGE_UPSTREAM_REJECTED`. `502` stays reserved for network errors
and upstream 5xx responses, which are the genuinely `uncertain` writes.

Synthetic success DTO examples:

```json
{"data":{"id":"kb-demo-1","name":"产品文档","description":"受控知识库","tenant_id":"10000","embedding_model_id":"embed-demo","type":"document"},"operationId":"kbop-1780000000000000000"}
```

```json
{"data":[{"id":"doc-demo-1","name":"使用指南.pdf","file_name":"使用指南.pdf","parse_status":"processing","created_at":"2026-10-09T05:00:00Z"}],"total":1,"page":1,"pageSize":20}
```

## Audit query

`GET /api/v1/knowledge/managed/audit` requires a freshly authenticated enterprise
administrator. Deployment and WeKnora tenant come from the authenticated
identity and managed configuration; caller-provided tenant/deployment filters
are rejected. It reads only Portal metadata and works without contacting WeKnora.

Optional filters: `operationId`, `actorUserId`, `action`, `resourceId`, `outcome`.
Each occurs once and is at most 256 bytes. `limit` defaults to 50 and accepts
1–100. `cursor` is the positive decimal audit ID returned by the previous page.
Pages are ordered by descending audit ID with `id < cursor`; results include
`items` and `nextCursor` (string or null). Each item contains `cursor`,
`operationId`, `actorUserId`, `action`, `resourceType`, `resourceId`,
`parentResourceId`, `outcome`, `statusCode`, and `createdAt`. No content bodies,
credentials, upstream response bodies, or raw database errors are exposed.

### Runtime audit completion

`POST /internal/v1/knowledge/employees/{name}/authorize` accepts the short-lived
opaque Portal handle in `X-DeerFlow-Knowledge-Context`. Authorization starts a
`pending` runtime audit; it is not yet a successful WeKnora read. The MCP bridge
completes it only after the upstream operation finishes, using the existing
`operation_id`, `outcome`, and `status_code` fields. `succeeded` requires a 2xx
status; `failed` requires a 4xx or 5xx status. Conflicting pairs are rejected.

Every runtime intent stores its verified deployment, configured tenant, real
actor, fixed runtime action, employee name, and SHA-256 of this exact run
context. Completion must match all these bindings and an existing `pending`
row. A handle for another employee or another run cannot complete that row,
even for the same actor. Administrative intents and legacy runtime intents
without these bindings are unavailable to this endpoint; replay is rejected.
The additive startup columns default to empty strings for old rows, deliberately
without backfilling authorization. Context hashes are internal metadata and are
not added to the public audit DTO; plaintext handles are not stored in audits.

## Deletion and binding serialization

Startup adds `knowledge_base_registry.lifecycle_state`, initially `active`.
Portal employee creation, pending-request insertion, binding update, and
approval acquire sorted registry row locks for the requested bases, keeping
locks through the Kubernetes write or durable pending-request insertion.
Deleting a base takes the same lock, checks existing employees and pending
requests, persists the audit intent, then commits `deleting` before contacting
WeKnora. A concurrent binding waits and rechecks state after the commit.

Managed listing, authorization and resource lookup exclude deleting entries.
After an upstream rejection, timeout, disconnect, or audit-result failure, the
barrier remains persisted. Restarting Portal does not restore binding access.
Successful deletion removes the registry row. There is no automatic retry or
reactivation endpoint: an operator must inspect the operation audit and verify
upstream resource state before a reviewed recovery. Image rollback does not
undo additive DDL or deleting states. Direct Kubernetes CR writes bypass Portal
and are outside this serialization guarantee; restrict those permissions.

## Real PostgreSQL regression

Use a disposable database, never a customer database:

```sh
PORTAL_TEST_POSTGRES_URL='<disposable PostgreSQL DSN>' \
  go test -race -run 'TestKnowledge(Lifecycle|RuntimeAudit)Postgres' -v ./...
```

The test owns a unique schema, observes the second transaction waiting in
`pg_locks`, commits deletion, and verifies rejection with a new connection
pool. CI runs it with an isolated PostgreSQL service. Unit tests also cover
reference failures, state/commit failures, upstream disconnects, audit failures,
tenant scoping, bounded pagination and malformed requests.
The runtime-audit regression upgrades an isolated legacy schema with the actual
startup DDL, rejects unbound/admin/foreign operations and completion replay,
and verifies both successful and failed completion outcomes.

The current checked-in local updater only replaces Portal. Complete release
requires matching Admin, Portal, WeKnora app, Python MCP, governed employee
runtime and operator artifacts, protected database backup/restore evidence,
configuration review, live permission revocation, and real ingestion/browser
acceptance. The unavailable machine's uncommitted six-component updater is not
part of the remote source and must not be assumed available.

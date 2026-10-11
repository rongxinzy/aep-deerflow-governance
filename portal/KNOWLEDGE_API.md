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
Before each mutation, Portal stores a `pending` audit intent with the real actor
and known resource/parent ID. Failure to persist intent prevents the upstream
write. Network errors are `uncertain`. Outcome persistence failures return the
operation ID, and creation/upload also return a known upstream resource ID.
Successful creation audit and registry insertion share a PostgreSQL transaction;
failure retains the pending intent for reconciliation. Startup metadata DDL is
additive and idempotent; merely merging code does not migrate a running database.

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
  go test -race -run TestKnowledgeLifecyclePostgres -v ./...
```

The test owns a unique schema, observes the second transaction waiting in
`pg_locks`, commits deletion, and verifies rejection with a new connection
pool. CI runs it with an isolated PostgreSQL service. Unit tests also cover
reference failures, state/commit failures, upstream disconnects, audit failures,
tenant scoping, bounded pagination and malformed requests.

The current checked-in local updater only replaces Portal. Complete release
requires matching Admin, Portal, WeKnora app, Python MCP, governed employee
runtime and operator artifacts, protected database backup/restore evidence,
configuration review, live permission revocation, and real ingestion/browser
acceptance. The unavailable machine's uncommitted six-component updater is not
part of the remote source and must not be assumed available.

# Managed knowledge API (phase A / round 1)

These routes are a fixed, administrator-only adapter for WeKnora v0.8.0. They do not expose the generic proxy and do not adopt pre-existing upstream knowledge bases. Every request requires a verified AEP `/aep/v1/user/me` response whose top-level `deploymentId` matches `PORTAL_AEP_DEPLOYMENT_ID`, an explicitly configured `PORTAL_KNOWLEDGE_TENANT_ID`, and the API key named by `PORTAL_KNOWLEDGE_API_KEY_SECRET_FIELD` (default `WEKNORA_KNOWLEDGE_API_KEY`) in `PORTAL_CREDENTIALS_SECRET`. There is no fallback to `WEKNORA_API_KEY`.

`PORTAL_KNOWLEDGE_EMBEDDING_MODEL_ID` must identify an upstream model whose type is `Embedding` before creating a base. Readiness calls this state `configured`, not `ready`: inference availability is `unverified` because no inference probe is performed. No model, tenant mismatch, or unavailable dependency fails closed. Storage and parser are also reported as `unverified`; successful API authentication is not evidence that those subsystems are ready.

| Method | Path | Contract |
| --- | --- | --- |
| GET | `/api/v1/knowledge/managed/readiness` | Verified tenant, `embeddingModel.state=configured` only means the configured model exists and is typed Embedding; `embeddingModel.availability`, storage and parser remain explicitly unverified. |
| GET / POST | `/api/v1/knowledge/managed/bases` | List only portal-registered bases; create accepts only `name`, `description`. |
| GET / PATCH / DELETE | `/api/v1/knowledge/managed/bases/{baseID}` | Operate only on a registered resource after checking its upstream tenant. PATCH accepts only optional `name`, `description`. Delete checks employee CRs and pending request `spec_json`. |
| GET | `/api/v1/knowledge/managed/bases/{baseID}/documents` | Paginated/query-filtered document list for a registered base. |
| POST | `/api/v1/knowledge/managed/bases/{baseID}/documents/file` | Multipart `file` only, bounded to 64 MiB. |

Before each upstream mutation, the portal persists an audit row with outcome `pending`; it records a known knowledge-base ID in `resource_id`, or as an upload's `parent_resource_id`. If this intent cannot be stored, it does not call WeKnora. Afterward it updates the row with the outcome and resulting resource ID. A network error after a mutation request is `uncertain`, not assumed failure. Outcome-update failures return the already-persisted operation ID; create/upload responses also include a resource ID if known. Create's successful audit outcome and registry insertion are one PostgreSQL transaction; an insertion failure rolls that transaction back while retaining the pending audit intent and upstream ID for reconciliation. The registry and audit DDL are idempotently created by the portal's existing startup migration; this code change does not execute a migration against a real database. Existing WeKnora bases are intentionally not backfilled or shown as managed.

Example success DTOs (synthetic IDs only):

```json
{"data":{"id":"kb-demo-1","name":"产品文档","description":"受控知识库","tenant_id":"10000","embedding_model_id":"embed-demo","type":"document"},"operationId":"kbop-1780000000000000000"}
```

```json
{"data":[{"id":"doc-demo-1","name":"使用指南.pdf","file_name":"使用指南.pdf","parse_status":"processing","created_at":"2026-10-09T05:00:00Z"}],"total":1,"page":1,"pageSize":20}
```

File upload accepts exactly one multipart field named `file`; additional fields are rejected. The portal currently caps the body at 64 MiB; the upstream may enforce a lower deployment-specific limit.

Deletion's CR/request scan prevents deletion when a reference is already present, but it is not yet race-safe against a concurrent employee create/update. A shared knowledge lifecycle lock/transaction and reference validation on employee writes must be completed before claiming complete deletion protection. Team-scoped management, retrieval, original-file download, broader document lifecycle, and a paginated audit-query endpoint are not part of this round.

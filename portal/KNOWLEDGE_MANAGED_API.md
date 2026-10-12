# Managed knowledge lifecycle API

These endpoints are the fixed, admin-only adapter used by the Zhiyuan console.
They are not a generic upstream proxy. Every document read/action verifies the
AEP deployment, configured WeKnora tenant, registered knowledge base, and live
document `tenant_id` + `knowledge_base_id` before forwarding. Mutations write a
persistent audit intent before calling WeKnora and return an operation ID.

| Method | Portal endpoint | WeKnora operation |
| --- | --- | --- |
| POST | `/api/v1/knowledge/managed/bases/{baseID}/documents/url` | `POST /knowledge-bases/{id}/knowledge/url` |
| POST | `/api/v1/knowledge/managed/bases/{baseID}/documents/manual` | `POST /knowledge-bases/{id}/knowledge/manual` |
| GET | `/api/v1/knowledge/managed/documents/{documentID}` | `GET /knowledge/{id}` |
| GET | `/api/v1/knowledge/managed/documents/{documentID}/spans` | `GET /knowledge/{id}/spans` |
| GET | `/api/v1/knowledge/managed/documents/{documentID}/chunks` | `GET /chunks/{knowledge_id}` |
| PUT | `/api/v1/knowledge/managed/documents/{documentID}/enable-status` | `PUT /knowledge/{id}/enable-status` |
| GET | `/api/v1/knowledge/managed/documents/{documentID}/chunks/{chunkID}/images/{index}` | `GET /knowledge/{id}/chunks/{chunk_id}/images/{index}` |
| POST | `/api/v1/knowledge/managed/documents/{documentID}/reparse` | `POST /knowledge/{id}/reparse` |
| POST | `/api/v1/knowledge/managed/documents/{documentID}/cancel-parse` | `POST /knowledge/{id}/cancel-parse` |
| DELETE | `/api/v1/knowledge/managed/documents/{documentID}` | `DELETE /knowledge/{id}` |
| GET | `/api/v1/knowledge/managed/documents/{documentID}/preview` | `GET /knowledge/{id}/preview` |
| GET | `/api/v1/knowledge/managed/documents/{documentID}/download` | `GET /knowledge/{id}/download` |

URL input accepts only `url`, `title`, `file_name`, and `file_type`. It rejects
non-HTTP(S), credentials, fragments, and obvious localhost/private/link-local
DNS results. This API preflight is only an early filter: it does not duplicate
the complete special-purpose address classification and is not the connection
security boundary. At pinned WeKnora source commit
`1edcd54b43606d9079bb36650efe3f68707a79ea`, the URL fetcher uses an SSRF-safe
transport that validates resolved IPs at dial time, dials the checked address,
and enforces redirect and fetch-timeout policies. This protection applies only
if the deployed WeKnora image contains that implementation or an independently
verified equivalent. The current local WeKnora checkout is at a different
commit, and the running image has no verified source commit; therefore real URL
imports must not be considered security-accepted until the deployed image is
matched to the pinned commit (or equivalent controls are proven). Redirects
from the WeKnora API client are never followed.
Manual input accepts only `title` and `content`, with a 1 MiB body limit and a
fixed published status. Duplicate upstream conflicts map to a stable 409;
upstream bodies and error text are not returned.

Document and span/chunk responses are projected onto explicit DTO fields.
Only `resource://` image references are projected as same-origin Portal paths;
external URLs and raw storage paths are omitted. Image reads verify the document
and registered KB, fetch the exact chunk to verify tenant/KB/document and handle
membership, then call WeKnora's fixed authorized image-index route. The Portal
accepts only PNG/JPEG/GIF/WebP bytes matching the sniffed MIME, stages at most
20 MiB before sending headers, shares the single bounded spool slot with file
transfers, and sends `nosniff`/`no-store`. It does not proxy arbitrary URLs or
storage paths. The deployed WeKnora image must contain the new resource-binding
and authorized-read implementation; an unverified image tag is not evidence.
Original document preview/download
is proxied only after ownership verification, uses an allowlisted MIME type,
safe attachment name, `nosniff`, and no-store. The adapter does not trust the
document's `file_size` metadata as proof of transfer size: URL/manual documents
may not have a reliable source size, and stored metadata can become stale. It
reads the actual response into a temporary file (mode 0600 on POSIX; on Windows
it inherits the ACL of the protected temp directory) before sending any
response headers, rejects empty/incomplete/over-256-MiB bodies, and then streams
the verified bytes to the browser. This supports the pinned WeKnora handlers'
chunked `c.Stream` responses, which do not set `Content-Length`. Only one file
transfer can spool per portal process at a time (maximum temporary disk use:
256 MiB); additional concurrent requests fail with `KNOWLEDGE_FILE_BUSY` and
can be retried. The temporary file is closed and removed on success and handled
errors. A process crash can leave a `zhiyuan-managed-knowledge-*` file behind,
so the host temporary directory needs at least 256 MiB free space, restricted
access, and routine stale-file cleanup. There is no arbitrary URL fetch
endpoint.

Document status uses WeKnora's lifecycle API with only `{enabled: boolean}`.
Disabling is permitted while parsing and persists a manual intent that finalizers
must preserve; enabling is permitted only after parsing completes. The Portal
checks the returned document, tenant, KB, and requested status and writes an
audit intent/outcome. Library-level enable/disable remains unavailable. The
original file can be previewed inline when it is a supported image/PDF/text file;
chunk images use the constrained endpoint above.

Stable mutation response:

```json
{
  "data": {
    "id": "synthetic-document-id",
    "tenant_id": 10000,
    "knowledge_base_id": "synthetic-registered-base-id",
    "type": "url",
    "title": "Example",
    "source": "https://example.org/guide",
    "parse_status": "processing",
    "enable_status": "disabled"
  },
  "operationId": "kbop-synthetic"
}
```

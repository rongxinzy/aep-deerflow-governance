# Local de-portal updater

This is an independent updater for **only** the `digital-employees/de-portal`
Deployment. It does not replace or call the existing desktop
`更新企业版代码.cmd`, `update-from-source.ps1`, or `update-source-images.py`.
It never updates operator, AEP, AaaS, WeKnora, Secrets, PVCs, or database
objects directly.

The PowerShell entrypoint defaults to `plan`. The plan reads the selected
Deployment, schedulable node platform, and local containerd image inventory,
then writes a sanitized plan under the explicitly supplied `RunRoot`. It does
not patch Kubernetes. No raw Deployment JSON, Secret data, literal environment
values, credentials, or database URLs are persisted. The plan records safe
image/pull-policy, UID/resourceVersion, environment variable names, references,
volume/PVC/Secret names, and the target-only rollback information.
Every Kubernetes read and write pins `KUBECONFIG` to the local WSL k3s
configuration, ignoring inherited kubeconfig settings. Before proceeding, the
helper requires a loopback API endpoint and exactly one Ready, schedulable
linux/amd64 node, because `imagePullPolicy: Never` depends on that node's local
image cache.

## Source and artifacts

Use the governance worktree path and a full expected source SHA. The helper
requires branch `feat/knowledge-phase-a`, exact `HEAD == ExpectedCommit`, and a
clean worktree including untracked files. It does not fetch, pull, checkout, or
commit. Keep each `RunRoot` outside the source repository and empty before
`plan`; it contains the sanitized plan, build output, prepared OCI overlay, and
deployment journal. Protect the directory as local operational data.

All commands below run from PowerShell. Paths are translated into Ubuntu WSL;
the helper invokes `k3s` there. Replace the example SHA with the reviewed,
committed governance source SHA. Use a new empty run directory for each
attempt.

```powershell
$repo = 'D:\rongxin\aep-governance-knowledge-proxy'
$commit = '<40-character-reviewed-governance-commit>'
$run = 'D:\rongxin\zhiyuan-deployment.local\portal-runs\phase-a-001'

# Default behavior is also plan; no Deployment patch is issued.
& "$repo\scripts\update-local-portal.ps1" -RepositoryPath $repo -ExpectedCommit $commit -RunRoot $run

# Run in separate, explicit stages after reviewing plan.json.
& "$repo\scripts\update-local-portal.ps1" -Mode build -RepositoryPath $repo -ExpectedCommit $commit -RunRoot $run
& "$repo\scripts\update-local-portal.ps1" -Mode prepare -RepositoryPath $repo -ExpectedCommit $commit -RunRoot $run

# Deploy is the only stage that patches Kubernetes and requires DB-backup confirmation.
& "$repo\scripts\update-local-portal.ps1" -Mode deploy -RepositoryPath $repo -ExpectedCommit $commit -RunRoot $run -ConfirmDatabaseBackup

# Manual recovery for the same journal, if automatic rollback did not complete.
& "$repo\scripts\update-local-portal.ps1" -Mode rollback -RepositoryPath $repo -ExpectedCommit $commit -RunRoot $run
```

`build` uses the installed Windows `go.exe` without installing a WSL toolchain:
it cross-compiles the Linux/amd64 test binary, runs that binary inside Ubuntu
WSL, runs `go vet`, then builds a linux/amd64, CGO-disabled Portal binary.
The test executable is copied into a temporary WSL filesystem path and removed
afterward. `prepare` exports the current Portal image already cached in local
containerd, overlays only `/de-portal`, and imports a uniquely tagged local
image; it does not pull from a registry or change a Deployment. Both source and
base OCI platform are checked. The short Deployment image reference is
normalized only for cache lookup/export, while the exact original reference is
retained for rollback; the cached base manifest digest is recorded and checked
again before prepare. Plan/build/prepare artifacts are not a claim
that the application is deployed or accepted.

## Safety and rollback

Before deploy, verify and back up the Portal database, then explicitly pass
`-ConfirmDatabaseBackup`. The database includes persisted AEP sessions/tokens
and CSRF material: the backup must follow the site's credential-protection and
encryption/access-control requirements. Do not create an unprotected plaintext
dump or place it in `RunRoot`, the repository, or logs. The helper records an atomic journal before patching
and marks the operation as potentially applied before sending the patch. The
patch contains only the named Portal container image, `imagePullPolicy`, and
one update annotation, with Deployment UID/resourceVersion preconditions.
Before deploy and rollback it rechecks UID, image, pull policy, and the
run-specific update marker to avoid overwriting a recreated Deployment or
another operator's image update. A patch or rollout
failure attempts rollback of only `de-portal` to the exact old image and
pull-policy, including when rollout timed out after the patch. A failed
rollback is journaled and reported with the run ID; it is not hidden.

The Portal startup migration adds `knowledge_base_registry` and
`knowledge_audit_events` metadata tables and the audit `parent_resource_id`
column/index. Restoring the old image does **not** reverse those additive DDL
changes or delete registry/audit rows. The updater never deletes or recreates
the Portal database, Secret, PVC, or knowledge data.

The plan deliberately does not patch application environment variables. The
current chart does not explicitly configure `PORTAL_KNOWLEDGE_TENANT_ID` or
`PORTAL_KNOWLEDGE_EMBEDDING_MODEL_ID`; the API-key Secret field defaults to
`WEKNORA_KNOWLEDGE_API_KEY`, with no silent fallback. The existing chart
references `de-platform-creds`; Secret values are never read by this updater.
The plan reports missing env-variable names but does not infer a tenant, create
a model, or alter a Secret. A model ID must refer to a separately provisioned
Embedding model. Even when the model is listed, readiness reports it as
`configured` with inference availability `unverified`; storage and parser
remain `unverified` until independently tested. Do not treat the updater's
health check or rollout as knowledge readiness or user acceptance.

Offline helper tests (no cluster access):

```powershell
wsl.exe -d Ubuntu -- python3 -m unittest /mnt/d/rongxin/aep-governance-knowledge-proxy/scripts/test_update_local_portal.py -v
```

The tests mock subprocess and Kubernetes responses. They cover source branch,
SHA and dirty-tree guards, metadata-only snapshots, fixed target, database
backup confirmation, UID/image drift, successful patch, patch/rollout failure,
automatic rollback and rollback failure. They do not deploy or validate a live
cluster.

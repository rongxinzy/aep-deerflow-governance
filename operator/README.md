# de-operator — DigitalEmployee CRD Operator

`de-operator` reconciles `DigitalEmployee` custom resources
(`digitalemployee.zhiyuan.io/v1alpha1`) into the child Kubernetes workloads
that run one governed agent instance.

## Layout (kubebuilder v4)

- `api/v1alpha1/` — CRD types (`digitalemployee_types.go`), generated deepcopy (`zz_generated.deepcopy.go`)
- `internal/controller/` — reconciler: lifecycle (`digitalemployee_controller.go`), child orchestration (`employee_reconcile.go`), config (`employee_config.go`), workloads (`employee_workload.go`, `a2a_render.go`), native model client (`model_runtime*.go`)
- `internal/openviking/` — OpenViking admin client (register/regenerate/delete user)
- `cmd/main.go` — manager entrypoint (namespace-scoped cache, `--namespace digital-employees`)
- `config/` — kustomize (CRD, RBAC, manager deployment)
- `deploy/` — plain manifests applied directly on k3s: `crd.yaml` (generated — never hand-edit; regenerate with `make manifests`), `operator.yaml`

## Commands

```bash
make manifests generate   # regenerate CRD + deepcopy after type changes
go build ./... && go vet ./...
go test ./...             # Ginkgo/Gomega suite (envtest-free: fake client + httptest)
```

## Key invariants

- CR name = business ID = OpenViking memory user. The operator auto-provisions
  the memory user at reconcile and deletes it via finalizer on CR deletion.
- The memory key + gateway internal-auth token live in the `de-<name>-memkey`
  Secret; existing values are authoritative (mint-once, never rotate).
- Runtime Deployments run with authentication ON (no `DEER_FLOW_AUTH_DISABLED`).
- A typed client silently drops struct fields it does not know: after any CRD
  field change, the operator must be rebuilt and redeployed in the same release.

## Model workload identity delivery (opt-in)

`MODEL_ACCESS_TOKEN_AUDIENCE` (Helm: `modelWorkloadIdentity.audience`) enables
identity delivery for DigitalEmployee runtime Pods. It defaults to empty and
does not change the existing model authentication path. Upgrade the operator
RBAC together with the binary; the added namespace-scoped ServiceAccount verbs
do not include token issuance or RoleBinding writes.

With an explicit, dedicated gateway audience such as `urn:zhiyuan:model-gateway`,
the operator creates a ServiceAccount owned by each employee CR. Its name is
derived from the namespace and immutable employee UID, not the human owner or
the reusable employee name. Reconciliation refuses foreign/unowned accounts.
No Role or RoleBinding is assigned to employee accounts. Both account and Pod
disable automatic Kubernetes API credential mounting.

The runtime receives only a read-only projected token file at
`/var/run/secrets/zhiyuan/model-access/token`, advertised as
`ZHIYUAN_MODEL_TOKEN_FILE`. Kubernetes issues and rotates the Pod-bound token;
the requested lifetime is one hour, with the actual signed expiry determined
by the API server. The directory mount uses no subPath, so file replacement is
visible without restarting the Pod. A consumer must reread the file when making
requests; caching its contents indefinitely defeats rotation. Init containers
and the separate A2A Deployment receive no model token from this feature.

An ordinary reconcile leaves the SA and Pod template unchanged. Recreating the
SA changes a template UID annotation and rolls the runtime onto the replacement
identity. Deleting/recreating an employee produces a different SA name; its old
SA is garbage-collected through the owner reference. Disabling the option
removes the projection on the next runtime rollout; owned accounts remain until
employee deletion. Never enable the option using the Kubernetes API audience.

Identity delivery is **not model authorization**. To explicitly declare the
employee's separate AEP `kind=agent` account, set `spec.modelAccess.agentId` and
`spec.governance.deploymentId` with the dedicated operator audience enabled.
Neither `spec.owner` nor `governance.defaultAEPUser` is a fallback agent identity.
The optional field is omitted for legacy employees; no account is automatically
created or assigned permissions. Existing resource assignments through effective
roles remain authoritative in AEP.

After checking live SA ownership, the operator publishes credential-free
`status.modelIdentity`: observed generation, agent/deployment IDs, employee UID,
SA name/UID and audience. It records identity observation even while the runtime
is Pending; it does not attest runtime readiness, a valid token, an authorization
grant or data-plane adoption. The whole status is compared before writing so
an SA replacement persists even when phase/message do not change, without an
unchanged-status watch loop. Removing the mapping or a reconciliation error
clears the observation. Deletion must be checked directly, not inferred from a
possibly retained status.

Kubernetes rejects changing an existing `modelAccess.agentId` in place. A trusted
AEP lifecycle writer must **also** reread the live CR and SA, reject deletion,
stale generation and foreign ownership, validate deployment/account kind in AEP,
and enforce immutable identity history. Removing/readding an optional field must
not permit rebinding the same employee UID to another agent or resurrecting a
retired SA UID. CR/status write access is privileged; never treat this declaration
or a user-supplied header as authentication. Identity delivery alone does not
implement that writer, publish workload ModelAccessGrant CRs or configure
Authorino. Native model client configuration is a separate opt-in below; the
remaining business and gateway integration stays tracked in
[#131](https://github.com/rongxinzy/aep-deerflow-governance/issues/131),
dependent on [AEP #337](https://github.com/rongxinzy/Agent-Enterprise-Protocol/issues/337)
and [AEP #338](https://github.com/rongxinzy/Agent-Enterprise-Protocol/issues/338).
No human login session, provider API key or static platform model key is
introduced by this feature. Existing application credential references remain
part of the legacy path; delivery alone does not prove a credential-free Pod.

The default Ginkgo suite checks isolation, ownership, idempotency and rollout
behavior. For a live identity-only check, create a **disposable** cluster,
install `deploy/crd.yaml` and preload `node:24-alpine`, then run:

```bash
EMPLOYEE_IDENTITY_KUBECONFIG=/path/to/disposable/kubeconfig \
go test ./internal/controller -run TestControllers \
  -ginkgo.focus='Employee workload identity Kubernetes API' -ginkgo.v
```

The check creates and deletes its own namespace. It verifies the actual kubelet
projection and native TokenReview audience/SA-UID behavior without printing a
bearer. TokenReview uses a separate in-memory Pod-bound TokenRequest; the
mounted file is decoded only for non-secret claim facts. This is not an end to
end employee model-request, cache-invalidation or quota test.

The explicit mapping/status admission check also runs the typed full reconcile
entry point against that disposable API. Install the KEDA `ScaledObject` and
`TriggerAuthentication` CRDs (the normal employee lifecycle dependency), then
select `-ginkgo.focus='Employee explicit identity Kubernetes API'`. It verifies
schema preservation, generation changes, immutable in-place agent mapping,
real SA-UID replacement while Pending, observation removal and status write
stability. It does not deploy a new operator image or prove runtime model access.

## Native model client configuration (additional opt-in)

To consume the projected identity in the main chat model client, configure the
trusted operator `MODEL_ACCESS_GATEWAY_URL` (Helm:
`modelWorkloadIdentity.gatewayURL`) as the shared model gateway's absolute
`http(s)://host[:port]/v1` endpoint. The operator also requires the dedicated
`MODEL_ACCESS_TOKEN_AUDIENCE` and the employee's explicit `spec.modelAccess`.
Either missing opt-in preserves the existing model client configuration. The
gateway option defaults to empty; it does not switch existing employees.

The runtime image must include DeerFlow's
`deerflow.models.token_file:TokenFileChatOpenAI` client
([DeerFlow #34](https://github.com/rongxinzy/deer-flow/pull/34)). The operator
renders `api_key_file` pointing to the projected file, removes `api_key`, and
sets the trusted gateway endpoint. The client rereads the file per request so
Kubernetes rotation remains visible. Do not enable this option with an image
that lacks the token-file client, or a gateway that still expects a human model
JWT. Never substitute the conversation owner's credentials.

`spec.runtime.models` (or the legacy singular model field) selects stable AEP
wire model IDs. Native mode rejects duplicate/invalid IDs, provider aliases,
static API-key env declarations, destination overrides and non-OpenAI protocols.
The existing per-user `ModelTools.GatewayURL` integration is also rejected in
native mode: its image/speech/video tools require a separate workload-identity
integration. Configuring a model here does not grant access. Effective roles,
verified license state and the trusted workload publisher remain authoritative
in AEP; changing roles does not automatically rewrite this runtime model list.

Each model receives an `x-aep-access-binding` routing hint derived from the live
owned ServiceAccount observation. It contains no credential or authorization;
native TokenReview and published grants enforce access. The immutable tuple
matches AEP's binding-key convention, covered by pinned vectors in
`internal/controller/testdata/model-access-binding-v1.json`. ServiceAccount UID
replacement changes the hint and the config checksum in the same reconcile,
rolling the runtime consistently. Owner changes do not change this identity.

Runtime enablement additionally requires the trusted AEP workload observer,
licensed workload publication and native gateway enforcement. This configuration
stage alone does not attest those deployments, cache adoption, independent quota
or real-provider success. Full employee-to-provider acceptance remains in
[#131](https://github.com/rongxinzy/aep-deerflow-governance/issues/131);
`adoptionGate=false` until that acceptance completes. Authorino's accepted cold
cache exception still applies per serving replica and after restart/eviction.

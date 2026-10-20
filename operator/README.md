# de-operator — DigitalEmployee CRD Operator

`de-operator` reconciles `DigitalEmployee` custom resources
(`digitalemployee.zhiyuan.io/v1alpha1`) into the child Kubernetes workloads
that run one governed agent instance.

## Layout (kubebuilder v4)

- `api/v1alpha1/` — CRD types (`digitalemployee_types.go`), generated deepcopy (`zz_generated.deepcopy.go`)
- `internal/controller/` — reconciler: memory-user lifecycle (`digitalemployee_controller.go`), child rendering (`employee_reconcile.go`, `a2a_render.go`)
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

This phase delivers identity, **not model authorization**. It does not map the
employee to an AEP `kind=agent` account, publish workload ModelAccessGrant CRs,
configure Authorino TokenReview/caching, or make DeerFlow consume this file.
Those are the next steps in [#131](https://github.com/rongxinzy/aep-deerflow-governance/issues/131),
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

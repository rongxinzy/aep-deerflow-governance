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

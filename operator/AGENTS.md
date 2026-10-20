# AGENTS.md — de-operator

Agent guidance for the `operator/` kubebuilder project. See `README.md` for layout.

## Testing discipline (mandatory)

Tests use **Ginkgo + Gomega** specs with the fake client + httptest OpenViking
stub (see `internal/controller/digitalemployee_controller_test.go`). Do not
introduce plain `testing`-style table tests here.

Run: `go test ./...` from `operator/`.

## Regeneration

After changing `api/v1alpha1/digitalemployee_types.go`:

1. `make manifests generate` (CRD + deepcopy; float fields need
   `crd:allowDangerousTypes=true`, already set in the Makefile target)
2. `python3 ../scripts/sync-crds.py` (run from `operator/`; the script itself
   resolves its own repo, so any cwd works) — regenerates `deploy/crd.yaml` and
   the `charts/zhiyuan-stack/crds/` copies from `config/crd/bases/*.yaml`. Do not
   hand-copy: CI runs the same script with `--check` and fails on drift.
3. Rebuild + redeploy the operator image — a typed client drops unknown
   fields, so CRD and operator must ship together.

## Conventions

- Reconcile logic lives in `internal/controller/`: lifecycle/finalizer/
  secrets in `digitalemployee_controller.go`, child orchestration in
  `employee_reconcile.go`, configuration in `employee_config.go`, and workload
  rendering in `employee_workload.go` and `a2a_render.go`. Native model runtime
  configuration and its immutable routing hint live in `model_runtime*.go`.
- Secret writes use `Data` (not `StringData`) so the fake client used by the
  specs behaves like the reconciler's own idempotency read-back.
- The operator is namespace-scoped (`--namespace`), blast radius matches the
  employee workload namespace.

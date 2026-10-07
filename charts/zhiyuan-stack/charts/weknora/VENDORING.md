# Vendoring notes — WeKnora chart

Source: fork `rongxinzy/WeKnora`, branch `zhiyuan/helm-extra-env`
(commit `606a7dc2`, 2026-10-07), directory `helm/`. Copied verbatim, then
patched (each patch is marked with a `Vendoring patch` / `收编` comment in
the touched file):

1. **Namespace override** — every `{{ .Release.Namespace }}` renders through
   the new `weknora.namespace` helper (`templates/_helpers.tpl`); empty
   value keeps the release namespace (standalone installs unchanged). The
   umbrella sets `weknora.namespace: weknora`. This includes the secret
   `lookup` in `templates/secrets.yaml` — it must search the same namespace
   it renders into, or upgrades would re-roll `SYSTEM_AES_KEY`.
2. **`templates/app.yaml` strategy** — the hardcoded RollingUpdate becomes
   values-driven (`app.strategy.type`, default unchanged). The umbrella sets
   `Recreate`: app mounts the RWO data-files PVC, which RollingUpdate can
   dual-mount during a same-node rollover (2026-10-06 incident class).
3. **`templates/app.yaml` duplicate `extraEnv`** — the fork rendered
   `.Values.app.extraEnv` twice (top-of-list loop + trailing `toYaml`); live
   deployments carry the duplicated `SSRF_WHITELIST_EXTRA` entry. Removed
   the loop, kept the `toYaml` block.
4. **`values.yaml`** — `namespace: ""` key added; duplicate `extraEnv` key
   collapsed to one; `app.strategy` block added; redis image repository
   switched to the Harbor DNS mirror (2026-10-07 registry standardization);
   the fork's `existingClaim: weknora-*-lh` values reverted to `""` (those
   are cicd migration facts and live in the umbrella overlay instead — fresh
   installs let the chart create `weknora-postgres` / `weknora-redis` /
   `weknora-data-files` on the longhorn storage class).
5. **`app.kubernetes.io/instance` selector label** — `weknora.selectorLabels`
   now honors an `instanceName` value (default `.Release.Name`). Deployment
   selectors are immutable: adopting the live objects into the umbrella
   release ("zhiyuan") must keep the label at "weknora" — the umbrella pins
   `weknora.instanceName: weknora`.

To re-vendor after a fork update: copy `helm/` over this directory and
re-apply the five patches above (the inline comments mark every site).

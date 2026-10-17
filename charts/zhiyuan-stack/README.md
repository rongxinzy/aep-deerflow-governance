# zhiyuan-stack

知远数字员工平台的伞形 Helm chart：AEP 控制面（ns `aep-system`）+ DE operator +
portal + 对话前端 + 管理台 + OpenViking 记忆栈 + WeKnora 知识库（vendored 子图，
`charts/weknora`，见 `charts/weknora/VENDORING.md`）+ 在线 GLM 的 Higress 模型路由。

## 前置（不在本 chart 内）

- Higress 本体（helm 安装，`higress-system`；reconciler 的 RBAC 需要该 ns）
- `aep-license` Secret（`aep-system`）—— 客户签名许可证（`license.zylic` +
  `license-trusted-keys.json`），由交付流程注入，不入库；缺失时控制面
  CreateContainerConfigError
- 凭据 Secret（chart 只引用名称）：`de-platform-creds`、`de-portal-db`
- 镜像已导入集群可达的 registry（Harbor，DNS 引用，见
  `zhiyuan-docs/guides/cicd-environment.md`）

## 安装 / 升级

```bash
cp values-cicd.example.yaml values-cicd.local.yaml   # 填真实凭据，勿入 git
helm upgrade --install zhiyuan . -f values-cicd.local.yaml \
  -n digital-employees --create-namespace --take-ownership
helm rollback zhiyuan <rev> -n digital-employees      # 一键回退
```

- `--take-ownership`：收编已存在的同名资源（迁移既有环境必需；helm ≥3.17；
  cicd 节点 helm 3.18.4 可用，本机旧版需升级或在节点上执行）。
- **收编既有 `weknora` release 后，只删 release 记录**：
  `kubectl -n weknora delete secret sh.helm.release.v1.weknora.v1`。
  **禁止 `helm uninstall weknora`** —— uninstall 按旧 manifest 删除对象且不校验
  归属，会连已收编的工作负载和 3 个 local-path 备份 PVC
  （weknora-postgres/-redis/-data-files）一起删掉。
- **占位符守卫**：`aepStack` 敏感项与 WeKnora 凭据里任何 `<...>` 占位符没被
  values-`<env>`.local.yaml 的真实值替换时，渲染直接失败（防止把无效配置静默
  写进 ConfigMap/Secret，或让控制面在下次重启时 CrashLoop）。
- `aepStack.modelGatewayBaseUrl` 与 `agentControlAdvertiseUrl` 必须是**集群外
  可达**地址（control-service 启动校验拒绝 `.svc.cluster.local`；模板期另有
  同类拦截）。cicd 用节点 IP + NodePort（30181/30182）。

## 版本策略

- ghcr.io 是版本源（CI 按组件 tag 出镜像：`operator-v<SemVer>` → `de-operator`，
  `portal-v<SemVer>` → `de-portal`；PR 只构建不推送；推送时附 SBOM、provenance，
  digest 写入 Actions job summary，pin 用 `repo:tag@digest`）；集群统一从集群内 Harbor 拉取
  （DNS 名 `harbor.zhiyuan.local:30443/zhiyuan`，2026-10-07 标准化），Harbor
  代理回源 ghcr/dockerhub，jfrog 降级为归档回退。镜像引用一律 DNS，禁 IP。
- 员工运行时镜像 deerflow-gov 是血统镜像：新版本由 `build-deerflow-gov`
  workflow 在上一版之上构建，勿从零重建。

## 已知边界

- operator 配置变更不滚动运行时 pod：改 employees 值后需重启对应运行时。
- RWO 卷工作负载一律 `strategy: Recreate`（postgres/rustfs/weknora-app；
  2026-10-06 双写事故后固化）——新增挂 RWO PVC 的组件时先设 Recreate。
- WeKnora 子图为 vendored fork chart：升级子图需按 VENDORING.md 重放补丁；
  Deployment selector（`app.kubernetes.io/instance`）不可变，收编时以
  `weknora.instanceName` 保持与活体一致。
- 旧 weknora 数据卷（`weknora-postgres/-redis/-data-files`，local-path）迁
  Longhorn 后保留为备份，不从 chart 渲染（overlay 里 `existingClaim` 指向
  `*-lh`）。

## Observability 底座（observability.*，2026-10-09 产品化）

默认 **opt-in**（`observability.enabled: false`）。置 true 即部署：

- **监控**（ns `monitoring`）：Prometheus（AEP/运行时/节点/ksm 抓取 + 6 条告警
  规则：DiskPressure/磁盘低/NotReady/抓取目标消失/重启/AEP 5xx；存储默认
  local-path——指标是软数据，不受 Longhorn 故障面牵连）、Grafana（NodePort 30161，
  匿名只读——生产必须覆盖 `grafana.adminPassword`）、node-exporter、
  kube-state-metrics。
- **告警评估器**：每 2 分钟把 Prometheus 的 firing 集合转发到
  `observability.alerts.webhookUrl`（企微群机器人 text 格式）。留空时只打 pod
  日志。真实 URL 属于凭据：只放 values-`<env>`.local.yaml（gitignored），
  **不要**装完后用 kubectl patch 临时写 Secret——下一次 helm upgrade 会把它
  重置回 values 渲染值。已知增强点：启用推送后会每 2 分钟重复推送整个 firing
  集合（暂无静默/去重）。
- **日志**（ns `logging`）：单节点 ES + Kibana（NodePort 30160）+ fluent-bit
  （SA/RBAC 齐备的 kubernetes filter → ES 文档带 pod/namespace 元数据）。采集
  范围 = `observability.logging.fluentBit.namespaces`（共享集群纪律：不列不采）。
- **审计**：audit-shipper CronJob。脚本本体只有一份：
  `charts/zhiyuan-stack/files/ship.py`（模板用 `.Files.Get` 装载；收编前的
  参考实例 `k3s/audit-shipper.yaml` 由 `scripts/test_audit_shipper.py` 守住
  逐字符一致）。按各自接口的 schema 分别采集：管理审计
  `/aep/v1/admin/audit/operations`、认证日志 `/aep/v1/admin/audit/authentication`、
  控制事件 `/aep/v1/admin/control-events`、遥测 `/aep/v1/admin/events`，加 AEP
  用户/模型健康与 portal 员工/申请快照。每个分页源都消费 `nextCursor` 直到取完；
  一页入库确认后才把游标写进 `zhiyuan-shipper-state` 索引，失败的那页下轮
  重读（事件带稳定文档 id，重读是 upsert 不是重复）。bulk 逐项判定：429/5xx
  只重试被拒文档，永久拒绝会打印文档 id 并以非零退出，同时触发
  `AuditShipperFailed` 告警——**CronJob 成功不再等同于归档完整**。
  管理员口令来自一次性 Secret `observability.auditShipper.credentialsSecret`
  （不入 chart，同 de-platform-creds 纪律）；该账号需具备 `audit.read`（管理
  审计/认证日志）、`events.read`（事件/控制事件）、`users.read`、`models.read`
  ——缺哪个权限，日志就报哪个源失败，不会静默少采。
- **节点日志上限**：node-log-cap DaemonSet（清 >200MiB 轮转档案、活跃文件超 1GiB
  截留尾部）。journald 的 SystemMaxUse 是主机单元，维持
  `k3s/node-log-cap/README.md` 的手工步骤。

### 收编既有环境（先读再置 true）

已按 `k3s/*.yaml` 跑着实例的环境（cicd）直接升 true 会因对象已存在而失败：

1. 对每个将收编的对象打 helm 归属标注：annotations
   `meta.helm.sh/release-name: <release>` + `meta.helm.sh/release-namespace: <ns>`，
   label `app.kubernetes.io/managed-by: Helm`；
2. 先 `helm template` diff 活体（尤其 ES StatefulSet 的不可变字段），差异先对齐；
3. `helm upgrade` 收编后 `kubectl -n monitoring rollout restart deploy/prometheus`
   （规则文件变更需重启或启用 lifecycle reload）；
4. cicd 的 systemd 版 node-log-cap 与 DaemonSet 版**二选一**（避免双写者）：
   收编时在 local values 关掉 DS（`observability.nodeLogCap.enabled: false`）
   或先卸载主机单元。

## 加固（P0）：网络隔离、Pod 安全、资源边界、异地备份

全部为增量配置，旧 values 无需修改即可升级；每一项都有开关可回退。

| 项 | values | 默认 | 回退 |
|---|---|---|---|
| NetworkPolicy（默认拒绝入站 + 白名单；运行时出站白名单） | `networkPolicy.*` | 开 | `networkPolicy.enabled=false` |
| AEP `/internal` 对运行时不可达（Cilium L7） | `networkPolicy.aepInternalGuard` | `cilium` | `none`（仅 L4，/internal 只靠令牌） |
| 运行时默认 requests/limits 与 securityContext | `runtimeDefaults.*` | 开 | 置为 `{}` |
| PSA 命名空间标签 | `podSecurity.*` | baseline enforce / restricted warn+audit | `podSecurity.enabled=false` |
| LimitRange / ResourceQuota（主命名空间） | `resourceBoundaries.*` | LimitRange 开 / Quota 关 | 各自 `enabled=false` |
| PostgreSQL 异地备份 CronJob | `backup.postgres.*` | 配了 `s3.bucket` 才渲染 | 清空 `s3.bucket` |

### 网络策略说明

- 默认拒绝入站覆盖：`aep-system`、主命名空间、记忆命名空间；`observability.enabled` 时含 monitoring/logging（`networkPolicy.observability`）；WeKnora 命名空间默认不覆盖（`networkPolicy.weknora`，子图内部调用未盘点）。
- 已通过 NodePort 对集群外开放的端口（control-service 8080、agent-control 8080、gateway-authorizer 8090、portal 8090、管理后台 5173/5174、chat-ui 3000、grafana、kibana、a2a 8102）入站不限来源：这些端口本来就面向集群外，访问控制在应用层。
- 数字员工运行时（`digitalemployee.zhiyuan.io/name` 存在且 component≠a2a）：
  - 入站 :8001 只接受 portal、operator、channel-gateway、a2a 适配器、监控命名空间（以及 `networkPolicy.runtime.extraIngressApps`）；员工之间不能直连。
  - 出站只允许：DNS、aep-system/postgres:5432、gateway-authorizer:8090、Higress 命名空间、openviking:1933、WeKnora 命名空间、channel-gateway、a2a 适配器:8102、集群外地址（排除 `clusterCIDRs`，`allowExternalEgress=false` 可关）。rustfs、其他员工、portal 不在白名单。
  - control-service 只经 CiliumNetworkPolicy 放行 `aepAllowedPaths`（默认 `/aep/v1/.*`、`/healthz`、`/readyz`），`/internal/*` 被 L7 代理拒绝（HTTP 403）。
- 主命名空间里不由本 chart 渲染的 Pod（zhiyuanBotUI、pi-companion、个人工作区等）默认保留入站全开（`openUnmanagedMainNamespacePods=true`），用 Hubble 盘点后再收紧。
- KEDA 唤醒触发器从 `keda` 命名空间直连 postgres，已在 `postgresExtraFromNamespaces` 放行。

### 运行时 Pod 安全

- operator 读取 `RUNTIME_DEFAULT_RESOURCES` / `RUNTIME_SECURITY_CONTEXT`（chart 从 `runtimeDefaults` 渲染），CR 的 `spec.resources` 优先。需要包含该功能的 operator 版本；旧 operator 忽略这两个 env。
- operator 升级后首次 reconcile 会让**每个员工按 Recreate 重建一次**（Pod 模板变化），请安排窗口。
- deerflow-gov 镜像未声明 `USER`（以 root 运行，2026-10-16 核对 ghcr v16 镜像配置），所以默认不加 `runAsNonRoot`；镜像改为非 root 后再在 `runtimeDefaults.securityContext` 加 `runAsNonRoot: true` 与 `runAsUser`。
- 主命名空间在 `workspaceAgent.enabled=true` 时 PSA enforce 自动为 `privileged`（pw-node-agent 是特权 + hostNetwork + hostPath）。

### 滚动顺序（先 staging）

1. `helm diff upgrade`（或 `helm template` 对比）确认只新增 NetworkPolicy/LimitRange/CronJob、Namespace 标签与镜像 tag 变化。
2. 先以 `networkPolicy.enabled=false` 升级，确认镜像版本与 PSA warn 输出（`kubectl label --dry-run=server` 可预览违规 Pod）。
3. 打开 `networkPolicy.enabled=true`，用 Hubble 观察 `verdict=DROPPED`：`hubble observe -n digital-employees --verdict DROPPED`（另看 aep-system、memory-system）。
4. 按下方连通性清单逐项验证，通过后再上生产。

连通性清单：portal 登录与对话（portal→运行时）；员工记忆读写（运行时→openviking）；知识库检索（运行时→weknora-mcp）；模型调用两条路径（运行时→authorizer→Higress、Anthropic 透传）；对话持久化（运行时→postgres）；新建员工（operator→运行时账号开通、db-init→postgres）；休眠/唤醒（KEDA→postgres）；A2A 委托；IM 通道（channel-gateway）；Prometheus targets 全部 up；从运行时 Pod `curl control-service:8080/internal/gateway/identity` 返回 403（L7 拒绝）、`/aep/v1/...` 正常；从运行时 Pod 访问其他员工 `de-<x>:8001`、`rustfs:9000` 超时。

## PostgreSQL 备份与恢复（backup.postgres.*）

CronJob `pg-backup`（主命名空间）每天 `pg_dumpall`（角色 + AEP + portal + 全部 `de_*` 对话库）→ gzip → 校验文件头 → rclone 上传到集群外 S3 `s3://<bucket>/<prefix>/aep-all-<UTC时间>.sql.gz`（附 `.sha256`），上传成功后删除超过 `retentionDays` 的旧对象；`--immutable` 保证不覆盖历史文件。替代 `k3s/pg-backup.yaml`（同节点 local-path，非异地）；启用后可删除旧 CronJob 与 `pg-backups` PVC。

准备：在主命名空间创建凭据 Secret（只引用，不进 values）：

```bash
kubectl -n digital-employees create secret generic pg-backup-s3 \
  --from-literal=AWS_ACCESS_KEY_ID=... --from-literal=AWS_SECRET_ACCESS_KEY=...
```

然后在 `values-<env>.local.yaml` 设置 `backup.postgres.s3.endpoint/bucket`，并把 rclone 镜像同步进 Harbor（`backup.postgres.uploadImage`）。手动触发一次：`kubectl -n digital-employees create job pg-backup-manual --from=cronjob/pg-backup`。

恢复（在演练环境先做一遍）：

1. 下载并校验：`rclone copy dst:<bucket>/<prefix>/aep-all-<stamp>.sql.gz{,.sha256} .`，`sha256sum -c aep-all-<stamp>.sql.gz.sha256`。
2. 停写（启用休眠的员工先暂停 KEDA：`kubectl -n digital-employees annotate scaledobject --all autoscaling.keda.sh/paused=true`）：`kubectl -n digital-employees scale deploy de-operator de-portal --replicas=0`，各员工运行时 `kubectl -n digital-employees scale deploy -l digitalemployee.zhiyuan.io/name --replicas=0`；`kubectl -n aep-system scale deploy aep-control-service aep-agent-control aep-gateway-reconciler --replicas=0`。
3. 准备空实例：在新 PVC 上起同版本 postgres（17.6），或在原实例上逐个 `DROP DATABASE`（不含 `postgres`/模板库）。`pg_dumpall` 的输出会 `CREATE ROLE`，已存在的角色会报错但不影响继续。
4. 导入：`gunzip -c aep-all-<stamp>.sql.gz | kubectl -n aep-system exec -i deploy/postgres -- psql -U <超级用户> -d postgres -v ON_ERROR_STOP=0`，检查输出里除 “role already exists” 外没有错误。
5. 核对：`\l` 中 `aep`、portal 库与全部 `de_*` 存在；抽查 AEP 用户数、portal 员工记录、某员工最近对话。
6. 按相反顺序恢复副本数，确认 portal 登录、员工对话可读历史。

RPO = 备份周期（默认 24h）；RustFS（Skill 包）与 OpenViking 记忆不在此备份范围内。

## 发版 BOM

`scripts/gen-release-bom.py` 先渲染 chart（或读取已渲染好的 manifest），再列出这一版实际部署的全部镜像。范围包括 Pod 的 containers 和 initContainers，以及 operator、portal 通过 `*_IMAGE` 环境变量交给子 Pod 的镜像。每个镜像记录仓库、tag、digest 和使用方，输出 JSON（`zhiyuan-release-bom/v1`）或 Markdown：

```bash
scripts/gen-release-bom.py -f values-cicd.local.yaml --format markdown -o bom.md
scripts/gen-release-bom.py -f values-cicd.local.yaml --resolve --require-digest -o bom.json
```

- `--resolve`：用 `crane digest` 或 `docker buildx imagetools inspect` 补全缺失的 digest，需要有镜像仓库的读权限。
- `--require-digest`：只要还有镜像没按 digest 固定，就以退出码 1 结束，可以直接用作发版门禁。

这个脚本只读，不会推送镜像、打 tag，也不会部署。

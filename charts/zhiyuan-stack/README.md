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

- ghcr.io 是版本源（CI 按 git tag 出镜像）；集群统一从集群内 Harbor 拉取
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

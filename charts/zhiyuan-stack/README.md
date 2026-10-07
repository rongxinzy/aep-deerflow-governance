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

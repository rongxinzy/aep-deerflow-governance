# zhiyuan-stack

知远数字员工平台的伞形 Helm chart：DE operator + portal + 对话前端 + 管理台 +
OpenViking 记忆栈 + 在线 GLM 的 Higress 模型路由（Anthropic 透传 + openai 转换）。

## 前置（不在本 chart 内）

- AEP 控制面（`Agent-Enterprise-Protocol/deploy/kubernetes/local-k3s/aep-stack.yaml`）
  及种子（`k3s/cicd/seed-aep.sh`）
- Higress 本体（helm 安装）
- 凭据 Secret（chart 只引用名称）：`de-platform-creds`、`de-portal-db`

## 安装 / 升级

```bash
cp values-cicd.example.yaml values-cicd.local.yaml   # 填三个凭据，勿入 git
helm upgrade --install zhiyuan . -f values-cicd.local.yaml \
  -n digital-employees --create-namespace --take-ownership
helm rollback zhiyuan <rev> -n digital-employees      # 一键回退
```

`--take-ownership` 收编已存在的同名资源（迁移既有环境时必需；helm ≥3.17）。

## 版本策略

- ghcr.io 是版本源（CI 按 git tag 出镜像）；集群默认从内网仓 `zhiyuan` 拉取
  （快且稳）。切到 ghcr 拉取：`global.registry: "172.18.5.251:8081/ghcr"`
  （注意按需回源偶发超时，待 CI 同步回内网仓后此路径仅作后备）或直连 ghcr.io。
- 员工运行时镜像 deerflow-gov 是血统镜像：新版本由 `build-deerflow-gov`
  workflow 在上一版之上构建，勿从零重建。

## 已知边界

- operator 配置变更不滚动运行时 pod：改 employees 值后需重启对应运行时。
- OpenViking 模板已用 Recreate 策略（数据目录文件锁，禁止滚动重叠）。
- WeKnora 未纳入本 chart（独立 helm，见 zhiyuan-docs 部署手册）。

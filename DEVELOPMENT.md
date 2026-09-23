# 开发启动环境指南

数字员工平台本地开发栈的完整搭建手册。目标形态：k3s 上跑 AEP 控制面 + Higress 数据
面 + 治理版 DeerFlow 网关，本地辅助运行 mock 平台与验收脚本。

```
飞书/企微(mock) ──ws──┐                        ┌── AEP 控制面 (aep-system)
                      ├─ k3s digital-employees ─┤      ├─ postgres / minio
对话端 adapter ───────┘   deerflow-gateway      │      ├─ control-service :30180
   (assistant-ui)         + 治理中间件          │      ├─ gateway-authorizer
                                                │      ├─ gateway-reconciler ──→ Higress CR
                                                │      └─ Higress (higress-system) ──→ 模型网关
                                                └── GLM 算力网关（直连）
```

## 0. 前置条件

| 依赖 | 说明 |
|---|---|
| WSL2 + systemd | `/etc/wsl.conf` `[boot] systemd=true`（重启 WSL 生效） |
| Docker 29 | daemon 代理 `127.0.0.1:7890`；**绝不 `systemctl restart docker`**（无重启策略的容器会全停），改配置后 `kill -SIGHUP $(pgrep -x dockerd)` |
| k3s | `curl -sfL https://get.k3s.io \| sudo INSTALL_K3S_EXEC="--write-kubeconfig-mode 644 --disable traefik" sh -` |
| helm | `curl -sfL https://get.helm.sh/helm-v3.16.4-linux-amd64.tar.gz` 直连可下 |
| uv / node ≥24 | DeerFlow 后端 / 脚本 |
| 内网仓凭证 | `~/.jfrog/jfrog-cli.conf.v6`（docker login 用 **password** 字段，accessToken 会 BAD_CREDENTIAL） |
| 1ms 专属域名 | 已付费，docker daemon `registry-mirrors` 与 k3s `mirrors.docker.io` 共用 |

## 1. k3s 与镜像源

`/etc/rancher/k3s/registries.yaml`（改完 `sudo systemctl restart k3s`）：

```yaml
mirrors:
  "172.18.5.251:8081":
    endpoint: ["http://172.18.5.251:8081"]   # 明文 http 必须写在 endpoint
  "docker.io":
    endpoint: ["https://<专属域名>.d.1ms.run"]
configs:
  "172.18.5.251:8081":
    auth: {username: ..., password: ...}
```

> `insecure_skip_tls_verify` ≠ 允许明文 HTTP，必须用 `http://` endpoint。

docker 侧同理：`/etc/docker/daemon.json` 加 `registry-mirrors`（1ms）与
`insecure-registries`（内网仓），SIGHUP 热加载。

## 2. Higress（数据面，helm）

```bash
helm repo add higress https://higress.io/helm-charts && helm repo update
helm install higress higress/higress -n higress-system --create-namespace \
  --version 2.2.4 --set global.enableIstioAPI=true \
  --set higress-core.controller.replicaCount=1 --set higress-core.gateway.replicas=1 \
  --set higress-core.controller.resources.requests.cpu=200m \
  --set higress-core.controller.resources.requests.memory=512Mi \
  --set higress-core.gateway.resources.requests.cpu=200m \
  --set higress-core.gateway.resources.requests.memory=512Mi
```

## 3. AEP 控制面（k3s 清单）

清单：`Agent-Enterprise-Protocol/deploy/kubernetes/local-k3s/aep-stack.yaml`
（postgres + minio + control-service + authorizer + reconciler + RBAC；control
NodePort **30180**）。镜像取自内网仓（`zhiyuan/aep-*:0.1.0` 等）。

```bash
sudo k3s kubectl apply -f deploy/kubernetes/local-k3s/aep-stack.yaml
watch sudo k3s kubectl -n aep-system get pods   # 全部 Running 后：
curl http://127.0.0.1:30180/readyz
```

reconciler 通过 k8s API 对 `higress-system` 的 Ingress/WasmPlugin 做
server-side apply——发布 desired state 后真实路由生效（compose dev 里做不到）。

## 4. 治理版 DeerFlow

**镜像构建（离线路径，网络受限也成立）**：构建上下文含
`backend/`（含宿主 `.venv`）、`py-runtime/`（uv standalone python 3.12）、
`aep_deerflow_governance/`、`mcp_dept.py`（纯 Python MCP 工具服务器）、
`extensions_config.json`。关键修正（Dockerfile 内）：

- `.venv/.../site-packages/*.pth` 宿主绝对路径 → `sed` 重写为 `/app/backend`
- `pyvenv.cfg` `home` → `/opt/py/bin`
- `CMD` 显式 `PYTHONPATH=.:/opt/governance`
- **治理包任何改动都必须重建镜像**（否则构造失败，每回合静默
  "(No response from agent)"）

```bash
docker build -t ${REG}/deerflow-gov:vN . && docker push ${REG}/deerflow-gov:vN
# 部署清单占位符替换后：
sudo k3s kubectl apply -f k3s/deployment.yaml
```

**本地裸跑（调试治理包）**：

```bash
cd DeerFlow/backend && uv sync --locked   # pypi 直连（USTC 镜像限流时 --default-index https://pypi.org/simple）
DEER_FLOW_CONFIG_PATH=<cfg> DEER_FLOW_AUTH_DISABLED=1 PYTHONPATH=<治理包目录> \
  uv run --locked uvicorn app.gateway.app:app --host 127.0.0.1 --port 8101
```

## 5. mock 平台与验收

- mockfeishu：`picoclaw/tests/aep-e2e/mockfeishu`（`go build -tags 'goolm,stdjson'`）。
  供集群用时 `-http 0.0.0.0:18086 -ws 0.0.0.0:18087 -advertise <nodeIP>:18087`
  （pod 需通告地址回连）。飞书回复是卡片：POST "thinking..." + PATCH 最终答案，
  断言需 `JSON.parse` 解 unicode 转义。
- AEP fixture：`gov-*` 团队/用户/角色、identity source `gov-feishu`（映射
  `ou_zhang_gov` → zhang）。
- 验收脚本：G1-G4（直连）、P1-P3（IM 桥）、adapter 五用例
  （`chat-runtime-adapter`，`DEERFLOW_URL=http://127.0.0.1:30101 node --test`）。

## 6. 端口一览

| 端口 | 服务 |
|---|---|
| 30101 | k3s NodePort → deerflow-gateway |
| 30180 | k3s NodePort → AEP control-service |
| 8090(svc) / 8091(svc) | authorizer / reconciler（集群内） |
| 18086/18087 | mockfeishu http/ws（宿主） |
| 18080 | AEP compose 栈的宿主转发（socat，供旧脚本） |
| 8101-8107 | 本地调试 DeerFlow 实例（退役中） |

## 7. 坑速查

- `pkill -f` 模式串出现在本命令行 → 自杀（exit 144）：**杀进程按监听端口找
  pid**（`ss -tlnp \| grep :PORT`）；uvicorn 子进程 cmdline 不含 env 前缀。
- `cd X && A & B &` 中 B 不继承 cd。
- Docker 29 containerd 存储产出 OCI index，内网仓拒收缺 manifest 的 index：
  `docker create + export + import --change`（ENTRYPOINT/USER/WORKDIR/ENV 重建）
  拍平后推送。
- Node fetch（undici）300s headersTimeout 掐长阻塞响应——用裸 `http.request`。
- lark SDK 断线重连有 NoneType bug：mock 重启后飞书通道死透，需重启实例。
- AEP 临时密码 ≥12 字符（PASSWORD_POLICY_VIOLATION）。
- k3s 系统 Pod 拉不动 docker.io：registries.yaml 镜像源或 `docker save \| k3s ctr
  images import`；pause 要精确 `3.10.2`。

## 8. 记忆栈（memory-system 命名空间）

数字员工长期记忆 = OpenViking（跨会话记忆/知识/技能）；会话线程状态仍走
DeerFlow 实例自身。部署 `k3s/memory-stack.yaml`（占位符）→ 生成
`memory-stack.local.yaml`（gitignored）→ apply：

- **embed-server**（`zhiyuan/embed-server:0.1.0`）：llama.cpp b11125 +
  bge-small-zh-v1.5-f16 GGUF（HF 镜像 CompendiumLabs/bge-small-zh-v1.5-gguf），
  OpenAI 兼容 `/v1/embeddings`，512 维。模型打进镜像，开箱即用。
- **openviking**（`zhiyuan/openviking:0.4.21`，volces 镜像仓拉取后 export/import
  拍平入库）：ov.conf 里 embedding 指向 embed-server Service，VLM 指向 GLM
  网关（**目标态：两者都走 Higress 模型路由**，AEP 管凭证，用户可自主配模型）。
  不要给容器传 args（entrypoint 自带启动+健康门，传参会 `exec: --host`）。
- 账号开通：root key（local manifest 生成时随机）→
  `POST /api/v1/admin/accounts {"account_id":"zhiyuan","admin_user_id":"deerflow"}`
  → 响应里的 `user_key` 填入 DeerFlow secrets `OPENVIKING_API_KEY`。
- DeerFlow 侧：config.yaml `memory.manager_class: openviking`，
  `owner_user_id: default`（auth-disabled 单用户下的固定搭配；key 换用户时
  manager 会拒绝共享凭证）。**langchain-openviking 0.1.0 的
  `viking://user/memories` 拼写在服务端 ≥0.4 被拒（400）**——DeerFlow 本地
  补丁已改 `viking://~/`，镜像 deerflow-gov:v3 起携带。
- 验证路径：告知事实 → OpenViking 出现 `memories/entities/…` → 删 Pod 重建
  → **全新会话**仍能答出 → `search/find` 应为 200。

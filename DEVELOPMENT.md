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

## 3. AEP 控制面（由 zhiyuan-stack chart 纳管）

自 2026-10-07 起控制面（postgres + rustfs + control-service + agent-control +
authorizer + reconciler + RBAC）由 `charts/zhiyuan-stack` 渲染
（`templates/aep-stack.yaml`；control NodePort **30180**），一次
`helm upgrade --install zhiyuan charts/zhiyuan-stack -f values-<env>.local.yaml`
连同整个平台安装（前置与凭据回填见 chart README）。
`Agent-Enterprise-Protocol/deploy/kubernetes/local-k3s/aep-stack.yaml` 是历史
直接 apply 的存量清单（上游来源），新环境勿再使用。

```bash
helm upgrade --install zhiyuan charts/zhiyuan-stack \
  -n digital-employees --create-namespace --take-ownership \
  -f charts/zhiyuan-stack/values-cicd.local.yaml
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
- **postgres 会话存储驱动必须随镜像走**：基底若走上游 `backend/Dockerfile`
  构建，必须 `--build-arg UV_EXTRAS=postgres`（构建期烘进 venv；运行时
  env 无效）。两个刷新层（`k3s/Dockerfile.deerflow-gov` 与
  `k3s/Dockerfile.deerflow-gov-governance`）各自带 check-first 的 pip
  兜底安装——基底缺驱动时 `import asyncpg` 直接 CrashLoop（压测轮实测）。
  发新 tag 请务必走这两个刷新层之一。

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

- mockfeishu：`tests/e2e-mocks/mockfeishu`（本仓，`go build -tags 'goolm,stdjson'`）。
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
| 30190 | k3s NodePort → de-portal（业务层 + 网页对话代理） |
| 8090(svc) / 8091(svc) | authorizer / reconciler（集群内） |
| 18086/18087 | mockfeishu http/ws（宿主） |
| 18080 | AEP compose 栈的宿主转发（socat，供旧脚本） |
| 8101-8107 | 本地调试 DeerFlow 实例（退役中） |
| 30195（已停用） | k3s NodePort → deerflow-chat-ui（网页对话 UI） |

## 6.5 网页对话与身份对齐（portal 0.3.1+）

**链路**：浏览器持 AEP token → `POST /api/v1/employees/{name}/chat/{rest...}`
→ portal 校验 owner/admin + Ready 相位 → 反代到运行时
`/api/{rest...}`，盖 `X-DeerFlow-Internal-Token`（来自
`de-<name>-memkey` Secret）与 `X-DeerFlow-Owner-User-Id`（调用者 AEP
user id）。浏览器永不接触内部 token / 模型凭证 / 集群网络。

**治理身份链**（middleware `resolve_aep_user`）：IM `channel_user_id`
→ `user_map` → 运行时 user_context id（经 AEP data-scope API 正缓存
校验后才信任）。三级都解析不出即 **fail-closed（返回 `None`，不再回退
`default_aep_user`）**：受管工具按访客拒绝，模型层走访客提示。网页路径靠
第三级：portal 传来的 AEP id 即运行时用户 id。记忆链是**另一条链**
（deer-flow 的 `_resolve_scope`），不随治理的拒绝自动收口；依赖面盘点、
残留路径与跨仓待办见
[docs/identity-fail-closed-inventory.md](docs/identity-fail-closed-inventory.md)。
离线自测：`python3 scripts/test_identity_chain.py`。

**第三方渠道对齐**（企业微信/飞书）：AEP identity mappings
（`/aep/v1/admin/identity-sources/{id}/mappings`，PUT 建、DELETE 撤、
GET 列）。无法自动对齐时管理员手动建映射：externalId = 渠道用户 ID
（企微 userid / 飞书 open_id），localSubjectId = AEP user id。
Admin Console「身份对齐」页提供 UI。

**E2E 快验**：
```bash
TOKEN=$(curl -s -X POST http://127.0.0.1:30180/aep/v1/auth/password/login \
  -H 'Content-Type: application/json' -H 'X-AEP-Protocol-Version: 1.0' \
  -d '{"deploymentId":"demo","sessionId":"t","username":"admin","password":"..."}' \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["accessToken"])')
curl -s -X POST "http://127.0.0.1:30190/api/v1/employees/sales-helper/chat/runs/wait" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"assistant_id":"lead_agent","input":{"messages":[{"role":"user","content":"你好"}]}}'
```
非 owner → 403；employee 非 Ready → 409；Secret 缺失 → 503。

## 6.6 运行时账号自动开通（operator 0.6.0+）

operator 在运行时 Deployment 可用后，自动走 DeerFlow 认证 API 开通每个员工的
运行时账号，凭证落 `de-<name>-runtime-creds` Secret（5 个 key：
RUNTIME_ADMIN_EMAIL/PASSWORD、RUNTIME_OWNER_EMAIL/USER_ID/PAT）：

1. `GET /api/v1/auth/setup-status` 判定认证库状态（404 = 旧镜像 →
   status.runtimeAccount=Unsupported，升级镜像自愈）；
2. 空库 → initialize 实例 admin（`admin@<name>.runtime.internal`，密码
   randomToken 一次成型入库）；已有库 → 用存量密码 login；
3. `POST /api/v1/auth/provision`（admin 会话 + CSRF 双提交，auto_pat）开
   owner 账号 `de-<name>@runtime.internal`，PAT 全 9 个 scope
   （threads/runs/projects）；owner 是纯 PAT 账号（无密码）；
4. 收敛检查：存量 PAT 经 `/api/v1/auth/me`（403=有效、401=失效）探测，
   有效即零写通过。

**home 卷是 EmptyDir**：Pod 重启认证库全丢，operator 用 Secret 里留存的
admin 密码重新 initialize + 换发新 PAT（已实测自愈）。密码被外部轮换 →
Failed（status 里有修复指引）；PAT 被吊销但账号还在 → Failed（删运行时
Pod 重建即恢复）。镜像构建：`docker build --build-arg
GOPROXY=https://goproxy.cn,direct`（容器内拉不动 proxy.golang.org）。
kubectl 快览：`kubectl get de` 的 ACCOUNT 列。

## 6.7 网页对话 UI（DeerFlow 前端复用，chat-ui 0.1.0+）

**复用而非重写**：DeerFlow 自带 Next.js 前端以“平台模式”（fork 补丁，
`NEXT_PUBLIC_PLATFORM_MODE=true`，默认关）跑在 `deerflow-chat-ui`（NodePort
**30195**），对话入口在 portal `GET /chat`（登录 + 员工选择页）。

```
浏览器 ──(de_portal_session/csrf_token/de_employee cookies)──> chat-ui :30195
   └─ 同源 /api/v1/employees/{name}/chat/{rest...}
        └─ Next rewrites（构建期固化 → de-portal svc:8090）
             └─ portal 鉴权（owner/admin + CSRF 双提交）→ 内部 token + owner 头 → 运行时
```

**登录链路**：`POST /api/v1/session`（AEP 密码登录或 token 换发）→ portal 签发
HttpOnly 会话 cookie（内存 store，单副本，滑续 12h）+ 可读 `csrf_token`；选员工
`POST /api/v1/session/employee`（复用 owner/admin 校验）设 `de_employee`（技术名，
API 路径由它派生）与可读伴随 cookie `de_employee_display`（显示名，PathEscape
编码——chat-ui 头部用 decodeURIComponent 解码，空格必须是 %20 而非 `+`；仅在
选择时刻刷新，员工改名后需重新选择才更新）。
cookie 不区分端口但**区分主机名**：30190 与 30195 须以同一主机串访问
（见下「地址零硬编码」——地址已按请求主机名自推导）。

**前端补丁面**（DeerFlow fork，全部走 `isPlatformMode()` 开关）：URL 构造器加
`/api/v1/employees/{name}/chat` 绝对前缀（portal 侧剥冗余 `api/`）；SSR 认证守卫
短路为平台伪用户（`id:"default"` 使偏好同步静默、`system_role:"user"` 避免
admin-only 调用 403）；401 跳回 portal `/chat`（`/login` 会死循环）。

**地址零硬编码（主机名自推导）**：portal 与 chat-ui 同主机、只差端口，所有
跨端跳转地址从请求自身 hostname 推导——用户用什么主机名访问，链接就跟什么
主机名（cookie 主机名一致性天然满足，WSL IP 变化零影响）：
- Console「打开对话」→ `${console主机}:30190/chat`（`VITE_PORTAL_URL` 可覆盖）
- portal 员工选择 → `${请求Host去端口}:30195/workspace`（`PORTAL_CHAT_UI_URL`
  可覆盖；默认不设）
- chat-ui 401 回跳 → `${自身主机}:30190/chat`（`NEXT_PUBLIC_PORTAL_CHAT_URL`
  可覆盖，默认不设）

**镜像构建**（`NEXT_PUBLIC_*` 进客户端 bundle、gateway URL 烧进
`routes-manifest.json`，都是**构建期**，运行时 env 改不了）：

```bash
cd DeerFlow && docker build -f frontend/Dockerfile --target prod \
  --build-arg NPM_REGISTRY=https://registry.npmmirror.com \
  --build-arg NEXT_PUBLIC_PLATFORM_MODE=true \
  --build-arg PLATFORM_GATEWAY_URL=http://de-portal.digital-employees.svc:8090 \
  -t ${REG}/deerflow-chat-ui:0.1.3 .
```

已知限制：browser live-view 面板（WebSocket）可能降级（Next rewrite 不保证
代理 WS upgrade）；`/api/agents*` 面板 403（运行时 agents_api 默认关）；
portal 重启全员登出（会话在内存）。

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

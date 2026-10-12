# 知识接入：本机源码验收与部署边界

本轮优先使用本机进程，不要求 Docker，不连接原 Windows K3s 或共享集群。原机器未提交的六组件 updater 不在远端，不能把它当成交付物。本页描述可复现的构建、配置和验收门；具体运行结果应另记源码 SHA、进程版本和检查结果。

## 六组件源码与作用

| 组件 | 构建/启动来源 | 本机进程验收的边界 |
| --- | --- | --- |
| Admin | AaaS `npm ci`、`npm run build:admin`；以仓库现有 admin server 启动 | 使用真实 Portal URL，浏览器不得获得 WeKnora API key；mock 浏览器检查另行记录 |
| Portal | 本仓 `portal/`：`go build -o <private-run-dir>/de-portal .` | loopback 监听、独立 PostgreSQL、真实 managed handler；AEP 与 Kubernetes 身份/Secret 替身必须标注 |
| WeKnora app | 固定 WeKnora commit 的 Go 服务及其 SQL migrations | 独立 PostgreSQL/Redis、合成资料、真实模型引用和解析依赖；无 DocReader 时 PDF 不算验收 |
| Python MCP | 固定 WeKnora commit 的 Python MCP 入口与锁定依赖 | 指向真实 Portal 授权 callback；没有 handle、撤权、错误审计均必须拒绝/可追溯 |
| governed runtime | 固定 DeerFlow backend commit + 本仓 `aep_deerflow_governance` | `uv sync --locked` 后 loopback uvicorn；模型调用、治理包注入与 MCP 权限交集需要真实证明 |
| operator | 本仓 `operator/`：`go test ./...`、`go build ./...` | 无 Kubernetes 时只能验证控制器代码，不宣称 CR→Deployment reconcile、重启恢复已实测 |

不要把现有 `k3s/Dockerfile.deerflow-gov*` 的内网 runtime 基镜像替换成普通 Python 镜像后称为兼容构建。它们依赖 `/app/backend/.venv`、`/opt/py` 等既有运行层；源码裸跑用 DeerFlow 锁文件建立独立环境，按 `DEVELOPMENT.md` 的裸跑入口加载治理包。禁止为了方便验收在可外部访问服务上设置 `DEER_FLOW_AUTH_DISABLED=1`。

## 配置核对

所有进程只绑定 loopback，临时目录权限 `0700`，配置与 dump 权限 `0600`；令牌通过进程环境或受控配置加载，不写入命令行、报告、仓库和原始响应日志。每个组件记录 clean commit SHA、运行工具版本、依赖锁摘要和自身监听端口，不能沿用原机器的缓存标签。

Portal 必须固定 `PORTAL_AEP_DEPLOYMENT_ID`、`PORTAL_KNOWLEDGE_TENANT_ID`、`PORTAL_KNOWLEDGE_EMBEDDING_MODEL_ID`、`PORTAL_KNOWLEDGE_API_URL`、`PORTAL_KNOWLEDGE_MCP_URL` 与独立 `PORTAL_DB_URL`。API key **不是** `PORTAL_KNOWLEDGE_API_KEY` 环境变量：实际从 `PORTAL_CREDENTIALS_SECRET` 指定的 Kubernetes Secret 中读取 `PORTAL_KNOWLEDGE_API_KEY_SECRET_FIELD` 字段。本机无集群时如使用 Secret client 替身，必须明确这是测试身份/Secret存储替身，不能据此宣称真实 Kubernetes 配置验收完成。

MCP 的 `PORTAL_KNOWLEDGE_AUTH_REQUIRED=true` 和 Portal callback URL 必須匹配，`WEKNORA_API_KEY` 与 `MCP_SERVER_AUTH_TOKEN` 仅在服务端。服务 bearer 不等于用户权限：每次读取仍须员工绑定、请求者 user/team grants、企业/tenant 的交集授权和完成审计。

## 两库备份与恢复

WeKnora 的原生 SQLite 验收需用 `CGO_ENABLED=1 go build -tags sqlite_fts5`，使用隔离数据库、`RETRIEVE_DRIVER=sqlite` 和真实本机嵌入模型。SQLite API/检索证据不代替生产 PostgreSQL/ParadeDB 兼容性。envtest 可启动真实 apiserver/etcd 并验证 operator 写入 API 对象，但没有 scheduler/kubelet，不能据此宣称 Pod 或 runtime 正常运行。

只操作本轮隔离数据库。使用相应 PostgreSQL 版本的 `pg_dump --format=custom` 生成私有备份，再 `pg_restore --exit-on-error` 到新的 `knowledge_acceptance_*` 数据库；不要恢复覆盖源库。记录备份 SHA-256、恢复库名、migration 版本、表/约束检查与合成数据校验结果，不记录 DSN 和 token。备份成功不等于恢复成功，镜像回滚也不会撤销 SQL DDL。

## 真实闭环与结果分级

1. 两个独立知远用户、两个隔离知识库；以真实 managed API 建文档/FAQ 库。
2. 合成 TXT/URL/手动文本导入并观察真实解析终态；PDF 独立依赖 DocReader。仅上传 accepted 不算 parsed。
3. 标签、文件夹、FAQ、chunk 修改后真实检索命中；禁用/删除后来源消失，原文件/图片无权读取被拒绝。
4. 浏览器建库→导入→检索→来源→审计，无 WeKnora 登录/跳转；失败、空态和权限提示可操作。
5. 员工 runtime 经 MCP 获取授权资料；两用户权限不同、跨库拒绝、撤权/解绑后旧会话立即拒绝，完成审计记录真实主体。
6. 重启 Portal/MCP/WeKnora 后持久状态恢复；上游断连、凭据失效、解析失败和删除不确定状态保持 failclosed。

结果逐项标为真实进程通过、HTTP替身通过、未运行或阻塞。真实 WeKnora + Portal 若 AEP/Kubernetes 身份为替身，是引擎/适配 API 集成证据；不能冒充知远完整登录、operator部署或生产交付。

## 可选未来镜像事务工具（本轮没有运行部署）

`update_knowledge_stack.py` 是新建的隔离 kind 路径，**不替代**原 Windows 固定 K3s updater，也不是本机进程验收的前置条件。既有 `update_local_portal.py` 原样保留。

工具默认 `plan`；`prepare` 仅核验已构建的全部六镜像：固定 digest、本机平台、OCI revision 标签与清洁源码 SHA。它不构建镜像，标签核验也不等于可验证供应链签名。仅接受显式 `kind-zhiyuan-knowledge-*` context、loopback HTTPS endpoint 和唯一预期 node，不读取 current-context，不获取 Secret 值。

输入 JSON 的 `components` 必须恰好为 `weknora/mcp/operator/runtime/portal/admin`。每项含 `source: {path, sha}`、immutable `image`、非空 `targets`（`kind/namespace/name/container`），runtime 可指向 `digitalemployee`，并给出 reconcile 的 `deployment` 和实际 runtime `container`。其他组件只更新 Deployment。Portal/MCP `requiredEnv` 必须包含脚本 `REQUIRED_ENV` 的全部字段；Secret 字段仅允许 `{secret:{name,key}}`，其余用 `{value:...}`。Portal runtime 默认值须与目标 runtime digest相同。工具仅检查配置已匹配，不修改 env/Secret；配置变化应先在可审查 chart 中完成。

`cluster` 含 `kubeconfig/context/node/architecture`；`databases` 含 `portal/weknora`，每项为 `{artifact,sha256,restoreReceipt}`。receipt 含 `backupSHA256/isolated:true/checksPassed:true/database:knowledge_acceptance_*`；它是操作者提供的真实恢复记录，不是工具自己执行恢复的证明。计划及 journal 不保存数据库内容或原始 Kubernetes 对象。

`apply` 必须使用原 manifest/plan 和新的 journal 路径。写前重验源码、镜像、备份与 live state；先存私有 journal，再用 UID/resourceVersion/image 的 JSON test 原子补丁更新镜像并写 run marker。CR controller 修改 resourceVersion 时重新读当前版本后打受控补丁。失败连同可能丢失成功响应的那个组件一起逆序恢复；UID、镜像或 marker 漂移即停止对应恢复并记录 incomplete，不盲目覆盖他人变更。rollout 后检查 Pod→ReplicaSet→Deployment 所有权链、Ready 与 imageID digest。

工具不创建集群、不迁移数据库、不导入镜像、不更改拉取策略、不升级 CRD，也没有对当前源码进行真实部署验证。未来运行前需独立审查实际 manifest、镜像可拉取性与剩余部署条件。

```sh
python3 -m unittest scripts/test_update_knowledge_stack.py scripts/test_update_local_portal.py
```

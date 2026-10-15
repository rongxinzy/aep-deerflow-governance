# 同事搭档协作：直接聊天、传话、原对话回复

本批实现组织内的搭档发现、独立接待、传话以及本人回复回到原会话。普通聊天不创建任务，不把“搭档已接收”写成本人已读或工作已完成。

## 合同与归属

- 新建搭档默认组织内可联系。未配置联系范围的存量搭档保持仅自己，主人可在搭档详情调整范围及对外介绍。
- 联系范围写入 PersonalWorkspace 的 `digitalemployee.zhiyuan.io/companion-contacts` annotation；联系范围不新增 CRD 字段。
- `GET /api/v1/colleague-companions` 返回组织内可联系的搭档及自己的搭档；核对 AEP 中的活动人员身份，隐藏停用人员。
- `PUT /api/v1/colleague-companions/{workspace}/{companion}` 仅主人可调整。
- `POST /api/v1/collaborations` 保存消息和稳定 requestId；`GET /api/v1/collaborations[/{id}]` 仅发起人与搭档主人可读。
- `/reply` 仅接收方主人可回答待本人处理的消息；`/retry` 仅发起人可重试失败的投递。
- 搭档工具通过 `/api/v1/collaboration-agent/{directory,send,read}` 调用同一条业务链路，使用工作区凭据，不能自行指定另一位用户身份。

## 持久化与回程

PostgreSQL 保存协作、来信、搭档回复、待本人处理的问题、本人回复及回程状态。后台 worker 用可过期领取记录防止并发重复处理；Portal 重启后继续领取。失败投递保留在会话中，可明确重试；回程失败保留真实回复并等待原搭档恢复。

传话保存完整来源 `(workspace, companion, conversationId)`，同事搭档的普通聊天与传话来源使用不同协作会话。接待 runtime 以消息 ID 去重，并持久化首次接收的上下文和结果。向原会话写入有稳定 ID 的被动回执，不自动运行主人的模型或工具。用户需要继续处理回复时，自己的搭档可显式读取协作结果；没有协作编号时先按当前来源会话查找传话记录。

接待 runtime 使用独立 SQLite harness，只提供 `request_owner_reply`。它可以聊天、处理来信提供的资料；需要主人的工作信息、决定或承诺时交给主人。不会加载主人私有记忆、邮件、文件、浏览器或企业连接器。对外协作及本人回复对双方可见，私有源会话内容不对接收方开放。

## 跨仓配合

- pi-durable-bench：`companion/collaboration.mjs`、gateway 读取 roster 的 workspace/portalURL。
- 本仓：Portal broker、持久化、worker；Operator 生成 portalURL。
- zhiyuanBotUI：同事详情直接聊天／传话、消息页协作列表、本人待回复入口、原对话回执及返回链接。

仅提交代码，不构建或发布生产镜像、不操作集群。运行能力需要未来统一发布 Portal、runtime、Operator 和 UI 后才会生效；本批不实施发布。

## 验证

- Portal：`go test ./...`；发现范围、所有权、幂等、代答、本人回复、回程失败和重试。
- Operator：`go test ./...`，保持现有工作区模板与清单测试。
- 真正 PostgreSQL：CI 的临时 postgres:16 服务运行 `TestCollaborationPostgresRelayRoundTrip`，验证消息去重、4 worker 并发领取、换连接池后继续回复、同一回执重复投递以及第三方读取被拒绝。没有配置一次性 PostgreSQL 时本地明确 skip。
- Runtime：实际 harness + 临时 SQLite + 合成模型，验证仅接待工具可用、本人私有回执不进入接待模型、重启去重、被动回执不触发模型、自然语言查找／发送／读取回复。
- UI：`verify/collaboration.mjs` 使用合成双用户 API，实际 Chromium 验证同事详情→直接聊天、原会话→传话、接收人回复→发起人原对话、刷新恢复、浅色桌面与深色移动端。该测试证明界面与 API 合同，不代表真实生产双用户联调。

## Persona 主线合同修复

Portal 已提供的 Persona 字段补齐到 CompanionSpec、生成的三处 CRD 和 companions.json。字段可选、最多 2000 个 Unicode 字符；旧对象不填保持原行为。配置变更沿现有清单 checksum 更新网关，清空后省略该字段。CRD 文件只提交代码，没有 apply 到集群。

Operator 回归验证 Persona 持久化、清单传递、配置变更与清空；Portal 保留 Unicode 长度、创建与摘要回显测试。覆盖率比较固定 head SHA，并选取主线上最近成功 push CI 的祖先作为可编译基线；找不到基线即失败，当前提交全量测试及 80% 门槛仍强制执行。这样已损坏的主线不会阻塞自身修复。

## 前后端隔离联调（2026-10-15）

联调使用真实 BotUI 浏览器页面与 BFF、Portal HTTP 路由与 cookie/CSRF 校验、PostgreSQL 16、两个工作区 gateway、真实 pi-durable harness 和独立 SQLite。AEP 人员目录、Kubernetes client 和模型输出为测试替身；不连接现有集群、不调用真实模型、不部署。测试用会话由 fixture 铸造，未覆盖真实 AEP 登录。

本仓 `portal/collaboration_integration_test.go` 是仅测试可用的显式 opt-in fixture，未配置时跳过，不增加生产路由或开关。只允许回环地址 PostgreSQL，每次使用独立 schema，退出删除；四个 worker 运行真实投递逻辑。配套 `zhiyuanBotUI/verify/collaboration-integration.mjs` 启动临时工作区和 BFF，通过两个独立浏览器用户上下文操作，不拦截业务 API 返回值。

复跑（先准备本地一次性 PostgreSQL、三仓依赖与 Chrome；所有路径可自行指定）：

```bash
# 本仓 portal 目录
 go test -c -o /tmp/collaboration-portal.test
# zhiyuanBotUI 目录
 npm --prefix web run build
 COLLABORATION_RUNTIME_DIR=/absolute/path/pi-durable-bench/companion \
 COLLABORATION_PORTAL_BINARY=/tmp/collaboration-portal.test \
 PORTAL_TEST_POSTGRES_URL='postgres://testuser@127.0.0.1:19432/postgres?sslmode=disable' \
 node verify/collaboration-integration.mjs
```

`CHROME_PATH` 可指定浏览器可执行文件；`COLLABORATION_QA_OUT` 可指定输出父目录，默认系统临时目录。输出包含截图、进程日志和 result.json；含测试会话数据，不应发布或提交输出目录。脚本清理自身子进程，PostgreSQL 由启动者负责关闭。

实际通过：同事详情直接聊天、按钮传话、自然语言查找/传话工具、主人待回复入口、被动回执实时返回原对话、返回链接与刷新恢复、断线期间回执恢复、重复重连无重复回执、迟到 REST 不覆盖 SSE、接待模型与主人私有会话隔离、浅色桌面和深色移动端。

联调发现并修复的前端断点：原页面忽略 SSE 的 snapshot；连接断开期间回复已持久化，重连却不补齐。前端现在从原子快照恢复会话和运行状态，同时阻止更早发起的 REST 请求覆盖快照。传输故障由测试网络转发层主动切断 SSE，业务请求仍由真实服务处理。此结果是隔离验收，不代表生产集群、真实 AEP、真实模型端到端验收。

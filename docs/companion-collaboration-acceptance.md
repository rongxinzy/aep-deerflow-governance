# 同事搭档协作：直接聊天、传话、原对话回复

本批实现组织内的搭档发现、独立接待、传话以及本人回复回到原会话。普通聊天不创建任务，不把“搭档已接收”写成本人已读或工作已完成。

## 合同与归属

- 新建搭档默认组织内可联系。未配置联系范围的存量搭档保持仅自己，主人可在搭档详情调整范围及对外介绍。
- 联系范围写入 PersonalWorkspace 的 `digitalemployee.zhiyuan.io/companion-contacts` annotation，不升级 CRD。
- `GET /api/v1/colleague-companions` 返回组织内可联系的搭档及自己的搭档；核对 AEP 中的活动人员身份，隐藏停用人员。
- `PUT /api/v1/colleague-companions/{workspace}/{companion}` 仅主人可调整。
- `POST /api/v1/collaborations` 保存消息和稳定 requestId；`GET /api/v1/collaborations[/{id}]` 仅发起人与搭档主人可读。
- `/reply` 仅接收方主人可回答待本人处理的消息；`/retry` 仅发起人可重试失败的投递。
- 搭档工具通过 `/api/v1/collaboration-agent/{directory,send,read}` 调用同一条业务链路，使用工作区凭据，不能自行指定另一位用户身份。

## 持久化与回程

PostgreSQL 保存协作、来信、搭档回复、待本人处理的问题、本人回复及回程状态。后台 worker 用可过期领取记录防止并发重复处理；Portal 重启后继续领取。失败投递保留在会话中，可明确重试；回程失败保留真实回复并等待原搭档恢复。

传话保存完整来源 `(workspace, companion, conversationId)`，同事搭档的普通聊天与传话来源使用不同协作会话。接待 runtime 以消息 ID 去重，并持久化首次接收的上下文和结果。向原会话写入有稳定 ID 的被动回执，不自动运行主人的模型或工具。用户需要继续处理回复时，自己的搭档可显式读取协作结果。

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

# 空间成果运行时契约

`shared_space_runtime.go` 增加 workspace UID/owner/companion/source conversation 绑定、执行 generation 和 durable outbox。成果、审阅和 Paperclip command 仍复用 `shared_spaces.go`，不复制业务账本。启动时数据库 migration 创建新元数据表；应用进程启动 outbox worker。

既有 `/api/v1/collaboration-agent/{action}` 的工作区 token 鉴权覆盖新增 `space-discover`、`space-bind`、`space-status`、`space-submit`、`space-receipt`。owner 与 deployment 由服务端派生，不接受任意 userId 或 originURL。bind 验证可信 Pi 会话 ID，空间任务与 source 绑定不可变；当前不支持重新绑定已有成果任务。

bind 先提交绑定与 start outbox，精确 Pi receipt 后填入 piTaskId。submit 验证 Pi completed/selected artifact/meta 与当前执行，复用 sharedSubmitInput，submit 与 review 成功事务同时创建回送意图。request_changes 同事务推进 generation 并创建含 result/revision/reason 的 revise。回送为原对话被动记录，包含待审阅/退回/接受阶段；历史成果回执可在新代次展示。

每次后台发送都在空间锁内重查 workspace UID/owner、账号 active、搭档存在、当前空间 ACL、PC project/company 和任务 assignee 快照。start/revise 额外检查终态与未核验 Paperclip commands；submit 锁内再次确认执行 fence。发送前 outbox 标记 unknown，只有精确 commandId/payloadHash/applied receipt 才推进；unknown 仅查询 receipt，absence 不重放。公平 retry 时间避免单一 unknown 阻塞其他任务。

需要既有 Paperclip/AEP/工作区服务配置；不新增浏览器 origin 或生产凭据。未提供空间成员可读取私有 source 的 API。旧 PoC 状态不迁移；业务验收、镜像构建和部署需单独执行。

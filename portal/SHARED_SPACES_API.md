# 协作空间读写入口

Portal 使用现有 AEP 登录/CSRF，所有共享入口额外实时验证 AEP token、部署身份和空间成员资格。管理员没有隐式业务访问权；只有项目绑定入口要求平台管理员。以下接口仅在 `PORTAL_PAPERCLIP_BASE_URL`（包含 `/api`）、`PORTAL_PAPERCLIP_TOKEN`、`PORTAL_AEP_DEPLOYMENT_ID` 和 Portal PostgreSQL 配置后开放。Paperclip 凭据只存在服务端。 `PORTAL_PAPERCLIP_TOKEN` 必须是专用 board 用户的 API key，不是 Agent key 或 task-bridge key。该 key 继承用户可访问的所有 company，创建时的 requestedCompanyId 不构成 token 的公司范围限制；因此应使用仅获目标 company 权限的专用服务用户。隔离验收产生的临时 key 在验收结束后撤销。

## 首期接口

全部位于 `/api/v1/shared-spaces`：

| 方法与相对路径 | 请求/返回 |
| --- | --- |
| GET 空路径 | `{spaces:[{id,companyId,projectId,name,role}]}`，仅当前成员空间 |
| POST 空路径 | 管理员绑定已有项目：`{companyId,projectId,ownerId,requestId}`，返回 `{space}`；不创建第二份项目 |
| GET `/{space}` | `{space,members:[{userId,role,version}],capabilities}` |
| PUT `/{space}/members/{user}` | `{role,expectedVersion,requestId}`，返回 `{member}`；角色 owner/editor/viewer/revoked |
| GET `/{space}/tasks?q=&limit=30&offset=0` | `{tasks,hasMore,nextOffset}`；上游强制 projectId 过滤，返回受限 DTO，可含 `reviewState` |
| GET `/{space}/tasks/{task}` | `{task,results,reviews,commands,capabilities}` |
| GET `/{space}/tasks/{task}/results/{result}` | `{result,body}`，Paperclip 历史 revision 精确读取 |
| POST `/{space}/tasks/{task}/results` | `{requestId,expectedVersion,title,body}`，返回 `{result}` |
| POST `/{space}/tasks/{task}/results/{result}/review` | `{requestId,expectedVersion,decision,reason}`，decision 为 request_changes 或 accept；退回必须原因 |

`capabilities` 包含 `manageMembers/submit/review`；任务详情按最新版本与当前用户细化。`result` 包含 `id/taskId/version/title/documentKey/documentId/revisionId/submittedBy/state/createdAt`。`state` 为 submitted/changes_requested/accepted。`review` 包含 `resultId/decision/reason/actorId/createdAt`。`command` 只暴露 requestId/state/action，不返回核验意图或授权凭据。

成员版本从 1 起，新增成员的 expectedVersion=0。成果 expectedVersion 是任务当前最新提交序号，初次=0；提交后+1。审阅必须指定最新已提交版本。owner/editor 可提交，owner 可审阅但不可审阅自己的成果；owner 可增设另一位 owner，不能移除/降级最后一位 owner。viewer 只读。审阅后保留版本和决定，新修订只在退回后提交。

成功初次绑定/提交返回 201，其余返回 200。400 无效参数；401 当前身份无法确认；403 无操作权限；404 空间/任务/成果不可见；409 冲突；423 上次写入未确认；502 上游读取或操作未确认（已发送的成果写命令转为 423）；503 未配置或依赖不可用。错误形状 `{error,code?}`：stale_version、idempotency_conflict、command_unknown、conflict、request_rejected。调用者遇到未知结果必须保留原请求编号和负载、刷新状态，不创建新编号重试。

## 数据与一致性边界

- Paperclip 是项目、任务状态、文档正文和原生修订的唯一来源。Portal PostgreSQL 保存部署限定的空间映射、成员 ACL、不可变提交 revision 引用、审阅决定和命令账本，不复制可编辑业务任务或文档正文。
- 所有任务路径先读取真实 companyId/projectId，不能由请求体声明授权范围。列表的 projectId 由空间映射固定，不接受任意上游透传。任务响应仅投影 id/identifier/title/description/status/priority/assigneeAgentId/assigneeUserId/createdAt/updatedAt；不暴露 execution 配置或私有上下文。
- 首期只有有限的显式读写接口，没有直接 Paperclip 凭据、任意路径代理、通用搜索、事件流或知识检索入口。原有个人工作区/私聊权限不因空间成员资格扩大。
- 每个空间使用 PostgreSQL 会话 advisory lock 串行化成员变更和成果写命令。写入期间撤销请求等待锁，已开始的授权操作先完成；撤销后新请求、重放都重新检查 ACL。读请求授权点为开始时的成员检查，不撤回已发送响应。
- 命令发送前独立提交 `sending`，保存负载 hash 和预期核验信息。网络超时、无法验证响应、上游非成功等置 `unknown`；崩溃或本地提交失败保留 `sending`。两者都阻塞同任务后续写入，不自动重放。包括确定性上游 4xx，目前也保守阻塞，需运维独立核验；尚无自动解除入口。
- 成果写入 Paperclip `shared-space-result` 文档。修订必须携带前一已提交 `baseRevisionId`，读取历史按 documentId/revisionId/companyId/issueId/body hash 核验。审阅先核验当前最新正文与引用一致。
- 退回时，有人类或 Agent 配属写 Paperclip `in_progress`；无配属写 `todo`（Paperclip 不允许无配属任务进入进行中），同时成果状态为 changes_requested。不为退回而伪造配属。接受写 `done`。只有上游确认成功后才原子保存审阅决定与 applied 回执。Portal 不将执行完成自动等同于接受。终止任务 done/cancelled 拒绝新成果或审阅，已应用同请求可返回原回执。
- 不同进程共用同一 DB，幂等由数据库约束和空间锁保护。相同编号、actor、action、负载返回原回执；变负载冲突。绑定有独立部署级幂等账本。
- 外部绕过 Portal 的 Paperclip 管理员/执行器并发修改不能靠 Portal 锁消除。上线前仍需收敛写凭据与网络入口；状态与文档读前校验不等于跨服务原子事务。未知结果恢复必须依据服务器账本、固定文档/修订与实际回执另行核验，不能重置账本后盲重放。

本模块不部署任何环境，不自动把历史个人会话内容暴露给空间。绑定现有项目没有可信原对话映射，暂不返回 origin；后续任务创建/委派适配器应写入可信来源绑定，而不能接收任意 href。

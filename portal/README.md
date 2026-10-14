# de-portal — 数字员工业务层

operator 之外唯一的 DigitalEmployee CR 写入口。终端用户持 AEP token 调用，
永不接触 Kubernetes。operator 持有 CR 全权（不可配置）；"谁能申请数字员工"
作为策略函数留在这层（这正是两者的分工边界）。

## API（Bearer = AEP 访问令牌）

| 方法 | 路径 | 语义 |
|---|---|---|
| POST | /api/v1/employees | 申请（策略：approval→202 pending / self-service→201 / admin-only→非管理员403；仅人类可申请） |
| GET | /api/v1/employees | 列表（管理员见全部，用户见自己） |
| GET | /api/v1/employees/{name} | 详情（owner 或管理员） |
| DELETE | /api/v1/employees/{name} | 注销（owner 或管理员） |
| GET | /api/v1/requests?state=pending | 审批队列（管理员全部，用户自己的） |
| POST | /api/v1/requests/{id}/approve\|reject | 审批（管理员） |

## 策略与配额（evaluatePolicy）

- 模式：`approval`（默认）/ `self-service` / `admin-only`（PORTAL_POLICY_MODE）
- 配额：已部署 CR + pending 请求合并计数（PORTAL_QUOTA_PER_USER）
- 管理员判定：PORTAL_ADMIN_USERS（AEP 用户名/id）或 PORTAL_ADMIN_ROLES（AEP 角色）
- agent 身份（kind=agent）不可申请——委托走 A2A，不走业务层

## 关键设计

- **owner 落 CR spec**（spec.owner = AEP user id）：配额、列表隔离、删除权限都
  以它为准；governance.defaultAEPUser 同步设为 owner——员工默认以属主的
  数据范围行事。
- **内置身份快照**：创建/更新时用 AEP `/aep/v1/auth/methods` 核对部署实例 ID 和名称，
  部门与归属人名称取已验证的 AEP 目录记录；这些名称供对话中自我介绍，
  不参与授权。实例 ID 不一致时拒绝发布。目录改名后重新提交员工的 `team`
  或 `owner` 字段刷新相应快照。
- **审批时重查**：待审批申请落地前重新核对 AEP 实例、部门和负责人，避免
  使用申请时已过期的显示名称或已停用的负责人。
- **审批状态机**存 Postgres（deportal 库，requests 表）；spec_json 保存申请时
  渲染的完整 CR，审批通过原样落 CR。
- **教训**：operator 的 typed 客户端读 CR 时丢弃其结构体没有的字段，任何
  `r.Update(整对象)`（如加 finalizer）都会把未知字段擦掉——CRD 加字段后
  **operator 必须同版本升级**（0.3.0 起 spec.owner 安全）。

## 个人工作现场代理

`/api/v1/workplace` 与 `/api/v1/workplace/{rest...}` 由登录身份解析本人的
PersonalWorkspace，映射到该工作区 gateway 的 `/api/workplace` 同名路径。
与群聊相同，管理员也不能通过此个人入口读取他人的工作现场；工作区必须为
`Running`。请求经过现有 session/CSRF 中间件，浏览器 Cookie、Authorization、
CSRF 和自行传入的内部 token 不会送到 gateway，owner header 由 portal 重写。

当前 runtime 契约包括：

- `GET /api/v1/workplace`：实际可用的共享环境和能力；没有桌面服务时返回不可用。
- `GET /api/v1/workplace/files?path=...`：共享文件目录。
- `GET /api/v1/workplace/files/content?path=...&download=1`：实际文件内容下载。

桌面/浏览器的截图与输入同样经此 HTTP 代理。Portal 将已认证凭据派生为
不透明的 `X-Workspace-Session-Id`，覆盖浏览器自带值；原始凭据不会传给
runtime。浏览器的 `X-Workspace-Agent-Token` 被移除，不能冒充本机模型工具。
接管租约的获取、续期、释放和输入由 runtime 按 owner、session 与随机 leaseId
校验；session 绑定不扩大工作区访问范围。

代理保留查询参数、响应 Content-Type/Content-Disposition 和流式响应。
每搭档的 `/api/tasks`、`/api/artifacts`（含内容下载）继续通过现有
`/api/v1/workspaces/{workspace}/{companion}/chat/api/...`，不共享搭档私有数据库。
此代理需要配套 pi-durable-bench gateway 的 `/api/workplace` 实现；旧 runtime
没有该接口时按上游失败返回，不生成模拟云电脑、文件或任务。

工作区列表/详情增加 `runtime` 和 `resources`，来源分别是 CR spec 的运行底座
和配置资源额度，并非实时利用率。未填写 runtime 时按 CRD 默认 `pod` 展示；
新建工作区仍使用既有 pod 默认，未改变集群配置或自动迁移到 Firecracker。

本地验证无需集群：`cd portal && go test ./...`。工作现场测试使用真实 HTTP
代理、假 AEP 登录及内存 Kubernetes client，覆盖 owner 范围、Running 门槛、
CSRF、文件下载头、请求头清理和流式内容；不代表已完成真实 FC 部署验收。

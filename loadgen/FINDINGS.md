# 规模化压测发现清单（2026-10-04，cicd 栈）

环境：AEP control-service 0.6.0（单副本无资源配额）· postgres 17.6（shared_buffers=16MB 默认）
· portal/0.4.x（单副本，DB 池 4）· de-operator 0.9.x · 种子：303 用户/41 部门/13 角色/
40 数据范围规则/23 模型分配 + 30 个新 DE。

## A. 结构性 bug（已定位根因）

### A1【严重】共享对话库导致 DE 认证体系互相顶掉
- 现象：5 个 DE 中 4 个 `runtimeAccount=Failed` 达 5 天，401 死循环；
  删 pod/PVC 均无效。
- 根因链：`conversationStore.backend=postgres` 时所有 runtime 拿同一条
  `CONVERSATION_DB_URL` → 同一个 `de_conversations` 库 → DeerFlow 的
  `users` 表（runtime 认证账号体系）全平台共享。第一个完成 setup 的
  （sales-helper，10-01 06:38）独占全局 admin；其余 DE 的 operator 各持
  独立凭据登录 → 用户不存在 → 401；`needs_setup` 由共享表决定永远为
  false → 结构性无法自愈。
- 次生放大：operator 每 5min 失败登录 → 触发 runtime IP 登录限速
  （slowapi，5 次/锁 300s）→ operator 无 429 退避 → 自锁死循环。
- 附带风险：各 DE 的 runs/checkpoints 混在同一组表，跨员工数据隔离
  依赖调用方自觉。
- **修复（已写好待部署）**：operator 默认 per-DE 数据库
  （`de_<name>`，CRD `conversationStore.database`，"shared" 保留旧路径）；
  账号探测改 PAT-first（稳态零登录）+ 429 退避 10min。见
  `operator/internal/controller/conversation_db.go`、`runtime_account.go`。
- 临时解围（已执行）：4 个坏 DE 切 sqlite 重启后自愈，5/5 Ready。

### A2【中】portal 部门成员 >200 静默截断
- `GET /api/v1/departments/{id}/members` 单页 `limit=200` 拉一次 AEP
  用户表就客户端过滤——300 用户后 Console 部门成员数据**不完整且无提示**。
  `resolveRuntimeModels` 的 `?limit=500` 同样被 AEP 静默压回 50
  （`limit>200 → 50`，不是 clamp 到 200）——模型目录 >50 时创建员工
  会报"未知模型"。
- 修复方向：portal 跟随 nextCursor 翻页；AEP 把超限 limit clamp 到 200。

### A3【低】PATCH /admin/users 静默忽略 teamIds/roleIds
- 请求体接受但只转发 displayName/email/status（admin_users.go:188）——
  调用方以为改了成员关系，实际没有。应 400 拒绝或真正支持。

### A4【中】部门创建是 portal 独占编排，operator 硬依赖其副作用
- portal POST /departments 是唯一的"AEP 团队 + OV 账号(tenant-<teamId>) +
  WeKnora 租户"三合一入口。用 AEP admin API 直接建团队（合法路径）后，
  该团队下建 DE 会 404：operator `register memory user` 直接把 OV 原始
  报错写进 CR message（`Account not found: tenant-load-team-r1f1`），
  且 CR 停 Error 直到指数退避重试（实测补建账号后 annotate 踢一脚，
  27 个 CR 60 秒内全部转 Ready）。
- 修复方向：operator 对缺失 OV 账号自建（有 root key），或部门三合一
  编排下沉为 AEP 侧原子操作。本轮解围：30 个团队 OV 账号已补齐。

## B. 性能量化（数字为准）

### B1 登录是唯一硬瓶颈（Argon2id 64MiB/t=3）
| 并发 | p50 | p95 | max | 服务端均值 |
|-----|-----|-----|-----|----------|
| 10  | 392ms | 652ms | 693ms | — |
| 30  | 830ms | 1.75s | 1.79s | — |
| 50  | 1.40s | 2.56s | 2.89s | 980ms |

- 单副本登录吞吐上限 ≈2.5/s；300 用户批量导入 24.9s（12.1/s，服务端
  串行 Argon2）。内存尖峰：c50 时 ~3.2GiB 纯 Argon2 分配（瞬时，无泄漏）。
- 读路径完全无压力：/user/me p95=13ms、models 10ms、teams 10ms
  （服务端 2-4ms）。**5 分钟 169.4 rps 混合流零错误**，p95≤15ms。
- 容量参考：万人组织早高峰（10 分钟 5000 登录 ≈8.3/s）需 ≥4 副本，
  或 Argon2 降到 32MiB/t=2（内网场景足够），或登录排队+验证码削峰。

### B2 遥测批写 p50=571ms/百条
- `POST /user/events/batch` 逐条 INSERT（不在显式事务，每条自动提交），
  180 批×100 条服务端均值 587ms。30 并发下吞吐仅 ~1.7k 行/s。
- 修复方向：单事务 multi-VALUES（百倍空间，一批 <10ms 可期）。

### B3 全局控制事件创建 p50=135-155ms
- 扇出 = 活跃会话全表 DISTINCT + 逐会话 INSERT 循环。383 会话时 135ms；
  会话数线性外推：1000 会话 ≈350ms。skill-assignment 变更同路径。
- 修复方向：`INSERT INTO ... SELECT` 一条语句完成扇出；
  `control_events` 补 (deployment_id, created_at DESC) 与
  (deployment_id, supersedes_key) 索引（admin 列表 ORDER BY 与 supersede
  UPDATE 现在都在顺序扫描）。

### B4 DB/部署基线欠账
- postgres `shared_buffers=16MB`（默认值未调）、max_connections=100。
- control-service 单副本、**无 resources requests/limits**；portal DB 池 4。
- 修复：shared_buffers ≥512MB（需重启 PG，业务低峰执行）；control-service
  requests 500m/512Mi limits 2/2Gi、副本 2；portal 池 ≥8。

## C. 工程与流程发现

- **C1**：cicd control-service 落后 main 6 天/4+ PR（旧契约：创建仍要求
  客户端 id，新 Console/SDK 已按新契约发版）——AEP 镜像无 CI/CD 通道，
  靠手工。0.7.0 镜像已构建推送（digest d813f1ab0a01）待授权滚动。
- **C2**：operator 无 429 语义（A1 次生）+ 稳态每 reconcile 都做真实登录
  健康检查（PAT-first 修复已写）。
- **C3**：operator 单 worker（MaxConcurrentReconciles=1）；30 CR 首轮
  全量串行 reconcile（实测影响见 S5 Ready 延迟分布）；休眠 CR 每 15s
  一次漂移检查 ×30 = 持续 2 reconcile/s 空转。
- **C4**：`GET /admin/sessions` 无 cursor（limit 之外不可达）；model/
  credential/skill-assignments 列表无分页；skills 列表 N+1
  （每技能一次 versions 查询，5 技能 14ms，量小暂不痛）。
- **C5**：`PORTAL_QUOTA_PER_USER=2` 连管理员也限（policy.go 顺序问题）；
  且 `envInt` 把一切 0 当未设置回落默认——**配额无法配置为"不限"**
  （cicd 实测 env=0 仍报 limit 2）。修复：PR #11（envIntNonNegative）。
- **C6**：portal /knowledge/* 每请求 WeKnora 管理端登录（无 token 缓存）；
  /memory/status 对每个员工逐个探测 OV 会话（N 次外呼）。
- **C7**：`jwks.json` 被外部组件 ~12 次/分钟拉取（0ms 无成本，观察项）。

## S5/S6/S7 结果

### S5 portal 业务流（30 DE：15 直建 + 15 申请 → 10 批 3 驳 + 遗留态合并）
- **API 层全绿**：直建 p50=16ms、申请 18ms、审批（含建 CR）50ms、驳回 4ms、
  待审列表 5ms；32 个 DE 的 employees-list 85ms（整命名空间 List + 内存过滤，
  300 CR 时需复测）。
- Ready 延迟本轮被 A4 污染（OV 账号缺失 → Error 退避等待）；解围后
  **27 个 CR annotate 触发 → 60 秒内全部 Ready**（operator 单 worker 串行
  + 账号探测，≈2s/CR）。
- 审批队列工作正常：非管理员申请 → pending → approve/reject 语义完整。

### S6 对话端到端（load-de-02 · bench-qwen · 303 用户/40 规则库上）
- 建线程 28ms；**热轮 SSE 全程 533ms**；冷唤醒轮 1.9s（实际是暖启动，
  此前轮次已唤醒）。治理中间件 data-scope/context 24ms×4（60s 缓存生效，
  每轮 0-1 次 AEP 外呼）。
- 休眠体系在 27 CR 规模：KEDA 对象 27 个，空闲 2 分钟内 26/27 缩零，
  operator 休眠态心跳（每 CR 15s 漂移检查）CPU 8m/内存 27Mi——便宜。

### S7 浸泡（5 分钟混合流）
- **169.4 rps，50812 ops，零错误**：users p95=15ms、me 13ms、models/teams
  10ms；登录（Argon2）在混合流中 p50=106/p95=251ms，服务端 127ms——
  仍是唯一显著贵路径。

## 修复 PR

| PR | 内容 |
|----|------|
| #8 fix/operator per-DE 对话库 | conversationStore.database + initContainer + 子 Secret |
| #9 fix/operator PAT-first 探测 | 稳态零登录 + 429 退避 10min |
| #10 chore/loadgen | 本工具 + 本文档 |
| #11 fix/portal 配额 0 | envIntNonNegative |

## 待授权部署项

1. control-service 0.7.0（镜像已推 251，digest d813f1ab0a01）——修契约漂移
2. operator 新镜像（#8+#9 合并后）+ env `CONVERSATION_ADMIN_DB_URL`/
   `CONVERSATION_INIT_IMAGE`，5 个 demo CR 钉 `database: shared` 或迁 per-DE
3. （建议）postgres shared_buffers 16MB→512MB（低峰重启）

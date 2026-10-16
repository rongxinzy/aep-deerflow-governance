# 身份解析 fail-closed 依赖面盘点（C1）

范围：`rongxinzy/aep-deerflow-governance`，基线 `a66d9f5`。目的：把「解析失败 →
回退实例默认用户」这条回退的**依赖面**列清，判断哪些路径还能收口、哪些收口会
破坏正常流程，并给出记忆链（在 `rongxinzy/deer-flow`）的跨仓待办与验收口径。

**本切片不改变任何鉴权行为。** 新增一个测试文件把「解析失败 → 拒绝」的契约钉住
（`tests/test_identity_fail_closed.py`），其余只做盘点与决策记录。逐条依据见下。

## 1. 结论摘要

1. **治理链（本仓）已经收口。** `resolve_aep_user()` 的三级解析在任何一级失败时
   返回 `None`（`aep_deerflow_governance/__init__.py:227-247`），`visible_teams()`
   失败返回空集（`:285-301`），工具层对 `None` 一律 DENIED（`:606-611`）。
   `default_aep_user` 仍被接收，但**不再参与授权**（`aep_deerflow_governance/__init__.py:157-207`）。
2. **记忆链是另一条链，本仓改不到。** 记忆的身份解析在 deer-flow 的
   `openviking_manager._resolve_scope()`；requester 模式已经 fail-closed
   （缺 requester 直接抛 `RequesterScopeError`，读空/写丢弃，不落共享桶），
   但 `memory_scope="instance"`（**默认值**）下未解析请求者仍共享员工记忆空间。
   跨仓待办与验收口径见 §6。
3. **发现 A（记录，未改）**：运行时用户分支会把 `user_id == default_aep_user`
   当作已解析请求者。由于 `defaultAEPUser` 就等于该员工 **owner 的 AEP user id**，
   若某条服务端路径把运行时 user 定为该 id（且本次运行没有 `channel_user_id`），
   未解析请求者会重新拿到「实例默认用户（=owner）」的数据范围——即 #63 删掉的
   那条回退以另一副面孔复活，并且 requester 记忆模式下会落到 **owner 自己的
   记忆命名空间**。复现路径、影响、修复前提与取舍见 §4。
4. `user_map` 分支（命中即视为已解析、不经 AEP 校验）是生产配置里的死分支，
   且最坏后果只是空范围拒绝（fail-closed），见 §2 R4。
5. `singleTenantInstance=true` + `memoryScope=requester` **不是矛盾组合**：requester
   模式下该开关被 `_resolve_scope()` 提前 return 屏蔽，属死配置而非语义冲突。
   因此本仓 operator/CRD 层不做任何收紧（依据见 §2 R10）。

## 2. 依赖面盘点表

「位置」列的行号对应**本次合并 main 之后**的 `aep_deerflow_governance/__init__.py`
（main head `1a7cfb4`）；deer-flow 侧对应 `rongxinzy/deer-flow` 基线 `ec9bc184`
（本 PR 合并 main 后重标过一次；此前版本锚在 `a66d9f5`）。

| # | 路径 / 位置 | 现状 | 解析失败时的后果（谁 403 / 谁静默降级） | 处置 |
|---|---|---|---|---|
| R1 | 治理中间件 **非 IM 分支**（运行时 user）；`aep_deerflow_governance/__init__.py:263-302` | 已 fail-closed：`user_map` 未命中且 `_is_aep_user()` 不通过 → `None`（`:296-297` 未命中、`:298-299` 校验不过、`:301` 返回） | 未解析 → 访客；受管工具返回显式拒绝（`:687` 企业数据拒绝），IM 侧模型层走访客提示（`:590`）。无静默降级 | 保持（已收口） |
| R2 | 治理中间件 **IM 分支**（`channel_user_id`）；`aep_deerflow_governance/__init__.py:281-287` | 已 fail-closed：未配 `identity_source_id` → `None`（`:282-283`）；映射表加载失败/未映射 → `.get()` → `None`（`_load` 见 `:130`；resolver 构造 `:249`，`_load` 异常被吞后置空表） | 同上。注意：AEP 不可达时**全体** IM 发送者判为访客（工具拒绝），属预期 fail-closed | 保持（已收口） |
| R3 | `default_aep_user` 配置面：`aep_deerflow_governance/__init__.py:192-240`（构造参数 `:213`，属性 `:231`）、operator 渲染 `operator/internal/controller/employee_reconcile.go:316`、清单 `k3s/deployment.yaml:22,74`、CRD `operator/api/v1alpha1/digitalemployee_types.go:248`（必填）、chart `charts/zhiyuan-stack/templates/digital-employees.yaml:29`、语义 `portal/README.md:28` | 仍必填、仍渲染、仍保存为实例属性，但**不再作为授权来源**（只在 `resolve_aep_user` 的属性里存在，无读取点） | 无授权后果。风险是"面还在"：任何重新接入该属性的补丁都会复活回退，且文档仍把它写成身份链一级（本仓 `DEVELOPMENT.md` 已在本切片修正） | 保持（从 CRD 删除是破坏性 API 变更，非本切片范围）；文档对齐 |
| R4 | 运行时用户分支的 **`user_map` 命中**；`aep_deerflow_governance/__init__.py:296-297` | 命中即视为已解析，**不经 AEP 校验**。生产不渲染 `user_map`：operator 渲染的 config map 只含 `aep_base_url/deployment_id/service_username/service_password/default_aep_user/identity_source_id/governed_tools/team_names`（`operator/internal/controller/employee_reconcile.go:316` 所在渲染块），`k3s/deployment.yaml:74` 同（env 见 `:22`） | 命中一个已失效/伪造的映射 → `resolved` 非空但 `visible_teams()` 取不到范围 → 空集 → 受管工具拒绝（fail-closed）。仅模型层提示会说"代表 AEP 用户 X"（观感不一致，无越权） | 保持（死配置，且已 fail-closed） |
| R5 | 运行时用户分支的 **`user_id == default_aep_user`**；`aep_deerflow_governance/__init__.py:298-299` | `_is_aep_user()` 通过即返回该 id；`default_aep_user` 由 CR 下发且等于 **owner 的 AEP user id**。（`:263-278` 的 docstring 说明运行时分支会经 AEP data-scope 正缓存校验——所以**伪造** id 会解析成 `None`；但 `default_aep_user` 是一个**真实存在**的 AEP 用户（属主），因此照样通过） | **发现 A**：若服务端把运行时 user 定为该 id，未解析请求者重新获得"实例默认用户（=owner）"范围：受管工具按 owner 范围放行、`requester_id` 发布为 owner id → requester 记忆模式落到 owner 记忆命名空间 | **本切片不改**，见 §4（需跨仓载体才能安全收口） |
| R6 | Portal 网页对话代理；`portal/chat.go:239-240`（盖 `X-DeerFlow-Owner-User-Id`）、`portal/auth.go:186-188`（AEP `/user/me` 无 id 即拒绝） | 盖章值 = 调用者 AEP user id，非空由 `verify()` 保证 | 无回退路径：拿不到身份时请求在 portal 层就 401，不会以实例默认用户代理 | 保持 |
| R7 | deer-flow 内部认证载体；`deer-flow backend/app/gateway/internal_auth.py:62-67`（缺 owner header → `DEFAULT_USER_ID="default"`） | 缺 header 时运行时 user 变 `"default"` | 本仓 `_is_aep_user("default")` 通常查不到该 AEP 用户 → `None` → 访客（fail-closed）；**若 AEP 里真存在 id 为 `default` 的用户则 fail-open** | 跨仓确认（§6） |
| R8 | deer-flow 运行时用户解析末级；`deer-flow .../runtime/user_context.py:215-250`（末级 `DEFAULT_USER_ID`），requester 读取 `:120-132` | 无身份时返回 `"default"`；requester 缺省返回 `None`（不发明身份） | requester 侧 fail-closed；user 侧给出 `"default"` 交由治理判定（见 R7） | 跨仓确认（§6） |
| R9 | 记忆链 **requester 模式**；`deer-flow .../openviking/openviking_manager.py:699-709`；失败策略 `:197-203,266-272,566-575` | requester 为空或 `"default"` → `RequesterScopeError`；按 `failure_policy{read,write}` 读返回空、写丢弃，**不回退共享桶** | 未解析请求者：无记忆可读、写入被丢（fail-closed）。`_publish_requester(None)` 是这条链的输入，本仓已保证不会被换成默认身份 | 本仓无需改；跨仓确认读/写策略与注入面 |
| R10 | 记忆链 **instance 模式 + `single_tenant_instance`**；`deer-flow .../openviking_manager.py:710-726`；本仓渲染 `operator/internal/controller/employee_reconcile.go:402-423`；CRD 语义 `operator/api/v1alpha1/digitalemployee_types.go:129-145` | `memory_scope="requester"` 时 `:699-709` 提前 return，**`single_tenant_instance` 不参与**（deer-flow `config.py:146-170` 也不做交叉校验，只单独校验各字段）；`instance` 模式下 `single_tenant_instance=true` 将任意运行时 user 映射到员工 owner（`:712-724`），`false` 则非 owner 运行时 user 直接抛 `MemoryManagerError`（`:725`） | 未解析请求者 + `instance` 模式（**默认**）→ 共享员工记忆被注入给访客：**记忆链不跟随治理链的拒绝**。这是记忆侧真正未收口的部分，但不是"字段组合矛盾" | **不做字段收紧**（无矛盾可证）；跨仓待办 §6 |
| R11 | 记忆治理写入面 HTTP 路由；`deer-flow backend/app/gateway/routers/memory.py` | 只解析 `user_id`，不传可信 requester；适配器 `create_fact/update_fact` 未实现 | requester 模式下 HTTP 路径拿不到 requester；CRUD 返回 501 | 跨仓（已提 PR：**deer-flow #20** 贯通可信 requester、**#21** 实现写入面；见 §6） |
| R12 | （相邻风险，非 C1）邮件工具身份门；`aep_deerflow_governance/__init__.py:672-675`（`_check` 见 `:660`） | **已在 main 收口**（治理 **#104**，2026-10-16 合入）：`email_list` / `email_read` / `email_send` 在进 `governed` 集合判定**之前**就要求已解析的企业身份，同步与异步工具链都在执行前拦截访客（`:675` 为拒绝文案） | 未映射 IM 发送者（访客）**不能再**读写员工邮箱 | 保持（已收口，本切片只更新口径）。**注**：这仍只是最低身份门槛，≠ 邮箱逐用户授权 / 发信审批 / 停用账号即时失效，后者仍须 AEP 账号状态与授权契约补齐（与 main `README.md` 的表述一致） |
| R13 | （相邻风险，非 C1）知识库工具身份门；`aep_deerflow_governance/__init__.py:446`、`:472-488`、`:514-535`、`:563-565` | **缺省语义已在 main 收口**（治理 **#110**，2026-10-16 合入）：`knowledge_bases is None`（未配置策略）按**配置错误**拒绝（`:483-485`，`kb_policy="missing"` 见 `:242`），`{}`（空白名单）为 deny-all（`:487-488`）。显式白名单下的只读工具仍**不看调用者身份**（`:514-535` 只按白名单与在线授权判定，`:563-565` 把白名单 id 注入工具调用） | 未解析（访客）在**无策略 / 空白名单**下被拒绝；**显式白名单**中列出的只读检索仍可被访客调用 | 保持原状：显式白名单下放行访客知识问答是既有产品口径（`:589-596` 的访客提示写明"知识问答照常回答"），收紧属产品决策，不在 C1 范围。**重要：`knowledge_bases is None` 已经不是访客放行** |

## 3. 逐条理由（为什么"能收口的都收了"）

- R1/R2 的返回形状已经是 `None`，R9 的输入因此只会是 `None` 或可信 AEP id；
  这两条是本切片新增测试的断言对象。
- R3/R4 都不产生**授权**后果：前者没有读取点，后者最坏是空范围。删除/收紧它们
  只会改变 API 面（CRD 必填字段、operator 渲染、清单模板），不改变安全属性，
  且是破坏性变更 —— 留给单独的清理切片。
- R5 是唯一"解析失败仍拿到身份"的路径，但它的安全收口需要一个能区分
  「portal 盖章的 AEP 身份」与「服务端遗留/默认身份」的载体，该载体在 deer-flow
  （R6~R8），本仓无法凭 id 相等关系安全区分。

## 4. 发现 A：运行时用户等于实例默认用户（记录，未修）

**代码依据**

1. `resolve_aep_user()` 非 IM 分支对任何能通过 `_is_aep_user()` 的运行时 user id
   直接返回该 id（`aep_deerflow_governance/__init__.py:298-299`），**没有**把
   `default_aep_user` 排除在外。
2. `defaultAEPUser` 就是该员工 owner 的 AEP user id：`portal/README.md:28`
   （"governance.defaultAEPUser 同步设为 owner——员工默认以属主的数据范围行事"）、
   chart 模板 `charts/zhiyuan-stack/templates/digital-employees.yaml:29`
   （`defaultAEPUser: {{ $.Values.aep.adminUserId }}`）。
3. 因此 `user_id == default_aep_user` 既可能是**合法**的属主本人（portal 盖章
   调用者自己的 AEP id，R6），也可能是**遗留**的服务端默认身份（R7/R8 的
   `"default"` 只有在 AEP 真存在同名用户时才等价）。二者在当前代码里不可区分。

**复现路径（单元级，已固化为测试）**

`tests/test_identity_fail_closed.py::test_residual_runtime_user_equal_to_instance_default_is_trusted`
用一个把 `default_aep_user` 本身当作"有效 AEP 用户"的假 AEP 客户端，断言当前实现
返回该 id 且把它发布为 `requester_id`（而不是 `None`）。这就是 #63 删掉的回退的
可观测等价物。

**影响（若被触发）**

- 工具面：未解析请求者按 owner 的 `orgScope` 通过受管工具（部门数据可读）。
- 记忆面：`requester_id` 被发布为 owner id，requester 记忆模式会把该运行放进
  **owner 自己的记忆命名空间**（`_requester_peer_id()` 对 UUID 形态的 AEP id 原样
  返回，见 `deer-flow .../openviking/session.py:57-72`）→ 跨用户记忆读写。

**修复前提与取舍**

- 前提（跨仓）：deer-flow 需要在运行时身份载体里给出**服务端所有权标记**
  （"这个 user id 来自 portal 盖章的调用者身份" vs "来自内部/遗留默认"），
  治理侧才能只信任前者。仅凭 id 相等关系判断一定误伤。
- 取舍：**朴素修法（`user_id == default_aep_user → None`）不可取** —— 属主本人经
  portal 网页对话时 `ident.UserID` 恰好等于 `defaultAEPUser`，会被误判为访客并被
  拒绝受管工具。这正是 C1 前置要求"先列依赖面，否则会让正常流程 403"指向的坑。
- 备选（未采纳，需产品决策）：让 portal 不再把 owner 的 AEP id 当作
  `defaultAEPUser`（改为单独的员工服务身份），使 id 相等不再可能。这会改变
  "员工默认以属主的数据范围行事"这条既有语义（`portal/README.md:28`），影响面
  超出本切片。

## 5. 本切片改了什么 / 没改什么

- 改：本文件（盘点 + 决策；已按 2026-10-16 合入 main 的 **#104 / #110** 更新 R12 / R13
  并**重标全部行号**）、`tests/test_identity_fail_closed.py`（契约回归）、
  `DEVELOPMENT.md` 身份链段落（原文仍把 `default_aep_user` 写成链末一级，与
  `README.md:32` 及代码不一致）。
- 合并：用**普通 merge commit** 把 `origin/main`（含 #104 #105 #106 #109 #110）并入本分支
  —— 不改写已推送历史。冲突仅在 `README.md`（#104 改过同一文件），解决时**两侧都保留**
  （main 的邮件门/知识库语义段 + 本切片的盘点指针段）。
- 没改：`aep_deerflow_governance/__init__.py` 的任何判定逻辑、operator/CRD、
  清单模板、portal。**鉴权行为零变化**（提交前后 `resolve_aep_user` / `_check` /
  `visible_teams` 的成败分支逐一相同）。

## 6. 跨仓待办与验收口径（`rongxinzy/deer-flow`）

> 记忆链的收口只能在 deer-flow 完成；切片 S2 正在改
> `.../openviking/openviking_manager.py`，为避免并发冲突，本切片只登记不实现。

1. **instance 模式下的未解析请求者**（对应 R10）：判断是否应让记忆链感知治理链的
   拒绝（例如 requester 未解析时按"无记忆"处理，而不是注入员工共享记忆）。
   验收口径：`memory_scope=instance` + `single_tenant_instance=true` 下，
   `requester_id is None` 的运行**不得**读到员工共享记忆（若决定收口），
   或明确记录"这是设计内的单租户语义"并补一句口径说明（若决定保持）。
2. **HTTP 记忆面贯通 requester**（R11）：`routers/memory.py` 各操作传可信
   requester，requester 模式缺 requester 必须拒绝、不得兜底成 owner。
   验收口径：HTTP 级 A/B 用户互不越界 + requester 缺失时拒绝 + 非 requester
   模式行为不变（**已提 PR：deer-flow #20**；写入面为 #21）。
3. **运行时身份的载体与所有权**（R5/R7/R8）：为"portal 盖章身份 vs 内部/默认身份"
   提供可判定的服务端标记。验收口径：伪造 `body.context` /
   `body.config.context` / `body.config.configurable` 中的同名字段**不改变**
   治理解析结果；内部默认身份（无 owner header）不得解析成任何 AEP 用户。
4. **回归**：`aep_deerflow_governance` 侧已由本切片钉住"未解析 → `None` →
   工具拒绝"；deer-flow 侧需要补"未解析 → 记忆不共享"的用例，两侧断言互补。

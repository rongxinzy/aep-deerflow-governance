# 知识库白名单：缺策略默认拒绝（迁移与回退）

治理中间件 `aep_deerflow_governance:AepGovernanceMiddleware` 的 weknora
知识库工具改为**缺策略默认拒绝**。本文件是这次行为变更的迁移规则、管理员提示与回退说明，
也是运行时拒绝文案（`_KNOWLEDGE_MISSING_POLICY_REASON`）的出处。

## 为什么移除「缺策略即放行」

weknora bridge 的凭据是**租户级**的，部署中的 PyPI bridge 还带管理权限
（建/删租户、知识库、模型）。因此「没配白名单 = 用租户全量凭据」等于把整个租户的知识库
（含未授权库）暴露给该数字员工。旧行为（`spec.knowledge.bases` 为 nil 时不渲染
`knowledge_bases`，中间件放行）已移除。

## 策略状态（新行为）

| 配置 | 读类工具（hybrid_search / wiki_search / knowledge_search / list_knowledge / get_knowledge_base 等） | 写/删/管理类（create_* / update_* / delete_* / chat / session / create_model …） | 配置错误提示 |
| --- | --- | --- | --- |
| 无策略（CR 的 `spec.knowledge.bases` 缺失/`null`） | 拒绝 | 拒绝 | 有（可操作，含 `spec.knowledge.bases`） |
| `knowledge.bases: []`（非 nil 空列表） | 拒绝（deny-all） | 拒绝 | 无 |
| 显式列表（id + name） | 仅列表内 kb，且必须显式传 `kb_id` / `knowledge_base_ids` | 拒绝 | 无（越库/省略 id 提示可用库清单） |

非知识库工具（`web_search`、其它 MCP server 的同名工具等）不受影响。
读、写、删除的授权彼此独立：显式白名单只放行**读**，写与删除一律拒绝。
当前用户的在线检查（`knowledge_online_authorization` / Portal run context）与检索结果的
原始引用行为保持不变：在线检查仍在显式策略之上叠加，不因本次变更放宽或收紧其它路径。

## 1. 只读盘点缺策略实例

先盘点，再改代码/升级：列出挂了知识库但没写 `bases` 的数字员工（这些实例在升级后会立刻
变成知识库工具全拒绝）：

```bash
kubectl get digitalemployees -A -o json | jq -r '
  .items[]
  | select(.spec.knowledge != null)
  | select((.spec.knowledge.bases // null) == null)
  | "\(.metadata.namespace)/\(.metadata.name)"'
```

同时确认哪些实例会**挂载** weknora 工具（只有挂了工具才有影响）：

```bash
kubectl get digitalemployees -A -o json | jq -r '
  .items[]
  | select(.spec.knowledge != null)
  | "\(.metadata.namespace)/\(.metadata.name)\tbases=\(.spec.knowledge.bases // "null")"'
```

（`spec.knowledge == null` 的实例不挂载 weknora MCP，不在受影响范围。）

受影响来源有两类：直接写 CR 时省略 `spec.knowledge.bases`；以及控制台/portal 创建或
PATCH 时未显式给出 `knowledgeBases`（portal 对 nil 输入原样落成 `Bases: nil`）。
portal 本次不改（非目标）：控制台需要为员工**显式选择知识库**或显式选「无」（空列表），
否则升级后该员工的知识库工具会按缺策略默认拒绝。（portal 的 PATCH 语义本就禁止回到
nil 无策略状态，与本次变更一致。）

## 2. 生成显式可访问库列表

对每个受影响实例，用**只读**方式确认该员工**实际需要**的 kb id，再写入白名单：

- 用该实例原本的 WeKnora 凭据执行只读的 `list_knowledge_bases`（或等价的只读目录查询），
  得到该员工此前实际检索过/应访问的库；
- **不要**把租户全量库照抄进白名单，也不要把「未知旧库」自动授给实例 owner——白名单必须
  由管理员按最小授权逐条确认。

## 3. 迁移步骤（管理员）

1. 按上面两节盘点受影响实例，记录每个实例应保留的 kb id 与名称。
2. 编辑对应 `DigitalEmployee`：

   ```yaml
   spec:
     knowledge:
       type: weknora
       mcpURL: <不变>
       bases:
         - {id: "kb-1", name: "销售知识库"}
         - {id: "kb-2", name: "产品知识库"}
   ```

3. 等 operator reconcile 完成（渲染出 `knowledge_bases` / `knowledge_online_authorization`），
   让用户重新发起本次对话——被拒绝的调用不会自动重试。
4. 确实不应访问任何知识库的实例，显式写 `bases: []`（deny-all），而不是留空/删掉整个
   `knowledge` 段：后者语义是「未配置」，同样默认拒绝，但会让管理员难以区分「刻意收回」
   与「漏配」。

## 4. 回退

- **恢复某员工的知识库访问**：在 `spec.knowledge.bases` 写入显式列表（可含多个库），
  reconcile 后生效。
- **缩小/收回**：删除对应 `bases` 条目（或整表写 `[]`）。
- **回退本次行为变更（仅应急）**：回滚 operator/governance 镜像到旧版本，旧版本对
  `bases: nil` 仍走放行分支。**不要**为了省事给实例配「租户全量库」来模拟旧行为。
- 缺策略不再有任何隐式放行开关：`knowledge_bases` kwarg 缺席就是拒绝。

## 运行时拒绝文案

```
DENIED <tool>: 知识库访问策略缺失（配置错误）：该数字员工未配置知识库白名单
（spec.knowledge.bases 缺失），缺策略时知识库工具按默认拒绝处理，原「未配置即不受限」的放行已移除。
管理员修复：① 只读盘点该员工实际需要的 kb id…② 在 DigitalEmployee 的 spec.knowledge.bases 显式写入这些库…
③ 保存并等 operator reconcile 完成后，重新发起本次调用。若该员工确实不应访问任何知识库，请显式配置
bases: []（deny-all）。迁移、盘点与回退步骤见 docs/design/knowledge-whitelist-default-deny.md。
```

被拒绝的调用**不会**触发原 handler（中间件在 `wrap_tool_call` 前置判定即返回）。

## 未覆盖 / 跨仓依赖

- 「撤销授权后，**基于该来源的历史记忆召回**同样受限」需要 DeerFlow 记忆侧按知识库来源过滤，
  不在本仓（治理中间件只约束知识库 MCP 工具调用）；本仓只覆盖工具调用层面的立即生效。
- 控制台/portal 侧对 `bases` 的编辑与校验不在本次变更范围。

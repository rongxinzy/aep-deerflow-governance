# IM 通道验证记录（飞书 leg）

状态：进行中 · 2026-10-12 · 环境：本机 WSL2 k3s（dev 形态）
范围：数字员工飞书通道的**协议级端到端**行为。企微（`de-channel-gateway`）与真实凭证联调未覆盖，见「未覆盖」。

> 本文是验证**记录**，不是设计规范。需求口径以 `zhiyuan-docs` 为准（FR-CH-02/03、FR-AC-08）。

## 1. 环境与形态（实测）

| 项 | 值 |
|---|---|
| 集群 | 本机 WSL2 k3s，命名空间 `digital-employees` |
| 运行时 | `deploy/deerflow-gateway`（治理版镜像，**非** operator CR 形态） |
| DigitalEmployee CR | **无**（`kubectl get de` 为空；走旧形态直连 Deployment） |
| `de-channel-gateway` | **未部署**（chart `channelGateway.enabled=false`） |
| 飞书通道配置 | 来自 ConfigMap `deerflow-gov-config`：`channels.feishu{enabled:true, app_id:cli_gov, domain:http://<wsl-host-ip>:18086}` |
| 身份源 | `governance.identitySource=gov-feishu`（占位符已实值化） |
| 平台替身 | `mockfeishu`（picoclaw `tests/aep-e2e/mockfeishu`，协议级替身：真 WS + REST + `_test/*` 面） |

## 2. 方法

mockfeishu 提供与真实飞书 SDK 不可区分的 WS/卡片协议，测试面：

- `POST /_test/push {chatID,senderOpenID,text,chatType,mentionBot}`
- `GET  /_test/sent` → 记录到的出站消息（反应 / 卡片 POST / 卡片 PATCH）
- `POST /_test/reset` → 清空记录

一键脚本：`scripts/im-mock-smoke.sh`（起 mock + 推一条 + 打印出站记录）。

## 3. 实测结果

| # | 用例 | 动作 | 期望 | 实测 | 判定 |
|---|---|---|---|---|---|
| V1 | 通道重连自愈 | 平台由**不可达→可达** | 通道恢复 | 重连循环在下一个周期停止；`/_test/push` 返回 200 | ✅ |
| V1b | 平台重启后恢复 | 连接建立后**重启平台**（up→down→up） | 通道恢复 | 180s 内持续 `503`，未恢复；日志出现 SDK 崩溃堆栈（见下） | ❌ 缺陷复现 |
| V2 | 入站接收 | 推 p2p 消息 | 通道收到 | 出站出现反应 + `thinking...` 交互卡 | ✅ |
| V3 | 卡片流式 → 最终 | 等一轮结束 | POST + PATCH 原位更新 | `interactive` 卡 → 两条 `card-update`（流式帧带 `▉`、终帧无光标） | ✅ |
| V4 | 状态反应 | 同上 | OK / DONE 反应 | 出站首尾各一条空内容反应记录 | ✅ |
| V5 | 未映射发送者（访客） | 推未映射 `sendOpenID` | 访客模式：普通对话继续 | 正常出卡（未整体拒答） | ✅ 口径一致 |
| V6 | 群聊非 @ | `chatType=group, mentionBot=false` | **忽略**（文档 FR-CH-02「群聊按 @ 策略响应」） | 仍产生完整回复（reset 后 0→5 条） | ❌ 偏差 |
| V7 | 群聊 @ | `mentionBot=true` | 应答 | 应答 | ✅ |
| V8 | 业务答复 | 任一轮 | 正常答案 | 最终卡为 `LLM request failed: The plain http request was sent to https port` | ❌ 下游故障 |

V6/V8 细节：

- **V6（群聊未 @ 应答）根因已定**：DeerFlow fork `backend/app/channels/` 中**只有 discord 有 `mention_only`**；`feishu.py` 无任何群聊 @ 门控（`group_trigger.mention_only` 是 picoclaw 侧字段，DeerFlow 无对应）。即文档 FR-CH-02 的 @ 策略在 DeerFlow 迁移中丢失。
- **V8（模型调用失败）**：错误文本为历史坑「plain http request sent to https port」——模型网关链路（Higress 80 listener 400）问题，**与通道无关**。通道的失败兜底是正确的（错误作为最终答案下发，未丢消息）。

## 4. 结论

- 飞书通道的**通道层**（WS 入站、卡片协议、反应、失败兜底、断线自愈）在本 dev 形态下**验证通过**。
- **lark 重连缺陷已精确复现（V1b）**，分两条路径：
  - 平台**不可达→可达**（V1）：走外层重试循环，**自愈** ✅。
  - 连接**已建立后平台重启**（V1b）：SDK 的 `_receive_message_loop` 断开后调 `_reconnect()`，在 `lark_oapi/ws/client.py:285 if self._reconnect_nonce > 0` 抛
    `TypeError: '>' not supported between instances of 'NoneType' and 'int'`，重连协程崩溃，**通道死透，需重启运行时实例** ❌。
  - 影响：平台侧重启/发版/抖动都会让已连通的飞书通道静默失效（无健康上报、无自愈）。对应 NFR-REL-05 的「已知限制」。
- **重连节奏**：外层重试循环约每 120s 一次（仅对「从未连上」的启动期有效，见 V1）。一旦连接建立过又断开，走的是会崩溃的 `_reconnect()`（V1b），重试循环不再兜底。`im-mock-smoke.sh` 的 attach 等待窗口据此设为 180s。
- **通道可用 ≠ 业务可用**：V8 表明当前部署模型链路故障，任何业务验收都会被它挡住。

## 5. 未覆盖

| 项 | 原因 |
|---|---|
| 企微通道（aibot 收发） | **无法用 mock 验证**：`wecom.py:184` 用 `WSClient(WSClientOptions(bot_id, secret, logger))` 建连，配置 schema 只有 `bot_id`/`bot_secret`（`service.py:48`），**无 endpoint/url 覆盖**；picoclaw 的 `mockwecom` 只对支持 `websocket_url` 的通道有效。须**真实企微 bot 凭证**。 |
| `de-channel-gateway` inbox/ack/send + 休眠唤醒 | 本地无该镜像，且其 Dockerfile `FROM ...deerflow-gov:v18` 与本地现役 `v3` 不符；Harbor 在本机解析到一个不可用地址（本地拉不到）。且该 leg 复用同一 `WeComChannel`，同样无法指向 mock。 |
| 真实飞书/企微凭证 | 仅有 mock；真实 app_id/secret 未提供 |
| AEP 身份 fixture（`gov-*`、`gov-feishu` 映射） | dev AEP 管理员口令已轮换，无法确认/重建 fixture |
| P1/P2/P3 业务用例（范围内放行 / 跨部门拒绝 / 未映射拒答） | 依赖 fixture + 可用模型 + 受管工具 MCP |

## 6. 复现

```bash
# 1) 构建替身（一次性）
git clone --depth 1 https://github.com/rongxinzy/picoclaw.git
cd picoclaw && GOOS=linux GOARCH=amd64 go build -tags 'goolm,stdjson' \
  -o /tmp/mockfeishu ./tests/aep-e2e/mockfeishu

# 2) 起 mock 并冒烟（在 k3s 宿主上执行；WSL 的 /mnt 为 noexec，须拷进本地盘）
MOCK_FEISHU_BIN=/tmp/mockfeishu bash scripts/im-mock-smoke.sh
```

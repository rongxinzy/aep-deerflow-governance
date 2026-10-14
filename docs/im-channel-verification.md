# IM 通道验证记录（飞书 + 企微 leg）

状态：进行中 · 2026-10-12 · 环境：本机 WSL2 k3s（dev 形态）
范围：数字员工飞书 / 企微通道的**协议级端到端**行为。真实凭证联调、`de-channel-gateway`（休眠）与业务用例未覆盖，见「未覆盖」。

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

### 企微 leg（mockwecom）

DeerFlow 的 wecom 通道默认连官方 `openws.work.weixin.qq.com`；本次通过 **ConfigMap 覆盖补丁模块**把它指向 `mockwecom`（见第 6 节脚手架），实测：

| # | 用例 | 动作 | 实测 | 判定 |
|---|---|---|---|---|
| W1 | 企微通道连接 | 起 mockwecom + 配 `channels.wecom{bot_id,bot_secret,ws_url}` | 日志 `Connecting to WebSocket: ws://<host>:18088` → `WebSocket connection established` → `Authentication successful` | ✅ |
| W2 | 入站接收 | `POST /_test/push {userID,text,chatType:single}` | `push` 200，出站出现 `aibot_respond_msg` 流 | ✅ |
| W3 | 流式 → 最终 | 等一轮结束 | `Working on it...`（finish=false）→ 流式帧（带 `▉`）→ 终帧（finish=true） | ✅ |
| W4 | 业务答复 | 同 V8 | 终帧内容同为 `LLM request failed: The plain http request was sent to https port` | ❌ 下游故障（同 V8） |

结论：企微 leg 的**通道层**（aibot WS 建连/认证、入站接收、`aibot_respond_msg` 流式回复协议）**验证通过**；业务答复受同一模型链路故障挡住。

## 3b. 测试脚手架（仅本机 dev，非产品改动）

要让企微走 mock，需两处**仅用于本机 dev 的 ConfigMap 覆盖**（不重建镜像）：

| 覆盖 | 作用 | 说明 |
|---|---|---|
| `wecom-wsurl-patch`（`wecom.py`） | 让 `WeComChannel` 读取并透传 `ws_url` | 即 DeerFlow#7 的补丁（本机版） |
| `aibot-ws-plain`（`aibot/ws.py`） | 明文 `ws://` 时传 `ssl=None`（SDK 原实现无条件带 SSL 上下文，拒绝 `ws://`） | 仅 mock 用；真机走 `wss://` 不受影响 |

叠加 Deployment 上两个 `volumeMount`（subPath）+ `config.yaml` 里的 `channels.wecom`。

**回滚**：`/tmp/chanbak/` 下有 `cm.yaml`、`deploy.yaml` 备份；`k3s kubectl apply -f` 还原后 `rollout restart` 即可。真机不要带这两处覆盖。

> 端到端真实 mock 复现还需处理 SDK 的 `ws://` 限制：要么上游让 `aibot` 支持明文 ws，要么把 mock 架在 TLS 后面。

## 4. 结论

- 飞书通道的**通道层**（WS 入站、卡片协议、反应、失败兜底、断线自愈）在本 dev 形态下**验证通过**。
- 企微通道的**通道层**（aibot WS 建连/认证、入站接收、`aibot_respond_msg` 流式回复）**验证通过**（W1–W3），但复现**依赖两处覆盖**：`wecom.py` 透传 `ws_url`（DeerFlow#7）**加上** `aibot/ws.py` 允许明文 `ws://`（或把 mock 架在 TLS 后）。**只合 DeerFlow#7 复现不了 W1–W3**。
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
| 企微通道（aibot 收发） | **通道层已验**（W1–W3，见第 3 节），复现依赖两处覆盖（`wecom.py` 透传 `ws_url` + `aibot/ws.py` 放宽明文 `ws`）。真实凭证联调未做。上游长期修法是让 `wecom.py` 透传 `ws_url`（DeerFlow#7）；明文 mock 另需 SDK 支持或 TLS 包装。 |
| `de-channel-gateway` inbox/ack/send + 休眠唤醒 | 镜像本体已取得（OCI 布局，`Downloads/`），但尚未导入集群；其内嵌代码来自 `deerflow-gov:v18`，本地现役运行时为 `v3`，且该 leg 复用同一 `WeComChannel`，同样受上面一行缺口影响。 |
| 真实飞书/企微凭证 | 仅有 mock；真实 app_id/secret 未提供 |
| 现役运行时源码 | `rongxinzy/deer-flow` main 带平台模式补丁，但**无 channel-gateway 模式**（`DEERFLOW_EMPLOYEE_NAME`/`channel_inbox`/`/inbox/` 零命中），而 governance `channel-gateway`/operator 依赖该模式。说明现役 `deerflow-gov` 镜像由 fork main 之外的源码树构建 → 直接按 fork main 重建运行时有**特性回退风险**，需先确认 v3 的源码来源。 |
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

企微（W1–W3）：

```bash
# 1) 构建 mockwecom（Windows 侧产出 linux 二进制）
GOOS=linux GOARCH=amd64 go build -o D:/tmp/mockwecom ./tests/aep-e2e/mockwecom
# 2) 在 k3s 宿主执行：WSL 的 /mnt 是 noexec，必须把二进制拷进本地盘再跑
cp /mnt/d/tmp/mockwecom /tmp/mockwecom && chmod +x /tmp/mockwecom
nohup /tmp/mockwecom -addr 0.0.0.0:18088 -bot-id mockbot -secret mock-secret >/tmp/mockwecom.log 2>&1 &

# 3) 两处 ConfigMap 覆盖（wecom.py 透传 ws_url；aibot/ws.py 允许明文 ws）+ 挂载到 Deployment
#    与开启 channels.wecom{bot_id,bot_secret,ws_url=ws://<host>:18088}，然后 rollout restart
#    （步骤见第 3b 节；先备份 cm/deploy）。注意真机不要带这两处覆盖。

# 4) 推一条并看回复
curl -s -X POST http://127.0.0.1:18088/_test/push -H 'Content-Type: application/json' \
  -d '{"chatID":"wc_probe","userID":"saber","text":"你好","chatType":"single"}'
curl -s http://127.0.0.1:18088/_test/sent   # → aibot_respond_msg 流（Working on it → 流式 → finish:true）
```

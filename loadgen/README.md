# loadgen — 组织规模 + 负载测试工具

针对已部署栈（默认 cicd：AEP :30180 / portal :30190）做大规模组织种子注入与
业务流/性能压测。纯 Node（>=24）无第三方依赖。

## 配置

`loadgen/.env.local`（gitignored，从 `~/.zhiyuan-cicd-secrets.md` 提取）：

```
AEP_BASE_URL=http://172.18.5.188:30180
PORTAL_BASE_URL=http://172.18.5.188:30190
AEP_ADMIN_USER=admin
AEP_ADMIN_PASSWORD=...
SEED_USER_PASSWORD=...          # 300 个测试用户的统一临时密码（12-1024 字符）
```

## 用法

```bash
node loadgen/seed-org.mjs [--users 300] [--roots 3] [--teams-per-root 10]
                          [--roles 10] [--rules 40] [--resume]

node loadgen/scenarios.mjs list-admin   # S1 管理面分页遍历（并发 1/10/30）
node loadgen/scenarios.mjs auth         # S2 登录风暴 / me / refresh
node loadgen/scenarios.mjs events       # S3 控制事件 全链（建→投→ack→result）+ 心跳
node loadgen/scenarios.mjs telemetry    # S4 遥测批写入 + 管理端检索
node loadgen/scenarios.mjs portal-de    # S5 portal 业务流：建 30 个 DE（15 直建 +
                                        #    15 审批 + 5 驳回）、Ready 时延、休眠、唤醒
node loadgen/scenarios.mjs chat         # S6 并发 SSE 对话（bench-qwen 本地模型）
node loadgen/scenarios.mjs soak         # S7 混合流量浸泡
```

结果落在 `loadgen/results/<时间戳>-<场景>.json`；每场景前后抓取 AEP
`/metrics` 服务端直方图做差分（服务端视角，排除网络）。种子状态在
`/tmp/zhiyuan-loadgen-state.json`（`--resume` 跳过已建实体）。

## 通用旗标

`--users/--agents/--duration/--concurrency` 等按场景不同；不带参数运行即打印默认值。

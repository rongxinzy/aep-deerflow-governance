# cicd 集群部署（172.18.5.188，k3s 双节点 rtl-control/rtl-l20）

知远企业版在共享 cicd 集群上的部署清单与脚本。与 dev（本机 k3s）的差异与
端口约定见 zhiyuan-docs `07-deployment` 的 "cicd 集群部署" 一节；本目录只放
清单。

## 命名空间与端口

| 命名空间 | 内容 |
| --- | --- |
| `aep-system` | AEP 控制面（NodePort 30180） |
| `higress-system` | Higress 2.2.4 模型网关 |
| `memory-system` | embed-server + OpenViking |
| `digital-employees` | operator、portal（NodePort 30190）、chat-ui（30195）、admin-console（30196）、员工运行时 |

l20 带 `rtl.ai/externally-managed:NoSchedule` 污点，负载默认全部落在
rtl-control；l20 的 `/etc/rancher/k3s/registries.yaml` 已配置（与 188 相同），
未来分担调度只需加容忍度。

## 文件

| 文件 | 作用 |
| --- | --- |
| `bigmodel-anthropic-route.yaml` | 在线 GLM 的 openai↔anthropic 转换子路由（VLM 用；Anthropic 原生透传已收编 AEP，见"已知边界"），`BIGMODEL_API_KEY=... envsubst` 渲染后 apply |
| `memory-stack.yaml` | 记忆栈（VLM 走 Higress 在线 GLM），`OPENVIKING_ROOT_API_KEY=... envsubst` 渲染后 apply |
| `sales-helper.yaml` | 首例员工 CR（anthropic provider 经 Higress 声明式路由 `/bench-anthropic`；企微通道关闭——bot WS 单会话，dev 仍在使用） |
| `demo-employees.yaml` | 演示员工批量清单（财务/生产/研发/行政，与 sales-helper 同规格） |
| `seed-aep.sh` | AEP 种子：wecom 身份源、saber 映射、gov-service 账号；密码走 env |

AEP 栈本体沿用 `Agent-Enterprise-Protocol/deploy/kubernetes/local-k3s/aep-stack.yaml`
改密码后 apply；CRD/operator/portal/chat-ui 直接用各自 deploy 目录的清单。
`de-platform-creds`/`de-portal-db` Secret 按同样的键名在 cicd 上单独创建
（值见工作区外凭证备忘，不入 git）。

## 部署顺序

1. 两节点 `registries.yaml`（172.18.5.251 http + auth）
2. AEP 栈 → readyz 200 → postgres 建 `deportal` 库 → `seed-aep.sh`
3. Higress → `bigmodel-anthropic-route.yaml`（VLM 转换子路由）→ AEP 注册
   anthropic 模型并 `data-plane/publish`（`aepctl model create --protocol
   anthropic`，凭证走 AEP credential + `aep-credential-<id>` Secret），员工 CR
   的 `modelBaseURL` 加 `/<模型ID>` 前缀。已注册模型：
   - `bench-anthropic` → 在线 BigModel（glm-5.3-flash，凭证走 AEP）
   - `bench-qwen` → l20 节点 llama.cpp（`http://172.18.5.123:8000`，
     Qwen3.6-35B-A3B，双协议无需凭证；llama-server 原生支持 /v1/messages，
     故按 anthropic 协议注册，走 /bench-qwen 前缀路由）
4. `memory-stack.yaml` → root key 调 accounts API 建 `zhiyuan` 账号
   （员工级 user 由 operator 自动开）
5. `operator/deploy/crd.yaml` + `operator.yaml` → Secret →
   `portal/deploy/portal.yaml` → `k3s/chat-ui.yaml` →
   zhiyuanAaaS `deploy/admin-console.yaml`（企业版前端，30196） →
   `sales-helper.yaml`
6. （可选）演示员工：`demo-employees.yaml`
7. 验证：`kubectl get de` Ready/Ready；portal 登录→选员工→SSE 真模型回复；
   跨会话记忆（告知事实后等待约 1 分钟抽取落库，新线程可召回）

## WeKnora（2026-09-28 补装）

dev 侧故障根因是 redis PVC 的 AOF 文件损坏（`redis-check-aof --fix` 修复后
全栈恢复）。cicd 用 WeKnora fork main 分支的 helm chart（`zhiyuan/helm-extra-env`
补丁已并入）全新安装：values 密钥随机生成（存工作区外凭证备忘），
`SSRF_WHITELIST_EXTRA` 同时放行 higress 网关与内网 embed-server；
`k3s/weknora-mcp.yaml` 按 cicd bearer 渲染后 apply（`WEKNORA_API_KEY` 待
实例初始化后回填）。实例初始化清单：管理员账户 → 嵌入模型指向
`http://embed-server.memory-system.svc.cluster.local:8080/v1`（bge-small-zh-v1.5）
→ 建库 → 租户 API key 回填 mcp → 员工 CR 挂 `spec.knowledge`（注意 operator
配置变更不滚动 pod，需重启运行时生效）。

## 已知边界

- WeKnora 已部署（栈健康、mcp 桥就位），但实例内容初始化未做（见上）。
- 企微通道在 dev 与 cicd 之间是单选：bot 的 WS 会话只有一个。迁移 =
  dev CR `channels.wecom: false`、cicd CR 置 true 并注入通道凭证。
- ~~AEP 契约的 model protocol 枚举仅 `openai-compatible`~~ **已收编
  （2026-10-03）**：AEP protocol 枚举含 `anthropic`，gateway-reconciler 按
  每模型前缀（`http://<gateway>/<模型ID>`）渲染 Ingress+EnvoyFilter 透传，
  凭证走 AEP credential 体系。本目录手工透传路由已退役，清单只剩 VLM 转换
  子路由（ai-proxy claude 转换，AEP 暂不承载 openai→anthropic 转换语义）。
  运行时账号 429 限流问题独立存在（见 operator 日志），与路由无关。

## 2026-10-07：注册表与存储标准化（Harbor + Longhorn）

### 镜像注册表
- **主库 = 集群内 Harbor**：`harbor.zhiyuan.local:30443`（compute1 节点，cert-manager 真证书，SAN 含域名+节点 IP）。所有清单的镜像引用一律走该 DNS 名，**禁止出现任何 IP**（换环境只换 DNS）。
- 节点级镜像链：`registries.yaml` 把 `172.18.5.251:8081` 映射为镜像端点 `https://harbor.zhiyuan.local:30443`（Harbor 优先、jfrog 原仓自动兜底老 tag），CA 见 `/etc/rancher/k3s/harbor-ca.crt`。
- jfrog（172.18.5.251:8081）**不再是主库**：降级为冻结归档 + 兜底；新版本推 Harbor。Harbor 侧的 `dockerhub`/`quay` 代理项目统一承接外部镜像拉取。
- 部署资产：`harbor/values.yaml`（含证书链 `harbor/certs.yaml`）。
- 推送注意：集群侧推送需要注册表 CA 与凭据（`--hosts-dir` 配 DNS 名条目的 certs.d）；Harbor 组件自举有"镜像不在自己里"死锁，预热办法见 harbor/values.yaml 头注释。

### 存储
- **默认 StorageClass = `longhorn`**（3 节点盘池：rtl-control/compute1/rtl-l20；burst6 经 `node.longhorn.io/create-default-disk` 标签刻意排除）。local-path 仅服务未迁移的存量卷。
- 部署资产：`longhorn/values.yaml`（tolerations/标签门控已固化在 values 里）。
- **PVC 迁移状态**：11 个生产卷已迁（weknora×3、harbor×4、memory openviking、aep rustfs/postgres、logging ES），旧 local-path 卷全部保留做回滚；DE helper-home ×6 因 operator 渲染硬编码 PVC 名暂缓（需 operator 支持可配），`digital-employees/pg-backups` 与两个孤儿卷待处置。

### 铁律（踩坑铸成）
1. **挂 RWO PVC 的 Deployment 一律 `strategy: Recreate`**——RollingUpdate 会同节点双挂同一卷导致双写损坏（2026-10-06 aep postgres 实际事故，pg_resetwal 救援）。
2. STS 换后端卷的手册：孤删 → 改 volumeClaimTemplates 名 + **同步改 mounts 同名** + storageClassName → 预建 `<模板名>-<sts>-<序数>` 卷并灌数 → 重放。命名推导绝不能错（harbor-database 踩过）。
3. 一切含密文件不入库；harbor admin 初始密码在控制节点 `/root/harbor-admin-password.txt`。

# aep-deerflow-governance

AEP 数字员工治理层 —— DeerFlow 运行时上的身份、数据范围与工具强制中间件，以及 k3s/K8s 部署清单。

DeerFlow 是执行引擎，治理（企业身份、按请求者数据范围、fail-closed 语义）由本包以
DeerFlow 官方 `extensions.middlewares` 扩展点注入，**不改 DeerFlow 源码**。迁移自
picoclaw 时代的 aepgate 资产（语义 1:1：活查、缓存、fail-closed、结构性拒绝）。

## 组件

| 文件 | 职责 |
|---|---|
| `aep_deerflow_governance/__init__.py` | `AepGovernanceMiddleware`（langchain AgentMiddleware） |
| `k3s/deployment.yaml` | 部署清单**模板**（占位符，见下） |
| `k3s/deployment.local.yaml` | 真实部署（gitignored，含密钥） |

## 中间件行为

挂钩：`wrap_model_call`（上下文注入）+ `wrap_tool_call`（结构性强制），lead 与
subagent 两条中间件链同时生效。

身份解析链（fail-closed）：

1. **IM 消息**：run context 的 `channel_user_id`（平台 open_id/userid）→ AEP
   identity mappings（`GET /aep/v1/admin/identity-sources/{id}/mappings`，缓存
   `scope_cache_seconds`，解析失败=零可见）→ AEP 用户；
   未映射 → 工具层 DENIED + "账号尚未绑定企业身份" 提示。
2. **直连 API**：DeerFlow 用户 → `user_map` 映射 → AEP 用户；
   兜底 `default_aep_user`（每实例一个，生产多用户应开 DeerFlow 认证 + user_map）。

范围：`GET /aep/v1/admin/data-scope/context?userId=` 活查（`orgScope`）+ 缓存 +
fail-closed；受管工具（`governed_tools`）调用范围外部门**不执行**，返回显式拒绝。

## 配置（config.yaml）

```yaml
extensions:
  middlewares:
    - class: aep_deerflow_governance:AepGovernanceMiddleware
      kwargs:
        aep_base_url: http://<AEP>:8080
        deployment_id: demo
        service_username: <agent-account>      # 需 data_scope.read (+identity.read 启用 IM 桥)
        service_password: <password>
        default_aep_user: <aep-user-id>
        identity_source_id: <source-id>        # 可选：启用 IM 身份桥
        governed_tools: [benchdept__dept_revenue, ...]
        team_names: {<team-id>: <部门名>}       # 范围 id → 工具参数名映射
```

## 镜像构建（离线路径，网络受限环境）

宿主 venv 平移（详见构建脚本思路）：

```
context/
  Dockerfile        # FROM debian:12-slim；COPY py-runtime(宿主 uv python) + backend(.venv) + 本包
  python - 修正 .pth 绝对路径 / pyvenv.cfg home / CMD PYTHONPATH
```

要点：`.venv` 的 `.pth` 内是宿主绝对路径，镜像内必须 `sed` 重写；MCP 工具服务器用
纯 Python（无 node 依赖）。**治理包任何改动都必须重建镜像**——配置 kwargs 传给旧
镜像里的中间件会构造失败，表现为每回合 "(No response from agent)"（静默）。

## k3s 部署

`k3s/deployment.yaml` 是模板，替换 `${...}` 后 apply：

```
NODE_IP（宿主 IP，AEP forwarder 与 mock 的可达地址）
GLM_GATEWAY / GLM_API_KEY（模型网关）
GOV_SERVICE_PASSWORD / GOV_DEFAULT_AEP_USER_ID
```

镜像来自内网仓 `${REGISTRY}/deerflow-gov:v2`；k3s 侧
`/etc/rancher/k3s/registries.yaml` 需为该仓库配 http endpoint 镜像与 auth。
真实部署落 `deployment.local.yaml`（gitignored）。

## 验收基准

- G1-G4（直连：允许/跨部门拒绝/异用户允许/质量）
- P1-P3（IM：映射+范围内 / 映射+范围外 / 未映射 fail-closed）
- 对话端 adapter 五用例（chat-runtime-adapter 仓）

## 文档

平台全量文档（产品设计、白皮书、需求、技术方案、架构图、API、实施、开发指南）在私有文档仓：
**[rongxinzy/zhiyuan-docs](https://github.com/rongxinzy/zhiyuan-docs)** —— 部署细节以其中
[07-deployment/k3s-bringup.md](https://github.com/rongxinzy/zhiyuan-docs/blob/main/07-deployment/k3s-bringup.md)
为准；本仓 DEVELOPMENT.md 保留为运维快查。

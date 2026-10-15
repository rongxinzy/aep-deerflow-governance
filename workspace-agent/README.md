# workspace-agent（P1 节点代理）

在一台 KVM 宿主上监督 PersonalWorkspace `runtime: microvm` 的 Firecracker
microVM。架构与 runbook 见 zhiyuan-docs `guides/microvm-runtime.md`（随 E2E 试点
补齐）；本 README 只记模块内事实。

## 职责边界（单写者约定）

- **operator 写一次**：`status.nodeName` / `status.vmSubnet`（sticky）。
- **agent 写**：`status.phase` / `status.gatewayEndpoint` / `status.vm{hostPort,
  configHash}`（microvm 路径）+ `digitalemployee.zhiyuan.io/workspace-node-agent`
  finalizer。
- 两者并行删除：agent 拆 VM/tap/链/盘/目录，operator 清 OpenViking 用户。

## 模块地图

| 包 | 职责 |
|---|---|
| `cmd/workspace-agent` | 入口：flags+Downward env（NODE_NAME/HOST_IP）、模板校验、10s resync 循环、`--metrics-addr`（默认 :9091，env METRICS_ADDR）上的 promhttp /metrics；直连 client（无 informer cache，对象个位数） |
| `internal/render` | config 盘 payload（companions.json 逐字节取自 ConfigMap + env（含 memkey 逐搭档投递，0600）+ resolv.conf→kube-dns）与 hash（config 滚动身份）；/30 端点与 GuestMAC 派生 |
| `internal/alloc` | host port 分配（8800-8899，CR 记录 + bind 探测） |
| `internal/hostcmd` | `chroot /host` 执行节点自身的 ip/iptables/mkfs.ext4（ko 静态镜像不带用户land；与 kube-proxy 同一 iptables 后端） |
| `internal/tap` | tap-pw-<ws> 生命周期 + per-subnet MASQUERADE |
| `internal/iptables` | E3b 白名单链 FC_WL_<ws>：scratch 链先建后换（部分渲染不会临时放宽）、FORWARD 跳入、端点变更 resync 跟随 |
| `internal/disk` | data 盘（建一次永不重建）与 config 盘（`mkfs.ext4 -d` 免挂载、tmp+rename 原子写；**仅 VM 停机时写**——PoC 热写损坏教训，状态机 guard） |
| `internal/template` | 模板 SHA256SUMS 校验 + keep-2 GC |
| `internal/vm` | Firecracker v1.17 UDS 驱动（golden PUT 序列、CtrlAltDel→SIGKILL 停机、serial rotate） |
| `internal/proxy` | userspace TCP proxy（host port → guest:8080；SSE 透明；零 iptables 入站态） |
| `internal/manager` | per-workspace 状态机 + Resync（含孤儿拆除、backoff 5/10/20/40/60、连续 5 败 give-up）；give-up 后每小时自愈重试 1 次、Running 稳定 1 小时清零 bootFails；每轮 Resync（无论成败）刷新 `<workdir>/run/agent.heartbeat` 供 liveness 探针；私有 registry 的 /metrics（phase/bootFails/resync 错误/VM 数） |

## 测试纪律

`testing+testify` 基线；接口 mock 用 uber-go/mock（mockgen 生成，见
`internal/vm/mocks`、`internal/disk/mocks`）；函数打桩用 bytedance/mockey
（healthz 探针），**必须** `go test -gcflags="all=-l" ./...`。

## 出镜像

```bash
KO_DOCKER_REPO=harbor.zhiyuan.local:30443/zhiyuan ko build \
  ./cmd/workspace-agent -t pw-node-agent:0.1.0
# Harbor CA 不被 ko 信任时：--tarball + crane push --insecure
```

DaemonSet 形态（chart `workspaceAgent` 值块）：privileged + runAsUser 0（覆盖
ko 默认 65532——需要节点 root 能力，chart PR 说明里已注明偏离）。

## 云电脑配置与模板（2026-10-15）

Portal 创建 PersonalWorkspace 可传 `runtime: microvm`、`desktop: {enabled: true}`、
`resources: {vcpus: 2, memory: 4Gi, disk: 20Gi}`、`templateTag`。CPU/内存、桌面开关与模板
通过既有 workspace PATCH 修改，owner/admin 权限与 CSRF 沿用原链路；不支持在原工作区切换
runtime 或调整已有盘容量。桌面至少 2 vCPU / 4 GiB，API 资源上限为 8 vCPU / 32 GiB / 100 GiB。
旧 pod 工作区不会自动迁移或打开桌面，新 BotUI 创建的工作区使用 microvm。

节点使用指定的已物化模板，空 tag 使用 `--template-tag`。新模板必须有 checksum 覆盖的
`template.json`（tag 和 desktop 能力）；缺失、不匹配或不支持桌面的模板在停止现有 VM 前被拒绝。
模板与机器资源参与 configHash，更新时先停 VM 再重写 config 盘，始终保留 data 盘；运行状态
在 `status.vm` 回报实际模板、配置资源和桌面开关。模板已允许固定版本，因此关闭启动时无引用
检查的 keep-2 清理，旧版本由运维确认无引用后删除。

发布顺序：先用 pi-durable-bench 新构建脚本生成并校验不可变桌面模板，再分发模板与更新
CRD/operator/portal/workspace-agent（CRD 与类型客户端一起发布），最后启用用户配置与新版 UI。
本轮只完成代码、隔离 FC 和本地真实 Portal/BFF/UI 验证，没有执行该发布流程。

本轮节点测试覆盖开关 env、资源/模板重建、旧数据盘保留与缺失模板拒绝。真实 FC 使用生产
init 和三盘挂载验证文件、暂停任务与持久 Cookie 在重建后保留；机器从 2 核/4 GiB 调至
4 核/8 GiB。前端验收使用真实 Portal mux、session/CSRF/proxy、BFF、UI 与 FC，只有 AEP 和
Kubernetes 使用 fixture；不代表线上控制器调度与模板分发已经验收。

```sh
# Portal 目录；gateway 必须是隔离测试 FC，脚本与浏览器由调用者准备。
WORKPLACE_ACCEPTANCE_GATEWAY=http://127.0.0.1:19080 \
WORKPLACE_ACCEPTANCE_SCRIPT=/path/to/zhiyuanBotUI/verify/workplace-live.mjs \
go test -count=1 -run '^TestWorkplaceLiveBrowser$' -v ./...
```

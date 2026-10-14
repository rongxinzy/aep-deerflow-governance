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
| `cmd/workspace-agent` | 入口：flags+Downward env（NODE_NAME/HOST_IP）、模板校验、10s resync 循环；直连 client（无 informer cache，对象个位数） |
| `internal/render` | config 盘 payload（companions.json 逐字节取自 ConfigMap + env（含 memkey 逐搭档投递，0600）+ resolv.conf→kube-dns）与 hash（config 滚动身份）；/30 端点与 GuestMAC 派生 |
| `internal/alloc` | host port 分配（8800-8899，CR 记录 + bind 探测） |
| `internal/hostcmd` | `chroot /host` 执行节点自身的 ip/iptables/mkfs.ext4（ko 静态镜像不带用户land；与 kube-proxy 同一 iptables 后端） |
| `internal/tap` | tap-pw-<ws> 生命周期 + per-subnet MASQUERADE |
| `internal/iptables` | E3b 白名单链 FC_WL_<ws>：scratch 链先建后换（部分渲染不会临时放宽）、FORWARD 跳入、端点变更 resync 跟随 |
| `internal/disk` | data 盘（建一次永不重建）与 config 盘（`mkfs.ext4 -d` 免挂载、tmp+rename 原子写；**仅 VM 停机时写**——PoC 热写损坏教训，状态机 guard） |
| `internal/template` | 模板 SHA256SUMS 校验 + keep-2 GC |
| `internal/vm` | Firecracker v1.17 UDS 驱动（golden PUT 序列、CtrlAltDel→SIGKILL 停机、serial rotate） |
| `internal/proxy` | userspace TCP proxy（host port → guest:8080；SSE 透明；零 iptables 入站态） |
| `internal/manager` | per-workspace 状态机 + Resync（含孤儿拆除、backoff 5/10/20/40/60、连续 5 败 Failed） |

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

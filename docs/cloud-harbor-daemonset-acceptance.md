# Harbor → DaemonSet → 云电脑接管隔离验收

本轮补齐[上一轮验收](cloud-control-plane-acceptance.md)未覆盖的真实 kubelet 调度、
Harbor 私有模板拉取和容器内节点代理执行。前端沿用现有视觉、头像和验收脚本。

## 隔离边界

- 经维护者明确授权升级共享 PersonalWorkspace CRD。结构对比仅新增
  `spec.desktop`、`spec.templateTag` 和三个 `status.vm` 已应用配置字段，
  不删除或改变原有字段。CRD 的升级与应用发布分开记录。
- 独立命名空间 `cloud-qa-ds`，工作区 `cloud-qa`；标签
  `zhiyuan.io/acceptance=harbor-daemonset` 标识可销毁的验收资源。
- DaemonSet 仅选择 `kubernetes.io/hostname=rtl-l20`；独立 hostPath
  `/tmp/cloud-harbor-qa-20261015/runtime`、18800–18809 端口、192.168.241.0/24
  工作区网段。本轮应用清单不修改生产代理或工作区配置，也不占用生产 8800 端口。
- AEP/PostgreSQL/对象存储使用一次性实例；Portal 和 Operator 使用生产源码构建，
  指向测试 AEP，Operator 仅观察 `cloud-qa-ds`。没有使用生产用户会话或模型凭据。
- Harbor 使用私有测试项目 `cloud-qa-20261015`。推送与拉取采用不同的一日有效机器人；
  Pod 的 `cloud-qa-harbor-pull` Secret 只有该项目的 repository pull 权限。
  TLS 使用现有 Harbor CA，不关闭证书校验。

## 可复用入口

Chart 新增 `workspaceAgent.imagePullSecrets` 与 `imagePullPolicy`，同时作用于
节点代理和模板 initContainer。默认仍为无拉取 Secret、`IfNotPresent`；
验收显式使用 `Always`，记录实际拉取事件和镜像摘要。

跨命名空间 endpoints 只读 ClusterRole/Binding 的名称增加命名空间后缀，
防止测试 release 覆盖另一命名空间的 ServiceAccount 绑定。
以后升级生产 Chart 时会创建新名称的绑定；旧固定名称绑定不会被本次隔离验收删除。

示例仅展示节点代理值，不应把整个平台 Chart 安装进测试命名空间：

```yaml
global:
  registry: harbor.zhiyuan.local:30443
  namespaces:
    main: cloud-qa-ds
aepStack: {enabled: false}
weknora: {enabled: false}
workspaceAgent:
  enabled: true
  imagePullSecrets: [{name: cloud-qa-harbor-pull}]
  imagePullPolicy: Always
  image: {repository: cloud-qa-20261015/node-agent, tag: qa-20261015-chroot}
  templateImage: {repository: cloud-qa-20261015/rootfs, tag: qa-harbor-v3}
  additionalTemplates:
    - {repository: cloud-qa-20261015/rootfs, tag: qa-harbor-v4}
  nodeSelector:
    zhiyuan.io/kvm: null
    kubernetes.io/hostname: rtl-l20
  workdirRoot: /tmp/cloud-harbor-qa-20261015/runtime
  portRange: 18800-18809
  resources:
    requests: {cpu: "2", memory: 10Gi}
    limits: {cpu: "6", memory: 12Gi}
```

```sh
helm template cloud-qa-ds charts/zhiyuan-stack -n cloud-qa-ds -f qa-values.yaml \
  --show-only templates/workspace-agent.yaml \
  --show-only templates/workspace-agent-rbac.yaml > qa-agent.yaml
# 人工核对资源均属于测试命名空间或其专属 endpoints 只读角色后应用。
kubectl apply -f qa-agent.yaml

# AEP 地址固定为控制主机 loopback 18080；口令仅由环境提供。
python3 scripts/seed-cloud-control-qa.py --shared-cluster-acceptance \
  --namespace cloud-qa-ds --template qa-harbor-v3
python3 scripts/verify-workspace-daemonset.py --namespace cloud-qa-ds \
  --node rtl-l20 --registry-prefix harbor.zhiyuan.local:30443/cloud-qa-20261015 \
  --registry-digests /tmp/harbor-digests.json --output /tmp/daemonset-evidence.json
```

shared-cluster 模式只写入有专门标签的 `cloud-qa-*` 命名空间，不创建或修改
共享 DNS、Higress Service/Endpoints。未提供此模式时，原脚本仍要求独立 API 16443。

前端运行 zhiyuanBotUI 的 `verify/workplace-control-plane.mjs` 和
`verify/workplace-live.mjs`，将 AEP/Portal 转发到本地，显式设置
`WORKPLACE_ACCEPTANCE_TEMPLATE=qa-harbor-v3`、
`WORKPLACE_ACCEPTANCE_NEXT_TEMPLATE=qa-harbor-v4` 和本次 TAP 网关上的测试网页地址。
保留前一轮约定的 `proof.txt`，覆盖持续任务、持久 Cookie、文件及配置重建。

## 实际发现并处理的分发与执行问题

1. Docker 的新式 `save` 归档含 OCI layout 和压缩层。把它当旧式 Docker tar
   传给 `crane push` 会重复压缩层，真实 kubelet 报 `archive/tar: invalid tar header`。
   若归档包含 `oci-layout`，先解包为目录，再执行 `crane push <目录> <镜像> --index`。
   失败的 v1/v2 不算验收通过；修正后发布新的 v3/v4，不覆盖已发布 tag。
2. ko 静态基础镜像没有 `chroot` 外部命令。原代理在 Pod 内创建 TAP 时失败。
   现在由 Go 的子进程属性设置 chroot，并从挂载的宿主目录解析可执行文件；
   保留绝对符号链接，让 iptables alternatives 在子进程的宿主根目录中解析。
   代理主进程不改变根目录，不向静态镜像加入 shell 或宿主工具。
3. Firecracker 与代理属于同一容器 cgroup。原先按 Go 进程配置的 512 MiB
   limit 导致桌面工作区 OOM，Pod Ready 也无法代表稳定可用。默认不再设置
   这个小限额，实际节点级 limits 必须包含全部 guest 与代理开销；本轮测试
   显式预留 10 GiB、限制 12 GiB/6 CPU。跨工作区的节点容量准入仍属后续范围，
   不能把 VM 内存配置视为 Kubernetes 自动完成的容量调度。

## 验收记录与清理

最终验收需要同时具备：无凭据拉取失败、实际调度到目标节点、所有镜像的
Pulled 事件与解析摘要、两个 initContainer 成功退出、模板 SHA256SUMS 校验、
工作区真实 Running，以及前端配置重建和桌面/浏览器接管成功。
不能把 Pod Ready 单独当作云电脑可用。

先删除测试 PersonalWorkspace，等待代理 finalizer 清理 VM、TAP、转发规则；
再删除测试 DaemonSet/命名空间及其专属 ClusterRole/Binding，停止记录 PID 的
测试服务、删除一次性容器、关闭转发，最后删除测试 Harbor 项目、机器人及临时凭据。
保留已授权升级的兼容 CRD，不回滚共享资源。验收产物不包括真实密码、token 或 kubeconfig。

## 2026-10-15 实测结果

| 验收项 | 结果 |
| --- | --- |
| 私有仓鉴权 | 无 imagePullSecret 的新 Pod 拉取失败；配置最小 pull 凭据后成功 |
| 实际调度 | DaemonSet 在 rtl-l20 上 1/1 Ready，最终 Pod 全程 0 次重启 |
| 分发 | v3/v4 两个 initContainer 均退出 0；模板校验通过；3 个镜像的 Pulled 事件与 Harbor API 摘要一致 |
| 配置闭环 | v3/2 CPU/4 GiB → v4/4 CPU/8 GiB → 关闭桌面并恢复 2 CPU/4 GiB → v3 并开启桌面 |
| 生效判定 | 每步核对实际 VM 资源、模板、桌面状态和新的 guest 启动时间 |
| 持久化 | 原暂停任务、登录 Cookie、proof.txt 均保留；数据盘 inode 2097354、大小 2147483648 字节不变 |
| 前端 | 桌面及浏览器查看、取得控制、导航/输入、归还均成功；页面异常和 console error 均为空 |
| 视觉 | 1440×1000 桌面与 390×844 移动布局检查通过；现有视觉与头像未修改 |
| 自动检查 | workspace-agent 全套 Go 测试及 vet、3 项 Chart 合约测试、UI 13 项测试和构建通过 |

前端验收第一次运行遇到环境请求更新配置版本、表单重新挂载导致定位失败；
保留失败证据后，脚本等待请求完成并用 locator.fill 填写，重置测试工作区并重新跑完
全部配置循环。不能用第一次已完成的单步重建替代完整循环通过。

最终镜像摘要：

- node-agent:qa-20261015-chroot：
  sha256:25cc166dd3b090f4e6033d12b5c0061ff555d126d6e5461c412b513f73a97f71
- rootfs:qa-harbor-v3：
  sha256:e813eb8f17fb602aa372d5a4aecf7f401be7aa75da7630705f6c7894914bbf9e
- rootfs:qa-harbor-v4：
  sha256:47aebfae32f43cb0ba4bb93995dc2e1f8cab4fbba0c2c69b3e82b23a333c22d1

验收证据保存在工作区 outputs/harbor-daemonset-2026-10-15，
包含 daemonset-evidence.json、harbor-digests.json、control-plane.json、
live-ui.log 和 UI 截图。它们是本地验收产物，不是公共仓库附件。
临时 Harbor 项目及镜像已删除，以上摘要用于追溯本轮证据，并非可继续拉取的发布版本。

清理完成：先通过 finalizer 删除测试 VM，再删除测试命名空间和专属集群 RBAC；
停止 AEP/Portal/Operator/网页服务，删除一次性数据库和对象存储容器、测试镜像引用、
Harbor 项目和两个机器人、临时凭据与 kubeconfig，关闭本地转发。保留授权升级的 CRD。

生产工作区 spec 前后比对一致，清理后生产 8800 健康检查返回 200。
验收期间同时观察到生产 DaemonSet 的 Helm managedFields 时间为 05:17:09 UTC，
其 Pod 与生产 guest 随后重新启动；本轮隔离应用未发布生产 DaemonSet，
但现有证据不能证明生产进程全程未重启，也不能仅凭 managedFields 归因到具体操作者。
这项观察与本轮测试资源分开记录，不把健康检查解释为无中断证明。

本轮不包含模型推理、OpenViking、自助开户、跨节点迁移或 Windows/ClearType 实机验证。
本次合并不代表生产应用部署，也未实现跨工作区的节点容量准入。

# 云电脑真实控制面隔离验收

本验收使用真实 AEP 服务与 PostgreSQL、独立 Kubernetes API、生产代码的
Portal / Operator / workspace-agent，以及真实 Firecracker 和浏览器/桌面。
不连接生产 Kubernetes API，不升级共享 CRD，不部署生产镜像。

## 隔离拓扑

- jump：独立 Docker 容器 `cloud-control-qa-k3s`、`cloud-control-qa-postgres`、
  `cloud-control-qa-s3`；Kubernetes 仅发布 `127.0.0.1:16443`，AEP/Portal
  分别监听 `127.0.0.1:18080/18090`。数据库分别为 `aep`、`portal`。
- K3s 容器使用自己的数据目录和默认网络，`--disable-agent`，不接管宿主机
  kubelet、CNI 或生产节点。仅向该 API 安装当前 CRD；Operator 只看 `cloud-qa`。
- L20：节点代理通过 SSH 隧道连接上述 API；节点标识 `cloud-qa-l20`，
  独立目录 `/tmp/cloud-control-qa-20261015/runtime`、端口 `18800-18809`、
  子网 `192.168.241.0/24`。生产目录、8800 端口和 192.168.77.0/24 不变。
- 前端通过本地 SSH 转发访问真实 AEP/Portal；登录表单建立真实会话，写请求走 CSRF。

`scripts/seed-cloud-control-qa.py` 只接受 loopback 16443 API，登录 AEP 后
把真实用户 ID 写入测试工作区。密码从环境读取，不输出 token。这里直接预置
工作区，**不宣称覆盖自助创建和 OpenViking 开通**。记忆、模型推理不在本轮范围内，
使用独立 companion 内部身份和本地网页夹具，不复制生产凭据。

## 模板分发

`workspaceAgent.templateImage` 为默认版本；`additionalTemplates` 列出允许切换的
其他版本，每项使用相同的 `repository`、`tag` 形状。节点代理启动前依次运行
各版本的 initContainer，随后 PersonalWorkspace `spec.templateTag` 可选择任一已准备版本。

共用脚本 `charts/zhiyuan-stack/files/materialize-template.sh` 在镜像内校验源 payload，
复制到临时目录后复验，再原子发布。已有目录必须与源版本清单一致且内容校验通过，
否则明确失败，不覆盖正在被工作区引用的模板。不要复用同一个 tag 发布不同内容。
旧版本仍由显式保留策略管理，不在启动时自动删除。

本轮用两版实际模板镜像执行同一物化脚本，再由真实节点代理选择和启动。
**K3s 未运行 kubelet，因此不包含 DaemonSet 调度和生产 Harbor 拉取验收**；
Helm 渲染/校验与镜像内执行分别验证，不把它们表述为生产部署成功。

## 验收入口

1. 独立控制面 ready 后运行 `seed-cloud-control-qa.py`（显式设置测试密码和 KUBECONFIG）。
2. 准备两版 desktop 模板 `qa-v1`、`qa-v2`，物化到独立 runtime/templates。
   两版复用相同运行时代码，只更改受校验的版本元数据，测试的是版本选择和回退。
3. 首次启动前在独立 data.ext4 的 `workspace/proof.txt` 写入
   `persistent cloud workspace 2026-10-15` 加换行；启动真实节点代理和测试网站。
4. 在 zhiyuanBotUI 仓库构建 web，然后运行：

   ```sh
   WORKPLACE_ACCEPTANCE_REAL=isolated \
   AEP_BASE_URL=http://127.0.0.1:28080 \
   PORTAL_BASE_URL=http://127.0.0.1:28090 \
   WORKPLACE_ACCEPTANCE_SITE=http://192.168.241.1:18131 \
   node verify/workplace-control-plane.mjs
   ```

   `WORKPLACE_ACCEPTANCE_PASSWORD` 由调用环境提供；默认用户名 admin。
   脚本等待真实 applied 配置和 guest 启动时间变化，不以 PATCH 200 作为配置生效证据。
5. 配置循环完成后，以相同地址、密码和 `WORKPLACE_SKIP_CONFIGURATION=1`
   运行 `verify/workplace-live.mjs`，覆盖浏览器和桌面的查看、接管、输入、归还，
   并保留桌面、移动宽度、深色模式截图。

配置循环：2 核/4 GiB/qa-v1 → 4 核/8 GiB/qa-v2 → 2 核/4 GiB/关闭桌面
→ 2 核/4 GiB/qa-v1/恢复桌面。每次检查运行时资源与工作文件；启用桌面时
检查持久 Cookie；所有重建后检查同一持续任务仍暂停。

## 清理与边界

先经隔离 API 删除测试工作区，等待节点代理 finalizer 清理 VMM、TAP、代理与规则，
然后停止此次记录 PID 的进程、删除命名的 QA 容器和临时目录、关闭本轮隧道。
不要使用全局 prune、清空 FORWARD 或按模糊名字终止进程。

本轮不验证模型回答质量、Windows 原生显示、生产升级、跨节点迁移或数据盘扩容。
模板切换通过现有 stop/rebuild 路径完成；内核不响应 CtrlAltDel 时仍使用现有
超时强制停止，因此持久化通过不代表桌面应用未保存内容能自动保留。

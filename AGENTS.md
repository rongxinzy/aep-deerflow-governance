# 代理协作指引

## 出镜像

改完 Go 组件（`operator/`、`portal/`）要出镜像时，**优先用 ko（https://ko.build），不新增、不修改 Dockerfile**：

- 只出镜像：`KO_DOCKER_REPO=<内网仓> ko build ./cmd/main.go`（不需要 Docker 守护进程）。
- 出镜像并落到集群：清单里写 `image: ko://<module-path>/cmd/main.go`，再 `ko apply -f k3s/`。
- 依赖拉取：`export GOPROXY=<内网 goproxy>,direct`。
- 保持非 root 与 CA 信任：`defaultBaseImage` 用 distroless / chainguard static（自带 CA，`USER 65532`）。

**不适用（重要）**：本仓的 `channel-gateway/`、`k3s/Dockerfile.deerflow-gov`、`k3s/Dockerfile.deerflow-gov-governance` 是 Python 运行时镜像，代码在镜像内的 `app/` 里——**ko 只构建 Go**，这些镜像仍按原方式重建（只重建刷新层），dev 下可用挂载覆盖。不要拿 ko 去改它们。

验收：构建后确认 entrypoint 与 `USER` 与旧镜像一致，再 apply 到本机 dev k3s 跑起来。

> 性能数字（ko vs docker build）待实测，暂不写入。同一段落亦见于 `Agent-Enterprise-Protocol`，改动请同步两处。

## 跨仓协作规范

提交、PR 标题与说明、review、bug fix 验证遵循 [DEVOPS.md](DEVOPS.md)。本仓已有专项安全、设计与发布门继续执行。

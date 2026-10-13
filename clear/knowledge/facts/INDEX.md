# Established knowledge (entries you can cite as known; older unstructured entries stay listed)

> Each entry carries its **scope**, **refutation condition** and **support level**: before citing it, check that this situation is within its scope.
> This file is maintained by the system (the worker cannot write `clear/knowledge/facts`); newest first.

## f-goad3t · 删除 listeners/anonymous_root 与 namespaces.default 后，服务仍能用单一主端点启动：显式 namespace 路径、推送、_catalog、/readyz 全部照旧。

- Scope: conditions 上游=docker.1ms.run,registry-1.docker.io,ghcr.io,gcr.io,quay.io,registry.k8s.io, 主机=192.168.188.103, 出网=局域网代理 192.168.188.1:3128, 客户端=docker/containerd 直连 103, 部署方式=docker compose + 单个主端点 :5000
- Refuted if: 启动失败（严格解码报错、无监听器）、或 e2e 里 login/push/catalog/degraded 任一条不再通过。
- Supported to: L3 · evidence: e-k4tehi, e-3wd5c9
- From goal: g-mv5eqq0n96w2 · promoted at: 2026-10-13T01:19:58.588Z

## f-bqg40o · 所有候选都 404 时返回 registry v2 的 NAME_UNKNOWN 404，而不是 503 或回落某个默认仓库。

- Scope: conditions 上游=docker.1ms.run,registry-1.docker.io,ghcr.io,gcr.io,quay.io,registry.k8s.io, 主机=192.168.188.103, 出网=局域网代理 192.168.188.1:3128, 客户端=docker/containerd 直连 103, 部署方式=docker compose + 单个主端点 :5000
- Refuted if: 返回 200（命中了不该命中的 namespace）、5xx，或错误码不是 NAME_UNKNOWN。
- Supported to: L3 · evidence: e-bt173f, e-4tpmou
- From goal: g-mv5eqq0n96w2 · promoted at: 2026-10-13T01:19:58.588Z

## f-3gzno8 · 对候选的探测是并发发出的，决策等待时间不随候选数线性增长。

- Scope: conditions 上游=docker.1ms.run,registry-1.docker.io,ghcr.io,gcr.io,quay.io,registry.k8s.io, 主机=192.168.188.103, 出网=局域网代理 192.168.188.1:3128, 客户端=docker/containerd 直连 103, 部署方式=docker compose + 单个主端点 :5000
- Refuted if: 实测两个上游收到请求的时间差与串行实现一致（差值 ≥ 第一个候选的响应延迟）。
- Supported to: L3 · evidence: e-66xb6b, e-x19a6p
- From goal: g-mv5eqq0n96w2 · promoted at: 2026-10-13T01:19:58.588Z

## f-udxt0n · 无限定请求可以只靠并发探测各候选 namespace 上游的 /v2/<repo>/manifests/<ref>、只接受 2xx 并取最快者来选定服务方，无需 namespaces.default。

- Scope: conditions 上游=docker.1ms.run,registry-1.docker.io,ghcr.io,gcr.io,quay.io,registry.k8s.io, 主机=192.168.188.103, 出网=局域网代理 192.168.188.1:3128, 客户端=docker/containerd 直连 103, 部署方式=docker compose + 单个主端点 :5000
- Refuted if: 任一候选上游可用而探测失败/超时，或出现「有镜像的 namespace 未被选中」的情况（例如 token 流程不通、探测判据把 404 与错误混淆）。
- Supported to: L3 · evidence: e-zwjsdc, e-2k26ka
- From goal: g-mv5eqq0n96w2 · promoted at: 2026-10-13T01:19:58.588Z

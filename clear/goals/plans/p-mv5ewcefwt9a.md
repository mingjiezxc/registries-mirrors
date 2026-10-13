# 阶段归档 · p-mv5ewcefwt9a

- 收束时间:2026-10-13T01:20:40.882Z
- 所属目标:g-mv5eqq0n96w2
- 收束之辞:六步全部交付，目标判据 1–6 逐条有证据（4 条判断已经过独立核验并写入事实库：f-udxt0n、f-3gzno8、f-bqg40o、f-goad3t）。

交付内容：
- internal/selection 并发抉择器（只有一个 proxy namespace 时退化为一次探测；并发时间差实测 3–22µs，两候选各 150ms 时总耗时 151ms 而非 300ms；只认 2xx；401 走 Bearer token 重试；决策按 probe_ttl 缓存 + 仓库亲和；cache-only 候选靠本地磁盘判定不发探测）。
- 路由与接线：删除 namespaces.default 与 listeners/anonymous_root，dispatcher 按选择结果转发（显式 namespace 前缀仍直连、不探测）；全部候选明确不存在 → 404 NAME_UNKNOWN，无人给出答复 → 503 UNAVAILABLE；GET/HEAD 失败重选一次；无仓库名的 blob 由亲和补回仓库段。
- 配置/文档/示例/e2e：单端点 5000 + 指标 9100，README 与 registries.yaml.example 改写为「无限定名 + :5000」与 hosts.toml 两条客户端路径。
- 103 上线：端口只剩 5000/9100，:5000/v2/ 仍 401，无限定已缓存镜像 200（命中数 36→37）、无仓库名 blob 200（source=affinity）、拼错镜像名 404 NAME_UNKNOWN（五候选全 absent）；备份与回滚步骤齐备。

过程中修掉 3 个真实缺陷（探测缺 manifest Accept、无限定 blob 丢仓库段、鉴权拒绝被误判为不可达）并做了 1 处口径裁决（多候选时「无人答复才 503」）。联调与核验阶段还纠正了多处读数转写错误——核验记录现在要求每组数字都能从落盘原文复算。

遗留：探测耗时随上游抖动（未命中缓存 2.5–7.5s，上限 10s）；ghcr/gcr 匿名 token 端点 403 导致「不存在 vs 存在但私有」不可区分；磁盘上有 blob 但进程内无仓库亲和时按 503 处理。

## 步骤
- [advanced] 1. 把三条并行支线的接口先钉死，防止各写一套：(1) 新增 internal/config/selection.go，定义 `Namespaces.Selection{ProbeTTL, ProbeTimeout}`（yaml: namespaces.selection.probe_ttl / probe_timeout）与默认值；(2) 新增 internal/selection/chooser.go 骨架：`Chooser`、`Candidate{Namespace,EnginePath,Repo}`、`Target`、`ErrNotFound`/`ErrUnreachable`、`New(Selector, Options)`、`func (c *Chooser) Decide(ctx, candidates, repo, ref) (Target, error)` 的签名与文档注释（实现先留 TODO）；(3) 记录当前基线：`go test ./...` 与 `go test -tags e2e -count=1 ./test/e2e/...` 的原始输出、`:5000` 与 `:15000/:15003/:15004` 的 curl 读数。 → internal/config/selection.go, internal/selection/chooser.go, reports/raw/selection-baseline.txt
- [advanced] 2. 实现 internal/selection 的并发探测与决策（写在 probe.go 与骨架 chooser.go 的实现体里，另加 chooser_test.go）：对每个候选并发 `HEAD <upstream>/v2/<repo>/manifests/<ref>`（User-Agent rms/selection-probe，per-probe ctx timeout = probe_timeout），401 时按 WWW-Authenticate 的 Bearer realm/service/scope 取 token（凭据来自候选的 ProxyNamespace）并重试一次，405/501 降级为 GET 并丢弃 body；只把 2xx 记为「存在」并记录耗时；在存在的候选里取最小耗时（并列按名字序），并按 repo/ref 缓存决策 TTL=probe_ttl（同时写 repo 级亲和供 blob 复用）；全 404 → ErrNotFound，全网络失败或无候选 → ErrUnreachable。同时给 registry.Manager 加 Serving()/Upstream()/UpstreamAuth()，给 upstream.Selector 加最近成功探测耗时（EWMA）。 → internal/selection/chooser_test.go, internal/selection/probe.go, internal/registry/manager.go, internal/upstream/selector.go
- [advanced] 3. 接线路由：删 config 的 Listeners/Listener/anonymous_root/Namespaces.Default 与相关校验和默认值；namespace.Table 删 defaultNS 与 Default()，route() 的 default 分支改成「待选」（Namespace 留空、Repo 保留客户端全名）；dispatcher 注入 chooser，无限定 KindRepository 请求先 Decide 再按结果设 Namespace/Mode/EnginePath 转发，ErrNotFound → 404 NAME_UNKNOWN、ErrUnreachable → 503 UNAVAILABLE，GET/HEAD 且引擎返回 404/5xx 时作废该决策重选一次；server.Handler 回到单参数、Listen() 只绑 server.listen；app 构造 Chooser 并统一 externalURLs；cmd 的 config 子命令去掉 default namespace 行。 → internal/config/config.go, internal/namespace/resolver.go, internal/server/dispatch.go, internal/app/app.go, cmd/rms/main.go
- [advanced] 4. 配置、文档与 e2e：config/rms.yaml 与 deploy/103/rms.yaml 删 default 与 listeners、加 namespaces.selection（probe_ttl/probe_timeout）；deploy/103/docker-compose.yml 端口只留 5000 与 9100，.env 删 RMS_PORT_DOCKERIO*/GCR/GHCR/K8S/QUAY；README 改写无限定请求一节、删端口绑定一节与 anonymous_root 说明、同步配置表与冲突表；registries.yaml.example 改成「镜像指 :5000，docker.io 可用无限定名，其它上游需带 namespace 或 hosts.toml 的 override_path」；test/e2e 的 startRMS/startRMSWithData 去 listeners/default，新增无限定选择的用例。 → config/rms.yaml, deploy/103/rms.yaml, deploy/103/docker-compose.yml, deploy/103/.env, README.md, config/registries.yaml.example, test/e2e/e2e_test.go
- [advanced] 5. 集成核验：跑 `make check`、`go test -race ./internal/selection/`、`go test -tags e2e -count=1 ./test/e2e/...`，逐条核对判据；用真实上游（docker.1ms.run / registry-1.docker.io / ghcr.io / gcr.io / quay.io / registry.k8s.io）实测一次并发探测：记录每个候选的探测耗时与两两开始时间差（并发压缩比），确认「存在但慢」「不存在」两类上游都被正确区分，并把读数写进报告。 → reports/selection-verification.md
- [advanced] 6. 103 上线（不可撤销，须人批准）：deploy/103/install.sh 同步 → 在 103 备份现有 config/rms.yaml 与客户端配置 → docker compose up -d --build → 核验端口只剩 5000/9100、:5000/v2/ 仍 401、无限定拉取一个已缓存镜像命中缓存、不存在的镜像返回 404 包络、候选全不可达场景返回 503，并采集 selection 决策日志里的逐候选探测耗时；把端口前后对照、客户端迁移清单（daemon.json registry-mirrors 指 5000；containerd 带 namespace 前缀或 hosts.toml override_path）与回滚步骤写进报告。 → reports/103-selection-rollout.md

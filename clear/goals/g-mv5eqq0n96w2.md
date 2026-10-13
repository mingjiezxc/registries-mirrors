# Goal g-mv5eqq0n96w2 (rev6 · achieved)
- **Headline**: 把 rms 无限定请求从「固定默认仓库」改成「并发挑出真有该镜像且最快的 namespace」，并删除按 namespace 绑定的独立端口
- **Claim**: rms 的无限定（不带 namespace）请求如何被路由：改前固定落到 namespaces.default，改后并发检查所有已配置 proxy namespace，选「该仓库真的存在且响应最快」的那个；全都不存在才报错。同时移除按 namespace 绑定的独立端口。
- **Promotion threshold**: L3
- **Criteria (full text)**:
  1. namespaces.default 与 listeners（含 anonymous_root）从配置结构体消失，严格解码下带这两个键的配置启动即报错
  2. 仓库与 103 在线配置里再无 15000–15006 端口映射与 namespace 绑定监听，只剩主端点 5000 与 9100
  3. internal/selection 的新单测断言两条硬性质：并发探测（两个上游收到请求的时间差 < 50ms）与只接受 2xx 作为「存在」
  4. go test ./... 与 go test -tags e2e -count=1 ./test/e2e/... 全绿；新增 e2e 证明 A 有镜像 B 没有时无限定请求命中 A，两边都没有的 tag 返回 404
  5. go test -race ./internal/selection/ 无数据竞争
  6. 103 上线后 docker ps 映射端口只有 5000/9100，:5000/v2/ 仍 401 挑战，无限定拉取命中 docker.io 缓存，不存在的镜像返回 404 包络，并采集到 selection 决策日志里的各候选探测耗时
> The system writes this file from the ledger (a projection); the authority is `goal/set` and `criteria/revised` in the ledger.
## Judgments (4)
- `h-gacxr2` [alive] 无限定请求可以只靠并发探测各候选 namespace 上游的 /v2/<repo>/manifests/<ref>、只接受 2xx 并取最快者来选定服务方，无需 namespaces.default。
  - Refuted if: 任一候选上游可用而探测失败/超时，或出现「有镜像的 namespace 未被选中」的情况（例如 token 流程不通、探测判据把 404 与错误混淆）。
  - scope: {"conditions":{"上游":"10 个 proxy namespace 及其上游（官方 + daocloud 镜像）","主机":"192.168.188.103","出网":"局域网代理 192.168.188.1:3128","客户端":"docker/containerd 直连 103","部署方式":"docker compose + 单个主端点 :5000"},"ranges":{}}
  - uses (framing declarations): [{"id":"x-h-nccqnt","kind":"negative","verdict":"out_of_scope"},{"id":"n-u-s01do2","kind":"negative","verdict":"out_of_scope"},{"id":"n-u-iiyp8u","kind":"negative","verdict":"out_of_scope"},{"id":"n-u-l29jwy","kind":"negative","verdict":"out_of_scope"},{"id":"n-u-fl1myg","kind":"negative","verdict":"out_of_scope"}]
  - Assertions: 1
- `h-0afup5` [alive] 对候选的探测是并发发出的，决策等待时间不随候选数线性增长。
  - Refuted if: 实测两个上游收到请求的时间差与串行实现一致（差值 ≥ 第一个候选的响应延迟）。
  - scope: {"conditions":{"上游":"10 个 proxy namespace 及其上游（官方 + daocloud 镜像）","主机":"192.168.188.103","出网":"局域网代理 192.168.188.1:3128","客户端":"docker/containerd 直连 103","部署方式":"docker compose + 单个主端点 :5000"},"ranges":{}}
  - uses (framing declarations): [{"id":"n-u-l136s0","kind":"negative","verdict":"out_of_scope"},{"id":"n-u-l29jwy","kind":"negative","verdict":"out_of_scope"}]
  - Assertions: 1
- `h-7yrf0w` [alive] 所有候选都 404 时返回 registry v2 的 NAME_UNKNOWN 404，而不是 503 或回落某个默认仓库。
  - Refuted if: 返回 200（命中了不该命中的 namespace）、5xx，或错误码不是 NAME_UNKNOWN。
  - scope: {"conditions":{"上游":"10 个 proxy namespace 及其上游（官方 + daocloud 镜像）","主机":"192.168.188.103","出网":"局域网代理 192.168.188.1:3128","客户端":"docker/containerd 直连 103","部署方式":"docker compose + 单个主端点 :5000"},"ranges":{}}
  - uses (framing declarations): [{"id":"x-h-nccqnt","kind":"negative","verdict":"out_of_scope"},{"id":"n-u-s01do2","kind":"negative","verdict":"out_of_scope"}]
  - Assertions: 1
- `h-63ooxn` [alive] 删除 listeners/anonymous_root 与 namespaces.default 后，服务仍能用单一主端点启动：显式 namespace 路径、推送、_catalog、/readyz 全部照旧。
  - Refuted if: 启动失败（严格解码报错、无监听器）、或 e2e 里 login/push/catalog/degraded 任一条不再通过。
  - scope: {"conditions":{"上游":"10 个 proxy namespace 及其上游（官方 + daocloud 镜像）","主机":"192.168.188.103","出网":"局域网代理 192.168.188.1:3128","客户端":"docker/containerd 直连 103","部署方式":"docker compose + 单个主端点 :5000"},"ranges":{}}
  - uses (framing declarations): [{"id":"x-h-nccqnt","kind":"negative","verdict":"out_of_scope"},{"id":"n-u-s01do2","kind":"negative","verdict":"out_of_scope"},{"id":"n-u-fl1myg","kind":"negative","verdict":"out_of_scope"}]
  - Assertions: 1

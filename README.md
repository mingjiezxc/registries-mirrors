# rms — 镜像代理 / 加速 / 缓存服务

**一个 Go 进程、一个内容端点，同时做三件事**：

1. **拉取加速与缓存** —— 把公共仓库按 namespace 代理并缓存到本地磁盘（`/data/<namespace>`）。
2. **私有仓库推送** —— 登录后把镜像推进自己的 namespace（或共享 namespace）。
3. **无限定请求自动选仓** —— 镜像名里不带 namespace 的请求，并发探测所有 proxy namespace，
   取**真有该镜像且响应最快**的那个。**没有默认仓库**。

镜像引擎复用 [`distribution/distribution/v3`](https://github.com/distribution/distribution)，rms 只做它
不提供的部分：namespace 路由、无限定请求的并发选仓、推送鉴权、上游并发与 digest 仲裁、HTTP 出网代理、
可观测性。

```text
客户端（docker / containerd / k3s）
  · registry-mirrors：裸 host:port，发无限定名
  · 显式镜像名：<host>:5000/<ns>/<repo>
  · hosts.toml + override_path：给某个上游做透明镜像
        │
        ▼
:5000  主端点（唯一内容入口）
        路径解析 → 选仓（并发探测）→ 上游竞速 → digest 仲裁 → 均衡池 → 鉴权 → 转发
        │
        ├─► docker.io         上游 docker.1ms.run · registry-1.docker.io · docker.m.daocloud.io
        ├─► ghcr.io           上游 ghcr.io · ghcr.m.daocloud.io
        ├─► gcr.io            上游 gcr.io · gcr.m.daocloud.io
        ├─► quay.io           上游 quay.io · quay.m.daocloud.io
        ├─► registry.k8s.io   上游 registry.k8s.io · k8s.m.daocloud.io
        └─► …                 每个 namespace 一个 distribution 引擎，可任意增删
        │
        ▼
本地缓存  /data/<namespace>/docker/registry/v2/...     层数据本地读；tag 每次回源校验

:9100  指标口  ← Prometheus 抓 /metrics 与 /registry/metrics（/debug/pprof 可选）
```

**并发一句话**：冷请求一次就向**所有**「namespace × 它的上游」并发探测，最快的那个真有内容的服务；
`probe_ttl` 窗口内不再探测。逐级展开见 [§4.8 并发模型](#48-并发模型)。

| 面 | 入口 | 内容 |
|---|---|---|
| 客户端 | `:5000` 主端点 | **唯一内容入口**：`/v2/`、`/v2/_catalog`、`/v2/<ns>/<repo>/...`、`/healthz`、`/readyz` |
| 观测 | `:9100` 指标口 | `/metrics`、`/registry/metrics`、`/debug/pprof/`（需 `observability.pprof`） |
| 磁盘 | — | 每 namespace 一个目录，distribution 原生布局：`/data/<namespace>/docker/registry/v2/...` |

> **只读本文档就能上手**：请求逻辑见 [§4](#4-请求生命周期)，全部配置键与默认值见 [§5](#5-配置参考)，
> 客户端接入见 [§6](#6-客户端接入)，部署与发布见 [§7](#7-部署)，历史决策与实测记录见[附录 B](#附录-b-变更史与实测记录)。

**目录**

- [1. 这是什么](#1-这是什么) · [2. 快速开始](#2-快速开始) · [3. 概念与术语](#3-概念与术语)
- [4. 请求生命周期](#4-请求生命周期)：[端点](#41-端点与端口) · [路径解析](#42-路径解析顺序) ·
  [选仓](#43-无限定请求的选仓) · [上游竞速与均衡](#44-上游并发digest-权威与负载均衡) ·
  [鉴权](#45-鉴权与授权) · [缓存与存储](#46-缓存与存储布局) · [降级与故障转移](#47-健康降级与故障转移) ·
  [并发模型](#48-并发模型)
- [5. 配置参考](#5-配置参考)：[加载与展开](#51-加载环境变量展开与严格解码) ·
  [server](#52-server) · [storage](#53-storage) · [auth](#54-auth) · [namespaces](#55-namespaces) ·
  [upstream](#56-upstream) · [observability](#57-observability) ·
  [校验规则](#58-校验规则清单) · [环境变量](#59-环境变量与-make-变量) · [三份配置](#510-三份配置的关系)
- [6. 客户端接入](#6-客户端接入) · [7. 部署](#7-部署) · [8. 运维](#8-运维) · [9. 开发](#9-开发)
- [附录 A 设计取舍与冲突汇总](#附录-a-设计取舍与冲突汇总) ·
  [附录 B 变更史与实测记录](#附录-b-变更史与实测记录) · [附录 C 未实现与已知遗留](#附录-c-未实现与已知遗留)

---

## 1. 这是什么

| 你想要的 | rms 怎么做 |
|---|---|
| 拉公共镜像更快、更省外网流量 | 按 namespace 代理上游并落盘缓存；层数据（体积大头）本地读 |
| 有地方推自己的镜像 | 可写 namespace（`namespaces.local`、`auth.shared_namespaces`、或同名用户 namespace） |
| 客户端只给一个 `host:port` 也能用 | 无限定请求（`library/alpine`）由 rms 自己并发探测、挑出真正有它的 proxy namespace |
| 上游多、有的快有的坏 | namespace 内所有上游**并发探测**，最快的服务；同 digest 的镜像按延迟加权分担 |
| 出网要过代理 / 只走内网镜像 | `upstream.http.proxy` + `no_proxy`，每个 namespace 还可单独覆盖 |

**边界（明确不做）**：Bearer token 服务端、限流/带宽控制、镜像白名单、S3 等对象存储后端、按容量/LRU
清理、namespace 配额。见[附录 C](#附录-c-未实现与已知遗留)。

---

## 2. 快速开始

```bash
cp .env.example .env          # 按部署宿主机调整出网代理（见 §3 与 §5.9）
make htpasswd USER=alice PASS=alicepw
make prepare                  # 建 ./data 并把权限调成容器用户（65532）可用
make up                       # docker compose up -d --build
make logs
```

`make up` 会构建镜像、把 `./data` 挂到 `/data`、把整个 `./config` 目录挂到 `/etc/rms`，并启动
`:5000`（主端点，唯一的内容入口）与 `:9100`（指标）。**没有按上游绑定的端口**。

> `config/htpasswd` 里是**演示凭据**（alice/bob/me/admin），上线前请用 `make htpasswd USER=... PASS=...`
> 替换或删掉。

> 配置目录是**整目录挂载**而不是逐文件挂载：`rms htpasswd add` 用「写临时文件再 rename」的方式原子替换
> htpasswd，而文件级 bind mount 会钉住原来的 inode，容器会一直读到旧文件，`kill -s HUP` 看起来毫无作用。

冒烟测试：

```bash
curl -i http://localhost:5000/v2/                     # 401 + WWW-Authenticate: Basic realm="rms"
curl -s http://localhost:5000/healthz                 # ok
curl -s http://localhost:5000/readyz                  # ready（有降级 namespace 时 503 并列出它们）
docker pull localhost:5000/docker.io/library/alpine   # 显式 namespace：直连该 namespace
docker pull localhost:5000/alpine                     # 无限定：并发探测后挑到 docker.io
docker login localhost:5000 -u alice                  # 认证（镜像名带 namespace 的显式拉取需要它）
docker tag alpine localhost:5000/alice/app:v1
docker push localhost:5000/alice/app:v1               # 推自己的 namespace
```

---

## 3. 概念与术语

| 术语 | 含义 |
|---|---|
| **namespace** | 缓存/私有仓库的隔离单位，也是磁盘目录名：`/data/<namespace>`。路径里写它就是「显式指定」。 |
| **proxy namespace** | 只读的 pull-through 缓存，键必须像注册表域名（`docker.io`、`ghcr.io`…）。写请求一律 `403`。 |
| **local namespace** | 可写命名空间。来源有三种：`namespaces.local`、`auth.shared_namespaces`、启用 `auth.user_namespace` 后每个 htpasswd 用户名。名字**不能**像域名。 |
| **无限定请求** | 路径首段既不是 proxy namespace 也不是 local namespace（`/v2/library/alpine/manifests/latest`）。rms 必须自己决定它属于哪个 namespace。 |
| **上游 / endpoint** | 一个 proxy namespace 下的一个远端注册表地址（`https://registry-1.docker.io`）。 |
| **竞速（racing）** | 把「namespace × 它的每个上游」当成并发候选一起探测，最快的服务。`upstream_race: false` 可退回有序选举。 |
| **digest 权威** | 同一 namespace 内，配置顺序里**第一个答出内容**的上游定义该 tag 的 digest；digest 与它不一致的镜像站判为过期，既不服务也不均衡。 |
| **均衡池** | 胜出 namespace 里 digest 一致、且延迟在 `balance_window` 倍数以内的上游；池内按 `balance` 模式分担流量。 |
| **决策缓存 / 亲和** | `probe_ttl` 窗口内，(仓库, tag/digest) 的选仓结果直接复用，不再探测。blob 请求路径里没有仓库名，按**仓库**复用 manifest 的决策 —— 这就是「仓库亲和」。 |
| **粘性引擎** | 一个 namespace 当前默认使用的引擎实例（有序选举模式下即「当选的上游」）；决策未落定时用它兜底。 |
| **降级 / cache-only** | 上游全不可达时，该 namespace 退化成只读缓存引擎：已缓存内容仍可拉，`/readyz` 报 503 并列出它。 |

---

## 4. 请求生命周期

一个请求依次经过：**端点判定 → 路径解析 → 选仓/选上游（仅 proxy）→ 鉴权 → 转发 → 失败时重选一次**。
完整的判定流程（标出每一步的并发宽度）：

```text
GET /v2/<path>   （客户端）
   │
   ├─ ① 端点判定              /healthz /readyz /v2/ /v2/_catalog 由 rms 自己回答，不进 namespace
   │
   ├─ ② 路径解析（namespace）  首段像域名？→ 该 proxy namespace（直连，不跨 ns 探测）
   │                          首段是 local？→ 可写 namespace（保留前缀）
   │                          都不是？      → 无限定请求，交给 ③
   │
   ├─ ③ 选仓（仅无限定请求）    ┌ 决策缓存 (repo,ref) 命中？ ──► 直接用，0 次探测   [decision-cache]
   │                          ├ 某个候选磁盘上已有？   ──► 一次 stat 即胜出      [cache]
   │                          └ 否则冷路径 ──► 并发扇出 ▼                        [probe]
   │
   ▼  并发宽度 = 候选对数 = Σ(每个可服务的 proxy namespace 的上游数)
   │
   ├─ namespace docker.io
   │     docker.1ms.run · registry-1.docker.io · docker.m.daocloud.io
   ├─ namespace ghcr.io
   │     ghcr.io · ghcr.m.daocloud.io
   ├─ namespace registry.k8s.io
   │     registry.k8s.io · k8s.m.daocloud.io
   └─ …（每个「namespace × 它的一个上游」= 一个候选对 = 一个 goroutine，同时开始；
        各自受 probe_timeout 限制、共享请求取消，所以墙钟 = 最慢的那个，而不是它们的和）
   │
   │  每个候选对做一次探测（通常就是一次 HEAD /v2/<repo>/manifests/<ref>；blob 请求走 blob 路径）
   │    2xx → 有 ｜ 401 → 取 Bearer token 重试一次 ｜ 405/501 → 降级 GET
   │    403/404 → 明确没有 ｜ 5xx/429/超时/网络错误 → 没答上来
   │
   ├─ 全部明确没有 → 404 NAME_UNKNOWN        全部没答上来 → 503 UNAVAILABLE
   │
   ├─ ④ digest 仲裁（namespace 内）  配置顺序里第一个答出内容的上游 = digest 权威
   │                                 其余上游 digest 不一致 → 不服务、不均衡（计入 mismatch）
   │
   ├─ ⑤ 均衡池                        digest 一致 且 延迟 ≤ balance_window(1.5×) 的上游
   │                                 weighted(1/延迟) / round_robin / off → 选一个上游
   │
   ├─ ⑥ 鉴权 Decide → 转发到 (namespace, upstream) 的引擎
   │
   ├─ ⑦ 缓存                           miss：回源并落盘 /data/<ns>/...；hit：本地读
   │                                  tag 每次回源校验，层数据本地读
   │
   └─ ⑧ 失败处理                      404/5xx(GET/HEAD) → 作废决策，重选一次
                                      内容失败 fail_threshold 次 → 冷却 2×interval
```

### 4.1 端点与端口

只监听两个端口（`server.listen` 与 `observability.metrics.listen`）：

| 端口 | 端点 | 说明 |
|---|---|---|
| 内容口 | `GET/HEAD /v2/` | API 版本探测，也是 `docker login` 用的那个。`auth.type: htpasswd` 时**无凭据总是 401** `Basic realm="rms"`；凭据正确回 200 + `{}`。`auth.type: none` 时回 200。 |
| 内容口 | `GET /v2/_catalog` | **可写 namespace 的仓库清单**（见下），支持 `n` / `last` 分页。 |
| 内容口 | `/v2/<...>` | 其余全部 registry v2 请求（manifest / blob / upload / tags）。 |
| 内容口 | `GET /healthz` | 存活：固定 200 `ok`。 |
| 内容口 | `GET /readyz` | 就绪：全部 namespace 正常回 200 `ready`；有降级 namespace 回 **503** + `degraded namespaces: <ns>, ...`。 |
| 指标口 | `GET /metrics` | rms 自己的 Prometheus 指标（路径由 `observability.metrics.path` 配置）。 |
| 指标口 | `GET /registry/metrics` | 内嵌 distribution 引擎自己的指标（与 `/metrics` 同级，路径随之推导）。 |
| 指标口 | `/debug/pprof/*` | 仅当 `observability.pprof: true`。 |

`/v2/_catalog` 的语义要准确理解：它列出的是**可写（local）namespace**里的仓库，也就是
`namespaces.local` + `auth.shared_namespaces` +（启用 `user_namespace` 时的）htpasswd 用户名；在主端点上
本地仓库名按存储原名输出（例如 `me/rms`）。**proxy 缓存 namespace 不出现在 catalog 里** —— 缓存内容由
各自的 namespace 路径直接访问，不参与清单。鉴权要求见 [§4.5](#45-鉴权与授权)（管理员，`type: none` 时匿名）。

### 4.2 路径解析顺序

`/v2/<首段>/<其余>` 按以下顺序判定（`internal/namespace`）：

1. **`/v2/` 或 `/v2`** → API 根（`KindBase`）。
2. **首段是 `_catalog` 且没有后续段** → rms 自己处理。
3. **首段像域名**（含 `.` 或 `:`，或等于 `localhost`，与 Docker 自身规范化规则一致）→ 该 **proxy
   namespace**，剥离前缀后交给它的引擎；**不跨 namespace 探测**（客户端已经点名了），但该 namespace 的
   多个上游之间仍然竞速。
4. **首段是 local namespace**（`namespaces.local` / `auth.shared_namespaces` / 用户名）→ 本地 namespace，
   **保留前缀**（仓库名仍是 `alice/app`）。
5. **否则** → **无限定请求**：不猜、不改写，交给选仓（§4.3）。没有任何候选有它 → `404 NAME_UNKNOWN`；
   所有候选都探不通 → `503 UNAVAILABLE`。

| 写法 | 行为 |
|---|---|
| `<host>:5000/docker.io/library/alpine` | 显式 namespace：直连 `docker.io`，不探测其它 namespace |
| `<host>:5000/alpine` | 无限定：并发探测所有 proxy namespace，挑真有该镜像且最快的一个 |

两个必须知道的细节：

- **`library/` 注入是 namespace 的属性，不是全局改写规则。** 只有 `official_library: true` 的 namespace
  会把单段仓库名补成 `library/<name>`（`docker.io` 默认如此，与 Docker Hub 行为一致）。判断只作用于仓库名，
  不含操作后缀。
- **前缀保留/剥离是相反的，且都是刻意的。** proxy namespace 必须**剥掉**前缀（上游 URL 需要不带 namespace
  的仓库名）；local namespace 必须**保留**前缀，因为引擎用仓库名拼上传返回的 `Location`，剥掉会让客户端拿到
  不可用的 URL（`test/distbehavior` 把这条依赖行为固定住了）。副作用见 [§4.6](#46-缓存与存储布局)。
- **没有 `namespaces.default`。** 旧版的「默认 namespace 改写」已删除，`alpine` 这种既不是域名也不是用户名的
  仓库名走第 5 步，由探测决定落到哪个 namespace。

### 4.3 无限定请求的选仓

路径里没写 namespace 的读请求（`GET /v2/library/alpine/manifests/latest`）由**并发探测**决定落到哪个
proxy namespace：

1. **候选**= 当前**能服务**的所有 proxy namespace（引擎处于降级/不可用的 namespace 不进候选）。
   每个候选按自己的规则算出要探测的仓库路径（含 `official_library` 补全）。
2. **探测**：对每个候选（**以及该 namespace 的每个上游**，§4.4）并发发
   `HEAD <upstream>/v2/<仓库>/manifests/<ref>`，单个探测受 `probe_timeout` 限制：

   | 探测结果 | 判定 |
   |---|---|
   | `2xx`（且媒体类型像 manifest/blob） | **有** |
   | `401` | 按 `WWW-Authenticate` 取 Bearer token 重试一次 |
   | `405` / `501` | 降级为 `GET` 再试 |
   | `403` / `404` | **明确没有** |
   | `5xx` / `429` / 超时 / 网络错误 | **没答上来**（不是「没有」） |

3. **判定**：在「有」的候选里取探测耗时最小者；并列取名字序在前者。全部没答上来 → `503 UNAVAILABLE`；
   全部明确没有 → `404 NAME_UNKNOWN`。
4. **缓存**：结果按 (仓库, tag/digest) 缓存 `probe_ttl`（默认 30s），窗口内不再探测；blob 请求按**仓库**
   复用这个决策（仓库亲和），随同一窗口过期。
5. **重选一次**：转发后若引擎回 `404` 或 `5xx` 且方法是 `GET/HEAD`，作废该决策并**重选一次**（只重选一次）；
   写请求不重选。

**代价（必须知道）**：未命中决策缓存时，一次无限定请求要等所有候选探测完（最慢的那个，上限
`probe_timeout`）。实测 6 个真实上游并发探测的墙钟中位数约 6s、最坏打满 10s
（`reports/raw/selection-real-upstream.txt`）。想零开销就写显式 namespace 前缀，那条路径完全不探测。

### 4.4 上游并发、digest 权威与负载均衡

一个 namespace 写了多个 `upstreams` 时，它们**默认是并发候选**而不是有序的故障转移链
（`upstream_race: true`，默认）。

1. **候选对**：「namespace × 它的每个上游」。带 namespace 的读请求（`GET /v2/docker.io/...`）走同一套，
   只是候选限定在该 namespace 内。一次冷请求的并发宽度就是候选对数。
2. **探测内容路径**：每个候选对并发探测（判定规则同 §4.3）。blob 请求走 blob 路径而不是 manifests 路径 ——
   否则会去问「这个 digest 有没有被当作 manifest 发布过」。
3. **digest 权威（同 hash 才均衡）**：每个 namespace 内，**配置顺序里第一个答出内容的上游**定义该 tag 的
   digest（它没给 `Docker-Content-Digest` 时 rms 取一次 manifest 自己算）。其余上游**只有 digest 与之相同**
   才可能被服务/被均衡；digest 不同者判为过期镜像站，既不均衡也不当选，计入
   `rms_upstream_digest_mismatch_total`。没有 digest 证据时只服务最快的一个，不做均衡。
4. **namespace 之间**：仍按「最快可用对」竞争（并列时先比上游 EWMA，再比 namespace 名）。
   **跨 namespace 不做 digest 仲裁** —— 不同 namespace 是不同来源的注册表。
5. **均衡池**：胜出 namespace 里 digest 一致、且延迟在 `balance_window`（默认 1.5×）内的上游构成池；
   池内按 `balance`（默认 `weighted` = 1/延迟加权）做**平滑加权轮询**（nginx 式，无突发）。窗口外的镜像
   不参与均衡，只在当选者失败时接手。`balance: off` 只留最快一个，`round_robin` 等权。
6. **决策缓存**：结果按 (仓库, tag/digest) 缓存 `probe_ttl`（默认 30s）；窗口内**不再并发**，
   池内轮询仍逐请求生效。blob 按仓库复用 manifest 的决策。
7. **失败重试**：转发后若回 `404`/`5xx`（`GET/HEAD`），作废决策并**重选一次**；代理读出现 `5xx` 会把该上游
   记为「内容不可用」（见 §4.7）。写请求不参与。

**「探活正常、取内容失败」有专门的缺口处理**：内容探测的结论会回灌上游健康状态，
`fail_threshold` 次（默认 2）连续「没答上来」就把该上游放进 `2 × health.interval` 的冷却，冷却期内不再进入
候选；它再答上一次就立即恢复。`/v2/` 探活**不会**清掉这个判定（两套状态是分开的），这正是 daocloud 那种
「`/v2/` 0.17s 回 401、大 blob 卡满 60s」的形态能被自动切走的原因。代理读的 `5xx` 也计一次内容失败。

**代价与取舍**：

- 冷决策的墙钟 = 最慢候选对的探测时间（上限 `probe_timeout`，默认 10s）。
  `decision_deadline`（默认 `0`=等全部）可在**第一个探测答出后**再等这么久就用已有结果决策；被放弃的候选
  记为 `skipped(decision deadline)`，**绝不**当作「没有内容」，也**不会**成为 digest 权威。代价是：权威上游
  若正好是慢的那个，该 namespace 这次退化为「只服务最快的一个，不均衡」。
- 每个 (namespace, upstream) 一个引擎（懒构建，只在它被选中/答出内容后构建），共享同一 storage root。
  安全性依据：内容是内容寻址 + 唯一临时文件原子改名；proxy namespace 关闭 TTL 调度器，没有第二个调度器争用
  `scheduler-state.json`；唯一共享可变对象是 tag link，而 digest 门保证只有同 digest 的引擎会写它。
  代价是内存随上游数增长（每个 namespace ≤ `len(upstreams)` 个引擎）。
- 并发宽度 = 候选对数。稳态不放大（决策窗口内不重复探测），冷回源时上游看到的请求数就是候选对数。

**向上游本身要求认证时**（例如把另一台开了 htpasswd 的 rms 当作上游）：必须在该 namespace 配
`username` / `password`。选仓探测是匿名的、且内容路径匿名可读（`anonymous_pull_proxy: true`）时会成功，
但 embedded proxy 在建立 Basic 挑战后会给内容请求带上（空的）`Authorization`，而 rms 对「凭据被拒绝」
**绝不**回落到匿名（§4.5）→ 401 → 本地 tag store 回落到空 → 404，且耗时只有几毫秒。症状是
「探测说存在、取内容 404」，实测读数见 `reports/raw/101-live-upstream-race.txt` §E。

**旧的顺序选举仍在**：`namespaces.proxy.<ns>.upstream_race: false` 让该 namespace 退回「配置顺序里第一个
健康者当选、失败重建引擎」的语义（`upstream.health.*` 控制），用于想要严格优先级顺序的部署。默认的并发模式下，
`upstream.health` 只用来做廉价淘汰，不再决定服务哪个上游。

实测：`test/e2e` 的 `TestNamespaceUpstreamRaceServesTheFastestMirror`、以及 `reports/rms-upstream-race.md` ——
同一 namespace 两个镜像、配置顺序故意把慢的放前面，一次无限定拉取由快的那台服务，慢的那台只收到探测；
决策窗口内的第二次拉取不再并发。

### 4.5 鉴权与授权

两个动作分开：`Authenticate` 只回答「凭据是谁」，`Decide` 才是策略。

**`Authenticate`**：无 `Authorization` → 匿名；有凭据但**校验失败** → `present=true, ok=false`，这被当作
硬 401，**不会**回落到匿名（否则 `docker login` 输错密码也会显示成功）。`auth.type: none` 时认证关闭，
任何凭据都被接受（保留用户名仅用于日志）。

**`Decide` 的规则（`auth.type: htpasswd`）**：

| 请求 | 规则 |
|---|---|
| `GET /v2/` | 无有效凭据**总是** 401 + `WWW-Authenticate: Basic realm="rms"` |
| `GET /v2/_catalog` | 仅 `admin_users` 成员（已登录但非管理员 → 403） |
| 读（GET/HEAD）proxy namespace | `anonymous_pull_proxy: true` 时匿名可读 |
| 读（GET/HEAD）local namespace | 默认要登录（`anonymous_pull_local`） |
| 写 proxy namespace | 一律 `403 DENIED`（消息里给出该往哪推）——与鉴权开关无关，是 namespace 属性 |
| 写 local namespace | 要登录；namespace 属于自己、在 `shared_namespaces` 里、或自己是 admin |
| DELETE | 鉴权同写规则；引擎侧还需 `storage.delete_enabled: true`，否则由引擎拒删（与鉴权无关） |

**`auth.type: none`（认证关闭）**：`Decide` 对本地 namespace 直接放行 —— push、DELETE、匿名读 `_catalog`
全部放开（proxy namespace 的写仍 403）。启动与 `rms config validate` 都会点名这个形态。

**为什么 `/v2/` 必须挑战**：Docker 客户端是「被挑战驱动」的。如果 `/v2/` 对匿名请求返回 200，
会同时坏两件事，都是在真实 daemon 上实测到的：

- `docker login` 不会去校验密码（它只把凭据发到被挑战的 API 根），于是**错密码也报 Login Succeeded**；
- 后续 `docker push` 不会带上已保存的凭据，blob 的 `HEAD` 拿到 401，push 直接失败。

401 只用于告知认证方式；真正的匿名读取策略仍然按 namespace 在内容请求上判定，所以公共缓存的匿名拉取不受影响
（含 containerd 透明镜像，已用 `ctr` 验证）。

**三种启动告警/校验输出（运维必读）**：

- `authentication is disabled: anyone who can reach this endpoint may push to its local namespaces`
- `anonymous pulls of proxy namespaces are disabled: a Docker daemon using this host as a registry mirror will abandon it and pull from the upstream registry instead`
- `anonymous pulls of local namespaces are enabled: every client can read every private namespace`

**镜像模式配方**（既当 dockerd `registry-mirrors`、又保留推送鉴权）：

```yaml
auth:
  type: htpasswd
  htpasswd: /etc/rms/htpasswd
  realm: rms
  user_namespace: true           # 每个 htpasswd 用户拥有同名 namespace（<user>/<image>）
  shared_namespaces: [shared]    # ⚠️ 别随手把公共名字写进来：写进来 = 任何登录用户都能推它
  anonymous_pull_proxy: true     # ← 拉取免认证（proxy / 缓存 namespace）
  anonymous_pull_local: false    # ← 私有 namespace 读也要登录
  admin_users: [admin]
```

- **pull**：`docker pull <host>:5000/library/alpine`、`docker pull <host>:5000/ghcr.io/owner/app` —— 匿名。
- **push 到私有 namespace**：`docker push <host>:5000/me/app:v1` —— 必须 `docker login`。
- **push 到 proxy namespace**：一律 `403 DENIED`（只读缓存）。
- ⚠️ **`auth.type: none` 是「显式主机名也要免密 pull」的唯一办法，代价是写也一并放开。**
  Basic 方案下 `/v2/` 必须挑战，而 classic dockerd 拿到 Basic 挑战后就不再发匿名内容请求 —— 两者只能二选一，
  本项目两台部署（`deploy/103`、`deploy/172`）于 **2026-10-14** 选择了 `type: none`（`user_namespace` 必须
  同时为 `false`）。完整的三方案实测对比与代价见[附录 B](#附录-b-变更史与实测记录)与
  [`deploy/172/README.md`](deploy/172/README.md)。**要保留认证就别关**，改用 `registry-mirrors` 那条路
  （见 [§6.6](#66-实测边界)）。

> 想更进一步（Harbor 那样用 Bearer token、按 scope 授权），需要额外做 token 签发端点，属于独立工作。

### 4.6 缓存与存储布局

每 namespace 一个根目录，内部是 distribution 原生布局：

```text
/data/<namespace>/docker/registry/v2/
├── blobs/sha256/<aa>/<digest>/data
└── repositories/<repo>/_manifests/{tags,revisions}/...
```

`<root>` 由 `storage.root` 控制，`/data/<namespace>` 就是清理与计量的边界。例如：

```text
rms/<me>/<image>:<version>        -> /data/me/...
rms/<docker.io>/<user>/<image>:…  -> /data/docker.io/...
```

**已知取舍**：可写 namespace 的仓库名里保留了 namespace（`alice/app`），所以路径是
`/data/alice/docker/registry/v2/repositories/alice/app/...`，namespace 会出现两次。理由见
[§4.2](#42-路径解析顺序)（`Location` 由仓库名拼出）。

**缓存行为**（内容已在本地时的实测）：

| 请求 | 延迟 | 说明 |
|---|---|---|
| `GET manifests/<tag>` | ~0.9s | **每次都回源校验**：distribution 的 proxyTagService 先问上游、再回落本地，这样 tag 移动能被感知 |
| `GET manifests/<digest>` | ~2ms | 本地 |
| `GET blobs/<digest>` | ~5ms | 本地 |
| `GET tags/list` | ~0.9s | 回源 |

也就是说：**层数据（体积的大头）本地读，tag 解析一次回源**，与 `registry:2` proxy 模式一致。上游不可达时
tag 查询会回落本地，缓存仍可用。

指标语义：`rms_cache_hits_total` 表示「对象已在本地、无需拉取内容」；tag 命中仍可能有一次上游校验请求。

`namespaces.proxy.<ns>.ttl: 0`（默认）表示永不过期，同时**不安装 distribution 的 TTL 调度器** —— 这一点是
必需的，否则上游切换重建引擎时两个调度器会争用同一份 `scheduler-state.json`。

### 4.7 健康、降级与故障转移

- **两套健康状态是分开的**：`upstream.health.*` 的 `/v2/` 周期探活（廉价淘汰，连续 `fail_threshold` 次失败
  驱逐、`success_threshold` 次成功恢复）；以及 §4.4 的「内容探测失败」判定（`fail_threshold` 次没答上来 →
  `2 × health.interval` 冷却）。后者**不会**被前者清掉。
- **降级（cache-only）**：上游全不可达时 namespace 退化成只读缓存引擎，已缓存镜像仍可拉，`/readyz` 报 503 并
  列出降级 namespace；上游恢复后自动构建为正常代理。带 namespace 的请求在上游全不可达时**不会**因此 503 ——
  它回落到该 namespace 的粘性引擎继续服务缓存（best-effort）。
- **无限定请求**遇到「当前没有任何可服务的 proxy namespace」时，直接 `503 UNAVAILABLE`（那些 namespace 不进入
  候选、也不会被探测）。

### 4.8 并发模型

**并发只发生在「冷决策」那一刻**：决策未命中缓存、且没有任何候选的磁盘上已有该内容时，才会向所有候选对并发
扇出。其余环节要么是单条连接，要么是逐请求的纯分配。

| 环节 | 并发？ | 并发宽度 | 说明 |
|---|---|---|---|
| 决策缓存命中（`decision-cache`） | 否 | 0 次探测 | `probe_ttl`（默认 30s）内复用 (仓库, tag/digest) 的决策 |
| 某候选磁盘已有（`cache`） | 否 | 0 次探测 | 一次 stat 就胜出，**优先级高于任何探测**，也是上游挂掉时缓存仍可拉的原因 |
| 冷决策的探测（`probe`） | **是** | 候选对数 = Σ(每个可服务 namespace 的上游数) | 每个候选对 1 个 goroutine，同时开始；各自受 `probe_timeout` 限制，整轮还受可选 `decision_deadline` 收口 |
| 单个候选对内部 | 顺序 | 1 条连接 | 一次探测最多 3 个请求：`HEAD` / `HEAD`+token（401 后）/ `GET`+token（405、501 时降级） |
| 同一 namespace 内多个上游 | **是** | = 该 namespace 的上游数 | 这也是 digest 权威的判定范围 |
| 跨 namespace | **是** | = 当前可服务的 proxy namespace 数 | 跨 namespace **不做** digest 仲裁 |
| 决策窗口内的后续请求 | 否 | 0 次探测 | 决策缓存命中；均衡池的轮询仍逐请求生效 |
| 均衡池选一个上游 | 否 | — | 只是分配，不再探测 |
| 引擎构建 | 懒构建 | 每 (namespace, upstream) 一个 | 只在它被选中/答出内容后构建，共享同一 storage root |

**并发宽度举例**：`deploy/103` 的配置里 10 个 proxy namespace 合计约 14 个上游，一次冷无限定请求就是约
**14 条并发探测连接**（外加真正取内容的那条）。带 namespace 的请求宽度小得多 —— 只有该 namespace 的上游数。

**三件必须知道的事**：

1. **没有 singleflight**。同一瞬间 N 个客户端同时拉同一个**冷**镜像，会各自跑一轮并发探测（`Chooser` 的锁只
   保护缓存的读写，不持有整轮竞速）。稳态不放大（窗口内不重复探测），但**冷启动时的并发拉取会成倍放大上游
   连接** —— 这是「冷决策并发」的代价。
2. **`decision_deadline` 是收口阀**。默认 `0` = 等全部探测（上限各自 `probe_timeout`）；设成 `3s`（两台现网
   都这么配）后，**第一个探测答出**再等 3s 就用已有结果决策，未答的候选记 `skipped(decision deadline)` ——
   它们既不算「没有」，也不会成为 digest 权威（§4.4）。代价是权威上游若正好是慢的那个，这次退化为「只服务
   最快的一个，不均衡」。
3. **上游只被探测一次，内容才是逐层取**。每个候选对在决策时只做**一次探测**（最多 3 个请求：`HEAD` /
   `HEAD`+token / `GET`+token，见上表）；真正的内容请求交给优胜 (namespace, upstream) 的引擎，layer 会按需
   取多次但**不再探测**。所以上游在冷回源时看到的是「候选对数」次探测 + 1 条真实内容请求；稳态（决策窗口内）
   接近 0 次探测。

另外两点：探测共享**请求自身的 context**，客户端断开即取消；cache-only（无上游）的 namespace 也会进入候选，
但它没有上游可问，直接记为 `skipped(no upstream configured)`，由本地磁盘判定来兜底。

---

## 5. 配置参考

完整可运行示例：[`config/rms.yaml`](config/rms.yaml)（通用/本机）、
[`deploy/103/rms.yaml`](deploy/103/rms.yaml)、[`deploy/172/rms.yaml`](deploy/172/rms.yaml)。
本节是**全部键**的参考（约 45 个），含默认值、取值域与约束。

### 5.1 加载、环境变量展开与严格解码

- 文件是 YAML，**严格解码**：未知键直接报错 —— namespace 或监听地址里打错一个字母不会静默失效。
- 每个**标量值**支持 `${VAR}` 与 `${VAR:-default}`。展开只作用于值：**键名与注释不展开**，
  所以文件里写 `# 例如 ${VAR}` 这样的注释不会导致启动失败。引用了未设置且无默认值的变量 → **启动失败**
  （而不是悄悄变成空串）。
- Duration 接受 Go duration（`30s`、`168h`）或**裸整数秒**（`0`、`90`），也接受引号包着的数字
  （`${VAR:-0}` 展开后是字符串）。
- 只允许**一个** YAML 文档，多文档报错。

### 5.2 server

| 键 | 类型 | 默认 | 说明与约束 |
|---|---|---|---|
| `listen` | string | `0.0.0.0:5000` | 主（多 namespace）监听地址。必须是 `IP:port`（host 可为空，填了必须是 IP） |
| `external_url` | string | `""` | 客户端访问主端点的 origin，**只用于拼 blob 上传返回的绝对 URL**。留空 = 按请求推导（直连时最省事，自动适配任何 host/port）。TLS 在外层终止时**必须**填 `https://...`，否则上传 `Location` 会说 http。必须 http/https、有 host、**不能带 path** |
| `http_secret` | string | `""` | 上传状态签名密钥。留空则取 `RMS_HTTP_SECRET`，仍为空则由**存储根 + 主机名**派生（稳定但不可控，启动告警）。必须跨重启稳定，否则进行中的上传失效 |
| `behind_proxy` | bool | `false` | 是否信任 `X-Forwarded-*` 推导客户端地址 |
| `graceful_shutdown` | duration | `20s` | 关闭时等待在途请求的上限 |
| `read_timeout` | duration | `0`=不限 | HTTP 读超时。默认 0 是因为层推送可能非常长 |
| `write_timeout` | duration | `0`=不限 | HTTP 写超时，同上 |

### 5.3 storage

**子块**：`upload_purge`（映射到 distribution 的 `maintenance.uploadpurging`）。

| 键 | 类型 | 默认 | 说明与约束 |
|---|---|---|---|
| `root` | string | `/data` | 每 namespace 根目录的父目录：`<root>/<namespace>`。**必须绝对路径** |
| `delete_enabled` | bool | `false` | 允许 DELETE local namespace 的 manifest/blob（示例配置置 `true`） |
| `upload_purge.enabled` | bool | 三个键全缺省时自动 `true` | 清理被放弃的 blob 上传（映射 distribution 的 `maintenance.uploadpurging`） |
| `upload_purge.age` | duration | `168h` | 多久没动的上传算被放弃 |
| `upload_purge.interval` | duration | `24h` | 清扫周期 |

### 5.4 auth

| 键 | 类型 | 默认 | 说明与约束 |
|---|---|---|---|
| `type` | string | `htpasswd` | 只接受 `htpasswd` / `none`。`none` = 完全开放，见 [§4.5](#45-鉴权与授权) |
| `htpasswd` | string | `""` | bcrypt htpasswd 路径。`type: htpasswd` 时**必填且文件必须存在**（校验会 stat） |
| `realm` | string | `rms` | `WWW-Authenticate: Basic realm="..."` 里广告的 realm |
| `user_namespace` | bool | `false` | 每个 htpasswd 用户拥有同名 namespace：`<user>/<repo>` → `/data/<user>`。**要求 `type: htpasswd`**，否则校验失败 |
| `shared_namespaces` | []string | `[]` | 额外的共享可写 namespace，任意已认证用户可推。**优先于同名用户**：显式配置即声明它是共享的 |
| `anonymous_pull_proxy` | bool | `false` | 允许匿名读 proxy namespace。**关掉 = dockerd 的镜像源会绕过本机**（示例配置置 `true`） |
| `anonymous_pull_local` | bool | `false` | 允许匿名读 local namespace。打开 = 任何人可读私有 namespace |
| `admin_users` | []string | `[]` | 可读合并 catalog、可写/删任意 local namespace |

### 5.5 namespaces

`namespaces` 的子块：`local`、`proxy.<域名>`、`selection`（`selection` 只影响无限定请求与上游竞速）。

| 键 | 类型 | 默认 | 说明与约束 |
|---|---|---|---|
| `local` | []string | `[]` | 额外的可写 namespace。名字**不能像域名**（不得含 `.` `:`），正则 `^[a-z0-9]+(?:[._-][a-z0-9]+)*$` |
| `proxy.<域名>` | 映射 | **至少一个** | 键必须像注册表域名（含 `.` 或 `:`，或为 `localhost`） |
| `proxy.<域名>.upstreams` | []string | 必填 ≥1 | 上游地址。必须 http/https、**不能带 path**（namespace 已经充当路径）。**顺序 = digest 权威顺序**；`upstream_race: false` 时也是选举顺序 |
| `proxy.<域名>.official_library` | bool | `false` | 单段仓库名补齐成 `library/<name>`（Docker Hub 规则） |
| `proxy.<域名>.username` / `.password` | string | `""` | 向上游认证（可选）。上游要求认证时必须配，否则症状是「探测说有、取内容 404」（§4.4） |
| `proxy.<域名>.ttl` | duration | `0`=永不过期 | 缓存过期。**0 同时不安装 TTL 调度器**（§4.6）。不能为负 |
| `proxy.<域名>.http` | 对象 | 空 = 整体继承 `upstream.http` | 该 namespace 的出网覆盖；**逐字段合并，只有非零字段覆盖** |
| `proxy.<域名>.upstream_race` | bool | 缺省 = `true` | `true` = 上游并发竞争；`false` = 退回有序选举（第一个健康者当选，失败重建引擎） |
| `proxy.<域名>.balance` | string | `""` → 继承 `selection.balance` | `weighted` / `round_robin` / `off`；只在 racing 打开时生效 |
| `selection.probe_ttl` | duration | `30s` | 决策缓存时长，也是**仓库亲和**窗口。窗口内不重复探测。（填 0 会被赋成默认值，不能用来关闭） |
| `selection.probe_timeout` | duration | `10s` | 单个候选探测上限，也是未命中缓存时请求等待决策的上限。与 `upstream.health.timeout` 同量级 |
| `selection.decision_deadline` | duration | `0`=等全部 | 软截止：第一个探测答出后再等这么久就用已有结果决策，未答的候选记为 `skipped`。**必须 ≥0 且 ≤ `probe_timeout`** |
| `selection.balance` | string | `weighted` | 全局均衡模式：`weighted`（1/延迟加权，默认）/ `round_robin` / `off` |
| `selection.balance_window` | float | `1.5` | 均衡窗口，倍数。**必须 ≥1**；只把该倍数以内、且 digest 一致的上游放进轮询池 |

### 5.6 upstream

| 键 | 类型 | 默认 | 说明与约束 |
|---|---|---|---|
| `health.enabled` | bool | **`false`** | 是否做 `/v2/` 周期探活（示例配置置 `true`）。**默认不探活**是代码行为 |
| `health.interval` | duration | `30s` | 探活周期（内容失败的冷却期是它的 2 倍） |
| `health.timeout` | duration | `10s` | 单次探活上限。给得宽松是实测结论：经共享代理时 quay.io / registry.k8s.io 要 4–5s |
| `health.fail_threshold` | int | `2` | 连续失败多少次驱逐一个 endpoint |
| `health.success_threshold` | int | `1` | 连续成功多少次让它重新可用 |
| `health.path` | string | `/v2/` | 探活路径 |
| `http.proxy` | string | `""` → 回落 `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY` | 出网代理。**实现细节**：distribution 的上游请求用 `http.DefaultTransport`，rms 启动时会替换它（`internal/httpx`），所以只设环境变量在进程内已有自定义 transport 时可能失效 |
| `http.no_proxy` | string | `""` | 逗号分隔的 host/CIDR/IP；Go 的 httpproxy 支持 CIDR |
| `http.dial_timeout` | duration | `10s` | 连接超时 |
| `http.response_header_timeout` | duration | `60s` | 等响应头超时 |
| `http.tls_handshake_timeout` | duration | `10s` | TLS 握手超时 |
| `http.idle_conn_timeout` | duration | `90s` | 空闲连接超时 |
| `http.insecure_skip_verify` | bool | `false` | 跳过上游证书校验 |
| `http.ca_file` | string | `""` | 自定义 CA。瘦基础镜像缺 `ca-certificates` 会让**所有** HTTPS 上游报 `certificate signed by unknown authority`（实测踩过） |

### 5.7 observability

`observability` 的子块：`logs`、`traces`、`metrics`、`pprof`；其中 `traces` 与 `metrics` 是两个独立子块。

| 键 | 类型 | 默认 | 说明与约束 |
|---|---|---|---|
| `logs.level` | string | `info` | 日志级别 |
| `logs.format` | string | `json` | 只接受 `json` / `text` |
| `logs.otlp` | bool | `false` | 同时把日志推到 OTLP 端点（预留） |
| `traces.enabled` | bool | `false` | OTLP trace 导出 |
| `traces.endpoint` | string | `192.168.188.103:4317` | SigNoz OTLP gRPC 端点。`enabled` 时必须是合法 `host:port` |
| `traces.insecure` | bool | `false` | 明文 gRPC |
| `traces.sample_ratio` | float | `0.2` | 采样率 |
| `traces.headers` | []string | `[]` | 额外的 OTLP header |
| `metrics.listen` | string | `0.0.0.0:9100` | 指标监听地址。**必须与 `server.listen` 不同**（指标与 pprof 不能暴露在发镜像的端口上） |
| `metrics.path` | string | `/metrics` | **必须以 `/` 开头**；distribution 的指标在同级 `/registry/metrics` |
| `pprof` | bool | `false` | 在指标口暴露 `/debug/pprof/` |

指标清单（含标签）：

| 指标 | 标签 | 含义 |
|---|---|---|
| `rms_http_requests_total` | `listener,namespace,method,status` | 请求数（无限定请求的 namespace 为空时记为 `none`，主端点 listener 记为 `<multi>`） |
| `rms_http_request_duration_seconds` | `listener,namespace,method` | 请求耗时 |
| `rms_cache_hits_total` | `namespace` | 对象已在本地、无需拉取内容（tag 命中仍可能有一次上游校验） |
| `rms_cache_misses_total` | `namespace` | 对象不在本地，引擎回源并缓存 |
| `rms_upstream_election_changes_total` | `namespace,upstream` | 上游选举变化（并发模式下只在粘性引擎跟着选举走时增长） |
| `rms_namespace_up` | `namespace` | 1=正常代理，0=降级（只服务缓存或不可用） |
| `rms_namespace_errors_total` | `namespace` | namespace 生命周期失败次数 |
| `rms_upstream_probe_duration_seconds` | `namespace,upstream` | 内容探测耗时（race 的判定依据；失败/超时也记录，错过 deadline 的在放弃时刻记录） |
| `rms_upstream_wins_total` | `namespace,upstream` | 请求被交给了哪个上游；开了均衡时这个分布就是均衡本身 |
| `rms_upstream_digest_mismatch_total` | `namespace,upstream` | digest 与权威上游不一致而被拒绝服务的次数（>0 说明某镜像站过期） |
| `rms_selection_decisions_total` | `source` | 决策来源：`probe` / `decision-cache` / `cache`（本地已有）/ `affinity`（按仓库亲和）/ `none` |
| `rms_build_info` | `version,commit` | 版本，值恒为 1 |

Traces：span 名保持低基数（`GET /v2/docker.io/library/alpine/manifests`），`service.name=rms`。
日志：JSON 到 stdout（`docker logs`），字段含 `namespace` / `request_id`。

### 5.8 校验规则清单

`rms config validate` 会跑与启动完全相同的校验。被拒绝的情形：

| 位置 | 条件 |
|---|---|
| 解码 | 未知键；多于一个 YAML 文档；Duration 不是 duration 也不是数字 |
| 环境展开 | `${VAR}` 未设置且无 `:-` 默认值 |
| `server.listen` / `observability.metrics.listen` | 不是合法 `IP:port` |
| `observability.metrics.listen` | 与 `server.listen` 相同 |
| `server.external_url` | scheme 不是 http/https、没有 host、或带了 path |
| `server.http_secret` | 为空（不报错，但会告警并派生出派生值） |
| `storage.root` | 不是绝对路径 |
| `auth.type` | 不是 `none` / `htpasswd` |
| `auth.htpasswd` | `type: htpasswd` 时为空，或文件不存在 |
| `auth.user_namespace` | 为 `true` 但 `type` 不是 `htpasswd` |
| `namespaces.proxy` | 一个都没有 |
| `namespaces.proxy[<k>]` | 键不像注册表域名 |
| `namespaces.proxy[<k>].upstreams` | 为空；URL scheme 不是 http/https；没有 host；带了 path |
| `namespaces.proxy[<k>].ttl` | 负数 |
| `namespaces.proxy[<k>].balance` / `selection.balance` | 不是 `weighted` / `round_robin` / `off` |
| `namespaces.selection.balance_window` | < 1 |
| `namespaces.selection.decision_deadline` | 负数；或 > `probe_timeout` |
| `namespaces.local` / `auth.shared_namespaces` | 名称为空、像域名（含 `.` `:`）、或不符合命名正则 |
| `observability.metrics.path` | 不以 `/` 开头 |
| `observability.traces.endpoint` | `enabled` 时不是合法 `host:port` |
| `observability.logs.format` | 不是 `json` / `text` |

```bash
# auth.htpasswd 默认是容器里的 /etc/rms/htpasswd，在宿主机上跑要指到真实文件
RMS_HTPASSWD=$PWD/config/htpasswd RMS_STORAGE_ROOT=$PWD/data \
  ./bin/rms config validate --config config/rms.yaml
```

### 5.9 环境变量与 Make 变量

**配置里通过 `${}` 引用的**（也可以在 `.env` 里由 compose 注入）：

| 变量 | 用途 |
|---|---|
| `RMS_EXTERNAL_URL` | `server.external_url` |
| `RMS_HTTP_SECRET` | `server.http_secret` 的回退值 |
| `RMS_STORAGE_ROOT` | `storage.root` |
| `RMS_HTPASSWD` | `auth.htpasswd` |
| `RMS_UPSTREAM_PROXY` | `upstream.http.proxy` |
| `RMS_NO_PROXY` | `upstream.http.no_proxy` |

**compose 使用的**：

| 变量 | 默认 | 用途 |
|---|---|---|
| `RMS_IMAGE` | `rms:dev` | 镜像名（103/172 上指向 `rms.majorc.eu.org/me/rms:latest`） |
| `RMS_PORT_PRIMARY` | `5000` | 宿主内容端口 |
| `RMS_PORT_METRICS` | `9100` | 宿主指标端口 |
| `RMS_DATA_DIR` | `./data` | 宿主缓存目录（挂到 `/data`） |
| `RMS_DOCKERFILE` | `Dockerfile` | 可换 `Dockerfile.binary` |
| `RMS_RUNTIME_BASE` | `gcr.io/distroless/static-debian12:nonroot` | 运行时基础镜像（必须带 CA 证书） |
| `RMS_VERSION` / `RMS_COMMIT` | `dev` / `none` | 编译期注入 |
| `HTTP_PROXY` / `HTTPS_PROXY` / `NO_PROXY` | — | 构建期与运行期的出网代理 |

**Make 变量**：`REGISTRY`、`REPO`、`RELEASE_IMAGE`、`RMS_REGISTRY_USER` / `RMS_REGISTRY_PASSWORD`、
`RMS_HTPASSWD_USER` / `RMS_HTPASSWD_PASSWORD`、`DEPLOY_REMOTE` / `DEPLOY_DEST`、
`DEPLOY172_REMOTE` / `DEPLOY172_DEST`、`PUSH_RETRIES`、`CONTAINER_UID` / `CONTAINER_GID`、
`RUNTIME_BASE`、`BUILDER_BASE`、`PROXY`、`VERSION`、`COMMIT`。

> **`deploy/103/.env` 与 `deploy/172/.env` 是被 `.gitignore` 忽略的每台机器本地文件**
> （`deploy/*/install.sh` 会读取并投递它，所以它们不在仓库里）。必需键就是上表 compose 那一段，
> 外加各自环境的 `RMS_UPSTREAM_PROXY`；两台现网的实际取值见 `deploy/103/README.md` 与
> `deploy/172/README.md`。根目录 `.env.example` 是本机 compose 的模板。

> `scripts/env.sh` 里的 `RMS_ROOT`、`RMS_PID` 属沙箱/验收脚本专用，不是运行配置。

### 5.10 三份配置的关系

| 文件 | 定位 | 差异 |
|---|---|---|
| [`config/rms.yaml`](config/rms.yaml) | 通用/本机模板，每个关键决策都有注释 | 5 个 proxy namespace；`auth.type: htpasswd` |
| [`deploy/103/rms.yaml`](deploy/103/rms.yaml) | `192.168.188.103` 现网 | 9 个 proxy namespace；`auth.type: none`；`shared_namespaces: [me, shared]`；`decision_deadline: 3s` |
| [`deploy/172/rms.yaml`](deploy/172/rms.yaml) | `10.82.64.172` 现网 | 10 个 proxy namespace；`auth.type: none`；只有私仓 `ops`；`decision_deadline: 3s` |

---

## 6. 客户端接入

rms 只有**一个**内容端点（主端点 `:5000`），namespace 写在镜像名里。

### 6.1 两种写法

| 写法 | 行为 | 适用 |
|---|---|---|
| `docker pull <host>:5000/docker.io/library/alpine` | 显式 namespace：**直连**该 namespace，不探测其它 | 任何上游、任何客户端；需要 `docker login`（Basic 挑战下无法匿名，见 [§6.6](#66-实测边界)） |
| `docker pull <host>:5000/alpine` | 无限定：并发探测所有 proxy namespace，挑真有该镜像且最快的一个 | 只能写裸 `host:port` 的客户端（`registry-mirrors`、旧式 `registries.yaml`），且只对 Docker Hub 可靠 |

### 6.2 Docker daemon.json

```json
{
  "insecure-registries": ["192.168.188.101:5000"],
  "registry-mirrors": ["http://192.168.188.101:5000"]
}
```

- `registry-mirrors` **只对 Docker Hub 生效**（发的是无限定名，由 rms 挑到 `docker.io`），且必须是**裸
  `host:port`**：URI 里带 path 会被 dockerd 判非法（起不来），见 [§6.5](#65-为什么客户端没法用路径表达-namespace)。
- 其它上游用显式镜像名：

  ```bash
  docker pull 192.168.188.101:5000/docker.io/library/nginx
  docker pull 192.168.188.101:5000/ghcr.io/owner/app
  docker pull 192.168.188.101:5000/nginx          # 无限定 + library/ 注入，落到 docker.io
  ```

- `localhost` / `127.0.0.1` 之外的宿主需要加 `insecure-registries`（rms 是明文 HTTP，TLS 由外层反代做）。

> 从「按上游绑定的端口」迁过来的人注意：那些端口**已经不存在**，`daemon.json` 里指向它们的条目要改成
> `:5000`（只留一个即可）。逐条迁移清单见 [`deploy/103/README.md`](deploy/103/README.md)。

### 6.3 k3s 与 containerd 的 registries.yaml

```yaml
mirrors:
  docker.io:
    endpoint:
      - "http://192.168.188.101:5000"     # 裸 host:port；namespace 由 rms 自己挑
configs:
  "192.168.188.101:5000":
    auth:
      username: alice
      password: alicepw          # 私有 namespace 才需要；公共缓存可省略
    tls:
      insecure_skip_verify: true # 明文 HTTP 时 containerd 需要允许
```

k3s 放 `/etc/rancher/k3s/registries.yaml`，然后 `systemctl restart k3s`。

**这里给不出「一个上游一个 endpoint」的写法**（旧的 `ghcr.io: endpoint: [...:5002]` 已失效）：
`registries.yaml` 没有 `override_path`，endpoint 不能带路径；现在所有 proxy namespace 都只在 `:5000` 后面。
要精确指定上游，用镜像名前缀或 §6.4 的 `hosts.toml`。完整示例见
[`config/registries.yaml.example`](config/registries.yaml.example)。

### 6.4 containerd hosts.toml（透明镜像）

```toml
# /etc/containerd/certs.d/docker.io/hosts.toml
server = "https://registry-1.docker.io"

[host."http://192.168.188.101:5000/v2/docker.io"]
  capabilities = ["pull", "resolve"]
  override_path = true
```

`override_path = true` 让 containerd 把整个路径当作 API 根，于是请求正好是 `/v2/docker.io/<repo>/...`，
落到主端点。对**任意**上游都成立，不只是 Hub：

```toml
# /etc/containerd/certs.d/ghcr.io/hosts.toml
server = "https://ghcr.io"

[host."http://192.168.188.101:5000/v2/ghcr.io"]
  capabilities = ["pull", "resolve"]
  override_path = true
  skip_verify = true
```

这条路径可用但不常用，因为是「不可见配置」，排障时容易迷惑。

### 6.5 为什么客户端没法用路径表达 namespace

「`registries.yaml` / `daemon.json` 里能不能写 `rms.example.org/docker.io` 这样的路径？」——**都不能**，
这是查过源码的结论：

**containerd（`core/remotes/docker/config/hosts.go`）**

```go
if len(u.Path) > 0 {
    u.Path = path.Clean(u.Path)
    if !strings.HasSuffix(u.Path, "/v2") && !config.OverridePath {
        u.Path = u.Path + "/v2"     // 路径被拼在 /v2 之前
    }
} else if !config.OverridePath {
    u.Path = "/v2"
}
```

- 旧式 `registries.yaml` 的 `mirrors.<domain>.endpoint` **没有** `override_path` 字段，所以
  `endpoint: http://host/docker.io` 会请求 `/docker.io/v2/<repo>` —— 不是我们要的 `/v2/docker.io/<repo>`。
- 只有新式 `config_path` + `hosts.toml` 显式写 `override_path = true`，endpoint 才能写成
  `http://host/v2/docker.io`（见 §6.4）。

**Docker `daemon.json`（moby `registry/config.go` `ValidateMirror`）**

```go
if (uri.Path != "" && uri.Path != "/") || uri.RawQuery != "" || uri.Fragment != "" {
    return "", invalidParamf("invalid mirror: path, query, or fragment at end of the URI %q", uri)
}
```

- 当前 dockerd **直接判非法**（写进去 dockerd 起不来），对应
  [moby#47144](https://github.com/moby/moby/issues/47144)；放宽校验的是
  [PR #46594](https://github.com/moby/moby/pull/46594)。
- 另外 `registry-mirrors` **只对 Docker Hub 生效**，对 ghcr.io / gcr.io / quay.io 无效。

所以：**containerd 想要「透明镜像」（镜像名不变）只能靠 `hosts.toml + override_path`**，
没有路径可写的旧式 `registries.yaml` 只能给 Docker Hub 用。

### 6.6 实测边界

一句话：**`/v2/` 401 是设计，内容匿名是前提，`probe_ttl` 窗口内不重复探测。**

**① `/v2/` 回 401 无害（2026-10-13 实测，101 的 dockerd 与线上版本一致）**：镜像源指向 `:5000`
（`/v2/` 仍是 401 `Basic realm="rms"`）时，`docker pull hello-world:latest` 成功，且 rms 侧
`rms_cache_hits_total{namespace="docker.io"}` 从 33 增到 40 —— 缓存确实被走到，不是回落官方。客户端
`~/.docker/config.json` 里**没有**该镜像源的凭据（只有 `10.82.64.172:5001`、`l-rms.majorc.eu.org`），即匿名。

- 真正决定死活的是**内容路径**：`anonymous_pull_proxy: true` 时代理 namespace 的内容请求匿名可读
  （§4.5），dockerd 的镜像源请求因此能拿到 manifest 与 blob；内容路径一旦挑战
  （`anonymous_pull_proxy: false`，或旧版那套「内容也挑战」的配置），dockerd 就报下面这条并转投官方，
  **缓存被整体绕过**：

  ```text
  level=info msg="Attempting next endpoint for pull after error:
    Head \"http://<rms>/v2/library/alpine/manifests/3.16\": no basic auth credentials"
  ```

- **`docker login <镜像源>` 救不了 `registry-mirrors`**：moby 按 **official index** 解析凭据
  （`registry.GetAuthConfigKey`：official 一律用 `https://index.docker.io/v1/` 这个键），镜像源主机自己
  那条 `auths` 记录不会被查。所以镜像源必须「内容匿名可读」，凭据只能在**显式带 namespace 的拉取**上起作用。

**② 显式主机名的匿名拉取做不到（2026-10-13 晚实测，103 与 172 两地同版本复现）**：
`docker pull <host>:5000/<ns>/<repo>` 先 ping `GET /v2/`，拿到 401 + `WWW-Authenticate: Basic` 之后 dockerd
就认定该 registry 需要凭据，**连 manifest 都不请求**（rms 侧来自 dockerd 的请求数 = 0），只留一行
`Head "http://<host>:5000/...": no basic auth credentials`。根因是 **Basic 认证没有匿名换令牌的流程**
（Docker Hub / Harbor 用 Bearer：401 里带 realm，客户端能匿名换 token）。所以「镜像名带 namespace」
并不等于「免登录」——显式拉取要么先 `docker login`，要么改用 `registry-mirrors`（只对 Docker Hub 名字生效），
要么把服务端切成 `auth.type: none`（代价见 §4.5 与[附录 B](#附录-b-变更史与实测记录)）。

**③ 无限定名 + 裸 `host:port` 的事实边界**：镜像源指 `:5000` 时，docker 拉 Docker Hub 的 `alpine` 这类
无限定名**仍能命中**（无限定请求会挑到 `docker.io`）；但 `ghcr.io` 等其它上游的无限定名**可能被挑到别的
namespace，或谁都对不上而返回 `404 NAME_UNKNOWN`**。适用范围：所有只写裸 `host:port` 的客户端；
不适用于镜像名里带 namespace 前缀的调用（那种请求根本不探测）。

**④ 上游可达性依赖宿主**（决定 `upstreams` 怎么填，实测）：

| 来源宿主 | 公共仓库（registry-1.docker.io 等） | `10.82.64.172:5000/5001/5002`（**内网镜像主机自己的端口，与 rms 暴露的端口无关**） |
|---|---|---|
| `192.168.188.101`（开发宿主，有 tun_sase） | **直连失败**；走 `http://192.168.188.1:3128` 才通 | ✅ 可达 |
| `192.168.188.103`（公共容器环境） | 握手直连可达，但**层下载**直连很差（12 KB/s、传不完）→ 实际仍走 `3128` 代理 | ❌ 不可达 |

- **跑在 `.101`**：必须给代理（`.env` 里的 `HTTP_PROXY` / `HTTPS_PROXY`，以及 `NO_PROXY` 排除
  `192.168.188.0/24,10.82.0.0/16,10.44.0.0/16`），并且可以让 `docker.io` 先链内网镜像再回落官方：

  ```yaml
  docker.io:
    upstreams:
      # 这三行是「内网镜像主机」自己的地址与端口（它有多个实例），
      # 不是 rms 对外暴露的端口 —— rms 只有 5000 与 9100。
      - "http://10.82.64.172:5000"
      - "http://10.82.64.172:5001"
      - "https://registry-1.docker.io"
  ```

- **跑在 `.103`**：`upstreams` 只写官方地址（写内网镜像地址会让该 namespace 一直降级），但**出网要走
  局域网代理** —— 这一点与早期文档写的「.103 直连即可」相反，是实测推翻的：`.103` 对公共仓库的 `/v2/`
  握手直连可达，但**层下载**直连很糟（1ms 直连 12 KB/s、传不完；经代理 168 KB/s、完整；官方 Hub 直连直接
  不可达）。现网配置见 `deploy/103/rms.yaml` 里该 namespace 的实测注释。

公共上游在探测时可能接近超时（`reports/raw/selection-real-upstream.txt` 里 6 个真实上游各 12 次探测：
quay.io 中位 2.7s、registry.k8s.io 中位 3.1s，最坏打满 10s 的那次是 quay.io），所以
`upstream.health.timeout` 与 `namespaces.selection.probe_timeout` 默认都给到 10s。

---

## 7. 部署

### 7.1 本机 docker compose

```bash
make prepare      # 建 ./data，chown 给容器 uid 65532；把 config/* 调成容器可读
make up           # docker compose up -d --build
```

容器以 `65532:65532`（nonroot）运行，因此：

- `./data` 必须能被 65532 写 —— `make prepare` 负责；
- `./config/rms.yaml` 0644、`./config/htpasswd` `root:65532 0640`（里面是密码哈希，不给 world read）——
  `make prepare` 负责。

`docker-compose.yml` 里所有端口、数据目录、代理变量都可通过 `.env` 覆盖。**只有两个端口：`5000`（唯一的内容
入口）与 `9100`（指标）。** 根目录这份 compose 与 [`deploy/103/docker-compose.yml`](deploy/103/docker-compose.yml)
是同一形态，不会分叉。

> 早先的部署把 `:5001` 绑到 `docker.io`、`:5002` 绑到 `ghcr.io`，那是靠**已经删除**的 `listeners` 配置块
> 实现的。**只把端口映射加回 compose 是不够的**：配置里已经没有能把端口绑定到 namespace 的键，容器起来了
> 也无处可路由。要恢复那种形态，得先把 `listeners` 特性做回来。

### 7.2 镜像构建的两条路径

| 方式 | 何时用 |
|---|---|
| `make image`（[`Dockerfile`](Dockerfile)，多阶段） | 常规。需要能拉 `golang:1.27-alpine` 与 distroless 基础镜像 |
| `make image-binary`（[`Dockerfile.binary`](Dockerfile.binary)） | CI，或拉不到 Go 工具链镜像的环境。先用 `make build` 在宿主产出静态二进制，再打进运行时镜像 |

两者基础镜像都可用 `--build-arg BUILDER_BASE=` / `RUNTIME_BASE=` 覆盖。**运行时基础镜像必须带 CA 证书**
（`make image-binary` 会先断言二进制是静态链接，避免 musl 下「no such file or directory」这种迷惑报错）。

依赖已 vendor（`vendor/`），所以镜像构建**不需要网络**。

### 7.3 发布与两台机器的部署

两台机器都不放源码、都不在本地构建，但投递方式不同：103 能拉到 `rms.majorc.eu.org`，走 push/pull；
172（公司内网，只能从 101 经 SASE 到）走 `docker save` → scp → `docker load`。

| 命令（在 101 / 本仓库执行） | 作用 |
|---|---|
| `make login RMS_REGISTRY_PASSWORD=...` | `docker login rms.majorc.eu.org`（凭据落在 `./.docker/`，已 gitignore） |
| `make push` | 构建并 push `rms.majorc.eu.org/me/rms:<version>` 与 `:latest`（脏树的 version 带 UTC 时间戳，每个 tag 都是一次可回滚的发布） |
| `make deploy-103` | `push` + 放 compose/.env/rms.yaml（带备份）+ 103 `docker compose pull` + 重启 + 清旧镜像 |
| `make update-103` | 只做 103 侧：拉取 → 重启 → 清旧镜像 |
| `make deploy-172` | 构建 → `docker save` → scp 到 `ops@10.82.64.172` → `docker load` + 重建 + 清旧镜像 |

- **103**：`/opt/stacks/rms` 只有 `docker-compose.yml` / `.env` / `config/` / `data/`。镜像名里的 `me` 是
  rms 的本地 namespace（`auth.shared_namespaces`）。**2026-10-14 起 103 是 `auth.type: none`**，所以 push 与
  103 拉取都不再需要登录，代理 namespace 照旧匿名可拉。103 的 compose 故意不写 `pull_policy: always` ——
  它拉的 registry 就是它自己（`rms.majorc.eu.org` 经路由器 nginx 指回 `192.168.188.103:5000`），容器停着时
  强制拉取会让 `up -d` 起不来；更新脚本用 `docker compose pull --policy always` 再 `up -d`。
  `make push` 自带重试：唯一的大层（约 9 MiB 的二进制）经 nginx 容器缓冲时偶尔会超过 registry 客户端 60s 的
  响应头超时，而 blob 通常已在服务端落盘，重试会看到 `Layer already exists` 后立刻完成。
  细节、代价、回滚与实测读数见 [`deploy/103/README.md`](deploy/103/README.md)。
- **172**：`/app/rms`，内容端口 **5000**、指标 **9100**，一个可写私仓 **`ops`**
  （`10.82.64.172:5000/ops/<镜像>`），10 个匿名可拉的 proxy namespace。2026-10-14 起同样 `auth.type: none`：
  拉和推都不再要密码，代价是任何能连到 `:5000` 的人都能推/删 `ops`、匿名读 `_catalog`。
  两边的 `deploy/*/install.sh` 在放置的文件真的变了时会 `docker compose up -d --force-recreate` ——
  compose 不会因为挂载文件内容变化而重建容器。
  细节、客户端要求（`insecure-registries`）与实测读数见 [`deploy/172/README.md`](deploy/172/README.md)。

### 7.4 生产建议

- TLS 与域名由外层反代（Caddy/nginx）终止，然后设置 `RMS_EXTERNAL_URL=https://rms.example.org`，
  否则上传 `Location` 会是 http。
- `:9100` 只绑内网或本机。
- 用 `RMS_HTTP_SECRET` 固定上传状态密钥。
- 定期按 namespace 清缓存：`du -sh data/*` 定位，删除整个 `<root>/<namespace>` 目录即可（只影响该
  namespace 的缓存）。
- 对外暴露前想清楚鉴权形态：`auth.type: none` 意味着**任何人**都能向 local namespace 推、删内容。
  需要保护就保留 `htpasswd`，并接受「显式主机名拉取必须 `docker login`」（§6.6）。

---

## 8. 运维

### 8.1 常用命令

```bash
# 用户管理（bcrypt，写文件是原子的）
make htpasswd USER=bob PASS=bobpw      # 顺带把权限调成容器用户可读（root:65532 0640）
./bin/rms htpasswd list --file config/htpasswd

# 直接用 CLI 时文件是 0600（里面有密码哈希），容器读不到，需要自己放权：
#   ./bin/rms htpasswd add --file config/htpasswd --user bob --password bobpw
#   chgrp 65532 config/htpasswd && chmod 0640 config/htpasswd

# 热加载 htpasswd（不用重启）
docker kill -s HUP rms

# 配置校验（宿主机跑：auth.htpasswd 默认是容器里的 /etc/rms/htpasswd，要指到真实文件）
RMS_HTPASSWD=$PWD/config/htpasswd RMS_STORAGE_ROOT=$PWD/data \
  ./bin/rms config validate --config config/rms.yaml
# 或在容器里（用的就是挂载进去的那份配置）
docker compose exec rms /usr/local/bin/rms config validate --config /etc/rms/rms.yaml

# 健康 / 状态
curl -s localhost:5000/healthz
curl -s localhost:5000/readyz          # 503 时列出降级 namespace
docker compose exec rms wget -qO- localhost:9100/metrics 2>/dev/null || curl -s localhost:9100/metrics
docker exec rms /usr/local/bin/rms version   # 镜像里编译进去的 version/commit
```

CLI 全貌：`rms serve --config`、`rms healthcheck --addr --timeout`、
`rms htpasswd add|list --file --user --password`、`rms config validate --config`、`rms version`。

### 8.2 排障速查

| 症状 | 多半是 | 处置 |
|---|---|---|
| 拉取成功但缓存没增长（`rms_cache_hits_total` 不动） | 客户端回落到了上游官方 | 确认内容路径匿名可读（`anonymous_pull_proxy: true`，或 `type: none`）；查看 rms 日志里有没有来自客户端的请求 |
| `no basic auth credentials` | Basic 挑战下客户端不做匿名显式拉取 | 改走 `registry-mirrors`，或 `docker login`，或改用 `auth.type: none`（§6.6） |
| 无限定请求 `404 NAME_UNKNOWN` | 该镜像不在任何 proxy namespace 上（非 Hub 上游尤其如此） | 用带 namespace 的镜像名（§4.2） |
| 无限定请求慢（数秒） | 冷决策要等所有候选探测完 | 写显式 namespace 前缀；或调小 `probe_timeout` / 设 `decision_deadline` |
| 无限定请求 `503 UNAVAILABLE` | 所有候选都在降级/不可用 | 看 `/readyz` 列出的 namespace 与 `rms_namespace_up` |
| 某 namespace 一直 degraded | 上游地址在**本机**不可达（见 §6.6 的可达性矩阵） | 修正 `upstreams` 或出网代理 |
| 所有 HTTPS 上游报 `certificate signed by unknown authority` | 运行时镜像没有 CA 证书 | 换带 CA 的基础镜像，或配 `upstream.http.ca_file` |
| 「探测说有、取内容 404」 | 上游要求认证但没配 `username`/`password` | 补上凭据（§4.4） |
| 上传 `Location` 是 http | 外层终止了 TLS 但没设 `external_url` | 设 `RMS_EXTERNAL_URL=https://...` |
| 改了 htpasswd 但不生效 | 逐文件挂载钉住了 inode | 挂**目录**，然后 `docker kill -s HUP rms` |
| `rms_namespace_up=0` 且缓存还能拉 | 上游全不可达后的 cache-only 降级 | 属预期行为；恢复上游后自动回到正常代理 |

---

## 9. 开发

### 9.1 Make 目标

| 类别 | 目标 |
|---|---|
| 依赖 | `deps` `tidy` `vendor` |
| 构建 | `build`（产出 `bin/rms`；`image-binary` 依赖它） |
| 质量 | `fmt` `vet` `test` `test-race` `cover` `check`（=fmt+vet+test） |
| 运行 | `run`（用 `config/rms.yaml` + `./data`） `up` `down` `logs` |
| 镜像 | `image` `image-binary` |
| 发布/部署 | `login` `push` `deploy-103` `update-103` `deploy-172` |
| 宿主准备 | `prepare` `clean` `distclean`（破坏性） |
| 测试 | `e2e`（进程内假上游 + 真 HTTP，不依赖网络）`e2e-docker`（再驱动真实 docker CLI） |
| 其他 | `htpasswd` `help` |

```bash
source scripts/env.sh     # 本沙箱里 Go 缓存与 DOCKER_CONFIG 必须落回仓库内
make check                # fmt + vet + 单测
make test-race
make cover
make e2e
make e2e-docker
make vendor               # 刷新 vendor/
```

### 9.2 代码结构

```text
cmd/rms/             CLI：serve / healthcheck / htpasswd / config validate / version
internal/config/     配置解析与严格校验（含 ${VAR:-default} 值展开）
internal/namespace/  请求路径 → (namespace, 引擎路径) 的唯一真相
internal/registryapp/ 一个 namespace 一个 distribution 引擎（含只读降级引擎）
internal/registry/   引擎生命周期、上游选举、故障转移重建、粘性引擎
internal/upstream/   endpoint 健康探测与选举
internal/auth/       htpasswd 校验 + 按 namespace 的推送 ACL
internal/server/     监听装配、dispatcher（含选仓接线）、catalog、健康端点
internal/selection/  无限定请求的并发探测与选仓（缓存、失效、重选）
internal/storage/    distribution 磁盘布局的只读查询（catalog、缓存命中判定）
internal/obs/        日志、Prometheus 指标、OTLP traces
internal/httpx/      出网 transport（代理、超时、CA）
test/distbehavior/   用测试固定住依赖的 distribution 上游行为
test/e2e/            真实客户端端到端
```

### 9.3 测试与覆盖率

`make cover` 实测（2026-10-15）：

| 包 | 覆盖率 | 包 | 覆盖率 |
|---|---|---|---|
| `registryapp` | 100.0% | `namespace` | 79.6% |
| `upstream` | 93.9% | `app` | 67.5% |
| `auth` | 93.0% | `server` | 51.6% |
| `selection` | 89.8% | `registry` | 39.3% |
| `config` | 87.2% | 合计 | 68.6% |
| `obs` | 84.7% | `cmd/rms`、`httpx` | 无单测 |
| `storage` | 82.5% | | |

dispatcher 的请求路径与引擎生命周期主要由 `test/e2e` 覆盖（它们需要活着的引擎与上游）。
改了代码请重跑 `make cover` 更新本表。

`test/distbehavior` 值得特别一提：它把 rms 依赖的 4 条 distribution 行为写成可执行文档（`HTTP.Host` 的 path
会被丢弃、上传 `Location` 由仓库名拼出、上游不可达时 `handlers.NewApp` 会 panic、proxy namespace 拒绝上传）。
升级 distribution 时这些会立刻变红，而不是让推送地址悄悄错掉。

---

## 附录 A 设计取舍与冲突汇总

| # | 冲突/风险 | 处置 |
|---|---|---|
| C1 | 需求 2（push）与需求 1/3/4（proxy 缓存）在同一 namespace 空间冲突 | 路径里没有可写 namespace 的 `push` 不会落到任何 proxy namespace，直接 `403 DENIED` 并提示应推到哪（旧版是「落到默认仓库再拒」，现在没有默认仓库这一步） |
| C2 | `me` 既是用户名又可能是路径首段 | 显式配置的 shared namespace **优先于**同名用户；要拉 Hub 上的同名仓库请写全 `docker.io/...` |
| C3 | 官方镜像需注入 `library/` | `official_library: true` 时对单段仓库注入（与 crproxy 一致） |
| C4 | 路径前缀形态无法被旧式 `registries.yaml` 当透明镜像（endpoint 不能带路径） | 端口绑定形态已删除；透明镜像改用 `hosts.toml + override_path`，Hub 之外的仓库用「镜像名带 namespace」 |
| C5 | `registry-mirrors` 只对 Docker Hub 生效 | 非 Hub 仓库统一用「镜像名带 namespace」或 containerd 侧 `hosts.toml` |
| C6 | 多 endpoint 故障转移无法复用 distribution proxy 包 | 改成 namespace 级选举 + 重建引擎，并明确它不是请求级重试；2026-10-13 起进一步改为并发竞速（§4.4） |
| C7 | proxy 模式不能 push（实测 `405 UNSUPPORTED`） | 放弃「crproxy + registry:2 proxy」架构，改为按 namespace 分离可写/只读引擎 |
| C8 | 跨 namespace 不去重 | 这是「按仓库点缓存」的直接代价，换来按 namespace 独立计量/清理/迁移 |
| C9 | 上游可达性依赖宿主 | §6.6 的可达性矩阵 + 两套配置 |
| C10 | 可写 namespace 路径里 namespace 出现两次 | 保留前缀是为让引擎返回的 `Location` 正确，见 §4.6 |
| C11 | tag 请求无法完全离线（除非上游挂掉） | distribution 行为：tag 优先回源、失败回落本地。层数据本地读，这就是加速的主要来源 |
| C12 | `/v2/` 匿名 200 会同时破坏 login 校验与 push | 改为总是挑战；匿名读策略下沉到 namespace 级 |
| C13 | docker 的 `registry-mirrors` 不带凭据，被 C12 的挑战挡在门外（实测 dockerd 报 `no basic auth credentials` 后转投官方仓库，缓存被整体绕过） | 端口与 `anonymous_root` 都已删除，改用主端点 `:5000` + 实测确认缓存命中（§6.6）：**镜像源那条路只要内容匿名就没事；显式主机名那条路必须 `docker login`，或在服务端关掉认证**（2026-10-13 实测，Basic 无匿名换令牌流程） |
| C14 | 无限定请求要并发探测，未命中缓存时得等最慢的候选 | 接受：上限是 `probe_timeout`（默认 10s），实测中位约 6s；要零开销就写显式 namespace 前缀（§4.3） |
| C15 | 「无限定名 + 裸 `host:port`」无法表达「我要 ghcr.io 上的那个」 | 只对 Docker Hub 可靠（只有 `docker.io` 有 `library/*`）；其它上游用镜像名前缀或 `hosts.toml`，否则可能被挑到别的 namespace 而 `404`（§6.6） |

---

## 附录 B 变更史与实测记录

按时间倒序。每条的详细读数在 `reports/` 与 `deploy/*/README.md` 里。

### 2026-10-15 · README 重构（本次）

文档重构：正文改为「概念 / 请求生命周期 / 完整配置参考 / 客户端 / 部署 / 运维 / 开发」，全部配置键与默认值
补全（原「关键项」表只覆盖约三分之一），历史与实测收进本附录。**代码与配置未改动。**
追加：请求工作流程图（含从客户端到各上游 namespace 的扇出）与 [§4.8 并发模型](#48-并发模型)
（并发宽度 = 候选对数、无 singleflight、`decision_deadline` 收口）。
顺带修正一处文档错误：`/v2/_catalog` 列出的是**可写 namespace** 的仓库，不含 proxy 缓存 namespace
（依据 `internal/server/catalog.go` 的 `catalogNamespaces`）。

### 2026-10-14 · 103 与 172 关闭认证（`auth.type: none`），172 端口 5003 → 5000

**起因**：把 `anonymous_pull_proxy` 与 `anonymous_pull_local` 都设成 `true` 之后，classic dockerd
（172 本机的 28.2.2、103 的 27.3.1）显式主机名 `docker pull <host>:<port>/<ns>/<repo>` 仍报
`no basic auth credentials`，rms 侧 0 个内容请求 —— 那两个开关只管内容路径，而 classic 客户端走不到内容路径。

**试过的三个方案**（在 172 的隔离实例上做：独立存储根、端口 5001/5002/5003、同一份镜像
`9417526-dirty-20261013T142913Z`；测完容器、镜像、临时目录全部删除）：

| 方案 | classic dockerd 匿名 pull | classic dockerd push | containerd 系（docker 29 + containerd 镜像存储） |
|---|---|---|---|
| 原样：`htpasswd` + 两个 `anonymous_pull_*` | ❌ `no basic auth credentials` | ✅ 登录后可用 | pull ✅ 匿名 / push ✅ 登录后 |
| `/v2/` 对无凭据请求回 200（原型开关，改 3 处 Go 代码） | ✅ | ❌ `unauthorized`，**凭据正确也一样** | pull ✅ / push ✅（登录后）；无凭据 push ❌ 401 |
| **`auth.type: none`（本次采用）** | ✅ | ✅ 零凭据直接推 | ✅ |

- 原型开关的机制：classic dockerd 的凭据是**被挑战驱动**的 —— `/v2/` 不回 401，它就永远不带
  `Authorization`；而写请求拿到 401 后**不重试**（实测两层，有正确凭据也 `unauthorized`）。所以 Basic 方案下
  `/v2/` 只能二选一：挑战 ⇒ 匿名 pull 做不到；不挑战 ⇒ push 废掉。Bearer 端点（Harbor / Docker Hub 那套按
  scope 签发 token）能同时满足，本次运维不要，故未实现；原型的代码改动已还原，没有留在仓库里。
- containerd 系客户端（docker 29 默认 containerd 镜像存储、nerdctl、ctr、k3s）本来就能匿名 pull，
  本次改动照顾的是 classic 客户端。

**实际改动与上线读数**：

- `deploy/103/rms.yaml`：`auth.type` → `none`，`user_namespace` 必须同时为 `false`（校验拒绝
  `user_namespace: true` + `type: none`）；`me` / `shared` 由 `shared_namespaces` 继续定义为可写本地
  namespace，发布路径 `rms.majorc.eu.org/me/rms` 不变。备份为 `config/rms.yaml.bak-20261014-151103`。
- `deploy/172/rms.yaml`：`auth.type` → `none`；`deploy/172/.env` 的 `RMS_PORT_PRIMARY` 由 5003 改为 5000
  （两个端口本来都在各客户端 `insecure-registries` 里，换端口不需要动客户端 `daemon.json`）。
- 读数：`curl http://10.82.64.172:5000/v2/`（零凭据）→ **200**（原来 401）；带错凭据也是 200
  （认证关闭后不再校验，`docker login` 因此失去意义）。`docker pull 10.82.64.172:5000/docker.io/library/alpine`
  （classic dockerd 28.2.2，零凭据）✅；`docker push 10.82.64.172:5000/ops/<镜像>`（零凭据）✅。
  `/healthz` 200、`/readyz` ready、容器 healthy。
- 代价与回滚见 [`deploy/103/README.md`](deploy/103/README.md) 与 [`deploy/172/README.md`](deploy/172/README.md)。

### 2026-10-13 · 单端点化：删除按上游绑定的端口与 `anonymous_root`

两件事被彻底删除，**旧配置与旧客户端配置都要改**：

1. **`namespaces.default` 删除** —— 无限定请求不再改写成一个固定仓库，改为并发探测所有 proxy namespace。
2. **`listeners` 与 `anonymous_root` 整体删除** —— 15000–15006 那七个按 namespace 绑定的端口下线，
   只剩 `5000` 与 `9100`。

**为什么删**：端口数量随上游增长（每加一个上游就要改 rms 配置 + 客户端配置 + 容器端口映射）；端口绑定
namespace 与「无限定请求自动选仓」是**互相矛盾**的两种语义；它还拖着一个额外开关 `anonymous_root`，
只为迁就 dockerd 的请求不带凭据。

被删的 `anonymous_root` 当年是为了绕开这个问题：早期版本里 docker 的 `registry-mirrors` 端点**不带凭据**，
而那时的 rms 连**内容路径**也回 401 挑战，dockerd 于是放弃这个镜像源、转去官方仓库 —— **拉取看起来正常，
缓存却完全没走 rms**。当年的实测日志原文：

```text
level=info msg="Attempting next endpoint for pull after error:
  Head \"http://<rms>/v2/library/alpine/manifests/3.16\": no basic auth credentials"
```

这条未决项**现已实测收口**（见 §6.6 ①）：`/v2/` 回 401 无害，内容路径匿名可读就能走缓存。

### 2026-10-13 · 上游并发选路 + 同 digest 负载均衡上线

`namespaces.proxy.<ns>.upstreams` 的顺序语义从「优先级」变为 **digest 权威顺序**：所有上游并发探测，
最快的服务，同 digest 的按 1/延迟加权分担。`deploy/103/rms.yaml` 与 `deploy/172/rms.yaml` 加了
`decision_deadline: 3s` / `balance: weighted` / `balance_window: 1.5`。

- 10:55Z 上线：现网镜像 `rms:dev` = `rms:race-20261013-184731`（旧镜像保留为
  `rms:rollback-20261013-184640`），切换后 31s healthy、`restarts=0`；101 匿名经 `registry-mirrors` 拉
  `busybox:1.36.1` 成功且缓存计数增长。
- 算法、指标与取舍见 §4.4；完整验收读数与回滚步骤见 `reports/rms-upstream-race.md` §8.3/§8.4。
- ⚠️ 同期发现：103 的现网配置曾把 `docker.elastic.co` 的上游写成裸主机名（无 scheme），**新旧二进制都会在
  启动时拒绝** —— 现役容器只是没重启过。已修复；改配置后务必**先验证再重启**：

  ```bash
  docker run --rm -v /opt/stacks/rms/config:/etc/rms:ro --entrypoint /usr/local/bin/rms \
    rms.majorc.eu.org/me/rms:latest config validate --config /etc/rms/rms.yaml
  ```

### 2026-10-13 · 172 上线（端口 5003）

- 容器 `rms.majorc.eu.org/me/rms:9417526-dirty-20261013T142913Z`，healthy、`restarts=0`；
  `rms version` 与 103 完全一致。之前那套公司内网的 `registry:2`（含 `registry-ui`）整体退役。
- `GET /healthz` → 200；`GET /readyz` → `ready`（补上官方上游前是 `degraded namespaces: nvcr.io`）。
- 私仓端到端（curl，`admin`）：blob 201 / manifest 201 / GET blob 200 且逐字节一致 / `tags/list` 与
  `_catalog` 正常 / 匿名 401 / `DELETE` manifest 202。
- 客户端实测：`docker login ... -u admin` 之后显式主机名 pull 成功；**匿名显式主机名拉取失败**
  （`no basic auth credentials`），103 上同样复现 —— 这一条直接导致了次日的 `auth.type: none`。

### 2026-10-13 · 103 改为 pull-only

`deploy/103/install.sh` 不再把仓库 rsync 到 103、也不在那台机器上构建：
**101 构建 → push → 103 `docker compose pull` → `up -d` → 清旧镜像**，入口是
`make push` / `make deploy-103` / `make update-103`。`/opt/stacks/rms` 里删掉了源码与构建产物，
`deploy/103/docker-compose.yml` 去掉 `build:` 段。清理动作在每次 install/update 里幂等重跑。

### 2026-10-13 之前 · 接管 103 的 8 个 `registry:2` 容器

`192.168.188.103` 原先跑 8 个 `registry:2` 容器（15000–15006 各代理一个上游，15008 是私有仓库）。
现在一个 rms 进程接管前者，缓存目录用 `mv` 改名迁移（同一文件系统内，不复制数据），映射关系与
「15008 保持不动」的说明见 [`deploy/103/README.md`](deploy/103/README.md) 与 `reports/103-rms-deploy.md`。

---

## 附录 C 未实现与已知遗留

**未实现（按需再加）**：Bearer token 服务端、限流/带宽控制、镜像白名单/黑名单、S3 等对象存储后端、
LRU/按容量清理、单 namespace 级配额。

**已知遗留**：

- `15008` 私有仓库仍由旧的 `registry:2` 提供（rms 的 local namespace 可以替代，但客户端要改成
  `docker login`，且没有 CORS 头），未迁移。
- `config/htpasswd` 是仓库里的**演示凭据**（alice/bob/me/carol/admin）。103 的现网 `config/htpasswd` 就是
  这一份；两台机器 2026-10-14 起是 `auth.type: none`，它只用于回滚。回滚前应先在 101 跑
  `make htpasswd USER=admin PASS=...` 换掉密码（`install.sh` **不会**覆盖已存在的 htpasswd）。
- `5002/5003`（内网镜像主机自己的端口）从 101 经 tun_sase 不可达；客户端 `registry-mirrors` 里留着它们
  只会每次多一次失败尝试。
- 172 的 `daemon.json` 的 `registry-mirrors` 仍挂着 `[5000, 5001, 5002, 5003]`，后三个端口已无人监听，
  会让无限定名拉取回落官方并超时；建议改成只留 `http://10.82.64.172:5000`。
- 172 root 的 `~/.docker/config.json` 里 `10.82.64.172:5000` 仍是过期密码（`auth.type: none` 后不生效）。
- 若把**另一台开启 htpasswd 的 rms**（例如 `10.82.64.172:5000`）配成某个 namespace 的上游，必须同时配
  `username`/`password`，否则症状是「探测说存在、取内容 404」（§4.4，
  `reports/raw/101-live-upstream-race.txt` §E）。

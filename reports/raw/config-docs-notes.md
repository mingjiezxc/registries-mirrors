# task-3 记录：配置 / 文档 / e2e（删 default 与独立端口）

写作用域：`config/**`、`deploy/103/**`、`README.md`、`test/**`、`reports/raw/config-docs-notes.md`。
未触碰：`internal/**`、`cmd/**`（另两条支线）；未触碰 192.168.188.103 上的任何容器或在线配置。

---

## 1. 配置字段口径（新增 / 删除）

### 新增 `namespaces.selection`

| 键 | 类型 | 默认 | 口径 |
|---|---|---|---|
| `namespaces.selection.probe_ttl` | duration | `30s` | 同一个 (仓库, tag/digest) 的决策复用多久。**同时也是仓库亲和窗口**：blob 请求路径里没有仓库名，它复用同仓库 manifest 的决策，随这个窗口一起过期。 |
| `namespaces.selection.probe_timeout` | duration | `10s` | 单个候选探测的上限，**也是未命中缓存时请求等待决策的上限**。 |

两者在 `internal/config/selection.go` 里定义，`withDefaults()` 填空值；`cmd/rms config validate` 现在会打印
`selection probe_ttl: 30s` / `selection probe_timeout: 10s`（实测见下）。

**写清楚的边界**：`probe_timeout` 不是"每个候选各 10s 的总额度上限"，而是每次探测的上限；一次无限定请求的
墙钟上界约等于"最慢候选的探测耗时"，实测 6 个真实上游并发探测的墙钟中位数约 6s、最坏打满 10s
（见 `reports/raw/selection-real-upstream.txt` 第三节）。

### 删除

| 键 | 原因 |
|---|---|
| `namespaces.default` | 无限定请求不再改写成一个固定仓库；改为并发探测所有 proxy namespace。 |
| `listeners`（整个块） | 按 namespace 绑定的独立端口整体下线。 |
| `listeners[].anonymous_root` | 随端口一起删；没有"绑定到代理 namespace 的端口"这个概念了。 |

严格解码（`KnownFields`）意味着**旧配置带这些键会直接启动失败**，这是预期行为而不是兼容性 bug：
README §2.1 与 `deploy/103/README.md` 都写明了迁移动作。

## 2. 校验读数

```console
$ RMS_HTPASSWD=$PWD/config/htpasswd go run ./cmd/rms config validate --config config/rms.yaml
... ok
$ RMS_HTPASSWD=$PWD/config/htpasswd go run ./cmd/rms config validate --config deploy/103/rms.yaml
deploy/103/rms.yaml: ok
  primary listener: 0.0.0.0:5000
  storage root: /data
  selection probe_ttl: 30s
  selection probe_timeout: 10s
  proxy docker.io        upstreams=https://docker.1ms.run,https://registry-1.docker.io
  proxy gcr.io           upstreams=https://gcr.io
  proxy ghcr.io          upstreams=https://ghcr.io
  proxy quay.io          upstreams=https://quay.io
  proxy registry.k8s.io  upstreams=https://registry.k8s.io
```

（`storage root: /data` 是因为没设 `RMS_STORAGE_ROOT`，与本次改动无关。）

## 3. 逐条核对的注释/配置漂移（negatives 提过的那类问题）

改了配置却没改注释，这次逐条核对并修正：

| 位置 | 原来写的 | 实际 | 处置 |
|---|---|---|---|
| `deploy/103/.env` 文件头 | "**没有任何 `*_PROXY` 变量**" | 同一个文件第 35 行就有 `RMS_UPSTREAM_PROXY=http://192.168.188.1:3128` | 改成"上游出网走局域网代理 `RMS_UPSTREAM_PROXY`（见文件末尾）"，并保留"公司内网镜像在 .103 上不可达"那句 |
| `deploy/103/docker-compose.yml` 顶部 | "15000-15002 …（上游顺序见 rms.yaml：**daocloud** → 1ms → 官方）" | `rms.yaml` 早已是 `1ms → 官方`，daocloud 在 2026-10-12 的实测后被弃用 | 整段重写，不再列上游顺序，改为指向 `rms.yaml` 的注释 |
| `deploy/103/README.md` 目标架构表 | "docker.io → **daocloud** → 1ms → 官方" | 同上 | 改成 `1ms → 官方` |
| `deploy/103/README.md` 客户端段 | "客户端配置不需要改" + 15000 系列示例 | 端口已删 | 改成"必须改"+ 新示例 + 迁移表 |
| `deploy/103/README.md` 配置校验输出 | 打印了 7 个 listener 与 `default namespace:` | CLI 已不打印这些 | 换成实际输出（含 selection 两行） |
| `deploy/103/README.md` 反向用例 | `anonymous_root requires a proxy namespace` 等两条 | 该选项已删，校验分支也没了 | 整段删除 |
| `README.md` §1 快速开始 | "`:5001`/`:5002`（按 upstream 绑定的镜像端口）" | 已删 | 改成 `:5000` + `:9100` |
| `README.md` §12 代码结构 | `internal/server/ 多监听器装配` | 单端点 | 改成"监听装配、dispatcher（含选仓接线）"，并补 `internal/selection/` |
| 根 `docker-compose.yml` 第 33-36 行 | "Namespace-bound mirrors for containerd registries.yaml / docker registry-mirrors" + `"${RMS_PORT_DOCKERIO:-5001}:5001"`、`"${RMS_PORT_GHCR:-5002}:5002"` | 该特性已删除；101 上实测容器仍发布着 `0.0.0.0:5000-5002` | 删两行映射与那段注释；顶部说明改为与 103 一致的单端点口径 |
| 根 `.env.example` 第 16-17 行 | `RMS_PORT_DOCKERIO=5001`、`RMS_PORT_GHCR=5002` | 同上 | 删除，只留 `RMS_PORT_PRIMARY` / `RMS_PORT_METRICS` |

## 4. 客户端迁移清单（旧 → 新）

| 客户端 | 旧写法 | 新写法 | 备注 |
|---|---|---|---|
| docker `daemon.json` | `registry-mirrors: [...:15000, ...:15001, ...:15002]` + `insecure-registries` 同上一串 | `registry-mirrors: ["http://<host>:5000"]`，`insecure-registries: ["<host>:5000"]` | `registry-mirrors` 只对 Docker Hub 生效；非 Hub 用镜像名前缀 |
| containerd `registries.yaml` | `ghcr.io: endpoint: [...:15004]` | 只留 `docker.io: endpoint: ["http://<host>:5000"]`；其它上游用镜像名前缀 | 旧式 `registries.yaml` 无 `override_path`，endpoint 不能带路径 |
| containerd `hosts.toml` | `[host."http://<host>:5001"]` | `[host."http://<host>:5000/v2/<namespace>"]` + `override_path = true` | 这是唯一的"给某个上游做透明镜像"办法 |
| 镜像名 | `<host>:5000/<user>/<image>`（旧：被改写成 docker.io） | 路径里没有可写 namespace 时不再改写；显式写 `<host>:5000/docker.io/<user>/<image>` 或让无限定请求去挑 | 见 README §4.1 |

**新注册的事实边界**（README §2.1 里那条）：镜像源指 `:5000` 时，docker 拉 Docker Hub 的 `alpine` 这类
无限定名**仍能命中**（无限定请求会挑到 `docker.io`，因为只有它有 `library/alpine`）；但 `ghcr.io` 等其它上游的
无限定名**可能被挑到别的 namespace，或谁都对不上而返回 404 NAME_UNKNOWN**。依据：task-4 对 6 个真实上游的
实测里，`library/alpine` 只在 docker.io 系的两个候选上 exists，ghcr.io/gcr.io/quay.io 判 401、registry.k8s.io 判 404
（`reports/raw/selection-real-upstream.txt` 第二节）。

## 4.5 根目录部署形态的残留（独立评估者发现，2026-10-13 补）

前一轮只改了 `deploy/103/**`，**漏了仓库根目录的部署文件** —— 也就是说「移除按 namespace 绑定的
独立端口」在根配置上并没有完成（103 完成了，根没有）。评估者的观察是对的：101 上运行的 rms 容器
当时确实还发布着 `0.0.0.0:5000-5002`。

| 文件 | 残留 | 处置 |
|---|---|---|
| `docker-compose.yml` | 第 33-36 行：一段 "Namespace-bound mirrors for containerd registries.yaml / docker registry-mirrors" 注释 + `"${RMS_PORT_DOCKERIO:-5001}:5001"`、`"${RMS_PORT_GHCR:-5002}:5002"` 两条映射 | 删除两条映射与那段注释；顶部说明改成与 103 一致的口径（单端点 + 无限定请求自动选仓），并写明**只把端口加回 compose 是不够的**（配置里已经没有能把端口绑定到 namespace 的键，容器起来了也无处可路由） |
| `.env.example` | 第 16-17 行 `RMS_PORT_DOCKERIO=5001`、`RMS_PORT_GHCR=5002` | 删除，只留 `RMS_PORT_PRIMARY` / `RMS_PORT_METRICS` |
| `Makefile` | 无端口清单（`make up` 只执行 `docker compose up -d --build`，不打印端口） | 无需改动（已 grep 确认） |
| `README.md` §1 | 上一轮已改成"只有 `:5000` 与 `:9100`" | 已在位 |
| `README.md` §9.1 | 没有说明根 compose 与 103 compose 是否同形态 | 补一句：两者都是**单端点**形态，不会分叉；并说明 `RMS_PORT_DOCKERIO`/`RMS_PORT_GHCR` 已不存在 |
| `deploy/103/README.md` | 同上 | 补一句：与仓库根目录的部署形态一致 |

**未做的一件事（刻意）**：没有重启/重建 101 上正在运行的容器。文件改完了，但那个容器要等一次
`docker compose up -d` 才会只发布 5000/9100 —— 这是部署动作，按分工由 Lead 决定时机。
本次只改仓库文件，没有碰任何在线配置。

## 4.6 e2e 端口分配的 flake（2026-10-13 补，独立评估者提出）

**现象**（router-dev、selection-dev 各撞到一次，Lead 也见过一次）：

```
--- FAIL: TestDegradedServesCache
    observability.metrics.listen: must differ from server.listen
```

**根因**：`test/e2e/e2e_test.go` 原来的 `freePort(t)` 是
`net.Listen("tcp","127.0.0.1:0")` → **立刻 Close** → 返回端口号。内核可以把刚释放的临时端口
直接还给下一次调用，于是相隔两次调用（primary 与 metrics）可能拿到同一个端口，配置校验随即报
「两个监听不能相同」。这是概率性 flake，不是回归。

**实测（不是理论推断）**，用一次性诊断程序 `.probe/selection/portrace/main.go`：

| 量 | 读数 |
|---|---|
| 旧分配器连续两次调用撞车 | **2 / 20000 对（0.010%）** |
| 用新用例的同款断言（20 次成对预留）跑旧分配器 | **40 / 500 轮失败（8.00%）** |
| 修好后 `go test -tags e2e -count=5` | ok（30.1s） |
| 修好后 `go test -tags e2e -count=10` | ok（52.2s） |

第二行是这次修复的关键：**新断言抓旧 bug 的概率约 8%，而 flake 在一次普通全量运行里现身的概率
只有约 0.15%** —— 所以「跑 5 遍都绿」本身并不足以证明修好了，必须把性质直接钉住。

**修法**（保留全部断言语义，没有放宽任何一条）：
- 新增 `reservePorts(t, n)`：成批预留 n 个端口，**在整批选完之前保持所有监听器打开** ——
  端口只要还被占着，内核就不会把它再分配给第二次预留，因此结果是**确定性不重复**而不是
  「通常不重复」。选完后统一关闭，调用方即可绑定（这是基于地址预留不可避免的一小段 TOCTOU，
  比原来小几个数量级）。
- 进程级 `issuedPorts` 集合：本进程发放过的端口永不二次发放，`deadPort()`（故意无人监听的端口）
  也走同一条路径。
- `startRMSOpts` 改为一次 `reservePorts(t, 2)` 取 primary 与 metrics，从根上消除两次独立调用
  之间撞车的可能。
- 新增 `TestReservePortsNeverRepeat`：确定性断言「本进程内端口不重复」且「预留后端口可绑定」，
  把这条性质变成常驻回归护栏。

## 5. e2e 改动与结果

`test/e2e/e2e_test.go`：

- `rmsInstance.docker`（namespace 绑定端口的地址）**删除**；`startRMS` / `startRMSWithData` 收敛到新的
  `startRMSOpts(t, rmsOptions{...})`，配置模板里**没有** `listeners` 与 `namespaces.default`，改为两个
  proxy namespace（`docker.io` 带 `official_library`、`ghcr.io` 不带），各自指向传入的假上游；
  模板里显式写 `namespaces.selection`（`probe_ttl: 30s`、`probe_timeout` 由用例给，默认 2s）。
- 依赖端口绑定形态的断言改为「主端点 + 显式 namespace 路径」，并新增**显式前缀仍直连**的断言：
  请求 `/v2/docker.io/...` 时另一个上游的 hits **一点都不能涨**（反之亦然）。
- 新增 `TestUnqualifiedPicksNamespaceWithContent`：两个假上游分别有 `library/demo:v1` 与 `owner/other:v1`；
  断言 (a) 无限定 `library/demo` 命中**有内容那一侧**且两个候选**都被探测过**，(b) 换一个只存在于另一侧的仓库时
  胜者翻转，(c) 显式前缀不探测另一侧，(d) 不存在的 tag → **404 且 code == NAME_UNKNOWN**。
- 新增 `TestUnqualifiedAllUnreachable`：两个候选都指向**已关闭的端口** → **503 且 code == UNAVAILABLE**。
- 新增 `TestUnqualifiedBlobFollowsTheManifest`（无限定 blob 路径，2026-10-13 补）：manifest 先建立
  仓库亲和，再无限定 `GET /v2/blobs/<configDigest>`；digest 从**响应体**里解析（不信 fixture 常量），
  断言 200 + 内容等于 fixture config blob + **只有服务过该仓库的那一侧 hits 增加**。
- 新增 `TestUnqualifiedBlobWithUnknownDigestIsUnavailable`：两边都没有的 digest（`sha256:` + 64 个 0）
  → **503 UNAVAILABLE**（不知道 digest 归谁 ≠ 内容不存在）。
- 端口分配改为成批预留（`reservePorts`）+ 进程级 `issuedPorts`，并新增 `TestReservePortsNeverRepeat`
  钉住「本进程内端口不重复」这条性质；§4.6 有 flake 的实测撞车率与修法。
- 保持 `//go:build e2e`、离线、不需要 docker daemon。

实测结果（`go test -tags e2e -count=1 -v ./test/e2e/...`）：

**最终结果：全绿。**

```
--- PASS: TestLoginContract (0.51s)
--- PASS: TestPullThroughAndCache (0.31s)                    4/4 子测试
--- PASS: TestUnqualifiedPicksNamespaceWithContent (0.31s)   4/4 子测试
--- PASS: TestUnqualifiedAllUnreachable (0.28s)
--- PASS: TestUnqualifiedBlobFollowsTheManifest (0.31s)
--- PASS: TestUnqualifiedBlobWithUnknownDigestIsUnavailable (0.29s)
--- PASS: TestPushFlow (0.93s)
--- PASS: TestAuthorisationMatrix (0.93s)                    10/10 子测试
--- PASS: TestDegradedServesCache (0.69s)
--- SKIP: TestDockerClient                                    (需要 RMS_E2E_DOCKER=1)
ok  	github.com/watsons/rms/test/e2e	4.572s
```

这个用例集在本次补测过程中先后抓到两个**被测代码的真实缺陷**（缺陷 A 已修、缺陷 B 已修），
原始失败输出与定位都保留在下面 —— 它们是这两个修复的证据，不是测试自身的毛病。

### 缺陷 A（已修）：探测请求缺 manifest Accept —— 曾被这 2 个子测试抓到

**2026-10-13 更新：已修，这 2 个子测试现在全绿。** 原始失败与读数保留如下（它是触发修复的证据）。

原失败原文（假上游日志）：

```
"err.message":"OCI manifest found, but accept header does not support OCI manifests",
"http.request.useragent":"rms/selection-probe","http.response.status":404
```

**原因（修复前）**：`internal/selection/probe.go` 构造探测 HEAD 时只设了 User-Agent，没有设 manifest 的
Accept；405/501 降级那条设的是 `Accept: application/json`，不是 manifest 的 media type。
**修复后**：该文件新增常量 `manifestAccept`（4 个 media type 一次列出）并在探测请求上设置
（`probe.go:51` 与 `probe.go:312`），注释里也记了同一组实测读数。

后果在真实上游上实测到了（同一时刻、同一个存在的 tag）：

| 上游 / 仓库 | 不带 Accept | 带 4 个 manifest media type |
|---|---|---|
| ghcr.io / astral-sh/uv:latest | **404** | 200 |
| registry-1.docker.io / library/alpine:latest | 200 | 200 |
| quay.io / prometheus/busybox:latest | 200 | 200 |
| docker.1ms.run / library/alpine:latest | 200 | 200 |

即 Docker Hub 与 quay.io 宽容、**ghcr.io 不宽容**。因为 `internal/selection` 先查本地缓存再探测
（`cachedOnDisk`），**热缓存不受影响**——只有冷缓存（第一次拉某个镜像）会把 ghcr.io 上存在的镜像判成 absent，
轻则 `404 NAME_UNKNOWN`（把"存在"说成"不存在"，客户端会缓存这个结论），重则在同名仓库存在于别的 namespace 时
**静默换源**。这也解释了为什么同一批里 `TestPullThroughAndCache` 的无限定子测试是绿的：它的显式 namespace
子测试先跑，已经把缓存捂热了。

**处置**：不为了让测试变绿而回退设计，也不改测试去迁就缺陷——用例保留「存在的镜像必须 200」这个正确语义。
已把完整读数与复现命令发给 Lead 转给 `internal/selection` 的所有者（修法：HEAD 与 GET 降级都带上
`application/vnd.docker.distribution.manifest.v2+json`、`...manifest.list.v2+json`、
`application/vnd.oci.image.manifest.v1+json`、`application/vnd.oci.image.index.v1+json`）。
同一条读数已补进 `reports/raw/selection-real-upstream.txt` 第八节；该节现在还有**全候选 × 全目标**的
「带 Accept / 不带 Accept」对照矩阵（24 格只有 2 格翻转，就是 ghcr.io 与 gcr.io 的 native 组）。

### 缺陷 B（已修）：无限定 blob 的引擎路径丢掉了仓库名

`TestUnqualifiedBlobFollowsTheManifest` 的失败原文：

```
e2e_test.go:781: unqualified blob after a manifest: got 404, want 200 (404 page not found)
```

判据：rms 自己的错误都是 JSON 包络（`writeError` → `{"errors":[{"code":…}]}`），而这里是 Go http 的
默认文案 `404 page not found` —— 说明这个 404 是**后面的 distribution 引擎**回的，即交给引擎的路径不对。

根因在 `internal/server/dispatch.go:323`（`applyBlobTarget`）：

```go
res.EnginePath = "/v2/blobs/" + digest          // 丢掉了仓库名
```

distribution 引擎只认 `/v2/<repo>/blobs/<digest>`。而亲和分支**本来就带着仓库名**：`Chooser.DecideBlob`
的 affinity 命中返回 `c.recent.target`（上一次 manifest 决策的 `Target`，其 `EnginePath` 就是
`library/demo` 这样的仓库级路径，见 `internal/selection/probe.go:498` 与 `chooser.go:68-76`）。
同一份代码的注释（`dispatch.go:289-292`）已经写明"引擎需要 `/v2/<repo>/blobs/<digest>`，没有仓库名会被拒"，
只是这一行没有照做。建议修法：`res.EnginePath = "/v2/" + target.EnginePath + "/blobs/" + digest`。

**2026-10-13 更新：已修，`TestUnqualifiedBlobFollowsTheManifest` 现在 PASS。** 修复方同时补了磁盘命中分支
（`DecideBlob` 能按 digest 反查仓库名），所以上面那条"磁盘命中但不知道仓库名"的悬空态也有了着落；
`TestUnqualifiedBlobWithUnknownDigestIsUnavailable` 在修复前后都保持 PASS（503 + UNAVAILABLE 口径未变）。

## 6. 验收命令与结果

```
$ grep -rn "1500[0-6]" config deploy README.md | sed 's/:.*//' | sort | uniq -c
     12 deploy/103/README.md

$ grep -rn "5001\|5002" docker-compose.yml .env.example README.md Makefile
docker-compose.yml:16    # (docker.io -> :5001, ghcr.io -> :5002, ...) is gone ...   ← 历史说明
README.md:134            ...旧的 `ghcr.io: endpoint: [...:5002]` 已失效...            ← 迁移说明
README.md:188            `10.82.64.172:5000/5001/5002`（内网镜像主机自己的端口…）    ← 别的宿主
README.md:203            - "http://10.82.64.172:5001"                                ← 别的宿主
README.md:380            早先的部署把 `:5001` 绑到 `docker.io`…                     ← 历史说明

$ docker compose config          # 通过；ports 实际渲染为
    ['${RMS_PORT_PRIMARY:-5000}:5000', '${RMS_PORT_METRICS:-9100}:9100']
$ make check                     # fmt + vet + test 全过

$ go test -tags e2e -count=5 ./test/e2e/...
ok  	github.com/watsons/rms/test/e2e	30.123s
$ go test -tags e2e -count=10 ./test/e2e/...
ok  	github.com/watsons/rms/test/e2e	52.218s
```

只有 `deploy/103/README.md` 命中（12 处，全在「现状」表、「2026-10-13 单端点化」迁移段与「回滚」段）。
`config/**`、`README.md`、`deploy/103/docker-compose.yml`、`deploy/103/.env`、`deploy/103/rms.yaml` 里
**已经没有端口号**——那些地方改成「按上游绑定的端口」并指向 `deploy/103/README.md`，
避免同一组数字散落在多处再次漂移。

```
$ go build ./... && go vet ./... && go vet -tags e2e ./test/e2e/     # 全部通过
$ go test -count=1 ./...                                             # 全绿(12 个包)
ok  github.com/watsons/rms/internal/app / auth / config / namespace / obs /
    registry / registryapp / selection / server / storage / upstream / test/distbehavior
$ go test -tags e2e -count=1 ./test/e2e/...
--- FAIL: TestUnqualifiedPicksNamespaceWithContent   ← 2 个子测试，原因见 §5（被测代码缺陷，非本次改动）
FAIL
```

## 7. 遗留

- ~~`go test -tags e2e` 未全绿~~：**已全绿**。补测过程中抓到的两个被测代码缺陷（Accept 头、blob 引擎路径）
  都由各自的支线修复，e2e 10 个顶层用例 28 个子测试全部 PASS（§5）。
- README §2.1 与 `deploy/103/README.md` 里「dockerd 在 `anonymous_root` 删除后是否仍吃 `registry-mirrors`
  的缓存」是**未实测项**，两处都显式写成待测而不是结论。本环境没有可安全重启的 docker daemon，
  没有替它下结论。
- 未跑 `make e2e-docker`（需要 docker CLI 与 daemon，且会真的拉镜像）。
- `deploy/103/README.md` 里的缓存搬迁映射与 15000-15006 表格**刻意保留**：它们是回滚路径的唯一记录，
  迁移完成也不该删。这是 `grep 1500[0-6]` 唯一允许出现的位置。
- 103 上的实际部署未动（本次只改仓库文件）；上线时按 `deploy/103/README.md` 的迁移段执行。
- **101 上正在运行的 rms 容器仍然发布着 5000-5002**：根 `docker-compose.yml` 已改成只发布 5000/9100，
  但要让运行中的容器跟上需要一次 `docker compose up -d`（部署动作，按分工由 Lead 决定时机）。
- `.probe/selection/portrace/main.go` 是一次性诊断程序（量旧分配器的撞车率），为可复现而保留；
  它不是交付物，可以随时删。

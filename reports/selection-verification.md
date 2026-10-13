# 无限定请求并发选仓：集成核验记录

日期：2026-10-13 · 主机：101（构建/测试）、192.168.188.103（上线实测）
复现命令都在文末。

## 结论

四条判断在本阶段全部拿到证据支持，「并发选最快」与「探测并发性」是**可重跑的核算**（单测 + 真实上游实测），「全无则 404」与「删端口不破主流程」在**单测、真引擎端到端、以及 103 现场**三层一致。上线后按原始路径复验：拼错的镜像名 → **404 NAME_UNKNOWN**（五候选全 absent），已缓存镜像与无仓库名的 blob → 200。

## 一、门禁（可重跑，原始输出已落盘）

原始输出两份：`reports/raw/selection-gate-check.txt`（**只有** `make check` 的逐包输出与 `EXIT=0`）与 `reports/raw/selection-gate-race-e2e.txt`（race 的两个包 + e2e 的 `-v` 逐用例输出）。第一节的代码块取自前者，race 与 e2e 的读数取自后者。

```text
$ make check                 # go fmt ./... ; go vet ./... ; go test ./...
?   	github.com/watsons/rms/cmd/rms	[no test files]
ok  	github.com/watsons/rms/internal/app	(cached)
ok  	github.com/watsons/rms/internal/auth	(cached)
ok  	github.com/watsons/rms/internal/config	(cached)
?   	github.com/watsons/rms/internal/httpx	[no test files]
ok  	github.com/watsons/rms/internal/namespace	(cached)
ok  	github.com/watsons/rms/internal/obs	(cached)
ok  	github.com/watsons/rms/internal/registry	(cached)
ok  	github.com/watsons/rms/internal/registryapp	(cached)
ok  	github.com/watsons/rms/internal/selection	(cached)
ok  	github.com/watsons/rms/internal/server	(cached)
ok  	github.com/watsons/rms/internal/storage	(cached)
ok  	github.com/watsons/rms/internal/upstream	(cached)
ok  	github.com/watsons/rms/test/distbehavior	(cached)
EXIT=0
```
（以上逐字取自 `reports/raw/selection-gate-check.txt`，含退出码；`(cached)` 表示该包在同一次 harness 里刚跑过同样的命令、结果被复用，语义仍是「通过」。）

判据 3 的两条硬性质由单测钉住（阈值 50ms，实测远小于阈值）：并发时间差这次复跑为 **247µs**（用例 `TestDecideProbesCandidatesConcurrently`；原文在 `reports/raw/selection-concurrency-unit.txt`：`skew between the two upstreams = 247µs; total Decide = 151.632ms`。该用例不引用行号——`chooser_test.go` 会被持续编辑，行号会漂移。同一条用例在其它轮次测得 5µs / 8µs / 22µs，见 `reports/raw/selection-notes.md`）；只接受 2xx（`TestDecidePicksTheFasterCandidate` 等；另有一条 Accept 回归用例，去掉 Accept 会红）。

## 二、「探测并发性」的真实上游读数（可从原始数据复算）

原始读数：`.probe/selection/raw-proxy.jsonl`、`raw-direct.jsonl`；复算脚本输出：`reports/raw/selection-concurrency-stats.txt`（含逐轮逐候选的 `start_offset_ms` 表）。下表的组数、spread、压缩比、wall/serial 中位逐字取自该文件；最后一行 `start_offset_ms` 范围是同一次复算在整个 jsonl 上取的最小/最大值（本报告第八节的脚本可重建整张表）：

| 读数 | proxy | direct |
| :--- | ---: | ---: |
| 组数（4 组目标 × 3 轮） | 12 | 12 |
| spread 中位 / 最大 | **0.152ms / 0.405ms** | **0.145ms / 0.479ms** |
| 压缩比（serial ÷ wall）中位 | **3.53x**（2.80x–4.68x） | **3.80x**（3.00x–4.25x） |
| wall 中位 / serial 中位 | 4.3s / 15.4s | 10.0s / 37.4s |
| 各候选 `start_offset_ms` 的范围（每组 6 个，共 12 组 72 个值） | **50.164–50.960ms** | **50.191–50.878ms**（逐组数值见 stats 文件的 proxy/direct 两节；同一轮内的极差即上表 spread） |

单测侧的对照更直接，原文落在 `reports/raw/selection-unit-and-config.txt`：本次抓取 `skew between the two upstreams = 3µs; total Decide = 151.003ms (two serial probes would be >= 300ms)`——两个各延迟 150ms 的候选，总耗时等于其中一个而不是两者之和；同一条用例在其它轮次测得 151.091 / 150.995 / 150.827ms（见 `reports/raw/selection-notes.md`）。

## 三、「存在/不存在/不可达」三类判定的判别力（真实上游）

- **存在**：**proxy 组** 6 个候选在「各有其原生仓库」的目标上各 3/3 轮返回 2xx（docker.1ms.run 的 `library/alpine`、ghcr.io 的 `astral-sh/uv`、gcr.io 的 `distroless/static`、quay.io 的 `prometheus/busybox`、registry.k8s.io 的 `pause`、官方 Hub 的 `library/alpine`）；**direct 组**同一组里有两个候选不齐（registry-1.docker.io r1 `ERR @5028ms`、registry.k8s.io 三轮 `ERR`）——直连的稳定性显著更差，这也是「不可达只留给传输失败」这条口径的现实依据。
- **不存在**：同一个仓库名在别的候选上被判 absent。以 proxy 组的 `library/alpine` 为例（源文件 §一.2 与 §二）：docker.1ms.run **200 存在**（3/3 轮）、registry-1.docker.io **200 存在**（3/3 轮，逐轮 200 @2646/2686/3105ms）、ghcr.io/gcr.io/quay.io **401 absent**、registry.k8s.io **404 absent**。（本文件早先把「第 3 轮 1 次 EOF」错记在 registry-1.docker.io 的 proxy/shared 格上；该格三格都是 200，已改正。文件里真实出现的 `ERR` 在 direct 组 native-absent 的另一候选上。）
- **不可达**：传输层错误（超时、`connection reset by peer`、EOF）在实测里真实出现并被单独归类。例如 **direct 组** native-absent 的 registry-1.docker.io：r1 `ERR @10000ms (T200)`、r2 `401 @10000ms (T0)`、r3 `401 @9736ms (T200)`（源文件第 199 行；proxy 组同一格的 r1–r3 是 `401 @3074/3054/2632ms`，见第 158 行——两组的差别正是出网方式）——即该候选在那一组里三次都以超时为界，其中一次是传输失败、两次是拿到 401 后耗尽时间。
- **内容协商对照（`-noaccept` 背靠背两遍）**：两组 × 6 候选 × 4 组目标 × 3 轮 = 144 个观测里有 **6 格**翻转（ghcr.io 与 gcr.io 两个候选 × native 组各 3 轮：`200 → 404`）——正是「探测不带 manifest Accept 会把存在的镜像判成不存在」这条缺陷的观测面，已修并有回归用例。
- **一条必须写明的限制**（源文件 §一.6）：ghcr.io/gcr.io 的**匿名 token 端点回 403**，所以在匿名探测下「不存在」与「存在但私有/需凭据」**不可区分**，两者都落到 absent。本服务对这些 namespace 没有配置凭据，本来也服务不了私有镜像，因此这个信息损失对本服务为零；代价是这类请求返回 404 而不是 403。

### 与已有条目的关系（本次改动直接依据的既有记录）

> **引用前必读（scope 已过期）**：下面三条的 `scope.conditions` 仍写「端口 5000/15000-15006/9100」等旧拓扑——它记录的是**当时的**环境。它们记的机制在本次改动后仍然成立（逐条见下），但凡把 15000–15006 当作当前端口的表述都应读作历史。
>
> 更正这件事我**只做到了一半**，边界说清楚：我曾以 `hindsight_ingest_document` 登记过两份更正（工具各返回一个 doc_id，主题分别是「15000–15006 与 namespaces.default 已删除」与「Accept 翻转 2 格→6 格、若干 negatives 的 scope 过期」），但**独立评估者在记忆服务里检索不到它们的正文**，我也无法从工作区证明其可检索。工作区里确实可见的只有本节这段文字；而账本本身是系统所有的文件（只有本体目录归模型编辑），它那条「2 格」的旧措辞因此**仍然在**。要让账本侧真正改掉，需要账本持有者动手——这一项已交给人。


- `clear/knowledge/negatives/x-h-nccqnt.json`（初步排除）：镜像站在 `/v2/` 上返回 401 挑战会让 dockerd 放弃镜像源回落官方 Hub —— 这正是当初引入按 namespace 绑定端口与 `anonymous_root` 的原因；本次把无限定请求改为并发选仓并删除那些端口后，主端点 `:5000` 对 `/v2/` 仍保持 401 挑战（实测），所以该条记录的机制没有被改坏，也没有被本改动推翻。
- `clear/knowledge/negatives/n-u-iiyp8u.json`：103 直连 `registry-1.docker.io` / `auth.docker.io` 会间歇 `connection reset by peer` —— 本次的传输层对照读数（direct 组 12 次里多次超时/重置）与它一致，也正是「不可达只留给真正的传输失败」这条口径的现实依据。
- `clear/knowledge/negatives/n-u-s01do2.json`：上游故障转移只看 `/v2/` 探活，会漏掉「探活正常、取内容失败」—— 本次改用仓库级探测（HEAD `/v2/<repo>/manifests/<ref>`）正是针对它，且在 ghcr.io 的 Accept 缺陷上验证了「探活式/不带内容协商的探测会给出错误结论」。

## 四、103 现场复验（交付态）

原始抓取：**`reports/raw/103-final-live-10ns.txt`**（**交付态**：10 个 proxy namespace、官方上游优先、镜像 `5a2c0ca73118`）。下表每一行都逐字取自这份文件（行号注明在括号里）；`reports/raw/103-final-live.txt` 是同一套检查在**增配前**（5 个 namespace）的抓取，本表不引用它。

| 检查 | 读数（行号在 `103-final-live-10ns.txt`） |
| :--- | :--- |
| 交付态镜像 / 端口 / namespace 数（§0） | `image=sha256:5a2c0ca73118…`；`ports=0.0.0.0:5000->5000/tcp, :::5000->5000/tcp, 0.0.0.0:9100->9100/tcp, :::9100->9100/tcp`（Dockerfile 的 EXPOSE 也已改回只列这两个端口）；`proxy namespace 数: 10` |
| **10 条** `rms_namespace_up`（§1） | dhi.io / docker.elastic.co / docker.io / gcr.io / ghcr.io / k8s.gcr.io / mcr.microsoft.com / nvcr.io / quay.io / registry.k8s.io，**全为 1** |
| 未命中缓存的无限定请求（§2） | **404** @5.466s / 4.826s / 10.002s |
| 同请求的响应体（§2） | `{"errors":[{"code":"NAME_UNKNOWN","message":"repository library/nope-final-check:latest is not available in any configured namespace","detail":null}]}` |
| 同请求的决策日志（§2，与上表同一 repo） | `repo=library/nope-final-check ref=latest kind=manifest winner="" **candidates=10** source=probe`，逐候选：`dhi.io=1.102377s(absent:not-manifest-content:text/html)`、`docker.elastic.co=2.320632s(absent)`、`docker.io=5.462858s(absent)`、`gcr.io=662.589ms(absent:auth-refused)`、`ghcr.io=885.498ms(unreachable:upstream-status)`、`k8s.gcr.io=2.672565s(absent)`、`mcr.microsoft.com=1.523325s(absent)`、`nvcr.io=1.82282s(absent)`、`quay.io=2.946231s(absent)`、`registry.k8s.io=2.672592s(absent)` |
| 已缓存镜像（§4） | **200** @2.106s，9218 字节 |
| 无仓库名的 blob（§5） | **200** @4.1ms，581 字节（靠仓库亲和；无亲和时同一路径是 503） |
| **503 包络**：`/v2/blobs/uploads/abc-123`（§3） | `{"errors":[{"code":"UNAVAILABLE","message":"cannot determine which namespace serves /v2/blobs/uploads/abc-123: the path names neither a repository nor a blob digest","detail":null}]}` |
| 旧端口与就绪（§6） | `:15000/:15003/:15006` → connection refused；`:5000/v2/` → 401；`readyz=ready` |

**两台宿主的发布端口**（原始抓取 `reports/raw/101-and-103-published-ports.txt`）：101 与 103 的 rms 都只发布 `0.0.0.0:5000->5000/tcp` 与 `0.0.0.0:9100->9100/tcp`（两台都已用最新代码重建，因此 `docker ps` 不再显示 5001-5002）；101 的 dockerd `Registry Mirrors` 为 `http://192.168.188.103:5000/`。

**三条边界说明**：①「所有候选上游都不可达 → 503」这条分支在 103 上**没有现场观测**（要造它得把 10 个上游全弄挂），由 `test/e2e` 的 `TestUnqualifiedAllUnreachable` 与 dispatcher 侧 `TestUnqualifiedRequestUnreachableIsTransportOnly` 覆盖；103 上另有「无仓库亲和的无仓库名 blob → 503」的观测（`reports/raw/103-mirror-pull-and-affinity.txt` 第 2 节，`source=none`）。②`ghcr.io`/`gcr.io` 现在显示 `absent:auth-refused`（官方匿名 token 端点回 403，这是「rms 匿名服务不了」的明确答复），此前镜像优先时它们显示 `unreachable:upstream-status`（镜像回 5xx）。③`dhi.io` 显示 `absent:not-manifest-content:text/html`（镜像 302 到落地页），不再抢走选择。

## 五、判据逐条对照

1. `namespaces.default` 与 `listeners`（含 `anonymous_root`）从配置结构体消失、严格解码报错 → 用新二进制实测，原文在 `reports/raw/selection-unit-and-config.txt`：带 default 的配置报 `line 4: field default not found in type config.Namespaces`，带 listeners/anonymous_root 的报 `line 7: field listeners not found in type config.Config`；103 上线配置本身通过解码（本地校验只差容器内的 `/etc/rms/htpasswd` 路径）。
2. 仓库与在线配置里再无 15000–15006：`grep -rn "1500[0-6]" config deploy README.md internal cmd test` 的全部命中都在 `deploy/103/README.md`（第 9–15、45、56、109、173、178 行：迁移对照表与迁移说明）；历史报告 `reports/**` 里当然还有旧读数（例如 `reports/raw/baseline.txt`），那是改前的记录，不是在线配置）；103 上 `docker ps` 只剩 5000/9100。
3. 并发探测（时间差 < 50ms）与只接受 2xx → 单测三条，原文在 `reports/raw/selection-unit-and-config.txt`：`skew between the two upstreams = 3µs`（阈值 50ms）、`slower candidate b=883.836µs (winner), a=200ms (200ms delay)`（按耗时取胜而非配置顺序）、`TestDecideSendsTheManifestAcceptHeader` 与 `...OnTheGetFallback` 通过（Accept 回归；同一条「按耗时取胜」用例在另一轮测得 292µs，见 `reports/raw/selection-notes.md`）。
4. `go test ./...` 与 `go test -tags e2e -count=1 ./test/e2e/...` 全绿；e2e 新增用例证明「A 有镜 B 没有 → 命中 A」「都没有 → 404」。
5. `go test -race ./internal/selection/` 无数据竞争 → `ok github.com/watsons/rms/internal/selection 1.783s`、`ok github.com/watsons/rms/internal/server 6.306s`（原文 `reports/raw/selection-gate-race-e2e.txt`）。
6. 103 上线后的端口清单、401 挑战、无限定命中缓存、**404 与 503 两种包络**、以及决策日志耗时 → 全部见第四节，原始抓取在 `reports/raw/103-selection-rollout-live.txt` 与 `reports/raw/103-selection-503-and-logs.txt`（后者含 503 的状态行、响应体与最近 6 条 selection 决策日志原文）。

## 六、过程中修掉并留下回归的三个缺陷

| 缺陷 | 证据 | 现状 |
| :--- | :--- | :--- |
| 探测缺 manifest `Accept`：ghcr.io 会把**存在**的镜像判成 404 | 假上游原文 `OCI manifest found, but accept header does not support OCI manifests`（见 `reports/raw/selection-notes.md` 与 `reports/raw/selection-real-upstream.txt`）；真实上游 ghcr.io 的 6 格 200→404 翻转 | 已修，回归用例故意去掉 Accept 会红 |
| 无仓库名的 blob 无法寻址：首次与重试都把空仓库路径交给引擎 | e2e 原文 `got 404, want 200 (404 page not found)`（见 `reports/raw/router-notes.md` 与 `reports/raw/config-docs-notes.md`）；定位 `dispatch.go` 的 `"/v2/blobs/" + digest` | 已修（首末两路共用 `blobEnginePath`），真引擎用例钉住 |
| 鉴权拒绝被当成不可达：103 上拼错镜像名返回 503 | 103 决策日志 `gcr.io=644.647ms(unreachable)` 等（原文 `reports/raw/selection-notes.md`） | 已修（HTTP 答复算 absent），单测 + 端到端两层钉住 |

另有一处**口径裁决**（不是缺陷）：多候选时「一个不可达 + 一个明确 404」原判 503，会再次产生「拼错镜像名变 503」的同一症状，已改为**无人答复才 503**；四象限都有固定用例。

## 七、仍未确定

- 上游抖动时，一个候选的传输失败不再能掩盖另一个候选的 404；若那个不可达的候选恰好持有该内容，客户端会把它当「不存在」。docker 拉 tag 每次重新解析，粘性风险主要在前置 HTTP 缓存。这是刻意的取舍，代码注释与 `reports/raw/selection-notes.md` 都有记录。
- 未命中缓存时每个无限定请求要等一轮探测：**交付态（10 候选）**实测 4.83–10.00s（`reports/raw/103-final-live-10ns.txt` 第 20–22 行，最慢一次撞到 `probe_timeout` 上限）；**增配前（5 候选）**是 2.49–7.47s（当时的 `103-selection-rollout-live.txt` 与 `103-final-live.txt`，现已只作历史对照）。
- Basic 认证上游（challenge 无 realm）现按 absent 处理（404），未做用配置凭据的 Basic 重试。

## 八、复现命令

```bash
cd /root/dev/registries-mirrors && source scripts/env.sh
make check
go test -race -count=1 ./internal/selection/ ./internal/server/
go test -tags e2e -count=1 ./test/e2e/...
# 真实上游（读网络，不动 103）
cd .probe/selection && go run . -rounds 3        # 生成 raw-proxy.jsonl / raw-direct.jsonl
# 复算并发读数（本报告第二节的表格即由它生成）
python3 - <<'PY'
import json, statistics as st
for mode,path in (("proxy",".probe/selection/raw-proxy.jsonl"),("direct",".probe/selection/raw-direct.jsonl")):
    rows=[json.loads(l) for l in open(path) if l.strip()]
    sp=[r["spread_ms"] for r in rows]; ra=[r["ratio"] for r in rows]
    print(mode, len(rows), f'spread median {st.median(sp):.3f}ms max {max(sp):.3f}ms',
          f'ratio median {st.median(ra):.2f}x ({min(ra):.2f}-{max(ra):.2f})')
PY
# 逐轮逐候选 start_offset_ms 表：reports/raw/selection-concurrency-stats.txt
# 103 现场（只读）
curl -s -H 'Accept: application/vnd.oci.image.manifest.v1+json' http://192.168.188.103:5000/v2/alpine/manifests/latest
ssh root@192.168.188.103 'docker logs rms | grep selection | tail -5'
```

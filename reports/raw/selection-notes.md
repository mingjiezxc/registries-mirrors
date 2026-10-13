# selection 并发抉择器实现与实测读数（task-1）

时间：2026-10-13 · 作者：selection-dev · 复现命令见文末

本文覆盖 task-1 原验收（(a)~(g)）、Lead 的多轮口径裁决（缓存优先参与 (h)(i)(j)、混合不可达一律 503 (k)、认证判定 (l)(m)(q)(r)、blob 无仓库名一律 503 (o)），两个被测代码的真实缺陷（manifest Accept、blob 路由），以及 103 上线实测后对「认证拒绝算不算不存在」的最终修正。

## 结论

`internal/selection` 的并发抉择器已实现并通过验收：`go test ./internal/selection/ ./internal/registry/ ./internal/upstream/` 三包全绿（含 `-race`），`go test -tags e2e -count=1 ./test/e2e/...` 全绿，`gofmt` / `go vet` 干净。

关键事实：

1. **候选探测确实是并发的**。两个各延迟 150ms 的上游，四轮独立运行的总耗时分别为 **151.091 / 150.827 / 150.960 / 150.995ms**，两上游收到请求的时间差为 **22µs / 8µs / 30µs / 5µs**。串行探测的下界是 300ms，压缩比 ≈ 2.0。
2. **按实测耗时而非配置顺序抉择**。A 人为延迟 200ms、B 不延迟时，即使 A 在候选列表里排第一，赢家仍是 B，赢家耗时 **292µs ~ 461µs**。
3. **401 → 匿名/凭据 Bearer token 流程走通**。假上游要求 token 时：1 次 token 请求 + 2 次 manifest HEAD（第一次 401、带 token 重试 200）。
4. **磁盘优先**。候选本地已有 (repo, ref) 时，决策 **0 次探测**：实测「3 个候选、1 个命中缓存」= 上游命中计数 `a=0 b=0 c=0`，只做 2 次 stat 就返回。
5. **404 的判据（最终版）**：**至少一个候选给出了明确答复 → `ErrNotFound`(404)**；**一个候选都没答 → `ErrUnreachable`(503)**。「明确答复」按三分类：2xx = 存在；**拒绝类**（401/403/404/400/410、两个方法都 405、token 端点的 401/403）= 明确不存在；**5xx、429、传输失败、超时、无当选上游 = 没能给出答复**（不可达）。ghcr/gcr/quay 那种「匿名不给读」属于拒绝类，是答复；上游 500/429 反过来也不能当成「不存在」——那会给客户端一个它不会重试的 404，而镜像可能确实在。两类错误（把答复当沉默、把沉默当答复）都已修掉。
6. **不存在的镜像 = 404**。五个 namespace 的假上游分别照抄 103 上四种真实鉴权行为，拼错的仓库名得到 404 而不是 503；其中**再有一个 namespace 挂掉**（地址无人监听）时仍然是 404，只有**五个全挂**才 503。
7. **2xx 不等于存在**。探测按最终响应的媒体类型判定「像不像被探的东西」：manifest 必须是 JSON 系，HTML 一律不算；blob 只要不是 HTML 即可。真实复现（`.probe/dhiprobe`，对着 dhi.m.daocloud.io）：修复前 `winner=dhi.io`，修复后 `winner=""`、`timings="[dhi.io=4.68s(absent:not-manifest-content:text/html)]"`。
8. **blob（只有 digest、没有仓库名）只跟着 affinity 走**；没有 affinity 就 503，绝不猜仓库 —— 见下文「缺陷二」。
9. **重定向跟随**（实测决定）：保留跟随（引擎也跟随；`registry.k8s.io` 用 307 把 manifest 转到 `pkg.dev`，这是它唯一的服务方式），靠媒体类型挡住 HTML 落地页。理由与轨迹见「缺陷四」。

## 两个被测代码的真实缺陷（本轮修复）

### 缺陷一：探测请求不带 manifest Accept → 存在的镜像被判 absent

**症状**：`"OCI manifest found, but accept header does not support OCI manifests"`，`http.response.status:404`；冷缓存首次拉 ghcr 上的镜像时判成「不存在」。

**修法**：探测请求（HEAD 与 405/501 降级后的 GET 都是）带上 `manifestAccept` 常量，四个 media type 就是内嵌 registry 自己的 manifest handler 认得的那四个（`application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json`）。值在 `probe.go` 里写死而非引用 image-spec 常量，是为了让这个包只依赖 `internal/config`（image-spec 在 go.mod 里是 indirect）。

**回归测试有牙**：把 `req.Header.Set("Accept", ...)` 那一行临时删掉后重跑，两条回归用例立刻变红（原文）：

```
--- FAIL: TestDecideSendsTheManifestAcceptHeader
    chooser_test.go:744: Decide: no configured namespace has this repository and reference (a registry that stores OCI manifests must not be judged absent)
--- FAIL: TestDecideSendsTheManifestAcceptHeaderOnTheGetFallback
    chooser_test.go:793: Decide: no configured namespace has this repository and reference
```

带上之后：`--- PASS: TestDecideSendsTheManifestAcceptHeader`、`--- PASS: TestDecideSendsTheManifestAcceptHeaderOnTheGetFallback`。测试里还有一个 control 子用例，直接对假上游发一个不带 Accept 的 HEAD，断言它回 404 —— 保证这条用例本身不会因为假上游太宽容而失去意义。

### 缺陷二：blob 请求（只有 digest）无法路由

**症状**：客户端只拿 digest 取 blob（`GET /v2/blobs/<digest>`）时会 404；旧实现靠被钉死的默认仓库躲过了这个问题。

**修法（Lead 裁决）**：新增 `Chooser.DecideBlob(digest string) (Target, error)`：
1. 命中最近一次 manifest 决策的 affinity → 原样返回该 Target（带仓库级 `EnginePath`，dispatcher 据此拼 `/v2/<repo>/blobs/<digest>`），**不发任何探测**；
2. 没有 affinity → 查各候选磁盘（`Manager.CachedBlob`，`storage.HasBlob`）：**即使命中也返回 `ErrUnreachable`**，只在日志里记下「哪个 namespace 有这个 digest」。理由：blob 在 distribution 的路径里必须靠 `/v2/<repo>/blobs/<digest>` 寻址，而磁盘上的 blob 是内容寻址、**不带仓库名**的，猜一个仓库等于拿别的仓库的路径去取数据；「不知道归哪个仓库」也不是「内容不存在」的证据，所以既不能 404 也不能瞎拼路径。
3. 都没有 → `ErrUnreachable`，日志 `source=none`。

**日志**（上线取证用）：affinity 命中记 `source=affinity` 并带上仓库名；磁盘命中记 `source=blob-disk` 且仓库名为空；都没有记 `source=none`。e2e 原文：

```
INFO selection digest=sha256:b3f00b7e9822cde37aabe439b92ec8f82e08dbcae095223888ec633c2ed2cd06 winner=docker.io repo=library/demo source=affinity
```

与之配对的 manifest 决策行（同一场 e2e）：

```
INFO selection repo=library/demo ref=v1 winner=docker.io candidates=2 source=probe cached=false timings="[docker.io=575µs ghcr.io=470µs(absent)]"
```

两行合看即证明「blob 跟到了服务 manifest 的同一个 namespace，且带上了仓库名」。

### 缺陷三：带仓库名的 blob 请求被当成 manifest 探测（103 真实客户端联调照出）

**症状**：客户端取一个**确实存在**的 layer：
```
GET http://192.168.188.103:5000/v2/library/busybox/blobs/sha256:034d6572bf28… → 404
selection repo=library/busybox ref=sha256:034d6572… winner="" candidates=5 source=probe
  timings=["docker.io=1.618885s(absent)","gcr.io=630.012ms(absent)","ghcr.io=1.515453s(absent)","quay.io=716.88ms(absent)","registry.k8s.io=462.554ms(absent)"]
```
**根因**：dispatcher 把这个路径也按「仓库 + 引用」交给 `Decide`，于是探测问的是 `/v2/<repo>/manifests/<layer 摘要>` —— 每个候选都诚实地回答「没有这个 manifest」，整体判 404。blob 的「存在」必须在 **blob 路径**上问。今天这次 docker 拉取之所以没炸，是因为 docker 把 blob 直接发去了 Docker Hub、没走 rms，直到真实客户端联调才照出来。

**修法**：新增 `Chooser.DecideBlobInRepo(ctx, candidates, repo, digest) (Target, error)`，语义与 `Decide` 完全一致（并发、只认 2xx、取最快、probe_ttl 缓存、写 repo 级亲和、同一套三分类），只把探测路径换成 `/v2/<EnginePath>/blobs/<digest>`。实现上把 `Decide` 与它共同的部分抽成 `resolve(ctx, lookup, candidates, onDisk)`，`lookup` 携带 `op`（`/manifests/` 或 `/blobs/`）、`probeRef`、`cacheRef`、`kind`。

两个细节：
- **缓存键**：blob 用 `(repo, "blobs/"+digest)`，与 manifest 的 `(repo, ref)` 分开，避免「blob 摘要恰好等于某个 manifest 引用」时互相顶掉（`TestBlobAndManifestDecisionsDoNotShareACacheKey` 钉住）。
- **`recent` 指针**：只有 manifest 决策会更新它。它是「最近一次 manifest 落在哪个 namespace」，给「路径里没有仓库名的 `/v2/blobs/<digest>`」用；blob 决策覆盖它会让那条路跟着 blob 走，不再是它自己的语义。
- 日志多了 `kind` 字段（`manifest` / `blob`），决策行与探测行都带，便于在上线日志里区分两类探测。

### 缺陷四：302 → HTML 落地页被当成 manifest（103 加 namespace 后照出）

**症状**：`Decide` 把**根本取不到内容**的上游判成存在并让它胜出，客户端随后 404；同一机制会让**真实存在的镜像**被这个上游抢走后 404。

**根因与轨迹**（Lead 在 103 抓到、我在本机复现，`.probe/dhiprobe`）：
```
→ HEAD https://dhi.m.daocloud.io/v2/library/definitely-not-here-rms-10ns/manifests/latest : 302 type="text/html" loc="https://github.com/DaoCloud/public-image-mirror"
→ HEAD https://github.com/DaoCloud/public-image-mirror : 200 type="text/html; charset=utf-8"
（修复前）INFO selection … winner=dhi.io candidates=1 source=probe timings="[dhi.io=3.66s]"
（修复后）INFO selection … winner="" candidates=1 source=probe timings="[dhi.io=4.679883s(absent:not-manifest-content:text/html)]"
Decide → err=no configured namespace has this repository and reference
```
daocloud 的 dhi 镜像对未知仓库回 302 到 GitHub 的项目页，Go 的 client 默认跟随，于是探针看到 GitHub 的 **200 text/html**，判成「manifest 存在」。

**修法（与 Lead 建议的差异，附理由）**：**保留跟随重定向**，改成**按最终响应的媒体类型判定**：
- 2xx 只有在「看起来像被探的东西」时才算存在：`looksLikeContent(kind, contentType)` —— HTML（`*html*`）= 不是内容；manifest 必须是 JSON 系（`+json` 或 `/json`）；blob 只要不是 HTML 即可（octet-stream / gzip / CDN 标签都算）。
- **媒体类型缺失或无法解析 = 没有证据，按存在接受**（`mediaTypeOf` 对两者都返回 `""`）。第一版把「缺失」也当成「不是 manifest」，把 `internal/server` 的假上游（裸 `WriteHeader(200)`，HEAD 不带 Content-Type）判成 absent，被 router-dev 的 12 条 dispatcher 用例当场抓到并修掉——**这条是本次最有价值的教训：过严会产生「把可服务的 namespace 判成不存在」，代价比漏判更高。**
- **不跟随重定向的做法被实测否决**：`registry.k8s.io` 对 manifest 路径回 **307** 到 `us-east4-docker.pkg.dev/v2/k8s-artifacts-prod/images/…`，跟随之后拿到 **200 `application/vnd.docker.distribution.manifest.list.v2+json`**（本机实测，含 `Docker-Content-Digest`）。若按「不跟随 + 3xx 算 absent」实现，`registry.k8s.io` 这个**当前可用的 namespace 会被永久判成不存在**。而且引擎（distribution proxy）自己就跟随重定向，探针不跟随的结果会与引擎的实际能力不符。
- 代价（实测）：dhi.io 那条探测要跟到 GitHub，耗时 **4.7–7.9s**（`probe_timeout` 10s 之内），因为 10 个候选是并发探测的，这只影响冷缓存首次抉择的最慢那个候选。
- 仍未被跟随的 3xx（300/304/305/306 或超出 10 跳）→ 记 `redirect`，算「没能给出答复」（unreachable），不是 absent。

**诊断字段**：决策行的 `timings` 现在带**短原因**，让「这个候选为什么不可达」能从生产日志直接读出来，而不必猜：
```
timings="[html=417µs(absent:not-manifest-content:text/html) refusing=612µs(absent:auth-refused) dead=194µs(unreachable:transport)]"
```
原因取值：`not-<kind>-content:<media type>` / `auth-refused` / `redirect` / `upstream-status` / `timeout` / `transport` / `token-transport` / `token-status` / `token-answer` / `canceled` / `skipped`。全部无 URL、无凭据。

每条探测还多一行 debug（`msg="selection probe"`），带 `kind` 与 **`path`**（实际请求的完整路径，如 `…/v2/library/busybox/blobs/sha256:…`）。「探针到底问了哪条路径」是这一类的根因所在（该问 blob 却问了 manifests），此前只能去上游日志里找；现在两条日志合起来能独立回答「谁的责任」：
```
INFO  selection repo=… ref=… kind=manifest winner="" … timings="[a=…(absent:not-manifest-content:text/html)]"
DEBUG selection probe repo=… ref=… kind=manifest namespace=a upstream=… path="…/v2/library/alpine/manifests/latest" status=200 …
```
`TestProbeDebugLineCarriesThePath` 钉住这个诊断面。

两侧的日志契约都各有测试钉住，任一侧改动不会悄悄让判据失效：
- 本侧（字段的来源）：`TestDecisionLineSaysWhichKindWasProbed`（决策行的 `kind`+`ref`：manifest→tag、blob→digest；把 `kind` 从决策行删掉即变红）、`TestProbeDebugLineCarriesThePath`（debug 行的 `path`）、`TestDecisionLogNamesWhyACandidateLost`（`timings` 的原因）。
- dispatcher 侧（字段被读出来的后果）：`internal/server` 的 `TestBlobDecisionLineSaysKindBlob` 等，见 `@reports/raw/router-notes.md` 第 8c 节。

这两条不是冗余：本侧那条走 `Decide`/`DecideBlobInRepo` 直调，抓 chooser 的回归；对侧那条走真 HTTP 穿 dispatcher，抓「blob 请求有没有真的被分流到 blob 分支」。谁改了各自的代码，只有对应那一颗牙会红。

这条是从两次真实教训里长出来的，两次的根因相同：**没有去看产出那个字段/响应头的那段代码**，而且都发生在各自最熟悉的包的边界上（最容易凭印象下结论的地方）。

- router-dev 写下的 103 判据「`timings` 里出现 `/manifests/`」**永远不会亮**（`timings` 的格式是 `namespace=耗时(结果[:原因])`，不含路径）。
- 我第一版的媒体类型校验**过严**：以为「缺失 Content-Type」会被当成 HTML 而拒绝，实际 `mime.ParseMediaType("")` 报错、我把它映射成了 `unparsable` 并拒绝，于是 12 条 dispatcher 用例全红——把**可服务**的候选判成了不存在。

**下次怎么做（比「这次错在哪」更值得留）**：
1. **任何要交给别人复验的判据，先在本地跑一次、确认它真的会亮**；亮不起来的判据比没有判据更危险（它给的是安心，不是信息）。
2. **新增严格判据前，先拿现有假上游/最小样本跑一遍**。「过严」与「过松」的代价不对称：过松会让错的东西胜出（本次缺陷四），过严会把**可用**的 namespace 判成不存在——后者的用户可见后果更重，所以默认取「没有证据就接受」。
3. **跨侧引用的字段，两侧各钉一颗牙**：写出去的事实（本侧）与读出来的后果（对侧）。本契约的四个测试名见上。

## 改动文件

| 文件 | 改动 |
| :--- | :--- |
| `internal/selection/probe.go` | 新增。`Decide`（磁盘优先 → 并发探测）、单候选探测状态机（HEAD / 挑战满足后重试 / 405-501 降级 GET）、`manifestAccept`、并列排序、决策缓存 + repo 级亲和 + 最近一次 manifest 指针、`Invalidate` / `InvalidateRepo` / `Affinity` / `DecideBlob`、结构化日志（`source` 字段） |
| `internal/selection/chooser.go` | 契约文件：删掉占位 `Decide`；`Chooser` 增加决策缓存 / 亲和 / 最近 manifest 字段；`Upstream` 增加 `CachedManifest`、`CachedBlob`；两个错误变量的文档注释按新口径重写 |
| `internal/selection/chooser_test.go` | (a)~(g) + (h)~(p) + Accept 两条回归 + EnginePath 路径、GET 降级、Invalidate/亲和、日志 `source`/`repo`、挑战头解析与转义、`pick` 排序 |
| `internal/registry/manager.go` | 新增 `Serving()` / `ElectedUpstream(ns)` / `UpstreamCredentials(ns)` / `CachedManifest(ns, repo, ref)` / `CachedBlob(ns, dgst)`，外加 `UpstreamLatency(ns)`（EWMA 兜底权重） |
| `internal/registry/manager_selection_test.go` | 新增。四个访问器 + `CachedManifest`（真写磁盘布局：tag 链接、revision 链接、digest 与 tag 两种引用、local 与 proxy 两类 namespace）+ `CachedBlob` + 「当选端点不健康时仍返回 URL」 |
| `internal/upstream/selector.go` | `probe` 返回实测耗时；`endpoint.ewma`（α=0.3）在 `ProbeAll` / `ElectForStartup` 更新；新增导出 `LatencyEWMA()` |

## 实测读数

四轮独立运行（`-count=1`）：

| 读数 | 第 1 轮 | 第 2 轮 | 第 3 轮 | 第 4 轮 |
| :--- | ---: | ---: | ---: | ---: |
| 两上游收到探测的时间差 | 22µs | 8µs | 30µs | 5µs |
| 两个 150ms 延迟上游的 Decide 总耗时 | 151.091ms | 150.827ms | 150.960ms | 150.995ms |
| 串行探测下界（对照） | 300ms | 300ms | 300ms | 300ms |
| 逐候选耗时 fast / mid / slow（人为 10/30/60ms） | 10.47/30.78/61.10ms | 10.66/30.91/61.29ms | 10.59/30.89/61.30ms | 10.59/30.89/61.30ms |
| token 流程 | 1 次 token 请求 + 2 次 HEAD（401→200） | 同 | 同 | 同 |
| 缓存命中（1 命中候选） | — | — | 1 次 stat，0 次探测 | 同 |
| 缓存命中（3 候选 1 命中） | — | — | 2 次 stat，探测 a=0 b=0 c=0 | 同 |
| 健康探测 EWMA 种子（registry 测试） | 503µs | 615µs | 664µs | — |

## 测试输出原文（关键行）

本次（第三次修订）新增的两条：

```
--- PASS: TestServerErrorsAreNotAbsence (0.00s)
    --- PASS: TestServerErrorsAreNotAbsence/manifest_answers_500 (0.00s)
    --- PASS: TestServerErrorsAreNotAbsence/manifest_answers_502 (0.00s)
    --- PASS: TestServerErrorsAreNotAbsence/manifest_answers_429 (0.00s)
    --- PASS: TestServerErrorsAreNotAbsence/a_500_on_the_GET_fallback_is_not_an_answer_either (0.00s)
--- PASS: TestAFailingCandidateStillLosesToAnAnswer (0.00s)
ok  	github.com/watsons/rms/internal/selection	0.010s
```

```
--- PASS: TestDecideSendsTheManifestAcceptHeader (0.00s)
--- PASS: TestDecideSendsTheManifestAcceptHeaderOnTheGetFallback (0.00s)
    chooser_test.go:478: concurrency: skew between the two upstreams = 5µs; total Decide = 150.995ms (two serial probes would be >= 300ms)
    chooser_test.go:517: token flow: 1 token request, 2 manifest HEADs (401 then 200)
    chooser_test.go:1151: cache hit decided the request with 1 disk lookups and 0 probes
    chooser_test.go:1179: cache hit with 3 candidates: probes a=0 b=0 c=0, 2 disk lookups
    chooser_test.go:1280: blob followed the manifest decision to a with 0 probes
    chooser_test.go:1331: disk hit without affinity: namespace b held the digest, no repository to address it by, refused with 503
ok  	github.com/watsons/rms/internal/selection	0.573s
```

e2e（`go test -tags e2e -count=1 ./test/e2e/...`）：

```
--- PASS: TestLoginContract (0.51s)
--- PASS: TestPullThroughAndCache (0.32s)
--- PASS: TestUnqualifiedPicksNamespaceWithContent (0.32s)     ← 缺陷一的证据用例，修复前红
--- PASS: TestUnqualifiedAllUnreachable (5.62s)
--- PASS: TestUnqualifiedBlobFollowsTheManifest (0.32s)        ← 缺陷二的证据用例，修复前红
--- PASS: TestUnqualifiedBlobWithUnknownDigestIsUnavailable (0.31s)
--- PASS: TestPushFlow (0.94s)
--- PASS: TestAuthorisationMatrix (0.97s)
--- PASS: TestDegradedServesCache (0.70s)
ok  	github.com/watsons/rms/test/e2e	10.024s
```

`-race`：`ok .../selection 1.623s`、`ok .../registry 1.046s`、`ok .../upstream 1.126s`。
整仓库 `go test ./internal/...` 全绿（含 `internal/app`、`internal/server`）。

## 口径裁决（Lead 定，本实现遵循）

1. **磁盘优先于探测**。`Decide` 先对所有候选做一次同步 `CachedManifest`：命中即选，多个命中取 namespace 名字序在前者，`ProbeLatency=0`，日志 `source="cache"`，其余候选一个探测都不发。传给 `CachedManifest` 的 repo 是候选的 `EnginePath`（namespace 存储/引擎看到的仓库名，如 `library/alpine`），不是客户端写的 `alpine`。
2. **混合「存在答复 + 不可达」的最终规则（第二次修订）**：**至少一个候选给出明确答复 → 404；一个都没答 → 503**。此前是「任一未答 → 503」，103 复验前被测出与事故同一症状：配置多个 namespace 时，只要有一个 namespace 抖动，客户端**拼错一个镜像名就拿 503**（router-dev 的读数：A 不可达+B 404 → 503；A 404+B 不可达 → 503；AB 都不可达 → 503；A 不可达+B 有该仓库 → 200）。改判理由（代价不对称）：报 503 让客户端对**根本不存在的镜像永久重试**，且每个未缓存请求都要先等一轮探测；报 404 的坏情况只是「那个没答话的候选恰好有它」，而那时它本来也取不到内容。这条让 404 的语义回到「**能被问到的候选都说没有**」，503 只留给真正的「谁都没答上话」——一个诚实的「不知道」。
3. **认证拒绝算「明确不存在」**（103 上线实测修正，取代此前「认证没走通算探测失败」的口径）：
   - 注册表的**拒绝类回答**算「明确的不是」，包括：token 端点回 401/403、challenge 里没有可用 realm、拿到票后 manifest 仍 401/403、405（HEAD 与 GET 都不支持）。理由：对匿名只读缓存来说，「它要鉴权而 rms 没有可用凭据」等价于「rms 服务不了这个仓库」，是明确的「不」；把它当不可达会让**每一个未缓存的无限定请求**都变 503。
   - 带 Bearer challenge 的 401 无论凭据是否为空都打 token 端点（匿名 token 是公开仓库的正常路径）。
4. **5xx 与 429 不算答复**（独立评估者发现的第二处口径不一致，第三次修订）：**manifest 路径上的 5xx 与 429 与 token 端点上的同样处理 → 探测失败（unreachable）**，不能判 absent。上游 500 时并没有说「这个仓库不存在」；判 absent 会给客户端一个它不会重试的 404，而镜像可能确实在。此前 manifest 路径的 default 分支把「除 401/405/501 外的任何状态」都当成「明确不存在」，与 token 端点那侧（5xx/429 已是失败）自相矛盾。
   - 405/501 的降级特例保留：HEAD 遇到它们先降级 GET 再判；GET 仍 405 → 明确不存在；GET 仍 501（属 5xx）→ 探测失败。
   - 全表（两个路径统一）：**2xx = 存在；401/403/404/400/410、两方法都 405、token 端点 401/403 = 明确不存在；5xx、429、传输失败、超时 = 没能给出答复**。
5. **blob 无仓库名 → 503，不猜仓库**（缺陷二的裁决）。affinity 命中是正常路径，不探测；磁盘命中但仓库未知时只记日志、仍 503。
6. **一个探测最多 3 个请求**，有界：`HEAD` →（401）`HEAD+token` →（405/501）`GET+token`。

### 103 修正的实测背景与复现

103 上线后一条不存在的镜像得到 503，逐候选读数是：

```
timings=["docker.io=3.78061s(absent)","gcr.io=644.647ms(unreachable)","ghcr.io=1.335972s(unreachable)",
         "quay.io=1.282109s(unreachable)","registry.k8s.io=402.814ms(absent)"]
```

对 .103 直接复测：`ghcr.io` / `gcr.io` 的**匿名 token 端点直接回 403**，`quay.io` **拿到票后 manifest 仍 401** —— 都不是上游故障，而是这些注册表对匿名客户端的正常回答。修正后同一场景在单测里复现为：

```
chooser_test.go:767: mistyped image across 5 namespaces:
  [docker.io=450µs(absent) gcr.io=966µs(absent) ghcr.io=589µs(absent) quay.io=872µs(absent) registry.k8s.io=493µs(absent)] (all absent ⇒ 404)
```

（`TestDecideMistypedImageIsNotFound`，五个假上游分别照抄上述四种真实行为：开放 404、token 端点 403、拿票后仍 401、直接 404。）

### 第二次修订（混合不可达）的读数

| 场景 | 改判前 | 改判后 |
| :--- | :--- | :--- |
| A 不可达 + B 明确 404 | 503 | **404** |
| A 明确 404 + B 不可达 | 503 | **404** |
| A、B 都不可达 | 503 | 503 |
| A 不可达 + B 有该仓库 | 200 | 200 |
| 103 形状：5 个 namespace，4 个回答、1 个挂掉 | 503 | **404** |

最后一行是新增用例 `TestDecideMistypedImageSurvivesOneNamespaceDown` 的实测：

```
chooser_test.go:846: four answered, one down ⇒ 404:
  [docker.io=523µs(absent) gcr.io=2.581ms(absent) ghcr.io=767µs(absent) quay.io=1.771ms(absent) registry.k8s.io=533µs(unreachable)]
```

同一用例的另一半（五个命名空间**全挂**）断言 503 —— 503 现在只表示「谁都没答上话」。

这条边界的另一侧（dispatcher 的选择结果 → 引擎路径 → 转发/重选）由 router-dev 的
`internal/server/dispatch_test.go` 夹住：`TestUnqualifiedRequestNotFoundWhenCandidatesOnlyChallenge`（挑战→403 的
token 端点 ⇒ 404）、`TestUnqualifiedRequestUnreachableIsTransportOnly`（无人监听 ⇒ 503）、
`TestAnsweredCandidateDecidesWhenAnotherIsDown`（上面四行读数的表）。两侧笔记互相指向：
dispatcher 侧见 `@reports/raw/router-notes.md`。跨侧结论刻意留在**两侧各自**的笔记里，出问题时任一侧都能独立定位。

> ⚠️ 编辑本节时注意：上面三个用例名被 `internal/server` 的 `TestSelectionBoundaryCasesAreStillNamed`
> 逐个断言存在（它读的就是**这个文件**）。改用例名或删用例时，请同步这里的引用和那个检查——否则服务端
> 那套测试会红，而它想说的是「引用漂移了」，不是「用例有问题」。本节标题「第二次修订」也被那边的注释按名引用。

## 实现中发现并修掉的问题

1. `UpstreamLatency` 对没有 selector 的 `proxyNamespace`（cache-only 状态）会空指针 panic。`ElectedUpstream` 与 `UpstreamLatency` 都加了 `p.selector == nil` 守卫。
2. 探测请求原本不带 manifest Accept（缺陷一）。
3. `DecideBlob` 的磁盘命中分支原本返回 `Target{Namespace}` 而没有仓库名，dispatcher 拼出的 `/v2/blobs/<digest>` 在 distribution 里无路由（engine 回答 `404 page not found`）；按裁决改为 503。
4. 认证判定反复过一轮：最初「401 一律算不存在」（会产出虚假 404）→ 中间一版「认证没走通算探测失败」→ 103 实测证明后者让拼错的镜像返回 503（ghcr/gcr/quay 恒被判 unreachable），最终定为「注册表给出的**拒绝类**回答算明确不存在」。这条改动把「404 还是 503」的判断**从上游的鉴权行为里解放出来**。
5. **manifest 路径的 5xx/429 曾被判成 absent**（独立评估者在代码里发现的第二处口径不一致，第三次修订）：`probe()` 的 default 分支把「除 401/405/501 外的任何状态」都当成「明确不存在」，而 token 端点那侧 5xx/429 早已算探测失败——同一份代码两套分类。已按「5xx/429 = 没能给出答复」统一；`TestServerErrorsAreNotAbsence`（500/502/429 各一条 + GET 降级后 503 一条）与 `TestAFailingCandidateStillLosesToAnAnswer`（500 + 另一候选 404 → 仍 404）钉住两侧。

## 尚未确定 / 未做

1. **真实上游未在本机复测**：ghcr.io 的 Accept 行为来自 docs-dev 的真实读数（`reports/raw/selection-real-upstream.txt`）与 Lead 转述，我没有在 101 上独立复跑那四条真实探测；本机全部数字来自 httptest 假上游与 e2e 的内嵌 distribution。
2. **Basic 认证上游**：此前的悬置项已被 103 修正顺带回答——只发 Basic challenge（没有 Bearer realm）的 namespace 现在算「明确不存在」（不是 503），符合「要鉴权而 rms 是匿名只读缓存 ⇒ 服务不了」。若将来要给这类上游用配置的 `UpstreamCredentials` 做 Basic 重试，是新增能力，不是当前语义的一部分。
3. **「无上游候选 + 其它候选明确 404」判 404**（第二次修订后）：有候选答了，未答的候选不否决它。测试 `one_candidate_answers_404_and_one_has_no_upstream` 钉住（此前断言 503，已随口径翻面）。
4. **缓存陈旧**：磁盘优先会让「某 namespace 已缓存的旧 tag」胜过另一 namespace 上更新的同名 tag（pull-through 缓存固有语义，非本次引入）。
5. **blob 磁盘命中路径的可用性缺口**：按裁决它是 503；只有 affinity 能救。docker 取 blob 前必先取 manifest，所以正常路径不受影响；但「客户端只拿 digest 直接取 blob 且决策已过期」会 503 而非命中磁盘。要支持它需要 digest→仓库 的反向索引（distribution 布局里没有现成的，需要遍历仓库读 manifest 反查），未做。
6. **EWMA 兜底几乎不触发**：只在两候选耗时**恰好相等**时生效，刻意不加模糊容差；用 `TestPickTieBreaksDeterministically` 直接对 `pick` 覆盖。
7. **未做限流**：候选数 = proxy namespace 数（配置量级很小），未加并发信号量；缓存检查是同步的，每候选一次 stat。

## 复现命令

```bash
cd /root/dev/registries-mirrors
source scripts/env.sh

# 验收（三个包）
go test ./internal/selection/ ./internal/registry/ ./internal/upstream/ -count=1

# 竞态
go test -race -count=1 ./internal/selection/ ./internal/registry/ ./internal/upstream/

# 端到端
go test -tags e2e -count=1 ./test/e2e/...

# 读数
go test -count=1 -v ./internal/selection/ | grep -E "chooser_test.go:|^ok"
```

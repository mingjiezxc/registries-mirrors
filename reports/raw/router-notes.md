# 路由与接线笔记（task-2，router-dev）

范围：`internal/config`、`internal/namespace`、`internal/server`、`internal/app`、`cmd/rms`。
契约方：`internal/selection`（selection-dev）。本文件记录决定、遗留与证据位置。

## 1. Resolution.NeedsSelection 的语义

`internal/namespace/resolver.go` 的 `Resolution` 新增一个字段：

```go
// NeedsSelection marks an unqualified request: the path named no configured
// namespace, so the dispatcher must ask the selector which proxy namespace
// serves it before the request can be forwarded.
NeedsSelection bool
```

- 触发条件：`/v2/<first>/...` 里 `first` **既不是**已配置的 proxy namespace、**也不是** local namespace（原来这种请求会落到 `namespaces.default`）。
- 这种 Resolution 的三个字段语义是**刻意的空**：`Namespace==""`、`Mode==""`、`EnginePath==""`；
  `Repo` 保留**客户端写的那份仓库名**（如 `alpine`、`library/alpine`、`myorg/app`），
  **不做** `library/` 注入——`official_library` 是 namespace 的属性，不是请求的属性，
  所以它只能在知道赢家之后逐候选施加。
- `Kind` 仍是 `KindRepository`。
- 为什么要显式布尔而不是靠空字符串猜：`Namespace==""` 在 `KindBase`/`KindCatalog` 上也是合法值，
  dispatcher 若用「Namespace 为空」来判断会误伤 API 根与 `_catalog`。测试
  `TestResolve` 的 `wantSelect` 列逐例钉住该字段（含 base/catalog/显式前缀/local 全为 false）。
- 未变：显式前缀（`/v2/docker.io/...` 去掉前缀交给引擎）、local namespace（保留前缀，因为上传 Location 由仓库名拼）、
  `_catalog`、`/v2/`、以及监听器形式的 socket（`Resolve(listenerNS, path)` 的 `listenerNS` 参数仍在，
  但生产上只有主端点会用，传 `""`）。
- `Table.Default()` 与 `defaultNS` 已删除；新增 `Table.ProxyNames()`（排序）供候选构造与启动摘要使用。

## 2. 候选构造与 ref 的取法

`internal/server/dispatch.go`：

- **候选集 = `mgr.Serving()`**（stateProxy + stateCacheOnly），不是「配置里所有 proxy namespace」。
  理由：没有引擎的 namespace 只会回 503，探它没有意义。这条是 Lead 的裁决，与 selection-dev 的 `Upstream` 语义一致。
- 每个候选：`Candidate{Namespace: ns, EnginePath: 该 ns 规则下的仓库名}`，
  其中 `official_library` 且仓库名不含 `/` 时前缀 `library/`。
  **EnginePath 是「不含 `/v2/` 前缀的仓库级路径」**，例如 `library/alpine`：
  selection 侧自己拼 `electedUpstream + "/v2/" + EnginePath + "/manifests/" + ref` 去探测，
  `Target.EnginePath` 原样返回同一份，dispatcher 再拼回 `/v2/` 与操作后缀。
  三处口径必须一起看：候选构造、`Target` 解包、重选时的转发路径。
- `Decide(ctx, candidates, repo, ref)`：
  - `repo` = 客户端写的原始仓库名（缓存键 + 日志；selection 侧用它做 (repo,ref) 键与 repo 亲和键）；
  - `ref` = 从操作后缀取出的 tag 或 digest，`/manifests/<ref>` 与 `/blobs/<digest>` 都取，
    `tags/list`、blob 上传会话、`/blobs/<digest>` 之外的都返回 `""`（见 `referenceOf`）。
- 成功后：`res.Namespace=Target.Namespace`、`res.Mode=proxy`、`res.EnginePath = "/v2/" + Target.EnginePath + op`、
  `res.NeedsSelection=false`，然后**照原流程**做鉴权与转发。
  操作后缀必须拼回去：`classifyCache` 依赖 `namespace.SplitOperation(res.EnginePath)` 里的 `manifests/`/`blobs/`/`tags/` 哨兵，
  否则缓存命中率指标全错。
- 失败映射：`ErrNotFound → 404 NAME_UNKNOWN`（消息带 `repo:ref`，且会被测试断言包含二者）、
  `ErrUnreachable → 503 UNAVAILABLE`、其它 error → 503 UNAVAILABLE（并打 Error 日志）。

## 2b. 无限定 blob 请求（无仓库名）的路由

客户端的 blob 请求**不带仓库名**：`/v2/blobs/<digest>`（manifest 之后取 config/layer），
甚至 `/v2/blobs/uploads/<id>`。旧实现被钉死在默认仓库所以看不出来。
`Resolve` 对这两种形态都返回 `Kind=repository`、`Repo=""`、`NeedsSelection=true`。
入口是 `dispatcher.decideUnqualified`，按有无仓库名分三条：

1. **有仓库名** → `decideProxy`（探测抉择，同第 2 节）。
2. **无仓库名但有 digest**（`/blobs/<digest>`）→ `decideBlob`：
   - 唯一一次查表：`chooser.DecideBlob(digest)` —— 近期 manifest 决策的**亲和**（返回带仓库级 `EnginePath` 的 Target），
     否则拒绝（见下面「尚未完成的一环」）。
   - 引擎路径由 `blobEnginePath(repoLevelPath, digest)` = `/v2/<repoLevelPath>/blobs/<digest>` 构造。
     **必须补回仓库段**：客户端省略了它，而引擎按「引用该 blob 的仓库」存取——同一仓库就是 manifest 取过的那个
     （含该 namespace 的 `official_library` 规则，如 `library/alpine`）。不补的话引擎在一个没有仓库的路径上找 blob，
     对磁盘上明明存在的内容回 404。
   - 只认 `ErrUnreachable`（→503 UNAVAILABLE）与其它 error（→503）；**这一路不会出现 404**——
     「不知道这个 digest 归谁」不等于「内容不存在」。
3. **既无仓库名也无 digest**（`/blobs/uploads/<id>` 续传等）→ 直接 503 UNAVAILABLE，
   **不探测抉择器**。理由写在 `decideUnqualified` 的注释里：亲和索引按仓库键、磁盘查询按 digest 键，两者都无从判定；
   而且上传本来就只能通过显式 namespace 寻址。用 503（可重试）而不是 404（声称内容不存在）。

重选：blob 与 manifest 同一套规则（GET/HEAD、失败才重选、至多一次），见第 3 节；
blob 的重试走同一个 `blobTarget`，所以路径同样带仓库段——已由
`TestUnqualifiedBlobRetryKeepsTheRepositorySegment` 钉住（客户端原始 op `/v2/blobs/<digest>` 绝不直接交给引擎）。

**磁盘命中但无亲和：刻意 503，不猜仓库**（Lead 的裁决，selection-dev 已实现）：
没有近期 manifest 决策、但某个 namespace 磁盘上已有该 digest 时，`DecideBlob` 只报告得出 namespace、
报告不出仓库——blob 是内容寻址的，`storage` 也没有「按 digest 反查仓库」的能力。
实测过猜的代价：把没有仓库段的路径交给真引擎，得到 `404 page not found`（更早一次是重定向环），
而随便挑一个仓库段等于拿别的仓库的路径取数据。所以取舍是**宁可 503 可重试，也不瞎拼路径**：
selection 侧返回 `ErrUnreachable`（日志 `source=blob-disk` 且 `repo=""`），dispatcher 侧再补一道防线
（`blobTarget` 见到空 `EnginePath` 会 Warn 并拒绝）。
影响范围：只有「亲和已过期、且期间没人重新取过该 manifest」的 blob 冷启动；正常 pull 流程（先取 manifest）不受影响，
端到端有测试。selection 侧由 `TestDecideBlobRefusesToGuessARepository` 钉住，dispatcher 侧由
`TestUnqualifiedBlobOnDiskWithoutARepositoryIsRefused` 与 `TestBuildServesUnqualifiedBlobFromDiskWithoutAffinity` 钉住。

## 2c. 带仓库名的 blob 请求（103 联调发现的缺陷）

`GET /v2/<repo>/blobs/<digest>` **必须按 blob 问，不能按 manifest 问**。
最初 `decideProxy` 对一切「有仓库名」的请求都走 `Decide(ctx, candidates, repo, ref)`，于是
`ref` 取到的是 blob 摘要，selection 去探 `/v2/<repo>/manifests/<blob 摘要>`——
问的是「有没有以该摘要发布的 manifest」，上游当然全 absent，一个各仓库都有的 layer blob 就这样 404 了。
103 上的原始读数：`GET /v2/library/busybox/blobs/sha256:034d6572… → 404`，
`selection … winner="" candidates=5 timings=["docker.io=…(absent)","gcr.io=…(absent)",…]`。

现在的分流（`decideProxy` 内，按 `op` 前缀）：

| `op` | 走哪条 | 探测路径 |
| :--- | :--- | :--- |
| `/manifests/<ref>` | `Decide` | `/v2/<候选 EnginePath>/manifests/<ref>` |
| `/blobs/<digest>`（repo 非空） | **`DecideBlobInRepo`** | `/v2/<候选 EnginePath>/blobs/<digest>` |
| `/blobs/uploads/…` | 不进 decideProxy 的 blob 分支（`blobDigestOf` 明确排除），按仓库级路径转发 |
| 无 repo：`/blobs/<digest>` | `DecideBlob`（见第 2b 节） | 亲和 / 拒绝 |
| 无 repo 也无 digest | 直接 503 | 不探 |

转发路径两处都用 `blobEnginePath(target.EnginePath, digest)` 拼（带仓库段），与第 2b 节同一个构造函数。
错误映射不变：`ErrNotFound → 404 NAME_UNKNOWN`、`ErrUnreachable → 503 UNAVAILABLE`
（统一收进 `writeSelectionFailure`）。重选也走同一分流：`retryTarget` 对 `op` 以 `/blobs/` 开头且 repo 非空的请求同样调
`DecideBlobInRepo`，绝不回到 manifests 探测。

固定用例：
- `TestRepoScopedBlobIsDecidedOnTheBlobPath`：命中时引擎收到 `/v2/alpine/blobs/<digest>`，
  且**断言没有任何探测走到 `/manifests/`**、A（开 `official_library`）被探的是 `/v2/library/alpine/blobs/<digest>`。
  做了变异验证：把 blob 分支关掉（恢复缺陷），该用例立刻红并直接点名
  `a probe went to "/v2/library/alpine/manifests/sha256:…": a blob must not be probed as a manifest`。
- `TestRepoScopedBlobNotFoundIsStillNameUnknown`：真的都没有 → 404 NAME_UNKNOWN（不是 503），且没有引擎被调用。

## 3. 重选（retry）规则

`dispatcher.tryReselect`，只在**无 namespace 前缀的读**上生效：

- 门槛四条，全中才重选：`chooser != nil`、方法为 `GET`/`HEAD`、
  `rec.status >= 404`（即 404 或 5xx）、且**第一次尝试还没写出 header**（`!rec.wroteHeader`）。
  写请求（POST/PUT/PATCH/DELETE）一律不重选；已经写出 header 的失败响应无法再替换，也不重选。
- 失效粒度：`ref != ""` 用 `chooser.Invalidate(repo, ref)`（同时清 (repo,ref) 与 repo 亲和）；
  `ref == ""`（blob 这类只带 digest 的请求没有 ref）用 `chooser.InvalidateRepo(repo)` 只清亲和。
- 然后**只再 Decide 一次**，不再循环。第二次的响应直接写给客户端：`retry.wroteHeader` 为真时
  把 `rec.status` 覆盖成第二次的状态（指标/日志按真实结果记）；第二次什么都没写时，
  客户端看到的是第一次引擎已经写出的响应——它已经在线上，再写一次 error 包络只会把第二个 JSON 追加到第一个 body 后面
  （`statusRecorder` 只挡 header，不挡 body）。这条权衡写进了 `tryReselect` 的注释。
- 为什么需要它：决策是**一次探测**，不是保证；上游可能在探测与取内容之间丢掉该仓库，
  或者被选中的 namespace 只能从过期的本地缓存回答。

## 4. 删掉的测试与替代覆盖

| 删除的用例 | 原来覆盖什么 | 替代 |
| :--- | :--- | :--- |
| `internal/config` `TestEffectiveListeners` | listeners 的主/额外监听器展开、`deriveExternalURL` 换端口 | 整特性删除；`TestRemovedKeysAreRejected` 断言带 `listeners:` 的配置**解码即报错**（严格 KnownFields） |
| `internal/config` `TestAnonymousRootValidation`、`TestAnonymousRootReachesTheListener` | `anonymous_root` 只能挂在 proxy namespace 的独立 socket 上 | 同上：`anonymous_root` 键已随 `Listener` 结构体消失，带该键的配置解码报错 |
| `internal/config` 校验用例「default namespace not defined」「duplicate listener」「listener namespace unknown」 | `namespaces.default` 必须存在于 proxy 表里、监听器地址查重、监听器 namespace 必须已定义 | 前两条对应特性删除；`TestRemovedKeysAreRejected` 覆盖 `namespaces.default` 的迁移报错。监听器端口查重随多监听器一起消失，仅剩 `metrics.listen != server.listen` 的校验（既有用例保留） |
| `internal/server` `TestDispatcherAnonymousRoot` | mirror socket 的 API 根放宽（匿名 200、错密码仍 401） | 特性删除。API 根契约仍由 `TestDispatcherAPIRootContract` 钉住（匿名 401 + 正确凭据 200） |
| `internal/namespace` `TestResolve` 的三条「default namespace rewrite」 | 无限定请求改写落到 docker.io 并注入 `library/` | 改成 5 条 `NeedsSelection` 用例（多段仓库、单段、显式 library、tags/list、blob），断言 Namespace/Mode/EnginePath 为空、Repo 是客户端原名、NeedsSelection=true；`library/` 注入改在 dispatcher 侧由 `TestSelectionAppliesOfficialLibraryPerCandidate` 覆盖 |
| `internal/namespace` `TestTablePredicates` 里的 `Default()` 断言 | 表能回答默认 namespace | 删断言；新增 `ProxyNames()` 由 dispatcher 候选构造与启动摘要使用 |

新增的 dispatcher 用例（`internal/server/dispatch_test.go`，用真实 `selection.Chooser` + 进程内 httptest 上游，
不 stub 决策）：

- `TestUnqualifiedRequestIsForwardedToTheSelectedNamespace`：两个候选、只有 B 有该仓库 → 转发到 B 的引擎，
  路径 `/v2/alpine/manifests/latest`（B 无 official_library，不加前缀），A 的引擎一次都没被调用。
- `TestSelectionAppliesOfficialLibraryPerCandidate`：只有 A 有（A 开 official_library）→ A 的引擎收到 `/v2/library/alpine/manifests/latest`。
- `TestUnqualifiedRequestNotFound`：两个候选都明确 404 → HTTP 404 + `errors[0].code == "NAME_UNKNOWN"`，
  消息含 repo 与 ref，且**没有**任何引擎被调用。
- `TestUnqualifiedRequestNotFoundWhenCandidatesOnlyChallenge`：两个候选都挑战、token 端点回 403（ghcr/gcr 的真实形态）
  → 仍是 404 NAME_UNKNOWN，且两个候选都被问到过。
- `TestUnqualifiedRequestUnreachable` / `TestUnqualifiedRequestUnreachableIsTransportOnly`：当选上游指向无人监听的地址
  → 503 UNAVAILABLE。后者与上一条成对，钉住「HTTP 状态算答复、网络层说不清才算不可达」这条线。
- `TestAnsweredCandidateDecidesWhenAnotherIsDown`：四行读数构成的那张表
  （A 不可达+B 404 → **404**；A 404+B 不可达 → **404**；都不通 → **503**；A 不可达+B **有** → **200**）。
  它钉住「答复过的候选说了算」以及「503 只留给谁都没答上话」，并显式记录这是裁决而非事故。
- `TestReselectOnReadFailure`：首次选中 A、A 的引擎 404、失效后 B 成为赢家 → 客户端拿到 B 的响应，B 恰好被调用一次。
- `TestNoReselectOnWriteRequests`：先跑一次真实请求把决策写进缓存，再直接调 `tryReselect`；
  POST 不重选、不回退缓存（A 的探测计数不变），紧接着用同一缓存的 GET **确实**重选（A 的探测计数增加）——
  后半段让前半段不是在断言一条死路径。
- `TestWriteToAProxyNamespaceIsRefused`：写进 proxy namespace 在鉴权阶段就被 403 `DENIED` 挡住，
  引擎看不到它（这也是为什么写请求的重选门槛要在 dispatcher 内部、而不是指望 POST 走到引擎）。
- `TestReselectHappensAtMostOnce`：两次都失败时，A 恰好一次、B 至多一次，不出现第三次。

blob 形态的用例（同文件，第 2b 节）：

- `TestUnqualifiedBlobFollowsTheManifestAffinity`：取一次无限定 manifest 建立亲和 → 无限定取 `/v2/blobs/<digest>`
  → 200 且引擎收到 `/v2/alpine/blobs/<digest>`（B 无 official_library，所以是 `alpine` 而不是 `library/alpine`），
  且**探测计数不变**（blob 不走探测）。
- `TestUnqualifiedBlobIsNotReportedAsMissing`：没有任何亲和、也没有磁盘命中 → 503 `UNAVAILABLE`，
  并显式断言**不是** 404。
- `TestUnqualifiedBlobOnDiskWithoutARepositoryIsRefused`：digest 只在 B 的磁盘上但无亲和 → 503，
  且**没有任何引擎被调用**（路径无法构造，不发送半成品路径）。
- `TestUnqualifiedBlobRetryKeepsTheRepositorySegment`：首次 blob 失败后重试 → 引擎看到的两个路径里
  **没有**客户端原始 `/v2/blobs/<digest>`，重试路径是 `/v2/alpine/blobs/<digest>`。

`internal/app/app_test.go`（真 distribution 引擎端到端）：

- `TestBuildServesUnqualifiedBlobAfterTheManifest`：manifest → config blob → 200，上游收到
  `/v2/library/alpine/blobs/<digest>`（该 config 里 docker.io 开了 official_library）。
- `TestBuildServesUnqualifiedBlobFromDiskWithoutAffinity`：把 blob 按 distribution 布局塞进 namespace 根、
  用 `probe_ttl: 1ms` 让亲和过期 → 断言 503（见第 2b 节「尚未完成的一环」）。

`test/e2e`（docs-dev 的 `TestUnqualifiedBlobFollowsTheManifest`，e2e tag）：
两个 namespace 各有自己的内容，先无限定取 manifest 建立亲和，再无限定取该 manifest 的 config blob；
断言 200 且**第二个上游没有流量**——即 blob 跟着亲和走，而不是被重新探测。本轮已跑绿。

配置侧新增：`TestRemovedKeysAreRejected`（`namespaces.default` / `listeners` 迁移报错）、
`TestSelectionKnobsOverrideDefaults`、`TestLoadMinimalAppliesDefaults` 里对 `probe_ttl`/`probe_timeout` 默认值的断言。

## 5. 实现中发现的三个真问题（已修 / 已记录）

1. **双重 `/v2/` 前缀**（已修，两次栽在同一处口径上）：`Candidate.EnginePath` / `Target.EnginePath` 是
   **不含 `/v2/` 的仓库级路径**（`library/alpine`）。我最初按「含 `/v2/`」构造，于是：
   - 第一轮：`"/v2/" + Target.EnginePath + op` → 引擎收到 `/v2//v2/library/alpine/manifests/latest`；
   - 第二轮改掉解包那处后，**候选构造**仍带 `/v2/`，于是探测 URL 变成 `/v2//v2/library/alpine/manifests/latest`,
     真实上游回 404 → 全候选判「不存在」→ 无限定请求静默返回 404。
   第二次只靠 httptest 假上游抓不到（假上游对路径不敏感），是**接真 distribution 引擎的端到端探针**抓出来的
   （见第 6 节 `internal/app/app_test.go`）。教训：「仓库级路径」这个词必须连同「含不含 `/v2/`」一起写死，
   否则两侧都以为对方负责前缀。
2. **探测的 401→token 路径：拒绝与不可达必须分开**（口径已修正）。
   早先 selection 侧把「token 端点连不上」也当成候选不可达，于是单个候选就足以把 404 拉成 503；
   103 上线实测暴露了它的代价——客户端拼错镜像名拿到 503，因为 ghcr.io/gcr.io 的匿名 token 端点恒回 403、
   quay.io 拿票后仍 401，三个候选恒被判 unreachable。
   现在的口径是：**注册表给出的任何 HTTP 状态都算「明确的不是」**（→404 NAME_UNKNOWN），
   只有**网络层**说不清才算不可达（→503）——连不上/超时/连接重置、token 端点不可达或回 5xx、候选没有当选上游。
   dispatcher 侧只按 `errors.Is` 分派，代码未变；变的是我夹具必须覆盖这两侧：
   `TestUnqualifiedRequestNotFoundWhenCandidatesOnlyChallenge`（两个候选都挑战、token 端点回 403 → 404）
   与 `TestUnqualifiedRequestUnreachableIsTransportOnly`（上游地址无人监听 → 503）成对钉住这条线。

   **紧接着又收敛了一次（Lead 裁决，selection 侧 `pick()` 已改）**：交集规则从「任一候选未答 → 503」
   改成「**无人答 → 503**」。即：任一候选 2xx → 当选；否则只要**至少一个候选给出了明确答复**（404/403/401）
   → 404 NAME_UNKNOWN；只有**所有候选都没能给出任何答复**（传输失败/超时/token 端点 5xx）→ 503。
   触发点是下面这张我量出来的表：多 namespace 配置下，**只要有一个 namespace 的上游抖动，另一个候选的「明确不存在」
   就被吞掉**，客户端拼错镜像名/拉不存在的 tag 拿到 503 并永久重试——与刚修掉的凭据拒绝那次是同一个用户可见症状，
   只是从传输层这条来路进来。取舍：404 的坏情况（没答话的候选恰好有它）发生时客户端本来也取不到内容，
   且 docker 拉 tag 每次重新解析、404 不会粘住。
   dispatcher 侧**未改一行实现**（只按 `errors.Is` 分派），只把固定用例翻面成
   `TestAnsweredCandidateDecidesWhenAnotherIsDown`（注释里留了四行读数与「这是决策不是事故」）。
   selection 侧新增了一条按同一四行读数做的 103 形状用例（五个真名候选：四个 absent + 一个 unreachable ⇒ 404），
   两边读数一致，见 `reports/raw/selection-notes.md` 的「第二次修订」小节。
   四象限至此各有固定用例：有该仓库 → 200；都没该仓库但有人答复 → 404；谁都没答复 → 503；
   「token 端点不可达/5xx」仍算「没人答」那一格。

   以下是这条口径修正前的旧记录，保留作为「为什么 404/503 的区分很敏感」的来由：
   假上游若对存在的仓库回 401 而 token 端点不可达，：假上游若对存在的仓库回 401 而 token 端点不可达，
   该候选被判为 unreachable。
   对我的意义是**测试夹具层面**的：我的假上游最终改成「不持有的仓库回 404，持有的回 401（token 端点由 `TestMain`
   起的 httptest 提供）→ 200」，这样 404 与 200 的耗时差固定，「谁赢」由夹具钉死而不靠竞态
   （更早一版让两个候选都回 200，胜负由探测耗时决定，用例因此偶发失败）。
3. **写进 proxy namespace 的请求根本到不了引擎**（已用测试钉住）：proxy 是只读缓存，`auth.Decide` 在
   转发前就回 403 `DENIED`。所以「写请求不重选」这条规则不可能靠 POST 端到端验证，
   必须直接驱动 `tryReselect`；`TestWriteToAProxyNamespaceIsRefused` 同时把这个前置事实固定下来。
4. **`/v2/` 前缀这次被我改错了方向**（已修，教训比 bug 值钱）：我为了让探测路径不带双重 `/v2/`，
   一度把**转发**路径也写成 `target.EnginePath + op`——但候选的 `EnginePath` 是**裸的**仓库级路径（`library/alpine`），
   `/v2/` 必须由 dispatcher 加。于是引擎收到 `alpine/manifests/latest`，而 `Resolve` 只认 `/v2/` 开头的路径，
   直接回 `not a registry v2 path`。**是我先前的 e2e 与 selection-dev 的独立 e2e 同时抓到的**（他们比我更早给出定位，
   报错里那句 `/library/demo/manifests/v1` 少了 `/v2/` 就是判据），修完两边都绿。
   教训：`EnginePath` 的「裸仓库级」性质在**同一个文件里被用了两次**（探测侧要裸的、转发侧要带前缀的），
   写注释也要把两侧都点到；只改一侧就会来回翻车（这个点位我前后错了两次）。
5. **blob 的仓库段必须补回**（已修）：客户端对 blob 用的是 `/v2/blobs/<digest>`（无仓库名），
   而引擎按仓库存取 blob。我第一版把客户端原始 op 直接交给引擎，于是「manifest 200、紧接着的 config blob 404」。
   修法是 `blobEnginePath(repoLevelPath, digest)` 一个构造函数，首次选择与重选共用；
   真实引擎的读数是判据：补回仓库段后上游确实收到 `/v2/library/alpine/blobs/<digest>` 并 200。
   同一条链上还发现：**空仓库段不是「无所谓」而是致命**——把 `/v2/blobs/<digest>` 交给真引擎会得到
   `404 page not found`（更早一次是重定向环），所以「没有仓库就不转发」是必须的，而不是可选优化。
6. **重试时「换一个候选」对 blob 是无效策略**（已删）：我一度在 blob 重选里跳过刚失败的 namespace，
   改试另一个——但另一个候选同样没有仓库信息，构造出的仍是空仓库路径，等于把半成品路径发给引擎。
   现在 blob 的首次与重试共用同一个 `blobTarget`（都走亲和），拿不到仓库就 503。

## 6. 测试可观测性的一个取舍（seam）

`internal/server/dispatch.go` 的 `dispatcher` 有两个函数字段，只在 `Server.newDispatcher` 里赋生产值：

```go
engineFor func(namespace string) (http.Handler, bool) // 生产: s.mgr.Handler
serving   func() []string                             // 生产: s.mgr.Serving
```

- 原因：未启动的 `*registry.Manager` 既没有引擎也没有 `Serving()` 结果，`registry` 又不在本任务写作用域内。
  有了这两个字段，dispatcher 的接线用例可以观察「哪个 namespace 的引擎收到了转发、路径是什么」，
  而不必为每个 namespace 起一个真实 distribution 引擎，也不需要给 `registry.Manager` 加测试后门
  （selection-dev 已确认不会加 `Set*ForTest`）。
- 生产路径没有任何行为变化：`Server.New` 一定把两者都指向 manager；`Handler()` 也只经 `newDispatcher` 构造。
  两个方法在字段为 nil 时分别退化为「找不到引擎」与「没有候选」，所以 `Server` 在测试里少配一个 manager 也不会 panic。
- `chooser` 是具体类型 `*selection.Chooser`（契约如此），不是接口。测试里的假上游是「实现 `selection.Upstream`
  的 httptest 上游 + 真 Chooser」，因此决策缓存、失效、探测路径这些行为是被真代码执行的，不是被 stub 掉的。
- 但假上游对路径不敏感这一点有代价：第 5 节第 1 条的第二个双重前缀就是它漏掉的。
  因此 `internal/app/app_test.go` 另有一组**真 distribution 引擎**的端到端用例（`Build` → `Listen` →
  真 HTTP 请求），覆盖「无限定请求 → 探测 → 转发 → 客户端拿到清单」，以及「全都不存在 → 404 NAME_UNKNOWN」。
  这是 `internal/app` 此前完全没有测试的补位。

## 8. 验收读数（本轮，全部来自本机实跑）

```
$ go build ./...                      → 干净
$ go vet ./internal/... ./cmd/...      → 干净
$ gofmt -l internal cmd                → 干净
$ go test ./internal/... -count=1      → 全部 ok
   ok  internal/app 0.053s   ok  internal/auth 3.565s   ok  internal/config 0.013s
   ok  internal/namespace 0.004s   ok  internal/obs 0.011s   ok  internal/registry 0.013s
   ok  internal/registryapp 0.030s   ok  internal/selection 0.558s   ok  internal/server 0.395s
   ok  internal/storage 0.006s   ok  internal/upstream 0.098s
$ go test -race ./internal/server/ ./internal/namespace/ ./internal/config/ -count=1 → 全绿
$ go test ./test/distbehavior/ -count=1 → ok
$ go test -tags e2e -count=1 ./test/e2e/... → ok 4.6s（含 docs-dev 的 TestUnqualifiedBlobFollowsTheManifest）

$ grep -rn "anonymous_root\|Namespaces.Default\|EffectiveListeners\|config.Listener" --include=*.go internal cmd
  → 无输出（exit 1）

$ go run ./cmd/rms config validate --config <新 schema>
  → ok / primary listener / storage root / selection probe_ttl: 45s / selection probe_timeout: 8s / proxy 行
$ go run ./cmd/rms config validate --config <旧 schema，含 namespaces.default>
  → exit 1，stderr: "line 6: field default not found in type config.Namespaces"（预期的迁移报错）
```

## 8b. 真实客户端（docker / ctr）本地探针：拿到了什么、卡在哪

为了在 103 复验前多一层证据，我起了一个本地栈实测真客户端：`python` 手写 registry stub（18081）→ rms（`rms serve`）→ 真 `docker pull` / `ctr images pull --plain-http`。
**结论分两半，必须分开看**：

**已证（客户端无关，来自 rms 与上游的日志）**：
- 多 namespace 下抉择正确。`selection` 决策行例如
  `repo=demo ref=v1 winner=docker.io candidates=2 timings=["docker.io=646µs","quay.io=559µs(absent)"]`——
  quay.io 的上游确实答了 absent，docker.io 有内容，所以它当选（不是靠耗时抢的）。
- **`official_library` 的单段前缀在真路径上生效**：客户端问 `/v2/demo/...`（单段）时，上游收到的是
  `HEAD/GET /v2/library/demo/manifests/v1`；客户端问 `/v2/library/demo/...` 时上游收到同一路径。
  这两条正是我这轮改动的核心，日志是直接证据。
- 引擎路径的仓库段在转发时确实被补回（blob 相关见第 2b 节，那部分由 `internal/app` 与 `internal/server` 的用例钉住）。

**没拿到（诚实记录）**：真客户端最终仍以 `not found` / `MANIFEST_UNKNOWN` 失败。
逐层排除后卡在：**我手写的 stub 返回的 manifest 被 distribution 的 proxy 引擎拒收**——
上游日志显示它确实 `HEAD` 然后 `GET` 到了我发的 manifest（OCI 与 Docker schema2 两种都试过），
但引擎回 `MANIFEST_UNKNOWN`，且**磁盘上不落任何 link**（换了全新 storage 根、且是进程内第一个请求，排除了旧负缓存）。
即失败发生在「取到了 manifest 之后」的引擎内部，不在选择/路径/转发这一层。
**未验证的是「引擎能不能吃下真 registry 的 manifest」，不是「路由对不对」**——前者需要 docker registry:2 之类能
真做 registry 协议的服务当上游，我没有在本机起它（环境里 docker 可用，但那是 103 部署形态的事，不在本任务范围）。
所以这一层证据我按「未完成」记，而不是按「通过」记；103 上那一步自然会覆盖它。

（探针脚手架写在 `.rmsprobe/`，跑完已删除；过程中我临时写过 `.docker/daemon.json`（insecure-registries）也已删除。
这类本地探针的脚手架不要留在工作区，否则会污染别人的 `git status`。）

## 8c. 排查用的决策字段（selection 侧提供，接线侧据此定位）

决策行 `timings` 现在带**短原因**，不开 debug 就能看出每个候选为什么输：

```
timings="[html=380µs(absent:not-manifest-content:text/html) refusing=611µs(absent:auth-refused) dead=220µs(unreachable:transport)]"
```

取值：`not-<kind>-content:<media type>`、`auth-refused`、`redirect`、`upstream-status`、`timeout`、`transport`、
`token-transport`、`token-status`、`token-answer`、`canceled`、`skipped`；
决策行另有 `kind`（`manifest` / `blob`）与 `source`（`probe` / `cache` / `decision-cache` / `affinity` / `blob-disk` / `none`）。

**103 复验的判据（已修正一次，见下）**：一条正确的 blob 请求应是
`repo=library/busybox ref=sha256:034d… kind=blob winner=<持有该 layer 的 ns> source=probe`，
`kind` 是**唯一**要看的字段（决策行与探测行都带）：

- `kind=blob` 出现在客户端问 `/v2/<repo>/blobs/<digest>` 的那一行 → 分流生效；
- **`kind=manifest` 而 `ref` 是 `sha256:…`（客户端问的是 `/blobs/`）→ 分流失效，那是我的链**；
- `winner=""` + `timings` 标 `(absent)` → 404（所有候选都明确说没有）；
  标 `(unreachable:…)` → 503（没人答上话），括号里的原因直接指出卡在哪一步。

**这条判据我第一版写错了，值得记下来**：我原本写的是「`timings` 里若出现 `/manifests/` 字样 → 分流失效」，
但 `timings` 的格式是 `namespace=耗时(结果[:原因])`（如 `docker.io=497µs(absent)`），**根本不含路径**——
`logDecision`/`logProbe` 的所有字段里也没有任何一个带请求路径（只有 `upstream` 主机）。
也就是说那条判据**永远不会亮**，而且会在最需要它的时候给出一份假的安心。是 selection-dev 核实后指出来的。
现在 selection 侧已给**探测的 debug 行**补上 `path` 字段（`msg="selection probe"` + `path="http://…/v2/…/blobs/sha256:…"`，
`TestProbeDebugLineCarriesThePath` 钉住），INFO 上仍靠 `kind` 区分。
教训：**写跨侧判据前先核对该字段到底装了什么**——我这次是照着「字段名像什么」推测的，没去看 `logDecision` 的实现。
因此我在 `internal/server/dispatch_test.go` 补了 `TestBlobDecisionLineSaysKindBlob`：用 `slog` 捕获抉择器**真实输出**，
断言 blob 请求产生 `kind=blob`、manifest 请求产生 `kind=manifest`。这样这条判据本身有证据支撑，
而不是又一次推测（变异验证：把 blob 分支关掉，该断言随之变红）。

### 一个跨侧耦合，别去「修」它
`internal/server` 的假上游对命中用**裸 `WriteHeader(200)`、不带 Content-Type**。这是刻意的容错面：
selection 侧的媒体类型校验（为修 103 上 dhi.io 的 302→GitHub HTML 误判而加）第一版把「没有 Content-Type」也判成
「不是 manifest」，本文件 12 条用例当场全红——裸 200 在 HEAD 下确实没有该 header。结论是**缺失或无法解析 = 没有证据 = 接受**，
过严那版已废。所以：**不要给假上游补 Content-Type**，那会把这份容错覆盖悄悄退掉。（已写进 `dispatch_test.go` 的注释。）

## 9. 遗留

- `internal/app` 原本没有测试；本任务补了 `internal/app/app_test.go`（真 distribution 引擎端到端，
  见第 6 节）。仍未覆盖的是「chooser 拿到的 TTL/Timeout 就是配置值」这一条的直接断言——
  现在它由端到端用例间接覆盖（`probe_timeout: 2s` 若没接上，探测会在默认 10s 上跑而不是用例里的 2s，
  但用例不含时长断言）。要直接断言需要一个能读取 chooser 内部配置的 seam，收益不大，先留着。
- `test/e2e` 已由 docs-dev 迁到新 schema，本轮 `go test -tags e2e -count=1 ./test/e2e/...` 全绿
  （含 `TestUnqualifiedBlobFollowsTheManifest`：manifest → config blob，断言第二个上游无流量）。
  它不在本任务写作用域内，这里只报告读数。
- `config/rms.yaml` 与 `deploy/103/rms.yaml` 里仍是旧的 `namespaces.default` 与 `listeners:` 段（含 15000–15006 端口绑定）。
  严格解码会让这两个文件**启动即报错**，这是预期行为；配置文件与部署编排不在本任务写作用域内。
- 上游凭据：`selection.Candidate` 只有 Namespace/EnginePath，探测用的凭据靠 `UpstreamCredentials(ns)` 从配置取。
  被选中的 namespace 上，私有用例的 `username/password` 是否会在引擎侧生效，本任务未验证（引擎装配在 selection/registry 侧）。
- `Selection.Std()`（`internal/config/selection.go`）返回的只是 `ProbeTTL`。接线侧没有用它（直接取
  `ProbeTTL.Std()`/`ProbeTimeout.Std()`），但这个名字容易误读成「两者都返回」，建议后续要么删要么改名。
- 无限定 blob 的「磁盘命中但无亲和」是**刻意 503**（见第 2b 节），不是待修缺陷。
  若将来要让这条也服务成功，前提是能确定 blob 所属仓库（例如给 `Upstream` 加「按 digest 反查仓库」），
  在拿到那个信息之前不要用「挑一个候选」的近似做法——它会把别的仓库的路径发给引擎。

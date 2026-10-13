# rms 上游级并发选路（racing）、digest 权威与负载均衡

日期：2026-10-13 · 范围：`internal/selection`、`internal/upstream`、`internal/registry`、`internal/server`、`internal/config`、`internal/obs`、`cmd/rms`、`deploy/103`、`test/e2e`

## 1. 做了什么（一句话）

一个 namespace 写了多个 `upstreams` 时，它们从「有序故障转移链」改成**并发候选**：每个 (namespace, upstream) 都发真实内容探测，最快且真有内容的胜出；同一 namespace 内以**配置顺序第一个答出内容的上游**为 digest 权威，digest 一致、延迟在窗口内的上游按 1/延迟**平滑加权轮询**；带 namespace 的读路径与无限定路径走同一套；pull 全匿名、只有 push 本地 namespace（`me`）需要认证。旧的顺序选举用 `upstream_race: false` 保留。

## 2. 算法

```
决策未命中缓存时（每次 probe_ttl 到期 / 新仓库）：
  候选对 = {(namespace, upstream)}                 ← 一个 namespace 的所有上游展开成对
  └─ 全部并发、同一 deadline；2xx 且媒体类型像内容才算「有」
  └─ namespace 内：配置顺序里第一个「有」的对 → 权威 digest
       （上游不发 Docker-Content-Digest 时，rms 取一次 manifest 自己 hash）
  └─ 该 namespace 可用集 = digest 一致的「有」对；无 digest 证据时只取最快的一个
  └─ namespace 之间：最快可用对胜（并列 → 上游 EWMA → namespace 名）
  └─ 胜出 namespace 的均衡池 = 可用集里延迟 ≤ balance_window × 最快者
  → Target{Namespace, EnginePath, Upstream, 池}，按 (仓库, tag/digest) 缓存 probe_ttl

决策窗口内（source=decision-cache）：逐请求从池里做一次平滑加权轮询，不再探测
命中本地磁盘（source=cache）：不探测，走该 namespace 的粘性引擎
digest-only blob（source=affinity）：跟随同仓库 manifest 决策的池
```

**软截止**（`decision_deadline`，默认 `0`=等全部）：第一个探测答出后再等这么久就用已有结果决策；未答的候选记为 `skipped(decision deadline)`，**不**算「没有内容」，也**不**会成为 digest 权威。

**引擎模型**：每个 (namespace, upstream) 一个 engine，懒构建（只在它被选中/答出内容后构建），共享同一 storage root。理由：distribution 的 proxy 只接受一个 `RemoteURL`，且 `realmAllowed()` 拒绝 realm 不在同一注册域的 Bearer 挑战（Docker Hub / ghcr 直接 401），所以「进程内 racing listener」不可行（`internal/upstream/selector.go` 顶部原有结论）。共享 root 的安全性：内容寻址 + 唯一临时文件原子改名（`distribution/v3/registry/storage/driver/filesystem` `PutContent`：`subPath.<uuid>.tmp` → `Move`）；proxy namespace 关闭 TTL 调度器，没有第二个调度器争用 `scheduler-state.json`；唯一共享可变对象是 tag link，digest 门保证只有同 digest 的引擎写它。

**「探活正常、取内容失败」的缺口已补**：内容探测结论回灌上游健康（`upstream.Selector.ObserveContentProbe`），`fail_threshold` 次连续「没答上来」→ 进 `2 × health.interval` 冷却，冷却期内不进候选；答上一次立即恢复。`/v2/` 健康探测不清这个状态（两套状态分开），否则 daocloud 那种形态会被健康探测反复「洗白」。代理读出现 `5xx` 也计一次内容失败（dispatcher）。

## 3. 配置（新增，均可选）

| 键 | 默认 | 说明 |
|---|---|---|
| `namespaces.selection.decision_deadline` | `0`（等全部） | 软截止，≤ `probe_timeout` |
| `namespaces.selection.balance` | `weighted` | `weighted` / `round_robin` / `off` |
| `namespaces.selection.balance_window` | `1.5` | 均衡窗口（相对最快可用对），≥ 1 |
| `namespaces.proxy.<ns>.upstream_race` | `true` | `false` = 退回旧的顺序选举 |
| `namespaces.proxy.<ns>.balance` | 继承全局 | 按 namespace 覆盖 |

`upstreams` 的语义变更：顺序不再是「优先级」，而是 **digest 权威顺序**（`upstream_race: false` 时才是旧的优先级语义）。

## 4. 指标（新增）

| 指标 | 含义 |
|---|---|
| `rms_upstream_probe_duration_seconds{namespace,upstream}` | 内容探测耗时（race 的判定依据；未答也记） |
| `rms_upstream_wins_total{namespace,upstream}` | 请求被交给哪个上游（均衡分布就是它） |
| `rms_upstream_digest_mismatch_total{namespace,upstream}` | digest 与权威上游不一致而被拒绝服务（>0 = 该镜像站过期） |
| `rms_selection_decisions_total{source}` | `probe` / `decision-cache` / `cache` / `affinity` |

决策日志新增 `upstream`、`digest`、`scoped_namespace`，`timings` 在同一 namespace 有多个上游时按 `namespace/host=…` 逐上游列出。

## 5. 证据（本次实测/门禁）

**单测**（`go test ./internal/...`，含 `-race`）：

- `internal/selection`：`TestRaceProbesEveryUpstreamAndServesTheFastest`（两个镜像都收到探测、快者服务）、`TestDigestAuthorityIgnoresAFasterMirrorThatDisagrees`（慢但权威的 mirror 永远服务，快的过期 mirror 一次都不服务）、`TestDigestAuthorityWhenMirrorsAgree`（同 digest → 30 次请求两边都有、快者份额更大）、`TestBalanceWindowKeepsAMuchSlowerMirrorOutOfRotation`（5× 慢者 20 次 0 流量）、`TestDecisionDeadlineBoundsTheFanOut`（慢镜像 2s 延迟下决策 < 500ms）、`TestBalanceOffKeepsOnlyTheFastest`、`TestQualifiedDecisionStaysInsideItsNamespace`、`TestQualifiedBlobFollowsItsManifestDecision`、`TestRefusalsAreReportedAsAnswersAndSilenceAsFailure`。
- `internal/selection/balance_test.go`：窗口/权重/模式/`pickPool` 1:2 加权在 30 次里精确 10:20。
- `internal/upstream`：`Upstreams()` 过滤（内容冷却、known-bad、全坏的兜底）、`ObserveContentProbe` 阈值与恢复、404（refusal）不算失败。
- `internal/registry`：`UpstreamsFor`/`ObserveContentProbe` 透传；`TestEngineBuildsOneEnginePerUpstream`（同一 namespace 两个上游两个引擎，未配置的上游回退粘性引擎）。
- `internal/server`：`TestQualifiedRequestIsServedByTheFastestUpstream`（带 namespace 请求被转发到快镜像）、`TestQualifiedUpstreamSelectionIsBestEffort`（上游全死时仍由粘性引擎服务，不 503）。
- `internal/config`：新键的解析/默认值/覆盖/校验（未知 balance、窗口 <1、deadline > probe_timeout 都报错）。

**e2e**（`go test -tags e2e`）：`TestNamespaceUpstreamRaceServesTheFastestMirror` —— 同一 namespace 两个真 distribution 上游、配置顺序故意慢的在前，一次无限定拉取由快镜像服务（日志：`winner=docker.io upstream=<fast> timings=[docker.io/<slow>=200.661ms docker.io/<fast>=1.074ms ghcr.io=511µs(absent)]`），决策窗口内第二次拉取慢镜像 0 额外请求。全套 e2e（登录契约、pull-through、选仓、push、鉴权矩阵、降级）保持绿。

**race detector**：`go test -race ./internal/selection/ ./internal/upstream/ ./internal/registry/ ./internal/server/` 全绿。

**README §2.1 的未决项收口**（同一台 101 上现场实测，dockerd 版本与线上一致）：镜像源指 `http://192.168.188.103:5000`（`/v2/` 仍 401 `Basic realm="rms"`），101 的 `~/.docker/config.json` 里**没有**该主机凭据（只有 `10.82.64.172:5001`、`l-rms.majorc.eu.org`），`docker pull hello-world:latest` 成功且 rms 侧 `rms_cache_hits_total{namespace="docker.io"}` 33 → 40（确实走缓存，不是回落官方）。机制侧：moby `registry.GetAuthConfigKey` 对 official index 一律用 `https://index.docker.io/v1/` 这个凭据键，所以 `docker login <镜像源>` 不作用于 `registry-mirrors`；`PingV2Registry` 的 `challengeManager.AddResponse` 容忍 401，真正决定死活的是**内容路径**是否匿名。

## 6. 边界与代价

- **跨 namespace 不做 digest 仲裁**：不同 namespace 是不同来源的注册表，仍按最快可用对竞争。
- **冷决策墙钟 = 最慢候选对**（上限 `probe_timeout`）。`decision_deadline` 是旋钮；放弃的候选不会成为 digest 权威，所以该 namespace 这次可能退化为「只服务最快一个、不均衡」。
- **并发宽度 = 候选对数**（deploy/103 现在是 10 个 namespace × 各自上游）。稳态不放大：`probe_ttl` 窗口内不重复探测，blob 按仓库复用 manifest 决策。**不做 blob body 级对冲**（不并发下载同一层）。
- **内存**随上游数增长，每个 namespace ≤ `len(upstreams)` 个引擎（懒构建）。
- **向上游是另一个开启鉴权的 rms 时，必须在该 namespace 配 `username`/`password`**（见 §7.1）：探测匿名可过，引擎的内容请求会带空 Basic 而被 401，症状是「探测说存在、取内容 404」。
- 旧语义（严格优先级顺序）用 `upstream_race: false` 保留；此时 `upstream.health.*` 仍决定当选者，内容失败不再影响选举（与本文档改动前的行为一致）。

## 7. 101 现场验收（已跑完）

脚本 `scripts/live-acceptance/run.sh` + 假镜像 `scripts/live-acceptance/fake-mirror.py`，原始读数 `reports/raw/101-live-upstream-race.txt`（2026-10-13T18:44，101）。它起一个**独立**的 rms（15100/19100，独立 storage root）与 4 个 loopback 假镜像，**不触碰 103 上正在服务的实例**；103 只作为只读上游参与，用来验证真实上游的探测与 digest 不一致判定。

配置：`docker.io` = [authority 假镜像(50ms), **真实 103**(~1.1s), stale 假镜像(6.5ms，内容不同)]；`example.test` = [slow(25ms), fast(20ms)，两者内容**完全相同**]。

| 判据 | 读数 |
|---|---|
| A 一个请求并发 5 条连接（2 namespace × 各自上游），日志 `candidates=5` | `timings=[docker.io/15104=51ms, docker.io/103=1.137s, docker.io/15101=6.5ms, example.test/15102=26ms(absent), example.test/15103=21ms(absent)]` |
| A digest 权威：**最快的** stale 镜像(6.5ms)不得服务 | 服务体 marker = `authority`（配置第一个可用上游）；stale 镜像只收到探测 `HEAD`，没有内容 `GET` |
| A digest 不一致计数 | `rms_upstream_digest_mismatch_total{docker.io, 15101}=1`、`{docker.io, 192.168.188.103:5000}=1`（真实 103 与该权威内容不同，同样被排除） |
| A 同 hash 才均衡：25ms/20ms 两台 | 13 次请求：slow=7、fast=11 次回源；`rms_upstream_wins_total` 5 : 8（偏向快者，两台都在用） |
| A 决策窗口内不重复并发 | `rms_selection_decisions_total{source="decision-cache"}=12`、`{source="probe"}=2` |
| A 指标与启动口径 | `rms_upstream_probe_duration_seconds_*` 有 4 台上游读数；启动行含 `upstream_race=docker.io,example.test`、`selection_balance=weighted`、`selection_decision_deadline=5s` |
| B pull 匿名 | `GET /v2/` → 401（挑战保留）；匿名读 `me/...` → 401；匿名写 `me/...` → 401；`me:mepw` 写 `me` → **202**；`alice` 写 `me` → **403**；写 proxy namespace → **403**；匿名写 `shared` → 401、任意登录用户 → 202 |
| C 环境异常时不含糊 | 假镜像未就绪时，启动探测判 unreachable，该上游**不进**首次 race（`candidates=4`），符合「健康循环会重新纳入」的设计 |

### 7.1 现场发现：把「开了鉴权的 rms」当上游，必须配 `username`/`password`

第一次运行时 `docker.io` 的内容路径返回 `404 MANIFEST_UNKNOWN`，`http.response.duration=3.7ms`（远小于探测的 534ms）。定位（读数 §E）：

```
HEAD <103>/v2/library/busybox/manifests/1.36.1   无凭据（探测发的）      -> 200
同上                                             空 Basic 对（引擎发的）  -> 401
同上                                             错误用户名密码           -> 401
```

- 选仓探测是**匿名**的，所以 103 被判「有内容」；但 embedded distribution 的 proxy 在建立 Basic 挑战后，会给上游请求带上（空的）`Authorization: Basic Og==`，而 rms 的鉴权策略是「凭据被拒绝时**绝不**回落到匿名」（README §4.2 的 C12，正是为了让 `docker login` 的错密码如实报错）→ 401 → distribution 的 `proxyTagService.Get` 回落到本地 tag store → `ErrTagUnknown` → 404。全程 3.7ms，因为根本没有第二次上游请求。
- 结论：**rms 链到另一个开启鉴权的 rms（或任何要求 Basic 的镜像）时，必须在该 namespace 配 `username`/`password`**（`dhi.io` 那条已有同样要求）。没有凭据的症状很隐蔽：探测说「有」，取内容 404。
- 这不是本次改动引入的（旧代码同样如此：`proxytagservice.go` 的行为与选举方式无关），已作为运维注意事项写进 README §6.2。

## 8. 103：候选镜像已构建并在 103 上烟测通过，切换待确认时间窗

在 103 上完成（**未动正在服务的容器**）：

| 产物 | 说明 |
|---|---|
| `rms:race-20261013-184731` | 新构建（`Dockerfile.binary` + 本次源码的静态二进制，离线用 103 本地缓存的 distroless 基镜像），25.6MB |
| `rms:rollback-20261013-184640` | 现役镜像 `5a2c0ca73118` 的备份标签（另有更早的 `rollback-20261012-232701`） |
| `config/rms.yaml.race-20261013-184731` | 现网配置 + 三个新键（`decision_deadline: 3s`、`balance: weighted`、`balance_window: 1.5`），已用新二进制 `config validate` 通过 |
| `config/rms.yaml.pre-race-20261013-184731` | 回滚用的配置快照（对**新旧两个二进制都合法**：不含新键） |
| `config/rms.yaml.bak-20261013-104935` | 改动前的原始配置（含下面那个非法上游，仅作记录） |

### 8.1 烟测（新镜像跑在 103 上，用现网配置，独立端口 15000/19100 + 临时数据目录）

```
GET /v2/ (anonymous)                        -> 401
unqualified library/busybox:1.36.1          -> 200 in 4.92s   ← 冷决策：candidates=20（10 namespace × 各自上游）
qualified docker.io/library/busybox:1.36.1  -> 200 in 1.61s   ← scoped_namespace=docker.io，candidates=3
same request again (decision cache)         -> 200 in 1.11s   ← source=decision-cache，不再并发探测
anonymous push me/...                       -> 401
anonymous push docker.io namespace          -> 403
```

决策行（现网 10 namespace 配置）：`winner=docker.io upstream=https://docker.1ms.run candidates=20 timings=[dhi.io/…, docker.elastic.co/…, docker.io/docker.1ms.run=1.649s, …]`；指标里 `rms_upstream_probe_duration_seconds_count` 对 20 个上游逐一有读数，`rms_selection_decisions_total{source=probe|cache|decision-cache}` 齐备。带 namespace 的第二次请求走了 `source=cache`（本地已有）+ 空上游 → 粘性引擎回落，符合设计。

### 8.2 现场发现：现网配置里有一处会让**任何**重启崩溃的非法上游

烟测第一次启动直接退出：

```
rms: /etc/rms/rms.yaml: namespaces.proxy[docker.elastic.co].upstreams[0]:
     invalid URL "docker.elastic.co": scheme must be http or https
```

`config/rms.yaml`（103，08:42Z 被改过）里 `docker.elastic.co` 的上游写成了裸主机名。**新旧两个二进制都拒绝它**（`docker run --entrypoint /usr/local/bin/rms rms:dev config validate` 同样报错）——现役容器之所以还活着，只是因为它启动时读的是**改之前**的内存配置；一旦因任何原因重启（崩溃、宿主机重启、`docker compose up -d`）就会 crash-loop。已就地修复为 `https://docker.elastic.co`（备份 `rms.yaml.bak-20261013-104935`），修复后新旧二进制 `config validate` 都通过。

> 这条与本改动无关，但**必须先修**才能重启：本次 rollout 的准备过程正好把它暴露出来了。

### 8.3 切换（2026-10-13 10:55Z 已执行）

计划获批后按上面的准备切换了 103 现网（`docker compose up -d`，容器 `Recreated` → `Started`）：

- **31s 后** `GET /v2/` → 401、`health=healthy`；`docker inspect` 显示 `restarts=0`，启动行确认新键生效：`selection_decision_deadline=3s`、`selection_balance=weighted`、`upstream_race=` 全部 10 个 namespace。
- **101 真实客户端（匿名、经 `registry-mirrors` → `http://192.168.188.103:5000`）**：`docker pull busybox:1.36.1` → `Downloaded newer image`（digest `sha256:73aaf090…`）；103 侧 `rms_cache_misses_total{namespace="docker.io"}` 从 0 → 2、`rms_cache_hits_total` → 5，证明走的是这个镜像源。
- **103 本地**：`/readyz` → 200；匿名 push `me/...` → 401；匿名 push proxy namespace → 403；匿名读本地 namespace → 401。
- **指标**：`rms_namespace_up` 10/10 = 1；`rms_selection_decisions_total{source="probe"}=5`、`{source="cache"}=4`；`rms_upstream_probe_duration_seconds_count` 覆盖 20 个上游；决策行形如 `kind=blob winner=docker.io upstream=https://registry-1.docker.io candidates=20 source=probe`。

**一处需要说明的读数**：在 103 上用**显式**镜像名拉取（`docker pull 127.0.0.1:5000/library/busybox:1.36.1`）会得到 `no basic auth credentials`。这不是回归——dockerd 对「显式 registry」会先 GET `/v2/`，rms 按设计回 401 `Basic realm="rms"`（README §4.2 的 C12），而 103 上没有该主机的凭据，于是客户端在内容请求之前就退出了（rms 侧没有内容请求记录）。A/B 实测：

```
OLD build (rms:rollback-20261013-184640) /v2/ -> 401 Unauthorized  Www-Authenticate: Basic realm="rms"
NEW build (live, rms:dev)                /v2/ -> 401 Unauthorized  Www-Authenticate: Basic realm="rms"
anonymous GET content path (mirror path) OLD -> 200 / NEW -> 200
```

即：**镜像源（`registry-mirrors` / containerd endpoint）路径匿名可用，显式镜像名需要 `docker login`** —— 与本次改动之前完全一致，也正是 §4.3 的配方。要「显式写主机名也能匿名拉」，只能放弃 `/v2/` 的挑战，而那会破坏 `docker login` 的密码校验与 push 的凭据携带（README §4.2 记录的两次实测）。

### 8.4 回滚（保留，一条命令）

切换会重启共享镜像源（101/102 等宿主与内部客户端的 `registry-mirrors` 都指向它），所以留给人确认：

```bash
# 回滚（旧镜像 + 不含新键的配置：对新旧二进制都合法）
ssh root@192.168.188.103 'cd /opt/stacks/rms \
  && docker tag rms:rollback-20261013-184640 rms:dev \
  && cp config/rms.yaml.pre-race-20261013-184731 config/rms.yaml \
  && docker compose up -d'
```

回滚后按 §8.1 的烟测口径复验一遍即可。

> 启动耗时：现网 10 个 namespace 逐个建引擎 + 探测，实测 **23–31s** 才 `rms ready`（健康检查 `start_period: 30s`、`retries: 5 × 15s` 足够）。切换窗口按 ~1 分钟留。

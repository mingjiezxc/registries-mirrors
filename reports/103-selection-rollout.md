# 103 上线记录：单端点 + 无限定请求并发挑选仓库

日期：2026-10-13 · 主机：192.168.188.103（公共容器环境） · 备份：`/root/rms-backup-20261012-232701/`

## 先看这里：客户端必须改（101 与 103 两台已迁）

> **关于「503 包络」的两种分支（必读）**：本报告在 103 上实取到的 503 是**「路径既无仓库名也无 digest、无法判定归属」**那一条（`/v2/blobs/uploads/abc-123`，原文 `reports/raw/103-final-live-10ns.txt` §3）。判据⑥提到的**另一条分支——「所有候选上游都不可达 → UNAVAILABLE」——在 103 上没有现场观测**：要制造它必须把 10 个 namespace 的上游全部弄成不可达，那会破坏线上服务。该分支由 `test/e2e` 的 `TestUnqualifiedAllUnreachable` 与 dispatcher 侧的 `TestUnqualifiedRequestUnreachableIsTransportOnly` 覆盖。

**旧的按 namespace 绑定端口 15000–15006 已全部下线**，同时删除了 `namespaces.default`。
- 指向这些端口的客户端（`daemon.json` 的 `registry-mirrors`、containerd 的 `registries.yaml`）会**立刻失效**（连接被拒），必须改：
  - docker：`registry-mirrors: ["http://192.168.188.103:5000"]`（只对 Docker Hub 生效；`alpine` 这类无限定名会由 rms 并发探测并落到 `docker.io`）；
  - containerd：镜像名带上 namespace（`192.168.188.103:5000/ghcr.io/foo/bar`），或改用 `hosts.toml` + `override_path = true` 的写法（见 `config/registries.yaml.example`）。
- 私有推送不受影响：`docker login 192.168.188.103:5000` 与 `docker push 192.168.188.103:5000/<user>/app:v1` 照旧。
- **已完成的迁移**：101 的 `/etc/docker/daemon.json` 的 `registry-mirrors` 改成 `http://192.168.188.103:5000`（备份 `daemon.json.bak-20261013-100452`，实测 `docker pull busybox:1.36.1` 经镜像成功）；103 自己的 `daemon.json` 改成 `http://127.0.0.1:5000`（备份 `daemon.json.bak-20261013-021532`）。**尚未迁移的**：本机之外还在指向 15000–15006 的客户端（例如 102/其它宿主）需要同样改指 `192.168.188.103:5000`。

## 回滚

```bash
ssh root@192.168.188.103
cd /opt/stacks/rms
# 1) 回到上一版镜像（上线前的镜像已打 tag）
docker tag rms:rollback-20261012-232701 rms:dev
# 2) 恢复上线前的配置
cp /root/rms-backup-20261012-232701/docker-compose.yml /root/rms-backup-20261012-232701/.env ./
cp /root/rms-backup-20261012-232701/rms.yaml config/rms.yaml
# 3) 重启
docker compose up -d
```
备份目录里另有 `image-id.txt`（上线前后镜像 ID）与 `htpasswd`（校验和 `69a0c8f9ff293797aab3f5b4fc5b934e`，与线上一致）。

## 上线前后对照

| 项目 | 上线前 | 上线后 |
| :--- | :--- | :--- |
| 容器端口 | 5000、9100、**15000–15006**、5001-5002(EXPOSE) | 5000、9100 |
| 配置键 | `namespaces.default: docker.io`、`listeners:`（含 `anonymous_root`） | 两者删除，新增 `namespaces.selection{probe_ttl:30s, probe_timeout:10s}` |
| 无限定请求 | 固定改写为 `docker.io` | 并发探测所有 proxy namespace，取真有该镜像且最快者（**上线当时是 5 个；2026-10-13 增配后交付态是 10 个**，见下「追加：新增 9 个上游 namespace」） |
| `:5000/v2/` | 401 挑战 | 401 挑战（未变） |
| 镜像 | `rms:dev` = 3bc123e229dd（已另存 `rms:rollback-20261012-232701`） | **现役 `rms:dev` = 5a2c0ca73118**（在 103 本地构建；五处修正：鉴权口径、「无人答复才 503」、上游 5xx/429 不算答复、带仓库名的 blob 走 blob 探测、2xx 还要按媒体类型确认内容）。构建序列：b2a9c2b846ca → 9ad639384d86 → db6d0948763a → 062d62246c1b → 9d528ac26182 → 5a2c0ca73118 |

## 验收读数（原文）

> **这一节是「增配前」的同一套检查**（5 个 proxy namespace、镜像 `062d62246c1b`）：第 1–7 点的 404 包络、503 包络、决策日志都取自那一轮。**交付态**（10 个 namespace、镜像 `5a2c0ca73118`、官方上游优先）的同一套读数在 `reports/raw/103-final-live-10ns.txt`，并在下面「2026-10-13 追加」与「第 5 个缺陷」两节里引用；两份的差别只在候选数与上游顺序，判据要求的要素（端口清单、5000 拉取命中缓存、404 与 503 包络、逐候选探测耗时）两份都有。

1) **端口**：`docker ps` 原文 → `0.0.0.0:5000->5000/tcp, :::5000->5000/tcp, 0.0.0.0:9100->9100/tcp, :::9100->9100/tcp, 5001-5002/tcp`（`5001-5002/tcp` 仅 EXPOSE、未发布）；15000–15006 逐个 `connection refused`（原文 `reports/raw/103-rollout-baseline-checks.txt` 第 1 节）。
2) **API 根**：`HTTP/1.1 401 Unauthorized` + `Www-Authenticate: Basic realm="rms"`（登录/推送语义未变；原文同文件第 2 节）。
3) **就绪**：`/readyz` → `ready`；**交付态的十条** `rms_namespace_up` 全为 1（增配前那一版是五条；原文分别是 `reports/raw/103-final-live-10ns.txt` 第 1 节与 `reports/raw/103-rollout-baseline-checks.txt` 第 3 节）。
4) **无限定 manifest（已缓存）**：`GET /v2/alpine/manifests/3.19` 三次重复均 `200`、8077 字节，耗时 1.136s / 1.115s / 1.088s（原文 `reports/raw/103-rollout-baseline-checks.txt` 第 4 节；上线首轮那次测得 1.83s，未落盘，故不引用）。
   决策日志（完整行，原文同文件第 6 节）：`{"time":"2026-10-13T01:35:55.22791565Z","level":"INFO","msg":"selection","repo":"alpine","ref":"3.19","winner":"docker.io","candidates":5,"source":"cache","cached":false}`，随后两次 `source=decision-cache cached=true`
   —— `source=cache` 说明先查本地磁盘命中、未发探测，`decision-cache` 说明 30s TTL 内复用；该文件同时记有 `rms_cache_hits_total{namespace="docker.io"} 19`（计数随清空/重启重置，故只看机制不看绝对值）。
5) **无限定子 manifest 与 blob（仓库名缺失路径）**，三次重复读数逐字取自 `reports/raw/103-child-manifest-and-blob.txt`（该文件逐条记录了实际命令与请求头）：
   `GET /v2/alpine/manifests/sha256:b58899…`（子 manifest，带 manifest Accept）→ 200 @4.599ms / 1.232ms / 1.252ms，各 1022 字节；同一路径**不带 Accept 时 404**（内容协商，对照行在同文件第 1b 节）；
   `GET /v2/blobs/sha256:83b2b670…`（**路径里没有仓库名**，blob 不需要 Accept）→ 200 @2.570ms / 2.752ms / 2.901ms，各 581 字节；
   无仓库名的 blob 的决策日志（完整行，逐字取自 `reports/raw/103-final-live.txt` 第 5 节）：`{"time":"2026-10-13T02:24:33.081666619Z","level":"INFO","msg":"selection","digest":"sha256:83b2b6703a620bf2e001ab57f7adc414d891787b3c59859b1b62909e48dd2242","winner":"docker.io","repo":"library/alpine","source":"affinity"}`
   —— 证明 blob 跟到了服务 manifest 的同一个 namespace，且仓库名由亲和补回。
6) **不存在的镜像（修复后复验，增配前的 5 候选轮次）**：`GET /v2/library/definitely-not-here-rms/manifests/latest` → **404 NAME_UNKNOWN**；耗时随上游/代理抖动：两份原始抓取各 3 次、共 6 次测量落在 **2.49–7.47s**（reports/raw/103-selection-rollout-live.txt 的三次 7.467/2.493/3.580s + reports/raw/103-final-live.txt 的三次 6.581/4.020/2.699s；另有独立评估者复跑的一次 5.19s）。**交付态（10 候选）的这一项是 5.466464/4.825984/10.002103s**，见 `reports/raw/103-final-live-10ns.txt` 第 20–22 行。
   决策日志**节选**（增配前那一轮：只删 `time`/`level` 两个字段；完整行见 `reports/raw/103-final-live.txt` 第 2 节。**注意这份是 5 候选轮次，五格全是 `absent`**；交付态（10 候选）同一请求的行里 `gcr.io` 是 `absent:auth-refused`、`ghcr.io` 是 `unreachable:upstream-status`，见 `reports/raw/103-final-live-10ns.txt` 第 26 行）：
   ```
   {"msg":"selection","repo":"library/definitely-not-here-rms","ref":"latest","kind":"manifest","winner":"","candidates":5,"source":"probe","cached":false,"timings":["docker.io=4.105094s(absent)","gcr.io=6.579239s(absent)","ghcr.io=1.739119s(absent)","quay.io=808.733ms(absent)","registry.k8s.io=504.281ms(absent)"]}
   {"msg":"selection","repo":"library/definitely-not-here-rms","ref":"latest","kind":"manifest","winner":"","candidates":5,"source":"probe","cached":false,"timings":["docker.io=4.01857s(absent)","gcr.io=1.898911s(absent)","ghcr.io=1.745391s(absent)","quay.io=815.536ms(absent)","registry.k8s.io=500.701ms(absent)"]}
   {"msg":"selection","repo":"library/definitely-not-here-rms","ref":"latest","kind":"manifest","winner":"","candidates":5,"source":"probe","cached":false,"timings":["docker.io=2.580712s(absent)","gcr.io=2.698011s(absent)","ghcr.io=1.754895s(absent)","quay.io=831.121ms(absent)","registry.k8s.io=526.799ms(absent)"]}
   ```
7) **无仓库名的 blob（两种情形要分清）**：
   - **有仓库亲和时**（正常路径，docker 先取 manifest 建立了亲和）：`GET /v2/blobs/sha256:83b2b670…` → 200；**若进程内没有该仓库的 manifest 决策（例如刚重启后直接按 digest 取 blob），这条路径会返回 503**（无法确定仓库归属就不猜，见 e2e `TestUnqualifiedBlobWithUnknownDigestIsUnavailable` 与 `TestUnqualifiedBlobOnDiskWithoutARepositoryIsRefused`，独立评估者现场复验也拿到了 503）。
     对**不存在的 digest** `sha256:000…0` 有两种情形，**别混为一谈**：
     - **有该仓库的亲和时**（本次实测的这一次）：`404`，响应体 `{"errors":[{"code":"BLOB_UNKNOWN","message":"blob unknown to registry","detail":"sha256:000…0"}]}`——由 docker.io 的引擎回答（原始抓取 `reports/raw/103-child-manifest-and-blob.txt` 第 3 节；**这是单次观测**，不是三次重复）。
     - **没有亲和时**（例如进程刚重启就直接按 digest 取）：`503`，`{"errors":[{"code":"UNAVAILABLE","message":"no configured namespace can be determined for blob sha256:000…0","detail":null}]}`，决策行 `digest=… winner="" repo="" source=none`（原始抓取 `reports/raw/103-mirror-pull-and-affinity.txt` 第 2 节）。这正是设计的选择：不知道仓库归属就不猜。
     这是对的：亲和已经确定仓库归属，引擎对「这个仓库里没有这个 blob」的回答就是 404。
   - **没有亲和、也没有磁盘命中时**：才走 503。**103 上实取到的 503 包络**（原文 `reports/raw/103-final-live.txt` 第 6/6b 节）：
     `GET /v2/blobs/uploads/abc-123`（既无仓库名也无 digest，无法判定归属）→
     ```
     HTTP/1.1 503 Service Unavailable
     {"errors":[{"code":"UNAVAILABLE","message":"cannot determine which namespace serves /v2/blobs/uploads/abc-123: the path names neither a repository nor a blob digest","detail":null}]}
     ```
     重复一次仍是 503。404 与 503 两种包络因此在 103 上都有原始观测。
   - 同一份 raw 文件还落盘了最近 6 条 selection 决策日志原文（`source=cache` / `decision-cache` / `probe` 三种来源都有）；**注意那 6 条是增配前抓的（`candidates=5`）**，交付态（`candidates=10`）的决策行见 `reports/raw/103-final-live-10ns.txt` 第 2 节。
8) **配置层面的破坏性变更（用新二进制直接验证）**：
   - 带 `namespaces.default` 的旧配置 → `decode config: yaml: unmarshal errors: line 4: field default not found in type config.Namespaces`；
   - 带 `listeners`/`anonymous_root` 的旧配置 → `field listeners not found in type config.Config`；
   - 103 上线用的配置本身通过校验（本地校验只差容器内的 `/etc/rms/htpasswd` 路径）。
9) **最终门禁**：`make check`（fmt/vet/全部单测，12 条 ok + 2 条 no test files、无 FAIL）、`go test -race -count=1 ./internal/selection/ ./internal/server/`（1.783s / 6.306s，取自 reports/raw/selection-gate-race-e2e.txt）、`go test -tags e2e -count=1 ./test/e2e/...`（顶层 11 RUN = 10 PASS + 1 SKIP、RUN 行合计 29，原始输出 `reports/raw/selection-gate-race-e2e.txt`）全部通过；原始输出见 `reports/raw/selection-gate-check.txt` 与 `reports/raw/selection-gate-race-e2e.txt`。

## 真实客户端联调（101 的 dockerd 经 103:5000 镜像）

上线后把 101 也迁到单端点（经人批准）：`/etc/docker/daemon.json` 的 `registry-mirrors` 从已下线的 `http://192.168.188.103:15000–15006` 改成 **`http://192.168.188.103:5000`**（备份 `daemon.json.bak-20261013-100452`），随后重启 dockerd。实测（原文 `reports/raw/101-real-client-and-blob-fix.txt`）：

- `docker info` → `Registry Mirrors: http://192.168.188.103:5000/`；
- `docker pull busybox:1.36.1` 成功（`docker.io/library/busybox:1.36.1`）；
- 103 侧确实收到了请求，且**带着 docker 的镜像命名提示**：`docker pull busybox` 在 103 侧留下三条 200 的 manifest 请求行（原文 `reports/raw/103-mirror-pull-and-affinity.txt` 第 1 节，逐字为 `200 /v2/library/busybox/manifests/1.36.0?ns=docker.io` 与两条按 digest 的 `…?ns=docker.io`）。rms 目前**不解析 `?ns=`**，靠并发探测照样选对了 namespace（`library/busybox` 只有 docker.io 有）；这条参数将来可用于省掉探测，但当前不是必需。

## 第 4 个缺陷：带仓库名的 blob 请求被当作 manifest 探测（真实客户端联调才发现）

> 构建方式的一处插曲（已解决）：中途发现 **103 自己的 dockerd** 的 `registry-mirrors` 也仍指向已下线的 `103:15000–15006`，在 103 上 `docker compose build` 会直接失败（原文：`Head "https://192.168.188.103:15000/v2/library/golang/manifests/1.27-alpine?ns=docker.io": dial tcp 192.168.188.103:15000: connect: connection refused`）。当时的绕行是先在 101 构建再 `docker save | ssh docker load` 送到 103。**经人批准后已修**：103 的 `daemon.json` 改为 `registry-mirrors: ["http://127.0.0.1:5000"]`、`insecure-registries` 加上 `127.0.0.1:5000`（备份 `daemon.json.bak-20261013-021532`），重启 dockerd 后 31 个容器自动恢复，随后在 103 本地重新构建成功（当时镜像 `062d62246c1b`，之后又因两处修正重建，最终为现役 `5a2c0ca73118`（同时把 Dockerfile 的 `EXPOSE` 从 `5000 5001 5002 9100` 改回 `5000 9100`，所以 `docker ps` 不再显示 5001-5002））。原始读数见 `reports/raw/103-dockerd-mirror-migration.txt`。

- **现象**：`GET /v2/library/busybox/blobs/sha256:034d6572…`（一个**确实存在**的 layer）→ **404**；selection 日志显示它在探 `/manifests/<layer 摘要>`，五个候选当然全 absent。
- **根因**：dispatcher 把 `/v2/<repo>/blobs/<digest>` 也交给「仓库+引用」的 `Decide`，于是用 manifest 路径去问一个 blob 摘要。
- **影响**：经 rms 取 blob 的客户端（containerd 的 `registries.yaml` 就是这样）会拿到 404。今天这次 docker 拉取之所以没暴露，是因为 docker 把 blob 直接发到 Docker Hub 了、没走 rms。
- **修复**：selection 侧新增 `DecideBlobInRepo`（探测路径改为 `/v2/<repo>/blobs/<digest>`，缓存键与 manifest 隔离，日志带 `kind:"blob"`），dispatcher 按 `op` 分流；回归用例断言上游**实际收到的路径**，把它改回 `/manifests/` 会立刻变红。
- **复验**（新镜像 `db6d0948763a`）：`GET /v2/library/busybox/blobs/sha256:034d6572…` → **200**，2,206,402 字节，9.7ms；日志 `kind="blob" winner="docker.io"`，第二次 `source=decision-cache cached=true`。

## 第 5 个缺陷：上游把 manifest 重定向到一个 HTML 落地页，被探针当成「存在」（加 dhi.io 后才暴露）

- **现象**：加上 `dhi.io` 后，无限定请求一个不存在的仓库时，决策日志里 `winner=dhi.io`（判成「存在」），客户端随后 404。危害不止于此：凡是这个上游「302 到落地页」的仓库路径，它都会赢下选择，**即使别的候选真有这个镜像**，客户端也会拿到 404。
- **根因（HTTP 轨迹）**：daocloud 的 dhi 镜像对这类路径回 **302 → `https://github.com/DaoCloud/public-image-mirror`（HTML）**，Go 客户端默认跟随重定向，探针看到的是 GitHub 的 **200 text/html**，于是「2xx = 存在」把它判成了 manifest。
- **修复**：探针按**最终响应的媒体类型**判定「像不像被探的东西」——manifest 的 2xx 必须是 JSON 系（HTML 一律不算），blob 的 2xx 只要不是 HTML 即可；媒体类型缺失/不可解析 = 没有证据 = 接受（过严会把可服务的 namespace 判成不存在，router-dev 的 12 条 dispatcher 用例当场红过，已回退到这个口径）。**保留跟随重定向**：实测 `registry.k8s.io` 的 manifest 正是 `307 → us-east4-docker.pkg.dev → 200 application/vnd.docker.distribution.manifest.list.v2+json`，不跟随会把这个当前可用的 namespace 永久判成不存在。
- **复现与验证**：`.probe/dhiprobe/main.go`（用真实 dhi 镜像 + 一个记录每次 HTTP 交换的 RoundTripper）。修复前 `winner=dhi.io err=<nil>`；修复后 `winner="" err=ErrNotFound`，日志 `dhi.io=…(absent:not-manifest-content:text/html)`。
- **交付态现场复验**（`reports/raw/103-final-live-10ns.txt` 第 2 节）：10 候选决策行里 `dhi.io=1.102377s(absent:not-manifest-content:text/html)`（该值取自当前 `reports/raw/103-final-live-10ns.txt` 第 26 行；此前那一版文件里的 813.528ms 已随镜像重建失效），`winner=""`，客户端收到 **404 NAME_UNKNOWN**——即该上游不再抢走选择。
- **顺带得到一个诊断能力**：`timings` 现在带短原因（`absent:not-manifest-content:text/html`、`unreachable:upstream-status`、`auth-refused`、`redirect`、`timeout`、`transport` 等），103 上 ghcr.io/gcr.io 显示 `(unreachable:upstream-status)`，即**它们现在的当选上游是 daocloud 镜像、该镜像对这些路径回 5xx**（不是官方 token 端点 403 被误判——那条按现行代码是 absent，有单测钉住）。

## 2026-10-13 追加：新增 9 个上游 namespace（daocloud 镜像 + k8s.gcr.io 别名）

按人的表格在 `deploy/103/rms.yaml` 里补齐（原 5 个 → 现 10 个 proxy namespace）：

| 客户端写的 namespace | 实际上游 | 实测（103 现场，显式 namespace 路径，manifest 取用） |
| :--- | :--- | :--- |
| `docker.io` | `docker.1ms.run` → `registry-1.docker.io` → `docker.m.daocloud.io` | 见下「两处刻意偏离」 |
| `ghcr.io` | `ghcr.io` → `ghcr.m.daocloud.io`（官方优先，见下） | `astral-sh/uv:latest` → **200**（走官方） |
| `gcr.io` | `gcr.io` → `gcr.m.daocloud.io` | `distroless/static:latest` → **200** |
| `quay.io` | `quay.io` → `quay.m.daocloud.io` | `prometheus/busybox:latest` → **200** |
| `registry.k8s.io` | `registry.k8s.io` → `k8s.m.daocloud.io` | `pause:3.9` → **200** |
| `docker.elastic.co` | `elastic.m.daocloud.io` | `elasticsearch/elasticsearch:8.15.0` → **200**，2.88s |
| `mcr.microsoft.com` | `mcr.m.daocloud.io` | `dotnet/runtime-deps:8.0` → **200**，2.20s |
| `nvcr.io` | `nvcr.m.daocloud.io` | `nvidia/cuda:12.4.0-base-ubuntu22.04` → **200**，4.83s |
| `k8s.gcr.io` | `registry.k8s.io`（**官方新域名，不是 daocloud**） | `pause:3.9` → **200**，7.79s |
| `dhi.io` | `dhi.m.daocloud.io` | 见下「dhi.io 需要凭据」 |

原文：`reports/raw/103-daocloud-namespaces-verify.txt`（含九个 daocloud 镜像的 `/v2/` 连通读数：全部 0.17–0.21s 回 401）。

### 两处刻意偏离：daocloud 全程排在官方之前没有被采纳

**第一处（docker.io）**：人的表格写 `docker.io → docker.m.daocloud.io`，我把它排在 **最后**（1ms → 官方 → daocloud），理由是仓库里已有两次实测（记为 negatives `n-u-fl1myg`、`n-u-l29jwy`）：daocloud 的失败形态是「**`/v2/` 探活正常、取内容失败**」——大 blob 被 302 到 `image-mirror.r2.daocloud.vip`（Cloudflare R2）后挂满 60s，且它的 token 绑客户端 IP、走代理直接 401。而 upstream 选举**只看 `/v2/` 探活**，所以把它放第一位会在探活健康的情况下拿不到层（这正是 negatives `n-u-s01do2` 记的那个缺口）。把它放第二位则只在 1ms 失效时才启用。**要改成第一位只需把 `deploy/103/rms.yaml` 里那三行重排**，代价见上。

**第二处（另外四个 namespace）**：最初我按「镜像优先、官方兜底」把 `ghcr.io`/`gcr.io`/`quay.io`/`registry.k8s.io` 配成 `[<daocloud 镜像>, <官方域名>]`。**103 上线后的决策行否证了这个顺序**：这两个 namespace 在无限定选仓里恒为 `unreachable:upstream-status`（即当选的 daocloud 镜像对这些路径回 5xx），而 `/v2/` 探活是好的——正是上面那段记录的「探活正常、取内容失败」。已改成**官方在前、镜像兜底**；改后同一请求里 `ghcr.io`/`gcr.io` 显示 `absent:auth-refused`（官方匿名 token 端点 403，是明确的「rms 服务不了」，不是「不知道」），语义更准。原始读数：`reports/raw/103-final-live-10ns.txt` 第 2 节。

### dhi.io 需要凭据（不是配置错）

四个候选镜像名（`alpine`/`alpine-base`/`nginx`/`python`）经 rms 都返回 **404 unknown tag**：rms 侧链路显示它**已成功与镜像建立 challenge 并拿到 token**，是上游说「没有这个 tag」。直接用镜像自己的令牌服务取票后复测两种路径形态（`/v2/dhi.io/alpine-base/...` 与 `/v2/alpine-base/...`）**都是 403**，且该镜像的 tag 列表被禁用（`{"name":"disable-list-tags","tags":[]}`）。结论：DHI 镜像在这一镜像源上需要**有权限的账号**。要用它，请在 `deploy/103/rms.yaml` 的 `dhi.io:` 下加 `username`/`password`（`ProxyNamespace` 支持），当前匿名只能得到 404/403。

### 代价：无限定请求的候选数从 5 变成 10

未命中缓存时，每个无限定请求要并发探测 10 个候选（实测决策日志 `candidates=10`，已缓存镜像仍在 1s 量级：`alpine` 1.72s，`source=cache`）。适配方法：调小 `namespaces.selection.probe_timeout`，或直接用显式 namespace 路径（那种请求不探测）。

## 其他缺陷与口径裁决

下面三条是上线前的联调（e2e + 真实上游）与上线后的实测抓出来的，不是猜测：

1. **探测请求没带 manifest 的 Accept**（ghcr.io 会把**存在**的镜像判成 404）；
   证据：假上游原文 `OCI manifest found, but accept header does not support OCI manifests`，以及真实上游对照（ghcr.io `astral-sh/uv` 不带 Accept 404×3 / 带 Accept 200×3）。已修，并留下「故意删掉 Accept 会变红」的回归测试。
2. **无限定 blob 请求无法寻址**：客户端取 blob 时不写仓库名（`/v2/blobs/<digest>`），首版第一次选择与重试都把空仓库路径交给引擎 → distribution 回 `404 page not found`。
   已修：首次与重试共用 `blobEnginePath(repoLevel, digest)`，仓库名由 manifest 决策的仓库亲和补回；拿不到仓库就 503，绝不发半成品路径。
3. **「鉴权走完的 401/403」被算成不可达**（上线后实测）：`timings` 显示 `gcr.io/ghcr.io/quay.io (unreachable)`、`docker.io/registry.k8s.io (absent)`，于是拼错的镜像名返回 503。
   根因：ghcr.io/gcr.io 的匿名 token 端点回 403、quay.io 拿到票后仍 401 —— 这是这些注册表**正常的鉴权回答**，不是上游故障。口径修正为：认证流程走完后的 401/403/404 一律算「明确不存在」，只有真正的传输层失败才算不可达。修复后复验：拼错的镜像名 → 404。
4. **口径裁决：多候选时「一个不可达 + 一个明确不存在」**。修掉第 3 条之后，同一个用户症状还有第二条来路：任何候选**传输层**抖动（103 上很常见，上游是公网 registry + 代理）都会把另一个候选的「明确 404」吞掉，拼错镜像名又变成 503。
   裁决改为「**无人答复才 503**」：任一候选 2xx → 选它；否则只要至少一个候选给出了明确答复（401/403/404 等状态判决）→ 404 NAME_UNKNOWN；只有所有候选都没能给出任何答复（传输失败/超时/token 端点 5xx）→ 503 UNAVAILABLE。
   依据：两个方向代价不对称——503 会让客户端**永久重试一个根本不存在的镜像**；404 的坏情况（那个没答话的候选恰好有它）发生时它本来也取不到内容，且 docker 拉 tag 每次都会重新解析，404 不会粘住，粘性风险主要在前置 HTTP 缓存。四象限（有该仓库 / 有人答复 / 谁都没答）各有固定用例，其中一条直接照抄 103 的候选组合（四答一挂 ⇒ 404）。
   复验：103 上拼错的镜像名仍为 404。

## 仍未确定

- `probe_timeout` 10s 在 103 上是**上限而非余量**：未命中缓存时每个无限定请求要等一轮探测。**交付态（10 候选）**实测 5.466464/4.825984/10.002103s（`reports/raw/103-final-live-10ns.txt` 第 20–22 行，最慢一次正好撞到 10s 上限）；**增配前（5 候选）**是 2.49–7.47s（当时的 `103-selection-rollout-live.txt` 与 `103-final-live.txt`，只作历史对照）。
- Basic 认证上游（challenge 里没有 realm）会让该候选恒判探测失败 → 相应请求 503；当前配置的六个上游都是 Bearer，未触发。
- 「磁盘上已有该 blob、但进程内没有仓库亲和」的 blob 请求会 503（不猜仓库）；docker 正常路径不受影响（一定先取 manifest）。

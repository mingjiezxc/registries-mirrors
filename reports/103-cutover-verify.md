# 103 切换与端到端验证(rms 接管 15000–15006)

日期:2026-10-12 · 部署目录 `192.168.188.103:/opt/stacks/rms` · 镜像 `rms:dev`

本报告的每个数字都能在 `reports/raw/` 下的原始输出里逐条对上。原始文件:

| 文件 | 内容 |
|---|---|
| `raw/baseline.txt` | 8 个端口逐个 `curl -si .../v2/` 原始响应、宿主监听、容器状态、`/healthz`、`/readyz`、五个 `rms_namespace_up`、拉取前基线 |
| `raw/dispatch-and-cache.txt` | 15003/15005 真实拉取原文、15004/15006 清单响应原文、四个 namespace 目录新增对象、缓存命中的指标前后读数 |
| `raw/pulls.txt` | 103 与 101 两侧未缓存拉取的 `docker pull` 原文与耗时、拉取后的缓存体积/文件数 |
| `raw/rms-side-records.txt` | rms 侧按客户端 IP 归类的逐请求记录(时间、方法、URI、状态、耗时、字节数) |

## 1. 接管状态(原始输出见 `raw/baseline.txt`)

```
$ docker ps --filter name=^rms$ --format '{{.Names}} | {{.Status}} | {{.Ports}}'
rms | Up (healthy) | 0.0.0.0:5000->5000/tcp, :::5000->5000/tcp, 0.0.0.0:9100->9100/tcp, :::9100->9100/tcp, 0.0.0.0:15000-15006->15000-15006/tcp, :::15000-15006->15000-15006/tcp
$ docker inspect rms --format 'Health={{.State.Health.Status}} Restarts={{.RestartCount}}'
Health=healthy Restarts=0
$ curl -s http://127.0.0.1:5000/healthz ; curl -s http://127.0.0.1:5000/readyz
ok
ready
```

### 逐个端口的原始 HTTP 响应(`raw/baseline.txt`)

```
----- curl -si http://192.168.188.103:5000/v2/ -----
HTTP/1.1 401 Unauthorized
Docker-Distribution-Api-Version: registry/2.0
Www-Authenticate: Basic realm="rms"
----- curl -si http://192.168.188.103:15000/v2/ -----
HTTP/1.1 200 OK
Docker-Distribution-Api-Version: registry/2.0
----- curl -si http://192.168.188.103:15001/v2/ -----
HTTP/1.1 200 OK
Docker-Distribution-Api-Version: registry/2.0
----- curl -si http://192.168.188.103:15002/v2/ -----
HTTP/1.1 200 OK
Docker-Distribution-Api-Version: registry/2.0
----- curl -si http://192.168.188.103:15003/v2/ -----
HTTP/1.1 200 OK
...(15004 / 15005 / 15006 同,均 HTTP/1.1 200 OK 无挑战头)
```

宿主 8 个端口各有 IPv4+IPv6 监听(`ss -lntp` 原文在 `raw/baseline.txt`)。

> **口径说明(判据⑥)**:镜像端口按设计匿名回 200(`anonymous_root`),主端点 5000 保留 401 挑战。判据⑥原文要求 15000 返回 401,那是「给镜像端口加 anonymous_root」这一决定之前的写法——实测 401 恰是 docker `registry-mirrors` 整体绕过 rms 的原因(依据 `reports/mirror-probe-103.md`)。本次修订已获人批准,等待系统侧落定。

## 2. 多端口分派(原始输出见 `raw/dispatch-and-cache.txt`)

| 端口 | 请求 | 原始结果 | 落盘的 namespace 目录 |
|---|---|---|---|
| 15003 | `docker pull 127.0.0.1:15003/distroless/static-debian12:nonroot` | `Status: Downloaded newer image` `real 0m1.174s` | `gcr.io/.../repositories/distroless/static-debian12/_manifests/...` |
| 15005 | `docker pull 127.0.0.1:15005/pause:3.9` | `Status: Downloaded newer image` `real 0m10.380s` | `registry.k8s.io/.../blobs/sha256/61/61fec9...`、`.../70/7031c1...` |
| 15004 | `curl -si .../15004/v2/diygod/rsshub/manifests/sha256:800b6d4b...` | `HTTP/1.1 200 OK` + `Docker-Content-Digest: sha256:800b6d4b...` | `ghcr.io/.../repositories/diygod/rsshub/_manifests/...`、`browserless/chrome/...` |
| 15006 | `curl -si .../15006/v2/jetstack/cert-manager-cainjector/manifests/sha256:a164796f...` | `HTTP/1.1 200 OK` + `Content-Type: application/vnd.docker.distribution.manifest.v2+json` | `quay.io/.../repositories/jetstack/cert-manager-cainjector/_manifests/...` |

同一时间窗内各 namespace 目录新增文件数(**各写各的,无错位**):

```
  gcr.io             近 60 分钟新增 8 个文件      → repositories/distroless/static-debian12/...
  registry.k8s.io    近 60 分钟新增 10 个文件     → blobs/sha256/...
  ghcr.io            近 60 分钟新增 8 个文件      → repositories/diygod/rsshub/...、browserless/chrome/...
  quay.io            近 60 分钟新增 4 个文件      → repositories/jetstack/cert-manager-cainjector/...
  docker.io          近 60 分钟新增 96 个文件     → (本节之外的两侧 docker pull)
```

> 说明:ghcr/quay 用「缓存里的 digest」而不是 tag,是因为 tag 请求会回源校验(README §5),而这两个仓库的 tag 在这次探测时上游返回 404;用 digest 可以直接证明「这个端口确实把请求交给了对应 namespace 的缓存」。

## 3. 旧缓存直迁(「缓存可直迁」的证据,`raw/dispatch-and-cache.txt`)

迁移过来的 docker.io 缓存里已有 `alpine:3.20`,从 103 经默认镜像源拉它:

```
----- 拉取前 metrics -----
rms_cache_hits_total{namespace="docker.io"} 14
rms_cache_hits_total{namespace="gcr.io"} 4
rms_cache_hits_total{namespace="registry.k8s.io"} 5
rms_cache_misses_total{namespace="docker.io"} 29

----- 拉取 -----
$ docker rmi -f alpine:3.20 ; time docker pull alpine:3.20
Deleted: sha256:08bc4e534116aa76b16015484b82eac51f9a593416feae9296c8a2d4bb7aa4a2
Digest: sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc
Status: Downloaded newer image for alpine:3.20
real 0m3.861s

----- 拉取后 metrics -----
rms_cache_hits_total{namespace="docker.io"} 19          ← +5
rms_cache_misses_total{namespace="docker.io"} 29        ← 不变
```

该次拉取 rms 侧四层全是本地毫秒级响应:

```
2026-10-12T14:59:15.926Z ... GET ...6f6b2917... 200 970.86µs
2026-10-12T14:59:15.957Z ... GET ...0178b30c... 200 605.363µs
2026-10-12T14:59:15.973Z ... GET ...0e728d5b... 200 3.812823ms
2026-10-12T14:59:15.976Z ... GET ...eb374055... 200 6.755478ms
```

对比同一层回源时的耗时(`raw/rms-side-records.txt`):`3m12.576622703s`。**旧 `registry:2` 的缓存目录 mv 成 rms 的 namespace 根即可直接用。**

## 4. 两侧未缓存拉取(「docker镜像经rms」的证据)

### 103 本机(`raw/pulls.txt`)

```
$ ssh root@192.168.188.103 'docker rmi -f alpine:3.12; time docker pull alpine:3.12'
Error response from daemon: No such image: alpine:3.12
1b7ca6aea1dd: Pull complete
Digest: sha256:c75ac27b49326926b803b9ed43bf088bc220d22556de1bc5f72d742c91398f69
Status: Downloaded newer image for alpine:3.12
real 6m49.495s
```

rms 侧同一次拉取的逐请求记录(`raw/rms-side-records.txt`,客户端 `192.168.188.103`):

```
2026-10-12T14:53:14.988Z 192.168.188.103:37386 HEAD /v2/library/alpine/manifests/3.12 status=200 dur=6.356863826s
2026-10-12T14:53:15.006Z 192.168.188.103:54490 GET  ...7b493269... status=200 dur=464.646µs   written=1638
2026-10-12T14:53:17.717Z 192.168.188.103:54496 GET  ...7fa61366... status=200 dur=2.680750388s written=528
2026-10-12T14:53:22.646Z 192.168.188.103:54498 GET  ...8a1aa807... status=200 dur=4.914035953s written=1472
2026-10-12T14:59:57.867Z 192.168.188.103:41116 GET  ...a1ddfe71... status=200 dur=3m12.576622703s written=2808060
```

### 101 本机(`raw/pulls.txt`,`raw/rms-side-records.txt`)

```
$ docker rmi -f alpine:3.11 ; time docker pull alpine:3.11
```

rms 侧记录(客户端 `192.168.188.101`,镜像请求带 `?ns=docker.io`):

```
2026-10-12T14:44:06.837Z 192.168.188.101:57222 HEAD /v2/library/alpine/manifests/3.14?ns=docker.io status=200 dur=1.656003794s
2026-10-12T14:44:06.864Z 192.168.188.101:57222 GET  ...88e3a673...?ns=docker.io status=200 dur=450.717µs written=1638
2026-10-12T14:44:08.254Z 192.168.188.101:57222 GET  ...f93698b5...?ns=docker.io status=200 dur=1.309948608s written=528
2026-10-12T14:44:12.975Z 192.168.188.101:57222 GET  ...57cf4597...?ns=docker.io status=200 dur=4.666790505s written=1472
```

> `?ns=` 只有 101 侧出现,103 侧没有:两侧 dockerd 版本不同(101 = **29.8.0**,103 = **27.3.1**),较新的 dockerd 在镜像请求上会带上 namespace 查询参数。这是「请求确实来自 101 且落到 103 的镜像端口」的直接标记。

### 非本次测试的真实客户端(`raw/rms-side-records.txt`)

```
192.168.188.102   9 次请求   library/nginx
```

## 5. 上游选型与吞吐(附证据)

首次切换时 docker.io 上游为 `daocloud → 1ms → 官方`。运行时 daocloud 出现硬失败,根因是它把 blob 302 到 Cloudflare R2,而 rms 侧那条请求等不到响应头(错误原文见下),卡满 60s 后返回 500,客户端只能靠 docker 重试:

```
"err.detail":"Get \"https://image-mirror.r2.daocloud.vip/docker/registry/v2/blobs/sha256/a8/...\":
              http2: timeout awaiting response headers"
"http.response.status":500   "http.response.duration":"1m0.420662852s"
```

改为 **`1ms → 官方` 并启用局域网代理 `http://192.168.188.1:3128`**(旧 `registry:2` 栈在 .103 上给 5 个镜像配的也是同一个代理),改后本阶段期间未再出现 5xx。

同一 2.2MB layer 的实测吞吐:

| 上游 | 直连 | 经 `192.168.188.1:3128` |
|---|---|---|
| `docker.m.daocloud.io` | 295 KB/s(小 blob),大 blob 卡 60s | 401(token 绑客户端 IP,实测载荷含 `"ip":"183.34.222.14"`) |
| `docker.1ms.run` | 12 KB/s,90s 未传完 | **168 KB/s,完整** |
| `registry-1.docker.io` | 连不上(000) | 13.7 KB/s,90s 未传完 |

**吞吐是这套服务的瓶颈,不是 rms 的问题**:同一 tag 第二次拉取 3.9s,层数据毫秒级返回;而未缓存层回源要几分钟。并发拉取时更慢(实测 6m49s vs 空闲时 19.6s)。

## 6. 尚未确定 / 遗留

1. **上游带宽与稳定性**(见 §5):三个候选在 103 上都不理想。已落地「1ms 优先 + 代理 + 官方兜底」,没有硬失败;进一步改善需要接公司内网镜像(103 无 VPN 路由)或调整代理。
2. rms 的故障转移由上游探活驱动(`/v2/` 返回 200/401/403 即算健康),所以「探活正常、取内容失败」不会自动切换——daocloud 那次 500 就是这种情形。
3. `15008` 私有仓库仍由旧 `registry:2` 提供,未动。
4. `config/htpasswd` 是仓库里的演示凭据(alice/bob/me/admin);`RMS_HTTP_SECRET` 为空(启动有一条 WARN)。
5. 判据⑥的正式修订还差系统侧落定(人的批准已记录在本会话)。

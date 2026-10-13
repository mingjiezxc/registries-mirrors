# 103 上的 rms 镜像服务(接管原 registry:2 栈)

## 现状与目标

`192.168.188.103` 上原本用 `/opt/stacks/registry/docker-compose.yml` 跑着 **8 个 `registry:2` 容器**提供镜像服务,`101` 与 `103` 的 `/etc/docker/daemon.json` 曾把其中 7 个当 `registry-mirrors` + `insecure-registries`:

| 旧容器 | 旧端口 | 角色 | 现在 |
|---|---|---|---|
| `reg-docker` | 15000 | `registry-1.docker.io` 代理 | 端口下线,内容归 `docker.io` namespace |
| `reg-daocloud` | 15001 | `docker.m.daocloud.io` 代理 | 端口下线,上游已弃用(见 rms.yaml 注释) |
| `reg-1ms` | 15002 | `docker.1ms.run` 代理 | 端口下线,上游成为 `docker.io` 的首选候选 |
| `reg-gcr` | 15003 | `gcr.io` 代理 | 端口下线,内容归 `gcr.io` namespace |
| `reg-ghcr` | 15004 | `ghcr.io` 代理 | 端口下线,内容归 `ghcr.io` namespace |
| `reg-k8s` | 15005 | `registry.k8s.io` 代理 | 端口下线,内容归 `registry.k8s.io` namespace |
| `reg-quay` | 15006 | `quay.io` 代理 | 端口下线,内容归 `quay.io` namespace |
| `registry` | 15008 | 私有仓库(可删 + CORS),**不在本次范围内,保持原样** | 不变 |

现在用**一个** `rms` 进程接管上面七个上游的代理缓存,对外只开 **5000**(主端点)与 **9100**(指标)。

## 目标架构

| 端口 | 作用 | 缓存目录 |
|---|---|---|
| 5000 | 主端点。路径里写 namespace(`docker.io/library/alpine`)就直连该 namespace;不写 namespace(`library/alpine`)则由 rms 并发探测所有 proxy namespace,挑真有该镜像、响应最快的那个 | `data/<namespace>/`、`data/<user>/` |
| 9100 | 指标 | — |

| namespace | 上游(默认**并发竞争**；顺序 = digest 权威顺序) |
|---|---|
| `docker.io` | `https://docker.1ms.run`、`https://registry-1.docker.io`、`https://docker.m.daocloud.io` |
| `gcr.io` / `ghcr.io` / `quay.io` / `registry.k8s.io` | 官方地址在前、``*.m.daocloud.io`` 兜底 |

`docker.io` 上游的选择理由与实测数据见 `deploy/103/rms.yaml` 里该 namespace 的注释。

**与仓库根目录的部署形态一致**:根 `docker-compose.yml` + `.env.example` 现在也是同样的单端点
(只发布 5000 与 9100),两边不会再分叉。早先根 compose 曾额外映射 5001/5002 做「按 namespace 绑定
的镜像端口」,已随 `listeners` 配置块一起删除 —— 根目录那份 `.env.example` 里的
`RMS_PORT_DOCKERIO` / `RMS_PORT_GHCR` 也一并去掉了。

## 2026-10-13:单端点化(本次变更)

两件事被彻底删除,**旧配置与旧客户端配置都需要改**:

1. **`namespaces.default` 删除**。无限定请求不再改写成一个固定仓库,改为并发探测所有
   proxy namespace、挑真有该镜像且最快的一个(`namespaces.selection` 控制探测缓存与超时)。
2. **`listeners` 与 `anonymous_root` 整体删除**。15000–15006 这七个按 namespace 绑定的端口下线,
   只剩 5000 与 9100。

代价(必须写清楚,别指望客户端"什么都不用改"):

- docker 的 `registry-mirrors` **只对 Docker Hub 生效**,而且必须是裸 `host:port`。指到
  `http://192.168.188.103:5000` 后,它发的无限定名(`library/alpine`)由 rms 自动挑到
  `docker.io` —— 因为只有 `docker.io` 有 `library/alpine`,所以 Docker Hub 的拉取仍然命中缓存。
- 但**其它上游的无限定名不再可靠**:镜像只在 `ghcr.io` 上时,无限定请求可能被挑到别的
  namespace,或者谁都对不上,返回 `404 NAME_UNKNOWN`。可靠写法是镜像名里带 namespace:
  `192.168.188.103:5000/ghcr.io/owner/app:tag`。
- containerd 的 `registries.yaml` 里按上游分端口的写法(`ghcr.io` → `...:15004`)**已经不可用**,
  替代是镜像名带 namespace 前缀,或改用 `hosts.toml` + `override_path = true` 把某个上游钉在
  路径上(见 `config/registries.yaml.example` 末尾)。
- `anonymous_root` 没有了,所以 `/v2/` 一律挑战(401)。**dockerd 会不会因此放弃这个镜像源,没有
  实测过**(当年加 `anonymous_root` 就是因为实测到它会放弃,但那次的内容路径也在挑战,配置与现在
  不同)。上线后必须确认它确实吃到了缓存:
  `docker pull <host>:5000/library/alpine` 之后看 `rms_cache_misses_total` 与 `du -sh data/docker.io`
  是否增长。若没有,退路是镜像名带 namespace(`<host>:5000/docker.io/library/alpine`)。

## 部署

```bash
# 在 101(仓库所在机)执行:同步仓库到 103:/opt/stacks/rms 并在那边构建镜像
deploy/103/install.sh

# 首次接管时,按 reports/103-rms-deploy.md 里的顺序:
#   1) 摘掉 7 个旧镜像容器(保留 15008)
#   2) 把旧缓存 mv 成 rms 的 namespace 根
#   3) 在 103:/opt/stacks/rms 执行 docker compose up -d
```

缓存搬迁映射(mv 是同一文件系统内的改名,不复制数据):

```
/opt/stacks/registry/data/docker  ->  /opt/stacks/rms/data/docker.io
/opt/stacks/registry/data/ghcr    ->  /opt/stacks/rms/data/ghcr.io
/opt/stacks/registry/data/gcr     ->  /opt/stacks/rms/data/gcr.io
/opt/stacks/registry/data/k8s     ->  /opt/stacks/rms/data/registry.k8s.io
/opt/stacks/registry/data/quay    ->  /opt/stacks/rms/data/quay.io
# data/registry(15008 私有仓库)保持不动
# 迁完记得 chown -R 65532:65532(容器以 nonroot 运行)
```

## 配置校验(部署前跑过)

```console
$ RMS_HTPASSWD=$PWD/config/htpasswd RMS_STORAGE_ROOT=$PWD/data \
    ./bin/rms config validate --config deploy/103/rms.yaml
deploy/103/rms.yaml: ok
  primary listener: 0.0.0.0:5000
  storage root: /root/dev/registries-mirrors/data
  selection probe_ttl: 30s
  selection probe_timeout: 10s
  proxy docker.io        upstreams=https://docker.1ms.run,https://registry-1.docker.io
  proxy gcr.io           upstreams=https://gcr.io
  proxy ghcr.io          upstreams=https://ghcr.io
  proxy quay.io          upstreams=https://quay.io
  proxy registry.k8s.io  upstreams=https://registry.k8s.io
退出码=0
```

## 客户端迁移

旧配置(指向 15000–15006)必须改。新的最小写法:

```json
{
  "registry-mirrors": ["http://192.168.188.103:5000"],
  "insecure-registries": ["192.168.188.103:5000"]
}
```

> 只保留 `5000`:`registry-mirrors` 对非 Hub 上游本来就不生效,列多个端口只是噪音。

containerd / k3s 把 endpoint 指到主端点,并给明文 HTTP 打开 `insecure_skip_verify`:

```yaml
mirrors:
  docker.io:
    endpoint: ["http://192.168.188.103:5000"]
configs:
  "192.168.188.103:5000":
    tls:
      insecure_skip_verify: true
```

其它上游用镜像名里的 namespace 前缀(不需要 mirror 配置):

```bash
docker pull 192.168.188.103:5000/ghcr.io/owner/app:tag
docker pull 192.168.188.103:5000/registry.k8s.io/pause:3.9
```

要给某个上游做"透明镜像"(镜像名不变),在 containerd 侧用 `hosts.toml` + `override_path`:

```toml
# /etc/containerd/certs.d/ghcr.io/hosts.toml
server = "https://ghcr.io"

[host."http://192.168.188.103:5000/v2/ghcr.io"]
  capabilities = ["pull", "resolve"]
  override_path = true
  skip_verify = true
```

## 运维

```bash
ssh root@192.168.188.103
cd /opt/stacks/rms
docker compose logs -f --tail=100          # 日志
docker compose ps                          # 状态(healthy)
curl -s localhost:5000/healthz             # 存活
curl -s localhost:5000/readyz              # 就绪;503 时会列出降级的 namespace
curl -s localhost:9100/metrics | grep -E 'rms_namespace_up|rms_cache_hits_total'
du -sh data/*                              # 各 namespace 缓存占用
```

清理某个 namespace 的缓存:直接删 `<data>/<namespace>`(只影响该 namespace,重新拉取即可)。

## 回滚

```bash
ssh root@192.168.188.103
cd /opt/stacks/registry
cp docker-compose.yml.bak-<日期> docker-compose.yml   # 备份在首次接管时生成
docker compose up -d                                   # 7 个 registry:2 容器回来
cd /opt/stacks/rms && docker compose down              # 停掉 rms,让出 15000–15006
```

注意两点:

- 端口已经不在 rms 的 compose 文件里了,回滚旧栈是唯一能让 15000–15006 重新可用的方式;
  也可以直接把旧容器绑到别的端口,但客户端还得跟着改。
- 缓存已经被 mv 到 `/opt/stacks/rms/data/`,回滚后旧容器会从空缓存重新拉;要连缓存一起回滚,
  就先 `mv` 回去再起容器。

## 2026-10-13:上游并发选路（本次变更）

`deploy/103/rms.yaml` 已加 `decision_deadline: 3s` / `balance: weighted` / `balance_window: 1.5`；`upstreams` 的顺序语义从「优先级」变为 **digest 权威顺序**（并发竞争、同 hash 才均衡）。算法、指标与取舍见仓库 README §6.2 与 `../../reports/rms-upstream-race.md`（含 101 现场验收读数与 103 烟测读数）。

**2026-10-13 10:55Z 已上线**：现网镜像 `rms:dev` = `rms:race-20261013-184731`（旧镜像保留为 `rms:rollback-20261013-184640`），配置加了三个新键；切换后 31s healthy、`restarts=0`，101 匿名经 `registry-mirrors` 拉 `busybox:1.36.1` 成功且缓存计数增长。回滚命令与验收读数见 `../../reports/rms-upstream-race.md` §8.3/§8.4。

> ⚠️ 2026-10-13 发现：103 的现网配置曾把 `docker.elastic.co` 的上游写成裸主机名（无 scheme），**新旧二进制都会在启动时拒绝**——现役容器只是没重启过。已修复；改配置后务必先 `docker run --rm -v .../config:/etc/rms:ro --entrypoint /usr/local/bin/rms rms:dev config validate --config /etc/rms/rms.yaml` 验证再重启。

## 已知遗留

- `15008` 私有仓库仍由旧的 `registry:2` 提供(rms 的 local namespace + htpasswd 可以替代,但客户端要改成 `docker login` 且没有 CORS 头),本次未动。
- `config/htpasswd` 是仓库里的**演示凭据**(alice/bob/me/admin),上生产前用 `make htpasswd USER=... PASS=...` 换掉。
- 无限定请求的选仓/选上游结果现在有指标了：`rms_selection_decisions_total{source}`、
  `rms_upstream_wins_total{namespace,upstream}`、`rms_upstream_probe_duration_seconds`、
  `rms_upstream_digest_mismatch_total`；日志里那行 `selection` 仍带 `winner`/`upstream`/`timings`。
- `5002/5003`（内网镜像主机自己的端口）从 101 经 tun_sase 不可达；客户端 `registry-mirrors` 里
  留着它们只会每次多一次失败尝试。
- 若把**另一台开启 htpasswd 的 rms**（例如 `10.82.64.172:5000`）配成某个 namespace 的上游，
  必须同时配 `username`/`password`：它的探测匿名可过，但引擎的内容请求会带空 Basic 被 401，
  症状是「探测说存在、取内容 404」（实测见 `../../reports/raw/101-live-upstream-race.txt` §E）。

## 上游 namespace 一览（2026-10-13 更新）

| 客户端写的 namespace | 实际上游（顺序 = digest 权威顺序，默认并发竞争） | 说明 |
| :--- | :--- | :--- |
| `docker.io` | `docker.1ms.run`、`registry-1.docker.io`、`docker.m.daocloud.io` | daocloud 排在最后：它的失败形态是「`/v2/` 探活正常、取内容失败」（大 blob 302 到 R2 卡 60s；token 绑客户端 IP、走代理 401）。并发模式下内容探测会把它连续失败后放进冷却期，自动切走；与权威上游 digest 不一致时也不会被服务 |
| `ghcr.io` | `ghcr.io`、`ghcr.m.daocloud.io` | 官方在前 = 官方定义 tag 的 digest，镜像站只在与之一致时接手（实测 manifest 200） |
| `gcr.io` | `gcr.io`、`gcr.m.daocloud.io` | 同上 |
| `quay.io` | `quay.io`、`quay.m.daocloud.io` | 同上 |
| `registry.k8s.io` | `registry.k8s.io`、`k8s.m.daocloud.io` | 同上 |
| `docker.elastic.co` | `elastic.m.daocloud.io` | 新增 |
| `dhi.io` | `dhi.m.daocloud.io` | **需要凭据**：匿名取票后该源仍回 403（tag 列表也被禁用）。要用 DHI 镜像请在该 namespace 下加 `username`/`password` |
| `k8s.gcr.io` | `registry.k8s.io` | 废弃旧名 → 官方新域名（**不是** daocloud 镜像），老镜像名可继续用 |
| `mcr.microsoft.com` | `mcr.m.daocloud.io` | 新增 |
| `nvcr.io` | `nvcr.m.daocloud.io` | 新增 |

九个 daocloud 镜像在 103 上（走局域网代理）的 `/v2/` 实测都是 0.17–0.21s 回 401，即**探活可达**；原始读数见 `../../reports/raw/daocloud-mirrors-probe.txt`，各 namespace 的 manifest 取用读数见 `../../reports/raw/103-daocloud-namespaces-verify.txt`。

**注意**：无限定请求（路径里不写 namespace）要并发探测 **10 个 namespace × 各自的上游**；已缓存的镜像不受影响（走磁盘/决策缓存），窗口内（`probe_ttl`）也不重复探测。代价与三个旋钮：

- `namespaces.selection.decision_deadline: 3s` —— 第一个探测答出后再等 3s 就用已有结果决策，避免一个卡住的镜像站把冷请求拖满 `probe_timeout`（10s）。
- `namespaces.selection.balance: weighted` + `balance_window: 1.5` —— digest 一致、延迟在 1.5× 以内的上游按 1/延迟加权轮询；窗口外的镜像不参与均衡。
- 每个 namespace 的唯一上游（`docker.elastic.co` / `dhi.io` / `mcr.microsoft.com` / `nvcr.io`）没有竞争也不均衡，行为与以前一致。

**鉴权形态（本次确认）**：`auth.type: htpasswd` + `anonymous_pull_proxy: true` = **pull 全匿名、只有 push 到本地 namespace（`me`）需要登录**；`/v2/` 仍回 401（dockerd 的镜像源只要求内容匿名，见仓库 README §2.1 的实测）。反复检查这一点可以用 `rms config validate`，它会打印 `auth:` 与 `pull:` 两行生效策略。

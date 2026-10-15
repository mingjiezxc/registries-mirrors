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

## 2026-10-14:取消认证（`auth.type: none`）

**现网状态**：`/opt/stacks/rms/config/rms.yaml` 的 `auth.type` 由 `htpasswd` 改为 **`none`** —— 拉、推
都不再要密码（`/v2/` 从 401 变 200），`user_namespace` 同时改成 `false`（见下）。改动前的备份是
`config/rms.yaml.bak-<时间戳>`（本次：`config/rms.yaml.bak-20261014-151103`）；`config/htpasswd`
（还是 alice/bob/me/carol/admin 那份演示凭据）保留，只在回滚时用到。

**为什么**（2026-10-14 的决定，与 172 同一天、同因，同一份代码）：目标是「classic dockerd 显式主机名
pull 也不要密码」。Basic 方案下 `/v2/` 必须回 401 挑战（`docker login` 靠它验密码、后续 push 靠它带
凭据），而 **classic dockerd 拿到 Basic 挑战后就不再发匿名内容请求** —— 显式主机名匿名 pull 直接
`no basic auth credentials`，rms 侧一个请求都收不到；`anonymous_pull_proxy/local` 只作用于内容路径，
救不了（实测见下面「单端点化」那节第 4 条）。也试过「`/v2/` 对无凭据请求回 200」的开关：匿名 pull
好用了，但 classic dockerd 的 push 彻底失效（凭据正确也 `unauthorized`）——它不会在 401 后用已保存
的凭据重试。运维要的是免密显式拉取，于是整体关掉认证。三种写法的完整实测见 `../172/README.md` 的
「2026-10-14 变更」（172 是同版本、同一晚）。鉴权**代码与开关没有改动**，改的只是这份配置。

**代价（必须知道）**：`type: none` = 完全开放。任何能连到 `rms.majorc.eu.org` / `192.168.188.103:5000`
的人（局域网 + tailnet；公网 DNS 里 `rms.majorc.eu.org` 指向 `192.168.188.11`，外部不可路由）都可以：

- **push 任意 tag 到本地 namespace `me` / `shared`** —— 发布镜像 `me/rms` 就在 `me` 里，任何人可以覆盖
  `:latest`；
- **`DELETE` 它们的 manifest / blob**（`storage.delete_enabled: true`）—— 删掉 `me/rms` 的 tag 就断了
  回滚历史（`make push` 能再推一份，但旧 `:<version>` 找不回）；
- 匿名读 `/v2/_catalog`（列出**可写** namespace 的仓库名；缓存 namespace 不在 catalog 里，见仓库 README 的[端点与端口](../../README.md#41-端点与端口)）。

proxy namespace 的写仍然是 `403 DENIED`（那是 namespace 属性，与鉴权开关无关）。启动日志会打一条
`WARN authentication is disabled: anyone who can reach this endpoint may push to its local namespaces`，
`rms config validate` 会打 `auth: none: PUSHES ARE UNPROTECTED (auth.type is none)`。

**一条不能忘的连带影响**：`user_namespace` 必须同时改成 `false` —— 配置校验拒绝
`user_namespace: true` + `type: none` 的组合。`me` / `shared` 仍由 `shared_namespaces` 定义为可写本地
namespace（发布路径 `rms.majorc.eu.org/me/rms` 不变），但 htpasswd 里 `alice` / `bob` / `carol` 的同名
namespace **不再存在**（那三个目录本来也没有内容，`data/` 下只有 `me`）。

**怎么收回**：

```bash
ssh root@192.168.188.103
cd /opt/stacks/rms
cp config/rms.yaml.bak-<时间戳> config/rms.yaml   # 或手动把 type 改回 htpasswd、user_namespace 改回 true
docker compose up -d --force-recreate
curl -s -o /dev/null -w '%{http_code}\n' localhost:5000/v2/    # 401 = 挑战回来了
```

收回之后 103 上的 `docker compose pull` 会重新需要 `docker login`（`me` 是本地 namespace，
`anonymous_pull_local: false`），也就是 `.env` 里原先那句「拉取要登录」——`deploy/103/install.sh` 的
远端 `docker login` 步骤一直留着，正是为了这一步。

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
- `anonymous_root` 没有了,所以 `/v2/` 一律挑战(401)。**这条已实测(2026-10-13 晚,103 与 172 两地
  同版本复现)**,结论要分两种用法看:
  - **当镜像源(`registry-mirrors`)用:不影响。** `/v2/` 回 401 无害,只要内容路径匿名可读
    (`anonymous_pull_proxy: true`),dockerd 就照常走缓存 —— 101 上无限定名拉取后
    `rms_cache_hits_total` 增长即为证（见仓库 README 的[实测边界](../../README.md#66-实测边界)）。
  - **显式主机名拉取(`docker pull <host>:5000/docker.io/library/alpine`)做不到匿名。** dockerd 先 ping
    `GET /v2/`,拿到 401 + `WWW-Authenticate: Basic` 就判定「这个 registry 要凭据」,连 manifest 都不
    请求(实测:rms 侧来自 dockerd 的请求数 = 0,dockerd 日志只留一行
    `Head "http://<host>:5000/...": no basic auth credentials`)。根因是 **Basic 认证没有匿名换令牌的
    流程**(Docker Hub / Harbor 用 Bearer:401 里带 realm,客户端能匿名换 token)。所以「镜像名带
    namespace」这条退路**同样不解决**这个问题,正确的做法是二选一:客户端 `docker login` 一次
    (推私仓本来也要登),或者干脆走上面的 `registry-mirrors`(只对 Docker Hub 名字生效)。

  > **2026-10-14 更新**：第一条已经不再是现网形态 —— 103 改成了 `auth.type: none`（见上一节），
  > `/v2/` 回 200，显式主机名 pull 与 push 都不再需要密码。下面这段保留为 htpasswd 时代
  > 「为什么 Basic 方案做不到匿名显式拉取」的实测记录（172 当晚同版本复现，完整读数见
  > `../172/README.md` 的「2026-10-14 变更」）。

## 部署（2026-10-13 起：101 构建、103 只拉取）

镜像在 **101（仓库所在机）** 构建并 push 到本机 rms 的本地 namespace，103 只负责拉取运行。
**103 上不再有源码**：不再 rsync 仓库、不再 `docker compose build`，`/opt/stacks/rms` 只剩
`docker-compose.yml` / `.env` / `config/` / `data/` 四样。变更入口是仓库的 Makefile：

```bash
# 在 101 执行。`make login` 只有回滚到 htpasswd 之后才需要（现在 push 匿名可做）：
# 凭据落在 $(DOCKER_CONFIG)，即仓库内 .docker/（已 gitignore）
make login RMS_REGISTRY_PASSWORD='<admin 密码>'      # 等价于 docker login rms.majorc.eu.org -u admin

make push        # 构建并 push rms.majorc.eu.org/me/rms:<version> 与 :latest
make deploy-103  # = push + 放 compose/.env/rms.yaml（带备份）→ 103 拉取 → 重启 → 清旧镜像
make update-103  # 只做 103 侧：拉取 → 重启 → 清旧镜像（不动 103 上的文件、不重新构建）
```

几个必须知道的点：

- **tag**：干净 checkout 用 `git describe`；脏树追加 UTC 时间戳，所以每次 `make push` 都是一个
  唯一、可回滚的 `:<version>`（registry 里就是回滚历史，103 上不再留 rollback tag）。
- **push 可能重试一次**：`rms.majorc.eu.org` 经路由器上的 nginx 容器（192.168.188.1），请求体被
  nginx 缓冲，唯一的大层（约 9 MiB 的二进制）偶尔会超过 registry 客户端 60s 的响应头超时。
  blob 通常已在服务端落盘，所以 `make push` 自带重试，第二次会看到 `Layer already exists` 后立刻完成。
- **103 拉取不再需要登录**（2026-10-14 起 `auth.type: none`，见本文第一节）：匿名就能拉
  `rms.majorc.eu.org/me/rms`。`make deploy-103 RMS_REGISTRY_PASSWORD=...` 仍会在远端补一步
  `docker login`，那是给「回滚到 htpasswd」留的路，在 `type: none` 下它也不会失败（`/v2/` 对任何
  凭据都回 200，CLI 把它当登录成功）。
- **compose 里故意不写 `pull_policy: always`**：`rms.majorc.eu.org` 就是 103 上这个容器自己，
  容器停着时强制拉取会让 `docker compose up -d` 直接起不来。更新脚本用
  `docker compose pull --policy always`（此时旧容器还在服务，拉得到）再 `up -d`；
  即使拉取失败也还能用本地镜像恢复。

手工等价操作（不经过 Makefile）：

```bash
ssh root@192.168.188.103
cd /opt/stacks/rms
docker compose pull --policy always
docker compose up -d
docker image prune -f          # 更新后上一个 :latest 变成悬空镜像，这一步清掉
```

首次接管旧 `registry:2` 栈时的顺序（历史操作，见 `reports/103-rms-deploy.md`）不变：
1) 摘掉 7 个旧镜像容器（保留 15008）→ 2) 把旧缓存 mv 成 rms 的 namespace 根 →
3) 在 103:/opt/stacks/rms 执行 `docker compose up -d`。

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

> **2026-10-14 起这里不需要登录**：103 是 `auth.type: none`（见本文第一节），显式主机名 pull 与
> push 都不要再 `docker login`，`/v2/` 回 200 —— 下面的迁移写法本身不变。

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
docker inspect rms --format '{{.Config.Image}} {{.State.Health.Status}} {{.RestartCount}}'   # 跑的是哪个镜像
docker exec rms /usr/local/bin/rms version # 镜像里编译进去的 version/commit
docker compose pull --policy always        # 只拉新镜像,不重启
docker compose up -d                       # 用拉下来的镜像重建容器
docker image prune -f                      # 清掉更新后悬空的上一个 :latest
curl -s localhost:5000/healthz             # 存活
curl -s localhost:5000/readyz              # 就绪;503 时会列出降级的 namespace
curl -s localhost:9100/metrics | grep -E 'rms_namespace_up|rms_cache_hits_total'
du -sh data/*                              # 各 namespace 缓存占用
```

清理某个 namespace 的缓存:直接删 `<data>/<namespace>`(只影响该 namespace,重新拉取即可)。

## 回滚

镜像的历史版本都在 registry 里（每次 `make push` 一个 `:<version>` tag），103 上不再保留
`rms:rollback-*` 这类本地 tag —— 回滚就是换回一个旧 tag：

```bash
# 看有哪些版本可用（2026-10-14 起匿名可读；回滚到 htpasswd 之后要加 -u admin:<密码>）
curl -s https://rms.majorc.eu.org/v2/me/rms/tags/list

ssh root@192.168.188.103
cd /opt/stacks/rms
sed -i 's|^RMS_IMAGE=.*|RMS_IMAGE=rms.majorc.eu.org/me/rms:<旧 version>|' .env
docker compose pull --policy always && docker compose up -d
# 回完记得把 .env 改回 :latest（或留着钉住该版本，下次 make deploy-103 会覆盖）
```

回滚到旧的 `registry:2` 栈（15000–15006），前提是 `/opt/stacks/registry` 及其
`docker-compose.yml.bak-<日期>` 还在：

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

`deploy/103/rms.yaml` 已加 `decision_deadline: 3s` / `balance: weighted` / `balance_window: 1.5`；`upstreams` 的顺序语义从「优先级」变为 **digest 权威顺序**（并发竞争、同 hash 才均衡）。算法、指标与取舍见仓库 README 的[上游并发、digest 权威与负载均衡](../../README.md#44-上游并发digest-权威与负载均衡)与 `../../reports/rms-upstream-race.md`（含 101 现场验收读数与 103 烟测读数）。

**2026-10-13 10:55Z 已上线**：现网镜像 `rms:dev` = `rms:race-20261013-184731`（旧镜像保留为 `rms:rollback-20261013-184640`），配置加了三个新键；切换后 31s healthy、`restarts=0`，101 匿名经 `registry-mirrors` 拉 `busybox:1.36.1` 成功且缓存计数增长。回滚命令与验收读数见 `../../reports/rms-upstream-race.md` §8.3/§8.4。

> ⚠️ 2026-10-13 发现：103 的现网配置曾把 `docker.elastic.co` 的上游写成裸主机名（无 scheme），**新旧二进制都会在启动时拒绝**——现役容器只是没重启过。已修复；改配置后务必先验证再重启：
> `docker run --rm -v /opt/stacks/rms/config:/etc/rms:ro --entrypoint /usr/local/bin/rms rms.majorc.eu.org/me/rms:latest config validate --config /etc/rms/rms.yaml`

## 2026-10-13:103 改为 pull-only（本次变更）

`deploy/103/install.sh` 不再把仓库 rsync 到 103、也不在那台机器上构建：

- **101 构建 → push `rms.majorc.eu.org/me/rms`（`:<version>` + `:latest`）→ 103 `docker compose pull` → `up -d` → 清旧镜像。**
  入口：`make push` / `make deploy-103` / `make update-103`。
- `deploy/103/docker-compose.yml` 去掉 `build:` 段，改成 `image: ${RMS_IMAGE:-rms.majorc.eu.org/me/rms:latest}`。
- `deploy/103/.env` 的 `RMS_IMAGE` 由 `rms:dev` 改为 `rms.majorc.eu.org/me/rms:latest`。
- `/opt/stacks/rms` 里删掉了 `bin/ clear/ cmd/ internal/ vendor/ test/ scripts/ deploy/ reports/
  Dockerfile* go.mod go.sum Makefile README.md rms.bin rms.tar.gz` 等源码与构建产物，并清掉了本地
  构建时代的 `rms:dev` / `rms:race-*` / `rms:rollback-*` 镜像、悬空镜像与 build cache
  （这段清理在每次 install/update 里都会重跑，幂等）。
- 顺带回同步了两处**只存在于 103 现网**的配置，仓库与现网现在一致：`.env` 的 `RMS_NO_PROXY`
  多一个 `.m.daocloud.io`；`rms.yaml` 里 `docker.elastic.co` / `dhi.io` / `mcr.microsoft.com` /
  `nvcr.io` 是「官方在前、daocloud 兜底」（仓库里那版是镜像在前，跑起来这几个 namespace 会恒降级）。
- 现网镜像：`rms.majorc.eu.org/me/rms:9417526-dirty-20261013T142913Z`（=`latest`），容器 healthy、
  `restarts=0`；`/v2/` 匿名 401、admin 200，代理 namespace 匿名可拉。

## 已知遗留

- `15008` 私有仓库仍由旧的 `registry:2` 提供(rms 的 local namespace + htpasswd 可以替代,但客户端要改成 `docker login` 且没有 CORS 头),本次未动。
- `config/htpasswd`（103 现网就是 `/opt/stacks/rms/config/htpasswd` 这一份）是仓库里的**演示凭据**
  (alice/bob/me/carol/admin)，**2026-10-14 起不再参与判定**（`auth.type: none`，只用于回滚）：
  那时候 `admin` 是管理员（`auth.admin_users: [admin]`，可写任意本地 namespace，也是
  `make push` / 103 `docker pull` 登录用的账号）；`me` / `shared` 是共享 namespace（见
  `auth.shared_namespaces`，现在由它单独定义这两个可写本地 namespace）。
  要换密码（回滚前）：在 101 跑 `make htpasswd USER=admin PASS=...`（写仓库的 `config/htpasswd`），
  再把这份文件传到 103 的 `config/` 覆盖，最后 `docker compose restart rms`。`install.sh` **不会**
  覆盖已存在的 htpasswd，免得把现网凭据冲成仓库里的演示口令。
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

**~~鉴权形态（本次确认）~~（2026-10-14 之前的形态，已失效）**：`auth.type: htpasswd` +
`anonymous_pull_proxy: true` 曾是 **pull 全匿名、只有 push 到本地 namespace（`me`）需要登录**，
`/v2/` 回 401（dockerd 的镜像源只要求内容匿名，见仓库 README 的[实测边界](../../README.md#66-实测边界)）。**2026-10-14 起 103 是
`auth.type: none`：拉、推都匿名，`/v2/` 回 200** —— 见本文第一节「2026-10-14:取消认证」。
`rms config validate` 仍会打印 `auth:` 与 `pull:` 两行生效策略，现在分别是
`auth: none: PUSHES ARE UNPROTECTED (auth.type is none)` 与
`pull: anonymous for every namespace (authentication is off)`。

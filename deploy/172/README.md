# 10.82.64.172:/app/rms —— 公司内网那台（WCN-GDUVA-MYSCAS）

同一份 rms，换一种投递方式：**101 构建 → `docker save` → scp → 远端 `docker load` → 重启**。
172 在公司内网，从 101 只能经 SASE 隧道到达，既够不着 `rms.majorc.eu.org`，也不该在那台机器上
留源码、跑构建 —— 所以这里没有 push/pull，也没有 `build:` 段。

## 现状

| 项 | 值 |
|---|---|
| 路径 | `/app/rms`（root 所有；`ops` 不在 docker 组，全程 `sudo -n` 免密提权） |
| 镜像 | `rms.majorc.eu.org/me/rms:<version>`（和 103 同一份构建，只是 load 而非 pull） |
| 内容端口 | **5000**（宿主）→ 容器 5000（2026-10-14 由 5003 改来） |
| 指标端口 | **9100**（宿主）→ 容器 9100 |
| 私仓 | **`ops`** —— `docker push 10.82.64.172:5000/ops/<镜像>`（2026-10-14 起不需要登录） |
| 鉴权 | **`auth.type: none`**（2026-10-14 起）—— 拉和推都不要密码，见下面「鉴权形态」；代价是任何人都能推/删 `ops` |
| 代理 namespace | 10 个（docker.io / ghcr.io / gcr.io / quay.io / registry.k8s.io / docker.elastic.co / dhi.io / k8s.gcr.io / mcr.microsoft.com / nvcr.io），**匿名可拉** |
| 出网 | 公司代理 `http://10.82.44.12:8080`（`deploy/172/.env` 的 `RMS_UPSTREAM_PROXY`） |

**端口沿革**：原先 5000 / 5001 / 5002 / 5003 分别是 `registry0` / `registry`（带 htpasswd 的私仓）/
`registry1` / `registry2`。2026-10-13 晚那套 `registry:2` 栈（含 `registry-ui`）整体退役，
rms 先接管了 **5003**，2026-10-14 又改成 **5000**（改的理由与读数见文末「2026-10-14 变更」）——
两个端口本来都在各客户端 `insecure-registries` 里，所以换端口不需要动客户端 daemon.json。
5001 / 5002 / 5003 / 5004 现在都是空的。

## 部署

```bash
# 101（仓库所在机）
make deploy-172                      # 构建 → docker save → scp → 远端 load → 重启 + 清理
make deploy-172 RMS_HTPASSWD_PASSWORD=mystore   # 顺带把私仓 htpasswd 重建成只含 admin

# 也可以直接调脚本（`make deploy-172` 只是「构建 + 调它」）
IMAGE=rms.majorc.eu.org/me/rms:<tag> deploy/172/install.sh
IMAGE=... deploy/172/install.sh --no-place   # 只 save/scp/load + 重启，不动远端文件
IMAGE=... deploy/172/install.sh --force      # 什么都没变也强制重建容器
IMAGE=... deploy/172/install.sh --prune-all  # 额外跑一次 `docker image prune -f`（宿主机全局）
```

脚本做的事（顺序）：

1. 本机 `docker save` 到 `dist/<tag>.tar`（`dist/` 已 gitignore，留着手工回滚用）；
2. `scp` 到 `ops@10.82.64.172:/tmp/rms-deploy-*`（`ops` 可写），文件里有一份把 `RMS_IMAGE`
   改写成实际 tag 的 `.env`；
3. 远端 `sudo -n bash -s`（整段以 root 跑）：`docker load` → 逐个放置 `compose.yaml` / `.env` /
   `config/rms.yaml`（内容相同就跳过，不同就 `.bak-<时间戳>` 备份）→ 权限（`data` 归 65532、
   `htpasswd` `root:65532 0640`）→ `docker compose up -d`；
4. **放了新文件就 `--force-recreate`**：compose 不会因为挂载文件内容变化而重建容器，不改这一步的话
   新 `rms.yaml` 在磁盘上、进程里还是旧的；
5. 等 healthy、`curl 127.0.0.1:5000/v2/`（`auth.type: none` 时 200、htpasswd 时 401 都算起来了）、
   删掉被替换的旧镜像（本次上线时删掉了本地构建时代的 `rms:dev`），清掉 `/tmp` 暂存目录；
6. 回到 101 再验一次：匿名 `/v2/`（走 SASE 链路，等于把网络也测了）；给了
   `RMS_HTPASSWD_PASSWORD` 时再验 `admin` → 200。

## 鉴权形态（`deploy/172/rms.yaml`，2026-10-14 起：`auth.type: none`）

```yaml
auth:
  type: none                    # ← 2026-10-14 起：拉、推都不要密码（下面有代价）
  user_namespace: false         # 以下四项在 type: none 时不参与判定，留着仅为切回 htpasswd
  shared_namespaces: []         # （切回时改回 htpasswd 即可，htpasswd 文件还在 config/htpasswd）
  anonymous_pull_proxy: true
  anonymous_pull_local: true
namespaces:
  local: [ops]                  # 唯一可写私仓
```

**为什么关认证**（2026-10-14 的决定，实测过程见文末「2026-10-14 变更」）：目标是「classic dockerd
显式主机名 pull 也不要密码」。Basic 方案下 `/v2/` 必须回 401 挑战（否则 `docker login` 验不了密码、
push 不带凭据），而 **classic dockerd 拿到 Basic 挑战后就不再发送匿名内容请求**
（`no basic auth credentials`，rms 侧 0 请求）——`anonymous_pull_proxy/local` 只作用于内容路径，
救不了。试过「`/v2/` 对无凭据请求回 200」的开关：匿名 pull 好用了，但 classic dockerd 的 push
彻底失效（凭据正确也 `unauthorized`）。运维不接受 Bearer token 端点，所以选了 `type: none`。

**代价（必须知道）**：`type: none` = 完全开放。任何能连到 `10.82.64.172:5000` 的人都可以：
push 任意 tag 到 `ops`、`DELETE` `ops` 的 manifest/blob（`storage.delete_enabled: true`）、
匿名读 `/v2/_catalog`（列出**可写** namespace 的仓库名，缓存 namespace 不在其中）。proxy namespace 的写仍然是 `403 DENIED`（那是 namespace 属性，与鉴权无关）。
启动日志会打一条 `WARN authentication is disabled: anyone who can reach this endpoint may push to
its local namespaces`，`rms config validate` 会打印 `auth: none: PUSHES ARE UNPROTECTED`。

**怎么收回**：把 `config/rms.yaml` 的 `type` 改回 `htpasswd`（`htpasswd` 文件仍在
`/app/rms/config/htpasswd`，内容为 `admin`），然后
`cd /app/rms && sudo docker compose up -d --force-recreate`；2026-10-14 改动前的备份是
`/app/rms/config/rms.yaml.bak-<时间戳>`。

**2026-10-13 晚的历史**：那时是 `htpasswd` + `anonymous_pull_proxy: true`（从 `false` 改的）+
`anonymous_pull_local: false`。`rms config validate` 当时打印 `pull: anonymous for proxy namespaces;
local namespaces require login`：缓存 namespace 的内容请求（GET/HEAD manifest、blob）匿名可读，
私仓 `ops` 连读都要登录。但 `/v2/` 在任何情况下都回 401（`Basic realm="rms"`，见 `internal/auth/auth.go`
的 `KindBase` 注释），而那个 401 恰好决定了 classic 客户端能不能匿名拉 —— 见下面一节。

## 客户端怎么用

rms 是明文 HTTP，客户端 `insecure-registries` 必须含 `10.82.64.172:5000`（172 自己的 daemon.json
本来就有；其它机器要自己加，`insecure-registries` 支持 `systemctl reload docker` 热加载）。

**2026-10-14 起（`auth.type: none`）：所有客户端、所有用法都不再需要登录**，包括 classic dockerd
（27.x/28.x，overlay2）的显式主机名 pull —— 读数见文末「2026-10-14 变更」。

| 用法 | 需要登录吗 | 实测 |
|---|---|---|
| **A. 当镜像源**：`daemon.json` 的 `registry-mirrors: ["http://10.82.64.172:5000"]`，然后 `docker pull alpine`（无限定名） | **不需要** | ✅ 可用。103 上同样形态已长期验证（`rms_cache_hits_total` 随拉取增长，不是回落官方） |
| **B. 显式主机名拉取**：`docker pull 10.82.64.172:5000/prom/node-exporter`（或 `<ns>/<repo>` 形式） | **不需要** | ✅ 2026-10-14 实测：classic dockerd 28.2.2（172）与 containerd 系（101）都成功 |
| **C. push**：`docker push 10.82.64.172:5000/ops/<镜像>` | **不需要** | ✅ 2026-10-14 实测：classic dockerd 零凭据直接推成功（这是 `type: none` 的代价，见「鉴权形态」） |

> ⚠️ 172 上 `daemon.json` 的 `registry-mirrors` 还挂着 `[5000, 5001, 5002, 5003]`，
> 后三个端口现在没人监听。实测这条列表会让无限定名拉取**回落官方**并超时
> （`Get "https://registry-1.docker.io/v2/": context deadline exceeded`，rms 侧 0 请求）。
> 建议改成只留 `http://10.82.64.172:5000` 再 `sudo systemctl reload docker`。

### 存档：htpasswd 时代为什么做不到匿名显式拉取（2026-10-13 实测，端口当时是 5003）

| 用法 | 需要登录吗 | 实测 |
|---|---|---|
| A. 当镜像源（无限定名） | **不需要** | ✅ 可用（内容路径匿名可读即可） |
| B. 显式主机名 | **需要**：先 `docker login 10.82.64.172:5003 -u admin` | ✅ 登录后可用（rms 侧收到 5 个 daemon 请求、manifest 200） |
| C. 匿名显式主机名拉取 | —— | ❌ **做不到** |

**为什么 C 不行**：docker 的显式主机名拉取会先 ping `GET /v2/`；rms 回 401 +
`WWW-Authenticate: Basic`，dockerd 把它当成「这个 registry 需要凭据」——它**不会**再去看内容路径是不是
匿名可读，直接报错返回，连 manifest 都不请求：

```
dockerd 日志：Error getting v2 registry: Get "https://10.82.64.172:5003/v2/": http: server gave HTTP response to HTTPS client
             Attempting next endpoint for pull after error: Head "http://10.82.64.172:5003/v2/prom/node-exporter/manifests/latest": no basic auth credentials
rms 日志：    这段时间里来自 dockerd 的请求数 = 0
```

同一个 rms（同一份代码）在 103 上复现一模一样：`docker pull 192.168.188.103:5000/docker.io/library/alpine:3.20`
→ `no basic auth credentials`，rms 同样一个请求都没收到。根因是 **Basic 认证没有匿名换取令牌的流程**
（Docker Hub / Harbor 用的是 Bearer：401 里带 realm，客户端能匿名换 token，所以它们能匿名拉）——
Basic 挑战 = 必须提供凭据。这也解释了为什么「内容匿名」只对**镜像源那条路**（A）有效：
dockerd 对镜像源是「拉不到就回落官方」，不强制认证。当时因此的结论是：想匿名吃缓存就配
`registry-mirrors`；要精确拉某个上游的镜像、或者要 push，就必须 `docker login`。
仓库 README 的[实测边界](../../README.md#66-实测边界)与附录 A 的 C13、`deploy/103/README.md` 的客户端迁移小节按这个实测结论更正过。

## 与 103 的配置差异（`deploy/172/rms.yaml`）

1. **只有私仓 `ops`**：`namespaces.local: [ops]` + `auth.user_namespace: false` +
   `shared_namespaces: []`。103 那套是 `me` / `shared`。
2. **上游顺序**（2026-10-13 从 172 走公司代理实测后调整）：
   - `docker.elastic.co` / `dhi.io` / `mcr.microsoft.com` / `nvcr.io` 都改成**官方在前、daocloud 兜底**。
     实测 `elastic.m.daocloud.io` 与 `mcr.m.daocloud.io` 经公司代理回 **502**，官方分别回 401 / 200；
     只挂 daocloud 时 `/readyz` 会把 `nvcr.io` 列成 degraded。
   - `docker.io` 顺序对齐 103：`1ms.run` → 官方 Hub → daocloud（daocloud 最后，理由见
     `../103/README.md`：它是「探活正常、取内容失败」，而且会抢 digest 权威）。
3. `namespaces.selection` 补上 `decision_deadline: 3s` + `balance: weighted`（10 个 namespace 的冷请求
   不该等满 `probe_timeout`）。
4. `.env` 的 `RMS_NO_PROXY` 覆盖掉了 `rms.yaml` 里带 `was.ink` 的默认值（现网 10 个上游都不是
   `was.ink`，眼下没有实际影响；要还原就把那三个域名补回 `.env`）。
5. **`auth.type: none`**（2026-10-14 起）：103 仍是 `htpasswd`。172 关认证的理由与代价见上面
   「鉴权形态」与文末「2026-10-14 变更」；`namespaces.local` / `shared_namespaces` / `admin_users`
   这些字段保留着，切回 `htpasswd` 时不用重写。

## 运维

```bash
ssh ops@10.82.64.172
sudo docker compose -f /app/rms/compose.yaml ps
sudo docker inspect rms --format '{{.Config.Image}} {{.State.Health.Status}} {{.RestartCount}}'
sudo docker exec rms /usr/local/bin/rms version
sudo docker logs -f --tail=100 rms
curl -s http://127.0.0.1:5000/healthz; curl -s http://127.0.0.1:5000/readyz   # readyz 会列出降级的 namespace
curl -s http://127.0.0.1:5000/v2/_catalog     # auth.type: none 下匿名可读；htpasswd 下要 -u admin:<pw>
sudo du -sh /app/rms/data/*            # 各 namespace 占用；私仓在 data/ops
```

改配置：改仓库里的 `deploy/172/rms.yaml` → `deploy/172/install.sh`（会备份 + 强制重建）。
只想重启：`sudo docker compose -f /app/rms/compose.yaml restart rms`。

## 回滚

镜像的历史版本都在 101 的 `dist/`（每次 `deploy-172` 留下一个 tar），远端只留当前这一份：

```bash
# 101
scp dist/<旧tag>.tar ops@10.82.64.172:/tmp/
ssh ops@10.82.64.172 'sudo docker load -i /tmp/<旧tag>.tar'
# 172：把 .env 的 RMS_IMAGE 换成旧 tag，再重建
ssh ops@10.82.64.172 'sudo sed -i "s|^RMS_IMAGE=.*|RMS_IMAGE=<旧tag>|" /app/rms/.env && cd /app/rms && sudo docker compose up -d --force-recreate'
```

## 2026-10-14 变更（5003 → 5000、`auth.type: none`）

**起因**：`anonymous_pull_proxy` 与 `anonymous_pull_local` 都设成 `true` 之后，classic dockerd
（172 本机的 28.2.2、103 的 27.3.1）显式主机名 `docker pull 10.82.64.172:5003/<ns>/<repo>` 仍然报
`no basic auth credentials`，rms 侧 0 个内容请求。根因是 `/v2/` 的 401 + `WWW-Authenticate: Basic`
（见上面「客户端怎么用」的存档一节）：那两个开关只管内容路径，而 classic 客户端走不到内容路径。

**试过的三个方案**（都在 172 的隔离实例上做：独立存储根、端口 5001/5002/5003、
同一份镜像 `9417526-dirty-20261013T142913Z`；测完容器、镜像、临时目录全部删除）：

| 方案 | classic dockerd 匿名 pull | classic dockerd push | containerd 系（docker 29 + containerd 镜像存储） |
|---|---|---|---|
| 原样：`htpasswd` + 两个 `anonymous_pull_*` | ❌ `no basic auth credentials` | ✅ 登录后可用 | pull ✅ 匿名 / push ✅ 登录后 |
| `/v2/` 对无凭据请求回 200（原型开关，改 3 处 Go 代码） | ✅ | ❌ `unauthorized`，**凭据正确也一样** | pull ✅ / push ✅（登录后）；无凭据 push ❌ 401 |
| **`auth.type: none`（本次采用）** | ✅ | ✅ 零凭据直接推 | ✅ |

- 原型开关的机制：classic dockerd 的凭据是**被挑战驱动**的——`/v2/` 不回 401，它就永远不带
  `Authorization`；而写请求拿到 401 后**不重试**（实测两层，有正确凭据也 `unauthorized`）。
  所以 Basic 方案下 `/v2/` 只能二选一：挑战 ⇒ 匿名 pull 做不到；不挑战 ⇒ push 废掉。
  Bearer 端点（Harbor / Docker Hub 那套按 scope 签发 token）能同时满足，本次运维不要，故未实现；
  原型的代码改动已还原，没有留在仓库里。
- containerd 系客户端（docker 29 默认 containerd 镜像存储、nerdctl、ctr、k3s）本来就能匿名 pull，
  在原配置下已经是「pull 免密 + push 要登录」——本次改动照顾的是 classic 客户端。

**本次实际改动**：

- `deploy/172/.env`：`RMS_PORT_PRIMARY=5003` → `5000`（宿主 5000 → 容器 5000），
  `deploy/172/compose.yaml` / `rms.yaml` / `install.sh` / `Makefile` 里的端口说明同步；
  `install.sh` 的本地校验端口现在默认读 `.env`，不再假设 5003。
- `deploy/172/rms.yaml`：`auth.type: htpasswd` → `none`（`anonymous_pull_local` 保留 `true`，
  作为切换前的遗留值；`type: none` 时它不参与判定）。
- 172 上只替换了 `/app/rms/config/rms.yaml`（旧文件已备份为 `config/rms.yaml.bak-<时间戳>`），
  然后 `cd /app/rms && sudo docker compose up -d --force-recreate`；`config/htpasswd` 原样保留，
  要收回「push 要登录」只改 `type` 一行再重建容器。
- 已知残留（本次范围外）：172 root 的 `~/.docker/config.json` 里 `10.82.64.172:5000` 仍是过期密码
  （`admin:zxc123..`；`admin:mystore` 才是对的），用它拉取会弹用户名密码；
  `daemon.json` 的 `registry-mirrors` 仍挂着 5001/5002/5003 三个没人监听的端口。

**上线读数**（2026-10-14，101 经 SASE 与 172 本机两处验证）：

- `curl http://10.82.64.172:5000/v2/`（零凭据）→ **200**（原来是 401 + `Basic realm="rms"`）；
  带错凭据也是 200（认证关闭后不再校验，`docker login` 因此失去意义，也不再弹密码）。
- `docker pull 10.82.64.172:5000/docker.io/library/alpine`（classic dockerd 28.2.2，零凭据）✅；
  `docker push 10.82.64.172:5000/ops/<镜像>`（零凭据）✅ —— 私仓 `ops` 现在谁都能推/删。
- `/healthz` 200、`/readyz` ready、容器 healthy；`rms version` 仍是
  `9417526-dirty-20261013T142913Z`（本次只改配置，没换镜像）。
- 启动日志：`WARN authentication is disabled: anyone who can reach this endpoint may push to its local namespaces`；
  `rms config validate --config deploy/172/rms.yaml`：`auth: none: PUSHES ARE UNPROTECTED (auth.type is none)`、
  `pull: anonymous for every namespace (authentication is off)`。

## 2026-10-13 上线读数（历史）

- 容器：`rms.majorc.eu.org/me/rms:9417526-dirty-20261013T142913Z`，`healthy`，`restarts=0`，
  `0.0.0.0:5003->5000`、`0.0.0.0:9100->9100`；`rms version` 与 103 完全一致。
- `GET /healthz` → 200 `ok`；`GET /readyz` → `ready`（补上官方上游前是 `degraded namespaces: nvcr.io`）。
- 匿名（无凭据）经 rms 拉 manifest：`prom/node-exporter` 200、`library/alpine` 200、
  `docker.io/library/alpine` 200；私仓 `ops/x` 匿名 401、`admin` 200。
- 私仓 `ops` 端到端（curl，`admin`）：blob 201 / manifest 201 / GET blob 200 且逐字节一致 /
  `tags/list` 与 `_catalog` 正常 / 匿名 401 / `DELETE` manifest 202。验收产生的测试仓库已清空，
  `_catalog` 现在是 `{"repositories":[]}`。
- 客户端实测：`docker login 10.82.64.172:5003 -u admin`（`Login Succeeded`）之后
  `docker pull 10.82.64.172:5003/prom/node-exporter` 成功（rms 侧 manifest HEAD 200）；
  匿名显式主机名拉取失败（`no basic auth credentials`），103 上同样复现，见上面「客户端怎么用」。
- 曾存在的 `rms:dev`（本地构建时代）已删除；`/app/rms` 只有 `compose.yaml` / `.env` / `config/` / `data/`
  （外加几次部署留下的 `.bak-*`）。
- **遗留**：172 的 `daemon.json` `registry-mirrors` 还指着三个已经没人监听的端口（见上）；
  `ops` 仍是空仓库，还没推过真实镜像。

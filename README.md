# rms — 镜像代理 / 加速 / 缓存服务

单个 Go 进程，一个端点同时做三件事：

1. **拉取加速与缓存**：把公共仓库按 namespace 代理并缓存到本地磁盘（`/data/<namespace>`）。
2. **私有仓库推送**：`docker login` 认证后往自己的 namespace 推镜像。
3. **无限定请求自动选仓**：不带 namespace 的请求并发探测所有 proxy namespace，取**真有该镜像且响应最快**的那个；没有默认仓库。

镜像引擎复用 [`distribution/distribution/v3`](https://github.com/distribution/distribution)，rms 负责它不提供的部分：namespace 路由、无限定请求的并发选仓、推送鉴权、上游选举与故障转移、HTTP 代理、可观测性。

---

## 1. 快速开始

```bash
cp .env.example .env          # 按部署宿主机调整代理配置
make htpasswd USER=alice PASS=alicepw
make prepare                  # 建目录并把权限调成容器用户可用
make up                       # docker compose up -d --build
make logs
```

`make up` 会：构建镜像、把 `./data` 挂到 `/data`、把整个 `./config` 目录挂到 `/etc/rms`，并启动 `:5000`（主端点，唯一的内容入口）与 `:9100`（指标）。**没有按上游绑定的端口**——理由与代价见 §2.1。

> `config/` 里的 `htpasswd` 是**演示凭据**（alice/bob/me/admin），上线前请用 `make htpasswd USER=... PASS=...` 替换或删掉。
>
> 配置目录是**整目录挂载**而不是逐文件挂载：`rms htpasswd add` 用「写临时文件再 rename」的方式原子替换 htpasswd，而文件级 bind mount 会钉住原来的 inode，容器会一直读到旧文件、`kill -s HUP` 看起来毫无作用。

冒烟测试：

```bash
curl -i http://localhost:5000/v2/                     # 401 + WWW-Authenticate: Basic realm="rms"
docker pull localhost:5000/docker.io/library/alpine   # 显式 namespace，直连该 namespace
docker pull localhost:5000/alpine                     # 无限定：并发探测后挑到 docker.io（只有它有 library/alpine）
docker login localhost:5000 -u alice                  # 认证
docker tag alpine localhost:5000/alice/app:v1
docker push localhost:5000/alice/app:v1               # 推私有 namespace
```

---

## 2. 客户端怎么配（重要，含实测结论）

rms 只有**一个**内容端点（主端点 `:5000`），namespace 写在镜像名里：

| 写法 | 行为 |
|---|---|
| `docker pull <host>:5000/docker.io/library/alpine` | 显式 namespace：**直连** `docker.io`，不探测其它 namespace |
| `docker pull <host>:5000/alpine` | 无限定：并发探测所有 proxy namespace，挑真有该镜像且最快的一个 |

### 2.1 为什么不再有按上游绑定的端口

早期版本给每个上游开一个端口（`docker.io` 一个、`ghcr.io` 一个、`gcr.io` 一个……），路径里不写 namespace，好让只能写裸 `host:port` 的客户端（containerd `registries.yaml`、docker `registry-mirrors`）也能用。**这套端口已经全部删除**，现在只有 `:5000` 与 `:9100`。原因：

- 端口数量随上游增长，每加一个上游就要改 rms 配置 + 客户端配置 + 容器端口映射；
- 端口绑定 namespace 与「无限定请求自动选仓」是**互相矛盾**的两种语义：前者固定一个 namespace，后者要求留白让 rms 去挑；
- 它还拖着一个额外的开关 `anonymous_root`（见下），只为迁就 dockerd 的请求不带凭据。

被删的 `anonymous_root` 当年是为了绕开这个问题：早期版本里 docker 的 `registry-mirrors` 端点**不带凭据**，而那时的 rms 连**内容路径**也回 401 挑战，dockerd 于是放弃这个镜像源、转去官方仓库——**拉取看起来正常，缓存却完全没走 rms**。当年的实测日志原文：

```
level=info msg="Attempting next endpoint for pull after error:
  Head \"http://<rms>/v2/library/alpine/manifests/3.16\": no basic auth credentials"
```

**这条未决项现已实测收口**（2026-10-13，101 的 dockerd 版本与线上一致）：

- **`GET /v2/` 回 401 无害**。镜像源指向 `:5000`（`/v2/` 仍是 401 `Basic realm="rms"`）时，`docker pull hello-world:latest` 成功，且 rms 侧 `rms_cache_hits_total{namespace="docker.io"}` 从 33 增到 40 —— 缓存确实被走到，不是回落官方。客户端 `~/.docker/config.json` 里**没有**该镜像源的凭据（只有 `10.82.64.172:5001`、`l-rms.majorc.eu.org`），即匿名。
- 真正决定死活的是**内容路径**：`anonymous_pull_proxy: true` 时代理 namespace 的内容请求匿名可读（§4.2），dockerd 的镜像源请求因此能拿到 manifest 与 blob；内容路径一旦挑战（`anonymous_pull_proxy: false`，或旧版那套「内容也挑战」的配置），dockerd 就报上面那条 `no basic auth credentials` 并转投官方。
- **`docker login <镜像源>` 救不了 `registry-mirrors`**：moby 按 **official index** 解析凭据（`registry.GetAuthConfigKey`：official 一律用 `https://index.docker.io/v1/` 这个键），镜像源主机自己那条 `auths` 记录不会被查。所以镜像源必须「内容匿名可读」，凭据只能在**显式带 namespace 的拉取**上起作用。
- 裸 `host:port` 的 `registry-mirrors` 仍然可写；**写路径（`daemon.json` 的 mirror URI 带 path）会被 dockerd 判非法**（§2.1.1）。

`rms` 与镜像源的关系因此是一句话：**`/v2/` 401 是设计，内容匿名是前提，`probe_ttl` 窗口内不重复探测**。

那么「镜像名里不带 namespace」这种需求怎么办？两条路：

1. **无限定名 + `:5000`**：对 Docker Hub 有效。`registry-mirrors` 只对 Hub 生效，docker 发的就是 `library/alpine` 这类无限定名，rms 会自动挑到 `docker.io`（因为只有它有 `library/alpine`）。这是现在唯一「透明」的场景。
2. **其它上游**：镜像名里带 namespace（`<host>:5000/ghcr.io/owner/app:tag`），或用 containerd 的 `hosts.toml` + `override_path = true` 把某个上游钉在路径上（§2.3）。

> **事实边界（新）**：镜像源指 `:5000` 时，docker 拉 Docker Hub 的 `alpine` 这类无限定名**仍能命中**（无限定请求会挑到 `docker.io`）；但 `ghcr.io` 等其它上游的无限定名**可能被挑到别的 namespace，或谁都对不上而返回 `404 NAME_UNKNOWN`**。适用范围：所有只写裸 `host:port` 的客户端；不适用于镜像名里带 namespace 前缀的调用（那种请求根本不探测）。

### 2.1.1 为什么客户端没法用路径来表达 namespace

「`registries.yaml` / `daemon.json` 里能不能写 `rms.watsons.eu.org/docker.io` 这样的路径？」——**都不能**，这是查过源码的结论：

**containerd（`core/remotes/docker/config/hosts.go`）**

```go
if len(u.Path) > 0 {
    u.Path = path.Clean(u.Path)
    if !strings.HasSuffix(u.Path, "/v2") && !config.OverridePath {
        u.Path = u.Path + "/v2"     // 路径被拼在 /v2 之前
    }
} else if !config.OverridePath {
    u.Path = "/v2"
}
```

- 旧式 `registries.yaml` 的 `mirrors.<domain>.endpoint` **没有** `override_path` 字段，所以 `endpoint: http://host/docker.io` 会请求 `/docker.io/v2/<repo>` —— 不是我们要的 `/v2/docker.io/<repo>`。
- 只有新式 `config_path` + `hosts.toml` 显式写 `override_path = true`，endpoint 才能写成 `http://host/v2/docker.io`（见 §2.3）。

**Docker `daemon.json`（moby `registry/config.go` `ValidateMirror`）**

```go
if (uri.Path != "" && uri.Path != "/") || uri.RawQuery != "" || uri.Fragment != "" {
    return "", invalidParamf("invalid mirror: path, query, or fragment at end of the URI %q", uri)
}
```

- 当前 dockerd **直接判非法**（写进去 dockerd 起不来），对应 [moby#47144](https://github.com/moby/moby/issues/47144)；放宽校验的是 PR [#46594](https://github.com/moby/moby/pull/46594)。
- 另外 `registry-mirrors` **只对 Docker Hub 生效**，对 ghcr.io / gcr.io / quay.io 无效。

所以：**containerd 想要「透明镜像」（镜像名不变）只能靠 `hosts.toml + override_path`**（§2.3），没有路径可写的旧式 `registries.yaml` 只能给 Docker Hub 用（§2.2）。

### 2.2 k3s / containerd `registries.yaml`

```yaml
mirrors:
  docker.io:
    endpoint:
      - "http://192.168.188.101:5000"     # 裸 host:port；namespace 由 rms 自己挑
configs:
  "192.168.188.101:5000":
    auth:
      username: alice
      password: alicepw          # 私有 namespace 才需要；公共缓存可省略
    tls:
      insecure_skip_verify: true # 明文 HTTP 时 containerd 需要允许
```

`k3s` 放 `/etc/rancher/k3s/registries.yaml`，然后 `systemctl restart k3s`。

**这里给不出「一个上游一个 endpoint」的写法了**（旧的 `ghcr.io: endpoint: [...:5002]` 已失效）：`registries.yaml` 没有 `override_path`，endpoint 不能带路径；现在所有 proxy namespace 都只在 `:5000` 后面，无限定名交给 rms 挑。要精确指定上游，用镜像名前缀或 §2.3 的 `hosts.toml`。

### 2.3 containerd `hosts.toml`（能给某个上游做透明镜像）

```toml
# /etc/containerd/certs.d/docker.io/hosts.toml
server = "https://registry-1.docker.io"

[host."http://192.168.188.101:5000/v2/docker.io"]
  capabilities = ["pull", "resolve"]
  override_path = true
```

`override_path = true` 让 containerd 把整个路径当作 API 根，于是请求正好是 `/v2/docker.io/<repo>/...`，落到主端点。对**任意**上游都成立，不只是 Hub：

```toml
# /etc/containerd/certs.d/ghcr.io/hosts.toml
server = "https://ghcr.io"

[host."http://192.168.188.101:5000/v2/ghcr.io"]
  capabilities = ["pull", "resolve"]
  override_path = true
  skip_verify = true
```

这条路径可用但不常用，因为是「不可见配置」，排障时容易迷惑。

### 2.4 Docker `daemon.json`

```json
{
  "insecure-registries": ["192.168.188.101:5000"],
  "registry-mirrors": ["http://192.168.188.101:5000"]
}
```

`registry-mirrors` 只对 Docker Hub 生效（发的是无限定名，由 rms 挑到 `docker.io`）；其它上游用显式镜像名：

```bash
docker pull 192.168.188.101:5000/docker.io/library/nginx
docker pull 192.168.188.101:5000/ghcr.io/owner/app
docker pull 192.168.188.101:5000/nginx          # 无限定 + library/ 注入，落到 docker.io
```

`localhost` / `127.0.0.1` 之外的宿主需要加 `insecure-registries`（现在 rms 是明文 HTTP，TLS 由外层反代做）。

> 从按上游绑定的那些端口迁过来的人注意：它们已经不存在，`daemon.json` 里指向它们的条目要改成 `:5000`（只留一个即可）。端口号与逐条迁移清单见 `deploy/103/README.md`。

---

## 3. 网络可达性矩阵（决定上游怎么填）

实测本环境：

| 来源宿主 | 公共仓库（registry-1.docker.io 等） | `10.82.64.172:5000/5001/5002`（**内网镜像主机自己的端口，与 rms 暴露的端口无关**） |
|---|---|---|
| `192.168.188.101`（开发宿主，有 tun_sase） | **直连失败**；走 `http://192.168.188.1:3128` 才通 | ✅ 可达 |
| `192.168.188.103`（公共容器环境） | 握手直连可达，但**层下载**直连很差（12 KB/s、传不完）→ 实际仍走 `3128` 代理 | ❌ 不可达 |

对应配置：

- **跑在 `.101`**：必须给代理（`.env` 里的 `HTTP_PROXY` / `HTTPS_PROXY`，以及 `NO_PROXY` 排除 `192.168.188.0/24,10.82.0.0/16,10.44.0.0/16`），并且可以让 `docker.io` 先链内网镜像再回落官方：

  ```yaml
  docker.io:
    upstreams:
      # 这三行是「内网镜像主机」自己的地址与端口（它有多个实例），
      # 不是 rms 对外暴露的端口 —— rms 只有 5000 与 9100。
      - "http://10.82.64.172:5000"
      - "http://10.82.64.172:5001"
      - "https://registry-1.docker.io"
  ```

- **跑在 `.103`**：`upstreams` 只写官方地址（写内网镜像地址会让该 namespace 一直降级），但**出网要走局域网代理**——这一点与早期文档写的「.103 直连即可」相反，是实测推翻的：`.103` 对公共仓库的 `/v2/` 握手直连可达，但**层下载**直连很糟（1ms 直连 12 KB/s、传不完；经代理 168 KB/s、完整；官方 Hub 直连直接不可达）。现在的 `.103` 部署就是这样配的，见 `deploy/103/.env` 的 `RMS_UPSTREAM_PROXY` 与 `deploy/103/rms.yaml` 里该 namespace 的实测注释。

`upstream.http.proxy` 为空时回落到标准环境变量。注意实现细节：**distribution 的上游请求直接用 `http.DefaultTransport`**，所以 rms 启动时会替换 `http.DefaultTransport`（见 `internal/httpx`），只设环境变量在进程内已有自定义 transport 时可能失效。

另外公共上游在探测时可能接近超时（`reports/raw/selection-real-upstream.txt` 里 6 个真实上游各 12 次探测：quay.io 中位 2.7s、registry.k8s.io 中位 3.1s，最坏打满 10s 的那次是 quay.io），所以 `upstream.health.timeout` 与 `namespaces.selection.probe_timeout` 默认都给到 10s。

---

## 4. 请求解析与鉴权

### 4.1 namespace 解析顺序

`/v2/<第一段>/<其余>` 按以下顺序判定：

1. **`_catalog`**：rms 自己处理（合并视图，仅管理员）。
2. **首段像域名**（含 `.` 或 `:`，或等于 `localhost`，与 Docker 自身规范化规则一致）→ 该 upstream namespace，**直连，不探测**。
3. **首段是可写 namespace**（某个 htpasswd 用户名，或 `namespaces.local` / `auth.shared_namespaces` 里的名字）→ 本地 namespace。
4. **否则**（首段不是域名也不是可写 namespace）→ **无限定请求**：并发探测所有能服务的 proxy namespace，挑真有该仓库且响应最快的一个（见 §6.1）。没有任何候选有它 → `404 NAME_UNKNOWN`；所有候选都探不通 → `503 UNAVAILABLE`。

写请求（push）同样按 1-3 判定：本地 namespace 可以直接推；落到第 4 步则说明路径里没有可写 namespace，会被拒（§4.2）。

注：**旧版的「默认 namespace 改写」已删除**（`namespaces.default` 不再存在），所以 `rms/<user>/<image>` 不再被改写成 `rms/docker.io/<user>/<image>`；`<user>` 是本地 namespace，照旧推拉；而 `alpine` 这种既不是域名也不是用户名的仓库名走第 4 步，由探测决定落到哪个 namespace。单段仓库名在 `docker.io` 这个 namespace 上仍会注入 `library/`（`official_library: true`），但它现在是**该 namespace 的属性**，不是全局改写规则。

### 4.2 鉴权与推送权限

| 方法 | namespace 类型 | 规则 |
|---|---|---|
| `GET /v2/` | — | **总是**在无凭据时返回 `401` + `WWW-Authenticate: Basic realm="rms"` |
| 读（GET/HEAD） | proxy | `anonymous_pull_proxy: true` 时匿名可读 |
| 读（GET/HEAD） | local | 默认需要登录（`anonymous_pull_local`） |
| 写 | proxy | 一律 `403 DENIED`，消息里给出该往哪推 |
| 写 | local | 需要登录；namespace 属于用户名或 admin 才可推 |
| 写 | shared | 任意已认证用户可推 |

**为什么 `/v2/` 必须挑战**：Docker 客户端是「被挑战驱动」的。如果 `/v2/` 对匿名请求返回 200，会同时坏两件事，都是在真实 daemon 上实测到的：

- `docker login` 不会去校验密码（它只把凭据发到被挑战的 API 根），于是**错密码也报 Login Succeeded**；
- 后续 `docker push` 不会带上已保存的凭据，blob 的 `HEAD` 拿到 401，push 直接失败。

`401` 只用于告知认证方式；真正的匿名读取策略仍然按 namespace 在内容请求上判定，所以公共缓存的匿名拉取不受影响（含 containerd 透明镜像，已用 `ctr` 验证）。

**没有例外了**：`listeners[].anonymous_root` 随端口一起删除，`/v2/` 在任何情况下都挑战。docker 的 `registry-mirrors` 是否因此绕过缓存**已实测**：不会——只要内容匿名可读就正常走缓存，见 §2.1。

> 想更进一步（Harbor 那样用 Bearer token、按 scope 授权），需要额外做 token 签发端点，属于独立工作。

### 4.3 镜像模式：pull 匿名、只有 push 私有 namespace 要认证

这是「当一个给 dockerd `registry-mirrors` / containerd `registries.yaml` 用的镜像源」的完整配方，也正是本项目的默认形态：

```yaml
auth:
  type: htpasswd                 # 必须开启：push 与 docker login 都靠它
  htpasswd: /etc/rms/htpasswd
  realm: rms
  user_namespace: true           # 每个 htpasswd 用户拥有同名 namespace（<user>/<image>）
  shared_namespaces: [shared]    # ⚠️ 别把 me 写进来：写进来 = 任何登录用户都能推 me
  anonymous_pull_proxy: true     # ← 拉取免认证（proxy / 缓存 namespace）
  anonymous_pull_local: false    # ← 私有 namespace 读也要登录
  admin_users: [admin]
```

- **pull**：`docker pull <host>:5000/library/alpine`、`docker pull <host>:5000/ghcr.io/owner/app` —— 匿名，不需要 `docker login`。
- **push 到私有 namespace**：`docker push <host>:5000/me/app:v1` —— 必须 `docker login`，且 `me` 属于用户 `me`（或 admin；把 `me` 列进 `shared_namespaces` 则是任意登录用户可推）。
- **push 到 proxy namespace**：一律 `403 DENIED`（只读缓存）。
- ⚠️ **不要用 `auth.type: none` 换取「拉取免认证」**：那会让 `me` 的 push 也不再认证（`Decide` 在认证关闭时对本地 namespace 的写请求直接放行）。启动日志与 `rms config validate` 都会把这两种形态点名：
  - `authentication is disabled: anyone who can reach this endpoint may push to its local namespaces`
  - `anonymous pulls of proxy namespaces are disabled: a Docker daemon using this host as a registry mirror will abandon it and pull from the upstream registry instead`
  - `anonymous pulls of local namespaces are enabled: every client can read every private namespace`


---

## 5. 存储布局与缓存行为

每 namespace 一个根目录，内部是 distribution 原生布局：

```
/data/<namespace>/docker/registry/v2/
├── blobs/sha256/<aa>/<digest>/data
└── repositories/<repo>/_manifests/{tags,revisions}/...
```

`<root>` 由 `storage.root` 控制，`/data/<namespace>` 就是清理与计量的边界。例如：

```
rms/<me>/<image>:<version>        -> /data/me/...
rms/<docker.io>/<user>/<image>:…  -> /data/docker.io/...
```

**已知取舍**：可写 namespace 的仓库名里保留了 namespace（`alice/app`），所以路径是 `/data/alice/docker/registry/v2/repositories/alice/app/...`，namespace 会出现两次。这是刻意的：引擎用**仓库名**拼上传返回的 `Location`，剥掉前缀会让客户端拿到不可用的 URL（`test/distbehavior` 把这条上游行为固定住了）。proxy namespace 则必须剥掉前缀，因为上游 URL 需要的是不带 namespace 的仓库名。

**缓存命中实测**（内容已在本地）：

| 请求 | 延迟 | 说明 |
|---|---|---|
| `GET manifests/<tag>` | ~0.9s | **每次都回源校验**：distribution 的 `proxyTagService.Get` 先问上游、再回落本地，这样 tag 移动能被感知 |
| `GET manifests/<digest>` | ~2ms | 本地 |
| `GET blobs/<digest>` | ~5ms | 本地 |
| `GET tags/list` | ~0.9s | 回源 |

也就是说：**层数据（体积的大头）本地读，tag 解析一次回源**。这与 `registry:2` proxy 模式一致。上游不可达时 tag 查询会回落本地，缓存仍可用。

指标语义：`rms_cache_hits_total` 表示「对象已在本地、无需拉取内容」；tag 命中仍可能有一次上游校验请求。

`namespaces.proxy.<ns>.ttl: 0`（默认）表示永不过期，同时**不安装 distribution 的 TTL 调度器**——这一点是必需的，否则上游切换重建引擎时两个调度器会争用同一份 `scheduler-state.json`。

---

## 6. 选仓、上游选举、故障转移与降级

### 6.1 无限定请求的选仓（selection）

路径里没写 namespace 的读请求（`GET /v2/library/alpine/manifests/latest`）由**并发探测**决定落到哪个 proxy namespace：

1. 候选 = 当前**能服务**的所有 proxy namespace（引擎处于降级/不可用的 namespace 不进候选）。每个候选按自己的规则算出要探测的仓库路径 —— 特别是 `official_library: true` 的 `docker.io` 会把单段名补成 `library/<name>`。
2. 对每个候选（**以及该 namespace 的每个上游**，见 §6.2）并发发 `HEAD <upstream>/v2/<仓库>/manifests/<ref>`，单个探测受 `probe_timeout` 限制。`2xx` 才算「有」；`401` 时按 `WWW-Authenticate` 取 Bearer token 重试一次；`405/501` 降级为 `GET`；其余状态（`403`/`404` 等）与网络错误都算「没有/不通」。
3. 在有的候选里取**探测耗时最小**者；并列取名字序在前者。全部不通 → `503 UNAVAILABLE`；全部明确没有 → `404 NAME_UNKNOWN`。
4. 结果按 (仓库, tag/digest) 缓存 `probe_ttl`，同一窗口内不再探测；blob 请求按**仓库**复用这个决策。
5. 转发后若引擎回 `404` 或 `5xx` 且方法是 `GET/HEAD`，会作废该决策并**重选一次**（只重选一次）；写请求不重选。

**代价（必须知道）**：未命中缓存时，一次无限定请求要等所有候选探测完（最慢的那个，上限 `probe_timeout`）。实测 6 个真实上游并发探测的墙钟中位数约 6s、最坏打满 10s，见 `reports/raw/selection-real-upstream.txt`。想避免这个开销就用显式 namespace 前缀，那条路径完全不探测。

### 6.2 上游并发（racing）、digest 权威与负载均衡

一个 namespace 写了多个 `upstreams` 时，它们**默认是并发候选**，而不是有序的故障转移链（`upstream_race: true`，默认）：

1. **候选对**：无限定请求的候选是「namespace × 它的每个上游」；带 namespace 的读请求（`GET /v2/docker.io/...`）同样跑这一套，只是候选限定在该 namespace 内。一次冷请求的并发宽度就是候选对数——`docker.io` 三个上游 + `ghcr.io` 两个 + `quay.io` 两个，正好 7 条连接。
2. **探测内容路径**：每个候选对并发发 `HEAD <upstream>/v2/<仓库>/manifests/<ref>`（blob 请求走 blob 路径）。`2xx` 且媒体类型像 manifest/blob 才算「有」；`401` 按挑战取 Bearer ticket 重试一次；`405/501` 降级为 `GET`；`403/404` 等是「明确没有」；`5xx/429/超时`是「没答上来」。细节同 §6.1。
3. **digest 权威（同 hash 才均衡）**：每个 namespace 内，**配置顺序里第一个答出内容的上游**定义该 tag 的 digest（它不发 `Docker-Content-Digest` 时 rms 取一次 manifest 自己算）。其余上游**只有 digest 与之相同**才可能被服务/被均衡；digest 不同者判为过期镜像站，既不均衡也不当选，计入 `rms_upstream_digest_mismatch_total`。没有 digest 证据时只服务最快的一个，不做均衡。
4. **namespace 之间**：仍按「最快可用对」竞争（并列时先比上游 EWMA，再比 namespace 名）。**跨 namespace 不做 digest 仲裁**：不同 namespace 是不同来源的注册表。
5. **均衡池**：胜出 namespace 里 digest 一致、且延迟在 `balance_window`（默认 1.5×）内的上游构成池；池内按 `balance`（默认 `weighted` = 1/延迟加权）做**平滑加权轮询**（nginx 式，无突发）。窗口外的镜像不参与均衡，只在当选者失败时接手。`balance: off` 只留最快一个，`round_robin` 等权。
6. **决策缓存**：结果按 (仓库, tag/digest) 缓存 `probe_ttl`（默认 `30s`）；窗口内**不再并发**，池内轮询仍逐请求生效。blob 按仓库复用 manifest 的决策。
7. **失败重试**：转发后若回 `404/5xx`（`GET/HEAD`），作废决策并**重选一次**；代理读出现 `5xx` 会把该上游记为「内容不可用」（见下）。写请求不参与。

**「探活正常、取内容失败」的缺口已补**：内容探测的结论会回灌上游健康状态，`fail_threshold` 次（默认 2）连续「没答上来」就把该上游放进 `2 × health.interval` 的冷却，冷却期内不再进入候选；它再答上一次就立即恢复。`/v2/` 健康探测**不会**清掉这个判定（两套状态是分开的），这正是 daocloud 那种「`/v2/` 0.17s 回 401、大 blob 卡满 60s」的形态能被自动切走的原因。代理读的 `5xx` 也会计一次内容失败。

**代价与取舍**：

- 冷决策的墙钟 = 最慢候选对的探测时间（上限 `probe_timeout`，默认 10s）。`decision_deadline`（默认 `0`=等全部）可在**第一个探测答出后**再等这么久就用已有结果决策；被放弃的候选记为 `skipped(decision deadline)`，**绝不**当作「没有内容」，也**不会**成为 digest 权威。代价是：权威上游若正好是慢的那个，该 namespace 这次退化为「只服务最快的一个，不均衡」。
- 每个 (namespace, upstream) 一个引擎（懒构建，只在它被选中/答出内容后构建），共享同一 storage root。安全性依据：内容是内容寻址 + 唯一临时文件原子改名；proxy namespace 关闭 TTL 调度器，没有第二个调度器争用 `scheduler-state.json`；唯一共享可变对象是 tag link，而 digest 门保证只有同 digest 的引擎会写它。代价是内存随上游数增长（每个 namespace ≤ `len(upstreams)` 个引擎）。
- 并发宽度 = 候选对数。稳态不放大（决策窗口内不重复探测），冷回源时上游看到的请求数就是候选对数。

**向上游本身要求认证时**（例如把另一台开了 htpasswd 的 rms 当作上游）：必须在该 namespace 配 `username`/`password`。选仓探测是匿名的、且内容路径匿名可读（`anonymous_pull_proxy: true`）时会成功，但 embedded proxy 在建立 Basic 挑战后会给内容请求带上（空的）`Authorization`，而 rms 对「凭据被拒绝」绝不回落到匿名（§4.2）→ 401 → 本地 tag store 回落到空 → 404，且耗时只有几毫秒。症状是「探测说存在、取内容 404」，实测读数见 `reports/raw/101-live-upstream-race.txt` §E。

**旧的顺序选举仍在**：`namespaces.proxy.<ns>.upstream_race: false` 让该 namespace 退回「配置顺序里第一个健康者当选、失败重建引擎」的语义（`upstream.health.*` 控制），用于想要严格优先级顺序的部署。默认的并发模式下，`upstream.health` 只用来做廉价淘汰，不再决定服务哪个上游。

**降级（cache-only）不变**：上游全不可达时 namespace 退化成只读缓存引擎，已缓存镜像仍可拉，`/readyz` 报 503 并列出降级 namespace；上游恢复后自动构建为正常代理。带 namespace 的请求在上游全不可达时**不会**因此 503——它回落到该 namespace 的粘性引擎继续服务缓存（best-effort）。

实测（`test/e2e` 的 `TestNamespaceUpstreamRaceServesTheFastestMirror`，以及 `reports/rms-upstream-race.md`）：同一 namespace 两个镜像、配置顺序故意把慢的放前面，一次无限定拉取由快的那台服务，慢的那台只收到探测；决策窗口内的第二次拉取不再并发。


---

## 7. 可观测性

默认上报本环境的 SigNoz（`192.168.188.103`）：

- **Traces**：OTLP gRPC → `192.168.188.103:4317`，`service.name=rms`，span 名保持低基数（`GET /v2/docker.io/library/alpine/manifests`）。
- **Metrics**：`:9100/metrics`（另有 `/registry/metrics` 暴露 distribution 自带指标）

  | 指标 | 含义 |
  |---|---|
  | `rms_http_requests_total{listener,namespace,method,status}` | 请求数 |
  | `rms_http_request_duration_seconds` | 请求耗时 |
  | `rms_cache_hits_total` / `rms_cache_misses_total` | 缓存是否命中本地对象 |
  | `rms_upstream_election_changes_total{namespace,upstream}` | 上游切换次数（并发模式下只在粘性引擎跟着选举走时增长） |
  | `rms_namespace_up{namespace}` | 1=正常代理，0=降级 |
  | `rms_namespace_errors_total` | namespace 生命周期失败次数 |
  | `rms_upstream_probe_duration_seconds{namespace,upstream}` | 内容探测耗时（即 race 的判定依据；失败/超时也记录） |
  | `rms_upstream_wins_total{namespace,upstream}` | 请求被交给哪个上游——开了均衡时，这个分布就是均衡本身 |
  | `rms_upstream_digest_mismatch_total{namespace,upstream}` | 内容 digest 与权威上游不一致而被拒绝服务的次数（>0 说明某镜像站过期） |
  | `rms_selection_decisions_total{source}` | 决策来源：`probe` / `decision-cache` / `cache`（本地已有）/ `affinity`（按仓库亲和） |
  | `rms_build_info` | 版本 |

- **Logs**：JSON 到 stdout（`docker logs`），字段含 `namespace`/`request_id`；`observability.logs.otlp` 预留。
- 另外启动时会检查系统 CA 池：为空会告警（`ca_roots=0`），因为瘦基础镜像缺 `ca-certificates` 会让**所有** HTTPS 上游报 `certificate signed by unknown authority`——这个坑实测踩到过。

---

## 8. 配置

完整示例见 `config/rms.yaml`，所有值支持 `${VAR}` / `${VAR:-default}`（**只在标量值里展开，注释和键名不展开**；未定义且无默认值会启动失败）。

关键项：

| 键 | 说明 |
|---|---|
| `server.listen` / `server.external_url` | 主端点；`external_url` 留空表示「按请求推导来源」，直连时最省事；TLS 在外层终止时必须填 `https://...`，否则上传 URL 会是 http |
| `server.http_secret` | 上传状态签名密钥；留空则由存储根+主机名派生（稳定但不可控），会告警 |
| `storage.root` | 每 namespace 的父目录 |
| `auth.*` | htpasswd 路径、`user_namespace`、共享 namespace、匿名拉取策略、管理员 |
| `namespaces.local` / `namespaces.proxy.<domain>.upstreams` | namespace 表；proxy 键必须是域名样式，local 名不能是域名样式。**没有 `namespaces.default`** |
| `namespaces.selection.probe_ttl` | 无限定请求的决策缓存时间（默认 `30s`）；同 (仓库, tag/digest) 在窗口内直接复用。它同时是**仓库亲和**窗口：blob 请求路径里没有仓库名，它复用同仓库 manifest 的决策，随这个窗口一起过期 |
| `namespaces.selection.probe_timeout` | 单个候选探测的上限（默认 `10s`），也是未命中缓存时请求等待决策的上限。与 `upstream.health.timeout` 同量级 |
| `namespaces.selection.decision_deadline` | 软截止（默认 `0`=等所有候选答完）：第一个探测答出后再等这么久就用已有结果决策，未答的候选记为 `skipped`。必须 ≤ `probe_timeout`。见 §6.2 的取舍 |
| `namespaces.selection.balance` / `balance_window` | 均衡模式（`weighted` 默认 / `round_robin` / `off`）与均衡窗口（默认 `1.5`，只把该倍数以内、且 digest 一致的上游放进轮询池） |
| `namespaces.proxy.<domain>.upstream_race` | 默认 `true`：该 namespace 的上游并发竞争；`false` 退回旧的顺序选举 |
| `namespaces.proxy.<domain>.balance` | 按 namespace 覆盖均衡模式（留空继承全局） |
| `upstream.health.*` / `upstream.http.*` | 探测与出网（代理、超时、CA、`insecure_skip_verify`） |
| `observability.*` | 日志、OTLP traces、metrics 监听与路径 |

`rms config validate --config config/rms.yaml` 可以先把配置跑一遍（严格模式：未知键会报错）。

---

## 9. 部署

### 9.1 docker compose（默认）

```bash
make prepare      # 建 ./data，chown 给容器 uid 65532；把 config/* 调成容器可读
make up
```

容器以 `65532:65532`（nonroot）运行，因此：

- `./data` 必须能被 65532 写 —— `make prepare` 负责；
- `./config/rms.yaml` 0644、`./config/htpasswd` `root:65532 0640`（里面是密码哈希，不给 world read）—— `make prepare` 负责。

`docker-compose.yml` 里所有端口、数据目录、代理变量都可通过 `.env` 覆盖。

**只有两个端口：`5000`（唯一的内容入口）与 `9100`（指标）。** 根目录这份 compose 与
`deploy/103/docker-compose.yml` 是**同一形态**（单端点 + 无限定请求自动选仓），不会分叉；
`.env` / `.env.example` 里也没有 `RMS_PORT_DOCKERIO` / `RMS_PORT_GHCR` 这类变量了。理由与代价见 §2.1。

> 早先的部署把 `:5001` 绑到 `docker.io`、`:5002` 绑到 `ghcr.io`，那是靠**已经删除**的 `listeners`
> 配置块实现的。**只把端口映射加回 compose 是不够的**：配置里已经没有能把端口绑定到 namespace 的键，
> 容器起来了也无处可路由。要恢复那种形态，得先把 `listeners` 特性做回来。

### 9.2 镜像构建的两条路径

| 方式 | 何时用 |
|---|---|
| `make image`（`Dockerfile`，多阶段） | 常规。需要能拉 `golang:1.27-alpine` 与 distroless 基础镜像 |
| `make image-binary`（`Dockerfile.binary`） | CI，或拉不到 Go 工具链镜像的环境。先用 `make build` 在宿主产出静态二进制，再打进运行时镜像 |

两者基础镜像都可用 `--build-arg BUILDER_BASE=` / `RUNTIME_BASE=` 覆盖。**运行时基础镜像必须带 CA 证书**（`make image-binary` 会先断言二进制是静态链接，避免 musl 下「no such file or directory」这种迷惑报错）。

依赖已 vendor（`vendor/`，1.3MB），所以镜像构建**不需要网络**。

### 9.3 生产建议

- TLS 与域名由外层反代（Caddy/nginx）终止，然后设置 `RMS_EXTERNAL_URL=https://rms.example.org`，否则上传 `Location` 会是 http。
- `:9100` 只绑内网或本机。
- 用 `RMS_HTTP_SECRET` 固定上传状态密钥。
- 定期按 namespace 清缓存：`du -sh data/*` 定位，删除整个 `<root>/<namespace>` 目录即可（只影响该 namespace 的缓存）。

---

## 10. 冲突点与设计取舍（汇总）

| # | 冲突/风险 | 处置 |
|---|---|---|
| C1 | 需求 2（push）与需求 1/3/4（proxy 缓存）在同一 namespace 空间冲突 | 路径里没有可写 namespace 的 `push` 不会落到任何 proxy namespace，直接 `403 DENIED` 并提示应推到哪（旧版是「落到默认仓库再拒」，现在没有默认仓库这一步） |
| C2 | `me` 既是用户名又可能是路径首段 | 显式配置的 shared namespace **优先于**同名用户；要拉 Hub 上的同名仓库请写全 `docker.io/...` |
| C3 | 官方镜像需注入 `library/` | `official_library: true` 时对单段仓库注入（与 crproxy 一致） |
| C4 | 路径前缀形态无法被旧式 `registries.yaml` 当透明镜像（endpoint 不能带路径） | 端口绑定形态已删除；透明镜像改用 `hosts.toml + override_path`，Hub 之外的仓库用「镜像名带 namespace」 |
| C5 | `registry-mirrors` 只对 Docker Hub 生效 | 非 Hub 仓库统一用「镜像名带 namespace」或 containerd 侧 `hosts.toml` |
| C6 | 多 endpoint 故障转移无法复用 distribution proxy 包 | 改成 namespace 级选举 + 重建引擎，并明确它不是请求级重试 |
| C7 | proxy 模式不能 push（实测 `405 UNSUPPORTED`） | 放弃「crproxy + registry:2 proxy」架构，改为按 namespace 分离可写/只读引擎 |
| C8 | 跨 namespace 不去重 | 这是「按仓库点缓存」的直接代价，换来按 namespace 独立计量/清理/迁移 |
| C9 | 上游可达性依赖宿主 | §3 的可达性矩阵 + 两套配置 |
| C10 | 可写 namespace 路径里 namespace 出现两次 | 保留前缀是为让引擎返回的 `Location` 正确，见 §5 |
| C11 | tag 请求无法完全离线（除非上游挂掉） | distribution 行为：tag 优先回源、失败回落本地。层数据本地读，这就是加速的主要来源 |
| C12 | `/v2/` 匿名 200 会同时破坏 login 校验与 push | 改为总是挑战；匿名读策略下沉到 namespace 级 |
| C13 | docker 的 `registry-mirrors` 不带凭据，被 C12 的挑战挡在门外（实测 dockerd 报 `no basic auth credentials` 后转投官方仓库，缓存被整体绕过） | 端口与 `anonymous_root` 都已删除，改用主端点 `:5000` + 实测确认缓存命中（§2.1）；内容请求本就不挑战，API 根仍挑战 |
| C14 | 无限定请求要并发探测，未命中缓存时得等最慢的候选 | 接受：上限是 `probe_timeout`（默认 10s），实测中位约 6s；要零开销就写显式 namespace 前缀（§6.1） |
| C15 | 「无限定名 + 裸 `host:port`」无法表达「我要 ghcr.io 上的那个」 | 只对 Docker Hub 可靠（只有 `docker.io` 有 `library/*`）；其它上游用镜像名前缀或 `hosts.toml`，否则可能被挑到别的 namespace 而 `404`（§2.1 的事实边界） |

---

## 11. 运维

```bash
# 用户管理（bcrypt，写文件是原子的）
make htpasswd USER=bob PASS=bobpw      # 顺带把权限调成容器用户可读（root:65532 0640）
./bin/rms htpasswd list --file config/htpasswd

# 直接用 CLI 时文件是 0600（里面有密码哈希），容器读不到，需要自己放权：
#   ./bin/rms htpasswd add --file config/htpasswd --user bob --password bobpw
#   chgrp 65532 config/htpasswd && chmod 0640 config/htpasswd

# 热加载 htpasswd（不用重启）
docker kill -s HUP rms

# 配置校验 / 状态
./bin/rms config validate --config config/rms.yaml
curl -s localhost:5000/healthz
curl -s localhost:5000/readyz          # 503 时列出降级 namespace
docker compose exec rms wget -qO- localhost:9100/metrics 2>/dev/null || curl -s localhost:9100/metrics
```

限流、镜像防漏拉、对象存储后端等 crproxy 具备但本服务未实现的能力，见 §12。

---

## 12. 开发

```bash
source scripts/env.sh     # 本沙箱里 Go 缓存与 DOCKER_CONFIG 必须落回仓库内
make check                # fmt + vet + 单测
make test-race
make cover
make e2e                  # 端到端：进程内假上游 + 真 HTTP，不依赖网络
make e2e-docker           # 再驱动真实 docker CLI 走一遍 pull/login/push
make vendor               # 刷新 vendor/
```

代码结构：

```
cmd/rms/            CLI：serve / healthcheck / htpasswd / config validate
internal/config/    配置解析与严格校验（含 `${VAR:-default}` 值展开）
internal/namespace/ 请求路径 → (namespace, 引擎路径) 的唯一真相
internal/registryapp/ 一个 namespace 一个 distribution 引擎（含只读降级引擎）
internal/registry/  引擎生命周期、上游选举、故障转移重建
internal/upstream/  endpoint 健康探测与选举
internal/auth/      htpasswd 校验 + 按 namespace 的推送 ACL
internal/server/    监听装配、dispatcher（含选仓接线）、catalog、健康端点
internal/selection/ 无限定请求的并发探测与选仓（缓存、失效、重选）
internal/storage/   distribution 磁盘布局的只读查询（catalog、缓存命中判定）
internal/obs/       日志、Prometheus 指标、OTLP traces
internal/httpx/     出网 transport（代理、超时、CA）
test/distbehavior/  用测试固定住依赖的 distribution 上游行为
test/e2e/           真实客户端端到端
```

单测覆盖率（核心逻辑）：`auth` 93%、`upstream` 96%、`config` 87%、`namespace` 84%、`storage` 83%、`registryapp` 100%、`obs` 80%。dispatcher 的请求路径主要由 `test/e2e` 覆盖（它需要活着的引擎与上游）。

`test/distbehavior` 值得特别一提：它把 rms 依赖的 4 条 distribution 行为写成可执行文档（`HTTP.Host` 的 path 会被丢弃、上传 `Location` 由仓库名拼出、上游不可达时 `handlers.NewApp` 会 panic、proxy namespace 拒绝上传）。升级 distribution 时这些会立刻变红，而不是让推送地址悄悄错掉。

**尚未实现**（按需再加）：Bearer token 服务端、限流/带宽控制、镜像白名单/黑名单、S3 等对象存储后端、LRU/按容量清理、单 namespace 级配额。

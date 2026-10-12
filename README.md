# rms — 镜像代理 / 加速 / 缓存服务

单个 Go 进程，一个端点同时做三件事：

1. **拉取加速与缓存**：把公共仓库按 namespace 代理并缓存到本地磁盘（`/data/<namespace>`）。
2. **私有仓库推送**：`docker login` 认证后往自己的 namespace 推镜像。
3. **默认仓库改写**：不带 namespace 的请求自动落到默认仓库（`docker.io`）。

镜像引擎复用 [`distribution/distribution/v3`](https://github.com/distribution/distribution)，rms 负责它不提供的部分：namespace 路由、默认仓库改写、推送鉴权、上游选举与故障转移、HTTP 代理、可观测性。

---

## 1. 快速开始

```bash
cp .env.example .env          # 按部署宿主机调整代理配置
make htpasswd USER=alice PASS=alicepw
make prepare                  # 建目录并把权限调成容器用户可用
make up                       # docker compose up -d --build
make logs
```

`make up` 会：构建镜像、把 `./data` 挂到 `/data`、把整个 `./config` 目录挂到 `/etc/rms`，并启动 `:5000`（主端点）、`:5001`/`:5002`（按 upstream 绑定的镜像端口）、`:9100`（指标与 pprof）。

> `config/` 里的 `htpasswd` 是**演示凭据**（alice/bob/me/admin），上线前请用 `make htpasswd USER=... PASS=...` 替换或删掉。
>
> 配置目录是**整目录挂载**而不是逐文件挂载：`rms htpasswd add` 用「写临时文件再 rename」的方式原子替换 htpasswd，而文件级 bind mount 会钉住原来的 inode，容器会一直读到旧文件、`kill -s HUP` 看起来毫无作用。

冒烟测试：

```bash
curl -i http://localhost:5000/v2/                     # 401 + WWW-Authenticate: Basic realm="rms"
docker pull localhost:5000/docker.io/library/alpine   # 显式 namespace
docker pull localhost:5000/alpine                     # 默认仓库改写
docker login localhost:5000 -u alice                  # 认证
docker tag alpine localhost:5000/alice/app:v1
docker push localhost:5000/alice/app:v1               # 推私有 namespace
```

---

## 2. 客户端怎么配（重要，含实测结论）

rms 支持两种端点形态，**因为两种客户端只能接受其中一种**：

| 形态 | 用法 | 适用客户端 |
|---|---|---|
| **路径前缀**（主端点 `:5000`） | `docker pull <host>:5000/docker.io/library/alpine`，namespace 写在镜像名里 | docker / containerd / podman，任何客户端 |
| **端口绑定 namespace**（`:5001`、`:5002`） | `docker pull <host>:5001/library/alpine`，路径里没有 namespace | containerd `registries.yaml`、docker `registry-mirrors` |

### 2.1 为什么必须提供端口绑定形态

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

所以：**containerd 想要「透明镜像」（镜像名不变）就必须用端口绑定形态。**

### 2.2 k3s / containerd `registries.yaml`

```yaml
mirrors:
  docker.io:
    endpoint:
      - "http://192.168.188.101:5001"     # endpoint 自己代表 docker.io
  ghcr.io:
    endpoint:
      - "http://192.168.188.101:5002"
configs:
  "192.168.188.101:5001":
    auth:
      username: alice
      password: alicepw          # 私有 namespace 才需要；公共缓存可省略
    tls:
      insecure_skip_verify: true # 明文 HTTP 时 containerd 需要允许
```

`k3s` 放 `/etc/rancher/k3s/registries.yaml`，然后 `systemctl restart k3s`。

### 2.3 containerd `hosts.toml`（可选，用路径前缀）

```toml
# /etc/containerd/certs.d/docker.io/hosts.toml
server = "https://registry-1.docker.io"

[host."http://192.168.188.101:5000/v2/docker.io"]
  capabilities = ["pull", "resolve"]
  override_path = true
```

`override_path = true` 让 containerd 把整个路径当作 API 根，于是请求正好是 `/v2/docker.io/<repo>/...`，落到主端点。这条路径可用但不常用，因为是「不可见配置」，排障时容易迷惑。

### 2.4 Docker `daemon.json`

```json
{
  "insecure-registries": ["192.168.188.101:5000", "192.168.188.101:5001", "192.168.188.101:5002"]
}
```

然后**用显式镜像名**（不需要 `registry-mirrors`，因为路径写不进去）：

```bash
docker pull 192.168.188.101:5000/docker.io/library/nginx
docker pull 192.168.188.101:5000/ghcr.io/owner/app
docker pull 192.168.188.101:5000/nginx          # 默认仓库 + library/ 注入
```

`localhost` / `127.0.0.1` 之外的宿主需要加 `insecure-registries`（现在 rms 是明文 HTTP，TLS 由外层反代做）。

---

## 3. 网络可达性矩阵（决定上游怎么填）

实测本环境：

| 来源宿主 | 公共仓库（registry-1.docker.io 等） | `10.82.64.172:5000/5001/5002` |
|---|---|---|
| `192.168.188.101`（开发宿主，有 tun_sase） | **直连失败**；走 `http://192.168.188.1:3128` 才通 | ✅ 可达 |
| `192.168.188.103`（公共容器环境） | ✅ 直连可达 | ❌ 不可达 |

对应配置：

- **跑在 `.101`**：必须给代理（`.env` 里的 `HTTP_PROXY` / `HTTPS_PROXY`，以及 `NO_PROXY` 排除 `192.168.188.0/24,10.82.0.0/16,10.44.0.0/16`），并且可以让 `docker.io` 先链内网镜像再回落官方：

  ```yaml
  docker.io:
    upstreams:
      - "http://10.82.64.172:5000"
      - "http://10.82.64.172:5001"
      - "https://registry-1.docker.io"
  ```

- **跑在 `.103`**：不要配代理，`upstreams` 只写官方地址；写内网地址会让该 namespace 一直降级。

`upstream.http.proxy` 为空时回落到标准环境变量。注意实现细节：**distribution 的上游请求直接用 `http.DefaultTransport`**，所以 rms 启动时会替换 `http.DefaultTransport`（见 `internal/httpx`），只设环境变量在进程内已有自定义 transport 时可能失效。

另外这两个地址在探测时都可能接近超时（实测 quay.io 4.2s、registry.k8s.io 偶发 5s+），所以 `upstream.health.timeout` 默认给到 10s。

---

## 4. 请求解析与鉴权

### 4.1 namespace 解析顺序

`/v2/<第一段>/<其余>` 按以下顺序判定：

1. **端口绑定了 namespace**：该 namespace 固定；路径里若又出现同名首段会被剥掉（两种端点形态可用同一个 URL）。
2. **`_catalog`**：rms 自己处理（合并视图，仅管理员）。
3. **首段像域名**（含 `.` 或 `:`，或等于 `localhost`，与 Docker 自身规范化规则一致）→ 该 upstream namespace。
4. **首段是可写 namespace**（某个 htpasswd 用户名，或 `namespaces.local` / `auth.shared_namespaces` 里的名字）→ 本地 namespace，**优先级高于默认仓库**。
5. **否则** → 默认 namespace（`docker.io`），实现 `rms/<user>/<image>` → `rms/docker.io/<user>/<image>`；单段仓库还会注入 `library/`（`alpine` → `library/alpine`）。

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

> 想更进一步（Harbor 那样用 Bearer token、按 scope 授权），需要额外做 token 签发端点，属于独立工作。

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

## 6. 上游选举、故障转移与降级

- 每个 proxy namespace 有一组有序 `upstreams`，写多个即为故障转移候选。
- 健康探测 `GET <endpoint>/v2/`，`200`/`401`/`403` 都算「说得来 v2 协议」，其余算失败。
- `fail_threshold`（默认 2）次连续失败后换下一个；`success_threshold`（默认 1）次成功后可以重新当选。**配置顺序即优先级**，本地镜像恢复后会重新被优先使用。
- **切换粒度是整个 namespace 重新选举 + 重建引擎**，不是单 blob 请求级重试。原因：distribution 的 proxy 只接受一个 `RemoteURL`，且它的 `realmAllowed()` 会拒绝「realm 与 RemoteURL 不在同一注册域」的 Bearer 挑战，所以不能把 `RemoteURL` 指向进程内的故障转移监听器（Docker Hub / ghcr 会直接 401）。
- **降级（cache-only）**：如果启动时上游全都不可达，该 namespace 会退化成「只读缓存引擎」，**已缓存的镜像仍然可拉**，`/readyz` 报 `503` 并列出降级 namespace；上游恢复后自动重建为正常代理。写操作在降级态一律 `403`。

实测：把 `docker.io` 的第一个候选指向死地址、第二个指向官方，启动后自动当选第二个；把全部候选指向死地址，`docker pull` 已缓存的 alpine 依然成功（跨重启有效）。

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
  | `rms_upstream_election_changes_total{namespace,upstream}` | 上游切换次数 |
  | `rms_namespace_up{namespace}` | 1=正常代理，0=降级 |
  | `rms_namespace_errors_total` | namespace 生命周期失败次数 |
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
| `namespaces.default` / `local` / `proxy.<domain>.upstreams` | namespace 表；proxy 键必须是域名样式，local 名不能是域名样式 |
| `listeners` | 额外监听；`namespace: ""` 等于主端点形态（可重复写主端口，会被忽略） |
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
| C1 | 需求 2（push）与需求 1/3/4（proxy 缓存）在同一 namespace 空间冲突 | 未加 namespace 的 `push` 会落到默认仓库（只读），返回可读的 `403 DENIED` 并提示应推到哪 |
| C2 | `me` 既是用户名又可能是路径首段 | 显式配置的 shared namespace **优先于**同名用户；要拉 Hub 上的同名仓库请写全 `docker.io/...` |
| C3 | 官方镜像需注入 `library/` | `official_library: true` 时对单段仓库注入（与 crproxy 一致） |
| C4 | 路径前缀形态无法被 k3s/containerd 当透明镜像 | 提供端口绑定形态；`hosts.toml + override_path` 作为可选路径 |
| C5 | `registry-mirrors` 只对 Docker Hub 生效 | 非 Hub 仓库统一用「镜像名带 namespace」或 containerd 侧配置 |
| C6 | 多 endpoint 故障转移无法复用 distribution proxy 包 | 改成 namespace 级选举 + 重建引擎，并明确它不是请求级重试 |
| C7 | proxy 模式不能 push（实测 `405 UNSUPPORTED`） | 放弃「crproxy + registry:2 proxy」架构，改为按 namespace 分离可写/只读引擎 |
| C8 | 跨 namespace 不去重 | 这是「按仓库点缓存」的直接代价，换来按 namespace 独立计量/清理/迁移 |
| C9 | 上游可达性依赖宿主 | §3 的可达性矩阵 + 两套配置 |
| C10 | 可写 namespace 路径里 namespace 出现两次 | 保留前缀是为让引擎返回的 `Location` 正确，见 §5 |
| C11 | tag 请求无法完全离线（除非上游挂掉） | distribution 行为：tag 优先回源、失败回落本地。层数据本地读，这就是加速的主要来源 |
| C12 | `/v2/` 匿名 200 会同时破坏 login 校验与 push | 改为总是挑战；匿名读策略下沉到 namespace 级 |

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
internal/server/    多监听器装配、dispatcher、catalog、健康端点
internal/storage/   distribution 磁盘布局的只读查询（catalog、缓存命中判定）
internal/obs/       日志、Prometheus 指标、OTLP traces
internal/httpx/     出网 transport（代理、超时、CA）
test/distbehavior/  用测试固定住依赖的 distribution 上游行为
test/e2e/           真实客户端端到端
```

单测覆盖率（核心逻辑）：`auth` 93%、`upstream` 96%、`config` 87%、`namespace` 84%、`storage` 83%、`registryapp` 100%、`obs` 80%。dispatcher 的请求路径主要由 `test/e2e` 覆盖（它需要活着的引擎与上游）。

`test/distbehavior` 值得特别一提：它把 rms 依赖的 4 条 distribution 行为写成可执行文档（`HTTP.Host` 的 path 会被丢弃、上传 `Location` 由仓库名拼出、上游不可达时 `handlers.NewApp` 会 panic、proxy namespace 拒绝上传）。升级 distribution 时这些会立刻变红，而不是让推送地址悄悄错掉。

**尚未实现**（按需再加）：Bearer token 服务端、限流/带宽控制、镜像白名单/黑名单、S3 等对象存储后端、LRU/按容量清理、单 namespace 级配额。

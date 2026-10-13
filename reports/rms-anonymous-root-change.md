# rms 改动:`listeners[].anonymous_root` —— 让 docker 的 registry-mirrors 用得上 rms

日期:2026-10-12 · 起因:`reports/mirror-probe-103.md` 证明 docker 的 `registry-mirrors` 被 rms 的 `/v2/` 401 挑战整体挡回,docker 侧的原始日志是 `Head "http://<rms>/v2/library/alpine/manifests/3.16": no basic auth credentials`,同时 rms 一个请求都没收到。

## 改了什么

| 文件 | 改动 |
|---|---|
| `internal/config/config.go` | `Listener` 增字段 `AnonymousRoot bool \`yaml:"anonymous_root"\``;校验里只允许「绑定到 proxy namespace」的监听器开启(主端点与 local namespace 一律拒绝) |
| `internal/server/dispatch.go` | dispatcher 增字段 `anonymousRoot`;`Server.Handler` 增加该参数;API 根(`KindBase`)在「未携带任何凭据 + 该端口开了开关」时不再挑战 |
| `internal/server/server.go` | `s.Handler(l.Namespace, l.AnonymousRoot)` |
| `internal/config/config_test.go` | 新增 `TestAnonymousRootValidation`、`TestAnonymousRootReachesTheListener` |
| `internal/server/dispatch_test.go` | 新增 `TestDispatcherAnonymousRoot` |
| `README.md` | §2.4 新增小节、§4.2 例外说明、§8 配置表、§10 冲突表 C13 |
| `config/rms.yaml` | 5001 监听器下加注释示例(默认不开启) |

核心逻辑(dispatch.go),只有「**完全没带凭据**」这一种情况被放宽:

```go
	denied := d.auth.Decide(r, res, id, ok, present)
	if denied != nil && d.anonymousRoot && !present &&
		res.Kind == namespace.KindBase && denied.Status == http.StatusUnauthorized {
		// A mirror socket answers the API root anonymously.
		//
		// Only the "no credentials at all" case is relaxed: credentials that
		// were supplied but rejected keep their 401, so an anonymous root can
		// never turn a wrong password into a success.
		denied = nil
	}
```

带上错密码仍然是 401;local namespace 与主端点不受影响 —— 依赖挑战的两件事(`docker login` 校验密码、push 携带凭据)都只在那些端口上发生。

## 顺带修掉的构建缺陷

`Dockerfile` 把 `ARG RUNTIME_BASE=...` 声明在第一个 `FROM` **之后**(第 48 行),而 `ARG` 在 stage 内声明时对后续 `FROM` 不可见。当前 BuildKit 直接拒绝构建:

```
ERROR: failed to build: failed to solve: base name (${RUNTIME_BASE}) should not be blank
 - UndefinedArgInFrom: FROM argument 'RUNTIME_BASE' is not declared (line 43)
```

`docker history rms:dev` 显示 101 上现存的镜像其实是用 `Dockerfile.binary`(`COPY bin/rms`)构建的,所以多阶段路径此前从未真正跑通 —— 而 `docker compose up --build` 默认走的就是它,103 上必然会踩。已把 `RUNTIME_BASE` 与 `BUILDER_BASE` 一起提到首个 `FROM` 之前,并加注释说明原因。修好后 `make image` 45s 构建成功。

## 验证

### 1. 单元 / 静态检查

```
$ make check
go fmt ./... ; go vet ./... ; go test ./...
ok  github.com/watsons/rms/internal/auth 3.402s
ok  github.com/watsons/rms/internal/config 0.015s
ok  github.com/watsons/rms/internal/server 0.741s
... (全部包通过)

$ go test ./internal/config/ -run TestAnonymousRoot -v
--- PASS: TestAnonymousRootValidation (0.00s)
    --- PASS: .../proxy_namespace_on_its_own_socket
    --- PASS: .../multi-namespace_socket_must_keep_challenging
    --- PASS: .../local_namespace_must_keep_challenging
--- PASS: TestAnonymousRootReachesTheListener (0.00s)

$ go test ./internal/server/ -run 'TestDispatcherAnonymousRoot|TestDispatcherAPIRootContract' -v
--- PASS: TestDispatcherAPIRootContract (0.18s)
--- PASS: TestDispatcherAnonymousRoot (0.36s)

$ make e2e
ok  github.com/watsons/rms/test/e2e 3.209s
```

### 2. 三种监听器的 API 根行为(101 上临时实例,端口 5010/5011/5012)

```
port 5010 -> HTTP 401  Www-Authenticate: Basic realm="rms"     # 主端点,仍挑战
port 5011 -> HTTP 200                                           # docker.io + anonymous_root
port 5012 -> HTTP 401  Www-Authenticate: Basic realm="rms"     # docker.io,未开开关
/readyz -> ready
```

### 3. 端到端复测(103 上隔离 dockerd,镜像源 = 101:5011)

改动前同一条路径:`no basic auth credentials`,rms 零请求。
改动后:

```
Mirrors=[http://192.168.188.101:5011/]
Status: Downloaded newer image for alpine:3.15
docker.io/library/alpine:3.15
$ grep -ac "no basic auth credentials" dockerd3.log
0

# rms 侧请求指标 diff(改动前 → 改动后)
> rms_http_requests_total{listener="docker.io",method="GET",namespace="docker.io",status="200"} 5
> rms_http_requests_total{listener="docker.io",method="GET",namespace="none",status="200"} 3
> rms_http_requests_total{listener="docker.io",method="HEAD",namespace="docker.io",status="404"} 1

# 缓存落盘
2.9M  .probe/rms-probe/data/docker.io
       .../repositories/library/alpine/{_manifests,_layers,_uploads}
```

即:docker 的拉取真正经过 rms 并写进了它的缓存。`HEAD .../manifests/...` 那条 404 是 docker 在探测某个平台特定 manifest 时的正常试探(随后仍成功拉取),已登记为待解释项。

## 部署侧怎么用

```yaml
listeners:
  - listen: "0.0.0.0:15000"
    namespace: docker.io
    anonymous_root: true
```

客户端 `daemon.json`:

```json
{ "registry-mirrors": ["http://192.168.188.103:15000"] }
```

## 复现

```bash
cd /root/dev/registries-mirrors
source scripts/env.sh
make check && make e2e && make image
# 临时实例(仓库内 .probe/,不污染线上);配置见 .probe/rms-probe/config/rms.yaml
RMS_HTPASSWD=$PWD/.probe/rms-probe/config/htpasswd ./bin/rms config validate --config .probe/rms-probe/config/rms.yaml
```

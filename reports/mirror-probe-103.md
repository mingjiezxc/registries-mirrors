# 103 上「docker registry-mirrors 是否真的走 rms」对照实测

日期:2026-10-12 · 目的:在动任何线上容器之前,确认把 15000–15006 交给 rms 后,docker 客户端(101/103 的 `daemon.json`)的拉取是否真的落到 rms。

## 结论(先说)

**不会。** rms 对 `/v2/` 一律返回 `401` 挑战,docker 的 `registry-mirrors` 请求不带凭据,于是 dockerd 直接放弃该镜像源回落官方 Hub:

```
level=info msg="Attempting next endpoint for pull after error:
  Head \"http://192.168.188.101:5001/v2/library/alpine/manifests/3.16\": no basic auth credentials"
```

同一时刻 rms 侧**一个请求都没收到**(请求指标无差异、日志行数不变)。作为对照,镜像源换成旧的 `registry:2`(匿名 200)时,docker 的请求确实落到该容器上。

影响:如果直接切换,docker 客户端表面上「还能拉」(回落官方 Hub),但**缓存与加速全部失效**;而 103 直连 Hub 实测不稳定(见文末异常),回落并不总是成功。

## 实验环境

在 103 上起了一个**隔离的临时 dockerd**,不影响线上 dockerd 与任何运行中的容器:

```
dockerd --config-file /var/tmp/dockerd-probe/daemon.json \
  --data-root /var/tmp/dockerd-probe/data --exec-root /var/tmp/dockerd-probe/exec \
  --host unix:///var/tmp/dockerd-probe/docker.sock --pidfile /var/tmp/dockerd-probe/docker.pid \
  --bridge=none --iptables=false --ip6tables=false --ip-forward=false
# Server version 27.3.1(与 103 线上 dockerd 同版本)
```

探针用的镜像源是 rms 在 **101:5001** 上的现成实例 —— 这是 `namespace: docker.io` 的端口绑定监听,形态与 103 将来的 15000 完全一致(README §2.2/§2.4)。

## 对照 A:镜像源 = 旧 registry:2(103:15000),预期「会走镜像源」

隔离 dockerd 配置:

```
Mirrors=[http://192.168.188.103:15000/]
```

拉取 `alpine:3.19`(旧缓存里只有 `3.18.2 / 3.20 / latest`,3.19 未缓存):

```
Status: Downloaded newer image for alpine:3.19
docker.io/library/alpine:3.19
```

`docker logs reg-docker` 最近 2 分钟内匹配 `3.19` 的行数 = **4**,其中:

```
192.168.188.103 - - [12/Oct/2026:13:08:42 +0000] "HEAD /v2/library/alpine/manifests/3.19 HTTP/1.1" 200 8077 "" "docker/27.3.1 ... UpstreamClient(Docker-Client/27.3.1 (linux))"
... http.request.uri="/v2/library/alpine/manifests/3.19" ... http.response.status=200 http.response.duration=9.476052293s
192.168.188.103 - - [12/Oct/2026:13:08:52 +0000] "GET /v2/library/alpine/blobs/sha256:17a39c0b... HTTP/1.1" 200 3419815 "" "docker/27.3.1 ..."
```

→ 阳性对照成立:**`registry-mirrors` 这条路径是可观测的**,docker 确实会主动找镜像源要 manifest 与 blob。

## 对照 B:镜像源 = rms(101:5001),预期「会走 rms」

隔离 dockerd 配置:

```
Mirrors=[http://192.168.188.101:5001/]
```

拉取 `alpine:3.16`(未缓存),拉取在探测 dockerd 日志里留下的关键行:

```
level=info msg="Attempting next endpoint for pull after error: Head \"http://192.168.188.101:5001/v2/library/alpine/manifests/3.16\": no basic auth credentials"
level=info msg="Attempting next endpoint for pull after error: Head \"https://registry-1.docker.io/v2/library/alpine/manifests/3.16\": Get \"https://auth.docker.io/token?...\": read tcp 192.168.188.103:35846->172.64.144.78:443: read: connection reset by peer"
level=error msg="Handler for POST /v1.47/images/create returned error: ..."
Error response from daemon: Head "https://registry-1.docker.io/v2/library/alpine/manifests/3.16": ... connection reset by peer
```

即:第一跳就是 rms,但 docker 因为拿不到 Basic 凭据而**在本地放弃**,连请求都没发出去;第二跳官方 Hub 又被 reset,整个 pull 失败。

同一次会话里另一次拉取(`alpine:3.18`)回落官方 Hub 成功,同时——

```
=== rms 请求指标 diff(空=rms 一个请求都没收到)===
(无差异)
=== rms 日志行数 before/after === 108 / 108
```

→ rms 的 `rms_http_requests_total` 与日志在整个拉取前后**逐字节相同**。

## 为什么

rms 的 `/v2/`(API 根)**总是**回 401 挑战(`internal/auth/auth.go` 的 `Decide` → `KindBase` 分支),这是刻意的:docker 是「被挑战驱动」的客户端,`docker login` 的密码校验与后续 `push` 的凭据携带都依赖这次挑战(README §4.2、冲突点 C12)。代价是:docker 的 `registry-mirrors` 端点**无法携带凭据**,于是 401 直接把它挡在门外。

旧的 `registry:2` 没有这个矛盾,因为它匿名返回 200。

## 可选处置(待定)

- **A(推荐)**:给 namespace 绑定的监听器加一个开关,让 API 根在该监听器上不挑战(即 `GET /v2/` 回 200,像 `registry:2`),主端点 `5000` 保持挑战不变。只影响 pull-only 的镜像端口,推送鉴权语义不动。
- B:把 103 上 rms 的认证全局关掉。零代码,但 rms 变成开放 registry(本地 namespace 匿名可读写),推送鉴权一并失去。
- C:不改 rms,15000–15006 继续由旧 `registry:2` 提供,rms 只上 `5000` 主端点(适合能配凭据的 containerd/k3s 或显式镜像名)。「接管」不完整。

## 反常(未解释,已登记)

- 探测期间 103 直连 `registry-1.docker.io` / `auth.docker.io` 出现 `read: connection reset by peer`(两次拉取都撞上),而同一时段 `curl https://registry-1.docker.io/v2/` 仍能拿到 401、5.8s。直连不稳定本身是「需要一个可用本地镜像源」的额外理由。
- 隔离 dockerd 日志里有一条与本次无关的 `failed to process event ... No such container`(dockerd 自身的容器事件),不影响本次结论。

## 复现命令

```bash
# 探针(103)
P=/var/tmp/dockerd-probe
cat > $P/daemon.json <<'EOF'
{ "registry-mirrors": ["http://192.168.188.101:5001"],
  "insecure-registries": ["192.168.188.101:5001","192.168.188.103:15000"] }
EOF
nohup dockerd --config-file $P/daemon.json --data-root $P/data --exec-root $P/exec \
  --host unix://$P/docker.sock --pidfile $P/docker.pid \
  --bridge=none --iptables=false --ip6tables=false --ip-forward=false > $P/dockerd2.log 2>&1 &
docker -H unix://$P/docker.sock pull alpine:3.16
grep -a "no basic auth credentials" $P/dockerd2.log

# rms 侧是否收到(101)
curl -s http://127.0.0.1:9100/metrics | grep '^rms_http_requests_total' | sort
```

# 103 冒烟测试(临时端口,不碰旧栈)

日期:2026-10-12 · 目的:在摘除旧容器之前,用**同一镜像、同一配置**、只改端口与数据目录,确认 rms 在 103 上能正常起来并服务。

```bash
ssh root@192.168.188.103
cd /opt/stacks/rms
docker run -d --name rms-smoke --restart no \
  -p 25000:5000 -p 25001:15000 -p 25004:15004 -p 29100:9100 \
  -v /opt/stacks/rms/.smoke-data:/data -v /opt/stacks/rms/config:/etc/rms:ro \
  -e RMS_STORAGE_ROOT=/data -e RMS_HTPASSWD=/etc/rms/htpasswd rms:dev
```

## 结果

```
=== 主端点 /v2/(应 401)===
HTTP/1.1 401 Unauthorized
Www-Authenticate: Basic realm="rms"

=== 镜像端口 /v2/(应 200)===
  25001 -> HTTP 200          # docker.io,anonymous_root 生效
  25004 -> HTTP 200          # ghcr.io,anonymous_root 生效

=== /healthz /readyz ===
ok
ready
readyz=200

=== 各 namespace 探活 ===
rms_namespace_up{namespace="docker.io"} 1
rms_namespace_up{namespace="gcr.io"} 1
rms_namespace_up{namespace="ghcr.io"} 1
rms_namespace_up{namespace="quay.io"} 1
rms_namespace_up{namespace="registry.k8s.io"} 1

=== 上游选举 ===
docker.io -> https://docker.m.daocloud.io
gcr.io -> https://gcr.io
ghcr.io -> https://ghcr.io
quay.io -> https://quay.io
registry.k8s.io -> https://registry.k8s.io
```

rms 日志里同时也确认了 Bearer 域校验通过(这正是先前担心的点):

```
"msg":"Discovered token authentication URL: https://m.daocloud.io/auth/token"
"msg":"Registry configured as a proxy cache to https://docker.m.daocloud.io"
"msg":"namespace ready","namespace":"docker.io","mode":"proxy","root":"/data/docker.io","upstream":"https://docker.m.daocloud.io"
```

## 经镜像端口真实拉取

```
$ docker pull 127.0.0.1:25001/library/busybox:latest
Status: Downloaded newer image for 127.0.0.1:25001/library/busybox:latest
real 0m17.091s

# rms 侧(节选):manifest HEAD 154ms → index 1.0ms → layer 16.7s / 2226163B
{"http.request.uri":"/v2/library/busybox/manifests/latest","http.response.status":200,"http.response.duration":"153.948882ms"}
{"http.request.uri":"/v2/library/busybox/blobs/sha256:37bb94b0...","http.response.status":200,"http.response.duration":"16.678205992s","http.response.written":2226163}

# 落盘
3.6M  /opt/stacks/rms/.smoke-data/docker.io
      .../repositories/library/alpine  .../repositories/library/busybox
```

第二次拉同一个 tag 走本地:0.54s。

## 启动耗时(运维须知)

rms 会先把每个 namespace 的引擎建好再开始监听,而每个引擎都要先探测上游、发现 token 端点(103 上每个约 7s),5 个 namespace 依次建完约 **40–60 秒**,期间端口是拒绝连接的。systemd/compose 的 `start_period: 30s` 偏紧,健康检查会在 `starting` 多待一会儿——接管切换时要留出这个窗口。

## 异常与限制

1. **上游 daocloud 会间歇性失败**。本次冒烟期间出现过一次:经 25001 拉 `busybox:latest` 连续 6 次重试后返回 `500 Internal Server Error`(rms 侧累计 11 条 `status=500`),5 分钟后同一镜像 17s 拉取成功。rms 的故障转移由**上游探活**驱动,而 daocloud 的 `/v2/` 一直正常返回 401(判定为健康),所以这种「探活正常、取内容失败」的情况**不会自动切换**——这是当前机制的已知边界(README §6)。
2. 三个上游在 103 上的实测吞吐(同一 layer,2.2MB):

   | 上游 | 直连 | 走 `192.168.188.1:3128` |
   |---|---|---|
   | `docker.m.daocloud.io` | **295 KB/s,完整** | 401(token 绑客户端 IP,经代理源 IP 变了) |
   | `docker.1ms.run` | 12 KB/s,90s 未传完 | 168 KB/s,完整 |
   | `registry-1.docker.io` | **连不上(000)** | 13.7 KB/s,90s 未传完 |

   所以「daocloud → 1ms → 官方」的顺序保持不变,且**不配代理**是对的:代理对 daocloud 直接无效(IP 绑定的 token),对官方 Hub 也只是勉强可用。旧的 `registry:2` 栈里 `reg-docker` 是走代理访问官方的,但实测现在这条路径很慢。
3. 首次拉取 `alpine:3.20` 花了 4m12s(镜像 3.6MB),同样是上游抖动所致,不是 rms 的解析问题(第二次 0.54s)。

## 清理

```bash
docker rm -f rms-smoke
rm -rf /opt/stacks/rms/.smoke-data
docker image rm 127.0.0.1:25001/library/alpine:3.20 127.0.0.1:25001/library/busybox:latest
```

旧栈的 7 个容器与 15008 在整个冒烟期间未受影响(仍 Up 8 days)。

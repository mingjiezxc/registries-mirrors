# 103 上的 rms 镜像服务:部署与接管报告

日期:2026-10-12 · 目标机 `192.168.188.103`(公共容器环境)· 部署目录 `/opt/stacks/rms`

## 1. 做了什么

原 `/opt/stacks/registry` 用 **8 个 `registry:2` 容器**提供镜像服务(15000–15006 七个端口 + 15008 私有仓库)。现在 **7 个镜像端口由单个 `rms` 进程接管**,`15008` 私有仓库原样保留。`101` 与 `103` 的 `/etc/docker/daemon.json` **未做任何改动**,仍然把 `103:15000–15006` 当镜像源。

| 端口 | namespace | 上游(按序故障转移) | 缓存目录 |
|---|---|---|---|
| 15000 / 15001 / 15002 | `docker.io` | `https://docker.1ms.run` → `https://registry-1.docker.io` | `data/docker.io/` |
| 15003 | `gcr.io` | `https://gcr.io` | `data/gcr.io/` |
| 15004 | `ghcr.io` | `https://ghcr.io` | `data/ghcr.io/` |
| 15005 | `registry.k8s.io` | `https://registry.k8s.io` | `data/registry.k8s.io/` |
| 15006 | `quay.io` | `https://quay.io` | `data/quay.io/` |
| 5000 | 主端点(多 namespace,可推送) | 路径里写 namespace | `data/<user>` 等 |
| 9100 | 指标 / pprof | — | — |

上游出网走局域网代理 `http://192.168.188.1:3128`(理由见 §5)。旧栈这三个 Hub 端口(官方 / daocloud / 1ms)合并成一个 `docker.io` namespace:三个端口命中同一份缓存,上游差异交给有序故障转移。

## 2. 落地物

| 位置 | 内容 |
|---|---|
| `deploy/103/docker-compose.yml` `rms.yaml` `.env` `README.md` `install.sh` | 103 专用部署文件与同步/构建脚本(仓库内,可复现) |
| `192.168.188.103:/opt/stacks/rms/` | 上述文件落地后的部署目录(含 vendor、config、data) |
| `192.168.188.103:/opt/stacks/registry/docker-compose.yml.bak-20261012-141308` | 旧栈 compose 备份 |
| `reports/mirror-probe-103.md` | 「docker registry-mirrors 是否走 rms」的对照实测 |
| `reports/rms-anonymous-root-change.md` | 为此给 rms 加的 `listeners[].anonymous_root` 改动与验证 |
| `reports/103-stage-smoke.md` | 临时端口冒烟 |
| `reports/103-cache-migration.md` | 摘旧容器 + 缓存迁移的逐项数字 |
| `reports/103-cutover-verify.md` `reports/raw/*` | 切换后的端到端验证与原始输出 |

## 3. 关键结论(实测)

- **docker 的 `registry-mirrors` 必须让镜像端口匿名回 200**。rms 原本对 `/v2/` 一律 401 挑战,实测 dockerd 会因此**整体放弃这个镜像源**回落官方仓库(日志原文:`no basic auth credentials`,rms 侧零请求)。为此在 rms 里加了 `listeners[].anonymous_root`,只允许绑定代理 namespace 的端口使用;主端点 5000 仍挑战,`docker login` 与 push 的凭据语义不变。依据:`reports/mirror-probe-103.md`、`reports/rms-anonymous-root-change.md`。
- **旧 `registry:2` 的缓存可以直接用**:42G 的 docker.io 缓存 mv 成 `data/docker.io` 后,rms 拉已缓存的 `alpine:3.20` 是 3.9 秒、层响应 0.6–6.8ms(回源同一层要 3m12s)。
- **接管是真的在服务客户端**:切换后 `192.168.188.102`(k3s 节点)拉了 `library/nginx`,`101` 拉 `alpine:3.14` 在 rms 侧留下完整记录。
- **上游链路是瓶颈,不是 rms**:未缓存镜像回源要几分钟(实测 103 侧 `alpine:3.12` 6m49s、`alpine:3.15` 4m40s、`alpine:3.16` 6m06s;空闲时 101 侧 `alpine:3.14` 19.6s),同一个 tag 第二次拉取只要 1–4 秒。

## 4. 运维手册

```bash
ssh root@192.168.188.103 && cd /opt/stacks/rms
docker compose ps                                   # 状态(healthy)
docker compose logs -f --tail=100                   # 日志(JSON)
curl -s localhost:5000/healthz; curl -s localhost:5000/readyz   # 503 时列出降级 namespace
curl -s localhost:9100/metrics | grep -E 'rms_namespace_up|rms_cache_hits_total'
du -sh data/*                                       # 各 namespace 占用
```

配置改动后:`docker compose up -d --force-recreate`(配置文件是挂载的,但 rms 只在启动时读取)。
新增用户:`make htpasswd USER=x PASS=y` 后 `docker kill -s HUP rms`。

## 5. 上游选型的依据(为什么是 1ms + 代理)

| 上游 | 直连 | 经 `192.168.188.1:3128` |
|---|---|---|
| `docker.m.daocloud.io` | 小 blob 295 KB/s,大 blob 卡 60s 报 500 | 401(token 绑客户端 IP) |
| `docker.1ms.run` | 12 KB/s,90s 未传完 | **168 KB/s,完整** |
| `registry-1.docker.io` | 连不上(000) | 13.7 KB/s,90s 未传完 |

daocloud 的 500 根因已定位:它把 blob 302 到 `image-mirror.r2.daocloud.vip`,rms 侧那条请求 `http2: timeout awaiting response headers` 卡满 60s。旧 `registry:2` 栈在 .103 上给 5 个镜像配的也是同一个代理 —— 这一点修正了仓库 README §3「.103 不需要代理」的旧结论:`.103` 对 `/v2/` 握手确实直连可达,但**层下载**直连很差。

## 6. 回滚

```bash
ssh root@192.168.188.103
cd /opt/stacks/rms && docker compose down                 # 让出 15000-15006
cd /opt/stacks/registry
cp docker-compose.yml.bak-20261012-141308 docker-compose.yml
docker compose up -d                                     # 7 个 registry:2 回来
```

缓存已被 mv 到 `/opt/stacks/rms/data/`,回滚后旧容器从空缓存重新拉(要连缓存一起回滚,先把 5 个目录 mv 回去)。

## 7. 遗留事项与建议

1. **上游带宽/稳定性**是这套服务的最大短板(§5)。可选的下一步:接入公司内网镜像(需要 101 那样的 SASE 链路,103 目前不可达)、或对上游做限速/预取、或把常拉镜像预热进缓存。
2. **rms 的故障转移只看探活**(`/v2/` 返回 200/401/403 即算健康),「探活正常但取内容失败」不会自动切换。若要更好的韧性,可考虑给 proxy 增加请求级回退(代码改动)。
3. `15008` 私有仓库仍由旧 `registry:2` 提供。rms 的 local namespace + htpasswd 可以替代,但客户端需要 `docker login`、且 rms 不提供旧容器那套 CORS 头 —— 属独立工作。
4. `config/htpasswd` 仍是仓库里的演示凭据(alice/bob/me/admin),上线前应换掉。
5. `RMS_HTTP_SECRET` 目前为空(rms 用存储根 + 主机名派生,启动有一条 WARN);用 `openssl rand -hex 32` 生成后填入 `deploy/103/.env` 更稳妥。
6. `daemon.json` 里把 `15003–15006`(gcr/ghcr/k8s/quay)也列进了 `registry-mirrors` —— 而 docker 的 `registry-mirrors` 只对 Docker Hub 生效,这几条其实只会浪费一次尝试。可精简为 `15000–15002` + `insecure-registries` 全留(未改,因为要重启 dockerd)。
7. 101 上那台 rms(`/root/dev/registries-mirrors`)也可以顺手打开 `anonymous_root`,让它的 docker 客户端同样受益;本次未动。

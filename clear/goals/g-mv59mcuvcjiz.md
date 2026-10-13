# 目标 g-mv59mcuvcjiz(rev3 · open)
- **一句话**:在 192.168.188.103 上用 docker compose 部署 rms,接管原 registry:2 镜像服务的 15000–15006 端口与其缓存,并验证 101/103 客户端的 docker 拉取确实经过 rms
- **主张**:在 192.168.188.103 上用 docker compose 部署 rms,接管原 registry:2 镜像服务的 15000–15006 端口与其缓存,并验证 101/103 客户端的 docker 拉取确实经过 rms。
- **升格门槛**:L3
- **判据(全文)**(未逐条拆分):
  ①103 上 /opt/stacks/rms 存在自洽部署目录(docker-compose.yml + 103 专用 config/rms.yaml + .env + README);②docker ps 显示 rms 容器 healthy 且监听 5000、15000–15006、9100;③原 7 个 registry:2 镜像容器(reg-docker/reg-daocloud/reg-1ms/reg-gcr/reg-ghcr/reg-k8s/reg-quay)已移除,原 compose 文件有备份与可执行回滚步骤,15008 私有 registry 仍在运行;④103:/opt/stacks/rms/data 下 docker.io、ghcr.io、gcr.io、quay.io、registry.k8s.io 五个目录存在,du 数字与迁移前逐项一致;⑤从 101 与 103 各实测一次经 15000 端口的 docker pull(取未缓存镜像),rms 侧出现该请求记录且缓存目录/缓存命中指标随之变化;⑥curl -i http://192.168.188.103:15000/v2/ 返回 401 挑战,/readyz 200 且 rms_namespace_up 五个 namespace 全为 1;⑦工作区 reports/103-rms-deploy.md 记录上述数字、回滚命令,以及「docker daemon.json 的 registry-mirrors 是否真的经 rms」的实测结论。
> 这份文件由系统按账本落盘(投影产物);权威是账本里的 `goal/set` 与 `criteria/revised`。
## 假设(4)
- `h-jixcpd` [proposed] 把旧 registry:2 proxy 的每个 namespace 缓存目录直接 mv 成 rms 的 <root>/<namespace> 后,rms 能直接命中其中已缓存的镜像,无需回源重下。
  - 推翻条件:rms 收到旧缓存已有镜像的请求时 rms_cache_hits_total 不增长,或 blob 请求返回 404 / 走上游重下。
  - scope: {"conditions":{"上游":"registry-1.docker.io,ghcr.io,gcr.io,quay.io,registry.k8s.io,docker.m.daocloud.io","主机":"192.168.188.103","端口":"5000/15000-15006/9100","部署方式":"docker compose","namespace":"docker.io,ghcr.io,gcr.io,quay.io,registry.k8s.io","迁移方式":"mv 目录"},"ranges":{}}
  - uses (framing declarations): []
  - 断言:1 条
- `h-nccqnt` [refuted] 101/103 的 daemon.json 保持 15000–15006 不变时,docker pull 未缓存镜像的请求确实落到 rms 并被缓存,而不是被 rms 的 /v2/ 401 挑战逼回官方 Hub。
  - 推翻条件:docker pull 成功但 rms 日志/metrics 里没有该镜像的请求,且 rms 的 data/ 无新增缓存。
  - scope: {"conditions":{"上游":"registry-1.docker.io,ghcr.io,gcr.io,quay.io,registry.k8s.io,docker.m.daocloud.io","主机":"192.168.188.103","端口":"5000/15000-15006/9100","部署方式":"docker compose","客户端主机":"192.168.188.101,192.168.188.103","客户端配置":"daemon.json registry-mirrors 指向 103:15000-15006"},"ranges":{}}
  - uses (framing declarations): []
  - 断言:1 条
- `h-te6zsh` [proposed] rms 单进程绑定 15000–15006 时,15003/15004/15005/15006 的请求分别落到 gcr.io/ghcr.io/registry.k8s.io/quay.io 这几个 namespace。
  - 推翻条件:某个端口的拉取失败,或内容被写进非预期的 namespace 目录。
  - scope: {"conditions":{"上游":"registry-1.docker.io,ghcr.io,gcr.io,quay.io,registry.k8s.io,docker.m.daocloud.io","主机":"192.168.188.103","端口":"5000/15000-15006/9100","部署方式":"docker compose"},"ranges":{}}
  - uses (framing declarations): []
  - 断言:5 条
- `h-nzvcx2` [proposed] 在 103 上 rms 不配 HTTP 代理时,五个官方上游 namespace 均可代理成功并拉到镜像。
  - 推翻条件:/readyz 报告某 namespace 降级,或 rms_namespace_up 有取值为 0。
  - scope: {"conditions":{"上游":"registry-1.docker.io,ghcr.io,gcr.io,quay.io,registry.k8s.io,docker.m.daocloud.io","主机":"192.168.188.103","端口":"5000/15000-15006/9100","部署方式":"docker compose","代理":"未配置"},"ranges":{}}
  - uses (framing declarations): []
  - 断言:5 条

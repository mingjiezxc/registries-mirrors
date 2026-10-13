# 103 缓存迁移与旧容器摘除

日期:2026-10-12 · 备份文件:`/opt/stacks/registry/docker-compose.yml.bak-20261012-141308`

## 1. 迁移前

```
$ du -sb /opt/stacks/registry/data/*
  /opt/stacks/registry/data/docker    44083847056    (44.08 GB)
  /opt/stacks/registry/data/gcr                 2
  /opt/stacks/registry/data/ghcr        8710587686    ( 8.71 GB)
  /opt/stacks/registry/data/k8s                 2
  /opt/stacks/registry/data/quay            25580
  /opt/stacks/registry/data/registry   1325737520    (15008 私有仓库,未动)

文件数  docker 5291 · ghcr 476 · gcr 0 · k8s 0 · quay 25
旧容器  reg-daocloud reg-1ms reg-docker reg-quay reg-k8s reg-ghcr reg-gcr
```

## 2. 摘除旧镜像容器

`/opt/stacks/registry/docker-compose.yml` 先整体备份,然后改写为**只保留 15008 的 `registry`** service(`networks` 段原样),再:

```
$ docker compose up -d --remove-orphans
 Container reg-k8s  Removed
 Container reg-quay Removed
 Container registry Running        # 15008 未受影响

剩余:registry Up 8 days 0.0.0.0:15008->5000/tcp
15000-15006 监听数:0                # 端口已全部让出
```

回滚:把备份文件复制回 `docker-compose.yml` 再 `docker compose up -d`。

## 3. 缓存迁移(同文件系统改名,不复制)

```
mv /opt/stacks/registry/data/docker -> /opt/stacks/rms/data/docker.io
mv /opt/stacks/registry/data/ghcr   -> /opt/stacks/rms/data/ghcr.io
mv /opt/stacks/registry/data/gcr    -> /opt/stacks/rms/data/gcr.io
mv /opt/stacks/registry/data/k8s    -> /opt/stacks/rms/data/registry.k8s.io
mv /opt/stacks/registry/data/quay   -> /opt/stacks/rms/data/quay.io
chown -R 65532:65532 /opt/stacks/rms/data
```

## 4. 迁移后核对(逐项对比第 1 节)

```
$ du -sb /opt/stacks/rms/data/*
  /opt/stacks/rms/data/docker.io         44083847056   ← 与迁移前逐字节相同
  /opt/stacks/rms/data/gcr.io                       2
  /opt/stacks/rms/data/ghcr.io            8710587686   ← 同上
  /opt/stacks/rms/data/quay.io                  25580
  /opt/stacks/rms/data/registry.k8s.io              2

文件数  docker.io 5291 · ghcr.io 476 · gcr.io 0 · registry.k8s.io 0 · quay.io 25

$ ls /opt/stacks/registry/data/
registry                                   # 只剩 15008 的数据
$ ls -ld /opt/stacks/rms/data/docker.io
drwxr-xr-x 3 65532 65532 4  3月 10  2026   # 属主已改为容器用户
```

迁移前旧的 `scheduler-state.json`(registry:2 的 TTL 调度器状态)随目录一起进来;rms 的 proxy namespace 配的是 `ttl: 0`,不安装调度器,该文件不会被读取。

**此刻的状态**:15000–15006 无人监听(旧容器已摘、rms 尚未接管)、缓存已就位。下一步 `docker compose up -d` 让 rms 接管这些端口。

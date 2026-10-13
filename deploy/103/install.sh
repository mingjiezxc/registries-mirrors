#!/usr/bin/env bash
#
# Sync this repository to 192.168.188.103:/opt/stacks/rms, place the
# 103-specific files, and build the image there.
#
# It deliberately does NOT start or stop anything: moving the mirror ports off
# the old registry:2 stack is a separate, deliberate step (see README.md and
# reports/103-rms-deploy.md), so this can be run while the old stack still
# serves clients.
#
#   deploy/103/install.sh              # sync + place + build
#   REMOTE=root@other DEST=/srv/rms deploy/103/install.sh
#
set -euo pipefail

REMOTE=${REMOTE:-root@192.168.188.103}
DEST=${DEST:-/opt/stacks/rms}
IMAGE=${RMS_IMAGE:-rms:dev}

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(cd "$HERE/../.." && pwd)

command -v rsync >/dev/null || { echo "install.sh: rsync is required" >&2; exit 1; }

echo "==> sync $ROOT -> $REMOTE:$DEST"
ssh "$REMOTE" "mkdir -p '$DEST'"
# Every directory pattern is anchored with a leading slash. Unanchored, "data"
# also strips vendor/golang.org/x/net/publicsuffix/data/ and the Go build then
# fails with "pattern data/children: no matching files found" — the same trap
# lives in .dockerignore.
rsync -a --delete \
  --exclude '/.git/' \
  --exclude '/.gocache/' \
  --exclude '/.docker/' \
  --exclude '/.probe/' \
  --exclude '/data/' \
  --exclude '/bin/' \
  --exclude '/reports/' \
  --exclude '/.env' \
  --exclude '/coverage.out' \
  --exclude '/*.log' \
  "$ROOT"/ "$REMOTE:$DEST/"

echo "==> place the 103-specific compose/config/env"
scp -q "$HERE/docker-compose.yml" "$REMOTE:$DEST/docker-compose.yml"
scp -q "$HERE/rms.yaml"           "$REMOTE:$DEST/config/rms.yaml"
scp -q "$HERE/.env"               "$REMOTE:$DEST/.env"

# The container runs as uid:gid 65532, so the bind mounts must be readable (and
# the cache directory writable) by it. This mirrors `make prepare`.
echo "==> prepare directories and permissions for uid 65532"
ssh "$REMOTE" "
  set -e
  mkdir -p '$DEST/data'
  chown -R 65532:65532 '$DEST/data'
  chmod 0644 '$DEST/config/rms.yaml'
  chgrp 65532 '$DEST/config/htpasswd' && chmod 0640 '$DEST/config/htpasswd'
  ls -ld '$DEST/data'; ls -l '$DEST/config/rms.yaml' '$DEST/config/htpasswd'
"

echo "==> build the image on $REMOTE (multi-stage Dockerfile; deps are vendored)"
ssh "$REMOTE" "cd '$DEST' && docker compose build"

echo "==> smoke: image runs"
ssh "$REMOTE" "docker run --rm '$IMAGE' version"

echo
echo "done. The old mirror containers are untouched."
echo "next: reports/103-rms-deploy.md — 摘除旧容器 → mv 缓存 → docker compose up -d"

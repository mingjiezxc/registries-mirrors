#!/usr/bin/env bash
#
# Place the 103-specific deployment files on 192.168.188.103 and update the
# running stack to the image published as rms.majorc.eu.org/me/rms.
#
# 103 is a *pull-only* host: this script never copies the repository there and
# never builds there. The image is built on the dev host by `make push` (see the
# Makefile) and only pulled here. DEST therefore holds exactly four things:
#
#   docker-compose.yml  .env  config/  data/
#
#   deploy/103/install.sh              # place files (backed up) + pull + restart
#   deploy/103/install.sh --no-place   # only pull + restart + prune
#   deploy/103/install.sh --force      # recreate the container even if nothing changed
#   REMOTE=root@other DEST=/srv/rms deploy/103/install.sh
#
# Order of operations:
#   1. place docker-compose.yml, .env and config/rms.yaml (each backed up first
#      when it changes). config/htpasswd is NEVER overwritten: it holds the live
#      credentials, and the repository copy is the demo one.
#   2. docker login on REMOTE, when RMS_REGISTRY_PASSWORD is set. Since
#      2026-10-14 rms.yaml pins auth.type: none, so the pull below needs no
#      credentials at all; the step is kept so the same flow still works after a
#      rollback to htpasswd (and it cannot fail while auth is off - /v2/ answers
#      200 to any credentials, so docker login reports success).
#   3. docker compose pull --policy always
#      (the compose file deliberately has no pull_policy: always - rms serves
#      the registry it pulls from, so an unconditional pull during `up` could
#      deadlock a stopped stack; see the comment in docker-compose.yml)
#   4. docker compose up -d, then wait for the container to be healthy
#   5. prune: images the update left behind, dangling layers, the old
#      build-on-103 tags (rms:dev, rms:race-*, rms:rollback-*), and any source
#      tree a previous install run had rsynced into DEST.
#
set -euo pipefail

REMOTE=${REMOTE:-root@192.168.188.103}
DEST=${DEST:-/opt/stacks/rms}
REGISTRY=${REGISTRY:-rms.majorc.eu.org}
RMS_REGISTRY_USER=${RMS_REGISTRY_USER:-admin}

PLACE=1
FORCE=0
for arg in "$@"; do
	case "$arg" in
		--no-place) PLACE=0 ;;
		--force) FORCE=1 ;;
		-h | --help)
			sed -n '2,33p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
			exit 0
			;;
		*)
			echo "install.sh: unknown argument: $arg" >&2
			exit 2
			;;
	esac
done

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(cd "$HERE/../.." && pwd)

command -v ssh >/dev/null || { echo "install.sh: ssh is required" >&2; exit 1; }
command -v scp >/dev/null || { echo "install.sh: scp is required" >&2; exit 1; }

# Source paths the pre-2026-10-13 flow rsynced into DEST. They are deleted so
# that DEST cannot silently drift back into a build host.
SOURCE_PATHS=(
	bin clear cmd internal vendor test scripts deploy reports
	Dockerfile Dockerfile.binary go.mod go.sum Makefile README.md
	.dockerignore .gitignore .env.example .gocache .docker .docker-anon
	rms.bin rms.tar.gz coverage.out
)

if [ "$PLACE" = 1 ]; then
	echo "==> stage deployment files for $REMOTE:$DEST"
	ssh "$REMOTE" "mkdir -p '$DEST/config'"
	scp -q -p "$HERE/docker-compose.yml" "$REMOTE:$DEST/docker-compose.yml.new"
	scp -q -p "$HERE/.env" "$REMOTE:$DEST/.env.new"
	scp -q -p "$HERE/rms.yaml" "$REMOTE:$DEST/config/rms.yaml.new"

	HT_CHANGED=0
	if ! ssh "$REMOTE" "test -f '$DEST/config/htpasswd'"; then
		echo "==> no htpasswd on $REMOTE: placing the repository copy (demo credentials!)"
		scp -q "$ROOT/config/htpasswd" "$REMOTE:$DEST/config/htpasswd.new"
		ssh "$REMOTE" "mv '$DEST/config/htpasswd.new' '$DEST/config/htpasswd'"
		HT_CHANGED=1
	fi

	echo "==> install them (backing up whatever they replace)"
	# The last line reports whether anything really changed: a container only
	# picks up a new .env / compose file / rms.yaml at create time, and compose
	# does not watch mounted files, so the update step below has to know.
	place_out=$(ssh "$REMOTE" bash -s -- "$DEST" <<'REMOTE_SCRIPT'
		set -euo pipefail
		DEST=$1
		cd "$DEST"
		ts=$(date +%Y%m%d-%H%M%S)
		changed=0
		for f in docker-compose.yml .env config/rms.yaml; do
			[ -f "$f.new" ] || { echo "install: missing $f.new" >&2; exit 1; }
			if [ -f "$f" ] && cmp -s "$f" "$f.new"; then
				mv "$f.new" "$f"
				echo "    unchanged $f"
				continue
			fi
			if [ -f "$f" ]; then
				cp -p "$f" "$f.bak-$ts"
				echo "    backed up $f -> $f.bak-$ts"
			fi
			mv "$f.new" "$f"
			echo "    placed $f"
			changed=1
		done
		echo "CHANGED=$changed"
REMOTE_SCRIPT
	)
	CHANGED=$(printf '%s\n' "$place_out" | sed -n 's/^CHANGED=//p' | tail -1)
	printf '%s\n' "$place_out" | grep -v '^CHANGED=' || true
	if [ "$HT_CHANGED" = 1 ]; then CHANGED=1; fi
else
	echo "==> --no-place: keeping the files already on $REMOTE:$DEST"
fi

echo "==> prepare directories and permissions for uid 65532"
ssh "$REMOTE" bash -s -- "$DEST" <<'REMOTE_SCRIPT'
	set -euo pipefail
	DEST=$1
	cd "$DEST"
	mkdir -p data
	chown -R 65532:65532 data
	if [ -f config/rms.yaml ]; then chmod 0644 config/rms.yaml; fi
	if [ -f config/htpasswd ]; then
		chgrp 65532 config/htpasswd && chmod 0640 config/htpasswd
	fi
	ls -ld data; ls -l config/rms.yaml config/htpasswd
REMOTE_SCRIPT

if [ -n "${RMS_REGISTRY_PASSWORD:-}" ]; then
	echo "==> docker login $REGISTRY on $REMOTE (as $RMS_REGISTRY_USER)"
	printf '%s' "$RMS_REGISTRY_PASSWORD" |
		ssh "$REMOTE" "docker login '$REGISTRY' -u '$RMS_REGISTRY_USER' --password-stdin" >/dev/null
else
	echo "==> no RMS_REGISTRY_PASSWORD given: relying on an existing docker login on $REMOTE (not needed while auth.type is none)"
fi

echo "==> pull + restart on $REMOTE"
ssh "$REMOTE" bash -s -- "$DEST" "$CHANGED" "$FORCE" <<'REMOTE_SCRIPT'
	set -euo pipefail
	DEST=$1; CHANGED=$2; FORCE=$3
	cd "$DEST"

	# The old container is still serving the registry during the pull, so this
	# one is allowed to reach itself. A failure is not fatal: the locally cached
	# image is still there and `up -d` can rebuild from it.
	if ! docker compose pull --policy always; then
		echo "!! pull failed - continuing with the locally cached image" >&2
	fi

	if [ "$CHANGED" = 1 ] || [ "$FORCE" = 1 ]; then
		echo "    recreating the container"
		docker compose up -d --force-recreate
	else
		docker compose up -d
	fi

	echo "==> waiting for the container to become healthy"
	state=none
	for _ in $(seq 1 40); do
		state=$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' rms 2>/dev/null || echo none)
		[ "$state" = healthy ] && break
		sleep 3
	done
	echo "    container state: $state"
	docker compose ps
	curl -s -o /dev/null -w '    /v2/ -> %{http_code} (200 = auth.type none, 401 = htpasswd challenge; either way the server is up)\n' http://127.0.0.1:5000/v2/ || true
	[ "$state" = healthy ] || { echo "!! container is not healthy" >&2; exit 1; }
REMOTE_SCRIPT

echo "==> prune old images and the leftover source tree on $REMOTE"
ssh "$REMOTE" bash -s -- "$DEST" "${SOURCE_PATHS[@]}" <<'REMOTE_SCRIPT'
	set -euo pipefail
	DEST=$1
	shift
	cd "$DEST"

	# Guard: only ever touch a directory that really is this compose stack.
	[ -f docker-compose.yml ] || { echo "prune: $DEST has no docker-compose.yml, refusing" >&2; exit 1; }
	grep -q 'rms' docker-compose.yml || { echo "prune: $DEST/docker-compose.yml does not look like the rms stack, refusing" >&2; exit 1; }

	# The previous :latest is untagged by the update above, so it shows up as a
	# dangling image. Note this prunes *every* dangling image on the host, not
	# only rms's - on a stack-heavy 103 that is where most of the space is (the
	# first run of this script reclaimed ~26 GiB of images plus the build cache).
	docker image prune -f

	# Tags from the era when 103 built the image itself.
	for img in $(docker images --format '{{.Repository}}:{{.Tag}}' | grep -E '^rms:' || true); do
		if docker image rm "$img" >/dev/null 2>&1; then
			echo "    removed image $img"
		else
			echo "    kept image $img (still referenced)"
		fi
	done

	# 103 no longer builds, so its build cache is dead weight (rpool was ~87% full).
	docker builder prune -f || true

	for p in "$@"; do
		if [ -e "$p" ]; then
			rm -rf -- "$p"
			echo "    removed source path $p"
		fi
	done
	rm -f -- ./*.log

	echo "==> $DEST now contains:"
	ls -A
REMOTE_SCRIPT

echo
echo "done. 103 runs ${REGISTRY}/me/rms (pull-only); source and old images are gone."

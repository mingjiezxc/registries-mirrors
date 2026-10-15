#!/usr/bin/env bash
#
# Ship an already-built rms image to 10.82.64.172:/app/rms and restart the stack
# there. 172 does not build and does not pull from a registry: this script
# `docker save`s the image here, copies it with scp, and `docker load`s it on the
# other side (the host is only reachable from 101 through the SASE tunnel, and
# the company's registry:2 stack has been retired and rms took over its port).
#
#   IMAGE=rms.majorc.eu.org/me/rms:v1 deploy/172/install.sh
#   deploy/172/install.sh --no-place     # save/scp/load + restart, keep remote files
#   deploy/172/install.sh --prune-all    # also `docker image prune -f` on the target
#   deploy/172/install.sh --force        # recreate the container even if nothing changed
#
# Makefile entry point: `make deploy-172` (builds, then calls this).
#
# Environment:
#   REMOTE=ops@10.82.64.172   DEST=/app/rms   IMAGE=<ref>  (required)
#   RMS_HTPASSWD_USER=admin   RMS_HTPASSWD_PASSWORD=<pw>
#       When a password is given, config/htpasswd is regenerated here (locally,
#       with the binary from the image, password on stdin) and installed on the
#       target; the file then contains that single user. Empty = leave the
#       target's htpasswd alone.
#
# The target needs passwordless sudo: `ops` is not in the docker group and
# /app/rms is owned by root. Everything privileged runs as `sudo -n`.
#
set -euo pipefail

REMOTE=${REMOTE:-ops@10.82.64.172}
DEST=${DEST:-/app/rms}
IMAGE=${IMAGE:-}
RMS_HTPASSWD_USER=${RMS_HTPASSWD_USER:-admin}
RMS_HTPASSWD_PASSWORD=${RMS_HTPASSWD_PASSWORD:-}
# Host port the target publishes the content endpoint on (see deploy/172/.env).
# The caller's RMS_PORT_PRIMARY wins; otherwise the value pinned in .env is used
# (resolved after HERE below). Only the verification at the end needs it.
PORT=${RMS_PORT_PRIMARY:-}
REMOTE_HOST=${REMOTE#*@}

PLACE=1
PRUNE_ALL=0
FORCE=0
for arg in "$@"; do
	case "$arg" in
		--no-place) PLACE=0 ;;
		--prune-all) PRUNE_ALL=1 ;;
		--force) FORCE=1 ;;
		-h | --help)
			sed -n '2,26p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
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

if [ -z "$PORT" ]; then
	PORT=$(sed -n 's/^RMS_PORT_PRIMARY=//p' "$HERE/.env" | head -1)
fi
PORT=${PORT:-5003}

[ -n "$IMAGE" ] || { echo "install.sh: IMAGE=<ref> is required (e.g. make deploy-172)" >&2; exit 1; }
command -v ssh >/dev/null || { echo "install.sh: ssh is required" >&2; exit 1; }
command -v scp >/dev/null || { echo "install.sh: scp is required" >&2; exit 1; }
docker image inspect "$IMAGE" >/dev/null 2>&1 \
	|| { echo "install.sh: $IMAGE is not present on this host; build it first (make image / make deploy-172)" >&2; exit 1; }

STAGE=/tmp/rms-deploy-$(date +%Y%m%d-%H%M%S)-$$
TAR=$(echo "$IMAGE" | tr ':/' '__')
TAR="$ROOT/dist/$TAR.tar"

# Staged under the repository, not /tmp: the docker daemon bind-mounts this
# directory for the htpasswd helper below, and a sandboxed /tmp may not be the
# path the daemon resolves.
LOCAL="$ROOT/dist/.stage-$$"
mkdir -p "$LOCAL"
trap 'rm -rf "$LOCAL"' EXIT

echo "==> docker save $IMAGE"
mkdir -p "$ROOT/dist"
docker save -o "$TAR" "$IMAGE"
ls -lh "$TAR" | awk '{print "    " $5 " " $9}'

# The staged .env pins the image tag that is about to be loaded on the target.
sed "s|^RMS_IMAGE=.*|RMS_IMAGE=$IMAGE|" "$HERE/.env" > "$LOCAL/.env"

HAS_HTPASSWD=0
if [ -n "$RMS_HTPASSWD_PASSWORD" ]; then
	echo "==> generate htpasswd for '$RMS_HTPASSWD_USER' (password on stdin, never in argv)"
	# Run the binary out of the image, so the hash is produced by rms itself and
	# stays compatible. The container runs as this caller, so the file lands
	# owned by us and can be shipped as-is.
	printf '%s' "$RMS_HTPASSWD_PASSWORD" |
		docker run --rm -i --user "$(id -u):$(id -g)" -v "$LOCAL":/out \
			--entrypoint /usr/local/bin/rms "$IMAGE" \
			htpasswd add --file /out/htpasswd --user "$RMS_HTPASSWD_USER" >/dev/null
	[ -s "$LOCAL/htpasswd" ] || { echo "install.sh: htpasswd generation produced nothing" >&2; exit 1; }
	echo "    $(cut -d: -f1 "$LOCAL/htpasswd" | tr '\n' ' ')"
	HAS_HTPASSWD=1
fi

echo "==> scp image + files to $REMOTE:$STAGE"
ssh "$REMOTE" "mkdir -p '$STAGE'"
scp -q -p "$TAR" "$REMOTE:$STAGE/rms-image.tar"
if [ "$PLACE" = 1 ]; then
	scp -q -p "$HERE/compose.yaml" "$LOCAL/.env" "$HERE/rms.yaml" "$REMOTE:$STAGE/"
	if [ "$HAS_HTPASSWD" = 1 ]; then
		scp -q -p "$LOCAL/htpasswd" "$REMOTE:$STAGE/htpasswd"
	fi
fi

echo "==> load + install + restart on $REMOTE (sudo -n, runs as root)"
ssh "$REMOTE" sudo -n bash -s -- "$DEST" "$IMAGE" "$STAGE" "$PLACE" "$HAS_HTPASSWD" "$PRUNE_ALL" "$FORCE" <<'REMOTE_SCRIPT'
	set -euo pipefail
	DEST=$1; IMAGE=$2; STAGE=$3; PLACE=$4; HAS_HTPASSWD=$5; PRUNE_ALL=$6; FORCE=$7
	ts=$(date +%Y%m%d-%H%M%S)

	echo "==> docker load"
	docker load -i "$STAGE/rms-image.tar"

	# The tag the target is running now, remembered so it can be pruned once the
	# new one is up.
	prev_image=""
	if [ -f "$DEST/.env" ]; then
		prev_image=$(sed -n 's/^RMS_IMAGE=//p' "$DEST/.env" | head -1)
	fi
	if [ -n "$prev_image" ]; then echo "    currently pinned: $prev_image"; fi

	# place <src> <dst> <mode>; sets `changed` when it really replaced something,
	# because a container only picks up a new rms.yaml / .env / compose.yaml at
	# create time (compose does not watch mounted files).
	changed=0
	place() {
		src=$1; dst=$2; mode=$3
		mkdir -p "$(dirname "$dst")"
		if [ -f "$dst" ] && cmp -s "$src" "$dst"; then
			echo "    unchanged $dst"
			return 0
		fi
		if [ -f "$dst" ]; then
			cp -p "$dst" "$dst.bak-$ts"
			echo "    backed up $dst -> $dst.bak-$ts"
		fi
		cp "$src" "$dst"
		chmod "$mode" "$dst"
		echo "    placed $dst"
		changed=1
	}

	if [ "$PLACE" = 1 ]; then
		echo "==> install files into $DEST"
		place "$STAGE/compose.yaml" "$DEST/compose.yaml" 0644
		place "$STAGE/.env" "$DEST/.env" 0600
		place "$STAGE/rms.yaml" "$DEST/config/rms.yaml" 0644
		if [ "$HAS_HTPASSWD" = 1 ]; then
			place "$STAGE/htpasswd" "$DEST/config/htpasswd" 0600
		fi
	else
		echo "==> --no-place: keeping the files already in $DEST"
	fi

	echo "==> permissions (container runs as uid 65532)"
	mkdir -p "$DEST/data"
	chown -R 65532:65532 "$DEST/data"
	if [ -f "$DEST/config/rms.yaml" ]; then chmod 0644 "$DEST/config/rms.yaml"; fi
	if [ -f "$DEST/config/htpasswd" ]; then
		chown root:65532 "$DEST/config/htpasswd"
		chmod 0640 "$DEST/config/htpasswd"
	fi
	ls -ld "$DEST" "$DEST/data" "$DEST/config"; ls -l "$DEST/config/"

	if [ "$changed" = 1 ] || [ "$FORCE" = 1 ]; then
		echo "==> docker compose up -d --force-recreate"
	else
		echo "==> docker compose up -d"
	fi
	cd "$DEST"
	if [ "$changed" = 1 ] || [ "$FORCE" = 1 ]; then
		docker compose up -d --force-recreate
	else
		docker compose up -d
	fi

	port=$(sed -n 's/^RMS_PORT_PRIMARY=//p' "$DEST/.env" | head -1)
	port=${port:-5003}
	echo "==> waiting for the container to become healthy"
	state=none
	for _ in $(seq 1 40); do
		state=$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' rms 2>/dev/null || echo none)
		[ "$state" = healthy ] && break
		sleep 3
	done
	echo "    container state: $state"
	docker compose ps
	curl -s -o /dev/null -w "    http://127.0.0.1:$port/v2/ -> %{http_code} (401 = htpasswd challenge, 200 = auth disabled)\n" "http://127.0.0.1:$port/v2/" || true

	echo "==> prune the image this update replaced + the old build-era tags"
	for img in "$prev_image" $(docker images --format '{{.Repository}}:{{.Tag}}' | grep -E '^rms:' || true); do
		[ -n "$img" ] || continue
		[ "$img" = "$IMAGE" ] && continue
		if docker image rm "$img" >/dev/null 2>&1; then
			echo "    removed image $img"
		else
			echo "    kept image $img (in use or already gone)"
		fi
	done
	if [ "$PRUNE_ALL" = 1 ]; then
		docker image prune -f
	fi

	rm -rf "$STAGE"
	echo "==> $DEST now contains:"
	ls -A "$DEST"

	[ "$state" = healthy ] || { echo "!! container is not healthy" >&2; exit 1; }
REMOTE_SCRIPT

echo "==> verify from here (through the SASE link)"
code=$(curl -s -o /dev/null -m 15 -w '%{http_code}' "http://$REMOTE_HOST:$PORT/v2/" || echo 000)
echo "    anonymous /v2/ -> $code (401 = htpasswd challenge, 200 = auth.type none)"
if [ -n "$RMS_HTPASSWD_PASSWORD" ]; then
	code=$(curl -s -o /dev/null -m 15 -u "$RMS_HTPASSWD_USER:$RMS_HTPASSWD_PASSWORD" -w '%{http_code}' \
		"http://$REMOTE_HOST:$PORT/v2/" || echo 000)
	echo "    $RMS_HTPASSWD_USER /v2/ -> $code (200 expected)"
	[ "$code" = 200 ] || { echo "!! credentials do not work against the deployed instance" >&2; exit 1; }
fi

echo
echo "done. $REMOTE runs $IMAGE (saved locally as $TAR)"

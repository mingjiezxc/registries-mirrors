#!/usr/bin/env bash
# Live acceptance for the upstream race, run on 192.168.188.101.
#
# It starts a *separate* rms process (ports 15100/19100, its own storage root)
# plus three fake mirrors on loopback, and drives it with curl. Nothing here
# touches the shared deployment on 192.168.188.103 — that host is used only as a
# read-only upstream, which is also what makes the digest gate observable: it is
# the authority mirror (first in configuration order) and holds a different
# manifest than the faster fake.
#
# usage: scripts/live-acceptance/run.sh [workdir]
set -u

REPO="$(cd "$(dirname "$0")/../.." && pwd)"
WORK="${1:-$REPO/.probe/live}"
RMS_PORT=15100
METRICS_PORT=19100
STALE_PORT=15101
SLOW_PORT=15102
FAST_PORT=15103
AUTH_PORT=15104
AUTH="http://127.0.0.1:$RMS_PORT"

rm -rf "$WORK"
mkdir -p "$WORK/data"

say() { printf '\n=== %s ===\n' "$1"; }
show() { printf '%-72s -> %s\n' "$1" "$2"; }

cleanup() {
  [[ -n "${RMS_PID:-}" ]] && kill "$RMS_PID" 2>/dev/null
  for pid in ${FAKE_PIDS:-}; do kill "$pid" 2>/dev/null; done
  wait 2>/dev/null
}
trap cleanup EXIT

# --- fixtures ---------------------------------------------------------------
"$REPO/bin/rms" htpasswd add --file "$WORK/htpasswd" --user me --password mepw >/dev/null
"$REPO/bin/rms" htpasswd add --file "$WORK/htpasswd" --user alice --password alicepw >/dev/null
"$REPO/bin/rms" htpasswd add --file "$WORK/htpasswd" --user admin --password adminpw >/dev/null
chmod 644 "$WORK/htpasswd"

# The stale mirror holds the wrong bytes for a repository the real mirror also
# has; the two example.test mirrors hold the SAME bytes and differ only in
# latency, which is the case load balancing exists for. Each only answers for
# the repositories it owns, so one namespace cannot answer for another's.
python3 "$REPO/scripts/live-acceptance/fake-mirror.py" --port $STALE_PORT --delay-ms 5  --marker stale --repo library/busybox --log "$WORK/stale.log" &
FAKE_PIDS="$!"
python3 "$REPO/scripts/live-acceptance/fake-mirror.py" --port $SLOW_PORT  --delay-ms 25 --marker same --repo app --log "$WORK/slow.log" &
FAKE_PIDS="$FAKE_PIDS $!"
python3 "$REPO/scripts/live-acceptance/fake-mirror.py" --port $FAST_PORT  --delay-ms 20 --marker same --repo app --log "$WORK/fast.log" &
FAKE_PIDS="$FAKE_PIDS $!"
python3 "$REPO/scripts/live-acceptance/fake-mirror.py" --port $AUTH_PORT  --delay-ms 50 --marker authority --repo library/busybox --log "$WORK/authority.log" &
FAKE_PIDS="$FAKE_PIDS $!"

# Wait for every fixture to listen before rms probes them: a mirror that is not
# up yet is marked unreachable at startup and left out of the first races (which
# is the health filter working, but not what this run is measuring).
for port in $STALE_PORT $SLOW_PORT $FAST_PORT $AUTH_PORT; do
  for _ in $(seq 1 40); do
    if curl -s -o /dev/null --noproxy '*' "http://127.0.0.1:$port/v2/"; then break; fi
    sleep 0.25
  done
done

cat > "$WORK/rms.yaml" <<EOF
server:
  listen: "127.0.0.1:$RMS_PORT"
  http_secret: "acceptance-secret"
storage:
  root: $WORK/data
auth:
  type: htpasswd
  htpasswd: $WORK/htpasswd
  realm: rms
  user_namespace: true
  shared_namespaces: [shared]
  anonymous_pull_proxy: true
  anonymous_pull_local: false
  admin_users: [admin]
namespaces:
  # me is the personal namespace of the htpasswd user "me" (user_namespace).
  # Listing it here would make it shared, i.e. writable by every logged-in
  # user — which is exactly the mistake this run checks for.
  local: [shared]
  selection:
    probe_ttl: 30s
    probe_timeout: 15s
    decision_deadline: 5s
    balance: weighted
    balance_window: 1.5
  proxy:
    docker.io:
      # Configuration order is digest-authority order. The first mirror here
      # answers with the marker "authority"; the real rms on 103 answers the same
      # tag with the real digest, and the fast fake answers with yet another
      # digest. Only the authority may serve.
      upstreams: ["http://127.0.0.1:$AUTH_PORT", "http://192.168.188.103:5000", "http://127.0.0.1:$STALE_PORT"]
      official_library: true
      ttl: 0
    example.test:
      upstreams: ["http://127.0.0.1:$SLOW_PORT", "http://127.0.0.1:$FAST_PORT"]
      ttl: 0
upstream:
  health:
    enabled: true
    interval: 10s
    timeout: 10s
  http:
    proxy: ""
    no_proxy: "127.0.0.1,localhost,192.168.188.103"
    dial_timeout: 10s
    response_header_timeout: 60s
observability:
  logs:
    level: info
    format: json
  traces:
    enabled: false
  metrics:
    listen: "127.0.0.1:$METRICS_PORT"
EOF

"$REPO/bin/rms" serve --config "$WORK/rms.yaml" > "$WORK/rms.log" 2>&1 &
RMS_PID=$!

for _ in $(seq 1 60); do
  code=$(curl -s -o /dev/null -w '%{http_code}' --noproxy '*' "$AUTH/v2/" || true)
  [[ "$code" == "401" ]] && break
  sleep 0.5
done

CURL=(curl -s --noproxy '*' -o /dev/null -w '%{http_code}')
CURLB=(curl -s --noproxy '*' -D - -o /dev/null)
MANIFEST_ACCEPT="Accept: application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json"

echo "date: $(date -Is)"
echo "host: $(hostname) $(hostname -I 2>/dev/null | awk '{print $1}')"
echo "rms $("$REPO/bin/rms" version), workdir $WORK"

# --- A. auth posture --------------------------------------------------------
say "A. pull anonymous, only push to a local namespace needs credentials"

show "anonymous GET /v2/ (challenge must stay)" \
  "$(curl -s --noproxy '*' -D - -o /dev/null "$AUTH/v2/" | head -1 | tr -d '\r')"
show "anonymous GET /v2/me/app/manifests/latest (local read)" \
  "$("${CURL[@]}" "$AUTH/v2/me/app/manifests/latest")"
show "anonymous POST /v2/me/app/blobs/uploads/ (local write)" \
  "$("${CURL[@]}" -X POST "$AUTH/v2/me/app/blobs/uploads/")"
show "me:mepw POST /v2/me/app/blobs/uploads/" \
  "$("${CURL[@]}" -u me:mepw -X POST "$AUTH/v2/me/app/blobs/uploads/")"
show "alice:alicepw POST /v2/me/app/blobs/uploads/ (someone else's namespace)" \
  "$("${CURL[@]}" -u alice:alicepw -X POST "$AUTH/v2/me/app/blobs/uploads/")"
show "me:mepw POST /v2/docker.io/library/app/blobs/uploads/ (proxy namespace is read-only)" \
  "$("${CURL[@]}" -u me:mepw -X POST "$AUTH/v2/docker.io/library/app/blobs/uploads/")"
show "anonymous POST /v2/shared/app/blobs/uploads/ (shared still needs a login)" \
  "$("${CURL[@]}" -X POST "$AUTH/v2/shared/app/blobs/uploads/")"
show "alice:alicepw POST /v2/shared/app/blobs/uploads/" \
  "$("${CURL[@]}" -u alice:alicepw -X POST "$AUTH/v2/shared/app/blobs/uploads/")"

# --- B. digest gate ---------------------------------------------------------
say "B. digest authority: the stale (faster) mirror must never be served"

# Pick a repository the real mirror on 103 can serve quickly.
REAL_REPO="library/busybox"
REAL_REF="1.36.1"
for cand in "library/busybox:1.36.1" "library/alpine:latest" "library/hello-world:latest"; do
  repo="${cand%%:*}"; ref="${cand##*:}"
  code=$(curl -s --noproxy '*' -o /dev/null -w '%{http_code}' -H "$MANIFEST_ACCEPT" \
    "http://192.168.188.103:5000/v2/$repo/manifests/$ref")
  echo "  candidate $repo:$ref from 103 -> $code"
  if [[ "$code" == "200" ]]; then REAL_REPO="$repo"; REAL_REF="$ref"; break; fi
done

BODY=$(curl -s --noproxy '*' -H "$MANIFEST_ACCEPT" "$AUTH/v2/$REAL_REPO/manifests/$REAL_REF")
show "anonymous GET /v2/$REAL_REPO/manifests/$REAL_REF (unqualified, raced)" \
  "$(printf '%s' "$BODY" | head -c 60)..."
MARKER=$(printf '%s' "$BODY" | python3 -c 'import json,sys
try: print(json.load(sys.stdin)["annotations"]["org.rms.acceptance.marker"])
except Exception: print("?")' 2>/dev/null)
show "marker of the served manifest" "$MARKER (want authority: the config-first mirror)"
show "authority.log content fetches" "$(grep -c manifests "$WORK/authority.log")"
show "stale.log content fetches" "$(grep -c manifests "$WORK/stale.log")"

say "B2. what the fake mirror saw (probe only, no content fetch)"
echo "  stale.log: $(wc -l < "$WORK/stale.log") request(s)"
sed 's/^/    /' "$WORK/stale.log"

say "B3. decision line (winner + per-upstream timings)"
grep -o '"msg":"selection".*' "$WORK/rms.log" | tail -2

say "B4. digest-mismatch counters (the real mirror on 103 disagrees with the authority too)"
curl -s --noproxy '*' "http://127.0.0.1:$METRICS_PORT/metrics" | grep '^rms_upstream_digest_mismatch_total' || echo "  (none)"

# --- C. fastest wins and load balancing -------------------------------------
say "C. two mirrors that agree on the digest: fastest serves, then both share"

code=$(curl -s --noproxy '*' -o /dev/null -w '%{http_code}' -H "$MANIFEST_ACCEPT" "$AUTH/v2/app/manifests/latest")
show "anonymous GET /v2/app/manifests/latest (only example.test has it)" "$code"
echo "  first request's upstream fetches: slow(25ms)=$(grep -c manifests "$WORK/slow.log") fast(20ms)=$(grep -c manifests "$WORK/fast.log")"

for i in $(seq 1 12); do
  curl -s --noproxy '*' -o /dev/null -H "$MANIFEST_ACCEPT" "$AUTH/v2/app/manifests/latest"
done
echo "  upstream fetches after 13 pulls: slow(25ms)=$(grep -c manifests "$WORK/slow.log") fast(20ms)=$(grep -c manifests "$WORK/fast.log")"

say "C2. served-by counters (the distribution itself)"
curl -s --noproxy '*' "http://127.0.0.1:$METRICS_PORT/metrics" | grep -E '^rms_upstream_wins_total' | sed 's/^/  /'

say "C3. probe durations"
curl -s --noproxy '*' "http://127.0.0.1:$METRICS_PORT/metrics" | grep -E '^rms_upstream_probe_duration_seconds_(count|sum)' | sed 's/^/  /'

say "C4. decision sources"
curl -s --noproxy '*' "http://127.0.0.1:$METRICS_PORT/metrics" | grep '^rms_selection_decisions_total' | sed 's/^/  /'

say "E. chaining to an auth-enabled upstream (why the credentials above are needed)"
U="http://192.168.188.103:5000/v2/library/busybox/manifests/1.36.1"
ACC="application/vnd.docker.distribution.manifest.v2+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.manifest.v1+json, application/vnd.oci.image.index.v1+json"
show "HEAD <upstream> with no credentials (what the selection probe sends)" \
  "$(curl -s -o /dev/null -w '%{http_code}' --noproxy '*' -I -H "Accept: $ACC" "$U")"
show "HEAD <upstream> with an EMPTY Basic pair (what distribution's proxy attaches)" \
  "$(curl -s -o /dev/null -w '%{http_code}' --noproxy '*' -I -u ':' -H "Accept: $ACC" "$U")"
show "HEAD <upstream> with a wrong pair (refused, never treated as anonymous)" \
  "$(curl -s -o /dev/null -w '%{http_code}' --noproxy '*' -I -u 'wrong:wrong' -H "Accept: $ACC" "$U")"
echo "  -> chaining to this upstream needs its real credentials in username/password;"
echo "     without them the probe (anonymous) says 'present' while the engine's fetch"
echo "     gets 401 and the tag lookup falls back to an empty local cache (404)."

say "D. startup posture lines"
grep -E '"msg":"(namespaces configured|outbound transport configured)"' "$WORK/rms.log" | sed 's/^/  /'

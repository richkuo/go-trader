#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO=$(cd "$SCRIPT_DIR/.." && pwd)

skip() {
    if [[ "${EGRESS_DOCKER_FIXTURE_REQUIRE_RUN:-0}" == "1" ]]; then
        echo "FAIL: EGRESS_DOCKER_FIXTURE_REQUIRE_RUN=1 but this host cannot run the fixture: $*" >&2
        exit 1
    fi
    echo "SKIP: $*"
    exit 0
}
[[ "$(uname -s)" == "Linux" ]] || skip "Linux required"
[[ "$(id -u)" == "0" ]] || skip "root required"
for tool in ip nft docker curl; do
    command -v "$tool" >/dev/null 2>&1 || skip "$tool required"
done
docker compose version >/dev/null 2>&1 || skip "docker compose v2 required"
: "${EGRESS_CONTROLLER_BIN:?set EGRESS_CONTROLLER_BIN to a built go-trader-egress-controller}"
: "${EGRESS_FIXTURE_BIN:?set EGRESS_FIXTURE_BIN to a built egress-fixture}"
: "${EGRESS_TEST_IMAGE:?set EGRESS_TEST_IMAGE to a go-trader image built from this checkout}"
DOCKER_CLI=$(command -v docker)

DEP="dock$RANDOM"
PROJECT="gtegress$DEP"
WORK=$(mktemp -d /tmp/egress-docker.XXXXXX)
chmod 0755 "$WORK"
MANIFEST="/etc/go-trader/egress/$DEP.json"
LIBEXEC="/usr/local/libexec/go-trader-egress/$DEP"
CTL="$LIBEXEC/go-trader-egress-controller"
FIX="$WORK/egress-fixture"
SENTINEL="sntl$(od -An -N10 -tx1 /dev/urandom | tr -d ' \n')"
PROXY_USER="fixtureuser"
OUT_IF="gtd$RANDOM"
STATUS_PORT=$((20000 + RANDOM % 20000))
pids=()
ctl_pid=""
failures=0
checks=0
LAST_OUT=""

compose() {
    docker compose --project-name "$PROJECT" --project-directory "$WORK/docker" -f "$WORK/docker/compose.yaml" -f "$WORK/docker/compose.egress.yaml" "$@"
}

cleanup() {
    set +e
    compose --profile notify down -v --remove-orphans >/dev/null 2>&1
    for p in "${pids[@]}" $ctl_pid; do
        kill "$p" 2>/dev/null
    done
    sleep 0.3
    ip netns delete "go-trader-egress-$DEP" 2>/dev/null
    ip netns delete "go-trader-notify-$DEP" 2>/dev/null
    ip link delete "$OUT_IF" 2>/dev/null
    rm -rf "/run/go-trader-egress/$DEP" "$LIBEXEC" "$MANIFEST" "$WORK"
}
trap cleanup EXIT

pass() {
    checks=$((checks + 1))
    echo "ok: $*"
}

bad() {
    checks=$((checks + 1))
    failures=$((failures + 1))
    echo "FAIL: $*" >&2
}

expect_rc() {
    local want="$1" desc="$2"
    shift 2
    local out rc=0
    out=$("$@" 2>&1) || rc=$?
    printf '%s\n' "$out" >> "$WORK/all-output.out"
    if [[ "$rc" == "$want" ]]; then
        pass "$desc"
    else
        bad "$desc (exit $rc, want $want): $out"
    fi
    LAST_OUT="$out"
}

expect_out() {
    if grep -Eq -- "$1" <<<"$LAST_OUT"; then
        pass "$2"
    else
        bad "$2: output did not match /$1/: $LAST_OUT"
    fi
}

wait_for() {
    local timeout="$1" desc="$2"
    shift 2
    local waited=0
    until "$@" >/dev/null 2>&1; do
        if [[ $waited -ge $timeout ]]; then
            bad "$desc (timed out after ${timeout}s)"
            return 1
        fi
        sleep 1
        waited=$((waited + 1))
    done
    pass "$desc"
}

install -d -o root -g root -m 0755 "$LIBEXEC" /etc/go-trader /etc/go-trader/egress
install -o root -g root -m 0755 "$EGRESS_CONTROLLER_BIN" "$CTL"
install -o root -g root -m 0755 "$EGRESS_FIXTURE_BIN" "$FIX"

ip link add "$OUT_IF" type dummy
ip addr add 10.232.0.1/24 dev "$OUT_IF"
ip addr add 10.232.0.2/24 dev "$OUT_IF"
ip link set "$OUT_IF" up
"$FIX" listen --addr 10.232.0.1:80 --reply venue-ok --log "$WORK/venue.log" &
pids+=($!)
"$FIX" listen --addr 10.232.0.1:9443 --log "$WORK/direct.log" &
pids+=($!)
"$FIX" proxy --listen 10.232.0.2:3128 --scheme http --user "$PROXY_USER" --pass "$SENTINEL" --forward --forward-host 10.232.0.1 --log "$WORK/proxy.log" &
pids+=($!)
sleep 0.5

mkdir -p "$WORK/docker"
cp "$REPO/docker/compose.yaml" "$REPO/docker/compose.egress.yaml" "$WORK/docker/"
IMAGE_REPO="${EGRESS_TEST_IMAGE%:*}"
IMAGE_TAG="${EGRESS_TEST_IMAGE##*:}"
if [[ "$IMAGE_REPO" != "ghcr.io/richkuo/go-trader" ]]; then
    docker tag "$EGRESS_TEST_IMAGE" "ghcr.io/richkuo/go-trader:$IMAGE_TAG"
fi
cat > "$WORK/docker/.env" <<EOF
GO_TRADER_TAG=$IMAGE_TAG
GO_TRADER_EGRESS_DEPLOYMENT=$DEP
EOF
cat > "$WORK/docker/go-trader.env" <<EOF
GO_TRADER_EGRESS_PROXY=http://$PROXY_USER:$SENTINEL@10.232.0.2:3128
STATUS_AUTH_TOKEN=fixture-status-token
EOF
: > "$WORK/docker/notify.env"
chmod 0600 "$WORK/docker/go-trader.env"

cat > "$MANIFEST" <<EOF
{
  "deployment": "$DEP",
  "mode": "docker",
  "proxy": {"scheme": "http", "host": "10.232.0.2", "port": 3128},
  "worker_uid": 10001,
  "worker_gid": 10001,
  "resolve_seconds": 10,
  "status_relays": [{"listen": "127.0.0.1:$STATUS_PORT", "target_port": 8099}],
  "docker": {"cli": "$DOCKER_CLI", "anchor_container": "$PROJECT-egress-net-1"}
}
EOF
chmod 0644 "$MANIFEST"

echo "== controller and anchor"
expect_rc 0 "docker-mode preflight passes" "$CTL" preflight --manifest "$MANIFEST"
"$CTL" serve --manifest "$MANIFEST" > "$WORK/controller.log" 2>&1 &
ctl_pid=$!
expect_rc 0 "the anchor container starts with no network" compose up -d egress-net
wait_for 60 "the controller enforces the anchor namespace" "$CTL" status --manifest "$MANIFEST"
wait_for 60 "the anchor health check sees the enforced namespace" sh -c "docker inspect --format '{{.State.Health.Status}}' $PROJECT-egress-net-1 | grep -qx healthy"

seed_config() {
    docker run --rm --user 10001:10001 -v "${PROJECT}_data:/data" --entrypoint /app/go-trader "ghcr.io/richkuo/go-trader:$IMAGE_TAG" version >/dev/null 2>&1
    cat > "$WORK/config.json" <<EOF
{"config_version": 20, "interval_seconds": 300, "db_file": "/data/state.db", "status_port": 8099, "egress": {"proxy_required": true}, "strategies": []}
EOF
    docker run --rm --user 0:0 -v "${PROJECT}_data:/data" -v "$WORK/config.json:/seed/config.json:ro" --entrypoint /bin/sh "ghcr.io/richkuo/go-trader:$IMAGE_TAG" -c 'cp /seed/config.json /data/config.json && chown 10001:10001 /data/config.json'
}
seed_config

echo "== the application container joins the enforced namespace"
expect_rc 0 "the application container starts" compose up -d go-trader
wait_for 120 "the application answers /health through the host status relay" curl -fsS --max-time 2 "http://127.0.0.1:$STATUS_PORT/health"
LAST_OUT=$(curl -fsS --max-time 5 -H "Authorization: Bearer fixture-status-token" "http://127.0.0.1:$STATUS_PORT/status" || true)
printf '%s\n' "$LAST_OUT" >> "$WORK/all-output.out"
expect_out '"mode": ?"proxy"' "container status reports proxy mode"
expect_out '"enforcement": ?"attested"' "container status reports attested enforcement"

cat > "$WORK/probe.py" <<'PY'
import json, socket
out = {}
for name, addr in [("tcp_direct", ("10.232.0.1", 9443))]:
    try:
        socket.create_connection(addr, timeout=3).close()
        out[name] = "connected"
    except Exception as exc:
        out[name] = "blocked: %s" % type(exc).__name__
try:
    socket.create_connection(("10.232.0.2", 3128), timeout=3).close()
    out["proxy_relay"] = "connected"
except Exception as exc:
    out["proxy_relay"] = "blocked: %s" % type(exc).__name__
print(json.dumps(out, sort_keys=True))
PY
compose exec -T go-trader sh -c 'cat > /tmp/probe.py' < "$WORK/probe.py"
expect_rc 0 "the probe runs inside the application container" compose exec -T go-trader /app/.venv/bin/python3 /tmp/probe.py
expect_out '"tcp_direct": "blocked' "direct TCP from the application container is blocked"
expect_out '"proxy_relay": "connected"' "the proxy address answers only through the controller relay"
cat > "$WORK/through.py" <<'PY'
import requests
print(requests.get("http://venue.egress.test/", timeout=10).text)
PY
compose exec -T go-trader sh -c 'cat > /tmp/through.py' < "$WORK/through.py"
expect_rc 0 "a bootstrapped request leaves through the proxy" compose exec -T go-trader /bin/sh -c 'GO_TRADER_EGRESS_MODE=proxy /app/.venv/bin/python3 /app/shared_tools/egress_bootstrap.py /tmp/through.py'
expect_out 'venue-ok' "the venue answered through the proxy"
if grep -q '"target":"venue.egress.test:80"' "$WORK/proxy.log"; then pass "the proxy recorded the venue host name"; else bad "the proxy recorded nothing: $(cat "$WORK/proxy.log")"; fi

echo "== one-shot commands attest in the namespace"
expect_rc 2 "a one-shot network command passes attestation" compose run --rm -T cli backfill --no-such-flag
if grep -q 'refusing backfill' <<<"$LAST_OUT"; then bad "the one-shot command failed attestation: $LAST_OUT"; else pass "the one-shot command attested"; fi

echo "== anchor restart"
expect_rc 0 "the anchor restarts" docker restart "$PROJECT-egress-net-1"
wait_for 60 "the controller re-enforces the restarted anchor" sh -c "'$CTL' status --manifest '$MANIFEST' && docker inspect --format '{{.State.Health.Status}}' $PROJECT-egress-net-1 | grep -qx healthy"
compose up -d go-trader >/dev/null 2>&1 || true
wait_for 240 "the application rejoins the new namespace and answers again after the anchor restart" curl -fsS --max-time 2 "http://127.0.0.1:$STATUS_PORT/health"

echo "== redaction and direct listeners"
docker logs "$PROJECT-go-trader-1" >> "$WORK/all-output.out" 2>&1 || true
if [[ -s "$WORK/direct.log" ]]; then bad "the direct listener saw a connection"; else pass "the direct listener saw no connection"; fi
if grep -q "$SENTINEL" "$WORK/all-output.out" "$WORK/controller.log"; then bad "the credential sentinel appears in an output"; else pass "the credential sentinel appears in no output"; fi

echo "checks: $checks, failures: $failures"
if [[ "$failures" != "0" ]]; then
    exit 1
fi
echo "PASS: egress docker deployment (anchor enforcement, application attestation, relay-only egress, status relay, one-shot commands, anchor restart)"

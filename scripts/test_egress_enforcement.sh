#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO=$(cd "$SCRIPT_DIR/.." && pwd)

skip() {
    if [[ "${EGRESS_FIXTURE_REQUIRE_RUN:-0}" == "1" ]]; then
        echo "FAIL: EGRESS_FIXTURE_REQUIRE_RUN=1 but this host cannot run the fixture: $*" >&2
        exit 1
    fi
    echo "SKIP: $*"
    exit 0
}
[[ "$(uname -s)" == "Linux" ]] || skip "Linux required"
[[ "$(id -u)" == "0" ]] || skip "root required"
for tool in ip nft setpriv unshare openssl curl python3 useradd; do
    command -v "$tool" >/dev/null 2>&1 || skip "$tool required"
done
: "${GO_TRADER_BIN:?set GO_TRADER_BIN to a built go-trader binary}"
: "${EGRESS_CONTROLLER_BIN:?set EGRESS_CONTROLLER_BIN to a built go-trader-egress-controller}"
: "${EGRESS_FIXTURE_BIN:?set EGRESS_FIXTURE_BIN to a built egress-fixture}"
VENV_PY="${EGRESS_VENV_PYTHON:-$REPO/.venv/bin/python3}"
[[ -x "$VENV_PY" ]] || skip "the repository virtualenv ($VENV_PY) is required; run uv sync"

have_systemd=0
if [[ -d /run/systemd/system ]] && command -v systemd-run >/dev/null 2>&1; then
    have_systemd=1
elif [[ "${EGRESS_REQUIRE_SYSTEMD:-0}" == "1" ]]; then
    echo "FAIL: EGRESS_REQUIRE_SYSTEMD=1 but systemd is not running" >&2
    exit 1
fi

DEP="fixture$RANDOM"
NS="go-trader-egress-$DEP"
WORK=$(mktemp -d /var/lib/egress-fixture.XXXXXX)
chmod 0755 "$WORK"
MANIFEST="/etc/go-trader/egress/$DEP.json"
LIBEXEC="/usr/local/libexec/go-trader-egress/$DEP"
CTL="$LIBEXEC/go-trader-egress-controller"
FIX="$WORK/egress-fixture"
WUID=1731
XUID=1732
WUSER="gtegw$DEP"
XUSER="gtegx$DEP"
SENTINEL="sntl$(od -An -N10 -tx1 /dev/urandom | tr -d ' \n')"
PROXY_USER="fixtureuser"
OUT_IF="gte$RANDOM"
STATUS_PORT=$((20000 + RANDOM % 20000))
HOSTS_BACKUP="$WORK/hosts.orig"
cp /etc/hosts "$HOSTS_BACKUP"
pids=()
ctl_pid=""
failures=0
checks=0

cleanup() {
    set +e
    for p in "${pids[@]}" $ctl_pid; do
        kill "$p" 2>/dev/null
    done
    sleep 0.3
    for p in "${pids[@]}" $ctl_pid; do
        kill -9 "$p" 2>/dev/null
    done
    cat "$HOSTS_BACKUP" > /etc/hosts
    if [[ -n "$(ip netns pids "$NS" 2>/dev/null)" ]]; then
        ip netns pids "$NS" | xargs -r kill -9
    fi
    ip netns delete "$NS" 2>/dev/null
    ip netns delete "go-trader-notify-$DEP" 2>/dev/null
    ip link delete "$OUT_IF" 2>/dev/null
    [[ -z "${IPV6_ALL:-}" ]] || sysctl -qw "net.ipv6.conf.all.disable_ipv6=$IPV6_ALL" "net.ipv6.conf.default.disable_ipv6=$IPV6_DEFAULT"
    rm -rf "/run/go-trader-egress/$DEP" "$LIBEXEC" "$MANIFEST"
    userdel "$WUSER" 2>/dev/null
    userdel "$XUSER" 2>/dev/null
    rm -rf "$WORK"
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
    local pattern="$1" desc="$2"
    if grep -Eq -- "$pattern" <<<"$LAST_OUT"; then
        pass "$desc"
    else
        bad "$desc: output did not match /$pattern/: $LAST_OUT"
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

useradd --uid "$WUID" --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin "$WUSER"
useradd --uid "$XUID" --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin "$XUSER"
install -d -o root -g root -m 0755 "$LIBEXEC" /etc/go-trader /etc/go-trader/egress
install -o root -g root -m 0755 "$EGRESS_CONTROLLER_BIN" "$CTL"
install -o root -g root -m 0755 "$EGRESS_FIXTURE_BIN" "$FIX"
install -o root -g root -m 0755 "$GO_TRADER_BIN" "$WORK/go-trader"

IPV6_ALL=$(sysctl -n net.ipv6.conf.all.disable_ipv6 2>/dev/null || echo 0)
IPV6_DEFAULT=$(sysctl -n net.ipv6.conf.default.disable_ipv6 2>/dev/null || echo 0)
sysctl -qw net.ipv6.conf.all.disable_ipv6=0 net.ipv6.conf.default.disable_ipv6=0
ip link add "$OUT_IF" type dummy
ip addr add 10.231.0.1/24 dev "$OUT_IF"
ip addr add 10.231.0.2/24 dev "$OUT_IF"
ip addr add 10.231.0.3/24 dev "$OUT_IF"
ip -6 addr add fd00:231::1/64 dev "$OUT_IF" nodad
ip link set "$OUT_IF" up

openssl req -x509 -newkey rsa:2048 -nodes -days 2 -subj "/CN=egress fixture CA" -keyout "$WORK/ca.key" -out "$WORK/ca.pem" >/dev/null 2>&1
issue_cert() {
    local name="$1" sans="$2"
    openssl req -newkey rsa:2048 -nodes -subj "/CN=$name" -keyout "$WORK/$name.key" -out "$WORK/$name.csr" >/dev/null 2>&1
    printf 'subjectAltName=%s\n' "$sans" > "$WORK/$name.ext"
    openssl x509 -req -in "$WORK/$name.csr" -CA "$WORK/ca.pem" -CAkey "$WORK/ca.key" -CAcreateserial -days 2 -extfile "$WORK/$name.ext" -out "$WORK/$name.pem" >/dev/null 2>&1
}
issue_cert venue "DNS:api.hyperliquid.xyz,DNS:api.binance.us,DNS:venue.egress.test,DNS:discord.com"
issue_cert proxy "DNS:proxy.egress.test"
chmod 0644 "$WORK"/*.pem

hosts_set_proxy() {
    {
        grep -v 'egress\.test$\|api\.hyperliquid\.xyz\|api\.binance\.us' "$HOSTS_BACKUP"
        echo "10.231.0.1 api.hyperliquid.xyz api.binance.us venue.egress.test"
        [[ -z "$1" ]] || echo "$1 proxy.egress.test"
    } > "$WORK/hosts.next"
    cat "$WORK/hosts.next" > /etc/hosts
}
hosts_set_proxy 10.231.0.2

"$FIX" listen --addr 10.231.0.1:443 --cert "$WORK/venue.pem" --key "$WORK/venue.key" --reply venue-tls-ok --log "$WORK/venue-tls.log" &
pids+=($!)
"$FIX" listen --addr 10.231.0.1:80 --reply venue-ok --log "$WORK/venue.log" &
pids+=($!)
"$FIX" listen --addr 10.231.0.1:81 --hold --log "$WORK/venue-hold.log" &
pids+=($!)
"$FIX" listen --addr 10.231.0.1:9443 --log "$WORK/direct4.log" &
pids+=($!)
"$FIX" listen --addr "[fd00:231::1]:9443" --log "$WORK/direct6.log" &
pids+=($!)
"$FIX" udp-listen --addr 10.231.0.1:53 --log "$WORK/direct-dns.log" &
pids+=($!)
"$FIX" udp-listen --addr 10.231.0.1:9053 --log "$WORK/direct-udp.log" &
pids+=($!)
"$FIX" proxy --listen 10.231.0.2:3128 --scheme http --user "$PROXY_USER" --pass "$SENTINEL" --forward --forward-host 10.231.0.1 --log "$WORK/proxy-a.log" &
pids+=($!)
"$FIX" proxy --listen 10.231.0.3:3128 --scheme http --user "$PROXY_USER" --pass "$SENTINEL" --forward --forward-host 10.231.0.1 --log "$WORK/proxy-b.log" &
pids+=($!)
"$FIX" proxy --listen 10.231.0.2:1080 --scheme socks5 --user "$PROXY_USER" --pass "$SENTINEL" --forward --forward-host 10.231.0.1 --log "$WORK/proxy-socks.log" &
pids+=($!)
"$FIX" proxy --listen 10.231.0.2:3129 --scheme https --cert "$WORK/proxy.pem" --key "$WORK/proxy.key" --user "$PROXY_USER" --pass "$SENTINEL" --forward --forward-host 10.231.0.1 --log "$WORK/proxy-https.log" &
pids+=($!)
sleep 0.5

PROXY_URL="http://$PROXY_USER:$SENTINEL@proxy.egress.test:3128"

TREE="$WORK/tree"
mkdir -p "$TREE/scheduler" "$WORK/state" "$WORK/logs"
for p in shared_scripts shared_strategies shared_tools platforms backtest; do
    cp -R "$REPO/$p" "$TREE/$p"
done
cp "$REPO/pyproject.toml" "$TREE/"
ln -s "$(dirname "$(dirname "$VENV_PY")")" "$TREE/.venv"
"$VENV_PY" -m compileall -q -j 0 "$(dirname "$(dirname "$VENV_PY")")/lib" "$TREE/shared_scripts" "$TREE/shared_strategies" "$TREE/shared_tools" "$TREE/platforms" "$TREE/backtest" >/dev/null 2>&1 || true
chown -R "$WUID:$WUID" "$WORK/state" "$WORK/logs"
cat > "$WORK/config.json" <<EOF
{
  "config_version": 20,
  "interval_seconds": 300,
  "db_file": "$WORK/state/state.db",
  "log_dir": "$WORK/logs",
  "status_port": $STATUS_PORT,
  "egress": {"proxy_required": true},
  "strategies": [
    {"id": "hl-momentum-btc", "type": "perps", "script": "shared_scripts/check_hyperliquid.py", "args": ["momentum", "BTC", "1h", "--mode=paper"], "capital": 1000, "max_drawdown_pct": 50, "allow_no_edge": true}
  ]
}
EOF
chmod 0644 "$WORK/config.json"
cat > "$WORK/app.env" <<EOF
GO_TRADER_EGRESS_PROXY=$PROXY_URL
SSL_CERT_FILE=$WORK/ca.pem
PYTHONDONTWRITEBYTECODE=1
EOF
chmod 0600 "$WORK/app.env"
chown "$WUID:$WUID" "$WORK/app.env"

cat > "$MANIFEST" <<EOF
{
  "deployment": "$DEP",
  "mode": "host",
  "proxy": {"scheme": "http", "host": "proxy.egress.test", "port": 3128},
  "worker_uid": $WUID,
  "worker_gid": $WUID,
  "resolve_seconds": 10,
  "status_relays": [{"listen": "127.0.0.1:$STATUS_PORT", "target_port": $STATUS_PORT}],
  "host": {
    "app_unit": "go-trader-egress-fixture-app.service",
    "app_executable": "$WORK/go-trader",
    "working_directory": "$TREE",
    "environment_files": ["$WORK/app.env"],
    "read_write_paths": ["$WORK/state", "$WORK/logs"]
  }
}
EOF
chmod 0644 "$MANIFEST"

echo "== preflight and controller start"
if [[ "$have_systemd" == "1" ]]; then
    expect_rc 0 "controller preflight passes" "$CTL" preflight --manifest "$MANIFEST"
else
    expect_rc 1 "controller preflight refuses host mode without systemd" "$CTL" preflight --manifest "$MANIFEST"
    expect_out 'namespace rules install and verify' "preflight installed and verified rules in a scratch namespace"
fi
"$CTL" serve --manifest "$MANIFEST" > "$WORK/controller.log" 2>&1 &
ctl_pid=$!
wait_for 20 "controller reports ready" "$CTL" status --manifest "$MANIFEST"

RES="/run/go-trader-egress/$DEP/resolver"
worker_env=(env -i "PATH=/usr/sbin:/usr/bin:/sbin:/bin" "HOME=/tmp" "GO_TRADER_EGRESS_DEPLOYMENT=$DEP" "PYTHONDONTWRITEBYTECODE=1")
as_worker() {
    unshare -m --propagation private /bin/sh -c 'mount --bind "$0/hosts" /etc/hosts && mount --bind "$0/resolv.conf" /etc/resolv.conf && mount --bind "$0/nsswitch.conf" /etc/nsswitch.conf && shift && exec "$@"' "$RES" x \
        ip netns exec "$NS" setpriv --reuid "$WUID" --regid "$WUID" --clear-groups --inh-caps=-all --bounding-set=-all --no-new-privs "${worker_env[@]}" "$@"
}

echo "== kernel rules"
LAST_OUT=$(ip netns exec "$NS" nft list table inet go_trader_egress 2>&1)
expect_out 'policy drop' "namespace output, input and forward chains drop by default"
expect_out "generation=" "rules carry the installed generation"

echo "== attestation"
expect_rc 0 "worker in the namespace with no capabilities and no_new_privs attests" as_worker "$FIX" attest --deployment "$DEP" --proxy "$PROXY_URL"
expect_out '"role":"worker"' "attestation names the worker role"
if grep -q "$SENTINEL" <<<"$LAST_OUT"; then bad "attestation output holds the credential"; else pass "attestation output holds no credential"; fi
expect_rc 3 "a process outside the namespace is refused" setpriv --reuid "$WUID" --regid "$WUID" --clear-groups --inh-caps=-all --bounding-set=-all --no-new-privs "$FIX" attest --deployment "$DEP" --proxy "$PROXY_URL"
expect_out 'not the managed namespace' "refusal names the namespace mismatch"
expect_rc 3 "a worker without no_new_privs is refused" ip netns exec "$NS" setpriv --reuid "$WUID" --regid "$WUID" --clear-groups --inh-caps=-all --bounding-set=-all "$FIX" attest --deployment "$DEP" --proxy "$PROXY_URL"
expect_out 'no_new_privs' "refusal names no_new_privs"
expect_rc 3 "a worker holding an ambient capability is refused" ip netns exec "$NS" setpriv --reuid "$WUID" --regid "$WUID" --clear-groups --inh-caps=+net_raw --ambient-caps=+net_raw --no-new-privs "$FIX" attest --deployment "$DEP" --proxy "$PROXY_URL"
expect_out 'holds capabilities' "refusal names the capability"
expect_rc 3 "another account in the namespace is refused" ip netns exec "$NS" setpriv --reuid "$XUID" --regid "$XUID" --clear-groups --inh-caps=-all --bounding-set=-all --no-new-privs "$FIX" attest --deployment "$DEP" --proxy "$PROXY_URL"
expect_rc 3 "a worker configured for a different proxy is refused" as_worker "$FIX" attest --deployment "$DEP" --proxy "http://other.egress.test:3128"
expect_out 'enforces proxy' "refusal names the proxy mismatch"

echo "== direct access is denied (Go raw sockets, IPv4, IPv6, UDP, DNS)"
expect_rc 3 "raw Go TCP to an outside IPv4 listener fails" as_worker "$FIX" connect --network tcp4 --addr 10.231.0.1:9443
expect_rc 3 "raw Go TCP to an outside IPv6 listener fails" as_worker "$FIX" connect --network tcp6 --addr "[fd00:231::1]:9443"
expect_rc 3 "raw Go UDP to an outside listener fails" as_worker "$FIX" connect --network udp4 --addr 10.231.0.1:9053
expect_rc 3 "raw Go DNS query to an outside resolver fails" as_worker "$FIX" connect --network udp4 --addr 10.231.0.1:53

cat > "$WORK/direct_probe.py" <<'PY'
import json
import socket
import subprocess
import sys

results = {}

def attempt(name, fn):
    try:
        fn()
        results[name] = "connected"
    except Exception as exc:
        results[name] = "blocked: %s" % type(exc).__name__

attempt("tcp4", lambda: socket.create_connection(("10.231.0.1", 9443), timeout=3).close())
attempt("tcp6", lambda: socket.create_connection(("fd00:231::1", 9443), timeout=3).close())

def udp():
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    s.sendto(b"probe", ("10.231.0.1", 9053))

attempt("udp", udp)

def dns():
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    s.sendto(b"\x12\x34\x01\x00\x00\x01\x00\x00\x00\x00\x00\x00\x03api\x0bhyperliquid\x03xyz\x00\x00\x01\x00\x01", ("10.231.0.1", 53))

attempt("dns_udp", dns)
attempt("getaddrinfo_venue", lambda: socket.getaddrinfo("api.hyperliquid.xyz", 443))

def icmp():
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM, socket.IPPROTO_ICMP)
    s.sendto(b"\x08\x00\xf7\xff\x00\x00\x00\x00", ("10.231.0.1", 0))

attempt("icmp", icmp)

def raw():
    s = socket.socket(socket.AF_INET, socket.SOCK_RAW, socket.IPPROTO_TCP)
    s.sendto(b"x" * 20, ("10.231.0.1", 0))

attempt("raw", raw)

def curl_direct():
    from curl_cffi import requests as cr
    cr.get("http://10.231.0.1:9443/", timeout=3, proxies={"http": "", "https": ""})

attempt("curl_cffi_direct", curl_direct)

child = subprocess.run([sys.executable, "-c", "import socket; socket.create_connection(('10.231.0.1', 9443), timeout=3)"], capture_output=True)
results["child_without_bootstrap"] = "connected" if child.returncode == 0 else "blocked"
print(json.dumps(results, sort_keys=True))
PY
chmod 0644 "$WORK/direct_probe.py"
expect_rc 0 "Python probe of direct paths runs in the namespace" as_worker "$VENV_PY" "$WORK/direct_probe.py"
for key in tcp4 tcp6 udp dns_udp getaddrinfo_venue icmp raw curl_cffi_direct child_without_bootstrap; do
    if "$VENV_PY" -c 'import json,sys; d=json.loads(sys.argv[1].splitlines()[-1]); sys.exit(0 if d[sys.argv[2]].startswith("blocked") else 1)' "$LAST_OUT" "$key"; then
        pass "Python $key is blocked"
    else
        bad "Python $key was not blocked: $LAST_OUT"
    fi
done

echo "== the configured proxy is the only way out"
expect_rc 0 "HTTP CONNECT through the relay reaches the venue" as_worker "$FIX" dial-proxy --proxy "$PROXY_URL" --target venue.egress.test:80
expect_out 'status=200 body=venue-ok' "venue answered through the proxy"
expect_rc 3 "wrong proxy credentials fail with no fallback" as_worker "$FIX" dial-proxy --proxy "http://$PROXY_USER:wrong@proxy.egress.test:3128" --target venue.egress.test:80
expect_out '407' "the proxy refusal is reported"
if grep -q '"target":"venue.egress.test:80"' "$WORK/proxy-a.log"; then pass "proxy A recorded the venue host name"; else bad "proxy A did not record the venue"; fi

cat > "$WORK/policy_driver.py" <<'PY'
import json
import os
import sys
import requests
r = requests.get("https://venue.egress.test/", timeout=10)
print(json.dumps({"status": r.status_code, "body": r.text, "https_proxy_set": bool(os.environ.get("HTTPS_PROXY"))}))
PY
chmod 0644 "$WORK/policy_driver.py"
expect_rc 0 "the Python bootstrap routes requests through the relay" as_worker env "GO_TRADER_EGRESS_MODE=proxy" "GO_TRADER_EGRESS_PROXY=$PROXY_URL" "NO_PROXY=*" "HTTP_PROXY=http://10.231.0.1:9443" "HTTPS_PROXY=http://10.231.0.1:9443" "REQUESTS_CA_BUNDLE=$WORK/ca.pem" "$VENV_PY" "$TREE/shared_tools/egress_bootstrap.py" "$WORK/policy_driver.py"
expect_out '"body": "venue-tls-ok"' "the venue answered the bootstrapped request despite hostile NO_PROXY, HTTP_PROXY and HTTPS_PROXY"
expect_rc 0 "the Python client policy check passes in the namespace" as_worker env "GO_TRADER_EGRESS_MODE=proxy" "GO_TRADER_EGRESS_PROXY=$PROXY_URL" "$VENV_PY" "$TREE/shared_tools/egress_bootstrap.py" --check-policy ccxt hyperliquid requests yfinance
expect_out '"ok": true' "policy check reports ok"
SOCKS_URL="socks5://$PROXY_USER:$SENTINEL@proxy.egress.test:1080"
"$CTL" status --manifest "$MANIFEST" >/dev/null

echo "== the go-trader binary refuses without enforcement"
expect_rc 1 "manual command with a malformed proxy URL is refused" env -i PATH=/usr/bin:/bin "GO_TRADER_EGRESS_PROXY=ftp://$PROXY_USER:$SENTINEL@proxy.egress.test:21" "$WORK/go-trader" manual-open --config "$WORK/config.json"
expect_out 'unsupported or missing scheme' "the malformed URL refusal names the reason"
expect_rc 1 "manual command outside the namespace is refused" env -i PATH=/usr/bin:/bin "GO_TRADER_EGRESS_PROXY=$PROXY_URL" "GO_TRADER_EGRESS_DEPLOYMENT=$DEP" "$WORK/go-trader" manual-open --config "$WORK/config.json"
expect_out 'enforcement is unavailable' "outside-namespace refusal names enforcement"
expect_rc 1 "daemon with proxy_required and no proxy URL is refused" env -i PATH=/usr/bin:/bin "$WORK/go-trader" --config "$WORK/config.json" --once
expect_out 'proxy_required is true' "missing URL refusal names proxy_required"

echo "== the daemon inside the namespace"
expect_rc 0 "one-cycle daemon in the namespace completes" as_worker env "GO_TRADER_EGRESS_PROXY=$PROXY_URL" "SSL_CERT_FILE=$WORK/ca.pem" /bin/sh -c "cd '$TREE' && exec timeout 600 '$WORK/go-trader' --config '$WORK/config.json' --once"
echo "$LAST_OUT" > "$WORK/once.log"
expect_out '\[egress\] proxy mode: on \(http://proxy.egress.test:3128' "startup line names the redacted proxy"
expect_out 'startup check: api.hyperliquid.xyz reachable' "Go startup check reached the venue through the proxy"
expect_out 'python client policy check passed' "Python client policy check passed at startup"
hl_connects=$(grep -c '"target":"api.hyperliquid.xyz:443"' "$WORK/proxy-a.log" || true)
if [[ "$hl_connects" -ge 2 ]]; then pass "the Go startup check and the Python check script both reached Hyperliquid through the proxy ($hl_connects tunnels)"; else bad "expected at least 2 Hyperliquid tunnels, saw $hl_connects"; fi

as_worker env "GO_TRADER_EGRESS_PROXY=$PROXY_URL" "SSL_CERT_FILE=$WORK/ca.pem" /bin/sh -c "cd '$TREE' && exec '$WORK/go-trader' --config '$WORK/config.json'" > "$WORK/daemon.log" 2>&1 &
daemon_pid=$!
pids+=($daemon_pid)
wait_for 90 "the daemon status answers through the host status relay" curl -fsS --max-time 2 "http://127.0.0.1:$STATUS_PORT/health"
LAST_OUT=$(curl -fsS --max-time 5 "http://127.0.0.1:$STATUS_PORT/status" 2>&1 || true)
echo "$LAST_OUT" > "$WORK/status.json"
expect_out '"mode": ?"proxy"' "status reports proxy mode"
expect_out '"enforcement": ?"attested"' "status reports attested enforcement"
kill "$daemon_pid" 2>/dev/null || true
wait "$daemon_pid" 2>/dev/null || true

echo "== controller loss keeps the namespace closed"
kill -9 "$ctl_pid"
wait "$ctl_pid" 2>/dev/null || true
ctl_pid=""
LAST_OUT=$(ip netns exec "$NS" nft list table inet go_trader_egress 2>&1)
expect_out 'policy drop' "rules stay installed after the controller dies"
expect_rc 3 "direct access stays blocked after the controller dies" as_worker "$FIX" connect --network tcp4 --addr 10.231.0.1:9443
expect_rc 3 "the proxy path fails closed while the controller is down" as_worker "$FIX" dial-proxy --proxy "$PROXY_URL" --target venue.egress.test:80 --timeout 5s
"$CTL" serve --manifest "$MANIFEST" >> "$WORK/controller.log" 2>&1 &
ctl_pid=$!
wait_for 20 "a restarted controller reports ready" "$CTL" status --manifest "$MANIFEST"
expect_rc 0 "the proxy path recovers after the controller restarts" as_worker "$FIX" dial-proxy --proxy "$PROXY_URL" --target venue.egress.test:80

echo "== proxy address refresh"
as_worker "$FIX" hold-tunnel --proxy "$PROXY_URL" --target venue.egress.test:81 --max 60s > "$WORK/hold.out" 2>&1 &
hold_pid=$!
pids+=($hold_pid)
sleep 1
hosts_set_proxy 10.231.0.3
wait_for 30 "the controller moved to the new proxy address" sh -c "'$CTL' status --manifest '$MANIFEST' | grep -q '\"10.231.0.3:3128\"'"
wait "$hold_pid" 2>/dev/null || true
LAST_OUT=$(cat "$WORK/hold.out")
expect_out 'closed after' "the relayed connection to the removed address was closed"
expect_rc 0 "new connections use the new proxy address" as_worker "$FIX" dial-proxy --proxy "$PROXY_URL" --target venue.egress.test:80
if grep -q '"target":"venue.egress.test:80"' "$WORK/proxy-b.log"; then pass "proxy B carried the new connection"; else bad "proxy B recorded nothing"; fi
hosts_set_proxy ""
wait_for 30 "a failed re-resolution is reported" sh -c "'$CTL' status --manifest '$MANIFEST' | grep -q 'last_resolve_error'"
LAST_OUT=$("$CTL" status --manifest "$MANIFEST" 2>&1 || true)
expect_out '"10.231.0.3:3128"' "the last good address set is kept after a failed resolution"
expect_rc 0 "connections still use the last good address" as_worker "$FIX" dial-proxy --proxy "$PROXY_URL" --target venue.egress.test:80
hosts_set_proxy 10.231.0.2

echo "== teardown waits for workers"
as_worker /bin/sleep 30 &
sleeper=$!
pids+=($sleeper)
sleep 0.5
expect_rc 1 "teardown refuses while a worker runs in the namespace" "$CTL" teardown --manifest "$MANIFEST"
kill "$sleeper" 2>/dev/null || true

if [[ "$have_systemd" == "1" ]]; then
    echo "== systemd sandbox and restricted launcher"
    mapfile -t props < <("$CTL" render-units --manifest "$MANIFEST" | sed -n '/^\[Service\]/,/^$/p' | grep '=' )
    run_props=()
    for p in "${props[@]}"; do
        run_props+=(-p "$p")
    done
    cat > "$WORK/sandbox_probe.py" <<'PY'
import json
import socket
out = {}
for path in ["/run/dbus/system_bus_socket", "/run/systemd/resolve/io.systemd.Resolve", "/run/nscd/socket"]:
    s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    try:
        s.connect(path)
        out[path] = "connected"
    except Exception as exc:
        out[path] = "blocked: %s" % type(exc).__name__
try:
    socket.getaddrinfo("api.hyperliquid.xyz", 443)
    out["resolve_venue"] = "resolved"
except Exception as exc:
    out["resolve_venue"] = "blocked: %s" % type(exc).__name__
try:
    socket.getaddrinfo("proxy.egress.test", 3128)
    out["resolve_proxy"] = "resolved"
except Exception as exc:
    out["resolve_proxy"] = "blocked: %s" % type(exc).__name__
print(json.dumps(out, sort_keys=True))
PY
    chmod 0644 "$WORK/sandbox_probe.py"
    expect_rc 0 "the unit sandbox from render-units runs" systemd-run --wait --pipe --collect --quiet --uid="$WUID" --gid="$WUID" "${run_props[@]}" -- "$VENV_PY" "$WORK/sandbox_probe.py"
    expect_out '"resolve_proxy": "resolved"' "the proxy name resolves inside the sandbox"
    expect_out '"resolve_venue": "blocked' "venue names do not resolve inside the sandbox"
    if grep -q '"connected"' <<<"$LAST_OUT"; then bad "a host resolver or bus socket is reachable: $LAST_OUT"; else pass "host resolver and bus sockets are unreachable"; fi
    expect_rc 0 "launcher runs probe inside the namespace" "$CTL" launch --manifest "$MANIFEST" -- probe --config "$WORK/config.json"
    expect_out 'probe: egress enforcement ready' "launched probe sees ready enforcement"
    expect_rc 2 "launcher runs a network command that attests inside the namespace" "$CTL" launch --manifest "$MANIFEST" -- backfill --no-such-flag
    expect_out 'Unknown backfill target' "launched backfill passed attestation and ran"
    expect_rc 1 "launcher refuses a local command" "$CTL" launch --manifest "$MANIFEST" -- inspect
else
    echo "NOTE: systemd is not running; the unit sandbox and the launcher are not checked"
fi

echo "== direct listeners and credential redaction"
for f in direct4 direct6 direct-udp direct-dns; do
    if [[ -s "$WORK/$f.log" ]]; then bad "$f listener saw a direct connection: $(cat "$WORK/$f.log")"; else pass "$f listener saw no direct connection"; fi
done
if grep -rl "$SENTINEL" "$WORK"/*.log "$WORK"/*.out "$WORK"/*.json 2>/dev/null; then
    bad "the credential sentinel appears in an output"
else
    pass "the credential sentinel appears in no log, status or output"
fi

echo "checks: $checks, failures: $failures"
if [[ "$failures" != "0" ]]; then
    exit 1
fi
echo "PASS: egress enforcement (namespace rules, attestation, relay-only egress, controller loss, address refresh, refusal paths)"

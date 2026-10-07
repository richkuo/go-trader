#!/usr/bin/env bash
set -euo pipefail

IMAGE="${GO_TRADER_TEST_IMAGE:?set GO_TRADER_TEST_IMAGE to the image under test}"
EXPECT_VERSION="${GO_TRADER_EXPECT_VERSION:?set GO_TRADER_EXPECT_VERSION to the version stamped into the image}"
EXPECT_COMMIT="${GO_TRADER_EXPECT_COMMIT:?set GO_TRADER_EXPECT_COMMIT to the source commit stamped into the image}"
CANARY="${GO_TRADER_CONTEXT_CANARY:-}"
HOST_PORT="${GO_TRADER_TEST_HOST_PORT:-18099}"
REQUIRE_ONCE="${GO_TRADER_TEST_REQUIRE_ONCE:-1}"

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
fixtures="$repo/scripts/fixtures/container"
work="$(mktemp -d "${TMPDIR:-/tmp}/go-trader-container-test.XXXXXX")"
project="gtct$$"
tag="container-test-$$"
token="$(python3 -c 'import secrets; print(secrets.token_hex(24))')"
extra_containers=()
extra_volumes=()

say() { printf '\n== %s\n' "$*"; }
ok() { printf 'ok: %s\n' "$*"; }

compose() {
    docker compose --project-directory "$work" -p "$project" -f "$work/compose.yaml" -f "$work/compose.test.yaml" "$@"
}

fail() {
    printf 'FAIL: %s\n' "$*" >&2
    compose logs --no-color --tail 200 go-trader >&2 2>/dev/null || true
    exit 1
}

cleanup() {
    local rc=$?
    for c in ${extra_containers[@]+"${extra_containers[@]}"}; do docker rm -f "$c" >/dev/null 2>&1 || true; done
    compose --profile cli down -v --remove-orphans >/dev/null 2>&1 || true
    for v in ${extra_volumes[@]+"${extra_volumes[@]}"}; do docker volume rm -f "$v" >/dev/null 2>&1 || true; done
    docker image rm "ghcr.io/richkuo/go-trader:$tag" >/dev/null 2>&1 || true
    rm -rf "$work"
    exit "$rc"
}
trap cleanup EXIT

cli() { compose run --rm -T cli "$@"; }
cli_sh() { compose run --rm -T --entrypoint sh cli -c "$1"; }
edit_config() { compose run --rm -T --entrypoint /app/.venv/bin/python3 cli /app/testsupport/edit_config.py /data/config.json "$@" >/dev/null; }
container_id() { docker ps -a -q --filter "label=com.docker.compose.project=$project" --filter label=com.docker.compose.service=go-trader --filter label=com.docker.compose.oneoff=False; }
restart_count() { docker inspect -f '{{.RestartCount}}' "$(container_id)"; }
health_status() { docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{end}}' "$1"; }
service_logs() { compose logs --no-color go-trader 2>&1; }
http_code() { curl -s -o /dev/null -w '%{http_code}' --max-time 10 "$@" || true; }
api() { curl -s --max-time 30 -H "Authorization: Bearer $token" "$@"; }
count_matches() { grep -c -- "$1" || true; }

wait_for() {
    local seconds="$1" desc="$2"
    shift 2
    local end=$((SECONDS + seconds))
    until "$@"; do
        ((SECONDS < end)) || fail "timed out after ${seconds}s waiting for: $desc"
        sleep 1
    done
}

has() { local data; data="$(cat)"; grep -q -- "$1" <<<"$data"; }
logs_contain() { service_logs | has "$1"; }
container_logs_contain() { docker logs "$1" 2>&1 | has "$2"; }
is_health() { [[ "$(health_status "$1")" == "$2" ]]; }
service_health_is() { is_health "$(container_id)" "$1"; }
restart_count_at_least() { (( $(restart_count) >= $1 )); }
daemon_pid() { service_logs | sed -n 's/.*\[supervisor\] started daemon pid \([0-9][0-9]*\).*/\1/p' | tail -1; }
proc_count() { compose exec -T go-trader sh -c "grep -l -- '$1' /proc/[0-9]*/cmdline 2>/dev/null | wc -l" | tr -d ' \r'; }
hup() { compose exec -T go-trader sh -c 'kill -HUP 1'; }
reload_count() { service_logs | grep -c "SIGHUP received; reloading" || true; }
reloads_above() { (( $(reload_count) > $1 )); }
slow_checks_running() { (( $(proc_count '[s]low_check') >= 2 )); }

json_field() { python3 -c 'import json,sys; d=json.load(sys.stdin); print(d.get(sys.argv[1], "") if isinstance(d, dict) else "")' "$1"; }

cp "$repo/docker/compose.yaml" "$work/compose.yaml"
printf 'GO_TRADER_TAG=%s\nGO_TRADER_HOST_PORT=%s\n' "$tag" "$HOST_PORT" >"$work/.env"
printf 'STATUS_AUTH_TOKEN=%s\n' "$token" >"$work/go-trader.env"
cat >"$work/compose.test.yaml" <<EOF
services:
  go-trader:
    volumes:
      - $fixtures:/app/testsupport:ro
    healthcheck:
      start_period: 10s
      start_interval: 2s
      interval: 3s
      retries: 2
  cli:
    volumes:
      - $fixtures:/app/testsupport:ro
EOF
docker tag "$IMAGE" "ghcr.io/richkuo/go-trader:$tag"

say "image identity"
got="$(docker run --rm "$IMAGE" --version)"
[[ "$got" == "$EXPECT_VERSION" ]] || fail "--version printed '$got', want '$EXPECT_VERSION'"
got="$(docker run --rm "$IMAGE" version)"
[[ "$got" == "$EXPECT_VERSION" ]] || fail "version printed '$got', want '$EXPECT_VERSION'"
vjson="$(docker run --rm "$IMAGE" version --json)"
bin_commit="$(json_field source_commit <<<"$vjson")"
bin_version="$(json_field version <<<"$vjson")"
label_commit="$(docker image inspect -f '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$IMAGE")"
label_version="$(docker image inspect -f '{{index .Config.Labels "org.opencontainers.image.version"}}' "$IMAGE")"
[[ "$bin_commit" == "$EXPECT_COMMIT" ]] || fail "binary SourceCommit '$bin_commit', want '$EXPECT_COMMIT'"
[[ "$bin_version" == "$EXPECT_VERSION" ]] || fail "binary Version '$bin_version', want '$EXPECT_VERSION'"
[[ "$label_commit" == "$bin_commit" ]] || fail "image revision label '$label_commit' differs from binary SourceCommit '$bin_commit'"
[[ "$label_version" == "$bin_version" ]] || fail "image version label '$label_version' differs from binary Version '$bin_version'"
ok "--version=$got; binary SourceCommit=$bin_commit matches the image revision label"

say "image layers and filesystem"
docker save "$IMAGE" -o "$work/image.tar"
python3 "$repo/scripts/container_layer_scan.py" "$work/image.tar" "$CANARY" || fail "image layers hold forbidden content"
rm -f "$work/image.tar"
listing="$(docker run --rm --entrypoint sh "$IMAGE" -c 'ls -A /data /app/logs; [ ! -e /app/.git ] && [ ! -e /app/scheduler ] && [ ! -e /app/scripts ] && echo clean')"
[[ "$(printf '%s\n' "$listing" | grep -v -e '^/data:$' -e '^/app/logs:$' -e '^$')" == "clean" ]] || fail "mount targets are not empty or the image holds the source tree: $listing"
owner="$(docker run --rm --entrypoint stat "$IMAGE" -c '%u:%g %a' /data /app/logs | tr '\n' ' ')"
[[ "$owner" == "10001:10001 700 10001:10001 750 " ]] || fail "mount target ownership is '$owner'"
[[ -n "$CANARY" ]] && ok "no planted canary bytes in any layer"
ok "no secrets, runtime config, databases, locks, logs or git metadata; empty mount targets owned by 10001"

say "first setup on a fresh named volume"
spot_json='{"assets":["BTC"],"enableSpot":true,"spotStrategies":["sma_crossover"],"spotCapital":1000,"spotDrawdown":10}'
cli init --json "$spot_json" >/dev/null || fail "init --json on a fresh volume failed"
cfg_json="$(cli_sh 'cat /data/config.json')"
python3 - "$cfg_json" <<'PY' || fail "generated config is not a paper-only config with its database in /data"
import json, sys
cfg = json.loads(sys.argv[1])
assert cfg["db_file"] == "/data/state.db", cfg["db_file"]
assert cfg.get("auto_update", "off") == "off", cfg.get("auto_update")
for s in cfg["strategies"]:
    assert "--mode=live" not in s.get("args", []), s["id"]
    assert s.get("type") != "manual", s["id"]
PY
strategy_id="$(python3 -c 'import json,sys; print(json.loads(sys.argv[1])["strategies"][0]["id"])' "$cfg_json")"
ok "init wrote /data/config.json with db_file /data/state.db (strategy $strategy_id)"

out="$(cli init --json '{"assets":["BTC"],"enablePerps":true,"perpsMode":"live"}' --output /data/live.json 2>&1)" && fail "init accepted a live perps config"
grep -q "paper strategies only" <<<"$out" || fail "live init refusal did not explain itself: $out"
out="$(cli init --json "$spot_json" --output /tmp/config.json 2>&1)" && fail "init wrote a config outside /data"
grep -q "outside the data volume" <<<"$out" || fail "outside-volume refusal did not explain itself: $out"
out="$(cli init --json '{"assets":["BTC"],"enableSpot":true,"spotStrategies":["sma_crossover"],"autoUpdate":"daily"}' --output /data/au.json 2>&1)" && fail "init accepted autoUpdate daily"
grep -q "autoUpdate" <<<"$out" || fail "autoUpdate refusal did not explain itself: $out"
files="$(cli_sh 'ls -A /data' | tr '\n' ' ')"
[[ "$files" == "config.json " ]] || fail "refused init runs left files in /data: $files"
ok "init refuses live strategies, outside-volume output and autoUpdate; refusals write nothing"

ivol="${project}_interactive"
ilogs="${project}_interactive_logs"
extra_volumes+=("$ivol" "$ilogs")
docker volume create "$ivol" >/dev/null
docker volume create "$ilogs" >/dev/null
python3 -c 'print("\n" * 300, end="")' | docker run --rm -i --read-only --tmpfs /tmp:size=64m,mode=1777 -v "$ivol:/data" -v "$ilogs:/app/logs" "$IMAGE" init >"$work/interactive.out" 2>&1 || { cat "$work/interactive.out" >&2; fail "interactive init failed"; }
grep -q "Config file: /data/config.json" "$work/interactive.out" || fail "interactive init did not use the --output default"
grep -q "Output config path" "$work/interactive.out" && fail "interactive init still asked for an output path in the container"
icfg="$(docker run --rm -v "$ivol:/data" --entrypoint cat "$IMAGE" /data/config.json)"
python3 -c 'import json,sys; c=json.loads(sys.argv[1]); assert c["db_file"]=="/data/state.db"; assert c.get("auto_update","off")=="off"; assert all("--mode=live" not in s.get("args",[]) for s in c["strategies"])' "$icfg" || fail "interactive init config is wrong"
ok "interactive init writes /data/config.json without an output prompt, paper only"

out="$(docker run --rm "$IMAGE" init --json "$spot_json" 2>&1)" && fail "init ran without a data volume"
grep -q "is not a mounted volume" <<<"$out" || fail "missing-volume refusal did not explain itself: $out"
ok "init without a mounted /data refuses instead of writing to the container layer"

say "existing volume with incompatible permissions"
bvol="${project}_badperm"
blogs="${project}_badperm_logs"
extra_volumes+=("$bvol" "$blogs")
docker volume create "$bvol" >/dev/null
docker volume create "$blogs" >/dev/null
docker run --rm --user 0:0 -v "$bvol:/data" --entrypoint sh "$IMAGE" -c 'touch /data/foreign && chown 0:0 /data /data/foreign && chmod 0755 /data'
out="$(docker run --rm --read-only --tmpfs /tmp:size=64m,mode=1777 -v "$bvol:/data" -v "$blogs:/app/logs" "$IMAGE" init --json "$spot_json" 2>&1)" && fail "init wrote to a root-owned volume"
grep -q "not writable by uid 10001" <<<"$out" || fail "permission refusal lacks the uid: $out"
grep -q "Volume permissions" <<<"$out" || fail "permission refusal lacks the recovery pointer: $out"
bad="${project}-badperm"
extra_containers+=("$bad")
docker run -d --name "$bad" --init --read-only --tmpfs /tmp:size=64m,mode=1777 -v "$bvol:/data" -v "$blogs:/app/logs" \
    --health-interval 2s --health-retries 1 --health-start-period 1s "$IMAGE" >/dev/null
wait_for 30 "permission hold reaches unhealthy" is_health "$bad" unhealthy
[[ "$(docker logs "$bad" 2>&1 | count_matches CRITICAL)" == "1" ]] || fail "permission hold did not log exactly one CRITICAL line"
docker logs "$bad" 2>&1 | has "started daemon" && fail "supervisor started a daemon on a root-owned volume"
docker stop -t 10 "$bad" >/dev/null
[[ "$(docker inspect -f '{{.State.ExitCode}}' "$bad")" == "1" ]] || fail "held supervisor did not exit 1 on stop"
ok "root-owned volume: init refuses with recovery text; supervisor holds unhealthy with one CRITICAL line and stops on SIGTERM"

say "runtime bind and token policy"
out="$(compose run --rm -T -e GO_TRADER_RUNTIME= cli --config /data/config.json --status-bind 0.0.0.0 --once 2>&1)" && fail "host runtime accepted a wildcard status bind"
grep -q "is not a loopback address" <<<"$out" || fail "host bind refusal did not explain itself: $out"
out="$(compose run --rm -T -e GO_TRADER_RUNTIME= cli --config /data/config.json --status-bind 192.0.2.10 --once 2>&1)" && fail "host runtime accepted a non-loopback status bind"
grep -q "is not a loopback address" <<<"$out" || fail "host bind refusal did not explain itself: $out"
out="$(compose run --rm -T cli --config /data/config.json --status-bind 192.0.2.10 --once 2>&1)" && fail "container runtime accepted a specific non-loopback bind"
grep -q "not supported in the container runtime" <<<"$out" || fail "container bind refusal did not explain itself: $out"
out="$(compose run --rm -T -e GO_TRADER_RUNTIME=containers cli version 2>&1)" && fail "an unknown GO_TRADER_RUNTIME value was accepted"
grep -q "is not supported" <<<"$out" || fail "runtime policy refusal did not explain itself: $out"
printf 'STATUS_AUTH_TOKEN=\n' >"$work/go-trader.env"
compose up -d go-trader >/dev/null
wait_for 60 "missing-token refusal" logs_contain "STATUS_AUTH_TOKEN is empty"
code="$(http_code "http://127.0.0.1:$HOST_PORT/health")"
[[ "$code" != "200" ]] || fail "the status server answered without STATUS_AUTH_TOKEN"
compose rm -s -f go-trader >/dev/null
printf 'STATUS_AUTH_TOKEN=%s\n' "$token" >"$work/go-trader.env"
ok "non-loopback bind refused outside the container; wildcard without a token refused before serving; unknown runtime refused"

say "service start, publishing and authentication"
compose up -d --wait --wait-timeout 180 go-trader >/dev/null || fail "service did not become healthy"
published="$(compose port go-trader 8099)"
[[ "$published" == "127.0.0.1:$HOST_PORT" ]] || fail "port published as '$published'"
bindings="$(docker inspect -f '{{json .HostConfig.PortBindings}}' "$(container_id)")"
python3 -c 'import json,sys; b=json.loads(sys.argv[1]); assert all(x["HostIp"]=="127.0.0.1" for v in b.values() for x in v), b' "$bindings" || fail "a port binding is not on host loopback: $bindings"
[[ "$(http_code "http://127.0.0.1:$HOST_PORT/health")" == "200" ]] || fail "/health is not 200 on host loopback"
[[ "$(http_code "http://127.0.0.1:$HOST_PORT/status")" == "401" ]] || fail "/status without a token is not 401"
[[ "$(http_code -H 'Authorization: Bearer wrong' "http://127.0.0.1:$HOST_PORT/status")" == "401" ]] || fail "/status with a wrong token is not 401"
[[ "$(http_code -H "Authorization: Bearer $token" "http://127.0.0.1:$HOST_PORT/status")" == "200" ]] || fail "/status with the token is not 200"
[[ "$(http_code "http://127.0.0.1:$HOST_PORT/api/strategies")" == "401" ]] || fail "/api/strategies without a token is not 401"
[[ "$(http_code "http://127.0.0.1:$HOST_PORT/history")" == "401" ]] || fail "/history without a token is not 401"
[[ "$(http_code -H "Authorization: Bearer $token" "http://127.0.0.1:$HOST_PORT/api/strategies")" == "200" ]] || fail "/api/strategies with the token is not 200"
health_body="$(curl -s "http://127.0.0.1:$HOST_PORT/health")"
grep -q run_evidence <<<"$health_body" && fail "/health without a token exposed run evidence"
[[ "$(http_code -X POST -H 'Content-Type: application/json' -d '{"paused":true}' "http://127.0.0.1:$HOST_PORT/api/strategies/$strategy_id/pause")" == "401" ]] || fail "pause without a token is not 401"
cli_sh 'cat /data/config.json' | has '"paused": true' && fail "an unauthenticated pause changed the config"
[[ "$(http_code -X POST -H "Authorization: Bearer $token" -H 'Content-Type: application/json' -d '{"paused":true}' "http://127.0.0.1:$HOST_PORT/api/strategies/$strategy_id/pause")" == "200" ]] || fail "pause with the token is not 200"
cli_sh 'cat /data/config.json' | has '"paused": true' || fail "the authorized pause did not reach the config"
[[ "$(http_code -X POST -H "Authorization: Bearer $token" -H 'Content-Type: application/json' -d '{"paused":false}' "http://127.0.0.1:$HOST_PORT/api/strategies/$strategy_id/pause")" == "200" ]] || fail "resume with the token is not 200"
ok "published only on 127.0.0.1:$HOST_PORT; reads and mutations need the token; authorized requests work"

hard="$(compose exec -T go-trader sh -c 'id -u; id -g; grep NoNewPrivs /proc/self/status; tr "\0" " " </proc/1/cmdline; echo; touch /app/x 2>/dev/null && echo app-writable || echo app-readonly; touch /app/.venv/x 2>/dev/null && echo venv-writable || echo venv-readonly; touch /data/.w && rm /data/.w && echo data-writable; touch /tmp/.w && echo tmp-writable')"
grep -qx 10001 <<<"$(sed -n 1p <<<"$hard")" || fail "service does not run as uid 10001: $hard"
grep -q 'NoNewPrivs:[[:space:]]*1' <<<"$hard" || fail "no-new-privileges is not set: $hard"
grep -q 'init' <<<"$(sed -n 4p <<<"$hard")" || fail "PID 1 is not the Docker init: $hard"
for want in app-readonly venv-readonly data-writable tmp-writable; do grep -qx "$want" <<<"$hard" || fail "runtime filesystem check '$want' failed: $hard"; done
ok "uid 10001, no-new-privileges, init PID 1, read-only code and venv, writable /data and /tmp"

say "read-only utilities beside the service, singleton refusals"
cli inspect --config /data/config.json --all >/dev/null || fail "inspect beside the service failed"
cli storage-inspect --config /data/config.json >/dev/null || fail "storage-inspect beside the service failed"
cli probe --config /data/config.json >/dev/null || fail "probe beside the service failed"
set +e
start=$SECONDS
cli --config /data/config.json --once >"$work/once-busy.out" 2>&1
rc=$?
set -e
[[ $rc -eq 79 ]] || { cat "$work/once-busy.out" >&2; fail "--once beside the service exited $rc, want 79"; }
((SECONDS - start < 60)) || fail "--once singleton refusal did not return promptly"
second="${project}-second"
extra_containers+=("$second")
compose run -d --name "$second" go-trader >/dev/null
wait_for 60 "second daemon refusal" container_logs_contain "$second" "exited 79"
sleep 8
[[ "$(docker logs "$second" 2>&1 | count_matches 'CRITICAL: daemon')" == "1" ]] || fail "second daemon hold did not log exactly one supervisor CRITICAL line"
[[ "$(docker logs "$second" 2>&1 | count_matches 'started daemon')" == "1" ]] || fail "held supervisor started the daemon again"
docker logs "$second" 2>&1 | has "\[singleton\]" || fail "the daemon's singleton diagnostics are missing from the held container log"
[[ "$(docker inspect -f '{{.State.Running}}' "$second")" == "true" ]] || fail "held container is not running"
docker exec "$second" /app/go-trader healthcheck >/dev/null 2>&1 && fail "held container health check passed"
docker stop -t 10 "$second" >/dev/null
[[ "$(docker inspect -f '{{.State.ExitCode}}' "$second")" == "79" ]] || fail "held second daemon did not exit 79 on stop"
docker rm -f "$second" >/dev/null
ok "inspect, storage-inspect and probe run beside the service; --once returns 79; a second daemon holds with one CRITICAL line and stops"

say "hot reload through SIGHUP"
(cd "$work" && compose cp go-trader:/data/config.json ./config.json >/dev/null)
python3 -c 'import json,sys; p=sys.argv[1]; c=json.load(open(p)); c["interval_seconds"]=1800; open(p,"w").write(json.dumps(c, indent=2)+"\n")' "$work/config.json"
(cd "$work" && compose cp ./config.json go-trader:/data/config.json >/dev/null)
compose run --rm -T --user 0:0 --cap-add CHOWN --cap-add DAC_READ_SEARCH --entrypoint chown cli 10001:10001 /data/config.json
[[ "$(cli_sh 'stat -c %u:%g:%a /data/config.json' | tr -d '\r')" == "10001:10001:600" ]] || fail "the documented config import changed the owner or mode"
cli probe --config /data/config.json >/dev/null || fail "probe failed after the documented config import"
hup
wait_for 60 "allowed reload" logs_contain "interval_seconds: 3600 -> 1800"
logs_contain "forwarding hangup" || fail "supervisor did not log the forwarded SIGHUP"
edit_config 'strategies+={"id":"sma-eth-reload","type":"spot","platform":"binanceus","script":"shared_scripts/check_strategy.py","args":["sma_crossover","ETH/USDT","1h"],"capital":1000,"max_drawdown_pct":10}'
hup
wait_for 60 "incompatible reload rejection" logs_contain "reload rejected; keeping previous config"
edit_config auto_update='"daily"'
hup
wait_for 60 "auto_update reload rejection" logs_contain "reload failed; keeping previous config: container runtime policy"
out="$(cli probe --config /data/config.json 2>&1)" && fail "probe accepted auto_update daily"
grep -q 'auto_update is "daily"' <<<"$out" || fail "probe auto_update refusal did not explain itself: $out"
compose run --rm -T --entrypoint /app/.venv/bin/python3 cli -c 'import json; p="/data/config.json"; c=json.load(open(p)); c["strategies"]=[s for s in c["strategies"] if s["id"]!="sma-eth-reload"]; c.pop("auto_update", None); open(p,"w").write(json.dumps(c, indent=2)+"\n")'
cli probe --config /data/config.json >/dev/null || fail "probe failed with auto_update omitted"
reloads="$(reload_count)"
compose kill -s HUP go-trader >/dev/null
wait_for 60 "reload through docker compose kill -s HUP" reloads_above "$reloads"
compose restart go-trader >/dev/null
wait_for 120 "service healthy after the restart that clears the docker kill stop mark" service_health_is healthy
ok "SIGHUP through PID 1 and through docker compose kill -s HUP reaches the daemon; allowed change applied; incompatible and auto_update reloads keep the running config; omitted auto_update probes clean"

say "intentional restart from the dashboard"
before="$(restart_count)"
params='{"name":"sma_crossover","platform":"binanceus","asset":"ETH","restart":true}'
nonce="$(api -X POST -H 'Content-Type: application/json' -d "{\"action\":\"add-strategy\",\"params\":$params}" "http://127.0.0.1:$HOST_PORT/api/confirm" | json_field nonce)"
[[ -n "$nonce" ]] || fail "dashboard confirm returned no nonce"
resp="$(api -X POST -H 'Content-Type: application/json' -d "{\"nonce\":\"$nonce\",\"params\":$params}" "http://127.0.0.1:$HOST_PORT/api/config/add-strategy")"
grep -q '"ok":true' <<<"$resp" || fail "dashboard add-strategy failed: $resp"
grep -q "docker compose restart\|service restart" <<<"$resp" || fail "restart message unexpected: $resp"
wait_for 90 "restart after the dashboard request" restart_count_at_least $((before + 1))
logs_contain "\[restart\] container restart requested" || fail "restart request line missing"
logs_contain "\[shutdown\] State saved." || fail "state save before the restart is missing"
logs_contain "exited 0" || fail "the daemon did not exit 0 for the restart"
service_logs | has "systemctl" && fail "a restart path invoked or mentioned systemctl"
wait_for 120 "healthy after the dashboard restart" service_health_is healthy
api "http://127.0.0.1:$HOST_PORT/api/strategies" | has '"sma-eth"' || fail "the restart-required strategy is not running after the restart"
ok "dashboard restart drained, saved state, exited 0 and Docker restarted the container (RestartCount $before -> $(restart_count))"

say "crash after healthy startup"
before="$(restart_count)"
pid="$(daemon_pid)"
[[ -n "$pid" ]] || fail "no daemon pid in the supervisor log"
compose exec -T go-trader sh -c "kill -KILL $pid"
wait_for 90 "restart after a daemon crash" restart_count_at_least $((before + 1))
logs_contain "daemon pid $pid exited 137" || fail "supervisor did not pass the crash status through"
wait_for 120 "healthy after the crash restart" service_health_is healthy
ok "daemon SIGKILL exits the container 137 and Docker restarts it"

say "persistence across compose down and up"
cycles="$(api "http://127.0.0.1:$HOST_PORT/status" | json_field cycle_count)"
cfg_sum="$(cli_sh 'sha256sum /data/config.json' | cut -d' ' -f1)"
compose down >/dev/null
compose up -d --wait --wait-timeout 180 go-trader >/dev/null || fail "service did not become healthy after down and up"
after_cycles="$(api "http://127.0.0.1:$HOST_PORT/status" | json_field cycle_count)"
((after_cycles >= cycles)) || fail "cycle count went from $cycles to $after_cycles across down and up"
[[ "$(cli_sh 'sha256sum /data/config.json' | cut -d' ' -f1)" == "$cfg_sum" ]] || fail "config changed across down and up"
cli_sh 'test -s /data/state.db && test -d /data/tuning_runs && test -f /data/ohlcv_cache.sqlite3' || fail "state database or config-adjacent tuning artifacts are missing"
ok "config, state database (cycle count $cycles -> $after_cycles) and tuning artifacts survive compose down and up"

say "graceful stop with an active Python subprocess"
compose stop go-trader >/dev/null
edit_config 'strategies+={"id":"slow-btc","type":"spot","platform":"binanceus","script":"testsupport/slow_check.py","args":["sma_crossover","BTC/USDT","1h"],"capital":1000,"max_drawdown_pct":10}'
compose up -d go-trader >/dev/null
wait_for 120 "slow check subprocess running" slow_checks_running
start=$SECONDS
compose stop go-trader >/dev/null
elapsed=$((SECONDS - start))
((elapsed < 20)) || fail "docker compose stop took ${elapsed}s, at or past the 20s grace period"
[[ "$(docker inspect -f '{{.State.ExitCode}}' "$(container_id)")" == "0" ]] || fail "service exited $(docker inspect -f '{{.State.ExitCode}}' "$(container_id)") on stop"
for line in "forwarding terminated" "Received terminated, draining" "\[shutdown\] State saved." "\[shutdown\] Complete." "exited 0"; do
    service_logs | tail -n 80 | has "$line" || fail "stop log lacks '$line'"
done
ok "docker compose stop drained and saved state in ${elapsed}s with a Python check running"

clean="${project}-cleanup"
extra_containers+=("$clean")
docker run -d --name "$clean" --init --read-only --tmpfs /tmp:size=64m,mode=1777 --user 10001:10001 \
    -v "${project}_data:/data" -v "${project}_logs:/app/logs" -v "$fixtures:/app/testsupport:ro" \
    --env-file "$work/go-trader.env" --entrypoint sleep "$IMAGE" infinity >/dev/null
docker exec -d "$clean" sh -c 'exec /app/go-trader supervise --config /data/config.json >/tmp/supervise.log 2>&1'
in_clean() { docker exec "$clean" sh -c "grep -l -- '$1' /proc/[0-9]*/cmdline 2>/dev/null | wc -l" | tr -d ' \r'; }
clean_slow_running() { (( $(in_clean '[s]low_check') >= 2 )); }
clean_supervisor_gone() { (( $(in_clean '[s]upervise') == 0 )); }
wait_for 120 "slow check under an exec supervisor" clean_slow_running
spid="$(docker exec "$clean" sh -c 'for p in /proc/[0-9]*; do tr "\0" " " <"$p/cmdline" 2>/dev/null | grep -q "^/app/go-trader supervise" && basename "$p"; done' | sed -n 1p)"
[[ -n "$spid" ]] || fail "supervisor pid not found"
docker exec "$clean" sh -c "kill -TERM $spid"
wait_for 30 "supervisor exit" clean_supervisor_gone
[[ "$(in_clean '[s]low_check')" == "0" ]] || fail "Python check processes survived the shutdown"
[[ "$(in_clean '/app/go-trade[r]')" == "0" ]] || fail "a go-trader process survived the shutdown"
sup_log="$(docker exec "$clean" cat /tmp/supervise.log)"
for line in "forwarding terminated" "Received terminated, draining" "State saved." "exited 0"; do
    grep -q -- "$line" <<<"$sup_log" || fail "supervisor log lacks '$line'"
done
docker rm -f "$clean" >/dev/null
ok "SIGTERM to the supervisor forwards, drains, cancels the check process group and leaves no child process"
compose run --rm -T --entrypoint /app/.venv/bin/python3 cli -c 'import json; p="/data/config.json"; c=json.load(open(p)); c["strategies"]=[s for s in c["strategies"] if s["id"]!="slow-btc"]; open(p,"w").write(json.dumps(c, indent=2)+"\n")'

say "fatal exits hold without a restart loop"
hold_check() {
    local code="$1" cid before_rc
    compose up -d go-trader >/dev/null
    cid="$(container_id)"
    wait_for 90 "exit $code hold" container_logs_contain "$cid" "CRITICAL: daemon pid [0-9]* exited $code"
    before_rc="$(restart_count)"
    wait_for 60 "unhealthy while held at $code" service_health_is unhealthy
    sleep 5
    [[ "$(docker logs --since 0 "$cid" 2>&1 | grep -c "CRITICAL: daemon pid [0-9]* exited $code")" == "1" ]] || fail "exit $code: more than one supervisor CRITICAL line"
    [[ "$(restart_count)" == "$before_rc" ]] || fail "exit $code: Docker restarted the held container"
    [[ "$(docker inspect -f '{{.State.Running}}' "$cid")" == "true" ]] || fail "exit $code: held container is not running"
    local start=$SECONDS
    compose stop go-trader >/dev/null
    ((SECONDS - start < 15)) || fail "exit $code: stopping the held container took too long"
    [[ "$(docker inspect -f '{{.State.ExitCode}}' "$cid")" == "$code" ]] || fail "exit $code: held container exited $(docker inspect -f '{{.State.ExitCode}}' "$cid")"
}
cli_sh 'cp /data/config.json /data/config.good'
edit_config 'strategies[0].script="testsupport/fail_probe.py"'
hold_check 78
service_logs | has "fixture check script refuses every probe" || fail "exit 78: probe diagnostics missing"
set +e
cli probe --config /data/config.json >/dev/null 2>&1
rc=$?
set -e
[[ $rc -eq 78 ]] || fail "one-shot probe returned $rc, want 78"
cli_sh 'cp /data/config.good /data/config.json'
edit_config 'paper_db_file="/data/./state.db"'
hold_check 80
service_logs | has "\[storage\] CRITICAL" || fail "exit 80: storage diagnostics missing"
cli_sh 'cp /data/config.good /data/config.json'
ok "exits 78 and 80 hold unhealthy with one CRITICAL line and no restart; one-shot probe returns 78 directly"

say "early failure and Docker restart policy"
edit_config auto_update='"daily"'
compose up -d go-trader >/dev/null
wait_for 60 "startup refusal of auto_update" logs_contain 'auto_update is "daily"'
wait_for 60 "Docker restarting an early failure" restart_count_at_least 2
early_restarts="$(restart_count)"
compose stop go-trader >/dev/null
cli_sh 'cp /data/config.good /data/config.json && rm /data/config.good'
ok "startup refuses auto_update daily (exit 1) and Docker keeps restarting the early failure (RestartCount $early_restarts)"

say "one-shot cycle with the service stopped"
if [[ "$REQUIRE_ONCE" == "1" ]]; then
    cli --config /data/config.json --once >"$work/once.out" 2>&1 || { tail -40 "$work/once.out" >&2; fail "--once with the service stopped failed"; }
    ok "--once succeeds with the service stopped"
else
    echo "NOTE: GO_TRADER_TEST_REQUIRE_ONCE=0; the networked --once cycle is skipped"
fi

say "stopped backup and restore"
cli_sh 'cd /data && find . -type f | LC_ALL=C sort | xargs sha256sum' >"$work/before.sums"
mkdir -p "$work/backups/b1"
compose cp go-trader:/data "$work/backups/b1/data" >/dev/null
cli version --json >"$work/backups/b1/image.json"
edit_config interval_seconds=900
cli_sh 'echo later >/data/later.txt'
compose run --rm -T --entrypoint find cli /data -mindepth 1 -delete
compose cp "$work/backups/b1/data/." go-trader:/data >/dev/null
compose run --rm -T --user 0:0 --cap-add CHOWN --cap-add DAC_READ_SEARCH --entrypoint chown cli -R 10001:10001 /data
cli_sh 'cd /data && find . -type f | LC_ALL=C sort | xargs sha256sum' >"$work/after.sums"
diff -u "$work/before.sums" "$work/after.sums" || fail "restore did not reproduce the backed-up files"
owners="$(cli_sh 'find /data ! -user 10001 | wc -l' | tr -d ' \r')"
[[ "$(cli_sh 'stat -c %a /data' | tr -d '\r')" == "700" ]] || fail "restored /data is not mode 0700"
[[ "$owners" == "0" ]] || fail "restored files are not owned by uid 10001"
grep -q '"source_commit"' "$work/backups/b1/image.json" || fail "backup identity record is missing"
compose up -d --wait --wait-timeout 180 go-trader >/dev/null || fail "service did not become healthy after the restore"
ok "stopped backup and restore reproduce config and every database byte for byte; the restored service is healthy"

compose --profile cli down -v >/dev/null
echo
echo "PASS: container image (identity, clean layers, volume setup, bind and token policy, auth, SIGHUP reload, dashboard and crash restarts, persistence, graceful stop with a Python child, fatal holds 78/79/80, early-failure restarts, one-shot exits, backup and restore)"

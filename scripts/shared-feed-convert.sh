#!/usr/bin/env bash

set -euo pipefail

THIS_SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd "$THIS_SCRIPT_DIR/.." && pwd)
source "${THIS_SCRIPT_DIR}/update_helpers.sh"
UPDATE_UNIT_SUDO=""

usage() {
    cat <<'EOF'
Usage: scripts/shared-feed-convert.sh <subcommand> [options]

Optional, operator-run conversion of an existing systemd deployment to the
shared market feed (SKILL.md "Shared market feed"). Nothing here runs during
an update: a deployment that never runs this script keeps its market_feed mode.
Run the stages in order; each checks its result, records it in the journal
(/var/lib/go-trader/shared-feed/convert.journal) and can be re-run.

Subcommands:
  plan [--consumer <unit>]...
        Read-only. Lists every scheduler unit and, with --consumer, checks the
        selection and prints the full target. Changes nothing on the host.
  feeds --consumer <unit>... [--per-minute <n> --startup <n>]
        Creates /opt/go-trader-feed-primary (websocket) and
        /opt/go-trader-feed-backup (rest) from the first consumer's tree, writes
        their configs over read-only shadow copies of the consumer configs,
        installs go-trader@feed-primary and go-trader@feed-backup, and waits for
        both to seal. No consumer config changes. A re-run with another
        selection resets the list only while no switched consumer uses the
        feeds; otherwise use consumers.
  calibrate [--per-minute <n> --startup <n>] [--baseline-ledger <file>] [--window <seconds>]
        Measures the backup's peak request use over the window (default two
        periods of the longest cadence) and writes its request_budget: the
        measured values times 1.5, or the fixed operator values. Fixed values
        are refused when the measured peak plus the mids reserve does not fit.
        verify and switch refuse until calibrate passes.
  verify [--accept-differences]
        Both feeds healthy and scripts/feed-source-compare.sh exits 0. A
        difference or an inconclusive result is re-checked once on newer keys;
        differences that remain stop the stage until the operator reviews them
        and re-runs with --accept-differences, which records the count.
  switch --consumer <unit> [--confirm-live <unit>] [--dropin]
        Switches one running consumer to market_feed "shared" and restarts it,
        then requires three sealed primary keys and feed-parity PASS. Any
        failure, interruption or early exit restores the saved config and
        restarts the unit. A unit with a live or
        manual strategy needs --confirm-live <unit> or the unit name typed.
  consumers --consumer <unit>...
        Sets the full list of consumers both feeds serve, for example after a
        paper fold with scripts/merge-paper-instance.sh: adds new units (as a
        shadow until they are switched), drops units no longer named, and
        reloads both feeds. Refuses to drop a running or enabled unit whose
        config is in shared mode. On any failure both feeds get their previous
        list back, checked on each running feed.
  rollback (--consumer <unit> | --all)
        Restores saved consumer configs and restarts the units that run; a
        stopped unit stays stopped. When a config changed after its switch
        (for example a fold), only market_feed and shared_market_feed are
        reverted. --all covers the current consumers and every unit with a
        switch not rolled back, then stops and disables both feed units and
        journals a reset: feeds, calibrate and verify must pass again before
        the next switch.
  status
        Prints the journal, the feed units and each recorded consumer.

Every stage except plan and status holds an exclusive lock while it runs.
Exit status: 0 success, 2 usage, 10-19 plan refusals (18: another stage
runs), 20-29 stage failures, 30 rollback failed.
EOF
}

STATE_DIR="/var/lib/go-trader/shared-feed"
JOURNAL="$STATE_DIR/convert.journal"
SHADOW_DIR="$STATE_DIR/shadow"
FEEDS=(primary backup)
AUDIT_KEYS="${SHARED_FEED_CONVERT_AUDIT_KEYS:-3}"
FAIL_AFTER="${SHARED_FEED_CONVERT_FAIL_AFTER:-}"

log() { echo "[shared-feed] $*" || true; }
warn() { echo "[shared-feed] WARN $*" >&2 || true; }
die() {
    local code="$1"
    shift
    echo "[shared-feed] ERROR $*" >&2 || true
    exit "$code"
}

need_root() {
    [[ $EUID -eq 0 ]] || die 2 "must be run as root (the script reads every consumer config and installs units)"
}

need_tools() {
    local t
    for t in systemctl python3 curl journalctl rsync git; do
        command -v "$t" >/dev/null 2>&1 || die 10 "$t is not on PATH"
    done
}

feed_dir() { printf '/opt/go-trader-feed-%s' "$1"; }
feed_config() { printf '/var/lib/go-trader/feed-%s/config.json' "$1"; }
feed_unit() { printf 'go-trader@feed-%s.service' "$1"; }
feed_socket() { printf '/run/go-trader-feed-%s/feed.sock' "$1"; }
feed_source() { [[ "$1" == "primary" ]] && printf 'websocket' || printf 'rest'; }

journal_add() {
    mkdir -p "$STATE_DIR"
    chmod 0755 "$STATE_DIR"
    touch "$JOURNAL"
    grep -qxF -- "$1" "$JOURNAL" 2>/dev/null || printf '%s\n' "$1" >>"$JOURNAL"
}

journal_last() {
    [[ -f "$JOURNAL" ]] || return 0
    grep -E -- "^$1( |$)" "$JOURNAL" | tail -n 1 || true
}

acquire_lock() {
    command -v flock >/dev/null 2>&1 || die 10 "flock is not on PATH"
    install -d -m 0755 "$STATE_DIR"
    exec 9>>"$STATE_DIR/convert.lock"
    flock -n 9 || die 18 "another shared-feed-convert.sh stage is running (lock $STATE_DIR/convert.lock); wait for it to finish, or check it with 'status'. Nothing changed"
}

journal_stage() {
    [[ -f "$JOURNAL" ]] || return 0
    awk -v p="$1" 'index($0, "reset ") == 1 { last = ""; next } index($0, p " ") == 1 || $0 == p { last = $0 } END { if (last != "") print last }' "$JOURNAL"
}

reset_note() {
    if [[ -n "$(journal_last "$1")" && -z "$(journal_stage "$1")" ]]; then
        printf '; rollback --all stopped the feeds after the last one, so run it again'
    fi
}

journal_field() {
    local line="$1" key="$2" tok
    for tok in $line; do
        if [[ "$tok" == "$key="* ]]; then
            printf '%s' "${tok#"$key="}"
            return 0
        fi
    done
    printf ''
}

PY_HELPER=$(cat <<'PY'
import json
import os
import re
import sys

def load(path):
    with open(path) as f:
        return json.load(f)

def dump(obj, path, mode):
    tmp = path + ".tmp-shared-feed"
    try:
        os.unlink(tmp)
    except FileNotFoundError:
        pass
    owner = None
    try:
        st = os.stat(path)
        owner = (st.st_uid, st.st_gid)
    except FileNotFoundError:
        pass
    fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "w") as f:
        json.dump(obj, f, indent=2, ensure_ascii=False)
        f.write("\n")
        if owner is not None:
            os.fchown(f.fileno(), owner[0], owner[1])
        os.fchmod(f.fileno(), mode)
    os.replace(tmp, path)

def is_live_args(args):
    args = args or []
    for i, a in enumerate(args):
        if a == "--mode=live":
            return True
        if a == "--mode" and i + 1 < len(args) and args[i + 1] == "live":
            return True
    return False

def summary(path):
    c = load(path)
    strategies = c.get("strategies") or []
    live = any(is_live_args(s.get("args")) or s.get("type") == "manual" for s in strategies)
    covered = 0
    for s in strategies:
        script = os.path.basename(s.get("script") or "")
        if (s.get("platform") == "hyperliquid" and s.get("type") in ("perps", "manual")
                and script == "check_hyperliquid.py"):
            covered += 1
    port = c.get("status_port") or 8099
    mode = c.get("market_feed") or "rest"
    print(f"live={'yes' if live else 'no'} strategies={len(strategies)} covered={covered} status_port={int(port)} market_feed={mode} config_version={c.get('config_version')}")

def write_shared(src, dst, primary, backup, mode):
    c = load(src)
    c["market_feed"] = "shared"
    c["shared_market_feed"] = {"primary_socket": primary, "backup_socket": backup}
    dump(c, dst, int(mode, 8))

def write_feed(path, consumer_cfg, source, socket, port, consumers, per_minute, startup):
    base = load(consumer_cfg)
    out = {"role": "feed", "config_version": base.get("config_version"), "status_port": int(port)}
    for key in ("log_level", "discord", "telegram", "alert_throttle_interval"):
        if key in base:
            out[key] = base[key]
    feed = {"source": source, "socket_path": socket, "consumer_configs": json.loads(consumers)}
    if per_minute and int(per_minute) > 0:
        feed["request_budget"] = {"per_minute": int(per_minute), "startup": int(startup)}
    out["feed"] = feed
    out["strategies"] = []
    dump(out, path, 0o600)

def replace_consumer(path, old, new):
    c = load(path)
    lst = c["feed"]["consumer_configs"]
    if new in lst:
        return
    if old not in lst:
        sys.exit(f"{old} is not listed in {path}")
    c["feed"]["consumer_configs"] = [new if p == old else p for p in lst]
    dump(c, path, 0o600)

def set_consumers(path, consumers):
    c = load(path)
    c["feed"]["consumer_configs"] = json.loads(consumers)
    dump(c, path, 0o600)

def is_shared(path, primary, backup):
    c = load(path)
    s = c.get("shared_market_feed") or {}
    ok = (c.get("market_feed") or "").strip() == "shared" and s.get("primary_socket") == primary and s.get("backup_socket") == backup
    print("yes" if ok else "no")

def revert_feed_keys(path, backup_path):
    c = load(path)
    b = load(backup_path)
    for key in ("market_feed", "shared_market_feed"):
        if key in b:
            c[key] = b[key]
        else:
            c.pop(key, None)
    mode = os.stat(path).st_mode & 0o777
    dump(c, path, mode)

def feed_keys_match(path, backup_path):
    c = load(path)
    b = load(backup_path)
    same = all(c.get(k) == b.get(k) for k in ("market_feed", "shared_market_feed"))
    print("yes" if same else "no")

def set_budget(path, per_minute, startup):
    c = load(path)
    c["feed"]["request_budget"] = {"per_minute": int(per_minute), "startup": int(startup)}
    dump(c, path, 0o600)

def get(url_json_path, dotted):
    c = json.load(open(url_json_path))
    for part in dotted.split("."):
        if isinstance(c, dict):
            c = c.get(part)
        else:
            c = None
    if c is None:
        print("")
    elif isinstance(c, bool):
        print("true" if c else "false")
    else:
        print(c)

def feed_consumer_state(status_path, consumer):
    s = json.load(open(status_path))
    for c in s.get("consumers") or []:
        if c.get("path") == consumer:
            ok = c.get("loaded") and not c.get("error") and not c.get("retained")
            print("loaded" if ok else "not_loaded:" + (c.get("error") or ("retained" if c.get("retained") else "not loaded")))
            return
    print("absent")

def ledger_caps(report_path):
    text = open(report_path).read()
    peak = re.search(r"summed steady load:.*?peak=(\d+)", text)
    cold = re.search(r"summed cold start:\s*(\d+)", text)
    print(f"{peak.group(1) if peak else ''} {cold.group(1) if cold else ''}")

op = sys.argv[1]
args = sys.argv[2:]
{
    "summary": summary,
    "write-shared": write_shared,
    "write-feed": write_feed,
    "replace-consumer": replace_consumer,
    "set-consumers": set_consumers,
    "is-shared": is_shared,
    "revert-feed-keys": revert_feed_keys,
    "feed-keys-match": feed_keys_match,
    "set-budget": set_budget,
    "get": get,
    "feed-consumer-state": feed_consumer_state,
    "ledger-caps": ledger_caps,
}[op](*args)
PY
)

py() { python3 -c "$PY_HELPER" "$@"; }

prepare_shadow_dir() {
    install -d -m 0755 "$STATE_DIR"
    install -d -m 0700 -o "$FEED_USER" -g "$FEED_GROUP" "$SHADOW_DIR"
    chown "$FEED_USER:$FEED_GROUP" "$SHADOW_DIR"
    chmod 0700 "$SHADOW_DIR"
}

write_shadow() {
    local cfg="$1" shadow="$2"
    py write-shared "$cfg" "$shadow" "$(feed_socket primary)" "$(feed_socket backup)" 0600
    chown --reference="$SHADOW_DIR" "$shadow"
}

unit_prop() { systemctl show "$1" -p "$2" --value 2>/dev/null | head -n 1 || true; }

unit_config_path() {
    local unit="$1" wd execstart cfg
    wd=$(unit_prop "$unit" WorkingDirectory)
    execstart=$(unit_prop "$unit" ExecStart)
    cfg=$(update_execstart_config_path "$execstart")
    if [[ -z "$cfg" ]]; then
        [[ -n "$wd" ]] || { printf ''; return 0; }
        cfg="${wd%/}/scheduler/config.json"
    elif [[ "$cfg" != /* ]]; then
        cfg="${wd%/}/$cfg"
    fi
    readlink -f "$cfg" 2>/dev/null || printf '%s' "$cfg"
}

unit_user() {
    local u
    u=$(unit_prop "$1" User)
    printf '%s' "${u:-root}"
}

unit_group() {
    local g u
    g=$(unit_prop "$1" Group)
    if [[ -z "$g" ]]; then
        u=$(unit_user "$1")
        g=$(id -gn "$u" 2>/dev/null || printf '%s' "$u")
    fi
    printf '%s' "$g"
}

unit_journal() {
    local unit="$1" since="$2" frag ns
    frag=$(unit_prop "$unit" FragmentPath)
    ns=$(update_unit_log_namespace "$frag")
    if [[ -n "$ns" ]]; then
        journalctl --namespace="+$ns" -u "$unit" --since "$since" -o cat --no-pager 2>/dev/null || true
    else
        journalctl -u "$unit" --since "$since" -o cat --no-pager 2>/dev/null || true
    fi
}

http_to_file() {
    local url="$1" out="$2" token="${3:-}"
    if [[ -n "$token" ]]; then
        printf 'Authorization: Bearer %s\n' "$token" | curl -sS -m 5 -o "$out" -H @- "$url" 2>/dev/null
    else
        curl -sS -m 5 -o "$out" "$url" 2>/dev/null
    fi
}

health_get() {
    local port="$1" field="$2" tmp
    tmp=$(mktemp)
    if http_to_file "http://127.0.0.1:${port}/health" "$tmp"; then
        py get "$tmp" "$field" 2>/dev/null || printf ''
    fi
    rm -f "$tmp"
}

source_fingerprint() {
    local wd="$1"
    (cd "$wd" && find scheduler -maxdepth 1 -type f \( -name '*.go' -o -name 'go.mod' -o -name 'go.sum' \) -print0 \
        | sort -z | xargs -0 sha256sum | sha256sum | awk '{print $1}')
}

binary_has_feed() {
    grep -aq 'feed.request_budget' "$1" 2>/dev/null
}

running_matches_disk() {
    local unit="$1" wd="$2" pid exe_sum disk_sum
    pid=$(unit_prop "$unit" MainPID)
    [[ -n "$pid" && "$pid" != "0" && -e "/proc/$pid/exe" ]] || return 1
    exe_sum=$(sha256sum "/proc/$pid/exe" | awk '{print $1}')
    disk_sum=$(sha256sum "$wd/go-trader" 2>/dev/null | awk '{print $1}')
    [[ -n "$disk_sum" && "$exe_sum" == "$disk_sum" ]]
}

port_in_use() {
    local port="$1"
    if command -v ss >/dev/null 2>&1; then
        ss -ltnH 2>/dev/null | awk '{print $4}' | grep -Eq "[:.]${port}\$"
        return $?
    fi
    return 1
}

scheduler_units() {
    local line unit
    while IFS= read -r line; do
        unit="${line#*|}"
        [[ -n "$unit" ]] || continue
        [[ "$(update_unit_role "$unit")" == "feed" ]] && continue
        printf '%s\n' "$unit"
    done < <(discover_deployment_unit_map) | sort -u
}

declare -a CONSUMERS=()
PORT_primary=""
PORT_backup=""
FEED_USER=""
FEED_GROUP=""

check_selection() {
    local -a units=("$@")
    [[ ${#units[@]} -gt 0 ]] || die 10 "name at least one consumer with --consumer <unit>"
    local known unit user first_fp="" fp wd cfg summary seen=$'\n'
    known=$'\n'"$(scheduler_units)"$'\n'
    FEED_USER=""
    for unit in "${units[@]}"; do
        [[ "$unit" == *.service ]] || unit="${unit}.service"
        case "$seen" in *$'\n'"$unit"$'\n'*) die 10 "$unit is named twice" ;; esac
        seen="${seen}${unit}"$'\n'
        case "$known" in *$'\n'"$unit"$'\n'*) ;; *) die 11 "$unit is not an active go-trader scheduler unit (signal-mode and bare-process deployments are not supported)" ;; esac
        wd=$(unit_prop "$unit" WorkingDirectory)
        cfg=$(unit_config_path "$unit")
        [[ -f "$cfg" ]] || die 11 "$unit: config $cfg does not exist"
        summary=$(py summary "$cfg") || die 11 "$unit: config $cfg does not parse"
        [[ "$(journal_field "$summary" covered)" -gt 0 ]] || die 12 "$unit: no Hyperliquid perps or manual strategy uses check_hyperliquid.py, so the shared feed covers nothing"
        [[ -x "$wd/go-trader" ]] || die 13 "$unit: no binary at $wd/go-trader"
        binary_has_feed "$wd/go-trader" || die 13 "$unit: the binary at $wd/go-trader predates the shared feed with a backup; run bash scripts/update.sh --restart in $wd first"
        running_matches_disk "$unit" "$wd" || die 13 "$unit: the running process does not run $wd/go-trader (restart pending or updated without restart); restart it with bash scripts/update.sh --restart first"
        fp=$(source_fingerprint "$wd")
        if [[ -z "$first_fp" ]]; then
            first_fp="$fp"
        elif [[ "$fp" != "$first_fp" ]]; then
            die 14 "$unit: its scheduler source differs from the first consumer's; update every consumer to the same release first"
        fi
        user=$(unit_user "$unit")
        if [[ -z "$FEED_USER" ]]; then
            FEED_USER="$user"
            FEED_GROUP=$(unit_group "$unit")
        elif [[ "$user" != "$FEED_USER" ]]; then
            die 15 "$unit runs as $user but another consumer runs as $FEED_USER; the feed sockets need one user"
        fi
        CONSUMERS+=("$unit")
    done
    id "$FEED_USER" >/dev/null 2>&1 || die 15 "consumer user $FEED_USER does not exist"
    for unit in "${CONSUMERS[@]}"; do
        cfg=$(unit_config_path "$unit")
        if [[ "$FEED_USER" != "root" ]] && ! runuser -u "$FEED_USER" -- test -r "$cfg" 2>/dev/null; then
            die 15 "$FEED_USER cannot read $cfg; the feed must read each consumer config after the switch"
        fi
    done
}

pick_ports() {
    local name port used rec
    for name in "${FEEDS[@]}"; do
        rec=$(journal_last "port $name")
        if [[ -n "$rec" ]]; then
            printf -v "PORT_$name" '%s' "${rec##* }"
            continue
        fi
        port=$([[ "$name" == "primary" ]] && echo 8190 || echo 8191)
        used=" "
        local unit
        for unit in $(scheduler_units); do
            used="${used}$(journal_field "$(py summary "$(unit_config_path "$unit")" 2>/dev/null || true)" status_port) "
        done
        [[ "$name" == "backup" ]] && used="${used}${PORT_primary} "
        while [[ "$used" == *" $port "* ]] || port_in_use "$port"; do
            port=$((port + 1))
        done
        printf -v "PORT_$name" '%s' "$port"
    done
}

print_inventory() {
    local unit cfg wd summary ver pid
    log "scheduler units on this host:"
    while IFS= read -r unit; do
        [[ -n "$unit" ]] || continue
        cfg=$(unit_config_path "$unit")
        wd=$(unit_prop "$unit" WorkingDirectory)
        summary=$(py summary "$cfg" 2>/dev/null || echo "config unreadable")
        ver=$(health_get "$(journal_field "$summary" status_port)" version)
        pid=$(unit_prop "$unit" MainPID)
        log "  $unit user=$(unit_user "$unit") dir=$wd config=$cfg pid=$pid version=${ver:-unknown} $summary"
    done < <(scheduler_units)
}

run_probe() {
    local tmp="$1" binary="$2" name="$3"
    (cd "$tmp" && "$binary" probe --config "$tmp/feed-$name.json")
}

build_temp_probe() {
    local tmp="$1" unit cfg
    local -a shadows=()
    for unit in "${CONSUMERS[@]}"; do
        cfg=$(unit_config_path "$unit")
        py write-shared "$cfg" "$tmp/${unit%.service}.json" "$(feed_socket primary)" "$(feed_socket backup)" 0600
        shadows+=("$tmp/${unit%.service}.json")
    done
    local list
    list=$(printf '%s\n' "${shadows[@]}" | python3 -c 'import json,sys; print(json.dumps([l.strip() for l in sys.stdin if l.strip()]))')
    python3 - "$tmp/feed-backup.json" "$list" <<'PY'
import json, sys
json.dump({"role": "feed", "status_port": 8191, "feed": {"source": "rest", "socket_path": "/run/go-trader-feed-backup/feed.sock",
           "consumer_configs": json.loads(sys.argv[2]), "request_budget": {"per_minute": 1000, "startup": 1000}}, "strategies": []},
          open(sys.argv[1], "w"), indent=2)
PY
}

cmd_plan() {
    need_root
    need_tools
    print_inventory
    [[ ${#SELECTED[@]} -gt 0 ]] || { log "name consumers with --consumer <unit> to check a selection and print the target"; return 0; }
    check_selection "${SELECTED[@]}"
    pick_ports
    local tmp first_wd probe_out rc unit cfg summary papers=0
    tmp=$(mktemp -d)
    build_temp_probe "$tmp"
    first_wd=$(unit_prop "${CONSUMERS[0]}" WorkingDirectory)
    set +e
    probe_out=$(run_probe "$tmp" "$first_wd/go-trader" backup 2>&1)
    rc=$?
    set -e
    rm -rf "$tmp"
    printf '%s\n' "$probe_out" | sed 's/^/[shared-feed]   /'
    [[ $rc -eq 0 ]] || die 16 "the feed probe over the selected consumers failed (exit $rc)"
    if printf '%s\n' "$probe_out" | grep -Eq 'probe: consumer .*(skipped|: 0 feed strategies)'; then
        die 16 "a selected consumer is skipped or has no feed strategy in the probe above"
    fi
    log "target:"
    local name
    for name in "${FEEDS[@]}"; do
        local portvar="PORT_$name"
        log "  $(feed_unit "$name"): dir=$(feed_dir "$name") config=$(feed_config "$name") source=$(feed_source "$name") socket=$(feed_socket "$name") status_port=${!portvar} user=$FEED_USER"
    done
    if [[ "$FEED_USER" != "go-trader" ]]; then
        log "  feed units get a User=$FEED_USER Group=$FEED_GROUP drop-in, because the consumers run as $FEED_USER"
    fi
    for unit in "${CONSUMERS[@]}"; do
        cfg=$(unit_config_path "$unit")
        summary=$(py summary "$cfg")
        [[ "$(journal_field "$summary" live)" == "no" ]] && papers=$((papers + 1))
        log "  $unit: $cfg gets market_feed \"shared\", primary_socket $(feed_socket primary), backup_socket $(feed_socket backup); restart required$([[ "$(journal_field "$summary" live)" == "yes" ]] && echo "; LIVE: needs --confirm-live $unit")"
    done
    if [[ $papers -gt 1 ]]; then
        log "optional: $papers paper units can be folded into one paper service first with scripts/merge-paper-instance.sh (SKILL.md cutover checklist)"
    fi
    log "plan OK: next run 'feeds' with the same --consumer list"
}

install_feed_unit() {
    local name="$1" unit dropin
    unit=$(feed_unit "$name")
    local src dest="/etc/systemd/system/go-trader@.service"
    src="$(feed_dir "$name")/systemd/go-trader@.service"
    [[ -f "$src" ]] || die 21 "$src is missing after the build"
    update_sync_journal_namespace "$(feed_dir "$name")" "$src" || die 21 "journald namespace setup failed"
    if [[ ! -f "$dest" ]] || ! cmp -s "$src" "$dest"; then
        install -m 0644 "$src" "$dest"
        log "installed $dest"
    fi
    install -d -m 0755 "$(feed_dir "$name")/logs"
    dropin=$(update_unit_dropin_path /etc/systemd/system "$unit" 10-shared-feed-user)
    if [[ "$FEED_USER" != "go-trader" ]]; then
        install -d -m 0755 "$(dirname "$dropin")"
        printf '[Service]\nUser=%s\nGroup=%s\n' "$FEED_USER" "$FEED_GROUP" >"$dropin"
        chmod 0644 "$dropin"
        journal_add "feed-dropin $name $dropin"
    fi
    chown -R "$FEED_USER:$FEED_GROUP" "$(feed_dir "$name")/logs" "$(dirname "$(feed_config "$name")")"
    systemctl daemon-reload
    systemctl enable "$unit" >/dev/null
}

wait_feed_serving() {
    local name="$1" timeout="$2" port="$3" waited=0 status serving key
    while [[ $waited -lt $timeout ]]; do
        status=$(health_get "$port" status)
        serving=$(health_get "$port" serving)
        key=$(health_get "$port" last_seal_key)
        if [[ "$status" == "ok" && "$serving" == "true" && -n "$key" && "$key" != "0" ]]; then
            log "$(feed_unit "$name") sealed key $key"
            return 0
        fi
        sleep 5
        waited=$((waited + 5))
    done
    return 1
}

cmd_feeds() {
    need_root
    need_tools
    check_selection "${SELECTED[@]}"
    pick_ports
    local first_wd first_cfg origin unit cfg shadow name
    first_wd=$(unit_prop "${CONSUMERS[0]}" WorkingDirectory)
    first_cfg=$(unit_config_path "${CONSUMERS[0]}")
    origin=$(git -C "$first_wd" remote get-url origin 2>/dev/null || true)
    [[ -n "$origin" ]] || die 20 "$first_wd has no git origin; the feed deployments clone it so update.sh can update them later"
    prepare_shadow_dir
    local -a entries=()
    local entry
    for unit in "${CONSUMERS[@]}"; do
        entry=$(consumer_entry "$unit")
        entries+=("$entry")
    done
    local list per_minute startup keys list_changed=0
    local -a relist=()
    list=$(printf '%s\n' "${entries[@]}" | python3 -c 'import json,sys; print(json.dumps([l.strip() for l in sys.stdin if l.strip()]))')
    for name in "${FEEDS[@]}"; do
        local dir config portvar="PORT_$name"
        dir=$(feed_dir "$name")
        config=$(feed_config "$name")
        journal_add "port $name ${!portvar}"
        if [[ ! -d "$dir/.git" ]]; then
            [[ ! -e "$dir" ]] || die 20 "$dir exists but is not a git checkout; remove it or finish it by hand"
            git clone --quiet "$origin" "$dir"
        fi
        install -d -m 0755 "$(dirname "$config")"
        per_minute=""
        startup=""
        if [[ "$name" == "backup" ]]; then
            local rec
            rec=$(journal_last "budget")
            if [[ -n "$rec" ]]; then
                per_minute=$(journal_field "$rec" per_minute)
                startup=$(journal_field "$rec" startup)
            elif [[ -n "$OPT_PER_MINUTE" ]]; then
                per_minute="$OPT_PER_MINUTE"
                startup="$OPT_STARTUP"
            else
                per_minute=1000
                startup=1000
            fi
        fi
        if [[ -f "$config" && -n "$(journal_last "feed $name")" ]]; then
            if [[ "$name" == "backup" && -n "$OPT_PER_MINUTE" && "$(feed_budget_matches "$config" "$OPT_PER_MINUTE" "$OPT_STARTUP")" != "yes" ]]; then
                die 20 "$config exists with another request budget; --per-minute and --startup apply only to a new feed, and 'calibrate --per-minute $OPT_PER_MINUTE --startup $OPT_STARTUP' changes an existing one. Nothing changed for the backup"
            fi
            case "$(feed_list_state "$config" "$list")" in
                same)
                    log "$config exists from an earlier feeds run; keeping it"
                    ;;
                shadows)
                    py set-consumers "$config" "$list"
                    list_changed=1
                    relist+=("$name")
                    log "$config exists; no switched consumer uses it, so its consumer list is set to this selection"
                    ;;
                *)
                    die 20 "$config serves a different consumer list that includes switched consumers; change the list with 'consumers --consumer <unit>...'. Nothing changed for $(feed_unit "$name")"
                    ;;
            esac
        else
            py write-feed "$config" "$first_cfg" "$(feed_source "$name")" "$(feed_socket "$name")" "${!portvar}" "$list" "$per_minute" "$startup"
        fi
        [[ "$name" == "backup" && -z "$(journal_last budget)" ]] && journal_add "budget provisional per_minute=$per_minute startup=$startup"
        ln -sfn "$config" "$dir/scheduler/config.json"
        local envfile
        envfile=$(update_systemd_envfile_check_path "$(unit_prop "${CONSUMERS[0]}" EnvironmentFiles)")
        if [[ ! -f "$dir/.env" ]]; then
            (
                umask 077
                if [[ -n "$envfile" && -f "$envfile" ]]; then
                    grep -E '^(export[[:space:]]+)?(DISCORD_|TELEGRAM_)' "$envfile" >"$dir/.env" || true
                else
                    : >"$dir/.env"
                fi
            )
            chmod 0600 "$dir/.env"
        fi
        log "building $dir from $first_wd"
        (cd "$dir" && bash scripts/update.sh --rsync-from "$first_wd") || die 22 "update.sh --rsync-from failed in $dir"
        [[ "$(source_fingerprint "$dir")" == "$(source_fingerprint "$first_wd")" ]] || die 22 "$dir source differs from $first_wd after the build"
        [[ "$FEED_USER" == "root" ]] || chown -R "$FEED_USER:$FEED_GROUP" "$dir"
        install_feed_unit "$name"
        if ! systemctl is-active --quiet "$(feed_unit "$name")"; then
            systemctl start "$(feed_unit "$name")"
        elif [[ " ${relist[*]} " == *" $name "* ]]; then
            reload_feed "$name" || die 23 "$(feed_unit "$name") published no new generation after SIGHUP with the new consumer list"
        fi
        journal_add "feed $name dir=$dir config=$config unit=$(feed_unit "$name") installed at=$(date -u +%s)"
    done
    for name in "${FEEDS[@]}"; do
        local portvar="PORT_$name"
        wait_feed_serving "$name" "${SHARED_FEED_CONVERT_SEAL_TIMEOUT:-900}" "${!portvar}" \
            || die 23 "$(feed_unit "$name") did not seal a key; see $(update_journalctl_unit_command "$(feed_unit "$name")" go-trader)"
        local st
        for entry in "${entries[@]}"; do
            st=$(wait_feed_loaded "$name" "$entry") || die 23 "$(feed_unit "$name") did not load $entry ($st)"
        done
    done
    keys=$(run_probe_live primary | grep -c '^probe: key ' || true)
    journal_add "feeds ready keys=$keys at=$(date -u +%s)"
    if [[ -z "$(journal_last "consumers-set")" || $list_changed -eq 1 ]]; then
        journal_add "consumers-set units=$(IFS=,; echo "${CONSUMERS[*]}") at=$(date -u +%s)"
    fi
    log "feeds OK: both feeds seal from shadow configs; no consumer changed. Next: calibrate"
}

consumer_entry() {
    local unit="$1" cfg shadow
    cfg=$(unit_config_path "$unit")
    shadow="$SHADOW_DIR/${unit%.service}.json"
    journal_add "consumer $unit config=$cfg shadow=$shadow"
    if [[ "$(py is-shared "$cfg" "$(feed_socket primary)" "$(feed_socket backup)")" == "yes" ]]; then
        printf '%s' "$cfg"
    else
        write_shadow "$cfg" "$shadow"
        printf '%s' "$shadow"
    fi
}

feed_list_state() {
    python3 - "$1" "$2" "$SHADOW_DIR" <<'PY'
import json, sys
kept = json.load(open(sys.argv[1]))["feed"]["consumer_configs"]
want = json.loads(sys.argv[2])
if sorted(kept) == sorted(want):
    print("same")
elif all(p.startswith(sys.argv[3].rstrip("/") + "/") for p in kept):
    print("shadows")
else:
    print("serving")
PY
}

feed_budget_matches() {
    python3 - "$1" "$2" "$3" <<'PY'
import json, sys
b = json.load(open(sys.argv[1]))["feed"].get("request_budget") or {}
print("yes" if str(b.get("per_minute")) == sys.argv[2] and str(b.get("startup")) == sys.argv[3] else "no")
PY
}

run_probe_live() {
    local name="$1"
    (cd "$(feed_dir "$name")" && ./go-trader probe --config "$(feed_config "$name")" 2>/dev/null) || true
}

max_cadence() {
    run_probe_live backup | sed -n 's/^probe: key .* cadences=//p' | tr ',' '\n' | tr -d 's' | sort -n | tail -n 1
}

reload_feed() {
    local name="$1" port gen_before waited=0 gen
    port=$(feed_port "$name")
    gen_before=$(health_get "$port" generation)
    systemctl kill -s HUP "$(feed_unit "$name")"
    while [[ $waited -lt 180 ]]; do
        sleep 3
        waited=$((waited + 3))
        gen=$(health_get "$port" generation)
        if [[ -n "$gen" && "$gen" != "$gen_before" ]]; then
            return 0
        fi
    done
    return 1
}

current_consumers() {
    local rec units
    rec=$(journal_last "consumers-set")
    if [[ -n "$rec" ]]; then
        units=$(journal_field "$rec" units)
        printf '%s\n' "${units//,/$'\n'}"
        return 0
    fi
    grep -E '^consumer ' "$JOURNAL" 2>/dev/null | awk '{print $2}' | sort -u || true
}

switched_units() {
    [[ -f "$JOURNAL" ]] || return 0
    local unit rec
    for unit in $(grep -E '^switch ' "$JOURNAL" | awk '{print $2}' | sort -u); do
        rec=$(journal_last "switch $unit")
        [[ "$rec" == *" rolled_back "* ]] || printf '%s\n' "$unit"
    done
}

in_current_consumers() {
    local unit="$1" u
    while IFS= read -r u; do
        [[ "$u" == "$unit" ]] && return 0
    done < <(current_consumers)
    return 1
}

wait_feed_loaded() {
    local name="$1" cfg="$2" tmp waited=0 state=""
    tmp=$(mktemp)
    while [[ $waited -lt 60 ]]; do
        http_to_file "http://127.0.0.1:$(feed_port "$name")/status" "$tmp" || true
        state=$(py feed-consumer-state "$tmp" "$cfg" 2>/dev/null || true)
        [[ "$state" == "loaded" ]] && break
        sleep 3
        waited=$((waited + 3))
    done
    rm -f "$tmp"
    [[ "$state" == "loaded" ]] || { printf '%s' "$state"; return 1; }
}

cmd_consumers() {
    need_root
    need_tools
    [[ -n "$(journal_stage "feeds ready")" ]] || die 20 "run 'feeds' first$(reset_note "feeds ready")"
    check_selection "${SELECTED[@]}"
    [[ "$(source_fingerprint "$(feed_dir primary)")" == "$(source_fingerprint "$(unit_prop "${CONSUMERS[0]}" WorkingDirectory)")" ]] \
        || die 14 "the consumers run a different scheduler source than the feeds; update every deployment with update.sh --all --restart first"
    local unit cfg shadow entry name old state
    local -a entries=()
    while IFS= read -r old; do
        [[ -n "$old" ]] || continue
        case $'\n'"$(printf '%s\n' "${CONSUMERS[@]}")"$'\n' in *$'\n'"$old"$'\n'*) continue ;; esac
        cfg=$(journal_field "$(journal_last "consumer $old")" config)
        [[ -n "$cfg" && -f "$cfg" ]] || continue
        [[ "$(py is-shared "$cfg" "$(feed_socket primary)" "$(feed_socket backup)")" == "yes" ]] || continue
        state=$(systemctl is-active "$old" 2>/dev/null || true)
        if [[ "$state" != "inactive" && "$state" != "failed" ]] || systemctl is-enabled --quiet "$old" 2>/dev/null; then
            die 17 "$old is $state$(systemctl is-enabled --quiet "$old" 2>/dev/null && echo ", enabled") and its config is in shared mode on these feeds, so dropping it would leave it with no feed. Name it with --consumer, or roll it back (rollback --consumer $old) or stop and disable it first. Nothing changed"
        fi
        log "  $old: stopped and disabled with a shared-mode config; dropped from the feeds (rollback --consumer $old still restores its config)"
    done < <(current_consumers)
    prepare_shadow_dir
    for unit in "${CONSUMERS[@]}"; do
        entry=$(consumer_entry "$unit")
        entries+=("$entry")
        log "  $unit -> $entry"
    done
    local list keep
    list=$(printf '%s\n' "${entries[@]}" | python3 -c 'import json,sys; print(json.dumps([l.strip() for l in sys.stdin if l.strip()]))')
    keep=$(mktemp -d)
    for name in "${FEEDS[@]}"; do
        cp -p "$(feed_config "$name")" "$keep/$name.json"
    done
    CONSUMERS_KEEP="$keep"
    guard_set consumers
    local failed=""
    for name in "${FEEDS[@]}"; do
        py set-consumers "$(feed_config "$name")" "$list"
        if ! reload_feed "$name"; then
            failed="$(feed_unit "$name") published no new generation after SIGHUP"
            break
        fi
        for entry in "${entries[@]}"; do
            local st
            if ! st=$(wait_feed_loaded "$name" "$entry"); then
                failed="$(feed_unit "$name") did not load $entry ($st)"
                break 2
            fi
        done
        [[ "$FAIL_AFTER" == "consumers-$name" ]] && kill -TERM $$
    done
    [[ -z "$failed" ]] || consumers_fail "$failed"
    guard_clear
    rm -rf "$keep"
    CONSUMERS_KEEP=""
    journal_add "consumers-set units=$(IFS=,; echo "${CONSUMERS[*]}") at=$(date -u +%s)"
    log "consumers OK: both feeds serve ${#CONSUMERS[@]} consumer(s); units no longer listed are dropped from the feeds and from rollback --all"
}

consumers_fail() {
    local failed="$1" keep="$CONSUMERS_KEEP" name
    undo_begin
    local restore_failed="" prev
    for name in "${FEEDS[@]}"; do
        cp -p "$keep/$name.json" "$(feed_config "$name").restore-shared-feed"
        mv -f "$(feed_config "$name").restore-shared-feed" "$(feed_config "$name")"
        if ! systemctl is-active --quiet "$(feed_unit "$name")"; then
            log "$(feed_unit "$name") is not running; it loads its previous consumer list when it starts"
            continue
        fi
        if ! reload_feed "$name"; then
            restore_failed="${restore_failed} $(feed_unit "$name") published no new generation after the restore"
            continue
        fi
        while IFS= read -r prev; do
            [[ -n "$prev" ]] || continue
            local pst
            pst=$(wait_feed_loaded "$name" "$prev") || restore_failed="${restore_failed} $(feed_unit "$name") did not load $prev again ($pst)"
        done < <(python3 -c 'import json,sys; print("\n".join(json.load(open(sys.argv[1]))["feed"]["consumer_configs"]))' "$keep/$name.json")
    done
    rm -rf "$keep"
    CONSUMERS_KEEP=""
    [[ -z "$restore_failed" ]] || die 30 "$failed; the previous feed configs are back on disk, but:${restore_failed}. Check both feeds' /status"
    die 27 "$failed; both feeds loaded their previous consumer list again"
}

feed_port() {
    local rec
    rec=$(journal_last "port $1")
    printf '%s' "${rec##* }"
}

PREV_PER_MINUTE=""
PREV_STARTUP=""

calibrate_fail() {
    undo_begin
    py set-budget "$(feed_config backup)" "$PREV_PER_MINUTE" "$PREV_STARTUP"
    systemctl kill -s HUP "$(feed_unit backup)" || true
    die 24 "$1; the backup keeps its previous budget per_minute=$PREV_PER_MINUTE startup=$PREV_STARTUP"
}

cmd_calibrate() {
    need_root
    [[ -n "$(journal_stage "feeds ready")" ]] || die 20 "run 'feeds' first$(reset_note "feeds ready")"
    local port window cad sampled=0 peak=0 used refused_before refused bootstrap per_minute startup cap_peak="" cap_cold=""
    port=$(feed_port backup)
    cad=$(max_cadence)
    window="${OPT_WINDOW:-$(( ${cad:-300} * 2 ))}"
    PREV_PER_MINUTE=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["feed"]["request_budget"]["per_minute"])' "$(feed_config backup)")
    PREV_STARTUP=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["feed"]["request_budget"]["startup"])' "$(feed_config backup)")
    guard_set calibrate
    if [[ -n "$OPT_PER_MINUTE" ]]; then
        py set-budget "$(feed_config backup)" "$OPT_PER_MINUTE" "$OPT_STARTUP"
        systemctl kill -s HUP "$(feed_unit backup)"
        sleep 5
    fi
    refused_before=$(health_get "$port" request_budget.totals.refused)
    log "sampling the backup's request window for ${window}s (longest cadence ${cad:-unknown}s)"
    while [[ $sampled -lt $window ]]; do
        used=$(health_get "$port" request_budget.window_used)
        [[ -n "$used" && "$used" -gt "$peak" ]] && peak="$used"
        sleep 2
        sampled=$((sampled + 2))
    done
    refused=$(health_get "$port" request_budget.totals.refused)
    [[ "${refused:-0}" == "${refused_before:-0}" ]] || calibrate_fail "the backup refused $((refused - refused_before)) request(s) during calibration; raise the budget and re-run"
    bootstrap=$(health_get "$port" request_budget.totals.by_reason.bootstrap)
    bootstrap="${bootstrap:-0}"
    if [[ -n "$OPT_PER_MINUTE" ]]; then
        per_minute="$OPT_PER_MINUTE"
        startup="$OPT_STARTUP"
        [[ $((peak + 1)) -lt $per_minute ]] || calibrate_fail "measured peak $peak plus the mids reserve does not fit per_minute $per_minute"
        [[ $bootstrap -le $startup ]] || warn "bootstrap used $bootstrap requests, above startup $startup; the rest came from the per-minute window"
    else
        per_minute=$(( (peak * 3 + 1) / 2 ))
        [[ $per_minute -ge $((peak + 2)) ]] || per_minute=$((peak + 2))
        startup=$(( (bootstrap * 3 + 1) / 2 ))
        [[ $startup -ge 1 ]] || startup=1
    fi
    if [[ -n "$OPT_LEDGER" ]]; then
        local report
        report=$(mktemp)
        python3 "$REPO_ROOT/scripts/hl-request-ledger.py" report --ledger "$OPT_LEDGER" >"$report" || calibrate_fail "hl-request-ledger.py report failed for $OPT_LEDGER"
        read -r cap_peak cap_cold < <(py ledger-caps "$report")
        rm -f "$report"
        [[ -n "$cap_peak" ]] || calibrate_fail "no summed steady peak in the ledger report"
        [[ $((peak + 2)) -le $cap_peak ]] || calibrate_fail "the backup needs $((peak + 2)) requests per minute, above today's measured peak $cap_peak"
        [[ $per_minute -le $cap_peak ]] || per_minute=$cap_peak
        if [[ -n "$cap_cold" && $startup -gt $cap_cold ]]; then startup=$cap_cold; fi
    fi
    py set-budget "$(feed_config backup)" "$per_minute" "$startup"
    systemctl kill -s HUP "$(feed_unit backup)"
    sleep 5
    [[ "$(health_get "$port" request_budget.per_minute)" == "$per_minute" ]] || calibrate_fail "the backup did not apply per_minute $per_minute after SIGHUP"
    journal_add "budget calibrated per_minute=$per_minute startup=$startup peak=$peak bootstrap=$bootstrap baseline_peak=${cap_peak:-none} fixed=$([[ -n "$OPT_PER_MINUTE" ]] && echo yes || echo no) at=$(date -u +%s)"
    guard_clear
    log "calibrate OK: per_minute=$per_minute startup=$startup (measured peak $peak, bootstrap $bootstrap). Next: verify"
}

common_retained_keys() {
    local tp tb
    tp=$(mktemp)
    tb=$(mktemp)
    "$(feed_dir primary)/go-trader" feed-fetch --socket "$(feed_socket primary)" --describe >"$tp" 2>/dev/null || true
    "$(feed_dir primary)/go-trader" feed-fetch --socket "$(feed_socket backup)" --describe >"$tb" 2>/dev/null || true
    python3 - "$tp" "$tb" <<'PY' || true
import json, sys
try:
    a = set(json.load(open(sys.argv[1]))["describe"]["retained_keys"])
    b = set(json.load(open(sys.argv[2]))["describe"]["retained_keys"])
except Exception:
    sys.exit(0)
for k in sorted(a & b):
    print(k)
PY
    rm -f "$tp" "$tb"
}

budget_calibrated() {
    [[ "$(journal_stage "budget")" == "budget calibrated "* ]]
}

cmd_verify() {
    need_root
    [[ -n "$(journal_stage "feeds ready")" ]] || die 20 "run 'feeds' first$(reset_note "feeds ready")"
    budget_calibrated || die 20 "the backup request budget is not calibrated; run 'calibrate' first$(reset_note budget)"
    local name port rc
    for name in "${FEEDS[@]}"; do
        port=$(feed_port "$name")
        [[ "$(health_get "$port" status)" == "ok" && "$(health_get "$port" serving)" == "true" ]] || die 25 "$(feed_unit "$name") is not healthy and serving"
    done
    local attempt after="" wait_s diffs="" accepted=0
    local -a key_args=()
    for attempt in 1 2; do
        if [[ -n "$after" ]]; then
            key_args=()
            local k
            for k in $(common_retained_keys); do
                [[ $k -gt $after ]] && key_args+=(--key "$k")
            done
            [[ ${#key_args[@]} -gt 0 ]] || die 25 "no new common key was sealed by both feeds after ${wait_s:-300}s"
        fi
        local out
        out=$(mktemp)
        set +e
        bash "$REPO_ROOT/scripts/feed-source-compare.sh" --binary "$(feed_dir primary)/go-trader" \
            --primary-socket "$(feed_socket primary)" --backup-socket "$(feed_socket backup)" "${key_args[@]}" | tee "$out"
        rc=${PIPESTATUS[0]}
        set -e
        diffs=$(sed -n 's/.* \([0-9][0-9]*\) difference(s)$/\1/p' "$out" | tail -n 1)
        rm -f "$out"
        [[ $rc -eq 0 ]] && break
        if [[ $rc -eq 3 && $attempt -eq 2 ]]; then
            [[ "$OPT_ACCEPT_DIFF" == "1" ]] || die 25 "the sources still differ on newer keys (${diffs:-?} difference(s) listed above). Review them; a venue revision of a closed bar that one source missed is a known cause. Re-run 'verify --accept-differences' to record them and continue"
            accepted="${diffs:-unknown}"
            break
        fi
        [[ ($rc -eq 1 || $rc -eq 3) && $attempt -eq 1 ]] || die 25 "feed-source-compare exited $rc"
        after=$(common_retained_keys | tail -n 1)
        wait_s=$(max_cadence)
        if [[ $rc -eq 3 ]]; then
            log "the sources differ (listed above; a late venue update to the newest closed bar can do this); comparing only newer keys after ${wait_s:-300}s"
        else
            log "comparison inconclusive; comparing only newer keys after ${wait_s:-300}s"
        fi
        sleep $(( ${wait_s:-300} + 10 ))
    done
    journal_add "verify pass accepted_differences=$accepted at=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    log "verify OK. Next: switch --consumer <unit>, paper units first"
}

status_token_for() {
    local unit="$1" envfile
    envfile=$(update_systemd_envfile_check_path "$(unit_prop "$unit" EnvironmentFiles)")
    [[ -n "$envfile" && -f "$envfile" ]] || { printf ''; return 0; }
    sed -n 's/^\(export[[:space:]]\+\)\?STATUS_AUTH_TOKEN=//p' "$envfile" | tail -n 1 | tr -d '"'"'"
}

wait_active_fresh() {
    local unit="$1" old_pid="$2" port="$3" waited=0 pid hpid
    while [[ $waited -lt 180 ]]; do
        sleep 3
        waited=$((waited + 3))
        systemctl is-active --quiet "$unit" || continue
        pid=$(unit_prop "$unit" MainPID)
        [[ -n "$pid" && "$pid" != "0" && "$pid" != "$old_pid" ]] || continue
        hpid=$(health_get "$port" pid)
        [[ "$hpid" == "$pid" ]] && { printf '%s' "$pid"; return 0; }
    done
    return 1
}

restore_consumer() {
    local unit="$1" policy="${2:-if-running}" rec cfg backup shadow pre post port old_pid mode="byte" state run=1
    rec=$(journal_last "switch $unit")
    cfg=$(journal_field "$rec" config)
    backup=$(journal_field "$rec" backup)
    pre=$(journal_field "$rec" pre)
    post=$(journal_field "$rec" post)
    shadow=$(journal_field "$(journal_last "consumer $unit")" shadow)
    [[ -n "$cfg" && -f "$backup" ]] || { warn "$unit: no saved config recorded"; return 1; }
    [[ "$(update_file_fingerprint "$backup")" == "$pre" ]] || { warn "$unit: $backup no longer matches the recorded pre-switch fingerprint"; return 1; }
    if [[ -n "$post" && "$(update_file_fingerprint "$cfg")" != "$post" ]]; then
        mode="keys"
    fi
    if [[ "$mode" == "byte" ]]; then
        cp -p "$backup" "$cfg.restore-shared-feed"
        mv -f "$cfg.restore-shared-feed" "$cfg"
    else
        local kept
        kept="${cfg}.pre-rollback.$(date -u +%Y%m%dT%H%M%SZ)"
        cp -p "$cfg" "$kept"
        py revert-feed-keys "$cfg" "$backup"
        chown --reference="$kept" "$cfg"
        log "$unit: the config changed after the switch (for example a paper fold), so only market_feed and shared_market_feed are reverted; the changed config is kept as $kept"
    fi
    [[ -z "$shadow" ]] || write_shadow "$cfg" "$shadow"
    local dropin
    dropin=$(update_unit_dropin_path /etc/systemd/system "$unit" 50-shared-feed)
    if [[ -f "$dropin" ]]; then
        rm -f "$dropin"
        systemctl daemon-reload
    fi
    local name
    for name in "${FEEDS[@]}"; do
        if [[ -n "$shadow" ]] && python3 -c 'import json,sys; sys.exit(0 if sys.argv[2] in json.load(open(sys.argv[1]))["feed"]["consumer_configs"] else 1)' "$(feed_config "$name")" "$cfg" 2>/dev/null; then
            py replace-consumer "$(feed_config "$name")" "$cfg" "$shadow"
            systemctl is-active --quiet "$(feed_unit "$name")" && systemctl kill -s HUP "$(feed_unit "$name")" || true
        fi
    done
    state=$(systemctl is-active "$unit" 2>/dev/null || true)
    if [[ "$policy" != "always" && ( "$state" == "inactive" || "$state" == "failed" ) ]]; then
        run=0
        log "$unit is $state; its config is restored and the unit stays stopped (no restart, no health wait)"
    fi
    if [[ $run -eq 1 ]]; then
        port=$(journal_field "$(py summary "$cfg")" status_port)
        old_pid=$(unit_prop "$unit" MainPID)
        systemctl restart "$unit"
        wait_active_fresh "$unit" "$old_pid" "$port" >/dev/null || { warn "$unit did not come back healthy after the restore"; return 1; }
    fi
    if [[ "$mode" == "byte" ]]; then
        [[ "$(update_file_fingerprint "$cfg")" == "$pre" ]] || { warn "$unit: restored config fingerprint differs"; return 1; }
    else
        [[ "$(py feed-keys-match "$cfg" "$backup")" == "yes" ]] || { warn "$unit: market_feed keys differ from the saved config after the revert"; return 1; }
    fi
    journal_add "switch $unit config=$cfg backup=$backup pre=$pre rolled_back mode=$mode restarted=$([[ $run -eq 1 ]] && echo yes || echo no) at=$(date -u +%s)"
    if [[ $run -eq 1 ]]; then
        log "$unit restored ($mode) and healthy"
    else
        log "$unit restored ($mode); left stopped"
    fi
}

GUARD_KIND=""
GUARD_UNIT=""
CONSUMERS_KEEP=""

guard_set() {
    GUARD_KIND="$1"
    GUARD_UNIT="${2:-}"
    trap 'guard_signal INT' INT
    trap 'guard_signal TERM' TERM
    trap 'guard_signal HUP' HUP
    trap 'guard_exit' EXIT
}

guard_clear() {
    GUARD_KIND=""
    GUARD_UNIT=""
    trap - INT TERM HUP EXIT
}

undo_begin() {
    GUARD_KIND=""
    GUARD_UNIT=""
    trap '' INT TERM HUP PIPE
    trap - EXIT
}

guard_undo() {
    local kind="$GUARD_KIND" unit="$GUARD_UNIT" why="$1"
    undo_begin
    case "$kind" in
        switch) switch_fail "$unit" "$why" ;;
        calibrate) calibrate_fail "$why" ;;
        consumers) consumers_fail "$why" ;;
    esac
}

guard_signal() {
    local sig="$1"
    if [[ "$sig" == "HUP" ]]; then
        exec >>"$STATE_DIR/interrupted.log" 2>&1
        echo "[shared-feed] $(date -u +%Y-%m-%dT%H:%M:%SZ) SIGHUP (terminal lost); undoing the unfinished $GUARD_KIND"
    fi
    [[ -n "$GUARD_KIND" ]] || exit 130
    guard_undo "interrupted by SIG$sig before its checks finished"
}

guard_exit() {
    local rc=$?
    [[ -n "$GUARD_KIND" ]] || exit "$rc"
    guard_undo "the script stopped early (exit $rc) before its checks finished"
}

switch_fail() {
    local unit="$1" why="$2"
    undo_begin
    warn "$unit: $why; restoring the saved config"
    if restore_consumer "$unit" always; then
        die 26 "$unit: switch failed ($why); the unit runs its original config again"
    fi
    die 30 "$unit: switch failed ($why) and the automatic restore also failed; restore $(journal_field "$(journal_last "switch $unit")" backup) by hand"
}

cmd_switch() {
    need_root
    [[ ${#SELECTED[@]} -eq 1 ]] || die 2 "switch takes exactly one --consumer <unit>"
    local unit="${SELECTED[0]}"
    [[ "$unit" == *.service ]] || unit="${unit}.service"
    [[ -n "$(journal_stage "verify pass")" ]] || die 20 "run 'verify' first$(reset_note "verify pass")"
    budget_calibrated || die 20 "the backup request budget is not calibrated; run 'calibrate' first$(reset_note budget)"
    local crec cfg shadow srec
    crec=$(journal_last "consumer $unit")
    [[ -n "$crec" ]] && in_current_consumers "$unit" || die 20 "$unit is not a current consumer; add it with 'consumers' (or 'feeds') first"
    cfg=$(journal_field "$crec" config)
    shadow=$(journal_field "$crec" shadow)
    srec=$(journal_last "switch $unit")
    if [[ "$(py is-shared "$cfg" "$(feed_socket primary)" "$(feed_socket backup)")" == "yes" ]]; then
        if [[ -n "$srec" && "$srec" != *" done" && "$srec" != *" rolled_back "* ]]; then
            die 20 "an earlier switch of $unit stopped before its checks finished and was not undone; run 'rollback --consumer $unit', then switch it again"
        fi
        log "$unit is already in shared mode on these feeds; nothing to do"
        return 0
    fi
    systemctl is-active --quiet "$unit" || die 20 "$unit is not running; switch checks a running unit, so start it first"
    local summary live port old_pid
    summary=$(py summary "$cfg")
    live=$(journal_field "$summary" live)
    port=$(journal_field "$summary" status_port)
    if [[ "$live" == "yes" ]]; then
        if [[ "$OPT_CONFIRM_LIVE" != "$unit" && "$OPT_CONFIRM_LIVE" != "${unit%.service}" ]]; then
            [[ -t 0 ]] || die 2 "$unit has live or manual strategies; pass --confirm-live $unit"
            local typed
            read -r -p "[shared-feed] $unit trades live. Type the unit name to switch it: " typed
            [[ "$typed" == "$unit" || "$typed" == "${unit%.service}" ]] || die 2 "confirmation did not match; nothing changed"
        fi
    fi
    local stamp backup pre post
    stamp=$(date -u +%Y%m%dT%H%M%SZ)
    backup="${cfg}.pre-shared-feed.${stamp}"
    pre=$(update_file_fingerprint "$cfg")
    cp -p "$cfg" "$backup"
    old_pid=$(unit_prop "$unit" MainPID)
    journal_add "switch $unit config=$cfg backup=$backup pre=$pre pid=$old_pid begin"
    guard_set switch "$unit"
    py write-shared "$cfg" "$cfg" "$(feed_socket primary)" "$(feed_socket backup)" "$(stat -c '%a' "$backup")"
    chown --reference="$backup" "$cfg"
    post=$(update_file_fingerprint "$cfg")
    journal_add "switch $unit config=$cfg backup=$backup pre=$pre post=$post pid=$old_pid written"
    local name tmp state waited
    tmp=$(mktemp)
    for name in "${FEEDS[@]}"; do
        py replace-consumer "$(feed_config "$name")" "$shadow" "$cfg" || switch_fail "$unit" "could not repoint $(feed_config "$name")"
        reload_feed "$name" || switch_fail "$unit" "$(feed_unit "$name") published no new generation after SIGHUP"
        waited=0
        state=""
        while [[ $waited -lt 60 ]]; do
            http_to_file "http://127.0.0.1:$(feed_port "$name")/status" "$tmp" || true
            state=$(py feed-consumer-state "$tmp" "$cfg" 2>/dev/null || true)
            [[ "$state" == "loaded" ]] && break
            sleep 3
            waited=$((waited + 3))
        done
        [[ "$state" == "loaded" ]] || switch_fail "$unit" "$(feed_unit "$name") did not load $cfg ($state)"
    done
    rm -f "$tmp"
    [[ "$FAIL_AFTER" == "feeds-reload" ]] && switch_fail "$unit" "SHARED_FEED_CONVERT_FAIL_AFTER=feeds-reload"
    if [[ "$OPT_DROPIN" == "1" ]]; then
        local dropin
        dropin=$(update_unit_dropin_path /etc/systemd/system "$unit" 50-shared-feed)
        install -d -m 0755 "$(dirname "$dropin")"
        printf '[Unit]\nWants=%s %s\nAfter=%s %s\n' "$(feed_unit primary)" "$(feed_unit backup)" "$(feed_unit primary)" "$(feed_unit backup)" >"$dropin"
        chmod 0644 "$dropin"
        systemctl daemon-reload
    fi
    local since new_pid
    since="@$(date +%s)"
    systemctl restart "$unit" || switch_fail "$unit" "systemctl restart failed"
    new_pid=$(wait_active_fresh "$unit" "$old_pid" "$port") || switch_fail "$unit" "the unit did not come back active and healthy with a fresh pid"
    [[ "$FAIL_AFTER" == "restart" ]] && switch_fail "$unit" "SHARED_FEED_CONVERT_FAIL_AFTER=restart"
    local cad timeout sealed=0 bad
    cad=$(max_cadence)
    timeout=$(( (${cad:-300}) * (AUDIT_KEYS + 1) + 120 ))
    waited=0
    log "$unit restarted (pid $new_pid); waiting up to ${timeout}s for $AUDIT_KEYS sealed primary keys"
    while [[ $waited -lt $timeout ]]; do
        sleep 10
        waited=$((waited + 10))
        local lines
        lines=$(unit_journal "$unit" "$since" | grep '^\[feed-audit\]' || true)
        bad=$(printf '%s\n' "$lines" | grep -E 'status=(sealed endpoint=backup|degraded)' | head -n 1 || true)
        [[ -z "$bad" ]] || switch_fail "$unit" "a key was not served by the primary: $bad"
        sealed=$(printf '%s\n' "$lines" | grep -c 'status=sealed endpoint=primary' || true)
        [[ $sealed -ge $AUDIT_KEYS ]] && break
    done
    [[ $sealed -ge $AUDIT_KEYS ]] || switch_fail "$unit" "only $sealed sealed primary key(s) in ${timeout}s"
    [[ "$FAIL_AFTER" == "audit" ]] && switch_fail "$unit" "SHARED_FEED_CONVERT_FAIL_AFTER=audit"
    local parity
    set +e
    parity=$(bash "$REPO_ROOT/scripts/feed-parity.sh" --since "$since" --feed-unit "$(feed_unit primary)" --feed-unit "$(feed_unit backup)" --consumer-unit "$unit" 2>&1)
    local prc=$?
    set -e
    printf '%s\n' "$parity" | tail -n 5 | sed 's/^/[shared-feed]   /'
    [[ $prc -eq 0 ]] || switch_fail "$unit" "feed-parity failed (exit $prc)"
    local token served tmp2
    token=$(status_token_for "$unit")
    tmp2=$(mktemp)
    if http_to_file "http://127.0.0.1:${port}/status" "$tmp2" "$token"; then
        served=$(py get "$tmp2" market_feed.shared.served_by 2>/dev/null || true)
        if [[ -n "$served" && "$served" != "primary" ]]; then
            rm -f "$tmp2"
            switch_fail "$unit" "/status served_by is $served"
        fi
        [[ -n "$served" ]] || warn "$unit /status gave no served_by (status token unreadable?); the journal proof above stands"
    fi
    rm -f "$tmp2"
    journal_add "switch $unit config=$cfg backup=$backup pre=$pre post=$post pid=$new_pid done"
    guard_clear
    log "$unit switched: $sealed sealed primary keys and feed-parity PASS"
}

cmd_rollback() {
    need_root
    local -a units=()
    local unit rec
    if [[ "$OPT_ALL" == "1" ]]; then
        while IFS= read -r unit; do
            [[ -n "$unit" ]] && units+=("$unit")
        done < <(current_consumers)
        local -a orphans=()
        local cfg
        for unit in "${units[@]}"; do
            rec=$(journal_last "switch $unit")
            [[ -n "$rec" && "$rec" != *" rolled_back "* ]] && continue
            cfg=$(journal_field "$(journal_last "consumer $unit")" config)
            [[ -n "$cfg" && -f "$cfg" && "$(py is-shared "$cfg" "$(feed_socket primary)" "$(feed_socket backup)")" == "yes" ]] && orphans+=("$unit")
        done
        [[ ${#orphans[@]} -eq 0 ]] || die 30 "${orphans[*]}: in shared mode with no saved pre-switch config (for example a unit created by a paper fold); set its market_feed by hand before rollback --all, because stopping the feeds would leave it degraded. Nothing changed"
        while IFS= read -r unit; do
            [[ -n "$unit" ]] || continue
            case $'\n'"$(printf '%s\n' "${units[@]}")"$'\n' in *$'\n'"$unit"$'\n'*) ;; *) units+=("$unit") ;; esac
        done < <(switched_units)
    else
        [[ ${#SELECTED[@]} -eq 1 ]] || die 2 "rollback takes --consumer <unit> or --all"
        unit="${SELECTED[0]}"
        [[ "$unit" == *.service ]] || unit="${unit}.service"
        units=("$unit")
    fi
    local failed=0
    for unit in "${units[@]}"; do
        rec=$(journal_last "switch $unit")
        if [[ -z "$rec" || "$rec" == *" rolled_back "* ]]; then
            log "$unit was never switched or is already restored"
            continue
        fi
        restore_consumer "$unit" || failed=1
    done
    [[ $failed -eq 0 ]] || die 30 "at least one consumer could not be restored; see the warnings above"
    if [[ "$OPT_ALL" == "1" ]]; then
        local name
        journal_add "reset rollback_all at=$(date -u +%s)"
        for name in "${FEEDS[@]}"; do
            systemctl disable --now "$(feed_unit "$name")" >/dev/null 2>&1 || true
            journal_add "feed $name stopped_disabled at=$(date -u +%s)"
        done
        log "rollback OK: every current consumer is back on its pre-switch market_feed; both feeds are stopped and disabled"
    fi
}

cmd_status() {
    need_root
    [[ -f "$JOURNAL" ]] && { log "journal $JOURNAL:"; sed 's/^/[shared-feed]   /' "$JOURNAL"; } || log "no journal yet"
    local name port
    for name in "${FEEDS[@]}"; do
        port=$(feed_port "$name")
        [[ -n "$port" ]] || continue
        log "$(feed_unit "$name"): $(systemctl is-active "$(feed_unit "$name")" 2>/dev/null || true) status=$(health_get "$port" status) serving=$(health_get "$port" serving) last_seal_key=$(health_get "$port" last_seal_key) window_used=$(health_get "$port" request_budget.window_used) per_minute=$(health_get "$port" request_budget.per_minute)"
    done
    local unit cfg
    while IFS= read -r unit; do
        [[ -n "$unit" ]] || continue
        cfg=$(journal_field "$(journal_last "consumer $unit")" config)
        log "$unit: $(systemctl is-active "$unit" 2>/dev/null || true) $(py summary "$cfg" 2>/dev/null || echo 'config unreadable')"
    done < <(current_consumers)
}

[[ $# -gt 0 ]] || { usage; exit 2; }
SUBCOMMAND="$1"
shift
declare -a SELECTED=()
OPT_PER_MINUTE=""
OPT_STARTUP=""
OPT_LEDGER=""
OPT_WINDOW=""
OPT_CONFIRM_LIVE=""
OPT_DROPIN=0
OPT_ACCEPT_DIFF=0
OPT_ALL=0
while [[ $# -gt 0 ]]; do
    case "$1" in
        --consumer) SELECTED+=("${2:-}"); shift 2 ;;
        --per-minute) OPT_PER_MINUTE="${2:-}"; shift 2 ;;
        --startup) OPT_STARTUP="${2:-}"; shift 2 ;;
        --baseline-ledger) OPT_LEDGER="${2:-}"; shift 2 ;;
        --window) OPT_WINDOW="${2:-}"; shift 2 ;;
        --confirm-live) OPT_CONFIRM_LIVE="${2:-}"; shift 2 ;;
        --dropin) OPT_DROPIN=1; shift ;;
        --accept-differences) OPT_ACCEPT_DIFF=1; shift ;;
        --all) OPT_ALL=1; shift ;;
        -h|--help) usage; exit 0 ;;
        *) echo "unknown arg: $1" >&2; usage >&2; exit 2 ;;
    esac
done
if [[ -n "$OPT_PER_MINUTE$OPT_STARTUP" ]]; then
    [[ "$OPT_PER_MINUTE" =~ ^[1-9][0-9]*$ && "$OPT_STARTUP" =~ ^[1-9][0-9]*$ ]] || die 2 "--per-minute and --startup must both be positive integers"
fi
[[ -z "$OPT_WINDOW" || "$OPT_WINDOW" =~ ^[1-9][0-9]*$ ]] || die 2 "--window must be a positive number of seconds"

case "$SUBCOMMAND" in
    plan) cmd_plan ;;
    feeds) need_root; acquire_lock; cmd_feeds ;;
    calibrate) need_root; acquire_lock; cmd_calibrate ;;
    verify) need_root; acquire_lock; cmd_verify ;;
    switch) need_root; acquire_lock; cmd_switch ;;
    consumers) need_root; acquire_lock; cmd_consumers ;;
    rollback) need_root; acquire_lock; cmd_rollback ;;
    status) cmd_status ;;
    -h|--help|help) usage ;;
    *) echo "unknown subcommand: $SUBCOMMAND" >&2; usage >&2; exit 2 ;;
esac

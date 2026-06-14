#!/usr/bin/env bash
# Atomic update with pre-flight probe, staging build, atomic binary swap,
# post-restart verification, and rollback (#682). #764: extra go(1) lookup
# paths + ExecStart vs swap-target warning before systemd restart. #766:
# RESTART_MODE=signal (explicit) for bare-process + pidfile deployments.
# #785: systemd mode falls back to signal restart when unit missing (exit 5).
# #790: --rsync-from safe tree sync; warn on missing systemd EnvironmentFile.
#   --rsync-from preserves deployment .git/ (not copied from source) so rollback
#   git reset --hard still targets the deployment repo's pre-sync SHA.
#
# Phases:
#   preflight  — git/uv/go sanity checks
#   rsync      — optional --rsync-from (replaces pull)
#   pull       — git pull --ff-only
#   sync       — uv sync
#   build      — go build to go-trader.new (live binary untouched)
#   probe      — go-trader.new probe against the just-synced Python
#   swap       — atomic mv: live binary -> .prev, staged -> live
#   restart    — systemd unit, or signal/kill + wrapper (explicit or #785 fallback)
#   verify     — wait active (systemd) or /health + PID freshness
#   rollback   — restore .prev on verify failure
#
# Use --restart (or RESTART=1) to restart after a successful build
# AND verify the running process matches the just-built version. Without
# --restart the script stops after swap; the caller (scheduler/updater.go's
# applyUpgrade) handles its own restart via restartSelf.

set -euo pipefail

THIS_SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
THIS_SCRIPT="${THIS_SCRIPT_DIR}/$(basename "${BASH_SOURCE[0]}")"
# shellcheck source=update_helpers.sh
source "${THIS_SCRIPT_DIR}/update_helpers.sh"
orig_argv=("$@")

trim_space() {
    local value="$1"
    value="${value#"${value%%[![:space:]]*}"}"
    value="${value%"${value##*[![:space:]]}"}"
    printf '%s' "$value"
}

restart=0
restart_mode="$(trim_space "${RESTART_MODE:-systemd}")"
restart_mode=$(printf '%s' "$restart_mode" | tr '[:upper:]' '[:lower:]')
restart_uses_signal=0
update_all=0
service_unit="$(trim_space "${GO_TRADER_SERVICE:-}")"
if [[ -z "$service_unit" ]]; then
    service_unit="go-trader"
fi
go_trader_pidfile="$(trim_space "${GO_TRADER_PIDFILE:-./go-trader.pid}")"
go_trader_run_sh="$(trim_space "${GO_TRADER_RUN_SH:-./run.sh}")"
rsync_from=""
tree_mutated=0

while [[ $# -gt 0 ]]; do
    case "$1" in
        --restart)
            restart=1
            shift
            ;;
        --restart-mode)
            if [[ $# -lt 2 ]]; then
                echo "$1 requires systemd or signal" >&2
                exit 2
            fi
            restart_mode="$(printf '%s' "$(trim_space "$2")" | tr '[:upper:]' '[:lower:]')"
            shift 2
            ;;
        --restart-mode=*)
            restart_mode="$(printf '%s' "$(trim_space "${1#*=}")" | tr '[:upper:]' '[:lower:]')"
            shift
            ;;
        --all)
            update_all=1
            shift
            ;;
        --update-all-root)
            if [[ $# -lt 2 ]]; then
                echo "$1 requires a directory path" >&2
                exit 2
            fi
            # Value is applied when processing --all from orig_argv (must parse here so argv is not rejected).
            shift 2
            ;;
        --update-all-root=*)
            shift
            ;;
        --rsync-from)
            if [[ $# -lt 2 ]]; then
                echo "$1 requires a source directory path" >&2
                exit 2
            fi
            rsync_from="$(trim_space "$2")"
            shift 2
            ;;
        --rsync-from=*)
            rsync_from="$(trim_space "${1#*=}")"
            shift
            ;;
        --unit|--service)
            if [[ $# -lt 2 ]]; then
                echo "$1 requires a systemd unit name" >&2
                exit 2
            fi
            unit_arg="$(trim_space "$2")"
            if [[ -z "$unit_arg" || "$unit_arg" == --* ]]; then
                echo "$1 requires a systemd unit name" >&2
                exit 2
            fi
            service_unit="$unit_arg"
            shift 2
            ;;
        --unit=*|--service=*)
            unit_arg="$(trim_space "${1#*=}")"
            if [[ -z "$unit_arg" || "$unit_arg" == --* ]]; then
                echo "${1%%=*} requires a systemd unit name" >&2
                exit 2
            fi
            service_unit="$unit_arg"
            shift
            ;;
        -h|--help)
            echo "Usage: $0 [--restart] [--restart-mode systemd|signal] [--unit <systemd-unit>]"
            echo "       $0 [--restart] [--service <systemd-unit>]"
            echo "       $0 [--rsync-from <source-dir>] [--restart] ..."
            echo "       $0 --all [--restart] [--restart-mode systemd|signal] [--update-all-root <dir>] [...]"
            echo "  --rsync-from <dir>  rsync code from a source clone into this deployment (skips git pull;"
            echo "                      hardcoded exclusions protect .env, config, state DB, venv, binaries)."
            echo "  With --all + systemd: each child inherits GO_TRADER_SERVICE — set per-worktree env if units differ."
            echo "  RESTART=1 env var also enables restart."
            echo "  RESTART_MODE=signal requires Linux, GO_TRADER_RUN_SH, GO_TRADER_PIDFILE (see #766)."
            echo "  systemd mode falls back to signal when the unit is not found (systemctl exit 5)."
            echo ""
            echo "Env overrides:"
            echo "  GO_TRADER_SERVICE=<unit>   systemd unit (default: go-trader; systemd mode only)"
            echo "  GO_TRADER_RUN_SH=<path>    wrapper to respawn (default: ./run.sh; signal mode)"
            echo "  GO_TRADER_PIDFILE=<path>   pidfile written by wrapper (default: ./go-trader.pid)"
            echo "  GO_TRADER_SIGNAL_LOG=<path> append stdout/stderr from wrapper (default: ./go-trader-signal.log)"
            echo "  GO_TRADER_UPDATE_ALL_ROOT=<dir>  parent scanned by --all for go-trader-*/ (default: parent of this repo)"
            echo "  STATUS_PORT=<n>            override /health port (default: read from config, else 8099)"
            echo "  ACTIVE_TIMEOUT=<sec>       systemd is-active or signal SIGTERM wait (default: 30)"
            echo "  HEALTH_TIMEOUT=<sec>       /health version-match poll timeout (default: 60)"
            echo ""
            echo "Go is resolved from: PATH, then /opt/homebrew/bin/go, then /usr/local/go/bin/go"
            exit 0
            ;;
        *)
            echo "unknown arg: $1" >&2
            exit 2
            ;;
    esac
done
if [[ "${RESTART:-0}" == "1" ]]; then
    restart=1
fi

case "$restart_mode" in
    systemd|signal) ;;
    *)
        echo "invalid --restart-mode / RESTART_MODE=$restart_mode (expected systemd or signal)" >&2
        exit 2
        ;;
esac

if [[ "$update_all" == "1" && "$restart" != "1" ]]; then
    echo "--all requires --restart (each instance is restarted after swap)" >&2
    exit 2
fi

if [[ -n "$rsync_from" && ( "$rsync_from" == --* || ! -d "$rsync_from" ) ]]; then
    echo "--rsync-from requires an existing source directory (got: ${rsync_from:-<empty>})" >&2
    exit 2
fi

active_timeout="${ACTIVE_TIMEOUT:-30}"
health_timeout="${HEALTH_TIMEOUT:-60}"
signal_log_out="${GO_TRADER_SIGNAL_LOG:-./go-trader-signal.log}"

phase_name=""
phase_start=0
begin_phase() {
    phase_name="$1"
    phase_start=$SECONDS
    echo "[update] phase: $phase_name"
}
end_phase() {
    local elapsed=$((SECONDS - phase_start))
    echo "[update] phase: $phase_name done (${elapsed}s)"
}
fail() {
    echo "[update] FAIL phase=$phase_name: $*" >&2
    exit 1
}

signal_read_pidfile() {
    local f="$1"
    if [[ ! -f "$f" ]]; then
        printf ''
        return 1
    fi
    local raw
    raw=$(trim_space "$(cat "$f" 2>/dev/null || true)")
    if [[ -z "$raw" ]]; then
        printf ''
        return 1
    fi
    if ! [[ "$raw" =~ ^[1-9][0-9]*$ ]]; then
        printf ''
        return 1
    fi
    printf '%s' "$raw"
    return 0
}

signal_log_proc_snapshot() {
    local pid="$1"
    [[ -d "/proc/$pid" ]] || return 0
    local cwd_line cmd_line
    cwd_line=$(readlink -f "/proc/$pid/cwd" 2>/dev/null || echo "<unread>")
    cmd_line=$(tr '\0' ' ' <"/proc/$pid/cmdline" 2>/dev/null || echo "<unread>")
    echo "[update] signal: pre-kill snapshot pid=$pid cwd=$cwd_line cmdline=${cmd_line:0:500}"
}

signal_wait_pid_exit() {
    local pid="$1"
    local reason_tag="$2"
    local waited=0
    while kill -0 "$pid" 2>/dev/null; do
        if [[ $waited -ge $active_timeout ]]; then
            echo "[update] signal: $reason_tag pid=$pid still alive after ${active_timeout}s — sending SIGKILL" >&2
            kill -KILL "$pid" 2>/dev/null || true
            sleep 1
            return 0
        fi
        sleep 1
        waited=$((waited + 1))
    done
}

signal_launch_wrapper() {
    local run_sh="$1"
    if [[ ! -f "$run_sh" ]]; then
        fail "GO_TRADER_RUN_SH ($run_sh) does not exist"
    fi
    if [[ ! -x "$run_sh" ]]; then
        fail "GO_TRADER_RUN_SH ($run_sh) is not executable"
    fi
    # Detach like a normal nohup deployment; wrapper must write GO_TRADER_PIDFILE with the trader PID.
    setsid nohup bash "$run_sh" >>"$signal_log_out" 2>&1 &
    # Brief yield only; pidfile freshness is enforced by verify / rollback polls (not this sleep).
    sleep 1
}

restart_uses_signal_pid() {
    [[ "$restart_mode" == "signal" || "$restart_uses_signal" == 1 ]]
}

require_signal_restart_prereqs() {
    if [[ ! -d /proc/self ]]; then
        fail "systemd unit missing and signal fallback requires Linux (/proc); install the unit or use --restart-mode signal on Linux with pidfile+run.sh"
    fi
    if [[ ! -f "$go_trader_pidfile" ]]; then
        fail "systemd unit missing and signal fallback requires pidfile ($go_trader_pidfile); start via wrapper once (scripts/create-run-sh.sh) or install the systemd unit"
    fi
    if [[ ! -f "$go_trader_run_sh" ]]; then
        fail "systemd unit missing and signal fallback requires GO_TRADER_RUN_SH ($go_trader_run_sh); create via scripts/create-run-sh.sh or install the systemd unit"
    fi
    if [[ ! -x "$go_trader_run_sh" ]]; then
        fail "systemd unit missing and signal fallback requires executable GO_TRADER_RUN_SH ($go_trader_run_sh)"
    fi
}

ensure_prev_main_pid_for_signal() {
    if [[ -n "$prev_main_pid" ]]; then
        return 0
    fi
    prev_main_pid=$(signal_read_pidfile "$go_trader_pidfile" || true)
    if [[ -n "$prev_main_pid" ]] && kill -0 "$prev_main_pid" 2>/dev/null; then
        signal_log_proc_snapshot "$prev_main_pid"
    fi
}

run_signal_restart() {
    ensure_prev_main_pid_for_signal
    if [[ -n "$prev_main_pid" ]] && kill -0 "$prev_main_pid" 2>/dev/null; then
        echo "[update] signal: SIGTERM old trader pid=$prev_main_pid" >&2
        kill -TERM "$prev_main_pid" 2>/dev/null || true
        signal_wait_pid_exit "$prev_main_pid" "post-swap-old-trader"
    else
        echo "[update] signal: old pid not running before respawn — launching wrapper" >&2
    fi
    signal_launch_wrapper "$go_trader_run_sh"
}

rollback_wait_signal_pidfile() {
    local rb_waited=0
    while [[ $rb_waited -lt $active_timeout ]]; do
        local cur_rb=""
        cur_rb=$(signal_read_pidfile "$go_trader_pidfile" || true)
        if [[ -n "$cur_rb" ]] && kill -0 "$cur_rb" 2>/dev/null; then
            echo "[update] rollback: signal respawn pid=$cur_rb (${rb_waited}s)"
            return 0
        fi
        sleep 1
        rb_waited=$((rb_waited + 1))
    done
    echo "[update] rollback: signal wrapper did not produce a live pid in $go_trader_pidfile within ${active_timeout}s" >&2
    return 1
}

signal_kill_pidfile_process_then_respawn() {
    local pidfile="$1"
    local run_sh="$2"
    local cur=""
    cur=$(signal_read_pidfile "$pidfile" || true)
    if [[ -n "$cur" ]] && kill -0 "$cur" 2>/dev/null; then
        echo "[update] signal: SIGTERM pid=$cur (from $pidfile)" >&2
        kill -TERM "$cur" 2>/dev/null || true
        signal_wait_pid_exit "$cur" "rollback-stop"
    else
        echo "[update] signal: no live pid in $pidfile (cur=${cur:-empty}) — starting wrapper anyway" >&2
    fi
    # The pidfile may not name the failed new process (it can survive on a
    # fallback port); sweep this instance's strays before respawning (#850).
    signal_sweep_stray_instance_procs
    signal_launch_wrapper "$run_sh"
}

execstart_main_binary() {
    local raw="$1"
    raw="${raw//$'\n'/ }"
    if [[ "$raw" == \{*path=* ]]; then
        local p="${raw#*path=}"
        p="${p%% ;*}"
        printf '%s' "$p"
        return 0
    fi
    local b="${raw%% *}"
    b="${b#\"}"
    b="${b%\"}"
    printf '%s' "$b"
}

update_canonicalize_path() {
    local p="$1"
    if [[ "$p" != /* ]]; then
        printf '%s' "$p"
        return 0
    fi
    if command -v realpath >/dev/null 2>&1; then
        if rp=$(realpath "$p" 2>/dev/null); then
            printf '%s' "$rp"
            return 0
        fi
    fi
    if rp=$(readlink -f "$p" 2>/dev/null); then
        printf '%s' "$rp"
        return 0
    fi
    printf '%s' "$p"
}

update_resolve_db_exclude() {
    local db_path="scheduler/state.db"
    if [[ -f scheduler/config.json && -x .venv/bin/python3 ]]; then
        local custom
        custom=$(.venv/bin/python3 -c '
import json
try:
    cfg = json.load(open("scheduler/config.json"))
    p = cfg.get("db_file") or ""
    if isinstance(p, str) and p.strip():
        print(p.strip())
except Exception:
    pass
' 2>/dev/null || true)
        if [[ -n "$custom" ]]; then
            db_path="$custom"
        fi
    fi
    printf '%s' "$db_path"
}

run_rsync_from() {
    local src="$1"
    local dest="$2"
    local db_excl signal_log_excl
    local -a rsync_excludes
    if ! command -v rsync >/dev/null 2>&1; then
        fail "rsync not on PATH — install rsync or omit --rsync-from"
    fi
    db_excl=$(update_resolve_db_exclude)
    signal_log_excl="${GO_TRADER_SIGNAL_LOG:-./go-trader-signal.log}"
    rsync_excludes=(
        --exclude='.git/'
        --exclude='.env'
        --exclude='scheduler/config.json'
        --exclude="${db_excl}*"
        --exclude='trading_bot.db*'
    )
    # Extension-based DB protection (#1012): any *.db (+ SQLite sidecar/lock) at
    # any path survives --delete, even if it isn't the config-resolved db_file.
    # db_excl above stays as belt-and-suspenders for non-.db-suffixed DB paths.
    local db_glob
    while IFS= read -r db_glob; do
        [[ -n "$db_glob" ]] && rsync_excludes+=(--exclude="$db_glob")
    done < <(update_db_rsync_excludes)
    rsync_excludes+=(
        --exclude='.venv/'
        --exclude='node_modules/'
        --exclude='__pycache__/'
        --exclude='go-trader'
        --exclude='go-trader.new'
        --exclude='go-trader.prev'
        --exclude='go-trader.pid'
        --exclude='go-trader-signal.log'
    )
    if [[ "$signal_log_excl" != "./go-trader-signal.log" && "$signal_log_excl" != "go-trader-signal.log" ]]; then
        rsync_excludes+=(--exclude="$signal_log_excl")
    fi
    echo "[update] rsync: $src/ -> $dest/ (excludes deployment .git, secrets, state DB, venv, binaries, signal log)"
    rsync -a --delete "${rsync_excludes[@]}" "$src/" "$dest/"
}

warn_execstart_vs_swap() {
    local unit="$1"
    local exec_line binary swap_abs bin_abs swap_res
    exec_line=$(systemctl show -p ExecStart --value "$unit" 2>/dev/null | head -n 1 || true)
    [[ -n "$exec_line" ]] || return 0
    binary=$(execstart_main_binary "$exec_line")
    [[ -n "$binary" ]] || return 0
    swap_abs="$(pwd)/go-trader"
    if [[ "$binary" != /* ]]; then
        echo "[update] warning: $unit ExecStart does not start with an absolute binary path ($binary); verify manually." >&2
        return 0
    fi
    bin_abs=$(update_canonicalize_path "$binary")
    swap_res=$(update_canonicalize_path "$swap_abs")
    if [[ "$bin_abs" != "$swap_res" ]]; then
        echo "[update] warning: $unit ExecStart binary resolves to ($bin_abs) but this script swapped ($swap_res); restart may not run the binary just installed. Check: systemctl cat $unit | grep ExecStart" >&2
    fi
}

# Return 0 when a systemd unit is active AND its ExecStart binary resolves to
# this deployment's ./go-trader. Used to redirect an explicit signal-mode restart
# through systemctl so we never spawn an out-of-cgroup duplicate (#850). The
# ExecStart match avoids false-positives across sibling worktrees that each run
# their own active unit. Conservative: any unreadable input falls through to 1.
systemd_unit_manages_this_instance() {
    local unit="$1"
    command -v systemctl >/dev/null 2>&1 || return 1
    local active_state exec_line binary bin_abs swap_res
    active_state=$(systemctl is-active "$unit" 2>/dev/null || true)
    exec_line=$(systemctl show -p ExecStart --value "$unit" 2>/dev/null | head -n 1 || true)
    binary=$(execstart_main_binary "$exec_line")
    if [[ "$binary" == /* ]]; then
        bin_abs=$(update_canonicalize_path "$binary")
    else
        bin_abs=""
    fi
    swap_res=$(update_canonicalize_path "$(pwd)/go-trader")
    [[ "$(update_signal_redirect_decision "$active_state" "$bin_abs" "$swap_res")" == "redirect" ]]
}

# Rollback hygiene (#850): SIGTERM (escalating to SIGKILL via signal_wait_pid_exit)
# any go-trader process whose cwd is this deployment dir, i.e. one sharing this
# instance's state DB — e.g. a failed new process still alive on a bindWithFallback
# port. Runs before the wrapper respawn so a rollback cycle ends with exactly one
# live process. cwd-matching spares other worktrees' traders. Linux/signal-only.
signal_sweep_stray_instance_procs() {
    [[ -d /proc ]] || return 0
    local repo_abs
    repo_abs=$(update_canonicalize_path "$(pwd)")
    [[ -n "$repo_abs" ]] || return 0
    local entry pid comm pid_cwd
    for entry in /proc/[0-9]*; do
        pid="${entry#/proc/}"
        kill -0 "$pid" 2>/dev/null || continue
        comm=$(cat "/proc/$pid/comm" 2>/dev/null || true)
        pid_cwd=$(readlink -f "/proc/$pid/cwd" 2>/dev/null || true)
        [[ "$(update_should_sweep_proc "$comm" "$pid_cwd" "$repo_abs")" == "sweep" ]] || continue
        echo "[update] rollback: sweeping stray go-trader pid=$pid (cwd=$pid_cwd) sharing this instance's state DB" >&2
        kill -TERM "$pid" 2>/dev/null || true
        signal_wait_pid_exit "$pid" "rollback-sweep"
    done
    return 0
}

do_rollback() {
    local reason="$1"
    echo "[update] rollback: $reason" >&2
    if [[ ! -x ./go-trader.prev ]]; then
        echo "[update] rollback: no go-trader.prev to restore — service stays on broken binary" >&2
        return
    fi
    mv -f ./go-trader.prev ./go-trader

    if [[ "$tree_mutated" == "1" && -n "$pre_pull_sha" ]]; then
        echo "[update] rollback: reverting git tree to $pre_pull_sha" >&2
        if git reset --hard "$pre_pull_sha" >&2; then
            if ! uv sync >&2; then
                echo "[update] rollback: uv sync FAILED — Python tree may be inconsistent with .prev binary" >&2
            fi
        else
            echo "[update] rollback: git reset --hard FAILED — Python tree still on new SHA, .prev binary may probe-fail" >&2
        fi
    fi

    if restart_uses_signal_pid; then
        signal_kill_pidfile_process_then_respawn "$go_trader_pidfile" "$go_trader_run_sh"
        rollback_wait_signal_pidfile || true
        return
    fi

    local rb_rc=0
    set +e
    sudo systemctl restart "$service_unit"
    rb_rc=$?
    set -e
    if [[ $rb_rc -eq 5 ]]; then
        echo "[update] rollback: systemd unit $service_unit not found — trying signal respawn" >&2
        if [[ ! -d /proc/self ]] || [[ ! -f "$go_trader_pidfile" ]]; then
            echo "[update] rollback: signal fallback unavailable (need Linux + pidfile)" >&2
            return
        fi
        if [[ ! -f "$go_trader_run_sh" ]] || [[ ! -x "$go_trader_run_sh" ]]; then
            echo "[update] rollback: signal fallback unavailable (missing executable $go_trader_run_sh)" >&2
            return
        fi
        restart_uses_signal=1
        signal_kill_pidfile_process_then_respawn "$go_trader_pidfile" "$go_trader_run_sh"
        rollback_wait_signal_pidfile || true
        return
    fi
    if [[ $rb_rc -ne 0 ]]; then
        echo "[update] rollback: systemctl restart failed with exit $rb_rc" >&2
        return
    fi

    local rb_waited=0
    while [[ $rb_waited -lt $active_timeout ]]; do
        if systemctl is-active --quiet "$service_unit"; then
            echo "[update] rollback: previous binary active again (${prev_running_version:-unknown})"
            return
        fi
        sleep 1
        rb_waited=$((rb_waited + 1))
    done
    echo "[update] rollback: previous binary did not reach active within ${active_timeout}s" >&2
}

verify_cur_restart_pid() {
    if restart_uses_signal_pid; then
        signal_read_pidfile "$go_trader_pidfile" || true
    else
        local p
        p=$(systemctl show -p MainPID --value "$service_unit" 2>/dev/null || echo "")
        if [[ "$p" == "0" ]]; then
            printf ''
        else
            printf '%s' "$p"
        fi
    fi
}

# --- begin single-repo update body (also invoked per dir for --all) ---

begin_phase preflight

if ! command -v uv >/dev/null 2>&1; then
    fail "uv not on PATH — install uv first (see CLAUDE.md → Setup)"
fi

go_bin=""
if command -v go >/dev/null 2>&1; then
    go_bin=$(command -v go)
elif [[ -x /opt/homebrew/bin/go ]]; then
    go_bin=/opt/homebrew/bin/go
elif [[ -x /usr/local/go/bin/go ]]; then
    go_bin=/usr/local/go/bin/go
else
    fail "go not on PATH and not found at /opt/homebrew/bin/go or /usr/local/go/bin/go"
fi

repo_root=$(git rev-parse --show-toplevel)
cd "$repo_root"

if [[ "$update_all" == "1" ]]; then
    scan_root="$(trim_space "${GO_TRADER_UPDATE_ALL_ROOT:-}")"
    if [[ -z "$scan_root" ]]; then
        scan_root=$(dirname "$repo_root")
    fi
    declare -a child_args=()
    skip_next=0
    for ((i = 0; i < ${#orig_argv[@]}; i++)); do
        if [[ $skip_next -eq 1 ]]; then
            skip_next=0
            continue
        fi
        a="${orig_argv[$i]}"
        if [[ "$a" == "--all" ]]; then
            continue
        fi
        if [[ "$a" == --update-all-root=* ]]; then
            scan_root="$(trim_space "${a#*=}")"
            continue
        fi
        if [[ "$a" == "--update-all-root" ]]; then
            scan_root="$(trim_space "${orig_argv[$((i + 1))]}")"
            skip_next=1
            continue
        fi
        child_args+=("$a")
    done
    if [[ ! -d "$scan_root" ]]; then
        fail "GO_TRADER_UPDATE_ALL_ROOT / --update-all-root is not a directory: $scan_root"
    fi
    shopt -s nullglob
    all_dirs=( "$scan_root"/go-trader-*/ )
    shopt -u nullglob
    if [[ ${#all_dirs[@]} -eq 0 ]]; then
        fail "no directories matching $scan_root/go-trader-*/ (batch root: $scan_root)"
    fi
    declare -a sorted_dirs=()
    while IFS= read -r line; do
        [[ -n "$line" ]] && sorted_dirs+=("$line")
    done < <(printf '%s\n' "${all_dirs[@]}" | sort -u)
    all_dirs=( "${sorted_dirs[@]}" )
    fail_count=0
    for d in "${all_dirs[@]}"; do
        [[ -d "$d" ]] || continue
        if [[ ! -f "${d}scheduler/config.json" ]]; then
            continue
        fi
        echo "[update] --all: $(cd "$d" && pwd)"
        if (cd "$d" && bash "$THIS_SCRIPT" "${child_args[@]}"); then
            :
        else
            echo "[update] --all: FAILED in $d" >&2
            fail_count=$((fail_count + 1))
        fi
    done
    if [[ $fail_count -ne 0 ]]; then
        fail "--all completed with $fail_count failing instance(s)"
    fi
    echo "[update] --all: all instances OK"
    exit 0
fi

if [[ ! -f scheduler/config.json ]]; then
    cat >&2 <<EOF
[update] scheduler/config.json not found in $(pwd).

update.sh must run from a deployment directory (where scheduler/config.json
exists). scheduler/config.json is gitignored, so a bare source clone has none
and the probe phase would later fail without it.

If this IS your deployment directory, copy scheduler/config.example.json to
scheduler/config.json and fill in API keys (see CLAUDE.md → Setup).

If you are syncing source to multiple deployment instances, build in the
source repo and run scripts/update.sh from each deployment instance.
EOF
    fail "scheduler/config.json missing — refusing to mutate tree from a non-deployment directory"
fi

if [[ -n "$rsync_from" ]]; then
    rsync_from=$(cd "$rsync_from" && pwd)
    if [[ "$(pwd)" == "$rsync_from" ]]; then
        fail "--rsync-from cannot be the deployment directory ($(pwd))"
    fi
fi

pre_pull_sha=$(git rev-parse HEAD 2>/dev/null || echo "")

if [[ "$restart" == "1" && "$restart_mode" == "signal" ]]; then
    if systemd_unit_manages_this_instance "$service_unit"; then
        echo "[update] signal: systemd unit '$service_unit' is active and its ExecStart runs this binary ($(pwd)/go-trader) — routing restart through systemctl to avoid spawning an out-of-cgroup duplicate (#850). Set GO_TRADER_SERVICE to target a different unit, or stop the unit to use signal mode." >&2
        restart_mode="systemd"
    else
        if [[ ! -d /proc/self ]]; then
            fail "RESTART_MODE=signal requires Linux (/proc); use systemd mode on other OSes"
        fi
        if [[ ! -f "$go_trader_pidfile" ]]; then
            fail "signal mode: pidfile missing ($go_trader_pidfile). Start the instance once via your wrapper so it writes the pidfile."
        fi
    fi
fi

if [[ -z "$rsync_from" ]]; then
    build_paths=(
        scheduler
        shared_scripts
        shared_strategies
        shared_tools
        platforms
        backtest
        pyproject.toml
        uv.lock
    )

    if ! git diff --quiet -- "${build_paths[@]}" || ! git diff --cached --quiet -- "${build_paths[@]}"; then
        git status --short -- "${build_paths[@]}" >&2
        fail "working tree has uncommitted changes in build-input paths; commit, stash, or revert first"
    fi
    if ! git diff --quiet || ! git diff --cached --quiet; then
        echo "[update] warning: uncommitted changes outside build-input paths (will survive git pull):" >&2
        git status --short >&2
    fi

    untracked=$(git ls-files --others --exclude-standard \
        scheduler shared_scripts shared_strategies shared_tools platforms backtest 2>/dev/null || true)
    untracked_root=$(git ls-files --others --exclude-standard 2>/dev/null | grep -v '/' || true)
    if [[ -n "$untracked" || -n "$untracked_root" ]]; then
        echo "[update] warning: untracked files (will not affect the build):" >&2
        [[ -n "$untracked" ]] && echo "$untracked" >&2
        [[ -n "$untracked_root" ]] && echo "$untracked_root" >&2
    fi
fi

prev_running_version=""
if [[ -x ./go-trader ]]; then
    prev_running_version=$(./go-trader version 2>/dev/null || echo "")
fi
echo "[update] previous binary version: ${prev_running_version:-<none>}"

prev_main_pid=""
if [[ "$restart" == "1" && "$restart_mode" == "signal" ]]; then
    prev_main_pid=$(signal_read_pidfile "$go_trader_pidfile") || fail "signal mode: could not read valid pid from $go_trader_pidfile"
    if ! kill -0 "$prev_main_pid" 2>/dev/null; then
        echo "[update] signal: warning: pid $prev_main_pid from pidfile is not running — capture is for bookkeeping only" >&2
    fi
    signal_log_proc_snapshot "$prev_main_pid"
    repo_abs=$(update_canonicalize_path "$(pwd)")
    proc_cwd=$(readlink -f "/proc/$prev_main_pid/cwd" 2>/dev/null || true)
    if [[ -n "$proc_cwd" && -n "$repo_abs" && "$proc_cwd" != "$repo_abs" ]]; then
        echo "[update] signal: warning: process cwd ($proc_cwd) != repo root ($repo_abs)" >&2
    fi
else
    # systemd MainPID (or best-effort when --restart off — same capture for restart=0 vs systemd restart=1)
    prev_main_pid=$(systemctl show -p MainPID --value "$service_unit" 2>/dev/null || echo "")
    if [[ "$prev_main_pid" == "0" ]]; then
        prev_main_pid=""
    fi
    # Bare-process deploys may have no unit; keep pidfile pid for #785 signal fallback + verify.
    if [[ -z "$prev_main_pid" && "$restart" == "1" ]]; then
        prev_main_pid=$(signal_read_pidfile "$go_trader_pidfile" || true)
    fi
fi

end_phase

if [[ -n "$rsync_from" ]]; then
    begin_phase rsync
    run_rsync_from "$rsync_from" "$(pwd)"
    tree_mutated=1
    end_phase
else
    begin_phase pull
    # DB protection on the pull path (#1012): a fast-forward only advances tracked
    # files. Every *.db is gitignored (untracked), and git never overwrites or
    # deletes untracked/ignored files on a fast-forward, so state DBs at any path
    # survive `pull --ff-only` unchanged — the guarantee is explicit, not incidental.
    git pull --ff-only
    post_pull_sha=$(git rev-parse HEAD 2>/dev/null || echo "")
    if [[ -n "$pre_pull_sha" && -n "$post_pull_sha" && "$pre_pull_sha" != "$post_pull_sha" ]]; then
        tree_mutated=1
    fi
    end_phase
fi

begin_phase sync
uv sync
end_phase

begin_phase build
if [[ -n "$rsync_from" ]]; then
    ver=$(git -C "$rsync_from" describe --tags --always --dirty=-mod 2>/dev/null || echo dev)
else
    ver=$(git describe --tags --always --dirty=-mod 2>/dev/null || echo dev)
fi
rm -f ./go-trader.new
"$go_bin" -C scheduler build -ldflags "-X main.Version=$ver" -o ../go-trader.new .
if [[ ! -s ./go-trader.new ]]; then
    fail "go build produced empty go-trader.new"
fi
if [[ ! -x ./go-trader.new ]]; then
    fail "go-trader.new is not executable"
fi
echo "[update] built ${ver}: $(stat -c '%s' ./go-trader.new 2>/dev/null || stat -f '%z' ./go-trader.new) bytes"
end_phase

begin_phase probe
if ! ./go-trader.new probe; then
    rm -f ./go-trader.new
    fail "go-trader.new probe rejected the freshly synced Python — refusing to swap"
fi
end_phase

begin_phase swap
rm -f ./go-trader.prev
if [[ -e ./go-trader ]]; then
    mv -f ./go-trader ./go-trader.prev
fi
mv -f ./go-trader.new ./go-trader
end_phase

if [[ "$restart" != "1" ]]; then
    echo "[update] build OK at $ver (skipping restart; pass --restart to enable)"
    exit 0
fi

status_port="${STATUS_PORT:-}"
if [[ -z "$status_port" && -f scheduler/config.json && -x .venv/bin/python3 ]]; then
    status_port=$(.venv/bin/python3 -c '
import json, sys
try:
    cfg = json.load(open("scheduler/config.json"))
    p = cfg.get("status_port") or 0
    if isinstance(p, (int, float)) and int(p) > 0:
        print(int(p))
except Exception:
    pass
' 2>/dev/null || true)
fi
status_port="${status_port:-8099}"

if [[ "$restart_mode" == "systemd" ]]; then
    warn_execstart_vs_swap "$service_unit"
    warn_missing_systemd_environment_files "$service_unit"

    begin_phase restart
    restart_rc=0
    set +e
    sudo systemctl restart "$service_unit"
    restart_rc=$?
    set -e
    if [[ $restart_rc -eq 0 ]]; then
        waited=0
        until systemctl is-active --quiet "$service_unit"; do
            if [[ $waited -ge $active_timeout ]]; then
                do_rollback "systemctl is-active timeout after ${active_timeout}s"
                fail "service did not reach active state within ${active_timeout}s"
            fi
            sleep 1
            waited=$((waited + 1))
        done
        echo "[update] systemd reports active: $service_unit (${waited}s)"
    elif [[ $restart_rc -eq 5 ]]; then
        echo "[update] systemd: unit $service_unit not found (exit $restart_rc) — falling back to signal restart" >&2
        require_signal_restart_prereqs
        restart_uses_signal=1
        run_signal_restart
    else
        fail "systemctl restart $service_unit failed with exit $restart_rc"
    fi
    end_phase
else
    begin_phase restart
    run_signal_restart
    end_phase
fi

begin_phase verify
url="http://localhost:${status_port}/health"
waited=0
verified=0
while [[ $waited -lt $health_timeout ]]; do
    body=$(curl -fsS --max-time 2 "$url" 2>/dev/null || true)
    if [[ -n "$body" ]]; then
        if [[ "$body" == *"\"version\":\"$ver\""* ]]; then
            cur_main_pid=$(verify_cur_restart_pid)
            if [[ -z "$cur_main_pid" ]]; then
                :
            elif [[ -n "$prev_main_pid" && "$prev_main_pid" == "$cur_main_pid" ]]; then
                :
            else
                echo "[update] /health version=$ver OK (pid ${prev_main_pid:-<none>} -> ${cur_main_pid:-<unknown>})"
                verified=1
                break
            fi
        fi
    fi
    sleep 1
    waited=$((waited + 1))
done
if [[ $verified -ne 1 ]]; then
    do_rollback "/health did not report version=$ver with a fresh PID within ${health_timeout}s (last body: ${body:-<empty>}, prev_pid=${prev_main_pid:-<none>})"
    fail "/health verify failed — expected version=$ver and PID != ${prev_main_pid:-<none>}"
fi
end_phase

echo "[update] done: $ver running, previous=${prev_running_version:-<none>} retained as go-trader.prev"

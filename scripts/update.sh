#!/usr/bin/env bash

set -euo pipefail

THIS_SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
THIS_SCRIPT="${THIS_SCRIPT_DIR}/$(basename "${BASH_SOURCE[0]}")"
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
clear_recovery=0
service_unit="$(trim_space "${GO_TRADER_SERVICE:-}")"
if [[ -z "$service_unit" ]]; then
    service_unit="go-trader"
fi
go_trader_pidfile="$(trim_space "${GO_TRADER_PIDFILE:-./go-trader.pid}")"
go_trader_run_sh="$(trim_space "${GO_TRADER_RUN_SH:-./run.sh}")"
rsync_from=""
tree_mutated=0
unit_sync_source_path=""
unit_sync_installed_path=""
unit_sync_backup_path=""
tree_owner=""
tree_owner_snapshot=""
owner_scope=""
probe_owner=""
uv_bin=""
uv_env=()
uv_sync_flags=(--no-dev)
build_export_tree=""
build_export_commit=""
egress_deployment=""
egress_staged=""
egress_switched=0
main_shell_pid="$BASHPID"
coordinator_active=0
coordinator_done=0
update_succeeded=0
mutation_started=0
snapshot_ready=0
recovery_root=""
recovery_key=""
recovery_dir=""
update_lock_held=0
failure_reason=""
caught_signal=""
pre_pull_sha=""
prev_main_pid=""
prev_pid_was_alive=0
prev_running_version=""
pre_update_status_port=""
active_child_pid=""
all_child_pid=""
swap_stage="none"
unit_mutated=0
unit_reloaded=0
journal_mutated=0
egress_controller_mutated=0
egress_state_file=""
restart_attempted=0

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
        --clear-recovery)
            clear_recovery=1
            shift
            ;;
        -h|--help)
            echo "Usage: $0 [--restart] [--restart-mode systemd|signal] [--unit <systemd-unit>]"
            echo "       $0 [--restart] [--service <systemd-unit>]"
            echo "       $0 [--rsync-from <source-dir>] [--restart] ..."
            echo "       $0 --all [--restart] [--restart-mode systemd|signal] [--update-all-root <dir>] [...]"
            echo "       $0 --clear-recovery"
            echo "  --rsync-from <dir>  rsync code from a source clone into this deployment (skips git pull;"
            echo "                      hardcoded exclusions protect .env, config, state DB, venv, binaries)."
            echo "  --clear-recovery    move an incomplete recovery snapshot aside to <key>.cleared-<UTC> and"
            echo "                      remove the incomplete-recovery marker blocking updates of this tree."
            echo "  --all               update+restart every deployment. Batch = union of ACTIVE go-trader"
            echo "                      systemd units' WorkingDirectory (layout-independent), ENABLED units whose"
            echo "                      --config sets role=feed, and the <root>/go-trader-*/ glob. A stopped or"
            echo "                      failed enabled feed is updated and started. Scheduler discovery is"
            echo "                      active-only, so it never starts a stopped/failed scheduler; the glob"
            echo "                      still restarts anything under <root>."
            echo "                      Deployments whose config sets role=feed run first, then schedulers."
            echo "                      A glob dir with no discovered unit maps to an active go-trader@<name>, or an"
            echo "                      enabled role=feed one, whose WorkingDirectory is the dir."
            echo "                      --update-all-root pins the glob root."
            echo "  With --all + systemd: each child resolves GO_TRADER_SERVICE from the active unit that owns"
            echo "                      that deployment's WorkingDirectory (per-dir systemd lookup). Parent"
            echo "                      --unit / --service / GO_TRADER_SERVICE is overridden when a per-dir"
            echo "                      unit exists; if no active unit owns a dir, the parent's service_unit"
            echo "                      is used and a warning is logged."
            echo "  With --restart in systemd mode the shipped unit file for the resolved unit is installed"
            echo "                      over the loaded one when they differ (previous kept as <unit>.prev) and"
            echo "                      systemd is reloaded before the restart. Drop-ins under <unit>.d/ are never"
            echo "                      touched; a unit loaded from outside /etc/systemd/system stops the update."
            echo "  RESTART=1 env var also enables restart."
            echo "  RESTART_MODE=signal requires Linux, GO_TRADER_RUN_SH, GO_TRADER_PIDFILE (see #766)."
            echo "  systemd mode falls back to signal when the unit is not found (systemctl exit 5)."
            echo ""
            echo "Env overrides:"
            echo "  GO_TRADER_SERVICE=<unit>   systemd unit (default: go-trader; systemd mode only)"
            echo "  GO_TRADER_RUN_SH=<path>    wrapper to respawn (default: ./run.sh; signal mode)"
            echo "  GO_TRADER_PIDFILE=<path>   pidfile written by wrapper (default: ./go-trader.pid)"
            echo "  GO_TRADER_SIGNAL_LOG=<path> append stdout/stderr from wrapper (default: ./go-trader-signal.log)"
            echo "  GO_TRADER_UPDATE_ALL_ROOT=<dir>  pin --all to the <dir>/go-trader-*/ glob (skips systemd auto-discovery; default: parent of this repo)"
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

if [[ "$clear_recovery" == "1" && ( "$update_all" == "1" || "$restart" == "1" || -n "$rsync_from" ) ]]; then
    echo "--clear-recovery cannot be combined with --all, --restart, or --rsync-from" >&2
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
    local msg="$*"
    echo "[update] FAIL phase=$phase_name: $msg" >&2
    if [[ "$BASHPID" == "$main_shell_pid" ]]; then
        failure_reason="phase=$phase_name: $msg"
    elif [[ -n "$recovery_dir" && -d "$recovery_dir" ]]; then
        printf 'phase=%s: %s\n' "$phase_name" "$msg" >"$recovery_dir/subshell_failure_reason" 2>/dev/null || true
    fi
    exit 1
}

run_tracked_child() {
    "$@" &
    active_child_pid=$!
    local rc=0
    wait "$active_child_pid" || rc=$?
    active_child_pid=""
    return "$rc"
}

stop_update_children() {
    if [[ -n "$active_child_pid" ]] && kill -0 "$active_child_pid" 2>/dev/null; then
        if command -v pgrep >/dev/null 2>&1; then
            local gpid
            while IFS= read -r gpid; do
                [[ -n "$gpid" ]] || continue
                kill -TERM "$gpid" 2>/dev/null || true
            done < <(pgrep -P "$active_child_pid" 2>/dev/null || true)
        fi
        kill -TERM "$active_child_pid" 2>/dev/null || true
        wait "$active_child_pid" 2>/dev/null || true
    fi
    active_child_pid=""
    if command -v pgrep >/dev/null 2>&1; then
        local cpid
        while IFS= read -r cpid; do
            [[ -n "$cpid" ]] || continue
            kill -TERM "$cpid" 2>/dev/null || true
        done < <(pgrep -P "$main_shell_pid" 2>/dev/null || true)
    fi
}

update_pause_if_requested() {
    local want="$1"
    local spec="${GO_TRADER_UPDATE_PAUSE_AT:-}"
    [[ -n "$spec" ]] || return 0
    local stage="${spec%%:*}"
    local sentinel="${spec#*:}"
    [[ "$stage" == "$want" && -n "$sentinel" ]] || return 0
    printf '%s\n' "$main_shell_pid" >"${sentinel}.reached"
    while [[ ! -e "$sentinel" ]]; do
        sleep 0.1 &
        active_child_pid=$!
        wait "$active_child_pid" 2>/dev/null || true
        active_child_pid=""
    done
}

update_record_intent() {
    local stage="$1"
    mutation_started=1
    if [[ -n "$recovery_dir" && -d "$recovery_dir" ]]; then
        printf '%s\n' "$stage" >>"$recovery_dir/intents.log" 2>/dev/null || true
    fi
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
    setsid nohup bash "$run_sh" >>"$signal_log_out" 2>&1 &
    sleep 1
}

signal_launch_wrapper_nonfatal() {
    local run_sh="$1"
    if [[ ! -f "$run_sh" ]]; then
        echo "[update] recovery: GO_TRADER_RUN_SH ($run_sh) does not exist" >&2
        return 1
    fi
    if [[ ! -x "$run_sh" ]]; then
        echo "[update] recovery: GO_TRADER_RUN_SH ($run_sh) is not executable" >&2
        return 1
    fi
    setsid nohup bash "$run_sh" >>"$signal_log_out" 2>&1 &
    sleep 1
    return 0
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

signal_stop_pidfile_and_stray() {
    local pidfile="$1"
    local cur=""
    cur=$(signal_read_pidfile "$pidfile" || true)
    if [[ -n "$cur" ]] && kill -0 "$cur" 2>/dev/null; then
        echo "[update] signal: SIGTERM pid=$cur (from $pidfile)" >&2
        kill -TERM "$cur" 2>/dev/null || true
        signal_wait_pid_exit "$cur" "rollback-stop"
    else
        echo "[update] signal: no live pid in $pidfile (cur=${cur:-empty})" >&2
    fi
    signal_sweep_stray_instance_procs
}

signal_kill_pidfile_process_then_respawn() {
    local pidfile="$1"
    local run_sh="$2"
    signal_stop_pidfile_and_stray "$pidfile"
    signal_launch_wrapper_nonfatal "$run_sh"
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

run_rsync_from() {
    local src="$1"
    local dest="$2"
    if ! command -v rsync >/dev/null 2>&1; then
        fail "rsync not on PATH — install rsync or omit --rsync-from"
    fi
    update_managed_rsync_excludes "$dest" || fail "could not build managed-set exclusions for $dest"
    local -a rsync_owner=()
    if [[ "$EUID" == "0" ]]; then
        rsync_owner=(--no-owner --no-group)
    fi
    echo "[update] rsync: $src/ -> $dest/ (excludes deployment .git, secrets, state DB, venv, binaries, signal log)"
    run_tracked_child rsync -a ${rsync_owner[@]+"${rsync_owner[@]}"} --delete "${UPDATE_MANAGED_EXCLUDES[@]}" "$src/" "$dest/" \
        || fail "rsync from $src to $dest failed"
    update_pause_if_requested rsync_mid
    if update_should_fail_at rsync_mid; then
        printf 'partial-rsync\n' >"$dest/.update-partial-rsync"
        rm -f "$dest/README.md"
        fail "forced mid-rsync failure after partial transfer"
    fi
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

run_recovery_probe() {
    if update_should_fail_at restore_probe; then
        echo "[update] recovery: forced restore_probe failure" >&2
        return 1
    fi
    if [[ -n "$egress_deployment" ]]; then
        local ep_owner="$probe_owner" line
        [[ -n "$ep_owner" ]] || ep_owner=$(update_unit_owner "$service_unit" 2>/dev/null || true)
        [[ -n "$ep_owner" ]] || return 1
        local -a ep_args=()
        while IFS= read -r line; do
            [[ -n "$line" ]] && ep_args+=("$line")
        done < <(update_egress_probe_args "$service_unit" "$egress_deployment")
        update_probe_as_owner "$repo_root" "$ep_owner" "$repo_root/go-trader" scheduler/config.json "${ep_args[@]}"
        return $?
    fi
    if [[ -n "$probe_owner" ]]; then
        update_probe_as_owner "$repo_root" "$probe_owner" "$repo_root/go-trader" scheduler/config.json
        return $?
    fi
    ./go-trader probe
}

verify_recovery_health() {
    if update_should_fail_at restore_health; then
        echo "[update] recovery: forced restore_health failure" >&2
        return 1
    fi
    local -a ports=()
    if [[ -n "${STATUS_PORT:-}" ]]; then
        ports+=("$STATUS_PORT")
        if [[ -n "$pre_update_status_port" && "$pre_update_status_port" != "$STATUS_PORT" ]]; then
            ports+=("$pre_update_status_port")
        fi
    else
        ports+=("${pre_update_status_port:-8099}")
    fi
    local rec_health_timeout="${RECOVERY_HEALTH_TIMEOUT:-$health_timeout}"
    if [[ "$rec_health_timeout" -lt 30 ]]; then
        rec_health_timeout=30
    fi
    local waited=0 p body cur_main_pid
    while [[ $waited -lt $rec_health_timeout ]]; do
        for p in "${ports[@]}"; do
            body=$(curl -fsS --max-time 2 "http://localhost:${p}/health" 2>/dev/null || true)
            if [[ -n "$body" ]]; then
                if [[ -z "$prev_running_version" || "$body" == *"\"version\":\"$prev_running_version\""* ]]; then
                    cur_main_pid=$(verify_cur_restart_pid)
                    if [[ -n "$cur_main_pid" && ( -z "$prev_main_pid" || "$prev_main_pid" != "$cur_main_pid" ) ]]; then
                        echo "[update] recovery: /health version=${prev_running_version:-unknown} OK on port $p (pid ${prev_main_pid:-<none>} -> $cur_main_pid)" >&2
                        return 0
                    fi
                fi
            fi
        done
        sleep 1
        waited=$((waited + 1))
    done
    echo "[update] recovery: /health did not report version=${prev_running_version:-unknown} with a fresh PID within ${rec_health_timeout}s" >&2
    return 1
}

start_and_verify_recovered_service() {
    if restart_uses_signal_pid; then
        signal_launch_wrapper_nonfatal "$go_trader_run_sh" || return 1
        rollback_wait_signal_pidfile || return 1
        verify_recovery_health || return 1
        return 0
    fi
    local rb_rc=0
    sudo systemctl restart "$service_unit" || rb_rc=$?
    if [[ $rb_rc -eq 5 ]]; then
        echo "[update] recovery: systemd unit $service_unit not found — trying signal respawn" >&2
        if [[ ! -d /proc/self ]] || [[ ! -f "$go_trader_pidfile" ]]; then
            echo "[update] recovery: signal fallback unavailable (need Linux + pidfile)" >&2
            return 1
        fi
        if [[ ! -f "$go_trader_run_sh" ]] || [[ ! -x "$go_trader_run_sh" ]]; then
            echo "[update] recovery: signal fallback unavailable (missing executable $go_trader_run_sh)" >&2
            return 1
        fi
        restart_uses_signal=1
        signal_stop_pidfile_and_stray "$go_trader_pidfile"
        signal_launch_wrapper_nonfatal "$go_trader_run_sh" || return 1
        rollback_wait_signal_pidfile || return 1
        verify_recovery_health || return 1
        return 0
    fi
    if [[ $rb_rc -ne 0 ]]; then
        echo "[update] recovery: systemctl restart failed with exit $rb_rc" >&2
        return 1
    fi
    local rb_waited=0
    while [[ $rb_waited -lt $active_timeout ]]; do
        if systemctl is-active --quiet "$service_unit"; then
            echo "[update] recovery: previous binary active again (${prev_running_version:-unknown})"
            verify_recovery_health || return 1
            return 0
        fi
        sleep 1
        rb_waited=$((rb_waited + 1))
    done
    echo "[update] recovery: previous binary did not reach active within ${active_timeout}s" >&2
    return 1
}

on_update_signal() {
    local sig="$1"
    if [[ "$BASHPID" != "$main_shell_pid" ]]; then
        return 0
    fi
    caught_signal="$sig"
    failure_reason="interrupted by SIG${sig} in phase=${phase_name:-unknown}"
    echo "[update] interrupted by SIG${sig} in phase=${phase_name:-unknown}" >&2
    stop_update_children
    case "$sig" in
        INT) exit 130 ;;
        HUP) exit 129 ;;
        *) exit 143 ;;
    esac
}

update_exit_coordinator() {
    local exit_rc=$?
    if [[ "$BASHPID" != "$main_shell_pid" ]]; then
        return "$exit_rc"
    fi
    if [[ "$coordinator_done" == "1" ]]; then
        return "$exit_rc"
    fi
    coordinator_done=1
    coordinator_active=1
    set +e
    trap '' TERM INT HUP EXIT

    stop_update_children

    if [[ "$update_succeeded" == "1" && "$exit_rc" -eq 0 ]]; then
        if [[ -n "$tree_owner" ]]; then
            if ! update_restore_tree_owner "$owner_scope" "$tree_owner" "$tree_owner_snapshot"; then
                echo "[update] WARNING: files this update wrote under $owner_scope may still be owned by root; check with: find $owner_scope -xdev -user root" >&2
            fi
        fi
        rm -f "$tree_owner_snapshot" "$egress_state_file"
        if [[ -n "$recovery_dir" && -d "$recovery_dir" ]]; then
            rm -rf "$recovery_dir"
        fi
        if [[ "$update_lock_held" == "1" ]]; then
            update_release_lock "$recovery_root" "$recovery_key" "$main_shell_pid"
            update_lock_held=0
        fi
        exit 0
    fi

    [[ "$exit_rc" -eq 0 ]] && exit_rc=1
    if [[ -z "$failure_reason" && -n "$recovery_dir" && -f "$recovery_dir/subshell_failure_reason" ]]; then
        failure_reason=$(cat "$recovery_dir/subshell_failure_reason" 2>/dev/null || true)
    fi
    [[ -n "$failure_reason" ]] || failure_reason="command exited with status $exit_rc in phase=${phase_name:-unknown}"

    if [[ "$mutation_started" != "1" ]]; then
        rm -f ./go-trader.new "$egress_state_file"
        if [[ -n "$egress_staged" ]]; then
            rm -rf "$(dirname "$egress_staged")" 2>/dev/null || true
        fi
        if [[ -n "$tree_owner" && -n "$tree_owner_snapshot" && -f "$tree_owner_snapshot" ]]; then
            update_restore_tree_owner "$owner_scope" "$tree_owner" "$tree_owner_snapshot" >/dev/null 2>&1 || true
        fi
        rm -f "$tree_owner_snapshot"
        if [[ -n "$recovery_dir" && -d "$recovery_dir" && ! -f "$recovery_dir/incomplete" ]]; then
            rm -rf "$recovery_dir"
        fi
        if [[ "$update_lock_held" == "1" ]]; then
            update_release_lock "$recovery_root" "$recovery_key" "$main_shell_pid"
            update_lock_held=0
        fi
        exit "$exit_rc"
    fi

    echo "[update] recovery: starting recovery after failure ($failure_reason)" >&2

    local stage_stop_service="skipped"
    local stage_restore_files="pending"
    local stage_restore_venv="pending"
    local stage_restore_binaries="pending"
    local stage_restore_external="skipped"
    local stage_restore_ownership="skipped"
    local venv_owner_status="skipped"
    local stage_recovery_probe="skipped"
    local stage_recovery_service="skipped"

    if [[ "$restart_attempted" == "1" ]]; then
        if restart_uses_signal_pid; then
            signal_stop_pidfile_and_stray "$go_trader_pidfile"
            stage_stop_service="ok"
        else
            local pre_stop_state pre_stop_pid stop_waited=0 cur_stop_pid=""
            pre_stop_state=$(systemctl show -p ActiveState --value "$service_unit" 2>/dev/null || true)
            pre_stop_pid=$(systemctl show -p MainPID --value "$service_unit" 2>/dev/null || true)
            echo "[update] recovery: stopping $service_unit before file restore (ActiveState=${pre_stop_state:-unknown} MainPID=${pre_stop_pid:-0})" >&2
            sudo systemctl stop "$service_unit" >/dev/null 2>&1 || true
            while [[ $stop_waited -lt $active_timeout ]]; do
                cur_stop_pid=$(systemctl show -p MainPID --value "$service_unit" 2>/dev/null || echo "0")
                if [[ -z "$cur_stop_pid" || "$cur_stop_pid" == "0" ]]; then
                    break
                fi
                sleep 1
                stop_waited=$((stop_waited + 1))
            done
            cur_stop_pid=$(systemctl show -p MainPID --value "$service_unit" 2>/dev/null || echo "0")
            if [[ -z "$cur_stop_pid" || "$cur_stop_pid" == "0" ]]; then
                stage_stop_service="ok"
            else
                echo "[update] recovery: $service_unit MainPID=$cur_stop_pid still running after stop" >&2
                stage_stop_service="failed"
            fi
        fi
    fi

    if update_restore_snapshot_files "$repo_root" "$recovery_dir" "${rsync_from:+1}"; then
        stage_restore_files="ok"
    else
        stage_restore_files="failed"
        echo "[update] recovery: failed to restore managed files and git state from $recovery_dir" >&2
    fi

    if update_restore_snapshot_venv "$repo_root" "$recovery_dir"; then
        stage_restore_venv="ok"
    else
        stage_restore_venv="failed"
        echo "[update] recovery: failed to restore .venv from $recovery_dir" >&2
    fi

    if update_restore_snapshot_binaries "$repo_root" "$recovery_dir"; then
        stage_restore_binaries="ok"
    else
        stage_restore_binaries="failed"
        echo "[update] recovery: failed to restore binaries from $recovery_dir" >&2
    fi

    if [[ -n "$egress_staged" ]]; then
        rm -rf "$(dirname "$egress_staged")" 2>/dev/null || true
    fi
    if [[ "$unit_mutated" == "1" || "$unit_reloaded" == "1" || "$journal_mutated" == "1" || "$egress_controller_mutated" == "1" ]]; then
        if update_restore_snapshot_external "$recovery_dir" "$unit_mutated" "$unit_reloaded" "$journal_mutated" "$egress_controller_mutated" "$egress_state_file"; then
            stage_restore_external="ok"
        else
            stage_restore_external="failed"
        fi
    fi

    if update_should_fail_at restore_ownership; then
        stage_restore_ownership="failed"
        venv_owner_status="failed"
        echo "[update] recovery: forced restore_ownership failure" >&2
    elif [[ -n "$tree_owner" ]]; then
        if update_restore_tree_owner "$owner_scope" "$tree_owner" "$tree_owner_snapshot"; then
            stage_restore_ownership="ok"
        else
            stage_restore_ownership="failed"
            echo "[update] recovery: failed to restore tree ownership under $owner_scope to $tree_owner" >&2
        fi
        if [[ "$owner_scope" == "$repo_root" ]]; then
            if venv_owner_status=$(update_owner_runs_venv_status "$repo_root"); then
                :
            else
                venv_owner_status="failed"
                stage_restore_ownership="failed"
                echo "[update] recovery: owner $(update_path_owner_name "$repo_root") cannot run restored .venv/bin/python3" >&2
            fi
        fi
    fi

    local had_go_trader_snap=0
    if [[ -f "$recovery_dir/manifest.env" ]]; then
        had_go_trader_snap=$(sed -n 's/^had_go_trader=//p' "$recovery_dir/manifest.env" | tr -d "'\"\\" | head -n 1)
    fi

    if [[ "$stage_stop_service" == "failed" || "$stage_restore_files" != "ok" || "$stage_restore_venv" != "ok" || "$stage_restore_binaries" != "ok" || "$stage_restore_external" == "failed" || "$stage_restore_ownership" == "failed" ]]; then
        stage_recovery_probe="blocked"
        stage_recovery_service="blocked"
        echo "[update] recovery: probe=blocked service=blocked (earlier recovery stage failed)" >&2
    else
        if [[ "$had_go_trader_snap" == "1" && -x "$repo_root/go-trader" ]]; then
            if run_recovery_probe; then
                stage_recovery_probe="ok"
                echo "[update] recovery: probe=ok" >&2
            else
                stage_recovery_probe="failed"
                echo "[update] recovery: probe=failed (restored binary probe failed; not starting trader)" >&2
            fi
            if [[ -n "$tree_owner" ]]; then
                if ! update_restore_tree_owner "$owner_scope" "$tree_owner" "$tree_owner_snapshot"; then
                    stage_restore_ownership="failed"
                fi
            fi
        else
            stage_recovery_probe="skipped"
            echo "[update] recovery: probe=skipped (no pre-update binary)" >&2
        fi

        if [[ "$stage_recovery_probe" == "failed" || "$stage_restore_ownership" == "failed" ]]; then
            stage_recovery_service="blocked"
            echo "[update] recovery: service=blocked (probe or post-probe ownership failed)" >&2
        elif [[ "$restart_attempted" == "1" ]]; then
            if start_and_verify_recovered_service; then
                stage_recovery_service="ok"
            else
                stage_recovery_service="failed"
            fi
        elif [[ "$restart" == "1" && "$prev_pid_was_alive" == "1" && -n "$prev_main_pid" ]]; then
            local check_cur_pid=""
            check_cur_pid=$(verify_cur_restart_pid)
            if update_pid_alive "$prev_main_pid" && [[ -z "$check_cur_pid" || "$check_cur_pid" == "$prev_main_pid" ]]; then
                stage_recovery_service="ok"
                echo "[update] recovery: service=ok (original trader pid=$prev_main_pid unchanged)" >&2
            else
                stage_recovery_service="failed"
                echo "[update] recovery: service=failed (original trader pid=$prev_main_pid changed or exited; cur=${check_cur_pid:-none})" >&2
            fi
        else
            stage_recovery_service="skipped"
        fi
    fi

    local stages_summary="stop_service=$stage_stop_service files=$stage_restore_files venv=$stage_restore_venv binaries=$stage_restore_binaries external=$stage_restore_external ownership=$stage_restore_ownership venv_owner=$venv_owner_status probe=$stage_recovery_probe service=$stage_recovery_service"
    local any_failed=0
    case " $stages_summary " in
        *"=failed "*|*"=blocked "*) any_failed=1 ;;
    esac

    rm -f "$tree_owner_snapshot" "$egress_state_file"
    if [[ "$any_failed" == "0" ]]; then
        echo "[update] recovery: complete ($stages_summary)" >&2
        if [[ -n "$recovery_dir" && -d "$recovery_dir" ]]; then
            rm -rf "$recovery_dir"
        fi
    else
        update_write_incomplete_marker "$recovery_root" "$recovery_key" "$repo_root" "$recovery_dir" "$failure_reason" "$stages_summary"
        echo "[update] recovery: INCOMPLETE ($stages_summary)" >&2
        echo "[update] recovery: snapshot retained at $recovery_dir (manifest: $recovery_dir/manifest.env, marker: $recovery_root/${recovery_key}.incomplete)" >&2
        echo "[update] recovery: ACTION REQUIRED: inspect $recovery_dir, repair the deployment, then run: bash scripts/update.sh --clear-recovery" >&2
    fi

    if [[ "$update_lock_held" == "1" ]]; then
        update_release_lock "$recovery_root" "$recovery_key" "$main_shell_pid"
        update_lock_held=0
    fi
    exit "$exit_rc"
}


begin_phase preflight

repo_root=$(update_git "$PWD" rev-parse --show-toplevel) || fail "$PWD is not inside a git checkout that git can read (see the git error above)"
cd "$repo_root"

if [[ "$clear_recovery" == "1" ]]; then
    update_clear_recovery "$repo_root" "$main_shell_pid" || exit 1
    end_phase
    exit 0
fi

if [[ "$update_all" == "1" ]]; then
    forward_all_signal() {
        local sig="$1"
        if [[ -n "$all_child_pid" ]] && kill -0 "$all_child_pid" 2>/dev/null; then
            kill "-$sig" "$all_child_pid" 2>/dev/null || true
            wait "$all_child_pid" 2>/dev/null || true
        fi
        case "$sig" in
            INT) exit 130 ;;
            HUP) exit 129 ;;
            *) exit 143 ;;
        esac
    }
    trap 'forward_all_signal TERM' TERM
    trap 'forward_all_signal INT' INT
    trap 'forward_all_signal HUP' HUP

    scan_root="$(trim_space "${GO_TRADER_UPDATE_ALL_ROOT:-}")"
    scan_root_explicit=0
    if [[ -n "$scan_root" ]]; then
        scan_root_explicit=1
    else
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
            scan_root_explicit=1
            continue
        fi
        if [[ "$a" == "--update-all-root" ]]; then
            scan_root="$(trim_space "${orig_argv[$((i + 1))]}")"
            scan_root_explicit=1
            skip_next=1
            continue
        fi
        child_args+=("$a")
    done
    declare -a discovered=()
    declare -a discovery_sources=()
    if [[ "$scan_root_explicit" != "1" ]]; then
        before_count=${#discovered[@]}
        while IFS= read -r line; do
            [[ -n "$line" ]] && discovered+=("$line")
        done < <(discover_deployment_dirs_from_systemd)
        [[ ${#discovered[@]} -gt $before_count ]] && discovery_sources+=("systemd")
    fi
    if [[ -d "$scan_root" ]]; then
        shopt -s nullglob
        glob_dirs=( "$scan_root"/go-trader-*/ )
        shopt -u nullglob
        if [[ ${#glob_dirs[@]} -gt 0 ]]; then
            discovered+=( "${glob_dirs[@]}" )
            discovery_sources+=("glob")
        fi
    elif [[ "$scan_root_explicit" == "1" ]]; then
        fail "GO_TRADER_UPDATE_ALL_ROOT / --update-all-root is not a directory: $scan_root"
    fi
    if [[ ${#discovered[@]} -eq 0 ]]; then
        fail "no deployments found: no ACTIVE go-trader systemd units (systemctl absent or none active) and no directories match $scan_root/go-trader-*/ (batch root: $scan_root). Set --update-all-root <dir> / GO_TRADER_UPDATE_ALL_ROOT, or start the deployments' systemd units."
    fi
    declare -a canon=()
    for d in "${discovered[@]}"; do
        canon+=("$(canonicalize_deployment_dir "$d")")
    done
    declare -a all_dirs=()
    while IFS= read -r line; do
        [[ -n "$line" ]] && all_dirs+=("$line")
    done < <(printf '%s\n' "${canon[@]}" | sort -u | order_deployments_feeds_first)
    discovery_source=$(IFS='+'; printf '%s' "${discovery_sources[*]}")
    echo "[update] --all: ${#all_dirs[@]} deployment dir(s) via ${discovery_source} discovery"
    declare -A unit_for_dir=()
    declare -a unit_map_warnings=()
    while IFS= read -r row; do
        [[ -n "$row" ]] || continue
        row_canon="${row%%|*}"
        row_unit="${row#*|}"
        [[ -n "$row_canon" && -n "$row_unit" ]] || continue
        if [[ -n "${unit_for_dir[$row_canon]:-}" && "${unit_for_dir[$row_canon]}" != "$row_unit" ]]; then
            unit_map_warnings+=("$row_canon ${unit_for_dir[$row_canon]} $row_unit")
            continue
        fi
        unit_for_dir["$row_canon"]="$row_unit"
    done < <(discover_deployment_unit_map)
    if [[ ${#unit_map_warnings[@]} -gt 0 ]]; then
        for warn_row in "${unit_map_warnings[@]}"; do
            read -r warn_dir warn_first warn_extra <<<"$warn_row"
            echo "[update] --all: WARNING: multiple active systemd units own $warn_dir ($warn_first, $warn_extra); using $warn_first" >&2
        done
    fi
    fail_count=0
    skip_count=0
    update_count=0
    for d in "${all_dirs[@]}"; do
        if [[ ! -d "$d" ]]; then
            echo "[update] --all: skipping $d (no longer a directory)" >&2
            skip_count=$((skip_count + 1))
            continue
        fi
        if [[ ! -f "${d}scheduler/config.json" ]]; then
            echo "[update] --all: skipping $(cd "$d" && pwd) (no scheduler/config.json — not a deployment)" >&2
            skip_count=$((skip_count + 1))
            continue
        fi
        update_count=$((update_count + 1))
        mapped_unit="${unit_for_dir[$d]:-}"
        if [[ -z "$mapped_unit" ]]; then
            mapped_unit=$(update_convention_unit_for_dir "$d")
        fi
        echo "[update] --all: $(cd "$d" && pwd) role=$(update_deployment_role "$d")"
        # resolve_child_unit_override is the tested helper that decides the
        # child's effective unit + argv. Production runs must share that
        # single decision so tests cover the real branch.
        declare -a _rco_lines=()
        while IFS= read -r _line || [[ -n "$_line" ]]; do
            _rco_lines+=("$_line")
        done < <(resolve_child_unit_override "$service_unit" "$mapped_unit" "${child_args[@]}")
        resolved_unit="${_rco_lines[0]:-$service_unit}"
        declare -a resolved_child_args=()
        for ((_i = 1; _i < ${#_rco_lines[@]}; _i++)); do
            [[ -n "${_rco_lines[$_i]}" ]] && resolved_child_args+=("${_rco_lines[$_i]}")
        done
        if [[ -n "$mapped_unit" ]]; then
            echo "[update] --all: $(cd "$d" && pwd) -> unit $resolved_unit (auto-resolved from systemd)"
        else
            echo "[update] --all: $(cd "$d" && pwd) -> unit $resolved_unit (no active systemd unit; falling back)"
        fi
        child_rc=0
        if [[ "$(type -t bash 2>/dev/null || true)" == "function" ]]; then
            (cd "$d" && GO_TRADER_SERVICE="$resolved_unit" bash "$THIS_SCRIPT" "${resolved_child_args[@]}") || child_rc=$?
        else
            (cd "$d" && GO_TRADER_SERVICE="$resolved_unit" exec bash "$THIS_SCRIPT" "${resolved_child_args[@]}") &
            all_child_pid=$!
            wait "$all_child_pid" || child_rc=$?
            all_child_pid=""
        fi
        if [[ "$child_rc" -ne 0 ]]; then
            echo "[update] --all: FAILED in $d" >&2
            fail_count=$((fail_count + 1))
        fi
    done
    if [[ $fail_count -ne 0 ]]; then
        fail "--all completed with $fail_count failing instance(s) ($update_count updated, $skip_count skipped)"
    fi
    if [[ $update_count -eq 0 ]]; then
        fail "--all updated 0 deployments ($skip_count of ${#all_dirs[@]} discovered dir(s) skipped — none had scheduler/config.json). Check discovery: --update-all-root <dir> / GO_TRADER_UPDATE_ALL_ROOT, or systemd unit WorkingDirectory."
    fi
    echo "[update] --all: all instances OK ($update_count updated, $skip_count skipped of ${#all_dirs[@]} discovered)"
    exit 0
fi

trap 'on_update_signal TERM' TERM
trap 'on_update_signal INT' INT
trap 'on_update_signal HUP' HUP
trap update_exit_coordinator EXIT

refuse_unconfined_tree() {
    local tree="$1" role="$2" issues owners
    owners="top $(update_path_owner_name "$tree"), .git $(update_path_owner_name "${tree%/}/.git")"
    if ! issues=$(update_foreign_tree_check "$tree"); then
        printf '%s\n' "$issues" >&2
        if [[ -n "${SUDO_UID:-}" ]] && grep -qx "$SUDO_UID" <<<"$(update_tree_foreign_accounts "$tree")"; then
            echo "[update] you ran this through sudo, and your account owns $tree: run the update as your account without sudo (it calls sudo itself only for systemd steps)" >&2
        fi
        fail "$role $tree ($owners) belongs in part to an account other than root, and root would build and run code from it, but the listed units or owners let that account change files in it outside scheduler/ and logs/ (or run without ProtectSystem=strict). Run those units from the go-trader@.service template, whose sandbox keeps the rest of the tree read-only, or give the tree to root"
    fi
}

give_back_tree_owner() {
    [[ -n "$tree_owner" ]] || return 0
    if update_should_fail_at give_back_owner; then
        rm -f ./go-trader.new
        fail "forced give_back_owner failure $1; the binary and the running service are unchanged"
    fi
    update_restore_tree_owner "$owner_scope" "$tree_owner" "$tree_owner_snapshot" && return 0
    rm -f ./go-trader.new
    fail "could not give the root-written files under $owner_scope back to $(update_path_owner_name "$owner_scope") $1; the binary and the running service are unchanged"
}

if [[ -n "$(update_tree_foreign_accounts "$repo_root")" ]]; then
    refuse_unconfined_tree "$repo_root" "the deployment"
    tree_owner=$(update_tree_foreign_owner "$repo_root")
    owner_scope="$repo_root"
    if [[ -z "$tree_owner" && -d "$repo_root/.git" && ! -L "$repo_root/.git" ]]; then
        tree_owner=$(update_tree_foreign_owner "$repo_root/.git")
        owner_scope="$repo_root/.git"
    fi
    probe_owner=$(update_tree_foreign_owner "$repo_root")
    [[ -n "$probe_owner" ]] || probe_owner=$(update_tree_foreign_owner "$repo_root/.git")
fi
if [[ -n "$tree_owner" ]]; then
    echo "[update] $owner_scope belongs to $(update_path_owner_name "$owner_scope"); git trusts only this tree for each command, and files this update writes there are given back to that owner"
    tree_owner_snapshot=$(mktemp "${TMPDIR:-/tmp}/go-trader-owner.XXXXXX") || fail "could not create a temporary file for the ownership list"
    if ! update_owner_snapshot "$owner_scope" "$tree_owner_snapshot"; then
        rm -f "$tree_owner_snapshot"
        tree_owner_snapshot=""
        fail "could not list the root-owned paths under $owner_scope before the update; nothing changed"
    fi
fi

uv_bin=$(update_resolve_tool uv) \
    || fail "uv not on PATH and not at $(update_tool_fixed_text uv) — install uv system-wide so every account finds it (SKILL.md → Prerequisites)"
if [[ "$EUID" == "0" ]]; then
    uv_env=(UV_LINK_MODE=copy)
fi

go_bin=$(update_resolve_tool go) \
    || fail "go not on PATH and not found at $(update_tool_fixed_text go)"

update_preflight_rsync \
    || fail "rsync preflight failed — install a working rsync on PATH (SKILL.md → Prerequisites)"

if [[ ! -f scheduler/config.json ]]; then
    cat >&2 <<EOF
[update] scheduler/config.json not found in $(pwd).

update.sh must run from a deployment directory (where scheduler/config.json
exists). scheduler/config.json is gitignored, so a bare source clone has none
and the probe phase would later fail without it.

If this IS your deployment directory, copy scheduler/config.example.json to
scheduler/config.json and fill in API keys (see SKILL.md → Configure).

If you moved config out of the tree (#1056), scheduler/config.json should be a
symlink to e.g. /var/lib/go-trader/<instance>/config.json — recreate it with
scripts/migrate-config-out-of-tree.sh or \`ln -s <target> scheduler/config.json\`.

If you are syncing source to multiple deployment instances, build in the
source repo and run scripts/update.sh from each deployment instance.
EOF
    fail "scheduler/config.json missing — refusing to mutate tree from a non-deployment directory"
fi

update_managed_rsync_excludes "$repo_root" \
    || fail "could not establish managed-set exclusions for $repo_root; refusing to mutate deployment"

recovery_root=$(update_recovery_root)
recovery_key=$(update_recovery_key "$repo_root")
recovery_dir="$recovery_root/$recovery_key"
update_acquire_lock "$recovery_root" "$recovery_key" "$main_shell_pid" "$repo_root" \
    || fail "another update is already in progress for $repo_root"
update_lock_held=1

update_check_incomplete_marker "$recovery_root" "$recovery_key" "$repo_root" \
    || fail "incomplete recovery marker blocks update of $repo_root; inspect the saved snapshot and run: bash scripts/update.sh --clear-recovery"
update_pause_if_requested locked

if [[ -n "$rsync_from" ]]; then
    rsync_from=$(cd "$rsync_from" && pwd)
    if [[ "$(pwd)" == "$rsync_from" ]]; then
        fail "--rsync-from cannot be the deployment directory ($(pwd))"
    fi
    rsync_src_top=$(update_git_top "$rsync_from" 2>/dev/null || printf '%s' "$rsync_from")
    rsync_src_owner=$(update_tree_foreign_accounts "$rsync_src_top")
    if [[ -n "$rsync_src_owner" ]]; then
        refuse_unconfined_tree "$rsync_src_top" "the --rsync-from source"
    fi
    if update_git_top "$rsync_from" >/dev/null; then
        ver=$(update_git_version "$rsync_from") \
            || fail "could not read the version of the --rsync-from source $rsync_from, a git checkout (see the git error above); refusing to stamp the build as dev"
        if [[ "$ver" != *-mod ]]; then
            build_export_tree="$rsync_from"
            build_export_commit=$(update_git "$rsync_from" rev-parse HEAD) \
                || fail "could not read HEAD of the --rsync-from source $rsync_from (see the git error above)"
        elif [[ -n "$rsync_src_owner" ]]; then
            fail "the --rsync-from source $rsync_from belongs to $(update_path_owner_name "$rsync_from") and has tracked changes against its HEAD ($ver); root builds only committed code from a tree another account owns. Commit or revert the changes, or when the files already match a release, move HEAD to it without touching files (git reset <release>)"
        fi
    else
        ver=dev
        if [[ -n "$rsync_src_owner" ]]; then
            fail "the --rsync-from source $rsync_from belongs to $(update_path_owner_name "$rsync_from") and is not a git checkout; root builds only committed code from a tree another account owns"
        fi
        echo "[update] --rsync-from source $rsync_from is not a git checkout; the build is stamped dev"
    fi
fi

pre_pull_sha=$(update_git "$repo_root" rev-parse HEAD 2>/dev/null || echo "")

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

if [[ "$restart" == "1" && "$restart_mode" == "systemd" ]]; then
    unit_sync_source_path=$(update_unit_source_path "$repo_root" "$service_unit")
    unit_sync_installed_path=$(trim_space "$(systemctl show -p FragmentPath --value "$service_unit" 2>/dev/null || true)")
    if [[ -n "$unit_sync_source_path" && -n "$unit_sync_installed_path" ]]; then
        if [[ "$(update_unit_fragment_scope "$unit_sync_installed_path")" != "etc" ]]; then
            fail "systemd unit $service_unit loads from $unit_sync_installed_path, outside /etc/systemd/system — update.sh will not overwrite a vendor or generator unit. Install the shipped unit with 'sudo bash scripts/install-service.sh', or set GO_TRADER_SERVICE / --unit to the unit this deployment owns."
        fi
    fi
fi

if [[ "$restart_mode" == "systemd" ]]; then
    egress_deployment=$(update_egress_deployment "$service_unit")
fi
if [[ -n "$egress_deployment" ]]; then
    if ! update_egress_valid_deployment "$egress_deployment"; then
        fail "unit $service_unit sets an invalid GO_TRADER_EGRESS_DEPLOYMENT; repair its egress drop-in (SKILL.md Egress proxy mode)"
    fi
    if [[ "$EUID" != "0" ]]; then
        fail "egress proxy deployment $egress_deployment updates as root, because the update rebuilds its root-owned egress controller: sudo bash scripts/update.sh --restart"
    fi
    if [[ ! -f "$UPDATE_EGRESS_MANIFEST_DIR/$egress_deployment.json" ]]; then
        fail "egress manifest $UPDATE_EGRESS_MANIFEST_DIR/$egress_deployment.json is missing; reinstall with scripts/egress-install.sh"
    fi
    echo "[update] egress: deployment $egress_deployment (controller go-trader-egress@$egress_deployment.service)"
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

    if ! update_git "$repo_root" --no-optional-locks diff --quiet -- "${build_paths[@]}" || ! update_git "$repo_root" --no-optional-locks diff --cached --quiet -- "${build_paths[@]}"; then
        update_git "$repo_root" --no-optional-locks status --short -- "${build_paths[@]}" >&2
        fail "working tree has uncommitted changes in build-input paths; commit, stash, or revert first"
    fi
    if ! update_git "$repo_root" --no-optional-locks diff --quiet || ! update_git "$repo_root" --no-optional-locks diff --cached --quiet; then
        echo "[update] warning: uncommitted changes outside build-input paths (will survive git pull):" >&2
        update_git "$repo_root" --no-optional-locks status --short >&2
    fi

    untracked=$(update_git "$repo_root" --no-optional-locks ls-files --others --exclude-standard \
        scheduler shared_scripts shared_strategies shared_tools platforms backtest 2>/dev/null || true)
    untracked_root=$(update_git "$repo_root" --no-optional-locks ls-files --others --exclude-standard 2>/dev/null | grep -v '/' || true)
    if [[ -n "$untracked" || -n "$untracked_root" ]]; then
        echo "[update] warning: untracked files (the Go build uses only committed sources; they stay in the tree):" >&2
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
prev_pid_was_alive=0
if [[ "$restart" == "1" && "$restart_mode" == "signal" ]]; then
    prev_main_pid=$(signal_read_pidfile "$go_trader_pidfile") || fail "signal mode: could not read valid pid from $go_trader_pidfile"
    if ! kill -0 "$prev_main_pid" 2>/dev/null; then
        echo "[update] signal: warning: pid $prev_main_pid from pidfile is not running — capture is for bookkeeping only" >&2
    else
        prev_pid_was_alive=1
    fi
    signal_log_proc_snapshot "$prev_main_pid"
    repo_abs=$(update_canonicalize_path "$(pwd)")
    proc_cwd=$(readlink -f "/proc/$prev_main_pid/cwd" 2>/dev/null || true)
    if [[ -n "$proc_cwd" && -n "$repo_abs" && "$proc_cwd" != "$repo_abs" ]]; then
        echo "[update] signal: warning: process cwd ($proc_cwd) != repo root ($repo_abs)" >&2
    fi
else
    prev_main_pid=$(systemctl show -p MainPID --value "$service_unit" 2>/dev/null || echo "")
    if [[ "$prev_main_pid" == "0" ]]; then
        prev_main_pid=""
    fi
    if [[ -z "$prev_main_pid" && "$restart" == "1" ]]; then
        prev_main_pid=$(signal_read_pidfile "$go_trader_pidfile" || true)
    fi
    if [[ -n "$prev_main_pid" ]] && update_pid_alive "$prev_main_pid"; then
        prev_pid_was_alive=1
    fi
fi

pre_update_status_port=$(python3 - "$repo_root/scheduler/config.json" <<'PY' 2>/dev/null || true
import json, sys
try:
    cfg = json.load(open(sys.argv[1]))
    p = cfg.get("status_port") or 0
    if isinstance(p, (int, float)) and int(p) > 0:
        print(int(p))
except Exception:
    pass
PY
)
pre_update_status_port="${pre_update_status_port:-8099}"

end_phase

begin_phase snapshot
update_take_snapshot "$repo_root" "$recovery_dir" "$unit_sync_source_path" "$unit_sync_installed_path" "$egress_deployment" \
    || fail "pre-update snapshot failed in $recovery_dir; nothing in $repo_root was modified"
snapshot_ready=1
echo "[update] snapshot: saved pre-update state in $recovery_dir"
end_phase

if [[ -n "$rsync_from" ]]; then
    begin_phase rsync
    update_record_intent rsync
    tree_mutated=1
    run_rsync_from "$rsync_from" "$(pwd)"
    end_phase
else
    begin_phase pull
    update_record_intent pull
    tree_mutated=1
    update_pause_if_requested pull
    if update_should_fail_at pull; then
        printf '\n# partial-pull-mutation\n' >>"$repo_root/README.md"
        fail "forced git pull failure in $repo_root"
    fi
    update_git "$repo_root" pull --ff-only || fail "git pull --ff-only failed in $repo_root (see the git error above)"
    post_pull_sha=$(update_git "$repo_root" rev-parse HEAD 2>/dev/null || echo "")
    end_phase
fi

begin_phase sync
update_record_intent sync
run_tracked_child env ${uv_env[@]+"${uv_env[@]}"} "$uv_bin" sync "${uv_sync_flags[@]}" \
    || fail "uv sync failed in $repo_root"
update_pause_if_requested sync
if update_should_fail_at sync; then
    fail "forced uv sync failure after dependency mutation"
fi
give_back_tree_owner "after uv sync"
if [[ "$owner_scope" == "$repo_root" && -n "$tree_owner" ]] && ! update_owner_runs_venv "$repo_root"; then
    fail "after uv sync, $(update_path_owner_name "$repo_root") cannot run $repo_root/.venv/bin/python3 ($(readlink -f .venv/bin/python3 2>/dev/null || echo unresolved)); the service runs as that account. Install a Python every account can read, then rebuild the venv"
fi
end_phase

begin_phase build
update_record_intent build
if [[ -z "$rsync_from" ]]; then
    ver=$(update_git_version "$repo_root") \
        || fail "could not read the version of $repo_root (see the git error above); refusing to stamp the build as dev"
    build_export_tree="$repo_root"
    build_export_commit=$(update_git "$repo_root" rev-parse HEAD) \
        || fail "could not read HEAD of $repo_root (see the git error above)"
fi
rm -f ./go-trader.new
if update_should_fail_at build; then
    fail "forced go build failure"
fi
if [[ -n "$build_export_tree" ]]; then
    echo "[update] build: Go sources of $build_export_commit exported from $build_export_tree (untracked files are never built)"
    update_build_go_export "$build_export_tree" "$build_export_commit" "$go_bin" "$ver" "$repo_root/go-trader.new" \
        || fail "go build of the exported sources failed"
else
    echo "[update] build: Go sources of the root-owned --rsync-from source $rsync_from/scheduler (never the deployment's scheduler/, which its service can write)"
    GOWORK=off GOFLAGS=-mod=readonly "$go_bin" -C "$rsync_from/scheduler" build -buildvcs=false -ldflags "-X main.Version=$ver" -o "$repo_root/go-trader.new" . \
        || fail "go build of $rsync_from/scheduler failed"
fi
if [[ ! -s ./go-trader.new ]]; then
    fail "go build produced empty go-trader.new"
fi
if [[ ! -x ./go-trader.new ]]; then
    fail "go-trader.new is not executable"
fi
echo "[update] built ${ver}: $(stat -c '%s' ./go-trader.new 2>/dev/null || stat -f '%z' ./go-trader.new) bytes"
if [[ -n "$egress_deployment" ]]; then
    egress_staged=$(update_egress_stage_controller "$egress_deployment" "$build_export_tree" "$build_export_commit" "$go_bin" "$ver" "$rsync_from") || {
        rm -f ./go-trader.new
        fail "egress controller build failed; the binary, the controller and the running service are unchanged"
    }
    echo "[update] egress: staged controller $egress_staged from the same sources (protocol $(update_egress_protocol "$egress_staged"))"
fi
end_phase

begin_phase probe
update_record_intent probe
give_back_tree_owner "before the probe"
if [[ -n "$egress_deployment" ]]; then
    egress_probe_owner="$probe_owner"
    [[ -n "$egress_probe_owner" ]] || egress_probe_owner=$(update_unit_owner "$service_unit") || fail "could not resolve the user of $service_unit for the probe"
    echo "[update] probe: egress deployment $egress_deployment; runs as uid:gid $egress_probe_owner with the unit's environment files, so it checks the proxy policy and the controller"
    egress_probe_args=()
    while IFS= read -r line; do
        [[ -n "$line" ]] && egress_probe_args+=("$line")
    done < <(update_egress_probe_args "$service_unit" "$egress_deployment")
    probe_ok=0
    update_probe_as_owner "$repo_root" "$egress_probe_owner" "$repo_root/go-trader.new" scheduler/config.json "${egress_probe_args[@]}" && probe_ok=1
elif [[ -n "$probe_owner" ]]; then
    echo "[update] probe: runs as $(id -nu "${probe_owner%%:*}" 2>/dev/null || printf 'uid %s' "${probe_owner%%:*}") under the unit sandbox, on a private copy of scheduler/config.json, because that account can change the config and the scripts it names"
    probe_ok=0
    update_probe_as_owner "$repo_root" "$probe_owner" "$repo_root/go-trader.new" scheduler/config.json && probe_ok=1
else
    probe_ok=0
    ./go-trader.new probe && probe_ok=1
fi
if update_should_fail_at probe; then
    probe_ok=0
fi
if [[ "$probe_ok" != 1 ]]; then
    rm -f ./go-trader.new
    fail "go-trader.new probe rejected the freshly synced Python — refusing to swap"
fi
end_phase

if [[ -n "$tree_owner" ]]; then
    begin_phase ownership
    update_record_intent ownership
    if update_should_fail_at ownership || ! update_restore_tree_owner "$owner_scope" "$tree_owner" "$tree_owner_snapshot"; then
        rm -f ./go-trader.new
        fail "could not give the root-written files under $owner_scope back to $(update_path_owner_name "$owner_scope"); the binary and the running service are unchanged"
    fi
    end_phase
fi

if [[ "$restart" == "1" && "$restart_mode" == "systemd" && -n "$unit_sync_source_path" && -n "$unit_sync_installed_path" ]]; then
    begin_phase journal
    update_record_intent journal
    journal_mutated=1
    if update_should_fail_at journal || ! update_sync_journal_namespace "$repo_root" "$unit_sync_source_path" "${UPDATE_JOURNALD_ETC_DIR:-/etc/systemd}" "$recovery_dir/journal_mutation.env"; then
        rm -f ./go-trader.new
        fail "journald namespace config for $unit_sync_source_path could not be installed; the go-trader binary, the unit and the running service were left unchanged"
    fi
    end_phase
fi

begin_phase swap
update_record_intent swap_prev
swap_stage="before_prev"
rm -f ./go-trader.prev
if [[ -e ./go-trader ]]; then
    if update_should_fail_at swap_before_prev; then
        fail "forced failure before moving go-trader to go-trader.prev"
    fi
    mv -f ./go-trader ./go-trader.prev
fi
swap_stage="between_renames"
if update_should_fail_at swap_between_renames; then
    fail "forced failure between swap renames (go-trader moved to go-trader.prev, go-trader.new not yet moved)"
fi
update_record_intent swap_new
mv -f ./go-trader.new ./go-trader
swap_stage="swapped"
if update_should_fail_at swap_after_new; then
    fail "forced failure immediately after binary swap"
fi
end_phase

begin_phase agent-info
agent_info_cfg=()
[[ -f scheduler/config.json ]] && agent_info_cfg=(--config scheduler/config.json)
if ./go-trader agent-info "${agent_info_cfg[@]}" --bootstrap-md --append-changelog; then
    echo "[update] refreshed AGENTS.generated.md"
else
    echo "[update] warning: agent-info --bootstrap-md failed (non-fatal); AGENTS.generated.md not refreshed" >&2
fi
end_phase

if [[ "$restart" != "1" ]]; then
    if [[ -n "$egress_staged" ]]; then
        rm -rf "$(dirname "$egress_staged")"
        echo "[update] egress: the staged controller was discarded; it is installed only with --restart"
    fi
    update_succeeded=1
    echo "[update] build OK at $ver (skipping restart; pass --restart to enable)"
    exit 0
fi

status_port="${STATUS_PORT:-}"
if [[ -z "$status_port" && -f scheduler/config.json ]]; then
    status_port="${pre_update_status_port:-8099}"
fi
status_port="${status_port:-8099}"

if [[ "$restart_mode" == "systemd" ]]; then
    begin_phase unit
    if [[ -z "$unit_sync_source_path" ]]; then
        echo "[update] unit: no shipped unit file matches '$service_unit' — leaving the installed unit alone (install it by hand with scripts/install-service.sh)"
    elif [[ -z "$unit_sync_installed_path" ]]; then
        echo "[update] unit: systemd reports no fragment path for '$service_unit' — skipping the unit install"
    else
        unit_needs_reload=$(trim_space "$(systemctl show -p NeedDaemonReload --value "$service_unit" 2>/dev/null || true)")
        unit_decision=$(update_unit_sync_decision "$unit_sync_installed_path" "$unit_sync_source_path" "$unit_needs_reload")
        case "$unit_decision" in
            install)
                update_record_intent unit_install
                unit_mutated=1
                if update_should_fail_at unit_install; then
                    fail "forced unit install failure for $unit_sync_installed_path"
                fi
                unit_sync_backup_path=$(update_unit_install_with_backup "$unit_sync_installed_path" "$unit_sync_source_path" "$recovery_dir/unit_mutation.env") \
                    || fail "could not install $unit_sync_source_path over $unit_sync_installed_path"
                echo "[update] unit: installed $unit_sync_installed_path from $unit_sync_source_path"
                if [[ -n "$unit_sync_backup_path" ]]; then
                    echo "[update] unit: previous unit retained as $unit_sync_backup_path"
                fi
                update_record_intent unit_daemon_reload_install
                unit_reloaded=1
                if update_should_fail_at daemon_reload_install || ! sudo systemctl daemon-reload; then
                    fail "systemctl daemon-reload failed after installing $unit_sync_installed_path"
                fi
                ;;
            skip)
                echo "[update] unit: $unit_sync_installed_path is a symlink whose target differs from $unit_sync_source_path — leaving the operator's link alone; re-point it or run 'sudo bash scripts/install-service.sh'" >&2
                ;;
            reload)
                echo "[update] unit: $unit_sync_installed_path already matches $unit_sync_source_path but systemd needs a reload"
                update_record_intent unit_daemon_reload_only
                unit_reloaded=1
                if update_should_fail_at daemon_reload_only || ! sudo systemctl daemon-reload; then
                    fail "systemctl daemon-reload failed for $service_unit"
                fi
                ;;
            *)
                echo "[update] unit: $unit_sync_installed_path already matches $unit_sync_source_path"
                ;;
        esac
    fi
    end_phase

    warn_execstart_vs_swap "$service_unit"
    warn_missing_systemd_environment_files "$service_unit"

    if [[ -n "$egress_staged" ]]; then
        begin_phase egress
        update_record_intent egress_switch
        egress_state_file="$recovery_dir/egress_state.env"
        egress_rc=0
        if update_should_fail_at egress_switch; then
            egress_controller_mutated=1
            egress_rc=1
        else
            update_egress_switch_controller "$egress_deployment" "$egress_staged" "$egress_state_file" || egress_rc=$?
            if [[ -f "$egress_state_file" ]]; then
                egress_controller_mutated=1
            fi
        fi
        case "$egress_rc" in
            0) egress_switched=1 ;;
            2) ;;
            *)
                fail "egress controller switch failed for $egress_deployment"
                ;;
        esac
        end_phase
    fi

    begin_phase restart
    update_record_intent restart
    restart_attempted=1
    restart_rc=0
    if update_should_fail_at restart_cmd; then
        restart_rc=1
    else
        set +e
        sudo systemctl restart "$service_unit"
        restart_rc=$?
        set -e
    fi
    if [[ $restart_rc -eq 0 ]]; then
        if update_should_fail_at is_active_timeout; then
            fail "service did not reach active state within ${active_timeout}s (forced is_active_timeout)"
        fi
        waited=0
        until systemctl is-active --quiet "$service_unit"; do
            if [[ $waited -ge $active_timeout ]]; then
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
    update_record_intent restart
    restart_attempted=1
    run_signal_restart
    end_phase
fi

begin_phase verify
if update_should_fail_at verify_health; then
    fail "/health verify failed (forced verify_health)"
fi
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
    fail "/health verify failed — expected version=$ver and PID != ${prev_main_pid:-<none>} (last body: ${body:-<empty>})"
fi
end_phase

update_succeeded=1
echo "[update] done: $ver running, previous=${prev_running_version:-<none>} retained as go-trader.prev"

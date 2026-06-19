#!/usr/bin/env bash
# Regression tests for scripts/update_helpers.sh (#790 review).
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=update_helpers.sh
source "${SCRIPT_DIR}/update_helpers.sh"

assert_eq() {
    local got="$1" want="$2" msg="$3"
    if [[ "$got" != "$want" ]]; then
        echo "FAIL: $msg (got=$got want=$want)" >&2
        exit 1
    fi
}

assert_eq "$(update_systemd_envfile_check_path '/opt/go-trader/.env (ignore_errors=no)')" \
    "/opt/go-trader/.env" "strip ignore_errors suffix"

assert_eq "$(update_systemd_envfile_check_path '-/opt/go-trader/.env (ignore_errors=yes)')" \
    "" "optional EnvironmentFile (- prefix) yields no check path"

assert_eq "$(update_systemd_envfile_check_path '(ignore_errors=no)')" \
    "" "ignore word-split artifact"

warn_out=$(
    printf '%s\n' \
        '/etc/required.env (ignore_errors=no)' \
        '-/etc/optional.env (ignore_errors=yes)' \
        '(ignore_errors=no)' \
        | warn_missing_systemd_environment_files_from_text 'test-unit' 2>&1 || true
)
if [[ "$warn_out" != *'/etc/required.env'* ]]; then
    echo "FAIL: expected warning for required missing env file" >&2
    echo "$warn_out" >&2
    exit 1
fi
if [[ "$warn_out" == *'optional.env'* || "$warn_out" == *'ignore_errors'* ]]; then
    echo "FAIL: must not warn for optional or metadata lines" >&2
    echo "$warn_out" >&2
    exit 1
fi

# --- #850: signal-mode redirect decision -------------------------------------
assert_eq "$(update_signal_redirect_decision active /opt/go-trader/go-trader /opt/go-trader/go-trader)" \
    "redirect" "active unit running this binary -> redirect"
assert_eq "$(update_signal_redirect_decision active /opt/other/go-trader /opt/go-trader/go-trader)" \
    "" "active unit running a different binary -> no redirect (sibling worktree)"
assert_eq "$(update_signal_redirect_decision inactive /opt/go-trader/go-trader /opt/go-trader/go-trader)" \
    "" "inactive unit -> no redirect"
assert_eq "$(update_signal_redirect_decision failed /opt/go-trader/go-trader /opt/go-trader/go-trader)" \
    "" "failed unit -> no redirect"
assert_eq "$(update_signal_redirect_decision active '' /opt/go-trader/go-trader)" \
    "" "unreadable ExecStart -> no redirect"
assert_eq "$(update_signal_redirect_decision active go-trader /opt/go-trader/go-trader)" \
    "" "non-absolute ExecStart binary -> no redirect"
assert_eq "$(update_signal_redirect_decision active /opt/go-trader/go-trader '')" \
    "" "empty swap target -> no redirect"

# --- #850: rollback stray-process sweep predicate ----------------------------
assert_eq "$(update_should_sweep_proc go-trader /opt/go-trader /opt/go-trader)" \
    "sweep" "go-trader in this deployment dir -> sweep"
assert_eq "$(update_should_sweep_proc go-trader /opt/go-trader-2 /opt/go-trader)" \
    "" "go-trader in a different deployment dir -> spare (other worktree)"
assert_eq "$(update_should_sweep_proc bash /opt/go-trader /opt/go-trader)" \
    "" "non go-trader process -> spare"
assert_eq "$(update_should_sweep_proc go-trader /opt/go-trader '')" \
    "" "empty deployment dir -> spare"
assert_eq "$(update_should_sweep_proc go-trader '' /opt/go-trader)" \
    "" "unreadable proc cwd -> spare"

# --- #1012: extension-based DB rsync excludes --------------------------------
db_globs=$(update_db_rsync_excludes)
assert_eq "$db_globs" $'*.db\n*.db-wal\n*.db-shm\n*.db.lock' \
    "db rsync excludes emit the full .db family, one glob per line"
# Globs must be unanchored (no leading slash) so rsync matches at any depth.
if printf '%s\n' "$db_globs" | grep -q '^/'; then
    echo "FAIL: db rsync globs must be unanchored (no leading slash)" >&2
    exit 1
fi
# Each suffix is distinct: *.db must NOT cover *.db-wal / *.db.lock (different
# trailing chars), which is why all four globs are required.
case "stale_instance.db" in *.db) ;; *) echo "FAIL: *.db should match stale_instance.db" >&2; exit 1;; esac
case "state.db-wal" in *.db) echo "FAIL: *.db must not match state.db-wal" >&2; exit 1;; esac
case "state.db.lock" in *.db) echo "FAIL: *.db must not match state.db.lock" >&2; exit 1;; esac

# --- #1055: systemd --all deployment auto-discovery --------------------------
# Normalizer: trims, requires absolute paths, collapses to one trailing slash,
# drops empty/relative, and de-dupes preserving first-seen order.
norm_in=$'/root/go-trader-live\n/root/.openclaw/workspace/go-trader-paper-1/\n\n  /opt/deploy/go-trader-x  \nrelative/dir\n/root/go-trader-live'
assert_eq "$(printf '%s' "$norm_in" | normalize_systemd_deployment_dirs)" \
    $'/root/go-trader-live/\n/root/.openclaw/workspace/go-trader-paper-1/\n/opt/deploy/go-trader-x/' \
    "normalize: trailing slash, drop empty/relative, de-dupe, layout-independent"

# A trailing-slash-only duplicate of a bare path must collapse to one entry.
assert_eq "$(printf '%s\n' '/a/b' '/a/b/' | normalize_systemd_deployment_dirs)" \
    "/a/b/" "normalize: bare and trailing-slash forms de-dupe to one"

# Empty input yields empty output (caller then falls back to the glob).
assert_eq "$(printf '' | normalize_systemd_deployment_dirs)" "" \
    "normalize: empty input -> empty output"

# Unit globs cover primary, plain per-deployment, and template-instance units.
unit_globs=$(update_systemd_unit_globs)
assert_eq "$unit_globs" $'go-trader.service\ngo-trader-*.service\ngo-trader@*.service' \
    "unit globs cover primary, plain, and template-instance units"

# discover_*: when systemctl is absent (e.g. macOS dev/CI), emit nothing so the
# caller falls back to the glob. Only assertable where systemctl is unavailable.
if ! command -v systemctl >/dev/null 2>&1; then
    assert_eq "$(discover_deployment_dirs_from_systemd)" "" \
        "discover: no systemctl -> empty (glob fallback)"
fi

# Full pipeline with a stubbed systemctl (runs on every platform): list-units ->
# show WorkingDirectory -> normalize. Exercises layout-independence (units in
# unrelated parent dirs), de-dupe, the unset-WorkingDirectory unit (dropped by the
# normalizer), and — critically (#1055 review) — that discovery passes --state=active
# so a stopped-but-loaded unit with a valid WorkingDirectory is NEVER surfaced (and
# thus never restarted/started by --all --restart).
(
    systemctl() {
        case "$1" in
            list-units)
                # Honor --state=active: real systemctl lists only running units then.
                local active_only=0 a
                for a in "$@"; do [[ "$a" == "--state=active" ]] && active_only=1; done
                # --plain --no-legend rows: UNIT LOAD ACTIVE SUB DESCRIPTION
                printf '%s\n' \
                    'go-trader.service           loaded active running primary' \
                    'go-trader-live.service      loaded active running live' \
                    'go-trader@paper-1.service   loaded active running paper-1' \
                    'go-trader@noworkdir.service loaded active running noworkdir'
                if [[ "$active_only" != "1" ]]; then
                    # Only --all (which we must NOT use) would surface this stopped unit.
                    printf '%s\n' 'go-trader@stopped.service   loaded inactive dead stopped'
                fi
                ;;
            show)
                # show <unit> -p WorkingDirectory --value  ->  $2 is the unit
                case "$2" in
                    go-trader.service) printf '%s\n' '/root/go-trader' ;;
                    go-trader-live.service) printf '%s\n' '/root/.openclaw/workspace/go-trader-live' ;;
                    go-trader@paper-1.service) printf '%s\n' '/srv/deploys/go-trader-paper-1/' ;;
                    go-trader@noworkdir.service) printf '%s\n' '' ;;          # unset -> dropped by normalizer
                    go-trader@stopped.service) printf '%s\n' '/srv/deploys/go-trader-stopped' ;;  # valid WD, but inactive -> excluded by --state=active
                esac
                ;;
        esac
    }
    export -f systemctl 2>/dev/null || true
    got=$(discover_deployment_dirs_from_systemd)
    want=$'/root/go-trader/\n/root/.openclaw/workspace/go-trader-live/\n/srv/deploys/go-trader-paper-1/'
    assert_eq "$got" "$want" "discover: active-only, layout-independent, unset-WD dropped, stopped unit excluded"
    # Explicit guard: the stopped deployment's dir must never appear (would be started).
    case "$got" in
        *go-trader-stopped*) echo "FAIL: discovery surfaced a stopped-but-loaded unit (--state=active not applied)" >&2; exit 1 ;;
    esac
)

echo "OK: update_helpers tests passed"

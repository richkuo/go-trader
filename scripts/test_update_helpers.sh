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

# canonicalize_deployment_dir (#1055 review): symlink + /./ + // spellings of the
# same dir resolve to one physical path so the --all dedup collapses them; a
# non-existent path is returned trailing-slash-normalized (loop reports/skips it).
canon_tmp=$(mktemp -d)
canon_phys=$(cd "$canon_tmp" && pwd -P)/
ln -s "$canon_tmp" "${canon_tmp}.link"
assert_eq "$(canonicalize_deployment_dir "$canon_tmp")" "$canon_phys" \
    "canon: plain dir -> physical path + trailing slash"
assert_eq "$(canonicalize_deployment_dir "${canon_tmp}.link")" "$canon_phys" \
    "canon: symlink -> physical target (aliases collapse)"
assert_eq "$(canonicalize_deployment_dir "${canon_tmp}/./")" "$canon_phys" \
    "canon: /./ segment normalized to the same physical path"
assert_eq "$(canonicalize_deployment_dir "/no/such/go-trader-x")" "/no/such/go-trader-x/" \
    "canon: non-existent dir -> trailing-slash literal (no collapse)"
# Two genuinely distinct dirs must NOT collapse.
canon_b=$(mktemp -d)
if [[ "$(canonicalize_deployment_dir "$canon_tmp")" == "$(canonicalize_deployment_dir "$canon_b")" ]]; then
    echo "FAIL: distinct dirs must not canonicalize to the same path" >&2; exit 1
fi
rm -rf "$canon_tmp" "${canon_tmp}.link" "$canon_b"

# --- #1056: out-of-tree config migration state classifier -------------------
mig_tmp=$(mktemp -d)
assert_eq "$(update_config_migration_state "$mig_tmp/none.json")" \
    "missing" "absent config -> missing"
: > "$mig_tmp/real.json"
assert_eq "$(update_config_migration_state "$mig_tmp/real.json")" \
    "regular" "regular file still in tree -> regular (needs migrating)"
ln -s "$mig_tmp/real.json" "$mig_tmp/link.json"
assert_eq "$(update_config_migration_state "$mig_tmp/link.json")" \
    "symlink" "symlink -> symlink (already migrated; idempotent no-op)"
# Adversarial: a DANGLING symlink (target already moved/removed) must still
# classify as 'symlink', never 'missing' — otherwise a re-run would treat the
# deployment as un-migrated and clobber the live config pointer.
ln -s "$mig_tmp/gone.json" "$mig_tmp/dangling.json"
assert_eq "$(update_config_migration_state "$mig_tmp/dangling.json")" \
    "symlink" "dangling symlink -> symlink (not missing)"
rm -rf "$mig_tmp"

# --- #1056: instance-name validation (PR #1060 review) -----------------------
assert_eq "$(update_validate_instance_name live)" "ok" "plain name -> ok"
assert_eq "$(update_validate_instance_name paper-hl-btc)" "ok" "dashed name -> ok"
assert_eq "$(update_validate_instance_name paper_testing.1)" "ok" "underscore/dot -> ok"
# Adversarial: path-escape and flag-misparse names the bare char-class let through.
assert_eq "$(update_validate_instance_name ..)" "bad" "'..' -> bad (escapes target dir)"
assert_eq "$(update_validate_instance_name .)" "bad" "'.' -> bad (escapes target dir)"
assert_eq "$(update_validate_instance_name -live)" "bad" "leading dash -> bad (misparses as flag)"
assert_eq "$(update_validate_instance_name 'a/b')" "bad" "slash -> bad (path separator)"
assert_eq "$(update_validate_instance_name 'a b')" "bad" "space -> bad (disallowed char)"
assert_eq "$(update_validate_instance_name '')" "bad" "empty -> bad (caller handles no-instance separately)"

# --- #1056: base-aware systemd writable directive (PR #1060 review) ----------
assert_eq "$(update_config_writable_directive /var/lib/go-trader live)" \
    "StateDirectory=go-trader/live" "default base + instance -> StateDirectory subdir"
assert_eq "$(update_config_writable_directive /var/lib/go-trader '')" \
    "StateDirectory=go-trader" "default base, no instance -> StateDirectory"
assert_eq "$(update_config_writable_directive /etc/go-trader live)" \
    "ReadWritePaths=/etc/go-trader/live" "non-/var/lib base -> ReadWritePaths (StateDirectory can't reach it)"
assert_eq "$(update_config_writable_directive /etc/go-trader '')" \
    "ReadWritePaths=/etc/go-trader" "non-/var/lib base, no instance -> ReadWritePaths"

# --- #1056/#1060: re-running on an already-migrated (symlink) deployment must
# be an idempotent no-op and must NOT trip the daemon-running refusal — that
# refusal is gated to the mutating (regular-file) case only. (End-to-end over
# the migrate script, since the ordering is script-level, not a pure helper.)
mig2=$(mktemp -d)
mkdir -p "$mig2/deploy/scheduler" "$mig2/var/live"
: > "$mig2/var/live/config.json"
ln -s "$mig2/var/live/config.json" "$mig2/deploy/scheduler/config.json"
noop_out=$(bash "${SCRIPT_DIR}/migrate-config-out-of-tree.sh" \
    --deploy-dir "$mig2/deploy" --base "$mig2/var" --instance live 2>&1) && noop_rc=0 || noop_rc=$?
assert_eq "$noop_rc" "0" "already-migrated symlink -> idempotent no-op exit 0 (no daemon refusal)"
if [[ "$noop_out" != *"already migrated"* ]]; then
    echo "FAIL: expected 'already migrated' no-op message, got: $noop_out" >&2
    exit 1
fi
# And the no-op must not have mutated anything (symlink intact, target intact).
[[ -L "$mig2/deploy/scheduler/config.json" ]] || { echo "FAIL: no-op altered the symlink" >&2; exit 1; }
[[ -f "$mig2/var/live/config.json" ]] || { echo "FAIL: no-op altered the target" >&2; exit 1; }
rm -rf "$mig2"

# --- #1285: ExecStart --config extraction for the fleet config_version audit --
assert_eq "$(update_execstart_config_path '{ path=/opt/go-trader/go-trader ; argv[]=/opt/go-trader/go-trader --config /var/lib/go-trader/config.json ; ignore_errors=no }')" \
    "/var/lib/go-trader/config.json" "systemd ExecStart show-value with --config <path>"
assert_eq "$(update_execstart_config_path '/opt/go-trader/go-trader --config=/var/lib/go-trader/live/config.json --once')" \
    "/var/lib/go-trader/live/config.json" "--config=<path> form"
assert_eq "$(update_execstart_config_path '/opt/go-trader/go-trader --status-port 8099')" \
    "" "no --config flag -> empty (caller falls back to scheduler/config.json)"
assert_eq "$(update_execstart_config_path '')" \
    "" "empty ExecStart -> empty"

# --- #1285: fleet audit script — read-only, explicit-dir mode ----------------
fleet=$(mktemp -d)
mkdir -p "$fleet/ok/scheduler" "$fleet/old/scheduler" "$fleet/none/scheduler"
printf '{"config_version": 16}\n' > "$fleet/ok/scheduler/config.json"
printf '{"config_version": 12}\n' > "$fleet/old/scheduler/config.json"
printf '{"interval_seconds": 600}\n' > "$fleet/none/scheduler/config.json"

audit_out=$(bash "${SCRIPT_DIR}/check-config-versions.sh" "$fleet/ok") && audit_rc=0 || audit_rc=$?
assert_eq "$audit_rc" "0" "fleet audit: v16-only fleet passes"
if [[ "$audit_out" != *"VERDICT: OK"* ]]; then
    echo "FAIL: expected OK verdict for v16 fleet, got: $audit_out" >&2
    exit 1
fi

audit_out=$(bash "${SCRIPT_DIR}/check-config-versions.sh" "$fleet/ok" "$fleet/old") && audit_rc=0 || audit_rc=$?
assert_eq "$audit_rc" "1" "fleet audit: v12 deployment blocks"
if [[ "$audit_out" != *"VERDICT: BLOCKED"* || "$audit_out" != *"below floor"* ]]; then
    echo "FAIL: expected BLOCKED verdict for v12 deployment, got: $audit_out" >&2
    exit 1
fi

audit_out=$(bash "${SCRIPT_DIR}/check-config-versions.sh" "$fleet/none") && audit_rc=0 || audit_rc=$?
assert_eq "$audit_rc" "0" "fleet audit: version-less config is OK (stamped on next start)"
if [[ "$audit_out" != *"version-less"* ]]; then
    echo "FAIL: expected version-less note, got: $audit_out" >&2
    exit 1
fi

audit_out=$(bash "${SCRIPT_DIR}/check-config-versions.sh" "$fleet/missing-dir") && audit_rc=0 || audit_rc=$?
assert_eq "$audit_rc" "1" "fleet audit: missing config is a FAIL (cannot verify)"

# The audit must never mutate a config it reads.
assert_eq "$(cat "$fleet/old/scheduler/config.json")" '{"config_version": 12}' "fleet audit is read-only"
rm -rf "$fleet"

echo "OK: update_helpers tests passed"

#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
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

db_globs=$(update_db_rsync_excludes)
assert_eq "$db_globs" $'*.db\n*.db-wal\n*.db-shm\n*.db.lock' \
    "db rsync excludes emit the full .db family, one glob per line"
if printf '%s\n' "$db_globs" | grep -q '^/'; then
    echo "FAIL: db rsync globs must be unanchored (no leading slash)" >&2
    exit 1
fi
case "stale_instance.db" in *.db) ;; *) echo "FAIL: *.db should match stale_instance.db" >&2; exit 1;; esac
case "state.db-wal" in *.db) echo "FAIL: *.db must not match state.db-wal" >&2; exit 1;; esac
case "state.db.lock" in *.db) echo "FAIL: *.db must not match state.db.lock" >&2; exit 1;; esac

norm_in=$'/root/go-trader-live\n/root/.openclaw/workspace/go-trader-paper-1/\n\n  /opt/deploy/go-trader-x  \nrelative/dir\n/root/go-trader-live'
assert_eq "$(printf '%s' "$norm_in" | normalize_systemd_deployment_dirs)" \
    $'/root/go-trader-live/\n/root/.openclaw/workspace/go-trader-paper-1/\n/opt/deploy/go-trader-x/' \
    "normalize: trailing slash, drop empty/relative, de-dupe, layout-independent"

assert_eq "$(printf '%s\n' '/a/b' '/a/b/' | normalize_systemd_deployment_dirs)" \
    "/a/b/" "normalize: bare and trailing-slash forms de-dupe to one"

assert_eq "$(printf '' | normalize_systemd_deployment_dirs)" "" \
    "normalize: empty input -> empty output"

unit_globs=$(update_systemd_unit_globs)
assert_eq "$unit_globs" $'go-trader.service\ngo-trader-*.service\ngo-trader@*.service' \
    "unit globs cover primary, plain, and template-instance units"

if ! command -v systemctl >/dev/null 2>&1; then
    assert_eq "$(discover_deployment_dirs_from_systemd)" "" \
        "discover: no systemctl -> empty (glob fallback)"
fi

(
    systemctl() {
        case "$1" in
            list-units)
                local active_only=0 a
                for a in "$@"; do [[ "$a" == "--state=active" ]] && active_only=1; done
                printf '%s\n' \
                    'go-trader.service           loaded active running primary' \
                    'go-trader-live.service      loaded active running live' \
                    'go-trader@paper-1.service   loaded active running paper-1' \
                    'go-trader@noworkdir.service loaded active running noworkdir'
                if [[ "$active_only" != "1" ]]; then
                    printf '%s\n' 'go-trader@stopped.service   loaded inactive dead stopped'
                fi
                ;;
            show)
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
    case "$got" in
        *go-trader-stopped*) echo "FAIL: discovery surfaced a stopped-but-loaded unit (--state=active not applied)" >&2; exit 1 ;;
    esac
)

if ! command -v systemctl >/dev/null 2>&1; then
    assert_eq "$(discover_deployment_unit_map)" "" \
        "unit_map: no systemctl -> empty (parent service_unit fallback)"
fi

(
    systemctl() {
        case "$1" in
            list-units)
                local active_only=0 a
                for a in "$@"; do [[ "$a" == "--state=active" ]] && active_only=1; done
                printf '%s\n' \
                    'go-trader.service           loaded active running primary' \
                    'go-trader-live.service      loaded active running live' \
                    'go-trader@paper-1.service   loaded active running paper-1' \
                    'go-trader@paper-2.service   loaded active running paper-2' \
                    'go-trader@noworkdir.service loaded active running noworkdir'
                if [[ "$active_only" != "1" ]]; then
                    printf '%s\n' 'go-trader@stopped.service   loaded inactive dead stopped'
                fi
                ;;
            show)
                case "$2" in
                    go-trader.service) printf '%s\n' '/root/go-trader' ;;
                    go-trader-live.service) printf '%s\n' '/root/.openclaw/workspace/go-trader-live' ;;
                    go-trader@paper-1.service) printf '%s\n' '/srv/deploys/go-trader-paper-1/' ;;
                    go-trader@paper-2.service) printf '%s\n' '/srv/deploys/go-trader-shared/' ;;
                    go-trader@noworkdir.service) printf '%s\n' '' ;;
                    go-trader@stopped.service) printf '%s\n' '/srv/deploys/go-trader-stopped' ;;
                esac
                ;;
        esac
    }
    export -f systemctl 2>/dev/null || true
    got=$(discover_deployment_unit_map)
    want=$'/root/go-trader/|go-trader.service\n/root/.openclaw/workspace/go-trader-live/|go-trader-live.service\n/srv/deploys/go-trader-paper-1/|go-trader@paper-1.service\n/srv/deploys/go-trader-shared/|go-trader@paper-2.service'
    assert_eq "$got" "$want" \
        "unit_map: active-only, layout-independent, unset-WD dropped, stopped unit excluded"
    case "$got" in
        *go-trader-stopped*) echo "FAIL: unit_map surfaced a stopped-but-loaded unit (--state=active not applied)" >&2; exit 1 ;;
        *go-trader@noworkdir*) echo "FAIL: unit_map surfaced an unset-WD unit (WorkingDirectory filter not applied)" >&2; exit 1 ;;
    esac
    canon_pair=$(printf '%s\n' "$got" | awk -F'|' '$1 == "/srv/deploys/go-trader-shared/" { print $2 }')
    assert_eq "$canon_pair" "go-trader@paper-2.service" \
        "unit_map: trailing-slash WD canonicalizes to physical-path+slash key"
    collision_count=$(printf '%s\n' "$got" | awk -F'|' '{ print $1 }' | sort | uniq -d | wc -l)
    assert_eq "$collision_count" "0" \
        "unit_map: helper does not de-dupe; consumer must dedupe + warn"
)

(
    link_tmp=$(mktemp -d)
    canon_phys=$(cd "$link_tmp" && pwd -P)/
    ln -s "$link_tmp" "${link_tmp}.link"
    systemctl() {
        case "$1" in
            list-units)
                printf '%s\n' 'go-trader-link.service loaded active running link'
                ;;
            show)
                printf '%s\n' "${link_tmp}.link"
                ;;
        esac
    }
    export -f systemctl 2>/dev/null || true
    got=$(discover_deployment_unit_map)
    want="${canon_phys}|go-trader-link.service"
    assert_eq "$got" "$want" \
        "unit_map: symlink WorkingDirectory resolves to physical-path key (aliases collapse)"
    rm -rf "$link_tmp" "${link_tmp}.link"
)

(
    no_wd_tmp=$(mktemp -d)
    systemctl() {
        case "$1" in
            list-units)
                printf '%s\n' 'go-trader-nowd.service loaded active running nowd'
                ;;
            show)
                printf '\n'
                ;;
        esac
    }
    export -f systemctl 2>/dev/null || true
    got=$(discover_deployment_unit_map)
    assert_eq "$got" "" \
        "unit_map: unit with unset WorkingDirectory -> no row (miss handled by consumer)"
    rm -rf "$no_wd_tmp"
)

assert_eq "$(strip_unit_flags_from_argv --all --unit go-trader-x --restart)" \
    $'--all\n--restart' \
    "strip: --unit <value> (the next token) is removed"
assert_eq "$(strip_unit_flags_from_argv --all --service go-trader-x --restart)" \
    $'--all\n--restart' \
    "strip: --service <value> (the next token) is removed"
assert_eq "$(strip_unit_flags_from_argv --all --unit=go-trader-x --restart)" \
    $'--all\n--restart' \
    "strip: --unit=<value> form is removed (single argv token)"
assert_eq "$(strip_unit_flags_from_argv --all --service=go-trader-x --restart)" \
    $'--all\n--restart' \
    "strip: --service=<value> form is removed (single argv token)"
assert_eq "$(strip_unit_flags_from_argv --all --unit go-trader-a --service go-trader-b --unit=go-trader-c --service=go-trader-d --restart)" \
    $'--all\n--restart' \
    "strip: mixed forms (space + equals) all removed"
assert_eq "$(strip_unit_flags_from_argv --all --restart --yes)" \
    $'--all\n--restart\n--yes' \
    "strip: unrelated flags untouched (--yes preserved)"
assert_eq "$(strip_unit_flags_from_argv)" "" \
    "strip: empty input -> empty output"
assert_eq "$(strip_unit_flags_from_argv --unit only)" "" \
    "strip: input that is only unit flags -> empty"

assert_eq "$(resolve_child_unit_override go-trader-parent go-trader-live --all --unit go-trader-x --restart)" \
    $'go-trader-live\n--all\n--restart' \
    "resolve: map hit picks mapped unit AND strips --unit <value> from child argv"
assert_eq "$(resolve_child_unit_override go-trader-parent go-trader-live --all --unit=go-trader-x --restart)" \
    $'go-trader-live\n--all\n--restart' \
    "resolve: map hit picks mapped unit AND strips --unit=<value> from child argv"
assert_eq "$(resolve_child_unit_override go-trader-parent go-trader-live --all --service=go-trader-x --restart)" \
    $'go-trader-live\n--all\n--restart' \
    "resolve: map hit strips --service=<value> from child argv"
assert_eq "$(resolve_child_unit_override go-trader-parent go-trader-live --all --unit foo --service bar --restart)" \
    $'go-trader-live\n--all\n--restart' \
    "resolve: map hit strips BOTH --unit <v> and --service <v> from child argv"
assert_eq "$(resolve_child_unit_override go-trader-parent go-trader-live --all --unit --service --unit=foo --restart)" \
    $'go-trader-live\n--all\n--restart' \
    "resolve: map hit strips clustered unit/service flags"
assert_eq "$(resolve_child_unit_override go-trader-parent "" --all --unit go-trader-x --restart)" \
    $'go-trader-parent\n--all\n--unit\ngo-trader-x\n--restart' \
    "resolve: map MISS keeps parent's service_unit AND preserves parent's --unit <v> in child argv"
assert_eq "$(resolve_child_unit_override go-trader-parent "" --all --unit=go-trader-x --restart)" \
    $'go-trader-parent\n--all\n--unit=go-trader-x\n--restart' \
    "resolve: map MISS preserves parent's --unit=<v> in child argv"
assert_eq "$(resolve_child_unit_override go-trader-parent "" --all --restart)" \
    $'go-trader-parent\n--all\n--restart' \
    "resolve: map MISS without any parent unit token still inherits parent's service_unit"

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
canon_b=$(mktemp -d)
if [[ "$(canonicalize_deployment_dir "$canon_tmp")" == "$(canonicalize_deployment_dir "$canon_b")" ]]; then
    echo "FAIL: distinct dirs must not canonicalize to the same path" >&2; exit 1
fi
rm -rf "$canon_tmp" "${canon_tmp}.link" "$canon_b"

mig_tmp=$(mktemp -d)
assert_eq "$(update_config_migration_state "$mig_tmp/none.json")" \
    "missing" "absent config -> missing"
: > "$mig_tmp/real.json"
assert_eq "$(update_config_migration_state "$mig_tmp/real.json")" \
    "regular" "regular file still in tree -> regular (needs migrating)"
ln -s "$mig_tmp/real.json" "$mig_tmp/link.json"
assert_eq "$(update_config_migration_state "$mig_tmp/link.json")" \
    "symlink" "symlink -> symlink (already migrated; idempotent no-op)"
ln -s "$mig_tmp/gone.json" "$mig_tmp/dangling.json"
assert_eq "$(update_config_migration_state "$mig_tmp/dangling.json")" \
    "symlink" "dangling symlink -> symlink (not missing)"
rm -rf "$mig_tmp"

assert_eq "$(update_validate_instance_name live)" "ok" "plain name -> ok"
assert_eq "$(update_validate_instance_name paper-hl-btc)" "ok" "dashed name -> ok"
assert_eq "$(update_validate_instance_name paper_testing.1)" "ok" "underscore/dot -> ok"
assert_eq "$(update_validate_instance_name ..)" "bad" "'..' -> bad (escapes target dir)"
assert_eq "$(update_validate_instance_name .)" "bad" "'.' -> bad (escapes target dir)"
assert_eq "$(update_validate_instance_name -live)" "bad" "leading dash -> bad (misparses as flag)"
assert_eq "$(update_validate_instance_name 'a/b')" "bad" "slash -> bad (path separator)"
assert_eq "$(update_validate_instance_name 'a b')" "bad" "space -> bad (disallowed char)"
assert_eq "$(update_validate_instance_name '')" "bad" "empty -> bad (caller handles no-instance separately)"

assert_eq "$(update_config_writable_directive /var/lib/go-trader live)" \
    "StateDirectory=go-trader/live" "default base + instance -> StateDirectory subdir"
assert_eq "$(update_config_writable_directive /var/lib/go-trader '')" \
    "StateDirectory=go-trader" "default base, no instance -> StateDirectory"
assert_eq "$(update_config_writable_directive /etc/go-trader live)" \
    "ReadWritePaths=/etc/go-trader/live" "non-/var/lib base -> ReadWritePaths (StateDirectory can't reach it)"
assert_eq "$(update_config_writable_directive /etc/go-trader '')" \
    "ReadWritePaths=/etc/go-trader" "non-/var/lib base, no instance -> ReadWritePaths"

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
[[ -L "$mig2/deploy/scheduler/config.json" ]] || { echo "FAIL: no-op altered the symlink" >&2; exit 1; }
[[ -f "$mig2/var/live/config.json" ]] || { echo "FAIL: no-op altered the target" >&2; exit 1; }
rm -rf "$mig2"

assert_eq "$(update_execstart_config_path '{ path=/opt/go-trader/go-trader ; argv[]=/opt/go-trader/go-trader --config /var/lib/go-trader/config.json ; ignore_errors=no }')" \
    "/var/lib/go-trader/config.json" "systemd ExecStart show-value with --config <path>"
assert_eq "$(update_execstart_config_path '/opt/go-trader/go-trader --config=/var/lib/go-trader/live/config.json --once')" \
    "/var/lib/go-trader/live/config.json" "--config=<path> form"
assert_eq "$(update_execstart_config_path '/opt/go-trader/go-trader --status-port 8099')" \
    "" "no --config flag -> empty (caller falls back to scheduler/config.json)"
assert_eq "$(update_execstart_config_path '')" \
    "" "empty ExecStart -> empty"

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

assert_eq "$(cat "$fleet/old/scheduler/config.json")" '{"config_version": 12}' "fleet audit is read-only"
rm -rf "$fleet"

drift=$(mktemp -d)
mkdir -p "$drift/live/scheduler" "$drift/paper/scheduler" "$drift/paper2/scheduler" \
    "$drift/paper3/scheduler" "$drift/synced/scheduler" "$drift/broken/scheduler"

cat > "$drift/live/scheduler/config.json" <<'JSON'
{"config_version": 17, "strategies": [
  {"id": "hl-vwap-eth-60", "type": "perps", "platform": "hyperliquid",
   "script": "shared_scripts/check_strategy.py",
   "args": ["vwap", "ETH", "1h", "--mode=live"],
   "interval_seconds": 300, "leverage": 20, "margin_per_trade_usd": 50,
   "capital": 100, "close_strategy": "trailing_tp_ratchet_regime"},
  {"id": "solo-live", "type": "perps",
   "args": ["sma", "BTC", "1h", "--mode=live"], "interval_seconds": 300},
  {"id": "solo-unset", "type": "perps",
   "args": ["sma", "DOGE", "1h"], "interval_seconds": 600}
]}
JSON

cat > "$drift/paper/scheduler/config.json" <<'JSON'
{"config_version": 17, "strategies": [
  {"id": "hl-vwap-eth-60", "type": "perps", "platform": "hyperliquid",
   "script": "shared_scripts/check_strategy.py",
   "args": ["vwap", "ETH", "1h", "--mode=paper"],
   "interval_seconds": 3600, "leverage": 1,
   "capital": 10000, "close_strategy": "trailing_tp_ratchet_regime"}
]}
JSON

cat > "$drift/paper2/scheduler/config.json" <<'JSON'
{"config_version": 17, "strategies": [
  {"id": "hl-vwap-eth-60", "type": "perps", "platform": "hyperliquid",
   "script": "shared_scripts/check_strategy.py",
   "args": ["vwap", "ETH", "15m", "--mode=paper"],
   "interval_seconds": 300, "leverage": 20, "margin_per_trade_usd": 50,
   "capital": 100, "close_strategy": "trailing_tp_ratchet_regime"}
]}
JSON

cat > "$drift/paper3/scheduler/config.json" <<'JSON'
{"config_version": 17, "strategies": [
  {"id": "hl-vwap-eth-60", "type": "perps", "platform": "hyperliquid",
   "script": "shared_scripts/check_strategy.py",
   "args": ["vwap", "ETH", "1h", "--mode=paper"],
   "interval_seconds": 3600, "leverage": 20, "margin_per_trade_usd": 50,
   "capital": 100, "close_strategy": "tiered_tp_atr"}
]}
JSON

cat > "$drift/synced/scheduler/config.json" <<'JSON'
{"config_version": 17, "strategies": [
  {"id": "hl-vwap-eth-60", "type": "perps", "platform": "hyperliquid",
   "script": "shared_scripts/check_strategy.py",
   "args": ["vwap", "ETH", "1h", "--mode=paper"],
   "interval_seconds": 300, "leverage": 20, "margin_per_trade_usd": 50,
   "capital": 100, "close_strategy": "trailing_tp_ratchet_regime"}
]}
JSON

printf 'not json\n' > "$drift/broken/scheduler/config.json"

mkdir -p "$drift/paper4/scheduler"
cat > "$drift/paper4/scheduler/config.json" <<'JSON'
{"config_version": 17, "strategies": [
  {"id": "hl-vwap-eth-60", "type": "perps", "platform": "hyperliquid",
   "script": "shared_scripts/check_strategy.py",
   "args": ["vwap", "ETH", "1h"],
   "interval_seconds": 3600, "leverage": 1,
   "capital": 10000, "close_strategy": "trailing_tp_ratchet_regime"}
]}
JSON

audit_out=$(bash "${SCRIPT_DIR}/check-live-paper-config-drift.sh" "$drift/live" "$drift/paper") && audit_rc=0 || audit_rc=$?
assert_eq "$audit_rc" "1" "drift audit: drifted live/paper pair exits 1"
if [[ "$audit_out" != *"hl-vwap-eth-60"* ]]; then
    echo "FAIL: expected pair header for hl-vwap-eth-60, got: $audit_out" >&2
    exit 1
fi
if [[ "$audit_out" != *"interval_seconds"* || "$audit_out" != *"3600"* ]]; then
    echo "FAIL: expected interval_seconds drift line (300 vs 3600), got: $audit_out" >&2
    exit 1
fi
if [[ "$audit_out" != *"leverage"* || "$audit_out" != *"margin_per_trade_usd"* || "$audit_out" != *"capital"* ]]; then
    echo "FAIL: expected leverage/margin/capital drift lines, got: $audit_out" >&2
    exit 1
fi
if [[ "$audit_out" != *"CANDIDATE"* ]]; then
    echo "FAIL: expected CANDIDATE verdict (differences limited to cadence/sizing/--mode), got: $audit_out" >&2
    exit 1
fi
if [[ "$audit_out" == *"solo-live"* && "$audit_out" == *"PAIR solo-live"* ]]; then
    echo "FAIL: live-only strategy must not form a pair, got: $audit_out" >&2
    exit 1
fi

audit_out=$(bash "${SCRIPT_DIR}/check-live-paper-config-drift.sh" "$drift/live" "$drift/synced") && audit_rc=0 || audit_rc=$?
assert_eq "$audit_rc" "0" "drift audit: in-sync pair exits 0"
if [[ "$audit_out" != *"IN SYNC"* ]]; then
    echo "FAIL: expected IN SYNC verdict, got: $audit_out" >&2
    exit 1
fi

audit_out=$(bash "${SCRIPT_DIR}/check-live-paper-config-drift.sh" "$drift/live" "$drift/paper2") && audit_rc=0 || audit_rc=$?
assert_eq "$audit_rc" "0" "drift audit: other-fields-only drift flags SKIP but does not gate"
if [[ "$audit_out" != *"SKIP"* || "$audit_out" != *"OTHER"* ]]; then
    echo "FAIL: expected SKIP verdict with OTHER lines, got: $audit_out" >&2
    exit 1
fi

audit_out=$(bash "${SCRIPT_DIR}/check-live-paper-config-drift.sh" "$drift/live" "$drift/paper" "$drift/paper2") && audit_rc=0 || audit_rc=$?
assert_eq "$audit_rc" "1" "drift audit: any cadence/sizing drift gates the runbook"
if [[ "$audit_out" != *"CANDIDATE"* || "$audit_out" != *"SKIP"* ]]; then
    echo "FAIL: expected both CANDIDATE and SKIP verdicts across pairs, got: $audit_out" >&2
    exit 1
fi

audit_out=$(bash "${SCRIPT_DIR}/check-live-paper-config-drift.sh" "$drift/live" "$drift/paper3") && audit_rc=0 || audit_rc=$?
assert_eq "$audit_rc" "1" "drift audit: a single watched+other pair still gates on its cadence/sizing drift"
if [[ "$audit_out" != *"SKIP"* ]]; then
    echo "FAIL: expected SKIP verdict for watched+other pair, got: $audit_out" >&2
    exit 1
fi
if [[ "$audit_out" != *"DRIFT"* || "$audit_out" == *"VERDICT: OK"* ]]; then
    echo "FAIL: expected overall DRIFT verdict for watched+other pair, got: $audit_out" >&2
    exit 1
fi

audit_out=$(bash "${SCRIPT_DIR}/check-live-paper-config-drift.sh" "$drift/live" "$drift/paper4") && audit_rc=0 || audit_rc=$?
assert_eq "$audit_rc" "1" "drift audit: no-mode paper twin pairs with live and gates on drift"
if [[ "$audit_out" != *"PAIR hl-vwap-eth-60"* || "$audit_out" != *"interval_seconds"* ]]; then
    echo "FAIL: expected drift pair for no-mode paper twin, got: $audit_out" >&2
    exit 1
fi
if [[ "$audit_out" != *"--mode"* ]]; then
    echo "FAIL: expected a no-mode annotation on the pair, got: $audit_out" >&2
    exit 1
fi

audit_out=$(bash "${SCRIPT_DIR}/check-live-paper-config-drift.sh" "$drift/live" "$drift/synced") && audit_rc=0 || audit_rc=$?
assert_eq "$audit_rc" "0" "drift audit: in-sync pair plus unique-id unset block exits 0"
if [[ "$audit_out" == *"PAIR solo-unset"* || "$audit_out" == *"UNPAIRED solo-unset"* ]]; then
    echo "FAIL: unique-id unset block must not be paired or flagged, got: $audit_out" >&2
    exit 1
fi

audit_out=$(bash "${SCRIPT_DIR}/check-live-paper-config-drift.sh" "$drift/live" "$drift/synced" "$drift/paper4") && audit_rc=0 || audit_rc=$?
assert_eq "$audit_rc" "0" "drift audit: in-sync explicit pair + UNPAIRED no-mode block exits 0"
pair_count=$(printf '%s\n' "$audit_out" | grep -c '^PAIR ')
assert_eq "$pair_count" "1" "drift audit: exactly one pair when explicit paper and no-mode blocks coexist"
if [[ "$audit_out" != *"UNPAIRED"* ]]; then
    echo "FAIL: expected UNPAIRED note for the no-mode block, got: $audit_out" >&2
    exit 1
fi

audit_out=$(bash "${SCRIPT_DIR}/check-live-paper-config-drift.sh" "$drift/live" "$drift/broken") && audit_rc=0 || audit_rc=$?
assert_eq "$audit_rc" "1" "drift audit: unreadable config exits 1"

audit_out=$(bash "${SCRIPT_DIR}/check-live-paper-config-drift.sh" "$drift/no-such-dir") && audit_rc=0 || audit_rc=$?
assert_eq "$audit_rc" "1" "drift audit: missing config exits 1"

assert_eq "$(cat "$drift/paper/scheduler/config.json")" "$(cat <<'JSON'
{"config_version": 17, "strategies": [
  {"id": "hl-vwap-eth-60", "type": "perps", "platform": "hyperliquid",
   "script": "shared_scripts/check_strategy.py",
   "args": ["vwap", "ETH", "1h", "--mode=paper"],
   "interval_seconds": 3600, "leverage": 1,
   "capital": 10000, "close_strategy": "trailing_tp_ratchet_regime"}
]}
JSON
)" "drift audit is read-only"
rm -rf "$drift"

echo "OK: update_helpers tests passed"

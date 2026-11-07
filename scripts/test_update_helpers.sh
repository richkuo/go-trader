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

unit_src_cases=(
    "go-trader|/repo/go-trader.service|bare default unit name resolves the plain shipped unit"
    "go-trader.service|/repo/go-trader.service|plain unit name resolves the plain shipped unit"
    "go-trader@live.service|/repo/systemd/go-trader@.service|instance unit resolves the shipped template"
    "go-trader@paper-testing|/repo/systemd/go-trader@.service|instance unit without suffix resolves the template"
    "go-trader-2.service||a unit name we do not ship yields no source"
    "go-trader@.service||a template with no instance yields no source"
    "go-trader@../etc/passwd||a traversal instance name yields no source"
    "go-trader.socket||a non-service unit yields no source"
    "||an empty unit name yields no source"
)
for row in "${unit_src_cases[@]}"; do
    IFS='|' read -r case_unit case_want case_msg <<<"$row"
    assert_eq "$(update_unit_source_path /repo "$case_unit")" "$case_want" "unit source: $case_msg"
done
assert_eq "$(update_unit_source_path /repo/ go-trader)" "/repo/go-trader.service" \
    "unit source: a trailing slash on the repo root does not double up"
assert_eq "$(update_unit_source_path '' go-trader)" "" "unit source: an empty repo root yields no source"

assert_eq "$(update_unit_fragment_scope /etc/systemd/system/go-trader.service)" "etc" \
    "fragment scope: an operator unit under /etc/systemd/system is ours"
assert_eq "$(update_unit_fragment_scope /usr/lib/systemd/system/go-trader.service)" "other" \
    "fragment scope: a vendor unit is not ours"
assert_eq "$(update_unit_fragment_scope /run/systemd/generator/go-trader.service)" "other" \
    "fragment scope: a generator unit is not ours"
assert_eq "$(update_unit_fragment_scope /etc/systemd/system/go-trader.service.d/50-override.conf)" "other" \
    "fragment scope: a drop-in path is not a fragment we install over"
assert_eq "$(update_unit_fragment_scope etc/systemd/system/go-trader.service)" "other" \
    "fragment scope: a relative path is not ours"
assert_eq "$(update_unit_fragment_scope '')" "" "fragment scope: no fragment path yields no scope"

unit_sync_dir=$(mktemp -d)
mkdir -p "$unit_sync_dir/etc" "$unit_sync_dir/repo"
printf 'shipped\n' >"$unit_sync_dir/repo/go-trader.service"
printf 'stale\n' >"$unit_sync_dir/etc/go-trader.service"
assert_eq "$(update_unit_sync_decision "$unit_sync_dir/etc/go-trader.service" "$unit_sync_dir/repo/go-trader.service" no)" \
    "install" "sync decision: a differing installed unit is installed"
assert_eq "$(update_unit_sync_decision "$unit_sync_dir/etc/absent.service" "$unit_sync_dir/repo/go-trader.service" no)" \
    "install" "sync decision: a missing installed unit is installed"
printf 'shipped\n' >"$unit_sync_dir/etc/go-trader.service"
assert_eq "$(update_unit_sync_decision "$unit_sync_dir/etc/go-trader.service" "$unit_sync_dir/repo/go-trader.service" yes)" \
    "reload" "sync decision: a matching unit systemd has not reloaded only reloads"
assert_eq "$(update_unit_sync_decision "$unit_sync_dir/etc/go-trader.service" "$unit_sync_dir/repo/go-trader.service" no)" \
    "none" "sync decision: a matching, loaded unit needs nothing"
assert_eq "$(update_unit_sync_decision "$unit_sync_dir/etc/go-trader.service" "$unit_sync_dir/repo/absent.service" yes)" \
    "none" "sync decision: no shipped source means no install and no reload"
assert_eq "$(update_unit_sync_decision "" "$unit_sync_dir/repo/go-trader.service" yes)" \
    "none" "sync decision: an unresolved fragment path means no install and no reload"
printf 'operator\n' >"$unit_sync_dir/etc/linked-target.service"
ln -s "$unit_sync_dir/etc/linked-target.service" "$unit_sync_dir/etc/linked.service"
assert_eq "$(update_unit_sync_decision "$unit_sync_dir/etc/linked.service" "$unit_sync_dir/repo/go-trader.service" no)" \
    "skip" "sync decision: a symlinked fragment that differs is left to the operator, never flattened"
printf 'shipped\n' >"$unit_sync_dir/etc/linked-target.service"
assert_eq "$(update_unit_sync_decision "$unit_sync_dir/etc/linked.service" "$unit_sync_dir/repo/go-trader.service" yes)" \
    "reload" "sync decision: a symlinked fragment already carrying the shipped unit still reloads"
ln -s "$unit_sync_dir/etc/no-such-target.service" "$unit_sync_dir/etc/dangling.service"
assert_eq "$(update_unit_sync_decision "$unit_sync_dir/etc/dangling.service" "$unit_sync_dir/repo/go-trader.service" no)" \
    "skip" "sync decision: a dangling symlink is left to the operator"

UPDATE_UNIT_SUDO=""
mkdir -p "$unit_sync_dir/etc/go-trader.service.d"
printf '[Service]\nEnvironment=X=1\n' >"$unit_sync_dir/etc/go-trader.service.d/50-merge-paper.conf"
dropin_before=$(cat "$unit_sync_dir/etc/go-trader.service.d/50-merge-paper.conf")
printf 'stale\n' >"$unit_sync_dir/etc/go-trader.service"
printf 'shipped v2\n' >"$unit_sync_dir/repo/go-trader.service"
unit_backup=$(update_unit_install_with_backup "$unit_sync_dir/etc/go-trader.service" "$unit_sync_dir/repo/go-trader.service") \
    || { echo "FAIL: unit install with backup returned non-zero" >&2; exit 1; }
assert_eq "$unit_backup" "$unit_sync_dir/etc/go-trader.service.prev" \
    "unit install: the replaced unit is retained beside it as .prev"
assert_eq "$(cat "$unit_sync_dir/etc/go-trader.service")" "shipped v2" "unit install: the shipped unit is on disk"
assert_eq "$(cat "$unit_backup")" "stale" "unit install: the backup holds the replaced unit"
assert_eq "$(cat "$unit_sync_dir/etc/go-trader.service.d/50-merge-paper.conf")" "$dropin_before" \
    "unit install: operator drop-ins are untouched"
assert_eq "$(stat -c '%a' "$unit_sync_dir/etc/go-trader.service" 2>/dev/null || stat -f '%Lp' "$unit_sync_dir/etc/go-trader.service")" \
    "644" "unit install: the installed unit is world-readable 0644"

update_unit_restore_backup "$unit_sync_dir/etc/go-trader.service" "$unit_backup" \
    || { echo "FAIL: unit restore returned non-zero" >&2; exit 1; }
assert_eq "$(cat "$unit_sync_dir/etc/go-trader.service")" "stale" "unit restore: rollback puts the previous unit back"
[[ ! -e "$unit_backup" ]] || { echo "FAIL: unit restore left the .prev backup behind" >&2; exit 1; }
assert_eq "$(cat "$unit_sync_dir/etc/go-trader.service.d/50-merge-paper.conf")" "$dropin_before" \
    "unit restore: operator drop-ins are untouched"
update_unit_restore_backup "$unit_sync_dir/etc/go-trader.service" "$unit_backup" && restore_rc=0 || restore_rc=$?
assert_eq "$restore_rc" "1" "unit restore: a missing backup fails instead of clobbering the unit"

unit_no_prev=$(update_unit_install_with_backup "$unit_sync_dir/etc/fresh.service" "$unit_sync_dir/repo/go-trader.service") \
    || { echo "FAIL: unit install onto a missing unit returned non-zero" >&2; exit 1; }
assert_eq "$unit_no_prev" "" "unit install: no backup is made when there was no installed unit"
[[ ! -e "$unit_sync_dir/etc/fresh.service.prev" ]] || { echo "FAIL: unit install invented a .prev for a missing unit" >&2; exit 1; }
update_unit_install_with_backup "$unit_sync_dir/etc/go-trader.service" "$unit_sync_dir/repo/absent.service" && install_rc=0 || install_rc=$?
assert_eq "$install_rc" "1" "unit install: a missing shipped source refuses"
assert_eq "$(cat "$unit_sync_dir/etc/go-trader.service")" "stale" "unit install: a refused install leaves the unit as it was"
unset UPDATE_UNIT_SUDO
rm -rf "$unit_sync_dir"

assert_eq "$(update_unit_source_path "$SCRIPT_DIR/.." go-trader)" "$SCRIPT_DIR/../go-trader.service" \
    "unit source: the plain mapping names a file this repo ships"
[[ -f "$SCRIPT_DIR/../go-trader.service" ]] || { echo "FAIL: shipped plain unit missing" >&2; exit 1; }
[[ -f "$SCRIPT_DIR/../systemd/go-trader@.service" ]] || { echo "FAIL: shipped template unit missing" >&2; exit 1; }

systemd_version_cases=(
    "systemd 255 (255.4-1ubuntu8.4)|255|Ubuntu 24.04 banner"
    "systemd 239 (239-58.el8)|239|RHEL 8 banner"
    "systemd 256~rc3 (256~rc3-1)|256|a release-candidate suffix keeps the major number"
    "not systemd|<none>|an unrelated banner yields no version"
    "|<none>|an empty banner yields no version"
)
for row in "${systemd_version_cases[@]}"; do
    IFS='|' read -r case_text case_want case_msg <<<"$row"
    [[ "$case_want" == "<none>" ]] && case_want=""
    assert_eq "$(update_systemd_major_version "$case_text"$'\n+PAM +AUDIT')" "$case_want" "systemd version: $case_msg"
done
assert_eq "$(update_journal_namespace_supported 244)" "no" "namespace support: systemd 244 predates LogNamespace="
assert_eq "$(update_journal_namespace_supported 245)" "yes" "namespace support: systemd 245 added LogNamespace="
assert_eq "$(update_journal_namespace_supported 255)" "yes" "namespace support: systemd 255 supports LogNamespace="
assert_eq "$(update_journal_namespace_supported '')" "no" "namespace support: an unknown version is treated as unsupported"

assert_eq "$(update_journalctl_unit_command go-trader@live.service go-trader)" \
    "journalctl --namespace=+go-trader -u go-trader@live.service" \
    "journalctl command: a namespaced unit reads its namespace merged with the default journal"
assert_eq "$(update_journalctl_unit_command go-trader.service '')" "journalctl -u go-trader.service" \
    "journalctl command: a unit without a namespace reads the default journal"

for shipped_unit in "$SCRIPT_DIR/../go-trader.service" "$SCRIPT_DIR/../systemd/go-trader@.service"; do
    shipped_ns=$(update_unit_log_namespace "$shipped_unit")
    [[ -n "$shipped_ns" ]] || { echo "FAIL: $shipped_unit sets no LogNamespace" >&2; exit 1; }
    shipped_conf="$SCRIPT_DIR/../systemd/journald@${shipped_ns}.conf"
    [[ -f "$shipped_conf" ]] || { echo "FAIL: $shipped_unit names namespace $shipped_ns but $shipped_conf is not shipped" >&2; exit 1; }
    grep -qx 'ForwardToSyslog=no' "$shipped_conf" || { echo "FAIL: $shipped_conf must turn syslog forwarding off explicitly" >&2; exit 1; }
    grep -q '^SystemMaxUse=' "$shipped_conf" || { echo "FAIL: $shipped_conf must cap the namespace size" >&2; exit 1; }
done

jns=$(mktemp -d)
mkdir -p "$jns/bin" "$jns/etc" "$jns/repo/systemd"
cat >"$jns/bin/systemctl" <<EOS
#!/usr/bin/env bash
if [[ "\$1" == "--version" ]]; then
    printf 'systemd %s (test)\n+PAM\n' "\$(cat "$jns/version")"
    exit 0
fi
printf '%s\n' "\$*" >>"$jns/systemctl.log"
[[ -f "$jns/fail-restart" ]] && exit 1
exit 0
EOS
chmod +x "$jns/bin/systemctl"
printf '[Service]\nLogNamespace=first\nLogNamespace= go-trader \n' >"$jns/repo/unit.service"
printf '[Service]\nExecStart=/bin/true\n' >"$jns/repo/plain.service"
printf '[Journal]\nForwardToSyslog=no\nSystemMaxUse=2G\n' >"$jns/repo/systemd/journald@go-trader.conf"
jdest="$jns/etc/journald@go-trader.conf"
assert_eq "$(update_unit_log_namespace "$jns/repo/unit.service")" "go-trader" \
    "unit namespace: the last LogNamespace= assignment wins, trimmed"
assert_eq "$(update_journald_conf_path "$jns/etc/" go-trader)" "$jdest" "journald conf path: namespace file under the etc dir"

run_jsync() {
    PATH="$jns/bin:$PATH" UPDATE_UNIT_SUDO="" update_sync_journal_namespace "$jns/repo" "$1" "$jns/etc" 2>&1
}
restart_count() {
    grep -c '^try-restart systemd-journald@go-trader.service$' "$jns/systemctl.log" 2>/dev/null || true
}

echo 244 >"$jns/version"
jout=$(run_jsync "$jns/repo/unit.service") && jrc=0 || jrc=$?
assert_eq "$jrc" "0" "journal sync: old systemd warns and continues"
[[ "$jout" == *"older than 245"* ]] || { echo "FAIL: old systemd must warn clearly, got: $jout" >&2; exit 1; }
[[ ! -e "$jdest" ]] || { echo "FAIL: old systemd must not get a namespace config" >&2; exit 1; }

echo 255 >"$jns/version"
jout=$(run_jsync "$jns/repo/plain.service") && jrc=0 || jrc=$?
assert_eq "$jrc" "0" "journal sync: a unit without LogNamespace is a no-op"
[[ ! -e "$jdest" ]] || { echo "FAIL: a unit without LogNamespace must not install a namespace config" >&2; exit 1; }

jout=$(run_jsync "$jns/repo/unit.service") && jrc=0 || jrc=$?
assert_eq "$jrc" "0" "journal sync: first install succeeds"
assert_eq "$(cat "$jdest")" "$(cat "$jns/repo/systemd/journald@go-trader.conf")" "journal sync: the shipped config is installed"
[[ ! -e "$jdest.prev" ]] || { echo "FAIL: a first install must not invent a .prev" >&2; exit 1; }
assert_eq "$(restart_count)" "1" "journal sync: an install restarts a running namespace journald (try-restart)"

jout=$(run_jsync "$jns/repo/unit.service") && jrc=0 || jrc=$?
assert_eq "$jrc" "0" "journal sync: a second run succeeds"
assert_eq "$(restart_count)" "1" "journal sync: an unchanged config does not restart journald again"

printf '[Journal]\nSystemMaxUse=9G\n' >"$jdest"
jout=$(run_jsync "$jns/repo/unit.service") && jrc=0 || jrc=$?
assert_eq "$jrc" "0" "journal sync: an operator-edited config is replaced"
assert_eq "$(cat "$jdest")" "$(cat "$jns/repo/systemd/journald@go-trader.conf")" "journal sync: the shipped config wins over an edit"
assert_eq "$(cat "$jdest.prev")" $'[Journal]\nSystemMaxUse=9G' "journal sync: the edited config is kept as .prev"
[[ "$jout" == *"journald@go-trader.conf.d/*.conf"* ]] || { echo "FAIL: replacing an edit must name the drop-in dir, got: $jout" >&2; exit 1; }
assert_eq "$(restart_count)" "2" "journal sync: a replaced config restarts journald"

printf '[Journal]\nSystemMaxUse=9G\n' >"$jdest"
touch "$jns/fail-restart"
jout=$(run_jsync "$jns/repo/unit.service") && jrc=0 || jrc=$?
assert_eq "$jrc" "1" "journal sync: a failed journald restart fails the sync"
assert_eq "$(cat "$jdest")" $'[Journal]\nSystemMaxUse=9G' "journal sync: a failed restart puts the previous config back"
rm -f "$jdest"
jout=$(run_jsync "$jns/repo/unit.service") && jrc=0 || jrc=$?
assert_eq "$jrc" "1" "journal sync: a failed restart after a first install fails the sync"
[[ ! -e "$jdest" ]] || { echo "FAIL: a failed restart after a first install must remove the new config" >&2; exit 1; }
rm -f "$jns/fail-restart"

printf 'operator\n' >"$jns/operator.conf"
ln -s "$jns/operator.conf" "$jdest"
jout=$(run_jsync "$jns/repo/unit.service") && jrc=0 || jrc=$?
assert_eq "$jrc" "0" "journal sync: a symlinked config is left alone"
assert_eq "$(cat "$jns/operator.conf")" "operator" "journal sync: the symlink target is untouched"
rm -f "$jdest"

rm -f "$jns/repo/systemd/journald@go-trader.conf"
jout=$(run_jsync "$jns/repo/unit.service") && jrc=0 || jrc=$?
assert_eq "$jrc" "1" "journal sync: a namespace with no shipped config refuses"
printf '[Service]\nLogNamespace=../evil\n' >"$jns/repo/bad.service"
jout=$(run_jsync "$jns/repo/bad.service") && jrc=0 || jrc=$?
assert_eq "$jrc" "1" "journal sync: a namespace that is not a plain name refuses"
rm -rf "$jns"

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

tmp_cfg_dir=$(mktemp -d)
trap 'rm -rf "$tmp_cfg_dir"' EXIT
export GO_TRADER_UPDATE_PYTHON="$(command -v python3)"

export GO_TRADER_UPDATE_CONFIG="$tmp_cfg_dir/single.json"
cat > "$GO_TRADER_UPDATE_CONFIG" <<'JSON'
{"db_file": "/var/lib/go-trader/state.db"}
JSON
assert_eq "$(update_resolve_db_exclude)" "/var/lib/go-trader/state.db" \
    "single-file layout excludes db_file only"

export GO_TRADER_UPDATE_CONFIG="$tmp_cfg_dir/split.json"
cat > "$GO_TRADER_UPDATE_CONFIG" <<'JSON'
{"db_file": "/var/lib/go-trader/live.db", "paper_db_file": "/var/lib/go-trader/paper.db"}
JSON
assert_eq "$(update_resolve_db_exclude)" $'/var/lib/go-trader/live.db\n/var/lib/go-trader/paper.db' \
    "split layout excludes db_file AND paper_db_file (#1523)"

export GO_TRADER_UPDATE_CONFIG="$tmp_cfg_dir/default.json"
cat > "$GO_TRADER_UPDATE_CONFIG" <<'JSON'
{"paper_db_file": "/var/lib/go-trader/paper.db"}
JSON
assert_eq "$(update_resolve_db_exclude)" $'scheduler/state.db\n/var/lib/go-trader/paper.db' \
    "an omitted db_file still falls back to the default primary path"

export GO_TRADER_UPDATE_CONFIG="$tmp_cfg_dir/sources.json"
cat > "$GO_TRADER_UPDATE_CONFIG" <<'JSON'
{"db_file": "/var/lib/go-trader/live.db",
 "paper_sources": [{"id": "btc", "db_file": "/var/lib/go-trader/btc.db"},
                   {"id": "eth", "db_file": "/var/lib/go-trader/eth.db"},
                   {"id": "blank"}]}
JSON
assert_eq "$(update_resolve_db_exclude)" $'/var/lib/go-trader/live.db\n/var/lib/go-trader/btc.db\n/var/lib/go-trader/eth.db' \
    "every paper source database is excluded from the update (#1561)"

unset GO_TRADER_UPDATE_CONFIG GO_TRADER_UPDATE_PYTHON

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
    collision_count=$(printf '%s\n' "$got" | awk -F'|' '{ print $1 }' | sort | uniq -d | wc -l | tr -d '[:space:]')
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

# Integration: drive the real scripts/update.sh --all coordinator with mocked
# systemctl + mocked bash recursion. Asserts that the per-dir hit/miss
# branch + GO_TRADER_SERVICE injection + parent-flag strip that production
# runs matches the resolve_child_unit_override contract.
update_test_repo_root=$(git rev-parse --show-toplevel 2>/dev/null || true)
if [[ -n "$update_test_repo_root" && -x "$update_test_repo_root/scripts/update.sh" ]]; then
(
    cd "$update_test_repo_root"

    fleet=$(cd "$(mktemp -d)" && pwd -P)
    for n in go-trader-a go-trader-b go-trader-c; do
        mkdir -p "$fleet/$n/scheduler"
        echo '{}' > "$fleet/$n/scheduler/config.json"
    done

    # Shim: a real `systemctl` binary on PATH so update_helpers.sh's
    # `command -v systemctl` guard resolves AND list-units/show returns
    # our test data. Functions exported from the test driver are not
    # visible to `command -v`, so a PATH shim is the only way to drive
    # discover_deployment_dirs_from_systemd + discover_deployment_unit_map
    # in the test.
    mkdir -p "$fleet/shimbin"
    cat > "$fleet/shimbin/systemctl" <<EOS
#!/usr/bin/env bash
case "\$1" in
    list-units)
        active_only=0
        for a in "\$@"; do [[ "\$a" == "--state=active" ]] && active_only=1; done
        if [[ "\$active_only" == "1" ]]; then
            printf '%s\n' \\
                'go-trader-x.service loaded active running x' \\
                'go-trader-y.service loaded active running y'
        fi
        ;;
    show)
        case "\$2" in
            go-trader-x.service) printf '%s\n' "$fleet/go-trader-a" ;;
            go-trader-y.service) printf '%s\n' "$fleet/go-trader-b" ;;
        esac
        ;;
esac
EOS
    chmod +x "$fleet/shimbin/systemctl"

    bash() {
        if [[ "${1:-}" == *update.sh ]]; then
            printf '[child] cwd=%s GO_TRADER_SERVICE=%s argv=%s\n' \
                "$(pwd)" "${GO_TRADER_SERVICE:-<unset>}" "$*" >> "${RECORD_OUT:?}"
            return 0
        fi
        command bash "$@"
    }
    export -f bash 2>/dev/null || true

    # Scenario 1: auto-resolve (default scan_root, shim systemd for a/b).
    : > "$fleet/records"
    RECORD_OUT="$fleet/records" \
        PATH="$fleet/shimbin:$PATH" \
        GO_TRADER_SERVICE=go-trader-parent \
        command bash scripts/update.sh --all --restart --unit=go-trader-foo >/dev/null 2>&1 || true
    r=$(grep "$fleet" "$fleet/records" || true)
    spawn_count=$(printf '%s\n' "$r" | grep -c '\[child\] ' || true)
    assert_eq "$spawn_count" "2" "integration auto-resolve: 2 systemd-mapped dirs spawn"
    a_line=$(printf '%s\n' "$r" | grep "cwd=$fleet/go-trader-a" || true)
    b_line=$(printf '%s\n' "$r" | grep "cwd=$fleet/go-trader-b" || true)
    if [[ "$a_line" != *"GO_TRADER_SERVICE=go-trader-x.service"* || "$a_line" != *"--restart"* ]]; then
        echo "FAIL: a record missing x.service or --restart" >&2; printf '%s\n' "$r" >&2; exit 1
    fi
    if [[ "$b_line" != *"GO_TRADER_SERVICE=go-trader-y.service"* || "$b_line" != *"--restart"* ]]; then
        echo "FAIL: b record missing y.service or --restart" >&2; printf '%s\n' "$r" >&2; exit 1
    fi
    if printf '%s\n' "$r" | grep -v '/go-trader-c/' | grep -q -- '--unit=go-trader-foo'; then
        echo "FAIL: parent --unit=go-trader-foo leaked into auto-resolved child argv" >&2
        printf '%s\n' "$r" >&2
        exit 1
    fi

    # Scenario 2: fallback (--update-all-root=$fleet, no systemd mapping).
    # Drop the shim so the bounded-glob run treats systemd as absent; all
    # 3 dirs must fall back to parent's service_unit.
    rm -rf "$fleet/shimbin"
    : > "$fleet/records"
    RECORD_OUT="$fleet/records" \
        GO_TRADER_SERVICE=go-trader-parent \
        command bash scripts/update.sh --all --restart --update-all-root="$fleet" >/dev/null 2>&1 || true
    r=$(grep "$fleet" "$fleet/records" || true)
    spawn_count=$(printf '%s\n' "$r" | grep -c '\[child\] ' || true)
    assert_eq "$spawn_count" "3" "integration fallback: 3 dirs spawn (a, b, c all miss)"
    if printf '%s\n' "$r" | grep -vq 'GO_TRADER_SERVICE=go-trader-parent'; then
        echo "FAIL: at least one fallback child did not inherit parent's service_unit" >&2
        printf '%s\n' "$r" >&2
        exit 1
    fi

    rm -rf "$fleet"
)
fi

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

merge_tmp=$(mktemp -d)
mkdir -p "$merge_tmp/real/dir" "$merge_tmp/deploy"
ln -s "$merge_tmp/real" "$merge_tmp/link"
absent_canon=$(update_canonical_db_path "$merge_tmp/link/dir/state.db")
assert_eq "$absent_canon" "$merge_tmp/link/dir/state.db" \
    "canonical db path keeps the unresolved absolute path of an absent file behind a symlinked parent, as the scheduler does"
assert_eq "$(update_state_lock_paths "$merge_tmp/link/dir/state.db")" "${absent_canon}.lock"$'\n'"${absent_canon}.manual-action.lock" \
    "state lock paths of an absent db sit beside the unresolved path"
assert_eq "$(update_file_fingerprint "$merge_tmp/nope")" "absent" "fingerprint of a missing file is absent"
printf 'abc' > "$merge_tmp/real/dir/state.db"
canon=$(update_canonical_db_path "$merge_tmp/link/dir/state.db")
assert_eq "$canon" "$(python3 -c 'import os,sys; print(os.path.realpath(sys.argv[1]))' "$merge_tmp/real")/dir/state.db" \
    "canonical db path resolves every symlink once the file exists"
lock_paths=$(update_state_lock_paths "$merge_tmp/link/dir/state.db")
assert_eq "$lock_paths" "${canon}.lock"$'\n'"${canon}.manual-action.lock" \
    "state lock paths sit beside the canonical db"
printf 'wal' > "$merge_tmp/real/dir/state.db-wal"
fp=$(update_db_fingerprint "$merge_tmp/link/dir/state.db")
assert_eq "$fp" "db=ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"$'\n'"wal=$(update_file_fingerprint "$merge_tmp/real/dir/state.db-wal")" \
    "db fingerprint covers the db and its wal only"
: > "$merge_tmp/real/dir/state.db-wal"
assert_eq "$(update_db_fingerprint "$merge_tmp/link/dir/state.db" | tail -n 1)" "wal=none" \
    "an empty wal holds no frames and fingerprints like an absent one"
rm -f "$merge_tmp/real/dir/state.db-wal"
assert_eq "$(update_db_fingerprint "$merge_tmp/link/dir/state.db" | tail -n 1)" "wal=none" \
    "an absent wal fingerprints as none"
assert_eq "$(update_resolve_config_db_path /opt/go-trader-a scheduler/state.db)" "/opt/go-trader-a/scheduler/state.db" \
    "relative db_file resolves under the deploy dir"
assert_eq "$(update_resolve_config_db_path /opt/go-trader-a /var/lib/go-trader/a/state.db)" "/var/lib/go-trader/a/state.db" \
    "absolute db_file is kept"
assert_eq "$(update_unit_dropin_path /etc/systemd/system go-trader@live.service 50-merge-paper-b)" \
    "/etc/systemd/system/go-trader@live.service.d/50-merge-paper-b.conf" "drop-in path"
assert_eq "$(update_paper_override_directive /var/lib/go-trader/paper)" "StateDirectory=go-trader/paper" \
    "a /var/lib instance directory becomes a StateDirectory directive"
assert_eq "$(update_paper_override_directive /var/lib/go-trader)" "StateDirectory=go-trader" \
    "a bare /var/lib directory becomes a StateDirectory directive"
assert_eq "$(update_paper_override_directive /opt/go-trader-paper/scheduler)" "ReadWritePaths=/opt/go-trader-paper/scheduler" \
    "a deploy-tree directory becomes a ReadWritePaths directive"

update_start_state_lock_holder "$merge_tmp/link/dir/state.db" || { echo "FAIL: lock holder did not start" >&2; exit 1; }
holder_pid="$UPDATE_LOCK_HOLDER_PID"
assert_eq "$(cat "${canon}.lock")" "$holder_pid" "the holder writes its pid into the ownership lock"
if [[ -s "${canon}.manual-action.lock" ]]; then
    echo "FAIL: the manual-action lock must stay empty" >&2
    exit 1
fi
contended=$(python3 -c '
import fcntl, os, sys
fd = os.open(sys.argv[1], os.O_RDWR)
try:
    fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
    print("free")
except OSError:
    print("held")
' "${canon}.lock")
assert_eq "$contended" "held" "the ownership lock is held while the holder runs"
second_out=$(
    UPDATE_LOCK_HOLDER_PID=""; UPDATE_LOCK_HOLDER_FD=""
    update_start_state_lock_holder "$merge_tmp/link/dir/state.db" 2>&1
) && second_rc=0 || second_rc=$?
assert_eq "$second_rc" "1" "a second holder on the same db is refused"
if [[ "$second_out" != *"CONTENDED ${canon}.lock pid=${holder_pid}"* ]]; then
    echo "FAIL: contention must name the lock and the holder pid, got: $second_out" >&2
    exit 1
fi
update_stop_state_lock_holder
released=$(python3 -c '
import fcntl, os, sys
fd = os.open(sys.argv[1], os.O_RDWR)
try:
    fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
    print("free")
except OSError:
    print("held")
' "${canon}.lock")
assert_eq "$released" "free" "stopping the holder releases the ownership lock"
rm -rf "$merge_tmp"

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

mkdir -p "$drift/inproc/scheduler" "$drift/inproc-drift/scheduler"
cat > "$drift/inproc/scheduler/config.json" <<'JSON'
{"config_version": 19, "replay_log_path": "/var/lib/go-trader/shared/replay.db", "strategies": [
  {"id": "hl-x-live", "type": "perps", "platform": "hyperliquid",
   "script": "shared_scripts/check_hyperliquid.py",
   "args": ["vwap", "ETH", "1h", "--mode=live"],
   "interval_seconds": 300, "leverage": 20, "margin_per_trade_usd": 50,
   "capital": 100, "close_strategy": "trailing_tp_ratchet_regime",
   "replay_sharing": "live_mirror"},
  {"id": "hl-x-paper", "type": "perps", "platform": "hyperliquid",
   "script": "shared_scripts/check_hyperliquid.py",
   "args": ["vwap", "ETH", "1h", "--mode=paper"],
   "interval_seconds": 300, "leverage": 20, "margin_per_trade_usd": 50,
   "capital": 100, "close_strategy": "trailing_tp_ratchet_regime",
   "replay_sharing": "live_mirror", "replay_source_id": "hl-x-live"}
]}
JSON

cat > "$drift/inproc-drift/scheduler/config.json" <<'JSON'
{"config_version": 19, "replay_log_path": "/var/lib/go-trader/shared/replay.db", "strategies": [
  {"id": "hl-x-live", "type": "perps", "platform": "hyperliquid",
   "script": "shared_scripts/check_hyperliquid.py",
   "args": ["vwap", "ETH", "1h", "--mode=live"],
   "interval_seconds": 300, "leverage": 20, "margin_per_trade_usd": 50,
   "capital": 100, "close_strategy": "trailing_tp_ratchet_regime",
   "replay_sharing": "live_mirror"},
  {"id": "hl-x-paper", "type": "perps", "platform": "hyperliquid",
   "script": "shared_scripts/check_hyperliquid.py",
   "args": ["vwap", "ETH", "1h", "--mode=paper"],
   "interval_seconds": 3600, "leverage": 20, "margin_per_trade_usd": 50,
   "capital": 100, "close_strategy": "trailing_tp_ratchet_regime",
   "replay_sharing": "live_mirror", "replay_source_id": "hl-x-live"}
]}
JSON

audit_out=$(bash "${SCRIPT_DIR}/check-live-paper-config-drift.sh" "$drift/inproc") && audit_rc=0 || audit_rc=$?
assert_eq "$audit_rc" "0" "drift audit: in-process twins paired by replay_source_id report no drift"
if [[ "$audit_out" != *"PAIR hl-x-live"* ]]; then
    echo "FAIL: expected the pair keyed on replay_source_id, got: $audit_out" >&2
    exit 1
fi
if [[ "$audit_out" != *"[id=hl-x-paper]"* ]]; then
    echo "FAIL: expected the mirror id annotated on the paper line, got: $audit_out" >&2
    exit 1
fi
if [[ "$audit_out" != *"IN SYNC"* || "$audit_out" == *"DRIFT"* || "$audit_out" == *"OTHER"* ]]; then
    echo "FAIL: differing id and replay_source_id must not read as drift, got: $audit_out" >&2
    exit 1
fi

audit_out=$(bash "${SCRIPT_DIR}/check-live-paper-config-drift.sh" "$drift/inproc-drift") && audit_rc=0 || audit_rc=$?
assert_eq "$audit_rc" "1" "drift audit: in-process pair still gates on cadence drift"
if [[ "$audit_out" != *"CANDIDATE"* || "$audit_out" != *"interval_seconds"* ]]; then
    echo "FAIL: expected interval_seconds drift for the in-process pair, got: $audit_out" >&2
    exit 1
fi

mkdir -p "$drift/alias/scheduler" "$drift/ambiguous/scheduler"
cat > "$drift/alias/scheduler/config.json" <<'JSON'
{"config_version": 19, "strategies": [
  {"id": "hl-y", "type": "perps", "platform": "hyperliquid",
   "script": "shared_scripts/check_hyperliquid.py",
   "args": ["vwap", "ETH", "1h", "--mode=live"],
   "interval_seconds": 300, "leverage": 20, "margin_per_trade_usd": 50, "capital": 100},
  {"id": "hl-y-paper", "storage_strategy_id": "hl-y", "type": "perps", "platform": "hyperliquid",
   "script": "shared_scripts/check_hyperliquid.py",
   "args": ["vwap", "ETH", "1h", "--mode=paper"],
   "interval_seconds": 300, "leverage": 20, "margin_per_trade_usd": 50, "capital": 100}
]}
JSON
cat > "$drift/ambiguous/scheduler/config.json" <<'JSON'
{"config_version": 19, "strategies": [
  {"id": "hl-y", "type": "perps", "platform": "hyperliquid",
   "script": "shared_scripts/check_hyperliquid.py",
   "args": ["vwap", "ETH", "1h", "--mode=live"],
   "interval_seconds": 300, "leverage": 20, "margin_per_trade_usd": 50, "capital": 100},
  {"id": "hl-y-paper", "type": "perps", "platform": "hyperliquid",
   "script": "shared_scripts/check_hyperliquid.py",
   "args": ["vwap", "ETH", "1h", "--mode=paper"],
   "interval_seconds": 300, "leverage": 20, "margin_per_trade_usd": 50, "capital": 100}
]}
JSON
audit_out=$(bash "${SCRIPT_DIR}/check-live-paper-config-drift.sh" "$drift/alias") && audit_rc=0 || audit_rc=$?
assert_eq "$audit_rc" "0" "drift audit: in-process twins paired by storage_strategy_id exit 0"
if [[ "$audit_out" != *"PAIR hl-y"* || "$audit_out" != *"[id=hl-y-paper]"* || "$audit_out" != *"IN SYNC"* ]]; then
    echo "FAIL: expected the storage-alias pair in sync, got: $audit_out" >&2
    exit 1
fi
audit_out=$(bash "${SCRIPT_DIR}/check-live-paper-config-drift.sh" "$drift/ambiguous") && audit_rc=0 || audit_rc=$?
assert_eq "$audit_rc" "1" "drift audit: a -paper suffix alone is ambiguous and gates"
if [[ "$audit_out" != *"AMBIGUOUS hl-y-paper"* || "$audit_out" == *"PAIR hl-y"* ]]; then
    echo "FAIL: expected an AMBIGUOUS line and no pair, got: $audit_out" >&2
    exit 1
fi

mkdir -p "$drift/notwin/scheduler"
cat > "$drift/notwin/scheduler/config.json" <<'JSON'
{"config_version": 19, "strategies": [
  {"id": "hl-z-paper", "storage_strategy_id": "hl-z", "type": "perps", "platform": "hyperliquid",
   "script": "shared_scripts/check_hyperliquid.py",
   "args": ["vwap", "ETH", "1h", "--mode=paper"],
   "interval_seconds": 300, "leverage": 20, "margin_per_trade_usd": 50, "capital": 100}
]}
JSON
audit_out=$(bash "${SCRIPT_DIR}/check-live-paper-config-drift.sh" "$drift/notwin") && audit_rc=0 || audit_rc=$?
assert_eq "$audit_rc" "0" "drift audit: a paper alias with no live twin is reported and does not gate"
if [[ "$audit_out" != *"UNPAIRED (no live twin) hl-z-paper"* ]]; then
    echo "FAIL: expected an UNPAIRED no-live-twin line, got: $audit_out" >&2
    exit 1
fi
if [[ "$audit_out" == *"AMBIGUOUS hl-z-paper"* ]]; then
    echo "FAIL: a paper alias with no live base must not be ambiguous, got: $audit_out" >&2
    exit 1
fi
audit_out=$(bash "${SCRIPT_DIR}/check-live-paper-config-drift.sh" "$drift/alias") && audit_rc=0 || audit_rc=$?
assert_eq "$audit_rc" "0" "drift audit: a paper alias whose live twin exists still pairs"
if [[ "$audit_out" == *"UNPAIRED (no live twin)"* ]]; then
    echo "FAIL: a paired alias must not be reported as having no live twin, got: $audit_out" >&2
    exit 1
fi
audit_out=$(bash "${SCRIPT_DIR}/check-live-paper-config-drift.sh" "$drift/live" "$drift/paper2") && audit_rc=0 || audit_rc=$?
assert_eq "$audit_rc" "0" "drift audit: an incompatible timeframe pair is reported and left alone"
if [[ "$audit_out" != *"INCOMPATIBLE timeframe"* || "$audit_out" != *"SKIP — INCOMPATIBLE"* ]]; then
    echo "FAIL: expected an INCOMPATIBLE timeframe marker, got: $audit_out" >&2
    exit 1
fi

mkdir -p "$drift/source/scheduler" "$drift/source-ambiguous/scheduler" "$drift/source-unset/scheduler"
cat > "$drift/source/scheduler/config.json" <<'JSON'
{"config_version": 19,
 "paper_sources": [{"id": "btc", "db_file": "/var/lib/go-trader/btc.db"}],
 "strategies": [
  {"id": "hl-y", "type": "perps", "platform": "hyperliquid",
   "script": "shared_scripts/check_hyperliquid.py",
   "args": ["vwap", "ETH", "1h", "--mode=live"],
   "interval_seconds": 300, "leverage": 20, "margin_per_trade_usd": 50, "capital": 100},
  {"id": "hl-y-paper-btc", "storage_strategy_id": "hl-y", "paper_source": "btc",
   "type": "perps", "platform": "hyperliquid",
   "script": "shared_scripts/check_hyperliquid.py",
   "args": ["vwap", "ETH", "1h", "--mode=paper"],
   "interval_seconds": 300, "leverage": 20, "margin_per_trade_usd": 50, "capital": 100}
]}
JSON
cat > "$drift/source-ambiguous/scheduler/config.json" <<'JSON'
{"config_version": 19,
 "paper_sources": [{"id": "btc", "db_file": "/var/lib/go-trader/btc.db"}],
 "strategies": [
  {"id": "hl-y", "type": "perps", "platform": "hyperliquid",
   "script": "shared_scripts/check_hyperliquid.py",
   "args": ["vwap", "ETH", "1h", "--mode=live"],
   "interval_seconds": 300, "leverage": 20, "margin_per_trade_usd": 50, "capital": 100},
  {"id": "hl-y-paper-btc", "paper_source": "btc",
   "type": "perps", "platform": "hyperliquid",
   "script": "shared_scripts/check_hyperliquid.py",
   "args": ["vwap", "ETH", "1h", "--mode=paper"],
   "interval_seconds": 300, "leverage": 20, "margin_per_trade_usd": 50, "capital": 100}
]}
JSON
cat > "$drift/source-unset/scheduler/config.json" <<'JSON'
{"config_version": 19, "strategies": [
  {"id": "hl-y", "type": "perps", "platform": "hyperliquid",
   "script": "shared_scripts/check_hyperliquid.py",
   "args": ["vwap", "ETH", "1h", "--mode=live"],
   "interval_seconds": 300, "leverage": 20, "margin_per_trade_usd": 50, "capital": 100},
  {"id": "hl-y-paper-btc", "type": "perps", "platform": "hyperliquid",
   "script": "shared_scripts/check_hyperliquid.py",
   "args": ["vwap", "ETH", "1h", "--mode=paper"],
   "interval_seconds": 300, "leverage": 20, "margin_per_trade_usd": 50, "capital": 100}
]}
JSON
audit_out=$(bash "${SCRIPT_DIR}/check-live-paper-config-drift.sh" "$drift/source") && audit_rc=0 || audit_rc=$?
assert_eq "$audit_rc" "0" "drift audit: a named paper source pairs through storage_strategy_id"
if [[ "$audit_out" != *"PAIR hl-y"* || "$audit_out" != *"[id=hl-y-paper-btc]"* || "$audit_out" != *"IN SYNC"* ]]; then
    echo "FAIL: expected the named-source pair in sync, got: $audit_out" >&2
    exit 1
fi
if [[ "$audit_out" == *"OTHER  paper_source"* ]]; then
    echo "FAIL: paper_source is the pairing key and must not read as drift, got: $audit_out" >&2
    exit 1
fi
audit_out=$(bash "${SCRIPT_DIR}/check-live-paper-config-drift.sh" "$drift/source-ambiguous") && audit_rc=0 || audit_rc=$?
assert_eq "$audit_rc" "1" "drift audit: a -paper-<id> suffix alone is ambiguous and gates"
if [[ "$audit_out" != *"AMBIGUOUS hl-y-paper-btc"* || "$audit_out" != *"the -paper-btc suffix alone"* ]]; then
    echo "FAIL: expected an AMBIGUOUS line naming the -paper-btc suffix, got: $audit_out" >&2
    exit 1
fi
if [[ "$audit_out" == *"PAIR hl-y"* ]]; then
    echo "FAIL: an unproven named-source alias must not pair, got: $audit_out" >&2
    exit 1
fi
audit_out=$(bash "${SCRIPT_DIR}/check-live-paper-config-drift.sh" "$drift/source-unset") && audit_rc=0 || audit_rc=$?
assert_eq "$audit_rc" "0" "drift audit: a -paper-<id> id without paper_source is not read as an alias"
if [[ "$audit_out" == *"PAIR hl-y"* || "$audit_out" == *"AMBIGUOUS"* || "$audit_out" == *"UNPAIRED (no live twin)"* ]]; then
    echo "FAIL: the suffix must only be read when paper_source names the source, got: $audit_out" >&2
    exit 1
fi

# A fleet folded from a pre-source deployment keeps its strategy ids, so a
# block can carry the bare -paper alias and a paper_source at the same time.
# Naming a source must never unread an alias the audit read before.
mkdir -p "$drift/source-bare/scheduler" "$drift/source-bare-drift/scheduler" \
    "$drift/source-bare-unproven/scheduler" "$drift/source-collision/scheduler" \
    "$drift/storage-only/scheduler" "$drift/storage-nolive/scheduler"
write_folded_config() {
    local path="$1" paper_id="$2" paper_interval="$3" extra="$4"
    cat > "$path" <<JSON
{"config_version": 19,
 "paper_sources": [{"id": "btc", "db_file": "/var/lib/go-trader/btc.db"}],
 "strategies": [
  {"id": "hl-y", "type": "perps", "platform": "hyperliquid",
   "script": "shared_scripts/check_hyperliquid.py",
   "args": ["vwap", "ETH", "1h", "--mode=live"],
   "interval_seconds": 300, "leverage": 20, "margin_per_trade_usd": 50, "capital": 100},
  {"id": "$paper_id", "paper_source": "btc"$extra,
   "type": "perps", "platform": "hyperliquid",
   "script": "shared_scripts/check_hyperliquid.py",
   "args": ["vwap", "ETH", "1h", "--mode=paper"],
   "interval_seconds": $paper_interval, "leverage": 20, "margin_per_trade_usd": 50, "capital": 100}
]}
JSON
}
write_folded_config "$drift/source-bare/scheduler/config.json" "hl-y-paper" 300 ', "storage_strategy_id": "hl-y"'
write_folded_config "$drift/source-bare-drift/scheduler/config.json" "hl-y-paper" 3600 ', "storage_strategy_id": "hl-y"'
write_folded_config "$drift/source-bare-unproven/scheduler/config.json" "hl-y-paper" 300 ""
write_folded_config "$drift/source-collision/scheduler/config.json" "hl-y-paper2" 3600 ', "storage_strategy_id": "hl-y"'

audit_out=$(bash "${SCRIPT_DIR}/check-live-paper-config-drift.sh" "$drift/source-bare") && audit_rc=0 || audit_rc=$?
assert_eq "$audit_rc" "0" "drift audit: a bare -paper alias under a named source still pairs"
if [[ "$audit_out" != *"PAIR hl-y"* || "$audit_out" != *"[id=hl-y-paper]"* || "$audit_out" != *"IN SYNC"* ]]; then
    echo "FAIL: naming a source must not unread the bare -paper alias, got: $audit_out" >&2
    exit 1
fi
audit_out=$(bash "${SCRIPT_DIR}/check-live-paper-config-drift.sh" "$drift/source-bare-drift") && audit_rc=0 || audit_rc=$?
assert_eq "$audit_rc" "1" "drift audit: cadence drift on a bare-aliased folded twin still gates"
if [[ "$audit_out" != *"interval_seconds"* || "$audit_out" != *"CANDIDATE"* ]]; then
    echo "FAIL: expected interval_seconds drift on the bare-aliased folded pair, got: $audit_out" >&2
    exit 1
fi
audit_out=$(bash "${SCRIPT_DIR}/check-live-paper-config-drift.sh" "$drift/source-bare-unproven") && audit_rc=0 || audit_rc=$?
assert_eq "$audit_rc" "1" "drift audit: a bare -paper alias under a named source with no storage id gates"
if [[ "$audit_out" != *"AMBIGUOUS hl-y-paper"* || "$audit_out" != *"the -paper suffix alone"* ]]; then
    echo "FAIL: expected an AMBIGUOUS line naming the bare suffix, got: $audit_out" >&2
    exit 1
fi
audit_out=$(bash "${SCRIPT_DIR}/check-live-paper-config-drift.sh" "$drift/source-collision") && audit_rc=0 || audit_rc=$?
assert_eq "$audit_rc" "1" "drift audit: the -paper<n> collision form under a named source still pairs and gates"
if [[ "$audit_out" != *"PAIR hl-y"* || "$audit_out" != *"[id=hl-y-paper2]"* || "$audit_out" != *"interval_seconds"* ]]; then
    echo "FAIL: expected the collision-form pair to report drift, got: $audit_out" >&2
    exit 1
fi

# storage_strategy_id is the same proof the alias branch demands, so a block
# that carries it is surfaced even when no alias rule reads its id.
cat > "$drift/storage-only/scheduler/config.json" <<'JSON'
{"config_version": 19, "strategies": [
  {"id": "hl-y", "type": "perps", "platform": "hyperliquid",
   "script": "shared_scripts/check_hyperliquid.py",
   "args": ["vwap", "ETH", "1h", "--mode=live"],
   "interval_seconds": 300, "leverage": 20, "margin_per_trade_usd": 50, "capital": 100},
  {"id": "hl-shadow", "storage_strategy_id": "hl-y",
   "type": "perps", "platform": "hyperliquid",
   "script": "shared_scripts/check_hyperliquid.py",
   "args": ["vwap", "ETH", "1h", "--mode=paper"],
   "interval_seconds": 3600, "leverage": 20, "margin_per_trade_usd": 50, "capital": 100}
]}
JSON
cat > "$drift/storage-nolive/scheduler/config.json" <<'JSON'
{"config_version": 19, "strategies": [
  {"id": "hl-y", "type": "perps", "platform": "hyperliquid",
   "script": "shared_scripts/check_hyperliquid.py",
   "args": ["vwap", "ETH", "1h", "--mode=live"],
   "interval_seconds": 300, "leverage": 20, "margin_per_trade_usd": 50, "capital": 100},
  {"id": "hl-shadow", "storage_strategy_id": "hl-absent",
   "type": "perps", "platform": "hyperliquid",
   "script": "shared_scripts/check_hyperliquid.py",
   "args": ["vwap", "ETH", "1h", "--mode=paper"],
   "interval_seconds": 3600, "leverage": 20, "margin_per_trade_usd": 50, "capital": 100}
]}
JSON
audit_out=$(bash "${SCRIPT_DIR}/check-live-paper-config-drift.sh" "$drift/storage-only") && audit_rc=0 || audit_rc=$?
assert_eq "$audit_rc" "1" "drift audit: a storage_strategy_id naming a live id pairs an unaliased block"
if [[ "$audit_out" != *"PAIR hl-y"* || "$audit_out" != *"[id=hl-shadow]"* || "$audit_out" != *"interval_seconds"* ]]; then
    echo "FAIL: expected the storage-id pair to report drift, got: $audit_out" >&2
    exit 1
fi
audit_out=$(bash "${SCRIPT_DIR}/check-live-paper-config-drift.sh" "$drift/storage-nolive") && audit_rc=0 || audit_rc=$?
assert_eq "$audit_rc" "0" "drift audit: a storage_strategy_id matching no live id stays silent"
if [[ "$audit_out" == *"PAIR"* || "$audit_out" == *"AMBIGUOUS"* || "$audit_out" == *"UNPAIRED"* ]]; then
    echo "FAIL: an unmatched storage id must not pair or gate, got: $audit_out" >&2
    exit 1
fi

# Adopting the -paper-<id> name must never drop a gate the bare -paper rule
# applied: an id spelled for a source this deployment declares, with neither
# paper_source nor storage_strategy_id, is reported when its base is live.
mkdir -p "$drift/declared/scheduler" "$drift/declared-nolive/scheduler" \
    "$drift/declared-other/scheduler" "$drift/declared-paperonly/scheduler"
write_declared_config() {
    local path="$1" source_id="$2" live_id="$3" paper_id="$4"
    local live_block=""
    if [[ -n "$live_id" ]]; then
        live_block=$(cat <<JSON
  {"id": "$live_id", "type": "perps", "platform": "hyperliquid",
   "script": "shared_scripts/check_hyperliquid.py",
   "args": ["vwap", "ETH", "1h", "--mode=live"],
   "interval_seconds": 300, "leverage": 20, "margin_per_trade_usd": 50, "capital": 100},
JSON
)
    fi
    cat > "$path" <<JSON
{"config_version": 19,
 "paper_sources": [{"id": "$source_id", "db_file": "/var/lib/go-trader/$source_id.db"}],
 "strategies": [
$live_block
  {"id": "$paper_id", "type": "perps", "platform": "hyperliquid",
   "script": "shared_scripts/check_hyperliquid.py",
   "args": ["vwap", "ETH", "1h", "--mode=paper"],
   "interval_seconds": 3600, "leverage": 20, "margin_per_trade_usd": 50, "capital": 100}
]}
JSON
}
write_declared_config "$drift/declared/scheduler/config.json" btc hl-y hl-y-paper-btc
write_declared_config "$drift/declared-nolive/scheduler/config.json" rsi hl-y hl-paper-rsi
write_declared_config "$drift/declared-other/scheduler/config.json" btc hl-y hl-y-paper-eth
write_declared_config "$drift/declared-paperonly/scheduler/config.json" btc "" hl-y-paper-btc

audit_out=$(bash "${SCRIPT_DIR}/check-live-paper-config-drift.sh" "$drift/declared") && audit_rc=0 || audit_rc=$?
assert_eq "$audit_rc" "1" "drift audit: a declared-source name with no pairing key is reported and gates"
if [[ "$audit_out" != *"AMBIGUOUS hl-y-paper-btc"* || "$audit_out" != *"the -paper-btc suffix alone"* ]]; then
    echo "FAIL: expected an AMBIGUOUS line for the declared-source name, got: $audit_out" >&2
    exit 1
fi
if [[ "$audit_out" == *"PAIR hl-y"* ]]; then
    echo "FAIL: a name alone must not pair, got: $audit_out" >&2
    exit 1
fi
audit_out=$(bash "${SCRIPT_DIR}/check-live-paper-config-drift.sh" "$drift/declared-nolive") && audit_rc=0 || audit_rc=$?
assert_eq "$audit_rc" "0" "drift audit: a declared-source name whose base is not live stays silent"
if [[ "$audit_out" == *"AMBIGUOUS"* || "$audit_out" == *"UNPAIRED"* || "$audit_out" == *"PAIR"* ]]; then
    echo "FAIL: hl-paper-rsi must stay silent when hl is not live, got: $audit_out" >&2
    exit 1
fi
audit_out=$(bash "${SCRIPT_DIR}/check-live-paper-config-drift.sh" "$drift/declared-other") && audit_rc=0 || audit_rc=$?
assert_eq "$audit_rc" "0" "drift audit: a -paper-<id> name for an undeclared source stays silent"
if [[ "$audit_out" == *"AMBIGUOUS"* || "$audit_out" == *"UNPAIRED"* || "$audit_out" == *"PAIR"* ]]; then
    echo "FAIL: an undeclared source id must stay silent, got: $audit_out" >&2
    exit 1
fi
audit_out=$(bash "${SCRIPT_DIR}/check-live-paper-config-drift.sh" "$drift/declared-paperonly") && audit_rc=0 || audit_rc=$?
assert_eq "$audit_rc" "0" "drift audit: a paper-only deployment prints no line for a declared-source name"
if [[ "$audit_out" == *"AMBIGUOUS"* || "$audit_out" == *"UNPAIRED"* || "$audit_out" == *"PAIR"* ]]; then
    echo "FAIL: an empty live set must print nothing, got: $audit_out" >&2
    exit 1
fi

if [[ -n "${GO_TRADER_BIN:-}" && -x "${GO_TRADER_BIN:-}" ]]; then
    mkdir -p "$drift/eff-live/scheduler" "$drift/eff-paper/scheduler"
    cp "$GO_TRADER_BIN" "$drift/eff-live/go-trader"
    cp "$GO_TRADER_BIN" "$drift/eff-paper/go-trader"
    printf 'HYPERLIQUID_SECRET_KEY=fixture\n' > "$drift/eff-live/.env"
    cat > "$drift/eff-live/scheduler/config.json" <<'JSON'
{"config_version": 19, "interval_seconds": 300, "strategies": [
  {"id": "hl-z", "type": "perps", "platform": "hyperliquid",
   "script": "shared_scripts/check_hyperliquid.py",
   "args": ["vwap", "ETH", "1h", "--mode=live"],
   "leverage": 20, "margin_per_trade_usd": 50, "capital": 100}
]}
JSON
    cat > "$drift/eff-paper/scheduler/config.json" <<'JSON'
{"config_version": 19, "interval_seconds": 600, "strategies": [
  {"id": "hl-z", "type": "perps", "platform": "hyperliquid",
   "script": "shared_scripts/check_hyperliquid.py",
   "args": ["vwap", "ETH", "1h", "--mode=paper"],
   "leverage": 20, "margin_per_trade_usd": 50, "capital": 100}
]}
JSON
    audit_out=$(bash "${SCRIPT_DIR}/check-live-paper-config-drift.sh" "$drift/eff-live" "$drift/eff-paper") && audit_rc=0 || audit_rc=$?
    assert_eq "$audit_rc" "1" "drift audit: root cadence differences surface as effective drift"
    if [[ "$audit_out" != *"interval_seconds"* || "$audit_out" != *"live=300"* || "$audit_out" != *"paper=600 (effective)"* ]]; then
        echo "FAIL: expected an effective interval_seconds drift line, got: $audit_out" >&2
        exit 1
    fi
    if [[ "$audit_out" == *"RAW"* ]]; then
        echo "FAIL: a deployment with a binary must not report RAW, got: $audit_out" >&2
        exit 1
    fi
    mkdir -p "$drift/eff-source/scheduler"
    cp "$GO_TRADER_BIN" "$drift/eff-source/go-trader"
    printf 'HYPERLIQUID_SECRET_KEY=fixture\n' > "$drift/eff-source/.env"
    cat > "$drift/eff-source/scheduler/config.json" <<JSON
{"config_version": 19, "interval_seconds": 300,
 "db_file": "$drift/eff-source/live.db",
 "paper_sources": [{"id": "btc", "label": "Paper BTC", "db_file": "$drift/eff-source/btc.db"}],
 "strategies": [
  {"id": "hl-z", "type": "perps", "platform": "hyperliquid",
   "script": "shared_scripts/check_hyperliquid.py",
   "args": ["vwap", "ETH", "1h", "--mode=live"],
   "leverage": 20, "margin_per_trade_usd": 50, "capital": 100},
  {"id": "hl-z-paper-btc", "storage_strategy_id": "hl-z", "paper_source": "btc",
   "type": "perps", "platform": "hyperliquid",
   "script": "shared_scripts/check_hyperliquid.py",
   "args": ["vwap", "ETH", "1h", "--mode=paper"],
   "leverage": 20, "margin_per_trade_usd": 50, "capital": 100}
]}
JSON
    audit_out=$(bash "${SCRIPT_DIR}/check-live-paper-config-drift.sh" "$drift/eff-source") && audit_rc=0 || audit_rc=$?
    assert_eq "$audit_rc" "0" "drift audit: a folded paper source pairs against the binary's effective view"
    if [[ "$audit_out" != *"PAIR hl-z"* || "$audit_out" != *"[id=hl-z-paper-btc]"* || "$audit_out" != *"IN SYNC (effective)"* ]]; then
        echo "FAIL: expected the folded source pair in sync on the effective view, got: $audit_out" >&2
        exit 1
    fi
    if [[ "$audit_out" == *"paper_source"* || "$audit_out" == *"AMBIGUOUS"* || "$audit_out" == *"RAW"* ]]; then
        echo "FAIL: the pairing keys must not read as drift, got: $audit_out" >&2
        exit 1
    fi

    mkdir -p "$drift/eff-paper/out"
    mv "$drift/eff-paper/scheduler/config.json" "$drift/eff-paper/out/config.json"
    python3 - "$drift/eff-paper/out/config.json" <<'PY'
import json, sys
p = sys.argv[1]
cfg = json.load(open(p))
cfg["config_version"] = 15
json.dump(cfg, open(p, "w"))
PY
    ln -s "$drift/eff-paper/out/config.json" "$drift/eff-paper/scheduler/config.json"
    old_bytes=$(cat "$drift/eff-paper/out/config.json")
    audit_out=$(bash "${SCRIPT_DIR}/check-live-paper-config-drift.sh" "$drift/eff-live" "$drift/eff-paper") && audit_rc=0 || audit_rc=$?
    assert_eq "$audit_rc" "1" "drift audit: a config below the current version still yields an effective view"
    if [[ "$audit_out" == *"RAW"* ]]; then
        echo "FAIL: a v15 config beside a binary must not fall back to RAW, got: $audit_out" >&2
        exit 1
    fi
    [[ -L "$drift/eff-paper/scheduler/config.json" ]] || { echo "FAIL: the drift audit replaced the transition symlink with a regular file" >&2; exit 1; }
    assert_eq "$(cat "$drift/eff-paper/out/config.json")" "$old_bytes" "drift audit with a binary beside the config is read-only (no migration rewrite)"
    [[ ! -e "$drift/eff-paper/out/config.json.tmp" ]] || { echo "FAIL: the drift audit left a migration temp file" >&2; exit 1; }
    rm -f "$drift/eff-paper/go-trader"
    audit_out=$(bash "${SCRIPT_DIR}/check-live-paper-config-drift.sh" "$drift/eff-live" "$drift/eff-paper") && audit_rc=0 || audit_rc=$?
    assert_eq "$audit_rc" "0" "drift audit: a deployment without a binary falls back to raw keys"
    if [[ "$audit_out" != *"(RAW: no go-trader binary"* || "$audit_out" != *"IN SYNC (RAW)"* ]]; then
        echo "FAIL: expected the RAW marker on the fallback pair, got: $audit_out" >&2
        exit 1
    fi
else
    echo "note: GO_TRADER_BIN unset; effective-cadence drift case skipped"
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

assert_eq "$(update_git_trust_decision 0 1001 0)" "trust" "root trusts a tree whose top another account owns"
assert_eq "$(update_git_trust_decision 0 0 1001)" "trust" "root trusts a tree whose .git another account owns"
assert_eq "$(update_git_trust_decision 0 0 0)" "" "root does not add trust for its own tree"
assert_eq "$(update_git_trust_decision 1001 0 1002)" "" "a non-root caller never adds trust"
trust_env=$(GIT_CONFIG_COUNT='' update_git_trust_env /opt/go-trader-paper 0 1001 1001)
assert_eq "$trust_env" "$(printf '%s\n' GIT_CONFIG_KEY_0=safe.directory GIT_CONFIG_VALUE_0=/opt/go-trader-paper GIT_CONFIG_KEY_1=core.fsmonitor GIT_CONFIG_VALUE_1=false GIT_CONFIG_KEY_2=core.hooksPath GIT_CONFIG_VALUE_2=/dev/null GIT_CONFIG_COUNT=3)" \
    "trust names only the exact tree, never *"
assert_eq "$(GIT_CONFIG_COUNT=2 update_git_trust_env /t 0 5 5 | tail -n 1)" "GIT_CONFIG_COUNT=5" "trust appends after existing GIT_CONFIG entries"
assert_eq "$(update_git_trust_env /t 0 0 0)" "" "no trust env for a root-owned tree"

if command -v git >/dev/null 2>&1; then
    gt=$(mktemp -d)
    gt=$(cd "$gt" && pwd -P)
    gtenv=(GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1 GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@example.com GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@example.com)
    env "${gtenv[@]}" git init -q "$gt/origin.git" --bare
    env "${gtenv[@]}" git init -q "$gt/tree"
    mkdir -p "$gt/tree/scheduler"
    echo one >"$gt/tree/scheduler/f.txt"
    env "${gtenv[@]}" git -C "$gt/tree" add -A
    env "${gtenv[@]}" git -C "$gt/tree" commit -qm one
    env "${gtenv[@]}" git -C "$gt/tree" remote add origin "$gt/origin.git"
    assert_eq "$(update_git_top "$gt/tree/scheduler")" "$gt/tree" "update_git_top walks up to the checkout"
    if update_git_top "$gt" >/dev/null; then
        echo "FAIL: update_git_top found a checkout above a plain directory" >&2
        exit 1
    fi

    dubious=$(env "${gtenv[@]}" GIT_TEST_ASSUME_DIFFERENT_OWNER=1 git -C "$gt/tree" rev-parse HEAD 2>&1) && dubious_rc=0 || dubious_rc=$?
    if [[ "$dubious_rc" == 0 || "$dubious" != *"dubious ownership"* ]]; then
        echo "note: this git does not simulate another owner; the trust and refusal cases are skipped ($dubious)"
    else
        trusted=()
        while IFS= read -r line; do
            [[ -n "$line" ]] && trusted+=("$line")
        done <<<"$(update_git_trust_env "$gt/tree" 0 1001 1001)"
        trusted_head=$(env "${gtenv[@]}" "${trusted[@]}" GIT_TEST_ASSUME_DIFFERENT_OWNER=1 git -C "$gt/tree/scheduler" rev-parse HEAD 2>&1) \
            || { echo "FAIL: git refused the tree with the per-command trust: $trusted_head" >&2; exit 1; }
        assert_eq "$trusted_head" "$(env "${gtenv[@]}" git -C "$gt/tree" rev-parse HEAD)" "per-command trust reads the tree another account owns"
        other_env=()
        while IFS= read -r line; do
            [[ -n "$line" ]] && other_env+=("$line")
        done <<<"$(update_git_trust_env "$gt/other" 0 1001 1001)"
        if env "${gtenv[@]}" "${other_env[@]}" GIT_TEST_ASSUME_DIFFERENT_OWNER=1 git -C "$gt/tree" rev-parse HEAD >/dev/null 2>&1; then
            echo "FAIL: trust for another path let git read this tree" >&2
            exit 1
        fi

        origin_err=$(env "${gtenv[@]}" GIT_TEST_ASSUME_DIFFERENT_OWNER=1 bash -c 'source "$1"; update_git "$2" remote get-url origin' _ "$SCRIPT_DIR/update_helpers.sh" "$gt/tree" 2>&1) && origin_rc=0 || origin_rc=$?
        if [[ "$origin_rc" == 0 ]]; then
            echo "FAIL: update_git hid a git refusal" >&2
            exit 1
        fi
        owner_name=$(update_path_owner_name "$gt/tree")
        for want in "git refused $gt/tree" "owned by $owner_name" "dubious ownership"; do
            if [[ "$origin_err" != *"$want"* ]]; then
                echo "FAIL: the refusal note lacks '$want': $origin_err" >&2
                exit 1
            fi
        done
        if [[ "$origin_err" == *"no git origin"* ]]; then
            echo "FAIL: a refusal was reported as a missing origin: $origin_err" >&2
            exit 1
        fi
    fi

    assert_eq "$(env "${gtenv[@]}" bash -c 'source "$1"; update_git "$2" remote get-url origin' _ "$SCRIPT_DIR/update_helpers.sh" "$gt/tree/scheduler")" \
        "$gt/origin.git" "update_git passes stdout through"
    env "${gtenv[@]}" git -C "$gt/tree" remote remove origin
    missing_err=$(env "${gtenv[@]}" bash -c 'source "$1"; update_git "$2" remote get-url origin' _ "$SCRIPT_DIR/update_helpers.sh" "$gt/tree" 2>&1) && missing_rc=0 || missing_rc=$?
    if [[ "$missing_rc" == 0 || "$missing_err" != *"git remote failed in $gt/tree"* || "$missing_err" != *"tree owner"* || "$missing_err" != *"origin"* ]]; then
        echo "FAIL: a missing remote must name the tree, the owner and the git error: $missing_err" >&2
        exit 1
    fi
    diff_rc=0
    echo two >"$gt/tree/scheduler/f.txt"
    diff_err=$(env "${gtenv[@]}" bash -c 'source "$1"; update_git "$2" diff --quiet' _ "$SCRIPT_DIR/update_helpers.sh" "$gt/tree" 2>&1) || diff_rc=$?
    assert_eq "$diff_rc" "1" "git diff --quiet keeps its exit status through update_git"
    assert_eq "$diff_err" "" "a quiet diff prints no failure note"

    base=$(env "${gtenv[@]}" git -C "$gt/tree" describe --tags --always)
    assert_eq "$(env "${gtenv[@]}" bash -c 'source "$1"; update_git_version "$2"' _ "$SCRIPT_DIR/update_helpers.sh" "$gt/tree")" "${base}-mod" "a tracked change stamps -mod"
    env "${gtenv[@]}" git -C "$gt/tree" checkout -q -- scheduler/f.txt
    echo new >"$gt/tree/untracked.txt"
    assert_eq "$(env "${gtenv[@]}" bash -c 'source "$1"; update_git_version "$2"' _ "$SCRIPT_DIR/update_helpers.sh" "$gt/tree")" "$base" "an untracked file does not stamp -mod"
    assert_eq "$base" "$(env "${gtenv[@]}" git -C "$gt/tree" describe --tags --always --dirty=-mod)" "update_git_version matches describe --dirty=-mod"
    rm -rf "$gt"
else
    echo "note: git not installed; update_git cases skipped"
fi

tools_dir=$(mktemp -d)
mkdir -p "$tools_dir/bin"
printf '#!/bin/sh\necho stub\n' >"$tools_dir/bin/uv"
chmod 0755 "$tools_dir/bin/uv"
assert_eq "$(PATH="$tools_dir/bin:/usr/bin:/bin" update_resolve_tool uv)" "$tools_dir/bin/uv" "uv resolves from PATH first"
home_dir=$(mktemp -d)
mkdir -p "$home_dir/.local/bin"
printf '#!/bin/sh\necho home\n' >"$home_dir/.local/bin/uv"
chmod 0755 "$home_dir/.local/bin/uv"
home_uv=$(HOME="$home_dir" PATH="/usr/bin:/bin" update_resolve_tool uv || true)
if [[ "$home_uv" == "$home_dir"/* ]]; then
    echo "FAIL: update_resolve_tool took uv from a home directory: $home_uv" >&2
    exit 1
fi
rm -rf "$home_dir"
for fixed in /usr/local/bin/uv /usr/bin/uv /opt/homebrew/bin/uv; do
    if [[ -x "$fixed" ]]; then
        echo "note: $fixed exists; the uv-missing case is skipped"
        fixed=""
        break
    fi
done
if [[ -n "${fixed:-}" ]]; then
    if PATH="/usr/bin:/bin" update_resolve_tool uv >/dev/null; then
        echo "FAIL: uv resolved with no uv on PATH or at a fixed path" >&2
        exit 1
    fi
    tools_out=$(PATH="/usr/bin:/bin" update_build_tools_preflight "$(update_current_account)") && tools_rc=0 || tools_rc=$?
    assert_eq "$tools_rc" "1" "the tools preflight fails without uv"
    if [[ "$tools_out" != *"finds no uv"* || "$tools_out" != *"UV_INSTALL_DIR=/usr/local/bin"* ]]; then
        echo "FAIL: the tools preflight did not name the missing uv and the fix: $tools_out" >&2
        exit 1
    fi
fi
tools_out=$(PATH="$tools_dir/bin:$PATH" update_build_tools_preflight "$(update_current_account)" 2>&1) && tools_rc=0 || tools_rc=$?
if [[ "$tools_out" != *"uv: $(update_current_account) runs $tools_dir/bin/uv"* ]]; then
    echo "FAIL: the tools preflight did not report the uv it runs: $tools_out" >&2
    exit 1
fi
rm -rf "$tools_dir"

if [[ "$EUID" != "0" ]]; then
    own=$(mktemp -d)
    assert_eq "$(update_tree_foreign_owner "$own")" "" "a non-root caller never restores ownership"
    rm -rf "$own"
fi

give_dir=$(mktemp -d)
mkdir -p "$give_dir/tree/sub"
echo outside >"$give_dir/outside.txt"
ln "$give_dir/outside.txt" "$give_dir/tree/sub/linked.txt"
echo own >"$give_dir/tree/sub/own.txt"
give_out=$(update_give_tree "$give_dir/tree" "$(id -u):$(id -g)" 2>&1)
if [[ "$give_out" != *"1 file(s) under $give_dir/tree share their data"* || "$give_out" != *"linked.txt"* ]]; then
    echo "FAIL: update_give_tree did not skip and report the hard-linked file: $give_out" >&2
    exit 1
fi
rm -rf "$give_dir"

wide=$(mktemp -d)
mkdir -p "$wide/tree"
for i in $(seq 1 300); do
    mkdir "$wide/tree/d$i"
    : >"$wide/tree/d$i/f"
done
wide_out=$( (ulimit -n 64 && update_give_tree "$wide/tree" "$(id -u):$(id -g)") 2>&1) && wide_rc=0 || wide_rc=$?
[[ "$wide_rc" == "0" ]] || { echo "FAIL: the give-back failed on a wide directory: $wide_out" >&2; exit 1; }
wide_gave=$( (ulimit -n 64 && python3 -I -c "$UPDATE_OWNER_PY" give "$wide/tree" "$(id -u):$(id -g)") | sed -n 's/^GAVE //p')
assert_eq "$wide_gave" "601" "the give-back reaches every entry of a wide directory, with no subtree skipped"
rm -rf "$wide"

if [[ "$EUID" == "0" ]]; then
    rs=$(mktemp -d)
    mkdir -p "$rs/tree/scheduler" "$rs/outside"
    echo secret >"$rs/tree/scheduler/secret"
    chmod 0600 "$rs/tree/scheduler/secret"
    echo outside >"$rs/outside/file"
    chown -R 65534:65534 "$rs/tree"
    chown root:root "$rs/tree/scheduler/secret"
    update_owner_snapshot "$rs/tree" "$rs/snap"
    mv "$rs/tree/scheduler/secret" "$rs/tree/scheduler/renamed"
    echo new >"$rs/tree/scheduler/new.txt"
    ln -s "$rs/outside" "$rs/tree/scheduler/link"
    rs_out=$(update_restore_tree_owner "$rs/tree" 65534:65534 "$rs/snap" 2>&1)
    assert_eq "$(stat -c '%u' "$rs/tree/scheduler/new.txt")" "65534" "a file the update wrote is given back"
    assert_eq "$(stat -c '%u' "$rs/tree/scheduler/renamed")" "0" "a root-owned file from before the update stays root-owned after a rename"
    assert_eq "$(stat -c '%u' "$rs/outside/file")" "0" "the give-back does not follow a symlink out of the tree"
    assert_eq "$(stat -c '%u' "$rs/tree/scheduler/link")" "65534" "the symlink itself is given back"
    if [[ "$rs_out" != *"renamed"* ]]; then
        echo "FAIL: the kept root-owned file was not named: $rs_out" >&2
        exit 1
    fi
    rm -rf "$rs"
fi

if [[ "$EUID" == "0" ]]; then
    ft=$(mktemp -d)
    mkdir -p "$ft/tree/.git" "$ft/bin"
    ft_tree=$(update_realpath "$ft/tree")
    chown 65534:65534 "$ft_tree/.git"
    cat >"$ft/bin/systemctl" <<'STUB'
#!/bin/bash
case "$1" in
    list-units) printf 'loose.service loaded inactive dead x\n' ;;
    list-unit-files) printf 'loose.service disabled enabled\n' ;;
    show) printf 'Id=loose.service\nUser=65534\nProtectSystem=no\nReadWritePaths=\nBindPaths=\n' ;;
esac
STUB
    chmod 0755 "$ft/bin/systemctl"
    assert_eq "$(update_tree_foreign_accounts "$ft_tree")" "65534" "a foreign-owned .git under a root-owned top is a foreign account"
    mkdir -p "$ft/plain"
    ft_rc=0
    (set -euo pipefail; ft_plain=$(update_tree_foreign_accounts "$ft/plain"); [[ -z "$ft_plain" ]]) || ft_rc=$?
    assert_eq "$ft_rc" "0" "a root-owned tree with no .git has no foreign account and succeeds under pipefail"
    ft_out=$(PATH="$ft/bin:$PATH" update_foreign_tree_check "$ft_tree") && ft_rc=0 || ft_rc=$?
    assert_eq "$ft_rc:$ft_out" "1:  loose.service (User=65534): ProtectSystem=no, not strict" "an unconfined unit of the .git owner refuses a root-owned tree"
    chown 65533:65533 "$ft_tree"
    ft_out=$(PATH="$ft/bin:$PATH" update_foreign_tree_check "$ft_tree") && ft_rc=0 || ft_rc=$?
    assert_eq "$ft_rc" "1" "a top and a .git owned by two other accounts are refused"
    if [[ "$ft_out" != *"uid 65533 and its .git to uid 65534"* ]]; then
        echo "FAIL: the two-owner refusal did not name both owners: $ft_out" >&2
        exit 1
    fi
    rm -rf "$ft"
fi

assert_eq "$(update_write_path_issue /opt/t /opt/t/scheduler)" "" "the scheduler directory is an allowed write path"
assert_eq "$(update_write_path_issue /opt/t /opt/t/logs/x)" "" "a path under logs is an allowed write path"
assert_eq "$(update_write_path_issue /opt/t /var/lib/go-trader/t)" "" "a path outside the tree is allowed"
assert_eq "$(update_write_path_issue /opt/t /opt/t-other)" "" "a sibling with the tree name as prefix is outside the tree"
assert_eq "$(update_write_path_issue /opt/t /opt/t/.git)" "can write /opt/t/.git inside the tree" "the git directory is refused"
assert_eq "$(update_write_path_issue /opt/t /opt)" "can write /opt, which holds the whole tree" "a parent of the tree is refused"
assert_eq "$(update_write_path_issue /opt/t /opt/t)" "can write /opt/t, which holds the whole tree" "the tree root is refused"
conf_dir=$(mktemp -d)
conf_tree=$(update_realpath "$conf_dir")
assert_eq "$(update_unit_confinement_issues "$conf_tree" strict "$conf_tree/scheduler -$conf_tree/logs" "")" "" "the template write paths pass"
assert_eq "$(update_unit_confinement_issues "$conf_tree" no "" "")" "ProtectSystem=no, not strict" "a unit without ProtectSystem=strict is refused"
assert_eq "$(update_unit_confinement_issues "$conf_tree" strict "$conf_tree/scheduler $conf_tree/.venv" "")" "ReadWritePaths can write $conf_tree/.venv inside the tree" "a write path over the venv is refused"
assert_eq "$(update_unit_confinement_issues "$conf_tree" strict "" "$conf_tree/shared_scripts:/srv/x:rbind")" "BindPaths can write $conf_tree/shared_scripts inside the tree" "a bind mount from inside the tree is refused"
mkdir -p "$conf_dir/bin"
cat >"$conf_dir/bin/systemctl" <<STUB
#!/bin/bash
block() {
    case "\$1" in
        loose.service) printf 'Id=loose.service\nUser=$(id -un)\nProtectSystem=no\nReadWritePaths=\nBindPaths=\n' ;;
        safe.service) printf 'Id=safe.service\nUser=$(id -un)\nProtectSystem=strict\nReadWritePaths=$conf_tree/scheduler $conf_tree/logs\nBindPaths=\n' ;;
        unloaded.service) printf 'Id=unloaded.service\nUser=$(id -un)\nProtectSystem=strict\nReadWritePaths=$conf_tree\nBindPaths=\n' ;;
        tmpl@$UPDATE_CONFINEMENT_INSTANCE.service) printf 'Id=tmpl@$UPDATE_CONFINEMENT_INSTANCE.service\nUser=$(id -u)\nProtectSystem=full\nReadWritePaths=\nBindPaths=\n' ;;
        *) printf 'Id=%s\nUser=\nProtectSystem=no\nReadWritePaths=\nBindPaths=\n' "\$1" ;;
    esac
}
case "\$1" in
    list-units) printf 'loose.service loaded inactive dead x\nsafe.service loaded active running x\nrootunit.service loaded active running x\n' ;;
    list-unit-files) printf 'loose.service disabled enabled\nunloaded.service disabled enabled\ntmpl@.service static -\nrootunit.service enabled enabled\n' ;;
    show)
        shift
        first=1
        for u in "\$@"; do
            case "\$u" in -p|--) continue ;; Id|User|ProtectSystem|ReadWritePaths|BindPaths) continue ;; esac
            [[ \$first == 1 ]] || printf '\n'
            first=0
            block "\$u"
        done ;;
esac
STUB
chmod 0755 "$conf_dir/bin/systemctl"
conf_out=$(PATH="$conf_dir/bin:$PATH" update_foreign_tree_confinement "$conf_tree" "$(id -u)") && conf_rc=0 || conf_rc=$?
assert_eq "$conf_rc" "1" "an unconfined unit of the tree owner fails the confinement check"
assert_eq "$conf_out" "  loose.service (User=$(id -un)): ProtectSystem=no, not strict
  tmpl@.service (User=$(id -u)): ProtectSystem=full, not strict
  unloaded.service (User=$(id -un)): ReadWritePaths can write $conf_tree, which holds the whole tree" "unconfined units of the owner are listed, including unit files systemd has not loaded and templates"
conf_out=$(PATH="$conf_dir/bin:$PATH" update_foreign_tree_confinement "$conf_tree" 99999) && conf_rc=0 || conf_rc=$?
assert_eq "$conf_rc:$conf_out" "0:" "units of other accounts do not count"
rm -rf "$conf_dir"

export_go=$(update_resolve_tool go || true)
if [[ -n "$export_go" ]] && command -v git >/dev/null 2>&1; then
    ex=$(mktemp -d)
    mkdir -p "$ex/tree/scheduler"
    printf 'module fixture\n\ngo 1.21\n' >"$ex/tree/scheduler/go.mod"
    printf 'package main\n\nimport "fmt"\n\nvar Version = "none"\n\nfunc main() { fmt.Println(Version) }\n' >"$ex/tree/scheduler/main.go"
    git -C "$ex/tree" init -q
    git -C "$ex/tree" add -A
    git -C "$ex/tree" -c user.email=t@example.invalid -c user.name=t commit -qm init
    printf 'package main\n\nimport "os"\n\nfunc init() { _ = os.WriteFile("%s/planted", nil, 0o644) }\n' "$ex" >"$ex/tree/scheduler/zz.go"
    printf '*\n' >"$ex/tree/scheduler/.gitignore"
    printf 'go 1.21\n\nuse ./missing\n' >"$ex/tree/scheduler/go.work"
    update_build_go_export "$ex/tree" HEAD "$export_go" v9.9.9 "$ex/out" >/dev/null
    assert_eq "$("$ex/out")" "v9.9.9" "the exported build carries the version stamp"
    if [[ -e "$ex/planted" ]]; then
        echo "FAIL: the exported build compiled an untracked scheduler file" >&2
        exit 1
    fi
    rm -rf "$ex"
else
    echo "note: go or git not installed; the export build case is skipped"
fi

rec_test=$(mktemp -d)
mkdir -p "$rec_test/deploy/scheduler" "$rec_test/deploy/sub/nested" "$rec_test/deploy/logs" "$rec_test/src/scheduler" "$rec_test/rec" "$rec_test/etc" "$rec_test/libexec/main"
printf '{\n  "db_file": "sub/nested/primary.db",\n  "paper_db_file": "scheduler/paper.db",\n  "paper_sources": [{"id": "btc", "db_file": "sub/nested/btc.db"}],\n  "log_dir": "custom_logs",\n  "status_port": 8123\n}\n' >"$rec_test/deploy/scheduler/real_config.json"
ln -s "$rec_test/deploy/scheduler/real_config.json" "$rec_test/deploy/scheduler/config.json"
mkdir -p "$rec_test/deploy/custom_logs"
printf 'secret=1\n' >"$rec_test/deploy/.env"
printf 'db1\n' >"$rec_test/deploy/sub/nested/primary.db"
printf 'wal1\n' >"$rec_test/deploy/sub/nested/primary.db-wal"
printf 'db2\n' >"$rec_test/deploy/scheduler/paper.db"
printf 'db3\n' >"$rec_test/deploy/sub/nested/btc.db"
printf 'log1\n' >"$rec_test/deploy/logs/app.log"
printf 'log2\n' >"$rec_test/deploy/custom_logs/app.log"
printf 'sig1\n' >"$rec_test/deploy/custom-signal.log"
printf 'code-v1\n' >"$rec_test/deploy/README.md"
printf 'untracked-v1\n' >"$rec_test/deploy/local_note.txt"
printf 'bin-v1\n' >"$rec_test/deploy/go-trader"
chmod +x "$rec_test/deploy/go-trader"
mkdir -p "$rec_test/deploy/.venv/bin" "$rec_test/deploy/.venv/pkg"
printf '#!/bin/sh\necho "Python 3.12.9"\n' >"$rec_test/deploy/.venv/bin/python3"
chmod +x "$rec_test/deploy/.venv/bin/python3"
printf 'pkg-v1\n' >"$rec_test/deploy/.venv/pkg/mod.py"
git -C "$rec_test/deploy" init -q
git -C "$rec_test/deploy" -c user.email=t@example.invalid -c user.name=t add README.md
git -C "$rec_test/deploy" -c user.email=t@example.invalid -c user.name=t commit -qm v1
printf 'staged-v1\n' >"$rec_test/deploy/staged.txt"
git -C "$rec_test/deploy" add staged.txt
printf 'unstaged-v1\n' >"$rec_test/deploy/README.md"
pre_head=$(git -C "$rec_test/deploy" rev-parse HEAD)
pre_idx_fp=$(update_file_fingerprint "$rec_test/deploy/.git/index")

UPDATE_NO_FREEZE_EXCLUDES=1 GO_TRADER_SIGNAL_LOG="$rec_test/deploy/custom-signal.log" update_managed_rsync_excludes "$rec_test/deploy"
excl_joined=$(printf '%s\n' "${UPDATE_MANAGED_EXCLUDES[@]}")
for want_excl in "--exclude=/scheduler/real_config.json" "--exclude=/sub/nested/primary.db*" "--exclude=/sub/nested/btc.db*" "--exclude=/custom_logs/" "--exclude=/custom-signal.log"; do
    if [[ "$excl_joined" != *"$want_excl"* ]]; then
        echo "FAIL: managed excludes missing $want_excl: $excl_joined" >&2
        exit 1
    fi
done

rkey=$(update_recovery_key "$rec_test/deploy")
update_acquire_lock "$rec_test/rec" "$rkey" "$$" "$rec_test/deploy" || { echo "FAIL: lock acquire failed" >&2; exit 1; }
if update_acquire_lock "$rec_test/rec" "$rkey" "99999999" "$rec_test/deploy" 2>/dev/null; then
    echo "FAIL: concurrent lock acquire succeeded while live pid holds lock" >&2
    exit 1
fi
printf '99999998\n' >"$rec_test/rec/${rkey}.lock/pid"
update_acquire_lock "$rec_test/rec" "$rkey" "$$" "$rec_test/deploy" 2>/dev/null || { echo "FAIL: stale lock was not reclaimed" >&2; exit 1; }
update_release_lock "$rec_test/rec" "$rkey" "$$"

if GO_TRADER_UPDATE_FAKE_FREE_BYTES=100 UPDATE_NO_FREEZE_EXCLUDES=1 update_take_snapshot "$rec_test/deploy" "$rec_test/rec/$rkey" "" "" "" 2>/dev/null; then
    echo "FAIL: snapshot succeeded with insufficient free space" >&2
    exit 1
fi

ln -s /tmp "$rec_test/deploy/.venv-link"
mv "$rec_test/deploy/.venv" "$rec_test/deploy/.venv-real"
mv "$rec_test/deploy/.venv-link" "$rec_test/deploy/.venv"
if UPDATE_NO_FREEZE_EXCLUDES=1 update_take_snapshot "$rec_test/deploy" "$rec_test/rec/$rkey" "" "" "" 2>/dev/null; then
    echo "FAIL: snapshot accepted a symlinked .venv" >&2
    exit 1
fi
rm -f "$rec_test/deploy/.venv"
mv "$rec_test/deploy/.venv-real" "$rec_test/deploy/.venv"

UPDATE_NO_FREEZE_EXCLUDES=1 GO_TRADER_SIGNAL_LOG="$rec_test/deploy/custom-signal.log" update_take_snapshot "$rec_test/deploy" "$rec_test/rec/$rkey" "" "" "" \
    || { echo "FAIL: update_take_snapshot failed" >&2; exit 1; }

printf 'mutated-readme\n' >"$rec_test/deploy/README.md"
printf 'added-by-failed-update\n' >"$rec_test/deploy/added_new.py"
rm -f "$rec_test/deploy/local_note.txt"
printf 'pkg-v2\n' >"$rec_test/deploy/.venv/pkg/mod.py"
printf 'extra-pkg\n' >"$rec_test/deploy/.venv/pkg/extra.py"
printf 'bin-v2\n' >"$rec_test/deploy/go-trader"
printf 'bin-prev\n' >"$rec_test/deploy/go-trader.prev"
printf 'db1-runtime-write\n' >"$rec_test/deploy/sub/nested/primary.db"
git -C "$rec_test/deploy" -c user.email=t@example.invalid -c user.name=t commit -am "bad-commit" -q

UPDATE_NO_FREEZE_EXCLUDES=1 GO_TRADER_SIGNAL_LOG="$rec_test/deploy/custom-signal.log" update_restore_snapshot_files "$rec_test/deploy" "$rec_test/rec/$rkey" "" \
    || { echo "FAIL: update_restore_snapshot_files failed" >&2; exit 1; }
update_restore_snapshot_venv "$rec_test/deploy" "$rec_test/rec/$rkey" \
    || { echo "FAIL: update_restore_snapshot_venv failed" >&2; exit 1; }
update_restore_snapshot_binaries "$rec_test/deploy" "$rec_test/rec/$rkey" \
    || { echo "FAIL: update_restore_snapshot_binaries failed" >&2; exit 1; }

assert_eq "$(cat "$rec_test/deploy/README.md")" "unstaged-v1" "unstaged working-tree edit restored"
assert_eq "$(cat "$rec_test/deploy/local_note.txt")" "untracked-v1" "deleted untracked file restored"
[[ ! -e "$rec_test/deploy/added_new.py" ]] || { echo "FAIL: added file was not removed by restore" >&2; exit 1; }
assert_eq "$(git -C "$rec_test/deploy" rev-parse HEAD)" "$pre_head" "git HEAD restored"
assert_eq "$(update_file_fingerprint "$rec_test/deploy/.git/index")" "$pre_idx_fp" "git index restored with staged edit intact"
assert_eq "$(cat "$rec_test/deploy/.venv/pkg/mod.py")" "pkg-v1" ".venv modified file restored"
[[ ! -e "$rec_test/deploy/.venv/pkg/extra.py" ]] || { echo "FAIL: .venv added file was not removed" >&2; exit 1; }
assert_eq "$(cat "$rec_test/deploy/go-trader")" "bin-v1" "binary restored from snapshot"
[[ ! -e "$rec_test/deploy/go-trader.prev" ]] || { echo "FAIL: absent go-trader.prev was not removed" >&2; exit 1; }
assert_eq "$(cat "$rec_test/deploy/sub/nested/primary.db")" "db1-runtime-write" "excluded nested runtime DB write survived restore"
assert_eq "$(cat "$rec_test/deploy/custom_logs/app.log")" "log2" "custom log_dir survived restore"
assert_eq "$(cat "$rec_test/deploy/custom-signal.log")" "sig1" "custom signal log survived restore"

update_write_incomplete_marker "$rec_test/rec" "$rkey" "$rec_test/deploy" "$rec_test/rec/$rkey" "forced test failure" "files=ok probe=failed"
if update_check_incomplete_marker "$rec_test/rec" "$rkey" "$rec_test/deploy" 2>/dev/null; then
    echo "FAIL: incomplete marker did not block same directory" >&2
    exit 1
fi
GO_TRADER_UPDATE_RECOVERY_DIR="$rec_test/rec" update_clear_recovery "$rec_test/deploy" "$$" >/dev/null
update_check_incomplete_marker "$rec_test/rec" "$rkey" "$rec_test/deploy" || { echo "FAIL: cleared marker still blocked update" >&2; exit 1; }
cleared_count=$(find "$rec_test/rec" -maxdepth 1 -name "${rkey}.cleared-*" | wc -l | tr -d '[:space:]')
assert_eq "$cleared_count" "1" "--clear-recovery moved snapshot to .cleared-<UTC>"

mkdir -p "$rec_test/rec/$rkey"
update_write_incomplete_marker "$rec_test/rec" "$rkey" "$rec_test/deploy" "$rec_test/rec/$rkey" "forced test failure 2" "files=failed"
mv "$rec_test/deploy" "$rec_test/deploy.moved"
mkdir -p "$rec_test/deploy"
update_check_incomplete_marker "$rec_test/rec" "$rkey" "$rec_test/deploy" 2>/dev/null || { echo "FAIL: stale marker on replaced directory inode blocked update" >&2; exit 1; }

mkdir -p "$rec_test/unit-test"
printf 'unit-v1\n' >"$rec_test/unit-test/go-trader.service"
printf 'unit-v2\n' >"$rec_test/unit-test/shipped.service"
chmod 0500 "$rec_test/unit-test"
UPDATE_UNIT_SUDO="" update_unit_install_with_backup "$rec_test/unit-test/go-trader.service" "$rec_test/unit-test/shipped.service" 2>/dev/null && u_rc=0 || u_rc=$?
chmod 0700 "$rec_test/unit-test"
rm -rf "$rec_test"

rh_test=$(mktemp -d)
rh_seed() {
    python3 -I - "$rh_test/state.db" "$1" "$2" <<'PY'
import sqlite3, sys
path, pos_symbol, attempt_symbol = sys.argv[1:4]
con = sqlite3.connect(path)
con.execute("DROP TABLE IF EXISTS positions")
con.execute("DROP TABLE IF EXISTS robinhood_order_attempts")
con.execute("DROP TABLE IF EXISTS strategies")
con.execute("CREATE TABLE positions (strategy_id TEXT, symbol TEXT)")
con.execute("CREATE TABLE robinhood_order_attempts (strategy_id TEXT, symbol TEXT, status TEXT)")
con.execute("CREATE TABLE strategies (id TEXT, risk_pending_circuit_closes_json TEXT)")
if pos_symbol:
    con.execute("INSERT INTO positions VALUES (?, ?)", ("rh-btc-store", pos_symbol))
if attempt_symbol:
    con.execute("INSERT INTO robinhood_order_attempts VALUES (?, ?, ?)", ("rh-btc-store", attempt_symbol, "unknown"))
    con.execute("INSERT INTO robinhood_order_attempts VALUES (?, ?, ?)", ("rh-btc-store", attempt_symbol.lower(), "settled"))
con.execute("INSERT INTO strategies VALUES (?, ?)", ("rh-btc-store", None))
con.commit()
PY
}
rh_config() {
    printf '{"db_file": "%s", "strategies": [{"id": "rh-btc", "storage_strategy_id": "rh-btc-store", "type": "spot", "args": ["momentum_pro", "%s", "1h", "--mode=live"]}, {"id": "hl-eth", "type": "perps", "args": ["x", "eth", "1h"]}]}\n' "$rh_test/state.db" "$1" >"$rh_test/config.json"
}
printf 'old go-trader build\n' >"$rh_test/go-trader-pre1908"
printf 'new go-trader build %s tail\n' "$UPDATE_RH_IDENTITY_CAPABILITY" >"$rh_test/go-trader-1908"
rh_check() {
    GO_TRADER_UPDATE_CONFIG="$rh_test/config.json" update_robinhood_identity_rollback_check "${1:-$rh_test/go-trader-pre1908}" 2>"$rh_test/err" && printf 'ok' || printf 'blocked'
}
grep -qF "const robinhoodCoinIdentityCapability = \"$UPDATE_RH_IDENTITY_CAPABILITY\"" "$(dirname "$0")/../scheduler/robinhood_identity.go" || { echo "FAIL: rh identity: update_helpers.sh capability marker differs from scheduler/robinhood_identity.go" >&2; exit 1; }
rh_config btc; rh_seed BTC ""
assert_eq "$(rh_check)" "blocked" "rh identity: a lowercase config over a migrated BTC book blocks a restored pre-#1908 binary"
grep -q "rh-btc: config args\[1\] is 'btc'" "$rh_test/err" || { echo "FAIL: rh identity blocker names the strategy" >&2; cat "$rh_test/err" >&2; exit 1; }
grep -q "predates the Robinhood coin identity rule" "$rh_test/err" || { echo "FAIL: rh identity: a pre-#1908 binary block names the old binary" >&2; cat "$rh_test/err" >&2; exit 1; }
assert_eq "$(rh_check "$rh_test/go-trader-1908")" "ok" "rh identity: a lowercase config over a migrated BTC book starts a restored binary that has the #1908 identity rule"
grep -q "reads the trimmed uppercase coin identity" "$rh_test/err" || { echo "FAIL: rh identity: a #1908 binary start logs why it does not block" >&2; cat "$rh_test/err" >&2; exit 1; }
assert_eq "$(rh_check "$rh_test/missing-go-trader")" "blocked" "rh identity: a lowercase config over a migrated BTC book blocks when the restored binary cannot be read"
grep -q "could not read the restored binary" "$rh_test/err" || { echo "FAIL: rh identity: an unknown binary block says the build is unknown" >&2; cat "$rh_test/err" >&2; exit 1; }
mkdir -p "$rh_test/dir-go-trader"
assert_eq "$(rh_check "$rh_test/dir-go-trader")" "blocked" "rh identity: a restored binary path that is not a file blocks"
assert_eq "$(GO_TRADER_UPDATE_CONFIG="$rh_test/config.json" update_robinhood_identity_rollback_check 2>/dev/null && printf ok || printf blocked)" "blocked" "rh identity: no restored binary path blocks"
rh_config BTC
assert_eq "$(rh_check)" "ok" "rh identity: a canonical config over a canonical book starts"
rh_config btc; rh_seed btc ""
assert_eq "$(rh_check)" "ok" "rh identity: an unmigrated lowercase book with a lowercase config starts"
rh_config BTC
assert_eq "$(rh_check)" "blocked" "rh identity: a canonical config over an unmigrated lowercase book blocks"
rh_config btc; rh_seed "" BTC
assert_eq "$(rh_check)" "blocked" "rh identity: a migrated unresolved attempt blocks under a lowercase config"
rh_config BTC
assert_eq "$(rh_check)" "ok" "rh identity: settled history in another spelling never blocks"
python3 -I - "$rh_test/state.db" <<'PY'
import sqlite3, sys
con = sqlite3.connect(sys.argv[1])
con.execute("DELETE FROM robinhood_order_attempts")
con.execute("UPDATE strategies SET risk_pending_circuit_closes_json = ?", ('{"robinhood": {"symbols": [{"symbol": "BTC", "size": 0.5}]}}',))
con.commit()
PY
rh_config btc
assert_eq "$(rh_check)" "blocked" "rh identity: a migrated pending circuit close blocks under a lowercase config"
printf 'not a database' >"$rh_test/state.db"
assert_eq "$(rh_check)" "blocked" "rh identity: an unreadable state file fails closed"
assert_eq "$(rh_check "$rh_test/go-trader-1908")" "blocked" "rh identity: an unreadable state file fails closed for a #1908 binary too"
printf '{"db_file": "%s", "strategies": [{"id": "hl-eth", "type": "perps", "args": ["x", "eth", "1h"]}]}\n' "$rh_test/state.db" >"$rh_test/config.json"
assert_eq "$(rh_check)" "ok" "rh identity: a roster with no Robinhood spot strategy never opens the state files"
rm -rf "$rh_test"

cert_test=$(mktemp -d)
printf 'FOO=1\nGO_TRADER_DIRECTIONAL_CERT_PATH=/srv/file-certs.json\n' >"$cert_test/a.env"
printf 'GO_TRADER_DIRECTIONAL_CERT_PATH=/srv/later-certs.json\n' >"$cert_test/b.env"
assert_eq "$(update_directional_cert_path_from_show 'GO_TRADER_DIRECTIONAL_CERT_PATH=/srv/inline.json' '')" \
    "SET /srv/inline.json" "cert path: an inline Environment= override reaches the probe"
assert_eq "$(update_directional_cert_path_from_show 'GO_TRADER_DIRECTIONAL_CERT_PATH=/srv/inline.json' "$cert_test/a.env (ignore_errors=no)")" \
    "SET /srv/file-certs.json" "cert path: an EnvironmentFile value overrides Environment= as systemd does"
assert_eq "$(update_directional_cert_path_from_show '' "$(printf '%s\n%s' "$cert_test/a.env (ignore_errors=no)" "$cert_test/b.env (ignore_errors=yes)")")" \
    "SET /srv/later-certs.json" "cert path: the later EnvironmentFile wins"
assert_eq "$(update_directional_cert_path_from_show 'OTHER=1' "$cert_test/missing.env (ignore_errors=yes)")" \
    "" "cert path: a unit with no override leaves the probe on the default artifact"
rm -rf "$cert_test"

rb_test=$(mktemp -d)
rb_seed() {
    python3 -I - "$rb_test/state.db" "$1" "$2" "$3" "$4" <<'PY'
import sqlite3, sys
path, columns, sid, qty, key = sys.argv[1:6]
con = sqlite3.connect(path)
con.execute("DROP TABLE IF EXISTS positions")
if columns == "new":
    con.execute("CREATE TABLE positions (strategy_id TEXT, symbol TEXT, side TEXT, quantity REAL, account_key TEXT NOT NULL DEFAULT '', venue_account_id TEXT NOT NULL DEFAULT '', account_binding_source TEXT NOT NULL DEFAULT '')")
    con.execute("INSERT INTO positions (strategy_id, symbol, side, quantity, account_key, venue_account_id) VALUES (?, ?, ?, ?, ?, ?)", (sid, "btc", "long", float(qty), key, "acct-1" if key else ""))
else:
    con.execute("CREATE TABLE positions (strategy_id TEXT, symbol TEXT, side TEXT, quantity REAL)")
    con.execute("INSERT INTO positions VALUES (?, ?, ?, ?)", (sid, "btc", "long", float(qty)))
con.commit()
PY
}
rb_config() {
    printf '{"db_file": "%s", "strategies": [{"id": "rh-btc", "storage_strategy_id": "rh-btc-store", "type": "spot", "args": ["momentum_pro", "BTC", "1h", "--mode=%s"]}, {"id": "hl-eth", "type": "perps", "args": ["x", "eth", "1h"]}]}\n' "$rb_test/state.db" "${1:-live}" >"$rb_test/config.json"
}
printf 'old go-trader build %s\n' "$UPDATE_RH_IDENTITY_CAPABILITY" >"$rb_test/go-trader-pre1921"
printf 'new go-trader build %s %s tail\n' "$UPDATE_RH_IDENTITY_CAPABILITY" "$UPDATE_RH_BINDING_CAPABILITY" >"$rb_test/go-trader-1921"
rb_check() {
    GO_TRADER_UPDATE_CONFIG="$rb_test/config.json" update_robinhood_binding_rollback_check "${1:-$rb_test/go-trader-pre1921}" 2>"$rb_test/err" && printf 'ok' || printf 'blocked'
}
grep -qF "const robinhoodAccountBindingCapability = \"$UPDATE_RH_BINDING_CAPABILITY\"" "$(dirname "$0")/../scheduler/robinhood_account_binding.go" || { echo "FAIL: rh binding: update_helpers.sh capability marker differs from scheduler/robinhood_account_binding.go" >&2; exit 1; }
rb_config live; rb_seed new rh-btc-store 0.5 robinhood:user-1
assert_eq "$(rb_check)" "blocked" "rh binding: an open bound live book blocks a restored binary without the account binding"
grep -q "rh-btc: .* holds an open BTC book of 0.5 (bound)" "$rb_test/err" || { echo "FAIL: rh binding blocker names the strategy and book" >&2; cat "$rb_test/err" >&2; exit 1; }
grep -q "predates the Robinhood account binding" "$rb_test/err" || { echo "FAIL: rh binding: the block names the old binary" >&2; cat "$rb_test/err" >&2; exit 1; }
rb_seed new rh-btc-store 0.5 ""
assert_eq "$(rb_check)" "blocked" "rh binding: an open book with empty binding values still blocks"
assert_eq "$(rb_check "$rb_test/go-trader-1921")" "ok" "rh binding: a restored binary with the account binding starts"
grep -q "enforces the account binding" "$rb_test/err" || { echo "FAIL: rh binding: a compatible start logs why it does not block" >&2; cat "$rb_test/err" >&2; exit 1; }
assert_eq "$(rb_check "$rb_test/missing-go-trader")" "blocked" "rh binding: an unreadable restored binary blocks"
rb_seed new rh-btc-store 0 robinhood:user-1
assert_eq "$(rb_check)" "ok" "rh binding: a flat book never blocks"
rb_seed new rh-btc 0.5 robinhood:user-1
assert_eq "$(rb_check)" "ok" "rh binding: only the configured storage id is owned"
rb_seed old rh-btc-store 0.5 ""
assert_eq "$(rb_check)" "ok" "rh binding: a state file without binding columns never ran the feature"
rb_seed new rh-btc-store 0.5 robinhood:user-1
rb_config paper
assert_eq "$(rb_check)" "ok" "rh binding: a paper Robinhood strategy never blocks"
rb_config live
printf 'not a database' >"$rb_test/state.db"
assert_eq "$(rb_check)" "blocked" "rh binding: an unreadable state file fails closed"
assert_eq "$(rb_check "$rb_test/go-trader-1921")" "blocked" "rh binding: an unreadable state file fails closed for a compatible binary too"
grep -q "could not read the config or a state file" "$rb_test/err" || { echo "FAIL: rh binding: an unreadable state block says so" >&2; cat "$rb_test/err" >&2; exit 1; }
rm -rf "$rb_test"

owner_repo_root=$(git rev-parse --show-toplevel 2>/dev/null || true)
if [[ -z "$owner_repo_root" || ! -x "$owner_repo_root/scripts/update.sh" ]]; then
    echo "FAIL: restart owner fixtures need the repository checkout with scripts/update.sh" >&2
    exit 1
fi

ow_root=$(cd "$(mktemp -d)" && pwd -P)

owner_fail() {
    echo "FAIL: $1" >&2
    [[ -n "${2:-}" && -f "$2" ]] && cat "$2" >&2
    exit 1
}

mkdir -p "$ow_root/shim"
cat >"$ow_root/shim/systemctl" <<'EOS'
#!/usr/bin/env bash
st="${SYSTEMCTL_SHIM_STATE:?}"
cmd="${1:-}"
[[ $# -gt 0 ]] && shift
unit=""
prop=""
quiet=0
while [[ $# -gt 0 ]]; do
    case "$1" in
        -p) prop="$2"; shift 2 ;;
        --quiet) quiet=1; shift ;;
        -*) shift ;;
        *) [[ -z "$unit" ]] && unit="$1"; shift ;;
    esac
done
case "$cmd" in
    --version) echo 'systemd 255'; exit 0 ;;
    list-units) [[ -f "$st/active-units" ]] && cat "$st/active-units"; exit 0 ;;
    list-unit-files) exit 0 ;;
    is-active)
        state=$(cat "$st/u/$unit/ActiveState" 2>/dev/null || echo inactive)
        [[ "$quiet" == "1" ]] || echo "$state"
        [[ "$state" == "active" ]] && exit 0
        exit 3
        ;;
    is-enabled) echo disabled; exit 1 ;;
    show)
        f="$st/u/$unit/$prop"
        [[ -f "$f" ]] && cat "$f"
        exit "$(cat "$f.rc" 2>/dev/null || echo 0)"
        ;;
    restart)
        echo "restart $unit" >>"$st/calls"
        seq_file="$st/u/$unit/restart-rcs"
        rc=0
        if [[ -s "$seq_file" ]]; then
            rc=$(head -n 1 "$seq_file")
            tail -n +2 "$seq_file" >"$seq_file.next"
            mv "$seq_file.next" "$seq_file"
        fi
        exit "$rc"
        ;;
    stop)
        echo "stop $unit" >>"$st/calls"
        mkdir -p "$st/u/$unit"
        printf 'inactive' >"$st/u/$unit/ActiveState"
        printf '0' >"$st/u/$unit/MainPID"
        exit 0
        ;;
    *) echo "$cmd $unit" >>"$st/calls"; exit 0 ;;
esac
EOS
chmod +x "$ow_root/shim/systemctl"

mkdir -p "$ow_root/nosys"
IFS=: read -r -a ow_path_dirs <<<"$PATH"
for ow_pd in "${ow_path_dirs[@]}"; do
    [[ -d "$ow_pd" ]] || continue
    for ow_f in "$ow_pd"/*; do
        ow_n="${ow_f##*/}"
        [[ "$ow_n" == "systemctl" || -e "$ow_root/nosys/$ow_n" || -L "$ow_root/nosys/$ow_n" ]] && continue
        [[ -f "$ow_f" && -x "$ow_f" ]] || continue
        ln -s "$ow_f" "$ow_root/nosys/$ow_n"
    done
done
if PATH="$ow_root/nosys" command -v systemctl >/dev/null 2>&1; then
    owner_fail "the systemctl-free PATH still resolves systemctl"
fi
if ! PATH="$ow_root/nosys" command -v git >/dev/null 2>&1; then
    owner_fail "the systemctl-free PATH lost git"
fi

unit_set() {
    mkdir -p "$1/u/$2"
    printf '%s' "$4" >"$1/u/$2/$3"
}
unit_rc() {
    mkdir -p "$1/u/$2"
    printf '%s' "$4" >"$1/u/$2/$3.rc"
}

(
    mk_fleet() {
        mkdir -p "$1/parent" "$1/st" "$1/proc/self"
        git -C "$1/parent" init -q
    }
    mk_dir() {
        mkdir -p "$1/$2/scheduler"
        git -C "$1/$2" init -q
        echo '{}' >"$1/$2/scheduler/config.json"
    }
    map_unit() {
        printf '%s loaded active running x\n' "$3" >>"$1/st/active-units"
        unit_set "$1/st" "$3" WorkingDirectory "$1/$2"
        unit_set "$1/st" "$3" ActiveState active
        unit_set "$1/st" "$3" LoadState loaded
    }
    run_fleet() {
        local fleet="$1" path="$2"
        shift 2
        local -a envs=()
        while [[ $# -gt 0 && "$1" != "--" ]]; do
            envs+=("$1")
            shift
        done
        shift
        : >"$fleet/records"
        local rc=0
        (
            cd "$fleet/parent"
            env -u GO_TRADER_PIDFILE -u GO_TRADER_RUN_SH -u RESTART -u RESTART_MODE -u GO_TRADER_UPDATE_ALL_ROOT \
                -u GO_TRADER_SERVICE -u GO_TRADER_PROC_ROOT \
                PATH="$path" RECORD_OUT="$fleet/records" SYSTEMCTL_SHIM_STATE="$fleet/st" \
                GO_TRADER_PROC_ROOT="$fleet/proc" GO_TRADER_SERVICE=go-trader-parent \
                GO_TRADER_UPDATE_RECOVERY_DIR="$fleet/rec" \
                ${envs[@]+"${envs[@]}"} \
                bash "$owner_repo_root/scripts/update.sh" "$@" --update-all-root="$fleet"
        ) >"$fleet/out" 2>&1 || rc=$?
        printf '%s' "$rc"
    }
    spawned() {
        grep -q "^\[child\] cwd=$1/$2 " "$1/records"
    }
    expect_spawn() {
        spawned "$1" "$2" || owner_fail "$3: $2 was not dispatched" "$1/out"
    }
    expect_skip() {
        ! spawned "$1" "$2" || owner_fail "$3: $2 was dispatched" "$1/out"
        grep -qF "WARNING: skipping $1/$2 (restart route cannot succeed: $4" "$1/out" \
            || owner_fail "$3: no skip line naming $2 and '$4'" "$1/out"
    }
    expect_text() {
        grep -qF -- "$2" "$1/out" || owner_fail "$3: output lacks '$2'" "$1/out"
    }

    bash() {
        if [[ "${1:-}" == *update.sh ]]; then
            printf '[child] cwd=%s GO_TRADER_SERVICE=%s argv=%s\n' \
                "$(pwd)" "${GO_TRADER_SERVICE:-<unset>}" "$*" >>"${RECORD_OUT:?}"
            case " ${MOCK_FAIL_DIRS:-} " in
                *" ${PWD##*/} "*) return 1 ;;
            esac
            return 0
        fi
        command bash "$@"
    }
    export -f bash

    f="$ow_root/f1"
    mk_fleet "$f"
    for n in live folded sigok nowrap nonexec; do
        mk_dir "$f" "go-trader-$n"
    done
    map_unit "$f" go-trader-live go-trader-live.service
    unit_set "$f/st" go-trader-parent LoadState not-found
    unit_set "$f/st" go-trader-parent FragmentPath ""
    for n in sigok nowrap nonexec; do
        echo 4242 >"$f/go-trader-$n/go-trader.pid"
    done
    printf '#!/bin/sh\n' >"$f/go-trader-sigok/run.sh"
    chmod 755 "$f/go-trader-sigok/run.sh"
    printf '#!/bin/sh\n' >"$f/go-trader-nonexec/run.sh"
    chmod 644 "$f/go-trader-nonexec/run.sh"
    rc=$(run_fleet "$f" "$ow_root/shim:$PATH" -- --all --restart)
    assert_eq "$rc" "0" "owner fleet: one proven skip plus successes exits 0"
    expect_spawn "$f" go-trader-live "owner fleet: a mapped loaded unit"
    expect_spawn "$f" go-trader-sigok "owner fleet: a missing unit with pidfile and executable wrapper"
    expect_spawn "$f" go-trader-nowrap "owner fleet: a missing wrapper the transfer may supply"
    expect_spawn "$f" go-trader-nonexec "owner fleet: a non-executable wrapper the transfer may repair"
    expect_skip "$f" go-trader-folded "owner fleet: a missing unit with no fragment and no pidfile" \
        "route=signal-fallback, LoadState=not-found: signal restart needs pidfile $f/go-trader-folded/go-trader.pid"
    expect_text "$f" "all instances OK (4 updated, 1 skipped of 5 discovered)" "owner fleet: totals"
    expect_text "$f" "1 deployment(s) skipped because their restart route cannot succeed" "owner fleet: skip summary"
    expect_text "$f" "$f/go-trader-nowrap restart route: unknown" "owner fleet: an uncertain wrapper is unknown"
    rc=$(run_fleet "$f" "$ow_root/shim:$PATH" GO_TRADER_PROC_ROOT="$f/noproc" -- --all --restart)
    assert_eq "$rc" "0" "owner fleet: proc-root skips plus one success exit 0"
    expect_spawn "$f" go-trader-live "owner fleet: a loaded unit needs no proc root"
    expect_skip "$f" go-trader-sigok "owner fleet: no proc root" \
        "route=signal-fallback, LoadState=not-found: signal restart needs $f/noproc/self"
    expect_text "$f" "all instances OK (1 updated, 4 skipped of 5 discovered)" "owner fleet: proc-root totals"
    rc=$(run_fleet "$f" "$ow_root/shim:$PATH" MOCK_FAIL_DIRS=go-trader-sigok -- --all --restart)
    [[ "$rc" != "0" ]] || owner_fail "owner fleet: a failed child with skips must still fail the batch" "$f/out"
    expect_text "$f" "1 failing instance(s) (4 updated, 1 skipped)" "owner fleet: a real child failure"
    rc=$(run_fleet "$f" "$ow_root/shim:$PATH" -- --all)
    assert_eq "$rc" "2" "owner fleet: --all without restart exits 2"
    [[ ! -s "$f/records" ]] || owner_fail "owner fleet: --all without restart dispatched a child" "$f/records"
    rc=$(run_fleet "$f" "$ow_root/shim:$PATH" RESTART=1 -- --all)
    assert_eq "$rc" "0" "owner fleet: RESTART=1 enables the batch"
    expect_spawn "$f" go-trader-live "owner fleet: RESTART=1 dispatches"
    expect_skip "$f" go-trader-folded "owner fleet: RESTART=1 applies the owner check" "route=signal-fallback"

    f="$ow_root/f2"
    mk_fleet "$f"
    mk_dir "$f" go-trader-custom
    mkdir -p "$f/go-trader-custom/bin"
    printf '#!/bin/sh\n' >"$f/go-trader-custom/bin/wrap.sh"
    chmod 755 "$f/go-trader-custom/bin/wrap.sh"
    echo 4242 >"$f/abs.pid"
    unit_set "$f/st" go-trader-parent LoadState not-found
    rc=$(run_fleet "$f" "$ow_root/shim:$PATH" GO_TRADER_PIDFILE="$f/abs.pid" GO_TRADER_RUN_SH=bin/wrap.sh -- --all --restart)
    assert_eq "$rc" "0" "owner custom: an absolute pidfile and a relative wrapper keep the deployment"
    expect_spawn "$f" go-trader-custom "owner custom: absolute pidfile plus relative wrapper"
    expect_text "$f" "wrapper $f/go-trader-custom/bin/wrap.sh" "owner custom: the wrapper resolves from the deployment root"
    rm -f "$f/abs.pid"
    rc=$(run_fleet "$f" "$ow_root/shim:$PATH" GO_TRADER_PIDFILE="$f/abs.pid" GO_TRADER_RUN_SH=bin/wrap.sh -- --all --restart)
    [[ "$rc" != "0" ]] || owner_fail "owner custom: a batch that skips every deployment must fail" "$f/out"
    expect_skip "$f" go-trader-custom "owner custom: the custom pidfile is gone" "route=signal-fallback, LoadState=not-found: signal restart needs pidfile $f/abs.pid"
    expect_text "$f" "updated 0 deployments" "owner custom: zero updates"
    expect_text "$f" "1 with no restart route that can succeed" "owner custom: zero updates names the route reason"

    f="$ow_root/f3"
    mk_fleet "$f"
    mk_dir "$f" go-trader-rel
    mkdir -p "$f/go-trader-rel/state" "$f/outside"
    echo 4242 >"$f/go-trader-rel/state/w.pid"
    printf '#!/bin/sh\n' >"$f/outside/wrap.sh"
    chmod 755 "$f/outside/wrap.sh"
    unit_set "$f/st" go-trader-parent LoadState not-found
    rc=$(run_fleet "$f" "$ow_root/shim:$PATH" GO_TRADER_PIDFILE=state/w.pid GO_TRADER_RUN_SH="$f/outside/wrap.sh" -- --all --restart)
    assert_eq "$rc" "0" "owner relative: a relative pidfile and an absolute wrapper keep the deployment"
    expect_spawn "$f" go-trader-rel "owner relative: relative pidfile plus absolute wrapper"
    rm -f "$f/go-trader-rel/state/w.pid"
    mkdir -p "$f/parent/state"
    echo 4242 >"$f/parent/state/w.pid"
    rc=$(run_fleet "$f" "$ow_root/shim:$PATH" GO_TRADER_PIDFILE=state/w.pid GO_TRADER_RUN_SH="$f/outside/wrap.sh" -- --all --restart)
    expect_skip "$f" go-trader-rel "owner relative: the pidfile resolves from the deployment, not the batch parent" \
        "route=signal-fallback, LoadState=not-found: signal restart needs pidfile $f/go-trader-rel/state/w.pid"
    echo 4242 >"$f/go-trader-rel/state/w.pid"
    chmod 644 "$f/outside/wrap.sh"
    rc=$(run_fleet "$f" "$ow_root/shim:$PATH" GO_TRADER_PIDFILE=state/w.pid GO_TRADER_RUN_SH="$f/outside/wrap.sh" -- --all --restart)
    expect_skip "$f" go-trader-rel "owner relative: a non-executable wrapper outside the tree" \
        "route=signal-fallback, LoadState=not-found: signal restart needs executable wrapper $f/outside/wrap.sh"
    expect_text "$f" "lies outside the deployment tree" "owner relative: the outside wrapper reason"

    f="$ow_root/f4"
    mk_fleet "$f"
    for n in redirect sigmissing uncertain; do
        mk_dir "$f" "go-trader-$n"
    done
    printf '#!/bin/sh\n' >"$f/go-trader-redirect/go-trader"
    chmod 755 "$f/go-trader-redirect/go-trader"
    map_unit "$f" go-trader-redirect go-trader-redirect.service
    unit_set "$f/st" go-trader-redirect.service ExecStart "$f/go-trader-redirect/go-trader --config $f/go-trader-redirect/scheduler/config.json"
    map_unit "$f" go-trader-uncertain go-trader-uncertain.service
    unit_rc "$f/st" go-trader-uncertain.service ExecStart 1
    rc=$(run_fleet "$f" "$ow_root/shim:$PATH" -- --all --restart --restart-mode signal)
    assert_eq "$rc" "0" "owner signal: a redirect, an uncertain probe and one skip exit 0"
    expect_spawn "$f" go-trader-redirect "owner signal: an active unit running this binary redirects to systemd"
    grep -q "cwd=$f/go-trader-redirect .*--restart-mode signal" "$f/records" \
        || owner_fail "owner signal: the redirected child lost --restart-mode signal" "$f/records"
    expect_text "$f" "$f/go-trader-redirect restart route: owned (route=systemd-redirect, LoadState=loaded" "owner signal: redirect route"
    expect_spawn "$f" go-trader-uncertain "owner signal: an uncertain redirect probe"
    expect_text "$f" "$f/go-trader-uncertain restart route: unknown" "owner signal: an uncertain redirect is unknown"
    expect_skip "$f" go-trader-sigmissing "owner signal: an inactive unit and no pidfile" \
        "route=signal, LoadState=-: signal restart needs pidfile $f/go-trader-sigmissing/go-trader.pid"

    load_case() {
        local name="$1" state="$2" verdict="$3"
        local cf="$ow_root/load-$name"
        mk_fleet "$cf"
        mk_dir "$cf" go-trader-x
        case "$state" in
            none) ;;
            rc1) unit_set "$cf/st" go-trader-parent LoadState loaded; unit_rc "$cf/st" go-trader-parent LoadState 1 ;;
            *) unit_set "$cf/st" go-trader-parent LoadState "$state" ;;
        esac
        case "$name" in
            masked-empty)
                : >"$cf/frag.service"
                unit_set "$cf/st" go-trader-parent FragmentPath "$cf/frag.service"
                ;;
            masked-link)
                ln -s /dev/null "$cf/frag.service"
                unit_set "$cf/st" go-trader-parent FragmentPath "$cf/frag.service"
                ;;
            inactive-other)
                unit_set "$cf/st" go-trader-parent ActiveState inactive
                unit_set "$cf/st" go-trader-parent WorkingDirectory /srv/another-deployment
                ;;
            not-found-fragment)
                unit_set "$cf/st" go-trader-parent FragmentPath /etc/systemd/system/go-trader-parent.service
                ;;
            frag-rc1)
                unit_rc "$cf/st" go-trader-parent FragmentPath 1
                ;;
        esac
        rc=$(run_fleet "$cf" "$ow_root/shim:$PATH" -- --all --restart)
        assert_eq "$rc" "0" "owner load state $name: the deployment stays in the batch"
        expect_spawn "$cf" go-trader-x "owner load state $name"
        expect_text "$cf" "$cf/go-trader-x restart route: $verdict" "owner load state $name: verdict"
    }
    load_case masked-empty masked owned
    load_case masked-link masked owned
    load_case inactive-other loaded owned
    load_case bad-setting bad-setting unknown
    load_case error error unknown
    load_case not-found-fragment not-found unknown
    load_case frag-rc1 not-found unknown
    load_case rc1 rc1 unknown
    load_case empty "" unknown
    load_case absent none unknown
    load_case malformed "LoadState=loaded" unknown
    load_case two-lines $'loaded\nloaded' unknown

    f="$ow_root/f5"
    mk_fleet "$f"
    mk_dir "$f" go-trader-x
    for mode in systemd signal; do
        rc=$(run_fleet "$f" "$ow_root/nosys" -- --all --restart --restart-mode "$mode")
        assert_eq "$rc" "0" "owner no systemctl ($mode): the deployment stays in the batch"
        expect_spawn "$f" go-trader-x "owner no systemctl ($mode)"
        expect_text "$f" "$f/go-trader-x restart route: unknown (route=$mode, LoadState=-: systemctl is not available" "owner no systemctl ($mode): verdict"
    done

    f="$ow_root/f6"
    mk_fleet "$f"
    mk_dir "$f" go-trader-live
    mk_dir "$f" go-trader-marked
    map_unit "$f" go-trader-live go-trader-live.service
    unit_set "$f/st" go-trader-parent LoadState not-found
    unit_set "$f/st" go-trader-parent FragmentPath ""
    mkdir -p "$f/rec"
    ow_key=$(update_recovery_key "$f/go-trader-marked")
    update_write_incomplete_marker "$f/rec" "$ow_key" "$f/go-trader-marked" "$f/rec/$ow_key" "fixture recovery failure" "fixture"
    rc=$(run_fleet "$f" "$ow_root/shim:$PATH" -- --all --restart)
    [[ "$rc" != "0" ]] || owner_fail "owner marker fleet: a no-owner tree held by a blocking marker must fail the batch" "$f/out"
    expect_spawn "$f" go-trader-live "owner marker fleet: the healthy tree still updates"
    ! spawned "$f" go-trader-marked || owner_fail "owner marker fleet: the marked tree was dispatched" "$f/out"
    expect_text "$f" "FAILED in $f/go-trader-marked (incomplete recovery marker blocks update of $f/go-trader-marked; inspect the saved snapshot and run --clear-recovery" "owner marker fleet: the parent names the marker"
    expect_text "$f" "REFUSED: incomplete recovery marker $f/rec/$ow_key.incomplete" "owner marker fleet: the marker check names the marker file"
    expect_text "$f" "1 failing instance(s) (1 updated, 0 skipped)" "owner marker fleet: counted as a failing instance"
    ! grep -qF "WARNING: skipping $f/go-trader-marked" "$f/out" || owner_fail "owner marker fleet: the marked tree was reported as a skip" "$f/out"
    [[ -f "$f/rec/$ow_key.incomplete" ]] || owner_fail "owner marker fleet: the marker was removed" "$f/out"
    sed -i.bak 's/^deploy_dev_ino=.*/deploy_dev_ino=0:1/' "$f/rec/$ow_key.incomplete"
    rm -f "$f/rec/$ow_key.incomplete.bak"
    rc=$(run_fleet "$f" "$ow_root/shim:$PATH" -- --all --restart)
    assert_eq "$rc" "0" "owner marker fleet: a stale marker does not block the skip"
    expect_skip "$f" go-trader-marked "owner marker fleet: a stale marker" \
        "route=signal-fallback, LoadState=not-found: signal restart needs pidfile $f/go-trader-marked/go-trader.pid"
    expect_text "$f" "all instances OK (1 updated, 1 skipped of 2 discovered)" "owner marker fleet: stale-marker totals"
)

ow_bin="$ow_root/bin"
mkdir -p "$ow_bin"
cp "$ow_root/shim/systemctl" "$ow_bin/systemctl"
cat >"$ow_bin/uv" <<'EOS'
#!/usr/bin/env bash
if [[ "${1:-}" == "--version" ]]; then echo 'uv 0.0.0'; exit 0; fi
echo "uv $*" >>"${OWNER_TOOL_LOG:?}"
mkdir -p .venv/bin
printf '#!/bin/sh\necho Python 3.12.9\n' >.venv/bin/python3
chmod +x .venv/bin/python3
echo synced >.venv/stamp
EOS
cat >"$ow_bin/go" <<'EOS'
#!/usr/bin/env bash
if [[ "${1:-}" == "version" ]]; then echo 'go version go1.26.2'; exit 0; fi
echo "go $*" >>"${OWNER_TOOL_LOG:?}"
dir=. out= ver=dev
while [[ $# -gt 0 ]]; do
    case "$1" in
        -C) dir="$2"; shift 2 ;;
        -o) out="$2"; shift 2 ;;
        -ldflags) ver="${2#*main.Version=}"; ver="${ver%% *}"; shift 2 ;;
        *) shift ;;
    esac
done
case "$out" in /*) ;; *) out="$dir/$out" ;; esac
printf '#!/usr/bin/env bash\nif [[ "$1" == "version" ]]; then echo "%s"; exit 0; fi\nif [[ "$1" == "probe" || "$1" == "agent-info" ]]; then exit 0; fi\nexec sleep 60\n' "$ver" >"$out"
chmod +x "$out"
EOS
printf '#!/usr/bin/env bash\n"$@"\n' >"$ow_bin/sudo"
printf '#!/usr/bin/env python3\nimport os, sys\ntry:\n    os.setsid()\nexcept OSError:\n    pass\nos.execvp(sys.argv[1], sys.argv[1:])\n' >"$ow_bin/setsid"
cat >"$ow_bin/curl" <<'EOS'
#!/usr/bin/env bash
ver=$(./go-trader version 2>/dev/null || echo dev)
printf '{"version":"%s","pid":99999}\n' "$ver"
EOS
chmod +x "$ow_bin"/*

(
    export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@example.com GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@example.com
    export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
    declare -a ow_pids=()
    cleanup_owner_pids() {
        local p
        for p in ${ow_pids[@]+"${ow_pids[@]}"}; do
            kill -KILL "$p" 2>/dev/null || true
        done
        for p in "$ow_root"/d-*/deploy/go-trader.pid; do
            [[ -f "$p" ]] && kill -KILL "$(cat "$p")" 2>/dev/null || true
        done
    }
    trap cleanup_owner_pids EXIT

    mk_deploy() {
        local p="$ow_root/d-$1"
        mkdir -p "$p/proc/self" "$p/st"
        git init -q --bare "$p/origin.git"
        git -C "$p/origin.git" symbolic-ref HEAD refs/heads/main
        git clone -q "$p/origin.git" "$p/deploy" 2>/dev/null
        git -C "$p/deploy" symbolic-ref HEAD refs/heads/main
        mkdir -p "$p/deploy/scripts" "$p/deploy/scheduler"
        cp "$owner_repo_root/scripts/update.sh" "$owner_repo_root/scripts/update_helpers.sh" "$p/deploy/scripts/"
        printf 'package main\nfunc main() {}\n' >"$p/deploy/scheduler/main.go"
        printf 'v1\n' >"$p/deploy/README.md"
        printf 'go-trader\ngo-trader.*\nrun.sh\nwrapper.pid\nscheduler/config.json\nscheduler/tuning_runs/\nscheduler/logs/\nstate/\n.venv/\n' >"$p/deploy/.gitignore"
        git -C "$p/deploy" add .
        git -C "$p/deploy" commit -q -m v1
        git -C "$p/deploy" push -q -u origin HEAD:main 2>/dev/null
        git clone -q -b main "$p/origin.git" "$p/work" 2>/dev/null
        printf 'v2\n' >"$p/work/README.md"
        git -C "$p/work" commit -q -am v2
        git -C "$p/work" push -q origin HEAD:main 2>/dev/null
        echo '{}' >"$p/deploy/scheduler/config.json"
        printf '#!/usr/bin/env bash\nif [[ "$1" == "version" ]]; then echo v1; exit 0; fi\nif [[ "$1" == "probe" || "$1" == "agent-info" ]]; then exit 0; fi\nexec sleep 60\n' >"$p/deploy/go-trader"
        chmod 755 "$p/deploy/go-trader"
        mkdir -p "$p/deploy/.venv/bin"
        printf '#!/bin/sh\necho Python 3.12.9\n' >"$p/deploy/.venv/bin/python3"
        chmod 755 "$p/deploy/.venv/bin/python3"
        : >"$p/tools.log"
        printf '%s' "$p"
    }
    run_deploy() {
        local p="$1"
        shift
        local -a envs=()
        while [[ $# -gt 0 && "$1" != "--" ]]; do
            envs+=("$1")
            shift
        done
        shift
        local rc=0
        (
            cd "$p/deploy"
            env -u GO_TRADER_PIDFILE -u GO_TRADER_RUN_SH -u RESTART -u RESTART_MODE -u GO_TRADER_SERVICE \
                -u GO_TRADER_UPDATE_PAUSE_AT -u GO_TRADER_UPDATE_FAIL_AT \
                PATH="$ow_bin:$PATH" SYSTEMCTL_SHIM_STATE="$p/st" OWNER_TOOL_LOG="$p/tools.log" \
                GO_TRADER_PROC_ROOT="$p/proc" GO_TRADER_UPDATE_RECOVERY_DIR="$p/rec" \
                ACTIVE_TIMEOUT=5 HEALTH_TIMEOUT=5 RECOVERY_HEALTH_TIMEOUT=5 \
                ${envs[@]+"${envs[@]}"} \
                bash scripts/update.sh "$@"
        ) >"$p/out" 2>&1 || rc=$?
        printf '%s' "$rc"
    }
    out_has() {
        grep -qF -- "$2" "$1/out" || owner_fail "$3: output lacks '$2'" "$1/out"
    }
    out_lacks() {
        ! grep -qF -- "$2" "$1/out" || owner_fail "$3: output has '$2'" "$1/out"
    }
    start_dummy() {
        local pid
        pid=$(sleep 60 >/dev/null 2>&1 & echo "$!")
        ow_pids+=("$pid")
        echo "$pid" >"$1/deploy/go-trader.pid"
    }
    write_exec_wrapper() {
        printf '#!/usr/bin/env bash\necho $$ > ./go-trader.pid\nexec ./go-trader --config scheduler/config.json\n' >"$1/deploy/run.sh"
        chmod 755 "$1/deploy/run.sh"
    }

    for how in flag env; do
        p=$(mk_deploy "refuse-$how")
        unit_set "$p/st" go-trader LoadState not-found
        head_before=$(git -C "$p/deploy" rev-parse HEAD)
        if [[ "$how" == "flag" ]]; then
            rc=$(run_deploy "$p" -- --restart)
        else
            rc=$(run_deploy "$p" RESTART=1 --)
        fi
        [[ "$rc" != "0" ]] || owner_fail "owner refusal ($how): a proven no-owner restart must fail" "$p/out"
        out_has "$p" "FAIL phase=preflight: restart route cannot succeed for $p/deploy (route=signal-fallback, LoadState=not-found" "owner refusal ($how)"
        out_lacks "$p" "phase: snapshot" "owner refusal ($how): no snapshot phase"
        [[ ! -e "$p/rec" ]] || owner_fail "owner refusal ($how): the recovery directory exists (lock, snapshot or marker)" "$p/out"
        [[ ! -s "$p/tools.log" ]] || owner_fail "owner refusal ($how): uv or go ran" "$p/tools.log"
        assert_eq "$(git -C "$p/deploy" rev-parse HEAD)" "$head_before" "owner refusal ($how): HEAD unchanged"
        assert_eq "$(cat "$p/deploy/README.md")" "v1" "owner refusal ($how): tracked files unchanged"
        assert_eq "$("$p/deploy/go-trader" version)" "v1" "owner refusal ($how): binary unchanged"
        [[ ! -e "$p/deploy/go-trader.new" ]] || owner_fail "owner refusal ($how): a staged binary exists" "$p/out"
    done

    p=$(mk_deploy refuse-marker)
    unit_set "$p/st" go-trader LoadState not-found
    mkdir -p "$p/rec"
    ow_key=$(update_recovery_key "$p/deploy")
    update_write_incomplete_marker "$p/rec" "$ow_key" "$p/deploy" "$p/rec/$ow_key" "fixture recovery failure" "fixture"
    head_before=$(git -C "$p/deploy" rev-parse HEAD)
    rc=$(run_deploy "$p" -- --restart)
    [[ "$rc" != "0" ]] || owner_fail "owner marker refusal: a marked no-owner tree must fail" "$p/out"
    out_has "$p" "FAIL phase=preflight: incomplete recovery marker blocks update of $p/deploy; inspect the saved snapshot and run --clear-recovery" "owner marker refusal: the marker message"
    out_has "$p" "REFUSED: incomplete recovery marker $p/rec/$ow_key.incomplete" "owner marker refusal: the marker file"
    out_lacks "$p" "restart route cannot succeed" "owner marker refusal: no restart-route advice"
    out_lacks "$p" "phase: snapshot" "owner marker refusal: no snapshot phase"
    [[ ! -e "$p/rec/$ow_key.lock" ]] || owner_fail "owner marker refusal: a lock exists" "$p/out"
    [[ ! -e "$p/rec/$ow_key" ]] || owner_fail "owner marker refusal: a snapshot directory exists" "$p/out"
    [[ -f "$p/rec/$ow_key.incomplete" ]] || owner_fail "owner marker refusal: the marker was removed" "$p/out"
    [[ ! -s "$p/tools.log" ]] || owner_fail "owner marker refusal: uv or go ran" "$p/tools.log"
    assert_eq "$(git -C "$p/deploy" rev-parse HEAD)" "$head_before" "owner marker refusal: HEAD unchanged"

    p=$(mk_deploy redirect)
    unit_set "$p/st" go-trader ActiveState active
    unit_set "$p/st" go-trader LoadState loaded
    unit_set "$p/st" go-trader ExecStart "$p/deploy/go-trader --config $p/deploy/scheduler/config.json"
    unit_set "$p/st" go-trader FragmentPath /usr/lib/systemd/system/go-trader.service
    rc=$(run_deploy "$p" -- --restart --restart-mode signal)
    [[ "$rc" != "0" ]] || owner_fail "owner redirect: the vendor fragment refusal must still stop the update" "$p/out"
    out_has "$p" "restart route: owned (route=systemd-redirect, LoadState=loaded" "owner redirect: the child keeps the redirect"
    out_has "$p" "routing restart through systemctl" "owner redirect: the existing redirect runs"
    out_has "$p" "outside /etc/systemd/system" "owner redirect: the existing fragment check runs next"
    out_lacks "$p" "restart route cannot succeed" "owner redirect: no owner refusal"
    out_lacks "$p" "phase: snapshot" "owner redirect: no snapshot phase"
    [[ -z "$(find "$p/rec" -name '*.incomplete' 2>/dev/null)" ]] || owner_fail "owner redirect: an incomplete marker exists" "$p/out"

    p=$(mk_deploy codeonly)
    unit_set "$p/st" go-trader LoadState not-found
    rc=$(run_deploy "$p" --)
    assert_eq "$rc" "0" "owner code-only: an update without restart completes"
    out_has "$p" "build OK at" "owner code-only"
    out_lacks "$p" "restart route" "owner code-only: no owner check runs"
    assert_eq "$(cat "$p/deploy/README.md")" "v2" "owner code-only: the pull landed"
    [[ -z "$(find "$p/rec" -name '*.incomplete' 2>/dev/null)" ]] || owner_fail "owner code-only: an incomplete marker exists" "$p/out"

    p=$(mk_deploy transfer)
    printf '#!/bin/sh\nexit 0\n' >"$p/work/run.sh"
    chmod 755 "$p/work/run.sh"
    rc=$(run_deploy "$p" -- --rsync-from "$p/work")
    assert_eq "$rc" "0" "owner transfer: a code-only rsync completes"
    [[ -f "$p/deploy/run.sh" && -x "$p/deploy/run.sh" ]] || owner_fail "owner transfer: the managed transfer did not supply an executable wrapper" "$p/out"
    chmod 644 "$p/deploy/run.sh"
    rc=$(run_deploy "$p" -- --rsync-from "$p/work")
    assert_eq "$rc" "0" "owner transfer: a second code-only rsync completes"
    [[ -x "$p/deploy/run.sh" ]] || owner_fail "owner transfer: the managed transfer did not repair a non-executable wrapper" "$p/out"

    p=$(mk_deploy fallback)
    unit_set "$p/st" go-trader LoadState not-found
    printf '5\n' >"$p/st/u/go-trader/restart-rcs"
    write_exec_wrapper "$p"
    start_dummy "$p"
    rc=$(run_deploy "$p" -- --restart)
    assert_eq "$rc" "0" "owner fallback: a missing unit with pidfile and wrapper restarts through signal"
    out_has "$p" "restart route: owned (route=signal-fallback, LoadState=not-found" "owner fallback: verdict"
    out_has "$p" "falling back to signal restart" "owner fallback: the signal fallback runs"
    out_has "$p" "[update] done:" "owner fallback: the update finishes"
    ow_pids+=("$(cat "$p/deploy/go-trader.pid")")

    p=$(mk_deploy nowrapper)
    unit_set "$p/st" go-trader LoadState not-found
    printf '5\n5\n' >"$p/st/u/go-trader/restart-rcs"
    start_dummy "$p"
    rc=$(run_deploy "$p" -- --restart)
    [[ "$rc" != "0" ]] || owner_fail "owner no wrapper: a restart without a wrapper must fail" "$p/out"
    out_has "$p" "restart route: unknown (route=signal-fallback" "owner no wrapper: an uncertain wrapper continues"
    out_has "$p" "signal fallback requires GO_TRADER_RUN_SH" "owner no wrapper: the restart prerequisite message is unchanged"
    out_has "$p" "recovery: signal fallback unavailable (missing executable" "owner no wrapper: the recovery prerequisite message is unchanged"
    out_has "$p" "recovery: INCOMPLETE" "owner no wrapper: recovery reports the failure"
    [[ -n "$(find "$p/rec" -name '*.incomplete' 2>/dev/null)" ]] || owner_fail "owner no wrapper: no incomplete marker" "$p/out"
    assert_eq "$("$p/deploy/go-trader" version)" "v1" "owner no wrapper: the previous binary is restored"

    for variant in removed kept; do
        p=$(mk_deploy "recovery-$variant")
        unit_set "$p/st" go-trader LoadState loaded
        unit_set "$p/st" go-trader ActiveState active
        unit_set "$p/st" go-trader MainPID 4242
        printf '0\n5\n' >"$p/st/u/go-trader/restart-rcs"
        write_exec_wrapper "$p"
        start_dummy "$p"
        sentinel="$p/pause"
        (run_deploy "$p" GO_TRADER_UPDATE_PAUSE_AT="probe:$sentinel" GO_TRADER_UPDATE_FAIL_AT=verify_health -- --restart >"$p/rc") &
        bg=$!
        waited=0
        until [[ -s "$sentinel.reached" ]]; do
            if [[ $waited -ge 300 ]] || ! kill -0 "$bg" 2>/dev/null; then
                wait "$bg" 2>/dev/null || true
                owner_fail "owner recovery $variant: the update never reached the probe pause" "$p/out"
            fi
            sleep 0.1
            waited=$((waited + 1))
        done
        [[ "$variant" == "removed" ]] && rm -rf "$p/proc/self"
        touch "$sentinel"
        wait "$bg" || true
        [[ "$(cat "$p/rc")" != "0" ]] || owner_fail "owner recovery $variant: a forced health failure must fail" "$p/out"
        if [[ -f "$p/deploy/go-trader.pid" ]]; then
            ow_pids+=("$(cat "$p/deploy/go-trader.pid")")
        fi
        out_has "$p" "restart route: owned (route=systemd, LoadState=loaded" "owner recovery $variant: verdict"
        out_has "$p" "recovery: systemd unit go-trader not found" "owner recovery $variant: recovery hits the missing unit"
        if [[ "$variant" == "removed" ]]; then
            out_has "$p" "recovery: signal fallback unavailable (need Linux + pidfile)" "owner recovery removed: recovery reads the proc root when it runs"
            [[ -n "$(find "$p/rec" -name '*.incomplete' 2>/dev/null)" ]] || owner_fail "owner recovery removed: no incomplete marker" "$p/out"
        else
            out_has "$p" "recovery: complete" "owner recovery kept: the signal respawn recovers"
            out_lacks "$p" "signal fallback unavailable" "owner recovery kept: the wrapper is still there"
        fi
    done
)

rm -rf "$ow_root"

cpu_root=$(mktemp -d)
(
    set -euo pipefail
    cpu_fail() {
        echo "FAIL: cpu: $1" >&2
        [[ -n "${2:-}" && -f "${2:-}" ]] && cat "$2" >&2
        exit 1
    }

    if [[ -n "${GO_TRADER_BIN:-}" && -x "${GO_TRADER_BIN:-}" ]]; then
        cc="$cpu_root/class"
        mkdir -p "$cc"
        hl_paper='{"id":"hl-a","type":"perps","platform":"hyperliquid","script":"shared_scripts/check_hyperliquid.py","args":["sma_crossover","BTC","1h","--mode=paper"],"capital":1000}'
        hl_live='{"id":"hl-b","type":"perps","platform":"hyperliquid","script":"shared_scripts/check_hyperliquid.py","args":["sma_crossover","ETH","1h","--mode=live"],"capital":1000}'
        hl_pct='{"id":"hl-c","type":"perps","platform":"hyperliquid","script":"shared_scripts/check_hyperliquid.py","args":["sma_crossover","SOL","1h","--mode=paper"],"capital_pct":0.5}'
        printf '{"interval_seconds":300,"paper_only":true,"strategies":[%s]}\n' "$hl_paper" >"$cc/paper.json"
        printf '{"interval_seconds":300,"strategies":[%s]}\n' "$hl_paper" >"$cc/roster.json"
        printf '{"interval_seconds":300,"strategies":[%s,%s]}\n' "$hl_paper" "$hl_live" >"$cc/live.json"
        printf '{"interval_seconds":300,"strategies":[]}\n' >"$cc/empty.json"
        printf '{"role":"feed","strategies":[]}\n' >"$cc/feed.json"
        printf '{"interval_seconds":300,"paper_only":true,"strategies":[%s]}\n' "$hl_pct" >"$cc/pct.json"
        printf '{"interval_seconds":300,"paper_only":true,"strategies":[%s,%s]}\n' "$hl_paper" "$hl_live" >"$cc/refused.json"
        for want in paper:paper roster:live live:live empty:live feed:feed pct:paper; do
            name="${want%%:*}"
            out=$(cd "$cc" && env -u HYPERLIQUID_ACCOUNT_ADDRESS "$GO_TRADER_BIN" cpu-class --config "$name.json" 2>"$cc/$name.err") \
                || cpu_fail "cpu-class $name exited non-zero" "$cc/$name.err"
            [[ "$out" == "${want#*:}" ]] || cpu_fail "cpu-class $name printed '$out', want '${want#*:}'" "$cc/$name.err"
        done
        grep -q '^\[INFO\]' "$cc/paper.err" || cpu_fail "cpu-class must keep the loader's [INFO] lines on stderr" "$cc/paper.err"
        before=$(cd "$cc" && find . | sort)
        rc=0
        out=$(cd "$cc" && "$GO_TRADER_BIN" cpu-class --config missing.json 2>"$cc/missing.err") || rc=$?
        [[ "$rc" != "0" && -z "$out" ]] || cpu_fail "cpu-class on a missing config must exit non-zero with no stdout (rc=$rc out=$out)"
        rc=0
        out=$(cd "$cc" && "$GO_TRADER_BIN" cpu-class --config refused.json 2>"$cc/refused.err") || rc=$?
        [[ "$rc" != "0" && -z "$out" ]] || cpu_fail "cpu-class on a paper_only config with a live strategy must fail (rc=$rc out=$out)"
        grep -q 'strategy\[hl-b\]: paper_only is true' "$cc/refused.err" || cpu_fail "the paper_only refusal must name the live strategy" "$cc/refused.err"
        after=$(cd "$cc" && find . -name '*.err' -prune -o -print | sort)
        [[ "$(printf '%s\n' "$before" | grep -v '\.err$')" == "$after" ]] || cpu_fail "cpu-class created or removed files: before=[$before] after=[$after]"
    else
        echo "note: GO_TRADER_BIN unset; cpu-class binary cases skipped"
    fi

    stub="$cpu_root/bin"
    units="$cpu_root/units"
    mkdir -p "$stub" "$units"
    cat >"$stub/systemctl" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$CPU_TEST_LOG"
case "$*" in
    --version) printf 'systemd %s (255.4-1)\n+PAM\n' "${CPU_TEST_SYSTEMD:-255}" ;;
    *"-p User --value"*) printf '%s\n' "${CPU_TEST_USER-}" ;;
    *"-p Group --value"*) printf '\n' ;;
    *"-p WorkingDirectory --value"*) printf '%s\n' "$CPU_TEST_WD" ;;
    *"-p ExecStart --value"*) printf '{ path=%s ; argv[]=%s --config %s ; ignore_errors=no }\n' "$CPU_TEST_BIN" "$CPU_TEST_BIN" "$CPU_TEST_CFG" ;;
    *"-p Environment --value"*) printf '%s\n' "${CPU_TEST_ENV-}" ;;
    *"-p EnvironmentFiles --value"*) printf '%s\n' "${CPU_TEST_ENVFILE-}" ;;
esac
exit 0
EOF
    cat >"$stub/systemd-run" <<'EOF'
#!/usr/bin/env bash
printf 'systemd-run %s\n' "$*" >>"$CPU_TEST_LOG"
while [[ $# -gt 0 && "$1" != "--" ]]; do shift; done
shift
exec "$@"
EOF
    cat >"$cpu_root/fake-go-trader" <<'EOF'
#!/usr/bin/env bash
[[ "$1" == "cpu-class" ]] || exit 2
echo "[INFO] loader noise" >&2
case "${CPU_TEST_CLASS:-}" in
    fail) echo "cpu-class: load config: broken" >&2; exit 1 ;;
    junk) printf 'paper\nlive\n'; exit 0 ;;
    *) printf '%s\n' "$CPU_TEST_CLASS" ;;
esac
EOF
    chmod +x "$stub/systemctl" "$stub/systemd-run" "$cpu_root/fake-go-trader"
    export PATH="$stub:$PATH" UPDATE_UNIT_SUDO="" UPDATE_SYSTEMD_UNIT_DIR="$units"
    export CPU_TEST_LOG="$cpu_root/calls.log" CPU_TEST_USER="$(id -un)" CPU_TEST_WD="$cpu_root" CPU_TEST_BIN="$cpu_root/fake-go-trader" CPU_TEST_CFG="$cpu_root/config.json"
    : >"$CPU_TEST_LOG"

    unit=go-trader@paper.service
    dest=$(update_cpu_dropin_path go-trader@paper)
    assert_eq "$dest" "$units/go-trader@paper.service.d/40-go-trader-cpu.conf" "cpu: drop-in path for a template instance"
    assert_eq "$(update_cpu_dropin_path go-trader)" "" "cpu: no drop-in path for the plain unit"
    assert_eq "$(update_cpu_dropin_path go-trader@bad/x)" "" "cpu: no drop-in path for an invalid instance"
    assert_eq "$(update_cpu_dropin_path other@paper.service)" "" "cpu: no drop-in path for a foreign template"
    assert_eq "$(update_unit_config_path "$unit")" "$cpu_root/config.json" "cpu: config path from ExecStart"
    assert_eq "$(update_unit_role "$unit")" "scheduler" "cpu: update_unit_role is unchanged for a missing config"

    plan() { update_cpu_dropin_plan "$unit" "$cpu_root/fake-go-trader" "$dest" 2>>"$cpu_root/plan.err"; }
    apply() {
        local p d w
        p=$(plan)
        d="${p%% *}"
        w="${p#* }"
        update_cpu_dropin_apply "$d" "$dest" "$w" 2>>"$cpu_root/apply.err" || cpu_fail "apply $d failed" "$cpu_root/apply.err"
        printf '%s' "$d"
    }

    mkdir -p "${dest%/*}"
    printf '[Service]\nCPUWeight=100\n' >"${dest%/*}/50-local.conf"
    : >"$cpu_root/apply.err"
    assert_eq "$(CPU_TEST_CLASS=paper apply)" "install" "cpu: paper class installs the drop-in"
    assert_eq "$(cat "$dest"; printf x)" "$(printf '[Service]\nCPUWeight=20\nx')" "cpu: installed drop-in body"
    grep -q '50-local.conf sorts after 40-go-trader-cpu.conf and sets CPUWeight=' "$cpu_root/apply.err" || cpu_fail "a later local CPUWeight drop-in must be named in a WARN" "$cpu_root/apply.err"
    rm -f "${dest%/*}/50-local.conf"
    update_cpu_dropin_is_managed "$dest" || cpu_fail "the installed drop-in must be in the managed form"
    [[ -z "$(find "${dest%/*}" -name '*.tmp-*')" ]] || cpu_fail "a staged temp file was left behind"
    assert_eq "$(CPU_TEST_CLASS=paper plan)" "none 20" "cpu: an unchanged drop-in needs nothing"
    assert_eq "$(CPU_TEST_CLASS=paper CPU_TEST_ENV='GO_TRADER_PAPER_CPU_WEIGHT=50' apply)" "replace" "cpu: a new weight replaces the drop-in"
    assert_eq "$(sed -n 's/^CPUWeight=//p' "$dest")" "50" "cpu: replaced weight"
    : >"$cpu_root/plan.err"
    assert_eq "$(CPU_TEST_CLASS=paper CPU_TEST_ENV='GO_TRADER_PAPER_CPU_WEIGHT=abc' plan)" "replace 20" "cpu: an invalid weight uses 20"
    grep -q "GO_TRADER_PAPER_CPU_WEIGHT='abc' is not an integer" "$cpu_root/plan.err" || cpu_fail "an invalid weight must WARN" "$cpu_root/plan.err"
    assert_eq "$(CPU_TEST_CLASS=paper CPU_TEST_ENV='GO_TRADER_PAPER_CPU_WEIGHT=10001' plan)" "replace 20" "cpu: an out-of-range weight uses 20"
    assert_eq "$(CPU_TEST_CLASS=paper CPU_TEST_ENV='GO_TRADER_PAPER_CPU_WEIGHT=0' plan)" "remove " "cpu: weight 0 removes the drop-in"
    printf 'GO_TRADER_PAPER_CPU_WEIGHT=0\n' >"$cpu_root/unit.env"
    assert_eq "$(CPU_TEST_CLASS=paper CPU_TEST_ENVFILE="$cpu_root/unit.env (ignore_errors=no)" plan)" "remove " "cpu: weight 0 from an EnvironmentFile removes the drop-in"
    printf 'GO_TRADER_PAPER_CPU_WEIGHT=45\n' >"$cpu_root/unit.env"
    assert_eq "$(CPU_TEST_CLASS=paper CPU_TEST_ENVFILE="$cpu_root/unit.env (ignore_errors=no)" plan)" "replace 45" "cpu: a weight from an EnvironmentFile is used"
    if [[ "$(id -u)" != "0" ]]; then
        chmod 000 "$cpu_root/unit.env"
        : >"$cpu_root/plan.err"
        assert_eq "$(CPU_TEST_CLASS=paper CPU_TEST_ENVFILE="$cpu_root/unit.env (ignore_errors=no)" plan)" "remove " "cpu: an unreadable EnvironmentFile removes the drop-in"
        grep -q "could not read the environment files of $unit" "$cpu_root/plan.err" || cpu_fail "an unreadable EnvironmentFile must WARN" "$cpu_root/plan.err"
        chmod 600 "$cpu_root/unit.env"
        mkdir -p "$cpu_root/sealed"
        printf 'GO_TRADER_PAPER_CPU_WEIGHT=0\n' >"$cpu_root/sealed/.env"
        chmod 000 "$cpu_root/sealed"
        : >"$cpu_root/plan.err"
        assert_eq "$(CPU_TEST_CLASS=paper CPU_TEST_ENVFILE="$cpu_root/sealed/.env (ignore_errors=no)" plan)" "remove " "cpu: an EnvironmentFile in an unsearchable folder removes the drop-in"
        grep -q "could not read the environment files of $unit" "$cpu_root/plan.err" || cpu_fail "an EnvironmentFile in an unsearchable folder must WARN" "$cpu_root/plan.err"
        chmod 700 "$cpu_root/sealed"
    fi
    assert_eq "$(CPU_TEST_CLASS=paper CPU_TEST_ENVFILE="$cpu_root/missing.env (ignore_errors=yes)" plan)" "replace 20" "cpu: a missing optional EnvironmentFile is unset"
    assert_eq "$(CPU_TEST_CLASS=paper CPU_TEST_SYSTEMD=231 plan)" "remove " "cpu: systemd older than 232 removes the drop-in"
    for old_systemd in 231 233 235; do
        : >"$CPU_TEST_LOG"
        : >"$cpu_root/plan.err"
        assert_eq "$(CPU_TEST_CLASS=paper CPU_TEST_SYSTEMD=$old_systemd plan)" "remove " "cpu: systemd $old_systemd removes the drop-in"
        ! grep -q '^systemd-run' "$CPU_TEST_LOG" || cpu_fail "the classifier ran under systemd $old_systemd" "$CPU_TEST_LOG"
        grep -qF "[cpu] systemd $old_systemd is older than 236 or unknown" "$cpu_root/plan.err" || cpu_fail "systemd $old_systemd must log the version gate" "$cpu_root/plan.err"
        ! grep -qE 'has no CPU class|cpu-class .* failed' "$cpu_root/plan.err" || cpu_fail "systemd $old_systemd must not log a classifier failure" "$cpu_root/plan.err"
    done
    assert_eq "$(CPU_TEST_CLASS=paper CPU_TEST_SYSTEMD=236 plan)" "replace 20" "cpu: systemd 236 runs the classifier"
    assert_eq "$(CPU_TEST_CLASS=live plan)" "remove " "cpu: live class removes the drop-in"
    assert_eq "$(CPU_TEST_CLASS=feed plan)" "remove " "cpu: feed class removes the drop-in"
    assert_eq "$(CPU_TEST_CLASS=fail plan)" "remove " "cpu: a classifier failure removes the drop-in"
    assert_eq "$(CPU_TEST_CLASS=junk plan)" "remove " "cpu: a two-line classifier result removes the drop-in"
    assert_eq "$(CPU_TEST_CLASS=paper CPU_TEST_USER='' plan)" "remove " "cpu: a root unit never runs the classifier"
    : >"$CPU_TEST_LOG"
    CPU_TEST_CLASS=paper CPU_TEST_USER='' plan >/dev/null
    ! grep -q '^systemd-run' "$CPU_TEST_LOG" || cpu_fail "the classifier ran for a root unit" "$CPU_TEST_LOG"
    CPU_TEST_CLASS=paper plan >/dev/null
    grep -q "^systemd-run .*--uid=$(id -u) .*-p ProtectSystem=strict .*-p RuntimeMaxSec=60 .*-- $cpu_root/fake-go-trader cpu-class --config $cpu_root/config.json" "$CPU_TEST_LOG" \
        || cpu_fail "the classifier must run sandboxed as the unit owner" "$CPU_TEST_LOG"
    assert_eq "$(CPU_TEST_CLASS=live apply)" "remove" "cpu: live class removes the file"
    [[ ! -e "$dest" && ! -d "${dest%/*}" ]] || cpu_fail "remove must delete the file and the empty drop-in directory"
    mkdir -p "${dest%/*}"
    printf 'keep\n' >"${dest%/*}/60-keep.conf"
    CPU_TEST_CLASS=paper apply >/dev/null
    assert_eq "$(CPU_TEST_CLASS=live apply)" "remove" "cpu: remove with another drop-in present"
    [[ ! -e "$dest" && -f "${dest%/*}/60-keep.conf" ]] || cpu_fail "remove must keep other drop-ins and their directory"
    assert_eq "$(CPU_TEST_CLASS=live plan)" "none " "cpu: absent and not wanted needs nothing"
    ln -s "${dest%/*}/60-keep.conf" "$dest"
    assert_eq "$(CPU_TEST_CLASS=paper plan)" "skip-symlink 20" "cpu: a symlink is left alone"
    assert_eq "$(CPU_TEST_CLASS=live plan)" "skip-symlink " "cpu: a symlink is never removed"
    rm -f "$dest" "${dest%/*}/60-keep.conf"
    rmdir "${dest%/*}"

    form="$cpu_root/form"
    mkdir -p "$form"
    printf '[Service]\nCPUWeight=20\n' >"$form/ok1"
    printf '[Service]\nCPUWeight=10000\n' >"$form/ok2"
    printf '[Service]\nCPUWeight=10001\n' >"$form/bad1"
    printf '[Service]\nCPUWeight=0\n' >"$form/bad2"
    printf '[Service]\nCPUWeight=20' >"$form/bad3"
    printf '[Service]\nCPUWeight=20\nNice=5\n' >"$form/bad4"
    printf '# local\n[Service]\nCPUWeight=20\n' >"$form/bad5"
    printf '[Service]\r\nCPUWeight=20\r\n' >"$form/bad6"
    ln -s "$form/ok1" "$form/bad7"
    for f in "$form"/*; do
        sh_ans=no
        update_cpu_dropin_is_managed "$f" && sh_ans=yes
        py_ans=$(python3 -I -c '
import importlib.util, sys
spec = importlib.util.spec_from_file_location("msl", sys.argv[1])
mod = importlib.util.module_from_spec(spec)
spec.loader.exec_module(mod)
print("yes" if mod.cpu_dropin_is_managed(sys.argv[2]) else "no")
' "$SCRIPT_DIR/migrate-service-layout.py" "$f")
        want=no
        [[ "${f##*/}" == ok* ]] && want=yes
        assert_eq "$sh_ans" "$want" "cpu: shell managed form for ${f##*/}"
        assert_eq "$py_ans" "$want" "cpu: Python managed form for ${f##*/}"
    done
    py_name=$(sed -n 's/^CPU_DROPIN_NAME = "\(.*\)"$/\1/p' "$SCRIPT_DIR/migrate-service-layout.py")
    assert_eq "$py_name" "${UPDATE_CPU_DROPIN_NAME}.conf" "cpu: migrate-service-layout.py and update_helpers.sh name the same drop-in"

    snap_deploy="$cpu_root/deploy"
    mkdir -p "$snap_deploy/scheduler"
    printf 'v1\n' >"$snap_deploy/README.md"
    printf '{"strategies":[]}\n' >"$snap_deploy/scheduler/config.json"
    git -C "$snap_deploy" init -q
    git -C "$snap_deploy" -c user.email=t@example.invalid -c user.name=t add README.md
    git -C "$snap_deploy" -c user.email=t@example.invalid -c user.name=t commit -qm v1
    skey=$(update_recovery_key "$snap_deploy")
    round_trip() {
        local label="$1" snap="$cpu_root/rec/$skey"
        rm -rf "$cpu_root/rec"
        UPDATE_NO_FREEZE_EXCLUDES=1 update_take_snapshot "$snap_deploy" "$snap" "" "" "" "$dest" >/dev/null 2>"$cpu_root/snap.err" \
            || cpu_fail "$label: snapshot failed" "$cpu_root/snap.err"
        grep -q "^cpu_dropin_dest=" "$snap/manifest.env" || cpu_fail "$label: manifest lacks cpu_dropin_dest"
        CPU_TEST_CLASS="$2" CPU_TEST_ENV="${3:-}" apply >/dev/null
        : >"$CPU_TEST_LOG"
        update_restore_snapshot_external "$snap" 0 0 0 0 "" 1 2>"$cpu_root/restore.err" || cpu_fail "$label: restore failed" "$cpu_root/restore.err"
        grep -qx 'daemon-reload' "$CPU_TEST_LOG" || cpu_fail "$label: restore must reload systemd" "$CPU_TEST_LOG"
    }
    round_trip "absent -> installed" paper
    [[ ! -e "$dest" && ! -d "${dest%/*}" ]] || cpu_fail "absent -> installed: restore must leave the drop-in absent"
    CPU_TEST_CLASS=paper CPU_TEST_ENV='GO_TRADER_PAPER_CPU_WEIGHT=35' apply >/dev/null
    before_fp=$(update_file_fingerprint "$dest")
    round_trip "present -> changed" paper 'GO_TRADER_PAPER_CPU_WEIGHT=70'
    assert_eq "$(update_file_fingerprint "$dest")" "$before_fp" "cpu: present -> changed restores the prior bytes"
    round_trip "present -> removed" live
    assert_eq "$(update_file_fingerprint "$dest")" "$before_fp" "cpu: present -> removed restores the prior bytes"
    : >"$CPU_TEST_LOG"
    update_restore_snapshot_external "$cpu_root/rec/$skey" 0 0 0 0 "" 0 2>/dev/null || cpu_fail "restore with nothing mutated failed"
    ! grep -qx 'daemon-reload' "$CPU_TEST_LOG" || cpu_fail "restore with nothing mutated must not reload" "$CPU_TEST_LOG"
)
rm -rf "$cpu_root"

echo "OK: update_helpers tests passed"

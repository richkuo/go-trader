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

echo "OK: update_helpers tests passed"

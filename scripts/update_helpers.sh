
update_systemd_envfile_check_path() {
    local entry="$1"
    local path="$entry"

    [[ -n "$path" ]] || return 0
    if [[ "$path" == '('* ]]; then
        return 0
    fi
    if [[ "$path" == -* ]]; then
        return 0
    fi
    if [[ "$path" == *' (ignore_errors='* ]]; then
        path="${path%% (ignore_errors=*}"
    fi
    [[ -n "$path" ]] || return 0
    printf '%s' "$path"
}

warn_missing_systemd_environment_files_from_text() {
    local unit="$1"
    local entry path
    while IFS= read -r entry || [[ -n "$entry" ]]; do
        path=$(update_systemd_envfile_check_path "$entry")
        [[ -n "$path" ]] || continue
        if [[ ! -f "$path" ]]; then
            printf '\033[1;31m[update] WARNING: EnvironmentFile %s is missing for unit %s; restart proceeds but secrets from this file will be absent\033[0m\n' "$path" "$unit" >&2
        fi
    done
}

warn_missing_systemd_environment_files() {
    local unit="$1"
    systemctl show -p EnvironmentFiles --value "$unit" 2>/dev/null \
        | warn_missing_systemd_environment_files_from_text "$unit"
}

update_unit_source_path() {
    local repo_root="${1%/}" unit="$2"
    [[ -n "$repo_root" && -n "$unit" ]] || { printf ''; return 0; }
    if [[ "$unit" != *.* ]]; then
        unit="${unit}.service"
    fi
    if [[ "$unit" != *.service ]]; then
        printf ''
        return 0
    fi
    local base="${unit%.service}"
    if [[ "$base" == "go-trader" ]]; then
        printf '%s/go-trader.service' "$repo_root"
        return 0
    fi
    if [[ "$base" == go-trader@* ]]; then
        local instance="${base#go-trader@}"
        if [[ "$(update_validate_instance_name "$instance")" == "ok" ]]; then
            printf '%s/systemd/go-trader@.service' "$repo_root"
            return 0
        fi
    fi
    printf ''
}

update_unit_fragment_scope() {
    local path="$1"
    [[ -n "$path" ]] || { printf ''; return 0; }
    if [[ "$path" != /* ]]; then
        printf 'other'
        return 0
    fi
    if [[ "${path%/*}" == "/etc/systemd/system" ]]; then
        printf 'etc'
    else
        printf 'other'
    fi
}

update_unit_sync_decision() {
    local installed="$1" source_path="$2" needs_reload="$3"
    [[ -n "$installed" && -n "$source_path" ]] || { printf 'none'; return 0; }
    [[ -f "$source_path" ]] || { printf 'none'; return 0; }
    if ! cmp -s "$installed" "$source_path"; then
        if [[ -L "$installed" ]]; then
            printf 'skip'
        else
            printf 'install'
        fi
        return 0
    fi
    case "$needs_reload" in
        yes|true) printf 'reload' ;;
        *) printf 'none' ;;
    esac
}

update_unit_sudo() {
    if [[ -n "${UPDATE_UNIT_SUDO+set}" ]]; then
        if [[ -z "$UPDATE_UNIT_SUDO" ]]; then
            "$@"
            return $?
        fi
        "$UPDATE_UNIT_SUDO" "$@"
        return $?
    fi
    sudo "$@"
}

update_unit_install_with_backup() {
    local installed="$1" source_path="$2"
    [[ -n "$installed" && -n "$source_path" && -f "$source_path" ]] || return 1
    local backup=""
    if [[ -f "$installed" ]]; then
        backup="${installed}.prev"
        update_unit_sudo cp -p "$installed" "$backup" || return 1
    fi
    if ! update_unit_sudo install -m 0644 "$source_path" "$installed"; then
        if [[ -n "$backup" ]]; then
            update_unit_sudo rm -f "$backup" || true
        fi
        return 1
    fi
    printf '%s' "$backup"
}

update_unit_restore_backup() {
    local installed="$1" backup="$2"
    [[ -n "$installed" && -n "$backup" && -f "$backup" ]] || return 1
    update_unit_sudo mv -f "$backup" "$installed"
}

UPDATE_JOURNAL_NAMESPACE_MIN_SYSTEMD=245

update_systemd_major_version() {
    local text="$1" first word ver
    first="${text%%$'\n'*}"
    read -r word ver _ <<<"$first" || true
    ver="${ver%%[!0-9]*}"
    if [[ "$word" != "systemd" || -z "$ver" ]]; then
        printf ''
        return 0
    fi
    printf '%s' "$ver"
}

update_journal_namespace_supported() {
    local major="$1"
    if [[ "$major" =~ ^[0-9]+$ ]] && (( 10#$major >= UPDATE_JOURNAL_NAMESPACE_MIN_SYSTEMD )); then
        printf 'yes'
        return 0
    fi
    printf 'no'
}

update_unit_log_namespace() {
    local unit_file="$1" raw
    [[ -n "$unit_file" && -f "$unit_file" ]] || { printf ''; return 0; }
    raw=$(sed -n 's/^[[:space:]]*LogNamespace[[:space:]]*=//p' "$unit_file" | tail -n 1)
    raw="${raw#"${raw%%[![:space:]]*}"}"
    raw="${raw%"${raw##*[![:space:]]}"}"
    printf '%s' "$raw"
}

update_journald_conf_path() {
    local etc_dir="${1%/}" namespace="$2"
    printf '%s/journald@%s.conf' "$etc_dir" "$namespace"
}

update_journalctl_unit_command() {
    local unit="$1" namespace="$2"
    if [[ -n "$namespace" ]]; then
        printf 'journalctl --namespace=+%s -u %s' "$namespace" "$unit"
        return 0
    fi
    printf 'journalctl -u %s' "$unit"
}

update_sync_journal_namespace() {
    local repo_root="${1%/}" unit_source="$2" etc_dir="${3:-/etc/systemd}"
    local namespace
    namespace=$(update_unit_log_namespace "$unit_source")
    if [[ -z "$namespace" ]]; then
        echo "[journal] $unit_source sets no LogNamespace; no journald namespace config to install"
        return 0
    fi
    if [[ ! "$namespace" =~ ^[A-Za-z0-9_-]+$ ]]; then
        echo "[journal] LogNamespace '$namespace' in $unit_source is not a plain name (letters, digits, dash, underscore); refusing to install a journald config for it" >&2
        return 1
    fi
    local major
    major=$(update_systemd_major_version "$(systemctl --version 2>/dev/null || true)")
    if [[ "$(update_journal_namespace_supported "$major")" != "yes" ]]; then
        printf '\033[1;31m[journal] WARNING: systemd %s is older than %s or unknown, and it does not support LogNamespace=. systemd ignores that line, so go-trader keeps logging to the default journal (and to syslog when journald forwards there). Skipping the journald@%s config.\033[0m\n' \
            "${major:-<unknown>}" "$UPDATE_JOURNAL_NAMESPACE_MIN_SYSTEMD" "$namespace" >&2
        return 0
    fi
    local source_conf="$repo_root/systemd/journald@${namespace}.conf"
    if [[ ! -f "$source_conf" ]]; then
        echo "[journal] $unit_source sets LogNamespace=$namespace but $source_conf is missing; refusing to run the unit in a namespace with no size cap" >&2
        return 1
    fi
    local dest dropin_dir decision backup
    dest=$(update_journald_conf_path "$etc_dir" "$namespace")
    dropin_dir="${dest}.d"
    decision=$(update_unit_sync_decision "$dest" "$source_conf" no)
    case "$decision" in
        install)
            backup=$(update_unit_install_with_backup "$dest" "$source_conf") || {
                echo "[journal] could not install $source_conf as $dest" >&2
                return 1
            }
            echo "[journal] installed $dest from $source_conf"
            if [[ -n "$backup" ]]; then
                echo "[journal] WARNING: $dest differed from the shipped file; the previous copy is kept as $backup. $dest is managed by go-trader: put local settings in $dropin_dir/*.conf" >&2
            fi
            if ! update_unit_sudo systemctl try-restart "systemd-journald@${namespace}.service"; then
                echo "[journal] systemd-journald@${namespace}.service did not restart with the new $dest; putting the previous config back" >&2
                if [[ -n "$backup" ]]; then
                    update_unit_restore_backup "$dest" "$backup" || true
                else
                    update_unit_sudo rm -f "$dest" || true
                fi
                update_unit_sudo systemctl try-restart "systemd-journald@${namespace}.service" || true
                return 1
            fi
            ;;
        skip)
            echo "[journal] $dest is a symlink whose target differs from $source_conf; leaving the operator's link alone" >&2
            ;;
        *)
            echo "[journal] $dest already matches $source_conf"
            ;;
    esac
    return 0
}

update_signal_redirect_decision() {
    local is_active="$1" exec_bin_abs="$2" swap_bin_abs="$3"
    [[ "$is_active" == "active" ]] || { printf ''; return 0; }
    [[ -n "$exec_bin_abs" && "$exec_bin_abs" == /* ]] || { printf ''; return 0; }
    [[ -n "$swap_bin_abs" ]] || { printf ''; return 0; }
    if [[ "$exec_bin_abs" == "$swap_bin_abs" ]]; then
        printf 'redirect'
        return 0
    fi
    printf ''
}

update_should_sweep_proc() {
    local comm="$1" pid_cwd="$2" repo_abs="$3"
    [[ "$comm" == "go-trader" ]] || { printf ''; return 0; }
    [[ -n "$repo_abs" && "$pid_cwd" == "$repo_abs" ]] || { printf ''; return 0; }
    printf 'sweep'
}

update_systemd_unit_globs() {
    printf '%s\n' 'go-trader.service' 'go-trader-*.service' 'go-trader@*.service'
}

normalize_systemd_deployment_dirs() {
    local line seen=$'\n'
    while IFS= read -r line || [[ -n "$line" ]]; do
        line="${line#"${line%%[![:space:]]*}"}"
        line="${line%"${line##*[![:space:]]}"}"
        [[ -n "$line" ]] || continue
        [[ "$line" == /* ]] || continue
        line="${line%/}/"
        case "$seen" in
            *$'\n'"$line"$'\n'*) continue ;;
        esac
        seen="${seen}${line}"$'\n'
        printf '%s\n' "$line"
    done
}

discover_enabled_inactive_feed_units() {
    command -v systemctl >/dev/null 2>&1 || return 0
    local -a globs=()
    local g
    while IFS= read -r g; do
        [[ -n "$g" ]] && globs+=("$g")
    done < <(update_systemd_unit_globs)
    local active=$'\n' unit seen=$'\n'
    while IFS= read -r unit; do
        [[ -n "$unit" ]] && active="${active}${unit}"$'\n'
    done < <(systemctl list-units --type=service --state=active --no-legend --plain "${globs[@]}" 2>/dev/null | awk '{print $1}')
    while IFS= read -r unit; do
        [[ -n "$unit" ]] || continue
        [[ "$unit" == *@.service ]] && continue
        case "$active" in *$'\n'"$unit"$'\n'*) continue ;; esac
        case "$seen" in *$'\n'"$unit"$'\n'*) continue ;; esac
        [[ "$(systemctl is-enabled "$unit" 2>/dev/null || true)" == "enabled" ]] || continue
        [[ "$(update_unit_role "$unit")" == "feed" ]] || continue
        seen="${seen}${unit}"$'\n'
        printf '%s\n' "$unit"
    done < <(
        systemctl list-units --type=service --all --no-legend --plain "${globs[@]}" 2>/dev/null | awk '{print $1}'
        systemctl list-unit-files --type=service --state=enabled --no-legend --plain "${globs[@]}" 2>/dev/null | awk '{print $1}'
    )
}

discover_deployment_dirs_from_systemd() {
    command -v systemctl >/dev/null 2>&1 || return 0
    local -a globs=()
    local g
    while IFS= read -r g; do
        [[ -n "$g" ]] && globs+=("$g")
    done < <(update_systemd_unit_globs)
    local -a units=()
    local unit
    while IFS= read -r unit; do
        [[ -n "$unit" ]] && units+=("$unit")
    done < <(systemctl list-units --type=service --state=active --no-legend --plain "${globs[@]}" 2>/dev/null | awk '{print $1}'; discover_enabled_inactive_feed_units)
    [[ ${#units[@]} -gt 0 ]] || return 0
    for unit in "${units[@]}"; do
        systemctl show "$unit" -p WorkingDirectory --value 2>/dev/null
    done | normalize_systemd_deployment_dirs
}

discover_deployment_unit_map() {
    command -v systemctl >/dev/null 2>&1 || return 0
    local -a globs=()
    local g
    while IFS= read -r g; do
        [[ -n "$g" ]] && globs+=("$g")
    done < <(update_systemd_unit_globs)
    local -a units=()
    local unit
    while IFS= read -r unit; do
        [[ -n "$unit" ]] && units+=("$unit")
    done < <(systemctl list-units --type=service --state=active --no-legend --plain "${globs[@]}" 2>/dev/null | awk '{print $1}'; discover_enabled_inactive_feed_units)
    [[ ${#units[@]} -gt 0 ]] || return 0
    local wd canon
    for unit in "${units[@]}"; do
        wd=$(systemctl show "$unit" -p WorkingDirectory --value 2>/dev/null)
        [[ -n "$wd" ]] || continue
        canon=$(canonicalize_deployment_dir "$wd")
        printf '%s|%s\n' "$canon" "$unit"
    done
}

update_convention_unit_for_dir() {
    local dir="${1%/}" base instance unit wd
    command -v systemctl >/dev/null 2>&1 || { printf ''; return 0; }
    base="${dir##*/}"
    [[ "$base" == go-trader-* ]] || { printf ''; return 0; }
    instance="${base#go-trader-}"
    [[ "$(update_validate_instance_name "$instance")" == "ok" ]] || { printf ''; return 0; }
    unit="go-trader@${instance}.service"
    if ! systemctl is-active --quiet "$unit" 2>/dev/null; then
        if [[ "$(systemctl is-enabled "$unit" 2>/dev/null || true)" != "enabled" || "$(update_unit_role "$unit")" != "feed" ]]; then
            printf ''
            return 0
        fi
    fi
    wd=$(systemctl show "$unit" -p WorkingDirectory --value 2>/dev/null || true)
    [[ -n "$wd" ]] || { printf ''; return 0; }
    if [[ "$(canonicalize_deployment_dir "$wd")" == "$(canonicalize_deployment_dir "$dir")" ]]; then
        printf '%s' "$unit"
        return 0
    fi
    printf ''
}

update_deployment_role() {
    update_deployment_role_for_config "${1%/}/scheduler/config.json"
}

update_unit_role() {
    local unit="$1" wd execstart cfg
    wd=$(systemctl show "$unit" -p WorkingDirectory --value 2>/dev/null || true)
    execstart=$(systemctl show "$unit" -p ExecStart --value 2>/dev/null | head -n 1 || true)
    cfg=$(update_execstart_config_path "$execstart")
    if [[ -z "$cfg" ]]; then
        [[ -n "$wd" ]] || { printf 'scheduler'; return 0; }
        cfg="${wd%/}/scheduler/config.json"
    elif [[ "$cfg" != /* ]]; then
        [[ -n "$wd" ]] || { printf 'scheduler'; return 0; }
        cfg="${wd%/}/$cfg"
    fi
    update_deployment_role_for_config "$cfg"
}

update_deployment_role_for_config() {
    local config="$1"
    [[ -f "$config" ]] || { printf 'scheduler'; return 0; }
    local role
    role=$(python3 -c '
import json, sys
try:
    cfg = json.load(open(sys.argv[1]))
    role = cfg.get("role") if isinstance(cfg, dict) else None
    print(role.strip() if isinstance(role, str) and role.strip() else "scheduler")
except Exception:
    print("scheduler")
' "$config" 2>/dev/null || true)
    [[ "$role" == "feed" ]] && { printf 'feed'; return 0; }
    printf 'scheduler'
}

order_deployments_feeds_first() {
    local -a feeds=() others=()
    local d
    while IFS= read -r d; do
        [[ -n "$d" ]] || continue
        if [[ "$(update_deployment_role "$d")" == "feed" ]]; then
            feeds+=("$d")
        else
            others+=("$d")
        fi
    done
    [[ ${#feeds[@]} -gt 0 ]] && printf '%s\n' "${feeds[@]}"
    [[ ${#others[@]} -gt 0 ]] && printf '%s\n' "${others[@]}"
    return 0
}

update_execstart_config_path() {
    local execstart="$1"
    if [[ "$execstart" =~ --config=([^[:space:]\;]+) ]]; then
        printf '%s' "${BASH_REMATCH[1]}"
        return 0
    fi
    if [[ "$execstart" =~ --config[[:space:]]+([^[:space:]\;]+) ]]; then
        printf '%s' "${BASH_REMATCH[1]}"
        return 0
    fi
    printf ''
}

canonicalize_deployment_dir() {
    local d="$1" phys
    if [[ -d "$d" ]] && phys=$(cd "$d" 2>/dev/null && pwd -P); then
        printf '%s/\n' "$phys"
    else
        printf '%s\n' "${d%/}/"
    fi
}

update_config_migration_state() {
    local path="$1"
    if [[ -L "$path" ]]; then
        printf 'symlink'
        return 0
    fi
    if [[ -e "$path" ]]; then
        printf 'regular'
        return 0
    fi
    printf 'missing'
}

update_validate_instance_name() {
    local name="$1"
    [[ -n "$name" ]] || { printf 'bad'; return 0; }
    case "$name" in
        .|..) printf 'bad'; return 0 ;;
        -*)   printf 'bad'; return 0 ;;
    esac
    if [[ "$name" =~ [^a-zA-Z0-9_.-] ]]; then
        printf 'bad'
        return 0
    fi
    printf 'ok'
}

update_config_writable_directive() {
    local base="$1" instance="$2" sub=""
    [[ -n "$instance" ]] && sub="/$instance"
    if [[ "$base" == /var/lib/* ]]; then
        printf 'StateDirectory=%s%s' "${base#/var/lib/}" "$sub"
    else
        printf 'ReadWritePaths=%s%s' "$base" "$sub"
    fi
}

update_db_rsync_excludes() {
    printf '%s\n' '*.db' '*.db-wal' '*.db-shm' '*.db.lock'
}

# Prints every configured state-file path, one per line: db_file first, then
# paper_db_file when the split live/paper layout is configured (#1523).
update_resolve_db_exclude() {
    local db_paths="scheduler/state.db"
    if [[ "$(update_deployment_role_for_config "${GO_TRADER_UPDATE_CONFIG:-scheduler/config.json}")" == "feed" ]]; then
        printf '%s\n' "$db_paths"
        return 0
    fi
    if [[ -f ${GO_TRADER_UPDATE_CONFIG:-scheduler/config.json} && -x "${GO_TRADER_UPDATE_PYTHON:-.venv/bin/python3}" ]]; then
        local custom
        custom=$("${GO_TRADER_UPDATE_PYTHON:-.venv/bin/python3}" -c '
import json, os
try:
    cfg = json.load(open(os.environ.get("GO_TRADER_UPDATE_CONFIG", "scheduler/config.json")))
    out = []
    p = cfg.get("db_file") or ""
    out.append(p.strip() if isinstance(p, str) and p.strip() else "scheduler/state.db")
    q = cfg.get("paper_db_file") or ""
    if isinstance(q, str) and q.strip():
        out.append(q.strip())
    for src in cfg.get("paper_sources") or []:
        if not isinstance(src, dict):
            continue
        s = src.get("db_file") or ""
        if isinstance(s, str) and s.strip():
            out.append(s.strip())
    print("\n".join(out))
except Exception:
    pass
' 2>/dev/null || true)
        if [[ -n "$custom" ]]; then
            db_paths="$custom"
        fi
    fi
    printf '%s\n' "$db_paths"
}

update_canonical_db_path() {
    python3 -c '
import os, sys
p = os.path.abspath(sys.argv[1])
try:
    os.stat(p)
    p = os.path.realpath(p)
except OSError:
    pass
print(p)
' "$1"
}

update_state_lock_paths() {
    local canon
    canon=$(update_canonical_db_path "$1")
    printf '%s\n' "${canon}.lock" "${canon}.manual-action.lock"
}

update_file_fingerprint() {
    local path="$1"
    if [[ ! -e "$path" ]]; then
        printf 'absent'
        return 0
    fi
    python3 -c 'import hashlib, sys; print(hashlib.sha256(open(sys.argv[1], "rb").read()).hexdigest())' "$path"
}

update_db_fingerprint() {
    local canon
    canon=$(update_canonical_db_path "$1")
    local wal="${canon}-wal" wal_fp
    if [[ -s "$wal" ]]; then
        wal_fp=$(update_file_fingerprint "$wal")
    else
        wal_fp="none"
    fi
    printf 'db=%s\nwal=%s\n' "$(update_file_fingerprint "$canon")" "$wal_fp"
}

update_resolve_config_db_path() {
    local deploy_dir="$1" db_path="$2"
    if [[ "$db_path" == /* ]]; then
        printf '%s' "$db_path"
    else
        printf '%s/%s' "${deploy_dir%/}" "$db_path"
    fi
}

update_unit_dropin_path() {
    local unit_dir="$1" unit="$2" name="$3"
    printf '%s/%s.d/%s.conf' "${unit_dir%/}" "$unit" "$name"
}

update_paper_override_directive() {
    local dir="${1%/}"
    if [[ "$dir" == /var/lib/*/* ]]; then
        update_config_writable_directive "${dir%/*}" "${dir##*/}"
    else
        update_config_writable_directive "$dir" ""
    fi
}

UPDATE_LOCK_HOLDER_PY='
import fcntl, os, sys
paths = sys.argv[1:]
held = []
warnings = []
def lock_owner(p):
    for suffix in (".manual-action.lock", ".lock"):
        if p.endswith(suffix):
            db = p[:-len(suffix)]
            break
    else:
        db = p
    for cand in (db, os.path.dirname(p)):
        try:
            st = os.stat(cand)
        except OSError:
            continue
        return st.st_uid, st.st_gid
    return None
for p in paths:
    existed = os.path.exists(p)
    fd = os.open(p, os.O_CREAT | os.O_RDWR, 0o644)
    if not existed:
        owner = lock_owner(p)
        if owner is not None and owner != (os.geteuid(), os.getegid()):
            try:
                os.fchown(fd, owner[0], owner[1])
            except OSError as exc:
                warnings.append("WARN %s created but could not be given to uid %d gid %d: %s" % (p, owner[0], owner[1], exc))
    try:
        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except OSError:
        pid = ""
        try:
            pid = os.read(fd, 32).decode("utf-8", "replace").strip()
        except OSError:
            pass
        print("CONTENDED %s pid=%s" % (p, pid or "unknown"))
        sys.stdout.flush()
        sys.exit(1)
    held.append((p, fd))
for p, fd in held:
    if p.endswith(".manual-action.lock"):
        continue
    os.ftruncate(fd, 0)
    os.lseek(fd, 0, os.SEEK_SET)
    os.write(fd, ("%d\n" % os.getpid()).encode())
    os.fsync(fd)
print("HELD %d" % os.getpid())
for w in warnings:
    print(w)
sys.stdout.flush()
sys.stdin.read()
'

update_start_state_lock_holder() {
    local -a locks=()
    local db
    for db in "$@"; do
        while IFS= read -r line; do
            locks+=("$line")
        done < <(update_state_lock_paths "$db")
    done
    local fifo out
    fifo=$(mktemp -u "${TMPDIR:-/tmp}/go-trader-lock-holder.XXXXXX")
    mkfifo "$fifo"
    out=$(mktemp "${TMPDIR:-/tmp}/go-trader-lock-holder-out.XXXXXX")
    python3 -c "$UPDATE_LOCK_HOLDER_PY" "${locks[@]}" <"$fifo" >"$out" 2>&1 &
    UPDATE_LOCK_HOLDER_PID=$!
    exec {UPDATE_LOCK_HOLDER_FD}>"$fifo"
    rm -f "$fifo"
    local i status=""
    for i in $(seq 1 200); do
        status=$(head -n 1 "$out" 2>/dev/null || true)
        [[ -n "$status" ]] && break
        sleep 0.05
    done
    if [[ "$status" != HELD* ]]; then
        exec {UPDATE_LOCK_HOLDER_FD}>&-
        wait "$UPDATE_LOCK_HOLDER_PID" 2>/dev/null || true
        cat "$out" >&2
        rm -f "$out"
        unset UPDATE_LOCK_HOLDER_PID UPDATE_LOCK_HOLDER_FD
        return 1
    fi
    tail -n +2 "$out" >&2
    rm -f "$out"
}

update_stop_state_lock_holder() {
    if [[ -n "${UPDATE_LOCK_HOLDER_FD:-}" ]]; then
        exec {UPDATE_LOCK_HOLDER_FD}>&-
    fi
    if [[ -n "${UPDATE_LOCK_HOLDER_PID:-}" ]]; then
        wait "$UPDATE_LOCK_HOLDER_PID" 2>/dev/null || true
    fi
    unset UPDATE_LOCK_HOLDER_PID UPDATE_LOCK_HOLDER_FD
}

strip_unit_flags_from_argv() {
    declare -a out=()
    local skip_next=0
    local a
    for a in "$@"; do
        if [[ "$skip_next" == "1" ]]; then
            skip_next=0
            continue
        fi
        case "$a" in
            --unit|--service)
                skip_next=1
                continue
                ;;
            --unit=*|--service=*)
                continue
                ;;
        esac
        out+=("$a")
    done
    printf '%s\n' "${out[@]}"
}

resolve_child_unit_override() {
    local parent_service_unit="$1"
    local mapped_unit="$2"
    shift 2
    if [[ -n "$mapped_unit" ]]; then
        printf '%s\n' "$mapped_unit"
        strip_unit_flags_from_argv "$@"
    else
        printf '%s\n' "$parent_service_unit"
        printf '%s\n' "$@"
    fi
}

UPDATE_SYSTEM_PATH="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

update_tool_fixed_paths() {
    case "$1" in
        go) printf '%s\n' /opt/homebrew/bin/go /usr/local/go/bin/go ;;
        uv) printf '%s\n' /usr/local/bin/uv /usr/bin/uv /opt/homebrew/bin/uv ;;
    esac
}

update_resolve_tool() {
    local name="$1" p
    p=$(command -v "$name" 2>/dev/null || true)
    if [[ "$p" == /* && -f "$p" && -x "$p" ]]; then
        printf '%s' "$p"
        return 0
    fi
    while IFS= read -r p; do
        if [[ -n "$p" && -f "$p" && -x "$p" ]]; then
            printf '%s' "$p"
            return 0
        fi
    done <<<"$(update_tool_fixed_paths "$name")"
    return 1
}

update_tool_fixed_text() {
    local name="$1" p list=""
    while IFS= read -r p; do
        [[ -n "$p" ]] && list="${list:+$list, }$p"
    done <<<"$(update_tool_fixed_paths "$name")"
    printf '%s' "$list"
}

update_tool_version_arg() {
    case "$1" in
        go) printf 'version' ;;
        *) printf -- '--version' ;;
    esac
}

update_current_account() {
    id -un 2>/dev/null || printf 'uid-%s' "$EUID"
}

update_tool_runs_as() {
    local account="$1" tool="$2" arg p out rc=0
    arg=$(update_tool_version_arg "$tool")
    if [[ "$account" == "$(update_current_account)" ]]; then
        p=$(update_resolve_tool "$tool") || return 1
        printf '%s' "$p"
        "$p" "$arg" >/dev/null 2>&1 || return 2
        return 0
    fi
    command -v runuser >/dev/null 2>&1 || return 3
    out=$(runuser -u "$account" -- /usr/bin/env -i PATH="$UPDATE_SYSTEM_PATH" "$BASH" -c \
        "cd / 2>/dev/null; $(declare -f update_tool_fixed_paths update_resolve_tool)"'
p=$(update_resolve_tool "$1") || exit 11
printf "%s" "$p"
"$p" "$2" >/dev/null 2>&1 || exit 12
exit 0' _ "$tool" "$arg" 2>/dev/null) || rc=$?
    printf '%s' "$out"
    case "$rc" in
        0) return 0 ;;
        11) return 1 ;;
        12) return 2 ;;
        *) return 4 ;;
    esac
}

update_resolve_tool_system() {
    /usr/bin/env -i PATH="$UPDATE_SYSTEM_PATH" "$BASH" -c \
        "$(declare -f update_tool_fixed_paths update_resolve_tool)"'
update_resolve_tool "$1"' _ "$1"
}

update_build_tools_preflight() {
    local tool account p rc bad_uv=0 bad_go=0 me searched
    me=$(update_current_account)
    for tool in uv go; do
        for account in "$@"; do
            [[ -n "$account" ]] || continue
            if [[ "$account" == "$me" ]]; then
                searched="PATH=$PATH"
            else
                searched="PATH=$UPDATE_SYSTEM_PATH"
            fi
            rc=0
            p=$(update_tool_runs_as "$account" "$tool") || rc=$?
            case "$rc" in
                0) echo "[tools] $tool: $account runs $p" ;;
                1) echo "[tools] $tool: $account finds no $tool (searched $searched, then $(update_tool_fixed_text "$tool"))" ;;
                2) echo "[tools] $tool: $account finds $p but cannot run it" ;;
                3) echo "[tools] $tool: cannot check $account (runuser is not installed)" ;;
                *) echo "[tools] $tool: the check as $account did not run (does the account exist?)" ;;
            esac
            if [[ "$rc" != 0 ]]; then
                [[ "$tool" == uv ]] && bad_uv=1
                [[ "$tool" == go ]] && bad_go=1
            fi
        done
    done
    [[ "$bad_uv" == 0 ]] || echo "[tools] $(update_build_tools_fix uv)"
    [[ "$bad_go" == 0 ]] || echo "[tools] $(update_build_tools_fix go)"
    [[ "$bad_uv" == 0 && "$bad_go" == 0 ]]
}

update_build_tools_fix() {
    case "$1" in
        uv) printf '%s' "fix: install uv where every account finds it: curl -LsSf https://astral.sh/uv/install.sh | sudo env UV_INSTALL_DIR=/usr/local/bin UV_NO_MODIFY_PATH=1 sh (a copy in one account's home directory, such as /root/.local/bin, does not count)" ;;
        go) printf '%s' "fix: install Go under /usr/local/go as SKILL.md Prerequisites shows, so every account finds /usr/local/go/bin/go" ;;
    esac
}

update_path_uid() {
    stat -c '%u' "$1" 2>/dev/null || stat -f '%u' "$1" 2>/dev/null
}

update_path_gid() {
    stat -c '%g' "$1" 2>/dev/null || stat -f '%g' "$1" 2>/dev/null
}

update_path_owner_name() {
    local name
    name=$(stat -c '%U' "$1" 2>/dev/null || stat -f '%Su' "$1" 2>/dev/null || true)
    printf '%s' "${name:-unknown}"
}

update_path_group_name() {
    local name
    name=$(stat -c '%G' "$1" 2>/dev/null || stat -f '%Sg' "$1" 2>/dev/null || true)
    printf '%s' "${name:-unknown}"
}

update_git_top() {
    local d
    d=$(cd "$1" 2>/dev/null && pwd -P) || return 1
    while true; do
        if [[ -e "${d%/}/.git" ]]; then
            printf '%s' "$d"
            return 0
        fi
        [[ "$d" != "/" ]] || return 1
        d="${d%/*}"
        [[ -n "$d" ]] || d="/"
    done
}

update_git_trust_decision() {
    local caller="$1" owner
    shift
    [[ "$caller" == "0" ]] || return 0
    for owner in "$@"; do
        if [[ -n "$owner" && "$owner" != "0" ]]; then
            printf 'trust'
            return 0
        fi
    done
}

update_git_trust_env() {
    local top="$1" caller="$2"
    shift 2
    [[ -n "$top" && "$(update_git_trust_decision "$caller" "$@")" == "trust" ]] || return 0
    local n="${GIT_CONFIG_COUNT:-0}"
    [[ "$n" =~ ^[0-9]+$ ]] || n=0
    printf '%s\n' \
        "GIT_CONFIG_KEY_$n=safe.directory" "GIT_CONFIG_VALUE_$n=$top" \
        "GIT_CONFIG_KEY_$((n + 1))=core.fsmonitor" "GIT_CONFIG_VALUE_$((n + 1))=false" \
        "GIT_CONFIG_KEY_$((n + 2))=core.hooksPath" "GIT_CONFIG_VALUE_$((n + 2))=/dev/null" \
        "GIT_CONFIG_COUNT=$((n + 3))"
}

update_git_env_for() {
    local top
    [[ "$EUID" == "0" ]] || return 0
    top=$(update_git_top "$1") || return 0
    update_git_trust_env "$top" "$EUID" "$(update_path_uid "$top")" "$(update_path_uid "$top/.git")"
}

update_git_failure_note() {
    local tree="$1" rc="$2" err="$3" sub="$4" top owner me first
    top=$(update_git_top "$tree" || true)
    owner=$(update_path_owner_name "${top:-$tree}")
    me=$(update_current_account)
    first="${err%%$'\n'*}"
    if [[ "$err" == *"dubious ownership"* ]]; then
        if [[ "$EUID" == "0" ]]; then
            printf '[git] git refused %s: owned by %s, running as %s, and this git ignored the per-command safe.directory for that tree (%s). Upgrade git, or run the command as %s.\n' \
                "${top:-$tree}" "$owner" "$me" "$first" "$owner"
        else
            printf '[git] git refused %s: owned by %s, running as %s (%s). Run the script as root, which trusts only this tree for each git command, or as %s.\n' \
                "${top:-$tree}" "$owner" "$me" "$first" "$owner"
        fi
        return 0
    fi
    printf '[git] git %s failed in %s (exit %s; tree owner %s, running as %s): %s\n' \
        "$sub" "$tree" "$rc" "$owner" "$me" "$first"
}

update_git() {
    local tree="$1"
    shift
    local line errtxt outfd rc=0 sub=""
    local -a trust=()
    while IFS= read -r line; do
        [[ -n "$line" ]] && trust+=("$line")
    done <<<"$(update_git_env_for "$tree")"
    for line in "$@"; do
        [[ "$line" == -* ]] && continue
        sub="$line"
        break
    done
    exec {outfd}>&1
    if [[ ${#trust[@]} -gt 0 ]]; then
        errtxt=$(/usr/bin/env "${trust[@]}" git -C "$tree" "$@" 2>&1 1>&"$outfd") || rc=$?
    else
        errtxt=$(git -C "$tree" "$@" 2>&1 1>&"$outfd") || rc=$?
    fi
    exec {outfd}>&-
    [[ -z "$errtxt" ]] || printf '%s\n' "$errtxt" >&2
    if [[ "$rc" != 0 && -n "$errtxt" ]]; then
        update_git_failure_note "$tree" "$rc" "$errtxt" "$sub" >&2
    fi
    return "$rc"
}

update_git_version() {
    local tree="$1" base dirty
    base=$(update_git "$tree" describe --tags --always) || return 1
    [[ -n "$base" ]] || return 1
    dirty=$(update_git "$tree" --no-optional-locks status --porcelain --untracked-files=no) || return 1
    if [[ -n "$dirty" ]]; then
        base="${base}-mod"
    fi
    printf '%s' "$base"
}

update_tree_foreign_owner() {
    [[ "$EUID" == "0" ]] || return 0
    local uid gid
    uid=$(update_path_uid "$1")
    gid=$(update_path_gid "$1")
    [[ -n "$uid" && -n "$gid" && "$uid" != "0" ]] || return 0
    printf '%s:%s' "$uid" "$gid"
}

UPDATE_OWNER_PY='
import os, sys
mode, tree, snap = sys.argv[1], sys.argv[2], sys.argv[3]
top = os.lstat(tree)
def root_owned():
    found = []
    if top.st_uid == 0:
        found.append(tree)
    for root, dirs, files in os.walk(tree):
        keep = []
        for name in dirs + files:
            path = os.path.join(root, name)
            try:
                st = os.lstat(path)
            except OSError:
                continue
            if st.st_dev != top.st_dev:
                continue
            if st.st_uid == 0:
                found.append(path)
            if name in dirs and not os.path.islink(path):
                keep.append(name)
        dirs[:] = [d for d in dirs if d in keep]
    return found
if mode == "snapshot":
    with open(snap, "wb") as f:
        f.write(b"\0".join(os.fsencode(p) for p in root_owned()))
    sys.exit(0)
uid, gid = (int(x) for x in sys.argv[4].split(":"))
with open(snap, "rb") as f:
    raw = f.read()
before = set(os.fsdecode(p) for p in raw.split(b"\0") if p)
now = root_owned()
new = []
for p in now:
    if p in before:
        continue
    try:
        os.lchown(p, uid, gid)
    except FileNotFoundError:
        continue
    new.append(p)
left = []
for p in new:
    try:
        if os.lstat(p).st_uid == 0:
            left.append(p)
    except FileNotFoundError:
        pass
if left:
    print("FAILED %d %s" % (len(left), left[0]))
    sys.exit(1)
old = [p for p in now if p in before]
print("GAVE %d" % len(new))
print("KEPT %d %s" % (len(old), " ".join(old[:5])))
'

update_owner_snapshot() {
    python3 -c "$UPDATE_OWNER_PY" snapshot "$1" "$2"
}

update_restore_tree_owner() {
    local tree="$1" owner="$2" snap="$3" out rc=0 line gave=0 kept=0 examples=""
    [[ -n "$tree" && -n "$owner" && -n "$snap" && -f "$snap" ]] || return 0
    out=$(python3 -c "$UPDATE_OWNER_PY" restore "$tree" "$snap" "$owner") || rc=$?
    while IFS= read -r line; do
        case "$line" in
            GAVE\ *) gave="${line#GAVE }" ;;
            KEPT\ *) line="${line#KEPT }"; kept="${line%% *}"; examples="${line#"$kept"}" ;;
            FAILED\ *) echo "[update] ownership: could not give ${line#FAILED } back to $owner" >&2 ;;
        esac
    done <<<"$out"
    [[ "$rc" == 0 ]] || return 1
    if [[ "$gave" != 0 ]]; then
        echo "[update] ownership: $gave path(s) this update wrote as root under $tree given back to $(update_path_owner_name "$tree")"
    fi
    if [[ "$kept" != 0 && "${UPDATE_OWNER_KEPT_WARNED:-}" != "$tree" ]]; then
        UPDATE_OWNER_KEPT_WARNED="$tree"
        echo "[update] warning: $kept path(s) under $tree were owned by root before this update and stay so (for example${examples}); if the tree's owner should own them, run: chown -R $(update_path_owner_name "$tree"):$(update_path_group_name "$tree") $tree" >&2
    fi
    return 0
}

update_owner_runs_venv() {
    local tree="$1" name py
    py="${tree%/}/.venv/bin/python3"
    [[ -e "$py" ]] || return 0
    name=$(update_path_owner_name "$tree")
    if [[ "$name" == "unknown" || "$name" == "UNKNOWN" ]]; then
        echo "[update] warning: $tree has no named owner; skipping the venv check as the owner" >&2
        return 0
    fi
    if ! command -v runuser >/dev/null 2>&1; then
        echo "[update] warning: runuser is not installed; skipping the venv check as $name" >&2
        return 0
    fi
    runuser -u "$name" -- "$py" -c 'import encodings, sqlite3' >/dev/null 2>&1
}

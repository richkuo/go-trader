
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
    done < <(systemctl list-units --type=service --state=active --no-legend --plain "${globs[@]}" 2>/dev/null | awk '{print $1}')
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
    done < <(systemctl list-units --type=service --state=active --no-legend --plain "${globs[@]}" 2>/dev/null | awk '{print $1}')
    [[ ${#units[@]} -gt 0 ]] || return 0
    local wd canon
    for unit in "${units[@]}"; do
        wd=$(systemctl show "$unit" -p WorkingDirectory --value 2>/dev/null)
        [[ -n "$wd" ]] || continue
        canon=$(canonicalize_deployment_dir "$wd")
        printf '%s|%s\n' "$canon" "$unit"
    done
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

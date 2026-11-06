
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
    local installed="$1" source_path="$2" record_file="${3:-}"
    [[ -n "$installed" && -n "$source_path" && -f "$source_path" ]] || return 1
    if [[ -n "$record_file" ]]; then
        printf 'unit_installed_path=%s\n' "$installed" >"$record_file"
    fi
    local backup=""
    if [[ -f "$installed" ]]; then
        backup="${installed}.prev"
        update_unit_sudo cp -p "$installed" "$backup" || return 1
    fi
    if ! update_unit_sudo install -m 0644 "$source_path" "$installed"; then
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
    local repo_root="${1%/}" unit_source="$2" etc_dir="${3:-/etc/systemd}" record_file="${4:-}"
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
            if [[ -n "$record_file" ]]; then
                printf 'journal_dest=%s\njournal_namespace=%s\n' "$dest" "$namespace" >"$record_file"
            fi
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

UPDATE_CPU_DROPIN_NAME=40-go-trader-cpu
UPDATE_CPU_WEIGHT_ENV=GO_TRADER_PAPER_CPU_WEIGHT
UPDATE_CPU_WEIGHT_DEFAULT=20
UPDATE_CPU_WEIGHT_MAX=10000
UPDATE_CPU_WEIGHT_MIN_SYSTEMD=232

update_cpu_template_unit() {
    local unit="$1" instance
    [[ -n "$unit" ]] || { printf ''; return 0; }
    [[ "$unit" == *.* ]] || unit="${unit}.service"
    [[ "$unit" == go-trader@*.service ]] || { printf ''; return 0; }
    instance="${unit#go-trader@}"
    instance="${instance%.service}"
    [[ "$(update_validate_instance_name "$instance")" == "ok" ]] || { printf ''; return 0; }
    printf '%s' "$unit"
}

update_cpu_dropin_path() {
    local unit
    unit=$(update_cpu_template_unit "$1")
    [[ -n "$unit" ]] || { printf ''; return 0; }
    update_unit_dropin_path "${UPDATE_SYSTEMD_UNIT_DIR:-/etc/systemd/system}" "$unit" "$UPDATE_CPU_DROPIN_NAME"
}

update_cpu_dropin_content() {
    printf '[Service]\nCPUWeight=%s\n' "$1"
}

update_cpu_dropin_is_managed() {
    local path="$1" body weight
    [[ -f "$path" && ! -L "$path" ]] || return 1
    body=$(cat "$path"; printf x) || return 1
    body="${body%x}"
    [[ "$body" =~ ^\[Service\]$'\n'CPUWeight=([1-9][0-9]{0,4})$'\n'$ ]] || return 1
    weight="${BASH_REMATCH[1]}"
    (( 10#$weight <= UPDATE_CPU_WEIGHT_MAX ))
}

update_cpu_weight_value() {
    local raw="$1" is_set="$2"
    raw="${raw#"${raw%%[![:space:]]*}"}"
    raw="${raw%"${raw##*[![:space:]]}"}"
    if [[ "$is_set" != "1" || -z "$raw" ]]; then
        printf '%s' "$UPDATE_CPU_WEIGHT_DEFAULT"
        return 0
    fi
    if [[ "$raw" =~ ^[0-9]{1,5}$ ]] && (( 10#$raw <= UPDATE_CPU_WEIGHT_MAX )); then
        printf '%s' "$((10#$raw))"
        return 0
    fi
    echo "[cpu] WARNING: $UPDATE_CPU_WEIGHT_ENV='$raw' is not an integer from 0 to $UPDATE_CPU_WEIGHT_MAX; using the default CPU weight $UPDATE_CPU_WEIGHT_DEFAULT" >&2
    printf '%s' "$UPDATE_CPU_WEIGHT_DEFAULT"
}

update_cpu_weight_for_unit() {
    local unit="$1" env_text files_text out
    env_text=$(systemctl show -p Environment --value "$unit" 2>/dev/null || true)
    files_text=$(systemctl show -p EnvironmentFiles --value "$unit" 2>/dev/null || true)
    out=$(update_unit_env_value_from_show "$UPDATE_CPU_WEIGHT_ENV" "$env_text" "$files_text" strict) || {
        echo "[cpu] WARNING: could not read the environment files of $unit, so an opt-out ($UPDATE_CPU_WEIGHT_ENV=0) cannot be ruled out; the managed CPU weight drop-in stays absent (run the update as root)" >&2
        return 1
    }
    if [[ "$out" == SET\ * ]]; then
        update_cpu_weight_value "${out#SET }" 1
    else
        update_cpu_weight_value "" 0
    fi
}

update_cpu_desired_weight() {
    local class="$1" weight="$2" major="$3"
    [[ "$class" == "paper" ]] || { printf ''; return 0; }
    [[ "$weight" =~ ^[0-9]+$ ]] && (( 10#$weight > 0 )) || { printf ''; return 0; }
    [[ "$major" =~ ^[0-9]+$ ]] && (( 10#$major >= UPDATE_CPU_WEIGHT_MIN_SYSTEMD )) || { printf ''; return 0; }
    printf '%s' "$((10#$weight))"
}

update_cpu_dropin_decision() {
    local dest="$1" weight="$2"
    [[ -n "$dest" ]] || { printf 'none'; return 0; }
    if [[ -L "$dest" ]]; then
        printf 'skip-symlink'
        return 0
    fi
    if [[ -z "$weight" ]]; then
        if [[ -e "$dest" ]]; then
            printf 'remove'
        else
            printf 'none'
        fi
        return 0
    fi
    if [[ ! -e "$dest" ]]; then
        printf 'install'
        return 0
    fi
    if cmp -s "$dest" <(update_cpu_dropin_content "$weight"); then
        printf 'none'
    else
        printf 'replace'
    fi
}

update_cpu_warn_later_dropins() {
    local dest="$1" unit_dir name d f base
    unit_dir="${dest%/*}"
    unit_dir="${unit_dir%/*}"
    name="${dest##*/}"
    for d in "${dest%/*}" "$unit_dir/go-trader@.service.d"; do
        [[ -d "$d" ]] || continue
        for f in "$d"/*.conf; do
            [[ -f "$f" ]] || continue
            base="${f##*/}"
            [[ "$f" != "$dest" ]] || continue
            [[ "$base" > "$name" ]] || continue
            if grep -Eq '^[[:space:]]*CPUWeight[[:space:]]*=' "$f" 2>/dev/null; then
                echo "[cpu] WARNING: $f sorts after $name and sets CPUWeight=, so systemd uses its value instead of the managed one" >&2
            fi
        done
    done
}

update_cpu_dropin_apply() {
    local decision="$1" dest="$2" weight="$3" dir tmp staged
    dir="${dest%/*}"
    case "$decision" in
        install|replace)
            [[ "$weight" =~ ^[1-9][0-9]*$ ]] || return 1
            tmp=$(mktemp) || return 1
            update_cpu_dropin_content "$weight" >"$tmp" || { rm -f "$tmp"; return 1; }
            staged="${dest}.tmp-$$"
            if ! update_unit_sudo install -d -m 0755 "$dir" \
                || ! update_unit_sudo install -m 0644 "$tmp" "$staged"; then
                rm -f "$tmp"
                update_unit_sudo rm -f "$staged" 2>/dev/null || true
                return 1
            fi
            rm -f "$tmp"
            if ! update_unit_sudo mv -f "$staged" "$dest"; then
                update_unit_sudo rm -f "$staged" 2>/dev/null || true
                return 1
            fi
            update_cpu_warn_later_dropins "$dest"
            ;;
        remove)
            update_unit_sudo rm -f "$dest" || return 1
            update_unit_sudo rmdir "$dir" 2>/dev/null || true
            ;;
        *)
            ;;
    esac
    return 0
}

update_cpu_class_for_unit() {
    local unit="$1" binary="$2" owner cfg wd out rc=0 errf
    if [[ ! -x "$binary" ]]; then
        echo "[cpu] $binary is not an executable go-trader binary" >&2
        return 1
    fi
    cfg=$(update_unit_config_path "$unit")
    if [[ -z "$cfg" ]]; then
        echo "[cpu] $unit names no config file and has no WorkingDirectory" >&2
        return 1
    fi
    owner=$(update_unit_owner "$unit") || {
        echo "[cpu] could not resolve the User and Group of $unit" >&2
        return 1
    }
    if [[ "${owner%%:*}" == "0" ]]; then
        echo "[cpu] $unit runs as root; the classifier never runs as root, so the CPU weight drop-in stays absent" >&2
        return 1
    fi
    if ! command -v systemd-run >/dev/null 2>&1; then
        echo "[cpu] systemd-run is not installed, so the classifier cannot run as the owner of $unit" >&2
        return 1
    fi
    wd=$(systemctl show -p WorkingDirectory --value "$unit" 2>/dev/null || true)
    local -a args=(--wait --pipe --collect --quiet --unit "go-trader-cpu-class-$$-$RANDOM"
        "--uid=${owner%%:*}" "--gid=${owner#*:}"
        -p ProtectSystem=strict -p PrivateTmp=true -p NoNewPrivileges=true -p RuntimeMaxSec=60)
    [[ -n "$wd" ]] && args+=(-p "WorkingDirectory=$wd")
    errf=$(mktemp) || return 1
    out=$(update_unit_sudo systemd-run "${args[@]}" -- "$binary" cpu-class --config "$cfg" </dev/null 2>"$errf") || rc=$?
    if [[ "$rc" != "0" ]]; then
        echo "[cpu] $binary cpu-class --config $cfg failed (exit $rc):" >&2
        sed 's/^/[cpu]   /' "$errf" >&2
        rm -f "$errf"
        return 1
    fi
    rm -f "$errf"
    case "$out" in
        paper|live|feed)
            printf '%s' "$out"
            return 0
            ;;
    esac
    echo "[cpu] $binary cpu-class --config $cfg printed an unexpected result: '$out'" >&2
    return 1
}

update_cpu_dropin_plan() {
    local unit="$1" binary="$2" dest="$3" class="" weight="" major desired decision
    if class=$(update_cpu_class_for_unit "$unit" "$binary"); then
        echo "[cpu] $unit config class: $class" >&2
    else
        class=""
        echo "[cpu] $unit has no CPU class; the managed CPU weight drop-in stays absent" >&2
    fi
    if [[ "$class" == "paper" ]] && ! weight=$(update_cpu_weight_for_unit "$unit"); then
        desired=""
    elif [[ "$class" == "paper" ]]; then
        major=$(update_systemd_major_version "$(systemctl --version 2>/dev/null || true)")
        desired=$(update_cpu_desired_weight "$class" "$weight" "$major")
        if [[ "$weight" == "0" ]]; then
            echo "[cpu] $UPDATE_CPU_WEIGHT_ENV=0 turns the paper CPU weight off for $unit" >&2
        elif [[ -z "$desired" ]]; then
            echo "[cpu] systemd ${major:-<unknown>} is older than $UPDATE_CPU_WEIGHT_MIN_SYSTEMD or unknown; no CPUWeight drop-in for $unit" >&2
        fi
    else
        desired=""
    fi
    decision=$(update_cpu_dropin_decision "$dest" "$desired")
    printf '%s %s' "$decision" "$desired"
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

update_resolve_from() {
    local base="$1" path="$2"
    if [[ -z "$path" || "$path" == /* ]]; then
        printf '%s' "$path"
        return 0
    fi
    while [[ "$path" == ./* ]]; do
        path="${path#./}"
    done
    printf '%s/%s' "${base%/}" "$path"
}

update_signal_prereq_status() {
    local base="$1" proc pidfile run_sh
    proc=$(update_resolve_from "$base" "$2")
    pidfile=$(update_resolve_from "$base" "$3")
    run_sh=$(update_resolve_from "$base" "$4")
    if [[ ! -d "$proc/self" ]]; then
        printf 'missing-proc'
    elif [[ ! -f "$pidfile" ]]; then
        printf 'missing-pidfile'
    elif [[ ! -f "$run_sh" ]]; then
        printf 'missing-wrapper'
    elif [[ ! -x "$run_sh" ]]; then
        printf 'wrapper-not-executable'
    else
        printf 'ok'
    fi
    return 0
}

update_signal_redirect_probe() {
    local unit="$1" root="$2" active_state exec_out exec_rc=0 exec_line binary bin_abs="" swap_res
    command -v systemctl >/dev/null 2>&1 || { printf 'unknown'; return 0; }
    active_state=$(systemctl is-active "$unit" 2>/dev/null || true)
    exec_out=$(systemctl show -p ExecStart --value "$unit" 2>/dev/null) || exec_rc=$?
    exec_line="${exec_out%%$'\n'*}"
    binary=$(execstart_main_binary "$exec_line")
    if [[ "$binary" == /* ]]; then
        bin_abs=$(update_canonicalize_path "$binary")
    fi
    swap_res=$(update_canonicalize_path "${root%/}/go-trader")
    if [[ "$(update_signal_redirect_decision "$active_state" "$bin_abs" "$swap_res")" == "redirect" ]]; then
        printf 'redirect'
        return 0
    fi
    case "$active_state" in
        inactive|failed)
            printf 'none'
            return 0
            ;;
        active)
            if [[ "$exec_rc" -eq 0 && "$bin_abs" == /* && -n "$swap_res" ]]; then
                printf 'none'
                return 0
            fi
            ;;
    esac
    printf 'unknown'
    return 0
}

update_path_tree_scope() {
    local root="${1%/}" path="$2" root_real="" dir dir_real path_real
    if [[ -z "$root" || "$path" != /* ]]; then
        printf 'unknown'
        return 0
    fi
    case "$path/" in
        *"/../"*)
            printf 'inside'
            return 0
            ;;
    esac
    if update_path_within "$path" "$root"; then
        printf 'inside'
        return 0
    fi
    if ! root_real=$(update_realpath "$root" 2>/dev/null) || [[ -z "$root_real" ]]; then
        printf 'unknown'
        return 0
    fi
    if update_path_within "$path" "$root_real"; then
        printf 'inside'
        return 0
    fi
    dir="${path%/*}"
    [[ -n "$dir" ]] || dir="/"
    if [[ ! -d "$dir" ]] || ! dir_real=$(cd "$dir" 2>/dev/null && pwd -P) || [[ -z "$dir_real" ]]; then
        printf 'unknown'
        return 0
    fi
    if update_path_within "$dir_real" "$root" || update_path_within "$dir_real" "$root_real"; then
        printf 'inside'
        return 0
    fi
    if ! path_real=$(update_realpath "$path" 2>/dev/null) || [[ -z "$path_real" ]]; then
        printf 'unknown'
        return 0
    fi
    if update_path_within "$path_real" "$root" || update_path_within "$path_real" "$root_real"; then
        printf 'inside'
        return 0
    fi
    printf 'outside'
    return 0
}

update_restart_owner_emit() {
    printf '%s\n%s\n%s\n%s\n' "$1" "$2" "$3" "$4"
}

update_restart_owner_decision() {
    local root="${1%/}" unit="$2" mode="$3" pidfile="$4" run_sh="$5" proc="$6"
    local route="$mode" load_state="-" redirect ls_out ls_rc=0 frag_out frag_rc=0 status scope
    local pid_abs run_abs proc_abs
    pid_abs=$(update_resolve_from "$root" "$pidfile")
    run_abs=$(update_resolve_from "$root" "$run_sh")
    proc_abs=$(update_resolve_from "$root" "$proc")
    if ! command -v systemctl >/dev/null 2>&1; then
        update_restart_owner_emit unknown "$mode" - "systemctl is not available, so the restart route of $root cannot be proven"
        return 0
    fi
    if [[ "$mode" == "signal" ]]; then
        redirect=$(update_signal_redirect_probe "$unit" "$root")
        case "$redirect" in
            redirect) route="systemd-redirect" ;;
            none) route="signal" ;;
            *)
                update_restart_owner_emit unknown signal - "could not prove whether systemd unit $unit runs $root/go-trader"
                return 0
                ;;
        esac
    fi
    if [[ "$route" != "signal" ]]; then
        ls_out=$(systemctl show -p LoadState --value "$unit" 2>/dev/null) || ls_rc=$?
        if [[ "$ls_rc" -ne 0 || ! "$ls_out" =~ ^[a-z][a-z-]*$ ]]; then
            update_restart_owner_emit unknown "$route" - "systemctl show -p LoadState for $unit failed or printed an unreadable value"
            return 0
        fi
        load_state="$ls_out"
        case "$load_state" in
            loaded|masked)
                update_restart_owner_emit owned "$route" "$load_state" "systemd unit $unit is $load_state"
                return 0
                ;;
            not-found) ;;
            *)
                update_restart_owner_emit unknown "$route" "$load_state" "systemd unit $unit has LoadState=$load_state"
                return 0
                ;;
        esac
        frag_out=$(systemctl show -p FragmentPath --value "$unit" 2>/dev/null) || frag_rc=$?
        frag_out="${frag_out#"${frag_out%%[![:space:]]*}"}"
        frag_out="${frag_out%"${frag_out##*[![:space:]]}"}"
        if [[ "$frag_rc" -ne 0 || -n "$frag_out" ]]; then
            update_restart_owner_emit unknown "$route" "$load_state" "systemd unit $unit is not-found but its fragment path could not be proven empty"
            return 0
        fi
        route="signal-fallback"
    fi
    status=$(update_signal_prereq_status "$root" "$proc" "$pidfile" "$run_sh")
    case "$status" in
        ok)
            update_restart_owner_emit owned "$route" "$load_state" "signal restart has $proc_abs/self, pidfile $pid_abs and executable wrapper $run_abs"
            ;;
        missing-proc)
            update_restart_owner_emit no-owner "$route" "$load_state" "signal restart needs $proc_abs/self (Linux /proc), which is missing"
            ;;
        missing-pidfile)
            update_restart_owner_emit no-owner "$route" "$load_state" "signal restart needs pidfile $pid_abs, which is missing and which the update never writes"
            ;;
        missing-wrapper|wrapper-not-executable)
            scope=$(update_path_tree_scope "$root" "$run_abs")
            if [[ "$scope" == "outside" ]]; then
                update_restart_owner_emit no-owner "$route" "$load_state" "signal restart needs executable wrapper $run_abs, which is missing or not executable and lies outside the deployment tree, so the code transfer cannot supply it"
            else
                update_restart_owner_emit unknown "$route" "$load_state" "wrapper $run_abs is missing or not executable, but the code transfer may supply it"
            fi
            ;;
        *)
            update_restart_owner_emit unknown "$route" "$load_state" "signal prerequisites of $root could not be read"
            ;;
    esac
    return 0
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

update_unit_config_path() {
    local unit="$1" wd execstart cfg
    wd=$(systemctl show "$unit" -p WorkingDirectory --value 2>/dev/null || true)
    execstart=$(systemctl show "$unit" -p ExecStart --value 2>/dev/null | head -n 1 || true)
    cfg=$(update_execstart_config_path "$execstart")
    if [[ -z "$cfg" ]]; then
        [[ -n "$wd" ]] || { printf ''; return 0; }
        cfg="${wd%/}/scheduler/config.json"
    elif [[ "$cfg" != /* ]]; then
        [[ -n "$wd" ]] || { printf ''; return 0; }
        cfg="${wd%/}/$cfg"
    fi
    printf '%s' "$cfg"
}

update_unit_role() {
    local cfg
    cfg=$(update_unit_config_path "$1")
    [[ -n "$cfg" ]] || { printf 'scheduler'; return 0; }
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
    local cfg_path="${GO_TRADER_UPDATE_CONFIG:-scheduler/config.json}"
    local db_paths="scheduler/state.db"
    if [[ "$(update_deployment_role_for_config "$cfg_path")" == "feed" ]]; then
        printf '%s\n' "$db_paths"
        return 0
    fi
    local py_bin="${GO_TRADER_UPDATE_PYTHON:-}"
    if [[ -z "$py_bin" || ! -x "$py_bin" ]]; then
        py_bin=$(command -v python3 2>/dev/null || printf '.venv/bin/python3')
    fi
    if [[ -f "$cfg_path" && -x "$py_bin" ]]; then
        local custom
        custom=$(GO_TRADER_UPDATE_CONFIG="$cfg_path" "$py_bin" -c '
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

UPDATE_RH_IDENTITY_PY='
import json, os, sqlite3, sys, urllib.parse

def canonical(raw):
    return raw.strip().upper() if isinstance(raw, str) else ""

cfg_path = sys.argv[1]
db_paths = sys.argv[2:]
try:
    cfg = json.load(open(cfg_path))
except Exception as exc:
    print("cannot read config %s: %s" % (cfg_path, exc))
    sys.exit(2)
owners = []
for sc in cfg.get("strategies") or []:
    if not isinstance(sc, dict):
        continue
    sid = sc.get("id") if isinstance(sc.get("id"), str) else ""
    platform = sc.get("platform") if isinstance(sc.get("platform"), str) else ""
    if not platform and sid.startswith("rh-"):
        platform = "robinhood"
    args = sc.get("args") if isinstance(sc.get("args"), list) else []
    if platform != "robinhood" or sc.get("type") != "spot" or len(args) < 2 or not isinstance(args[1], str):
        continue
    storage = sc.get("storage_strategy_id")
    storage = storage.strip() if isinstance(storage, str) and storage.strip() else sid
    owners.append((sid, storage, args[1], canonical(args[1])))
if not owners:
    sys.exit(0)
blockers = []
for path in db_paths:
    if not path or not os.path.exists(path):
        continue
    uri = "file:%s?mode=ro" % urllib.parse.quote(os.path.abspath(path))
    try:
        con = sqlite3.connect(uri, uri=True, timeout=10)
        tables = {r[0] for r in con.execute("SELECT name FROM sqlite_master WHERE type = ?", ("table",))}
        for sid, storage, raw, canon in owners:
            rows = []
            if "positions" in tables:
                rows += [("position", r[0]) for r in con.execute("SELECT symbol FROM positions WHERE strategy_id = ?", (storage,))]
            if "robinhood_order_attempts" in tables:
                rows += [("unresolved order attempt", r[0]) for r in con.execute(
                    "SELECT symbol FROM robinhood_order_attempts WHERE strategy_id = ? AND status IN (?, ?, ?, ?)",
                    (storage, "submitting", "open", "unknown", "settling"))]
            if "strategies" in tables:
                for (raw_json,) in con.execute("SELECT risk_pending_circuit_closes_json FROM strategies WHERE id = ?", (storage,)):
                    try:
                        pending = json.loads(raw_json) if raw_json else {}
                    except ValueError:
                        pending = {}
                    entry = pending.get("robinhood") if isinstance(pending, dict) else None
                    for sym in (entry or {}).get("symbols") or []:
                        if isinstance(sym, dict):
                            rows.append(("pending circuit close", sym.get("symbol")))
            for kind, sym in rows:
                if isinstance(sym, str) and sym != raw and canonical(sym) == canon:
                    blockers.append("%s: config args[1] is %r but %s holds a %s under %r" % (sid, raw, path, kind, sym))
        con.close()
    except Exception as exc:
        print("cannot read state file %s: %s" % (path, exc))
        sys.exit(2)
for line in sorted(set(blockers)):
    print(line)
sys.exit(1 if blockers else 0)
'

UPDATE_RH_IDENTITY_CAPABILITY="go-trader-capability:robinhood-coin-identity-v1"

update_robinhood_identity_binary_capability() {
    local bin_path="$1" grep_rc=0
    [[ -n "$bin_path" && -f "$bin_path" && -r "$bin_path" ]] || return 2
    LC_ALL=C grep -aqF -- "$UPDATE_RH_IDENTITY_CAPABILITY" "$bin_path" || grep_rc=$?
    case "$grep_rc" in
        0) return 0 ;;
        1) return 1 ;;
        *) return 2 ;;
    esac
}

update_robinhood_identity_rollback_check() {
    local bin_path="${1:-}"
    local cfg_path="${GO_TRADER_UPDATE_CONFIG:-scheduler/config.json}"
    local -a db_paths=()
    local line out rc=0 cap_rc=0
    [[ -f "$cfg_path" ]] || return 0
    while IFS= read -r line; do
        [[ -n "$line" ]] && db_paths+=("$line")
    done < <(update_resolve_db_exclude)
    out=$(python3 -I -c "$UPDATE_RH_IDENTITY_PY" "$cfg_path" "${db_paths[@]}" 2>&1) || rc=$?
    if [[ "$rc" == "0" ]]; then
        return 0
    fi
    if [[ "$rc" == "1" ]]; then
        update_robinhood_identity_binary_capability "$bin_path" || cap_rc=$?
        if [[ "$cap_rc" == "0" ]]; then
            echo "[update] recovery: Robinhood coin identity: the restored binary ${bin_path} reads the trimmed uppercase coin identity (#1908), so the spelling differences below do not block the start" >&2
            while IFS= read -r line; do
                [[ -n "$line" ]] && echo "[update] recovery: Robinhood coin identity (not blocking): $line" >&2
            done <<<"$out"
            return 0
        fi
    fi
    while IFS= read -r line; do
        [[ -n "$line" ]] && echo "[update] recovery: Robinhood coin identity: $line" >&2
    done <<<"$out"
    if [[ "$rc" == "1" && "$cap_rc" == "1" ]]; then
        echo "[update] recovery: ACTION REQUIRED: the restored binary ${bin_path} predates the Robinhood coin identity rule (#1908) and reads Robinhood books by the exact configured coin spelling, so it would see these held books as flat. Write the uppercase coin shown above into args[1] for each named strategy in the config, then start the service (#1908)" >&2
    elif [[ "$rc" == "1" ]]; then
        echo "[update] recovery: ACTION REQUIRED: could not read the restored binary ${bin_path:-<none>} to confirm that it reads the trimmed uppercase Robinhood coin identity (#1908). An older binary reads Robinhood books by the exact configured coin spelling and would see these held books as flat. Write the uppercase coin shown above into args[1] for each named strategy in the config, then start the service (#1908)" >&2
    else
        echo "[update] recovery: ACTION REQUIRED: the Robinhood coin identity check could not read the config or a state file; check them by hand before starting the previous binary (#1908)" >&2
    fi
    return 1
}

UPDATE_RH_BINDING_PY='
import json, os, sqlite3, sys, urllib.parse

def canonical(raw):
    return raw.strip().upper() if isinstance(raw, str) else ""

def live(args):
    for i, a in enumerate(args):
        if a == "--mode=live":
            return True
        if a == "--mode" and i + 1 < len(args) and args[i + 1] == "live":
            return True
    return False

cfg_path = sys.argv[1]
db_paths = sys.argv[2:]
try:
    cfg = json.load(open(cfg_path))
except Exception as exc:
    print("cannot read config %s: %s" % (cfg_path, exc))
    sys.exit(2)
owners = []
for sc in cfg.get("strategies") or []:
    if not isinstance(sc, dict):
        continue
    sid = sc.get("id") if isinstance(sc.get("id"), str) else ""
    platform = sc.get("platform") if isinstance(sc.get("platform"), str) else ""
    if not platform and sid.startswith("rh-"):
        platform = "robinhood"
    args = sc.get("args") if isinstance(sc.get("args"), list) else []
    if platform != "robinhood" or sc.get("type") != "spot" or len(args) < 2 or not isinstance(args[1], str):
        continue
    if not live([a for a in args if isinstance(a, str)]):
        continue
    storage = sc.get("storage_strategy_id")
    storage = storage.strip() if isinstance(storage, str) and storage.strip() else sid
    owners.append((sid, storage, canonical(args[1])))
if not owners:
    sys.exit(0)
blockers = []
for path in db_paths:
    if not path or not os.path.exists(path):
        continue
    uri = "file:%s?mode=ro" % urllib.parse.quote(os.path.abspath(path))
    try:
        con = sqlite3.connect(uri, uri=True, timeout=10)
        tables = {r[0] for r in con.execute("SELECT name FROM sqlite_master WHERE type = ?", ("table",))}
        if "positions" not in tables:
            con.close()
            continue
        cols = {r[1] for r in con.execute("PRAGMA table_info(positions)")}
        if "account_key" not in cols:
            con.close()
            continue
        for sid, storage, canon in owners:
            for sym, qty, key, venue in con.execute(
                    "SELECT symbol, quantity, account_key, venue_account_id FROM positions WHERE strategy_id = ? AND side = ?",
                    (storage, "long")):
                if canonical(sym) == canon and isinstance(qty, (int, float)) and qty > 0:
                    state = "bound" if key and venue else "unbound"
                    blockers.append("%s: %s holds an open %s book of %.10g (%s) in a state file with Robinhood account binding columns" % (sid, path, canon, qty, state))
        con.close()
    except Exception as exc:
        print("cannot read state file %s: %s" % (path, exc))
        sys.exit(2)
for line in sorted(set(blockers)):
    print(line)
sys.exit(1 if blockers else 0)
'

UPDATE_RH_BINDING_CAPABILITY="go-trader-capability:robinhood-account-binding-v1"

update_robinhood_binding_binary_capability() {
    local bin_path="$1" grep_rc=0
    [[ -n "$bin_path" && -f "$bin_path" && -r "$bin_path" ]] || return 2
    LC_ALL=C grep -aqF -- "$UPDATE_RH_BINDING_CAPABILITY" "$bin_path" || grep_rc=$?
    case "$grep_rc" in
        0) return 0 ;;
        1) return 1 ;;
        *) return 2 ;;
    esac
}

update_robinhood_binding_rollback_check() {
    local bin_path="${1:-}"
    local cfg_path="${GO_TRADER_UPDATE_CONFIG:-scheduler/config.json}"
    local -a db_paths=()
    local line out rc=0 cap_rc=0
    [[ -f "$cfg_path" ]] || return 0
    while IFS= read -r line; do
        [[ -n "$line" ]] && db_paths+=("$line")
    done < <(update_resolve_db_exclude)
    out=$(python3 -I -c "$UPDATE_RH_BINDING_PY" "$cfg_path" "${db_paths[@]}" 2>&1) || rc=$?
    if [[ "$rc" == "0" ]]; then
        return 0
    fi
    if [[ "$rc" == "1" ]]; then
        update_robinhood_binding_binary_capability "$bin_path" || cap_rc=$?
        if [[ "$cap_rc" == "0" ]]; then
            echo "[update] recovery: Robinhood account binding: the restored binary ${bin_path} enforces the account binding of live Robinhood books (#1921), so the open books below do not block the start" >&2
            while IFS= read -r line; do
                [[ -n "$line" ]] && echo "[update] recovery: Robinhood account binding (not blocking): $line" >&2
            done <<<"$out"
            return 0
        fi
    fi
    while IFS= read -r line; do
        [[ -n "$line" ]] && echo "[update] recovery: Robinhood account binding: $line" >&2
    done <<<"$out"
    if [[ "$rc" == "1" && "$cap_rc" == "1" ]]; then
        echo "[update] recovery: ACTION REQUIRED: the restored binary ${bin_path} predates the Robinhood account binding (#1921). It would sell and reconcile the open live Robinhood books above without checking which account owns them. Keep the service stopped; update again to a build with the account binding, or flatten and confirm each named book by hand first (#1921)" >&2
    elif [[ "$rc" == "1" ]]; then
        echo "[update] recovery: ACTION REQUIRED: could not read the restored binary ${bin_path:-<none>} to confirm that it enforces the Robinhood account binding (#1921). Keep the service stopped until a build with the account binding is in place (#1921)" >&2
    else
        echo "[update] recovery: ACTION REQUIRED: the Robinhood account binding check could not read the config or a state file; check them by hand before starting the previous binary (#1921)" >&2
    fi
    return 1
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

update_build_go_export() {
    local tree="$1" commit="$2" go_bin="$3" ver="$4" out="$5" pkg="${6:-.}" work full rc=0
    full=$(update_git "$tree" rev-parse --verify "$commit^{commit}") || return 1
    work=$(mktemp -d "${TMPDIR:-/tmp}/go-trader-build.XXXXXX") || return 1
    (
        trap 'rm -rf "$work"' EXIT TERM INT HUP
        if ! update_git "$tree" archive --format=tar -o "$work/src.tar" "$full" scheduler || ! tar -x -f "$work/src.tar" -C "$work"; then
            exit 1
        fi
        GOWORK=off GOFLAGS=-mod=readonly "$go_bin" -C "$work/scheduler" build -buildvcs=false -ldflags "-X main.Version=$ver -X main.SourceCommit=$full" -o "$out" "$pkg"
    ) || rc=$?
    rm -rf "$work"
    return "$rc"
}

update_realpath() {
    local phys
    if [[ -d "$1" ]] && phys=$(cd "$1" 2>/dev/null && pwd -P); then
        printf '%s\n' "$phys"
        return 0
    fi
    python3 -c 'import os, sys; print(os.path.realpath(sys.argv[1]))' "$1"
}

update_path_within() {
    local p="${1%/}" d="${2%/}"
    [[ -n "$p" ]] || p="/"
    [[ -n "$d" ]] || d="/"
    [[ "$p" == "$d" || "$d" == "/" || "$p" == "$d/"* ]]
}

update_write_path_issue() {
    local tree="${1%/}" path="$2"
    if update_path_within "$tree" "$path"; then
        printf 'can write %s, which holds the whole tree' "$path"
        return 0
    fi
    if update_path_within "$path" "$tree" && ! update_path_within "$path" "$tree/scheduler" && ! update_path_within "$path" "$tree/logs"; then
        printf 'can write %s inside the tree' "$path"
    fi
}

update_unit_confinement_issues() {
    local tree="$1" protect="$2" rw="$3" bind="$4" entry path issue
    local -a entries=()
    [[ "$protect" == "strict" ]] || printf 'ProtectSystem=%s, not strict\n' "${protect:-no}"
    read -r -a entries <<<"$rw"
    for entry in ${entries[@]+"${entries[@]}"}; do
        path="${entry#-}"
        path="${path#+}"
        [[ "$path" == /* ]] || continue
        issue=$(update_write_path_issue "$tree" "$(update_realpath "$path")")
        [[ -z "$issue" ]] || printf 'ReadWritePaths %s\n' "$issue"
    done
    entries=()
    read -r -a entries <<<"$bind"
    for entry in ${entries[@]+"${entries[@]}"}; do
        path="${entry#-}"
        path="${path%%:*}"
        [[ "$path" == /* ]] || continue
        issue=$(update_write_path_issue "$tree" "$(update_realpath "$path")")
        [[ -z "$issue" ]] || printf 'BindPaths %s\n' "$issue"
    done
}

UPDATE_CONFINEMENT_INSTANCE="update-confinement-check"

update_foreign_tree_units() {
    local listed files
    listed=$(systemctl list-units --type=service --all --no-legend --plain 2>/dev/null) || return 1
    files=$(systemctl list-unit-files --type=service --no-legend --plain 2>/dev/null) || return 1
    printf '%s\n%s\n' "$listed" "$files" | awk -v inst="$UPDATE_CONFINEMENT_INSTANCE" '
        { n = $1; sub(/@\.service$/, "@" inst ".service", n); if (n ~ /\.service$/) print n }' | sort -u
}

update_foreign_tree_confinement() {
    local tree="$1" uid="$2" units_text shown unit user protect rw bind unit_uid issues issue out=""
    local -a units=()
    if ! command -v systemctl >/dev/null 2>&1; then
        printf 'systemd is not available here, so nothing shows which files the tree owner can change\n'
        return 1
    fi
    tree=$(update_realpath "$tree")
    if ! units_text=$(update_foreign_tree_units); then
        printf 'systemctl could not list the service units and unit files, so nothing shows which files the tree owner can change\n'
        return 1
    fi
    while IFS= read -r unit; do
        [[ -n "$unit" ]] && units+=("$unit")
    done <<<"$units_text"
    [[ ${#units[@]} -gt 0 ]] || return 0
    if ! shown=$(systemctl show -p Id -p User -p ProtectSystem -p ReadWritePaths -p BindPaths -- "${units[@]}" 2>/dev/null); then
        printf 'systemctl could not read the settings of the service units, so nothing shows which files the tree owner can change\n'
        return 1
    fi
    while IFS=$'\x1f' read -r unit user protect rw bind; do
        [[ -n "$unit" && -n "$user" ]] || continue
        if [[ "$user" =~ ^[0-9]+$ ]]; then
            unit_uid="$user"
        else
            unit_uid=$(id -u "$user" 2>/dev/null) || continue
        fi
        [[ "$unit_uid" == "$uid" ]] || continue
        [[ "$unit" == *"@$UPDATE_CONFINEMENT_INSTANCE.service" ]] && unit="${unit%"$UPDATE_CONFINEMENT_INSTANCE.service"}.service"
        issues=$(update_unit_confinement_issues "$tree" "$protect" "$rw" "$bind")
        while IFS= read -r issue; do
            [[ -n "$issue" ]] && out+="  $unit (User=$user): $issue"$'\n'
        done <<<"$issues"
    done < <(awk '
        function flush() { if (id != "") printf "%s\037%s\037%s\037%s\037%s\n", id, user, protect, rw, bind; id = user = protect = rw = bind = "" }
        /^$/ { flush(); next }
        { k = $0; sub(/=.*/, "", k); v = substr($0, length(k) + 2) }
        k == "Id" { id = v } k == "User" { user = v } k == "ProtectSystem" { protect = v }
        k == "ReadWritePaths" { rw = v } k == "BindPaths" { bind = v }
        END { flush() }' <<<"$shown")
    [[ -z "$out" ]] || { printf '%s' "$out"; return 1; }
}

update_tree_foreign_accounts() {
    [[ "$EUID" == "0" ]] || return 0
    local tree="${1%/}" uid
    for uid in "$(update_path_uid "$tree")" "$(update_path_uid "$tree/.git")"; do
        if [[ -n "$uid" && "$uid" != "0" ]]; then
            printf '%s\n' "$uid"
        fi
    done | sort -u
}

update_foreign_tree_check() {
    local tree="${1%/}" uids
    local -a list=()
    uids=$(update_tree_foreign_accounts "$tree")
    [[ -n "$uids" ]] || return 0
    while IFS= read -r uid; do
        [[ -n "$uid" ]] && list+=("$uid")
    done <<<"$uids"
    if [[ ${#list[@]} -gt 1 ]]; then
        printf '  the tree top belongs to uid %s and its .git to uid %s; root trusts a checkout only when one account besides root owns it\n' \
            "$(update_path_uid "$tree")" "$(update_path_uid "$tree/.git")"
        return 1
    fi
    update_foreign_tree_confinement "$tree" "${list[0]}"
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
import errno, os, stat, sys
mode, tree = sys.argv[1], os.path.realpath(sys.argv[2])
O_PATH = getattr(os, "O_PATH", 0)
if O_PATH:
    import ctypes
    libc = ctypes.CDLL(None, use_errno=True)
    libc.fchownat.argtypes = [ctypes.c_int, ctypes.c_char_p, ctypes.c_uint, ctypes.c_uint, ctypes.c_int]
AT_EMPTY_PATH = 0x1000
DIR_FLAGS = os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_NONBLOCK
class Entry:
    def __init__(self, path, name, dfd, fd, st, opath):
        self.path, self.name, self.dfd, self.fd, self.st, self.opath = path, name, dfd, fd, st, opath
    def chown(self, uid, gid):
        if self.fd is None:
            os.chown(self.name, uid, gid, dir_fd=self.dfd, follow_symlinks=False)
        elif self.opath:
            if libc.fchownat(self.fd, b"", uid, gid, AT_EMPTY_PATH) != 0:
                e = ctypes.get_errno()
                raise OSError(e, os.strerror(e), self.path)
        else:
            os.fchown(self.fd, uid, gid)
    def stat(self):
        if self.fd is None:
            return os.stat(self.name, dir_fd=self.dfd, follow_symlinks=False)
        return os.fstat(self.fd)
def pin(name, dfd):
    try:
        if O_PATH:
            return os.open(name, O_PATH | os.O_NOFOLLOW, dir_fd=dfd)
        st = os.stat(name, dir_fd=dfd, follow_symlinks=False)
        if stat.S_ISREG(st.st_mode) or stat.S_ISDIR(st.st_mode) or stat.S_ISFIFO(st.st_mode):
            return os.open(name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK | getattr(os, "O_NOCTTY", 0), dir_fd=dfd)
        return None
    except OSError as e:
        if isinstance(e, FileNotFoundError) or e.errno == errno.ELOOP:
            return False
        raise
SKIP_OPEN = (errno.ELOOP, errno.ENOTDIR, errno.ENOENT)
def walk_dir(path, dfd, top):
    for name in sorted(os.listdir(dfd)):
        fd = pin(name, dfd)
        if fd is False:
            continue
        try:
            entry = Entry(os.path.join(path, name), name, dfd, fd, None, bool(O_PATH))
            try:
                entry.st = entry.stat()
            except FileNotFoundError:
                continue
            if entry.st.st_dev != top.st_dev:
                continue
            yield entry, top
            ident = (entry.st.st_dev, entry.st.st_ino) if stat.S_ISDIR(entry.st.st_mode) else None
        finally:
            if fd is not None:
                os.close(fd)
        if ident is None:
            continue
        try:
            child = os.open(name, DIR_FLAGS, dir_fd=dfd)
        except OSError as e:
            if e.errno in SKIP_OPEN:
                continue
            raise
        try:
            cst = os.fstat(child)
            if (cst.st_dev, cst.st_ino) == ident:
                yield from walk_dir(entry.path, child, top)
        finally:
            os.close(child)
def walk():
    top_fd = os.open(tree, DIR_FLAGS)
    try:
        top = os.fstat(top_fd)
        yield Entry(tree, None, None, top_fd, top, False), top
        yield from walk_dir(tree, top_fd, top)
    finally:
        os.close(top_fd)
def report(kind, exc, tb):
    if isinstance(exc, OSError):
        sys.stderr.write("[ownership] %s: %s\n" % (exc.filename or tree, exc.strerror or exc))
    else:
        sys.__excepthook__(kind, exc, tb)
sys.excepthook = report
def shared_inode(st):
    return stat.S_ISREG(st.st_mode) and st.st_nlink > 1
def owner_ids(spec):
    import pwd, grp
    user, group = spec.split(":", 1)
    uid = int(user) if user.isdigit() else pwd.getpwnam(user).pw_uid
    gid = int(group) if group.isdigit() else grp.getgrnam(group).gr_gid
    return uid, gid
if mode == "snapshot":
    recs = []
    for e, _ in walk():
        if e.st.st_uid == 0:
            recs.append(b"%d:%d:%d:%s" % (e.st.st_dev, e.st.st_ino, e.st.st_mtime_ns, os.fsencode(e.path)))
    with open(sys.argv[3], "wb") as f:
        f.write(b"\0".join(recs))
    sys.exit(0)
if mode == "give":
    uid, gid = owner_ids(sys.argv[3])
    gave, linked = 0, []
    for e, _ in walk():
        if shared_inode(e.st):
            linked.append(e.path)
            continue
        e.chown(uid, gid)
        gave += 1
    print("GAVE %d" % gave)
    print("LINKED %d %s" % (len(linked), " ".join(linked[:5])))
    sys.exit(0)
uid, gid = owner_ids(sys.argv[4])
with open(sys.argv[3], "rb") as f:
    raw = f.read()
before_paths, before_ids = set(), set()
for rec in raw.split(b"\0"):
    if not rec:
        continue
    dev, ino, mtime, p = rec.split(b":", 3)
    before_ids.add((int(dev), int(ino), int(mtime)))
    before_paths.add(os.fsdecode(p))
new, old, linked, left = [], [], [], []
for e, _ in walk():
    if e.st.st_uid != 0:
        continue
    if e.path in before_paths or (e.st.st_dev, e.st.st_ino, e.st.st_mtime_ns) in before_ids:
        old.append(e.path)
        continue
    if shared_inode(e.st):
        linked.append(e.path)
        continue
    e.chown(uid, gid)
    if e.stat().st_uid == 0:
        left.append(e.path)
    else:
        new.append(e.path)
if left:
    print("FAILED %d %s" % (len(left), left[0]))
    sys.exit(1)
print("GAVE %d" % len(new))
print("KEPT %d %s" % (len(old), " ".join(old[:5])))
print("LINKED %d %s" % (len(linked), " ".join(linked[:5])))
'

update_owner_snapshot() {
    python3 -I -c "$UPDATE_OWNER_PY" snapshot "$1" "$2"
}

update_restore_tree_owner() {
    local tree="$1" owner="$2" snap="$3" out rc=0 line gave=0 kept=0 examples="" linked=0 linked_examples=""
    [[ -n "$tree" && -n "$owner" && -n "$snap" && -f "$snap" ]] || return 0
    out=$(python3 -I -c "$UPDATE_OWNER_PY" restore "$tree" "$snap" "$owner") || rc=$?
    while IFS= read -r line; do
        case "$line" in
            GAVE\ *) gave="${line#GAVE }" ;;
            KEPT\ *) line="${line#KEPT }"; kept="${line%% *}"; examples="${line#"$kept"}" ;;
            LINKED\ *) line="${line#LINKED }"; linked="${line%% *}"; linked_examples="${line#"$linked"}" ;;
            FAILED\ *) echo "[update] ownership: could not give ${line#FAILED } back to $owner" >&2 ;;
        esac
    done <<<"$out"
    [[ "$rc" == 0 ]] || return 1
    if [[ "$gave" != 0 ]]; then
        echo "[update] ownership: $gave path(s) this update wrote as root under $tree given back to $(update_path_owner_name "$tree")"
    fi
    if [[ "$linked" != 0 ]]; then
        echo "[update] warning: $linked root-owned file(s) under $tree share their data with another path (a hard link) and stay root-owned, so their other names keep their owner (for example${linked_examples})" >&2
    fi
    if [[ "$kept" != 0 && "${UPDATE_OWNER_KEPT_WARNED:-}" != "$tree" ]]; then
        UPDATE_OWNER_KEPT_WARNED="$tree"
        echo "[update] warning: $kept path(s) under $tree were owned by root before this update and stay so (for example${examples}); list them with: find $tree -xdev -user root, then give only the ones the owner needs with: chown -h $(update_path_owner_name "$tree"):$(update_path_group_name "$tree") <path>" >&2
    fi
    return 0
}

update_give_tree() {
    local dir="$1" owner="$2" out line gave=0 linked=0 linked_examples=""
    out=$(python3 -I -c "$UPDATE_OWNER_PY" give "$dir" "$owner") || return 1
    while IFS= read -r line; do
        case "$line" in
            GAVE\ *) gave="${line#GAVE }" ;;
            LINKED\ *) line="${line#LINKED }"; linked="${line%% *}"; linked_examples="${line#"$linked"}" ;;
        esac
    done <<<"$out"
    if [[ "$linked" != 0 ]]; then
        echo "[ownership] warning: $linked file(s) under $dir share their data with another path (a hard link) and keep their owner (for example${linked_examples})" >&2
    fi
    return 0
}

UPDATE_UNIT_ENV_PY='
import importlib.util, os, shlex, stat, sys
sys.dont_write_bytecode = True
spec = importlib.util.spec_from_file_location("migrate_service_layout", sys.argv[1])
mod = importlib.util.module_from_spec(spec)
spec.loader.exec_module(mod)
key = sys.argv[4]
strict = len(sys.argv) > 5 and sys.argv[5] == "strict"
found = None
for k, v in mod.parse_env_assignments(sys.argv[2]) or []:
    if k == key:
        found = v
for entry in sys.argv[3].splitlines():
    path = entry.split(" (ignore_errors=")[0].strip()
    if not path.startswith("/"):
        continue
    if strict:
        try:
            st = os.stat(path)
        except (FileNotFoundError, NotADirectoryError):
            continue
        except OSError as exc:
            sys.stderr.write("cannot stat environment file %s: %s\n" % (path, exc))
            sys.exit(1)
        if not stat.S_ISREG(st.st_mode):
            continue
    elif not os.path.isfile(path):
        continue
    for k, v in mod.parse_env_file(path):
        if k == key:
            found = v
if found is not None:
    print("SET " + found)
'

update_directional_cert_path_from_show() {
    local env_text="$1" files_text="$2" dir out entry path mentioned=0
    [[ "$env_text" == *GO_TRADER_DIRECTIONAL_CERT_PATH* ]] && mentioned=1
    while IFS= read -r entry || [[ -n "$entry" ]]; do
        path="${entry%% (ignore_errors=*}"
        [[ "$path" == /* && -f "$path" ]] || continue
        grep -q GO_TRADER_DIRECTIONAL_CERT_PATH "$path" 2>/dev/null && mentioned=1
    done <<<"$files_text"
    [[ "$mentioned" == 1 ]] || return 0
    update_unit_env_value_from_show GO_TRADER_DIRECTIONAL_CERT_PATH "$env_text" "$files_text"
}

update_unit_env_value_from_show() {
    local key="$1" env_text="$2" files_text="$3" mode="${4:-}" dir out
    dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
    out=$(python3 -I -B -c "$UPDATE_UNIT_ENV_PY" "$dir/migrate-service-layout.py" "$env_text" "$files_text" "$key" "$mode") || return 1
    printf '%s' "$out"
}

update_unit_directional_cert_path() {
    local unit="$1" env_text files_text
    command -v systemctl >/dev/null 2>&1 || return 0
    env_text=$(systemctl show -p Environment --value "$unit" 2>/dev/null || true)
    files_text=$(systemctl show -p EnvironmentFiles --value "$unit" 2>/dev/null || true)
    update_directional_cert_path_from_show "$env_text" "$files_text"
}

update_probe_as_owner() {
    local tree="$1" owner="$2" binary="$3" config="$4" p
    local -a rw=() args=() extra=("${@:5}")
    if ! command -v systemd-run >/dev/null 2>&1; then
        echo "[update] systemd-run is not installed, so the probe cannot run as the owner of $tree under the unit sandbox" >&2
        return 1
    fi
    for p in "$tree/scheduler" "$tree/logs"; do
        [[ -d "$p" ]] && rw+=("$p")
    done
    args=(--wait --pipe --collect --quiet --unit "go-trader-update-probe-$$-$RANDOM"
        "--uid=${owner%%:*}" "--gid=${owner#*:}"
        -p "WorkingDirectory=$tree" -p ProtectSystem=strict -p PrivateTmp=true -p NoNewPrivileges=true
        -E PYTHONDONTWRITEBYTECODE=1)
    [[ ${#rw[@]} -gt 0 ]] && args+=(-p "ReadWritePaths=${rw[*]}")
    [[ ${#extra[@]} -gt 0 ]] && args+=("${extra[@]}")
    systemd-run "${args[@]}" -- /bin/sh -c 'd=$(mktemp -d) && cp -- "$2" "$d/config.json" && exec "$1" probe --config "$d/config.json"' sh "$binary" "$config" </dev/null
}

update_owner_runs_venv() {
    local status
    status=$(update_owner_runs_venv_status "$1") || return 1
    return 0
}

update_owner_runs_venv_status() {
    local tree="$1" name py
    py="${tree%/}/.venv/bin/python3"
    [[ -e "$py" ]] || { printf 'skipped'; return 0; }
    name=$(update_path_owner_name "$tree")
    if [[ "$name" == "unknown" || "$name" == "UNKNOWN" ]]; then
        echo "[update] warning: $tree has no named owner; skipping the venv check as the owner" >&2
        printf 'skipped'
        return 0
    fi
    if ! command -v runuser >/dev/null 2>&1; then
        echo "[update] warning: runuser is not installed; skipping the venv check as $name" >&2
        printf 'skipped'
        return 0
    fi
    if runuser -u "$name" -- "$py" -c 'import encodings, sqlite3' >/dev/null 2>&1; then
        printf 'ok'
        return 0
    fi
    printf 'failed'
    return 1
}

UPDATE_EGRESS_LIBEXEC_ROOT="${UPDATE_EGRESS_LIBEXEC_ROOT:-/usr/local/libexec/go-trader-egress}"
UPDATE_EGRESS_MANIFEST_DIR="${UPDATE_EGRESS_MANIFEST_DIR:-/etc/go-trader/egress}"

update_egress_valid_deployment() {
    [[ "$1" =~ ^[a-z0-9][a-z0-9-]{0,30}$ ]]
}

update_egress_deployment_from_show() {
    local ns="$1" env="$2" tok found=""
    [[ -n "$ns" ]] || return 0
    for tok in $env; do
        if [[ "$tok" == GO_TRADER_EGRESS_DEPLOYMENT=* ]]; then
            found="${tok#GO_TRADER_EGRESS_DEPLOYMENT=}"
        fi
    done
    printf '%s' "$found"
}

update_egress_deployment() {
    local unit="$1" ns env
    command -v systemctl >/dev/null 2>&1 || return 0
    ns=$(systemctl show -p NetworkNamespacePath --value "$unit" 2>/dev/null || true)
    env=$(systemctl show -p Environment --value "$unit" 2>/dev/null || true)
    update_egress_deployment_from_show "$ns" "$env"
}

update_egress_controller_path() {
    printf '%s/%s/go-trader-egress-controller' "$UPDATE_EGRESS_LIBEXEC_ROOT" "$1"
}

update_egress_stage_controller() {
    local deployment="$1" tree="$2" commit="$3" go_bin="$4" ver="$5" rsync_src="$6" dir stage
    dir="$UPDATE_EGRESS_LIBEXEC_ROOT/$deployment"
    install -d -o root -g root -m 0755 "$UPDATE_EGRESS_LIBEXEC_ROOT" "$dir" || return 1
    stage=$(mktemp -d "$dir/.stage.XXXXXX") || return 1
    chmod 0700 "$stage" || return 1
    if [[ -n "$tree" ]]; then
        update_build_go_export "$tree" "$commit" "$go_bin" "$ver" "$stage/go-trader-egress-controller" ./cmd/egress-controller >&2 || { rm -rf "$stage"; return 1; }
    else
        GOWORK=off GOFLAGS=-mod=readonly "$go_bin" -C "$rsync_src/scheduler" build -buildvcs=false -ldflags "-X main.Version=$ver" -o "$stage/go-trader-egress-controller" ./cmd/egress-controller >&2 || { rm -rf "$stage"; return 1; }
    fi
    "$stage/go-trader-egress-controller" version >&2 || { rm -rf "$stage"; return 1; }
    printf '%s/go-trader-egress-controller' "$stage"
}

update_egress_protocol() {
    "$1" version 2>/dev/null | sed -n 's/.* protocol \([0-9][0-9]*\)$/\1/p'
}

update_egress_wait_ready() {
    local deployment="$1" timeout="${2:-30}" waited=0 bin
    bin=$(update_egress_controller_path "$deployment")
    while [[ $waited -lt $timeout ]]; do
        if "$bin" status --manifest "$UPDATE_EGRESS_MANIFEST_DIR/$deployment.json" >/dev/null 2>&1; then
            return 0
        fi
        sleep 1
        waited=$((waited + 1))
    done
    return 1
}

update_egress_wait_answering() {
    local deployment="$1" timeout="${2:-30}" waited=0 bin out
    bin=$(update_egress_controller_path "$deployment")
    while [[ $waited -lt $timeout ]]; do
        out=$("$bin" status --manifest "$UPDATE_EGRESS_MANIFEST_DIR/$deployment.json" 2>/dev/null || true)
        if grep -q '"deployment": *"'"$deployment"'"' <<<"$out"; then
            return 0
        fi
        sleep 1
        waited=$((waited + 1))
    done
    return 1
}

update_egress_restart_controller() {
    systemctl restart --job-mode=ignore-dependencies "go-trader-egress@$1.service"
}

update_egress_restart_broker() {
    local unit="go-trader-notify@$1.service"
    if ! systemctl is-active --quiet "$unit"; then
        echo "[update] egress: no running notification broker for $1; nothing to restart"
        return 0
    fi
    if ! systemctl restart "$unit"; then
        echo "[update] egress: restarting $unit failed; it still runs the previous controller binary" >&2
        return 1
    fi
    echo "[update] egress: notification broker $unit restarted on the installed controller binary"
}

update_egress_switch_controller() {
    local deployment="$1" staged="$2" state_file="${3:-}" cur
    cur=$(update_egress_controller_path "$deployment")
    if [[ -f "$cur" ]] && cmp -s "$staged" "$cur"; then
        echo "[update] egress: controller for $deployment is unchanged"
        rm -rf "$(dirname "$staged")"
        return 2
    fi
    if [[ -n "$state_file" ]]; then
        printf 'installed=1\nrestarted=0\n' >"$state_file"
    fi
    if [[ -f "$cur" ]]; then
        echo "[update] egress: controller protocol $(update_egress_protocol "$cur") -> $(update_egress_protocol "$staged")"
        cp -p "$cur" "$cur.prev.tmp" && mv -f "$cur.prev.tmp" "$cur.prev" || return 1
    else
        rm -f "$cur.prev"
    fi
    install -o root -g root -m 0755 "$staged" "$cur.tmp" && mv -f "$cur.tmp" "$cur" || return 1
    rm -rf "$(dirname "$staged")"
    if [[ -n "$state_file" ]]; then
        printf 'installed=1\nrestarted=1\n' >"$state_file"
    fi
    update_egress_restart_controller "$deployment" || return 1
    if ! update_egress_wait_ready "$deployment" 30; then
        echo "[update] egress: controller for $deployment did not report ready within 30s" >&2
        return 1
    fi
    update_egress_restart_broker "$deployment" || return 1
    echo "[update] egress: controller for $deployment switched and ready (namespace rules stayed installed throughout)"
    return 0
}

update_egress_restore_controller() {
    local deployment="$1" snap_cur="${2:-}" snap_prev="${3:-}" need_restart="${4:-1}" cur
    cur=$(update_egress_controller_path "$deployment")
    if [[ -n "$snap_cur" && -f "$snap_cur" ]]; then
        cp -p "$snap_cur" "$cur.restore.tmp" && mv -f "$cur.restore.tmp" "$cur" || return 1
        if [[ -n "$snap_prev" && -f "$snap_prev" ]]; then
            cp -p "$snap_prev" "$cur.prev.restore.tmp" && mv -f "$cur.prev.restore.tmp" "$cur.prev" || return 1
        else
            rm -f "$cur.prev"
        fi
    else
        [[ -f "$cur.prev" ]] || return 1
        mv -f "$cur.prev" "$cur" || return 1
    fi
    if [[ "$need_restart" == "1" ]]; then
        update_egress_restart_controller "$deployment" || return 1
        update_egress_wait_ready "$deployment" 30 || return 1
        update_egress_restart_broker "$deployment"
        return
    fi
    update_egress_wait_ready "$deployment" 30
}

update_egress_probe_args() {
    local unit="$1" deployment="$2" entry path
    local -a out=(-E "GO_TRADER_EGRESS_DEPLOYMENT=$deployment")
    while IFS= read -r entry || [[ -n "$entry" ]]; do
        [[ -n "$entry" ]] || continue
        path="${entry%% (ignore_errors=*}"
        [[ "$path" == /* ]] || continue
        out+=(-p "EnvironmentFile=-$path")
    done < <(systemctl show -p EnvironmentFiles --value "$unit" 2>/dev/null)
    printf '%s\n' "${out[@]}"
}

update_unit_owner() {
    local unit="$1" user group uid gid
    user=$(systemctl show -p User --value "$unit" 2>/dev/null || true)
    group=$(systemctl show -p Group --value "$unit" 2>/dev/null || true)
    [[ -n "$user" ]] || { printf '0:0'; return 0; }
    uid=$(id -u "$user" 2>/dev/null) || return 1
    if [[ -n "$group" ]]; then
        gid=$(getent group "$group" | cut -d: -f3) || return 1
    else
        gid=$(id -g "$user" 2>/dev/null) || return 1
    fi
    printf '%s:%s' "$uid" "$gid"
}

update_relpath_in_tree() {
    local tree="$1" raw="$2"
    [[ -n "$raw" ]] || return 0
    if [[ "$raw" != /* && "$raw" != *..* ]]; then
        printf '%s\n' "${raw#./}"
        return 0
    fi
    python3 - "$tree" "$raw" <<'PY'
import os, sys
tree = os.path.realpath(sys.argv[1])
raw = sys.argv[2]
if not raw:
    sys.exit(0)
if os.path.isabs(raw):
    parent = os.path.realpath(os.path.dirname(raw))
    cand = os.path.join(parent, os.path.basename(raw))
else:
    cand = os.path.normpath(os.path.join(tree, raw))
if cand == tree:
    print(".")
elif cand.startswith(tree.rstrip("/") + "/"):
    print(cand[len(tree.rstrip("/")) + 1:])
PY
}

update_managed_rsync_excludes() {
    if [[ "${UPDATE_MANAGED_EXCLUDES_FROZEN:-0}" == "1" ]]; then
        return 0
    fi
    local deploy_dir="${1:-$PWD}"
    local cfg_path="${GO_TRADER_UPDATE_CONFIG:-$deploy_dir/scheduler/config.json}"
    if [[ "$cfg_path" != /* ]]; then
        cfg_path="$deploy_dir/$cfg_path"
    fi
    local signal_log_excl="${GO_TRADER_SIGNAL_LOG:-./go-trader-signal.log}"
    local pidfile_excl="${GO_TRADER_PIDFILE:-./go-trader.pid}"
    local parsed_meta
    if ! parsed_meta=$(python3 - "$deploy_dir" "$cfg_path" <<'PY'
import json, os, sys
deploy = os.path.realpath(sys.argv[1])
cfg_path = sys.argv[2]
if not os.path.exists(cfg_path):
    sys.stderr.write("[update] config missing: %s\n" % cfg_path)
    sys.exit(1)
try:
    with open(cfg_path, "r", encoding="utf-8") as f:
        cfg = json.load(f)
except Exception as exc:
    sys.stderr.write("[update] invalid config JSON in %s: %s\n" % (cfg_path, exc))
    sys.exit(1)
if not isinstance(cfg, dict):
    sys.stderr.write("[update] config root in %s must be a JSON object\n" % cfg_path)
    sys.exit(1)
def rel_inside(p):
    if not p or not isinstance(p, str):
        return ""
    p = p.strip()
    if not p:
        return ""
    if os.path.isabs(p):
        parent = os.path.realpath(os.path.dirname(p))
        full = os.path.join(parent, os.path.basename(p))
    else:
        full = os.path.normpath(os.path.join(deploy, p))
    if full == deploy:
        sys.stderr.write("[update] unsafe path resolves to deployment root: %s\n" % p)
        sys.exit(1)
    if os.path.isdir(full) and not os.path.islink(full):
        sys.stderr.write("[update] unsafe state path is a directory: %s\n" % p)
        sys.exit(1)
    if full.startswith(deploy.rstrip("/") + "/"):
        return full[len(deploy.rstrip("/")) + 1:]
    return ""
if os.path.islink(cfg_path):
    real_cfg = os.path.realpath(cfg_path)
    if real_cfg == deploy or (os.path.isdir(real_cfg)):
        sys.stderr.write("[update] unsafe config symlink target: %s -> %s\n" % (cfg_path, real_cfg))
        sys.exit(1)
    if real_cfg.startswith(deploy.rstrip("/") + "/"):
        print("CFG_TARGET=" + real_cfg[len(deploy.rstrip("/")) + 1:])
else:
    real_cfg = os.path.realpath(cfg_path)
cfg_dir = os.path.dirname(real_cfg)
if cfg_dir == deploy:
    print("CFG_DIR_REL=.")
elif cfg_dir.startswith(deploy.rstrip("/") + "/"):
    print("CFG_DIR_REL=" + cfg_dir[len(deploy.rstrip("/")) + 1:])
role = cfg.get("role")
is_feed = isinstance(role, str) and role.strip() == "feed"
if is_feed:
    print("DB_EXCL=scheduler/state.db")
else:
    p = cfg.get("db_file") or ""
    print("DB_EXCL=" + (p.strip() if isinstance(p, str) and p.strip() else "scheduler/state.db"))
    q = cfg.get("paper_db_file") or ""
    if isinstance(q, str) and q.strip():
        print("DB_EXCL=" + q.strip())
    for key in ("db_file", "paper_db_file", "replay_log_path"):
        v = cfg.get(key)
        r = rel_inside(v)
        if r:
            print("DB_REL=" + r)
    for src in cfg.get("paper_sources") or []:
        if isinstance(src, dict):
            s = src.get("db_file") or ""
            if isinstance(s, str) and s.strip():
                print("DB_EXCL=" + s.strip())
            r = rel_inside(s)
            if r:
                print("DB_REL=" + r)
log_dir = cfg.get("log_dir")
if isinstance(log_dir, str) and log_dir.strip():
    ld = log_dir.strip().rstrip("/")
    if ld and ld != "logs" and ld != "./logs":
        if os.path.isabs(ld):
            full_ld = os.path.realpath(ld)
        else:
            full_ld = os.path.normpath(os.path.join(deploy, ld))
        if full_ld == deploy:
            sys.stderr.write("[update] unsafe log_dir resolves to deployment root: %s\n" % ld)
            sys.exit(1)
        if full_ld.startswith(deploy.rstrip("/") + "/"):
            print("LOG_REL=" + full_ld[len(deploy.rstrip("/")) + 1:])
sp = cfg.get("status_port") or 0
if isinstance(sp, (int, float)) and int(sp) > 0:
    print("STATUS_PORT=%d" % int(sp))
PY
    ); then
        return 1
    fi

    UPDATE_CONFIG_STATUS_PORT="8099"
    UPDATE_MANAGED_EXCLUDES=(
        --exclude='.git/'
        --exclude='.env'
        --exclude='scheduler/config.json'
        --exclude='trading_bot.db*'
        --exclude='scheduler/ohlcv_cache.sqlite3*'
        --exclude='/scheduler/tuning_runs/'
        --exclude='scheduler/tuning_runs/'
    )
    local line db_excl db_glob rel_p cfg_dir_rel
    while IFS= read -r line; do
        case "$line" in
            CFG_TARGET=*)
                UPDATE_MANAGED_EXCLUDES+=(--exclude="/${line#CFG_TARGET=}")
                ;;
            CFG_DIR_REL=*)
                cfg_dir_rel="${line#CFG_DIR_REL=}"
                if [[ "$cfg_dir_rel" == "." ]]; then
                    UPDATE_MANAGED_EXCLUDES+=(
                        --exclude='/tuning_runs/'
                        --exclude='tuning_runs/'
                        --exclude='/ohlcv_cache.sqlite3*'
                        --exclude='ohlcv_cache.sqlite3*'
                    )
                elif [[ -n "$cfg_dir_rel" ]]; then
                    UPDATE_MANAGED_EXCLUDES+=(
                        --exclude="/${cfg_dir_rel}/tuning_runs/"
                        --exclude="${cfg_dir_rel}/tuning_runs/"
                        --exclude="/${cfg_dir_rel}/ohlcv_cache.sqlite3*"
                        --exclude="${cfg_dir_rel}/ohlcv_cache.sqlite3*"
                    )
                fi
                ;;
            DB_REL=*)
                UPDATE_MANAGED_EXCLUDES+=(--exclude="/${line#DB_REL=}*" --exclude="${line#DB_REL=}*")
                ;;
            DB_EXCL=*)
                db_excl="${line#DB_EXCL=}"
                if [[ -n "$db_excl" ]]; then
                    UPDATE_MANAGED_EXCLUDES+=(--exclude="${db_excl}*")
                    rel_p=$(update_relpath_in_tree "$deploy_dir" "$db_excl" || true)
                    if [[ -n "$rel_p" && "$rel_p" != "." ]]; then
                        UPDATE_MANAGED_EXCLUDES+=(--exclude="/${rel_p}*")
                    fi
                fi
                ;;
            LOG_REL=*)
                UPDATE_MANAGED_EXCLUDES+=(--exclude="/${line#LOG_REL=}/")
                ;;
            STATUS_PORT=*)
                UPDATE_CONFIG_STATUS_PORT="${line#STATUS_PORT=}"
                ;;
        esac
    done <<<"$parsed_meta"

    while IFS= read -r db_glob; do
        [[ -n "$db_glob" ]] && UPDATE_MANAGED_EXCLUDES+=(--exclude="$db_glob")
    done < <(update_db_rsync_excludes)
    UPDATE_MANAGED_EXCLUDES+=(
        --exclude='*.db.manual-action.lock'
        --exclude='*.manual-regime-owner.json'
        --exclude='.venv/'
        --exclude='.venv.restore-new/'
        --exclude='.venv.restore-old/'
        --exclude='robinhood-session/'
        --exclude='/logs/'
        --exclude='node_modules/'
        --exclude='__pycache__/'
        --exclude='go-trader'
        --exclude='go-trader.new'
        --exclude='go-trader.prev'
        --exclude='go-trader.restore-tmp'
        --exclude='go-trader.prev.restore-tmp'
        --exclude='go-trader.pid'
        --exclude='go-trader-signal.log'
    )
    if [[ "$signal_log_excl" != "./go-trader-signal.log" && "$signal_log_excl" != "go-trader-signal.log" ]]; then
        UPDATE_MANAGED_EXCLUDES+=(--exclude="$signal_log_excl")
        rel_p=$(update_relpath_in_tree "$deploy_dir" "$signal_log_excl" || true)
        if [[ -n "$rel_p" && "$rel_p" != "." ]]; then
            UPDATE_MANAGED_EXCLUDES+=(--exclude="/$rel_p")
        fi
    fi
    if [[ "$pidfile_excl" != "./go-trader.pid" && "$pidfile_excl" != "go-trader.pid" ]]; then
        UPDATE_MANAGED_EXCLUDES+=(--exclude="$pidfile_excl")
        rel_p=$(update_relpath_in_tree "$deploy_dir" "$pidfile_excl" || true)
        if [[ -n "$rel_p" && "$rel_p" != "." ]]; then
            UPDATE_MANAGED_EXCLUDES+=(--exclude="/$rel_p")
        fi
    fi
    if [[ "${UPDATE_NO_FREEZE_EXCLUDES:-0}" != "1" ]]; then
        readonly -a UPDATE_MANAGED_EXCLUDES
        readonly UPDATE_MANAGED_EXCLUDES_FROZEN=1
    fi
    return 0
}

update_preflight_rsync() {
    if ! command -v rsync >/dev/null 2>&1; then
        echo "[update] rsync not on PATH — install rsync (SKILL.md → Prerequisites)" >&2
        return 1
    fi
    local probe_dir
    probe_dir=$(mktemp -d "${TMPDIR:-/tmp}/go-trader-rsync-probe.XXXXXX") || return 1
    mkdir -p "$probe_dir/src" "$probe_dir/dst"
    printf 'ok\n' >"$probe_dir/src/f"
    local -a owner_flags=()
    if [[ "$EUID" == "0" ]]; then
        owner_flags=(--no-owner --no-group)
    fi
    if ! rsync -a ${owner_flags[@]+"${owner_flags[@]}"} --delete --exclude='.git/' "$probe_dir/src/" "$probe_dir/dst/" >/dev/null 2>&1; then
        rm -rf "$probe_dir"
        echo "[update] rsync failed flag compatibility check (-a --delete --exclude)" >&2
        return 1
    fi
    rm -rf "$probe_dir"
    return 0
}

update_recovery_root() {
    if [[ -n "${GO_TRADER_UPDATE_RECOVERY_DIR:-}" ]]; then
        printf '%s' "${GO_TRADER_UPDATE_RECOVERY_DIR%/}"
        return 0
    fi
    if [[ "$EUID" == "0" ]]; then
        printf '/var/lib/go-trader-update'
        return 0
    fi
    local state_home="${XDG_STATE_HOME:-}"
    if [[ -z "$state_home" ]]; then
        state_home="${HOME:-/var/tmp}/.local/state"
    fi
    printf '%s/go-trader-update' "${state_home%/}"
}

update_candidate_recovery_roots() {
    local deploy_dir="${1:-$PWD}" explicit_root="${2:-}"
    local -a raw_roots=()
    if [[ -n "$explicit_root" ]]; then
        raw_roots+=("${explicit_root%/}")
    fi
    if [[ -n "${GO_TRADER_UPDATE_RECOVERY_DIR:-}" ]]; then
        raw_roots+=("${GO_TRADER_UPDATE_RECOVERY_DIR%/}")
    fi
    raw_roots+=("$(update_recovery_root)")
    raw_roots+=("/var/lib/go-trader-update")
    if [[ -n "${GO_TRADER_UPDATE_SYSTEM_RECOVERY_DIR:-}" ]]; then
        raw_roots+=("${GO_TRADER_UPDATE_SYSTEM_RECOVERY_DIR%/}")
    fi
    if [[ -z "${GO_TRADER_UPDATE_RECOVERY_DIR:-}" || -n "${GO_TRADER_UPDATE_SYSTEM_RECOVERY_DIR:-}" ]]; then
        if [[ -n "${XDG_STATE_HOME:-}" ]]; then
            raw_roots+=("${XDG_STATE_HOME%/}/go-trader-update")
        fi
        if [[ -n "${HOME:-}" && "${HOME:-}" != "/nonexistent" ]]; then
            raw_roots+=("${HOME%/}/.local/state/go-trader-update")
        fi
    fi
    local owner_homes=""
    if [[ "$EUID" == "0" || -n "${SUDO_USER:-}" ]]; then
        owner_homes=$(python3 - "$deploy_dir" "${SUDO_USER:-}" <<'PY' 2>/dev/null || true
import os, pwd, sys
deploy = sys.argv[1]
sudo_user = sys.argv[2]
uids = set()
for p in (deploy, os.path.join(deploy, ".git")):
    try:
        st = os.lstat(p)
        if st.st_uid != 0:
            uids.add(st.st_uid)
    except OSError:
        pass
for uid in sorted(uids):
    try:
        h = pwd.getpwuid(uid).pw_dir
        if h and h != "/nonexistent":
            print(h.rstrip("/") + "/.local/state/go-trader-update")
    except KeyError:
        pass
if sudo_user:
    try:
        h = pwd.getpwnam(sudo_user).pw_dir
        if h and h != "/nonexistent":
            print(h.rstrip("/") + "/.local/state/go-trader-update")
    except KeyError:
        pass
PY
        )
    fi
    local line seen=$'\n' r
    while IFS= read -r line; do
        [[ -n "$line" ]] && raw_roots+=("${line%/}")
    done <<<"$owner_homes"
    for r in "${raw_roots[@]}"; do
        [[ -n "$r" ]] || continue
        case "$seen" in
            *$'\n'"$r"$'\n'*) continue ;;
        esac
        seen="${seen}${r}"$'\n'
        printf '%s\n' "$r"
    done
}

update_recovery_key() {
    local canon
    canon=$(update_realpath "$1")
    python3 - "$canon" <<'PY'
import hashlib, os, re, sys
p = sys.argv[1].rstrip("/") or "/"
base = re.sub(r"[^A-Za-z0-9_.-]+", "-", os.path.basename(p)).strip("-") or "root"
h = hashlib.sha256(p.encode("utf-8")).hexdigest()[:16]
print("%s-%s" % (base, h))
PY
}

update_path_dev_ino() {
    stat -c '%d:%i' "$1" 2>/dev/null || stat -f '%d:%i' "$1" 2>/dev/null
}

update_dev_ino_inode() {
    local val="$1"
    printf '%s' "${val#*:}"
}

update_pid_alive() {
    local pid="$1"
    [[ "$pid" =~ ^[1-9][0-9]*$ ]] || return 1
    python3 - "$pid" <<'PY' 2>/dev/null
import errno, os, sys
pid = int(sys.argv[1])
try:
    os.kill(pid, 0)
    sys.exit(0)
except PermissionError:
    sys.exit(0)
except ProcessLookupError:
    sys.exit(1)
except OSError as e:
    if e.errno == errno.EPERM:
        sys.exit(0)
    sys.exit(1)
PY
}

update_current_boot_id() {
    if [[ -n "${GO_TRADER_UPDATE_BOOT_ID:-}" ]]; then
        printf '%s' "${GO_TRADER_UPDATE_BOOT_ID}"
        return 0
    fi
    if [[ -r /proc/sys/kernel/random/boot_id ]]; then
        tr -d '[:space:]' </proc/sys/kernel/random/boot_id 2>/dev/null || true
        return 0
    fi
    if command -v sysctl >/dev/null 2>&1; then
        sysctl -n kern.bootsessionuuid 2>/dev/null | tr -d '[:space:]' || true
    fi
}

update_pid_start_stamp() {
    local pid="$1"
    [[ "$pid" =~ ^[1-9][0-9]*$ ]] || return 0
    local proot="${GO_TRADER_PROC_ROOT:-/proc}"
    if [[ -r "$proot/$pid/stat" ]]; then
        awk '{print $22}' "$proot/$pid/stat" 2>/dev/null || true
        return 0
    fi
    LC_ALL=C ps -o lstart= -p "$pid" 2>/dev/null | sed 's/^[[:space:]]*//;s/[[:space:]]*$//' || true
}

update_lock_is_live() {
    local lock_dir="$1" holder_pid="$2"
    [[ -n "$holder_pid" ]] || return 1
    update_pid_alive "$holder_pid" || return 1
    if [[ -f "$lock_dir/boot_id" ]]; then
        local lock_boot="" cur_boot=""
        lock_boot=$(cat "$lock_dir/boot_id" 2>/dev/null | tr -d '[:space:]' || true)
        cur_boot=$(update_current_boot_id)
        if [[ -n "$lock_boot" && -n "$cur_boot" && "$lock_boot" != "$cur_boot" ]]; then
            return 1
        fi
    fi
    if [[ -f "$lock_dir/start_stamp" ]]; then
        local lock_start="" cur_start=""
        lock_start=$(cat "$lock_dir/start_stamp" 2>/dev/null || true)
        cur_start=$(update_pid_start_stamp "$holder_pid")
        if [[ -n "$lock_start" && -n "$cur_start" && "$lock_start" != "$cur_start" ]]; then
            return 1
        fi
    fi
    return 0
}

update_write_lock_metadata() {
    local lock_dir="$1" owner_pid="$2" root_dir="${3:-}"
    printf '%s\n' "$owner_pid" >"$lock_dir/pid"
    printf '%s\n' "$EUID" >"$lock_dir/uid"
    local boot_id start_stamp
    boot_id=$(update_current_boot_id)
    start_stamp=$(update_pid_start_stamp "$owner_pid")
    [[ -n "$boot_id" ]] && printf '%s\n' "$boot_id" >"$lock_dir/boot_id"
    [[ -n "$start_stamp" ]] && printf '%s\n' "$start_stamp" >"$lock_dir/start_stamp"
    chmod 0644 "$lock_dir/pid" "$lock_dir/uid" "$lock_dir/boot_id" "$lock_dir/start_stamp" 2>/dev/null || true
    if [[ "$EUID" == "0" && -n "$root_dir" && -d "$root_dir" ]]; then
        local r_uid r_gid
        r_uid=$(update_path_uid "$root_dir" 2>/dev/null || echo "")
        r_gid=$(update_path_gid "$root_dir" 2>/dev/null || echo "")
        if [[ -n "$r_uid" && "$r_uid" != "0" ]]; then
            chown -R "${r_uid}:${r_gid:-$r_uid}" "$lock_dir" 2>/dev/null || true
        fi
    fi
}

update_lock_holder_pid() {
    local lock_dir="$1"
    [[ -d "$lock_dir" ]] || return 1
    local holder_pid="" i
    for i in 1 2 3 4 5; do
        if [[ -f "$lock_dir/pid" ]]; then
            holder_pid=$(cat "$lock_dir/pid" 2>/dev/null | tr -d '[:space:]' || true)
            [[ -n "$holder_pid" ]] && break
        fi
        sleep 0.05
    done
    printf '%s' "$holder_pid"
    return 0
}

update_ensure_recovery_root() {
    local root="$1"
    if [[ -d "$root" ]]; then
        [[ -w "$root" ]] || return 1
        return 0
    fi
    mkdir -p "$root" 2>/dev/null || return 1
    [[ -d "$root" && -w "$root" ]] || return 1
    chmod 0755 "$root" 2>/dev/null || return 1
    if [[ "$EUID" == "0" ]]; then
        local own_root
        own_root=$(update_recovery_root)
        if [[ "${root%/}" == "/var/lib/go-trader-update" || "${root%/}" == "${own_root%/}" || ( -n "${GO_TRADER_UPDATE_SYSTEM_RECOVERY_DIR:-}" && "${root%/}" == "${GO_TRADER_UPDATE_SYSTEM_RECOVERY_DIR%/}" ) ]]; then
            chown 0:0 "$root" 2>/dev/null || true
        fi
    fi
    return 0
}

update_acquire_lock() {
    local root="$1" key="$2" owner_pid="$3" deploy_dir="$4"
    local cand_root cand_lock holder_pid
    while IFS= read -r cand_root; do
        [[ -n "$cand_root" ]] || continue
        cand_lock="$cand_root/${key}.lock"
        [[ -d "$cand_lock" ]] || continue
        holder_pid=$(update_lock_holder_pid "$cand_lock" || true)
        if [[ -z "$holder_pid" ]] || update_lock_is_live "$cand_lock" "$holder_pid"; then
            echo "[update] lock: another update is already running for $deploy_dir (lock $cand_lock held by pid ${holder_pid:-starting}; if stale, remove with: rm -rf $cand_lock)" >&2
            return 1
        fi
        echo "[update] lock: removing stale lock $cand_lock from dead or pre-reboot pid $holder_pid" >&2
        if ! rm -rf "$cand_lock" 2>/dev/null || [[ -d "$cand_lock" ]]; then
            echo "[update] lock: cannot remove stale lock $cand_lock from dead pid $holder_pid; remove it with sudo" >&2
            return 1
        fi
    done < <(update_candidate_recovery_roots "$deploy_dir" "$root")

    update_pause_if_requested lock_before_mkdir

    if ! update_ensure_recovery_root "$root"; then
        if ! update_check_incomplete_marker "$root" "$key" "$deploy_dir"; then
            return 3
        fi
        echo "[update] lock: cannot create or access recovery directory $root" >&2
        return 2
    fi
    local lock_dir="$root/${key}.lock"
    if mkdir -m 0755 "$lock_dir" 2>/dev/null; then
        update_write_lock_metadata "$lock_dir" "$owner_pid" "$root"
    else
        holder_pid=$(update_lock_holder_pid "$lock_dir" || true)
        if [[ -z "$holder_pid" ]] || update_lock_is_live "$lock_dir" "$holder_pid"; then
            echo "[update] lock: another update is already running for $deploy_dir (lock $lock_dir held by pid ${holder_pid:-starting}; if stale, remove with: rm -rf $lock_dir)" >&2
            return 1
        fi
        echo "[update] lock: removing stale lock $lock_dir from dead or pre-reboot pid $holder_pid" >&2
        rm -rf "$lock_dir" 2>/dev/null || true
        if ! mkdir -m 0755 "$lock_dir" 2>/dev/null; then
            echo "[update] lock: another update acquired $lock_dir concurrently for $deploy_dir" >&2
            return 1
        fi
        update_write_lock_metadata "$lock_dir" "$owner_pid" "$root"
    fi

    while IFS= read -r cand_root; do
        [[ -n "$cand_root" ]] || continue
        [[ "${cand_root%/}" == "${root%/}" ]] && continue
        cand_lock="$cand_root/${key}.lock"
        [[ -d "$cand_lock" ]] || continue
        holder_pid=$(update_lock_holder_pid "$cand_lock" || true)
        if [[ -z "$holder_pid" ]] || ( [[ "$holder_pid" != "$owner_pid" ]] && update_lock_is_live "$cand_lock" "$holder_pid" ); then
            rm -rf "$lock_dir"
            echo "[update] lock: another update is already running for $deploy_dir (lock $cand_lock held by pid ${holder_pid:-starting}; if stale, remove with: rm -rf $cand_lock)" >&2
            return 1
        fi
    done < <(update_candidate_recovery_roots "$deploy_dir" "$root")
    return 0
}

update_release_lock() {
    local root="$1" key="$2" owner_pid="$3"
    [[ -n "$root" && -n "$key" ]] || return 0
    local lock_dir="$root/${key}.lock"
    [[ -d "$lock_dir" ]] || return 0
    local holder_pid=""
    if [[ -f "$lock_dir/pid" ]]; then
        holder_pid=$(cat "$lock_dir/pid" 2>/dev/null | tr -d '[:space:]' || true)
    fi
    if [[ -z "$holder_pid" || "$holder_pid" == "$owner_pid" ]]; then
        rm -rf "$lock_dir"
    fi
    return 0
}

update_check_incomplete_marker() {
    local root="$1" key="$2" deploy_dir="$3"
    local cur_canon cur_dev_ino cur_ino
    cur_canon=$(update_realpath "$deploy_dir")
    cur_dev_ino=$(update_path_dev_ino "$cur_canon" || true)
    cur_ino=$(update_dev_ino_inode "$cur_dev_ino")

    local cand_root mfile m_path m_dev_ino m_ino m_snap m_uid
    while IFS= read -r cand_root; do
        [[ -n "$cand_root" && -d "$cand_root" ]] || continue

        shopt -s nullglob
        local -a markers=( "$cand_root"/*.incomplete )
        shopt -u nullglob
        for mfile in ${markers[@]+"${markers[@]}"}; do
            [[ -f "$mfile" ]] || continue
            m_path=$(sed -n 's/^deploy_path=//p' "$mfile" | head -n 1)
            m_dev_ino=$(sed -n 's/^deploy_dev_ino=//p' "$mfile" | head -n 1)
            m_ino=$(update_dev_ino_inode "$m_dev_ino")
            m_snap=$(sed -n 's/^snapshot_dir=//p' "$mfile" | head -n 1)
            if [[ -z "$m_path" || ! -d "$m_path" ]]; then
                echo "[update] recovery: stale incomplete-recovery marker $mfile for missing directory ${m_path:-<unknown>} (snapshot: ${m_snap:-none}; does not block)" >&2
                continue
            fi
            local actual_dev_ino actual_ino
            actual_dev_ino=$(update_path_dev_ino "$m_path" || true)
            actual_ino=$(update_dev_ino_inode "$actual_dev_ino")
            if [[ -n "$m_ino" && -n "$actual_ino" && "$m_ino" != "$actual_ino" ]]; then
                echo "[update] recovery: stale incomplete-recovery marker $mfile for replaced directory $m_path (recorded inode $m_ino != current $actual_ino; snapshot: ${m_snap:-none}; does not block)" >&2
                continue
            fi
        done

        local own_marker="$cand_root/${key}.incomplete"
        if [[ ! -f "$own_marker" && ( -d "$cand_root/$key" || -f "$cand_root/${key}.state" ) && "${snapshot_started:-0}" != "1" ]]; then
            local lock_holder="" other_live=0
            if [[ -d "$cand_root/${key}.lock" ]]; then
                lock_holder=$(update_lock_holder_pid "$cand_root/${key}.lock" || true)
                if [[ -n "$lock_holder" && "$lock_holder" != "${main_shell_pid:-$$}" ]] && update_lock_is_live "$cand_root/${key}.lock" "$lock_holder"; then
                    other_live=1
                fi
            fi
            if [[ "$other_live" == "0" ]]; then
                local is_killed_mutated=0 snap_dev_ino="" snap_ino="" intents_list=""
                if [[ -f "$cand_root/$key/COMPLETE" && -s "$cand_root/$key/intents.log" ]]; then
                    is_killed_mutated=1
                    snap_dev_ino=$(sed -n 's/^deploy_dev_ino=//p' "$cand_root/$key/manifest.env" 2>/dev/null | tr -d "'\"\\" | head -n 1)
                    intents_list=$(tr '\n' ',' <"$cand_root/$key/intents.log" 2>/dev/null | sed 's/,$//')
                elif [[ -f "$cand_root/${key}.state" ]] && grep -q '^complete=1$' "$cand_root/${key}.state" 2>/dev/null && grep -q '^intent=' "$cand_root/${key}.state" 2>/dev/null; then
                    is_killed_mutated=1
                    snap_dev_ino=$(sed -n 's/^deploy_dev_ino=//p' "$cand_root/${key}.state" 2>/dev/null | head -n 1)
                    intents_list=$(sed -n 's/^intent=//p' "$cand_root/${key}.state" 2>/dev/null | tr '\n' ',' | sed 's/,$//')
                elif [[ -f "$cand_root/$key/COMPLETE" && ( -e "$deploy_dir/.venv.restore-old" || -e "$deploy_dir/.venv.restore-new" ) ]]; then
                    is_killed_mutated=1
                    snap_dev_ino=$(sed -n 's/^deploy_dev_ino=//p' "$cand_root/$key/manifest.env" 2>/dev/null | tr -d "'\"\\" | head -n 1)
                    intents_list="recovery_venv"
                fi
                snap_ino=$(update_dev_ino_inode "$snap_dev_ino")
                if [[ "$is_killed_mutated" == "1" ]]; then
                    if [[ -n "$snap_ino" && -n "$cur_ino" && "$snap_ino" != "$cur_ino" ]]; then
                        echo "[update] recovery: stale unfinished snapshot $cand_root/$key for replaced directory $cur_canon (recorded inode $snap_ino != current $cur_ino; does not block)" >&2
                    else
                        if [[ -w "$cand_root" ]]; then
                            update_write_incomplete_marker "$cand_root" "$key" "$deploy_dir" "$cand_root/$key" "previous update was killed or lost power after mutation started (${intents_list:-unknown})" "interrupted=killed" || true
                        fi
                        if [[ ! -f "$own_marker" ]]; then
                            local clear_killed="bash scripts/update.sh --clear-recovery"
                            [[ "$(update_path_uid "$cand_root" 2>/dev/null || echo "")" == "0" ]] && clear_killed="sudo bash scripts/update.sh --clear-recovery"
                            echo "[update] REFUSED: incomplete recovery marker for killed update in $cand_root/$key blocks update of $cur_canon. Inspect $cand_root/$key, verify the deployment, then run: $clear_killed" >&2
                            return 1
                        fi
                    fi
                elif [[ "${update_lock_held:-0}" == "1" && -w "$cand_root" ]]; then
                    rm -rf "$cand_root/$key" "$cand_root/${key}.state"
                fi
            fi
        fi

        if [[ -f "$own_marker" ]]; then
            m_dev_ino=$(sed -n 's/^deploy_dev_ino=//p' "$own_marker" | head -n 1)
            m_ino=$(update_dev_ino_inode "$m_dev_ino")
            m_snap=$(sed -n 's/^snapshot_dir=//p' "$own_marker" | head -n 1)
            m_uid=$(sed -n 's/^owner_uid=//p' "$own_marker" | head -n 1)
            if [[ -z "$m_ino" || "$m_ino" == "$cur_ino" ]]; then
                local clear_cmd="bash scripts/update.sh --clear-recovery"
                if [[ "${m_uid:-}" == "0" || "$(update_path_uid "$own_marker" 2>/dev/null || echo "")" == "0" ]]; then
                    clear_cmd="sudo bash scripts/update.sh --clear-recovery"
                fi
                echo "[update] REFUSED: incomplete recovery marker $own_marker blocks update of $cur_canon (snapshot: ${m_snap:-$cand_root/$key}). Inspect the saved snapshot and manifest, verify the deployment, then run: $clear_cmd" >&2
                return 1
            fi
        fi
    done < <(update_candidate_recovery_roots "$deploy_dir" "$root")
    return 0
}

update_write_incomplete_marker() {
    local root="$1" key="$2" deploy_dir="$3" snap_dir="$4" reason="$5" stages_summary="$6"
    update_ensure_recovery_root "$root" || return 1
    local canon dev_ino ts marker_file root_uid root_gid marker_owner_uid="$EUID"
    canon=$(update_realpath "$deploy_dir")
    dev_ino=$(update_path_dev_ino "$canon" || true)
    ts=$(date -u +%Y-%m-%dT%H:%M:%SZ)
    root_uid=$(update_path_uid "$root" 2>/dev/null || echo "")
    root_gid=$(update_path_gid "$root" 2>/dev/null || echo "")
    if [[ -n "$root_uid" && "$root_uid" != "$EUID" ]]; then
        marker_owner_uid="$root_uid"
    fi
    marker_file="$root/${key}.incomplete"
    {
        printf 'deploy_path=%s\n' "$canon"
        printf 'deploy_dev_ino=%s\n' "$dev_ino"
        printf 'owner_uid=%s\n' "$marker_owner_uid"
        printf 'snapshot_dir=%s\n' "$snap_dir"
        printf 'manifest_path=%s/manifest.env\n' "$snap_dir"
        printf 'failed_at=%s\n' "$ts"
        printf 'failure_reason=%s\n' "$reason"
        printf 'stages=%s\n' "$stages_summary"
    } >"$marker_file"
    chmod 0644 "$marker_file" 2>/dev/null || true
    if [[ "$EUID" == "0" && -n "$root_uid" && "$root_uid" != "0" ]]; then
        chown "${root_uid}:${root_gid:-$root_uid}" "$marker_file" 2>/dev/null || true
    fi
    return 0
}

update_clear_recovery() {
    local deploy_dir="$1" owner_pid="${2:-$$}"
    local root key
    root=$(update_recovery_root)
    key=$(update_recovery_key "$deploy_dir")

    local cand_root cand_lock holder_pid
    while IFS= read -r cand_root; do
        [[ -n "$cand_root" ]] || continue
        cand_lock="$cand_root/${key}.lock"
        [[ -d "$cand_lock" ]] || continue
        holder_pid=$(update_lock_holder_pid "$cand_lock" || true)
        if [[ -z "$holder_pid" ]] || update_lock_is_live "$cand_lock" "$holder_pid"; then
            echo "[update] lock: another update is already running for $deploy_dir (lock $cand_lock held by pid ${holder_pid:-starting}; if stale, remove with: rm -rf $cand_lock)" >&2
            return 1
        fi
    done < <(update_candidate_recovery_roots "$deploy_dir" "$root")

    local -a target_roots=()
    local m_uid="" f_uid=""
    while IFS= read -r cand_root; do
        [[ -n "$cand_root" && -d "$cand_root" ]] || continue
        if [[ -f "$cand_root/${key}.incomplete" || -d "$cand_root/$key" || -f "$cand_root/${key}.state" ]]; then
            m_uid=$(sed -n 's/^owner_uid=//p' "$cand_root/${key}.incomplete" 2>/dev/null | head -n 1)
            f_uid=$(update_path_uid "$cand_root/${key}.incomplete" 2>/dev/null || update_path_uid "$cand_root/$key" 2>/dev/null || echo "")
            if [[ "$EUID" != "0" && ( ! -w "$cand_root" || "$m_uid" == "0" || "$f_uid" == "0" ) ]]; then
                echo "[update] recovery: REFUSED: incomplete recovery marker in $cand_root is owned by root; clear it with: sudo bash scripts/update.sh --clear-recovery" >&2
                return 1
            fi
            target_roots+=("$cand_root")
        fi
    done < <(update_candidate_recovery_roots "$deploy_dir" "$root")

    if [[ ${#target_roots[@]} -eq 0 ]]; then
        update_acquire_lock "$root" "$key" "$owner_pid" "$deploy_dir" || return $?
        update_release_lock "$root" "$key" "$owner_pid"
        echo "[update] recovery: cleared incomplete-recovery marker for $deploy_dir (no snapshot directory present)"
        return 0
    fi

    local ts cleared_dir="" any_moved="" r_uid="" r_gid=""
    ts=$(date -u +%Y%m%dT%H%M%SZ)
    for cand_root in "${target_roots[@]}"; do
        update_acquire_lock "$cand_root" "$key" "$owner_pid" "$deploy_dir" || return 1
        local snap_dir="$cand_root/$key"
        local marker_file="$cand_root/${key}.incomplete"
        if [[ -d "$snap_dir" ]]; then
            cleared_dir="$cand_root/${key}.cleared-${ts}"
            [[ -e "$cleared_dir" ]] && cleared_dir="${cleared_dir}-$$"
            mv -f "$snap_dir" "$cleared_dir" || {
                update_release_lock "$cand_root" "$key" "$owner_pid"
                return 1
            }
            rm -f "$cleared_dir/incomplete"
            r_uid=$(update_path_uid "$cand_root" 2>/dev/null || echo "")
            r_gid=$(update_path_gid "$cand_root" 2>/dev/null || echo "")
            if [[ "$EUID" == "0" && -n "$r_uid" && "$r_uid" != "0" ]]; then
                chown -R "${r_uid}:${r_gid:-$r_uid}" "$cleared_dir" 2>/dev/null || true
            fi
            any_moved="$cleared_dir"
        fi
        rm -f "$marker_file" "$cand_root/${key}.state"
        update_release_lock "$cand_root" "$key" "$owner_pid"
    done
    if [[ -n "$any_moved" ]]; then
        echo "[update] recovery: cleared incomplete-recovery marker for $deploy_dir; snapshot moved to $any_moved"
    else
        echo "[update] recovery: cleared incomplete-recovery marker for $deploy_dir (no snapshot directory present)"
    fi
    return 0
}

update_pause_if_requested() {
    local want="$1"
    local spec="${GO_TRADER_UPDATE_PAUSE_AT:-}"
    [[ -n "$spec" ]] || return 0
    local -a entries=()
    local entry stage sentinel
    IFS=',' read -r -a entries <<<"$spec"
    for entry in ${entries[@]+"${entries[@]}"}; do
        stage="${entry%%:*}"
        sentinel="${entry#*:}"
        if [[ "$stage" == "$want" && -n "$sentinel" && ! -e "$sentinel" ]]; then
            printf '%s\n' "${main_shell_pid:-$BASHPID}" >"${sentinel}.reached"
            while [[ ! -e "$sentinel" ]]; do
                sleep 0.1 &
                active_child_pid=$!
                wait "$active_child_pid" 2>/dev/null || true
                active_child_pid=""
            done
        fi
    done
}

update_should_fail_at() {
    local want="$1"
    local spec="${GO_TRADER_UPDATE_FAIL_AT:-${UPDATE_FAIL_AFTER:-}}"
    [[ -n "$spec" ]] || return 1
    local -a entries=()
    local tok
    IFS=',' read -r -a entries <<<"$spec"
    for tok in ${entries[@]+"${entries[@]}"}; do
        if [[ "$tok" == "$want" ]]; then
            if [[ -n "${GO_TRADER_UPDATE_FAIL_ONCE_DIR:-}" ]]; then
                mkdir -p "$GO_TRADER_UPDATE_FAIL_ONCE_DIR"
                if [[ -e "$GO_TRADER_UPDATE_FAIL_ONCE_DIR/$want" ]]; then
                    return 1
                fi
                : >"$GO_TRADER_UPDATE_FAIL_ONCE_DIR/$want"
            fi
            return 0
        fi
    done
    return 1
}

update_compute_tree_manifest() {
    local root_dir="$1" out_file="$2" live_check_dir="${3:-}"
    python3 - "$root_dir" "$out_file" "$live_check_dir" <<'PY'
import hashlib, os, stat, sys
root = os.path.abspath(sys.argv[1])
out_path = sys.argv[2]
live_dir = os.path.abspath(sys.argv[3]) if len(sys.argv) > 3 and sys.argv[3] else ""
live_inodes = set()
if live_dir:
    for cur, dirs, files in os.walk(live_dir, topdown=True, followlinks=False):
        for name in files:
            try:
                st = os.lstat(os.path.join(cur, name))
            except OSError:
                continue
            if stat.S_ISREG(st.st_mode):
                live_inodes.add((st.st_dev, st.st_ino))
rows = []
for cur, dirs, files in os.walk(root, topdown=True, followlinks=False):
    dirs.sort()
    files.sort()
    entries = sorted(set(dirs + files))
    rel_cur = os.path.relpath(cur, root)
    if rel_cur == ".":
        rel_cur = ""
    for name in entries:
        full = os.path.join(cur, name)
        rel = os.path.join(rel_cur, name) if rel_cur else name
        st = os.lstat(full)
        mode = stat.S_IMODE(st.st_mode)
        if stat.S_ISLNK(st.st_mode):
            rows.append("L\t%s\t%04o\t%d:%d\t%s" % (rel, mode, st.st_uid, st.st_gid, os.readlink(full)))
        elif stat.S_ISDIR(st.st_mode):
            rows.append("D\t%s\t%04o\t%d:%d\t-" % (rel, mode, st.st_uid, st.st_gid))
        elif stat.S_ISREG(st.st_mode):
            if live_inodes and (st.st_dev, st.st_ino) in live_inodes:
                sys.stderr.write("[update] snapshot: %s shares inode with live deployment file\n" % full)
                sys.exit(1)
            h = hashlib.sha256()
            with open(full, "rb") as f:
                while True:
                    b = f.read(65536)
                    if not b:
                        break
                    h.update(b)
            rows.append("F\t%s\t%04o\t%d:%d\t%s" % (rel, mode, st.st_uid, st.st_gid, h.hexdigest()))
with open(out_path, "w", encoding="utf-8") as f:
    for r in rows:
        f.write(r + "\n")
PY
}

update_validate_venv_topology() {
    local deploy_dir="$1"
    local venv_path="${deploy_dir%/}/.venv"
    if [[ -L "$venv_path" ]]; then
        echo "[update] snapshot: $venv_path is a symlink ($(readlink "$venv_path")); refusing to mutate an external virtual environment" >&2
        return 1
    fi
    if [[ -e "$venv_path" && ! -d "$venv_path" ]]; then
        echo "[update] snapshot: $venv_path exists and is not a directory" >&2
        return 1
    fi
    if [[ -d "$venv_path" ]]; then
        python3 - "$venv_path" <<'PY' || return 1
import os, stat, sys
venv = os.path.realpath(sys.argv[1])
for cur, dirs, files in os.walk(venv, topdown=True, followlinks=False):
    for d in list(dirs):
        full = os.path.join(cur, d)
        st = os.lstat(full)
        if stat.S_ISLNK(st.st_mode):
            target = os.path.realpath(full)
            if os.path.isdir(target) and target != venv and not target.startswith(venv.rstrip("/") + "/"):
                sys.stderr.write("[update] snapshot: %s in .venv links to external directory %s; refusing unsafe venv topology\n" % (full, target))
                sys.exit(1)
PY
    fi
    return 0
}

update_check_snapshot_space() {
    local deploy_dir="$1" recovery_root="$2"
    update_managed_rsync_excludes "$deploy_dir" || return 1
    local empty_dir stats_out
    empty_dir=$(mktemp -d "${TMPDIR:-/tmp}/go-trader-space-check.XXXXXX") || return 1
    if ! stats_out=$(rsync -a --dry-run --stats "${UPDATE_MANAGED_EXCLUDES[@]}" "$deploy_dir/" "$empty_dir/" 2>/dev/null); then
        rm -rf "$empty_dir"
        return 1
    fi
    rm -rf "$empty_dir"
    python3 - "$deploy_dir" "$recovery_root" "$stats_out" <<'PY'
import os, re, stat, sys
deploy = os.path.abspath(sys.argv[1])
rec_root = os.path.abspath(sys.argv[2])
stats_text = sys.argv[3]
m = re.search(r"Total file size:\s*([0-9,]+)", stats_text)
if not m:
    m = re.search(r"total size is\s*([0-9,]+)", stats_text)
total = int(m.group(1).replace(",", "")) if m else 0
venv_dir = os.path.join(deploy, ".venv")
if os.path.isdir(venv_dir) and not os.path.islink(venv_dir):
    for cur, dirs, files in os.walk(venv_dir, topdown=True, followlinks=False):
        for name in files:
            try:
                st = os.lstat(os.path.join(cur, name))
            except OSError:
                continue
            if stat.S_ISREG(st.st_mode) or stat.S_ISLNK(st.st_mode):
                total += st.st_size
for extra in ("go-trader", "go-trader.prev", os.path.join(".git", "index")):
    p = os.path.join(deploy, extra)
    try:
        st = os.lstat(p)
        if stat.S_ISREG(st.st_mode) or stat.S_ISLNK(st.st_mode):
            total += st.st_size
    except OSError:
        pass
required = total + (total + 9) // 10
if required < 4096:
    required = 4096
st_vfs = os.statvfs(rec_root)
free = st_vfs.f_bavail * st_vfs.f_frsize
fake_free = os.environ.get("GO_TRADER_UPDATE_FAKE_FREE_BYTES", "").strip()
if fake_free:
    free = int(fake_free)
print("[update] snapshot: size=%dB required=%dB (size+10%%) free=%dB in %s" % (total, required, free, rec_root))
if free < required:
    sys.stderr.write("[update] snapshot: insufficient free space in %s (need %dB including 10%% margin, have %dB)\n" % (rec_root, required, free))
    sys.exit(1)
PY
}

update_verify_no_hardlinks_to_live() {
    local live_dir="$1" snap_dir="$2"
    python3 - "$live_dir" "$snap_dir" <<'PY'
import os, stat, sys
live = os.path.abspath(sys.argv[1])
snap = os.path.abspath(sys.argv[2])
live_inodes = set()
for cur, dirs, files in os.walk(live, topdown=True, followlinks=False):
    for name in files:
        try:
            st = os.lstat(os.path.join(cur, name))
        except OSError:
            continue
        if stat.S_ISREG(st.st_mode):
            live_inodes.add((st.st_dev, st.st_ino))
for cur, dirs, files in os.walk(snap, topdown=True, followlinks=False):
    for name in files:
        full = os.path.join(cur, name)
        st = os.lstat(full)
        if stat.S_ISREG(st.st_mode) and (st.st_dev, st.st_ino) in live_inodes:
            sys.stderr.write("[update] snapshot: %s shares inode with live deployment file\n" % full)
            sys.exit(1)
PY
}

update_take_snapshot() {
    local deploy_dir="$1" snap_dir="$2" unit_source="$3" unit_installed="$4" egress_dep="$5" cpu_dropin_dest="${6:-}"
    update_managed_rsync_excludes "$deploy_dir" || return 1
    update_validate_venv_topology "$deploy_dir" || return 1
    local rec_root
    rec_root=$(dirname "$snap_dir")
    update_ensure_recovery_root "$rec_root" || return 1
    if update_should_fail_at snapshot_space; then
        echo "[update] snapshot: forced snapshot_space failure" >&2
        return 1
    fi
    update_check_snapshot_space "$deploy_dir" "$rec_root" || return 1

    local deploy_dev_ino="" deploy_ino=""
    deploy_dev_ino=$(update_path_dev_ino "$deploy_dir" || true)
    deploy_ino=$(update_dev_ino_inode "$deploy_dev_ino")
    if [[ -f "${snap_dir}.incomplete" ]]; then
        local inc_dev_ino inc_ino
        inc_dev_ino=$(sed -n 's/^deploy_dev_ino=//p' "${snap_dir}.incomplete" 2>/dev/null | head -n 1)
        inc_ino=$(update_dev_ino_inode "$inc_dev_ino")
        if [[ -z "$inc_ino" || "$inc_ino" == "$deploy_ino" ]]; then
            echo "[update] snapshot: refusing to overwrite unfinished snapshot at $snap_dir" >&2
            return 1
        fi
    fi
    if [[ -f "$snap_dir/COMPLETE" && -s "$snap_dir/intents.log" ]]; then
        local s_dev_ino s_ino
        s_dev_ino=$(sed -n 's/^deploy_dev_ino=//p' "$snap_dir/manifest.env" 2>/dev/null | tr -d "'\"\\" | head -n 1)
        s_ino=$(update_dev_ino_inode "$s_dev_ino")
        if [[ -z "$s_ino" || "$s_ino" == "$deploy_ino" ]]; then
            echo "[update] snapshot: refusing to overwrite unfinished snapshot at $snap_dir" >&2
            return 1
        fi
    fi
    rm -rf "$snap_dir" "${snap_dir}.state"
    mkdir -p "$snap_dir/managed" "$snap_dir/binaries" "$snap_dir/git" "$snap_dir/external" || return 1
    chmod 0700 "$snap_dir" "$snap_dir/managed" "$snap_dir/binaries" "$snap_dir/git" "$snap_dir/external" || return 1

    local -a snap_rsync_flags=(-a --delete)
    if [[ "$EUID" == "0" ]]; then
        snap_rsync_flags+=(--numeric-ids)
    fi
    rsync "${snap_rsync_flags[@]}" "${UPDATE_MANAGED_EXCLUDES[@]}" "$deploy_dir/" "$snap_dir/managed/" || return 1
    update_compute_tree_manifest "$snap_dir/managed" "$snap_dir/managed.manifest" "$deploy_dir" || return 1

    local had_go_trader=0 had_go_trader_prev=0
    if [[ -e "$deploy_dir/go-trader" || -L "$deploy_dir/go-trader" ]]; then
        had_go_trader=1
        cp -pP "$deploy_dir/go-trader" "$snap_dir/binaries/go-trader" || return 1
    fi
    if [[ -e "$deploy_dir/go-trader.prev" || -L "$deploy_dir/go-trader.prev" ]]; then
        had_go_trader_prev=1
        cp -pP "$deploy_dir/go-trader.prev" "$snap_dir/binaries/go-trader.prev" || return 1
    fi

    local had_venv=0 venv_exec=""
    if [[ -d "$deploy_dir/.venv" ]]; then
        had_venv=1
        mkdir -p "$snap_dir/venv" || return 1
        rsync "${snap_rsync_flags[@]}" "$deploy_dir/.venv/" "$snap_dir/venv/" || return 1
        update_compute_tree_manifest "$snap_dir/venv" "$snap_dir/venv.manifest" "$deploy_dir" || return 1
        if [[ -x "$deploy_dir/.venv/bin/python3" ]]; then
            venv_exec=$("$deploy_dir/.venv/bin/python3" -c 'import sys; print(sys.version.split()[0])' 2>/dev/null || true)
            printf '%s\n' "$venv_exec" >"$snap_dir/venv.exec"
        fi
    fi

    local git_head_sha="" git_head_ref="" git_head_symbolic=0 had_git_index=0 git_index_path="" git_head_path=""
    if update_git_top "$deploy_dir" >/dev/null 2>&1; then
        git_head_sha=$(update_git "$deploy_dir" rev-parse HEAD 2>/dev/null || echo "")
        git_head_ref=$(update_git "$deploy_dir" symbolic-ref -q HEAD 2>/dev/null || echo "")
        [[ -n "$git_head_ref" ]] && git_head_symbolic=1
        git_head_path=$(update_git "$deploy_dir" rev-parse --git-path HEAD 2>/dev/null || echo "")
        if [[ -n "$git_head_path" && "$git_head_path" != /* ]]; then
            git_head_path="$deploy_dir/$git_head_path"
        fi
        if [[ -n "$git_head_path" && -f "$git_head_path" ]]; then
            cp -p "$git_head_path" "$snap_dir/git/HEAD" || return 1
        fi
        git_index_path=$(update_git "$deploy_dir" rev-parse --git-path index 2>/dev/null || echo "")
        if [[ -n "$git_index_path" && "$git_index_path" != /* ]]; then
            git_index_path="$deploy_dir/$git_index_path"
        fi
        if [[ -n "$git_index_path" && -f "$git_index_path" ]]; then
            had_git_index=1
            cp -p "$git_index_path" "$snap_dir/git/index" || return 1
        fi
    fi

    local had_unit_file=0 had_unit_prev=0
    if [[ -n "$unit_installed" ]]; then
        if [[ -e "$unit_installed" || -L "$unit_installed" ]]; then
            had_unit_file=1
            cp -pP "$unit_installed" "$snap_dir/external/unit.service" || return 1
        fi
        if [[ -e "${unit_installed}.prev" || -L "${unit_installed}.prev" ]]; then
            had_unit_prev=1
            cp -pP "${unit_installed}.prev" "$snap_dir/external/unit.service.prev" || return 1
        fi
    fi

    local journal_ns="" journal_conf_dest="" had_journal_conf=0 had_journal_conf_prev=0
    if [[ -n "$unit_source" ]]; then
        journal_ns=$(update_unit_log_namespace "$unit_source")
        if [[ -n "$journal_ns" && "$journal_ns" =~ ^[A-Za-z0-9_-]+$ ]]; then
            local journal_etc="${UPDATE_JOURNALD_ETC_DIR:-/etc/systemd}"
            journal_conf_dest=$(update_journald_conf_path "$journal_etc" "$journal_ns")
            if [[ -e "$journal_conf_dest" || -L "$journal_conf_dest" ]]; then
                had_journal_conf=1
                cp -pP "$journal_conf_dest" "$snap_dir/external/journald.conf" || return 1
            fi
            if [[ -e "${journal_conf_dest}.prev" || -L "${journal_conf_dest}.prev" ]]; then
                had_journal_conf_prev=1
                cp -pP "${journal_conf_dest}.prev" "$snap_dir/external/journald.conf.prev" || return 1
            fi
        fi
    fi

    local egress_cur="" had_egress_controller=0 had_egress_controller_prev=0
    if [[ -n "$egress_dep" ]]; then
        egress_cur=$(update_egress_controller_path "$egress_dep")
        if [[ -f "$egress_cur" ]]; then
            had_egress_controller=1
            cp -p "$egress_cur" "$snap_dir/external/egress-controller" || return 1
        fi
        if [[ -f "${egress_cur}.prev" ]]; then
            had_egress_controller_prev=1
            cp -p "${egress_cur}.prev" "$snap_dir/external/egress-controller.prev" || return 1
        fi
    fi

    local had_cpu_dropin=0
    if [[ -n "$cpu_dropin_dest" ]] && [[ -e "$cpu_dropin_dest" || -L "$cpu_dropin_dest" ]]; then
        had_cpu_dropin=1
        cp -pP "$cpu_dropin_dest" "$snap_dir/external/cpu-dropin.conf" || return 1
    fi

    local cfg_status_port="${UPDATE_CONFIG_STATUS_PORT:-8099}"

    if update_should_fail_at snapshot_copy; then
        echo "[update] snapshot: forced snapshot_copy failure" >&2
        return 1
    fi

    {
        printf 'deploy_dev_ino=%q\n' "$deploy_dev_ino"
        printf 'had_go_trader=%q\n' "$had_go_trader"
        printf 'had_go_trader_prev=%q\n' "$had_go_trader_prev"
        printf 'had_venv=%q\n' "$had_venv"
        printf 'git_head_sha=%q\n' "$git_head_sha"
        printf 'git_head_ref=%q\n' "$git_head_ref"
        printf 'git_head_symbolic=%q\n' "$git_head_symbolic"
        printf 'had_git_index=%q\n' "$had_git_index"
        printf 'git_index_path=%q\n' "$git_index_path"
        printf 'unit_installed=%q\n' "$unit_installed"
        printf 'had_unit_file=%q\n' "$had_unit_file"
        printf 'had_unit_prev=%q\n' "$had_unit_prev"
        printf 'journal_ns=%q\n' "$journal_ns"
        printf 'journal_conf_dest=%q\n' "$journal_conf_dest"
        printf 'had_journal_conf=%q\n' "$had_journal_conf"
        printf 'had_journal_conf_prev=%q\n' "$had_journal_conf_prev"
        printf 'egress_dep=%q\n' "$egress_dep"
        printf 'egress_cur=%q\n' "$egress_cur"
        printf 'had_egress_controller=%q\n' "$had_egress_controller"
        printf 'had_egress_controller_prev=%q\n' "$had_egress_controller_prev"
        printf 'cpu_dropin_dest=%q\n' "$cpu_dropin_dest"
        printf 'had_cpu_dropin=%q\n' "$had_cpu_dropin"
        printf 'cfg_status_port=%q\n' "$cfg_status_port"
    } >"$snap_dir/manifest.env"
    update_pause_if_requested snapshot_before_complete
    : >"$snap_dir/COMPLETE"
    {
        printf 'deploy_dev_ino=%s\n' "$deploy_dev_ino"
        printf 'owner_uid=%s\n' "$EUID"
        printf 'complete=1\n'
    } >"${snap_dir}.state"
    chmod 0644 "${snap_dir}.state" 2>/dev/null || true
    return 0
}

update_restore_snapshot_files() {
    local deploy_dir="$1" snap_dir="$2" is_rsync_mode="$3"
    [[ -f "$snap_dir/COMPLETE" && -f "$snap_dir/manifest.env" ]] || return 1
    if update_should_fail_at restore_files; then
        echo "[update] recovery: forced restore_files failure" >&2
        return 1
    fi
    update_managed_rsync_excludes "$deploy_dir" || return 1
    source "$snap_dir/manifest.env"
    local -a restore_rsync_flags=(-a -c --delete)
    if [[ "$EUID" == "0" ]]; then
        restore_rsync_flags+=(--numeric-ids)
    fi
    rsync "${restore_rsync_flags[@]}" "${UPDATE_MANAGED_EXCLUDES[@]}" "$snap_dir/managed/" "$deploy_dir/" || return 1

    if [[ "$is_rsync_mode" != "1" && -n "$git_head_sha" ]]; then
        if [[ "$git_head_symbolic" == "1" && -n "$git_head_ref" ]]; then
            update_git "$deploy_dir" symbolic-ref HEAD "$git_head_ref" || return 1
            update_git "$deploy_dir" update-ref "$git_head_ref" "$git_head_sha" || return 1
        else
            update_git "$deploy_dir" update-ref --no-deref HEAD "$git_head_sha" || return 1
        fi
        if [[ -n "$git_index_path" ]]; then
            if [[ "$had_git_index" == "1" && -f "$snap_dir/git/index" ]]; then
                cp -p "$snap_dir/git/index" "$git_index_path" || return 1
            else
                rm -f "$git_index_path"
            fi
        fi
        local cur_sha
        cur_sha=$(update_git "$deploy_dir" rev-parse HEAD 2>/dev/null || echo "")
        [[ "$cur_sha" == "$git_head_sha" ]] || return 1
        if [[ "$had_git_index" == "1" && -n "$git_index_path" ]]; then
            [[ "$(update_file_fingerprint "$git_index_path")" == "$(update_file_fingerprint "$snap_dir/git/index")" ]] || return 1
        fi
    elif [[ "$is_rsync_mode" == "1" && -n "$git_head_sha" ]]; then
        local cur_sha
        cur_sha=$(update_git "$deploy_dir" rev-parse HEAD 2>/dev/null || echo "")
        [[ "$cur_sha" == "$git_head_sha" ]] || return 1
    fi
    return 0
}

update_restore_snapshot_venv() {
    local deploy_dir="$1" snap_dir="$2"
    [[ -f "$snap_dir/COMPLETE" && -f "$snap_dir/manifest.env" ]] || return 1
    if update_should_fail_at restore_venv; then
        echo "[update] recovery: forced restore_venv failure" >&2
        return 1
    fi
    source "$snap_dir/manifest.env"
    rm -rf "$deploy_dir/.venv.restore-new" "$deploy_dir/.venv.restore-old"
    if [[ "$had_venv" == "1" ]]; then
        mkdir -p "$deploy_dir/.venv.restore-new" || return 1
        local -a venv_rsync_flags=(-a --delete)
        if [[ "$EUID" == "0" ]]; then
            venv_rsync_flags+=(--numeric-ids)
        fi
        if ! rsync "${venv_rsync_flags[@]}" "$snap_dir/venv/" "$deploy_dir/.venv.restore-new/"; then
            rm -rf "$deploy_dir/.venv.restore-new"
            return 1
        fi
        if [[ -e "$deploy_dir/.venv" || -L "$deploy_dir/.venv" ]]; then
            mv -f "$deploy_dir/.venv" "$deploy_dir/.venv.restore-old" || {
                rm -rf "$deploy_dir/.venv.restore-new"
                return 1
            }
            update_pause_if_requested restore_venv_moved_old
        fi
        if ! mv -f "$deploy_dir/.venv.restore-new" "$deploy_dir/.venv"; then
            if [[ -e "$deploy_dir/.venv.restore-old" || -L "$deploy_dir/.venv.restore-old" ]]; then
                mv -f "$deploy_dir/.venv.restore-old" "$deploy_dir/.venv" 2>/dev/null || true
            fi
            return 1
        fi
        rm -rf "$deploy_dir/.venv.restore-old"
        local post_manifest="$snap_dir/venv.post.manifest"
        update_compute_tree_manifest "$deploy_dir/.venv" "$post_manifest" || return 1
        cmp -s "$snap_dir/venv.manifest" "$post_manifest" || return 1
        if [[ -f "$snap_dir/venv.exec" && -s "$snap_dir/venv.exec" ]]; then
            local want_ver got_ver
            want_ver=$(cat "$snap_dir/venv.exec")
            got_ver=$("$deploy_dir/.venv/bin/python3" -c 'import sys; print(sys.version.split()[0])' 2>/dev/null || true)
            [[ "$got_ver" == "$want_ver" ]] || return 1
        fi
    else
        rm -rf "$deploy_dir/.venv"
        [[ ! -e "$deploy_dir/.venv" && ! -L "$deploy_dir/.venv" ]] || return 1
    fi
    return 0
}

update_restore_snapshot_binaries() {
    local deploy_dir="$1" snap_dir="$2"
    [[ -f "$snap_dir/COMPLETE" && -f "$snap_dir/manifest.env" ]] || return 1
    if update_should_fail_at restore_binaries; then
        echo "[update] recovery: forced restore_binaries failure" >&2
        return 1
    fi
    source "$snap_dir/manifest.env"
    rm -f "$deploy_dir/go-trader.new" "$deploy_dir/go-trader.restore-tmp" "$deploy_dir/go-trader.prev.restore-tmp"
    if [[ "$had_go_trader" == "1" ]]; then
        cp -pP "$snap_dir/binaries/go-trader" "$deploy_dir/go-trader.restore-tmp" || return 1
        mv -f "$deploy_dir/go-trader.restore-tmp" "$deploy_dir/go-trader" || return 1
        [[ "$(update_file_fingerprint "$deploy_dir/go-trader")" == "$(update_file_fingerprint "$snap_dir/binaries/go-trader")" ]] || return 1
    else
        rm -f "$deploy_dir/go-trader"
        [[ ! -e "$deploy_dir/go-trader" && ! -L "$deploy_dir/go-trader" ]] || return 1
    fi
    if [[ "$had_go_trader_prev" == "1" ]]; then
        cp -pP "$snap_dir/binaries/go-trader.prev" "$deploy_dir/go-trader.prev.restore-tmp" || return 1
        mv -f "$deploy_dir/go-trader.prev.restore-tmp" "$deploy_dir/go-trader.prev" || return 1
        [[ "$(update_file_fingerprint "$deploy_dir/go-trader.prev")" == "$(update_file_fingerprint "$snap_dir/binaries/go-trader.prev")" ]] || return 1
    else
        rm -f "$deploy_dir/go-trader.prev"
        [[ ! -e "$deploy_dir/go-trader.prev" && ! -L "$deploy_dir/go-trader.prev" ]] || return 1
    fi
    return 0
}

update_restore_snapshot_external() {
    local snap_dir="$1" unit_mut="$2" unit_rel="$3" journal_mut="$4" egress_mut="$5" egress_state_file="$6" cpu_mut="${7:-0}"
    [[ -f "$snap_dir/COMPLETE" && -f "$snap_dir/manifest.env" ]] || return 1
    if update_should_fail_at restore_external; then
        echo "[update] recovery: forced restore_external failure" >&2
        return 1
    fi
    local cpu_dropin_dest="" had_cpu_dropin=0
    source "$snap_dir/manifest.env"
    local ext_ok=1

    if [[ "$unit_mut" == "1" && -n "$unit_installed" ]]; then
        if [[ "$had_unit_file" == "1" ]]; then
            if ! update_unit_sudo cp -pP "$snap_dir/external/unit.service" "$unit_installed"; then
                echo "[update] recovery: could not restore unit file $unit_installed" >&2
                ext_ok=0
            fi
        else
            update_unit_sudo rm -f "$unit_installed" || ext_ok=0
        fi
        if [[ "$had_unit_prev" == "1" ]]; then
            update_unit_sudo cp -pP "$snap_dir/external/unit.service.prev" "${unit_installed}.prev" || ext_ok=0
        else
            update_unit_sudo rm -f "${unit_installed}.prev" || true
        fi
    fi
    if [[ "$cpu_mut" == "1" && -n "$cpu_dropin_dest" ]]; then
        if [[ "$had_cpu_dropin" == "1" ]]; then
            if ! update_unit_sudo install -d -m 0755 "${cpu_dropin_dest%/*}" \
                || ! update_unit_sudo cp -pP "$snap_dir/external/cpu-dropin.conf" "$cpu_dropin_dest"; then
                echo "[update] recovery: could not restore CPU weight drop-in $cpu_dropin_dest" >&2
                ext_ok=0
            fi
        else
            if ! update_unit_sudo rm -f "$cpu_dropin_dest"; then
                echo "[update] recovery: could not remove CPU weight drop-in $cpu_dropin_dest" >&2
                ext_ok=0
            fi
            update_unit_sudo rmdir "${cpu_dropin_dest%/*}" 2>/dev/null || true
        fi
    fi
    if [[ "$unit_mut" == "1" || "$unit_rel" == "1" || "$cpu_mut" == "1" ]]; then
        if ! update_unit_sudo systemctl daemon-reload; then
            echo "[update] recovery: systemctl daemon-reload failed after unit restore" >&2
            ext_ok=0
        fi
    fi

    if [[ "$journal_mut" == "1" && -n "$journal_conf_dest" ]]; then
        if [[ "$had_journal_conf" == "1" ]]; then
            if ! update_unit_sudo cp -pP "$snap_dir/external/journald.conf" "$journal_conf_dest"; then
                echo "[update] recovery: could not restore journald config $journal_conf_dest" >&2
                ext_ok=0
            fi
        else
            update_unit_sudo rm -f "$journal_conf_dest" || ext_ok=0
        fi
        if [[ "$had_journal_conf_prev" == "1" ]]; then
            update_unit_sudo cp -pP "$snap_dir/external/journald.conf.prev" "${journal_conf_dest}.prev" || ext_ok=0
        else
            update_unit_sudo rm -f "${journal_conf_dest}.prev" || true
        fi
        if [[ -n "$journal_ns" ]]; then
            if ! update_unit_sudo systemctl try-restart "systemd-journald@${journal_ns}.service"; then
                echo "[update] recovery: systemd-journald@${journal_ns}.service try-restart failed" >&2
                ext_ok=0
            fi
        fi
    fi

    if [[ "$egress_mut" == "1" && -n "$egress_dep" ]]; then
        local need_ctrl_restart=1
        if [[ -n "$egress_state_file" && -f "$egress_state_file" ]]; then
            local restarted=""
            restarted=$(sed -n 's/^restarted=//p' "$egress_state_file" | head -n 1)
            [[ "$restarted" == "0" ]] && need_ctrl_restart=0
        fi
        local snap_ctrl="" snap_ctrl_prev=""
        [[ "$had_egress_controller" == "1" ]] && snap_ctrl="$snap_dir/external/egress-controller"
        [[ "$had_egress_controller_prev" == "1" ]] && snap_ctrl_prev="$snap_dir/external/egress-controller.prev"
        if ! update_egress_restore_controller "$egress_dep" "$snap_ctrl" "$snap_ctrl_prev" "$need_ctrl_restart"; then
            echo "[update] recovery: could not restore egress controller for $egress_dep" >&2
            ext_ok=0
        fi
    fi

    [[ "$ext_ok" == "1" ]]
}

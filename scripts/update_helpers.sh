# shellcheck shell=bash
# Pure helpers sourced by update.sh (and test_update_helpers.sh).

# Emit the filesystem path to check for one EnvironmentFiles line from systemctl show.
# Returns empty when the entry is optional (-prefix), malformed, or only metadata.
update_systemd_envfile_check_path() {
    local entry="$1"
    local path="$entry"

    [[ -n "$path" ]] || return 0
    if [[ "$path" == '('* ]]; then
        return 0
    fi
    # EnvironmentFile=-/path — operator declared missing file tolerable.
    if [[ "$path" == -* ]]; then
        return 0
    fi
    if [[ "$path" == *' (ignore_errors='* ]]; then
        path="${path%% (ignore_errors=*}"
    fi
    [[ -n "$path" ]] || return 0
    printf '%s' "$path"
}

# Read EnvironmentFiles lines on stdin; warn on stderr for required missing paths.
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

# Decide whether signal-mode restart must be redirected to systemctl to avoid an
# out-of-cgroup duplicate (#850). Inputs (pre-resolved by the caller):
#   is_active      — `systemctl is-active <unit>` output ("active" when running)
#   exec_bin_abs   — canonicalized ExecStart binary path for the unit
#   swap_bin_abs   — canonicalized swap-target binary (this deployment's ./go-trader)
# Echoes "redirect" only when the unit is active AND its ExecStart binary matches
# this deployment's binary (so a sibling worktree's active unit does not redirect a
# legitimate signal-mode restart of a different instance); echoes "" otherwise.
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

# Pure predicate for the rollback stray-process sweep (#850): should a candidate
# pid be SIGTERM'd as a leftover of THIS instance? Matches a go-trader process
# whose working directory is this deployment dir (i.e. it shares this instance's
# state DB), which catches a failed new process surviving on a fallback port.
# cwd-matching deliberately spares other worktrees' traders. Echoes "sweep" or "".
update_should_sweep_proc() {
    local comm="$1" pid_cwd="$2" repo_abs="$3"
    [[ "$comm" == "go-trader" ]] || { printf ''; return 0; }
    [[ -n "$repo_abs" && "$pid_cwd" == "$repo_abs" ]] || { printf ''; return 0; }
    printf 'sweep'
}

# Classify a config path for the out-of-tree migration (#1056). Echoes:
#   symlink  — already a symlink (migration done; idempotent no-op). Checked
#              FIRST so a DANGLING symlink (target moved/removed) still reports
#              'symlink', never 'missing' — re-migrating would clobber the live
#              config pointer.
#   regular  — a real file still in the deployment tree (needs migrating)
#   missing  — nothing there
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

# Static, extension-based DB rsync excludes (#1012). Emits one glob per line so
# any .db / SQLite sidecar / lock file at ANY path survives --rsync-from's
# --delete, independent of the config-resolved db_file. These globs are
# unanchored (no leading slash), so rsync matches them at every directory depth.
# Defense-in-depth: run_rsync_from still adds the config-resolved db_excl for
# DBs whose name doesn't end in .db. Keep in sync with .gitignore's *.db family.
update_db_rsync_excludes() {
    printf '%s\n' '*.db' '*.db-wal' '*.db-shm' '*.db.lock'
}

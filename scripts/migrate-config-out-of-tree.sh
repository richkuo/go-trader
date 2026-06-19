#!/usr/bin/env bash
# #1056: Move a deployment's runtime config out of the deploy tree so no
# rsync/git-clean/rm operation can ever reach it, then leave a transition
# symlink behind so existing tooling (update.sh, ad-hoc --config-less runs)
# keeps working.
#
#   before:  <deploy>/scheduler/config.json            (real file, in the tree)
#   after:   /var/lib/go-trader[/<instance>]/config.json   (real file, out of tree)
#            <deploy>/scheduler/config.json -> <target>     (symlink)
#
# This script ONLY moves the file and creates the symlink. It does NOT edit live
# systemd units — it prints the exact edits to apply afterward (the daemon must
# be launched with `--config <target>` and the new dir made writable, which the
# shipped units do via StateDirectory=go-trader[/%i]).
#
# Idempotent: a config that is already a symlink is left untouched.
#
# STOP THE SERVICE FIRST: the script refuses to run while this deployment's
# daemon is live (it would clobber the new symlink on the next config write or
# crash-restart). Stop the unit, migrate, re-point ExecStart, then start.
#
# Usage:
#   scripts/migrate-config-out-of-tree.sh                     # cwd deploy -> /var/lib/go-trader/config.json
#   scripts/migrate-config-out-of-tree.sh --instance live     # -> /var/lib/go-trader/live/config.json
#   scripts/migrate-config-out-of-tree.sh --deploy-dir /opt/go-trader-live --instance live
#   scripts/migrate-config-out-of-tree.sh --base /etc/go-trader --instance live   # prints ReadWritePaths form
#   scripts/migrate-config-out-of-tree.sh --owner go-trader:go-trader
#   scripts/migrate-config-out-of-tree.sh --dry-run
#   scripts/migrate-config-out-of-tree.sh --force            # skip the running-daemon refusal (you stopped it yourself)
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=update_helpers.sh
source "${SCRIPT_DIR}/update_helpers.sh"

deploy_dir="$(pwd)"
instance=""
base="/var/lib/go-trader"
owner=""
dry_run=0
force=0

trim_space() { local s="$1"; s="${s#"${s%%[![:space:]]*}"}"; s="${s%"${s##*[![:space:]]}"}"; printf '%s' "$s"; }

while [[ $# -gt 0 ]]; do
    case "$1" in
        --deploy-dir) deploy_dir="$(trim_space "${2:-}")"; shift 2 ;;
        --deploy-dir=*) deploy_dir="$(trim_space "${1#*=}")"; shift ;;
        --instance) instance="$(trim_space "${2:-}")"; shift 2 ;;
        --instance=*) instance="$(trim_space "${1#*=}")"; shift ;;
        --base) base="$(trim_space "${2:-}")"; shift 2 ;;
        --base=*) base="$(trim_space "${1#*=}")"; shift ;;
        --owner) owner="$(trim_space "${2:-}")"; shift 2 ;;
        --owner=*) owner="$(trim_space "${1#*=}")"; shift ;;
        --dry-run) dry_run=1; shift ;;
        --force) force=1; shift ;;
        -h|--help)
            sed -n '2,30p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
            exit 0 ;;
        *) echo "error: unknown argument: $1" >&2; exit 2 ;;
    esac
done

# systemd instance names flow into paths and unit names. Reject not just stray
# characters but '.'/'..' (escape the target dir) and leading '-' (misparses as
# a flag) — see update_validate_instance_name.
if [[ -n "$instance" && "$(update_validate_instance_name "$instance")" != "ok" ]]; then
    echo "error: --instance must be a simple name (alphanumerics, dash, dot, underscore; not '.'/'..'; no leading dash). Got: $instance" >&2
    exit 2
fi

if [[ ! -d "$deploy_dir" ]]; then
    echo "error: --deploy-dir is not a directory: $deploy_dir" >&2
    exit 2
fi
deploy_dir=$(cd "$deploy_dir" && pwd)

# Refuse to migrate while THIS deployment's daemon is still running (#1060
# review): its --config still points at scheduler/config.json, which we are
# about to turn into a symlink. A config write (UI tuner, Discord /config) or a
# Restart=always crash-restart that re-runs startup migration in the window
# before the unit is re-pointed does os.Rename(tmp, configPath) — and rename(2)
# over a symlink REPLACES the symlink with a real file in the tree, orphaning
# the moved copy and losing that write. Detect the daemon by working directory
# (== deploy_dir, where the symlink lives) via the existing tested predicate.
# Best-effort: needs /proc, so non-Linux/undetectable falls through to the
# stop-first guidance printed at the end. --force overrides.
if [[ "$force" -ne 1 ]] && command -v pgrep >/dev/null 2>&1; then
    running_pid=""
    while IFS= read -r pid; do
        [[ -n "$pid" ]] || continue
        pcwd=$(readlink "/proc/$pid/cwd" 2>/dev/null || true)
        if [[ "$(update_should_sweep_proc go-trader "$pcwd" "$deploy_dir")" == "sweep" ]]; then
            running_pid="$pid"
            break
        fi
    done < <(pgrep -x go-trader 2>/dev/null || true)
    if [[ -n "$running_pid" ]]; then
        echo "error: a go-trader daemon (pid $running_pid) is running in $deploy_dir." >&2
        echo "       Stop it before migrating, or a config write/crash-restart during the" >&2
        echo "       migrate->restart window will clobber the new symlink back into an" >&2
        echo "       in-tree file and lose that write:" >&2
        echo "         sudo systemctl stop <unit>   # then re-run this script" >&2
        echo "       Re-run with --force only if you have already stopped the service." >&2
        exit 1
    fi
fi

src="$deploy_dir/scheduler/config.json"
if [[ -n "$instance" ]]; then
    target_dir="$base/$instance"
else
    target_dir="$base"
fi
target="$target_dir/config.json"

state=$(update_config_migration_state "$src")
case "$state" in
    symlink)
        link_dest=$(readlink "$src" 2>/dev/null || true)
        echo "[migrate] already migrated: $src -> ${link_dest:-<unreadable>} (no-op)"
        exit 0 ;;
    missing)
        echo "error: no config at $src — run from a deployment directory (or pass --deploy-dir)" >&2
        exit 1 ;;
    regular)
        : ;;  # proceed
esac

# Never clobber anything at the target (another instance's config, or a
# half-finished prior run). Refuse on any existing path — even a broken symlink.
if [[ -e "$target" || -L "$target" ]]; then
    echo "error: target already exists: $target — refusing to overwrite. Inspect and move it aside manually." >&2
    exit 1
fi

cmd() {
    echo "  + $*"
    [[ "$dry_run" -eq 1 ]] && return 0
    "$@"
}

echo "[migrate] $src  ->  $target"
[[ "$dry_run" -eq 1 ]] && echo "[migrate] DRY RUN — no changes will be made"

cmd mkdir -p "$target_dir"
cmd mv "$src" "$target"
cmd ln -s "$target" "$src"
if [[ -n "$owner" ]]; then
    # Best-effort: systemd StateDirectory re-asserts ownership on next start,
    # so a chown failure here (e.g. not root) is non-fatal.
    cmd chown "$owner" "$target_dir" "$target" || echo "[migrate] warning: chown $owner failed (non-fatal; StateDirectory will fix it on restart)" >&2
fi

# Base-aware: StateDirectory only works for /var/lib bases; any other base must
# use ReadWritePaths or the daemon points at a never-made-writable dir (#1060).
writable_directive=$(update_config_writable_directive "$base" "$instance")

cat <<EOF

[migrate] Done. Now update the systemd unit for this instance:

  ExecStart=... /go-trader --config $target
  $writable_directive    # makes $target_dir writable under ProtectSystem=strict

Then, with the service STOPPED for the whole window (so no config write or
Restart=always crash-restart turns the new symlink back into an in-tree file):

  sudo systemctl stop <unit>      # if not already stopped
  sudo systemctl daemon-reload
  sudo systemctl start <unit>

The transition symlink ($src) keeps update.sh and ad-hoc runs working. Drop it
only after update.sh learns the new path (#1056 follow-up).
EOF

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
# Usage:
#   scripts/migrate-config-out-of-tree.sh                     # cwd deploy -> /var/lib/go-trader/config.json
#   scripts/migrate-config-out-of-tree.sh --instance live     # -> /var/lib/go-trader/live/config.json
#   scripts/migrate-config-out-of-tree.sh --deploy-dir /opt/go-trader-live --instance live
#   scripts/migrate-config-out-of-tree.sh --base /etc/go-trader --instance live
#   scripts/migrate-config-out-of-tree.sh --owner go-trader:go-trader
#   scripts/migrate-config-out-of-tree.sh --dry-run
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=update_helpers.sh
source "${SCRIPT_DIR}/update_helpers.sh"

deploy_dir="$(pwd)"
instance=""
base="/var/lib/go-trader"
owner=""
dry_run=0

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
        -h|--help)
            sed -n '2,30p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
            exit 0 ;;
        *) echo "error: unknown argument: $1" >&2; exit 2 ;;
    esac
done

# systemd instance names flow into paths and unit names — reject anything that
# could escape the intended directory or confuse systemctl.
if [[ -n "$instance" && "$instance" =~ [^a-zA-Z0-9_.-] ]]; then
    echo "error: --instance must contain only alphanumerics, dash, dot, or underscore (got: $instance)" >&2
    exit 2
fi

if [[ ! -d "$deploy_dir" ]]; then
    echo "error: --deploy-dir is not a directory: $deploy_dir" >&2
    exit 2
fi
deploy_dir=$(cd "$deploy_dir" && pwd)

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

cat <<EOF

[migrate] Done. Now update the systemd unit for this instance:

  ExecStart=... /go-trader --config $target
  StateDirectory=go-trader${instance:+/$instance}    # makes $target_dir writable under ProtectSystem=strict

Then:

  sudo systemctl daemon-reload
  sudo systemctl restart <unit>

The transition symlink ($src) keeps update.sh and ad-hoc runs working. Drop it
only after update.sh learns the new path (#1056 follow-up).
EOF

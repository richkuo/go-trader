#!/usr/bin/env bash
# Atomic update with pre-flight probe, staging build, atomic binary swap,
# post-restart verification, and rollback (#682).
#
# Phases:
#   preflight  — git/uv/go sanity checks
#   pull       — git pull --ff-only
#   sync       — uv sync
#   build      — go build to go-trader.new (live binary untouched)
#   probe      — go-trader.new probe against the just-synced Python
#   swap       — atomic mv: live binary -> .prev, staged -> live
#   restart    — sudo systemctl restart <unit> (only with --restart)
#   verify     — wait active + /health version match (only with --restart)
#   rollback   — restore .prev on verify failure (only with --restart)
#
# Use --restart (or RESTART=1) to restart the systemd unit after a successful build
# AND verify the running process matches the just-built version. Without
# --restart the script stops after swap; the caller (scheduler/updater.go's
# applyUpgrade) handles its own restart via restartSelf.

set -euo pipefail

trim_space() {
    local value="$1"
    value="${value#"${value%%[![:space:]]*}"}"
    value="${value%"${value##*[![:space:]]}"}"
    printf '%s' "$value"
}

restart=0
service_unit="$(trim_space "${GO_TRADER_SERVICE:-}")"
if [[ -z "$service_unit" ]]; then
    service_unit="go-trader"
fi
while [[ $# -gt 0 ]]; do
    case "$1" in
        --restart)
            restart=1
            shift
            ;;
        --unit|--service)
            if [[ $# -lt 2 ]]; then
                echo "$1 requires a systemd unit name" >&2
                exit 2
            fi
            unit_arg="$(trim_space "$2")"
            if [[ -z "$unit_arg" || "$unit_arg" == --* ]]; then
                echo "$1 requires a systemd unit name" >&2
                exit 2
            fi
            service_unit="$unit_arg"
            shift 2
            ;;
        --unit=*|--service=*)
            unit_arg="$(trim_space "${1#*=}")"
            if [[ -z "$unit_arg" || "$unit_arg" == --* ]]; then
                echo "${1%%=*} requires a systemd unit name" >&2
                exit 2
            fi
            service_unit="$unit_arg"
            shift
            ;;
        -h|--help)
            echo "Usage: $0 [--restart] [--unit <systemd-unit>]"
            echo "       $0 [--restart] [--service <systemd-unit>]"
            echo "  RESTART=1 env var also enables restart."
            echo ""
            echo "Env overrides:"
            echo "  GO_TRADER_SERVICE=<unit> systemd unit to restart/verify (default: go-trader)"
            echo "  STATUS_PORT=<n>          override /health port (default: read from config, else 8099)"
            echo "  ACTIVE_TIMEOUT=<sec>     systemctl is-active poll timeout (default: 30)"
            echo "  HEALTH_TIMEOUT=<sec>     /health version-match poll timeout (default: 60)"
            exit 0
            ;;
        *)
            echo "unknown arg: $1" >&2
            exit 2
            ;;
    esac
done
if [[ "${RESTART:-0}" == "1" ]]; then
    restart=1
fi

active_timeout="${ACTIVE_TIMEOUT:-30}"
# 60s default because go-trader startup runs probeCheckScripts (every unique
# check script with --probe-only) BEFORE the status server starts, so installs
# with several distinct platforms can legitimately exceed a tighter bound and
# trigger a spurious rollback.
health_timeout="${HEALTH_TIMEOUT:-60}"

# Phase logging — prefix every milestone with `[update] phase: <name>` and
# emit elapsed seconds on phase end so a slow update can be localized in
# journal/DM output. begin_phase records start; end_phase prints duration.
phase_name=""
phase_start=0
begin_phase() {
    phase_name="$1"
    phase_start=$SECONDS
    echo "[update] phase: $phase_name"
}
end_phase() {
    local elapsed=$((SECONDS - phase_start))
    echo "[update] phase: $phase_name done (${elapsed}s)"
}
fail() {
    # Mark which phase failed before exiting non-zero — operator and the
    # auto-update DM see the failing phase first instead of having to
    # reverse-engineer it from stderr.
    echo "[update] FAIL phase=$phase_name: $*" >&2
    exit 1
}

# Phase 1: pre-flight — every check that should abort BEFORE any mutation.
begin_phase preflight

if ! command -v uv >/dev/null 2>&1; then
    fail "uv not on PATH — install uv first (see CLAUDE.md → Setup)"
fi

go_bin=""
if command -v go >/dev/null 2>&1; then
    go_bin=$(command -v go)
elif [[ -x /opt/homebrew/bin/go ]]; then
    go_bin=/opt/homebrew/bin/go
else
    fail "go not on PATH and /opt/homebrew/bin/go missing"
fi

repo_root=$(git rev-parse --show-toplevel)
cd "$repo_root"

# scheduler/config.json is gitignored, so a bare source clone never has one.
# Without it, `go-trader.new probe` later loads no strategies and fails with
# "read config: open scheduler/config.json: no such file or directory" — but
# only after git pull + uv sync + build have already mutated the tree (#702).
# Refuse early so the operator runs update.sh from the deployment directory
# instead of improvising rsync-based syncs that can clobber deployment state.
# Check runs AFTER `cd "$repo_root"` so a subdirectory invocation (e.g. from
# `scheduler/`) doesn't report a misleading "missing config" message.
if [[ ! -f scheduler/config.json ]]; then
    cat >&2 <<EOF
[update] scheduler/config.json not found in $(pwd).

update.sh must run from a deployment directory (where scheduler/config.json
exists). scheduler/config.json is gitignored, so a bare source clone has none
and the probe phase would later fail without it.

If this IS your deployment directory, copy scheduler/config.example.json to
scheduler/config.json and fill in API keys (see CLAUDE.md → Setup).

If you are syncing source to multiple deployment instances, build in the
source repo and run scripts/update.sh from each deployment instance.
EOF
    fail "scheduler/config.json missing — refusing to mutate tree from a non-deployment directory"
fi

# Build-input paths — anything touched here would actually affect the binary
# or its Python contract. Modifications outside this list (e.g. CLAUDE.md,
# scratch notes) are tolerated so an operator with unrelated local edits
# isn't blocked from updating.
build_paths=(
    scheduler
    shared_scripts
    shared_strategies
    shared_tools
    platforms
    backtest
    pyproject.toml
    uv.lock
)

# Refuse to update with uncommitted build-input modifications — they'd
# silently survive git pull --ff-only and produce a binary built from a tree
# the operator may not have intended. Non-build-input dirty paths are
# warn-only.
if ! git diff --quiet -- "${build_paths[@]}" || ! git diff --cached --quiet -- "${build_paths[@]}"; then
    git status --short -- "${build_paths[@]}" >&2
    fail "working tree has uncommitted changes in build-input paths; commit, stash, or revert first"
fi
if ! git diff --quiet || ! git diff --cached --quiet; then
    echo "[update] warning: uncommitted changes outside build-input paths (will survive git pull):" >&2
    git status --short >&2
fi

# Untracked files in source dirs and at the repo root are warn-only —
# operators sometimes drop scratch files in worktrees on purpose; they don't
# affect the build. Repo-root scan filters to top-level entries only (no
# slashes) so a stray `pyproject.toml.bak` or scratch file at the root is
# surfaced even though it would never have shown up in a source-dir scan.
untracked=$(git ls-files --others --exclude-standard \
    scheduler shared_scripts shared_strategies shared_tools platforms backtest 2>/dev/null || true)
untracked_root=$(git ls-files --others --exclude-standard 2>/dev/null | grep -v '/' || true)
if [[ -n "$untracked" || -n "$untracked_root" ]]; then
    echo "[update] warning: untracked files (will not affect the build):" >&2
    [[ -n "$untracked" ]] && echo "$untracked" >&2
    [[ -n "$untracked_root" ]] && echo "$untracked_root" >&2
fi

# Capture currently running version for rollback comparison and post-update
# logging. Best-effort — empty on first install (no binary yet) and ALSO
# empty when crossing the pre-#682 boundary because pre-#682 binaries don't
# implement the `version` subcommand (they print usage + exit non-zero).
prev_running_version=""
if [[ -x ./go-trader ]]; then
    prev_running_version=$(./go-trader version 2>/dev/null || echo "")
fi
echo "[update] previous binary version: ${prev_running_version:-<none>}"

# Capture pre-pull SHA and main PID so rollback can revert ALL state
# touched by this update (binary, git tree, Python deps, running process),
# not just the binary swap.
pre_pull_sha=$(git rev-parse HEAD 2>/dev/null || echo "")
prev_main_pid=$(systemctl show -p MainPID --value "$service_unit" 2>/dev/null || echo "")
# systemd reports MainPID=0 when the unit is inactive; normalize to empty
# so the verify step doesn't try to compare against a meaningless 0.
if [[ "$prev_main_pid" == "0" ]]; then
    prev_main_pid=""
fi

end_phase

# Phase 2: git pull
begin_phase pull
git pull --ff-only
post_pull_sha=$(git rev-parse HEAD 2>/dev/null || echo "")
end_phase

# Phase 3: uv sync
begin_phase sync
uv sync
end_phase

# Phase 4: build to staging path. A killed/failed build leaves go-trader.new
# corrupt or missing; the live ./go-trader is untouched until phase swap.
begin_phase build
ver=$(git describe --tags --always --dirty=-mod 2>/dev/null || echo dev)
rm -f ./go-trader.new
"$go_bin" -C scheduler build -ldflags "-X main.Version=$ver" -o ../go-trader.new .
if [[ ! -s ./go-trader.new ]]; then
    fail "go build produced empty go-trader.new"
fi
if [[ ! -x ./go-trader.new ]]; then
    fail "go-trader.new is not executable"
fi
echo "[update] built ${ver}: $(stat -c '%s' ./go-trader.new 2>/dev/null || stat -f '%z' ./go-trader.new) bytes"
end_phase

# Phase 5: probe the staged binary against the just-synced Python BEFORE
# overwriting the live binary. Catches binary/Python argv mismatches while
# the live process is still serving.
begin_phase probe
if ! ./go-trader.new probe; then
    rm -f ./go-trader.new
    fail "go-trader.new probe rejected the freshly synced Python — refusing to swap"
fi
end_phase

# Phase 6: atomic swap. POSIX rename on the same filesystem is atomic, so
# concurrent reads of ./go-trader either see the old or new file, never a
# partial write. Cap retention at one .prev to bound disk use.
begin_phase swap
rm -f ./go-trader.prev
if [[ -e ./go-trader ]]; then
    mv -f ./go-trader ./go-trader.prev
fi
mv -f ./go-trader.new ./go-trader
end_phase

# Phase 7: restart + verify (only when --restart). Without --restart, the
# in-process applyUpgrade path will trigger restartSelf on its own; this
# script stops after swap.
if [[ "$restart" != "1" ]]; then
    echo "[update] build OK at $ver (skipping restart; pass --restart to enable)"
    exit 0
fi

# Determine the /health port: explicit STATUS_PORT env > config.status_port > 8099.
# Use the freshly synced uv environment to parse JSON so we don't depend on jq.
status_port="${STATUS_PORT:-}"
if [[ -z "$status_port" && -f scheduler/config.json ]]; then
    status_port=$(uv run --no-sync python -c '
import json, sys
try:
    cfg = json.load(open("scheduler/config.json"))
    p = cfg.get("status_port") or 0
    if isinstance(p, (int, float)) and int(p) > 0:
        print(int(p))
except Exception:
    pass
' 2>/dev/null || true)
fi
status_port="${status_port:-8099}"

# Rollback function — restores the previous binary AND reverts the git tree
# + Python deps so the .prev binary's startup probe sees the Python contract
# it was built against (#682). Called from trap on any post-swap failure
# path so a failed verify doesn't leave the service down on a broken binary
# or on a .prev binary fighting a newer Python tree.
do_rollback() {
    local reason="$1"
    echo "[update] rollback: $reason" >&2
    if [[ ! -x ./go-trader.prev ]]; then
        echo "[update] rollback: no go-trader.prev to restore — service stays on broken binary" >&2
        return
    fi
    mv -f ./go-trader.prev ./go-trader

    # Revert git tree and re-sync Python deps if the pull actually advanced
    # HEAD — otherwise the .prev binary may fail its own startup probe
    # against a Python tree that requires a runtime CLI flag added in this
    # update (per CLAUDE.md: required CLI flags must be appended to
    # probeArgv). Best-effort: any failure here is logged but doesn't abort
    # the rollback — getting the previous binary back up is the priority.
    if [[ -n "$pre_pull_sha" && -n "${post_pull_sha:-}" && "$pre_pull_sha" != "$post_pull_sha" ]]; then
        echo "[update] rollback: reverting git tree to $pre_pull_sha" >&2
        if git reset --hard "$pre_pull_sha" >&2; then
            if ! uv sync >&2; then
                echo "[update] rollback: uv sync FAILED — Python tree may be inconsistent with .prev binary" >&2
            fi
        else
            echo "[update] rollback: git reset --hard FAILED — Python tree still on new SHA, .prev binary may probe-fail" >&2
        fi
    fi

    sudo systemctl restart "$service_unit" || true
    # Best-effort wait for active after rollback. We don't fail the rollback
    # itself on a slow restart — operators see the rollback marker either way.
    local waited=0
    while [[ $waited -lt $active_timeout ]]; do
        if systemctl is-active --quiet "$service_unit"; then
            echo "[update] rollback: previous binary active again (${prev_running_version:-unknown})"
            return
        fi
        sleep 1
        waited=$((waited + 1))
    done
    echo "[update] rollback: previous binary did not reach active within ${active_timeout}s" >&2
}

begin_phase restart
sudo systemctl restart "$service_unit"

waited=0
until systemctl is-active --quiet "$service_unit"; do
    if [[ $waited -ge $active_timeout ]]; then
        do_rollback "systemctl is-active timeout after ${active_timeout}s"
        fail "service did not reach active state within ${active_timeout}s"
    fi
    sleep 1
    waited=$((waited + 1))
done
echo "[update] systemd reports active: $service_unit (${waited}s)"
end_phase

begin_phase verify
# Poll /health until it returns a body with the expected version AND the
# systemd MainPID has changed from prev_main_pid. The PID check guards
# against same-SHA rebuilds (where prev_running_version == ver and the old
# process would otherwise pass the substring match) and against systemd
# decisions that leave the previous process running (e.g. a fast restart
# with no MainPID change). Failure modes triggering rollback:
#   - HTTP not yet listening
#   - HTTP up but version mismatch (service restarted on .prev because new
#     binary panicked post-init)
#   - version match but MainPID unchanged (old process still running)
url="http://localhost:${status_port}/health"
waited=0
verified=0
while [[ $waited -lt $health_timeout ]]; do
    body=$(curl -fsS --max-time 2 "$url" 2>/dev/null || true)
    if [[ -n "$body" ]]; then
        # JSON shape: {"status":"ok","version":"v1.2.3"}. Substring match is
        # sufficient — the version string is git describe output, no quoting
        # ambiguity.
        if [[ "$body" == *"\"version\":\"$ver\""* ]]; then
            cur_main_pid=$(systemctl show -p MainPID --value "$service_unit" 2>/dev/null || echo "")
            if [[ "$cur_main_pid" == "0" ]]; then
                cur_main_pid=""
            fi
            # Only enforce PID change when we successfully captured a
            # pre-restart PID. If the unit was inactive before this run,
            # prev_main_pid is empty and version-match alone is sufficient.
            if [[ -n "$prev_main_pid" && -n "$cur_main_pid" && "$prev_main_pid" == "$cur_main_pid" ]]; then
                : # same PID — old process still running, keep polling
            else
                echo "[update] /health version=$ver OK (pid ${prev_main_pid:-<none>} -> ${cur_main_pid:-<unknown>})"
                verified=1
                break
            fi
        fi
    fi
    sleep 1
    waited=$((waited + 1))
done
if [[ $verified -ne 1 ]]; then
    do_rollback "/health did not report version=$ver with a fresh PID within ${health_timeout}s (last body: ${body:-<empty>}, prev_pid=${prev_main_pid:-<none>})"
    fail "/health verify failed — expected version=$ver and PID != ${prev_main_pid:-<none>}"
fi
end_phase

echo "[update] done: $ver running, previous=${prev_running_version:-<none>} retained as go-trader.prev"

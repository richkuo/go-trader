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
#   restart    — sudo systemctl restart go-trader (only with --restart)
#   verify     — wait active + /health version match (only with --restart)
#   rollback   — restore .prev on verify failure (only with --restart)
#
# Use --restart (or RESTART=1) to restart the service after a successful build
# AND verify the running process matches the just-built version. Without
# --restart the script stops after swap; the caller (scheduler/updater.go's
# applyUpgrade) handles its own restart via restartSelf.

set -euo pipefail

restart=0
for arg in "$@"; do
    case "$arg" in
        --restart) restart=1 ;;
        -h|--help)
            echo "Usage: $0 [--restart]"
            echo "  RESTART=1 env var also enables restart."
            echo ""
            echo "Env overrides:"
            echo "  STATUS_PORT=<n>          override /health port (default: read from config, else 8099)"
            echo "  ACTIVE_TIMEOUT=<sec>     systemctl is-active poll timeout (default: 30)"
            echo "  HEALTH_TIMEOUT=<sec>     /health version-match poll timeout (default: 15)"
            exit 0
            ;;
        *)
            echo "unknown arg: $arg" >&2
            exit 2
            ;;
    esac
done
if [[ "${RESTART:-0}" == "1" ]]; then
    restart=1
fi

active_timeout="${ACTIVE_TIMEOUT:-30}"
health_timeout="${HEALTH_TIMEOUT:-15}"

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

# Refuse to update with uncommitted modifications — they'd silently survive
# git pull --ff-only and produce a binary built from a tree the operator may
# not have intended.
if ! git diff --quiet || ! git diff --cached --quiet; then
    git status --short >&2
    fail "working tree has uncommitted changes; commit, stash, or revert first"
fi

# Untracked files in source dirs are warn-only — operators sometimes drop
# scratch files in worktrees on purpose; they don't affect the build.
untracked=$(git ls-files --others --exclude-standard scheduler shared_scripts shared_strategies shared_tools platforms backtest 2>/dev/null || true)
if [[ -n "$untracked" ]]; then
    echo "[update] warning: untracked files in source dirs:" >&2
    echo "$untracked" >&2
fi

# Capture currently running version for rollback comparison and post-update
# logging. Best-effort — a missing binary is fine on first install.
prev_running_version=""
if [[ -x ./go-trader ]]; then
    prev_running_version=$(./go-trader version 2>/dev/null || echo "")
fi
echo "[update] previous binary version: ${prev_running_version:-<none>}"

end_phase

# Phase 2: git pull
begin_phase pull
git pull --ff-only
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
# Use the freshly synced .venv to parse JSON so we don't depend on jq.
status_port="${STATUS_PORT:-}"
if [[ -z "$status_port" && -f scheduler/config.json && -x .venv/bin/python3 ]]; then
    status_port=$(.venv/bin/python3 -c '
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

# Rollback function — restores the previous binary and restarts. Called from
# trap on any post-swap failure path so a failed verify doesn't leave the
# service down on a broken binary.
do_rollback() {
    local reason="$1"
    echo "[update] rollback: $reason" >&2
    if [[ ! -x ./go-trader.prev ]]; then
        echo "[update] rollback: no go-trader.prev to restore — service stays on broken binary" >&2
        return
    fi
    mv -f ./go-trader.prev ./go-trader
    sudo systemctl restart go-trader || true
    # Best-effort wait for active after rollback. We don't fail the rollback
    # itself on a slow restart — operators see the rollback marker either way.
    local waited=0
    while [[ $waited -lt $active_timeout ]]; do
        if systemctl is-active --quiet go-trader; then
            echo "[update] rollback: previous binary active again (${prev_running_version:-unknown})"
            return
        fi
        sleep 1
        waited=$((waited + 1))
    done
    echo "[update] rollback: previous binary did not reach active within ${active_timeout}s" >&2
}

begin_phase restart
sudo systemctl restart go-trader

waited=0
until systemctl is-active --quiet go-trader; do
    if [[ $waited -ge $active_timeout ]]; then
        do_rollback "systemctl is-active timeout after ${active_timeout}s"
        fail "service did not reach active state within ${active_timeout}s"
    fi
    sleep 1
    waited=$((waited + 1))
done
echo "[update] systemd reports active (${waited}s)"
end_phase

begin_phase verify
# Poll /health until it returns a body containing the expected version string.
# Two failure modes here: HTTP not yet listening, or HTTP up but version
# mismatch (e.g. service restarted on .prev because new binary panicked
# post-init). Both trigger rollback.
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
            echo "[update] /health version=$ver OK"
            verified=1
            break
        fi
    fi
    sleep 1
    waited=$((waited + 1))
done
if [[ $verified -ne 1 ]]; then
    do_rollback "/health did not report version=$ver within ${health_timeout}s (last body: ${body:-<empty>})"
    fail "/health version mismatch — expected $ver"
fi
end_phase

echo "[update] done: $ver running, previous=${prev_running_version:-<none>} retained as go-trader.prev"

#!/usr/bin/env bash
# Atomic update: git pull → uv sync → go build, optional systemctl restart.
# Use --restart (or RESTART=1) to restart the service after a successful build.
# Called by both operators (from the auto-update DM) and scheduler/updater.go.

set -euo pipefail

restart=0
for arg in "$@"; do
    case "$arg" in
        --restart) restart=1 ;;
        -h|--help)
            echo "Usage: $0 [--restart]"
            echo "  RESTART=1 env var also enables restart."
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

repo_root=$(git rev-parse --show-toplevel)
cd "$repo_root"

echo "[update] git pull --ff-only"
git pull --ff-only

echo "[update] uv sync"
uv sync

echo "[update] go build"
ver=$(git describe --tags --always --dirty=-mod 2>/dev/null || echo dev)
go_bin=$(command -v go || echo /opt/homebrew/bin/go)
"$go_bin" -C scheduler build -ldflags "-X main.Version=$ver" -o ../go-trader .

if [[ "$restart" == "1" ]]; then
    echo "[update] systemctl restart go-trader"
    sudo systemctl restart go-trader
else
    echo "[update] build OK at $ver (skipping restart; pass --restart to enable)"
fi

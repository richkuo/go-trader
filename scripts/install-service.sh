#!/usr/bin/env bash
# Install a go-trader systemd unit, reload systemd, and enable it so the
# service survives reboots. Optionally start it immediately.
#
# Without arguments, installs the canonical go-trader.service from the repo root.
# Pass a path to install an ad hoc named variant (e.g. go-trader-paper-testing.service)
# or the templated unit (systemd/go-trader@.service).
#
# Usage:
#   scripts/install-service.sh                                  # installs go-trader.service and starts it
#   scripts/install-service.sh path/to/go-trader-foo.service    # installs + enables + starts go-trader-foo
#   scripts/install-service.sh systemd/go-trader@.service live  # installs template, enables+starts go-trader@live
#   NO_START=1 scripts/install-service.sh ...                   # enable only, do not start
#
# The script is idempotent: re-running it will refresh the unit file and
# re-enable the service without error.
set -euo pipefail

if [[ $EUID -ne 0 ]]; then
  echo "error: must be run as root (try: sudo $0 $*)" >&2
  exit 1
fi

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SRC="${1:-$REPO_ROOT/go-trader.service}"
INSTANCE="${2:-}"

if [[ ! -f "$SRC" ]]; then
  echo "error: unit file not found: $SRC" >&2
  exit 1
fi

UNIT_FILENAME="$(basename "$SRC")"
DEST="/etc/systemd/system/$UNIT_FILENAME"

# For a template unit (go-trader@.service), the instance name is what gets
# enabled/started (e.g. go-trader@live). For a plain unit, ignore $INSTANCE.
if [[ "$UNIT_FILENAME" == *@.service ]]; then
  if [[ -z "$INSTANCE" ]]; then
    echo "error: template unit $UNIT_FILENAME requires an instance name as arg 2" >&2
    echo "       example: $0 $SRC live" >&2
    exit 1
  fi
  SERVICE_NAME="${UNIT_FILENAME%@.service}@${INSTANCE}.service"
else
  SERVICE_NAME="$UNIT_FILENAME"
fi

echo "Installing $SRC -> $DEST"
install -m 0644 "$SRC" "$DEST"

echo "Reloading systemd"
systemctl daemon-reload

echo "Enabling $SERVICE_NAME (will auto-start on boot)"
systemctl enable "$SERVICE_NAME"

if [[ "${NO_START:-0}" == "1" ]]; then
  echo "NO_START=1 set; skipping start. Run: systemctl start $SERVICE_NAME"
else
  echo "Starting $SERVICE_NAME"
  systemctl restart "$SERVICE_NAME"
fi

echo
systemctl is-enabled "$SERVICE_NAME" >/dev/null && echo "enabled: yes" || echo "enabled: no"
systemctl is-active "$SERVICE_NAME"  >/dev/null && echo "active:  yes" || echo "active:  no"
echo
echo "Done. Tail logs: journalctl -u $SERVICE_NAME -f"

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
  echo "error: must be run as root (try: sudo $0 $@)" >&2
  exit 1
fi

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SRC="${1:-$REPO_ROOT/go-trader.service}"
INSTANCE="${2:-}"

if [[ ! -f "$SRC" ]]; then
  echo "error: unit file not found: $SRC" >&2
  exit 1
fi

# systemd template instance names flow into the unit name and `systemctl enable`;
# reject anything outside [A-Za-z0-9_.-] to avoid confusing systemd errors.
if [[ -n "$INSTANCE" && "$INSTANCE" =~ [^a-zA-Z0-9_.-] ]]; then
  echo "error: instance name must contain only alphanumerics, dash, dot, or underscore (got: $INSTANCE)" >&2
  exit 1
fi

UNIT_FILENAME="$(basename "$SRC")"
DEST="/etc/systemd/system/$UNIT_FILENAME"

resolve_unit_value() {
  local value="$1"
  if [[ "$UNIT_FILENAME" == *@.service ]]; then
    value="${value//%i/$INSTANCE}"
    value="${value//%I/$INSTANCE}"
  fi
  printf '%s\n' "$value"
}

read_unit_field() {
  local key="$1"
  local raw
  raw="$(sed -n "s/^${key}=//p" "$SRC" | head -n 1)"
  if [[ -z "$raw" ]]; then
    return 0
  fi
  resolve_unit_value "$raw"
}

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

WORKING_DIR="$(read_unit_field WorkingDirectory)"
SERVICE_USER="$(read_unit_field User)"
SERVICE_GROUP="$(read_unit_field Group)"
LOG_DIR=""
if [[ -n "$WORKING_DIR" ]]; then
  LOG_DIR="$WORKING_DIR/logs"
fi

echo "Installing $SRC -> $DEST"
install -m 0644 "$SRC" "$DEST"

if [[ -n "$LOG_DIR" ]]; then
  echo "Ensuring log directory exists: $LOG_DIR"
  install -d -m 0755 "$LOG_DIR"
  if [[ -n "$SERVICE_USER" ]]; then
    OWNER="$SERVICE_USER"
    if [[ -n "$SERVICE_GROUP" ]]; then
      OWNER="${SERVICE_USER}:${SERVICE_GROUP}"
    fi
    chown "$OWNER" "$LOG_DIR"
  fi
fi

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
ENABLED_STATE="$(systemctl is-enabled "$SERVICE_NAME" 2>/dev/null || true)"
ACTIVE_STATE="$(systemctl is-active "$SERVICE_NAME" 2>/dev/null || true)"
echo "enabled: $ENABLED_STATE"
echo "active:  $ACTIVE_STATE"
echo
echo "Done. Tail logs: journalctl -u $SERVICE_NAME -f"

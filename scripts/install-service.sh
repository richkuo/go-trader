#!/usr/bin/env bash
set -euo pipefail

if [[ $EUID -ne 0 ]]; then
  echo "error: must be run as root (try: sudo $0 $@)" >&2
  exit 1
fi

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source "$REPO_ROOT/scripts/update_helpers.sh"
UPDATE_UNIT_SUDO=""
SRC="${1:-$REPO_ROOT/go-trader.service}"
INSTANCE="${2:-}"

if [[ ! -f "$SRC" ]]; then
  echo "error: unit file not found: $SRC" >&2
  exit 1
fi

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

update_sync_journal_namespace "$REPO_ROOT" "$SRC" || {
  echo "error: journald namespace setup failed; $DEST was not installed" >&2
  exit 1
}

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

CPU_DROPIN="$(update_cpu_dropin_path "$SERVICE_NAME")"
if [[ -n "$CPU_DROPIN" ]]; then
  CPU_BINARY="$(execstart_main_binary "$(systemctl show -p ExecStart --value "$SERVICE_NAME" 2>/dev/null || true)")"
  CPU_CONFIG="$(update_unit_config_path "$SERVICE_NAME")"
  if [[ -z "$CPU_BINARY" || ! -x "$CPU_BINARY" ]]; then
    echo "[cpu] $SERVICE_NAME has no go-trader binary at '${CPU_BINARY:-<none>}' yet; the CPU weight drop-in is unchanged (the next update.sh --restart sets it)"
  elif [[ -z "$CPU_CONFIG" || ! -f "$CPU_CONFIG" ]]; then
    echo "[cpu] $SERVICE_NAME has no config at '${CPU_CONFIG:-<none>}' yet; the CPU weight drop-in is unchanged (the next update.sh --restart sets it)"
  else
    CPU_PLAN="$(update_cpu_dropin_plan "$SERVICE_NAME" "$CPU_BINARY" "$CPU_DROPIN")"
    CPU_DECISION="${CPU_PLAN%% *}"
    CPU_WEIGHT="${CPU_PLAN#* }"
    case "$CPU_DECISION" in
      install|replace|remove)
        if ! update_cpu_dropin_apply "$CPU_DECISION" "$CPU_DROPIN" "$CPU_WEIGHT"; then
          echo "error: could not $CPU_DECISION the CPU weight drop-in $CPU_DROPIN; $SERVICE_NAME was not enabled or started" >&2
          exit 1
        fi
        echo "[cpu] $CPU_DECISION $CPU_DROPIN${CPU_WEIGHT:+ (CPUWeight=$CPU_WEIGHT)}"
        systemctl daemon-reload
        ;;
      skip-symlink)
        echo "[cpu] $CPU_DROPIN is a symlink; leaving the operator's link alone" >&2
        ;;
      *)
        echo "[cpu] CPU weight drop-in for $SERVICE_NAME is unchanged${CPU_WEIGHT:+ (CPUWeight=$CPU_WEIGHT)}"
        ;;
    esac
  fi
fi

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
LOG_NAMESPACE="$(systemctl show -p LogNamespace --value "$SERVICE_NAME" 2>/dev/null || true)"
echo "Done. Tail logs: $(update_journalctl_unit_command "$SERVICE_NAME" "$LOG_NAMESPACE") -f"

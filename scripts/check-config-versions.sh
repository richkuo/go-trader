#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
source "${SCRIPT_DIR}/update_helpers.sh"

read_floor_from_source() {
    local src="${SCRIPT_DIR}/../scheduler/config_migration.go" v=""
    if [[ -f "$src" ]]; then
        v=$(sed -n 's/^const MinSupportedConfigVersion = \([0-9][0-9]*\)$/\1/p' "$src" | head -n 1)
    fi
    printf '%s' "${v:-13}"
}
MIN_SUPPORTED_CONFIG_VERSION=$(read_floor_from_source)

read_config_version() {
    local path="$1"
    if [[ ! -e "$path" ]]; then
        printf 'missing-file'
        return 0
    fi
    python3 - "$path" <<'PY' 2>/dev/null || printf 'unreadable'
import json, sys
try:
    with open(sys.argv[1]) as f:
        cfg = json.load(f)
except Exception:
    print("unreadable")
    sys.exit(0)
v = cfg.get("config_version")
if v is None:
    print("missing-key")
elif isinstance(v, int) and not isinstance(v, bool):
    print(v)
else:
    print("unreadable")
PY
}

rows=0
bad=0
min_seen=""

report_row() {
    local source="$1" cfg_path="$2"
    local version verdict
    version=$(read_config_version "$cfg_path")
    case "$version" in
        missing-file|unreadable)
            verdict="FAIL (cannot verify)"
            bad=$((bad + 1))
            ;;
        missing-key)
            verdict="OK (version-less; stamped on next daemon start)"
            ;;
        *)
            if [[ "$version" -lt "$MIN_SUPPORTED_CONFIG_VERSION" ]]; then
                verdict="FAIL (below floor ${MIN_SUPPORTED_CONFIG_VERSION})"
                bad=$((bad + 1))
            else
                verdict="OK"
            fi
            if [[ -z "$min_seen" || "$version" -lt "$min_seen" ]]; then
                min_seen="$version"
            fi
            ;;
    esac
    rows=$((rows + 1))
    printf '%-40s %-60s %-12s %s\n' "$source" "$cfg_path" "$version" "$verdict"
}

printf 'go-trader fleet config_version audit (#1285) — floor: %s\n' "$MIN_SUPPORTED_CONFIG_VERSION"
printf '%-40s %-60s %-12s %s\n' 'DEPLOYMENT' 'CONFIG' 'VERSION' 'VERDICT'

if [[ $# -gt 0 ]]; then
    for dir in "$@"; do
        dir=$(canonicalize_deployment_dir "$dir")
        report_row "$dir" "${dir}scheduler/config.json"
    done
else
    if ! command -v systemctl >/dev/null 2>&1; then
        echo "ERROR: systemctl not available and no deployment dirs given — pass dirs explicitly" >&2
        exit 2
    fi
    globs=()
    while IFS= read -r g; do
        [[ -n "$g" ]] && globs+=("$g")
    done < <(update_systemd_unit_globs)
    units=()
    while IFS= read -r unit; do
        [[ -n "$unit" ]] && units+=("$unit")
    done < <(systemctl list-units --type=service --state=active --no-legend --plain "${globs[@]}" 2>/dev/null | awk '{print $1}')
    if [[ ${#units[@]} -eq 0 ]]; then
        echo "ERROR: no active go-trader systemd units found — an empty audit is not a verified fleet" >&2
        exit 2
    fi
    for unit in "${units[@]}"; do
        execstart=$(systemctl show "$unit" -p ExecStart --value 2>/dev/null || true)
        cfg_path=$(update_execstart_config_path "$execstart")
        if [[ -z "$cfg_path" ]]; then
            wd=$(systemctl show "$unit" -p WorkingDirectory --value 2>/dev/null || true)
            if [[ -z "$wd" ]]; then
                printf '%-40s %-60s %-12s %s\n' "$unit" '-' '-' 'FAIL (no --config and no WorkingDirectory)'
                rows=$((rows + 1))
                bad=$((bad + 1))
                continue
            fi
            cfg_path="${wd%/}/scheduler/config.json"
        fi
        report_row "$unit" "$cfg_path"
    done
fi

echo
if [[ -n "$min_seen" ]]; then
    printf 'minimum stamped config_version observed: %s\n' "$min_seen"
fi
if [[ $bad -gt 0 ]]; then
    printf 'VERDICT: BLOCKED — %d of %d deployment(s) below the floor or unverifiable. Do NOT prune migration handlers.\n' "$bad" "$rows"
    exit 1
fi
printf 'VERDICT: OK — all %d deployment(s) at or above config_version %s.\n' "$rows" "$MIN_SUPPORTED_CONFIG_VERSION"

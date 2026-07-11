#!/usr/bin/env bash
# check-config-versions.sh — READ-ONLY fleet audit of config_version (#1285).
#
# Prints one line per deployment (source, config path, config_version) and a
# verdict against the migration floor. This produces the blocking-gate artifact
# for pruning migration handlers: run it on the production host and record its
# full output in the PR BEFORE raising MinSupportedConfigVersion. The script
# never writes anything — no config rewrite, no daemon interaction.
#
# Usage:
#   bash scripts/check-config-versions.sh                    # auto-discover active systemd deployments (#1055)
#   bash scripts/check-config-versions.sh /opt/go-trader ... # audit explicit deployment dirs instead
#
# Discovery mirrors update.sh --all: active go-trader systemd units, reading
# each unit's ExecStart --config path (the #1056 out-of-tree location) and
# falling back to <WorkingDirectory>/scheduler/config.json (the transition
# symlink, or the legacy in-tree file).
#
# Exit codes:
#   0 — every deployment readable and at/above the floor (fleet verified)
#   1 — at least one deployment below the floor, unreadable, or missing
#   2 — nothing to audit (an empty audit is NOT a verified fleet)
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=update_helpers.sh
source "${SCRIPT_DIR}/update_helpers.sh"

# The floor is read from the Go source so the audit can never drift from the
# binary's MinSupportedConfigVersion; 13 is the #1285 fallback for a copy of
# this script run outside a checkout.
read_floor_from_source() {
    local src="${SCRIPT_DIR}/../scheduler/config_migration.go" v=""
    if [[ -f "$src" ]]; then
        v=$(sed -n 's/^const MinSupportedConfigVersion = \([0-9][0-9]*\)$/\1/p' "$src" | head -n 1)
    fi
    printf '%s' "${v:-13}"
}
MIN_SUPPORTED_CONFIG_VERSION=$(read_floor_from_source)

# Echo the config_version of a JSON config, or a diagnostic token:
# "missing-file", "missing-key" (version-less config — loads as current shape),
# or "unreadable" (broken JSON / permissions).
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
            # Version-less configs are hand-authored current-shape files; the
            # daemon stamps CurrentConfigVersion on first run. Not a floor
            # violation, but surfaced so the operator can eyeball it.
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

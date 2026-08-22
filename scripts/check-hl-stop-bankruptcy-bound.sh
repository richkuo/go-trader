#!/usr/bin/env bash
# check-hl-stop-bankruptcy-bound.sh — READ-ONLY fleet preflight for the #1450
# isolated-margin bankruptcy bound on percentage stop-loss owners.
#
# #1450 added validateHLStopWithinBankruptcyBound to validateConfig, which makes
# a percentage stop at or beyond 100 / leverage a FATAL config error. A geometry
# like stop_loss_pct: 5 with leverage: 20 loads today and refuses to load after
# the update. The rule is correct — Hyperliquid force-closes the position before
# such a stop could ever fill — but a fatal rule that only announces itself at
# restart-verify would have update.sh roll back with live positions unmanaged in
# the interim.
#
# This is that rule's preflight, mirroring scripts/check-config-versions.sh
# (#1285): run it on the production host BEFORE the update, and record its
# output. It never writes anything — no config rewrite, no daemon interaction.
#
# Usage:
#   bash scripts/check-hl-stop-bankruptcy-bound.sh                    # auto-discover active systemd deployments
#   bash scripts/check-hl-stop-bankruptcy-bound.sh /opt/go-trader ... # audit explicit deployment dirs instead
#
# Scope mirrors the Go check EXACTLY (scheduler/hyperliquid_liquidation_guard.go,
# validateHLStopWithinBankruptcyBound). Change one, change the other:
#   - platform hyperliquid, type perps, LIVE args only (paper has no account);
#   - ISOLATED margin only (empty margin_mode reads as isolated; a cross-margin
#     liquidation can sit beyond 1/leverage, so the bound would falsely reject);
#   - percentage owners only: stop_loss_pct, trailing_stop_pct, and the derived
#     stop_loss_margin_pct / leverage. ATR-derived owners need a per-position
#     EntryATR and are checked at arm time by the runtime clamp instead.
#
# Exit codes:
#   0 — every deployment readable and every live isolated strategy inside the bound
#   1 — at least one offending strategy, or a deployment that could not be read
#   2 — nothing to audit (an empty audit is NOT a verified fleet)
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=update_helpers.sh
source "${SCRIPT_DIR}/update_helpers.sh"

# Emit one line per offending strategy, or a diagnostic token on its own line
# ("missing-file" / "unreadable"). Silence means the config is clean.
scan_config() {
    local path="$1"
    if [[ ! -e "$path" ]]; then
        printf 'missing-file\n'
        return 0
    fi
    python3 - "$path" <<'PY' 2>/dev/null || printf 'unreadable\n'
import json, sys

try:
    with open(sys.argv[1]) as f:
        cfg = json.load(f)
except Exception:
    print("unreadable")
    sys.exit(0)


def is_live(args):
    # Mirrors isLiveArgs (scheduler/state_presence.go): "--mode=live", or
    # "--mode" immediately followed by "live". Nothing looser — a bare "live"
    # token elsewhere in argv is not a live marker.
    args = args or []
    for i, a in enumerate(args):
        if a == "--mode=live":
            return True
        if a == "--mode" and i + 1 < len(args) and args[i + 1] == "live":
            return True
    return False


def effective_leverage(sc):
    # Mirrors EffectiveExchangeLeverage (scheduler/config.go): the "leverage"
    # field on a perps strategy, defaulting to 1.
    v = sc.get("leverage")
    if isinstance(v, (int, float)) and not isinstance(v, bool) and v > 0:
        return float(v)
    return 1.0


for sc in cfg.get("strategies", []) or []:
    if not isinstance(sc, dict):
        continue
    if sc.get("platform") != "hyperliquid" or sc.get("type") != "perps":
        continue
    if not is_live(sc.get("args")):
        continue
    if str(sc.get("margin_mode", "") or "").strip().lower() == "cross":
        continue
    lev = effective_leverage(sc)
    bound = 100.0 / max(lev, 1.0)
    checks = []
    for field in ("stop_loss_pct", "trailing_stop_pct"):
        v = sc.get(field)
        if isinstance(v, (int, float)) and not isinstance(v, bool):
            checks.append((field, float(v)))
    smp = sc.get("stop_loss_margin_pct")
    if isinstance(smp, (int, float)) and not isinstance(smp, bool) and smp > 0:
        checks.append(("stop_loss_margin_pct/leverage", float(smp) / max(lev, 1.0)))
    for field, pct in checks:
        if pct > 0 and pct >= bound:
            print(
                "%s: %s = %g%% >= bound %g%% (leverage %g)"
                % (sc.get("id", "<no id>"), field, pct, bound, lev)
            )
PY
}

rows=0
bad=0
offenders=0

report_row() {
    local source="$1" cfg_path="$2" out
    out=$(scan_config "$cfg_path")
    rows=$((rows + 1))
    if [[ "$out" == "missing-file" || "$out" == "unreadable" ]]; then
        printf '%-40s %-60s %s\n' "$source" "$cfg_path" "FAIL (cannot verify: ${out})"
        bad=$((bad + 1))
        return 0
    fi
    if [[ -z "$out" ]]; then
        printf '%-40s %-60s %s\n' "$source" "$cfg_path" 'OK'
        return 0
    fi
    printf '%-40s %-60s %s\n' "$source" "$cfg_path" 'FAIL (stop beyond bankruptcy bound)'
    while IFS= read -r line; do
        [[ -n "$line" ]] || continue
        printf '    %s\n' "$line"
        offenders=$((offenders + 1))
    done <<< "$out"
    bad=$((bad + 1))
}

printf 'go-trader fleet HL stop-vs-bankruptcy-bound preflight (#1450)\n'
printf '%-40s %-60s %s\n' 'DEPLOYMENT' 'CONFIG' 'VERDICT'

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
                printf '%-40s %-60s %s\n' "$unit" '-' 'FAIL (no --config and no WorkingDirectory)'
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
if [[ $bad -gt 0 ]]; then
    printf 'VERDICT: BLOCKED — %d of %d deployment(s) unverifiable or carrying %d offending strategy setting(s). Fix them BEFORE running scripts/update.sh --restart, or the restart-verify fails and rolls back.\n' "$bad" "$rows" "$offenders"
    exit 1
fi
printf 'VERDICT: OK — all %d deployment(s) load under the #1450 bankruptcy bound.\n' "$rows"

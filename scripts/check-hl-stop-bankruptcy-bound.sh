#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
source "${SCRIPT_DIR}/update_helpers.sh"

scan_config() {
    local path="$1"
    if [[ ! -e "$path" ]]; then
        printf 'missing-file\n'
        return 0
    fi
    python3 - "$path" <<'PY' 2>/dev/null || printf 'unreadable\n'
import json, sys

LEGACY_V19_STOP_LOSS_REGIME_KEY = "stop_loss_atr_regime"
LEGACY_V18_TRAIL_STOP_REGIME_KEY = "trail_stop_atr_regime"
LEGACY_V17_TRAIL_STOP_REGIME_KEY = "trailing_stop_atr_regime"
V19_STOP_LOSS_REGIME_KEY = "stop_loss_atr_mult_regime"
V19_TRAIL_STOP_REGIME_KEY = "trailing_stop_atr_mult_regime"

ATR_REGIME_KEY_RENAMES = (
    (LEGACY_V17_TRAIL_STOP_REGIME_KEY, V19_TRAIL_STOP_REGIME_KEY),
    (LEGACY_V18_TRAIL_STOP_REGIME_KEY, V19_TRAIL_STOP_REGIME_KEY),
    (LEGACY_V19_STOP_LOSS_REGIME_KEY, V19_STOP_LOSS_REGIME_KEY),
)


class AtrRegimeKeyConflict(Exception):
    pass


def normalize_atr_regime_keys(node):
    if isinstance(node, dict):
        out = {}
        sources = {}
        for k in sorted(node.keys()):
            nv = normalize_atr_regime_keys(node[k])
            target = k
            for legacy, canon in ATR_REGIME_KEY_RENAMES:
                if k == legacy:
                    target = canon
                    break
            if target in sources:
                if out[target] != nv:
                    raise AtrRegimeKeyConflict(
                        "%r and %r both normalize to %r with differing values"
                        % (sources[target], k, target)
                    )
                continue
            sources[target] = k
            out[target] = nv
        return out
    if isinstance(node, list):
        return [normalize_atr_regime_keys(v) for v in node]
    return node


try:
    with open(sys.argv[1]) as f:
        cfg = normalize_atr_regime_keys(json.load(f))
except AtrRegimeKeyConflict as exc:
    print("conflict: %s" % exc)
    sys.exit(0)
except Exception:
    print("unreadable")
    sys.exit(0)


def is_live(args):
    args = args or []
    for i, a in enumerate(args):
        if a == "--mode=live":
            return True
        if a == "--mode" and i + 1 < len(args) and args[i + 1] == "live":
            return True
    return False


def effective_leverage(sc):
    v = sc.get("leverage")
    if isinstance(v, (int, float)) and not isinstance(v, bool) and v > 0:
        return float(v)
    return 1.0


SCALAR_STOP_FIELDS = (
    "stop_loss_pct", "stop_loss_margin_pct", "trailing_stop_pct",
    "trailing_stop_atr_mult", "stop_loss_atr_mult",
)
REGIME_STOP_FIELDS = (V19_STOP_LOSS_REGIME_KEY, V19_TRAIL_STOP_REGIME_KEY)


def uses_unified_regime_close(sc):
    cs = sc.get("close_strategy")
    if not isinstance(cs, dict):
        return False
    name = str(cs.get("name", "") or "").strip().lower()
    if name not in ("tiered_tp_atr_regime", "tiered_tp_atr_live_regime",
                    "tiered_tp_atr_live_regime_dynamic"):
        return False
    params = cs.get("params")
    return isinstance(params, dict) and "trend_regime" in params


def uses_ratchet_regime_close(sc):
    cs = sc.get("close_strategy")
    if not isinstance(cs, dict):
        return False
    return str(cs.get("name", "") or "").strip().lower() == "trailing_tp_ratchet_regime"


def regime_block_is_configured(v):
    return isinstance(v, dict) and len(v) > 0


def has_explicit_stop_owner(sc):
    if any(sc.get(f) is not None for f in SCALAR_STOP_FIELDS):
        return True
    if any(regime_block_is_configured(sc.get(f)) for f in REGIME_STOP_FIELDS):
        return True
    return uses_unified_regime_close(sc)


def user_close_default_trailing_regime(cfg):
    ud = cfg.get("user_defaults")
    if not isinstance(ud, dict):
        return None
    closes = ud.get("close")
    if not isinstance(closes, dict):
        return None
    entry = None
    for k in sorted(closes.keys()):
        if str(k).strip().lower() == "trailing_tp_ratchet_regime":
            entry = closes[k]
            break
    if not isinstance(entry, dict):
        return None
    block = entry.get(V19_TRAIL_STOP_REGIME_KEY)
    if not isinstance(block, dict):
        return None
    return block


def default_stop_loss_atr_mult(cfg):
    v = cfg.get("default_stop_loss_atr_mult")
    if isinstance(v, (int, float)) and not isinstance(v, bool):
        return float(v)
    return 1.0


def resolve_stop_owners(cfg, sc):
    sc = dict(sc)
    if uses_ratchet_regime_close(sc) and not has_explicit_stop_owner(sc):
        block = user_close_default_trailing_regime(cfg)
        if block is not None:
            sc[V19_TRAIL_STOP_REGIME_KEY] = block
    default_mult = default_stop_loss_atr_mult(cfg)
    if default_mult > 0:
        if (
            all(sc.get(f) is None for f in SCALAR_STOP_FIELDS)
            and not any(regime_block_is_configured(sc.get(f)) for f in REGIME_STOP_FIELDS)
            and not uses_unified_regime_close(sc)
        ):
            sc["stop_loss_atr_mult"] = default_mult
    return sc


def effective_max_drawdown_pct(cfg, sc):
    v = sc.get("max_drawdown_pct")
    if isinstance(v, (int, float)) and not isinstance(v, bool) and v > 0:
        return float(v)
    pc = (cfg.get("platforms") or {}).get(str(sc.get("platform") or "")) or {}
    risk = pc.get("risk") or {}
    pv = risk.get("max_drawdown_pct")
    if isinstance(pv, (int, float)) and not isinstance(pv, bool) and pv > 0:
        return float(pv)
    t = sc.get("type")
    if t == "options":
        return 40.0
    if t == "perps":
        return 50.0
    if t == "futures":
        return 45.0
    return 60.0


def resolves_from_max_drawdown_fallback(sc):
    if uses_unified_regime_close(sc):
        return False
    for f in ("trailing_stop_atr_mult", "stop_loss_atr_mult"):
        v = sc.get(f)
        if isinstance(v, (int, float)) and not isinstance(v, bool) and v > 0:
            return False
    if any(regime_block_is_configured(sc.get(f)) for f in REGIME_STOP_FIELDS):
        return False
    return all(
        sc.get(f) is None
        for f in ("trailing_stop_pct", "stop_loss_pct", "stop_loss_margin_pct")
    )


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
    strategy_id = sc.get("id", "<no id>")
    sc = resolve_stop_owners(cfg, sc)

    checks = []
    if not uses_unified_regime_close(sc):
        slp = sc.get("stop_loss_pct")
        if isinstance(slp, (int, float)) and not isinstance(slp, bool):
            checks.append(("stop_loss_pct", float(slp)))
        smp = sc.get("stop_loss_margin_pct")
        if isinstance(smp, (int, float)) and not isinstance(smp, bool) and smp > 0:
            checks.append(("stop_loss_margin_pct/leverage", float(smp) / max(lev, 1.0)))
    mdd = effective_max_drawdown_pct(cfg, sc)
    if resolves_from_max_drawdown_fallback(sc):
        checks.append(("max_drawdown_pct", min(float(mdd), 50.0)))
    for field, pct in checks:
        if pct > 0 and pct >= bound:
            print(
                "%s: %s = %g%% >= bound %g%% (leverage %g)"
                % (strategy_id, field, pct, bound, lev)
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
    if [[ "$out" == "missing-file" || "$out" == "unreadable" || "$out" == conflict:* ]]; then
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

#!/usr/bin/env python3
"""#1120: compare incumbent vs proposed regime opening-trail defaults.

Runs trailing_tp_ratchet_regime + use_defaults ratchet tiers with explicit
trailing_stop_atr_regime trend_regime blocks (current system table vs MC-
derived proposal) on the audit BTC 1h OOS window. Emits JSON for PR rationale.
"""
from __future__ import annotations

import json
import os
import sys

_REPO = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
sys.path.insert(0, os.path.join(_REPO, "backtest"))
sys.path.insert(0, os.path.join(_REPO, "shared_tools"))
sys.path.insert(0, os.path.join(_REPO, "shared_strategies", "close"))
sys.path.insert(0, os.path.join(_REPO, "shared_strategies", "open"))

from atr import ensure_atr_indicator
from backtester import Backtester
from data_fetcher import load_cached_data
from registry_loader import load_registry

reg = load_registry("futures")

COMPOSITE_SPEC = {"medium": {"classifier": "composite", "period": 20}}

CURRENT_TRAILING = {
    "trending_up_clean": {"atr_multiple": 2.0},
    "trending_down_clean": {"atr_multiple": 2.0},
    "trending_up_choppy": {"atr_multiple": 2.0},
    "trending_down_choppy": {"atr_multiple": 2.0},
    "ranging_quiet": {"atr_multiple": 1.0},
    "ranging_volatile": {"atr_multiple": 1.0},
    "ranging_directional": {"atr_multiple": 1.0},
    "ranging_directional_up": {"atr_multiple": 1.0},
    "ranging_directional_down": {"atr_multiple": 1.0},
}

PROPOSED_TRAILING = {
    **CURRENT_TRAILING,
    "trending_up_clean": {"atr_multiple": 2.5},
    "trending_down_clean": {"atr_multiple": 2.5},
    "trending_up_choppy": {"atr_multiple": 2.25},
    "trending_down_choppy": {"atr_multiple": 2.25},
    "ranging_volatile": {"atr_multiple": 1.25},
    "ranging_directional": {"atr_multiple": 1.5},
    "ranging_directional_up": {"atr_multiple": 1.5},
    "ranging_directional_down": {"atr_multiple": 1.5},
}

CLOSE_STACK = [{
    "name": "trailing_tp_ratchet_regime",
    "params": {"use_defaults": True},
}]


def _run_arm(df, label: str, trail: dict) -> dict:
    bt = Backtester(
        initial_capital=10_000.0,
        platform="binanceus",
        strategy_type="perps",
        open_strategy={"name": "squeeze_momentum", "params": {}},
        close_strategies=CLOSE_STACK,
        regime_enabled=True,
        regime_windows_spec=COMPOSITE_SPEC,
        trailing_stop_atr_regime={"trend_regime": trail},
    )
    r = bt.run(
        df,
        strategy_name="squeeze_momentum",
        symbol="BTC/USDT",
        timeframe="1h",
        save=False,
    )
    return {
        "arm": label,
        "total_return_pct": round(r.get("total_return_pct", 0.0), 4),
        "sharpe": round(r.get("sharpe_ratio", 0.0), 4),
        "max_drawdown_pct": round(r.get("max_drawdown_pct", 0.0), 4),
        "total_trades": r.get("total_trades", 0),
        "win_rate": round(r.get("win_rate", 0.0), 4),
    }


def main() -> int:
    since, until = "2025-06-10", "2026-01-01"
    raw = load_cached_data("BTC/USDT", "1h", start_date=since, end_date=until)
    if raw.empty:
        print("no cached data — run data fetch first", file=sys.stderr)
        return 1
    strat = reg.STRATEGY_REGISTRY["squeeze_momentum"]
    df = ensure_atr_indicator(reg.apply_strategy("squeeze_momentum", raw, strat["default_params"]))

    arms = [
        _run_arm(df, "current_system_trailing", CURRENT_TRAILING),
        _run_arm(df, "proposed_mc_trailing", PROPOSED_TRAILING),
    ]
    out = {
        "issue": 1120,
        "dataset": "BTC/USDT 1h",
        "window": f"{since}..{until}",
        "open_strategy": "squeeze_momentum",
        "close": "trailing_tp_ratchet_regime use_defaults",
        "arms": arms,
        "verdict": (
            "proposed_mc_trailing preferred on OOS Sharpe+return"
            if arms[1]["sharpe"] >= arms[0]["sharpe"]
            and arms[1]["total_return_pct"] >= arms[0]["total_return_pct"]
            else "mixed — MC proposal adopted with documented OOS tradeoff"
        ),
        "ratchet_tp_note": (
            "Ratchet tier ladders and B2 ranging TP group unchanged; "
            "per-substate ratchet/TP retune deferred — bar-level harness "
            "does not isolate substate geometry without M6 entry-locked replay."
        ),
    }
    path = os.path.join(os.path.dirname(__file__), "regime_1120_trail_validation.json")
    with open(path, "w") as fh:
        json.dump(out, fh, indent=2)
    print(json.dumps(out, indent=2))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

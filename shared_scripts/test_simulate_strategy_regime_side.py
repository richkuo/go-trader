import json
import math
import os
import subprocess
import sys

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
SCRIPT = os.path.join(ROOT, "shared_scripts", "simulate_strategy.py")
N = 300
CLOSE = {"name": "tiered_tp_atr", "params": {"tp_tiers": [{"atr_multiple": 50.0, "close_fraction": 1.0}]}}
REGIME = {"enabled": True, "period": 14, "adx_threshold": 20,
          "windows": {"medium": {"classifier": "composite", "period": 20}}}


def _candles(drift):
    closes = [300.0 + drift * i + 3.0 * math.sin(2 * math.pi * i / 12) for i in range(N)]
    return [{"time": 1704067200 + i * 3600, "open": closes[i - 1] if i else closes[0],
             "high": closes[i] + 0.5, "low": closes[i] - 0.5, "close": closes[i], "volume": 10}
            for i in range(N)]


def _config(**extra):
    cfg = {"type": "perps", "platform": "hyperliquid", "symbol": "BTC", "timeframe": "1h",
           "strategy": "rsi", "open_strategy": {"name": "rsi"}, "close_strategy": CLOSE,
           "regime": REGIME}
    cfg.update(extra)
    return cfg


def _side(**extra):
    fields = {"regime_side": True, "direction": "both",
              "regime_side_certification": {"context": "paper"}}
    fields.update(extra)
    return _config(**fields)


def _run(candles, arms):
    payload = {"candles": candles, "configs": [{"label": k, "config": v} for k, v in arms.items()]}
    proc = subprocess.run([sys.executable, SCRIPT], input=json.dumps(payload),
                          capture_output=True, text=True, cwd=ROOT, timeout=600)
    assert proc.returncode == 0, proc.stdout + proc.stderr
    return json.loads(proc.stdout)


def _entry_sides(markers):
    return sorted({m["side"] for m in markers if not m["is_close"]})


def test_both_preview_arms_use_their_own_regime_side_configuration():
    out = _run(_candles(-0.6), {"current": _config(), "proposed": _side()})
    assert _entry_sides(out["markers"]["current"]) == ["buy"]
    assert _entry_sides(out["markers"]["proposed"]) == ["sell"]
    summary = out["regime_side"]["proposed"]
    assert summary["context"] == "paper"
    assert summary["counts"]["opened_short"] >= 1
    assert "current" not in out["regime_side"]
    rising = _run(_candles(0.6), {"proposed": _side()})
    assert _entry_sides(rising["markers"]["proposed"]) == ["buy"]


def test_preview_live_context_needs_exact_label_evidence_or_acknowledgment():
    live_empty = {"context": "live", "states": {}, "active_from": "2023-12-01T00:00:00Z"}
    certified = {"context": "live", "active_from": "2023-12-01T00:00:00Z",
                 "states": {"trending_down_clean": "short", "trending_down_choppy": "short"}}
    out = _run(_candles(-0.6), {
        "empty": _side(regime_side_certification=live_empty),
        "acknowledged": _side(regime_side_certification=live_empty, allow_uncertified_regime_side=True),
        "certified": _side(regime_side_certification=certified),
        "base_long": _side(direction="long"),
    })
    assert _entry_sides(out["markers"]["empty"]) == []
    assert out["regime_side"]["empty"]["counts"]["held:uncertified"] >= 1
    assert _entry_sides(out["markers"]["acknowledged"]) == ["sell"]
    assert _entry_sides(out["markers"]["certified"]) == ["sell"]
    assert _entry_sides(out["markers"]["base_long"]) == []


def test_unpreviewable_arms_refuse_without_blocking_the_other_arm():
    arms = {
        "ok": _side(),
        "no_direction": _side(direction=None),
        "no_certification": _config(regime_side=True, direction="both"),
        "invert": _side(invert_signal=True),
        "open_as_close": _side(close_strategy={"name": "rsi"}),
        "ack_only": _config(allow_uncertified_regime_side=True),
        "spot": _side(type="spot", platform="binanceus"),
        "other_regime_timeframe": _side(regime=dict(REGIME, timeframe="4h")),
    }
    out = _run(_candles(-0.6), arms)
    assert sorted(out["refusals"]) == sorted(k for k in arms if k != "ok")
    assert sorted(out["markers"]) == ["ok"]
    assert _entry_sides(out["markers"]["ok"]) == ["sell"]

import json
import sys
import os

import pandas as pd
import pytest

import parity_diff
from registry_loader import load_registry

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
N = 300
WINDOW = 200
TRIGGER_BAR = 250
REGIME = {"enabled": True, "period": 14, "adx_threshold": 20,
          "windows": {"medium": {"classifier": "composite", "period": 20}}}
CLOSE = {"name": "tiered_tp_atr", "params": {"tp_tiers": [{"atr_multiple": 50.0, "close_fraction": 1.0}]}}


def _frame(drift, wiggle):
    idx = pd.date_range("2024-01-01", periods=N, freq="1h")
    close = pd.Series([300.0 + drift * i + (wiggle if i % 2 == 0 else -wiggle) for i in range(N)], index=idx)
    return pd.DataFrame({"open": close.shift(1).fillna(close.iloc[0]), "high": close + 0.5,
                         "low": close - 0.5, "close": close, "volume": 10.0})


class _TimestampTriggers:
    def __init__(self, triggers):
        self._real = load_registry("futures")
        self._triggers = triggers

    def __getattr__(self, name):
        return getattr(self._real, name)

    def apply_strategy(self, name, df, params=None):
        out = self._real.apply_strategy(name, df, params)
        out["signal"] = [self._triggers.get(ts, 0) for ts in out.index]
        return out


def _config(tmp_path, **extra):
    sc = {"id": "hl-side", "type": "perps", "platform": "hyperliquid",
          "args": ["sma_crossover", "BTC", "1h", "--mode=paper"],
          "open_strategy": {"name": "sma_crossover"}, "close_strategy": CLOSE,
          "direction": "both", "leverage": 1, "regime_side": True}
    sc.update(extra)
    path = tmp_path / "config.json"
    path.write_text(json.dumps({"config_version": 15, "regime": REGIME, "strategies": [sc]}))
    return str(path)


def test_engine_and_live_arms_agree_on_the_mapped_side(monkeypatch, tmp_path, subtests):
    cfg = parity_diff.config_from_live_config(_config(tmp_path), "hl-side")
    assert cfg.regime_side is True
    for (drift, wiggle), side in (((-0.6, 1.5), "short"), ((0.6, 0.0), "long")):
        frame = _frame(drift, wiggle)
        for trigger in (1, -1):
            with subtests.test(side=side, trigger=trigger):
                stamp = frame.index[TRIGGER_BAR]
                monkeypatch.setattr(parity_diff, "load_registry",
                                    lambda *_a, **_k: _TimestampTriggers({stamp: trigger}))
                out = parity_diff.compute_parity_frame(frame, cfg=cfg, window=WINDOW)
                assert int((~out["match"]).sum()) == 0
                row = out[out["ts"] == stamp].iloc[0]
                assert row["bt_open_side"] == side and row["live_open_side"] == side
                assert row["live_open_action"] == side and row["bt_open_action"] == side
                after = out[out["ts"] > stamp]
                assert set(after["live_open_side"]) == {"none"}
                assert int((after["live_signal"] != 0).sum()) == 0


def test_live_equivalence_holds_without_evidence_in_both_arms(monkeypatch, tmp_path):
    monkeypatch.setenv("GO_TRADER_DIRECTIONAL_CERT_PATH", str(tmp_path / "absent.json"))
    cfg = parity_diff.config_from_live_config(
        _config(tmp_path, args=["sma_crossover", "BTC", "1h", "--mode=live"]), "hl-side")
    assert cfg.regime_side_certification["context"] == "live"
    frame = _frame(-0.6, 1.5)
    stamp = frame.index[TRIGGER_BAR]
    monkeypatch.setattr(parity_diff, "load_registry", lambda *_a, **_k: _TimestampTriggers({stamp: 1}))
    out = parity_diff.compute_parity_frame(frame, cfg=cfg, window=WINDOW)
    assert int((~out["match"]).sum()) == 0
    row = out[out["ts"] == stamp].iloc[0]
    assert row["live_open_side"] == "none" and row["bt_open_side"] == "none"
    assert int(row["live_signal"]) == 0


def test_batched_slot_carries_the_open_side_request(tmp_path):
    cfg = parity_diff.config_from_live_config(_config(tmp_path), "hl-side")
    frame = _frame(-0.6, 1.5).iloc[:WINDOW]
    for open_side in ("short", "none"):
        solo, batched = parity_diff._batched_bar_decisions(frame, cfg, open_side=open_side)
        assert solo == batched
        if open_side == "none":
            assert solo["signal"] == 0
        else:
            assert solo["signal"] in (0, -1)


def test_unmodeled_combinations_refuse(tmp_path, subtests):
    with subtests.test(case="htf_filter"), pytest.raises(ValueError):
        parity_diff.config_from_live_config(_config(tmp_path, htf_filter=True), "hl-side")
    with subtests.test(case="no_label_plan"), pytest.raises(ValueError):
        parity_diff.ParityConfig(strategy_name="sma_crossover", regime_side=True, direction="both",
                                 close_refs=[CLOSE], regime_enabled=True,
                                 regime_side_certification={"context": "paper"})
    with subtests.test(case="cli_exit_code"):
        rc = parity_diff.main(["--config", _config(tmp_path, htf_filter=True), "--strategy-id", "hl-side"])
        assert rc == 2

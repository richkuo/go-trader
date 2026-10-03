import hashlib
import json
import os

import numpy as np
import pandas as pd
import pytest

import offline_manifest as om
import data_fetcher
import eval_windows
from backtester import Backtester
from registry_loader import load_registry

HOUR = 3_600_000
T0 = 1_767_225_600_000


def _spec(**kw):
    base = {"taker_fee_pct": 0.0, "maker_fee_pct": 0.0, "half_spread_pct": 0.0,
            "slippage_pct": 0.0, "size_decimals": 2, "min_notional_usd": 10.0,
            "min_notional_margin": 0.0}
    base.update(kw)
    return base


def _engine_df(prices, actions=None, fractions=None):
    n = len(prices)
    px = np.asarray(prices, dtype=float)
    df = pd.DataFrame({"open": px, "high": px, "low": px, "close": px,
                       "volume": np.ones(n)},
                      index=pd.date_range("2026-01-01", periods=n, freq="1h"))
    df["open_action"] = list(actions) if actions is not None else ["none"] * n
    if fractions is not None:
        df["close_fraction"] = list(fractions)
    return df


def _run(df, spec, capital=1000.0, **kw):
    bt = Backtester(initial_capital=capital, platform="hyperliquid",
                    execution_spec=spec, **kw)
    return bt.run(df, strategy_name="engine", symbol="BTC/USDT", timeframe="1h", save=False)


def test_exact_lot_entry_and_fee_accounting():
    df = _engine_df([100.0] * 5, ["long"] + ["none"] * 4)
    res = _run(df, _spec())
    assert res["trades"][0]["shares"] == 10.0
    assert res["final_capital"] == 1000.0
    assert res["execution"]["rejected_entry_count"] == 0

    taker = 0.00045
    res = _run(df, _spec(taker_fee_pct=taker))
    t = res["trades"][0]
    assert t["shares"] == 9.99
    assert t["entry_fee"] == pytest.approx(9.99 * 100 * taker, abs=1e-6)
    assert t["exit_fee"] == pytest.approx(9.99 * 100 * taker, abs=1e-6)
    assert res["final_capital"] == pytest.approx(1000.0 - 2 * 9.99 * 100 * taker, abs=0.01)
    assert res["execution"]["entry_lot_residual_qty"] == pytest.approx(1000 / (1 + taker) / 100 - 9.99)


def test_short_entry_fee_and_cash_are_consistent():
    df = _engine_df([100.0, 100.0, 90.0, 90.0], ["short", "none", "none", "none"])
    res = _run(df, _spec(taker_fee_pct=0.001))
    t = res["trades"][0]
    assert t["side"] == "short"
    qty = t["shares"]
    assert qty == 9.99
    expected = 1000.0 + qty * (100.0 - 90.0) - qty * 100.0 * 0.001 - qty * 90.0 * 0.001
    assert res["final_capital"] == pytest.approx(expected, abs=0.01)


def test_below_lot_and_min_notional_entries_rejected_and_recorded():
    df = _engine_df([100.0] * 4, ["long", "none", "none", "none"])
    res = _run(df, _spec(size_decimals=0), capital=50.0)
    assert res["trades"] == []
    assert res["execution"]["rejected_entries"][0]["reason"] == "below_lot"

    margin = {"size_decimals": 4, "min_notional_margin": 0.03}
    below = _run(df, _spec(**margin), capital=10.29)
    assert below["trades"] == []
    assert below["execution"]["rejected_entries"][0]["reason"] == "below_min_notional"
    assert below["execution"]["rejected_entries"][0]["threshold_usd"] == pytest.approx(10.3)
    above = _run(df, _spec(**margin), capital=10.31)
    assert above["trades"][0]["shares"] == 0.1031


def test_partial_close_lot_floor_min_gate_and_full_close_below_minimum():
    prices = [100.0] * 6 + [1.5] * 5
    fractions = [0, 0.05, 0, 0.55, 0, 0, 0.5, 0, 1.0, 0, 0]
    df = _engine_df(prices, ["long"] + ["none"] * 10, fractions)
    res = _run(df, _spec(size_decimals=0, min_notional_margin=0.03))
    ex = res["execution"]
    gates = [(s["gate"], s["requested_qty"], s["floored_qty"]) for s in ex["skipped_partial_closes"]]
    assert gates[0] == ("below_lot", pytest.approx(0.5), 0.0)
    assert gates[1][0] == "below_min_notional" and gates[1][2] == 2.0
    assert ex["close_residuals"][0]["residual_qty"] == pytest.approx(0.5)
    shares = [t["shares"] for t in res["trades"]]
    assert shares == [5.0, 5.0]
    assert res["trades"][1]["exit_price"] == 1.5
    assert res["trades"][1]["exit_reason"] != "end_of_data"


def test_resting_take_profit_uses_maker_fee():
    prices = [100.0, 100.0, 100.0, 103.0, 103.0]
    df = _engine_df(prices, ["long"] + ["none"] * 4)
    df["atr"] = 1.0
    res = _run(df, _spec(taker_fee_pct=0.0005, maker_fee_pct=0.0001),
               close_strategies=[{"name": "tiered_tp_atr",
                                  "params": {"tp_tiers": [{"atr_multiple": 2.0, "close_fraction": 1.0}]}}])
    t = res["trades"][0]
    assert t["exit_price"] == pytest.approx(102.0)
    assert t["exit_fee"] == pytest.approx(t["shares"] * 102.0 * 0.0001)
    assert t["entry_fee"] == pytest.approx(t["shares"] * 100.0 * 0.0005)


def test_spec_guards():
    df = _engine_df([100.0] * 3, ["long", "none", "none"])
    with pytest.raises(ValueError, match="charged twice"):
        Backtester(execution_spec=_spec(), commission_pct=0.001)
    with pytest.raises(ValueError, match="charged twice"):
        Backtester(execution_spec=_spec(), slippage_pct=0.001)
    with pytest.raises(ValueError, match="keys must be exactly"):
        Backtester(execution_spec={**_spec(), "lot": 1})
    with pytest.raises(ValueError, match="integer"):
        Backtester(execution_spec=_spec(size_decimals=1.5))
    plain = df.drop(columns=["open_action"]).assign(signal=[1, 0, 0])
    with pytest.raises(ValueError, match="open/close engine"):
        _run(plain, _spec())
    with pytest.raises(ValueError, match="scale-in"):
        _run(df, _spec(), allow_scale_in=True)


def _sha(path):
    return om.sha256_file(path)


def _build_manifest(tmp_path, n_bars=400, funding_drop=(), funding=True, meta_dec=5,
                    windows=None):
    data = tmp_path / "data"
    data.mkdir()
    rng = np.random.RandomState(3)
    close = 100 + np.cumsum(rng.randn(n_bars) * 0.8)
    open_ = np.concatenate([[close[0]], close[:-1]])
    high = np.maximum(open_, close) + 0.5
    low = np.minimum(open_, close) - 0.5
    ts = T0 + HOUR * np.arange(n_bars)
    candles = pd.DataFrame({"timestamp": ts, "open": open_, "high": high, "low": low,
                            "close": close, "volume": 10 + rng.rand(n_bars)})
    om._write_csv_gz(candles, str(data / "BTC.csv.gz"))
    rates = pd.DataFrame({"timestamp": ts + 1, "rate": np.full(n_bars, 0.0001)})
    rates = rates.drop(index=list(funding_drop)).reset_index(drop=True)
    om._write_csv_gz(rates, str(data / "BTC_f.csv.gz"))
    (data / "meta.json").write_text(json.dumps({"universe": [{"name": "BTC", "szDecimals": meta_dec}]}))
    quote = "Order MinTradeNtl Order must have minimum value of $10."
    windows = windows or {
        "train": {"start": pd.Timestamp(T0 + HOUR * 100, unit="ms").isoformat(),
                  "end": pd.Timestamp(T0 + HOUR * 250, unit="ms").isoformat()},
        "test": {"start": pd.Timestamp(T0 + HOUR * 250, unit="ms").isoformat(),
                 "end": pd.Timestamp(T0 + HOUR * 400, unit="ms").isoformat()},
    }
    manifest = {
        "schema": om.SCHEMA, "study": "fixture", "venue": "hyperliquid",
        "provenance": {"kind": "venue", "label": "fixture candles"},
        "interval": "1h", "warmup_bars": 60, "windows": windows,
        "costs": {"taker_fee_pct": 0.00045, "maker_fee_pct": 0.00015, "slippage_bps": 5.0,
                  "min_notional_usd": 10.0, "min_notional_margin": 0.03},
        "venue_reference": {
            "meta": {"path": "data/meta.json", "sha256": _sha(data / "meta.json")},
            "documents": [{"url": "https://example.invalid", "quote": quote,
                           "quote_sha256": hashlib.sha256(quote.encode()).hexdigest()}],
        },
        "datasets": [{"coin": "BTC", "size_decimals": 5, "half_spread_bps": 0.5,
                      "candles": {"path": "data/BTC.csv.gz", "sha256": _sha(data / "BTC.csv.gz")},
                      "funding": ({"path": "data/BTC_f.csv.gz", "sha256": _sha(data / "BTC_f.csv.gz")}
                                  if funding else None)}],
    }
    path = tmp_path / "manifest.json"
    path.write_text(json.dumps(manifest))
    return path


def test_manifest_loads_and_reports_complete_coverage(tmp_path):
    m = om.load_manifest(str(_build_manifest(tmp_path)))
    report = om.verify_report(m)
    win = report["datasets"]["BTC 1h"]["windows"]["train"]
    assert win["candles"]["complete"] and win["candles"]["present_bars"] == 150
    assert win["funding"]["complete"] and win["funding"]["present_hours"] == 150


@pytest.mark.parametrize("damage,match", [
    ("tamper", "sha256 mismatch"),
    ("missing", "is missing"),
    ("escape", "escapes"),
    ("quote", "quote hash mismatch"),
])
def test_manifest_fails_closed_on_bad_inputs(tmp_path, damage, match):
    path = _build_manifest(tmp_path)
    raw = json.loads(path.read_text())
    if damage == "tamper":
        with open(tmp_path / "data" / "BTC.csv.gz", "ab") as fh:
            fh.write(b"x")
    elif damage == "missing":
        os.remove(tmp_path / "data" / "BTC_f.csv.gz")
    elif damage == "escape":
        raw["datasets"][0]["candles"]["path"] = "../outside.csv.gz"
    else:
        raw["venue_reference"]["documents"][0]["quote"] += " edited"
    path.write_text(json.dumps(raw))
    with pytest.raises(om.ManifestError, match=match):
        om.load_manifest(str(path))


def test_manifest_lot_size_must_match_pinned_meta(tmp_path):
    with pytest.raises(om.ManifestError, match="pinned venue meta"):
        om.load_manifest(str(_build_manifest(tmp_path, meta_dec=4)))


def test_insufficient_warmup_fails_closed(tmp_path):
    windows = {"early": {"start": pd.Timestamp(T0 + HOUR * 30, unit="ms").isoformat(),
                         "end": pd.Timestamp(T0 + HOUR * 200, unit="ms").isoformat()}}
    m = om.load_manifest(str(_build_manifest(tmp_path, windows=windows)))
    with pytest.raises(om.ManifestError, match="warm-up"):
        om.window_frame(m, m["datasets"][0], "early")


def test_funding_gaps_and_absence_are_never_verified_zero(tmp_path):
    m = om.load_manifest(str(_build_manifest(tmp_path, funding_drop=(120, 121, 122))))
    ds = m["datasets"][0]
    frame, win, _ = om.window_frame(m, ds, "train")
    _, cov = om.attach_funding_cost(frame, ds, win)
    assert not cov["complete"] and cov["missing_hours"] == 3 and cov["max_gap_hours"] == 3

    other = tmp_path / "nofunding"
    other.mkdir()
    m2 = om.load_manifest(str(_build_manifest(other, funding=False)))
    ds2 = m2["datasets"][0]
    frame2, win2, _ = om.window_frame(m2, ds2, "train")
    out, cov2 = om.attach_funding_cost(frame2, ds2, win2)
    assert cov2["available"] is False and cov2["complete"] is False
    assert "funding_accrual" not in out.columns


def test_manifest_funding_charges_long_and_credits_short(tmp_path):
    m = om.load_manifest(str(_build_manifest(tmp_path)))
    ds = m["datasets"][0]
    frame, win, _ = om.window_frame(m, ds, "train")
    frame, _ = om.attach_funding_cost(frame, ds, win)
    scored = om.slice_window(frame, win)
    assert np.allclose(scored["funding_accrual"].to_numpy(), 0.0001, rtol=1e-9, atol=0.0)
    for side, sign in (("long", -1), ("short", 1)):
        df = scored.copy()
        df["open_action"] = [side] + ["none"] * (len(df) - 1)
        res = _run(df, _spec(size_decimals=5))
        qty = res["trades"][0]["shares"]
        expected = qty * float((df["close"] * df["funding_accrual"]).iloc[2:].sum())
        assert np.sign(res["total_funding_pnl"]) == sign
        assert abs(res["total_funding_pnl"]) == pytest.approx(expected, abs=1e-4)


def test_eval_windows_manifest_leg_is_offline_and_repeatable(tmp_path, monkeypatch):
    def _no_loader(*a, **k):
        raise AssertionError("manifest path must not call the network loader")
    monkeypatch.setattr(data_fetcher, "load_cached_data", _no_loader)
    m = om.load_manifest(str(_build_manifest(tmp_path)))
    reg = load_registry("futures")
    ctx = {"manifest": m, "window": "test", "cost_multiplier": 1.0}
    close = [{"name": "time_stop", "params": {"max_bars": 5}}]
    leg = eval_windows.run_leg(reg, "breakout", None, "BTC", "1h", (None, None),
                               close_strategies=close, direction="both",
                               stop_loss_atr_mult=1.0, manifest_ctx=ctx)
    again = eval_windows.run_leg(reg, "breakout", None, "BTC", "1h", (None, None),
                                 close_strategies=close, direction="both",
                                 stop_loss_atr_mult=1.0, manifest_ctx=ctx)
    assert leg == again
    assert leg["manifest"]["cost_model"] == "execution_spec"
    assert leg["manifest"]["candle_coverage"]["present_bars"] == 150
    assert leg["span_days"] == pytest.approx(149 / 24, abs=1e-3)
    plain = eval_windows.run_leg(reg, "sma_crossover", None, "BTC", "1h", (None, None),
                                 manifest_ctx=ctx)
    assert plain["manifest"]["cost_model"] == "legacy_flat"
    with pytest.raises(ValueError, match="funding as an entry input"):
        eval_windows.run_leg(reg, "funding_skew", None, "BTC", "1h", (None, None),
                             manifest_ctx=ctx)

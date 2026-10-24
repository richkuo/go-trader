import importlib.util
import os

import numpy as np
import pandas as pd
import pytest

_HERE = os.path.dirname(os.path.abspath(__file__))


def _load(name: str, path: str):
    spec = importlib.util.spec_from_file_location(name, path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


_REGISTRY = _load(
    "_registry_forward_peek", os.path.join(_HERE, "registry.py")
)

SKIP_STRATEGIES = {
    "basket_scripted_fixture": "basket evaluator: it ranks a symbol-keyed universe and refuses one-symbol evaluation by contract (#1781)",
    "cross_sectional_momentum": "basket evaluator: it ranks a symbol-keyed universe and refuses one-symbol evaluation by contract (#1781); its post-deadline invariance runs through the real basket runner (#1787)",
    "cross_sectional_reversal": "basket evaluator: it ranks a symbol-keyed universe and refuses one-symbol evaluation by contract (#1781); its post-deadline invariance runs through the real basket runner (#1793)",
    "order_book_imbalance": "book evaluator: it decides once, at the deadline of the supplied l2Book section, and never per candle row (#1635); its post-deadline invariance runs through the evaluator and study-path parity in test_order_book_imbalance_integration.py",
    "inventory_market_making": "quote capability: it has no scalar candle signal and refuses scalar evaluation by contract (#1861); its post-deadline invariance runs through the real quote coordinator in the TestQuoteReplay integration cases (later receipts leave past decisions unchanged)",
    "cross_exchange_perp_arbitrage": "pair policy: it decides one cross-venue pair from a frozen two-venue request and refuses one-symbol evaluation by contract (#1878); its post-deadline invariance runs through the real pair runner in TestBasketPairPolicyRealFrozenRequestIgnoresLaterInputs (identical frozen request bytes give identical output after later quote and account inputs change)",
}

EXPECTED_ALL_ZERO_SIGNAL = {
    "hold",
}


N_BARS = 960
_SEED = 20261279


def _make_close(rng: np.random.RandomState, n: int) -> np.ndarray:
    t = np.arange(n)
    trend = 100.0 + 0.02 * t
    swings = 6.0 * np.sin(t / 24.0) + 3.0 * np.sin(t / 7.0)
    seg = (t // 96) % 4
    vol = np.choose(seg, [0.08, 0.4, 1.2, 0.4])
    noise = (rng.randn(n) * vol).cumsum() * 0.6
    shocks = np.zeros(n)
    if n > 96:
        shock_bars = rng.choice(np.arange(48, n), size=max(4, n // 160), replace=False)
        shocks[shock_bars] = rng.randn(len(shock_bars)) * 8.0
    return trend + swings + noise + shocks.cumsum() * 0.5


def _make_ohlcv(rng: np.random.RandomState, n: int, start: str) -> pd.DataFrame:
    close = _make_close(rng, n)
    open_ = np.concatenate([[close[0]], close[:-1]])
    span = np.abs(rng.randn(n)) * 0.6 + 0.2
    high = np.maximum(open_, close) + span
    low = np.minimum(open_, close) - span
    volume = 100.0 + np.abs(rng.randn(n)) * 40.0 + 30.0 * (np.sin(np.arange(n) / 5.0) + 1.0)
    idx = pd.date_range(start, periods=n, freq="1h")
    df = pd.DataFrame(
        {"open": open_, "high": high, "low": low, "close": close, "volume": volume},
        index=idx,
    )
    df["close_b"] = close * 0.5 + rng.randn(n).cumsum() * 0.3 + 10.0
    df["funding_rate"] = 0.00005 + 0.00008 * np.sin(np.arange(n) / 30.0) + rng.randn(n) * 0.00002
    return df


def _fixture_df() -> pd.DataFrame:
    return _make_ohlcv(np.random.RandomState(_SEED), N_BARS, "2026-01-01")


def _perturbed_df(df: pd.DataFrame, cut: int) -> pd.DataFrame:
    tail = _make_ohlcv(
        np.random.RandomState(_SEED + 1), len(df) - cut, str(df.index[cut])
    )
    scale = df["close"].iloc[cut] / tail["close"].iloc[0]
    out = df.copy()
    for col in ("open", "high", "low", "close", "close_b"):
        if col in out.columns:
            out.iloc[cut:, out.columns.get_loc(col)] = tail[col].to_numpy() * scale
    for col in ("volume", "funding_rate"):
        if col in out.columns:
            out.iloc[cut:, out.columns.get_loc(col)] = tail[col].to_numpy() * 1.3
    return out


_DF = _fixture_df()


def _range_scalper_fixture() -> pd.DataFrame:
    rng = np.random.RandomState(_SEED + 2)
    n = 400
    close = 100.0 + rng.randn(n) * 0.02
    volume = np.full(n, 60.0)
    volume[::7] = 200.0
    for k in range(60, n, 60):
        for j in range(7):
            close[k - 6 + j] = close[k - 7] - 0.04 * (j + 1)
            close[k + 1 + j] = close[k - 7] + 0.04 * (j + 1)
    idx = pd.date_range("2026-01-01", periods=n, freq="5min")
    return pd.DataFrame(
        {"open": close, "high": close + 0.02, "low": close - 0.02,
         "close": close, "volume": volume},
        index=idx,
    )


def _sweep_squeeze_combo_fixture() -> pd.DataFrame:
    rng = np.random.RandomState(_SEED + 3)
    n = 400
    close = 100.0 + 6.0 * np.sin(2 * np.pi * np.arange(n) / 125.0) + rng.randn(n) * 0.3
    df = pd.DataFrame(
        {"open": close, "high": close + 0.4, "low": close - 0.4,
         "close": close, "volume": np.full(n, 100.0)},
        index=pd.date_range("2026-01-01", periods=n, freq="15min"),
    )
    stoch = _REGISTRY.STRATEGIES["stoch_rsi"]
    sr = pd.to_numeric(
        stoch["fn"](df.copy(), **stoch["default_params"])["signal"], errors="coerce"
    ).fillna(0).to_numpy()
    for b in np.flatnonzero(sr):
        if b < 40:
            continue
        if sr[b] == 1:
            df.iloc[b, df.columns.get_loc("low")] = df["low"].iloc[b - 25:b].min() - 1.0
        else:
            df.iloc[b, df.columns.get_loc("high")] = df["high"].iloc[b - 25:b].max() + 1.0
    return df


def _ultimate_oscillator_fixture() -> pd.DataFrame:
    rng = np.random.RandomState(_SEED + 4)
    n = 600
    t = np.arange(n)
    drift = np.where((t // 60) % 2 == 0, 0.45, -0.45)
    close = 200.0 + np.cumsum(drift + rng.randn(n) * 0.6)
    open_ = np.concatenate([[close[0]], close[:-1]])
    span = np.abs(rng.randn(n)) * 0.5 + 0.1
    return pd.DataFrame(
        {"open": open_, "high": np.maximum(open_, close) + span,
         "low": np.minimum(open_, close) - span, "close": close, "volume": np.full(n, 100.0)},
        index=pd.date_range("2026-01-01", periods=n, freq="1h"),
    )


def _mass_index_reversal_fixture() -> pd.DataFrame:
    rng = np.random.RandomState(_SEED + 5)
    n = N_BARS
    t = np.arange(n)
    drift = np.where(((t + 8) // 80) % 2 == 0, 0.6, -0.6)
    close = 200.0 + np.cumsum(drift + rng.randn(n) * 0.3)
    open_ = np.concatenate([[close[0]], close[:-1]])
    span = np.abs(rng.randn(n)) * 0.3 + 0.4
    span = np.where(((t % 80) >= 60) & ((t % 80) < 70), span * 4.0, span)
    return pd.DataFrame(
        {"open": open_, "high": np.maximum(open_, close) + span,
         "low": np.minimum(open_, close) - span, "close": close, "volume": np.full(n, 100.0)},
        index=pd.date_range("2026-01-01", periods=n, freq="1h"),
    )


def _calendar_conditioned_entry_fixture() -> pd.DataFrame:
    rng = np.random.RandomState(_SEED + 5)
    n = 6000
    idx = pd.date_range("2026-01-05", periods=n, freq="1h")
    ret = rng.randn(n) * 0.003
    cell = idx.dayofweek * 24 + idx.hour
    ret[cell == 14] += 0.006
    ret[cell == 3 * 24 + 3] -= 0.006
    close = 100.0 * np.exp(np.cumsum(ret))
    open_ = np.concatenate([[100.0], close[:-1]])
    span = np.abs(rng.randn(n)) * 0.001 + 0.0005
    return pd.DataFrame(
        {"open": open_, "high": np.maximum(open_, close) * (1 + span),
         "low": np.minimum(open_, close) * (1 - span), "close": close, "volume": np.full(n, 100.0)},
        index=idx,
    )


def _cvd_fixture() -> pd.DataFrame:
    df = _DF[["open", "high", "low", "close", "volume"]].copy()
    df.index = pd.date_range("2026-01-01", periods=len(df), freq="5min")
    return df


def _vpr_fixture() -> pd.DataFrame:
    df = _DF[["open", "high", "low", "close", "volume"]].copy()
    df.index = pd.date_range("2026-01-01", periods=len(df), freq="15min")
    return df


def _premium_fixture() -> pd.DataFrame:
    df = _DF[["open", "high", "low", "close", "volume"]].copy()
    df.index = pd.date_range("2026-01-01", periods=len(df), freq="1min")
    return df


STRATEGY_FIXTURES = {
    "volume_profile_reclaim": _vpr_fixture,
    "perpetual_premium_reversion": _premium_fixture,
    "cumulative_volume_delta_divergence": _cvd_fixture,
    "calendar_conditioned_entry": _calendar_conditioned_entry_fixture,
    "range_scalper": _range_scalper_fixture,
    "sweep_squeeze_combo": _sweep_squeeze_combo_fixture,
    "ultimate_oscillator": _ultimate_oscillator_fixture,
    "mass_index_reversal": _mass_index_reversal_fixture,
}


def _open_interest_from_bars(df: pd.DataFrame) -> dict:
    opens = df.index.as_unit("ms").asi8.astype(np.int64)
    interval = int(opens[1] - opens[0])
    minutes = interval // 60_000
    drift = 0.02 * np.sin(df["volume"].to_numpy() * 1000.0)
    level = 1e4 * np.concatenate([[1.0], np.cumprod(1.0 + drift)[:-1]])
    samples = []
    seq = 0
    for i, open_ms in enumerate(opens):
        for k in range(minutes):
            seq += 1
            samples.append({"recv_ms": int(open_ms + k * 60_000 + 30_000), "event_ms": None,
                            "value": float(level[i] * (1.0 + drift[i] * (k + 1) / minutes)),
                            "session": 1, "seq": seq})
    return {"available": True, "kind": "open_interest", "units": "base", "time_basis": "receipt",
            "source": "forward_peek_fixture", "cadence_ms": 60_000, "bar_interval_ms": interval,
            "bar_endpoint_offset_ms": interval, "samples": samples, "gaps": []}


def _with_open_interest(fn):
    def wrapped(df, **params):
        return fn(df, **params, open_interest_observations=_open_interest_from_bars(df))
    return wrapped


def _with_declared_cadence(fn):
    def wrapped(df, **params):
        out = df.copy()
        step_ms = int((out.index[1] - out.index[0]) / pd.Timedelta(milliseconds=1))
        out.attrs["bar_interval_ms"] = step_ms
        out.attrs["bar_timestamp_kind"] = "open"
        return fn(out, **params)
    return wrapped


def _trade_flow_from_bars(df: pd.DataFrame):
    from decimal import Decimal
    opens = df.index.as_unit("ms").asi8.astype(np.int64)
    interval = int(opens[1] - opens[0])
    parts = interval // 60_000
    intervals = []
    volumes = []
    move = (df["close"].to_numpy() - df["open"].to_numpy()) / (df["high"].to_numpy() - df["low"].to_numpy() + 1e-9)
    wobble = np.sin(df["volume"].to_numpy() * 37.0)
    for i, open_ms in enumerate(opens):
        unit = Decimal(int(round(float(df["volume"].iloc[i]) * 1000))) / Decimal(1000) / parts
        unit = unit.quantize(Decimal("0.00001"))
        frac = Decimal(str(round(0.5 + 0.3 * float(move[i]) + 0.15 * float(wobble[i]), 4)))
        bar_total = Decimal(0)
        for k in range(parts):
            start = int(open_ms + k * 60_000)
            buy = (unit * frac).quantize(Decimal("0.00001"))
            sell = unit - buy
            bar_total += unit
            invalid = start + 60_000 + 6_000 + 2 * interval if (start // 60_000) % 211 == 0 else None
            intervals.append({"start_ms": start, "end_ms": start + 60_000, "buy": _cvd_dec(buy), "sell": _cvd_dec(sell),
                              "total": _cvd_dec(unit), "trades": 7, "records_hash": "0" * 64, "session": 1,
                              "finalized_ms": start + 60_000 + 6_000,
                              "status": "invalid" if invalid is not None else "complete", "invalid_ms": invalid})
        volumes.append(float(bar_total))
    payload = {"available": True, "kind": "trade_flow", "coin": "FIX", "source": "hyperliquid_ws_trades",
               "units": "base", "time_basis": "event_time_receipt_cutoff", "identity": "venue+namespace+coin+time+tid",
               "side_evidence": "hyperliquid-trades-side-aggressor/1638-v1", "interval_ms": 60_000,
               "reorder_grace_ms": 5_000, "max_event_gap_ms": 30_000, "decimal_scale": 8,
               "bar_interval_ms": interval, "intervals": intervals, "gaps": []}
    return payload, volumes


def _cvd_dec(value) -> str:
    text = format(value.normalize(), "f")
    return text if "." not in text else text.rstrip("0").rstrip(".") or "0"


def _with_trade_flow(fn):
    def wrapped(df, **params):
        out = df.copy()
        payload, volumes = _trade_flow_from_bars(out)
        out["volume"] = volumes
        return fn(out, **params, trade_flow_observations=payload)
    return wrapped


def _premium_dec(value: float) -> str:
    text = f"{value:.6f}".rstrip("0").rstrip(".")
    return text or "0"


def _premium_from_bars(df: pd.DataFrame) -> dict:
    opens = df.index.as_unit("ms").asi8.astype(np.int64)
    interval = int(opens[1] - opens[0])
    closes = df["close"].to_numpy()
    samples = []
    for i, open_ms in enumerate(opens):
        t = int(open_ms + interval)
        p = 0.0002 * np.sin(i * 0.3)
        if i % 47 == 0:
            p = 0.0015 if (i // 47) % 2 == 0 else -0.0015
        elif i % 47 == 1:
            p = 0.001 if ((i - 1) // 47) % 2 == 0 else -0.001
        mid = float(closes[i])
        half = mid * 0.00005
        samples.append({
            "t": t, "status": "sampled", "finalized_ms": t + 1, "invalid_ms": None,
            "quote": {"bid": _premium_dec(mid - half), "bid_sz": "1", "bid_n": 1, "ask": _premium_dec(mid + half),
                      "ask_sz": "1", "ask_n": 1, "event_ms": t - 1_200, "recv_ms": t - 1_000, "session": 1, "seq": i + 1},
            "context": {"oracle": _premium_dec(mid / (1.0 + p)), "mark": _premium_dec(mid), "mid": None,
                        "oracle_event_ms": None, "recv_ms": t - 2_000, "session": 1, "seq": i + 1},
        })
    return {"available": True, "kind": "perp_premium", "coin": "FIX", "source": "hyperliquid_ws_activeAssetCtx+bbo",
            "contract": "perp-premium-quote-oracle/1639-v1", "time_basis": "receipt_cutoff_latest_component",
            "oracle_time_basis": "receipt_only_no_source_time", "sample_ms": 60_000, "max_future_skew_ms": 5_000,
            "bar_interval_ms": interval, "samples": samples, "gaps": []}


def _with_premium(fn):
    def wrapped(df, **params):
        return fn(df, **params, premium_observations=_premium_from_bars(df))
    return wrapped


def _macro_inputs_from_bars(df: pd.DataFrame):
    import macro_event_inputs as mi
    opens = df.index.as_unit("ms").asi8.astype(np.int64)
    step = int(opens[1] - opens[0])
    first, last = int(opens[0]), int(opens[-1]) + step
    source = {"source_id": "forward_peek_fixture", "publisher": "fixture", "url": "fixture:forward-peek",
              "retrieved_ms": first, "sha256": "0" * 64, "published_ms": None, "note": "synthetic fixture"}
    rows = []
    for k, open_ms in enumerate(opens[60::40]):
        stamp = pd.Timestamp(int(open_ms), unit="ms")
        rows.append({"event_id": f"fixture/{stamp:%Y-%m-%d}-{k}", "event_type": mi.EVENT_TYPE_FOMC, "revision": 1,
                     "status": "scheduled", "scheduled_local": stamp.strftime("%Y-%m-%dT%H:%M:%S"),
                     "source_timezone": "UTC", "known_ms": first, "publication_evidence": "fixture",
                     "source_id": "forward_peek_fixture"})
    cal, _ = mi.import_calendar(rows, [source], [{"event_type": mi.EVENT_TYPE_FOMC, "start_ms": first - 400 * step,
                                                  "end_ms": last + step}], "fixture", [mi.EVENT_TYPE_FOMC])
    qsource = {k: v for k, v in source.items() if k != "published_ms"}
    quotes = [{"event_ms": int(o) + step - 2_000, "recv_ms": int(o) + step - 1_000,
               "bid": f"{float(df['close'].iloc[i]):.4f}", "ask": f"{float(df['close'].iloc[i]) * 1.0001:.4f}",
               "source_id": "forward_peek_fixture"} for i, o in enumerate(opens)]
    qdoc, _ = mi.import_quotes(quotes, [qsource], "FIX", "fixture")
    return mi.calendar_payload(cal, "1" * 64), mi.quote_payload(qdoc, "2" * 64)


def _with_macro_events(fn):
    def wrapped(df, **params):
        events, quotes = _macro_inputs_from_bars(df)
        return fn(df, **params, event_records=events, spread_records=quotes)
    return wrapped


def _with_leader(fn):
    def wrapped(df, **params):
        import sys
        tools = os.path.abspath(os.path.join(_HERE, "..", "..", "shared_tools"))
        if tools not in sys.path:
            sys.path.insert(0, tools)
        from lead_lag_inputs import prepare_historical_inputs
        out = df.copy()
        step_ms = int((out.index[1] - out.index[0]) / pd.Timedelta(milliseconds=1))
        out.attrs["bar_interval_ms"] = step_ms
        out.attrs["bar_timestamp_kind"] = "open"
        leader = out[["open", "high", "low", "close", "volume"]].copy()
        leader["close"] = out["close"] * (1.0 + 0.002 * np.sin(out["volume"].to_numpy()))
        leader.attrs = dict(out.attrs)
        timeframe = {3_600_000: "1h", 900_000: "15m", 14_400_000: "4h"}[step_ms]
        ctx = prepare_historical_inputs(out, leader, "FIX/USDT", timeframe)
        return fn(out, **params, leader_context=ctx)
    return wrapped


def _volume_profile_from_bars(df: pd.DataFrame) -> dict:
    import sys
    tools = os.path.abspath(os.path.join(_HERE, "..", "..", "shared_tools"))
    if tools not in sys.path:
        sys.path.insert(0, tools)
    import volume_profile_inputs as vpi
    opens = df.index.as_unit("ms").asi8.astype(np.int64)
    step = int(opens[1] - opens[0])
    by_session = {}
    for i, open_ms in enumerate(opens):
        start = vpi.session_start(int(open_ms))
        px = f"{round(float(df['close'].iloc[i]) * 2) / 2:.1f}".rstrip("0").rstrip(".")
        units = int(round(float(df["volume"].iloc[i]) * 1000)) * 100_000
        book = by_session.setdefault(start, {"prices": {}, "first": int(open_ms), "last": int(open_ms) + step - 1, "trades": 0})
        book["prices"][px] = book["prices"].get(px, 0) + units
        book["last"] = int(open_ms) + step - 1
        book["trades"] += 3
    sessions = []
    for start in sorted(by_session):
        book = by_session[start]
        prices = sorted(book["prices"].items(), key=lambda kv: float(kv[0]))
        if len(prices) < 2:
            continue
        art = {"session_start_ms": start, "session_end_ms": start + vpi.SESSION_MS, "trades": max(book["trades"], len(prices)),
               "volume": vpi.format_units(sum(u for _, u in prices)), "trades_hash": "0" * 64,
               "first_event_ms": book["first"], "last_event_ms": book["last"], "complete_minutes": vpi.MINUTES,
               "available_ms": start + vpi.SESSION_MS + 7_000 + (start // vpi.SESSION_MS % 3) * step,
               "prices": [[px, vpi.format_units(u)] for px, u in prices]}
        art["sha256"] = vpi.artifact_sha256("FIX", art)
        invalid = art["available_ms"] + 9 * step if (start // vpi.SESSION_MS) % 4 == 3 else None
        sessions.append({"session_start_ms": start, "status": "invalidated" if invalid else "available",
                         "reason_code": "late_event" if invalid else "", "reason": "fixture" if invalid else "",
                         "invalid_ms": invalid, "artifact": art})
    return {"available": True, "kind": "volume_profile", "coin": "FIX", **vpi.CONTRACT, "window_ms": vpi.WINDOW_MS,
            "bar_interval_ms": step, "snapshot_id": "", "sessions": sessions}


def _with_volume_profile(fn):
    def wrapped(df, **params):
        out = df.copy()
        step_ms = int((out.index[1] - out.index[0]) / pd.Timedelta(milliseconds=1))
        out.attrs["bar_interval_ms"] = step_ms
        out.attrs["bar_timestamp_kind"] = "open"
        return fn(out, **params, volume_profile_observations=_volume_profile_from_bars(out))
    return wrapped


STRATEGY_INPUTS = {
    "volume_profile_reclaim": _with_volume_profile,
    "perpetual_premium_reversion": _with_premium,
    "macro_event_breakout": _with_macro_events,
    "bitcoin_lead_lag": _with_leader,
    "cumulative_volume_delta_divergence": _with_trade_flow,
    "open_interest_breakout": _with_open_interest,
    "ease_of_movement_breakout": _with_declared_cadence,
    "adaptive_moving_average": _with_declared_cadence,
    "dual_thrust": _with_declared_cadence,
    "schaff_trend_cycle": _with_declared_cadence,
    "klinger_divergence": _with_declared_cadence,
    "regression_channel_reversion": _with_declared_cadence,
    "trix_momentum": _with_declared_cadence,
    "camarilla_pivot_reversal": _with_declared_cadence,
    "mass_index_reversal": _with_declared_cadence,
    "calendar_conditioned_entry": _with_declared_cadence,
    "stochastic_momentum_index": _with_declared_cadence,
    "elder_force_pullback": _with_declared_cadence,
    "true_strength_index": _with_declared_cadence,
}

_FIXTURE_CACHE = {}
_PERTURBED_CACHE = {}


def _df_for(name: str):
    key = name if name in STRATEGY_FIXTURES else "_default"
    if key not in _FIXTURE_CACHE:
        _FIXTURE_CACHE[key] = STRATEGY_FIXTURES[name]() if key != "_default" else _DF
    return key, _FIXTURE_CACHE[key]


def _perturbed_at(key: str, df: pd.DataFrame, cut: int) -> pd.DataFrame:
    if (key, cut) not in _PERTURBED_CACHE:
        _PERTURBED_CACHE[(key, cut)] = _perturbed_df(df, cut)
    return _PERTURBED_CACHE[(key, cut)]


def _signal(fn, params, df: pd.DataFrame) -> np.ndarray:
    result = fn(df.copy(), **params)
    assert "signal" in result.columns, "strategy returned no 'signal' column"
    sig = pd.to_numeric(result["signal"], errors="coerce").to_numpy(dtype=np.float64)
    assert len(sig) == len(df), (
        f"signal length {len(sig)} != frame length {len(df)}"
    )
    return sig


MAX_TRANSITION_CUTS = 4


def _cuts_for(full: np.ndarray) -> list:
    n = len(full)
    cuts = {n * 5 // 6}
    min_cut = n // 3
    vals = np.nan_to_num(full)
    transitions = np.flatnonzero(np.diff(vals) != 0) + 1
    transitions = transitions[(transitions >= min_cut) & (transitions < n - 2)]
    for t in transitions[-MAX_TRANSITION_CUTS:]:
        cuts.add(int(t))
    return sorted(cuts)


def _prefix_violations(fn, params, df, cache_key):
    full = _signal(fn, params, df)
    violations = []

    def _mismatch(a, b):
        return np.flatnonzero(~((a == b) | (np.isnan(a) & np.isnan(b))))

    for cut in _cuts_for(full):
        trunc = _signal(fn, params, df.iloc[:cut])
        bad = _mismatch(full[:cut], trunc)
        if len(bad):
            violations.append(
                f"cut={cut}: truncation changed signals at bars {bad[:10].tolist()}"
                f"{'…' if len(bad) > 10 else ''} (dropping future bars must not "
                f"change an earlier signal)"
            )

        pert = _signal(fn, params, _perturbed_at(cache_key, df, cut))
        bad = _mismatch(full[:cut], pert[:cut])
        if len(bad):
            violations.append(
                f"cut={cut}: perturbing future bars changed signals at bars "
                f"{bad[:10].tolist()}{'…' if len(bad) > 10 else ''} (bars >= "
                f"{cut} must be invisible to earlier signals)"
            )

    return violations, full


def _sweep_cases():
    cases = []
    for name, entry in _REGISTRY.STRATEGIES.items():
        seen = {}
        for platform in entry["platforms"]:
            merged = {
                **entry["default_params"],
                **entry["variants"].get(platform, {}).get("default_params", {}),
            }
            key = repr(sorted(merged.items()))
            seen.setdefault(key, (merged, []))[1].append(platform)
        for merged, platforms in seen.values():
            cases.append(pytest.param(
                name, merged, id=f"{name}[{'+'.join(platforms)}]",
            ))
    return cases


@pytest.mark.parametrize("name,params", _sweep_cases())
def test_registry_strategy_is_truncation_invariant(name, params):
    if name in SKIP_STRATEGIES:
        pytest.skip(f"{name}: {SKIP_STRATEGIES[name]}")
    fn = _REGISTRY.STRATEGIES[name]["fn"]
    if name in STRATEGY_INPUTS:
        fn = STRATEGY_INPUTS[name](fn)
    key, df = _df_for(name)
    violations, full = _prefix_violations(fn, params, df, key)
    assert not violations, (
        f"{name} appears to read future bars (forward peek):\n  "
        + "\n  ".join(violations)
    )

    nonzero = np.nan_to_num(full) != 0
    if not nonzero.any():
        assert name in EXPECTED_ALL_ZERO_SIGNAL, (
            f"{name} produced all-zero signals on the sweep fixture — the "
            f"truncation-invariance check is vacuous for it. Either enrich "
            f"the fixture / add a per-strategy input hook, or add it to "
            f"EXPECTED_ALL_ZERO_SIGNAL with an inline justification."
        )


def test_harness_detects_forward_peeking_strategy():
    base_fn = _REGISTRY.STRATEGIES["sma_crossover"]["fn"]
    base_params = _REGISTRY.STRATEGIES["sma_crossover"]["default_params"]

    def peeking(df, **params):
        result = base_fn(df, **params)
        s = result["signal"].to_numpy().copy()
        if len(s) > 1:
            s[:-1] = s[1:]
        result["signal"] = s
        return result

    honest = _signal(base_fn, base_params, _DF)
    assert (np.nan_to_num(honest) != 0).any(), "sensitivity base strategy is vacuous"

    violations, _ = _prefix_violations(peeking, base_params, _DF, "_default")
    assert violations, (
        "forward-peeking strategy passed the truncation-invariance checker — "
        "the sweep is not sensitive to look-ahead"
    )

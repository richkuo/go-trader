
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


STRATEGY_FIXTURES = {
    "range_scalper": _range_scalper_fixture,
    "sweep_squeeze_combo": _sweep_squeeze_combo_fixture,
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


def test_zero_signal_allowlist_is_exact():
    unknown = set(EXPECTED_ALL_ZERO_SIGNAL) - set(_REGISTRY.STRATEGIES)
    assert not unknown, f"EXPECTED_ALL_ZERO_SIGNAL names unregistered strategies: {sorted(unknown)}"
    for name in sorted(EXPECTED_ALL_ZERO_SIGNAL):
        if name in SKIP_STRATEGIES:
            continue
        entry = _REGISTRY.STRATEGIES[name]
        emitted = False
        for platform in entry["platforms"]:
            merged = {
                **entry["default_params"],
                **entry["variants"].get(platform, {}).get("default_params", {}),
            }
            sig = _signal(entry["fn"], merged, _df_for(name)[1])
            emitted = emitted or bool((np.nan_to_num(sig) != 0).any())
        assert not emitted, (
            f"{name} now emits signals on the sweep fixture — remove it from "
            f"EXPECTED_ALL_ZERO_SIGNAL (stale vacuity entry)."
        )


def test_skip_list_is_closed_allowlist():
    unknown = set(SKIP_STRATEGIES) - set(_REGISTRY.STRATEGIES)
    assert not unknown, f"SKIP_STRATEGIES names unregistered strategies: {sorted(unknown)}"
    for name, reason in SKIP_STRATEGIES.items():
        assert isinstance(reason, str) and reason.strip(), (
            f"SKIP_STRATEGIES[{name!r}] must carry a non-empty justification"
        )


def test_fixture_hooks_are_closed_allowlist():
    unknown = set(STRATEGY_FIXTURES) - set(_REGISTRY.STRATEGIES)
    assert not unknown, f"STRATEGY_FIXTURES names unregistered strategies: {sorted(unknown)}"


def test_sweep_covers_every_registered_strategy():
    swept = {p.id.split("[")[0] for p in _sweep_cases()}
    assert swept == set(_REGISTRY.STRATEGIES)
    assert _REGISTRY.DISCOVERY_HIDDEN_STRATEGIES <= swept
    assert any(e.get("backtest_only") for e in _REGISTRY.STRATEGIES.values()), (
        "expected at least one backtest_only strategy in the sweep (#1138)"
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

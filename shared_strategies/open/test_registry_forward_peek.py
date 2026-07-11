"""Registry-wide forward-peek (truncation-invariance) sweep (#1279).

The backtester's ``shift(1)`` enforces *fill timing only* — signal purity is
caller-trusted (``backtest/tests/test_backtester_lookahead.py`` documents the
gap, #730). Per-strategy no-lookahead tests existed for only three strategies
(#1016/#1169/#1170); this module sweeps EVERY registered open strategy —
including hidden and ``backtest_only`` entries, since research strategies feed
promotion decisions too — so a forward-peeking strategy fails CI the day it is
registered, with no test edit.

Invariant per strategy (with registry-default params, per platform variant):
at every checked cut, signals on bars before the cut must be byte-identical
whether the frame is

* truncated at the cut (future bars absent),
* the full extended frame (future bars present), or
* the full frame with every bar past the cut perturbed (future bars
  *different*).

Cuts are placed at a fixed prefix boundary AND at the strategy's own signal
transitions — the transition cuts are what expose single-bar peeks. Strategies
whose entry conditions the generic fixture can't line up get a purpose-built
frame via ``STRATEGY_FIXTURES`` (input data only; params stay registry
defaults).

A causal strategy — one whose signal at bar i reads only bars ``<= i`` — passes
exactly; any read of a future bar (``shift(-1)``, ``center=True`` rolling,
whole-series normalization, unconfirmed pivots, …) shows up as a prefix
mismatch in at least one of the two comparisons.

Sensitivity (non-vacuity) is guarded two ways:

* a deliberately forward-peeking wrapper strategy must FAIL the same checker
  the sweep uses (``test_harness_detects_forward_peeking_strategy``);
* strategies that emit all-zero signals on the fixture are asserted against a
  closed, justified allowlist (``EXPECTED_ALL_ZERO_SIGNAL``) — silent
  per-strategy vacuity is a failure, in both directions.
"""

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


# Loaded at import time: pytest parameterization needs the strategy names at
# collection. The unique module name keeps this instance isolated from the
# spot/futures shims and the close-side registry (full-suite import-order rule).
_REGISTRY = _load(
    "_registry_forward_peek", os.path.join(_HERE, "registry.py")
)

# ─── Skip-list (closed allowlist) ────────────────────────────────────────────
# Strategies the shared fixture cannot exercise at all. Every entry MUST carry
# a reason; test_skip_list_is_closed_allowlist fails on stale/unknown names so
# an entry can't outlive its justification.
SKIP_STRATEGIES = {
    # name: reason
}

# ─── Expected all-zero-signal strategies (closed allowlist) ──────────────────
# Per-strategy vacuity guard: a strategy listed here produces NO signal on the
# fixture — the truncation-invariance assertions above are vacuous for it, and
# that fact is recorded loudly instead of silently. A strategy that goes
# all-zero WITHOUT being listed fails the sweep; a listed strategy that starts
# emitting signals fails test_zero_signal_allowlist_is_exact (stale entry).
EXPECTED_ALL_ZERO_SIGNAL = {
    # hold is signal=0 by contract (type=manual close-evaluator loop, #569).
    "hold",
}


# ─── Shared fixture ──────────────────────────────────────────────────────────
# Hourly DatetimeIndex is required (amd_ifvg reads index.hour, vwap_reversion
# buckets by index.date, session_breakout uses UTC session hours). Length must
# clear the largest warmups in the registry: ema_long=200 (momentum_pro,
# regime_adaptive*), analog_retrieval min_index=200 + horizon=12,
# funding_window=168 (funding_skew), 7d funding average = 168 hourly bars
# (delta_neutral_funding), senkou_b=52 (ichimoku).
N_BARS = 960          # 40 days of hourly bars; base cut at 800 (len*5//6)
_SEED = 20261279      # deterministic fixture (issue #1279)


def _make_close(rng: np.random.RandomState, n: int) -> np.ndarray:
    """Trend + swings + noise + regime shifts: rich enough that most
    strategies fire. Segments cycle through quiet consolidation (range/squeeze
    strategies), strong trend legs with volatility shocks (breakout/momentum
    strategies), and mean-reverting oscillation (reversion strategies)."""
    t = np.arange(n)
    trend = 100.0 + 0.02 * t
    swings = 6.0 * np.sin(t / 24.0) + 3.0 * np.sin(t / 7.0)
    # Per-segment volatility regime: quiet → normal → violent → normal → …
    seg = (t // 96) % 4  # 4-day segments on an hourly index
    vol = np.choose(seg, [0.08, 0.4, 1.2, 0.4])
    noise = (rng.randn(n) * vol).cumsum() * 0.6
    # Sparse shocks (gap-like bars) so ATR/band breakouts actually trigger.
    # Perturbation tails can be arbitrarily short (transition cuts near the
    # frame end), so shocks only apply when the segment is long enough.
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
    # Extra input columns some strategies consume when present; harmless to the
    # rest (strategies copy the frame). close_b: pairs_spread. funding_rate:
    # delta_neutral_funding / funding_skew take the per-bar series path — the
    # live scalar path writes a decision onto the LAST bar only, which is a
    # live-latest-bar surface, not the backtest series this sweep guards.
    df["close_b"] = close * 0.5 + rng.randn(n).cumsum() * 0.3 + 10.0
    df["funding_rate"] = 0.00005 + 0.00008 * np.sin(np.arange(n) / 30.0) + rng.randn(n) * 0.00002
    return df


def _fixture_df() -> pd.DataFrame:
    return _make_ohlcv(np.random.RandomState(_SEED), N_BARS, "2026-01-01")


def _perturbed_df(df: pd.DataFrame, cut: int) -> pd.DataFrame:
    """Same frame with every bar >= cut replaced by a differently-seeded tail.

    A strategy whose signal at bar i < cut reads ANY value from bars >= cut
    (price, volume, close_b, funding_rate) produces a different prefix here.
    """
    tail = _make_ohlcv(
        np.random.RandomState(_SEED + 1), len(df) - cut, str(df.index[cut])
    )
    # Re-base the perturbed tail near the real bar at the boundary so the
    # perturbation is a plausible continuation, not a price teleport that a
    # sanity-clamping strategy might coincidentally ignore.
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


# ─── Per-strategy fixture hooks (closed allowlist) ───────────────────────────
# Strategies whose entry conditions the generic fixture cannot plausibly line
# up get a purpose-built frame instead of a vacuity allowlist entry — the
# truncation-invariance check then actually exercises them. Builders must be
# deterministic (seeded) and only shape the INPUT data; params stay registry
# defaults. test_fixture_hooks_are_closed_allowlist rejects stale names.


def _range_scalper_fixture() -> pd.DataFrame:
    """Ultra-tight consolidation with engineered dip/pop excursions.

    range_scalper (deprecated/hidden, but still explicitly loadable) needs
    Bollinger bandwidth < 0.8%, below-average volume, AND a same-bar band
    cross with RSI(7) past its threshold — a generic random walk never
    satisfies all three at once. Slow multi-bar slides through the bands keep
    bandwidth tiny while dragging RSI to the extreme by the crossing bar.
    """
    rng = np.random.RandomState(_SEED + 2)
    n = 400
    close = 100.0 + rng.randn(n) * 0.02
    volume = np.full(n, 60.0)
    volume[::7] = 200.0  # periodic spikes keep vol_sma above the quiet bars
    for k in range(60, n, 60):
        for j in range(7):
            close[k - 6 + j] = close[k - 7] - 0.04 * (j + 1)  # slide down
            close[k + 1 + j] = close[k - 7] + 0.04 * (j + 1)  # pop back up
    idx = pd.date_range("2026-01-01", periods=n, freq="5min")
    return pd.DataFrame(
        {"open": close, "high": close + 0.02, "low": close - 0.02,
         "close": close, "volume": volume},
        index=idx,
    )


def _sweep_squeeze_combo_fixture() -> pd.DataFrame:
    """Sine cycle with stop-hunt wicks placed ON stoch-RSI trigger bars.

    sweep_squeeze_combo needs 2-of-3 same-bar, same-direction consensus
    (liquidity sweep + squeeze momentum + stoch RSI) — near-impossible to hit
    by chance. The stoch-RSI component reads only ``close``, so engineering a
    sweep wick (low below the prior swing-low pool with the close held back
    inside, and the mirror above for shorts) onto its trigger bars aligns the
    liquidity-sweep vote with the stoch-RSI vote without disturbing either.
    """
    rng = np.random.RandomState(_SEED + 3)
    n = 400
    close = 100.0 + 6.0 * np.sin(2 * np.pi * np.arange(n) / 125.0) + rng.randn(n) * 0.3
    df = pd.DataFrame(
        {"open": close, "high": close + 0.4, "low": close - 0.4,
         "close": close, "volume": np.full(n, 100.0)},
        index=pd.date_range("2026-01-01", periods=n, freq="15min"),
    )
    # Registered stoch_rsi shares the combo's forwarded sub-strategy defaults,
    # so its signals mark exactly where the combo's stoch-RSI vote lands.
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
    """Return ``(cache_key, frame)`` — the strategy's hook fixture or the
    shared default frame."""
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


# How many signal-transition cuts to check in addition to the fixed base cut.
MAX_TRANSITION_CUTS = 4


def _cuts_for(full: np.ndarray) -> list:
    """A fixed base cut plus cuts placed AT the strategy's own signal
    transitions.

    Cutting exactly where full[t] != full[t-1] is what makes the checker
    sensitive to single-bar peeks: a strategy whose bar t-1 adopted bar t's
    value cannot reproduce that value when bar t is truncated away or
    perturbed. A fixed cut alone can land where the signal is locally
    constant, letting a shift(-1) contamination through.

    The floor below only keeps comparisons meaningful (past warmup, signals
    exist) — a causal strategy satisfies the invariant at EVERY cut.
    """
    n = len(full)
    cuts = {n * 5 // 6}  # 960-bar default frame → cut at 800
    min_cut = n // 3
    vals = np.nan_to_num(full)
    transitions = np.flatnonzero(np.diff(vals) != 0) + 1
    transitions = transitions[(transitions >= min_cut) & (transitions < n - 2)]
    for t in transitions[-MAX_TRANSITION_CUTS:]:
        cuts.add(int(t))
    return sorted(cuts)


def _prefix_violations(fn, params, df, cache_key):
    """Core checker the sweep AND the sensitivity test share.

    Returns ``(violations, full_signal)``; empty == truncation-invariant.
    """
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
    """One case per (strategy, distinct merged param set across platforms).

    Variant default_params can change behavior (e.g. futures enabling shorts),
    so each distinct merged parameterization is swept; identical merges dedup
    to a single case labelled with the platforms sharing it.
    """
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

    # Per-strategy vacuity accounting: an all-zero signal makes the assertions
    # above prove nothing for this strategy, so it must be on the closed
    # allowlist (with a justification comment) — not silently green.
    nonzero = np.nan_to_num(full) != 0
    if not nonzero.any():
        assert name in EXPECTED_ALL_ZERO_SIGNAL, (
            f"{name} produced all-zero signals on the sweep fixture — the "
            f"truncation-invariance check is vacuous for it. Either enrich "
            f"the fixture / add a per-strategy input hook, or add it to "
            f"EXPECTED_ALL_ZERO_SIGNAL with an inline justification."
        )


def test_zero_signal_allowlist_is_exact():
    """Stale-entry guard: every EXPECTED_ALL_ZERO_SIGNAL entry must (a) name a
    registered strategy and (b) still be all-zero on the fixture for every
    swept param set. Always recomputed here (one strategy eval per allowlisted
    name — cheap) so the verdict never depends on which sweep cases happened
    to run in this session (test selection, ordering plugins, xdist workers).
    """
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
    """Adding a register(...) call must add it to the sweep with no test edit —
    and the sweep must include hidden + backtest_only entries."""
    swept = {p.id.split("[")[0] for p in _sweep_cases()}
    assert swept == set(_REGISTRY.STRATEGIES)
    assert _REGISTRY.DISCOVERY_HIDDEN_STRATEGIES <= swept
    assert any(e.get("backtest_only") for e in _REGISTRY.STRATEGIES.values()), (
        "expected at least one backtest_only strategy in the sweep (#1138)"
    )


def test_harness_detects_forward_peeking_strategy():
    """Sensitivity guard: a deliberately forward-peeking strategy must FAIL the
    exact checker the sweep uses — otherwise a fixture/cut choice could make
    the whole sweep vacuous. Mirrors the contamination pattern documented in
    backtest/tests/test_backtester_lookahead.py (#730): bar n adopts bar n+1's
    signal.
    """
    base_fn = _REGISTRY.STRATEGIES["sma_crossover"]["fn"]
    base_params = _REGISTRY.STRATEGIES["sma_crossover"]["default_params"]

    def peeking(df, **params):
        result = base_fn(df, **params)
        s = result["signal"].to_numpy().copy()
        if len(s) > 1:
            s[:-1] = s[1:]
        result["signal"] = s
        return result

    # The underlying signal must be non-vacuous for the peek to be observable.
    honest = _signal(base_fn, base_params, _DF)
    assert (np.nan_to_num(honest) != 0).any(), "sensitivity base strategy is vacuous"

    violations, _ = _prefix_violations(peeking, base_params, _DF, "_default")
    assert violations, (
        "forward-peeking strategy passed the truncation-invariance checker — "
        "the sweep is not sensitive to look-ahead"
    )

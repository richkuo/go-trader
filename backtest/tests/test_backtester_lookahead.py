"""
Look-ahead bias regression tests for Backtester (issue #730).

These tests pin the engine's look-ahead contract so future refactors can't
silently regress it:

  1. Entry fill timing — signal computed on bar N fills at bar N+1's open.
     The ``shift(1)`` in the signal-normalization block of
     ``Backtester.run`` is the only thing preventing same-bar fill from a
     signal generated at end-of-bar.
  2. Regime gate timing — regime label gating entry at row N+1 must be the
     regime from bar N's closed data, not bar N+1's (#730 Gap 1). Backtester
     shifts the regime column post-injection in the regime-shift block of
     ``Backtester.run``.
  3. Forward-peek contract — strategies that peek forward (e.g.
     ``signal = (close.shift(-1) > close)``) WILL inflate returns. The shift
     is not a defense against caller-side forward-peeking; it only enforces
     next-bar fill timing. This is a documented limitation — strategy
     scripts are responsible for not peeking.
"""
import sys
import pathlib

import numpy as np
import pandas as pd
import pytest

sys.path.insert(0, str(pathlib.Path(__file__).parent.parent.parent / "shared_tools"))
sys.path.insert(0, str(pathlib.Path(__file__).parent.parent))

from backtester import Backtester

_NEVER_FIRES_CLOSE = [{"name": "tiered_tp_pct", "params": {"tp_tiers": [
    {"profit_pct": 0.9, "close_fraction": 1.0},
]}}]

_REGIME_DIRECTIONAL_POLICY = {
    "trend_regime": {
        "trending_up": {"direction": "long", "invert_signal": False},
        "trending_down": {"direction": "short", "invert_signal": True},
        "ranging": {"direction": "long", "invert_signal": False},
    },
}


def _step_up_df(n: int = 20, jump_bar: int = 10, jump_pct: float = 0.10) -> pd.DataFrame:
    """Flat price with a single up-jump at ``jump_bar``.

    Designed so that the entry fill price differs sharply depending on
    whether the engine fills at bar K's open vs bar K+1's open:
      • close[K-1] = open[K] = 100, close[K] = open[K+1] = 110 (jump),
        close[K+1] = 110 (held).
      • Entry at open[K] = 100 → captures the jump → +10% return.
      • Entry at open[K+1] = 110 → misses the jump → ~0% return.
    """
    close = np.full(n, 100.0)
    close[jump_bar:] = 100.0 * (1.0 + jump_pct)
    open_ = close.copy()
    # open[K] = pre-jump price (matches close[K-1]) — the jump happens
    # within bar K, between open and close.
    open_[jump_bar] = 100.0
    idx = pd.date_range("2024-01-01", periods=n, freq="D")
    return pd.DataFrame(
        {"open": open_, "high": np.maximum(open_, close) + 0.01,
         "low": np.minimum(open_, close) - 0.01, "close": close,
         "volume": 1000.0},
        index=idx,
    )


# ─── 1. Entry fills at next bar's open, not same bar ─────────────────────────


def test_signal_at_bar_k_fills_at_bar_k_plus_1_open():
    """Signal=1 at bar K must fill at row K+1's open price (the shift(1)
    contract). If anyone removes the shift in the signal-normalization
    block of ``Backtester.run``, this test detects it: the entry would land
    at bar K's open (the jump) and the backtest would book a +10% gain
    instead of ~0%.
    """
    df = _step_up_df(n=20, jump_bar=10, jump_pct=0.10)
    df["signal"] = 0
    # Signal placed on bar 9 (pre-jump). Pre-shift contract:
    # signal generator saw close[9] = 100, didn't see close[10] = 110.
    df.iloc[9, df.columns.get_loc("signal")] = 1

    bt = Backtester(initial_capital=1000.0, commission_pct=0.0, slippage_pct=0.0)
    result = bt.run(df, save=False)

    assert result["total_trades"] >= 1
    entry_price = result["trades"][0]["entry_price"]
    # Bar 10's open = 100 (pre-jump). If shift is broken, entry would be
    # at bar 9's open = 100 (also 100 — bad luck with this fixture).
    # The discriminator is the entry timestamp.
    entry_date = pd.Timestamp(result["trades"][0]["entry_date"])
    # bar 9 + 1 = bar 10 = 2024-01-11
    assert entry_date == df.index[10], (
        f"Entry filled at {entry_date}, expected {df.index[10]} (bar 10 = signal_bar + 1). "
        f"Shift-by-1 protection in the signal-normalization block of "
        f"Backtester.run may be broken."
    )
    assert entry_price == 100.0, f"Expected entry at bar 10's open=100, got {entry_price}"


def test_intra_bar_jump_captured_at_next_bar_open_documents_limit():
    """End-to-end variant documenting the engine's *fill-timing only*
    contract: a signal that 'magically' fires the bar BEFORE a jump DOES
    capture the jump in this fixture, because the entry fills at the jump
    bar's open (which is still pre-jump in our setup) and then rides the
    intra-bar move up to the post-jump close.

    This pins the entry fill price to bar K+1's OPEN (not bar K's close,
    and not bar K+1's close): the shift only enforces next-bar fill
    timing — it is NOT a defense against caller-side forward-peeking.
    True forward-peek protection is the caller's responsibility (closed-bar
    indicator consumption).
    """
    df = _step_up_df(n=20, jump_bar=10, jump_pct=0.20)
    df["signal"] = 0
    df.iloc[9, df.columns.get_loc("signal")] = 1   # buy
    df.iloc[15, df.columns.get_loc("signal")] = -1  # sell

    bt = Backtester(initial_capital=1000.0, commission_pct=0.0, slippage_pct=0.0)
    result = bt.run(df, save=False)

    # Entry at bar 10's open = 100 (the open is pre-jump; close is post-jump).
    # Exit at bar 16's open = 120 (post-jump, flat thereafter).
    # P&L ≈ +20% — captures the jump because entry happened to land at
    # bar 10's open which is still pre-jump in our fixture.
    #
    # This documents the contract: the shift puts the fill at bar K+1's
    # OPEN, not close. A strategy peeking forward by 1 bar and emitting
    # signal at bar K-1 will still capture moves that happen WITHIN bar K
    # (between bar K's open and close). True forward-peek protection
    # requires the caller to not peek; the engine only enforces timing.
    final_pct = (result["final_capital"] - 1000.0) / 1000.0 * 100.0
    assert final_pct > 15.0, (
        f"Expected ≥+15% (captures intra-bar jump at bar 10's open), got {final_pct:.2f}%"
    )


# ─── 2. Regime gate uses bar N's regime when entry fills at bar N+1 ──────────


def test_regime_gate_uses_prior_bar_regime_not_current():
    """The regime gating an entry at row N+1 must be bar N's regime
    (knowable at decision time), not bar N+1's regime (only knowable after
    bar N+1 closes). Pre-#730 the backtester used the row-N+1 regime —
    look-ahead.

    Test: signal at bar 9 → entry at bar 10's open. Set bar 9's regime to
    'trending_up' (allowed) and bar 10's regime to 'ranging' (not allowed).
    Post-fix, the entry sees bar 9's regime → entry passes.
    Pre-fix, the entry saw bar 10's regime → entry blocked.
    """
    df = _step_up_df(n=20, jump_bar=15)
    df["signal"] = 0
    df.iloc[9, df.columns.get_loc("signal")] = 1
    # Hand-construct regime so the test depends on which bar's regime is read.
    df["regime"] = "ranging"
    df.iloc[9, df.columns.get_loc("regime")] = "trending_up"   # bar 9 (decision)
    # bar 10's regime stays "ranging" — pre-fix this blocks the entry.

    bt = Backtester(
        initial_capital=1000.0, commission_pct=0.0, slippage_pct=0.0,
        regime_enabled=True, allowed_regimes=["trending_up"],
    )
    result = bt.run(df, save=False)

    assert result["total_trades"] == 1, (
        "Entry should pass: bar 9's regime is 'trending_up' (allowed). "
        "If 0 trades, the backtester is reading bar 10's regime (look-ahead) "
        "instead of bar 9's. See the regime-shift block in Backtester.run."
    )


def test_regime_gate_blocks_when_prior_bar_regime_disallows():
    """Opposite of the above: bar N's regime disallows, bar N+1's regime
    would have allowed. Entry must be blocked (gate honors bar N).
    """
    df = _step_up_df(n=20, jump_bar=15)
    df["signal"] = 0
    df.iloc[9, df.columns.get_loc("signal")] = 1
    df["regime"] = "trending_up"
    df.iloc[9, df.columns.get_loc("regime")] = "ranging"   # bar 9 disallows
    # bar 10's regime is "trending_up" — pre-fix this would let entry pass.

    bt = Backtester(
        initial_capital=1000.0, commission_pct=0.0, slippage_pct=0.0,
        regime_enabled=True, allowed_regimes=["trending_up"],
    )
    result = bt.run(df, save=False)

    assert result["total_trades"] == 0, (
        "Entry should be blocked by bar 9's 'ranging' regime. "
        "If a trade fired, the backtester is reading bar 10's regime "
        "(look-ahead). See the regime-shift block in Backtester.run."
    )


def test_regime_directional_policy_uses_prior_bar_regime_not_current():
    """A policy-resolved entry at row N+1 must use bar N's regime label."""
    df = _step_up_df(n=20, jump_bar=15)
    df["signal"] = 0
    df.iloc[9, df.columns.get_loc("signal")] = 1
    df["regime"] = "trending_up"
    df.iloc[9, df.columns.get_loc("regime")] = "trending_down"
    # bar 10 stays trending_up. A look-ahead resolver would open long; the
    # correct closed-bar resolver opens the inverse short from bar 9's label.

    bt = Backtester(
        initial_capital=1000.0,
        commission_pct=0.0,
        slippage_pct=0.0,
        close_strategies=_NEVER_FIRES_CLOSE,
        regime_enabled=True,
        regime_directional_policy=_REGIME_DIRECTIONAL_POLICY,
        regime_directional_certified=True,  # #1085: test the resolver under certification
    )
    result = bt.run(df, save=False)

    assert [t["side"] for t in result["trades"]] == ["short"], (
        "Policy resolver must read bar 9's trending_down label for the bar 10 "
        "fill. Reading bar 10's trending_up label would open long instead."
    )


# ─── 3. Forward-peek in caller signal is NOT defended (documented limit) ─────


def test_forward_peek_signal_documents_caller_responsibility():
    """A signal generator that peeks forward by 1 bar
    (``signal = (close.shift(-1) > close)``) bypasses the engine's
    shift(1). The shift moves the signal from row N to row N+1; the
    forward-peek read close[N+1] when computing signal[N]; net result:
    the engine fills at row N+1's open knowing the bar will close higher
    than the prior bar.

    This is a DOCUMENTED LIMITATION (#730): the engine enforces fill
    timing, not signal purity. Callers are responsible for closed-bar
    indicator consumption.

    Test asserts that a forward-peek signal produces returns meaningfully
    above a no-trade baseline — confirming the engine does NOT magically
    catch forward-peeking strategies. If a future change makes the engine
    detect/reject forward-peek signals, this test should be updated to
    reflect the new contract.
    """
    n = 200
    rng = np.random.default_rng(42)
    returns = rng.normal(0.001, 0.02, n)  # noisy upward drift
    close = 100.0 * np.cumprod(1.0 + returns)
    idx = pd.date_range("2024-01-01", periods=n, freq="D")
    df = pd.DataFrame(
        {"open": close, "high": close * 1.005, "low": close * 0.995,
         "close": close, "volume": 1000.0},
        index=idx,
    )
    # Forward-peek: 1 if next bar closes higher, else -1.
    df["signal"] = np.where(
        df["close"].shift(-1) > df["close"], 1, -1
    ).astype(int)
    # First/last NaN-derived values get fillna(0) inside the backtester.
    df.loc[df.index[-1], "signal"] = 0

    bt = Backtester(initial_capital=1000.0, commission_pct=0.0, slippage_pct=0.0)
    result = bt.run(df, save=False)

    # Buy-and-hold baseline for comparison.
    buy_and_hold_pct = (close[-1] - close[0]) / close[0] * 100.0
    final_pct = (result["final_capital"] - 1000.0) / 1000.0 * 100.0

    # Forward-peek with shift still beats buy-and-hold by a wide margin
    # because the signal correlates with next-bar direction and the entry
    # at row N+1's open captures bar N+1's intraday move (open → close).
    # We assert strictly above buy-and-hold to lock in the contract that
    # the engine does NOT defend against forward-peek.
    assert final_pct > buy_and_hold_pct + 20.0, (
        f"Forward-peek signal should inflate returns past buy-and-hold "
        f"(documented limit). Got {final_pct:.1f}% vs BAH {buy_and_hold_pct:.1f}%. "
        f"If this assertion fails, the engine has gained forward-peek detection — "
        f"update the test and the look-ahead contract docstring at the top of "
        f"backtest/backtester.py."
    )


def test_shift_moves_signal_by_exactly_one_row():
    """Mechanical test of the shift(1): a single signal=1 at bar K produces
    an entry at bar K+1, not bar K and not bar K+2.

    This is the cheapest possible canary for the shift's presence.
    """
    df = _step_up_df(n=20, jump_bar=15)
    df["signal"] = 0
    df.iloc[5, df.columns.get_loc("signal")] = 1
    df.iloc[10, df.columns.get_loc("signal")] = -1

    bt = Backtester(initial_capital=1000.0, commission_pct=0.0, slippage_pct=0.0)
    result = bt.run(df, save=False)

    assert result["total_trades"] == 1
    entry_date = pd.Timestamp(result["trades"][0]["entry_date"])
    exit_date = pd.Timestamp(result["trades"][0]["exit_date"])
    assert entry_date == df.index[6], f"Entry should be at bar 6, got {entry_date}"
    assert exit_date == df.index[11], f"Exit should be at bar 11, got {exit_date}"


def test_zscore_target_close_uses_closed_bar_z_and_fills_next_open():
    """#997: the zscore_target exit must read bar N's closed-bar z-score and
    fill at bar N+1's open — never act on a spike intrabar at bar N.

    Build a flat series that spikes once at bar K. The rolling z first reaches
    the target at bar K (computed from closed data through K); the close must
    therefore fill at K+1's open, not K's close.
    """
    import pandas as pd
    n = 12
    idx = pd.date_range("2024-01-01", periods=n, freq="h")
    closes = [100.0] * 6 + [100.0, 100.0, 130.0, 130.0, 130.0, 130.0]
    spike_bar = 8  # close jumps here
    df = pd.DataFrame({
        "open": closes, "high": closes, "low": closes, "close": closes,
        "open_action": ["long"] + ["none"] * (n - 1),
    }, index=idx)
    bt = Backtester(initial_capital=1000.0, commission_pct=0.0, slippage_pct=0.0,
                    open_strategy={"name": "x"},
                    close_strategies=[{"name": "zscore_target",
                                       "params": {"lookback": 4, "z_target": 1.0}}],
                    direction="long")
    result = bt.run(df, strategy_name="x", save=False)
    assert result["total_trades"] == 1
    exit_date = pd.Timestamp(result["trades"][0]["exit_date"])
    # z first crosses the target at the spike bar (closed-bar data); the exit
    # fills at the NEXT bar's open, strictly after the spike bar.
    assert exit_date > df.index[spike_bar], (
        f"exit {exit_date} must be after the spike bar {df.index[spike_bar]} "
        "(next-open fill, not intrabar)"
    )
    assert result["trades"][0]["exit_reason"].startswith("zscore_target:")


# ─── anchored_vwap: signals at bars <= cut don't depend on future bars (#1016) ─
sys.path.insert(0, str(pathlib.Path(__file__).parent.parent.parent / "shared_strategies" / "open"))
from anchored_vwap import anchored_vwap_core  # noqa: E402


_AVWAP_PARAMS = dict(pivot_strength=2, confirm_bars=2, atr_period=3)


def _avwap_mixed_fixture() -> pd.DataFrame:
    """OHLCV forming a strict swing LOW (idx 5) then a strict swing HIGH (idx 17).

    The reclaim above the low-anchored AVWAP fires +1 (bar 11) and the breakdown
    below the high-anchored AVWAP fires -1 (bar 20) — a non-trivial mix across a
    re-anchor (anchors progress -1 -> 5 -> 9 -> 17). A smooth linspace fixture is
    NOT usable here: equal lows/highs at a monotonic turn tie under the strict
    pivot rule, so no pivot confirms, anchor_index stays -1, signal is all zeros,
    and the truncation assertion would be vacuous (#1019 review).
    """
    seg = [110, 108, 106, 104, 102, 100, 100.5, 100.2, 99.8, 99.5,
           103.5, 104, 104.5, 105, 105.5, 106, 108, 110, 109.5, 109,
           108.5, 104.5, 104, 103.5, 103]
    closes = np.array(seg, dtype=float)
    idx = pd.date_range("2026-01-01", periods=len(closes), freq="1h")
    return pd.DataFrame(
        {"open": closes, "high": closes + 0.5, "low": closes - 0.5,
         "close": closes, "volume": np.full(len(closes), 10.0)},
        index=idx,
    )


def test_anchored_vwap_no_lookahead():
    """Truncating future bars must not change any signal at bars <= cut.

    anchored_vwap_core anchors to *confirmed* pivots (pivot_strength bars on each
    side) and derives AVWAP/ATR from prefix sums — all using only data at or
    before the current bar, so appending future bars cannot change an earlier
    signal.

    Guards against the #1019 review finding (a vacuous all-zero fixture): the
    fixture must emit nonzero signals, and a deliberately forward-peeking variant
    must make the invariance assertion FAIL (sensitivity check), proving the test
    can actually detect look-ahead.
    """
    df = _avwap_mixed_fixture()
    cut = 20  # straddles the confirm_bars window [19, 20]; bar 20 fires -1

    def real(d):
        return anchored_vwap_core(d, **_AVWAP_PARAMS)["signal"].to_numpy()

    def forward_peeking(d):
        # bar n adopts bar n+1's signal — a canonical look-ahead contamination.
        s = anchored_vwap_core(d, **_AVWAP_PARAMS)["signal"].to_numpy().copy()
        if len(s) > 1:
            s[:-1] = s[1:]
        return s

    full = real(df)
    # Non-vacuity: the fixture must actually produce signals (both directions).
    assert (full != 0).any(), "fixture is vacuous — no signal to guard"
    assert (full == 1).any() and (full == -1).any(), "expected a +1 and a -1"

    # Invariant: signals at bars < cut are unchanged when future bars are dropped.
    trunc = real(df.iloc[:cut])
    assert np.array_equal(full[:cut], trunc), "signals < cut must not depend on future bars"

    # Sensitivity: a forward-peeking core variant must NOT be truncation-invariant,
    # else the assertion above proves nothing.
    bf = forward_peeking(df)
    bt = forward_peeking(df.iloc[:cut])
    assert not np.array_equal(bf[:cut], bt), (
        "forward-peeking variant should break truncation-invariance — the test "
        "is not sensitive to look-ahead")


# ─── anchored_vwap_channel: signals at bars <= cut don't depend on future bars (#1169) ─
from anchored_vwap_channel import anchored_vwap_channel_core  # noqa: E402


_AVWAP_CHANNEL_PARAMS = dict(pivot_strength=2, buffer_atr_mult=0.0, confirm_bars=2,
                             min_width_atr_mult=0.0, atr_period=3)


def _avwap_channel_mixed_fixture() -> pd.DataFrame:
    """OHLCV forming a swing HIGH (idx 2), a swing LOW (idx 6), then a channel.

    Bar 10 wicks through the support line and reclaims (+1 fires at bar 11 —
    and bar 10's hand-set wick low is itself a strict swing low, re-anchoring
    support at bar 12). A later swing HIGH (idx 13) re-anchors resistance; the
    bar-15 dip to the risen support fires +1 at 16, and bar 16's hand-set
    up-wick through resistance with a held rejection fires -1 at bar 17. A
    smooth fixture is NOT usable: equal highs/lows at a monotonic turn tie
    under the strict pivot rule and nothing confirms (see the anchored_vwap
    fixture note above).
    """
    closes = np.array([104, 106, 108, 106, 104, 102, 100, 102, 104, 103,
                       102.5, 103.5, 104.5, 106.0, 104.8, 104.2, 104.6, 104.0],
                      dtype=float)
    lows = closes - 0.5
    lows[10] = 101.0
    highs = closes + 0.5
    highs[16] = 105.8
    idx = pd.date_range("2026-01-01", periods=len(closes), freq="1h")
    return pd.DataFrame(
        {"open": closes, "high": highs, "low": lows, "close": closes,
         "volume": np.full(len(closes), 10.0)},
        index=idx,
    )


def test_anchored_vwap_channel_no_lookahead():
    """Truncating future bars must not change any signal at bars <= cut.

    anchored_vwap_channel_core confirms typed pivots (pivot_strength bars on
    each side), derives both AVWAP lines from prefix sums, and evaluates every
    trigger/validity clause on bars at or before the current bar, so appending
    future bars cannot change an earlier signal.

    Mirrors test_anchored_vwap_no_lookahead's non-vacuity and sensitivity
    checks (#1019 review): the fixture must emit both signal directions, and a
    deliberately forward-peeking variant must make the invariance assertion
    FAIL, proving the test can actually detect look-ahead.
    """
    df = _avwap_channel_mixed_fixture()
    cut = 17  # straddles the confirm_bars window [16, 17]; bar 17 fires -1

    def real(d):
        return anchored_vwap_channel_core(d, **_AVWAP_CHANNEL_PARAMS)["signal"].to_numpy()

    def forward_peeking(d):
        # bar n adopts bar n+1's signal — a canonical look-ahead contamination.
        s = anchored_vwap_channel_core(d, **_AVWAP_CHANNEL_PARAMS)["signal"].to_numpy().copy()
        if len(s) > 1:
            s[:-1] = s[1:]
        return s

    full = real(df)
    # Non-vacuity: the fixture must actually produce signals (both directions).
    assert (full != 0).any(), "fixture is vacuous — no signal to guard"
    assert (full == 1).any() and (full == -1).any(), "expected a +1 and a -1"

    # Invariant: signals at bars < cut are unchanged when future bars are
    # dropped — at the single cut straddling the firing window, and at every
    # earlier cut (re-anchors at 12 and 15 sit inside the sweep).
    trunc = real(df.iloc[:cut])
    assert np.array_equal(full[:cut], trunc), "signals < cut must not depend on future bars"
    for c in range(3, len(df)):
        assert np.array_equal(full[:c], real(df.iloc[:c])), f"prefix changed at cut {c}"

    # Sensitivity: a forward-peeking core variant must NOT be truncation-invariant,
    # else the assertion above proves nothing.
    bf = forward_peeking(df)
    bt = forward_peeking(df.iloc[:cut])
    assert not np.array_equal(bf[:cut], bt), (
        "forward-peeking variant should break truncation-invariance — the test "
        "is not sensitive to look-ahead")


# ─── anchored_vwap_reversion: signals at bars <= cut don't depend on future bars (#1170) ─
from anchored_vwap_reversion import anchored_vwap_reversion_core  # noqa: E402


_AVWAP_REVERSION_PARAMS = dict(pivot_strength=2, entry_atr_mult=1.0,
                               buffer_atr_mult=0.0, confirm_bars=2, atr_period=3)


def _avwap_reversion_mixed_fixture() -> pd.DataFrame:
    """OHLCV forming a swing LOW (idx 5), a downside stretch, then a swing
    HIGH (idx 13) and an upside stretch.

    Bar 9 wicks through the lower ATR band and closes back inside — below the
    line — so +1 fires at bar 10; the rally re-anchors (bar 9's wick low is
    itself a strict swing low, confirming at 11; the bar-13 swing high
    confirms at 15) and bar 16's hand-set up-wick through the upper band with
    a held in-zone close fires -1 at bar 17. A smooth fixture is NOT usable:
    equal lows/highs at a monotonic turn tie under the strict pivot rule and
    nothing confirms (see the anchored_vwap fixture note above).
    """
    closes = np.array([110, 108, 106, 104, 102, 100, 101, 102, 101, 100.2,
                       100.6, 103, 105, 107, 106, 105, 106.5, 106.3],
                      dtype=float)
    lows = closes - 0.5
    lows[9] = 99.0
    highs = closes + 0.5
    highs[16] = 108.5
    idx = pd.date_range("2026-01-01", periods=len(closes), freq="1h")
    return pd.DataFrame(
        {"open": closes, "high": highs, "low": lows, "close": closes,
         "volume": np.full(len(closes), 10.0)},
        index=idx,
    )


def test_anchored_vwap_reversion_no_lookahead():
    """Truncating future bars must not change any signal at bars <= cut.

    anchored_vwap_reversion_core anchors to *confirmed* pivots (pivot_strength
    bars on each side), derives AVWAP/ATR from prefix sums, and evaluates every
    trigger clause (stretch touch, snap-back, zone hold, freshness) on bars at
    or before the current bar, so appending future bars cannot change an
    earlier signal.

    Mirrors test_anchored_vwap_no_lookahead's non-vacuity and sensitivity
    checks (#1019 review) and the #1169 full prefix sweep: the fixture must
    emit both signal directions, and a deliberately forward-peeking variant
    must make the invariance assertion FAIL, proving the test can actually
    detect look-ahead.
    """
    df = _avwap_reversion_mixed_fixture()
    cut = 17  # straddles the confirm_bars window [16, 17]; bar 17 fires -1

    def real(d):
        return anchored_vwap_reversion_core(d, **_AVWAP_REVERSION_PARAMS)["signal"].to_numpy()

    def forward_peeking(d):
        # bar n adopts bar n+1's signal — a canonical look-ahead contamination.
        s = anchored_vwap_reversion_core(d, **_AVWAP_REVERSION_PARAMS)["signal"].to_numpy().copy()
        if len(s) > 1:
            s[:-1] = s[1:]
        return s

    full = real(df)
    # Non-vacuity: the fixture must actually produce signals (both directions).
    assert (full != 0).any(), "fixture is vacuous — no signal to guard"
    assert (full == 1).any() and (full == -1).any(), "expected a +1 and a -1"

    # Invariant: signals at bars < cut are unchanged when future bars are
    # dropped — at the single cut straddling the firing window, and at every
    # earlier cut (the re-anchors at 11 and 15 sit inside the sweep).
    trunc = real(df.iloc[:cut])
    assert np.array_equal(full[:cut], trunc), "signals < cut must not depend on future bars"
    for c in range(3, len(df)):
        assert np.array_equal(full[:c], real(df.iloc[:c])), f"prefix changed at cut {c}"

    # Sensitivity: a forward-peeking core variant must NOT be truncation-invariant,
    # else the assertion above proves nothing.
    bf = forward_peeking(df)
    bt = forward_peeking(df.iloc[:cut])
    assert not np.array_equal(bf[:cut], bt), (
        "forward-peeking variant should break truncation-invariance — the test "
        "is not sensitive to look-ahead")


# ---------------------------------------------------------------------------
# #1228: EntryATR stamp must read the last CLOSED bar before the fill bar.
# The fill happens at bar N+1's open, when that bar's own high/low/close (and
# hence its ATR) are still unknown — stamping the fill bar's ATR leaked its
# own range into the stop/TP geometry.
# ---------------------------------------------------------------------------

def test_entry_atr_stamped_from_bar_before_fill():
    # Bar 0 emits long -> fill at bar 1 open. Closed-bar ATR at order time is
    # bar 0's (5); the fill bar's ATR (20) must NOT be stamped. With a 1xATR
    # tier, bar 2 close 110 (=100+2x5) fires only under the closed-bar stamp
    # (a 20-ATR stamp would need 120).
    idx = pd.date_range("2024-01-01", periods=5, freq="D")
    df = pd.DataFrame({
        "open": [100, 100, 100, 110, 110],
        "close": [100, 100, 110, 110, 110],
        "atr": [5, 20, 20, 20, 20],
        "open_action": ["long", "none", "none", "none", "none"],
    }, index=idx)
    bt = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        close_strategies=[{"name": "tiered_tp_atr", "params": {"tp_tiers": [
            {"atr_multiple": 1.0, "close_fraction": 1.0},
        ]}}],
    )
    result = bt.run(df, save=False)
    assert result["total_trades"] == 1
    assert result["trades"][0]["exit_price"] == 110.0
    assert result["trades"][0]["exit_date"] == str(idx[3])


def test_entry_atr_stamp_no_prior_bar_returns_zero():
    # A fill on the very first bar has no closed prior bar -> no usable ATR
    # (evaluators that need it no-op), never the fill bar's own value.
    bt = Backtester(initial_capital=1000, commission_pct=0, slippage_pct=0)
    idx = pd.date_range("2024-01-01", periods=3, freq="D")
    atr = pd.Series([5.0, 6.0, 7.0], index=idx)
    assert bt._stamp_entry_atr(atr, idx[0], 100.0) == 0.0
    assert bt._stamp_entry_atr(atr, idx[2], 100.0) == 6.0


def test_scale_in_add_fills_at_next_bar_open():
    # #1276: the add signal computed on bar N fills at bar N+1's open — same
    # contract as entries. Bar 2 emits the same-direction signal; the add
    # must fill at bar 3's open (108), not bar 2's (100).
    idx = pd.date_range("2024-01-01", periods=6, freq="D")
    df = pd.DataFrame({
        "open": [100.0, 100.0, 100.0, 108.0, 108.0, 108.0],
        "high": [100.5, 100.5, 108.0, 108.5, 108.5, 108.5],
        "low": [99.5, 99.5, 99.5, 107.5, 107.5, 107.5],
        "close": [100.0, 100.0, 108.0, 108.0, 108.0, 108.0],
        "signal": [1, 0, 1, 0, -1, 0],
    }, index=idx)
    bt = Backtester(initial_capital=10000, commission_pct=0, slippage_pct=0,
                    allow_scale_in=True)
    result = bt.run(df, save=False)
    assert result["scale_in_adds"] == 1
    (trade,) = result["trades"]
    # Open leg: 100 sh @ 100. Add: notional 10000 at DECISION price (bar 2's
    # close, 108) → qty 10000/108, FILLED at bar 3's open 108. Blend follows.
    add_qty = 10000.0 / 108.0
    blend = (100.0 * 100.0 + add_qty * 108.0) / (100.0 + add_qty)
    assert trade["shares"] == pytest.approx(100.0 + add_qty)
    assert trade["entry_price"] == pytest.approx(blend)


def test_scale_in_decision_ignores_fill_bar_range():
    # #1276: perturbing the FILL bar's high/low/close (holding its open)
    # must change neither the add decision nor the add size — the gate reads
    # only bar N's close.
    def _res(fill_bar_close, fill_bar_high):
        idx = pd.date_range("2024-01-01", periods=6, freq="D")
        df = pd.DataFrame({
            "open": [100.0, 100.0, 100.0, 108.0, 108.0, 108.0],
            "high": [100.5, 100.5, 108.0, fill_bar_high, 130.0, 130.0],
            "low": [99.5, 99.5, 99.5, 107.5, 107.5, 107.5],
            "close": [100.0, 100.0, 108.0, fill_bar_close, 120.0, 120.0],
            "signal": [1, 0, 1, 0, -1, 0],
        }, index=idx)
        bt = Backtester(initial_capital=10000, commission_pct=0,
                        slippage_pct=0, allow_scale_in=True,
                        scale_in={"add_spacing_atr": 0.0})
        return bt.run(df, save=False)

    base = _res(108.0, 108.5)
    perturbed = _res(125.0, 126.0)
    assert base["scale_in_adds"] == perturbed["scale_in_adds"] == 1
    assert (base["trades"][0]["shares"]
            == pytest.approx(perturbed["trades"][0]["shares"]))


def test_scale_in_spacing_gate_reads_signal_bar_close():
    # #1276: with +1 ATR(2) spacing from the 100 entry, the gate needs the
    # DECISION price (bar N's close) >= 102. Bar 2 closes at 101.9 (fails)
    # even though bar 3 — the would-be fill bar — opens at 104 (would pass).
    # Reading the fill bar's price would take the add; the gate must skip it.
    idx = pd.date_range("2024-01-01", periods=6, freq="D")
    df = pd.DataFrame({
        "open": [100.0, 100.0, 100.0, 104.0, 104.0, 104.0],
        "high": [100.5, 100.5, 102.0, 104.5, 104.5, 104.5],
        "low": [99.5, 99.5, 99.5, 103.5, 103.5, 103.5],
        "close": [100.0, 100.0, 101.9, 104.0, 104.0, 104.0],
        "atr": [2.0] * 6,
        "signal": [1, 0, 1, 0, 0, 0],
    }, index=idx)
    bt = Backtester(initial_capital=10000, commission_pct=0, slippage_pct=0,
                    allow_scale_in=True, scale_in={"add_spacing_atr": 1.0})
    result = bt.run(df, save=False)
    assert result["scale_in_adds"] == 0

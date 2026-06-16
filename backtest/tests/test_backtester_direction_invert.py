"""#942: Backtester models the live `invert_signal` and `direction` entry
transforms instead of silently dropping them.

`_apply_direction_invert` is the single transform point. It mirrors the live
scheduler ordering — `applySignalInversion` (BUY<->SELL) runs BEFORE
`EffectiveDirection` / `PerpsOrderSkipReason` gate which side may open — and is
path-aware because the raw signal means different things in the two engine
paths:

  • open/close path (a close evaluator drives exits): signal>0 opens long,
    signal<0 opens short; masking the disallowed open side is exact.
  • plain signal path: single-leg, so direction masking is skipped there.
    Long/flat (default): signal=1 opens long, signal=-1 only *closes* it.
    direction="short" engages the short/flat mirror (#989 — see
    test_backtester_short_leg); direction="both" stays unmodelable on this
    path and is rejected at config load (see test_strategy_refs_641).
"""
import pandas as pd
import pytest

from backtester import Backtester


# ─── _apply_direction_invert: pure transform mechanics ───────────────────────


def _bt(direction=None, invert_signal=False):
    return Backtester(initial_capital=1000, direction=direction,
                      invert_signal=invert_signal)


def test_invert_signal_negates_in_domain():
    out = _bt(invert_signal=True)._apply_direction_invert(
        pd.Series([1, -1, 0, 1]), uses_open_close=True)
    assert out.tolist() == [-1, 1, 0, -1]


def test_no_transform_when_unset():
    # Default (non --config) callers must be a pure pass-through in both paths.
    sig = pd.Series([1, -1, 0])
    assert _bt()._apply_direction_invert(sig, uses_open_close=True).tolist() == [1, -1, 0]
    assert _bt()._apply_direction_invert(sig, uses_open_close=False).tolist() == [1, -1, 0]


def test_direction_long_masks_short_opens_in_open_close_path():
    out = _bt(direction="long")._apply_direction_invert(
        pd.Series([1, -1, 0]), uses_open_close=True)
    assert out.tolist() == [1, 0, 0]  # the -1 short-open is dropped


def test_direction_short_masks_long_opens_in_open_close_path():
    out = _bt(direction="short")._apply_direction_invert(
        pd.Series([1, -1, 0]), uses_open_close=True)
    assert out.tolist() == [0, -1, 0]  # the +1 long-open is dropped


def test_direction_both_never_masks():
    out = _bt(direction="both")._apply_direction_invert(
        pd.Series([1, -1, 0]), uses_open_close=True)
    assert out.tolist() == [1, -1, 0]


def test_direction_long_plain_path_preserves_close_signal():
    # In the plain long/flat path signal=-1 CLOSES the long. Masking it would
    # wrongly suppress the exit, so the plain path leaves the signal untouched.
    out = _bt(direction="long")._apply_direction_invert(
        pd.Series([1, -1, 0]), uses_open_close=False)
    assert out.tolist() == [1, -1, 0]


def test_invert_runs_before_direction_gating():
    # Live order: invert flips first, THEN direction gates the resulting open
    # side. With direction="long" + invert:
    #   original BUY(+1)  -> invert -> SELL(-1) -> masked (no short open)
    #   original SELL(-1) -> invert -> BUY(+1)  -> opens long
    out = _bt(direction="long", invert_signal=True)._apply_direction_invert(
        pd.Series([1, -1]), uses_open_close=True)
    assert out.tolist() == [0, 1]


# ─── End-to-end: realized trade side reflects the transform ──────────────────


def _ohlc(signal):
    n = len(signal)
    # Flat prices: a pct close never fires, so the position survives to the
    # end-of-run flush and the recorded trade carries its OPEN side.
    return pd.DataFrame(
        {
            "open":   [100.0] * n,
            "high":   [101.0] * n,
            "low":    [99.0] * n,
            "close":  [100.0] * n,
            "volume": [1.0] * n,
            "signal": signal,
        },
        index=pd.date_range("2024-01-01", periods=n, freq="D"),
    )


_NEVER_FIRES_CLOSE = [{"name": "tiered_tp_pct", "params": {"tp_tiers": [
    {"profit_pct": 0.9, "close_fraction": 1.0},
]}}]

_REGIME_POLICY = {
    "trend_regime": {
        "trending_up": {"direction": "long", "invert_signal": False},
        "trending_down": {"direction": "short", "invert_signal": True},
        "ranging": {"direction": "long", "invert_signal": False},
    },
}


def _run(signal, **kw):
    bt = Backtester(
        initial_capital=1000, commission_pct=0.0, slippage_pct=0.0,
        close_strategies=_NEVER_FIRES_CLOSE, **kw,
    )
    return bt.run(_ohlc(signal), save=False)


def test_invert_signal_flips_realized_trade_side():
    # Same signal, opposite realized side once inversion is on (issue 944 #4).
    base = _run([1, 0, 0, 0], invert_signal=False)
    inv = _run([1, 0, 0, 0], invert_signal=True)
    assert [t["side"] for t in base["trades"]] == ["long"]
    assert [t["side"] for t in inv["trades"]] == ["short"]


def test_direction_long_blocks_short_entry_end_to_end():
    # A short-opening signal under direction="long" opens nothing.
    blocked = _run([-1, 0, 0, 0], direction="long")
    allowed = _run([-1, 0, 0, 0], direction="both")
    assert blocked["trades"] == []
    assert [t["side"] for t in allowed["trades"]] == ["short"]


def test_direction_short_blocks_long_entry_end_to_end():
    blocked = _run([1, 0, 0, 0], direction="short")
    allowed = _run([1, 0, 0, 0], direction="both")
    assert blocked["trades"] == []
    assert [t["side"] for t in allowed["trades"]] == ["long"]


def test_invert_then_direction_opens_long_from_inverted_sell():
    # direction="long" + invert: original SELL(-1) inverts to BUY(+1) and opens
    # a long; original BUY(+1) inverts to SELL(-1) and is gated out.
    inverted_sell = _run([-1, 0, 0, 0], direction="long", invert_signal=True)
    inverted_buy = _run([1, 0, 0, 0], direction="long", invert_signal=True)
    assert [t["side"] for t in inverted_sell["trades"]] == ["long"]
    assert inverted_buy["trades"] == []


def test_regime_directional_policy_opens_inverse_short():
    df = _ohlc([1, 0, 0, 0])
    df["regime"] = "trending_down"
    bt = Backtester(
        initial_capital=1000,
        commission_pct=0.0,
        slippage_pct=0.0,
        close_strategies=_NEVER_FIRES_CLOSE,
        regime_enabled=True,
        regime_directional_policy=_REGIME_POLICY,
    )
    res = bt.run(df, save=False)
    assert [t["side"] for t in res["trades"]] == ["short"]


def test_regime_directional_policy_holds_open_position_regime_plain_path():
    df = _ohlc([1, 0, 1, 0, 0])
    df["regime"] = "trending_down"
    # The second +1 decision would close the short if the resolver followed the
    # current row's trending_up regime. The stamped entry regime keeps the short
    # policy in force until the end-of-data flush.
    df.iloc[2:, df.columns.get_loc("regime")] = "trending_up"
    bt = Backtester(
        initial_capital=1000,
        commission_pct=0.0,
        slippage_pct=0.0,
        regime_enabled=True,
        regime_directional_policy=_REGIME_POLICY,
    )
    res = bt.run(df, save=False)
    assert [t["side"] for t in res["trades"]] == ["short"]
    assert res["trades"][0]["exit_reason"] == "end_of_data"


# ─── #1025 review: same-bar close→reopen on the open/close engine path ────────
#
# Invariant: an entry opened on a bar resolves its direction/invert from the
# regime that applies to a FLAT book at that bar (the current bar regime), never
# from a regime frozen to a position fully closed earlier in the same bar. The
# fix re-resolves the open-action gate AFTER the close leg, once the position
# state for the open block is known.


def _oc_flip_df(open_action, close_fraction, regime):
    # open_action / close_fraction / regime columns are each shifted +1 by the
    # engine (live N→N+1 fill), so row i governs the fill at bar i+1. Flat prices
    # (no stop fields, no close evaluator) keep the only close driver the
    # close_fraction column, so the surviving leg flushes at end-of-data.
    n = len(open_action)
    return pd.DataFrame(
        {
            "open":   [100.0] * n,
            "high":   [100.5] * n,
            "low":    [99.5] * n,
            "close":  [100.0] * n,
            "volume": [1.0] * n,
            "signal": [0] * n,
            "open_action": open_action,
            "close_fraction": close_fraction,
            "regime": regime,
        },
        index=pd.date_range("2024-01-01", periods=n, freq="D"),
    )


def _run_oc_flip(open_action, close_fraction, regime):
    bt = Backtester(
        initial_capital=1000, commission_pct=0.0, slippage_pct=0.0,
        regime_enabled=True, regime_directional_policy=_REGIME_POLICY,
    )
    return bt.run(_oc_flip_df(open_action, close_fraction, regime), save=False)


def test_open_close_same_bar_flip_reopens_from_current_regime():
    # Long opens at bar 1 in trending_up; at bar 3 the close_fraction column
    # fully closes it AND the open_action re-enters with the regime flipped to
    # trending_down (short policy). The re-entry must resolve from the CURRENT
    # bar regime (flat book → short), not the just-closed long's frozen
    # trending_up (which would wrongly re-open long).
    res = _run_oc_flip(
        open_action=["long", "none", "long", "none", "none", "none"],
        close_fraction=[0.0, 0.0, 1.0, 0.0, 0.0, 0.0],
        regime=["trending_up", "trending_up", "trending_down",
                "trending_down", "trending_down", "trending_down"],
    )
    assert [t["side"] for t in res["trades"]] == ["long", "short"]


def test_open_close_same_bar_flip_inverse_reopens_long():
    # Inverse case: short opens at bar 1 in trending_down; at bar 3 the regime
    # flips to trending_up and the same close+reopen lands. The re-entry must
    # open LONG from the current regime, not short from the frozen trending_down.
    res = _run_oc_flip(
        open_action=["long", "none", "long", "none", "none", "none"],
        close_fraction=[0.0, 0.0, 1.0, 0.0, 0.0, 0.0],
        regime=["trending_down", "trending_down", "trending_up",
                "trending_up", "trending_up", "trending_up"],
    )
    assert [t["side"] for t in res["trades"]] == ["short", "long"]


def test_open_close_partial_close_keeps_frozen_regime_no_flip():
    # A partial close on a regime-flip bar must NOT re-open: the surviving leg
    # keeps position != 0 (the open block can't fire) and its frozen regime, so
    # no opposite-side phantom trade appears. Only the two long legs (the partial
    # close at bar 3 and the surviving leg at end-of-data) are recorded — the
    # freeze must still hold whenever the position stays non-zero.
    res = _run_oc_flip(
        open_action=["long", "none", "long", "none", "none", "none"],
        close_fraction=[0.0, 0.0, 0.5, 0.0, 0.0, 0.0],
        regime=["trending_up", "trending_up", "trending_down",
                "trending_down", "trending_down", "trending_down"],
    )
    assert [t["side"] for t in res["trades"]] == ["long", "long"]
    assert res["trades"][-1]["exit_reason"] == "end_of_data"

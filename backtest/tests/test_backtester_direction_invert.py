import pandas as pd
import pytest

from backtester import Backtester


def _bt(direction=None, invert_signal=False):
    return Backtester(initial_capital=1000, direction=direction,
                      invert_signal=invert_signal)


def test_invert_signal_negates_in_domain():
    out = _bt(invert_signal=True)._apply_direction_invert(
        pd.Series([1, -1, 0, 1]), uses_open_close=True)
    assert out.tolist() == [-1, 1, 0, -1]


def test_no_transform_when_unset():
    sig = pd.Series([1, -1, 0])
    assert _bt()._apply_direction_invert(sig, uses_open_close=True).tolist() == [1, -1, 0]
    assert _bt()._apply_direction_invert(sig, uses_open_close=False).tolist() == [1, -1, 0]


def test_direction_long_masks_short_opens_in_open_close_path():
    out = _bt(direction="long")._apply_direction_invert(
        pd.Series([1, -1, 0]), uses_open_close=True)
    assert out.tolist() == [1, 0, 0]


def test_direction_short_masks_long_opens_in_open_close_path():
    out = _bt(direction="short")._apply_direction_invert(
        pd.Series([1, -1, 0]), uses_open_close=True)
    assert out.tolist() == [0, -1, 0]


def test_direction_both_never_masks():
    out = _bt(direction="both")._apply_direction_invert(
        pd.Series([1, -1, 0]), uses_open_close=True)
    assert out.tolist() == [1, -1, 0]


def test_direction_long_plain_path_preserves_close_signal():
    out = _bt(direction="long")._apply_direction_invert(
        pd.Series([1, -1, 0]), uses_open_close=False)
    assert out.tolist() == [1, -1, 0]


def test_invert_runs_before_direction_gating():
    out = _bt(direction="long", invert_signal=True)._apply_direction_invert(
        pd.Series([1, -1]), uses_open_close=True)
    assert out.tolist() == [0, 1]


def _ohlc(signal):
    n = len(signal)
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
    base = _run([1, 0, 0, 0], invert_signal=False)
    inv = _run([1, 0, 0, 0], invert_signal=True)
    assert [t["side"] for t in base["trades"]] == ["long"]
    assert [t["side"] for t in inv["trades"]] == ["short"]


def test_direction_long_blocks_short_entry_end_to_end():
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
        regime_directional_certified=True,
    )
    res = bt.run(df, save=False)
    assert [t["side"] for t in res["trades"]] == ["short"]


def test_regime_directional_policy_default_off_when_uncertified():
    df = _ohlc([1, 0, 0, 0])
    df["regime"] = "trending_down"
    bt = Backtester(
        initial_capital=1000,
        commission_pct=0.0,
        slippage_pct=0.0,
        close_strategies=_NEVER_FIRES_CLOSE,
        regime_enabled=True,
        regime_directional_policy=_REGIME_POLICY,
        regime_directional_certified=False,
    )
    res = bt.run(df, save=False)
    assert [t["side"] for t in res["trades"]] == ["long"]


def test_regime_directional_policy_per_state_certified_states_honors_match():
    df = _ohlc([1, 0, 0, 0])
    df["regime"] = "trending_down"
    bt = Backtester(
        initial_capital=1000,
        commission_pct=0.0,
        slippage_pct=0.0,
        close_strategies=_NEVER_FIRES_CLOSE,
        regime_enabled=True,
        regime_directional_policy=_REGIME_POLICY,
        regime_directional_certified_states={"trending_down": "short"},
    )
    res = bt.run(df, save=False)
    assert [t["side"] for t in res["trades"]] == ["short"]


def test_regime_directional_policy_per_state_sign_contradiction_falls_to_base():
    df = _ohlc([1, 0, 0, 0])
    df["regime"] = "trending_down"
    bt = Backtester(
        initial_capital=1000,
        commission_pct=0.0,
        slippage_pct=0.0,
        close_strategies=_NEVER_FIRES_CLOSE,
        regime_enabled=True,
        regime_directional_policy=_REGIME_POLICY,
        regime_directional_certified_states={"trending_down": "long"},
    )
    res = bt.run(df, save=False)
    assert [t["side"] for t in res["trades"]] == ["long"]


def test_regime_directional_policy_holds_open_position_regime_plain_path():
    df = _ohlc([1, 0, 1, 0, 0])
    df["regime"] = "trending_down"
    df.iloc[2:, df.columns.get_loc("regime")] = "trending_up"
    bt = Backtester(
        initial_capital=1000,
        commission_pct=0.0,
        slippage_pct=0.0,
        regime_enabled=True,
        regime_directional_policy=_REGIME_POLICY,
        regime_directional_certified=True,
    )
    res = bt.run(df, save=False)
    assert [t["side"] for t in res["trades"]] == ["short"]
    assert res["trades"][0]["exit_reason"] == "end_of_data"


def _oc_flip_df(open_action, close_fraction, regime):
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
        regime_directional_certified=True,
    )
    return bt.run(_oc_flip_df(open_action, close_fraction, regime), save=False)


def test_open_close_same_bar_flip_reopens_from_current_regime():
    res = _run_oc_flip(
        open_action=["long", "none", "long", "none", "none", "none"],
        close_fraction=[0.0, 0.0, 1.0, 0.0, 0.0, 0.0],
        regime=["trending_up", "trending_up", "trending_down",
                "trending_down", "trending_down", "trending_down"],
    )
    assert [t["side"] for t in res["trades"]] == ["long", "short"]


def test_open_close_same_bar_flip_inverse_reopens_long():
    res = _run_oc_flip(
        open_action=["long", "none", "long", "none", "none", "none"],
        close_fraction=[0.0, 0.0, 1.0, 0.0, 0.0, 0.0],
        regime=["trending_down", "trending_down", "trending_up",
                "trending_up", "trending_up", "trending_up"],
    )
    assert [t["side"] for t in res["trades"]] == ["short", "long"]


def test_open_close_partial_close_keeps_frozen_regime_no_flip():
    res = _run_oc_flip(
        open_action=["long", "none", "long", "none", "none", "none"],
        close_fraction=[0.0, 0.0, 0.5, 0.0, 0.0, 0.0],
        regime=["trending_up", "trending_up", "trending_down",
                "trending_down", "trending_down", "trending_down"],
    )
    assert [t["side"] for t in res["trades"]] == ["long", "long"]
    assert res["trades"][-1]["exit_reason"] == "end_of_data"

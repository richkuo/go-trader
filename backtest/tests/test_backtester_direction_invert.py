import pandas as pd
import pytest

from backtester import Backtester


def _bt(direction=None, invert_signal=False):
    return Backtester(initial_capital=1000, direction=direction,
                      invert_signal=invert_signal)


@pytest.mark.parametrize(
    "direction,invert_signal,uses_open_close,signal,expected",
    [
        (None, True, True, [1, -1, 0, 1], [-1, 1, 0, -1]),
        (None, False, True, [1, -1, 0], [1, -1, 0]),
        (None, False, False, [1, -1, 0], [1, -1, 0]),
        ("long", False, True, [1, -1, 0], [1, 0, 0]),
        ("short", False, True, [1, -1, 0], [0, -1, 0]),
        ("both", False, True, [1, -1, 0], [1, -1, 0]),
        ("long", False, False, [1, -1, 0], [1, -1, 0]),
        ("long", True, True, [1, -1], [0, 1]),
    ],
)
def test_apply_direction_invert(direction, invert_signal, uses_open_close,
                                signal, expected):
    out = _bt(direction=direction,
              invert_signal=invert_signal)._apply_direction_invert(
        pd.Series(signal), uses_open_close=uses_open_close)
    assert out.tolist() == expected


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


@pytest.mark.parametrize(
    "signal,kwargs,expected_sides",
    [
        ([1, 0, 0, 0], {"invert_signal": False}, ["long"]),
        ([1, 0, 0, 0], {"invert_signal": True}, ["short"]),
        ([-1, 0, 0, 0], {"direction": "long"}, []),
        ([-1, 0, 0, 0], {"direction": "both"}, ["short"]),
        ([1, 0, 0, 0], {"direction": "short"}, []),
        ([1, 0, 0, 0], {"direction": "both"}, ["long"]),
        ([-1, 0, 0, 0], {"direction": "long", "invert_signal": True}, ["long"]),
        ([1, 0, 0, 0], {"direction": "long", "invert_signal": True}, []),
    ],
)
def test_direction_and_invert_realized_sides(signal, kwargs, expected_sides):
    res = _run(signal, **kwargs)
    assert [t["side"] for t in res["trades"]] == expected_sides


@pytest.mark.parametrize(
    "cert_kwargs,expected_sides",
    [
        ({"regime_directional_certified": True}, ["short"]),
        ({"regime_directional_certified": False}, ["long"]),
        ({"regime_directional_certified_states": {"trending_down": "short"}},
         ["short"]),
        ({"regime_directional_certified_states": {"trending_down": "long"}},
         ["long"]),
    ],
)
def test_regime_directional_policy_certification(cert_kwargs, expected_sides):
    df = _ohlc([1, 0, 0, 0])
    df["regime"] = "trending_down"
    bt = Backtester(
        initial_capital=1000,
        commission_pct=0.0,
        slippage_pct=0.0,
        close_strategies=_NEVER_FIRES_CLOSE,
        regime_enabled=True,
        regime_directional_policy=_REGIME_POLICY,
        **cert_kwargs,
    )
    res = bt.run(df, save=False)
    assert [t["side"] for t in res["trades"]] == expected_sides


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


@pytest.mark.parametrize(
    "close_fraction,regime,expected_sides,expected_last_exit_reason",
    [
        (
            [0.0, 0.0, 1.0, 0.0, 0.0, 0.0],
            ["trending_up", "trending_up", "trending_down",
             "trending_down", "trending_down", "trending_down"],
            ["long", "short"],
            None,
        ),
        (
            [0.0, 0.0, 1.0, 0.0, 0.0, 0.0],
            ["trending_down", "trending_down", "trending_up",
             "trending_up", "trending_up", "trending_up"],
            ["short", "long"],
            None,
        ),
        (
            [0.0, 0.0, 0.5, 0.0, 0.0, 0.0],
            ["trending_up", "trending_up", "trending_down",
             "trending_down", "trending_down", "trending_down"],
            ["long", "long"],
            "end_of_data",
        ),
    ],
)
def test_open_close_same_bar_flip(close_fraction, regime, expected_sides,
                                  expected_last_exit_reason):
    res = _run_oc_flip(
        open_action=["long", "none", "long", "none", "none", "none"],
        close_fraction=close_fraction,
        regime=regime,
    )
    assert [t["side"] for t in res["trades"]] == expected_sides
    if expected_last_exit_reason is not None:
        assert res["trades"][-1]["exit_reason"] == expected_last_exit_reason

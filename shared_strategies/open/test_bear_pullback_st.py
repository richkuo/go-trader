
import numpy as np

from shared_strategies.open.conftest import load_module, make_ohlcv

_BEAR_PULLBACK = load_module("_bear_pullback_st_test", __file__.replace("test_bear_pullback_st.py", "bear_pullback_st.py"))
bear_pullback_st_core = _BEAR_PULLBACK.bear_pullback_st_core


def _bear_setup_with_rally_and_rejection():
    rng = np.random.default_rng(42)
    down = np.linspace(200.0, 110.0, 230) + rng.normal(0, 0.4, 230)
    rally = np.linspace(110.0, 132.0, 10)
    reject = [128.0, 124.0, 119.0, 113.0]
    closes = np.concatenate([down, rally, reject])
    return make_ohlcv(closes)


def test_emits_short_on_failed_rally_in_bear_trend():
    df = _bear_setup_with_rally_and_rejection()
    result = bear_pullback_st_core(df)
    assert (result["signal"] == -1).any(), (
        "Expected at least one short signal on rejection bars after a bear-trend rally"
    )
    last_signals = result["signal"].iloc[-5:]
    assert (last_signals == -1).any(), (
        f"Short signal should land in the rejection window, got {last_signals.tolist()}"
    )



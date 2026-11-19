
import pathlib
import sys


_SHARED_TOOLS = pathlib.Path(__file__).parent.parent / "shared_tools"
_REPO_ROOT = _SHARED_TOOLS.parent
if str(_REPO_ROOT) not in sys.path:
    sys.path.insert(0, str(_REPO_ROOT))
if str(_SHARED_TOOLS) not in sys.path:
    sys.path.insert(0, str(_SHARED_TOOLS))

from shared_tools.conftest import load_module

_REGIME = load_module("_regime_wiring_regime_test", _SHARED_TOOLS / "regime.py")
latest_regime = _REGIME.latest_regime
latest_regime_composite = _REGIME.latest_regime_composite
_STRATEGY_COMPOSITION = load_module("_regime_wiring_composition_test", _SHARED_TOOLS / "strategy_composition.py")


def _load_check_options_module():
    src_path = pathlib.Path(__file__).parent / "check_options.py"
    return load_module("_check_options_under_test", src_path)


def test_check_options_fetch_ohlcv_df_reads_the_adapter_and_refuses_short_frames(subtests):
    module = _load_check_options_module()

    with subtests.test(case="uses_adapter_when_available"):
        class StubAdapter:
            def __init__(self, rows):
                self._rows = rows
                self.calls = []

            def get_ohlcv(self, symbol, timeframe, limit):
                self.calls.append((symbol, timeframe, limit))
                return self._rows

        rows = [
            [i * 1000, 100.0 + i, 101.0 + i, 99.0 + i, 100.5 + i, 1000.0]
            for i in range(50)
        ]
        adapter = StubAdapter(rows)
        df = module._fetch_ohlcv_df("BTC", "4h", 100, 30, adapter=adapter)
        assert df is not None
        assert len(df) == 50
        assert {"high", "low", "close"}.issubset(df.columns)
        assert adapter.calls == [("BTC", "4h", 100)]

    with subtests.test(case="short_returns_none"):
        class ShortAdapter:
            def get_ohlcv(self, symbol, timeframe, limit):
                return [[i * 1000, 100.0, 101.0, 99.0, 100.5, 1000.0] for i in range(5)]

        df = module._fetch_ohlcv_df("BTC", "4h", 100, 30, adapter=ShortAdapter())
        assert df is None

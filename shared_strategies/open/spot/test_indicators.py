
import importlib.util
import numpy as np
import pandas as pd
import pytest

import sys, os

_spot_dir = os.path.dirname(os.path.abspath(__file__))
_shared_dir = os.path.join(_spot_dir, '..')
sys.path.insert(0, _spot_dir)
sys.path.insert(0, _shared_dir)

_spec = importlib.util.spec_from_file_location(
    "spot_indicators", os.path.join(_spot_dir, "indicators.py"))
_imod = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(_imod)

sma = _imod.sma
ema = _imod.ema
sma_crossover = _imod.sma_crossover
rsi = _imod.rsi
bollinger_bands = _imod.bollinger_bands

from shared_strategies.open.conftest import make_ohlcv


class TestSMACrossover:
    @pytest.mark.parametrize("closes,expected", [
        (list(np.linspace(120, 90, 60)) + list(np.linspace(90, 130, 60)), 1),
        (list(np.linspace(90, 130, 60)) + list(np.linspace(130, 80, 60)), -1),
    ])
    def test_crossover_emits_signal(self, closes, expected):
        result = sma_crossover(make_ohlcv(closes), fast_period=10, slow_period=30)
        assert "signal" in result.columns
        assert len(result[result["signal"] == expected]) >= 1



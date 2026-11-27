import os, sys
import numpy as np

_THIS = os.path.dirname(os.path.abspath(__file__))
_BACKTEST = os.path.abspath(os.path.join(_THIS, ".."))
_ROOT = os.path.abspath(os.path.join(_BACKTEST, ".."))
for _p in (_BACKTEST, _ROOT, os.path.join(_ROOT, "shared_tools")):
    if _p not in sys.path:
        sys.path.insert(0, _p)

import regime_vol_model as rvm


def test_handrule_cells_partition_matches_map_composite_label():
    from regime import map_composite_label, _DEFAULT_COMPOSITE_THRESHOLDS as TH
    th = dict(TH)
    cells = rvm.handrule_cells(th)
    assert set(cells) == {"trending_up_clean", "trending_up_choppy", "trending_down_clean",
                          "trending_down_choppy", "ranging_directional_up",
                          "ranging_directional_down", "ranging_directional",
                          "ranging_volatile", "ranging_quiet"}
    rng = np.random.default_rng(7)
    scale = {"return_eff": 1.0, "range_eff": 1.0, "efficiency": 1.0, "adx": 1.0}
    for _ in range(2000):
        pt = {"return_eff": float(rng.uniform(-0.2, 0.2)), "range_eff": float(rng.uniform(0, 0.1)),
              "efficiency": float(rng.uniform(0, 1)), "adx": float(rng.uniform(0, 60))}
        expected = map_composite_label(pt["return_eff"], pt["adx"], pt["range_eff"],
                                       pt["efficiency"], th)
        zero = [lab for lab, cell in cells.items() if rvm.cell_distance_z(pt, scale, cell) == 0.0]
        assert zero == [expected], (pt, zero, expected)

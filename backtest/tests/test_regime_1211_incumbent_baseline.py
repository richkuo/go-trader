"""Unit tests for #1211 incumbent-baseline harness: pure statistical helpers + the
pre-registered replication decision rule. No cached data required except one guarded smoke test."""
import importlib.util
import os, sys

import numpy as np
import pytest

_THIS = os.path.dirname(os.path.abspath(__file__))
_BACKTEST = os.path.abspath(os.path.join(_THIS, ".."))
_ROOT = os.path.abspath(os.path.join(_BACKTEST, ".."))
for _p in (_BACKTEST, _ROOT, os.path.join(_ROOT, "shared_tools")):
    if _p not in sys.path:
        sys.path.insert(0, _p)


def _load():
    path = os.path.join(_BACKTEST, "research", "regime_1211_incumbent_baseline.py")
    spec = importlib.util.spec_from_file_location("regime_1211_under_test", path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


M = _load()


# --------------------------------------------------------------------------- epsilon_squared

def test_epsilon_squared_known_value():
    # (H - k + 1) / (n - k) = (10 - 3 + 1) / (100 - 3) = 8/97
    assert M.epsilon_squared(10.0, 100, 3) == pytest.approx(8.0 / 97.0)


def test_epsilon_squared_clamps_and_guards_degenerate_shapes():
    assert np.isnan(M.epsilon_squared(5.0, 3, 3))       # n <= k -> nan (no division)
    assert np.isnan(M.epsilon_squared(5.0, 10, 0))      # k < 1 -> nan
    assert M.epsilon_squared(1e9, 100, 3) == 1.0        # clamped to 1.0
    assert M.epsilon_squared(0.0, 100, 3) == 0.0        # clamped to 0.0 (never negative)


# --------------------------------------------------------------------------- effect profile

def test_forward_vol_profile_is_median_ratio_per_state():
    labels = np.array(["a", "a", "b", "b"], dtype=object)
    fwd = np.array([1.0, 1.0, 3.0, 3.0])                 # pooled median = 2.0
    prof = M.forward_vol_profile(labels, fwd)
    assert prof["a"] == pytest.approx(0.5)
    assert prof["b"] == pytest.approx(1.5)


def test_forward_vol_profile_flat_when_pooled_median_nonpositive():
    labels = np.array(["a", "b"], dtype=object)
    fwd = np.array([0.0, 0.0])
    prof = M.forward_vol_profile(labels, fwd)
    assert prof == {"a": 1.0, "b": 1.0}


def test_forward_vol_profile_drops_nan_forward():
    labels = np.array(["a", "a", "b"], dtype=object)
    fwd = np.array([2.0, np.nan, 6.0])                   # pooled median over {2,6} = 4.0
    prof = M.forward_vol_profile(labels, fwd)
    assert prof["a"] == pytest.approx(0.5)
    assert prof["b"] == pytest.approx(1.5)


# --------------------------------------------------------------------------- inject_effect

def test_inject_effect_lam_zero_is_identity():
    labels = np.array(["a", "b", "a"], dtype=object)
    fwd = np.array([1.0, 2.0, 3.0])
    prof = {"a": 0.5, "b": 2.0}
    assert np.allclose(M.inject_effect(labels, fwd, prof, 0.0), fwd)


def test_inject_effect_lam_one_applies_profile():
    labels = np.array(["a", "b"], dtype=object)
    fwd = np.array([4.0, 4.0])
    prof = {"a": 0.5, "b": 2.0}
    assert np.allclose(M.inject_effect(labels, fwd, prof, 1.0), [2.0, 8.0])


def test_inject_effect_monotone_separation_in_lam():
    # As lam grows, the between-state gap widens monotonically.
    labels = np.array(["lo", "hi"], dtype=object)
    fwd = np.array([1.0, 1.0])
    prof = {"lo": 0.5, "hi": 2.0}
    gaps = []
    for lam in (0.0, 0.5, 1.0, 2.0):
        out = M.inject_effect(labels, fwd, prof, lam)
        gaps.append(out[1] - out[0])
    assert gaps == sorted(gaps)
    assert gaps[0] == pytest.approx(0.0)                 # lam=0 -> no gap


def test_inject_effect_unknown_state_defaults_to_unit_ratio():
    labels = np.array(["a", "z"], dtype=object)          # "z" absent from profile
    fwd = np.array([2.0, 5.0])
    prof = {"a": 3.0}
    out = M.inject_effect(labels, fwd, prof, 1.0)
    assert out[0] == pytest.approx(6.0)                  # a scaled
    assert out[1] == pytest.approx(5.0)                  # z left unchanged (ratio 1.0)


# --------------------------------------------------------------------------- block_bootstrap

def test_block_bootstrap_length_and_membership():
    rng = np.random.default_rng(0)
    src = np.arange(10.0)
    out = M.block_bootstrap(src, block_len=3, n_out=7, rng=rng)
    assert len(out) == 7
    assert set(out.tolist()).issubset(set(src.tolist()))  # only resampled source values


def test_block_bootstrap_block_len_one_is_iid_resample():
    rng = np.random.default_rng(1)
    src = np.array([5.0, 5.0, 5.0])
    out = M.block_bootstrap(src, block_len=1, n_out=100, rng=rng)
    assert np.all(out == 5.0)


# --------------------------------------------------------------------------- simulate_power

def _synthetic_cell(n=600, block_len=6, seed=0):
    """A label stream with dwell structure + a pooled forward-vol sample."""
    rng = np.random.default_rng(seed)
    labels = np.repeat(rng.integers(0, 3, size=n // block_len + 1).astype(object),
                       block_len)[:n]
    fwd = np.abs(rng.normal(1.0, 0.3, size=n)) + 0.1     # strictly positive vols
    return labels, fwd


def test_simulate_power_null_is_near_alpha_and_monotone():
    labels, fwd = _synthetic_cell()
    prof = {"0": 0.5, "1": 1.0, "2": 2.0}
    lam_grid = (0.0, 1.0, 2.0)
    powers = M.simulate_power(labels, fwd, block_len=6, profile=prof, lam_grid=lam_grid,
                              n_sim=120, n_perm=99, alpha=0.05, seed=7)
    # lam=0 (no injected association) -> power ~ alpha (type-I), generously bounded for n_sim=120.
    assert powers[0.0] <= 0.20
    # Power is non-decreasing in lam, and a strong injected effect is detected far above null.
    assert powers[0.0] <= powers[1.0] <= powers[2.0]
    assert powers[2.0] > powers[0.0]


# --------------------------------------------------------------------------- min_detectable_effect

def test_min_detectable_effect_interpolates_crossing():
    grid = (0.0, 1.0, 2.0)
    powers = {0.0: 0.1, 1.0: 0.6, 2.0: 0.9}              # crosses 0.8 between 1.0 and 2.0
    mde = M.min_detectable_effect(grid, powers, target=0.8)
    # 0.6 -> 0.9 spans lam 1->2; 0.8 is 2/3 of the way: 1 + (0.2/0.3) = 1.6667
    assert mde == pytest.approx(1.0 + (0.8 - 0.6) / (0.9 - 0.6))


def test_min_detectable_effect_none_when_never_powered():
    grid = (0.0, 1.0, 2.0)
    powers = {0.0: 0.05, 1.0: 0.2, 2.0: 0.4}
    assert M.min_detectable_effect(grid, powers, target=0.8) is None


def test_min_detectable_effect_first_point_already_powered():
    grid = (0.0, 1.0)
    powers = {0.0: 0.95, 1.0: 0.99}
    assert M.min_detectable_effect(grid, powers, target=0.8) == 0.0


# --------------------------------------------------------------------------- replication_verdict

def _cell(symbol, tf, window, p, *, knife_edge=False, seed_stable=True):
    return {"symbol": symbol, "timeframe": tf, "window": window, "p_value": p,
            "knife_edge": knife_edge, "seed_stable": seed_stable}


def test_replicates_requires_primary_breadth_and_two_symbols():
    alpha = 0.01
    cells = [
        _cell("BTC/USDT", "1h", "2023", 0.001),          # primary, significant, stable, not knife
        _cell("BTC/USDT", "1h", "2024", 0.002),
        _cell("ETH/USDT", "1h", "2023", 0.003),          # 2nd symbol significant
        _cell("SOL/USDT", "1h", "2023", 0.5),            # insignificant
    ]
    v = M.replication_verdict(cells, alpha)
    assert v["replicates"] is True
    assert v["primary_met"] and v["breadth_met"] and v["symbols_met"]


def test_primary_only_fails_breadth():
    alpha = 0.01
    cells = [
        _cell("BTC/USDT", "1h", "2023", 0.001),          # only significant cell
        _cell("ETH/USDT", "1h", "2023", 0.5),
        _cell("SOL/USDT", "1h", "2023", 0.5),
        _cell("ETH/USDT", "4h", "2024", 0.5),
    ]
    v = M.replication_verdict(cells, alpha)
    assert v["primary_met"] is True
    assert v["breadth_met"] is False                      # 1 of 4 < half
    assert v["replicates"] is False


def test_breadth_without_primary_fails():
    alpha = 0.01
    cells = [
        _cell("ETH/USDT", "1h", "2023", 0.001),
        _cell("SOL/USDT", "1h", "2023", 0.001),
        _cell("BTC/USDT", "1h", "2023", 0.5),            # primary NOT significant
    ]
    v = M.replication_verdict(cells, alpha)
    assert v["breadth_met"] is True and v["symbols_met"] is True
    assert v["primary_met"] is False
    assert v["replicates"] is False


def test_knife_edge_primary_blocks_replication():
    alpha = 0.01
    cells = [
        _cell("BTC/USDT", "1h", "2023", 0.001, knife_edge=True),   # knife-edge -> not trusted
        _cell("ETH/USDT", "1h", "2023", 0.002),
    ]
    v = M.replication_verdict(cells, alpha)
    assert v["primary_met"] is False
    assert v["replicates"] is False


def test_seed_unstable_primary_blocks_replication():
    alpha = 0.01
    cells = [
        _cell("BTC/USDT", "1h", "2023", 0.001, seed_stable=False),
        _cell("ETH/USDT", "1h", "2023", 0.002),
    ]
    v = M.replication_verdict(cells, alpha)
    assert v["primary_met"] is False
    assert v["replicates"] is False


def test_single_symbol_breadth_fails_symbols():
    alpha = 0.01
    cells = [
        _cell("BTC/USDT", "1h", "2023", 0.001),
        _cell("BTC/USDT", "4h", "2023", 0.001),
        _cell("BTC/USDT", "1h", "2024", 0.001),          # all BTC -> only one symbol
        _cell("ETH/USDT", "1h", "2023", 0.5),
    ]
    v = M.replication_verdict(cells, alpha)
    assert v["breadth_met"] is True and v["primary_met"] is True
    assert v["symbols_met"] is False
    assert v["replicates"] is False


def test_verdict_counts_available_denominator_only():
    # replication_verdict receives ONLY available (scored) cells; unavailable never reach it.
    alpha = 0.01
    cells = [_cell("BTC/USDT", "1h", "2023", 0.001), _cell("ETH/USDT", "1h", "2023", 0.001)]
    v = M.replication_verdict(cells, alpha)
    assert v["n_available_cells"] == 2
    assert v["n_significant_cells"] == 2


# --------------------------------------------------------------------------- cache-stability guard

def _fake_df(index):
    import pandas as pd
    n = len(index)
    return pd.DataFrame({"open": np.ones(n), "high": np.ones(n), "low": np.ones(n),
                         "close": np.ones(n), "volume": np.ones(n)}, index=index)


def test_load_cell_rejects_duplicate_timestamps(monkeypatch):
    import pandas as pd
    base = pd.date_range("2023-01-01", periods=M.MIN_VALID_BARS + 50, freq="h")
    dup_index = base.insert(10, base[9])                 # one duplicated timestamp
    monkeypatch.setattr(M, "load_cached_data", lambda *a, **k: _fake_df(dup_index))
    cell = M.load_cell("BTC/USDT", "1h", "2023")
    assert cell["status"] == "unavailable"
    assert "cache_unstable" in cell["reason"] and "dup_timestamps=1" in cell["reason"]


def test_load_cell_rejects_non_monotonic_index(monkeypatch):
    import pandas as pd
    base = list(pd.date_range("2023-01-01", periods=M.MIN_VALID_BARS + 50, freq="h"))
    base[20], base[21] = base[21], base[20]              # out-of-order pair (mid-write signature)
    monkeypatch.setattr(M, "load_cached_data", lambda *a, **k: _fake_df(pd.DatetimeIndex(base)))
    cell = M.load_cell("BTC/USDT", "1h", "2023")
    assert cell["status"] == "unavailable"
    assert "cache_unstable" in cell["reason"]


# --------------------------------------------------------------------------- guarded smoke test

def test_load_cell_on_cached_data_if_available():
    """If the BTC/USDT 1h OOS cache exists, load_cell returns a scoreable cell whose kruskal_h
    matches run_window's h4 (proving the harness mirrors the incumbent measurement exactly)."""
    cell = M.load_cell("BTC/USDT", "1h", "oos")
    if cell["status"] != "available":
        pytest.skip(f"no cached data: {cell.get('reason')}")
    from regime_diagnostics import run_window
    rep = run_window("BTC/USDT", "1h", "oos", model=None, horizons=(M.PRIMARY_HORIZON,),
                     target="volatility", n_perm=99, seed=0)
    assert cell["kruskal_h"] == pytest.approx(rep["h4"]["separation"]["kruskal_h"])
    assert cell["n_valid"] > M.MIN_VALID_BARS
    assert cell["k_states"] >= 1

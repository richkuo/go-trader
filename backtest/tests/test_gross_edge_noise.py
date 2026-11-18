
import numpy as np
import pandas as pd

import eval_windows as ew
import gross_edge_noise as gen


def test_pool_trade_samples_drops_exact_and_overlapping_entries(subtests):
    with subtests.test(case="test_pool_exact_duplicate_still_dropped"):
        legs = [
            {"dataset": "BTC/USDT 1h", "window": "is",
             "trade_samples": [{"entry_date": "2025-08-15 04:00:00", "pnl_pct": 1.0}]},
            {"dataset": "BTC/USDT 1h", "window": "is",
             "trade_samples": [{"entry_date": "2025-08-15 04:00:00", "pnl_pct": 1.0}]},
            {"dataset": "ETH/USDT 1h", "window": "is", "trade_samples": []},
        ]
        samples, n_exact, n_overlap = gen.pool_trade_samples(legs)
        assert n_exact == 1 and n_overlap == 0
        assert len(samples) == 1
        assert samples[0]["window"] == "is"

    with subtests.test(case="test_pool_overlap_nonidentical_entries_dropped"):
        legs = [
            {"dataset": "BTC/USDT 1h", "window": "is",
             "trade_samples": [{"entry_date": "2025-06-17 19:00:00", "pnl_pct": 1.37}]},
            {"dataset": "BTC/USDT 1h", "window": "2025H1",
             "trade_samples": [{"entry_date": "2025-06-18 21:00:00", "pnl_pct": 0.22},
                               {"entry_date": "2025-03-01 00:00:00", "pnl_pct": -0.5}]},
        ]
        samples, n_exact, n_overlap = gen.pool_trade_samples(legs)
        assert n_exact == 0 and n_overlap == 1
        assert [s["entry_date"] for s in samples] == [
            "2025-06-17 19:00:00", "2025-03-01 00:00:00"]

    with subtests.test(case="test_pool_shared_start_windows_keep_distinct_entries"):
        windows = {"a": ("2023-01-01", "2024-01-01"),
                   "b": ("2023-01-01", "2025-01-01")}
        legs = [
            {"dataset": "BTC/USDT 1h", "window": "a",
             "trade_samples": [{"entry_date": "2023-05-01 00:00:00", "pnl_pct": 1.0}]},
            {"dataset": "BTC/USDT 1h", "window": "b",
             "trade_samples": [{"entry_date": "2023-06-01 00:00:00", "pnl_pct": 2.0},
                               {"entry_date": "2024-06-01 00:00:00", "pnl_pct": 3.0}]},
        ]
        samples, n_exact, n_overlap = gen.pool_trade_samples(legs, windows=windows)
        assert n_exact == 0 and n_overlap == 1
        assert [s["pnl_pct"] for s in samples] == [1.0, 3.0]

    with subtests.test(case="test_pool_zero_trade_leg_still_claims_coverage"):
        windows = {"a": ("2023-01-01", "2024-01-01"),
                   "b": ("2023-06-01", "2024-06-01")}
        legs = [
            {"dataset": "BTC/USDT 1h", "window": "a", "trade_samples": []},
            {"dataset": "BTC/USDT 1h", "window": "b",
             "trade_samples": [{"entry_date": "2023-08-01 00:00:00", "pnl_pct": 1.0},
                               {"entry_date": "2024-03-01 00:00:00", "pnl_pct": 2.0}]},
        ]
        samples, n_exact, n_overlap = gen.pool_trade_samples(legs, windows=windows)
        assert n_overlap == 1
        assert [s["pnl_pct"] for s in samples] == [2.0]

    with subtests.test(case="test_pool_coverage_is_per_dataset"):
        windows = {"a": ("2023-01-01", "2024-01-01"),
                   "b": ("2023-01-01", "2024-01-01")}
        legs = [
            {"dataset": "BTC/USDT 1h", "window": "a",
             "trade_samples": [{"entry_date": "2023-05-01 00:00:00", "pnl_pct": 1.0}]},
            {"dataset": "ETH/USDT 1h", "window": "b",
             "trade_samples": [{"entry_date": "2023-05-01 00:00:00", "pnl_pct": 2.0}]},
        ]
        samples, n_exact, n_overlap = gen.pool_trade_samples(legs, windows=windows)
        assert n_exact == 0 and n_overlap == 0
        assert len(samples) == 2

    with subtests.test(case="test_pool_open_ended_window_claims_to_infinity"):
        windows = {"oos": ("2026-01-01", None),
                   "late": ("2026-06-01", "2027-01-01")}
        legs = [
            {"dataset": "BTC/USDT 1h", "window": "oos",
             "trade_samples": [{"entry_date": "2026-02-01 00:00:00", "pnl_pct": 1.0}]},
            {"dataset": "BTC/USDT 1h", "window": "late",
             "trade_samples": [{"entry_date": "2026-08-01 00:00:00", "pnl_pct": 2.0}]},
        ]
        samples, n_exact, n_overlap = gen.pool_trade_samples(legs, windows=windows)
        assert n_overlap == 1
        assert [s["pnl_pct"] for s in samples] == [1.0]


def test_trade_samples_from_results_extracts_entry_and_pnl():
    results = {"trades": [
        {"entry_date": "2025-06-15 04:00:00", "pnl_pct": 1.25, "pnl": 12.5},
        {"entry_date": "2025-06-20 09:00:00", "pnl_pct": -0.4, "pnl": -4.0},
    ]}
    out = ew.trade_samples_from_results(results)
    assert out == [
        {"entry_date": "2025-06-15 04:00:00", "pnl_pct": 1.25,
         "pnl_pct_net": 1.25},
        {"entry_date": "2025-06-20 09:00:00", "pnl_pct": -0.4,
         "pnl_pct_net": -0.4},
    ]
    assert ew.trade_samples_from_results({}) == []
    assert ew.trade_samples_from_results({"trades": None}) == []


class _FakeRegistry:
    STRATEGY_REGISTRY = {"alternator": {"default_params": {"period": 2},
                                        "description": "test"}}

    @staticmethod
    def list_strategies():
        return ["alternator"]

    @staticmethod
    def apply_strategy(name, df, params):
        out = df.copy()
        sig = np.zeros(len(out), dtype=int)
        sig[10::20] = 1
        sig[20::20] = -1
        out["signal"] = sig
        return out


def _synthetic_df(n=120):
    idx = pd.date_range("2026-01-01", periods=n, freq="1h")
    base = 100 + np.cumsum(np.sin(np.arange(n) / 5.0))
    return pd.DataFrame({
        "open": base, "high": base * 1.01, "low": base * 0.99,
        "close": base, "volume": np.full(n, 1000.0),
    }, index=idx)


def test_run_leg_keep_trades_attaches_samples(monkeypatch, subtests):
    with subtests.test("test_run_leg_keep_trades_attaches_samples"):
        with monkeypatch.context() as mp:
            df = _synthetic_df()
            import data_fetcher
            mp.setattr(data_fetcher, "load_cached_data",
                       lambda *a, **k: df, raising=True)
            leg = ew.run_leg(_FakeRegistry(), "alternator", None, "BTC/USDT", "1h",
                             ("2026-01-01", None), keep_trades=True)
            assert leg is not None and leg["trades"] > 0
            samples = leg["trade_samples"]
            assert len(samples) == leg["trades"]
            for s in samples:
                assert isinstance(s["entry_date"], str) and s["entry_date"]
                assert isinstance(s["pnl_pct"], float)
    with subtests.test("test_run_leg_default_omits_trade_samples"):
        with monkeypatch.context() as mp:
            df = _synthetic_df()
            import data_fetcher
            mp.setattr(data_fetcher, "load_cached_data",
                       lambda *a, **k: df, raising=True)
            leg = ew.run_leg(_FakeRegistry(), "alternator", None, "BTC/USDT", "1h",
                             ("2026-01-01", None))
            assert leg is not None
            assert "trade_samples" not in leg

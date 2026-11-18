
import sqlite3

import pytest

from shared_tools.conftest import load_module

_STORAGE = load_module("_storage_test", __file__.replace("test_storage.py", "storage.py"))
get_connection = _STORAGE.get_connection
init_db = _STORAGE.init_db
store_ohlcv = _STORAGE.store_ohlcv
load_ohlcv = _STORAGE.load_ohlcv
store_backtest_result = _STORAGE.store_backtest_result
get_backtest_results = _STORAGE.get_backtest_results


@pytest.fixture
def db_path(tmp_path):
    path = str(tmp_path / "test_trading.db")
    init_db(path)
    return path


def _sample_backtest_result():
    return {
        "strategy_name": "sma_crossover",
        "symbol": "BTC/USDT",
        "timeframe": "1h",
        "start_date": "2023-01-01",
        "end_date": "2023-12-31",
        "initial_capital": 10000.0,
        "final_capital": 12500.0,
        "total_return_pct": 25.0,
        "annual_return_pct": 25.0,
        "sharpe_ratio": 1.5,
        "sortino_ratio": 2.0,
        "max_drawdown_pct": 8.0,
        "win_rate": 0.55,
        "profit_factor": 1.8,
        "total_trades": 42,
        "params": {"fast_period": 10, "slow_period": 50},
        "trades": [{"side": "buy", "price": 35000, "qty": 0.1}],
    }


class TestBacktestRoundTrip:

    def test_filter_by_strategy_name(self, db_path):
        r1 = _sample_backtest_result()
        r2 = _sample_backtest_result()
        r2["strategy_name"] = "momentum"
        store_backtest_result(r1, db_path=db_path)
        store_backtest_result(r2, db_path=db_path)

        df = get_backtest_results(strategy_name="sma_crossover", db_path=db_path)
        assert len(df) == 1
        assert df["strategy_name"].iloc[0] == "sma_crossover"


class TestFundingCoverage:

    def _load(self, db_path, exchange="hyperliquid", coin="BTC"):
        from storage import load_funding_coverage
        return load_funding_coverage(exchange, coin, db_path=db_path)

    def test_stored_intervals_merge_only_when_they_overlap_or_touch(self, tmp_path, subtests):
        from storage import store_funding_coverage
        for label, intervals, expected in [
            ("empty_coverage", [], []),
            ("single_interval_roundtrip", [(100, 200)], [(100, 200)]),
            ("disjoint_intervals_stay_disjoint", [(1000, 2000), (100, 200)], [(100, 200), (1000, 2000)]),
            ("overlapping_intervals_merge", [(100, 200), (150, 300)], [(100, 300)]),
            ("touching_intervals_merge", [(100, 200), (200, 300)], [(100, 300)]),
            ("bridging_interval_collapses_neighbors", [(100, 200), (400, 500), (150, 450)], [(100, 500)]),
        ]:
            with subtests.test(case=label):
                path = str(tmp_path / f"{label}.db")
                init_db(path)
                for start, end in intervals:
                    store_funding_coverage("hyperliquid", "BTC", start, end, db_path=path)
                assert self._load(path) == expected

    def test_coins_and_exchanges_isolated(self, db_path):
        from storage import store_funding_coverage
        store_funding_coverage("hyperliquid", "BTC", 100, 200, db_path=db_path)
        store_funding_coverage("hyperliquid", "ETH", 300, 400, db_path=db_path)
        assert self._load(db_path, coin="BTC") == [(100, 200)]
        assert self._load(db_path, coin="ETH") == [(300, 400)]
        assert self._load(db_path, coin="SOL") == []

    def test_old_single_row_schema_migrates_and_drops_rows(self, tmp_path):
        path = str(tmp_path / "legacy.db")
        conn = sqlite3.connect(path)
        conn.execute("""
            CREATE TABLE funding_coverage (
                exchange TEXT NOT NULL,
                coin TEXT NOT NULL,
                start_ts INTEGER NOT NULL,
                end_ts INTEGER NOT NULL,
                UNIQUE(exchange, coin)
            )
        """)
        conn.execute("INSERT INTO funding_coverage VALUES ('hyperliquid', 'BTC', 100, 99999)")
        conn.commit()
        conn.close()

        init_db(path)
        assert self._load(path) == [], "poisoned min/max-union row must be dropped"
        from storage import store_funding_coverage
        store_funding_coverage("hyperliquid", "BTC", 100, 200, db_path=path)
        store_funding_coverage("hyperliquid", "BTC", 400, 500, db_path=path)
        assert self._load(path) == [(100, 200), (400, 500)]

    def test_new_schema_not_re_migrated(self, db_path):
        from storage import store_funding_coverage
        store_funding_coverage("hyperliquid", "BTC", 100, 200, db_path=db_path)
        init_db(db_path)
        assert self._load(db_path) == [(100, 200)]

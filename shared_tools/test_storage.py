"""Tests for storage.py — SQLite persistence for OHLCV and backtest results."""

import json
import os
import sqlite3

import pandas as pd
import pytest

# Import storage functions directly — we override DB_PATH per-test via db_path param
# Note: storage.py calls init_db() at import time using default DB_PATH.
# Tests always pass an explicit db_path to avoid touching the real database.
from storage import get_connection, init_db, store_ohlcv, load_ohlcv, store_backtest_result, get_backtest_results


# ─── Fixtures ──────────────────────────────────

@pytest.fixture
def db_path(tmp_path):
    """Create a temporary SQLite database."""
    path = str(tmp_path / "test_trading.db")
    init_db(path)
    return path


def _sample_ohlcv_df():
    """Create a sample OHLCV DataFrame."""
    return pd.DataFrame({
        "timestamp": [1700000000000, 1700003600000, 1700007200000],
        "open":      [35000.0, 35200.0, 35600.0],
        "high":      [35500.0, 35800.0, 36000.0],
        "low":       [34800.0, 35100.0, 35400.0],
        "close":     [35200.0, 35600.0, 35900.0],
        "volume":    [100.0, 120.0, 90.0],
    })


def _sample_backtest_result():
    """Create a sample backtest result dict."""
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


# ─── init_db ───────────────────────────────────

class TestInitDb:
    def test_creates_ohlcv_table(self, db_path):
        conn = sqlite3.connect(db_path)
        tables = conn.execute(
            "SELECT name FROM sqlite_master WHERE type='table'"
        ).fetchall()
        table_names = [t[0] for t in tables]
        assert "ohlcv" in table_names
        conn.close()

    def test_creates_backtest_results_table(self, db_path):
        conn = sqlite3.connect(db_path)
        tables = conn.execute(
            "SELECT name FROM sqlite_master WHERE type='table'"
        ).fetchall()
        table_names = [t[0] for t in tables]
        assert "backtest_results" in table_names
        conn.close()

    def test_idempotent(self, db_path):
        # Calling init_db twice should not raise
        init_db(db_path)
        init_db(db_path)

    def test_creates_index(self, db_path):
        conn = sqlite3.connect(db_path)
        indexes = conn.execute(
            "SELECT name FROM sqlite_master WHERE type='index'"
        ).fetchall()
        index_names = [i[0] for i in indexes]
        assert "idx_ohlcv_lookup" in index_names
        conn.close()


# ─── get_connection ────────────────────────────

class TestGetConnection:
    def test_returns_connection(self, db_path):
        conn = get_connection(db_path)
        assert isinstance(conn, sqlite3.Connection)
        conn.close()

    def test_wal_mode(self, db_path):
        conn = get_connection(db_path)
        mode = conn.execute("PRAGMA journal_mode").fetchone()[0]
        assert mode == "wal"
        conn.close()


# ─── store_ohlcv / load_ohlcv ─────────────────

class TestOhlcvRoundTrip:
    def test_store_and_load(self, db_path):
        df = _sample_ohlcv_df()
        store_ohlcv(df, "binanceus", "BTC/USDT", "1h", db_path=db_path)
        loaded = load_ohlcv("binanceus", "BTC/USDT", "1h", db_path=db_path)

        assert len(loaded) == 3
        assert list(loaded["timestamp"]) == [1700000000000, 1700003600000, 1700007200000]
        assert loaded["close"].iloc[0] == pytest.approx(35200.0)
        assert loaded["close"].iloc[2] == pytest.approx(35900.0)

    def test_datetime_index_set(self, db_path):
        df = _sample_ohlcv_df()
        store_ohlcv(df, "binanceus", "BTC/USDT", "1h", db_path=db_path)
        loaded = load_ohlcv("binanceus", "BTC/USDT", "1h", db_path=db_path)
        assert loaded.index.name == "datetime"

    def test_load_empty_when_no_data(self, db_path):
        loaded = load_ohlcv("binanceus", "ETH/USDT", "1d", db_path=db_path)
        assert len(loaded) == 0

    def test_upsert_on_duplicate_timestamp(self, db_path):
        df = _sample_ohlcv_df()
        store_ohlcv(df, "binanceus", "BTC/USDT", "1h", db_path=db_path)

        # Store again with updated close price for first candle
        df2 = pd.DataFrame({
            "timestamp": [1700000000000],
            "open": [35000.0],
            "high": [35500.0],
            "low": [34800.0],
            "close": [99999.0],  # changed
            "volume": [100.0],
        })
        store_ohlcv(df2, "binanceus", "BTC/USDT", "1h", db_path=db_path)

        loaded = load_ohlcv("binanceus", "BTC/USDT", "1h", db_path=db_path)
        assert len(loaded) == 3  # still 3 rows, not 4
        assert loaded["close"].iloc[0] == pytest.approx(99999.0)  # updated

    def test_filter_by_start_ts(self, db_path):
        df = _sample_ohlcv_df()
        store_ohlcv(df, "binanceus", "BTC/USDT", "1h", db_path=db_path)

        loaded = load_ohlcv("binanceus", "BTC/USDT", "1h",
                            start_ts=1700003600000, db_path=db_path)
        assert len(loaded) == 2

    def test_filter_by_end_ts(self, db_path):
        df = _sample_ohlcv_df()
        store_ohlcv(df, "binanceus", "BTC/USDT", "1h", db_path=db_path)

        loaded = load_ohlcv("binanceus", "BTC/USDT", "1h",
                            end_ts=1700003600000, db_path=db_path)
        assert len(loaded) == 2

    def test_filter_by_date_range(self, db_path):
        df = _sample_ohlcv_df()
        store_ohlcv(df, "binanceus", "BTC/USDT", "1h", db_path=db_path)

        loaded = load_ohlcv("binanceus", "BTC/USDT", "1h",
                            start_ts=1700003600000, end_ts=1700003600000,
                            db_path=db_path)
        assert len(loaded) == 1

    def test_different_exchanges_isolated(self, db_path):
        df = _sample_ohlcv_df()
        store_ohlcv(df, "binanceus", "BTC/USDT", "1h", db_path=db_path)
        store_ohlcv(df, "coinbase", "BTC/USDT", "1h", db_path=db_path)

        binance = load_ohlcv("binanceus", "BTC/USDT", "1h", db_path=db_path)
        coinbase = load_ohlcv("coinbase", "BTC/USDT", "1h", db_path=db_path)
        assert len(binance) == 3
        assert len(coinbase) == 3

    def test_different_symbols_isolated(self, db_path):
        df = _sample_ohlcv_df()
        store_ohlcv(df, "binanceus", "BTC/USDT", "1h", db_path=db_path)

        loaded = load_ohlcv("binanceus", "ETH/USDT", "1h", db_path=db_path)
        assert len(loaded) == 0

    def test_ordered_by_timestamp(self, db_path):
        # Insert in reverse order
        df = pd.DataFrame({
            "timestamp": [1700007200000, 1700000000000, 1700003600000],
            "open": [1.0, 2.0, 3.0],
            "high": [1.0, 2.0, 3.0],
            "low": [1.0, 2.0, 3.0],
            "close": [1.0, 2.0, 3.0],
            "volume": [1.0, 2.0, 3.0],
        })
        store_ohlcv(df, "binanceus", "BTC/USDT", "1h", db_path=db_path)
        loaded = load_ohlcv("binanceus", "BTC/USDT", "1h", db_path=db_path)
        timestamps = list(loaded["timestamp"])
        assert timestamps == sorted(timestamps)


# ─── store_backtest_result / get_backtest_results ──

class TestBacktestRoundTrip:
    def test_store_and_retrieve(self, db_path):
        result = _sample_backtest_result()
        store_backtest_result(result, db_path=db_path)

        df = get_backtest_results(db_path=db_path)
        assert len(df) == 1
        assert df["strategy_name"].iloc[0] == "sma_crossover"
        assert df["symbol"].iloc[0] == "BTC/USDT"
        assert df["initial_capital"].iloc[0] == pytest.approx(10000.0)
        assert df["final_capital"].iloc[0] == pytest.approx(12500.0)
        assert df["total_return_pct"].iloc[0] == pytest.approx(25.0)
        assert df["sharpe_ratio"].iloc[0] == pytest.approx(1.5)
        assert df["total_trades"].iloc[0] == 42

    def test_params_stored_as_json(self, db_path):
        result = _sample_backtest_result()
        store_backtest_result(result, db_path=db_path)

        df = get_backtest_results(db_path=db_path)
        params = json.loads(df["params"].iloc[0])
        assert params["fast_period"] == 10
        assert params["slow_period"] == 50

    def test_trades_stored_as_json(self, db_path):
        result = _sample_backtest_result()
        store_backtest_result(result, db_path=db_path)

        df = get_backtest_results(db_path=db_path)
        trades = json.loads(df["trades_json"].iloc[0])
        assert len(trades) == 1
        assert trades[0]["side"] == "buy"

    def test_filter_by_strategy_name(self, db_path):
        r1 = _sample_backtest_result()
        r2 = _sample_backtest_result()
        r2["strategy_name"] = "momentum"
        store_backtest_result(r1, db_path=db_path)
        store_backtest_result(r2, db_path=db_path)

        df = get_backtest_results(strategy_name="sma_crossover", db_path=db_path)
        assert len(df) == 1
        assert df["strategy_name"].iloc[0] == "sma_crossover"

    def test_all_results_when_no_filter(self, db_path):
        r1 = _sample_backtest_result()
        r2 = _sample_backtest_result()
        r2["strategy_name"] = "momentum"
        store_backtest_result(r1, db_path=db_path)
        store_backtest_result(r2, db_path=db_path)

        df = get_backtest_results(db_path=db_path)
        assert len(df) == 2

    def test_empty_results(self, db_path):
        df = get_backtest_results(db_path=db_path)
        assert len(df) == 0

    def test_missing_fields_use_defaults(self, db_path):
        # Minimal result dict
        result = {
            "strategy_name": "test",
            "symbol": "BTC/USDT",
            "timeframe": "1d",
            "start_date": "2023-01-01",
            "end_date": "2023-12-31",
        }
        store_backtest_result(result, db_path=db_path)

        df = get_backtest_results(db_path=db_path)
        assert len(df) == 1
        assert df["initial_capital"].iloc[0] == 0
        assert df["total_trades"].iloc[0] == 0

    def test_created_at_populated(self, db_path):
        result = _sample_backtest_result()
        store_backtest_result(result, db_path=db_path)

        df = get_backtest_results(db_path=db_path)
        assert df["created_at"].iloc[0] is not None
        assert len(df["created_at"].iloc[0]) > 0

    def test_multiple_results_ordered_desc(self, db_path):
        for i in range(3):
            r = _sample_backtest_result()
            r["strategy_name"] = f"strategy_{i}"
            store_backtest_result(r, db_path=db_path)

        df = get_backtest_results(db_path=db_path)
        assert len(df) == 3
        # Results ordered by created_at DESC (most recent first)
        # All created in same second, but insertion order preserved with ROWID
        assert df["strategy_name"].iloc[0] == "strategy_2"

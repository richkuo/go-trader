"""
SQLite storage layer for price data and backtest results.
"""

import sqlite3
import json
import os
from datetime import datetime
from typing import Optional

import pandas as pd

DB_PATH = os.path.join(os.path.dirname(__file__), "trading_bot.db")


def get_connection(db_path: str = DB_PATH) -> sqlite3.Connection:
    conn = sqlite3.connect(db_path)
    conn.execute("PRAGMA journal_mode=WAL")
    conn.execute("PRAGMA foreign_keys=ON")
    return conn


def init_db(db_path: str = DB_PATH):
    """Create tables if they don't exist."""
    conn = get_connection(db_path)
    conn.executescript("""
        CREATE TABLE IF NOT EXISTS ohlcv (
            id INTEGER PRIMARY KEY AUTOINCREMENT,
            exchange TEXT NOT NULL,
            symbol TEXT NOT NULL,
            timeframe TEXT NOT NULL,
            timestamp INTEGER NOT NULL,
            open REAL NOT NULL,
            high REAL NOT NULL,
            low REAL NOT NULL,
            close REAL NOT NULL,
            volume REAL NOT NULL,
            UNIQUE(exchange, symbol, timeframe, timestamp)
        );

        CREATE INDEX IF NOT EXISTS idx_ohlcv_lookup
            ON ohlcv(exchange, symbol, timeframe, timestamp);

        CREATE TABLE IF NOT EXISTS backtest_results (
            id INTEGER PRIMARY KEY AUTOINCREMENT,
            strategy_name TEXT NOT NULL,
            symbol TEXT NOT NULL,
            timeframe TEXT NOT NULL,
            start_date TEXT NOT NULL,
            end_date TEXT NOT NULL,
            initial_capital REAL NOT NULL,
            final_capital REAL NOT NULL,
            total_return_pct REAL,
            annual_return_pct REAL,
            sharpe_ratio REAL,
            sortino_ratio REAL,
            max_drawdown_pct REAL,
            win_rate REAL,
            profit_factor REAL,
            total_trades INTEGER,
            params TEXT,  -- JSON string of strategy parameters
            created_at TEXT DEFAULT (datetime('now')),
            trades_json TEXT  -- JSON string of all trades
        );
    """)
    conn.commit()
    conn.close()


def store_ohlcv(df: pd.DataFrame, exchange: str, symbol: str, timeframe: str,
                db_path: str = DB_PATH):
    """
    Store OHLCV dataframe. df must have columns: timestamp, open, high, low, close, volume.
    timestamp should be Unix ms.
    """
    conn = get_connection(db_path)
    rows = []
    for _, row in df.iterrows():
        rows.append((
            exchange, symbol, timeframe,
            int(row["timestamp"]),
            float(row["open"]), float(row["high"]),
            float(row["low"]), float(row["close"]),
            float(row["volume"])
        ))
    conn.executemany("""
        INSERT OR REPLACE INTO ohlcv
        (exchange, symbol, timeframe, timestamp, open, high, low, close, volume)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
    """, rows)
    conn.commit()
    conn.close()


def load_ohlcv(exchange: str, symbol: str, timeframe: str,
               start_ts: Optional[int] = None, end_ts: Optional[int] = None,
               db_path: str = DB_PATH) -> pd.DataFrame:
    """Load OHLCV data from DB into a DataFrame."""
    conn = get_connection(db_path)
    query = "SELECT timestamp, open, high, low, close, volume FROM ohlcv WHERE exchange=? AND symbol=? AND timeframe=?"
    params = [exchange, symbol, timeframe]

    if start_ts is not None:
        query += " AND timestamp >= ?"
        params.append(start_ts)
    if end_ts is not None:
        query += " AND timestamp <= ?"
        params.append(end_ts)

    query += " ORDER BY timestamp ASC"
    df = pd.read_sql_query(query, conn, params=params)
    conn.close()

    if not df.empty:
        df["datetime"] = pd.to_datetime(df["timestamp"], unit="ms")
        df.set_index("datetime", inplace=True)

    return df


def store_backtest_result(result: dict, db_path: str = DB_PATH):
    """Store a backtest result dict."""
    conn = get_connection(db_path)
    conn.execute("""
        INSERT INTO backtest_results
        (strategy_name, symbol, timeframe, start_date, end_date,
         initial_capital, final_capital, total_return_pct, annual_return_pct,
         sharpe_ratio, sortino_ratio, max_drawdown_pct, win_rate, profit_factor,
         total_trades, params, trades_json)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
    """, (
        result.get("strategy_name", ""),
        result.get("symbol", ""),
        result.get("timeframe", ""),
        result.get("start_date", ""),
        result.get("end_date", ""),
        result.get("initial_capital", 0),
        result.get("final_capital", 0),
        result.get("total_return_pct"),
        result.get("annual_return_pct"),
        result.get("sharpe_ratio"),
        result.get("sortino_ratio"),
        result.get("max_drawdown_pct"),
        result.get("win_rate"),
        result.get("profit_factor"),
        result.get("total_trades", 0),
        json.dumps(result.get("params", {})),
        json.dumps(result.get("trades", []))
    ))
    conn.commit()
    conn.close()


def get_backtest_results(strategy_name: Optional[str] = None,
                         db_path: str = DB_PATH) -> pd.DataFrame:
    """Retrieve backtest results."""
    conn = get_connection(db_path)
    query = "SELECT * FROM backtest_results"
    params = []
    if strategy_name:
        query += " WHERE strategy_name = ?"
        params.append(strategy_name)
    query += " ORDER BY created_at DESC"
    df = pd.read_sql_query(query, conn, params=params)
    conn.close()
    return df


# Initialize DB on import
init_db()

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

# Paths whose schema has already been ensured this process. Lets us create
# tables lazily on first real use instead of at import time — importing this
# module must stay side-effect free so it works under read-only sandboxes
# (e.g. systemd ProtectSystem=strict during the startup probe).
_SCHEMA_READY: set = set()


def _connect(db_path: str) -> sqlite3.Connection:
    conn = sqlite3.connect(db_path)
    conn.execute("PRAGMA journal_mode=WAL")
    conn.execute("PRAGMA foreign_keys=ON")
    return conn


def get_connection(db_path: str = DB_PATH) -> sqlite3.Connection:
    if db_path not in _SCHEMA_READY:
        init_db(db_path)
    return _connect(db_path)


def init_db(db_path: str = DB_PATH):
    """Create tables if they don't exist."""
    conn = _connect(db_path)
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

        CREATE TABLE IF NOT EXISTS funding_rates (
            id INTEGER PRIMARY KEY AUTOINCREMENT,
            exchange TEXT NOT NULL,
            coin TEXT NOT NULL,
            timestamp INTEGER NOT NULL,
            rate REAL NOT NULL,
            UNIQUE(exchange, coin, timestamp)
        );

        CREATE INDEX IF NOT EXISTS idx_funding_lookup
            ON funding_rates(exchange, coin, timestamp);

        CREATE TABLE IF NOT EXISTS funding_coverage (
            exchange TEXT NOT NULL,
            coin TEXT NOT NULL,
            start_ts INTEGER NOT NULL,
            end_ts INTEGER NOT NULL,
            UNIQUE(exchange, coin, start_ts)
        );

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
    _migrate_funding_coverage_to_intervals(conn)
    conn.commit()
    conn.close()
    _SCHEMA_READY.add(db_path)


def _migrate_funding_coverage_to_intervals(conn: sqlite3.Connection):
    """#1176: migrate a pre-interval funding_coverage table (one row per
    (exchange, coin), widened by min/max union) to the interval-set schema.
    Existing rows are DROPPED, not carried over: a min/max-unioned row may
    claim never-fetched middles as covered (that is exactly the bug — it
    manufactured the 2024 BTC funding hole), and there is no way to tell
    which parts of the range were actually fetched. Refetching is the only
    safe recovery; the rates themselves are untouched. Idempotent: the check
    keys off the old schema's UNIQUE(exchange, coin) constraint."""
    row = conn.execute(
        "SELECT sql FROM sqlite_master WHERE type='table' AND name='funding_coverage'"
    ).fetchone()
    if not row or not row[0]:
        return
    normalized = row[0].lower().replace(" ", "").replace("\n", "")
    if "unique(exchange,coin)" not in normalized:
        return  # already the interval schema (UNIQUE(exchange, coin, start_ts))
    conn.executescript("""
        DROP TABLE funding_coverage;
        CREATE TABLE funding_coverage (
            exchange TEXT NOT NULL,
            coin TEXT NOT NULL,
            start_ts INTEGER NOT NULL,
            end_ts INTEGER NOT NULL,
            UNIQUE(exchange, coin, start_ts)
        );
    """)


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


def store_funding_rates(records: list, exchange: str, coin: str,
                        db_path: str = DB_PATH):
    """Store funding-rate snapshots: list of {"rate": float, "time": int(ms)}."""
    if not records:
        return
    conn = get_connection(db_path)
    conn.executemany(
        "INSERT OR REPLACE INTO funding_rates (exchange, coin, timestamp, rate)"
        " VALUES (?, ?, ?, ?)",
        [(exchange, coin, int(r["time"]), float(r["rate"])) for r in records],
    )
    conn.commit()
    conn.close()


def load_funding_rates(exchange: str, coin: str,
                       start_ts: Optional[int] = None,
                       end_ts: Optional[int] = None,
                       db_path: str = DB_PATH) -> pd.DataFrame:
    """Load funding rates as a DataFrame(timestamp, rate) with a UTC
    DatetimeIndex, oldest first."""
    conn = get_connection(db_path)
    query = "SELECT timestamp, rate FROM funding_rates WHERE exchange=? AND coin=?"
    params = [exchange, coin]
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
        df["datetime"] = pd.to_datetime(df["timestamp"], unit="ms", utc=True)
        df.set_index("datetime", inplace=True)
    return df


def load_funding_coverage(exchange: str, coin: str,
                          db_path: str = DB_PATH) -> list:
    """Return the DISJOINT [start_ts, end_ts] intervals already fetched from
    the API for this coin, sorted ascending ([] when never fetched). Distinct
    from the stored rates themselves: a coin listed mid-range has rates
    starting later than the fetched-from point, and only the coverage
    intervals prove nothing earlier exists to fetch."""
    conn = get_connection(db_path)
    rows = conn.execute(
        "SELECT start_ts, end_ts FROM funding_coverage WHERE exchange=? AND coin=?"
        " ORDER BY start_ts ASC",
        (exchange, coin),
    ).fetchall()
    conn.close()
    return [(int(s), int(e)) for s, e in rows]


def store_funding_coverage(exchange: str, coin: str,
                           start_ts: int, end_ts: int,
                           db_path: str = DB_PATH):
    """Record that [start_ts, end_ts] has been fetched. Coverage is a set of
    DISJOINT intervals: the new range merges only with intervals it overlaps
    or touches — NEVER min/max across disjoint fetches (#1176: that unioned an
    early historical fetch and a recent fetch into one row that falsely
    claimed the never-fetched middle as covered)."""
    intervals = load_funding_coverage(exchange, coin, db_path=db_path)
    intervals.append((int(start_ts), int(end_ts)))
    intervals.sort()
    merged = []
    for s, e in intervals:
        if merged and s <= merged[-1][1]:
            merged[-1][1] = max(merged[-1][1], e)
        else:
            merged.append([s, e])
    conn = get_connection(db_path)
    conn.execute("DELETE FROM funding_coverage WHERE exchange=? AND coin=?",
                 (exchange, coin))
    conn.executemany(
        "INSERT INTO funding_coverage (exchange, coin, start_ts, end_ts)"
        " VALUES (?, ?, ?, ?)",
        [(exchange, coin, s, e) for s, e in merged],
    )
    conn.commit()
    conn.close()


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
    query += " ORDER BY created_at DESC, id DESC"
    df = pd.read_sql_query(query, conn, params=params)
    conn.close()
    return df

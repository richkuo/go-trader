import sqlite3
import json
import os
from datetime import datetime
from typing import Optional
import pandas as pd
OHLCV_CACHE_DB_ENV = 'GO_TRADER_OHLCV_CACHE_DB'

def _resolve_default_db_path() -> str:
    configured = os.environ.get(OHLCV_CACHE_DB_ENV)
    if configured is None:
        return os.path.join(os.path.dirname(__file__), 'trading_bot.db')
    configured = configured.strip()
    if not configured:
        raise RuntimeError(f'{OHLCV_CACHE_DB_ENV} must not be blank')
    return os.path.abspath(os.path.expanduser(configured))
DB_PATH = _resolve_default_db_path()
_SCHEMA_READY: set = set()

def _connect(db_path: str) -> sqlite3.Connection:
    try:
        conn = sqlite3.connect(db_path)
    except sqlite3.Error as exc:
        raise sqlite3.OperationalError(f'cannot open SQLite cache {db_path}: {exc}') from exc
    conn.execute('PRAGMA journal_mode=WAL')
    conn.execute('PRAGMA foreign_keys=ON')
    return conn

def get_connection(db_path: str=DB_PATH) -> sqlite3.Connection:
    if db_path not in _SCHEMA_READY:
        init_db(db_path)
    return _connect(db_path)

def init_db(db_path: str=DB_PATH):
    conn = _connect(db_path)
    conn.executescript("\n        CREATE TABLE IF NOT EXISTS ohlcv (\n            id INTEGER PRIMARY KEY AUTOINCREMENT,\n            exchange TEXT NOT NULL,\n            symbol TEXT NOT NULL,\n            timeframe TEXT NOT NULL,\n            timestamp INTEGER NOT NULL,\n            open REAL NOT NULL,\n            high REAL NOT NULL,\n            low REAL NOT NULL,\n            close REAL NOT NULL,\n            volume REAL NOT NULL,\n            UNIQUE(exchange, symbol, timeframe, timestamp)\n        );\n\n        CREATE INDEX IF NOT EXISTS idx_ohlcv_lookup\n            ON ohlcv(exchange, symbol, timeframe, timestamp);\n\n        CREATE TABLE IF NOT EXISTS funding_rates (\n            id INTEGER PRIMARY KEY AUTOINCREMENT,\n            exchange TEXT NOT NULL,\n            coin TEXT NOT NULL,\n            timestamp INTEGER NOT NULL,\n            rate REAL NOT NULL,\n            UNIQUE(exchange, coin, timestamp)\n        );\n\n        CREATE INDEX IF NOT EXISTS idx_funding_lookup\n            ON funding_rates(exchange, coin, timestamp);\n\n        CREATE TABLE IF NOT EXISTS funding_coverage (\n            exchange TEXT NOT NULL,\n            coin TEXT NOT NULL,\n            start_ts INTEGER NOT NULL,\n            end_ts INTEGER NOT NULL,\n            UNIQUE(exchange, coin, start_ts)\n        );\n\n        CREATE TABLE IF NOT EXISTS backtest_results (\n            id INTEGER PRIMARY KEY AUTOINCREMENT,\n            strategy_name TEXT NOT NULL,\n            symbol TEXT NOT NULL,\n            timeframe TEXT NOT NULL,\n            start_date TEXT NOT NULL,\n            end_date TEXT NOT NULL,\n            initial_capital REAL NOT NULL,\n            final_capital REAL NOT NULL,\n            total_return_pct REAL,\n            annual_return_pct REAL,\n            sharpe_ratio REAL,\n            sortino_ratio REAL,\n            max_drawdown_pct REAL,\n            win_rate REAL,\n            profit_factor REAL,\n            total_trades INTEGER,\n            params TEXT,  -- JSON string of strategy parameters\n            created_at TEXT DEFAULT (datetime('now')),\n            trades_json TEXT  -- JSON string of all trades\n        );\n    ")
    _migrate_funding_coverage_to_intervals(conn)
    conn.commit()
    conn.close()
    _SCHEMA_READY.add(db_path)

def _migrate_funding_coverage_to_intervals(conn: sqlite3.Connection):
    row = conn.execute("SELECT sql FROM sqlite_master WHERE type='table' AND name='funding_coverage'").fetchone()
    if not row or not row[0]:
        return
    normalized = row[0].lower().replace(' ', '').replace('\n', '')
    if 'unique(exchange,coin)' not in normalized:
        return
    conn.executescript('\n        DROP TABLE funding_coverage;\n        CREATE TABLE funding_coverage (\n            exchange TEXT NOT NULL,\n            coin TEXT NOT NULL,\n            start_ts INTEGER NOT NULL,\n            end_ts INTEGER NOT NULL,\n            UNIQUE(exchange, coin, start_ts)\n        );\n    ')

def store_ohlcv(df: pd.DataFrame, exchange: str, symbol: str, timeframe: str, db_path: str=DB_PATH):
    conn = get_connection(db_path)
    rows = []
    for _, row in df.iterrows():
        rows.append((exchange, symbol, timeframe, int(row['timestamp']), float(row['open']), float(row['high']), float(row['low']), float(row['close']), float(row['volume'])))
    conn.executemany('\n        INSERT OR REPLACE INTO ohlcv\n        (exchange, symbol, timeframe, timestamp, open, high, low, close, volume)\n        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)\n    ', rows)
    conn.commit()
    conn.close()

def load_ohlcv(exchange: str, symbol: str, timeframe: str, start_ts: Optional[int]=None, end_ts: Optional[int]=None, db_path: str=DB_PATH) -> pd.DataFrame:
    conn = get_connection(db_path)
    query = 'SELECT timestamp, open, high, low, close, volume FROM ohlcv WHERE exchange=? AND symbol=? AND timeframe=?'
    params = [exchange, symbol, timeframe]
    if start_ts is not None:
        query += ' AND timestamp >= ?'
        params.append(start_ts)
    if end_ts is not None:
        query += ' AND timestamp <= ?'
        params.append(end_ts)
    query += ' ORDER BY timestamp ASC'
    df = pd.read_sql_query(query, conn, params=params)
    conn.close()
    if not df.empty:
        df['datetime'] = pd.to_datetime(df['timestamp'], unit='ms')
        df.set_index('datetime', inplace=True)
    return df

def store_funding_rates(records: list, exchange: str, coin: str, db_path: str=DB_PATH):
    if not records:
        return
    conn = get_connection(db_path)
    conn.executemany('INSERT OR REPLACE INTO funding_rates (exchange, coin, timestamp, rate) VALUES (?, ?, ?, ?)', [(exchange, coin, int(r['time']), float(r['rate'])) for r in records])
    conn.commit()
    conn.close()

def load_funding_rates(exchange: str, coin: str, start_ts: Optional[int]=None, end_ts: Optional[int]=None, db_path: str=DB_PATH) -> pd.DataFrame:
    conn = get_connection(db_path)
    query = 'SELECT timestamp, rate FROM funding_rates WHERE exchange=? AND coin=?'
    params = [exchange, coin]
    if start_ts is not None:
        query += ' AND timestamp >= ?'
        params.append(start_ts)
    if end_ts is not None:
        query += ' AND timestamp <= ?'
        params.append(end_ts)
    query += ' ORDER BY timestamp ASC'
    df = pd.read_sql_query(query, conn, params=params)
    conn.close()
    if not df.empty:
        df['datetime'] = pd.to_datetime(df['timestamp'], unit='ms', utc=True)
        df.set_index('datetime', inplace=True)
    return df

def load_funding_coverage(exchange: str, coin: str, db_path: str=DB_PATH) -> list:
    conn = get_connection(db_path)
    rows = conn.execute('SELECT start_ts, end_ts FROM funding_coverage WHERE exchange=? AND coin=? ORDER BY start_ts ASC', (exchange, coin)).fetchall()
    conn.close()
    return [(int(s), int(e)) for s, e in rows]

def store_funding_coverage(exchange: str, coin: str, start_ts: int, end_ts: int, db_path: str=DB_PATH):
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
    conn.execute('DELETE FROM funding_coverage WHERE exchange=? AND coin=?', (exchange, coin))
    conn.executemany('INSERT INTO funding_coverage (exchange, coin, start_ts, end_ts) VALUES (?, ?, ?, ?)', [(exchange, coin, s, e) for s, e in merged])
    conn.commit()
    conn.close()

def store_backtest_result(result: dict, db_path: str=DB_PATH):
    conn = get_connection(db_path)
    conn.execute('\n        INSERT INTO backtest_results\n        (strategy_name, symbol, timeframe, start_date, end_date,\n         initial_capital, final_capital, total_return_pct, annual_return_pct,\n         sharpe_ratio, sortino_ratio, max_drawdown_pct, win_rate, profit_factor,\n         total_trades, params, trades_json)\n        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)\n    ', (result.get('strategy_name', ''), result.get('symbol', ''), result.get('timeframe', ''), result.get('start_date', ''), result.get('end_date', ''), result.get('initial_capital', 0), result.get('final_capital', 0), result.get('total_return_pct'), result.get('annual_return_pct'), result.get('sharpe_ratio'), result.get('sortino_ratio'), result.get('max_drawdown_pct'), result.get('win_rate'), result.get('profit_factor'), result.get('total_trades', 0), json.dumps(result.get('params', {})), json.dumps(result.get('trades', []))))
    conn.commit()
    conn.close()

def get_backtest_results(strategy_name: Optional[str]=None, db_path: str=DB_PATH) -> pd.DataFrame:
    conn = get_connection(db_path)
    query = 'SELECT * FROM backtest_results'
    params = []
    if strategy_name:
        query += ' WHERE strategy_name = ?'
        params.append(strategy_name)
    query += ' ORDER BY created_at DESC, id DESC'
    df = pd.read_sql_query(query, conn, params=params)
    conn.close()
    return df

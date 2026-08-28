
import time
from typing import Optional
from datetime import datetime

import ccxt
import pandas as pd

from storage import store_ohlcv, load_ohlcv


def get_exchange(exchange_id: str = "binanceus") -> ccxt.Exchange:
    exchange_class = getattr(ccxt, exchange_id)
    exchange = exchange_class({
        "enableRateLimit": True,
    })
    return exchange


def fetch_ohlcv(
    symbol: str = "BTC/USDT",
    timeframe: str = "1d",
    since: Optional[str] = None,
    limit: int = 500,
    exchange_id: str = "binanceus",
    store: bool = True,
) -> pd.DataFrame:
    exchange = get_exchange(exchange_id)

    since_ts = None
    if since:
        since_ts = exchange.parse8601(since + "T00:00:00Z")

    raw = exchange.fetch_ohlcv(symbol, timeframe, since=since_ts, limit=limit)

    if not raw:
        return pd.DataFrame(columns=["timestamp", "open", "high", "low", "close", "volume"])

    df = pd.DataFrame(raw, columns=["timestamp", "open", "high", "low", "close", "volume"])

    if store:
        store_ohlcv(df, exchange_id, symbol, timeframe)

    df["datetime"] = pd.to_datetime(df["timestamp"], unit="ms")
    df.set_index("datetime", inplace=True)

    return df


def fetch_full_history(
    symbol: str = "BTC/USDT",
    timeframe: str = "1d",
    since: str = "2020-01-01",
    exchange_id: str = "binanceus",
    store: bool = True,
    limit: int = 500,
) -> pd.DataFrame:
    exchange = get_exchange(exchange_id)
    since_ts = exchange.parse8601(since + "T00:00:00Z")
    now_ts = exchange.milliseconds()

    all_candles = []
    current_since = since_ts

    tf_ms = {
        "1m": 60_000, "5m": 300_000, "15m": 900_000,
        "1h": 3_600_000, "4h": 14_400_000,
        "1d": 86_400_000, "1w": 604_800_000,
    }

    print(f"Fetching {symbol} {timeframe} from {since}...")

    rate_limit_retries = 0
    network_retries = 0
    while current_since < now_ts:
        try:
            candles = exchange.fetch_ohlcv(symbol, timeframe, since=current_since,
                                           limit=limit)
            rate_limit_retries = 0
            network_retries = 0
        except ccxt.RateLimitExceeded:
            rate_limit_retries += 1
            if rate_limit_retries >= 5:
                print(f"Rate limit exceeded {rate_limit_retries} times, aborting fetch")
                break
            print(f"Rate limited, sleeping 10s... ({rate_limit_retries}/5)")
            time.sleep(10)
            continue
        except ccxt.NetworkError as e:
            network_retries += 1
            if network_retries >= 5:
                print(f"Network error after {network_retries} retries, aborting fetch: {e}")
                break
            print(f"Network error: {e}, retrying in 5s... ({network_retries}/5)")
            time.sleep(5)
            continue

        if not candles:
            break

        all_candles.extend(candles)

        last_ts = candles[-1][0]
        if last_ts == current_since:
            break
        current_since = last_ts + tf_ms.get(timeframe, 86_400_000)

        time.sleep(exchange.rateLimit / 1000)

    if not all_candles:
        return pd.DataFrame(columns=["timestamp", "open", "high", "low", "close", "volume"])

    df = pd.DataFrame(all_candles, columns=["timestamp", "open", "high", "low", "close", "volume"])
    df.drop_duplicates(subset=["timestamp"], inplace=True)
    df.sort_values("timestamp", inplace=True)
    df.reset_index(drop=True, inplace=True)

    print(f"Fetched {len(df)} candles from {pd.to_datetime(df['timestamp'].iloc[0], unit='ms')} "
          f"to {pd.to_datetime(df['timestamp'].iloc[-1], unit='ms')}")

    if store:
        store_ohlcv(df, exchange_id, symbol, timeframe)

    df["datetime"] = pd.to_datetime(df["timestamp"], unit="ms")
    df.set_index("datetime", inplace=True)

    return df


def load_cached_data(
    symbol: str = "BTC/USDT",
    timeframe: str = "1d",
    exchange_id: str = "binanceus",
    start_date: Optional[str] = None,
    end_date: Optional[str] = None,
) -> pd.DataFrame:
    start_ts = None
    end_ts = None
    if start_date:
        start_ts = int(pd.Timestamp(start_date).timestamp() * 1000)
    if end_date:
        end_ts = int(pd.Timestamp(end_date).timestamp() * 1000)

    df = load_ohlcv(exchange_id, symbol, timeframe, start_ts, end_ts)

    if df.empty:
        print(f"No cached data for {symbol} {timeframe}, fetching from exchange...")
        since = start_date or "2020-01-01"
        df = fetch_full_history(symbol, timeframe, since, exchange_id, store=True)

    return df


if __name__ == "__main__":
    df = fetch_ohlcv("BTC/USDT", "1d", limit=30)
    print(f"\nFetched {len(df)} candles:")
    print(df.tail())

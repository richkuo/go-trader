import os
import sys
from typing import Optional

import pandas as pd

_HERE = os.path.dirname(os.path.abspath(__file__))
_TOOLS = os.path.abspath(os.path.join(_HERE, "..", "shared_tools"))
if _TOOLS not in sys.path:
    sys.path.insert(0, _TOOLS)

from closed_bar import TIMESTAMP_KIND_OPEN, declare_frame_cadence
from lead_lag_inputs import LEADER_COIN, coin_of, prepare_historical_inputs

LEADER_INPUT_STRATEGIES = frozenset({"bitcoin_lead_lag"})
CONTEXT_PARAM = "leader_context"


class LeaderDataUnavailable(ValueError):
    pass


def refuse_leader_input_strategy(name: str, consumer: str) -> None:
    if str(name or "").strip() in LEADER_INPUT_STRATEGIES:
        raise ValueError(
            f"{name} reads a pinned BTC leader series as an entry input; {consumer} does not attach it. "
            "Run it with run_backtest.py --mode single or eval_windows.py (cached or --manifest), which attach "
            "the leader with the same warm-up and closed-bar alignment")


def leader_symbol(follower_symbol: str) -> str:
    text = str(follower_symbol or "")
    if "/" in text:
        return f"{LEADER_COIN}/{text.split('/', 1)[1]}"
    return LEADER_COIN


def cached_leader_frame(follower_symbol: str, timeframe: str, start, end=None,
                        exchange_id: str = "binanceus") -> pd.DataFrame:
    from storage import load_ohlcv
    symbol = leader_symbol(follower_symbol)
    start_ts = int(pd.Timestamp(start).timestamp() * 1000) if start is not None else None
    end_ts = int(pd.Timestamp(end).timestamp() * 1000) if end is not None else None
    df = load_ohlcv(exchange_id, symbol, timeframe, start_ts, end_ts)
    if df.empty:
        raise LeaderDataUnavailable(
            f"the candle cache has no {exchange_id} {symbol} {timeframe} leader rows from {start} to {end}; "
            "acquire them explicitly before the run (this path never fetches or substitutes current data)")
    if end is not None:
        df = df[df.index < pd.Timestamp(end)]
    return declare_frame_cadence(df, timeframe, TIMESTAMP_KIND_OPEN)


def manifest_leader_frame(manifest: dict, window_name: str):
    import offline_manifest as om
    dataset = om.dataset_by_key(manifest, f"{LEADER_COIN} {manifest['interval']}")
    frame, window, coverage = om.window_frame(manifest, dataset, window_name)
    return frame, dataset, coverage


def leader_context(follower_df: pd.DataFrame, leader_df: pd.DataFrame, follower_symbol: str, timeframe: str,
                   source: Optional[dict] = None) -> dict:
    return prepare_historical_inputs(follower_df, leader_df, follower_symbol, timeframe, source=source)


def manifest_leader_params(manifest: dict, window_name: str, follower_df: pd.DataFrame, follower_symbol: str):
    frame, dataset, coverage = manifest_leader_frame(manifest, window_name)
    source = {
        "path": "manifest",
        "manifest_study": manifest["study"],
        "provenance": dict(manifest["provenance"]),
        "dataset": dataset["key"],
        "candles_sha256": dataset["candles"]["sha256"],
        "window": window_name,
        "candle_coverage": coverage,
    }
    ctx = leader_context(follower_df, frame, follower_symbol, manifest["interval"], source)
    return {CONTEXT_PARAM: ctx}, leader_summary(ctx)


def cached_leader_params(follower_df: pd.DataFrame, follower_symbol: str, timeframe: str, start, end=None,
                         exchange_id: str = "binanceus"):
    if coin_of(follower_symbol) == LEADER_COIN:
        ctx = leader_context(follower_df, follower_df, follower_symbol, timeframe, {"path": "cache"})
        return {CONTEXT_PARAM: ctx}, leader_summary(ctx)
    frame = cached_leader_frame(follower_symbol, timeframe, start, end, exchange_id)
    source = {"path": "cache", "exchange": exchange_id, "symbol": leader_symbol(follower_symbol),
              "timeframe": timeframe, "start": str(start), "end": str(end),
              "first_bar": str(frame.index[0]), "last_bar": str(frame.index[-1]), "rows": int(len(frame))}
    ctx = leader_context(follower_df, frame, follower_symbol, timeframe, source)
    return {CONTEXT_PARAM: ctx}, leader_summary(ctx)


def leader_summary(ctx: dict) -> dict:
    keys = ("available", "hold_reason", "detail", "basis", "leader", "follower", "timeframe", "coverage",
            "leader_sha256", "follower_sha256", "source")
    return {k: ctx.get(k) for k in keys if k in ctx}

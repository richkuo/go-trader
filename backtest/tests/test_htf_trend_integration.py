import json
import os
import subprocess
import sys


_ROOT = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", ".."))

_SCRIPT = """
import json, os, socket, sys
root = sys.argv[1]
sys.path.insert(0, os.path.join(root, "shared_tools"))
sys.path.insert(0, os.path.join(root, "backtest"))
import numpy as np
import pandas as pd
from storage import store_ohlcv

calls = []
def refuse(*a, **k):
    calls.append(1)
    raise RuntimeError("network blocked in this test")
socket.socket.connect = refuse
socket.create_connection = refuse
import data_fetcher
data_fetcher.fetch_full_history = refuse
data_fetcher.fetch_ohlcv = refuse
data_fetcher.get_exchange = refuse
import run_backtest
run_backtest.get_exchange = refuse

step = 1_800_000
start = int(pd.Timestamp("2024-01-01").timestamp() * 1000)
n = 6000
rng = np.random.default_rng(7)
close = 100 * np.exp(np.cumsum(rng.normal(0, 0.006, n) + 0.0004 * np.sin(np.arange(n) / 300.0)))
ts = start + step * np.arange(n)
ltf = pd.DataFrame({"timestamp": ts, "open": np.r_[close[0], close[:-1]], "high": close * 1.004,
                    "low": close * 0.996, "close": close, "volume": 1.0})
store_ohlcv(ltf, "binanceus", "ETH/USDT", "30m")
b = ltf.copy()
b["bucket"] = (b["timestamp"] // 14_400_000) * 14_400_000
htf = b.groupby("bucket").agg(open=("open", "first"), high=("high", "max"), low=("low", "min"),
                              close=("close", "last"), volume=("volume", "sum")).reset_index()
htf = htf.rename(columns={"bucket": "timestamp"})
store_ohlcv(htf, "binanceus", "ETH/USDT", "4h")

from data_fetcher import load_cached_data
from htf_trend import frozen_htf_trend
frame = load_cached_data("ETH/USDT", "30m")
existing = run_backtest._htf_trend_series("ETH/USDT", "30m", frame.index)
derived, cov, _ = frozen_htf_trend(frame, "30m")

def last_bucket_sign(f):
    _, _, h = frozen_htf_trend(f, "30m")
    c = h["close"].astype(float)
    e = c.ewm(span=HTF_EMA_PERIOD, adjust=False).mean()
    return int(np.sign(c.iloc[-1] - e.iloc[-1])), float(c.iloc[-1])

from htf_filter import HTF_EMA_PERIOD
sign_before, last_close_before = last_bucket_sign(frame)
changed = frame.copy()
changed.iloc[-3:, changed.columns.get_loc("close")] *= (0.5 if sign_before > 0 else 1.5)
derived_changed, _, _ = frozen_htf_trend(changed, "30m")
sign_after, last_close_after = last_bucket_sign(changed)

from registry_loader import load_registry
import eval_windows as ew
reg = load_registry("futures")
window = ("2024-03-15", "2024-05-01")
common = dict(close_strategies=[{"name": "trailing_tp_ratchet", "params": {}}], direction="long",
              trailing_stop_atr_mult=2.5, warmup_bars=1500, funding_mode="off", keep_trades=True)
plain = ew.run_leg(reg, "tema_cross", None, "ETH/USDT", "30m", window, atr_method="wilder", **common)
filt = ew.run_leg(reg, "tema_cross", None, "ETH/USDT", "30m", window, atr_method="wilder",
                  htf_filter=True, **common)
refused = None
try:
    ew.run_leg(reg, "tema_cross", None, "ETH/USDT", "30m", ("2024-01-01", "2024-01-20"), htf_filter=True,
               **{**common, "warmup_bars": 0})
except ValueError as exc:
    refused = str(exc)
print(json.dumps({
    "equal": bool((existing.to_numpy() == derived.to_numpy()).all()),
    "nonzero": int((derived != 0).sum()),
    "rows": int(len(derived)),
    "series_unchanged": bool((derived.to_numpy() == derived_changed.to_numpy()).all()),
    "last_close_changed": last_close_after != last_close_before,
    "last_sign_flipped": sign_before != 0 and sign_after == -sign_before,
    "dropped": cov["buckets_dropped_unclosed"],
    "plain_trades": plain["trades"], "filt_trades": filt["trades"],
    "htf": filt["htf_filter"], "atr_method": filt.get("atr_method"),
    "refused": refused, "network_calls": len(calls),
}))
"""


def _run(tmp_path):
    script = tmp_path / "htf_trend_run.py"
    script.write_text(_SCRIPT)
    env = dict(os.environ)
    env["GO_TRADER_OHLCV_CACHE_DB"] = str(tmp_path / "cache.db")
    env.pop("HYPERLIQUID_SECRET_KEY", None)
    proc = subprocess.run([sys.executable, str(script), _ROOT], env=env, cwd=str(tmp_path),
                          capture_output=True, text=True, timeout=600)
    assert proc.returncode == 0, proc.stderr[-4000:]
    return json.loads(proc.stdout.strip().splitlines()[-1])


def test_frozen_htf_trend_matches_cache_path_and_filters_a_real_leg_offline(tmp_path):
    out = _run(tmp_path)
    assert out["equal"] is True
    assert out["nonzero"] > out["rows"] // 2
    assert out["last_close_changed"] is True
    assert out["last_sign_flipped"] is True
    assert out["series_unchanged"] is True
    assert out["dropped"] == 0
    assert out["network_calls"] == 0
    htf = out["htf"]
    assert htf["htf_timeframe"] == "4h"
    assert htf["closed_buckets_before_first_scored_bar"] >= 50
    assert htf["long_signals"] == htf["long_passed"] + htf["long_blocked"]
    assert htf["long_blocked"] > 0
    assert out["filt_trades"] <= out["plain_trades"]
    assert out["atr_method"] == "wilder"
    assert out["refused"] is not None and "closed 4h buckets precede" in out["refused"]

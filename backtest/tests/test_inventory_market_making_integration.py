import json
import os
import subprocess
import sys

import numpy as np
import pandas as pd
import pytest

_BACKTEST = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
_ROOT = os.path.dirname(_BACKTEST)
for _p in (_BACKTEST, os.path.join(_ROOT, "shared_strategies", "open")):
    if _p not in sys.path:
        sys.path.insert(0, _p)

import optimizer
import quote_replay
import run_backtest
from registry_loader import load_registry

NAME = "inventory_market_making"
CANDIDATE = os.path.join(_BACKTEST, "candidates", "inventory_market_making_1636")


def _frame(n=400):
    idx = pd.date_range("2026-01-01", periods=n, freq="1min", tz="UTC")
    close = 100000 + np.cumsum(np.sin(np.arange(n) / 7.0))
    return pd.DataFrame({"open": close, "high": close + 5, "low": close - 5, "close": close, "volume": 1.0}, index=idx)


def test_registration_is_a_hidden_futures_quote_capability_with_the_committed_specification():
    reg = load_registry("futures")
    entry = reg.STRATEGY_REGISTRY[NAME]
    assert entry["production_refused"] is True and entry["edge_status"] == "no_edge"
    assert entry["edge_source"] == "study_inconclusive"
    assert entry["edge_ref"] == "backtest/candidates/inventory_market_making_1636/REPORT.md"
    assert entry["quote"] == {"input": "deadline_book_evidence", "output": "two_sided_quotes"}
    assert NAME not in reg.list_strategies()
    assert NAME not in load_registry("spot").STRATEGY_REGISTRY
    registry = reg._registry
    assert registry.STRATEGIES[NAME]["short_entries"] is False
    assert NAME not in registry.basket_rankers()
    with open(os.path.join(CANDIDATE, "discovery_quote.json")) as fh:
        committed = json.load(fh)
    assert quote_replay.discover() == committed
    assert committed["capabilities"][NAME]["spec_sha256"] == committed["spec_sha256"]


def test_candle_backtest_walk_forward_optimizer_and_parity_refuse_before_any_market_access(monkeypatch):
    def no_data(*_a, **_k):
        raise AssertionError("market data was requested for a quote capability")

    monkeypatch.setattr(run_backtest, "load_cached_data", no_data, raising=False)
    with pytest.raises(ValueError, match="quote capability"):
        run_backtest.run_single_backtest(strategy_name=NAME, symbol="BTC", timeframe="1m", registry="futures", platform="hyperliquid")
    with pytest.raises(ValueError, match="quote capability"):
        run_backtest.run_walk_forward(strategy_name=NAME, symbol="BTC", timeframe="1m", registry="futures", platform="hyperliquid")
    with pytest.raises(ValueError, match="quote capability"):
        optimizer.walk_forward_optimize(_frame(), NAME, {}, registry="futures", platform="hyperliquid", verbose=False)
    reg = load_registry("futures")
    with pytest.raises(ValueError, match="quote capability"):
        reg.STRATEGY_REGISTRY[NAME]["fn"](_frame())


def test_the_solo_check_script_refuses_the_candidate_before_any_fetch():
    env = {k: v for k, v in os.environ.items() if not k.upper().startswith(("HYPERLIQUID_", "OKX_"))}
    proc = subprocess.run([sys.executable, os.path.join(_ROOT, "shared_scripts", "check_hyperliquid.py"), NAME, "BTC", "1m", "--mode=paper"],
                          capture_output=True, text=True, cwd=_ROOT, env=env, timeout=120)
    assert proc.returncode == 1, proc.stderr[-1000:]
    payload = json.loads(proc.stdout.strip().splitlines()[-1])
    assert f"open strategy '{NAME}' is production_refused" in payload["error"]
    assert "Fetching" not in proc.stderr


def test_the_committed_report_is_what_the_frozen_invocation_writes_without_a_pinned_recording(tmp_path):
    with open(os.path.join(CANDIDATE, "data-manifest.json")) as fh:
        manifest = json.load(fh)
    assert manifest["recordings"] == [] and manifest["funding"] is None
    out = tmp_path / "study"
    proc = subprocess.run([sys.executable, os.path.join(CANDIDATE, "run_study.py"), "--protocol", os.path.join(CANDIDATE, "protocol-lock.json"),
                           "--out-dir", str(out)], capture_output=True, text=True, cwd=_ROOT, timeout=300)
    assert proc.returncode == 0, proc.stderr[-2000:]
    got = json.loads((out / "results.json").read_text())
    with open(os.path.join(CANDIDATE, "results.json")) as fh:
        committed = json.load(fh)
    assert got["verdict"] == committed["verdict"] == "INCONCLUSIVE"
    assert got["edge_source"] == "study_inconclusive"
    assert got["protocol_sha256"] == committed["protocol_sha256"]
    assert got["audit"]["sources"] == committed["audit"]["sources"]
    want = [r for r in committed["missing_inputs"] if "binary" not in r]
    assert [r for r in got["missing_inputs"] if "binary" not in r] == want
    assert any("no go-trader binary was supplied" in r for r in got["missing_inputs"])

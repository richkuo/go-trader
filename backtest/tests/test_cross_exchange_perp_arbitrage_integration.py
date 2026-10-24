import json
import os
import subprocess
import sys

import pytest

_BACKTEST = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
_ROOT = os.path.dirname(_BACKTEST)
if _BACKTEST not in sys.path:
    sys.path.insert(0, _BACKTEST)

import optimizer
import parity_diff
import run_backtest

import data_fetcher
import offline_manifest

NAME = "cross_exchange_perp_arbitrage"
REFUSAL = "is a pair policy"


class _UnreadableFrame:
    def __init__(self, touched):
        self._touched = touched

    def __len__(self):
        self._touched.append("len")
        raise AssertionError("the frame length was read before the pair refusal")

    def __getattr__(self, name):
        self._touched.append(name)
        raise AssertionError(f"the frame attribute {name!r} was read before the pair refusal")

    def __getitem__(self, key):
        self._touched.append(key)
        raise AssertionError("a frame column was read before the pair refusal")


@pytest.fixture
def data_calls(monkeypatch):
    calls = []

    def record(label):
        def fn(*args, **kwargs):
            calls.append(label)
            raise AssertionError(f"{label} ran before the pair refusal")
        return fn

    monkeypatch.setattr(run_backtest, "load_cached_data", record("run_backtest.load_cached_data"))
    monkeypatch.setattr(data_fetcher, "load_cached_data", record("data_fetcher.load_cached_data"))
    monkeypatch.setattr(offline_manifest, "load_manifest", record("offline_manifest.load_manifest"))
    return calls


@pytest.mark.parametrize("registry", ["spot", "futures"])
def test_single_backtest_refuses_before_the_manifest_or_cached_data_load(data_calls, tmp_path, registry):
    manifest = tmp_path / "manifest.json"
    manifest.write_text("{}")
    with pytest.raises(ValueError, match=REFUSAL):
        run_backtest.run_single_backtest(strategy_name=NAME, symbol="BTC", timeframe="1m", registry=registry,
                                         platform="hyperliquid", manifest_path=str(manifest),
                                         manifest_dataset="btc", manifest_window="train",
                                         close_strategies=[{"name": "tiered_tp_atr"}])
    with pytest.raises(ValueError, match=REFUSAL):
        run_backtest.run_single_backtest(strategy_name=NAME, symbol="BTC", timeframe="1m", registry=registry,
                                         platform="hyperliquid")
    assert data_calls == []


def test_walk_forward_wrapper_and_direct_optimizer_refuse_before_reading_data(data_calls):
    with pytest.raises(ValueError, match=REFUSAL):
        run_backtest.run_walk_forward(strategy_name=NAME, symbol="BTC", timeframe="1m", registry="futures",
                                      platform="hyperliquid")
    touched = []
    with pytest.raises(ValueError, match=REFUSAL):
        optimizer.walk_forward_optimize(_UnreadableFrame(touched), NAME, {}, registry="futures",
                                        platform="hyperliquid", verbose=False,
                                        regime_label_plan={"windows_spec": {}})
    with pytest.raises(ValueError, match=REFUSAL):
        optimizer.walk_forward_optimize(None, NAME, {}, registry="futures", platform="hyperliquid", verbose=False)
    assert touched == [] and data_calls == []


def test_parity_frame_and_fill_report_refuse_before_evaluating_the_frame(data_calls):
    touched = []
    with pytest.raises(ValueError, match=REFUSAL):
        parity_diff.compute_parity_frame(_UnreadableFrame(touched), strategy_name=NAME, registry="futures")
    cfg = parity_diff.ParityConfig(strategy_name=NAME, params={}, registry="futures", platform="hyperliquid",
                                   strategy_type="perps")
    with pytest.raises(ValueError, match=REFUSAL):
        parity_diff.extract_fills_report(_UnreadableFrame(touched), cfg)
    assert touched == [] and data_calls == []


def test_parity_cli_refuses_before_the_cached_data_load_in_both_argument_forms(data_calls, tmp_path, capsys):
    assert parity_diff.main(["--strategy", NAME, "--registry", "futures", "--symbol", "BTC", "--timeframe", "1m"]) == 2
    direct = capsys.readouterr().err
    config = tmp_path / "config.json"
    config.write_text(json.dumps({"config_version": 15, "strategies": [{
        "id": "hl-xarb-btc", "type": "perps", "platform": "hyperliquid",
        "script": "shared_scripts/check_hyperliquid.py", "args": [NAME, "BTC", "1m", "--mode=paper"],
        "close_strategy": {"name": "tiered_tp_atr"}}]}))
    assert parity_diff.main(["--config", str(config), "--strategy-id", "hl-xarb-btc"]) == 2
    configured = capsys.readouterr().err
    assert [REFUSAL in direct, REFUSAL in configured] == [True, True], (direct, configured)
    assert data_calls == []


@pytest.mark.parametrize("script", ["check_strategy.py", "check_robinhood.py", "check_topstep.py"])
def test_spot_registry_check_scripts_refuse_the_pair_before_any_fetch(script):
    env = {k: v for k, v in os.environ.items()
           if not k.upper().startswith(("HYPERLIQUID", "OKX", "ROBINHOOD", "TOPSTEP", "PROJECTX"))}
    proc = subprocess.run([sys.executable, os.path.join(_ROOT, "shared_scripts", script), NAME, "BTC", "1h", "--mode=paper"],
                          capture_output=True, text=True, cwd=_ROOT, env=env, timeout=240)
    payload = json.loads(proc.stdout.strip().splitlines()[-1])
    assert (proc.returncode, payload["signal"]) == (1, 0), proc.stderr[-2000:]
    assert payload["error"].startswith(f"open strategy '{NAME}' is production_refused"), payload["error"]
    assert "Fetching" not in proc.stderr

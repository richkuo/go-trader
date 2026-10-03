import importlib.util
import json
import pathlib
import subprocess
import sys

import pytest

_HERE = pathlib.Path(__file__).parent
_ROOT = _HERE.parent
for _p in (str(_HERE), str(_ROOT), str(_ROOT / "shared_tools")):
    if _p not in sys.path:
        sys.path.insert(0, _p)


def _load(name: str, relpath: str):
    spec = importlib.util.spec_from_file_location(name, str(_ROOT / relpath))
    mod = importlib.util.module_from_spec(spec)
    sys.modules[name] = mod
    spec.loader.exec_module(mod)
    return mod


rb = _load("run_backtest_under_test_atr_regime", "backtest/run_backtest.py")

TRAIL_V17 = "trailing_stop_atr_regime"
TRAIL_V18 = "trail_stop_atr_regime"
TRAIL_CANON = "trailing_stop_atr_mult_regime"
SL_LEGACY = "stop_loss_atr_regime"
SL_CANON = "stop_loss_atr_mult_regime"

BLOCK_A = {"trending_up": 2.0}
BLOCK_B = {"trending_up": 4.0}


def test_single_legacy_spelling_renames_to_canonical():
    out = rb._normalize_atr_regime_keys({"strategies": [{TRAIL_V18: BLOCK_A}]})
    assert out["strategies"][0] == {TRAIL_CANON: BLOCK_A}

    out = rb._normalize_atr_regime_keys({"strategies": [{TRAIL_V17: BLOCK_A}]})
    assert out["strategies"][0] == {TRAIL_CANON: BLOCK_A}

    out = rb._normalize_atr_regime_keys({SL_LEGACY: BLOCK_A})
    assert out == {SL_CANON: BLOCK_A}


@pytest.mark.parametrize("block", [{TRAIL_V17: BLOCK_A, TRAIL_V18: BLOCK_A},
                                   {TRAIL_V18: BLOCK_A, TRAIL_V17: BLOCK_A}])
def test_two_legacy_spellings_with_equal_values_merge_regardless_of_order(block):
    assert rb._normalize_atr_regime_keys(block) == {TRAIL_CANON: BLOCK_A}


@pytest.mark.parametrize("block", [{TRAIL_V17: BLOCK_A, TRAIL_V18: BLOCK_B},
                                   {TRAIL_V18: BLOCK_B, TRAIL_V17: BLOCK_A}])
def test_two_legacy_spellings_with_differing_values_raise(block):
    with pytest.raises(rb.AtrRegimeKeyConflict):
        rb._normalize_atr_regime_keys(block)


@pytest.mark.parametrize("block", [{TRAIL_V17: BLOCK_A, TRAIL_CANON: BLOCK_B},
                                   {TRAIL_V18: BLOCK_A, TRAIL_CANON: BLOCK_B}])
def test_legacy_conflicting_with_canonical_raises(block):
    with pytest.raises(rb.AtrRegimeKeyConflict):
        rb._normalize_atr_regime_keys(block)


def test_conflict_is_detected_when_nested_in_lists_and_dicts():
    cfg = {"user_defaults": {"close": {"trailing_tp_ratchet_regime": {
        TRAIL_V17: BLOCK_A, TRAIL_V18: BLOCK_B}}}}
    with pytest.raises(rb.AtrRegimeKeyConflict):
        rb._normalize_atr_regime_keys(cfg)


def _run_preflight(tmp_path, strategy_block):
    cfg = {
        "config_version": 19,
        "default_stop_loss_atr_mult": 1.0,
        "strategies": [dict({
            "id": "hl-x", "platform": "hyperliquid", "type": "perps",
            "args": ["--mode", "live"], "leverage": 10,
        }, **strategy_block)],
    }
    deploy = tmp_path / "deploy"
    (deploy / "scheduler").mkdir(parents=True)
    (deploy / "scheduler" / "config.json").write_text(json.dumps(cfg))
    return subprocess.run(
        ["bash", str(_ROOT / "scripts" / "check-hl-stop-bankruptcy-bound.sh"),
         str(deploy)],
        capture_output=True, text=True)


def test_preflight_fails_closed_on_conflicting_legacy_spellings(tmp_path):
    res = _run_preflight(tmp_path, {TRAIL_V17: BLOCK_A, TRAIL_V18: BLOCK_B})
    assert res.returncode == 1
    assert "cannot verify: conflict:" in res.stdout


def test_preflight_passes_when_legacy_spellings_agree(tmp_path):
    res = _run_preflight(tmp_path, {TRAIL_V17: BLOCK_A, TRAIL_V18: BLOCK_A})
    assert res.returncode == 0, res.stdout
    assert "VERDICT: OK" in res.stdout

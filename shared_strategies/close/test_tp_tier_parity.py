import importlib
import importlib.util
import json
from pathlib import Path

import pytest

_FIXTURE = json.loads(
    (Path(__file__).resolve().parents[2] / "backtest" / "testdata" / "tp_tier_parity.json").read_text()
)


def _load_close_registry():
    path = Path(__file__).resolve().parent / "registry.py"
    spec = importlib.util.spec_from_file_location("_close_registry_under_test", path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


@pytest.fixture(scope="module")
def registry():
    return _load_close_registry()


@pytest.mark.parametrize("case", _FIXTURE["ladder_rule"], ids=lambda c: c["id"])
def test_resting_ladder_rule_matches_fixture(registry, case):
    got = importlib.import_module("tiered_tp_atr").resting_tiers(case["tiers"])

    assert (None if got is None else [list(pair) for pair in got]) == case["want"]

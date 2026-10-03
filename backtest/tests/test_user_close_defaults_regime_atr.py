
from __future__ import annotations

import json
import sys
from pathlib import Path

import pytest

REPO_ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(REPO_ROOT / "backtest"))
sys.path.insert(0, str(REPO_ROOT / "shared_strategies" / "close"))

from run_backtest import (
    _apply_user_close_defaults,
    _validate_user_close_defaults_regime_atr,
    load_strategy_config,
)


@pytest.mark.parametrize("payload,match", [
    (
        {"regime_atr": {"stop_loss_atr_mult_regime": {"use_defaults": True},
                        "foo": 1}},
        "unknown key 'foo'",
    ),
    (
        {
            "regime_atr": {
                "stop_loss_atr_mult_regime": {
                    "trend_regime": {
                        "trending_up": {"close_fraction": 0.5},
                    }
                }
            }
        },
        "close_fraction",
    ),
])
def test_validate_regime_atr_rejects(payload, match):
    with pytest.raises(ValueError, match=match):
        _validate_user_close_defaults_regime_atr(payload)


_USER_SL_REGIME_ATR = {
    "regime_atr": {
        "stop_loss_atr_mult_regime": {
            "trend_regime": {
                "trending_up": {"atr_multiple": 2.25},
                "trending_down": {"atr_multiple": 2.25},
                "ranging": {"atr_multiple": 1.25},
            }
        }
    }
}

_USER_TRAIL_REGIME_ATR = {
    "regime_atr": {
        "trailing_stop_atr_mult_regime": {
            "trend_regime": {
                "trending_up": {"atr_multiple": 9.0},
                "trending_down": {"atr_multiple": 9.0},
                "ranging": {"atr_multiple": 9.0},
            }
        }
    }
}

_RATCHET_REGIME_REF = [
    {"name": "trailing_tp_ratchet_regime", "params": {"use_defaults": True}}
]


@pytest.mark.parametrize("sc,close_refs,user_defaults,key,path,expected", [
    (
        {"stop_loss_atr_mult_regime": {"use_defaults": True}},
        [],
        _USER_SL_REGIME_ATR,
        "stop_loss_atr_mult_regime",
        ("trend_regime", "ranging", "atr_multiple"),
        1.25,
    ),
    (
        {"stop_loss_atr_mult_regime": {"use_defaults": True}},
        [],
        {"regime_atr": {"stop_loss_atr_mult_regime": {"use_defaults": True}}},
        "stop_loss_atr_mult_regime",
        (),
        {"use_defaults": True},
    ),
    (
        {},
        _RATCHET_REGIME_REF,
        dict(
            _USER_TRAIL_REGIME_ATR,
            trailing_tp_ratchet_regime={
                "tp_tiers": [
                    {"atr_multiple": 1.0, "trailing_mult_after": 1.0,
                     "close_fraction": 0.0},
                ],
                "trailing_stop_atr_mult_regime": {
                    "trend_regime": {
                        "trending_up": {"atr_multiple": 2.75},
                        "trending_down": {"atr_multiple": 2.75},
                        "ranging": {"atr_multiple": 1.5},
                    }
                },
            },
        ),
        "trailing_stop_atr_mult_regime",
        ("trend_regime", "trending_up", "atr_multiple"),
        2.75,
    ),
    (
        {"trailing_stop_atr_mult_regime": {"use_defaults": True}},
        _RATCHET_REGIME_REF,
        _USER_TRAIL_REGIME_ATR,
        "trailing_stop_atr_mult_regime",
        (),
        {"use_defaults": True},
    ),
])
def test_apply_user_close_defaults_regime_atr(
        sc, close_refs, user_defaults, key, path, expected):
    sc = json.loads(json.dumps(sc))
    _apply_user_close_defaults(list(close_refs), user_defaults, sc)
    got = sc[key]
    for step in path:
        got = got[step]
    assert got == expected


def test_load_strategy_config_rejects_malformed_regime_atr(tmp_path):
    cfg = {
        "config_version": 16,
        "strategies": [
            {
                "id": "hl-test",
                "type": "perps",
                "platform": "hyperliquid",
                "args": ["hold", "BTC", "1h"],
                "stop_loss_atr_mult_regime": {"use_defaults": True},
            }
        ],
        "user_defaults": {
            "regime_atr": {
                "stop_loss_atr_mult_regime": {
                    "trend_regime": {"trending_up": {"close_fraction": 0.5}}
                }
            }
        },
    }
    path = tmp_path / "config.json"
    path.write_text(json.dumps(cfg))
    with pytest.raises(ValueError, match="close_fraction"):
        load_strategy_config(str(path), "hl-test", inject_user_defaults=True)


def _legacy_trail_config() -> dict:
    return {
        "config_version": 17,
        "regime": {"enabled": True, "period": 14, "adx_threshold": 20},
        "strategies": [
            {
                "id": "hl-test",
                "type": "perps",
                "platform": "hyperliquid",
                "args": ["hold", "BTC", "1h"],
                "open_strategy": {"name": "hold", "params": {}},
                "trail_stop_atr_regime": {
                    "trend_regime": {
                        "trending_up": {"atr_multiple": 2.5},
                        "trending_down": {"atr_multiple": 2.5},
                        "ranging": {"atr_multiple": 2.0},
                    }
                },
            }
        ],
    }


@pytest.mark.parametrize("duplicate_canonical", [False, True])
def test_load_strategy_config_accepts_legacy_trail_key(
        tmp_path, duplicate_canonical):
    cfg = _legacy_trail_config()
    if duplicate_canonical:
        sc = cfg["strategies"][0]
        sc["trailing_stop_atr_mult_regime"] = json.loads(
            json.dumps(sc["trail_stop_atr_regime"]))
    path = tmp_path / "config.json"
    path.write_text(json.dumps(cfg))
    kwargs = load_strategy_config(str(path), "hl-test")
    assert "trail_stop_atr_regime" not in kwargs
    block = kwargs["trailing_stop_atr_mult_regime"]
    assert block["trend_regime"]["ranging"]["atr_multiple"] == 2.0


def test_load_strategy_config_legacy_and_canonical_trail_keys_agree(tmp_path):
    legacy_path = tmp_path / "legacy.json"
    legacy_path.write_text(json.dumps(_legacy_trail_config()))

    canonical = _legacy_trail_config()
    sc = canonical["strategies"][0]
    sc["trailing_stop_atr_mult_regime"] = sc.pop("trail_stop_atr_regime")
    canonical_path = tmp_path / "canonical.json"
    canonical_path.write_text(json.dumps(canonical))

    assert load_strategy_config(str(legacy_path), "hl-test") == load_strategy_config(
        str(canonical_path), "hl-test"
    )


@pytest.mark.parametrize("conflicting_key", [
    "trailing_stop_atr_mult_regime",
    "trailing_stop_atr_regime",
])
def test_load_strategy_config_rejects_conflicting_trail_keys(
        tmp_path, conflicting_key):
    cfg = _legacy_trail_config()
    cfg["strategies"][0][conflicting_key] = {
        "trend_regime": {
            "trending_up": {"atr_multiple": 1.1},
            "trending_down": {"atr_multiple": 1.1},
            "ranging": {"atr_multiple": 1.1},
        }
    }
    path = tmp_path / "config.json"
    path.write_text(json.dumps(cfg))
    with pytest.raises(ValueError):
        load_strategy_config(str(path), "hl-test")

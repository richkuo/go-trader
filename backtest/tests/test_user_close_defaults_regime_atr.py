
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


def test_validate_regime_atr_rejects_stray_key():
    with pytest.raises(ValueError, match="unknown key 'foo'"):
        _validate_user_close_defaults_regime_atr(
            {"regime_atr": {"stop_loss_atr_regime": {"use_defaults": True}, "foo": 1}}
        )


def test_validate_regime_atr_rejects_bad_stop_shape():
    with pytest.raises(ValueError, match="close_fraction"):
        _validate_user_close_defaults_regime_atr(
            {
                "regime_atr": {
                    "stop_loss_atr_regime": {
                        "trend_regime": {
                            "trending_up": {"close_fraction": 0.5},
                        }
                    }
                }
            }
        )


def test_apply_regime_atr_injects_standalone_stop_loss():
    sc = {"stop_loss_atr_regime": {"use_defaults": True}}
    close_refs = []
    user_defaults = {
        "regime_atr": {
            "stop_loss_atr_regime": {
                "trend_regime": {
                    "trending_up": {"atr_multiple": 2.25},
                    "trending_down": {"atr_multiple": 2.25},
                    "ranging": {"atr_multiple": 1.25},
                }
            }
        }
    }
    _apply_user_close_defaults(close_refs, user_defaults, sc)
    assert sc["stop_loss_atr_regime"]["trend_regime"]["ranging"]["atr_multiple"] == 1.25


def test_apply_regime_atr_use_defaults_user_block_is_noop():
    sc = {"stop_loss_atr_regime": {"use_defaults": True}}
    close_refs = []
    user_defaults = {"regime_atr": {"stop_loss_atr_regime": {"use_defaults": True}}}
    _apply_user_close_defaults(close_refs, user_defaults, sc)
    assert sc["stop_loss_atr_regime"] == {"use_defaults": True}


def test_apply_regime_atr_skips_ratchet_close():
    sc: dict = {}
    close_refs = [{"name": "trailing_tp_ratchet_regime", "params": {"use_defaults": True}}]
    user_defaults = {
        "regime_atr": {
            "trail_stop_atr_regime": {
                "trend_regime": {
                    "trending_up": {"atr_multiple": 9.0},
                    "trending_down": {"atr_multiple": 9.0},
                    "ranging": {"atr_multiple": 9.0},
                }
            }
        },
        "trailing_tp_ratchet_regime": {
            "tp_tiers": [
                {"atr_multiple": 1.0, "trailing_mult_after": 1.0, "close_fraction": 0.0},
            ],
            "trail_stop_atr_regime": {
                "trend_regime": {
                    "trending_up": {"atr_multiple": 2.75},
                    "trending_down": {"atr_multiple": 2.75},
                    "ranging": {"atr_multiple": 1.5},
                }
            },
        },
    }
    _apply_user_close_defaults(close_refs, user_defaults, sc)
    assert sc["trail_stop_atr_regime"]["trend_regime"]["trending_up"]["atr_multiple"] == 2.75


def test_apply_regime_atr_leaves_ratchet_use_defaults_trail_untouched():
    sc = {"trail_stop_atr_regime": {"use_defaults": True}}
    close_refs = [{"name": "trailing_tp_ratchet_regime", "params": {"use_defaults": True}}]
    user_defaults = {
        "regime_atr": {
            "trail_stop_atr_regime": {
                "trend_regime": {
                    "trending_up": {"atr_multiple": 9.0},
                    "trending_down": {"atr_multiple": 9.0},
                    "ranging": {"atr_multiple": 9.0},
                }
            }
        },
    }
    _apply_user_close_defaults(close_refs, user_defaults, sc)
    assert sc["trail_stop_atr_regime"] == {"use_defaults": True}


def test_load_strategy_config_rejects_malformed_regime_atr(tmp_path):
    cfg = {
        "config_version": 16,
        "strategies": [
            {
                "id": "hl-test",
                "type": "perps",
                "platform": "hyperliquid",
                "args": ["hold", "BTC", "1h"],
                "stop_loss_atr_regime": {"use_defaults": True},
            }
        ],
        "user_defaults": {
            "regime_atr": {
                "stop_loss_atr_regime": {
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
                "trailing_stop_atr_regime": {
                    "trend_regime": {
                        "trending_up": {"atr_multiple": 2.5},
                        "trending_down": {"atr_multiple": 2.5},
                        "ranging": {"atr_multiple": 2.0},
                    }
                },
            }
        ],
    }


def test_load_strategy_config_accepts_legacy_trailing_stop_atr_regime_key(tmp_path):
    path = tmp_path / "config.json"
    path.write_text(json.dumps(_legacy_trail_config()))
    kwargs = load_strategy_config(str(path), "hl-test")
    assert "trailing_stop_atr_regime" not in kwargs
    block = kwargs["trail_stop_atr_regime"]
    assert block["trend_regime"]["ranging"]["atr_multiple"] == 2.0


def test_load_strategy_config_legacy_and_canonical_trail_keys_agree(tmp_path):
    legacy_path = tmp_path / "legacy.json"
    legacy_path.write_text(json.dumps(_legacy_trail_config()))

    canonical = _legacy_trail_config()
    sc = canonical["strategies"][0]
    sc["trail_stop_atr_regime"] = sc.pop("trailing_stop_atr_regime")
    canonical_path = tmp_path / "canonical.json"
    canonical_path.write_text(json.dumps(canonical))

    assert load_strategy_config(str(legacy_path), "hl-test") == load_strategy_config(
        str(canonical_path), "hl-test"
    )


def test_load_strategy_config_canonical_trail_key_wins_over_legacy(tmp_path):
    cfg = _legacy_trail_config()
    cfg["strategies"][0]["trail_stop_atr_regime"] = {
        "trend_regime": {
            "trending_up": {"atr_multiple": 1.1},
            "trending_down": {"atr_multiple": 1.1},
            "ranging": {"atr_multiple": 1.1},
        }
    }
    path = tmp_path / "config.json"
    path.write_text(json.dumps(cfg))
    kwargs = load_strategy_config(str(path), "hl-test")
    assert kwargs["trail_stop_atr_regime"]["trend_regime"]["ranging"]["atr_multiple"] == 1.1

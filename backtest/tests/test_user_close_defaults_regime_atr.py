"""#1134: user_close_defaults.regime_atr parity with the Go loader."""

from __future__ import annotations

import json
import sys
from pathlib import Path

import pytest

REPO_ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(REPO_ROOT / "backtest"))
sys.path.insert(0, str(REPO_ROOT / "shared_strategies" / "close"))

from run_backtest import (  # noqa: E402
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
            "trailing_stop_atr_regime": {
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
            "trailing_stop_atr_regime": {
                "trend_regime": {
                    "trending_up": {"atr_multiple": 2.75},
                    "trending_down": {"atr_multiple": 2.75},
                    "ranging": {"atr_multiple": 1.5},
                }
            },
        },
    }
    _apply_user_close_defaults(close_refs, user_defaults, sc)
    assert sc["trailing_stop_atr_regime"]["trend_regime"]["trending_up"]["atr_multiple"] == 2.75


def test_apply_regime_atr_leaves_ratchet_use_defaults_trail_untouched():
    sc = {"trailing_stop_atr_regime": {"use_defaults": True}}
    close_refs = [{"name": "trailing_tp_ratchet_regime", "params": {"use_defaults": True}}]
    user_defaults = {
        "regime_atr": {
            "trailing_stop_atr_regime": {
                "trend_regime": {
                    "trending_up": {"atr_multiple": 9.0},
                    "trending_down": {"atr_multiple": 9.0},
                    "ranging": {"atr_multiple": 9.0},
                }
            }
        },
    }
    _apply_user_close_defaults(close_refs, user_defaults, sc)
    assert sc["trailing_stop_atr_regime"] == {"use_defaults": True}


def test_load_strategy_config_rejects_malformed_regime_atr(tmp_path):
    cfg = {
        "config_version": 15,
        "strategies": [
            {
                "id": "hl-test",
                "type": "perps",
                "platform": "hyperliquid",
                "args": ["hold", "BTC", "1h"],
                "stop_loss_atr_regime": {"use_defaults": True},
            }
        ],
        "user_close_defaults": {
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

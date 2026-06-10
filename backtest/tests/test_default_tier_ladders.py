"""Default tiered-TP ATR ladder must agree across all three sources of truth.

The fallback ladder used when a ``tiered_tp_atr*`` close ref omits explicit
tiers lives in THREE places that must stay byte-for-byte identical:

  • Go   — ``defaultHLProtectionTiers()`` in scheduler/hyperliquid_protection.go
           (the on-chain reduce-only TP source of truth; #870)
  • Py   — ``DEFAULT_TIERS`` in shared_strategies/close/tiered_tp_atr.py
           (paper + backtest scale-out for tiered_tp_atr / tiered_tp_atr_live)
  • Py   — ``_DEFAULT_SCALAR_TP_TIERS`` in shared_strategies/close/post_tp_sl.py
           (the ``tp_atr_fraction`` firing-tier multiple is derived from this)

Today all three are 1.5×/3×/5× @ 40%/80%/100% cumulative. A retune that
updates one mirror but misses another silently desyncs live on-chain TP
placement from paper/backtest scale-outs. This test (mirroring the parity
style of test_platform_fees.py) pins the literal and cross-checks all three
so the suite fails the moment they drift. If you intentionally retune the
ladder, update ALL THREE sources AND the ``EXPECTED_LADDER`` below together.
"""
import importlib.util
import os
import re
import sys

import pytest


_REPO_ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
_CLOSE_DIR = os.path.join(_REPO_ROOT, "shared_strategies", "close")
_HYPERLIQUID_PROTECTION_GO = os.path.join(
    _REPO_ROOT, "scheduler", "hyperliquid_protection.go"
)

# The canonical ladder as (atr_multiple, cumulative_close_fraction) pairs.
# Mirror of #870's patient 3-rung scale-out. If a real retune lands, change
# this AND all three sources scraped/imported below in the same commit.
EXPECTED_LADDER = (
    (1.5, 0.40),
    (3.0, 0.80),
    (5.0, 1.00),
)


def _load_close_module(filename: str, attr: str, mod_name: str):
    """Load a module under shared_strategies/close via spec_from_file_location
    (sidesteps the open/close registry.py name collision) with both the repo
    root and the close dir on sys.path so absolute/relative imports resolve."""
    for p in (_REPO_ROOT, _CLOSE_DIR):
        if p not in sys.path:
            sys.path.insert(0, p)
    path = os.path.join(_CLOSE_DIR, filename)
    spec = importlib.util.spec_from_file_location(mod_name, path)
    mod = importlib.util.module_from_spec(spec)
    sys.modules[mod_name] = mod
    spec.loader.exec_module(mod)
    return getattr(mod, attr)


def _python_tiered_tp_atr_ladder():
    """``DEFAULT_TIERS`` from tiered_tp_atr.py as (mult, frac) pairs."""
    raw = _load_close_module(
        "tiered_tp_atr.py", "DEFAULT_TIERS", "_ladder_probe_tiered_tp_atr"
    )
    return tuple(
        (float(t["atr_multiple"]), float(t["close_fraction"])) for t in raw
    )


def _python_scalar_tp_ladder():
    """``_DEFAULT_SCALAR_TP_TIERS`` from post_tp_sl.py as (mult, frac) pairs."""
    raw = _load_close_module(
        "post_tp_sl.py", "_DEFAULT_SCALAR_TP_TIERS", "_ladder_probe_post_tp_sl"
    )
    return tuple((float(m), float(f)) for m, f in raw)


def _go_default_protection_tiers():
    """Scrape ``defaultHLProtectionTiers()`` from hyperliquid_protection.go and
    parse its ``{Multiple: X, Fraction: Y}`` rows into (mult, frac) pairs."""
    text = open(_HYPERLIQUID_PROTECTION_GO, encoding="utf-8").read()
    body = re.search(
        r"func defaultHLProtectionTiers\(\)\s*\[\]hlProtectionTier\s*\{(.*?)\n\}",
        text,
        re.DOTALL,
    )
    assert body, "defaultHLProtectionTiers() not found in hyperliquid_protection.go"
    rows = re.findall(
        r"\{\s*Multiple:\s*([0-9.]+),\s*Fraction:\s*([0-9.]+)\s*\}", body.group(1)
    )
    assert rows, "no {Multiple:..,Fraction:..} rows parsed from the Go ladder"
    return tuple((float(m), float(f)) for m, f in rows)


# ─── Each source matches the pinned expectation ──────────────────────────────


def test_python_tiered_tp_atr_default_tiers_match_expected():
    assert _python_tiered_tp_atr_ladder() == EXPECTED_LADDER


def test_python_scalar_tp_tiers_match_expected():
    assert _python_scalar_tp_ladder() == EXPECTED_LADDER


def test_go_default_protection_tiers_match_expected():
    assert _go_default_protection_tiers() == EXPECTED_LADDER


# ─── Cross-source parity (the actual desync guard) ───────────────────────────


def test_all_three_default_tier_ladders_agree():
    go = _go_default_protection_tiers()
    py_atr = _python_tiered_tp_atr_ladder()
    py_scalar = _python_scalar_tp_ladder()
    assert go == py_atr == py_scalar, (
        "Default tier ladder desync — retune one of "
        "defaultHLProtectionTiers() (Go), DEFAULT_TIERS (tiered_tp_atr.py), "
        "_DEFAULT_SCALAR_TP_TIERS (post_tp_sl.py) and you MUST update all three "
        f"together. Go={go} tiered_tp_atr={py_atr} scalar={py_scalar}"
    )


def test_final_tier_closes_everything_remaining():
    """The final rung is a full exit (cumulative fraction == 1.0); finalize
    coerces it to 1.0 live, so the default must already encode that."""
    for ladder in (
        _go_default_protection_tiers(),
        _python_tiered_tp_atr_ladder(),
        _python_scalar_tp_ladder(),
    ):
        assert ladder[-1][1] == pytest.approx(1.0)


def test_tier_multiples_and_fractions_are_monotonic():
    """ATR triggers strictly increase and cumulative fractions are
    non-decreasing — a retune that breaks this would mis-order the scale-out."""
    for ladder in (
        _go_default_protection_tiers(),
        _python_tiered_tp_atr_ladder(),
        _python_scalar_tp_ladder(),
    ):
        mults = [m for m, _ in ladder]
        fracs = [f for _, f in ladder]
        assert mults == sorted(mults) and len(set(mults)) == len(mults)
        assert fracs == sorted(fracs)

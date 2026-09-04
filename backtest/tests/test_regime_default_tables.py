from __future__ import annotations

import importlib.util
import os
import re
import sys

_REPO_ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
_CLOSE_DIR = os.path.join(_REPO_ROOT, "shared_strategies", "close")
_REGIME_ATR_GO = os.path.join(_REPO_ROOT, "scheduler", "regime_atr.go")
_TRAILING_RATCHET_GO = os.path.join(_REPO_ROOT, "scheduler", "trailing_tp_ratchet.go")

def _load_close_module(filename: str, attr: str, mod_name: str):
    for p in (_REPO_ROOT, _CLOSE_DIR):
        if p not in sys.path:
            sys.path.insert(0, p)
    path = os.path.join(_CLOSE_DIR, filename)
    spec = importlib.util.spec_from_file_location(mod_name, path)
    mod = importlib.util.module_from_spec(spec)
    sys.modules[mod_name] = mod
    spec.loader.exec_module(mod)
    return getattr(mod, attr)


def _go_regime_atr_trailing() -> dict[str, float]:
    text = open(_REGIME_ATR_GO, encoding="utf-8").read()
    body = re.search(
        r"Trailing:\s*map\[string\]RegimeATREntry\s*\{(.*?)\n\t\},\n\}",
        text,
        re.DOTALL,
    )
    assert body, "regimeATRDefaults.Trailing block not found"
    return {
        m.group(1): float(m.group(2))
        for m in re.finditer(r'"([^"]+)":\s*\{ATR:\s*([0-9.]+)\}', body.group(1))
    }


def _go_regime_atr_stop_loss() -> dict[str, float]:
    text = open(_REGIME_ATR_GO, encoding="utf-8").read()
    body = re.search(
        r"StopLoss:\s*map\[string\]RegimeATREntry\s*\{(.*?)\},\s*\n\s*Trailing:",
        text,
        re.DOTALL,
    )
    assert body, "regimeATRDefaults.StopLoss block not found"
    return {
        m.group(1): float(m.group(2))
        for m in re.finditer(r'"([^"]+)":\s*\{ATR:\s*([0-9.]+)\}', body.group(1))
    }


def _go_ratchet_tiers() -> dict[str, tuple]:
    text = open(_TRAILING_RATCHET_GO, encoding="utf-8").read()
    body = re.search(
        r"var ratchetTierGroupDefaults = map\[string\]\[\]trailingRatchetTier\s*\{(.*?)\n\}",
        text,
        re.DOTALL,
    )
    assert body, "ratchetTierGroupDefaults not found"
    out: dict[str, tuple] = {}
    chunks = re.split(r'\n\t"([^"]+)":\s*\{', body.group(1))
    i = 1
    while i + 1 < len(chunks):
        group = chunks[i]
        chunk = chunks[i + 1]
        rows = re.findall(
            r"\{ATRMultiple:\s*([0-9.]+),\s*CloseFraction:\s*([0-9.]+),\s*TrailingMultAfter:\s*([0-9.]+)\}",
            chunk,
        )
        out[group] = tuple((float(a), float(c), float(t)) for a, c, t in rows)
        i += 2
    return out


def _go_tp_tier_groups() -> dict[str, tuple]:
    text = open(_REGIME_ATR_GO, encoding="utf-8").read()
    body = re.search(
        r"var regimeTPTierGroupDefaults = map\[string\]\[\]hlProtectionTier\s*\{(.*?)\n\}",
        text,
        re.DOTALL,
    )
    assert body, "regimeTPTierGroupDefaults not found"
    out: dict[str, tuple] = {}
    for line in body.group(1).splitlines():
        line = line.strip().rstrip(",")
        m = re.match(r'"([^"]+)":\s*\{(.*)\}$', line)
        if not m:
            continue
        group, inner = m.group(1), m.group(2)
        rows = re.findall(
            r"\{Multiple:\s*([0-9.]+),\s*Fraction:\s*([0-9.]+)\}",
            inner,
        )
        out[group] = tuple((float(a), float(b)) for a, b in rows)
    return out


def _py_trailing() -> dict[str, float]:
    raw = _load_close_module(
        "regime_atr.py", "REGIME_ATR_DEFAULTS_TRAILING", "_regime_probe_trailing"
    )
    return {k: float(v.atr) for k, v in raw.items()}


def _py_stop_loss() -> dict[str, float]:
    raw = _load_close_module(
        "regime_atr.py", "REGIME_ATR_DEFAULTS_STOP_LOSS", "_regime_probe_sl"
    )
    return {k: float(v.atr) for k, v in raw.items()}


def _py_ratchet_tiers() -> dict[str, tuple]:
    raw = _load_close_module(
        "trailing_tp_ratchet.py",
        "DEFAULT_RATCHET_TIERS_BY_GROUP",
        "_regime_probe_ratchet",
    )
    out: dict[str, tuple] = {}
    for group, tiers in raw.items():
        out[group] = tuple(
            (float(t["atr_multiple"]), float(t["close_fraction"]), float(t["trailing_mult_after"]))
            for t in tiers
        )
    return out


def _py_tp_tier_groups() -> dict[str, tuple]:
    raw = _load_close_module(
        "regime_atr.py", "REGIME_TP_TIER_GROUP_DEFAULTS", "_regime_probe_tp"
    )
    return {k: tuple((float(m), float(f)) for m, f in v) for k, v in raw.items()}


def test_go_python_regime_tables_agree():
    assert _go_regime_atr_trailing() == _py_trailing()
    assert _go_regime_atr_stop_loss() == _py_stop_loss()
    assert _go_ratchet_tiers() == _py_ratchet_tiers()
    assert _go_tp_tier_groups() == _py_tp_tier_groups()

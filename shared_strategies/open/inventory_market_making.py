import hashlib
import json
import math
from fractions import Fraction
from typing import Any, Dict, List

NAME = "inventory_market_making"
SPEC_VERSION = 1

QUOTE_CONTRACT: Dict[str, str] = {"input": "deadline_book_evidence", "output": "two_sided_quotes"}

QUOTE_POLICY_SPEC: Dict[str, Any] = {
    "schema": "go-trader.quote-policy-spec",
    "version": 1,
    "strategy": "inventory_market_making",
    "capability": {"input": "deadline_book_evidence", "output": "two_sided_quotes"},
    "params": [
        {"name": "quote_notional_usd", "type": "number", "default": 100, "min": 20, "max": 500, "unit": "USD per side"},
        {"name": "target_inventory_usd", "type": "number", "default": 0, "min": -2500, "max": 2500, "unit": "USD marked"},
        {"name": "risk_aversion", "type": "number", "default": 1, "min": 0, "max": 4, "unit": "dimensionless"},
        {"name": "volatility_multiplier", "type": "number", "default": 1, "min": 0, "max": 4, "unit": "dimensionless"},
        {"name": "volatility_window_seconds", "type": "integer", "default": 300, "min": 60, "max": 600, "unit": "seconds"},
        {"name": "min_spread_bps", "type": "number", "default": 10, "min": 2, "max": 50, "unit": "basis points, full spread"},
        {"name": "max_spread_bps", "type": "number", "default": 100, "min": 20, "max": 200, "unit": "basis points, full spread"},
        {"name": "max_skew_bps", "type": "number", "default": 50, "min": 0, "max": 100, "unit": "basis points"},
        {"name": "taper_band_pct", "type": "number", "default": 25, "min": 0, "min_exclusive": True, "max": 100,
         "unit": "percent of the cap"},
        {"name": "quote_lifetime_seconds", "type": "integer", "default": 30, "min": 5, "max": 120, "unit": "seconds"},
        {"name": "max_input_age_ms", "type": "integer", "default": 7500, "min": 1000, "max": 10000, "unit": "milliseconds"},
        {"name": "refresh_threshold_bps", "type": "number", "default": 2, "min": 0, "max": 20,
         "unit": "basis points of the midpoint"},
        {"name": "max_inventory_age_seconds", "type": "integer", "default": 300, "min": 30, "max": 1800, "unit": "seconds"},
        {"name": "round_trip_reserve_bps", "type": "number", "default": 2, "min": 0, "max": 20, "unit": "basis points"},
        {"name": "maker_fee_bps", "type": "number", "default": 1.5, "min": -10, "max": 10,
         "unit": "basis points per fill; a negative value never narrows the spread floor"},
        {"name": "min_order_value_usd", "type": "number", "default": 10, "min": 0, "max": 1000,
         "unit": "USD per risk-increasing order"},
    ],
    "param_rules": [
        {"kind": "le", "left": "min_spread_bps", "right": "max_spread_bps"},
    ],
    "config": [
        {"name": "interval_seconds", "type": "integer", "default": 5, "min": 1, "max": 60, "unit": "seconds, the due-deadline cycle"},
        {"name": "max_inventory_usd", "type": "number", "default": 1000, "min": 200, "max": 5000,
         "unit": "USD marked, quote.max_inventory_usd"},
        {"name": "stop_loss_pct", "type": "number", "default": 2, "min": 0, "min_exclusive": True, "max": 20,
         "unit": "percent from the risk anchor"},
    ],
    "config_rules": [
        {"kind": "le_fraction", "left": "quote_notional_usd", "right": "max_inventory_usd", "fraction": "1/4"},
        {"kind": "abs_le_fraction", "left": "target_inventory_usd", "right": "max_inventory_usd", "fraction": "1/2"},
        {"kind": "ge", "left": "quote_lifetime_seconds", "right": "interval_seconds"},
    ],
    "fixed": {"capital": 10000, "leverage": 1, "margin_mode": "isolated", "platform": "hyperliquid", "type": "perps",
              "close_strategy": "inventory_quote_exit"},
}


def spec_canonical_bytes(spec: Dict[str, Any] = None) -> bytes:
    return json.dumps(QUOTE_POLICY_SPEC if spec is None else spec, sort_keys=True, separators=(",", ":"),
                      ensure_ascii=False).encode("utf-8")


def spec_sha256(spec: Dict[str, Any] = None) -> str:
    return hashlib.sha256(spec_canonical_bytes(spec)).hexdigest()


def _fields(group: str) -> List[Dict[str, Any]]:
    return QUOTE_POLICY_SPEC[group]


def default_params() -> Dict[str, Any]:
    return {f["name"]: f["default"] for f in _fields("params")}


def _exact(name: str, raw) -> Fraction:
    if isinstance(raw, bool) or not isinstance(raw, (int, float)):
        raise ValueError(f"{NAME}: {name} must be a number, got {raw!r}")
    if isinstance(raw, float) and not math.isfinite(raw):
        raise ValueError(f"{NAME}: {name} must be finite, got {raw!r}")
    if isinstance(raw, float):
        return Fraction(repr(raw))
    return Fraction(raw)


def _text(v: Fraction) -> str:
    if v.denominator == 1:
        return str(v.numerator)
    return repr(float(v))


def _check(field: Dict[str, Any], v: Fraction) -> None:
    name = field["name"]
    if field["type"] == "integer" and v.denominator != 1:
        raise ValueError(f"{NAME}: {name} must be an integer, got {_text(v)}")
    lo, hi = Fraction(str(field["min"])), Fraction(str(field["max"]))
    if field.get("min_exclusive") and v <= lo:
        raise ValueError(f"{NAME}: {name} must be > {field['min']}, got {_text(v)}")
    if not field.get("min_exclusive") and v < lo:
        raise ValueError(f"{NAME}: {name} must be >= {field['min']}, got {_text(v)}")
    if v > hi:
        raise ValueError(f"{NAME}: {name} must be <= {field['max']}, got {_text(v)}")


def _resolve(fields: List[Dict[str, Any]], values: Dict[str, Any]) -> Dict[str, Fraction]:
    if not isinstance(values, dict):
        raise ValueError(f"{NAME}: parameters must be an object, got {values!r}")
    known = {f["name"] for f in fields}
    unknown = sorted(k for k in values if k not in known)
    if unknown:
        raise ValueError(f"{NAME}: unknown parameters {unknown}")
    out: Dict[str, Fraction] = {}
    for f in fields:
        if f["name"] in values:
            v = _exact(f["name"], values[f["name"]])
        else:
            v = Fraction(str(f["default"]))
        _check(f, v)
        out[f["name"]] = v
    return out


def _rule_errors(rules: List[Dict[str, Any]], values: Dict[str, Fraction]) -> List[str]:
    errs = []
    for r in rules:
        a, b = values[r["left"]], values[r["right"]]
        if r["kind"] == "le" and a > b:
            errs.append(f"{r['left']} ({_text(a)}) must be <= {r['right']} ({_text(b)})")
        elif r["kind"] == "ge" and a < b:
            errs.append(f"{r['left']} ({_text(a)}) must be >= {r['right']} ({_text(b)})")
        elif r["kind"] in ("le_fraction", "abs_le_fraction"):
            limit = b * Fraction(r["fraction"])
            lhs = abs(a) if r["kind"] == "abs_le_fraction" else a
            if lhs > limit:
                label = f"|{r['left']}|" if r["kind"] == "abs_le_fraction" else r["left"]
                errs.append(f"{label} ({_text(lhs)}) must be <= {r['fraction']} x {r['right']} ({_text(limit)})")
    return errs


def inventory_market_making_param_validator(params: Dict[str, Any]) -> None:
    values = _resolve(_fields("params"), params)
    errs = _rule_errors(QUOTE_POLICY_SPEC["param_rules"], values)
    if errs:
        raise ValueError(f"{NAME}: " + "; ".join(errs))


def validate_run_config(params: Dict[str, Any], config: Dict[str, Any]) -> None:
    values = _resolve(_fields("params"), params)
    missing = sorted(f["name"] for f in _fields("config") if f["name"] not in config)
    if missing:
        raise ValueError(f"{NAME}: run configuration needs {missing}")
    config_names = {f["name"] for f in _fields("config")}
    values.update(_resolve(_fields("config"), {k: v for k, v in config.items() if k in config_names}))
    errs = _rule_errors(QUOTE_POLICY_SPEC["param_rules"], values) + _rule_errors(QUOTE_POLICY_SPEC["config_rules"], values)
    if errs:
        raise ValueError(f"{NAME}: " + "; ".join(errs))


def inventory_market_making_scalar_refusal(*_args, **_kwargs):
    raise ValueError(
        f"{NAME} is a quote capability: it decides two-sided quotes at due deadlines through the quote coordinator "
        "and has no scalar candle signal; scalar checks, candle backtests, the optimizer and parity runs refuse it")

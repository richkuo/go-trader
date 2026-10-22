import hashlib
import json
import math
import os
import sys
from fractions import Fraction
from numbers import Real

import numpy as np
import pandas as pd

_TOOLS_DIR = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "..", "shared_tools"))
if _TOOLS_DIR not in sys.path:
    sys.path.insert(0, _TOOLS_DIR)

from book_inputs import CONTRACT, book_features, parse_book_input  # noqa: E402

STRATEGY = "order_book_imbalance"
SPECIFICATION = "order_book_imbalance/1635-v1"
INJECTED_KEY = "l2_book_observations"
EVIDENCE_WINDOW_MS = 60_000
EVIDENCE_CAPACITY = 256
DECISION_GRID_MS = 1000
SOURCE_KEY = "https://api.hyperliquid.xyz|perps|{coin}|l2_book"

DEFAULT_PARAMS = {
    "depth_levels": 5,
    "entry_threshold": 0.30,
    "confirmation_observations": 3,
    "max_spread_bps": 5.0,
    "min_combined_depth_usd": 100000.0,
    "max_snapshot_age_ms": 2000,
}
SEARCH = {
    "depth_levels": [1, 5, 10, 20],
    "entry_threshold": [0.20, 0.30, 0.40],
    "confirmation_observations": [2, 3, 5],
    "max_spread_bps": [2.0, 5.0, 10.0],
    "min_combined_depth_usd": [10000.0, 100000.0, 1000000.0],
    "max_snapshot_age_ms": [1000, 2000, 5000],
}

REASON_CODES = {
    "entry_long": 1,
    "entry_short": -1,
    "no_episode": 0,
    "confirming": 2,
    "consumed": 3,
    "stale_crossing": 4,
    "evidence_truncated": 5,
    "not_decision_row": 6,
    "deadline_off_grid": 7,
    "book_input_missing": 20,
    "book_unavailable": 21,
    "book_malformed": 22,
    "coin_unknown": 23,
}


class BookInputMissing(ValueError):
    pass


def _int_param(name, value, lo, hi):
    if isinstance(value, bool) or not isinstance(value, Real):
        raise ValueError(f"{STRATEGY}: {name} must be an integer, got {value!r}")
    as_float = float(value)
    if not math.isfinite(as_float) or not as_float.is_integer():
        raise ValueError(f"{STRATEGY}: {name} must be an integer, got {value!r}")
    as_int = int(as_float)
    if not lo <= as_int <= hi:
        raise ValueError(f"{STRATEGY}: {name} must be in [{lo}, {hi}], got {value!r}")
    return as_int


def _decimal_param(name, value, lo_exclusive, hi, hi_inclusive=True):
    if isinstance(value, bool) or not isinstance(value, Real):
        raise ValueError(f"{STRATEGY}: {name} must be a number, got {value!r}")
    as_float = float(value)
    if not math.isfinite(as_float):
        raise ValueError(f"{STRATEGY}: {name} must be finite, got {value!r}")
    frac = Fraction(repr(as_float))
    if not frac > lo_exclusive or (frac > hi if hi_inclusive else frac >= hi):
        bound = "]" if hi_inclusive else ")"
        raise ValueError(f"{STRATEGY}: {name} must be in ({lo_exclusive}, {hi}{bound}, got {value!r}")
    return as_float


def validate_params(params: dict) -> dict:
    if params is None:
        params = {}
    if not isinstance(params, dict):
        raise ValueError(f"{STRATEGY}: params must be an object, got {params!r}")
    unknown = sorted(set(params) - set(DEFAULT_PARAMS) - {INJECTED_KEY})
    if unknown:
        raise ValueError(f"{STRATEGY}: unknown parameters {unknown}; expected a subset of {sorted(DEFAULT_PARAMS)}")
    p = {**DEFAULT_PARAMS, **{k: v for k, v in params.items() if k in DEFAULT_PARAMS}}
    return {
        "depth_levels": _int_param("depth_levels", p["depth_levels"], 1, 20),
        "entry_threshold": _decimal_param("entry_threshold", p["entry_threshold"], 0, 1, hi_inclusive=False),
        "confirmation_observations": _int_param("confirmation_observations", p["confirmation_observations"], 2, 5),
        "max_spread_bps": _decimal_param("max_spread_bps", p["max_spread_bps"], 0, 100),
        "min_combined_depth_usd": _decimal_param("min_combined_depth_usd", p["min_combined_depth_usd"], 0, 1_000_000_000),
        "max_snapshot_age_ms": _int_param("max_snapshot_age_ms", p["max_snapshot_age_ms"], 250, 5000),
    }


def obi_param_validator(params: dict) -> None:
    validate_params(params)


def params_json(p: dict) -> str:
    return json.dumps({k: p[k] for k in sorted(p)}, sort_keys=True, separators=(",", ":"))


def classify(feat, p: dict) -> tuple:
    if feat is None:
        return 0, "insufficient_depth"
    if feat["total"] <= 0 or feat["imbalance"] is None:
        return 0, "empty_depth"
    if feat["spread_bps"] > Fraction(repr(p["max_spread_bps"])):
        return 0, "spread_filter"
    if feat["total"] < Fraction(repr(p["min_combined_depth_usd"])):
        return 0, "depth_filter"
    theta = Fraction(repr(p["entry_threshold"]))
    if feat["imbalance"] > theta:
        return 1, "long"
    if feat["imbalance"] < -theta:
        return -1, "short"
    return 0, "neutral"


def _sha(*parts) -> str:
    h = hashlib.sha256()
    for part in parts:
        b = str(part).encode("utf-8")
        h.update(str(len(b)).encode("ascii") + b":" + b + b";")
    return h.hexdigest()


def ceil_grid(ms: int) -> int:
    return -(-ms // DECISION_GRID_MS) * DECISION_GRID_MS


def episode_ids(coin: str, p: dict, boundary_id: str, direction: int, first_identity: str, crossing_identity: str, deadline: int) -> tuple:
    episode = _sha(CONTRACT["contract"], SOURCE_KEY.format(coin=coin), SPECIFICATION, params_json(p), boundary_id, direction, first_identity)
    return episode, _sha(episode, crossing_identity, deadline)


def decide(book: dict, deadline_ms: int, p: dict) -> dict:
    coin = book.get("coin", "")
    out = {"signal": 0, "reason": "", "deadline_ms": deadline_ms, "episode_id": "", "entry_id": "", "direction": 0,
           "confirmations": 0, "used": [], "boundary": "", "imbalance": None, "spread_bps": None, "B": None, "A": None,
           "total": None, "event_age_ms": None, "receipt_age_ms": None, "crossing_ms": None, "episode_deadline_ms": None,
           "params": params_json(p), "specification": SPECIFICATION, "contract": CONTRACT["contract"]}
    if not book.get("available"):
        out["reason"] = book.get("reason") or "book_unavailable"
        out["detail"] = book.get("detail", "")
        return out
    if deadline_ms % DECISION_GRID_MS:
        out["reason"] = "deadline_off_grid"
        return out
    if book.get("prefix_complete") is not True:
        out["reason"] = "evidence_truncated"
        out["detail"] = (f"the section history is complete only from {book.get('retained_from_ms')}, after its window start, "
                         "so no boundary before a run can be proven")
        return out
    window_start = deadline_ms - EVIDENCE_WINDOW_MS
    items = []
    for m in book["markers"]:
        if window_start <= m["at_ms"] <= deadline_ms:
            items.append((m["at_ms"], m["seq"], "m", m))
    for o in book["observations"]:
        if window_start <= o["recv_ms"] <= deadline_ms and o["event_ms"] <= deadline_ms:
            items.append((o["recv_ms"], o["seq"], "o", o))
    items.sort(key=lambda x: (x[0], x[1]))
    depth = p["depth_levels"]
    run = []
    direction = 0
    boundary = None
    proven = False
    feats = {}
    for _, _, kind, item in items:
        if kind == "m":
            run, direction = [], 0
            boundary, proven = f"marker:{item['seq']}:{item['reason']}:{item['at_ms']}", True
            continue
        feat = book_features(item, depth)
        d, _why = classify(feat, p)
        feats[item["identity"]] = feat
        if d == 0:
            run, direction = [], 0
            boundary, proven = f"book:{item['identity']}", True
            continue
        if d != direction:
            if direction != 0:
                boundary, proven = f"book:{run[-1]['identity']}", True
            run, direction = [item], d
            run_proven = proven
            run_boundary = boundary
            continue
        run.append(item)
    if not run:
        out["reason"] = "no_episode"
        return out
    out["direction"] = direction
    out["confirmations"] = len(run)
    if not run_proven:
        out["reason"] = "evidence_truncated"
        return out
    n = p["confirmation_observations"]
    if len(run) < n:
        out["reason"] = "confirming"
        out["boundary"] = run_boundary
        return out
    crossing = run[n - 1]
    c = max(crossing["event_ms"], crossing["recv_ms"])
    episode_deadline = ceil_grid(c)
    feat = feats[crossing["identity"]]
    episode, entry = episode_ids(coin, p, run_boundary, direction, run[0]["identity"], crossing["identity"], episode_deadline)
    out.update({"boundary": run_boundary, "episode_id": episode, "crossing_ms": c, "episode_deadline_ms": episode_deadline,
                "used": [o["identity"] for o in run[:n]], "imbalance": float(feat["imbalance"]), "spread_bps": float(feat["spread_bps"]),
                "B": float(feat["B"]), "A": float(feat["A"]), "total": float(feat["total"]),
                "event_age_ms": deadline_ms - crossing["event_ms"], "receipt_age_ms": deadline_ms - crossing["recv_ms"]})
    if deadline_ms > episode_deadline:
        out["reason"] = "consumed"
        return out
    if out["event_age_ms"] > p["max_snapshot_age_ms"] or out["receipt_age_ms"] > p["max_snapshot_age_ms"]:
        out["reason"] = "stale_crossing"
        return out
    out["entry_id"] = entry
    out["signal"] = direction
    out["reason"] = "entry_long" if direction > 0 else "entry_short"
    return out


def evaluate_section(section, coin: str, params: dict, deadline_ms=None, accept_time_semantics=None) -> dict:
    p = validate_params(params)
    kwargs = {"accept_time_semantics": accept_time_semantics} if accept_time_semantics else {}
    book = parse_book_input(section, coin, **kwargs)
    if deadline_ms is None:
        deadline_ms = book.get("cutoff_ms") if book.get("available") else 0
    return decide(book, int(deadline_ms or 0), p)


EVIDENCE_COLUMNS = ("obi_reason", "obi_episode_id", "obi_entry_id", "obi_boundary", "obi_params", "obi_specification", "obi_used_json")
NUMBER_COLUMNS = ("obi_deadline_ms", "obi_crossing_ms", "obi_episode_deadline_ms", "obi_confirmations", "obi_imbalance",
                  "obi_spread_bps", "obi_total", "obi_bid_notional", "obi_ask_notional", "obi_event_age_ms", "obi_receipt_age_ms")


def order_book_imbalance_core(df: pd.DataFrame, depth_levels: int = 5, entry_threshold: float = 0.30,
                              confirmation_observations: int = 3, max_spread_bps: float = 5.0,
                              min_combined_depth_usd: float = 100000.0, max_snapshot_age_ms: int = 2000,
                              l2_book_observations=None) -> pd.DataFrame:
    p = validate_params({"depth_levels": depth_levels, "entry_threshold": entry_threshold,
                         "confirmation_observations": confirmation_observations, "max_spread_bps": max_spread_bps,
                         "min_combined_depth_usd": min_combined_depth_usd, "max_snapshot_age_ms": max_snapshot_age_ms})
    if l2_book_observations is None:
        raise BookInputMissing(
            f"{STRATEGY} reads supplied l2Book depth observations (l2_book_observations); a candle-only run cannot "
            "score it, so this consumer is refused")
    result = df.copy()
    n = len(result)
    signal = np.zeros(n, dtype=np.int64)
    text = {c: [""] * n for c in EVIDENCE_COLUMNS}
    nums = {c: np.full(n, np.nan) for c in NUMBER_COLUMNS}
    for i in range(n):
        text["obi_reason"][i] = "not_decision_row"
    if n:
        section = l2_book_observations
        book_coin = (section.get("coin") if isinstance(section, dict) else "") or ""
        d = decide(parse_book_input(section, book_coin), int(section.get("cutoff_ms") or 0) if isinstance(section, dict) else 0, p)
        k = n - 1
        signal[k] = d["signal"]
        text["obi_reason"][k] = d["reason"]
        text["obi_episode_id"][k] = d["episode_id"]
        text["obi_entry_id"][k] = d["entry_id"]
        text["obi_boundary"][k] = d["boundary"]
        text["obi_params"][k] = d["params"]
        text["obi_specification"][k] = d["specification"]
        text["obi_used_json"][k] = json.dumps(d["used"], separators=(",", ":"))
        for col, key in (("obi_deadline_ms", "deadline_ms"), ("obi_crossing_ms", "crossing_ms"),
                         ("obi_episode_deadline_ms", "episode_deadline_ms"), ("obi_confirmations", "confirmations"),
                         ("obi_imbalance", "imbalance"), ("obi_spread_bps", "spread_bps"), ("obi_total", "total"),
                         ("obi_bid_notional", "B"), ("obi_ask_notional", "A"), ("obi_event_age_ms", "event_age_ms"),
                         ("obi_receipt_age_ms", "receipt_age_ms")):
            if d.get(key) is not None:
                nums[col][k] = float(d[key])
    result["signal"] = signal
    for c in EVIDENCE_COLUMNS:
        result[c] = text[c]
    for c in NUMBER_COLUMNS:
        result[c] = nums[c]
    return result

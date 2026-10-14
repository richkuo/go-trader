import json
import math
import os
import subprocess
import sys

import pytest

_REPO_ROOT = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", ".."))
RANKER = os.path.join(_REPO_ROOT, "shared_scripts", "run_basket_ranker.py")

H = 3_600_000
DEADLINE = 1_791_849_600_000
LISTED = 1_577_836_800_000
MARKS = {"AVAX": 30.0, "BTC": 60000.0, "DOGE": 0.2, "ETH": 3000.0, "LINK": 15.0, "SOL": 3000.0}
SERIES = {
    "BTC": (0.004, 0.005),
    "ETH": (0.003, 0.015),
    "SOL": (0.003, 0.004),
    "LINK": (0.0, 0.008),
    "AVAX": (-0.002, 0.01),
    "DOGE": (-0.003, 0.01),
}
CONTEXT = {"gross_budget_usd": 4000.0, "max_coin_notional_usd": 1200.0, "max_transition_net_usd": 1100.0,
           "lookback_bars": 48, "min_history_bars": 24}


def _closes(coin, bars):
    drift, swing = SERIES[coin]
    out = []
    for i in range(bars):
        back = bars - 1 - i
        sign = 1.0 if back % 2 == 0 else -1.0
        out.append(math.exp(math.log(MARKS[coin]) - back * drift + swing * (sign - 1)))
    return out


def _frame(coin, bars=60, stale=False):
    closes = _closes(coin, bars)
    rows, timing = [], []
    start = DEADLINE - bars * H
    for i, c in enumerate(closes):
        open_ms = start + i * H
        prev = closes[i - 1] if i else c
        rows.append([open_ms + H - 1, prev, max(prev, c) * 1.001, min(prev, c) * 0.999, c, 100.0 + i])
        timing.append([open_ms, open_ms + H - 1, True])
    last = closes[-1]
    rows.append([DEADLINE, last, last * 1.002, last * 0.998, last, 50.0])
    timing.append([DEADLINE, DEADLINE + H - 1, False])
    frame = {"rows": rows, "required": len(rows), "bars": len(rows), "coverage_short": False,
             "first_open_ms": start, "last_open_ms": DEADLINE, "last_close_ms": DEADLINE + H - 1,
             "last_recv_at_ms": DEADLINE, "source": "test", "ready": True, "forming_bar_included": True,
             "timing": {"rule": "hyperliquid_native_close", "interval_ms": H, "bars": timing}}
    if stale:
        frame.update({"stale": True, "stale_reason": "recorded gap"})
    return frame


def _request(ranker="cross_sectional_reversal", params=None, context=None, frames=None, listings=None,
             min_eligible=4):
    frames = {f"{c}|1h": _frame(c) for c in MARKS} if frames is None else frames
    market = {
        "version": 2, "snapshot_id": "test/1", "generation": 7, "sealed_at_ms": DEADLINE + 30_000,
        "frames": frames,
        "mids": {c: {"px": MARKS[c], "recv_at_ms": DEADLINE + 30_000, "source": "test", "age_ms": 0,
                     "stale": False, "confirmed": True} for c in MARKS},
        "feed_complete": True, "decision_cutoff_ms": DEADLINE,
    }
    base_listings = {c: {"listed_at_ms": LISTED, "known_at_ms": LISTED} for c in MARKS}
    base_listings.update(listings or {})
    return {
        "v": 2, "basket_id": "xs-rev", "ranker": ranker, "params": {"quantile": 0.4} if params is None else params,
        "deadline_ms": DEADLINE, "expected": {"snapshot_id": "test/1", "generation": 7, "cutoff_ms": DEADLINE},
        "members": [{"symbol": c, "timeframe": "1h"} for c in sorted(MARKS)],
        "allocation_context": {**CONTEXT, **(context or {})}, "min_eligible": min_eligible,
        "listings": base_listings, "mode": "paper", "market": market,
    }


def _rank(req):
    proc = subprocess.run([sys.executable, RANKER], input=json.dumps(req), capture_output=True, text=True,
                          cwd=_REPO_ROOT, timeout=240)
    try:
        out = json.loads(proc.stdout)
    except ValueError:
        raise AssertionError(proc.stdout + proc.stderr)
    return proc.returncode, out


def test_reversal_runner_longs_the_bottom_tail_and_shorts_the_top_tail():
    code, rev = _rank(_request())
    assert code == 0 and rev["status"] == "targets", rev
    code, mom = _rank(_request(ranker="cross_sectional_momentum"))
    assert code == 0 and mom["status"] == "targets", mom
    weights, scores = rev["weights"], rev["scores"]
    assert set(weights) == set(MARKS) and set(scores) == set(MARKS)
    assert rev["exclusions"] == {}
    assert scores == mom["scores"]
    for coin, (drift, _) in SERIES.items():
        assert scores[coin] == pytest.approx(math.exp(24 * drift) - 1, rel=1e-9)
    assert scores["ETH"] == scores["SOL"]
    order = sorted(scores, key=lambda s: (-scores[s], s))
    assert order == ["BTC", "ETH", "SOL", "LINK", "AVAX", "DOGE"]
    assert {c for c, w in weights.items() if w < 0} == {"BTC", "ETH"}
    assert {c for c, w in weights.items() if w > 0} == {"AVAX", "DOGE"}
    assert weights["LINK"] == 0.0 and weights["SOL"] == 0.0
    assert {c: -w for c, w in mom["weights"].items()} == weights
    longs = [w for w in weights.values() if w > 0]
    shorts = [w for w in weights.values() if w < 0]
    assert math.fsum(longs) == 0.5 and math.fsum(shorts) == -0.5 and math.fsum(weights.values()) == 0.0
    cap = CONTEXT["max_coin_notional_usd"] / CONTEXT["gross_budget_usd"]
    expected = {"BTC": -cap, "ETH": -(0.5 - cap), "AVAX": 0.25, "DOGE": 0.25, "LINK": 0.0, "SOL": 0.0}
    assert weights == pytest.approx(expected, abs=1e-12)
    assert all(abs(w) <= cap for w in weights.values())
    assert rev["input_fingerprint"] != mom["input_fingerprint"] and len(rev["input_fingerprint"]) == 64


def _without(coin):
    return {f"{c}|1h": _frame(c) for c in MARKS if c != coin}


def _with(coin, frame):
    frames = {f"{c}|1h": _frame(c) for c in MARKS}
    frames[f"{coin}|1h"] = frame
    return frames


DELISTED = {"LINK": {"listed_at_ms": LISTED, "known_at_ms": DEADLINE - H, "delisted_at_ms": DEADLINE - H}}

DATA = [
    ("short frame", {"frames": _with("BTC", _frame("BTC", bars=5))}, "BTC: the sealed frame carries"),
    ("missing frame", {"frames": _without("ETH")}, "no frame for ETH|1h"),
    ("stale frame", {"frames": _with("SOL", _frame("SOL", stale=True))}, "SOL: the sealed frame is stale"),
    ("eligible population below the minimum", {"frames": _without("LINK"), "listings": DELISTED, "min_eligible": 6},
     "the eligible population 5 is below the minimum 6"),
]

CONFIGS = [
    ("boolean return lookback", {"params": {"quantile": 0.4, "return_lookback_bars": True}},
     "cross_sectional_reversal: return_lookback_bars must be an integer"),
    ("quantile at one half", {"params": {"quantile": 0.5}}, "constraint 'quantile < 0.5' violated"),
]


@pytest.mark.parametrize("config_name, config, invalid", CONFIGS, ids=[c[0] for c in CONFIGS])
def test_reversal_refuses_invalid_configuration_before_member_data(config_name, config, invalid):
    code, out = _rank(_request(frames=_with("SOL", _frame("SOL", stale=True)), **config))
    assert code == 1, out
    assert out["status"] == "invalid", out
    assert invalid in out["error"], out["error"]
    assert "weights" not in out and "hold_reason" not in out


@pytest.mark.parametrize("data_name, data, hold", DATA, ids=[d[0] for d in DATA])
def test_reversal_valid_configuration_holds_on_the_same_member_data(data_name, data, hold):
    code, held = _rank(_request(**data))
    assert code == 0 and held["status"] == "hold" and held["v"] == 2, held
    assert hold in held["hold_reason"], held["hold_reason"]
    assert held["weights"] == {} and held["scores"] == {} and held["exclusions"] == {}


def _bad_closes(coin, edit):
    frame = _frame(coin)
    edit(frame["rows"])
    return frame


def _set_close(index, value):
    def edit(rows):
        rows[index][4] = value
    return edit


def _flat(rows):
    for row in rows:
        row[1:5] = [15.0, 15.0, 15.0, 15.0]


def _explosive(rows):
    for row in rows[:-1]:
        row[1:5] = [1e-300, 1e-300, 1e-300, 1e-300]
    rows[-2][1:5] = [1e-300, 1e300, 1e-300, 1e300]


HOLDS = [
    ("short eligible history", {"frames": _with("LINK", _frame("LINK", bars=20)), "context": {"min_history_bars": 10},
                                "listings": {"LINK": {"listed_at_ms": DEADLINE - 20 * H, "known_at_ms": DEADLINE - 20 * H}}},
     "LINK: the frame carries 20 closed bars, cross_sectional_reversal needs 25"),
    ("non-positive price on a middle member", {"frames": _with("SOL", _bad_closes("SOL", _set_close(50, -1.0)))},
     "non-positive"),
    ("non-finite price on a selected member", {"frames": _with("DOGE", _bad_closes("DOGE", _set_close(55, float("inf"))))},
     "non-finite"),
    ("zero volatility on a middle member", {"frames": _with("LINK", _bad_closes("LINK", _flat))},
     "LINK: the trailing volatility 0.0 is zero or not finite"),
    ("overflowed trailing return on a selected member", {"frames": _with("DOGE", _bad_closes("DOGE", _explosive))},
     "DOGE: the trailing return is not finite"),
    ("infeasible side under the cap", {"context": {"max_coin_notional_usd": 900.0}}, "cannot reach 0.5"),
]


@pytest.mark.parametrize("name, data, reason", HOLDS, ids=[h[0] for h in HOLDS])
def test_reversal_holds_the_whole_set_on_bad_member_data(name, data, reason):
    code, out = _rank(_request(**data))
    assert code == 0 and out["status"] == "hold", out
    assert reason in out["hold_reason"], out["hold_reason"]
    assert out["weights"] == {} and out["scores"] == {} and out["exclusions"] == {}

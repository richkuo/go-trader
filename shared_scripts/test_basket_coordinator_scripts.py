import copy
import json
import math
import os
import subprocess
import sys

import pytest

_REPO_ROOT = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
RANKER = os.path.join(_REPO_ROOT, "shared_scripts", "run_basket_ranker.py")
CHECK_HL = os.path.join(_REPO_ROOT, "shared_scripts", "check_hyperliquid.py")
CLOSE_HL = os.path.join(_REPO_ROOT, "shared_scripts", "close_hyperliquid_position.py")

H = 3_600_000
DEADLINE = 1_791_849_600_000
MARKS = {"AVAX": 30.0, "BTC": 60000.0, "ETH": 3000.0, "SOL": 150.0}
WEIGHTS = {"AVAX": -0.25, "BTC": 0.25, "ETH": 0.25, "SOL": -0.25}
CONTEXT = {"gross_budget_usd": 4000.0, "max_coin_notional_usd": 1500.0, "max_transition_net_usd": 400.0,
           "lookback_bars": 48, "min_history_bars": 24}


def _env():
    env = dict(os.environ)
    env["HYPERLIQUID_SECRET_KEY"] = ""
    env["HYPERLIQUID_ACCOUNT_ADDRESS"] = ""
    env["GO_TRADER_HL_OHLCV_CACHE"] = "0"
    return env


def _run(script, argv, payload):
    stdin = payload if isinstance(payload, str) else json.dumps(payload)
    return subprocess.run([sys.executable, script] + argv, input=stdin, capture_output=True, text=True,
                          cwd=_REPO_ROOT, env=_env(), timeout=240)


def _frame(coin, bars=60, deadline=DEADLINE, forming=True):
    mark = MARKS[coin]
    rows, timing = [], []
    start = deadline - bars * H
    total = bars + (1 if forming else 0)
    for i in range(total):
        open_ms = start + i * H
        px = mark * (1 + 0.002 * math.sin(i / 3))
        closed = i < bars
        ts = open_ms + H - 1 if closed else open_ms
        rows.append([ts, px, px * 1.004, px * 0.996, px * 1.001, 100.0 + i])
        timing.append([open_ms, open_ms + H - 1, closed])
    return {"rows": rows, "required": len(rows), "bars": len(rows), "coverage_short": False,
            "first_open_ms": start, "last_open_ms": start + (total - 1) * H,
            "last_close_ms": start + total * H - 1, "last_recv_at_ms": deadline, "source": "test",
            "ready": True, "forming_bar_included": forming,
            "timing": {"rule": "hyperliquid_native_close", "interval_ms": H, "bars": timing}}


def _market(coins=tuple(MARKS), cutoff=DEADLINE):
    return {
        "version": 1, "snapshot_id": "test/1", "generation": 7, "sealed_at_ms": cutoff + 30_000,
        "frames": {f"{c}|1h": _frame(c) for c in coins},
        "mids": {c: {"px": MARKS[c], "recv_at_ms": cutoff + 30_000, "source": "test", "age_ms": 0,
                     "stale": False, "confirmed": True} for c in coins},
        "feed_complete": True, "decision_cutoff_ms": cutoff,
    }


def _request(**overrides):
    req = {
        "v": 2, "basket_id": "xs-test", "ranker": "basket_scripted_fixture",
        "params": {"weights": dict(WEIGHTS)}, "deadline_ms": DEADLINE,
        "expected": {"snapshot_id": "test/1", "generation": 7, "cutoff_ms": DEADLINE},
        "members": [{"symbol": c, "timeframe": "1h"} for c in sorted(MARKS)],
        "allocation_context": dict(CONTEXT), "min_eligible": 3,
        "listings": {c: {"listed_at_ms": 1_577_836_800_000, "known_at_ms": 1_577_836_800_000} for c in MARKS},
        "mode": "paper", "market": _market(),
    }
    req.update(overrides)
    return req


def _rank(req):
    proc = _run(RANKER, [], req)
    try:
        out = json.loads(proc.stdout)
    except ValueError:
        raise AssertionError(proc.stdout + proc.stderr)
    return proc.returncode, out


def test_ranker_freezes_validated_targets_for_every_eligible_member():
    code, out = _rank(_request())
    assert code == 0, out
    assert out["status"] == "targets"
    assert out["weights"] == WEIGHTS
    assert set(out["eligibility"]) == set(MARKS)
    for sym, ev in out["eligibility"].items():
        assert ev["status"] == "eligible"
        assert ev["boundary_ms"] == DEADLINE
        assert ev["closed_bars"] == 48
        assert ev["mark"] == _frame(sym)["rows"][59][4]
    assert out["snapshot_id"] == "test/1" and out["generation"] == 7 and out["cutoff_ms"] == DEADLINE
    assert len(out["input_fingerprint"]) == 64
    _, again = _rank(_request())
    assert again["input_fingerprint"] == out["input_fingerprint"]


def _mutated_market(edit):
    market = _market()
    edit(market)
    return _request(market=market)


@pytest.mark.parametrize("name, req, reason", [
    ("missing frame", lambda: _mutated_market(lambda m: m["frames"].pop("SOL|1h")), "no frame for SOL|1h"),
    ("stale frame", lambda: _mutated_market(lambda m: m["frames"]["SOL|1h"].update({"stale": True, "stale_reason": "gap"})), "stale"),
    ("not ready", lambda: _mutated_market(lambda m: m["frames"]["SOL|1h"].update({"ready": False})), "not ready"),
    ("non-finite price", lambda: _mutated_market(lambda m: m["frames"]["ETH|1h"]["rows"][-5].__setitem__(4, float("nan"))), "non-finite"),
    ("non-positive price", lambda: _mutated_market(lambda m: m["frames"]["ETH|1h"]["rows"][-5].__setitem__(4, -1.0)), "non-positive"),
    ("malformed row", lambda: _mutated_market(lambda m: m["frames"]["BTC|1h"]["rows"].__setitem__(3, [1, 2])), "malformed"),
    ("missing timing", lambda: _mutated_market(lambda m: m["frames"]["BTC|1h"].pop("timing")), "no per-row timing"),
    ("short history", lambda: _mutated_market(lambda m: m["frames"].__setitem__("BTC|1h", _frame("BTC", bars=20))), "closed bars, needs 48"),
    ("wrong generation", lambda: _mutated_market(lambda m: m.__setitem__("generation", 8)), "generation"),
    ("wrong snapshot", lambda: _mutated_market(lambda m: m.__setitem__("snapshot_id", "test/2")), "snapshot"),
    ("wrong cutoff", lambda: _mutated_market(lambda m: m.__setitem__("decision_cutoff_ms", DEADLINE + 1000)), "cutoff"),
    ("incomplete feed", lambda: _mutated_market(lambda m: m.__setitem__("feed_complete", False)), "not complete"),
    ("missing listing", lambda: _request(listings={c: v for c, v in _request()["listings"].items() if c != "ETH"}), "no listing evidence"),
    ("listing not yet known", lambda: _request(listings={**_request()["listings"], "ETH": {"listed_at_ms": 1, "known_at_ms": DEADLINE + 1}}), "not known at the deadline"),
    ("small population", lambda: _request(min_eligible=5), "below the minimum 5"),
    ("empty short side", lambda: _request(params={"weights": {"BTC": 0.5, "ETH": 0.5}}), "short side is empty"),
    ("ranker hold", lambda: _request(params={"hold": "regime unclear"}), "regime unclear"),
])
def test_ranker_holds_the_whole_target_set(name, req, reason):
    code, out = _rank(req())
    assert code == 0, out
    assert out["status"] == "hold", out
    assert reason in out["hold_reason"], out["hold_reason"]
    assert out["weights"] == {}


def test_ranker_records_known_ineligibility_as_an_exclusion():
    listings = dict(_request()["listings"])
    listings["AVAX"] = {"listed_at_ms": DEADLINE - 10 * H, "known_at_ms": DEADLINE - 10 * H}
    listings["SOL"] = {"listed_at_ms": 1, "known_at_ms": 1, "delisted_at_ms": DEADLINE - H}
    market = _market()
    market["frames"]["AVAX|1h"] = _frame("AVAX", bars=10)
    weights = {"BTC": 0.4, "ETH": -0.4}
    code, out = _rank(_request(listings=listings, market=market, min_eligible=2, params={"weights": weights}))
    assert code == 0, out
    assert out["status"] == "targets", out
    assert out["eligibility"]["AVAX"]["status"] == "excluded" and "needs 24" in out["eligibility"]["AVAX"]["reason"]
    assert out["eligibility"]["SOL"]["status"] == "excluded" and "delisted" in out["eligibility"]["SOL"]["reason"]
    assert set(out["weights"]) == {"BTC", "ETH"}


@pytest.mark.parametrize("listing, reason", [
    ({"listed_at_ms": 1, "known_at_ms": 1, "delisted_at_ms": DEADLINE - H}, "delisted at or before the deadline"),
    ({"listed_at_ms": DEADLINE + H, "known_at_ms": DEADLINE - H}, "not listed at the deadline"),
    ({"listed_at_ms": DEADLINE - 5 * H, "known_at_ms": DEADLINE - 5 * H}, "listed 5 bars before the deadline, needs 24"),
])
def test_listing_excluded_member_needs_no_frame_or_mark(listing, reason):
    listings = {**_request()["listings"], "SOL": listing}
    market = _market(coins=("AVAX", "BTC", "ETH"))
    weights = {"BTC": 0.25, "ETH": -0.15, "AVAX": -0.1}
    code, out = _rank(_request(listings=listings, market=market, params={"weights": weights}))
    assert code == 0, out
    assert out["status"] == "targets", out
    assert out["eligibility"]["SOL"]["status"] == "excluded"
    assert out["eligibility"]["SOL"]["reason"] == reason
    assert "closed_bars" not in out["eligibility"]["SOL"]
    assert set(out["weights"]) == {"AVAX", "BTC", "ETH"}
    for sym in ("AVAX", "BTC", "ETH"):
        assert out["eligibility"][sym]["status"] == "eligible"


def test_listing_excluded_member_ignores_a_malformed_frame():
    listings = {**_request()["listings"], "SOL": {"listed_at_ms": 1, "known_at_ms": 1, "delisted_at_ms": DEADLINE - H}}
    market = _market()
    market["frames"]["SOL|1h"]["stale"] = True
    code, out = _rank(_request(listings=listings, market=market, params={"weights": {"BTC": 0.25, "ETH": -0.15, "AVAX": -0.1}}))
    assert code == 0, out
    assert out["status"] == "targets", out
    assert out["eligibility"]["SOL"]["status"] == "excluded"


@pytest.mark.parametrize("raw, reason", [
    ({"scores": {"BTC": 1}, "weights": {"BTC": 0.5}}, "covers"),
    ({"scores": {s: 1 for s in MARKS}, "weights": {**WEIGHTS, "DOGE": 0.1}}, "covers"),
    ({"scores": {**{s: 1 for s in MARKS}, "BTC": float("inf")}, "weights": WEIGHTS}, "not a finite number"),
    ({"scores": {s: 1 for s in MARKS}, "weights": {**WEIGHTS, "BTC": "0.25"}}, "not a finite number"),
    ({"scores": {s: 1 for s in MARKS}, "weights": {s: 0.4 * (1 if w > 0 else -1) for s, w in WEIGHTS.items()}}, "above 1"),
    ({"scores": {s: 1 for s in MARKS}, "weights": WEIGHTS, "exclusions": {"BTC": "x"}}, "assigned it weight"),
    ({"scores": {s: 1 for s in MARKS}, "weights": WEIGHTS, "extra": 1}, "unknown keys"),
    (["not", "an", "object"], "must be an object"),
])
def test_ranker_refuses_invalid_output(raw, reason):
    code, out = _rank(_request(params={"raw": raw}))
    assert code == 1
    assert out["status"] == "invalid"
    assert reason in out["error"], out["error"]


def test_ranker_refuses_unknown_params_and_live_mode():
    code, out = _rank(_request(params={"weights": WEIGHTS, "momentum_window": 3}))
    assert code == 1 and "unknown ranker params" in out["error"]
    code, out = _rank(_request(mode="live"))
    assert code == 1 and "explicit paper mode" in out["error"]
    code, out = _rank(_request(ranker="sma_crossover"))
    assert code == 1 and "not a registered basket evaluator" in out["error"]


def test_momentum_preflight_refuses_a_short_lookback_before_member_data():
    def momentum(lookback):
        market = _market()
        market["frames"].pop("SOL|1h")
        return _request(ranker="cross_sectional_momentum", params={}, market=market,
                        allocation_context={**CONTEXT, "lookback_bars": lookback, "min_history_bars": 24})
    code, held = _rank(momentum(48))
    assert code == 0 and held["status"] == "hold" and held["v"] == 2, held
    assert "no frame for SOL|1h" in held["hold_reason"], held["hold_reason"]
    code, out = _rank(momentum(24))
    assert code == 1, out
    assert out["status"] == "invalid", out
    assert "lookback_bars 24 cannot supply" in out["error"], out["error"]
    assert "the ranker needs 25 closed bars" in out["error"], out["error"]


def _without(key):
    req = _request()
    req.pop(key)
    return req


def _context(**edits):
    ctx = dict(CONTEXT)
    ctx.update(edits)
    return _request(allocation_context=ctx)


@pytest.mark.parametrize("name, req, reason", [
    ("version 1", lambda: _request(v=1), "request version 1 is not 2"),
    ("old top-level lookback", lambda: _request(lookback_bars=48), "moved into allocation_context"),
    ("old top-level minimum history", lambda: _request(min_history_bars=24), "moved into allocation_context"),
    ("missing context", lambda: _without("allocation_context"), "'allocation_context' must be an object"),
    ("missing context field", lambda: _request(allocation_context={k: v for k, v in CONTEXT.items() if k != "max_transition_net_usd"}), "missing fields ['max_transition_net_usd']"),
    ("unknown context field", lambda: _context(max_leverage=3), "unknown fields ['max_leverage']"),
    ("boolean budget", lambda: _context(gross_budget_usd=True), "gross_budget_usd must be a finite positive number"),
    ("non-finite cap", lambda: _context(max_coin_notional_usd=float("inf")), "max_coin_notional_usd must be a finite positive number"),
    ("non-positive net bound", lambda: _context(max_transition_net_usd=0), "max_transition_net_usd must be a finite positive number"),
    ("cap above gross", lambda: _context(max_coin_notional_usd=4000.5), "max_coin_notional_usd 4000.5 exceeds gross_budget_usd"),
    ("net above gross", lambda: _context(max_transition_net_usd=5000), "max_transition_net_usd 5000.0 exceeds gross_budget_usd"),
    ("boolean lookback", lambda: _context(lookback_bars=True), "lookback_bars must be an integer in [2, 5000]"),
    ("lookback above the bound", lambda: _context(lookback_bars=5001), "lookback_bars must be an integer in [2, 5000]"),
    ("fractional minimum history", lambda: _context(min_history_bars=2.5), "min_history_bars must be an integer in [1, lookback_bars]"),
    ("minimum history above lookback", lambda: _context(min_history_bars=49), "min_history_bars must be an integer in [1, lookback_bars]"),
])
def test_ranker_refuses_malformed_version_2_requests_before_evaluation(name, req, reason):
    payload = req()
    payload["params"] = {"hold": "the evaluator ran"}
    code, out = _rank(payload)
    assert code == 1, out
    assert out["status"] == "invalid", out
    assert reason in out["error"], out["error"]
    assert "the evaluator ran" not in json.dumps(out)


def test_ranker_echoes_version_2_and_fingerprints_the_whole_context():
    code, base = _rank(_request())
    assert code == 0 and base["status"] == "targets" and base["v"] == 2, base
    code, held = _rank(_request(params={"hold": "regime unclear"}))
    assert code == 0 and held["status"] == "hold" and held["v"] == 2, held
    assert len(held["input_fingerprint"]) == 64
    for key, value in (("gross_budget_usd", 4100.0), ("max_coin_notional_usd", 1400.0),
                       ("max_transition_net_usd", 300.0), ("lookback_bars", 47), ("min_history_bars", 23)):
        code, out = _rank(_context(**{key: value}))
        assert code == 0 and out["status"] == "targets" and out["v"] == 2, (key, out)
        assert len(out["input_fingerprint"]) == 64
        assert out["input_fingerprint"] != base["input_fingerprint"], key


_INSTRUMENTED_RUNNER = """
import json, os, sys
sys.path.insert(0, os.path.join({root!r}, "shared_scripts"))
import run_basket_ranker as runner

real_load = runner._load_registry


def instrumented(frames, eligibility, allocation_context, **params):
    seen = {{"context": dict(allocation_context), "params": sorted(params)}}
    try:
        allocation_context[{mutate!r}] = 1
    except TypeError as e:
        seen["refused"] = type(e).__name__
    else:
        seen["refused"] = ""
    return {{"hold": json.dumps(seen, sort_keys=True)}}


def load():
    registry = real_load()
    entry = registry.get_basket_ranker("basket_scripted_fixture")
    entry["basket_fn"] = instrumented
    return registry


runner._load_registry = load
runner.main()
"""


@pytest.mark.parametrize("field", ["gross_budget_usd", "lookback_bars"])
def test_ranker_passes_a_read_only_context_apart_from_params(tmp_path, field):
    script = tmp_path / "instrumented_ranker.py"
    script.write_text(_INSTRUMENTED_RUNNER.format(root=_REPO_ROOT, mutate=field))
    proc = _run(str(script), [], _request(params={"weights": dict(WEIGHTS)}))
    out = json.loads(proc.stdout)
    assert proc.returncode == 0 and out["status"] == "hold", proc.stdout + proc.stderr
    seen = json.loads(out["hold_reason"].split("the ranker held: ", 1)[1])
    assert seen["refused"] == "TypeError"
    assert seen["context"] == CONTEXT
    assert seen["params"] == ["hold", "raw", "scores", "sleep_seconds", "weights"]
    assert set(seen["params"]).isdisjoint(CONTEXT)
    code, refused = _rank(_request(params={"weights": dict(WEIGHTS), field: 1}))
    assert code == 1 and refused["status"] == "invalid" and f"unknown ranker params ['{field}']" in refused["error"]


def test_ranker_probe_exits_cleanly_without_input():
    proc = subprocess.run([sys.executable, RANKER, "--probe-only"], capture_output=True, text=True,
                          cwd=_REPO_ROOT, env=_env(), timeout=60)
    assert proc.returncode == 0, proc.stderr


def _member_argv(extra=()):
    refs = json.dumps({"open": {"name": "basket_member"}, "closes": [{"name": "tiered_tp_pct", "params": {"tp_tiers": [{"profit_pct": 0.05, "close_fraction": 1.0}]}}]})
    return ["basket_member", "BTC", "1h", "--mode=paper", "--market-stdin", "--closed-bar-decisions",
            "--strategy-refs", refs, "--basket-member", *extra]


def _member_payload():
    return {"v": 2, "market": _market(coins=("BTC",), cutoff=DEADLINE + 30_000)}


def test_member_check_runs_management_only_and_echoes_the_contract():
    proc = _run(CHECK_HL, _member_argv(), _member_payload())
    assert proc.returncode == 0, proc.stderr + proc.stdout
    out = json.loads(proc.stdout)
    assert out["basket_member"] is True
    assert out["open_action"] == "none"
    assert out["signal"] == 0
    assert out["closed_bar_decision"]["decision_boundary_ms"] == DEADLINE
    assert out["indicators"]["atr"] > 0


def test_member_check_closes_from_its_native_close_strategy():
    argv = _member_argv(["--position-side", "long", "--position-avg-cost=40000", "--position-qty=0.01",
                         "--position-initial-qty=0.01"])
    proc = _run(CHECK_HL, argv, _member_payload())
    assert proc.returncode == 0, proc.stderr + proc.stdout
    out = json.loads(proc.stdout)
    assert out["basket_member"] is True
    assert out["close_fraction"] == 1.0
    assert out["signal"] == -1


@pytest.mark.parametrize("argv, reason", [
    (["basket_member", "BTC", "1h", "--mode=paper", "--market-stdin", "--closed-bar-decisions",
      "--strategy-refs", json.dumps({"open": {"name": "basket_member"}, "closes": [{"name": "rsi"}]}), "--basket-member"],
     "not a native close strategy"),
    (["basket_member", "BTC", "1h", "--mode=paper", "--market-stdin", "--closed-bar-decisions",
      "--strategy-refs", json.dumps({"open": {"name": "basket_member"}}), "--basket-member"],
     "explicit close strategy"),
    (["rsi", "BTC", "1h", "--mode=paper", "--market-stdin", "--closed-bar-decisions",
      "--strategy-refs", json.dumps({"open": {"name": "rsi"}, "closes": [{"name": "tiered_tp_pct"}]}), "--basket-member"],
     "positional strategy"),
    (["basket_member", "BTC", "1h", "--mode=paper", "--market-stdin",
      "--strategy-refs", json.dumps({"open": {"name": "basket_member"}, "closes": [{"name": "tiered_tp_pct"}]}), "--basket-member"],
     "needs closed_bar_decisions"),
    (["basket_member", "BTC", "1h", "--mode=paper", "--market-stdin", "--closed-bar-decisions",
      "--strategy-refs", json.dumps({"open": {"name": "basket_member"}, "closes": [{"name": "tiered_tp_pct"}]})],
     "Unknown strategy"),
    (["basket_scripted_fixture", "BTC", "1h", "--mode=paper", "--market-stdin"], "Unknown strategy"),
    (["cross_sectional_momentum", "BTC", "1h", "--mode=paper", "--market-stdin"], "Unknown strategy"),
    (["cross_sectional_reversal", "BTC", "1h", "--mode=paper", "--market-stdin"], "Unknown strategy"),
])
def test_member_and_single_symbol_paths_refuse_before_evaluation(argv, reason):
    proc = _run(CHECK_HL, argv, _member_payload())
    assert proc.returncode == 1, proc.stdout
    out = json.loads(proc.stdout)
    assert reason in out["error"], out["error"]
    assert out["signal"] == 0


def test_batch_member_slot_runs_management_only():
    refs = {"open": {"name": "basket_member"}, "closes": [{"name": "tiered_tp_pct", "params": {"tp_tiers": [{"profit_pct": 0.05, "close_fraction": 1.0}]}}]}
    slots = [
        {"id": "hl-bsk-btc", "strategy": "basket_member", "mode": "paper", "mode_args": ["--mode=paper"],
         "strategy_refs": refs, "closed_bar_decisions": True, "basket_member": True},
        {"id": "hl-bad", "strategy": "basket_member", "mode": "paper", "mode_args": ["--mode=paper"],
         "strategy_refs": refs, "closed_bar_decisions": True, "basket_member": "yes"},
    ]
    payload = {"v": 2, "slots": slots[:1], "market": _member_payload()["market"]}
    argv = ["--batch-check", "--symbol=BTC", "--timeframe=1h", "--ohlcv-limit", "60", "--atr-method=simple", "--market-stdin"]
    proc = _run(CHECK_HL, argv, payload)
    assert proc.returncode == 0, proc.stderr + proc.stdout
    result = json.loads(proc.stdout)["results"][0]
    assert result["basket_member"] is True and result["signal"] == 0 and result["open_action"] == "none"
    bad = dict(payload, slots=slots[1:])
    proc = _run(CHECK_HL, argv, bad)
    assert proc.returncode == 1
    assert "basket_member must be a JSON boolean" in proc.stdout


def test_sized_close_cloid_contract_is_validated_before_any_order():
    base = ["--symbol=BTC", "--mode=live", "--sz=0.01", "--side=sell", "--close-mode=reduce_only"]
    proc = subprocess.run([sys.executable, CLOSE_HL, *base, "--cloid=0x" + "0" * 32, "--probe-only"],
                          capture_output=True, text=True, cwd=_REPO_ROOT, env=_env(), timeout=60)
    assert proc.returncode == 0, proc.stderr
    proc = subprocess.run([sys.executable, CLOSE_HL, *base, "--cloid=not-a-cloid"],
                          capture_output=True, text=True, cwd=_REPO_ROOT, env=_env(), timeout=60)
    out = json.loads(proc.stdout)
    assert out["order_outcome"] == "not_sent"
    assert "--cloid" in out["error"]
    proc = subprocess.run([sys.executable, CLOSE_HL, "--symbol=BTC", "--mode=live", "--cloid=0x" + "0" * 32],
                          capture_output=True, text=True, cwd=_REPO_ROOT, env=_env(), timeout=60)
    assert proc.returncode == 1
    out = json.loads(proc.stdout)
    assert out["order_outcome"] == "not_sent" and "requires --close-mode" in out["error"]

import json
import os
import subprocess
import sys

import pytest

REPO = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
SCRIPTS = os.path.join(REPO, "shared_scripts")
CREDENTIAL_PREFIXES = ("HYPERLIQUID", "OKX", "ROBINHOOD", "TOPSTEP", "PROJECTX")

SOLO_SCRIPTS = [
    ("check_hyperliquid.py", ["vortex_trend", "BTC", "4h"]),
    ("check_topstep.py", ["rsi", "ES", "1h"]),
    ("check_okx.py", ["rsi", "BTC", "1h", "--inst-type=swap"]),
    ("check_robinhood.py", ["rsi", "BTC", "1h"]),
    ("check_strategy.py", ["rsi", "BTC/USDT", "1h"]),
]

REFUSED_INPUTS = [
    (["--mode=live"], "explicit live mode"),
    (["--mode", "live"], "explicit live mode"),
    ([], "missing mode"),
    (["--mode=paper", "--mode=paper"], "invalid mode input"),
    (["--mode=paper", "--mode", "live"], "invalid mode input"),
    (["--mode="], "invalid mode input"),
    (["--mode=Paper"], "invalid mode input"),
    (["--mode=live", "--allow-no-edge=1"], "invalid acknowledgement"),
    (["--mode=live", "--allow-no-edge=true"], "invalid acknowledgement"),
    (["--mode=paper", "--allow-no-edge", "--allow-no-edge"], "invalid acknowledgement"),
]


def _env():
    return {k: v for k, v in os.environ.items() if not k.startswith(CREDENTIAL_PREFIXES)}


def _run(script, argv, stdin=None):
    return subprocess.run(
        [sys.executable, os.path.join(SCRIPTS, script)] + argv,
        input=None if stdin is None else json.dumps(stdin),
        capture_output=True, text=True, cwd=REPO, env=_env(), timeout=180,
    )


def _payload(proc):
    return json.loads(proc.stdout.strip().splitlines()[-1])


def _candles(n=220, start_ms=1_700_000_000_000, step_ms=14_400_000):
    out = []
    price = 100.0
    for i in range(n):
        drift = 0.9 if (i // 25) % 2 == 0 else -0.8
        price = max(price + drift + (1.4 if i % 5 == 0 else -0.6), 5.0)
        out.append([start_ms + i * step_ms, price - 0.4, price + 1.5, price - 1.5, price, 1000.0 + i])
    return out


def _market(rows, mid):
    return {
        "version": 1,
        "snapshot_id": "14400s/1",
        "generation": 1,
        "sealed_at_ms": rows[-1][0],
        "frames": {"BTC|4h": {
            "rows": rows, "required": 200, "bars": len(rows), "coverage_short": False,
            "first_open_ms": rows[0][0], "last_open_ms": rows[-1][0], "last_close_ms": rows[-1][0],
            "last_recv_at_ms": rows[-1][0], "source": "ws", "ready": True, "forming_bar_included": True,
        }},
        "mids": {"BTC": {"px": mid, "recv_at_ms": rows[-1][0], "source": "ws", "age_ms": 0,
                         "stale": False, "confirmed": True}},
        "feed_complete": True,
    }


def _solo_envelope():
    rows = _candles()
    return {"v": 2, "market": _market(rows, rows[-1][4])}


@pytest.mark.parametrize("script,positional", SOLO_SCRIPTS)
@pytest.mark.parametrize("extra,reason", REFUSED_INPUTS)
def test_every_check_script_refuses_no_edge_outside_explicit_paper(script, positional, extra, reason):
    proc = _run(script, positional + extra)
    assert proc.returncode == 1, proc.stderr[-2000:]
    payload = _payload(proc)
    assert payload["signal"] == 0
    assert payload["strategy"] == positional[0]
    assert f"open strategy '{positional[0]}' is edge_status=no_edge" in payload["error"]
    assert reason in payload["error"]


@pytest.mark.parametrize("script,positional", [s for s in SOLO_SCRIPTS if s[0] != "check_strategy.py"])
def test_dangling_mode_never_reaches_evaluation(script, positional):
    proc = _run(script, positional + ["--mode"])
    assert proc.returncode == 2
    assert "--mode: expected one argument" in proc.stderr
    assert proc.stdout.strip() == ""


def test_spot_dangling_mode_is_refused_as_invalid_input():
    proc = _run("check_strategy.py", ["rsi", "BTC/USDT", "1h", "--mode"])
    assert proc.returncode == 1
    assert "dangling --mode" in _payload(proc)["error"]


def test_close_fallback_reference_is_gated_and_native_close_is_not():
    refs_fallback = json.dumps({"open": {"name": "breakout"}, "closes": [{"name": "rsi"}]})
    refused = _run("check_hyperliquid.py", ["breakout", "BTC", "4h", "--mode=live", "--strategy-refs", refs_fallback])
    assert refused.returncode == 1
    error = _payload(refused)["error"]
    assert "close strategy 'rsi' is edge_status=no_edge" in error and "explicit live mode" in error

    refs_native = json.dumps({"open": {"name": "breakout"}, "closes": [{"name": "tiered_tp_atr"}]})
    native = _run("check_hyperliquid.py", ["breakout", "BTC", "4h", "--mode=live", "--strategy-refs", refs_native,
                                           "--market-stdin"], _solo_envelope())
    assert native.returncode == 0, native.stderr[-2000:]
    assert _payload(native)["close_strategies"] == ["tiered_tp_atr"]


def test_explicit_open_reference_replaces_the_positional_name():
    refs = json.dumps({"open": {"name": "breakout"}})
    proc = _run("check_hyperliquid.py", ["rsi", "BTC", "4h", "--mode=live", "--strategy-refs", refs,
                                         "--market-stdin"], _solo_envelope())
    assert proc.returncode == 0, proc.stderr[-2000:]
    assert _payload(proc)["open_strategy"] == "breakout"


@pytest.mark.parametrize("extra", [["--mode=paper"], ["--mode", "paper"], ["--mode=live", "--allow-no-edge"],
                                   ["--allow-no-edge"]])
def test_solo_paper_or_acknowledged_evaluation_runs_offline(extra):
    proc = _run("check_hyperliquid.py", ["vortex_trend", "BTC", "4h", "--market-stdin"] + extra, _solo_envelope())
    assert proc.returncode == 0, proc.stderr[-2000:]
    out = _payload(proc)
    assert out["strategy"] == "vortex_trend"
    assert out["signal"] in (-1, 0, 1)
    assert not out.get("error")


def _batch(slots):
    rows = _candles()
    return _run("check_hyperliquid.py", ["--batch-check", "--symbol=BTC", "--timeframe=4h", "--market-stdin"],
                {"v": 2, "slots": slots, "market": _market(rows, rows[-1][4])})


def test_batch_slots_carry_their_own_mode_evidence_and_match_solo():
    proc = _batch([
        {"id": "paper", "strategy": "vortex_trend", "mode": "paper", "mode_args": ["--mode=paper"]},
        {"id": "normalized-paper-only", "strategy": "vortex_trend", "mode": "paper"},
        {"id": "acked", "strategy": "vortex_trend", "mode": "live", "mode_args": ["--mode=live"],
         "allow_no_edge": True},
        {"id": "unacked-live", "strategy": "vortex_trend", "mode": "live", "mode_args": ["--mode=live"]},
        {"id": "repeated", "strategy": "vortex_trend", "mode": "paper", "mode_args": ["--mode=paper", "--mode=paper"]},
        {"id": "peer", "strategy": "breakout", "mode": "paper"},
    ])
    assert proc.returncode == 1
    results = {r["id"]: r for r in _payload(proc)["results"]}
    assert not results["paper"].get("error") and not results["acked"].get("error") and not results["peer"].get("error")
    assert "missing mode" in results["normalized-paper-only"]["error"]
    assert "explicit live mode" in results["unacked-live"]["error"]
    assert "invalid mode input" in results["repeated"]["error"]

    solo = _run("check_hyperliquid.py", ["vortex_trend", "BTC", "4h", "--mode=paper", "--market-stdin"], _solo_envelope())
    solo_out = _payload(solo)
    for key in ("signal", "open_action", "close_fraction"):
        assert results["paper"].get(key) == solo_out.get(key)
        assert results["acked"].get(key) == solo_out.get(key)


@pytest.mark.parametrize("bad", [{"allow_no_edge": "true"}, {"allow_no_edge": 1}, {"mode_args": "--mode=paper"},
                                 {"mode_args": ["--mode=paper", 1]}])
def test_malformed_batch_evidence_rejects_the_payload_before_evaluation(bad):
    proc = _batch([dict({"id": "slot", "strategy": "vortex_trend", "mode": "paper"}, **bad)])
    assert proc.returncode == 1
    payload = _payload(proc)
    assert payload["error_scope"] == "shared_state"
    assert payload["results"] == []
    assert payload["error"].startswith("invalid batch payload")


OBV = "on_balance_volume_divergence"


@pytest.mark.parametrize("extra,reason", REFUSED_INPUTS)
def test_obv_candidate_is_refused_outside_explicit_paper(extra, reason):
    proc = _run("check_hyperliquid.py", [OBV, "BTC", "4h"] + extra)
    assert proc.returncode == 1, proc.stderr[-2000:]
    payload = _payload(proc)
    assert payload["signal"] == 0
    assert f"open strategy '{OBV}' is edge_status=no_edge" in payload["error"]
    assert reason in payload["error"]


@pytest.mark.parametrize("extra,reason", [(["--mode=live"], "explicit live mode"), ([], "missing mode")])
def test_obv_close_fallback_reference_is_refused_without_paper_or_acknowledgement(extra, reason):
    refs = json.dumps({"open": {"name": "breakout"}, "closes": [{"name": OBV}]})
    proc = _run("check_hyperliquid.py", ["breakout", "BTC", "4h", "--strategy-refs", refs] + extra)
    assert proc.returncode == 1
    error = _payload(proc)["error"]
    assert f"close strategy '{OBV}' is edge_status=no_edge" in error and reason in error


@pytest.mark.parametrize("extra", [["--mode=paper"], ["--mode=live", "--allow-no-edge"]])
def test_obv_paper_or_acknowledged_frozen_input_check_runs_without_an_order(extra):
    refs = json.dumps({"open": {"name": OBV}, "closes": [{"name": "tiered_tp_pct"}]})
    proc = _run("check_hyperliquid.py", [OBV, "BTC", "4h", "--strategy-refs", refs, "--market-stdin"] + extra,
                _solo_envelope())
    assert proc.returncode == 0, proc.stderr[-2000:]
    out = _payload(proc)
    assert out["strategy"] == OBV and out["open_strategy"] == OBV
    assert out["close_strategies"] == ["tiered_tp_pct"]
    assert out["signal"] in (-1, 0, 1)
    assert not out.get("error")
    assert out["indicators"]["obvd_support_bars"] == 59
    assert "order" not in out and "fill" not in out


def test_obv_batch_slots_carry_their_own_mode_evidence_and_match_solo():
    proc = _batch([
        {"id": "paper", "strategy": OBV, "mode": "paper", "mode_args": ["--mode=paper"]},
        {"id": "normalized-paper-only", "strategy": OBV, "mode": "paper"},
        {"id": "acked", "strategy": OBV, "mode": "live", "mode_args": ["--mode=live"], "allow_no_edge": True},
        {"id": "unacked-live", "strategy": OBV, "mode": "live", "mode_args": ["--mode=live"]},
        {"id": "peer", "strategy": "breakout", "mode": "paper", "mode_args": ["--mode=paper"]},
    ])
    assert proc.returncode == 1
    results = {r["id"]: r for r in _payload(proc)["results"]}
    assert not results["paper"].get("error") and not results["acked"].get("error") and not results["peer"].get("error")
    assert "missing mode" in results["normalized-paper-only"]["error"]
    assert "explicit live mode" in results["unacked-live"]["error"]
    solo = _payload(_run("check_hyperliquid.py", [OBV, "BTC", "4h", "--mode=paper", "--market-stdin"], _solo_envelope()))
    for key in ("signal", "open_action", "close_fraction", "indicators"):
        assert results["paper"].get(key) == solo.get(key)
        assert results["acked"].get(key) == solo.get(key)

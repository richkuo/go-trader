import json
import os
import subprocess
import sys

REPO = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
SCRIPTS = os.path.join(REPO, "shared_scripts")
CREDENTIAL_PREFIXES = ("HYPERLIQUID", "OKX", "ROBINHOOD", "TOPSTEP", "PROJECTX")
NAME = "cumulative_volume_delta_divergence"
REFUSED = "is production_refused"
STEP = 300_000
T0 = 1_791_000_000_000 // STEP * STEP


def _env():
    return {k: v for k, v in os.environ.items() if not k.startswith(CREDENTIAL_PREFIXES)}


def _run(script, argv, stdin=None):
    return subprocess.run([sys.executable, os.path.join(SCRIPTS, script)] + argv,
                          input=None if stdin is None else json.dumps(stdin),
                          capture_output=True, text=True, cwd=REPO, env=_env(), timeout=240)


def _payload(proc):
    return json.loads(proc.stdout.strip().splitlines()[-1])


def _market():
    rows = []
    for i in range(220):
        t = T0 + i * STEP
        px = 100.0 + (i % 17) * 0.3
        rows.append([t + STEP - 1, px, px + 1.0, px - 1.0, px + 0.2, 10.0])
    flow = {"available": True, "kind": "trade_flow", "coin": "BTC", "source": "hyperliquid_ws_trades",
            "units": "base", "time_basis": "event_time_receipt_cutoff", "identity": "venue+namespace+coin+time+tid",
            "side_evidence": "hyperliquid-trades-side-aggressor/1638-v1", "interval_ms": 60_000,
            "reorder_grace_ms": 5_000, "max_event_gap_ms": 30_000, "decimal_scale": 8, "window_ms": 15_000_000,
            "cutoff_ms": rows[-1][0] + 1, "bar_interval_ms": STEP, "snapshot_id": "300s/1", "intervals": [], "gaps": []}
    return {"version": 1, "snapshot_id": "300s/1", "generation": 1, "sealed_at_ms": rows[-1][0],
            "frames": {"BTC|5m": {"rows": rows, "required": 200, "bars": len(rows), "coverage_short": False,
                                  "first_open_ms": T0, "last_open_ms": rows[-1][0] - STEP + 1,
                                  "last_close_ms": rows[-1][0], "last_recv_at_ms": rows[-1][0], "source": "ws",
                                  "ready": True, "forming_bar_included": True}},
            "mids": {"BTC": {"px": 100.0, "recv_at_ms": rows[-1][0], "source": "ws", "age_ms": 0, "stale": False,
                             "confirmed": True}},
            "trade_flows": {"BTC|trade_flow": flow}, "feed_complete": True}


def _assert_refused(proc, role):
    assert proc.returncode == 1, proc.stderr[-2000:]
    payload = _payload(proc)
    assert payload["signal"] == 0
    assert f"{role} strategy '{NAME}' {REFUSED}" in payload["error"], payload["error"]
    assert "Fetching" not in proc.stderr


def test_every_solo_check_refuses_the_candidate_in_any_mode_and_reference():
    envelope = {"v": 2, "market": _market()}
    cases = [
        ([NAME, "BTC", "5m", "--mode=paper", "--market-stdin"], "open"),
        ([NAME, "BTC", "5m", "--mode", "paper", "--market-stdin"], "open"),
        ([NAME, "BTC", "5m", "--mode=live", "--allow-no-edge", "--market-stdin"], "open"),
        ([NAME, "BTC", "5m", "--market-stdin"], "open"),
        (["breakout", "BTC", "5m", "--mode=paper", "--market-stdin",
          "--strategy-refs", json.dumps({"open": {"name": NAME}, "closes": []})], "open"),
        ([NAME, "BTC", "5m", "--mode=paper", "--market-stdin",
          "--strategy-refs", json.dumps({"open": {"name": "breakout"}, "closes": []})], "open"),
        (["breakout", "BTC", "5m", "--mode=live", "--allow-no-edge", "--market-stdin",
          "--strategy-refs", json.dumps({"open": {"name": "breakout"}, "closes": [{"name": NAME}]})], "close"),
    ]
    for argv, role in cases:
        _assert_refused(_run("check_hyperliquid.py", argv, envelope), role)
    proc = _run("check_okx.py", [NAME, "BTC", "5m", "--inst-type=swap", "--mode=paper"])
    assert proc.returncode == 1 and REFUSED in _payload(proc)["error"], proc.stdout[-2000:] + proc.stderr[-2000:]
    assert "Fetching" not in proc.stderr


def test_batch_slots_refuse_the_candidate_and_keep_their_peers():
    slots = [
        {"id": "paper", "strategy": NAME, "mode": "paper", "mode_args": ["--mode=paper"]},
        {"id": "acked", "strategy": NAME, "mode": "live", "mode_args": ["--mode=live"], "allow_no_edge": True},
        {"id": "peer", "strategy": "breakout", "mode": "paper"},
    ]
    proc = _run("check_hyperliquid.py", ["--batch-check", "--symbol=BTC", "--timeframe=5m", "--market-stdin"],
                {"v": 2, "slots": slots, "market": _market()})
    results = {r["id"]: r for r in _payload(proc)["results"]}
    for slot in ("paper", "acked"):
        assert f"open strategy '{NAME}' {REFUSED}" in results[slot]["error"]
        assert results[slot].get("signal", 0) == 0
    assert not results["peer"].get("error")

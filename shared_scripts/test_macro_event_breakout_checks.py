import json
import os
import subprocess
import sys

REPO = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
SCRIPTS = os.path.join(REPO, "shared_scripts")
CREDENTIAL_PREFIXES = ("HYPERLIQUID", "OKX", "ROBINHOOD", "TOPSTEP", "PROJECTX")
NAME = "macro_event_breakout"
REFUSED = "is production_refused"
STEP = 3_600_000
T0 = 1_791_000_000_000 // STEP * STEP


def _env():
    return {k: v for k, v in os.environ.items() if not k.startswith(CREDENTIAL_PREFIXES)}


def _run(argv, stdin):
    return subprocess.run([sys.executable, os.path.join(SCRIPTS, "check_hyperliquid.py")] + argv,
                          input=json.dumps(stdin), capture_output=True, text=True, cwd=REPO, env=_env(), timeout=240)


def _payload(proc):
    return json.loads(proc.stdout.strip().splitlines()[-1])


def _market():
    rows = []
    for i in range(220):
        px = 100.0 + (i % 13) * 0.4
        rows.append([T0 + i * STEP + STEP - 1, px, px + 1.0, px - 1.0, px + 0.2, 10.0])
    unavailable = {"kind": "macro_calendar", "available": False, "reason": "fixture", "calendar_sha256": "1" * 64}
    return {"version": 4, "snapshot_id": "3600s/1", "generation": 1, "sealed_at_ms": rows[-1][0],
            "frames": {"BTC|1h": {"rows": rows, "required": 200, "bars": len(rows), "coverage_short": False,
                                  "first_open_ms": T0, "last_open_ms": rows[-1][0] - STEP + 1,
                                  "last_close_ms": rows[-1][0], "last_recv_at_ms": rows[-1][0], "source": "ws",
                                  "ready": True, "forming_bar_included": True}},
            "mids": {"BTC": {"px": 100.0, "recv_at_ms": rows[-1][0], "source": "ws", "age_ms": 0, "stale": False,
                             "confirmed": True}},
            "macro_events": {"1" * 64: unavailable}, "feed_complete": True}


def test_solo_checks_refuse_the_candidate_in_explicit_paper_mode_and_as_a_close_fallback():
    envelope = {"v": 2, "market": _market()}
    for argv, role in (([NAME, "BTC", "1h", "--mode=paper", "--market-stdin"], "open"),
                       (["breakout", "BTC", "1h", "--mode=live", "--allow-no-edge", "--market-stdin",
                         "--strategy-refs", json.dumps({"open": {"name": "breakout"}, "closes": [{"name": NAME}]})],
                        "close")):
        proc = _run(argv, envelope)
        assert proc.returncode == 1, proc.stderr[-2000:]
        payload = _payload(proc)
        assert payload["signal"] == 0
        assert f"{role} strategy '{NAME}' {REFUSED}" in payload["error"], payload["error"]
        assert "Fetching" not in proc.stderr


def test_batch_slots_refuse_the_candidate_and_keep_their_peers():
    slots = [
        {"id": "paper", "strategy": NAME, "mode": "paper", "mode_args": ["--mode=paper"]},
        {"id": "acked", "strategy": NAME, "mode": "live", "mode_args": ["--mode=live"], "allow_no_edge": True},
        {"id": "peer", "strategy": "breakout", "mode": "paper"},
    ]
    proc = _run(["--batch-check", "--symbol=BTC", "--timeframe=1h", "--market-stdin"],
                {"v": 2, "slots": slots, "market": _market()})
    results = {r["id"]: r for r in _payload(proc)["results"]}
    for slot in ("paper", "acked"):
        assert f"open strategy '{NAME}' {REFUSED}" in results[slot]["error"]
        assert results[slot].get("signal", 0) == 0
    assert not results["peer"].get("error")

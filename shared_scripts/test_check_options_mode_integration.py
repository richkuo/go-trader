import json
import os
import subprocess
import sys

import pytest

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
STUB_DIR = os.path.join(ROOT, "shared_scripts", "testdata", "quote_precision")
CHECK_OPTIONS = os.path.join(ROOT, "shared_scripts", "check_options.py")
CREDENTIALS = (
    "OKX_API_KEY", "OKX_API_SECRET", "OKX_PASSPHRASE", "OKX_SANDBOX",
    "ROBINHOOD_USERNAME", "ROBINHOOD_PASSWORD", "ROBINHOOD_TOTP_SECRET",
)
FOUR_POSITIONS = json.dumps([
    {"option_type": "call", "strike": 500, "expiry": "2026-12-18"},
    {"option_type": "call", "strike": 510, "expiry": "2026-12-18"},
    {"option_type": "put", "strike": 450, "expiry": "2026-12-18"},
    {"option_type": "put", "strike": 440, "expiry": "2026-12-18"},
])
CAP_REASON = "Max positions reached (4/4)"


def _expected_error(platform):
    return (
        "options strategies run as paper only: no venue order path exists for options on "
        f"{platform}; remove --mode=live (or --mode live) or use --mode=paper"
    )


def _run(argv, tmp_path, stdin):
    env = dict(os.environ)
    for key in CREDENTIALS:
        env.pop(key, None)
    for key in ("QP_CLOSE", "QP_QUOTE", "QP_SHAPE", "QP_NOW", "QP_TICKERS", "QP_CLOSES", "QP_CANDLES", "QP_OKX_FILL"):
        env.pop(key, None)
    log_path = tmp_path / "qp.log"
    env["QP_STUB"] = "1"
    env["QP_LOG"] = str(log_path)
    env["PYTHONPATH"] = STUB_DIR
    env["HOME"] = str(tmp_path)
    proc = subprocess.run(
        [sys.executable, CHECK_OPTIONS] + argv,
        input=stdin, capture_output=True, text=True, cwd=ROOT, env=env, timeout=180,
    )
    records = []
    if log_path.exists():
        records = [json.loads(ln) for ln in log_path.read_text().splitlines() if ln.strip()]
    return proc, records


def _single_document(proc):
    lines = [ln for ln in proc.stdout.splitlines() if ln.strip()]
    assert len(lines) == 1, f"stdout must be one JSON document:\n{proc.stdout}\nstderr:\n{proc.stderr}"
    return json.loads(lines[0])


PAPER_ARGVS = {
    "omitted": ["covered_calls", "SPY", "--platform=deribit"],
    "equals_form": ["covered_calls", "SPY", "--platform=deribit", "--mode=paper"],
    "two_token_form": ["covered_calls", "SPY", "--platform=deribit", "--mode", "paper"],
    "two_token_form_first": ["--mode", "paper", "covered_calls", "SPY", "--platform=deribit"],
}


@pytest.mark.parametrize("shape", sorted(PAPER_ARGVS))
def test_paper_and_omitted_mode_read_the_stdin_positions(shape, tmp_path):
    proc, records = _run(PAPER_ARGVS[shape], tmp_path, FOUR_POSITIONS)
    assert proc.returncode == 0, proc.stderr
    out = _single_document(proc)
    assert out["skip_reason"] == CAP_REASON
    assert out["strategy"] == "covered_calls"
    assert out["underlying"] == "SPY"
    assert out["platform"] == "deribit"
    assert records == []


def test_inline_positions_argument_with_two_token_mode_reaches_the_cap(tmp_path):
    argv = ["covered_calls", "SPY", FOUR_POSITIONS, "--platform=deribit", "--mode", "paper"]
    proc, records = _run(argv, tmp_path, "")
    assert proc.returncode == 0, proc.stderr
    out = _single_document(proc)
    assert out["skip_reason"] == CAP_REASON
    assert out["underlying"] == "SPY"
    assert records == []


def test_paper_control_builds_the_adapter_and_fetches(tmp_path):
    proc, records = _run(["covered_calls", "SPY", "--platform=deribit", "--mode", "paper"], tmp_path, "[]")
    out = _single_document(proc)
    assert out.get("skip_reason") is None
    assert len(records) > 0


LIVE_TAILS = {
    "equals_form": ["--mode=live"],
    "two_token_form": ["--mode", "live"],
}


@pytest.mark.parametrize("platform", ["deribit", "robinhood"])
@pytest.mark.parametrize("tail", sorted(LIVE_TAILS))
def test_live_mode_refuses_before_the_cap_and_the_adapter(platform, tail, tmp_path):
    argv = ["covered_calls", "SPY", f"--platform={platform}"] + LIVE_TAILS[tail]
    proc, records = _run(argv, tmp_path, FOUR_POSITIONS)
    assert proc.returncode == 1
    out = _single_document(proc)
    assert out["error"] == _expected_error(platform)
    assert out["signal"] == 0
    assert out["actions"] == []
    assert out["platform"] == platform
    assert out.get("skip_reason") is None
    assert records == []

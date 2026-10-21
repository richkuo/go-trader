import json
import os
import subprocess
import sys
from pathlib import Path

import pytest

REPO_ROOT = Path(__file__).resolve().parent.parent
SCRIPTS = REPO_ROOT / "shared_scripts"
FIXTURE = SCRIPTS / "testdata" / "robinhood_offline"
NOISE_MARKER = "robinhood offline stub stdout noise"
MODES = ("login_ok", "login_rejected", "transport_error")

CASES = [
    ("check_paper", "check_robinhood.py", ["momentum_pro", "BTC", "1h", "--mode=paper"], None, True, True),
    ("check_live", "check_robinhood.py", ["momentum_pro", "BTC", "1h", "--mode=live"], None, True, True),
    ("execute_buy", "check_robinhood.py", ["--execute", "--symbol=BTC", "--side=buy", "--amount_usd=10", "--mode=live"], None, True, True),
    ("execute_sell", "check_robinhood.py", ["--execute", "--symbol=BTC", "--side=sell", "--quantity=0.001", "--mode=live"], None, True, True),
    ("close", "close_robinhood_position.py", ["--symbol=BTC", "--mode=live"], None, True, True),
    ("positions", "fetch_robinhood_positions.py", [], None, True, True),
    ("options", "check_options.py", ["vol_mean_reversion", "BTC", "--platform=robinhood"], "[]", False, True),
    ("balance", "check_balance.py", ["--platform=robinhood"], None, True, True),
    ("candles_paper", "fetch_candles.py", ["--platform", "robinhood", "--type", "spot", "--symbol", "BTC", "--timeframe", "1h", "--limit", "50", "--mode", "paper"], None, False, False),
    ("candles_live", "fetch_candles.py", ["--platform", "robinhood", "--type", "spot", "--symbol", "BTC", "--timeframe", "1h", "--limit", "50", "--mode", "live"], None, False, False),
]

PARAMS = [
    pytest.param(case, mode, id=f"{case[0]}-{mode}")
    for case in CASES
    for mode in MODES
]


def _refuse_constant(name):
    raise ValueError(f"non-finite JSON constant {name} on stdout")


def _offline_env(home, mode, noise):
    env = {k: v for k, v in os.environ.items() if not k.startswith("ROBINHOOD")}
    env.pop("RH_OFFLINE_STUB_STDOUT_NOISE", None)
    env.update({
        "HOME": str(home),
        "PYTHONPATH": str(FIXTURE),
        "RH_OFFLINE_STUB_MODE": mode,
        "ROBINHOOD_USERNAME": "offline-user",
        "ROBINHOOD_PASSWORD": "offline-pass",
        "ROBINHOOD_TOTP_SECRET": "JBSWY3DPEHPK3PXP",
    })
    if noise:
        env["RH_OFFLINE_STUB_STDOUT_NOISE"] = "1"
    return env


@pytest.mark.parametrize("case,mode", PARAMS)
def test_robinhood_script_writes_one_json_document(case, mode, tmp_path):
    _case_id, script, args, stdin_text, script_guard, call_after_login = case
    kwargs = {"input": stdin_text} if stdin_text is not None else {"stdin": subprocess.DEVNULL}
    proc = subprocess.run(
        [sys.executable, str(SCRIPTS / script), *args],
        cwd=str(REPO_ROOT),
        env=_offline_env(tmp_path, mode, script_guard),
        capture_output=True,
        text=True,
        timeout=180,
        **kwargs,
    )
    detail = f"exit={proc.returncode}\nstdout:\n{proc.stdout}\nstderr:\n{proc.stderr}"

    assert proc.returncode in (0, 1), detail
    doc = json.loads(proc.stdout, parse_constant=_refuse_constant)
    assert isinstance(doc, dict), detail

    assert f"robinhood offline stub active: {mode}" in proc.stderr, detail
    assert "Starting login process..." in proc.stderr, detail
    if mode == "login_rejected":
        assert "Login failed. Check credentials and try again." in proc.stderr, detail
    elif mode == "transport_error":
        assert "Error in request_post:" in proc.stderr, detail
        assert "Login failed." in proc.stderr, detail
    else:
        assert "Login failed" not in proc.stderr, detail
        assert ("503 Server Error" in proc.stderr) is call_after_login, detail
    if script_guard:
        assert NOISE_MARKER in proc.stderr, detail

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
REF_ID = "11111111-1111-4111-8111-111111111111"

CASES = [
    ("check_paper", "check_robinhood.py", ["momentum_pro", "BTC", "1h", "--mode=paper"], None, True, True),
    ("check_live", "check_robinhood.py", ["momentum_pro", "BTC", "1h", "--mode=live"], None, True, True),
    ("execute_buy", "check_robinhood.py", ["--execute", "--symbol=BTC", "--side=buy", "--amount_usd=10", "--mode=live", "--ref-id=" + REF_ID, "--budget-ms=3000"], None, True, False),
    ("execute_sell", "check_robinhood.py", ["--execute", "--symbol=BTC", "--side=sell", "--quantity=0.001", "--mode=live", "--ref-id=" + REF_ID, "--budget-ms=3000"], None, True, False),
    ("close", "close_robinhood_position.py", ["--symbol=BTC", "--mode=live"], None, True, False),
    ("positions", "fetch_robinhood_positions.py", [], None, True, False),
    ("options", "check_options.py", ["vol_mean_reversion", "BTC", "--platform=robinhood"], "[]", False, True),
    ("balance", "check_balance.py", ["--platform=robinhood"], None, True, True),
    ("candles_paper", "fetch_candles.py", ["--platform", "robinhood", "--type", "spot", "--symbol", "BTC", "--timeframe", "1h", "--limit", "50", "--mode", "paper"], None, False, False),
    ("candles_live", "fetch_candles.py", ["--platform", "robinhood", "--type", "spot", "--symbol", "BTC", "--timeframe", "1h", "--limit", "50", "--mode", "live"], None, False, False),
]

READS_FAIL_CLOSED = ("close", "positions")

BUDGET = "--budget-ms=3000"
EXECUTE_BUY = ["check_robinhood.py", "--execute", "--symbol=BTC", "--side=buy", "--amount_usd=10", "--mode=live", "--ref-id=" + REF_ID, BUDGET]
EXECUTE_SELL = ["check_robinhood.py", "--execute", "--symbol=BTC", "--side=sell", "--quantity=0.1", "--mode=live", "--ref-id=" + REF_ID, BUDGET]
CLOSE_BTC = ["close_robinhood_position.py", "--symbol=BTC", "--mode=live", "--ref-id=" + REF_ID, BUDGET]
CLOSE_ETH = ["close_robinhood_position.py", "--symbol=ETH", "--mode=live", "--ref-id=" + REF_ID, BUDGET]
REFUSED = "Robinhood refused the order"
UNKNOWN = "the outcome is unknown"
UNCONFIRMED = "state is unreadable"

ORDER_CASES = [
    ("buy_refused", EXECUTE_BUY, "order_rejected", 1, REFUSED, "rejected"),
    ("sell_refused", EXECUTE_SELL, "order_rejected", 1, REFUSED, "rejected"),
    ("close_refused", CLOSE_BTC, "order_rejected", 1, REFUSED, "rejected"),
    ("buy_unknown", EXECUTE_BUY, "order_unknown", 1, UNKNOWN, "unknown"),
    ("sell_unknown", EXECUTE_SELL, "order_unknown", 1, UNKNOWN, "unknown"),
    ("close_unknown", CLOSE_BTC, "order_unknown", 1, UNKNOWN, "unknown"),
    ("buy_accepted", EXECUTE_BUY, "order_accepted", 1, UNCONFIRMED, "unknown"),
    ("sell_accepted", EXECUTE_SELL, "order_accepted", 1, UNCONFIRMED, "unknown"),
    ("close_accepted", CLOSE_BTC, "order_accepted", 1, UNCONFIRMED, "unknown"),
    ("close_no_holding", CLOSE_ETH, "order_accepted", 0, None, "already_flat"),
]

PARAMS = [
    pytest.param(case, mode, id=f"{case[0]}-{mode}")
    for case in CASES
    for mode in MODES
]


def _refuse_constant(name):
    raise ValueError(f"non-finite JSON constant {name} on stdout")


def _refuse_constant_doc(stdout):
    return json.loads(stdout, parse_constant=_refuse_constant)


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
        if _case_id.startswith("execute_"):
            assert "HTTP status 503" in proc.stderr, detail
    if script_guard:
        assert NOISE_MARKER in proc.stderr, detail
    if _case_id in READS_FAIL_CLOSED:
        assert proc.returncode == 1, detail
        assert doc.get("error"), detail
        assert not (doc.get("close") or {}).get("already_flat"), detail


@pytest.mark.parametrize("label,argv,mode,want_exit,want_error,want_outcome", ORDER_CASES, ids=[c[0] for c in ORDER_CASES])
def test_robinhood_order_response_decides_exit(label, argv, mode, want_exit, want_error, want_outcome, tmp_path):
    script, *args = argv
    proc = subprocess.run(
        [sys.executable, str(SCRIPTS / script), *args],
        cwd=str(REPO_ROOT),
        env=_offline_env(tmp_path, mode, True),
        capture_output=True,
        text=True,
        timeout=180,
        stdin=subprocess.DEVNULL,
    )
    detail = f"exit={proc.returncode}\nstdout:\n{proc.stdout}\nstderr:\n{proc.stderr}"

    doc = _refuse_constant_doc(proc.stdout)
    assert isinstance(doc, dict), detail
    assert f"robinhood offline stub active: {mode}" in proc.stderr, detail
    assert "Starting login process..." in proc.stderr, detail
    assert NOISE_MARKER in proc.stderr, detail
    assert proc.returncode == want_exit, detail
    assert doc.get("order_outcome") == want_outcome, detail
    if want_error is None:
        assert "error" not in doc, detail
    else:
        assert want_error in (doc.get("error") or ""), detail
        assert "fill" not in (doc.get("execution") or {}), detail
        assert "fill" not in (doc.get("close") or {}), detail
    if label == "close_accepted":
        assert doc["order"].get("id") == "offline-order-1", detail
        assert not doc["close"].get("already_flat"), detail
    if label == "close_no_holding":
        assert doc["close"].get("already_flat") is True, detail
    if label in ("buy_accepted", "sell_accepted"):
        assert doc["order"].get("id") == "offline-order-1", detail
        assert doc["cancel"]["requested"] is True, detail


FAILED_LOGIN_MODES = (
    "login_rejected",
    "transport_error",
    "transport_error_secret",
    "login_no_refresh_token",
    "login_empty_token",
    "token_cache_unwritable",
    "token_cache_dir_error",
    "login_verification_required",
)
LIVE_CASE_IDS = ("check_live", "execute_buy", "execute_sell", "close", "positions", "candles_live", "balance")
PAPER_CASE_IDS = ("check_paper", "options", "candles_paper")
SECRETS = ("offline-user", "offline-pass", "JBSWY3DPEHPK3PXP", "offline-access-token", "offline-refresh-token")
LOGIN_FAILED = "Robinhood login failed:"
TOKEN_URL = "https://api.robinhood.com/oauth2/token/"
PATHFINDER_URL = "https://api.robinhood.com/pathfinder/user_machine/"
MODE_REASONS = {
    "login_empty_token": ("no access token",),
    "login_no_refresh_token": ("token cache", "robin_stocks reported 'refresh_token'"),
    "token_cache_unwritable": ("token cache", "robin_stocks reported [Errno 20]"),
    "token_cache_dir_error": ("raised PermissionError", "token cache"),
}
MODE_STDERR = {
    "login_verification_required": ("Verification required, handling challenge...", "Error in request_post: Received 503"),
    "transport_error_secret": ("Error in request_post: (detail withheld)",),
    "transport_error": ("Error in request_post: (detail withheld)",),
}

FAILED_LOGIN_PARAMS = [
    pytest.param(case, mode, id=f"{case[0]}-{mode}")
    for case in CASES
    for mode in FAILED_LOGIN_MODES
]


def _run_logged(case, mode, tmp_path):
    _case_id, script, args, stdin_text, script_guard, _call_after_login = case
    home = tmp_path / "home"
    home.mkdir()
    log = tmp_path / "requests.jsonl"
    env = _offline_env(home, mode, script_guard)
    env["RH_OFFLINE_STUB_LOG"] = str(log)
    kwargs = {"input": stdin_text} if stdin_text is not None else {"stdin": subprocess.DEVNULL}
    proc = subprocess.run(
        [sys.executable, str(SCRIPTS / script), *args],
        cwd=str(REPO_ROOT),
        env=env,
        capture_output=True,
        text=True,
        timeout=180,
        **kwargs,
    )
    made = [json.loads(ln) for ln in log.read_text().splitlines() if ln.strip()] if log.exists() else []
    return proc, made


def _login_reason(doc, stderr):
    error = doc.get("error")
    if isinstance(error, str) and error.startswith(LOGIN_FAILED):
        return error
    lines = [ln for ln in stderr.splitlines() if ln.startswith("[robinhood] " + LOGIN_FAILED)]
    return lines[-1][len("[robinhood] "):] if lines else ""


@pytest.mark.parametrize("case,mode", FAILED_LOGIN_PARAMS)
def test_failed_login_refuses_live_work_and_keeps_paper_logged_out(case, mode, tmp_path):
    case_id = case[0]
    proc, made = _run_logged(case, mode, tmp_path)
    detail = f"exit={proc.returncode}\nstdout:\n{proc.stdout}\nstderr:\n{proc.stderr}\nrequests:\n{made}"

    doc = _refuse_constant_doc(proc.stdout)
    assert isinstance(doc, dict), detail
    assert f"robinhood offline stub active: {mode}" in proc.stderr, detail
    leaked = [s for s in SECRETS if s in proc.stdout or s in proc.stderr]
    assert leaked == [], detail
    robinhood_requests = [r for r in made if "robinhood.com" in r["url"]]
    assert [r for r in robinhood_requests if r["auth"]] == [], detail

    reason = _login_reason(doc, proc.stderr)
    assert reason.startswith(LOGIN_FAILED), detail
    for want in MODE_REASONS.get(mode, ()):
        assert want in reason, detail
    for want in MODE_STDERR.get(mode, ()):
        assert want in proc.stderr, detail

    if case_id in LIVE_CASE_IDS:
        assert proc.returncode == 1, detail
        assert doc["error"].startswith(LOGIN_FAILED), detail
        assert (doc.get("close") or {}).get("already_flat") is None, detail
        assert "fill" not in (doc.get("execution") or {}), detail
        if case_id.startswith("execute_"):
            assert doc.get("order_outcome") == "not_sent", detail
        assert [r for r in made if "nummus.robinhood.com" in r["url"]] == [], detail
    else:
        assert case_id in PAPER_CASE_IDS, detail
        assert proc.returncode == 0, detail
        assert "error" not in doc, detail
        allowed = {TOKEN_URL, PATHFINDER_URL} if mode == "login_verification_required" else {TOKEN_URL}
        assert {r["url"] for r in robinhood_requests} <= allowed, detail
        assert "can only be called when logged in" not in proc.stderr, detail
        assert "503 Server Error" not in proc.stderr, detail
        assert "paper mode continues logged out on public data" in proc.stderr, detail
        if case_id == "candles_paper":
            assert doc["source"] == "robinhood:adapter" and len(doc["candles"]) > 0, detail
        if case_id == "check_paper":
            assert isinstance(doc.get("price"), (int, float)) and doc["price"] > 0, detail


def test_successful_login_reads_holdings_with_the_returned_token(tmp_path):
    case = next(c for c in CASES if c[0] == "positions")
    proc, made = _run_logged(case, "order_accepted", tmp_path)
    detail = f"exit={proc.returncode}\nstdout:\n{proc.stdout}\nstderr:\n{proc.stderr}\nrequests:\n{made}"

    doc = _refuse_constant_doc(proc.stdout)
    assert proc.returncode == 0, detail
    assert doc.get("error") is None, detail
    assert doc["positions"] == [{"coin": "BTC", "size": 0.5, "avg_price": 100.0}], detail
    holdings = [r for r in made if r["url"] == "https://nummus.robinhood.com/holdings/"]
    assert len(holdings) >= 1, detail
    assert all(r["auth_is_offline_token"] for r in holdings), detail
    token_posts = [r for r in made if r["method"] == "POST" and r["url"] == TOKEN_URL]
    assert len(token_posts) == 1 and not token_posts[0]["auth"], detail

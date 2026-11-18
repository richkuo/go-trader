import json
import os
import subprocess
import sys

import pytest

SCRIPT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "fetch_hl_agent_status.py")

OUT_OF_RANGE_KEY = "0x" + "ff" * 32


def _key_forms(key):
    body = key[2:] if key[:2].lower() == "0x" else key
    forms = [f for f in {body, body.lower(), body.upper()} if len(f) >= 8]
    try:
        forms.append(str(int(body, 16)))
    except ValueError:
        pass
    return forms


def _run(env_updates, *argv):
    env = {k: v for k, v in os.environ.items() if not k.startswith("HYPERLIQUID_")}
    env.update(env_updates)
    proc = subprocess.run(
        [sys.executable, SCRIPT, *argv],
        env=env,
        capture_output=True,
        text=True,
        timeout=60,
    )
    return proc.returncode, proc.stdout, proc.stderr


@pytest.mark.parametrize(
    "env_updates",
    [
        {},
        {"HYPERLIQUID_SECRET_KEY": "0xnot-hex-at-all-0000000000000000000000000000000000000000000000"},
        {"HYPERLIQUID_SECRET_KEY": "0x" + "é" * 64},
        {"HYPERLIQUID_SECRET_KEY": "0x1234"},
        {"HYPERLIQUID_SECRET_KEY": OUT_OF_RANGE_KEY},
        {"HYPERLIQUID_SECRET_KEY": "0x" + "11" * 32, "HYPERLIQUID_ACCOUNT_ADDRESS": "0xnot-an-address"},
    ],
)
def test_key_and_address_errors_report_without_the_key(env_updates):
    code, stdout, stderr = _run(env_updates, "--timeout=1")
    doc = json.loads(stdout)
    assert code == 1
    assert doc["error"]
    assert doc["role"] is None
    assert doc["valid_until_ms"] is None
    assert stderr == ""
    key = env_updates.get("HYPERLIQUID_SECRET_KEY", "")
    for form in _key_forms(key) if key else []:
        assert form not in stdout
        assert form not in stderr


def test_probe_only_reads_no_key_and_exits_zero():
    code, stdout, stderr = _run({"HYPERLIQUID_SECRET_KEY": OUT_OF_RANGE_KEY}, "--probe-only")
    doc = json.loads(stdout)
    assert code == 0
    assert doc["error"] == ""
    assert doc["signer_address"] == ""
    assert stderr == ""

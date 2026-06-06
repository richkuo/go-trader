"""Contract tests for --regime-payload-json on check scripts (#879)."""

from __future__ import annotations

import json
import subprocess
import sys
from pathlib import Path

import pytest

REPO = Path(__file__).resolve().parents[1]
PYTHON = sys.executable
PROBE_ARGS = [
    "probe",
    "BTC",
    "1h",
    "--strategy-refs",
    '{"open":{"name":"probe","params":{}},"closes":[{"name":"probe_close","params":{}}]}',
    "--mark-price=0",
    "--ohlcv-limit",
    "200",
    "--regime-enabled",
    "--regime-windows-spec-json",
    '{"default":{"classifier":"adx","period":14,"adx_threshold":20}}',
    "--regime-atr-window",
    "",
    "--regime-payload-json",
    '"trending_up"',
    "--probe-only",
]


@pytest.mark.parametrize(
    "script",
    [
        "check_hyperliquid.py",
        "check_strategy.py",
        "check_okx.py",
        "check_robinhood.py",
        "check_topstep.py",
    ],
)
def test_check_script_accepts_injected_regime_probe(script: str) -> None:
    path = REPO / "shared_scripts" / script
    proc = subprocess.run(
        [PYTHON, str(path), *PROBE_ARGS],
        cwd=REPO,
        capture_output=True,
        text=True,
        timeout=30,
    )
    assert proc.returncode == 0, proc.stderr or proc.stdout


def test_check_options_accepts_injected_regime_probe() -> None:
    path = REPO / "shared_scripts" / "check_options.py"
    proc = subprocess.run(
        [
            PYTHON,
            str(path),
            "short_put",
            "BTC",
            "--platform=deribit",
            "--regime-payload-json",
            '"ranging"',
            "--probe-only",
        ],
        cwd=REPO,
        capture_output=True,
        text=True,
        timeout=30,
    )
    assert proc.returncode == 0, proc.stderr or proc.stdout


def test_options_argv_strips_regime_flags_for_stdin_positions() -> None:
    import importlib.util

    path = REPO / "shared_scripts" / "check_options.py"
    spec = importlib.util.spec_from_file_location("check_options", path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    argv = [
        "short_put",
        "BTC",
        "--platform=deribit",
        "--regime-enabled",
        "--regime-windows-spec-json",
        '{"default":{"classifier":"adx","period":14,"adx_threshold":20}}',
        "--ohlcv-limit",
        "200",
        "--regime-payload-json",
        '"ranging"',
    ]
    platform, remaining = mod._split_options_argv(argv)
    assert platform == "deribit"
    assert remaining == ["short_put", "BTC"]


def test_compute_regime_bundle_probe_only() -> None:
    path = REPO / "shared_scripts" / "compute_regime_bundle.py"
    proc = subprocess.run(
        [
            PYTHON,
            str(path),
            "--platform=hyperliquid",
            "--type=perps",
            "--symbol=BTC",
            "--timeframe=1h",
            "--period=14",
            "--probe-only",
        ],
        cwd=REPO,
        capture_output=True,
        text=True,
        timeout=30,
    )
    assert proc.returncode == 0, proc.stderr or proc.stdout
    assert json.loads(proc.stdout)["ok"] is True

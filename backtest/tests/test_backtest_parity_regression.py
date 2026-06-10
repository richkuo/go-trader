"""
Regression umbrella for issue #303 — backtest ↔ live parity.

#303 fixed distributed parity gaps (fees, regime, options pricing, sl_after).
Coverage is intentionally spread across multiple modules rather than one golden
file. This meta-test pins the contract: each module below must exist and cite
#303 or a child parity issue so future refactors don't drop the net.

#824 (storage.py startup probe under ProtectSystem=strict) is **out of backtest
scope** per #906 — enforced by shared_tools tests + ``--probe-only`` scripts, not
the backtest engine.
"""
from __future__ import annotations

import os
import re

_TESTS_DIR = os.path.join(os.path.dirname(__file__))


# module → acceptable issue citations (at least one must appear in file body)
_PARITY_REGRESSION_MODULES: dict[str, tuple[str, ...]] = {
    "test_backtester_regime.py": ("#303", "#543", "parity", "Parity"),
    "test_options_adapter_parity.py": ("parity", "Parity", "adapter"),
    "test_platform_fees.py": ("fees.go", "parity", "Parity"),
    "test_post_tp_sl.py": ("#709", "#715", "parity", "Parity"),
    "test_regime_backtester_737.py": ("#737", "#733", "parity", "Parity"),
    "test_strategy_refs_641.py": ("#641", "#640"),
    "test_parity_diff.py": ("#906", "parity"),
}


def test_parity_regression_modules_exist_and_cite_contract():
    """#303 / #906 D8.4 — distributed parity regression index."""
    for fname, markers in _PARITY_REGRESSION_MODULES.items():
        path = os.path.join(_TESTS_DIR, fname)
        assert os.path.isfile(path), f"missing parity regression module: {fname}"
        body = open(path, encoding="utf-8").read()
        assert any(m in body for m in markers), (
            f"{fname} should cite one of {markers} (parity contract)"
        )


def test_platform_fees_scrapes_scheduler_fees_go():
    """#303 H-item: backtest fee table stays tied to scheduler/fees.go."""
    path = os.path.join(_TESTS_DIR, "test_platform_fees.py")
    body = open(path, encoding="utf-8").read()
    assert "fees.go" in body
    assert re.search(r"PLATFORM_FEE_PCT|fee_pct", body)


def test_issue_824_documented_out_of_backtest_scope():
    """#824 is closed but lives in shared_tools — not a backtest regression."""
    audit_path = os.path.join(os.path.dirname(_TESTS_DIR), "AUDIT.md")
    assert os.path.isfile(audit_path)
    audit = open(audit_path, encoding="utf-8").read()
    assert "#824" in audit
    assert "out of backtest" in audit.lower() or "Out of backtest" in audit

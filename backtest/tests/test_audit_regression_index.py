"""
D8.4 regression index for closed backtest bug issues (#906).

Each closed issue that shipped a backtest-specific bug must retain at least one
test file that cites the issue and would have caught the original failure class.
"""
from __future__ import annotations

import os

_TESTS_DIR = os.path.join(os.path.dirname(__file__))

_CLOSED_ISSUE_TESTS: dict[str, list[str]] = {
    "#302": [
        "test_backtester_end_to_end.py",
        "test_backtester_fills.py",
        "test_options_vol_math.py",
        "test_options_iv_rank.py",
    ],
    "#304": ["test_backtest_reporting.py"],
    "#715": ["test_post_tp_sl.py"],
    "#730": ["test_backtester_lookahead.py"],
}


def test_closed_backtest_issues_have_regression_files():
    for issue, files in _CLOSED_ISSUE_TESTS.items():
        for fname in files:
            path = os.path.join(_TESTS_DIR, fname)
            assert os.path.isfile(path), f"{issue}: missing {fname}"
            body = open(path, encoding="utf-8").read()
            num = issue.replace("#", "")
            assert num in body or issue in body, (
                f"{fname} should cite {issue}"
            )

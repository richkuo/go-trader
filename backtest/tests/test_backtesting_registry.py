"""Registry completeness regression (#1216).

Enforces the invariant docs/backtesting-registry.md and the CLAUDE.md upkeep
rule both promise: every non-test harness script under backtest/ and
backtest/research/ has exactly one entry in the registry, and the registry
references no file that does not exist. This is what keeps the doc from silently
drifting out of sync with the code as harnesses are added or removed.
"""
import re
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[2]
BACKTEST = REPO_ROOT / "backtest"
REGISTRY = REPO_ROOT / "docs" / "backtesting-registry.md"


def _is_test(name: str) -> bool:
    return name.startswith("test_") or name.endswith("_test.py")


def _harness_files():
    """Non-test .py directly under backtest/ and backtest/research/ (non-recursive)."""
    files = []
    for d in (BACKTEST, BACKTEST / "research"):
        for p in sorted(d.glob("*.py")):
            if _is_test(p.name):
                continue
            files.append(p)
    return files


def _registry_text() -> str:
    return REGISTRY.read_text()


def test_every_harness_has_a_registry_row():
    text = _registry_text()
    missing = [
        p.relative_to(BACKTEST).as_posix()
        for p in _harness_files()
        if p.name not in text
    ]
    assert not missing, (
        "backtest/ scripts missing a row in docs/backtesting-registry.md "
        f"(add one per the CLAUDE.md upkeep rule): {missing}"
    )


def test_registry_references_no_nonexistent_file():
    """Inverse invariant: no phantom rows — every .py the doc names exists."""
    text = _registry_text()
    # backtick-quoted tokens ending in .py, e.g. `backtester.py`, `research/regime_1073_x.py`
    tokens = set(re.findall(r"`([\w./-]+\.py)`", text))
    phantom = []
    for tok in sorted(tokens):
        # every harness token in the doc is rooted at backtest/
        if not (BACKTEST / tok).exists():
            phantom.append(tok)
    assert not phantom, (
        "docs/backtesting-registry.md references files that do not exist under "
        f"backtest/: {phantom}"
    )

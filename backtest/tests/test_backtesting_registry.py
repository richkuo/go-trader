"""Registry completeness regression (#1216, hardened #1220).

Enforces the invariant docs/backtesting-registry.md and the CLAUDE.md upkeep
rule both promise: every non-test harness script under backtest/ and
backtest/research/ has exactly one table row in the registry, every candidate
study directory is listed, and the registry references no file or study that
does not exist. This is what keeps the doc from silently drifting out of sync
with the code as harnesses are added or removed.

Matching is row-anchored and exact — a filename that is merely a *substring* of
an already-listed one (e.g. `run.py` inside `run_backtest.py`) does NOT satisfy
its own requirement, and an accidental duplicate row fails the "exactly one"
check.
"""
import re
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[2]
BACKTEST = REPO_ROOT / "backtest"
CANDIDATES = BACKTEST / "candidates"
REGISTRY = REPO_ROOT / "docs" / "backtesting-registry.md"

# First-cell token of a Markdown table row: `| `<token>` | ...`
_ROW_TOKEN = re.compile(r"(?m)^\|\s*`([^`]+)`\s*\|")


def _is_test(name: str) -> bool:
    return name.startswith("test_") or name.endswith("_test.py")


def _harness_files():
    """Non-test .py directly under backtest/ and backtest/research/ (non-recursive)."""
    files = []
    for d in (BACKTEST, BACKTEST / "research"):
        for p in sorted(d.glob("*.py")):
            if not _is_test(p.name):
                files.append(p)
    return files


def _token_for(p: Path) -> str:
    """The exact backtick token the registry uses: bare name, or `research/<name>`."""
    return p.relative_to(BACKTEST).as_posix()


def _registry_text() -> str:
    return REGISTRY.read_text()


def _row_tokens():
    """Every first-cell backtick token that appears as a table row, with counts."""
    counts = {}
    for t in _ROW_TOKEN.findall(_registry_text()):
        counts[t] = counts.get(t, 0) + 1
    return counts


def test_every_harness_has_exactly_one_row():
    rows = _row_tokens()
    problems = []
    for p in _harness_files():
        tok = _token_for(p)
        n = rows.get(tok, 0)
        if n != 1:
            problems.append(f"{tok}: {n} rows (want exactly 1)")
    assert not problems, (
        "backtest/ harness scripts must each have exactly one registry row in "
        f"docs/backtesting-registry.md (add one per the CLAUDE.md upkeep rule): {problems}"
    )


def test_every_candidate_study_has_exactly_one_row():
    rows = _row_tokens()
    problems = []
    for d in sorted(CANDIDATES.iterdir()):
        if not d.is_dir():
            continue
        n = rows.get(d.name, 0)
        if n != 1:
            problems.append(f"{d.name}: {n} rows (want exactly 1)")
    assert not problems, (
        "every backtest/candidates/<study>/ directory must have exactly one row "
        f"in the candidate-studies table: {problems}"
    )


def test_registry_rows_reference_no_nonexistent_target():
    """Inverse: no phantom rows — every table-row token maps to a real file or study."""
    phantom = []
    for tok in sorted(_row_tokens()):
        if tok.endswith(".py"):
            if not (BACKTEST / tok).exists():
                phantom.append(f"{tok} (no such file)")
        elif "." not in tok:
            # bare token → a candidate study directory
            if not (CANDIDATES / tok).is_dir():
                phantom.append(f"{tok} (no such candidate study)")
    assert not phantom, (
        "docs/backtesting-registry.md has rows pointing at things that do not exist: "
        f"{phantom}"
    )

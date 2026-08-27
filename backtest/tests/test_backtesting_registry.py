import re
from pathlib import Path
REPO_ROOT = Path(__file__).resolve().parents[2]
BACKTEST = REPO_ROOT / 'backtest'
CANDIDATES = BACKTEST / 'candidates'
REGISTRY = REPO_ROOT / 'docs' / 'backtesting-registry.md'
_ROW_TOKEN = re.compile('(?m)^\\|\\s*`([^`]+)`\\s*\\|')

def _is_test(name: str) -> bool:
    return name.startswith('test_') or name.endswith('_test.py')

def _harness_files():
    files = []
    for d in (BACKTEST, BACKTEST / 'research'):
        for p in sorted(d.glob('*.py')):
            if not _is_test(p.name):
                files.append(p)
    return files

def _token_for(p: Path) -> str:
    return p.relative_to(BACKTEST).as_posix()

def _registry_text() -> str:
    return REGISTRY.read_text()

def _row_tokens():
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
            problems.append(f'{tok}: {n} rows (want exactly 1)')
    assert not problems, f'backtest/ harness scripts must each have exactly one registry row in docs/backtesting-registry.md (add one per the CLAUDE.md upkeep rule): {problems}'

def test_every_candidate_study_has_exactly_one_row():
    rows = _row_tokens()
    problems = []
    for d in sorted(CANDIDATES.iterdir()):
        if not d.is_dir():
            continue
        n = rows.get(d.name, 0)
        if n != 1:
            problems.append(f'{d.name}: {n} rows (want exactly 1)')
    assert not problems, f'every backtest/candidates/<study>/ directory must have exactly one row in the candidate-studies table: {problems}'

def test_registry_rows_reference_no_nonexistent_target():
    phantom = []
    for tok in sorted(_row_tokens()):
        if (BACKTEST / tok).exists() or (CANDIDATES / tok).is_dir():
            continue
        phantom.append(tok)
    assert not phantom, f'docs/backtesting-registry.md has rows pointing at things that do not exist (no matching file under backtest/ or candidates/<study>/ dir): {phantom}'

import importlib.util
from pathlib import Path

import pytest

_SCRIPT = Path(__file__).resolve().parent / "strip_comments_preserve.py"
_SPEC = importlib.util.spec_from_file_location("strip_comments_preserve", _SCRIPT)
strip = importlib.util.module_from_spec(_SPEC)
assert _SPEC.loader is not None
_SPEC.loader.exec_module(strip)

CHARTS = (
    Path(__file__).resolve().parents[1]
    / "scheduler"
    / "static"
    / "ui"
    / "lightweight-charts.standalone.production.js"
)


def test_iter_source_files_skips_lightweight_charts_production_bundle():
    files = strip.iter_source_files()
    assert not any(
        path.endswith("lightweight-charts.standalone.production.js") for path in files
    )


def test_charts_bundle_keeps_apache_license_header():
    raw = CHARTS.read_bytes()
    assert strip.has_license_banner(raw)
    text = raw.decode("utf-8")
    assert text.lstrip().startswith("/*!")
    assert "@license" in text[:400]
    assert "Copyright (c) 2025 TradingView, Inc." in text
    assert "Apache License 2.0" in text


JS_CASES = [
    (
        "preserves_important_license_comment",
        "/*!\n * @license\n * Copyright (c) 2025 TradingView, Inc.\n */\n!function(){}\n",
        "/*!",
        ["/*!", "@license", "Copyright (c) 2025 TradingView, Inc.", "!function(){}"],
        [],
    ),
    (
        "drops_ordinary_block_comments",
        "/* ordinary */\nconst x = 1;\n",
        None,
        ["const x = 1;"],
        ["ordinary"],
    ),
]


@pytest.mark.parametrize(
    "src,starts_with,present,absent",
    [c[1:] for c in JS_CASES],
    ids=[c[0] for c in JS_CASES],
)
def test_js_stripper(src, starts_with, present, absent):
    out = strip.strip_js_css_html(src)
    if starts_with is not None:
        assert out.startswith(starts_with)
    for needle in present:
        assert needle in out
    for needle in absent:
        assert needle not in out
    assert strip.strip_js_css_html(out) == out


PY_CASES = [
    (
        "keeps_hash_lines_inside_triple_quoted_string",
        '''FAKE = """#!/usr/bin/env bash
# Fake gh for tests
set -euo pipefail
"""
x = 1  # inline
# full line comment
''',
        None,
        ["# Fake gh for tests", "#!/usr/bin/env bash", "x = 1"],
        ["# inline", "# full line comment"],
    ),
    (
        "keeps_hash_inside_single_quoted_triple_string",
        "s = '''\n## Summary\nkeep me\n'''\n",
        None,
        ["## Summary", "keep me"],
        [],
    ),
    (
        "keeps_hash_inside_same_line_string",
        'assert "never read by\\n    # map_composite_label" not in source\n',
        None,
        ["# map_composite_label"],
        [],
    ),
    (
        "strips_real_comments_and_docstrings",
        '''"""module doc"""
def f():
    """fn doc"""
    return 1  # trailing
''',
        None,
        ["return 1"],
        ["module doc", "fn doc", "trailing"],
    ),
    (
        "docstring_only_body_becomes_pass",
        '''def f():
    """only"""
''',
        None,
        ["pass"],
        ["only"],
    ),
    (
        "preserves_shebang",
        "#!/usr/bin/env python3\nprint(1)\n",
        "#!/usr/bin/env python3\n",
        ["print(1)"],
        [],
    ),
]


@pytest.mark.parametrize(
    "src,starts_with,present,absent",
    [c[1:] for c in PY_CASES],
    ids=[c[0] for c in PY_CASES],
)
def test_python_stripper(src, starts_with, present, absent):
    out = strip.strip_python(src)
    if starts_with is not None:
        assert out.startswith(starts_with)
    for needle in present:
        assert needle in out
    for needle in absent:
        assert needle not in out

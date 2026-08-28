import importlib.util
from pathlib import Path

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


def test_skip_list_includes_lightweight_charts_production_bundle():
    assert "lightweight-charts.standalone.production.js" in strip.SKIP_FILE_NAMES
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


def test_js_stripper_preserves_important_license_comment():
    src = "/*!\n * @license\n * Copyright (c) 2025 TradingView, Inc.\n */\n!function(){}\n"
    out = strip.strip_js_css_html(src)
    assert out.startswith("/*!")
    assert "@license" in out
    assert "Copyright (c) 2025 TradingView, Inc." in out
    assert "!function(){}" in out
    assert strip.strip_js_css_html(out) == out


def test_js_stripper_still_drops_ordinary_block_comments():
    src = "/* ordinary */\nconst x = 1;\n"
    assert "ordinary" not in strip.strip_js_css_html(src)
    assert "const x = 1;" in strip.strip_js_css_html(src)


def test_python_keeps_hash_lines_inside_triple_quoted_string():
    src = '''FAKE = """#!/usr/bin/env bash
# Fake gh for tests
set -euo pipefail
"""
x = 1  # inline
# full line comment
'''
    out = strip.strip_python(src)
    assert "# Fake gh for tests" in out
    assert "#!/usr/bin/env bash" in out
    assert "x = 1" in out
    assert "# inline" not in out
    assert "# full line comment" not in out


def test_python_keeps_hash_inside_single_quoted_triple_string():
    src = "s = '''\n## Summary\nkeep me\n'''\n"
    out = strip.strip_python(src)
    assert "## Summary" in out
    assert "keep me" in out


def test_python_keeps_hash_inside_same_line_string():
    src = 'assert "never read by\\n    # map_composite_label" not in source\n'
    out = strip.strip_python(src)
    assert "# map_composite_label" in out


def test_python_strips_real_comments_and_docstrings():
    src = '''"""module doc"""
def f():
    """fn doc"""
    return 1  # trailing
'''
    out = strip.strip_python(src)
    assert "module doc" not in out
    assert "fn doc" not in out
    assert "return 1" in out
    assert "trailing" not in out


def test_python_docstring_only_body_becomes_pass():
    src = '''def f():
    """only"""
'''
    out = strip.strip_python(src)
    assert "only" not in out
    assert "pass" in out


def test_python_preserves_shebang():
    src = "#!/usr/bin/env python3\nprint(1)\n"
    out = strip.strip_python(src)
    assert out.startswith("#!/usr/bin/env python3\n")

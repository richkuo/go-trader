#!/usr/bin/env python3

from __future__ import annotations

import ast
import io
import os
import re
import subprocess
import sys
import tokenize
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]

SKIP_DIR_NAMES = {
    ".git",
    ".venv",
    "node_modules",
    "__pycache__",
    ".pytest_cache",
    "dist",
    "build",
}

SKIP_FILE_SUFFIXES = {
    ".min.js",
    ".min.css",
}

SKIP_FILE_NAMES = {
    "charts.bundle.js",
    "lightweight-charts.standalone.production.js",
}

SOURCE_SUFFIXES = {
    ".go",
    ".py",
    ".sh",
    ".js",
    ".css",
    ".html",
    ".yaml",
    ".yml",
    ".toml",
    ".md",
}

def has_license_banner(raw: bytes) -> bool:
    try:
        head = raw[:4096].decode("utf-8")
    except UnicodeDecodeError:
        return False
    stripped = head.lstrip("\ufeff \t\r\n")
    return stripped.startswith("/*!") and "@license" in stripped[:500]


def git_show_main(rel: str) -> bytes | None:
    try:
        return subprocess.check_output(
            ["git", "show", f"origin/main:{rel}"],
            cwd=ROOT,
            stderr=subprocess.DEVNULL,
        )
    except subprocess.CalledProcessError:
        return None

def collapse_blank_lines(text: str) -> str:
    out: list[str] = []
    blank_run = 0
    for line in text.splitlines(keepends=True):
        if line.strip() == "":
            blank_run += 1
            if blank_run >= 3:
                continue
            out.append(line)
        else:
            blank_run = 0
            out.append(line)
    return "".join(out)

def preserve_go_comment(line: str) -> bool:
    stripped = line.lstrip()
    return stripped.startswith(("//go:", "//line "))

def strip_go(source: str) -> str:
    out: list[str] = []
    i = 0
    n = len(source)
    line_start_out = 0

    def peek(k: int = 0) -> str:
        j = i + k
        return source[j] if j < n else ""

    def append(ch: str) -> None:
        nonlocal line_start_out
        out.append(ch)
        if ch == "\n":
            line_start_out = len(out)

    while i < n:
        ch = source[i]
        if ch == '"':
            append(ch)
            i += 1
            while i < n:
                c = source[i]
                append(c)
                if c == "\\" and i + 1 < n:
                    append(source[i + 1])
                    i += 2
                    continue
                if c == '"':
                    i += 1
                    break
                i += 1
            continue
        if ch == "`":
            append(ch)
            i += 1
            while i < n and source[i] != "`":
                append(source[i])
                i += 1
            if i < n:
                append(source[i])
                i += 1
            continue
        if ch == "'" and peek(1) == "'":
            append("''")
            i += 2
            while i + 1 < n:
                if source[i : i + 2] == "''":
                    append("''")
                    i += 2
                    break
                append(source[i])
                i += 1
            continue
        if ch == "/" and peek(1) == "/":
            comment_end = source.find("\n", i)
            if comment_end == -1:
                comment_end = n
            comment_text = source[i:comment_end]
            if preserve_go_comment(comment_text):
                if comment_end < n:
                    for c in source[i : comment_end + 1]:
                        append(c)
                    i = comment_end + 1
                else:
                    for c in source[i:]:
                        append(c)
                    i = n
                continue
            line_start = source.rfind("\n", 0, i) + 1
            before = source[line_start:i].strip()
            if before:
                while out and out[-1] in " \t":
                    out.pop()
                i = comment_end + 1 if comment_end < n else n
                if comment_end < n:
                    append("\n")
            else:
                del out[line_start_out:]
                i = comment_end + 1 if comment_end < n else n
            continue
        if ch == "/" and peek(1) == "*":
            line_start = source.rfind("\n", 0, i) + 1
            before = source[line_start:i].strip()
            end = source.find("*/", i + 2)
            if end == -1:
                if not before:
                    del out[line_start_out:]
                i = n
            else:
                if not before:
                    del out[line_start_out:]
                i = end + 2
            continue
        append(ch)
        i += 1
    return collapse_blank_lines("".join(out))

def _docstring_line_range(node: ast.AST, lines: list[str]) -> tuple[int, int] | None:
    body = getattr(node, "body", None)
    if not body:
        return None
    first = body[0]
    if not isinstance(first, ast.Expr):
        return None
    val = first.value
    if isinstance(val, ast.Constant) and isinstance(val.value, str):
        start = first.lineno - 1
        end = (first.end_lineno or first.lineno) - 1
        return start, end
    if hasattr(ast, "Str") and isinstance(val, ast.Str):
        start = first.lineno - 1
        end = (first.end_lineno or first.lineno) - 1
        return start, end
    return None

def _leading_indent(line: str) -> str:
    m = re.match(r"[ \t]*", line)
    return m.group(0) if m else ""

def _docstring_only_body(node: ast.AST) -> bool:
    body = getattr(node, "body", None)
    if not body or len(body) != 1:
        return False
    first = body[0]
    if not isinstance(first, ast.Expr):
        return False
    val = first.value
    return isinstance(val, ast.Constant) and isinstance(val.value, str)

def _strip_python_docstrings(source: str) -> str:
    lines = source.splitlines(keepends=True)
    delete: set[int] = set()
    replace: dict[int, str] = {}
    try:
        tree = ast.parse(source)
    except SyntaxError:
        return source
    nodes: list[ast.AST] = []
    if isinstance(tree, ast.Module):
        nodes.append(tree)
    for node in ast.walk(tree):
        if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef, ast.ClassDef)):
            nodes.append(node)
    seen: set[tuple[int, int]] = set()
    for node in nodes:
        r = _docstring_line_range(node, lines)
        if not r or r in seen:
            continue
        seen.add(r)
        start, end = r
        if isinstance(node, ast.Module) or not _docstring_only_body(node):
            delete.update(range(start, end + 1))
            continue
        indent = _leading_indent(lines[start])
        replace[start] = f"{indent}pass\n"
        delete.update(range(start + 1, end + 1))
    out_lines: list[str] = []
    for idx, line in enumerate(lines):
        if idx in replace:
            out_lines.append(replace[idx])
            continue
        if idx in delete:
            continue
        out_lines.append(line)
    return "".join(out_lines)


def _line_start_offsets(source: str) -> list[int]:
    offsets = [0]
    for i, ch in enumerate(source):
        if ch == "\n":
            offsets.append(i + 1)
    return offsets


def _strip_python_hash_comments_tokenize(source: str) -> str | None:
    try:
        tokens = list(tokenize.generate_tokens(io.StringIO(source).readline))
    except (tokenize.TokenError, IndentationError, SyntaxError):
        return None
    offsets = _line_start_offsets(source)
    n = len(source)

    def pos_offset(lineno: int, col: int) -> int:
        if lineno < 1 or lineno - 1 >= len(offsets):
            return n
        off = offsets[lineno - 1] + col
        return n if off > n else off

    delete_ranges: list[tuple[int, int]] = []
    for tok in tokens:
        if tok.type != tokenize.COMMENT:
            continue
        if tok.string.startswith("#!") and tok.start == (1, 0):
            continue
        start = pos_offset(tok.start[0], tok.start[1])
        end = pos_offset(tok.end[0], tok.end[1])
        line_start = offsets[tok.start[0] - 1] if tok.start[0] >= 1 else 0
        before = source[line_start:start]
        if before.strip() == "":
            if tok.start[0] < len(offsets):
                next_line = offsets[tok.start[0]] if tok.start[0] < len(offsets) else n
                delete_ranges.append((line_start, next_line))
            else:
                delete_ranges.append((line_start, n))
        else:
            trimmed = start
            while trimmed > line_start and source[trimmed - 1] in " \t":
                trimmed -= 1
            delete_ranges.append((trimmed, end))
    if not delete_ranges:
        return source
    parts: list[str] = []
    cursor = 0
    for a, b in sorted(delete_ranges):
        if a < cursor:
            continue
        parts.append(source[cursor:a])
        cursor = b
    parts.append(source[cursor:])
    return "".join(parts)


def _update_python_string_state(
    line: str,
    in_single: bool,
    in_double: bool,
    in_triple_single: bool,
    in_triple_double: bool,
) -> tuple[bool, bool, bool, bool]:
    escape = False
    i = 0
    while i < len(line):
        if escape:
            escape = False
            i += 1
            continue
        ch = line[i]
        if in_single or in_double or in_triple_single or in_triple_double:
            if ch == "\\" and (in_single or in_double):
                escape = True
                i += 1
                continue
            if in_triple_single and line.startswith("'''", i):
                in_triple_single = False
                i += 3
                continue
            if in_triple_double and line.startswith('"""', i):
                in_triple_double = False
                i += 3
                continue
            if in_single and ch == "'":
                in_single = False
            elif in_double and ch == '"':
                in_double = False
            i += 1
            continue
        if line.startswith("'''", i):
            in_triple_single = True
            i += 3
            continue
        if line.startswith('"""', i):
            in_triple_double = True
            i += 3
            continue
        if ch == "'":
            in_single = True
            i += 1
            continue
        if ch == '"':
            in_double = True
            i += 1
            continue
        i += 1
    return in_single, in_double, in_triple_single, in_triple_double


def _strip_python_hash_comments_linewise(source: str) -> str:
    in_single = in_double = in_triple_single = in_triple_double = False
    out: list[str] = []
    for line in source.splitlines(keepends=True):
        in_string = in_single or in_double or in_triple_single or in_triple_double
        if in_string:
            out.append(line)
        else:
            stripped = line.lstrip()
            if stripped.startswith("#!"):
                out.append(line)
            elif stripped.startswith("#"):
                in_single, in_double, in_triple_single, in_triple_double = (
                    _update_python_string_state(
                        line, in_single, in_double, in_triple_single, in_triple_double
                    )
                )
                continue
            elif "#" in line:
                out.append(_strip_python_inline_hash(line))
            else:
                out.append(line)
        in_single, in_double, in_triple_single, in_triple_double = (
            _update_python_string_state(
                line, in_single, in_double, in_triple_single, in_triple_double
            )
        )
    return "".join(out)


def _strip_python_hash_comments(source: str) -> str:
    tokenized = _strip_python_hash_comments_tokenize(source)
    if tokenized is not None:
        return tokenized
    return _strip_python_hash_comments_linewise(source)


def strip_python(source: str) -> str:
    text = _strip_python_docstrings(source)
    text = _strip_python_hash_comments(text)
    return collapse_blank_lines(text)

def _strip_python_inline_hash(line: str) -> str:
    in_single = False
    in_double = False
    in_triple_single = False
    in_triple_double = False
    escape = False
    i = 0
    while i < len(line):
        if escape:
            escape = False
            i += 1
            continue
        ch = line[i]
        if in_single or in_double or in_triple_single or in_triple_double:
            if ch == "\\" and (in_single or in_double):
                escape = True
                i += 1
                continue
            if in_triple_single and line.startswith("'''", i):
                in_triple_single = False
                i += 3
                continue
            if in_triple_double and line.startswith('"""', i):
                in_triple_double = False
                i += 3
                continue
            if in_single and ch == "'":
                in_single = False
            elif in_double and ch == '"':
                in_double = False
            i += 1
            continue
        if line.startswith("'''", i):
            in_triple_single = True
            i += 3
            continue
        if line.startswith('"""', i):
            in_triple_double = True
            i += 3
            continue
        if ch == "'":
            in_single = True
            i += 1
            continue
        if ch == '"':
            in_double = True
            i += 1
            continue
        if ch == "#":
            trimmed = line[:i].rstrip()
            nl = "\n" if line.endswith("\n") else ""
            if not trimmed:
                return nl
            return trimmed + nl
        i += 1
    return line

def strip_shell_or_yaml(source: str) -> str:
    out: list[str] = []
    for line in source.splitlines(keepends=True):
        stripped = line.lstrip()
        if stripped.startswith("#!"):
            out.append(line)
            continue
        if stripped.startswith("#"):
            continue
        out.append(line)
    return collapse_blank_lines("".join(out))

def strip_js_css_html(source: str) -> str:
    out: list[str] = []
    i = 0
    n = len(source)
    line_start_out = 0
    in_str: str | None = None
    escape = False

    def append(ch: str) -> None:
        nonlocal line_start_out
        out.append(ch)
        if ch == "\n":
            line_start_out = len(out)

    while i < n:
        if in_str:
            c = source[i]
            append(c)
            if escape:
                escape = False
            elif c == "\\":
                escape = True
            elif c == in_str:
                in_str = None
            i += 1
            continue
        if source.startswith("/*!", i):
            end = source.find("*/", i + 3)
            if end == -1:
                for c in source[i:]:
                    append(c)
                i = n
            else:
                for c in source[i : end + 2]:
                    append(c)
                i = end + 2
            continue
        if source.startswith("/*", i):
            line_start = source.rfind("\n", 0, i) + 1
            before = source[line_start:i].strip()
            end = source.find("*/", i + 2)
            if not before:
                del out[line_start_out:]
            i = n if end == -1 else end + 2
            continue
        if source.startswith("//", i):
            line_start = source.rfind("\n", 0, i) + 1
            before = source[line_start:i].strip()
            end = source.find("\n", i)
            if end == -1:
                end = n
            if before:
                while out and out[-1] in " \t":
                    out.pop()
                i = end + 1 if end < n else n
                if end < n:
                    append("\n")
            else:
                del out[line_start_out:]
                i = end + 1 if end < n else n
            continue
        c = source[i]
        if c in ("'", '"', "`"):
            in_str = c
            append(c)
            i += 1
            continue
        append(c)
        i += 1
    return collapse_blank_lines("".join(out))

def strip_file(rel: str, raw: bytes) -> bytes:
    text = raw.decode("utf-8")
    suffix = Path(rel).suffix.lower()
    if suffix == ".go":
        out = strip_go(text)
    elif suffix == ".py":
        out = strip_python(text)
    elif suffix in {".sh", ".yaml", ".yml"}:
        out = strip_shell_or_yaml(text)
    elif suffix in {".js", ".css", ".html"}:
        out = strip_js_css_html(text)
    elif suffix == ".toml":
        out = strip_shell_or_yaml(text)
    elif suffix == ".md":
        out = text
    else:
        out = text
    return out.encode("utf-8")

def iter_source_files() -> list[str]:
    files: list[str] = []
    for dirpath, dirnames, filenames in os.walk(ROOT):
        dirnames[:] = sorted(d for d in dirnames if d not in SKIP_DIR_NAMES)
        for name in sorted(filenames):
            if name in SKIP_FILE_NAMES:
                continue
            if any(name.endswith(s) for s in SKIP_FILE_SUFFIXES):
                continue
            path = Path(dirpath) / name
            rel = path.relative_to(ROOT).as_posix()
            if path.suffix.lower() not in SOURCE_SUFFIXES:
                continue
            files.append(rel)
    return files

def apply_post_strip_fixes() -> int:
    patches: list[tuple[str, str, str]] = [
        (
            "shared_scripts/test_regime_wiring.py",
            "from regime import latest_regime, latest_regime_composite\n",
            "from regime import latest_regime, latest_regime_composite, map_composite_label\n",
        ),
        (
            "shared_scripts/test_regime_wiring.py",
            """def test_1409_advisory_only_comment_was_revoked_for_gating_and_sizing():
    source = (_SHARED_TOOLS / "regime.py").read_text()
    assert "never read by\\n    # map_composite_label, gating, or sizing" not in source
    assert "gating, or sizing" not in source
    assert "#1411" in source
    assert "map_composite_label" in source
    label_fn_start = source.index("def map_composite_label")
    label_fn = source[label_fn_start : source.index("\\ndef ", label_fn_start + 1)]
    assert "hurst" not in label_fn


def test_regime_label_string_is_safe_for_output_field():""",
            """def test_map_composite_label_does_not_reference_hurst():
    import inspect
    body = inspect.getsource(map_composite_label)
    assert body is not None
    assert "hurst" not in body


def test_regime_label_string_is_safe_for_output_field():""",
        ),
        (
            "backtest/tests/test_regime_default_tables.py",
            r'StopLoss:\s*map\[string\]RegimeATREntry\s*\{(.*?)\n\t\},\n\t// #870',
            r"StopLoss:\s*map\[string\]RegimeATREntry\s*\{(.*?)\},\s*\n\s*Trailing:",
        ),
        (
            "backtest/tests/test_options_adapter_parity.py",
            r'underlying\.upper\(\) == "BTC":\s*\n\s*return round\(target_strike, (-?\d+)\)',
            r'underlying\.upper\(\) == ["\']BTC["\']:\s*(?:\n\s*)?return round\(target_strike, (-?\d+)\)',
        ),
        (
            "scheduler/ui_tuning_page_test.go",
            "\t\t`Always re-read live config at render time`,\n\t\t`memoized server-side`,\n\t\t`detailReloadPending`,\n\t\t`Defer clearing until replacement content is ready`,\n",
            "\t\t`detailReloadPending`,\n",
        ),
    ]
    argparse_old = [
        'argparse.ArgumentParser(description=__doc__.splitlines()[0])',
        'argparse.ArgumentParser(description=__doc__.split("\\n")[0])',
        "argparse.ArgumentParser(description=__doc__)",
    ]
    argparse_files = [
        "backtest/auto_suggest.py",
        "backtest/tune_live.py",
        "backtest/candidates/rahtf_1054/entry_condition_split.py",
        "backtest/research/hurst_1410_gate_calibration.py",
        "backtest/research/hurst_1422_gate_power.py",
        "backtest/research/hurst_1424_gate_resolution.py",
        "backtest/research/hurst_1426_two_sided_sort.py",
        "backtest/research/regime_1152_exit_retune.py",
        "scripts/bench_hl_batch.py",
    ]
    changed = 0
    for rel, old, new in patches:
        path = ROOT / rel
        if not path.exists():
            continue
        text = path.read_text()
        if old not in text:
            continue
        path.write_text(text.replace(old, new, 1))
        changed += 1
    js_comment_blocks = [
        "// Raw empty baseline vs Go-merged live params (common args-form) is current\n// once defaults are applied to both sides — not drifted.\n",
        "// Explicit override recorded at run start still matches merged live.\n",
        "// Genuine post-run live edit still reports drifted.\n",
    ]
    tuning = ROOT / "scheduler/ui_tuning_page_test.go"
    tuning_text = tuning.read_text()
    tuning_new = tuning_text
    for block in js_comment_blocks:
        tuning_new = tuning_new.replace(block, "")
    if tuning_new != tuning_text:
        tuning.write_text(tuning_new)
        changed += 1
    for rel in argparse_files:
        path = ROOT / rel
        text = path.read_text()
        new_text = text
        for old in argparse_old:
            new_text = new_text.replace(old, "argparse.ArgumentParser()", 1)
        if new_text != text:
            path.write_text(new_text)
            changed += 1
    return changed


def main() -> int:
    changed = 0
    for rel in iter_source_files():
        baseline = git_show_main(rel)
        if baseline is None:
            baseline = (ROOT / rel).read_bytes()
        current = (ROOT / rel).read_bytes()
        if has_license_banner(baseline):
            if current != baseline:
                (ROOT / rel).write_bytes(baseline)
                changed += 1
                print(rel)
            continue
        stripped = strip_file(rel, baseline)
        if stripped != current:
            (ROOT / rel).write_bytes(stripped)
            changed += 1
            print(rel)
    changed += apply_post_strip_fixes()
    print(f"updated {changed} files", file=sys.stderr)
    return 0

if __name__ == "__main__":
    raise SystemExit(main())

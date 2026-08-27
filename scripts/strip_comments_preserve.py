#!/usr/bin/env python3

from __future__ import annotations

import ast
import os
import re
import subprocess
import sys
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

def strip_python(source: str) -> str:
    lines = source.splitlines(keepends=True)
    delete = set()
    replace: dict[int, str] = {}
    try:
        tree = ast.parse(source)
    except SyntaxError:
        tree = None
    if tree is not None:
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
        if "#" not in line:
            out_lines.append(line)
            continue
        stripped = line.lstrip()
        if stripped.startswith("#!"):
            out_lines.append(line)
            continue
        if stripped.startswith("#"):
            continue
        out_lines.append(_strip_python_inline_hash(line))
    return collapse_blank_lines("".join(out_lines))

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
            """def test_hurst_label_gate_stays_separate_from_composite_label():
    source = (_SHARED_TOOLS / "regime.py").read_text()
    label_fn_start = source.index("def map_composite_label")
    label_fn = source[label_fn_start : source.index("\\ndef ", label_fn_start + 1)]
    assert "hurst" not in label_fn
    composite_start = source.index("def latest_regime_composite")
    composite_fn = source[composite_start : source.index("\\ndef ", composite_start + 1)]
    assert "hurst_exponent" in composite_fn

def test_regime_label_string_is_safe_for_output_field():""",
        ),
        (
            "backtest/tests/test_hurst_1424_gate_resolution.py",
            """def test_stage_0_is_scored_on_net_return_so_it_stays_comparable():
    assert study.joint_separation_verdict.__doc__
    assert "NET RETURN" in study.joint_separation_verdict.__doc__""",
            """def test_stage_0_is_scored_on_net_return_so_it_stays_comparable(monkeypatch):
    sentinel = object()
    monkeypatch.setattr(study1422, "joint_separation_verdict", lambda *a, **k: sentinel)
    assert study.joint_separation_verdict([], 512) is sentinel""",
        ),
        (
            "backtest/tests/test_hurst_1426_two_sided_sort.py",
            """def test_stage_0_is_the_deliberately_inherited_one_sided_exception():
    assert study.joint_separation_verdict.__doc__
    assert "ONE-SIDED" in study.joint_separation_verdict.__doc__
    assert "#1412" in study.joint_separation_verdict.__doc__""",
            """def test_stage_0_is_the_deliberately_inherited_one_sided_exception(monkeypatch):
    sentinel = object()
    monkeypatch.setattr(study1422, "joint_separation_verdict", lambda *a, **k: sentinel)
    assert study.joint_separation_verdict([], 512) is sentinel""",
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
        stripped = strip_file(rel, baseline)
        current = (ROOT / rel).read_bytes()
        if stripped != current:
            (ROOT / rel).write_bytes(stripped)
            changed += 1
            print(rel)
    changed += apply_post_strip_fixes()
    print(f"updated {changed} files", file=sys.stderr)
    return 0

if __name__ == "__main__":
    raise SystemExit(main())

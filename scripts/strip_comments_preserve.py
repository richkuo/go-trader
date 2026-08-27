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


def preserve_go_comment(line: str) -> bool:
    stripped = line.lstrip()
    return stripped.startswith(("//go:", "//line "))


def strip_go(source: str) -> str:
    out: list[str] = []
    i = 0
    n = len(source)

    def peek(k: int = 0) -> str:
        j = i + k
        return source[j] if j < n else ""

    while i < n:
        ch = source[i]
        if ch == '"':
            out.append(ch)
            i += 1
            while i < n:
                c = source[i]
                out.append(c)
                if c == "\\" and i + 1 < n:
                    out.append(source[i + 1])
                    i += 2
                    continue
                if c == '"':
                    i += 1
                    break
                i += 1
            continue
        if ch == "`":
            out.append(ch)
            i += 1
            while i < n and source[i] != "`":
                out.append(source[i])
                i += 1
            if i < n:
                out.append(source[i])
                i += 1
            continue
        if ch == "'" and peek(1) == "'":
            out.append("''")
            i += 2
            while i + 1 < n:
                if source[i : i + 2] == "''":
                    out.append("''")
                    i += 2
                    break
                out.append(source[i])
                i += 1
            continue
        if ch == "/" and peek(1) == "/":
            line_start = source.rfind("\n", 0, i) + 1
            line = source[line_start:i]
            if preserve_go_comment(source[i:].split("\n", 1)[0]):
                end = source.find("\n", i)
                if end == -1:
                    out.append(source[i:])
                    i = n
                else:
                    out.append(source[i : end + 1])
                    i = end + 1
                continue
            end = source.find("\n", i)
            if end == -1:
                while out and out[-1] in " \t":
                    out.pop()
                i = n
            else:
                while out and out[-1] in " \t":
                    out.pop()
                if out and out[-1] != "\n" and (not out or out[-1] != "\n"):
                    pass
                i = end + 1
                if not out or out[-1] != "\n":
                    out.append("\n")
            continue
        if ch == "/" and peek(1) == "*":
            end = source.find("*/", i + 2)
            if end == -1:
                i = n
            else:
                i = end + 2
            continue
        out.append(ch)
        i += 1
    return "".join(out)


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
    return "".join(out_lines)


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
    return "".join(out)


def strip_js_css_html(source: str) -> str:
    out: list[str] = []
    i = 0
    n = len(source)
    in_str: str | None = None
    escape = False
    while i < n:
        if in_str:
            c = source[i]
            out.append(c)
            if escape:
                escape = False
            elif c == "\\":
                escape = True
            elif c == in_str:
                in_str = None
            i += 1
            continue
        if source.startswith("/*", i):
            end = source.find("*/", i + 2)
            i = n if end == -1 else end + 2
            continue
        if source.startswith("//", i):
            end = source.find("\n", i)
            if end == -1:
                while out and out[-1] in " \t":
                    out.pop()
                break
            while out and out[-1] in " \t":
                out.pop()
            i = end + 1
            if not out or out[-1] != "\n":
                out.append("\n")
            continue
        c = source[i]
        if c in ("'", '"', "`"):
            in_str = c
            out.append(c)
            i += 1
            continue
        out.append(c)
        i += 1
    return "".join(out)


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
    print(f"updated {changed} files", file=sys.stderr)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

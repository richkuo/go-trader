#!/usr/bin/env python3
import argparse
import ast
import bisect
import collections
import concurrent.futures
import fnmatch
import hashlib
import importlib.util
import io
import json
import multiprocessing
import os
import pathlib
import queue
import random
import re
import shlex
import shutil
import signal
import subprocess
import sys
import time
import threading
import tokenize

ROOT = pathlib.Path(__file__).resolve().parent.parent
GO_TOOLCHAIN = "go1.26.2"
COVERAGE_VERSION = "7.10.7"
LINUX_IMAGE = "golang:1.26.2"
MODULE = "trading-scheduler"
MODULE_DIR = "scheduler"
CI_FILE = ".github/workflows/ci.yml"
PY_COLLECTIONS = (
    ("default", []),
    ("shared_scripts", ["shared_scripts/"]),
    ("options", ["shared_strategies/options/test_strategies.py"]),
)
PROTECTED_HINTS = (
    "venue", "partial", "reject", "unknown", "fill", "sizing", "size", "pnl", "fee", "funding", "parity",
    "paper", "live", "migrat", "kill", "stop", "protect", "liquidat", "circuit", "drawdown", "risk", "hedge",
    "margin", "cash", "money", "notional", "exposure", "close", "ledger",
)
GO_DECL = re.compile(r"^func (Test\w+)\((\w+) \*testing\.(T|M)\)", re.M)
OUTCOME_LINE = re.compile(r"^\s*--- (PASS|FAIL|SKIP): (\S+)")
TRACE_LINE = re.compile(r"^\++@A@([^@]*)@(\d+)@")
FAMILIES = ("condition_negation", "comparison_boundary", "arithmetic_swap", "constant_change", "statement_removal", "return_value")


class AuditError(Exception):
    pass


def run(cmd, cwd=None, env=None, check=True, capture=True, timeout=None):
    p = subprocess.run(cmd, cwd=cwd, env=env, text=True, capture_output=capture, timeout=timeout)
    if check and p.returncode != 0:
        raise AuditError(f"{shlex.join(map(str, cmd))} exited {p.returncode}: {(p.stderr or '')[-4000:]}")
    return p


def git(*args, cwd=ROOT, check=True):
    return run(["git", *args], cwd=cwd, check=check).stdout


def sha(text):
    return hashlib.sha256(text.encode()).hexdigest()[:16]


def write_json(path, data):
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_suffix(path.suffix + ".tmp")
    tmp.write_text(json.dumps(data, indent=1, sort_keys=True) + "\n")
    tmp.replace(path)


def read_json(path):
    return json.loads(path.read_text())


def append_jsonl(path, rows):
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("a") as fh:
        for row in rows:
            fh.write(json.dumps(row, sort_keys=True) + "\n")


def read_jsonl(path):
    if not path.exists():
        return []
    out = []
    for line in path.read_text().splitlines():
        if line.strip():
            out.append(json.loads(line))
    return out


def base_env():
    env = {k: v for k, v in os.environ.items() if not k.startswith(("COVERAGE_", "PYTEST_", "GO_TRADER_"))}
    for k in ("VIRTUAL_ENV", "GOFLAGS", "GOCOVERDIR", "BASH_ENV", "PYTHONPATH", "UV_PROJECT_ENVIRONMENT", "CONDA_PREFIX"):
        env.pop(k, None)
    env["GOTOOLCHAIN"] = GO_TOOLCHAIN
    env["GOPROXY"] = "off"
    env["GOFLAGS"] = "-mod=readonly"
    env["UV_OFFLINE"] = "1"
    env["PYTHONDONTWRITEBYTECODE"] = "1"
    env["PYTHONHASHSEED"] = "0"
    return env


def is_test_file(rel):
    p = pathlib.PurePosixPath(rel)
    name = p.name
    if name.endswith("_test.go") or name == "conftest.py":
        return True
    if name.startswith("test_") and name.endswith((".py", ".sh")):
        return True
    if name.endswith("_test.py"):
        return True
    return any(part in ("tests", "fixtures", "testdata") for part in p.parts[:-1])


def ci_go_modes():
    lines = (ROOT / CI_FILE).read_text().splitlines()
    job = step = None
    modes = []
    for ln in lines:
        m = re.match(r"^  ([A-Za-z0-9_-]+):\s*$", ln)
        if m:
            job, step = m[1], None
            continue
        m = re.match(r"^\s+- name:\s*(.+?)\s*$", ln)
        if m:
            step = m[1]
            continue
        if not re.search(r"\bgo (-C scheduler )?test\b", ln):
            continue
        cmd = re.sub(r"^\s*(run:\s*)?", "", ln)
        cmd = cmd.replace("cd scheduler && ", "")
        cmd = re.split(r"\s+2>&1|\s+\|\s", cmd)[0]
        toks = shlex.split(cmd)
        i = toks.index("test")
        flags = toks[i + 1:]
        spec = {"tags": "", "race": False, "run": None, "skip": None, "packages": []}
        j = 0
        while j < len(flags):
            f = flags[j]
            if f == "-race":
                spec["race"] = True
            elif f in ("-tags", "-run", "-skip", "-timeout", "-count"):
                key = f[1:]
                if key in ("tags", "run", "skip"):
                    spec[key] = flags[j + 1]
                j += 1
            elif f.startswith("-"):
                pass
            else:
                spec["packages"].append(f)
            j += 1
        n = sum(1 for m2 in modes if m2["job"] == job) + 1
        spec.update({"name": f"go/{job}/{n}", "job": job, "step": step, "command": cmd.strip()})
        modes.append(spec)
    if not modes:
        raise AuditError(f"no go test commands found in {CI_FILE}")
    return modes


def go_list(tree, tags, goos):
    env = base_env()
    env["GOOS"] = goos
    env["GOARCH"] = "arm64"
    env["CGO_ENABLED"] = "0" if goos == "linux" else env.get("CGO_ENABLED", "1")
    args = ["go", "list", "-json"]
    if tags:
        args += ["-tags", tags]
    args += ["./..."]
    out = run(args, cwd=tree / MODULE_DIR, env=env).stdout
    dec = json.JSONDecoder()
    pkgs = []
    idx = 0
    while idx < len(out):
        while idx < len(out) and out[idx].isspace():
            idx += 1
        if idx >= len(out):
            break
        obj, idx = dec.raw_decode(out, idx)
        pkgs.append(obj)
    return pkgs


def go_tests_in_files(pkgdir, files):
    names = {}
    for f in files:
        text = (pathlib.Path(pkgdir) / f).read_text()
        for m in GO_DECL.finditer(text):
            if m[3] == "T":
                names[m[1]] = f
    return names


def go_declarations(tree):
    out = []
    for path in sorted((tree / MODULE_DIR).rglob("*_test.go")):
        text = path.read_text()
        constraint = ""
        first = text.split("\n", 1)[0]
        if first.startswith("//go:build "):
            constraint = first[len("//go:build "):]
        for m in GO_DECL.finditer(text):
            if m[3] != "T":
                continue
            line = text.count("\n", 0, m.start()) + 1
            out.append({"name": m[1], "file": str(path.relative_to(tree)), "line": line, "constraint": constraint})
    return out


def go_test_body(text, name):
    m = re.search(rf"^func {re.escape(name)}\(", text, re.M)
    if not m:
        return ""
    nxt = re.search(r"^func ", text[m.end():], re.M)
    return text[m.start(): m.end() + nxt.start()] if nxt else text[m.start():]


def subprocess_entry_point(body):
    lines = [ln.strip() for ln in body.splitlines()[1:6] if ln.strip()]
    if not lines:
        return False
    return bool(re.match(r'if os\.Getenv\("[A-Z0-9_]+"\) != "1" \{', lines[0])) and lines[1] in ("return", "t.Skip()", 't.SkipNow()')


def py_collect(tree, suite, args, out):
    env = base_env()
    cmd = [str(tree / ".venv/bin/python3"), "scripts/check_pytest_shards.py", "collect", suite, "--out", str(out)]
    if args:
        cmd += ["--", *args]
    run(cmd, cwd=tree, env=env)
    return read_json(out)["nodeids"]


def tool_versions(tree):
    env = base_env()
    v = {}
    v["go"] = run(["go", "version"], cwd=tree / MODULE_DIR, env=env).stdout.strip()
    v["python"] = run([str(tree / ".venv/bin/python3"), "--version"], env=env).stdout.strip()
    v["pytest"] = run([str(tree / ".venv/bin/python3"), "-m", "pytest", "--version"], env=env).stdout.strip() or run([str(tree / ".venv/bin/python3"), "-m", "pytest", "--version"], env=env).stderr.strip()
    v["uv"] = run(["uv", "--version"], env=env).stdout.strip()
    v["coverage"] = COVERAGE_VERSION
    v["bash"] = run([shutil.which("bash"), "--version"], env=env).stdout.splitlines()[0]
    d = run(["docker", "image", "inspect", LINUX_IMAGE, "--format", "{{.Id}} {{.Os}}/{{.Architecture}}"], check=False)
    v["linux_image"] = f"{LINUX_IMAGE} {d.stdout.strip()}" if d.returncode == 0 else f"{LINUX_IMAGE} unavailable"
    v["host"] = f"{os.uname().sysname} {os.uname().release} {os.uname().machine}"
    return v


class PyMutator(ast.NodeVisitor):
    CMP = {ast.Eq: (ast.NotEq, "==", "!="), ast.NotEq: (ast.Eq, "!=", "=="), ast.Is: (ast.IsNot, "is", "is not"), ast.IsNot: (ast.Is, "is not", "is"), ast.In: (ast.NotIn, "in", "not in"), ast.NotIn: (ast.In, "not in", "in")}
    BOUND = {ast.Lt: ("<", "<="), ast.LtE: ("<=", "<"), ast.Gt: (">", ">="), ast.GtE: (">=", ">")}
    ARITH = {ast.Add: ("+", "-"), ast.Sub: ("-", "+"), ast.Mult: ("*", "/"), ast.Div: ("/", "*")}

    def __init__(self, rel, src):
        self.rel = rel
        self.src = src
        self.raw = src.encode()
        self.starts = [0]
        for line in self.raw.split(b"\n")[:-1]:
            self.starts.append(self.starts[-1] + len(line) + 1)
        self.sites = []
        self.stmt = []
        self.func = []

    def off(self, line, col):
        return self.starts[line - 1] + col

    def seg(self, node):
        return self.raw[self.off(node.lineno, node.col_offset): self.off(node.end_lineno, node.end_col_offset)].decode()

    def add(self, start, end, family, op, repl, line):
        orig = self.raw[start:end].decode()
        if orig == repl:
            return
        stmt_line = self.stmt[-1] if self.stmt else line
        self.sites.append({"file": self.rel, "line": line, "stmt_line": stmt_line, "start": start, "end": end, "family": family, "op": op, "orig": orig, "repl": repl, "function": self.func[-1] if self.func else ""})

    def node_add(self, node, family, op, repl):
        self.add(self.off(node.lineno, node.col_offset), self.off(node.end_lineno, node.end_col_offset), family, op, repl, node.lineno)

    def op_between(self, left, right, texts):
        a = self.off(left.end_lineno, left.end_col_offset)
        b = self.off(right.lineno, right.col_offset)
        gap = self.raw[a:b].decode()
        toks = []
        try:
            for t in tokenize.generate_tokens(io.StringIO(gap).readline):
                if t.type in (tokenize.OP, tokenize.NAME):
                    toks.append(t)
        except (tokenize.TokenError, IndentationError, SyntaxError):
            return None
        words = [t.string for t in toks if t.string not in ("(", ")")]
        joined = " ".join(words)
        if joined not in texts:
            return None
        first = next(t for t in toks if t.string not in ("(", ")"))
        last = [t for t in toks if t.string not in ("(", ")")][-1]
        lines = gap.split("\n")
        def pos(row, col):
            return a + len("\n".join(lines[: row - 1]).encode()) + (1 if row > 1 else 0) + len(lines[row - 1][:col].encode())
        return pos(*first.start), pos(*last.end)

    def generic_visit(self, node):
        if isinstance(node, ast.stmt):
            self.stmt.append(node.lineno)
            super().generic_visit(node)
            self.stmt.pop()
        else:
            super().generic_visit(node)

    def visit_FunctionDef(self, node):
        self.func.append(node.name)
        self.generic_visit(node)
        self.func.pop()

    visit_AsyncFunctionDef = visit_FunctionDef

    def visit_If(self, node):
        self.stmt.append(node.lineno)
        self.node_add(node.test, "condition_negation", "negate_if", "not (" + self.seg(node.test) + ")")
        self.stmt.pop()
        self.generic_visit(node)

    def visit_While(self, node):
        self.stmt.append(node.lineno)
        self.node_add(node.test, "condition_negation", "negate_while", "not (" + self.seg(node.test) + ")")
        self.stmt.pop()
        self.generic_visit(node)

    def visit_IfExp(self, node):
        self.node_add(node.test, "condition_negation", "negate_ifexp", "(not (" + self.seg(node.test) + "))")
        self.generic_visit(node)

    def visit_Compare(self, node):
        if len(node.ops) == 1:
            op = type(node.ops[0])
            right = node.comparators[0]
            if op in self.CMP:
                _, a, b = self.CMP[op]
                span = self.op_between(node.left, right, (a,))
                if span:
                    self.add(span[0], span[1], "condition_negation", f"{a}->{b}", b, node.lineno)
            if op in self.BOUND:
                a, b = self.BOUND[op]
                span = self.op_between(node.left, right, (a,))
                if span:
                    self.add(span[0], span[1], "comparison_boundary", f"{a}->{b}", b, node.lineno)
        self.generic_visit(node)

    def visit_BinOp(self, node):
        op = type(node.op)
        if op in self.ARITH and not any(isinstance(x, (ast.JoinedStr, ast.List, ast.Tuple)) or (isinstance(x, ast.Constant) and isinstance(x.value, (str, bytes))) for x in (node.left, node.right)):
            a, b = self.ARITH[op]
            span = self.op_between(node.left, node.right, (a,))
            if span:
                self.add(span[0], span[1], "arithmetic_swap", f"{a}->{b}", b, node.lineno)
        self.generic_visit(node)

    def visit_AugAssign(self, node):
        self.stmt.append(node.lineno)
        op = type(node.op)
        if op in self.ARITH and not (isinstance(node.value, ast.Constant) and isinstance(node.value.value, (str, bytes))):
            a, b = self.ARITH[op]
            span = self.op_between(node.target, node.value, (a + "=",))
            if span:
                self.add(span[0], span[1], "arithmetic_swap", f"{a}=->{b}=", b + "=", node.lineno)
        self.node_add(node, "statement_removal", "remove_augassign", "pass")
        self.stmt.pop()
        self.generic_visit(node)

    def visit_Expr(self, node):
        self.stmt.append(node.lineno)
        if isinstance(node.value, ast.Call):
            self.node_add(node, "statement_removal", "remove_call", "pass")
        self.stmt.pop()
        self.generic_visit(node)

    def visit_Constant(self, node):
        v = node.value
        if isinstance(v, bool):
            self.node_add(node, "constant_change", "bool", "False" if v else "True")
        elif isinstance(v, int):
            self.node_add(node, "constant_change", "int", "0" if v == 1 else str(v + 1))
        elif isinstance(v, float):
            self.node_add(node, "constant_change", "float", repr(v * 2 + 1))

    def visit_JoinedStr(self, node):
        return

    def visit_Return(self, node):
        self.stmt.append(node.lineno)
        if node.value is not None:
            v = node.value
            if isinstance(v, ast.Constant) and isinstance(v.value, bool):
                self.node_add(v, "return_value", "return_bool", "False" if v.value else "True")
            elif not (isinstance(v, ast.Constant) and v.value is None):
                self.node_add(v, "return_value", "return_none", "None")
                self.stmt.pop()
                return
        self.stmt.pop()
        self.generic_visit(node)


def py_sites(tree, rel):
    src = (tree / rel).read_text()
    try:
        mod = ast.parse(src)
    except SyntaxError:
        return []
    mut = PyMutator(rel, src)
    for node in mod.body:
        if isinstance(node, (ast.Import, ast.ImportFrom)):
            continue
        if isinstance(node, ast.Expr) and isinstance(node.value, ast.Constant) and isinstance(node.value.value, str):
            continue
        mut.visit(node)
    out = []
    for s in mut.sites:
        mutated = mut.raw[: s["start"]] + s["repl"].encode() + mut.raw[s["end"]:]
        try:
            compile(mutated, rel, "exec")
        except (SyntaxError, ValueError):
            continue
        out.append(s)
    return out


SH_HEREDOC = re.compile(r"<<-?\s*(['\"]?)([A-Za-z_][A-Za-z0-9_]*)\1")
SH_CONTROL = re.compile(r"^\s*(then|do|done|fi|else|esac|;;|\}|\{|\)|case\b|function\b|[A-Za-z_][A-Za-z0-9_]*\s*\(\)\s*\{?)")


def sh_sites(tree, rel):
    raw = (tree / rel).read_bytes()
    try:
        text = raw.decode()
    except UnicodeDecodeError:
        return []
    lines = text.split("\n")
    starts = [0]
    for line in lines[:-1]:
        starts.append(starts[-1] + len(line.encode()) + 1)
    sites = []
    heredoc = None
    prev_cont = False
    func = ""
    for i, line in enumerate(lines):
        n = i + 1
        if heredoc:
            if line.strip() == heredoc:
                heredoc = None
            continue
        hd = SH_HEREDOC.search(line)
        stripped = line.strip()
        cont = line.rstrip().endswith("\\")
        fm = re.match(r"^\s*(?:function\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*\(\)", line)
        if fm:
            func = fm[1]
        if not stripped or stripped.startswith("#") or prev_cont:
            prev_cont = cont
            if hd:
                heredoc = hd[2]
            continue
        base = starts[i]

        def add(a, b, family, op, repl):
            sites.append({"file": rel, "line": n, "stmt_line": n, "start": base + len(line[:a].encode()), "end": base + len(line[:b].encode()), "family": family, "op": op, "orig": line[a:b], "repl": repl, "function": func})

        m = re.match(r"^(\s*(?:if|elif|while|until)\s+)(?!!)(.+?)(;\s*(?:then|do)\s*)$", line)
        if m and not hd:
            add(m.start(2), m.start(2), "condition_negation", "negate_condition", "! ")
        for m in re.finditer(r"\s(-eq|-ne)\s", line):
            add(m.start(1), m.end(1), "condition_negation", f"{m[1]}->", "-ne" if m[1] == "-eq" else "-eq")
        for m in re.finditer(r"\[\[? .*?(\s(==?|!=)\s)", line):
            op = m[2]
            add(m.start(2), m.end(2), "condition_negation", f"{op}->", "!=" if op in ("=", "==") else "=")
        for m in re.finditer(r"\s(-lt|-le|-gt|-ge)\s", line):
            r = {"-lt": "-le", "-le": "-lt", "-gt": "-ge", "-ge": "-gt"}[m[1]]
            add(m.start(1), m.end(1), "comparison_boundary", f"{m[1]}->{r}", r)
        for m in re.finditer(r"\s(-lt|-le|-gt|-ge|-eq|-ne)\s+(\d+)\b", line):
            v = int(m[2])
            add(m.start(2), m.end(2), "constant_change", "int", "0" if v == 1 else str(v + 1))
        for m in re.finditer(r"\$\(\(\s*[^)]*?\s([+\-*/])\s", line):
            r = {"+": "-", "-": "+", "*": "/", "/": "*"}[m[1]]
            add(m.start(1), m.end(1), "arithmetic_swap", f"{m[1]}->{r}", r)
        for m in re.finditer(r"\b(return|exit)\s+(\d+)\b", line):
            v = int(m[2])
            add(m.start(2), m.end(2), "return_value", f"{m[1]}_code", "1" if v == 0 else "0")
        if not hd and not cont and not SH_CONTROL.match(line) and not re.search(r"[{}()]\s*$|^\s*(if|elif|while|until|for|case|select|local|declare|readonly|export|return|exit|set|shopt|trap|source|\.)\b|\b(then|do|in)\s*$|=\(|<<|;;", line) and re.match(r"^\s*[A-Za-z_\"$][^=]*$|^\s*[A-Za-z_][A-Za-z0-9_]*=", line):
            ind = len(line) - len(line.lstrip())
            add(ind, len(line), "statement_removal", "remove_command", ":")
        if hd:
            heredoc = hd[2]
        prev_cont = cont
    out = []
    for s in sites:
        mutated = raw[: s["start"]] + s["repl"].encode() + raw[s["end"]:]
        p = subprocess.run([shutil.which("bash"), "-n"], input=mutated, capture_output=True)
        if p.returncode == 0:
            out.append(s)
    return out


def generate_sites(tree):
    files = git("ls-files", cwd=tree).splitlines()
    go_files = [f for f in files if f.startswith(MODULE_DIR + "/") and f.endswith(".go") and not is_test_file(f)]
    py_files = [f for f in files if f.endswith(".py") and not is_test_file(f) and not f.startswith(("scripts/defect_audit",))]
    sh_files = [f for f in files if f.endswith(".sh") and not is_test_file(f)]
    env = base_env()
    out = run(["go", "run", str(ROOT / "scripts/defect_audit_gomutate.go"), str(tree), *go_files], cwd=ROOT, env=env).stdout
    sites = []
    for s in json.loads(out) or []:
        s["lang"] = "go"
        s["stmt_line"] = s["line"]
        sites.append(s)
    for f in py_files:
        for s in py_sites(tree, f):
            s["lang"] = "py"
            sites.append(s)
    for f in sh_files:
        for s in sh_sites(tree, f):
            s["lang"] = "sh"
            sites.append(s)
    for s in sites:
        s["id"] = sha(f"{s['file']}:{s['start']}:{s['end']}:{s['op']}:{s['repl']}")
    seen = set()
    uniq = []
    for s in sites:
        if s["id"] not in seen:
            seen.add(s["id"])
            uniq.append(s)
    return uniq


def cmd_plan(args):
    out = pathlib.Path(args.out).resolve()
    if git("status", "--porcelain").strip():
        raise AuditError("the worktree is not clean; commit or remove changes before planning the audit")
    base = git("rev-parse", "--verify", args.base + "^{commit}").strip()
    out.mkdir(parents=True, exist_ok=True)
    tree = out / "workers" / "w0" / "tree"
    setup_tree(out, 0, base)
    modes = ci_go_modes()
    specs = {}
    for goos in ("linux", "darwin"):
        for tags in sorted({m["tags"] for m in modes}):
            specs[(goos, tags)] = go_list(tree, tags, goos)
    decls = go_declarations(tree)
    by_name = collections.defaultdict(list)
    for d in decls:
        by_name[d["name"]].append(d)
    mode_rows = []
    for m in modes:
        selected = []
        for pkg in specs[("linux", m["tags"])]:
            if "./..." not in m["packages"] and pkg["ImportPath"] != MODULE:
                continue
            files = (pkg.get("TestGoFiles") or []) + (pkg.get("XTestGoFiles") or [])
            linux_tests = go_tests_in_files(pkg["Dir"], files)
            darwin_pkg = next((p for p in specs[("darwin", m["tags"])] if p["ImportPath"] == pkg["ImportPath"]), {})
            darwin_tests = go_tests_in_files(pkg["Dir"], (darwin_pkg.get("TestGoFiles") or []) + (darwin_pkg.get("XTestGoFiles") or []))
            for name, f in sorted(linux_tests.items()):
                if m["run"] and not re.search(m["run"], name):
                    continue
                if m["skip"] and re.search(m["skip"], name):
                    continue
                selected.append({"package": pkg["ImportPath"], "name": name, "file": str(pathlib.Path(pkg["Dir"]).relative_to(tree) / f), "platform": "darwin" if name in darwin_tests else "linux"})
        mode_rows.append({**m, "tests": selected})
    selected_names = {(t["package"], t["name"]) for m in mode_rows for t in m["tests"]}
    exclusions = {}
    for d in decls:
        body = go_test_body((tree / d["file"]).read_text(), d["name"])
        if subprocess_entry_point(body):
            exclusions[d["name"]] = "subprocess entry point: returns at once unless its helper environment variable is set; its execution is attributed to the calling test"
    unselected = [d for d in decls if d["name"] not in {n for _, n in selected_names}]
    python = {}
    for suite, pargs in PY_COLLECTIONS:
        python[suite] = py_collect(tree, suite, pargs, out / "collect" / f"{suite}.json")
    sites = generate_sites(tree)
    write_json(out / "sites.json", sites)
    inventory = {
        "base": base,
        "go_declarations": decls,
        "go_modes": mode_rows,
        "go_unselected_declarations": unselected,
        "go_exclusions": exclusions,
        "python": python,
        "standalone_shell_suites": sorted(f for f in git("ls-files", cwd=tree).splitlines() if re.match(r"scripts/test_.*\.sh$", f)),
    }
    write_json(out / "inventory.json", inventory)
    meta = {"base": base, "tools": tool_versions(tree), "families": FAMILIES, "planned_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())}
    write_json(out / "meta.json", meta)
    total_go = sum(len(m["tests"]) for m in mode_rows)
    print(f"base {base}")
    print(f"go declarations {len(decls)} in {len({d['file'] for d in decls})} files; go mode records {total_go}; unselected {len(unselected)}; excluded {len(exclusions)}")
    for m in mode_rows:
        print(f"  {m['name']}: {len(m['tests'])} tests ({sum(t['platform'] == 'linux' for t in m['tests'])} linux-only)")
    for suite, ids in python.items():
        print(f"python {suite}: {len(ids)} node ids")
    print(f"mutation sites {len(sites)}: " + ", ".join(f"{k} {v}" for k, v in sorted(collections.Counter(s['lang'] for s in sites).items())))


def records_from_inventory(inv):
    recs = []
    for m in inv["go_modes"]:
        for t in m["tests"]:
            rid = f"{m['name']}::{t['name']}" if t["package"] == MODULE else f"{m['name']}::{t['package']}.{t['name']}"
            recs.append({"id": rid, "lang": "go", "mode": m["name"], "test": t["name"], "package": t["package"], "file": t["file"], "platform": t["platform"], "tags": m["tags"], "race": m["race"], "exclusion": inv["go_exclusions"].get(t["name"])})
    for suite, ids in inv["python"].items():
        for nid in ids:
            recs.append({"id": f"py/{suite}::{nid}", "lang": "py", "mode": f"py/{suite}", "test": nid, "file": nid.split("::")[0], "platform": "darwin", "exclusion": None})
    return recs


def setup_tree(out, idx, base):
    wdir = out / "workers" / f"w{idx}"
    tree = wdir / "tree"
    infra = wdir / "infra"
    marker = wdir / "ready"
    if marker.exists() and marker.read_text().strip() == base:
        return tree, infra
    if wdir.exists():
        shutil.rmtree(wdir)
    infra.mkdir(parents=True)
    run(["git", "clone", "--quiet", "--no-checkout", str(ROOT), str(tree)])
    git("checkout", "--quiet", "--detach", base, cwd=tree)
    git("config", "user.name", "defect-audit", cwd=tree)
    git("config", "user.email", "defect-audit@invalid", cwd=tree)
    git("config", "commit.gpgsign", "false", cwd=tree)
    env = base_env()
    env.pop("UV_OFFLINE", None)
    run(["uv", "sync", "--frozen", "--quiet"], cwd=tree, env=env)
    pylib = out / "tools" / "pylib"
    if not (pylib / "coverage").exists():
        run(["uv", "pip", "install", "--quiet", "--python", str(tree / ".venv/bin/python3"), "--target", str(pylib), f"coverage=={COVERAGE_VERSION}"], env=env)
    site = next((tree / ".venv/lib").glob("python3*/site-packages"))
    current = infra / "current"
    current.write_text("")
    (site / "_defect_audit_hook.py").write_text(
        "import os, sys\n"
        f"_d = ''\n"
        "try:\n"
        f"    _d = open({str(current)!r}).read().strip()\n"
        "except OSError:\n"
        "    pass\n"
        "if _d:\n"
        f"    sys.path.append({str(pylib)!r})\n"
        "    import atexit, coverage\n"
        f"    _c = coverage.Coverage(data_file=os.path.join(_d, 'py', 'cov'), data_suffix=True, config_file=False, omit=[{str(tree / '.venv')!r} + '/*', '*/site-packages/*'])\n"
        "    _c.start()\n"
        "    def _stop():\n"
        "        try:\n"
        "            _c.stop()\n"
        "            _c.save()\n"
        "        except Exception:\n"
        "            pass\n"
        "    atexit.register(_stop)\n"
    )
    (site / "zz_defect_audit.pth").write_text("import _defect_audit_hook\n")
    install_switch_hooks(tree, infra)
    shims = infra / "shims"
    shims.mkdir()
    real_go = shutil.which("go")
    real_bash = shutil.which("bash")
    (shims / "go").write_text(
        "#!/bin/sh\n"
        f"d=$(cat {shlex.quote(str(current))} 2>/dev/null)\n"
        "if [ -n \"$d\" ] && [ \"$1\" = build ]; then\n"
        "  shift\n"
        f"  if {shlex.quote(real_go)} build -cover -covermode=atomic -coverpkg={MODULE}/... \"$@\"; then exit 0; fi\n"
        "  echo \"go build with coverage failed: $*\" >> \"$d/gaps.log\"\n"
        f"  exec {shlex.quote(real_go)} build \"$@\"\n"
        "fi\n"
        f"exec {shlex.quote(real_go)} \"$@\"\n"
    )
    bash_env = infra / "bash_env.sh"
    bash_env.write_text(
        "if [ -n \"${BASH_VERSINFO:-}\" ] && [ \"${BASH_VERSINFO[0]}\" -ge 5 ]; then\n"
        "  __defect_audit_dir=\n"
        f"  read -r __defect_audit_dir < {shlex.quote(str(current))} 2>/dev/null || true\n"
        "  if [ -n \"$__defect_audit_dir\" ] && [ -d \"$__defect_audit_dir/sh\" ]; then\n"
        "    exec 237>>\"$__defect_audit_dir/sh/trace.$$.$RANDOM\"\n"
        "    printf '@ARGV0@%s@%s\\n' \"$0\" \"$PWD\" >&237\n"
        "    BASH_XTRACEFD=237\n"
        "    PS4='+@A@${BASH_SOURCE[0]-}@${LINENO}@ '\n"
        "    set -x\n"
        "  fi\n"
        "  unset __defect_audit_dir\n"
        "fi\n"
    )
    (shims / "bash").write_text(f"#!/bin/sh\nBASH_ENV={shlex.quote(str(bash_env))}\nexport BASH_ENV\nexec {shlex.quote(real_bash)} \"$@\"\n")
    for f in shims.iterdir():
        f.chmod(0o755)
    (infra / "bin").mkdir()
    marker.write_text(base)
    return tree, infra


PY_SWITCH_HOOK = """
_defect_audit_switch = {switch!r}
try:
    with open(_defect_audit_switch) as _fh:
        _parts = _fh.read().split("\\n")
except OSError:
    _parts = []
_defect_audit_state = {{"active": _parts[0].strip() if _parts else "", "dir": _parts[1].strip() if len(_parts) > 1 else "", "hit": False}}


def _defect_audit_on(i, _st=_defect_audit_state):
    if i != _st["active"]:
        return False
    if not _st["hit"]:
        _st["hit"] = True
        if _st["dir"]:
            try:
                open(os.path.join(_st["dir"], "py.%d" % os.getpid()), "a").close()
            except OSError:
                pass
    return True


sys._defect_audit_on = _defect_audit_on
sys._defect_audit_state = _defect_audit_state
"""

PY_PLUGIN = """import json
import os
import signal
import sys

import pytest

_RESULTS = os.environ.get("DEFECT_AUDIT_RESULTS", "")
try:
    with open(os.environ.get("DEFECT_AUDIT_TIMEOUTS", "")) as _fh:
        _TIMEOUTS = json.load(_fh)
except (OSError, ValueError):
    _TIMEOUTS = {}
_IMPORT_REACHED = [False]


class DefectAuditTimeout(BaseException):
    pass


def _markers():
    st = getattr(sys, "_defect_audit_state", None)
    if not st or not st.get("dir"):
        return []
    try:
        return os.listdir(st["dir"])
    except OSError:
        return []


def _clear():
    st = getattr(sys, "_defect_audit_state", None)
    if not st:
        return
    st["hit"] = False
    for name in _markers():
        try:
            os.unlink(os.path.join(st["dir"], name))
        except OSError:
            pass


def pytest_collection_finish(session):
    _IMPORT_REACHED[0] = bool(_markers())


@pytest.hookimpl(wrapper=True)
def pytest_runtest_protocol(item, nextitem):
    _clear()
    timed = [False]
    limit = int(_TIMEOUTS.get(item.nodeid, 0) or 0)
    old = None
    if limit:
        def _alarm(signum, frame):
            timed[0] = True
            raise DefectAuditTimeout(f"test exceeded {limit}s")
        old = signal.signal(signal.SIGALRM, _alarm)
        signal.alarm(limit)
    try:
        return (yield)
    finally:
        if limit:
            signal.alarm(0)
            signal.signal(signal.SIGALRM, old or signal.SIG_DFL)
        if _RESULTS:
            with open(_RESULTS, "a") as fh:
                fh.write(json.dumps({"nodeid": item.nodeid, "reached": bool(_markers()), "import_reached": _IMPORT_REACHED[0], "timed_out": timed[0]}) + "\\n")
"""

GO_SWITCH = """package {pkg}

import (
\t"os"
\t"strconv"
\t"strings"
\t"sync"
)

var defectAuditState struct {
\tonce   sync.Once
\tmark   sync.Once
\tactive string
\tdir    string
}

func defectAuditOn(id string) bool {
\ts := &defectAuditState
\ts.once.Do(func() {
\t\tb, _ := os.ReadFile({path})
\t\tparts := strings.Split(string(b), "\\n")
\t\ts.active = strings.TrimSpace(parts[0])
\t\tif len(parts) > 1 {
\t\t\ts.dir = strings.TrimSpace(parts[1])
\t\t}
\t})
\tif id != s.active {
\t\treturn false
\t}
\ts.mark.Do(func() {
\t\tif s.dir != "" {
\t\t\tif f, err := os.Create(s.dir + "/go." + strconv.Itoa(os.Getpid())); err == nil {
\t\t\t\tf.Close()
\t\t\t}
\t\t}
\t})
\treturn true
}
"""

GO_SWITCH_FILE = "zz_defect_audit_switch.go"


def py_on(sid):
    return f'getattr(__import__("sys"), "_defect_audit_on", callable)({sid!r})'


def install_switch_hooks(tree, infra):
    site = next((tree / ".venv/lib").glob("python3*/site-packages"))
    hook = site / "_defect_audit_hook.py"
    text = hook.read_text()
    marker = "\n_defect_audit_switch = "
    if marker in text:
        text = text[: text.index(marker)]
    hook.write_text(text + PY_SWITCH_HOOK.format(switch=str(infra / "mutant")))
    (site / "_defect_audit_pytest.py").write_text(PY_PLUGIN)
    sw = infra / "mutant"
    if not sw.exists():
        sw.write_text("")


def build_key(rec):
    return f"{'linux' if rec['platform'] == 'linux' else 'darwin'}-{rec.get('tags') or 'none'}-{'race' if rec.get('race') else 'norace'}"


def compile_go(tree, infra, key, label, cover, sem):
    goos, tags, race = key.split("-")
    bin_real = infra / "bin" / f"{key}.{label}.{'cover' if cover else 'plain'}.real"
    wrapper = infra / "bin" / f"{key}.{label}.{'cover' if cover else 'plain'}.test"
    env = base_env()
    if goos == "linux":
        env["GOOS"] = "linux"
        env["GOARCH"] = "arm64"
        env["CGO_ENABLED"] = "1" if race == "race" else "0"
    args = ["go", "test", "-c", "-o", str(bin_real)]
    if tags != "none":
        args += ["-tags", tags]
    if race == "race":
        args += ["-race"]
    if cover:
        args += ["-cover", "-covermode=atomic", f"-coverpkg={MODULE}/..."]
    args += ["."]
    with sem:
        p = run(args, cwd=tree / MODULE_DIR, env=env, check=False, timeout=1800)
    if p.returncode != 0:
        return None, (p.stdout + p.stderr)[-3000:]
    current = infra / "current"
    extra = ' -test.gocoverdir="$d/go"' if cover else ""
    export = 'if [ -n "$d" ]; then export GOCOVERDIR="$d/go"; fi\n' if cover else ""
    wrapper.write_text(f"#!/bin/bash\nd=$(cat {shlex.quote(str(current))} 2>/dev/null)\n{export}exec -a \"$0\" {shlex.quote(str(bin_real))} \"$@\"{extra}\n")
    wrapper.chmod(0o755)
    return wrapper, None


def kill_tree(proc, name=None):
    try:
        os.killpg(proc.pid, signal.SIGKILL)
    except (ProcessLookupError, PermissionError):
        pass
    if name:
        subprocess.run(["docker", "rm", "-f", name], capture_output=True)


def run_test_process(cmd, cwd, env, timeout, docker_name=None):
    start = time.time()
    proc = subprocess.Popen(cmd, cwd=cwd, env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, start_new_session=True, text=True, errors="replace")
    try:
        out, _ = proc.communicate(timeout=timeout)
        timed_out = False
    except subprocess.TimeoutExpired:
        kill_tree(proc, docker_name)
        out, _ = proc.communicate()
        timed_out = True
    kill_tree(proc)
    return proc.returncode, out or "", time.time() - start, timed_out


def parse_go_output(name, code, out, timed_out):
    subs = {}
    top = None
    for line in out.splitlines():
        m = OUTCOME_LINE.match(line)
        if m:
            if m[2] == name:
                top = m[1]
            elif m[2].startswith(name + "/"):
                subs[m[2]] = m[1]
    if timed_out or "panic: test timed out" in out:
        result = "timeout"
    elif "testing: warning: no tests to run" in out:
        result = "not_selected"
    elif top == "SKIP":
        result = "skip"
    elif top == "PASS" and code == 0:
        result = "pass"
    elif top == "FAIL" or code != 0:
        result = "fail"
    else:
        result = "error"
    skipped = sorted(k for k, v in subs.items() if v == "SKIP")
    return {"result": result, "subtests": collections.Counter(subs.values()), "skipped_subtests": skipped}


def parse_py_output(code, out, log):
    outcomes = collections.Counter()
    subtests = collections.Counter()
    phases = []
    for row in read_jsonl(log) if log.exists() else []:
        rt = row.get("$report_type")
        if rt == "TestReport":
            phases.append((row.get("when"), row.get("outcome")))
            if row.get("when") == "call":
                outcomes[row.get("outcome")] += 1
        elif rt == "SubTestReport":
            subtests[row.get("outcome")] += 1
    setup_failed = any(w in ("setup", "teardown") and o == "failed" for w, o in phases)
    call_failed = any(w == "call" and o == "failed" for w, o in phases)
    skipped = any(o == "skipped" for _, o in phases)
    if code == 5 or not phases:
        result = "not_selected" if code == 5 else "error"
    elif call_failed or subtests.get("failed"):
        result = "fail"
    elif setup_failed:
        result = "setup_error"
    elif skipped:
        result = "skip"
    elif code == 0:
        result = "pass"
    else:
        result = "error"
    return {"result": result, "subtests": subtests, "skipped_subtests": ["skipped"] * subtests.get("skipped", 0)}


def collect_coverage(rundir, tree):
    cov = {"go": collections.defaultdict(list), "py": collections.defaultdict(set), "py_copies": collections.defaultdict(set), "sh": collections.defaultdict(set), "sh_copies": collections.defaultdict(set), "processes": {"go": 0, "py": 0, "sh": 0}, "gaps": []}
    gdir = rundir / "go"
    if gdir.exists() and any(gdir.iterdir()):
        cov["processes"]["go"] = sum(1 for f in gdir.iterdir() if f.name.startswith("covcounters"))
        prof = rundir / "go.prof"
        p = run(["go", "tool", "covdata", "textfmt", f"-i={gdir}", f"-o={prof}"], env=base_env(), check=False)
        if p.returncode != 0:
            cov["gaps"].append("go coverage data could not be read: " + p.stderr[-300:])
        else:
            for line in prof.read_text().splitlines()[1:]:
                m = re.match(r"^(.+?):(\d+)\.(\d+),(\d+)\.(\d+) \d+ (\d+)$", line)
                if not m or int(m[6]) == 0:
                    continue
                path = m[1]
                if path == MODULE or not path.startswith(MODULE + "/"):
                    continue
                rel = MODULE_DIR + path[len(MODULE):]
                cov["go"][rel].append((int(m[2]), int(m[3]), int(m[4]), int(m[5])))
    pdir = rundir / "py"
    if pdir.exists():
        files = [f for f in pdir.iterdir() if f.name.startswith("cov")]
        cov["processes"]["py"] = len(files)
        if files:
            from coverage import CoverageData
            tracked = TRACKED.get(str(tree))
            for f in files:
                data = CoverageData(basename=str(f))
                try:
                    data.read()
                except Exception as exc:
                    cov["gaps"].append(f"python coverage data could not be read: {exc}")
                    continue
                for mf in data.measured_files():
                    lines = data.lines(mf) or []
                    rel, copy = map_path(mf, tree, tracked)
                    if rel is None:
                        continue
                    (cov["py_copies"] if copy else cov["py"])[rel].update(lines)
    sdir = rundir / "sh"
    if sdir.exists():
        tracked = TRACKED.get(str(tree))
        traces = list(sdir.iterdir())
        cov["processes"]["sh"] = len(traces)
        for f in traces:
            pwd0 = None
            for line in f.read_text(errors="replace").splitlines():
                if line.startswith("@ARGV0@"):
                    parts = line.split("@")
                    pwd0 = parts[3] if len(parts) > 3 else ""
                    continue
                m = TRACE_LINE.match(line)
                if not m or not m[1]:
                    continue
                src = m[1]
                if not src.startswith("/"):
                    src = os.path.normpath(os.path.join(pwd0 or "", src))
                rel, copy = map_path(src, tree, tracked)
                if rel is None or not rel.endswith(".sh"):
                    continue
                (cov["sh_copies"] if copy else cov["sh"])[rel].add(int(m[2]))
    gl = rundir / "gaps.log"
    if gl.exists():
        cov["gaps"] += sorted(set(gl.read_text().splitlines()))
    return cov


TRACKED = {}


def tracked_index(tree):
    key = str(tree)
    if key not in TRACKED:
        files = git("ls-files", cwd=tree).splitlines()
        by_suffix = collections.defaultdict(list)
        for f in files:
            parts = f.split("/")
            for i in range(len(parts)):
                by_suffix["/".join(parts[i:])].append(f)
        TRACKED[key] = by_suffix
    return TRACKED[key]


def map_path(path, tree, tracked):
    path = os.path.realpath(path) if os.path.exists(path) else path
    t = os.path.realpath(str(tree))
    if path.startswith(t + "/"):
        rel = path[len(t) + 1:]
        if rel.startswith(".venv/"):
            return None, False
        return rel, False
    parts = path.split("/")
    for i in range(1, len(parts)):
        suffix = "/".join(parts[i:])
        hits = tracked.get(suffix) if tracked else None
        if hits and len(hits) == 1 and "/" in suffix:
            return hits[0], True
    return None, False


def coverage_summary(cov):
    def files(d):
        return {k: sorted(v) if isinstance(v, set) else len(v) for k, v in sorted(d.items())}
    go_lines = {}
    for f, blocks in cov["go"].items():
        lines = set()
        for sl, _, el, _ in blocks:
            lines.update(range(sl, el + 1))
        go_lines[f] = len(lines)
    product = lambda d: {k: v for k, v in d.items() if not is_test_file(k)}
    return {
        "go_files": product(go_lines),
        "py_files": {k: len(v) for k, v in product(cov["py"]).items()},
        "sh_files": {k: len(v) for k, v in product(cov["sh"]).items()},
        "copied_py_files": {k: len(v) for k, v in product(cov["py_copies"]).items()},
        "copied_sh_files": {k: len(v) for k, v in product(cov["sh_copies"]).items()},
        "harness_files": sorted({k for d in (go_lines, cov["py"], cov["sh"]) for k in d if is_test_file(k)}),
        "processes": cov["processes"],
    }


def site_executed(site, cov):
    if site["lang"] == "go":
        for sl, sc, el, ec in cov["go"].get(site["file"], []):
            if (sl, sc) <= (site["line"], site["col"]) <= (el, ec):
                return True
        return False
    if site["lang"] == "py":
        return site["stmt_line"] in cov["py"].get(site["file"], set())
    return site["line"] in cov["sh"].get(site["file"], set())


def site_copy_executed(site, cov):
    if site["lang"] == "py":
        return site["stmt_line"] in cov["py_copies"].get(site["file"], set())
    if site["lang"] == "sh":
        return site["line"] in cov["sh_copies"].get(site["file"], set())
    return False


class Worker:
    def __init__(self, out, idx, base, sem, heavy=None):
        self.out = out
        self.idx = idx
        self.base = base
        self.sem = sem
        self.tree, self.infra = setup_tree(out, idx, base)
        install_switch_hooks(self.tree, self.infra)
        tracked_index(self.tree)
        self.bins = {}
        self.heavy = heavy
        self.head = base
        self.round = None
        self.reach = out / "runs" / f"w{idx}" / "reach"
        self.reach.mkdir(parents=True, exist_ok=True)
        sys.path.append(str(out / "tools" / "pylib"))

    def ensure_round(self, rnd):
        if self.round == rnd:
            return
        spec = read_json(self.out / "results" / f"round{rnd}-schemata.json")
        self.git_retry("reset", "--quiet", "--hard", self.base)
        self.git_retry("clean", "-ffdxq", "-e", "/.venv")
        apply_schemata(self.tree, spec, self.infra / "mutant")
        self.git_retry("add", "-A")
        self.git_retry("commit", "--quiet", "--no-verify", "-m", f"defect-audit schemata round {rnd}")
        self.head = self.git_retry("rev-parse", "HEAD").strip()
        for wrapper, _ in self.bins.values():
            if wrapper:
                for p in (wrapper, pathlib.Path(str(wrapper)[: -len(".test")] + ".real")):
                    try:
                        p.unlink()
                    except OSError:
                        pass
        self.bins = {}
        self.round = rnd

    def set_switch(self, sid):
        (self.infra / "mutant").write_text(f"{sid}\n{self.reach}\n" if sid else "")

    def clear_reach(self):
        for f in self.reach.iterdir():
            try:
                f.unlink()
            except OSError:
                pass

    def reached(self):
        return any(self.reach.iterdir())

    def py_batch(self, recs, timeouts, tag):
        rundir = self.out / "runs" / f"w{self.idx}" / f"batch-{tag}"
        if rundir.exists():
            shutil.rmtree(rundir)
        rundir.mkdir(parents=True)
        log = rundir / "report.jsonl"
        reach = rundir / "reach.jsonl"
        tfile = rundir / "timeouts.json"
        tfile.write_text(json.dumps({r["test"]: timeouts[r["id"]] for r in recs}))
        env = self.env(None)
        env["DEFECT_AUDIT_RESULTS"] = str(reach)
        env["DEFECT_AUDIT_TIMEOUTS"] = str(tfile)
        self.clear_reach()
        cmd = [str(self.tree / ".venv/bin/python3"), "-m", "pytest", "-p", "no:cacheprovider", "-p", "_defect_audit_pytest", "-q", "--no-header", "-rN", f"--report-log={log}", *[r["test"] for r in recs]]
        code, out, el, to = run_test_process(cmd, self.tree, env, sum(timeouts[r["id"]] for r in recs) + 120)
        rows = collections.defaultdict(list)
        collect_failed = []
        for row in read_jsonl(log) if log.exists() else []:
            if row.get("$report_type") == "CollectReport" and row.get("outcome") == "failed":
                collect_failed.append(str(row.get("longrepr") or "")[-1200:])
            elif row.get("nodeid"):
                rows[row["nodeid"]].append(row)
        reach_rows = {r["nodeid"]: r for r in read_jsonl(reach)} if reach.exists() else {}
        results = {}
        for r in recs:
            node = r["test"]
            rr = reach_rows.get(node)
            if rr is None:
                if collect_failed:
                    results[r["id"]] = {"result": "collection_error", "elapsed": round(el, 3), "tail": " | ".join(collect_failed)[-2500:], "reached": self.reached()}
                elif not to and code not in (0, 5) and not reach_rows:
                    results[r["id"]] = {"result": "session_error", "elapsed": round(el, 3), "tail": f"pytest exited {code} before any test ran: " + out[-2400:], "reached": self.reached()}
                else:
                    results[r["id"]] = {"result": "timeout" if to else "missing", "elapsed": round(el, 3), "tail": out[-2500:], "reached": False}
                continue
            phases = [(x.get("when"), x.get("outcome")) for x in rows[node] if x.get("$report_type") == "TestReport"]
            subs = collections.Counter(x.get("outcome") for x in rows[node] if x.get("$report_type") == "SubTestReport")
            longrepr = " ".join(str(x.get("longrepr") or "")[-800:] for x in rows[node] if x.get("outcome") == "failed")
            if rr.get("timed_out"):
                res = "timeout"
            elif any(w == "call" and o == "failed" for w, o in phases) or subs.get("failed"):
                res = "fail"
            elif any(w in ("setup", "teardown") and o == "failed" for w, o in phases):
                res = "setup_error"
            elif any(o == "skipped" for _, o in phases):
                res = "skip"
            elif phases and all(o == "passed" for _, o in phases):
                res = "pass"
            else:
                res = "error"
            results[r["id"]] = {"result": res, "elapsed": round(el, 3), "reached": bool(rr.get("reached") or rr.get("import_reached")), "tail": longrepr[-2500:] if res != "pass" else ""}
        return results

    def go_batch(self, recs, timeouts, label):
        wrapper, err = self.binary(build_key(recs[0]), label, False)
        if wrapper is None:
            return {r["id"]: {"result": "build_failure", "detail": err, "elapsed": 0, "reached": False} for r in recs}
        limit = {r["test"]: timeouts[r["id"]] for r in recs}
        names = "|".join(re.escape(r["test"]) for r in recs)
        self.clear_reach()
        (self.infra / "current").write_text("")
        cmd = [str(wrapper), "-test.run", f"^({names})$", "-test.count=1", "-test.v", "-test.timeout", f"{int(sum(limit.values()) + 60)}s"]
        start = time.time()
        proc = subprocess.Popen(cmd, cwd=self.tree / MODULE_DIR, env=self.env(None), stdout=subprocess.PIPE, stderr=subprocess.STDOUT, start_new_session=True, text=True, errors="replace")
        lines = queue.Queue()

        def pump():
            for line in proc.stdout:
                lines.put(line)
            lines.put(None)

        threading.Thread(target=pump, daemon=True).start()
        pending = set(limit)
        deadline = time.time() + max(limit.values()) + 60
        current = None
        hung = None
        buf = []
        tops = {}
        subs = collections.defaultdict(list)
        while True:
            try:
                line = lines.get(timeout=1)
            except queue.Empty:
                if time.time() > deadline:
                    hung = current
                    kill_tree(proc)
                    break
                continue
            if line is None:
                break
            buf.append(line)
            m = re.match(r"^=== RUN   (Test[^/\s]+)\s*$", line)
            if m:
                current = m[1]
            m = OUTCOME_LINE.match(line)
            if m:
                name = m[2].split("/")[0]
                if "/" in m[2]:
                    subs[name].append(m[1])
                elif not line.startswith(" "):
                    tops[name] = m[1]
                    pending.discard(name)
                    if pending:
                        deadline = time.time() + max(limit[n] for n in pending) + 30
        kill_tree(proc)
        proc.wait()
        el = time.time() - start
        out = "".join(buf)
        reached = self.reached()
        results = {}
        for r in recs:
            top = tops.get(r["test"])
            if top == "PASS":
                res = "skip" if "SKIP" in subs[r["test"]] else "pass"
            elif top == "FAIL":
                res = "fail"
            elif top == "SKIP":
                res = "skip"
            elif r["test"] == hung or "panic: test timed out" in out:
                res = "timeout"
            elif r["test"] == current and hung is None and proc.returncode not in (0, None):
                res = "fail"
            else:
                res = "missing"
            results[r["id"]] = {"result": res, "elapsed": round(el, 3), "exit": proc.returncode, "reached": reached, "batch": len(recs), "tail": "" if res == "pass" else out[-2500:]}
        return results

    def git_retry(self, *args):
        for attempt in range(5):
            try:
                return git(*args, cwd=self.tree)
            except AuditError:
                if attempt == 4:
                    raise
                time.sleep(2)
                lock = self.tree / ".git" / "index.lock"
                if attempt >= 2 and lock.exists():
                    lock.unlink()

    def reset(self):
        self.git_retry("reset", "--quiet", "--hard", self.head)
        self.git_retry("clean", "-ffdxq", "-e", "/.venv")

    def clean(self):
        self.git_retry("reset", "--quiet", "--hard", "HEAD")
        self.git_retry("clean", "-ffdxq", "-e", "/.venv")

    def binary(self, key, label, cover):
        k = (key, label, cover)
        if k not in self.bins:
            self.bins[k] = compile_go(self.tree, self.infra, key, label, cover, self.sem)
        return self.bins[k]

    def env(self, rundir):
        env = base_env()
        if rundir:
            env["PATH"] = f"{self.infra / 'shims'}:{env.get('PATH', '')}"
            env["BASH_ENV"] = str(self.infra / "bash_env.sh")
            env["GOCOVERDIR"] = str(rundir / "go")
        return env

    def execute(self, rec, rundir, timeout, label="base"):
        cover = rundir is not None
        current = self.infra / "current"
        if rundir:
            if rundir.exists():
                shutil.rmtree(rundir)
            for sub in ("go", "py", "sh"):
                (rundir / sub).mkdir(parents=True)
        try:
            if rec["lang"] == "go":
                wrapper, err = self.binary(build_key(rec), label, cover)
                if wrapper is None:
                    return {"result": "build_failure", "detail": err, "elapsed": 0}
                args = ["-test.run", f"^{re.escape(rec['test'])}$", "-test.count=1", "-test.v", "-test.timeout", f"{int(timeout)}s"]
                current.write_text(f"{rundir}\n" if rundir else "")
                if rec["platform"] == "linux":
                    name = f"defect-audit-{self.idx}-{os.getpid()}-{int(time.time() * 1000)}"
                    cmd = ["docker", "run", "--rm", "--name", name, "--network", "none", "-v", f"{self.out}:{self.out}", "-w", str(self.tree / MODULE_DIR), LINUX_IMAGE, str(wrapper), *args]
                    code, out, el, to = run_test_process(cmd, self.tree / MODULE_DIR, self.env(None), timeout + 120, docker_name=name)
                else:
                    heavy = rec.get("tags") == "pyintegration" or rec.get("race")
                    if heavy and self.heavy is not None:
                        self.heavy.acquire()
                    try:
                        code, out, el, to = run_test_process([str(wrapper), *args], self.tree / MODULE_DIR, self.env(rundir), timeout + 60)
                    finally:
                        if heavy and self.heavy is not None:
                            self.heavy.release()
                res = parse_go_output(rec["test"], code, out, to)
            else:
                log = (rundir or self.out / "tmp" / f"w{self.idx}") / "report.jsonl"
                log.parent.mkdir(parents=True, exist_ok=True)
                if log.exists():
                    log.unlink()
                current.write_text(f"{rundir}\n" if rundir else "")
                cmd = [str(self.tree / ".venv/bin/python3"), "-m", "pytest", "-p", "no:cacheprovider", "-q", "--no-header", "-rN", f"--report-log={log}", rec["test"]]
                code, out, el, to = run_test_process(cmd, self.tree, self.env(rundir), timeout)
                res = parse_py_output(code, out, log) if not to else {"result": "timeout", "subtests": {}, "skipped_subtests": []}
            res["elapsed"] = round(el, 3)
            res["exit"] = code
            if res["result"] not in ("pass",):
                res["tail"] = out[-2500:]
            return res
        finally:
            current.write_text("")


WORKER = None


def init_worker(out, base, queue, sem, heavy):
    global WORKER
    signal.signal(signal.SIGINT, signal.SIG_IGN)
    idx = queue.get()
    out = pathlib.Path(out)
    os.environ["GOCACHE"] = str(out / "gocache")
    WORKER = Worker(out, idx, base, sem, heavy)


def task_baseline(rec):
    w = WORKER
    w.reset()
    rundir = w.out / "runs" / f"w{w.idx}" / "baseline"
    plain = w.execute(rec, None, 1800)
    w.reset()
    first = w.execute(rec, rundir, 1800)
    cov = collect_coverage(rundir, w.tree) if first["result"] == "pass" else None
    w.reset()
    second = w.execute(rec, rundir.with_name("baseline2"), 1800)
    w.reset()
    covered = []
    summary = None
    gaps = []
    if cov is not None:
        summary = coverage_summary(cov)
        gaps = cov["gaps"]
        covered = sorted(SITE_INDEX.covered(cov))
    return {"record": rec["id"], "plain": plain, "instrumented": [first, second], "coverage": summary, "coverage_gaps": gaps, "covered_sites": covered}


def job_timeout(b):
    el = max([b["plain"].get("elapsed", 0)] + [x.get("elapsed", 0) for x in b["instrumented"]])
    return max(90, int(el * 1.5 + 45))


def brief(res):
    return {k: res.get(k) for k in ("result", "elapsed", "exit", "reached")}


def task_job(job):
    w = WORKER
    w.ensure_round(job["round"])
    site = SITE_INDEX.by_id[job["site"]]
    disk = job["kind"] == "disk"
    label = f"r{job['round']}"
    rows = {}

    def run_set(recs, tag):
        res = {}
        batches = collections.defaultdict(list)
        for rec in recs:
            if rec["lang"] != "go":
                continue
            if is_heavy(rec) and rec["platform"] != "linux":
                batches[build_key(rec)].append(rec)
                continue
            w.clean()
            w.clear_reach()
            r = w.execute(rec, None, job["timeouts"][rec["id"]], label=label)
            r["reached"] = w.reached()
            res[rec["id"]] = r
        for key, rs in sorted(batches.items()):
            todo = rs
            for _ in range(3):
                w.clean()
                res.update(w.go_batch(todo, job["timeouts"], label))
                todo = [rec for rec in todo if res[rec["id"]]["result"] == "missing"]
                if not todo:
                    break
        suites = collections.defaultdict(list)
        for rec in recs:
            if rec["lang"] == "py":
                suites[rec["mode"]].append(rec)
        for suite, rs in sorted(suites.items()):
            w.clean()
            res.update(w.py_batch(rs, job["timeouts"], f"{tag}-{suite.split('/')[-1]}"))
            for rec in rs:
                if res[rec["id"]]["result"] in ("missing", "session_error") and len(rs) > 1:
                    w.clean()
                    res.update(w.py_batch([rec], job["timeouts"], f"{tag}-alone"))
        return res

    w.set_switch(None)
    w.reset()
    try:
        if disk:
            path = w.tree / site["file"]
            raw = path.read_bytes()
            if raw[site["start"]: site["end"]].decode(errors="replace") != site["orig"]:
                return [{"site": site["id"], "record": r["id"], "kind": job["kind"], "outcome": "invalid", "reason": "mutation site text does not match the baseline source"} for r in job["records"]]
            path.write_bytes(raw[: site["start"]] + site["repl"].encode() + raw[site["end"]:])
            w.git_retry("commit", "--quiet", "--no-verify", "-a", "-m", f"defect-audit mutant {site['id']}")
        else:
            w.set_switch(site["id"])
        first = run_set(job["records"], "first")
        failing = []
        for rec in job["records"]:
            res = first[rec["id"]]
            reached = True if disk else bool(res.get("reached"))
            if disk:
                basis = "baseline coverage"
            elif res.get("batch", 1) > 1:
                basis = f"switch marker from one process that ran {res['batch']} tests, plus baseline coverage of this test"
            else:
                basis = "switch marker written by a process of this test"
            row = {"site": site["id"], "record": rec["id"], "kind": job["kind"], "first": brief(res), "reached": reached, "reach_basis": basis}
            r = res["result"]
            if r == "build_failure":
                row.update(outcome="invalid", reason="the schemata does not build for this mode: " + (res.get("detail") or "")[-600:])
            elif r == "timeout":
                row.update(outcome="inconclusive", reason="timeout")
            elif r in ("skip", "not_selected", "missing"):
                row.update(outcome="inconclusive", reason=r)
            elif r in ("setup_error", "error", "collection_error", "session_error"):
                row.update(outcome="inconclusive", reason=f"{r}: " + (res.get("tail") or "")[-400:])
            elif r == "pass":
                if reached:
                    row.update(outcome="survived")
                else:
                    row.update(outcome="inconclusive", reason="mutated code not reached")
            elif r == "fail":
                if reached:
                    row.update(outcome="pending")
                    failing.append(rec)
                else:
                    row.update(outcome="inconclusive", reason="the test failed, but no process reached the mutated code")
            else:
                row.update(outcome="inconclusive", reason=f"unexpected result {r}")
            rows[rec["id"]] = row
        if failing:
            second = run_set(failing, "second")
            confirmed = []
            for rec in failing:
                res = second[rec["id"]]
                row = rows[rec["id"]]
                row["second"] = brief(res)
                if res["result"] == "fail":
                    row["failure"] = (first[rec["id"]].get("tail") or "")[-600:]
                    confirmed.append(rec)
                else:
                    row.update(outcome="inconclusive", reason=f"unstable: rerun gave {res['result']}")
            if confirmed:
                w.set_switch(None)
                w.reset()
                control = run_set(confirmed, "control")
                for rec in confirmed:
                    res = control[rec["id"]]
                    row = rows[rec["id"]]
                    row["control"] = brief(res)
                    if res["result"] == "pass":
                        row["outcome"] = "detected"
                    else:
                        row.update(outcome="inconclusive", reason=f"the unmutated control run gave {res['result']} at the same time")
    finally:
        w.set_switch(None)
        w.reset()
    return list(rows.values())


class SiteIndex:
    def __init__(self, sites):
        self.by_id = {s["id"]: s for s in sites}
        self.go = collections.defaultdict(list)
        self.line = collections.defaultdict(lambda: collections.defaultdict(list))
        for s in sites:
            if s["lang"] == "go":
                self.go[s["file"]].append(((s["line"], s["col"]), s["id"]))
            else:
                self.line[s["file"]][s["stmt_line"]].append(s["id"])
        for f in self.go:
            self.go[f].sort()
        self.go_keys = {f: [k for k, _ in v] for f, v in self.go.items()}

    def covered(self, cov):
        out = set()
        for f, blocks in cov["go"].items():
            if f not in self.go:
                continue
            keys = self.go_keys[f]
            for sl, sc, el, ec in blocks:
                i = bisect.bisect_left(keys, (sl, sc))
                j = bisect.bisect_right(keys, (el, ec))
                out.update(sid for _, sid in self.go[f][i:j])
        for kind in ("py", "sh"):
            for f, lines in cov[kind].items():
                idx = self.line.get(f)
                if not idx:
                    continue
                for ln in lines:
                    out.update(idx.get(ln, ()))
        return out


SITE_INDEX = None


def load_sites(out):
    global SITE_INDEX
    if SITE_INDEX is None:
        SITE_INDEX = SiteIndex(read_json(out / "sites.json"))
    return SITE_INDEX


def init_worker_sites(out, base, queue, sem, heavy):
    load_sites(pathlib.Path(out))
    init_worker(out, base, queue, sem, heavy)


def is_heavy(rec):
    return rec["lang"] == "go" and (rec.get("tags") == "pyintegration" or bool(rec.get("race")))


class Pools:
    def __init__(self, out, base, workers, compile_slots, heavy_slots):
        ctx = multiprocessing.get_context("spawn")
        self.manager = ctx.Manager()
        sem = self.manager.Semaphore(compile_slots)
        heavy_n = max(0, min(heavy_slots, workers - 1))
        light_n = workers - heavy_n
        self.heavy = self.light = None
        if heavy_n:
            qh = ctx.Queue()
            for i in range(heavy_n):
                qh.put(i)
            self.heavy = concurrent.futures.ProcessPoolExecutor(max_workers=heavy_n, mp_context=ctx, initializer=init_worker_sites, initargs=(str(out), base, qh, sem, None))
        ql = ctx.Queue()
        for i in range(heavy_n, workers):
            ql.put(i)
        self.light = concurrent.futures.ProcessPoolExecutor(max_workers=light_n, mp_context=ctx, initializer=init_worker_sites, initargs=(str(out), base, ql, sem, None))

    def submit(self, heavy, fn, arg):
        return (self.heavy if heavy and self.heavy else self.light).submit(fn, arg)

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        for ex in (self.heavy, self.light):
            if ex:
                ex.shutdown(wait=True, cancel_futures=True)
        self.manager.shutdown()
        return False


def pool(out, base, workers, compile_slots, heavy_slots):
    return Pools(out, base, workers, compile_slots, heavy_slots)


def baseline_ok(b):
    if not b:
        return False, "no baseline"
    if "error" in b:
        return False, f"baseline not measured: {b['error']}"
    if b["plain"]["result"] != "pass":
        return False, f"baseline run without instrumentation: {b['plain']['result']}"
    res = [r["result"] for r in b["instrumented"]]
    if res != ["pass", "pass"]:
        if b["plain"]["result"] == "pass" and "pass" not in res:
            return False, f"instrumentation changed the result: {res}"
        return False, f"unstable instrumented baseline: {res}"
    skipped = b["plain"].get("skipped_subtests") or b["instrumented"][0].get("skipped_subtests")
    if skipped:
        return False, f"skipped subtests: {len(skipped)}"
    return True, ""


def py_guards(tree, sites):
    out = {}
    by_file = collections.defaultdict(list)
    for s in sites:
        by_file[s["file"]].append(s)
    for rel, ss in sorted(by_file.items()):
        raw = (tree / rel).read_bytes()
        try:
            mod = ast.parse(raw)
        except SyntaxError:
            for s in ss:
                out[s["id"]] = {"reason": "file does not parse"}
            continue
        starts = [0]
        for line in raw.split(b"\n")[:-1]:
            starts.append(starts[-1] + len(line) + 1)
        parent = {}
        exact = {}
        ops = []
        stmts = {}
        for node in ast.walk(mod):
            for ch in ast.iter_child_nodes(node):
                parent[ch] = node
            if getattr(node, "end_lineno", None) is None or not hasattr(node, "lineno"):
                continue
            sp = (starts[node.lineno - 1] + node.col_offset, starts[node.end_lineno - 1] + node.end_col_offset)
            if isinstance(node, ast.expr):
                exact.setdefault(sp, node)
                if isinstance(node, (ast.Compare, ast.BinOp)):
                    ops.append((sp[0], sp[1], node))
            elif isinstance(node, ast.stmt):
                stmts.setdefault(sp, node)

        def blocked(n):
            if isinstance(getattr(n, "ctx", None), (ast.Store, ast.Del)):
                return True
            while n in parent:
                n = parent[n]
                if isinstance(n, (ast.JoinedStr, ast.FormattedValue, ast.pattern)):
                    return True
            return False

        for s in ss:
            a, b, sid = s["start"], s["end"], s["id"]
            if s["family"] == "statement_removal":
                if (a, b) not in stmts:
                    out[sid] = {"reason": "statement not found"}
                    continue
                ls = raw.rfind(b"\n", 0, a) + 1
                le = raw.find(b"\n", b)
                le = len(raw) if le < 0 else le
                tail = raw[b:le].strip()
                if raw[ls:a].strip() or (tail and not tail.startswith(b"#")):
                    out[sid] = {"reason": "statement shares its line with other code"}
                    continue
                out[sid] = {"kind": "stmt", "start": a, "end": b, "text": f"if not {py_on(sid)}: " + raw[a:b].decode()}
                continue
            if s["family"] == "arithmetic_swap" and s["op"].endswith("="):
                out[sid] = {"reason": "augmented assignment operator has no expression guard"}
                continue
            node = exact.get((a, b))
            span = (a, b)
            if node is None:
                best = None
                for x0, x1, n in ops:
                    if x0 <= a and b <= x1 and (best is None or x1 - x0 < best[1] - best[0]):
                        best = (x0, x1, n)
                if best is None:
                    out[sid] = {"reason": "no enclosing expression"}
                    continue
                span, node = (best[0], best[1]), best[2]
            if blocked(node):
                out[sid] = {"reason": "expression is an assignment target, pattern, or f-string part"}
                continue
            x0, x1 = span
            orig = raw[x0:x1].decode()
            mut = (raw[x0:a] + s["repl"].encode() + raw[b:x1]).decode()
            out[sid] = {"kind": "expr", "start": x0, "end": x1, "text": f"(({mut}) if {py_on(sid)} else ({orig}))"}
    return out


def go_guards(tree, sites):
    req = [{k: s[k] for k in ("id", "file", "start", "end", "repl", "family", "op")} for s in sites]
    p = subprocess.run(["go", "run", str(ROOT / "scripts/defect_audit_gomutate.go"), "-guards", str(tree)], input=json.dumps(req), text=True, capture_output=True, cwd=ROOT, env=base_env())
    if p.returncode != 0:
        raise AuditError(f"go guard helper failed: {p.stderr[-2000:]}")
    out = {}
    for g in json.loads(p.stdout) or []:
        if g.get("reason"):
            out[g["id"]] = {"reason": g["reason"]}
        else:
            out[g["id"]] = {"kind": g["kind"], "start": g["start"], "end": g["end"], "text": g["text"], "package": g["package"]}
    return out


def base_source(out, tree, base):
    src = out / "src-base"
    marker = src / ".defect-audit-base"
    if marker.exists() and marker.read_text().strip() == base:
        return src
    if src.exists():
        shutil.rmtree(src)
    src.mkdir(parents=True)
    archive = subprocess.run(["git", "archive", "--format=tar", base], cwd=tree, capture_output=True, check=True).stdout
    subprocess.run(["tar", "-x", "-C", str(src)], input=archive, check=True)
    marker.write_text(base + "\n")
    return src


def load_guards(out, tree, sids, base):
    path = out / "guards.json"
    cache = read_json(path) if path.exists() else {}
    if cache.get("_base") != base:
        cache = {"_base": base}
    src = base_source(out, tree, base)
    need = [SITE_INDEX.by_id[s] for s in sorted(sids) if s not in cache]
    for s in need:
        raw = (src / s["file"]).read_bytes()
        if raw[s["start"]: s["end"]].decode(errors="replace") != s["orig"]:
            raise AuditError(f"site {s['id']} text does not match {s['file']} at the base revision")
    tree = src
    if need:
        go = [s for s in need if s["lang"] == "go"]
        py = [s for s in need if s["lang"] == "py"]
        if go:
            cache.update(go_guards(tree, go))
        if py:
            cache.update(py_guards(tree, py))
        for s in need:
            if s["lang"] == "sh":
                cache[s["id"]] = {"kind": "disk", "start": s["start"], "end": s["end"]}
        write_json(path, cache)
    return cache


def apply_guards(raw, gs):
    parts = []
    spans = []
    pos = 0
    delta = 0
    for g in sorted(gs, key=lambda g: g["start"]):
        parts.append(raw[pos:g["start"]])
        t = g["text"].encode()
        ns = g["start"] + delta
        spans.append((ns, ns + len(t), g["id"]))
        parts.append(t)
        delta += len(t) - (g["end"] - g["start"])
        pos = g["end"]
    parts.append(raw[pos:])
    return b"".join(parts), spans


def go_schemata_errors(tree):
    env = base_env()
    linux = {"GOOS": "linux", "GOARCH": "arm64", "CGO_ENABLED": "0"}
    cmds = [
        (["go", "build", "-gcflags=-e", "./..."], {}),
        (["go", "build", "-gcflags=-e", "./..."], linux),
        (["go", "test", "-c", "-vet=off", "-gcflags=-e", "-o", os.devnull, "."], {}),
        (["go", "test", "-c", "-vet=off", "-gcflags=-e", "-tags", "pyintegration", "-o", os.devnull, "."], {}),
        (["go", "test", "-c", "-vet=off", "-gcflags=-e", "-o", os.devnull, "."], linux),
    ]
    mdir = tree / MODULE_DIR
    for cmd, extra in cmds:
        p = run(cmd, cwd=mdir, env={**env, **extra}, check=False, timeout=3600)
        if p.returncode == 0:
            continue
        errs = []
        for line in (p.stdout + p.stderr).splitlines():
            m = re.match(r"^(\S+?\.go):(\d+):(\d+): (.*)$", line.strip())
            if not m:
                continue
            path = pathlib.Path(m[1])
            path = path if path.is_absolute() else (mdir / path)
            try:
                rel = str(path.resolve().relative_to(tree.resolve()))
            except ValueError:
                rel = str(path)
            errs.append((rel, int(m[2]), int(m[3]), m[4]))
        if not errs:
            raise AuditError(f"{shlex.join(cmd)} failed without a parsable error: {(p.stdout + p.stderr)[-2000:]}")
        return errs
    return []


def go_switch_files(tree, packages, switch):
    for d, pkg in packages.items():
        (tree / d / GO_SWITCH_FILE).write_text(GO_SWITCH.replace("{pkg}", pkg).replace("{path}", json.dumps(str(switch))))


def apply_schemata(tree, spec, switch):
    for rel, text in spec["files"].items():
        (tree / rel).write_bytes(text.encode())
    go_switch_files(tree, spec["packages"], switch)


def build_schemata(tree, base, switch, chosen):
    git("reset", "--quiet", "--hard", base, cwd=tree)
    git("clean", "-ffdxq", "-e", "/.venv", cwd=tree)
    if git("rev-parse", "HEAD", cwd=tree).strip() != base or git("status", "--porcelain", cwd=tree).strip():
        raise AuditError(f"{tree} is not a clean checkout of {base}")
    dropped = {}
    by_file = collections.defaultdict(list)
    for g in chosen.values():
        by_file[g["file"]].append(g)
    files = {}
    for rel, gs in sorted(by_file.items()):
        if not rel.endswith(".py"):
            continue
        raw = (tree / rel).read_bytes()
        keep = []
        for g in gs:
            try:
                compile(apply_guards(raw, [g])[0], rel, "exec")
                keep.append(g)
            except (SyntaxError, ValueError) as exc:
                dropped[g["id"]] = f"python guard does not compile: {exc}"
        new = apply_guards(raw, keep)[0]
        try:
            compile(new, rel, "exec")
        except (SyntaxError, ValueError) as exc:
            for g in keep:
                dropped[g["id"]] = f"python guards in one file do not compile together: {exc}"
            continue
        if keep:
            files[rel] = new.decode()
    orig = {rel: (tree / rel).read_bytes() for rel in by_file if rel.endswith(".go")}
    remaining = {rel: list(by_file[rel]) for rel in orig}
    for _ in range(12):
        packages = {}
        layout = {}
        for rel, gs in remaining.items():
            new, spans = apply_guards(orig[rel], gs)
            (tree / rel).write_bytes(new)
            layout[rel] = (new, spans)
            if gs:
                packages[str(pathlib.PurePosixPath(rel).parent)] = gs[0]["package"]
        for d in {str(pathlib.PurePosixPath(rel).parent) for rel in orig}:
            sw = tree / d / GO_SWITCH_FILE
            if sw.exists():
                sw.unlink()
        go_switch_files(tree, packages, switch)
        errs = go_schemata_errors(tree)
        if not errs:
            break
        hit = {}
        for rel, line, col, msg in errs:
            if rel not in layout or not layout[rel][1]:
                raise AuditError(f"schemata build error outside a guard: {rel}:{line}:{col}: {msg}")
            new, spans = layout[rel]
            lines = new.split(b"\n")
            off = sum(len(x) + 1 for x in lines[: line - 1]) + col - 1
            sid = next((s for a, b, s in spans if a <= off <= b), None)
            if sid is None:
                sid = min(spans, key=lambda x: min(abs(x[0] - off), abs(x[1] - off)))[2]
            hit[sid] = (rel, f"go guard does not build: {msg}")
        for sid, (rel, why) in hit.items():
            dropped[sid] = why
            remaining[rel] = [g for g in remaining[rel] if g["id"] != sid]
    else:
        raise AuditError("the Go schemata still fails to build after 12 repair passes")
    packages = {}
    for rel, gs in remaining.items():
        if gs:
            files[rel] = apply_guards(orig[rel], gs)[0].decode()
            packages[str(pathlib.PurePosixPath(rel).parent)] = gs[0]["package"]
    git("reset", "--quiet", "--hard", base, cwd=tree)
    git("clean", "-ffdxq", "-e", "/.venv", cwd=tree)
    return {"files": files, "packages": packages}, dropped


def select_sites(eligible, baselines, guards, by_rec, banned, rnd, k_go, k_py, cap=30, pool_size=300):
    cands = {}
    for rec in eligible:
        attempted = {m["site"] for m in by_rec[rec["id"]]}
        cands[rec["id"]] = [sid for sid in baselines[rec["id"]]["covered_sites"] if sid not in attempted and sid not in banned and "kind" in guards.get(sid, {})]
    needy = collections.Counter()
    for rid, sids in cands.items():
        needy.update(sids)
    chosen = set()
    spans = collections.defaultdict(list)
    plan = {}

    def free(sid):
        g = guards[sid]
        if g["kind"] == "disk":
            return True
        f = SITE_INDEX.by_id[sid]["file"]
        return all(g["end"] <= a or b <= g["start"] for a, b in spans[f])

    for rec in sorted(eligible, key=lambda r: (len(cands[r["id"]]), r["id"])):
        k = k_go if rec["lang"] == "go" else k_py
        rng = random.Random(f"{rec['id']}:{rnd}")
        by_family = collections.defaultdict(list)
        for sid in cands[rec["id"]]:
            by_family[SITE_INDEX.by_id[sid]["family"]].append(sid)
        fams = sorted(by_family)
        rng.shuffle(fams)
        picked = []
        lines = set()
        progressed = True
        while len(picked) < k and progressed:
            progressed = False
            for fam in fams:
                if len(picked) >= k:
                    break
                opts = [sid for sid in by_family[fam] if sid not in picked and (SITE_INDEX.by_id[sid]["file"], SITE_INDEX.by_id[sid]["stmt_line"]) not in lines]
                if not opts:
                    continue
                reuse = sorted(sid for sid in opts if sid in chosen)
                if reuse:
                    sid = rng.choice(reuse)
                else:
                    pool = opts if len(opts) <= pool_size else rng.sample(opts, pool_size)
                    pool = [sid for sid in pool if free(sid)]
                    if not pool:
                        continue
                    sid = max(pool, key=lambda s: (min(needy[s], cap), rng.random()))
                    chosen.add(sid)
                    g = guards[sid]
                    if g["kind"] != "disk":
                        spans[SITE_INDEX.by_id[sid]["file"]].append((g["start"], g["end"]))
                s = SITE_INDEX.by_id[sid]
                lines.add((s["file"], s["stmt_line"]))
                picked.append(sid)
                progressed = True
        plan[rec["id"]] = picked
        needy.subtract(cands[rec["id"]])
    return plan


def mutant_phase(out, args, base, active, baselines):
    tree = out / "workers" / "w0" / "tree"
    switch = out / "workers" / "w0" / "infra" / "mutant"
    mpath = out / "results" / "mutants.jsonl"
    recs = {r["id"]: r for r in active}
    rounds = [(1, args.go_mutants, args.py_mutants)] + ([(2, args.escalate, args.escalate)] if args.escalate else [])
    for rnd, k_go, k_py in rounds:
        prior = read_jsonl(mpath)
        by_rec = collections.defaultdict(list)
        for m in prior:
            by_rec[m["record"]].append(m)
        plan_path = out / "results" / f"round{rnd}-plan.json"
        spec_path = out / "results" / f"round{rnd}-schemata.json"
        if not spec_path.exists():
            eligible = []
            for rec in sorted(active, key=lambda r: r["id"]):
                if not baseline_ok(baselines.get(rec["id"]))[0]:
                    continue
                if rnd == 2 and any(m["outcome"] == "detected" for m in by_rec[rec["id"]]):
                    continue
                eligible.append(rec)
            sids = {sid for rec in eligible for sid in baselines[rec["id"]]["covered_sites"]}
            t0 = time.time()
            guards = load_guards(out, tree, sids, base)
            print(f"round {rnd}: {len(eligible)} records, {len(sids)} candidate sites, {sum('kind' in guards.get(s, {}) for s in sids)} with a guard ({time.time() - t0:.0f}s)", flush=True)
            banned = {}
            for attempt in range(40):
                plan = select_sites(eligible, baselines, guards, by_rec, banned, rnd, k_go, k_py)
                chosen = {}
                for sids_ in plan.values():
                    for sid in sids_:
                        if guards[sid]["kind"] != "disk":
                            chosen[sid] = {**guards[sid], "id": sid, "file": SITE_INDEX.by_id[sid]["file"]}
                t0 = time.time()
                spec, dropped = build_schemata(tree, base, switch, chosen)
                print(f"round {rnd}: schemata pass {attempt + 1}: {len(chosen)} guarded sites, {len(dropped)} dropped ({time.time() - t0:.0f}s)" + "".join(f"\n  dropped {sid} {SITE_INDEX.by_id[sid]['file']}:{SITE_INDEX.by_id[sid]['line']} {why[:160]}" for sid, why in sorted(dropped.items())), flush=True)
                if not dropped:
                    break
                banned.update(dropped)
            else:
                raise AuditError("site selection did not converge on a schemata that builds")
            spec["dropped"] = banned
            write_json(plan_path, plan)
            write_json(spec_path, spec)
        plan = read_json(plan_path)
        guards = read_json(out / "guards.json")
        done = {(m["site"], m["record"]) for m in prior if m.get("round") == rnd}
        groups = collections.defaultdict(list)
        for rid, sids in plan.items():
            for sid in sids:
                if (sid, rid) not in done:
                    groups[sid].append(recs[rid])
        jobs = []
        for sid, rs in groups.items():
            est = {r["id"]: baselines[r["id"]]["plain"].get("elapsed", 0) for r in rs}
            chunks = []
            for r in sorted(rs, key=lambda r: (-est[r["id"]], r["id"])):
                fit = next((c for c in chunks if c[0] + est[r["id"]] <= args.max_job_seconds), None)
                if fit is None:
                    fit = [0.0, []]
                    chunks.append(fit)
                fit[0] += est[r["id"]]
                fit[1].append(r)
            for cost, members in chunks:
                jobs.append({"round": rnd, "site": sid, "kind": "disk" if guards[sid]["kind"] == "disk" else "switch", "records": sorted(members, key=lambda r: r["id"]), "timeouts": {r["id"]: job_timeout(baselines[r["id"]]) for r in members}, "cost": cost})
        jobs.sort(key=lambda j: (-j["cost"], j["site"]))
        print(f"round {rnd}: {len(jobs)} mutants for {sum(len(j['records']) for j in jobs)} record runs", flush=True)
        t0 = time.time()
        with pool(out, base, args.workers, args.compile_slots, args.heavy_slots) as ex:
            futs = {ex.submit(any(is_heavy(r) for r in j["records"]), task_job, j): j for j in jobs}
            n = 0
            for fut in concurrent.futures.as_completed(futs):
                j = futs[fut]
                try:
                    rows = fut.result()
                except Exception as exc:
                    rows = [{"site": j["site"], "record": r["id"], "kind": j["kind"], "outcome": "inconclusive", "reason": f"driver error: {type(exc).__name__}: {exc}"} for r in j["records"]]
                for row in rows:
                    row["round"] = rnd
                append_jsonl(mpath, rows)
                n += 1
                if n % 25 == 0 or n == len(jobs):
                    print(f"round {rnd}: {n}/{len(jobs)} mutants ({time.time() - t0:.0f}s)", flush=True)


def cmd_run(args):
    out = pathlib.Path(args.out).resolve()
    inv = read_json(out / "inventory.json")
    base = inv["base"]
    load_sites(out)
    sys.path.append(str(out / "tools" / "pylib"))
    recs = records_from_inventory(inv)
    if args.select:
        recs = [r for r in recs if re.search(args.select, r["id"])]
    active = [r for r in recs if not r["exclusion"]]
    bpath = out / "results" / "baseline.jsonl"
    done = {b["record"] for b in read_jsonl(bpath)}
    todo = [r for r in active if r["id"] not in done]
    heavy = sorted((r for r in todo if r["lang"] == "go" and (r["tags"] == "pyintegration" or r["race"])), key=lambda r: r["id"])
    light = sorted((r for r in todo if not (r["lang"] == "go" and (r["tags"] == "pyintegration" or r["race"]))), key=lambda r: r["id"])
    step = max(1, len(light) // max(1, len(heavy)))
    todo = []
    while heavy or light:
        if heavy:
            todo.append(heavy.pop(0))
        todo.extend(light[:step])
        del light[:step]
    print(f"baseline: {len(todo)} records to run, {len(done)} done", flush=True)
    with pool(out, base, args.workers, args.compile_slots, args.heavy_slots) as ex:
        futs = {ex.submit(is_heavy(r), task_baseline, r): r for r in todo}
        n = 0
        for fut in concurrent.futures.as_completed(futs):
            r = futs[fut]
            try:
                row = fut.result()
            except Exception as exc:
                row = {"record": r["id"], "error": f"{type(exc).__name__}: {exc}"}
            append_jsonl(bpath, [row])
            n += 1
            if n % 50 == 0:
                print(f"baseline {n}/{len(todo)}", flush=True)
    rows = read_jsonl(bpath)
    latest = {b["record"]: b for b in rows}
    exclusive_done = {b["record"] for b in rows if b.get("exclusive")}
    def all_timeouts(b):
        return "error" not in b and [b["plain"]["result"], *(x["result"] for x in b["instrumented"])] == ["timeout"] * 3
    retry = [r for r in active if r["id"] in latest and r["id"] not in exclusive_done and not baseline_ok(latest[r["id"]])[0] and "error" not in latest[r["id"]] and not all_timeouts(latest[r["id"]])]
    retry += [r for r in active if r["id"] in latest and "error" in latest[r["id"]] and r["id"] not in exclusive_done]
    if retry:
        print(f"baseline: {len(retry)} records rerun alone", flush=True)
        with pool(out, base, 1, 1, 1) as ex:
            for r in retry:
                try:
                    row = ex.submit(False, task_baseline, r).result()
                except Exception as exc:
                    row = {"record": r["id"], "error": f"{type(exc).__name__}: {exc}"}
                row["exclusive"] = True
                row["parallel_result"] = {k: v for k, v in latest[r["id"]].items() if k in ("plain", "instrumented", "error")}
                append_jsonl(bpath, [row])
    baselines = {b["record"]: b for b in read_jsonl(bpath)}
    mutant_phase(out, args, base, active, baselines)


def static_dependencies(rec, tree, tracked_files):
    try:
        text = (tree / rec["file"]).read_text()
    except OSError:
        return []
    if rec["lang"] == "go":
        body = go_test_body(text, rec["test"])
    else:
        name = rec["test"].split("::")[-1].split("[")[0]
        m = re.search(rf"^(\s*)def {re.escape(name)}\(", text, re.M)
        if not m:
            return []
        ind = len(m[1])
        rest = text[m.end():]
        nxt = re.search(rf"^\s{{0,{ind}}}(def |class |@)", rest, re.M)
        body = text[m.start(): m.end() + (nxt.start() if nxt else len(rest))]
    lits = set(re.findall(r'"([^"\n]{3,200})"', body)) | set(re.findall(r"'([^'\n]{3,200})'", body)) | set(re.findall(r"`([^`\n]{3,200})`", body))
    deps = set()
    basenames = collections.defaultdict(list)
    for f in tracked_files:
        basenames[f.rsplit("/", 1)[-1]].append(f)
    for lit in lits:
        lit = lit.strip().lstrip("./")
        for cand in (lit, f"{MODULE_DIR}/{lit}", lit.replace("../", "")):
            if cand in tracked_files:
                deps.add(cand)
        bn = lit.rsplit("/", 1)[-1]
        if "." in bn and len(basenames.get(bn, [])) == 1 and not bn.startswith("."):
            deps.add(basenames[bn][0])
    return sorted(d for d in deps if d != rec["file"])


def protected_area(rec):
    text = (rec["test"] + " " + rec["file"]).lower()
    return sorted({h for h in PROTECTED_HINTS if h in text})


def cmd_report(args):
    out = pathlib.Path(args.out).resolve()
    inv = read_json(out / "inventory.json")
    meta = read_json(out / "meta.json")
    load_sites(out)
    recs = records_from_inventory(inv)
    baselines = {b["record"]: b for b in read_jsonl(out / "results" / "baseline.jsonl")}
    muts = collections.defaultdict(list)
    for m in read_jsonl(out / "results" / "mutants.jsonl"):
        muts[m["record"]].append(m)
    tree = out / "workers" / "w0" / "tree"
    tracked_files = set(git("ls-files", cwd=tree).splitlines())
    rows = []
    status = collections.Counter()
    for rec in recs:
        row = {k: rec[k] for k in ("id", "lang", "mode", "test", "file", "platform")}
        row["protected_area_terms"] = protected_area(rec)
        gaps = []
        if rec["exclusion"]:
            row.update(status="excluded", exclusion=rec["exclusion"])
            status["excluded"] += 1
            rows.append(row)
            continue
        b = baselines.get(rec["id"])
        if b is None or "error" in b:
            row.update(status="inconclusive", gaps=[f"baseline not measured: {(b or {}).get('error', 'missing')}"])
            status["inconclusive"] += 1
            rows.append(row)
            continue
        row["baseline"] = {"plain": b["plain"]["result"], "instrumented": [r["result"] for r in b["instrumented"]], "elapsed_s": b["plain"].get("elapsed"), "subtests": b["plain"].get("subtests")}
        if b.get("exclusive"):
            par = b.get("parallel_result") or {}
            row["baseline"]["measured_alone"] = True
            row["baseline"]["parallel_run"] = {"plain": (par.get("plain") or {}).get("result"), "instrumented": [r.get("result") for r in par.get("instrumented") or []], "error": par.get("error")}
        ok, why = baseline_ok(b)
        if not ok:
            gaps.append(why)
            for r in [b["plain"], *b["instrumented"]]:
                if r.get("tail"):
                    row["baseline_tail"] = r["tail"][-800:]
                    break
        cov = b.get("coverage")
        row["coverage"] = cov
        gaps += b.get("coverage_gaps") or []
        deps = static_dependencies(rec, tree, tracked_files)
        measured = set((cov or {}).get("go_files", {})) | set((cov or {}).get("py_files", {})) | set((cov or {}).get("sh_files", {})) | set((cov or {}).get("harness_files", []))
        row["static_dependencies"] = deps
        unmeasured = [d for d in deps if d not in measured]
        row["unmeasured_dependencies"] = unmeasured
        if cov and rec["lang"] == "go" and cov["processes"]["go"] <= 1:
            src = (tree / rec["file"]).read_text()
            if "pyintegrationBinary(" in src or re.search(r'"go", "build"', src):
                gaps.append("the test file builds a Go binary, but no child Go process wrote coverage for this test; a child that clears its environment is not measured")
        if cov and (cov.get("copied_py_files") or cov.get("copied_sh_files")):
            gaps.append("some code ran from copies outside the scratch tree; mutations reach those copies only through the committed mutant")
        product_lines = sum(sum((cov or {}).get(k, {}).values()) for k in ("go_files", "py_files", "sh_files"))
        row["covered_sites"] = len(b.get("covered_sites") or [])
        ms = muts.get(rec["id"], [])
        row["mutations"] = []
        for m in ms:
            s = SITE_INDEX.by_id.get(m["site"], {})
            row["mutations"].append({"site": m["site"], "round": m.get("round"), "file": s.get("file"), "line": s.get("line"), "function": s.get("function"), "family": s.get("family"), "op": s.get("op"), "orig": s.get("orig"), "repl": s.get("repl"), "outcome": m["outcome"], "reason": m.get("reason"), "reached": m.get("reached"), "reach_basis": m.get("reach_basis"), "runs": {k: (m.get(k) or {}).get("result") for k in ("first", "second", "control") if m.get(k)}})
        oc = collections.Counter(m["outcome"] for m in ms)
        row["outcomes"] = dict(oc)
        if not ok:
            st = "inconclusive"
        elif product_lines == 0 and not cov.get("copied_py_files") and not cov.get("copied_sh_files"):
            st = "review_candidate"
            gaps.append("no product code execution measured" + (f"; unmeasured dependencies: {', '.join(unmeasured)}" if unmeasured else ""))
        elif oc.get("detected"):
            st = "detects"
        elif oc.get("survived"):
            st = "review_candidate"
        else:
            st = "inconclusive"
            gaps.append("no conclusive mutation result" if ms else "no mutation site in the measured code")
        if st == "review_candidate" and unmeasured:
            gaps.append("checks dependencies the coverage tools cannot measure: " + ", ".join(unmeasured))
        row["status"] = st
        row["gaps"] = gaps
        status[st] += 1
        rows.append(row)
    rec_path = out / "records.jsonl"
    rec_path.write_text("".join(json.dumps(r, sort_keys=True) + "\n" for r in rows))
    decl_names = collections.Counter(d["name"] for d in inv["go_declarations"])
    go_recorded = {r["test"] for r in rows if r["lang"] == "go"}
    missing_decls = sorted(n for n in decl_names if n not in go_recorded)
    py_expected = sum(len(v) for v in inv["python"].values())
    py_recorded = sum(1 for r in rows if r["lang"] == "py")
    families = collections.Counter()
    outcome_by_lang = collections.Counter()
    for r in rows:
        for m in r.get("mutations", []):
            families[(m["family"], m["outcome"])] += 1
            outcome_by_lang[(r["lang"], m["outcome"])] += 1
    lines = [
        "# Test defect audit",
        "",
        f"Baseline: `{meta['base']}`",
        "",
        "## Tools",
        "",
        *[f"- {k}: {v}" for k, v in sorted(meta["tools"].items())],
        "",
        "## Inventory",
        "",
        f"- Go test declarations: {len(inv['go_declarations'])} ({len(decl_names)} names) in {len({d['file'] for d in inv['go_declarations']})} files; declarations with no record: {len(missing_decls)} {missing_decls if missing_decls else ''}",
        *[f"- `{m['name']}` ({m['step']}): {len(m['tests'])} tests, {sum(t['platform'] == 'linux' for t in m['tests'])} run in the Linux container. Command: `{m['command']}`" for m in inv["go_modes"]],
        *[f"- Python `{k}`: {len(v)} node ids" for k, v in inv["python"].items()],
        f"- Python records: {py_recorded} of {py_expected}",
        f"- Standalone shell suites outside the removal inventory: {', '.join(inv['standalone_shell_suites'])}",
        "",
        "## Record status",
        "",
        *[f"- {k}: {v}" for k, v in sorted(status.items())],
        "",
        "## Mutation outcomes by language",
        "",
        *[f"- {k[0]} {k[1]}: {v}" for k, v in sorted(outcome_by_lang.items())],
        "",
        "## Mutation outcomes by family",
        "",
        *[f"- {k[0]} {k[1]}: {v}" for k, v in sorted(families.items())],
        "",
        "## Review candidates",
        "",
    ]
    for r in rows:
        if r["status"] == "review_candidate":
            lines.append(f"- `{r['id']}`: outcomes {r.get('outcomes')}; protected terms {r['protected_area_terms']}; gaps: {'; '.join(r['gaps']) or 'none'}")
    (out / "summary.md").write_text("\n".join(lines) + "\n")
    print("\n".join(lines[:60]))
    print(f"records written to {rec_path}")


SAFETY_HINTS = PROTECTED_HINTS + (
    "reconcil", "storage", "order", "recover", "update", "upgrade", "lock", "partition", "egress", "refus", "fail",
    "pause", "admission", "probe", "secret", "token", "auth", "session", "verif", "reset", "backup", "restore",
    "rollback", "deploy", "retired", "orphan", "corrupt", "state", "journal", "alert", "notif", "trade", "position",
    "loss", "own_share", "correlat", "_marks", "marks_", "mark_price", "wallet", "capital", "leverage", "balance", "quote", "pricing", "portfolio", "execut", "sell", "buy", "cancel", "unconfirmed", "quantit", "commission", "slippage", "robinhood", "hyperliquid", "ibkr", "deribit", "okx",
)
AGENT_RULES = "CLAUDE.md with the .claude/rules files it lists"
RULE_DOCS = ("SKILL.md", "README.md", "backtest/AUDIT.md", "docs/backtesting-registry.md", "docs/POST_UPDATE_HISTORY.md")


def agent_guide():
    spec = importlib.util.spec_from_file_location("agent_guide", ROOT / "scripts" / "agent_guide.py")
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def rule_sets():
    guide = agent_guide()
    try:
        rules = guide.rules_text(ROOT)
    except guide.GuideError as exc:
        raise AuditError(f"cannot read the agent rules ({AGENT_RULES}): {exc}")
    docs = "\n".join([rules] + [(ROOT / d).read_text() for d in RULE_DOCS if (ROOT / d).exists()])
    return docs, guide.guard_globs(rules)


def cmd_guard_set(args):
    docs, guard_globs = rule_sets()
    print(json.dumps({"guard_globs": guard_globs, "rule_doc_tokens": len(set(re.findall(r"\w+", docs)))}, indent=1))


def py_decl_key(rec):
    return re.sub(r"\[.*\]$", "", rec["test"])


def cmd_prune(args):
    out = pathlib.Path(args.out).resolve()
    recs = {r["id"]: r for r in read_jsonl(out / "records.jsonl")}
    baselines = {b["record"]: b for b in read_jsonl(out / "results" / "baseline.jsonl")}
    cov = {rid: frozenset((baselines.get(rid) or {}).get("covered_sites") or ()) for rid in recs}
    det = collections.defaultdict(set)
    for m in read_jsonl(out / "results" / "mutants.jsonl"):
        if m["outcome"] == "detected":
            det[m["record"]].add(m["site"])
    detectors = collections.defaultdict(set)
    for rid, sites in det.items():
        for sid in sites:
            detectors[sid].add(rid)
    holders = collections.defaultdict(set)
    for rid, sites in cov.items():
        for sid in sites:
            holders[sid].add(rid)

    rule_docs, guard_globs = rule_sets()

    def named_in_rules(r):
        name = r["test"] if r["lang"] == "go" else py_decl_key(r).rsplit("::", 1)[-1]
        return re.search(rf"\b{re.escape(name)}\b", rule_docs) is not None

    def safe_path(path):
        text = path.lower().replace("shared_", "")
        name = path.rsplit("/", 1)[-1]
        return any(h in text for h in SAFETY_HINTS) or any(fnmatch.fnmatch(name, g) or fnmatch.fnmatch(path, g) or path.endswith("/" + g) for g in guard_globs)

    def code_file(test_file):
        d, _, name = test_file.rpartition("/")
        if name.endswith("_test.go"):
            name = name[:-8] + ".go"
        elif name.startswith("test_"):
            name = name[5:]
        return (d + "/" if d else "") + name

    def safety(r):
        return any(h in r["test"].lower() for h in SAFETY_HINTS) or safe_path(r["file"]) or safe_path(code_file(r["file"]))

    def detects_safety(r):
        return any(safe_path(m.get("file") or "") for m in r.get("mutations") or [] if m.get("outcome") == "detected")

    def eligible(r):
        return not (r["mode"] == "py/options" or safety(r) or r.get("gaps") or r.get("unmeasured_dependencies") or r["status"] in ("excluded", "inconclusive") or named_in_rules(r))

    def decl(r):
        return ("go", r["test"]) if r["lang"] == "go" else ("py", py_decl_key(r))

    members = collections.defaultdict(set)
    for rid, r in recs.items():
        members[decl(r)].add(rid)
    def processes(rid):
        return ((baselines.get(rid) or {}).get("coverage") or {}).get("processes")

    def in_process(r):
        p = processes(r["id"])
        if p is None:
            return False
        if r["lang"] == "go":
            return p["go"] <= 1 and p["py"] == 0 and p["sh"] == 0
        return p["py"] <= 1 and p["go"] == 0 and p["sh"] == 0

    weak = {rid for rid, r in recs.items() if eligible(r) and r["status"] == "review_candidate" and r["mutations"]}
    unit = {rid for rid, r in recs.items() if eligible(r) and rid not in weak and r["status"] == "detects" and in_process(r) and not detects_safety(r)}
    remove = weak | unit
    subsumer = {}
    for r in sorted((r for r in recs.values() if eligible(r) and r["status"] == "detects" and cov[r["id"]]), key=lambda r: (len(cov[r["id"]]), r["id"])):
        rid = r["id"]
        rare = min(cov[rid], key=lambda s: (len(holders[s]), s))
        for sid in sorted(holders[rare]):
            if sid == rid or sid in remove or recs[sid]["status"] in ("excluded", "inconclusive") or not cov[rid] <= cov[sid]:
                continue
            if all(detectors[m] - remove - {rid} for m in det[rid]):
                subsumer[rid] = sid
                remove.add(rid)
                break
    while True:
        changed = False
        for key, ids in members.items():
            if ids & remove and not ids <= remove:
                remove -= ids
                changed = True
        for rid in sorted(subsumer):
            if rid not in remove:
                continue
            kept = [s for s in sorted(holders[min(cov[rid], key=lambda s: (len(holders[s]), s))]) if s not in remove and s != rid and recs[s]["status"] not in ("excluded", "inconclusive") and cov[rid] <= cov[s]]
            if not kept or not all(detectors[m] - remove for m in det[rid]):
                remove.discard(rid)
                changed = True
            else:
                subsumer[rid] = kept[0]
        if not changed:
            break
    plan = {}
    for rid in sorted(remove):
        r = recs[rid]
        if rid in unit:
            plan[rid] = {"rule": "unit_outside_protected", "file": r["file"], "declaration": decl(r)[1], "evidence": {"processes": processes(rid), "outcomes": r["outcomes"]}}
        elif rid in weak:
            plan[rid] = {"rule": "no_detection", "file": r["file"], "declaration": decl(r)[1], "evidence": {"outcomes": r["outcomes"], "mutations": len(r["mutations"])}}
        else:
            plan[rid] = {"rule": "subsumed", "file": r["file"], "declaration": decl(r)[1], "evidence": {"subsumed_by": subsumer[rid], "covered_sites": len(cov[rid]), "subsumer_covered_sites": len(cov[subsumer[rid]]), "detected": {m: sorted(detectors[m] - remove) for m in sorted(det[rid])}}}
    held = sorted(rid for rid in weak | unit | set(subsumer) if rid not in remove)
    write_json(out / "prune.json", {"rules": {"eligible": "not in a collection that CI runs by file path (options); not excluded or inconclusive; no safety term in the test name, its file path, or the code file it is named for (" + ", ".join(SAFETY_HINTS) + "); none of those files matches a file that the agent rules (" + AGENT_RULES + ") name as a guardrail (" + ", ".join(guard_globs) + "); no measurement gap; no unmeasured dependency; not named in a rule document (" + ", ".join((AGENT_RULES,) + RULE_DOCS) + ")", "unit_outside_protected": "an eligible detecting test that ran only in the test process (no child Go, Python, or shell process wrote coverage) and detects no mutant in a file with a safety term or an agent-rules guardrail file; the agent rules allow unit tests only for rare venue states, money math, paper/live parity, DB migrations, and large refactors, which the eligibility filter keeps", "no_detection": "an eligible review candidate with at least one completed mutation and no detection in either round", "subsumed": "an eligible detecting test whose covered sites are a subset of one kept test's covered sites, where every sampled mutant it detects is also detected by a kept test", "declaration": "a Go test function or Python test function is deleted only when every record of it is removed"}, "remove": plan, "held_by_declaration": held})
    print(json.dumps({"unit_outside_protected": sum(p["rule"] == "unit_outside_protected" for p in plan.values()), "no_detection": sum(p["rule"] == "no_detection" for p in plan.values()), "subsumed": sum(p["rule"] == "subsumed" for p in plan.values()), "held_by_declaration": len(held), "declarations": len({(p["file"], p["declaration"]) for p in plan.values()})}, indent=1))


def cmd_removals(args):
    out = pathlib.Path(args.out).resolve()
    inv = read_json(out / "inventory.json")
    base = inv["base"]
    head = git("rev-parse", "HEAD").strip()
    changed = [p for p in git("diff", "--name-only", "--no-renames", base, head).splitlines() if is_test_file(p)]
    diff = git("diff", "--unified=0", "--no-renames", base, head, "--", *(":(literal)" + p for p in changed)) if changed else ""
    removed_decls = []
    added_decls = []
    for line in diff.splitlines():
        m = re.match(r"^([+-])(?:func (Test\w+)\(|\s*def (test_\w+)\()", line)
        if m:
            (removed_decls if m[1] == "-" else added_decls).append(m[2] or m[3])
    changed_bodies = []
    for line in diff.splitlines():
        if line.startswith("+") and not line.startswith("+++"):
            changed_bodies.append(line)
    tmp = out / "removals"
    tmp.mkdir(exist_ok=True)
    current_py = {}
    for suite, pargs in PY_COLLECTIONS:
        cmd = [sys.executable, "scripts/check_pytest_shards.py", "collect", suite, "--out", str(tmp / f"{suite}.json")]
        if pargs:
            cmd += ["--", *pargs]
        run(cmd, cwd=ROOT, env=base_env())
        current_py[suite] = set(read_json(tmp / f"{suite}.json")["nodeids"])
    removed_ids = []
    for suite, ids in inv["python"].items():
        removed_ids += [f"py/{suite}::{n}" for n in ids if n not in current_py[suite]]
    cur_decls = {d["name"] for d in go_declarations(ROOT)}
    removed_ids += [f"go::{d['name']}" for d in inv["go_declarations"] if d["name"] not in cur_decls]
    plan = read_json(out / "prune.json")["remove"] if (out / "prune.json").exists() else {}
    planned = set()
    for rid in plan:
        planned.add(f"go::{rid.split('::', 1)[1]}" if rid.startswith("go/") else rid)
    unplanned = sorted(set(removed_ids) - planned)
    not_removed = sorted(planned - set(removed_ids))
    report = {"base": base, "head": head, "removed_test_ids": sorted(removed_ids), "removed_declarations": sorted(removed_decls), "added_declarations": sorted(added_decls), "added_lines_in_test_files": changed_bodies, "removed_without_plan": unplanned, "planned_but_present": not_removed}
    write_json(out / "removals.json", report)
    print(json.dumps({k: (v if not isinstance(v, list) else len(v)) for k, v in report.items()}, indent=1))
    if added_decls or unplanned or not_removed:
        print("removed test IDs differ from the prune plan, or a test declaration was added")
        sys.exit(1)


def main():
    ap = argparse.ArgumentParser(description="Measure which sampled defects the Go and Python tests detect.")
    sub = ap.add_subparsers(dest="cmd", required=True)
    p = sub.add_parser("plan")
    p.add_argument("--out", required=True)
    p.add_argument("--base", required=True)
    p = sub.add_parser("run")
    p.add_argument("--out", required=True)
    p.add_argument("--workers", type=int, default=max(1, (os.cpu_count() or 2) - 2))
    p.add_argument("--compile-slots", type=int, default=2)
    p.add_argument("--heavy-slots", type=int, default=2)
    p.add_argument("--go-mutants", type=int, default=4)
    p.add_argument("--py-mutants", type=int, default=5)
    p.add_argument("--escalate", type=int, default=0)
    p.add_argument("--max-job-seconds", type=int, default=150)
    p.add_argument("--select")
    p = sub.add_parser("report")
    p.add_argument("--out", required=True)
    p = sub.add_parser("prune")
    p.add_argument("--out", required=True)
    p = sub.add_parser("removals")
    p.add_argument("--out", required=True)
    sub.add_parser("guard-set")
    args = ap.parse_args()
    try:
        {"plan": cmd_plan, "run": cmd_run, "report": cmd_report, "prune": cmd_prune, "removals": cmd_removals, "guard-set": cmd_guard_set}[args.cmd](args)
    except AuditError as exc:
        print(f"defect_audit: {exc}", file=sys.stderr)
        sys.exit(1)


if __name__ == "__main__":
    main()

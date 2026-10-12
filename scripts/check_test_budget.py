#!/usr/bin/env python3
import argparse
import ast
import importlib.util
import json
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
BASELINE = ROOT / "scripts" / "test_budget_baseline.json"
COMPOSITION = ROOT / "shared_tools" / "strategy_composition.py"
PY_ROOTS = ["shared_tools", "shared_strategies", "platforms", "backtest", "shared_scripts", "scripts", ".github/scripts"]

WORDING_RULE = "wording_only_tests"
REFUSAL_GRID_RULE = "refusal_grid_tests"
RULES = (WORDING_RULE, REFUSAL_GRID_RULE)

REFUSAL_CLASSES = ("explicit live mode", "missing mode", "invalid mode input", "invalid acknowledgement")
REFUSAL_GRID_MIN_CASES = 3
REFERENCE_TOKENS = ("--strategy-refs", "--open-strategy", "--close-strateg")
STRATEGY_SECTION = "SKILL.md section Add Or Change Strategies (no-edge registration step)"
NAME_DEPTH = 8

GO_FUNC = re.compile(r"^func (Test\w+)\(", re.M)
GO_WORDING = re.compile(r"strings\.(Contains|HasPrefix|HasSuffix|EqualFold)\(")
GO_ASSERT = re.compile(r"\bt\.(Fatal|Fatalf|Error|Errorf)\(")
PY_FUNC = re.compile(r"^(?:    )?def (test_\w+)\(", re.M)
PY_ASSERT = re.compile(r"^\s*assert\b")
PY_WORDING = re.compile(r'^\s*assert\s+(?:not\s+)?["\'][^"\']*["\']\s+(?:not\s+)?in\b')


class BudgetError(Exception):
    pass


def go_bodies(text):
    starts = [(m.start(), m.group(1)) for m in GO_FUNC.finditer(text)]
    for i, (start, name) in enumerate(starts):
        end = starts[i + 1][0] if i + 1 < len(starts) else len(text)
        yield name, text[start:end]


def py_bodies(text):
    starts = [(m.start(), m.group(1)) for m in PY_FUNC.finditer(text)]
    for i, (start, name) in enumerate(starts):
        end = starts[i + 1][0] if i + 1 < len(starts) else len(text)
        yield name, text[start:end]


def go_wording_only(body):
    lines = body.splitlines()
    asserts = [ln for ln in lines if GO_ASSERT.search(ln)]
    if not asserts:
        return False
    conds = [ln for ln in lines if re.match(r"\s*if\b", ln)]
    if not conds:
        return False
    return all(GO_WORDING.search(ln) for ln in conds)


def py_wording_only(body):
    asserts = [ln for ln in body.splitlines() if PY_ASSERT.match(ln)]
    if not asserts:
        return False
    return all(PY_WORDING.match(ln) for ln in asserts)


def load_composition():
    try:
        spec = importlib.util.spec_from_file_location("strategy_composition_for_test_budget", COMPOSITION)
        module = importlib.util.module_from_spec(spec)
        sys.modules[spec.name] = module
        spec.loader.exec_module(module)
    except Exception as exc:
        raise BudgetError(f"cannot load the admission parser {COMPOSITION.relative_to(ROOT)}: {exc}") from exc
    return module


def refusal_class(composition, tokens):
    try:
        composition.admit_strategy_references(
            [composition.StrategyReference("open", "budget_probe")],
            composition.parse_raw_gate_mode(list(tokens)),
            composition.parse_allow_no_edge_tokens(list(tokens)),
            lambda _name: {"edge_status": composition.EDGE_STATUS_NO_EDGE},
        )
    except ValueError as exc:
        return next((c for c in REFUSAL_CLASSES if c in str(exc)), None)
    return None


class SourceModule:
    def __init__(self, path, text):
        self.path = path
        self.tree = ast.parse(text, filename=str(path))
        self.assignments = {}
        self.imports = {}
        for node in self.tree.body:
            if isinstance(node, ast.Assign) and len(node.targets) == 1 and isinstance(node.targets[0], ast.Name):
                self.assignments.setdefault(node.targets[0].id, []).append((node.lineno, node.value))
            elif isinstance(node, ast.AnnAssign) and isinstance(node.target, ast.Name) and node.value is not None:
                self.assignments.setdefault(node.target.id, []).append((node.lineno, node.value))
            elif isinstance(node, ast.ImportFrom) and node.module and not node.level:
                for alias in node.names:
                    self.imports[alias.asname or alias.name] = (node.module, alias.name)


class RefusalGridScanner:
    def __init__(self, modules, composition):
        self.modules = modules
        self.composition = composition

    def imported_module(self, module, dotted):
        rel = pathlib.Path(*dotted.split(".")).with_suffix(".py")
        for candidate in (module.path.parent / rel, ROOT / rel):
            found = self.modules.get(candidate)
            if found is not None:
                return found
        return None

    def resolve(self, module, node, before):
        for _ in range(NAME_DEPTH):
            if not isinstance(node, ast.Name):
                break
            bound = [value for lineno, value in module.assignments.get(node.id, []) if lineno < before]
            if bound:
                node = bound[-1]
                continue
            target = module.imports.get(node.id)
            source = self.imported_module(module, target[0]) if target else None
            if source is None:
                break
            module, node, before = source, ast.Name(id=target[1], ctx=ast.Load()), float("inf")
        return module, node, before

    def string_list(self, module, node, before):
        module, node, before = self.resolve(module, node, before)
        if not isinstance(node, (ast.List, ast.Tuple)):
            return None
        items = []
        for elt in node.elts:
            _, elt, _ = self.resolve(module, elt, before)
            if not (isinstance(elt, ast.Constant) and isinstance(elt.value, str)):
                return None
            items.append(elt.value)
        return items

    def references_strategy(self, module, node, before):
        module, node, before = self.resolve(module, node, before)
        if isinstance(node, (ast.Dict, ast.Call)):
            return True
        if isinstance(node, ast.Constant) and isinstance(node.value, str):
            return any(tok in node.value for tok in REFERENCE_TOKENS)
        if isinstance(node, (ast.List, ast.Tuple, ast.JoinedStr, ast.BinOp)):
            return any(self.references_strategy(module, child, before) for child in ast.iter_child_nodes(node))
        return False

    def case_elements(self, module, case, argcount, before):
        module, case, before = self.resolve(module, case, before)
        if isinstance(case, ast.Call) and _call_name(case.func) == "param":
            values = list(case.args)
        elif argcount == 1:
            values = [case]
        elif isinstance(case, (ast.Tuple, ast.List)):
            values = list(case.elts)
        else:
            return module, None, before
        if argcount == 1 and isinstance(case, ast.Call) and len(values) != 1:
            return module, None, before
        return module, values, before

    def is_refusal_case(self, module, case, argcount, before):
        module, elements, before = self.case_elements(module, case, argcount, before)
        if not elements:
            return False
        expected = set()
        token_lists = []
        for elt in elements:
            if self.references_strategy(module, elt, before):
                return False
            _, value, _ = self.resolve(module, elt, before)
            if isinstance(value, ast.Constant) and isinstance(value.value, str):
                expected.update(c for c in REFUSAL_CLASSES if c in value.value)
                continue
            tokens = self.string_list(module, elt, before)
            if tokens is not None:
                token_lists.append(tokens)
        if len(expected) != 1:
            return False
        want = next(iter(expected))
        return any(refusal_class(self.composition, tokens) == want for tokens in token_lists)

    def refusal_cases(self, module, decorator):
        if not (isinstance(decorator, ast.Call) and _call_name(decorator.func) == "parametrize"):
            return 0
        args = list(decorator.args)
        keywords = {kw.arg: kw.value for kw in decorator.keywords if kw.arg}
        argnames = args[0] if args else keywords.get("argnames")
        argvalues = args[1] if len(args) > 1 else keywords.get("argvalues")
        if argnames is None or argvalues is None:
            return 0
        argcount = _argcount(argnames)
        if argcount is None:
            return 0
        before = decorator.lineno
        values_module, values, values_before = self.resolve(module, argvalues, before)
        if not isinstance(values, (ast.List, ast.Tuple)):
            return 0
        return sum(1 for case in values.elts if self.is_refusal_case(values_module, case, argcount, values_before))

    def scan(self, module):
        hits = []
        for owner, func in _test_functions(module.tree):
            if any(self.refusal_cases(module, dec) >= REFUSAL_GRID_MIN_CASES for dec in func.decorator_list):
                parts = [str(module.path.relative_to(ROOT))] + owner + [func.name]
                hits.append("::".join(parts))
        return hits


def _call_name(func):
    if isinstance(func, ast.Attribute):
        return func.attr
    if isinstance(func, ast.Name):
        return func.id
    return None


def _argcount(node):
    if isinstance(node, ast.Constant) and isinstance(node.value, str):
        names = [n for n in (part.strip() for part in node.value.split(",")) if n]
        return len(names) or None
    if isinstance(node, (ast.List, ast.Tuple)) and all(
        isinstance(e, ast.Constant) and isinstance(e.value, str) for e in node.elts
    ):
        return len(node.elts) or None
    return None


def _test_functions(tree):
    for node in tree.body:
        if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)) and node.name.startswith("test"):
            yield [], node
        elif isinstance(node, ast.ClassDef) and node.name.startswith("Test"):
            for item in node.body:
                if isinstance(item, (ast.FunctionDef, ast.AsyncFunctionDef)) and item.name.startswith("test"):
                    yield [node.name], item


def _read(path, errors):
    try:
        return path.read_text()
    except (OSError, UnicodeDecodeError) as exc:
        errors.append(f"{path.relative_to(ROOT)}: unreadable test source ({exc})")
        return None


def scan():
    hits = {rule: [] for rule in RULES}
    errors = []
    for path in sorted((ROOT / "scheduler").glob("*_test.go")):
        text = _read(path, errors)
        if text is None:
            continue
        for name, body in go_bodies(text):
            if go_wording_only(body):
                hits[WORDING_RULE].append(f"{path.relative_to(ROOT)}::{name}")
    modules = {}
    for root in PY_ROOTS:
        for path in sorted((ROOT / root).rglob("test_*.py")):
            text = _read(path, errors)
            if text is None:
                continue
            for name, body in py_bodies(text):
                if py_wording_only(body):
                    hits[WORDING_RULE].append(f"{path.relative_to(ROOT)}::{name}")
            try:
                modules[path] = SourceModule(path, text)
            except (SyntaxError, ValueError) as exc:
                errors.append(f"{path.relative_to(ROOT)}: invalid test source ({exc})")
    if errors:
        raise BudgetError("test budget scan failed:\n" + "\n".join(f"  {e}" for e in errors))
    scanner = RefusalGridScanner(modules, load_composition())
    for path in sorted(modules):
        hits[REFUSAL_GRID_RULE].extend(scanner.scan(modules[path]))
    return {rule: sorted(set(ids)) for rule, ids in hits.items()}


def compare(hits, baseline):
    hit_set = set(hits)
    base_set = set(baseline)
    return sorted(hit_set - base_set), sorted(base_set - hit_set)


def load_baseline():
    if not BASELINE.exists():
        return {rule: [] for rule in RULES}
    name = BASELINE.relative_to(ROOT)
    try:
        data = json.loads(BASELINE.read_text())
    except (OSError, UnicodeDecodeError, ValueError) as exc:
        raise BudgetError(f"invalid baseline {name}: {exc}") from exc
    if not isinstance(data, dict):
        raise BudgetError(f"invalid baseline {name}: expected an object with keys {', '.join(RULES)}")
    unknown = sorted(set(data) - set(RULES))
    missing = [rule for rule in RULES if rule not in data]
    if unknown or missing:
        raise BudgetError(f"invalid baseline {name}: unknown keys {unknown}, missing keys {missing}")
    out = {}
    for rule in RULES:
        entries = data[rule]
        if not isinstance(entries, list) or not all(isinstance(e, str) and e for e in entries):
            raise BudgetError(f"invalid baseline {name}: {rule} must list test identifiers")
        if len(set(entries)) != len(entries):
            raise BudgetError(f"invalid baseline {name}: {rule} repeats an identifier")
        out[rule] = entries
    return out


def report(rule, new, stale):
    rc = 0
    if rule == WORDING_RULE:
        if new:
            print("test budget exceeded: new tests assert only message wording", file=sys.stderr)
            for ident in new:
                print(f"  new: {ident}", file=sys.stderr)
            print("add an identifier to scripts/test_budget_baseline.json only when the wording drives an operator decision", file=sys.stderr)
            rc = 1
        if stale:
            print("baseline lists wording-only tests that no longer exist; run scripts/check_test_budget.py --write-baseline to tighten it", file=sys.stderr)
            for ident in stale:
                print(f"  stale: {ident}", file=sys.stderr)
            rc = 1
        return rc
    if new:
        print(f"test budget exceeded ({rule}): new tests repeat the no-edge refusal matrix", file=sys.stderr)
        for ident in new:
            print(f"  new: {ident}", file=sys.stderr)
        print(f"a no-edge strategy keeps one missing-mode refusal and reuses one admitted --mode=paper case; see {STRATEGY_SECTION}", file=sys.stderr)
        rc = 1
    if stale:
        print(f"baseline lists refusal-grid tests that no longer exist ({rule})", file=sys.stderr)
        for ident in stale:
            print(f"  stale: {ident}", file=sys.stderr)
        print(f"remove them from scripts/test_budget_baseline.json; see {STRATEGY_SECTION}", file=sys.stderr)
        rc = 1
    return rc


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--write-baseline", action="store_true")
    ap.add_argument("--list", action="store_true")
    args = ap.parse_args()
    try:
        hits = scan()
        if args.list:
            for rule in RULES:
                for ident in hits[rule]:
                    print(f"{rule} {ident}")
        if args.write_baseline:
            BASELINE.write_text(json.dumps({rule: hits[rule] for rule in RULES}, indent=2) + "\n")
            print(f"baseline written: {len(hits[WORDING_RULE])} wording-only tests, "
                  f"{len(hits[REFUSAL_GRID_RULE])} refusal-grid tests")
            return 0
        baseline = load_baseline()
    except BudgetError as exc:
        print(str(exc), file=sys.stderr)
        return 1
    rc = 0
    labels = {WORDING_RULE: "wording-only tests", REFUSAL_GRID_RULE: "refusal-grid tests"}
    for rule in RULES:
        print(f"{labels[rule]}: {len(hits[rule])} (baseline {len(baseline[rule])})")
    for rule in RULES:
        new, stale = compare(hits[rule], baseline[rule])
        rc |= report(rule, new, stale)
    return rc


if __name__ == "__main__":
    sys.exit(main())

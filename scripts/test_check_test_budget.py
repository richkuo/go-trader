import importlib.util
import json
import pathlib
import subprocess
import sys

_SPEC = importlib.util.spec_from_file_location(
    "check_test_budget_under_test", pathlib.Path(__file__).with_name("check_test_budget.py")
)
budget = importlib.util.module_from_spec(_SPEC)
_SPEC.loader.exec_module(budget)


def test_go_wording_only(subtests):
    for body, expected in [
        (
            'func TestA(t *testing.T) {\n\tmsg := f()\n\tif !strings.Contains(msg, "x") {\n\t\tt.Fatal("no")\n\t}\n}\n',
            True,
        ),
        (
            'func TestB(t *testing.T) {\n\tgot := f()\n\tif got != 3 {\n\t\tt.Fatalf("got %d", got)\n\t}\n\tif !strings.Contains(msg, "x") {\n\t\tt.Fatal("no")\n\t}\n}\n',
            False,
        ),
        ('func TestC(t *testing.T) {\n\tf()\n}\n', False),
    ]:
        with subtests.test(body=body):
            assert budget.go_wording_only(body) is expected


def test_py_wording_only(subtests):
    for body, expected in [
        ('def test_a():\n    assert "x" in f()\n', True),
        ('def test_b():\n    assert f() == 3\n    assert "x" in f()\n', False),
        ("def test_c():\n    f()\n", False),
    ]:
        with subtests.test(body=body):
            assert budget.py_wording_only(body) is expected


def test_compare_flags_new_and_stale_identifiers(subtests):
    for hits, baseline, expected_new, expected_stale in [
        (["a::t1", "b::t2"], ["a::t1", "b::t2"], [], []),
        (["a::t1", "c::t3"], ["a::t1", "b::t2"], ["c::t3"], ["b::t2"]),
        (["a::t1"], ["a::t1", "b::t2"], [], ["b::t2"]),
        (["a::t1", "a::t9"], ["a::t1"], ["a::t9"], []),
    ]:
        with subtests.test(hits=hits, baseline=baseline):
            assert budget.compare(hits, baseline) == (expected_new, expected_stale)


SCRIPT = pathlib.Path(__file__).with_name("check_test_budget.py")
COMPOSITION = pathlib.Path(__file__).resolve().parent.parent / "shared_tools" / "strategy_composition.py"
CANONICAL = "shared_scripts/test_no_edge_admission.py"
CANONICAL_IDS = [
    f"{CANONICAL}::test_every_other_check_script_wires_each_refusal_class",
    f"{CANONICAL}::test_hyperliquid_check_refuses_every_no_edge_input_outside_explicit_paper",
]
SECTION = "SKILL.md section Add Or Change Strategies"
WORDING_SOURCE = ["def test_message():", '    assert "x" in message()']
CONTRACT_SOURCES = {
    "backtest/tests/test_strategy_backtest_contract.py": [
        "REPLACED_PER_STRATEGY_TESTS = ()",
        "def test_backtest_contract(case):",
        "    assert case",
    ],
    "shared_scripts/test_strategy_check_contract.py": [
        'REPLACED_PER_STRATEGY_TESTS = ("test_retired_check",)',
        "def test_check_contract(case):",
        "    assert case",
    ],
}

GRID_CASES = [
    '    (["--mode=live"], "explicit live mode"),',
    '    (["--mode", "live"], "explicit live mode"),',
    '    ([], "missing mode"),',
    '    (["--mode=paper", "--mode=paper"], "invalid mode input"),',
    '    (["--mode=paper", "--mode", "live"], "invalid mode input"),',
    '    (["--mode="], "invalid mode input"),',
    '    (["--mode=Paper"], "invalid mode input"),',
    '    (["--mode=live", "--allow-no-edge=1"], "invalid acknowledgement"),',
    '    (["--mode=live", "--allow-no-edge=true"], "invalid acknowledgement"),',
    '    (["--mode=paper", "--allow-no-edge", "--allow-no-edge"], "invalid acknowledgement"),',
]
CANONICAL_SOURCE = [
    "import pytest",
    "",
    "REFUSED_INPUTS = [",
    *GRID_CASES,
    "]",
    "REFUSAL_CLASS_INPUTS = [",
    '    (["--mode=live"], "explicit live mode"),',
    '    ([], "missing mode"),',
    '    (["--mode=Paper"], "invalid mode input"),',
    '    (["--mode=live", "--allow-no-edge=1"], "invalid acknowledgement"),',
    "]",
    'SCRIPTS = [("check_okx.py", ["rsi"]), ("check_topstep.py", ["rsi"])]',
    "",
    "",
    '@pytest.mark.parametrize("extra,reason", REFUSED_INPUTS)',
    "def test_hyperliquid_check_refuses_every_no_edge_input_outside_explicit_paper(extra, reason):",
    "    assert run(extra) == reason",
    "",
    "",
    '@pytest.mark.parametrize("script,positional", SCRIPTS)',
    '@pytest.mark.parametrize("extra,reason", REFUSAL_CLASS_INPUTS)',
    "def test_every_other_check_script_wires_each_refusal_class(script, positional, extra, reason):",
    "    assert run(extra) == reason",
    "",
    "",
    '@pytest.mark.parametrize("extra", [["--mode=paper"], ["--mode", "paper"], ["--mode=live", "--allow-no-edge"]])',
    "def test_paper_or_acknowledged_evaluation_runs(extra):",
    "    assert run(extra) is None",
]
PLANTED = {
    "literal": [
        "import pytest",
        "",
        "",
        '@pytest.mark.parametrize("extra,reason", [',
        *GRID_CASES,
        "])",
        "def test_new_strategy_is_refused(extra, reason):",
        "    assert run(extra) == reason",
    ],
    "named": [
        "import pytest",
        "",
        "NEW_INPUTS = [",
        *GRID_CASES,
        "]",
        "ALIAS = NEW_INPUTS",
        "",
        "",
        '@pytest.mark.parametrize("extra,reason", ALIAS)',
        "def test_new_strategy_is_refused(extra, reason):",
        "    assert run(extra) == reason",
    ],
    "imported": [
        "import pytest",
        "from test_no_edge_admission import REFUSED_INPUTS",
        "",
        "",
        '@pytest.mark.parametrize("extra,reason", REFUSED_INPUTS)',
        "def test_new_strategy_is_refused(extra, reason):",
        "    assert run(extra) == reason",
    ],
    "pytest_param_with_missing_mode": [
        "import pytest",
        "",
        "",
        "class TestNewStrategy:",
        '    @pytest.mark.parametrize(argnames=["extra", "reason"], argvalues=[',
        '        pytest.param([], "missing mode", id="missing"),',
        '        pytest.param(["--mode=live"], "explicit live mode", id="live"),',
        '        pytest.param(["--mode=Paper"], "invalid mode input", id="invalid"),',
        "    ])",
        "    def test_new_strategy_is_refused(self, extra, reason):",
        "        assert run(extra) == reason",
    ],
    "close_fallback_copy": [
        "import json",
        "import pytest",
        "",
        "SHORT = [",
        '    (["--mode=live"], "explicit live mode"),',
        '    ([], "missing mode (treated as live)"),',
        '    (["--mode=live", "--allow-no-edge=true"], "invalid acknowledgement refused"),',
        "]",
        "",
        "",
        '@pytest.mark.parametrize("extra,reason", SHORT)',
        "def test_new_strategy_is_refused(extra, reason):",
        '    refs = json.dumps({"open": {"name": "breakout"}, "closes": [{"name": "new"}]})',
        '    assert run(["--strategy-refs", refs] + extra) == reason',
    ],
    "subtest_loop": [
        "from test_no_edge_admission import REFUSAL_CLASS_INPUTS",
        "",
        "",
        "def test_new_strategy_is_refused(subtests):",
        "    for extra, reason in REFUSAL_CLASS_INPUTS:",
        '        with subtests.test(extra=" ".join(extra)):',
        "            assert run(extra) == reason",
    ],
    "comprehension_over_imported": [
        "from test_no_edge_admission import REFUSED_INPUTS",
        "",
        "",
        "def test_new_strategy_is_refused(subtests):",
        '    cases = [("new", extra, reason) for extra, reason in REFUSED_INPUTS]',
        "    for name, extra, reason in cases:",
        "        with subtests.test(name=name):",
        "            assert run(extra) == reason",
    ],
}
PERMITTED = {
    "admitted_and_acknowledged": [
        "import pytest",
        "",
        "",
        '@pytest.mark.parametrize("extra,outcome", [',
        '    (["--mode=paper"], "admitted"),',
        '    (["--mode", "paper"], "admitted"),',
        '    (["--mode=live", "--allow-no-edge"], "acknowledged live"),',
        '    (["--allow-no-edge"], "acknowledged missing mode"),',
        "])",
        "def test_new_strategy_is_refused_outside_explicit_paper(extra, outcome):",
        "    assert run(extra) == outcome",
    ],
    "mixed_effective_references": [
        "import pytest",
        "",
        'NAME = "new"',
        "",
        "",
        '@pytest.mark.parametrize("refs,mode,needle", [',
        '    ({"open": {"name": NAME}}, ["--mode=live"], "explicit live mode"),',
        '    ({"open": {"name": NAME}}, [], "missing mode"),',
        '    ({"open": {"name": "breakout"}, "closes": [{"name": NAME}]}, ["--mode=live"], "explicit live mode"),',
        '    ({"open": {"name": "breakout"}, "closes": [{"name": NAME}]}, [], "missing mode"),',
        '    ({"open": {"name": "breakout"}, "closes": [{"name": NAME}]}, ["--mode=Paper"], "invalid mode input"),',
        "])",
        "def test_effective_open_and_close_fallback_are_gated(refs, mode, needle):",
        "    assert run(refs, mode) == needle",
    ],
    "unrelated_backtest_modes": [
        "import pytest",
        "",
        "MODES = [",
        '    (["--mode", "approximate"], "approximate comparison"),',
        '    (["--mode=strict"], "strict comparison"),',
        '    (["--mode=walk_forward"], "walk forward"),',
        '    ([], "strict comparison"),',
        "]",
        "",
        "",
        '@pytest.mark.parametrize("argv,label", MODES)',
        "def test_backtest_modes(argv, label):",
        "    assert run(argv) == label",
    ],
    "mismatched_class_and_arguments": [
        "import pytest",
        "",
        "",
        '@pytest.mark.parametrize("extra,reason", [',
        '    (["--mode=paper"], "explicit live mode"),',
        '    (["--mode=live", "--allow-no-edge"], "missing mode"),',
        '    (["--mode=paper"], "invalid acknowledgement"),',
        "])",
        "def test_messages(extra, reason):",
        "    assert run(extra) != reason",
    ],
    "two_case_registration": [
        "import pytest",
        "",
        "",
        '@pytest.mark.parametrize("extra,reason", [(["--mode=live"], "explicit live mode"), ([], "missing mode")])',
        "def test_close_fallback_is_refused(extra, reason):",
        "    assert run(extra) == reason",
    ],
    "non_refusal_loop": [
        "def test_new_strategy_runs_when_admitted(subtests):",
        '    for extra in (["--mode=paper"], ["--mode=live", "--allow-no-edge"]):',
        '        with subtests.test(extra=" ".join(extra)):',
        "            assert run(extra) is None",
        '    for extra, outcome in [(["--mode=paper"], "admitted"), (["--mode=live", "--allow-no-edge"], "acked")]:',
        "        with subtests.test(outcome=outcome):",
        "            assert run(extra) == outcome",
    ],
}


def _write(root, rel, content):
    path = root / rel
    path.parent.mkdir(parents=True, exist_ok=True)
    if isinstance(content, bytes):
        path.write_bytes(content)
    elif isinstance(content, list):
        path.write_text("\n".join(content) + "\n")
    else:
        path.write_text(content)


def _tree(tmp_path, files=None, baseline=None):
    _write(tmp_path, "scripts/check_test_budget.py", SCRIPT.read_text())
    _write(tmp_path, "shared_tools/strategy_composition.py", COMPOSITION.read_text())
    _write(tmp_path, CANONICAL, CANONICAL_SOURCE)
    for rel, content in CONTRACT_SOURCES.items():
        _write(tmp_path, rel, content)
    for rel, content in (files or {}).items():
        _write(tmp_path, rel, content)
    if baseline is None:
        baseline = {"wording_only_tests": [], "refusal_grid_tests": list(CANONICAL_IDS), "contract_copy_tests": []}
    if baseline is not False:
        _write(tmp_path, "scripts/test_budget_baseline.json",
               baseline if isinstance(baseline, str) else json.dumps(baseline))
    return tmp_path


def _cli(root, *flags):
    return subprocess.run([sys.executable, str(root / "scripts" / "check_test_budget.py"), *flags],
                          capture_output=True, text=True, cwd=root, timeout=120)


def _listed(proc, rule):
    return [line.split(" ", 1)[1] for line in proc.stdout.splitlines() if line.startswith(rule + " ")]


def test_cli_passes_with_only_the_canonical_and_wiring_matrices(tmp_path):
    proc = _cli(_tree(tmp_path))
    assert proc.returncode == 0, proc.stderr
    assert "refusal-grid tests: 2 (baseline 2)" in proc.stdout.splitlines()
    assert proc.stderr == ""


def test_cli_reports_a_detected_reviewed_matrix_missing_from_the_baseline_as_new(tmp_path, subtests):
    for index, dropped in enumerate(CANONICAL_IDS):
        with subtests.test(dropped=dropped):
            kept = [ident for ident in CANONICAL_IDS if ident != dropped]
            proc = _cli(_tree(tmp_path / f"case{index}", baseline={"wording_only_tests": [], "refusal_grid_tests": kept,
                                                                  "contract_copy_tests": []}))
            assert proc.returncode == 1
            assert f"  new: {dropped}" in proc.stderr.splitlines()
            assert "refusal_grid_tests" in proc.stderr and SECTION in proc.stderr


def test_cli_reports_a_nonexistent_baseline_identifier_as_stale(tmp_path):
    ghost = "shared_scripts/test_gone.py::test_gone_is_refused"
    proc = _cli(_tree(tmp_path, baseline={"wording_only_tests": [],
                                          "refusal_grid_tests": CANONICAL_IDS + [ghost],
                                          "contract_copy_tests": []}))
    assert proc.returncode == 1
    assert f"  stale: {ghost}" in proc.stderr.splitlines()
    assert "refusal_grid_tests" in proc.stderr and SECTION in proc.stderr


def test_cli_rejects_a_planted_refusal_matrix_copy_and_passes_once_it_is_removed(tmp_path, subtests):
    for kind in sorted(PLANTED):
        with subtests.test(kind=kind):
            rel = "shared_scripts/test_new_strategy_checks.py"
            root = _tree(tmp_path / kind, {rel: PLANTED[kind]})
            proc = _cli(root)
            assert proc.returncode == 1
            owner = "TestNewStrategy::" if kind == "pytest_param_with_missing_mode" else ""
            assert proc.stderr.splitlines().count(f"  new: {rel}::{owner}test_new_strategy_is_refused") == 1
            assert "test budget exceeded (refusal_grid_tests)" in proc.stderr and SECTION in proc.stderr
            (root / rel).unlink()
            clean = _cli(root)
            assert clean.returncode == 0, clean.stderr


def test_cli_permits_admitted_mixed_reference_and_unrelated_mode_matrices(tmp_path, subtests):
    for kind in sorted(PERMITTED):
        with subtests.test(kind=kind):
            proc = _cli(_tree(tmp_path / kind, {"backtest/tests/test_permitted.py": PERMITTED[kind]}), "--list")
            assert proc.returncode == 0, proc.stderr
            assert _listed(proc, "refusal_grid_tests") == CANONICAL_IDS


def test_cli_fails_on_invalid_or_undecodable_test_source_without_writing_a_baseline(tmp_path, subtests):
    for content, needle in [
        (["def test_broken(:", "    pass"], "invalid test source"),
        (b"def test_bytes():\n    assert '\xff' == 1\n", "unreadable test source"),
    ]:
        with subtests.test(needle=needle):
            rel = "platforms/test_broken.py"
            root = _tree(tmp_path / needle.replace(" ", "_"), {rel: content})
            proc = _cli(root)
            assert proc.returncode == 1
            assert f"{rel}: {needle}" in proc.stderr
            before = (root / "scripts" / "test_budget_baseline.json").read_text()
            write = _cli(root, "--write-baseline")
            assert write.returncode == 1
            assert (root / "scripts" / "test_budget_baseline.json").read_text() == before


def test_cli_fails_on_an_unreadable_test_source(tmp_path):
    root = _tree(tmp_path)
    (root / "shared_tools" / "test_missing_target.py").symlink_to(root / "shared_tools" / "absent.py")
    proc = _cli(root)
    assert proc.returncode == 1
    assert "shared_tools/test_missing_target.py: unreadable test source" in proc.stderr


def test_cli_fails_on_invalid_baseline_data(tmp_path, subtests):
    for index, baseline in enumerate([
        "{not json",
        json.dumps(CANONICAL_IDS),
        json.dumps({"wording_only_tests": []}),
        json.dumps({"wording_only_tests": [], "refusal_grid_tests": CANONICAL_IDS}),
        json.dumps({"wording_only_tests": [], "refusal_grid_tests": "all", "contract_copy_tests": []}),
        json.dumps({"wording_only_tests": [], "refusal_grid_tests": [1], "contract_copy_tests": []}),
        json.dumps({"wording_only_tests": [], "refusal_grid_tests": CANONICAL_IDS + CANONICAL_IDS[:1],
                    "contract_copy_tests": []}),
        json.dumps({"wording_only_tests": [], "refusal_grid_tests": CANONICAL_IDS, "contract_copy_tests": "all"}),
        json.dumps({"wording_only_tests": [], "refusal_grid_tests": CANONICAL_IDS, "contract_copy_tests": [],
                    "extra": []}),
    ]):
        with subtests.test(baseline=baseline):
            proc = _cli(_tree(tmp_path / f"case{index}", baseline=baseline))
            assert proc.returncode == 1
            assert "invalid baseline scripts/test_budget_baseline.json" in proc.stderr


def test_list_prints_every_rule_in_order_and_still_validates(tmp_path):
    files = {
        "shared_tools/test_wording.py": WORDING_SOURCE,
        "shared_scripts/test_new_strategy_checks.py": PLANTED["literal"] + ["", "def test_check_contract():", "    pass"],
    }
    proc = _cli(_tree(tmp_path, files), "--list")
    assert proc.returncode == 1
    rules = ("wording_only_tests ", "refusal_grid_tests ", "contract_copy_tests ")
    assert [line for line in proc.stdout.splitlines() if line.startswith(rules)] == [
        "wording_only_tests shared_tools/test_wording.py::test_message",
        "refusal_grid_tests shared_scripts/test_new_strategy_checks.py::test_new_strategy_is_refused",
        "refusal_grid_tests " + CANONICAL_IDS[0],
        "refusal_grid_tests " + CANONICAL_IDS[1],
        "contract_copy_tests shared_scripts/test_new_strategy_checks.py::test_check_contract",
    ]
    assert "  new: shared_tools/test_wording.py::test_message" in proc.stderr.splitlines()


def test_write_baseline_keeps_every_key_sorted_and_the_next_run_passes(tmp_path):
    root = _tree(tmp_path, {"shared_tools/test_wording.py": WORDING_SOURCE}, baseline=False)
    proc = _cli(root, "--write-baseline")
    assert proc.returncode == 0, proc.stderr
    data = json.loads((root / "scripts" / "test_budget_baseline.json").read_text())
    assert data == {"wording_only_tests": ["shared_tools/test_wording.py::test_message"],
                    "refusal_grid_tests": CANONICAL_IDS, "contract_copy_tests": []}
    assert _cli(root).returncode == 0


def test_wording_only_rule_still_reports_new_and_stale_entries(tmp_path):
    stale = "shared_tools/test_wording.py::test_removed"
    root = _tree(tmp_path, {"shared_tools/test_wording.py": WORDING_SOURCE},
                 baseline={"wording_only_tests": [stale], "refusal_grid_tests": CANONICAL_IDS,
                           "contract_copy_tests": []})
    proc = _cli(root)
    assert proc.returncode == 1
    lines = proc.stderr.splitlines()
    assert "test budget exceeded: new tests assert only message wording" in lines
    assert "  new: shared_tools/test_wording.py::test_message" in lines
    assert f"  stale: {stale}" in lines


def test_cli_rejects_a_per_strategy_copy_of_a_contract_test_and_passes_once_it_is_removed(tmp_path, subtests):
    for index, (rel, owner) in enumerate([
        ("shared_scripts/test_new_strategy_checks.py", ""),
        ("backtest/tests/test_new_strategy_integration.py", "TestNewStrategy::"),
    ]):
        with subtests.test(rel=rel):
            if owner:
                source = ["class TestNewStrategy:", "    def test_backtest_contract(self):", "        assert True"]
                name = "test_backtest_contract"
            else:
                source = ["def test_check_contract():", "    assert True"]
                name = "test_check_contract"
            root = _tree(tmp_path / f"case{index}", {rel: source})
            proc = _cli(root)
            assert proc.returncode == 1
            lines = proc.stderr.splitlines()
            assert "test budget exceeded (contract_copy_tests): new tests repeat a strategy contract test" in lines
            assert f"  new: {rel}::{owner}{name}" in lines
            assert SECTION in proc.stderr
            (root / rel).unlink()
            clean = _cli(root)
            assert clean.returncode == 0, clean.stderr
            assert "contract-copy tests: 0 (baseline 0)" in clean.stdout.splitlines()


def test_cli_reports_a_removed_contract_copy_left_in_the_baseline_as_stale(tmp_path):
    ghost = "shared_scripts/test_gone_checks.py::test_check_contract"
    proc = _cli(_tree(tmp_path, baseline={"wording_only_tests": [], "refusal_grid_tests": CANONICAL_IDS,
                                          "contract_copy_tests": [ghost]}))
    assert proc.returncode == 1
    assert f"  stale: {ghost}" in proc.stderr.splitlines()


def test_cli_fails_when_a_contract_suite_is_missing(tmp_path, subtests):
    for index, missing in enumerate(sorted(CONTRACT_SOURCES)):
        with subtests.test(missing=missing):
            root = _tree(tmp_path / f"case{index}")
            (root / missing).unlink()
            proc = _cli(root)
            assert proc.returncode == 1
            assert f"{missing}: strategy contract suite missing" in proc.stderr


def test_cli_rejects_a_new_test_that_reuses_a_replaced_per_strategy_name(tmp_path):
    rel = "shared_scripts/test_new_strategy_checks.py"
    proc = _cli(_tree(tmp_path, {rel: ["def test_retired_check():", "    assert True"]}))
    assert proc.returncode == 1
    assert f"  new: {rel}::test_retired_check" in proc.stderr.splitlines()


def test_cli_fails_when_a_contract_suite_drops_its_replaced_name_list(tmp_path, subtests):
    for label, rel, first in [
        ("check-suite-list-deleted", "shared_scripts/test_strategy_check_contract.py", "import pytest"),
        ("backtest-suite-list-renamed", "backtest/tests/test_strategy_backtest_contract.py",
         'REPLACED_TESTS = ("test_retired_backtest",)'),
    ]:
        with subtests.test(label=label):
            proc = _cli(_tree(tmp_path / label, {rel: [first, "def test_contract(case):", "    assert case"]}))
            assert proc.returncode == 1
            assert f"{rel}: REPLACED_PER_STRATEGY_TESTS missing" in proc.stderr


def test_cli_fails_on_a_malformed_replaced_name_list(tmp_path, subtests):
    for index, value in enumerate(['"test_retired_check"', "(1,)", '("helper",)']):
        with subtests.test(value=value):
            rel = "shared_scripts/test_strategy_check_contract.py"
            proc = _cli(_tree(tmp_path / f"case{index}", {rel: [f"REPLACED_PER_STRATEGY_TESTS = {value}",
                                                                "def test_check_contract(case):", "    assert case"]}))
            assert proc.returncode == 1
            assert f"{rel}: REPLACED_PER_STRATEGY_TESTS must be a tuple or list of test names" in proc.stderr

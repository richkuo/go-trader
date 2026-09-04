import importlib.util
import pathlib

import pytest

_SPEC = importlib.util.spec_from_file_location(
    "check_test_budget_under_test", pathlib.Path(__file__).with_name("check_test_budget.py")
)
budget = importlib.util.module_from_spec(_SPEC)
_SPEC.loader.exec_module(budget)


@pytest.mark.parametrize(
    "body,expected",
    [
        (
            'func TestA(t *testing.T) {\n\tmsg := f()\n\tif !strings.Contains(msg, "x") {\n\t\tt.Fatal("no")\n\t}\n}\n',
            True,
        ),
        (
            'func TestB(t *testing.T) {\n\tgot := f()\n\tif got != 3 {\n\t\tt.Fatalf("got %d", got)\n\t}\n\tif !strings.Contains(msg, "x") {\n\t\tt.Fatal("no")\n\t}\n}\n',
            False,
        ),
        ('func TestC(t *testing.T) {\n\tf()\n}\n', False),
    ],
)
def test_go_wording_only(body, expected):
    assert budget.go_wording_only(body) is expected


@pytest.mark.parametrize(
    "body,expected",
    [
        ('def test_a():\n    assert "x" in f()\n', True),
        ('def test_b():\n    assert f() == 3\n    assert "x" in f()\n', False),
        ("def test_c():\n    f()\n", False),
    ],
)
def test_py_wording_only(body, expected):
    assert budget.py_wording_only(body) is expected

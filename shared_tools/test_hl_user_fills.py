import importlib.util
import pathlib
from unittest.mock import MagicMock

import pytest

_spec = importlib.util.spec_from_file_location(
    "hl_user_fills", pathlib.Path(__file__).parent / "hl_user_fills.py"
)
_mod = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(_mod)

_finite_number = _mod._finite_number


FINITE_NUMBER_CASES = [
    ("3.14", 3.14),
    (42, 42.0),
    (1.5, 1.5),
    (True, None),
    (False, None),
    ("abc", None),
    (None, None),
    (MagicMock(), None),
    (float("inf"), None),
    ("-inf", None),
    (float("nan"), None),
]


def test_finite_number(subtests):
    for value, expected in FINITE_NUMBER_CASES:
        with subtests.test(value=value, expected=expected):
            got = _finite_number(value)
            if expected is None:
                assert got is None
            else:
                assert got == pytest.approx(expected)

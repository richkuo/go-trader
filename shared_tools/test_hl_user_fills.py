
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
apply_user_fills_lookup = _mod.apply_user_fills_lookup


@pytest.mark.parametrize("value,expected", [
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
])
def test_finite_number(value, expected):
    got = _finite_number(value)
    if expected is None:
        assert got is None
    else:
        assert got == pytest.approx(expected)


class TestApplyUserFillsLookup:
    @pytest.mark.parametrize("lookup,expected_fee,expected_closed_pnl", [
        ({"fee": "0.42", "closed_pnl": "3.14"}, 0.42, 3.14),
        ({"fee": 0.5}, 0.5, None),
        ({"fee": 2}, 2.0, None),
    ])
    def test_valid_lookup_applied(self, lookup, expected_fee, expected_closed_pnl):
        fill = {}
        result = apply_user_fills_lookup(fill, lookup)
        assert result is True
        assert fill["fee"] == pytest.approx(expected_fee)
        if expected_closed_pnl is None:
            assert "closed_pnl" not in fill
        else:
            assert fill["closed_pnl"] == pytest.approx(expected_closed_pnl)

    @pytest.mark.parametrize("lookup", [
        MagicMock(),
        {"fee": MagicMock()},
        {"fee": True},
        {},
        None,
    ])
    def test_malformed_lookup_rejected(self, lookup):
        fill = {}
        result = apply_user_fills_lookup(fill, lookup)
        assert result is False
        assert "fee" not in fill

    def test_malformed_closed_pnl_warns_and_keeps_fee(self, capsys):
        fill = {}
        result = apply_user_fills_lookup(fill, {"fee": "1.0", "closed_pnl": MagicMock()})
        assert result is True
        assert fill["fee"] == pytest.approx(1.0)
        assert "closed_pnl" not in fill
        captured = capsys.readouterr()
        assert "[WARN]" in captured.err
        assert "closed_pnl" in captured.err

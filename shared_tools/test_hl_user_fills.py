"""Tests for shared_tools/hl_user_fills.py — shape/numeric guard helpers."""

import importlib.util
import pathlib
import sys
from io import StringIO
from unittest.mock import MagicMock

import pytest

_spec = importlib.util.spec_from_file_location(
    "hl_user_fills", pathlib.Path(__file__).parent / "hl_user_fills.py"
)
_mod = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(_mod)

_finite_number = _mod._finite_number
apply_user_fills_lookup = _mod.apply_user_fills_lookup


class TestFiniteNumber:
    def test_float_string(self):
        assert _finite_number("3.14") == pytest.approx(3.14)

    def test_int(self):
        assert _finite_number(42) == pytest.approx(42.0)

    def test_float(self):
        assert _finite_number(1.5) == pytest.approx(1.5)

    def test_bool_rejected(self):
        assert _finite_number(True) is None
        assert _finite_number(False) is None

    def test_non_numeric_string(self):
        assert _finite_number("abc") is None

    def test_none(self):
        assert _finite_number(None) is None

    def test_mock_rejected(self):
        assert _finite_number(MagicMock()) is None

    def test_inf_rejected(self):
        assert _finite_number(float("inf")) is None
        assert _finite_number("-inf") is None

    def test_nan_rejected(self):
        assert _finite_number(float("nan")) is None


class TestApplyUserFillsLookup:
    def test_valid_fee_and_closed_pnl(self):
        fill = {}
        result = apply_user_fills_lookup(fill, {"fee": "0.42", "closed_pnl": "3.14"})
        assert result is True
        assert fill["fee"] == pytest.approx(0.42)
        assert fill["closed_pnl"] == pytest.approx(3.14)

    def test_valid_fee_no_closed_pnl_key(self):
        fill = {}
        result = apply_user_fills_lookup(fill, {"fee": 0.5})
        assert result is True
        assert fill["fee"] == pytest.approx(0.5)
        assert "closed_pnl" not in fill

    def test_truthy_non_mapping_rejected(self):
        fill = {}
        result = apply_user_fills_lookup(fill, MagicMock())
        assert result is False
        assert "fee" not in fill

    def test_malformed_fee_rejected(self):
        fill = {}
        result = apply_user_fills_lookup(fill, {"fee": MagicMock()})
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

    def test_bool_fee_rejected(self):
        fill = {}
        result = apply_user_fills_lookup(fill, {"fee": True})
        assert result is False
        assert "fee" not in fill

    def test_empty_mapping_rejected(self):
        fill = {}
        result = apply_user_fills_lookup(fill, {})
        assert result is False
        assert "fee" not in fill

    def test_none_lookup_rejected(self):
        fill = {}
        result = apply_user_fills_lookup(fill, None)
        assert result is False
        assert "fee" not in fill

    def test_numeric_int_fee(self):
        fill = {}
        result = apply_user_fills_lookup(fill, {"fee": 2})
        assert result is True
        assert fill["fee"] == pytest.approx(2.0)

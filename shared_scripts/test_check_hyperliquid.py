
import sys
import os
import json
import math
import contextlib
import importlib.util
from unittest.mock import MagicMock, call, patch
from io import StringIO

import pytest


_UNSET = object()
_ABSENT = object()
_TRUTHY = object()
_PRESENT = object()
_FALSY = object()


class _Has:
    def __init__(self, text):
        self.text = text


class _Len:
    def __init__(self, n):
        self.n = n


class _FirstArgSeen:
    def __init__(self, value):
        self.value = value


def _get(out, path):
    value = out
    for step in path:
        value = value[step]
    return value


def _assert_out(out, want):
    for key, value in want.items():
        path = key if isinstance(key, tuple) else (key,)
        if value is _ABSENT:
            assert key not in out, key
        elif value is _PRESENT:
            assert key in out, key
        elif value is _TRUTHY:
            assert _get(out, path), key
        elif value is _FALSY:
            assert not out.get(key), key
        elif isinstance(value, _Has):
            assert value.text in _get(out, path), key
        elif isinstance(value, _Len):
            assert len(out.get(key) or []) == value.n, key
        elif value is True or value is False or value is None:
            got = out.get(key) if len(path) == 1 else _get(out, path)
            assert got is value, key
        else:
            assert _get(out, path) == value, key


def _assert_calls(adapter, want):
    for name, value in want.items():
        method = getattr(adapter, name)
        if value is None:
            method.assert_not_called()
        elif isinstance(value, str):
            {"once": method.assert_called_once, "called": method.assert_called}[value]()
        elif isinstance(value, _FirstArgSeen):
            assert value.value in [c.args[0] for c in method.call_args_list], name
        elif isinstance(value, int):
            assert method.call_count == value, name
        else:
            method.assert_called_once_with(*value.args, **value.kwargs)


@contextlib.contextmanager
def _case(*label):
    try:
        yield
    except (Exception, pytest.fail.Exception) as exc:
        raise AssertionError(f"case {label!r} failed: {exc}") from exc


def _load_check_module():
    script_path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "check_hyperliquid.py")
    spec = importlib.util.spec_from_file_location("check_hyperliquid", script_path)
    mod = importlib.util.module_from_spec(spec)
    return mod, spec


class TestFillExtraction:

    def _run_execute_with_mock_response(self, sdk_response, lookup_result=_UNSET, **execute_kwargs):
        mod, spec = _load_check_module()
        spec.loader.exec_module(mod)

        mock_adapter_cls = MagicMock()
        mock_adapter = MagicMock()
        mock_adapter_cls.return_value = mock_adapter
        mock_adapter.market_open.return_value = sdk_response
        mock_adapter.cancel_trigger_order.return_value = {
            "status": "ok",
            "response": {"data": {"statuses": [{}]}},
        }
        if lookup_result is not _UNSET:
            mock_adapter.lookup_fill_fee_by_oid.return_value = lookup_result

        captured = StringIO()
        with patch.dict(sys.modules, {}):
            with patch.object(mod, "__builtins__", mod.__builtins__):
                import builtins
                original_import = builtins.__import__

                def mock_import(name, *args, **kwargs):
                    if name == "adapter":
                        fake_mod = MagicMock()
                        fake_mod.HyperliquidExchangeAdapter = mock_adapter_cls
                        return fake_mod
                    return original_import(name, *args, **kwargs)

                with patch("builtins.__import__", side_effect=mock_import):
                    with patch("sys.stdout", captured):
                        exit_code = 0
                        try:
                            mod.run_execute("BTC", "buy", 0.01, "live", **execute_kwargs)
                        except SystemExit as e:
                            exit_code = e.code

        return json.loads(captured.getvalue()), exit_code

    _OID_AND_FEE_RESPONSE = {
        "status": "ok",
        "response": {
            "type": "order",
            "data": {
                "statuses": [
                    {
                        "filled": {
                            "avgPx": "55000.5",
                            "totalSz": "0.01",
                            "oid": 1234567890,
                            "fee": "0.35",
                        }
                    }
                ]
            },
        },
    }

    _NO_OID_RESPONSE = {
        "status": "ok",
        "response": {
            "type": "order",
            "data": {
                "statuses": [
                    {
                        "filled": {
                            "avgPx": "50000",
                            "totalSz": "0.1",
                        }
                    }
                ]
            },
        },
    }

    _NO_FEE_RESPONSE = {
        "status": "ok",
        "response": {
            "type": "order",
            "data": {
                "statuses": [
                    {
                        "filled": {
                            "avgPx": "2100.0",
                            "totalSz": "0.5",
                            "oid": 9876543210,
                        }
                    }
                ]
            },
        },
    }

    def test_filled_execute_reports_the_fill(self, subtests):
        no_fee = {"avg_px": 2100.0, "total_sz": 0.5, "oid": 9876543210}
        cases = [
            ("oid_and_fee", self._OID_AND_FEE_RESPONSE, _UNSET,
             {"avg_px": 55000.5, "total_sz": 0.01, "oid": 1234567890, "fee": 0.35}),
            ("without_oid", self._NO_OID_RESPONSE, _UNSET,
             {"avg_px": 50000.0, "total_sz": 0.1, "oid": _ABSENT, "fee": _ABSENT}),
            ("lookup_unset", self._NO_FEE_RESPONSE, _UNSET,
             {**no_fee, "fee": _ABSENT, "closed_pnl": _ABSENT}),
            ("lookup_fee_and_pnl", self._NO_FEE_RESPONSE, {"fee": "0.42", "closed_pnl": "3.14"},
             {**no_fee, "fee": 0.42, "closed_pnl": 3.14}),
            ("lookup_mock_result", self._NO_FEE_RESPONSE, MagicMock(),
             {**no_fee, "fee": _ABSENT, "closed_pnl": _ABSENT}),
            ("lookup_mock_fields", self._NO_FEE_RESPONSE, {"fee": MagicMock(), "closed_pnl": MagicMock()},
             {**no_fee, "fee": _ABSENT, "closed_pnl": _ABSENT}),
        ]
        for label, sdk_response, lookup_result, want_fill in cases:
            with subtests.test(case=label):
                result, exit_code = self._run_execute_with_mock_response(
                    sdk_response,
                    **({} if lookup_result is _UNSET else {"lookup_result": lookup_result}),
                )
                assert exit_code == 0
                _assert_out(result["execution"]["fill"], want_fill)

    _UNCONFIRMED_FILL_RESPONSES = [
            {
                "status": "ok",
                "response": {"type": "order", "data": {"statuses": []}},
            },
            {"status": "err", "response": "rejected"},
            {
                "status": "ok",
                "response": {"type": "order", "data": {"statuses": [{}]}},
            },
            {
                "status": "ok",
                "response": {
                    "type": "order",
                    "data": {"statuses": [{"filled": {"avgPx": "bad", "totalSz": "0.1"}}]},
                },
            },
            {
                "status": "ok",
                "response": {
                    "type": "order",
                    "data": {"statuses": [{"filled": {"avgPx": "50000", "totalSz": "bad"}}]},
                },
            },
            {
                "status": "ok",
                "response": {
                    "type": "order",
                    "data": {"statuses": [{"filled": {"avgPx": "0", "totalSz": "0.1"}}]},
                },
            },
            {
                "status": "ok",
                "response": {
                    "type": "order",
                    "data": {"statuses": [{"filled": {"avgPx": "50000", "totalSz": "0"}}]},
                },
            },
    ]

    def test_unconfirmed_fill_exits_with_error(self):
        for i, sdk_response in enumerate(self._UNCONFIRMED_FILL_RESPONSES):
            with _case(i, sdk_response):
                result, exit_code = self._run_execute_with_mock_response(sdk_response)
                assert exit_code == 1
                assert result["execution"] is None
                assert "error" in result
                assert result["error"]

    _FILL_OUTCOME_CASES = [
        ({"status": "ok", "response": {"type": "order", "data": {"statuses": [{"filled": {"avgPx": "3000", "totalSz": "0.2", "oid": 7}}]}}}, "filled", True),
        ({"status": "err", "response": "Insufficient margin"}, "rejected", False),
        ({"status": "ok", "response": {"type": "order", "data": {"statuses": [{"error": "Order could not immediately match"}]}}}, "rejected", False),
        ({"status": "ok", "response": {"type": "order", "data": {"statuses": [{"filled": {"avgPx": "3000", "totalSz": "0"}}]}}}, "rejected", False),
        ({"status": "ok", "response": {"type": "order", "data": {"statuses": []}}}, "unknown", False),
        ({"status": "ok", "response": {"type": "order"}}, "unknown", False),
        ({"status": "ok", "response": {"type": "order", "data": {"statuses": ["garbled"]}}}, "unknown", False),
        ({"status": "ok", "response": {"type": "order", "data": {"statuses": [{"resting": {"oid": 5}}]}}}, "unknown", False),
        ({"status": "ok", "response": {"type": "order", "data": {"statuses": [{"filled": {"avgPx": "bad", "totalSz": "0.2"}}]}}}, "unknown", False),
        ({"status": "ok", "response": {"type": "order", "data": {"statuses": [{"filled": {"avgPx": "nan", "totalSz": "0.2"}}]}}}, "unknown", False),
        (None, "unknown", False),
    ]

    def test_extract_execute_fill_outcome(self):
        mod, spec = _load_check_module()
        spec.loader.exec_module(mod)
        for i, (sdk_response, want_outcome, want_fill) in enumerate(self._FILL_OUTCOME_CASES):
            with _case(i, sdk_response):
                fill, error, outcome = mod._extract_execute_fill(sdk_response)
                assert outcome == want_outcome
                assert (fill is not None) is want_fill
                assert bool(error) is not want_fill

    def test_unconfirmed_fill_preserves_cancel_metadata(self):
        sdk_response = {
            "status": "ok",
            "response": {"type": "order", "data": {"statuses": []}},
        }
        result, exit_code = self._run_execute_with_mock_response(
            sdk_response,
            cancel_oid=[111, 222],
        )
        assert exit_code == 1
        assert result["cancel_stop_loss_succeeded"] is True
        assert result["cancel_stop_loss_succeeded_oids"] == [111, 222]


class TestMarginMode:

    def _run_execute_with_margin(self, margin_mode, leverage, update_leverage_side_effect=None):
        mod, spec = _load_check_module()
        spec.loader.exec_module(mod)

        mock_adapter_cls = MagicMock()
        mock_adapter = MagicMock()
        mock_adapter_cls.return_value = mock_adapter
        if update_leverage_side_effect is not None:
            mock_adapter.update_leverage.side_effect = update_leverage_side_effect
        mock_adapter.market_open.return_value = {
            "status": "ok",
            "response": {"type": "order", "data": {"statuses": [
                {"filled": {"avgPx": "50000", "totalSz": "0.01"}}
            ]}},
        }

        captured = StringIO()
        import builtins
        original_import = builtins.__import__

        def mock_import(name, *args, **kwargs):
            if name == "adapter":
                fake_mod = MagicMock()
                fake_mod.HyperliquidExchangeAdapter = mock_adapter_cls
                return fake_mod
            return original_import(name, *args, **kwargs)

        with patch("builtins.__import__", side_effect=mock_import):
            with patch("sys.stdout", captured):
                exit_code = 0
                try:
                    mod.run_execute("BTC", "buy", 0.01, "live",
                                    margin_mode=margin_mode, leverage=leverage)
                except SystemExit as e:
                    exit_code = e.code
        return json.loads(captured.getvalue()), mock_adapter, exit_code

    def test_margin_mode_sets_leverage_before_the_order_or_fails_closed(self, subtests):
        cases = [
            ("isolated", "isolated", 5, None, 0, call(5, "BTC", is_cross=False), "once", "buy", [], []),
            ("cross", "cross", 3, None, 0, call(3, "BTC", is_cross=True), "once", None, [], []),
            ("no_margin_mode", "", 0, None, 0, None, "once", None, [], []),
            ("invalid_margin_mode", "portfolio", 5, None, 1, None, None, None, ["invalid margin_mode"], []),
            ("zero_leverage", "isolated", 0, None, 1, None, None, None, [], ["leverage"]),
            ("update_leverage_failure", "isolated", 5, RuntimeError("HL rejected: position open"), 1, "once", None,
             None, ["update_leverage failed", "position open"], []),
        ]
        for (label, margin_mode, leverage, side_effect, want_exit, want_update, want_open, want_action,
             error_parts, lower_error_parts) in cases:
            with subtests.test(case=label):
                result, adapter, exit_code = self._run_execute_with_margin(
                    margin_mode, leverage, update_leverage_side_effect=side_effect)
                assert exit_code == want_exit
                _assert_calls(adapter, {"update_leverage": want_update, "market_open": want_open})
                if want_action is not None:
                    assert result["execution"]["action"] == want_action
                for part in error_parts:
                    assert part in result.get("error", "")
                for part in lower_error_parts:
                    assert part in result.get("error", "").lower()


class TestPeerLeverageSkip:

    def _run_execute_with_existing_pos(self, margin_mode, leverage, current_state):
        mod, spec = _load_check_module()
        spec.loader.exec_module(mod)

        mock_adapter_cls = MagicMock()
        mock_adapter = MagicMock()
        mock_adapter_cls.return_value = mock_adapter
        if isinstance(current_state, Exception):
            mock_adapter.get_position_leverage.side_effect = current_state
        else:
            mock_adapter.get_position_leverage.return_value = current_state
        mock_adapter.market_open.return_value = {
            "status": "ok",
            "response": {"type": "order", "data": {"statuses": [
                {"filled": {"avgPx": "50000", "totalSz": "0.01"}}
            ]}},
        }

        captured = StringIO()
        import builtins
        original_import = builtins.__import__

        def mock_import(name, *args, **kwargs):
            if name == "adapter":
                fake_mod = MagicMock()
                fake_mod.HyperliquidExchangeAdapter = mock_adapter_cls
                return fake_mod
            return original_import(name, *args, **kwargs)

        with patch("builtins.__import__", side_effect=mock_import):
            with patch("sys.stdout", captured):
                exit_code = 0
                try:
                    mod.run_execute("ETH", "buy", 0.5, "live",
                                    margin_mode=margin_mode, leverage=leverage)
                except SystemExit as e:
                    exit_code = e.code
        return json.loads(captured.getvalue()), mock_adapter, exit_code

    def test_update_leverage_only_when_state_differs(self):
        cases = [
            ({"margin_mode": "isolated", "leverage": 5}, False),
            ({"margin_mode": "cross", "leverage": 5}, True),
            ({"margin_mode": "isolated", "leverage": 3}, True),
            (None, True),
            (RuntimeError("info endpoint timeout"), True),
        ]
        for current_state, expect_update in cases:
            with _case(current_state, expect_update):
                result, adapter, exit_code = self._run_execute_with_existing_pos(
                    "isolated", 5, current_state)
                assert exit_code == 0
                if expect_update:
                    adapter.update_leverage.assert_called_once_with(5, "ETH", is_cross=False)
                else:
                    adapter.update_leverage.assert_not_called()
                adapter.market_open.assert_called_once()
                assert result["execution"]["action"] == "buy"


class TestClassifySLResponse:

    def _classify(self, response):
        mod, spec = _load_check_module()
        spec.loader.exec_module(mod)
        return mod._classify_sl_response(response)

    _SL_RESPONSE_CASES = [
        ({"response": {"type": "order", "data": {"statuses": [{"resting": {"oid": 12345}}]}}},
         ("resting", 12345)),
        ({"response": {"type": "order", "data": {"statuses": [{"resting": {}}]}}},
         ("resting", 0)),
        ({"response": {"type": "order", "data": {"statuses": [
            {"filled": {"oid": 67890, "avgPx": "3000"}}]}}},
         ("filled", 67890)),
        ({"response": {"type": "order", "data": {"statuses": [{"filled": {}}]}}},
         ("filled", 0)),
        ({"response": {"type": "order", "data": {"statuses": [
            {"error": "Too many open trigger orders"}]}}},
         ("error", "Too many open trigger orders")),
        ({"response": {"type": "order", "data": {"statuses": []}}}, ("missing", None)),
        ({}, ("missing", None)),
        ({"response": {"type": "order", "data": {"statuses": ["not a dict"]}}},
         ("missing", None)),
    ]

    def test_classify_sl_response(self):
        for response, expected in self._SL_RESPONSE_CASES:
            assert self._classify(response) == expected, (response, expected)


_CANCEL_OK_RESPONSE = {
    "status": "ok",
    "response": {"type": "cancel", "data": {"statuses": ["success"]}},
}
_CANCEL_REJECTED_RESPONSE = {
    "status": "err",
    "response": {"type": "cancel", "data": {"statuses": [{"error": "order already filled"}]}},
}


class TestClassifyCancelResponse:

    def _load(self):
        mod, spec = _load_check_module()
        spec.loader.exec_module(mod)
        return mod

    def test_classify_cancel_response(self):
        cases = [
            (_CANCEL_OK_RESPONSE, "ok", ""),
            ({"status": "ok", "response": {"data": {"statuses": [{}]}}}, "ok", ""),
            ({"status": "err"}, "error", "err"),
            ({"status": "ok", "response": {"data": {"statuses": [{"error": "no such order"}]}}},
             "error", None),
            ({}, "error", None),
            (None, "error", None),
        ]
        mod = self._load()
        for response, kind, fragment in cases:
            label = (response, kind, fragment)
            got_kind, payload = mod._classify_cancel_response(response)
            assert got_kind == kind, label
            if fragment == "":
                assert payload == "", label
            elif fragment is not None:
                assert fragment in payload, label


class TestUpdateStopLoss:

    def _run_update(
        self,
        side="long",
        place_response=None,
        cancel_side_effect=None,
        cancel_response=_UNSET,
        place_side_effect=None,
        open_oids=None,
        open_oids_side_effect=None,
        lookup_result=_UNSET,
        cancel_oid=11111,
        post_place_oids=_UNSET,
        size=0.5,
        frontend_reads=None,
    ):
        mod, spec = _load_check_module()
        spec.loader.exec_module(mod)

        mock_adapter_cls = MagicMock()
        mock_adapter = MagicMock()
        mock_adapter_cls.return_value = mock_adapter
        mock_adapter.round_perps_trigger_px.side_effect = lambda _symbol, px: round(px, 2)
        mock_adapter.cancel_trigger_order.return_value = (
            _CANCEL_OK_RESPONSE if cancel_response is _UNSET else cancel_response
        )
        base_oids = {11111} if open_oids is None else open_oids
        if open_oids_side_effect is not None:
            mock_adapter.open_order_oids.side_effect = open_oids_side_effect
        elif post_place_oids is not _UNSET:
            reads = {"n": 0}

            def _oids(_symbol):
                reads["n"] += 1
                if reads["n"] == 1:
                    return base_oids
                if isinstance(post_place_oids, Exception):
                    raise post_place_oids
                return post_place_oids

            mock_adapter.open_order_oids.side_effect = _oids
        else:
            mock_adapter.open_order_oids.return_value = base_oids
        if cancel_side_effect is not None:
            mock_adapter.cancel_trigger_order.side_effect = cancel_side_effect
        if lookup_result is not _UNSET:
            mock_adapter.lookup_fill_fee_by_oid.return_value = lookup_result
        mock_adapter.place_stop_loss.return_value = place_response or {
            "response": {"type": "order", "data": {"statuses": [
                {"resting": {"oid": 22222}}
            ]}}
        }
        if place_side_effect is not None:
            mock_adapter.place_stop_loss.side_effect = place_side_effect
            mock_adapter.modify_stop_loss.side_effect = place_side_effect
        else:
            mock_adapter.modify_stop_loss.return_value = mock_adapter.place_stop_loss.return_value
        if frontend_reads is not None:
            mock_adapter.frontend_open_orders.side_effect = frontend_reads
        else:
            mock_adapter.frontend_open_orders.return_value = []

        captured = StringIO()
        import builtins
        original_import = builtins.__import__

        def mock_import(name, *args, **kwargs):
            if name == "adapter":
                fake_mod = MagicMock()
                fake_mod.HyperliquidExchangeAdapter = mock_adapter_cls
                return fake_mod
            return original_import(name, *args, **kwargs)

        self.exit_code = 0
        with patch("builtins.__import__", side_effect=mock_import):
            with patch("sys.stdout", captured):
                try:
                    mod.run_update_stop_loss("ETH", side, size, 3104.123, "live", cancel_oid=cancel_oid)
                except SystemExit as exc:
                    self.exit_code = exc.code
        return json.loads(captured.getvalue()), mock_adapter

    def test_update_stop_loss_cancel_only(self):
        cases = [
            ({11111}, None, _CANCEL_OK_RESPONSE, None, _UNSET, {"cancel_stop_loss_succeeded": True}, 0, True),
            ({11111}, None, _CANCEL_REJECTED_RESPONSE, None, _UNSET, {"cancel_stop_loss_error": "already filled"}, 1, True),
            ({11111}, None, _UNSET, RuntimeError("cancel down"), _UNSET, {"cancel_stop_loss_error": "cancel down"}, 1, True),
            (set(), None, _UNSET, None, {"fee": 0.01, "count": 1}, {"stop_loss_filled_externally": True}, 0, False),
            (set(), None, _UNSET, None, None, {"stop_loss_not_open": True}, 0, False),
            (None, RuntimeError("indexer down"), _UNSET, None, _UNSET, {"open_order_check_error": "indexer down"}, 1, False),
        ]
        for i, (open_oids, open_err, cancel_response, cancel_err, lookup, want_keys, want_exit, want_cancel) in enumerate(cases):
            with _case(i, want_keys):
                out, adapter = self._run_update(
                    size=0,
                    open_oids=open_oids,
                    open_oids_side_effect=open_err,
                    cancel_response=cancel_response,
                    cancel_side_effect=cancel_err,
                    lookup_result=lookup,
                )
                adapter.place_stop_loss.assert_not_called()
                adapter.round_perps_trigger_px.assert_not_called()
                assert out["cancel_only"] is True
                assert self.exit_code == want_exit
                assert bool(out.get("error")) is (want_exit == 1)
                if want_cancel:
                    adapter.cancel_trigger_order.assert_called_once_with("ETH", 11111)
                else:
                    adapter.cancel_trigger_order.assert_not_called()
                for key, want in want_keys.items():
                    if isinstance(want, str):
                        assert want in out[key]
                    else:
                        assert out[key] == want

    @staticmethod
    def _frontend_stop(oid, trigger):
        return {
            "oid": oid, "side": "A", "sz": "0.5", "reduceOnly": True,
            "isTrigger": True, "orderType": "Stop Market", "triggerPx": trigger,
        }

    def _check_update_cases(self, subtests, cases):
        for label, kwargs, want_calls, want_out in cases:
            with subtests.test(case=label):
                out, adapter = self._run_update(**kwargs)
                _assert_calls(adapter, want_calls)
                _assert_out(out, want_out)

    def test_readable_modify_replies_never_cancel_or_add_a_second_stop(self, subtests):
        self._check_update_cases(subtests, [
            ("modify_in_place_long_stop", {"side": "long"},
             {"modify_stop_loss": call("ETH", 11111, 0.5, 3104.12, False), "place_stop_loss": None,
              "cancel_trigger_order": None},
             {"stop_loss_oid": 22222, "stop_loss_trigger_px": 3104.12, "cancel_stop_loss_succeeded": _ABSENT}),
            ("short_stop_places_buy_trigger", {"side": "short"},
             {"modify_stop_loss": call("ETH", 11111, 0.5, 3104.12, True), "place_stop_loss": None},
             {"stop_loss_oid": 22222}),
            ("resting_modify_does_not_cancel", {"cancel_response": _CANCEL_REJECTED_RESPONSE},
             {"modify_stop_loss": "once", "place_stop_loss": None, "cancel_trigger_order": None},
             {"cancel_stop_loss_succeeded": _ABSENT, "stop_loss_oid": 22222}),
            ("immediate_fill_does_not_cancel_a_second_stop",
             {"place_response": {"response": {"data": {"statuses": [{"filled": {"totalSz": "0.5", "avgPx": "3104"}}]}}}},
             {"cancel_trigger_order": None, "place_stop_loss": None},
             {"stop_loss_filled_immediately": True}),
            ("rejected_placement_leaves_the_old_stop",
             {"place_response": {"status": "err", "response": "invalid trigger price"}},
             {"cancel_trigger_order": None},
             {"stop_loss_old_still_open": True, "stop_loss_outcome_unknown": _ABSENT,
              "cancel_stop_loss_succeeded": _ABSENT}),
            ("cap_rejection_does_not_add_a_second_stop",
             {"place_response": {"status": "err", "response": "open order limit"}},
             {"modify_stop_loss": "once", "place_stop_loss": None, "cancel_trigger_order": None},
             {"stop_loss_old_still_open": True, "stop_loss_oid": _ABSENT}),
        ])

    def test_unconfirmed_modify_never_adds_a_second_stop(self, subtests):
        reset = RuntimeError("connection reset after submit")
        old = self._frontend_stop(11111, "3104.11")
        moved = self._frontend_stop(11111, "3104.12")
        peer = self._frontend_stop(22222, "3104.12")
        landed = self._frontend_stop(77, "3104.12")
        self._check_update_cases(subtests, [
            ("unreadable_modify_does_not_add_a_second_stop",
             {"place_response": {"status": "weird"}, "post_place_oids": {22222}},
             {"place_stop_loss": None, "cancel_trigger_order": None},
             {"stop_loss_oid": _ABSENT, "stop_loss_outcome_unknown": True, "stop_loss_old_still_open": True}),
            ("modify_exception_does_not_add_a_second_stop",
             {"place_side_effect": reset, "post_place_oids": {22222}},
             {"place_stop_loss": None, "cancel_trigger_order": None},
             {"stop_loss_oid": _ABSENT, "stop_loss_old_still_open": True}),
            ("unreadable_modify_with_nothing_else_resting_is_unknown",
             {"place_response": {"status": "weird"}, "post_place_oids": set()},
             {"place_stop_loss": None},
             {"stop_loss_outcome_unknown": True, "stop_loss_oid": _ABSENT,
              "stop_loss_error": _Has("no usable status")}),
            ("ambiguous_diff_marks_outcome_unknown",
             {"place_response": {"status": "weird"}, "post_place_oids": {22222, 33333}},
             {},
             {"stop_loss_outcome_unknown": True, "stop_loss_oid": _ABSENT}),
            ("unreadable_place_with_unresolvable_diff_is_unknown_and_does_not_cancel",
             {"place_response": {"status": "weird"}, "post_place_oids": RuntimeError("indexer down")},
             {"cancel_trigger_order": None},
             {"stop_loss_outcome_unknown": True, "stop_loss_old_still_open": True, "stop_loss_oid": _ABSENT}),
            ("modify_failure_leaves_the_old_stop",
             {"place_side_effect": RuntimeError("cancel down")},
             {"modify_stop_loss": "once", "place_stop_loss": None, "cancel_trigger_order": None},
             {"stop_loss_oid": _ABSENT, "stop_loss_old_still_open": True}),
            ("modify_exception_adopts_the_landed_stop",
             {"place_side_effect": reset, "frontend_reads": [[], [landed]]},
             {"place_stop_loss": None},
             {"stop_loss_oid": 77, "stop_loss_outcome_unknown": _ABSENT}),
            ("modify_exception_old_stop_at_old_trigger_is_unknown",
             {"place_side_effect": reset, "frontend_reads": [[old], [old]]},
             {"place_stop_loss": None},
             {"stop_loss_oid": _ABSENT, "stop_loss_outcome_unknown": True, "stop_loss_old_still_open": True}),
            ("modify_exception_old_stop_at_new_trigger_confirms_in_place",
             {"place_side_effect": reset, "frontend_reads": [[moved], [moved]]},
             {"place_stop_loss": None},
             {"stop_loss_oid": 11111, "stop_loss_outcome_unknown": _ABSENT}),
            ("modify_exception_ignores_a_peers_matching_stop",
             {"place_side_effect": reset, "frontend_reads": [[old, peer], [old, peer]]},
             {"place_stop_loss": None},
             {"stop_loss_oid": _ABSENT, "stop_loss_outcome_unknown": True}),
            ("modify_value_error_is_a_plain_rejection",
             {"place_side_effect": ValueError("Size rounded to zero")},
             {"place_stop_loss": None},
             {"stop_loss_outcome_unknown": _ABSENT, "stop_loss_old_still_open": True}),
        ])

    def test_fresh_stop_placement_checks_the_old_stop_first(self, subtests):
        self._check_update_cases(subtests, [
            ("fresh_place_unreadable_reply_resolves_by_book_diff",
             {"open_oids": set(), "lookup_result": None, "place_response": {"status": "weird"},
              "post_place_oids": {22222}},
             {},
             {"stop_loss_oid": 22222, "stop_loss_outcome_unknown": _ABSENT}),
            ("open_order_lookup_failure_defers_replacement",
             {"open_oids_side_effect": RuntimeError("indexer down")},
             {"cancel_trigger_order": None, "place_stop_loss": None},
             {"open_order_check_error": "indexer down", "stop_loss_oid": _ABSENT}),
            ("missing_old_oid_with_fill_does_not_replace",
             {"open_oids": set(), "lookup_result": {"fee": 0.01, "count": 1}},
             {"cancel_trigger_order": None, "place_stop_loss": None},
             {"stop_loss_filled_externally": True, "stop_loss_oid": _ABSENT}),
            ("missing_old_oid_without_fill_places_replacement",
             {"open_oids": set(), "lookup_result": None},
             {"cancel_trigger_order": None, "lookup_fill_fee_by_oid": "once",
              "place_stop_loss": call("ETH", 0.5, 3104.12, False)},
             {"stop_loss_oid": 22222}),
            ("initial_placement_without_cancel_oid",
             {"cancel_oid": 0},
             {"open_order_oids": call("ETH"), "cancel_trigger_order": None,
              "place_stop_loss": call("ETH", 0.5, 3104.12, False)},
             {"stop_loss_oid": 22222}),
        ])

    def test_list_open_orders_reads_frontend_stop_fields(self):
        mod, spec = _load_check_module()
        spec.loader.exec_module(mod)
        mock_adapter_cls = MagicMock()
        mock_adapter = MagicMock()
        mock_adapter_cls.return_value = mock_adapter
        mock_adapter.frontend_open_orders.return_value = [{
            "coin": "ETH",
            "oid": 44444,
            "side": "A",
            "sz": "0.5",
            "isTrigger": True,
            "reduceOnly": True,
            "orderType": "Stop Market",
            "triggerPx": "3104.12",
        }]
        captured = StringIO()
        import builtins
        original_import = builtins.__import__

        def mock_import(name, *args, **kwargs):
            if name == "adapter":
                fake_mod = MagicMock()
                fake_mod.HyperliquidExchangeAdapter = mock_adapter_cls
                return fake_mod
            return original_import(name, *args, **kwargs)

        with patch("builtins.__import__", side_effect=mock_import):
            with patch("sys.stdout", captured):
                mod.run_list_open_order_oids("ETH")
        out = json.loads(captured.getvalue())
        mock_adapter.open_order_oids.assert_not_called()
        order = out["open_orders"][0]
        assert order["oid"] == 44444
        assert order["is_trigger"] is True
        assert order["reduce_only"] is True
        assert order["order_type"] == "Stop Market"
        assert order["trigger_px"] == 3104.12
        assert order["coin"] == "ETH"
        assert order["sz"] == 0.5


class TestCloseFullPosition:

    def _run_close_full(self, market_close_response=None):
        mod, spec = _load_check_module()
        spec.loader.exec_module(mod)

        mock_adapter_cls = MagicMock()
        mock_adapter = MagicMock()
        mock_adapter_cls.return_value = mock_adapter
        mock_adapter.lookup_fill_fee_by_oid.return_value = {}
        mock_adapter.market_close.return_value = market_close_response or {
            "status": "ok",
            "response": {
                "type": "order",
                "data": {
                    "statuses": [
                        {
                            "filled": {
                                "avgPx": "3000.5",
                                "totalSz": "0.211",
                                "oid": 888,
                            }
                        }
                    ]
                },
            },
        }

        captured = StringIO()
        import builtins
        original_import = builtins.__import__

        def mock_import(name, *args, **kwargs):
            if name == "adapter":
                fake_mod = MagicMock()
                fake_mod.HyperliquidExchangeAdapter = mock_adapter_cls
                return fake_mod
            return original_import(name, *args, **kwargs)

        with patch("builtins.__import__", side_effect=mock_import):
            with patch("sys.stdout", captured):
                mod.run_execute("ETH", "sell", 0.0, "live", close_full_position=True)

        return json.loads(captured.getvalue()), mock_adapter

    def test_uses_market_close_and_reports_the_sized_close_shape(self):
        out, adapter = self._run_close_full()
        adapter.market_close.assert_called_once_with("ETH", sz=None)
        adapter.market_open.assert_not_called()
        assert "execution" in out
        fill = out["execution"]["fill"]
        assert fill["avg_px"] == 3000.5
        assert fill["total_sz"] == 0.211
        assert fill["oid"] == 888


class TestRunExecuteCloseMode:

    _FILLED = {
        "status": "ok",
        "response": {"type": "order", "data": {"statuses": [{"filled": {"avgPx": "3000", "totalSz": "0.2", "oid": 77}}]}},
    }

    def test_run_execute_close_mode_routes(self):
        cases = [
            ({"close_mode": "reduce_only"}, "live", 0.2, 0, ("market_close_sized", ("ETH", False, 0.2, 2970.0), {"reduce_only": True})),
            ({"close_mode": "cross"}, "live", 0.2, 0, ("market_close_sized", ("ETH", False, 0.2, 2970.0), {"reduce_only": False})),
            ({}, "live", 0.2, 0, ("market_open", ("ETH", False, 0.2), {})),
            ({"close_full_position": True}, "live", 0.2, 0, ("market_close", ("ETH",), {"sz": None})),
            ({"close_mode": "reduce_only", "close_full_position": True}, "live", 0.2, 1, None),
            ({"close_mode": "reduce_only", "stop_loss_pct": 2.0}, "live", 0.2, 1, None),
            ({"close_mode": "cross", "prev_pos_qty": 0.1}, "live", 0.2, 1, None),
            ({"close_mode": "reduce_only"}, "paper", 0.2, 1, None),
            ({"close_mode": "reduce_only", "cancel_oid": [555]}, "live", 0.0, 1, None),
        ]
        for kwargs, mode, floored, want_exit, want_call in cases:
            with _case(kwargs, mode, floored):
                self._check_close_mode_route(kwargs, mode, floored, want_exit, want_call)

    def _check_close_mode_route(self, kwargs, mode, floored, want_exit, want_call):
        mod, spec = _load_check_module()
        spec.loader.exec_module(mod)

        mock_adapter_cls = MagicMock()
        mock_adapter = MagicMock()
        mock_adapter_cls.return_value = mock_adapter
        mock_adapter.lookup_fill_fee_by_oid.return_value = {}
        mock_adapter.floor_size.return_value = floored
        mock_adapter.sized_close_price.return_value = 2970.0
        for name in ("market_open", "market_close", "market_close_sized"):
            getattr(mock_adapter, name).return_value = self._FILLED

        captured = StringIO()
        import builtins
        original_import = builtins.__import__

        def mock_import(name, *args, **kw):
            if name == "adapter":
                fake_mod = MagicMock()
                fake_mod.HyperliquidExchangeAdapter = mock_adapter_cls
                return fake_mod
            return original_import(name, *args, **kw)

        exit_code = 0
        with patch("builtins.__import__", side_effect=mock_import):
            with patch("sys.stdout", captured):
                try:
                    mod.run_execute("ETH", "sell", 0.2, mode, **kwargs)
                except SystemExit as e:
                    exit_code = e.code

        order_methods = ("market_open", "market_close", "market_close_sized")
        assert exit_code == want_exit
        if want_call is None:
            for name in order_methods:
                getattr(mock_adapter, name).assert_not_called()
            mock_adapter.cancel_trigger_order.assert_not_called()
            assert json.loads(captured.getvalue()).get("error")
            return
        name, args, call_kwargs = want_call
        getattr(mock_adapter, name).assert_called_once_with(*args, **call_kwargs)
        for other in order_methods:
            if other != name:
                getattr(mock_adapter, other).assert_not_called()


    def test_run_execute_order_outcome(self):
        filled = self._FILLED
        cases = [
            ({"close_mode": "reduce_only", "cancel_oid": [555]}, "live", 0.2, 2970.0, filled, 0, "filled", True),
            ({"close_mode": "reduce_only", "cancel_oid": [555]}, "live", 0.2, 2970.0, {"status": "ok", "response": {"type": "order", "data": {"statuses": [{"error": "Order could not immediately match"}]}}}, 1, "rejected", True),
            ({"close_mode": "reduce_only", "cancel_oid": [555]}, "live", 0.2, 2970.0, {"status": "ok", "response": {"type": "order", "data": {"statuses": []}}}, 1, "unknown", True),
            ({"close_mode": "reduce_only", "cancel_oid": [555]}, "live", 0.2, 2970.0, RuntimeError("socket closed"), 1, "unknown", True),
            ({"close_mode": "reduce_only", "cancel_oid": [555]}, "paper", 0.2, 2970.0, filled, 1, "not_sent", False),
            ({"close_mode": "reduce_only", "cancel_oid": [555], "stop_loss_pct": 2.0}, "live", 0.2, 2970.0, filled, 1, "not_sent", False),
            ({"close_mode": "reduce_only", "cancel_oid": [555]}, "live", 0.0, 2970.0, filled, 1, "not_sent", False),
            ({"close_mode": "reduce_only", "cancel_oid": [555]}, "live", 0.2, ValueError("no usable mid price for ETH"), filled, 1, "not_sent", False),
            ({"close_mode": "cross", "cancel_oid": [555]}, "live", 0.2, ConnectionError("allMids down"), filled, 1, "not_sent", False),
            ({"cancel_oid": [555], "margin_mode": "bogus", "leverage": 3}, "live", 0.2, 2970.0, filled, 1, "not_sent", False),
        ]
        for i, (kwargs, mode, floored, mid, order, want_exit, want_outcome, want_cancel) in enumerate(cases):
            with _case(i, kwargs, mode, floored, mid, want_outcome):
                self._check_order_outcome(kwargs, mode, floored, mid, order, want_exit, want_outcome, want_cancel)

    def _check_order_outcome(self, kwargs, mode, floored, mid, order, want_exit, want_outcome, want_cancel):
        mod, spec = _load_check_module()
        spec.loader.exec_module(mod)

        mock_adapter_cls = MagicMock()
        mock_adapter = MagicMock()
        mock_adapter_cls.return_value = mock_adapter
        mock_adapter.lookup_fill_fee_by_oid.return_value = {}
        mock_adapter.floor_size.return_value = floored
        mock_adapter.cancel_trigger_order.return_value = _CANCEL_OK_RESPONSE
        if isinstance(mid, Exception):
            mock_adapter.sized_close_price.side_effect = mid
        else:
            mock_adapter.sized_close_price.return_value = mid
        for name in ("market_open", "market_close", "market_close_sized"):
            if isinstance(order, Exception):
                getattr(mock_adapter, name).side_effect = order
            else:
                getattr(mock_adapter, name).return_value = order

        captured = StringIO()
        import builtins
        original_import = builtins.__import__

        def mock_import(name, *args, **kw):
            if name == "adapter":
                fake_mod = MagicMock()
                fake_mod.HyperliquidExchangeAdapter = mock_adapter_cls
                return fake_mod
            return original_import(name, *args, **kw)

        exit_code = 0
        with patch("builtins.__import__", side_effect=mock_import):
            with patch("sys.stdout", captured):
                try:
                    mod.run_execute("ETH", "sell", 0.2, mode, **kwargs)
                except SystemExit as e:
                    exit_code = e.code

        out = json.loads(captured.getvalue())
        assert exit_code == want_exit
        assert out["order_outcome"] == want_outcome
        if want_cancel:
            mock_adapter.cancel_trigger_order.assert_called_once_with("ETH", 555)
            assert out["cancel_stop_loss_succeeded_oids"] == [555]
            return
        mock_adapter.cancel_trigger_order.assert_not_called()
        for name in ("market_open", "market_close", "market_close_sized"):
            getattr(mock_adapter, name).assert_not_called()
        assert "cancel_stop_loss_succeeded_oids" not in out


class TestSyncProtection:

    def _run_sync(
        self,
        *,
        size=1.0,
        avg_cost=2000.0,
        entry_atr=20.0,
        side="long",
        sl_oid=0,
        tp1_oid=0,
        tp2_oid=0,
        tp_tiers=None,
        tp_oids=None,
        tp_armed_tiers=None,
        open_oids=None,
        fill_lookup_by_oid=None,
        place_responses=None,
        reconcile_fill_hints_json=None,
        cancel_tp_oids=None,
        force_sl_replace=False,
        stop_loss_atr_mult=1.0,
        open_orders_error=None,
        open_oids_sequence=None,
        tp_place_error=None,
        cancel_response=None,
        force_tp_replace=None,
        cancel_side_effect=None,
        tp1_atr_mult=1.0,
        tp1_fraction=0.5,
        tp2_atr_mult=2.0,
    ):
        mod, spec = _load_check_module()
        spec.loader.exec_module(mod)

        mock_adapter_cls = MagicMock()
        mock_adapter = MagicMock()
        mock_adapter_cls.return_value = mock_adapter
        if open_orders_error is not None:
            mock_adapter.open_order_oids.side_effect = (
                open_orders_error if isinstance(open_orders_error, Exception) else Exception(open_orders_error)
            )
        elif open_oids_sequence is not None:
            reads = list(open_oids_sequence)

            def open_oids_step(_symbol):
                step = reads.pop(0) if reads else set()
                if isinstance(step, Exception):
                    raise step
                return step

            mock_adapter.open_order_oids.side_effect = open_oids_step
        else:
            mock_adapter.open_order_oids.return_value = (
                set() if open_oids is None else set(open_oids)
            )
        mock_adapter.round_perps_trigger_px.side_effect = lambda _sym, px: round(px, 4)
        mock_adapter.round_size.side_effect = lambda _sym, sz: round(sz, 3)
        mock_adapter.floor_size.side_effect = lambda _sym, sz: math.floor(sz * 1000 + 1e-9) / 1000
        if cancel_response is not None:
            mock_adapter.cancel_order_by_oid.return_value = cancel_response
        if cancel_side_effect is not None:
            mock_adapter.cancel_order_by_oid.side_effect = cancel_side_effect

        fills = fill_lookup_by_oid or {}

        def lookup_side_effect(oid, *args, **kwargs):
            return fills.get(int(oid), {})

        mock_adapter.lookup_fill_fee_by_oid.side_effect = lookup_side_effect

        responses = place_responses or {}

        def stop_loss_side_effect(*args, **kwargs):
            return responses.get("sl", {"status": "ok", "response": {"type": "order", "data": {"statuses": [{"resting": {"oid": 9000}}]}}})

        def tp_side_effect(symbol, sz, px, is_buy):
            if tp_place_error is not None:
                raise tp_place_error
            count = mock_adapter.place_take_profit_limit.call_count
            key = "tp1" if count == 1 else "tp2"
            return responses.get(key, {
                "status": "ok",
                "response": {"type": "order", "data": {"statuses": [{"resting": {"oid": 9100 + count}}]}}
            })

        mock_adapter.place_stop_loss.side_effect = stop_loss_side_effect
        mock_adapter.place_take_profit_limit.side_effect = tp_side_effect

        captured = StringIO()
        import builtins
        original_import = builtins.__import__

        def mock_import(name, *args, **kwargs):
            if name == "adapter":
                fake_mod = MagicMock()
                fake_mod.HyperliquidExchangeAdapter = mock_adapter_cls
                return fake_mod
            return original_import(name, *args, **kwargs)

        with patch("builtins.__import__", side_effect=mock_import):
            with patch("sys.stdout", captured):
                mod.run_sync_protection(
                    "ETH",
                    side,
                    size,
                    avg_cost,
                    entry_atr,
                    "live",
                    stop_loss_atr_mult=stop_loss_atr_mult,
                    tp1_atr_mult=tp1_atr_mult,
                    tp1_fraction=tp1_fraction,
                    tp2_atr_mult=tp2_atr_mult,
                    stop_loss_oid=sl_oid,
                    tp1_oid=tp1_oid,
                    tp2_oid=tp2_oid,
                    tp_tiers=tp_tiers,
                    tp_oids=tp_oids,
                    tp_armed_tiers=tp_armed_tiers,
                    reconcile_fill_hints_json=reconcile_fill_hints_json or "",
                    cancel_tp_oids=cancel_tp_oids,
                    force_sl_replace=force_sl_replace,
                    force_tp_replace=force_tp_replace,
                )
        return json.loads(captured.getvalue()), mock_adapter


    def test_force_tp_replace_places_only_after_a_confirmed_cancel(self):
        cases = [
            ("cancel_ok", _CANCEL_OK_RESPONSE, [9101, 7002], 1, False),
            ("cancel_rejected", _CANCEL_REJECTED_RESPONSE, [7001, 7002], 0, True),
        ]
        for label, cancel_response, want_oids, want_placements, want_error in cases:
            with _case(label):
                out, adapter = self._run_sync(
                    stop_loss_atr_mult=0,
                    tp_tiers=[(1.0, 0.5), (2.0, 1.0)],
                    tp_oids=[7001, 7002],
                    tp_armed_tiers=[True, True],
                    open_oids={7001, 7002},
                    cancel_response=cancel_response,
                    force_tp_replace=[True, False],
                )
                adapter.cancel_order_by_oid.assert_called_once_with("ETH", 7001)
                assert adapter.place_take_profit_limit.call_count == want_placements
                assert out["tp_oids"] == want_oids
                assert bool(out.get("tp_errors", [""])[0]) is want_error

    def test_size_skipped_tiers_report_unverified(self):
        cases = [
            (0.003, [(1.0, 0.4), (2.0, 0.5), (3.0, 1.0)], [0, 7002, 0], [False, True, False], 2),
            (0.0004, [(1.0, 0.5), (2.0, 1.0)], [7001, 7002], [True, True], 0),
        ]
        for size, tiers, oids, skipped, placements in cases:
            with _case(size, tiers, oids):
                out, adapter = self._run_sync(
                    size=size, stop_loss_atr_mult=0, tp_tiers=tiers,
                    tp_oids=oids, tp_armed_tiers=[oid > 0 for oid in oids],
                    open_oids=set(),
                )
                assert out["tp_size_skipped"] == skipped
                assert adapter.place_take_profit_limit.call_count == placements
                if placements:
                    assert out["tp_oids"][1] == 7002
                    assert out["tp_oids"][0] > 0
                    assert out["tp_oids"][2] > 0
                adapter.lookup_fill_fee_by_oid.assert_not_called()

    def test_unresolved_tp_placement_resolves_by_book_diff(self):
        cases = [
            ([{7002}, {7002}, {7002, 9500}], None, True, 9500, False, False),
            ([{7002}, {7002}, {7002, 9500}], RuntimeError("read timeout"), False, 9500, False, False),
            ([{7002}, {7002}, {7002}], RuntimeError("connection refused"), False, 0, False, True),
            ([{7002}, {7002}, Exception("re-read failed")], RuntimeError("connection refused"), False, 0, True, True),
            ([{7002}, Exception("snapshot failed"), {7002, 9500}], RuntimeError("connection refused"), False, 0, True, True),
            ([{7002}, {7002}, {7002, 9500, 9501}], RuntimeError("connection refused"), False, 0, True, True),
        ]
        for i, (reads, place_error, unreadable_response, want_oid, want_unknown, want_error) in enumerate(cases):
            with _case(i, reads, place_error, unreadable_response):
                responses = {"tp1": {"status": "ok", "response": {"type": "order", "data": {"statuses": []}}}} if unreadable_response else None
                out, adapter = self._run_sync(
                    stop_loss_atr_mult=0,
                    tp_tiers=[(1.0, 0.5), (2.0, 1.0)],
                    tp_oids=[0, 7002],
                    tp_armed_tiers=[False, True],
                    open_oids_sequence=reads,
                    tp_place_error=place_error,
                    place_responses=responses,
                )
                assert adapter.place_take_profit_limit.call_count == 1
                assert out["tp_oids"] == [want_oid, 7002]
                assert bool(out.get("tp_outcome_unknown", [False])[0]) is want_unknown
                assert bool(out.get("tp_errors", [""])[0]) is want_error

    def _check_sync_cases(self, subtests, cases):
        for label, kwargs, want_calls, want_out in cases:
            with subtests.test(case=label):
                out, adapter = self._run_sync(**kwargs)
                _assert_calls(adapter, want_calls)
                _assert_out(out, want_out)

    def test_manual_close_recovery(self, subtests):
        three_tiers = [(1.0, 0.4), (2.0, 0.8), (3.0, 1.0)]
        self._check_sync_cases(subtests, [
            ("leaves_the_stop_alone",
             {"stop_loss_atr_mult": 0, "tp_tiers": three_tiers, "tp_oids": [0, 0, 7003],
              "tp_armed_tiers": [True, False, True], "open_oids": {7003}},
             {"place_stop_loss": None, "cancel_order_by_oid": None, "place_take_profit_limit": 1},
             {"stop_loss_oid": _ABSENT, "tp_oids": [0, 9101, 7003], "tp_errors": _FALSY}),
            ("reports_a_filled_tier",
             {"stop_loss_atr_mult": 0, "tp_tiers": three_tiers, "tp_oids": [7001, 0, 0],
              "tp_armed_tiers": [True, True, True], "open_oids": set(),
              "fill_lookup_by_oid": {7001: {"fee": 0.05, "closed_pnl": 12.0, "count": 1}}},
             {"place_take_profit_limit": None},
             {"tp_filled_externally": [True, False, False], "tp_oids": [0, 0, 0]}),
            ("leaves_an_unreadable_tier_untouched",
             {"stop_loss_atr_mult": 0, "tp_tiers": three_tiers, "tp_oids": [7001, 0, 0],
              "tp_armed_tiers": [True, True, True], "open_orders_error": "userOpenOrders failed"},
             {"place_take_profit_limit": None},
             {"open_order_check_error": "userOpenOrders failed", "tp_oids": [7001, 0, 0]}),
            ("reports_an_immediate_fill",
             {"stop_loss_atr_mult": 0, "tp_tiers": three_tiers, "tp_oids": [0, 0, 0],
              "tp_armed_tiers": [False, True, True], "open_oids": set(),
              "place_responses": {"tp1": {
                  "status": "ok",
                  "response": {"type": "order", "data": {"statuses": [{"filled": {"oid": 9101}}]}},
              }}},
             {"place_take_profit_limit": 1},
             {"tp_filled_immediately": [True, False, False], "tp_oids": [0, 0, 0]}),
        ])

    def test_sync_protection_surplus_cancel_verifies_first(self, subtests):
        size_zero = {"size": 0, "stop_loss_atr_mult": 0, "cancel_tp_oids": [303]}
        placements = {"place_stop_loss": None, "place_take_profit_limit": None}
        cancelled = call("ETH", 303)

        def tp_cancel(failed=None, filled=None, not_open=None):
            return {"tp_cancel_failed_oids": failed, "tp_cancel_filled_oids": filled,
                    "tp_cancel_not_open_oids": not_open}

        self._check_sync_cases(subtests, [
            ("open_cancel_ok",
             {**size_zero, "open_oids": {303}, "cancel_response": _CANCEL_OK_RESPONSE, "fill_lookup_by_oid": {}},
             {**placements, "cancel_order_by_oid": cancelled},
             tp_cancel()),
            ("open_cancel_rejected",
             {**size_zero, "open_oids": {303}, "cancel_response": _CANCEL_REJECTED_RESPONSE, "fill_lookup_by_oid": {}},
             {**placements, "cancel_order_by_oid": cancelled},
             tp_cancel(failed=[303])),
            ("not_open_unfilled",
             {**size_zero, "open_oids": set(), "fill_lookup_by_oid": {}},
             {**placements, "cancel_order_by_oid": None},
             tp_cancel(not_open=[303])),
            ("not_open_filled",
             {**size_zero, "open_oids": set(),
              "fill_lookup_by_oid": {303: {"fee": 0.05, "closed_pnl": 1.0, "count": 1}}},
             {**placements, "cancel_order_by_oid": None},
             tp_cancel(filled=[303])),
            ("open_orders_unreadable",
             {**size_zero, "open_orders_error": "userOpenOrders failed", "fill_lookup_by_oid": {}},
             {**placements, "cancel_order_by_oid": None},
             tp_cancel(failed=[303])),
            ("cancel_exception_is_reported_failed",
             {**size_zero, "tp1_atr_mult": 0.0, "tp1_fraction": 0.0, "tp2_atr_mult": 0.0,
              "open_oids": {303}, "cancel_side_effect": Exception("rpc down")},
             {"cancel_order_by_oid": cancelled},
             {"tp_cancel_failed_oids": [303]}),
            ("filled_surplus_with_open_position_skips_cancel",
             {"cancel_tp_oids": [303], "open_oids": set(),
              "fill_lookup_by_oid": {303: {"fee": 0.05, "closed_pnl": 25.0, "count": 1}}},
             {"cancel_order_by_oid": None},
             {"tp_cancel_filled_oids": [303], "tp_cancel_failed_oids": _FALSY}),
            ("surplus_cancel_runs_when_size_zero",
             {"size": 0, "tp_tiers": [(1.0, 0.5), (2.0, 1.0)], "cancel_tp_oids": [303], "open_oids": {303}},
             {"cancel_order_by_oid": cancelled},
             {}),
        ])

    def test_stop_loss_outcome_gates_the_take_profits(self, subtests):
        two_tiers = [(1.0, 0.5), (2.0, 1.0)]
        self._check_sync_cases(subtests, [
            ("sl_filled_at_submit_skips_tp_placement",
             {"tp_tiers": two_tiers, "place_responses": {"sl": {
                 "status": "ok",
                 "response": {"type": "order", "data": {"statuses": [
                     {"filled": {"oid": 67890, "avgPx": "1980"}}
                 ]}},
             }}},
             {"place_take_profit_limit": None},
             {"stop_loss_filled_immediately": True, "tp_oids": _FALSY}),
            ("resting_still_places_and_records_tps",
             {"tp_tiers": two_tiers},
             {"place_take_profit_limit": 2},
             {"tp_oids": _Len(2)}),
        ])

    def test_existing_protection_oids_reconcile_before_any_placement(self, subtests):
        three_tiers = [
            {"atr_multiple": 1.0, "close_fraction": 0.5},
            {"atr_multiple": 2.0, "close_fraction": 0.8},
            {"atr_multiple": 3.0, "close_fraction": 1.0},
        ]
        self._check_sync_cases(subtests, [
            ("existing_oid_still_open_returns_same_oid",
             {"tp1_oid": 200, "tp2_oid": 300, "sl_oid": 100, "open_oids": {100, 200, 300}},
             {"place_take_profit_limit": None, "place_stop_loss": None, "lookup_fill_fee_by_oid": None},
             {"tp1_oid": 200, "tp2_oid": 300, "stop_loss_oid": 100}),
            ("missing_oid_with_no_fill_places_replacement",
             {"tp1_oid": 200, "tp2_oid": 300, "open_oids": set(), "fill_lookup_by_oid": {}},
             {"lookup_fill_fee_by_oid": "called", "place_take_profit_limit": 2},
             {"tp1_oid": _PRESENT, "tp2_oid": _PRESENT,
              "tp1_filled_externally": _FALSY, "tp2_filled_externally": _FALSY}),
            ("missing_oid_with_fill_marks_externally_filled",
             {"tp1_oid": 200, "tp2_oid": 300, "open_oids": set(),
              "fill_lookup_by_oid": {200: {"fee": 0.05, "closed_pnl": 25.0, "count": 1}}},
             {"place_take_profit_limit": 1},
             {"tp1_filled_externally": True, "tp1_fill": _PRESENT, ("tp1_fill", "fee"): 0.05,
              "tp2_filled_externally": _FALSY}),
            ("zero_oid_armed_true_does_not_re_place_consumed_tp1",
             {"size": 0.22, "tp_oids": [0, 300], "tp_armed_tiers": [True, True], "open_oids": {300, 9000},
              "sl_oid": 9000},
             {"place_take_profit_limit": None},
             {"tp_oids": [0, 300], "tp2_oid": 300, "tp1_oid": _ABSENT}),
            ("three_tiers_detects_middle_oid_filled_externally",
             {"tp_tiers": three_tiers, "tp_oids": [100, 200, 300], "open_oids": {100, 300},
              "fill_lookup_by_oid": {200: {"fee": 0.01, "closed_pnl": 7.0, "count": 1}}},
             {"place_take_profit_limit": None},
             {"tp_oids": [100, 0, 300], "tp_filled_externally": [False, True, False],
              ("tp_fills", 1, "closed_pnl"): 7.0}),
            ("sl_skips_force_replace_when_size_zero",
             {"size": 0, "cancel_tp_oids": [303], "open_oids": {100}, "sl_oid": 100, "force_sl_replace": True},
             {"place_stop_loss": None},
             {"stop_loss_oid": 100}),
            ("open_orders_fetch_failure_defers_replacement",
             {"sl_oid": 100, "tp1_oid": 200, "tp2_oid": 300,
              "open_orders_error": RuntimeError("indexer down")},
             {"place_take_profit_limit": None, "place_stop_loss": None},
             {"open_order_check_error": "indexer down"}),
        ])

    def test_reconcile_fill_hints(self, subtests):
        legacy = {"tp1_oid": 9100, "tp2_oid": 9101, "sl_oid": 0, "open_oids": {9100}}
        self._check_sync_cases(subtests, [
            ("filled_hint_skips_lookup_fill_fee_by_oid",
             {**legacy, "fill_lookup_by_oid": {}, "reconcile_fill_hints_json": json.dumps(
                 [{"oid": 9101, "filled": True, "fee": 0.02, "closed_pnl": 1.5, "count": 2}])},
             {"lookup_fill_fee_by_oid": None},
             {("tp_oids", 1): 0, ("tp_filled_externally", 1): True, ("tp_fills", 1, "fee"): 0.02,
              ("tp_fills", 1, "closed_pnl"): 1.5}),
            ("filled_false_still_queries_userfills",
             {**legacy, "fill_lookup_by_oid": {9101: {"fee": 0.03, "closed_pnl": 2.0, "count": 1}},
              "reconcile_fill_hints_json": json.dumps([{"oid": 9101, "filled": False}])},
             {"lookup_fill_fee_by_oid": "called"},
             {("tp_filled_externally", 1): True, ("tp_fills", 1, "fee"): 0.03}),
            ("malformed_json_queries_userfills",
             {**legacy, "fill_lookup_by_oid": {9101: {"fee": 0.04, "closed_pnl": 3.0, "count": 1}},
              "reconcile_fill_hints_json": "{not-json"},
             {"lookup_fill_fee_by_oid": "called"},
             {("tp_filled_externally", 1): True}),
            ("extra_oid_does_not_skip_other_oid_lookup",
             {**legacy, "fill_lookup_by_oid": {9101: {"fee": 0.05, "closed_pnl": 4.0, "count": 1}},
              "reconcile_fill_hints_json": json.dumps([{"oid": 1, "filled": True, "fee": 0.0, "count": 0}])},
             {"lookup_fill_fee_by_oid": _FirstArgSeen(9101)},
             {("tp_filled_externally", 1): True}),
        ])

    def test_tier_sizes_sum_to_the_floored_position(self, subtests):
        drifted = 0.011 - 0.010
        assert drifted < 0.001
        cases = [
            ("three_tiers_places_incremental_sizes", 10.0, [(1.0, 0.5), (2.0, 0.8), (3.0, 1.0)],
             {"tp_oids": _Len(3), "tp_pxs": [2020.0, 2040.0, 2060.0]}, [5.0, 3.0, 2.0], None, 3),
            ("final_tier_fraction_is_coerced_to_remaining_size", 10.0, [(1.0, 0.5), (2.0, 0.7)],
             {"tp_oids": _TRUTHY}, [5.0, 5.0], None, None),
            ("floor_residual_absorbed_by_final_tier", 0.003, [(1.0, 0.5), (2.0, 1.0)],
             {"tp_oids": _TRUTHY}, [0.001, 0.002], 0.003, None),
            ("float_drift_below_lot_boundary_normalizes", drifted, [(1.0, 0.5), (2.0, 1.0)],
             {"tp_oids": _TRUTHY}, None, 0.001, None),
            ("three_tier_non_uniform_flooring_zero_residual", 0.007, [(1.0, 0.3), (2.0, 0.6), (3.0, 1.0)],
             {"tp_oids": _TRUTHY}, None, 0.007, None),
        ]
        for label, size, tiers, want_out, want_sizes, want_sum, want_count in cases:
            with subtests.test(case=label):
                out, adapter = self._run_sync(
                    size=size,
                    tp_tiers=[{"atr_multiple": m, "close_fraction": f} for m, f in tiers],
                    tp_oids=[0] * len(tiers),
                    open_oids=set(),
                )
                _assert_out(out, want_out)
                sizes = [c.args[1] for c in adapter.place_take_profit_limit.call_args_list]
                if want_sizes is not None:
                    assert sizes == pytest.approx(want_sizes)
                if want_sum is not None:
                    assert sum(sizes) == pytest.approx(want_sum)
                if want_count is not None:
                    assert adapter.place_take_profit_limit.call_count == want_count

    def test_unplaceable_tier_blocks_place_nothing(self, subtests):
        cases = [
            ("non_increasing_sorted_tiers_are_rejected", 1.0, [(1.0, 0.5), (0.5, 0.7)], {"tp_oids": _ABSENT}),
            ("single_tier_config_is_rejected", 1.0, [(1.0, 1.0)], {"tp_oids": _ABSENT}),
            ("size_rounds_to_zero_skips_tier_block", 0.0004, [(1.0, 0.5), (2.0, 1.0)],
             {"tp_oids": _ABSENT, "tp_pxs": _ABSENT}),
        ]
        for label, size, tiers, want_out in cases:
            with subtests.test(case=label):
                out, adapter = self._run_sync(
                    size=size,
                    tp_tiers=[{"atr_multiple": m, "close_fraction": f} for m, f in tiers],
                    tp_oids=[0] * len(tiers),
                    open_oids=set(),
                )
                adapter.place_take_profit_limit.assert_not_called()
                _assert_out(out, want_out)


class TestComputeTPTierSizes:

    @staticmethod
    def _floor3(sz):
        return math.floor(sz * 1000) / 1000

    def _load(self):
        mod, spec = _load_check_module()
        spec.loader.exec_module(mod)
        return mod

    def test_compute_tp_tier_sizes(self, subtests):
        with subtests.test(case="never_sums_above_the_floored_input"):
            mod = self._load()

            def floor(sz):
                return math.floor(sz * 100) / 100.0

            sizes = mod.compute_tp_tier_sizes(8.127, [(1.0, 0.5), (2.0, 1.0)], floor)
            assert sum(sizes) <= floor(8.127) + 1e-9
        cases = [
            (0.0, [(1.0, 0.5), (2.0, 1.0)], [0.0, 0.0]),
            (-0.5, [(1.0, 0.5), (2.0, 1.0)], [0.0, 0.0]),
            (1.0, [], []),
            (0.003, [(1.0, 0.5), (2.0, 1.0)], [0.001, 0.002]),
            (0.007, [(1.0, 0.3), (2.0, 0.6), (3.0, 1.0)], [0.002, 0.002, 0.003]),
            (10.0, [(1.0, 0.5), (2.0, 1.0)], [5.0, 5.0]),
            (0.5, [(1.0, 0.5), (2.0, 1.0)], [0.25, 0.25]),
        ]
        mod = self._load()
        for size, tiers, expected in cases:
            label = (size, tiers, expected)
            sizes = mod.compute_tp_tier_sizes(size, tiers, self._floor3)
            assert sizes == pytest.approx(expected), label
            if expected:
                assert sum(sizes) == pytest.approx(sum(expected)), label


class TestProtectionSyncStopLossTriggerContract:

    def _run_sync(self, *, open_oids, stop_loss_oid, force_sl_replace, place_response,
                  cancel_response=_CANCEL_OK_RESPONSE):
        mod, spec = _load_check_module()
        spec.loader.exec_module(mod)

        mock_adapter_cls = MagicMock()
        adapter = MagicMock()
        mock_adapter_cls.return_value = adapter
        if isinstance(open_oids, list) and open_oids and isinstance(open_oids[0], set):
            adapter.open_order_oids.side_effect = [set(s) for s in open_oids]
        else:
            adapter.open_order_oids.return_value = set(open_oids)
        adapter.round_perps_trigger_px.side_effect = lambda _sym, px: round(px, 2)
        adapter.round_size.side_effect = lambda _sym, sz: sz
        adapter.place_stop_loss.return_value = place_response
        if isinstance(cancel_response, Exception):
            adapter.cancel_order_by_oid.side_effect = cancel_response
        else:
            adapter.cancel_order_by_oid.return_value = cancel_response

        captured = StringIO()
        import builtins
        original_import = builtins.__import__

        def mock_import(name, *args, **kwargs):
            if name == "adapter":
                fake_mod = MagicMock()
                fake_mod.HyperliquidExchangeAdapter = mock_adapter_cls
                return fake_mod
            return original_import(name, *args, **kwargs)

        with patch("builtins.__import__", side_effect=mock_import):
            with patch("sys.stdout", captured):
                mod.run_sync_protection(
                    "ETH", "long", 1.0, 2400.0, 30.0, "live",
                    stop_loss_atr_mult=2.5,
                    stop_loss_oid=stop_loss_oid,
                    force_sl_replace=force_sl_replace,
                )
        return json.loads(captured.getvalue()), adapter

    @staticmethod
    def _resting(oid):
        return {"status": "ok", "response": {"data": {"statuses": [{"resting": {"oid": oid}}]}}}

    def test_stop_loss_trigger_px_is_reported_only_for_a_placed_stop(self, subtests):
        weird = {"status": "weird"}
        cases = [
            ("echoed_oid_reports_no_trigger_price",
             {"open_oids": [4242], "stop_loss_oid": 4242, "force_sl_replace": False,
              "place_response": self._resting(9001)},
             {"place_stop_loss": None},
             {"stop_loss_oid": 4242, "stop_loss_trigger_px": _ABSENT}),
            ("force_replace_that_rests_reports_the_placed_trigger",
             {"open_oids": [4242], "stop_loss_oid": 4242, "force_sl_replace": True,
              "place_response": self._resting(9002)},
             {"place_stop_loss": "once"},
             {"stop_loss_oid": 9002, "stop_loss_trigger_px": pytest.approx(2325.0)}),
            ("force_replace_whose_placement_fails_reports_no_trigger_price",
             {"open_oids": [4242], "stop_loss_oid": 4242, "force_sl_replace": True,
              "place_response": {"status": "err", "response": "open order limit"}},
             {},
             {"stop_loss_trigger_px": _ABSENT, "stop_loss_error": _TRUTHY}),
            ("fresh_placement_reports_the_placed_trigger",
             {"open_oids": [], "stop_loss_oid": 0, "force_sl_replace": False,
              "place_response": self._resting(9003)},
             {"place_stop_loss": "once"},
             {"stop_loss_oid": 9003, "stop_loss_trigger_px": pytest.approx(2325.0)}),
            ("unreadable_placement_resolved_to_resting_reports_no_error",
             {"open_oids": [{4242}, {9004}], "stop_loss_oid": 4242, "force_sl_replace": True,
              "place_response": weird},
             {},
             {"stop_loss_oid": 9004, "stop_loss_error": _ABSENT}),
            ("unreadable_placement_ambiguous_diff_reports_no_warning_error",
             {"open_oids": [{4242}, {9004, 9005}], "stop_loss_oid": 4242, "force_sl_replace": True,
              "place_response": weird},
             {},
             {"stop_loss_outcome_unknown": True, "stop_loss_error": _ABSENT}),
            ("unreadable_placement_nothing_resting_keeps_the_error",
             {"open_oids": [{4242}, set()], "stop_loss_oid": 4242, "force_sl_replace": True,
              "place_response": weird},
             {},
             {"stop_loss_oid": _ABSENT, "stop_loss_outcome_unknown": _ABSENT,
              "stop_loss_error": _Has("no usable status")}),
        ]
        for label, kwargs, want_calls, want_out in cases:
            with subtests.test(case=label):
                out, adapter = self._run_sync(**kwargs)
                _assert_calls(adapter, want_calls)
                _assert_out(out, want_out)

    def test_force_replace_cancel_failure_defers_replacement_and_reports_cancel_error(self):
        for label, cancel_response in (("rejected", _CANCEL_REJECTED_RESPONSE), ("rpc_down", RuntimeError("rpc down"))):
            with _case("test_force_replace_cancel_failure_reports_cancel_error", label):
                out, adapter = self._run_sync(
                    open_oids=[4242], stop_loss_oid=4242, force_sl_replace=True,
                    place_response=self._resting(9002), cancel_response=cancel_response,
                )
                adapter.place_stop_loss.assert_not_called()
                assert out.get("cancel_stop_loss_succeeded") is False
                assert out["cancel_stop_loss_error"]
                assert out["cancel_stop_loss_error"] == out["stop_loss_error"]
            if label == "rejected":
                assert "already filled" in out["stop_loss_error"], "test_force_replace_cancel_rejected_defers_replacement"
                assert "stop_loss_oid" not in out, "test_force_replace_cancel_rejected_defers_replacement"


class TestSyncProtectionPreservedTrigger:

    @staticmethod
    def _resting(oid):
        return {"status": "ok", "response": {"data": {"statuses": [{"resting": {"oid": oid}}]}}}

    def _run(self, *, trigger, preserve, side="long", decimals=2, tick=0.01,
             open_oids=(4242,), stop_loss_oid=4242, force=True, place_response=None):
        import builtins
        mod, spec = _load_check_module()
        spec.loader.exec_module(mod)

        mock_adapter_cls = MagicMock()
        adapter = MagicMock()
        mock_adapter_cls.return_value = adapter
        adapter.open_order_oids.return_value = set(open_oids)
        adapter.round_perps_trigger_px.side_effect = lambda _sym, px: round(px, decimals)
        adapter.perps_trigger_px_tick.return_value = tick
        adapter.round_size.side_effect = lambda _sym, sz: sz
        adapter.place_stop_loss.return_value = place_response or self._resting(9002)
        adapter.cancel_order_by_oid.return_value = _CANCEL_OK_RESPONSE

        captured = StringIO()
        original_import = builtins.__import__

        def mock_import(name, *args, **kwargs):
            if name == "adapter":
                fake_mod = MagicMock()
                fake_mod.HyperliquidExchangeAdapter = mock_adapter_cls
                return fake_mod
            return original_import(name, *args, **kwargs)

        exit_code = None
        with patch("builtins.__import__", side_effect=mock_import):
            with patch("sys.stdout", captured):
                try:
                    mod.run_sync_protection(
                        "ETH", side, 1.0, 2400.0, 30.0, "live",
                        stop_loss_atr_mult=2.5,
                        stop_loss_oid=stop_loss_oid,
                        force_sl_replace=force,
                        stop_loss_trigger_px=trigger,
                        preserve_moved_stop=preserve,
                    )
                except SystemExit as e:
                    exit_code = e.code
        return json.loads(captured.getvalue()), adapter, mock_adapter_cls, exit_code

    def test_moved_trigger_outcomes(self, subtests):
        cases = [
            ("moved_trigger_replaces_the_label_after_a_confirmed_cancel",
             {"trigger": 2400.0, "preserve": True}, None,
             {"cancel_order_by_oid": "once", "place_stop_loss": "once"}, 2400.0,
             {"stop_loss_oid": 9002, "stop_loss_trigger_px": pytest.approx(2400.0)}),
            ("moved_trigger_places_a_missing_stop_at_the_moved_price",
             {"trigger": 2410.0, "preserve": True, "open_oids": (), "stop_loss_oid": 0, "force": False}, _UNSET,
             {"cancel_order_by_oid": None}, 2410.0,
             {"stop_loss_trigger_px": pytest.approx(2410.0)}),
            ("resting_moved_stop_is_kept_without_replacement",
             {"trigger": 2400.0, "preserve": True, "force": False}, _UNSET,
             {"cancel_order_by_oid": None, "place_stop_loss": None}, None,
             {"stop_loss_oid": 4242}),
        ]
        for label, kwargs, want_code, want_calls, want_placed, want_out in cases:
            with subtests.test(case=label):
                out, adapter, _, code = self._run(**kwargs)
                if want_code is not _UNSET:
                    assert code is want_code
                _assert_calls(adapter, want_calls)
                if want_placed is not None:
                    assert adapter.place_stop_loss.call_args.args[2] == pytest.approx(want_placed)
                _assert_out(out, want_out)

    def test_invalid_trigger_fails_before_any_exchange_call(self, subtests):
        cases = [(trigger, True) for trigger in (None, 0.0, -1.0, float("nan"), float("inf"))]
        cases += [(trigger, False) for trigger in (-1.0, float("nan"))]
        for trigger, preserve in cases:
            with subtests.test(trigger=trigger, preserve=preserve):
                out, adapter, adapter_cls, code = self._run(trigger=trigger, preserve=preserve)
                assert code == 1
                adapter_cls.assert_not_called()
                adapter.place_stop_loss.assert_not_called()
                if preserve:
                    assert "stop-loss-trigger-px" in out["error"]
                    adapter.cancel_order_by_oid.assert_not_called()

    def test_venue_rounding_never_loosens_a_moved_trigger(self):
        for side, requested in (("long", 2400.004), ("short", 2399.996)):
            with _case(side, requested):
                _, adapter, _, _ = self._run(trigger=requested, preserve=True, side=side)
                placed = adapter.place_stop_loss.call_args.args[2]
                if side == "long":
                    assert placed >= requested
                else:
                    assert placed <= requested
                assert placed == pytest.approx(requested, abs=0.011)

    def test_unroundable_moved_trigger_never_loosens_the_stop(self, subtests):
        loosening = _Has("without loosening")
        cases = [
            ("keeps_the_resting_stop", {},
             {"stop_loss_oid": _ABSENT, "cancel_stop_loss_succeeded": False,
              "cancel_stop_loss_error": loosening, "stop_loss_error": loosening}),
            ("without_force_echoes_the_resting_stop", {"force": False},
             {"stop_loss_oid": 4242, "cancel_stop_loss_error": _ABSENT, "stop_loss_error": loosening}),
            ("with_no_resting_stop_places_nothing", {"open_oids": ()},
             {"stop_loss_oid": _ABSENT, "cancel_stop_loss_error": _ABSENT, "stop_loss_error": loosening}),
        ]
        for label, kwargs, want_out in cases:
            with subtests.test(case=label):
                out, adapter, _, code = self._run(trigger=2400.004, preserve=True, tick=0.0, **kwargs)
                assert code is None
                adapter.cancel_order_by_oid.assert_not_called()
                adapter.place_stop_loss.assert_not_called()
                _assert_out(out, want_out)

    def test_parser_routes_the_moved_stop_flags(self, subtests):
        base = [
            "check_hyperliquid.py", "--sync-protection", "--symbol=ETH", "--side=long",
            "--size=1", "--avg-cost=2400", "--entry-atr=30", "--stop-loss-atr-mult=2.5",
        ]
        cases = [
            ("moved_stop_flags", base + ["--stop-loss-trigger-px=2400.5", "--preserve-moved-stop", "--mode=live"],
             pytest.approx(2400.5), True),
            ("defaults_keep_the_label_contract", base + ["--mode=live"], None, False),
        ]
        for label, argv, want_px, want_preserve in cases:
            with subtests.test(case=label):
                mod, spec = _load_check_module()
                spec.loader.exec_module(mod)
                with patch.object(mod, "run_sync_protection") as run, patch.object(sys, "argv", argv):
                    mod.main()
                kwargs = run.call_args.kwargs
                if want_px is None:
                    assert kwargs["stop_loss_trigger_px"] is None
                else:
                    assert kwargs["stop_loss_trigger_px"] == want_px
                assert kwargs["preserve_moved_stop"] is want_preserve

    def test_probe_only_exits_before_any_exchange_call(self):
        import subprocess
        script = os.path.join(os.path.dirname(os.path.abspath(__file__)), "check_hyperliquid.py")
        proc = subprocess.run(
            [sys.executable, script, "--sync-protection", "--symbol=BTC", "--side=long",
             "--size=0.01", "--avg-cost=1", "--entry-atr=1", "--stop-loss-atr-mult=1",
             "--stop-loss-trigger-px=1", "--preserve-moved-stop", "--mode=live", "--probe-only"],
            capture_output=True, text=True, timeout=60,
        )
        assert proc.returncode == 0, proc.stderr
        assert proc.stdout == ""

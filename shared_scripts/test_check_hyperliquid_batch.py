
import importlib.util
import io
import json
import math
import os
import sys
import types

import pytest


SCRIPT_PATH = os.path.join(os.path.dirname(os.path.abspath(__file__)), "check_hyperliquid.py")


def _load_check_module():
    spec = importlib.util.spec_from_file_location("_check_hyperliquid_batch_under_test", SCRIPT_PATH)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


@pytest.fixture(scope="module")
def mod():
    return _load_check_module()


def _candles(n=160, start_ms=1_700_000_000_000, step_ms=3_600_000):
    out = []
    price = 100.0
    for i in range(n):
        drift = 0.35 if i < n // 2 else -0.2
        price = price + drift + (0.6 if i % 7 == 0 else -0.25)
        high = price + 1.2
        low = price - 1.1
        out.append([start_ms + i * step_ms, price - 0.3, high, low, price, 1000.0 + i])
    return out


class FakeAdapter:

    def __init__(self, candles=None, spot_price=0.0, ohlcv_error=None):
        self._candles = candles if candles is not None else _candles()
        self._spot_price = spot_price
        self._ohlcv_error = ohlcv_error
        self.ohlcv_calls = []
        self.spot_price_calls = 0
        self.funding_rate_calls = 0
        self.funding_range_calls = 0

    def get_ohlcv(self, symbol, interval="1h", limit=200):
        self.ohlcv_calls.append((symbol, interval, limit))
        if self._ohlcv_error is not None:
            raise self._ohlcv_error
        return list(self._candles)

    def get_spot_price(self, symbol):
        self.spot_price_calls += 1
        return self._spot_price

    def get_funding_rate(self, symbol):
        self.funding_rate_calls += 1
        return 0.0001

    def get_funding_history(self, symbol, days=7):
        return [{"rate": 0.0001}, {"rate": 0.0002}]

    def get_funding_history_range(self, symbol, start_ms):
        self.funding_range_calls += 1
        return [{"time": start_ms, "rate": 0.0001}]


def _slot(slot_id, strategy, **overrides):
    slot = {
        "id": slot_id,
        "strategy": strategy,
        "mode": "paper",
        "htf_filter": False,
        "open_strategy": strategy,
        "close_strategies": None,
        "regime_atr_window": "",
        "position_side": "",
        "position_ctx": None,
    }
    slot.update(overrides)
    return slot


def _shared(mod, adapter, **overrides):
    kwargs = {
        "adapter": adapter,
        "ohlcv_limit": 200,
        "atr_method": "simple",
        "mark_price": 25_000.0,
    }
    kwargs.update(overrides)
    return mod.build_shared_signal_state("BTC", "1h", **kwargs)


def _strip_volatile(result):
    out = dict(result)
    out.pop("timestamp", None)
    out.pop("id", None)
    return out


SLOT_MATRIX = [
    _slot("hl-a", "breakout"),
    _slot("hl-b", "momentum_pro", mode="live"),
    _slot(
        "hl-c",
        "breakout",
        position_side="long",
        position_ctx={"side": "long", "avg_cost": 90.0, "current_quantity": 1.5,
                      "initial_quantity": 2.0, "entry_atr": 2.0},
        close_strategies="atr_stop",
        regime_atr_window="macro",
    ),
]


def test_batched_slots_match_their_solo_evaluation(mod):
    batch_adapter = FakeAdapter()
    envelope, exit_code = mod.run_batch_signal_check(
        "BTC", "1h", SLOT_MATRIX,
        ohlcv_limit=200, atr_method="simple", mark_price=25_000.0,
        adapter=batch_adapter,
    )
    assert exit_code == 0, envelope
    assert envelope["error"] == "" and envelope["error_scope"] == ""
    assert [r["id"] for r in envelope["results"]] == ["hl-a", "hl-b", "hl-c"]

    for slot, batched in zip(SLOT_MATRIX, envelope["results"]):
        solo_shared = _shared(mod, FakeAdapter())
        solo = mod.evaluate_signal_slot(solo_shared, slot)
        assert _strip_volatile(batched) == _strip_volatile(solo), slot["id"]


def test_slot_timestamps_are_rfc3339(mod):
    from datetime import datetime

    envelope, _ = mod.run_batch_signal_check(
        "BTC", "1h", SLOT_MATRIX, mark_price=25_000.0, adapter=FakeAdapter())
    datetime.fromisoformat(envelope["timestamp"])
    for result in envelope["results"]:
        datetime.fromisoformat(result["timestamp"])


def test_heterogeneous_slots_keep_their_own_values(mod):
    envelope, exit_code = mod.run_batch_signal_check(
        "BTC", "1h", SLOT_MATRIX, mark_price=25_000.0, adapter=FakeAdapter())
    assert exit_code == 0
    by_id = {r["id"]: r for r in envelope["results"]}
    assert by_id["hl-a"]["mode"] == "paper"
    assert by_id["hl-b"]["mode"] == "live"
    assert by_id["hl-a"]["strategy"] == "breakout"
    assert by_id["hl-b"]["strategy"] == "momentum_pro"
    assert by_id["hl-c"]["close_fraction"] >= 0.0
    assert by_id["hl-a"].get("close_fraction", 0.0) == 0.0


def test_shared_mark_price_is_applied_to_every_slot(mod):
    envelope, _ = mod.run_batch_signal_check(
        "BTC", "1h", SLOT_MATRIX, mark_price=25_000.0, adapter=FakeAdapter())
    for result in envelope["results"]:
        assert result["price"] == 25_000.0


def test_spot_price_fallback_used_once_when_mark_absent(mod):
    adapter = FakeAdapter(spot_price=31_337.0)
    envelope, _ = mod.run_batch_signal_check(
        "BTC", "1h", SLOT_MATRIX, mark_price=0.0, adapter=adapter)
    assert adapter.spot_price_calls == 1
    for result in envelope["results"]:
        assert result["price"] == 31_337.0


def test_slot_cannot_mutate_the_shared_frame(mod):
    adapter = FakeAdapter()
    shared = _shared(mod, adapter)
    base_columns = list(shared["df"].columns)

    deps = mod._signal_check_deps()
    real_apply = deps.apply_strategy

    def mutating_apply(name, df, params=None):
        df["leaked_column"] = 1.0
        return real_apply(name, df, params)

    mutating_deps = types.SimpleNamespace(**vars(deps))
    mutating_deps.apply_strategy = mutating_apply

    mod.evaluate_signal_slot(shared, _slot("hl-mut", "breakout"), deps=mutating_deps)
    assert list(shared["df"].columns) == base_columns

    peer = mod.evaluate_signal_slot(shared, _slot("hl-peer", "breakout"))
    solo = mod.evaluate_signal_slot(_shared(mod, FakeAdapter()), _slot("hl-peer", "breakout"))
    assert "leaked_column" not in peer["indicators"]
    assert _strip_volatile(peer) == _strip_volatile(solo)


def test_candles_are_fetched_once_per_batch(mod):
    adapter = FakeAdapter()
    mod.run_batch_signal_check("BTC", "1h", SLOT_MATRIX, mark_price=1.0, adapter=adapter)
    assert adapter.ohlcv_calls == [("BTC", "1h", 200)]


def test_htf_frames_are_fetched_once_and_handed_out_as_copies(mod):
    adapter = FakeAdapter()
    shared = _shared(mod, adapter)
    first = mod._shared_htf_frame(shared, "BTC", "4h", 100)
    second = mod._shared_htf_frame(shared, "BTC", "4h", 100)
    assert adapter.ohlcv_calls == [("BTC", "1h", 200), ("BTC", "4h", 100)]
    assert first is not second
    first["annotation"] = 1.0
    third = mod._shared_htf_frame(shared, "BTC", "4h", 100)
    assert "annotation" not in third.columns


def test_funding_fetches_are_memoized_across_slots(mod):
    adapter = FakeAdapter()
    slots = [_slot("hl-f1", "delta_neutral_funding"), _slot("hl-f2", "delta_neutral_funding")]
    envelope, _ = mod.run_batch_signal_check(
        "BTC", "1h", slots, mark_price=1.0, adapter=adapter)
    assert adapter.funding_rate_calls == 1
    assert len(envelope["results"]) == 2


def test_one_failing_slot_does_not_disturb_its_peers(mod):
    slots = [
        _slot("hl-ok-1", "breakout"),
        _slot("hl-bad", "no_such_strategy_1442", open_strategy="no_such_strategy_1442"),
        _slot("hl-ok-2", "momentum_pro"),
    ]
    envelope, exit_code = mod.run_batch_signal_check(
        "BTC", "1h", slots, mark_price=1.0, adapter=FakeAdapter())
    assert exit_code == 1
    assert envelope["error"] == "" and envelope["error_scope"] == ""
    by_id = {r["id"]: r for r in envelope["results"]}
    assert by_id["hl-bad"]["error"]
    assert by_id["hl-bad"]["signal"] == 0
    assert "error" not in by_id["hl-ok-1"]
    assert "error" not in by_id["hl-ok-2"]

    solo = mod.evaluate_signal_slot(_shared(mod, FakeAdapter(), mark_price=1.0), slots[0])
    assert _strip_volatile(by_id["hl-ok-1"]) == _strip_volatile(solo)


def test_shared_state_failure_returns_a_distinct_sentinel(mod):
    adapter = FakeAdapter(ohlcv_error=RuntimeError("upstream 429"))
    envelope, exit_code = mod.run_batch_signal_check(
        "BTC", "1h", SLOT_MATRIX, mark_price=1.0, adapter=adapter)
    assert exit_code == 1
    assert envelope["error_scope"] == "shared_state"
    assert "upstream 429" in envelope["error"]
    assert envelope["results"] == []


def test_insufficient_candles_is_a_shared_state_failure(mod):
    adapter = FakeAdapter(candles=_candles(n=10))
    envelope, exit_code = mod.run_batch_signal_check(
        "BTC", "1h", SLOT_MATRIX, mark_price=1.0, adapter=adapter)
    assert exit_code == 1
    assert envelope["error_scope"] == "shared_state"
    assert envelope["error"] == "Insufficient data: 10 candles"
    assert envelope["results"] == []


def test_build_shared_signal_state_raises_typed_errors(mod):
    with pytest.raises(mod.InsufficientCandlesError):
        mod.build_shared_signal_state("BTC", "1h", adapter=FakeAdapter(candles=_candles(n=5)))
    with pytest.raises(mod.SharedSignalStateError):
        mod.build_shared_signal_state("BTC", "1h")


def test_parse_batch_slots_accepts_the_documented_envelope(mod):
    raw = json.dumps({"v": 1, "slots": [
        {"id": "hl-a", "strategy": "breakout",
         "strategy_refs": {"open": {"name": "breakout", "params": {"lookback": 30}},
                           "closes": [{"name": "atr_stop", "params": {"atr_multiple": 2.0}}]}},
    ]})
    slots = mod.parse_batch_slots(raw)
    assert len(slots) == 1
    assert slots[0]["open_strategy"] == "breakout"
    assert slots[0]["close_strategies"] == "atr_stop"
    assert slots[0]["params"] == {"lookback": 30}
    assert slots[0]["close_params_by_name"] == {"atr_stop": {"atr_multiple": 2.0}}


@pytest.mark.parametrize("payload,fragment", [
    ({"v": 2, "slots": [{"id": "a", "strategy": "breakout"}]}, "protocol version"),
    ({"v": 1, "slots": []}, "non-empty"),
    ({"v": 1, "slots": [{"strategy": "breakout"}]}, "missing 'id'"),
    ({"v": 1, "slots": [{"id": "a", "strategy": "breakout"},
                        {"id": "a", "strategy": "breakout"}]}, "duplicate slot id"),
    ({"v": 1, "slots": [{"id": "a"}]}, "missing 'strategy'"),
])
def test_parse_batch_slots_rejects_bad_envelopes(mod, payload, fragment):
    with pytest.raises(ValueError) as exc:
        mod.parse_batch_slots(json.dumps(payload))
    assert fragment in str(exc.value)


def _run_main(mod, monkeypatch, argv, stdin_text, adapter):
    fake_module = types.ModuleType("adapter")
    fake_module.HyperliquidExchangeAdapter = lambda *a, **kw: adapter
    monkeypatch.setitem(sys.modules, "adapter", fake_module)
    monkeypatch.setattr(sys, "argv", ["check_hyperliquid.py"] + argv)
    monkeypatch.setattr(sys, "stdin", io.StringIO(stdin_text))
    buf = io.StringIO()
    monkeypatch.setattr(sys, "stdout", buf)
    code = 0
    try:
        mod.main()
    except SystemExit as e:
        code = e.code or 0
    return buf.getvalue(), code


def test_batch_check_argv_returns_the_documented_json(mod, monkeypatch):
    stdin_text = json.dumps({"v": 1, "slots": [
        {"id": "hl-a", "strategy": "breakout", "mode": "paper",
         "strategy_refs": {"open": {"name": "breakout", "params": {}}}},
        {"id": "hl-b", "strategy": "momentum_pro", "mode": "live",
         "strategy_refs": {"open": {"name": "momentum_pro", "params": {}}}},
    ]})
    out, code = _run_main(mod, monkeypatch, [
        "--batch-check", "--symbol=BTC", "--timeframe=1h",
        "--ohlcv-limit", "200", "--atr-method=simple", "--mark-price=25000",
    ], stdin_text, FakeAdapter())
    assert code == 0, out
    envelope = json.loads(out)
    assert envelope["platform"] == "hyperliquid"
    assert envelope["symbol"] == "BTC" and envelope["timeframe"] == "1h"
    assert envelope["error"] == "" and envelope["error_scope"] == ""
    assert [r["id"] for r in envelope["results"]] == ["hl-a", "hl-b"]
    for result in envelope["results"]:
        for key in ("strategy", "symbol", "timeframe", "signal", "price",
                    "indicators", "mode", "platform", "timestamp"):
            assert key in result
        assert isinstance(result["signal"], int)
        assert isinstance(result["price"], (int, float))


def test_batch_check_rejects_a_malformed_stdin_envelope(mod, monkeypatch):
    out, code = _run_main(mod, monkeypatch, [
        "--batch-check", "--symbol=BTC", "--timeframe=1h",
    ], "{not json", FakeAdapter())
    assert code == 1
    envelope = json.loads(out)
    assert envelope["error_scope"] == "shared_state"
    assert "invalid batch payload" in envelope["error"]
    assert envelope["results"] == []


def test_batch_check_probe_only_exits_before_reading_stdin(mod, monkeypatch):
    class ExplodingStdin:
        def read(self):
            raise AssertionError("--probe-only must not read stdin")

    monkeypatch.setattr(sys, "argv", ["check_hyperliquid.py", "--batch-check",
                                      "--symbol=BTC", "--timeframe=1h", "--probe-only"])
    monkeypatch.setattr(sys, "stdin", ExplodingStdin())
    with pytest.raises(SystemExit) as exc:
        mod.main()
    assert exc.value.code == 0


def test_single_strategy_mode_still_prints_one_object(mod, monkeypatch):
    out, code = _run_main(mod, monkeypatch, [
        "breakout", "BTC", "1h", "--mode=paper", "--mark-price=25000",
        "--strategy-refs", json.dumps({"open": {"name": "breakout", "params": {}}}),
    ], "", FakeAdapter())
    assert code == 0, out
    result = json.loads(out)
    assert result["strategy"] == "breakout"
    assert result["platform"] == "hyperliquid"
    assert result["price"] == 25_000.0
    assert "results" not in result


def test_single_strategy_insufficient_data_shape_is_unchanged(mod, monkeypatch):
    out, code = _run_main(mod, monkeypatch, [
        "breakout", "BTC", "1h", "--mode=paper",
        "--strategy-refs", json.dumps({"open": {"name": "breakout", "params": {}}}),
    ], "", FakeAdapter(candles=_candles(n=12)))
    assert code == 1
    result = json.loads(out)
    assert result["error"] == "Insufficient data: 12 candles"
    assert "regime" not in result


def test_single_strategy_error_shape_is_unchanged(mod, monkeypatch):
    out, code = _run_main(mod, monkeypatch, [
        "no_such_strategy_1442", "BTC", "1h", "--mode=paper",
        "--strategy-refs", json.dumps({"open": {"name": "no_such_strategy_1442", "params": {}}}),
    ], "", FakeAdapter())
    assert code == 1
    result = json.loads(out)
    assert result["error"]
    assert result["regime"] is None
    assert result["signal"] == 0


def test_shared_state_accepts_a_prebuilt_frame_without_an_adapter(mod):
    df = mod._make_dataframe(_candles())
    shared = mod.build_shared_signal_state("BTC", "1h", df=df, atr_method="simple")
    assert shared["adapter"] is None
    assert shared["atr"] > 0
    result = mod.evaluate_signal_slot(shared, _slot("hl-offline", "breakout"))
    assert result["strategy"] == "breakout"
    assert math.isfinite(result["price"])


def _spot_registry_path(mod):
    return os.path.join(
        os.path.dirname(os.path.abspath(mod.__file__)),
        "..", "shared_strategies", "open", "spot", "strategies.py")


@pytest.mark.parametrize("stub_file,accepted", [
    ("spot", False),
    (None, False),
    ("futures", True),
])
def test_futures_registry_fast_path_only_accepts_the_futures_registry(
        mod, monkeypatch, stub_file, accepted):
    stub = types.ModuleType("strategies")
    if stub_file == "spot":
        stub.__file__ = _spot_registry_path(mod)
    elif stub_file == "futures":
        stub.__file__ = mod.FUTURES_STRATEGIES_PATH
    stub.apply_strategy = lambda *a, **k: None
    monkeypatch.setitem(sys.modules, "strategies", stub)

    resolved = mod._futures_strategies_module()
    if accepted:
        assert resolved is stub
    else:
        assert resolved is not stub
    assert os.path.realpath(resolved.__file__) == os.path.realpath(mod.FUTURES_STRATEGIES_PATH)


def _market_frame(rows, required=200, ready=True):
    return {
        "rows": rows,
        "required": required,
        "bars": len(rows),
        "coverage_short": False,
        "first_open_ms": rows[0][0] if rows else 0,
        "last_open_ms": rows[-1][0] if rows else 0,
        "last_close_ms": rows[-1][0] if rows else 0,
        "last_recv_at_ms": 1_700_000_000_000,
        "source": "ws",
        "ready": ready,
        "forming_bar_included": True,
    }


def _market_payload(symbol="BTC", timeframe="1h", rows=None, htf_rows=None,
                    funding=None, mid=25_000.0):
    rows = rows if rows is not None else _candles()
    payload = {
        "version": 1,
        "snapshot_id": "300s/1700000000",
        "generation": 4,
        "sealed_at_ms": 1_700_000_000_000,
        "frames": {f"{symbol}|{timeframe}": _market_frame(rows)},
        "mids": {symbol: {"px": mid, "recv_at_ms": 1_700_000_000_000,
                          "source": "ws", "age_ms": 0, "stale": False, "confirmed": True}},
        "feed_complete": True,
    }
    if htf_rows is not None:
        payload["frames"][f"{symbol}|4h"] = _market_frame(htf_rows, required=60)
    if funding is not None:
        payload["funding"] = {symbol: funding}
    return payload


def test_market_payload_contract(mod):
    adapter = FakeAdapter()
    market = _market_payload()
    shared = mod.build_shared_signal_state(
        "BTC", "1h", adapter=adapter, ohlcv_limit=200, atr_method="simple",
        mark_price=0.0, market=market)
    assert adapter.ohlcv_calls == []
    assert adapter.spot_price_calls == 0
    assert shared["adapter"] is None
    assert shared["price_override"] == 25_000.0
    assert len(shared["df"]) == len(_candles())


def test_market_payload_matches_legacy_polling_for_the_same_frozen_rows(mod):
    rows = _candles()
    legacy = mod.evaluate_signal_slot(_shared(mod, FakeAdapter(rows)), SLOT_MATRIX[0])
    injected_shared = mod.build_shared_signal_state(
        "BTC", "1h", ohlcv_limit=200, atr_method="simple", mark_price=25_000.0,
        market=_market_payload(rows=rows))
    injected = mod.evaluate_signal_slot(injected_shared, SLOT_MATRIX[0])
    assert _strip_volatile(injected) == _strip_volatile(legacy)


def test_market_payload_serves_the_higher_timeframe_frame_without_fetching(mod):
    adapter = FakeAdapter()
    htf_rows = _candles(n=80, step_ms=4 * 3_600_000)
    market = _market_payload(htf_rows=htf_rows)
    shared = mod.build_shared_signal_state(
        "BTC", "1h", adapter=adapter, ohlcv_limit=200, atr_method="simple",
        mark_price=25_000.0, market=market)
    out = mod.evaluate_signal_slot(shared, _slot("hl-htf", "breakout", htf_filter=True))
    assert adapter.ohlcv_calls == []
    assert out["indicators"].get("htf_trend") in (-1, 0, 1)


def test_market_payload_missing_htf_frame_is_an_explicit_error(mod):
    adapter = FakeAdapter()
    shared = mod.build_shared_signal_state(
        "BTC", "1h", adapter=adapter, ohlcv_limit=200, atr_method="simple",
        mark_price=25_000.0, market=_market_payload())
    with pytest.raises(mod.MarketPayloadError):
        mod._shared_htf_frame(shared, "BTC", "4h", 60)
    assert adapter.ohlcv_calls == []


def test_market_payload_funding_comes_from_the_payload(mod):
    adapter = FakeAdapter()
    rows = _candles()
    funding = {
        "current": 0.0003, "avg_7d": 0.0002, "has_scalar": True,
        "records": [{"rate": 0.0004, "time": rows[0][0] - 10},
                    {"rate": 0.0005, "time": rows[0][0] + 10}],
        "has_records": True, "fetched_at_ms": 1_700_000_000_000, "source": "rest",
    }
    shared = mod.build_shared_signal_state(
        "BTC", "1h", adapter=adapter, ohlcv_limit=200, atr_method="simple",
        mark_price=25_000.0, market=_market_payload(rows=rows, funding=funding))
    scalar = mod._shared_funding_scalar(shared, "BTC")
    assert scalar == {"current_funding_rate": 0.0003, "avg_funding_rate_7d": 0.0002}
    records = mod._shared_funding_records(shared, "BTC")
    assert records == [{"rate": 0.0005, "time": rows[0][0] + 10}]
    assert adapter.funding_rate_calls == 0
    assert adapter.funding_range_calls == 0


def test_market_payload_missing_funding_is_an_explicit_error(mod):
    adapter = FakeAdapter()
    shared = mod.build_shared_signal_state(
        "BTC", "1h", adapter=adapter, ohlcv_limit=200, atr_method="simple",
        mark_price=25_000.0, market=_market_payload())
    with pytest.raises(mod.MarketPayloadError):
        mod._shared_funding_scalar(shared, "BTC")
    with pytest.raises(mod.MarketPayloadError):
        mod._shared_funding_records(shared, "BTC")
    assert adapter.funding_rate_calls == 0
    assert adapter.funding_range_calls == 0


def test_market_payload_defects_fail_the_shared_state(mod):
    adapter = FakeAdapter()
    defects = {
        "missing frame": {"version": 1, "snapshot_id": "x", "frames": {}},
        "unready frame": {"version": 1, "snapshot_id": "x",
                          "frames": {"BTC|1h": _market_frame(_candles(), ready=False)}},
        "wrong version": {"version": 99, "snapshot_id": "x",
                          "frames": {"BTC|1h": _market_frame(_candles())}},
        "no snapshot id": {"version": 1, "frames": {"BTC|1h": _market_frame(_candles())}},
        "malformed row": {"version": 1, "snapshot_id": "x",
                          "frames": {"BTC|1h": _market_frame([[1, 2, 3]])}},
        "not an object": "nope",
    }
    for name, market in defects.items():
        envelope, exit_code = mod.run_batch_signal_check(
            "BTC", "1h", SLOT_MATRIX, ohlcv_limit=200, atr_method="simple",
            mark_price=25_000.0, adapter=adapter, market=market)
        assert exit_code == 1, name
        assert envelope["error_scope"] == "shared_state", name
        assert envelope["results"] == [], name
    assert adapter.ohlcv_calls == []


def test_market_payload_short_history_is_insufficient_data(mod):
    envelope, exit_code = mod.run_batch_signal_check(
        "BTC", "1h", SLOT_MATRIX, ohlcv_limit=200, atr_method="simple",
        mark_price=25_000.0, market=_market_payload(rows=_candles(n=10)))
    assert exit_code == 1
    assert envelope["error_scope"] == "shared_state"
    assert "candles" in envelope["error"]


def test_parse_batch_request_versions(mod):
    slots_json = json.dumps({"v": 1, "slots": [{"id": "a", "strategy": "breakout"}]})
    slots, market = mod.parse_batch_request(slots_json)
    assert market is None and len(slots) == 1

    v2 = json.dumps({"v": 2, "slots": [{"id": "a", "strategy": "breakout"}],
                     "market": _market_payload()})
    slots, market = mod.parse_batch_request(v2)
    assert len(slots) == 1
    assert market["snapshot_id"] == "300s/1700000000"

    with pytest.raises(ValueError):
        mod.parse_batch_request(json.dumps({"v": 2, "slots": [{"id": "a", "strategy": "b"}]}))
    with pytest.raises(ValueError):
        mod.parse_batch_request(json.dumps({"v": 1, "slots": [{"id": "a", "strategy": "b"}],
                                            "market": _market_payload()}))
    with pytest.raises(ValueError):
        mod.parse_batch_request(json.dumps({"v": 3, "slots": [{"id": "a", "strategy": "b"}]}))


def test_v1_batch_requests_still_fetch_through_the_adapter(mod):
    adapter = FakeAdapter()
    envelope, exit_code = mod.run_batch_signal_check(
        "BTC", "1h", SLOT_MATRIX, ohlcv_limit=200, atr_method="simple",
        mark_price=25_000.0, adapter=adapter)
    assert exit_code == 0, envelope
    assert adapter.ohlcv_calls, "legacy polling must still reach the adapter"


def test_parse_market_stdin_requires_the_v2_envelope(mod):
    market = _market_payload()
    parsed = mod.parse_market_stdin(json.dumps({"v": 2, "market": market}))
    assert parsed["snapshot_id"] == market["snapshot_id"]
    with pytest.raises(ValueError):
        mod.parse_market_stdin(json.dumps({"v": 1, "market": market}))
    with pytest.raises(mod.MarketPayloadError):
        mod.parse_market_stdin(json.dumps({"v": 2}))

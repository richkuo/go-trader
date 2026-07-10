"""Unit tests for the trade-order Monte Carlo resampler (#1274).

The statistics layer is pure (lists of floats / plain dicts, stdlib-only,
seeded) so it is tested without data access — same architecture as
test_gross_edge_noise. The 3-trade permutation case is hand-enumerable: every
ordering's max drawdown is computed independently and the resampled
distribution must stay inside that enumerated set.
"""

import itertools
import json

import pytest

import eval_windows as ew
import monte_carlo as mc


# ---------------------------------------------------------------------------
# equity_path_stats
# ---------------------------------------------------------------------------

def test_equity_path_empty():
    assert mc.equity_path_stats([]) == (0.0, 0.0)


def test_equity_path_all_winners_has_zero_dd():
    dd, final = mc.equity_path_stats([10.0, 10.0])
    assert dd == 0.0
    assert final == pytest.approx(21.0)


def test_equity_path_hand_computed_dd():
    # 1.0 -> 1.10 (peak) -> 0.88 (dd 20%) -> 0.968 (final -3.2%)
    dd, final = mc.equity_path_stats([10.0, -20.0, 10.0])
    assert dd == pytest.approx(20.0)
    assert final == pytest.approx(-3.2)


def test_equity_path_bust_is_sticky_floor():
    # -100% trade zeroes the account; #1005 convention: DD 100, final -100,
    # later winners never resurrect it.
    dd, final = mc.equity_path_stats([-100.0, 50.0])
    assert (dd, final) == (100.0, -100.0)


def test_equity_path_dd_positive_magnitude():
    dd, _ = mc.equity_path_stats([-10.0])
    assert dd == pytest.approx(10.0)


# ---------------------------------------------------------------------------
# percentile / smoothing / block length
# ---------------------------------------------------------------------------

def test_percentile_empty_none_and_interpolation():
    assert mc.percentile([], 50) is None
    assert mc.percentile([5.0], 95) == 5.0
    assert mc.percentile([0.0, 10.0], 50) == pytest.approx(5.0)
    assert mc.percentile([0.0, 10.0, 20.0], 25) == pytest.approx(5.0)


def test_add_one_smoothed_never_zero_or_one():
    assert mc.add_one_smoothed(0, 100) == pytest.approx(1 / 101)
    assert mc.add_one_smoothed(100, 100) == pytest.approx(101 / 101)


def test_auto_block_len():
    assert mc.auto_block_len(1) == 1
    assert mc.auto_block_len(27) == 3
    assert mc.auto_block_len(28) == 4


# ---------------------------------------------------------------------------
# permute scheme — hand-enumerable 3-trade case
# ---------------------------------------------------------------------------

def test_permutation_dds_stay_in_enumerated_set():
    trades = [10.0, -20.0, 5.0]
    enumerated = {round(mc.equity_path_stats(list(p))[0], 4)
                  for p in itertools.permutations(trades)}
    stats = mc.resample_stats(trades, "permute", n_paths=500, seed=42)
    # Every reported percentile is a value from (or interpolated within) the
    # enumerated distribution's range.
    dd_vals = stats["max_dd_pct_percentiles"]
    assert min(enumerated) <= dd_vals["p5"] <= dd_vals["p50"] \
        <= dd_vals["p95"] <= max(enumerated)
    # Final return is order-invariant under permutation (same multiset).
    fr = stats["final_return_pct_percentiles"]
    expected_final = mc.equity_path_stats(trades)[1]
    assert fr["p5"] == pytest.approx(expected_final, abs=1e-4)
    assert fr["p95"] == pytest.approx(expected_final, abs=1e-4)


def test_permutation_per_path_dds_match_enumeration_exactly():
    trades = [10.0, -20.0, 5.0]
    enumerated = {round(mc.equity_path_stats(list(p))[0], 6)
                  for p in itertools.permutations(trades)}
    from random import Random
    rng = Random(7)
    seen = {round(mc.equity_path_stats(mc.permuted_path(trades, rng))[0], 6)
            for _ in range(300)}
    assert seen <= enumerated
    assert seen == enumerated  # 300 draws over 6 orderings covers all


# ---------------------------------------------------------------------------
# block scheme
# ---------------------------------------------------------------------------

def test_block_path_preserves_circular_contiguity():
    values = [1.0, 2.0, 3.0, 4.0, 5.0]
    adjacent = {(values[i], values[(i + 1) % len(values)])
                for i in range(len(values))}
    from random import Random
    rng = Random(3)
    for _ in range(50):
        path = mc.block_bootstrap_path(values, 2, rng)
        assert len(path) == len(values)
        # Each drawn block of 2 is a circularly-adjacent pair; pairs at even
        # offsets within the path are whole blocks (last may be truncated).
        for i in range(0, len(path) - 1, 2):
            assert (path[i], path[i + 1]) in adjacent


def test_block_len_full_series_yields_rotations():
    values = [1.0, 2.0, 3.0, 4.0]
    rotations = {tuple(values[i:] + values[:i]) for i in range(len(values))}
    from random import Random
    rng = Random(11)
    for _ in range(30):
        assert tuple(mc.block_bootstrap_path(values, len(values), rng)) \
            in rotations


def test_block_scheme_auto_len_recorded():
    stats = mc.resample_stats([1.0] * 27, "block", n_paths=10, seed=1)
    assert stats["block_len"] == 3
    stats = mc.resample_stats([1.0] * 27, "block", n_paths=10, seed=1,
                              block_len=5)
    assert stats["block_len"] == 5


# ---------------------------------------------------------------------------
# resample_stats — determinism, degenerate inputs, smoothing
# ---------------------------------------------------------------------------

def test_resample_stats_deterministic_under_seed():
    trades = [3.0, -2.0, 1.5, -4.0, 2.2, 0.7, -1.1]
    for scheme in mc.SCHEMES:
        a = mc.resample_stats(trades, scheme, n_paths=200, seed=99)
        b = mc.resample_stats(trades, scheme, n_paths=200, seed=99)
        assert a == b


def test_resample_stats_empty_is_degenerate_not_crash():
    for scheme in mc.SCHEMES:
        s = mc.resample_stats([], scheme, n_paths=100, seed=1)
        assert s["n_trades"] == 0
        assert s["max_dd_pct_percentiles"] is None
        assert s["p_dd_ge_kill_switch"] is None


def test_resample_stats_single_trade():
    for scheme in mc.SCHEMES:
        s = mc.resample_stats([5.0], scheme, n_paths=50, seed=1)
        assert s["max_dd_pct_percentiles"]["p50"] == 0.0
        assert s["final_return_pct_percentiles"]["p50"] == pytest.approx(5.0)


def test_all_winner_series_reports_smoothed_floor():
    s = mc.resample_stats([1.0, 2.0, 3.0], "permute", n_paths=100, seed=1,
                          kill_switch_pct=25.0)
    assert s["p_dd_ge_kill_switch"] == pytest.approx(1 / 101)
    assert s["p_final_below_start"] == pytest.approx(1 / 101)


def test_certain_breach_smoothed_below_one():
    # Every ordering of a single -50% trade breaches a 25% threshold.
    s = mc.resample_stats([-50.0], "permute", n_paths=100, seed=1,
                          kill_switch_pct=25.0)
    assert s["p_dd_ge_kill_switch"] == pytest.approx(101 / 101)
    assert s["p_final_below_start"] == pytest.approx(101 / 101)


def test_unknown_scheme_rejected():
    with pytest.raises(ValueError):
        mc.resample_stats([1.0], "bogus")


# ---------------------------------------------------------------------------
# trade_returns / trades_from_json_payload
# ---------------------------------------------------------------------------

def _trade(pnl_pct, shares=2.0, entry_price=100.0, pnl=None):
    return {"pnl_pct": pnl_pct, "shares": shares, "entry_price": entry_price,
            "pnl": pnl}


def test_trade_returns_net_deducts_fees():
    # Gross +5% on 200 notional = +10 gross; net pnl 8 after fees -> +4%.
    vals = mc.trade_returns([_trade(5.0, pnl=8.0)])
    assert vals == [pytest.approx(4.0)]


def test_trade_returns_gross_reads_pnl_pct():
    vals = mc.trade_returns([_trade(5.0, pnl=8.0)], returns="gross")
    assert vals == [5.0]


def test_trade_returns_net_falls_back_without_notional():
    vals = mc.trade_returns([_trade(5.0, shares=0.0, pnl=8.0)])
    assert vals == [5.0]


def test_trade_returns_accepts_bare_numbers():
    assert mc.trade_returns([1.5, -2.0]) == [1.5, -2.0]


def test_trade_returns_rejects_bad_mode():
    with pytest.raises(ValueError):
        mc.trade_returns([], returns="fees")


def test_payload_dict_and_list_forms():
    assert mc.trades_from_json_payload({"trades": [1, 2]}) == [1, 2]
    assert mc.trades_from_json_payload([1, 2]) == [1, 2]
    with pytest.raises(ValueError):
        mc.trades_from_json_payload({"no_trades": []})
    with pytest.raises(ValueError):
        mc.trades_from_json_payload("nope")


# ---------------------------------------------------------------------------
# resolve_kill_switch_pct — mirror of config.go's hierarchy
# ---------------------------------------------------------------------------

def _cfg(strategies, platforms=None):
    return {"strategies": strategies, "platforms": platforms or {}}


def test_kill_switch_explicit_strategy_value_wins():
    cfg = _cfg([{"id": "s1", "type": "perps", "max_drawdown_pct": 12.5}])
    assert mc.resolve_kill_switch_pct(cfg, "s1") == 12.5


def test_kill_switch_platform_risk_override():
    cfg = _cfg([{"id": "s1", "type": "spot", "platform": "okx"}],
               {"okx": {"risk": {"max_drawdown_pct": 33.0}}})
    assert mc.resolve_kill_switch_pct(cfg, "s1") == 33.0


def test_kill_switch_type_defaults():
    for stype, want in (("options", 40.0), ("futures", 45.0),
                        ("perps", 50.0), ("spot", 60.0)):
        cfg = _cfg([{"id": "s1", "type": stype, "platform": "binanceus"}])
        assert mc.resolve_kill_switch_pct(cfg, "s1") == want


def test_kill_switch_platform_inferred_from_id_prefix():
    # hl- prefix -> hyperliquid platform risk override applies.
    cfg = _cfg([{"id": "hl-btc-x", "type": "perps"}],
               {"hyperliquid": {"risk": {"max_drawdown_pct": 18.0}}})
    assert mc.resolve_kill_switch_pct(cfg, "hl-btc-x") == 18.0


def test_kill_switch_missing_strategy_raises():
    with pytest.raises(ValueError):
        mc.resolve_kill_switch_pct(_cfg([]), "ghost")


# ---------------------------------------------------------------------------
# trade_samples_from_results — additive pnl_pct_net key (#1274)
# ---------------------------------------------------------------------------

def test_trade_samples_carry_net_return():
    results = {"trades": [{"entry_date": "2025-01-01", "pnl_pct": 5.0,
                           "shares": 2.0, "entry_price": 100.0, "pnl": 8.0}]}
    samples = ew.trade_samples_from_results(results)
    assert samples[0]["pnl_pct"] == 5.0
    assert samples[0]["pnl_pct_net"] == pytest.approx(4.0)


def test_trade_samples_net_falls_back_to_gross():
    results = {"trades": [{"entry_date": "2025-01-01", "pnl_pct": 5.0,
                           "shares": 0.0, "entry_price": 0.0, "pnl": 8.0}]}
    samples = ew.trade_samples_from_results(results)
    assert samples[0]["pnl_pct_net"] == 5.0


# ---------------------------------------------------------------------------
# CLI end-to-end on a trades-JSON file (no data cache needed)
# ---------------------------------------------------------------------------

def test_cli_deterministic_byte_identical_json(tmp_path, capsys):
    trades = [{"entry_date": "2025-01-01", "pnl_pct": 3.0, "shares": 1.0,
               "entry_price": 100.0, "pnl": 2.5},
              {"entry_date": "2025-01-02", "pnl_pct": -2.0, "shares": 1.0,
               "entry_price": 100.0, "pnl": -2.5},
              {"entry_date": "2025-01-03", "pnl_pct": 4.0, "shares": 1.0,
               "entry_price": 100.0, "pnl": 3.5}]
    src = tmp_path / "results.json"
    src.write_text(json.dumps({"trades": trades}))
    outs = []
    for i in range(2):
        out = tmp_path / f"mc{i}.json"
        rc = mc.main(["--trades-json", str(src), "--seed", "42",
                      "--n-paths", "300", "--json", str(out)])
        assert rc == 0
        outs.append(out.read_bytes())
    assert outs[0] == outs[1]  # byte-identical under the same seed
    payload = json.loads(outs[0])
    assert {b["scheme"] for b in payload["schemes"]} == set(mc.SCHEMES)
    assert payload["kill_switch_pct"] == mc.DEFAULT_KILL_SWITCH_PCT


def test_cli_config_threshold_resolution(tmp_path, capsys):
    src = tmp_path / "results.json"
    src.write_text(json.dumps([1.0, -2.0, 3.0]))
    cfg = tmp_path / "config.json"
    cfg.write_text(json.dumps({"strategies": [
        {"id": "hl-x", "type": "perps", "max_drawdown_pct": 15.0}]}))
    out = tmp_path / "mc.json"
    rc = mc.main(["--trades-json", str(src), "--config", str(cfg),
                  "--strategy-id", "hl-x", "--n-paths", "50",
                  "--json", str(out)])
    assert rc == 0
    assert json.loads(out.read_text())["kill_switch_pct"] == 15.0


def test_cli_requires_exactly_one_source(tmp_path):
    with pytest.raises(SystemExit):
        mc.main([])
    with pytest.raises(SystemExit):
        mc.main(["--trades-json", "a.json", "--strategy", "breakout"])


def test_cli_config_requires_strategy_id(tmp_path):
    src = tmp_path / "results.json"
    src.write_text(json.dumps([1.0]))
    with pytest.raises(SystemExit):
        mc.main(["--trades-json", str(src), "--config", "cfg.json"])


def test_cli_empty_trades_no_crash(tmp_path, capsys):
    src = tmp_path / "results.json"
    src.write_text(json.dumps({"trades": []}))
    rc = mc.main(["--trades-json", str(src), "--n-paths", "50"])
    assert rc == 0
    assert "nothing to resample" in capsys.readouterr().out


# ---------------------------------------------------------------------------
# CLI numeric/enum arg guards — malformed input exits cleanly (SystemExit),
# never a raw IndexError/TypeError (review on #1293).
# ---------------------------------------------------------------------------

def _valid_trades_json(tmp_path):
    src = tmp_path / "results.json"
    src.write_text(json.dumps([1.0, -2.0, 3.0]))
    return src


def test_cli_rejects_negative_n_paths(tmp_path):
    src = _valid_trades_json(tmp_path)
    with pytest.raises(SystemExit):
        mc.main(["--trades-json", str(src), "--n-paths", "-5"])


def test_cli_rejects_zero_n_paths(tmp_path):
    src = _valid_trades_json(tmp_path)
    with pytest.raises(SystemExit):
        mc.main(["--trades-json", str(src), "--n-paths", "0"])


def test_cli_rejects_percentile_above_100(tmp_path):
    src = _valid_trades_json(tmp_path)
    with pytest.raises(SystemExit):
        mc.main(["--trades-json", str(src), "--percentiles", "5,50,150"])


def test_cli_rejects_negative_percentile(tmp_path):
    src = _valid_trades_json(tmp_path)
    with pytest.raises(SystemExit):
        mc.main(["--trades-json", str(src), "--percentiles", "-1"])


def test_cli_rejects_empty_schemes(tmp_path):
    src = _valid_trades_json(tmp_path)
    with pytest.raises(SystemExit):
        mc.main(["--trades-json", str(src), "--schemes", ","])


def test_cli_rejects_empty_percentiles(tmp_path):
    src = _valid_trades_json(tmp_path)
    with pytest.raises(SystemExit):
        mc.main(["--trades-json", str(src), "--percentiles", ","])


def test_cli_rejects_non_numeric_percentile(tmp_path):
    src = _valid_trades_json(tmp_path)
    with pytest.raises(SystemExit):
        mc.main(["--trades-json", str(src), "--percentiles", "5,abc,95"])


def test_cli_rejects_unknown_strategy_id_in_config(tmp_path):
    src = _valid_trades_json(tmp_path)
    cfg = tmp_path / "config.json"
    cfg.write_text(json.dumps({"strategies": [
        {"id": "hl-x", "type": "perps", "max_drawdown_pct": 15.0}]}))
    with pytest.raises(SystemExit):
        mc.main(["--trades-json", str(src), "--config", str(cfg),
                  "--strategy-id", "typo-d-id"])


def test_cli_rejects_trades_json_dict_without_trades_key(tmp_path):
    src = tmp_path / "results.json"
    src.write_text(json.dumps({"foo": []}))
    with pytest.raises(SystemExit):
        mc.main(["--trades-json", str(src)])


def test_cli_rejects_trades_json_bare_string(tmp_path):
    src = tmp_path / "results.json"
    src.write_text(json.dumps("nope"))
    with pytest.raises(SystemExit):
        mc.main(["--trades-json", str(src)])


def test_cli_rejects_trades_json_bare_number(tmp_path):
    src = tmp_path / "results.json"
    src.write_text(json.dumps(42))
    with pytest.raises(SystemExit):
        mc.main(["--trades-json", str(src)])


# ---------------------------------------------------------------------------
# Malformed-CLI-input sub-cases (review on #1293): --config / --trades-json
# missing or invalid, --params invalid JSON, bad --dataset, and trade dicts
# missing pnl_pct — all must SystemExit with an actionable message, never an
# unhandled traceback.
# ---------------------------------------------------------------------------

def test_cli_config_missing_file_exits_cleanly(tmp_path):
    src = _valid_trades_json(tmp_path)
    with pytest.raises(SystemExit):
        mc.main(["--trades-json", str(src),
                  "--config", str(tmp_path / "does_not_exist.json"),
                  "--strategy-id", "hl-x"])


def test_cli_config_invalid_json_exits_cleanly(tmp_path):
    src = _valid_trades_json(tmp_path)
    cfg = tmp_path / "config.json"
    cfg.write_text("{not valid json")
    with pytest.raises(SystemExit):
        mc.main(["--trades-json", str(src), "--config", str(cfg),
                  "--strategy-id", "hl-x"])


def test_cli_trades_json_missing_file_exits_cleanly(tmp_path):
    with pytest.raises(SystemExit):
        mc.main(["--trades-json", str(tmp_path / "does_not_exist.json")])


def test_cli_trades_json_invalid_json_exits_cleanly(tmp_path):
    src = tmp_path / "results.json"
    src.write_text("{not valid json")
    with pytest.raises(SystemExit):
        mc.main(["--trades-json", str(src)])


def test_cli_params_invalid_json_exits_cleanly(tmp_path):
    with pytest.raises(SystemExit):
        mc.main(["--strategy", "squeeze_momentum", "--params", "{bad json"])


def test_run_leg_trades_rejects_bad_dataset(tmp_path):
    with pytest.raises(SystemExit, match="--dataset"):
        mc.run_leg_trades({"name": "squeeze_momentum"}, "spot",
                          "not-a-valid-dataset", "is", 1000.0, "net")


def test_run_leg_trades_rejects_bad_window():
    with pytest.raises(SystemExit, match="unknown window"):
        mc.run_leg_trades({"name": "squeeze_momentum"}, "spot",
                          "BTC/USDT:1h", "no-such-window", 1000.0, "net")


def test_trade_returns_missing_pnl_pct_gross_raises_value_error():
    trade = {"shares": 2.0, "entry_price": 100.0, "pnl": 8.0}
    with pytest.raises(ValueError, match="pnl_pct"):
        mc.trade_returns([trade], returns="gross")


def test_trade_returns_missing_pnl_pct_net_fallback_raises_value_error():
    # No notional (shares=0) forces the net-fallback-to-pnl_pct path, which
    # is also missing pnl_pct here.
    trade = {"shares": 0.0, "entry_price": 0.0, "pnl": 8.0}
    with pytest.raises(ValueError, match="pnl_pct"):
        mc.trade_returns([trade], returns="net")


def test_cli_trades_json_missing_pnl_pct_exits_cleanly(tmp_path):
    src = tmp_path / "results.json"
    src.write_text(json.dumps({"trades": [
        {"shares": 0.0, "entry_price": 0.0, "pnl": 8.0}]}))
    with pytest.raises(SystemExit):
        mc.main(["--trades-json", str(src)])


# ---------------------------------------------------------------------------
# #1295 — candidate fidelity (D1): the run source replays through
# eval_windows.run_candidate_leg, so closes / regime gate / stops survive.
# ---------------------------------------------------------------------------

def _fake_leg(pnls):
    return {"trade_samples": [{"pnl_pct_net": p, "pnl_pct": p} for p in pnls]}


def test_candidate_json_is_a_third_exclusive_source(tmp_path):
    cand = tmp_path / "c.json"
    cand.write_text(json.dumps({"name": "squeeze_momentum"}))
    src = tmp_path / "results.json"
    src.write_text(json.dumps([1.0, -2.0]))
    # two sources
    with pytest.raises(SystemExit, match="exactly one trade source"):
        mc.main(["--trades-json", str(src), "--candidate-json", str(cand)])
    with pytest.raises(SystemExit, match="exactly one trade source"):
        mc.main(["--strategy", "squeeze_momentum",
                 "--candidate-json", str(cand)])
    # zero sources
    with pytest.raises(SystemExit, match="exactly one trade source"):
        mc.main([])


def test_candidate_json_rejects_params_and_direction(tmp_path):
    cand = tmp_path / "c.json"
    cand.write_text(json.dumps({"name": "squeeze_momentum"}))
    with pytest.raises(SystemExit, match="carries its own"):
        mc.main(["--candidate-json", str(cand), "--params", "{}"])
    with pytest.raises(SystemExit, match="carries its own"):
        mc.main(["--candidate-json", str(cand), "--direction", "short"])


def test_candidate_json_threads_closes_gate_and_stops(tmp_path, monkeypatch):
    """The D1 acceptance criterion: the FULL candidate dict reaches
    run_candidate_leg with keep_trades=True — not a hand-picked subset."""
    candidate = {
        "name": "squeeze_momentum",
        "params": {"kc_mult": 1.3},
        "direction": "short",
        "close_strategies": [{"name": "tiered_tp_atr"}],
        "allowed_regimes": ["trending_up"],
        "stop_loss_atr_mult": 2.0,
    }
    cand_path = tmp_path / "c.json"
    cand_path.write_text(json.dumps(candidate))

    seen = {}

    def fake_run_candidate_leg(reg, cand, symbol, timeframe, window,
                               capital=1000.0, *, keep_trades=False,
                               intrabar_resolution="ohlc_walk"):
        seen["candidate"] = cand
        seen["symbol"] = symbol
        seen["timeframe"] = timeframe
        seen["keep_trades"] = keep_trades
        return _fake_leg([3.0, -1.0, 2.0])

    monkeypatch.setattr(ew, "run_candidate_leg", fake_run_candidate_leg)
    out = tmp_path / "mc.json"
    rc = mc.main(["--candidate-json", str(cand_path), "--n-paths", "50",
                  "--json", str(out)])
    assert rc == 0
    assert seen["candidate"] == candidate       # every field, untouched
    assert seen["keep_trades"] is True
    assert (seen["symbol"], seen["timeframe"]) == ("BTC/USDT", "1h")


def test_candidate_json_replays_the_normalized_regime_windows_spec(tmp_path,
                                                                   monkeypatch):
    """validate_candidate normalizes regime_windows_spec in place and the
    Backtester never re-parses it, so the EXECUTED candidate must be the
    validated object. A compact bare-int spec would otherwise reach
    _regime_primary_labels as an int and raise AttributeError."""
    candidate = {"name": "squeeze_momentum",
                 "allowed_regimes": ["trending_up"],
                 "regime_windows_spec": {"medium": 14}}
    cand_path = tmp_path / "c.json"
    cand_path.write_text(json.dumps(candidate))

    seen = {}

    def fake_run_candidate_leg(reg, cand, symbol, timeframe, window,
                               capital=1000.0, *, keep_trades=False,
                               intrabar_resolution="ohlc_walk"):
        seen["spec"] = cand.get("regime_windows_spec")
        return _fake_leg([3.0, -1.0, 2.0])

    monkeypatch.setattr(ew, "run_candidate_leg", fake_run_candidate_leg)
    out = tmp_path / "mc.json"
    rc = mc.main(["--candidate-json", str(cand_path), "--n-paths", "20",
                  "--windows", "is", "--datasets", "BTC/USDT:1h",
                  "--json", str(out)])
    assert rc == 0
    assert seen["spec"] == {"medium": {"classifier": "adx", "period": 14}}
    # ...and the payload still echoes the candidate exactly as authored.
    assert json.loads(out.read_text())["candidate"] == candidate


def test_single_leg_candidate_json_replays_the_normalized_spec(tmp_path,
                                                               monkeypatch):
    """Same invariant on the single-leg path, which uses a separate call."""
    candidate = {"name": "squeeze_momentum",
                 "regime_windows_spec": {"medium": 14}}
    cand_path = tmp_path / "c.json"
    cand_path.write_text(json.dumps(candidate))

    seen = {}

    def fake_run_candidate_leg(reg, cand, symbol, timeframe, window,
                               capital=1000.0, *, keep_trades=False,
                               intrabar_resolution="ohlc_walk"):
        seen["spec"] = cand.get("regime_windows_spec")
        return _fake_leg([1.0, -2.0, 1.5])

    monkeypatch.setattr(ew, "run_candidate_leg", fake_run_candidate_leg)
    rc = mc.main(["--candidate-json", str(cand_path), "--n-paths", "20"])
    assert rc == 0
    assert seen["spec"] == {"medium": {"classifier": "adx", "period": 14}}


def test_candidate_json_replays_the_normalized_directional_policy(tmp_path,
                                                                  monkeypatch):
    """regime_directional_policy is also normalized in place (invert_signal
    defaults filled) — the executed candidate carries the compacted entries."""
    candidate = {
        "name": "squeeze_momentum",
        "close_strategies": [{"name": "atr_stop", "params": {"atr_mult": 2.0}}],
        "regime_directional_policy": {
            "trend_regime": {"trending_up": {"direction": "long"}}},
    }
    cand_path = tmp_path / "c.json"
    cand_path.write_text(json.dumps(candidate))

    seen = {}

    def fake_run_candidate_leg(reg, cand, symbol, timeframe, window,
                               capital=1000.0, *, keep_trades=False,
                               intrabar_resolution="ohlc_walk"):
        seen["rdp"] = cand.get("regime_directional_policy")
        return _fake_leg([1.0, -2.0, 1.5])

    monkeypatch.setattr(ew, "run_candidate_leg", fake_run_candidate_leg)
    rc = mc.main(["--candidate-json", str(cand_path), "--n-paths", "20"])
    assert rc == 0
    assert seen["rdp"] == {"trend_regime": {
        "trending_up": {"direction": "long", "invert_signal": False}}}


def test_candidate_json_without_normalizable_fields_is_byte_identical(
        tmp_path, monkeypatch):
    """Normalization is a no-op for a plain candidate: the executed dict
    equals the authored dict, so D3 leg-equivalence cannot regress."""
    candidate = {"name": "squeeze_momentum", "params": {"kc_mult": 1.3},
                 "direction": "short",
                 "close_strategies": [{"name": "tiered_tp_atr"}]}
    cand_path = tmp_path / "c.json"
    cand_path.write_text(json.dumps(candidate))

    seen = {}

    def fake_run_candidate_leg(reg, cand, symbol, timeframe, window,
                               capital=1000.0, *, keep_trades=False,
                               intrabar_resolution="ohlc_walk"):
        seen["candidate"] = cand
        return _fake_leg([1.0, -2.0])

    monkeypatch.setattr(ew, "run_candidate_leg", fake_run_candidate_leg)
    assert mc.main(["--candidate-json", str(cand_path), "--n-paths", "20"]) == 0
    assert seen["candidate"] == candidate


def test_strategy_path_builds_minimal_candidate():
    assert mc.candidate_from_strategy_args("sq", None, None) == {"name": "sq"}
    assert mc.candidate_from_strategy_args("sq", {"a": 1}, "short") == {
        "name": "sq", "params": {"a": 1}, "direction": "short"}
    # direction omitted (not None-stamped) so run_candidate_leg's validated
    # default ("long") applies.
    assert "direction" not in mc.candidate_from_strategy_args("sq", None, None)


def test_strategy_path_routes_through_run_candidate_leg(tmp_path, monkeypatch):
    seen = {}

    def fake_run_candidate_leg(reg, cand, symbol, timeframe, window,
                               capital=1000.0, *, keep_trades=False,
                               intrabar_resolution="ohlc_walk"):
        seen["candidate"] = cand
        return _fake_leg([1.0, 2.0])

    monkeypatch.setattr(ew, "run_candidate_leg", fake_run_candidate_leg)
    rc = mc.main(["--strategy", "squeeze_momentum", "--params",
                  '{"kc_mult": 1.3}', "--n-paths", "20"])
    assert rc == 0
    assert seen["candidate"] == {"name": "squeeze_momentum",
                                 "params": {"kc_mult": 1.3}}


# ---------------------------------------------------------------------------
# #1295 — multi-leg mode (D2) and seed semantics (D3)
# ---------------------------------------------------------------------------

_MC_KW = dict(schemes=("permute", "block"), n_paths=100, block_len=0,
              seed=42, kill_switch_pct=25.0, kill_switch_source="test",
              percentiles=(5.0, 50.0, 95.0))


def test_multi_leg_payload_shape_and_ordering():
    legs = {("is", "BTC/USDT:1h"): [3.0, -2.0, 1.0],
            ("is", "ETH/USDT:4h"): [1.0, -1.0],
            ("oos", "BTC/USDT:1h"): [2.0, 2.0]}
    payload = mc.build_multi_leg_payload("src", "net", legs, **_MC_KW)
    assert [(l["window"], l["dataset"]) for l in payload["legs"]] == list(legs)
    assert payload["kill_switch_pct"] == 25.0
    assert payload["seed"] == 42
    first = payload["legs"][0]
    assert first["n_trades"] == 3
    assert {b["scheme"] for b in first["schemes"]} == set(mc.SCHEMES)
    assert "observed" in first
    assert "candidate" not in payload  # omitted when not supplied


def test_multi_leg_payload_echoes_candidate():
    payload = mc.build_multi_leg_payload(
        "src", "net", {("is", "BTC/USDT:1h"): [1.0]},
        candidate={"name": "sq"}, **_MC_KW)
    assert payload["candidate"] == {"name": "sq"}


def test_multi_leg_deterministic_under_seed():
    legs = {("is", "BTC/USDT:1h"): [3.0, -2.0, 1.0, 0.5]}
    a = mc.build_multi_leg_payload("s", "net", dict(legs), **_MC_KW)
    b = mc.build_multi_leg_payload("s", "net", dict(legs), **_MC_KW)
    assert json.dumps(a, sort_keys=True) == json.dumps(b, sort_keys=True)


def test_multi_leg_leg_equals_single_run_at_same_seed():
    """D3: a leg's stats block is byte-identical to a standalone single-run
    invocation of that (window, dataset) at the same base seed."""
    values = [3.0, -2.0, 1.0, 0.5, -4.0]
    payload = mc.build_multi_leg_payload(
        "s", "net", {("is", "BTC/USDT:1h"): values,
                     ("oos", "ETH/USDT:4h"): [1.0, -1.0]}, **_MC_KW)
    leg = payload["legs"][0]
    for block in leg["schemes"]:
        standalone = mc.resample_stats(
            values, block["scheme"], n_paths=100, block_len=0, seed=42,
            kill_switch_pct=25.0, percentiles=(5.0, 50.0, 95.0))
        assert block == standalone


def test_multi_leg_empty_leg_reports_error_not_abort():
    legs = {("is", "BTC/USDT:1h"): [1.0, 2.0],
            ("is", "SOL/USDT:4h"): None,          # no cached data
            ("oos", "BTC/USDT:1h"): [3.0]}
    payload = mc.build_multi_leg_payload("s", "net", legs, **_MC_KW)
    bad = payload["legs"][1]
    assert bad["error"] == "no_cached_data"
    assert bad["n_trades"] == 0 and bad["schemes"] == []
    # the other legs survive intact
    assert payload["legs"][0]["n_trades"] == 2
    assert payload["legs"][2]["n_trades"] == 1


def test_multi_leg_zero_trade_leg_is_degenerate_not_error():
    payload = mc.build_multi_leg_payload(
        "s", "net", {("is", "BTC/USDT:1h"): []}, **_MC_KW)
    leg = payload["legs"][0]
    assert "error" not in leg
    assert leg["n_trades"] == 0
    assert all(b["p_dd_ge_kill_switch"] is None for b in leg["schemes"])


def test_multi_leg_rejects_explicit_singular_flags(tmp_path):
    cand = tmp_path / "c.json"
    cand.write_text(json.dumps({"name": "squeeze_momentum"}))
    with pytest.raises(SystemExit, match="single-leg flags"):
        mc.main(["--candidate-json", str(cand), "--windows", "is",
                 "--window", "oos"])
    with pytest.raises(SystemExit, match="single-leg flags"):
        mc.main(["--candidate-json", str(cand), "--datasets", "BTC/USDT:1h",
                 "--dataset", "ETH/USDT:4h"])


def test_multi_leg_rejects_trades_json_source(tmp_path):
    src = tmp_path / "results.json"
    src.write_text(json.dumps([1.0, -2.0]))
    with pytest.raises(SystemExit, match="cannot apply to a --trades-json"):
        mc.main(["--trades-json", str(src), "--windows", "is,oos"])


def test_multi_leg_cli_fans_windows_x_datasets(tmp_path, monkeypatch):
    calls = []

    def fake_run_candidate_leg(reg, cand, symbol, timeframe, window,
                               capital=1000.0, *, keep_trades=False,
                               intrabar_resolution="ohlc_walk"):
        calls.append((symbol, timeframe))
        return _fake_leg([2.0, -1.0, 1.5])

    monkeypatch.setattr(ew, "run_candidate_leg", fake_run_candidate_leg)
    out = tmp_path / "mc.json"
    rc = mc.main(["--strategy", "squeeze_momentum", "--windows", "is,oos",
                  "--datasets", "BTC/USDT:1h,ETH/USDT:4h",
                  "--n-paths", "20", "--json", str(out)])
    assert rc == 0
    assert len(calls) == 4
    payload = json.loads(out.read_text())
    assert [(l["window"], l["dataset"]) for l in payload["legs"]] == [
        ("is", "BTC/USDT:1h"), ("is", "ETH/USDT:4h"),
        ("oos", "BTC/USDT:1h"), ("oos", "ETH/USDT:4h")]
    assert payload["candidate"] == {"name": "squeeze_momentum"}


def test_multi_leg_datasets_default_to_the_audit_six(tmp_path, monkeypatch):
    monkeypatch.setattr(ew, "run_candidate_leg",
                        lambda *a, **k: _fake_leg([1.0, -1.0]))
    out = tmp_path / "mc.json"
    rc = mc.main(["--strategy", "squeeze_momentum", "--windows", "is",
                  "--n-paths", "10", "--json", str(out)])
    assert rc == 0
    payload = json.loads(out.read_text())
    assert len(payload["legs"]) == len(ew.DATASETS)


def test_multi_leg_uncached_leg_skips_without_aborting(tmp_path, monkeypatch):
    def fake_run_candidate_leg(reg, cand, symbol, timeframe, window,
                               capital=1000.0, *, keep_trades=False,
                               intrabar_resolution="ohlc_walk"):
        if symbol == "ETH/USDT":
            return None                      # no cached bars for this leg
        return _fake_leg([1.0, -1.0, 2.0])

    monkeypatch.setattr(ew, "run_candidate_leg", fake_run_candidate_leg)
    out = tmp_path / "mc.json"
    rc = mc.main(["--strategy", "squeeze_momentum", "--windows", "is",
                  "--datasets", "BTC/USDT:1h,ETH/USDT:4h", "--n-paths", "10",
                  "--json", str(out)])
    assert rc == 0
    legs = json.loads(out.read_text())["legs"]
    assert legs[0]["n_trades"] == 3
    assert legs[1]["error"] == "no_cached_data"


def test_multi_leg_kill_switch_from_config(tmp_path, monkeypatch):
    monkeypatch.setattr(ew, "run_candidate_leg",
                        lambda *a, **k: _fake_leg([1.0, -1.0]))
    cfg = tmp_path / "config.json"
    cfg.write_text(json.dumps({"strategies": [
        {"id": "hl-x", "type": "perps", "max_drawdown_pct": 15.0}]}))
    out = tmp_path / "mc.json"
    rc = mc.main(["--strategy", "squeeze_momentum", "--windows", "is",
                  "--datasets", "BTC/USDT:1h", "--config", str(cfg),
                  "--strategy-id", "hl-x", "--n-paths", "10",
                  "--json", str(out)])
    assert rc == 0
    payload = json.loads(out.read_text())
    assert payload["kill_switch_pct"] == 15.0
    assert payload["legs"][0]["schemes"][0]["kill_switch_pct"] == 15.0


def test_multi_leg_report_renders_error_legs():
    payload = mc.build_multi_leg_payload(
        "s", "net", {("is", "BTC/USDT:1h"): [1.0, -1.0],
                     ("is", "SOL/USDT:4h"): None}, **_MC_KW)
    text = mc.format_multi_leg_report(payload)
    assert "[is] BTC/USDT:1h" in text
    assert "no_cached_data" in text

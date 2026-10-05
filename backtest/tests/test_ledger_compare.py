import copy
import gzip
import hashlib
import io
import json
import os
import shutil
import socket

import pytest

import ledger_compare as lc
import data_fetcher
import offline_manifest as om

FIXTURE = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "testdata", "ledger_export"))
INPUTS = {
    "export.json": "comparison_input.json",
    "export_scale_in.json": "comparison_input_scale_in.json",
    "export_manual.json": "comparison_input_manual.json",
}
INTERVAL_START = "2026-01-05T00:00:00Z"
START_MS = 1767571200000
HOUR_MS = 3_600_000


def _load(path):
    with open(path) as fh:
        return json.load(fh)


def _dump(path, obj):
    with open(path, "w") as fh:
        fh.write(json.dumps(obj, indent=2) + "\n")


def _sha(path):
    with open(path, "rb") as fh:
        return hashlib.sha256(fh.read()).hexdigest()


def _copy(tmp_path):
    dst = tmp_path / "fx"
    shutil.copytree(FIXTURE, dst, ignore=shutil.ignore_patterns("snapshot", "source"))
    return dst


def _rebind(fx):
    manifest_sha = _sha(fx / "market" / "manifest.json")
    for export, inp in INPUTS.items():
        doc = _load(fx / export)
        cin = _load(fx / inp)
        cin["export"]["booked_sections_sha256"] = lc.booked_sections_sha256(doc)
        cin["market"]["manifest"]["sha256"] = manifest_sha
        _dump(fx / inp, cin)


def _run(fx, tmp_path, export="export.json", mode="strict", name="report.json"):
    out = tmp_path / name
    if out.exists():
        out.unlink()
    rc = lc.main(["--export", str(fx / export), "--comparison-input", str(fx / INPUTS[export]),
                  "--mode", mode, "--output", str(out)])
    return rc, (_load(out) if out.exists() else None)


def _in_interval(ev):
    return ev["timestamp"] >= INTERVAL_START and ev["timestamp"] < "2026-01-06T20:00:00Z"


def _new_event(template, key, ts, position_id, qty, price):
    ev = copy.deepcopy(template)
    ev["event_key"] = key
    ev["source_row_id"] = key.rsplit("/", 1)[-1]
    ev["timestamp"] = ev["timestamp_raw"] = ts
    ev["position_id"]["value"] = ev["position_id"]["raw_value"] = position_id
    ev["quantity"]["value"] = ev["quantity"]["raw_value"] = qty
    ev["price"]["value"] = ev["price"]["raw_value"] = price
    return ev


def _rewrite_gz(path, keep):
    with gzip.open(path, "rt") as fh:
        lines = fh.read().splitlines()
    body = "\n".join([lines[0]] + [ln for ln in lines[1:] if keep(int(ln.split(",")[0]))]) + "\n"
    buf = io.BytesIO()
    with gzip.GzipFile(fileobj=buf, mode="wb", mtime=0, filename="") as gz:
        gz.write(body.encode())
    path.write_bytes(buf.getvalue())


def _rehash_market(fx, warmup=None):
    mpath = fx / "market" / "manifest.json"
    m = _load(mpath)
    ds = m["datasets"][0]
    ds["candles"]["sha256"] = _sha(fx / "market" / ds["candles"]["path"])
    ds["funding"]["sha256"] = _sha(fx / "market" / ds["funding"]["path"])
    if warmup is not None:
        m["warmup_bars"] = warmup
    _dump(mpath, m)
    _rebind(fx)


def _dispositions_complete(report, export_doc):
    keys = {e["event_key"] for e in export_doc["events"]}
    return set(report["booked"]["dispositions"]) == keys


def test_strict_fixture_reaches_strict_success_offline(tmp_path, monkeypatch):
    def no_network(*a, **k):
        raise AssertionError("the comparison must not touch the network or cached market data")

    monkeypatch.setattr(socket.socket, "connect", no_network)
    monkeypatch.setattr(data_fetcher, "load_cached_data", no_network)
    monkeypatch.setattr(om, "_post_info", no_network)
    rc, rep = _run(_copy(tmp_path), tmp_path)
    assert rc == 0
    assert rep["outcome"] == "strict_success" and all(rep["strict_checks"].values())
    assert rep["provenance"]["export_sha256"] == _sha(os.path.join(FIXTURE, "export.json"))
    assert rep["provenance"]["inspected_revision"] is None
    assert rep["configuration"]["present"]["basis"] == "current_at_capture"
    assert rep["configuration"]["historical"]["source"] == "comparison_input.historical_configuration"
    assert rep["timestamps"]["booked_event_timestamp_meaning"].startswith("ledger_record_time")
    cash = rep["starting_state"]["cash_usd"]
    assert cash["status"] == "verified" and cash["verification"] == "recomputed"
    assert cash["recomputed"] == pytest.approx(1009.168047, abs=1e-9)
    assert rep["starting_state"]["pending_decision"]["recomputed"] == "none"
    pairs = {m["booked_position_id"]: m for m in rep["matching"]["matched"]}
    assert set(pairs) == {"pos-s-1", "pos-s-2"}
    assert pairs["pos-s-1"]["simulated"]["closed_qty"] == pytest.approx(pairs["pos-s-1"]["simulated"]["opened_qty"])
    tail = pairs["pos-s-2"]
    assert tail["booked"]["crosses_interval_end"] is True
    assert tail["booked"]["residual_qty"] == pytest.approx(0.01719, abs=1e-12)
    assert tail["simulated"]["residual_qty"] == pytest.approx(0.01719, abs=1e-12)
    assert len(rep["simulated"]["synthetic_terminal_events"]) == 1
    assert rep["simulated"]["dispositions"][rep["simulated"]["synthetic_terminal_events"][0]] == "synthetic"
    relations = {o["relation"] for o in rep["booked"]["outside_interval"]}
    assert relations == {"before", "after"}
    assert rep["booked"]["strategy_funding"]["allocation"] == "unallocated"
    assert rep["strategy_totals"]["deltas"]["funding"] == pytest.approx(0.0, abs=0.01)
    assert rep["wallet_orphan_context"]["records"] == 1
    assert rep["wallet_orphan_context"]["included_in_strategy_totals"] is False
    assert rep["costs"]["actual_recorded"]["fees"] != rep["costs"]["modeled"]["fees"]
    assert all(c["ok"] for c in rep["conservation"]["checks"])
    assert _dispositions_complete(rep, _load(os.path.join(FIXTURE, "export.json")))


def test_known_divergence_fails_and_restoration_passes(tmp_path):
    fx = _copy(tmp_path)
    original = (fx / "export.json").read_bytes()
    doc = json.loads(original)
    ev = next(e for e in doc["events"] if e["event_kind"]["value"] == "non_close" and _in_interval(e))
    for field, delta in (("exchange_fee", 0.1), ("row_net_pnl", -0.1), ("ledger_delta", -0.1)):
        ev[field]["value"] += delta
        if ev[field]["raw_value"] is not None:
            ev[field]["raw_value"] += delta
    _dump(fx / "export.json", doc)
    _rebind(fx)
    rc, rep = _run(fx, tmp_path)
    assert rc == 1 and rep["outcome"] == "mismatch"
    pair = next(m for m in rep["matching"]["matched"] if m["booked_position_id"] == ev["position_id"]["value"])
    assert pair["within_tolerance"] is False
    assert pair["deltas"]["entry_fees"] == pytest.approx(0.1, abs=1e-4)
    assert pair["deltas"]["net"] == pytest.approx(-0.1, abs=0.05)
    (fx / "export.json").write_bytes(original)
    _rebind(fx)
    rc, rep = _run(fx, tmp_path)
    assert rc == 0 and rep["outcome"] == "strict_success"


def test_changed_inputs_without_rebinding_are_refused(tmp_path):
    fx = _copy(tmp_path)
    doc = _load(fx / "export.json")
    ev = next(e for e in doc["events"] if e["event_kind"]["value"] == "close" and _in_interval(e))
    ev["quantity"]["value"] = ev["quantity"]["raw_value"] = ev["quantity"]["value"] + 0.001
    _dump(fx / "export.json", doc)
    rc, rep = _run(fx, tmp_path)
    assert rc == 1 and rep["outcome"] == "refused"
    assert {r.get("field") for r in rep["eligibility"]["refusals"] if r["reason"] == "input_binding"} == {"booked_sections_sha256"}
    assert rep["simulation"]["status"] == "not_run"

    fx2 = _copy(tmp_path / "second")
    candles = fx2 / "market" / "BTC_1h_candles.csv.gz"
    _rewrite_gz(candles, lambda ts: ts != START_MS + 5 * HOUR_MS)
    rc, rep = _run(fx2, tmp_path)
    assert rc == 1 and rep["outcome"] == "refused"
    assert "market_input_integrity" in {r["reason"] for r in rep["eligibility"]["refusals"]}


def test_ambiguous_and_unmatched_outcomes_stay_in_the_report(tmp_path):
    fx = _copy(tmp_path)
    doc = _load(fx / "export.json")
    opening = next(e for e in doc["events"] if e["event_kind"]["value"] == "non_close" and _in_interval(e))
    twin = _new_event(opening, "primary/trades/9001", opening["timestamp"], "pos-twin",
                      opening["quantity"]["value"], opening["price"]["value"])
    stray = _new_event(opening, "primary/trades/9002", "2026-01-05T12:00:41Z", "pos-stray", 0.01, 61000.0)
    tail_id = "pos-s-2"
    doc["events"] = [e for e in doc["events"] if e["position_id"]["value"] != tail_id] + [twin, stray]
    _dump(fx / "export.json", doc)
    _rebind(fx)
    rc, rep = _run(fx, tmp_path)
    assert rc == 1 and rep["outcome"] == "mismatch"
    amb = rep["matching"]["ambiguous"]
    assert len(amb) == 1
    assert sorted(amb[0]["booked_position_ids"]) == ["pos-s-1", "pos-twin"]
    assert amb[0]["simulated_position_ids"] == ["sim-pos-0001"]
    assert amb[0]["reason"] == "equally_supported_assignments" and amb[0]["alternatives"] == 2
    assert len(amb[0]["candidates"]) == 2
    assert [u["booked_position_id"] for u in rep["matching"]["unmatched_booked"]] == ["pos-stray"]
    assert [u["simulated_position_id"] for u in rep["matching"]["unmatched_simulated"]] == ["sim-pos-0002"]
    disp = rep["booked"]["dispositions"]
    assert disp["primary/trades/9001"] == "ambiguous" and disp["primary/trades/9002"] == "unmatched"
    assert _dispositions_complete(rep, doc)
    assert all(c["ok"] for c in rep["conservation"]["checks"])


def test_scale_in_fixture_conserves_booked_accounting_and_refuses_strict(tmp_path):
    fx = _copy(tmp_path)
    doc = _load(fx / "export_scale_in.json")
    rc, rep = _run(fx, tmp_path, "export_scale_in.json")
    assert rc == 1 and rep["outcome"] == "refused"
    fields = {r.get("field") for r in rep["eligibility"]["refusals"]}
    assert {"allow_scale_in", "scale_in", "stop_loss_atr_mult"} <= fields
    assert rep["simulation"]["status"] == "not_run"
    positions = {p["position_id"]: p for p in rep["booked"]["positions"]}
    closed = positions["pos-c-1"]
    assert closed["opened_qty"] == pytest.approx(0.0084) and closed["closed_qty"] == pytest.approx(0.0084)
    assert closed["residual_qty"] == pytest.approx(0.0, abs=1e-12)
    assert closed["entry_fees"] == pytest.approx(0.112096 + 0.11264)
    assert closed["exit_fees"] == pytest.approx(0.113419 + 0.114969)
    assert closed["ledger_delta"] == pytest.approx(-0.112096 - 0.11264 + (2.331 - 0.113419) + 5.660031)
    assert len(closed["legacy_rows"]) == 1
    open_pos = positions["pos-c-2"]
    assert open_pos["residual_qty"] == pytest.approx(0.0084)
    legacy_open = next(e for e in doc["events"] if e["event_key"] == open_pos["legacy_rows"][0])
    assert legacy_open["row_net_pnl"]["value"] == 0
    assert legacy_open["ledger_delta"]["value"] == pytest.approx(-legacy_open["exchange_fee"]["value"])
    assert open_pos["ledger_delta"] == pytest.approx(-0.11285 - 0.113211)
    funding = rep["booked"]["strategy_funding"]
    assert funding["in_interval_total"] == pytest.approx(-0.003114 - 0.003128 - 0.006261 - 0.003099)
    assert funding["allocated_to_positions"] == 0.0
    assert [u["reason"] for u in rep["booked"]["unresolved"]] == ["no recorded position_id; position allocation is unresolved"]
    assert all(c["ok"] for c in rep["conservation"]["checks"])
    assert _dispositions_complete(rep, doc)

    rc, rep = _run(fx, tmp_path, "export_scale_in.json", mode="approximate", name="approx.json")
    assert rc == 1 and rep["outcome"] == "incomplete" and rep["strict_success"] is False
    assumed = {a["feature"] for a in rep["eligibility"]["approximations"]}
    assert {"comparison_mode", "allow_scale_in", "stop_loss_atr_mult", "execution_cost"} <= assumed
    assert rep["simulation"]["status"] == "run"
    assert rep["simulation"]["cost_model"]["kind"] == "flat_taker_fee_and_adverse_price"
    assert all(c["ok"] for c in rep["conservation"]["checks"])
    assert rep["matching"]["unmatched_booked"] and rep["matching"]["unmatched_simulated"]


def test_manual_owner_is_refused_and_not_comparable(tmp_path):
    fx = _copy(tmp_path)
    doc = _load(fx / "export_manual.json")
    for mode in ("strict", "approximate"):
        rc, rep = _run(fx, tmp_path, "export_manual.json", mode=mode, name=f"{mode}.json")
        assert rc == 1 and rep["strict_success"] is False
        assert any(r.get("field") == "type" and r["reason"] == "capability_signal_model"
                   for r in rep["eligibility"]["refusals"])
        assert {n["event_key"] for n in rep["booked"]["not_comparable"]} == {e["event_key"] for e in doc["events"]}
        assert set(rep["booked"]["dispositions"].values()) == {"not_comparable"}
        assert rep["simulation"]["status"] == "not_run"


@pytest.mark.parametrize("case,reason", [
    ("edge_gap", "market_candle_grid_incomplete"),
    ("warmup_join_gap", "market_candle_grid_incomplete"),
    ("missing_funding", "market_funding_incomplete"),
    ("short_warmup", "market_indicator_history_insufficient"),
])
def test_hash_valid_incomplete_market_inputs_refuse_strict(tmp_path, case, reason):
    fx = _copy(tmp_path)
    market = fx / "market"
    warmup = None
    if case == "edge_gap":
        end_ms = START_MS + 44 * HOUR_MS
        _rewrite_gz(market / "BTC_1h_candles.csv.gz", lambda ts: ts != end_ms - HOUR_MS)
    elif case == "warmup_join_gap":
        _rewrite_gz(market / "BTC_1h_candles.csv.gz", lambda ts: ts != START_MS - HOUR_MS)
    elif case == "missing_funding":
        _rewrite_gz(market / "BTC_funding.csv.gz", lambda ts: not (START_MS + 3 * HOUR_MS <= ts < START_MS + 6 * HOUR_MS))
    else:
        warmup = 10
    _rehash_market(fx, warmup)
    rc, rep = _run(fx, tmp_path)
    assert rc == 1 and rep["outcome"] == "refused"
    assert reason in {r["reason"] for r in rep["eligibility"]["refusals"]}
    assert not any(r["reason"] == "market_input_integrity" for r in rep["eligibility"]["refusals"])
    assert rep["simulation"]["status"] == "not_run"


def test_stale_recorded_observations_refuse_strict_and_fresh_ones_pass(tmp_path):
    import pandas as pd
    from registry_loader import load_registry

    study = os.path.join(os.path.dirname(FIXTURE), "..", "candidates", "open_interest_breakout_1637")
    dst = tmp_path / "study"
    shutil.copytree(study, dst, ignore=shutil.ignore_patterns("recording", "*.md", "*.py", "results*.json"))
    manifest = _load(dst / "study_manifest.json")
    manifest["windows"]["fresh"] = {"start": "2026-10-04T03:20:00Z", "end": "2026-10-04T05:25:00Z", "role": "test"}
    _dump(dst / "study_manifest.json", manifest)
    params = load_registry("futures").STRATEGY_REGISTRY["open_interest_breakout"]["default_params"]
    results = {}
    for window, (start, end) in {"train": ("2026-10-04T03:10:00Z", "2026-10-04T06:10:00Z"),
                                 "fresh": ("2026-10-04T03:20:00Z", "2026-10-04T05:25:00Z")}.items():
        market = lc.prepare_market(str(dst / "study_manifest.json"), _sha(dst / "study_manifest.json"), "BTC 5m",
                                   window, pd.Timestamp(start), pd.Timestamp(end))
        assert market["refusals"] == [] or {r["reason"] for r in market["refusals"]} == {"market_funding_incomplete"}
        results[window] = lc.market_strategy_checks(market, "open_interest_breakout", params, "long", False, True, False)
    stale = results["train"]
    assert stale["checks"]["observations"]["ok"] is False
    assert stale["checks"]["observations"]["invalid_decision_bar_count"] == 11
    assert "market_observations_incomplete" in {r["reason"] for r in stale["refusals"]}
    fresh = results["fresh"]
    assert fresh["checks"]["observations"]["ok"] is True
    assert fresh["checks"]["observations"]["invalid_decision_bar_count"] == 0
    assert "market_observations_incomplete" not in {r["reason"] for r in fresh["refusals"]}


def test_missing_or_unverified_history_keeps_results_unverified(tmp_path):
    fx = _copy(tmp_path)
    base = _load(fx / "comparison_input.json")

    cin = copy.deepcopy(base)
    cin["starting_state"]["cash_usd"]["status"] = "unverified"
    _dump(fx / "comparison_input.json", cin)
    rc, rep = _run(fx, tmp_path, name="cash.json")
    assert rc == 1 and rep["outcome"] == "unverified"
    assert rep["strict_checks"]["close_validation_strict_eligible"] is True
    assert rep["strict_checks"]["matched_within_tolerance"] is True
    assert {u["input"] for u in rep["eligibility"]["unverified"]} == {"starting_state.cash_usd"}

    cin = copy.deepcopy(base)
    cin["starting_state"]["cash_usd"]["value"] = 1000.0
    _dump(fx / "comparison_input.json", cin)
    rc, rep = _run(fx, tmp_path, name="replay.json")
    assert rc == 1 and rep["starting_state"]["cash_usd"]["status"] == "unverified"
    assert rep["starting_state"]["cash_usd"]["recomputed"] == pytest.approx(1009.168047, abs=1e-9)

    cin = copy.deepcopy(base)
    cin["historical_configuration"]["timeline"] = []
    _dump(fx / "comparison_input.json", cin)
    rc, rep = _run(fx, tmp_path, name="nohist.json")
    assert rc == 1 and rep["outcome"] == "unverified"
    assert rep["simulation"]["status"] == "not_run"
    assert {u["input"] for u in rep["eligibility"]["unverified"]} >= {"historical_configuration"}
    assert rep["configuration"]["present"]["strategy"]["id"] == "hl-strict-btc"

    cin = copy.deepcopy(base)
    seg = cin["historical_configuration"]["timeline"][0]
    later = copy.deepcopy(seg)
    seg["effective_to"] = later["effective_from"] = "2026-01-06T00:00:00Z"
    cin["historical_configuration"]["timeline"].append(later)
    _dump(fx / "comparison_input.json", cin)
    rc, rep = _run(fx, tmp_path, name="transition.json")
    assert rc == 1 and rep["outcome"] == "refused"
    assert "configuration_transition_unsupported" in {r["reason"] for r in rep["eligibility"]["refusals"]}

    cin = copy.deepcopy(base)
    cin["historical_configuration"]["timeline"][0]["strategy"]["close_strategy"]["params"]["tp_tiers"] = [
        {"profit_pct": 0.005, "close_fraction": 0.5}, {"profit_pct": 0.01, "close_fraction": 1.0}]
    _dump(fx / "comparison_input.json", cin)
    rc, rep = _run(fx, tmp_path, name="history.json")
    assert rc == 1 and rep["outcome"] == "mismatch"
    assert "strategy.close_strategy" in {d["field"] for d in rep["configuration"]["present_vs_historical_differences"]}

    cin = copy.deepcopy(base)
    cin["starting_state"]["inventory"] = {"quantity": 0.01, "status": "verified", "rule": "attested",
                                          "evidence": "disposable seeded inventory"}
    _dump(fx / "comparison_input.json", cin)
    rc, rep = _run(fx, tmp_path, name="seeded.json")
    assert rc == 1 and "seeded_inventory_unsupported" in {r["reason"] for r in rep["eligibility"]["refusals"]}

    cin = copy.deepcopy(base)
    cin["starting_state"]["pending_decision"]["value"] = "long"
    _dump(fx / "comparison_input.json", cin)
    rc, rep = _run(fx, tmp_path, name="pending.json")
    assert rc == 1 and rep["starting_state"]["pending_decision"]["status"] == "unverified"


def test_close_validation_alone_never_yields_strict_success(tmp_path):
    fx = _copy(tmp_path)
    rc, rep = _run(fx, tmp_path, mode="approximate", name="approx.json")
    assert rc == 1 and rep["outcome"] == "incomplete"
    assert rep["strict_checks"]["matched_within_tolerance"] is True
    assert rep["eligibility"]["close_validation"]["parity_status"] == "incomplete"

    base = _load(fx / "comparison_input.json")
    cin = copy.deepcopy(base)
    cin["historical_configuration"]["timeline"][0]["strategy"]["close_strategy"] = {
        "name": "time_stop", "params": {"max_bars": 6}}
    _dump(fx / "comparison_input.json", cin)
    rc, rep = _run(fx, tmp_path, name="time_stop.json")
    assert rc == 1 and rep["outcome"] == "refused"
    assert "close_validation_refused" in {r["reason"] for r in rep["eligibility"]["refusals"]}
    assert rep["eligibility"]["close_validation"]["close_eligibility"] == "refused"
    rc, rep = _run(fx, tmp_path, mode="approximate", name="time_stop_approx.json")
    assert rc == 1 and rep["outcome"] == "incomplete" and rep["strict_success"] is False
    assert rep["eligibility"]["close_validation"]["close_eligibility"] == "approximate"
    assert "close_validation" in {a["category"] for a in rep["eligibility"]["approximations"]}


def test_malformed_inputs_exit_without_a_report(tmp_path, capsys):
    fx = _copy(tmp_path)
    doc = _load(fx / "export.json")
    doc["events"].append(copy.deepcopy(doc["events"][0]))
    _dump(fx / "export.json", doc)
    rc, rep = _run(fx, tmp_path)
    assert rc == 2 and rep is None
    fx2 = _copy(tmp_path / "second")
    out = tmp_path / "exists.json"
    out.write_text("{}")
    rc = lc.main(["--export", str(fx2 / "export.json"), "--comparison-input", str(fx2 / "comparison_input.json"),
                  "--output", str(out)])
    assert rc == 2 and out.read_text() == "{}"

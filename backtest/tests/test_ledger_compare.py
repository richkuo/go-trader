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
    assert {"allow_scale_in", "scale_in"} <= fields
    assert not fields & set(lc.STOP_ROLE_FIELDS)
    assert not any(r["reason"] == "capability_protection" or r["reason"].startswith("stop_")
                   for r in rep["eligibility"]["refusals"])
    assert rep["stops"]["owner"] == "fixed_atr" and rep["stops"]["status"] == "modeled"
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
    assert {"comparison_mode", "allow_scale_in", "execution_cost"} <= assumed
    assert "stop_loss_atr_mult" not in assumed
    arms = rep["stops"]["arm_events"]
    assert arms and all(a["owner"] == "fixed_atr" and a["trigger"] > 0 for a in arms)
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


def _template(doc, kind):
    return next(e for e in doc["events"] if e["event_kind"]["value"] == kind and e["position_id"]["value"])


def test_attested_inventory_is_checked_against_the_booked_replay(tmp_path):
    fx = _copy(tmp_path)
    original = _load(fx / "export.json")
    base = _load(fx / "comparison_input.json")
    attested = {"quantity": 0.0, "status": "verified", "rule": "attested", "evidence": "operator says flat"}

    doc = copy.deepcopy(original)
    doc["events"] += [
        _new_event(_template(doc, "non_close"), "primary/trades/9101", "2026-01-04T20:00:41Z", "pos-held", 0.002, 60000.0),
        _new_event(_template(doc, "close"), "primary/trades/9102", "2026-01-07T01:00:41Z", "pos-held", 0.002, 61000.0),
    ]
    _dump(fx / "export.json", doc)
    cin = copy.deepcopy(base)
    cin["starting_state"]["inventory"] = attested
    _dump(fx / "comparison_input.json", cin)
    _rebind(fx)
    rc, rep = _run(fx, tmp_path, name="held.json")
    assert rc == 1 and rep["strict_success"] is False
    inv = rep["starting_state"]["inventory"]
    assert inv["status"] == "unverified" and inv["recomputed"] == pytest.approx(0.002)
    assert inv["verification"] == "attested_and_checked_against_booked_ledger_replay"
    seeded = next(r for r in rep["eligibility"]["refusals"] if r["reason"] == "seeded_inventory_unsupported")
    assert seeded["declared"] == 0.0 and seeded["booked_replay"] == pytest.approx(0.002)
    assert rep["strategy_totals"]["booked"]["end_inventory_signed"] == pytest.approx(0.002 + 0.01719)

    doc = copy.deepcopy(original)
    orphan = _new_event(_template(doc, "non_close"), "primary/trades/9103", "2026-01-04T20:00:41Z", None, 0.002, 60000.0)
    orphan["position_id"] = {"value": None, "raw_value": None, "status": "unavailable",
                             "reason": "legacy_row_without_position_id", "provenance": []}
    doc["events"].append(orphan)
    _dump(fx / "export.json", doc)
    _rebind(fx)
    rc, rep = _run(fx, tmp_path, name="unresolved.json")
    assert rc == 1 and rep["strict_success"] is False
    inv = rep["starting_state"]["inventory"]
    assert inv["status"] == "unverified" and "unresolved position identity" in inv["note"]
    assert "starting_state.inventory" in {u["input"] for u in rep["eligibility"]["unverified"]}

    (fx / "export.json").write_text(json.dumps(original, indent=2) + "\n")
    _dump(fx / "comparison_input.json", base)
    _rebind(fx)
    rc, rep = _run(fx, tmp_path, name="restored.json")
    assert rc == 0 and rep["outcome"] == "strict_success"


@pytest.mark.parametrize("field,value", [
    ("open_strategy", "sma_crossover"),
    ("params", ["fast", 10]),
    ("args", "sma_crossover BTC 1h"),
])
def test_malformed_strategy_shapes_exit_without_a_report(tmp_path, field, value):
    fx = _copy(tmp_path)
    cin = _load(fx / "comparison_input.json")
    strategy = cin["historical_configuration"]["timeline"][0]["strategy"]
    if field == "params":
        strategy["open_strategy"]["params"] = value
    else:
        strategy[field] = value
    _dump(fx / "comparison_input.json", cin)
    rc, rep = _run(fx, tmp_path)
    assert rc == lc.EXIT_INPUT_ERROR and rep is None


def test_unexpected_errors_exit_without_a_report(tmp_path, monkeypatch):
    def broken(*a, **k):
        raise RuntimeError("disposable internal failure")

    monkeypatch.setattr(lc, "run_simulation", broken)
    rc, rep = _run(_copy(tmp_path), tmp_path)
    assert rc == lc.EXIT_INTERNAL_ERROR and rep is None


def test_booked_conservation_checks_fail_on_inconsistent_ledgers(tmp_path):
    fx = _copy(tmp_path)
    doc = _load(fx / "export.json")
    row = next(e for e in doc["events"] if e["event_kind"]["value"] == "close" and _in_interval(e))
    row["ledger_delta"]["value"] += 0.5
    doc["events"] += [
        _new_event(_template(doc, "close"), "primary/trades/9201", "2026-01-05T10:00:41Z", "pos-early-close", 0.001, 61000.0),
        _new_event(_template(doc, "non_close"), "primary/trades/9202", "2026-01-05T11:00:41Z", "pos-early-close", 0.001, 61000.0),
    ]
    _dump(fx / "export.json", doc)
    _rebind(fx)
    rc, rep = _run(fx, tmp_path)
    assert rc == 1 and rep["strict_success"] is False
    assert rep["strict_checks"]["conservation_passed"] is False
    checks = {c["check"]: c for c in rep["conservation"]["checks"]}
    identity = checks["booked_row_accounting_identity"]
    assert identity["ok"] is False and [m["event_key"] for m in identity["mismatched"]] == [row["event_key"]]
    early = checks["booked_position_inventory_never_negative:pos-early-close"]
    assert early["ok"] is False and early["min_running_inventory"] == pytest.approx(-0.001)
    assert checks["booked_report_sections_partition_events"]["ok"] is True
    assert {i["total"] for i in rep["conservation"]["informational_totals"]} == {"booked_strategy_funding_unallocated"}


STOP_KEYS = lc.STOP_EVIDENCE_FIELDS
PARITY = _load(os.path.join(os.path.dirname(FIXTURE), "stop_geometry_parity.json"))
PARITY_CASES = {c["id"]: c for c in PARITY["geometry"] + PARITY["admission"]}
UNIFIED_CLOSE = PARITY["unified_close"]
REGIME_ON = {"enabled": True, "period": 14, "adx_threshold": 20}
USER_RATCHET_REGIME = {"trailing_tp_ratchet_regime": {
    "tp_tiers": {label: [{"atr_multiple": 1.0, "trailing_mult_after": 1.0, "close_fraction": 0.0}]
                 for label in ("trending_up", "trending_down", "ranging")},
    "trailing_stop_atr_mult_regime": {"trend_regime": {
        "trending_up": {"atr_multiple": 2.75}, "trending_down": {"atr_multiple": 2.75},
        "ranging": {"atr_multiple": 1.5}}},
}}
EXTRA_CASES = {
    "user_close_default_tp_tiers": {
        "config": {"user_defaults": {"close": {"tiered_tp_atr": {"tp_tiers": [
            {"atr_multiple": 2, "close_fraction": 0.5}, {"atr_multiple": 4, "close_fraction": 1}]}}}},
        "strategy": {"stop_loss_atr_mult": 1.5, "close_strategy": {"name": "tiered_tp_atr", "params": {}}},
        "side": "long", "regime": None},
    "user_ratchet_regime_trail": {
        "config": {"user_defaults": {"close": USER_RATCHET_REGIME}, "regime": REGIME_ON},
        "strategy": {"close_strategy": {"name": "trailing_tp_ratchet_regime", "params": {"use_defaults": True}}},
        "side": "long", "regime": "trending_up"},
}


def _parity_case(case_id):
    if case_id in EXTRA_CASES:
        c = EXTRA_CASES[case_id]
        return copy.deepcopy(c["config"]), copy.deepcopy(c["strategy"]), c["side"], c["regime"]
    c = PARITY_CASES[case_id]
    cfg = copy.deepcopy(c.get("config") or {})
    if c.get("regime"):
        cfg.setdefault("regime", copy.deepcopy(PARITY["regime_enabled"]["regime"]))
    strategy = copy.deepcopy(c.get("strategy") or {})
    if c.get("unified"):
        strategy["close_strategy"] = copy.deepcopy(UNIFIED_CLOSE)
    pos = c.get("position") or {}
    return cfg, strategy, pos.get("side", strategy.get("direction") or "long"), pos.get("regime")


def _raw_strategy(strategy):
    sc = copy.deepcopy(PARITY["base_strategy"])
    sc.update(strategy)
    return sc


def _prov(value, present=True, source="synthetic test evidence"):
    return {"status": "verified", "source": source, "present": present, "value": value}


def _raw_segment(cfg, strategy):
    sc = _raw_strategy(strategy)
    platform_dd = ((cfg.get("platforms") or {}).get("hyperliquid") or {}).get("risk", {}).get("max_drawdown_pct")
    user = cfg.get("user_defaults") or {}

    def default(value):
        return _prov(value, value is not None)

    return {
        "effective_from": "2026-01-01T00:00:00Z", "effective_to": None, "status": "verified",
        "rule": "attested", "evidence": "synthetic test segment", "basis": "raw_config",
        "strategy": sc, "regime": copy.deepcopy(cfg.get("regime") or {"enabled": False}), "portfolio_risk": {},
        "stop_defaults": {
            "default_stop_loss_atr_mult": default(cfg.get("default_stop_loss_atr_mult")),
            "user_close_defaults": default(user.get("close")),
            "user_regime_atr_defaults": default(user.get("regime_atr")),
            "platform_max_drawdown_pct": default(platform_dd),
        },
        "stop_evidence": {
            "raw_fields": {k: _prov(sc.get(k), sc.get(k) is not None) for k in STOP_KEYS},
            "leverage_origin": {"status": "verified", "source": "synthetic test evidence",
                                "value": "config" if (sc.get("leverage") or 0) > 0 else "default"},
            "regime_atr_window": _prov(sc.get("regime_atr_window"), sc.get("regime_atr_window") is not None),
        },
    }


def _loader_segment(tmp_path, cfg, strategy):
    import run_backtest
    raw = _raw_segment(cfg, strategy)
    doc = copy.deepcopy(cfg)
    doc["config_version"] = 20
    doc["strategies"] = [_raw_strategy(strategy)]
    path = tmp_path / "loader_config.json"
    path.write_text(json.dumps(doc))
    from backtester import CloseCapabilityError
    try:
        loaded = run_backtest.load_strategy_config(str(path), "hl-stop-geo", inject_user_defaults=True)
        context, close_refs = loaded["capability_context"], loaded["close_strategies"]
    except CloseCapabilityError:
        stops = run_backtest.resolve_raw_config_stops(doc, "hl-stop-geo", "refused test config")
        context, close_refs = stops["stop_context"], stops["close_refs"]
    resolved = context.to_dict()["input_evidence"]["resolved_live_units"]["value"]
    sc = _raw_strategy(strategy)
    for k in STOP_KEYS:
        if resolved[k] is None:
            sc.pop(k, None)
        else:
            sc[k] = copy.deepcopy(resolved[k])
    if close_refs:
        sc["close_strategy"] = copy.deepcopy(close_refs[0])
    seg = copy.deepcopy(raw)
    seg["basis"] = "loader_resolved"
    seg["strategy"] = sc
    seg["stop_evidence"]["resolved_fields"] = {
        k: {"status": "verified", "source": "synthetic loader output", "value": copy.deepcopy(resolved[k])}
        for k in STOP_KEYS}
    return seg


def _stop_frame(side, regime, anchor=100.0, marks=(99.0, 101.0, 100.0), entry_atr=2.0):
    import pandas as pd
    n = len(marks) + 1
    opens = [anchor, anchor] + list(marks[:-1])
    closes = [anchor] + list(marks)
    df = pd.DataFrame({
        "open": opens, "high": [max(o, c) for o, c in zip(opens, closes)],
        "low": [min(o, c) for o, c in zip(opens, closes)], "close": closes,
        "volume": [1000.0] * n, "atr": [entry_atr] * n,
        "signal": [1 if side == "long" else -1] + [0] * (n - 1),
    }, index=pd.date_range("2026-03-01", periods=n, freq="1h"))
    if regime:
        df["regime"] = regime
    return df


def _engine_events(verdict, side, regime, **frame):
    from backtester import Backtester
    events = []
    bt = Backtester(initial_capital=1000.0, commission_pct=0.0, slippage_pct=0.0, platform="hyperliquid",
                    intrabar_resolution="bar_close", direction=side,
                    close_strategies=copy.deepcopy(verdict["close_refs"]) or None, **verdict["kwargs"])
    bt.run(_stop_frame(side, regime, **frame), save=False, stop_observer=events.append)
    return bt, events


EQUIVALENCE_CASES = [
    "default_scalar_atr", "default_scalar_atr_custom", "explicit_zero_atr_drawdown_fallback",
    "explicit_zero_stop_pct_disables", "default_opt_out_custom_drawdown", "platform_drawdown_fallback",
    "margin_verified_leverage", "regime_fixed_atr_user_defaults", "regime_trailing_system_defaults",
    "unified_regime_close", "user_close_default_tp_tiers", "user_ratchet_regime_trail",
]


@pytest.mark.parametrize("case_id", EQUIVALENCE_CASES)
def test_raw_and_loader_resolved_segments_resolve_identically(tmp_path, case_id):
    cfg, strategy, side, regime = _parity_case(case_id)
    raw = lc.resolve_historical_stops(_raw_segment(cfg, strategy), 2, "strict")
    loader = lc.resolve_historical_stops(_loader_segment(tmp_path, cfg, strategy), 2, "strict")
    assert raw["status"] == loader["status"] == "modeled", (raw["refusals"], loader["refusals"])
    assert raw["owner"] == loader["owner"]
    assert lc._jsonable_stop_inputs(raw["kwargs"]) == lc._jsonable_stop_inputs(loader["kwargs"])
    assert raw["capability_context"] == loader["capability_context"]
    assert raw["close_refs"] == loader["close_refs"]
    raw_bt, raw_events = _engine_events(raw, side, regime)
    load_bt, load_events = _engine_events(loader, side, regime)
    assert raw_bt._stop_owner == load_bt._stop_owner == raw["owner"]
    assert raw_events == load_events
    arms = [e for e in raw_events if e["event"] == "arm"]
    assert len(arms) == 1
    if raw["owner"] != "none":
        assert arms[0]["trigger"] > 0


def test_defaults_apply_in_order_and_convert_once(tmp_path):
    cfg, strategy, _, _ = _parity_case("default_scalar_atr_custom")
    raw = lc.resolve_historical_stops(_raw_segment(cfg, strategy), 2, "strict")
    assert raw["owner"] == "fixed_atr" and raw["kwargs"]["stop_loss_atr_mult"] == 2.5
    assert raw["kwargs"]["max_drawdown_pct"] == pytest.approx(0.5)
    assert raw["capability_context"]["raw_fields"]["stop_loss_atr_mult"] == {"present": False, "value": None}
    cfg, strategy, _, _ = _parity_case("regime_fixed_atr_user_defaults")
    raw = lc.resolve_historical_stops(_raw_segment(cfg, strategy), 2, "strict")
    assert raw["owner"] == "fixed_atr_regime"
    assert raw["kwargs"]["stop_loss_atr_mult"] is None
    assert raw["kwargs"]["stop_loss_atr_mult_regime"] == cfg["user_defaults"]["regime_atr"]["stop_loss_atr_mult_regime"]
    cfg, strategy, _, _ = _parity_case("pct_long")
    seg = _loader_segment(tmp_path, cfg, strategy)
    loader = lc.resolve_historical_stops(seg, 2, "strict")
    assert loader["kwargs"]["stop_loss_pct"] == pytest.approx(0.02)
    assert loader["resolved_live_units"]["stop_loss_pct"] == 2
    seg["stop_defaults"]["default_stop_loss_atr_mult"] = _prov(3.0)
    seg["stop_evidence"]["resolved_fields"]["stop_loss_pct"]["value"] = 2
    again = lc.resolve_historical_stops(seg, 2, "strict")
    assert again["kwargs"]["stop_loss_atr_mult"] is None and again["owner"] == "fixed_pct"


def test_drawdown_fallback_geometry_matches_the_platform_oracle_case():
    case = PARITY_CASES["platform_drawdown_fallback"]
    want = PARITY["expected"]["geometry"]["platform_drawdown_fallback"]
    cfg, strategy, side, regime = _parity_case("platform_drawdown_fallback")
    verdict = lc.resolve_historical_stops(_raw_segment(cfg, strategy), 2, "strict")
    assert verdict["status"] == "modeled" and verdict["owner"] == "drawdown_fallback"
    assert verdict["required_inputs"] >= ["max_drawdown_pct", "platform_max_drawdown_pct"][:1]
    assert "platform_max_drawdown_pct" in verdict["required_inputs"]
    pos = case["position"]
    _, events = _engine_events(verdict, side, regime, anchor=float(pos["anchor"]),
                               marks=tuple(float(m) for m in case["marks"]), entry_atr=float(pos["entry_atr"]))
    arm = next(e for e in events if e["event"] == "arm")
    assert abs(arm["trigger"] - want["arm_trigger"]) <= PARITY["tolerance"] * max(1.0, abs(want["arm_trigger"]))
    seg = _raw_segment(cfg, strategy)
    seg["stop_defaults"]["platform_max_drawdown_pct"]["status"] = "unverified"
    refused = lc.resolve_historical_stops(seg, 2, "strict")
    assert [(r["reason"], r["field"]) for r in refused["refusals"]] == [
        ("stop_inputs_unverified", "platform_max_drawdown_pct")]


@pytest.mark.parametrize("basis", ["raw_config", "loader_resolved"])
def test_both_bases_refuse_unverified_margin_leverage_and_unsupported_windows(tmp_path, basis):
    def build(case_id, extra_cfg=None, extra_strategy=None):
        cfg, strategy, _, _ = _parity_case(case_id)
        cfg.update(extra_cfg or {})
        strategy.update(extra_strategy or {})
        return (_raw_segment(cfg, strategy) if basis == "raw_config"
                else _loader_segment(tmp_path, cfg, strategy))

    def codes(seg):
        return {(r["reason"], r.get("reason_code")) for r in lc.resolve_historical_stops(seg, 2, "strict")["refusals"]}

    want_margin = ("stop_capability_refused", "UNVERIFIED_MARGIN_LEVERAGE")
    assert want_margin in codes(build("margin_with_defaulted_leverage"))
    seg = build("margin_verified_leverage")
    assert lc.resolve_historical_stops(seg, 2, "strict")["status"] == "modeled"
    if basis == "raw_config":
        seg["stop_evidence"]["raw_fields"]["leverage"]["status"] = "unverified"
    else:
        seg["stop_evidence"]["leverage_origin"]["status"] = "unverified"
    assert want_margin in codes(seg)
    named = build("named_atr_window_not_modeled")
    assert ("stop_capability_refused", "MISSING_STOP_INPUT") in codes(named)


@pytest.mark.parametrize("basis", ["raw_config", "loader_resolved"])
def test_contradictory_stop_evidence_refuses_before_simulation(tmp_path, basis):
    cfg, strategy, _, _ = _parity_case("pct_long")
    seg = _raw_segment(cfg, strategy) if basis == "raw_config" else _loader_segment(tmp_path, cfg, strategy)
    seg["stop_evidence"]["raw_fields"]["stop_loss_pct"] = _prov(3)
    verdict = lc.resolve_historical_stops(seg, 2, "strict")
    assert ("stop_evidence_contradictory", "stop_loss_pct") in {(r["reason"], r["field"]) for r in verdict["refusals"]}
    assert all(r["approximable"] is False for r in verdict["refusals"] if r["reason"] == "stop_evidence_contradictory")
    seg = _raw_segment(cfg, strategy) if basis == "raw_config" else _loader_segment(tmp_path, cfg, strategy)
    seg["stop_evidence"]["raw_fields"]["trailing_stop_pct"]["status"] = "unverified"
    verdict = lc.resolve_historical_stops(seg, 2, "strict")
    assert [(r["reason"], r["field"]) for r in verdict["refusals"]] == [("stop_inputs_unverified", "trailing_stop_pct")]


@pytest.mark.parametrize("case_id,owner,fields", [
    ("pct_long", "fixed_pct", {"basis", "stop_loss_pct"}),
    ("default_scalar_atr", "fixed_atr", {"basis", "stop_loss_atr_mult"}),
    ("unified_regime_close", "unified_regime", {"basis", "close_strategy", "regime_atr_window"}),
    ("explicit_zero_atr_drawdown_fallback", "drawdown_fallback", {"basis", "max_drawdown_pct"}),
])
def test_version_one_segments_cannot_rule_out_a_protective_owner(case_id, owner, fields):
    cfg, strategy, _, _ = _parity_case(case_id)
    seg = _raw_segment(cfg, strategy)
    for key in ("basis", "stop_defaults", "stop_evidence"):
        seg.pop(key)
    verdict = lc.resolve_historical_stops(seg, 1, "strict")
    assert verdict["owner"] == owner and verdict["status"] == "refused"
    assert {r["reason"] for r in verdict["refusals"]} == {"stop_inputs_unverified"}
    assert {r["field"] for r in verdict["refusals"]} == fields


def _as_raw_basis(cin):
    seg = cin["historical_configuration"]["timeline"][0]
    src = next(s for s in _load(os.path.join(FIXTURE, "source", "config.json"))["strategies"]
               if s["id"] == seg["strategy"]["id"])
    seg["basis"] = "raw_config"
    seg["strategy"] = copy.deepcopy(src)
    seg["stop_evidence"].pop("resolved_fields")
    return cin


def _stop_free_loader_segment(seg):
    for k in ("stop_loss_atr_mult",):
        seg["strategy"].pop(k, None)
        seg["stop_evidence"]["raw_fields"][k] = {"status": "verified", "source": "test edit", "present": False,
                                                 "value": None}
        seg["stop_evidence"]["resolved_fields"][k]["value"] = None


def test_strict_fixture_arms_the_drawdown_fallback_on_both_bases(tmp_path):
    fx = _copy(tmp_path)
    rc, rep = _run(fx, tmp_path, name="loader.json")
    assert rc == 0 and rep["outcome"] == "strict_success"
    assert rep["provenance"]["comparison_input_schema"] == [lc.INPUT_SCHEMA, 2]
    stops = rep["stops"]
    assert stops["basis"] == "loader_resolved" and stops["owner"] == "drawdown_fallback"
    assert stops["status"] == "modeled" and stops["simulated_owner"] == "drawdown_fallback"
    assert stops["engine_inputs"]["max_drawdown_pct"] == pytest.approx(0.5)
    assert stops["engine_inputs"]["stop_loss_atr_mult"] == 0
    arms = stops["arm_events"]
    assert len(arms) == len(rep["matching"]["matched"]) == 2
    for arm in arms:
        assert arm["geometry"] == "percent" and arm["trigger"] == pytest.approx(arm["anchor"] * 0.5)
    assert not any(r["reason"] == "capability_protection" for r in rep["eligibility"]["refusals"])
    rows = {(r["field"], r["category"]): r for r in rep["eligibility"]["capability_matrix"]}
    assert rows[("max_drawdown_pct", "portfolio_controls")]["decision"] == "requires_evidence_verified"
    assert rows[("max_drawdown_pct", "protection")]["decision"] == "modeled"
    assert rep["initial_stop_geometry"]["counts"]["unavailable"] == 2

    _dump(fx / "comparison_input.json", _as_raw_basis(_load(fx / "comparison_input.json")))
    rc, raw_rep = _run(fx, tmp_path, name="raw.json")
    assert rc == 0 and raw_rep["outcome"] == "strict_success"
    assert raw_rep["stops"]["basis"] == "raw_config"
    for key in ("owner", "engine_inputs", "capability_context", "arm_events"):
        assert raw_rep["stops"][key] == stops[key], key

    rc, approx = _run(fx, tmp_path, mode="approximate", name="approx.json")
    assert rc == 1 and approx["outcome"] == "incomplete"
    assert approx["stops"]["engine_inputs"] == stops["engine_inputs"]
    assert approx["stops"]["arm_events"] == stops["arm_events"]


def _drawdown_free_fixture(fx, resolved_entry):
    cin = _load(fx / "comparison_input.json")
    seg = cin["historical_configuration"]["timeline"][0]
    seg["strategy"].pop("max_drawdown_pct")
    if resolved_entry is None:
        seg["stop_evidence"]["resolved_fields"].pop("max_drawdown_pct")
    else:
        seg["stop_evidence"]["resolved_fields"]["max_drawdown_pct"] = resolved_entry
    _dump(fx / "comparison_input.json", cin)


def _stop_refusals(rep):
    return {(r["reason"], r["field"]) for r in rep["eligibility"]["refusals"]
            if r["reason"].startswith("stop_")}


def test_loader_drawdown_without_verified_provenance_never_reaches_strict_success(tmp_path):
    fx = _copy(tmp_path)
    _drawdown_free_fixture(fx, None)
    rc, rep = _run(fx, tmp_path, name="missing.json")
    assert rc == 1 and rep["outcome"] != "strict_success"
    assert rep["stops"]["owner"] == "none" and rep["stops"]["status"] == "refused"
    assert _stop_refusals(rep) == {("stop_inputs_unverified", "max_drawdown_pct")}

    fx = _copy(tmp_path / "null")
    _drawdown_free_fixture(fx, {"status": "verified", "source": "test edit", "value": None})
    rc, rep = _run(fx, tmp_path, name="null.json")
    assert rc == 1 and rep["outcome"] != "strict_success"
    assert rep["stops"]["status"] == "refused"
    assert ("stop_evidence_contradictory", "max_drawdown_pct") in _stop_refusals(rep)


def test_drawdown_provenance_is_required_only_where_precedence_reaches_it(tmp_path):
    cfg, strategy, _, _ = _parity_case("explicit_zero_stop_pct_disables")
    seg = _loader_segment(tmp_path, cfg, strategy)
    seg["stop_evidence"]["resolved_fields"]["max_drawdown_pct"]["status"] = "unverified"
    verdict = lc.resolve_historical_stops(seg, 2, "strict")
    assert verdict["owner"] == "none" and verdict["status"] == "modeled", verdict["refusals"]
    assert "max_drawdown_pct" not in verdict["required_inputs"]
    cfg, strategy, _, _ = _parity_case("platform_drawdown_fallback")
    seg = _raw_segment(cfg, dict(strategy, max_drawdown_pct=-5))
    verdict = lc.resolve_historical_stops(seg, 2, "strict")
    assert verdict["owner"] == "none" and verdict["status"] == "refused"
    assert ("stop_configuration_invalid", "max_drawdown_pct") in {
        (r["reason"], r["field"]) for r in verdict["refusals"]}


@pytest.mark.parametrize("basis", ["raw_config", "loader_resolved"])
def test_unverified_leverage_refuses_a_margin_stop_on_both_bases(tmp_path, basis):
    strategy = {"stop_loss_margin_pct": 10, "leverage": 1}
    if basis == "raw_config":
        seg = _raw_segment({}, strategy)
        seg["stop_evidence"]["raw_fields"]["leverage"]["status"] = "unverified"
    else:
        seg = _loader_segment(tmp_path, {}, strategy)
        assert seg["stop_evidence"]["leverage_origin"] == {
            "status": "verified", "source": "synthetic test evidence", "value": "config"}
        seg["stop_evidence"]["resolved_fields"]["leverage"]["status"] = "unverified"
    verdict = lc.resolve_historical_stops(seg, 2, "strict")
    assert verdict["owner"] == "margin_pct" and verdict["status"] == "refused"
    assert {(r["reason"], r.get("reason_code")) for r in verdict["refusals"]} == {
        ("stop_capability_refused", "UNVERIFIED_MARGIN_LEVERAGE")}


def test_loader_scalar_default_follows_the_live_default_rule_under_a_unified_close(tmp_path):
    cfg, strategy, _, _ = _parity_case("unified_regime_close")
    seg = _loader_segment(tmp_path, cfg, strategy)
    assert seg["stop_evidence"]["resolved_fields"]["stop_loss_atr_mult"]["value"] is None
    verdict = lc.resolve_historical_stops(seg, 2, "strict")
    assert verdict["owner"] == "unified_regime" and verdict["status"] == "modeled", verdict["refusals"]
    seg["strategy"]["stop_loss_atr_mult"] = 1.0
    seg["stop_evidence"]["resolved_fields"]["stop_loss_atr_mult"]["value"] = 1.0
    verdict = lc.resolve_historical_stops(seg, 2, "strict")
    assert ("stop_evidence_contradictory", "stop_loss_atr_mult") in {
        (r["reason"], r["field"]) for r in verdict["refusals"]}
    cfg, strategy, _, _ = _parity_case("default_scalar_atr")
    seg = _loader_segment(tmp_path, cfg, strategy)
    assert seg["stop_evidence"]["resolved_fields"]["stop_loss_atr_mult"]["value"] == 1.0
    assert lc.resolve_historical_stops(seg, 2, "strict")["status"] == "modeled"


def test_version_one_input_is_read_and_refused_for_unverified_stops(tmp_path):
    fx = _copy(tmp_path)
    cin = _load(fx / "comparison_input.json")
    cin["schema_version"] = 1
    for key in ("basis", "stop_defaults", "stop_evidence", "atr_defaults"):
        cin["historical_configuration"]["timeline"][0].pop(key)
    _dump(fx / "comparison_input.json", cin)
    rc, rep = _run(fx, tmp_path)
    assert rc == 1 and rep["outcome"] == "refused"
    assert rep["provenance"]["comparison_input_schema"] == [lc.INPUT_SCHEMA, 1]
    stop = [r for r in rep["eligibility"]["refusals"] if r["reason"] == "stop_inputs_unverified"]
    assert {r["field"] for r in stop} == {"basis", "max_drawdown_pct"}
    assert rep["simulation"]["status"] == "not_run"
    rc, rep = _run(fx, tmp_path, mode="approximate", name="approx.json")
    assert rc == 1 and rep["outcome"] == "incomplete" and rep["simulation"]["status"] == "run"
    assert {"basis", "max_drawdown_pct"} <= {a["feature"] for a in rep["eligibility"]["approximations"]}
    assert rep["stops"]["owner"] == "drawdown_fallback" and rep["stops"]["arm_events"]


def _frame_atr(fx, method):
    import pandas as pd
    from atr import ensure_atr_indicator
    m = om.load_manifest(str(fx / "market" / "manifest.json"))
    frame, _, _ = om.window_frame(m, m["datasets"][0], "comparison")
    frame = ensure_atr_indicator(frame.copy(), method=method)
    return {str(pd.to_datetime(int(t), unit="ms")): float(v) for t, v in zip(frame["timestamp"], frame["atr"])}


def _arm_atrs(rep):
    import pandas as pd
    return [(str(pd.Timestamp(a["date"]) - pd.Timedelta(hours=1)), a["entry_atr"]) for a in rep["stops"]["arm_events"]]


def _set_root_atr(fx, entry):
    cin = _load(fx / "comparison_input.json")
    seg = cin["historical_configuration"]["timeline"][0]
    if entry is None:
        seg.pop("atr_defaults")
    else:
        seg["atr_defaults"] = {"root_atr_method": entry}
    _dump(fx / "comparison_input.json", cin)
    return cin


def test_verified_root_atr_method_sets_the_simulated_entry_atr(tmp_path):
    fx = _copy(tmp_path)
    rc, rep = _run(fx, tmp_path, name="absent.json")
    assert rc == 0 and rep["outcome"] == "strict_success"
    assert (rep["atr_method"]["method"], rep["atr_method"]["source"]) == ("simple", "live_default")
    simple = _frame_atr(fx, "simple")
    assert rep["stops"]["arm_events"] and all(atr == simple[bar] for bar, atr in _arm_atrs(rep))

    _set_root_atr(fx, {"status": "verified", "source": "test edit: root atr_method wilder", "present": True,
                       "value": "wilder"})
    rc, rep = _run(fx, tmp_path, name="wilder.json")
    assert rc == 0 and rep["outcome"] == "strict_success"
    assert (rep["atr_method"]["method"], rep["atr_method"]["source"]) == ("wilder", "root")
    rows = {(r["field"], r["category"]): r for r in rep["eligibility"]["capability_matrix"]}
    assert rows[("atr_method", "signal_model")]["decision"] == "modeled"
    assert rows[("atr_method", "signal_model")]["value"] == "wilder"
    wilder = _frame_atr(fx, "wilder")
    arms = _arm_atrs(rep)
    assert arms and all(atr == wilder[bar] and atr != simple[bar] for bar, atr in arms)


def test_atr_method_without_verified_root_evidence_is_refused_in_strict_mode(tmp_path):
    fx = _copy(tmp_path)
    _set_root_atr(fx, None)
    rc, rep = _run(fx, tmp_path, name="missing.json")
    assert rc == 1 and rep["outcome"] == "refused" and rep["simulation"]["status"] == "not_run"
    assert ("atr_method_unverified", "atr_defaults.root_atr_method") in {
        (r["reason"], r.get("field")) for r in rep["eligibility"]["refusals"]}
    rc, rep = _run(fx, tmp_path, mode="approximate", name="missing_approx.json")
    assert rc == 1 and rep["outcome"] == "incomplete" and rep["simulation"]["status"] == "run"
    assert "atr_method" in {a["feature"] for a in rep["eligibility"]["approximations"]}
    assert rep["atr_method"]["source"] == "unverified_substitute"

    _set_root_atr(fx, {"status": "unverified", "source": None, "present": True, "value": "wilder"})
    rc, rep = _run(fx, tmp_path, name="unverified.json")
    assert rc == 1 and "atr_method_unverified" in {r["reason"] for r in rep["eligibility"]["refusals"]}
    rc, rep = _run(fx, tmp_path, mode="approximate", name="unverified_approx.json")
    wilder = _frame_atr(fx, "wilder")
    assert rep["atr_method"]["method"] == "wilder"
    assert rep["stops"]["arm_events"] and all(atr == wilder[bar] for bar, atr in _arm_atrs(rep))


def test_strategy_atr_method_overrides_root_and_an_invalid_value_is_never_simulated(tmp_path):
    fx = _copy(tmp_path)
    cin = _set_root_atr(fx, {"status": "verified", "source": "test edit: root atr_method wilder", "present": True,
                             "value": "wilder"})
    cin["historical_configuration"]["timeline"][0]["strategy"]["atr_method"] = "Simple"
    _dump(fx / "comparison_input.json", cin)
    rc, rep = _run(fx, tmp_path, name="override.json")
    assert rc == 0 and (rep["atr_method"]["method"], rep["atr_method"]["source"]) == ("simple", "strategy")
    simple = _frame_atr(fx, "simple")
    assert all(atr == simple[bar] for bar, atr in _arm_atrs(rep))

    cin["historical_configuration"]["timeline"][0]["strategy"].pop("atr_method")
    cin["historical_configuration"]["timeline"][0]["atr_defaults"]["root_atr_method"]["value"] = "ema"
    _dump(fx / "comparison_input.json", cin)
    for mode in ("strict", "approximate"):
        rc, rep = _run(fx, tmp_path, mode=mode, name=f"invalid_{mode}.json")
        assert rc == 1 and rep["simulation"]["status"] == "not_run"
        assert ("atr_configuration_invalid", "atr_defaults.root_atr_method") in {
            (r["reason"], r.get("field")) for r in rep["eligibility"]["refusals"]}


def test_an_invalid_atr_method_is_refused_unverified_or_unread(tmp_path):
    fx = _copy(tmp_path)
    _set_root_atr(fx, {"status": "unverified", "source": None, "present": True, "value": "ema"})
    rc, rep = _run(fx, tmp_path, mode="approximate", name="unverified_invalid.json")
    assert rep["atr_method"]["needs_atr"] is True
    assert rc == 1 and rep["simulation"]["status"] == "not_run"
    assert ("atr_configuration_invalid", "atr_defaults.root_atr_method") in {
        (r["reason"], r.get("field")) for r in rep["eligibility"]["refusals"]}

    cin = _set_root_atr(fx, {"status": "verified", "source": "test edit: no root atr_method", "present": False,
                             "value": None})
    seg = cin["historical_configuration"]["timeline"][0]
    seg["strategy"]["atr_method"] = "ema"
    seg["strategy"].pop("close_strategy")
    _dump(fx / "comparison_input.json", cin)
    rc, rep = _run(fx, tmp_path, name="unread_invalid.json")
    assert rep["atr_method"]["needs_atr"] is False and rep["stops"]["owner"] == "drawdown_fallback"
    assert rc == 1 and rep["outcome"] == "refused"
    assert ("atr_configuration_invalid", "strategy.atr_method") in {
        (r["reason"], r.get("field")) for r in rep["eligibility"]["refusals"]}


@pytest.mark.parametrize("variant", ["unified_close", "trailing_regime"])
def test_regime_owned_stops_simulate_with_labels_and_no_gating(tmp_path, variant):
    from regime import valid_labels_for_classifier
    fx = _copy(tmp_path)
    _rehash_market(fx, warmup=72)
    base = _load(fx / "comparison_input.json")
    cin = copy.deepcopy(base)
    seg = cin["historical_configuration"]["timeline"][0]
    seg["regime"] = dict(REGIME_ON)
    _stop_free_loader_segment(seg)
    if variant == "unified_close":
        seg["strategy"]["close_strategy"] = copy.deepcopy(UNIFIED_CLOSE)
        owner = "unified_regime"
    else:
        block = {"use_defaults": True}
        seg["strategy"]["trailing_stop_atr_mult_regime"] = block
        seg["stop_evidence"]["raw_fields"]["trailing_stop_atr_mult_regime"] = {
            "status": "verified", "source": "test edit", "present": True, "value": block}
        seg["stop_evidence"]["resolved_fields"]["trailing_stop_atr_mult_regime"]["value"] = block
        owner = "trailing_atr_regime"
    _dump(fx / "comparison_input.json", cin)
    rc, rep = _run(fx, tmp_path, name=f"{variant}.json")
    checks = rep["eligibility"]["market"]["strategy_checks"]["indicator_history"]
    assert {"atr", "regime"} <= set(checks["compared_columns"]) and checks["ok"] is True
    assert rep["stops"]["owner"] == owner and rep["stops"]["status"] == "modeled"
    assert rep["simulation"]["status"] == "run", rep["eligibility"]["refusals"]
    assert not any(r["reason"] in ("capability_regime", "capability_protection") for r in rep["eligibility"]["refusals"])
    assert rep["stops"]["labels"]["enabled"] is True
    labels = set(valid_labels_for_classifier("adx"))
    arms = rep["stops"]["arm_events"]
    assert arms and all(a["trigger"] > 0 and a["regime"] in labels for a in arms)
    rows = {(r["field"], r["category"]): r for r in rep["eligibility"]["capability_matrix"]}
    assert rows[("regime.enabled", "regime")]["decision"] == "modeled"
    assert rows[("allowed_regimes", "regime")]["decision"] == "inactive"
    assert rep["eligibility"]["regime"]["protection_source"] == "shifted_decision_bar:primary_column"
    assert rep["eligibility"]["regime"]["timing"]["protection"] == "shifted_decision_bar"

    seg["strategy"]["allowed_regimes"] = ["trending_up"]
    _dump(fx / "comparison_input.json", cin)
    rc, rep = _run(fx, tmp_path, name=f"{variant}_gated.json")
    assert rc == 1 and rep["outcome"] == "refused"
    assert ("regime_lookback_insufficient", "allowed_regimes") in {(r["reason"], r.get("field"))
                                                                   for r in rep["eligibility"]["refusals"]}

    seg["strategy"].pop("allowed_regimes")
    seg["regime"]["enabled"] = False
    _dump(fx / "comparison_input.json", cin)
    rc, rep = _run(fx, tmp_path, name=f"{variant}_disabled.json")
    assert rc == 1 and ("stop_capability_refused", "MISSING_STOP_INPUT") in {
        (r["reason"], r.get("reason_code")) for r in rep["eligibility"]["refusals"]}


def test_regime_labels_that_depend_on_warmup_refuse_strict(tmp_path):
    fx = _copy(tmp_path)
    cin = _load(fx / "comparison_input.json")
    seg = cin["historical_configuration"]["timeline"][0]
    seg["regime"] = dict(REGIME_ON)
    _stop_free_loader_segment(seg)
    seg["strategy"]["close_strategy"] = copy.deepcopy(UNIFIED_CLOSE)
    _dump(fx / "comparison_input.json", cin)
    rc, rep = _run(fx, tmp_path)
    assert rc == 1 and rep["outcome"] == "refused" and rep["simulation"]["status"] == "not_run"
    assert {r["reason"] for r in rep["eligibility"]["refusals"]} == {"market_indicator_history_insufficient"}
    assert rep["stops"]["status"] == "modeled"


def _synthetic_stamp(value):
    return {"value": value, "raw_value": value, "status": "available", "reason": None,
            "provenance": [{"kind": "synthetic_test_evidence", "source_role": "primary", "source_table": "trades",
                            "source_row_id": "test", "source_field": "inserted by test_ledger_compare"}]}


def _geometry_fixture(tmp_path, scale=1.0, attest=True, stamp_close=False, name="g"):
    fx = _copy(tmp_path / name)
    _, base = _run(fx, tmp_path, name=f"{name}_base.json")
    doc = _load(fx / "export.json")
    events = {e["event_key"]: e for e in doc["events"]}
    cin = _load(fx / "comparison_input.json")
    attested = {}
    for res in base["initial_stop_geometry"]["positions"]:
        key = res["booked_event_key"]
        if stamp_close:
            pid = res["booked_position_id"]
            key = next(e["event_key"] for e in doc["events"]
                       if e["position_id"]["value"] == pid and e["event_kind"]["value"] == "close")
        events[key]["stop_loss_trigger_px"] = _synthetic_stamp(res["simulated_trigger"] * scale)
        events[key]["entry_atr"] = _synthetic_stamp(res["simulated_entry_atr"])
        attested[key] = {"status": "verified", "source": "synthetic test evidence: initial-entry stamp",
                         "stamp": "initial_entry"}
    if attest:
        cin["initial_stop_geometry_evidence"] = attested
    _dump(fx / "export.json", doc)
    _dump(fx / "comparison_input.json", cin)
    _rebind(fx)
    return fx


def test_verified_initial_geometry_agrees_and_an_altered_trigger_mismatches(tmp_path):
    fx = _geometry_fixture(tmp_path, name="agree")
    rc, rep = _run(fx, tmp_path, name="agree.json")
    assert rc == 0 and rep["outcome"] == "strict_success"
    geo = rep["initial_stop_geometry"]
    assert geo["counts"]["agreement"] == 2
    for res in geo["positions"]:
        assert res["booked_entry_atr"] == res["simulated_entry_atr"] and res["relative_delta"] == pytest.approx(0.0)
    assert all(m["initial_stop_geometry"]["status"] == "agreement" for m in rep["matching"]["matched"])

    fx = _geometry_fixture(tmp_path, scale=1.01, name="altered")
    rc, rep = _run(fx, tmp_path, name="altered.json")
    assert rc == 1 and rep["outcome"] == "mismatch"
    assert rep["initial_stop_geometry"]["counts"]["mismatch"] == 2
    assert rep["strict_checks"]["initial_stop_geometry_consistent"] is False
    assert rep["strict_checks"]["matched_within_tolerance"] is False
    assert all(m["within_tolerance"] is False for m in rep["matching"]["matched"])


@pytest.mark.parametrize("variant", ["no_attestation", "later_stamp", "ambiguous_arms"])
def test_unproven_initial_geometry_never_reports_agreement(tmp_path, monkeypatch, variant):
    fx = _geometry_fixture(tmp_path, attest=variant != "no_attestation", stamp_close=variant == "later_stamp",
                           name=variant)
    if variant == "ambiguous_arms":
        from backtester import Backtester
        original = Backtester._emit_stop_event

        def doubled(self, event, **fields):
            original(self, event, **fields)
            if event == "arm":
                original(self, event, **fields)

        monkeypatch.setattr(Backtester, "_emit_stop_event", doubled)
    rc, rep = _run(fx, tmp_path, name=f"{variant}.json")
    assert rc == 1 and rep["strict_success"] is False and rep["outcome"] == "unverified"
    geo = rep["initial_stop_geometry"]
    assert geo["counts"]["agreement"] == 0 and geo["counts"]["unverified"] == 2
    assert rep["strict_checks"]["initial_stop_geometry_consistent"] is False
    assert {u["input"] for u in rep["eligibility"]["unverified"]} == {"initial_stop_geometry"}
    if variant == "later_stamp":
        assert all(r["later_stamps"] for r in geo["positions"])


@pytest.mark.parametrize("mutate", [
    lambda seg: seg.pop("basis"),
    lambda seg: seg["stop_defaults"].pop("user_close_defaults"),
    lambda seg: seg["stop_evidence"]["raw_fields"].update(bogus=seg["stop_evidence"]["raw_fields"]["leverage"]),
    lambda seg: seg["stop_evidence"]["raw_fields"]["leverage"].update(status="verified", source=""),
    lambda seg: seg["stop_evidence"]["raw_fields"]["trailing_stop_pct"].update(value=1),
    lambda seg: seg["stop_evidence"].pop("resolved_fields"),
])
def test_malformed_stop_evidence_exits_without_a_report(tmp_path, mutate):
    fx = _copy(tmp_path)
    cin = _load(fx / "comparison_input.json")
    mutate(cin["historical_configuration"]["timeline"][0])
    _dump(fx / "comparison_input.json", cin)
    rc, rep = _run(fx, tmp_path)
    assert rc == lc.EXIT_INPUT_ERROR and rep is None


REGIME_WINDOWS = {
    "short": {"classifier": "adx", "period": 8, "adx_threshold": 20},
    "medium": {"classifier": "adx", "period": 28, "adx_threshold": 20},
}
DIRECTIONAL_POLICY = {"trend_regime": {
    "trending_up": {"direction": "long", "invert_signal": False},
    "trending_down": {"direction": "short", "invert_signal": False},
    "ranging": {"direction": "both", "invert_signal": False},
}}
INTERVAL_END = "2026-01-06T20:00:00Z"


def _codes(rep):
    return {(r["reason"], r.get("field")) for r in rep["eligibility"]["refusals"]}


def _segment(fx):
    cin = _load(fx / "comparison_input.json")
    return cin, cin["historical_configuration"]["timeline"][0]


def _binding(seg):
    from regime_context import configuration_sha256
    return {
        "partition": "live",
        "strategy_id": "hl-strict-btc",
        "configuration_sha256": configuration_sha256(seg["strategy"], seg["regime"]),
        "interval": {"start": INTERVAL_START, "end": INTERVAL_END},
    }


def _verified_evidence(binding, **extra):
    entry = {"status": "verified", "evidence": "fixture evidence for issue 1730",
             "rule": "bound to this partition, strategy, configuration and the whole interval",
             "binding": binding, "coverage": "complete_interval"}
    entry.update(extra)
    return entry


def _write_json(fx, name, payload):
    path = fx / name
    _dump(path, payload)
    return name, _sha(path)


def _use_regime_market(cin, fx):
    cin["market"]["manifest"]["path"] = "regime_market/manifest.json"
    cin["market"]["manifest"]["sha256"] = _sha(fx / "regime_market" / "manifest.json")


def _cert_artifact(states, expires_at=None, extra_entry=None, unknown_key=False):
    entry = {"asset": "BTC", "timeframe": "1h", "classifier": "adx",
             "generated_at": "2026-01-01T00:00:00Z", "states": states}
    if expires_at:
        entry["expires_at"] = expires_at
    certified = [entry]
    if extra_entry is not None:
        certified.append(extra_entry)
    payload = {"schema_version": 1, "generated_at": "2026-01-01T00:00:00Z",
               "generator": "test", "source_evidence": "fixture", "criteria": {},
               "default_ttl_days": 30, "certified": certified}
    if unknown_key:
        payload["notes"] = "not a Go field"
    return payload


def test_live_shape_regime_with_no_consumer_is_inactive(tmp_path):
    fx = _copy(tmp_path)
    cin, seg = _segment(fx)
    seg["regime"] = {"enabled": True, "period": 14, "adx_threshold": 20, "windows": REGIME_WINDOWS}
    seg["strategy"]["regime_gate_window"] = "short"
    seg["strategy"]["regime_directional_window"] = "short"
    seg["strategy"]["regime_directional_policy"] = DIRECTIONAL_POLICY
    cin.setdefault("capability_evidence", {})["directional_certification"] = _verified_evidence(
        _binding(seg), reloads="none", effective="empty")
    _dump(fx / "comparison_input.json", cin)
    rc, rep = _run(fx, tmp_path)
    assert rc == 0 and rep["outcome"] == "strict_success"
    rows = {(r["field"], r["category"]): r for r in rep["eligibility"]["capability_matrix"]}
    for field in ("allowed_regimes", "regime_gate_window", "regime_directional_window",
                  "regime_directional_policy"):
        assert rows[(field, "regime")]["decision"] == "inactive", rows[(field, "regime")]
        assert rows[(field, "regime")]["reason_code"] in (
            "regime_selector_no_consumer", "directional_policy_uncertified")
    assert not any(r[0].startswith("regime_") or r[0].startswith("directional_") for r in _codes(rep))

    cin["capability_evidence"].pop("directional_certification")
    _dump(fx / "comparison_input.json", cin)
    rc, rep = _run(fx, tmp_path, mode="strict", name="no_cert.json")
    assert rc == 1 and ("directional_certification_unverified", "regime_directional_policy") in _codes(rep)
    rc, rep = _run(fx, tmp_path, mode="approximate", name="no_cert_approx.json")
    assert rc == 1 and rep["outcome"] == "incomplete"
    assert any(a.get("reason_code") == "directional_certification_unverified"
               for a in rep["eligibility"]["approximations"])


def test_named_gate_uses_its_window_and_refuses_without_label_evidence(tmp_path):
    fx = _copy(tmp_path)
    cin, seg = _segment(fx)
    _use_regime_market(cin, fx)
    seg["regime"] = {"enabled": True, "period": 14, "adx_threshold": 20, "windows": REGIME_WINDOWS}
    seg["strategy"]["closed_bar_decisions"] = True
    seg["strategy"]["direction"] = "both"
    seg["strategy"]["allowed_regimes"] = ["trending_down"]
    seg["strategy"]["regime_gate_window"] = "short"
    rel, digest = _write_json(fx, "regime_labels.json", {"values_and_timing": True})
    cin.setdefault("capability_evidence", {})["regime_labels"] = _verified_evidence(
        _binding(seg), artifact={"path": rel, "sha256": digest})
    _dump(fx / "comparison_input.json", cin)
    rc, rep = _run(fx, tmp_path, name="named.json")
    assert rep["simulation"]["status"] == "run", rep["eligibility"]["refusals"]
    rows = {(r["field"], r["category"]): r for r in rep["eligibility"]["capability_matrix"]}
    assert rows[("allowed_regimes", "regime")]["decision"] == "modeled"
    assert rows[("allowed_regimes", "regime")]["reason_code"] == "regime_gate_modeled"
    scored = rep["eligibility"]["regime"]["scored_labels"]
    assert scored

    import gzip
    import pandas as pd
    from regime import prepare_check_regime
    with gzip.open(fx / "regime_market" / "BTC_1h_candles.csv.gz", "rt") as fh:
        candles = pd.read_csv(fh)
    limit = rep["eligibility"]["regime"]["lookback"]
    column = "regime_w_short"
    by_ts = {int(row["timestamp"]): i for i, row in candles.iterrows()}
    for row in scored:
        i = by_ts[row["timestamp"]]
        payload, _, _ = prepare_check_regime(
            candles.iloc[i - limit + 1:i + 1], regime_enabled=True, period=14, adx_threshold=20.0,
            windows_spec=REGIME_WINDOWS)
        assert row[column] == payload["short"]["regime"]

    named_sides = [(p["side"], p["opened_qty"]) for p in rep["simulated"]["positions"]]
    seg["strategy"]["regime_gate_window"] = "medium"
    cin["capability_evidence"]["regime_labels"]["binding"] = _binding(seg)
    _dump(fx / "comparison_input.json", cin)
    rc, primary = _run(fx, tmp_path, name="primary.json")
    assert primary["simulation"]["status"] == "run", primary["eligibility"]["refusals"]
    primary_sides = [(p["side"], p["opened_qty"]) for p in primary["simulated"]["positions"]]
    assert named_sides != primary_sides

    cin["capability_evidence"].pop("regime_labels")
    seg["strategy"]["regime_gate_window"] = "short"
    _dump(fx / "comparison_input.json", cin)
    rc, missing = _run(fx, tmp_path, name="no_labels.json")
    assert ("regime_label_availability_unverified", "allowed_regimes") in _codes(missing)
    rc, approx = _run(fx, tmp_path, mode="approximate", name="no_labels_approx.json")
    assert approx["outcome"] == "incomplete"
    assert any(a.get("reason_code") == "regime_label_availability_unverified"
               for a in approx["eligibility"]["approximations"])


def test_regime_evidence_failures_keep_their_reason_codes(tmp_path):
    fx = _copy(tmp_path)
    cin, seg = _segment(fx)
    _use_regime_market(cin, fx)
    seg["regime"] = {"enabled": True, "period": 14, "adx_threshold": 20, "windows": REGIME_WINDOWS}
    seg["strategy"]["closed_bar_decisions"] = True
    seg["strategy"]["allowed_regimes"] = ["trending_up"]
    seg["strategy"]["regime_directional_policy"] = DIRECTIONAL_POLICY
    rel, digest = _write_json(fx, "regime_labels.json", {"values_and_timing": False})
    evidence = cin.setdefault("capability_evidence", {})
    evidence["regime_labels"] = _verified_evidence(_binding(seg), artifact={"path": rel, "sha256": digest})
    evidence["regime_feature_timing"] = _verified_evidence(
        _binding(seg), features={"directional": "unshifted_closed_candle"})
    good = _cert_artifact({"trending_up": "long", "trending_down": "short", "ranging": "long"})
    cert_rel, cert_sha = _write_json(fx, "cert.json", good)
    evidence["directional_certification"] = _verified_evidence(
        _binding(seg), reloads="none", artifact={"path": cert_rel, "sha256": cert_sha})
    _dump(fx / "comparison_input.json", cin)
    _, rep = _run(fx, tmp_path, name="incomplete_labels.json")
    assert ("regime_labels_incomplete", "allowed_regimes") in _codes(rep)

    _write_json(fx, "regime_labels.json", {"values_and_timing": True})
    evidence["regime_labels"]["artifact"]["sha256"] = "0" * 64
    evidence["regime_labels"]["binding"] = _binding(seg)
    evidence["directional_certification"]["binding"] = _binding(seg)
    evidence["regime_feature_timing"]["binding"] = _binding(seg)
    _dump(fx / "comparison_input.json", cin)
    _, rep = _run(fx, tmp_path, name="hash.json")
    assert ("regime_labels_hash_mismatch", "allowed_regimes") in _codes(rep)

    evidence["regime_labels"]["artifact"] = {"path": "missing-labels.json", "sha256": "a" * 64}
    _dump(fx / "comparison_input.json", cin)
    _, rep = _run(fx, tmp_path, name="missing.json")
    assert ("regime_labels_missing", "allowed_regimes") in _codes(rep)

    evidence["regime_labels"]["binding"] = dict(_binding(seg), partition="paper")
    evidence["regime_labels"]["artifact"] = {"path": "regime_labels.json", "sha256": _sha(fx / "regime_labels.json")}
    _dump(fx / "comparison_input.json", cin)
    _, rep = _run(fx, tmp_path, name="unbound.json")
    assert ("regime_labels_incomplete", "allowed_regimes") in _codes(rep)

    bad = _cert_artifact(
        {"trending_up": "long", "trending_down": "short", "ranging": "long"},
        extra_entry={"states": {"trending_up": "sideways"}})
    from directional_certification import CertificationInvalid, load_certifications, parse_certifications_strict
    cert_rel, cert_sha = _write_json(fx, "cert.json", bad)
    assert "BTC|1h|adx" in load_certifications(str(fx / "cert.json"))
    with pytest.raises(CertificationInvalid):
        parse_certifications_strict(_load(fx / "cert.json"))
    evidence["regime_labels"]["binding"] = _binding(seg)
    evidence["directional_certification"] = _verified_evidence(
        _binding(seg), reloads="none", artifact={"path": cert_rel, "sha256": cert_sha})
    _dump(fx / "comparison_input.json", cin)
    _, rep = _run(fx, tmp_path, name="malformed_cert.json")
    assert ("directional_certification_invalid", "regime_directional_policy") in _codes(rep)

    expiring = _cert_artifact({"trending_up": "long", "trending_down": "short", "ranging": "long"},
                              expires_at="2026-01-05T12:00:00Z")
    cert_rel, cert_sha = _write_json(fx, "cert.json", expiring)
    evidence["directional_certification"]["artifact"] = {"path": cert_rel, "sha256": cert_sha}
    evidence["directional_certification"]["binding"] = _binding(seg)
    _dump(fx / "comparison_input.json", cin)
    _, rep = _run(fx, tmp_path, name="expires.json")
    assert ("directional_certification_changes_in_window", "regime_directional_policy") in _codes(rep)


def test_a_certified_policy_is_modeled_from_unshifted_labels(tmp_path):
    fx = _copy(tmp_path)
    cin, seg = _segment(fx)
    _use_regime_market(cin, fx)
    seg["regime"] = {"enabled": True, "period": 14, "adx_threshold": 20, "windows": REGIME_WINDOWS}
    seg["strategy"]["regime_directional_window"] = "short"
    seg["strategy"]["regime_directional_policy"] = DIRECTIONAL_POLICY
    rel, digest = _write_json(fx, "regime_labels.json", {"values_and_timing": True})
    cert = _cert_artifact({"trending_up": "long", "trending_down": "short", "ranging": "long"},
                          expires_at="2026-02-01T00:00:00Z")
    cert_rel, cert_sha = _write_json(fx, "cert.json", cert)
    evidence = cin.setdefault("capability_evidence", {})
    evidence["regime_labels"] = _verified_evidence(_binding(seg), artifact={"path": rel, "sha256": digest})
    evidence["regime_feature_timing"] = _verified_evidence(
        _binding(seg), features={"directional": "unshifted_closed_candle"})
    evidence["directional_certification"] = _verified_evidence(
        _binding(seg), reloads="none", artifact={"path": cert_rel, "sha256": cert_sha})
    _dump(fx / "comparison_input.json", cin)
    _, rep = _run(fx, tmp_path, name="certified.json")
    assert rep["simulation"]["status"] == "run", rep["eligibility"]["refusals"]
    rows = {(r["field"], r["category"]): r for r in rep["eligibility"]["capability_matrix"]}
    assert rows[("regime_directional_policy", "regime")]["decision"] == "modeled"
    assert rep["eligibility"]["regime"]["certification"]["status"] == "verified"
    assert rep["eligibility"]["regime"]["timing"]["directional"] == "result.Regime"
    assert rep["eligibility"]["regime"]["protection_source"] is None

    seg["strategy"].pop("regime_feature_timing", None)
    evidence.pop("regime_feature_timing")
    for key in ("regime_labels", "directional_certification"):
        evidence[key]["binding"] = _binding(seg)
    _dump(fx / "comparison_input.json", cin)
    _, strict = _run(fx, tmp_path, name="timing.json")
    assert ("regime_feature_timing_unsupported", "regime_directional_policy") in _codes(strict)
    _, approx = _run(fx, tmp_path, mode="approximate", name="timing_approx.json")
    assert approx["outcome"] == "incomplete"
    assert any(a.get("reason_code") == "regime_feature_timing_unsupported"
               for a in approx["eligibility"]["approximations"])


def test_selector_timeframe_and_other_regime_refusals(tmp_path):
    fx = _copy(tmp_path)
    cin, seg = _segment(fx)
    seg["regime"] = {"enabled": True, "period": 14, "adx_threshold": 20}
    seg["strategy"]["regime_gate_window"] = "short"
    _dump(fx / "comparison_input.json", cin)
    _, rep = _run(fx, tmp_path, name="multi_off.json")
    assert ("regime_window_multi_disabled", "regime_gate_window") in _codes(rep)

    seg["strategy"]["regime_atr_window"] = "slow"
    _dump(fx / "comparison_input.json", cin)
    _, rep = _run(fx, tmp_path, name="atr_named.json")
    assert ("regime_window_multi_disabled", "regime_atr_window") in _codes(rep)

    seg["regime"]["windows"] = {"medium": {"classifier": "adx", "period": 14, "adx_threshold": 20}}
    seg["strategy"].pop("regime_atr_window")
    seg["strategy"]["regime_gate_window"] = "missing"
    _dump(fx / "comparison_input.json", cin)
    _, rep = _run(fx, tmp_path, name="unknown.json")
    assert ("regime_window_unknown", "regime_gate_window") in _codes(rep)

    seg["regime"] = {"enabled": False, "period": 14, "adx_threshold": 20}
    seg["strategy"].pop("regime_gate_window")
    seg["strategy"]["allowed_regimes"] = ["trending_up"]
    seg["strategy"]["regime_gate_on_failure"] = "closed"
    _dump(fx / "comparison_input.json", cin)
    _, rep = _run(fx, tmp_path, name="fail_closed.json")
    assert ("regime_gate_disabled_fail_closed", "allowed_regimes") in _codes(rep)

    seg["regime"] = {"enabled": True, "period": 14, "adx_threshold": 20, "timeframe": "4h",
                     "windows": REGIME_WINDOWS}
    seg["strategy"]["allowed_regimes"] = ["trending_up"]
    seg["strategy"]["closed_bar_decisions"] = True
    _dump(fx / "comparison_input.json", cin)
    _, rep = _run(fx, tmp_path, name="timeframe.json")
    assert ("regime_timeframe_unprepared", "allowed_regimes") in _codes(rep)

    seg["regime"].pop("timeframe")
    seg["strategy"].pop("allowed_regimes")
    seg["strategy"]["regime_window_divergence"] = {"enabled": True}
    seg["strategy"]["regime_profile_allocation"] = {"trending_up": "balanced"}
    seg["strategy"]["margin_per_trade_usd"] = 25
    seg["strategy"]["risk_per_trade_pct"] = 1
    seg["strategy"]["capital_pct"] = 10
    _dump(fx / "comparison_input.json", cin)
    _, rep = _run(fx, tmp_path, name="other.json")
    assert ("regime_divergence_unmodeled", "regime_window_divergence") in _codes(rep)
    assert ("regime_profile_allocation_unsupported", "regime_profile_allocation") in _codes(rep)
    assert ("sizing_margin_per_trade_usd", "margin_per_trade_usd") in _codes(rep)
    assert ("sizing_risk_per_trade_pct", "risk_per_trade_pct") in _codes(rep)
    assert ("sizing_capital_pct", "capital_pct") in _codes(rep)


def test_engine_keeps_feature_stamps_and_refuses_a_missing_column():
    import pandas as pd
    from backtester import Backtester

    idx = pd.date_range("2026-01-01", periods=6, freq="1h")
    px = [100, 101, 102, 103, 104, 105]
    # Gate is trending_down. The default directional window is the primary
    # column, trending_up. A flat entry follows trending_up (long). The open
    # stamp stays on the gate label.
    df = pd.DataFrame({
        "open": px, "high": [p + 1 for p in px], "low": [p - 1 for p in px], "close": px,
        "volume": [1000] * 6,
        "open_action": ["long", "none", "short", "none", "none", "none"],
        "close_fraction": [0.0, 0.4, 1.0, 0.0, 0.0, 0.0],
        "regime": ["trending_up"] * 6,
        "regime_w_gate": ["", "trending_down", "trending_down", "trending_down",
                          "trending_down", "trending_down"],
        "regime_w_medium": ["", "trending_up", "trending_up", "trending_up",
                            "trending_up", "trending_up"],
    }, index=idx)
    policy = {"trend_regime": {
        "trending_up": {"direction": "long", "invert_signal": False},
        "trending_down": {"direction": "short", "invert_signal": False},
    }}
    states = {"trending_up": "long", "trending_down": "short"}
    columns = {
        "gate": "regime_w_gate",
        "directional": "regime_w_medium",
        "directional_named": False,
    }
    bt = Backtester(
        initial_capital=1000, platform="hyperliquid", regime_enabled=True,
        regime_directional_policy=policy, regime_directional_certified_states=states,
        regime_label_columns=columns,
    )
    result = bt.run(df, save=False)
    assert [t["side"] for t in result["trades"]] == ["long", "long"]
    stamps = [e for e in bt._regime_stamp_trace if e["event"] == "stamp"]
    clears = [e for e in bt._regime_stamp_trace if e["event"] == "clear"]
    assert [e["directional"] for e in stamps] == ["trending_down"]
    assert clears and clears[0]["directional"] == ""

    partial = df.copy()
    partial["open_action"] = ["long", "none", "none", "none", "none", "none"]
    partial["close_fraction"] = [0.0, 0.4, 0.0, 0.0, 0.0, 0.0]
    held = Backtester(
        initial_capital=1000, platform="hyperliquid", regime_enabled=True,
        regime_directional_policy=policy, regime_directional_certified_states=states,
        regime_label_columns=columns,
    )
    held.run(partial, save=False)
    assert held._stamp_directional == "trending_down"
    assert not any(e["event"] == "clear" for e in held._regime_stamp_trace)
    open_direction, _ = held._effective_directional_entry(
        "trending_up", held._stamp_directional, 1.0)
    assert open_direction == "short"

    named = df.copy()
    named["open_action"] = ["short", "none", "none", "none", "none", "none"]
    named["close_fraction"] = [0.0] * 6
    named_bt = Backtester(
        initial_capital=1000, platform="hyperliquid", regime_enabled=True,
        regime_directional_policy=policy, regime_directional_certified_states=states,
        regime_label_columns={
            "gate": "regime_w_medium",
            "directional": "regime_w_gate",
            "directional_named": True,
        },
    )
    named_result = named_bt.run(named, save=False)
    assert [t["side"] for t in named_result["trades"]] == ["short"]
    assert named_bt._stamp_directional == "trending_down"

    missing = df.drop(columns=["regime_w_gate"])
    refused = Backtester(
        initial_capital=1000, platform="hyperliquid", regime_enabled=True,
        regime_label_columns=columns,
    )
    with pytest.raises(ValueError, match="refusing missing regime label column regime_w_gate"):
        refused.run(missing, save=False)


_REGIME_STOP = {"trend_regime": {
    "trending_up": {"atr_multiple": 2.0},
    "trending_down": {"atr_multiple": 1.0},
    "ranging": {"atr_multiple": 1.5},
}}


def _arms(df, *, columns=None, seed=None, regime_enabled=True):
    from backtester import Backtester
    events = []
    kwargs = dict(
        initial_capital=10_000, commission_pct=0.0, slippage_pct=0.0,
        platform="hyperliquid", strategy_type="perps", regime_enabled=regime_enabled,
        stop_loss_atr_mult_regime=_REGIME_STOP,
    )
    if columns is not None:
        kwargs["regime_label_columns"] = columns
    bt = Backtester(**kwargs)
    bt.run(df, save=False, starting_long=seed, stop_observer=events.append)
    return bt, [e for e in events if e.get("event") == "arm"]


def _ohlc(labels, actions):
    import pandas as pd
    n = len(labels)
    idx = pd.date_range("2026-01-01", periods=n, freq="1h", tz="UTC")
    px = [100.0] * n
    return pd.DataFrame({
        "open": px, "high": [101.0] * n, "low": [99.0] * n, "close": px,
        "atr": [2.0] * n, "volume": [1.0] * n,
        "open_action": actions, "regime": labels, "regime_w_short": labels,
    }, index=idx)


def test_regime_owned_stop_arms_from_the_decision_bar(tmp_path):
    """The fill bar's close differs from the previous bar. Both paths arm from the previous bar."""
    labels = ["trending_up", "trending_down", "trending_down", "trending_down"]
    actions = ["long", "none", "none", "none"]
    bare = _ohlc(labels, actions)
    _, bare_arms = _arms(bare)
    assert bare_arms and bare_arms[0]["regime"] == "trending_up"
    assert bare_arms[0]["fraction"] == pytest.approx(0.04)

    named = _ohlc(labels, actions)
    _, named_arms = _arms(named, columns={"gate": "regime_w_short", "directional_named": False})
    assert named_arms and named_arms[0]["regime"] == "trending_up"
    assert named_arms[0]["fraction"] == pytest.approx(0.04)
    assert named_arms[0]["regime"] != "trending_down"

    fx = _copy(tmp_path)
    cin, seg = _segment(fx)
    _use_regime_market(cin, fx)
    seg["regime"] = {"enabled": True, "period": 14, "adx_threshold": 20, "windows": dict(REGIME_WINDOWS)}
    _stop_free_loader_segment(seg)
    seg["strategy"]["close_strategy"] = copy.deepcopy(UNIFIED_CLOSE)
    seg["strategy"]["regime_gate_window"] = "short"
    _dump(fx / "comparison_input.json", cin)
    _, rep = _run(fx, tmp_path, name="named_gate_stop.json")
    assert rep["simulation"]["status"] == "run", rep["eligibility"]["refusals"]
    assert rep["eligibility"]["regime"]["protection_source"] == "shifted_decision_bar:gate_window:short"
    assert rep["eligibility"]["regime"]["timing"]["protection"] == "shifted_decision_bar"
    import pandas as pd
    from regime import bounded_window_labels, required_ohlcv_limit
    with gzip.open(fx / "regime_market" / "BTC_1h_candles.csv.gz", "rt") as fh:
        candles = pd.read_csv(fh)
    limit = required_ohlcv_limit(14, REGIME_WINDOWS)
    bounded_window_labels(
        candles, period=14, adx_threshold=20.0, windows_spec=REGIME_WINDOWS, limit=limit,
        columns={"regime_w_short": "short"})
    by_ts = {int(ts): lab for ts, lab in zip(candles["timestamp"], candles["regime_w_short"])}

    def _fill_ms(date) -> int:
        ts = pd.Timestamp(date)
        ts = ts.tz_localize("UTC") if ts.tzinfo is None else ts.tz_convert("UTC")
        return int(ts.timestamp() * 1000)

    for arm in rep["stops"]["arm_events"]:
        fill = _fill_ms(arm["date"])
        decision = by_ts[fill - HOUR_MS]
        assert arm["regime"] == decision
    rows = [(int(ts), str(lab or "")) for ts, lab in zip(candles["timestamp"], candles["regime_w_short"])]
    pair = next((prev, cur) for prev, cur in zip(rows, rows[1:])
                if prev[1] and cur[1] and prev[1] != cur[1])
    crossed = _ohlc([pair[0][1], pair[1][1], pair[1][1]], ["long", "none", "none"])
    _, crossed_arms = _arms(crossed, columns={"gate": "regime_w_short", "directional_named": False})
    assert crossed_arms and crossed_arms[0]["regime"] == pair[0][1]
    assert crossed_arms[0]["regime"] != pair[1][1]

    seg["strategy"].pop("regime_gate_window")
    _dump(fx / "comparison_input.json", cin)
    _, primary = _run(fx, tmp_path, name="primary_stop.json")
    assert primary["simulation"]["status"] == "run", primary["eligibility"]["refusals"]
    assert primary["eligibility"]["regime"]["protection_source"] == "shifted_decision_bar:primary_column"
    from regime import ensure_regime_columns
    primary_frame = candles.drop(columns=["regime_w_short"]).copy()
    ensure_regime_columns(primary_frame, period=14, adx_threshold=20.0, windows_spec=REGIME_WINDOWS)
    primary_by_ts = {int(ts): lab for ts, lab in zip(primary_frame["timestamp"], primary_frame["regime"])}
    gated = cin
    seg["strategy"]["allowed_regimes"] = ["trending_up", "trending_down", "ranging"]
    seg["strategy"]["closed_bar_decisions"] = True
    rel, digest = _write_json(fx, "regime_labels.json", {"values_and_timing": True})
    gated.setdefault("capability_evidence", {})["regime_labels"] = _verified_evidence(
        _binding(seg), artifact={"path": rel, "sha256": digest})
    _dump(fx / "comparison_input.json", gated)
    _, every = _run(fx, tmp_path, name="every_label.json")
    assert every["simulation"]["status"] == "run", every["eligibility"]["refusals"]
    bounded_window_labels(
        candles, period=14, adx_threshold=20.0, windows_spec=REGIME_WINDOWS, limit=limit,
        columns={"regime_w_medium": "medium"})
    medium_by_ts = {int(ts): lab for ts, lab in zip(candles["timestamp"], candles["regime_w_medium"])}
    for arm in primary["stops"]["arm_events"]:
        fill = _fill_ms(arm["date"])
        assert arm["regime"] == str(primary_by_ts[fill - HOUR_MS])
    for arm in every["stops"]["arm_events"]:
        fill = _fill_ms(arm["date"])
        assert arm["regime"] == str(medium_by_ts[fill - HOUR_MS])
    assert [a["date"] for a in every["stops"]["arm_events"]] == [
        a["date"] for a in primary["stops"]["arm_events"]]


_STAMP_POLICY = {"trend_regime": {
    "trending_up": {"direction": "long", "invert_signal": False},
    "trending_down": {"direction": "short", "invert_signal": False},
}}
_STAMP_STATES = {"trending_up": "long", "trending_down": "short"}


def _armed_with_policy(df, columns, *, policy=None):
    from backtester import Backtester
    events = []
    kwargs = dict(
        initial_capital=10_000, commission_pct=0.0, slippage_pct=0.0,
        platform="hyperliquid", strategy_type="perps", regime_enabled=True,
        stop_loss_atr_mult_regime=_REGIME_STOP, regime_label_columns=columns,
    )
    if policy is not None:
        kwargs["regime_directional_policy"] = policy
        kwargs["regime_directional_certified_states"] = dict(_STAMP_STATES)
    bt = Backtester(**kwargs)
    result = bt.run(df, save=False, stop_observer=events.append)
    arms = [e for e in events if e.get("event") == "arm"]
    return bt, arms, result["trades"]


def test_modeled_policy_arms_the_stop_from_the_stamp_row(tmp_path):
    """One position uses one closed-candle row for the side, the stamp, and the arm."""
    labels = ["trending_down", "trending_up", "trending_up", "trending_up"]
    actions = ["long", "none", "none", "none"]
    primary = _ohlc(labels, actions)
    columns = {"gate": "regime", "directional": "regime", "directional_named": False}
    bt, arms, trades = _armed_with_policy(primary, columns, policy=_STAMP_POLICY)
    assert trades and trades[0]["side"] == "long"
    assert arms and arms[0]["regime"] == "trending_up"
    assert arms[0]["fraction"] == pytest.approx(0.04)
    assert bt._stamp_directional == "trending_up"
    assert arms[0]["regime"] == bt._stamp_directional

    short = ["trending_up", "trending_down", "trending_down", "trending_down"]
    medium = ["trending_down", "trending_up", "trending_up", "trending_up"]
    named = _ohlc(medium, actions)
    named["regime_w_short"] = short
    named["regime_w_medium"] = medium
    bt, arms, trades = _armed_with_policy(named, {
        "gate": "regime_w_short",
        "directional": "regime_w_medium",
        "directional_named": False,
    }, policy=_STAMP_POLICY)
    assert trades and trades[0]["side"] == "long"
    assert arms and arms[0]["regime"] == "trending_down"
    assert arms[0]["fraction"] == pytest.approx(0.02)
    assert bt._stamp_directional == "trending_down"
    assert arms[0]["regime"] == bt._stamp_directional
    assert arms[0]["regime"] != "trending_up"

    fx = _copy(tmp_path)
    cin, seg = _segment(fx)
    _use_regime_market(cin, fx)
    seg["regime"] = {"enabled": True, "period": 14, "adx_threshold": 20,
                     "windows": dict(REGIME_WINDOWS)}
    _stop_free_loader_segment(seg)
    seg["strategy"]["close_strategy"] = copy.deepcopy(UNIFIED_CLOSE)
    seg["strategy"]["regime_directional_policy"] = copy.deepcopy(DIRECTIONAL_POLICY)
    rel, digest = _write_json(fx, "regime_labels.json", {"values_and_timing": True})
    cert = _cert_artifact(
        {"trending_up": "long", "trending_down": "short", "ranging": "long"},
        expires_at="2026-02-01T00:00:00Z")
    cert_rel, cert_sha = _write_json(fx, "cert.json", cert)

    def _bind(name):
        binding = _binding(seg)
        evidence = cin.setdefault("capability_evidence", {})
        evidence["regime_labels"] = _verified_evidence(
            binding, artifact={"path": rel, "sha256": digest})
        evidence["regime_feature_timing"] = _verified_evidence(
            binding, features={"directional": "unshifted_closed_candle"})
        evidence["directional_certification"] = _verified_evidence(
            binding, reloads="none", artifact={"path": cert_rel, "sha256": cert_sha})
        _dump(fx / "comparison_input.json", cin)
        _, rep = _run(fx, tmp_path, name=name)
        assert rep["simulation"]["status"] == "run", rep["eligibility"]["refusals"]
        return rep

    primary_rep = _bind("policy_primary_stop.json")
    assert primary_rep["eligibility"]["regime"]["protection_source"] == (
        "unshifted_closed_candle:primary_column")
    assert primary_rep["eligibility"]["regime"]["timing"]["protection"] == "unshifted_closed_candle"

    seg["strategy"]["regime_gate_window"] = "short"
    named_rep = _bind("policy_short_stop.json")
    assert named_rep["eligibility"]["regime"]["protection_source"] == (
        "unshifted_closed_candle:gate_window:short")
    assert named_rep["eligibility"]["regime"]["timing"]["protection"] == "unshifted_closed_candle"


def test_certification_rejects_short_timestamps_and_nonfinite_tokens(tmp_path):
    fx = _copy(tmp_path)
    cin, seg = _segment(fx)
    _use_regime_market(cin, fx)
    seg["regime"] = {"enabled": True, "period": 14, "adx_threshold": 20, "windows": dict(REGIME_WINDOWS)}
    seg["strategy"]["regime_directional_window"] = "short"
    seg["strategy"]["regime_directional_policy"] = copy.deepcopy(DIRECTIONAL_POLICY)
    rel, digest = _write_json(fx, "regime_labels.json", {"values_and_timing": True})
    evidence = cin.setdefault("capability_evidence", {})
    evidence["regime_labels"] = _verified_evidence(_binding(seg), artifact={"path": rel, "sha256": digest})
    evidence["regime_feature_timing"] = _verified_evidence(
        _binding(seg), features={"directional": "unshifted_closed_candle"})

    def _bind(payload, name):
        cert_rel, cert_sha = _write_json(fx, "cert.json", payload)
        evidence["directional_certification"] = _verified_evidence(
            _binding(seg), reloads="none", artifact={"path": cert_rel, "sha256": cert_sha})
        evidence["regime_labels"]["binding"] = _binding(seg)
        evidence["regime_feature_timing"]["binding"] = _binding(seg)
        _dump(fx / "comparison_input.json", cin)
        _, rep = _run(fx, tmp_path, name=name)
        return rep

    states = {"trending_up": "long", "trending_down": "short", "ranging": "long"}
    short = _bind(_cert_artifact(states, expires_at="2026-12-01T00:00Z"), "no_seconds.json")
    assert ("directional_certification_invalid", "regime_directional_policy") in _codes(short)

    nan_payload = _cert_artifact(states, expires_at="2026-12-01T00:00:00Z")
    nan_payload["criteria"] = {"x": float("nan")}
    nan = _bind(nan_payload, "nan.json")
    assert ("directional_certification_invalid", "regime_directional_policy") in _codes(nan)

    from directional_certification import parse_certifications_strict
    accepted = _cert_artifact(states, expires_at="2026-12-01T00:00:00.5+05:30")
    parsed = parse_certifications_strict(accepted)
    assert parsed["BTC|1h|adx"]["states"]["trending_up"] == "long"
    held = _bind(accepted, "fractional.json")
    assert ("directional_certification_invalid", "regime_directional_policy") not in _codes(held)
    assert held["eligibility"]["regime"]["certification"]["status"] == "verified"


def test_seeded_open_keeps_its_recorded_entry_label():
    import pandas as pd
    idx = pd.date_range("2026-01-01", periods=3, freq="1h", tz="UTC")
    px = [100.0, 100.0, 100.0]
    df = pd.DataFrame({
        "open": px, "high": [101.0] * 3, "low": [99.0] * 3, "close": px,
        "atr": [2.0] * 3, "volume": [1.0] * 3,
        "open_action": ["none", "none", "none"],
        "regime": ["trending_down"] * 3,
        "regime_w_short": ["trending_down", "trending_up", "trending_up"],
    }, index=idx)
    columns = {"gate": "regime_w_short", "directional_named": False}
    seed = {"entry_price": 100.0, "entry_atr": 2.0, "entry_date": idx[0],
            "entry_regime": "trending_up"}
    bt, arms = _arms(df, columns=columns, seed=seed)
    assert arms and arms[0]["regime"] == "trending_up"
    assert arms[0]["fraction"] == pytest.approx(0.04)
    assert bt._stamp_gate == "trending_up"
    assert bt._stamp_directional == "trending_up"
    assert bt._stamp_atr == "trending_up"

    missing = dict(seed)
    missing.pop("entry_regime")
    bt, arms = _arms(df, columns=columns, seed=missing)
    assert arms and arms[0]["regime"] == "trending_down"
    assert arms[0]["fraction"] == pytest.approx(0.02)
    assert bt._stamp_directional == "trending_down"

    plain = df.drop(columns=["regime_w_short"])
    bt, arms = _arms(plain, seed=seed)
    assert arms and arms[0]["regime"] == "trending_up"
    assert arms[0]["fraction"] == pytest.approx(0.04)
    _, fallback = _arms(plain, seed=missing, regime_enabled=False)
    assert fallback and fallback[0]["regime"] == "trending_down"


def _slice_regime_warmup(fx, cin, warmup):
    base = fx / "regime_market"
    cutoff = START_MS - warmup * HOUR_MS

    def _keep(path):
        with gzip.open(path, "rt") as fh:
            lines = fh.read().splitlines()
        kept = [lines[0]] + [ln for ln in lines[1:] if ln and int(ln.split(",")[0]) >= cutoff]
        buf = io.BytesIO()
        with gzip.GzipFile(fileobj=buf, mode="wb", mtime=0, filename="") as gz:
            gz.write(("\n".join(kept) + "\n").encode())
        path.write_bytes(buf.getvalue())

    _keep(base / "BTC_1h_candles.csv.gz")
    _keep(base / "BTC_funding.csv.gz")
    manifest = _load(base / "manifest.json")
    ds = manifest["datasets"][0]
    ds["candles"]["sha256"] = _sha(base / ds["candles"]["path"])
    ds["funding"]["sha256"] = _sha(base / ds["funding"]["path"])
    manifest["warmup_bars"] = warmup
    _dump(base / "manifest.json", manifest)
    cin["market"]["manifest"]["sha256"] = _sha(base / "manifest.json")


def test_warmup_between_lookback_and_trim_is_not_an_indicator_refusal(tmp_path):
    def _run_warmup(warmup, name):
        slot = tmp_path / f"warm{warmup}"
        slot.mkdir()
        fx = _copy(slot)
        cin, seg = _segment(fx)
        _use_regime_market(cin, fx)
        _slice_regime_warmup(fx, cin, warmup)
        seg["regime"] = {"enabled": True, "period": 14, "adx_threshold": 20, "windows": dict(REGIME_WINDOWS)}
        _stop_free_loader_segment(seg)
        seg["strategy"]["close_strategy"] = copy.deepcopy(UNIFIED_CLOSE)
        seg["strategy"]["allowed_regimes"] = ["trending_up", "trending_down", "ranging"]
        seg["strategy"]["closed_bar_decisions"] = True
        rel, digest = _write_json(fx, "regime_labels.json", {"values_and_timing": True})
        cin.setdefault("capability_evidence", {})["regime_labels"] = _verified_evidence(
            _binding(seg), artifact={"path": rel, "sha256": digest})
        _dump(fx / "comparison_input.json", cin)
        return _run(fx, tmp_path, name=name)

    _, mid = _run_warmup(250, "warmup_250.json")
    reasons = {r["reason"] for r in mid["eligibility"]["refusals"]}
    assert "market_indicator_history_insufficient" not in reasons
    assert "regime_lookback_insufficient" not in reasons
    assert mid["eligibility"]["market"]["strategy_checks"]["indicator_history"]["ok"] is True
    assert mid["simulation"]["status"] == "run"

    _, full = _run_warmup(300, "warmup_300.json")
    assert full["eligibility"]["market"]["strategy_checks"]["indicator_history"]["ok"] is True
    assert full["simulation"]["status"] == "run", full["eligibility"]["refusals"]

    _, short = _run_warmup(199, "warmup_199.json")
    assert ("regime_lookback_insufficient", "allowed_regimes") in _codes(short)
    assert "market_indicator_history_insufficient" not in {r["reason"] for r in short["eligibility"]["refusals"]}


def test_unshifted_gate_attestation_reads_the_fill_row(tmp_path):
    import pandas as pd
    from backtester import Backtester

    labels = ["trending_down", "trending_up", "trending_up", "trending_up"]
    actions = ["long", "none", "none", "none"]

    def _gate_run(unshifted):
        frame = _ohlc(labels, actions)
        columns = {"gate": "regime", "directional_named": False}
        if unshifted:
            columns["gate_unshifted"] = True
        bt = Backtester(
            initial_capital=10_000, commission_pct=0.0, slippage_pct=0.0,
            platform="hyperliquid", strategy_type="perps", regime_enabled=True,
            allowed_regimes=["trending_up"], regime_label_columns=columns,
        )
        result = bt.run(frame, save=False)
        return frame, result["trades"]

    admitted, admitted_trades = _gate_run(True)
    assert admitted_trades and admitted_trades[0]["side"] == "long"
    assert pd.Timestamp(admitted_trades[0]["entry_date"]) == admitted.index[1]

    _, blocked_trades = _gate_run(False)
    assert blocked_trades == []

    prior_up = _ohlc(
        ["trending_up", "trending_down", "trending_down", "trending_down"], actions)
    prior_bt = Backtester(
        initial_capital=10_000, commission_pct=0.0, slippage_pct=0.0,
        platform="hyperliquid", strategy_type="perps", regime_enabled=True,
        allowed_regimes=["trending_up"],
        regime_label_columns={"gate": "regime", "directional_named": False},
    )
    prior = prior_bt.run(prior_up, save=False)
    assert prior["trades"] and prior["trades"][0]["side"] == "long"
    assert pd.Timestamp(prior["trades"][0]["entry_date"]) == prior_up.index[1]

    def _compare(closed_bar, attest, name):
        slot = tmp_path / name
        slot.mkdir()
        fx = _copy(slot)
        cin, seg = _segment(fx)
        _use_regime_market(cin, fx)
        seg["regime"] = {"enabled": True, "period": 14, "adx_threshold": 20,
                         "windows": dict(REGIME_WINDOWS)}
        seg["strategy"]["allowed_regimes"] = ["trending_up"]
        if closed_bar:
            seg["strategy"]["closed_bar_decisions"] = True
        rel, digest = _write_json(fx, "regime_labels.json", {"values_and_timing": True})
        evidence = cin.setdefault("capability_evidence", {})
        evidence["regime_labels"] = _verified_evidence(
            _binding(seg), artifact={"path": rel, "sha256": digest})
        if attest:
            evidence["regime_feature_timing"] = _verified_evidence(
                _binding(seg), features={"gate": "unshifted_closed_candle"})
        _dump(fx / "comparison_input.json", cin)
        return _run(fx, tmp_path, name=name + ".json")

    _, attested = _compare(False, True, "unshifted")
    assert attested["simulation"]["status"] == "run", attested["eligibility"]["refusals"]
    assert attested["eligibility"]["regime"]["timing"]["gate"] == "unshifted_closed_candle"
    rows = {(r["field"], r["category"]): r for r in attested["eligibility"]["capability_matrix"]}
    assert rows[("allowed_regimes", "regime")]["decision"] == "modeled"
    assert "unshifted closed-candle" in rows[("allowed_regimes", "regime")]["reason"]

    _, closed = _compare(True, True, "shifted")
    assert closed["simulation"]["status"] == "run", closed["eligibility"]["refusals"]
    assert closed["eligibility"]["regime"]["timing"]["gate"] == "shifted_closed_bar"
    closed_rows = {(r["field"], r["category"]): r for r in closed["eligibility"]["capability_matrix"]}
    assert "shifted closed-bar" in closed_rows[("allowed_regimes", "regime")]["reason"]

    _, bare = _compare(False, False, "bare")
    assert ("regime_feature_timing_unsupported", "allowed_regimes") in _codes(bare)
    assert bare["eligibility"]["regime"]["timing"]["gate"] == "unsupported_without_evidence"


def test_unshifted_gate_arms_protection_from_the_same_row(tmp_path):
    """An attested unshifted gate and the regime-owned stop share one closed-candle row."""
    import pandas as pd
    from backtester import Backtester

    def _arm(labels, columns, *, short=None):
        frame = _ohlc(labels, ["long", "none", "none", "none"])
        if short is not None:
            frame["regime_w_short"] = short
        events = []
        bt = Backtester(
            initial_capital=10_000, commission_pct=0.0, slippage_pct=0.0,
            platform="hyperliquid", strategy_type="perps", regime_enabled=True,
            allowed_regimes=["trending_up"], stop_loss_atr_mult_regime=_REGIME_STOP,
            regime_label_columns=columns,
        )
        result = bt.run(frame, save=False, stop_observer=events.append)
        arms = [e for e in events if e.get("event") == "arm"]
        return frame, bt, arms, result["trades"]

    down_then_up = ["trending_down", "trending_up", "trending_up", "trending_up"]
    unshifted = {"gate": "regime", "directional_named": False, "gate_unshifted": True}
    frame, bt, arms, trades = _arm(down_then_up, unshifted)
    assert trades and trades[0]["side"] == "long"
    assert pd.Timestamp(trades[0]["entry_date"]) == frame.index[1]
    assert arms and arms[0]["regime"] == "trending_up"
    assert arms[0]["fraction"] == pytest.approx(0.04)
    assert bt._run_position_regime == "trending_up"
    assert bt._stamp_gate == bt._stamp_atr == bt._stamp_directional == "trending_up"

    _, shifted_bt, shifted_arms, shifted_trades = _arm(
        down_then_up, {"gate": "regime", "directional_named": False})
    assert shifted_trades == [] and shifted_arms == []
    assert shifted_bt._run_position_regime == ""

    up_then_down = ["trending_up", "trending_down", "trending_down", "trending_down"]
    _, decision_bt, decision_arms, decision_trades = _arm(
        up_then_down, {"gate": "regime", "directional_named": False})
    assert decision_trades and decision_arms
    assert decision_arms[0]["regime"] == "trending_up"
    assert decision_arms[0]["fraction"] == pytest.approx(0.04)
    assert decision_arms[0]["regime"] != "trending_down"
    assert decision_bt._run_position_regime == "trending_up"

    _, named_bt, named_arms, named_trades = _arm(
        ["ranging", "trending_down", "trending_down", "trending_down"],
        {"gate": "regime_w_short", "directional_named": False, "gate_unshifted": True},
        short=down_then_up,
    )
    assert named_trades and named_arms
    assert named_arms[0]["regime"] == "trending_up"
    assert named_arms[0]["fraction"] == pytest.approx(0.04)
    assert named_bt._stamp_gate == "trending_up"
    assert named_bt._run_position_regime == "trending_up"
    assert named_arms[0]["regime"] != "ranging"
    assert named_arms[0]["regime"] != "trending_down"

    def _stop_compare(closed_bar, name, *, gate_window=None, allowed=None):
        slot = tmp_path / name
        slot.mkdir()
        fx = _copy(slot)
        cin, seg = _segment(fx)
        _use_regime_market(cin, fx)
        seg["regime"] = {"enabled": True, "period": 14, "adx_threshold": 20,
                         "windows": dict(REGIME_WINDOWS)}
        _stop_free_loader_segment(seg)
        seg["strategy"]["close_strategy"] = copy.deepcopy(UNIFIED_CLOSE)
        if allowed is not None:
            seg["strategy"]["allowed_regimes"] = list(allowed)
        if gate_window:
            seg["strategy"]["regime_gate_window"] = gate_window
        if closed_bar:
            seg["strategy"]["closed_bar_decisions"] = True
        rel, digest = _write_json(fx, "regime_labels.json", {"values_and_timing": True})
        evidence = cin.setdefault("capability_evidence", {})
        evidence["regime_labels"] = _verified_evidence(
            _binding(seg), artifact={"path": rel, "sha256": digest})
        evidence["regime_feature_timing"] = _verified_evidence(
            _binding(seg), features={"gate": "unshifted_closed_candle"})
        _dump(fx / "comparison_input.json", cin)
        return fx, _run(fx, tmp_path, name=name + ".json")

    _, (_, attested) = _stop_compare(False, "fill_row_stop", allowed=["trending_up"])
    assert attested["simulation"]["status"] == "run", attested["eligibility"]["refusals"]
    assert attested["eligibility"]["regime"]["timing"]["protection"] == "unshifted_closed_candle"
    assert attested["eligibility"]["regime"]["protection_source"] == (
        "unshifted_closed_candle:primary_column")
    assert "unshifted closed-candle row" in attested["eligibility"]["regime"]["stamps"]

    _, (_, closed) = _stop_compare(True, "decision_bar_stop", gate_window="short")
    assert closed["simulation"]["status"] == "run", closed["eligibility"]["refusals"]
    assert closed["eligibility"]["regime"]["timing"]["protection"] == "shifted_decision_bar"
    assert closed["eligibility"]["regime"]["protection_source"] == (
        "shifted_decision_bar:gate_window:short")
    assert closed["eligibility"]["regime"]["timing"]["gate"] == "shifted_closed_bar"

    _, (_, named) = _stop_compare(
        False, "short_fill_row", gate_window="short", allowed=["trending_up"])
    assert named["simulation"]["status"] == "run", named["eligibility"]["refusals"]
    assert named["eligibility"]["regime"]["timing"]["protection"] == "unshifted_closed_candle"
    assert named["eligibility"]["regime"]["protection_source"] == (
        "unshifted_closed_candle:gate_window:short")

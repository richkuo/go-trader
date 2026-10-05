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


def test_version_one_input_is_read_and_refused_for_unverified_stops(tmp_path):
    fx = _copy(tmp_path)
    cin = _load(fx / "comparison_input.json")
    cin["schema_version"] = 1
    for key in ("basis", "stop_defaults", "stop_evidence"):
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

    seg["strategy"]["allowed_regimes"] = ["trending_up"]
    _dump(fx / "comparison_input.json", cin)
    rc, rep = _run(fx, tmp_path, name=f"{variant}_gated.json")
    assert rc == 1 and rep["outcome"] == "refused"
    assert ("capability_regime", "allowed_regimes") in {(r["reason"], r.get("field"))
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

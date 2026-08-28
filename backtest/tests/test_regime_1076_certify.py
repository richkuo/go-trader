import json
import os
import sys
from datetime import datetime, timezone

sys.path.insert(0, os.path.abspath(
    os.path.join(os.path.dirname(__file__), "..", "research")))

import regime_1076_certify as certify_mod

GEN_AT = datetime(2026, 6, 19, tzinfo=timezone.utc)


def _row(symbol="BTC/USDT", timeframe="1h", classifier="composite",
         state="trending_up", window="oos", p=0.0001, policy_dir=1,
         sign_aligned=True):
    return {
        "classifier": classifier, "symbol": symbol, "timeframe": timeframe,
        "window": window, "horizon": 4, "state": state, "gap": 0.01,
        "mean_fwd": 0.01, "p_value": p, "fdr_reject": True,
        "policy_dir": policy_dir, "sign_aligned": sign_aligned,
        "candidate_edge": True,
    }


def test_empty_rows_certify_nothing():
    art = certify_mod.certify([], generated_at=GEN_AT)
    assert art["schema_version"] == 1
    assert art["certified"] == []


def test_surviving_aligned_heldout_row_is_certified():
    art = certify_mod.certify([_row()], generated_at=GEN_AT)
    assert len(art["certified"]) == 1
    e = art["certified"][0]
    assert (e["asset"], e["timeframe"], e["classifier"]) == ("BTC", "1h", "composite")
    assert e["states"] == {"trending_up": "long"}
    assert e["expires_at"] > e["generated_at"]


def test_historical_window_not_certified():
    art = certify_mod.certify([_row(window="2024")], generated_at=GEN_AT)
    assert art["certified"] == []


def test_wrong_signed_not_certified():
    art = certify_mod.certify([_row(sign_aligned=False)], generated_at=GEN_AT)
    assert art["certified"] == []


def test_non_significant_not_certified():
    art = certify_mod.certify([_row(p=0.9)], generated_at=GEN_AT)
    assert art["certified"] == []


def test_composite_sublabel_maps_to_canonical():
    art = certify_mod.certify(
        [_row(state="trending_down_strong", policy_dir=-1)], generated_at=GEN_AT)
    assert len(art["certified"]) == 1
    assert art["certified"][0]["states"] == {"trending_down": "short"}


import pytest

import regime_1076_directional_premise as premise_mod

FULL_UNIVERSE = dict(
    symbols=premise_mod.DEFAULT_SYMBOLS,
    timeframes=premise_mod.DEFAULT_TIMEFRAMES,
    windows=premise_mod.DEFAULT_WINDOWS,
    classifiers=premise_mod.DEFAULT_CLASSIFIERS,
    horizons=premise_mod.DEFAULT_HORIZONS,
)


def test_screened_family_size_counts_directional_rows_only():
    rows = [_row(), _row(symbol="ETH/USDT"),
            _row(state="ranging", policy_dir=0, sign_aligned=False)]
    art = certify_mod.certify(rows, generated_at=GEN_AT)
    assert art["criteria"]["screened_family_size"] == 2


def test_screened_family_size_is_emitted_even_for_an_empty_screen():
    art = certify_mod.certify([], generated_at=GEN_AT)
    assert art["criteria"]["screened_family_size"] == 0


def test_provenance_keys_are_absent_when_not_supplied():
    c = certify_mod.certify([_row()], generated_at=GEN_AT)["criteria"]
    for key in ("universe", "data_sources", "n_perm", "permutation_p_floor"):
        assert key not in c


def test_provenance_keys_pass_through_and_nest_under_criteria():
    sources = {"HYPE/USDC:USDC": "hyperliquid", "BTC/USDT": "binanceus"}
    art = certify_mod.certify(
        [_row()], generated_at=GEN_AT, universe=FULL_UNIVERSE,
        data_sources=sources, n_perm=30000)
    c = art["criteria"]
    assert c["data_sources"] == sources
    assert c["n_perm"] == 30000
    assert c["permutation_p_floor"] == pytest.approx(1 / 30001)
    assert c["universe"]["symbols"] == list(premise_mod.DEFAULT_SYMBOLS)
    assert c["universe"]["horizons"] == list(premise_mod.DEFAULT_HORIZONS)
    assert set(art) == {"schema_version", "generated_at", "generator",
                        "source_evidence", "criteria", "default_ttl_days",
                        "certified"}
    assert art["schema_version"] == 1


def test_permutation_p_floor_arithmetic():
    assert certify_mod.permutation_p_floor(500) == pytest.approx(1 / 501)
    assert certify_mod.permutation_p_floor(30000) == pytest.approx(1 / 30001)


def test_family_is_superset_true_for_the_default_universe():
    assert certify_mod.family_is_superset(**FULL_UNIVERSE)


def test_family_is_superset_true_when_a_symbol_is_added():
    assert certify_mod.family_is_superset(
        symbols=premise_mod.parse_symbols_arg(
            "BTC/USDT,ETH/USDT,SOL/USDT,HYPE/USDC:USDC@hyperliquid"),
        timeframes=premise_mod.DEFAULT_TIMEFRAMES,
        windows=premise_mod.DEFAULT_WINDOWS,
        classifiers=premise_mod.DEFAULT_CLASSIFIERS,
        horizons=premise_mod.DEFAULT_HORIZONS)


def test_family_is_superset_ignores_the_exchange_suffix():
    assert certify_mod.family_is_superset(
        symbols=("BTC/USDT@kraken", "ETH/USDT", "SOL/USDT"),
        timeframes=premise_mod.DEFAULT_TIMEFRAMES,
        windows=premise_mod.DEFAULT_WINDOWS,
        classifiers=premise_mod.DEFAULT_CLASSIFIERS,
        horizons=premise_mod.DEFAULT_HORIZONS)


@pytest.mark.parametrize("axis", ["symbols", "timeframes", "windows",
                                  "classifiers", "horizons"])
def test_family_is_superset_false_when_any_axis_is_narrowed(axis):
    kwargs = dict(FULL_UNIVERSE)
    kwargs[axis] = tuple(kwargs[axis])[:-1]
    assert not certify_mod.family_is_superset(**kwargs)


def test_family_is_superset_skips_horizons_when_not_supplied():
    kwargs = dict(FULL_UNIVERSE)
    kwargs["horizons"] = None
    assert certify_mod.family_is_superset(**kwargs)


def test_narrowed_run_refuses_to_write_the_repo_artifact():
    with pytest.raises(SystemExit) as exc:
        certify_mod.main(["--symbols", "ETH/USDT", "--timeframes", "1h",
                          "--classifiers", "composite"])
    assert "narrowed family" in str(exc.value)


def test_narrowed_run_may_write_a_research_artifact_elsewhere(tmp_path, monkeypatch):
    monkeypatch.setattr(premise_mod, "run", lambda *a, **k: [_row(symbol="ETH/USDT")])
    out = tmp_path / "research_artifact.json"
    report = tmp_path / "research_report.json"
    rc = certify_mod.main(["--symbols", "ETH/USDT", "--timeframes", "1h",
                           "--classifiers", "composite", "--windows", "oos",
                           "--horizons", "4", "--n-perm", "1",
                           "--out", str(out), "--report-out", str(report)])
    assert rc == 0
    art = json.loads(out.read_text())
    assert art["schema_version"] == 1
    assert art["criteria"]["universe"]["symbols"] == ["ETH/USDT"]
    assert art["criteria"]["screened_family_size"] == 1
    assert json.loads(report.read_text())["cell_verdicts"][0]["verdict"] == (
        certify_mod.VERDICT_CERTIFIED)


def test_repo_artifact_run_over_the_full_family_is_allowed(tmp_path, monkeypatch):
    monkeypatch.setattr(premise_mod, "run", lambda *a, **k: [_row(p=0.9)])
    monkeypatch.setattr(certify_mod, "DEFAULT_ARTIFACT", str(tmp_path / "art.json"))
    rc = certify_mod.main([
        "--symbols", "BTC/USDT,ETH/USDT,SOL/USDT,HYPE/USDC:USDC@hyperliquid",
        "--out", str(tmp_path / "art.json"),
        "--report-out", str(tmp_path / "rep.json")])
    assert rc == 0
    assert json.loads((tmp_path / "art.json").read_text())["certified"] == []


def test_cell_verdict_certified_matches_the_artifact():
    rows = [_row()]
    v = certify_mod.cell_verdicts(rows)[("BTC", "1h", "composite")]
    assert v["verdict"] == certify_mod.VERDICT_CERTIFIED
    assert v["certified_states"] == {"trending_up": "long"}
    assert v["global_bh_family_size"] == 1
    assert v["bh_rank"] == 1


def test_cell_verdict_reports_the_bh_bar_it_missed():
    rows = [_row(symbol="ETH/USDT", p=0.30 + i * 0.001) for i in range(200)]
    v = certify_mod.cell_verdicts(rows)[("ETH", "1h", "composite")]
    assert v["verdict"] == certify_mod.VERDICT_FAILS_GLOBAL_BH
    assert v["min_p_value"] == pytest.approx(0.30)
    assert v["bh_rank"] == 1
    assert v["bh_threshold"] == pytest.approx(0.05 / 200)
    assert v["n_survive_global_bh"] == 0


def test_cell_verdict_wrong_signed_outranks_the_held_out_check():
    rows = [_row(sign_aligned=False, window="2024")]
    v = certify_mod.cell_verdicts(rows)[("BTC", "1h", "composite")]
    assert v["verdict"] == certify_mod.VERDICT_WRONG_SIGNED


def test_cell_verdict_not_held_out_forward():
    v = certify_mod.cell_verdicts([_row(window="2024")])[("BTC", "1h", "composite")]
    assert v["verdict"] == certify_mod.VERDICT_NOT_HELD_OUT
    assert v["n_survive_and_aligned"] == 1
    assert v["n_survive_aligned_held_out"] == 0


def test_cell_verdict_no_directional_rows():
    v = certify_mod.cell_verdicts(
        [_row(state="ranging", policy_dir=0, sign_aligned=False)]
    )[("BTC", "1h", "composite")]
    assert v["verdict"] == certify_mod.VERDICT_NO_DIRECTIONAL_ROWS
    assert v["n_directional_rows"] == 0
    assert v["min_p_value"] is None


def test_cell_verdicts_cover_every_screened_cell_not_only_certified_ones():
    rows = [_row(symbol="BTC/USDT"), _row(symbol="ETH/USDT", p=0.9),
            _row(symbol="HYPE/USDC:USDC", p=0.9, window="is")]
    v = certify_mod.cell_verdicts(rows)
    assert set(v) == {("BTC", "1h", "composite"), ("ETH", "1h", "composite"),
                      ("HYPE", "1h", "composite")}
    assert {e["global_bh_family_size"] for e in v.values()} == {3}


def test_cell_verdict_best_row_records_the_data_source():
    rows = [dict(_row(symbol="HYPE/USDC:USDC", window="is"),
                 source="hyperliquid")]
    v = certify_mod.cell_verdicts(rows)[("HYPE", "1h", "composite")]
    assert v["best_row"]["source"] == "hyperliquid"


def test_cell_verdicts_handle_repeated_and_equal_row_objects():
    shared = _row(symbol="ETH/USDT", p=0.4)
    rows = [shared, shared, _row(symbol="ETH/USDT", p=0.4), _row(p=0.001)]
    v = certify_mod.cell_verdicts(rows)
    eth = v[("ETH", "1h", "composite")]
    assert eth["n_directional_rows"] == 3
    assert eth["global_bh_family_size"] == 4
    assert eth["min_p_value"] == pytest.approx(0.4)


def test_bh_ranks_give_tied_p_values_the_same_bar():
    ranks = certify_mod._bh_ranks([0.2, 0.1, 0.2], 0.05)
    assert [r for r, _ in ranks] == [2, 1, 2]
    assert ranks[1][1] == pytest.approx(0.05 * 1 / 3)


def test_baseline_source_violations_is_empty_on_the_baseline_venue():
    sources = {"BTC/USDT": "binanceus", "ETH/USDT": "binanceus",
               "SOL/USDT": "binanceus"}
    assert certify_mod.baseline_source_violations(sources, "binanceus") == {}


def test_baseline_source_violations_ignores_added_symbols():
    sources = {"BTC/USDT": "binanceus", "ETH/USDT": "binanceus",
               "SOL/USDT": "binanceus", "HYPE/USDC:USDC": "hyperliquid"}
    assert certify_mod.baseline_source_violations(sources, "binanceus") == {}


def test_baseline_source_violations_reports_every_repointed_default():
    sources = {"BTC/USDT": "kraken", "ETH/USDT": "coinbase",
               "SOL/USDT": "binanceus"}
    assert certify_mod.baseline_source_violations(sources, "binanceus") == {
        "BTC/USDT": "kraken", "ETH/USDT": "coinbase"}


def test_repointed_baseline_refuses_to_write_the_repo_artifact(monkeypatch):
    with pytest.raises(SystemExit) as exc:
        certify_mod.main(["--symbols", "BTC/USDT@kraken,ETH/USDT,SOL/USDT"])
    assert "repointed baseline" in str(exc.value)
    assert "BTC/USDT@kraken" in str(exc.value)


def test_repointed_baseline_may_be_written_elsewhere(tmp_path, monkeypatch):
    monkeypatch.setattr(premise_mod, "run", lambda *a, **k: [_row(p=0.9)])
    out = tmp_path / "art.json"
    rc = certify_mod.main(["--symbols", "BTC/USDT@kraken,ETH/USDT,SOL/USDT",
                           "--out", str(out), "--report-out", ""])
    assert rc == 0
    assert json.loads(out.read_text())["criteria"]["data_sources"]["BTC/USDT"] == "kraken"


def test_narrowed_run_refuses_when_only_the_report_targets_the_repo(monkeypatch):
    with pytest.raises(SystemExit) as exc:
        certify_mod.main(["--symbols", "BTC/USDT", "--timeframes", "1h",
                          "--classifiers", "composite",
                          "--out", "/tmp/research_cert.json"])
    assert "narrowed family" in str(exc.value)
    assert "regime_1443_run_report.json" in str(exc.value)


def test_empty_report_out_skips_cleanly_instead_of_refusing(tmp_path, monkeypatch):
    monkeypatch.setattr(premise_mod, "run", lambda *a, **k: [_row(symbol="ETH/USDT")])
    out = tmp_path / "research.json"
    rc = certify_mod.main(["--symbols", "ETH/USDT", "--timeframes", "1h",
                           "--classifiers", "composite", "--windows", "oos",
                           "--horizons", "4", "--n-perm", "1",
                           "--out", str(out), "--report-out", ""])
    assert rc == 0
    assert out.exists()


def test_full_width_run_writes_both_repo_defaults(tmp_path, monkeypatch):
    monkeypatch.setattr(premise_mod, "run", lambda *a, **k: [_row(p=0.9)])
    art, rep = tmp_path / "art.json", tmp_path / "rep.json"
    monkeypatch.setattr(certify_mod, "DEFAULT_ARTIFACT", str(art))
    monkeypatch.setattr(certify_mod, "DEFAULT_RUN_REPORT", str(rep))
    rc = certify_mod.main(["--symbols", ",".join(premise_mod.DEFAULT_SYMBOLS)])
    assert rc == 0
    assert art.exists() and rep.exists()


def test_zero_coverage_run_refuses_and_leaves_the_artifact_untouched(tmp_path, monkeypatch):
    monkeypatch.setattr(premise_mod, "run", lambda *a, **k: [])
    art = tmp_path / "art.json"
    art.write_text('{"sentinel": true}\n')
    monkeypatch.setattr(certify_mod, "DEFAULT_ARTIFACT", str(art))
    with pytest.raises(SystemExit) as exc:
        certify_mod.main(["--symbols", ",".join(premise_mod.DEFAULT_SYMBOLS),
                          "--report-out", ""])
    assert "NO directional rows" in str(exc.value)
    assert json.loads(art.read_text()) == {"sentinel": True}


def test_under_resolved_run_refuses_the_repo_artifact(tmp_path, monkeypatch):
    monkeypatch.setattr(premise_mod, "run",
                        lambda *a, **k: [_row(p=0.5) for _ in range(400)])
    monkeypatch.setattr(certify_mod, "DEFAULT_ARTIFACT", str(tmp_path / "art.json"))
    with pytest.raises(SystemExit) as exc:
        certify_mod.main(["--symbols", ",".join(premise_mod.DEFAULT_SYMBOLS),
                          "--n-perm", "500", "--report-out", ""])
    assert "p-floor" in str(exc.value)
    assert not (tmp_path / "art.json").exists()


def test_degenerate_research_run_written_elsewhere_is_allowed(tmp_path, monkeypatch):
    monkeypatch.setattr(premise_mod, "run", lambda *a, **k: [])
    out = tmp_path / "research.json"
    rc = certify_mod.main(["--symbols", "BTC/USDT", "--timeframes", "1h",
                           "--classifiers", "composite", "--windows", "oos",
                           "--horizons", "4", "--allow-narrowed-family",
                           "--out", str(out), "--report-out", str(tmp_path / "r.json")])
    assert rc == 0
    assert json.loads(out.read_text())["certified"] == []


def test_allow_degenerate_run_is_separate_from_allow_narrowed_family(tmp_path, monkeypatch):
    monkeypatch.setattr(premise_mod, "run", lambda *a, **k: [])
    monkeypatch.setattr(certify_mod, "DEFAULT_ARTIFACT", str(tmp_path / "art.json"))
    with pytest.raises(SystemExit) as exc:
        certify_mod.main(["--symbols", "BTC/USDT", "--timeframes", "1h",
                          "--classifiers", "composite", "--windows", "oos",
                          "--horizons", "4", "--allow-narrowed-family",
                          "--report-out", ""])
    assert "NO directional rows" in str(exc.value)
    rc = certify_mod.main(["--symbols", "BTC/USDT", "--timeframes", "1h",
                           "--classifiers", "composite", "--windows", "oos",
                           "--horizons", "4", "--allow-narrowed-family",
                           "--allow-degenerate-run", "--report-out", ""])
    assert rc == 0
    assert json.loads((tmp_path / "art.json").read_text())["certified"] == []


def test_n_perm_default_resolves_the_full_screen():
    n_perm = certify_mod.build_parser().parse_args([]).n_perm
    assert certify_mod.permutation_p_floor(n_perm) <= 0.05 / 1319


def test_cert_asset_collisions_flags_two_venues_for_one_asset():
    got = certify_mod.cert_asset_collisions(
        premise_mod.parse_symbols_arg("BTC/USDT,BTC/USDC:USDC@hyperliquid"))
    assert got == {"BTC": ["BTC/USDC:USDC", "BTC/USDT"]}


def test_cert_asset_collisions_accepts_distinct_assets():
    assert certify_mod.cert_asset_collisions(premise_mod.parse_symbols_arg(
        "BTC/USDT,ETH/USDT,SOL/USDT,HYPE/USDC:USDC@hyperliquid")) == {}


def test_colliding_assets_refuse_before_any_data_access():
    with pytest.raises(SystemExit) as exc:
        certify_mod.main(["--symbols",
                          "BTC/USDT,ETH/USDT,SOL/USDT,BTC/USDC:USDC@hyperliquid"])
    assert "normalize to the same certification asset" in str(exc.value)


def test_step_up_cutoff_is_none_when_the_family_rejects_nothing():
    assert certify_mod.bh_step_up_cutoff([0.3, 0.4, 0.5], 0.05) is None
    assert certify_mod.bh_step_up_cutoff([], 0.05) is None


def test_step_up_cutoff_reports_the_bar_the_family_cleared():
    assert certify_mod.bh_step_up_cutoff([0.04, 0.041], 0.05) == pytest.approx(0.05)


def test_reported_bar_never_contradicts_the_verdict():
    rows = [_row(symbol="BTC/USDT", p=0.04), _row(symbol="ETH/USDT", p=0.041)]
    v = certify_mod.cell_verdicts(rows, fdr_q=0.05)
    for entry in v.values():
        assert entry["verdict"] == certify_mod.VERDICT_CERTIFIED
        assert entry["min_p_value"] <= entry["bh_threshold"]
    btc = v[("BTC", "1h", "composite")]
    assert btc["bh_rank_threshold"] == pytest.approx(0.025)
    assert btc["bh_step_up_cutoff"] == pytest.approx(0.05)


def test_tied_p_values_straddling_the_cut_report_one_consistent_bar():
    rows = [_row(symbol=s, p=0.03) for s in ("BTC/USDT", "ETH/USDT", "SOL/USDT")]
    v = certify_mod.cell_verdicts(rows, fdr_q=0.05)
    assert {e["verdict"] for e in v.values()} == {certify_mod.VERDICT_CERTIFIED}
    bars = [e["bh_threshold"] for e in v.values()]
    assert bars == [pytest.approx(0.05)] * 3
    for e in v.values():
        assert e["min_p_value"] <= e["bh_threshold"]


def test_failing_family_keeps_the_per_rank_bar():
    rows = [_row(symbol="ETH/USDT", p=0.30 + i * 0.001) for i in range(200)]
    v = certify_mod.cell_verdicts(rows)[("ETH", "1h", "composite")]
    assert v["bh_step_up_cutoff"] is None
    assert v["bh_threshold"] == pytest.approx(0.05 / 200)
    assert v["bh_threshold"] == v["bh_rank_threshold"]


def test_certified_cell_does_not_display_a_wrong_signed_best_row():
    rows = [_row(p=1e-6, sign_aligned=False), _row(p=1e-5, sign_aligned=True)]
    v = certify_mod.cell_verdicts(rows)[("BTC", "1h", "composite")]
    assert v["verdict"] == certify_mod.VERDICT_CERTIFIED
    assert v["best_row"]["sign_aligned"] is True
    assert v["best_row"]["p_value"] == 1e-5
    assert v["best_row_basis"] == certify_mod.BASIS_CERTIFIED_HELD_OUT
    assert v["min_p_value"] == 1e-6


def test_certified_cell_does_not_display_a_historical_window_best_row():
    rows = [_row(p=1e-6, window="2024"), _row(p=1e-5, window="oos")]
    v = certify_mod.cell_verdicts(rows)[("BTC", "1h", "composite")]
    assert v["verdict"] == certify_mod.VERDICT_CERTIFIED
    assert v["best_row"]["window"] == "oos"
    assert v["best_row_basis"] == certify_mod.BASIS_CERTIFIED_HELD_OUT


def test_not_held_out_cell_displays_an_aligned_row():
    rows = [_row(p=1e-6, window="2024", sign_aligned=False),
            _row(p=1e-5, window="2024", sign_aligned=True)]
    v = certify_mod.cell_verdicts(rows)[("BTC", "1h", "composite")]
    assert v["verdict"] == certify_mod.VERDICT_NOT_HELD_OUT
    assert v["best_row"]["sign_aligned"] is True
    assert v["best_row_basis"] == certify_mod.BASIS_SURVIVED_ALIGNED


def test_wrong_signed_cell_displays_a_surviving_row():
    rows = [_row(p=1e-6, sign_aligned=False), _row(p=1e-5, sign_aligned=False)]
    v = certify_mod.cell_verdicts(rows)[("BTC", "1h", "composite")]
    assert v["verdict"] == certify_mod.VERDICT_WRONG_SIGNED
    assert v["best_row"]["p_value"] == 1e-6
    assert v["best_row_basis"] == certify_mod.BASIS_SURVIVED_BH


def test_single_row_cells_display_that_row_unchanged():
    for row, verdict in ((_row(p=1e-6), certify_mod.VERDICT_CERTIFIED),
                         (_row(p=1e-6, window="2024"),
                          certify_mod.VERDICT_NOT_HELD_OUT),
                         (_row(p=1e-6, sign_aligned=False),
                          certify_mod.VERDICT_WRONG_SIGNED),
                         (_row(p=0.9), certify_mod.VERDICT_FAILS_GLOBAL_BH)):
        v = certify_mod.cell_verdicts([row])[("BTC", "1h", "composite")]
        assert v["verdict"] == verdict
        assert v["best_row"]["p_value"] == row["p_value"] == v["min_p_value"]


def test_failing_cell_still_displays_its_globally_lowest_p_row():
    rows = [_row(p=0.5), _row(p=0.9)]
    v = certify_mod.cell_verdicts(rows)[("BTC", "1h", "composite")]
    assert v["verdict"] == certify_mod.VERDICT_FAILS_GLOBAL_BH
    assert v["best_row"]["p_value"] == 0.5 == v["min_p_value"]
    assert v["best_row_basis"] == certify_mod.BASIS_ALL_DIRECTIONAL


def test_verdict_line_names_the_displayed_row_when_it_is_not_the_minimum():
    rows = [_row(p=1e-6, sign_aligned=False), _row(p=1e-5, sign_aligned=True)]
    v = certify_mod.cell_verdicts(rows)[("BTC", "1h", "composite")]
    line = certify_mod._format_verdict_line(v)
    assert "min_p=1e-06" in line
    assert "p=1e-05" in line
    assert f"basis={certify_mod.BASIS_CERTIFIED_HELD_OUT}" in line


def test_verdict_line_is_unchanged_when_the_displayed_row_is_the_minimum():
    v = certify_mod.cell_verdicts([_row(p=0.5)])[("BTC", "1h", "composite")]
    line = certify_mod._format_verdict_line(v)
    assert " p=" not in line
    assert "basis=" not in line

"""#1085: the certification producer's pure gate (regime_1076_certify.certify).

Tests the global-correction + sign-alignment + held-out-forward gate over
synthetic premise-screen rows — no data access."""
import json
import os
import sys
from datetime import datetime, timezone

sys.path.insert(0, os.path.abspath(
    os.path.join(os.path.dirname(__file__), "..", "research")))

import regime_1076_certify as certify_mod  # noqa: E402

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
    # Same strong row but in a historical window → not held-out-forward.
    art = certify_mod.certify([_row(window="2024")], generated_at=GEN_AT)
    assert art["certified"] == []


def test_wrong_signed_not_certified():
    art = certify_mod.certify([_row(sign_aligned=False)], generated_at=GEN_AT)
    assert art["certified"] == []


def test_non_significant_not_certified():
    # A large p-value should not survive global BH even alone.
    art = certify_mod.certify([_row(p=0.9)], generated_at=GEN_AT)
    assert art["certified"] == []


def test_composite_sublabel_maps_to_canonical():
    art = certify_mod.certify(
        [_row(state="trending_down_strong", policy_dir=-1)], generated_at=GEN_AT)
    assert len(art["certified"]) == 1
    assert art["certified"][0]["states"] == {"trending_down": "short"}


# ==========================================================================
# #1443 — criteria provenance, family integrity, per-cell verdicts
# ==========================================================================
import pytest  # noqa: E402

import regime_1076_directional_premise as premise_mod  # noqa: E402

FULL_UNIVERSE = dict(
    symbols=premise_mod.DEFAULT_SYMBOLS,
    timeframes=premise_mod.DEFAULT_TIMEFRAMES,
    windows=premise_mod.DEFAULT_WINDOWS,
    classifiers=premise_mod.DEFAULT_CLASSIFIERS,
    horizons=premise_mod.DEFAULT_HORIZONS,
)


# -------------------------------------------------------------- criteria
def test_screened_family_size_counts_directional_rows_only():
    # A ranging row is not part of the directional BH family and must not
    # inflate the recorded family size.
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
    # The Go loader parses with DisallowUnknownFields and only `criteria` is a
    # free-form map, so NO new key may appear at the top level.
    assert set(art) == {"schema_version", "generated_at", "generator",
                        "source_evidence", "criteria", "default_ttl_days",
                        "certified"}
    assert art["schema_version"] == 1


def test_permutation_p_floor_arithmetic():
    assert certify_mod.permutation_p_floor(500) == pytest.approx(1 / 501)
    assert certify_mod.permutation_p_floor(30000) == pytest.approx(1 / 30001)


# ------------------------------------------------------- family integrity
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
    # Sourcing a default symbol elsewhere still covers that axis value.
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
    # The gate fires BEFORE any data access, so this never touches the cache.
    with pytest.raises(SystemExit) as exc:
        certify_mod.main(["--symbols", "ETH/USDT", "--timeframes", "1h",
                          "--classifiers", "composite"])
    assert "narrowed family" in str(exc.value)


def test_narrowed_run_may_write_a_research_artifact_elsewhere(tmp_path, monkeypatch):
    # Same narrowed universe, but writing OUTSIDE the repo artifact: allowed,
    # with the narrowing warned about and recorded. premise.run is substituted so
    # this stays a pure test.
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
    # Narrowed on purpose: the recorded universe makes that inspectable later.
    assert art["criteria"]["universe"]["symbols"] == ["ETH/USDT"]
    assert art["criteria"]["screened_family_size"] == 1
    assert json.loads(report.read_text())["cell_verdicts"][0]["verdict"] == (
        certify_mod.VERDICT_CERTIFIED)


def test_repo_artifact_run_over_the_full_family_is_allowed(tmp_path, monkeypatch):
    # The inverse of the refusal above: a superset universe writing the repo
    # artifact path proceeds. Guards against a guard that refuses everything.
    monkeypatch.setattr(premise_mod, "run", lambda *a, **k: [])
    monkeypatch.setattr(certify_mod, "DEFAULT_ARTIFACT", str(tmp_path / "art.json"))
    rc = certify_mod.main([
        "--symbols", "BTC/USDT,ETH/USDT,SOL/USDT,HYPE/USDC:USDC@hyperliquid",
        "--out", str(tmp_path / "art.json"),
        "--report-out", str(tmp_path / "rep.json")])
    assert rc == 0
    assert json.loads((tmp_path / "art.json").read_text())["certified"] == []


# ---------------------------------------------------------- cell verdicts
def test_cell_verdict_certified_matches_the_artifact():
    rows = [_row()]
    v = certify_mod.cell_verdicts(rows)[("BTC", "1h", "composite")]
    assert v["verdict"] == certify_mod.VERDICT_CERTIFIED
    assert v["certified_states"] == {"trending_up": "long"}
    assert v["global_bh_family_size"] == 1
    assert v["bh_rank"] == 1


def test_cell_verdict_reports_the_bh_bar_it_missed():
    # 200 weak rows: the best one is nowhere near the rank-1 critical value.
    rows = [_row(symbol="ETH/USDT", p=0.30 + i * 0.001) for i in range(200)]
    v = certify_mod.cell_verdicts(rows)[("ETH", "1h", "composite")]
    assert v["verdict"] == certify_mod.VERDICT_FAILS_GLOBAL_BH
    assert v["min_p_value"] == pytest.approx(0.30)
    assert v["bh_rank"] == 1
    assert v["bh_threshold"] == pytest.approx(0.05 / 200)
    assert v["n_survive_global_bh"] == 0


def test_cell_verdict_wrong_signed_outranks_the_held_out_check():
    # Strong and BH-surviving, but the separation points against the policy bet:
    # the binding failure is the sign, not the window.
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
    # The BH family is the whole run, so every cell reports the same denominator.
    assert {e["global_bh_family_size"] for e in v.values()} == {3}


def test_cell_verdict_best_row_records_the_data_source():
    rows = [dict(_row(symbol="HYPE/USDC:USDC", window="is"),
                 source="hyperliquid")]
    v = certify_mod.cell_verdicts(rows)[("HYPE", "1h", "composite")]
    assert v["best_row"]["source"] == "hyperliquid"


def test_cell_verdicts_handle_repeated_and_equal_row_objects():
    # Rows are matched to BH ranks by POSITION in the family. Equal-valued rows —
    # or literally the same dict passed twice — must each occupy their own slot,
    # otherwise a cell's reported bar belongs to a different hypothesis.
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

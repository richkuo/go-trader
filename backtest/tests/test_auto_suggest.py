"""#1210: pure-helper unit tests for the M1-M6 auto-suggester.

Every test exercises a PURE helper against fixture payload dicts — no subprocess
spawning, no market-data access, no live-config touch (matching the repo's
"extract pure helpers from subprocess wrappers" rule so Go/Python CI never
depends on running a harness)."""
import copy
import json
import os

import pytest

import auto_suggest as asug
from regime_stats import benjamini_hochberg

_STUDY_DIR = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))),
                          "candidates", "squeeze_momentum_1198")


# --------------------------------------------------------------------------
# 1. Spec loading / validation
# --------------------------------------------------------------------------

def _base_spec(**over):
    spec = {
        "study": "t",
        "registry": "spot",
        "harnesses": ["m1_noise", "m1"],
        "windows": ["is", "oos"],
        "correction": {"method": "benjamini_hochberg", "alpha": 0.05},
        "candidates": [{"key": "c1", "candidate": {"name": "squeeze_momentum",
                                                   "direction": "long"}}],
    }
    spec.update(over)
    return spec


def test_load_spec_accepts_committed_1198_shape():
    with open(os.path.join(_STUDY_DIR, "suggest.json")) as fh:
        raw = json.load(fh)
    spec = asug.load_spec(raw, _STUDY_DIR)
    assert spec["registry"] == "spot"
    assert [c["key"] for c in spec["candidates"]] == ["baseline", "adx_not_down",
                                                      "comp_up_family"]
    # file refs resolved to dicts
    assert spec["candidates"][1]["candidate"]["allowed_regimes"] == ["trending_up", "ranging"]


def test_load_spec_rejects_unknown_harness():
    with pytest.raises(ValueError, match="unknown harnesses"):
        asug.load_spec(_base_spec(harnesses=["m1", "m9"]), _STUDY_DIR)


def test_load_spec_rejects_unknown_window():
    with pytest.raises(ValueError, match="unknown windows"):
        asug.load_spec(_base_spec(windows=["is", "nope"]), _STUDY_DIR)


def test_load_spec_rejects_unknown_registry():
    with pytest.raises(ValueError, match="registry must be"):
        asug.load_spec(_base_spec(registry="options"), _STUDY_DIR)


def test_load_spec_rejects_non_bh_correction():
    with pytest.raises(ValueError, match="benjamini_hochberg"):
        asug.load_spec(_base_spec(correction={"method": "bonferroni"}), _STUDY_DIR)


def test_load_spec_runs_candidate_validator():
    # allowed_regimes as a bare string is the #1031 trap validate_candidate catches.
    bad = _base_spec(candidates=[{"key": "c", "candidate": {
        "name": "squeeze_momentum", "allowed_regimes": "trending_up"}}])
    with pytest.raises(ValueError, match="allowed_regimes"):
        asug.load_spec(bad, _STUDY_DIR)


# --------------------------------------------------------------------------
# 2. Candidate expansion
# --------------------------------------------------------------------------

def test_expand_explicit_candidates():
    spec = asug.load_spec(_base_spec(), _STUDY_DIR)
    entries = asug.expand_candidates(spec)
    assert [e["key"] for e in entries] == ["c1"]
    assert entries[0]["kind"] == "open"
    assert entries[0]["harnesses"] == ["m1_noise", "m1"]


def test_expand_sweep_and_gate_variants_cartesian_deterministic():
    spec = asug.load_spec(_base_spec(
        candidates=[],
        base={"name": "squeeze_momentum", "direction": "long", "params": {"kc_mult": 1.5}},
        sweep={"kc_mult": [1.3, 1.5]},
        gate_variants=[
            {"label": "up", "allowed_regimes": ["trending_up_clean"],
             "regime_windows_spec": {"medium": {"classifier": "composite", "period": 21}}},
            {"label": "rng", "allowed_regimes": ["ranging_quiet"]},
        ],
    ), _STUDY_DIR)
    entries = asug.expand_candidates(spec)
    keys = [e["key"] for e in entries]
    assert keys == [
        "squeeze_momentum.kc_mult1.3.up", "squeeze_momentum.kc_mult1.3.rng",
        "squeeze_momentum.kc_mult1.5.up", "squeeze_momentum.kc_mult1.5.rng",
    ]
    up = next(e for e in entries if e["key"].endswith(".up"))
    assert up["candidate"]["allowed_regimes"] == ["trending_up_clean"]
    assert up["candidate"]["params"]["kc_mult"] == 1.3


def test_expand_rejects_duplicate_keys():
    spec = asug.load_spec(_base_spec(candidates=[
        {"key": "dup", "candidate": {"name": "squeeze_momentum"}},
        {"key": "dup", "candidate": {"name": "squeeze_momentum"}},
    ]), _STUDY_DIR)
    with pytest.raises(ValueError, match="duplicate candidate key"):
        asug.expand_candidates(spec)


def test_expand_sweep_requires_base():
    spec = asug.load_spec(_base_spec(candidates=[], sweep={"kc_mult": [1.3]}), _STUDY_DIR)
    with pytest.raises(ValueError, match="require a 'base'"):
        asug.expand_candidates(spec)


def test_close_stack_specs_round_trip_through_optimizer_grid():
    from optimizer import generate_close_stack_grid
    stack_specs = [{"close": {"name": "atr_stop", "params": {"atr_mult": [2.0, 2.5]}}}]
    expected = len(generate_close_stack_grid(stack_specs))
    spec = asug.load_spec(_base_spec(
        candidates=[],
        m6={"baseline_config": "cfg.json", "strategy_id": "s",
            "close_stack_specs": stack_specs},
    ), _STUDY_DIR)
    entries = asug.expand_candidates(spec)
    ab = [e for e in entries if e["kind"] == "exit_ab"]
    assert len(ab) == expected


# --------------------------------------------------------------------------
# 3. Preconditions (replayability, m5 params limitation)
# --------------------------------------------------------------------------

def test_non_replayable_m6_close_excluded():
    spec = asug.load_spec(_base_spec(candidates=[], base=None, m6={
        "baseline_config": "cfg.json", "strategy_id": "s",
        "candidate_close_variants": [
            {"key": "bad", "candidate_close": [{"name": "tiered_tp_atr_live_regime_dynamic"}]},
            {"key": "good", "candidate_close": [{"name": "atr_stop", "params": {"atr_mult": 2}}]},
        ]}), _STUDY_DIR)
    entries = {e["key"]: e for e in asug.expand_candidates(spec)}
    assert entries["m6.bad"]["precondition_errors"] == ["excluded_not_replayable"]
    assert entries["m6.good"]["precondition_errors"] == []


def test_m6_requires_exactly_one_incumbent_source():
    base = dict(candidates=[])
    both = _base_spec(**base, m6={"strategy_id": "s", "baseline_config": "cfg.json",
                                  "incumbent_close": [{"name": "tiered_tp_atr"}],
                                  "candidate_close_variants": []})
    with pytest.raises(ValueError, match="EXACTLY one"):
        asug.load_spec(both, _STUDY_DIR)
    neither = _base_spec(**base, m6={"strategy_id": "s",
                                     "candidate_close_variants": []})
    with pytest.raises(ValueError, match="EXACTLY one"):
        asug.load_spec(neither, _STUDY_DIR)


def test_m6_incumbent_close_only_spec_loads():
    spec = asug.load_spec(_base_spec(candidates=[], m6={
        "strategy_id": "squeeze_momentum",
        "incumbent_close": [{"name": "tiered_tp_atr", "params": {}}],
        "candidate_close_variants": [
            {"key": "v", "candidate_close": [{"name": "atr_stop", "params": {"atr_mult": 2}}]}]},
    ), _STUDY_DIR)
    entries = asug.expand_candidates(spec)
    ab = [e for e in entries if e["kind"] == "exit_ab"]
    assert len(ab) == 1 and ab[0]["precondition_errors"] == []
    assert ab[0]["candidate"]["baseline_config"] is None


def test_m6_missing_strategy_id_everywhere_fails_at_load():
    # (a) incumbent_close set, no strategy_id at m6 OR variant level -> must
    # raise at load, not surface as a broken '--strategy None' subprocess.
    bad = _base_spec(candidates=[], m6={
        "incumbent_close": [{"name": "tiered_tp_atr", "params": {}}],
        "candidate_close_variants": [
            {"key": "v", "candidate_close": [{"name": "atr_stop", "params": {"atr_mult": 2}}]}]})
    with pytest.raises(ValueError, match="strategy_id"):
        asug.load_spec(bad, _STUDY_DIR)


def test_m6_per_variant_strategy_id_override_loads_without_m6_default():
    # (b) m6 block omits strategy_id but each variant supplies its own -> legit.
    spec = asug.load_spec(_base_spec(candidates=[], m6={
        "incumbent_close": [{"name": "tiered_tp_atr", "params": {}}],
        "candidate_close_variants": [
            {"key": "v", "strategy_id": "squeeze_momentum",
             "candidate_close": [{"name": "atr_stop", "params": {"atr_mult": 2}}]}]},
    ), _STUDY_DIR)
    ab = [e for e in asug.expand_candidates(spec) if e["kind"] == "exit_ab"]
    assert len(ab) == 1 and ab[0]["candidate"]["strategy_id"] == "squeeze_momentum"


def test_m6_close_stack_specs_require_m6_level_strategy_id():
    # close_stack variants are generated and cannot carry a per-variant
    # strategy_id, so an m6-level default is mandatory when they are present.
    bad = _base_spec(candidates=[], m6={
        "incumbent_close": [{"name": "tiered_tp_atr", "params": {}}],
        "candidate_close_variants": [],
        "close_stack_specs": [{"close": {"name": "atr_stop", "params": {"atr_mult": [2.0]}}}]})
    with pytest.raises(ValueError, match="close_stack_specs"):
        asug.load_spec(bad, _STUDY_DIR)


def test_m6_baseline_config_path_also_requires_strategy_id():
    # (c) the baseline_config path embeds --strategy too; same missing-value guard.
    bad = _base_spec(candidates=[], m6={
        "baseline_config": "cfg.json",
        "candidate_close_variants": [
            {"key": "v", "candidate_close": [{"name": "atr_stop", "params": {"atr_mult": 2}}]}]})
    with pytest.raises(ValueError, match="strategy_id"):
        asug.load_spec(bad, _STUDY_DIR)


_TEMPLATE = os.path.join(os.path.dirname(_STUDY_DIR), "suggest.template.jsonc")


def _load_template_json():
    import re
    return json.loads(re.sub(r"//.*", "", open(_TEMPLATE).read()))


def test_template_documents_every_variant_and_candidate_option():
    # The template header asserts it "documents EVERY option the loader accepts";
    # three prior review rounds each found one omitted key. Lock in the fixes so
    # a future edit can't silently regress the completeness claim.
    raw = _load_template_json()
    # per-candidate harness override (auto_suggest.py: c.get("harnesses"))
    assert any("harnesses" in c for c in raw["candidates"]), "template omits per-candidate harnesses"
    m6 = raw["m6"]
    # m6-level allowed_regimes default (seeds close_stack variants)
    assert "allowed_regimes" in m6, "template omits m6-level allowed_regimes"
    # per-variant strategy_id override (variant.get("strategy_id"))
    assert any("strategy_id" in v for v in m6["candidate_close_variants"]), \
        "template omits per-variant strategy_id override"


def test_shipped_full_options_spec_loads_and_expands():
    # The committed all-options default must load and expand cleanly (guards the
    # demo from silently rotting — every generator + M6 exercised).
    with open(os.path.join(_STUDY_DIR, "suggest.json")) as fh:
        raw = json.load(fh)
    spec = asug.load_spec(raw, _STUDY_DIR)
    entries = asug.expand_candidates(spec)
    kinds = {e["kind"] for e in entries}
    assert kinds == {"open", "exit_ab"}          # both harness families present
    ab = [e for e in entries if e["kind"] == "exit_ab"]
    assert all(e["precondition_errors"] == [] for e in ab)   # all M6 closes replayable
    assert len({e["key"] for e in entries}) == len(entries)  # keys unique


def test_m5_params_limitation_flagged():
    spec = asug.load_spec(_base_spec(
        harnesses=["m5"],
        candidates=[{"key": "c", "candidate": {"name": "squeeze_momentum",
                                               "params": {"kc_mult": 2.0}}}]), _STUDY_DIR)
    entry = asug.expand_candidates(spec)[0]
    assert "m5_params_unaudited" in entry["limitations"]


# --------------------------------------------------------------------------
# 4. argv-tail builders (golden)
# --------------------------------------------------------------------------

def test_m1_argv_tail():
    assert asug.m1_argv_tail("/t/c.json", "spot", ["is", "oos"],
                             ["BTC/USDT:1h"], "/t/o.json") == [
        "--candidate-json", "/t/c.json", "--registry", "spot",
        "--windows", "is,oos", "--json", "/t/o.json",
        "--datasets", "BTC/USDT:1h"]


def test_m1_argv_tail_omits_datasets_when_none():
    tail = asug.m1_argv_tail("/t/c.json", "spot", ["is"], None, "/t/o.json")
    assert "--datasets" not in tail


def test_noise_argv_tail_threads_seed_and_direction():
    tail = asug.noise_argv_tail("sq", '{"kc_mult": 2}', "futures", "short",
                                ["is", "oos"], None, 500, 1066, 0.05, "/t/n.json")
    assert tail[:8] == ["--strategy", "sq", "--registry", "futures",
                        "--windows", "is,oos", "--resamples", "500"]
    assert "--seed" in tail and "1066" in tail
    assert "--direction" in tail and "short" in tail
    assert "--params" in tail


def test_m6_argv_tail_repeats_allowed_regimes():
    m6c = {"baseline_config": "/cfg", "strategy_id": "s",
           "candidate_close": [{"name": "atr_stop"}], "candidate_stops": "inherit",
           "allowed_regimes": ["ranging_quiet", "ranging_volatile"]}
    tail = asug.m6_argv_tail(m6c, "futures", ["is", "oos"], None, 10000, 1066, "/t/m6.json")
    assert tail.count("--allowed-regimes") == 2
    assert "--bootstrap-resamples" in tail and "10000" in tail
    assert "--candidate-stops" in tail and "inherit" in tail


def test_m6_argv_tail_baseline_config_path():
    m6c = {"baseline_config": "/cfg.json", "strategy_id": "s",
           "candidate_close": [{"name": "atr_stop"}]}
    tail = asug.m6_argv_tail(m6c, "spot", ["oos"], None, 100, 1066, "/t/m6.json")
    assert "--baseline-config" in tail and "/cfg.json" in tail
    assert "--incumbent-close" not in tail  # exactly one incumbent source


def test_m6_argv_tail_incumbent_close_path_omits_baseline():
    # The self-contained path: no baseline_config, an explicit incumbent ladder.
    m6c = {"strategy_id": "squeeze_momentum",
           "incumbent_close": [{"name": "tiered_tp_atr", "params": {}}],
           "candidate_close": [{"name": "atr_stop", "params": {"atr_mult": 2}}]}
    tail = asug.m6_argv_tail(m6c, "spot", ["oos"], None, 100, 1066, "/t/m6.json")
    assert "--baseline-config" not in tail
    assert "--incumbent-close" in tail
    assert "--strategy" in tail and "squeeze_momentum" in tail


def test_m5_argv_tail():
    tail = asug.m5_argv_tail("sq", "spot", None, ["oos"], None, "/t/m5.json")
    assert tail == ["--strategies", "sq", "--registry", "spot",
                    "--windows", "oos", "--json", "/t/m5.json"]


# --------------------------------------------------------------------------
# 5. Extractors / rollup
# --------------------------------------------------------------------------

def _m6_payload(is_rows, oos_rows):
    def _mk(rows):
        return [{"dataset": ds, "per_regime": {"all": {
            "n": n, "paired_delta": {"mean": mean, "signed_rank": {"p_value": p}}}}}
            for ds, mean, n, p in rows]
    return {"results": {"is": _mk(is_rows), "oos": _mk(oos_rows)}}


def test_m6_window_rollup_matches_paired_n_weighting():
    payload = _m6_payload(
        is_rows=[("BTC 1h", 0.10, 100, 0.01), ("ETH 1h", -0.20, 50, 0.30)],
        oos_rows=[("BTC 1h", 0.05, 40, 0.20)])
    roll = asug.m6_window_rollup(payload)
    # pooled = (0.10*100 + -0.20*50) / 150 = 0.0
    assert roll["is"]["pooled_delta_net_pct_per_entry"] == 0.0
    assert roll["is"]["paired_n"] == 150
    assert roll["is"]["datasets_delta_pos"] == 1
    assert roll["is"]["datasets_delta_neg"] == 1
    assert roll["is"]["per_dataset"][0] == {"dataset": "BTC 1h", "mean": 0.10,
                                            "n": 100, "p": 0.01}


def test_m6_rollup_skips_none_mean_datasets():
    payload = {"results": {"oos": [
        {"dataset": "A", "per_regime": {"all": {"n": 10,
         "paired_delta": {"mean": None, "signed_rank": {"p_value": None}}}}},
        {"dataset": "B", "per_regime": {"all": {"n": 20,
         "paired_delta": {"mean": 0.3, "signed_rank": {"p_value": 0.04}}}}},
    ]}}
    roll = asug.m6_window_rollup(payload)
    assert roll["oos"]["paired_n"] == 20
    assert len(roll["oos"]["per_dataset"]) == 1


def test_m6_rollup_missing_results_is_empty():
    assert asug.m6_window_rollup({}) == {}


def test_extract_noise():
    payload = {"trade_level": {"verdict": "distinguishable_positive",
                               "permutation": {"p_value": 0.012, "mean": 0.4},
                               "summary": {"n": 88}}}
    assert asug.extract_noise(payload) == {"verdict": "distinguishable_positive",
                                           "permutation_p": 0.012, "mean": 0.4, "n": 88}


def test_extract_m1():
    payload = {"window_scores": [
        {"window": "is", "verdict": "pass", "mean_sharpe": 1.2, "mean_ddadj": 0.8},
        {"window": "oos", "verdict": "fail", "mean_sharpe": -0.1, "mean_ddadj": 0.0}]}
    out = asug.extract_m1(payload)
    assert out["is"]["verdict"] == "pass"
    assert out["oos"]["verdict"] == "fail"


def test_extract_m5_matches_strategy_row():
    payload = {"rows": [
        {"strategy": "other", "verdict": "healthy"},
        {"strategy": "sq", "verdict": "graduate_m1", "fee_drag_pp": 0.3,
         "trades_per_year": 12.0, "mean_gross_ret": 0.5, "mean_net_ret": 0.2}]}
    out = asug.extract_m5(payload, "sq")
    assert out["salvage_verdict"] == "graduate_m1"
    assert out["fee_drag_pp"] == 0.3


# --------------------------------------------------------------------------
# 6. Family correction
# --------------------------------------------------------------------------

def test_apply_correction_matches_direct_bh_and_reports_threshold():
    tests = [{"candidate_key": "a", "harness": "m6", "p": p, "effect_positive": True}
             for p in [0.001, 0.02, 0.04, 0.5]]
    corr = asug.apply_family_correction(copy.deepcopy(tests), alpha=0.05)
    mask = benjamini_hochberg([t["p"] for t in tests], 0.05)
    # stamped bh_pass agrees with a direct BH call
    stamped = asug.apply_family_correction(tests, 0.05) and [t["bh_pass"] for t in tests]
    assert stamped == mask
    assert corr["m"] == 4
    assert corr["bonferroni_threshold"] == pytest.approx(0.05 / 4)
    if any(mask):
        assert corr["effective_threshold"] == max(p for p, ok in
                                                   zip([t["p"] for t in tests], mask) if ok)


def test_correction_empty_family():
    corr = asug.apply_family_correction([], alpha=0.05)
    assert corr["m"] == 0
    assert corr["effective_threshold"] is None
    assert corr["n_survivors"] == 0


def test_collect_family_pvalues_dedupes_noise_and_excludes_m3_m5():
    e1 = {"key": "a", "kind": "open", "noise_family_key": "K",
          "results": {"m1_noise": {"data": {"permutation_p": 0.01, "mean": 0.2}},
                      "m3": {"data": {"x": 1}}, "m5": {"data": {"salvage_verdict": "healthy"}}}}
    e2 = {"key": "b", "kind": "open", "noise_family_key": "K",  # same base -> deduped
          "results": {"m1_noise": {"data": {"permutation_p": 0.01, "mean": 0.2}}}}
    e3 = {"key": "c", "kind": "exit_ab",
          "results": {"m6": {"data": {"oos": {"per_dataset": [
              {"dataset": "BTC 1h", "mean": 0.3, "p": 0.02}]}}}}}
    tests = asug.collect_family_pvalues([e1, e2, e3])
    harnesses = sorted(t["harness"] for t in tests)
    assert harnesses == ["m1_noise", "m6"]  # one noise (deduped), one m6; no m3/m5


# --------------------------------------------------------------------------
# 7. Promotion gate (verdict matrix)
# --------------------------------------------------------------------------

def _open_entry(key, noise=None, m1=None, harnesses=("m1_noise", "m1"), fam=None):
    results = {}
    if noise is not None:
        results["m1_noise"] = {"status": "ok", "data": noise}
    if m1 is not None:
        results["m1"] = {"status": "ok", "data": m1}
    return {"key": key, "kind": "open", "harnesses": list(harnesses),
            "precondition_errors": [], "noise_family_key": fam or f"fam::{key}",
            "results": results}


def _noise_test(fam, key, p, bh_pass, positive=True):
    return {"candidate_key": key, "harness": "m1_noise", "noise_family_key": fam,
            "p": p, "effect_positive": positive, "bh_pass": bh_pass}


def test_verdict_run_failed_never_survivor():
    e = {"key": "x", "kind": "open", "precondition_errors": [],
         "results": {"m1": {"status": "failed"}}}
    assert asug.candidate_verdict(e, []) == "run_failed"


def test_verdict_excluded_not_replayable():
    e = {"key": "x", "kind": "exit_ab",
         "precondition_errors": ["excluded_not_replayable"], "results": {}}
    assert asug.candidate_verdict(e, []) == "excluded_not_replayable"


def test_verdict_noise_gate_blocks_before_selectivity():
    e = _open_entry("x", noise={"verdict": "no_positive_edge", "mean": -0.1},
                    m1={"is": {"verdict": "pass"}, "oos": {"verdict": "pass"}})
    assert asug.candidate_verdict(e, []) == "noise_gate_blocked"


def test_verdict_open_survivor_requires_bh_survival():
    e = _open_entry("x", fam="F", noise={"verdict": "distinguishable_positive", "mean": 0.3},
                    m1={"is": {"verdict": "pass"}, "oos": {"verdict": "pass"}})
    tests = [_noise_test("F", "x", 0.001, True)]
    assert asug.candidate_verdict(e, tests) == "survivor"
    tests[0]["bh_pass"] = False
    assert asug.candidate_verdict(e, tests) == "positive_uncorrected_only"


def test_verdict_open_m1_fail_is_incumbent_stands():
    e = _open_entry("x", fam="F", noise={"verdict": "distinguishable_positive", "mean": 0.3},
                    m1={"is": {"verdict": "pass"}, "oos": {"verdict": "fail"}})
    tests = [_noise_test("F", "x", 0.001, True)]
    assert asug.candidate_verdict(e, tests) == "incumbent_stands"


def test_gated_siblings_share_noise_bh_verdict():
    # #1210 review (Needs Fixing): candidates differing only by gate share ONE
    # noise_family_key. The deduped noise p is attached to the FIRST sibling's
    # candidate_key only, so lookup-by-family (not by candidate_key) is required
    # or the other siblings skip the BH downgrade and promote on a failed p.
    fam = "shared"
    m1_pass = {"is": {"verdict": "pass"}, "oos": {"verdict": "pass"}}
    noise = {"verdict": "distinguishable_positive", "mean": 0.3}
    siblings = [_open_entry(k, fam=fam, noise=noise, m1=m1_pass)
                for k in ("baseline", "adx_gate", "comp_gate")]
    # The deduped noise test lives under the FIRST sibling only, and it FAILS BH.
    tests = [_noise_test(fam, "baseline", 0.049, bh_pass=False)]
    verdicts = [asug.candidate_verdict(e, tests) for e in siblings]
    # (a) NONE may be survivor — every sibling gets the downgrade.
    assert verdicts == ["positive_uncorrected_only"] * 3
    # (c) reordering the entries must not change any verdict.
    verdicts_rev = [asug.candidate_verdict(e, tests) for e in reversed(siblings)]
    assert set(verdicts_rev) == {"positive_uncorrected_only"}
    # When the shared noise p SURVIVES BH, all three become survivors together.
    tests[0]["bh_pass"] = True
    assert [asug.candidate_verdict(e, tests) for e in siblings] == ["survivor"] * 3


def test_distinct_param_families_keep_independent_noise_tests():
    # (b) same name+direction, different params => distinct families => two noise
    # tests, each governing its own candidate.
    e_a = {"key": "a", "kind": "open", "noise_family_key": "famA",
           "results": {"m1_noise": {"data": {"permutation_p": 0.01, "mean": 0.2}}}}
    e_b = {"key": "b", "kind": "open", "noise_family_key": "famB",
           "results": {"m1_noise": {"data": {"permutation_p": 0.30, "mean": 0.1}}}}
    tests = asug.collect_family_pvalues([e_a, e_b])
    noise_tests = [t for t in tests if t["harness"] == "m1_noise"]
    assert len(noise_tests) == 2
    assert {t["noise_family_key"] for t in noise_tests} == {"famA", "famB"}


def _ab_entry(key, is_pooled, oos_pooled):
    return {"key": key, "kind": "exit_ab", "precondition_errors": [],
            "results": {"m6": {"status": "ok", "data": {
                "is": {"pooled_delta_net_pct_per_entry": is_pooled},
                "oos": {"pooled_delta_net_pct_per_entry": oos_pooled}}}}}


def test_verdict_m6_survivor_needs_bh_positive_and_no_contradiction():
    e = _ab_entry("x", 0.2, 0.15)
    tests = [{"candidate_key": "x", "harness": "m6", "p": 0.01,
              "effect_positive": True, "bh_pass": True}]
    assert asug.candidate_verdict(e, tests) == "survivor"


def test_verdict_m6_significant_contradiction_blocks():
    e = _ab_entry("x", 0.2, 0.15)
    tests = [{"candidate_key": "x", "harness": "m6", "p": 0.01,
              "effect_positive": True, "bh_pass": True},
             {"candidate_key": "x", "harness": "m6", "p": 0.02,
              "effect_positive": False, "bh_pass": False}]
    assert asug.candidate_verdict(e, tests) == "incumbent_stands"


def test_verdict_m6_positive_uncorrected_only():
    e = _ab_entry("x", 0.2, 0.15)
    tests = [{"candidate_key": "x", "harness": "m6", "p": 0.03,
              "effect_positive": True, "bh_pass": False}]
    assert asug.candidate_verdict(e, tests) == "positive_uncorrected_only"


def test_verdict_m6_inconclusive_on_none_pooled():
    e = _ab_entry("x", None, 0.15)
    assert asug.candidate_verdict(e, []) == "inconclusive"


def test_verdict_m6_incumbent_stands_when_not_both_positive():
    e = _ab_entry("x", 0.2, -0.05)
    assert asug.candidate_verdict(e, []) == "incumbent_stands"


# --------------------------------------------------------------------------
# 8. Ranking + report
# --------------------------------------------------------------------------

def test_rank_survivors_first_failed_still_present():
    entries = [
        {"key": "loser", "verdict": "incumbent_stands", "results": {}},
        {"key": "win", "verdict": "survivor", "results": {}},
        {"key": "broke", "verdict": "run_failed", "results": {}},
    ]
    ranked = asug.rank_shortlist(entries)
    assert ranked[0]["key"] == "win"
    assert [e["key"] for e in ranked] == ["win", "loser", "broke"]
    assert any(e["verdict"] == "run_failed" for e in ranked)  # never dropped


def test_format_shortlist_has_correction_line_context_label_and_footer():
    report = {
        "study": "t", "exploratory": False,
        "correction": {"method": "benjamini_hochberg", "alpha": 0.05, "m": 3,
                       "effective_threshold": 0.01, "bonferroni_threshold": 0.0167,
                       "n_survivors": 1},
        "ranked": [{"key": "win", "verdict": "survivor", "limitations": [],
                    "results": {"m5": {"data": {"salvage_verdict": "graduate_m1"}}}}],
    }
    text = asug.format_shortlist(report)
    assert "benjamini_hochberg" in text
    assert "UNCORRECTED CONTEXT" in text
    assert asug.FOOTER in text


def test_reproduction_command_uses_relative_harness_paths():
    entry = {"key": "x", "results": {
        "m1": {"argv_tail": ["--candidate-json", "/t/c.json", "--registry", "spot"]}}}
    cmds = asug.reproduction_command(entry)
    assert cmds and cmds[0].startswith("uv run --no-sync python backtest/eval_windows.py")
    assert "--candidate-json" in cmds[0]


# --------------------------------------------------------------------------
# 9. #1295 — advisory Monte Carlo columns must never touch the promotion gate
# --------------------------------------------------------------------------

def _survivor_entry(**results_over):
    """An entry whose GATE evidence is a clean pass: noise distinguishable,
    M1 pass on both protocol windows."""
    results = {
        "m1_noise": {"status": "ok",
                     "data": {"verdict": "distinguishable_positive",
                              "permutation_p": 0.001, "mean": 0.5, "n": 80}},
        "m1": {"status": "ok", "data": {"is": {"verdict": "pass"},
                                        "oos": {"verdict": "pass"}}},
    }
    results.update(results_over)
    return {"key": "c1", "kind": "open", "limitations": [],
            "precondition_errors": [], "noise_family_key": "fam",
            "results": results}


def _mc_ok_run():
    return {"status": "ok", "data": {"oos": {"per_dataset": {}, "worst": {
        "permute": {"p_dd_ge_kill_switch": 0.42, "p95_max_dd": 61.0,
                    "p_final_below_start": 0.3}}}}}


def _family_tests(entry):
    tests = asug.collect_family_pvalues([entry])
    asug.apply_family_correction(tests, 0.05)
    return tests


def test_mc_absent_present_and_failed_all_yield_the_same_verdict():
    # The #1274 pre-registered criterion, strengthened per #1295: a FAILED mc
    # run is the case the naive "no mc key is read" invariant misses, because
    # candidate_verdict's failed-run scan reads results.values() generically.
    absent = _survivor_entry()
    present = _survivor_entry(mc=_mc_ok_run())
    failed = _survivor_entry(mc={"status": "failed", "argv_tail": ["--json", "x"]})

    verdicts = {name: asug.candidate_verdict(e, _family_tests(e))
                for name, e in (("absent", absent), ("present", present),
                                ("failed", failed))}
    assert verdicts == {"absent": "survivor", "present": "survivor",
                        "failed": "survivor"}


def test_gate_relevant_results_drops_only_advisory_harnesses():
    entry = _survivor_entry(mc={"status": "failed"},
                            m5={"status": "ok", "data": {}})
    gate = asug.gate_relevant_results(entry)
    assert set(gate) == {"m1_noise", "m1", "m5"}
    assert "mc" not in gate
    # M5 stays gate-relevant: a failed M5 still means run_failed (pre-#1295).
    assert asug.candidate_verdict(
        _survivor_entry(m5={"status": "failed"}), []) == "run_failed"


def test_failed_gate_harness_still_yields_run_failed():
    entry = _survivor_entry(m1={"status": "failed"})
    assert asug.candidate_verdict(entry, []) == "run_failed"


def test_mc_only_failure_does_not_flip_the_process_exit_code():
    mc_failed = _survivor_entry(mc={"status": "failed"})
    assert asug.any_gate_failure([mc_failed]) is False
    gate_failed = _survivor_entry(m3={"status": "failed"})
    assert asug.any_gate_failure([gate_failed]) is True


def test_mc_failure_surfaces_as_a_limitation_not_a_verdict():
    entry = _survivor_entry(mc={"status": "failed"})
    assert asug.advisory_failures(entry) == ["mc"]
    assert asug.advisory_failures(_survivor_entry(mc=_mc_ok_run())) == []
    # a failed GATE harness is not an "advisory failure"
    assert asug.advisory_failures(_survivor_entry(m1={"status": "failed"})) == []


def test_mc_contributes_no_pvalue_and_leaves_bh_family_size_unchanged():
    without = asug.collect_family_pvalues([_survivor_entry()])
    with_mc = asug.collect_family_pvalues([_survivor_entry(mc=_mc_ok_run())])
    assert [t["p"] for t in without] == [t["p"] for t in with_mc]
    assert asug.apply_family_correction(with_mc, 0.05)["m"] == \
        asug.apply_family_correction(without, 0.05)["m"]
    assert all(t["harness"] != "mc" for t in with_mc)


# ---- mc argv tail ---------------------------------------------------------

def test_mc_argv_tail_threads_the_candidate_json_not_a_bare_strategy():
    tail = asug.mc_argv_tail("/t/c.json", "spot", ["is", "oos"],
                             ["BTC/USDT:1h"], 500, 7, {}, "/t/o.json")
    assert tail[:2] == ["--candidate-json", "/t/c.json"]
    assert "--strategy" not in tail          # fidelity: never the bare strategy
    assert "--windows" in tail and tail[tail.index("--windows") + 1] == "is,oos"
    assert tail[tail.index("--datasets") + 1] == "BTC/USDT:1h"
    assert tail[tail.index("--n-paths") + 1] == "500"
    assert tail[tail.index("--seed") + 1] == "7"


def test_mc_argv_tail_omits_datasets_when_none_and_threshold_when_default():
    tail = asug.mc_argv_tail("/t/c.json", "spot", ["is"], None, 10, 1, {}, "/o")
    assert "--datasets" not in tail
    assert "--kill-switch-pct" not in tail and "--config" not in tail


def test_mc_argv_tail_threshold_sources_are_exclusive():
    explicit = asug.mc_argv_tail("/c", "spot", ["is"], None, 10, 1,
                                 {"kill_switch_pct": 30}, "/o")
    assert explicit[explicit.index("--kill-switch-pct") + 1] == "30"
    assert "--config" not in explicit

    from_cfg = asug.mc_argv_tail("/c", "spot", ["is"], None, 10, 1,
                                 {"config": "/cfg.json", "strategy_id": "hl-x"},
                                 "/o")
    assert from_cfg[from_cfg.index("--config") + 1] == "/cfg.json"
    assert from_cfg[from_cfg.index("--strategy-id") + 1] == "hl-x"
    assert "--kill-switch-pct" not in from_cfg   # monte_carlo.py refuses both


# ---- mc spec block --------------------------------------------------------

def test_load_spec_rejects_both_mc_threshold_sources():
    with pytest.raises(ValueError, match="mutually exclusive"):
        asug.load_spec(_base_spec(mc={"kill_switch_pct": 30,
                                      "config": "c.json",
                                      "strategy_id": "x"}), _STUDY_DIR)


def test_load_spec_rejects_mc_config_without_strategy_id():
    with pytest.raises(ValueError, match="go together"):
        asug.load_spec(_base_spec(mc={"config": "c.json"}), _STUDY_DIR)
    with pytest.raises(ValueError, match="go together"):
        asug.load_spec(_base_spec(mc={"strategy_id": "x"}), _STUDY_DIR)


def test_load_spec_rejects_unknown_mc_key_and_bad_n_paths():
    with pytest.raises(ValueError, match="unknown mc keys"):
        asug.load_spec(_base_spec(mc={"schemes": ["permute"]}), _STUDY_DIR)
    with pytest.raises(ValueError, match="n_paths"):
        asug.load_spec(_base_spec(mc={"n_paths": 0}), _STUDY_DIR)


def test_load_spec_resolves_mc_config_against_the_spec_dir():
    spec = asug.load_spec(_base_spec(mc={"config": "cfg.json",
                                         "strategy_id": "hl-x"}), _STUDY_DIR)
    assert spec["mc"]["config"] == os.path.join(_STUDY_DIR, "cfg.json")


def test_mc_is_a_default_open_harness_but_never_an_m6_one():
    assert "mc" in asug.DEFAULT_HARNESSES and "mc" in asug.OPEN_HARNESSES
    spec = asug.load_spec(_base_spec(harnesses=None), _STUDY_DIR)
    entry = asug.expand_candidates(spec)[0]
    assert "mc" in entry["harnesses"]
    # explicit harness list without mc skips it cleanly
    spec = asug.load_spec(_base_spec(harnesses=["m1"]), _STUDY_DIR)
    assert "mc" not in asug.expand_candidates(spec)[0]["harnesses"]


# ---- extract_mc -----------------------------------------------------------

def _mc_payload():
    def leg(window, ds, p_ks, p95, p_down, status="ok"):
        return {"window": window, "dataset": ds, "status": status, "n_trades": 20,
                "schemes": [{"scheme": "permute", "p_dd_ge_kill_switch": p_ks,
                             "max_dd_pct_percentiles": {"p5": 1.0, "p50": 10.0,
                                                        "p95": p95},
                             "p_final_below_start": p_down}]}
    return {"legs": [leg("is", "BTC/USDT 1h", 0.10, 30.0, 0.20),
                     leg("oos", "BTC/USDT 1h", 0.30, 45.0, 0.25),
                     leg("oos", "ETH/USDT 4h", 0.55, 70.0, 0.40)]}


def test_extract_mc_keys_by_window_and_takes_the_worst_dataset():
    out = asug.extract_mc(_mc_payload())
    assert set(out) == {"is", "oos"}
    worst = out["oos"]["worst"]["permute"]
    # worst-case, not mean: the fragile ETH leg must not be averaged away
    assert worst == {"p_dd_ge_kill_switch": 0.55, "p95_max_dd": 70.0,
                     "p_final_below_start": 0.40}
    assert set(out["oos"]["per_dataset"]) == {"BTC/USDT 1h", "ETH/USDT 4h"}


def test_extract_mc_tolerates_no_data_legs_and_missing_percentiles():
    payload = {"legs": [
        {"window": "oos", "dataset": "BTC/USDT 1h", "status": "no_data",
         "n_trades": 0, "schemes": []},
        {"window": "oos", "dataset": "ETH/USDT 1h", "status": "ok", "n_trades": 3,
         "schemes": [{"scheme": "permute", "p_dd_ge_kill_switch": 0.2,
                      "max_dd_pct_percentiles": {"p50": 5.0},  # no p95 configured
                      "p_final_below_start": 0.1}]}]}
    out = asug.extract_mc(payload)
    worst = out["oos"]["worst"]["permute"]
    assert worst["p_dd_ge_kill_switch"] == 0.2
    assert worst["p95_max_dd"] is None      # never fabricated
    assert asug.extract_mc({}) == {}


# ---- report ---------------------------------------------------------------

def test_format_shortlist_labels_mc_advisory_and_prints_the_oos_worst_case():
    report = {
        "study": "t", "exploratory": False,
        "correction": {"method": "benjamini_hochberg", "alpha": 0.05, "m": 1,
                       "effective_threshold": 0.01, "bonferroni_threshold": 0.05,
                       "n_survivors": 1},
        "ranked": [{"key": "win", "verdict": "survivor", "limitations": [],
                    "results": {"mc": {"data": asug.extract_mc(_mc_payload())}}}],
    }
    text = asug.format_shortlist(report)
    assert "MC(adv,oos)" in text
    assert "p95DD 70.0%" in text and "pKS 0.550" in text
    assert "does not gate" in text
    assert "M3/M5/MC figures are UNCORRECTED CONTEXT" in text


def test_format_shortlist_omits_the_mc_segment_when_the_run_failed():
    report = {
        "study": "t", "exploratory": False,
        "correction": {"method": "benjamini_hochberg", "alpha": 0.05, "m": 1,
                       "effective_threshold": None, "bonferroni_threshold": None,
                       "n_survivors": 0},
        "ranked": [{"key": "win", "verdict": "survivor",
                    "limitations": ["mc_run_failed"],
                    "results": {"mc": {"status": "failed"}}}],
    }
    text = asug.format_shortlist(report)
    row = next(ln for ln in text.splitlines() if ln.strip().startswith("1 "))
    assert "MC(adv," not in row       # no column, rather than a fake 0
    assert "mc_run_failed" in row     # but the absence is visible
    assert "survivor" in row          # and the verdict is untouched


# ---- dry run --------------------------------------------------------------

def test_dry_run_prints_a_command_for_every_enabled_harness():
    spec = asug.load_spec(_base_spec(harnesses=None), _STUDY_DIR)
    spec["seed"], spec["resamples"], spec["datasets"] = 1066, 10, None
    entries = asug.expand_candidates(spec)
    cmds = asug._dry_run_commands(entries, spec, "/tmp/out")
    for harness in ("m1_noise", "m1", "m3", "m5", "mc"):
        assert any(asug.HARNESS_REL[harness] in c for c in cmds), \
            f"dry-run omits {harness} — it would spawn it anyway"


def test_dry_run_omits_the_mc_command_when_mc_is_not_enabled():
    spec = asug.load_spec(_base_spec(harnesses=["m1"]), _STUDY_DIR)
    spec["seed"], spec["resamples"], spec["datasets"] = 1066, 10, None
    cmds = asug._dry_run_commands(asug.expand_candidates(spec), spec, "/tmp/out")
    assert not any("monte_carlo.py" in c for c in cmds)

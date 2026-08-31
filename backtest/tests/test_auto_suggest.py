import copy
import json
import os

import pytest

import auto_suggest as asug
from regime_stats import benjamini_hochberg

_STUDY_DIR = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))),
                          "candidates", "squeeze_momentum_1198")


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
    bad = _base_spec(candidates=[{"key": "c", "candidate": {
        "name": "squeeze_momentum", "allowed_regimes": "trending_up"}}])
    with pytest.raises(ValueError, match="allowed_regimes"):
        asug.load_spec(bad, _STUDY_DIR)


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
    bad = _base_spec(candidates=[], m6={
        "incumbent_close": [{"name": "tiered_tp_atr", "params": {}}],
        "candidate_close_variants": [
            {"key": "v", "candidate_close": [{"name": "atr_stop", "params": {"atr_mult": 2}}]}]})
    with pytest.raises(ValueError, match="strategy_id"):
        asug.load_spec(bad, _STUDY_DIR)


def test_m6_per_variant_strategy_id_override_loads_without_m6_default():
    spec = asug.load_spec(_base_spec(candidates=[], m6={
        "incumbent_close": [{"name": "tiered_tp_atr", "params": {}}],
        "candidate_close_variants": [
            {"key": "v", "strategy_id": "squeeze_momentum",
             "candidate_close": [{"name": "atr_stop", "params": {"atr_mult": 2}}]}]},
    ), _STUDY_DIR)
    ab = [e for e in asug.expand_candidates(spec) if e["kind"] == "exit_ab"]
    assert len(ab) == 1 and ab[0]["candidate"]["strategy_id"] == "squeeze_momentum"


def test_m6_close_stack_specs_require_m6_level_strategy_id():
    bad = _base_spec(candidates=[], m6={
        "incumbent_close": [{"name": "tiered_tp_atr", "params": {}}],
        "candidate_close_variants": [],
        "close_stack_specs": [{"close": {"name": "atr_stop", "params": {"atr_mult": [2.0]}}}]})
    with pytest.raises(ValueError, match="close_stack_specs"):
        asug.load_spec(bad, _STUDY_DIR)


def test_m6_baseline_config_path_also_requires_strategy_id():
    bad = _base_spec(candidates=[], m6={
        "baseline_config": "cfg.json",
        "candidate_close_variants": [
            {"key": "v", "candidate_close": [{"name": "atr_stop", "params": {"atr_mult": 2}}]}]})
    with pytest.raises(ValueError, match="strategy_id"):
        asug.load_spec(bad, _STUDY_DIR)




def test_shipped_full_options_spec_loads_and_expands():
    with open(os.path.join(_STUDY_DIR, "suggest.json")) as fh:
        raw = json.load(fh)
    spec = asug.load_spec(raw, _STUDY_DIR)
    entries = asug.expand_candidates(spec)
    kinds = {e["kind"] for e in entries}
    assert kinds == {"open", "exit_ab"}
    ab = [e for e in entries if e["kind"] == "exit_ab"]
    assert all(e["precondition_errors"] == [] for e in ab)
    assert len({e["key"] for e in entries}) == len(entries)


def test_m5_params_limitation_flagged():
    spec = asug.load_spec(_base_spec(
        harnesses=["m5"],
        candidates=[{"key": "c", "candidate": {"name": "squeeze_momentum",
                                               "params": {"kc_mult": 2.0}}}]), _STUDY_DIR)
    entry = asug.expand_candidates(spec)[0]
    assert "m5_params_unaudited" in entry["limitations"]


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
    assert "--incumbent-close" not in tail


def test_m6_argv_tail_incumbent_close_path_omits_baseline():
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


def test_apply_correction_matches_direct_bh_and_reports_threshold():
    tests = [{"candidate_key": "a", "harness": "m6", "p": p, "effect_positive": True}
             for p in [0.001, 0.02, 0.04, 0.5]]
    corr = asug.apply_family_correction(copy.deepcopy(tests), alpha=0.05)
    mask = benjamini_hochberg([t["p"] for t in tests], 0.05)
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


def _one_test(p):
    return [{"candidate_key": "a", "harness": "m6", "p": p, "effect_positive": True}]


def test_bh_family_size_widens_denominator_and_is_stricter():
    pvals = [0.01, 0.04]
    assert benjamini_hochberg(pvals, 0.05) == [True, True]
    assert benjamini_hochberg(pvals, 0.05, family_size=10) == [False, False]
    assert (benjamini_hochberg(pvals, 0.05, family_size=2)
            == benjamini_hochberg(pvals, 0.05))


def test_bh_family_size_below_test_count_raises():
    with pytest.raises(ValueError, match="family_size"):
        benjamini_hochberg([0.01, 0.04], 0.05, family_size=1)


def test_apply_correction_family_size_reports_m_and_tests_run():
    tests = [{"candidate_key": "a", "harness": "m6", "p": p, "effect_positive": True}
             for p in [0.001, 0.02]]
    corr = asug.apply_family_correction(copy.deepcopy(tests), alpha=0.05,
                                        family_size=8)
    assert corr["m"] == 8
    assert corr["tests_run"] == 2
    assert corr["bonferroni_threshold"] == pytest.approx(0.05 / 8)
    strict = asug.apply_family_correction(copy.deepcopy(tests), alpha=0.05,
                                          family_size=8)
    loose = asug.apply_family_correction(copy.deepcopy(tests), alpha=0.05)
    assert strict["n_survivors"] <= loose["n_survivors"]


def test_apply_correction_family_size_below_produced_count_raises():
    tests = [{"candidate_key": "a", "harness": "m6", "p": p, "effect_positive": True}
             for p in [0.001, 0.02, 0.04]]
    with pytest.raises(ValueError, match="family_size"):
        asug.apply_family_correction(tests, alpha=0.05, family_size=2)


def test_load_spec_accepts_positive_family_size():
    spec = asug.load_spec(
        _base_spec(correction={"method": "benjamini_hochberg", "alpha": 0.05,
                               "family_size": 40}),
        _STUDY_DIR)
    assert spec["correction"]["family_size"] == 40


def test_load_spec_defaults_family_size_none():
    spec = asug.load_spec(_base_spec(), _STUDY_DIR)
    assert spec["correction"]["family_size"] is None


@pytest.mark.parametrize("bad", [0, -1, 1.5, True, "8"])
def test_load_spec_rejects_bad_family_size(bad):
    with pytest.raises(ValueError, match="family_size"):
        asug.load_spec(
            _base_spec(correction={"method": "benjamini_hochberg",
                                   "family_size": bad}),
            _STUDY_DIR)


def test_collect_family_pvalues_dedupes_noise_and_excludes_m3_m5():
    e1 = {"key": "a", "kind": "open", "noise_family_key": "K",
          "results": {"m1_noise": {"data": {"permutation_p": 0.01, "mean": 0.2}},
                      "m3": {"data": {"x": 1}}, "m5": {"data": {"salvage_verdict": "healthy"}}}}
    e2 = {"key": "b", "kind": "open", "noise_family_key": "K",
          "results": {"m1_noise": {"data": {"permutation_p": 0.01, "mean": 0.2}}}}
    e3 = {"key": "c", "kind": "exit_ab",
          "results": {"m6": {"data": {"oos": {"per_dataset": [
              {"dataset": "BTC 1h", "mean": 0.3, "p": 0.02}]}}}}}
    tests = asug.collect_family_pvalues([e1, e2, e3])
    harnesses = sorted(t["harness"] for t in tests)
    assert harnesses == ["m1_noise", "m6"]


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
    fam = "shared"
    m1_pass = {"is": {"verdict": "pass"}, "oos": {"verdict": "pass"}}
    noise = {"verdict": "distinguishable_positive", "mean": 0.3}
    siblings = [_open_entry(k, fam=fam, noise=noise, m1=m1_pass)
                for k in ("baseline", "adx_gate", "comp_gate")]
    tests = [_noise_test(fam, "baseline", 0.049, bh_pass=False)]
    verdicts = [asug.candidate_verdict(e, tests) for e in siblings]
    assert verdicts == ["positive_uncorrected_only"] * 3
    verdicts_rev = [asug.candidate_verdict(e, tests) for e in reversed(siblings)]
    assert set(verdicts_rev) == {"positive_uncorrected_only"}
    tests[0]["bh_pass"] = True
    assert [asug.candidate_verdict(e, tests) for e in siblings] == ["survivor"] * 3


def test_distinct_param_families_keep_independent_noise_tests():
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


def test_rank_survivors_first_failed_still_present():
    entries = [
        {"key": "loser", "verdict": "incumbent_stands", "results": {}},
        {"key": "win", "verdict": "survivor", "results": {}},
        {"key": "broke", "verdict": "run_failed", "results": {}},
    ]
    ranked = asug.rank_shortlist(entries)
    assert ranked[0]["key"] == "win"
    assert [e["key"] for e in ranked] == ["win", "loser", "broke"]
    assert any(e["verdict"] == "run_failed" for e in ranked)


def test_reproduction_command_uses_relative_harness_paths():
    entry = {"key": "x", "results": {
        "m1": {"argv_tail": ["--candidate-json", "/t/c.json", "--registry", "spot"]}}}
    cmds = asug.reproduction_command(entry)
    assert cmds and cmds[0].startswith("uv run --no-sync python backtest/eval_windows.py")
    assert "--candidate-json" in cmds[0]


def _survivor_entry(**results_over):
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
    assert asug.advisory_failures(_survivor_entry(m1={"status": "failed"})) == []


def test_mc_contributes_no_pvalue_and_leaves_bh_family_size_unchanged():
    without = asug.collect_family_pvalues([_survivor_entry()])
    with_mc = asug.collect_family_pvalues([_survivor_entry(mc=_mc_ok_run())])
    assert [t["p"] for t in without] == [t["p"] for t in with_mc]
    assert asug.apply_family_correction(with_mc, 0.05)["m"] == \
        asug.apply_family_correction(without, 0.05)["m"]
    assert all(t["harness"] != "mc" for t in with_mc)


def test_mc_argv_tail_threads_the_candidate_json_not_a_bare_strategy():
    tail = asug.mc_argv_tail("/t/c.json", "spot", ["is", "oos"],
                             ["BTC/USDT:1h"], 500, 7, {}, "/t/o.json")
    assert tail[:2] == ["--candidate-json", "/t/c.json"]
    assert "--strategy" not in tail
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
    assert "--kill-switch-pct" not in from_cfg


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


def test_mc_is_opt_in_not_a_default_harness():
    assert "mc" not in asug.DEFAULT_HARNESSES and "mc" in asug.OPEN_HARNESSES
    spec = asug.load_spec(_base_spec(harnesses=None), _STUDY_DIR)
    entry = asug.expand_candidates(spec)[0]
    assert "mc" not in entry["harnesses"]
    spec = asug.load_spec(_base_spec(harnesses=["m1", "mc"]), _STUDY_DIR)
    assert "mc" in asug.expand_candidates(spec)[0]["harnesses"]


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
    assert worst == {"p_dd_ge_kill_switch": 0.55, "p95_max_dd": 70.0,
                     "p_final_below_start": 0.40}
    assert set(out["oos"]["per_dataset"]) == {"BTC/USDT 1h", "ETH/USDT 4h"}


def test_extract_mc_tolerates_no_data_legs_and_missing_percentiles():
    payload = {"legs": [
        {"window": "oos", "dataset": "BTC/USDT 1h", "status": "no_data",
         "n_trades": 0, "schemes": []},
        {"window": "oos", "dataset": "ETH/USDT 1h", "status": "ok", "n_trades": 3,
         "schemes": [{"scheme": "permute", "p_dd_ge_kill_switch": 0.2,
                      "max_dd_pct_percentiles": {"p50": 5.0},
                      "p_final_below_start": 0.1}]}]}
    out = asug.extract_mc(payload)
    worst = out["oos"]["worst"]["permute"]
    assert worst["p_dd_ge_kill_switch"] == 0.2
    assert worst["p95_max_dd"] is None
    assert asug.extract_mc({}) == {}


def test_dry_run_prints_a_command_for_every_enabled_harness():
    spec = asug.load_spec(_base_spec(harnesses=["m1_noise", "m1", "m3", "m5", "mc"]),
                           _STUDY_DIR)
    spec["seed"], spec["resamples"], spec["datasets"] = 1066, 10, None
    entries = asug.expand_candidates(spec)
    cmds = asug._dry_run_commands(entries, spec, "/tmp/out")
    for harness in ("m1_noise", "m1", "m3", "m5", "mc"):
        assert any(asug.HARNESS_REL[harness] in c for c in cmds), \
            f"dry-run omits {harness} — it would spawn it anyway"


def test_dry_run_with_default_harnesses_omits_mc():
    spec = asug.load_spec(_base_spec(harnesses=None), _STUDY_DIR)
    spec["seed"], spec["resamples"], spec["datasets"] = 1066, 10, None
    entries = asug.expand_candidates(spec)
    cmds = asug._dry_run_commands(entries, spec, "/tmp/out")
    for harness in ("m1_noise", "m1", "m3", "m5"):
        assert any(asug.HARNESS_REL[harness] in c for c in cmds), \
            f"dry-run omits {harness} — it would spawn it anyway"
    assert not any("monte_carlo.py" in c for c in cmds), \
        "mc must not run when a spec doesn't opt into it (#1316)"


def _open_spec(harnesses):
    spec = asug.load_spec(_base_spec(harnesses=harnesses), _STUDY_DIR)
    spec["seed"], spec["resamples"], spec["datasets"] = 1066, 10, None
    return spec


def _capture_candidate(monkeypatch, harness_flag):
    seen = {}

    def fake_run_harness(harness, tail, out_json):
        path = tail[tail.index(harness_flag) + 1]
        with open(path) as fh:
            seen[harness] = json.load(fh)
        return {"harness": harness, "argv_tail": tail, "status": "failed"}

    monkeypatch.setattr(asug, "_run_harness", fake_run_harness)
    return seen


def test_candidate_json_is_rewritten_over_a_stale_file_from_a_prior_run(monkeypatch, tmp_path):
    spec = _open_spec(["m1"])
    entry = asug.expand_candidates(spec)[0]
    stale = tmp_path / f"{entry['key']}.candidate.json"
    stale.write_text(json.dumps({"name": "STALE_STRATEGY",
                                 "allowed_regimes": ["stale_gate"]}))

    seen = _capture_candidate(monkeypatch, "--candidate-json")
    asug.run_open_entry(entry, spec, str(tmp_path), {}, {})
    assert seen["m1"]["name"] == "squeeze_momentum"
    assert "allowed_regimes" not in seen["m1"]
    assert json.loads(stale.read_text())["name"] == "squeeze_momentum"


def test_mc_also_reads_a_freshly_written_candidate_not_a_stale_one(monkeypatch, tmp_path):
    spec = _open_spec(["mc"])
    entry = asug.expand_candidates(spec)[0]
    (tmp_path / f"{entry['key']}.candidate.json").write_text(
        json.dumps({"name": "STALE_STRATEGY"}))

    seen = _capture_candidate(monkeypatch, "--candidate-json")
    asug.run_open_entry(entry, spec, str(tmp_path), {}, {})
    assert seen["mc"]["name"] == "squeeze_momentum"


def test_candidate_json_is_written_once_per_run_and_shared_by_m1_and_mc(monkeypatch, tmp_path):
    spec = _open_spec(["m1", "mc"])
    entry = asug.expand_candidates(spec)[0]
    paths, writes = [], []

    real_dump = json.dump

    def counting_dump(obj, fh, **kw):
        if getattr(fh, "name", "").endswith(".candidate.json"):
            writes.append(fh.name)
        return real_dump(obj, fh, **kw)

    monkeypatch.setattr(asug.json, "dump", counting_dump)
    monkeypatch.setattr(asug, "_run_harness", lambda h, tail, out: (
        paths.append(tail[tail.index("--candidate-json") + 1])
        or {"harness": h, "argv_tail": tail, "status": "failed"}))

    asug.run_open_entry(entry, spec, str(tmp_path), {}, {})
    assert len(paths) == 2 and paths[0] == paths[1]
    assert len(writes) == 1


def _mc_data(**windows):
    out = {}
    for w, stats in windows.items():
        out[w] = {"per_dataset": {},
                  "worst": {} if stats is None else {"permute": stats}}
    return out


_SCORED = {"p_dd_ge_kill_switch": 0.30, "p95_max_dd": 40.0,
           "p_final_below_start": 0.20}
_SCORED_OOS = {"p_dd_ge_kill_switch": 0.55, "p95_max_dd": 70.0,
               "p_final_below_start": 0.40}
_NO_TRADES = {"p_dd_ge_kill_switch": None, "p95_max_dd": None,
              "p_final_below_start": None}


def test_mc_column_falls_back_to_a_scored_window_when_oos_is_all_no_data():
    mc = _mc_data(**{"is": _SCORED, "oos": None})
    assert asug._mc_column_window(mc) == "is"
    seg = asug._mc_segment(mc)
    assert "MC(adv,is)=p95DD 40.0%" in seg


def test_mc_column_still_prefers_oos_when_both_windows_are_scored():
    mc = _mc_data(**{"is": _SCORED, "oos": _SCORED_OOS})
    assert asug._mc_column_window(mc) == "oos"
    assert "MC(adv,oos)=p95DD 70.0%" in asug._mc_segment(mc)


def test_mc_column_prefers_a_scored_window_over_a_zero_trade_one():
    mc = _mc_data(**{"is": _SCORED, "oos": _NO_TRADES})
    assert asug._mc_column_window(mc) == "is"
    assert "MC(adv,is)" in asug._mc_segment(mc)


def test_mc_column_absent_when_no_window_was_resampled_at_all():
    assert asug._mc_column_window(_mc_data(**{"is": None, "oos": None})) is None
    assert asug._mc_segment(_mc_data(**{"is": None, "oos": None})) == ""
    assert asug._mc_segment({}) == ""

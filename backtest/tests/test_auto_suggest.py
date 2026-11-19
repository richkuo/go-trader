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


def test_committed_1198_spec_loads_and_expands():
    with open(os.path.join(_STUDY_DIR, "suggest.json")) as fh:
        raw = json.load(fh)
    spec = asug.load_spec(raw, _STUDY_DIR)
    case = "test_load_spec_accepts_committed_1198_shape"
    assert spec["registry"] == "spot", case
    assert [c["key"] for c in spec["candidates"]] == ["baseline", "adx_not_down",
                                                      "comp_up_family"], case
    assert spec["candidates"][1]["candidate"]["allowed_regimes"] == ["trending_up", "ranging"], case

    case = "test_shipped_full_options_spec_loads_and_expands"
    entries = asug.expand_candidates(spec)
    kinds = {e["kind"] for e in entries}
    assert kinds == {"open", "exit_ab"}, case
    ab = {e["key"]: e for e in entries if e["kind"] == "exit_ab"}
    atr_trail = ab["m6.atr_trail"]["candidate"]
    assert atr_trail["candidate_close"] == [], case
    assert atr_trail["candidate_stops"] == {"trailing_stop_atr_mult": 3.0}, case
    assert atr_trail["candidate_stop_only"] is True, case
    assert all(e["precondition_errors"] == [] for e in ab.values()), case
    assert len({e["key"] for e in entries}) == len(entries), case


_M6_VARIANT = {"key": "v",
               "candidate_close": [{"name": "atr_stop",
                                    "params": {"atr_mult": 2}}]}


_LOAD_SPEC_REJECTS_INVALID_SPEC_CASES = [
    (dict(harnesses=["m1", "m9"]), "unknown harnesses"),
    (dict(windows=["is", "nope"]), "unknown windows"),
    (dict(registry="options"), "registry must be"),
    (dict(correction={"method": "bonferroni"}), "benjamini_hochberg"),
    (dict(candidates=[{"key": "c", "candidate": {
        "name": "squeeze_momentum", "allowed_regimes": "trending_up"}}]),
     "allowed_regimes"),
    (dict(correction={"method": "benjamini_hochberg", "family_size": 0}),
     "family_size"),
    (dict(correction={"method": "benjamini_hochberg", "family_size": -1}),
     "family_size"),
    (dict(correction={"method": "benjamini_hochberg", "family_size": 1.5}),
     "family_size"),
    (dict(correction={"method": "benjamini_hochberg", "family_size": True}),
     "family_size"),
    (dict(correction={"method": "benjamini_hochberg", "family_size": "8"}),
     "family_size"),
    (dict(candidates=[], m6={"strategy_id": "s", "baseline_config": "cfg.json",
                             "incumbent_close": [{"name": "tiered_tp_atr"}],
                             "candidate_close_variants": []}), "EXACTLY one"),
    (dict(candidates=[], m6={"strategy_id": "s",
                             "candidate_close_variants": []}), "EXACTLY one"),
    (dict(candidates=[], m6={"incumbent_close": [{"name": "tiered_tp_atr",
                                                  "params": {}}],
                             "candidate_close_variants": [_M6_VARIANT]}),
     "strategy_id"),
    (dict(candidates=[], m6={"baseline_config": "cfg.json",
                             "candidate_close_variants": [_M6_VARIANT]}),
     "strategy_id"),
    (dict(candidates=[], m6={
        "incumbent_close": [{"name": "tiered_tp_atr", "params": {}}],
        "candidate_close_variants": [],
        "close_stack_specs": [{"close": {"name": "atr_stop",
                                         "params": {"atr_mult": [2.0]}}}]}),
     "close_stack_specs"),
    (dict(mc={"kill_switch_pct": 30, "config": "c.json", "strategy_id": "x"}),
     "mutually exclusive"),
    (dict(mc={"config": "c.json"}), "go together"),
    (dict(mc={"strategy_id": "x"}), "go together"),
    (dict(mc={"schemes": ["permute"]}), "unknown mc keys"),
    (dict(mc={"n_paths": 0}), "n_paths"),
]


def test_load_spec_rejects_invalid_spec(subtests):
    for i, (overrides, match) in enumerate(_LOAD_SPEC_REJECTS_INVALID_SPEC_CASES):
        with subtests.test(case=i, overrides=overrides, match=match):
            with pytest.raises(ValueError, match=match):
                asug.load_spec(_base_spec(**overrides), _STUDY_DIR)


def test_expand_rejects_duplicate_keys(subtests):
    with subtests.test(case="test_expand_rejects_duplicate_keys"):
        spec = asug.load_spec(_base_spec(candidates=[
            {"key": "dup", "candidate": {"name": "squeeze_momentum"}},
            {"key": "dup", "candidate": {"name": "squeeze_momentum"}},
        ]), _STUDY_DIR)
        with pytest.raises(ValueError, match="duplicate candidate key"):
            asug.expand_candidates(spec)

    with subtests.test(case="test_expand_sweep_requires_base"):
        spec = asug.load_spec(_base_spec(candidates=[], sweep={"kc_mult": [1.3]}), _STUDY_DIR)
        with pytest.raises(ValueError, match="require a 'base'"):
            asug.expand_candidates(spec)


def test_non_replayable_m6_close_excluded(subtests):
    with subtests.test(case="test_non_replayable_m6_close_excluded"):
        spec = asug.load_spec(_base_spec(candidates=[], base=None, m6={
            "baseline_config": "cfg.json", "strategy_id": "s",
            "candidate_close_variants": [
                {"key": "bad", "candidate_close": [{"name": "tiered_tp_atr_live_regime_dynamic"}]},
                {"key": "good", "candidate_close": [{"name": "atr_stop", "params": {"atr_mult": 2}}]},
            ]}), _STUDY_DIR)
        entries = {e["key"]: e for e in asug.expand_candidates(spec)}
        assert entries["m6.bad"]["precondition_errors"] == ["close_capability_refused:LIVE_ONLY_CLOSE"]
        assert entries["m6.good"]["precondition_errors"] == []

    with subtests.test(case="test_m6_incumbent_close_only_spec_loads"):
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

    with subtests.test(case="test_m6_per_variant_strategy_id_override_loads_without_m6_default"):
        spec = asug.load_spec(_base_spec(candidates=[], m6={
            "incumbent_close": [{"name": "tiered_tp_atr", "params": {}}],
            "candidate_close_variants": [
                {"key": "v", "strategy_id": "squeeze_momentum",
                 "candidate_close": [{"name": "atr_stop", "params": {"atr_mult": 2}}]}]},
        ), _STUDY_DIR)
        ab = [e for e in asug.expand_candidates(spec) if e["kind"] == "exit_ab"]
        assert len(ab) == 1 and ab[0]["candidate"]["strategy_id"] == "squeeze_momentum"

    with subtests.test(case="test_close_stack_specs_round_trip_through_optimizer_grid"):
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


def _m6_payload(is_rows, oos_rows):
    def _mk(rows):
        return [{"dataset": ds, "per_regime": {"all": {
            "n": n, "paired_delta": {"mean": mean, "signed_rank": {"p_value": p}}}}}
            for ds, mean, n, p in rows]
    return {"results": {"is": _mk(is_rows), "oos": _mk(oos_rows)}}


def test_apply_correction_matches_direct_bh_and_handles_empty_and_family_size(subtests):
    with subtests.test("test_apply_correction_matches_direct_bh_and_handles_empty_and_family_size"):
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

        case = "test_correction_empty_family"
        corr = asug.apply_family_correction([], alpha=0.05)
        assert corr["m"] == 0, case
        assert corr["effective_threshold"] is None, case
        assert corr["n_survivors"] == 0, case

        case = "test_apply_correction_family_size_reports_m_and_tests_run"
        tests = [{"candidate_key": "a", "harness": "m6", "p": p, "effect_positive": True}
                 for p in [0.001, 0.02]]
        corr = asug.apply_family_correction(copy.deepcopy(tests), alpha=0.05,
                                            family_size=8)
        assert corr["m"] == 8, case
        assert corr["tests_run"] == 2, case
        assert corr["bonferroni_threshold"] == pytest.approx(0.05 / 8), case
        strict = asug.apply_family_correction(copy.deepcopy(tests), alpha=0.05,
                                              family_size=8)
        loose = asug.apply_family_correction(copy.deepcopy(tests), alpha=0.05)
        assert strict["n_survivors"] <= loose["n_survivors"], case
    with subtests.test("test_apply_correction_family_size_below_produced_count_raises"):
        tests = [{"candidate_key": "a", "harness": "m6", "p": p, "effect_positive": True}
                 for p in [0.001, 0.02, 0.04]]
        with pytest.raises(ValueError, match="family_size"):
            asug.apply_family_correction(tests, alpha=0.05, family_size=2)


def test_bh_family_size_widens_denominator_and_is_stricter(subtests):
    with subtests.test("test_bh_family_size_widens_denominator_and_is_stricter"):
        pvals = [0.01, 0.04]
        assert benjamini_hochberg(pvals, 0.05) == [True, True]
        assert benjamini_hochberg(pvals, 0.05, family_size=10) == [False, False]
        assert (benjamini_hochberg(pvals, 0.05, family_size=2)
                == benjamini_hochberg(pvals, 0.05))
    with subtests.test("test_bh_family_size_below_test_count_raises"):
        with pytest.raises(ValueError, match="family_size"):
            benjamini_hochberg([0.01, 0.04], 0.05, family_size=1)


def test_load_spec_accepts_positive_family_size():
    spec = asug.load_spec(
        _base_spec(correction={"method": "benjamini_hochberg", "alpha": 0.05,
                               "family_size": 40}),
        _STUDY_DIR)
    assert spec["correction"]["family_size"] == 40


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


def test_verdict_failure_exclusion_and_open_entry_paths():
    e = {"key": "x", "kind": "open", "precondition_errors": [],
         "results": {"m1": {"status": "failed"}}}
    assert asug.candidate_verdict(e, []) == "run_failed", "test_verdict_run_failed_never_survivor"

    e = {"key": "x", "kind": "exit_ab",
         "precondition_errors": ["excluded_not_replayable"], "results": {}}
    assert asug.candidate_verdict(e, []) == "excluded_not_replayable", \
        "test_verdict_excluded_not_replayable"

    e = _open_entry("x", noise={"verdict": "no_positive_edge", "mean": -0.1},
                    m1={"is": {"verdict": "pass"}, "oos": {"verdict": "pass"}})
    assert asug.candidate_verdict(e, []) == "noise_gate_blocked", \
        "test_verdict_noise_gate_blocks_before_selectivity"

    case = "test_verdict_open_survivor_requires_bh_survival"
    e = _open_entry("x", fam="F", noise={"verdict": "distinguishable_positive", "mean": 0.3},
                    m1={"is": {"verdict": "pass"}, "oos": {"verdict": "pass"}})
    tests = [_noise_test("F", "x", 0.001, True)]
    assert asug.candidate_verdict(e, tests) == "survivor", case
    tests[0]["bh_pass"] = False
    assert asug.candidate_verdict(e, tests) == "positive_uncorrected_only", case

    e = _open_entry("x", fam="F", noise={"verdict": "distinguishable_positive", "mean": 0.3},
                    m1={"is": {"verdict": "pass"}, "oos": {"verdict": "fail"}})
    tests = [_noise_test("F", "x", 0.001, True)]
    assert asug.candidate_verdict(e, tests) == "incumbent_stands", \
        "test_verdict_open_m1_fail_is_incumbent_stands"


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


def test_extract_mc_worst_dataset_per_window_and_tolerates_partial_legs():
    out = asug.extract_mc(_mc_payload())
    assert set(out) == {"is", "oos"}
    worst = out["oos"]["worst"]["permute"]
    assert worst == {"p_dd_ge_kill_switch": 0.55, "p95_max_dd": 70.0,
                     "p_final_below_start": 0.40}
    assert set(out["oos"]["per_dataset"]) == {"BTC/USDT 1h", "ETH/USDT 4h"}

    case = "test_extract_mc_tolerates_no_data_legs_and_missing_percentiles"
    payload = {"legs": [
        {"window": "oos", "dataset": "BTC/USDT 1h", "status": "no_data",
         "n_trades": 0, "schemes": []},
        {"window": "oos", "dataset": "ETH/USDT 1h", "status": "ok", "n_trades": 3,
         "schemes": [{"scheme": "permute", "p_dd_ge_kill_switch": 0.2,
                      "max_dd_pct_percentiles": {"p50": 5.0},
                      "p_final_below_start": 0.1}]}]}
    out = asug.extract_mc(payload)
    worst = out["oos"]["worst"]["permute"]
    assert worst["p_dd_ge_kill_switch"] == 0.2, case
    assert worst["p95_max_dd"] is None, case
    assert asug.extract_mc({}) == {}, case


def test_dry_run_prints_every_enabled_harness_and_default_omits_mc():
    spec = asug.load_spec(_base_spec(harnesses=["m1_noise", "m1", "m3", "m5", "mc"]),
                           _STUDY_DIR)
    spec["seed"], spec["resamples"], spec["datasets"] = 1066, 10, None
    entries = asug.expand_candidates(spec)
    cmds = asug._dry_run_commands(entries, spec, "/tmp/out")
    for harness in ("m1_noise", "m1", "m3", "m5", "mc"):
        assert any(asug.HARNESS_REL[harness] in c for c in cmds), \
            f"dry-run omits {harness} — it would spawn it anyway"

    spec = asug.load_spec(_base_spec(harnesses=None), _STUDY_DIR)
    spec["seed"], spec["resamples"], spec["datasets"] = 1066, 10, None
    entries = asug.expand_candidates(spec)
    cmds = asug._dry_run_commands(entries, spec, "/tmp/out")
    for harness in ("m1_noise", "m1", "m3", "m5"):
        assert any(asug.HARNESS_REL[harness] in c for c in cmds), \
            f"test_dry_run_with_default_harnesses_omits_mc: dry-run omits {harness} — it would spawn it anyway"
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


def test_candidate_json_is_rewritten_over_a_stale_file_from_a_prior_run(monkeypatch, tmp_path, subtests):
    for harness in ("m1", "mc"):
        with subtests.test(harness=harness):
            out_dir = tmp_path / harness
            out_dir.mkdir()
            spec = _open_spec([harness])
            entry = asug.expand_candidates(spec)[0]
            stale = out_dir / f"{entry['key']}.candidate.json"
            stale.write_text(json.dumps({"name": "STALE_STRATEGY",
                                         "allowed_regimes": ["stale_gate"]}))

            seen = _capture_candidate(monkeypatch, "--candidate-json")
            asug.run_open_entry(entry, spec, str(out_dir), {}, {})
            assert seen[harness]["name"] == "squeeze_momentum"
            assert "allowed_regimes" not in seen[harness]
            assert json.loads(stale.read_text())["name"] == "squeeze_momentum"

import json
import os
import sys
from datetime import datetime, timezone

import numpy as np
import pandas as pd
import pytest

sys.path.insert(0, os.path.abspath(
    os.path.join(os.path.dirname(__file__), "..", "research")))

import regime_1076_certify as certify_mod
import regime_1076_directional_premise as premise_mod

GEN_AT = datetime(2026, 6, 19, tzinfo=timezone.utc)

FULL_UNIVERSE = dict(
    symbols=premise_mod.DEFAULT_SYMBOLS,
    timeframes=premise_mod.DEFAULT_TIMEFRAMES,
    windows=premise_mod.DEFAULT_WINDOWS,
    classifiers=premise_mod.DEFAULT_CLASSIFIERS,
    horizons=premise_mod.DEFAULT_HORIZONS,
)


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


# --- what lands in the artifact scheduler/regime_directional_certification.go
#     reads at runtime ---------------------------------------------------------


def test_a_surviving_aligned_held_out_row_is_certified():
    art = certify_mod.certify([_row()], generated_at=GEN_AT)
    assert len(art["certified"]) == 1
    e = art["certified"][0]
    assert (e["asset"], e["timeframe"], e["classifier"]) == ("BTC", "1h",
                                                             "composite")
    assert e["states"] == {"trending_up": "long"}
    assert e["expires_at"] > e["generated_at"]


@pytest.mark.parametrize("rows", [
    pytest.param([], id="no_rows"),
    pytest.param([_row(window="2024")], id="historical_window"),
    pytest.param([_row(sign_aligned=False)], id="wrong_signed"),
    pytest.param([_row(p=0.9)], id="not_significant"),
])
def test_nothing_else_is_certified(rows):
    art = certify_mod.certify(rows, generated_at=GEN_AT)
    assert art["schema_version"] == 1
    assert art["certified"] == []


def test_composite_sublabel_maps_to_canonical():
    art = certify_mod.certify(
        [_row(state="trending_down_strong", policy_dir=-1)], generated_at=GEN_AT)
    assert art["certified"][0]["states"] == {"trending_down": "short"}


def test_the_artifact_carries_the_schema_the_scheduler_parses():
    sources = {"HYPE/USDC:USDC": "hyperliquid", "BTC/USDT": "binanceus"}
    art = certify_mod.certify(
        [_row()], generated_at=GEN_AT, universe=FULL_UNIVERSE,
        data_sources=sources, n_perm=30000)
    assert set(art) == {"schema_version", "generated_at", "generator",
                        "source_evidence", "criteria", "default_ttl_days",
                        "certified"}
    assert art["schema_version"] == 1
    c = art["criteria"]
    assert c["data_sources"] == sources
    assert c["n_perm"] == 30000
    assert c["permutation_p_floor"] == pytest.approx(1 / 30001)
    assert c["universe"]["symbols"] == list(premise_mod.DEFAULT_SYMBOLS)
    assert c["universe"]["horizons"] == list(premise_mod.DEFAULT_HORIZONS)


# --- refusals that keep an unearned certification out of the repo artifact ---


def test_family_is_superset_true_for_the_default_universe():
    assert certify_mod.family_is_superset(**FULL_UNIVERSE)


@pytest.mark.parametrize("axis", ["symbols", "timeframes", "windows",
                                  "classifiers", "horizons"])
def test_family_is_superset_false_when_any_axis_is_narrowed(axis):
    kwargs = dict(FULL_UNIVERSE)
    kwargs[axis] = tuple(kwargs[axis])[:-1]
    assert not certify_mod.family_is_superset(**kwargs)


def test_narrowed_run_refuses_to_write_the_repo_artifact():
    with pytest.raises(SystemExit) as exc:
        certify_mod.main(["--symbols", "ETH/USDT", "--timeframes", "1h",
                          "--classifiers", "composite"])
    assert "narrowed family" in str(exc.value)


def test_narrowed_run_refuses_when_only_the_report_targets_the_repo():
    with pytest.raises(SystemExit) as exc:
        certify_mod.main(["--symbols", "BTC/USDT", "--timeframes", "1h",
                          "--classifiers", "composite",
                          "--out", "/tmp/research_cert.json"])
    assert "narrowed family" in str(exc.value)
    assert "regime_1443_run_report.json" in str(exc.value)


def test_baseline_source_violations_reports_every_repointed_default():
    sources = {"BTC/USDT": "kraken", "ETH/USDT": "coinbase",
               "SOL/USDT": "binanceus"}
    assert certify_mod.baseline_source_violations(sources, "binanceus") == {
        "BTC/USDT": "kraken", "ETH/USDT": "coinbase"}


def test_baseline_source_violations_ignores_added_symbols():
    sources = {"BTC/USDT": "binanceus", "ETH/USDT": "binanceus",
               "SOL/USDT": "binanceus", "HYPE/USDC:USDC": "hyperliquid"}
    assert certify_mod.baseline_source_violations(sources, "binanceus") == {}


def test_repointed_baseline_refuses_to_write_the_repo_artifact():
    with pytest.raises(SystemExit) as exc:
        certify_mod.main(["--symbols", "BTC/USDT@kraken,ETH/USDT,SOL/USDT"])
    assert "repointed baseline" in str(exc.value)
    assert "BTC/USDT@kraken" in str(exc.value)


def test_colliding_assets_refuse_before_any_data_access():
    with pytest.raises(SystemExit) as exc:
        certify_mod.main(["--symbols",
                          "BTC/USDT,ETH/USDT,SOL/USDT,BTC/USDC:USDC@hyperliquid"])
    assert "normalize to the same certification asset" in str(exc.value)


def test_zero_coverage_run_refuses_and_leaves_the_artifact_untouched(
        tmp_path, monkeypatch):
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


def test_n_perm_default_resolves_the_full_screen():
    n_perm = certify_mod.build_parser().parse_args([]).n_perm
    assert certify_mod.permutation_p_floor(n_perm) <= 0.05 / 1319


def test_allow_degenerate_run_is_separate_from_allow_narrowed_family(
        tmp_path, monkeypatch):
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


# --- the refusals must not block a research run written elsewhere ------------


def test_narrowed_run_may_write_a_research_artifact_elsewhere(
        tmp_path, monkeypatch):
    monkeypatch.setattr(premise_mod, "run",
                        lambda *a, **k: [_row(symbol="ETH/USDT")])
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
    assert json.loads(report.read_text())["cell_verdicts"][0]["verdict"] == (
        certify_mod.VERDICT_CERTIFIED)


def test_repo_artifact_run_over_the_full_family_is_allowed(
        tmp_path, monkeypatch):
    monkeypatch.setattr(premise_mod, "run", lambda *a, **k: [_row(p=0.9)])
    art, rep = tmp_path / "art.json", tmp_path / "rep.json"
    monkeypatch.setattr(certify_mod, "DEFAULT_ARTIFACT", str(art))
    monkeypatch.setattr(certify_mod, "DEFAULT_RUN_REPORT", str(rep))
    rc = certify_mod.main(["--symbols", ",".join(premise_mod.DEFAULT_SYMBOLS)])
    assert rc == 0
    assert art.exists() and rep.exists()
    assert json.loads(art.read_text())["certified"] == []


# --- the producer feeding that artifact: premise functions certify() calls for
#     real. docs/backtesting-registry.md marks regime_1076_directional_premise
#     "the screen re-run by the #1085 producer", so the window clip that defines
#     held-out data and the source resolution behind the repointed-baseline
#     refusal are runtime-gating, not report-only. -------------------------------


def _ohlcv(start, periods, freq="1h"):
    rng = np.random.default_rng(7)
    idx = pd.date_range(start, periods=periods, freq=freq)
    close = 100.0 * np.exp(np.cumsum(rng.normal(0, 0.002, periods)))
    return pd.DataFrame({"open": close, "high": close * 1.001,
                         "low": close * 0.999, "close": close,
                         "volume": 1.0}, index=idx)


@pytest.mark.parametrize("window", premise_mod.HELD_OUT_FORWARD)
def test_load_clips_a_wider_fetch_to_the_held_out_window(window, monkeypatch):
    start, end = premise_mod.WINDOWS[window]
    full = _ohlcv(pd.Timestamp(start) - pd.Timedelta(days=120), 24 * 400)
    monkeypatch.setattr(premise_mod, "load_cached_data",
                        lambda *a, **k: full.copy())

    d = premise_mod._load("BTC/USDT", "1h", window, "composite",
                          dict(premise_mod._DEFAULT_COMPOSITE_THRESHOLDS))

    in_window = (full.index >= pd.Timestamp(start))
    if end is not None:
        in_window &= (full.index <= pd.Timestamp(end))
    n_in = int(in_window.sum())
    assert 0 < n_in < len(full)
    assert d is not None
    assert len(d["close"]) == n_in


def test_clip_window_is_inclusive_of_both_bounds_and_drops_the_rest():
    start, end = premise_mod.WINDOWS["is"]
    out = premise_mod._clip_window(
        _ohlcv(pd.Timestamp(start) - pd.Timedelta(days=30), 24 * 400),
        start, end)
    assert out.index.min() >= pd.Timestamp(start)
    assert out.index.max() <= pd.Timestamp(end)
    assert len(out) > 0


def test_clip_window_refuses_a_frame_with_no_datetime_index():
    with pytest.raises(ValueError):
        premise_mod._clip_window(pd.DataFrame({"close": [1.0, 2.0]}),
                                 *premise_mod.WINDOWS["is"])


def test_an_added_symbol_on_another_venue_resolves_and_is_not_repointed(
        tmp_path, monkeypatch):
    monkeypatch.setattr(premise_mod, "run", lambda *a, **k: [_row(p=0.9)])
    art, rep = tmp_path / "art.json", tmp_path / "rep.json"
    monkeypatch.setattr(certify_mod, "DEFAULT_ARTIFACT", str(art))
    monkeypatch.setattr(certify_mod, "DEFAULT_RUN_REPORT", str(rep))
    rc = certify_mod.main([
        "--symbols",
        ",".join(premise_mod.DEFAULT_SYMBOLS) + ",HYPE/USDC:USDC@hyperliquid"])
    assert rc == 0
    sources = json.loads(art.read_text())["criteria"]["data_sources"]
    assert sources["HYPE/USDC:USDC"] == "hyperliquid"
    assert all(sources[s] == premise_mod.PLATFORM
               for s in premise_mod.DEFAULT_SYMBOLS)


def test_each_symbol_is_fetched_from_its_own_mapped_venue(monkeypatch):
    seen = []

    def recorder(symbol, timeframe, exchange_id=None, start_date=None,
                 end_date=None):
        seen.append((symbol, exchange_id))
        return _ohlcv(start_date, 24 * 60)

    monkeypatch.setattr(premise_mod, "load_cached_data", recorder)
    th = dict(premise_mod._DEFAULT_COMPOSITE_THRESHOLDS)

    premise_mod._load("BTC/USDT", "1h", "is", "composite", th)
    premise_mod._load("HYPE/USDC:USDC", "1h", "is", "composite", th,
                      exchange="hyperliquid")
    assert seen == [("BTC/USDT", premise_mod.PLATFORM),
                    ("HYPE/USDC:USDC", "hyperliquid")]

    seen.clear()
    specs = premise_mod.parse_symbols_arg(
        "BTC/USDT,HYPE/USDC:USDC@hyperliquid")
    rows = premise_mod.run(specs, ("1h",), ("is",), (4,), ("composite",),
                           th, 1, 0)
    assert dict(seen) == {"BTC/USDT": premise_mod.PLATFORM,
                          "HYPE/USDC:USDC": "hyperliquid"}
    assert rows
    assert {r["symbol"]: r["source"] for r in rows} == {
        "BTC/USDT": premise_mod.PLATFORM,
        "HYPE/USDC:USDC": "hyperliquid"}
    assert (certify_mod.baseline_source_violations(
        premise_mod.resolve_data_sources(specs), premise_mod.PLATFORM) == {})

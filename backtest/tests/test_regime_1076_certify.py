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


# --- refusals that keep an unearned certification out of the repo artifact ---


def test_family_is_superset_only_for_the_full_default_universe():
    assert certify_mod.family_is_superset(**FULL_UNIVERSE), (
        "test_family_is_superset_true_for_the_default_universe")
    for axis in ["symbols", "timeframes", "windows", "classifiers", "horizons"]:
        kwargs = dict(FULL_UNIVERSE)
        kwargs[axis] = tuple(kwargs[axis])[:-1]
        assert not certify_mod.family_is_superset(**kwargs), (
            f"test_family_is_superset_false_when_any_axis_is_narrowed[{axis}]")


def test_narrowed_run_refuses_to_write_the_repo_artifact(subtests):
    cases = [
        ("narrowed_run", ["--symbols", "ETH/USDT", "--timeframes", "1h",
                          "--classifiers", "composite"],
         ["narrowed family"]),
        ("narrowed_run_report_targets_the_repo",
         ["--symbols", "BTC/USDT", "--timeframes", "1h",
          "--classifiers", "composite", "--out", "/tmp/research_cert.json"],
         ["narrowed family", "regime_1443_run_report.json"]),
        ("repointed_baseline", ["--symbols", "BTC/USDT@kraken,ETH/USDT,SOL/USDT"],
         ["repointed baseline", "BTC/USDT@kraken"]),
        ("colliding_assets",
         ["--symbols", "BTC/USDT,ETH/USDT,SOL/USDT,BTC/USDC:USDC@hyperliquid"],
         ["normalize to the same certification asset"]),
    ]
    for case_id, argv, needles in cases:
        with subtests.test(case=case_id):
            with pytest.raises(SystemExit) as exc:
                certify_mod.main(argv)
            for needle in needles:
                assert needle in str(exc.value)


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


# --- the refusals must not block a research run written elsewhere ------------


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

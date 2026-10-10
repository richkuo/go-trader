import gzip
import importlib.util
import json
import os
import shutil

import pandas as pd
import pytest

import offline_manifest as om

_REPO = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
STUDY_DIR = os.path.join(_REPO, "backtest", "candidates", "money_flow_index_reversal_1658")
HOUR_MS = 3_600_000


def _load_driver():
    spec = importlib.util.spec_from_file_location("_mfi_1658_run_study", os.path.join(STUDY_DIR, "run_study.py"))
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


DRIVER = _load_driver()


def _copy_study(tmp_path):
    dst = tmp_path / "study"
    shutil.copytree(os.path.join(STUDY_DIR, "data"), dst / "data")
    for name in ("study_manifest.json", "study_manifest_fee_x2.json"):
        shutil.copy(os.path.join(STUDY_DIR, name), dst / name)
    return dst


def _rewrite(study, rel, mutate):
    path = study / rel
    with gzip.open(path, "rt") as fh:
        df = pd.read_csv(fh)
    df = mutate(df)
    with gzip.open(path, "wt") as fh:
        df.to_csv(fh, index=False)
    digest = om.sha256_file(str(path))
    for name in ("study_manifest.json", "study_manifest_fee_x2.json"):
        manifest_path = study / name
        raw = json.loads(manifest_path.read_text())
        for ds in raw["datasets"]:
            for key in ("candles", "funding"):
                if ds.get(key) and ds[key]["path"] == rel:
                    ds[key]["sha256"] = digest
        manifest_path.write_text(json.dumps(raw, indent=2))


def _ms(ts):
    return int(pd.Timestamp(ts).value // 1_000_000)


def _gate(study):
    return DRIVER.preflight([om.load_manifest(str(study / "study_manifest.json")),
                             om.load_manifest(str(study / "study_manifest_fee_x2.json"))])


def test_frozen_inputs_pass_every_coverage_check():
    gate = _gate_from_repo()
    assert gate["complete"], gate["refused"]
    assert len(gate["windows"]) == 12
    for row in gate["windows"]:
        assert row["accrual_span"]["missing_slots"] == 0
        assert row["invalid_volume_rows"] == 0 and row["invalid_price_rows"] == 0


def _gate_from_repo():
    return DRIVER.preflight([om.load_manifest(os.path.join(STUDY_DIR, "study_manifest.json")),
                             om.load_manifest(os.path.join(STUDY_DIR, "study_manifest_fee_x2.json"))])


@pytest.mark.parametrize("rel,mutate,window,needle", [
    ("data/BTC_4h_candles.csv.gz",
     lambda df: df[df["timestamp"] != _ms("2025-10-01 08:00")], "test", "window bars"),
    ("data/ETH_4h_candles.csv.gz",
     lambda df: df.assign(volume=df["volume"].where(df["timestamp"] != _ms("2025-03-01 04:00"), -1.0)),
     "train", "volume"),
    ("data/SOL_4h_candles.csv.gz",
     lambda df: df.assign(high=df["high"].where(df["timestamp"] != _ms("2025-03-01 04:00"), float("nan"))),
     "train", "invalid prices"),
    ("data/BTC_funding.csv.gz",
     lambda df: df[(df["timestamp"] // HOUR_MS) != _ms("2026-01-05 13:00") // HOUR_MS], "test",
     "left-closed window"),
    ("data/ETH_funding.csv.gz",
     lambda df: df[(df["timestamp"] // HOUR_MS) != _ms("2025-08-31 22:00") // HOUR_MS], "test",
     "right-closed accrual span"),
])
def test_coverage_gate_refuses_before_selection(tmp_path, rel, mutate, window, needle):
    study = _copy_study(tmp_path)
    _rewrite(study, rel, mutate)
    gate = _gate(study)
    assert not gate["complete"]
    refused = [r for r in gate["refused"] if r["window"] == window]
    assert refused and any(needle in p for r in refused for p in r["problems"]), gate["refused"]
    result = DRIVER.run(str(study / "study_manifest.json"), str(study / "study_manifest_fee_x2.json"))
    assert result["verdict"]["outcome"] == "inconclusive"
    assert "coverage gate refused" in result["verdict"]["reason"]
    assert "arms" not in result and "held_out" not in result
    report = DRIVER.render(result)
    assert "REFUSED" in report and "No selection or score was computed" in report


def test_a_changed_hash_is_an_explicit_failure(tmp_path, capsys):
    study = _copy_study(tmp_path)
    path = study / "data" / "SOL_funding.csv.gz"
    with gzip.open(path, "rt") as fh:
        df = pd.read_csv(fh)
    with gzip.open(path, "wt") as fh:
        df.iloc[:-1].to_csv(fh, index=False)
    out_json = tmp_path / "results.json"
    code = DRIVER.main(["--manifest", str(study / "study_manifest.json"),
                        "--stress-manifest", str(study / "study_manifest_fee_x2.json"),
                        "--json", str(out_json), "--report", str(tmp_path / "REPORT.md")])
    assert code == 1
    assert "manifest error" in capsys.readouterr().err
    assert not out_json.exists()


def test_a_stress_manifest_with_other_data_is_refused(tmp_path):
    study = _copy_study(tmp_path)
    raw = json.loads((study / "study_manifest_fee_x2.json").read_text())
    raw["datasets"] = raw["datasets"][:2]
    (study / "study_manifest_fee_x2.json").write_text(json.dumps(raw))
    with pytest.raises(om.ManifestError, match="same datasets"):
        DRIVER.run(str(study / "study_manifest.json"), str(study / "study_manifest_fee_x2.json"))


def test_stress_manifest_doubles_only_the_fees():
    base = om.load_manifest(os.path.join(STUDY_DIR, "study_manifest.json"))
    stress = om.load_manifest(os.path.join(STUDY_DIR, "study_manifest_fee_x2.json"))
    assert stress["costs"]["taker_fee_pct"] == pytest.approx(2 * base["costs"]["taker_fee_pct"])
    assert stress["costs"]["maker_fee_pct"] == pytest.approx(2 * base["costs"]["maker_fee_pct"])
    for key in ("slippage_bps", "min_notional_usd", "min_notional_margin"):
        assert stress["costs"][key] == base["costs"][key]
    assert [d["candles"]["sha256"] for d in stress["datasets"]] == [d["candles"]["sha256"] for d in base["datasets"]]
    spec = om.execution_spec(stress, stress["datasets"][0], DRIVER.STRESS_COST)
    base_spec = om.execution_spec(base, base["datasets"][0], 1.0)
    assert spec["half_spread_pct"] == pytest.approx(2 * base_spec["half_spread_pct"])
    assert spec["slippage_pct"] == pytest.approx(2 * base_spec["slippage_pct"])


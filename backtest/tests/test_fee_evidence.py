import json
import os
import subprocess
import sys

import pytest

BACKTEST = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
TOOL = os.path.join(BACKTEST, "fee_evidence.py")
FIXTURE = os.path.join(BACKTEST, "testdata", "fee_evidence")
EXPORTS = ("export_live.json", "export_live_doge.json", "export_paper.json")


def _run(*extra):
    args = [sys.executable, TOOL]
    for name in EXPORTS:
        args += ["--export", os.path.join(FIXTURE, name)]
    proc = subprocess.run(args + list(extra), capture_output=True, text=True, cwd=BACKTEST)
    return proc.returncode, proc.stdout, proc.stderr


def _report(*extra):
    code, out, err = _run(*extra)
    assert code == 0, err
    return json.loads(out)


def test_report_without_venue_fills_or_log_extract():
    rep = _report()
    assert rep["schema"] == "go-trader.fee-evidence"
    assert rep["suggest_only"] is True
    assert rep["rows"]["candidates"] == 10
    assert rep["rows"]["excluded"] == {
        "fee_source_modeled": 1, "fee_source_reconcile_adjustment": 1,
        "funding": 1, "partition_not_live": 1,
    }
    groups = rep["booked_rates"]["groups"]
    assert groups["open"]["count"] == 4
    assert groups["open"]["median"] == pytest.approx(0.00045, rel=1e-12)
    assert groups["add"]["median"] == pytest.approx(0.00045, rel=1e-12)
    assert groups["take_profit_tier"]["median"] == pytest.approx(0.00015, rel=1e-12)
    assert groups["signal_close"]["count"] == 1
    assert groups["stop_loss"]["count"] == 2
    assert groups["unclassified"]["count"] == 1
    assert rep["booked_rates"]["unclassified_event_keys"] == ["primary/trades/12"]
    assert [r["event_key"] for r in rep["booked_rates"]["rebates"]] == ["primary/trades/8"]
    assert [r["event_key"] for r in rep["booked_rates"]["outliers"]] == ["primary/trades/8"]
    assert rep["booked_rates"]["stop_rows_value_basis_trigger_price"] == ["primary/trades/5"]

    assert rep["venue_rates"] == {"status": "unavailable", "reason": "no_user_fills"}
    stop = rep["stop_slippage"]
    assert stop["status"] == "unavailable"
    assert stop["reason"] == "no_user_fills"
    assert stop["venue_joined"] == {"count": 0}
    assert stop["booked_at_trigger_no_venue_evidence"] == 1
    assert [d["event_key"] for d in stop["export_price_differs"]] == ["primary/trades/7"]
    assert rep["market_fill_slippage"] == {"status": "unavailable", "reason": "no_log_extract"}


def test_venue_join_and_log_extract():
    rep = _report("--user-fills", os.path.join(FIXTURE, "user_fills.json"),
                  "--fill-log", os.path.join(FIXTURE, "fills.log"))
    venue = rep["venue_rates"]
    assert venue["status"] == "available"
    assert venue["fills_usable"] == 10
    assert venue["fills_not_joined"] == 1
    assert venue["all_fills_by_role"]["taker"]["notional_weighted"] == pytest.approx(0.00045, rel=1e-12)
    tp = venue["by_group"]["take_profit_tier"]
    assert tp["role_counts"] == {"maker": 1, "taker": 0, "unknown": 0}
    assert tp["by_role"]["maker"]["median"] == pytest.approx(0.00015, rel=1e-12)
    assert venue["by_group"]["stop_loss"]["role_counts"]["taker"] == 2

    stop = rep["stop_slippage"]
    assert stop["status"] == "available"
    assert stop["booked_at_trigger_no_venue_evidence"] == 0
    assert stop["stop_rows_with_order_id_not_in_user_fills"] == 1
    sample = stop["venue_joined_samples"]
    assert [s["event_key"] for s in sample] == ["primary/trades/5"]
    vwap = (0.3 * 1988.0 + 0.1 * 1990.0) / 0.4
    assert sample[0]["slippage"] == pytest.approx((1990.0 - vwap) / 1990.0, rel=1e-12)

    mkt = rep["market_fill_slippage"]
    assert mkt["status"] == "available"
    assert mkt["label"] == "reference price as logged"
    assert mkt["excluded"] == {"ambiguous": 0, "log_precision_insufficient": 1, "unpaired": 1}
    by_key = {s["event_key"]: s["slippage"] for s in mkt["samples"]}
    assert by_key["primary/trades/1"] == pytest.approx((2000.0 - 1999.5) / 1999.5, rel=1e-12)
    assert by_key["primary/trades/2"] == pytest.approx((2010.0 - 2009.0) / 2009.0, rel=1e-12)


def test_refuses_a_fill_whose_coin_disagrees_with_the_row(tmp_path):
    with open(os.path.join(FIXTURE, "user_fills.json")) as fh:
        fills = json.load(fh)
    fills[3]["coin"] = "BTC"
    bad = tmp_path / "user_fills.json"
    bad.write_text(json.dumps(fills))
    code, out, err = _run("--user-fills", str(bad))
    assert code == 1
    assert "disagrees" in json.loads(out)["error"]


def test_refuses_an_export_that_fails_ledger_validation(tmp_path):
    with open(os.path.join(FIXTURE, "export_live.json")) as fh:
        doc = json.load(fh)
    doc["schema_version"] = 99
    bad = tmp_path / "export.json"
    bad.write_text(json.dumps(doc))
    proc = subprocess.run([sys.executable, TOOL, "--export", str(bad)], capture_output=True, text=True,
                          cwd=BACKTEST)
    assert proc.returncode == 1
    assert "version" in json.loads(proc.stdout)["error"]

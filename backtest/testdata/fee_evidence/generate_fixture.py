#!/usr/bin/env python3

import json
import os

HERE = os.path.dirname(os.path.abspath(__file__))
FIELDS = (
    "symbol", "side", "trade_type", "details", "position_id", "exchange_order_id", "fee_source",
    "regime", "quantity", "price", "value", "exchange_fee", "realized_pnl", "is_close", "pnl_gross",
    "manual", "row_net_pnl", "ledger_delta", "event_kind", "close_reason", "close_extent",
    "position_allocation", "entry_atr", "stop_loss_atr_mult", "stop_loss_trigger_px",
    "stop_loss_oid", "tp_oids_json", "tp_tiers_json",
)


def _prov(row_id, field):
    return [{"kind": "synthetic_test_evidence", "source_role": "primary", "source_table": "trades",
             "source_row_id": str(row_id), "source_field": field}]


def _ev(value, row_id, field):
    if value is None:
        return {"value": None, "raw_value": None, "status": "unavailable", "reason": "not_recorded",
                "provenance": []}
    return {"value": value, "raw_value": value, "status": "available", "reason": None,
            "provenance": _prov(row_id, field)}


def _na():
    return {"value": None, "raw_value": None, "status": "not_applicable", "reason": "not_applicable",
            "provenance": []}


def event(strategy, partition, row_id, ts, kind, symbol, side, qty, price, fee, fee_source, details,
          oid=None, close_reason=None, trigger=None, trade_type="perps"):
    value = round(qty * price, 10) if kind != "funding" else 0.0
    is_close = kind == "close"
    ev = {
        "event_key": f"primary/trades/{row_id}", "source_role": "primary", "source_table": "trades",
        "source_row_id": str(row_id), "partition": partition, "process_strategy_id": strategy,
        "storage_strategy_id": strategy, "timestamp_raw": ts, "timestamp": ts,
        "timestamp_meaning": "ledger_record_time",
    }
    vals = {
        "symbol": symbol, "side": side, "trade_type": trade_type, "details": details,
        "position_id": f"pos-{row_id}", "exchange_order_id": oid, "fee_source": fee_source,
        "regime": None, "quantity": qty, "price": price, "value": value, "exchange_fee": fee,
        "realized_pnl": 0.0, "is_close": is_close, "pnl_gross": True, "manual": False,
        "row_net_pnl": -fee, "ledger_delta": -fee, "event_kind": kind, "close_reason": close_reason,
        "close_extent": None, "position_allocation": "recorded", "entry_atr": None,
        "stop_loss_atr_mult": None, "stop_loss_trigger_px": trigger, "stop_loss_oid": None,
        "tp_oids_json": None, "tp_tiers_json": None,
    }
    for f in FIELDS:
        ev[f] = _ev(vals[f], row_id, f)
    if not is_close:
        ev["close_reason"] = _na()
        ev["close_extent"] = _na()
    return ev


def document(strategy, partition, capture_sha, events):
    return {
        "schema": "go-trader.booked-ledger",
        "schema_version": 1,
        "inspected_revision": None,
        "capture_manifest_sha256": capture_sha,
        "time_basis": "UTC",
        "timestamp_meanings": {
            "trades": "stored ledger timestamp; exchange fill time is not established",
            "wallet_transfers": "stored exchange ledger event time in Unix milliseconds",
        },
        "selection": {"partition": partition, "process_strategy_id": strategy, "storage_strategy_id": strategy,
                      "source_role": "primary", "platform": "hyperliquid"},
        "capture": {"started_at": "2026-01-06T00:00:00Z", "completed_at": "2026-01-06T00:00:01Z",
                    "source_revision": None, "consistency": "transactional_per_file"},
        "snapshot_files": [{"source_role": "primary", "relative_path": "state/primary.db",
                            "sha256": "0" * 64}],
        "current_effective_configuration": {"basis": "current_at_capture", "config_version": 20,
                                            "strategy": {}, "regime": {}, "portfolio_risk": {}},
        "events": events,
        "wallet_orphan_context": {"status": "available", "reason": None, "ownership": "live_wallet",
                                  "allocation": "unallocated", "records": []},
    }


def fill(coin, oid, tid, ms, px, sz, fee, crossed, side):
    return {"coin": coin, "px": px, "sz": sz, "side": side, "time": ms, "startPosition": "0",
            "dir": "synthetic", "closedPnl": "0", "hash": "0x" + "0" * 64, "oid": oid,
            "crossed": crossed, "fee": fee, "tid": tid, "feeToken": "USDC"}


def main():
    s = "hl-fee-fixture"
    live = document(s, "live", "a" * 64, [
        event(s, "live", 1, "2026-01-05T00:00:41Z", "non_close", "ETH", "buy", 0.5, 2000.0, 0.45,
              "userfills", "Open long 0.500000 @ $2000.00 (5x, fee $0.45)", oid="1001"),
        event(s, "live", 2, "2026-01-05T01:00:41Z", "scale_in", "ETH", "buy", 0.25, 2010.0, 0.226125,
              "userfills", "Scale-in long", oid="1002", trade_type="scale_in"),
        event(s, "live", 3, "2026-01-05T02:00:41Z", "close", "ETH", "sell", 0.25, 2050.0, 0.076875,
              "userfills", "TP1 fill close 0.250000, PnL: $11.00 (fee $0.08)", oid="1003"),
        event(s, "live", 4, "2026-01-05T03:00:41Z", "close", "ETH", "sell", 0.1, 2040.0, 0.0918,
              "userfills", "Partial-close long 0.100000, PnL: $3.90 (fee $0.09)", oid="1004"),
        event(s, "live", 5, "2026-01-05T04:00:41Z", "close", "ETH", "sell", 0.4, 1990.0, 0.35793,
              "userfills", "Stop loss close, PnL: $-4.36 (fee $0.36)", oid="1005", close_reason="stop_loss",
              trigger=1990.0),
        event(s, "live", 6, "2026-01-05T05:00:41Z", "non_close", "ETH", "sell", 0.3, 2100.0, 0.2835,
              "userfills", "Open short 0.300000 @ $2100.00 (5x, fee $0.28)", oid="1006"),
        event(s, "live", 7, "2026-01-05T06:00:41Z", "close", "ETH", "buy", 0.3, 2106.3, 0.28435,
              "userfills", "Stop loss close, PnL: $-2.17 (fee $0.28)", oid="1007",
              close_reason="hl_sync_stop_loss", trigger=2105.0),
        event(s, "live", 8, "2026-01-05T07:00:41Z", "non_close", "ETH", "buy", 0.2, 2000.0, -0.008,
              "userfills", "Open long 0.200000 @ $2000.00 (5x, fee $-0.01)", oid="1008"),
        event(s, "live", 9, "2026-01-05T08:00:41Z", "close", "ETH", "sell", 0.2, 2001.0, 0.18009,
              "modeled", "Close long, PnL: $0.02 (fee $0.18)"),
        event(s, "live", 10, "2026-01-05T08:30:41Z", "close", "ETH", "sell", 0.01, 2001.0, 0.01,
              "reconcile_adjustment", "Circuit breaker close long, PnL: $0.00 (model-only reconciliation adjustment; no exchange fill)"),
        event(s, "live", 11, "2026-01-05T09:00:00Z", "funding", "ETH", "funding", 0.0, 0.0, 0.0,
              None, "Funding payment", trade_type="funding"),
        event(s, "live", 12, "2026-01-05T10:00:41Z", "close", "ETH", "sell", 0.05, 2000.0, 0.045,
              "userfills", "Unrecognised close label", oid="1012"),
    ])
    doge = document("hl-fee-fixture-doge", "live", "b" * 64, [
        event("hl-fee-fixture-doge", "live", 21, "2026-01-05T00:30:41Z", "non_close", "DOGE", "buy", 1000.0,
              0.15, 0.0675, "userfills", "Open long 1000.000000 @ $0.15 (3x, fee $0.07)", oid="3001"),
    ])
    paper = document("hl-fee-fixture-paper", "paper", "c" * 64, [
        event("hl-fee-fixture-paper", "paper", 31, "2026-01-05T00:00:42Z", "non_close", "ETH", "buy", 0.5,
              2000.0, 0.45, "modeled", "Open long 0.500000 @ $2000.00 (5x, fee $0.45)"),
    ])
    base = 1767571200000
    hour = 3600000
    fills = [
        fill("ETH", 1001, 9001, base + 40000, "2000.0", "0.5", "0.45", True, "B"),
        fill("ETH", 1001, 9001, base + 40000, "2000.0", "0.5", "0.45", True, "B"),
        fill("ETH", 1002, 9002, base + hour + 40000, "2010.0", "0.25", "0.226125", True, "B"),
        fill("ETH", 1003, 9003, base + 2 * hour + 40000, "2050.0", "0.25", "0.076875", False, "A"),
        fill("ETH", 1004, 9004, base + 3 * hour + 40000, "2040.0", "0.1", "0.0918", True, "A"),
        fill("ETH", 1005, 9005, base + 4 * hour + 40000, "1988.0", "0.3", "0.268380", True, "A"),
        fill("ETH", 1005, 9006, base + 4 * hour + 40500, "1990.0", "0.1", "0.089550", True, "A"),
        fill("ETH", 1006, 9007, base + 5 * hour + 40000, "2100.0", "0.3", "0.2835", True, "A"),
        fill("ETH", 1008, 9008, base + 7 * hour + 40000, "2000.0", "0.2", "-0.008", False, "B"),
        fill("DOGE", 3001, 9021, base + 1800000 + 40000, "0.15", "1000.0", "0.0675", True, "B"),
        fill("BTC", 2001, 9011, base + 9 * hour, "60000.0", "0.01", "0.27", True, "B"),
    ]
    log_lines = [
        "[2026-01-05 00:00:40] [hl-fee-fixture] [INFO] Live fill at $2000.00 qty=0.500000 (mid was $1999.50)",
        "[2026-01-05 00:30:40] [hl-fee-fixture-doge] [INFO] Live fill at $0.15 qty=1000.000000 (mid was $0.15)",
        "[2026-01-05 01:00:40] [hl-fee-fixture] [INFO] Live scale-in fill at $2010.00 qty=0.250000 (mid was $2009.00)",
        "[2026-01-05 01:00:40] [hl-fee-fixture] [INFO] Placing stop-loss trigger (unrelated line)",
        "[2026-01-05 11:00:40] [hl-fee-fixture] [INFO] Live fill at $2100.00 qty=0.123456 (mid was $2099.00)",
    ]
    outputs = {
        "export_live.json": live,
        "export_live_doge.json": doge,
        "export_paper.json": paper,
        "user_fills.json": fills,
    }
    for name, obj in outputs.items():
        with open(os.path.join(HERE, name), "w") as fh:
            fh.write(json.dumps(obj, indent=2) + "\n")
    with open(os.path.join(HERE, "fills.log"), "w") as fh:
        fh.write("\n".join(log_lines) + "\n")


if __name__ == "__main__":
    main()

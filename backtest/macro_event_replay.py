import argparse
import hashlib
import json
import os
import sys

_HERE = os.path.dirname(os.path.abspath(__file__))
_REPO = os.path.abspath(os.path.join(_HERE, ".."))
for _p in (_HERE, os.path.join(_REPO, "shared_tools")):
    if _p not in sys.path:
        sys.path.insert(0, _p)

import macro_event_inputs as mi

HOUR_MS = 3_600_000
EVENT_BAR = 100
NOW_BAR = 102
NAME = "macro_event_breakout"
EVIDENCE = ("signal", "meb_reason", "meb_event_id", "meb_event_record_sha256", "meb_range_high", "meb_range_low",
            "meb_range_start_ms", "meb_range_end_ms", "meb_bid", "meb_ask", "meb_quote_record_sha256")


class HarnessError(RuntimeError):
    pass


def fixture(out_dir: str, t0: int) -> dict:
    os.makedirs(out_dir, exist_ok=True)
    cutoff = t0 + NOW_BAR * HOUR_MS

    def local(ms):
        import datetime as dt
        return dt.datetime.fromtimestamp(ms / 1000, dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%S")

    def event(eid, ms, known):
        return {"event_id": eid, "event_type": mi.EVENT_TYPE_FOMC, "revision": 1, "status": "scheduled",
                "scheduled_local": local(ms), "source_timezone": "UTC", "known_ms": known,
                "publication_evidence": "fixture", "source_id": "fixture"}

    source = {"source_id": "fixture", "publisher": "test", "url": "fixture:macro", "retrieved_ms": t0,
              "sha256": "0" * 64, "published_ms": None, "note": "integration fixture"}
    rows = [event("fixture/event", t0 + EVENT_BAR * HOUR_MS, t0 - 200 * HOUR_MS),
            event("fixture/at-cutoff", t0 + 140 * HOUR_MS, cutoff),
            event("fixture/after-cutoff", t0 + 150 * HOUR_MS, cutoff + 1),
            event("fixture/unknown", t0 + 130 * HOUR_MS, None)]
    coverage = [{"event_type": mi.EVENT_TYPE_FOMC, "start_ms": t0 - 1000 * HOUR_MS, "end_ms": t0 + 1000 * HOUR_MS}]
    cal, _ = mi.import_calendar(rows, [source], coverage, "fixture", [mi.EVENT_TYPE_FOMC])
    quotes = [{"event_ms": t0 + i * HOUR_MS - 2_000, "recv_ms": t0 + i * HOUR_MS - 1_500, "bid": "100",
               "ask": "100.01", "source_id": "fixture"} for i in range(NOW_BAR + 1)]
    quotes += [{"event_ms": cutoff - 500, "recv_ms": cutoff, "bid": "100", "ask": "100.02", "source_id": "fixture"},
               {"event_ms": cutoff - 400, "recv_ms": cutoff + 1, "bid": "100", "ask": "100.03", "source_id": "fixture"}]
    qdoc, _ = mi.import_quotes(quotes, [{k: v for k, v in source.items() if k != "published_ms"}], "BTC", "fixture")
    inputs = {"calendar_path": "calendar.json", "calendar_sha256": mi.write_artifact(os.path.join(out_dir, "calendar.json"), cal),
              "quote_path": "quotes.json", "quote_sha256": mi.write_artifact(os.path.join(out_dir, "quotes.json"), qdoc)}
    with open(os.path.join(out_dir, "inputs.json"), "w") as fh:
        json.dump(inputs, fh, sort_keys=True)
    return inputs


def _payload_frame(market, coin, timeframe):
    import pandas as pd
    from market_payload import market_frame_rows_with_timing
    rows, timing = market_frame_rows_with_timing(market, coin, timeframe, HarnessError)
    bars = (timing or {}).get("bars")
    if not isinstance(bars, list) or len(bars) != len(rows):
        raise HarnessError("the sealed frame carries no per-bar timing")
    df = pd.DataFrame([r[1:] for r in rows], columns=["open", "high", "low", "close", "volume"])
    df.index = pd.to_datetime([int(b[0]) for b in bars], unit="ms")
    df.attrs["bar_interval_ms"] = HOUR_MS
    df.attrs["bar_timestamp_kind"] = "open"
    closed = [bool(b[2]) for b in bars]
    return df, closed


def _evaluate(df, params, inputs):
    from registry_loader import load_registry
    from close_registry_loader import evaluate as close_evaluate
    from strategy_composition import evaluate_open_close
    reg = load_registry("futures")
    ev = evaluate_open_close(reg.apply_strategy, reg.get_strategy, df, NAME, None, ["time_stop"], "",
                             params={**params, **inputs}, position_ctx={}, close_evaluate=close_evaluate,
                             close_params_by_name={"time_stop": {"max_bars": int(params["holding_bars"])}})
    return ev.open_result_df


def _rows(frame, n=None):
    sub = frame if n is None else frame.iloc[:n]
    return [[None if (isinstance(v, float) and v != v) else v for v in r] for r in sub[list(EVIDENCE)].itertuples(index=False)]


def harness(dir_: str) -> dict:
    from market_payload import market_macro_events, market_macro_quotes, validate_market_payload
    with open(os.path.join(dir_, "harness.json")) as fh:
        meta = json.load(fh)
    with open(os.path.join(dir_, "seal.json"), "rb") as fh:
        if hashlib.sha256(fh.read()).hexdigest() != meta["seal_sha256"]:
            raise HarnessError("the seal bytes do not match the harness metadata")
    params = {k: v for k, v in meta["params"].items()}
    for k in ("expected_interval_ms", "range_bars", "post_window_bars", "confirm_bars", "cooldown_bars",
              "holding_bars", "max_quote_age_ms"):
        params[k] = int(params[k])
    frames = {}
    for name in ("ws", "shared"):
        with open(os.path.join(dir_, f"payload_{name}.json")) as fh:
            env = json.load(fh)
        market = validate_market_payload(env["market"], HarnessError)
        events = market_macro_events(market, params["calendar_sha256"], HarnessError)
        quotes = market_macro_quotes(market, "BTC", params["quote_sha256"], HarnessError)
        if not events.get("available") or not quotes.get("available"):
            raise HarnessError(f"{name} payload macro inputs are unavailable: {events.get('reason')} {quotes.get('reason')}")
        if events["cutoff_ms"] != meta["cutoff_ms"] or quotes["cutoff_ms"] != meta["cutoff_ms"]:
            raise HarnessError(f"{name} payload cutoff disagrees with the seal key")
        df, closed = _payload_frame(market, "BTC", "1h")
        frames[name] = (df, closed, _evaluate(df, params, {"event_records": events, "spread_records": quotes}))
    ws, shared = frames["ws"][2], frames["shared"][2]
    if _rows(ws) != _rows(shared):
        raise HarnessError("websocket and shared payloads produced different decisions")
    df, closed, _ = frames["ws"]
    n_closed = sum(closed)
    research_events, research_quotes, _ = mi.research_inputs(params, "BTC")
    offline = _evaluate(df.iloc[:n_closed].copy(), params, {"event_records": research_events,
                                                           "spread_records": research_quotes})
    if _rows(offline) != _rows(ws, n_closed):
        raise HarnessError("the sealed decisions differ from the offline research loader on the closed bars")
    entry_idx = [i for i, s in enumerate(ws["signal"].tolist()) if s != 0]
    want = int((meta["cutoff_ms"] - (NOW_BAR - EVENT_BAR) * HOUR_MS - int(df.index[0].value // 1_000_000)) // HOUR_MS)
    if entry_idx != [want] or ws["meb_reason"].iloc[want] != "entry_long":
        raise HarnessError(f"the fixture entry is not the single long at bar {want}: {entry_idx}")
    if not bool(ws["meb_carried"].iloc[-1]) or closed[-1]:
        raise HarnessError("the forming bar must carry the last closed decision")
    return {"ok": True, "rows": len(ws), "closed_rows": n_closed, "entry_bar": want,
            "entry_event": ws["meb_event_id"].iloc[want]}


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description="Macro-event research fixture and sealed-payload harness (#1654)")
    sub = ap.add_subparsers(dest="cmd", required=True)
    f = sub.add_parser("fixture")
    f.add_argument("--out", required=True)
    f.add_argument("--t0", type=int, required=True)
    h = sub.add_parser("harness")
    h.add_argument("--dir", required=True)
    args = ap.parse_args(argv)
    try:
        out = fixture(args.out, args.t0) if args.cmd == "fixture" else harness(args.dir)
    except (HarnessError, mi.MacroInputError) as exc:
        print(json.dumps({"ok": False, "error": str(exc)}))
        return 1
    print(json.dumps(out, sort_keys=True))
    return 0


if __name__ == "__main__":
    sys.exit(main())

import copy
import gzip
import io
import json
import os
import subprocess
import sys

import numpy as np
import pandas as pd
import pytest

_HERE = os.path.dirname(os.path.abspath(__file__))
_REPO = os.path.abspath(os.path.join(_HERE, "..", ".."))
_TOOLS = os.path.join(_REPO, "shared_tools")
if _TOOLS not in sys.path:
    sys.path.insert(0, _TOOLS)

import macro_event_inputs as mi
import offline_manifest as om
from atr import ensure_atr_indicator
from backtester import Backtester
from close_registry_loader import evaluate as close_evaluate
from close_registry_loader import get_strategy as get_close
from optimizer import DEFAULT_PARAM_RANGES, generate_param_grid
from registry_loader import load_registry
from strategy_composition import (
    GATE_MODE_LIVE,
    GATE_MODE_MISSING,
    GATE_MODE_PAPER,
    Acknowledgement,
    GateMode,
    admit_configured_strategies,
    evaluate_open_close,
    finalize_decision,
)

NAME = "macro_event_breakout"
H = 3_600_000
T0 = int(pd.Timestamp("2026-01-05T00:00:00Z").value // 1_000_000)
N = 160
EVENT_BAR = 100
RELEASE = T0 + EVENT_BAR * H
SEED = {"expected_interval_ms": H, "range_bars": 6, "post_window_bars": 4, "confirm_bars": 1,
        "max_spread_frac": 0.0005, "max_quote_age_ms": 60_000, "cooldown_bars": 24, "holding_bars": 6,
        "spread_gate": True}
SOURCE = {"source_id": "fixture", "publisher": "test", "url": "fixture:macro", "retrieved_ms": T0,
          "sha256": "0" * 64, "published_ms": None, "note": "integration fixture"}
QSOURCE = {k: v for k, v in SOURCE.items() if k != "published_ms"}
COVER = [{"event_type": mi.EVENT_TYPE_FOMC, "start_ms": T0 - 1000 * H, "end_ms": T0 + 1000 * H}]
RUN_BACKTEST_SNIPPET = (
    "import json, sys; sys.path[:0] = ['backtest', 'shared_tools']\n"
    "from run_backtest import run_single_backtest\n"
    "params = json.loads(sys.argv[1])\n"
    "out = run_single_backtest('macro_event_breakout', 'BTC/USDT', '1h', '2026-01-05', params=params, "
    "registry='futures', platform='hyperliquid', funding_mode='off', direction='both', "
    "close_strategies=[{'name': 'time_stop', 'params': {'max_bars': 6}}], stop_loss_atr_mult=5.0, "
    "comparison_mode='approximate')\n"
    "print('RESULT', json.dumps([[t['entry_date'], t['side'], t['exit_reason']] for t in out['trades']]))\n"
)


def _reg():
    return load_registry("futures")


def _utc_local(ms):
    return pd.Timestamp(ms, unit="ms").strftime("%Y-%m-%dT%H:%M:%S")


def _event(ms=RELEASE, known=T0 - 200 * H, revision=1, status="scheduled", evidence="fixture", eid=None):
    return {"event_id": eid or f"fixture/{pd.Timestamp(RELEASE, unit='ms'):%Y-%m-%d}", "event_type": mi.EVENT_TYPE_FOMC,
            "revision": revision, "status": status, "scheduled_local": _utc_local(ms), "source_timezone": "UTC",
            "known_ms": known, "publication_evidence": evidence, "source_id": "fixture"}


def _calendar(rows, coverage=COVER):
    doc, stats = mi.import_calendar(rows, [SOURCE], coverage, "fixture", [mi.EVENT_TYPE_FOMC])
    return doc, stats


def _quotes(coin="BTC", spread=0.0001, skip=(), late=(), conflict=(), age_ms=2_000):
    rows = []
    for i in range(N + 1):
        deadline = T0 + i * H
        if i in skip:
            continue
        ev = deadline - age_ms
        recv = deadline + 5_000 if i in late else ev + 500
        bid = 100.0
        rows.append({"event_ms": ev, "recv_ms": recv, "bid": f"{bid:.6f}", "ask": f"{bid * (1 + spread):.6f}",
                     "source_id": "fixture"})
        if i in conflict:
            rows.append({"event_ms": ev, "recv_ms": recv + 1, "bid": "99.5", "ask": "99.51", "source_id": "fixture"})
    doc, _ = mi.import_quotes(rows, [QSOURCE], coin, "fixture")
    return doc


def _frame(n=N, breakout=None, level=100.0):
    close = np.full(n, level)
    if breakout is not None:
        for i, px in breakout.items():
            close[i] = px
    opens = np.concatenate([[close[0]], close[:-1]])
    df = pd.DataFrame({"open": opens, "high": np.maximum(opens, close) + 0.5,
                       "low": np.minimum(opens, close) - 0.5, "close": close, "volume": 1.0})
    df.index = pd.to_datetime(T0 + np.arange(n) * H, unit="ms")
    return df


def _run(df, cal=None, quotes=None, params=None, cutoff=None):
    cal = cal if cal is not None else _calendar([_event()])[0]
    events = mi.calendar_payload(cal, "1" * 64, cutoff)
    spread = mi.quote_payload(quotes, "2" * 64, cutoff) if quotes is not None else None
    return _reg().apply_strategy(NAME, df, {**SEED, **(params or {}), "event_records": events,
                                            "spread_records": spread})


def _reason(out, i):
    return out["meb_reason"].iloc[i]


UP = {EVENT_BAR: 101.2, EVENT_BAR + 1: 101.5, EVENT_BAR + 2: 101.7}
DOWN = {EVENT_BAR: 98.8, EVENT_BAR + 1: 98.5}


def test_registration_is_futures_only_bidirectional_hidden_production_refused_with_a_valid_grid():
    reg = _reg()
    entry = reg.STRATEGY_REGISTRY[NAME]
    assert entry["production_refused"] is True and entry["edge_status"] == "no_edge"
    assert entry["edge_ref"] == "backtest/candidates/macro_event_breakout_1654/REPORT.md"
    assert NAME not in reg.list_strategies()
    assert NAME not in load_registry("spot").STRATEGY_REGISTRY
    registry = reg._registry
    assert registry.STRATEGIES[NAME]["short_entries"] is True
    assert NAME in registry.production_refused_strategies()
    grid = generate_param_grid(DEFAULT_PARAM_RANGES[NAME])
    assert len(grid) == 12
    for params in grid:
        registry.validate_params(NAME, params)
    for bad in ({"confirm_bars": 7}, {"range_bars": 0}, {"max_spread_frac": 0.0}, {"confirm_bars": 3,
                                                                                   "post_window_bars": 2},
                {"spread_gate": "yes"}, {"event_type": "cpi"}, {"calendar_path": "x.json"},
                {"calendar_sha256": "ABC"}, {"max_quote_age_ms": H + 1}, {"expected_interval_ms": 300_000},
                {"unknown_key": 1}):
        with pytest.raises(ValueError):
            registry.validate_params(NAME, bad)


def test_importer_is_deterministic_and_artifact_identity_is_pinned(tmp_path):
    rows = [_event(), _event(eid="fixture/b", ms=RELEASE + 48 * H), _event()]
    a, stats = _calendar(rows)
    b, _ = _calendar(list(reversed(rows)))
    assert stats["identical_duplicates"] == 1 and mi.artifact_bytes(a) == mi.artifact_bytes(b)
    path = str(tmp_path / "cal.json")
    sha = mi.write_artifact(path, a)
    doc, got = mi.load_calendar_artifact(path, sha)
    assert got == sha and doc == a
    assert mi.calendar_payload(doc, sha) == mi.calendar_payload(mi.load_calendar_artifact(path, sha)[0], sha)
    with open(path, "rb") as fh:
        blob = fh.read()
    with open(path, "wb") as fh:
        fh.write(blob.replace(b'"revision":1', b'"revision":2', 1))
    with pytest.raises(mi.MacroInputError, match="sha256"):
        mi.load_calendar_artifact(path, sha)
    tampered = copy.deepcopy(a)
    tampered["records"][0]["scheduled_utc_ms"] += H
    with pytest.raises(mi.MacroInputError, match="UTC conversion"):
        mi.validate_calendar_artifact(tampered)
    swapped = copy.deepcopy(a)
    swapped["records"].reverse()
    with pytest.raises(mi.MacroInputError, match="canonical order"):
        mi.validate_calendar_artifact(swapped)
    duplicated = copy.deepcopy(a)
    duplicated["records"].insert(0, duplicated["records"][0])
    with pytest.raises(mi.MacroInputError, match="duplicates"):
        mi.validate_calendar_artifact(duplicated)
    extra = copy.deepcopy(a)
    extra["records"][0]["note"] = "x"
    with pytest.raises(mi.MacroInputError, match="unknown"):
        mi.validate_calendar_artifact(extra)
    qpath = str(tmp_path / "q.json")
    qsha = mi.write_artifact(qpath, _quotes())
    with pytest.raises(mi.MacroInputError, match="coin"):
        mi.load_quote_artifact(qpath, qsha, "ETH")
    with pytest.raises(mi.MacroInputError, match="ambiguous"):
        mi.local_to_utc_ms("2026-11-01T01:30:00", "America/New_York")
    with pytest.raises(mi.MacroInputError, match="does not exist"):
        mi.local_to_utc_ms("2026-03-08T02:30:00", "America/New_York")


def test_mirrored_entries_record_their_evidence_and_respect_the_range_boundary():
    quotes = _quotes()
    long_out = _run(_frame(breakout=UP), quotes=quotes)
    i = int(np.flatnonzero(long_out["signal"].to_numpy() == 1)[0])
    assert i == EVENT_BAR and _reason(long_out, i) == "entry_long"
    row = long_out.iloc[i]
    assert row.meb_range_high == 100.5 and row.meb_range_start_ms == RELEASE - 6 * H
    assert row.meb_range_end_ms == RELEASE and row.meb_release_ms == RELEASE
    assert row.meb_event_revision == 1 and len(row.meb_event_record_sha256) == 64
    assert row.meb_bid == "100" and row.meb_ask == "100.01" and row.meb_quote_sha256 == "2" * 64
    assert json.loads(row.meb_params)["range_bars"] == 6
    assert long_out["signal"].abs().sum() == 1
    assert _reason(long_out, EVENT_BAR + 1) == "event_consumed"
    short_out = _run(_frame(breakout=DOWN), quotes=quotes)
    assert int(np.flatnonzero(short_out["signal"].to_numpy() == -1)[0]) == EVENT_BAR
    equal = _run(_frame(breakout={EVENT_BAR: 100.5}), quotes=quotes)
    assert _reason(equal, EVENT_BAR) == "inside_range" and equal["signal"].abs().sum() == 0
    pre = _run(_frame(breakout={EVENT_BAR - 1: 103.0}), quotes=quotes)
    assert pre["signal"].abs().sum() == 0


def test_confirmation_expiry_and_cooldown():
    quotes = _quotes()
    two = _run(_frame(breakout=UP), quotes=quotes, params={"confirm_bars": 2})
    assert _reason(two, EVENT_BAR) == "awaiting_confirmation"
    assert int(np.flatnonzero(two["signal"].to_numpy() == 1)[0]) == EVENT_BAR + 1
    late = _run(_frame(breakout={EVENT_BAR + 4: 102.0, EVENT_BAR + 5: 102.0}), quotes=quotes)
    assert late["signal"].abs().sum() == 0 and _reason(late, EVENT_BAR + 4) == "no_event"
    second = RELEASE + 10 * H
    cal, _ = _calendar([_event(), _event(eid="fixture/second", ms=second)])
    df = _frame(breakout={**UP, EVENT_BAR + 10: 102.5, EVENT_BAR + 11: 102.6})
    cooled = _run(df, cal=cal, quotes=quotes)
    assert _reason(cooled, EVENT_BAR + 10) == "cooldown" and cooled["signal"].abs().sum() == 1
    free = _run(df, cal=cal, quotes=quotes, params={"cooldown_bars": 0})
    assert free["signal"].abs().sum() == 2


def test_schedule_provenance_holds_unknown_late_revised_postponed_conflicting_and_uncovered_events():
    quotes = _quotes()
    df = _frame(breakout=UP)
    unknown, _ = _calendar([_event(known=None)])
    assert _run(df, cal=unknown, quotes=quotes)["signal"].abs().sum() == 0
    late, _ = _calendar([_event(known=RELEASE + 1)])
    assert _reason(_run(df, cal=late, quotes=quotes), EVENT_BAR) == "event_not_known_before_release"
    revised, _ = _calendar([_event(), _event(revision=2, known=RELEASE + H // 2, evidence="reissued")])
    out = _run(_frame(breakout={EVENT_BAR: 100.2, EVENT_BAR + 1: 101.5}), cal=revised, quotes=quotes)
    assert _reason(out, EVENT_BAR + 1) == "schedule_revised" and out["signal"].abs().sum() == 0
    postponed, _ = _calendar([_event(), _event(revision=2, status="postponed", known=RELEASE - H)])
    assert _reason(_run(df, cal=postponed, quotes=quotes), EVENT_BAR) == "event_postponed"
    moved, _ = _calendar([_event(), _event(revision=2, ms=RELEASE + 2 * H, known=RELEASE - 10 * H)])
    moved_out = _run(_frame(breakout={EVENT_BAR + 2: 101.4}), cal=moved, quotes=quotes)
    assert int(np.flatnonzero(moved_out["signal"].to_numpy() == 1)[0]) == EVENT_BAR + 2
    assert moved_out["meb_range_end_ms"].iloc[EVENT_BAR + 2] == RELEASE + 2 * H
    conflict, stats = _calendar([_event(), _event(revision=2, known=RELEASE - H),
                                 _event(revision=2, ms=RELEASE + H, known=RELEASE - H)])
    assert stats["conflicting_versions"] == 1
    assert _reason(_run(df, cal=conflict, quotes=quotes), EVENT_BAR) == "event_conflict"
    uncovered, _ = _calendar([_event()], coverage=[{"event_type": mi.EVENT_TYPE_FOMC, "start_ms": RELEASE,
                                                    "end_ms": RELEASE + 100 * H}])
    assert _reason(_run(df, cal=uncovered, quotes=quotes), EVENT_BAR) == "calendar_not_covered"
    assert _reason(_run(df, quotes=quotes, params={"event_type": mi.EVENT_TYPE_CONTROL}),
                   EVENT_BAR) == "calendar_not_covered"


def test_spread_gate_holds_missing_stale_late_conflicting_and_wide_quotes():
    df = _frame(breakout=UP)
    assert _reason(_run(df), EVENT_BAR) == "quotes_unavailable"
    for quotes, why in ((_quotes(skip={EVENT_BAR + 1}), "quote_missing"),
                        (_quotes(age_ms=61_000), "quote_missing"),
                        (_quotes(late={EVENT_BAR + 1}), "quote_missing"),
                        (_quotes(conflict={EVENT_BAR + 1}), "quote_conflict"),
                        (_quotes(spread=0.0006), "spread_too_wide")):
        out = _run(df, quotes=quotes)
        assert _reason(out, EVENT_BAR) == why and out["signal"].iloc[EVENT_BAR] == 0
    edge = _run(df, quotes=_quotes(spread=0.0005), params={"max_spread_frac": 0.000500125})
    assert edge["signal"].iloc[EVENT_BAR] == 1
    ungated = _run(df, params={"spread_gate": False})
    assert ungated["signal"].iloc[EVENT_BAR] == 1 and not ungated["meb_spread_gate"].iloc[0]
    malformed = _run(df, quotes=_quotes(), params={"quote_sha256": "3" * 64, "quote_path": "/x.json"})
    assert _reason(malformed, EVENT_BAR) == "quotes_malformed"


def test_decisions_ignore_future_bars_and_truncated_frames_agree():
    quotes = _quotes()
    df = _frame(breakout=UP)
    full = _run(df, quotes=quotes)
    for cut in (EVENT_BAR, EVENT_BAR + 1, EVENT_BAR + 3, N - 1):
        part = _run(df.iloc[:cut], quotes=quotes)
        assert (part["signal"].to_numpy() == full["signal"].to_numpy()[:cut]).all()
        perturbed = df.copy()
        perturbed.iloc[cut:, perturbed.columns.get_loc("close")] = 90.0
        assert (_run(perturbed, quotes=quotes)["signal"].to_numpy()[:cut] == full["signal"].to_numpy()[:cut]).all()
    support = 24 + 4 + 6 + 1
    end = EVENT_BAR + 1
    tail = _run(df.iloc[end - support:end], quotes=quotes)
    assert tail["signal"].iloc[-1] == full["signal"].iloc[end - 1]


def test_check_time_cutoff_holds_forming_and_stale_bars_and_carries_a_fresh_decision():
    quotes = _quotes()
    df = _frame(breakout=UP).iloc[:EVENT_BAR + 2]
    cutoff = RELEASE + H + 10_000
    out = _run(df, quotes=quotes, cutoff=cutoff)
    assert out["meb_carried"].iloc[-1] and out["signal"].iloc[-1] == 1
    timed = df.iloc[:EVENT_BAR + 1].copy()
    timed.attrs["decision_time"] = {"cutoff_ms": RELEASE + 2 * H + 1, "boundary_ms": RELEASE + H}
    assert _reason(_run(timed, quotes=quotes), EVENT_BAR) == "decision_bar_stale"
    untimed = df.copy()
    untimed.attrs["decision_time"] = {}
    assert (_reason(_reg().apply_strategy(NAME, untimed, dict(SEED)), 0)) == "decision_time_missing"


def test_simulator_fills_at_the_next_bar_open_and_time_stop_owns_the_holding_limit():
    df = _frame(breakout=UP)
    sig = ensure_atr_indicator(_run(df, quotes=_quotes()))
    bt = Backtester(initial_capital=1000.0, platform="hyperliquid",
                    open_strategy={"name": NAME, "params": dict(SEED)},
                    close_strategies=[{"name": "time_stop", "params": {"max_bars": SEED["holding_bars"]}}],
                    stop_loss_atr_mult=5.0, direction="both", comparison_mode="approximate")
    res = bt.run(sig, strategy_name=NAME, symbol="BTC/USDT", timeframe="1h", save=False)
    assert len(res["trades"]) == 1
    t = res["trades"][0]
    assert t["side"] == "long" and pd.Timestamp(t["entry_date"]) == df.index[EVENT_BAR + 1]
    assert t["exit_reason"] == f"time_stop:{SEED['holding_bars']}"


def test_composer_keeps_the_close_owner_when_entry_inputs_are_missing_and_admission_refuses_every_mode():
    reg = _reg()
    df = _frame(breakout=UP).iloc[:EVENT_BAR + 1]
    params = dict(SEED)
    ctx = {"side": "long", "avg_cost": 100.0, "current_quantity": 1.0, "initial_quantity": 1.0, "bars_held": 6}
    held = evaluate_open_close(reg.apply_strategy, reg.get_strategy, df, NAME, None, ["time_stop"], "long",
                               params=params, position_ctx=ctx, close_evaluate=close_evaluate,
                               close_params_by_name={"time_stop": {"max_bars": 6}})
    assert held.open_signal == 0 and held.open_result_df["meb_reason"].iloc[-1] == "calendar_unavailable"
    out = finalize_decision(held, "long")
    assert out["close_fraction"] == 1.0 and out["close_strategy"] == "time_stop"
    for positional, open_name, closes, mode, ack, role in (
            (NAME, None, None, GateMode(GATE_MODE_PAPER), Acknowledgement(False), "open"),
            (NAME, None, None, GateMode(GATE_MODE_LIVE), Acknowledgement(True), "open"),
            (NAME, None, None, GateMode(GATE_MODE_MISSING), Acknowledgement(True), "open"),
            ("breakout", None, [NAME], GateMode(GATE_MODE_PAPER), Acknowledgement(True), "close")):
        with pytest.raises(ValueError, match=f"{role} strategy '{NAME}' is production_refused"):
            admit_configured_strategies(positional, open_name, closes, mode, ack, reg.get_strategy, get_close)


def _write_gz_csv(df, path):
    buf = io.BytesIO()
    with gzip.GzipFile(filename="", mode="wb", fileobj=buf, mtime=0) as gz:
        gz.write(df.to_csv(index=False, lineterminator="\n").encode())
    with open(path, "wb") as fh:
        fh.write(buf.getvalue())


@pytest.fixture(scope="module")
def research_inputs(tmp_path_factory):
    import storage
    root = tmp_path_factory.mktemp("macro_inputs")
    df = _frame(breakout=UP)
    rows = df.reset_index(drop=True)
    rows.insert(0, "timestamp", df.index.as_unit("ms").asi8.astype("int64"))
    db = str(root / "ohlcv.db")
    storage.init_db(db)
    storage.store_ohlcv(rows, "binanceus", "BTC/USDT", "1h", db_path=db)
    cal_path = str(root / "cal.json")
    cal_sha = mi.write_artifact(cal_path, _calendar([_event()])[0])
    q_path = str(root / "quotes.json")
    q_sha = mi.write_artifact(q_path, _quotes())
    _write_gz_csv(rows[["timestamp", "open", "high", "low", "close", "volume"]], str(root / "BTC_1h.csv.gz"))
    with open(root / "meta.json", "w") as fh:
        json.dump({"universe": [{"name": "BTC", "szDecimals": 5, "maxLeverage": 40}]}, fh)
    manifest = {
        "schema": om.SCHEMA_V4, "study": "macro_fixture", "venue": "hyperliquid",
        "provenance": {"kind": "venue", "label": "fixture candles"}, "interval": "1h", "warmup_bars": 40,
        "windows": {"test": {"start": "2026-01-07", "end": "2026-01-11"}},
        "costs": {"taker_fee_pct": 0.00045, "maker_fee_pct": 0.00015, "slippage_bps": 5.0,
                  "min_notional_usd": 10.0, "min_notional_margin": 0.03},
        "venue_reference": {"meta": {"path": "meta.json", "sha256": om.sha256_file(str(root / "meta.json"))}},
        "datasets": [{"coin": "BTC", "symbol": "BTC/USDT", "size_decimals": 5, "half_spread_bps": 0.1,
                      "candles": {"path": "BTC_1h.csv.gz", "sha256": om.sha256_file(str(root / "BTC_1h.csv.gz"))},
                      "macro_calendar": {"path": "cal.json", "sha256": cal_sha},
                      "macro_quotes": {"path": "quotes.json", "sha256": q_sha}}],
    }
    with open(root / "manifest.json", "w") as fh:
        json.dump(manifest, fh)
    env = dict(os.environ, GO_TRADER_OHLCV_CACHE_DB=db)
    params = {**SEED, "calendar_path": cal_path, "calendar_sha256": cal_sha, "quote_path": q_path,
              "quote_sha256": q_sha}
    return {"root": root, "env": env, "params": params, "manifest": str(root / "manifest.json"), "df": df}


def test_cached_single_run_attaches_the_pinned_artifacts(research_inputs):
    proc = subprocess.run([sys.executable, "-c", RUN_BACKTEST_SNIPPET, json.dumps(research_inputs["params"])],
                          cwd=_REPO, env=research_inputs["env"], capture_output=True, text=True, timeout=600)
    assert proc.returncode == 0, proc.stderr[-3000:] + proc.stdout[-3000:]
    assert "Macro-event inputs:" in proc.stdout
    trades = json.loads(next(l for l in proc.stdout.splitlines() if l.startswith("RESULT")).split(" ", 1)[1])
    assert trades == [[str(research_inputs["df"].index[EVENT_BAR + 1]), "long", "time_stop:6"]]
    bad = dict(research_inputs["params"], calendar_sha256="f" * 64)
    proc = subprocess.run([sys.executable, "-c", RUN_BACKTEST_SNIPPET, json.dumps(bad)], cwd=_REPO,
                          env=research_inputs["env"], capture_output=True, text=True, timeout=600)
    assert proc.returncode != 0 and "sha256" in (proc.stderr + proc.stdout)


def test_cached_m1_and_frozen_manifest_legs_agree_under_equal_inputs(research_inputs):
    snippet = (
        "import json, sys; sys.path[:0] = ['backtest', 'shared_tools']\n"
        "import eval_windows as ew, offline_manifest as om\n"
        "from registry_loader import load_registry\n"
        "args = json.loads(sys.argv[1])\n"
        "reg = load_registry('futures')\n"
        "cand = {'name': 'macro_event_breakout', 'params': args['params'], 'direction': 'both',\n"
        "        'close_strategies': [{'name': 'time_stop', 'params': {'max_bars': 6}}],\n"
        "        'stop_loss_atr_mult': 5.0, 'comparison_mode': 'approximate'}\n"
        "ew.validate_candidate(cand)\n"
        "out = {}\n"
        "cap = []\n"
        "import backtester\n"
        "base = backtester.Backtester\n"
        "class B(base):\n"
        "    def run(self, *a, **k):\n"
        "        r = super().run(*a, **k); cap.append(r); return r\n"
        "backtester.Backtester = B\n"
        "leg = ew.run_candidate_leg(reg, cand, 'BTC/USDT', '1h', ('2026-01-07', '2026-01-11'), warmup_bars=40,\n"
        "                           funding_mode='off')\n"
        "out['cached'] = [[t['entry_date'], t['side'], t['exit_reason']] for t in cap[-1]['trades']]\n"
        "m = om.load_manifest(args['manifest'])\n"
        "ctx = {'manifest': m, 'window': 'test', 'cost_multiplier': 1.0}\n"
        "leg = ew.run_candidate_leg(reg, cand, 'BTC', '1h', (None, None), manifest_ctx=ctx)\n"
        "out['manifest'] = [[t['entry_date'], t['side'], t['exit_reason']] for t in cap[-1]['trades']]\n"
        "out['macro'] = leg['manifest']['macro_event_coverage']['quote_records']\n"
        "print('RESULT', json.dumps(out))\n"
    )
    payload = json.dumps({"params": research_inputs["params"], "manifest": research_inputs["manifest"]})
    proc = subprocess.run([sys.executable, "-c", snippet, payload], cwd=_REPO, env=research_inputs["env"],
                          capture_output=True, text=True, timeout=600)
    assert proc.returncode == 0, proc.stderr[-3000:] + proc.stdout[-3000:]
    out = json.loads(next(l for l in proc.stdout.splitlines() if l.startswith("RESULT")).split(" ", 1)[1])
    want = [[str(research_inputs["df"].index[EVENT_BAR + 1]), "long", "time_stop:6"]]
    assert out["cached"] == want and out["manifest"] == want and out["macro"] == N + 1


def test_manifest_v4_contract_refuses_old_schemas_and_mismatched_pins(research_inputs, tmp_path):
    with open(research_inputs["manifest"]) as fh:
        raw = json.load(fh)
    old = dict(raw, schema=om.SCHEMA_V3)
    path = str(research_inputs["root"] / "old.json")
    with open(path, "w") as fh:
        json.dump(old, fh)
    with pytest.raises(om.ManifestError, match="macro_calendar and macro_quotes need schema"):
        om.load_manifest(path)
    m = om.load_manifest(research_inputs["manifest"])
    ds = m["datasets"][0]
    win = m["windows"]["test"]
    inputs, cov = om.attach_macro_events(m, ds, win, research_inputs["params"])
    assert inputs["event_records"]["available"] and cov["calendar"]["eligible_events"] == 1
    with pytest.raises(om.ManifestError, match="pins calendar"):
        om.attach_macro_events(m, ds, win, dict(research_inputs["params"], calendar_sha256="e" * 64))
    quotes_only = copy.deepcopy(raw)
    del quotes_only["datasets"][0]["macro_calendar"]
    path = str(research_inputs["root"] / "quotes_only.json")
    with open(path, "w") as fh:
        json.dump(quotes_only, fh)
    with pytest.raises(om.ManifestError, match="macro_quotes needs a macro_calendar"):
        om.load_manifest(path)

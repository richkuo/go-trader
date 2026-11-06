import json
import os
import subprocess
import sys
from datetime import datetime, timezone

_ROOT = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
_OFFLINE = os.path.join(_ROOT, "shared_scripts", "testdata", "candle_range_offline")
H = 3_600_000
T0 = int(datetime(2026, 10, 1, 0, 0, tzinfo=timezone.utc).timestamp() * 1000)


def _bars(start, count, step=H):
    rows = []
    for i in range(count):
        p = 100.0 + i
        rows.append([start + i * step, p, p + 1.0, p - 1.0, p + 0.5, 10.0 + i])
    return rows


def _hl_rows(rows, step=H):
    return [{"t": r[0], "T": r[0] + step - 1, "s": "ETH", "i": "1h",
             "o": str(r[1]), "h": str(r[2]), "l": str(r[3]), "c": str(r[4]), "v": str(r[5]), "n": 1} for r in rows]


def _run(tmp_path, fixture, argv, script=None):
    log = tmp_path / "calls.log"
    fixture = dict(fixture, log=str(log))
    path = tmp_path / "fixture.json"
    path.write_text(json.dumps(fixture))
    env = dict(os.environ)
    env.update({"PYTHONPATH": _OFFLINE, "CANDLE_RANGE_FIXTURE": str(path), "ROBINHOOD_USERNAME": "",
                "HYPERLIQUID_SECRET_KEY": "", "GO_TRADER_HL_OHLCV_CACHE": "0"})
    cmd = [sys.executable] + (["-c", script] if script else [os.path.join(_ROOT, "shared_scripts", "fetch_candles.py")] + argv)
    proc = subprocess.run(cmd, capture_output=True, text=True, cwd=_ROOT, env=env, timeout=240)
    assert "network blocked by the candle range fixture" not in proc.stderr, proc.stderr
    calls = [json.loads(line) for line in log.read_text().splitlines()] if log.exists() else []
    return proc, calls


def _out(proc):
    return json.loads(proc.stdout)


def _iso(ms):
    return datetime.fromtimestamp(ms / 1000, tz=timezone.utc).isoformat().replace("+00:00", "Z")


def test_hyperliquid_range_pages_with_opening_times_inside_exact_intraday_bounds(tmp_path):
    rows = _bars(T0, 30)
    start = T0 + 5 * H + 30 * 60_000
    end = T0 + 20 * H + 15 * 60_000
    proc, calls = _run(tmp_path, {"hl": _hl_rows(rows), "hl_page_cap": 4},
                       ["--platform", "hyperliquid", "--type", "perps", "--symbol", "ETH", "--timeframe", "1h",
                        "--from", _iso(start), "--to", _iso(end), "--limit", "50"])
    assert proc.returncode == 0, proc.stdout + proc.stderr
    out = _out(proc)
    assert out["source"] == "hyperliquid:adapter:range"
    times = [c["time"] * 1000 for c in out["candles"]]
    assert times == [T0 + i * H for i in range(6, 21)]
    assert out["range"]["timestamp"] == "open"
    assert out["range"]["from_ms"] == start and out["range"]["to_ms"] == end
    assert out["range"]["truncated"] is False
    hl_calls = [c for c in calls if c[0] == "hl"]
    assert hl_calls[0][3] == start
    assert [c[3] for c in hl_calls] == sorted(c[3] for c in hl_calls)
    assert len(hl_calls) == out["range"]["requests"] >= 4


def test_hyperliquid_range_stops_on_a_stalled_page(tmp_path):
    rows = _bars(T0, 10)
    proc, calls = _run(tmp_path, {"hl": _hl_rows(rows), "hl_stall": True},
                       ["--platform", "hyperliquid", "--type", "perps", "--symbol", "ETH", "--timeframe", "1h",
                        "--from", _iso(T0 + 5 * H), "--to", _iso(T0 + 9 * H)])
    assert proc.returncode == 0, proc.stdout + proc.stderr
    out = _out(proc)
    assert out["candles"] == []
    assert out["range"]["stalled"] is True
    assert len([c for c in calls if c[0] == "hl"]) == 1


def test_ccxt_range_pages_drops_invalid_rows_and_terminates(tmp_path):
    rows = _bars(T0, 9)
    rows[4] = [rows[4][0], 100.0, 99.0, 101.0, 100.0, 1.0]
    proc, calls = _run(tmp_path, {"ccxt": {"BTC/USDT|1h": rows}, "ccxt_page_cap": 2},
                       ["--platform", "binanceus", "--type", "spot", "--symbol", "BTC/USDT", "--timeframe", "1h",
                        "--from", _iso(T0), "--to", _iso(T0 + 8 * H)])
    assert proc.returncode == 0, proc.stdout + proc.stderr
    out = _out(proc)
    assert out["source"] == "binanceus:ccxt:range"
    assert [c["time"] * 1000 for c in out["candles"]] == [T0 + i * H for i in range(9) if i != 4]
    assert out["range"]["invalid_rows"] == 1
    ccxt_calls = [c for c in calls if c[0] == "ccxt"]
    assert [c[4] for c in ccxt_calls] == [T0, T0 + 2 * H, T0 + 4 * H, T0 + 6 * H, T0 + 8 * H]
    assert out["range"]["requests"] == len(ccxt_calls)


def test_ccxt_range_stall_and_request_budget_end_the_loop(tmp_path):
    rows = _bars(T0, 4)
    proc, calls = _run(tmp_path, {"ccxt": {"BTC/USDT|1h": rows}, "ccxt_stall": True},
                       ["--platform", "binanceus", "--type", "spot", "--symbol", "BTC/USDT", "--timeframe", "1h",
                        "--from", _iso(T0 + 3 * H), "--to", _iso(T0 + 3 * H)])
    assert proc.returncode == 0, proc.stdout + proc.stderr
    assert _out(proc)["range"]["stalled"] is True
    assert len([c for c in calls if c[0] == "ccxt"]) == 1

    rows = _bars(T0, 400)
    run_dir = tmp_path / "budget"
    run_dir.mkdir()
    proc, calls = _run(run_dir, {"ccxt": {"BTC/USDT|1h": rows}, "ccxt_page_cap": 1},
                       ["--platform", "binanceus", "--type", "spot", "--symbol", "BTC/USDT", "--timeframe", "1h",
                        "--from", _iso(T0), "--to", _iso(T0 + 399 * H)])
    assert proc.returncode == 0, proc.stdout + proc.stderr
    out = _out(proc)
    assert out["range"]["truncated"] is True
    assert len([c for c in calls if c[0] == "ccxt"]) == out["range"]["requests"] == 5


def test_without_from_the_latest_window_request_is_unchanged(tmp_path):
    rows = _bars(T0, 20)
    proc, calls = _run(tmp_path, {"ccxt": {"BTC/USDT|1h": rows}},
                       ["--platform", "binanceus", "--type", "spot", "--symbol", "BTC/USDT", "--timeframe", "1h",
                        "--limit", "7"])
    assert proc.returncode == 0, proc.stdout + proc.stderr
    out = _out(proc)
    assert "range" not in out
    assert out["source"] == "binanceus:ccxt"
    assert [c for c in calls if c[0] == "ccxt"] == [["ccxt", "binanceus", "BTC/USDT", "1h", None, 7]]
    assert len(out["candles"]) == 7


def test_robinhood_range_returns_latest_bars_marked_unsupported(tmp_path):
    rows = _bars(T0, 6)
    proc, _ = _run(tmp_path, {"yahoo": {"BTC-USD|1h": rows}},
                   ["--platform", "robinhood", "--type", "spot", "--symbol", "BTC", "--timeframe", "1h",
                    "--from", _iso(T0 + H), "--to", _iso(T0 + 4 * H)])
    assert proc.returncode == 0, proc.stdout + proc.stderr
    out = _out(proc)
    assert out["source"] == "robinhood:adapter:latest"
    assert out["range"]["supported"] is False
    assert out["range"]["timestamp"] == "unverified"
    assert [c["time"] * 1000 for c in out["candles"]] == [T0 + i * H for i in range(1, 5)]


def test_invalid_or_reversed_bounds_exit_one(tmp_path):
    for i, extra in enumerate((["--from", "yesterday"], ["--from", _iso(T0 + 2 * H), "--to", _iso(T0)],
                               ["--from", "2026-10-01T00:00:00"])):
        run_dir = tmp_path / str(i)
        run_dir.mkdir()
        proc, calls = _run(run_dir, {"ccxt": {}},
                           ["--platform", "binanceus", "--symbol", "BTC/USDT", "--timeframe", "1h"] + extra)
        assert proc.returncode == 1
        assert _out(proc)["source"] == "error"
        assert calls == []


def test_date_only_since_keeps_its_midnight_meaning(tmp_path):
    rows = _bars(T0, 3)
    script = ("import sys; sys.path.insert(0, 'shared_tools'); import data_fetcher; "
              "df = data_fetcher.fetch_ohlcv('BTC/USDT', '1h', since='2026-10-01', limit=3, store=False); "
              "print(len(df))")
    proc, calls = _run(tmp_path, {"ccxt": {"BTC/USDT|1h": rows}}, [], script=script)
    assert proc.returncode == 0, proc.stdout + proc.stderr
    assert proc.stdout.strip() == "3"
    assert [c for c in calls if c[0] == "ccxt"] == [["ccxt", "binanceus", "BTC/USDT", "1h", T0, 3]]

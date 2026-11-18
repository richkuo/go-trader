import json
import os
import subprocess
import sys
import time

import pytest

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
RH_FIXTURE = os.path.join(ROOT, "shared_scripts", "testdata", "robinhood_offline")
HTF_FIXTURE = os.path.join(ROOT, "shared_scripts", "testdata", "htf_offline")
H = 3_600_000
SPEC = '{"default":{"classifier":"adx","period":14,"adx_threshold":20}}'



def _crypto_map():
    out = subprocess.run([sys.executable, "-c", "import json, sys; sys.path.insert(0, 'platforms/robinhood'); "
                          "sys.path.insert(0, 'shared_tools'); from adapter import YAHOO_CRYPTO_MAP; "
                          "print(json.dumps(YAHOO_CRYPTO_MAP))"], capture_output=True, text=True, cwd=ROOT, check=True)
    return json.loads(out.stdout)


YAHOO_CRYPTO_MAP = _crypto_map()

SUFFIXED = sorted(k for k, v in YAHOO_CRYPTO_MAP.items() if v != k + "-USD")

QUOTE_DRIVER = """
import json, sys
sys.path.insert(0, "platforms/robinhood")
sys.path.insert(0, "shared_tools")
from adapter import YAHOO_CRYPTO_MAP, RobinhoodExchangeAdapter
adapter = RobinhoodExchangeAdapter(mode="paper", asset_class="crypto")
out = {"logged_in": adapter.quotes_authenticated, "prices": {}}
for code in sorted(YAHOO_CRYPTO_MAP):
    out["prices"][code] = adapter.get_price(code)
sys.__stdout__.write(json.dumps(out) + "\\n")
"""


def _lines(path):
    if not os.path.exists(path):
        return []
    with open(path) as fh:
        return [json.loads(line) for line in fh if line.strip()]


def _rh_env(tmp_path, routes):
    routes_path = tmp_path / "routes.json"
    routes_path.write_text(json.dumps(routes))
    env = {k: v for k, v in os.environ.items() if not k.startswith(("ROBINHOOD", "RH_OFFLINE", "HTF_OFFLINE"))}
    env.update({
        "HOME": str(tmp_path),
        "PYTHONPATH": RH_FIXTURE,
        "RH_OFFLINE_STUB_MODE": "routes",
        "RH_OFFLINE_STUB_ROUTES": str(routes_path),
        "RH_OFFLINE_STUB_LOG": str(tmp_path / "requests.jsonl"),
        "RH_OFFLINE_STUB_YAHOO_LOG": str(tmp_path / "yahoo.jsonl"),
        "ROBINHOOD_USERNAME": "offline-user",
        "ROBINHOOD_PASSWORD": "offline-pass",
        "ROBINHOOD_TOTP_SECRET": "JBSWY3DPEHPK3PXP",
        "GO_TRADER_ROBINHOOD_SESSION_DIR": str(tmp_path / "robinhood-session"),
    })
    return env


def _pairs():
    return [{"id": "pair-" + code, "symbol": code + "-USD", "asset_currency": {"code": code},
             "quote_currency": {"code": "USD"}, "tradability": "tradable"} for code in sorted(YAHOO_CRYPTO_MAP)]


def _quote_routes(quote):
    return {
        "GET https://nummus.robinhood.com/currency_pairs/": [{"json": {"next": None, "results": _pairs()}}],
        "GET https://api.robinhood.com/marketdata/forex/quotes/*": [quote],
    }


def _one_doc(proc):
    lines = [ln for ln in proc.stdout.splitlines() if ln.strip()]
    assert len(lines) == 1, f"stdout must be one JSON document:\n{proc.stdout}\nstderr:\n{proc.stderr}"
    return json.loads(lines[0])


def test_an_unsupported_coin_signal_run_fails_before_login_quote_or_yahoo(tmp_path, subtests):
    for symbol in ("AAPL", "SPY", "XRP-USD", "BTC/USD", "MATIC", "xrp"):
        with subtests.test(symbol=symbol):
            run_dir = tmp_path / symbol.replace("/", "_")
            run_dir.mkdir()
            env = _rh_env(run_dir, {})
            proc = subprocess.run([sys.executable, os.path.join(ROOT, "shared_scripts", "check_robinhood.py"),
                                   "momentum_pro", symbol, "1h", "--mode=paper"],
                                  capture_output=True, text=True, cwd=ROOT, env=env, timeout=180)
            doc = _one_doc(proc)
            assert proc.returncode == 1, proc.stderr
            assert doc["error"].startswith(f"unsupported Robinhood crypto coin {symbol!r}"), doc
            assert doc["signal"] == 0 and doc["price"] == 0, doc
            assert "Starting login process..." not in proc.stderr, proc.stderr
            assert _lines(env["RH_OFFLINE_STUB_LOG"]) == []
            assert _lines(env["RH_OFFLINE_STUB_YAHOO_LOG"]) == []


def test_every_table_key_quotes_on_the_crypto_route_and_falls_back_to_its_verified_ticker(tmp_path):
    for name, quote in (("quote", {"json": {"id": "pair", "mark_price": "123.45"}}),
                        ("fallback", {"status": 503, "json": {"detail": "busy"}})):
        run_dir = tmp_path / name
        run_dir.mkdir()
        env = _rh_env(run_dir, _quote_routes(quote))
        proc = subprocess.run([sys.executable, "-c", QUOTE_DRIVER], capture_output=True, text=True, cwd=ROOT, env=env,
                              timeout=600)
        assert proc.returncode == 0, proc.stderr
        out = json.loads(proc.stdout.strip().splitlines()[-1])
        assert out["logged_in"] is True, proc.stderr
        made = _lines(env["RH_OFFLINE_STUB_LOG"])
        urls = [r["url"].split("?", 1)[0] for r in made if "/oauth2/token/" not in r["url"]]
        assert urls, made
        for url in urls:
            assert url == "https://nummus.robinhood.com/currency_pairs/" or \
                url.startswith("https://api.robinhood.com/marketdata/forex/quotes/pair-"), made
        quoted = sorted({u.rsplit("/", 2)[-2] for u in urls if "marketdata/forex/quotes" in u})
        assert quoted == ["pair-" + code for code in sorted(YAHOO_CRYPTO_MAP)]
        yahoo = _lines(env["RH_OFFLINE_STUB_YAHOO_LOG"])
        if name == "quote":
            assert out["prices"] == {code: 123.45 for code in YAHOO_CRYPTO_MAP}
            assert yahoo == []
        else:
            assert [r["yahoo"] for r in yahoo] == [YAHOO_CRYPTO_MAP[code] for code in sorted(YAHOO_CRYPTO_MAP)]
            assert all(r["period"] == "1d" for r in yahoo), yahoo
            assert all(p > 0 for p in out["prices"].values()), out


def _now_hour():
    now = int(time.time() * 1000)
    return now - now % H


def _hourly(start, end, base=100.0):
    rows = []
    t, i = start, 0
    while t <= end:
        p = base + (i % 7) * 0.5 - (i % 3) * 0.25
        rows.append([t, p - 0.1, p + 0.6, p - 0.7, p, 10.0 + i])
        t += H
        i += 1
    return rows


def _htf_env(fixture_path):
    env = {k: v for k, v in os.environ.items() if not k.startswith(("ROBINHOOD", "RH_OFFLINE"))}
    env.update({"PYTHONPATH": HTF_FIXTURE, "HTF_OFFLINE_FIXTURE": str(fixture_path), "ROBINHOOD_USERNAME": ""})
    return env


def _settled_hour():
    now = int(time.time() * 1000)
    if now % H < 30_000:
        time.sleep((30_000 - now % H) / 1000)
    return _now_hour()


@pytest.mark.parametrize("code", sorted(set(SUFFIXED) | {"BTC", "LTC", "XRP"}))
def test_a_signal_check_reads_only_the_verified_ticker_for_every_candle_and_price_read(tmp_path, code):
    ticker = YAHOO_CRYPTO_MAP[code]
    for attempt in range(3):
        now = _settled_hour()
        run_dir = tmp_path / f"attempt-{attempt}"
        run_dir.mkdir()
        fixture = run_dir / "fixture.json"
        fixture.write_text(json.dumps({
            "yahoo": {ticker + "|1h": _hourly(now - 1200 * H, now)},
            "yahoo_price": {ticker: 101.5},
            "yahoo_log": str(run_dir / "yahoo.log"),
            "yahoo_price_log": str(run_dir / "price.log"),
            "ccxt_log": str(run_dir / "ccxt.log"),
        }))
        proc = subprocess.run([sys.executable, os.path.join(ROOT, "shared_scripts", "check_robinhood.py"),
                               "atr_breakout", code, "1h", "--mode=paper", "--closed-bar-decisions", "--htf-filter",
                               "--regime-enabled", "--regime-windows-spec-json", SPEC,
                               "--decision-regime-timeframe", "4h"],
                              capture_output=True, text=True, cwd=ROOT, env=_htf_env(fixture), timeout=240)
        if _now_hour() == now:
            break
    else:
        pytest.fail("each closed-bar run crossed an hour boundary")
    doc = _one_doc(proc)
    assert proc.returncode == 0, proc.stdout + proc.stderr
    assert doc["closed_bar_decision"]["held"] is False, doc
    assert doc["htf_filter"]["status"] == "ok", doc
    assert doc["price"] == 101.5, doc
    candles = _lines(str(run_dir / "yahoo.log"))
    assert [c[0] for c in candles] == [ticker] * 3, candles
    assert sorted(c[1] for c in candles) == ["1h", "1h", "1h"], candles
    assert _lines(str(run_dir / "price.log")) == [[ticker, "1d", "1d"]]
    assert _lines(str(run_dir / "ccxt.log")) == []


def _regime(tmp_path, argv, yahoo=None):
    fixture = tmp_path / "fixture.json"
    fixture.write_text(json.dumps({"yahoo": yahoo or {}, "yahoo_log": str(tmp_path / "yahoo.log"),
                                   "ccxt_log": str(tmp_path / "ccxt.log")}))
    proc = subprocess.run([sys.executable, os.path.join(ROOT, "shared_scripts", "check_regime.py"),
                           "--timeframe", "4h", "--regime-windows-spec-json", SPEC, "--ohlcv-limit", "100"] + argv,
                          capture_output=True, text=True, cwd=ROOT, env=_htf_env(fixture), timeout=240)
    return proc, _lines(str(tmp_path / "yahoo.log")), _lines(str(tmp_path / "ccxt.log"))


def test_crypto_and_stock_regime_reads_for_one_symbol_stay_separate(tmp_path):
    now = _now_hour()
    four_h = [r for r in _hourly(now - 2400 * H, now) if r[0] % (4 * H) == 0]
    yahoo = {"LTC-USD|1h": _hourly(now - 1200 * H, now), "LTC|4h": four_h}
    for asset_class, want in (("crypto", [["LTC-USD", "1h", "1mo"]]), ("stock", [["LTC", "4h", "1y"]])):
        run_dir = tmp_path / asset_class
        run_dir.mkdir()
        proc, candles, ccxt = _regime(run_dir, ["--platform", "robinhood", "--symbol", "LTC", "--asset-class", asset_class], yahoo)
        doc = _one_doc(proc)
        assert proc.returncode == 0, proc.stdout + proc.stderr
        assert doc["asset_class"] == asset_class and doc["symbol"] == "LTC", doc
        assert doc["regime"]["default"]["regime"], doc
        assert candles == want
        assert ccxt == []


def test_regime_class_refusals_run_before_the_probe_return_and_any_read(tmp_path, subtests):
    cases = [
        ("missing", ["--platform", "robinhood", "--symbol", "BTC"], "--asset-class must be one of crypto, stock for platform robinhood, got ''"),
        ("invalid", ["--platform", "robinhood", "--symbol", "BTC", "--asset-class", "equity"], "got 'equity'"),
        ("misplaced", ["--platform", "binanceus", "--symbol", "BTC/USDT", "--asset-class", "crypto"],
         "--asset-class is only for platform robinhood, not 'binanceus'"),
        ("unsupported_coin", ["--platform", "robinhood", "--symbol", "AAPL", "--asset-class", "crypto"],
         "unsupported Robinhood crypto coin 'AAPL'"),
        ("pair_as_stock", ["--platform", "robinhood", "--symbol", "LTC-USD", "--asset-class", "stock"],
         "Robinhood stock underlying 'LTC-USD' is a currency pair"),
    ]
    for name, argv, want in cases:
        for probe in (False, True):
            with subtests.test(case=name, probe=probe):
                run_dir = tmp_path / f"{name}-{probe}"
                run_dir.mkdir()
                proc, candles, ccxt = _regime(run_dir, argv + (["--probe-only"] if probe else []))
                if probe and name in ("unsupported_coin", "pair_as_stock"):
                    assert proc.returncode == 0, proc.stdout + proc.stderr
                    assert proc.stdout == ""
                else:
                    doc = _one_doc(proc)
                    assert proc.returncode == 1
                    assert want in doc["error"], doc
                    assert doc["regime"] is None
                assert candles == [] and ccxt == []


def test_robinhood_regime_read_failures_never_retry_on_binanceus(tmp_path, subtests):
    for name, yahoo in (("empty", {}), ("error", {"BTC-USD|1h": "network_error"})):
        with subtests.test(case=name):
            run_dir = tmp_path / name
            run_dir.mkdir()
            proc, candles, ccxt = _regime(run_dir, ["--platform", "robinhood", "--symbol", "BTC", "--asset-class", "crypto",
                                                    "--allow-spot-fallback"], yahoo)
            doc = _one_doc(proc)
            assert proc.returncode == 1
            assert doc["regime"] is None and doc["asset_class"] == "crypto", doc
            assert candles == [["BTC-USD", "1h", "1mo"]]
            assert ccxt == []


def test_robinhood_options_check_candle_failure_never_retries_on_binanceus(tmp_path):
    fixture = tmp_path / "fixture.json"
    fixture.write_text(json.dumps({"yahoo": {}, "yahoo_price": {"SPY": 450.0}, "yahoo_log": str(tmp_path / "yahoo.log"),
                                   "ccxt_log": str(tmp_path / "ccxt.log")}))
    proc = subprocess.run([sys.executable, os.path.join(ROOT, "shared_scripts", "check_options.py"),
                           "momentum_options", "SPY", "--platform=robinhood", "[]"],
                          capture_output=True, text=True, cwd=ROOT, env=_htf_env(fixture), timeout=240)
    doc = _one_doc(proc)
    assert proc.returncode == 1, proc.stdout + proc.stderr
    assert doc["error"].startswith("momentum bars unavailable"), doc
    requested = _lines(str(tmp_path / "yahoo.log"))
    assert {r[0] for r in requested} == {"SPY"}, requested
    assert ["SPY", "4h", "1y"] in requested, requested
    assert _lines(str(tmp_path / "ccxt.log")) == []


LLM_DRIVER = """
import json, sys
sys.path.insert(0, "shared_scripts")
sys.path.insert(0, "shared_tools")
import llm_review
out = llm_review.gather_market_context(json.loads(sys.argv[1]))
sys.__stdout__.write(json.dumps({"ohlcv": out["ohlcv_summary"] is not None}) + "\\n")
"""


def test_llm_entry_review_reads_robinhood_data_only_through_the_declared_class(tmp_path, subtests):
    now = _now_hour()
    yahoo = {"LTC-USD|1h": _hourly(now - 400 * H, now), "LTC|1h": _hourly(now - 400 * H, now)}
    for name, ctx, want_ohlcv, want_requests in (
        ("spot_coin", {"platform": "robinhood", "type": "spot", "symbol": "LTC", "timeframe": "1h"}, True, ["LTC-USD"]),
        ("options_stock", {"platform": "robinhood", "type": "options", "symbol": "LTC", "timeframe": "1h"}, True, ["LTC"]),
        ("unsupported_coin", {"platform": "robinhood", "type": "spot", "symbol": "AAPL", "timeframe": "1h"}, False, []),
        ("other_type", {"platform": "robinhood", "type": "perps", "symbol": "LTC", "timeframe": "1h"}, False, []),
    ):
        with subtests.test(case=name):
            run_dir = tmp_path / name
            run_dir.mkdir()
            fixture = run_dir / "fixture.json"
            fixture.write_text(json.dumps({"yahoo": yahoo, "yahoo_log": str(run_dir / "yahoo.log"),
                                           "ccxt_log": str(run_dir / "ccxt.log")}))
            proc = subprocess.run([sys.executable, "-c", LLM_DRIVER, json.dumps(ctx)], capture_output=True, text=True,
                                  cwd=ROOT, env=_htf_env(fixture), timeout=240)
            assert proc.returncode == 0, proc.stderr
            assert json.loads(proc.stdout.strip().splitlines()[-1]) == {"ohlcv": want_ohlcv}
            assert [r[0] for r in _lines(str(run_dir / "yahoo.log"))] == want_requests
            assert _lines(str(run_dir / "ccxt.log")) == []

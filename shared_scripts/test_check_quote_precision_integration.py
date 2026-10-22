import json
import os
import subprocess
import sys

import pytest

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
STUB_DIR = os.path.join(ROOT, "shared_scripts", "testdata", "quote_precision")
CHECK_STRATEGY = os.path.join(ROOT, "shared_scripts", "check_strategy.py")
CHECK_OKX = os.path.join(ROOT, "shared_scripts", "check_okx.py")
CHECK_ROBINHOOD = os.path.join(ROOT, "shared_scripts", "check_robinhood.py")
CHECK_TOPSTEP = os.path.join(ROOT, "shared_scripts", "check_topstep.py")
CHECK_PRICE = os.path.join(ROOT, "shared_scripts", "check_price.py")
CHECK_OPTIONS = os.path.join(ROOT, "shared_scripts", "check_options.py")

QUOTES = (0.1234, 0.006, 0.00001234)
MARKET_OPEN = "2026-10-21T12:00:00-04:00"
MARKET_CLOSED = "2026-10-24T12:00:00-04:00"
CREDENTIALS = (
    "OKX_API_KEY", "OKX_API_SECRET", "OKX_PASSPHRASE", "OKX_SANDBOX",
    "ROBINHOOD_USERNAME", "ROBINHOOD_PASSWORD", "ROBINHOOD_TOTP_SECRET",
    "TOPSTEP_API_KEY", "TOPSTEP_API_SECRET", "TOPSTEP_ACCOUNT_ID",
)


def _run(script, argv, env_extra, stdin=None, tmp_path=None):
    env = dict(os.environ)
    for key in CREDENTIALS:
        env.pop(key, None)
    for key in ("QP_CLOSE", "QP_QUOTE", "QP_SHAPE", "QP_NOW", "QP_TICKERS", "QP_CLOSES", "QP_CANDLES", "QP_OKX_FILL"):
        env.pop(key, None)
    env["QP_STUB"] = "1"
    env["PYTHONPATH"] = STUB_DIR
    if tmp_path is not None:
        env["HOME"] = str(tmp_path)
    env.update({k: str(v) for k, v in env_extra.items()})
    return subprocess.run(
        [sys.executable, script] + argv,
        input=stdin, capture_output=True, text=True, cwd=ROOT, env=env, timeout=180,
    )


def _single_document(proc):
    lines = [ln for ln in proc.stdout.splitlines() if ln.strip()]
    assert len(lines) == 1, f"stdout must be one JSON document:\n{proc.stdout}\nstderr:\n{proc.stderr}"
    return json.loads(lines[0])


CHECK_CASES = {
    "spot_candle_close": (CHECK_STRATEGY, ["sma_crossover", "BTC/USDT", "1h", "--mode=paper"], "close", {}),
    "okx_spot_adapter_quote": (CHECK_OKX, ["sma_crossover", "BTC", "1h", "--mode=paper", "--inst-type=spot"], "quote", {}),
    "okx_swap_adapter_quote": (CHECK_OKX, ["sma_crossover", "BTC", "1h", "--mode=paper", "--inst-type=swap"], "quote", {}),
    "okx_spot_candle_fallback_after_zero_quote": (CHECK_OKX, ["sma_crossover", "BTC", "1h", "--mode=paper", "--inst-type=spot"], "close", {"QP_QUOTE": "0"}),
    "okx_swap_candle_fallback_after_fetch_error": (CHECK_OKX, ["sma_crossover", "BTC", "1h", "--mode=paper", "--inst-type=swap"], "close", {"QP_QUOTE": "error"}),
    "robinhood_adapter_quote": (CHECK_ROBINHOOD, ["sma_crossover", "DOGE", "1h", "--mode=paper"], "quote", {}),
    "robinhood_candle_fallback_after_missing_quote": (CHECK_ROBINHOOD, ["sma_crossover", "DOGE", "1h", "--mode=paper"], "close", {}),
    "robinhood_candle_fallback_after_quote_error": (CHECK_ROBINHOOD, ["sma_crossover", "DOGE", "1h", "--mode=paper"], "close", {"QP_QUOTE": "error"}),
    "topstep_adapter_quote": (CHECK_TOPSTEP, ["sma_crossover", "MES", "1h", "--mode=paper"], "quote", {"QP_NOW": MARKET_OPEN}),
    "topstep_candle_fallback_after_missing_quote": (CHECK_TOPSTEP, ["sma_crossover", "MES", "1h", "--mode=paper"], "close", {"QP_NOW": MARKET_OPEN}),
}


@pytest.mark.parametrize("quote", QUOTES)
@pytest.mark.parametrize("case", sorted(CHECK_CASES))
def test_non_option_checks_emit_the_selected_quote_exactly(case, quote, tmp_path):
    script, argv, source, extra = CHECK_CASES[case]
    env = {"QP_SHAPE": "cross_up"}
    if source == "quote":
        env["QP_CLOSE"] = 0.5
        env["QP_QUOTE"] = repr(quote)
    else:
        env["QP_CLOSE"] = repr(quote)
    env.update(extra)
    proc = _run(script, argv, env, tmp_path=tmp_path)
    assert proc.returncode == 0, proc.stderr
    out = _single_document(proc)
    assert not out.get("error"), out
    assert out["signal"] == 1, out
    assert isinstance(out["price"], float)
    assert out["price"] == quote
    if script == CHECK_TOPSTEP:
        assert out["market_open"] is True


def test_topstep_closed_market_keeps_its_refusal(tmp_path):
    proc = _run(CHECK_TOPSTEP, ["sma_crossover", "MES", "1h", "--mode=paper"],
                {"QP_NOW": MARKET_CLOSED, "QP_CLOSE": 0.1234, "QP_QUOTE": 0.1234, "QP_SHAPE": "cross_up"}, tmp_path=tmp_path)
    assert proc.returncode == 0, proc.stderr
    out = _single_document(proc)
    assert out["market_open"] is False
    assert out["signal"] == 0
    assert out["price"] == 0


def test_check_price_emits_the_exact_ticker_quote(tmp_path):
    tickers = {"A/USDT": 0.1234, "B/USDT": 0.006, "C/USDT": 0.00001234, "BTC/USDT": 65000.5, "INT/USDT": 3}
    proc = _run(CHECK_PRICE, sorted(tickers), {"QP_TICKERS": json.dumps(tickers)}, tmp_path=tmp_path)
    assert proc.returncode == 0, proc.stderr
    out = _single_document(proc)
    assert out == {k: float(v) for k, v in tickers.items()}
    assert all(isinstance(v, float) for v in out.values())


def test_check_price_omits_missing_and_invalid_tickers_and_keeps_valid_peers(tmp_path):
    tickers = {
        "OK/USDT": 0.00001234,
        "NULL/USDT": None,
        "STRING/USDT": "0.12",
        "BOOL/USDT": True,
        "NOLAST/USDT": {"symbol": "NOLAST/USDT"},
        "NAN/USDT": float("nan"),
        "INF/USDT": float("inf"),
    }
    symbols = sorted(tickers) + ["GONE/USDT"]
    proc = _run(CHECK_PRICE, symbols, {"QP_TICKERS": json.dumps(tickers)}, tmp_path=tmp_path)
    assert proc.returncode == 0, proc.stderr
    out = _single_document(proc)
    assert out == {"OK/USDT": 0.00001234}
    failed = sorted(ln.split(":", 1)[0].removeprefix("Failed to fetch ") for ln in proc.stderr.splitlines() if ln.startswith("Failed to fetch "))
    assert failed == sorted(s for s in symbols if s != "OK/USDT")


def test_check_price_without_symbols_emits_an_empty_map(tmp_path):
    proc = _run(CHECK_PRICE, [], {}, tmp_path=tmp_path)
    assert proc.returncode == 0, proc.stderr
    assert _single_document(proc) == {}


@pytest.mark.parametrize("quote", QUOTES)
def test_check_options_preserves_the_spot_price(quote, tmp_path):
    proc = _run(CHECK_OPTIONS, ["vol_mean_reversion", "BTC", "--platform=deribit"],
                {"QP_QUOTE": repr(quote), "QP_CLOSE": repr(quote)}, stdin="[]", tmp_path=tmp_path)
    assert proc.returncode == 0, proc.stderr
    out = _single_document(proc)
    assert not out.get("error"), out
    assert isinstance(out["spot_price"], float)
    assert out["spot_price"] == quote


def test_check_options_without_a_spot_quote_keeps_its_error(tmp_path):
    proc = _run(CHECK_OPTIONS, ["vol_mean_reversion", "BTC", "--platform=deribit"],
                {"QP_CLOSE": 0.1234}, stdin="[]", tmp_path=tmp_path)
    out = _single_document(proc)
    assert out["spot_price"] == 0
    assert out["actions"] == []
    assert out["error"] == "Could not fetch spot price"

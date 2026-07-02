"""Tests for llm_review.py (#1137) — pipeline logic with an injected LLM,
word-cap enforcement, judge parsing, and the subprocess/probe contract.
Not in pyproject testpaths; invoke explicitly:
    uv run --no-sync python -m pytest shared_scripts/test_llm_review.py
"""

import importlib.util
import json
import os
import subprocess
import sys

import pytest

SCRIPT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "llm_review.py")


def _load():
    spec = importlib.util.spec_from_file_location("llm_review", SCRIPT)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


@pytest.fixture(scope="module")
def mod():
    return _load()


CTX = {
    "strategy_id": "hl-btc",
    "symbol": "BTC",
    "platform": "hyperliquid",
    "type": "perps",
    "side": "long",
    "entry_price": 50000.0,
    "quantity": 0.1,
    "leverage": 3,
    "entry_atr": 400.0,
    "timeframe": "4h",
    "regime": "trending_up",
    "is_live": True,
    "indicators": {"atr": 400.0, "rsi": 61.2},
    "model": "claude-sonnet-5",
}


class TestWordCap:
    def test_under_cap_unchanged(self, mod):
        assert mod.truncate_to_word_cap("one two three", 5) == "one two three"

    def test_normalizes_whitespace(self, mod):
        assert mod.truncate_to_word_cap("  a\n b\tc ", 5) == "a b c"

    def test_truncates_with_ellipsis(self, mod):
        assert mod.truncate_to_word_cap("a b c d e", 3) == "a b c …"

    def test_none_safe(self, mod):
        assert mod.truncate_to_word_cap(None, 3) == ""


class TestSummarizeOhlcv:
    def test_summary_fields(self, mod):
        candles = [[i, 100 + i, 101 + i, 99 + i, 100 + i, 10] for i in range(60)]
        s = mod.summarize_ohlcv(candles)
        assert s["bars"] == 60
        assert s["last_close"] == 159
        assert s["change_pct_5"] is not None
        assert s["change_pct_50"] is not None

    def test_too_short_or_garbage(self, mod):
        assert mod.summarize_ohlcv([[1, 1, 1, 1, 1, 1]]) is None
        assert mod.summarize_ohlcv([["x"] * 6] * 10) is None
        assert mod.summarize_ohlcv(None) is None


class TestJudgeParsing:
    def test_strict_json(self, mod):
        v, r = mod.parse_judge_output('{"verdict": "Bullish", "rationale": "looks good"}', 55)
        assert v == "bullish" and r == "looks good"

    def test_fenced_json(self, mod):
        v, _ = mod.parse_judge_output('```json\n{"verdict":"bearish","rationale":"r"}\n```', 55)
        assert v == "bearish"

    def test_keyword_fallback_single(self, mod):
        v, r = mod.parse_judge_output("Overall this reads mixed to me because ...", 55)
        assert v == "mixed"

    def test_ambiguous_raises(self, mod):
        with pytest.raises(RuntimeError):
            mod.parse_judge_output("could be bullish or bearish", 55)
        with pytest.raises(RuntimeError):
            mod.parse_judge_output("no verdict here", 55)

    def test_rationale_capped(self, mod):
        long = " ".join(["w"] * 100)
        _, r = mod.parse_judge_output(json.dumps({"verdict": "mixed", "rationale": long}), 10)
        assert len(r.split()) == 11  # 10 words + ellipsis


class TestPipeline:
    def _fake_llm(self, calls):
        def llm_call(system, user):
            calls.append((system, user))
            if "risk manager" in system:
                return '{"verdict": "bullish", "rationale": "momentum and funding both lean up"}'
            return "short note " + " ".join(["pad"] * 80)  # over-cap on purpose
        return llm_call

    def test_full_pipeline_with_debate(self, mod):
        calls = []
        market = {"ohlcv_summary": {"last_close": 50000, "bars": 60}, "funding": {"current_rate": 0.0001}}
        out = mod.run_pipeline(CTX, market, self._fake_llm(calls), max_debate_rounds=2, word_cap=55)
        assert out["verdict"] == "bullish"
        assert out["rationale"]
        assert set(out["per_analyst"]) == {"technical", "derivatives"}
        # Every topic obeys the cap (55 words + ellipsis marker at most).
        for note in list(out["per_analyst"].values()) + [out["rationale"]]:
            assert len(note.split()) <= 56
        # 2 analysts + 2 rounds x (bull+bear) + judge = 7 calls
        assert len(calls) == 7

    def test_zero_rounds_skips_debate(self, mod):
        calls = []
        market = {"ohlcv_summary": None, "funding": None}
        out = mod.run_pipeline(CTX, market, self._fake_llm(calls), max_debate_rounds=0, word_cap=55)
        assert out["verdict"] == "bullish"
        # funding unavailable -> derivatives analyst skipped; technical + judge only
        assert set(out["per_analyst"]) == {"technical"}
        assert len(calls) == 2

    def test_llm_failure_propagates(self, mod):
        def boom(system, user):
            raise RuntimeError("api down")
        with pytest.raises(RuntimeError):
            mod.run_pipeline(CTX, {"ohlcv_summary": None, "funding": None}, boom)


class TestSubprocessContract:
    def test_probe_only(self):
        r = subprocess.run([sys.executable, SCRIPT, "--probe-only"], capture_output=True, text=True, timeout=30)
        assert r.returncode == 0
        assert json.loads(r.stdout)["status"] == "ok"

    def test_missing_api_key_errors_json(self):
        env = {k: v for k, v in os.environ.items() if k != "ANTHROPIC_API_KEY"}
        r = subprocess.run(
            [sys.executable, SCRIPT],
            input=json.dumps({**CTX, "platform": "nonexistent"}),
            capture_output=True, text=True, timeout=60, env=env,
        )
        assert r.returncode == 1
        out = json.loads(r.stdout)
        assert "ANTHROPIC_API_KEY" in out["error"]

    def test_garbage_stdin_errors_json(self):
        r = subprocess.run([sys.executable, SCRIPT], input="not json",
                           capture_output=True, text=True, timeout=30)
        assert r.returncode == 1
        assert "error" in json.loads(r.stdout)


class TestBuildLLMCall:
    def test_missing_key_raises(self, mod, monkeypatch):
        monkeypatch.delenv("ANTHROPIC_API_KEY", raising=False)
        with pytest.raises(RuntimeError):
            mod.build_llm_call("claude-sonnet-5")

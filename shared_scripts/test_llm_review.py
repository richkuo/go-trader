
import importlib.util
import io
import json
import os
import subprocess
import sys
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer

import pytest

SCRIPT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "llm_review.py")
GO_USAGE_STDERR_PREFIX = "llm_review_usage "


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
    "model": "claude-opus-5-5",
}


class TestWordCap:
    @pytest.mark.parametrize("text,cap,expected", [
        ("one two three", 5, "one two three"),
        ("  a\n b\tc ", 5, "a b c"),
        ("a b c d e", 3, "a b c …"),
        (None, 3, ""),
    ])
    def test_word_cap(self, mod, text, cap, expected):
        assert mod.truncate_to_word_cap(text, cap) == expected


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
    @pytest.mark.parametrize("raw,verdict", [
        ('{"verdict": "bullish", "rationale": "looks good"}', "bullish"),
        ('{"verdict":"bearish","rationale":"r"}', "bearish"),
        ('{"rationale": "r", "verdict": "mixed"}', "mixed"),
    ])
    def test_verdict_parsing(self, mod, raw, verdict):
        v, _ = mod.parse_judge_output(raw, 55)
        assert v == verdict

    def test_strict_json_rationale(self, mod):
        _, r = mod.parse_judge_output('{"verdict": "bullish", "rationale": "looks good"}', 55)
        assert r == "looks good"

    @pytest.mark.parametrize("raw", [
        "could be bullish or bearish",
        "no verdict here",
        "I am not bullish here",
        "Overall this reads mixed to me because ...",
        '```json\n{"verdict":"bearish","rationale":"r"}\n```',
        '{"verdict": "Bullish", "rationale": "looks good"}',
        '{"verdict": "neutral", "rationale": "r"}',
        '{"verdict": "bullish", "rationale": ""}',
        '{"verdict": "bullish"}',
        '{"verdict": "bullish", "rationale": "r", "extra": 1}',
        '["bullish", "r"]',
        "",
        None,
    ])
    def test_non_schema_output_raises(self, mod, raw):
        with pytest.raises(RuntimeError):
            mod.parse_judge_output(raw, 55)

    def test_rationale_capped(self, mod):
        long = " ".join(["w"] * 100)
        _, r = mod.parse_judge_output(json.dumps({"verdict": "mixed", "rationale": long}), 10)
        assert len(r.split()) == 11


class TestPipeline:
    def _fake_llm(self, calls):
        def llm_call(system, user, schema=None):
            calls.append((system, user, schema))
            if "risk manager" in system:
                return '{"verdict": "bullish", "rationale": "momentum and funding both lean up"}'
            return "short note " + " ".join(["pad"] * 80)
        return llm_call

    def test_full_pipeline_with_debate(self, mod):
        calls = []
        market = {"ohlcv_summary": {"last_close": 50000, "bars": 60}, "funding": {"current_rate": 0.0001}}
        out = mod.run_pipeline(CTX, market, self._fake_llm(calls), max_debate_rounds=2, word_cap=55)
        assert out["verdict"] == "bullish"
        assert out["rationale"]
        assert set(out["per_analyst"]) == {"technical", "derivatives"}
        for note in list(out["per_analyst"].values()) + [out["rationale"]]:
            assert len(note.split()) <= 56
        assert len(calls) == 7
        assert [c[2] for c in calls] == [None] * 6 + [mod.JUDGE_SCHEMA]

    def test_zero_rounds_skips_debate(self, mod):
        calls = []
        market = {"ohlcv_summary": None, "funding": None}
        out = mod.run_pipeline(CTX, market, self._fake_llm(calls), max_debate_rounds=0, word_cap=55)
        assert out["verdict"] == "bullish"
        assert set(out["per_analyst"]) == {"technical"}
        assert len(calls) == 2

    def test_llm_failure_propagates(self, mod):
        def boom(system, user, schema=None):
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
        assert out["usage"] == {"calls": 0, "input_tokens": 0, "output_tokens": 0}

    def test_garbage_stdin_errors_json(self):
        r = subprocess.run([sys.executable, SCRIPT], input="not json",
                           capture_output=True, text=True, timeout=30)
        assert r.returncode == 1
        assert "error" in json.loads(r.stdout)


class TestBuildLLMCall:
    def test_missing_key_raises(self, mod, monkeypatch):
        monkeypatch.delenv("ANTHROPIC_API_KEY", raising=False)
        with pytest.raises(RuntimeError):
            mod.build_llm_call("claude-opus-5-5")


def _reply(n, stop_reason="end_turn", text="short note"):
    return {
        "content": [{"type": "text", "text": text}],
        "stop_reason": stop_reason,
        "usage": {"input_tokens": 100 * n, "output_tokens": 10 * n},
    }


def _serve(reply_for):
    seen = []

    class Handler(BaseHTTPRequestHandler):
        def do_POST(self):
            seen.append(json.loads(self.rfile.read(int(self.headers["content-length"]))))
            data = json.dumps(reply_for(seen[-1], len(seen))).encode("utf-8")
            self.send_response(200)
            self.send_header("content-type", "application/json")
            self.send_header("content-length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        def log_message(self, *args):
            pass

    server = HTTPServer(("127.0.0.1", 0), Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    return server, seen


class TestUsageAccounting:
    def _run_main(self, mod, monkeypatch, capsys, reply_for):
        server, seen = _serve(reply_for)
        url = f"http://127.0.0.1:{server.server_port}/v1/messages"
        real = mod.build_llm_call
        monkeypatch.setattr(mod, "build_llm_call", lambda model, usage=None: real(model, api_url=url, usage=usage))
        monkeypatch.setenv("ANTHROPIC_API_KEY", "test-key")
        monkeypatch.setattr(sys, "argv", [SCRIPT])
        monkeypatch.setattr(sys, "stdin", io.StringIO(json.dumps({**CTX, "platform": "nonexistent"})))
        try:
            rc = mod.main()
        finally:
            server.shutdown()
            server.server_close()
        captured = capsys.readouterr()
        stderr_usage = [
            json.loads(line[len(GO_USAGE_STDERR_PREFIX):])
            for line in captured.err.splitlines()
            if line.startswith(GO_USAGE_STDERR_PREFIX)
        ]
        return rc, json.loads(captured.out), stderr_usage, seen

    def test_success_sums_every_call(self, mod, monkeypatch, capsys):
        def reply_for(body, n):
            if "format" in body["output_config"]:
                return _reply(n, text='{"verdict": "bullish", "rationale": "momentum leans up"}')
            return _reply(n)

        rc, out, stderr_usage, seen = self._run_main(mod, monkeypatch, capsys, reply_for)
        assert rc == 0
        assert out["verdict"] == "bullish"
        assert len(seen) == 4
        assert out["usage"] == {"calls": 4, "input_tokens": 1000, "output_tokens": 100}
        assert stderr_usage[-1] == out["usage"]

    def test_failure_keeps_usage_of_calls_made(self, mod, monkeypatch, capsys):
        def reply_for(body, n):
            if n == 3:
                return _reply(n, stop_reason="refusal", text="")
            return _reply(n)

        rc, out, stderr_usage, seen = self._run_main(mod, monkeypatch, capsys, reply_for)
        assert rc == 1
        assert "verdict" not in out
        assert "refused" in out["error"]
        assert len(seen) == 3
        assert out["usage"] == {"calls": 3, "input_tokens": 600, "output_tokens": 60}
        assert [u["calls"] for u in stderr_usage] == [1, 2, 3]

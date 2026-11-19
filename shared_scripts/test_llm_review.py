
import contextlib
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


@contextlib.contextmanager
def _case(*label):
    try:
        yield
    except (Exception, pytest.fail.Exception) as exc:
        raise AssertionError(f"case {label!r} failed: {exc}") from exc


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


class TestPipeline:
    def _fake_llm(self, calls):
        def llm_call(system, user, schema=None):
            calls.append((system, user, schema))
            if "risk manager" in system:
                return '{"verdict": "bullish", "rationale": "momentum and funding both lean up"}'
            return "short note " + " ".join(["pad"] * 80)
        return llm_call

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

    def test_missing_api_key_errors_json(self, subtests):
        with subtests.test(case="missing_api_key"):
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
        with subtests.test(case="garbage_stdin"):
            r = subprocess.run([sys.executable, SCRIPT], input="not json",
                               capture_output=True, text=True, timeout=30)
            assert r.returncode == 1
            assert "error" in json.loads(r.stdout)


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

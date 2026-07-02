#!/usr/bin/env python3
"""LLM multi-agent entry analysis (#1137, TradingAgents-inspired).

Invoked by the Go scheduler AFTER a fresh position-open is confirmed, on a
dedicated async lane (never the shared trading-subprocess semaphore). Purely
advisory: the output is commentary posted to the strategy's Discord/Telegram
channel plus a one-word verdict persisted for diagnostics. It never gates,
sizes, or closes anything — an error here exits 1 and Go posts nothing.

Pipeline: analysts (technical from the open cycle's indicators + OHLCV,
derivatives from funding rates) -> bounded bull/bear debate -> judge verdict.
Every topic is ELI18 and word-capped; Go re-enforces the cap server-side.

Contract (mirrors the check-script conventions):
    stdin:  one JSON object (see Go's llmReviewInput)
    stdout: one JSON object {verdict, rationale, per_analyst, model} on
            success, {"error": ...} on failure; exit 1 on error
    --probe-only: print {"status": "ok"} and exit 0 before any stdin/env/
            network access (startup deploy-mismatch probe)

The Anthropic API is called via urllib (no SDK dependency); the key comes
from ANTHROPIC_API_KEY.
"""

import json
import os
import sys
import urllib.error
import urllib.request

# shared_tools for parity with other scripts' import layout (not strictly
# required today; keeps adapter/sys.path conventions available).
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "shared_tools"))

ANTHROPIC_API_URL = "https://api.anthropic.com/v1/messages"
ANTHROPIC_VERSION = "2023-06-01"
API_KEY_ENV = "ANTHROPIC_API_KEY"

DEFAULT_WORD_CAP = 55
DEFAULT_MAX_DEBATE_ROUNDS = 1
PER_CALL_TIMEOUT_S = 60
MAX_TOKENS_PER_CALL = 400
OHLCV_LIMIT = 100

VALID_VERDICTS = ("bullish", "bearish", "mixed")

STYLE_RULES = (
    "Write for a smart 18-year-old with no trading background: plain language, "
    "no jargon, no indicator names without a one-word gloss. "
    "HARD CAP: {cap} words. Never exceed it."
)


def truncate_to_word_cap(text, cap):
    """Hard-cap text at `cap` words (whitespace-normalizing); append an
    ellipsis when truncating. Mirrors Go's truncateToWordCap."""
    words = str(text or "").split()
    if len(words) <= cap:
        return " ".join(words)
    return " ".join(words[:cap]) + " …"


def summarize_ohlcv(candles):
    """Reduce raw [ts, o, h, l, c, v] rows to a compact stats dict the
    technical analyst can reason over. Returns None when unusable."""
    closes = []
    highs = []
    lows = []
    for row in candles or []:
        try:
            highs.append(float(row[2]))
            lows.append(float(row[3]))
            closes.append(float(row[4]))
        except (TypeError, ValueError, IndexError):
            return None
    if len(closes) < 5:
        return None

    def pct_change(n):
        if len(closes) <= n or closes[-1 - n] == 0:
            return None
        return round((closes[-1] / closes[-1 - n] - 1) * 100, 2)

    last = closes[-1]
    hi = max(highs)
    lo = min(lows)
    return {
        "last_close": last,
        "bars": len(closes),
        "change_pct_5": pct_change(5),
        "change_pct_20": pct_change(20),
        "change_pct_50": pct_change(50),
        "pct_below_high": round((hi - last) / hi * 100, 2) if hi else None,
        "pct_above_low": round((last - lo) / lo * 100, 2) if lo else None,
    }


def load_platform_adapter(platform):
    """Load platforms/<platform>/adapter.py and instantiate its
    *ExchangeAdapter class (public methods only, mirroring check scripts).
    Returns None when the platform has no adapter or loading fails — analysts
    degrade to 'data unavailable' instead of erroring the pipeline."""
    if not platform:
        return None
    base = os.path.join(os.path.dirname(__file__), "..", "platforms", platform)
    path = os.path.join(base, "adapter.py")
    if not os.path.exists(path):
        return None
    try:
        import importlib.util

        # Platform dir first on sys.path: HL's adapter does `from adapter
        # import ...`-adjacent local imports and must not collide with the
        # hyperliquid SDK package.
        sys.path.insert(0, base)
        spec = importlib.util.spec_from_file_location(f"{platform}_adapter_llm_review", path)
        mod = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(mod)
        for name in dir(mod):
            if name.endswith("ExchangeAdapter"):
                return getattr(mod, name)()
    except Exception:
        return None
    return None


def gather_market_context(ctx):
    """Best-effort market data for the analysts. Every fetch is wrapped: a
    failure yields None for that block, never a pipeline error. On Hyperliquid
    the OHLCV call rides the shared /tmp disk cache (60s TTL) — dispatched
    right after the open, this is a cache read, not a refetch."""
    adapter = load_platform_adapter(ctx.get("platform"))
    symbol = ctx.get("symbol") or ""
    coin = symbol.split("/")[0] if "/" in symbol else symbol
    timeframe = ctx.get("timeframe") or "1h"

    ohlcv_summary = None
    funding = None
    if adapter is not None:
        try:
            if hasattr(adapter, "get_ohlcv"):
                ohlcv_summary = summarize_ohlcv(adapter.get_ohlcv(coin, timeframe, OHLCV_LIMIT))
        except Exception:
            ohlcv_summary = None
        try:
            if hasattr(adapter, "get_funding_rate"):
                funding = {"current_rate": adapter.get_funding_rate(coin)}
                if hasattr(adapter, "get_funding_history"):
                    hist = adapter.get_funding_history(coin, 3) or []
                    rates = [h.get("rate") for h in hist if isinstance(h, dict) and h.get("rate") is not None]
                    if rates:
                        funding["avg_rate_3d"] = sum(rates) / len(rates)
                        funding["samples_3d"] = len(rates)
        except Exception:
            funding = None
    return {"ohlcv_summary": ohlcv_summary, "funding": funding}


def build_llm_call(model, api_url=ANTHROPIC_API_URL, timeout=PER_CALL_TIMEOUT_S):
    """Return llm_call(system, user) -> str against the Anthropic Messages
    API. Raises on missing key or HTTP/parse failure (pipeline fails closed:
    Go logs and posts nothing)."""
    api_key = os.environ.get(API_KEY_ENV, "")
    if not api_key:
        raise RuntimeError(f"{API_KEY_ENV} is not set")

    def llm_call(system, user):
        payload = json.dumps({
            "model": model,
            "max_tokens": MAX_TOKENS_PER_CALL,
            "system": system,
            "messages": [{"role": "user", "content": user}],
        }).encode("utf-8")
        req = urllib.request.Request(api_url, data=payload, method="POST", headers={
            "content-type": "application/json",
            "x-api-key": api_key,
            "anthropic-version": ANTHROPIC_VERSION,
        })
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            body = json.loads(resp.read().decode("utf-8"))
        parts = body.get("content") or []
        text = "".join(p.get("text", "") for p in parts if p.get("type") == "text")
        if not text.strip():
            raise RuntimeError("empty completion")
        return text.strip()

    return llm_call


def entry_summary(ctx):
    lev = ctx.get("leverage") or 0
    lev_txt = f" at {lev:g}x leverage" if lev else ""
    return (
        f"Strategy {ctx.get('strategy_id')} just opened a {ctx.get('side')} position on "
        f"{ctx.get('symbol')} ({ctx.get('platform')}, {ctx.get('type')}, "
        f"{'live' if ctx.get('is_live') else 'paper'}) at {ctx.get('entry_price')}"
        f"{lev_txt}, timeframe {ctx.get('timeframe')}"
        + (f", market regime '{ctx['regime']}'" if ctx.get("regime") else "")
        + "."
    )


def parse_judge_output(text, word_cap):
    """Parse the judge's reply into (verdict, rationale). Prefers strict JSON;
    falls back to scanning for a verdict keyword. Raises when no verdict can
    be extracted (fail closed — never invent a read)."""
    raw = str(text or "").strip()
    # Strip a markdown fence if the model wrapped its JSON.
    if raw.startswith("```"):
        raw = raw.strip("`")
        if raw.startswith("json"):
            raw = raw[4:]
        raw = raw.strip()
    try:
        obj = json.loads(raw)
        verdict = str(obj.get("verdict", "")).strip().lower()
        rationale = str(obj.get("rationale", "")).strip()
        if verdict in VALID_VERDICTS and rationale:
            return verdict, truncate_to_word_cap(rationale, word_cap)
    except (ValueError, AttributeError):
        pass
    lowered = raw.lower()
    found = [v for v in VALID_VERDICTS if v in lowered]
    if len(found) == 1:
        return found[0], truncate_to_word_cap(raw, word_cap)
    raise RuntimeError("judge output had no parseable verdict")


def run_pipeline(ctx, market, llm_call, max_debate_rounds=DEFAULT_MAX_DEBATE_ROUNDS,
                 word_cap=DEFAULT_WORD_CAP):
    """Analysts -> bounded bull/bear debate -> judge. llm_call is injected so
    tests run without network. Returns the output dict (verdict, rationale,
    per_analyst)."""
    style = STYLE_RULES.format(cap=word_cap)
    entry = entry_summary(ctx)

    per_analyst = {}

    tech_data = {
        "indicators": ctx.get("indicators") or {},
        "entry_atr": ctx.get("entry_atr"),
        "ohlcv_summary": market.get("ohlcv_summary"),
    }
    per_analyst["technical"] = truncate_to_word_cap(llm_call(
        "You are the technical analyst on a trading desk. " + style,
        f"{entry}\nTechnical data (the strategy's own entry indicators plus recent "
        f"price stats; null means unavailable):\n{json.dumps(tech_data, default=str)}\n"
        "Give your read on this entry's price/momentum picture.",
    ), word_cap)

    funding = market.get("funding")
    if funding is not None:
        per_analyst["derivatives"] = truncate_to_word_cap(llm_call(
            "You are the derivatives analyst on a trading desk. " + style,
            f"{entry}\nFunding-rate data (positive = longs pay shorts):\n"
            f"{json.dumps(funding, default=str)}\n"
            "Give your read on what positioning/funding implies for this entry.",
        ), word_cap)

    notes = "\n".join(f"- {k}: {v}" for k, v in sorted(per_analyst.items()))
    transcript = []
    for _ in range(max(0, max_debate_rounds)):
        prior = "\n".join(transcript) if transcript else "(none yet)"
        bull = truncate_to_word_cap(llm_call(
            "You are the bull researcher: argue the strongest honest case FOR this entry. " + style,
            f"{entry}\nAnalyst notes:\n{notes}\nDebate so far:\n{prior}",
        ), word_cap)
        transcript.append(f"bull: {bull}")
        bear = truncate_to_word_cap(llm_call(
            "You are the bear researcher: argue the strongest honest case AGAINST this entry. " + style,
            f"{entry}\nAnalyst notes:\n{notes}\nDebate so far:\n" + "\n".join(transcript),
        ), word_cap)
        transcript.append(f"bear: {bear}")

    judge_raw = llm_call(
        "You are the risk manager issuing the desk's final read on an already-open "
        "position. This is commentary only — the trade stands regardless. " + style +
        ' Reply with ONLY a JSON object: {"verdict": "bullish"|"bearish"|"mixed", '
        '"rationale": "<plain-language reasoning>"}.',
        f"{entry}\nAnalyst notes:\n{notes}\nDebate:\n"
        + ("\n".join(transcript) if transcript else "(debate skipped)"),
    )
    verdict, rationale = parse_judge_output(judge_raw, word_cap)

    return {
        "verdict": verdict,
        "rationale": rationale,
        "per_analyst": per_analyst,
        "model": ctx.get("model"),
    }


def main():
    if "--probe-only" in sys.argv[1:]:
        print(json.dumps({"status": "ok"}))
        return 0
    try:
        ctx = json.loads(sys.stdin.read())
        if not isinstance(ctx, dict):
            raise ValueError("stdin payload must be a JSON object")
        model = ctx.get("model") or "claude-sonnet-5"
        word_cap = int(ctx.get("word_cap") or DEFAULT_WORD_CAP)
        rounds = ctx.get("max_debate_rounds")
        rounds = DEFAULT_MAX_DEBATE_ROUNDS if rounds is None else int(rounds)
        market = gather_market_context(ctx)
        llm_call = build_llm_call(model)
        out = run_pipeline(ctx, market, llm_call, max_debate_rounds=rounds, word_cap=word_cap)
        print(json.dumps(out))
        return 0
    except Exception as e:  # noqa: BLE001 — subprocess contract: JSON even on error
        print(json.dumps({"error": f"{type(e).__name__}: {e}"}))
        return 1


if __name__ == "__main__":
    sys.exit(main())

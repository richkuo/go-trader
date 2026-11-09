from __future__ import annotations

import argparse
import importlib.util
import json
import os
import sys
from typing import Any, Dict, List, Optional

_TOOLS = os.path.dirname(os.path.abspath(__file__))
_ROOT = os.path.abspath(os.path.join(_TOOLS, ".."))
_OPEN_REGISTRY_PATH = os.path.join(_ROOT, "shared_strategies", "open", "registry.py")
if _TOOLS not in sys.path:
    sys.path.insert(0, _TOOLS)

import close_registry_loader

SCHEMA_VERSION = 1
KINDS = ("open", "close")
PLATFORMS = ("spot", "futures")

ROLE_SCALAR_OPEN = "scalar_open"
ROLE_QUOTE_CAPABILITY = "quote_capability"
ROLE_NATIVE_CLOSE = "native_close"
OWNER_SCALAR = "scalar"
OWNER_QUOTE = "quote_coordinator"

PAPER_AVAILABLE = "available"
PAPER_HELD = "held"
LIVE_ALLOWED = "allowed"
LIVE_ACK = "acknowledgement_required"
LIVE_REFUSED = "refused"

RESEARCH_COMMON_REQUIRES = (
    "exactly one explicit paper mode in args (--mode=paper or --mode paper); missing, live, invalid, dangling "
    "and repeated modes are refused with or without allow_no_edge",
    "Hyperliquid perps on shared_scripts/check_hyperliquid.py with market_feed websocket or shared "
    "(the REST feed and every other route are refused)",
    "a research open name in a close slot evaluates only when it equals the strategy's effective open strategy, "
    "because sealed inputs attach only to the open strategy",
)

RESEARCH_INPUT_REQUIRES = {
    "cumulative_volume_delta_divergence": (
        "complete signed executed trade-flow coverage (sealed trade_flows) with matching candle volume and "
        "complete session history",
    ),
    "macro_event_breakout": (
        "hash-pinned point-in-time calendar (calendar_path and calendar_sha256) and, with spread_gate, "
        "hash-pinned quote records (quote_path and quote_sha256)",
        "expected_interval_ms equal to the timeframe duration",
        "close_strategy time_stop with max_bars equal to holding_bars",
    ),
    "bitcoin_lead_lag": (
        "matched follower and BTC leader frames on the same timeframe (the follower is not BTC) with "
        "lag_bars + horizon_bars + training_bars of closed history",
        "expected_interval_ms equal to the timeframe duration",
    ),
    "volume_profile_reclaim": (
        "complete previous-session executed-trade volume profile (sealed volume_profiles) and consecutive "
        "current-session closed candles with bar timing",
        "expected_interval_ms equal to the timeframe duration",
    ),
    "perpetual_premium_reversion": (
        "complete synchronized 60 s premium grid with valid quote and oracle receipt history (sealed perp_premiums)",
        "expected_interval_ms equal to the timeframe duration",
    ),
    "order_book_imbalance": (
        "complete l2Book boundary and episode with event and receipt coverage (sealed l2_books)",
        "closed_bar_decisions must be false",
    ),
    "sweep_absorption_ifvg": (
        "complete signed executed trade-flow coverage (sealed trade_flows) from the session start through the "
        "sweep, with matching candle volume and complete session history",
        "sealed one-hour candles covering the swing-structure search (htf_candles)",
        "closed_bar_decisions must be true",
        "expected_interval_ms equal to the timeframe duration",
    ),
}

QUOTE_REQUIRES = (
    "typed quote_owner setup with exactly one explicit --mode=paper",
    "complete quote block whose coin equals the strategy symbol",
    "close_strategy inventory_quote_exit as the sole elective close",
    "exactly one static percent stop (stop_loss_pct or stop_loss_margin_pct)",
    "exclusive ownership of the coin in its paper partition",
)


def _open_registry():
    spec = importlib.util.spec_from_file_location("_go_trader_catalog_open_registry", _OPEN_REGISTRY_PATH)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def _jsonable(value):
    return json.loads(json.dumps(value))


def _variants(entry: dict) -> Dict[str, Dict[str, Any]]:
    out = {}
    for platform, variant in sorted((entry.get("variants") or {}).items()):
        item = {}
        if "description" in variant:
            item["description"] = variant["description"]
        if "default_params" in variant:
            item["default_params"] = _jsonable(variant["default_params"])
        out[platform] = item
    return out


def _quote_hold(registry, subject: str) -> Dict[str, str]:
    hold = registry.QUOTE_ENGINE_HOLD
    return {
        "reason": f"{hold['reason']}: {subject} is quote-owned; a configured explicit-paper quote owner loads and the "
                  "scheduler holds it every due cycle until a forward paper quote engine exists; scalar, batch and "
                  "fallback evaluation refuse it in every mode",
        "tracking_url": hold["tracking_url"],
    }


def _open_entry(registry, name: str, entry: dict) -> dict:
    quote = bool(entry.get("quote"))
    research = bool(entry.get("production_refused"))
    no_edge = entry.get("edge_status") == registry.EDGE_STATUS_NO_EDGE
    route = registry.research_paper_route(name)
    paper = {"status": PAPER_AVAILABLE, "route": None, "requires": [], "reason": "", "tracking_url": ""}
    if quote:
        hold = _quote_hold(registry, f"quote capability {name}")
        paper.update(status=PAPER_HELD, route=route, requires=list(QUOTE_REQUIRES), **hold)
        live = LIVE_REFUSED
    elif research:
        paper.update(route=route, requires=list(RESEARCH_COMMON_REQUIRES) + list(RESEARCH_INPUT_REQUIRES.get(name, ())),
                     reason=f"research-only (production_refused, evidence {entry.get('edge_ref')}): explicit paper "
                            "mode only; live is refused until a separate reviewed promotion")
        live = LIVE_REFUSED
    elif no_edge:
        paper["requires"] = ["an explicit --mode=paper in args for paper evaluation"]
        live = LIVE_ACK
    else:
        live = LIVE_ALLOWED
    return {
        "id": name,
        "description": entry["description"],
        "kind": "open",
        "role": ROLE_QUOTE_CAPABILITY if quote else ROLE_SCALAR_OPEN,
        "platforms": list(entry["platforms"]),
        "default_params": _jsonable(entry["default_params"]),
        "constraints": list(entry.get("constraints") or ()),
        "variants": _variants(entry),
        "short_entries": bool(entry["short_entries"]),
        "edge_status": entry.get("edge_status"),
        "edge_source": entry.get("edge_source"),
        "edge_ref": entry.get("edge_ref"),
        "production_refused": research,
        "owner_role": OWNER_QUOTE if quote else OWNER_SCALAR,
        "live_execution": live,
        "paper_execution": paper,
    }


def _close_entry(registry, item: dict) -> dict:
    research = bool(item["research_only"])
    paper = {"status": PAPER_AVAILABLE, "route": None, "requires": [], "reason": "", "tracking_url": ""}
    if research:
        hold = _quote_hold(registry, f"close strategy {item['name']}")
        paper.update(status=PAPER_HELD, route=dict(registry.RESEARCH_PAPER_ROUTE), requires=list(QUOTE_REQUIRES), **hold)
    return {
        "id": item["name"],
        "description": item["description"],
        "kind": "close",
        "role": ROLE_NATIVE_CLOSE,
        "platforms": list(item["platforms"]),
        "default_params": _jsonable(item["default_params"]),
        "constraints": [],
        "variants": {},
        "short_entries": None,
        "research_only": research,
        "owner_role": OWNER_QUOTE if research else OWNER_SCALAR,
        "live_execution": LIVE_REFUSED if research else LIVE_ALLOWED,
        "paper_execution": paper,
    }


def build_catalog(kind: str = "all", platform: Optional[str] = None) -> dict:
    if kind not in KINDS + ("all",):
        raise ValueError(f"--kind must be one of open, close, all; got {kind!r}")
    if platform is not None and platform not in PLATFORMS:
        raise ValueError(f"--platform must be one of {', '.join(PLATFORMS)}; got {platform!r}")
    registry = _open_registry()
    entries: List[dict] = []
    if kind in ("open", "all"):
        for name, entry in registry.STRATEGIES.items():
            entries.append(_open_entry(registry, name, entry))
    if kind in ("close", "all"):
        for item in close_registry_loader.list_all_strategies_detailed():
            entries.append(_close_entry(registry, item))
    if platform is not None:
        entries = [e for e in entries if platform in e["platforms"]]
    entries.sort(key=lambda e: (e["kind"], e["id"]))
    return {"schema_version": SCHEMA_VERSION, "entries": entries}


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(prog="strategy_catalog.py")
    parser.add_argument("--list-json", action="store_true")
    parser.add_argument("--kind", default="all")
    parser.add_argument("--platform", default=None)
    try:
        args = parser.parse_args(argv)
        if not args.list_json:
            raise ValueError("usage: strategy_catalog.py --list-json [--kind open|close|all] [--platform spot|futures]")
        catalog = build_catalog(args.kind, args.platform)
    except SystemExit as exc:
        print(json.dumps({"error": f"invalid arguments (exit {exc.code})"}))
        return 1
    except Exception as exc:
        print(json.dumps({"error": str(exc)}))
        return 1
    print(json.dumps(catalog, sort_keys=True))
    return 0


if __name__ == "__main__":
    sys.exit(main())

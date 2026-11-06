#!/usr/bin/env python3

import argparse
import json
import os
import sys

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
sys.path.insert(0, os.path.join(ROOT, "backtest"))

from registry_loader import load_registry, registry_for_strategy_type


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--type", default="spot")
    parser.add_argument("--strategy", required=True)
    parser.add_argument("--probe-only", action="store_true")
    parser.add_argument("--research-support", action="store_true")
    parser.add_argument("--config", default="")
    parser.add_argument("--strategy-id", default="")
    args = parser.parse_args()

    if args.probe_only:
        print(json.dumps({"ok": True}))
        return

    reg_key = registry_for_strategy_type(args.type)
    reg = load_registry(reg_key)
    name = args.strategy.strip()
    strat = reg.STRATEGY_REGISTRY.get(name)
    if not strat:
        print(json.dumps({"error": f"unknown strategy {name!r} in {reg_key} registry"}))
        sys.exit(1)

    out = {
        "strategy": name,
        "registry": reg_key,
        "description": strat.get("description", ""),
        "default_params": dict(strat.get("default_params") or {}),
    }
    if args.research_support:
        if not args.config or not args.strategy_id:
            print(json.dumps({"error": "--research-support needs --config and --strategy-id"}))
            sys.exit(1)
        from tune_live import research_support
        try:
            out["research"] = research_support(args.config, args.strategy_id)
        except (OSError, ValueError) as exc:
            out["research"] = {"supported": False, "status": "config_error", "reason": str(exc)}
    print(json.dumps(out))


if __name__ == "__main__":
    main()

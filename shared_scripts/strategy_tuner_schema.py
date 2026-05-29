#!/usr/bin/env python3
"""Return open-strategy default params for the dashboard tuner (#811)."""

import argparse
import json
import os
import sys

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
sys.path.insert(0, os.path.join(ROOT, "backtest"))

from registry_loader import load_registry  # noqa: E402


def _registry_for_type(strategy_type: str) -> str:
    if strategy_type in ("perps", "futures", "manual"):
        return "futures"
    return "spot"


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--type", default="spot")
    parser.add_argument("--strategy", required=True)
    parser.add_argument("--probe-only", action="store_true")
    args = parser.parse_args()

    if args.probe_only:
        print(json.dumps({"ok": True}))
        return

    reg_key = _registry_for_type(args.type)
    reg = load_registry(reg_key)
    name = args.strategy.strip()
    strat = reg.STRATEGY_REGISTRY.get(name)
    if not strat:
        print(json.dumps({"error": f"unknown strategy {name!r} in {reg_key} registry"}))
        sys.exit(1)

    print(
        json.dumps(
            {
                "strategy": name,
                "registry": reg_key,
                "description": strat.get("description", ""),
                "default_params": dict(strat.get("default_params") or {}),
            }
        )
    )


if __name__ == "__main__":
    main()

#!/usr/bin/env python3

import argparse
import json
import os
import sys
import traceback
from datetime import datetime, timezone


sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "platforms", "topstep"))


def _parse_args(argv=None):
    parser = argparse.ArgumentParser(description="Fetch TopStep trade fills since a cursor.")
    parser.add_argument(
        "--since-ms",
        type=int,
        default=0,
        help="Only return fills settled at or after this epoch-millisecond cursor.",
    )
    return parser.parse_args(argv)


def main(argv=None):
    args = _parse_args(argv)
    try:
        from adapter import TopStepExchangeAdapter
        adapter = TopStepExchangeAdapter(mode="live")
        if not adapter.is_live:
            _emit_error("TopStep adapter not live — set TOPSTEP_API_KEY / TOPSTEP_API_SECRET / TOPSTEP_ACCOUNT_ID")
            return
        fills, capped = adapter.get_account_fills(since_ms=args.since_ms)
    except Exception as e:
        traceback.print_exc(file=sys.stderr)
        _emit_error(str(e))
        return

    print(json.dumps({
        "fills": fills or [],
        "capped": bool(capped),
        "platform": "topstep",
        "timestamp": datetime.now(timezone.utc).isoformat(),
    }))


def _emit_error(message):
    print(json.dumps({
        "fills": [],
        "capped": False,
        "platform": "topstep",
        "timestamp": datetime.now(timezone.utc).isoformat(),
        "error": message,
    }))
    sys.exit(1)


if __name__ == "__main__":
    main()

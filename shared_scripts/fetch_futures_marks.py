#!/usr/bin/env python3
"""
Mark-price fetcher for CME futures symbols (TopStep / issue #261).

Called by the Go scheduler alongside check_price.py to revalue open futures
positions in PortfolioNotional / PortfolioValue at the live mark rather than
the frozen entry cost (pos.AvgCost). Cannot reuse check_price.py because
BinanceUS does not quote CME futures — this script delegates to the TopStep
adapter, which auto-selects live TopStepX quotes (if TOPSTEP_API_KEY +
TOPSTEP_API_SECRET + TOPSTEP_ACCOUNT_ID are set) or the yfinance paper
fallback (ES=F, NQ=F, MES=F, MNQ=F, CL=F, GC=F).

Usage: python3 fetch_futures_marks.py ES NQ MES

Always outputs a JSON object to stdout. Symbols whose price cannot be
fetched are omitted (matching check_price.py), so the Go caller can detect
misses and fall back to pos.AvgCost with a [WARN] log — graceful
degradation, not a hard cycle skip.
"""

import json
import os
import sys
import traceback


def main():
    symbols = sys.argv[1:]
    if not symbols:
        print(json.dumps({}))
        return

    try:
        sys.path.insert(
            0,
            os.path.join(os.path.dirname(__file__), "..", "platforms", "topstep"),
        )
        from adapter import TopStepExchangeAdapter  # type: ignore
    except Exception as e:  # noqa: BLE001
        print(
            f"[WARN][fetch_futures_marks] adapter import failed: {e}",
            file=sys.stderr,
        )
        traceback.print_exc(file=sys.stderr)
        print(json.dumps({}))
        sys.exit(1)

    # Auto-select live vs paper based on whether TopStepX creds are present.
    # Live path hits the TopStepX /v1/market/quote endpoint; paper path uses
    # the yfinance fallback. Both route through adapter.get_price, so the
    # caller doesn't care which was used.
    if (
        os.environ.get("TOPSTEP_API_KEY")
        and os.environ.get("TOPSTEP_API_SECRET")
        and os.environ.get("TOPSTEP_ACCOUNT_ID")
    ):
        mode = "live"
    else:
        mode = "paper"

    # Track whether we had to downgrade live→paper so the caller can
    # surface it in the cycle summary. Emitted as a reserved "_mode" key
    # in the JSON output, alongside normal symbol→price entries.
    effective_mode = mode
    try:
        adapter = TopStepExchangeAdapter(mode=mode)
    except Exception as e:  # noqa: BLE001
        # E.g. live mode was requested but requests is missing. Degrade to
        # paper so the scheduler still gets a mark rather than frozen entry
        # costs on every TopStep position. Use [WARN] prefix so the Go
        # scheduler's log pipeline picks this up as a warning rather than
        # burying it in generic stderr noise.
        print(
            f"[WARN][fetch_futures_marks] {mode} mode init failed ({e}); "
            "falling back to paper",
            file=sys.stderr,
        )
        try:
            adapter = TopStepExchangeAdapter(mode="paper")
            effective_mode = "paper_fallback"
        except Exception as e2:  # noqa: BLE001
            print(
                f"[WARN][fetch_futures_marks] paper fallback failed: {e2}",
                file=sys.stderr,
            )
            print(json.dumps({}))
            sys.exit(1)

    marks: "dict[str, float | str]" = {}
    for symbol in symbols:
        try:
            price = adapter.get_price(symbol)
            if price and price > 0:
                marks[symbol] = float(price)
            else:
                # Omit symbols with no price so Go can detect misses and
                # fall back to pos.AvgCost with a [WARN] log (same
                # degradation as get_price exceptions below).
                print(
                    f"[WARN][fetch_futures_marks] no price for {symbol}",
                    file=sys.stderr,
                )
        except Exception as e:  # noqa: BLE001
            print(
                f"[WARN][fetch_futures_marks] get_price({symbol}) failed: {e}",
                file=sys.stderr,
            )
            # Omit failed symbols so Go can detect misses.

    # Attach mode metadata under a reserved "_mode" key so the Go side can
    # distinguish normal live/paper output from a silent paper_fallback
    # downgrade. The type-splitting happens in FetchFuturesMarks
    # (scheduler/executor.go): that function decodes the JSON into
    # map[string]interface{}, strips "_mode" into a separate string return
    # value, and only then populates the float-typed marks map. By the
    # time mergeFuturesMarks runs, "_mode" has already been filtered out,
    # so the skip-if-exists / skip-non-positive guards in mergeFuturesMarks
    # are *separate* safeguards for numeric prices — not defenses against
    # this metadata key. Do NOT "simplify" by relying on mergeFuturesMarks
    # to filter "_mode"; if FetchFuturesMarks is ever refactored to return
    # map[string]interface{} directly, that filter has to move with it.
    marks["_mode"] = effective_mode
    print(json.dumps(marks))


if __name__ == "__main__":
    main()

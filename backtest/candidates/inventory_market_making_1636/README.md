# inventory_market_making (issue 1861, parent 1636)

Research-only, production-refused quote capability. `inventory_market_making` is registered `edge_status="no_edge"`, `edge_source="study_inconclusive"` and `production_refused=True`, futures only, with no scalar signal. Every production configuration, mode and `allow_no_edge` form refuses it; scalar checks, batch checks, close fallback, candle backtests, the optimizer and parity runs refuse it. Only `go-trader quote-replay` admits it, through the internal research admission of the quote coordinator (issue 1860).

**Verdict: INCONCLUSIVE** (`REPORT.md`, `results.json`). No pinned recording of l2Book depth with raw executed trades exists, and the maker fee, minimum order rule, latency and margin evidence are unverified. The executable study and its integration evidence are delivered; no synthetic profitability evidence is reported.

## Files

| File | Content |
|---|---|
| `protocol-lock.json` | Frozen protocol: base parameters (the registry defaults), cycle, cap, stop, quote block, lot decimals and their pinned evidence (`order_book_imbalance_1635/data/venue_meta.json`), costs, stress costs, latencies, queue model, variant grid, fixed-spread baseline, splits, eligibility rule, gates, assumptions and unverified evidence. |
| `data-manifest.json` | Pinned recordings (path, coin, `run.manifest.json` sha256) and one pinned `fundingHistory` file. Empty today. |
| `run_study.py` | The driver. Checks the protocol against the registry specification and the binary's `quote-replay discover`, checks every pinned input hash, selects eligible windows by the frozen rule, splits them 60/20/20 by time, runs every grid variant and the fixed-spread baseline on training, the selected variant on validation, the selected variant and the baseline on held-out, and the stress run, verifies each run with the independent ledger, then applies the gates. |
| `discovery_before_*.json`, `discovery_after_*.json` | Ordinary spot and futures discovery from `origin/main` 5e9c3aa9 and from this change; byte-identical. |
| `discovery_quote.json` | Dedicated quote discovery (`backtest/quote_replay.py discover`): quote metadata, the versioned parameter specification and its sha256. |
| `REPORT.md`, `results.json` | The report and full results that the frozen invocation writes. |

## Reproduce

```bash
BIN="$(mktemp -d)/go-trader"
go -C scheduler build -o "$BIN" .
uv run --no-sync python backtest/candidates/inventory_market_making_1636/run_study.py \
    --protocol backtest/candidates/inventory_market_making_1636/protocol-lock.json \
    --binary "$BIN" --out-dir "$(mktemp -d)"
```

To score, record a bounded public run with no credentials (`go-trader record-observations --coins BTC --kinds l2_book,trade_flow --out-dir <new-dir> --duration <d>`), pin its manifest hash and a matching `fundingHistory` file in `data-manifest.json` before any scoring, and pin venue evidence for every unverified item in `protocol-lock.json`. A PASS describes only the frozen simulation and gives no live promotion; promotion needs a separate reviewed decision.

`scripts/test_quote_replay.sh` runs the same driver end to end on a recorder harness recording (real market-feed owner and recorder with controlled frames), so every split, the baseline, the stress run and the independent ledger execute; that recording is a fixture and is never economic evidence.

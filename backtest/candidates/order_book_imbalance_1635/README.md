# order_book_imbalance (issue 1635, Delivery 2)

Research-only, production-refused candidate. Delivery 1 (PR 1837) supplies the typed `l2_book` owner, recorder, importer, sealed transport and accessor. This directory holds the Delivery 2 study.

**Verdict: FAIL** (`REPORT.md`, `results.json`). The candidate stays production-refused.

## History of the specification

1. `candidate_spec.json` was first frozen for an 840-hour live recording. That recording never started.
2. It was frozen again for the public Hyperliquid S3 archive before any archive object of the study window was downloaded. Only object listings were read.
3. One clarification was committed before any PnL was computed: the 99% opportunity-coverage gate counts the eligible days of each split. The original text did not say whether ineligible days count, and ineligible days allow no entries in either arm. The report publishes coverage under both readings.

## Data

| Item | Value |
|---|---|
| Coins | BTC, ETH |
| Window | 2026-07-18 through 2026-09-14 (59 UTC days); lookback 2026-07-17 |
| Splits | train 35 days, validation 12, held-out 12 (three 4-day blocks) |
| Books | `s3://hyperliquid-archive/market_data/YYYYMMDD/H/l2Book/<COIN>.lz4` (2826 present, 6 window hours absent) |
| Marks, oracle prices | `s3://hyperliquid-archive/asset_ctxs/YYYYMMDD.csv.lz4` |
| Funding | public `fundingHistory`, `data/funding.json` |
| Venue meta | `data/venue_meta.json` (size decimals, max leverage, margin tables) |
| Bars and ATR | one-minute mid bars derived from archive books by the frozen rule |

Raw objects (1.25 GB) and derived series stay in `data/archive/`, which git ignores. `data/archive_manifest.json` pins the S3 key, size and sha256 of every object; absent keys are listed as absent. `data/import_report.json` holds the importer counts and the sha256 of every derived day file, and `run` checks those hashes before scoring.

## Commands

```bash
uv run --no-sync python backtest/candidates/order_book_imbalance_1635/run_study.py fetch          # aws CLI, --request-payer requester
uv run --no-sync python backtest/candidates/order_book_imbalance_1635/run_study.py fetch-funding
uv run --no-sync python backtest/candidates/order_book_imbalance_1635/run_study.py fetch-venue
uv run --no-sync python backtest/candidates/order_book_imbalance_1635/run_study.py import
uv run --no-sync python backtest/candidates/order_book_imbalance_1635/run_study.py run
uv run --no-sync python backtest/candidates/order_book_imbalance_1635/run_study.py render
```

`fetch`, `fetch-funding` and `fetch-venue` rewrite the pinned files. A fresh `fetch` must reproduce the manifest sha256 values, or `import` refuses the objects.

The scoring run used commit `6c7a7080`. The working tree differed from it only in `SKILL.md` and `docs/research/1635-l2-book-contract.md`; the sha256 values that `results.json` records for every scoring source match that commit.

Discovery before (`origin/main` 793097a1) and after this change is byte-identical (`discovery_before_*.json`, `discovery_after_*.json`).

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

Raw objects (1.25 GB) live in the shared archive cache outside the checkout (`backtest/archive_cache.py`): `$GO_TRADER_ARCHIVE_CACHE_DIR`, else `${XDG_CACHE_HOME:-~/.cache}/go-trader/archive`, at `<root>/hyperliquid-archive/<S3 key>`. Every worktree reads the same files, and removing a worktree does not touch them. Derived series stay in `data/archive/series/`, which git ignores. `data/archive_manifest.json` pins the S3 key, size and sha256 of every object; absent keys are listed as absent. `import` and `run` read each book and asset-context object only after its size and sha256 match the manifest, and a missing or altered `present` object refuses the command. `data/import_report.json` holds the importer counts and the sha256 of every derived day file, and `run` checks those hashes before scoring.

## Commands

```bash
uv run --no-sync python backtest/candidates/order_book_imbalance_1635/run_study.py fetch          # aws CLI, --request-payer requester
uv run --no-sync python backtest/candidates/order_book_imbalance_1635/run_study.py fetch-funding
uv run --no-sync python backtest/candidates/order_book_imbalance_1635/run_study.py fetch-venue
uv run --no-sync python backtest/candidates/order_book_imbalance_1635/run_study.py import
uv run --no-sync python backtest/candidates/order_book_imbalance_1635/run_study.py run
uv run --no-sync python backtest/candidates/order_book_imbalance_1635/run_study.py render
```

`fetch` reads the committed manifest and never writes it. It downloads only the `present` objects that the cache does not hold, each to a unique temporary file that it verifies against the manifest before it puts the file at its final path (read-only). A cached file that does not match refuses with its key and both hashes, and `fetch` never replaces it. An `absent` key that the archive now holds also refuses. `fetch --refreeze` downloads the window and writes a new manifest only when no manifest file exists. `fetch-funding` and `fetch-venue` rewrite their pinned files.

Issue 1853 changed `run_study.py` and `backtest/book_archive.py` after scoring, to read the raw objects from the shared cache. `candidate_spec.json` stays frozen, so its `storage` line still names the old folder. A `run` on the cached data reproduced the committed `results.json` in every field except `source_commit`, `source_tree_dirty` and `source_sha256`; the committed file stays historical.

The scoring run used local commit `6c7a7080`, which became `d3293502` after rebasing onto newer `main` commits that do not touch the study. The working tree differed from it only in `SKILL.md` and `docs/research/1635-l2-book-contract.md`. The sha256 values that `results.json` records for every scoring source match `d3293502`.

Discovery before (`origin/main` 793097a1) and after this change is byte-identical (`discovery_before_*.json`, `discovery_after_*.json`).

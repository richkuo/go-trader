# order_book_imbalance (issue 1635, Delivery 2)

Research-only, production-refused candidate. Delivery 1 (PR 1837) supplies the typed `l2_book` owner, recorder, importer, sealed transport and accessor. This directory holds the Delivery 2 study.

The specification is frozen in `candidate_spec.json` before any archive object of the study window was downloaded. The data source is the public Hyperliquid S3 archive (`docs/research/1635-l2-book-archive-note.md`). The earlier 840-hour recording plan was never started and is replaced.

## Data

| Item | Value |
|---|---|
| Coins | BTC, ETH |
| Window | 2026-07-18 through 2026-09-14 (59 UTC days); lookback 2026-07-17 |
| Splits | train 35 days, validation 12, held-out 12 (three 4-day blocks) |
| Books | `s3://hyperliquid-archive/market_data/YYYYMMDD/H/l2Book/<COIN>.lz4` |
| Marks, oracle prices | `s3://hyperliquid-archive/asset_ctxs/YYYYMMDD.csv.lz4` |
| Funding | public `fundingHistory`, fetched once and pinned |
| Bars and ATR | derived from archive books by the frozen rule |

Raw objects and derived series stay in `data/archive/`, which git ignores. `data/archive_manifest.json` pins the S3 key, size and sha256 of every object; absent keys are listed as absent.

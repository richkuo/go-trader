# l2_book archive import note (1635, version 1)

This note adds one source to the `l2_book` contract (`docs/research/1635-l2-book-contract.md`, version 1). The contract is unchanged: every archived frame goes through the same validation, identity, outcome and marker rules as a live frame. `backtest/book_archive.py` implements this note by driving the replay owner model of `backtest/book_replay.py`.

## Source

- Bucket `s3://hyperliquid-archive` (requester-pays). Books: `market_data/YYYYMMDD/H/l2Book/<COIN>.lz4`, one LZ4 frame file per coin per UTC hour (`H` has no leading zero). Asset contexts: `asset_ctxs/YYYYMMDD.csv.lz4`.
- Each book line is `{"time": "<UTC ISO, up to 9 fractional digits, no zone>", "ver_num": 1, "raw": {"channel": "l2Book", "data": {"coin", "time", "levels"}}}`. `raw` is the venue message; `data` is the same `WsBook` the live socket delivers.
- `backtest/lz4_frame.py` decodes LZ4 frames in pure Python and checks the header, block and content checksums.

## Time basis

| | Live owner | Archive |
|---|---|---|
| Event time | `data.time` | `data.time` (same field) |
| Receipt time | this process's clock at socket read | the archiver's clock in `time`, floored to whole milliseconds |
| Time-basis label | `source_event_ms+first_receipt_ms/1635-v1` | `archiver_receipt_ns_floor_ms/1635-v1` |

The archive receipt is another process's receipt. It is not the receipt this system would have had. One archive hour (BTC, 2026-10-19 hour 12) had receipt minus event p50 307, p99 584 and max 764 ms. The 6-hour live recording had p50 326, p99 816 and max 1064 ms. A study that needs this system's delivery time adds its own modeled latency to the archive receipt; it never treats the archive receipt as a decision delivery time.

## Coverage and barriers

The archive has no subscription acknowledgements, sockets or sessions. The importer infers them:

1. **Epoch:** an `epoch_start` marker at the first imported hour start, then `subscribed` (session 1) at the same time. The owner instance is `hyperliquid-archive`.
2. **Missing hour** (no object for that coin and hour): a `disconnected` marker at the hour start. The next present hour starts a new session with `subscribed` at its hour start.
3. **Silence:** when no frame is accepted for more than the 10000 ms gap bound, a `silence` marker at the last acceptance plus 10001 ms (the earliest time the bound is exceeded, before the next frame is applied). A silence is never placed after the frame that ends the gap. The contract's receipt-gap and event-gap rules also apply at the next accepted frame.
4. **Hour boundaries:** files are ordered by hour; a frame whose receipt lies outside its file's hour is refused (`invalid_frame`).
5. **Window end:** a silence is added if the bound is exceeded before the window end.

## Refusals

- **Global refusal:** a line that is not JSON, lacks exactly one `time`, `ver_num` and `raw`, has `ver_num` other than 1, has a channel other than `l2Book`, has a raw message over 16384 bytes, or has no usable coin. Each one counts as a rejection and writes `invalid_frame` while covered. Its time is the last known receipt (or the frame's own receipt when it parses).
- **Frame refusal:** a frame for another coin than its file, or one that breaks any contract rule (decimal form, level count, `n` bound, sort order, crossed book, future skew, lag, clock regression). It is `rejected` with `invalid_frame` or `clock_regression`, exactly as live.
- Duplicate, conflict and out-of-order outcomes follow the contract unchanged.

## Retention and outputs

The importer applies the research need (window 60000 ms, capacity 4096, depth 20) to the owner, so trimming and `retained_from_ms` follow the live owner rules. It writes per coin and UTC day `go_trader_l2_book_archive_day/v1` files (observations with eviction times, markers and the coverage timeline) and a header with stats, refusal counts and missing hours. These derived files and the raw archive objects stay outside git. The repository keeps the manifest of S3 keys, sizes and sha256 values.

## Other archive gaps

Asset-context files can cover part of a day (for example, 366 or 720 one-minute rows instead of 1440), and some days have no file. A study must treat missing rows as missing evidence, never as unchanged values.

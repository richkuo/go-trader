# l2_book common depth contract (1635, version 1)

Status: implemented in Delivery 1 of issue 1635 (typed owner, recorder, importer, sealed transport and accessor). Delivery 1 registers no entry candidate. Issue 1636 adopts this contract if it lands second.

Machine-readable contract: `docs/research/1635-l2-book-contract-v1.json`, sha256 `a0030268e56c2ddfb9c018ce00868de8dd1f9a12c658cd05615b6426a8320869`. The recorder writes the same object as `book_contract` in every v3 run manifest and segment header. `backtest/book_replay.py` accepts only an exact match. `scripts/test_book_replay.sh` checks the file hash, the recorder copy and the replay copy.

## Source and key

- Key: the existing feed observation key (host `https://api.hyperliquid.xyz`, namespace `perps`, coin, kind `l2_book`). Payload id `<coin>|l2_book`. Risk partitions do not own market evidence.
- Source `hyperliquid_ws_l2Book`, contract `hyperliquid-l2book-native-slow/1635-v1`.
- Subscription `{"type":"l2Book","coin":<coin>,"fast":false}`. `nSigFigs` and `mantissa` are omitted (native precision). The acknowledgement must echo `fast: false` and `null` (or no) `nSigFigs`/`mantissa`. Any other acknowledgement starts no coverage and writes `subscription_failed` when coverage was active.
- Coverage starts at an accepted acknowledgement in the current socket session. A frame before it, or from an older session, is `uncovered`.
- The socket tracks each subscription by its key state. A key that is released and tracked again before the next subscription sync gets an unsubscribe and a new subscribe in the same session, and its new state is covered only from the new acknowledgement.

## Frame validation

- A raw frame over 16384 bytes, an envelope that does not decode, or an `l2Book` frame with no usable coin is a global malformed frame. It counts as rejected for every tracked key and writes `invalid_frame` on every covered key.
- Coin: nonempty, at most 64 bytes, no surrounding whitespace.
- `time`: positive signed 64-bit integer milliseconds (source event time).
- Exactly two sides, bids then asks, each with 1 to 20 levels.
- Decimal price and size: plain positive ASCII decimal, at most 32 characters and 18 fractional digits after trailing zeros are removed. No sign, exponent, whitespace, second point, NaN or infinity. Leading integer zeros and trailing fractional zeros are removed to give the canonical form.
- `n`: unsigned integer from 1 to 4294967295.
- Bids strictly descending, asks strictly ascending, best bid strictly below best ask (exact rational comparison). Input is never repaired.

## Identity

SHA-256 over a length-delimited encoding: each field is written as `<byte length>:<bytes>;` in this order: tag `go-trader-l2book-identity/v1`, host, namespace, coin, decimal event milliseconds, then for each side its level count and each level's canonical price, canonical size and decimal `n`. All supplied levels are hashed before any depth cut. Receipt time, session and local sequence are not part of identity.

## Outcomes, in evaluation order

1. Receipt earlier than the key's receipt high-water mark: rejected, `clock_regression` marker.
2. Frame validation failure: rejected, `invalid_frame` marker.
3. No acknowledged coverage in the current session: `uncovered`.
4. Event more than 250 ms after receipt: rejected (`future_event`). Receipt more than 5000 ms after the event: rejected (`stale_receipt`). Equality at either bound is valid.
5. Same event time, same identity: `duplicate`. It does not refresh freshness or close a gap.
6. Same event time, other identity: `conflict`, `conflict` marker.
7. Event at or below the accepted event high-water mark: `out_of_order`, `out_of_order` marker.
8. Otherwise `accepted`. A receipt gap since the last acceptance, or an event gap since the event high-water mark, over 10000 ms writes a `silence` marker at this receipt.

Markers only reach rejected or defect outcomes while the key is covered. Each frame and each marker takes the next value of one owner-wide local sequence.

## Markers

`epoch_start` (new key state, detail names the owner instance), `subscribed`, `subscription_failed`, `disconnected`, `invalid_frame`, `conflict`, `out_of_order`, `clock_regression`, `silence`, `evidence_loss`, `depth_raised`. A marker carries its detection time and the coverage session of its key. The owner timer (once per second) and every snapshot freeze write `silence` once per gap when no book was accepted for more than 10000 ms. A silence marker is never backdated.

Marker times never decrease within a key. A marker whose detection time is earlier than the key's previous marker (a frame read before a timer or freeze marker took the lock, or a `clock_regression` receipt) takes the previous marker's time, and the timer and freeze test the silence bound at that time. The marker keeps its own, larger sequence.

## Bounds

| Bound | Value |
|---|---|
| Observations per key | 4096 |
| Window per need | 900000 ms |
| Markers per key | 64; the oldest excess markers coalesce into one `evidence_loss` marker and history before the dropped marker is evicted |
| Book keys | 16 |
| Raw frame | 16384 bytes |
| Book bytes per single or batch envelope | 4 MiB |
| Book bytes per seal | 16 MiB |
| Owner book memory | 64 MiB |
| Future skew / lag / gap | 250 / 5000 / 10000 ms |
| Retention slack past the window | 60000 ms |

The capacity bounds are joint. A need set whose estimated seal, envelope or memory size exceeds a budget is refused before it is applied.

## Need, union and history

- A need carries source, contract, time semantics, identity semantics, window, capacity and depth. Needs for one key merge by the per-dimension maximum. Different semantics are a configuration error.
- The owner keeps levels only to the need depth. A depth raise evicts stored history, writes `depth_raised` and starts coverage again.
- `retained_from_ms` is the first receipt time with complete history (eviction, coalescing, depth raise or epoch start move it forward). `prefix_complete` is true only when `retained_from_ms` is at or before the window start. A payload capacity cut sets `prefix_complete` false and moves `retained_from_ms` past the cut. An incomplete prefix is an entry-only hold: the Go hold for a need is exactly `!prefix_complete` of the payload section built for that need, including its own capacity cut when a larger union capacity keeps more history.
- An incomplete prefix (warm-up, eviction, coalescing, a depth raise or a capacity cut) is no seal coverage gap and raises no coverage-gap alert. A coverage gap is only a missing key or a seal whose semantics, window, capacity or depth is short of the need, and its text carries no time, so it alerts once per change.

## Transport

- Seal version 7 carries `l2_books` sections; a seal that also carries `perp_premiums` (#1639) is seal version 8. Market payload version 4 (3 before #1639) carries `l2_books` keyed by payload id, or `l2_books_refused` when not even refusal metadata fits, and the same version carries the optional `perp_premiums` section. Exact payload-header agreement is required with every peer, so a payload-3 peer is refused even for an older seal form.
- A section that does not fit the remaining envelope budget is sent as bounded refusal metadata (`book_capacity_refused`) and the candle frames stay usable.
- Sealed bytes never change after sealing. Observations and markers are those known by the seal deadline.
- Describe advertises the source, contract, semantics, bounds, window, capacity and depth for each key; a consumer refuses a peer whose advertised or actual seal coverage is short in any dimension.

## Recording and replay

- `record-observations --kinds l2_book` writes recording schema v3 (`go_trader_observation_run/v3`, `go_trader_observation_segment/v3`, `go_trader_observation_recording/v3`). Record kinds: `book` (raw frame or the hash of an oversized frame, sequence, outcome, reason, event time, identity), `book_sub` (raw acknowledgement and verdict), `book_marker`, `book_need` (window, capacity, depth, owner instance) and `book_release`.
- `backtest/observation_replay.py import-books` replays the raw frames through an independent parser and owner model, compares every outcome, sequence, identity and marker, and refuses evidence on any disagreement, drop, unclosed run or incomplete segment. Timer silence markers are accepted only when the replayed state allows them at that time.
- `payload_from_series` rebuilds a sealed section from a replayed series at a cutoff, need and freeze time. A state change recorded at the freeze millisecond is ordered after the freeze.
- The open-interest, trade-flow and volume-profile importers accept v3 runs that recorded their kind and ignore book records. Every importer refuses an unknown run schema.

## Deviation from the plan and Delivery 2 note

The plan proposed a 5000 ms gap bound. Version 1 uses 10000 ms. In the earlier 6-hour BTC/ETH recording (default cadence, native precision), receipt gaps per coin had median 5386 ms, p99 5906 ms and maximum 6185 ms, and 96.6% of gaps were over 5000 ms. 10000 ms is about 1.6 times the longest normal gap and still resets on the first missed push (about 10.8 s).

The plan's candidate defaults of 2000 ms maximum snapshot age and a 1000 ms decision grid assume a faster push rate than the venue gives with `fast: false`. Delivery 1 does not own those values. Delivery 2 must resolve them in a new specification freeze before any new recording.

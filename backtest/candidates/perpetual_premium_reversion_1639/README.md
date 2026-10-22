# Perpetual premium mean reversion study (#1639)

Research-only. `perpetual_premium_reversion` is registered `no_edge` and
`production_refused`: every production config, mode and `allow_no_edge`
acknowledgement is refused until a separate reviewed promotion. Nothing here
changes a live default or a deployed config.

## What is measured

The candidate trades Hyperliquid perpetuals on 1m bars when the premium
(executable bid/ask midpoint divided by the oracle price, minus one) reaches a
strict z-score extreme against the mean of strictly earlier samples and then
turns toward that mean. `candidate_spec.json` freezes every definition, bound,
grid, exit, cost and verdict rule; it was committed before the recording
closed and before any window was scored.

## Inputs

- `data/recording/`: the raw `go-trader record-observations --kinds
  perp_premium` run (gzip copies of every hashed JSONL segment plus the run and
  segment manifests). It holds every public `bbo` and `activeAssetCtx` frame
  with its receipt time and outcome, every finalized 60 s sample and every
  revocation.
- `data/<COIN>_perp_premium.json.gz`: the per-coin sample series that
  `backtest/premium_replay.py` rebuilds from the raw frames; `run_study.py run`
  rebuilds it again and refuses a mismatch.
- `data/<COIN>_1m_candles.csv.gz`, `data/<COIN>_funding.csv.gz`,
  `data/venue_meta.json`, `data/venue_l2_snapshot.json`: public venue data
  pinned by `study_manifest.json` (`offline_candle_manifest/v6`).

The venue asset context carries no source time, so oracle freshness is the
age since the observed context receipt only. No historical archive with the
same cadence, receipt times and alignment was available, so the study uses
only this forward recording.

## Reproduce

The committed `results.json` and `REPORT.md` are historical artifacts. Source
revision `793097a10c1061b2de379b16c9a5271bc551f872` produced them, and their
`source_sha256` pins name that revision's files. #1836 later changed
`event_replay.py`, `run_study.py` and `scheduler/market_feed_premium.go`, so
the current sources no longer match those pins. `run` and `run --render-only`
refuse to overwrite a `results.json` whose pins differ from the current
sources.

`run` writes `results.json` and `REPORT.md` beside its own source, from the
pinned inputs only; it makes no network request. Run it from a scratch copy so
the committed artifacts never change:

```
uv run --no-sync python backtest/offline_manifest.py verify --manifest backtest/candidates/perpetual_premium_reversion_1639/study_manifest.json
uv run --no-sync python backtest/observation_replay.py import-premium --run-dir backtest/candidates/perpetual_premium_reversion_1639/data/recording

# historical reproduction: the outputs match the committed files byte for byte
git worktree add --detach "$SCRATCH/ppr-historical" 793097a10c1061b2de379b16c9a5271bc551f872
uv run --no-sync python "$SCRATCH/ppr-historical/backtest/candidates/perpetual_premium_reversion_1639/run_study.py" run

# corrected run: remove the copied historical outputs first, then run
git worktree add --detach "$SCRATCH/ppr-corrected" HEAD
rm "$SCRATCH/ppr-corrected/backtest/candidates/perpetual_premium_reversion_1639/"{results.json,REPORT.md}
uv run --no-sync python "$SCRATCH/ppr-corrected/backtest/candidates/perpetual_premium_reversion_1639/run_study.py" run
uv run --no-sync python "$SCRATCH/ppr-corrected/backtest/candidates/perpetual_premium_reversion_1639/run_study.py" run --render-only
```

`uv run` from the main checkout supplies the environment; the script puts its
own copy's `backtest/`, `shared_tools/` and candidate directory first on the
import path, so each copy scores its own sources. `$SCRATCH` is any directory
outside the committed study. The freezing steps that produced the inputs were
`init-manifest --start-ms <run start> --close-ms <run close>` (windows follow the frozen rule in `candidate_spec.json`), `acquire`, `prepare --run-dir <recording>` and `finalize`.

## Execution models

- Candle model: the M1 runner (`eval_windows.evaluate_window`) on the frozen
  manifest, next-bar-open fills, manifest execution spec, funding charged for
  every arm, incumbents under the candidate's close and stop contract. It does
  not prove fills at recorded bid/ask.
- Event replay (`event_replay.py`): entries and exits fill at the first
  recorded quote after the decision plus a modeled latency, at the touch,
  limited to displayed size, lot floor and venue minimum, with hourly
  funding. No maker fill is assumed. An entry fills within one 10 s window.
  An exit keeps every fill and asks only for the unfilled quantity in
  successive 10 s windows (each starts at the first quote after the previous
  window) until the position closes or the tape ends. Each quote side's
  displayed size is used once per coin and cost case. The exit price is the
  volume-weighted average of all exit fills; each entry and exit fill pays
  the taker fee once. Funding at each settlement charges the quantity open at
  that settlement. A settlement applies before a fill with the same receipt
  time, so a settlement at the entry fill time is not charged and one at the
  final exit fill time is.
- A position still open when the tape ends is an `open_positions` record
  (entry fills, completed exit fills, remaining quantity, entry and exit
  fees, realized PnL on the closed quantity, funding accrued through the
  last recorded quote). Its remaining quantity is unpriced, and no later
  decision on that coin enters. The event summary statistics count closed
  positions only (`statistics_scope`). When any position is open, `complete`
  is false and `incomplete_reason` is set, and the held-out verdict check
  `event_positions_resolved` then makes the outcome inconclusive.

## Limits

- `time_stop` reads the simulator's held-bar count, which no live close
  context supplies; results carry `comparison_mode: approximate` and claim no
  strict live close parity.
- A socket expiry or disconnect resets the single-session lookback window, so
  each reconnect removes the following lookback span from eligible decisions.
- The recording covers one span of market conditions; a short recording gives
  few independent positions, and the frozen sample floor then makes the
  verdict inconclusive.

## Deviations from the frozen specification

- Recording length. `candidate_spec.json` `data_plan.planned_length` freezes
  "20 hours; the recording may be stopped early, at 12 hours or more". The
  recording ran from 2026-10-21 03:25:15.334Z to 14:15:40.519Z (run manifest
  `started_ms` 1792553115334, `closed_ms` 1792592140519), which is 10.84 h.
  This is 1.16 h below the 12 h early-stop floor. The stop came before any
  scoring, because the windows are derived from the close time. The frozen
  window rule ran unchanged on the recorded start and close times: warm-up
  04:00 to 05:00, train 05:00 to 10:00, test 10:00 to 14:00. The specification
  file and its hash are unchanged. `run_study.py` renders the same deviation
  line into `REPORT.md` (Recording and replay section).
- Provenance re-run. After #1837 (l2_book, seal v7, payload v3) merged, this
  branch moved to seal v8 and market payload v4, and recording schema v3
  became one shared schema with a typed contract block per kind. That changed
  hash-pinned sources (`shared_tools/market_payload.py`,
  `backtest/observation_replay.py`, `backtest/premium_replay.py`,
  `scheduler/market_feed_premium.go`, `scheduler/market_feed_recorder.go`,
  `scheduler/market_feed_ws.go`, `run_study.py`) and added
  `backtest/observation_kinds.py`, which `premium_replay.py` now imports. One
  `run_study.py run` re-ran the study with every frozen rule unchanged: in
  `results.json` only `source_sha256` changed (the new pin included), and in
  `REPORT.md` only the deviation line and the source hashes changed. No
  recorded byte changed.
- Exit accounting repair (#1836). The committed artifacts stay as produced
  at `793097a10c1061b2de379b16c9a5271bc551f872`, because no published
  position used the defective exit retry (zero delayed exits, no refusals).
  Two corrected scratch runs gave byte-identical outputs. Against the
  historical `results.json` they differ in three source hashes
  (`event_replay.py`, `run_study.py`, `scheduler/market_feed_premium.go`)
  and in added fields: `open_positions` per window and cost case; the
  summary fields `complete`, `statistics_scope`, `incomplete_reason`,
  `open_positions`, `open_remaining_qty`, `open_fees_usd`,
  `open_realized_gross_usd`, `open_funding_usd` and `open_realized_net_usd`;
  the trade fields `entry_fills`, `exit_fills` and `exit_windows`; and the
  verdict check `event_positions_resolved` (true). Eight trade-level values
  of two held-out x1 BTC trades also differ in the last binary digits. Each
  trade's exit took two fills in one window, and the old replay used the
  floating-point sum of the fill sizes (`0.011989999999999999` BTC for one
  of them) where the repair uses the exact lot quantity (`0.01199` BTC). The
  changed values are `qty`, `exit_px`, `gross_usd`, `fees_usd` and
  `net_usd`. Every rounded summary value, the verdict and every
  pre-existing `REPORT.md` value are unchanged; the corrected report adds an
  event replay completeness section and the new verdict check line.

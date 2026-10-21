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

```
uv run --no-sync python backtest/offline_manifest.py verify --manifest backtest/candidates/perpetual_premium_reversion_1639/study_manifest.json
uv run --no-sync python backtest/observation_replay.py import-premium --run-dir backtest/candidates/perpetual_premium_reversion_1639/data/recording
uv run --no-sync python backtest/candidates/perpetual_premium_reversion_1639/run_study.py run
uv run --no-sync python backtest/candidates/perpetual_premium_reversion_1639/run_study.py run --render-only
```

`run` rewrites `results.json` and `REPORT.md` from the pinned inputs only; it
makes no network request. The freezing steps that produced the inputs were
`init-manifest --start-ms <run start> --close-ms <run close>` (windows follow the frozen rule in `candidate_spec.json`), `acquire`, `prepare --run-dir <recording>` and `finalize`.

## Execution models

- Candle model: the M1 runner (`eval_windows.evaluate_window`) on the frozen
  manifest, next-bar-open fills, manifest execution spec, funding charged for
  every arm, incumbents under the candidate's close and stop contract. It does
  not prove fills at recorded bid/ask.
- Event replay (`event_replay.py`): entries and exits fill at the first
  recorded quote after the decision plus a modeled latency, at the touch,
  limited to displayed size, lot floor and venue minimum, with hourly
  funding. No maker fill is assumed.

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
  file and its hash are unchanged. `REPORT.md` and `results.json` are as
  scored, and they do not repeat this note.

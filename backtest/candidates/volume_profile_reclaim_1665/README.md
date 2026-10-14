# volume_profile_reclaim (#1665)

Research-only, production-refused candidate. Every production config, mode and `allow_no_edge` acknowledgement is refused
until a separate reviewed promotion, whatever the study shows.

## What it does

The evaluator reads the exact executed-trade volume profile of the previous complete UTC session: base-asset size summed
per canonical Hyperliquid price over every validated accepted trade. `profile_bins` equal-width bins over the session price
range give the point of control (the lowest bin on a tie) and a value area that grows to the adjacent bin with more volume
(the lower side on a tie) until `value_area_fraction` of the volume is included. A long needs the previous closed candle of
the same session to close below the value-area low and the current consecutive closed candle to close strictly inside the
value area; a short is the mirror. `session_bounds=true` is the ablation that uses the session low and high instead.
`candidate_spec.json` is the frozen definition.

## Inputs and where they come from

- Live and shared feeds: the Go feed owner builds the profile on the coin's existing `trades` subscription
  (`scheduler/market_feed_profile.go`) and seals it from seal version 6; the check script reads it only from the sealed
  payload (`volume_profiles`).
- Offline: `backtest/volume_profile_replay.py` rebuilds the same sessions from a `go-trader record-observations --kinds
  trade_flow` run; `offline_candle_manifest/v5` pins the series per dataset.
- Study data: the committed issue-1638 recording (`../cumulative_volume_delta_divergence_1638/data/recording`) replayed
  into `data/profile/`; Hyperliquid 15m candles, funding, meta and an l2Book snapshot in `data/venue/` from
  `offline_manifest.py acquire`; labelled candle-proxy profiles in `data/proxy/`.
- No complete historical trade archive is available here: Hyperliquid node trade archives sit in a requester-pays bucket and
  the build environment has no AWS access. A forward recording cannot hold a complete UTC session while the venue closes the
  socket about every three hours (issue-1638 recording), because the profile never treats a disconnected minute as zero.

## Reproduce

```bash
uv run --no-sync python backtest/offline_manifest.py verify --manifest backtest/candidates/volume_profile_reclaim_1665_proxy_manifest.json
uv run --no-sync python backtest/offline_manifest.py verify --manifest backtest/candidates/volume_profile_reclaim_1665_exact_manifest.json
uv run --no-sync python backtest/candidates/volume_profile_reclaim_1665/run_study.py prepare
uv run --no-sync python backtest/candidates/volume_profile_reclaim_1665/run_study.py run
uv run --no-sync python backtest/candidates/volume_profile_reclaim_1665/run_study.py run --render-only
bash scripts/test_volume_profile_replay.sh
```

`prepare` rewrites the profile series, both manifests and `data/import_report.json` from the committed recording and
candles; a fresh run must reproduce their hashes. `run` refuses to score when the committed exact series differs from a
fresh replay of the recording.

`discovery_before_*.json` (base commit `70b9725d`) and `discovery_after_*.json` are the spot and futures `--list-json`
output; they are byte-identical because discovery hides no-edge names.

## Result

See `REPORT.md`. The primary result is inconclusive: no complete UTC session exists in the executed-trade data, so no arm is
scored on exact profiles. The proxy rows are a labelled sensitivity on candle-distributed volume and never change the
verdict.

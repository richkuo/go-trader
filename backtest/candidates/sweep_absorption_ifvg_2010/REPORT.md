# sweep_absorption_ifvg study (#2010)

**Verdict: INCONCLUSIVE.** Live use stays refused for every verdict; research is suggest-only.

## Summary

`sweep_absorption_ifvg` is a production-refused research candidate: a liquidity sweep of the UTC session low (high), buyer (seller) absorption in session-anchored aggressor volume, a displacement close through the inverse fair value gap and a retest of its consequent encroachment, gated by one-hour swing structure. The specification (`candidate_spec.json`) was frozen before any window was read.

Economic scoring ran: no.

## Verdict reasons

- missing or failed evidence gates: dataset, offline_manifest_15m, offline_manifest_5m, sealed_replay_proof, side_evidence, source_b_recording

## Evidence gates

| Gate | Item | Reason |
| --- | --- | --- |
| dataset | BTC 15m | needs candles, funding, trade_flow and htf_candles in data/offline_15m.json |
| dataset | BTC 5m | needs candles, funding, trade_flow and htf_candles in data/offline_5m.json |
| dataset | ETH 15m | needs candles, funding, trade_flow and htf_candles in data/offline_15m.json |
| dataset | ETH 5m | needs candles, funding, trade_flow and htf_candles in data/offline_5m.json |
| dataset | HYPE 15m | needs candles, funding, trade_flow and htf_candles in data/offline_15m.json |
| dataset | HYPE 5m | needs candles, funding, trade_flow and htf_candles in data/offline_5m.json |
| offline_manifest_15m | data/offline_15m.json | not acquired: the file does not exist |
| offline_manifest_5m | data/offline_5m.json | not acquired: the file does not exist |
| sealed_replay_proof | data/sealed_payloads/index.json | not acquired: the file does not exist |
| side_evidence | data/side_evidence/side_evidence.json | not acquired: the file does not exist |
| source_b_recording | data/recording | no forward trade-flow recording has been captured for this study |

## Source

Source B: forward recording of the public Hyperliquid trades WebSocket stream through the existing trade-flow recorder (go-trader record-observations --kinds trade_flow) and the independent Python replay (backtest/trade_flow_replay.py).

Reason: Source B keeps the verified receipt-cutoff time basis, trade identity and side-evidence contract the sealed live payload uses, so the sealed and replayed decisions are comparable; the requester-pays archive (source A) has no importer, no checked record shape and no proved parity with the websocket contract.

## Frozen protocol

- Required datasets: BTC 5m, ETH 5m, HYPE 5m, BTC 15m, ETH 15m, HYPE 15m (no dataset is dropped to reach a verdict).
- Windows: train 2026-11-16T00:00:00Z to 2027-01-15T00:00:00Z, test 2027-01-15T00:00:00Z to 2027-03-15T00:00:00Z.
- Flow-usable floor 0.8 per dataset and window; denominator: every bar of the scoring window inside the post-session horizon (bar offsets 0 to post_bars - 1 after the session end) of a reference session whose start lies inside the dataset; frozen before results are read.
- Independent positions: at least 30 per held-out dataset (A primary-arm entry from a distinct (reference session, side) setup whose holding period does not overlap the previous counted position on the same dataset; partial exits stay one position.)
- Support horizon (decision bars at the widest grid point): 15m: 80, 5m: 144.
- Arms: market+atr+tiered, market+atr+pool, market+structural+tiered, market+structural+pool, ce_limit+atr+tiered, ce_limit+atr+pool, ce_limit+structural+tiered, ce_limit+structural+pool; primary arm: market + existing owner stack on the real backtester.
- Controls: absorption_disabled, amd_ifvg, liquidity_sweeps (registry default parameters evaluated on causal prefixes of at most 200 bars ending at each decision bar; entries outside the candidate's eligible decisions are dropped; same execution stack, costs and funding).
- Costs: fees, spread, slippage, lot size and the 10 USD minimum from the pinned manifests; stress multiplier 2.0; funding charge.
- Limit fills from candles are a queue assumption; a one-minute volume aggregate proves neither trade-through order nor available quantity.
- Requested risk, initial stop risk and realized loss are reported separately; the Kelly fraction is a diagnostic only.

## Reproduce

```bash
uv run --no-sync python backtest/candidates/sweep_absorption_ifvg_2010/run_study.py run
uv run --no-sync python backtest/candidates/sweep_absorption_ifvg_2010/run_study.py check
```

`check` reruns the study into a scratch copy and compares it byte for byte with the committed `results.json` and `REPORT.md`.

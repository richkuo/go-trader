# cross_exchange_perp_arbitrage study

**Verdict: inconclusive.** `scored` is false. `promotion_eligible` is false. `edge_source` stays `unvalidated`.

## Lock

- Commit recorded by the lock: `0e3966625b657754f089391ae42ccb4a09b2b326`.
- Binary sha256: `91feea68337510ad51b9e67c2275b804dc10de32918ef236ba2e030e45c0e77e`.
- Protocol sha256: `58e553386fa68e4f195b6a0dd61cb3f90df386c036f5ef56b04b06186667736e`.
- Signal specification sha256: `8c61cee1f5d94cc200c19f2a9fffd313909caa838f790665431f78210660c571`.
- Result lock sha256: `819d79e4a845bd27f6cf9c2dd75b1f1a24e368ebcc2ff2201f74cee59ea8762b`.

## Evidence

- no frozen real fee schedule is attached
- no recording manifest or frozen funding document is attached
- no sealed OKX paper recording is frozen
- real OKX history, cross-venue depth, capability, fees or funding evidence is not frozen
- research admission uses synthetic accounts and refuses a private account reader
- snapshot history does not establish continuous cross-venue depth

## Report fields

- source_hashes: source `protocol.lock.json sources`.
- binary_hash: source `protocol.lock.json binary_sha256`.
- data_coverage: missing. no OKX sealed recording and no continuous cross-venue depth are frozen
- windows: source `protocol.json windows`.
- parameters: source `protocol.json parameters`.
- costs: missing. no frozen real fee schedule is attached to this study
- baselines: source `protocol.json baselines`.
- orientations: source `protocol.json orientations`.
- latency: missing. latency cases are declared and not run; no depth history is frozen
- failure_cases: missing. venue failure cases are declared and not run; no depth history is frozen
- booked_net: missing. no booked trades exist for the committed study
- funding_policy: missing. no recording manifest or frozen funding document is attached
- recording_family: missing. no sealed recording is attached
- oracle_visibility: source `a standalone research oracle price is visible at receipt; sealed playback publishes it at the recorded consumption step; neither is a historical venue oracle price`.
- verdict: source `results.json verdict`.

## Baselines and orientations

The declared baselines are no trade and a frozen two-leg schedule. The declared orientations are long Hyperliquid short OKX, and long OKX short Hyperliquid.
A scheduled slot with no entry holds. The candidate policy is not a fallback. A lifecycle conflict holds and does not force a transition.

## Funding and recordings

Funding policy on this result: `None`. Recording family: `None`.
a research receipt price is not a historical venue oracle price
The driver accepts recording manifest versions 1, 2 and 3. A family label changed on a log does not make that log a different family.
scenario.funding_policy is only for a paper collection plan. An offline experiment reads the policy from the recording manifest or the frozen funding document.

## Booked results

No booked net is claimed. An immediate-or-cancel order can stay unfilled when its rounded policy bound excludes the worst consumed price level.
Adapter comparison eligibility is not study equality and does not clear an incomplete reason.

## Commands

```
uv run --no-sync python backtest/candidates/cross_exchange_perp_arbitrage_1655/run_study.py validate
uv run --no-sync python backtest/candidates/cross_exchange_perp_arbitrage_1655/run_study.py lock --binary <go-trader>
uv run --no-sync python backtest/candidates/cross_exchange_perp_arbitrage_1655/run_study.py run --binary <go-trader> --work <fresh-dir>
uv run --no-sync python backtest/candidates/cross_exchange_perp_arbitrage_1655/run_study.py render
```

Production refusal stays enabled. This report does not change a deployed configuration, credential, default, or live trading path.

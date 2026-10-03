# #1500: Decoder smoothing sweep for the `volume:gmm:k=5` regime candidate

**Verdict: the candidate does not ship.** With a three-bar dwell hold and a small
stickiness the model passes the raw `gate_verdict` for the first time (separation and
stability both true), but its permutation p-value of 0.01667 misses the family-wise
Bonferroni alpha of 0.001111. Bonferroni is the binding arm at every in-band setting.
Without smoothing the binding arm is stability, as the issue recorded.

Three context candidates clear the full bar under stickiness 20: `htf:hmm:k=7`,
`all_enriched:kmeans:k=5` and `all_enriched:kmeans:k=7`. They are reported for context
only. The gate code, its thresholds, and the incumbent hand-rule path are byte-identical
to `origin/main` at `17cdacb9`.

## What was run

- Branch base: `17cdacb9` (the #1499 merge). Data: the frozen OHLCV cache used by #1218
  and #1499 (truncated 2026-07-05 20:00 UTC), selected with `GO_TRADER_OHLCV_CACHE_DB`.
- Script: `backtest/research/regime_1500_gmm_stability_sweep.py` (one-shot, registered in
  `docs/backtesting-registry.md`). Artifact: `docs/research/1500-artifacts/regime_1500_btc.json`.
- Command:

```
GO_TRADER_OHLCV_CACHE_DB=<frozen cache> uv run --no-sync python \
  backtest/research/regime_1500_gmm_stability_sweep.py \
  --json docs/research/1500-artifacts/regime_1500_btc.json
```

- Fit settings: BTC/USDT 1h, seed 0, `period=48`, base `filter_window=64`, five eval windows
  (`is`, `oos`, `2023`, `2024`, `2025H1`). Scoring: `n_perm=1799`, Bonferroni denominator 45,
  alpha 0.001111. Non-degeneracy thresholds: 5 active labels, occupancy at most 0.3920,
  transition rate at least 0.0687.
- Incumbent hand-rule on the held-out window: H 85.69, transition rate 0.1374 (volume
  subset), and H 80.47, rate 0.1386 (`htf` and `all_enriched` subsets). A model on the
  volume subset therefore needs H at least 81.41 (0.95 x 85.69) and a transition rate at
  most 0.1174 (0.1374 minus 0.02) to pass the raw gate.

The in-band condition in the tables below means: every eval window keeps every latent
state active, the occupancy and churn floors hold on every window, and the held-out
transition rate is at most the incumbent's rate minus 0.02.

## Decoder options added (both off by default)

`forward_filter_labels` in `backtest/regime_hmm.py` reads two optional model-dict keys.
Both are absent on every existing model, and an absent, `0`, `None`, or `1` value gives a
byte-identical decode (labels and confidence bytes) to the pre-change decoder. The test
`test_forward_filter_without_decode_options_matches_reference_decoder` holds a frozen copy
of the old decoder and proves this.

- `decode_min_dwell` (int, default off). Hysteresis: the emitted label switches to a new
  state only after the filtered argmax has pointed at that same new state for N consecutive
  bars. The filtered posterior is untouched, so confidence still reports the filter's view.
  Look-ahead safety is tested.
- `decode_stickiness` (float, default off). Adds s to the diagonal of the transition
  matrix and renormalises rows for decoding only. The stored model is not mutated.

Both keys live on the model dict, not on the fit path, so they survive a move of the
decoder into `shared_tools/` (#1074): the live decoder reads the same dict and will honour
or ignore the keys the same way. Negative values raise.

## Step 1: `filter_window` sweep (inert)

Every filter window from 8 to 512 gives the identical label stream on every window. The
forward filter forgets its initial distribution within a few bars, so a longer window
only replays the same posterior. The issue's proposed knob has no effect on this candidate.

| filter_window | held-out rate | min active | min rate | held-out H | non-degenerate all |
|---|---|---|---|---|---|
| 8, 16, 32, 64, 128, 256, 512 | 0.2541 | 5 | 0.2395 | 107.82 | yes |

The same holds for every context candidate (rows at 16, 64 and 256 are identical, with
one 0.0014 difference on `htf:hmm:k=7` at 16).

## Step 2: dwell and stickiness sweep on `volume:gmm:k=5`

Dwell rows (filter_window 64; identical at 256):

| dwell | held-out rate | min active | min rate | held-out H | non-degenerate all | in band |
|---|---|---|---|---|---|---|
| 2 | 0.1183 | 5 | 0.1065 | 73.02 | yes | no (rate 0.0009 above the floor) |
| 3 | 0.0793 | 5 | 0.0716 | 78.86 | yes | yes |
| 4 | 0.0567 | 5 | 0.0499 | 75.36 | no (churn floor) | no |
| 6 | 0.0333 | 5 | 0.0300 | 54.56 | no | no |
| 8 | 0.0247 | 5 | 0.0208 | 76.10 | no | no |
| 12 | 0.0129 | 4 | 0.0123 | 105.80 | no | no |
| 24 | 0.0018 | 3 | 0.0012 | 356.24 | no | no |

Dwell 3 is the only dwell inside the band. Dwell 2 stays just above the stability floor
and dwell 4 already breaks the churn floor on at least one window. The band is one bar wide.

Stickiness-only rows (filter_window 64):

| stickiness | held-out rate | min rate | held-out H | in band |
|---|---|---|---|---|
| 1 | 0.2353 | 0.2231 | 103.83 | no |
| 2 | 0.2212 | 0.2112 | 108.88 | no |
| 5 | 0.2049 | 0.1960 | 114.11 | no |
| 10 | 0.1927 | 0.1841 | 115.18 | no |
| 20 | 0.1818 | 0.1712 | 110.71 | no |
| 50 | 0.1720 | 0.1607 | 114.74 | no |
| 100 | 0.1614 | 0.1514 | 113.68 | no |
| 200 | 0.1562 | 0.1447 | 105.59 | no |
| 500 | 0.1494 | 0.1336 | 90.68 | no |
| 1000 | 0.1464 | 0.1258 | 90.71 | no |

Stickiness alone plateaus near 0.15 and never reaches the 0.1174 stability floor, even at
1000 (a transition matrix that is almost the identity). The GMM emissions are separated
enough that the filtered posterior still flips on single bars.

Combined rows (all with five active labels):

| dwell | stickiness | held-out rate | min rate | held-out H | non-degenerate all | in band |
|---|---|---|---|---|---|---|
| 2 | 1 | 0.1199 | 0.1094 | 74.78 | yes | no |
| 2 | 2 | 0.1233 | 0.1100 | 77.28 | yes | no |
| 2 | 5 | 0.1242 | 0.1104 | 80.95 | yes | no |
| 2 | 10 | 0.1267 | 0.1137 | 74.71 | yes | no |
| 2 | 20 | 0.1285 | 0.1132 | 59.59 | yes | no |
| 2 | 50 | 0.1260 | 0.1099 | 66.74 | yes | no |
| 2 | 100 | 0.1233 | 0.1087 | 71.95 | yes | no |
| 3 | 1 | 0.0832 | 0.0737 | 86.13 | yes | yes |
| 3 | 2 | 0.0857 | 0.0757 | 87.58 | yes | yes |
| 3 | 3 | 0.0870 | 0.0769 | 86.81 | yes | yes |
| 3 | 5 | 0.0893 | 0.0761 | 80.28 | yes | yes |
| 3 | 10 | 0.0934 | 0.0792 | 68.06 | yes | yes |
| 3 | 20 | 0.0929 | 0.0817 | 57.55 | yes | yes |
| 6 | 5 | 0.0408 | 0.0348 | 80.65 | no | no |
| 6 | 20 | 0.0447 | 0.0398 | 77.39 | no | no |

Stickiness on top of a dwell hold raises the transition rate slightly (the held state
lasts longer, so the pending run resets less often), which is why dwell 2 plus stickiness
never crosses the floor. Small stickiness (1 to 3) with dwell 3 lifts H above the
separation floor of 81.41; larger stickiness lowers H again.

## Step 3: re-score through the unchanged gate

Best reachable setting (highest held-out H inside the band): `filter_window=64`,
`decode_min_dwell=3`, `decode_stickiness=2`.

| arm | value | threshold | pass |
|---|---|---|---|
| separation, H | 87.58 | at least 81.41 | yes |
| separation, p | 0.01667 | at most 0.05 | yes |
| stability gain | +0.0517 | at least 0.02 | yes |
| raw `verdict.ship` | true | | yes |
| Bonferroni p | 0.01667 | at most 0.001111 | **no** |
| non-degenerate all windows | true | | yes |
| full bar | | | **no** |

The permutation null uses a block length derived from the mean dwell of the model's
labels. Smoothing lengthens the dwell, so the null draws longer blocks, and the same
separation becomes less unusual under the null. The unsmoothed model reached the minimum
achievable p (0.00056); at dwell 3 plus stickiness 2 the p rises to 0.01667. The gain in stability is paid for in significance.

## Step 4: the same sweep on the other Bonferroni-passing BTC candidates

Settings tried per context candidate: filter windows 16, 64, 256; dwell 3 and 6;
stickiness 5 and 20; dwell 6 with stickiness 20. Re-score at the best in-band setting.

| candidate | best in-band setting | held-out rate | H | raw ship | p | Bonferroni | non-degenerate all | full bar |
|---|---|---|---|---|---|---|---|---|
| `volume:hmm:k=7` | dwell 3 | 0.0728 | 164.30 | yes | 0.00222 | no | yes | no |
| `volume:gmm:k=6` | dwell 3 | 0.0762 | 87.80 | yes | 0.03444 | no | yes | no |
| `volume:gmm:k=7` | dwell 3 | 0.0820 | 81.79 | no (p 0.0667 above 0.05) | 0.06667 | no | yes | no |
| `htf:hmm:k=7` | stickiness 20 | 0.0976 | 180.82 | yes | 0.00111 | yes (at the alpha) | yes | **yes** |
| `all_enriched:kmeans:k=5` | stickiness 20 | 0.1180 | 201.07 | yes | 0.00056 | yes | yes | **yes** |
| `all_enriched:kmeans:k=6` | none in band | | | | | | | no |
| `all_enriched:kmeans:k=7` | stickiness 20 | 0.0938 | 283.32 | yes | 0.00056 | yes | yes | **yes** |
| `all_enriched:gmm:k=7` | none in band | | | | | | | no |
| `all_enriched:hmm:k=6` | none in band | | | | | | | no |

Notes on the class:

- Dwell 3 breaks the churn floor on at least one window for every `htf` and
  `all_enriched` candidate (their unsmoothed rates are already lower, 0.12 to 0.17), so
  only stickiness stays in band for them. On the `volume` candidates, whose unsmoothed
  rates are 0.20 to 0.28, only dwell reaches the floor.
- Smoothing helps the class, and it is the `kmeans` and `hmm` families with wide feature
  sets that benefit, because their unsmoothed p-values already sat at the achievable
  minimum and stayed there. The `volume:gmm` family loses significance as soon as it is
  smoothed enough to pass stability.
- `htf:hmm:k=7` passes Bonferroni at exactly the corrected alpha (2 of 1800), the same
  knife-edge #1499 reported for other candidates. `all_enriched:kmeans:k=5` clears the
  stability floor by 0.0006 (gain 0.0206). Both are fragile results.
- `all_enriched:hmm:k=6` fits are slow (about 1000 s each) and no setting stays in band.

## Step 5: gate held fixed

`backtest/regime_calibrate.py` (`gate_verdict`, `SIGNIFICANCE_ALPHA`, `SEPARATION_TOLERANCE`,
`STABILITY_MIN_GAIN`), `backtest/regime_diagnostics.py` (`non_degeneracy`, permutation
null), and the hand-rule path are not touched by this change. `git diff 17cdacb9 -- backtest/regime_calibrate.py backtest/regime_diagnostics.py` is empty.

## Answer to the issue

- Does decoder smoothing bring `volume:gmm:k=5` under the incumbent's transition rate
  while keeping five active labels above the churn floor? Yes, at `decode_min_dwell=3`
  (with stickiness 0 to 20). The window is one dwell value wide.
- Does it then pass the gate? It passes the raw `gate_verdict` at dwell 3 with stickiness
  1 to 3, and fails the family-wise Bonferroni bar at every one of those settings.
  **The candidate does not ship. The binding arm is Bonferroni significance.**
- The issue text quotes the #1218 numbers (transition rate 0.2539, gain -0.1165). The
  current record after #1499 is rate 0.2541 and gain -0.1167, reproduced here at every
  filter window. The difference does not change any conclusion.
- Because the candidate does not ship, #1081 and #1082 are not triggered by it. The three
  context candidates that clear the full bar under stickiness 20 are new evidence and are
  not promoted by this document. A follow-up that re-runs the #1499 bake-off with
  `decode_stickiness=20` on every candidate, on both symbols, and with a higher `n_perm`
  to get off the alpha edge, would be the next research step. That follow-up is unfiled.

---
Created with LLM: Fable 5.1 | low | Harness: Claude Code

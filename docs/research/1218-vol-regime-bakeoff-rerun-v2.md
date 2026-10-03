# #1218 — Re-run of the #1080/#1095 vol-regime bake-offs under candidate-self-v2 gate semantics

**Verdict: NEGATIVE — no candidate ships.** Under the #1211 candidate-self-v2 gate
(`ship = separation_ok and stability_ok`, judged on the candidate's own block-shuffle
permutation significance + non-inferiority + stability gain), no k-means / GMM / HMM
candidate on any feature subset survives the full promotion bar (raw `verdict.ship` +
family-wise Bonferroni significance + non-degeneracy on all five eval windows) on
BTC or ETH. **#1074 blocker 2 is now measured, and it failed.**

The failure mode changed, though: the pre-#1211 runs abstained every verdict on the
incumbent-trustworthy hard-gate. Under v2, `incumbent_trustworthy=true` everywhere and
many candidates now pass the raw gate — what kills every one of them is **non-degeneracy**
(collapsing to fewer active labels than the incumbent-derived floor), and in one notable
BTC case **stability** (more label churn than the hand-rule).

## Commands (harnesses unchanged — no gate or threshold edits)

Run at repo SHA `e7d4660` (branch base = `origin/main`), 2026-07-05:

```
uv run --no-sync python backtest/research/regime_1080_unsupervised_vol_model.py \
    --json docs/research/1218-artifacts/regime_1080_btc.json
uv run --no-sync python backtest/research/regime_1095_enriched_vol_model.py \
    --json docs/research/1218-artifacts/regime_1095_btc.json
uv run --no-sync python backtest/research/regime_1095_enriched_vol_model.py \
    --symbol ETH/USDT --timeframe 1h \
    --json docs/research/1218-artifacts/regime_1095_eth.json
```

Full per-candidate reports (every `verdict`, non-degeneracy breakdown, states/mappings)
are the three JSON artifacts in `docs/research/1218-artifacts/`. Each 1095 run swept the
full 90-candidate family (5 subsets × 3 families × K=2..7) in one invocation, so the
Bonferroni correction divides alpha across the combined structurally-eligible grid.

## Resolved permutation counts and corrected alphas

| Run | swept | structurally eligible (Bonferroni denom) | resolved `n_perm` | corrected alpha | min achievable p |
|---|---|---|---|---|---|
| 1080 BTC/USDT 1h | 18 | 9 | 1000 | 0.005556 | 0.000999 |
| 1095 BTC/USDT 1h | 90 | 45 | 1799 | 0.001111 | 0.000556 |
| 1095 ETH/USDT 1h | 90 | 30 | 1199 | 0.001667 | 0.000833 |

All five 1095 feature subsets built successfully on both assets
(`subset_status`: `canonical`/`funding`/`volume`/`htf`/`all_enriched` all `"ok"` —
no funding-unavailable arm). Incumbent hand-rule held-out separation was significant
in all three runs (`knife_edge=false`; BTC p=0.0044, steps_to_alpha=82; ETH p=0.0058,
steps_to_alpha=53; 1080 run p=0.0060, steps_to_alpha=44), so v2's non-inferiority
arm was compared against a live, non-abstained incumbent.

## Results

### #1080 (canonical four features, BTC 1h): winner = none

`verdict.ship=true` for 3 of 18 candidates (hmm k=6, kmeans k=3, kmeans k=4), but:

- **No candidate passes the corrected alpha** — best eligible p=0.01299 (gmm k=6/k=7)
  vs alpha 0.005556.
- **Every one of the 18 candidates is degenerate on at least one eval window**
  (`non_degenerate_all=false` across the board), overwhelmingly
  `min_active_labels: 4 < 5` — the models persistently occupy only 4 states.

### #1095 (enriched features): winner = none on both assets; every subset `any_ships=false`

**BTC:** 15/90 raw `verdict.ship=true`; 7 pass Bonferroni; only 2 are non-degenerate on
all windows — and the three sets never intersect. Closest misses:

- `htf:hmm:k=6` — passes everything except non-degeneracy: p=0.00056 (min achievable),
  KW-H=137.9 vs incumbent 85.7, stability gain +0.065, `ship=true`, Bonferroni pass —
  but sits at exactly 4 active labels on **all five** windows (floor is 5).
- `volume:gmm:k=5` — the inverse miss: non-degenerate on **all** windows AND
  Bonferroni-significant (p=0.00056, KW-H=107.8), but the **stability arm** fails —
  it churns **more** than the hand-rule (stability gain −0.1165, i.e. transition rate
  ≈0.254 vs the incumbent's 0.137), so `ship=false`.

**ETH:** 60/90 raw ships; 5 pass Bonferroni; **zero** non-degenerate on all windows
(ETH's incumbent-derived floor is 6 active labels). Closest misses — all three
ship+Bonferroni candidates (`canonical:kmeans:k=7`, `funding:kmeans:k=6`,
`all_enriched:kmeans:k=6`) fail only non-degeneracy: 3–4 active labels on every
window (plus `max_occupancy` breaches for the funding/all_enriched arms).

### Which arm failed (the record #1218 asked for)

- **Separation significance:** clearable — several candidates hit the minimum
  achievable p under the family correction on both assets.
- **Non-inferiority (KW-H vs incumbent):** clearable — e.g. BTC `htf:hmm:k=6`
  at 137.9 vs 85.7.
- **Stability:** the binding arm for the best BTC volume-subset models (GMM churns
  more than the hand-rule).
- **Non-degeneracy:** the binding arm for everything else — the single dominant
  failure. Unsupervised fits keep collapsing to ~4 effective states, below the
  incumbent-derived `min_active_labels` floor (5 on BTC, 6 on ETH).

## Disposition

- **#1074 blocker 2: measured negative.** No hand-off to the #1081 economic gate or
  #1082 bounded-window validation — nothing shipped. Live wiring stays blocked.
- No gate-semantics or threshold change proposed here (out of scope per #1218; any such
  change belongs to #1211's lineage). If a future issue wants to pursue the near-misses,
  the evidence points at the state-collapse problem (fit-time K ≠ decoded effective
  states), not at significance power — `n_perm` resolution is no longer the binding
  constraint anywhere.

---
Created with LLM: Fable 5 | high | Harness: Claude Code

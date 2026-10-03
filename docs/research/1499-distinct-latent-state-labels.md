# #1499: Re-run of the #1080/#1095 vol-regime bake-offs with distinct latent-state labels

**Verdict: POSITIVE for the first time. Five candidates clear the full promotion bar** (raw
`verdict.ship`, family-wise Bonferroni significance, non-degeneracy on all five eval windows):
two on BTC (`htf:hmm:k=6`, `htf:kmeans:k=7`) and three on ETH (`canonical:kmeans:k=7`,
`funding:kmeans:k=7`, `all_enriched:hmm:k=7`). The gate code, its thresholds, and the
incumbent hand-rule path are unchanged. The only change is how a fitted model names its
latent states.

## What changed and why

The #1218 runs named every latent state by pushing its centroid through the hand-rule
`map_composite_label`. Two or more centroids that fall in the same hand-rule cell received
the same name, so the decoder emitted the same string for separate states. `non_degeneracy`
counts distinct emitted labels, so a k=6 model that kept six separate states was scored as
four active labels and failed the floor (5 on BTC, 6 on ETH). In #1218, 9 of 18 (1080),
48 of 90 (1095 BTC) and 60 of 90 (1095 ETH) candidates carried duplicate names.

`regime_vol_model.py` now applies the naming rule `distinct_nearest_cell`
(`MODEL_VERSION=2`):

- A state whose hand-rule name is uncontested keeps that name.
- Colliding states are resolved by a minimum-cost assignment over the nine hand-rule
  cells. The cost is the z-scaled distance from the centroid to the nearest point of each
  cell (per-feature violation divided by the in-sample feature standard deviation), plus
  a large fixed penalty per relabel, so the assignment first minimises the number of
  relabeled states and then the total displacement.
- k larger than the nine-label vocabulary falls back to hand-rule names and records the
  duplicates in `naming.duplicate_names`.
- Every model records `mapping[i].handrule_name`, `relabeled`, and `displacement_z`, and a
  `naming` summary with the relabeled states. Every harness artifact carries it.

`non_degeneracy`, `derive_thresholds`, `gate_verdict`, the alpha resolution and the
permutation-count resolution are byte-identical to #1218.

## Commands (harnesses unchanged apart from recording `naming`)

Run at branch base `87430501` (`origin/main`), 2026-09-01, against a frozen copy of the
OHLCV cache truncated at 2026-07-05 20:00 UTC (the last bar the #1218 run saw), selected
through `GO_TRADER_OHLCV_CACHE_DB`:

```
uv run --no-sync python backtest/research/regime_1080_unsupervised_vol_model.py \
    --json docs/research/1499-artifacts/regime_1080_btc.json
uv run --no-sync python backtest/research/regime_1095_enriched_vol_model.py \
    --json docs/research/1499-artifacts/regime_1095_btc.json
uv run --no-sync python backtest/research/regime_1095_enriched_vol_model.py \
    --symbol ETH/USDT --timeframe 1h \
    --json docs/research/1499-artifacts/regime_1095_eth.json
```

Full per-candidate reports are the three JSON artifacts in `docs/research/1499-artifacts/`.

## Incumbent reproduction against the #1218 artifacts

| Run | swept | eligible (Bonferroni denom) | `n_perm` | corrected alpha | incumbent p | steps to alpha | transition rate |
|---|---|---|---|---|---|---|---|
| 1080 BTC/USDT 1h | 18 | 9 | 1000 | 0.005556 | 0.005994 | 44 | 0.137353 |
| 1095 BTC/USDT 1h | 90 | 45 | 1799 | 0.001111 | 0.004444 | 82 | 0.137353 |
| 1095 ETH/USDT 1h | 90 | 30 | 1199 | 0.001667 | 0.005833 | 53 | 0.140752 |

Every value in this table, the non-degeneracy thresholds, and all five
`subset_status="ok"` flags are identical to #1218. Among the candidates whose state names
did not change (9 of 18 in 1080, 40 of 90 on BTC, 30 of 90 on ETH), every `canonical`,
`funding` and `htf` candidate reproduces its #1218 p-value and stability gain exactly.

The incumbent Kruskal-Wallis H differs in the fourth significant figure (BTC canonical
85.6906 in #1218 vs 85.6949 here; ETH 85.2101 vs 85.2110). The cause is the data and it
is measured. The last cached candle (2026-07-05 20:00 UTC) was still in progress when
#1218 ran. Replacing its final close with the in-progress close (BTC 62719.11 for the
finalized 62731.66; ETH 1777.81 for 1777.67) reproduces the #1218 H to the last digit in
both runs. The incumbent label streams are identical, so every gate arm that depends on
them is identical. The same partial candle also changes the volume and combined
subsets at the tail of the held-out window: among unchanged-name candidates, three BTC
`volume` candidates move their stability gain by at most 0.0002, and the `all_enriched`
candidates move gain by at most 0.0026 and p by at most 0.024 on BTC (on the
structurally ineligible `kmeans:k=3`) and 0.0017 on ETH. The only arm flip among
unchanged-name candidates is BTC `all_enriched:kmeans:k=3` (ineligible) gaining raw
`ship`. No full-bar result depends on these tail differences.

## Results

### Duplicate names and arm counts, #1218 vs this run

| Run | duplicate-name candidates | raw `ship` | Bonferroni | non-degenerate all windows | full bar |
|---|---|---|---|---|---|
| 1080 BTC | 9 → 0 (of 18) | 3 → 6 | 0 → 0 | 0 → 8 | 0 → 0 |
| 1095 BTC | 48 → 0 (of 90) | 15 → 25 | 7 → 15 | 2 → 31 | 0 → 2 |
| 1095 ETH | 60 → 0 (of 90) | 60 → 61 | 5 → 15 | 0 → 18 | 0 → 3 |

### #1080 (canonical four features, BTC 1h): winner = none

Six candidates ship and eight are non-degenerate on all windows, but none reaches the
corrected alpha 0.005556. The closest are `gmm:k=5` (p 0.005994, non-degenerate, but its
stability gain +0.0129 is below the 0.02 floor so it does not ship) and `hmm:k=6`
(ships, non-degenerate on all windows, p 0.007992). The 1080 grid has only 9 eligible
candidates and 1000 permutations, so the corrected alpha sits close to the minimum
achievable p.

### #1095 BTC: winner = `htf:hmm:k=6`

Candidates clearing the full bar:

| candidate | ship | separation | stability (gain) | Bonferroni (p vs 0.001111) | non-degenerate all | KW-H |
|---|---|---|---|---|---|---|
| `htf:hmm:k=6` | yes | yes | yes (+0.0349) | yes (0.001111) | yes, 6 active on all five windows | 179.8 |
| `htf:kmeans:k=7` | yes | yes | yes (+0.0378) | yes (0.001111) | yes, 7 active on all five windows | 176.1 |

Both p-values equal the corrected alpha exactly: one of 1799 block-shuffled permutations
matched or exceeded the observed H, so p = 2/1800. This is a knife-edge pass on the
significance arm and the record should carry it forward.

The #1218 BTC near-misses, arm by arm:

- `htf:hmm:k=6`: in #1218 it failed only non-degeneracy (4 active labels on all five
  windows because states 0 and 4 both read `trending_up_choppy` and states 2 and 5 both
  read `trending_down_choppy`). Now state 0 is `ranging_directional` (from
  `trending_up_choppy`, 0.80 z) and state 5 is `ranging_directional_down` (from
  `trending_down_choppy`, 0.18 z). It passes separation, stability, Bonferroni and
  non-degeneracy. Its p moved from 0.00056 to 0.00111 and its stability gain from +0.0647
  to +0.0349, because the decoded stream now switches between six names instead of four.
- `volume:gmm:k=5`: no collision, so nothing changed. It passes separation, Bonferroni
  (p 0.00056) and non-degeneracy, and still fails stability (gain −0.1167). That is #1500.

New BTC near-misses (pass every arm but one):

- Stability only: `htf:hmm:k=7` (gain +0.0190, floor 0.02), `volume:hmm:k=7`,
  `volume:gmm:k=6`, `volume:gmm:k=7`, `all_enriched:kmeans:k=5/6/7`,
  `all_enriched:gmm:k=7`, `all_enriched:hmm:k=6`.
- Bonferroni only: `canonical:hmm:k=6` (p 0.00722), `funding:kmeans:k=7` (p 0.00222),
  `funding:kmeans:k=6`, `canonical:kmeans:k=6`, `canonical:hmm:k=5`, `htf:hmm:k=5`.
- Two arms, Bonferroni and non-degeneracy: `htf:kmeans:k=6` (p 0.00167; six states on
  every window, but occupancy or transition rate breaches the floor on three of them),
  `htf:kmeans:k=5`.

### #1095 ETH: winner = `funding:kmeans:k=7`

Candidates clearing the full bar:

| candidate | ship | separation | stability (gain) | Bonferroni (p vs 0.001667) | non-degenerate all | KW-H |
|---|---|---|---|---|---|---|
| `funding:kmeans:k=7` | yes | yes | yes (+0.0279) | yes (0.00083) | yes, 7 active on all five windows | 176.2 |
| `canonical:kmeans:k=7` | yes | yes | yes (+0.0351) | yes (0.00083) | yes, 7 active on all five windows | 173.8 |
| `all_enriched:hmm:k=7` | yes | yes | yes (+0.0204) | yes (0.00167) | yes, 7 active on all five windows | 146.0 |

`funding:kmeans:k=7` and `canonical:kmeans:k=7` are near-identical models (the funding
centroid coordinate is zero to four decimals on every state), so the funding column adds
almost nothing on ETH. `all_enriched:hmm:k=7` passes Bonferroni at exactly the corrected
alpha and stability at +0.0204 against the 0.02 floor, so both of those arms are
knife-edge.

The #1218 ETH near-misses, arm by arm:

- `canonical:kmeans:k=7`: in #1218 it failed only non-degeneracy (three of its seven
  states read `trending_up_choppy` and two read `trending_down_choppy`). Now states 4, 5
  and 6 are `trending_up_clean` (0.76 z), `ranging_directional_up` (0.44 z) and
  `ranging_directional` (1.05 z). It clears the full bar.
- `funding:kmeans:k=6`: in #1218 it failed only non-degeneracy. Now it is non-degenerate
  on all windows and still ships (gain +0.0392), but its p moved from 0.00167 to 0.00333
  and misses the corrected alpha 0.001667. It fails Bonferroni only.
- `all_enriched:kmeans:k=6`: in #1218 it failed only non-degeneracy. Now it is
  non-degenerate on all windows and passes Bonferroni (p 0.00083), but its stability gain
  moved from +0.0314 to −0.0141, so it fails stability only.

New ETH near-misses:

- Stability only: `all_enriched:kmeans:k=7` (gain −0.0009), `all_enriched:hmm:k=6`
  (+0.0155), `all_enriched:gmm:k=6`, `funding:gmm:k=7`, `volume:gmm:k=7`.
- Non-degeneracy only: `htf:hmm:k=6` (2023 window transition rate 0.0684 vs floor 0.0695),
  `htf:hmm:k=7` (three windows under the transition-rate floor).
- Bonferroni only: `funding:kmeans:k=6` (above), `htf:gmm:k=7` (p 0.0075).

### Which arm binds now

- **Non-degeneracy** was the dominant failure in #1218. With distinct names it binds only
  where a model's decoded stream really is degenerate (`kmeans:k=3/4` on BTC with three or
  four states and occupancy over 0.39; ETH `htf:hmm` at the transition-rate floor).
- **Stability** is now the most common single failing arm for the high-separation models
  (BTC volume and all_enriched subsets, ETH all_enriched). Distinct names raise the
  measured transition rate, because a switch between two states that used to share a name
  now counts as a transition. Several #1218 stability passes turned into failures for
  this reason. That is a true property of the model, and #1218 was under-counting it.
- **Bonferroni** binds on the 1080 grid and on several BTC `canonical`/`funding` models
  whose p sits between 0.002 and 0.008.

## Does the corrected mapping change what a label means?

Yes, for relabeled states only. Every uncontested state keeps its hand-rule name and its
meaning is unchanged. A relabeled state carries the name of the nearest free hand-rule
cell, measured in in-sample standard deviations, and its centroid does **not** satisfy that
cell's rule. The name says where the state sits in the vocabulary. It does not say the
hand-rule condition holds. Examples from the passing models (out-of-sample median forward
realized volatility at horizon 4, in log-return units):

BTC `htf:hmm:k=6` (states sorted by forward vol):

| state | name | hand-rule name | displacement | centroid (ret_eff, range_eff, eff, adx) | oos bars | median fwd vol |
|---|---|---|---|---|---|---|
| 3 | `ranging_volatile` | same | 0 | 0.000, 0.118, 0.041, 21.1 | 1101 | 0.00555 |
| 0 | `ranging_directional` | `trending_up_choppy` | 0.80 z | 0.069, 0.136, 0.145, 23.8 | 809 | 0.00603 |
| 5 | `ranging_directional_down` | `trending_down_choppy` | 0.18 z | −0.066, 0.146, 0.137, 28.4 | 675 | 0.00616 |
| 4 | `trending_up_choppy` | same | 0 | 0.152, 0.204, 0.303, 37.3 | 541 | 0.00630 |
| 2 | `trending_down_choppy` | same | 0 | −0.130, 0.189, 0.261, 41.6 | 842 | 0.00753 |
| 1 | `ranging_directional_up` | same | 0 | 0.008, 0.139, 0.093, 28.2 | 293 | 0.00914 |

State 0 is a weak up-drift with ADX below 25. The hand rule calls it
`trending_up_choppy` because return efficiency clears 0.05. The nearest free cell is
`ranging_directional` (return efficiency at zero, ADX at 25), 0.80 z away. Its forward vol
sits between `ranging_volatile` and the true `trending_up_choppy` state, so the model
separates it from state 4 for a reason, and the old mapping was merging two states with
different forward vol.

ETH `funding:kmeans:k=7`:

| state | name | hand-rule name | displacement | centroid (ret_eff, range_eff, eff, adx) | oos bars | median fwd vol |
|---|---|---|---|---|---|---|
| 1 | `ranging_volatile` | same | 0 | 0.005, 0.121, 0.051, 17.9 | 983 | 0.00669 |
| 5 | `ranging_directional_up` | `trending_up_choppy` | 0.44 z | 0.073, 0.150, 0.153, 20.6 | 600 | 0.00788 |
| 0 | `ranging_directional_down` | same | 0 | −0.029, 0.152, 0.092, 27.1 | 1072 | 0.00814 |
| 6 | `ranging_directional` | `trending_down_choppy` | 1.07 z | −0.111, 0.182, 0.220, 34.4 | 673 | 0.00840 |
| 2 | `trending_up_choppy` | same | 0 | 0.138, 0.211, 0.269, 32.0 | 440 | 0.00881 |
| 4 | `trending_up_clean` | `trending_up_choppy` | 0.90 z | 0.205, 0.255, 0.394, 46.3 | 177 | 0.01035 |
| 3 | `trending_down_choppy` | same | 0 | −0.178, 0.245, 0.343, 47.8 | 464 | 0.01137 |

State 4 is the strongest up-trend state (ADX 46, efficiency 0.39). The hand rule calls it
`trending_up_choppy` because efficiency is under 0.5. `trending_up_clean` is the nearest
free cell and the forward vol ordering agrees that it is a separate, higher-vol state.
State 6 is a mid-strength down-trend that the vocabulary has no free cell for after the
true `trending_down_choppy` state takes its name, so it lands on `ranging_directional`
1.07 z away. This name is the least faithful of the set, and a consumer must read
`handrule_name` to see that the state is a down-trend.

The same per-state pass was run for every ship+Bonferroni candidate and every #1218
near-miss. The refit reproduced the artifact's state names for every candidate except
the two ETH `all_enriched` ones (`hmm:k=7`, `kmeans:k=6`), where the refit swapped the
order of two states with the same hand-rule name. Those two candidates' per-state rows
are therefore not reported here; their arm results above come from the artifact.

Consequence for #1074 live wiring: a model label must be consumed through
`mapping[i]`. String-matching the name against the hand-rule vocabulary is unsafe.
`relabeled=true` means the name is a position in the vocabulary and the hand-rule
condition for that name is false at the centroid. Any live gate that keys on model labels
(for example an `allowed_regimes` list) has to be authored per model from the mapping,
and a model with a relabeled state must not share an `allowed_regimes` list with the
hand-rule classifier.

## Disposition

- **#1074 blocker 2 is now measured positive on both assets.** BTC `htf:hmm:k=6` and ETH
  `funding:kmeans:k=7` (with `canonical:kmeans:k=7` as an equivalent) clear the full bar.
  Both BTC passers and ETH `all_enriched:hmm:k=7` sit exactly on the corrected alpha, so
  the significance evidence is thin; the ETH k-means pair pass at half the alpha.
- Hand-off to the #1081 economic gate and #1082 bounded-window validation is now
  possible for those candidates. Nothing in this issue promotes a model or writes live
  config.
- Stability is the arm to watch next. Distinct names raised measured churn across the
  board, and #1500 (`volume:gmm:k=5` churn) applies to more candidates than it did under
  the old mapping.
- No gate-semantics or threshold change is proposed. The knife-edge Bonferroni passes are
  a property of `n_perm` resolution at 1799 permutations; raising `n_perm` for a
  confirmation run would tighten the p-value estimate without changing the gate.

---
Created with LLM: Fable 5.1 | high | Harness: Claude Code

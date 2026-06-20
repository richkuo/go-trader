# #1076 — Does the regime label predict forward DIRECTION? (Negative result)

**Validates the edge premise behind `regime_directional_policy` (#779) and regime
directional entry-gating (`allowed_regimes`).** Those surfaces bet a live HL-perps
strategy long/short on the *current* regime label (long in `trending_up`, short in
`trending_down` — `scheduler/regime_directional_policy.go:5-16`). The entire premise is
**regime → forward-direction**. #1073 finding 1 refuted it for the 7-state composite
classifier on BTC/USDT 1h; this issue generalizes the test across assets, timeframes,
both classifiers, and an economic walk-forward — statistically and economically.

## Verdict

**Negative across the entire tested universe.** The regime → direction premise has **no
statistically real, multiplicity-honest forward-return separation** and **no economic edge
over its own shuffled-label null** anywhere tested. The directional-gating surface is
choosing long vs short on noise; its only realized effect is a change in market exposure
(defensive beta), not a directional forecast.

## Universe tested

| Axis | Values |
|---|---|
| Exchange / fee model | `binanceus` (eval_windows audit platform) |
| Assets | BTC, ETH, SOL (1h + 4h); BTC also 15m/30m/2h; BNB, XRP (4h) |
| Windows | `is`, `oos` (2025-06→2026, the held-out forward split), `2023`, `2024`, `2025H1` |
| Classifiers | `adx` (3-state, period 14, the policy-doc default) **and** `composite` (7-state, period 48, the #1073 surface) |
| Horizons | 1, 4, 8, 12, 24, 48, 72 bars |
| Block-shuffle perms | 1000 (scope 1), 300 (scope 2 placebo) |

## Scope 1 — per-state forward-return significance

`regime_1076_directional_premise.py` reuses `regime_diagnostics.py:per_state_significance`
(block-shuffle + Benjamini-Hochberg FDR). For each (classifier, asset, timeframe, window,
horizon, state) it tests whether that state's forward return separates from the rest, and
flags a **candidate edge** only when the state is FDR-significant **and** its gap sign
matches the side the policy bets (long states want gap > 0, short states gap < 0).

`per_state_significance` corrects FDR only *within* a cell. Running ~300 cells is a family
of **2121 directional-state tests**, so within-cell hits are expected by chance.
`regime_1076_aggregate.py` pools every test and corrects **once**, globally.

```
total directional-state tests pooled: 2121
  within-cell candidate edges (uncorrected):           20  (held-out is/oos: 1, oos: 1)
  GLOBAL Benjamini-Hochberg FDR q=0.05:    0 survive (0 policy-aligned)
  GLOBAL Bonferroni  (p <= 2.36e-05):      0 survive (0 policy-aligned)
```

- **0 of 2121** states survive global correction (BH or Bonferroni).
- The 20 within-cell candidate edges cluster in single historical windows (mostly SOL 4h
  2023) and at correlated overlapping horizons — exactly multiple-comparisons noise. Only
  **one** lands in a held-out forward window (BTC 4h `oos`, `trending_down_clean`, h1,
  p=0.004), a single-bar artifact that no multi-bar policy can bank.
- For the composite classifier, FDR-significant states are **wrong-signed as often as
  aligned** (e.g. core: 6 aligned vs 9 wrong-signed) — a state "predicting" direction
  opposite to the policy's bet is noise, not signal.

## Scope 2 — economic walk-forward (the real arbiter)

`regime_1076_economic_sim.py` is a look-ahead-safe regime-timing portfolio: three books on
identical bars, each side decided from the regime known at the **prior** bar close
(mirrors the backtester's regime `shift(1)`, #730):

- `policy` — long in `trending_up*`, **short** in `trending_down*`, flat (or long) in `ranging*`
- `long_only` — long in `trending_up*`, flat otherwise (isolates "short the downtrend" value)
- `buyhold` — long every bar (the regime-agnostic base)

It prices the bare directional premise with **no strategy-signal confound** — if even
continuously applying the regime's directional call can't beat buy-and-hold risk-adjusted
out-of-sample, the premise confers no economic value on any strategy that uses it for side
selection. Shorting funding cost is omitted (favors `policy`, so a loss is conservative);
fees are charged on turnover (10 bps/side default).

**The naive read is a trap.** `policy` "beats buyhold on Sharpe AND DDadj 12/12 in `oos`."
But `oos` (2026 H1) was a *down* market — buy-and-hold is negative in every `oos` cell —
and `policy` only "wins" by being defensively flat/short. In the *bull* windows it is
destroyed (e.g. SOL 1h 2023: buy-and-hold **+937%** vs policy **−32%**; BTC 1h 2024:
**+121%** vs **−58%**), and its *absolute* Sharpe is negative in most cells. A book whose
sign-of-outperformance flips entirely on the sample's drift is reduced beta, not direction
skill.

**Placebo control settles it.** Block-shuffle the policy's per-bar side decisions
(preserving the long/short/flat mix and dwell, destroying the alignment with price) and ask
whether the real policy's Sharpe beats its own shuffled null:

```
ranging=flat:  cells beating shuffled null  raw p<=0.05: 7/60   after BH FDR: 0/60
ranging=long:  cells beating shuffled null  raw p<=0.05: 3/60   after BH FDR: 0/60
```

**0 of 60** cells (either ranging mode) show regime timing beating its own shuffled null
after FDR. The economic "wins" are the exposure mix (defensive beta in a down sample), not
regime → direction skill.

## Reproduce

```bash
# Scope 1 — per-state significance (per-battery global correction in each run)
uv run --no-sync python backtest/research/regime_1076_directional_premise.py \
    --symbols BTC/USDT,ETH/USDT,SOL/USDT --timeframes 1h,4h --n-perm 1000 --out /tmp/core.json
uv run --no-sync python backtest/research/regime_1076_directional_premise.py \
    --symbols BTC/USDT --timeframes 15m,30m,2h --n-perm 1000 --out /tmp/btc_extra.json
uv run --no-sync python backtest/research/regime_1076_directional_premise.py \
    --symbols BNB/USDT,XRP/USDT --timeframes 4h --n-perm 1000 --out /tmp/alt4h.json
# Unified global correction across the FULL battery
uv run --no-sync python backtest/research/regime_1076_aggregate.py \
    /tmp/core.json /tmp/btc_extra.json /tmp/alt4h.json

# Scope 2 — economic isolation + placebo control
uv run --no-sync python backtest/research/regime_1076_economic_sim.py \
    --symbols BTC/USDT,ETH/USDT,SOL/USDT --timeframes 1h,4h --ranging-mode flat --placebo-perm 300
```

Read-only; no live or Go path touched. (A look-ahead bug in the first economic-sim draft —
using `labels[t]` to trade the move *into* bar `t` — produced impossible Sharpe ~9; the
committed `_book` decides the side at bar `t` and holds it over `t→t+1`, verified by a
buy-and-hold book reproducing the asset return exactly.)

## Action taken (#1076 scope 3)

The premise holds **nowhere** in the tested universe, so the directional-gating surface must
not be deployed believing it has validated directional edge.

**Implemented: a non-breaking operator warning** (`regimeDirectionalPolicyWarnings`,
`scheduler/config.go`) — every strategy that configures `regime_directional_policy` prints a
`[WARN]` at config load citing this negative result and pointing operators to ATR-scaled
sizing (#1078). Existing live configs still load.

**Why warn, not deprecate/restrict — a verified safety inversion.** Hard-rejecting the keys
or auto-disabling is the *less* safe option for the money path. Disabling the policy on a
strategy with an open position relies on the #822 orphan auto-close
(`perpsRegimeDirectionOrphanConflict` → `runRegimeDirectionOrphanCloses`,
`scheduler/hyperliquid_balance.go`), which fires **only for sole-owner coins** — a shared-coin
live short would be **stranded** for manual close. SIGHUP already blocks adding/removing the
policy while a position is open (`scheduler/config_reload.go`), so the safe disable path is
operator-driven **from flat**. A warning prompts exactly that without forcing an unsafe state
transition; a blanket reject would not.

**Deliberate follow-up (recommended new issue):** an evidence-gated, default-off migration —
the directional surface activates only where a per-(asset, timeframe) validation gate
certifies real edge (none currently qualifies), mirroring the #1073/#1078 `gate_verdict`
"abstain unless trustworthy" philosophy. That is a live-money-behavior change deserving its
own design (shared-coin orphan handling + hot-reload + a from-flat migration), not a bolt-on
to this validation.

The regime classifier's real, validated signal is forward **volatility**, not direction
(#1073 finding + #1078) — regime should drive ATR-scaled SL/TP sizing, not side selection.

### Note on the economic method
Scope 2 uses a look-ahead-safe regime-timing **isolation** (always-in-market, side from the
prior bar's regime) rather than the literal `Backtester` + `regime_directional_policy` config.
This is deliberate: the isolation removes the strategy-signal confound and, critically,
supports the **block-shuffle placebo** that actually separates regime-timing skill from
defensive beta — a control the strategy-confounded backtester cannot cleanly provide. A
literal-`Backtester` run is feasible (`run_backtest.py --config` threads the policy via the
#1025 resolver) and remains available as corroboration of the real-usage entry pattern; given
the statistical screen (0/2121) and the placebo (0/60), it is not expected to differ.

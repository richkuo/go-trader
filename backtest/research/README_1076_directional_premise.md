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

## Refreshed run — 2026-08-22, HYPE added (#1443)

The #1085 producer was re-run over the full default universe **plus** HYPE sourced
from Hyperliquid, in **one invocation**, so the Benjamini-Hochberg family is a
superset of the previous certify screen. Command:

```bash
uv run --no-sync python backtest/research/regime_1076_certify.py \
    --symbols "BTC/USDT,ETH/USDT,SOL/USDT,HYPE/USDC:USDC@hyperliquid" \
    --n-perm 30000 --seed 0
```

Full per-cell output: `regime_1443_run_report.json`. Artifact:
`regime_directional_certifications.json`.

### Verdict — still 0 certified, and this time the arithmetic permits one

**0 of 16 screened cells certify. Every cell fails at the first gate, global BH.**

The important difference from earlier runs is the *resolution*.
`per_state_significance` reports `p = (ge+1)/(n_perm+1)`, so its smallest possible
p-value is `1/(n_perm+1)`. At `n_perm=30000` that floor is **3.333e-05**, and the
rank-1 global-BH critical value over the 1319-row directional family is
`q/m = 0.05/1319 =` **3.791e-05**. The floor sits *below* the bar, so a single
genuinely-separating state was arithmetically able to certify. It did not.

This run is therefore a real negative at the tested resolution, and not the
"could never have passed anyway" artifact a lower `n_perm` would have produced —
at the previous default of 500, the floor (1.996e-03) sat ~53× **above** the
rank-1 bar, so no single row could have certified at any effect size.

### The two target cells

| Cell | Verdict | best p | BH bar it needed | Best row | Sign |
|---|---|---|---|---|---|
| **(ETH, 1h, composite)** | `fails_global_bh` | 0.002167 | 1.137e-04 (rank 3/1319) | `trending_down_clean` @ `2025H1`, h1, gap **+0.00923** | wrong-signed |
| **(HYPE, 1h, composite)** | `fails_global_bh` | 0.072631 | 5.004e-03 (rank 132/1319) | `trending_up_clean` @ `oos`, h48, gap **−0.06573** | wrong-signed |

ETH's best row misses its bar by ~19×; HYPE's by ~15×. Both are additionally
**wrong-signed** — their separation points *against* the side the policy would
bet — so even a relaxed significance bar would not produce a profitable
certification for either.

The closest cell in the whole screen was `(HYPE, 4h, adx)` at p=7.000e-04 against
a rank-2 bar of 7.582e-05 — still ~9× short, and also wrong-signed.

### HYPE window coverage — a hard data-availability limit

Hyperliquid's candle endpoint serves a fixed **5000-candle retention window per
interval, anchored at *now***; `since` is ignored for anything older. Verified at
fetch time (repeated requests with `since` at 2023-01-01, 2024-01-01, 2025-01-01
and 2025-06-10 all returned the same 1h start). The consequence:

| HYPE window | 1h | 4h |
|---|---|---|
| `2023` | empty | empty |
| `2024` | empty | 113 composite bars (Dec only) |
| `2025H1` | empty | 1039 |
| `is` | **empty** | 1183 |
| `oos` | 4955 (from 2026-01-26) | 1353 |

So **(HYPE, 1h, composite) rests entirely on `oos`**, and its `oos` starts
2026-01-26 rather than the window's nominal 2026-01-01. `oos` is a held-out
forward window, so the cell was still eligible to certify; it simply is not close.
Per the issue boundary, no other venue's HYPE series was substituted.

`_load` clips every loaded frame to its eval window before labeling. Without that
clip, HYPE's empty windows would have ingested the fetch fallback's post-listing
history under the earlier window's label — a silent statistical corruption, and
the one place this run could have gone quietly wrong.

### Consequence for the deployed strategies

`certified` stays `[]`, so `regime_directional_policy` resolves to base direction
on `hl-mr-hype-60`, `hl-vwap-eth-60` and `hl-rmc-eth-live`. The #1085 `[WARN]`
those three print at config load is the **expected, verified** outcome, not a
defect — the gate is doing exactly what it exists to do.

The configured policy blocks are left in place (the `keep-with-comment` option in
the issue's Approach 4) rather than removed. Removing them buys tidier config but
loses the recorded intent, and re-adding a block is blocked by SIGHUP while a
position is open — so the safe moment to remove one is from flat, at the
operator's choosing. That deployment config lives out of tree
(`/var/lib/go-trader/config.json`); this repo cannot and should not edit it.

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

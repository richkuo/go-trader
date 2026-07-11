# #1280 — edge verdicts for the five verdict-less registered strategies

Date: 2026-07-10
Base: `origin/main` / `8ce3675e`
Fee model: hyperliquid base tier (0.045% taker/side + 5 bps slippage) — the post-#1315 audit model.

Closes the M5 coverage gap: `anchored_vwap_channel`, `anchored_vwap_reversion`,
`atr_band_revert`, `delta_neutral_funding`, and `bear_pullback_st` had no
recorded backtest edge verdict (absent from `fee-audit-m5.md` and the #1315
re-screen; `bear_pullback_st`/`delta_neutral_funding` carried only
`unscreened_short`/`no_trades` placeholders that reflected harness shape, not
strategy behavior).

## Commands

```bash
# M5 long legs (both registries)
uv run --no-sync python backtest/fee_audit.py --registry both \
  --strategies anchored_vwap_channel,anchored_vwap_reversion,atr_band_revert

# M5 short legs (futures)
uv run --no-sync python backtest/fee_audit.py --registry futures --direction short \
  --strategies anchored_vwap_channel,anchored_vwap_reversion,atr_band_revert,bear_pullback_st

# Noise gate on every net-positive short leg
uv run --no-sync python backtest/gross_edge_noise.py --strategy <name> \
  --registry futures --direction short

# M1 held-out confirmation (is/oos/2023/2024/2025H1)
uv run --no-sync python backtest/eval_windows.py --strategy <name> \
  --registry futures --direction short

# atr_band_revert designed pairing (ranging gate), long and short
uv run --no-sync python backtest/eval_windows.py --strategy atr_band_revert \
  --registry futures [--direction short] --allowed-regimes ranging

# delta_neutral_funding — funding-aware path (eval_windows attaches the per-bar
# funding column and the Backtester books the carry, #988). NOT an M5 re-run:
# fee_audit.py has no funding attach, so M5 re-records no_trades forever.
uv run --no-sync python backtest/eval_windows.py --strategy delta_neutral_funding \
  --registry futures --direction short
```

Datasets/windows: the six audit datasets (BTC/ETH/SOL × 1h/4h), protocol
windows is (2025-06-10 → 2026-01-01) and oos (2026-01-01 → 2026-07-10); M1 adds
held-out 2023 / 2024 / 2025H1.

## M5 screen results

Long legs (`--registry both`; the anchored variants' long legs were scored on
the spot registry rows):

| strategy | reg | trades | gross %/leg | net %/leg | net Sharpe | M5 label |
|----------|-----|-------:|------------:|----------:|-----------:|----------|
| atr_band_revert | futures | 509 | -12.37 | -18.84 | -0.68 | `unscreened_short` |
| anchored_vwap_reversion | spot | 202 | -13.29 | -15.84 | -0.69 | `unscreened_short` |
| anchored_vwap_channel | spot | 25 | -1.30 | -1.61 | -0.10 | `unscreened_short` |
| atr_band_revert | spot | 12 | -23.37 | -23.44 | -0.72 | `deprecate` |

Short legs (`--registry futures --direction short`; anchored_vwap_channel row
is the clean single re-run — the batch run had one locked-cache errored leg):

| strategy | trades | gross %/leg | net %/leg | net Sharpe | M5 label | noise gate (perm p, trade level) |
|----------|-------:|------------:|----------:|-----------:|----------|---------------------------------:|
| bear_pullback_st | 12 | 13.58 | 13.49 | -6.92 | `healthy` (1/12 legs liquidated) | **0.007** (positive) |
| anchored_vwap_channel | 20 | 5.26 | 4.99 | 0.90 | `healthy` | 0.243 (indistinguishable) |
| anchored_vwap_reversion | 203 | 5.53 | 2.48 | 0.28 | `healthy` | 0.187 (indistinguishable) |
| atr_band_revert | 514 | 8.58 | 0.55 | 0.28 | `healthy` | 0.113 (indistinguishable) |

Leg-level noise verdicts are `indistinguishable_from_zero` for all four
(bear_pullback_st's trade-level positive comes from 12 trades concentrated in
the 2025–26 bear/flat protocol windows).

## M1 held-out confirmation (short legs, mean Sharpe vs incumbent bar)

| strategy | is | oos | 2023 | 2024 | 2025H1 |
|----------|----|-----|------|------|--------|
| bear_pullback_st | fail (1 liq) | pass | fail (3 liq) | fail (4 liq) | pass |
| anchored_vwap_channel | pass | pass | fail (1 liq) | fail (1 liq) | pass |
| anchored_vwap_reversion | pass | pass | fail (1 liq) | fail | pass |
| atr_band_revert + `allowed_regimes=ranging` (short) | fail | pass | fail (1 liq) | fail | pass |
| atr_band_revert + `allowed_regimes=ranging` (long) | fail | fail | fail | fail | pass |

The shared pattern: every short leg's positive edge lives in the 2025–26
bear/flat protocol windows and dies — with liquidated legs — in the 2023/2024
bull windows. The M1 harness holds naked all-in shorts without a stop; live
deployments carry the default SL (`DefaultStopLossATRMult=1.0`) and, for the
three ranging faders, the init.go pre-gates
(`atr_band_revert`/`anchored_vwap_channel`/`anchored_vwap_reversion` →
`ranging_quiet`,`ranging_volatile`), so the liquidation tails overstate live
risk — but the regime-conditionality itself is real: the ranging gate did NOT
rescue atr_band_revert's held-out windows.

## delta_neutral_funding (funding-aware run)

The eval_windows funding-aware short path (per-bar `funding_rate` column +
#988 carry booking) fires almost never at the default
`entry_threshold=0.0001`/hr (≈88% APY): zero trades in is/oos/2025H1, 4
datasets traded in 2023 (ETH +10.5/+11.4%, SOL −28.2/−36.3%), 2 in 2024 (BTC
+6.6/−0.2%). Where it fires, the naked short's price PnL dwarfs the funding
carry — the SOL 2023 legs lose ~30% to price while collecting single-digit
carry. That is exactly the component the live strategy's spot hedge is
designed to offset, and **no current harness models the hedged pair** (the
Backtester holds one leg; the delta-offset spot leg and its rebalancing are
unmodeled).

**Decision: live-only, verdict withheld — not deprecated, not promoted.** The
naked-short backtest is not representative of the delta-neutral structure, in
either direction (its losses are hedged away live; its price gains are equally
not the strategy's edge). A representative harness needs: (a) two synchronized
legs (perp short + spot long) with per-leg fees, (b) funding accrual on the
perp leg only, (c) drift-triggered rebalancing (`drift_threshold`), (d) a
margin model for the short leg. That is a new engine capability
(carry-pair simulation), not a parameter of the existing long/flat or
short/flat paths. Until someone builds it, `delta_neutral_funding` stays
registered and live-eligible with this documented rationale; its M5 row stays
`no_trades` with a pointer here.

## Verdicts

| strategy | verdict | disposition |
|----------|---------|-------------|
| `anchored_vwap_channel` | `healthy` (short leg: net +4.99%/leg, Sharpe 0.90; noise-unconfirmed at n=20; long leg gross ≤ 0) | keep registered; short-side, ranging-gated deployments only; no promotion until the short edge survives a larger sample |
| `anchored_vwap_reversion` | `healthy` (short leg: net +2.48%/leg over 203 trades; noise-unconfirmed p=0.19; long leg gross −13.29%) | keep registered; same conditions |
| `atr_band_revert` | spot long leg `deprecate` (gross −23.37%); futures short leg `healthy` but marginal (net +0.55%/leg, noise p=0.11) and the designed ranging gate fails 3/5 M1 windows | keep registered on futures (short); **do not deploy the spot long-only variant**; weakest of the three faders — first candidate for quarantine if a future re-screen stays flat |
| `delta_neutral_funding` | verdict withheld — structure unmodelable in current harnesses (see above) | live-only with documented rationale; carry-pair harness spec'd above if anyone wants the verdict |
| `bear_pullback_st` | `healthy` (short leg: net +13.49%/leg, 12 trades, trade-level noise-positive p=0.007) — replaces `unscreened_short`; explicitly bear-regime-conditional (2023/2024 M1 windows liquidate 3–4/6 legs without stops) | keep registered; short-only bear-market tool; must deploy with the default SL and never ungated in trending-up regimes |

No strategy earned a whole-strategy `deprecate` (every measured short leg has
positive gross), so no `M5_DEPRECATED_EDGE_STRATEGIES` / discovery-hidden
roster change ships with this study. No harness was added or repurposed
(`docs/backtesting-registry.md` unchanged).

---
Created with LLM: Fable 5 | medium | Harness: Claude Code

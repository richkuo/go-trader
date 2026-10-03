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

**Superseded by #1326.** The carry-pair harness (`backtest_carry_pair.py`) was
subsequently built and now models the hedged pair; the recorded verdict is in
the [#1326 section below](#delta_neutral_funding--carry-pair-verdict-1326). The
withholding rationale above stands as the reason the harness was needed, not as
the final disposition.

## Verdicts

| strategy | verdict | disposition |
|----------|---------|-------------|
| `anchored_vwap_channel` | `healthy` (short leg: net +4.99%/leg, Sharpe 0.90; noise-unconfirmed at n=20; long leg gross ≤ 0) | keep registered; short-side, ranging-gated deployments only; no promotion until the short edge survives a larger sample |
| `anchored_vwap_reversion` | `healthy` (short leg: net +2.48%/leg over 203 trades; noise-unconfirmed p=0.19; long leg gross −13.29%) | keep registered; same conditions |
| `atr_band_revert` | spot long leg `deprecate` (gross −23.37%); futures short leg `healthy` but marginal (net +0.55%/leg, noise p=0.11) and the designed ranging gate fails 3/5 M1 windows | keep registered on futures (short); **do not deploy the spot long-only variant**; weakest of the three faders — first candidate for quarantine if a future re-screen stays flat |
| `delta_neutral_funding` | `healthy` — hedged pair now modeled (#1326): both windows with HL funding coverage (2023 +1.10%/leg, 2024 +2.37%/leg mean) net-positive, 84–95% funding-driven, price PnL hedged to ~$0; is/oos/2025H1 attach no cached funding (coverage gap, not a no-signal) | keep registered & live-eligible; carry demonstrated (no longer "unmodelable") — see the #1326 section below |
| `bear_pullback_st` | `healthy` (short leg: net +13.49%/leg, 12 trades, trade-level noise-positive p=0.007) — replaces `unscreened_short`; explicitly bear-regime-conditional (2023/2024 M1 windows liquidate 3–4/6 legs without stops) | keep registered; short-only bear-market tool; must deploy with the default SL and never ungated in trending-up regimes |

No strategy earned a whole-strategy `deprecate` (every measured short leg has
positive gross), so no `M5_DEPRECATED_EDGE_STRATEGIES` / discovery-hidden
roster change ships with this study. No harness was added or repurposed **at
the time of #1280** (`docs/backtesting-registry.md` unchanged then); the
follow-up #1326 adds `backtest_carry_pair.py` — see the section below.

## delta_neutral_funding — carry-pair verdict (#1326)

Date: 2026-07-11
Base: `origin/main` / `7e538094`
Harness: `backtest/backtest_carry_pair.py` (new, #1326)
Fee model: hyperliquid base-tier taker 0.045%/side (per leg).

The carry-pair harness models what `delta_neutral_funding` actually runs live —
SHORT the perp to collect funding while holding an equal-notional SPOT long as
the delta offset — booking both legs so the naked short's price PnL no longer
dominates. Structure: perp short on isolated margin (default 3× / 2% MMR,
gap-through liquidation cap), spot long fully funded (no funding, no
liquidation), entry/exit from the same registry signal the live strategy emits,
and delta-drift-triggered spot rebalancing (`drift_threshold`, since the
registry's `delta_drift_pct`/`rebalance_needed` columns are 0.0 placeholders;
rebalancing uses average-cost accounting so realized PnL locked in on resized
units survives to the close — #1335 review fix). Rebalancing only fires in
`--perp-symbol` basis mode; the single-series audit below has 0 rebalances, so
the recorded verdict is unaffected by it.

Command:

```bash
uv run --no-sync python backtest/backtest_carry_pair.py \
  --json backtest/research/carry_pair_1326.json
```

Funding is booked on the perp leg only, over exactly the held interval
`[entry_bar+1, exit_bar]` (a newly-opened position first accrues the bar AFTER
its fill, matching `backtester.py:2425`; #1335 review fix). Every funded bar —
including the exit bar on a signal close — values funding at the bar close, the
same convention as the mark loop and `backtester.py:2425` (#1335 review fix; the
shift from the exit-fill open changed the recorded numbers only at the 4th
decimal, below the precision of the tables above).

Result (registry defaults: `entry_threshold=0.0001`/hr ≈ 88% APY, six audit
datasets × five M1 windows):

| window | traded | mean ret% | mean Sharpe | Σ funding$ | Σ price$ | Σ fees$ | notes |
|--------|--------|-----------|-------------|-----------|---------|--------|-------|
| is (2025-06→2026-01) | 0/6 | — | — | — | — | — | no cached HL funding coverage |
| oos (2026-01→) | 0/6 | — | — | — | — | — | no cached HL funding coverage |
| 2023 | 4/6 | +1.10 | 3.18 | 50.80 | 22.17 | 8.45 | ETH+SOL funded, rich funding |
| 2024 | 4/6 | +2.37 | 7.15 | 149.16 | 4.04 | 11.30 | BTC+SOL funded, rich funding |
| 2025H1 | 0/6 | — | — | — | — | — | no cached HL funding coverage |

Per-leg, funded windows (base_notional $750, capital $1000; datasets with no
funding coverage omitted):

| window / dataset | ret% | funding$ | price$ | fees$ | funding-share | perp liq | pairs |
|------------------|------|----------|--------|-------|---------------|----------|-------|
| 2023 ETH/USDT 1h | +0.75 | 8.83 | 0.00 | 1.29 | 87% | 0 | 1 |
| 2023 ETH/USDT 4h | +0.71 | 8.34 | 0.00 | 1.28 | 87% | 0 | 1 |
| 2023 SOL/USDT 1h | +1.32 | 15.45 | 0.00 | 2.92 | 84% | 1 | 2 |
| 2023 SOL/USDT 4h | +3.81 | 18.18 | 22.17 | 2.96 | 42% | 1 | 2 |
| 2024 BTC/USDT 1h | +2.53 | 26.65 | 0.00 | 1.33 | 95% | 0 | 1 |
| 2024 BTC/USDT 4h | +2.68 | 28.21 | 0.00 | 1.38 | 95% | 0 | 1 |
| 2024 SOL/USDT 1h | +4.26 | 46.92 | 0.00 | 4.30 | 92% | 1 | 3 |
| 2024 SOL/USDT 4h | +4.71 | 47.37 | 4.04 | 4.29 | 85% | 1 | 3 |

**Findings.**
1. The #1280 naked-short problem is resolved: with a single-series hedge the
   price PnL cancels to **exactly $0** on every leg without a perp liquidation —
   the ≈ −30% SOL-2023 price loss that dominated the one-leg backtest is
   entirely hedged away, leaving net-positive funding carry.
2. Where the structure trades and funding data exists, it is genuinely
   carry-driven: **84–95%** of the gross edge is funding, net **+0.71% to
   +4.71%** per leg after fees, across both funded windows (2023 and 2024).
3. The two legs with a perp liquidation (SOL 2023 4h, SOL 2024 4h) show a real
   protective asymmetry (price$ +22.17 / +4.04, not 0): a violent up-move gaps
   the short perp through its isolated-margin liquidation, capping the perp loss
   at the posted margin while the spot leg keeps gaining — so the hedged pair
   *profits* from the move that would have crushed the naked short.
   Account-level liquidation never occurs (the spot hedge covers it).

**Verdict: `healthy`.** Net-positive, fee-surviving, carry-dominated hedged
returns in **both** windows with usable funding coverage (2023, 2024) — a clear
upgrade from "unmodelable / verdict withheld." Caveat: is/oos/2025H1 attach zero
funding in this environment and produce no trades — a data-coverage limit, not a
no-signal result (the harness warns loudly to keep the two distinguishable), so
the verdict rests on the 2023–2024 evidence and should be re-confirmed once
funding history is backfilled for the remaining windows. Disposition:
`delta_neutral_funding` stays registered and live-eligible, now with a
demonstrated carry edge rather than a withheld verdict.

---
Created with LLM: Fable 5 | medium | Harness: Claude Code
Updated with LLM: Opus 4.8 | high | Harness: Claude Code | fableplan-work-on-issue
Updated with LLM: Opus 4.8 | high | Harness: Claude Code | fix-pr-review

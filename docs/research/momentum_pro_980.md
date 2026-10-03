# momentum_pro short-leg measurement + volatility-targeted sizing (#980)

Generated: 2026-07-01

Full M1 protocol (#995) on `momentum_pro`, the #956 audit's best OOS-checked
strategy. Two mechanisms per M1 step 4, each isolated and combined:

1. **Short-leg measurement** — `momentum_pro_core` already emits `signal=-1`
   shorts unconditionally; the audit's compare runs measured the long/flat
   path only (on that path a bearish `-1` acts as the long *exit*). The short
   leg is measured here via the #989 `--direction short` harness. No strategy
   code change.
2. **ATR-normalized (volatility-targeted) entry sizing** — new unregistered
   kwargs on `momentum_pro_core` (`vol_target_atr_pct`, default **0.0 = OFF**;
   `vol_target_atr_period=14`; `vol_target_min_fraction=0.10`) emit an
   `entry_fraction` column, consumed by a new Backtester surface (#980,
   `close_fraction`-precedent column, shift(1) with the signal) that scales
   the notional committed at open: `clip(target / (ATR/close), 0.10, 1.0)`.
   Signals and trade counts are never changed — exposure only. Not a
   registered default param, so `--list-json` stays byte-identical (verified);
   live order sizing is config-driven and ignores the column (backtest-research
   surface only).

## Reproduce

Data: binanceus OHLCV cache populated 2026-07-01 (fetched since 2023-01-01;
`oos` end = latest cached bar 2026-07-01). All runs `--registry spot`
(momentum_pro is byte-identical in both registries).

```bash
# Baseline long leg (M1 steps 1-2)
uv run --no-sync python backtest/eval_windows.py --strategy momentum_pro --registry spot --json /tmp/mp980/base_long.json

# Short leg (mechanism 1)
uv run --no-sync python backtest/eval_windows.py --strategy momentum_pro --registry spot --direction short --json /tmp/mp980/short.json

# Sizing sweeps (mechanism 2; tuned on IS + held-outs per M1 step 5, never OOS)
uv run --no-sync python backtest/eval_windows.py --strategy momentum_pro --registry spot --sweep vol_target_atr_pct=0.005,0.0075,0.01,0.015,0.02,0.03 --sweep-window is
uv run --no-sync python backtest/eval_windows.py --strategy momentum_pro --registry spot --sweep vol_target_atr_pct=0.005,0.0075,0.01,0.015,0.02,0.03 --sweep-window 2023
uv run --no-sync python backtest/eval_windows.py --strategy momentum_pro --registry spot --sweep vol_target_atr_pct=0.005,0.0075,0.01,0.015,0.02,0.03 --sweep-window 2024
uv run --no-sync python backtest/eval_windows.py --strategy momentum_pro --registry spot --sweep vol_target_atr_pct=0.005,0.0075,0.01,0.015,0.02,0.03 --sweep-window 2025H1

# Final protocol measurement of the chosen sizing config (one OOS look) + combo
uv run --no-sync python backtest/eval_windows.py --strategy momentum_pro --registry spot --params '{"vol_target_atr_pct": 0.015}' --json /tmp/mp980/final_sized_long.json
uv run --no-sync python backtest/eval_windows.py --strategy momentum_pro --registry spot --direction short --params '{"vol_target_atr_pct": 0.015}' --json /tmp/mp980/final_sized_short.json
```

Caveat (same class as #1021's): a run against a **cold or partially-populated
cache is not reproducible** — `load_cached_data` only falls back to fetching
when the requested range is completely empty, so a partial cache silently
truncates a window. Populate the full 2023-01-01→now range first (any
all-windows run does it); with a warm cache, back-to-back runs reproduce to
the digit (verified across three invocations).

## Baseline reproduction (M1 step 1)

The audit snapshot (`scheduler/ui_reports.go`, generated 2026-06-10) is not
digit-reproducible on the current cache: its OOS row (+27.3pts vs B&H, 5
trades) was **BTC/USDT 1h only** and ended 2026-06-10, while the harness OOS
now runs to 2026-07-01. The current-cache BTC/USDT 1h OOS leg is consistent
with it: -6.87% vs B&H -30.77% = **+23.9pts on 6 trades**, and it still beats
the incumbent bar on both Sharpe and DDadj.

The 6-dataset harness baseline (registry defaults, long leg):

| window | mean Sharpe | bar | mean DDadj | bar | verdict |
|---|---:|---:|---:|---:|---|
| is | 0.02 | -0.12 | 0.17 | -0.14 | PASS |
| oos | -1.34 | -0.63 | -0.70 | -0.46 | **FAIL** |
| 2023 | 1.88 | 1.46 | 6.95 | 3.67 | PASS |
| 2024 | 0.78 | 0.90 | 0.95 | 1.07 | FAIL |
| 2025H1 | -0.34 | -0.42 | -0.06 | -0.37 | PASS |

Diagnosis: the audit's "best OOS" edge is **BTC-concentrated**. On the
current OOS window BTC 1h still clears the bar decisively (SD), but all four
ETH/SOL legs and BTC 4h fail it (1/6), dragging the mean below the bar. The
long leg remains selective (2-9 trades per leg) — the failure is where the
edge lives (BTC), not churn.

## Mechanism 1 — short leg

| config | IS | OOS | 2023 | 2024 | 2025H1 | held-out |
|---|---|---|---|---|---|---|
| long (baseline) | PASS | FAIL | PASS | FAIL | PASS | 2/3 |
| short | PASS | **PASS** | FAIL | FAIL | PASS | 1/3 |

Protocol OOS for the short leg is emphatic: **6/6 datasets beat the bar on
both Sharpe and DDadj** (mean Sharpe +1.52 vs bar -0.63; every leg posts a
positive return — BTC 4h +38.2%, ETH 4h +44.2%, SOL 4h +37.8% — against
B&H -30..-45%). The IS window also passes (0.32 vs -0.12).

The held-outs expose the regime dependency: **0/6 legs beat the bar in both
2023 and 2024** (bull years; e.g. 2024 mean return -37.5% across legs while
B&H is +46..+122%). Same failure shape as `session_breakout`'s short leg
(#1031) and `vol_momentum`'s (#1021): a real bear-window short edge that
cannot survive an uptrend.

**Verdict: negative result for an unconditional standalone short config —
documented, not deleted.** The stacked-bearish-EMA + ADX gate is not
sufficient regime protection across bull years. Live behavior is unchanged
(momentum_pro stays in `bidirectionalPerpsStrategies`; live perps positions
get the SL/TP protection stack the plain harness deliberately omits).
Follow-on direction if pursued: regime-gate the short side (the
`regime_directional_policy` machinery is the existing surface for exactly
this), which is out of scope here.

## Mechanism 2 — volatility-targeted sizing

Sweep of `vol_target_atr_pct` on the tuning windows (IS + held-outs; mean
Sharpe / mean DDadj vs the baseline row):

| config | is | 2023 | 2024 | 2025H1 |
|---|---|---|---|---|
| baseline (off) | 0.02 / 0.17 | 1.88 / 6.95 | 0.78 / 0.95 | -0.34 / -0.06 |
| 0.005 | -0.02 / 0.12 | 2.10 / 5.77 | 0.78 / 0.95 | -0.47 / -0.12 |
| 0.0075 | -0.01 / 0.15 | 2.09 / 6.52 | 0.77 / 0.93 | -0.39 / -0.09 |
| 0.01 | -0.02 / 0.14 | 2.07 / 6.87 | 0.80 / 0.98 | -0.30 / -0.05 |
| **0.015** | 0.01 / 0.15 | 2.01 / 7.35 | 0.77 / 0.95 | **-0.24 / -0.03** |
| 0.02 | 0.02 / 0.17 | 1.95 / 7.26 | 0.75 / 0.93 | -0.25 / -0.03 |
| 0.03 | 0.02 / 0.17 | 1.91 / 7.18 | 0.78 / 0.95 | -0.29 / -0.04 |

Chosen: **0.015** — the center of a broad plateau (0.01-0.02 all move the
same windows the same way; M1 step 6 satisfied, no single-param spike).
Mechanically it binds mostly on the 4h / high-vol legs (1h ATR/close rarely
exceeds 1.5%): e.g. 2025H1 SOL 4h Sharpe -1.23 → -0.55 with maxDD -39.1% →
-24.7%; 2023 DDadj 6.95 → 7.35. Trade counts are identical everywhere by
construction.

Final protocol measurement (one OOS look, chosen config):

| config | IS | OOS | 2023 | 2024 | 2025H1 | held-out |
|---|---|---|---|---|---|---|
| long + sizing 0.015 | PASS | FAIL | PASS | FAIL | PASS | 2/3 |
| short + sizing 0.015 | PASS | PASS | FAIL | FAIL | PASS | 1/3 |

**Verdict: mild, consistent risk-adjusted improvement — but it flips no
window verdict on either leg.** OOS is nearly untouched (the 1h legs, where
the OOS failure lives, rarely bind the clip). Per M1 selection discipline
that is not promotion evidence: the knob stays **default-off and
unregistered** (`--list-json` byte-identical), available to future
applications via `--params`.

## Verdict table

| mechanism | protocol OOS | held-out | disposition |
|---|---|---|---|
| long baseline | FAIL (BTC-concentrated edge) | 2/3 | remains the shipped default |
| short leg (unconditional) | PASS 6/6 | 1/3 (fails both bull years) | negative result — documented, not shipped standalone; regime-gating is the follow-on surface |
| sizing 0.015 (long) | FAIL (no flip) | 2/3 | keep default-off, unregistered |
| sizing 0.015 (short combo) | PASS (short's own edge) | 1/3 | no interaction — sizing neither rescues nor harms the short leg |

No registry defaults change; `--list-json` verified byte-identical against
`main` for both registries. The engine gains the `entry_fraction` surface
(+ tests) and momentum_pro gains the default-off sizing kwargs (+ tests);
both are inert unless explicitly enabled.

---
Created with LLM: Fable 5 | high | Harness: Claude Code

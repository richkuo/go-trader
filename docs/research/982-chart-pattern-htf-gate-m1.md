# chart_pattern HTF trend gate M1 application (#982)

Part of the M1 program (#977/#978); audit source #956/#975. The audit's
highest-conviction CONFIRM candidate was multi-timeframe confluence — "an HTF
trend gate over an LTF entry attacks exactly" the registry-wide failure mode of
high-frequency long entries fighting the higher-timeframe trend. This
application adds that gate over `chart_pattern`'s existing pattern entries as
**default-off knobs** and validates it on the full M1 protocol.

## Mechanism (implemented, default-off)

`shared_strategies/open/chart_patterns.py` — four new `chart_pattern` params
(registry defaults keep the gate off; `--list-json` byte-identical, both
registries verified):

- `htf_gate_factor` (default **0** = off; >1 enables): native bars per HTF
  bucket, resampled in-frame via the `mtf_confluence` (#963)
  `_resample_htf`/`_project_to_native` machinery — no extra data fetch, and the
  same anti-look-ahead contract (an HTF bucket is only readable from the native
  bar at which it has fully closed).
- `htf_gate_mode` (default `"veto"`): `veto` blocks only counter-trend signals
  (+1 in an HTF downtrend, -1 in an HTF uptrend; neutral/warmup passes) — the
  live DSL `htf_filter` semantics. `align` requires agreement (neutral blocks).
- `htf_gate_ema_fast` / `htf_gate_ema_slow` (defaults 20/40): EMAs over HTF
  bucket closes; trend is the fast/slow relation, neutral until `ema_slow`
  buckets have accrued.

The gate filters signals **after** volume confirmation, so it sees exactly what
the strategy would otherwise emit. `htf_gate_factor<=1` is bit-identical to the
pre-#982 strategy (regression-tested).

Why not reuse the DSL-level `HTFFilter` (`config.go` `htf_filter`,
`shared_tools/htf_filter.py`): it is a binary flag with a fixed default-HTF map
and a fixed 50-EMA — no sweepable surface, so an M1 plateau check (step 6) is
impossible against it, and the M1 bar (`eval_windows.run_leg`) does not apply
it. The strategy-param form follows the #976 (`regime_adaptive_htf`) reference
pattern, backtests through `apply_strategy` on every harness unchanged, and
remains available to live configs via `params`.

## Baseline reproduction (M1 step 1)

Audit row (#956, in `scheduler/ui_reports.go`): in-sample mean Sharpe -0.27,
mean return -11.5%, 185 trades; OOS +24.0pt edge vs B&H on 23 trades, verdict
"yes". Reproduced on the M1 harness 2026-07-01 (data has accrued ~3 weeks since
the audit): BTC/USDT 1h OOS leg -2.20% vs B&H -26.77% (+24.6pt edge, 21
trades) — consistent. Full baseline (registry defaults, 8-incumbent median bar
recomputed per window × dataset, 6 audit datasets):

| Window | mean Sharpe / bar | mean DDadj / bar | verdict |
|--------|-------------------|------------------|---------|
| IS (2025-06-10→2026-01-01) | -0.42 / -0.12 | -0.12 / -0.14 | FAIL |
| OOS (2026-01-01→latest)    | -0.61 / -0.75 | -0.37 / -0.49 | PASS |
| 2023                        | +1.33 / +1.46 | +4.85 / +3.67 | FAIL |
| 2024                        | +1.25 / +0.90 | +1.96 / +1.07 | PASS |
| 2025H1                      | -0.53 / -0.42 | -0.29 / -0.37 | FAIL |

Baseline: protocol OOS PASS, held-out 1/3 — the entry edge is real but the
strategy bleeds in chop/bear windows, exactly the audit's diagnosis.

**Trade-count / fee diagnosis (M1 step 3):** chart_pattern is mid-frequency
(24–109 trades per window across the 6 legs; 250 in 2023) — not the M5
fee-churn class; the bleed is directional (counter-trend entries), not fee
drag. The mechanism to test is a trend gate, not selectivity-by-cost.

## Plateau sweeps (M1 step 6) — `htf_gate_factor` 0,4,5,6,8,10,12, veto mode

Mean candidate Sharpe per window (bar in header; PASS = beats bar on Sharpe AND
DDadj means):

| factor | IS (bar -0.12) | OOS (bar -0.75) | 2023 (bar 1.46) | 2024 (bar 0.90) | 2025H1 (bar -0.42) |
|--------|----------------|-----------------|------------------|------------------|---------------------|
| 0      | -0.42 FAIL | -0.61 PASS | 1.33 FAIL | **1.25 PASS** | -0.53 FAIL |
| 4      | **0.15 PASS** | **-0.67 PASS** | **1.67 PASS** | 0.65 FAIL | **0.59 PASS** |
| 5      | 0.23 PASS | -0.87 FAIL | 1.70 PASS | 0.85 FAIL | 0.30 PASS |
| 6      | 0.48 PASS | -1.02 FAIL | 1.81 PASS | 0.74 FAIL | -0.01 PASS |
| 8      | -0.03 PASS | -1.34 FAIL | 1.66 PASS | 0.72 FAIL | -0.04 PASS |
| 10     | -0.08 PASS | -1.09 FAIL | 1.51 PASS | 0.87 FAIL | 0.02 PASS |
| 12     | -0.22 FAIL | -0.97 FAIL | 1.57 FAIL | 1.02 PASS | 0.03 PASS |

- **IS, 2023, 2025H1: broad plateau.** IS and 2023 flip FAIL→PASS at factors
  4–10; 2025H1 flips FAIL→PASS at every gated factor (4–12). Not a spike.
- **EMA axis is also a plateau:** at factor 4 on IS, all 9 combos of
  `ema_fast`∈{15,20,25} × `ema_slow`∈{30,40,50} PASS (Sharpe 0.09–0.31).
- **Mode isolation:** `align` is dominated by `veto` everywhere (e.g. IS f6
  -0.02 vs +0.48; OOS strictly worse, with legs going zero-trade at f≥8) —
  requiring agreement starves entries without adding selectivity value. The
  shipped default mode is `veto`.
- **Selectivity:** the gate cuts trades ~70% (IS 109→29, 2023 250→58,
  2024 191→58 summed across the 6 legs) — blocking counter-trend entries AND
  suppressing bearish closes during HTF uptrends (longer holds in bulls, which
  is where the 2023 DDadj gain comes from).

## Recommended opt-in config and its full M1 table

`{"htf_gate_factor": 4}` (veto, EMAs 20/40) — the lightest gate on the
plateau, and the only one that keeps the protocol-OOS pass:

| Window | baseline verdict | f4 verdict | f4 Sharpe / bar |
|--------|------------------|------------|------------------|
| IS     | FAIL | **PASS** | 0.15 / -0.12 |
| OOS    | PASS | **PASS** | -0.67 / -0.75 |
| 2023   | FAIL | **PASS** | 1.67 / 1.46 |
| 2024   | PASS | FAIL | 0.65 / 0.90 |
| 2025H1 | FAIL | **PASS** | 0.59 / -0.42 |

Windows passed: **4/5 vs baseline 2/5**; protocol (IS+OOS) both PASS (baseline
failed IS); held-out 2/3 vs baseline 1/3.

## Honest caveats

1. **2024 is a real regression** (PASS→FAIL, Sharpe 1.25→0.65). In a steady
   bull with shallow HTF pullbacks, the gate blocks recovering dip-buys while
   the ungated strategy monetizes them. 2023 (also a bull) still improves
   because its H1 bear-continuation phases were where ungated entries bled.
   The mechanism trades bull-year upside for chop/bear protection.
2. **OOS is factor-fragile.** Only f4 keeps the OOS pass; f≥5 fails it. The
   2026 crash tape rewards the mean-reversion dip-buys the gate suppresses.
   The f4 OOS margin over the bar is thin (-0.67 vs -0.75) and its mean is
   slightly below the ungated -0.61.
3. Both caveats point the same way: the gate's value is **regime-conditional**.
   That is M4's (#998 regime-profile allocation) territory — switching
   `htf_gate_factor` 0↔4 on a slow regime signal is the natural follow-on
   experiment, out of scope here.

## Verdict

Mechanism validated per the #982 decision frame: default-off knobs shipped, the
gate clears the #955/#977 bar on the protocol window (IS+OOS PASS at f4,
baseline failed IS), sits on a broad plateau on IS/2023/2025H1 across both the
factor and EMA axes, and the held-out table improves 1/3 → 2/3. **Registry
default stays off** (`htf_gate_factor=0`): the 2024 regression and the OOS
factor-fragility do not support changing live behavior wholesale, and the
issue scoped the deliverable as default-off knobs. Operators wanting the gate
opt in per-config with `{"htf_gate_factor": 4}`.

The audit's optional fold-in (untested-level confluence, from the
volume-profile CUT verdict) was **not implemented**: M1 isolates one mechanism
at a time, and the HTF gate alone already changes the trade universe ~70%;
level-confluence stacking is a separate candidate to run against this new
baseline if pursued.

## Reproduce

```
# Baseline, all 5 windows
uv run --no-sync python backtest/eval_windows.py --strategy chart_pattern

# Recommended opt-in, all 5 windows
uv run --no-sync python backtest/eval_windows.py --strategy chart_pattern \
  --params '{"htf_gate_factor": 4}'

# Factor plateau on any window W ∈ {is,oos,2023,2024,2025H1}
uv run --no-sync python backtest/eval_windows.py --strategy chart_pattern \
  --windows W --sweep htf_gate_factor=0,4,5,6,8,10,12 --sweep-window W

# EMA plateau at f4 (IS)
uv run --no-sync python backtest/eval_windows.py --strategy chart_pattern \
  --params '{"htf_gate_factor":4}' --windows is \
  --sweep htf_gate_ema_fast=15,20,25 --sweep htf_gate_ema_slow=30,40,50 \
  --sweep-window is
```

Runs executed 2026-07-01 on the audit datasets (BTC/ETH/SOL × 1h/4h,
binanceus fee model, 5 bps slippage, long-leg open-as-close harness).

Generated: 2026-07-01

LLM: Fable 5 | high | Harness: Claude Code + live M1 runs

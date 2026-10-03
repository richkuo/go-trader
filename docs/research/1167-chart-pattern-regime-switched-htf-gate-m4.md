# chart_pattern regime-switched HTF gate — M4 profile allocation over the #982 knobs (#1167)

Follow-on named by the #982 verdict (`docs/research/982-chart-pattern-htf-gate-m1.md`,
PR #1163); umbrella #978; sibling of #1165 (breakout regime arms) / #1166
(momentum_pro directional policy). The #982 HTF trend gate is validated but
regime-conditional: static `{"htf_gate_factor": 4}` flips IS/2023/2025H1
FAIL→PASS and holds the protocol-OOS pass, but 2024 regresses PASS→FAIL (the
gate blocks recovering dip-buys in a steady bull) and the OOS pass is
factor-fragile. This experiment tests whether M4 (#998 regime-profile
allocation) can arbitrate: gate ON in ranging/bear labels, OFF in trending-up
labels, switched by a slow ADX classifier.

**Verdict: documented negative.** The IS-selected switch beats the static-f4
parent on IS but fails protocol OOS (worse than BOTH static parents) and does
not recover 2024. The #982 static opt-in (`{"htf_gate_factor": 4}`) stands.
No code changed anywhere — this run rode the shipped #998/`eval_windows`
`--profile-allocation` surface end to end; registry defaults untouched,
`--list-json` unaffected, live behavior unchanged.

## Setup

- **Profiles (both are validated #982 configs of the same strategy):**
  `ungated` = `{"htf_gate_factor": 0}` (audit baseline), `gated` =
  `{"htf_gate_factor": 4}` (veto, EMAs 20/40 — the shipped registry defaults).
  The sweep is over the switch spec only; nothing under `shared_strategies/`
  changes.
- **Harness:** `eval_windows.run_leg` `profile_allocation` (#998): per-profile
  signal columns from `apply_strategy` with the profile's `param_sets` merged
  over base params, plus a `_profile_label` series from the inline
  `window_spec`; the Backtester shifts the label one bar and replays the
  flat-only, `confirm_bars`-hysteresis switch (`_ProfileSwitcher`). Zero
  harness changes.
- **Data vintage:** same OHLCV cache as the #982 runs (2026-07-01); the static
  parents reproduce #982's tables exactly (e.g. baseline OOS BTC/USDT 1h
  -2.20% vs B&H -26.77%, 21 trades), so all three columns of the final table
  are directly comparable.
- **Protocol:** full M1 — 8-incumbent median bar recomputed per (window,
  dataset), 6 audit datasets (BTC/ETH/SOL × 1h/4h), IS-only selection, one
  final protocol-OOS look, held-out 2023/2024/2025H1. #955 pass = candidate
  mean beats the bar on Sharpe AND DDadj.

## IS-only selection (switch-spec sweep)

Label→profile mappings over the legacy ADX 3-label classifier
(`trending_up` / `trending_down` / `ranging`):

- **A (the issue's hypothesis):** `trending_up`→ungated, `trending_down`→gated,
  `ranging`→gated
- **B:** `trending_up`→ungated, `trending_down`→ungated, `ranging`→gated

Stage 1 — mapping × period × adx_threshold (confirm_bars 12, initial gated),
IS window (bar Sharpe -0.116 / DDadj -0.141); static parents on the same run:

| config | mean Sharpe | mean DDadj | verdict |
|--------|-------------|------------|---------|
| static f0 (ungated) | -0.42 | -0.12 | FAIL |
| static f4 (gated)   | 0.15  | 0.22  | PASS |
| A p14 t20 | **0.52** | 0.44 | PASS |
| A p14 t25 | 0.33 | 0.29 | PASS |
| A p28 t20 | 0.39 | 0.32 | PASS |
| A p28 t25 | 0.33 | 0.28 | PASS |
| A p56 t20 | 0.34 | 0.30 | PASS |
| A p56 t25 | 0.34 | 0.30 | PASS |
| B p14 t20 | 0.20 | 0.25 | PASS |
| B p14 t25 | 0.11 | 0.18 | PASS |
| B p28 t20 | 0.04 | 0.08 | PASS |
| B p28 t25 | 0.18 | 0.26 | PASS |
| B p56 t20 | 0.25 | 0.16 | PASS |
| B p56 t25 | 0.26 | 0.25 | PASS |

Mapping A dominates B and beats static f4 at every (period, threshold) — a
broad plateau, not a spike. On IS the switch works exactly as hypothesized:
it recovers ungated entries during trending-up phases (ETH 1h 8→11-12 trades,
Sharpe 0.68→0.75-1.22 vs f4) while keeping f4's chop protection elsewhere.

Stage 2 — confirm_bars {4, 12, 24} × initial_profile {gated, ungated} across
the mapping-A plateau (all 18 PASS):

| period/threshold | c4 | c12 | c24 | c12 init=ungated |
|------------------|-----|-----|-----|------------------|
| p14 t20 | 0.14 | 0.52 | 0.35 | 0.52 |
| p14 t25 | 0.02 | 0.33 | 0.32 | 0.33 |
| p28 t20 | 0.31 | 0.39 | 0.28 | 0.39 |
| p28 t25 | 0.32 | 0.33 | 0.31 | 0.33 |
| p56 t20 | 0.22 | 0.34 | 0.28 | 0.34 |
| p56 t25 | 0.25 | 0.34 | 0.28 | 0.34 |

`confirm_bars` 4 flickers at the fast end (p14 drops to 0.14/0.02 — the #998
"switch slower than the profiles' signals" warning, observed); 12/24 are
stable. `initial_profile` is a no-op at c12 (identical means everywhere).

**Chosen spec (IS-only, plateau center — not the p14 spike whose c4 neighbor
craters):** mapping A, ADX period 28, adx_threshold 20, confirm_bars 12,
initial gated.

## Final table — one protocol-OOS look + held-out (mean Sharpe / bar)

| window | static f0 | static f4 | switched A p28 t20 c12 | bar |
|--------|-----------|-----------|------------------------|-----|
| IS     | -0.42 FAIL | 0.15 PASS | **0.39 PASS** | -0.12 |
| OOS    | -0.61 PASS | -0.67 PASS | **-0.79 FAIL** | -0.75 |
| 2023   | 1.33 FAIL | 1.67 PASS | 1.60 PASS | 1.46 |
| 2024   | 1.25 PASS | 0.65 FAIL | 0.74 FAIL | 0.90 |
| 2025H1 | -0.53 FAIL | 0.59 PASS | 0.51 PASS | -0.42 |

Windows passed: switched **3/5** vs static-f4 4/5 (f0 2/5). Protocol: IS PASS
but **OOS FAIL — worse than both static parents**. 2024 recovers only
partially (0.65→0.74, bar 0.90). The issue's validation bar (keep IS+OOS
PASS, keep 2023/2025H1, recover 2024) is not met.

## Why it fails: the label mix is tape-invariant

ADX-28/t20 label shares across the 6 datasets:

| window | ranging | trending_down | trending_up | tape → which gate state wins |
|--------|---------|---------------|-------------|------------------------------|
| IS   | 55.4% | 28.4% | 16.2% | chop → gated (and the switch's ungated sliver was well-timed) |
| OOS  | 53.8% | 28.2% | 18.0% | crash → **ungated** (mean-reversion dip-buys) |
| 2024 | 56.6% | 23.6% | 19.7% | steady bull → **ungated** (dip-buys compound) |

Three tapes with opposite gate preferences produce **nearly identical label
mixes**: under mapping A the switch sits gated on ~80% of bars in every tape.
The ADX label measures local trendiness at the bar scale, not the tape
character that decides whether the gate helps. Consequences visible in the
legs:

1. **2024:** the switch trades 68 summed entries vs f4's 58 and f0's 191 — the
   ~20% trending-up sliver recovers only a fraction of the bull-year dip-buy
   universe (ETH 1h -0.16→0.29 helps, but the window mean 0.74 stays under
   the 0.90 bar). A steady bull with shallow pullbacks reads "ranging" to a
   28-period ADX most of the time, so the gate stays ON — the exact #982
   caveat the switch was meant to fix, re-inherited by the classifier.
2. **OOS (2026 crash):** `trending_down`→gated keeps the gate ON through the
   crash, blocking the dip-buys that made the ungated parent pass — and the
   sparse flips it does make land badly (BTC 4h 0.05→-0.45, ETH 4h
   -1.05→-1.96 vs f4, each adding one trade). 27 trades vs f4's 24, mean
   -0.79 vs bar -0.75: label lag turns the switch into "f4 plus mistimed
   re-entries" on a crash tape.
3. The two "bear-flavored" tapes in the suite demand **opposite** gate states
   (2025H1 grind-down → gated wins; 2026 crash → ungated wins). Mapping B
   (`trending_down`→ungated) encodes the crash-tape preference but is
   dominated on IS (0.04-0.26 vs A's 0.33-0.52), so IS-only selection can
   never choose it — IS contains no crash tape to reward it.

**Composite second phase not run.** The issue scoped composite windows for
"the coarse labels can't separate the tapes" — which is true at the window
level (the mix table), but the binding constraint is selection, not classifier
resolution: certifying any crash-vs-grind mapping needs a crash tape in the
selection window, and the only crash tape in the suite is protocol OOS itself.
Running composite specs against OOS/held-out windows after this table would be
selection-on-OOS. If a future non-protocol crash window enters the versioned
window set (#977), a composite `trending_down_clean` (crash) vs
`ranging_*`/`_choppy` (grind) split is the natural retry.

## Verdict

Negative, documented per the issue's acceptance criteria: the M4 switch over
the #982 knobs beats static f4 in-sample on a broad plateau but fails the
protocol-OOS look against both parents and does not recover 2024. **The #982
static opt-in stands** — operators wanting the gate use
`{"htf_gate_factor": 4}`; no live `regime_profile_allocation` config is
recommended for chart_pattern. Registry defaults untouched; no code changed.

For #1165's Arm B (same M4 mechanism, breakout): the reusable lessons are
(a) sweep driver shape — batch `eval_windows.evaluate_window` calls with a
shared per-window `bars_memo`, specs as {mapping, period, threshold,
confirm_bars, initial}; (b) check the label-mix table across the target
windows FIRST — if the mix doesn't separate the tapes the profiles are meant
to split, the switch can only ever be "the majority profile plus a sliver".

## Reproduce

```
# Static parents (identical to #982's commands)
uv run --no-sync python backtest/eval_windows.py --strategy chart_pattern
uv run --no-sync python backtest/eval_windows.py --strategy chart_pattern \
  --params '{"htf_gate_factor": 4}'

# Chosen switched composite, all 5 windows (the final-table column)
uv run --no-sync python backtest/eval_windows.py --strategy chart_pattern \
  --profile-allocation '{"window_spec":{"classifier":"adx","period":28,"adx_threshold":20},
    "profiles":{"trending_up":"ungated","trending_down":"gated","ranging":"gated"},
    "param_sets":{"gated":{"htf_gate_factor":4},"ungated":{"htf_gate_factor":0}},
    "confirm_bars":12,"initial_profile":"gated"}'

# Any sweep row: same command with --windows is and the row's
# period/adx_threshold/profiles/confirm_bars/initial_profile substituted.
```

Runs executed 2026-07-01 on the audit datasets (BTC/ETH/SOL × 1h/4h,
binanceus fee model, 5 bps slippage, long-leg open-as-close harness), same
OHLCV cache vintage as the #982 report.

Generated: 2026-07-01

LLM: Fable 5 | high | Harness: Claude Code + live M1 runs

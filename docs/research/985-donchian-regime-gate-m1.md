# donchian_breakout regime-gate M1 application — verdict: deprecate (#985)

Generated: 2026-07-01

Part of the M1 program (#977/#978); audit source #956/#975; M5 screen #999.
This is the #985 improve-or-deprecate application: regime-gate the breakouts
(M1 mechanism, ADX and composite classifiers), complete the short-side
diagnosis the M5 `unscreened_short` verdict required, and evaluate the #977 M4
dual-profile angle. **No mechanism combination clears the incumbent-median bar
on protocol OOS and the held-out windows → donchian_breakout moves to the
deprecation list** (`DISCOVERY_HIDDEN_STRATEGIES`, hidden from discovery, kept
loadable — the #1031/#1021/#987/#1023 pattern).

## Context going in

- Audit (#956): in-sample mean Sharpe -0.42, +31.0pts vs B&H, 355 trades
  ("watch"); OOS -20.5% / Sharpe -1.53 / 43 trades, +9.9pts, "weak".
- M5 (#999, `docs/research/fee-audit-m5.md`): long leg gross -2.01%/leg, net
  -10.53%/leg, 8.52pp fee drag over 354 long trades — `unscreened_short`
  (bidirectional strategy, short half unmeasured by the long/flat harness).
  The report requires both-sides evidence before any deprecate/graduate call —
  provided here via the #989 `--direction short` harness.
- Audit finding no. 2 (#956): "Regime selectivity, not entry style, separated
  winners from losers" — the mechanism under test is a trend gate on entries.

## Harness extension (shipped with this PR)

`eval_windows.py` gains `regime_windows_spec` (candidate JSON key +
`--regime-windows-spec` CLI flag): a windows spec validated by
`parse_regime_windows_spec_json` (the same parser the live config and
`run_backtest.py --config` use) and threaded to the Backtester's #1058
primary-window path, so `--allowed-regimes` can gate entries with the
**composite 9-state classifier** on the M1 incumbent-relative bar instead of
only the legacy single-lookback ADX. A spec alone forces `regime_enabled`
(threading it without computing the regime column would be a silent no-op).
Tests: threading proof (composite vocabulary blocks an ADX-label gate to zero
trades), normalization (bare int → ADX spec; empty dict → no spec), malformed
and reserved-name rejection. All 32 `test_eval_windows.py` tests pass; the
three new ones fail without the extension (red → green verified).

## Reproduce

Data: binanceus OHLCV cache populated 2026-07-01 (same warm cache as the
#980/#982 runs; a cold or partial cache silently truncates windows — populate
2023-01-01→now first). All runs `--registry spot` (donchian_breakout is
byte-identical in both registries); 8-incumbent median bar recomputed per
(window, dataset); 6 audit datasets (BTC/ETH/SOL × 1h/4h).

```bash
# Baselines (M1 steps 1-3; long = shipped default, short = #989 mirror path)
uv run --no-sync python backtest/eval_windows.py --strategy donchian_breakout --registry spot
uv run --no-sync python backtest/eval_windows.py --strategy donchian_breakout --registry spot --direction short

# ADX regime gates (legacy single-lookback, period 14 / threshold 20)
uv run --no-sync python backtest/eval_windows.py --strategy donchian_breakout --registry spot --allowed-regimes trending_up
uv run --no-sync python backtest/eval_windows.py --strategy donchian_breakout --registry spot --direction short --allowed-regimes trending_down

# Composite gates (the new regime_windows_spec threading)
uv run --no-sync python backtest/eval_windows.py --strategy donchian_breakout --registry spot \
  --allowed-regimes trending_up_clean \
  --regime-windows-spec '{"medium": {"classifier": "composite", "period": 14}}'
uv run --no-sync python backtest/eval_windows.py --strategy donchian_breakout --registry spot \
  --allowed-regimes trending_up_clean --allowed-regimes trending_up_choppy \
  --regime-windows-spec '{"medium": {"classifier": "composite", "period": 14}}'
uv run --no-sync python backtest/eval_windows.py --strategy donchian_breakout --registry spot \
  --direction short --allowed-regimes trending_down_clean --allowed-regimes trending_down_choppy \
  --regime-windows-spec '{"medium": {"classifier": "composite", "period": 14}}'

# M4 dual profile (#977 mapping: trend profile vs selective-in-chop profile)
uv run --no-sync python backtest/eval_windows.py --strategy donchian_breakout --registry spot \
  --profile-allocation '{"window_spec": {"classifier": "adx", "period": 14, "adx_threshold": 20},
    "profiles": {"trending_up": "trend", "trending_down": "trend", "ranging": "selective"},
    "param_sets": {"trend": {"entry_period": 20}, "selective": {"entry_period": 55}},
    "confirm_bars": 2, "initial_profile": "trend"}'

# entry_period plateau sweeps (tuning windows only, never OOS; W ∈ {is,2023,2024,2025H1})
uv run --no-sync python backtest/eval_windows.py --strategy donchian_breakout --registry spot \
  --allowed-regimes trending_up --windows W --sweep entry_period=20,30,40,55 --sweep-window W
uv run --no-sync python backtest/eval_windows.py --strategy donchian_breakout --registry spot \
  --direction short --allowed-regimes trending_down --windows W --sweep entry_period=20,30,40,55 --sweep-window W

# One protocol-OOS look for the best tuned long config (M1 step 5 discipline)
uv run --no-sync python backtest/eval_windows.py --strategy donchian_breakout --registry spot \
  --params '{"entry_period": 40}' --allowed-regimes trending_up --windows oos
```

## Baseline reproduction (M1 step 1)

The audit's shape reproduces on the current cache: the long leg is churny
(94-107 trades per bull-year 1h leg; the audit's "355 trades" class) and sits
below the bar everywhere. Mean candidate Sharpe / DDadj vs bar:

| window | long Sharpe / bar | long DDadj / bar | verdict |
|--------|-------------------|------------------|---------|
| IS     | -0.17 / -0.12 | -0.14 / -0.14 | FAIL |
| OOS    | -1.07 / -0.75 | -0.62 / -0.49 | FAIL |
| 2023   | +1.11 / +1.46 | +2.72 / +3.67 | FAIL |
| 2024   | +0.59 / +0.90 | +0.81 / +1.07 | FAIL |
| 2025H1 | -1.03 / -0.42 | -0.52 / -0.37 | FAIL |

Baseline long: **0/5 windows, held-out 0/3.** The audit diagnosis holds — the
1h legs churn through ranges (fee drag 8.52pp per M5) and drag every mean; the
4h legs alone would look mid-table (e.g. 2023 BTC 4h Sharpe 2.31 vs bar 1.81).

## Mechanism 1 — regime gate (ADX and composite)

| candidate | IS | OOS | 2023 | 2024 | 2025H1 | held-out |
|---|---|---|---|---|---|---|
| long baseline | FAIL | FAIL | FAIL | FAIL | FAIL | 0/3 |
| long + ADX `trending_up` | **PASS** | FAIL | FAIL | FAIL | FAIL | 0/3 |
| long + composite `trending_up_clean` | **PASS** | FAIL | FAIL | FAIL | FAIL | 0/3 |
| long + composite up family (clean+choppy) | FAIL | FAIL | FAIL | FAIL | FAIL | 0/3 |
| short baseline | PASS | **PASS** | FAIL | FAIL | FAIL | 0/3 |
| short + ADX `trending_down` | PASS | **PASS** | FAIL | FAIL | **PASS** | 1/3 |
| short + composite down family | PASS | **PASS** | FAIL | FAIL | FAIL | 0/3 |

- **Long leg:** the gate flips IS only (ADX -0.17 → -0.05; composite clean
  +0.15). Protocol OOS gets *worse* under both gates (-1.07 → -1.37 ADX,
  -1.75 composite-clean): in the 2026 crash tape the trending_up labels land
  on dead-cat rallies, so the gate concentrates exposure in the worst entries.
  Held-out stays 0/3 under every gate variant.
- **Short leg:** the edge is real — gated OOS Sharpe 1.62 vs bar -0.75, and
  the ADX bear gate flips 2025H1 (-0.77 → -0.17, PASS). But 2023/2024 are
  structural: a short-only leg cannot beat a long-biased bar in bull years
  (2023 gated short -1.76 vs bar +1.46), the same failure shape as
  session_breakout (#1031), momentum_pro's short (#980), and vol_momentum
  (#1021). The composite down-family gate is *worse* than ADX (2025H1 back to
  FAIL at -0.87) — no classifier substitution fixes a directional problem.

## Mechanism 2 — entry_period plateau (M1 step 6, tuning windows only)

Gated long (`trending_up`), mean Sharpe per window (bar in header):

| entry_period | IS (bar -0.12) | 2023 (bar 1.46) | 2024 (bar 0.90) | 2025H1 (bar -0.42) |
|---|---|---|---|---|
| 20 | -0.05 PASS | 1.27 FAIL | 0.50 FAIL | -1.28 FAIL |
| 30 | 0.27 PASS | 1.46 PASS | 0.90 FAIL | -0.92 FAIL |
| 40 | -0.05 PASS | 1.42 FAIL | 1.33 PASS | -0.81 FAIL |
| 55 | -0.35 FAIL | 1.62 FAIL | 1.31 PASS | -0.45 FAIL |

**The winning period wanders per window** — 30 alone passes 2023, 40/55 alone
pass 2024, nothing passes 2025H1. That is a per-window spike, not a plateau;
M1 step 6 rejects promoting any of them. The single protocol-OOS look for the
best tuned config (period 40 + gate) confirms: Sharpe -0.68 beats the bar
(-0.75) but DDadj -0.53 misses (-0.49) → **FAIL** (the bar requires both).

Gated short (`trending_down`): every period fails both bull years by wide
margins (2023 best -1.44 vs +1.46; 2024 best -0.27 vs +0.90) and every period
passes 2025H1 — the sweep confirms the failure is directional, not
parameter-sensitive.

## Mechanism 3 — M4 dual profile (#977 mapping)

Two-profile allocation on the ADX switch window (trending → entry_period 20,
ranging → 55, confirm_bars 2): **FAIL all five windows** (IS -0.40, OOS -1.36,
2023 +1.10, 2024 +0.47, 2025H1 -1.04), held-out 0/3 — worse than baseline on
IS and identical-to-worse elsewhere. Param switching between two breakout
profiles cannot change the leg's directionality, and donchian has no fade
profile to allocate to; the #977 M4 hypothesis ("trend-style profiles win
trend windows") is not supported for donchian itself.

## Verdict table

| mechanism | protocol OOS | held-out | disposition |
|---|---|---|---|
| long baseline (shipped default) | FAIL | 0/3 | — |
| long + regime gate (ADX / composite, period sweep 20-55) | FAIL (best: p40 misses DDadj) | 0/3 | rejected — no plateau, OOS worsens under gates |
| M4 dual profile (20/55, ADX switch) | FAIL | 0/3 | rejected |
| short leg (ungated / ADX gate / composite gate) | PASS (Sharpe up to 1.62) | best 1/3 (2025H1, ADX gate) | negative result — bull-year bars structurally unbeatable |

Per the #985 decision gate — *"If no mechanism combination clears the
incumbent-median bar on protocol OOS **and** the held-out windows, move
donchian_breakout to the deprecation list — do not keep iterating past the M1
protocol"* — with both sides measured per the M5 requirement:
**deprecate donchian_breakout.**

The short-side bear edge is worth revisiting only under the same conditions
noted for session_breakout (#1031): a bull-regime flat filter that genuinely
zeroes bull-year trading (flat scores degenerate, not pass, on the M1 bar), or
a live surface that only deploys the short config in confirmed bear regimes
(`regime_directional_policy` + #1085 certification is that surface). Out of
scope here.

## Deprecation (implemented — #1031/#1034/#1035 pattern)

`donchian_breakout` is hidden from discovery but kept loadable for existing
configs/backtests:

- `shared_strategies/open/registry.py`: added to `DISCOVERY_HIDDEN_STRATEGIES`
  (spot discovery 34 → 33, futures 41 → 40; all other `--list-json` entries
  verified byte-identical before/after).
- `scheduler/init.go`: removed from `defaultSpotStrategies` /
  `defaultPerpsStrategies` / `defaultFuturesStrategies`; kept in
  `knownShortNames` ("dbo") and `bidirectionalPerpsStrategies` so explicit
  configs still resolve and wire shorts.
- `scheduler/ui_reports.go`: ranking verdict watch → deprecate + a
  Deprecations entry; `ui_reports_test.go` count 17 → 18.
- `README.md`: removed from the spot discovery example list.
- `backtest/optimizer.py` `DEFAULT_PARAM_RANGES` and the incumbent set in
  `eval_windows.py` are intentionally unchanged (donchian_breakout remains an
  M1 incumbent, like range_scalper — the bar definition is versioned and
  deprecation does not alter it).
- Tests: `test_registry_parity.py` hidden-but-loadable (red → green verified);
  full Go suite + full pytest suite pass.

Status: M1 protocol complete (baseline both legs + 7 gate/profile candidates +
8 plateau sweeps + 1 OOS look, all 6 datasets × 5 windows). Verdict: deprecate
— **implemented.**

---
Created with LLM: Fable 5 | high | Harness: Claude Code + live M1 runs

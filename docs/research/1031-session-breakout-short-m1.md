# session_breakout short leg M1 application (#1031)

Follow-up to #992 (short leg gross edge confirmed) and part of the M1 program (#977/#978).

Validated mechanisms per code audit (2026-06-17):
- `session_breakout` is futures-only, bidirectional (emits -1 shorts), registered in `bidirectionalPerpsStrategies`.
- Primary DD control: bear-regime gate via `allowed_regimes` (backtester gates both directions on prior-bar regime label; explicit short-gate test exists).
- `regime_directional_policy` is HL-perps-only (use `allowed_regimes` + `direction=short` analogue for backtest/futures).
- Close quality: `atr_stop` (early_reversal) and `zscore_target` (giveback) are default-off, backtest-wired close evaluators for futures. `time_stop` is available but contraindicated (profit concentrates in 51+ bar holds).
- Backtester threads `allowed_regimes` from `--config` (#1025); rejects CLI flag alongside config and named `regime_gate_window` (only legacy single-lookback ADX modeled in BT).
- Default (no close refs) = open-as-close incumbent. Adding close refs changes trade count/holding structure — must be compared vs incumbent on M1 bar.
- Live note: TopStep futures executor currently close-long-only (`FuturesOrderSkipReason`); short opens are paper + backtest only today. Results here inform paper configs; live short support would be separate work.

## Validation summary (from code + harness review)
- `session_breakout` roster entry, bidirectional short emission, `bidirectionalPerpsStrategies`
  membership, and `atr_stop`/`zscore_target` registration + M1 evaluator path all confirmed.
- Backtester implements short entry gating under `allowed_regimes` + `direction=short` (and
  the legacy single-lookback); explicit test covers short blocking. The gate is now also
  wired into the M1 bar path (`eval_windows` → `run_leg` → `Backtester`) for this research.
- `--list-json` parity, look-ahead invariants, liquidation floor, and close-always-execute
  while gating entries are unchanged.
- Direct `run_backtest.py` supports `--allowed-regimes` (append) + `--close-strategy`
  (append) + `--config` threading (#1025). `eval_windows.py` (M1 bar) uses `--candidate-json`
  (or the added `--allowed-regimes`/`--direction` shorthands) and `close_strategies` inside
  the candidate dict.

Prior blanket claim of "no contradictions on all technical points" was overstated for the
specific command examples that were not executable on the M1 harness; this doc now uses
only runnable forms and explicitly notes the previous gap + its resolution.

See GitHub issue #1031 for full rationale, #992 numbers, and the decision gate.

## Reproduce / run plan

M1 protocol (incumbent-median bar, IS/OOS + held-out, continuous windows for holding changes).

**Note on flags:** `eval_windows.py` (the M1 bar producer) takes complex candidate
shape via `--candidate-json` (or simple pieces via `--direction`, `--allowed-regimes`,
`--profile-allocation`). Direct `--close-strategies` / `--allowed-regimes` JSON-array
style only exist on `run_backtest.py`. `--close-strategy` (singular, repeatable) is
the append form on `run_backtest.py`.

```
# Baseline (open-as-close, long/flat harness as before)
uv run --no-sync python backtest/run_backtest.py --strategy session_breakout \
  --symbol BTC/USDT --timeframe 1h --mode single --registry futures

# Short leg + atr_stop close on the M1 incumbent-relative bar (eval_windows)
cat > /tmp/sbo-short-atr.json <<'J'
{
  "name": "session_breakout",
  "direction": "short",
  "close_strategies": [
    {"name": "atr_stop", "params": {"atr_mult": 2.0, "atr_source": "entry"}}
  ]
}
J
uv run --no-sync python backtest/eval_windows.py --registry futures \
  --candidate-json /tmp/sbo-short-atr.json \
  --windows is,oos,heldout --json /tmp/1031-short-atr.json

# Bear-regime gate (primary DD control) + close on the M1 bar
cat > /tmp/sbo-short-gated.json <<'J'
{
  "name": "session_breakout",
  "direction": "short",
  "close_strategies": [
    {"name": "atr_stop", "params": {"atr_mult": 2.0}}
  ],
  "allowed_regimes": ["trending_down", "ranging"]
}
J
uv run --no-sync python backtest/eval_windows.py --registry futures \
  --candidate-json /tmp/sbo-short-gated.json \
  --windows is,oos,heldout

# Same via CLI pieces (allowed_regimes repeatable; direction on CLI)
uv run --no-sync python backtest/eval_windows.py --strategy session_breakout \
  --registry futures --direction short \
  --allowed-regimes trending_down --allowed-regimes ranging \
  --candidate-json /tmp/sbo-short-atr.json \
  --windows is,oos,heldout

# Add zscore_target for late giveback (avoid time_stop)
cat > /tmp/sbo-short-z.json <<'J'
{
  "name": "session_breakout",
  "direction": "short",
  "close_strategies": [
    {"name": "atr_stop", "params": {"atr_mult": 1.5}},
    {"name": "zscore_target", "params": {"lookback": 20, "z_target": 1.5}}
  ],
  "allowed_regimes": ["trending_down"]
}
J
uv run --no-sync python backtest/eval_windows.py --registry futures \
  --candidate-json /tmp/sbo-short-z.json --windows is,oos,heldout

# Full M1 candidate grid + continuous re-run if holding structure changes
uv run --no-sync python backtest/run_backtest.py --mode optimize --sweep-close ...
```

Also run `exit_diagnostics.py` on the short leg to confirm mode distribution before/after.

**Regime gate on M1 bar:** `allowed_regimes` (with the legacy single-lookback) is now
forwarded through `eval_windows.py` → `run_leg` → `Backtester` so a bear-gated short
candidate can be scored against the incumbent-median bar. When `allowed_regimes` is
present the harness forces `regime_enabled` for that leg. Named `regime_gate_window`
is still unsupported in the backtester (loud reject on `--config` path; M1 uses the
default lookback).

## Decision gate (from #1031)
- Must beat open-as-close incumbent on M1 per-window incumbent-median bar.
- Protocol IS/OOS PASS + held-out table.
- `--list-json` byte-identical (no registry change expected).
- If bear-gated + exit-improved short cannot clear OOS + held-out → deprecate `session_breakout`.

## Notes / caveats from validation
- `direction` in live config is perps/manual only; for futures M1 use the `--direction` flag on the harness (it is supported).
- Regime labels depend on the classifier (adx default vs composite). Use the same as the #992 run for apples-to-apples.
- Any change to holding time distribution requires the continuous-window re-run (#977).

Generated: 2026-06-17 (initial plan + validation)
Status: research branch opened; runs + candidate results to be appended.

LLM: grok-build-0.1 | medium | Harness: code audit + backtester review

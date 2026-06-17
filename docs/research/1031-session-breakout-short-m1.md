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
- Backtester short entry gating with `allowed_regimes` + `direction=short` works as described.
- `atr_stop`/`zscore_target` are first-class in close registry + `exit_diagnostics.py` + M1 path.
- `--list-json` parity, look-ahead, liquidation floor, incumbent-relative bar all unchanged.
- No silent drops for short regime gates in the modeled path.

See GitHub issue #1031 for full rationale, #992 numbers, and the decision gate.

## Reproduce / run plan

M1 protocol (incumbent-median bar, IS/OOS + held-out, continuous windows for holding changes):

```
# Baseline (open-as-close, long/flat harness as before)
uv run --no-sync python backtest/run_backtest.py --strategy session_breakout \
  --symbol BTC/USDT --timeframe 1h --mode single --registry futures

# Short leg with bear gate + atr_stop (example candidate)
uv run --no-sync python backtest/eval_windows.py --strategy session_breakout \
  --registry futures --direction short \
  --close-strategies '[{"name":"atr_stop","params":{"atr_mult":2.0,"atr_source":"entry"}}]' \
  --allowed-regimes '["trending_down","ranging"]' \
  --windows is,oos,heldout --json /tmp/1031-short-atr.json

# Add zscore_target for late giveback (avoid time_stop)
uv run --no-sync python backtest/eval_windows.py --strategy session_breakout \
  --registry futures --direction short \
  --close-strategies '[{"name":"atr_stop","params":{"atr_mult":1.5}},{"name":"zscore_target","params":{"lookback":20,"z_target":1.5}}]' \
  --allowed-regimes '["trending_down"]' \
  --windows is,oos,heldout

# Full M1 candidate grid + continuous re-run if holding structure changes
uv run --no-sync python backtest/run_backtest.py --mode optimize --sweep-close ...
```

Also run `exit_diagnostics.py` on the short leg to confirm mode distribution before/after.

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

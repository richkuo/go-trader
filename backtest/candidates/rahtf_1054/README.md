# regime_adaptive_htf M1 adjudication artifacts (#1054)

Evidence set behind `docs/research/1054-regime-adaptive-htf-m1.md` (the
write-up is the source of truth for the verdict; this directory holds the
runs it cites). The M5 fee audit (#999) graduated `regime_adaptive_htf` as
`graduate_m1` off gross +0.27%/leg vs net -0.66%/leg over 37 trades; the
M1 step-2 noise check (`backtest/gross_edge_noise.py`, added by #1054) found
that gross edge statistically indistinguishable from zero on the screen's own
slices and **negative** pooled across all five protocol/held-out windows, so
per the issue's pre-registered rule the verdict is a documented deprecate
recommendation — no selectivity mechanism was swept (searching for one AFTER
a noise verdict is the multiple-comparisons trap the protocol exists to
block).

## Artifacts

- `fee_repro.json` — M5 row reproduction (`fee_audit.py --strategies
  regime_adaptive_htf --registry spot`): 37 trades, gross +0.27, net -0.66,
  drag 0.94pp, `graduate_m1` — matches `docs/research/fee-audit-m5.md` rank 41
  exactly (trades/yr reads 6.2 vs the committed 6.3, a span-rounding artifact).
- `baseline_full.json` — full M1 net baseline (`eval_windows.py --strategy
  regime_adaptive_htf`): protocol IS+OOS PASS on the incumbent-relative bar,
  held-out 2023/2024/2025H1 all FAIL (0/3).
- `noise_m5.json` — noise check on the M5 slices (is+oos): n=37, mean
  +0.082%/trade, permutation p=0.3913, bootstrap 95% CI [-0.510, +0.671] →
  `INDISTINGUISHABLE_FROM_ZERO`.
- `noise_all.json` — noise check pooled across is,oos,2023,2024,2025H1:
  n=173 after calendar-coverage dedupe (the is∩2025H1 overlap fires
  non-identical entries per window — 3 dropped; PR #1172 review finding),
  mean -0.022%/trade (leg-level -0.180%/leg, 21-day overlap disclosed) →
  `NO_POSITIVE_EDGE`. Sign test 110/173 positive (p=0.0004) with a negative
  mean = the classic fade shape: frequent small wins, fat left tail
  (min -11.70%).
- `entry_condition_split.py` (+ `entry_condition_split.json`,
  `entry_condition_split_allwin.json`) — signal-bar regime-label join:
  **every** trade (37/37 on the M5 slices, 176/176 across the raw window
  slices) entered on
  `ranging_volatile`; `ranging_quiet` never fires. There is no second regime
  axis for a selectivity knob to isolate, and the timeframe split flips sign
  between pools (1h +0.61 → -0.06; 4h -1.17 → +0.10) — noise.

## Reproduce

```
uv run --no-sync python backtest/fee_audit.py --strategies regime_adaptive_htf \
  --registry spot --json backtest/candidates/rahtf_1054/fee_repro.json
uv run --no-sync python backtest/eval_windows.py --strategy regime_adaptive_htf \
  --json backtest/candidates/rahtf_1054/baseline_full.json
uv run --no-sync python backtest/gross_edge_noise.py --strategy regime_adaptive_htf \
  --registry spot --json backtest/candidates/rahtf_1054/noise_m5.json
uv run --no-sync python backtest/gross_edge_noise.py --strategy regime_adaptive_htf \
  --registry spot --windows is,oos,2023,2024,2025H1 \
  --json backtest/candidates/rahtf_1054/noise_all.json
uv run --no-sync python backtest/candidates/rahtf_1054/entry_condition_split.py \
  --json backtest/candidates/rahtf_1054/entry_condition_split.json
uv run --no-sync python backtest/candidates/rahtf_1054/entry_condition_split.py \
  --windows is,oos,2023,2024,2025H1 \
  --json backtest/candidates/rahtf_1054/entry_condition_split_allwin.json
```

Runs executed 2026-07-01 (data cache through 2026-06-04/12 per dataset — the
same cache state the M5 audit saw; the fee-audit row reproduces exactly).
Stats are deterministic under the default seed (1066, 10000 resamples).

---
Created with LLM: Fable 5 | high | Harness: Claude Code

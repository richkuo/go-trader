# vwap_rejection_st short-leg baseline (#990)

Generated: 2026-06-15

## Reproduce

M1 protocol OOS short-leg baseline:

```bash
uv run --no-sync python backtest/eval_windows.py --strategy vwap_rejection_st --registry futures --direction short --windows oos --json /tmp/vwap_rejection_st_m1_oos.json
```

M5 short-leg net-vs-gross churn screen:

```bash
uv run --no-sync python backtest/fee_audit.py --registry futures --strategies vwap_rejection_st --direction short --json /tmp/vwap_rejection_st_fee_audit.json --markdown /tmp/vwap_rejection_st_fee_audit.md
```

## M1 OOS Result

`vwap_rejection_st` passes the protocol OOS incumbent-relative bar on the short leg.

| dataset | Sharpe | bar | DDadj | bar | return % | max DD % | trades |
|---|---:|---:|---:|---:|---:|---:|---:|
| BTC/USDT 1h | 2.08 | -1.18 | 1.72 | -0.57 | 28.78 | -16.71 | 1 |
| BTC/USDT 4h | 2.14 | -0.30 | 1.64 | -0.25 | 28.18 | -17.17 | 1 |
| ETH/USDT 1h | 2.54 | -0.19 | 3.14 | -0.23 | 45.68 | -14.55 | 1 |
| ETH/USDT 4h | 2.52 | -0.43 | 2.61 | -0.44 | 39.62 | -15.16 | 1 |
| SOL/USDT 1h | 2.88 | -1.64 | 4.18 | -0.80 | 49.49 | -11.85 | 1 |
| SOL/USDT 4h | 2.74 | -0.76 | 3.71 | -0.66 | 44.83 | -12.10 | 1 |
| mean | 2.48 | -0.75 | 2.83 | -0.49 |  |  |  |

Verdict: `PASS` on 6/6 datasets for Sharpe and 6/6 for DD-adjusted return; traded 6/6 datasets; no liquidated legs.

## M5 Churn Screen

Focused short-leg fee audit over protocol IS+OOS (`is`, `oos`) on the six audit datasets:

| strategy | registry | direction | trades | trades/yr | gross %/leg | net %/leg | fee drag pp | drag/trade pp | net Sharpe | verdict |
|---|---|---|---:|---:|---:|---:|---:|---:|---:|---|
| vwap_rejection_st | futures | short | 12 | 2.0 | 22.54 | 22.38 | 0.16 | 0.1608 | 1.78 | `healthy` |

The churn screen does not resemble `vwap_reversion`: trade count is low, fee drag is small, and the net edge survives fees.

## Conclusion

Do not deprecate on the #990 baseline gate. The first short-leg measurement is mid-tier-or-better by the issue criteria: M1 OOS passes cleanly and M5 is `healthy`. Any next work should be an M1 application/refinement child under #977, not a churn-driven deprecation.

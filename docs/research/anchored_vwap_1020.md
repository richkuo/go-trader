# anchored_vwap #1020 research pass

Date: 2026-06-18
Base: `origin/main` / `2145dc0`

## Commands

Baseline M1:

```bash
uv run --no-sync python backtest/eval_windows.py \
  --strategy anchored_vwap --registry futures \
  --json /tmp/anchored_vwap_1020_m1_long.json

uv run --no-sync python backtest/eval_windows.py \
  --strategy anchored_vwap --registry futures --direction short \
  --json /tmp/anchored_vwap_1020_m1_short.json
```

Baseline M5:

```bash
uv run --no-sync python backtest/fee_audit.py \
  --registry futures --strategies anchored_vwap \
  --json /tmp/anchored_vwap_1020_m5_long.json \
  --markdown /tmp/anchored_vwap_1020_m5_long.md

uv run --no-sync python backtest/fee_audit.py \
  --registry futures --strategies anchored_vwap --direction short \
  --json /tmp/anchored_vwap_1020_m5_short.json \
  --markdown /tmp/anchored_vwap_1020_m5_short.md
```

Short-leg selectivity sweep:

```bash
uv run --no-sync python backtest/eval_windows.py \
  --strategy anchored_vwap --registry futures --direction short --windows oos \
  --sweep pivot_strength=3,5,8,13 \
  --sweep buffer_atr_mult=0.25,0.5,0.75,1.0 \
  --sweep confirm_bars=1,2,3,4 \
  --json /tmp/anchored_vwap_1020_m1_short_sweep_oos.json
```

Full-window checks for the two selected short variants:

```bash
uv run --no-sync python backtest/eval_windows.py \
  --strategy anchored_vwap --registry futures --direction short \
  --params '{"pivot_strength":8,"buffer_atr_mult":1.0,"confirm_bars":1}' \
  --json /tmp/anchored_vwap_1020_m1_short_p8_b1_c1.json

uv run --no-sync python backtest/eval_windows.py \
  --strategy anchored_vwap --registry futures --direction short \
  --params '{"pivot_strength":13,"buffer_atr_mult":1.0,"confirm_bars":1}' \
  --json /tmp/anchored_vwap_1020_m1_short_p13_b1_c1.json
```

## Baseline M1

Default long leg fails every M1 window.

| leg | window | verdict | mean Sharpe vs bar | mean DDadj vs bar | beats Sharpe | beats DDadj |
| --- | --- | --- | ---: | ---: | ---: | ---: |
| long | is | fail | -2.154 vs -0.116 | -0.803 vs -0.141 | 0/6 | 0/6 |
| long | oos | fail | -2.096 vs -0.593 | -0.738 vs -0.405 | 1/6 | 1/6 |
| long | 2023 | fail | 0.534 vs 1.459 | 1.788 vs 3.675 | 1/6 | 1/6 |
| long | 2024 | fail | 0.158 vs 0.904 | 0.479 vs 1.075 | 1/6 | 1/6 |
| long | 2025H1 | fail | -0.788 vs -0.424 | -0.514 vs -0.366 | 3/6 | 2/6 |

Default short leg has one attractive recent window, but it is not robust.

| leg | window | verdict | mean Sharpe vs bar | mean DDadj vs bar | beats Sharpe | beats DDadj |
| --- | --- | --- | ---: | ---: | ---: | ---: |
| short | is | fail | -1.930 vs -0.207 | -0.742 vs -0.223 | 1/6 | 1/6 |
| short | oos | pass | 0.075 vs -0.593 | 0.207 vs -0.405 | 5/6 | 5/6 |
| short | 2023 | fail | -2.603 vs 1.211 | -0.942 vs 2.664 | 0/6 | 0/6 |
| short | 2024 | fail | -1.480 vs 0.904 | -0.725 vs 1.075 | 0/6 | 0/6 |
| short | 2025H1 | fail | -0.647 vs -0.424 | -0.430 vs -0.366 | 3/6 | 3/6 |

## Baseline M5

| leg | trades | trades/yr | gross % | net % | fee drag pp | verdict |
| --- | ---: | ---: | ---: | ---: | ---: | --- |
| long | 989 | 161.4 | -20.051 | -37.124 | 17.073 | `unscreened_short` |
| short | 993 | 162.1 | 1.884 | -19.607 | 21.491 | `graduate_m1` |

Interpretation: the long side has no gross edge. The short side has a small gross edge, but the default signal churn gives up roughly 21.5 percentage points to fees and ends deeply net-negative.

## Short Selectivity Sweep

The OOS sweep found a real short-side high-buffer plateau. Top rows by mean DD-adjusted return:

| params | mean Sharpe | mean DDadj | beats Sharpe | beats DDadj |
| --- | ---: | ---: | ---: | ---: |
| `pivot_strength=8 buffer_atr_mult=1.0 confirm_bars=1` | 2.323 | 2.911 | 6/6 | 6/6 |
| `pivot_strength=8 buffer_atr_mult=1.0 confirm_bars=2` | 1.912 | 2.786 | 5/6 | 5/6 |
| `pivot_strength=5 buffer_atr_mult=1.0 confirm_bars=4` | 1.753 | 2.041 | 6/6 | 6/6 |
| `pivot_strength=8 buffer_atr_mult=1.0 confirm_bars=4` | 1.593 | 2.033 | 6/6 | 5/6 |
| `pivot_strength=8 buffer_atr_mult=1.0 confirm_bars=3` | 1.568 | 2.013 | 5/6 | 5/6 |
| `pivot_strength=13 buffer_atr_mult=0.75 confirm_bars=1` | 1.020 | 1.958 | 6/6 | 6/6 |
| `pivot_strength=13 buffer_atr_mult=1.0 confirm_bars=1` | 1.327 | 1.930 | 5/6 | 5/6 |
| `pivot_strength=5 buffer_atr_mult=1.0 confirm_bars=1` | 1.721 | 1.848 | 6/6 | 6/6 |

This is not enough by itself: the OOS window is the recent bear-heavy stress slice. Full held-out validation is the gate.

## Selected Variants

Best OOS variant:

| params | is | oos | 2023 | 2024 | 2025H1 | note |
| --- | --- | --- | --- | --- | --- | --- |
| `pivot_strength=8, buffer_atr_mult=1.0, confirm_bars=1` | fail | pass | fail | fail | fail | 2024 SOL/USDT 4h liquidated |

Less twitchy high-buffer variant:

| params | is | oos | 2023 | 2024 | 2025H1 | note |
| --- | --- | --- | --- | --- | --- | --- |
| `pivot_strength=13, buffer_atr_mult=1.0, confirm_bars=1` | fail | pass | fail | fail | pass | 1/3 held-out windows pass |

The selectivity sweep improves recent OOS materially, but it still does not survive 2023/2024 bull-market held-out stress. The high-buffer versions are basically short-regime harvesters without a reliable regime gate.

## Decision

Do not deploy `anchored_vwap` from this pass.

Current disposition: **registry-only**. Do not change defaults and do not promote a live/paper config. A hard deprecation is premature because the short side shows a small gross edge and a high-buffer OOS plateau, but the edge needs either:

1. an explicit trend/regime filter from #1017,
2. a higher-timeframe-only research pass (especially 1d), or
3. a separate short-aware WFO warmup implementation before optimize-mode short sweeps are meaningful.

Absent one of those, the issue should not graduate to deployment. The long side can be treated as rejected for now.

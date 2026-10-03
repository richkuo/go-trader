# vol_momentum short-leg + plateau check (#1021)

Generated: 2026-06-16

## Reproduce

Focused current-cache M5 long-leg check:

```bash
uv run --no-sync python backtest/fee_audit.py --registry futures --strategies vol_momentum --json /tmp/vol_momentum_1021_fee_long.json --markdown /tmp/vol_momentum_1021_fee_long.md
```

Focused current-cache M5 short-leg check:

```bash
uv run --no-sync python backtest/fee_audit.py --registry futures --strategies vol_momentum --direction short --json /tmp/vol_momentum_1021_fee_short.json --markdown /tmp/vol_momentum_1021_fee_short.md
```

M1 long/short protocol runs:

```bash
uv run --no-sync python backtest/eval_windows.py --strategy vol_momentum --registry futures --json /tmp/vol_momentum_1021_m1_long.json
uv run --no-sync python backtest/eval_windows.py --strategy vol_momentum --registry futures --direction short --json /tmp/vol_momentum_1021_m1_short.json
```

OOS plateau sweeps, keeping the futures `allow_short=true` variant explicit so
the sweep does not silently become the spot/long-only default:

```bash
uv run --no-sync python backtest/eval_windows.py --strategy vol_momentum --registry futures --params '{"allow_short": true}' --windows oos --sweep entry_threshold=0.25,0.30,0.35 --sweep exit_threshold=0,0.05,0.10 --sweep eff_entry=0.30,0.35,0.40 --sweep eff_exit=0.10,0.15,0.20 --json /tmp/vol_momentum_1021_plateau_long_oos.json
uv run --no-sync python backtest/eval_windows.py --strategy vol_momentum --registry futures --direction short --params '{"allow_short": true}' --windows oos --sweep entry_threshold=0.25,0.30,0.35 --sweep exit_threshold=0,0.05,0.10 --sweep eff_entry=0.30,0.35,0.40 --sweep eff_exit=0.10,0.15,0.20 --json /tmp/vol_momentum_1021_plateau_short_oos.json
```

Held-out validation of the OOS-pass candidates:

```bash
uv run --no-sync python backtest/eval_windows.py --strategy vol_momentum --registry futures --params '{"allow_short": true, "entry_threshold": 0.30, "exit_threshold": 0.05, "eff_entry": 0.30, "eff_exit": 0.20}' --json /tmp/vol_momentum_1021_m1_long_eff30_exit20.json
uv run --no-sync python backtest/eval_windows.py --strategy vol_momentum --registry futures --params '{"allow_short": true, "entry_threshold": 0.30, "exit_threshold": 0.0, "eff_entry": 0.30, "eff_exit": 0.20}' --json /tmp/vol_momentum_1021_m1_long_exit00_eff30_exit20.json
uv run --no-sync python backtest/eval_windows.py --strategy vol_momentum --registry futures --params '{"allow_short": true, "entry_threshold": 0.30, "exit_threshold": 0.10, "eff_entry": 0.30, "eff_exit": 0.15}' --json /tmp/vol_momentum_1021_m1_long_exit10_eff30_exit15.json
uv run --no-sync python backtest/eval_windows.py --strategy vol_momentum --registry futures --params '{"allow_short": true, "entry_threshold": 0.30, "exit_threshold": 0.10, "eff_entry": 0.30, "eff_exit": 0.20}' --json /tmp/vol_momentum_1021_m1_long_exit10_eff30_exit20.json
uv run --no-sync python backtest/eval_windows.py --strategy vol_momentum --registry futures --direction short --params '{"allow_short": true, "entry_threshold": 0.35, "exit_threshold": 0.10, "eff_entry": 0.40, "eff_exit": 0.20}' --json /tmp/vol_momentum_1021_m1_short_selective.json
```

## Baseline Reproduction

The validation target in #1021 is the committed PR #1003 M5 row in
`docs/research/fee-audit-m5.md`: futures `vol_momentum`, long leg only,
146 trades, 24.9/yr, -0.27% gross, -3.81% net, verdict `unscreened_short`.

The current-cache focused rerun does **not** reproduce that row to the digit
because the local cache now extends to 2026-06-16. The current-cache long-leg
focused screen is materially worse:

| direction | trades | trades/yr | gross %/leg | net %/leg | fee drag pp | net Sharpe | verdict |
|---|---:|---:|---:|---:|---:|---:|---|
| long | 78 | 28.5 | -16.29 | -19.41 | 3.12 | -0.67 | `unscreened_short` |
| short | 151 | 24.8 | 15.77 | 11.33 | 4.44 | 0.79 | `healthy` |

The short leg has a real current-cache fee-adjusted edge. The question is
whether it survives the full M1 held-out gate.

## M1 Direction Results

Default long/short protocol and held-out windows:

| direction | IS | OOS | 2023 | 2024 | 2025H1 | held-out |
|---|---|---|---|---|---|---|
| long | PASS | **FAIL** | FAIL | FAIL | FAIL | 0/3 |
| short | PASS | **PASS** | FAIL | FAIL | PASS | 1/3 |

Default short OOS is strong (mean Sharpe 0.90 vs bar -0.52, DDadj 1.04 vs
bar -0.35), but the held-out stress exposes the regime dependency: the short
leg fails the 2023 and 2024 bull windows, including one 2023 liquidation on
SOL/USDT 4h.

## Threshold Plateau

Long-side OOS sweep over the 3x3x3x3 neighborhood found only a small pass
pocket:

| sweep | pass combos | total combos | note |
|---|---:|---:|---|
| long | 4 | 81 | all require `entry_threshold=0.30`, `eff_entry=0.30`; best DDadj -0.19 vs bar -0.36 |
| short | 81 | 81 | broad OOS pass plateau; Sharpe range 0.24 to 1.59, DDadj range 0.63 to 1.76 |

The long-side OOS-pass pocket is not deployable. All four OOS-pass neighbors
fail every held-out window:

| params | IS | OOS | 2023 | 2024 | 2025H1 | held-out |
|---|---|---|---|---|---|---|
| `entry=0.30 exit=0.00 eff_entry=0.30 eff_exit=0.20` | PASS | PASS | FAIL | FAIL | FAIL | 0/3 |
| `entry=0.30 exit=0.05 eff_entry=0.30 eff_exit=0.20` | PASS | PASS | FAIL | FAIL | FAIL | 0/3 |
| `entry=0.30 exit=0.10 eff_entry=0.30 eff_exit=0.15` | PASS | PASS | FAIL | FAIL | FAIL | 0/3 |
| `entry=0.30 exit=0.10 eff_entry=0.30 eff_exit=0.20` | PASS | PASS | FAIL | FAIL | FAIL | 0/3 |

The short-side plateau is broad on OOS, but the stricter corner
(`entry_threshold=0.35`, `exit_threshold=0.10`, `eff_entry=0.40`,
`eff_exit=0.20`) still fails all held-out windows and liquidates once in
2023:

| candidate | IS | OOS | 2023 | 2024 | 2025H1 | held-out |
|---|---|---|---|---|---|---|
| default short | PASS | PASS | FAIL | FAIL | PASS | 1/3 |
| high-selectivity short | PASS | PASS | FAIL | FAIL | FAIL | 0/3 |

## Verdict

`vol_momentum` should not receive a registry-default parameter change. The
long leg fails protocol OOS on current cache; the small long OOS tuning pocket
does not survive held-out stress. The short leg is fee-healthy and clears the
judged OOS window, but it is a regime-dependent bear-window edge that fails
the 2023/2024 held-outs and can liquidate in strong bull regimes.

Per #1021's decision gate, move `vol_momentum` to the deprecation list for the
default strategy set. Do not iterate past M1 on threshold tuning. A future
follow-up would need a regime-profile/directional gate that explicitly avoids
short-only exposure in bull windows, not another static threshold sweep.

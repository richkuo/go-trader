# regime_adaptive entry filters (issue 2017)

Offline study. It asks whether an existing entry filter leaves `regime_adaptive` with a positive return after fees and funding.

The historical audit frame is spot-registry long entries on BTC, ETH and SOL at 1h and 4h, on the pinned Binance US candles, with Hyperliquid fees and funding. The perps frame is a research context at 30m and 1h for long, short and both. It is not a deployment record. A perps verdict needs native Hyperliquid candles over the frozen judgment windows and a lot-aware execution spec. The pinned venue files are the recent tail, and the scored book is the cash cache path.

No command sends an order. The study does not change a live config, a paper config, a registry default, the quarantine roster, or `edge_status`.

## Commands

Run these from the repository root, in order. `lock` must come before `select`. `select` writes the shortlist before `judge` scores a judgment window.

```
uv run --no-sync python backtest/candidates/regime_adaptive_2017/run_study.py verify-inputs
uv run --no-sync python backtest/candidates/regime_adaptive_2017/run_study.py lock
uv run --no-sync python backtest/candidates/regime_adaptive_2017/run_study.py coverage
uv run --no-sync python backtest/candidates/regime_adaptive_2017/run_study.py select --cache /tmp/ra2017.db --work /tmp/ra2017
uv run --no-sync python backtest/candidates/regime_adaptive_2017/run_study.py judge --cache /tmp/ra2017.db --work /tmp/ra2017
uv run --no-sync python backtest/candidates/regime_adaptive_2017/run_study.py render
```

Scoring reads `backtest/candidates/tema_cross_2016/data` by hash, loads a new cache, and blocks network access. The cache and checkpoints stay outside the repository.

## Errata

These notes describe the committed run. They do not change `protocol.json`, `run_study.py`, or the lock.

### The audit suggestion was already below the training floor

`combo_from_picks` builds a combination whenever two filter families have a pick. It does not apply the sample floor. `suggestion_name` then names that combination the suggestion arm. The audit long combination (mean-reversion gate, higher-timeframe filter, and `mr_entry_z` 3.0) opened 0 training positions and is recorded `eligible: false` with reason `position floor` in `selection.json`. The audit verdict, "suggestion arm is below the held-out sample floor", was therefore fixed at selection, before any judgment window. The held-out rows for `htf` (net 2.7448% at base cost and 2.6418% at stress) and `z_3_0` (0.5058% and 0.4052%) are descriptive. No searched arm survives the family correction (0 survivors of 44). They are not a decision. Answering the audit question needs a fresh study, with a frozen floor-or-fallback combination rule, on data that study has not already read.

The perps short combination opened 45 training positions and was eligible. This note does not say that it failed the training floor.

### What the report columns and the M1 lines actually are

`REPORT.md` says gross position return is net return plus fee drag, and that fee drag is fees divided by entry notional. The table does not follow that sentence.

- **Gross %** is the mean per-position net return of the zero-commission, zero-slippage run. On the audit control row that value is 0.3985. It is not 0.2080 + 0.0902.
- **Fee drag pp** is commission only (`entry_fee` + `exit_fee`), about 0.090 percentage points, which is two times the 0.045% taker fee. Slippage is not in this column.
- **Total cost drag** for a row is Gross % minus Net %. For the audit control row, 0.3985 − 0.2080 = 0.1905. For the perps both control row, Gross % is 0.0612 and Net % is −0.1172.

The M1 lines are not `compute_incumbent_legs` plus `score_candidate`. Each incumbent is one `run_leg` at the study's base cost and direction. The pass rule compares mean Sharpe and mean drawdown-adjusted return, requires at least half the datasets to trade, and checks funding only on the candidate legs. It does not read the incumbent bar's `funding_incomplete` flag.

### What the lock does not pin

`protocol_lock.json` records `git_head` `ab5214ca216e5923a3f8f5c104e161fca4d41033`. That commit does not contain this study. The order of the run is the study's own timestamps: locked at 2026-11-12T06:00:50Z, selected at 2026-11-12T07:48:42Z, first judged leg at 2026-11-12T07:49:15Z.

The lock hashes the files in `ENGINE_FILES`. It does not hash these modules, which the scored legs also call. Their sha256 at commit `ff314316be1bc6856abd0e01f15b28b8e22a69d5` is:

| File | sha256 |
| --- | --- |
| `backtest/tune_live.py` | `36a722b6bbfa1629a7157827cd9ecc1221dedc0af62c745f2e5e01333c7666c9` |
| `backtest/registry_loader.py` | `376811b19edc73f14893bf903d2651a67e0983b23e0c61b37679e859b54f3140` |
| `shared_strategies/open/spot/strategies.py` | `0a40fe1abf06399588f893154e71988b0dd3f6eaddfc4660c6fe59116ea13db9` |
| `shared_strategies/open/futures/strategies.py` | `3cd3907e2cac5574cfe2839adec33c21db6503cc8e1d57ad6d932d1a59e0b1d7` |
| `shared_strategies/open/registry.py` | `f0cdce0e7011ea9322754d88cbe42c06e77b03ac6e315bdd5db27334a1a30b0d` |

A later edit only to `regime_adaptive` in `shared_strategies/open/registry.py`, or only to `build_candidate` in `backtest/tune_live.py`, does not change the lock digest. `judge` with the old checkpoint directory would reuse the saved legs. Those files belong in `ENGINE_FILES` at the next re-lock. This run does not re-lock.

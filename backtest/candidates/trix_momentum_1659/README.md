# trix_momentum_1659

Study for the no-edge `trix_momentum` entry evaluator (issue 1659). The verdict and every number are in `REPORT.md`;
this file lists what each artifact is for.

| file | role |
|---|---|
| `protocol.json` | Frozen protocol: arms, grids, seed, finite windows (pinned end 2026-10-01), 300-bar warmup, costs and x2 slippage stress, selection rule and cluster floor, evidence gates, pass conditions, parity spans and the sha256 pins of every input file. |
| `study_manifest.json` | Written by `run_study.py freeze`: the protocol sha256 at freeze time and the provenance of the reused inputs. |
| `spec_seed.json` | Seed candidate spec for `eval_windows.py --candidate-json` (direction `both`, one `tiered_tp_pct` ladder, `stop_loss_atr_mult: 1.0`). |
| `selected_{1h,4h}_{candidate_both,candidate_long}.json` | The training selections written by `run_study.py run`, same format. |
| `paper_parity_1h.json`, `paper_parity_4h.json` | Explicit-paper go-trader configurations (BTC, `closed_bar_decisions: true`, the frozen `tiered_tp_pct` ladder, `stop_loss_atr_mult: 1.0`, `direction: both`); they load in the Go loader as unacknowledged paper. |
| `run_study.py` | `freeze` (verify pins, write the manifest; no network), `run` (rebuild a fresh cache from the frozen files, disable network fetches, run the protocol through the M1 runner functions and `parity_diff`, then render), `render` (re-render `REPORT.md` from `results.json`). |
| `results.json` | Full output: every training variant, every held-out arm and stress leg, funding coverage, next-bar fill checks, the continuous-history reference, stability rows, parity runs and the verdict. |
| `evidence/entries_1h.csv.gz`, `evidence/entries_4h.csv.gz` | Every held-out entry signal of the selected bidirectional candidate: diagnostic values at full precision, the hash of the exact L + 2 input rows, and whether replaying the entry on only those rows reproduced it. |
| `discovery_{before,after}_{spot,futures}.json` | `--list-json` output of the spot and futures shims before and after the registration; they are identical because a no-edge name is hidden from discovery. |

The inputs are the hash-pinned Binance US candles and Hyperliquid funding frozen by study `chande_momentum_1661`, read
in place. The evaluator holds entries whose dependency window contains a candle gap (`cadence_gap`), and the engine keeps
managing open positions through it.

All commands are suggest-only. Nothing here edits deployed configuration, live defaults or acknowledgements.

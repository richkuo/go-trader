# adaptive_moving_average_1641

Study for the no-edge `adaptive_moving_average` entry evaluator (issue 1641). The verdict and every number are in
`REPORT.md`; this file lists what each artifact is for.

| file | role |
|---|---|
| `protocol.json` | Frozen protocol: grids, seed, windows (pinned end 2026-10-01), 300-bar warmup, costs and x2 slippage stress, selection rule, sample floor, pass/fail/inconclusive rules, parity spans and the sha256 pins of every input file. |
| `study_manifest.json` | Written by `run_study.py freeze`: the protocol sha256 at freeze time and the provenance of the reused inputs. |
| `spec_seed.json` | Seed candidate spec for `eval_windows.py --candidate-json` (direction `both`, one `tiered_tp_pct` close, `stop_loss_atr_mult: 1.0`). |
| `selected_1h.json`, `selected_4h.json` | The training selections written by `run_study.py run`, same format. |
| `paper_parity_1h.json`, `paper_parity_4h.json` | Explicit-paper go-trader configurations (BTC, `closed_bar_decisions: true`, `tiered_tp_pct` +3%, `stop_loss_atr_mult: 1.0`, `direction: both`); they load in the Go loader as unacknowledged paper. |
| `run_study.py` | `freeze` (verify pins, write the manifest; no network), `run` (rebuild a fresh cache from the frozen files, disable network fetches, run the protocol through the M1 runner functions), `parity` (paper configurations through `parity_diff`), `render`. |
| `render_report.py` | Renders `REPORT.md` from `results.json`, `parity_results.json` and the manifest. |
| `results.json`, `parity_results.json` | Full outputs, including every training variant, every held-out arm, funding coverage, next-bar fill checks and the stability rows. |
| `evidence/entries_1h.csv.gz`, `evidence/entries_4h.csv.gz` | Every held-out entry signal of the selected candidate: indicator values at full precision, the hash of the exact input rows, and whether replaying the entry on only those rows reproduced it. |
| `discovery_{before,after}_{spot,futures}.json` | `--list-json` output of the spot and futures shims before and after the registration; they are identical because a no-edge name is hidden from discovery. |

The inputs are the hash-pinned Binance US candles and Hyperliquid funding frozen by study `chande_momentum_1661`,
read in place. Binance US candles have two multi-hour outages in that span (February 2023 and 31 August 2026). The
evaluator holds entries whose dependency window contains such a gap (`cadence_gap`), and the engine keeps managing
open positions through it. The warmup rule refuses a window whose 300 pre-window bars are not consecutive; no
protocol window touches a gap in its warmup.

All commands are suggest-only. Nothing here edits deployed configuration, live defaults or acknowledgements.

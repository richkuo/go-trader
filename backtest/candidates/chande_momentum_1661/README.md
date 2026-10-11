# chande_momentum_1661

Study for the no-edge `chande_momentum` entry evaluator (issue 1661). The verdict and every number are in
`REPORT.md`; this file lists what each artifact is for.

| file | role |
|---|---|
| `protocol.json` | Frozen protocol: grids, `is`-window selection rule, confirmation windows, sample floor, funding rule, pass/fail/inconclusive rules and the one amendment made before scoring. |
| `spec_1h.json`, `spec_4h.json` | Seed candidate specs for `eval_windows.py --candidate-json` (period 20, threshold 25, fixed `expected_interval_ms` per interval). |
| `selected_1h.json`, `selected_4h.json` | The locked training selections, written by `run_study.py run`, same format. |
| `paper_parity_1h.json`, `paper_parity_4h.json` | Explicit-paper go-trader configurations (BTC, `tiered_tp_pct` +3%, `stop_loss_atr_mult: 1.0`, `direction: both`) used for decision parity; they load in the Go loader as unacknowledged paper. |
| `study_manifest.json`, `data/` | Hash-pinned Binance US candles (1h, 4h) and Hyperliquid funding, 2023-01-01 to the pinned end 2026-10-01. |
| `run_study.py` | `acquire` (network, writes `data/` + manifest), `run` (offline: rebuilds a fresh cache from `data/`, verifies every hash, disables network acquisition, runs the protocol through the M1 runner functions), `parity` (paper configs through `parity_diff`), `render`. |
| `render_report.py` | Renders `REPORT.md` from `results.json`, `parity_results.json` and the manifest. |
| `results.json`, `parity_results.json` | Full outputs, including every training variant and the exploratory stability sweep. |

Binance US candles have a few multi-hour outages in this span. The evaluator holds entries whose windows contain
such a gap (`cadence_gap`), and the engine keeps managing open positions through it.

Hyperliquid funding history begins in May 2023, so under `--funding charge` any leg holding a position before the
first funding record is funding-incomplete. `protocol.json` treats such a window as unscorable: it cannot support a
pass, and the remaining complete windows decide whether a fail is established.

All commands are suggest-only. Nothing here edits deployed configuration, live defaults or acknowledgements.

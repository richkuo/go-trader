# breakout_2014

Study for issue 2014: does the composite `trending_up_clean` entry gate improve `breakout` in the shape operators run
on Hyperliquid perps (ten paper strategies: BNB, BTC, ETH, HYPE and SOL at 30m and 1h, base `direction: long`, the
`trailing_tp_ratchet_regime` close with a 2.5 ATR regime trail, one composite window of period 350)? The verdict and
every number are in `REPORT.md`. Everything here is suggest-only: no command writes a live or paper config, a
registry default or `edge_status`, and none sends an order.

| file | role |
|---|---|
| `deployed_breakout_sanitized.json` | Sanitized copy of the deployed shape: the ten `breakout` strategies and the `regime` block with generic ids, `config_version` 20, `atr_method: wilder`, `default_stop_loss_atr_mult: 1`, `regime_enabled: true`; no channel, wallet, storage, host or credential field. |
| `protocol.json` | Frozen protocol: arms, windows, warm-up, dataset sets, the `is` shortlist rule, the adoption rule, the venue veto rule, fidelity bars and the first-study rerun. |
| `protocol_lock.json` | Written by `run_study.py lock` before the first score; every scoring command refuses a different `protocol.json` or `coverage_report.json`. |
| `arms/control.json`, `arms/gate_p21.json`, `arms/gate_p350.json` | Pure builds of the deployed copy (`build-arms`); policy arms reuse them with the configured `regime_directional_policy` attached to the candidate. |
| `study_manifest.json`, `data/proxy/` | Frozen Binance US 30m and 1h candles (BNB, BTC, ETH, HYPE, SOL) and Hyperliquid funding with sha256 values, gaps, first venue rate and venue-absent hours. |
| `venue_manifest_30m.json`, `venue_manifest_1h.json`, `data/venue_30m/`, `data/venue_1h/` | Frozen Hyperliquid candles, funding, venue meta and L2 snapshot (`offline_candle_manifest/v1`, warm-up 400 bars, `tail` window; the 1h manifest also has `common`, the 30m tail dates). |
| `bridge_manifest_30m.json`, `bridge_manifest_1h.json` | The frozen Binance US candles on the venue windows with the venue funding and execution specification, so only the candle source differs. |
| `coverage_report.json` | Read-only coverage check of every dataset in both groups: included or excluded with the reason. |
| `cache_parity.json` | Legs scored with and without the label cache; they must be byte-identical. |
| `timing.json` | One measured leg and the projected label cost of the full run. |
| `selection.json` | The `is` shortlist and primary arm per dataset set, written once before any judged window. |
| `results.json`, `run_log.json` | Every judged proxy, venue, bridge and common-date window, the reference arms, the first-study rerun and the `policy_gate_p350` equality check; one log entry per judge run. |
| `fidelity.json` | Live-label fidelity rows (`validate_frames`) and the engine label check. |
| `verdict.json`, `suggested_paper_config.json` | The adoption verdict per set; the suggested config exists only when a set is adopted. |
| `run_study.py`, `render_report.py` | The driver and the report renderer. |

## Reproduction

Commands in order. Only `acquire-proxy` and the two `offline_manifest.py acquire` runs read the network, and only
public market data with `HYPERLIQUID_SECRET_KEY` unset; they ran once and refuse to overwrite. Every later command
reads only the frozen files, rebuilds a throwaway cache at a new path (never `shared_tools/trading_bot.db`) and
blocks the network at the socket level.

```
uv run --no-sync python backtest/candidates/breakout_2014/run_study.py build-arms --check
env -u HYPERLIQUID_SECRET_KEY uv run --no-sync python backtest/candidates/breakout_2014/run_study.py acquire-proxy --cache-db <new path>
env -u HYPERLIQUID_SECRET_KEY GO_TRADER_OHLCV_CACHE_DB=<new path> uv run --no-sync python backtest/offline_manifest.py acquire --manifest backtest/candidates/breakout_2014/venue_manifest_30m.json --write-hashes
env -u HYPERLIQUID_SECRET_KEY GO_TRADER_OHLCV_CACHE_DB=<new path> uv run --no-sync python backtest/offline_manifest.py acquire --manifest backtest/candidates/breakout_2014/venue_manifest_1h.json --write-hashes
uv run --no-sync python backtest/offline_manifest.py verify --manifest backtest/candidates/breakout_2014/venue_manifest_30m.json
uv run --no-sync python backtest/candidates/breakout_2014/run_study.py verify-frozen
uv run --no-sync python backtest/candidates/breakout_2014/run_study.py coverage
uv run --no-sync python backtest/candidates/breakout_2014/run_study.py lock
uv run --no-sync python backtest/candidates/breakout_2014/run_study.py cache-parity --cache-db <new path> --work-dir <dir>
uv run --no-sync python backtest/candidates/breakout_2014/run_study.py time-leg --cache-db <new path> --work-dir <dir>
uv run --no-sync python backtest/candidates/breakout_2014/run_study.py select --cache-db <new path> --work-dir <dir> --workers 8
uv run --no-sync python backtest/candidates/breakout_2014/run_study.py judge --cache-db <new path> --work-dir <dir> --workers 8
uv run --no-sync python backtest/candidates/breakout_2014/run_study.py fidelity --work-dir <dir> --workers 8
uv run --no-sync python backtest/candidates/breakout_2014/run_study.py suggest
uv run --no-sync python backtest/candidates/breakout_2014/run_study.py render
go -C scheduler build -o <scratch>/go-trader . && <scratch>/go-trader inspect --config backtest/candidates/breakout_2014/suggested_paper_config.json --all
```

`--work-dir` holds the label cache and one checkpoint per dataset and window; a rerun with the same `--work-dir`
reuses checkpoints whose protocol, engine-file and arm hashes match, so an interrupted `select`, `judge` or
`fidelity` resumes where it stopped. `select` writes `selection.json` once and `judge` refuses to start without it.
`fidelity` reads the label cache that `judge` wrote to check the engine labels against the validator.

# dual_thrust_2019

Frozen confirmation of the `dual_thrust_1642` Hyperliquid 4h settings on additional coins (issue 2019). Primary verdict: **fail** (false: beats_breakout_sharpe_base, m1_pass_base, net_positive_base, net_positive_stress). Every number is in `REPORT.md`; this file lists what each artifact is for. Everything here is suggest-only: no command writes configuration, registry metadata, defaults or acknowledgements, and none sends an order.

| file | role |
|---|---|
| `protocol.json` | Frozen rules: settings, contract, costs, populations, screen, sample floor and the ordered verdict rule. Committed before acquisition. |
| `screen.json` | The liquidity-and-coverage screen: observation time, ranking, every screened coin with its checks, and the frozen rosters. |
| `study_manifest.json` | Input inventory: every file under `data/` with sha256, rows, coin, interval, acquired, scored and warm-up ranges, and the manifest hashes. |
| `venue_primary_4h.json`, `venue_forward_4h.json`, `venue_descriptive_1h.json` | One `offline_candle_manifest/v1` manifest per population. |
| `data/venue/` | Frozen Hyperliquid candles, hourly funding, venue meta, the liquidity response and the L2 snapshot. |
| `protocol_lock.json` | Written by `lock` before any score: protocol, inventory, screen, manifest, input and scoring-source hashes. |
| `run_study.py` | `acquire` (network), `lock`, `run` (offline, network blocked, fresh research cache), `verify`, `render`. |
| `verify_study.py` | Integration verification on disposable copies and saved evidence (`run_study.py verify`). |
| `report.py` | Writes `REPORT.md` and this file from the committed results (`run_study.py render`). |
| `results.json` | Every leg at both costs for every population, input checks, the entry audit, reductions and the verdict. |
| `verification.json` | Outcome of every verification case. |

## Reproduction

```
uv run --no-sync python backtest/candidates/dual_thrust_2019/run_study.py run --cache-db <fresh-research-path>/cache.db --out <path>/rerun.json
uv run --no-sync python backtest/candidates/dual_thrust_2019/run_study.py verify --work-dir <fresh-empty-directory>
uv run --no-sync python backtest/candidates/dual_thrust_2019/run_study.py render
```

`run` refuses an existing cache, a missing or changed lock, an unregistered input, uncommitted locked files and changed scoring sources; check out the lock's source commit to reproduce on a later tree. Without `--out` it refuses to replace `results.json`.

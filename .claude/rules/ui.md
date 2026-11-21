---
paths:
  - "scheduler/ui_*.go"
  - "scheduler/server.go"
  - "scheduler/static/**"
  - "scheduler/executor.go"
  - "scheduler/trade_feedback*.go"
  - "scheduler/trade_review*.go"
  - "scheduler/candle_archive_cmd.go"
---

## UI

- `/tuning` never writes config;`ui_tuning.go`=`spawnPythonProcessWithEnv`,NEVER `runPython*`;`POST /api/tuning/apply`=sole promotion.
- `uiPartitionParam`=sole `?partition=` resolver:unparseable=400,unowned=404;diagnostics filter `SourceRole` pre paging+total;cash flow `live_owned`;selected correlation/portfolio-risk NEVER untagged legacy
- Dashboard candles(`ui_candle_*`):published feed copies|read-only archive|trades(display only),NEVER venue/Python/`mu`/feed requirement on a read or miss;review=archive only,generation rechecked under `configWriteMu`(SKILL.md Dashboard Candle Sources)

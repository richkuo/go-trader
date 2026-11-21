---
paths:
  - "scheduler/ui_*.go"
  - "scheduler/server.go"
  - "scheduler/static/**"
  - "scheduler/executor.go"
  - "scheduler/trade_feedback*.go"
---

## UI

- `/tuning` never writes config;`ui_tuning.go`=`spawnPythonProcessWithEnv`,NEVER `runPython*`;`POST /api/tuning/apply`=sole promotion.
- `uiPartitionParam`=sole `?partition=` resolver:unparseable=400,unowned=404;diagnostics filter `SourceRole` pre paging+total;cash flow `live_owned`;selected correlation/portfolio-risk NEVER untagged legacy

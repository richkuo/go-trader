---
paths:
  - "scheduler/*feed*.go"
  - "scheduler/hyperliquid_balance.go"
  - "scheduler/hyperliquid_candles.go"
  - "scheduler/hyperliquid_funding.go"
  - "scheduler/hyperliquid_marks.go"
  - "scheduler/hyperliquid_lot_metadata.go"
  - "scheduler/hyperliquid_paper_funding*.go"
  - "scheduler/main.go"
  - "scheduler/agent_info.go"
  - "scheduler/config*.go"
  - "scheduler/egress*.go"
  - "scheduler/probe*.go"
  - "scheduler/server.go"
  - "shared_scripts/**"
  - "platforms/hyperliquid/**"
  - "scripts/shared-feed-convert.sh"
  - "scripts/feed-parity.sh"
  - "scripts/feed-source-compare.sh"
  - "scripts/fixtures/shared_feed_*"
  - "scripts/update_helpers.sh"
  - "scripts/migrate-service-layout.py"
  - "scripts/test_shared_feed_convert_budget.sh"
  - "scripts/hl_request_ledger/**"
---

## Market feed

- `market_feed_*.go`:`role: feed` runs pre-`LoadConfig`(no DB/lock/probe).
  - Seal bytes immutable;missed/evicted/pre-start key=`unavailable`,NEVER current data;sealer never takes `mu`.
  - Consumer(`websocket|shared`):1 sealed stdin `marketSnapshot`,paper funding=seal;missing frame=error,NEVER private fetch;1 deadline/cycle,non-nil degraded snapshot,primary>backup;ready backup fills ONLY missing|unready candle key,never by lookback.
  - `/info` ONLY `hlInfoPost`,weight held pre-send(shared:durable,no heap fallback),refund ONLY `settle`;candle window>5000 rows NEVER sent;429=host pause,no retry,`rate_limited` til post-pause fetch,never unhealthy.
  - Key ready ONLY after verified full history(lookback|venue edge)+REST deadline refresh,except `feedClosedFrameReuse`(closed-bar-only readers,host pause refuses,seal v9 `forming_bar_omitted`);funding tail falls back to full;WS seal/repair incremental-only.
  - Correction:outside `feedMu`,closed bars only,never `LastRecvAt`/seals/readiness.
  - Weight file(not SQLite):no `flock` wait under mutex;never unlink locks;stopped owner holds `weight_reserve`(cap=ceiling-max reserve of ANY owner),lowered ONLY by same key(register|full-policy reload|`feed-weight-release`)

# inventory_market_making study (issue 1861)

Verdict: **INCONCLUSIVE**. Registry: `inventory_market_making` stays `no_edge` and `production_refused`; edge_source `study_inconclusive`. A PASS would describe only this frozen simulation and would give no live promotion.

## Why

- no pinned recording with raw executed trades and l2Book depth exists: queue-aware evidence needs a bounded public `go-trader record-observations --kinds l2_book,trade_flow` recording of at least 30 complete UTC days (the archive holds depth only)
- no pinned public fundingHistory records cover the recording window
- liquidation_and_margin: no liquidation price or venue free margin is modeled; the envelope uses book cash
- maker_fee: the Hyperliquid maker rate for this account tier is not measured (issue 1726 found no maker fill); 1.5 bps is an unverified constant and no rebate is assumed
- minimum_order_rule: the venue minimum order value and its reduce-only exception are not pinned from venue evidence
- placement_latency: production delivery, placement and cancel latency are not measured; the frozen 50 ms is an assumption
- no eligible data fills all three chronological splits

## Frozen protocol

- Protocol lock sha256 `e131dbb8b8516cf6dd829147e8501aaa0efe2a7fde902702df6521d69d7f1e37`; data manifest sha256 `dd5f378f0f0ae62a55dfe3dfa385e9f61f3362932c88dcec65c098bb7df58bd6`.
- Policy specification sha256 `ae70fffa7c988a2ee108670071277fe9e0c916fdf4f2b8d7524fa43856844856` (registry and discovery agree; binary discovery: {'spec_sha256': 'ae70fffa7c988a2ee108670071277fe9e0c916fdf4f2b8d7524fa43856844856', 'strategy': 'inventory_market_making', 'queue_model': 'strict_trade_through_v1'}).
- Coins BTC; cycle 5 s; cap USD 1000; stop 2%; capital USD 10000, leverage 1.
- Costs: maker 1.5 bps, taker 4.5 bps, slippage reserve 5 bps, minimum order USD 10; stress doubles them with 100 ms latency.
- Queue model `strict_trade_through_v1`, participation 1/100 of each trade; latency 50 ms place, 50 ms cancel.
- Splits {'held_out': 0.2, 'train': 0.6, 'validation': 0.2}; eligibility: maximal receipt-time windows where raw-trade minutes are complete and valid (one minute trimmed from each end of every run of at least three complete minutes) and no l2Book coverage-break marker occurs; the first deadline is the first whole cycle after the warmup (volatility window + 1 s predecessor + maximum input age) past the window start; windows shorter than the minimum are dropped; each eligible window is cut into bounded runs of at most max_deadlines_per_run deadlines; splits are chronological over the concatenated eligible deadlines, each run starts flat with its own state, and a run never spans a split boundary.
- Grid {'min_spread_bps': [6, 10, 20], 'risk_aversion': [0.5, 1, 2], 'volatility_multiplier': [0.5, 1, 2]} (27 variants); fixed-spread baseline {'min_spread_bps_grid': [4, 6, 10, 20, 40], 'risk_aversion': 0, 'volatility_multiplier': 0} fitted on training only.
- Gates: 200 held-out round trips, 30 scored UTC days, positive held-out net and improvement, positive lower bootstrap bounds (seed 1636, 10000 resamples, blocks [1, 7] days), drawdown at most 10% of initial equity, positive stress net.
- Circuit-breaker and portfolio kill-switch triggers are frozen off in the runner, so their exits are outside these economics; the retained quote integration tests cover the latch branches.

## Assumptions

- `aggressor_side`: a recorded trade side A is a sell aggressor and B a buy aggressor
- `latency`: 50 ms place and cancel latency is assumed; production delivery and order latency are not measured
- `liquidation`: no liquidation price is modeled (paper venue reports 0 = unknown); free margin falls back to book cash
- `maker_fee_bps`: 1.5 bps maker rate is the unverified scheduler/fees.go constant (issue 1726 measured no maker fill); no rebate is assumed
- `min_order_value_usd`: USD 10 minimum for risk-increasing quotes is an unverified venue rule; reduce-only orders below it are assumed accepted
- `slippage_reserve_bps`: 5 bps reserve equals the paper SlippagePct constant; it is a reserve, not a measured cost
- `stop_reference`: stops trigger on recorded trades and fill against recorded depth less the slippage reserve
- `taker_fee_bps`: 4.5 bps taker rate is the scheduler/fees.go constant, reported as a conservative upper bound by the private issue 1726 evidence

## Results

No variant was scored: no eligible input. The executable study, the integration evidence and this report are delivered; no synthetic profitability evidence is reported.

## Source pins

- `backtest/book_replay.py` `df45592108ab14a4f2332aadca1d328f06438025556a5465c13b69bfbacb8a8d`
- `backtest/candidates/inventory_market_making_1636/protocol-lock.json` `e131dbb8b8516cf6dd829147e8501aaa0efe2a7fde902702df6521d69d7f1e37`
- `backtest/candidates/inventory_market_making_1636/run_study.py` `5f1e95428c41f13efdc4166cb152df6722c909fe0535570775627f9fbfcd0b5f`
- `backtest/quote_replay.py` `a4199b77d7cb1c60c321d40f9431bd166b7499652e59c0f82251d5a4800d2972`
- `backtest/trade_flow_replay.py` `badd02d175995110d694845e45ce58611df002b80f3660f5ad62b24a61dc79b4`
- `scheduler/quote_coordinator.go` `8a45fcfe6e60a14109d234e846d3bdd25e3d4f2f7c283a7f5c779f30a7072c96`
- `scheduler/quote_paper_venue.go` `182a279a4a77c9fda0c4ec6e2d828f0c9bbcbea79b0ab18913632043d92df047`
- `scheduler/quote_policy.go` `d1207988915684b02b094010f3cefb8560242c48b9ef49bad952826a3c036ce7`
- `scheduler/quote_policy_spec.go` `52554e741ad1c7c077420865b3b7874af9c1f5d65f883ec14edf1fd368a09346`
- `scheduler/quote_replay.go` `bff54ef22b956bf1063fcb00c88b41f2ad92916ab0c9b080dc8c2138b50c63e6`
- `scheduler/quote_settlement.go` `eb89fd28188da774f278749780cd4285e25a95ae2529eccf69ff9a845f57cac9`
- `shared_strategies/open/inventory_market_making.py` `53863b4a74ee0be5b3469a9264baed5ecf0eba5ac5f34033d41e073a5f38ddf5`

## Reproduce

```bash
BIN="$(mktemp -d)/go-trader"
go -C scheduler build -o "$BIN" .
uv run --no-sync python backtest/candidates/inventory_market_making_1636/run_study.py \
    --protocol backtest/candidates/inventory_market_making_1636/protocol-lock.json \
    --binary "$BIN" --out-dir "$(mktemp -d)"
```

# cross_exchange_perp_arbitrage: registration record (#1878)

**Status: UNVALIDATED.** This file is a registration record. It is not a study report.

Issue #1878 registers the research pair policy `cross_exchange_perp_arbitrage` (#1875) in the canonical open registry with `edge_status="no_edge"`, `edge_source="unvalidated"` and `production_refused=True`.
This change supplies no study result, no performance measurement, no baseline comparison, no verdict and no promotion.
No backtest, replay or paper run of the policy was scored for this record.

## What the registration states

- Name: `cross_exchange_perp_arbitrage`; contract `{"input": "pair_policy_request_v2", "output": "pair_policy_result_v2"}`; platforms `("futures",)`; `short_entries=False`.
- Owner: `shared_strategies/open/pair_registry.py` holds the only pair descriptor; `shared_strategies/open/registry.py` projects it into `STRATEGIES` through `register(pair=...)`.
- Parameters: `adverse_band_bps`, `entry_threshold_bps`, `exit_threshold_bps` and `funding_allowance_bps` are all required and have no defaults; the policy and the Go pair admission validate them.
- Every production configuration, mode and `allow_no_edge` acknowledgement refuses the name; single-symbol registries, scalar checks, candle backtests, the optimizer and parity runs refuse it.
  Only the internally admitted research pair coordinator runs it, through `shared_scripts/run_pair_policy.py`.

## Evidence source

`edge_source="unvalidated"` means that no evidence of edge exists.
A later change of `edge_source` needs the frozen study lock and the final-tree rerun that #1883 describes; until then this record stays unvalidated.

## Discovery captures

The raw `--list-json` outputs of both single-symbol shims were captured on the build base before the registration (`discovery_spot_before.json`, `discovery_futures_before.json`) and on the final tree after it (`discovery_spot_after.json`, `discovery_futures_after.json`).

- Base commit: `f576802c44e298c1c544db7b0dd5831a70672ddc`.
- Commands: `uv run --no-sync python shared_strategies/open/spot/strategies.py --list-json` and `uv run --no-sync python shared_strategies/open/futures/strategies.py --list-json`.
- Comparison: `cmp` reports each after capture byte-identical to its before capture (spot 2716 bytes, futures 3227 bytes); the pair adds no discovery entry.

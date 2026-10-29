# cross_exchange_perp_arbitrage signal specification

Frozen from `shared_strategies/open/cross_exchange_perp_arbitrage.py`. The study does not reimplement these formulas. Policy diagnostics are decision evidence. Booked adapter exports are the only measured returns.

## Contract

- Input `pair_policy_request_v2`. Output `pair_policy_result_v2`.
- Parameters, all required and with no defaults: `adverse_band_bps`, `entry_threshold_bps`, `exit_threshold_bps`, `funding_allowance_bps`.
- `exit_threshold_bps` is at least 0 and strictly below `entry_threshold_bps`. `adverse_band_bps` is at least 0 and below 10000. `funding_allowance_bps` is at least 0. `entry_threshold_bps` is at most 10000.
- A flat pair is an entry. A matched pair is an exit. Any other lifecycle holds. The policy does not force a transition.

## Units

- Base quantity is the member's base asset. Native quantity is base quantity divided by `contract_size`.
- Prices are native venue prices. USD values multiply by the saved conversion.
- Spreads and thresholds are in basis points. One basis point is 1/10000.
- Costs are the member taker fee and the adverse-band reserve. Funding reserve on entry is `buy_cost_usd * funding_allowance_bps / 10000`.

## Orientations

Both orientations are searched on a flat pair:

- `long_hyperliquid_short_okx`: buy Hyperliquid, sell OKX.
- `long_okx_short_hyperliquid`: buy OKX, sell Hyperliquid.

The selected orientation is the one with the largest legal base quantity. Equal quantities break ties by member key order.

## Entry

Entry walks saved asks on the buy leg and saved bids on the sell leg. Capture is sell proceeds minus buy cost, entry fees, an equal exit-fee reserve, the adverse-band reserve, and the funding reserve.

The entry comparison is strict. Capture must be above zero, and `10000 * capture` must be above `entry_threshold_bps * buy_cost`. Equality holds.

The selected quantity is the largest common lot that also passes, strictly:

- both lot grids and both minimum base quantities
- both minimum notionals
- depth at the rounded bound (`reach`)
- gross budget, each venue cap, free collateral, and maintenance equity

Price bounds are the worst consumed level widened by the adverse band, then rounded to the venue tick. A bound that rounds off the worst consumed level fails `reach` and holds. The coordinator remains the owner of rounding, caps, and order placement.

## Exit

Exit sells the long leg and buys the short leg for the booked base quantities. The remaining spread is `10000 * (cover_unit - sale_unit) / sale_unit`.

The exit comparison is inclusive of the threshold. A remaining spread above `exit_threshold_bps` holds. A remaining spread at or below that threshold exits, with zero targets and the rounded closing bounds.

Closing depth that cannot cover the booked quantity holds. A rounded closing bound that cannot reach the saved depth holds. Protection and recovery stay with the coordinator.

## What this file does not score

A diagnostic from this policy is not a booked return. A missing book, fee, funding observation, conversion, or account is not filled with a current price.

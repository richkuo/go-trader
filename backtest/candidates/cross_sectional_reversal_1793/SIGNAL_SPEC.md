# Cross-sectional reversal: frozen signal specification (issue 1793)

Status: `edge_status="no_edge"`, `edge_source="unvalidated"`. This file is the registry `edge_ref` for `cross_sectional_reversal`.
It freezes the signal definitions before any study evaluation. The study protocol and its results belong to issue 1790 and go in this directory; they do not change the definitions below.
The definitions are those of the merged `cross_sectional_momentum` specification (`backtest/candidates/cross_sectional_momentum_1787/SIGNAL_SPEC.md`) with one change: the side assignment of the two tails is reversed. Both evaluators run the same shared code in `shared_strategies/open/basket_allocation.py`.
The default values are research seeds. This file endorses no trading setting and is not evidence of a Hyperliquid edge.
The candidate research reference in issue 1652 is a research lead only; its contents were not verified, and it is not evidence of a Hyperliquid edge.

## Scope

- A basket evaluator (`basket=BASKET_CONTRACT`, platforms `("futures",)`, `short_entries=False`). Short targets come from negative weights.
- It runs only through the basket coordinator, only in explicit paper mode, and live basket dispatch stays refused.
- It is absent from every single-symbol registry; its single-symbol entry refuses one-symbol evaluation.

## Inputs

- `frames`: the closed-bar frames of the eligible members only, as selected by `shared_scripts/run_basket_ranker.py` (at most `allocation_context.lookback_bars` closed bars, ending at the bar that closes at the rebalance deadline). Listing-excluded members have no frame and appear in neither output map.
- `allocation_context`: a read-only copy of the saved basket contract for that basket and deadline: `gross_budget_usd`, `max_coin_notional_usd`, `max_transition_net_usd`, `lookback_bars`, `min_history_bars`.
- Parameters (effective = registry defaults overlaid by the basket `params`):

| Parameter | Type | Range | Seed |
| --- | --- | --- | --- |
| `return_lookback_bars` (R) | integer, not boolean | [1, 4999] | 24 |
| `volatility_lookback_bars` (V) | integer, not boolean | [2, 4999] | 24 |
| `quantile` (q) | finite number, not boolean | 0 < q < 0.5 (both bounds exclusive) | 0.25 |

## Warmup and preflight

- Required history per member: `max(R, V) + 1` closed bars (R intervals need R + 1 closes; V one-bar returns need V + 1 closes).
- Configuration preflight (`basket_preflight`, before any member frame is read): `allocation_context.lookback_bars >= max(R, V) + 1`, else the request is invalid (exit 1). A missing, short or stale frame never turns this configuration error into a data hold.
- At evaluation: an eligible member whose frame carries fewer than `max(R, V) + 1` closed bars holds the whole target set. This happens only when `min_history_bars` is below that need and the member was listed recently.

## Signal

For each eligible member with closes `c[0..n-1]` (oldest first, `c[n-1]` is the close of the deadline bar):

- Score: the trailing simple close return over R intervals, `c[n-1] / c[n-1-R] - 1`, in decimal return units.
- Volatility: the population standard deviation (divide by V) of the last V one-bar log returns `ln(c[i]) - ln(c[i-1])` for `i = n-V .. n-1`, with no annualization. The mean and the squared deviations are summed with exact floating-point summation (`math.fsum`).
- Every open, high, low and close of every input bar must be finite and positive.

## Ranking and tails

- N is the eligible population. Members rank by descending score, then ascending symbol (stable tie rule).
- Tail size: `k = max(1, floor(q * N))`, where `q * N` is computed exactly from the shortest decimal representation of q (Python `repr`). Two disjoint tails need `N >= 2` and `2k <= N`; otherwise the whole set holds.
- The first k members (highest trailing returns) are the short tail; the last k members (lowest trailing returns) are the long tail. Every other eligible member gets a finite score and weight 0.
- The side assignment is a selection of the two tails that the momentum ranking already returns. Scores are never negated and the sort is unchanged, so tail membership at a tie boundary is the same set of members as in momentum, with the sides swapped.
- Scores stay in trailing-return units: a long member has one of the lowest scores, a short member one of the highest.

## Weights

- Each side has a nominal allocation of one half of the gross budget: long weights sum to +0.5, short weights to -0.5.
- Inside a side, raw weights are inverse volatility, computed as the ratio `min_side_vol / vol` (in (0, 1], no overflow) and normalized to 0.5.
- Cap fraction: `cap = max_coin_notional_usd / gross_budget_usd`. A side with `k * cap < 0.5 - 1e-9` is infeasible and holds the whole set.
- Capped allocation (water filling): cap every member whose proportional share exceeds `cap` (strictly greater) at `cap`, redistribute the excess among the uncapped members of the same side in proportion to their ratios, and repeat until no share exceeds `cap`.
- Emission grid: every emitted weight is an integer multiple of 2^-52. Units are `floor(w * 2^52)` (toward zero), each at most `floor(cap * 2^52)`. A side tops up to exactly 2^51 units (0.5) by adding one unit to members in order of descending fractional remainder, then ascending symbol, and then fills any remainder in the same order up to each member's cap. Both sides then trim to the smaller side total, removing units from the member with the most units first (ties by ascending symbol).
- Side tolerance: an absolute 1e-9 of the gross budget. If the common side total falls short of 0.5 by more than that, the whole set holds.
- On this grid every partial sum the coordinator forms is exact in float64, in any summation order. Both sides have equal totals, so the target net weight is exactly 0, and the absolute weight sum is at most 1.
- Before output, the evaluator applies the coordinator's own checks with its tolerance: per coin `|w| * gross > cap_usd * (1 + 1e-9)`, absolute sum `> 1 + 1e-9`, and target net `|net| * gross > max_transition_net_usd * (1 + 1e-9)`. Any failure holds the whole set with a reason; the checks are never relaxed.

## Holds

The evaluator returns `{"hold": "<reason>"}` with no scores, weights or exclusions when any eligible member (selected or middle) has:

- fewer closed bars than `max(R, V) + 1`;
- a non-finite or non-positive price;
- a non-finite trailing return;
- zero or non-finite volatility;

or when either tail is empty, a side is infeasible under the cap, or a coordinator bound check fails. A bad frame never becomes an exclusion or a smaller-universe rebalance; the evaluator adds no exclusions.

## Lifecycle notes

- The holding horizon is the existing basket lifecycle field (`holding_horizon_seconds`), outside the ranker parameters. Issue 1790 freezes its study grid values.

- Allocation ends before the coordinator freezes targets. A failed, rejected, partial or unknown order keeps its saved target and never transfers its allocation to another member.
- Exits come from membership change through the coordinator and from each member's own close and stop owners.
- Dollar neutrality does not establish beta neutrality.

---
Created with LLM: Opus 5.5 | high | Harness: Claude Code

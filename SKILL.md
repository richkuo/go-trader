# Agent Setup Guide — go-trader

Repository: `https://github.com/richkuo/go-trader.git`

Operator runbook for agents that install, configure, run, update, and operate go-trader. Reader-facing overview: [README.md](README.md). Coding constraints and PR conventions: [CLAUDE.md](CLAUDE.md) and [AGENTS.md](AGENTS.md).

Quick flow for a new server: tell OpenClaw `install https://github.com/richkuo/go-trader and init`.

---

## Core Rules

- Run git from the repo root.
- Use `/opt/homebrew/bin/go` (macOS) or `/usr/local/go/bin/go` (Linux) if `go` is not on PATH.
- Use `uv run --no-sync python` for dev, backtest, and manual CLI work. The scheduler calls `.venv/bin/python3` directly, so no PATH configuration is needed for the service.
- Install Python dependencies with `uv sync --no-dev` on a service host and `uv sync` in a development checkout or worktree.
- Scheduler config: `scheduler/config.json` (start from `scheduler/config.example.json`). On deployments the real file lives outside the deploy tree at `/var/lib/go-trader[/<instance>]/config.json`.
- State is SQLite only: default `scheduler/state.db`. Optional root `paper_db_file` moves the paper scope into a second file (§ Storage Ownership).
- Never store secrets in config files. Put Discord and exchange credentials in systemd environment variables (a Docker deployment uses `docker/go-trader.env`, § Docker Container).
- Prefer `./go-trader init` for humans, `./go-trader init --json … --output scheduler/config.json` for agents and scripts.
- TradingView export: ask which strategy IDs (or all) before running.
- **CRITICAL: always update a host deployment with `scripts/update.sh`. Never run `git pull` + `go build` by hand.** The Go binary and the Python check scripts share an argv contract, so a build at a different commit than the scripts is an asymmetric deploy. A Docker deployment updates only by pulling a release image, which holds both from one commit (§ Docker Container).

---

## Prerequisites

```bash
python3 --version
uv --version 2>/dev/null || /usr/local/bin/uv --version 2>/dev/null || /opt/homebrew/bin/uv --version 2>/dev/null || echo "NOT_INSTALLED"
go version 2>/dev/null || /usr/local/go/bin/go version 2>/dev/null || /opt/homebrew/bin/go version 2>/dev/null || echo "NOT_INSTALLED"
git --version
rsync --version
```

Requirements: Python 3.12+, `uv`, Go 1.26.2, Git, `rsync`.

```bash
# Linux: system-wide, so root and the service account both find them
curl -LsSf https://astral.sh/uv/install.sh | sudo env UV_INSTALL_DIR=/usr/local/bin UV_NO_MODIFY_PATH=1 sh
curl -sL https://go.dev/dl/go1.26.2.linux-amd64.tar.gz | sudo tar -C /usr/local -xzf -
# macOS
brew install uv go@1.26
```

Install `uv` and Go where every account can run them. `scripts/update.sh` looks for `uv` on `PATH`, then at `/usr/local/bin/uv`, `/usr/bin/uv` and `/opt/homebrew/bin/uv`, and for Go on `PATH`, then at `/opt/homebrew/bin/go` and `/usr/local/go/bin/go`. It never looks in a home directory. The per-user installer puts `uv` in `~/.local/bin`, which only that account can use: a copy in `/root/.local/bin` is not visible under `sudo`, and the `go-trader` service account cannot run it. `scripts/shared-feed-convert.sh plan` and `scripts/migrate-service-layout.py plan` check that both tools run as root and as the service account, and refuse with this fix before any build.

---

## Install

```bash
git clone https://github.com/richkuo/go-trader.git
cd go-trader
uv sync --no-dev
```

This installs a service host. In a development checkout or worktree, run `uv sync`, which also installs the test tools (`pytest`, `pytest-mock`, `pytest-xdist`) from the `dev` dependency group.

If the repo already exists, ask whether to reconfigure, update, or do a fresh install before changing it.

Build:

```bash
VER=$(git describe --tags --always --dirty 2>/dev/null || echo dev)
/opt/homebrew/bin/go -C scheduler build -ldflags "-X main.Version=$VER" -o ../go-trader .
./go-trader --help
```

The `Version` ldflag appears in Discord summary titles; without it the binary reports `dev`. After the initial install, use `scripts/update.sh` for every rebuild.

---

## Configure

```bash
./go-trader init                                                    # human flow
./go-trader init --json '{"assets":["BTC","ETH"],"enableSpot":true,"spotStrategies":["momentum","rsi"],"spotCapital":1000,"spotDrawdown":60}' --output scheduler/config.json
```

The wizard covers assets, strategy groups, paper/live mode, per-strategy capital, live risk settings, Discord channels, and auto-update mode. It prompts before overwriting.

Config skeleton and the full field list: [README.md](README.md) § Configuration Reference. Rules that govern a hand-written config:

- Strategy entries need `id`, `type`, `script`, `args`, `capital`, `max_drawdown_pct`, `interval_seconds`.
- `open_strategy` and `close_strategy` are objects of shape `{"name": "<id>", "params": {…}}`. Per-evaluator params live on the close ref, never on the strategy. A legacy `close_strategies` array of length ≤1 is still read; length >1 is rejected at load.
- Tier lists use `tp_tiers`; each tier is `{"atr_multiple"|"profit_pct": N, "close_fraction": 0..1, "sl_after"?: {…}}`. Per-tier legacy `atr` / `multiple` / `fraction` aliases still parse. Write the canonical names.
- `discord.channels` / `telegram.channels` keys: `spot`, `options`, `hyperliquid`, `topstep`, `robinhood`, `okx`, `luno`, plus optional paper keys such as `okx-paper`. A non-empty `<platform>-paper` key also splits cycle summaries, leaderboards and Sharpe groups by mode; with no such key the grouping stays merged.
- `summary_frequency` uses the same key scheme. Values: `hourly`, `daily`, `every`, `per_check`, `always`, or a Go duration (`30m`, `2h`). The wall-clock cadence persists in SQLite and survives restart and SIGHUP. `options`, `perps`, `futures`, and `manual` post every channel run; `spot` posts hourly. A trade always forces an immediate post.
- `discord.owner_id` comes from `DISCORD_OWNER_ID`; it enables DM upgrade and migration prompts.
- **Separate live and paper state files.** Set root `paper_db_file` beside `db_file` and the paper scope's books, risk row, kill-switch events and correlation snapshot move to that file; the live scope, process metadata, the live-only wallet and cash-flow tables and shared regime history stay in `db_file`. Omit `paper_db_file` and the single-file layout is unchanged. The two paths must resolve to different physical files — relative paths, symbolic links and hard links are all checked — or startup refuses with exit code 80.
- **Stored identity.** Optional per-strategy `storage_strategy_id` names the row a strategy owns inside its file; it defaults to `id`. Storage identity is `(scope, storage_strategy_id)` and must be unique **within one file**; the same value in the live and paper files is the supported alias. Rename a strategy's `id` and set `storage_strategy_id` to the old name to keep its cash, positions, latches, queued actions and history. Both keys are restart-required.

Live-mode risk defaults offered by `init`: per-strategy spot drawdown 5%, per-strategy options drawdown 10%, portfolio kill switch 25%, portfolio warn threshold 60% of the kill switch.

**Unified per-regime close block** (`tiered_tp_atr_regime` / `tiered_tp_atr_live_regime` / `tiered_tp_atr_live_regime_dynamic`): instead of a tier-keyed list, the close ref carries a top-level `trend_regime` where each label owns its stop loss and tier ladder:

```json
{"name": "tiered_tp_atr_live_regime", "params": {"trend_regime": {
  "trending_up": {"stop_loss_atr": 1.5, "tp_tiers": [
    {"atr_multiple": 2.0, "close_fraction": 0.5, "sl_after": {"kind": "trail_from_here", "tp_atr_fraction": 0.5}},
    {"atr_multiple": 4.0, "close_fraction": 1.0}]},
  "ranging": {"stop_loss_atr": 0.8, "tp_tiers": [
    {"atr_multiple": 1.0, "close_fraction": 1.0}]}
}}}
```

All regime labels must be present (exhaustive, no fallback). Tier counts may differ per label. The block **owns the stop loss** through per-regime `stop_loss_atr`, so declaring any strategy-level stop field alongside it is rejected at load. The whole block is hot-reload-gated as a unit: change it while a position is open and the reload is refused. Flatten first.

**Dynamic variant** (`tiered_tp_atr_live_regime_dynamic`, HL perps and `type=manual` only): the same block, plus an optional top-level `regime_confirm_cycles`. It is the number of consecutive cycles a new ATR-regime label must hold before the position applies it. On a live position, the protection sync then re-places the stop-loss and take-profit orders for the new label. A paper position moves its stop as **Paper stops** describes. The default is `2`. The value must be a whole JSON number >= 1, and `1` applies a change on the first cycle that shows it. A string, a boolean, `null`, `0`, a negative value, a fraction, or a value past the integer range fails to load. The key is part of the hot-reload-gated block. It is an unknown param on the other two unified closes. The backtester rejects this close.

**Trailing-ratchet close** (`trailing_tp_ratchet` / `trailing_tp_ratchet_regime`): a trailing-ATR stop where each cleared take-profit tier tightens the trail and optionally scales out.
The scalar form needs a positive strategy-level `trailing_stop_atr_mult` (the initial loose trail, and the sole stop owner).
The regime form needs `trailing_stop_atr_mult_regime` instead.
`tp_tiers` is a list (scalar form) or `{label: [tiers]}` (regime form, frozen at open).
Each tier is `{atr_multiple, close_fraction?, trailing_mult_after | tp_atr_fraction}`: `close_fraction` (default `0`, cumulative) scales out, `0` means trail-only; the trail tightens to `trailing_mult_after` (absolute ATR multiple) **or** `tp_atr_fraction × atr_multiple` (relative), never both, and never loosens.
The first rung must be ≤ the initial trail.
It places **no on-chain take-profit**: partial closes ride the close evaluator, the on-chain stop rides the trailing-stop walker.
Tier triggers use entry ATR.
Scope: Hyperliquid perps and `type=manual`.
Backtestable.
In live mode only (paper places no venue order and must match the backtester), the Hyperliquid check script rewrites a partial close whose lot-floored quantity is zero or whose value at the check-time price is below the venue minimum order value plus a 3% safety margin (the execute-side market close submits at 1% limit slippage and the price can drift before submission) to a noop (`close_gate: below_venue_minimum`); a full close is never rewritten (`current_close_fraction` snaps a final tier within `1e-9` of the whole remainder to exactly `1.0`), and the gate is skipped when the lot size cannot be resolved (adapter meta, else the `/tmp/hl_meta.json` cache under `market_feed=websocket`, never a fetch).
Paper Hyperliquid perps (#1716) book synthetic entries, flips, scale-in adds and partial closes (signal partials and paper take-profit tier fills) at venue lot sizes (`scheduler/hyperliquid_paper_lot.go`).
The scheduler reads `universe[].szDecimals` for the exact coin from the public `meta` request through `hlPostInfoTo` (`scheduler/hyperliquid_lot_metadata.go`), at the endpoint the adapter uses (`HYPERLIQUID_TESTNET=1` selects testnet).
The refresh runs outside `mu`, before the execute lock (and in the paper no-signal upkeep of a strategy with `sl_after` rules), and only one refresh per endpoint runs at a time.
It is charged to the websocket feed ledger when one exists, else to a local enforced ledger of 4 requests a minute, with reason `lot_metadata`.
A snapshot is refreshed after 1 hour and expires after 6 hours; a failed refresh retries after 30 seconds, doubling up to 10 minutes; a response above 8 MiB is refused.
A valid `szDecimals` of 0 is a lot size; an absent, null, non-integer, negative, above-12 or duplicated entry is refused for that coin.
Each quantity is floored the way `adapter.floor_lot_size` floors it (`hlFloorLotSize`: the shortest decimal string plus 1e-9 lot of slack, Python's 28-digit decimal context).
An entry or add holds when its floored quantity is 0 or its floored value at the booked price is below $10.30 ($10 plus the 3% margin).
A partial close holds on the same test at the decision price (the cycle mid), and a value equal to $10.30 passes, as in the live gate; a tier fill still books at the tier price.
A held order writes nothing: no cash, fee, trade, risk result, tier consumption, position stamp or alert, and a held partial close never opens the other side.
When the lot size is unknown (no snapshot, expired, coin absent or entry refused) these orders hold, and one `[hl-lot]` line is logged per endpoint and coin until the lot size returns, which logs one `restored` line.
Full signal closes, full stops and paper kill-switch and circuit-breaker flattening still close the whole book, also for unrounded legacy positions and during an outage.
A flip books its full close and holds only the new open when that open fails the test, the same as an open refused for cash.
A cycle whose partial close holds (for example a sub-lot leftover that the close evaluator asks for again after a floored tier) runs the paper no-signal upkeep (`runPaperHLQuietCycleMaintenance`: dynamic-regime stop re-arm and breach, then the `sl_after` move), as live does when its close gate rewrites the same close to a noop.
A paper tier counts as cleared, for `sl_after` and for unified take-profit consumption, when the closed ratio reaches the threshold within `1e-9` or when the quantity still missing to the threshold floors to zero lots at the coin's lot size (`paperTierClearedByLot`); a close more than one lot short does not clear it, and replay-mirror strategies keep the exact ratio test.
Confirmed live fills and replay-mirror rows keep their booked quantities.
Remaining differences, pinned by `TestHLPaperLotParityWithLiveGateAndBacktester` (`-tags pyintegration`): live opens round to the nearest lot, the execution-spec backtester reserves the taker fee before it floors an entry and rejects scale-in, no path gates a full close, and the paper tier price can differ from the decision price.
A **full close on a shared coin** (two or more live HL perps strategies on the coin) is sent as a sized close to protect peer exposure.
Every sized execute-lane close (a partial close, a shared-coin full close, a legacy reverse-signal close with close fraction 0, a `type=manual` cycle close and an operator `manual-close`) takes its size and order type from `planHLCloseOrder` (`scheduler/hyperliquid_close_plan.go`).
The key is the strategy's side and book, the peers' books on the coin split by side (hedge legs and `type=manual` peers included, `hlPeerBooksOnCoin`) and the signed on-chain net `S`.
`target` is the strategy's remainder plus same-side peer books minus opposite-side peer books; the size is `min(close, distance from S to target in the closing direction)`; when that distance is zero or less nothing is sent and a throttled CRITICAL alert fires (the reconciler owns the book).
The order is `reduce_only` when `S` is on the strategy's side and `target` is zero or on that side, else `cross` (a normal IOC that moves the net onto the opposite-side peers' books), and `resolveHLCloseOrder` re-plans every sized close on a fresh `clearinghouseState` refetch taken just before the order, after every earlier strategy in the cycle has booked, so a peer that opened or closed earlier in the same cycle is seen (the cycle-start snapshot never caps or skips a close).
When that refetch fails, only a plan that is an uncapped `reduce_only` close at the book size is still sent (it cannot cross zero, and with the cycle-start account unreadable that is the plan when no opposite-side peer holds a book); a plan that would cap, skip or cross is deferred, with no order sent and no protection cancelled.
So the chain always ends between `S` and `target`, and a close never opens exposure past what the books state.
A full-intent sized close that fills short of the book (more than `0.0001` below it) books only the filled quantity (`HyperliquidResult.SizedCloseBookedQty`), keeps the remainder on the book with the protection ids that execute cancelled cleared, and re-arms the remainder's stop in the same cycle (`rearmAfterSizedClose`) in place of the post-trade protection sync; the whole book is deleted only when the fill covers it.
The `type=manual` cycle close books the same way through `bookManualCycleClose` inside `settleManualCycleClose`: on a short fill it clears the cancelled protection ids from the book (a cancel the venue reported as failed keeps its id), queues the close row, and re-arms the remainder's stop in the same cycle, because the book is reduced only when the next cycle drains the row.
A rejected close (execute lane or `type=manual` cycle) re-arms the whole book the same way when its outcome is unknown or a requested cancel succeeded (`hlUnfilledCloseNeedsRearm`); a close the script never sent (`order_outcome: not_sent`) cancelled nothing and re-arms nothing, and a rejected `type=manual` cycle close sends the throttled live-exec failure alert. **Stop and take-profit size (`hlOwnStopShare`, the only sizer):** every live Hyperliquid stop and take-profit, including the close re-arm, uses `Q` from `hlOwnStopShare`.
`Q` is at most the book and at most the own-side chain units.
On a drift, resting stops share the backed units first and unarmed books take what is left, so the side's stops cover the chain.
The close re-arm calls it with the remainder as the book and its own stop not resting; an empty peer list counts as one armed peer book, which keeps the #1577 decision table.
The Hyperliquid reconciler runs every cycle with a fetched account (its shared-coin pass covers every live book), so the share never counts a peer fill that is not booked yet.
A book alone on its coin is booked only by its own due reconcile, so its share CRITICAL fires only in a cycle where that reconcile ran (`hlCycleShare.markReconciled`); before that it logs one line.
A pre-loop resize pass then grows or shrinks a resting stop that is outside one lot of the lot-floored `Q`, and flags resting take-profit tiers above `Q`; the pre-loop re-arm pass (`runHyperliquidShareRearm`) runs the protection sync for each flagged strategy, so the tiers shrink in the same cycle.
A zero `Q` that only opposite-side books explain (no drift) keeps the resting stop and tiers: they are reduce-only and protect the book again when the netting book shrinks.
A zero `Q` from drift cancels them; a cancelled `type=manual` recorded stop sends one CRITICAL with its trigger, because no config owner can re-place it, when a peer is on the coin or its own reconcile ran this cycle; a book alone on its coin before its reconcile logs one line.
An unknown or stale account view places at the book and never shrinks or cancels.
A book above what the chain and the opposite-side books explain (drift) sends one CRITICAL per change of `Q`; a book that only an opposite-side book nets logs one line.
The re-arm pass also arms, once a fresh `Q` is above zero, a book with no stop and no recorded trigger: the trailing arm for a trail owner, the protection sync for an ATR or tiered owner.
The re-arm pass syncs in `hlProtectionGuardFullHoldRegime`, which plans from the applied regime label and never advances the dynamic-regime confirmation count (only due evaluations count).
Static-scalar owners are re-armed by the liquidation audit.
An immediate stop fill books the lot-floored size the script reports (`stop_loss_size`), never more than was sent.
The liquidation audit places and tightens at `Q`.
The sizer reads the account after the close first on every lane and outcome (`fresh`).
When that read fails, a known outcome uses the pre-send reading less the confirmed fill (`derived`), an unknown outcome uses the pre-send reading with no fill subtracted (`stale`, an upper bound), and no reading at all uses the remainder (`none`).
The pre-send reading is the refetch the plan used, else the cycle-start snapshot (the refetch-failed uncapped close and whole mode).
A close outcome is known only when `check_hyperliquid.py execute` reports `order_outcome` as `filled`, `rejected` or `not_sent` (`hlExecuteFillOutcome`); a missing field, `unknown` (the catch-all) or no JSON is unknown.
`Q` at zero on any basis but `none` places no stop.
Every arm consumes `Q` and never resizes it, and the re-arm calls `hlOwnStopShare`.
The re-arm covers every stop owner of the strategy: the protection sync for an ATR stop (in `hlProtectionGuardStopLegAfterFailedClose` mode, so the queued row keeps the take-profit ids, and with the take-profit tiers sized the same), the trailing arm for a trail or ratchet stop, the percentage arm, and, for a `type=manual` strategy with none of these owners (`manualRecordedStopOwner`, for example a stop set by `manual-open --stop-loss-pct`), the recorded pre-close trigger.
Each arm hands the old order id to the placement so it is verified on-chain first, and after a fill the protection sync force-replaces a pre-close stop whose cancel failed. **Unconfirmed pre-close orders:** the unconfirmed set is every order id the close asked to cancel that the venue did not confirm cancelled, which is the requested ids the book still holds after the confirmed ones are cleared (`hlCloseUnconfirmedSet`).
Every member is verified by id on the chain and then removed, resized or reported; nothing is cancelled blind.
With `Q > 0` the stop in the set is resized through the arm's verify-first cancel id, and after a fill or a resize each take-profit in the set is force-replaced at the new size (`ForceTPReplace`), or, when a queued row owns the tiers, cancelled verify-first through `cancel_tp_oids` so the next due sync re-places it after the drain.
With `Q = 0` nothing is placed and every member of the set is cancelled verify-first: the stop through the cancel-only mode of `check_hyperliquid.py --update-stop-loss` (`--size=0`: read the open orders, cancel the id when it is open, report an external fill, place nothing), the take-profits through the protection sync `cancel_tp_oids` (open means cancelled, not open means gone, filled is left to the reconciler, an unreadable open-order list is UNVERIFIED).
A removed id leaves the book like a confirmed cancel (the stop id and trigger are cleared, and a take-profit tier stays re-placeable), through the same book rule (`applyRearmedStopLossToBook`) for the CLI and the dashboard, which keeps a newer stop id the book gained before the write;
a failed or unverified id keeps its place in the book. **Alerts:** one CRITICAL owner alert per outcome (`formatCloseRearmReport`, sent by `notifyCloseRearm` to the channels and the owner DM, outside `mu`) that names the position and the next action: no stop because nothing backs the remainder;
a stop smaller than the remainder (it names the unbacked units);
a stop placed on a `derived`, `stale` or `none` basis (it names the basis and why the post-close read failed);
no trigger;
no arm owning the cancelled stop;
an unreadable open-order list;
a rejected cancel that leaves the pre-close stop resting at its pre-close size (a rejected force-replace cancel in the protection sync reports this through `cancel_stop_loss_error`);
a rejected placement;
a replacement that did not rest;
or an unknown outcome.
A stop update or protection sync that returns no result is reported as UNVERIFIED (a stop may rest untracked, or the pre-close stop may still rest; read the open orders before placing anything), never as no stop.
The `Q = 0` alert names each unconfirmed pre-close order as removed, already filled, STILL RESTING (with the manual cancel action: the Hyperliquid UI, or `go-trader manual-cancel-sl` for the stop of a `type=manual` strategy) or UNVERIFIED.
The same alert reports the take-profit leg as placed, kept, removed, failed or unknown, and a failed or unknown take-profit leg makes it CRITICAL; a tier whose placement the venue never resolved is reported unknown and never failed (even though the sync also lists an error for it), a sync with both an unknown and a failed tier names each state in that one alert, and the re-arm sends no separate unresolved-placement alert (the ordinary protection sync still sends it).
The arms' own alerts are suppressed on this path (the trailing update and the protection sync get no notifier).
The `SIZED CLOSE FILLED SHORT` alert is sent only after the re-arm result is known and states that result, CRITICAL when the result is; it never claims a stop that is not placed.
A re-armed stop that fills at once books only its own quantity, and a drained close row that empties the book closes the position, so the close is never booked twice.
Go passes `--size` plus `--close-mode=reduce_only|cross` (also in `executeProbeArgv`); `check_hyperliquid.py execute` floors the size with `adapter.floor_lot_size` (the only lot floor, shared with the venue close gate), reads the mid price (`sized_close_price`, mid plus or minus 1%) before any protection cancel, exits 1 with `order_outcome: not_sent` and cancels nothing when the size floors to zero or the mid cannot be read, and sends `market_close_sized` (`Exchange.order` IOC at that price with the strategy's own side).
Every execute payload carries `order_outcome`.
The sole-owner full close and the escalated `ForceFullClose` keep `--close-full-position` (`market_close(sz=None)`); opens and the direction=both flip keep `market_open`.
Venue behavior for a reduce-only IOC larger than the position is not confirmed by official docs (an unofficial source says the fill is clamped).
Because the cap uses the pre-send refetch, an oversize order is sent only when the chain shrinks after that read or the refetch failed and the uncapped book size went out.
A clamp gives a short fill, which the short-fill booking above records at the filled quantity without deleting the remainder.
A rejection is a failed execute: nothing is booked and the failed-close re-arm restores the stop.
When that remainder is worth less than the venue minimum plus the 3% margin, `applySharedCoinFullCloseFloor` (`scheduler/hyperliquid_shared_close_floor.go`) escalates to `market_close(sz=None)` only when every peer is provably flat: every peer's own virtual book on the coin is zero (the wallet reports one netted total per coin, so an opposite-side peer or a virtual-over-on-chain drift can make the net look like this strategy alone) AND the on-chain position (looked up by the raw venue coin name, e.g. `kPEPE`) is on the strategy's side and no larger than its virtual quantity, on BOTH the cycle-start snapshot AND a fresh `clearinghouseState` refetch taken immediately before the order (a peer that opened earlier in the same cycle is seen; a refetch failure defers).
It defers the close when the account state is not known, and otherwise sends ONE CRITICAL alert, rewrites the decision to `close_gate: shared_coin_stranded_remainder`, and persists `shared_close_hold_usd` plus `shared_close_hold_reason` (`peer_busy` or `venue_rejected`) on the position so the sized order is not resent.
The decision runs BEFORE the trailing-stop, fixed-ATR arm and protection-sync blocks, so a held or deferred close reaches them with `Signal == 0` and the stop keeps walking, arming and syncing exactly as a noop cycle would;
any live execute that was actually submitted to the venue AND asked it to cancel protection (`HyperliquidResult.LiveOrderSubmitted` and `LiveOrderCancelRequested`;
never a pre-submit skip such as `already long, skipping buy`, and never a partial close, which requests no cancel and leaves the resting stop untouched) and then failed after its cancel-then-close ordering removed (or may have removed) the on-chain stop, a `venue_rejected` rejection included, re-arms the stop in the same cycle, when its outcome is unknown or a requested cancel succeeded, through `rearmAfterSizedClose` (sized by `resolveHLCloseRemainderStop`, with one CRITICAL alert per outcome as described for the sized close above) into `rearmProtectionForCloseRemainder`, which covers all seven exclusive stop owners across three arms:
protection sync covers the fixed and regime ATR stop owners and the tiered-TP surface, `rearmScalarStopAfterFailedClose` covers the percentage owners `EffectiveStopLossPct` resolves (`stop_loss_pct`, `stop_loss_margin_pct` and the `max_drawdown_pct` fallback) by re-placing at `riskAnchorPrice() × EffectiveStopLossPct` through `hlLiquidationClampReplace`, the same liquidation-clamped update-script path the #1450 audit re-arms with, and only when `effectiveTrailingStopPct` resolves nothing so a trail owner is never double-armed, and `rearmTrailingStopAfterFailedClose` covers a trail-owned stop (`trailing_stop_atr_mult`, its regime form, and `trailing_tp_ratchet`) with the scale-in resize shape:
the persisted `StopLossHighWaterPx` anchors the trigger (never the mark), the pre-execute trigger is the floor the replacement may only tighten, the liquidation clamp applies, the arm places the size `resolveHLCloseRemainderStop` returns, and the pre-execute OID is handed to the update script as the cancel OID so it verifies the order on-chain first (an unreadable execute result therefore never clears the OID in the book;
a cancel that actually failed is cancelled there, and one that already landed places fresh).
A `peer_busy` hold still escalates once every peer is flat on-chain and in its book; a `venue_rejected` hold sends no order and no alert again, and a peer going flat does not resend it.
Both clear when the remainder is again above the gate or the escalated close fills.
Venue result for a whole-position reduce-only close below $10: the Hyperliquid API docs state only "Order must have minimum value of $10." with no exemption for reduce-only or full closes, and no testnet probe has been run from this repo, so the escalated close is treated as unconfirmed: a `minimum value` rejection of it strands the position with one alert and a `venue_rejected` hold instead of the throttled failure loop.
Because that wording is unconfirmed, the hold does not depend on it: any escalated whole-position close that fails stamps `escalate_failed` on the first failure (a transient failure is still retried the next cycle) and, when the next escalated attempt fails too, stamps the same `venue_rejected` hold with one alert whatever the rejection text.
`escalate_failed` is a retry marker only — a peer that becomes busy under it still takes the `peer_busy` hold and its alert.

---

## Secrets

Set in systemd overrides or exported environment variables before installation:

| Variable | Description |
| --- | --- |
| `DISCORD_BOT_TOKEN` | Discord bot token |
| `DISCORD_OWNER_ID` | Discord user ID for DM upgrades and migrations |
| `STATUS_AUTH_TOKEN` | Optional bearer token for `/status` |
| `ANTHROPIC_API_KEY` | Required only when a strategy opts into `llm_entry_analysis` |
| `GO_TRADER_GITHUB_TOKEN` | Token for `/go-trader-report-an-issue` (falls back to `GITHUB_TOKEN`) |
| `BINANCE_API_KEY`, `BINANCE_API_SECRET` | Binance live |
| `HYPERLIQUID_SECRET_KEY`, `HYPERLIQUID_ACCOUNT_ADDRESS` | Hyperliquid live; an API wallet key has a fixed lifetime (see Hyperliquid API wallet lifetime below) |
| `TOPSTEP_API_KEY`, `TOPSTEP_API_SECRET`, `TOPSTEP_ACCOUNT_ID` | TopStep live |
| `ROBINHOOD_USERNAME`, `ROBINHOOD_PASSWORD`, `ROBINHOOD_TOTP_SECRET` | Robinhood live crypto orders; when set, paper Robinhood checks and the options check also log in for authenticated option data (expiry, strikes, premiums, Greeks); options run as paper only and place no order; the login session is kept in `<config folder>/robinhood-session` (see Robinhood session below) |
| `OKX_API_KEY`, `OKX_API_SECRET`, `OKX_PASSPHRASE`, `OKX_SANDBOX` | OKX live/demo |
| `LUNO_API_KEY_ID`, `LUNO_API_KEY_SECRET` | Luno live |
| `GO_TRADER_ALLOW_MISSING_STATE` | `1` only for a genuine first-run live deployment |
| `GO_TRADER_MIGRATE_FIXTURE_HOLD_LATCH` | `1` only in the migrate layout fixture's latch scenario. A paper kill switch with no DM owner stays latched, and the daemon logs that this variable suppressed the auto-reset. Do not set it on a running service |
| `GO_TRADER_CASHFLOW_JOURNAL_ALARM` | `0`/`off`/`false`/`no` forces the legacy trade-ledger drift basis for HL shared wallets (default on) |
| `GO_TRADER_HL_BATCH` | `0`/`off`/`false`/`no` disables batched Hyperliquid signal checks |
| `GO_TRADER_PAPER_NICE` | Nice increment for the paper check children of a paper-only process (§ Paper CPU priority): unset = `10`, an integer `0`-`19` is used as is, `0` turns the feature off, any other value logs one WARN and uses `10` |
| `GO_TRADER_EGRESS_PROXY` | Egress proxy URL (`http`, `https`, `socks5`, `socks5h`; explicit port; optional credentials); a non-empty value turns on egress proxy mode (§ Egress Proxy Mode) |
| `GO_TRADER_EGRESS_DEPLOYMENT` | Egress deployment name; set by the egress drop-in or `docker/compose.egress.yaml`, not by hand |
| `GO_TRADER_NOTIFY_GITHUB_REPOS` | Notification broker only: comma-separated repositories it may file issues in (default `richkuo/go-trader`) |

### Hyperliquid API wallet lifetime

A Hyperliquid API wallet (agent wallet) has a fixed lifetime that is set when the operator creates it.
When it ends, Hyperliquid removes the agent, and every signed action fails with `User or API Wallet <address> does not exist.`: entries, closes, stop-loss placement and updates, and take-profits.
When at least one strategy is live Hyperliquid and `HYPERLIQUID_SECRET_KEY` is set, the daemon checks the key with `shared_scripts/fetch_hl_agent_status.py` (public info endpoint: `userRole` for the signer, `extraAgents` for its master).
The check runs once at startup, directly after the startup probe, and then every hour (sooner when `validUntil` is less than an hour away).
It is read-only and non-fatal: a venue read failure logs a WARN and never exits 78; 24 hours of failed checks send one `HL API WALLET CHECK FAILING` warning, repeated on the alert throttle interval.
Alerts go to the owner DM only (every channel only when no DM owner is configured): `HL API WALLET EXPIRES IN N DAYS` at 14, 7, 3 and 1 days before `validUntil` (each threshold once; a first reading inside a later threshold sends only that one; under 1 day the head reads `EXPIRES IN UNDER 1 DAY`), and `HL API WALLET CRITICAL` when the key is expired or unknown to Hyperliquid, or signs for an account other than `HYPERLIQUID_ACCOUNT_ADDRESS` (that CRITICAL names the account that receives the orders, because the venue applies a signed action to the signer's own account, and tells the operator to check it for positions).
When Hyperliquid lists no end date for a valid API wallet, one `HL API WALLET END DATE UNKNOWN` DM says that no expiry warning can be sent; it is sent again only after the state changes.
A signed order, stop or take-profit rejected with that venue text sends the same CRITICAL; many strategies failing together send one CRITICAL, repeated on the alert throttle interval, and a later good check sends one `HL API WALLET RECOVERED` line.
The venue names the address it recovered from the signature. When that address differs from the signer that the last check read, the cause is a bad signature or request, not the key: one `HL SIGNATURE MISMATCH` alert is sent (repeated on the alert throttle interval), with no replace-key fix, no CRITICAL state and no RECOVERED line. With no address in the text, or before any check has read the signer, the CRITICAL path runs.
The generic `LIVE ORDER FAILED` alert and every trading, booking and protection decision stay unchanged.
Replacement: create a new API wallet for the master account on Hyperliquid, set `HYPERLIQUID_SECRET_KEY` to its key (the account address stays the master), then restart go-trader.
The scheduler never creates, approves or rotates keys, and no alert or log line shows the key.

### Robinhood session

Every Robinhood login (signal check, execute, emergency close, position fetch, options check, chart and balance check) reuses one stored session in a folder that go-trader owns (#1818).
The folder is `<folder of the resolved --config file>/robinhood-session`: `/var/lib/go-trader/robinhood-session` for `go-trader.service`, `/var/lib/go-trader/<instance>/robinhood-session` for `go-trader@<instance>`, and `/data/robinhood-session` in the container image.
go-trader sets `GO_TRADER_ROBINHOOD_SESSION_DIR` to that folder at startup, directly after the config loads, and replaces any inherited value; the startup log shows `[robinhood] session folder: <path>` when a strategy uses Robinhood.
The Python scripts never use the home folder (`$HOME/.tokens`) and have no fallback: a missing, empty, relative, non-normalized, non-canonical, symlinked, foreign-owned or unwritable folder value refuses the login before any request, so live work fails with `Robinhood login failed:` and paper work continues logged out.
The folder is mode 0700 and every file in it is 0600, owned by the service user: `robinhood-<key>.pickle` (the session, which holds access and refresh tokens), `robinhood-<key>.device` (the device token), `robinhood-<key>.lock` and, after a device challenge, `robinhood-<key>.verification`; `<key>` is the first 16 hex characters of the SHA-256 of `go-trader/robinhood-username/v1:` plus the lowercased, trimmed username.
One lock per account serializes every login, and one watchdog bounds the lock wait and the login together (15 seconds, less for order scripts with a shorter budget, 600 seconds for the bootstrap); a waiter that runs out of time fails with `session lock busy` and sends no request.
robin_stocks writes into a private stage folder under the lock, and the stage replaces the stored session only after the login checks pass, so a failed login keeps the stored session and device token unchanged; the stored session is read with a reader that refuses any pickle global.
A scheduler login never answers a device verification prompt: it fails at once with an error that names the bootstrap, and it writes the verification latch, so later logins send no request until the bootstrap succeeds.
Deleting `robinhood-<key>.verification` allows one more automatic attempt.
Bootstrap on a host, as the service user, with the same sandbox as the unit: `sudo systemd-run --pty --collect --uid=go-trader --gid=go-trader -p WorkingDirectory=/opt/go-trader -p EnvironmentFile=/opt/go-trader/.env -p StateDirectory=go-trader -p ProtectSystem=strict -p PrivateTmp=true -p NoNewPrivileges=true -p "ReadWritePaths=/opt/go-trader/scheduler /opt/go-trader/logs" /opt/go-trader/go-trader robinhood-login --config /var/lib/go-trader/config.json`.
For `go-trader@<instance>`, use the instance service user, `/opt/go-trader-<instance>` for the working folder, `.env`, `scheduler` and `logs`, `StateDirectory=go-trader/<instance>` and `--config /var/lib/go-trader/<instance>/config.json`.
In the container image: `docker compose run --rm cli robinhood-login --config /data/config.json` (docs/DOCKER.md).
The bootstrap needs a terminal (`--pty`), runs the real robin_stocks verification flow (app approval, SMS or email code), and can run while the service runs: scheduler logins during it fail with `session lock busy` and retry next cycle.
Each instance keeps its own session, and a backup of the state folder or `/data` holds the session token, so protect it like the `.env` file.
After a service-layout migration the folder moves with the config, so the first login after it can need the bootstrap.

### Robinhood candles

Robinhood crypto candles come only from Yahoo through yfinance (`_get_yahoo_ohlcv` in `platforms/robinhood/adapter.py`), for the primary timeframe, the higher-timeframe (HTF) filter, the regime producer and the chart (#1822).
`ROBINHOOD_TIMEFRAMES` in the adapter is the only mapping from a go-trader timeframe to a Yahoo interval; Go `robinhoodTimeframes` (`scheduler/robinhood_timeframes.go`) mirrors its keys, Yahoo intervals and stock (session) intervals, Go `robinhoodYahooCryptoMap` mirrors `YAHOO_CRYPTO_MAP`, and `TestRobinhoodTimeframeTablesMatchThePythonAdapter` (`pyintegration`) fails on any drift.
The lookup is exact and case-sensitive with no trimming: `1M` is one month and `1m` is one minute, and any other name raises `unsupported Robinhood timeframe '<name>'` with the sorted list instead of returning an empty frame.

| go-trader timeframe | Yahoo request |
| --- | --- |
| `1m`, `2m`, `5m`, `15m`, `30m`, `90m`, `1d` | the same interval |
| `1h`, `60m` | `1h` |
| `1w` | `1wk` |
| `1M`, `1mo` | `1mo` |
| `2h`, `4h`, `6h`, `8h`, `12h` | `1h` rows resampled into UTC epoch buckets (crypto pairs only) |

- Resampling runs only for the crypto class (§ Robinhood asset classes); the stock class never resamples. A closed bucket is emitted only with every source hour present and finite; a partial leading bucket is dropped, an incomplete interior bucket is dropped and counted in one `dropped N incomplete <tf> buckets` stderr line, and the bucket that holds the forming hour is the last row when its hours form a gap-free prefix. OHLCV is first open, max high, min low, last close and summed volume.
- A stock underlying (a Robinhood options underlying, class `stock`) gets Yahoo's own session-shaped `4h` interval for `4h` (the fixed options regime timeframe) and the unsupported error for `2h`, `6h`, `8h` and `12h`.
- Crypto rows of a fixed interval (`1m` to `1d`) must sit on their UTC interval grid: a final off-grid row inside the current interval is moved to its grid opening, and any other off-grid, duplicate or out-of-order row is an error.
- The range uses standard Yahoo periods only (`1d`, `5d`, `1mo`, `3mo`, `6mo`, `1y`, `2y`, `5y`, `10y`, `max`; no `start=`): the smallest period that holds `limit + 2` bars (plus one target bucket when resampling) with a 25% margin, capped by the source interval (`1m` at `5d`, `2m` to `90m` at `1mo`, `1h` and every resampled name at `1y`). The stock class uses the cap for intraday sources. A Yahoo error or an empty frame still returns no rows, and the check's `< 30` guard decides.
- Config load (`robinhoodTimeframeConfigErrors` in `validateConfig`, so startup, SIGHUP and `go-trader probe`) refuses a Robinhood `check_robinhood.py` strategy whose `args` timeframe, or effective HTF interval with `htf_filter`, is not in the table, and a Robinhood spot strategy whose effective `regime.timeframe` is not in the table. It also refuses a raw `regime.timeframe` of `1M`, which go-trader lowercases to `1m` (one minute) for every platform; write `1mo` or `1m`. Each error names the strategy, the role and the value with the sorted list.
  Every admitted Robinhood spot coin is crypto, so every table timeframe serves it; a coin outside the crypto table is refused before these checks and gets no timeframe error (§ Robinhood asset classes).

### Robinhood asset classes

A Robinhood symbol has no class of its own: `type: spot` is always a crypto coin and `type: options` is always a stock or ETF underlying, so `LTC` the coin and `LTC` the stock stay apart (#1909).

- **Crypto table.** `YAHOO_CRYPTO_MAP` in `platforms/robinhood/adapter.py` is the only list of supported coins; Go `robinhoodYahooCryptoMap` mirrors it and `TestRobinhoodTimeframeTablesMatchThePythonAdapter` (`pyintegration`) compares keys, tickers and both resolvers on a fixed probe set. It holds 59 coins: AAVE, ADA, ALGO, ATOM, AVAX, AVNT, AXS, BAT, BCH, BNB, BONK, BTC, CASHCAT, DOGE, DOT, ENA, ETC, ETH, FLR, HBAR, HYPE, INJ, JTO, LDO, LINK, LTC, MEW, MORPHO, NEAR, ONDO, OP, ORCA, PAXG, PEPE, PNUT, PYTH, QNT, RAY, RE, RENDER, SEI, SHIB, SNX, SOL, SUI, SYRUP, UNI, USDC, VIRTUAL, W, WIF, WLD, WLFI, XDC, XPL, XRP, XTZ, ZEC and ZORA. Each ticker is `<CODE>-USD` except HYPE `HYPE32196-USD`, MEW `MEW30126-USD`, MORPHO `MORPHO34104-USD`, PEPE `PEPE24478-USD`, SUI `SUI20947-USD`, UNI `UNI7083-USD`, WLFI `WLFI33251-USD` and ZORA `ZORA35931-USD`.
  Evidence (2026-11-03, public unauthenticated reads, no Robinhood login): each coin is a `tradable` USD pair in `GET https://nummus.robinhood.com/currency_pairs/`, its Yahoo chart and search report quote type `CRYPTOCURRENCY` with the Robinhood asset name (case and spacing aside) and 114 non-null hourly rows over 5 days, and its last Yahoo hourly close is within 1% of the public Robinhood mark (`GET https://api.robinhood.com/marketdata/forex/quotes/?ids=<pair ids>`). MATIC was removed: Robinhood has no MATIC pair and Yahoo `MATIC-USD` returned 404; POL is not a substitute. Same-asset coins with another Yahoo name (CRV, EIGEN, FET, FLOKI, GRAM, XLM, ZRX), coins whose Yahoo ticker is another asset (MON, STRK, AERO, ARB, SKY, USDG) and coins with no Yahoo crypto ticker stay unsupported. Add a coin only with the same dated venue and Yahoo evidence in its pull request; never infer a ticker from a suffix.
- **Admission.** `robinhoodTimeframeConfigErrors` (every loader: startup, SIGHUP, `probe`, `inspect`, `storage-inspect`, ledger export and capture) refuses every Robinhood `type: spot` strategy, custom check scripts included, whose normalized `args[1]` is missing or not a table key (a stock ticker such as `AAPL`, a pair such as `XRP-USD` or `BTC/USD`, or `MATIC`), with one error naming the strategy, the input, the sorted keys and the options route; that strategy gets no dependent timeframe or closed-bar error. An empty-after-trimming coin keeps its earlier normalization error alone. Orders and holdings keep the bare coin code.
- **Adapter.** `RobinhoodExchangeAdapter(asset_class=...)` takes `crypto`, `stock` or nothing; an invalid value fails before login. With no class only order, holdings and order-resolution methods work (execute, close, position and resolve scripts). Every market-data method needs a class and validates the symbol before any provider call: crypto accepts only an exact table key (`robinhood_crypto_yahoo_ticker`, `UnsupportedRobinhoodSymbol` otherwise); stock (`robinhood_stock_yahoo_ticker`) accepts a 1 to 6 character ticker starting with a letter with an optional `.X`/`-X` share class, keeps its spelling, and refuses `/` and `-USD`/`-USDT`/`-USDC` pairs. Crypto prices use only `rh.crypto` and fall back only to the table ticker; stock prices use `rh.stocks` and the stock ticker. Volatility, expiry, strike, premium, option-mark and settlement reads need the stock class (`require_stock_underlying`). Class is never inferred from table membership or a ticker suffix.
- **Callers.** `check_robinhood.py` validates the coin before it builds the adapter (one JSON error and exit 1 with no login, quote or Yahoo request) and passes `crypto`; `fetch_candles.py` maps spot to `crypto` and options to `stock` and refuses other Robinhood types, also under `--probe-only`; `check_options.py` and `fetch_option_marks.py` pass `stock` (the options check validates the underlying first); `check_regime.py` requires `--asset-class crypto|stock` for Robinhood, refuses it elsewhere, checks it before the `--probe-only` return, validates the symbol before the adapter and echoes `asset_class`; `llm_review.py` builds a Robinhood adapter only for spot (`crypto`) or options (`stock`) after the symbol check and stays advisory. For Robinhood, the options check's candle helper and the regime worker never retry on Binance.US, also with `--allow-spot-fallback`; other platforms keep their fallback.
- **Regime identity.** `regimeBundleKey.AssetClass` is `crypto` or `stock` for Robinhood and empty elsewhere; it is part of request de-duplication, both sort orders, the worker argv, the output check (a missing or other `asset_class` refuses the bundle) and the identity text (`robinhood:crypto/LTC/4h`). Robinhood options regime requests no longer allow the spot fallback. Startup probes run the regime worker with `--asset-class crypto` for any Robinhood spot strategy and `--asset-class stock` for any Robinhood options strategy (plus the matching chart probes), so an older worker fails before a cycle.
- **Stored identity.** `regime_window_history`, `regime_window_transitions` and `regime_reversal_alerts` carry `asset_class TEXT NOT NULL DEFAULT ''`, added by `migrateRegimeAssetClassSchema` in one transaction (§ Storage Ownership). Earlier Robinhood rows keep an empty class: the status note, API and dashboard show them as `robinhood:unclassified`, and they never seed crypto or stock debounce or reversal state. Retention still prunes them by `ts` under `regime.transitions.retention_days`.
- **Unmeasured.** The table was proven with public reads only; an authenticated `currency_pairs` read was not run. Generic startup price fetching (`collectPriceSymbols` sends bare Robinhood symbols to Binance.US) is unchanged.

### Robinhood coin identity

A Robinhood spot strategy has one coin identity: `args[1]` trimmed and uppercased (`canonicalRobinhoodCoin` in `scheduler/robinhood_identity.go`, #1908).
Every config loader (startup, SIGHUP, `probe`, read-only reports, `inspect`) applies it in memory after platform inference and never writes the config file; a coin that is empty after trimming refuses the load.
The daemon prints one `[config] WARN: strategy <id> robinhood coin "<old>" is read as "<NEW>"` line per rewritten strategy. Write the uppercase spelling into `args[1]`: an older binary reads the book by the exact configured spelling.
A case-only `args[1]` edit reloads on SIGHUP; a different coin stays restart-required.
The signal result must echo the configured coin exactly; any other `symbol` is a script error (log, script-failure alert after the usual threshold) and reaches no order.
The holdings read uppercases each coin and fails the whole read on an empty coin or two rows that are one coin, so it can never prove flat.
Locks, unresolved-order checks, owner lists and books all use the canonical coin, so `btc` and `BTC` peers are one shared coin.

**Startup migration.** Writable startup (daemon and `--once`, never `summary`, `leaderboard`, `probe`, `inspect` or `storage-inspect`) runs it after the ownership locks and storage inspection and before any other state write: legacy portfolio-risk placement, `initial_capital` reconciliation, shared-wallet transitions, quote recovery, order recovery and the first cycle.
It loads every book and then checks every configured Robinhood spot strategy in live, paper and every named paper source before it writes anything. For configured coin `C` it renames only spellings whose trimmed uppercase form is `C`: the position row (key and symbol), the Robinhood pending circuit-close symbols, the shared-close notice coin, the strategy's unresolved `robinhood_order_attempts` symbol, and its `entry_decision_state` rows.
It keeps quantities, costs, stops, position ids, order ids, `ref_id`, statuses, amounts, fees and the frozen `request_json` and `evidence_json` as they are. It never rewrites trades, closed positions, diagnostics, settled or `no_fill` attempts, completed entry intents or regime history; settlement and recovery read the attempt row's symbol.
It refuses (exit 80, CRITICAL log, owner DM, nothing written) when two positions or a key and its symbol field convert to one coin (a zero-quantity row counts), two pending closes or two entry decisions on one interval and side convert to one key, an active entry intent uses an alias spelling, or two unresolved attempts would hold one account and coin. The message names each strategy and key. Compare them with the Robinhood account and a state-file backup before any manual correction; never delete a row to pass the check.
Each changed book commits in its own transaction (primary, then paper, then sources by id; strategies sorted), with each renamed row checked by its full old key. A failure rolls that book back whole. There is no rollback across state files: a later book failure stops startup with exit 80 after earlier books committed, and the next start finishes the rest without migrating a committed book again.
Each committed book logs `[storage] Robinhood coin identity migrated for <id> in the <role> state file to <C>: positions=… pending_circuit_closes=… shared_close_notice=… unresolved_attempts=… entry_decisions=…`. A start with nothing to convert writes nothing and logs no migration line.

**Rollback.** An older binary reads `Positions[args[1]]`, so a migrated `BTC` book under a config that still says `btc` reads as flat: it would never sell that coin and could buy again.
`update.sh` recovery therefore checks before it starts the previous binary (`update_robinhood_identity_rollback_check`): when a configured Robinhood spot strategy's `args[1]` differs from the spelling of its position, unresolved attempt or pending circuit close in any state file (same coin after trim and uppercase), and the restored `go-trader` does not carry the `go-trader-capability:robinhood-coin-identity-v1` marker (the binary is read as bytes and never run), recovery leaves the service stopped (`rh_identity=blocked`, `INCOMPLETE`) and names each strategy.
A restored binary that carries the marker canonicalizes the coin itself, so recovery logs the differences as not blocking and starts it.
A restored binary that cannot be read, or a state file that cannot be read, always blocks.
Write the spelling it names into `args[1]`, then start the service.
`go-trader version --json` lists the marker under `capabilities`.
The `update.sh` that deploys this change for the first time is the previous one and has no such check, so for that update write the uppercase spelling into `args[1]` just before `bash scripts/update.sh --restart` (the new binary accepts both), or keep the service stopped if recovery restarts an older binary.

### Robinhood account binding

Each open live Robinhood spot book records the account that owns its coins (#1921): `Position.AccountKey` (the `ROBINHOOD_USERNAME` key of `robinhoodAccountKey`), `VenueAccountID` (the crypto account id the venue reports) and `AccountBindingSource` (`settlement`, `backfill` or `operator:<discord user id>`), stored in the `positions` columns `account_key`, `venue_account_id` and `account_binding_source` (`TEXT NOT NULL DEFAULT ''`). Paper books, other platforms and Robinhood options keep empty values. The session files are keyed by the username hash too, so they cannot prove which account owns an older book.

`robinhoodBookAccountState` (`scheduler/robinhood_account_binding.go`) is the one classifier. `foreign`: no configured username, a stored key that differs from the configured key, or a stored venue id that differs from the venue id of the holdings read in use. `unbound`: a key or a venue id is missing (key-only legacy evidence stays held). `current`: both are present and match. Only `current` books are sold or reconciled; `foreign` and `unbound` books are **held**.

- **Holdings reads.** `fetch_robinhood_positions.py` reads the crypto account id (`adapter.crypto_account_id`) and the holdings in one session with one 30 s budget and always emits `account_id`, also with no positions. Go (`RobinhoodHoldings`) refuses the whole read on a missing or blank id, a failed read, malformed holdings or an empty or duplicate coin. Every flat proof names the read's account id.
- **Venue-side check.** `check_robinhood.py --execute --side=sell` and `close_robinhood_position.py` need `--expected-account-id`; the close script checks it before the holdings read and never returns `already_flat` for a mismatch. A missing id or a mismatch is `not_sent` with no order. Go always passes the book's venue id (signal sell, frozen as `expected_account_id` in the request) or the common id of the eligible books (emergency close). Recovery passes the attempt's venue id, else the frozen id.
- **Signal orders.** A held book reserves no attempt and sends nothing (CRITICAL log). The account key, position id, side, quantity and binding are checked again under `mu` before the attempt row is saved. A buy into a flat strategy proceeds; its settlement stamps the attempt's key and venue id with source `settlement` (a missing venue id leaves a held key-only binding).
- **Settlement.** Signal and emergency settlement hold an attempt (status `settling`, evidence kept, no venue call) whose account key is empty or differs from the configured key, so restoring the original username lets it settle. A sell settles only into a book whose key and venue id equal the attempt's. Emergency settlement splits a fill only over those books; when the fill is above them and a held book of the coin has quantity, the attempt stays `settling` with an account-binding hold reason and no excess alert.
- **Kill switch and circuit breaker.** The kill switch sells only the eligible books' quantity (one common venue id) and settles only into them; a coin with any held quantity stays in `Errors` and out of every flat list, so `ConfirmedFlat` and the no-owner auto-reset stay false. Both circuit-breaker loops hold a held own book: no close, no model-only close, and the pending close is kept, also on the shared-coin failure path. Every automatic model-only close (`closeRobinhoodBookVerifiedFlat`) needs a `robinhoodFlatProof`: the post-close read's account id and the eligible position snapshot taken after settlement and before that read. Under `mu` the position id, side, quantity and binding must still match and the book must be `current` for that read; otherwise CRITICAL and the book stays. A kept book blocks the kill-switch auto-reset for that cycle.
- **Startup backfill.** Writable startup (never `probe`, `inspect`, `storage-inspect` or read-only reports) runs `backfillRobinhoodAccountBindings` directly after the coin-identity migration. For each configured live Robinhood spot long book with an empty key it needs a position id, exactly one opening trade (`is_close = 0`) with a venue order id and exactly one settled live signal buy attempt of that strategy with that order id (coins compared canonically). A unique match copies the attempt's key and venue id with source `backfill`; anything else leaves the book unbound. Every candidate is read before any write; a read error refuses startup (exit 80, owner DM) with nothing bound. Each bound book commits alone; a later failure names the earlier commits and the next start finishes. Startup prints one sorted `[robinhood-account]` line per open live book with its state and key fingerprints; it never prints the username.
- **Alerts.** `notifyRobinhoodHeldBooks` (startup and each cycle) sends one **ROBINHOOD BOOK HELD** owner DM per held book per `effectiveAlertThrottleInterval()`; with no owner only the CRITICAL log line is written.
  The collector classifies each book with the venue id of the configured account's latest holdings read (kill switch, circuit breaker or owner resolution; `noteRobinhoodVenueRead`, memory only), so a configured-key book bound to another venue account alerts as `foreign` after such a read; before any read in the process it uses no venue id.
- **Owner resolution.** `/go-trader-resolve-rh-account <strategy> <bind-current|release-book>` (owner DM only, DM confirm). Both refuse any unresolved attempt for the coin and a busy coin lock, and re-check the book after confirmation and after each read.
  A stored binding that names the configured key and a venue id is not refused at the command: the fresh holdings read decides, and both actions refuse the book as current only when that read names its bound venue id (a configured-key book bound to another venue account can be resolved).
  `bind-current` reads the configured account's holdings and binds the configured key and read venue id only if a stored venue id matches and the holdings cover the current-bound peers plus this book; one exception: a `settling` sell that targets this book with the configured key and the read venue id is allowed, and the check is net of its confirmed quantity.
  `release-book` is for coins outside every account this daemon manages (for example an inaccessible former account): it reads the configured account's holdings fresh and refuses if they are above the current-bound peer books (then the coins may be here: bind instead), and refuses a book opened within the holdings-lag window and a missing or non-finite mark.
  It then closes the book at the mark as a model-only close with reason `robinhood_account_released`, the operator id and the prior binding, sends no order and does not claim the former account is flat. The owner decided to keep this fresh read ("flat only via fresh strict read") over the issue's release with no venue read.
  Both stage a copy of the book and publish memory only after the save commits.
- **No owner.** With no DM owner, a completely bound book resumes only when its original credentials are configured again; a book with missing historical evidence needs a DM owner and `bind-current`. Do not edit state rows while the daemon runs.
- **Operator rule.** Flatten and confirm the old account before you change `ROBINHOOD_USERNAME`.
- **Inspection.** `storage-inspect` lists each open live Robinhood spot book with its stored binding, fingerprints and local state (`--json` key `robinhood_account_bindings`); it makes no venue read, so it cannot confirm the logged-in account. An old state file without the columns reports missing evidence, with no migration.
- **Rollback.** A binary without the binding must not run over open live Robinhood books after this change. `update.sh` recovery (`update_robinhood_binding_rollback_check`) keeps the service stopped (`rh_binding=blocked`, `INCOMPLETE`) when a state file has the binding columns and an owned live Robinhood spot book has positive quantity (empty bindings included), unless the restored binary carries `go-trader-capability:robinhood-account-binding-v1`; an unreadable binary or state file always blocks. The `update.sh` that first deploys this change is the previous one, so keep the original Robinhood credentials until that update succeeds.

---

## Run And Install Service

```bash
./go-trader --config scheduler/config.json --once     # smoke test
mkdir -p logs
export DISCORD_BOT_TOKEN="{token}"
sudo bash scripts/install-service.sh
```

The installer copies the unit, runs `daemon-reload`, enables, starts, and pre-creates `logs/` so `ProtectSystem=strict` does not block first-run logging.

Templated multi-instance: `sudo bash scripts/install-service.sh systemd/go-trader@.service paper-testing`. A shared market feed service installs the same way (`... go-trader@.service feed-primary`) with a `role: "feed"` config (§ Shared market feed). Install without starting: `NO_START=1 sudo bash scripts/install-service.sh`.

```bash
sudo systemctl start|stop|restart|status go-trader
journalctl --namespace=+go-trader -u go-trader -n 100 --no-pager
```

**Journal namespace.** Both shipped units set `LogNamespace=go-trader`: every go-trader unit (plain and `go-trader@<instance>`) logs to one separate journal under `/var/log/journal/<machine-id>.go-trader/`, with its own size cap, and nothing is forwarded to syslog.
A plain `journalctl -u <unit>` shows only systemd's own start, stop and exit lines, which stay in the default journal.
`--namespace=go-trader` shows the go-trader output alone; `--namespace=+go-trader` merges it with the default journal, so the exit lines and any pre-move history appear too.
Before it installs the unit, the installer syncs `systemd/journald@go-trader.conf` to `/etc/systemd/journald@go-trader.conf` (`Storage=persistent`, `SystemMaxUse=2G`, `ForwardToSyslog=no`).
That file is managed: a differing copy is replaced, kept as `.prev`, and a warning names the drop-in directory.
A symlink there is left alone.
After a change, `systemctl try-restart systemd-journald@go-trader.service` applies it (journald keeps the service's log streams across the restart).
If that restart fails, the previous file is put back and the install stops before the unit.
Local settings (for example a different `SystemMaxUse`) go in `/etc/systemd/journald@go-trader.conf.d/*.conf`, followed by `sudo systemctl restart systemd-journald@go-trader.service`.
`LogNamespace=` needs systemd 245 or newer.
On an older systemd the installer warns, skips the config, and continues; systemd ignores the unknown unit line and the logs stay in the default journal.

**Paper CPU priority (#1987).** A scheduler process whose roster is non-empty and holds no live strategy (by `isLiveArgs`) runs its paper check children at a lower CPU priority.
The decision is made once at startup, because a SIGHUP reload cannot add or remove strategies or change their args. One `[priority]` line states the result: the increment, or the reason it is off (empty roster, a live strategy, or `GO_TRADER_PAPER_NICE=0`).
The children that get the lower priority are the per-strategy check scripts (spot, options, Hyperliquid, TopStep, Robinhood, OKX, and the Hyperliquid manual check) and a Hyperliquid batch group whose members are all paper.
Target nice = `min(19, scheduler nice + GO_TRADER_PAPER_NICE)`, default increment 10, so the value is relative to the nice the unit already runs at.
Right after `cmd.Start()` the scheduler calls `setpriority(PRIO_PGRP, <child pid>, <target>)`. Each child leads its own process group, so every thread of the child and every process it starts later get the value. The child runs at the scheduler priority only for the short interpreter-start window before that call.
A process with any live strategy keeps every child at its own priority: its cycle loop runs checks one at a time, so a slower paper check would delay the next live check.
These children always keep the default priority: every `runPythonSideEffect*` caller and `Run*Execute` wrapper, the regime bundle check, LLM entry analysis, UI candle and tuning children, the startup probe, the Robinhood login helper, and shared read-only fetches such as prices and balances.
Failure policy: a child that already exited (`ESRCH`) is ignored. Any other error prints one `[WARN] paper check child priority:` line per distinct error, and the check continues at the default priority. Raising a child's nice needs no privilege, so `NoNewPrivileges=true` does not block it.
Known limits: a lower-priority child holds its `pythonSemaphore` slot (capacity 4) longer on a busy host, and paper checks can reach the 30 s script timeout or the batch timeout more often. A batch timeout keeps the shared-state failure contract (§ Hyperliquid Batched Signal Checks), so count `hl-batch .* shared state failed` lines on a paper instance after the deploy.
Scope across services: nice orders tasks that share one CPU cgroup. When the root `cgroup.subtree_control` lists `cpu` and `system.slice` does not (no unit sets a CPU property), all go-trader services share one pool, and a paper instance yields to a live instance on the same host. When `cpu` is off at the root and `kernel.sched_autogroup_enabled=1`, each service session is its own autogroup, and nice orders tasks only inside one service.
Operator check: `ps -eLo pid,tid,ni,args | grep check_` while a paper cycle runs. A per-unit `CPUWeight` for paper-only systemd instances is a separate follow-up change.

**Config out of the deploy tree.** Both units set `StateDirectory=go-trader[/%i]` and point `ExecStart --config` at `/var/lib/go-trader[/<instance>]/config.json`. For an existing in-tree deploy, stop the service first, then run `scripts/migrate-config-out-of-tree.sh [--instance <name>]` — it refuses while the daemon is live. `scheduler/config.json` stays as a transition symlink; a config-version migration that rewrites the file replaces that symlink with a regular in-tree file, so prefer the out-of-tree path.

**Moving a hand-made unit to the template layout (optional).** `scripts/migrate-service-layout.py` moves one custom scheduler unit (for example `go-trader-live.service` run as root from a workspace) to `go-trader@<instance>.service`: code in `/opt/go-trader-<instance>`, config in `/var/lib/go-trader/<instance>/config.json`, user `go-trader`, the template's sandbox and journal namespace. The binary, the source version, the effective config and the status port do not change. Updates never run it. Run every subcommand as root, one unit at a time:

1. **`plan --unit <old> --instance <name>`** is read-only: it creates no journal or lock file and changes no unit, config, secret, tree, database, sidecar or lock content.
   It needs the old unit active and on this release (its `/health` carries `run_evidence`).
   It prints the source and target, each state file's canonical source and target path (`db_file`, `paper_db_file`, every `paper_sources[].db_file`; a file outside `scheduler/` of the tree moves to `/var/lib/go-trader/<name>/`, and the config value changes with it), every config and environment change (variable names only, never values), each access change (the target config and every database become 0600 `go-trader`, the config directory is created 0700, and the tree root is 0700 when other accounts cannot reach the source tree, else 0755), strategies the running daemon's kill switch held in its latest cycle, the journal command, service references and tunnel notes, the execution-proof bound, and a plan id.
   The running daemon must hold the ownership lock of every mapped file and no other state file, so a discovery error is refused and never omits a database.
   `replay_log_path` stays shared and unchanged, and only an absolute path under `/var/lib/go-trader/shared/` is accepted.
2. **`apply --unit <old> --instance <name> [--plan-id <id>] [--confirm-live <old>] [--exec-timeout <s>]`** re-inspects first.
   A unit with a live or `manual` strategy needs `--confirm-live <old>`, and a stale `--plan-id` is refused, both before any change.
   Before the old unit stops, it creates the `go-trader` account when needed, copies the tree without state files, locks, logs, `.env`, the tuning cache or other units' configs (a git-tracked file that only a generic pattern such as `*.db` matches, for example a test fixture, is still copied) and proves the copy (checksums, the binary hash, and a fingerprint of the tracked files, or of the code files in a tree deployed without `.git`), gives the tree to `go-trader` as the README template install does (root can still run `scripts/update.sh` and `scripts/shared-feed-convert.sh` there with no `safe.directory` setting;
   Auto-Update, "Root on a tree another account owns"), writes the config (transition symlink in the new tree;
   the original stays as evidence) and a 0600 `.env` built from `Environment=` and `EnvironmentFile=`, runs the binary's `probe` and write checks as `go-trader` under the template sandbox, checks that systemd reads the new `.env` to the same values, and installs the shared template and journald config only where they are absent.
   It then stops, disables and masks the old unit, takes the ownership and manual-action locks of every source and target file (primary, paper, then sources by id), snapshots each stopped file with its WAL, copies the tuning run history to the new config directory, writes a checkpointed copy to the target, and proves identical content table by table plus a clean `storage-inspect` by the target binary before any target start.
   It releases only the target locks, installs with `NO_START=1`, starts the new unit, and requires `/health` with the unit's pid, the old version and the old port, then every strategy evaluated, skipped at zero capital, or held by the portfolio kill switch, and a later successful save (a strategy held for any other reason fails the proof at once), read back from the primary file.
   The old unit stays stopped, disabled and masked, and its tree stays in place.
3. **`rollback --instance <name> [--confirm-live <old>]`** refuses with no change (exit 31) when a source file or config changed after the apply snapshot, or when the new unit's config names a state file (`db_file`, `paper_db_file`, a `paper_sources[].db_file`) that apply did not move and that the old unit would resolve to another path, or that lies inside a directory the rollback moves aside (a folded paper source that stays at its own absolute path passes).
   An automatic recovery holds both units stopped in that case (exit 30).
   Otherwise it stops and disables the new unit, takes both lock types on both sides, copies the new unit's latest files to the recovery evidence, writes them back to the original canonical paths (a config the new unit changed is mapped back), restores the old unit's file, enable links and state, starts it, and moves `/opt/go-trader-<name>` and `/var/lib/go-trader/<name>` aside as `.rolled-back-<txn>` (tuning runs made meanwhile stay there).
   A repeat is a no-op, and a later `apply` starts a new transaction.
4. **`status [--instance <name>]`** is read-only and prints the journal, both units, the lock holders and `/health`.

Any failure recovers on its own.
Before the old unit stops, the new artifacts are removed (exit 20).
Before the new unit's enable, the old unit restarts on its own files (exit 21).
From the enable on, the new unit's records are authoritative, including after an uncertain start, and the reverse transfer above runs (exit 22).
A process loss or reboot leaves a journal in `/var/lib/go-trader/service-layout/<name>/journal.jsonl`; at most one unit is enabled at any stage, `apply` refuses (exit 18) until `rollback` finishes the recovery, and SIGINT, SIGTERM and SIGHUP run the same recovery as a failure (SIGHUP output goes to `interrupted.log`).
The initial snapshot, the config copy and every recovery copy stay under `/var/lib/go-trader/service-layout/<name>/<txn>/evidence/`.
A recovery that cannot confirm the stop, prove the state, or finish the transfer never restarts from the initial snapshot: it stops and disables both units (the new one is also masked, and the old one stays masked until its restore), exits 30 and names the evidence; `rollback` then resumes it.

Refusals come before any change: 10 unit (unsupported directives such as `ExecStartPre`, extra `ExecStart` flags, aliases, units that other units require), 11 config or databases, 12 binary or health contract (restart pending, older release, `/health` not answering with the unit's pid), 13 environment (`PATH` or `UV_CACHE_DIR`, `PassEnvironment`, a running environment that differs from the unit's inputs, values that name the old tree), 14 target instance taken, 15 shared host files (an installed `go-trader@.service` or `journald@go-trader.conf` that differs from this tree, a foreign-owned file in `/var/lib/go-trader/shared`, a replay layout that cannot stay shared), 16 target runtime (a venv interpreter the service account cannot run, a venv tied to the old tree, `uv` or Go that root or the service account cannot run, low disk space;
`apply` also stops here with exit 20 when the sandboxed `probe` fails), 17 live confirmation missing, 18 another run holds the lock or a transaction is incomplete, 19 stale plan id.
`market_feed: shared` consumers and `role: feed` services are refused; use `scripts/shared-feed-convert.sh` for those.
After the move, `scripts/merge-paper-instance.sh` folds migrated paper deployments with its default paths.
`scripts/test_migrate_service_layout_fixture.sh` proves these rules on a Linux systemd host; the `shell-suites-migrate-<n>` CI jobs run its scenarios in shards (see Tests).

**File ownership.** Before any migration, startup write, probe or trading, the scheduler takes an exclusive lock on **every** configured state file, in role order (primary, then paper); a failure releases everything already held and exits with code 79 (`ExitSingletonLock`), naming the file and the holder pid. `--once` takes ownership too, so it refuses to run while the daemon is up — the daemon drains queued manual fills on its own next cycle. Startup then runs the read-only ownership check (`storage-inspect`); a rejected layout exits with code 80 (`ExitStorageOwnership`). Manual commands and the dashboard take only the owning file's manual-action lock, so they still work beside a running daemon; `backfill --apply` takes full ownership plus both manual-action locks.

**Startup probe.** Every unique check script runs with `--probe-only`. A non-zero result logs, DMs the owner, and exits with code 78 (`ExitProbeFailure`). Both unit files set `RestartPreventExitStatus=78 79 80`, so the service stays down instead of crash-looping. A probe failure right after an update almost always means `shared_scripts/` was not updated or the binary was not rebuilt — rerun `scripts/update.sh`.

**Graceful shutdown.** The daemon drains side-effecting subprocesses for up to 15 seconds, then SIGKILLs; state save, notifier flush, and DB close run afterwards. The unit sets `TimeoutStopSec=20`. Service-file edits need `daemon-reload`; `scripts/update.sh --restart` runs it for you when it installs a changed shipped unit (see Auto-Update § Unit sync), so a shipped unit change reaches a deployment on the next update instead of waiting for a hand-run `install-service.sh`.

---

## Docker Container

`docs/DOCKER.md` is the operator guide (install, setup, backup, restore, upgrade, rollback limits). systemd stays the canonical Linux production deployment, and the Linux process deployment is unchanged. The mechanism:

- **Image.** The root `Dockerfile` builds the binary (`-buildvcs=false`, `-X main.Version`, `-X main.SourceCommit`) and the venv (`uv sync --frozen --no-dev`) from one commit, and copies only the runtime allowlist (`shared_scripts/`, `shared_strategies/`, `shared_tools/`, `platforms/`, `backtest/`) into `/app`. `.dockerignore` starts from `*`, re-includes that allowlist and strips tests, state, configs, locks, logs and secrets. The image runs as uid/gid 10001 with `GO_TRADER_RUNTIME=container`, owns empty `/data` (0700) and `/app/logs` (0750), and sets `GO_TRADER_OHLCV_CACHE_DB=/data/ohlcv_cache.sqlite3`.
- **Provenance.** `go-trader version --json` prints `version`, the embedded `source_commit` and `capabilities` (the Robinhood coin identity and account binding markers that update recovery reads); `--version` prints the release alone. The image labels `org.opencontainers.image.version` and `org.opencontainers.image.revision` carry the same values.
- **Runtime policy** (`runtime_policy.go`). `GO_TRADER_RUNTIME` is read once in `main` before any dispatch: unset means host, `container` means container, any other value exits 2. Container mode is never inferred from the filesystem.
- **Container config rules.** Every path-based config load (`LoadConfig`, `LoadConfigForProbe`, `LoadConfigReadOnly`; so startup, `probe`, SIGHUP reload and the dashboard and Discord config writes) refuses `auto_update` other than `off`, `role: "feed"`, `market_feed: "shared"`, a `status_port` other than 8099, and any `db_file`, `paper_db_file`, `paper_sources[].db_file` or `replay_log_path` that is in memory or resolves outside `/data`. The ledger-export config load is exempt because it binds snapshot paths.
- **Volumes.** The daemon and `supervise` refuse to start unless `/data` and `/app/logs` are mount points (read from `/proc/self/mountinfo`) and writable by the running uid; the message names the uid and docs/DOCKER.md § Volume permissions. A new empty named volume takes the image directory's owner and mode on first mount, so first setup needs no chown, and nothing changes the owner of existing content.
- **Setup.** In container mode `init` defaults `--output` to `/data/config.json`, never prompts for a path, refuses an output outside `/data`, skips the live-mode and manual-trading prompts, refuses a live or `manual` strategy and an `autoUpdate` other than `off` (interactive and `--json`), and writes `db_file` next to the config (`/data/state.db`). On a host, interactive `init` offers the `--output` value as the path default (still `scheduler/config.json`).
- **Status bind.** `validateStatusBind` runs right after the config load, before ownership and before any listener: loopback always passes, a host refuses everything else, and a container accepts only `0.0.0.0` or `::` and only with `STATUS_AUTH_TOKEN`. Every bearer check (`/status`, `/history`, `/health` run evidence, `requireAPIAuth`) compares in constant time.
- **Updates, restarts and logs.** In container mode `checkForUpdates` returns before Git, `applyUpgrade` DMs the image-pull steps, `restartSelf` signals the daemon's own stop path (drain, state save, exit 0) instead of `systemctl` or `syscall.Exec`, `/logs` returns the `docker compose logs` command, and the dashboard restart message names `docker compose restart go-trader`.
- **Supervisor** (`go-trader supervise`, container only). Compose runs it behind `init: true`. It starts one daemon in its own process group, forwards SIGTERM, SIGINT and SIGHUP, and exits with the daemon's status (128 plus the signal number for a signal death). On 78, 79 or 80 without a pending termination it prints one `[supervisor] CRITICAL` line and holds with no daemon until SIGTERM or SIGINT, then exits with that code; the health check fails while it holds. It refuses a subcommand argument, so utilities never run under it.
- **Health.** `go-trader healthcheck [--url] [--timeout]` exits 0 only on an HTTP 2xx from `http://127.0.0.1:8099/health` (§ Status gives the `/health` rule). The daemon refuses `--status-port` other than 8099 and binds 8099 with no fallback port, so the bound port is always the one Compose publishes and the health check reads. Discord platform setup names `docker/go-trader.env` and `docker compose up -d --force-recreate go-trader`, because a restart keeps the old environment.
- **Compose** (`docker/compose.yaml`). Service `go-trader` runs `supervise --config /data/config.json` with `GO_TRADER_STATUS_BIND=0.0.0.0`, publishes `127.0.0.1:${GO_TRADER_HOST_PORT:-8099}`, and has its own network, `restart: unless-stopped`, `stop_grace_period: 20s`, a read-only root, a `/tmp` tmpfs, `no-new-privileges` and every capability dropped.
  Service `cli` (profile `cli`) has the same image, volumes and hardening with `restart: "no"`, no health check and no published port; it runs `init`, `probe`, `inspect`, `storage-inspect`, manual commands and `--once` (which needs the service stopped).
  `GO_TRADER_TAG` in `docker/.env` is required and pins the image tag; secrets come from `docker/go-trader.env`. `docker/compose.build.yaml` builds `go-trader:local` from the checkout.
- **Docker restart behavior** (verified by `scripts/test_container_image.sh`): a daemon crash after a healthy start (exit 137) and an intentional restart (exit 0) both restart the container; an early exit 1 (for example a refused `auto_update`) restarts with Docker's doubling delay; a held 78, 79 or 80 stays running and unhealthy with no restart.
- **Reload.** Send SIGHUP through the container's init: `docker compose exec go-trader sh -c 'kill -HUP 1'`. `docker compose kill -s HUP` reaches the daemon too, but Docker Engine (checked on 29.8.2) then marks the container as stopped by hand, so the next exit is not restarted until `docker compose restart go-trader`; `docs/DOCKER.md` states this.
- **CI** (`.github/workflows/container.yml`). Every pull request and `main` push builds the image natively on `linux/amd64` and `linux/arm64` with canary files planted in the build context, then runs `scripts/test_container_image.sh` under `scripts/run_ci_shell_suite.sh`. The `image` jobs get a read-only token, and their checkout keeps no credential (a step fails if one is left in `.git/config`). On a published release each `image` job saves its tested image as an artifact; the `publish` job, the only job with `packages: write`, checks out no code, loads both images, refuses one whose image ID or commit differs from the tested one, pushes each by digest and joins them under the release tag, the source commit and `latest` (not for a prerelease).

---

## Egress Proxy Mode

Egress proxy mode sends every external connection of the scheduler, the feed service, manual venue commands and every child process through one configured proxy, and a kernel boundary blocks every other route (issue 1841).
With the mode off (the default) nothing changes: no new environment variable is read into a policy, no child argument changes, and the clients keep their existing transports.

**Turning it on.**
- `GO_TRADER_EGRESS_PROXY` in the unit's environment file (or `docker/go-trader.env`) holds the proxy URL: `http`, `https` (TLS to the proxy), `socks5` or `socks5h`, a host, an explicit port, and optional `user:password`.
  A path, query or fragment is refused.
  Both SOCKS forms send venue host names to the proxy; no venue name is resolved locally.
- A non-empty value turns the mode on, and an invalid value refuses every entry path, even with no `egress` block.
  Error text names only `scheme://host:port` (or a scheme-free reason); credentials never reach logs, status, alerts, child output or the manifest.
- The optional top-level `egress` block takes only `proxy_required` and `notifications_direct` (booleans, default false; any other key or type is refused by the scheduler, feed, probe and read-only loaders).
  `proxy_required: true` refuses entry when the variable is unset or empty.
  `notifications_direct: true` without the mode is refused.
- The policy is frozen per process.
  A SIGHUP that changes the `egress` block is refused in the scheduler and the feed (restart required), and the environment is read once.

**Enforcement boundary** (`scheduler/cmd/egress-controller/`, `scheduler/internal/egresspolicy/`).
- A root-owned controller (`go-trader-egress@<deployment>.service`, binary `/usr/local/libexec/go-trader-egress/<deployment>/go-trader-egress-controller`, manifest `/etc/go-trader/egress/<deployment>.json`) owns a network namespace, `go-trader-egress-<deployment>`, whose only usable link is loopback.
- Inside it, table `inet go_trader_egress` drops all output, input and forward traffic except loopback; the output rule comment carries the installed generation.
  Rules are installed in one `nft -f` transaction before the controller serves, re-verified every 15 seconds and on every attestation (exact chains, policies, rules, and no non-loopback link up), and reinstalled with a new generation when they drift.
- The controller listens inside the namespace on the proxy address the workers dial: `127.77.0.1:<port>` for a proxy host name (the namespace hosts file maps the name there), the proxy's own literal address (added to `lo`), or the loopback address for a host-loopback proxy.
  It relays each TCP connection to the proxy, so workers never route past loopback and the controller is the only process that resolves the proxy name.
- The controller re-resolves the proxy name every `resolve_seconds` (default 60, minimum 10).
  It accepts several addresses, refuses loopback, link-local, unspecified and multicast answers for a name, swaps the set atomically, and closes relayed connections to removed addresses.
  A failed resolution keeps the last good set and shows the error in `status`; with no good set the controller is not ready and workers refuse to start.
- Name service inside the namespace is the hosts file only (`/run/go-trader-egress/<deployment>/resolver/{hosts,resolv.conf,nsswitch.conf}` bind-mounted over `/etc`), and the drop-in makes the host resolver, D-Bus, nscd, Avahi, NetworkManager and container-runtime sockets inaccessible, so no venue name leaves through a host daemon.
  Filesystem Unix sockets (the shared feed socket) keep working.
- Workers run with no capabilities, `NoNewPrivileges`, `RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK`, `ProtectProc=invisible` and `SystemCallFilter=~@debug` (no ptrace or cross-process memory calls).
  On a host the controller refuses attestation while the worker account runs any process outside every enforced worker namespace (`go-trader-egress-*` with verified rules), so a worker cannot reach an unconfined process of its own account; instances of one account (`go-trader@.service`, a shared feed and its consumers) may share it when each runs in its own enforced namespace.
  A raw socket, a direct TCP or UDP connection over IPv4 or IPv6, ICMP, a direct DNS query, curl_cffi, and a child that never loads the Python bootstrap all fail with "network unreachable".
- The controller stopping or dying leaves the namespace and its rules in place; only the relays close, so proxy traffic fails closed until it restarts.
- **Attestation.** Every network entry path asks the controller over `/run/go-trader-egress/<deployment>/control.sock` (protocol 1; the peer must be root).
  The controller reads the caller's `SO_PEERCRED`, then checks the uid, that effective, permitted and ambient capabilities are empty, that `no_new_privs` is set, that the process did not change during the check, and that its network namespace is the managed one; it re-verifies the rules and requires a ready relay.
  The worker then checks that its own namespace and the configured proxy identity match the answer.
  An environment variable or a file cannot stand in for this answer.
  The daemon and the feed re-attest every 60 seconds (status `last_attested_at`, `attest_error`, `enforcement: unverified` while it fails); when the answer says the process is no longer in the managed namespace (for example after the Docker anchor restarted), the daemon DMs the owner with **EGRESS NAMESPACE LOST** and restarts, and the feed exits 1 so its unit restarts it, so both rejoin the enforced namespace.
- Supported deployments: Linux system services under systemd 247 or newer (`NetworkNamespacePath`, `ProtectProc`), rootful Docker Engine on a Linux host with the controller on the host, and manual commands through the controller launcher.
  Anything else, including a desktop shell or macOS, refuses the mode.

**Entry paths** (`egress.go`, `egress_gate.go`).
- The daemon (including `--once`, `--summary`, `--leaderboard`) and the feed service establish the policy right after the config load and before the notifier, ownership, capital sync or any client.
  Manual venue commands (`manual-open`, `manual-add`, `manual-close`, `force-close`, `manual-cancel`, `manual-update-sl`, `manual-cancel-sl`), `backfill`, `record-observations` (it opens the Hyperliquid websocket) and `robinhood-login` (its Python child runs under the bootstrap) do the same before dispatch.
- `probe` validates the policy and requires a ready controller with the same proxy identity (exit 78 otherwise); its Python children run under the bootstrap.
  It does not need the namespace, because it makes no external connection.
- Local commands (`init`, `export`, `inspect`, `storage-inspect`, `agent-info`, `diagnostics`, `feed-fetch`, `manual-clear-limit-row`) make no network call; when the variable is set they still pin their transports to the proxy, and they ignore `proxy_required`.
- `quote-replay` is offline (`egressOfflineSubcommands`): with no proxy variable it sets nothing up, and with the variable it records proxy mode with no configuration and no attestation, so it never reads `scheduler/config.json` or any deployed file.
- After attestation the daemon checks each Go client host it uses (Hyperliquid, OKX, Deribit, and Discord and Telegram when they go through the proxy) with a tunnel and a verified TLS handshake (no HTTP request, so no venue rate budget is spent), then runs `shared_tools/egress_bootstrap.py --check-policy <families>` for the Python client families of its strategies.
  The feed checks Hyperliquid in Go only.
  A failure is an ordinary exit 1, so the service retries; exits 78, 79 and 80 keep their meanings.

**Go clients.** Every venue client gets an explicit policy transport (`egressHTTPClient`), the feed websocket and Discord use the policy dialer (`egressWebsocketDialer`, `pinDiscordSession`), and `http.DefaultTransport`, `http.DefaultClient` and `websocket.DefaultDialer` are replaced in the mode.
One dialer (`egresspolicy.Dialer`) does HTTP CONNECT, CONNECT over TLS to an `https` proxy (the proxy certificate is verified against its host name), and SOCKS5 with user/password and domain targets, with context cancellation and deadlines and no direct fallback.
Host `HTTP(S)_PROXY`, `ALL_PROXY` and `NO_PROXY` never change routing in the mode.
Loopback and Unix-socket connections stay direct.

**Python children.** In the mode every launch (regular, side-effect, advisory, tuning, backtest and the probe launcher) runs `.venv/bin/python3 shared_tools/egress_bootstrap.py <script> <args>`.
Go applies per-call overrides first and then restores `GO_TRADER_EGRESS_MODE=proxy`, `GO_TRADER_EGRESS_PROXY`, upper- and lowercase `HTTP_PROXY`, `HTTPS_PROXY`, `ALL_PROXY` (socks5 becomes socks5h) and `NO_PROXY=localhost,127.0.0.1,::1`.
The bootstrap refuses a missing or invalid policy with `{"error": "egress policy: ..."}` and exit 1 before the target is imported, then runs the target exactly as plain Python would.
`shared_tools/egress_policy.py` pins each client family: ccxt (`httpsProxy`, or `socksProxy` with socks5h, plus a safety-net wrapper on `Exchange.__init__`), the Hyperliquid SDK at its base `API.__init__` (every `Info`/`Exchange` session before its first request), the robin_stocks module session before login, TopStep and Deribit requests, yfinance (`YfConfig.network.proxy` for curl_cffi), and the advisory script (`llm_review.py` now uses a pinned requests session).
At exit a child that made proxied requests writes one `go-trader-egress-outcome {"requests":N,"proxy_failures":M,"last_error":"..."}` line to stderr; Go adds it to the status and removes it before any stderr consumer sees it.
A pinned client verifies the proxy certificate against public roots only, so an `https` proxy with a private CA works only for clients that read `REQUESTS_CA_BUNDLE`.

**Notifications.** By default Discord REST and gateway, Telegram and GitHub issue reports use the proxy.
With `notifications_direct: true` they go through the notification broker (`internal/notifybroker`, run as `notify-broker --deployment <name>` under its own account and its own loopback-only namespace `go-trader-notify-<deployment>`).
The broker holds only the notification tokens (`/etc/go-trader/notify-<deployment>.env`, written by the installer with only `DISCORD_BOT_TOKEN`, `TELEGRAM_BOT_TOKEN`, `GO_TRADER_GITHUB_TOKEN`, `GITHUB_TOKEN`, `GO_TRADER_NOTIFY_GITHUB_REPOS`), cannot read the state, config or venue credentials, and reaches the network only through a controller-owned CONNECT proxy at `127.77.0.2:3128` that allows `discord.com`, `*.discord.gg`, `api.telegram.org` and `api.github.com` on port 443 to public addresses.
The scheduler talks to it over `/run/go-trader-egress/<deployment>/notify/broker.sock` with typed operations (send, DM channel, interaction response, follow-up with attachments, command registration, an allowlisted Telegram call, an issue in an allowlisted repository); both sides check the peer uid, and there is no URL, request or dial operation.
Inbound gateway interactions and DMs come back as events; owner checks, command dispatch, config changes and trading stay in the scheduler.
The worker's network policy is the same in both notification modes.
In direct mode the scheduler and feed refuse to start while they hold a Discord, Telegram or GitHub token (environment or config), so only the broker holds them.
The app unit starts after the broker unit; if the broker is not answering yet the scheduler waits up to 30 seconds and then keeps reconnecting, and the broker keeps serving Telegram and issue reports while it retries a failed Discord gateway open.
The scheduler installs its interaction handler before it waits for the broker's Discord identity; if the identity is not there within 30 seconds it logs one warning and registers the slash commands in the background once the gateway is up.
A startup refusal in direct mode sends one owner DM per reason per alert throttle interval across restarts (`.egress-startup-refusal.json` beside the config, removed on a clean start).

**Status, alerts and outages.** `/status` (scheduler) and the feed `/status` carry an `egress` block: `mode`, the redacted proxy, deployment, `enforcement` and generation, last success and error times, the redacted last error, consecutive and total tunnel failures, and the Python child totals.
Three consecutive tunnel failures send one **EGRESS PROXY FAILING** owner DM, repeated at the alert throttle interval while they last, and one **EGRESS PROXY RECOVERED** DM on recovery (in proxy notification mode those DMs only arrive once the proxy is back).
During an outage venue calls fail or hold through the existing paths: an order error after submission keeps the unknown-outcome hold and recovery path, nothing is retried blindly, and nothing is booked without a confirmed fill.
The application cannot close positions through an unreachable proxy; resting venue protection that is already armed (for example an on-chain stop) keeps working, and a position without it has none.

**Install and operate (host).**
1.
   Set `GO_TRADER_EGRESS_PROXY` (and, if wanted, `"egress": {"proxy_required": true}`) for the unit, then from the deployment checkout run `sudo bash scripts/egress-install.sh host --deployment <name> --unit <unit> [--notify-user go-trader-notify]`.
2.
   The installer refuses a checkout that `update.sh` would refuse (a foreign-owned tree whose units can change it outside `scheduler/` and `logs/`), builds the controller from the checkout's committed sources (git archive, never the service-writable tree), installs it root-owned, writes the manifest (scheme, host, port only), runs `preflight` (tools, a scratch namespace with rules, proxy resolution, systemd version), starts the controller, writes the app drop-in (`/etc/systemd/system/<unit>.d/50-egress.conf`, from `render-units`) and the broker unit, restarts the app inside the namespace, and checks `/health` through the status relay (host `127.0.0.1:<status_port>` to the namespace loopback, a fixed destination).
   The controller does not need the status port free: while the pre-egress service or a published container port still holds it, the relay retries every 2 seconds and `status` shows `status_relay_errors`; the proxy relay and attestation do not depend on it.
3.
   Manual venue commands run through the restricted launcher: `sudo /usr/local/libexec/go-trader-egress/<name>/go-trader-egress-controller launch --manifest /etc/go-trader/egress/<name>.json -- manual-open ...`.
   It accepts only a manual or backfill subcommand, `record-observations`, `robinhood-login`, `probe`, or the daemon with `--once`, `--summary` or `--leaderboard`, and runs the manifest's binary as the worker in a transient unit with the same sandbox and every environment file of the unit (`host.environment_files`).
   Running `go-trader` from a shell with the variable set refuses.
4.
   `status --manifest ...` prints the controller state; `sudo bash scripts/egress-install.sh uninstall --deployment <name> --unit <unit>` removes the drop-in, broker, controller and namespaces (remove the variable before starting the unit again, or it refuses).
5.
   Updates stay `sudo bash scripts/update.sh --restart`.
   For a unit with an egress drop-in it must run as root: it builds the controller from the same exported sources, runs the probe as the unit's user with the unit's environment files, switches the controller (previous binary kept as `.prev`) before the app restart, and restores both on rollback.
   Controller restarts use `systemctl restart --job-mode=ignore-dependencies`, because the drop-in's `Requires=` would otherwise restart the app with the controller; the app restarts once, after the controller is ready.
   The namespace rules stay installed through the switch.
   The notification broker unit (`go-trader-notify@<name>.service`) runs the controller binary, so after a switch or a rollback restore that restarted the controller, the update restarts a running broker too; a host with no running broker skips it.
   The scheduler and the broker share one request protocol, so a broker left on an older binary refuses new requests (`unknown operation`); the slash-command error then names the unit to restart.
   In the mode the in-process updater and `auto_update` checks refuse (the service has no route to Git) and `/restart` asks the controller to restart the registered unit.

**Container deployment.** `docker/compose.egress.yaml` adds `egress-net` (the release image running `egress-anchor`, `network_mode: none`, labeled with the deployment) and joins `go-trader` and `cli` to it with `network_mode: service:egress-net` (no published port; the host status relay serves `127.0.0.1:<port>`).
The host controller (`scripts/egress-install.sh docker ...`, mode `docker`; the installer waits only until the controller answers, since the anchor starts in the next step) attaches to the anchor's namespace, writes its hosts and resolv.conf, installs the rules and relays, and re-attaches when the anchor restarts; the anchor health check passes only when the controller reports this namespace enforced, and the application still attests on its own.
The optional `notify-broker` service (profile `notify`) runs the broker under its own uid with `network_mode: none`.
`docker compose run --rm cli ...` commands join the same namespace. docs/DOCKER.md § Egress proxy mode has the steps.

**Remote proxies and Gluetun.** Any reachable proxy works, including a Gluetun sidecar or a remote host; point the URL at the address the host can reach (a published Gluetun port such as `http://127.0.0.1:8888` works through the relay).
Verifying the exit address is an operator task outside the application: compare a non-venue echo service through and outside the proxy by hand; the application never makes a direct comparison.

**Adapter integration requirement.** A new platform adapter or check script must build its clients through the `shared_tools/egress_policy.py` helpers (`pin_ccxt`, `pin_requests_session`, `requests_proxies`, `pin_yfinance`, `pin_hyperliquid_sdk`), and a new Go client must use `egressHTTPClient` or `egressWebsocketDialer`; the kernel boundary still blocks a client that forgets, but it then fails instead of using the proxy.

**Tests.** `scheduler/egress_integration_test.go` (every scheme, failures, cancellation, loopback, host proxy variables, config strictness, entry refusals), `scheduler/egress_broker_integration_test.go` (Linux: direct notifications through a real broker with a fake Discord gateway, Telegram and GitHub, during a venue proxy outage), `scheduler/egress_pyintegration_test.go` (`pyintegration`), `shared_tools/test_egress_integration.py`, `scripts/test_egress_enforcement.sh` (root: namespace rules, attestation, every direct path, relay-only egress, the daemon in the namespace, controller loss, address refresh, systemd sandbox and launcher) and `scripts/test_egress_docker.sh` (root, rootful Docker: anchor, application attestation, status relay, one-shot commands, anchor restart).

---

## Auto-Update

`auto_update`: `off` | `daily` | `heartbeat`. When an update is found the bot notifies active Discord channels. With `DISCORD_OWNER_ID` set it DMs the owner; replying yes within 30 minutes runs `scripts/update.sh`, saves state, and restarts.
A container deployment (§ Docker Container) refuses any value other than `off`, never runs an update check, Git, `scripts/update.sh` or `systemctl`, and answers an upgrade request with the image-pull steps.

```bash
# Systemd deploy (default)
cd /path/to/go-trader && bash scripts/update.sh --restart

# Linux bare-process deploy (no systemd)
cd /path/to/go-trader && bash scripts/update.sh --restart --restart-mode signal

# Sync from a source clone without clobbering secrets/state/venv/binary
bash scripts/update.sh --rsync-from /path/to/source-clone --restart

# Batch-update every discovered deployment (requires --restart)
bash scripts/update.sh --all --restart [--update-all-root <parent-dir>]
```

`scripts/update.sh` is the single source of truth for host updates: `git pull --ff-only` + `uv sync --no-dev` + `go build`, all gated under `set -euo pipefail`. External host deploy automation (for example Ansible) must call this script rather than reproduce the steps inline.
The Docker image is the one exception: the root `Dockerfile` builds Go and Python from one committed revision, and a container updates only by pulling a new image (`docs/DOCKER.md` § Upgrade). Never run `scripts/update.sh` inside or against a container.

**Unit sync.** With `--restart` in systemd mode, the `unit` phase (between the binary swap and the restart) compares the loaded unit file against the one this repo ships for the resolved unit name — `go-trader.service` for a plain unit, `systemd/go-trader@.service` for `go-trader@<instance>.service`.
On a difference it keeps the loaded file as `<unit>.prev`, installs the shipped one with mode 0644, and runs `daemon-reload`; on a match that systemd has not reloaded it runs `daemon-reload` only; otherwise it does nothing.
A rollback restores `<unit>.prev` and reloads before restarting the previous binary.
Before the binary swap, a `journal` phase runs the same journald namespace sync as the installer (Run And Install Service § Journal namespace); a failed sync removes `go-trader.new` and stops the update, so the binary, the unit and the running service stay unchanged.
Drop-ins under `<unit>.d/` are never written — `scripts/merge-paper-instance.sh` and `scripts/egress-install.sh` own those.
For a unit with an egress drop-in (`NetworkNamespacePath` plus `GO_TRADER_EGRESS_DEPLOYMENT`), the update must run as root: it builds the egress controller from the same exported sources into a root-owned staging directory, runs the probe as the unit's user with the unit's environment files, switches `go-trader-egress@<deployment>` to the new controller (the previous one kept as `.prev`) before the app restart, restarts a running `go-trader-notify@<deployment>` broker after each controller restart, and restores both on rollback (§ Egress Proxy Mode).
A unit name this repo does not ship is logged and skipped, never guessed; a fragment that is a symlink whose target differs is left to the operator; a unit loaded from outside `/etc/systemd/system` (vendor or generator) stops the update before the swap.
`scripts/install-service.sh` is still the first-install path: it also creates `logs/`, enables the unit, and starts it.

**Root on a tree another account owns.** A tree that `scripts/migrate-service-layout.py` or `scripts/shared-feed-convert.sh` gave to a service account can be updated by root (`sudo bash scripts/update.sh --restart`, `--all --restart`, `--rsync-from`) with no git setting.
Every git call in `update.sh` and `shared-feed-convert.sh` goes through `update_git` in `scripts/update_helpers.sh`.
When root runs git on a checkout whose top directory or `.git` another account owns, that one command trusts only that exact checkout (`safe.directory` for its path, with `core.fsmonitor` and hooks off), passed in the command's environment.
Nothing is written to any git config, so do not add a root-wide `safe.directory`.
Every account other than root that owns the top directory or `.git` must pass this check, and a top and a `.git` that two different accounts own are refused.
Root builds and runs code from such a tree only when every systemd unit that runs as its owner (loaded or only installed as a unit file; a template is read with a placeholder instance) has `ProtectSystem=strict` and no write path (`ReadWritePaths`, `BindPaths`) over the tree root or inside the tree outside `scheduler/` and `logs/`, which is what the `go-trader@.service` template gives.
Otherwise the update stops before any change and lists each unit and write path, because the owner could then change git settings, the venv or the tracked scripts that root runs.
This also applies under `sudo` when the invoking account owns the tree, because a service running as that account cannot use its `sudo`; run the update as that account without `sudo` instead.
Without systemd, root does not update such a tree.
The probe of the new binary runs as the tree owner under the template sandbox (`systemd-run`, `ProtectSystem=strict`, writes only in `scheduler/` and `logs/`) on a private copy of `scheduler/config.json`, because the owner can change that config and the scripts it names; `shared-feed-convert.sh` probes a feed tree that the feed account owns the same way.
The Go build never compiles untracked files: `update.sh` exports the committed `scheduler/` sources of the checked-out commit (`git archive`) into a root-owned temporary directory and builds there with `GOWORK=off` and `-mod=readonly`, so a `.go` file or `go.work` that the service writes into `scheduler/` is never built.
With `--rsync-from`, the export comes from the source's `HEAD` when the source has no tracked changes.
A source that another account owns and that has tracked changes, or that is not a git checkout, is refused; a root-owned one builds from its own working tree, in the source's `scheduler/` with `GOWORK=off` and `-mod=readonly`, never in the deployment's copy, which the deployment's service can write.
When the files already match a release, `git reset <release>` moves `HEAD` without touching them.
As root, `uv sync --no-dev` runs with `UV_LINK_MODE=copy`, so the venv never shares files with root's uv cache.
After `uv sync --no-dev` as root, the owner must still run `.venv/bin/python3`, or the update stops before the swap.
Before any write, the update lists the paths in the tree that root already owns.
After `uv sync --no-dev`, after the build (so the owner can run the probe and the venv under any root umask), before the swap, and again when the script exits (also after a rollback), every other root-owned path, which this update wrote, is given back to the owner of the tree's top directory, so the owner can update the tree later.
When root owns the top directory and another account owns `.git`, the give-back covers `.git` for that account.
The give-back opens each directory without following symlinks and changes each owner through a handle to that entry, so a directory swapped for a symlink during the walk is never followed.
A file that shares its data with another path (a hard link) keeps its owner and is named, so a give-back never changes a file outside the tree.
The same rule applies when `shared-feed-convert.sh` gives a feed tree to the consumer user.
A path that root owned before the update stays as it is, also after a rename, because the list records each file's identity (device, inode and modification time) with its path; the update names examples and the `find <tree> -xdev -user root` command, so the operator can give only the paths the owner needs.
An rsync as root copies no owners from the source.
The build version comes from the tree, or from the `--rsync-from` source when that source is a git checkout; if git cannot read it the update stops and never stamps `dev`.
A git failure names the tree, its owner, the account that ran git, and the git error.
`scripts/test_migrate_service_layout_fixture.sh` (scenario `update`; Linux systemd host, run by a `shell-suites-migrate-<n>` CI job) proves the update, the rollback, the feed build and `--all` on such trees.

**`--rsync-from <src>`** replaces `git pull --ff-only` with an rsync from a source clone.
It keeps the deployment `.git/` frozen, and one shared exclusion builder (`update_managed_rsync_excludes`) protects `.git/`, `.env`, `scheduler/config.json` (and any in-tree symlink target), `tuning_runs/` and `ohlcv_cache.sqlite3*` next to the resolved config, configured state DBs (`db_file`, `paper_db_file`, `paper_sources[].db_file`, `replay_log_path`) and their WAL/lock sidecars, `.venv/`, `logs/` (and any custom `log_dir`), `robinhood-session/`, binaries, and the signal log across forward rsync, snapshot, and restore.
Use it when the deployment directory has local changes or was not cloned from origin.
Before the restart it warns on stderr about any required `EnvironmentFile=` the unit declares but the disk does not have; optional entries prefixed with `-` are skipped silently.

**Pre-update snapshot recovery.** Both git mode and `--rsync-from` mode recover from a protected pre-update snapshot taken before any write (`update_take_snapshot`), never from `git reset --hard` or from the deployment's `.git/` commit.
- The recovery root is `/var/lib/go-trader-update/<key>/` (0700 snapshot directory inside 0755 `/var/lib/go-trader-update`, root-owned) when run as root, or `${XDG_STATE_HOME:-$HOME/.local/state}/go-trader-update/<key>/` (0700) when run as non-root (`GO_TRADER_UPDATE_RECOVERY_DIR` overrides it for tests), where `<key>` is derived from the canonical deployment path.
- Before any write, `update.sh` checks that `rsync` is installed and compatible, verifies that free space in the recovery root exceeds the snapshot size (managed set, `.venv`, binaries, git index) plus 10 percent, and checks all candidate recovery roots for that deployment (system root, caller root, and tree-owner root) for an incomplete-recovery marker or a live `<key>.lock` (taken after the `--all` dispatch so a batch parent never blocks its children; a live holder refuses a concurrent update across accounts, and a dead holder's lock is reclaimed).
- The snapshot copies the managed code set (tracked edits and untracked files, without hard-linking to live files), the complete `.venv/` (or records its absence), `go-trader` and `go-trader.prev` (independent of `go-trader.prev` during the swap), git `HEAD` (symbolic or detached) and `.git/index` in git mode, and the installed systemd unit, journald namespace config, and egress controller files before any mutation.
- One exit coordinator (`update_exit_coordinator`) owns every failure after mutation starts (explicit `fail`, non-zero exit under `set -euo pipefail`, and catchable `SIGTERM`, `SIGINT`, `SIGHUP`, including `--all` forwarding `SIGINT` as `SIGTERM` to its child and Discord auto-update timeouts sending `SIGTERM` without killing recovery).
- On failure, the coordinator stops any running transfer or dependency child, stops the service first only if a restart was already attempted (leaving a healthy running trader untouched on pre-swap failures and on post-swap failures before the restart step), restores managed files with `rsync -a -c --delete` using the frozen exclusion list, restores git `HEAD` and `.git/index` in git mode while keeping staged and unstaged edits, atomically swaps the saved `.venv/` back in place via a sibling directory on the same filesystem, restores the binaries and any modified unit, journald, or egress controller files, restores file ownership, runs the restored binary's `probe` as the service owner under the unit sandbox, restarts the trader if it was stopped, and verifies `/health` for the previous version.
- The snapshot is removed only after file, `.venv`, ownership, probe, and service checks all pass; a recovered failed update still exits non-zero.
- If any recovery stage fails (including a failed post-recovery probe, which blocks starting the trader), the snapshot and `manifest.env` are kept, an incomplete-recovery marker `<recovery-root>/<key>.incomplete` recording the deployment directory's device, inode, and owner UID is written, and later updates of that directory by any account are refused until an operator inspects the deployment and runs `bash scripts/update.sh --clear-recovery` (`sudo bash scripts/update.sh --clear-recovery` for a root marker; a marker whose recorded directory no longer exists at that path or inode does not block and prints its location).
- Automatic recovery covers failures and catchable signals (`SIGTERM`, `SIGINT`, `SIGHUP`); `SIGKILL` and power loss get no automatic recovery, and the next `update.sh` run detects the unfinished snapshot (`COMPLETE` with recorded mutation intents), writes `<key>.incomplete`, preserves the snapshot, and refuses until cleared with `--clear-recovery`.

**Signal mode** (`--restart-mode signal` or `RESTART_MODE=signal`) SIGTERMs the PID in `GO_TRADER_PIDFILE` (default `./go-trader.pid`), respawns through `GO_TRADER_RUN_SH` (default `./run.sh`), then polls `/health` and PID freshness with the same verify-and-rollback flow as systemd mode.
Generate a starter `run.sh` with `bash scripts/create-run-sh.sh`.
Other signal-mode variables: `GO_TRADER_SIGNAL_LOG`.
When systemd mode meets a missing unit (systemctl exit 5), update.sh retries in signal mode automatically if `go-trader.pid` and an executable `run.sh` are present.
Relative `GO_TRADER_PIDFILE`, `GO_TRADER_RUN_SH` and `GO_TRADER_PROC_ROOT` resolve from the deployment root.
With restart enabled (`--restart` or `RESTART=1`), preflight runs one read-only restart-route check (`update_restart_owner_decision`, #1979) before the recovery lock, snapshot, transfer, dependency sync and build.
An explicit signal request first takes the existing redirect to systemd when an active unit runs this deployment's binary.
The check returns `no-owner` only on proof: a systemd unit that a successful probe reports `LoadState=not-found` with no fragment path (or an effective signal route), plus a missing `<proc root>/self`, a missing pidfile, or a missing or non-executable wrapper outside the deployment tree.
On `no-owner` the update fails in phase `preflight`, and nothing changes: no lock, snapshot or incomplete-recovery marker.
Before that refusal it runs the existing incomplete-recovery marker check (`update_check_incomplete_marker`); a blocking marker fails the update with the marker message, as before #1979.
A loaded unit (also inactive or inherited from the parent's service) and a masked unit (unit sync can replace an empty regular fragment, and the load state alone cannot tell that from a symlink mask) are `owned`.
These continue unchanged as `unknown`: no `systemctl`, a failed, empty or malformed probe, an uncertain redirect probe, any other load state (`bad-setting`, `error`, not-found with a fragment path), and a wrapper inside the tree, which the code transfer can supply or repair.
A code-only update (no restart, including the in-process auto-updater) never runs the check.

**Batch mode** (`--all`, which requires restart) discovers deployments from the systemd `WorkingDirectory` of every ACTIVE `go-trader`, `go-trader-*`, and `go-trader@*` unit and every ENABLED unit whose config sets `role: feed`, unioned with the `<root>/go-trader-*/` glob, where the root defaults to the parent of the invoking checkout; siblings need not share a parent directory.
Feeds run first, then schedulers, one at a time.
`--update-all-root <dir>` or `GO_TRADER_UPDATE_ALL_ROOT` pins the glob root and skips systemd discovery; the per-directory unit map still applies.
Each child resolves `GO_TRADER_SERVICE` from the active unit that owns its `WorkingDirectory`; a directory no active unit owns falls back to the parent's service and logs a warning.
Before it starts a child, the parent runs the signal-mode restart-route check above in that deployment's context (its git root, resolved unit, restart mode and paths).
On `no-owner` it logs `WARNING: skipping <dir> (restart route cannot succeed: ...)` on stderr with the route, load state and failed prerequisite, counts the directory as skipped, and changes nothing in it; the child still runs its own check.
A `no-owner` deployment that a blocking incomplete-recovery marker holds is not skipped: the parent logs `FAILED in <dir> (incomplete recovery marker blocks update ...)` and counts a failing instance, so the batch exits non-zero, as before #1979; a stale marker does not block, and the deployment is skipped.
Directories that are gone or have no `scheduler/config.json` are also skipped.
Exit status (changed in #1979): the batch exits 0 when at least one deployment updates and no child fails, even with skips, and prints a WARNING with the count of restart-route skips; any failing child still exits non-zero; a batch that updates nothing still fails with `updated 0 deployments` and names both skip reasons.
Cron jobs or alerts that read the exit status no longer see a failure for a skipped deployment, so read the WARNING lines.

Verify: `journalctl --namespace=+go-trader -u go-trader -f | grep -i "\[update\]"` (systemd) or `tail -f ./go-trader-signal.log` (signal mode).

**Preflight audits before a fleet update:**

```bash
bash scripts/check-config-versions.sh              # every deployment at or above the supported config floor
bash scripts/check-live-paper-config-drift.sh      # live/paper twin cadence + sizing drift
bash scripts/check-hl-stop-bankruptcy-bound.sh     # no HL stop sits past the isolated-margin bankruptcy distance
```

Each one auto-discovers active systemd deployments and accepts explicit deployment directories instead.
Each exits non-zero on a finding.
The drift audit pairs a paper strategy with its live twin only through an explicit `replay_source_id`, an equal id across deployments, or a `-paper` suffix backed by `storage_strategy_id` naming the live id; a suffix alone prints `AMBIGUOUS` and gates.
A `-paper` alias whose base names no live strategy in the audited deployments prints `UNPAIRED (no live twin)` and does not gate, so a merged paper book is visible even before its live twin exists.
Both the audit and `merge-paper-instance.sh` import the `-paper` suffix rule from one shared source, `scripts/paper_alias.py`, so the audit reads the same names the merge composes; each script refuses with exit 2 when that file is absent.
A folded source's alias carries its id (`<base>-paper-<id>`, with the same numeric-suffix rule), and the audit derives the suffix from the strategy's `paper_source`, so a source strategy pairs with its live twin instead of reading as unpaired.
A reserved or malformed source id yields no suffix at all rather than a wrong pairing.
Where `<dir>/go-trader` exists it reads cadence and sizing from `inspect --all --json` so root defaults compare as effective values; otherwise the row carries a `RAW` marker.
Twins that differ in platform, type, symbol or timeframe print `INCOMPATIBLE` and are left alone.

---

## Post-Update Agent Protocol

When invoked after an update (manual `git pull`, auto-update restart, "I just updated", "what changed"), walk the operator through anything new commits change on their existing config, strategies, and open positions, and prompt before applying any opt-in. The binary's own migration DM only covers a small registered field set; newer config-version bumps and opt-ins land silently unless an agent surfaces them.

### Trigger

Run when ANY of:

- The operator says "I updated", "I just pulled", "what's new", or asks about migration.
- `git log -1 --format=%cI` is newer than the running binary's version (`./go-trader --version`, or `curl -s localhost:8099/health` → `version`).
- `git status` is clean and `git rev-list --count <running-version>..HEAD` > 0.

### Steps

1. **Identify the diff.** `git log --oneline <running-version>..HEAD -- scheduler/ shared_scripts/ shared_strategies/ platforms/`. If the running version is unknown, ask the operator, or fall back to the last 30 commits.
2. **Classify** each commit against the table below.
3. **Read current state.** Load the config and query the state DB:
   ```sql
   SELECT strategy_id, symbol, quantity, side FROM positions WHERE quantity > 0;
   SELECT strategy_id, id, underlying, option_type, strike, expiry, action, quantity FROM option_positions WHERE quantity > 0;
   ```
4. **Prompt per item.** Default to no change if declined. For runtime defaults, also offer to write the explicit opt-out value.
5. **Apply through SIGHUP-safe edits** when the field supports it (see Reconfiguration); otherwise require a full restart.
6. **Verify.** Tail the logs for `[reload]`. On rejection, show the reason and offer a restart.

### Required prompt template

> Change: `<short description>`
> Affects: `<strategy IDs>` (and any open positions: `<symbol qty side>`)
> Default if you do nothing: `<what happens silently>`
> Options: 1) accept the new default, 2) opt out by setting `<field> = <value>`, 3) opt in to the new feature with `<field> = <value>` (requires flat? Y/N).
> Your choice?

Never apply a runtime-default change silently when the operator has not been shown the affected strategies. "Auto" means an automatic JSON rewrite, not an automatic behavior change.

### Reference: classification table

When in doubt, treat a commit as a runtime default and prompt. Per-release narrative for every archived entry lives in [`docs/POST_UPDATE_HISTORY.md`](docs/POST_UPDATE_HISTORY.md); regenerate a fresh candidate list from `git log --oneline -50`.

- **Paper check children of a paper-only process run at a lower CPU priority (#1987, runtime default, opt-out `GO_TRADER_PAPER_NICE=0`, no config or schema change).** Before this change every Python child ran at the scheduler's own priority, so a large paper cycle competed for CPU with live work on the same host.
  Now a scheduler whose roster holds no live strategy runs its per-strategy check children and all-paper Hyperliquid batch children at the scheduler nice +10 (at most 19) and prints one `[priority]` line at startup. A process with any live strategy, and every side-effect, execute, regime, LLM, UI and probe child, keep the default priority (§ Paper CPU priority).
  Operator action: none. To opt out, set `GO_TRADER_PAPER_NICE=0` in the unit environment and restart. To check, run `ps -eLo pid,tid,ni,args | grep check_` during a paper cycle, and compare live cycle times and `hl-batch .* shared state failed` counts with the times before the update.

- **Operator paper close and the remove-strategy guard (#1984, runtime default, no opt-out, no config or schema change, new owner command).** Before this change no command could close a paper position of a non-manual strategy, and `remove-strategy` let the next restart delete open paper positions with no close trade.
  Now `/go-trader-paper-close <strategy> [symbol]` and the dashboard paper-close action book an `operator_paper_close` trade at a fresh mark, and `remove-strategy` refuses on Discord and the dashboard while the strategy holds any position or option-leg row, is not paused, is a replay mirror, or has a queued manual action or resting limit row (§ Discord Slash Commands, § Operator paper close).
  Operator action: pause a strategy and flatten it with `paper-close` (paper) or its live close command before you remove it. A new slash command can take about an hour to appear.

- **Hyperliquid API wallet expiry warnings and CRITICAL (#1983, runtime default, alerts only, no opt-out, no config or schema change, new startup probe).** Before this change an expired Hyperliquid API wallet made every live order and stop fail with only the generic `LIVE ORDER FAILED` alert, and nothing warned before expiry.
  Now a live Hyperliquid daemon checks the key at startup and hourly, warns 14, 7, 3 and 1 days before `validUntil`, and sends one `HL API WALLET CRITICAL` when Hyperliquid no longer knows the key or a signed action is rejected for it (see Hyperliquid API wallet lifetime). The check never exits 78.
  Operator action: none, unless a warning or CRITICAL arrives; then create a new API wallet, set `HYPERLIQUID_SECRET_KEY`, and restart. Deploy Go and Python together with `bash scripts/update.sh --restart` (a live Hyperliquid config now probes `fetch_hl_agent_status.py --probe-only`, and a stale Python tree exits 78).

- **Discord slash commands sync by name and the update restarts the notification broker (#1982, runtime default, no opt-out, no config change, operator action required).** Before this change one 33-character command name made Discord reject the whole bulk overwrite, so no go-trader command registered, and a global bulk overwrite deleted the commands of any other program that shares the bot token.
  Now `/go-trader-resolve-robinhood-order` is `/go-trader-resolve-rh-order`, and registration creates, edits or deletes only `go-trader-` commands at startup and every minute after (§ Discord Slash Commands). On a broker-mode host the update restarts `go-trader-notify@<name>.service` after the controller switch.
  Operator action: tell operators to use `/go-trader-resolve-rh-order`. When a broker-mode host was updated by a checkout without this change, run `sudo systemctl restart go-trader-notify@<name>.service` once; the next re-sync registers the commands.

- **Updates skip or refuse a deployment whose restart cannot succeed (#1979, update tooling, no opt-out, no config or schema change, exit-status change).** Before this change `update.sh --all --restart` rebuilt every discovered deployment, so a retained tree with no restart route (for example a not-found unit and no signal pidfile) was transferred, built and swapped before the restart failed, and a failed recovery could leave an incomplete-recovery marker that blocked later updates.
  Now a read-only restart-route check runs before any lock or snapshot: the batch parent skips a deployment proven unable to restart with a WARNING, and a single-folder restart update refuses it in preflight; uncertain results continue as before (§ Auto-Update, signal and batch mode).
  Operator action: a fleet batch with skips and no failing child now exits 0, so review the `WARNING: skipping` lines and repair the unit or pidfile, or retire the tree. The rule applies once the checkout that runs `--all` carries this change.

- **IBKR paper options: 0.1-coin contracts, independent marks, London cutoff and dated settlement (#1918, runtime default, no opt-out, no schema change, config refusal, new startup probes, operator action required).** Before this change a new IBKR option leg was booked at 1 coin, its mark needed a spot strategy's cycle price and used a fixed volatility of 0.80, it expired at 00:00 UTC and settled at the cycle spot, and entry expiries were today plus the target days (weekends included).
  Now a new IBKR BTC or ETH leg is 0.1 coin (CME Micro); an unstamped older IBKR leg keeps 1 coin and every positive stored size stays. Marks come from `fetch_option_marks.py --platform=ibkr` (BinanceUS spot and the entry's 14-day measured volatility, source `binanceus_black_scholes_hv14`) with no spot strategy; a missing volatility keeps the last mark with an error. Expiry is 16:00 Europe/London; from 16:05 London the leg cash-settles at the BinanceUS 15:30-16:00 London one-minute close VWAP (`binanceus_spot_vwap_1530_1600_london`, basis `spot_candle_close_vwap`, dated to the expiry), else it holds as settlement pending. New entries pick the nearest weekday candidate from a paper CME Micro calendar (UK and US holidays not modelled). § IBKR options calendar, marks and settlement.
  Operator action: before updating, run the new binary's `./go-trader inspect --config <copy>`; an IBKR options strategy on any underlying other than BTC or ETH is now refused, and a refused config cannot manage its open legs, so close or migrate those strategies first on the old binary. The proposed contract quantity does not change, so a new IBKR entry carries a tenth of the old notional per contract. Deploy Go and Python together with `bash scripts/update.sh --restart` (the new IBKR probes exit 78 on a stale Python tree).
- **Options entries book whole structures (#1919, runtime default, no opt-out, no config or schema change).** Before this change an options entry could replace an open leg with the same rounded key (losing its liability or asset), a strategy could book only some legs of a butterfly or strangle, and Python's 4-row cap ignored the proposed legs.
  Now Python and Go keep or drop each structure whole, refuse a repeated open contract, and hold at most 4 option rows after an entry; accepted entries are written by the next state save (§ Options entry structures). A legacy book with 4 or more rows takes no new entry until rows close or expire. Results before and after this change are not directly comparable.
  Operator action: none. Startup renames only legacy fractional-strike keys that map safely; ambiguous rows keep their ids with a `[state] WARN`. Go and Python change together: deploy only with `bash scripts/update.sh --restart` (a stale Python tree sends no `structure_contract`, so every entry is refused with a script-failure alert).
- **Robinhood spot coins are crypto only and options underlyings are stock only (#1909, runtime default, config refusal, schema migration, operator action required).** Before this change a Robinhood spot coin outside the 10-entry Yahoo map was read as a bare Yahoo ticker and, logged in, as a stock quote, so a coin that shares letters with a stock or ETF (LTC, BCH, XRP, COMP) could take stock prices and candles into signals, paper fills and marks; and a Robinhood options underlying that matched the map was refused. Now spot means crypto and options means stock (§ Robinhood asset classes): 59 verified coins are supported (MATIC removed, no substitute), every loader refuses any other spot coin, and regime history, transitions and reversal signatures carry the class.
  Before updating: back up every state file (primary, `paper_db_file`, each `paper_sources[].db_file`), then run the new build's read-only check on a config copy (`./go-trader inspect --config <copy> --all`); the old binary cannot prove the new list. For each refused strategy, resolve any open position while the old binary still runs it (a live Robinhood book closes through the strategy's own sell or the kill switch, whose emergency closer `newRobinhoodEmergencyCloser` only serves configured live owners; then confirm flat with `./go-trader storage-inspect`), and only then remove or change the strategy. `force-close` is Hyperliquid-only. A refused strategy left in the config stops the update: `scripts/update.sh` probes the new binary before the swap and keeps the running service unchanged when it refuses. A container image has no pre-swap probe, so close those positions and fix the config before the image restart.
  Paper strategies on a coin outside the old map (for example XRP or LTC) switch from bare-ticker data to the verified crypto ticker; earlier paper history may hold stock-priced signals, but the symbol alone does not prove it. The first open migrates the regime tables in one transaction (verify on copies first); earlier Robinhood rows stay visible as `robinhood:unclassified`, never seed the new classes and age out under `regime.transitions.retention_days`. Ledger export and capture load the configuration through the same admission, so a pre-update snapshot that names an unsupported Robinhood spot coin is refused. An older binary on a migrated file reads both classes as one history again and cannot write reversal signatures (its conflict key no longer matches); restore the state backup for a full rollback. Go and Python change together: deploy only with `bash scripts/update.sh --restart` or the documented image update.
- **Option expiry trade rows, risk results and settlement styles (#1917, runtime default, no opt-out, no config or schema change, operator action required).** Before this change an expired option wrote no closing option trade and no loss-control result, a bought leg in the money was labelled `expired_worthless`, every platform delivered underlying units at the strike for an in-the-money sold leg, and delivered units carried strike cost, so option and underlying records did not add up to the cash result.
  Now each expired leg writes one closing `options` trade, one closed record (`expired_itm` when it has intrinsic) and one `RecordTradeResult`, so daily-loss holds and the loss-streak breaker see expiry results. Deribit, OKX and IBKR settle in cash only; Robinhood delivers at the settlement price (§ Option marks and settlement).
  Operator action: startup prints one `[WARN] state: ... holds a legacy long ...` line per cash-platform options strategy that still holds a long in its underlying from an earlier delivery. The position and historical rows are kept; tell the operator that a cycle with no price for that symbol values it at cost basis, that a wheel with such a long can still sell calls, and that closing or keeping it is their decision. Expect expiry losses to count toward `portfolio_risk` daily loss and the circuit-breaker loss streak from the first expiry after the update. Deploy with `bash scripts/update.sh --restart`.
- **Robinhood order recovery uses a `ref_id` match found before the page limit (#1915, runtime default, no opt-out, no config change, Python only).** Before this change a held live Robinhood attempt with no venue order id stayed `unknown` when the order list had more than 20 pages, even after the scan found the attempt's exact `ref_id` on an earlier page, so the coin stayed held (also for a kill-switch or circuit-breaker sell) until an operator acted.
  Now the scan finishes the page that holds an exact `ref_id` match, requests no further list page, and polls that order through the existing checks (order id, `ref_id`, side and account agree; a fill needs a finite price and quantity). Two exact matches on one page stay ambiguous and held. A field-only match and a `not_found` result still need the complete list, and a `ref_id` only on page 21 or later stays held.
  Limits: a second order with the same `ref_id` on a later page is not read, so it no longer makes the result ambiguous. A matching page whose `next` link repeats an earlier page now resolves, where before the repeated link held the attempt; a repeated link before a match still holds.
  The change assumes the venue keeps one order per `ref_id`. The scheduler sends at most one possibly delivered order POST per attempt (random UUID `ref_id`, attempt saved before the send, resend only after a pre-send failure), but venue-wide uniqueness is not verified, and other clients or direct script runs are outside that proof.
  Before the update, tell operators with live Robinhood strategies, run `./go-trader storage-inspect`, and name each unresolved attempt with no venue order id: the first recovery cycle after the update can settle it when its `ref_id` is on a page within the limit. Only the Python adapter changes; deploy with `bash scripts/update.sh --restart`.
- **Option marks, expiry holds and harvest eligibility (#1912, runtime default, new columns, new startup probe, config refusal, operator action required).** Robinhood option legs were marked by the Deribit pricer (`<UNDERLYING>-PERPETUAL`), so their value and days to expiry (DTE) never changed, and a failed expiry spot read closed every leg at zero intrinsic. Now each platform has its own mark and settlement source (§ Option marks and settlement), DTE advances from a stored cutoff even when a read fails, an expired leg with no valid settlement price stays open with `settlement pending: <reason>`, and theta harvest needs a successful mark in the current process.
  Before updating, check the config with `./go-trader inspect --config <copy>` on the new binary: every loader now refuses an options strategy on a platform outside `deribit`, `ibkr`, `okx` and `robinhood`, a Deribit or OKX underlying other than BTC or ETH, and a Robinhood underlying that resolves to a Yahoo crypto pair (`BTC`, `SOL`, any `-USD` symbol). Close or migrate the open legs of such a strategy first (step 3 `option_positions` query); a refused strategy does not start and its legs stay in the state file without an owner.
  After the update: expect no harvest closes in the first options cycle after every restart, `OPTION MARKS FAILING` alerts where reads fail, and CRITICAL `OPTION SETTLEMENT PENDING` alerts for expired legs whose price is missing after the read boundary. Five `option_positions` columns are added by the idempotent migration. Go and Python change together (new `exchange-calendars` dependency, new `shared_scripts/fetch_option_marks.py`): deploy only with `bash scripts/update.sh --restart`; a stale Python tree fails the startup probe with exit 78.
- **Live Robinhood buys send a budget-bounded limit and sells floor their price (#1910, runtime default, no opt-out, no config change).** Before this change a live buy rounded the coin quantity to the nearest value, so it could request up to about 3% more coin than its budget paid for, and a live sell rounded the bid to the nearest value, which could send a price above the bid or zero.
  Now a live buy reads the pair's `min_order_price_increment` and quantity rules and sends a `gtc` limit at the ask plus 0.5 percent (a design choice with no venue measurement behind it), rounded up to the price increment, for the budget divided by that limit, floored to the quantity increment and capped at `max_order_size`; the quantity times the limit is at or below the budget before fees, in exact decimal math. A missing or invalid price increment or quantity rule, an unreadable ask, a quantity below the minimum, an inexact wire number or too little remaining time after the account reads refuses the buy before the send with a **LIVE ORDER FAILED** alert.
  Every live sell (signal, kill switch, circuit breaker) keeps its quantity, market type and refusals and floors only its price (§ Custom Platform Integration step 4). Fees and venue results can still latch the cash (§ Cash Reconcile Latch), and a limit that rests or partly fills follows the existing open and partial-fill handling.
  Open venue facts: whether the live API always supplies `min_order_price_increment` (if it does not, every live buy refuses with an alert until a fix), whether the venue enforces the buy limit, what the price of a market sell means to the venue, and whether an off-grid fallback price is accepted. After an update, tell operators with live Robinhood strategies and watch the first live buy for a refusal. Go and Python change together: deploy only with `bash scripts/update.sh --restart`.
- **Option contract size, collateral and coverage (#1911, runtime default, startup stamp, operator action required).** Robinhood option entry premiums were 100-share contract prices while collateral, marks, expiry, assignment and risk used one share, so paper books could sell puts on 1% of the needed cash and credit strike proceeds for shares never held.
  Now every option leg carries a contract size (Robinhood 100, Deribit, OKX and IBKR 1; § Options contract size), short-put collateral is reserved, Robinhood short calls need free held shares, and a quantity above 1 scales marks, fees and trade quantities on every platform. Existing legs are stamped from their platform at startup; recorded cash, entry premiums and assigned shares are not changed.
  Operator action: with `option_positions` from step 3, compare each strategy's open short puts (strike × 100 × quantity on Robinhood) and Robinhood short calls (100 × quantity shares) with its cash and held shares. An undercollateralized put or uncovered call stays open, but new entries hold (INFO `Insufficient collateral` / `Uncovered short call refused`) until it closes or expires; an uncovered call that expires in the money now debits its intrinsic and credits no strike proceeds for missing shares on every platform, and paper options cash may stay negative after restart.
  Earlier Robinhood premiums booked at 100x against one-share marks cannot be corrected from the new field; start the paper strategy under a new id when a clean history is needed. Make each options strategy's `platform` match its `--platform=` argument, or every entry and close holds with a `Script contract failure`. A negative or non-finite stored size refuses startup and names the leg; fix the row from a backup and keep the row.
  Go and Python change together: deploy only with `bash scripts/update.sh --restart`.
- **Robinhood books are bound to their account (#1921, runtime default, startup backfill, no config change).** Before this change an open live Robinhood book had no account binding, so after a `ROBINHOOD_USERNAME` change the kill switch or circuit breaker could clear the old book with no sale or sell the new account's coins into it. Now each live book records its account; books that cannot be proven to belong to the logged-in account are held (no sale, no flat reconciliation) with a **ROBINHOOD BOOK HELD** owner DM (§ Robinhood account binding).
  After the update, read the `[robinhood-account]` startup lines: list every book in state `unbound` or `foreign` to the operator and offer `/go-trader-resolve-rh-account <strategy> bind-current` (coins on this account) or `release-book` (coins outside every managed account). Keep the original Robinhood credentials until this update succeeds; the previous `update.sh` cannot block a rollback. Deploy only with `bash scripts/update.sh --restart` (new script argv).
- **Options entries refuse missing market data (#1920, runtime default, no opt-out, no config change).** Before this change a failed or short daily-data read gave the options check a default volatility (0.30 on Robinhood, 0.60 elsewhere) and a rank of 50, missing venue quotes fell back to a model price, 5% of spot or zero, and several errors exited 0 or looked like a normal hold, so paper option legs could open at invented premiums with no alert.
  Now each pricing input that a strategy needs must be measured (§ Options market data): Deribit, OKX and logged-in Robinhood price only from a venue quote, IBKR and logged-out Robinhood only from measured volatility, and `vol_mean_reversion` and `butterfly` only with a measured rank. A partial result shows `IV rank=unavailable`.
  Every handled error exits 1 with one JSON document; ordinary failures alert at 3 consecutive failures and upstream 429/5xx at 15 failures or 75 minutes, as before. `check_options.py` now refuses platforms other than `deribit`, `ibkr`, `okx` and `robinhood`.
  Eligible theta-harvest exits now run (and reach trade alerts and channel totals) when the entry check fails. Expect fewer paper option entries during data outages and more script-failure alerts where data was silently defaulted before.
  Go and Python change together: deploy only with `bash scripts/update.sh --restart`. Tell operators with options strategies; there is nothing to opt out of.
- **One Robinhood coin identity (#1908, runtime default, startup state migration, operator action required).** A Robinhood spot `args[1]` is now trimmed and uppercased at config load, and startup renames matching lowercase books, pending circuit closes, shared-close notices, unresolved order attempts and entry decisions before any other write (§ Robinhood coin identity). Before this change a lowercase coin let the kill switch and circuit-breaker close clear a held book with no sale.
  Before updating, list every Robinhood spot strategy whose `args[1]` is not uppercase and write the uppercase spelling into the config right before `bash scripts/update.sh --restart`; the previous `update.sh` that runs this deploy cannot block a rollback to the older binary, which would read a migrated book as flat.
  After the update, confirm one `[storage] Robinhood coin identity migrated` line per converted book and no `[config] WARN: … robinhood coin` line. An exit 80 refusal names conflicting rows: review them against the venue and a backup, never delete a row to pass, and start again; a partial migration finishes on the next start.
- **Operator-required circuit-breaker warning is throttled and clearable (#1824, runtime default, no opt-out, no config change, no new column).** When a live OKX spot strategy fires its circuit breaker (or a live Robinhood options strategy did, before #1823 made options paper only), the scheduler cannot close the venue position, so it keeps an operator-required pending entry and warns the operator.
  Before this change that warning posted to every channel and sent an owner DM on every cycle with due work until a portfolio kill switch fired; circuit-breaker expiry never cleared it.
  Now the warning repeats once per `alert_throttle_interval` (default 6h) and at once after a new contributing fire; the CRITICAL log line stays on every cycle. A fire with a flat own book adds no leg, and a later fire merges into the earlier entry instead of replacing it. Clear an entry with the new owner-DM command `/go-trader-clear-operator-close <strategy> [platform]` after you flatten it on the venue (§ Operator-Required Circuit Breakers); the command registers at startup.
  Existing entries send one warning on the first drain after the update and then follow the throttle. Legacy size-0 legs stay until you clear them. The new `episode` and `fired_at` keys live inside `risk_pending_circuit_closes_json`; an older binary ignores them.
  Go-only change; deploy with `bash scripts/update.sh --restart` as usual. After an update, tell operators with live OKX spot strategies or a saved `robinhood_options` entry; there is nothing to opt out of.
- **Options strategies run as paper only (#1823, runtime default, operator action required).** A `type: options` strategy with `--mode=live` or `--mode live` used to book simulated option trades in the live scope; no option order reached Robinhood, Deribit, IBKR or OKX, and its P&L fed live portfolio risk.
  Every config load (startup, SIGHUP, `probe`, `inspect`, `storage-inspect`, ledger export and every Discord and dashboard config write) now refuses it on every platform, with or without credentials, and `paper-to-live` refuses every options strategy.
  `check_options.py` refuses the same live forms with a JSON `error` and exit 1, and now reads its stdin positions when the args hold `--mode=paper` or `--mode paper` (before, either form dropped them, so the 4-position cap saw an empty book).
  Operator action: list each options strategy with a live mode, back up every state file, then remove it or give it a fresh `id` on `--mode=paper`; never keep the old `id` (split storage then exits 80, single-file storage resumes the phantom book in the paper scope and leaves the live peak unchanged).
  Full steps and effects: docs/POST_UPDATE_HISTORY.md, Runtime default, #1823.
- **Basket and pair research removed (#1955, operator action only for configs with `baskets`, `pairs`, `basket_id` or `pair_id`).** The unused basket coordinator, research pairs, their replays, `basket-replay`, `basket-replay-paper` and `/resolve-pair-native` are gone; the code is kept in git tag `archive/basket-pair-research`.
  Every config load (startup, SIGHUP, `probe`, `inspect`, `storage-inspect`, ledger export, feed consumer) now refuses a root `baskets` or `pairs` key, a strategy `basket_id` or `pair_id`, the `basket_member`/`pair_member` tokens and the `--basket-member`, `--pair-member-json` and `--management-stdin` args, before any config migration; `scripts/update.sh` probes the new binary against the deployed config and refuses the swap, so such a deployment keeps running the old binary until the operator acts.
  Ordinary configs need no action: valuation, risk, kill switch, fees and close output are unchanged (the HL and OKX close JSON drops the pair-only `close.evidence` key, and the OKX close no longer reads order history after it places the order). `/api/status` drops the always-zero `portfolio_risk_by_scope.*.pair_valuation_incomplete_since` and the empty `pair_*` fields; `storage-inspect --json` drops the basket and pair counter keys.
  Operator action for a paper deployment that ran baskets: back up every state file (`db_file`, `paper_db_file`, each `paper_sources[].db_file`), for example `sqlite3 <db> ".backup '<db>.pre-1955'"`, then either remove `baskets` and the member strategies (their paper books are pruned on the next save, as for any removed strategy) or re-add the members under fresh ids. Never reuse a former member id: its unresolved retired entry intent loads and holds new entries, and recovery and `/resolve-entry` refuse it. Retired tables and rows stay in the file untouched.
  Go and Python change together (the probe and check-script argv drop the member flags): deploy only with `bash scripts/update.sh --restart`.
- **Robinhood shared-coin circuit-breaker DM is throttled (#1817, runtime default, no opt-out, no config change).** When a live Robinhood strategy latches its circuit breaker (CB) on a coin that another live Robinhood strategy also trades, go-trader still sends no close for that coin and tells the owner by DM.
  Before this change that DM went out on every cycle until the CB cooldown ended (about 288 DMs for one 24-hour drawdown latch at a 5-minute cycle).
  Now the owner gets it once for each CB episode and coin, then again only after `alert_throttle_interval` (default 6h) while the CB stays latched; the CRITICAL log line stays on every cycle. A new CB episode notifies at once.
  The marker is stored per strategy in the new `strategies.risk_rh_shared_close_notice_json` column (idempotent migration), so a restart does not resend it; it clears when the CB unlatches, when the strategy's own book or the account is flat, when the coin has one live owner, and while the live kill switch fires. A failed save keeps the marker in memory, still sends the DM and retries with the cycle save.
  Go-only change; deploy with `bash scripts/update.sh --restart` as usual. An older binary ignores the column. After an update, tell operators with two or more live Robinhood strategies on one coin; there is nothing to opt out of.
- **Robinhood candles follow one timeframe table (#1822, runtime default, no opt-out).** Robinhood strategies now get Yahoo candles through one table (§ Robinhood candles): `2h`, `4h`, `6h`, `8h` and `12h` are built from Yahoo `1h` rows in UTC buckets, `1w` and `1M` map to `1wk` and `1mo`, and the Yahoo range follows `--ohlcv-limit`.
  Startup, SIGHUP and `probe` now refuse a Robinhood strategy whose `args` timeframe, HTF interval or `regime.timeframe` is not in the table, a `2h`, `6h`, `8h` or `12h` `args` timeframe or `regime.timeframe` for a symbol that is not a Yahoo crypto pair, and a `regime.timeframe` of `1M` when a Robinhood spot strategy uses the regime. Before updating, list every Robinhood strategy with its `args` timeframe, HTF (`htf_filter`) and `regime.timeframe`, and fix any refused value first; run `./go-trader probe --config <config>` to check.
  Effects: a Robinhood `1h` or `30m` strategy with `htf_filter` that held every entry since #1826 (its `4h` HTF had no rows) now filters on a real `4h` trend; `60m`, `1wk` and `1mo` strategies get more history (`60m` had a 5-day range), so paper results before and after are not comparable; an unsupported timeframe in the chart shows an error instead of an empty chart. The Robinhood options regime keeps its `4h` Yahoo series for stock underlyings.
  `closed_bar_decisions` is now accepted on Robinhood **paper** spot (opt-in, with the #1769 entry allowance, `5m` to `1d`, with a 20 s Yahoo settle hold after each boundary) and stays refused on live Robinhood, on `1m` and for a symbol that is not a Yahoo crypto pair (§ Closed-Bar Decisions). Go and Python change together, so deploy only with `bash scripts/update.sh --restart`.
- **Missing higher-timeframe input holds entries (#1826, runtime default, no opt-out, no config change).** A strategy with `htf_filter: true` (Hyperliquid perps, OKX spot and perps, Binance.US spot, Robinhood, TopStep) now holds new positions and additions whenever its higher-timeframe (HTF) candles are missing, fewer than 50, unreadable, non-finite or failed to fetch.
  Before this change the filter returned a neutral trend for that input, a neutral trend passes every signal, and the entry opened unfiltered with no warning.
  Each successful check now returns an `htf_filter` block (`ok`, `unavailable`, `skipped` or `not_evaluated`, with timeframe, trend and reason); a missing block or an unknown status is a script error (§ Higher-timeframe filter).
  A successful neutral trend still passes its signal. Closes, partial closes, a spot sell that reduces a long, stops, ratchet and protection continue while the input is unavailable.
  Repeated unavailable evaluations send an **HTF FILTER UNAVAILABLE** alert to the strategy's partition channels and the owner (3 evaluations, or 15 evaluations or 75 minutes for upstream 429/5xx errors), with the usual repeat throttle; the first successful evaluation after an alert sends one **HTF FILTER RECOVERED** message. The count is held in memory and starts at zero after a restart.
  Backtests (`run_backtest.py --htf-filter`) and the `/tuning` simulator first top up a short or non-covering HTF cache from the exchange, then refuse an HTF cache that is still empty, shorter than 50 rows, or not covering the requested range (`HTF FILTER REFUSED`) and save no result; before, they ran unfiltered or on a stale trend. The simulator's filtered path also works again with a valid cache (it failed on a time-zone mismatch).
  The Go result contract and the check scripts change together, so deploy only with `bash scripts/update.sh --restart`. This change does not depend on #1822 (Robinhood timeframe mapping); a Robinhood or TopStep strategy whose HTF interval the provider does not serve now holds entries and alerts instead of trading unfiltered.
  After an update, list every strategy with `htf_filter: true`, tell the operator that unavailable HTF input now holds its entries, and point to the `HTF filter: ... signal suppressed` log line and the alert; do not turn the filter off on the operator's behalf.
- **Egress proxy mode (#1841, opt-in, nothing changes unless `GO_TRADER_EGRESS_PROXY` is set).** A deployment can send every external connection through one proxy behind a root-owned egress controller that blocks every direct route (§ Egress Proxy Mode).
  With the variable unset, transports, child arguments and Python behavior are unchanged; `/status` and the feed `/status` gain an `egress` block that reads `{"mode": "disabled"}`.
  The Python environment gains `requests[socks]` (PySocks), and `shared_scripts/llm_review.py` now calls the API through a requests session instead of urllib (same response parsing and stdout contract; transport error names in the advisory `error` text can differ).
  An `egress` config key other than `proxy_required` or `notifications_direct`, or a non-boolean value, now fails the load, and a SIGHUP that changes the block is refused.
  Deploy only with `bash scripts/update.sh --restart` (Go and Python change together).
  After an update, tell the operator the mode exists and is off; do not turn it on without consent, because it needs the installer, a reachable proxy, and Linux with systemd 247+ or rootful Docker.
- **Robinhood logins keep one owner-only session beside the config (#1818, runtime default, no opt-out, no config change).** Every Robinhood login now stores and reuses its session only in `<config folder>/robinhood-session` (Secrets, Robinhood session), which go-trader sets at startup; the scripts no longer read or write `$HOME/.tokens`.
  Before this change a shipped unit with home `/nonexistent` failed every live Robinhood login, and a host that followed the #1816 HOME step sent a new password login with a new random device token whenever the cache could not be used.
  A legacy `$HOME/.tokens/robinhood.pickle` is not read, so the first login after the update uses a new device identity and can need the one-time bootstrap (`go-trader robinhood-login --config <config.json>`); a login that meets a device challenge now fails at once and stays latched until the bootstrap succeeds, and it never waits for a prompt.
  Operators can delete the old `$HOME/.tokens` cache after the bootstrap; the `HOME=` line that #1816 told them to add can stay for other per-user caches such as the yfinance cache, though Robinhood no longer uses it.
  The new `robinhood-login` subcommand and `GO_TRADER_ROBINHOOD_SESSION_DIR` change no probe argv, and `scripts/update.sh` keeps a `robinhood-session/` folder out of its rsync delete.
  After an update, tell operators with live or paper Robinhood strategies, show them the bootstrap command for their layout, and confirm the `[robinhood] session folder` startup line and that later cycles show no `Robinhood login failed`.
- **Robinhood emergency closes sell only the go-trader book (#1825, runtime default, no opt-out, no config change).** Before this change the kill switch and the circuit-breaker (CB) close sold the whole account balance of a coin, also coin the operator holds by hand in the same Robinhood account.
  Now each sells at most the remaining go-trader book for the coin (the kill switch sums every live Robinhood spot book; the CB uses the latched strategy's own book), capped by `quantity_available` through the same exact-decimal sizing as #1819 with close kind `emergency`, which never sweeps dust.
  Holdings above the book are the operator's: they are logged as CRITICAL and named in the kill-switch message, the reset DM and a CB owner DM, and are never sold. A coin that no configured strategy trades stops blocking kill-switch confirmation only when no Robinhood book (configured or not) holds it and no order for it is unresolved.
  A CB trip sets its pending close from the book at fire time (`size_basis: book`) and binds it to that position's `position_id`; a flat book sets none and later cycles send nothing, also while the account holds the coin. When that position is gone and the strategy holds a newer one (for example after the cooldown ended), the pending close is cleared and nothing is sold. A partial fill, a failed holdings read or an unsaved settlement keeps the pending close, and the next cycle asks only the unsold remainder, also after a restart.
  A coin counts as closed only when no order for it is unresolved after settlement, the settlement save succeeded, a strict holdings read succeeded, and the remaining book is at or below tolerance; or when that read shows no holding at all outside the 10-minute lag window, and the verified-flat cleanup closes the leftover book. `quantity_available = 0` alone never proves flat.
  Reserved holdings, an account shortfall and a remainder below the venue minimum or one increment hold the coin unconfirmed with the reason in the kill-switch message or the CB drain-failure DM; the operator sells by hand, resets the kill switch, or removes the strategy.
  A pending close from an earlier version (no `size_basis`) never sells above the book: while the breaker is still latched and the book is long it becomes a book-sized close bound to that position; with a flat book, or once the breaker is no longer latched, it sends nothing, sends one CRITICAL owner DM and clears. A fill above every book books what fits and sends one **ROBINHOOD SALE ABOVE THE BOOK** owner DM.
  Emergency sells freeze request version 3. The close script argv changed (required `--quantity`; a missing or invalid value or any argument error exits 1 with one JSON document and no login), so deploy only with `bash scripts/update.sh --restart`. Do not roll back while a Robinhood CB pending close or a version 3 attempt exists: an older binary cannot read version 3 and sells the account quantity.
  After an update, list every live Robinhood strategy and its open positions, and tell the operator that hand-held coin in the same account is no longer sold and is reported instead.
- **A failed Robinhood login stops live Robinhood work (#1816, runtime default, no opt-out, no config change).** A Robinhood login now succeeds only when robin_stocks returns a token, sets its login state, and sets the matching `Authorization` header.
  In live mode a failed login ends the signal check, execute, emergency close, position fetch, live strategy chart and balance check with an `error` that starts `Robinhood login failed:` and names the failed check, so there is no signal and no order, the kill switch stays unconfirmed, and the circuit-breaker pending close is kept until a login succeeds.
  Before this change the adapter stayed marked live after a failed login, signal checks ran on Yahoo prices, and orders failed with a library "can only be called when logged in" error.
  In paper mode a failed login leaves the adapter logged out, and it uses the public price and option fallbacks with no Robinhood call.
  robin_stocks login lines on stderr now pass through an allowlist with a `[robinhood] robin_stocks:` prefix, and other login text shows as `(detail withheld)`, so credentials, tokens and paths stay out of logs.
  The Robinhood token cache and HOME steps of this entry are replaced by the #1818 entry above (Secrets, Robinhood session).
  A `HOME=` line set for this entry can stay, because other per-user caches of the service, for example the yfinance cache, go under that HOME.
  After an update, tell operators with live or paper Robinhood strategies; there is nothing to opt out of.
- **Robinhood live sells use the account's sellable quantity (#1819, runtime default, no opt-out, no config change).** A live Robinhood signal sell now reads the coin's `quantity_available` and the pair's `min_order_size`, `min_order_quantity_increment` and `max_order_size` in the order subprocess, on the same login and under the coin lock, before it sends.
  It sends `min(asked, available)` floored to the increment with exact decimal math, and refuses with `not_sent`, no order and a **LIVE ORDER FAILED** alert when a read fails, a field is missing or invalid, nothing is available, or the result is below the minimum or above the maximum.
  Only a whole close by the sole configured live owner of the coin sells sub-minimum dust above the book (`0 < available - asked < min_order_size`); a peer strategy or a partial close never sweeps, and a whole close that leaves at least the minimum reports the residue at WARN.
  The book changes only by the confirmed fill and keeps every remainder; a residue below one increment closes at zero proceeds when the account held the full ask and the order filled completely.
  When the account holds less than the ask, the book keeps the unbacked part and a once-per-attempt **ROBINHOOD BOOK SHORTFALL** DM names the asked, available, sent and filled quantities; there is no automatic write-off.
  Signal sells are frozen as request version 2 with write-once `sizing` evidence, and recovery searches for the sent quantity, never the ask.
  If the live API omits `quantity_available` or a pair rule, every live signal sell refuses with a LIVE ORDER FAILED alert that names the field; since #1825 the kill-switch and circuit-breaker closes use the same rules with close kind `emergency` and refuse the same way.
  The order argv changed (`--quantity` text, `--close-kind`, `--sole-owner`), so deploy only with `bash scripts/update.sh --restart`.
  After an update, list every live Robinhood strategy with open positions and tell the operator that a position booked before #1815 can raise a shortfall alert on its first close.
- **Live Robinhood strategies start trading (#1814, runtime default, no opt-out, no config change).** Before this change, when `ROBINHOOD_USERNAME`, `ROBINHOOD_PASSWORD` and `ROBINHOOD_TOTP_SECRET` were set, robin_stocks login text went to stdout and every Robinhood script result failed to parse: Robinhood strategies got no signals, live ones placed no orders, and the kill switch and circuit-breaker close never sold Robinhood coin.
  Now each Robinhood script writes one JSON document, so with credentials set, live Robinhood strategies place real orders and the kill switch and circuit-breaker close sell.
  The live sell size defect is fixed: a live signal sell sends at most the account's sellable quantity (#1819).
  The emergency close size defect is fixed: the kill switch and circuit-breaker close sell at most the go-trader book and report holdings above it (#1825).
  After an update, use the required prompt template to list every live Robinhood strategy (`platform: robinhood` with `--mode=live` in `args`) and its open positions.
  To stop orders for a strategy: `paused: true` (hot-reloadable) stops new buys, but signal closes, the kill switch and the circuit-breaker close still sell its book; removing the strategy from the config (restart required; read its open positions first) stops all Robinhood orders for it.
  With no live Robinhood strategy, there is nothing to change; tell the operator that paper Robinhood strategies with credentials set now get signals again, so their paper results before and after this update are not comparable.
  This entry is the warning, because the update notice shows only commit titles.
- **Robinhood live orders book only confirmed venue fills (#1815, runtime default, no opt-out, no config change).** Every live Robinhood order (signal, circuit-breaker close, kill-switch close) is first saved in `robinhood_order_attempts` with a go-trader `ref_id`.
  The order script then polls the order to a final state within one budget, cancels it at the deadline and reports `order_outcome`.
  Only a confirmed fill (finite price and quantity above 0, an order id) changes the book, at the venue price and quantity, once; a partial sell keeps the unsold remainder.
  A missing, unknown, still-open or contradictory result books nothing and holds the coin: no Robinhood order for that coin is sent until recovery (every cycle) or `/resolve-rh-order` resolves it, with a **ROBINHOOD ORDER UNRESOLVED** CRITICAL DM.
  Circuit-breaker activation no longer clears a live Robinhood book at the mark; the circuit-breaker and kill-switch closes count a coin as closed only after settlement and a fresh strict holdings read prove the book sold (#1825 rule), and close a leftover book quantity with a `*_verified_flat` row only when that read shows no holding at all.
  A flat read within 10 minutes of the buy that opened a live Robinhood long does not confirm flat, because the holdings read can lag the fill; the close retries next cycle.
  A fill split across strategies that share a coin commits only with a full live partition save, so a per-strategy save cannot store part of it.
  Startup exits 80 when an unresolved attempt belongs to no configured live Robinhood spot strategy; `storage-inspect` lists every unresolved attempt.
  The Python order argv changed (`--ref-id`, `--budget-ms`), so deploy only with `bash scripts/update.sh --restart`.
  After an update, tell operators with live Robinhood strategies, run `./go-trader storage-inspect`, and name each unresolved attempt.
- **Robinhood position reads fail closed (#1813, runtime default, no opt-out, no config change).** The Robinhood position fetch and emergency close now read every holdings page with bounded requests and check each row. An incomplete or invalid read (HTTP error, failed later page, bad pagination, missing symbol or quantity, library not logged in) now exits 1 with an `error`, so the kill switch stays unconfirmed and the circuit-breaker pending close is kept, with a CRITICAL `rh-close` or `rh-circuit-close` log line each cycle until a read succeeds. Before this change, such a read could report a held coin as already flat. After an update, tell operators with live Robinhood strategies; there is nothing to opt out of.
- **Ratchet owner DM reports the booked stop (#1725, runtime default).** The trailing-ratchet owner DM now shows the stop recorded after the same-cycle trailing update, and it says when the venue result is unknown. Stops and orders are unchanged. No config change; `notify_ratchet_triggers: false` still disables the DM.
- **Docker image and Compose setup (#1734, internal for host deployments).** systemd and Linux process deployments keep their update, restart, bind and storage behavior. The new daemon flag `--status-bind` (and `GO_TRADER_STATUS_BIND`) accepts only loopback on a host, so a host config needs no change. Mention `docs/DOCKER.md` to an operator who wants a container; apply nothing.
- **`resting_tp_trade_through` (#1727, new opt-in field).** Dormant until set; with it off nothing changes and live is never affected. After an update, mention it and, only if the operator wants paper tier take-profits that need a one-tick trade-through instead of a mark touch, offer it on a new paper Hyperliquid perps strategy ID (paper tier results before and after are not comparable). Warn before the restart if a config names a custom check script, because the probe now sends `--resting-tp-rule-json`, and update a `market_feed: shared` feed service in the same release.
- **Live entry absence proof and `/resolve-entry` (#1771, runtime default for live `closed_bar_decisions` strategies, no opt-out).** Releasing a held live entry attempt because the venue shows no order now needs the saved venue account to match the queried account and a complete, successful fill-history check; a missing order alone, a flat position or an incomplete history keeps it held. New attempts save a non-secret account key (Hyperliquid network plus address; OKX network plus a SHA-256 fingerprint of the API key). After the update, run `./go-trader storage-inspect` and name each unresolved `[entry]` attempt saved before the update: it has no account key, so only a terminal order or a proven fill can resolve it, and `/resolve-entry` refuses it. Mention the new owner DM command. Deploy with `bash scripts/update.sh --restart`: the recovery probes now send `--since-ms` (OKX) and `--until-ms` (both), so a custom check script needs the flags.
- **Closed-bar entry allowance (#1769, runtime default for `closed_bar_decisions` strategies, no opt-out).** A strategy with `closed_bar_decisions: true` now takes at most one entry per decision candle and side, holds a reversal's opening leg until a later check from a flat book, and holds new live entries while an attempt's venue outcome is unresolved. After the update, list every `closed_bar_decisions` strategy, tell the operator each pre-existing one holds the first candle it observes (one candle of entries) and that paper results before and after are not comparable, and check the start log and `./go-trader storage-inspect` for `[entry]` unresolved lines. New state tables migrate on start; deploy with `bash scripts/update.sh --restart` (the probe now sends the new execute and recovery flags).
- **`closed_bar_decisions` (#1712, new opt-in field).** Dormant until set; with it off nothing changes. After an update, mention it and, only if the operator wants backtest-aligned decisions, offer it on a new paper strategy ID (entries can start up to one bar later). Warn before the restart if a config names a custom check script, because the probe now sends `--closed-bar-decisions` and `--decision-regime-timeframe`, and update a `market_feed: shared` feed service in the same release.
- **Hyperliquid perps `--config` backtests (#1728, research results only).** Live and paper trading are unchanged. After an update, tell the operator that a new `--config` backtest of every Hyperliquid perps strategy changes, including leverage 1 with no `margin_per_trade_usd` (fee on top of notional). It is not comparable with an older `--config` result for that strategy. `momentum_pro` with `vol_target_atr_pct` > 0 now refuses, because that setting emits `entry_fraction` and the live sizer has no such input. A backtest that does not load a config is unchanged. No config edit and no restart.
- **Paper Hyperliquid perps book venue lot sizes (#1716, runtime default, no opt-out).** Paper HL perps entries, flips, scale-in adds and partial closes (tier fills included) now floor to the coin's `szDecimals`, and hold with no book write when the floored order is 0 or below $10.30 or when the lot size is unknown. Full closes, stops and kill-switch flattening are unchanged. After an update, list every paper HL perps strategy and its open positions, tell the operator that paper results before and after this update are not comparable (recommend a new paper strategy ID when a clean comparison matters), and check the start log for `[hl-lot] ... unavailable` lines. No config, `state.db` or Python change; deploy with `bash scripts/update.sh --restart`.
- **Args parameter flags warn at load when `--strategy-refs` supersedes them (#1711, runtime default with no behavior shift).** After an update, run `./go-trader inspect --all` (or read the start log), list each strategy that shows the new `[WARN]` with the unused args entry, and prompt per strategy: delete the entry (trading unchanged), or move the value into `open_strategy.params` or the matching field (trading changes, so recommend a new paper strategy ID). Apply an approved edit only with a restart, because an args change blocks SIGHUP hot reload. Default if the operator declines: nothing changes, and the warning repeats on each start.

| Category | How to recognize it | What to do |
| --- | --- | --- |
| Auto-migration | `CurrentConfigVersion` bumped; the loader rewrites the JSON on next start | Summarize. No prompt. Warn that a rewrite replaces a still-symlinked `scheduler/config.json` with a regular file |
| Runtime default | Behavior shifts on existing strategies with no config edit | Prompt: confirm, or write the explicit opt-out |
| New opt-in field | The feature stays dormant until the field is set | Prompt per affected strategy |
| Open-position constraint | The change needs flat positions to apply | List affected strategies, warn, and skip until flat |
| Internal / no-op | Refactors, tests, docs, dashboard and formatting work | Mention briefly |

**Config-version floor.** `CurrentConfigVersion` is 20 and `MinSupportedConfigVersion` is 13 (`scheduler/config_migration.go`; `config_migration_test.go` pins the floor). A stamped `config_version` below that fails loudly at load instead of migrating. Run `scripts/check-config-versions.sh` and confirm the whole fleet is at or above the floor before raising it again.

**Opt-in fields** stay dormant until set. Adjustable Settings is the complete list, with each shape, default, and reload rule.

**Fields blocked while a position is open** (flatten first, or restart after the close):

- `margin_mode` and exchange `leverage`
- kill-switch identity changes
- `stop_loss_atr_mult` / `trailing_stop_atr_mult` nil↔positive toggles, and any scalar↔regime stop flip
- `invert_signal`
- `regime_side` and `allow_uncertified_regime_side`
- `regime_*_window` selectors, `regime_directional_policy`, `regime_window_divergence`, `regime_profile_allocation` shape changes
- `hedge` add, remove, or shape change (either leg open)
- `replay_sharing` toggle and `replay_source_id` changes (the paper book would desync from the log mid-trade)
- `atr_method`
- `allow_scale_in` and the `scale_in` block
- the unified per-regime close block, and any ratchet tier table

---

## Status

Default port `8099`. Override with `--status-port <port>` or `status_port` in config. If the port is busy the server tries the next five; the log names the one it took. The container binds `8099` exactly and refuses any other port (§ Docker Container).

```bash
curl -s localhost:8099/status | python3 -m json.tool
curl -s localhost:8099/health
curl -s localhost:8099/history
open http://localhost:8099/dashboard
```

`/health` returns 503 while the daemon drains, and when the main loop misses its own schedule: a pass of the loop (a cycle, reload or off-cycle audit) still running 30 minutes after it started, startup not reaching the loop within 30 minutes, or no wake 5 minutes after the time the scheduler planned. The age of the last cycle alone never makes it unhealthy, so a long `interval_seconds` stays healthy between cycles. Discord `/health` uses the same rule.

`/health` also carries `run_evidence`: the process start time, the time each strategy was last evaluated (`evaluated`), the strategies skipped at zero capital (`zero_capital_skipped`), the strategies a cycle held back with the reason and time (`held`: `portfolio_kill_switch` for a latched or fired scope kill switch, `save_blocked` after three failed saves), and `last_state_save`, the end of the last cycle whose save succeeded for every partition.
A hold replaces that strategy's `evaluated` time, and an evaluation replaces that strategy's hold.
At the end of a cycle every due strategy is recorded held or cleared.
A hold on a strategy that is not due is cleared on the next cycle whose partition is neither latched nor save-blocked, whether or not that strategy runs.
Every map is empty after a restart. When `STATUS_AUTH_TOKEN` is set, `/health` includes `run_evidence` only for a request with `Authorization: Bearer <token>`; `status`, `version` and `pid` stay open for monitors. `scripts/migrate-service-layout.py` uses it as its execution proof and sends the token read from the running process's environment (never written to its manifest or journal).

Dashboard JSON endpoints: `/api/strategies`, `/api/strategies/overview`, `/api/strategies/<id>/(candles|trades|status|equity|config|simulate)`, `/api/regime/transitions`, `/api/tuning/runs[/<id>]`. Candles and equity are cached 30 seconds. `config` (GET) and `simulate`/`config` (POST) need `status_token` plus a same-origin header; when `status_token` is set the dashboard page prompts for it and keeps it in browser local storage.

**Never expose the status port publicly.** On a host the server listens on loopback only: `--status-bind <host>` (or `GO_TRADER_STATUS_BIND`, default `localhost`; the flag wins) accepts only `localhost`, `127.0.0.0/8` or `::1`, and any other value stops the start. Do not rebind a host to `0.0.0.0`.
Only the Docker image (`GO_TRADER_RUNTIME=container`) may bind the wildcard address, and only with `STATUS_AUTH_TOKEN` set; Compose then publishes the port on host `127.0.0.1` only (§ Docker Container). Front each instance with [Tailscale Serve](https://tailscale.com/kb/1242/tailscale-serve) or another authenticated proxy on the same machine — for example `tailscale serve --bg --https=8443 http://127.0.0.1:8099`, then browse `https://<node>.tailnet.ts.net:8443/dashboard`. A common multi-instance port map (match each `status_port`): live `8099`, paper-testing `8100`, then `8101`+ per paper instance. After a paper instance is folded into the combined paper service (§ Storage Ownership), its port and its proxy or tunnel mapping are retired; the combined paper service's port serves every folded partition. Paper and live never share a service. An agent stack such as OpenClaw may serve its own dashboard on other ports; that UI is not go-trader's.

The dashboard also carries mutating controls behind a typed-confirmation nonce: pause/unpause and ratchet-notification toggles, trade actions (close, manual edits), and structural mutations (add/remove strategy, paper-to-live, apply-regime-gate).

`/tuning` is a read-and-launch research page for suggest-only per-strategy retunes. It re-reads live config on every poll and never writes config itself; `POST /api/tuning/apply` is the only promotion path.

If Discord is enabled, wait for the first cycle and confirm messages in the configured channels. Report success with mode, strategy count, status URL, and the log command.

---

## Discord Slash Commands

Global slash commands register at startup, covering every guild the bot is in plus DMs; a first-time command-shape change can take about an hour to propagate.
Registration never bulk-overwrites the command list: it reads the global list and creates, edits or deletes only `go-trader-` chat commands, so commands another program registered with the same bot token stay, and a renamed or removed go-trader command is deleted. The notification broker refuses a create, edit or delete outside the `go-trader-` namespace.
Every command sets its contexts, integration types and `nsfw`, so a sync can reset a field that something else changed; a command that holds a field a PATCH cannot clear (`default_member_permissions` or localizations) is deleted and created again. The list request asks Discord for full localizations (`with_localizations=true`), so a stored localization is visible to the compare.
After startup the sync repeats every minute and writes only on drift, so a global bulk overwrite by the other program is repaired within a minute; three repairs within 30 minutes send the owner one DM that names the shared bot token.
While repairs continue past that point, or Discord rate-limits a write, the interval doubles up to 30 minutes, and it returns to one minute only after a sync that writes nothing; the owner gets no second flap DM while the back-off holds.
The re-sync makes at most 100 command creates in 24 hours; only creates Discord accepts count (startup creates count but are never blocked; Discord's daily create limit is about 200); at the limit it DMs the owner once, and missing commands stay missing until the window passes or go-trader restarts. Creates run in priority order, so a cut-off repair restores the held-state commands first (`clear-cash-reconcile`, `clear-operator-close`, `resolve-entry`, `resolve-rh-order`, `resolve-rh-account`), then the other owner ops commands, then the read-only ones.
`discord.guild_id` (optional, restart-required) only names a guild from which go-trader deletes the `go-trader-` commands that the guild-scope build registered there, once, after the first global sync with no error (startup or a later re-sync; a failed cleanup is retried by the re-sync); commands still register globally.
The bot must be invited with the `applications.commands` OAuth scope in addition to `bot`. Every command carries the `go-trader-` prefix on the wire (`/go-trader-status`); authorization and dispatch operate on the bare ID. Registration failure is non-fatal — it logs and DMs the owner.

**Read-only** — any guild or DM, anyone. They read live in-process state with no HTTP round trip. Replies are public in-channel unless `discord.ephemeral_replies: true`.

`/go-trader-status`, `/go-trader-health`, `/go-trader-positions`, `/go-trader-pnl`, `/go-trader-leaderboard [top]`, `/go-trader-circuit-breakers`, `/go-trader-dead-strategies`, `/go-trader-correlation`, `/go-trader-closing-strategies`. The four that fetch live marks (`status`, `positions`, `pnl`, `leaderboard`) defer the ACK so they do not blow Discord's 3-second deadline.
`scripts/post-paper-leaderboards.py --port <paper status_port> --channel <Discord channel id> [--top N] [--dry-run]` posts the cross-coin paper leaderboard to Discord from the paper service's status API, reading `DISCORD_BOT_TOKEN` (and `GO_TRADER_STATUS_TOKEN` when the status server has a token); a 10-minute sentinel file dedupes repeat posts.

**Ops and mutating ops** — owner-only AND DM-only:

| Command | What it does |
| --- | --- |
| `logs [n]` | The last N journal lines of the unit (`GO_TRADER_SERVICE`, default `go-trader`). It reads the unit's `LogNamespace` from `systemctl show` and runs `journalctl --namespace=+<ns>`; an empty value or a failed lookup reads the default journal. In the container image it runs neither and replies with the `docker compose logs` command. DM-only because logs can carry wallet addresses and error payloads |
| `restart` | `systemctl restart go-trader`; it ACKs, then this instance is replaced. In the container image the daemon drains, saves state and exits 0, and Docker starts the container again |
| `backtest <strategy> <symbol> [timeframe]` | A single-mode backtest, 5-minute timeout, holding one of the four Python semaphore slots; replies with a summary and attaches the report |
| `report-an-issue <title> <body> [label]` | Files a GitHub issue against `discord.report_repo` (default `richkuo/go-trader`). Token from `GO_TRADER_GITHUB_TOKEN`, then `GITHUB_TOKEN`, then `discord.report_github_token`; it says so when none is set |
| `config show` | The running config with secrets redacted |
| `config set <key> <value>` | A top-level or per-strategy field. Per-strategy keys (`strategies.<id>.<field>`) need `config_version` 13 or newer. Writes are atomic and serialized with the dashboard tuner; the reply states whether it applied through SIGHUP or a restart |
| `add-strategy <name> <platform> <asset>` | Generates a Hyperliquid perps (always paper) or BinanceUS spot entry. The name must be a known short name. An entry-only candidate listed in `addStrategyEntryOnlyCandidates` also gets its explicit `close_strategy` (with tier params where listed) and `closed_bar_decisions: true`, so it decides on the closed bars its study used, plus `stop_loss_atr_mult` where listed (for example `mass_index_reversal`: `tiered_tp_pct` +3%, `stop_loss_atr_mult: 1.0`); otherwise the stop stays the loader default. A research scalar (`addStrategyResearchCandidates`, #1923) is Hyperliquid-only, always `--mode=paper`, carries its timeframe, cadence, close and stop, needs `open_params` (a JSON object) for its required params (`macro_event_breakout`: calendar and quote artifacts), and is refused unless the config's root `market_feed` is `websocket` or `shared`; the add never changes root settings. The web dashboard takes the same `open_params` |
| `add-quote-owner <fragment>` | Adds one held paper quote owner from an attached `quote_owner` JSON file (§ Quote Lifecycle, Typed quote setup), after a DM confirm that shows the full entry, its digest and the engine hold. Needs a restart |
| `remove-strategy <id>` | Removes a strategy after an out-of-band DM confirm. Needs a restart. It refuses while the strategy holds any position or option-leg row, zero quantity included, because the startup prune and the next full-file save delete those rows by cascade with no close trade. The refusal names the sorted symbols and the close path: `paper-close` for paper, `manual-close` for live `type=manual`, `force-close` for live Hyperliquid perps, the `inventory_quote_exit` lane for a quote-owned book, or no close command for any other live platform. It also refuses unless the running roster shows the strategy paused (a reload, and so a pause, takes effect only between cycles, so no running or later cycle can open after the check), refuses a replay mirror (pause does not hold replayed opens; turn off `replay_sharing` while flat first), and refuses while a queued manual action or resting limit row names it. The checks run before the confirm and again inside the config write lock. The dashboard remove always restarts, whatever its restart box says, and dashboard open and add refuse a removed strategy until the process exits. Trade history stays |
| `paper-close <strategy> [symbol]` | Flattens the open positions and option legs of one paper strategy, in any paper partition and under its `storage_strategy_id` alias, after a DM confirm. It books one close trade per leg at a fresh mark with reason `operator_paper_close`, sends no order and needs no restart; it saves only that strategy's book (`SaveStrategyBook`). Details: § Operator paper close |
| `add-platform <name>` | Emits a setup checklist. Secrets go in the environment file, never the config |
| `paper-to-live <strategy>` | Flips `--mode=paper` or `--mode paper` to one `--mode=live` token after a DM confirm. Needs a restart, refuses while the strategy holds an open position, and refuses every `type: options` strategy (options run as paper only). It refuses any research reference (§ Add Or Change Strategies item 7) at confirmation and again on the current disk config before the flip |
| `apply-regime-gate` | Interactive picker over type-eligible flat strategies, applies a named regime entry-gate preset, then confirms before writing. It refuses a non-flat target both before and after the confirm. The confirm lists any OTHER strategy whose dormant `allowed_regimes` gate the accompanying `regime.enabled` flip would reactivate — read that list first. Applies through a full restart |
| `clear-cash-reconcile <strategy>` | Clears the cash-reconcile latch after you confirm virtual cash matches the venue. It never invents or adjusts cash; it only drops the block on live spot buys |
| `clear-operator-close <strategy> [platform]` | Clears one operator-required circuit-breaker entry (`okx_spot` or `robinhood_options`) after you flatten it on the venue (§ Operator-Required Circuit Breakers). It refuses every automatic pending close, an unknown platform, and an omitted platform when the strategy holds both keys. The DM `confirm` prompt shows the strategy, platform, episode and legs; after the confirm it checks them again under the state lock and refuses when a new fire or another clear changed the entry. A failed save restores the entry and refuses. It does not read the venue or change cash, trades, positions or the circuit breaker |
| `resolve-rh-order <strategy> <attempt> [order] [release:attest-no-order]` | Runs a fresh venue check for one held live Robinhood order attempt (`rh-…` id from the alert, the start log or `storage-inspect`). The optional order id is only a lookup key: the order must carry the attempt's `ref_id`, or else match its side, pair, quantity, submit window and account; for a sized sell the quantity is the saved sent quantity, and with no saved sent quantity no field match is tried, so the check never proves that no order exists while a same-pair order without `ref_id` is in the window (#1819). With no order id, the check reads the order list: an exact `ref_id` match on a valid page within the 20-page limit ends the list read after that page and polls that order through the same checks (#1915), so later pages are not read and a later order with the same `ref_id` is not detected (venue `ref_id` uniqueness is assumed); a field-only match or a no-order result still needs the whole list, and a `ref_id` only past page 20 stays held. A proven fill moves the attempt to `settling` (the next cycle books it); a proven no-fill releases it. `release:attest-no-order` is the only release without venue proof: it applies only to a `submitting`/`unknown` attempt with no venue order id after its settle window, runs the venue check first, then shows the side, size, `ref_id` and submit window and needs a DM `confirm`; it records `no_fill` with the operator id, prior status, hold reason and evidence. Use it only when no usable `ref_id` match is reached within the 20-page limit, the order list cannot be read to its end (more than 20 pages), and the Robinhood order history shows no order. It never sends, cancels or books an order |
| `resolve-rh-account <strategy> <bind-current\|release-book>` | Resolves one live Robinhood book held for account identity (§ Robinhood account binding). `bind-current` binds it to the logged-in account after a fresh holdings read covers it; `release-book` closes it at the mark as `robinhood_account_released` on your attestation that its coins are outside every managed account, after a fresh read shows the logged-in account does not hold them. Both need a DM `confirm` and send no order |
| `resolve-entry <strategy> <attempt>` | Runs a fresh venue check for one held live entry attempt (`ei-…` id from the alert, the start log or `storage-inspect`). It releases a `submitting` attempt as `no_fill` only on complete no-execution evidence and refuses everything else (§ Entry allowance, Operator release). It never sends, cancels or books an order |

Every config write serializes on one mutex, and mutating commands restart deployment-agnostically (systemctl with `GO_TRADER_SERVICE`, falling back to an in-process exec for signal-mode deploys; the container image uses neither and restarts by drain, state save and exit).

---

## TradingView Export

```bash
./go-trader export tradingview --strategy hl-btc-momentum --output tv-hl-btc.csv
./go-trader export tradingview --strategy hl-btc-momentum --strategy okx-eth-breakout --output tv-selected.csv
./go-trader export tradingview --all --output tv-all.csv
```

Ask which strategy IDs (or all) before running. CSV header, symbol mappings, and `tradingview_export.symbol_overrides`: [README.md](README.md) § TradingView Export.

---

## Booked-Ledger Export

The reconciliation input (issue 1685; the offline comparison is issue 1686). Two steps, both one-shot commands outside normal scheduler execution. Neither needs trading secrets, starts a notifier or Python, fetches venue data, takes the scheduler's ownership or manual-action lock, or opens migrating storage; the only lock on a source is the read lock any SQLite reader takes. Configuration loads through `LoadConfigForLedgerExport` (live credential checks off, migration in memory only), so a configuration that needs migration is never rewritten.

```bash
./go-trader export capture --config /var/lib/go-trader/config.json --output-dir /var/tmp/ledger-snap-20261005
./go-trader export ledger --manifest /var/tmp/ledger-snap-20261005/capture.json \
  --partition live --strategy hl-btc-momentum --output /var/tmp/hl-btc-momentum.ledger.json
```

**Capture.** Linux only.
The capture worker runs in a private mount namespace (root with `CAP_SYS_ADMIN`, or an unprivileged user namespace) that bind-mounts every canonical state directory read-only over itself, checks with `statfs` that each directory, state file and sidecar is on a read-only mount, then opens each file with `file:<path>?mode=ro&cache=private&readonly_shm=1` (one pinned connection; only `busy_timeout=5000` and `temp_store=MEMORY`) and runs `VACUUM INTO` a new file in the output directory.
The worker checks its own namespace before any mount: its mount namespace must differ from its parent's, or, when the parent's is unreadable (unprivileged user namespace), be owned by its own non-initial user namespace; plan input never vouches for it.
The running scheduler stays outside that namespace. macOS, or a Linux host that blocks both namespace paths, refuses capture before creating anything; source permissions are never changed to make it work.
Refused sources: a missing configured file (named apart from other status errors), a hard-linked state file, a symlinked or hard-linked sidecar (a symlinked configured path resolves to its target, and capture protects the target's directory), a rollback journal beside a file, a WAL-mode file with no `-wal` (no live connection, for example a stopped scheduler), `-wal` without `-shm`, and `-wal`/`-shm` that no live SQLite connection holds (WAL recovery).
So capture a WAL-mode deployment while its scheduler runs.
Each file is one transaction (`transactional_per_file`); separate files are separate snapshots, and the manifest records each file's capture interval.
The worker digests `trades`, `wallet_transfers` and `trade_diagnostics` before and after `VACUUM INTO` (up to 3 attempts while they change) and requires the snapshot to match, so row identifiers and content are preserved; the snapshot must pass `PRAGMA integrity_check` = `ok` and be rollback (`delete`) mode with no sidecar.
The output directory must be new and outside every state and configuration directory.
The set is `state/<role>.db` files, `config.json` (the source configuration with the storage paths changed to relative snapshot paths and `discord.token`, `discord.report_github_token` and `telegram.bot_token` removed; every other setting is checked equal) and `capture.json` (manifest version 1, written last: source and snapshot paths, hashes, capture method, confinement, driver and SQLite version, per-file intervals, source configuration hash).
A failed capture removes only what it created.

**Export.** `--manifest`, `--partition`, `--strategy` and `--output` each exactly once; duplicates, extra arguments, empty values, `--all` and `--config` exit 2.
The manifest must be named `capture.json`; its directory may move.
Before any read: manifest version, clean contained relative paths, the exact inventory (any extra file, any `-wal`/`-shm`/`-journal` even empty, symlink or hard link is refused), every hash, and a rollback-mode SQLite header.
Snapshot files open `mode=ro&immutable=1&cache=private` with `query_only=ON`.
The existing ownership inspection must report no rejection, the strategy must match its partition through `partitionFor` and the identity map, be Hyperliquid perps or Hyperliquid manual, and have its stored `strategies` row in its own file (unsplit layouts included).
Mandatory `trades` accounting columns missing = refused (no migration runs); invalid or empty timestamps, malformed Booleans, corrupt TP JSON, text or non-finite amounts fail the whole export.
The output must not exist (symlinks and hard links included), must not use a sidecar name, and must sit outside the snapshot, state and configuration directories; it is written to a private staging file beside it and linked into place without replacement only after every input is re-verified.
A killed export leaves no output, at most a `.<name>.<hex>.staging` file.

**Contract (schema `go-trader.booked-ledger`, `schema_version` 2).** Required top-level fields: `inspected_revision` (the binary's source commit from `-X main.SourceCommit`, which `update.sh` stamps from the exported commit; else `null`, never the version label), `capture_manifest_sha256`, `time_basis` `UTC`, `timestamp_meanings`, `selection`, `capture`, `snapshot_files`, `current_effective_configuration` (basis `current_at_capture`: selected strategy, `regime`, partition `portfolio_risk`, and the optional `atr_method` object with the strategy value, the root value and the method `resolveATRMethod` resolves; no tokens or credentials), `events` and `wallet_orphan_context`.
Events are the selected strategy's every `trades` row in `rowid` order (keyset pages of 500 in one transaction); `event_key` = `<source_role>/trades/<rowid>`, unique only within its manifest.
Every data field is `{value, raw_value, status, reason, provenance[]}`: status `available`/`unavailable`/`not_applicable`, reason `column_absent`/`stored_null`/`unstamped`/`not_recorded`/`ambiguous_evidence`/`not_applicable`.
Stored amounts, gross flags, fees (zero and negative kept) and identifiers are copied, never recomputed; order ids are decimal strings.
`row_net_pnl` = `tradeNetPnL`, `ledger_delta` = `tradeLedgerDelta` (they differ on legacy opening rows; keep both).
`event_kind` is `funding`, `scale_in`, `close`, `non_close` or `other` from stored classification.
`close_reason`/`close_extent` come only from one unique `trade_diagnostics` match on strategy, non-empty position id, symbol, close instant, quantity and exit price, else unavailable.
Entry ATR and stop/TP geometry come from the trade row only; unstamped zeros and blanks stay unavailable with the stored sentinel in `raw_value`.
Funding with no position id is `position_allocation: unallocated` and stays the strategy's.
`wallet_orphan_context` (live only, read from the primary file in its own transaction) lists Hyperliquid `funding_orphan` rows of `wallet_transfers` as `live_wallet`/`unallocated` with null strategy and position; it is outside the strategy's totals.
`cost_model_version` (version 2, issue 1726) is the paper cost model that booked the row: `available` when the stored value is at least 1, `unavailable`/`unstamped` with raw value 0 for a row booked before the stamp, and `unavailable`/`column_absent` when the file has no such column; a negative value fails the export.
Within a version, only optional fields may be added; any change to required fields, types, units, enumerations, ordering, accounting or event keys is a new version, and consumers must refuse an unknown `schema` or `schema_version`.
Version 2 differs from version 1 only by the `cost_model_version` field; `ledger_compare.py` and `fee_evidence.py` read both, and every row of a version 1 export reads as cost model version 0.

Proof: `GO_TRADER_BIN=<binary> bash scripts/test_ledger_export.sh` (Linux with a private mount namespace for the capture checks; elsewhere it checks the refusal and prints SKIP, and `LEDGER_EXPORT_REQUIRE_CAPTURE=1` turns that SKIP into a failure). The `shell-suites` CI job runs it as the runner user with `LEDGER_EXPORT_REQUIRE_CAPTURE=1` and `LEDGER_EXPORT_REQUIRE_STRACE=1` (see Tests).

### Ledger comparison (issue 1686)

`backtest/ledger_compare.py` compares one export with the `Backtester` over frozen inputs. It is research-only: it reads the export, a comparison-input file and a frozen market manifest, writes one new report file, and never touches live configuration, defaults, state, orders, the network or the cached-data loader. Acquire public market data separately (`offline_manifest.py acquire`) in an environment with no trading secrets.

```bash
uv run --no-sync python backtest/ledger_compare.py --export <export.json> \
  --comparison-input <comparison_input.json> --output <new-report.json> [--mode strict|approximate]
```

Exit 0 only for `outcome: strict_success`; 1 for any other outcome (`refused`, `unverified`, `mismatch`, `incomplete`), with the report written; 2 for malformed input (including a historical `strategy` whose `args`, `open_strategy`, `open_strategy.params` or string fields have the wrong type) or an existing output (no report); 3 for an unexpected internal error (traceback on stderr, no report). A strategy that rejects its own parameters with `ValueError` is a `strategy_rejected_inputs` refusal with the report written. `--mode` defaults to `strict`; a strict refusal is never retried as approximate. `approximate` is explicit research: it lists every approximation (the mode itself, flat costs instead of the execution spec, unverified stop inputs, assumed-inactive controls, approximate closes) and is always `incomplete`. Both modes carry the same complete stop context to the engine.

**Comparison input (`go-trader.ledger-comparison-input` version 4 is current; versions 1 to 3 stay readable; the committed live-export inputs are version 2).** Separate from the export and the market manifest; its sha256 is in the report.
`export` binds the export by schema, `capture_manifest_sha256`, selection and `booked_sections_sha256` (sha256 of the canonical JSON of `events` plus `wallet_orphan_context`), so an exporter configuration-version bump that only changes `current_effective_configuration` keeps the binding while any booked change breaks it.
`market` names the manifest (relative path and sha256), dataset and window; the window must equal `interval` (UTC, start inclusive, end exclusive).
`tolerances` gives `time_seconds` (s), `price_relative` (fraction), `quantity_absolute` (base asset) and `money_absolute` (USD).
`starting_state` gives `cash_usd`, `inventory` and `pending_decision`, each with `status`, `evidence` and a `rule`: `attested` (evidence accepted as given, except that an attested inventory is still checked against the booked replay), `booked_ledger_replay` (cash = `rule_inputs.base_value` plus the ledger delta of every booked event before the interval; inventory = booked opens minus closes before the interval) or `signal_replay` (the strategy decision on the last warm-up bar, recomputed).
`historical_configuration.timeline` holds `strategy`, `regime` and `portfolio_risk` segments with evidence; the export's `current_effective_configuration` is reported as current-at-capture evidence only and never replaces history.
`capability_evidence.portfolio_controls` may verify that drawdown, circuit-breaker and portfolio controls did not act.
Each version 2 segment declares `basis` (`raw_config`: the strategy as written; `loader_resolved`: the strategy after the loader applied defaults) and carries stop provenance.
`stop_defaults` holds `default_stop_loss_atr_mult`, `user_close_defaults`, `user_regime_atr_defaults` and `platform_max_drawdown_pct`, and `stop_evidence` holds `raw_fields` (every stop field, `leverage`, `max_drawdown_pct`, `trailing_stop_min_move_pct`), `resolved_fields` (`loader_resolved` only), `leverage_origin` (`config` or `default`) and `regime_atr_window`.
Every entry has `status` (`verified` needs a nonempty `source`) and `value`; presence entries add `present`, with verified absence as `present: false, value: null` and an explicit zero kept distinct.
A loader-resolved value never proves a field was explicit.
The optional top-level `initial_stop_geometry_evidence` maps a booked event key to `{status, source, stamp: "initial_entry"}`.
The optional version 2 segment field `atr_defaults.root_atr_method` (a presence entry) is the root `atr_method`; the loader never stamps it into the strategy.
The report records the input version it read.

**ATR method.** The comparison resolves the method as live does: the strategy `atr_method`, else a verified root value, else `simple` only when the root value is verified absent.
The resolution and its evidence requirement apply only when the simulation reads ATR (a close strategy or an ATR stop owner).
With no strategy value and no verified root evidence (always the case for a version 1 input without a strategy value), the run is refused with `atr_method_unverified`; approximate mode substitutes the declared root value or `simple` and lists it.
A strategy value or a declared root value (verified or not) other than `simple` or `wilder` is `atr_configuration_invalid` in both modes, also when the simulation reads no ATR, because live config validation rejects it at either level; it is never approximated.
The report `atr_method` section holds the method, its source (`strategy`, `root`, `live_default`, `unverified_substitute` or `not_used`) and the evidence.
Wilder ATR is recursive, so the indicator-history check needs a long warm-up when an ATR stop owner is active (1500 hourly bars passed in issue 1682's production run; 400 did not).

**Regime context (#1730).** One comparison run resolves one regime context.
`capability_evidence.directional_certification`, `regime_labels` and `regime_feature_timing` bind `partition`, `strategy_id`, `configuration_sha256` and the complete interval (`coverage: complete_interval`).
A certification entry is `reloads: none` plus exactly one of `effective: empty` or `artifact`.
Label evidence with `values_and_timing: true` is the only proof that label values and timing matched the frozen candles.
The Python certification loader is not the check: a malformed entry rejects the whole artifact, the way Go does.
A non-object `criteria`, an empty timestamp, a non-integer `schema_version`, and a state direction other than the exact strings `long` and `short` are `directional_certification_invalid`.
Flat decisions use the current expiry; an open position keeps the states frozen at entry.
A change inside the interval is `directional_certification_changes_in_window`.
Missing, hash, invalid and incomplete evidence keep those words in the reason code (`directional_certification_*`, `regime_labels_*`).
No evidence is `directional_certification_unverified` or `regime_label_availability_unverified` (approximable).
Selectors are checked before a row can be inactive: `regime_window_multi_disabled`, `regime_window_unknown`, and `regime_gate_disabled_fail_closed` (disabled regime, `allowed_regimes`, fail-closed).
A named selector with no consumer is inactive `regime_selector_no_consumer`.
A regime-owned stop consumes a named gate window, so that selector is not inactive.
Live writes one `result.Regime` payload per cycle. The position stamps, the regime-owned stop, the regime-tier label and the directional policy read one row of it, and so does the gate with `closed_bar_decisions` off.
`features.directional`, and `features.gate` with `closed_bar_decisions` off, describe that row. `unshifted_closed_candle` attests the fill bar's own close; with no token the row is the decision bar.
Tokens that disagree are `regime_feature_timing_conflict`. A token the frozen candles do not reproduce is `regime_feature_timing_unsupported` (approximable; the substitute is the decision bar).
An uncertified policy is inactive `directional_policy_uncertified`.
A bare `ranging_directional` entry is honored when a certified sub-label resolves to it, matching live `gatedDirectionalEntry`.
A gate with `closed_bar_decisions` reads one shifted closed-bar column.
With `closed_bar_decisions` off, the gate reads the attested `result.Regime` row.
Without an attested row the gate is `regime_feature_timing_unsupported`, and approximate mode uses the decision bar.
Directional checks read `result.Regime`.
A flat directional decision reads the resolved directional window, which is the primary window when the selector is default.
An open position keeps the entry stamp, the gate-window label when the directional selector is default.
Directional checks need an attested `result.Regime` row; otherwise `regime_feature_timing_unsupported`, and approximate mode uses the decision bar.
Approximate mode records that substitute and stays incomplete.
Warm-up shorter than the live lookback (at least 200) is `regime_lookback_insufficient`.
Another timeframe is `regime_timeframe_unprepared`.
`regime_window_divergence` is `regime_divergence_unmodeled`.
`regime_profile_allocation` is `regime_profile_allocation_unsupported`.
A named ATR window on the ledger path is `regime_atr_window_unsupported`. `--config` admits that window when its label column was prepared (#1732).
The engine takes `regime_label_columns`: `gate`, `directional` and `atr` columns, `payload_row` (`decision_bar`, the default, or `closed_candle`) and `gate_row` (`decision_bar` or `payload`).
An unknown key, or a named column that is absent, is refused; an absent column is not recomputed from the primary column.
Stamps survive a partial close, clear on a full close, and the same-bar reopen reads the current label.
The default constructor, with no label columns, is unchanged.

**Decision timing (#1712).** The historical strategy's `closed_bar_decisions` is a capability row (`decision_timing`) and the report `timestamps.decision_timing` section.
`true` is modeled: live decision inputs use the bar the simulator decides on, but a booked record time can still trail the bar boundary (per-strategy timer, entry gates, retries), and under `market_feed: websocket` or `shared` a decision can read a closed bar before a later venue correction.
`false` or omitted is legacy runtime timing, reported as informational with the limitation that a closed-bar simulation cannot establish forming-bar decision parity without recorded decision evidence.
A non-boolean value is refused; recognizing the flag never removes another refusal.

**Resting take-profit fills (#1727).** Comparison input version 4 (versions 1 to 3 are still read) adds an optional top-level `resting_tp_rule` `{enabled, k_ticks, source}`; in an earlier version it is an input error (exit 2).
The rule source is the paper strategy's own `resting_tp_trade_through` (a JSON boolean; absent means off) for a paper strategy, and `resting_tp_rule` for a live strategy (absent means `default_off`). `k_ticks` must equal the structural floor 1.
The capability row `resting_tp_trade_through` (`take_profit_fill_model`) records the source and is `modeled` when the rule is on, `informational` when it is off on a strategy with a tier close, and `inactive` otherwise.
It is refused, never approximated, with `resting_rule_flag_invalid` (a non-boolean flag), `resting_rule_conflict` (a paper input that disagrees with the flag), `resting_rule_live_flag` (a live strategy with the flag `true`), `resting_rule_k_unsupported`, or `resting_rule_ineligible` (no tier close, the dynamic close, scale-in, replay fields, or not Hyperliquid perps).
A rule without the execution spec refuses before simulation with `resting_rule_tick_unknown`; the rule never switches the cost mode. Turning the rule on only adds the backtester keyword after the cost block, so `cost_model`, the paper spec and `value_sources` are byte-identical with the rule on and off.
Report version 2 adds the top-level `resting_fill` section: the resolved `rule`, `simulated_tier_closes`, `booked_tier_closes`, `counts`, `failures`, `parity_scope` and `verified`.
A simulated tier close is any non-synthetic simulated close whose reason name (text before the first `:`) is one of the four rule evaluators or `tiered_tp_atr_live_regime_dynamic`; the presence of `resting_fill` never decides membership and is checked after it.
On a strategy with a tier close, a booked close is `tier` (a tier evaluator reason or `hl_sync_tp<N>_fill`), `possible_tier` (reason unavailable, as for a paper partial close, or `signal`, the reason of a paper final tier close) or non-tier. A `possible_tier` close paired within tolerance with a simulated non-tier close becomes `resolved_non_tier`. On any other strategy both populations are empty.
Failure codes: `resting_fill_unverified` (rule off and a member on either side), `resting_fill_missing`, `resting_fill_inconsistent` (not `rule: trade_through`, `k_ticks` 1, `verdict: traded_through` and `fill_px` equal to the event's raw price), `simulated_tier_close_unmatched`, `simulated_tier_close_paired_with_non_tier`, `booked_tier_close_unmatched`, `booked_tier_close_paired_with_non_tier`, `tier_close_out_of_tolerance`, and `resting_rule_suppressed_every_tier_close` (rule on, no simulated tier close, and a booked `tier` or `possible_tier` close remains).
The additive strict check `resting_tier_closes_verified` is true when both populations are empty, and otherwise only with no failure. No partition, existing strict check, tolerance or match window changes.
Parity is qualified: agreement is claimed only for tier closes on frames where no armed stop is reached, under `ohlc_walk`. An intrabar close matches a booked record time inside its bar plus the time tolerance, so a booked touch fill recorded inside the next bar's window cannot be told apart from a trade-through in that bar; the report states this limit in `parity_scope`.

**Stops.** `resolve_historical_stops` builds one stop context per segment.
`raw_config` runs `run_backtest.resolve_raw_config_stops`, the in-memory resolver the file loader also uses: normalization, the verified user close and regime-ATR defaults, then the scalar ATR default, the drawdown fallback and one percent conversion.
`loader_resolved` passes the resolved values through `live_stop_engine_inputs` once and reapplies no default.
Both bases go through `build_stop_capability_context` and central validation before the resolved owner is read, and that context reaches classification and the engine.
The required provenance is every stop field (precedence), plus the owner's own inputs (`max_drawdown_pct` and, for a raw basis, its platform default; `trailing_stop_min_move_pct`; the regime ATR window), plus `max_drawdown_pct` whenever precedence reaches the drawdown step (an owner `none` that a positive drawdown would turn into `drawdown_fallback`), plus each raw default that could apply.
A missing or unverified one is a field-named `stop_inputs_unverified` refusal (approximable).
Evidence that disagrees with the strategy or with itself is `stop_evidence_contradictory`; on a loader basis this includes a resolved `max_drawdown_pct` outside (0, 100] and a resolved `stop_loss_atr_mult` the live scalar default could not produce (a raw scalar stop field, an active regime block or a unified close skips it).
A raw basis that resolves `max_drawdown_pct` outside (0, 100] is `stop_configuration_invalid`, because live config validation rejects it.
On a loader basis a `config` leverage is verified only with a verified `resolved_fields.leverage`.
The central `UNSUPPORTED_STOP_OWNER`, `MISSING_STOP_INPUT` (including an unsupported named window) and `UNVERIFIED_MARGIN_LEVERAGE` (a defaulted or unverified leverage) are `stop_capability_refused` with `reason_code`.
These refusals are never approximated.
Version 1 has no basis or provenance, so it cannot rule out an explicit stop, an implicit ATR default, a close-owned stop or the drawdown fallback: a Hyperliquid perps segment is refused with `stop_inputs_unverified` (`basis` plus the owner's inputs).
A regime-owned stop gets labels from the historical regime configuration (`regime.enabled` is its label input; a disabled regime is `MISSING_STOP_INPUT`).
When the ATR selector is default, protection uses the gate-window stamp: the named gate window, or the primary column when the gate selector is also default.
The stamp is on the `result.Regime` row (Regime context above): the unshifted closed candle when verified timing evidence attests it, with or without a gate or policy, otherwise the decision bar.
A named ATR window stays refused for issue 1732.
The `max_drawdown_pct` portfolio-control row stays separate from its stop-fallback row.
The report `stops` section holds the basis, owner, required inputs, live and engine values, the capability context and every simulated arm.

**Initial stop geometry.** After matching, each simulated first arm is mapped to its position by event sequence, bar time and side. The booked initial-entry row's `stop_loss_trigger_px` is compared with that arm within `price_relative`, with booked and simulated `entry_atr` beside it, only when `initial_stop_geometry_evidence` verifies the row as the initial-entry stamp. A close or scale-in stamp, or a position whose first in-interval row is not its opening, never establishes the first arm. No recorded trigger is `unavailable`, which is never agreement and never blocks. A stamp without verified provenance, an attestation of another row, or an unmapped arm is `unverified`. A verified disagreement is `mismatch` and fails the pair's tolerance. Both block strict success through `initial_stop_geometry_consistent`.

**Strict eligibility.** A missing or unverified input makes the result `unverified` (diagnostic only); a recomputed rule that disagrees with the declared value is unverified.
Starting inventory under either rule is unverified when the booked replay disagrees with it or cannot be computed (a booked event before the interval with unresolved identity).
Refusals (`refused`): input binding mismatch, a configuration change inside the interval, a non-flat start (seeded inventory, declared or shown by the booked replay), a manual owner (every event `not_comparable`), a non-perps or non-Hyperliquid owner, a missing close strategy (the execution-spec path needs the open/close engine), scale-in, stop refusals (above), leverage or sizing leverage above 1, `capital_pct` (`sizing_capital_pct`) and the #1728 sizing refusals, regime rows that are not modeled (the regime context above), Hurst gate, HTF filter or HTF strategies, hedge, replay, funding-input strategies, unknown active fields, portfolio controls without verified non-intervention evidence, a pending entry on the last warm-up bar, and frozen-input gaps.
Frozen checks: manifest and file hashes; one exact candle grid across warm-up and scoring (no gap at either edge or the join); finite, consistent OHLCV; hourly funding for every hour the simulator attaches to a scored bar (after the previous bar open through the last scored bar open); scored decisions, and the ATR and regime labels a stop owner reads, unchanged when a third of warm-up is removed; for observation-input strategies, the strategy's own per-bar observation validity on every scored bar.
The central close contract is decoded with `decode_close_validation`; strict success needs `mode: strict`, `close_eligibility: eligible` and no approximation, and a passing close validation alone never makes strict success.

**Simulator events.** `Backtester.run(record_events=True)` adds `metrics["ledger_events"]` (`go-trader.backtester-ledger-events` version 3), written where the engine does the arithmetic: unrounded open, scale-in, partial and full close, funding accrual, seeded inventory and the synthetic terminal liquidation, each with timing meaning, decision and bar times, quantity, raw and effective price, fee charged, entry-fee allocation and outstanding entry fee, gross realized, funding, and quantity, average cost and cash before and after. `interval_end` records inventory and cash before the synthetic end-of-data liquidation; the comparison uses that boundary. With the option off nothing changes; with it on every other result field is unchanged.
Every event has `resting_fill`, null except on a resting tier booking: with the rule on it is the evidence of the close evaluation that set the winning fraction (a non-winning evaluation's evidence is never attached), and with the rule off under the resting model it is `{rule: legacy_mark_cross, k_ticks: null, verdict: resting_fill_unverified, reach_px, fill_px}`.

**Normalization and matching.** Booked events become position outcomes by recorded `position_id`; a missing id, a non-fill event kind, another symbol or a missing quantity/price is `unresolved_identity`, and contradictory sides stay unresolved.
Events before or after the interval are `outside_interval` and stay listed; a position open at the interval end is compared on its in-interval components and its booked residual, and the booked end inventory also counts a position held through the whole interval with no fill inside it.
Strategy funding stays unallocated and is compared only as a strategy total; wallet-orphan context is reported apart and never enters strategy totals.
Paper funding rows (Paper Funding Accrual below) carry the captured `position_id`; they still count only as strategy funding, and the informational `rows_with_position_id` count reports them.
A held position does not guarantee a strict funding mismatch: zero funding, offsetting payments or a delta within tolerance can pass, and equal paper and simulated funding cash needs equal exposure timing, rates and valuation prices.
`row_net_pnl` and `ledger_delta` are taken from the export as authoritative (legacy opening rows differ) and fees are never subtracted again.
A booked and a simulated position are candidates only with the same side and an opening fill inside the time window (bar open plus or minus the tolerance; intrabar fills add the bar length), price and quantity tolerances; nearest time alone is never enough.
Assignment is global and one-to-one: a component with more than one maximum matching is `ambiguous` with every candidate listed, and leftovers on either side are `unmatched`.
Every booked and simulated event gets exactly one disposition.
Every conservation check can fail: each booked row's `ledger_delta` and `row_net_pnl` recomputed from `realized_pnl`, `exchange_fee`, `is_close` and `pnl_gross`; every booked event listed in exactly one report section that agrees with its disposition, and booked fees and ledger deltas summed over those sections equal to the ledger totals; each matched pair's compared and leftover booked components equal to the position's in-interval fees and ledger delta; no position closing more than it has opened in record-time order; wallet-orphan exclusion; and the simulator's cash continuity, cash identity, entry-fee allocation and quantity.
Unallocated strategy funding is reported under `informational_totals`, outside `conservation_passed`.
Recorded fees and funding are reported apart from modeled ones; a cause is set only when evidence supports it.

**Fixture.** `backtest/testdata/ledger_export/` holds the real capture (`snapshot/`: only `capture.json`, `config.json`, `state/primary.db`), the real exports (`export.json` strict-supported, `export_scale_in.json`, `export_manual.json`), their comparison inputs, the frozen synthetic market (`market/`), `fixture_production.json` (documented captures and the export list) and the production sources and instructions (`source/`, `README.md`).
`scheduler/ledger_export_fixture_test.go` (`TestLedgerExportFixture`, Go CI, no Python) runs `runLedgerExport` over the snapshot into a temporary file and requires byte equality with every listed export; `backtest/tests/test_ledger_compare.py` and `test_backtester_ledger_events.py` (Python CI, no Go) read the committed files.
Regenerate exports only on purpose (export schema change, configuration-version increase, changed effective defaults or serialization): `GO_TRADER_REGENERATE_LEDGER_EXPORT_FIXTURE=1 go -C scheduler test -run '^TestLedgerExportFixture$' -count=1 .`.
It refuses unless the snapshot and `source/seed.sql` match the latest documented capture, and, without an export schema-version change, it may change only `current_effective_configuration`, plus `capture_manifest_sha256`, `capture` and `snapshot_files` after a documented re-capture, plus `events` and `wallet_orphan_context` only when that capture records a changed seed.
When `MinSupportedConfigVersion` passes the snapshot's configuration version, re-capture on Linux per the fixture README.
`export_paper_resting.json` and `export_paper_resting_touch.json`, with their version 4 inputs over `resting_market/` (the `market/` candles with two highs raised), are synthetic paper fixtures with a `tiered_tp_atr` ladder and `resting_tp_trade_through: true`, made by `source/build_paper_resting_fixture.py`. The first reaches strict success with one trade-through tier close; the touch variant books that close in the touch bar's window and fails only `matched_within_tolerance` and `resting_tier_closes_verified`.

**Cost model by partition (issue 1726).** A `live` export keeps the manifest costs, and the report `cost_model` adds `value_sources` naming the manifest keys.
Without an execution spec (scale-in strategies), the live simulation also passes the manifest `maker_fee_pct` as the backtester `maker_fee_pct`, so both live paths price resting tier fills from the same manifest value; a paper export without a spec passes its frozen `tier_fee_pct`.
Both live kinds add an informational `tier_fee_check` (`maker_fee_pct`, `taker_fee_pct`, `maker_equals_taker`, `rule`). The live comparison charges the manifest `maker_fee_pct` on simulated tier fills, while paper and the backtester without a maker rate charge taker (see Paper fill cost model), so a Hyperliquid comparison manifest must set `maker_fee_pct` equal to `taker_fee_pct` to match the evidence. The committed manifests in `backtest/testdata/ledger_export/` are frozen at a maker rate of 0.00015 and do not meet this rule yet. `maker_equals_taker: false` means the simulated tier fees differ from what the evidence supports. The check adds no refusal and no approximation and never changes the outcome.
A `paper` or `paper:<id>` export is simulated with the paper fill cost model of the version stamped on its rows: the frozen `PAPER_FILL_COST_MODELS[<version>]` taker and tier rates, the paper slippage and zero half spread, whatever the manifest cost block says.
With the execution spec, only the cost fields are overridden; `size_decimals`, `min_notional_usd` and `min_notional_margin` stay from the manifest.
The version is the one value shared by every in-interval fill row (funding excluded); with no in-interval fill it is the current `FILL_COST_MODEL_VERSION` (`version_source: no_in_interval_fills`).
The report `cost_model` is `{kind: "paper_fill_cost_model", version, version_source, values: {name: {value, source}}}` plus the spec or flat values used.
Refusals: `paper_cost_model_partition_unknown` (a partition that is neither live nor paper), `paper_cost_model_mode_disagrees` (a paper partition with live args, or a live partition without them; live args follow `isLiveArgs`), `paper_cost_model_mixed` (rows from more than one version), `paper_cost_model_unknown` (no history entry), `paper_cost_model_replay_unsupported` (rows stamped `1001` by a retired offline research replay cost policy, #1791; default paper costs are never substituted), all in both modes, and `paper_cost_model_not_reproducible` in strict mode for version 0 (random slippage); approximate mode runs version 0 with zero slippage and lists the approximation.
The check runs only for a non-manual owner with a resolved configuration segment.
The price tolerance stays an input, because decision timing (issues 1712 and 1723) still moves the reference price.
`export_paper.json` with `comparison_input_paper.json` is a synthetic paper fixture made by `source/build_paper_fixture.py` from the live fixture; it reaches strict success at a relative price tolerance of `1e-9`.

**Limitations.** Strict support covers flat-start, single-configuration Hyperliquid perps owners on the open/close engine with verified stops and no scale-in, leverage, regime gating or HTF inputs. The committed exports carry no stop stamps, so their initial geometry is `unavailable`; agreement is proven only on synthetic test copies. Booked timestamps are ledger record times, not exchange fill times. Live sizing is not modeled beyond the simulator's all-cash sizing from the verified starting cash; quantity deltas expose differences. Separate state files are separate snapshots; the comparison makes no cross-file simultaneity claim.

### Fee evidence (issue 1726)

`backtest/fee_evidence.py` measures the Hyperliquid fee rates and slippage of booked live fills from one or more exports. It is offline and suggest-only, makes no network call and writes no config. See the `docs/backtesting-registry.md` row for inputs and output.
Every export must have `selection.platform = hyperliquid`; any other platform exits 1. Rows count only when the partition is `live`, `fee_source` is `userfills` and `value` is nonzero; funding, modeled and `reconcile_adjustment` rows are counted as excluded. Hedge-leg rows are `hedge` and operator rows (`manual = true`, including manual and manual-limit opens and adds) are `manual`; other opens and adds are `open` and `add`, and close rows are grouped by `close_reason`, else by the booking code's details prefix; anything else is `unclassified` and listed. A row that appears in several exports (same storage strategy, source role and row id) counts once, and the run refuses when the copies disagree.
Live stop slippage needs venue fills: the reconcile path books a confirmed live stop at the stored trigger (`hyperliquid_balance.go`), so its exported `price` equals `stop_loss_trigger_px`. Pass raw `userFillsByTime` captures with `--user-fills` (account address only, no trading key); fills join rows by order id, and the role comes from the fill's `crossed` flag. A trigger-priced row with no venue fill is `booked_at_trigger_no_venue_evidence`, never a zero-slippage sample. Market-fill slippage needs a `--fill-log` extract and is labelled "reference price as logged"; it uses the exact booked fill price, so only the 2-decimal logged reference is rounded, and samples whose rounding bound exceeds 0.5 bps are excluded.
Production evidence is captured read-only (export capture and `export ledger`, no `--once`, no restart, no state write). Post only the derived numbers; never post or commit the account address, raw fills or exports.

### Paper fill cost model (issue 1726)

The Hyperliquid taker rate 0.045%, maker rate 0.015% and paper slippage 0.05% in `scheduler/fees.go` are code constants.
Fee evidence (2026-10-08, [issue 1726 comment](https://github.com/richkuo/go-trader/issues/1726#issuecomment-6055361624)): an operator ran `fee_evidence.py` on a read-only capture of 4 live Hyperliquid exports completed 2026-10-06. The exports, logs and report are private, so the repository cannot verify this result.
It reports that every booked `userfills` group's median and maximum is at or below `HyperliquidTakerFeePct`, so the taker constant is a conservative upper bound; the exact rate is not published.
The maker rate is not measured: the capture has 0 booked take-profit tier rows and no venue fills with a maker or taker role, so tier fills cannot be classified as maker or taker. Live stop slippage is unavailable (no venue fill capture).
Decision: both rate constants stay, paper and the non-spec backtester without a maker rate charge taker on resting take-profit tier fills, and `FillCostModelVersion` stays 1.
The one exception is the live ledger comparison, which charges the manifest `maker_fee_pct` on simulated tier fills and reports it in `tier_fee_check`.
To match the evidence, a Hyperliquid comparison manifest must set `maker_fee_pct` equal to `taker_fee_pct`. The committed manifests in `backtest/testdata/ledger_export/` are frozen at a maker rate of 0.00015, so they do not meet this rule yet, and a live strict comparison on them charges 0.00015 on tier fills and reports `maker_equals_taker: false`.
The maker constant 0.015% is an unverified assumption that no tier fill uses by default.
`ApplyAdverseSlippage(price, isBuy)` returns exactly `price * (1 + SlippagePct)` for a buy and `price * (1 - SlippagePct)` for a sell, the backtester formula; there is no random slippage.
Paper market opens, flips, signal closes and partial closes on perps, spot and futures take it inside the existing no-fill branch (`fillQty == 0` or `fillContracts == 0`), so a venue-filled live row keeps its venue price.
OKX and TopStep live executes whose result has no fill (or `AvgPx <= 0`) also reach that branch, so those live modeled rows now book the deterministic adverse price instead of a random one; Hyperliquid and Robinhood live never reach it: Hyperliquid opens only after `confirmHyperliquidExecuteFill` confirms the fill, and Robinhood books only a settled confirmed fill from `robinhood_order_attempts` (`confirmRobinhoodFill`, #1815).
Paper Hyperliquid scale-in adds book at the mid moved against the add side; the add quantity stays the decision quantity.
A paper stop books the worse of the trigger and the mark moved against the position (`paperStopBookPx`), applied only at the six paper stop sites (`applyPaperStopLossBreach`, `advancePaperDynamicCloseRegime`, the open-cycle arm, the main-loop trailing and fixed stops, the ratchet same-cycle trailing stop); the shared booking functions are unchanged, so live stop bookings keep their trigger or venue price, and the close row keeps the unslipped `stop_loss_trigger_px`.
A paper take-profit tier fill books at the tier price with no slippage and the taker fee.
The replay mirror books an open at the live reference price moved against the trade, with the live quantity; that reference is the live venue average, so a replay open or add carries the real live slippage plus the paper slippage.
A replay add books the same way; a replay take-profit close (`replayCloseReasonIsTakeProfit`) books at the row's live tier price with no slippage, else at the cycle price with no slippage and `tier_price_source=cycle_price` in the details; any other replay close books at the cycle price moved against the close side.
Exceptions in version 1: paper hedge legs and the paper hedge unwind book at the mark with no slippage (the backtester rejects hedge).
`CalculateHyperliquidFee` and `CalculatePlatformSpotFee` stay the taker rate because they are the live fallback fee, and `executionFee` uses the venue fee only when it is greater than 0, so a zero-fee or rebate venue fill books the modeled taker fee with `fee_source = modeled`.
Every new trade row stores `cost_model_version` = `FillCostModelVersion` (1), set only by `RecordTrade` (`stampTradeCostModel` in `fees.go`, empty `cost_policy_id`); rows booked before this change keep 0 and are never restamped, because loads read the stored value and saves skip persisted rows.
Model 1001 (`FillCostModelReplayCustomVersion`) rows came only from offline research replays and research pair members, both removed in #1955 (§ Retired research); no current code writes one, and `ledger_compare.py` still refuses a ledger that holds one (`paper_cost_model_replay_unsupported`).
Version 0 means random slippage on market fills, no stop slippage and taker on every fill; version 1 is the model above.
Any change to `SlippagePct`, a Hyperliquid fee constant or which fill kind pays maker or taker raises `FillCostModelVersion` and adds a new literal `PAPER_FILL_COST_MODELS` entry in `backtester.py`; existing entries are never edited, and `test_platform_fees.py` holds a literal copy of each.
Live rows carry the stamp too; only paper comparisons use it.
Every paper strategy's booked fill prices change at the deploy of this model, so a paper comparison window (issue 1723) must not span that deploy.

---

## `/go-trader` Command

When the user says `/go-trader`, "check bot status", "show strategy health", or "how are the bots doing":

```bash
curl -s localhost:8099/status | python3 -c "
import json, sys
d = json.load(sys.stdin)
strats = d.get('strategies', {})
print(f'=== GO-TRADER (Cycle {d[\"cycle_count\"]}) ===')
for sym, p in sorted(d.get('prices', {}).items()):
    print(f'  {sym}: \${p:,.2f}')
val = sum(s['portfolio_value'] for s in strats.values())
cap = sum(s['initial_capital'] for s in strats.values())
pct = ((val-cap)/cap)*100 if cap else 0
print(f'\nPortfolio: \${cap:,.0f} -> \${val:,.0f} ({val-cap:+,.0f} / {pct:+.1f}%)')
cb = [(i,s) for i,s in strats.items() if s['risk_state'].get('circuit_breaker_until','').startswith('20')]
print(f'Strategies: {len(strats)} | Circuit breakers active: {len(cb)}')
ranked = sorted(strats.items(), key=lambda x: x[1]['pnl_pct'], reverse=True)
for label, rows in (('Top 5', ranked[:5]), ('Bottom 5', ranked[-5:])):
    print(f'\n{label}:')
    for i, s in rows:
        print(f'  {i}: {s[\"pnl_pct\"]:+.1f}% (\${s[\"pnl\"]:+,.0f}) | {s[\"trade_count\"]} trades')
dead = [i for i,s in strats.items() if s['trade_count'] == 0]
if dead:
    print(f'\nDead (0 trades): {len(dead)} - {dead}')
for i, s in cb:
    rs = s['risk_state']
    print(f'  CB {i}: dd={rs[\"current_drawdown_pct\"]:.1f}% / max={rs[\"max_drawdown_pct\"]:.0f}% | until {rs[\"circuit_breaker_until\"][:19]}')
"
```

Present the output as readable prose. Highlight circuit breakers, dead strategies, large PnL changes, and missing status data. When `/status` reports `drawdown_reading_substituted`, say so — the drawdown figure is carried forward, not measured this cycle.

---

## `/menu` Command

When the user says `/menu`, "show menu", "what can I configure", or "help me get started", present these five groups and pull the live detail from the named source rather than from memory:

1. **Trading platforms** — Strategy Reference § Platform conventions.
2. **Available strategies** — list them from the registries, never from memory (Strategy Reference), plus `/go-trader-closing-strategies` for close evaluators.
3. **Adjustable settings** — Adjustable Settings.
4. **Commands** — the operator CLI below.
5. **Backtesting** — Backtesting.

Operator CLI:

```bash
./go-trader init [--json '{…}' --output scheduler/config.json]
./go-trader manual-open <strategy-id> [--side long|short] [--size N | --notional N | --margin N]
./go-trader manual-open <strategy-id> --limit-price N [--tif Alo|Gtc] [--expire-after N]
./go-trader manual-add <strategy-id> [--size N | --notional N | --margin N]
./go-trader manual-cancel <limit-order-id>
./go-trader manual-close <strategy-id> [--qty N]
./go-trader force-close <strategy-id> [--qty N] [--dry-run]      # live HL perps strategy close
./go-trader manual-update-sl <strategy-id> --trigger N [--symbol Y] [--dry-run]
./go-trader manual-cancel-sl <strategy-id> [--symbol Y] [--dry-run]
./go-trader backfill hl-fees [--strategy <id>|--all] [--apply] [--reset-cash]
./go-trader backfill trade-ledger [--strategy <id>|--all] [--apply] [--reset-cash]
./go-trader diagnostics [--strategy <id>]
./go-trader inspect <strategy-id> [--all] [--json]
./go-trader export tradingview [--strategy <id>|--all] --output <file>
./go-trader export capture --config <source-config> --output-dir <new-dir>      # Linux; read-only snapshot set
./go-trader export ledger --manifest <dir>/capture.json --partition <live|paper|paper:<id>> --strategy <id> --output <new-file>
./go-trader agent-info [--bootstrap-md] [--append-changelog]
sudo systemctl start|stop|restart|status go-trader
journalctl --namespace=+go-trader -u go-trader -n 50 --no-pager
curl -s localhost:8099/status | python3 -m json.tool
```

`agent-info --bootstrap-md` writes `AGENTS.generated.md`, never `AGENTS.md`.

---

## Manual Trading (HL perps)

`type: "manual"` on Hyperliquid gives hand-driven entries and exits scheduler-tracked P/L, close evaluators, and Discord trade DMs. Skeleton — no `script`, `args`, or `interval_seconds`, the loader fills them:

```json
{"id":"hl-manual-btc","type":"manual","platform":"hyperliquid","symbol":"BTC","capital":1000,"leverage":3,"max_drawdown_pct":10}
```

**Close defaults:** with `regime.enabled` and a resolvable per-regime trail, a manual strategy defaults to `trailing_tp_ratchet_regime` (the regime trail owns the stop). Otherwise `tiered_tp_atr_live` (TP1 at 2× ATR, TP2 at 3×) with a scalar stop at 2.0× ATR. The tiers rest frozen on-chain, and the evaluator prices them from the same entry ATR, risk anchor and position regime. Override through `close_strategy`, an explicit stop field, or `user_defaults.manual`.

Manual and perps strategies may share a coin. Owner guards prevent cross-strategy mutation, a full close never flattens a peer's position, and all take-profit order IDs are cancelled on full close. Peers must share `leverage` and `margin_mode`, and at most one may own a trailing stop.

```bash
# Open — at most one of --size, --notional, --margin
./go-trader manual-open hl-manual-btc                              # defaults: --side long --margin 50
./go-trader manual-open hl-manual-btc --side long --size 0.01
./go-trader manual-open hl-manual-btc --side short --margin 100    # margin × leverage = notional
./go-trader manual-open hl-manual-btc --side long --size 0.01 --atr 850   # skip the ATR fetch

# Scale in — side inferred, blends avg cost, freezes the risk plan
./go-trader manual-add hl-manual-btc --margin 50

# Edit the resting stop in place
./go-trader manual-update-sl hl-manual-btc --trigger 66000
./go-trader manual-cancel-sl hl-manual-btc

# Close — full or partial
./go-trader manual-close hl-manual-btc [--qty 0.005]

# Record-only (order placed on the HL UI; the scheduler tracks it)
./go-trader manual-open  hl-manual-btc --side long --size 0.01 --record-only --fill-price 67800
./go-trader manual-close hl-manual-btc --qty 0.005 --record-only --fill-price 68250
```

Guardrails:

- `--record-only` skips the live order; pair it with `--fill-price`. The stop is **not** auto-armed — place the trigger on the UI yourself.
- Stop and take-profit reduce-only orders are placed inline on open. Omitting `--atr` auto-fetches ATR(14) for the strategy's symbol and timeframe, defaulting the fetch to 1h when the strategy has no `timeframe`. A failed fetch falls back to `0.1 × fillPrice / leverage` with one combined notification. The success log names the timeframe used.
- `--side` defaults to `long`. With no sizing flag and no `--record-only`, `--margin 50` is applied, so a bare `manual-open <strategy-id>` works as a smoke test.
- The manual default stop is 2.0× ATR, distinct from the fleet-wide `default_stop_loss_atr_mult` (typically 1.0×) for non-manual HL perps. An explicit stop field still wins. `user_defaults.manual.stop_loss_atr_mult: 0` opts manual out without touching non-manual perps; the ratchet fallback ignores that 0.
- Opening is blocked while the portfolio kill switch is active or the strategy has a pending circuit-breaker close.
- Live market `manual-open`, `manual-close`, and `manual-add` refuse without queueing when the exchange returns no confirmed fill (`AvgPx>0` and `TotalSz>0` required); state is unchanged. The dashboard trade-actions API returns HTTP 409 with the same error.
- A full `manual-close` that fills short of the book (a capped plan or a partial IOC fill) books the fill, queues the close, and in the same command restores the stop it asked the venue to cancel through the verify-first stop-loss restore below (`restoreManualStopLoss`), at the recorded trigger.
  The stop size comes from `resolveHLCloseRemainderStop` with the same rule and reading order as the cycle re-arm (post-close read first, then the pre-send reading less the fill, then the remainder).
  No backed units places no stop and sends a CRITICAL alert; a stop smaller than the remainder, or one sized without the post-close read, also sends one CRITICAL alert that names the unbacked units or the basis.
  With no backed units, the same command also cancels verify-first the pre-close stop when its cancel was not confirmed (`--update-stop-loss --size=0`, booked as a `cancel-sl` row) and every take-profit whose cancel was not confirmed, and the one CRITICAL alert names each id as removed, already filled, still resting or unverified.
  With backed units, a take-profit whose cancel was not confirmed is cancelled verify-first in the same command (`removeManualTakeProfitsAfterShortFill`) and cleared from the book through a `restore-tp` row, with a CRITICAL alert only when one could not be removed.
  The cancelled and removed tiers return at the scheduler's protection sync, sized to the book, after the queued close applies.
- A rejected full `manual-close` restores the exchange-side protection it asked the venue to cancel, before it returns the error.
  The stop-loss leg re-arms first (verified on-chain, never assumed live), sized by `resolveHLCloseRemainderStop` like the cycle re-arm, and reports whether it left the position flat (no own-side units back the book, which after an unknown outcome most likely means the close filled after the lost reply, with a CRITICAL alert);
  with no own-side units behind the book it also cancels verify-first the pre-close stop and the tiers whose cancel was not confirmed, and names each id in that alert;
  a close the script never sent (`order_outcome: not_sent`) that confirmed no cancel restores nothing on either leg, reads no account and sends no restore alert (a missing `order_outcome` stays unknown and restores);
  the take-profit leg then rebuilds the cancelled tiers through the shared protection planner (`buildHyperliquidProtectionPlan`) and the shared reduce-only placement path (`check_hyperliquid.py --sync-protection`) with the stop-loss multiplier and force-replace zeroed, so the restored stop is never touched and there is no second placement implementation.
  It runs only for live Hyperliquid strategies that place on-chain tiers (`hyperliquidPlacesOnChainTPs`), and places nothing when the stop-loss leg left the position flat, when a fresh account read reports the position gone or reversed, or when a fresh book read shows a different trade position, side, owner or zero quantity.
  Per tier: a confirmed-cancelled tier is re-placed; a tier whose cancel was never confirmed is verified on-chain first (still resting = preserved, already filled = left to the confirmed-fill reconciler, unreadable open orders = reported UNVERIFIED and left alone, never blindly re-placed); a cleared armed tier stays completed; a tier the close never asked to cancel is left to the scheduler.
  Restored take-profit sizes use `hlOwnStopShare` (`Q` at or under the book and the own-side chain), so shared-coin peers keep their own orders, and the tier geometry comes from `riskAnchorPrice()` and the position's regime label, matching the scheduler.
  The command line saves the book and queues a `restore-tp` action carrying the previous and restored order ids, the armed flags and the trade position id (`prev_tp_oids_json`, `tp_armed_tiers_json`, `position_id` on `pending_manual_actions`); the dashboard applies the same change to scheduler memory under its state lock and queues nothing.
  Adoption is per tier and idempotent across restarts: it takes the restored id when memory still holds the previous id or a cleared, unarmed tier, is a no-op when memory already holds the restored id, and otherwise keeps memory and raises a CRITICAL naming both ids.
  An action whose trade position id no longer matches the book is acknowledged without mutating anything, so a stale row never attaches orders to a replacement position.
  Both legs run under the per-symbol protection lock (`lockHyperliquidProtectionSync`, taken before `mu`), which the in-process `runHyperliquidProtectionSync` also takes; the cycle sync additionally takes the strategy file's manual-action lock without waiting and holds it through placement and bookkeeping (a manual command holds it across its whole run and queues its action row in the same transaction that saves the book) and skips the symbol for the cycle while a position-changing action (`open`/`add`/`close`/`update-sl`/`cancel-sl`/`restore-tp`) is queued, so the cycle does not place protection during an operator command or before it adopts the queued recovery.
  The skip has three bounds, because an unbounded one would leave a live position with no maintained protection.
  First, the owner decides: `runHyperliquidProtectionSync` takes an `hlProtectionGuardMode`, and the failed-close owner (`rearmProtectionForCloseRemainder`, reached from the cycle only after a submitted order requested the cancel) passes `hlProtectionGuardStopLegAfterFailedClose`, which re-arms the stop-loss leg with `stopLegOnlyProtectionPlan` stripping every tier input, so the queued row still owns the take-profit ids while the stop goes back in the same cycle; a strategy whose stop belongs to another owner returns without placing, since the trailing and scalar arms cover it.
  Every other owner passes `hlProtectionGuardFull` and skips whole.
  Second, three consecutive blocked syncs for one strategy and symbol raise the CRITICAL notification once (`notifyHLProtectionGuardStall`), and a sync that runs clears the count, so an operator command's ordinary one-cycle hand-off stays quiet while a stuck row is reported.
  Third, `drainPendingManualActions` gives every retained row a terminal state: a `restore-tp` row whose position is gone, whose trade position id no longer matches, or whose position is now owned by a peer is acknowledged without mutating the book (the ownership case raises a CRITICAL naming the restored ids, because adopting them would attach the orders to another strategy's position), and any action whose apply keeps failing for longer than `staleManualActionMaxAge` (one hour, measured from `created_at`) is acknowledged with a CRITICAL that names the action and the error.
  A row with no readable insert time never expires.

The command line writes the book change and the `restore-tp` row in **one** transaction (`StateStore.SaveStrategyBookQueueingManualAction`, which threads the insert into `saveStrategyBookWithAcks`), and the stop-loss re-arm's `update-sl` / `cancel-sl` row goes the same way. A restored order id is therefore never resting on the venue with neither the book nor a queued row naming it: a rejected write rolls both back, and the alert path reports the placed ids.

**Unresolved take-profit placements.** A `place_take_profit_limit` call whose reply is unreadable, or which raises after the venue accepted the order, is resolved the way the stop-loss path already resolves its own: `_place_tp` snapshots the open-order ids before the call and, on an unreadable status or an exception, re-reads them through `_resolve_placement_by_book_diff` (the generalised `_resolve_sl_placement_by_book_diff`).
Exactly one fresh id takes the tier; no fresh id keeps the error and leaves the tier unplaced; anything else (a failed snapshot, a failed re-read, two or more fresh ids) sets `tp_outcome_unknown[idx]`.
An unknown tier is recorded with id 0 and **armed**, on both the cycle path (`applyUnknownTPPlacementOutcome`) and the recovery path (`manualCloseTPOutcomeUnknown`), so the next cycle places no second order at the same price, and both paths raise the CRITICAL that names the tier and asks for exchange verification.
An explicit venue rejection is still a plain error, never a book diff.
Missing or unverified protection raises the same CRITICAL as the stop-loss re-arm, naming the symbol and the affected order ids and asking for exchange verification and restoration or closure; no message promises the next scheduler cycle, the close still returns its original error, and no fill price, fee or closed quantity is ever invented.
- Fills queue in `pending_manual_actions` and apply at the top of the next cycle; the running daemon drains them itself.
  `--once` takes file ownership, so it refuses while the daemon is up — start the daemon rather than racing it.
  Each drained action is acknowledged **by row id inside the transaction that persists its effect**, so an action that fails to apply keeps its row and is never deleted by a later success, in its own file or the other one.
  If the queue insert fails after a successful on-chain fill, the cleanup closes that fill through `planHLCloseOrder` (the strategy's own side, the unqueued fill counted once on top of the strategy's own book, the peers' books split by side, a fresh account read) as a lot-floored sized close through `close_hyperliquid_position.py --close-mode`, and cancels the new stop and take-profits only after a confirmed fill covers the whole fill.
  It reports success only then.
  When those books cannot be read, the same close is sent only when no other live Hyperliquid strategy uses the coin as its symbol or its hedge coin: the fill is planned alone, with zero peers, against the fresh account read, so an unread same-side book is left open and an unread opposite-side book caps or skips the order.
  A configured peer on the coin, a skipped plan (a flat shared net does not prove the fill is closed), a deferred plan, a rejected, unknown, partial or capped close, or an unconfirmed trigger cancel alerts loudly with the open quantity and the protection ids — flatten by hand.
- A 99% partial close is never silently collapsed into a full close: the queue carries the explicit `--qty` intent.
- `manual-update-sl` / `manual-cancel-sl` cancel-then-place (or cancel) the on-chain stop, then queue an action the daemon drains into memory — no direct state-DB write, no restart. They are **hard-rejected** when the strategy's automated protection would re-pin the edit next cycle; only strategies opted out of auto-stops qualify, and the error names the opt-out. `update-sl` also refuses a trigger that would fill immediately against the current mark. A stop edit records no trade.
- `force-close <strategy-id>` closes a position on a **live Hyperliquid `type=perps`** strategy — the automated-strategy analog of `manual-close`.
  It rejects paper mode and every non-HL or non-perps strategy.
  A sole-owner or escalated full close sends the whole-position close (`market_close(sz=None)`) and cancels the on-chain triggers only after a confirmed fill.
  Every other close (a `--qty` partial, or a shared-coin full close that does not escalate) is planned by `planHLCloseOrder` like `manual-close` (the strategy's side, the peers' books, a fresh account read; a failed read sends only an uncapped reduce-only close) and sent through `close_hyperliquid_position.py --side --close-mode=reduce_only|cross`, which floors the size with `floor_lot_size` and never rounds up.
  A skipped or deferred plan, a size that floors to zero, or an unreadable mid price sends no order and cancels no protection; `--dry-run` prints `sized reduce-only N` or `sized cross N`.
  A full close passes its protection ids with `--cancel-protection-after-close --cancel-min-fill=<book - hlFullCloseTolerance(book)>`, where `hlFullCloseTolerance` is the smaller of 0.0001 and 1% of the book, so the threshold stays positive for a book of 0.0001 or less.
  The script cancels them only after a confirmed fill covers the whole book; a capped plan passes none.
  The manual-open cleanup and the fail-closed hedge unwind use the same threshold.
  Only a confirmed finite fill is booked, never the requested size; a rejected or unknown outcome books nothing, sends no second order and leaves protection in place (unknown sends a CRITICAL alert).
  A full close that fills short of the book, or is capped, books the fill and resizes the remainder's stop through the verify-first `restoreManualStopLoss` (sized by `resolveHLCloseRemainderStop`) and removes the unconfirmed take-profits for the next sync, with the manual-close alerts.
  The coupled hedge leg close plans with the leg's own side and book and no peers (the hedge coin is exclusive) against a fresh read, and books only its confirmed fill.
  Outside this lane: the kill-switch whole-coin close passes no size, and the regime orphan close and the circuit-breaker pending close (both refuse shared coins and cap from an existing snapshot) still send an explicit size through the legacy `market_close(sz)` path, which rounds with `round()`.
  The booked leg records a `force_close` trade and, unlike a manual close, updates the strategy's risk state, so the circuit breaker sees it.
  A full close is refused while a stop edit is queued.
  `--qty` closes a partial.
  `--dry-run` sends no order and writes no state.
  A sole-owner whole-position preview makes no account read.
  An escalated whole-position preview makes the shared-coin floor's one read-only account read and sends no order.
  A sized preview (`--qty`, or a shared-coin full close that does not escalate) makes one read-only account request, prints `sized reduce-only N` or `sized cross N`, and still sends no order.
- A full `manual-close` or `force-close` on a **shared coin** (two or more live strategies on it) whose closing value is under the venue minimum gate takes one deliberate outcome before the venue call, through `decideOperatorSharedCloseFloor`.
  It escalates to a whole-position close (`market_close(sz=None)`) only when every peer is flat both in its own book and on a freshly refetched on-chain account, on the same side, and no larger than the operator's own position — the same proof the automatic cycle floor requires.
  Every other case refuses and sends no close order (`"no close order sent"`), including an unreadable account or a failed refetch; a hand close never holds or defers the way the cycle does, and a stored cycle hold reason is never an input to the decision.
  The one exception is an unreadable mark price: it cannot be measured against the gate, so the decision withholds only the escalation and lets the sized close through unchanged, with a warning — the venue's own minimum still catches a truly small one.
  The sized `manual-close` (a `--qty` partial or a shared-coin full close) reads the on-chain account and runs `planHLCloseOrder` (reduce-only capped at the on-chain quantity, or the netted cross order when an opposite-side peer shares the coin); a skip, or an unreadable account with an opposite-side peer, refuses with no order sent, and `--dry-run` prints `sized reduce-only N` or `sized cross N`.
  `force-close` plans its sized close the same way (see above).
  On escalation the booked trade reflects the venue's actual fill (`Execution.Fill.TotalSz`), not the requested book quantity, so a short fill attributes only the virtual portion (fee pro-rated, a warning logged) and `IsFullClose` follows the fill rather than deleting a position that still holds a remainder.
  A refusal still cancels the strategy's resting limit orders as usual, and both the refusal text and the CRITICAL alert say the order itself was never sent, naming the cancelled ids as not restored.
  With a notifier present, a refusal sends the stranded-remainder alert once under the `operator_refused` label; with no notifier (command line against a stopped daemon) it prints the full reason.
  `manual-close --dry-run` and `force-close --dry-run` preview the escalate/refuse/sized-with-warning outcome and send no order.
  That preview reads the account when the shared-coin floor or the sized plan needs it; a sole-owner whole-position preview does not.
- External closes made on the UI, or by a stop or take-profit, are detected by the reconciler and cleared automatically.
- `type=manual` is exempt from circuit-breaker drawdown checks.

### Resting limit orders

```bash
./go-trader manual-open hl-manual-btc --limit-price 68000 --side long --margin 50 [--tif Gtc] [--expire-after 4h]
./go-trader manual-cancel <limit-order-id>
```

- `--tif Alo` (default, post-only) or `--tif Gtc` only. `Ioc` is rejected because it never rests.
- The CLI exits right after placing the order. The scheduler polls fill status each cycle and books fills incrementally; partial fills open the position and grow it under the same position ID.
- Protection is **not** placed inline. The next cycle after the first fill applies it, as for any manual position.
- `manual-cancel <id>` queues a cancel; the scheduler cancels on-chain and finalizes next cycle. Expiry and operator cancel share that path.
- Rows live in `pending_limit_orders`; each partial-fill leg is tagged `scale_in`, so `#T` counts one position however many fills it took.

### Scale-in / pyramiding

An opt-in way to **increase** an open position instead of the default skip-on-same-direction. Scope: Hyperliquid perps and manual, live and paper. A same-direction add blends only price and size for PnL (`AvgCost`, `Quantity`, `InitialQuantity` grow) and **freezes the original risk plan** — entry ATR, the regime label, and the trigger geometry stay pinned to the first entry, and the cleared-tier watermark is never reset. Only the on-chain protection **size** is re-based, at unchanged triggers, on the next protection sync.

- **Strategy flags (perps):** `allow_scale_in: true` plus an optional `scale_in` block — `max_adds` (0 = unlimited), `max_added_notional_usd` (0 = unlimited), `add_spacing_atr` (signed: `>0` adds to winners, `<0` averages down, `0` no gate, measured in entry-ATR multiples from the last leg), `add_notional_usd` (0 = the standard open notional). It fires only when a same-direction signal actually reaches Go. A strategy with `open_strategy` or `close_strategy` set holds every open signal while a position is open, so it takes no adds from signals. `manual-add` accepts only `type: manual`, so such a `type: perps` strategy has no add path while a position is open.
- **CLI (manual):** `manual-add <strategy-id>` takes the same sizing flags as `manual-open`. Side is inferred, it refuses when flat, and the kill-switch and pending-breaker guards apply.
- An add books as `trade_type=scale_in` on the same position ID and is excluded from the `#T` open count, so `#T` stays distinct positions. Win/loss is unaffected.
- **Live perps guard:** `allow_scale_in` needs an ATR, regime, or trailing stop — one the resize path can grow. A static scalar stop is rejected at load because it would under-cover the grown position. Manual auto-uses an ATR stop, so it qualifies.
- Hot-reloadable when flat; toggling either field while open is blocked. Backtestable; add legs simulate against the frozen risk anchor, not the blended average cost.

---

## Backfill HL Fees

Hyperliquid `exchange_fee` was $0 for trades placed before exact-fee resolution shipped.

```bash
./go-trader backfill hl-fees --all                    # dry run
./go-trader backfill hl-fees --strategy hl-btc-momentum
sudo systemctl stop go-trader
./go-trader backfill hl-fees --all --apply
sudo systemctl start go-trader
```

- `--apply` refuses while another `go-trader` process is alive.
- Close-leg `realized_pnl` is adjusted by `(modeled_fee − real_fee)`.
- `strategies.cash` is replayed from `initial_capital` on the corrected fee and PnL stream.
- A cash-replay divergence over $1 (usually a SIGHUP capital top-up) is a warning and blocks `--apply` unless `--reset-cash` is passed.
- Paper-mode HL strategies, `perps` and `manual` alike, are skipped (no real order IDs). Live manual strategies are included.
- Per-row skip reasons are reported: `missing_oid`, `no_fill_match`, `already_real_fee`. Rows already on the gross convention are skipped as `gross_convention_row` — they belong to the trade-ledger backfill below.

---

## Backfill Trade Ledger

Migrates legacy trade rows to the gross-PnL convention and trues fee, price, and PnL up to Hyperliquid `userFills`, so the shared-wallet ledger display path reads exchange-accurate values.

```bash
./go-trader backfill trade-ledger --all               # dry run
sudo systemctl stop go-trader
./go-trader backfill trade-ledger --all --apply
sudo systemctl start go-trader
```

- Two chronological passes per row. Pass one migrates legacy net rows to gross: the fee deducted at booking (the stored real fee, else the modeled taker fee) is stamped into `exchange_fee`, the close leg gets it added back to `realized_pnl`, and `fee_source` records provenance. Rows whose `fee_source` is `reconcile_adjustment` skip migration and the userFills true-up, but cash replay still includes them. Pass two gives every row whose order ID matches a userFills aggregate the real fee, the fill VWAP price, and the exchange gross closed PnL.
- Rows sharing one order ID (partial take-profit legs, flip close-and-open pairs, shared-coin aggregate fills) apportion the aggregate by quantity share: fee across ALL legs, closed PnL across close legs only.
- `strategies.cash` and `closed_positions` replay under net semantics, with the same `--reset-cash` divergence gate as the fee backfill.
- Live funding rows are never rewritten and never touch cash. Paper funding rows do change paper cash (Paper Funding Accrual below), so this tool, like the fee backfill, skips every non-live HL strategy, `perps` and `manual` alike.
- `--apply` resets every shared wallet's ledger drift baseline, so the next reconciled cycle re-anchors on the repaired ledger instead of alarming on the correction.
- Idempotent: a second run over the same fills reports zero changes.

---

## Paper Funding Accrual (HL perps)

Paper Hyperliquid `perps` and `manual` strategies (partition not `live`) pay or receive venue funding, as live does through the wallet ledger. Live wallet funding (`fetchWalletLedgerEvents`, `ingestFundingEvent`, `funding_orphan`, the watermark) is unchanged; a paper strategy never enters it and a live strategy never gets paper funding state. Eligible coins are the primary coin and `hedge.symbol`.

- **Exposure rule.** Accounting time is the wall clock at which the in-memory paper book changes (`paperFundingClock`), never a trade-row timestamp (replay rows carry decision times). A record at venue time `t` charges the signed book quantity held at `t`; a change at exactly `t` counts as after `t`; intervals are `(From, To]`. This matches the simulator, which charges the position carried into a bar before its open fills.
- **Inventory helper.** Every paper HL perps/manual book change goes through `mutatePaperPerpsBook`, which closes the exposure segment before the change and re-anchors after it (`hyperliquid_paper_funding_state.go`). Every helper call and every accounting run checks the anchor against the book; a mismatch becomes an explicit unknown window with a CRITICAL alert, and a record inside it is held, never guessed. The helper is a no-op for live strategies.
- **Valuation.** The first cycle mark at or after the record time (sampled during accounting runs at least once after each clock hour and every 5 minutes, positive and finite). The row records the mark, sample time and lag.
- **Coverage.** Feed modes read accounting funding only from the sealed snapshot (seal v3, below) and never fetch; `market_feed: rest` fetches bounded `fundingHistory` pages per coin at most every 5 minutes, outside `mu`. Coverage `{from_ms, to_ms}` asserts every record in that range is present, strictly ordered, finite and conflict-free; conflicting duplicates are refused before deduplication.
- **Settlement.** Per coin, `settled_through_ms` advances only through covered time. A transient blocker (no mark yet, storage error, coverage that starts after progress while exposure is open, conflicting or under-30-minute records) stops the coin with no progress. A record in an unknown window or with a non-finite payment becomes a visible held item. A spacing over 90 minutes while a position was held is a recorded gap; a late record inside it books once. A missing record never books as zero; a confirmed zero rate settles with no row.
- **Row.** `Side: funding`, `TradeType: funding`, `PnLGross`, `RealizedPnL` = `-signedQty * mark * rate`, no fee, `ExchangeOrderID` `paper_funding:<COIN>:<t ms>`, `Timestamp` = `t`, `PositionID` = the captured identity (never the position open at settle time), quantity, price and value 0. Cash and the row are applied with eager persist suspended, so the partition save commits cash, rows and `strategies.paper_funding_state` together.
- **Never a risk input other than cash.** Funding never touches `RecordTradeResult`, `RiskState.DailyPnL`, consecutive losses or circuit-breaker state, and is not gated by the kill switch, circuit breaker, pause, notional cap, daily-loss hold or persistence hold. It runs each due cycle after marks merge and before the save-blocked branch, so `measureScopeCycleRisk` sees the new cash in the same cycle; a partition whose saves are blocked gets observation and sampling only. Operator trade counts exclude funding rows.
- **Lifecycle.** The first sync starts at the clock with every open position anchored and no historical replay. A strategy that becomes ineligible drops its state with one log line. Unreadable stored state is held, alerted CRITICAL and saved back unchanged, never reset. Eligible paper cash may go negative and survives restart (`ValidateState` skips the clamp for it).
- **Alerts.** One owner DM per episode per strategy, coin and kind (`records_unavailable` after 2 hours of lag, `backlog`, `anomalous_records`, `mark_missing`, `storage_error`, `not_served`, `corrupt_state`); events alert each time (`gap`, `held_unknown_window`, `held_non_finite`, `helper_bypass`, `dedup_inconsistency`). Backlog older than the 7-day feed window stays pending; the remedy is one run with `market_feed: rest` (restart required).

---

## Trade Diagnostics

```bash
./go-trader diagnostics                    # all strategies
./go-trader diagnostics --strategy hl-btc-momentum
```

Every full close eagerly inserts a `trade_diagnostics` row at close time; a background worker fills in MFE, MAE, and capture ratio from hold-window OHLCV afterwards. That worker never blocks or alters a close — a failure just leaves those columns NULL and downgrades `metrics_status`. The report opens the state DB read-only, aggregates NET PnL per strategy through the trades join (so tiered take-profits and partial exits sum correctly across legs), splits by regime-at-open and direction, and prints sample-size-gated hypotheses with the exact backtest command to validate each one. Synthetic closes (`hl_sync_external`, `*_corrupt`, `*_dup_oid`) are excluded. `llm_verdict` shows per row when present, but only the LLM entry-analysis pipeline ever writes it.

---

## Backtesting

Run every backtest through `uv run --no-sync python`. Harness map: [`docs/backtesting-registry.md`](docs/backtesting-registry.md).

Hyperliquid perps runs charge hourly funding by default (`--funding charge`). The shared helper attaches right-closed accrual (a funding time is in a bar when it is after the previous bar open and at or before that bar). Missing-hour counts use that same bar for every bar width. The engine refuses an incomplete charge result before it saves. Compare and multi-asset record a refused strategy and still run the others, then exit 1. A frame that does not charge funding is stored as `not_priced` (`off` stays `off`). `--funding partial` keeps a flagged result. `--funding off` reproduces the older directional result: cash still includes `delta_neutral_funding` accrual, and per-trade statistics stay on price PnL.
A cache hit also refills finalized hours that have no stored rate inside a covered interval, by a strict Hyperliquid download of each contiguous run (#1747).
Only a complete download with ascending, contiguous pages and a print on both sides of an hour records that hour as venue-absent in `funding_venue_gaps`, and charge counts a marked hour as present at zero accrual.
A failed download writes nothing and reports `fetch_error`.
M1 noise, the fee audit, and Monte Carlo take that same option and record `funding_incomplete`. M1–M6, `auto_suggest`, and `tune_live` stay suggest-only.

```bash
uv run --no-sync python backtest/run_backtest.py --strategy momentum --symbol BTC/USDT --timeframe 1h --mode single|compare|multi|optimize
uv run --no-sync python backtest/run_backtest.py --strategy momentum --symbol BTC/USDT --timeframe 1h --since 90

# Close evaluator — one close per strategy; --close-strategy takes a bare name or a JSON ref
uv run --no-sync python backtest/run_backtest.py --strategy momentum --symbol BTC/USDT --timeframe 1h \
  --close-strategy '{"name":"tiered_tp_atr","params":{"tp_tiers":[{"atr_multiple":1,"close_fraction":0.5},{"atr_multiple":2,"close_fraction":1.0}]}}'

# Backtest a live strategy verbatim (single mode only) — pulls its open + close refs from live config
uv run --no-sync python backtest/run_backtest.py --config scheduler/config.json --strategy hl-btc-momentum \
  --symbol BTC/USDT --timeframe 1h --mode single

# Regime gate — blocks entries outside the allowed labels; closes always execute
uv run --no-sync python backtest/run_backtest.py --strategy momentum --symbol BTC/USDT --timeframe 1h \
  --regime-enabled --regime-period 14 --regime-adx-threshold 20 --allowed-regimes trending_up trending_down

# Joint open × close-stack walk-forward co-optimization (backtest-only)
uv run --no-sync python backtest/run_backtest.py --strategy momentum --symbol BTC/USDT --timeframe 1h \
  --mode optimize --sweep-close --optimize-metric sharpe_ratio|total_return_pct|dd_adjusted_return

uv run --no-sync python backtest/backtest_options.py --underlying BTC --since 90 --capital 10000
uv run --no-sync python backtest/backtest_theta.py --underlying BTC --since 90 --capital 10000
```

- `--config` needs `config_version` 15 or newer, applies `user_defaults` by default, and `--defaults system` keeps the built-in baseline instead.
- Stop-versus-take-profit races default to `ohlc_walk`; `--intrabar-resolution bar_close` restores the legacy behavior.
- A backtest whose equity hits 0 prints a LIQUIDATED banner and floors return and Sharpe at −100%, so a deeper blowup can never rank above a shallower one.
- The backtester rejects HL-live-only mechanisms: `regime_window_divergence`, `tiered_tp_atr_live_regime_dynamic` (through the close-capability policy below, in every consumer and both modes), and an enabled `hedge` block.
- With `platform=hyperliquid` (a `--config` run of a Hyperliquid strategy defaults to it), tiered take-profits follow the resting-limit model: tiers are priced from the entry ATR, the risk anchor and the position regime, a tier crossed at the bar close fills in that bar at the tier price (quantity-weighted across tiers crossed together) with no slippage and the tier fee, and a non-increasing ladder is rejected at init. Other platforms keep the legacy next-open fill.
- Tier fee precedence (`Backtester(maker_fee_pct=...)`, issue 1726): the execution spec `maker_fee_pct`; else an explicit `maker_fee_pct`; else an explicit `commission_pct` (so `commission_pct=0` charges zero on tier fills); else the platform taker rate. `execution_spec` plus `maker_fee_pct` raises (charged twice). The default is taker, because the fee evidence did not classify live tier fills as maker. `backtest_pairs.py` takes its taker and maker defaults from `PLATFORM_FEE_PCT["hyperliquid"]` and `HYPERLIQUID_MAKER_FEE_PCT`, so a pairs run that omits `--taker-fee` now charges 0.00045 (was 0.000432).
- Research surfaces (`tune_live.py`, auto-suggest, regime promotion, the `/tuning` page) are **suggest-only**. They never write live defaults, config, or PRs.
- `--config` on a Hyperliquid perps strategy sizes with the live rule: sizing cash starts at initial capital, an open subtracts only the fee, free margin is sizing cash minus used margin, and `margin_per_trade_usd` notionals are `min(margin, sizing cash) × exchange leverage`. `sizing_leverage` below 1 still sizes. Callers that do not pass `perps_sizing` keep the previous cash-fraction fills. `--liquidation-model venue_isolated` needs `--manifest` and `ohlc_walk` (it refuses `bar_close`). The isolated liquidation price is anchored to average entry and the live quantity's margin. A bar-open stop move does not suppress a range liquidation. It still cannot pass a strict comparison until a recorded liquidation price verifies the formula. A shared-wallet pool with no per-entry available margin is labeled `pool_cap_unverified` on a research run and refused by the strategy-simulation preview. Live and paper order execution are unchanged.

### Close comparison mode (#1683)

`backtest/backtester.py` `validate_close_capabilities` is the one close-capability policy for the common backtest engine. The `Backtester` constructor calls it right after it normalizes close refs and before the Hyperliquid resting-ladder check, so the named refusal always fires first. `load_strategy_config` (after user close defaults resolve), the runner, `eval_windows.py`, `parity_diff.py` (decision walk, before strategy evaluation), `exit_diagnostics.py`, `exit_policy_ab.py` (arms and entry replay), the regime economic gate and `shared_scripts/simulate_strategy.py` all reach it. No other close-name list decides engine eligibility.

- **Mode.** `comparison_mode` (`Backtester` kwarg, candidate/research JSON field, `--comparison-mode` on `run_backtest.py`, `eval_windows.py`, `parity_diff.py`, `exit_diagnostics.py`, `exit_policy_ab.py` and the 983/984 `sweep_supplementary.py`; m6 block or variant `comparison_mode` in an auto-suggest spec). Omitted means `strict` on every platform. Only the exact strings `strict` and `approximate` are valid; anything else is `INVALID_COMPARISON_MODE`. Conflicting explicit selections fail. Live configs carry no mode field.
- **Strict** refuses `time_stop` (needs `bars_held`) and `zscore_target` (needs `zscore`) with `UNSUPPORTED_LIVE_CONTEXT`, because no live check script, shared tool or scheduler path supplies those inputs, so the live evaluators no-op. **Approximate** admits them with the simulator's own inputs and records a `RESEARCH_ONLY_CLOSE_CONTEXT` approximation. `tiered_tp_atr_live_regime_dynamic` is `LIVE_ONLY_CLOSE` in both modes. An unregistered name is `UNKNOWN_CLOSE_STRATEGY` in every regime branch; a registered close missing from `CLOSE_CAPABILITIES` is `UNCLASSIFIED_CLOSE_CAPABILITY`. `INVALID_CLOSE_REFERENCE`, `UNSUPPORTED_REPLAY_CAPABILITY` (entry replay only) and `INVALID_CAPABILITY_CONTEXT` complete the codes.
- **Errors.** `CloseCapabilityError(ValueError)` carries `reason_code` (first refusal in stable check order), `reasons` (every finding), `validation`, and `to_dict()`. Each record has `reason_code`, `feature`, `close_ref_index`, sorted `required_inputs`, and `details` (with the human `message`, e.g. "Unknown close strategy: …"). Refusals stop the run with the named reason: `run_backtest.py` exits 1 and prints the serialized refusal; `simulate_strategy.py` returns the per-label `error` plus `close_capability`, empty `markers`, exit 1, so the dashboard tuner preview shows the named reason; `tune_live.py` marks the strategy `close_capability_refused` and the artifact `requested_set_complete: false`; auto-suggest marks an M6 variant `excluded_close_capability`, flags the shortlist INCOMPLETE and exits 1. No consumer retries without the close or switches mode.
- **Result contract.** Every engine result carries `close_validation` (schema 1): `mode`, `close_eligibility` (`eligible`, `approximate`, `refused`), `approximations`, `incomplete_parity` (always true in approximate mode), `parity_status` (`unverified` for accepted strict, `incomplete` for approximate, `refused`) and `refusals`.
  Accepted strict validation proves close eligibility only; it never states overall strict parity, which needs history, coverage, execution and accounting proof elsewhere.
  `decode_close_validation` turns missing, unknown-schema or inconsistent metadata into `close_eligibility: unknown`, never strict evidence.
  `aggregate_close_validations` keeps every child's modes, approximations and refusals; an unknown or refused child sets `requested_set_complete: false`, so a supported winner cannot hide a limited comparison set.
  Windows, optimizer summaries (per stack and grid), parity frames (`frame.attrs`) and summaries, diagnostics, M6 payloads, Monte Carlo payloads, auto-suggest evidence and study reports carry it.
- **Parity.** `parity_diff.py` reports `clean` only for zero mismatches with complete close validation. Approximate zero-mismatch output is `decision_agreement` with `close_parity: incomplete` and exit code 3, because the decision walk and the live path both omit held bars and zscore.
- **Saved results.** `shared_tools/storage.py` adds nullable `backtest_results.close_validation_json` through an idempotent column-presence migration. Older rows keep NULL and decode as unknown.
- **Extension point.** `CLOSE_CAPABILITY_CHECKS` is a fixed source-owned tuple of `CloseCapabilityCheck(check_id, phases, reason_codes, callback)`; callbacks return `CapabilityRecord` findings and never raise. `CapabilityContext` carries `raw_fields` (`{present, value}`), `resolved_stop_owner` (`{name, parameters}`) and `input_evidence` (`{status, source, value}`). #1684 adds `stop_owner_support`, `stop_owner_inputs` (also registered for the `runtime` phase) and `stop_margin_leverage` with their own reason codes; there is no second refusal path or exception type.

### Backtest stop geometry (#1684)

The common engine reproduces the live Hyperliquid perps stop geometry. Live functions stay the reference: `effectiveTrailingStopPct`, `effectiveFixedStopLossATRPct`/`fixedStopLossATRTriggerPx`, `EffectiveStopLossPct`/`percentStopLossTriggerPx`, `effectiveTrailingStopMinMovePct`, `computeTrailingStopUpdateInternal` and `PerpsRiskStopDistance`.

- **Units.** `Backtester` percent fields (`stop_loss_pct`, `stop_loss_margin_pct`, `trailing_stop_pct`, `max_drawdown_pct`, `trailing_stop_min_move_pct`) are fractions for direct callers. Live percent values convert exactly once, in `run_backtest.load_strategy_config` (live config) or `live_stop_engine_inputs` from `simulate_strategy.py` (tuner preview, which must send `stop_units: live_percent`). The returned `capability_context` records the source; there is no magnitude guess.
- **Defaults.** The loader follows the live order: the user ratchet regime trail, then the scalar `default_stop_loss_atr_mult` (HL perps with no stop field, no configured regime block and no unified close), then user `tp_tiers`, then user `regime_atr`. It resolves `max_drawdown_pct` (strategy, platform risk, type default) and `leverage` (perps default 1, recorded as unverified). Raw `{present, value}` evidence is captured before defaults, so an explicit zero stays distinct from an absent field.
- **One owner.** On HL perps the engine arms the first owner live arms: trailing (post-profit override, `trailing_stop_pct`, `trailing_stop_atr_mult`, regime trail), then fixed ATR (unified close, scalar, regime, in that order), then the percent stop (`stop_loss_pct`, `stop_loss_margin_pct` divided by verified leverage, then the `max_drawdown_pct` fallback capped at 50%). An explicit `trailing_stop_pct` or `stop_loss_pct` of 0 disables the stop, as live does. ATR distances are anchor percentages capped at 50%. Both the plain-signal and open/close paths use it, with the frozen risk anchor across scale-in and average cost only for PnL.
- **Trailing.** Each bar moves the high-water mark; the trigger is replaced only when the move is at least `trailing_stop_min_move_pct` (default 0.5%), except the bar a ratchet tier tightens the trail. `trail_from_here` seeds the absolute ATR trigger at the bar mark, then trails as an anchor percentage. Live's one-shot widen is manual-adoption only and never arms in a backtest.
- **Refusals.** `UNSUPPORTED_STOP_OWNER` (owner conflicts, unified sole-owner admission, ratchet requirements, margin-only post-profit rules, risk sizing without a sizing-grade owner), `MISSING_STOP_INPUT` (invalid regime blocks, a `regime_atr_window` whose label column was not prepared — the missing column is named and is not read from the primary column — and at runtime an entry whose entry ATR is missing or implausible after ATR history exists, a regime label that does not resolve, or an ATR post-profit rule without entry ATR) and `UNVERIFIED_MARGIN_LEVERAGE` (a margin owner without verified leverage).
  Approximate mode never waives them.
  An entry during indicator warm-up (no ATR or regime label history yet) is skipped and counted in `stop_warmup_skipped_entries`, and a seeded position without those inputs starts flat (`stop_seed_dropped`), so no position runs unprotected.
  A regime-label owner with no label source at all (`regime_enabled` false and no `regime` column) refuses with `MISSING_STOP_INPUT` before the bar loop; it is never treated as warm-up.
  At the live-config boundary a unified close beside any present scalar stop field, including an explicit zero, is refused like live; direct callers keep the positive-value rule.
- **Transport.** `parity_diff.py` (`ParityConfig.stop_kwargs`), `exit_policy_ab.py`, `optimizer.walk_forward_optimize(stop_kwargs=)`, `eval_windows.py` (candidate `stop_context`), `tune_live.py`, `ledger_compare.py` (historical segments, both bases) and the tuner preview (`leverage`, `leverage_source`, `max_drawdown_pct`, `trailing_stop_min_move_pct`, `regime_atr_window`, `regime_gate_window`, `regime_directional_window`, `regime_gate_on_failure`, `regime.timeframe`, `regime.windows`, `regime_directional_policy` and its certified states) carry the full context.
  M6 stamps regime labels on any arm whose resolved owner needs one, even with no `allowed_regimes`.
  The preview applies the translator's window rule and label vocabulary; `leverage_source` is `strategy_config` (verified), `loader_default` (the Go loader defaulted it, unverified) or `tuner_override` (verified).
  Each arm reports its own refusal in `live_refusal` or `simulated_refusal` with HTTP 200, and the preview note quotes it; the other arm still renders.
  The preview applies `regime_directional_policy` with the certified states of the daemon's loaded store.
  A `regime.timeframe` that differs from the chart timeframe refuses any arm that reads a regime label (gate, named window, directional policy, or a regime stop or tier owner), because the preview does not fetch regime-timeframe candles.
  `tune_live.py` reports `stop_owner` in live units with `stop_owner_units`.
- **M6 candidate stops (#1690).** `candidate_stops` is `inherit` (default), `drop`, or an object with exactly one finite positive `stop_loss_atr_mult` or `trailing_stop_atr_mult`. The object replaces every inherited stop-owner selector on the candidate arm; it never merges, so a requested ATR stop always owns the stop even when the incumbent had a percent stop. Platform, type, leverage, drawdown and trailing-move inputs and their evidence stay; the context drops the incumbent's declared owner and records `candidate_stop_override`. With no candidate close it is stop-only: no signal-reversal exit, paired replay runs. Auto-suggest never sends a stop field as a close ref.
- **Oracle.** `backtest/testdata/stop_geometry_parity.json` holds paper-mode configs, positions, mark paths and the expected outputs. `scheduler/stop_geometry_parity_test.go` asserts the real Go functions (`STOP_GEOMETRY_FIXTURE_UPDATE=1` rewrites `expected`); `backtest/tests/test_stop_geometry_parity_1684.py` asserts the translator and `Backtester.run` through the `stop_observer` hook. Geometry agreement is not historical, execution or liquidation parity. Manual-type strategies and non-Hyperliquid platforms keep the legacy geometry.

---

## Reconfiguration

```bash
sudo systemctl kill -s HUP go-trader   # hot reload, no state loss
sudo systemctl restart go-trader       # full restart
```

Hot reload re-applies a safe subset: capital, drawdown, intervals, params, stop-loss fields (including percentage and ATR-multiple trailing), sizing leverage, theta harvest, `portfolio_risk` knobs and their `portfolio_risk.paper` overrides except `max_notional_usd` on either, summary cadence, per-strategy `allowed_regimes`, `paused`, `circuit_breaker` and its cooldowns, `notify_ratchet_triggers`, paper `allow_no_edge`, `llm_entry_analysis`, `hurst_gate`, `alert_throttle_interval`, `kill_switch_reset_dm_timeout`, `log_level`, `user_defaults`, `tuning.max_retained_runs`, and the Discord/Telegram **channel maps**. Per-strategy `regime_*_window` selectors, `replay_sharing` and `replay_source_id` reload only while flat.

**Restart-required — a SIGHUP is rejected outright:** the strategy roster;
any `script`/`args`/`type`/`platform` field, the HTF filter, or kill-switch identity;
`db_file`;
`log_dir`;
`status_port`;
the status token;
`auto_update`;
`paper_db_file`, every `paper_sources` entry and its `db_file`, any strategy's `paper_source`, and any strategy's effective `storage_strategy_id`;
`risk_free_rate`;
`leaderboard_post_time` and `leaderboard_summaries`;
`tradingview_export`;
`replay_log_path`;
`market_feed`, `role` and both `shared_market_feed` socket paths (a feed service rejects `feed.socket_path`, `feed.source` and `status_port` changes the same way);
`portfolio_risk.max_notional_usd`, `portfolio_risk.paper.max_notional_usd` and every `paper_sources[].portfolio_risk.max_notional_usd`;
the whole `correlation` block;
the global `regime` block (enabled, period, adx_threshold, windows);
`discord.enabled`/`token`/`owner_id` and `telegram.enabled`/`bot_token`/`owner_chat_id`;
and a shared-wallet pool↔allocated budgeting switch.

It also refuses when per-strategy exchange `leverage`, `direction`, `invert_signal`, `regime_side`, `allow_uncertified_regime_side`, HL `margin_mode`, or the regime timeframe changed while a position is open, and for every field in the blocked-while-open list under Post-Update Agent Protocol. It re-runs the HL peer-on-same-coin check for `margin_mode` and exchange `leverage` agreement and the single-trailing-stop-owner rule, and re-validates every `hedge` block. A rejection names every offending field at once; fall back to a restart.

Common changes:

- Regenerate config: `./go-trader init`, or the scripted `--json` form.
- Channels: edit `discord.channels` / `telegram.channels`; use `trade_alert_channels` to send fills somewhere other than the summaries.
- Token: `sudo systemctl edit go-trader`, add the environment override, restart.
- Add or remove strategies: edit the `strategies` array. Removed strategies are pruned from state.
- Risk: edit strategy `max_drawdown_pct`, portfolio `max_drawdown_pct`, `portfolio_risk.warn_threshold_pct`.
- Paper to live: change `--mode=paper` to `--mode=live`, add `--execute` where required, and configure the exchange credentials.

Changing `capital` does not reset cash or positions. For a full reset, remove `scheduler/state.db` (or that strategy's rows) and restart.

---

## Adjustable Settings

Global — key, default, notes:

| Key | Default | Notes |
| --- | --- | --- |
| `interval_seconds` | 300 | Check interval |
| `db_file` | `scheduler/state.db` | State DB. In the split layout it holds the live scope, process metadata, the live-only wallet/cash-flow tables and shared regime history. **Restart-required.** |
| `paper_db_file` | absent = single file | Optional second state file owning the paper scope's books, risk row, kill-switch events and correlation snapshot. Must resolve to a different physical file than `db_file`. **Restart-required.** |
| `paper_sources` | absent | Folds several paper deployments into one process, each in its own partition `paper:<id>` and its own file. Each entry carries `id`, `db_file` and an optional `label` and `portfolio_risk` override; every path must be distinct from every other state file. A paper strategy joins one with `paper_source`. **Restart-required.** See § Storage Ownership. |
| `storage_strategy_id` (per strategy) | `id` | The identifier this strategy owns inside its state file. Unique per file; the same value in both files is the supported alias. Lets a strategy `id` be renamed with no stored rewrite. **Restart-required.** |
| `auto_update` | `off` | `off` \| `daily` \| `heartbeat` |
| `status_port` | 8099 | Loopback only |
| `risk_free_rate` | 0.04 | Sharpe basis |
| `max_drawdown_pct` | 25 | Portfolio kill switch, per scope (live and paper latch independently) |
| `portfolio_risk.warn_threshold_pct` | 60 | Percent of the kill-switch limit |
| `portfolio_risk.max_notional_usd` | `0` = off | Gross notional cap. Over it, opens/adds/flips are held and manual open/add/limit-open refuse; closes, reductions and protection keep running. **Restart-required.** |
| `portfolio_risk.daily_max_loss_usd` | `0` = off | Cap on the day's aggregate PRE-FEE realized loss. Hold-only until UTC rollover; nothing is force-closed. Hot-reloadable even while tripped. Ignored inside `platforms.<name>.risk`. |
| `portfolio_risk.daily_max_loss_pct` | `0` = off | Same limit as a percent of Σ per-strategy `initial_capital`. With both arms set the lower resolved USD threshold wins; a zero-capital basis cannot evaluate and `/status` says so. |
| `portfolio_risk.max_same_direction_notional_usd` | `0` = off | Blocks a new same-direction open over the cap. Blocking only, direction-aware. Hot-reloadable. |
| `portfolio_risk.max_asset_concentration_pct` | `0` = off | Same blocking behavior scoped to one asset's share of exposure. Shares its exposure model with `correlation.*`. |
| `portfolio_risk.paper` | absent = inherit | Optional override block with the same fields, applied to the paper scope only. A zero or omitted field inherits the parent value; a nested `paper.paper` is rejected. `paper.max_notional_usd` is restart-required; the rest hot-reload. |
| `portfolio_risk.include_paused_in_warning` | `false` | When true, a paused strategy with no open position (regular or option) is still counted in the portfolio warning's Top Contributors block and lead-attribution line. Default excludes flat paused strategies and appends an excluded-count footnote; a paused strategy with an open position is always shown. Layers root > `paper` > `paper_sources[].portfolio_risk` like the other fields; `false` never turns an enabled layer back off. Hot-reloadable. |
| `alert_throttle_interval` | 6h | Go duration. Coalesces repeat operator alerts. |
| `kill_switch_reset_dm_timeout` | empty = 6h | Go duration. How long the reset prompt waits. Independent of `alert_throttle_interval`. |
| `log_level` | `"info"` | `"info"` or `"debug"`; any other value fails `loadConfig`. `info` prints trades, fills, non-HOLD signals, warnings, errors, CRITICAL alerts, risk and circuit-breaker events, and one `Cycle N complete` line per cycle. State lines print once and then again only when their state changes: regime labels, the cash-flow journal basis, the feed health, the per-cycle protection lines (`HL protection synced`, `HL manual protection synced`), the directional policy, divergence, the regime profile, the market-closed state, the update check, and each strategy's `Status:` line (positions, regime, markers; always on a trade). A protection sync after a trade, a limit fill, or a failed close prints every time. Script stderr prints without the known progress lines (`Fetching ...`, `Funding rate ...`, `Funding history ...`, `Merged pair ...`). A failed check prints its command with each argument over 256 bytes shown as its size. `debug` restores the full output: cycle headers, `Prices:`, every `Running: python3 ...` argv, HOLD signals, progress stderr, per-batch timing, and every state line each cycle, tagged `[DEBUG]` on the per-strategy lines. The level applies to stdout (the journal) and to `logs/<id>.log` alike. Hot-reloadable. |
| `correlation.enabled`, `.max_concentration_pct`, `.max_same_direction_pct` | off, 60, 75 | Warnings to all active channels plus an owner DM; snapshot in `/status`. Restart-required. |
| `summary_frequency` | see Configure | Per-channel cadence |
| `regime.enabled`, `.period`, `.adx_threshold`, `.windows`, `.gate_on_failure`, `.transitions` | off, 14, 20, empty | Empty `windows` = one legacy horizon. Restart-required as a block. `transitions` is alerting-only — it never gates entries, mutates config, or touches positions. |
| `notify_tp_sl_fills` | enabled when nil | `false` stops owner DMs from reconciler-detected fills |
| `notify_ratchet_triggers` | enabled when nil | Owner DM when a ratchet tier clears and tightens the trail. The per-strategy field overrides it. |
| `market_feed` | `"rest"` | `"rest"` (default; an omitted field means this) keeps legacy per-check polling. `"websocket"` opens one Hyperliquid socket and hands Hyperliquid perps and `manual` checks a sealed market snapshot on stdin. `"shared"` reads that snapshot from a separate feed service (§ Shared market feed). Any other value fails `loadConfig`. **Restart-required.** § Hyperliquid Batched Signal Checks. |
| `shared_market_feed` | unset | `{"primary_socket": "/run/go-trader-<feed>/feed.sock"}`, required with `market_feed: "shared"` and rejected with any other mode. The path must be absolute, clean and at most 100 bytes. Optional `backup_socket` names the REST backup feed; it follows the same path rules and must differ from the primary. **Restart-required.** § Shared market feed. |
| `role` | `"scheduler"` | `"feed"` turns the file into a market feed service config (§ Shared market feed); scheduler commands refuse a feed config by name. **Restart-required.** |
| `atr_method` | `"simple"` | `"simple"` (legacy rolling mean, ≥100 rounding) or `"wilder"` (published RMA, never rounded). Governs the standard ATR surface only — entry-ATR stamping, live market ATR, the manual fetch, backtester injection, tuner simulate. Strategy-internal indicators and the regime classifier are untouched. |
| `default_stop_loss_atr_mult` | `1.0` | Applies to every HL perps strategy omitting all stop-owner fields, shared-coin peers included. `0` restores the `max_drawdown_pct` fallback fleet-wide. |
| `user_defaults.manual.{margin_usd,stop_loss_atr_mult,side,tp_tiers,trailing_stop_atr_mult_regime}` | see Manual Trading | Overrides the manual-open defaults. Order: CLI or strategy param → `user_defaults.manual` → constant. `stop_loss_atr_mult: 0` opts scalar manual out and the ratchet fallback ignores that 0. `tp_tiers: []` is rejected — omit the key to inherit. Hot-reloadable. |
| `user_defaults.close`, `user_defaults.regime_atr` | none | `user_defaults.close` injects `tp_tiers` into matching close refs that omit them; its `trailing_tp_ratchet_regime` entry may carry a coupled `trailing_stop_atr_mult_regime`. `user_defaults.regime_atr` supplies fleet-wide `stop_loss_atr_mult_regime` / `trailing_stop_atr_mult_regime` for standalone `use_defaults` owners. Three layers — system → user → strategy, explicit wins. Hot-reloadable. |
| `tuning.max_retained_runs` | `0` = keep all | Prunes oldest-first terminal research-run directories; never touches a queued or running row. Hot-reloadable. |
| `replay_log_path` | `""` = off | Shared SQLite path for live→paper decision replay. It must live outside every deploy tree, e.g. `/var/lib/go-trader/shared/replay.db`, which the template unit grants through `StateDirectory=go-trader/shared`. Required by `replay_sharing`. **Restart-required.** |

Per-strategy:

| Key | Scope | Notes |
| --- | --- | --- |
| `capital` | all | Starting capital reference |
| `max_drawdown_pct` | all | The strategy circuit breaker |
| `interval_seconds` | all | `0` uses the global; auto-accelerates inside the drawdown warn band |
| `circuit_breaker` | all but manual | `false` disables BOTH arms (drawdown and consecutive losses), live and paper; nil = enabled. It suppresses only NEW fires — a latched breaker or pending close still drains — and displayed drawdown still updates. One warning per suppressed breach, `cb=off` in the startup summary and `inspect`. Hot-reloadable even while open. |
| `cb_drawdown_cooldown_minutes`, `cb_loss_streak_threshold`, `cb_loss_streak_cooldown_minutes` | all but manual | Override the hardcoded breaker parameters; nil keeps 24h, 5 losses, 1h. Positive only; cooldowns ≤ 30 days, threshold ≤ 100. Hot-reloadable even while open, for new fires only — a latched expiry is untouched. |
| `paused` | all | `false`. Holds opens, adds and flips while closes, trailing stop, ratchet and protection sync keep running. Hot-reloadable always, including while open. Shows `⏸️ paused:` in Discord `/status`. |
| `allow_no_edge` | all but options | JSON boolean only. Required to admit an effective `no_edge` open or close-fallback reference outside explicit `--mode=paper`; an explicit `false` stays refused. Live use still warns. Adding or removing it on a live strategy is restart-required (SIGHUP refuses it before admission); paper changes hot-reload. Replaces `allow_deprecated`, which v20 migration removes. |
| `htf_filter` | HL perps, OKX, Binance.US spot, Robinhood, TopStep | Skips counter-trend signals (50-EMA trend on the default higher timeframe). Missing, short or unusable higher-timeframe input holds position-increasing signals while closes and protection continue, and repeated failures alert (§ Higher-timeframe filter). Restart-required. |
| `resting_tp_trade_through` | HL perps paper; opt-in | JSON literal `true` or `false`, default `false` (`null` and any other type are refused). A tier take-profit books only after a completed bar since entry traded at least one tick past the adapter-rounded limit, at that rounded limit; a touch never fills. Live is unchanged. **Restart-required.** § Resting Take-Profit Trade-Through. |
| `closed_bar_decisions` | Binance.US spot, OKX spot/perps, HL perps, Robinhood paper spot (crypto table coins); opt-in | JSON boolean, default `false`. The signal, the exported entry ATR and entry sizing come from the last bar closed at or before the check's evaluation cutoff, as in the backtester; stops, trailing stops, ratchets and take-profits keep the current mark, ATR and regime. Missing or unverifiable closed history holds candle-derived opens and closes while protection continues. **Restart-required.** § Closed-Bar Decisions. |
| `open_strategy` | all | `{name, params}`; otherwise the name comes from `args[0]` |
| `close_strategy` | all | The single exit ref `{name, params}`; nil = open-as-close. A legacy `close_strategies` array of length ≤1 still parses, length >1 is rejected. |
| `direction` | perps | `"long"` (default), `"short"` (opens shorts only), `"both"`. Hot-reloadable when flat. |
| `invert_signal` | HL perps, manual | Inversion applies to the open signal only. A composed close is never inverted. HL checks send `invert_open_signal` on `--strategy-refs` and require the `open_signal_inverted` echo; a mismatch holds the signal. `type: manual` does not send the key. Composes with `direction="short"`. Blocked while open. |
| `stop_loss_pct` | HL perps | Sole owner auto-derives from `max_drawdown_pct` (cap 50) when omitted; same-coin peers need one explicit positive owner. `0` opts out. |
| `stop_loss_margin_pct` | HL perps | Leverage-aware. `0` opts out. |
| `stop_loss_atr_mult` | HL perps | Trigger at `avg_cost ± mult × entry_atr`, armed once after open. Live and paper. `0` restores the `max_drawdown_pct` fallback. |
| `trailing_stop_pct` | HL perps | Distance from the high-water mark. Live and paper. Capped at 50%; `0` disables. |
| `trailing_stop_atr_mult` | HL perps | `mult × entry_atr / avg_cost` frozen at open. Live and paper. Arms at open, once ATR exists. |
| `stop_loss_atr_mult_regime`, `trailing_stop_atr_mult_regime` | HL perps | Resolve the ATR multiplier per the position's frozen regime label. `{"trend_regime": {"<label>": {"atr": N}, …}}` or `{"use_defaults": true}`. Need `regime.enabled`. Backtestable. |
| `trailing_stop_min_move_pct` | HL perps | Minimum trigger move before a cancel-and-replace. Default 0.5%. A ratchet tier tighten bypasses it once, same cycle. |

**Exactly one of the seven stop owners** may be positive on an HL perps strategy: `stop_loss_pct`, `stop_loss_margin_pct`, `stop_loss_atr_mult`, `stop_loss_atr_mult_regime`, `trailing_stop_pct`, `trailing_stop_atr_mult`, `trailing_stop_atr_mult_regime` — plus the unified per-regime close block, which owns the stop itself and rejects every strategy-level stop field. Omit all of them and `default_stop_loss_atr_mult` applies. A nil↔positive toggle or a scalar↔regime flip is blocked while a position is open.

**Paper stops.** A paper HL perps position models the stop of its live twin for every owner above, the `max_drawdown_pct` fallback and the unified per-regime close.
The trailing owners use the trailing walker.
The fixed and regime ATR owners and the unified close use `fixedStopLossATRTriggerPx`.
The percentage owners use `riskAnchorPrice × (1 ∓ EffectiveStopLossPct / 100)`, the formula the live execute uses.
Paper arms the trigger at open from the booked paper fill (the fill is also the trailing high-water mark) and tests it against the mark in that cycle.
Each later cycle tests the stored trigger against the mark before signal handling, for any signal.
A breach closes the position (`trailing_stop_loss_paper`, `stop_loss_atr_paper` or `stop_loss_pct_paper`), and the signal of that cycle then runs against the flat book.
A paper stop books at the worse of the trigger and the mark (the lower for a long, the higher for a short), moved against the position by `SlippagePct` (see Paper fill cost model).
Paper triggers are not rounded to the venue tick, and the liquidation clamp is live-only.
As in live, the trailing walk and the arm of a missing trigger run only on `Signal == 0` cycles.
A paper position also moves its stop after a take-profit tier under `sl_after` (see that row).
A paper position under `tiered_tp_atr_live_regime_dynamic` also moves its stop when the applied regime label changes (`advancePaperDynamicCloseRegime`).
This runs only on `Signal == 0` cycles, after the new label is seen on `regime_confirm_cycles` hold cycles in a row (default 2), and before the `sl_after` move of that cycle.
The new trigger is `fixedStopLossATRTriggerPx` for the new label (risk anchor and entry ATR).
An unmarked stop moves, looser or tighter, only when the distance from the current trigger to the new trigger is at least `trailing_stop_min_move_pct` (default 0.5%) of the current trigger.
A stop already moved by `sl_after` (`sl_after_moved`) moves only when the new trigger is strictly tighter for the side and that same distance gate passes.
Pending confirmed take-profit consumption holds the applied label until that work is done; stop repair continues on the held label.
The stop does not move when no trigger is armed, when a trailing owner holds the stop (this includes an `sl_after` `trail_from_here` trail), when the close is a trailing take-profit ratchet, or when the new label gives no trigger.
When the mark has already crossed the moved trigger, the position closes in the same cycle (`stop_loss_atr_paper`) at the worse of the mark and the trigger.

| Key | Scope | Notes |
| --- | --- | --- |
| `sl_after` (on the close ref and/or per tier) | HL perps, manual | Post-take-profit stop move. Scalar modes: `"breakeven"`, `{atr_mult: N}` (signed), `{trail_from_here: {atr_mult: M}}`, `{trail_from_here: {tp_atr_fraction: F}}` where the trail is F × the firing tier's ATR multiple. Regime-aware shapes exist for each; composite labels follow `regime_atr_window`. Needs a fixed stop owner. Scalar↔regime or shape change blocked while open. The backtester has scalar parity and rejects the regime-aware shapes at init. **Paper HL perps** apply the same move (`runPaperPostTPStopLossAdjustment`, no subprocess): the cleared tier comes from the closed ratio `1 - Quantity/InitialQuantity` against the cumulative fractions of the position-regime tier ladder, with the backtester rule (highest tier at or above `SLAdjustedTiersProcessed` whose fraction is at most ratio + 1e-9). The move writes `StopLossTriggerPx` and advances `SLAdjustedTiersProcessed`; `trail_from_here` also writes `PostTPTrailingATRMult` and seeds `StopLossHighWaterPx` from the cycle mark, so the paper trailing walker owns the stop. Same signed trigger as live, no tighten-only clamp. An empty tier rule advances the watermark; an unresolved regime rule or an unarmed paper trigger defers. It runs in the same cycle after a paper partial close books (signal or replay mirror) and on hold cycles as catch-up, after that cycle's stop test, so the next cycle tests the moved trigger. Parity fixture: `backtest/testdata/sl_after_paper_parity.json` (Go and pytest). Paper `type=manual` is not wired. Unified per-regime closes (`tiered_tp_atr_regime`, `tiered_tp_atr_live_regime`, `tiered_tp_atr_live_regime_dynamic`) run `sl_after` only from confirmed consumption stored on the position (`tp_consumptions`). A venue discovery is not consumable. Live writes the booked record in the same book save as the quantity reduction. A discovered fill whose label still resolves is booked under that label, including when the applied label has since changed. A tier that fills as it is placed is recorded under the label of the plan that placed it. A discovered label that does not resolve stays deferred. Paper signal closes and replayed take-profit partials (`hl_sync_tp<N>_fill`, `hl_sync_external_partial`) record each tier whose fraction is crossed by that close. The processor takes the oldest pending label, runs the highest tier's rule in that group, and completes the group. An empty rule completes with no stop move. Missing or unresolved evidence stays `deferred`, is logged, sends one owner DM per position and cause, and does not hold regime advancement. A `booked` record holds dynamic regime advancement on live and paper until the group is done. `sl_after_moved` is set when the live replacement rests or fills, or when the paper trigger is written, and it remains until the position is deleted. A regime change does not loosen a marked stop and does not replace a trailing owner. A later label's `sl_after` rule also does not loosen a stop an earlier label's rule already moved: the group completes and the tighter stop stays. A rule under the same label can still move either way. An open unified position that is already partially closed and has no consumption records does not guess a historical fill: one diagnostic says earlier take-profit fills do not move the stop and that a manual stop edit is the remedy. Manual `trail_from_here` fails load and names the label and raw tier index. The label's `stop_loss_atr` is the fixed stop. The backtester runs the first two names from the open-time label and still rejects the dynamic close. Paper still tests the moved trigger on the next cycle. The paper tier close uses this same ladder: the final tier closes the rest, the tiers come from the position label (the dynamic applied label advances on paper hold cycles), and a paper tier fill books in the same cycle as this move. **Live HL perps** (`runPostTPStopLossAdjustment`) read the venue reply as the trailing walker does, through `applyTrailingStopUpdateResult`. `--update-stop-loss` modifies the resting stop in place when it is still open, so two full-size stops never rest together; an open old stop is modified in place and never cancelled; a modify rejected before it is sent leaves the old stop; a modify whose result cannot be read is outcome unknown, leaves the old stop resting, and does not place another stop until a later readable order book shows whether it landed; a new stop is placed only when the old one is already gone, so a rejected or unreadable place leaves the position without an exchange-side stop until a later cycle places one. A resting stop writes the new OID and trigger; a fill at submit books `post_tp_stop_loss_immediate` for the placed size and counts in the cycle trade tally (a capped-size residue keeps OID 0 and trigger 0); an unreadable fresh placement keeps the requested trigger with OID 0 and sends one CRITICAL alert; an unreadable order book, an external fill, a script error with no cancel or a failed subprocess writes nothing. `SLAdjustedTiersProcessed` and the `trail_from_here` owner advance only on a rested or filled stop, so the tier rule runs again once a stop OID exists. The rule trigger is clamped inside liquidation with `hlLiquidationPxForSide` + `clampStopInsideLiquidation` before the call; a clamped move sends the one `notifyHLStopPastLiquidation` alert of that cycle, with the walker's action mapping. The venue calls hold `lockHyperliquidTrailingUpdate(symbol)`, released before `mu`; every alert is sent outside both locks. |
| `leverage` | perps | Exchange margin and risk leverage, and the HL `update_leverage` call. Default 1×. Applied from flat. |
| `sizing_leverage` | perps | Notional multiplier (`cash × sizing_leverage`); defaults to `leverage`. |
| `margin_per_trade_usd` | perps, opt-in | `notional = min(margin_per_trade_usd, cash) × leverage`. Overrides `sizing_leverage`. In shared-wallet pool mode (2+ live HL/OKX perps where every member omits the capital fields) notional is `min(cap, account equity − deployed wallet margin) × leverage` with entry/mark reservation; a missing balance blocks opens but not closes. Allocated↔pool is flat-only and restart-required. |
| `risk_per_trade_pct` | HL perps, opt-in | `qty = (cash × pct/100) / stop_distance`, capped at `cash × leverage`. Bounds `(0, 10]`. Mutually exclusive with `sizing_leverage`, `margin_per_trade_usd`, `allow_scale_in`. Needs a stop owner resolvable at sizing time; regime-resolved and unified-close owners are rejected at load. **Fail-closed** — an unresolvable stop distance refuses the open rather than falling back to notional sizing. A risk↔notional mode switch is blocked while open. |
| `margin_mode` | HL perps | `isolated` (default) or `cross`. Applied from flat. |
| `allow_scale_in`, `scale_in` | HL perps, manual, opt-in | See Scale-in / pyramiding |
| `hedge` | HL perps, opt-in | `{enabled, symbol, side:"inverse", ratio, margin_mode, leverage}`. Auto-manages a leg on a different coin, mirrored from the primary's quantity by one per-cycle reconciler; the hedge leg has no independent stop, take-profit or close evaluator, and mark drift never re-trades. The hedge coin must be nobody's primary and no other strategy's hedge coin. Hedge PnL is recorded separately and excluded from the primary's lifetime trade and win/loss counts and from its loss streak. **Fail-closed** — a hedge failure on a cycle that added primary exposure unwinds that increment and sends a CRITICAL DM. The unwind and every hedge-leg reduce or close are sized closes planned by `planHLCloseOrder` against a fresh account read (the unwind with the primary's full book, the increment, its side and the peers' books captured by the scheduler; a hedge leg with its own side and no peers) and sent through `close_hyperliquid_position.py --close-mode`. An unwind cancels the primary's protection only on a full, uncapped unwind whose confirmed fill covers it. A skipped, deferred, rejected, unknown, capped or partial unwind books only the confirmed fill and sends a CRITICAL DM with the quantity still open. A hedge-leg plan skipped on a flat chain clears the leg; one skipped on a smaller or opposite-side chain sends nothing and leaves the book to the reconciler. Hot-reloadable only while flat; the backtester rejects an enabled block. |
| `hurst_gate` | opt-in | `{enabled, mode:"gate"\|"size", min, max, disarm_min, disarm_max, window_key, on_failure, size_floor}`. Sits ON TOP of `allowed_regimes`, which is unchanged. `mode=gate` holds position-increasing signals while disarmed; `mode=size` scales computed open size by `clamp(\|H-0.5\|/0.15, size_floor, 1.0)`, never above 1. Reads the Hurst metric from a composite regime window only — an ADX or missing window is rejected at load. `on_failure` inherits `regime.hurst_gate_on_failure` then `"open"`; fail-closed is flat-only. Hysteresis is keyed by a threshold hash, so editing a threshold resets it. Hot-reloadable always. **No thresholds ship** — calibration was inconclusive. Backtest through `--config`. |
| `allowed_regimes` | not options | Labels that allow an entry. Empty allows all. Needs `regime.enabled`. |
| `regime_gate_on_failure` | not options | `"open"` (default, legacy fail-open) or `"closed"`, which holds fresh opens only — management and closes always pass — while the regime store cannot produce a label. Overrides the global; empty inherits. Hot-reloadable always. `closed` with `allowed_regimes` and `regime.enabled=false` is rejected at load as a permanent block. |
| `regime_gate_window`, `regime_atr_window`, `regime_directional_window` | not options | Route the entry gate, regime-aware ATR and take-profits, and the directional policy to different horizons. Need a non-empty `regime.windows`; empty or `default` uses `regime.period`. Stamped labels persist on the position. Reload only while flat. |
| `regime_side` | HL perps | DEFAULT-OFF. While the strategy starts a decision flat, any non-zero open trigger opens on the side the directional window (`regime_directional_window`) selects: `trending_up_clean`/`_choppy`, `ranging_directional_up`, legacy `trending_up` = long; the `down` equivalents = short; every other label, an unavailable or warming window = no entry. Order: raw trigger, side mapping, then `htf_filter` on the mapped side, then base `direction` (both sides need `both`). A decision that starts open never adds, reverses or reopens; closes, stops, trailing, ratchet and protection run as usual. Needs `regime.enabled`, an explicit `close_strategy` that differs from the open strategy, and refuses `regime_directional_policy`, `regime_window_divergence`, `regime_profile_allocation`, `invert_signal` and `closed_bar_decisions`. Live entries need an active certification cell with an exact label certified for the side the table selects (each entry re-checks the current exact label and expiry), or `allow_uncertified_regime_side`; without it they hold. `probe` and a reload that turns the need on or moves the cell refuse missing evidence; startup and operator commands never do. Paper needs neither. Hot-reloads while flat; blocked while open. |
| `allow_uncertified_regime_side` | HL perps | Live acknowledgment for `regime_side` without certified evidence (#1076 found no supported forward-direction edge). Bypasses only the evidence check; readiness, labels, `htf_filter`, `direction`, risk and protection still apply. Startup logs a warning and DMs the owner. Needs `regime_side: true`. Hot-reloads while flat; blocked while open. |
| `regime_directional_policy` | HL perps | Per-regime `direction` plus `invert_signal` override. Needs `regime.enabled` and every canonical label. Resolves from the current regime while flat, from the position's frozen label while open. **Evidence-gated, DEFAULT-OFF** — it resolves to the base direction unless the `(asset, timeframe, classifier)` cell is certified in the shipped-empty artifact, so configuring it today is inert and logs a non-breaking warning. Certification is exact-match: a bare label never certifies its substates. Backtestable through `--config`. |
| `regime_window_divergence` | HL perps live | `{"short_window", "medium_window", "on_divergence": "trust_short"\|"trust_medium"\|"alert_only"}`. Overrides the direction when the two windows diverge (hard = bullish plus bearish, soft = one ranging), applied after the directional policy. Needs both windows in `regime.windows`. Visible in `/status`, DMs and a dashboard badge. The backtester rejects it. |
| `regime_profile_allocation` | HL perps | Two open-param profiles of one strategy; a slow long-window label picks the active one, switched hysteretically (`confirm_bars`, warn below 12) and only while flat — it freezes at open. `{window, profiles{label→name, all labels}, param_sets{name→overrides, exactly 2}, confirm_bars≥1, initial_profile}`. Needs `regime.enabled`. Persisted. Backtestable through `--config`. |
| `atr_method` | not options | Per-strategy override of the global; empty inherits. |
| `replay_sharing` | HL perps | `"none"` (default) or `"live_mirror"`. On live it records exposure-changing decisions to `replay_log_path`; on paper it suppresses its own position-increasing signals and replays the rows of the live source it names — the **same strategy `id`** by default, or the id in `replay_source_id` — opens at live quantity and VWAP, full closes at the paper mark under reason `replay_live_mirror`. Never forwarded to the check scripts. Hot-reloadable flat-only. Book-drift skips warn and DM (throttled) but are never retried; a close-while-flat is INFO only. |
| `replay_source_id` | HL perps, paper, opt-in | The id of the live strategy this paper mirror replays. Empty (default) keeps the pre-#1510 rule: the mirror reads the rows of the live twin that shares its own id, in another process. Set it when the live source and the paper mirror run in one process, where the two ids must differ. Requires `replay_sharing="live_mirror"` on both sides; the named strategy must be a live HL perps strategy in the same config on the same symbol and timeframe, and one live source takes one mirror. The daemon evaluates the source before the mirror in the same cycle, so a live decision reaches the paper book the cycle it is made. Changing it resets the mirror watermark and logs a WARN. Hot-reloadable flat-only. |
| `notify_ratchet_triggers` | HL perps, manual | Overrides the global; nil inherits. Notification-only, so it hot-reloads even while open. |
| `llm_entry_analysis` | all, opt-in | `{enabled, model, max_debate_rounds, timeout_s, notify_dm, notify_channel}`, default off; model default `claude-opus-5-5` (must accept `output_config.effort` and structured outputs; Claude Haiku 4.5 rejects effort), 1 round (0–3), 120s timeout (max 600), `notify_dm` on, `notify_channel` off, both-off legal. After a FRESH open — never an add, flip or manual — an async pipeline posts a short digest to the trade-alert DM and stamps `bullish`/`bearish`/`mixed` into `trade_diagnostics.llm_verdict` at close. Every call runs at effort `low`; the judge reply is schema-constrained JSON. **Advisory only**: an error, timeout, refusal, cut-off reply or off-schema verdict posts nothing, stamps nothing and has zero trade impact. Each job logs one line with its token usage (§ LLM review). Runs on its own job lane, never the shared Python semaphore. Needs `ANTHROPIC_API_KEY`. Hot-reloadable even while open. |
| `theta_harvest.*` | options | Early exit |
| `close_strategy.params.tp_tiers` with ref `tiered_tp_atr` / `tiered_tp_atr_live` | HL perps, manual | On-chain take-profit tiers, a list of `{atr_multiple, close_fraction}` (cumulative). Default `[{1.5×,0.4},{3×,0.8},{5×,1.0}]`; the final tier is coerced to 1.0 and a non-numeric tier is rejected. **Live:** the on-chain tiers own the take-profit, so an on-chain limit fill cannot race the in-process evaluator. Go keeps `closes` and always sends the position context, and adds `"close_owner": "on_chain_tp"` to `--strategy-refs` (batch: the slot's `strategy_refs`). Python then evaluates no close and applies no open-as-close fallback, so an opposite open signal holds while a position is open, and it echoes `close_owner` in its result. A same-direction open signal also holds while a position is open, so a live `type: perps` strategy with no `open_strategy` takes no `allow_scale_in` adds from signals, the same as paper. `manual-add` accepts only `type: manual`, so a live `type: perps` tiered strategy has no add path while a position is open. A `type: manual` tiered strategy adds with `manual-add`. Go treats a missing echo as a script error and holds the signal. For `type: perps`, that cycle skips every step after the check for the strategy: the ratchet, the live trailing stop replace, the fixed ATR stop arm, the protection sync, the post-take-profit stop adjustment, the hedge sync, the missing-entry-ATR alerts, and the regime and profile state updates. For `type: manual`, only the close evaluation and the regime stamp stop; the protection sync (it runs before the check), the post-take-profit stop adjustment, the ratchet and the trailing stop replace still run. A degraded feed result is exempt from the echo check and skips none of these steps. Resting on-chain orders stay for both types. Fallback: the in-process evaluator owns the take-profit (no marker, one alert per position) only when a position is open, no tier rests or is armed on-chain, and the sync cannot place tiers (no entry ATR, or for a non-dynamic ref no ladder of two or more tiers for the position's regime). **Paper:** never suppressed; the evaluator always runs. **Resting-limit model:** `check_hyperliquid.py` stamps `tp_model: resting_limit` on the position context, so every tiered evaluator prices tiers the way the on-chain plan does: the risk anchor (`risk_anchor_price`, else `avg_cost`), the entry ATR, and the position label (`protectionATRRegimeLabel`). `atr_source: live` only lets the live ATR price a position that has no entry ATR (load logs one `[WARN]` when it is set explicitly). The ladder rule matches `finalizeProtectionTiers`: two or more tiers need strictly increasing cumulative fractions and the final one closes 100%; a single tier keeps its existing evaluator behavior (no on-chain tier rests for it). Paper books a tier fill at the tier price (`close_tier_fill_price`, quantity-weighted when several tiers cross in one cycle) with no modeled slippage; detection stays on the check candle close, so a wick between cycles is not modeled. Live ignores that price. The load rejects a Hyperliquid ladder (strategy `tp_tiers`, each regime label, unified blocks, and ladders injected from `user_defaults`) whose cumulative fractions do not strictly increase or whose tier-keyed multiples are out of order; other platforms keep their ladders. |
| ref `tiered_tp_atr_regime` / `tiered_tp_atr_live_regime` | HL perps | Per-regime tiers. On Hyperliquid the `_live_` variants use the position label first (the confirmed applied label for the dynamic variant) and read the market regime only when the position has none; other platforms re-resolve each tick. Backtestable. |
| ref `tiered_tp_atr_live_regime_dynamic` | HL perps, manual | Unified per-regime block only. Applies a new ATR-regime label after `regime_confirm_cycles` consecutive cycles (a whole number >= 1, default 2), then re-places the stop-loss and take-profit orders for that label. Hot-reload-gated while a position is open. The backtester rejects it. |
| `close_strategy.params.tp_tiers` with ref `trailing_tp_ratchet` / `trailing_tp_ratchet_regime` | HL perps, manual | Shape and rules in Configure. `use_defaults: true` or an omitted `tp_tiers` takes the system ladder (scalar: trails 1.5×/1.5×/0.8× at 2×/2.5×/3× ATR; regime: per quality group). Tier-table changes blocked while open. |

Discord and Telegram: `enabled`; `channels` (the platform/type map for summaries and, as a fallback, trade alerts); `trade_alert_channels` (an override for fills only, same key scheme, hot-reloadable); `dm_channels`; `owner_id` (prefer `DISCORD_OWNER_ID`); `ephemeral_replies`; `report_repo` and `report_github_token`.

---

## Strategy Reference

**Never enumerate strategies from memory.** The registries are the source of truth and they change:

```bash
uv run --no-sync python shared_strategies/open/spot/strategies.py --list-json
uv run --no-sync python shared_strategies/open/futures/strategies.py --list-json
uv run --no-sync python shared_strategies/options/strategies.py --list-json
uv run --no-sync python shared_tools/strategy_catalog.py --list-json [--kind open|close|all] [--platform spot|futures]
```

The canonical catalog (`shared_tools/strategy_catalog.py`, #1923) lists every open and native close entry, independent of execution lookup: `{"schema_version": 1, "entries": [...]}` sorted by kind then id, each with role (`scalar_open`, `quote_capability`, `native_close`), registry platforms, parameters, edge and research markers, `owner_role`, `live_execution` (`allowed`, `acknowledgement_required`, `refused`) and `paper_execution` (status `available`/`held`/`refused`, route, requirements, reason, tracking URL). The wizard's spot, futures and research perps choices and `/go-trader-closing-strategies` read it; a catalog error stops `go-trader init` (only a missing Python environment falls back to the labelled built-in lists). The platform shims keep their `id`/`description` output for existing consumers.

`/go-trader-closing-strategies` catalogs every registered close evaluator — name, description, platforms, config params, and the role, ownership and hold labels — and marks the ones `user_defaults.close` overrides.

Strategies registered `edge_status="no_edge"` (#1681) are hidden from the shim `--list-json` and from the wizard's default lists but stay registered and appear in the catalog, so an explicit `args[0]` or config ref still loads them. The wizard offers the research perps scalars only as an explicit-paper opt-in (`researchStrategies`, `researchOpenParams`), and a quote owner only from a `quoteOwnerFile`; it sets `market_feed: websocket` when research entries need it and validates the generated config before writing.
The label is an availability policy: the cited review or study (`edge_source` `fee_audit_m5`, `study_fail`, `study_inconclusive` or `unvalidated`; `edge_ref` names the evidence) has not established approved edge evidence.
It does not claim the strategy lost money; each study keeps its own outcome.
Every effective no-edge open name or open-strategy close fallback is admitted by one rule in config validation, probe and every check script: exactly one explicit `--mode=paper` (either argument form) runs without an acknowledgement; `--mode=live` or a missing `--mode` needs `"allow_no_edge": true` on the strategy; an empty, unknown, dangling or repeated `--mode` and a non-boolean acknowledgement are refused.
The Go spot dispatch (type `spot`, not OKX or Robinhood) is paper-only and passes `--mode=paper` itself when args carry no mode.
Admission never changes portfolio scope or storage: `isLiveArgs` still decides those, so a missing-mode strategy acknowledged for admission stays in the paper partition.
A live no-edge strategy still warns at startup (stderr and one owner DM) and on reload for each new warning identity, and the summary and `inspect` show `edge=no_edge:<source>` with `(ack)` or `(paper)`.

What an operator needs to choose one:

- **Direction.** A bidirectional strategy needs `"direction": "both"`, or `"short"` to run it as a dedicated bear-only instrument. Short-only strategies emit sell signals exclusively and are pre-registered bidirectional, so they need one of those two values. Pair a short strategy with `allowed_regimes: ["trending_down"]` for clean entry gating.
- **Validation status.** `--list-json` returns only the ID and the description, and the description carries the research verdict — read it. The availability tag surfaces as `edge=no_edge:<source>` in the startup summary and in `./go-trader inspect` (with the evidence reference), plus a one-time owner DM for a live strategy. Treat anything tagged `no_edge`, or described as not out-of-sample validated, as paper-trade-first.
- **Entry versus exit.** Several entry strategies ship entries only and expect the exit to come from config — pair them with a close evaluator and a stop rather than expecting a built-in exit.

Platform conventions:

| Platform | ID prefix | Type / script |
| --- | --- | --- |
| BinanceUS spot | none | `spot`, `shared_scripts/check_strategy.py` |
| Hyperliquid perps | `hl-` | `perps`, `shared_scripts/check_hyperliquid.py` |
| Hyperliquid manual | `hl-` | `manual`, no script or interval; driven by the `manual-*` CLI; may share a coin with HL perps peers |
| TopStep futures | `ts-` | `futures`, `shared_scripts/check_topstep.py` |
| Robinhood | `rh-` | spot through `check_robinhood.py`, options through `check_options.py --platform=robinhood` |
| OKX | `okx-` | `check_okx.py` (spot and perps), `check_options.py --platform=okx` for options |
| Deribit options | `deribit-` | `check_options.py --platform=deribit` |
| IBKR options | `ibkr-` | `check_options.py --platform=ibkr` |
| Luno | `luno-` | Luno adapter and scripts |

Options on every platform run as paper only (#1823): no venue order path exists for options, so `validateConfig` refuses a `type: options` strategy whose args `isLiveArgs` classifies as live, and `check_options.py` refuses the same forms with a JSON `error` and exit 1.

**Options market data (#1920).** `check_options.py` opens a paper option leg only when every input its strategy and pricing source need is available; it never substitutes a default volatility, a rank of 50, a model price for a missing venue quote, a percentage of spot or a zero premium.
`check_options.py` accepts only `deribit`, `ibkr`, `okx` and `robinhood`; any other `--platform` fails before an adapter loads.
Pricing source per platform: Deribit uses its public ticker mark; OKX uses its listed option ticker; Robinhood with a working login uses the authenticated per-share option mark; IBKR and a logged-out Robinhood paper check use Black-Scholes with the measured realized volatility. IBKR expiry selection needs the scheduler calendar context (§ IBKR options calendar, marks and settlement).
Each adapter's `get_vol_metrics` measures a 14-return realized volatility from 90 daily closes (365 periods for crypto venues, 252 for Robinhood) and its rank against the rolling range; it raises `OptionDataUnavailable` (`shared_tools/option_data.py`) for a read failure, fewer than 15 closes, a non-finite or non-positive close or a non-positive volatility.
A flat rolling range (and exactly 15 closes) keeps the measured volatility and raises a rank-unavailable reason; the result then carries `iv_rank: null` and `vol_metrics_error`, and the signal log shows `IV rank=unavailable` and the trade summary `IV=unavailable`.
`vol_mean_reversion` and `butterfly` need a rank and fail before leg selection without one; a model-priced leg needs a measured volatility; a venue-priced leg needs neither; an existing-leg hold and a momentum hold with usable bars need no pricing input.
A leg whose effective amount (`premium_usd` when positive, else `premium × spot`, the Go booking rule) is not finite and positive is an error, and any failed leg discards the whole proposal.
Every handled error (live mode, usage, unknown strategy, unsupported platform, invalid positions JSON or shape, a missing or non-finite spot, missing momentum bars, missing data, an evaluator exception, a non-finite result) emits one JSON document with `signal` 0, no actions and a stable `error`, and exits 1; detail and tracebacks go to stderr.
An upstream 429 or 5xx keeps its class as `(upstream status <code>)` in the reason, so the existing transient alert policy applies (15 failures or 75 minutes); every other failure, including an upstream request timeout inside Python, uses the ordinary policy (alert at 3 consecutive failures). Both counters live in scheduler memory and reset on restart.
The scheduler runs eligible theta harvest once per options cycle whether the entry check succeeded, failed, or never ran because the positions failed to encode (`EncodeAllPositionsJSON` now returns the `json.Marshal` error and the check is not started). Harvest-only trades reach trade alerts and channel totals.
Harvest reads the marks from the previous cycle's refresh and needs a successful mark in this process (§ Option marks and settlement, #1912).

**Options contract size (#1911, #1918).** `scheduler/option_contract.go` is the only owner of option contract size: Robinhood 100 shares and covered short calls only; Deribit and OKX 1 unit; IBKR 0.1 coin for BTC and ETH (`optionContractUnderlyingMultipliers`, the CME Micro size), with no platform-wide size, so a new IBKR lookup for any other underlying fails. Each spec also has `LegacyMultiplier` (Robinhood 100, Deribit, OKX and IBKR 1), the size of an unstamped stored leg. An unknown platform refuses the leg. The Python adapters emit no size (`check_options.py` has no `multiplier`, `contract_spec` or `contract_type` field). Each spec also names the paper expiry settlement style (`Settlement`, #1917): Robinhood `physical`; Deribit, OKX and IBKR `cash` (§ Option marks and settlement).
Every `check_options.py` document carries `platform` (the `--platform=` argument) and `premium_unit: "underlying_unit"`: `premium_usd` is the price of one underlying unit, unrounded (Robinhood no longer multiplies by 100 or rounds to cents). Go refuses a result whose unit is missing or different or whose `platform` differs from the strategy `platform`, through the script-failure path (`Script contract failure: ...`) before `clearScriptFailure`, and `ExecuteOptionsSignal` repeats the check for direct callers; no buy, sell or signal close books, and eligible theta harvest still runs once. Keep the strategy `platform` and its `--platform=` argument on the same venue.
Booking: cost or gross premium = per-unit price × contract size × contract quantity (also for the `premium × spot` fallback); fees and trade quantity use contract quantity; each new leg stores `OptionPosition.ContractMultiplier` (`option_positions.contract_multiplier`).
Short-put collateral (strike × size × quantity of every open short put) is derived from the open legs and has no stored value. A new short put needs existing reserve plus its own collateral within cash before the premium credit, every sell must leave cash after premium and fee at or above the resulting reserve, and a buy spends only cash above the reserve. A Robinhood short call needs held long shares of the same underlying in the same strategy, minus shares reserved by its open short calls, of at least size × quantity; long calls and other strategies' shares are no cover. Before any entry books, aggregate every short call in the signal: insufficient free cover refuses every buy and sell in that signal with one INFO line, while close actions still run. Other refusals log required and available amounts at INFO and change nothing. Legs are then checked in sequence under `mu`, so an accepted earlier leg is included in the checks for later legs.
Marks, expiry intrinsic and delivered units use size × quantity; theta harvest, circuit-breaker closes and portfolio value keep the aggregate `CurrentValueUSD`. A short signal close debits per-unit price × stored size × whole quantity. Both encoders send `entry_premium_usd` per underlying unit. On a `physical` platform (Robinhood) an in-the-money sold call delivers at most the held long shares at the settlement price; the uncovered units get no delivery and no strike proceeds (their intrinsic is already in the option debit), with one WARN naming the leg. A `cash` platform delivers no units at expiry. `portfolioNotional` and `computeAssetDeltas` scale short-option notional and option delta by the stored size.
Load: a zero stored size is a legacy row and is stamped in memory with the configured platform's `LegacyMultiplier` (platform inferred as `inferStrategyPlatform` does for options) before any startup save, never with the current size and without an underlying check, so an old IBKR row (also an unsupported one such as SOL) loads at 1 coin; the next save persists it, and a positive stamp is never replaced. A negative or non-finite size fails the load (and both save paths) naming the strategy and leg, and deletes nothing. A read-only open of an older file selects a literal zero for the absent column. Paper options keep negative cash through `ValidateState`. Pricer selection, freshness and settlement source are described in § Option marks and settlement (#1912); other atomic multi-leg structures stay with issue 1919, which must place these gates in its structure planner.
The `rh-wheel-spy` example below runs as paper (`--mode=paper`). The wheel sells its put at `0.94 × spot`, so one contract needs capital of at least `0.94 × spot × 100`, plus a margin: $56,400 at spot 600 (the repository fixtures), $47,000 at strike 470. Its $80,000 capital covers one contract up to spot of about $851; set capital from the current spot before copying it, or every put is refused at INFO and the call phase never starts. A covered call phase uses the shares a put assignment delivers (Robinhood only; § Option marks and settlement for cash platforms). Real entries still need valid market data.
`go-trader init` does not generate `covered_calls` or `butterfly` for Robinhood (they never buy shares, so their short calls can never open) and prints the short-put cash needed per contract; config load prints one `[WARN]` for each existing Robinhood `covered_calls` or `butterfly` strategy and still loads it. The high-IV side of `rh-vol-*` opens nothing without held shares, rather than booking only its short put.

**Options entry structures (#1919).** `shared_scripts/check_options.py` stamps every buy and sell of an evaluator result with `structure_id` (`<strategy>-1`) and `structure_legs` (the entry-row total), and every document (entry, early-cap hold, error) carries `structure_contract: "options_structures_v1"`.
`select_structures` keeps a structure only when its minimum leg score meets `MIN_SCORE_THRESHOLD` and existing rows plus selected rows plus its rows stay at or under `MAX_POSITIONS_PER_STRATEGY` (4; one row per leg, whatever its quantity); a refused structure emits no leg and logs every leg score to stderr.
`scheduler/options_structure.go` owns the Go side. `OptionsAction` decodes `structure_id` and `structure_legs` tolerantly, so a malformed field never fails the whole result: `structure_legs` must be a whole positive JSON number, `structure_id` a non-empty JSON string.
`checkOptionsStructures` validates raw groups before any filter: a missing or wrong marker, or an entry with no usable id, refuses every entry; a malformed identifiable group (bad total, wrong membership, unknown action, bad option type) refuses that group only.
`runOptionsCheck` reports refusals through `notifyScriptFailure` (fixed action-order text, no strikes) and does not clear the failure that cycle; explicit closes and independent valid groups still run, and harvest still runs once.
A platform or premium-unit failure keeps its whole-result refusal. `ExecuteOptionsSignal` repeats the check for direct callers.
`exposureCapOptionsActions` drops every entry of a structure that has one blocked leg; pause, daily-loss, notional and persistence holds still drop every entry.
Order in `ExecuteOptionsSignal` (under `mu`): the aggregate Robinhood short-call cover preflight over the surviving signal (unchanged; insufficient cover refuses every entry), explicit closes, then one plan per structure in first-seen order.
`planOptionStructure` works on a copy of the option map and never writes through an existing leg.
It validates action, type, `YYYY-MM-DD` expiry, finite positive strike, finite quantity (non-positive defaults to 1), finite premium, DTE and Greeks, a positive contract value and a finite non-negative fee.
It refuses a leg that repeats an open contract (underlying compared case-insensitively, then type, action, strike, expiry; legacy and arbitrary ids included) or another leg of the structure, or whose key is already in the map, and refuses rows above `maxOptionPositionsPerStrategy` (4).
It requires all buy costs and fees to fit free cash before any sell premium of the structure, then applies the existing put collateral, post-fee reserve, buy free-cash and Robinhood per-leg cover checks in action order.
One refusal logs the reason at INFO with the structure and leg and books nothing.
`applyOptionStructurePlan` has no gate: it sets cash, inserts the legs and appends the trades with `deferTradePersist`, so the owning file's next state save writes cash, rows and trades in one transaction.
A failed save keeps the structure in memory for retry; a failed `SaveAll` or `SavePartition` also holds entries through the persistence hold.
Keys: `optionContractKey` writes the strike with `strconv.FormatFloat(strike, 'f', -1, 64)`, so integer keys keep their text and 101.5 and 102.5 differ; trade symbols, details and entry logs use the same strike text.
On every load `normalizeLoadedOptionKeys` (called from `StateDB.loadScopeBooks`, memory only) renames a leg only when its stored id equals the old `%.0f` key, the new key differs, its identity is unique in the book and no other row ends with the same final id; duplicates and their collision chains keep their stored ids with a `[state] WARN`.
A rename changes the map key and `ID` together and the matching `robinhood_options` pending-close symbol; `TradePositionID`, financial fields, historical trades and closed records keep their text. The next ordinary save persists the new ids.

**Option marks and settlement (#1912).** `scheduler/option_mark_lifecycle.go` owns the leg lifecycle. After each due strategy runs, the main loop collects one request per leg under `mu.RLock` (a bad expiry, bad size, refused underlying or unknown platform is a request with an error, never a dropped leg), calls `OptionPricer.Refresh` once outside `mu`, and applies one reply per leg under `mu.Lock`, matched by leg key and `TradePositionID`.
Pricers by resolved platform (`optionPricerFor`): `robinhood` runs `shared_scripts/fetch_option_marks.py --platform=robinhood` once per strategy refresh through `runPythonReadOnlyWithStdin` (stdin request and one stdout document with `contract: option_marks_v1`, `platform` and `premium_unit: underlying_unit`; a bad contract, platform or unit, a duplicate or unknown leg id, a malformed number, a second document, a timeout or no output fails every leg, and an omitted leg fails alone).
`deribit` reads the exact `public/ticker` instrument (the nearest-expiry substitute is removed) and settles from `public/get_delivery_prices` for `<ccy>_usd` on the expiry date, paging newest first with strictly descending dates and never taking the newest row (sources `deribit_ticker`, `deribit_delivery_price`).
`okx` uses the same Deribit client labelled `deribit_proxy`: a paper-model proxy, not an OKX mark or settlement price.
`ibkr` runs `fetch_option_marks.py --platform=ibkr` through the same transport (`runOptionMarksBatch`) with IBKR decoding (§ IBKR options calendar, marks and settlement); it needs no cycle price key.
Any other platform has no pricer: every leg errors and expired legs hold.
`optionPricingPolicyError` in `scheduler/option_contract.go` is the one supported-underlying policy, used by `validateConfig` (every loader) and per leg at refresh: a platform outside `optionContractSpecs` is refused, Deribit and OKX accept only BTC and ETH (the inverse families that the `UNDERLYING-DDMONYY-STRIKE-C|P` naming path builds; a local restriction, not a venue listing), and Robinhood refuses an underlying that is not a stock or ETF ticker or is a currency pair (`robinhoodStockUnderlyingError`; `fetch_option_marks.py` repeats it per leg through the stock class). A coin-named ticker such as `LTC` or `BTC` is a valid stock underlying and reads only stock data (§ Robinhood asset classes).
Cutoffs: Deribit and OKX 08:00 UTC on the expiry date, settlement read boundary cutoff + 1 hour (model choice from one observed publication time); IBKR 16:00 Europe/London, boundary cutoff + 5 minutes (from `optionContractSpec.Cutoff`). Robinhood: `shared_tools/option_sessions.py` (`exchange_calendars` XNYS) is the only session owner; the cutoff is the close of the expiry session (early closes included), a non-session expiry maps to the previous session only when the calendar says the date is not a session, an invalid or out-of-bounds date is unresolved, and the boundary is close + 60 minutes (model choice). Go stores the returned `ExpiryCutoffAt` and `SettlementReadyAt` and computes DTE = max(0, cutoff − refresh time) / 1 day whenever the cutoff is known, also when the read fails; with no known cutoff DTE stays.
Robinhood marks: logged in, only the authenticated quote `adjusted_mark_price` (source `robinhood_quote`; a missing or empty field is an error, an explicit `0` is a valid mark, no model fallback); logged out, Black-Scholes with a positive Yahoo spot, measured volatility (a measured value with an unavailable rank is used) and the DTE to the cutoff, where a zero or non-finite model price is an error (source `robinhood_black_scholes`). Settlement (source `yahoo_daily_raw_close`, basis `raw_close`): the Yahoo daily `Close` of the session bar from an explicit window (session − 7 days to refresh date + 1 day, `auto_adjust=False`, `back_adjust=False`, `actions=True`); Yahoo's `Close` is split-adjusted, so any split after the session holds the leg (`split after expiry`). A missing bar for an expected session stays pending, and a later bar never proves a holiday.
A good mark sets the value (`optionContractValueUSD` once), Greeks, `MarkUpdatedAt`, `MarkSource`, clears `MarkError` and sets the process-local `markVerifiedAt`; a failed mark keeps the last value and timestamp, sets `MarkError` and clears `markVerifiedAt`. The five fields persist in `option_positions` (both save paths and the load; a read-only load of an older file selects empty literals); `markVerifiedAt` never persists.
At or after the cutoff a valid settlement books in `settleExpiredOptionLeg`, the only expiry booking owner (#1917). It resolves the platform settlement style (`optionSettlementStyle`) and validates every booking value first; a missing or invalid style, a non-finite settlement price, strike, entry premium, quantity or size, or an overflow returns an error into the settlement-pending path below with the leg, cash, positions, rows and risk state unchanged. Let `I` be intrinsic per unit × stored size × quantity and `S` the settlement price.
Option component, every platform: a bought leg credits `I` and realizes `I − EntryPremiumUSD`; a sold leg debits `I` and realizes `EntryPremiumUSD − I`. It writes one closing `options` trade (symbol = leg id, `PositionID` = the leg `TradePositionID`, contract quantity, `optionCloseTradeSide`, price and value `I`, `IsClose`, `PnLGross`, no exchange fee, strategy regime, details ending `[settlement <source> <basis> <date> $<price>]`), calls `RecordTradeResult` once, appends one closed option record (`expired_itm` when `I > 0`, else `expired_worthless`) and deletes the leg. Every record uses the refresh time. Bought legs are cash exercised on every platform and open no underlying (a paper policy). Entry premium keeps its current basis (bought before the entry fee, sold after it), and no expiry exchange fee is charged.
Physical transfer, only an in-the-money sold leg on a `physical` platform (Robinhood), at `S`, after the option component: a put buys size × quantity shares at `S` (debit `S × units`, so total cash falls by strike × units), opens a long at cost `S` or adds to a held long at the weighted cost, and writes one opening `assignment` trade with no risk result; a held short or corrupt long (quantity or cost basis ≤ 0) is kept, no shares are delivered and one WARN names the leg and the position.
A call sells `min(units, held long)` at `S` (credit `S × delivered`, realized `(S − AvgCost) × delivered`), keeps the lot `TradePositionID`, reduces a partial lot or closes it with one `assignment` closed position, writes one closing `assignment` trade and calls `RecordTradeResult` after the option result; uncovered units get one `UNCOVERED CALL ASSIGNMENT` WARN with the leg id and no second debit, and a corrupt long is not cover (WARN, row kept).
Assignment details keep the `[settlement ...]` suffix.
Legs settle in sorted key order, so two calls consume held shares once.
Cash platforms (Deribit, OKX, IBKR) leave every underlying position unchanged; IBKR cash settlement is a paper simplification with no exercised futures position. These styles are explicit paper policies, not venue parity. Example: a Robinhood sold put (strike 100, `S` 90, premium $300, 100 shares) records option PnL −$700 and 100 shares at 90, so a later sale at 90 realizes 0; a covered call (strike 100, `S` 110, premium $300, 100 shares at 80) records −$700 and +$3,000.
A fresh wheel on a cash platform never receives units, so it stays in the put phase; a cash-platform wheel with a preserved legacy long can still enter the call phase, because the strategy checks held units. Cash-platform covered calls open under the existing entry policy without held units and settle only the option component; Robinhood keeps its covered-call entry restriction.
Startup (`ValidateState` → `legacyCashSettledUnderlyingWarnings`): for each configured options strategy on a `cash` platform that holds a long in its configured underlying, one `[WARN] state:` line in sorted strategy order names the strategy, platform, symbol, quantity and cost basis and states that a cycle with no price key for that symbol values it at cost basis. This works with no open leg; the position and all history stay.
The booking sets `deferTradePersist` (restored after), so its option and assignment rows, cash, positions, closed records, risk state and the delete land in one state-save transaction; a stop or a failed commit before the save reloads the open leg with nothing written, and a retry, repeated refresh or restart books each result once.
With no valid settlement (or a settlement the owner refuses) the leg stays with DTE 0 and `MarkError: settlement pending: <reason>`, and no close, assignment or cash change is booked; it retries every refresh. An unknown platform errors earlier at request collection: with no stored cutoff it holds as `expiry cutoff unresolved`, with one it holds as settlement pending.
Only settlement removes a leg at or after its cutoff (`optionLegAwaitingSettlement`: the Go schedule when the platform has one, else the stored `ExpiryCutoffAt`, else 00:00 UTC on the expiry date; a stale stored IBKR midnight cutoff never applies): the circuit-breaker and kill-switch force close (`forceCloseAllPositions`) and a signal close (`executeOptionClose`) skip it with a WARN. The paper kill switch lists a strategy as force-closed only when it closed something, and its message, auto-reset text and `[CRITICAL]` log name each kept leg.
Before the read boundary there is no alert; at or after it (or when the cutoff is unresolved after the UTC expiry date) a CRITICAL `OPTION SETTLEMENT PENDING` goes outside `mu` to `SendOwnerDM` and `SendToPartitionChannels(partitionFor(sc))`, throttled per partition, strategy, leg and position by `alert_throttle_interval` in main-loop memory (pruned when the leg leaves; a restart may repeat it).
Mark-read failures use the script-failure trackers under the key `<id>#option_marks` (`OPTION MARKS FAILING`, ordinary at 3 refreshes, upstream 429/5xx at 15 or 75 minutes) and clear only when every leg marks or settles, so an entry-check success never clears them.
Theta harvest eligibility (`optionHarvestEligibility`): `markVerifiedAt` set, no `MarkError`, mark age at most 1.5 × `configuredStrategyIntervalSeconds` (read live, so a SIGHUP interval change applies at once), a known cutoff and now before it. Profit, stop and DTE exits all need it, and the DTE exit uses the DTE to the cutoff at harvest time. A restart, a failed or skipped refresh, or an entry-only value holds the leg (INFO `Theta harvest holds <leg>: <reason>` once per change); the first options cycle after a restart never harvests. Harvest still runs once per options cycle after a successful, failed, unspawned or encoding-failed entry check.
Operator output: Discord positions show `mark <age> ago (<source>)` or `mark unknown`, then `| <mark error>`; the dashboard option rows show the same;
`/status` serializes the fields (an unknown timestamp is omitted, never shown as fresh).
Startup probe: with any Robinhood options strategy, `fetch_option_marks.py --platform=robinhood --probe-only` must import `option_sessions`, build the XNYS calendar and print exactly one `{"contract":"option_marks_v1","platform":"robinhood","probe":true}` document with no login or data read, else exit 78.
With any IBKR options strategy (`ibkrOptionsCheckScripts`), `fetch_option_marks.py --platform=ibkr --probe-only` must load the IBKR adapter, `ccxt`, `option_data` and `pricing` and print `{"contract":"option_marks_v1","platform":"ibkr","probe":true}`, and each IBKR options check script must accept `optionCalendarProbeArgv` and print `{"contract":"option_calendar_v1","platform":"ibkr","probe":true}` (an older script that exits 0 with no document fails), else exit 78.
Not changed (separate work): OKX venue marks and exact OKX settlement; circuit-breaker force close at the stored aggregate value for a leg before its cutoff; existing `expired_worthless` rows.

**IBKR options calendar, marks and settlement (#1918).** Go owns the size, the cutoff (16:00 Europe/London, resolved through the named zone with embedded `time/tzdata`) and the read boundary (cutoff + 5 minutes); Python reads data. Paper only; BTC and ETH only (`optionPricingPolicyError` refuses others at config load with the strategy, platform and underlying, and per leg at refresh, while an old row still loads for inspection).
Entry: `runOptionsCheck` adds `--option-calendar-json=<json>` (`optionCalendarArg`: `contract: option_calendar_v1`, platform, underlying, `evaluated_at`, `cutoff_zone`, `cutoff_hour`, `cutoff_minute`) only when the check's own `--platform=` argument (`optionsCheckArgsPlatform`, default `deribit`) names a platform whose spec has a `Cutoff` (IBKR), so a strategy whose config and `--platform=` disagree still reaches the result-contract refusal.
It is a runtime argument, never stored in config, and is not in `probeArgv`/`probeCompositeArgv` because only `check_options.py` receives it; the dedicated calendar probe covers the version contract.
`check_options.py` validates it when the adapter loads (wrong platform or underlying, naive clock, unknown zone, bad hour or minute: one error document, exit 1); a platform without `parse_calendar_context` refuses it.
`IBKRExchangeAdapter.get_real_expiry` requires it (`expiry calendar unavailable: ...` without it) and never uses a Python default cutoff.
Paper candidate calendar (`candidate_expiries`): monthly expiries on the last Friday of six consecutive unexpired months, the next four quarterly months (Mar, Jun, Sep, Dec), and a second December when only one is listed; one Monday, Tuesday, Wednesday and Thursday weekly series each; the four nearest Friday weeklies that are not monthly Fridays; and during the rollover overlap (from two weekdays before a series expires) the next series of that weekday (a fifth Friday).
Candidates whose 16:00 London cutoff is at or before `evaluated_at` are dropped; weekends never appear.
`select_expiry` picks the nearest calendar-day distance from the London date of `evaluated_at` to the target, the later date on a tie, and returns the true distance.
Existing legs keep their stored expiry.
This is a paper approximation: UK and US holiday exclusions and monthly holiday date adjustments are not modelled, so a candidate (for example 2026-12-25) is not guaranteed to be a listed contract.
Venue evidence (CME Micro Bitcoin and Micro Ether specifications, rulebook chapters 348A and 347A, and the 2023 clearing notice 23-165 for the two-business-day rollover) was read on 2026-11-04 by the issue and plan authors; the build session could not reach the CME pages, so the listing horizon is recorded as their reading.
Marks: the IBKR request legs carry `cutoff_utc` and `settlement_ready_utc` from Go;
`fetch_option_marks.py` validates them (present, ISO with zone, ready not before cutoff, cutoff on the expiry date; else a batch error) and echoes them on every leg.
Before the cutoff it marks with a finite positive BinanceUS spot, `option_data.measured_vol_metrics` (the entry helper; a measured volatility without a rank is used, a missing one is a leg error) and `pricing.bs_price_and_greeks` with DTE to the cutoff (source `binanceus_black_scholes_hv14`); spot, volatility and settlement reads are cached per batch only.
Between cutoff and boundary it returns `pending` with no market read.
At or after the boundary it reads BinanceUS one-minute candles for `<U>/USDT`, then `<U>/USD`, from cutoff - 30 minutes (`since`, `limit` 30): one symbol must give exactly one candle per expected minute (rows outside the window are ignored and cannot fill a gap; a duplicate, off-grid, missing, non-finite or non-positive close, negative volume or zero total volume refuses that symbol), never mixing symbols; the result is the volume-weighted close (`basis: spot_candle_close_vwap`, `source: binanceus_spot_vwap_1530_1600_london`, `date` = expiry), else `pending` with every symbol's reason.
Go side: `ibkrOptionMarkReply` requires the echoed times to equal the expected ones (else a leg error), the mark source, and the settlement basis, source and expiry date.
`applyOptionLegReplies` never copies a reply schedule into a leg whose schedule Go resolved (`optionLegRequest.GoSchedule`; a changed time becomes a leg error and DTE still follows the Go cutoff), and for a platform with a `Cutoff` it turns any settlement before the boundary into a wait with no booking and no alert.
`optionSettlementError` accepts only the dated VWAP contract for IBKR; the legacy `cycle_price_legacy_model` basis is refused.
Settlement then goes through `settleExpiredOptionLeg` (cash: a sold leg debits intrinsic x stored size x quantity, a bought leg credits it, no coin delivered).
The real micro options deliver a futures position and fix on the futures price (weekly) or the reference rate (monthly); the spot-candle VWAP and direct cash settlement are paper approximations of both.
IBKR fee accuracy against the broker is unverified.

ID conventions: `ts-{strategy}-{symbol}`, `rh-{strategy_short}-{asset_or_symbol}`, `okx-{strategy_short}-{asset}` for spot and options, `okx-{strategy_short}-{asset}-perp` for perps. Options short names: `vol_mean_reversion → vol`, `momentum_options → momentum`, `protective_puts → puts`, `covered_calls → calls`, `wheel`, `butterfly`.

Example entries:

```json
{"id":"momentum-btc","type":"spot","script":"shared_scripts/check_strategy.py","args":["momentum","BTC/USDT","1h"],"capital":1000,"max_drawdown_pct":60,"interval_seconds":300}
{"id":"ts-momentum-es","type":"futures","platform":"topstep","script":"shared_scripts/check_topstep.py","args":["momentum","ES","1h","--mode=paper"],"capital":1000,"max_drawdown_pct":5,"interval_seconds":3600}
{"id":"rh-wheel-spy","type":"options","platform":"robinhood","script":"shared_scripts/check_options.py","args":["wheel","SPY","--platform=robinhood","--mode=paper"],"capital":80000,"max_drawdown_pct":10,"interval_seconds":14400,"theta_harvest":{"enabled":true,"profit_target_pct":60,"stop_loss_pct":200,"min_dte_close":3}}
{"id":"okx-sma-btc-perp","type":"perps","platform":"okx","script":"shared_scripts/check_okx.py","args":["sma_crossover","BTC","1h","--mode=paper","--inst-type=swap"],"capital":1000,"max_drawdown_pct":5,"interval_seconds":3600}
```

**Peers on one Hyperliquid coin** share a single on-chain position. They must agree on `margin_mode` and exchange `leverage`; `sizing_leverage` may differ. Each peer places its own per-strategy-sized reduce-only protection, so several peers may own fixed-ATR, margin, or trailing stops at once. Peers that omit every stop field fall back to `default_stop_loss_atr_mult`. A per-strategy circuit-breaker drain skips the on-chain close when peers share the coin, so the exchange leg stays open until another path flattens it. Sub-account isolation is the only route to full per-strategy independence.

---

## Add Or Change Strategies

Open registry: `shared_strategies/open/registry.py`. Close registry: `shared_strategies/close/registry.py`.

New spot or futures strategy:

1. Add the implementation and its `@register(...)` in `shared_strategies/open/registry.py`. Declare the required `short_entries=True|False`: True only when the strategy ships short entries (a -1 that opens a short, not a long exit). A missing or non-bool value fails at import. True needs `"futures"` in `platforms`, and any shipped `allow_short: True` (base or variant) needs True. `short_entry_strategies()` derives the fee-audit short set (`LIVE_BIDIRECTIONAL_STRATEGIES`).
2. Set `platforms=(…)` correctly; use variants for platform-specific defaults.
3. Append the name to `PLATFORM_ORDER`.
4. Add the short name to `knownShortNames`, the `registeredOpenStrategyPlatforms` row and default entries in `scheduler/init.go`; with `short_entries=True`, also add it to `bidirectionalPerpsStrategies` (wizard and Discord add write `direction: both` only for listed names).
5. Add a param grid to `DEFAULT_PARAM_RANGES` in `backtest/optimizer.py`.
6. A parameter rule the binary `constraints` cannot express (integer-only, finite, type checks) goes in an optional `param_validator=` callback on `@register`: `validate_params` runs it on the effective parameters, and `check_hyperliquid.py` runs it for every admitted open and open-fallback close reference before any candle fetch (solo) or shared-state build (batch), failing only that slot.
7. A strategy without approved edge evidence registers `edge_status="no_edge"` with a valid `edge_source` and a nonempty `edge_ref` (the evidence file); mirror it in `noEdgeStrategies` in `scheduler/edge_status.go`. That hides it from discovery, runs it with explicit `--mode=paper`, and needs `allow_no_edge: true` for live use. A new study records its verdict without changing that rule. Never add the name to `v20StampEligibleStrategies` in `scheduler/config_migration_v20.go`: that frozen list holds only the no-edge names that could run live before v20, so the legacy v20 migration never stamps `allow_no_edge: true` on a later name.
   A fixed candle-cadence parameter (`expected_interval_ms`) also needs its registry default in `fixedIntervalStrategyDefaults` (`scheduler/fixed_interval_strategies.go`): config load refuses an open or close-fallback reference whose explicit or default value differs from the `args` timeframe (a Hyperliquid manual strategy without `args` uses `timeframe`), or that runs outside perps and Hyperliquid manual strategies, and the backtest harnesses fill it from `--timeframe` (`bind_fixed_interval_params` in `shared_tools/closed_bar.py`) and refuse an explicit mismatch.
   A research candidate that must never run live also registers `production_refused=True` (requires `no_edge`) with `paper_route=RESEARCH_PAPER_ROUTE`, and is mirrored in `productionRefusedStrategies` and `researchPaperRouteStrategies` (`scheduler/research_admission.go`); the parity script pins the route and the quote engine hold constants.
   `researchReferences` resolves every reference after defaults (positional and effective opens, open-fallback close, native research close, quote block).
   Exactly one explicit paper mode (`--mode=paper` or `--mode`, `paper`) on the Hyperliquid perps `check_hyperliquid.py` route with `market_feed` `websocket` or `shared` is admitted; explicit live, missing, invalid, dangling and duplicate modes are refused with or without `allow_no_edge`, and live is refused until a separate reviewed promotion.
   A research name in a close slot is admitted only when it equals the effective open (sealed inputs attach only to the open).
   An options entry with any quote reference takes the quote-owner path and is refused, because quote ownership is Hyperliquid perps only.
   `bitcoin_lead_lag` refuses BTC as its follower coin (the evaluator would hold every bar with `follower_is_leader`) at load and in the add builder, and the wizard skips BTC for it and refuses a run with no other asset.
   Python mirrors the mode, route and same-name rules in `admit_strategy_references` (explicit paper needs the sealed `--market-stdin` route) and refuses a native research close (`inventory_quote_exit`) on every scalar path.
   `cumulative_volume_delta_divergence` (#1638), `bitcoin_lead_lag` (#1640), `macro_event_breakout` (#1654), `volume_profile_reclaim` (#1665), `perpetual_premium_reversion` (#1639) and `order_book_imbalance` (#1635) are scalar research names with `knownShortNames`, `bidirectionalPerpsStrategies` and `addStrategyResearchCandidates` rows (#1923); the quote capability `inventory_market_making` (#1861) gets a short name but no bidirectional or scalar add row.
   The removed basket and pair research (#1955) stays retired.
   Admission tests for a new no-edge strategy: one missing-mode refusal (no `--mode`) that asserts the strategy name and `edge_status=no_edge`, plus one admitted `--mode=paper` evaluation (reuse a behavior test that already runs one; add one only when none does).
   A close-fallback reference is a separate path and keeps its own single refusal.
   The full ten-input refusal matrix runs only once, against `check_hyperliquid.py` in `shared_scripts/test_no_edge_admission.py`, beside one case per refusal class for each other check script; never copy either matrix per strategy (`scripts/check_test_budget.py` rejects a copy).
   Strategy contract suites (#1885): the shared harness contract (registration metadata, admission and routing, batch slots, closed-bar selection and holds, composer, simulator fills, backtester parity, parameter preflight) runs once per strategy as a case of a registry-driven suite.
   A new single-symbol study candidate (futures only, `short_entries=True`, `no_edge`, `edge_ref` under `backtest/candidates/`, no quote) adds one `CASES` entry in `backtest/tests/test_strategy_backtest_contract.py` (production-refused side-input candidates too), with every family configured or `NotCovered` with a reason, and one `CASES` entry, or an `EXEMPT` entry with its reason, in `shared_scripts/test_strategy_check_contract.py` (a production-refused scalar also adds its `RESEARCH_CASES` explicit-paper evaluation; quote capabilities stay excluded); each suite's registry guard fails until it does.
   Never copy a contract test into a per-strategy file: `scripts/check_test_budget.py` refuses a test outside the suites that reuses a contract test name or a name listed in a suite's `REPLACED_PER_STRATEGY_TESTS`.
   A per-strategy file keeps only behavior unique to that strategy: signal values against an independent reference, its own parameter and history bounds, and its own closed-bar edge and reason vocabulary.
8. Run the registry, optimizer and `scripts/test_go_python_registry_parity.py` tests.

New quote capability (decides two-sided quotes at due deadlines through the Quote Lifecycle coordinator, never a scalar signal; #1861):

1. Put the versioned parameter specification (units, defaults, bounds, integer and cross-field rules) once in Go as a raw-string constant (`quotePolicySpecJSON` in `scheduler/quote_policy_spec.go`) and once as a self-contained Python literal beside the registration (`shared_strategies/open/inventory_market_making.py`); the registry never reads a Go file at import, because the container image ships no Go source.
   `scripts/test_go_python_registry_parity.py` pins the two copies equal and their canonical SHA-256 equal (`quote-replay discover` prints the binary's copy).
2. Register it with `quote=dict(QUOTE_CONTRACT)`, `quote_spec=`, `platforms=("futures",)`, `short_entries=False` (signed inventory belongs to the quote contract), a `param_validator`, `edge_status="no_edge"` and `production_refused=True`; registration refuses any other combination.
   The registered function is a scalar refusal, so composer, check scripts, candle backtests, `walk_forward_optimize`, `parity_diff` and close fallback refuse it (`refuse_quote_capability` in `backtest/registry_loader.py`, the quote guard in `admit_strategy_references`).
3. Append it to `PLATFORM_ORDER["futures"]` so the hidden futures lookup resolves its production refusal, and give it an empty `DEFAULT_PARAM_RANGES` row (no optimizer admission); add a `knownShortNames` entry but no `bidirectionalPerpsStrategies`, `v20StampEligibleStrategies` or scalar wizard entry (owners come only from the typed quote_owner setup), and never edit the shims.
4. Mirror it in Go: the quoted key in the `quoteCapabilityStrategies` map literal (version and validator; no `init()`), `productionRefusedStrategies`, `researchPaperRouteStrategies`, `noEdgeStrategies` and `registeredOpenStrategyPlatforms`; register its book need through `feedBookStrategyNeeds` (`inventory_market_making_feed.go`).
5. Add it to every exact production-refused table (the parity literal, `TestBitcoinLeadLagRegistrationTables`, `test_production_refused_set_is_the_side_input_cases_plus_the_non_candle_capabilities` in `backtest/tests/test_strategy_backtest_contract.py`), the role matrix in `TestCumulativeVolumeDeltaIsRefusedInEveryModeAndReference`, and the forward-peek `SKIP_STRATEGIES` with its ground.

For a close evaluator, add an `evaluate(position, market, params)` implementation under `shared_strategies/close/` and register it in `close/registry.py`.

Do not edit `shared_strategies/open/{spot,futures}/strategies.py` — they are thin shims.

Before refactoring a registry or shim, snapshot discovery and diff it afterwards unless the change is meant to alter discovery:

```bash
uv run --no-sync python shared_strategies/open/spot/strategies.py --list-json > /tmp/spot.json
uv run --no-sync python shared_strategies/open/futures/strategies.py --list-json > /tmp/futures.json
```

---

## Custom Platform Integration

Gather first: platform name and ID prefix; products (spot, perps, futures, options); API docs URL or `ccxt` coverage; credential environment variable names; fees; assets and strategies; paper and live requirements.

1. `platforms/<name>/__init__.py`
2. `platforms/<name>/adapter.py` — exactly one class whose name ends in `ExchangeAdapter`; check scripts load the file through `importlib` and pick the class by `endswith("ExchangeAdapter")`
3. Implement public adapter methods only; check scripts must never touch private attributes. A shared loader (`check_options.py`, `check_regime.py`, `fetch_option_marks.py`, `llm_review.py`, `fetch_candles.py`) passes `asset_class` only to the Robinhood adapter and validates the Robinhood symbol before it builds the adapter; other constructors take no class (§ Robinhood asset classes)
4. `shared_scripts/check_<name>.py`, only if an existing entry script does not fit. Go parses stdout as exactly one JSON document, so a venue SDK that prints must run inside an adapter guard that sends its output to stderr (Robinhood: `robinhood_library()` in `platforms/robinhood/adapter.py`), and an entry script that can print other library text writes its one JSON result through `json_stdout()` from `shared_tools/stdout_contract.py` (Robinhood: `check_robinhood.py`, `close_robinhood_position.py`, `fetch_robinhood_positions.py`, `check_balance.py`, `check_options.py`; `fetch_candles.py` relies on the adapter guard alone).
   Robinhood live orders go only through the adapter-owned `place_market_order` and `resolve_order` (`platforms/robinhood/adapter.py`): every request has an explicit timeout inside the `--budget-ms` budget, login runs under a watchdog, and the result carries one `order_outcome` (`filled`, `partially_filled`, `no_fill`, `rejected`, `not_sent`, `not_found`, `open`, `unknown`, `inconsistent`; the close script adds `already_flat`).
   The scripts exit 0 only for a confirmed fill or `already_flat`, and Go books only a confirmed fill (#1815)
   A signal sell passes `sell_policy` to `place_market_order`: the adapter reads the pair rules and `quantity_available`, sizes the order with `plan_sized_sell` (exact `Decimal`, floor to the increment, sole-owner whole-close dust sweep) and echoes `sizing`; every refusal happens before the send (#1819). The emergency close script uses the same path with close kind `emergency` and the go-trader book as the ask, which never sweeps (#1825).
   A buy sends a `gtc` limit from `plan_sized_buy` (#1910): limit L = the ask times 1.005 rounded up to `min_order_price_increment`, quantity Q = the budget divided by L floored to `min_order_quantity_increment` and capped at `max_order_size`, with Q times L at or below the budget checked in exact `Decimal` and both wire numbers exact; every refusal happens before the send. A sent buy and a buy refused for too little remaining time after the account reads echo `buy_sizing` (the time refusal has `order_outcome` `not_sent`); every other refusal sends no `buy_sizing` and carries the plan values only in its error text.
   Every sell keeps `type: market` and takes its price from `plan_sell_price`, which never refuses: the bid floored to 6, 4 or 2 decimals (as the robin_stocks rounding helper picks) and then to the price increment when it is valid (`floor_decimals`, `floor_increment`), at or below the bid and the old rounded price; a zero result uses the old rounded price (`round_fallback`) or, when that is zero, one price increment or 0.000001 (`one_step`), and these two can be above the bid or off the increment grid; a value with no exact JSON form or a decimal failure moves down to the next float (`wire_floor`).
   The result echoes `sell_price`; Go decodes `buy_sizing` and `sell_price` apart from the core result, so a malformed echo is dropped with a WARN and never blocks a confirmed fill.
   Every Robinhood login goes through `robinhood_login()` in `platforms/robinhood/adapter.py`, which accepts a login only with a returned token, the library login state set, and the matching `Authorization` header, and which passes robin_stocks login text through an allowlist so no credential or token reaches stderr.
   `robinhood_login()` also owns the session (#1818): it checks the `GO_TRADER_ROBINHOOD_SESSION_DIR` folder before any request, takes the per-account lock under one watchdog, gives robin_stocks an explicit stage `pickle_path`, promotes the stage only after a validated login, reads the stored session with a global-refusing unpickler, keeps a stable per-account device token, and refuses a device verification prompt with a latch that only `go-trader robinhood-login` clears.
5. ID-prefix inference in `scheduler/config.go`
6. Fee dispatch in `scheduler/fees.go`
7. Executor wiring, only if a new live execution path is needed
8. Config examples
9. The init wizard and `generateConfig`, if the platform is user-selectable
10. `pyproject.toml` for any new SDK dependency, then `uv sync`
11. Egress proxy mode: build every client through `shared_tools/egress_policy.py` (`pin_ccxt`, `pin_requests_session`, `requests_proxies`, `pin_yfinance`, `pin_hyperliquid_sdk`), add the family to `--check-policy` in `shared_tools/egress_bootstrap.py`, and use `egressHTTPClient` / `egressWebsocketDialer` for a Go client (§ Egress Proxy Mode)
12. Tests only where CLAUDE.md § Testing allows them (venue states, money math such as fees, paper/live parity); otherwise prove the platform with the real build and `--once` run below

A check script that receives `--htf-filter` must return the `htf_filter` block from `shared_tools/htf_filter.py` (`run_htf_filter`, `htf_skipped_block`, `htf_not_evaluated_block`); with the filter on, Go treats a result without it as a script error (§ Higher-timeframe filter).

Options platforms also need volatility, expiry, strike, and premium helpers, `CalculateOptionFee`, an `OptionPlatforms` entry, an `optionContractSpecs` size and settlement style (`cash` or `physical`; a missing or other value holds every expired leg as settlement pending), a `LegacyMultiplier` for unstamped rows (and per-underlying sizes in `optionContractUnderlyingMultipliers` when the size differs by underlying), an optional `Cutoff` (zone, hour, minute, read delay) when Go owns the expiry time, an `optionPricingPolicyError` underlying rule (Robinhood: stock-ticker syntax and pair refusal, never crypto-table membership), and an `OptionPricer` (`Refresh` returning one reply per leg with cutoff, read boundary, and a labelled mark or dated settlement) selected in `optionPricerFor` (§ Option marks and settlement). Reference adapters: spot `binanceus`, perps `hyperliquid`, futures `topstep`, options `deribit`.

```bash
uv run --no-sync python -m py_compile platforms/<name>/adapter.py
uv run --no-sync python -m py_compile shared_scripts/check_<name>.py
/opt/homebrew/bin/go -C scheduler build .
./go-trader --config scheduler/config.json --once
```

---

## Storage Ownership

Set `paper_db_file` and the scheduler runs both modes in one process against two state files. `db_file` is the **primary** file; `paper_db_file` is the **paper** file. Omit `paper_db_file` and every behaviour below collapses to the single-file layout.

**Several paper sources.** Root `paper_sources` folds more than one paper deployment into the same process, each with its own file. Every entry carries `id` (the stable identity: `[a-z0-9][a-z0-9_-]{0,31}`, never `live`, `paper` or `primary`), `db_file` and an optional `label` and `portfolio_risk` override. A paper strategy joins a source with `paper_source: "<id>"`; a live strategy that carries one is refused at load, as is a `paper_source` naming no declared entry, a duplicate id and a source `portfolio_risk.paper` (a source override cannot nest another override).

The identity that matters at runtime is the **risk partition**: `live`, `paper` (the default paper partition) or `paper:<id>`. A partition is one independent risk, evaluation and storage domain — its own peak, drawdown, latch, daily-loss ledger, notional total, exposure total, correlation model and state file. `paper_sources[].db_file` must be pairwise distinct and distinct from `db_file` and `paper_db_file`; an alias exits 80. One partition owns exactly one file, and the lock and save order is primary, then paper, then the sources by id.

`paper_sources` entries, their `db_file`, and any strategy's `paper_source` are **restart-required**: a SIGHUP that adds, removes or repoints one is rejected. So is a source's `portfolio_risk.max_notional_usd`, exactly as the root and `paper` ones are.

Supported removal (`remove-strategy` on Discord and the dashboard) refuses while the strategy holds any `positions` or `option_positions` row, while it is not paused in the running roster, while it is a replay mirror, and while a queued manual action or resting limit row names it. A pause reaches the roster only through a reload, which runs between cycles, so no cycle can open a position after the check. Both surfaces restart at once after the write, and dashboard open and add refuse a removed strategy until then (the CLI `manual-open` and `manual-limit` read the disk config, which no longer lists it), so supported removal never leaves rows for the startup orphan prune; only a hand edit of the config can still reach the prune WARN.

**Who owns what**

| Table | Owner | Notes |
| --- | --- | --- |
| `app_state` | primary | The paper file's row is read once at boot and its summary/leaderboard stamps are unioned in with primary precedence; it is never written again. |
| `strategies`, `positions`, `option_positions`, `trades`, `closed_positions`, `closed_option_positions`, `trade_diagnostics`, `pending_manual_actions` | the strategy's scope | Identifiers are translated in both directions at the SQL boundary. `strategies.paper_funding_state` (the paper funding timeline, no strategy ids inside) commits with cash and trade rows in the owning file; without `paper_db_file` default paper strategies share the primary file under paper scope ownership. |
| `portfolio_risk`, `kill_switch_events`, `correlation_snapshot` | the row's explicit scope | A legacy unscoped row is placed using the owning file's validated scope. |
| `wallet_ledger_state`, `wallet_transfers`, `cashflow_journal`, `cashflow_journal_state`, `cashflow_hl_basis_state`, `cashflow_hl_basis_book`, `cashflow_hl_basis_seen`, `pending_limit_orders` (manual rows, `owner_kind = 'manual'`) | primary, live-only | A write naming a paper-scope strategy is refused. Every legacy pending-limit read, insert, row-id update, delete, cancel flag, operator marker and tally filters `owner_kind = 'manual'`, so it can never select a quote row in the same file. An unmigrated read-only file projects every row as manual. |
| `pending_limit_orders` (quote rows, `owner_kind = 'quote'`) | the quote owner's partition | Quote order lifecycle (#1860): explicit `partition_scope`/`partition_source`, `quote_role` (`bid`, `ask`, `stop`, `close`), `quote_generation` (the generation for bid/ask, the per-role sequence for stop/close), provider, account key, venue namespace, the frozen canonical `request_json`, `request_expiry_ms`, client and venue identities, `submit_state`, `cancel_state` and exact `confirmed_qty`/`applied_qty`. Unique per (provider, account, client id) and per (partition, strategy, role, generation). Triggers refuse an invalid quote identity, any change to a quote row's identity or frozen request, and every delete. A quote row is addressed only with its file, partition and strategy; a numeric id alone selects nothing. |
| `quote_contracts` | the quote owner's partition | One row per quote owner: coin, provider, account key and namespace (unique together, so ownership is exclusive), both fingerprints, the start time, the monotonic book `revision`, the last committed deadline, the applied coverage boundary, the generation/stop/close sequences, the exact book (`book_json`: signed quantity, cost basis, cash, realized gross, fees, funding, position id, first opening fill time, risk anchor) and the durable operator hold. Every full, partition, subset and single-strategy save validates each durable owner first: an absent or stale revision, an omitted owner in a full save, and a changed cash, position or `paper_funding_state` all refuse before any row is replaced. |
| `quote_generations` | the quote owner's partition | One row per generation and deadline (unique per owner and deadline): desired sides, the reserved risk envelope inputs, status (`reserved`, `active`, `resolved`) and the terminal proof. |
| `quote_fills` | the quote owner's partition | Immutable raw fill evidence keyed by (provider, account, namespace, coin, order, event time, trade id, kind) with the canonical body and digest, so an exact repeat and a conflicting copy stay distinguishable. `kind = 'fee_correction'` is a separately keyed authoritative fee adjustment. `settlement_id` is set once by the settlement that applies it; triggers refuse any other update and every delete, so unapplied and completed evidence survive strategy and position replacement. |
| `quote_settlements` | the quote owner's partition | One immutable row per committed settlement: new revision, kind (`fills`, `funding`, `both`), deadline, applied coverage boundary, applied fill ids and funding ids, and the commit summary. |
| `quote_funding_evidence` | the quote owner's partition | Live funding events for a quote-owned coin, saved by `ingestFundingEvent` instead of a trade row, keyed by (provider, account, namespace, coin, funding dedup id) with the raw body, a digest and the applying settlement. Retained and immutable like `quote_fills`. |
| `regime_window_history`, `regime_window_transitions`, `regime_reversal_alerts` | primary for new writes | A paper file's pre-split history is read with its source named and is never pruned or marked. Every key read, insert, delete, signature update and conflict key includes `asset_class` (#1909); the migration adds the column (empty default) to both history tables, rebuilds the reversal table with `PRIMARY KEY (platform, asset_class, symbol, timeframe, spec_json)` copying rows with class `''`, and replaces `idx_regime_hist_key`/`idx_regime_trans_key` with `idx_regime_hist_class_key`/`idx_regime_trans_class_key`, all in one transaction. A read-only open of an unmigrated file selects an empty class. |
| `entry_decision_state` | the strategy's partition | Closed-bar entry allowance (#1769): per `(scope, source, stored strategy id, symbol, interval_ms, target_side)` the newest observed decision candle, the upgrade bootstrap boundary and the consumed boundary. Boundaries only move forward (`MAX` upsert), the row commits with the book in the owning file, and no foreign key ties it to `strategies` or `positions`, so a whole-book save, a partition save or a deleted position never removes it. |
| Retired research tables: `basket_contracts`, `basket_rebalances`, `basket_member_actions`, `basket_member_state`, `basket_replay_*`, `fill_cost_policies` and the seven `pair_*` tables (`pair_quote_event_uses`, `pair_funding_events`, `pair_funding_inventory`, `pair_funding_chain`, `pair_virtual_events`, `pair_native_resolutions`, `pair_virtual_allocations`) | the file that holds them | Present only in state files written before #1955 by the removed basket and pair research feature (§ Retired research: basket and pair). No code reads or writes them, and a new file never creates them; an old file keeps every table, row, index and trigger unchanged across load and save (no `DROP`). `portfolio_risk.pair_valuation_incomplete_since` stays on every file (fresh files get `''`) and its stored text is carried through each save verbatim (`retiredPairValuationSince`). A file that holds `basket_replay_runs` is refused by storage inspection (exit 80) as retired offline replay state. Retired `entry_order_intents` rows (owner mode `pair` or `basket`, decision kind `quote`, or a `pair:`/`basket:` request kind) keep the table's DDL text; recovery and `/resolve-entry` refuse them with the archive tag (`researchGroupOwned`). |
| `quote_replay_runs`, `quote_replay_venue_events`, `quote_replay_decisions`, `quote_replay_checkpoints`, `quote_replay_deadline_results` | the replay run's own file | Offline quote replay records (#1861), written only by `go-trader quote-replay` in a state directory it created; the general schema and migration never create them (the runner creates them in one transaction after it holds ownership, before binding). `quote_replay_runs` holds exactly one marker row (owner, protocol name, and the protocol, source, data, portable config, policy, model and run fingerprints, owner and stored strategy ids, deadline window, cursor). `quote_replay_venue_events` is the paper venue's immutable memory in sequence order (issued ids, placements, cancels, fills, triggers and venue cancels with effect times and digests), each committed before the coordinator observes it. `quote_replay_decisions` (immutable intent per due deadline), `quote_replay_checkpoints` (venue queue state at the deadline) and the pending `quote_replay_deadline_results` row commit inside the quote settlement transaction through `WriteQuoteProgress`, which also moves the cursor and refuses when a persisted fill at or before the applied boundary is unsettled; the result row is completed after `RunDeadline` returns. `storage-inspect` lists replay runs and refuses a file that holds the marker (also an empty marker table), so the scheduler never starts on replay state, also after a copy or rename. |
| `entry_order_intents` | the strategy's partition | One row per live closed-bar entry attempt (#1769): decision identity, provider, account key (non-secret venue account binding since #1771), client order id (and the initial Hyperliquid stop client id), the frozen request, venue evidence, status `reserved`, `submitting`, `settling`, `committed` or `no_fill`, and the replay events a committed source entry still owes. A partial unique index allows one unresolved attempt per strategy; another unique index forbids reusing a client order id. Completed rows are kept; an operator release (#1771) keeps its audit in `evidence_json`. The table was rebuilt once (idempotent, indexes recreated) with `decision_kind` (`candle` for every existing row, or `quote`), `quote_event_id`, and an immutable saved owner `owner_mode` (`''`, `basket` for rows whose request kind is `basket:`, or `pair`) with `owner_group_id`; a `candle` row keeps `interval_ms > 0`, a `quote` row needs `interval_ms = 0`, a quote event and the `pair` owner. Ordinary entry recovery, the operator release and the ordinary transition refuse a pair-owned row from the saved owner as well as from configuration. A progress-only write never advances a pair-owned row's applied totals. |
| `robinhood_order_attempts` | primary, live-only | One row per live Robinhood order attempt (#1815): stored strategy id, account key (a SHA-256 fingerprint of `ROBINHOOD_USERNAME`), coin, origin (`signal`, `circuit_breaker`, `kill_switch`), side, the go-trader `ref_id`, venue order and account ids, the frozen request, venue evidence and status `submitting`, `open`, `unknown`, `settling`, `settled` or `no_fill`. The row commits before the order subprocess starts. Partial unique indexes allow one unresolved attempt per account and coin and per strategy. `settled` is written only in the transaction that saves the book and trade rows. Completed rows are kept. Startup and `storage-inspect` reject an unresolved row in a file that does not own live or owned by no configured live Robinhood spot strategy. A signal sell freezes request version 2 (`close_kind`, `sole_owner`); its evidence keeps the first validated `sizing` write-once, and `requested_quantity` stays the venue order quantity (#1819). A kill-switch or circuit-breaker sell freezes request version 3 (close kind `emergency`, `quantity` = the remaining book asked, never a sole-owner sweep) and its `sizing` also records the total `account_quantity`; recovery uses the saved sent quantity and holds when it is unknown (#1825). A sell also freezes `expected_account_id` (#1921). The evidence keeps the first valid optional `buy_sizing` and `sell_price` echoes across later outcome writes, recovery and an operator release, and a buy's `requested_quantity` is the sent limit quantity, which recovery searches for (#1910). The book's own binding lives in `positions.account_key`, `venue_account_id` and `account_binding_source` (§ Robinhood account binding). |
| `decisions` (replay log) | unchanged | Its own `replay_log_path` file. A source entry published from an `entry_order_intents` row carries `source_entry_event_id` (unique when set), so a retried publication never adds a second decision. |

**Identity translation.** A strategy's process identifier (`id`) and its stored identifier (`storage_strategy_id`, default `id`) are separate namespaces. Every strategy identifier is translated inside the per-file handle, so no caller can skip it. Values that stay in the process namespace: `position_id`, `replay_mirror_watermark_source`, `hedge_for` (a symbol) and `replay_source_id`. A stored row that maps to no configured strategy in its file is an **orphan**: it is reported, never keyed into the roster, and dropped by that file's next full save. Historical trade, closed-position and diagnostics rows with no current strategy keep their stored identifier and carry their source file; nothing assigns them a scope by identifier suffix.

**Journals and rollback order.** One journal per run sits beside the live config, named for the whole key set (`merge-paper-<keys>.journal`), and records each fold's key, partition and instance. Every mode reads every journal for that live instance: an unfinished apply under another fold set refuses a dry run, an apply **and** a rollback, and a completed run that already owns one of this run's drop-in names refuses too. A journal written before folds were recorded yields its key from its file name and the partition `paper`, so an older release's merge still owns its drop-in. Each run retains the config and each drop-in it replaces under its own key set, so stacked runs never share one copy. `--rollback` takes the same `--paper` and `--source` arguments as the apply it undoes, and runs **newest first**: rolling back an older run while a newer merge is installed is refused by name.

**Acknowledgement.** A drained manual action is deleted **by row id inside the transaction that persists its effect**. A failed action keeps its row. A high-water mark is never used, so one file's acknowledgement can never delete another file's row.

**Persistence hold.** Each scope carries its own save-failure counter. Any unacknowledged failure holds position-increasing signals for that scope at all six regime-gated dispatch sites (OKX spot and OKX perps both reach it through `runOKXStep`) and refuses `manual-open` / `manual-add`; closes, trailing stops, ratchet, protection sync and hedge management keep running. Three consecutive failures skip that scope's strategies for the cycle. A successful save clears the hold. In the single-file layout both counters move together, so three failures skip every strategy for the cycle.

**Combined reads.** History, counts, statistics, exports and diagnostics query every required file, translate identifiers, merge, then order and page globally by `(timestamp DESC, scope ASC, rowid DESC)`. A file that cannot be read fails the whole read; no page or map is ever returned with a scope missing.

**Inspection.** `go-trader storage-inspect [--config <path>] [--json] [--require-idle]` prints, per file, the canonical path, the scopes it owns, any held lock and its pid, every mapped strategy with its position count, orphans (flagged when they hold positions), the pending-action count and the risk rows with their latch.
Each mapped or orphan `position_count` is the number of stored rows in `positions` plus `option_positions` for that physical file and stored strategy id, with no strategy-type or quantity filter; the paper-merge summary and per-book output use the same totals.
A missing position table contributes zero, while an existing table that cannot be read fails inspection.
It opens each file read-only, tolerates a pre-#1509 schema, and never migrates, moves or deletes a row.
It also prints each file's closed-bar entry record count (with unmapped rows) and every unresolved live entry attempt with its client order id and hold reason, and each file's Robinhood order attempt count with every unresolved attempt (`rh-…` id, coin, status, `ref_id`, venue order and hold reason; `--json` key `unresolved_robinhood_order_attempts`).
It exits 1 on a rejection: a book in the wrong file, a risk row whose scope the file does not own, an ambiguous legacy row, a duplicate stored identifier, an entry record whose partition the file does not own, or an unresolved live entry attempt held in a file other than the one its strategy is mapped to. `--require-idle` also rejects a file owned by a running process. Startup runs the same check and exits 80 on a rejection, before the first migrating open. A mixed single-file deployment that already holds paper books cannot be split automatically — resolve it by hand or stay single-file.

**Backup and restore.** Stop the service, then copy **every** retained state file with its `-wal` and `-shm` sidecars: `db_file`, `paper_db_file` when set, and each `paper_sources[].db_file`. Restore them together, in the lock order primary, paper, then sources by id, so no partition comes back against another partition's snapshot. `update_resolve_db_exclude` (`scripts/update_helpers.sh`) enumerates the same list, so `scripts/update.sh` excludes every one of them from its rsync; a new state path must be added there or the update overwrites it. There is no transaction across the files: a crash stops every partition at once, and separate files give record separation without fault isolation inside one process. `./go-trader storage-inspect --json` is the discovery command — it names the canonical path, the partitions and the lock holder of every file, so a backup script never has to parse the config itself.

**Service writable paths.** Each folded database directory needs its own systemd directive, and the merge tool writes one drop-in per folded deployment: `50-merge-paper-<id>.conf` for a source and `50-merge-paper-<instance>.conf` for `--paper`. A `/var/lib/<instance>/<dir>` path gets `StateDirectory=`, a deploy-tree path `ReadWritePaths=`. Removing a drop-in without removing its partition leaves the merged unit unable to write that database.

**Access mapping.** After a deployment is folded, its status port and its proxy or tunnel mapping are retired; the combined paper service's port serves every folded partition and the dashboard's partition selector navigates between them. Retire only the routes of folded units — a separate paper-testing deployment keeps its own port and route.

### Folding paper deployments into one paper service

**Paper and live never share a service.** The fold target is always a paper-only service: every mode refuses (exit 15, before any lock or binary) a target config that runs a strategy with `--mode=live`, the same classifier the scheduler uses (`isLiveArgs`). `--rollback` of an earlier fold stays allowed. A live service stays a separate process, and both may read the same shared market feed (§ Shared market feed). In this section "live" in a flag, a message or the journal names the target service, for historical reasons; it never means real-money trading. A paper strategy that mirrors a live one (`replay_sharing: "live_mirror"`) cannot fold, because its live source would have to run in the same config; compose refuses it (exit 21).

`bash scripts/merge-paper-instance.sh --live <instance> --paper <instance>` moves a paper deployment into an existing paper service while both keep their existing state files.
`--paper` refuses (exit 17) when the target's primary file already holds books, a paper risk row or pending actions of the target's own default paper partition, because the fold moves that partition to the folded database; fold such a deployment with `--source` instead.
`--source <id>=<instance>` folds a deployment into its own partition `paper:<id>` instead: it adds one `paper_sources` entry with that id and the deployment's database, aliases every strategy as `<base>-paper-<id>` (numeric suffix when the name is taken) and stamps `paper_source=<id>`.
`--paper` and `--source` may be combined and `--source` may repeat, so several deployments fold in one run.
Defaults: deployments at `/opt/go-trader-<instance>`, configs at `/var/lib/go-trader/<instance>/config.json`, units `go-trader@<instance>.service`; `--base`, `--deploy-root`, `--unit-dir`, `--live-unit` and `--paper-unit` override them.
Both binaries must be one release with the ownership-lock contract, and **both units must be stopped** except for `--diff`.

1. **Dry run (default).** Preflight (both units must already be stopped before any binary runs), then the script holds the ownership lock and the manual-action lock of **both** databases for the whole run and runs each deployment's own `storage-inspect --json` (without `--require-idle`, which would reject the script's own lock) and `inspect --all --json` against **copies** of both configs under its work directory.
   `inspect` and `storage-inspect` load configs through `LoadConfigReadOnly`, which applies a pending migration in memory and never rewrites the file; the script still fingerprints each copy after every run and exits 18 if an older binary rewrote it, and it refuses with exit 18 when the two configs carry different `config_version`s (a config below the current version stays as it is and the daemon migrates it as the service user on its next start).
   It refuses any other lock holder and any rejection; the paper file may hold fewer books than the paper config has strategies (a strategy that never ran a cycle has nothing to move and is reported), but a stored book with no configured strategy is an orphan and refuses.
   It composes `config.json.merge-staged` beside the live config: **every** paper id is aliased as `<base>-paper`, taking the first free numeric suffix (`-paper2`, `-paper3`, ...) when the plain alias is used, whether or not the bare id collides with a live strategy, and `storage_strategy_id` is set to the bare stored id so the stored book is untouched; a paper id that already carries the alias keeps it when the name is free.
   Mirrors get `replay_source_id` in the process namespace, a paper root cadence or ATR method that differs is stamped per strategy, the paper deployment's effective risk limits (read from its own `inspect --all --json`, so loader defaults count) become `portfolio_risk.paper` wherever they differ from the live scope's effective limits, and a live config with no `portfolio_risk` block gets its effective root limits written out explicitly so the merged file keeps the loader's default drawdown limit, `-paper` channel keys are added wherever the paper deployment routes a moved strategy somewhere the merged config does not already send it (a paper value the merged bare key already resolves to adds no `-paper` key, so the merged map keeps the single bare key; the script's route lookup, `merged_channel_route_key`, mirrors the runtime `resolveChannel` fallback, so a change to one must change the other), and `paper_db_file` names the paper database.
   `leaderboard_summaries` is merged, not dropped: the merged list keeps the live entries, then every folded deployment's entries, keyed like the scheduler's own duplicate check (lower-case platform, lower-case trimmed ticker or `*`, channel), so every per-coin board survives the fold; an identical entry appears once, one key with two different entries refuses with exit 21 naming both, and compose and `--diff` print the combined list.
   `channels` and `trade_alert_channels` both skip a redundant `-paper` key when the merged bare key already routes there.
   `SendToPartitionChannels` resolves paper destinations from each partition's own roster with `resolveTradeChannel`, so dropping the suffix key does not widen a paper-scope broadcast.
   `dm_channels` is never skipped: `tradeAlertRoutes` reads a paper strategy's DM destination at one literal key with no fallback — `<platform>-paper:<source id>` for a named paper source, `<platform>-paper` otherwise (`scheduler/notifier.go`) — so dropping that key would silence the paper trade DM.
   `inspect` uses that same exact key (`resolveDMKeyOverMaps`, `scheduler/inspect_cmd.go`), so the proof refuses a staged config that would drop a paper DM route or add one the paper send path was not using.
   Every root key in `regime`, `correlation`, `user_defaults`, `default_stop_loss_atr_mult`, `market_feed`, `shared_market_feed`, `role`, notification flags, `platforms`, `risk_free_rate`, `alert_throttle_interval`, `kill_switch_reset_dm_timeout` and `telegram` is compared in both directions, so a key set only in the live config refuses too (the merged config would apply it to the moved strategies and the per-strategy proof cannot see it); an absent `market_feed` counts as `rest` and an absent `role` as `scheduler`.
   Compose lists every differing non-dropped root key in one refuse (exit 21) and names differing dropped keys in that same report as live-value-kept.
   `--diff` prints that classified list from the two config files alone (no binary, lock, or unit-stop) and does not refuse; it also names compose refuses those files can see (`replay_log_path` when a paper `live_mirror` is present and the merged config would still have a live mirror, discord `-paper` clashes from strategies compose would newly merge), previews the alias every paper strategy would take, prints one `channel-plan` line per discord channel key the compose step would add (and per key it skips because the merged bare key already routes there, the same content compose prints under its own prefix), and states that inspect-based `portfolio_risk` refuses still need a dry run.
   `--align-to-live` (only with `--diff` or `--apply`) copies live's value for each non-dropped non-`strategies` differing root key into `<paper-config>.aligned` and never mutates the paper source; `--apply` composes from that file and the journal records each aligned key's before and after values.
   Proof compares moved strategies against inspect of the aligned paper file, not the pre-alignment source.
   It refuses a zero paper risk override where live is nonzero (zero inherits the live limit; it never disables it), any remaining differing shared root key, and a `-paper` channel that already differs.
   The live binary then proves the staged file: `storage-inspect` must map every paper book with no orphan and `inspect --all --json` must match each strategy's pre-move effective view (cadence, sizing, stops, take-profit ladder, regime inputs, direction, ATR method, scope risk, replay source, notification channels).
   It ends with the service override (`StateDirectory=` for a `/var/lib` paper directory, `ReadWritePaths=` for a deploy-tree one), the post-apply commands, and `VERDICT: READY`.
   Both databases and their write-ahead logs are fingerprinted before and after; a read-only open may create an empty `-wal` or a `-shm`, neither of which holds frames.
2. **Apply.** `--apply` re-checks the config and database fingerprints and the unit states, writes `merge-paper-<key>.journal` beside the live config, retains the prior config as `config.json.pre-merge-<key>` and each prior drop-in as `<drop-in>.pre-merge-<key>`, then moves the staged config and one drop-in `50-merge-paper-<fold>.conf` per folded deployment into place one at a time.
   `<fold>` is the `--paper` instance name or the `--source` id, and `<key>` joins every fold of the run with `+` in partition order (`--paper` first, then sources by id), so two runs that share a fold keep separate retained copies; a journal from a release that recorded no fold lines names its retained drop-in `<drop-in>.pre-merge`.
   A failure between the two steps restores every completed step and exits 4 with both units still stopped; a re-run of `--apply` on an interrupted journal restores first, then starts fresh; a re-run on a complete journal whose files still match is a no-op.
   A dry run reads the same journal: it refuses an interrupted one with exit 24 (run `--rollback` or `--apply`), reports a complete or rolled-back one and continues.
   Any restore (rollback, resume, or the failure path) compares the live config and the drop-in against the fingerprints the journal recorded and keeps a `<file>.merge-edited.<timestamp>` copy of a file that was edited after the apply before overwriting it.
   Lock files the script creates during the run are given the database's owner (or its directory's owner when the database does not exist yet), so the merged service can open them; pre-existing lock files keep their ownership.
   Apply never starts a service or places an order.
3. **Cutover.** `systemctl daemon-reload`, `systemctl disable go-trader@<paper>.service`, `systemctl start go-trader@<live>.service`, then `journalctl --namespace=+go-trader -u go-trader@<live>.service -n 50 | grep '\[storage\]'`: the start prints the layout, both resolved paths, every `alias <role>: <process id> -> <storage id>` line and `[config] portfolio scopes: live=N paper=M`. Retire the paper instance's `status_port` and any tunnel mapping that pointed at it; the combined process serves both scopes on the live port. Every moved paper strategy now answers to its alias, so Discord manual commands, the dashboard and the per-strategy operator surfaces take `<base>-paper`. The bare id names nothing in the merged process. `/go-trader-paper-to-live` changes only the strategy args and keeps the id, so a promoted strategy keeps its `-paper` name until an operator renames it (set `storage_strategy_id` to the stored id when renaming).
4. **Rollback.** With both units stopped, `--rollback` restores the retained config and drop-in and never touches a database, so the paper strategies come back under their bare ids; the paper source config is never rewritten. Records the combined process wrote to either file after cutover stay in that file.

**Folding into a new paper service.** `--new-target <name> --status-port <n>` (exclusive with `--live`) folds every `--source` and the optional `--paper` deployment into a new template service `go-trader@<name>`, so no folded deployment becomes the combined one and keeps a coin name and port.
`--status-port` must lie in the range the scheduler loads, `statusPortMinimum` through `65535 - statusPortMaxAttempts + 1` (1024 through 65531 today), which the script reads from `scheduler/server.go`; any other value, or an unreadable bound, exits 2 before any temporary file or check, `--diff` included.
The target's own `live` and default `paper` scopes hold no strategy unless `--paper` is given.
Preflight refuses with exit 19 when `/opt/go-trader-<name>`, `/var/lib/go-trader/<name>`, the unit file or its drop-in directory already exists, when the unit is enabled, masked, active or failed (the refusal names `systemctl reset-failed` for a failed unit), when a folded unit does not load from the installed `go-trader@.service`, when the folded units run as different users, when the installed template or journald namespace config differs from the release's copy, when the first folded deployment has no git `origin`, has uncommitted changes to tracked files, or runs a commit that no `origin` branch contains (a branch checkout needs its own `origin` branch to contain it; a detached checkout uses the default branch, else the first `origin` branch that contains it), when a running process has bound `--status-port`, and when two folded `.env` files give one variable different values (the refusal names the variable, never a value).
Different releases still exit 11 and different `config_version`s exit 18.
The root settings come from `--root-from <instance>` (default: the first folded deployment): the target config is that deployment's config with no strategy, no `paper_sources`, no leaderboard entries, `status_port` from the flag and `db_file` `/var/lib/go-trader/<name>/state.db`, which starts empty.
An absolute `log_dir` inside the root deployment's tree moves to the same path under `/opt/go-trader-<name>`, and the dry run prints the rewrite.
In both modes compose refuses (exit 21) when any value of the merged config names a path inside a folded deployment's tree, except the folded database routes (`paper_db_file`, `paper_sources[].db_file`); `--diff` previews the same refusal.
Every refuse-on-difference key must then agree across all folded deployments (one conflict report, `--align-to-live` aligns to the root deployment), the other dropped keys keep the root value and the dry run lists every differing source value per fold, root `portfolio_risk` is the root deployment's block, and every source keeps its own effective limits in `paper_sources[].portfolio_risk`.
The target has no strategy of its own, so compose reads the root effective risk view from the root deployment's paper-scope inspect; the per-strategy proof still compares every moved strategy's scope risk.
The dry run stages the config and the drop-ins in a temporary directory it prints and keeps, proves them with the first folded deployment's binary, and creates nothing else except the database lock files.
`--apply` creates `/var/lib/go-trader/<name>/` (0700) holding the journal `merge-paper-<key>.journal`, clones the first folded deployment's `origin` into `/opt/go-trader-<name>` on the `origin` branch preflight chose, at that deployment's commit, installs `config.json` (0600, owned by the service user), links `scheduler/config.json` to it, writes `.env` (0600) from the folded `.env` files (`GO_TRADER_SERVICE` that names a folded unit becomes `go-trader@<name>.service`), builds the tree with `scripts/update.sh --rsync-from <first folded deployment>`, requires the new binary to report the folded release and the new tree's tracked files to match that commit (so a later plain `update.sh` can pull it), gives the tree to the service user, proves the staged config again with the new binary, installs `go-trader@.service` and the journald namespace config only when absent, and writes one drop-in per fold under `go-trader@<name>.service.d/`.
It never enables or starts a unit and never changes a folded config.
The printed cutover is `daemon-reload`, `disable --now` for every folded unit, `enable --now go-trader@<name>.service`, the `[storage]` boot lines, and retiring every folded status port.
A failed apply moves what it created aside and exits 4; a re-run with a complete journal whose files match is a no-op, and a journal with no `complete` line refuses with exit 24 until `--rollback`.
`--rollback --new-target <name>` with the same fold arguments refuses while the unit is active (exit 14), and refuses (exit 24) while `/var/lib/go-trader/<name>/` holds the journal of a later `--live <name>` fold that is not rolled back, since moving the directory aside would strand that fold; otherwise it disables the unit, clears its failed state, removes the drop-ins, moves `/opt/go-trader-<name>` and `/var/lib/go-trader/<name>` aside as `.rolled-back-<run_id>` (the new `state.db` and the journal move with it), and prints the commands that enable and start every folded unit again.
Folded configs are never changed in this mode, so nothing is restored, and records the combined service wrote stay in each source database.
Read-only `inspect` loads a config whose primary file does not exist yet from its other files, the way a first start does, which is what lets the proof see every folded book before the new primary exists.

**Exit 79 during a handoff.** A scheduler that starts while the script holds a lock exits 79 naming the holder pid, `RestartPreventExitStatus` keeps it down and the owner is paged. That is the intended guard; run the script only with both units stopped and start the live unit after apply. Back up both files first; there is still no transaction across the two files, a process crash or stuck cycle stops both modes, and the script never runs `--once`, never opens the migrating writer, and never treats `--probe-only` as proof of ownership or write access. Exit codes: 2 usage, 3 lock contention, 4 restore failed, 5 a source file changed before apply, 10 to 19 preflight refusals (18: a config migration is pending or the configs carry different versions; 19: a `--new-target` refusal), 20 inspection, 21 compose, 22 proof, 23 override, 24 journal state.

### Cutover checklist and the recovery boundary

Run in order. Nothing outside the staging area changes before step 5.

1. **Inventory.** For every unit, record the instance, its config path, `db_file` (and `paper_db_file`, `paper_sources[].db_file` if already folded), `status_port`, the binary version and the proxy or tunnel mapping. `./go-trader storage-inspect --json --config <path>` answers the storage half per deployment.
2. **Update every deployment to one release.** `bash scripts/update.sh --restart` per instance, never a Go-only rebuild: Go and Python share one argv contract per commit. Confirm `./go-trader --version` matches across deployments and that every config carries the same `config_version`; the merge refuses with exit 18 otherwise.
3. **Preview.** `--diff` reads only the config files and needs no stopped unit. Resolve every refuse-on-difference root key it names, by editing the paper config or with `--align-to-live`.
4. **Prove.** Stop **every** unit named in the run, then the dry run (no `--apply`). It holds each database's ownership and manual-action locks, proves the staged config with the live binary's own `inspect --all --json` and `storage-inspect --json`, and ends at `VERDICT: READY`. Databases are fingerprinted before and after; the dry run never writes one.
5. **Apply.** The same command with `--apply`.
   The **recovery boundary** is the journal line `config begin`, which the apply appends before it retains the live config and installs the staged one; `config done` is appended only after the install.
   Before `config begin` nothing outside the staging area and the journal itself has changed and there is nothing to roll back.
   From `config begin` on, the live config may already hold the merged config while `config done` is still missing, and each drop-in behaves the same way from its own `override begin` line (`override begin.<id>` for a source).
   The restore path keys on the `begin` markers, so `--rollback` and a re-`--apply` (which restores the retained files first) recover from the retained copies in either window.
   Read a journal that carries `config begin` as material that may already be installed; never start the merged unit or set that journal aside on the strength of a missing `config done`.
   An interrupted apply restores what it installed and exits 4; if that restore also fails it prints CRITICAL with the retained paths and stops, and the deployment needs a hand repair before any further run.
6. **Restart and verify.** `systemctl daemon-reload`, disable each folded unit, start the live unit, then check the `[storage]` boot lines for the layout, every resolved path and every alias, `[config] portfolio scopes`, that each partition's books and risk row came back, that protection is armed on every open position, that paper signals are produced on the next cycle, that replay pairing still holds and that a trade alert reaches each partition's channel. **`VERDICT: APPLIED` is not proof that trading resumed**; the boot lines and the first cycle are.
7. **Retire.** Only after step 6 passes: disable the folded units, retire their status ports and their proxy or tunnel mappings, and keep every source database, its writable-path drop-in and the retained copies. A folded unit that restarts would be a second owner of a database this process holds, which is what exit 79 guards.

**With `--new-target`.** Step 1 also picks the new name and a free status port, and step 3 runs `--diff --new-target <name> --status-port <n>` against the root deployment.
Step 4 proves with the first folded deployment's binary.
In step 5 nothing that carries the new name exists before `/var/lib/go-trader/<name>/` and its journal are created, and every later path is recorded there before it is written (`deploy begin`, `config begin`, `env begin`, `build begin`, `template begin`, `override begin.<id>`), so `--rollback --new-target <name>` with the same fold arguments moves aside whatever the interrupted run created.
No folded config or database is ever written.
Step 6 is `systemctl daemon-reload`, `systemctl disable --now` for every folded unit, then `systemctl enable --now go-trader@<name>.service`; the `[storage]` boot lines must show `primary -> /var/lib/go-trader/<name>/state.db` and one `paper:<id> -> <source database>` line per fold.
Until step 7, `--rollback` and the printed `enable --now` commands bring every folded unit back on its own port.

**Databases are never rolled back.** `--rollback` restores the config and the drop-ins and never opens a database for writing, so records the merged process wrote after cutover stay in the file that owns them. Restoring a database snapshot is a separate, manual step under § Storage Ownership Backup and restore, with every retained file restored together.

---

## Portfolio Kill Switch And Latch Ownership

**The latch is partitioned by mode.** One scheduler holds two portfolio scopes, `live` and `paper`, decided only by `--mode=live` in the strategy args. Each scope keeps its own peak, drawdown, latch, events, daily-loss ledger, notional total, exposure total and correlation model, and each is evaluated once per cycle over its own strategies. A paper drawdown can never latch live, and a live drawdown can never latch paper. Only a scope with at least one configured strategy is evaluated, so a single-mode deployment evaluates one scope, and the operator surfaces name it. On upgrade, an existing unscoped `portfolio_risk` row moves into `live` when the config holds any live strategy, else into `paper`; the move is written back on the first boot and is a no-op afterwards. With `paper_db_file` set, each file's unscoped row is placed from that file's own owned scope (§ Storage Ownership).

A live latch runs the exchange close plan over the live roster only. A paper latch closes paper books virtually at mark, sends no exchange order, and posts to the channels the paper strategies resolve to; it never auto-resets without an owner. Because paper has no wallet fetch, its equity reading is always trusted, so a paper scope never shows a substituted reading or a deferred latch.

The portfolio kill switch latches on drawdown and halts new trading until it is reset. **Exactly one measurement owns the latch each cycle** — there is no tie-break:

- **Equity drawdown owns it** when the equity guard is armed: a portfolio total is available AND the recorded peak is above zero. The kill switch then trips on equity drawdown over `max_drawdown_pct`.
- **Perps margin drawdown owns it** only when the equity guard is not armed.

When equity owns the latch, a margin drawdown over the limit is a throttled WARNING, not a trip: per-strategy circuit breakers own margin protection. The warning is coalesced by `alert_throttle_interval` and re-fires early on a band entry or a 1-point drawdown escalation.

**Untrusted readings.** A cycle's total is trusted only when the pooled equity read is complete AND no portfolio-value fallback and no stale risk balance were used. On an untrusted cycle the latch stays with equity, but:

- The peak never ratchets up, so a bad total cannot inflate the peak.
- The equity drawdown reading is FLOORED at the last reading, clamped to `max_drawdown_pct` so the floor alone can never latch.
- The substitution is persisted and flagged as `drawdown_reading_substituted`. **Label it on every operator surface** — the number is carried forward, not measured this cycle. The warning DM marks it as "carried forward; balance substituted this cycle, does not reconcile with the figures below".

**An untrusted over-limit reading defers the latch; it never vetoes it.** The first such cycle records the timestamp and a `latch_deferred` event naming the untrusted basis. While the run is unbroken and under 15 minutes old, the full-book latch is held and per-strategy circuit breakers are the active protection. Past 15 minutes the latch escalates and the reason names the untrusted basis and the deferral. A trusted reading landing first clears the timer. The deferral is loud: the log line is `[CRITICAL]`, and the warning DM bypasses the throttle for as long as the deferral stands. `/go-trader-circuit-breakers` shows the deferral, when it started, and when it escalates.

Reset is owner-DM only and clears one scope. With no DM owner configured, the live scope auto-resets only after a confirmed-flat close, and the paper scope auto-resets right after its virtual close. The reset DM names the scope in its header, and carries the drawdown reason, the trader-instance label, the HL wallet address (live prompts only), and a protection-gap warning when the close plan has not confirmed flat. Reply `reset` while one scope is latched. While both are latched, one prompt covers both scopes: reply `reset live` or `reset paper`; a bare `reset` is refused and names both, and the scope still latched is re-prompted next cycle. One single-flight prompt waits at a time, so one owner reply is never consumed by the wrong waiter. `kill_switch_reset_dm_timeout` sets how long that prompt waits (empty = 6h).

**Auto-reset.** Once every platform is confirmed flat the next cycle clears virtual state and resumes trading, posting `Virtual state cleared. Kill switch auto-reset; trading will resume next cycle.` Auto-reset also needs every resting limit order resolved (see below) and no operator-required venue outstanding.

**Resting limit orders are cancelled before the flatten.** The kill-switch close cancels every `pending_limit_orders` row first, keyed on the ROW rather than a position, under a 60-second deadline. Each row goes cancel → status check → delete; a row whose outcome cannot be resolved clears the confirmed-flat flag and blocks auto-reset without an owner. A row carrying an unadopted fill is never auto-deleted. Cancellation and adoption are separate eligibilities: a row belonging to a strategy that is absent from the config, or is no longer `type=manual`, is still cancelled even though it can no longer be adopted. Ordinary limit-order reconciliation is never gated on kill-switch state.

**Multi-strategy HL coins.** Kill-switch fills split by virtual quantity at snapshot time, and the split fails closed. If HL flattens to about zero, a sole-stop trigger fires with the residual matching non-owner peers, or a single take-profit tier fills externally, the next cycle closes the affected virtual peers automatically; an ambiguous gap stays a gap.

Warn-band messages repeat while the drawdown sits inside `portfolio_risk.warn_threshold_pct`. Silence them by resolving the drawdown or changing the threshold.

Drain and live-execution failure alerts: `journalctl --namespace=+go-trader -u go-trader -n 100 | grep "liveExec\|drain"`.

---

## Hyperliquid Liquidation Guard

A stop-loss trigger placed past the exchange liquidation price can never fill: Hyperliquid force-closes first, at liquidation-engine pricing. The guard makes that geometry unreachable.

**Boot check.** For every live isolated-margin HL perps strategy, a stop distance at or beyond the bankruptcy distance (`100 / leverage` percent) fails the load with a message naming the field, the bound, and the leverage. It covers `stop_loss_pct`, the price-percentage derived from `stop_loss_margin_pct`, and the `max_drawdown_pct` fallback. Cross-margin strategies are exempt. Run the same audit across the fleet before an update with `bash scripts/check-hl-stop-bankruptcy-bound.sh`, which reads raw JSON, mirrors the loader's stop-owner resolution, and exits 1 on a finding.

**Runtime clamp.** Live only: paper triggers are never clamped. The guard reads the per-coin liquidation price and matches it against the coin's net side; a side mismatch means unknown and the coin is skipped. It **clamps, never refuses to arm**, and it tightens **one way only**: a trigger past liquidation moves to 0.5% inside it, and a replacement is submitted only when it is strictly tighter than what is resting. The liquidation price is never persisted, so 0 always means unknown. Geometry that cannot be clamped is refused rather than guessed. Before the dispatch of each cycle an audit tightens every stop owner; when nothing is due, an off-cycle pass runs at half the shortest live HL interval, floored at 60 seconds, over live HL perps and `manual`.

**Refusals that protect peers.** The audit will not touch a coin whose recorded size across live strategies exceeds the on-chain snapshot — moving a reduce-only trigger there could close a peer's real position. It reports that as `not reconciled`: reconcile the coin and the audit heals it on the next pass.

**Alerts.** Each outcome DMs the owner and posts to the channels, deduplicated per strategy and symbol and re-sent on an action change or after `alert_throttle_interval`. The action names what happened:

| Action | Meaning | What to do |
| --- | --- | --- |
| `clamped` | The trigger was tightened to just inside liquidation | Nothing. Lower the leverage or the stop distance so the configured geometry is reachable |
| `replace deferred` | The replacement could not be placed; the ORIGINAL stop is still resting | Nothing. The scheduler retries next cycle |
| `protection lost` | The old trigger was cancelled and the replacement did NOT rest — **the position has no exchange-side stop** | Act now. The message names when the scheduler re-arms |
| `re-armed` | A position with no stop got one | Nothing |
| `re-arm failed` | The re-arm did not rest — **no exchange-side stop** | Act now |
| `not reconciled` | Recorded size does not match the on-chain snapshot, so the audit did not touch the order | Reconcile the coin |
| `SL filled` / `exited` | The original stop already fired, or the replacement filled at submit | Nothing. A replacement that filled at submit is booked by the step that placed it, which sends its trade alert and counts it in that cycle. A fill of the original stop is booked by the reconciliation pass |
| `outcome unknown` / `placement unknown` | The result could not be read; an order may be resting untracked | Verify the order book on Hyperliquid. Recorded state is kept and nothing is re-placed |

An unreadable outcome always keeps the recorded state. A cancel that leaves nothing resting gets exactly one in-cycle retry, and only when the placement was positively rejected — classification comes from what actually rests, never from error text.

---

## Model-Only Close Reconciliation

When a circuit-breaker close books a row from the model rather than a real fill, the row carries `fee_source='reconcile_adjustment'` and no exchange order ID. Once the real Hyperliquid fill lands, the scheduler corrects that row **in place** — quantity, fill VWAP price, exchange fee, gross realized PnL — instead of writing a second close. Partial fills accumulate against the closed basis over successive cycles, and the row stops accepting corrections after 48 hours.

If the coin goes flat on-chain while the row still covers only part of the close, the residual was finished by another mechanism such as a resting stop. That raises an owner alert, at most once a day per strategy and symbol, saying that the trade row, `closed_positions`, and cash are inconsistent. Fix it with `backfill trade-ledger` or reconcile by hand.

---

## Retired research: basket and pair (archive tag)

The basket coordinator (#1781), its rankers (`basket_scripted_fixture`, `cross_sectional_momentum`, `cross_sectional_reversal`), research pairs (#1862, `cross_exchange_perp_arbitrage`), the offline basket and pair replays (`basket-replay`), isolated paper recording (`basket-replay-paper`) and the `/resolve-pair-native` command were removed in #1955.
The annotated git tag `archive/basket-pair-research` points at the main commit the removal branched from and holds the whole feature: Go, Python, tests, research candidates and the SKILL.md sections.

- **Config.** Every load route (startup, SIGHUP reload, `probe`, `inspect`, `storage-inspect`, ledger export, the feed consumer load and the quote-admitted load) refuses a root `baskets` or `pairs` key, a strategy `basket_id` or `pair_id`, `args[0]` or `open_strategy` equal to `basket_member` or `pair_member`, and the args `--basket-member`, `--pair-member-json` and `--management-stdin`, whatever the value (`null` and `[]` included). The check (`retiredResearchConfigRefusals`, `config_unknown_keys.go`) is the first step of `loadConfigDataOpts`, before any version check or config migration, so a refused load never rewrites the file; the error names the key and the tag. A refused SIGHUP keeps the previous config.
- **State.** Old state files keep their retired tables, rows and the `portfolio_risk.pair_valuation_incomplete_since` value (Storage Ownership, Retired research tables). A former member id reused by an ordinary strategy loads its unresolved retired entry intent and holds new entries; give the strategy a fresh id.
- **Restore.** A restore needs a port onto current main; a plain cherry-pick of the tag does not apply. The neutral moves (`replay_run.go`, `columnProjection`, `shortFingerprint`, `okx_account_key.py`), the cost scope (fee and slippage wrappers, `fill_cost_policy.go`), the valuation threading (`pairMarks` through portfolio, risk, exposure, correlation, wallet, UI and Discord), the close-script evidence, the probes and the Go and Python registries all have to be re-bridged. The archived migrations recreate the tables idempotently.
- **Tests.** The golden `TestOrdinaryAccountingGolden` and `TestRetiredResearchStateSurvivesLoadAndSave` were captured on the base code (`scheduler/testdata/ordinary_accounting`, `scheduler/testdata/retired_research_state`) and pin ordinary accounting and old-file survival across the removal.

## Quote Lifecycle (#1860, research-only)

A durable, partition-owned quote lifecycle for Hyperliquid perps with one settlement authority.
It is **default-off and live-refused**: no deployed configuration runs a quote engine.
A structurally valid explicit-paper quote owner loads on an ordinary configuration under one derived **engine hold** (#1923, `quote_engine_unavailable`, tracking issue 1923) until a forward paper quote engine exists.
Live activation needs a separate reviewed promotion and an operator decision.
Part 3 (#1861) owns the quote policy, the paper venue, the `quote-replay` runner and the study; this section owns the coordinator, storage, settlement, safety and controls those parts run through.

**Configuration and refusal.** A quote owner is a Hyperliquid perps strategy with a `quote` block (`coin`, `max_inventory_usd`, `max_order_qty`, `request_expiry_ms`, optional `fee_reserve_rate`, `margin_price_buffer_pct`), `close_strategy: inventory_quote_exit` and exactly one static percent stop (`stop_loss_pct` or `stop_loss_margin_pct`, inside the isolated bankruptcy bound in paper as well as live; cross margin keeps its exception).
A non-empty `symbol` must be exactly the quote coin, and the position, funding anchor and stop all use the quote coin as their key.
ATR, regime and trailing stop owners, the `max_drawdown_pct` fallback, tiered take-profit, ratchet, hedge, scale-in and replay sharing are refused.
Unknown keys, booleans as numbers and non-finite values fail the raw parse.
Both production hooks (`loadConfigData` and `validateConfig`) run `quoteAdmissionErrors`: with no admission, `researchAdmissionErrors` admits a quote block, `quoteCapabilityStrategies` seam name and `inventory_quote_exit` close only together on one complete explicit-paper owner that passes `quoteOwnerConfigErrors`, `inventoryMarketMakingConfigErrors`, the research route and `quoteExclusiveOwnershipError` (the coin/partition rule `bindQuoteAdmission` also runs); any other quote reference, and every non-paper mode, is refused with any `allow_no_edge` form. An admitted ordinary owner sets no `quoteLoad` and creates no admission.

**Engine hold (#1923).** `engineHoldFor` derives the hold from configuration only, at startup, every due cycle and after a valid reload; nothing persists it.
The main loop splits held entries from the due set before the feed fetch, price check, persistence and kill-switch filters and dispatch, logs `[hold] <id>: quote_engine_unavailable ...`, records it in run evidence (`/health` `held`, `engine_hold` on `/status`, `/api/strategies` and the overview), advances the due mark without `markEvaluated`, drops its evaluation mark (a held-only cycle requests no snapshot) and still reaches the end-of-cycle save and the `--once` exit.
Persistence and kill-switch holds never overwrite or clear it.
A reload that changes the held set is refused (restart required).
Held owners stay inert outside the due loop through the existing `quoteOwnedStrategy`/`withoutQuoteOwned` exclusions (batch, perps step, check and execute dispatch, protection, trailing stop, shared close floor, paper funding, liquidation guard, balance sync, kill switch, force-close) and the startup contract below; `quoteStartupRecoveryFor` and shutdown cleanup stay bound-admission only.
Before any migration or save, storage inspection (`engine_hold_storage.go`) resolves each held identity through the identity map and refuses startup when its owning or any other file holds positions, option positions, pending manual actions, pending limit orders, unresolved entry intents, Robinhood attempts, pending circuit-breaker closes or open paper funding evidence, or when that evidence is unreadable or the identity is unmapped; durable quote contracts keep their admission-required refusal (flat or resolved too). Completed ordinary history starts unchanged.

**Typed quote setup (#1923).** `add-quote-owner` takes a `{"role":"quote_owner","strategies":[<one owner>]}` fragment (1 MiB ceiling, duplicate and unknown keys refused, no root settings): web upload (`POST /api/config/add-quote-owner`, the browser sends the file text unchanged as `fragment_text` so the server parses the exact bytes, confirm nonce bound to the fragment and its digest), Discord attachment (one download from the Discord CDN through the egress client, size and type bound, DM confirmation showing the digest) and the wizard `quoteOwnerFile`.
Execution appends the confirmed entry under `configWriteMu` and revalidates the whole candidate; collisions, expiry and mismatches write nothing.
`openStrategyRegisteredFor`, `hyperliquidBatchEligible`, `runHyperliquidCheck` (scalar, composer and batch fallback), `runHyperliquidPerpsStep`, `runHyperliquidExecuteOrder`, `force-close`, the scalar add-strategy route and paper-to-live refuse a quote owner even in a directly built configuration.
`inventory_quote_exit` carries the close registry's `research_only` marker: the legacy `close_registry_loader.py --list-json`, evaluation and close fallback refuse it, the canonical catalog lists it as quote-owned and held, and the parity test pins the Go `researchOnlyCloseStrategies` set to it.

**Internal admission.** Only code creates an admission; no key, flag, environment variable or route can.
The load form `quoteLoadAdmission` (kind, owner id, run state directory, SHA-256 of the exact configuration bytes; no store) comes from `newQuoteResearchLoadAdmission` (the #1861 runner) or from a `_test.go` fixture constructor.
`loadQuoteAdmittedConfigData(data, adm)` is the only loader that sets it (read-only, credential-skip, no file write; a digest mismatch refuses).
The research kind needs a paper partition, one explicit `--mode=paper` and a seam open name whose parameters pass the seam validator; only that owner's open-role reference is exempt from `productionRefusedAdmissionErrors`.
`bindQuoteAdmission(load, cfg, store, ownership, venue, clock)` binds once: the store must come from this configuration's layout and identity map, every file must be a real file inside the admitted directory with ownership held, and the venue must match the partition (research: provider `paper`, account `paper:<partition>`, not live-shaped).
The policy fingerprint is a digest of the open name, seam specification version, canonical open params and quote block; the configuration fingerprint is the load digest.
Both are saved with the contract and a resumed process with a different one refuses.

**Deadline call.** `RunDeadline(ctx, adm, req)` takes the deadline, deadline marks (`EvidenceMs`, `MaxAgeMs`), paper funding coverage (`paperFundingInputs`), the input-health verdict and a pure decision callback, and reads only the injected clock.
Stages: refuse an earlier deadline (equal repeats; inside the effective cadence is not due); create the contract at the first call (pre-existing exposure or unrelated pending orders refuse); read the venue and stage the settlement outside `mu`; call the callback on the post-settlement staged snapshot (or the durable one with the settlement holds); in one write hold of `mu` recheck the revision, commit one owning-file transaction (the settlement plus the caller's `WriteQuoteProgress` record, or a progress-only transaction) and swap; then run holds, the stop owner, the risk envelope, reservations, submissions and cancellations.
At most one settlement per call; fills confirmed while applying settle later.
A revision mismatch, failed commit or progress error reloads durably and returns a `quoteReloadError` (the deadline is a resume point).
`RecoverOnStart` resolves uncertain orders and settles before any new submission.

**Settlement (sole sink).** Read the fill history from the applied boundary (with overlap), read the strict position, recheck the history through that read (bounded retries; fills that keep arriving hold).
Every fill must validate, name this account and namespace and attribute to a saved quote, stop or close identity; an unrelated fill, a conflicting copy, incomplete or retention-limited history, a non-advancing page, a contradiction with the settled prefix (an unapplied fill at or before the applied boundary), an ambiguous tied chain (equal times need one unique chain from their starting positions) or a final exposure mismatch hold the whole uncommitted prefix, also at flat.
Arithmetic is exact (`big.Rat`): same-side fills add at weighted cost (`scale_in` trade type), opposite fills realize gross PnL from the saved basis (venue `closedPnl` never enters `realized_pnl`), a reversal closes and opens a residual with a new position identity and an exactly split fee, partial reductions keep the initial quantity, and floats are projections checked against the exact totals.
The applied coverage boundary is the position read time minus one millisecond.
A fee correction adjusts cash, fees and daily PnL once with no quantity replay; an unsupported fee asset holds.
Each correction of a fill starts from the fee the previous correction set (applied earlier or in the same settlement), so the booked fee equals the newest correction; a correction with a lower row id than one already applied holds.

**Paper funding.** The settlement is the only funding writer for a quote owner: fills move inventory through the event-time `mutatePaperPerpsBookAt` (an earlier event than the anchor is a contradiction, never clamped), only the settlement advances the anchor's verified time, and the shared per-strategy funding core (`paperFundingSampleCoins` + `paperFundingSettleCoins`) books records with settled time < record time <= min(coverage end, applied boundary).
The generic loop (`planPaperFunding`, `runPaperFundingAccounting`, `syncPaperFundingEligibility`) skips quote owners.
Live funding for a quote-owned coin is saved in `quote_funding_evidence` and applied once by the settlement.

**Risk envelope and holds.** Before either side is sent the whole pair, its envelope inputs and both identities commit in one transaction; the request is persisted `submitting` before the send and the cancel intent before a cancel.
The signed inventory interval runs from settled inventory plus confirmed-unapplied fills, minus every remaining sell through plus every remaining buy (existing, unknown, cancelling and proposed; reduce-only, stop and close orders excluded).
Each extreme must pass the marked inventory cap, the partition notional cap, the direction-aware exposure cap (`evaluateExposureCap`) and verified free margin with a conservative price and a non-negative fee reserve; the envelope is rechecked before each risk-increasing send.
Pause, circuit breaker, daily loss, kill switch, persistence hold, operator hold, a missing, unhealthy, stale, incompatible or out-of-order verdict, a stale mark, a failed evaluation, a held settlement, an unknown order outcome and nonzero inventory without a confirmed resting stop hold increases and cancel risk-increasing resting quotes; reductions (reduce-only reducing quotes, the exit lane) and the stop continue.
A replacement waits until both predecessor sides are terminal and settled; an identical desired side keeps its identity.

**Stop and exit.** The coordinator is the stop owner: side opposite the settled inventory, size through `hlOwnStopShare` against the fresh read, trigger from `RiskAnchorPrice` via `hlLiquidationScalarRearmTriggerPx` (one-way tightening clamp), rounded toward the market.
A changed stop is verified on the venue before any cancel; an unknown outcome keeps both identities unresolved with one CRITICAL alert.
`inventory_quote_exit` handles caller age/cap exits, a price-driven cap breach and a latched kill switch: freeze increases, cancel quotes, wait for settlement, then send a reduce-only IOC close sized by `planHLCloseOrder`; a partial exit re-sizes the stop through `resolveHLCloseRemainderStop`.
Every stop, close and kill-switch fill settles only through the sink; ordinary protection, paper stop owners, circuit-breaker fills, kill-switch settled legs, generic reconciliation, backfills and trade stamping skip or refuse a quote owner.

**Venue and adapter.** `quoteVenue` (provider, account key, namespace, live-shaped flag, client ids, place/cancel/status by saved identity, raw fill recovery with coverage, strict position read) has a live implementation over `check_hyperliquid.py --quote-place | --quote-cancel | --quote-status | --quote-recover | --quote-position`.
Place and cancel run through `runPythonSideEffectWithTimeout`, never `runPython`; reads take the caller's bounded context.
The adapter's `limit_open` takes `cloid`, `reduce_only` and `require_exact` (a request that rounding would change is refused before submission), `set_order_expiry` bounds acceptance (`now < expiry`), the stop role uses `place_stop_loss` with a saved cloid, and `strict_position` returns the exact signed size, liquidation price and withdrawable margin.
`--quote-status` reports the filled quantity as exact decimal text (`origSz - sz`), and the settlement saves the confirmed quantity as the larger of the confirmed and applied quantities, so a terminal order resolves once its fills are applied.
The quote scanner compares repeated identities before deduplication and reports conflicts.
`quotePriceRule` (`roundQuotePx`: buy down, sell up; `roundTriggerPx`: toward the market; `floorQuoteSize`) refuses any request rounding would change.
All six quote modes are in both startup probe paths (`quoteProbeArgvs`) and exit on `--probe-only` before adapter construction.

**Boundary rules.** Caps pass at equality; margin refuses only when available < required; a mark is fresh when `EvidenceMs <= deadline` and `deadline - EvidenceMs <= MaxAgeMs`; submission and acceptance happen only when `now < expiry`; an uncertain absence proves "never accepted" only when `now > expiry + 2m` and the applied boundary reaches `expiry + 2m`; the liquidation clamp applies at equality; an earlier deadline refuses and an equal one repeats.

**Storage, startup and shutdown.** All quote access goes through `StateStore`; legacy pending-limit paths stay manual-only, and `LoadPendingOrdersCombined` returns manual rows plus every file's quote rows with source role, partition and strategy (one failed file fails the read).
`reconcilePendingLimitOrders` dispatches quote rows before the manual gates: to the bound owner, or held untouched when no admitted coordinator owns them.
The kill-switch cancel-before-flatten lane adds an owner-qualified quote lane; unresolved quote evidence prevents a confirmed-flat result.
Production startup runs the quote recovery hook (a no-op without an admission) before reconciliation, and storage inspection refuses (exit 80) any quote contract, flat or not, and any unresolved quote evidence that no admitted owner runs, because every full save refuses an unowned contract.
An order write never erases a saved cancel request (a terminal outcome closes it as confirmed) and never lowers the saved confirmed quantity.
Shutdown persists cancel requests and cancels risk-increasing quotes with its own bounded context before `runDrain`, keeping a confirmed stop on nonzero inventory; unknown outcomes survive for restart.
SIGHUP refuses ownership, policy, cadence, close/stop and storage changes while quotes, unresolved intents or inventory exist.

**Diagnostics and controls.** `GET /api/quotes?partition=` shows each owned snapshot (deadline, revision, fingerprints, exact and projected inventory, first opening fill time, generation and its sides with identities, acceptance time and confirmed/applied quantities, envelope, holds, stop, applied boundary, funding coverage start and held records) plus held durable evidence.
`POST /api/quotes/cancel` and `/api/quotes/hold` need an explicit owned partition (400 malformed, 404 unowned), same origin and JSON; a cancel targets only the current generation's quote sides (never the stop) and is idempotent.
Controls cannot enable production, waive coverage, ownership or position proof, or adopt inventory.

### Offline quote replay (#1861)

`go-trader quote-replay --spec <plan|-> [--max-deadlines N] [--fault <point>:<n>]` runs the production-refused `inventory_market_making` policy through the real coordinator, settlement, envelope, stop and exit lanes above; `quote-replay discover` prints the policy specification and its hash. `backtest/quote_replay.py` is the driver (`discover`, `plan`, `run`, `verify`) and `backtest/candidates/inventory_market_making_1636/` holds the frozen study.
It is research-only: it adds no second booking path, writer or admission form, and no key, flag or environment variable can admit the candidate elsewhere.

**Plan and isolation.** Input is one hashed `go-trader.quote-replay` v1 plan (unknown fields refused): mode, run (`run_id`, an absolute real `state_dir`, source provenance), coin, deadline window (whole seconds, at most `max_deadlines`), every policy parameter, cycle, cap and stop, the quote block, lot decimals, costs, latencies, the queue model, pinned funding records, books (receipt and event times, levels), book coverage-break points, deduplicated raw trades and raw-trade coverage ranges.
Before any state opens it refuses venue credential names (`HYPERLIQUID_`, `OKX_`, `ROBINHOOD_`, `TOPSTEP_`, `DERIBIT_`, `IBKR_`, `BINANCE_`, `LUNO_`), scrubs notifier and token variables, refuses deployed paths, a parent behind a symbolic link, new mode on an existing path and resume of any directory it did not create (manifest, single marker row, owner, entries, hard links and every fingerprint must match); a moved directory refuses at binding through the saved configuration fingerprint.
It builds its own paper configuration (one owner `quote-replay-owner`, `inventory_market_making`, `--mode=paper`, `inventory_quote_exit`, `stop_loss_pct`, isolated margin, leverage 1, capital USD 10000, circuit breaker off), loads it only through `newQuoteResearchLoadAdmission` and `loadQuoteAdmittedConfigData`, checks `quoteReplayConfigRefusal` (one owner, exactly the owner's frozen book need, no other observation, the spec's cross-field rules), takes ownership and binds a paper venue (`provider paper`, account `paper:<partition>`).

**Deadline order.** At each due deadline the clock moves to the deadline, the venue processes recorded events through it and persists fills, the runner builds the evidence (last eligible book within `max_input_age_ms` by receipt and event, one sample per closed second over the volatility window, no coverage break, raw-trade coverage over the cycle), the verdict, the deadline mark (book midpoint, receipt evidence time) and pinned funding coverage, then calls `RunDeadline`.
The decision callback calls the pure policy (`scheduler/quote_policy.go`) on the staged snapshot and returns the immutable intent record, the venue checkpoint and the progress record; the result row is completed after the call.
A receipt after a deadline never enters its evidence, seal fingerprint or intent.

**Paper venue.** Every place and cancel takes effect at max(call clock, previous effect) + its latency and moves the simulated clock there; an effect at or after the next deadline refuses the run.
Quotes are post-only against the recorded book at the effect time (a crossing or out-of-depth price is rejected); a resting quote fills only on a later opposing aggressor trade strictly through its price, at its price, capped at the frozen participation of each trade across all own orders; trades at its price only reduce the queue ahead, depth increases are charged ahead and depth decreases never improve rank.
Stops trigger on the first recorded trade after acceptance at or through the trigger and fill as takers against recorded depth less the slippage reserve; the inventory close fills inside its limit; reduce-only orders never cross flat and are cancelled when their reducing position is gone.
Reads at a time see market events through it and every persisted own answer; fill times, coverage and the position sum use the receipt basis.
A raw-trade coverage break while an own order rests makes fill history incomplete, the settlement holds and the window ends unscored.

**Restart classes.** A completed result, a rollback before the settlement and progress commit, and a venue answer persisted before the deadline progress resume to records equal to an uninterrupted run (position ids keep a process-local nonce that comparisons drop).
A crash after the progress commit leaves the result pending: resume marks it interrupted, repeats the deadline with the saved intent and no policy call (any settlement kind commits with a recovery row), never resends a saved identity and stays unscored; the recovered ledger still equals the independent ledger of its durable events.
A callback fault that settles without progress leaves the contract ahead of the cursor and refuses resume.
`--fault` points: `issue-before-reserve`, `venue-before-persist`, `venue-after-persist`, `progress-before-write`, `progress-mid-write`, `progress-after-write`, `commit-hook`, `after-call`, `after-result`, `decide-error`; exit 75.

**Verification.** `quote_replay.py verify` checks every fill against the plan (source trade, receipt after acceptance, strict trade-through, participation budget, lot grid, fee, taker level less slippage, start-position chain, reduce-only never crossing flat), rebuilds the exact ledger and the paper funding samples and payments independently and compares them with the Go book, funding rows and position identities.
Depth-only and archive input (receipt basis `archiver_receipt_ns_floor_ms/1635-v1`, cache objects verified by `archive_cache.verified_object`, never downloaded) supplies no queue evidence and is never scored.
Output JSON lists decisions, results, venue events, fills, settlements, trades, the exact book, pending orders and the measured `RunDeadline` time per deadline (controlled, never production latency); exit codes 0 run, 1 error, 2 usage, 3 refused, 75 fault.

## Closed-Bar Decisions

`closed_bar_decisions: true` (#1712) makes a strategy decide on the last closed bar, the bar the backtester trades from (signal at bar N, fill at the open of bar N+1, entry ATR of bar N).
It is off by default; the default stays off until a paper comparison measures the effect.

**Supported scope.** Binance.US spot (`shared_scripts/check_strategy.py`), OKX spot and perps (`shared_scripts/check_okx.py`), Hyperliquid perps (`shared_scripts/check_hyperliquid.py`) and Robinhood paper spot (`shared_scripts/check_robinhood.py`, #1822; every admitted coin is crypto, § Robinhood asset classes), legacy and composed strategies, on the fixed-duration timeframes `1m`, `3m`, `5m`, `15m`, `30m`, `1h`, `2h`, `4h`, `6h`, `8h`, `12h` and `1d` (Hyperliquid: only its own intervals; Robinhood: only those in its timeframe table and not `1m`, so not `1m` or `3m`).
`loadConfig` refuses the flag on `manual`, options, TopStep, live Robinhood (live fills settle through `robinhood_order_attempts` and the #1769 entry ledger has no Robinhood provider; `check_robinhood.py --mode=live --closed-bar-decisions` refuses with the same reason), a custom check script, another timeframe, a higher-timeframe filter or regime timeframe outside that list, `delta_neutral_funding`, `open_interest_breakout`, `funding_skew` on OKX, and `regime_directional_policy`, `regime_window_divergence` or `regime_profile_allocation` (those resolve before the check runs, so they would read the current regime).
The reverse rule (`closedBarRequiredOpenStrategies`): `loadConfig` refuses every `schaff_trend_cycle`, `camarilla_pivot_reversal`, `stochastic_momentum_index`, `calendar_conditioned_entry` or `klinger_divergence` reference (positional open, `open_strategy`, `close_strategy` fallback) unless the strategy is Hyperliquid perps with the flag on, because its last-bar decision needs the verified closed-bar cutoff (#1651, #1669, #1663, #1653, #1662).
A replay mirror with the flag needs an explicit `replay_source_id`, and a named mirror and its source must agree on the flag.
Changing the flag is restart-required; SIGHUP refuses it.

**Closure.** The check captures one evaluation cutoff: the sealed snapshot's deadline (or its seal time when earlier or absent) in `market_feed: websocket|shared`, or the clock before the fetch in REST mode.
A bar is closed when its close boundary is at or before the cutoff; a final row that is already closed is kept.
Hyperliquid rows keep their native `t` and `T` (`T = t + interval - 1`), and the sealed payload carries a per-row `timing` sidecar plus `decision_cutoff_ms` only for frames an enabled strategy requests; Binance.US and OKX use the ccxt opening time plus the fixed interval.
Robinhood uses the Yahoo opening time plus the fixed interval (resampled buckets use their target interval) and adds a freshness rule to the primary, regime and HTF selections: the selected bar must be the newest bar that closed at or before its cutoff, Yahoo must already show the next (forming) row, and the cutoff must be at least `YAHOO_SETTLE_MS` (20 s, `check_robinhood.py`) after that bar's close; otherwise the decision holds, so a missing or unsettled Yahoo row never moves the decision to an older bar.
The rule comes from a live Yahoo measurement for #1822 (BTC-USD and ETH-USD): rows are UTC, 24/7 and on the epoch grid of their interval, with the forming interval as the final row and no off-grid live row; the newest closed `5m` row changed in 2 of 8 sampled boundaries (its low moved) between 10 and 15 s after the close, at the moment the next row appeared, and never after it; the `1h` row did not change from 3 s to 900 s after one sampled boundary; and the `1h` series has gaps (11, 20 and 1 missing hours over one year), which drop the resampled buckets that hold them.
A Robinhood `1m` closed-bar decision stays refused in Go and Python: the newest closed `1m` row was a flat zero-volume placeholder in 6 of 8 sampled minutes until the next minute's row appeared, 10 to 20 s after the close, so the settle hold would cover a third of every interval.
A check that runs within the settle window after a boundary holds and the next check retries the same bar (Repeat rule); give a Robinhood closed-bar strategy an `interval_seconds` below its bar interval so a check phase that lands in the window does not hold every bar.
The row timestamps a strategy sees are unchanged.
Unknown or contradictory timing, duplicate or overlapping bars, invalid prices, fewer than 30 closed bars, a payload without timing (an older producer), a missing dependency or a failed regime or higher-timeframe candle fetch hold the candle decision: the check returns `closed_bar_decision.held: true`, no open and no candle-derived close, and composed close evaluators still run on current inputs.
A sealed-feed check never falls back to a private fetch.
Limitation under `market_feed: websocket` and `shared`: the seal settles 5s after the deadline, while the first closed-bar correction read starts at close+5s, so the first check after a boundary usually decides on the socket-stored bar before any correction (§ Closed-bar correction); a venue revision that lands later reaches only later seals, so a booked open, its sizing and its entry ATR can differ from the backtester's corrected bar.
A later check on the same bar re-evaluates the corrected values (Repeat rule). `market_feed: rest` fetches after the boundary and has no such gap.
Enabled strategies request one more raw row (signal, higher-timeframe and regime frames) so the closed view keeps the same history length.

**Decision and protection views.** On the closed view: the open strategy, candle-derived closes (legacy signals and unknown-close fallbacks), `indicators.atr` (the strategy's own ATR column, else the `atr_method` ATR), the higher-timeframe filter (bars closed by the decision boundary), `funding_skew` records (time at or before the boundary), and the decision regime, computed in the check from the regime timeframe's closed bars and returned as `decision_regime`.
The regime gate and the Hurst gate read `decision_regime`; a missing one holds position-increasing signals even when `regime_gate_on_failure` is `open`.
On the current view: the price and mark override, the market ATR, the injected regime and AVWAP passed to close evaluators, the regime store, position regime stamps, divergence, dynamic exits and every Go protection path.
Entry sizing and `Position.EntryATR` both read the closed `indicators.atr`; an existing position keeps a nonzero entry ATR, and a zero entry ATR is stamped from the next check whose decision is not held, so ATR stops still arm.
An enabled replay mirror sizes and stamps from the source row's `entry_atr`; an open row without a valid one is not booked: the mirror sends an `open-without-entry-atr` drift alert, marks the row applied and applies the later rows.

**Repeat rule.** Every due check re-evaluates the selected closed bar against the current position and gates, so a gate hold, a failed check or a failed execution retries the same bar on the next check, and existing same-side guards, scale-in rules and fill confirmation stay authoritative.

**Entry allowance (#1769).** One closed decision candle permits at most one confirmed primary increase per strategy and target side: a new position or an enabled scale-in add.
A check interval shorter than the candle (the global default 600 s, or the 90 s drawdown-warning cadence) therefore cannot open the same side again from the same candle after a stop closes the first entry.
The scheduler checks the allowance after every other entry gate, at the closed-bar dispatch sites (Hyperliquid after the solo/batch join, the shared OKX step `runOKXStep` for OKX spot and perps, Binance.US spot paper).
A held entry only zeroes the position-increasing signal, so closes, stops, trailing stops, ratchets, protection and hedge unwind continue.
The allowance holds an entry when the decision identity is missing, held, malformed (interval mismatch, an opening off the interval grid, a boundary that is not opening plus interval), not the newest completed candle at the check's cutoff, from a cutoff older than one interval or in the future, older than the newest observed candle, inside the upgrade bootstrap, already consumed for that side, or while the strategy has an unresolved live attempt.
A confirmed target-side opening trade consumes the candle, including a partial fill and an opening that an immediate stop closed at once; a paper sizing or lot hold, a rejection and a verified not-sent order consume nothing, so a later check of the same candle may retry.
Consumption is written with the book in the owning file's save transaction, and the opening trade of an admitted entry waits for that transaction instead of the eager trade insert.
**Reversals.** A signal that would flip an open position closes it through the existing close-only path (as `close_fraction = 1`; a guarded OKX perps reversal close is sent reduce-only) and never sends the opposite opening leg in the same check.
The opening leg is taken on a later check of the same candle once the book is flat, if the allowance and every gate still permit it, so a held target side never blocks the close.
**Live attempts.** Before a live Hyperliquid or OKX entry order, the scheduler reserves the attempt in `entry_order_intents`, commits the `submitting` transition immediately before the order subprocess starts, and passes the saved client order id to the venue (Hyperliquid also gets a separate initial-stop client id and a signed order deadline 2 minutes out, 3 minutes for the stop; OKX gets the `expTime` request header 2 minutes out through `--exp-time-ms`; a late arrival is rejected by the venue in both cases).
A confirmed fill books the position, consumes the candle and marks the attempt `committed` in one transaction.
An explicit venue rejection or a verified not-sent result releases it as `no_fill`; any other outcome (a timeout, missing or malformed output, an unconfirmed OKX fill) keeps it held and is never resent. An OKX create error counts as a rejection only for a ccxt class raised before the order exists (insufficient funds, bad request, authentication, missing arguments, invalid order other than a duplicate client id); any other exchange error reads the order back by `clOrdId` and books or releases from that evidence, else holds. A guarded OKX entry books only a finite positive fill read back from the order; it never falls back to paper sizing.
**Recovery.** Each cycle starts by recovering held attempts before any dispatch: a `reserved` row was never submitted and is released; a `submitting` or `settling` row is resolved from venue evidence only (Hyperliquid `orderStatus` by client id plus order-matched `userFillsByTime`, after the signed deadlines pass; OKX `fetch_order` by `clOrdId` plus `fetch_order_trades`).
A terminal order with complete matched fills is booked once from the book the attempt started from, with the initial stop adopted or its immediate fill booked; a terminal zero fill is released unless saved execution proof or a client-id fill contradicts it; an open order, an incomplete fill history, a client id unknown before its deadline or a changed book stays held with a logged reason and one owner DM. A scale-in whose on-chain size the Hyperliquid account sync already copied into the book (book = before-add size + proven fill) is booked once from the saved pre-add size and average cost.
**Absence proof (#1771).** A client id with no order is released only when every check passes: the attempt's saved account key (`hyperliquid:<network>:<address>`, or `okx:<inst>:<network>:key-<16 hex of SHA-256 of the API key>`) matches the configured account and the account the script queried; the saved entry deadline (and, with an initial-stop client id, the stop deadline) plus 2 minutes has passed; the saved request is readable with a submission time before its deadline; no saved confirmed or booked quantity, venue order id or fill evidence exists; and the order lookup and every client id (entry and initial stop) report no order, no fill and a complete fill history from before the attempt window through the release deadline.
Both scripts take `--since-ms` (window start) and `--until-ms` (the release deadline); the reported coverage ends at the earlier of the release deadline and the request time.
Hyperliquid reads every `userFillsByTime` page from the window start to the request (a page shorter than 500 rows ends the scan; a longer one is followed until a page adds no new fill) and attributes a fill without a `cloid` through `orderStatus` by its order id only when the fill is at or before the release deadline; later fills cost no lookup, a later fill carrying the client id is still a match, and a found order (such as an initial stop that fires later) still sums every fill by order id.
10,000 or more fills since the window start (the venue retains only the latest 10,000, so the window may be gone), a fill with no time, a page that does not advance, an unretained order or more than 40 lookups inside the window is incomplete.
OKX reads `trade/fills` (last 3 days) or `trade/fills-history` (3 months) by `billId` page up to the release deadline and matches `clOrdId`, even when `fetch_order` returns `OrderNotFound`; a window older than 3 months, a row with no `clOrdId` field, more than 50 pages or a failed fills query is incomplete.
A fills query failure never blocks booking an order that `fetch_order` finds with complete trades.
It does hold a terminal zero-fill release, because the client id cannot be cross-checked (`query_error` in the coverage).
A missing order, a flat position, retention loss or a partial response alone never releases an attempt. Every transport, subprocess, parse or venue error refuses, even when a result was also printed, and each query has a 45-second deadline. An attempt saved before #1771 has no account key, so absence never releases it.
A proven fill that cannot be booked (the book changed) saves its quantity and evidence (`filled_unbooked`) before the hold, so a later empty result cannot release it.
**Serialization.** One per-strategy attempt exclusion covers the live entry path (reservation through settlement), cycle recovery and the operator command. Everyone takes it with a non-blocking try, before `mu`, never while holding `mu`, and never holds `mu` across a venue query. Recovery and the command recheck membership, status, request and evidence under `mu` after the query; a stale result changes nothing (no booking, no hold update, no boundary move).
**Operator release (`/go-trader-resolve-entry <strategy> <attempt>`, owner DM only).** It requests a fresh resolution check for one attempt. It refuses an unconfigured, paper or non-Hyperliquid-perps/OKX strategy, an attempt of another strategy, unavailable or read-only storage, another resolution in progress, a held attempt whose state-file row differs, `reserved` (recovery releases it), every `settling` attempt (a proven fill, partial or not), saved execution proof, any failed absence check above, and an attempt that changed during the query. The only transition is `submitting` → `no_fill`, in one conditional transaction in the owning file that matches the checked row exactly. The hold is dropped only after it commits; a failed commit keeps the durable and in-memory hold and is safe to retry. A repeated request reports the attempt as already resolved and changes nothing.
The row is kept, and `evidence_json` records outcome `operator_verified_absent`, the operator id, the prior status, hold reason and evidence, the account, both client ids, both deadlines, the release time and the order and stop fill coverage. Confirmed and booked quantities, trades, positions, cash and the consumed and bootstrap candle boundaries are untouched; the next entry still passes every gate.
**Future work.** No command repairs a fill booking after the book changed, and none resolves an attempt whose venue history stays incomplete; such an attempt stays held until recovery finds conclusive evidence.
**Upgrade bootstrap.** A strategy loaded from state with no entry record for its symbol and interval holds the first candle it observes, for both sides, even when that check's signal is zero, and saves that boundary; it never invents a consumed candle from a trade time. A new strategy admits its first valid candle.
Records survive restarts, saves and permitted reloads; live, default paper and each named paper source keep separate rows, and storage aliases resolve inside the per-file handle.
A replay source's guarded entry publishes its replay events only after the settlement commits, each with a stable `source_entry_event_id`.
**Backtest agreement.** The backtester takes one entry per decision row, so this rule makes repeated runtime checks match its entry count per decision candle; prices, fees, funding and fill timing still differ.

**Contract.** Go sends `--closed-bar-decisions` (and `--decision-regime-timeframe=<tf>` when regime is on) and requires the `closed_bar_decision` block back; a missing or unexpected block is a script error that holds the signal.
In a Hyperliquid batch the slot carries `closed_bar_decisions: true`, mixed slots share the raw frame and each selects its own view, and shared-state failure falls back to per-strategy checks in the same cycle.
With the flag off the argv, the batch slot and the check output (apart from its generated timestamp) are unchanged.

## Resting Take-Profit Trade-Through

`resting_tp_trade_through: true` (#1727) changes when a paper Hyperliquid perps strategy books a tier take-profit.
Live places resting reduce-only limits and the venue decides the fill, so a mark that only reaches the tier price does not prove the live order filled.
With the flag a tier fills only when a completed bar since entry traded at least `k` ticks beyond the tier limit rounded exactly as the adapter rounds it (`k = 1`, a constant with no config field). The tier fills in full at the rounded limit; several tiers in one step use the quantity-weighted rounded limits.
The study in `backtest/resting_tp_fill_study.py` measures the rule against venue orders. Its summary records the candle price basis and the cancel-time source as unconfirmed, so `k = 1` is a structural floor, not a fitted value.

**Scope and refusals.** `loadConfig` refuses the flag on a live strategy, on anything other than Hyperliquid perps, on a custom check script, without one of `tiered_tp_atr`, `tiered_tp_atr_live`, `tiered_tp_atr_regime` or `tiered_tp_atr_live_regime` as the close, with `tiered_tp_atr_live_regime_dynamic`, on a replay-mirror paper strategy (it books the live tier price), with `allow_scale_in`, on a timeframe outside the fixed Hyperliquid intervals, and when the check interval is longer than the bar.
The key must be the JSON literal `true` or `false`; a raw-key check refuses `null` and every other type before decode, on load and on reload.
Changing the flag is restart-required. A `close_strategy` change on a flagged strategy with an open position is refused (flatten first).

**Observation policy (shared with the backtester).** The check uses only completed bars at or before its evaluation cutoff; the forming bar never counts.
The entry bar (the bar that contains the position's open time) counts only its close; each later bar counts its favorable extreme (high for a long, low for a short).
Each check scans the completed bars after the position's stored scan watermark (from the entry bar on the first check), so a missed check or a restart heals while those bars are still in the frame. When the frame starts after the first bar it needs, the echo says `coverage: frame_truncated`, and the only possible effect is a missed fill.
There is no cadence guarantee: a late check books at check time, never backdated.

**Frozen geometry.** Under the rule tier prices come only from the risk anchor, the entry ATR and the position regime. A missing entry ATR, or a missing position regime for a regime ladder, holds the tier close; the live ATR and live regime fallbacks never apply.

**Stop-first.** Go builds the stop trigger from the Phase 1 position snapshot. The scan ends at the first new bar whose adverse extreme reaches that trigger, so a tier crossed in that bar does not fill (the backtester's `ohlc_walk` already books the stop first).
Each bar is stop-tested once, on the first check after it completes. The position stores the last scanned bar, the best reach and the Phase 1 trigger of the check that last advanced the scan (`resting_tp_scanned_open_ms`, `resting_tp_reach_px`, `resting_tp_stop_trigger_px` in `positions`). Later checks send the first two as `scanned_through_ms` and `prior_reach_px`, and send as `stop_trigger_px` the looser of the stored trigger and the current one (the first scan uses the current one; no stop on either side means no stop test), so a bar is tested against a trigger that was in force from its open. A trailing stop that ratchets inside a bar, or a stop that tightens later (post-take-profit breakeven), never ends the scan at an earlier bar.
A stop-reached bar advances the watermark but adds no reach, so its crossing never fills and later bars are scanned from the next check; the paper stop itself still books only on the check mark. A held rule does not advance the watermark. `recordRestingTPScan` is the only runtime writer of the three scan fields. An unarmed stop with an active stop owner (`stop_unarmed`) or a trailing stop that can still widen once (`stop_may_widen`) holds the tier close.
Paper stops stay mark-based at check time, so paper and backtest agree on tier closes only on frames where no armed stop is reached, under `ohlc_walk`.

**Contract.** Go sends `--resting-tp-rule-json={v,k_ticks,sz_decimals,entry_time_ms,stop_trigger_px,hold_reason,scanned_through_ms,prior_reach_px}` (one `resting_tp_rule` object in a Hyperliquid batch slot, byte-identical and covered by the batch fingerprint). `sz_decimals` comes from the venue lot metadata; unknown sends `null` and holds. `prior_reach_px` stays `null` until a scanned bar adds reach, even after the watermark moves past a stop-reached bar.
The check requires the `resting_tp_rule` echo back with the same `k_ticks`, `sz_decimals`, `entry_time_ms`, `stop_trigger_px`, `scanned_through_ms` and `prior_reach_px`, plus a next watermark and reach that never move backwards; a missing or different echo, an echo for an unsent rule, a held rule with a tier fill price, or a tier close without `close_tier_fill_price` is a script error that holds the signal. A missing input holds with a `noop:resting_rule_*` reason and never falls back to the mark rule.
The check refuses the rule in live mode. With the flag off the argv, the batch slot and the check output are unchanged.
The flag gives the signal frame bar timing, the decision cutoff and one more lookback row in both the single-check and the batch feed paths; `closed_bar_decisions` keeps its own decision semantics.
Logs: a held rule, `frame_truncated` and a stop-reached bar warn once per position; the paper tier fill line names the bar that traded through.

**Backtest.** `Backtester(resting_tp_trade_through=True)` (default off) applies the same helper per bar; it refuses without `platform="hyperliquid"`, perps, `execution_spec` (the tick grid comes from its `size_decimals`), a tier close, or with the dynamic close or scale-in. `run_backtest.py` reads a paper `--config` strategy's flag and takes `--resting-tp-trade-through` for a live strategy, and both need `--mode single --manifest`. Other `Backtester` callers keep the default. Fees are unchanged (issue 1726).
Each resting tier booking records its winning `resting_fill` in the ledger events (version 3), and `ledger_compare.py` checks every tier close against it (§ Ledger comparison, Resting take-profit fills). The comparison is offline and research-only and changes no runtime path.

## Hyperliquid Batched Signal Checks

Due Hyperliquid perps strategies that share market data run ONE batched signal check instead of one process per strategy. Same decisions, fewer subprocesses, faster cycles. Strategies batch together when they share data platform, symbol, timeframe, OHLCV limit, and ATR method.

Operator-visible behavior:

- Set `GO_TRADER_HL_BATCH=0` (or `off`/`false`/`no`) to disable batching entirely and go back to one process per strategy.
- A batch that fails on the shared market-data step makes **every member spawn its own check the same cycle**, so a batch failure never blanks a close, stop, ratchet, protection sync, or hedge. One alert is raised per group, not per strategy.
- Three consecutive shared-state failures on one group revert it to per-strategy checks and retry the batch every 10 cycles. A success clears the state and re-alerts as recovered.
- A slot whose configuration changed between snapshot and dispatch spawns its own check instead of trusting the batch.
- In a paper-only process an all-paper group runs its batch child at the paper nice (§ Paper CPU priority). On a saturated host that child can reach the batch timeout more often; a timeout takes the shared-failure path above, and the per-member checks it starts are also at the paper nice.

The log line names the group as `platform/symbol/timeframe/limit=N/atr=M`.

### Market feed (`market_feed: websocket`)

Root `market_feed` chooses where Hyperliquid candles come from. `rest` (the default, and what an omitted field means) keeps per-check REST polling and last-run scheduling. `websocket` opens ONE Hyperliquid socket, keeps the candle history and mid prices in the Go process, and hands every covered check a sealed snapshot on standard input. The mode is logged at startup as `Market feed: rest (legacy polling)` or `Market feed: websocket (Hyperliquid perps + manual)`, and changing it is restart-required.

**Scope.** Hyperliquid `perps` and `manual` strategies only. Every other platform keeps legacy polling under both values.

**Seven REST consumers stay outside the feed** and outside the zero-call criterion: the manual-open ATR fetch (`--fetch-atr`; status UI opens and limit rows with no stored ATR only, since a command-line open under a sealed feed must pass `--atr`), the manual-open regime-trail label check (`resolveManualRatchetRegimeLabel`, which runs the check script off-cycle with no feed context from both the CLI and the UI open paths), the LLM entry-review candles, the UI candle chart, the status-server mid fetch, the liquidation-guard off-cycle mid fetch, and OKX perps checks that fetch their own candles.

**Scheduling.** In-scope strategies become due on epoch-aligned deadlines (`floor(now / interval) * interval`), so live and paper twins on the same cadence share one evaluation identifier and one snapshot no matter how long each check takes. A missed deadline is never replayed: the next cycle consumes only the newest one. Strategies on other platforms keep last-run scheduling. Cadences that differ (a drawdown-accelerated strategy, say) get their own identifier, and the cycle log says so.

**Freshness.** A key is ready once it holds at least 30 bars (`coverage_short` is reported when the venue's history is shorter than the lookback). It is stale when its newest bar closed more than two intervals plus 60s ago, or when a connected socket has sent nothing for that key in `max(2 × interval, 5m)`. Mids expire after 15s. A reconnect gets a 30s grace before silence counts. A sealed snapshot older than `max(60s, interval / 2)` at dispatch holds entries.

**Outage behavior.** A key that is not ready and cannot be recovered yields no candle frame. The strategy still evaluates: signal 0, close fraction 0, price from the freshest verified mid, and a `degraded` reason in the log plus one throttled owner alert per key. Trailing stops, ratchets, protection sync, hedge sync and reconciliation keep running on verified inputs; candle-dependent closes report degraded rather than guessing. Entries are held at both in-scope dispatch sites. With no verified mark either, the strategy is skipped with a CRITICAL line. The operator pause state is never written.

**Accounting funding.** Eligible paper HL strategies (Paper Funding Accrual) add a separate need, `AccountingCoins` (each coin plus its hedge coin), kept apart from the signal funding flags: it never triggers `fundingHold`, never holds a check and never deletes a seal key. The owner keeps a 7-day rolling `fundingHistory` window per coin, refreshes it from the last record minus 3 hours under ledger reason `accounting_funding` (at most every 5 minutes and only once the next hourly record can exist, 60s after a failure; each request leaves two budget slots for other reads), trims it at 7 days, and seals it with coverage `{from_ms, to_ms}`. A changed or vanished record refuses the refresh, keeps the previous coverage and raises `accounting_funding_failed`.

**Never a silent private fetch.** With the feed on, a malformed, stale, incomplete or mismatched payload is an explicit error. Python sets the adapter aside for candle, higher-timeframe and funding reads whenever a market payload is present.

**Counting.** Steady-state evaluations make zero candle REST calls. Bootstrap, reconnect repair and stale recovery are counted separately and printed each cycle:

```
[feed] snapshot=300s/1788592500/1 keys=1 ready=1 stale=0 rest{bootstrap,repair,recovery,correction}=1,0,0,0 steady_candle_rest=0
```

`/status` carries a `market_feed` block with the mode, connection state, generation, last snapshot identifier and per-key readiness.

**Closed-bar correction.** The socket can miss a venue revision of a closed candle, and a healthy key otherwise never re-reads it.
Under `market_feed: websocket` and on a `role: feed` service with `feed.source: websocket`, the owner re-reads every retained closed bar at close+5s, close+20s and close+60s.
One `candleSnapshot` read starts at the oldest bar with a due checkpoint, and checkpoints that fall due within 5s of each other share a read.
Reads run beside the seal deadline, so correction never extends it, and a sealed snapshot keeps its bytes and hash; only later seals carry a corrected bar.
Against a socket-stored bar, REST replaces it only when its volume is not lower (volume only grows within a bar), so an older venue read never overwrites newer socket data, and a socket bar never lowers a REST bar's volume.
That rule is an assumption until host captures confirm it: a lower-volume REST revision against a socket-stored bar is rejected by correction and shows as `rest_older`, then overdue, then `unverified`.
REST-versus-REST keeps newer-read-wins ordering, including lower-volume revisions, so the volume rule does not change how the REST backup orders its own reads.
A read that returns no candles, an invalid response, a timeout or a budget refusal leaves the checkpoint pending and retries after 2s, doubling to 30s.
An HTTP 429 from the venue pauses every correction read for 60s (`rate_limited`), because the venue limit is per IP.
A read that returns no newer value for a due bar counts as a failure.
Supported window: a revision that lands by close+60s is corrected by a successful read.
The supported bound is the final checkpoint plus the 5s coalescing delay plus one successful read: close+65s on a 1m key, where the previous bar's final checkpoint always waits for the next bar's close+5s read, and close+60s where no checkpoint coalesces.
The bound holds only while reads succeed and the correction cap is not reached.
A bar whose final checkpoint is still unconfirmed 60s later logs one `correction_overdue` line per key and shows in `/status` (`market_feed.correction`: `reads`, `retries`, `failed`, `refused`, `capped`, `rate_limited`, `corrected`, `added`, `rest_older`, `unverified`, `pending`, `overdue`, `max_reads_per_minute`, `window_reads`, `paused_until`; per key `correction_pending`, `correction_overdue`, `correction_error`).
The correction status block is present only while the correction loop runs; REST backup owners omit it.
Overdue correction never changes key readiness.
After 10 minutes past the final checkpoint the owner stops retrying that bar and logs `status=unverified`; when the overdue episode then ends, the key logs `status=abandoned`, and `status=recovered` only when every overdue bar was confirmed by a read.
Each actual change logs `[feed-correction] status=corrected` with the key, open time, changed fields, prior source and delay.
Every read is counted in the request ledger as `correction` (first attempt) or `correction_retry`; a standalone scheduler installs an unlimited ledger for this, and a `role: feed` service uses its own.
Load is about two to three reads per key per bar interval on a healthy venue (a stubbed 10-minute run gave 2.1 reads per minute for a 1m key and 3.5 per 5 minutes for a 5m key), plus retries after failures.
Each owner holds a hard cap of 20 correction reads per rolling 60s across all keys, in both deployments and whatever the request ledger enforces.
Due keys are served oldest checkpoint first; past the cap a key stays pending (`status=deferred`, counted in `capped`) and can become overdue.
Hyperliquid documents a 1200-per-minute REST weight limit per IP shared with order traffic, with `candleSnapshot` at weight 20 plus more per 60 returned bars, so the cap holds correction to about 420 weight per minute (roughly 35%).
The cap covers about nine 1m keys; more keys show as `capped` and overdue.
A revision after close+60s is outside the window.
`scripts/feed-revision-capture.py` records socket and REST samples for measuring revision delay.
It caps reads across workers at 10 per rolling 60s (about 210 weight); `--max-reads-per-minute` can lower the cap.
Startup refuses plans whose average exceeds the cap or whose peak rolling 60s request count reaches it; accepted schedules leave at least one read of margin for worker start delay.
Defaults capture BTC on 1m and 5m at +1.5s, +5s, +20s and +60s; run other coins in sequential captures.
Report output separates budget refusals from failed requests.
Refused samples are recorded, and HTTP 429 pauses all new reads for 60s with a pause record.
Windows are limited to 50 bars and the anchor interval.
Shared consumers log `correction=n/a`, because wire v1 omits that counter.

### Shared market feed (`role: feed`, `market_feed: shared`)

Separate live and paper schedulers read the same immutable market snapshot from one feed service, so a paper process failure cannot stop the live process and both evaluate identical inputs. Topology: one primary feed service (websocket source), one backup feed service (REST source, running continuously as a warm standby), one live scheduler, one paper scheduler holding the paper deployments as `paper_sources` partitions. Consumers ask the primary first and the backup after a bounded failure. Paper-testing stays separate. This boundary does not isolate the services from a host failure or a feed outage.

**Feed config.** The same binary runs the feed when the config sets `role: "feed"`. The allowed root keys are `role`, `feed`, `status_port`, `log_level`, `discord`, `telegram`, `alert_throttle_interval`, `config_version` and an empty `strategies`; any other key (a state file, a strategy, a risk block) refuses by name. `status_port` is required and bound exactly, with no fallback port. `feed` holds `source` (`"websocket"` for the primary, `"rest"` for the backup), `socket_path`, `consumer_configs` (the scheduler configs it serves; relative paths resolve against the working directory) and `request_budget` (`{"per_minute": N, "startup": M}`; required and enforced for `rest`, optional and only logged for `websocket`). The feed opens no state file, takes no trading lock and never runs a check script; `--once`, `--summary` and `--leaderboard` exit 2. See `scheduler/config.feed.example.json` and `scheduler/config.feed.backup.example.json`.

**Consumer config.** A scheduler sets `market_feed: "shared"`, `shared_market_feed.primary_socket` and, for failover, `shared_market_feed.backup_socket` (absolute, clean, and different from the primary). It owns no websocket and makes no candle fetch for covered strategies; every Hyperliquid perps and `manual` check still reads one sealed snapshot through the unchanged stdin payload (`v:2` batch envelope, market payload v2 since #1665, 8 MiB cap).

**Requirements.** The feed loads each consumer config read-only and without live-credential checks, derives each one's keys with `deriveFeedRequirements`, and serves the union: each key at the highest lookback any consumer asks for, the union of mid coins, and funding needs combined per coin. Strategy maps stay inside each consumer, so two consumers may reuse a strategy id. A consumer is skipped with an owner DM when it does not load, is not in shared mode, or does not name this feed's socket; zero loadable consumers exit 78.

**Schedule.** A seal key is a deadline instant in Unix seconds. The feed schedules every cadence its consumers can select: each feed-scoped strategy's `interval_seconds` (else the root value, else 60) plus the 90-second drawdown-warning cadence for any strategy whose interval is above 90. It seals the whole union at `deadline + 5s` (settle delay) with a 20s preparation budget for stale-key recovery and funding, and stores the bytes only until `deadline + 27s` (hard deadline). Seals run on schedule whether or not a consumer asks. `describe` publishes the cadences, first deadline and timing values. Sealing starts at the first deadline after the initial generation publishes (or after the 120s startup budget). A restarted feed never serves a key from before its own start, and every reply names the feed instance (`host-pid-startms`).

**Seal v3.** A seal that carries accounting funding (`accounting_funding`: coin, `from_ms`, `to_ms`, records, `fetched_at_ms`, `error`) is seal version 3; describe adds `accounting_funding` and `accounting_window_ms`. The union, the size estimate and the consumer coverage check carry the need, and a consumer reports `accounting funding <coin> not served` as a gap. A config with no eligible paper strategy keeps byte-identical v1/v2 seals and describe. A consumer built before v3 refuses a v3 seal, so the feed and every consumer must run the same SHA (`scripts/update.sh --restart`).

**Seal contract.** A seal is canonical JSON (keys, mids and funding in sorted order); its SHA-256 hash travels beside it, never inside it. Stored bytes are immutable: a second attempt for a key keeps the first, and a key past its hard deadline with no seal answers `unavailable` from then on. A late request gets the retained bytes. Each cadence keeps its last three sealed deadlines; older seals are evicted and logged (`[feed-seal] status=evicted`). An evicted, missed or pre-start key answers `unavailable`, never current data under an old key. A consumer measures decision age from the deadline and from the seal time and uses the older (`max(60s, interval/2)` holds entries).

**Backup source (`source: "rest"`).** The backup runs continuously with the same consumer union, schedule, settle delay, preparation budget, retention and immutable-key contract as the primary.
It opens no websocket.
It bootstraps every key over REST, then does all network work inside each seal's preparation window: at deadline D it refreshes each key whose cadence set contains a cadence that divides D (the due keys), then the funding refresh (5-minute freshness) and last one `allMids` read, so the sealed mids are the newest input.
A key's cadence set is the union of the cadences of every consumer that reads it, because a consumer can move a strategy to any of its own cadence keys after a skipped deadline.
Signal keys refresh before higher-timeframe and regime keys; a key that is not due is never fetched, so no request goes to an input no consumer reads at D.
A due refresh that fails is retried once after 1s when the preparation window allows. **A backup seal marks a key ready only if the key was refreshed for that deadline.**
A due key whose refresh fails or is refused is sealed not ready (`failed` or `budget_exhausted` with the deadline in its detail), and a key that was not due is sealed not ready as `not_due` in that seal only (the feed's own state is unchanged).
A consumer whose cadences changed by SIGHUP before the feed reloaded, or that the feed keeps `Retained` with old cadences, therefore holds entries on such a key instead of trading on a closed bar with a partial value.
The next successful refresh makes a failed or refused key ready again.

**Request budget.** Every Hyperliquid `/info` request of a feed process (candle snapshots with each history-widening pass, funding scalar and funding history pages, `allMids`) passes one ledger (`market_feed_budget.go`), which counts it by reason (`bootstrap`, `refresh`, `recovery`, `retry`, `funding`, `mids`, `repair`) and request type.
For `rest`, the ledger enforces `per_minute` over a rolling 60s window and a `startup` allowance that only bootstrap requests draw on, refilled at startup and at each reload; bootstrap past that allowance uses the window.
Key refreshes and funding leave one request in the window for the mids read.
A refused request never blocks: the key is sealed `budget_exhausted`, refused funding stores `request budget exhausted` as its error (funding strategies hold), and refused mids age out (the consumer uses its REST mark fallback).
One `**MARKET FEED REQUEST BUDGET EXHAUSTED**` owner alert fires per episode and one `**RECOVERED**` alert when a seal is prepared with no refusal.
The budget values must come from a measured baseline of today's consumers (see Measured values); a websocket feed counts and logs without refusing.

**Transport.** One Unix stream socket per feed (mode 0660, under the unit's `RuntimeDirectory`), one length-prefixed request per connection: `describe` or `snapshot <key>`.
Replies are `sealed` (header plus seal frame), `pending` (with retry and give-up times), `unavailable` or `error`.
Wire v1 and the seal versions are independent of the stdin payload versions.
A seal that carries open-interest observations (#1637) is seal v2; every other seal, and every reply header of a feed with no observation coverage, stays seal v1 with the v1 bytes, so a v1-only consumer keeps reading a feed that serves no open-interest strategy during a feeds-first update or a rollback.
A consumer accepts seal v1 and v2, refuses a v1 seal that carries observations or a v2 seal that carries none, and requires the seal body version to equal its reply header.
A seal that carries trade flows (#1638) is seal v4 whether or not it also carries open interest or accounting funding; a seal without trade flows keeps its v1, v2 or v3 bytes, so a consumer that predates v4 keeps reading every feed that serves no trade-flow strategy, and refuses a v4 seal or header.
A seal that carries macro calendar or quote sections (#1654) is seal v5 whatever else it carries; every other seal keeps its earlier bytes and version, and a consumer that predates v5 refuses a v5 seal or header.
A seal that carries volume profiles (#1665) is seal v6 whatever else it carries; every other seal keeps its earlier bytes and version, and a consumer that predates v6 refuses a v6 seal or header.
A seal that carries `l2_books` sections (#1635) and no `perp_premiums` is seal v7; a seal that carries `perp_premiums` (#1639) is seal v8 whatever else it carries (including `l2_books`, volume profiles and macro sections). Every other seal keeps its earlier bytes and version, and a consumer that predates v7 or v8 refuses that seal or header.
The market payload version (`marketSnapshotVersion` in Go, `MARKET_PAYLOAD_VERSION` in `shared_tools/market_payload.py`) is 4 since #1639 (3 since #1635, 2 since #1665). One version carries the optional `l2_books` and `perp_premiums` sections; the Go builder and the Python validator change together and refuse any other version. A consumer requires exact payload-header agreement with every peer, so a payload-3 feed is refused even when it serves an older seal form.
Requests are capped at 4 KiB, headers at 1 MiB and seals at 64 MiB, each checked before allocation.
A consumer rejects an unknown field, a non-canonical seal, a hash or header mismatch, or a version it does not speak.
The feed holds `<socket>.lock` for its lifetime (exit 79 when another process holds it) before it removes a stale socket, so a running peer's socket is never replaced.
Each connection has a 10s deadline and at most 64 run at once.

**Consumer cycle.** Shared cycles use one deadline each.
When strategies are due on different deadlines, the earliest runs first and the rest run in the next cycle.
A consumer never requests a key after its give-up time (`deadline + 32s`).
When a strategy's own deadline is past that time at cycle time (after a restart, or after a long cycle), the consumer logs `[feed-audit] key=<k> status=skipped` once for that key.
The strategy then runs on the newest key of this consumer's cadences that is still before its give-up time, or it waits for the next such key, so an old seal is never evaluated.
The consumer asks the primary for that key and waits through `pending` up to the primary's own hard deadline, bounded by `deadline + 29s` (the give-up time less a 3s backup reserve, applied as a connection deadline so a hung socket is bounded too).
A fetch that starts after that bound still gets one probe of the primary, up to 2s and never past the give-up time, so a primary that already holds the sealed key serves it; a `pending` reply then fails over at once.
An endpoint is never asked once the give-up time has passed.
A `pending` reply inside the bound is a wait, never a failure.
After a transport error, an `unavailable`, `error`, incompatible or malformed reply, or `pending` past the bound, it asks the backup for the same key, waiting up to `deadline + 32s`.
No endpoint ever rebuilds a missing historical seal, so a key the backup missed or evicted is degraded.
`**SHARED MARKET FEED FAILOVER**` fires when the backup serves a key and the previous served key came from the primary, or no key was served since startup or the last outage; it names the primary's error and the backup's source, instance, generation and hash; `**SHARED MARKET FEED PRIMARY RESTORED**` fires when the primary serves again after the backup.
Every key tries the primary first, so the return to the primary is automatic.
The consumer verifies the hash and canonical form and checks coverage: a key missing from the seal or below this consumer's lookback is removed, so its strategies hold entries (one `COVERAGE GAP` alert per change).
With no compatible seal it builds a non-nil degraded snapshot: entries hold, covered checks never fall back to private candle reads, and closes, stops, ratchet, protection, hedge sync and reconciliation continue on verified inputs; with no verified mark the strategy is skipped, as in websocket mode.
One owner alert fires per outage transition and one per recovery.
A fetch that ends because the consumer is stopping logs its degraded key with a `the consumer is stopping` reason and sends no alert.
A sealed mid is a valid mark only while its receive time is at most 15s before the current time, so a seal consumed late never supplies old marks.
A covered strategy that has no such mark and no REST mark is degraded and skipped for that cycle, and its batch group falls back to per-strategy checks, so a check script never prices from an old sealed mid.
The manual close evaluation receives the same marks.
Coins without such a mid use the REST mark fallback.
It can supply check prices and degraded results as well as valuation, and each use is logged as `[feed-audit] key=<k> mark_fallback=<coins>`.

**Open-interest observations (#1637, research-only).** A consumer strategy whose open strategy is `open_interest_breakout` adds an observation need for its coin: a window of `oi_lookback` bars (default 4) times the bar interval plus `max_observation_age_ms` (default 120000) plus one minute.
Only a `websocket` source collects it (the `activeAssetCtx` subscription).
A `rest` feed source drops only the observation need and keeps serving that consumer's candle, mid and funding keys (owner DM, `notice` in feed `/status`); the consumer sees the missing window as a coverage gap and its open-interest strategies hold entries.
A scheduler on `market_feed: rest` refuses the config.
The owner keeps the latest sample per one-minute bucket by receipt time (the venue sends no event time), marks a disconnect as a gap from the last sample (with its detection time) that the first sample of the next session closes, rejects non-finite, negative and backwards-clock records, and trims to the largest window plus two minutes.
A seal copies each needed window with the gaps detected by its deadline, cut at that deadline, and `describe` lists it; the evaluator counts a gap only when it was detected at or before the bar close, so a backtest never uses a disconnect the live decision could not yet see; a consumer reports a missing or short window as a coverage gap.
The payload carries it as `observations["<coin>|open_interest"]` with the bar interval and row-endpoint offset.
A missing, stale, gapped or thin window never holds the candle frame: closes, stops and protection continue, and the Python evaluator gives a reasoned entry hold.
The strategy is `edge_status=no_edge` (`study_inconclusive`): it runs with explicit `--mode=paper` and needs `allow_no_edge: true` for live use.
`go-trader record-observations --coins <list> --out-dir <empty dir>` records the same stream into hashed JSONL segments for offline study (`backtest/observation_replay.py`).

**Trade-flow observations (#1638, research-only, production-refused).** A consumer strategy whose open strategy is `cumulative_volume_delta_divergence` adds a `trade_flow` observation need for its coin: `session_hours` (default 4) hours plus one bar plus one minute.
Only a `websocket` source collects it (one `trades` subscription per coin on the existing socket); a `rest` source drops only the need, and `market_feed: rest` refuses the config. Config admission refuses the strategy itself in every mode, so no production config reaches this path today.
The owner folds trades into exact per-minute buy/sell aggregates by venue block time (8-decimal fixed point, `side` B = buyer aggressor). Identity is venue, namespace, coin, block time and tid (the venue documents tid as a 50-bit hash of the two order ids): an identical duplicate is idempotent, a conflicting duplicate invalidates both minutes.
Coverage starts at the first minute after the subscription acknowledgement. A minute becomes final at end + 5 s with a later frame for the coin; a disconnect, a 30 s frame silence (coverage resumes on the next frame), a malformed element or frame, a future-dated trade, a receipt-clock regression or an identity-store overflow invalidates the open minutes, and a late trade for a final minute revokes it with its detection time. Missing minutes never count as zero.
A seal copies the minutes final by its deadline with their status as of that deadline (seal v4, strict decode of the contract, decimals, tallies, hashes and finalization times); `describe` lists the key with source `hyperliquid_ws_trades`; the payload carries `trade_flows["<coin>|trade_flow"]`. Trade flow never holds the candle frame.
The evaluator also requires each used bar's exact minute total to equal its candle volume. `record-observations --kinds trade_flow` records raw frames with per-trade outcomes and every coverage, interval, invalidation and tick event (run/segment/recording schema v2; open-interest-only runs keep v1); `observation_replay.py import-trades` replays a run independently and refuses it when any outcome or event differs.
**BTC leader frames (#1640, research-only, production-refused).** A consumer strategy whose effective open strategy is `bitcoin_lead_lag` adds a candle requirement for `BTC` on its own signal timeframe (`feedStrategyRequirement.Leader`/`LeaderLookback`, `bitcoin_lead_lag_feed.go`): `training_bars + 2*lag_bars + 2*horizon_bars - 2` closed rows plus one forming row, with timing, and raises its follower lookback to the same count.
`cycleRequirementsForDue`, `frameSpecs` (one frame when BTC is the follower) and the batch payload (per-key timing) carry it, and the feed union marks it a signal key for REST refresh priority.
A missing leader frame is a `holdFor` hold and the payload builder refuses it, so the whole candle evaluation degrades while verified protection continues.
Python reads both frames only from the sealed payload (`shared_tools/lead_lag_inputs.py` `prepare_sealed_inputs`: one cutoff, `sealed_frame_timings` + `select_closed`, matched boundaries, no gaps or forward fill); a usable follower with unusable leader input holds the entry while the close owner runs.
Config admission refuses the strategy in every mode; a promotion must also refuse it under `market_feed: rest`.

**Macro-event inputs (#1654, research-only, production-refused).** A consumer strategy whose open strategy is `macro_event_breakout` names a calendar artifact (`calendar_path`, `calendar_sha256`) and optionally a quote artifact (`quote_path`, `quote_sha256`) in its open-strategy params; the feed owner needs absolute paths. Calendar records are shared by artifact digest; quote records by venue, namespace, coin and digest. One path pinned to two digests, a relative path or bad params become a diagnostic on that strategy only (an entry hold), and peers keep their requirements; `market_feed: rest` refuses the config. Config admission refuses the strategy itself in every mode, so no production config reaches this path today.
`ApplyGeneration` reads each new artifact once, outside `feedMu`, hashes and validates the same bytes (`macro_event_calendar/v1`, `macro_event_quotes/v1`, owner `shared_tools/macro_event_inputs.py`; the Go reader in `market_feed_macro.go` mirrors its contract) and installs it only with the matching generation token.
A later generation with the same identity reuses the installed records and never rereads the file; a restarted owner refuses changed bytes by digest.
A freeze copies calendar records known by the cutoff and quote records received in `[cutoff - window, cutoff]` (at most 20,000 per seal); no network request runs at seal time.
`describe` lists each input; the payload carries `macro_events["<calendar sha256>"]` and `macro_quotes["<coin>|<quote sha256>"]` (payload version 1 when added; 3 since #1635).
An unavailable input is an entry-only hold (`feedHold.EntryOnly`): the check still runs so closes, stops, ratchet and protection continue.
An artifact identity change is restart-required for the scheduler (`validateHotReloadCompatible`) and the feed service (`macroGenerationReloadErrors`).

**Volume profiles (#1665, research-only, production-refused).** A consumer strategy whose open strategy is `volume_profile_reclaim` adds a `volume_profile` observation need for its coin (window two UTC days) and a timed signal frame.
Only a `websocket` source collects it, on the coin's existing `trades` subscription and ingestion path; a `rest` source drops only the need, and `market_feed: rest` refuses the config. Config admission refuses the strategy itself in every mode, so no production config reaches this path today.
The owner keeps the profile on the coin's trade state: validated accepted trades add exact base size per canonical price to the UTC session accumulator (identical duplicates add nothing); a profile-only coin keeps an internal trade state whose trade flow is never sealed.
A session is final only when all 1440 minutes finalize complete; an invalid or twice-finalized minute, a conflicting duplicate, a new trade for a finalized minute, overflow, more than 16384 prices, a price over 24 characters, zero volume or one price make it unavailable. A revocation after finalization dates the frozen artifact's invalidation.
A seal carries the session before its cutoff, plus the one before that during the first hour of a session (the longest supported bar interval), each with status `available`, `invalidated`, `pending` or `unavailable` and a reason code; at most 16 profile coins per union, 1 MiB of pairs per session.
Strict decode refuses contract or identity mismatches; a malformed session becomes unavailable metadata (`seal_malformed`) without rejecting the seal. The payload carries `volume_profiles["<coin>|volume_profile"]`; a missing profile never holds the candle frame or a batch, and the evaluator returns a reasoned zero entry while the close owner runs.

**Order-book depth (#1635, research-only, production-refused consumer).** The frozen contract is `docs/research/1635-l2-book-contract.md` (hash-pinned JSON beside it). A typed `l2_book` need (source, contract, time and identity semantics, window, capacity, depth) comes from `feedBookStrategyNeeds`; `order_book_imbalance_feed.go` registers `order_book_imbalance` there (window 60000 ms, capacity 256, depth `depth_levels`), with a Go validator that mirrors the Python one through `shared_strategies/open/testdata/order_book_imbalance_param_cases.json`. Config admission refuses the candidate in every mode, so no production config reaches the book need today; needs merge per key by maximum and refuse different semantics, more than 16 keys, or a set over the joint seal (16 MiB), envelope (4 MiB) or memory (64 MiB) estimate.
Only a `websocket` source collects it (`{"type":"l2Book","coin":C,"fast":false}`, native precision, acknowledgement checked; subscriptions are tracked by key state, so a key released and tracked again before the next sync unsubscribes and subscribes again); a `rest` source drops only the need, and `market_feed: rest` refuses the config.
The owner keeps distinct canonical snapshots (identity = length-delimited SHA-256 of host, namespace, coin, event time and all levels) up to the need depth, with outcomes `accepted`, `duplicate`, `conflict`, `out_of_order`, `rejected`, `uncovered` and `untracked`, and markers for epoch start, subscription, disconnect, invalid frames, conflict, ordering, clock regression, silence over 10000 ms (timer and freeze), evidence loss and depth raise; marker times never decrease per key (an earlier detection takes the previous marker's time, mirrored by the replay).
A seal carries the observations and markers known by its deadline with `retained_from_ms`; the payload carries `l2_books["<coin>|l2_book"]` cut to the consumer's window, capacity and depth with `prefix_complete`, or bounded `book_capacity_refused` metadata when it does not fit (the candle frame stays). A missing, short or incomplete book is an entry-only hold, and the Go hold equals `!prefix_complete` of the section built for that strategy's own need (capacity cut included); only a missing or short seal (key, semantics, window, capacity, depth) is a coverage gap, never an incomplete prefix; `shared_tools/market_payload.py` `market_l2_book` is the typed reader.
`order_book_imbalance` (`shared_strategies/open/order_book_imbalance.py`, typed input `shared_tools/book_inputs.py`) decides once per episode at a one-second deadline from the supplied section; `check_hyperliquid.py` passes `l2_book_observations` from the market payload only, the closed-bar branch refuses it (`unsupported_cadence`), and every candle research tool (`run_backtest.py`, `optimizer.py`, `eval_windows.py`, `exit_policy_ab.py`, `ledger_compare.py`) refuses it before data is read; its study runs only on `backtest/book_event_simulator.py` over the public S3 archive (`backtest/book_archive.py`, `docs/research/1635-l2-book-archive-note.md`).
`inventory_market_making` (#1861) registers a book need of depth 20, capacity 256 and a window of the volatility window plus a one-second predecessor plus `max_input_age_ms` (`inventory_market_making_feed.go`); a `rest` source refuses it, 256 observations cannot hold a full window at every source rate, and it is production-refused, so only the offline quote replay reads books for it (Offline quote replay).
`record-observations --kinds l2_book` writes recording schema v3 (`book`, `book_sub`, `book_marker`, `book_need`, `book_release` records); `observation_replay.py import-books` replays it independently (`backtest/book_replay.py`) and `harness-books` compares sealed payloads with the replay. The open-interest, trade-flow and profile importers read v3 runs that recorded their kind.
Recording schema v3 is one shared schema with a typed contract block per kind: a v3 run, segment header and recording carry `book_contract` exactly when `kinds` holds `l2_book` and `premium_contract` exactly when it holds `perp_premium` (the `trade_contract` and `open_interest_contract` blocks stay as in v2). `book_replay.py`, `premium_replay.py` and the open-interest, trade-flow and profile importers refuse a v3 run whose kinds and typed contract blocks disagree or that names an unknown kind; no recorded byte changed, so the frozen #1639 premium recording imports as recorded.
**Quote/oracle premium samples (#1639, research-only, production-refused).** A consumer strategy whose effective open strategy is `perpetual_premium_reversion` needs observation kind `perp_premium` for its coin (`feedStrategyRequirement.Premium`/`PremiumWindow`, window `(lookback_samples + 1) * 60 s + bar interval`, at most 26 h), independent of both funding flags. The websocket owner (`market_feed_premium.go`) subscribes `bbo` per premium coin and `activeAssetCtx` once per coin for open interest and premium together; each consumer parses its own fields, so a malformed field of one kind never blocks the other. Per coin it keeps the latest accepted quote (bid, ask, sizes, order counts, venue time, receipt, session, sequence) and context (oracle, mark, nullable mid, receipt, session, sequence); the context has no source time, so `oracle_event_ms` stays null and freshness is the receipt age only. Receipt times are taken under `feedMu`.
A sample at each 60 s grid time T holds the latest components received at or before T and becomes final when the owner first processes a frame or tick after T: `sampled`, `missing` (no quote, no context, one-sided book, disconnected) or `invalid` (malformed frame, crossed book, conflicting duplicate, out-of-order or future-dated bbo, receipt-clock regression in that interval). An identical bbo resend is idempotent; a conflicting resend poisons that venue time and revokes earlier samples that used it with a dated `invalid_ms`. A disconnect clears both components and opens a gap, so a restart or reconnect rebuilds the window from new samples; readiness needs a contiguous single-session run.
The freeze copies samples in `[cutoff - window, cutoff]` and hides revocations after the cutoff; the payload carries `perp_premiums["<coin>|perp_premium"]` (payload version 4). The evaluator, not the owner, computes premium and every bound. `market_feed: rest` refuses the config and a `rest` feed source drops the need (its strategies hold entries), because deadline-only REST sampling cannot meet the 60 s contract; premium acquisition makes no `/info` request. An unavailable or invalid premium window is an entry hold inside the evaluator; the check still runs, so closes, stops, ratchet and protection continue. Config admission refuses the strategy in every mode, so no production config reaches this path today.

**Reloads.** A feed SIGHUP re-reads its config and every consumer config; an unreadable consumer keeps its previous contribution. The new union bootstraps new keys and publishes a new generation, which applies to deadlines sealed after it publishes. Changing `feed.socket_path`, `feed.source` or `status_port` is rejected; `feed.request_budget` values apply at the reload, and a `rest` config without them is rejected. A consumer SIGHUP re-derives its requirements and re-checks feed coverage with `describe`; a flat-only regime timeframe change stays hot-reloadable. **Reload the feed first**, then the consumer. Until the feed serves a new key, the affected strategies hold entries with a coverage-gap alert, and they recover without a consumer restart once the feed serves it.

**Audit and parity.** The feed logs one `[feed-seal]` line per deadline (key, status, source, instance, generation, hash, bytes, ready keys, preparation time, and the requests and refusals counted while that seal was prepared) and one `[feed-budget]` line per deadline (requests and refusals by reason and type, window use, per-minute limit, startup allowance left, refreshed keys, refused and failed keys).
Each consumer logs one `[feed-audit]` line per evaluated key (sealed or degraded, whether or not feed health changed) and one `[feed-payload]` line per market payload (key, kind, frame specs, coins, SHA-256).
`scripts/feed-parity.sh --feed-unit go-trader@feed-primary --consumer-unit go-trader@live --consumer-unit go-trader@paper [--since ...]` reads each unit's own journal namespace, joins records by key, and fails on empty input, a key of a cadence that a consumer skipped while its audit records listed that cadence as active (a consumer start, logged as `[feed-audit] event=start`, ends a run, so keys missed while a consumer was down do not count), a consumer hash that differs from the feed's for the same instance, or two consumers that built different payloads from one seal and one frame spec.
Degraded keys, skipped keys, keys served by the backup, source-switch races and mark fallbacks are listed as exceptions, and each consumer line counts its keys per endpoint and source.
Pass both feeds (`--feed-unit go-trader@feed-primary --feed-unit go-trader@feed-backup`) so a backup-served key is checked against the backup's own seal.
`--feed-log label=file` and `--consumer-log label=file` read files instead of the journal.

**Source comparison.** Independent sources are not byte-identical, so cross-source equality is measured separately: `scripts/feed-source-compare.sh --binary ./go-trader --primary-socket <p> --backup-socket <b> [--key <k>]` fetches the same key from both feeds with `go-trader feed-fetch` (which verifies each seal's hash) and compares every closed bar in the open-time range both seals hold (open, high, low, close, volume) and every funding record.
A candle key that either seal marks not ready is skipped with a NOTE line, because its last closed bar can be a partial bar from an earlier refresh.
It excludes source labels, receive times, readiness detail, instance, generation, seal time, forming bars, mids and current funding scalars, prints every difference with both values, and exits 3 when any exists.
It exits 1 with an `INCONCLUSIVE` line when no closed bar was compared, so an all-skipped run never reads as a match.
`go-trader feed-fetch --socket <p> [--key <k>] [--out <file>] [--describe]` is the read-only client behind it.

**Request baseline.** `scripts/hl-request-ledger.py proxy --port <n> --label <name> --ledger <file>` forwards `/info` requests to the venue and logs one line per request; `report --ledger <file>` prints per-label totals by type, cold-start and steady per-minute load (mean, 95th percentile, peak) and the summed load.
A measurement binary built with `-ldflags "-X main.hlMainnetURL=http://127.0.0.1:<n>"` (plus `-X main.hlFeedWebsocketURLOverride=wss://api.hyperliquid.xyz/ws` for a websocket process) sends its Go reads through the proxy, and `PYTHONPATH=scripts/hl_request_ledger HL_REQUEST_LEDGER_URL=http://127.0.0.1:<n>` sends the check scripts' SDK reads through it.
The shim never redirects the SDK's `Exchange` (order) client, which signs by comparing its URL with mainnet, and it does nothing, with one stderr line, in a process where `HYPERLIQUID_SECRET_KEY` is set.
Run paper-mode copies of the production consumer configs this way to set `per_minute` from the summed steady peak and `startup` from the summed cold start.
The Python OHLCV cache is shared between processes on one host and private per unit under `PrivateTmp`, so measure on the service host.

**Status.** The feed serves `/health` (`role: feed`, version, pid, instance, generation, last seal key and age; unhealthy after 10 minutes without a seal) and `/status` (owner readiness, the `describe` block and each consumer's load state); both carry `request_budget` (limits, window use, startup allowance left and cumulative totals by reason and type). A consumer's `/status` `market_feed` block carries `mode: shared` and a `shared` object with `served_by` (the endpoint of the last served key), each endpoint's last status and the last key, source, instance, generation, hash and ready-key count.

**Deployment.** Run each service from its own deployment directory and unit, for example `go-trader@feed-primary` (`/opt/go-trader-feed-primary`), `go-trader@feed-backup` (`/opt/go-trader-feed-backup`, socket `/run/go-trader-feed-backup/feed.sock`), `go-trader@live` and `go-trader@paper`.
Both feeds list the same consumer configs, and each consumer names both sockets.
Both shipped units create `RuntimeDirectory=go-trader-%i` (`go-trader-main` for the plain unit) with mode 0750, so the feed socket goes at `/run/go-trader-feed-primary/feed.sock`; consumers run as the same user and connect through their read-only view of `/run`.
Consumer units must not `Requires=` or `BindsTo=` a feed.
An optional drop-in such as `/etc/systemd/system/go-trader@live.service.d/feed.conf` with `[Unit]`, `Wants=go-trader@feed-primary.service` and `After=go-trader@feed-primary.service` orders boot without tying the live service to the feed.
`bash scripts/update.sh --all --restart` discovers active units plus enabled units that are stopped or failed and whose `--config` sets `role: feed`, so a stopped feed is updated and started.
Scheduler discovery stays active-only: a scheduler unit that an operator stopped stays stopped.
It updates every `role: feed` deployment before the schedulers and verifies each one's `/health` version and fresh pid.
A glob-discovered `/opt/go-trader-<name>` with no discovered unit maps to `go-trader@<name>` when that unit is active, or enabled with `role: feed`, and its working directory matches.

**Converting an existing deployment (optional).** Updates never convert a deployment. `market_feed` defaults to `rest`, and the Post-Update Agent Protocol forbids a behavior change without the operator's consent, so a deployment that does nothing keeps its mode on every update. An operator who wants the shared feed runs `sudo bash scripts/shared-feed-convert.sh` in stages (systemd only; exit 2 usage, 10-19 plan refusals, 20-29 stage failures, 30 failed restore):
1. **`plan [--consumer <unit>]...`** is read-only. It lists every scheduler unit (named `go-trader-*.service` units and `go-trader@*` instances) with its user, config, version, `market_feed` and covered strategies. For a selection, it refuses in these cases: a unit that is not an active scheduler, no covered strategy, a binary without the backup feed, a running process that is not the binary on disk, consumers whose scheduler sources differ, and consumers that run as different users. It refuses (exit 19) when `uv` or Go cannot run as root and as the consumer user, and names the fix (Prerequisites). It reads the first consumer's git `origin`, and when git cannot read it, it refuses (exit 19) with the git error, the tree and its owner. It then runs the feed probe over temporary shadow configs and prints the target.
2. **`feeds --consumer <unit>...`** writes shadow copies of the consumer configs, converted to shared mode, under `/var/lib/go-trader/shared-feed/shadow/` (mode 0600, owned by the consumer user, in a 0700 directory; the feed configs are also 0600, because a config can hold bot tokens).
   The feed skips a consumer that is not in shared mode, so the shadows let both feeds start before any consumer changes.
   It repeats the `uv` and Go check, then clones the first consumer's `origin` into `/opt/go-trader-feed-primary` and `/opt/go-trader-feed-backup` and builds each with `update.sh --rsync-from <first consumer dir>`, which gives the same source and the same version as the consumers.
   The build runs as root on a root-owned clone (the rsync copies no owners), then the tree goes to the consumer user; a consumer tree that `go-trader` owns needs no `safe.directory` setting.
   It writes the two feed configs (ports 8190 and 8191 unless taken), links `scheduler/config.json` to each config, copies only `DISCORD_*` and `TELEGRAM_*` lines into each `.env` (created with mode 0600), installs `go-trader@.service` from the built feed tree (the consumers' release) with its journald namespace, and waits for both feeds to seal and to load every named consumer.
   A re-run keeps an existing feed config when it already serves the named consumers.
   When it serves only shadows (no switched consumer), the re-run sets its list to the new selection and records it.
   When it serves switched consumers, the re-run refuses and names `consumers`.
   `--per-minute`/`--startup` apply only to a new backup; a re-run with other values refuses and names `calibrate`.
   When the consumers run as a user other than `go-trader`, for example `root`, each feed unit gets a `10-shared-feed-user.conf` drop-in with `User=` and `Group=`.
   `update.sh` never rewrites drop-ins, so a later `update.sh --restart --unit go-trader@feed-<name>.service` keeps it.
3. **`calibrate [--per-minute <n> --startup <n>] [--baseline-ledger <file>] [--window <s>]`** samples the backup's `/health` `request_budget.window_used` for two periods of the longest cadence. It then writes the fixed operator values, or 1.5 times the measured peak and bootstrap total, and sends SIGHUP. It fails, and restores the previous budget, when any request was refused during the sample, when the peak plus the mids reserve does not fit, when the need exceeds a `--baseline-ledger` cap, or when the script is interrupted or stops early. `feeds` journals its budget as provisional, and `verify` and `switch` refuse until a `calibrate` passes.
4. **`verify [--accept-differences]`** requires both feeds healthy and `feed-source-compare.sh` exit 0. It re-checks a difference or an INCONCLUSIVE result once, on keys newer than the first comparison. Differences that remain stop the stage (exit 25) until the operator reviews the listed bars and re-runs with `--accept-differences`, which records the count in the journal. A bar that the venue revised after the primary sealed it is a known cause, because the websocket owner keeps that bar until it leaves the lookback window.
5. **`switch --consumer <unit> [--confirm-live <unit>] [--dropin]`** handles one running consumer.
   It keeps a byte copy (`<config>.pre-shared-feed.<UTC stamp>`), writes `market_feed: "shared"` and both sockets, repoints both feeds from the shadow to the real config, sends SIGHUP, and waits until each feed's `/status` shows the config loaded.
   It then restarts the unit and requires a fresh healthy pid.
   It waits for three `[feed-audit] ... status=sealed endpoint=primary` lines and requires `feed-parity: PASS` since the restart.
   Any backup-served or degraded key, any failed check, and an interruption (SIGINT, SIGTERM, SIGHUP from a lost terminal, logged to `interrupted.log` in the state directory) or early exit restores the byte copy and the shadow, restarts the unit and verifies its health (exit 26).
   A write to a closed output pipe during a guarded stage starts its undo.
   Every undo (of `switch`, `calibrate` and `consumers`) ignores SIGINT, SIGTERM, SIGHUP and SIGPIPE from its first step until it exits, so a second interrupt, a lost terminal or a closed output pipe does not stop a restore half way.
   Output after a lost terminal is not kept; `status` shows the result (journal, feed budgets, each unit's mode).
   The post-switch fingerprint is journaled right after the write.
   A re-run of `switch` on a shared config whose last switch record did not finish refuses and names `rollback --consumer`.
   A unit with a `--mode=live` or `manual` strategy needs `--confirm-live <unit>` or the unit name typed.
   `--dropin` adds `Wants=`/`After=` on both feeds, never `Requires=` or `BindsTo=`.
6. **`consumers --consumer <unit>...`** sets the full list of consumers that both feeds serve. It adds a new unit (served from a shadow until it is switched), drops units no longer named, and reloads both feeds. It refuses (exit 17), and changes nothing, when a unit it would drop is running or enabled with a config in shared mode on these feeds; a unit a fold stopped and disabled can be dropped. If either feed fails to load the list, or the stage is interrupted or stops early, both get their previous config back (written atomically), and the script checks that each running feed loaded it again (exit 27; exit 30 when that check fails). Use it after a paper fold.
7. **`rollback --consumer <unit> | --all`** restores the byte copy when the config still matches its post-switch fingerprint.
   When the config changed after the switch, for example in a fold, it reverts only `market_feed` and `shared_market_feed` and keeps the changed config as `<config>.pre-rollback.<UTC stamp>`.
   It restarts and health-checks only a unit that is running; a stopped or failed unit gets its config back and stays stopped.
   `--all` covers the current consumers and every unit with a switch that is not rolled back.
   It refuses, and changes nothing, when a current consumer is in shared mode with no saved pre-switch config.
   It then stops and disables both feeds, because `update.sh --all` starts an enabled stopped feed.
   It journals a reset first, so the earlier `feeds`, `calibrate` and `verify` results no longer count: `calibrate`, `verify` and `consumers` need a new `feeds` run, and `switch` needs all three again.
   The calibrated budget stays in the backup config as the starting value, and the new `calibrate` measures it again.

`status` prints the journal (`/var/lib/go-trader/shared-feed/convert.journal`), both feeds and each consumer.
Every stage except `plan` and `status` takes an exclusive lock (`convert.lock` in the state directory) for its whole run, so a second stage started meanwhile exits 18 and changes nothing; the lock ends with the process, on every exit path.
Every stage can be re-run; a finished stage keeps its feed config, and a switched consumer whose config still matches is left alone.
The paper fold (`scripts/merge-paper-instance.sh`) and the conversion are independent, and they can run in either order.
The fold refuses when `market_feed` or `shared_market_feed` differ between the target and a folded source.
Fold before converting, or switch the target and every folded source first.
After a fold on converted units, finish the fold's printed steps, then run `consumers` with the remaining units.
The fold tool needs the template layout (`go-trader@<instance>`, `/opt/go-trader-<instance>`, `/var/lib/go-trader/<instance>/config.json`).
`plan` prints a fold hint when it finds more than one paper unit.
Test hooks: `SHARED_FEED_CONVERT_FAIL_AFTER=feeds-reload|restart|audit` forces the switch restore path, `consumers-primary|consumers-backup` sends the stage SIGTERM after that feed loaded the new list, and `SHARED_FEED_CONVERT_AUDIT_KEYS` sets the key count.

**Measured values.** A 45-minute websocket capture on 2026-09-29 (BTC, ETH, SOL, HYPE, DOGE at 1m, 5m and 15m; 269 closed bars) saw the last update of a closed bar arrive at most 3.9s after the bar's close (99th percentile 2.3s), and 41 bars changed after their close time; the 5s settle delay covers that sample, and a longer sample on the service host should confirm it.
REST reads from the development host took 45 to 230 ms per call (a 500-bar candle snapshot, a three-bar recovery window, one funding page, all mids), well inside the 20s preparation budget.
The decision-age proof: a seal is stored by `deadline + 27s` at the latest, and the entry limit is at least 60s from the deadline, so the check dispatch and the rest of the cycle get at least 33s.
A three-key union (ETH 1m, BTC 1m, SOL 5m at 200 bars) sealed at 62,803 to 65,817 bytes, about 100 bytes per bar, so the 64 MiB cap holds roughly 600,000 bars; the feed also refuses a union whose conservative estimate (160 bytes per bar) exceeds the cap.
With three seals retained per cadence, memory is at most `3 × cadences × seal size`.
Every production consumer's cadences, bytes per seal and the rehearsal on the service host are still to be recorded before cutover.
The backup's `request_budget` has no measured production value yet: the request baseline of the production consumers is measured on the service host with `scripts/hl-request-ledger.py` during the host rehearsal, and the budget is set from it before the backup serves production consumers.

---

## Operator-Required Circuit Breakers

Some venues have no safe automated close path:

| Platform | Type | Pending key |
| --- | --- | --- |
| OKX | spot | `okx_spot` |
| Robinhood | options | `robinhood_options` |

When one fires and the strategy's own book holds a positive leg (the OKX spot position, or each Robinhood option position), the scheduler records `operator_required: true` under the pending key before it closes the virtual book. A fire with a flat own book adds no leg and logs one `[WARN] operator-required-close:` line; an earlier entry keeps its legs, episode and reminder schedule. A later contributing fire merges into the earlier entry that you did not clear: the union of symbols, the sum of the sizes for a symbol in both (each fire closes the virtual book, so a later own-book leg is new exposure on top of the earlier one), sorted by symbol, a new random `episode`, a new `fired_at`, and a reset reminder stamp, even when the legs are the same. Every size is the virtual book at fire time, summed across the fires not cleared. It does not show the current venue position.
The `robinhood_options` row applies only to a pending close saved before #1823, because an options strategy with a live mode no longer loads; such a saved close still loads, renders and clears through the same paths.

Cadence (#1824):

- At the fire, the per-strategy circuit-breaker alert posts once to the channels and the owner DM and lists the legs as "manual flatten required".
- The operator-required drain runs before `CheckRisk`, so its first warning follows on a later cycle with due work. It then repeats once per `alert_throttle_interval` (default 6h), and at once after a new contributing fire. The `[CRITICAL] operator-required-close:` log line repeats on every drain, due or not.
- The drain stamps `last_notified_at` only after a channel post or the owner DM was accepted, and saves it at once. With no notification backend, or when every destination fails, nothing is stamped and the next drain retries. A failed save keeps the stamp in memory and the cycle save retries it; a restart before that save can repeat one warning. A clock that moves backward makes the entry due.
- The stamp, `episode` and `fired_at` live inside `risk_pending_circuit_closes_json`, so a restart keeps the schedule.

```bash
curl -s localhost:8099/status | uv run --no-sync python -c "
import json, sys
d = json.load(sys.stdin)
for sid, s in d['strategies'].items():
    pc = s['risk_state'].get('pending_circuit_closes') or {}
    for platform, p in pc.items():
        if p.get('operator_required'):
            legs = ', '.join(f\"{x['symbol']} size={x['size']}\" for x in p['symbols'])
            print(f\"{sid} [{platform}]: {legs} episode={p.get('episode')} fired_at={p.get('fired_at')} last_notified_at={p.get('last_notified_at')}\")
"
```

Response: open the venue UI, flatten the listed positions, check the venue position, then send `/go-trader-clear-operator-close <strategy> [platform]` in a DM to the bot and reply `confirm`. The command does not read the venue. Only two paths clear an entry: this command, and a portfolio kill-switch fire, which hands the manual check to the kill-switch flow (the kill-switch plan flags OKX spot and Robinhood options and blocks an owner-less auto-reset). Circuit-breaker expiry, a restart and a flat book never clear it. The drain and the command read only the running state, so a `robinhood_options` entry stays shown, reminded and clearable even if the strategy config changes.

This is not the portfolio kill switch. Operator-required is per-strategy and affects only the strategy that breached drawdown.

---

## Operator paper close

`/go-trader-paper-close <strategy> [symbol]` (owner DM, `paper_close.go`) and the dashboard paper-close action (`POST /api/confirm` with `action: paper-close`, then `POST /api/strategies/<id>/paper-close`) share one core on `StatusServer`: `preparePaperClose` builds and shows the plan, and `applyPaperClose` books it.
Scope: a paper strategy (`portfolioScopeFor`) whose `partitionForStrategy` is `paper` or `paper:<id>`; a live, unmapped or quote-owned strategy refuses. Live closes keep `manual-close` and `force-close`. There is no CLI writer, because a CLI process beside the daemon would be overwritten by the daemon's next full-file save.
Marks come from the platform's own source, never the merged `fetchLiveMarkPrices` map: Hyperliquid perps, manual and hedge legs use `allMids`; OKX perps the swap ticker; OKX spot the spot ticker (`-USDT`, then `-USD`, then `-USDC`); Robinhood spot the BinanceUS `<COIN>/USD` ticker; other `/` spot symbols `FetchPrices`; futures `FetchFuturesMarks`, with its mode in the reply. Any other platform and type refuses. A missing, zero or non-finite mark refuses; `AvgCost` is never used.
Option legs at or past the expiry cutoff stay open and are listed. Every other leg must pass the harvest fresh-mark gate (`optionHarvestEligibility`) and books at its stored `CurrentValueUSD`; any failure refuses the whole command.
Apply order: the per-file manual-action lock, a plan compare under `mu.RLock` (symbols, sides, quantities, average costs, position ids, kept legs), the queued manual action and resting limit row checks, the mark fetch with no `mu`, then `mu.Lock`, a second compare, the closes and `SaveStrategyBook`. A plan that changed during the confirm refuses with no state change.
Closes run through `closeVirtualPositionAtPrice` (inside `mutatePaperPerpsBook`) and `closeOptionLegAtValue` with reason `operator_paper_close`; the kill switch and circuit breaker keep `circuit_breaker`. Each close is a `FeeSourceReconcileAdjustment` trade with gross PnL and goes through `RecordTradeResult` (or `RecordHedgeTradeResult` for a hedge leg), so `DailyPnL` and `ConsecutiveLosses` move and a losing close can trip the per-strategy breaker.
A `symbol` that names a hedge leg, or a primary with a coupled hedge leg, refuses; omit `symbol` to close both. The reply warns when the strategy is not paused (its next signal can reopen) and when it is a replay mirror. A failed save leaves the close in memory, and the next cycle save retries.

## Cash Reconcile Latch

A live spot fill that overshoots virtual cash is always booked, because the venue already filled it. The strategy then latches `CashReconcileRequired`, raises a CRITICAL alert, and blocks further live buys. Closes keep running. Clear it with `/go-trader-clear-cash-reconcile <strategy>` only after the books match the venue — the command drops the buy block and never invents or adjusts cash.

A live Robinhood buy limits its order to the budget before fees (#1910), so a buy latches only when a venue fee, a fill above the request, or another venue result pushes the debit more than $0.01 above the cash. The scheduler logs a CRITICAL line when the echoed limit notional exceeds the budget, and still books the confirmed fill.

---

## Implementation Patterns

Full coding constraints live in [CLAUDE.md](CLAUDE.md) § Patterns. Notes that bite most often:

- A new trade-recording path must populate `Trade.PositionID`, or rely on the recorder's lookup against the strategy's positions, so partial closes collapse into one round trip.
- A new summary-posting path must thread the last-post map and call the shared cadence helper.
- Category-summary row labels use a fixed-width label helper; assert the exact text in tests.
- A new side-effecting subprocess wrapper goes through the side-effect runner, never the plain runner.
- Hedge PnL is recorded through the hedge recorder, never the ordinary trade recorder.
- A new per-strategy flag needs the `StrategyConfig` field, the `run*Check` CLI argument, the Python parse in the check script, and `InitOptions`/wizard support. A runtime-required flag also goes into both probe argvs. A request carried on `--strategy-refs` (such as `open_side`) instead gets a probe that sends each value under both probe argv shapes and one that proves the script rejects a bad value.

Audits:

```bash
grep -n "mu\.\(R\)\?Lock\(\)\|mu\.\(R\)\?Unlock\(\)" scheduler/main.go
grep -n "liveExecFailed" scheduler/main.go
```

---

## Subsystem Mechanism Reference

Per-subsystem mechanism notes for coding agents. `CLAUDE.md` keeps only the guardrail for each file; the mechanism behind it lives here. Grouped by `scheduler/` file.

### Execution and fill confirmation (`executor.go`, `shutdown.go`)

- `runPythonSideEffect` is the runner for every subprocess that can place, cancel, or modify an order; `runPython` is read-only. Graceful shutdown drains side-effecting subprocesses for at most `shutdownDrainCap=15s` and then SIGKILLs; state save, notifier flush, and DB close run afterwards through deferred LIFO. `TimeoutStopSec=20` in the units.
- `confirmHyperliquidExecuteFill` covers open, close, scale-in, hedge, manual, and UI paths. A response without a confirmed fill returns `"exchange returned no confirmed fill"` and books nothing. `hyperliquidExecuteSucceededCancelOIDs` retains the confirmed cancel OIDs so protection reconciliation can tell a cancelled trigger from a lost one.
- Bidirectional perps: `bidirectionalPerpsStrategies` mirrors the open registry `short_entries=True` set (`short_entry_strategies()`); `scripts/test_go_python_registry_parity.py` fails on a name missing from either side. Flip sizing uses `perpsLiveOrderSize`.

### Shared OKX step (`okx_step.go`, #1870)

- `runOKXStep(c okxStepContext, sc, check okxCheckFunc) okxStepResult` is the only OKX gate sequence. The main loop calls it from the OKX spot branch and the OKX perps branch, both with `okxOrdinaryCheck`. Direction comes from `sc.Type`: spot is long-only (`true, false`), perps uses `PerpsAllowsLong`/`PerpsAllowsShort`. Any other type, another platform, a nil check, a missing configuration, state, store, database, lock, price map or partition risk refuses the step before the check runs; there is no long-only fallback.
- The caller keeps everything before dispatch: the read-locked capture of position, side, quantity, average cost, leverage, cash, pool-balance availability and the cash-reconcile flag (before `CheckRisk`), `CheckRisk` and its notifications, cycle-level entry recovery, and the persistence-hold read. The caller also keeps everything after: channel totals, `sendTradeAlerts`, evaluation marks and the cycle save. The step returns trades and detail.
- Inside the step the order is unchanged from the two former branches: check, price publication, regime payload, closed-bar decision log, regime gate, Hurst evaluation, closed-bar hold, higher-timeframe hold, Hurst hold, pause, daily loss, notional, persistence and exposure holds, closed-bar admission with `CloseReduceOnly`, locked regime sync, live intent reservation and submit gate, `runOKXExecuteOrder` with `liveExecFailed` and `finishFailedLiveEntry`, locked `beginEntryBookingLocked`/`executeOKXResult`/`finishEntryBookingLocked`, then the spot cash alert and the locked cash-reconcile reminder. Checks and order subprocesses run outside `mu`.
- Check contract: `func(sc, posCtx, logger) (res, signalLabel, price, ok)`. `okxOrdinaryCheck` wraps `runOKXCheck`, which ends in `completeOKXCheck`; it owns script errors and stderr, failure and recovery alerts, closed-bar and higher-timeframe contract checks, higher-timeframe observation, the signal label, a nil-result refusal and the price fallback (finite positive price only). The research fallback map holds only the frozen mark, so a missing price never fetches from the exchange. A failed check returns before price publication, observation, send or booking.
- Clock: admission, live intent reservation and the cash-reminder stamp read `tradingNow()`, as the Hyperliquid step does, so a host that binds the trading clock moves admission and paper settlement together; there is no separate clock field. The live submit deadline in `okxEntrySubmitGateFor` and live recovery keep system time.
- Proof: none. The OKX step goldens and the `TestOKXSharedStep*` tests were removed on owner instruction because the owner deployment does not use OKX; `okx_step.go` has no automated test.

### Loopback UI and tuning (`server.go`, `ui_*.go`, `static/ui/*`, `ui_tuning.go`)

- Paper close (§ Operator paper close): the confirm pins the plan as the nonce payload, and the apply refuses with 409 when the book differs. Position rows of a paper strategy show "Paper close", and the trade panel shows "Close all paper positions"; "Force close" shows only for live Hyperliquid perps. `removeStrategyGuarded` takes `tradeActionMu`, then `configWriteMu`, then `mu.RLock` inside; no path holds `mu` and then takes either, and no trade action takes `configWriteMu`. A successful removal records the ID in `removedStrategies`, which dashboard open and add check under `tradeActionMu`, so a removal and a dashboard open never interleave. The dashboard remove ignores `restart:false`.
- `applyStrategyConfigPatch` needs `config_version>=13`. `/tuning` (`static/ui/tuning.html`) is read-and-launch: it re-reads `/api/strategies/<id>/config` on every poll.
- `/api/tuning/runs` is a suggest-only lane. `tuning.max_retained_runs` prunes terminal run directories. `POST /api/tuning/apply` is refused on drift against the schema-v2 `promotion_baseline` and applies through `mutateConfigRoot` as an exact replace.
- **Partition navigation.** Every dashboard read takes an optional `partition` query key carrying the runtime's own partition text (`live`, `paper`, `paper:<source id>`).
  An absent key keeps every partition.
  `uiPartitionParam` (`ui_partition.go`) is the single resolver: unparseable text is 400, and a well-formed partition this process does not own is 404, so a stale selector never reads another deployment's view.
  `/status` carries a `partitions` roster (partition, label, scope, source) in the stable order live, default paper, then sources by id; an unlabelled source reads as its partition text, never the bare id.
  `/api/strategies`, `/api/strategies/overview`, `/api/leaderboard`, `/api/strategies/dead` and `/api/diagnostics` honour the key; `UIStrategy` and `UIStrategyOverview` carry `partition` and `paper_source`.
  Diagnostics filters before paging **and** before the total, through `TradeDiagnosticsRowsPageForPartition`, which resolves the owning file by `SourceRole` (never by scope) and counts only that partition's ids; a `strategy` query outside the selected partition returns an empty page.
  Cash flow stays live-owned (`live_owned: true`) and answers a paper partition with `available: false` and a reason instead of the live wallets under a paper label; the close-evaluator catalogue stays shared (`shared: true`).
  Correlation and portfolio risk are already keyed by partition, so the front end reads only the selected key: under a selection the untagged single-value `correlation` and `portfolio_risk` fields, which belong to the legacy scope, are never a fallback, and the panel reports no reading instead.
  `/status` names a strategy's `partition` only when the roster still configures it, so an orphan state row's circuit-breaker alert is shown under every selection rather than filed under a partition it may not belong to.
  The selector stays hidden while the process owns fewer than two partitions, resets the selection to every partition when it hides, falls back to every partition on any 404 of a partitioned read so sibling reads recover in the same cycle, drops a response whose selection is no longer current, and clears a selected strategy the new partition does not own.

### Config and close defaults (`config.go`, `config_migration.go`, `close_defaults.go`)

- Options paper-only gate (#1823): `validateConfig` appends `strategy[<id>]: ` plus `optionsLiveModeConfigError(<inferred platform>)` (`options_paper_only.go`) for `type: options` with `isLiveArgs`, in the per-strategy loop outside the `skipLiveCredentialChecks` block, so every loader refuses it; `paperToLiveBlockedReason` uses `optionsPaperOnlyReason`, and `check_options.py` ports `isLiveArgs` and emits the same text.
- v20 (#1681) removes `allow_deprecated` and stamps `allow_no_edge: true` only on explicitly live pre-v20 strategies whose whole effective reference set ran live before; `migrateConfigDataReport` returns the stamp list, the loader keeps the first applied report on `Config` (non-JSON), and `runConfigMigrationDM` delivers it when v20 changed legacy input (versionless included). `LoadConfigForProbe` migrates in memory like `LoadConfigReadOnly`.
- v19 renames the per-regime stop fields to `*_atr_mult_regime`; legacy-key presence gates the boot rewrite. The seven HL stop owners are mutually exclusive and all-omitted resolves to `DefaultStopLossATRMult=1.0`. A single `*StrategyRef` is accepted, `close_strategy` canonical.
- `portfolio_risk.paper` is an optional override block with the parent's fields. `scopeRiskConfig(cfg, scope)` merges non-zero override fields over a clone of the parent (zero = inherit). `paper.max_notional_usd` is restart-required like the parent; the other override fields hot-reload.
- Close defaults resolve system → user → strategy. A reserved `regime_atr` section serves standalone `*_atr_mult_regime` `use_defaults` owners. `user_defaults.close["trailing_tp_ratchet_regime"]` may carry `trailing_stop_atr_mult_regime`; `applyUserCloseDefaultRatchetRegimeTrails` applies it inside `loadConfig` before the scalar ATR-stop default so the scalar default never shadows it.

### Portfolio scope and state (`portfolio_scope.go`, `state.go`, `db.go`)

- `hyperliquidModeFromArgs` is a wrapper over `PortfolioScope`. `measureScopeCycleRisk` and `applyScopeCycleRisk` own the per-scope cycle read; `dueStrategiesNotLatched` filters the due set per scope.
- `portfolio_risk` is keyed by `scope` (rebuild migration `migratePortfolioRiskScopeColumns`); `kill_switch_events` and `correlation_snapshot` carry `scope`. Legacy unscoped rows load under `scopeUnassigned`; `placeLegacyPortfolioRisk` puts each file's row into the scope that file owns and the placement is saved at once, so it is idempotent. In a split layout with no live strategy the primary file's unscoped row is rejected instead of guessed.
- `StateStore` (`state_store.go`) owns the physical handles and the immutable identity map, routes every write to the owning file and combines every cross-scope read. `LoadStateWithStore` composes `loadProcessMeta` and `loadScopeBooks` per file, so paper books load with no process-metadata row in the primary. `SaveAll` runs one transaction per file and returns an error per scope; a failed file leaves its scope in memory for retry and cannot duplicate the other file's committed effects. Test hook `storeCommitHook` injects a per-file commit failure. § Storage Ownership.
- `LoadState` bounds per-strategy trades in SQL (`ORDER BY timestamp DESC, rowid DESC LIMIT maxTradeHistory`, index `idx_trades_strategy_timestamp`), separately for funding and non-funding rows, and merges the two windows in that order, so funding rows never push real trades out of the loaded history. Backward scans for the opening trade skip funding rows. `ValidatePerpsDirectionConfig` runs at startup; `CheckStatePresence` is bypassed with `GO_TRADER_ALLOW_MISSING_STATE=1`.

### Risk, latch, and the portfolio gates (`risk.go`, `strategy_interval.go`, `daily_loss.go`, `exposure_cap.go`, `notional_cap.go`)

- `collectPerpsMarkSymbols` feeds `type=manual` positions at live mids. A one-shot `PortfolioRisk.PeakValue` migration runs on first load.
- Latch ownership per cycle: equity drawdown when `equityGuardArmed`, else margin drawdown, with no tie-break. When equity owns the latch, margin drawdown over the limit is a throttled WARN. With `equityTrusted` false the latch stays equity-side, the peak ratchet is skipped, and drawdown is floored at the last reading (`DrawdownReadingSubstituted`). An untrusted over-limit reading defers until `untrustedEquityLatchDeferral` (15m), then latches loudly. Per-scope single-flight flags guard the reset prompt; the prompt names the scope and accepts `reset` for one latched scope, `reset live`/`reset paper` when both are latched. A paper latch calls `forceClosePaperScopePositions` (virtual close at mark, no exchange call), guarded once by `KillSwitchCloseApplied`. Per-position margin protection is the per-strategy circuit breaker (`circuit_breaker:false` opt-out). Full operator behavior: § Portfolio Kill Switch And Latch Ownership.
- Daily loss limit: `portfolio_risk.daily_max_loss_usd`/`daily_max_loss_pct`, 0 = off; both set → the lower resolved USD wins; the pct basis is the sum of strategy `initial_capital` per scope. The gate shape to copy for any new `portfolio_risk` gate: RLock evaluation per scope, `pausedBlocksSignal` holds, `manualStateView` refusals, `clonePortfolioRiskConfig` hot-reload.
- Exposure cap: `portfolio_risk.max_same_direction_notional_usd`/`max_asset_concentration_pct`, 0 = off; `exposureCapBlocksSignal` decides; TopStep futures are ungated. `computeAssetDeltas` in `correlation.go` is the one exposure model shared with `ComputeCorrelation`. Hot-reloadable via SIGHUP.
- Notional cap: `portfolio_risk.max_notional_usd`, 0 = off. `notionalCapSkipsStrategyCycle` is always false: closes, SL, and TP maintenance keep running; manual open, add, and limit-open refuse. Restart-required.
- Kill-switch fill attribution on shared HL coins goes through `hyperliquidKillSwitchFillShare`, which fails closed when the split cannot be determined. The reset prompt is single-flight via an `atomic.Bool`; the deferral timestamp is `UntrustedOverLimitSince`.

### Live-to-paper replay (`replay_log.go`, `replay_mirror.go`)

Enabled by `replay_log_path` plus per-strategy `replay_sharing="live_mirror"`. Paper suppresses its own entries and replays the live decisions. The source id comes from `replayMirrorSourceID` (`replay_source_id`, else the strategy's own id); `orderReplaySourcesBeforeMirrors` runs an in-process source before its mirror in the same cycle. The watermark is keyed on the paper strategy plus `ReplayMirrorWatermarkSource`; a source change resets it with a WARN. Book drift raises WARN plus DM; a close while flat is INFO.

### Batched HL checks and fills (`hl_batch.go`, `hyperliquid_fills.go`, `hyperliquid_balance.go`)

- Batching is a pure partition on `hlBatchKey` into one `check_hyperliquid.py --batch-check`; the fingerprint is re-checked at dispatch. Three strikes revert a group to per-strategy checks with a batch retry every 10 cycles. Operator view: § Hyperliquid Batched Signal Checks.

### Market feed (`market_feed*.go`, `hyperliquid_candles.go`, `hyperliquid_funding.go`)

- `marketFeedOwner` owns one websocket, its own `feedMu`, and a bar ring per `marketFeedKey{Host, Namespace, Symbol, Timeframe}` sized `required + 50`. It never takes the scheduler state lock; alerts leave through a channel the cycle drains outside `mu`. A race test and a source guard hold that line.
- `deriveFeedRequirements(cfg)` builds the key set from every in-scope consumer: signal frames, higher-timeframe filter frames (`hlFeedHTFMap` mirrors Python `_HTF_MAP` through `shared_tools/testdata/htf_map.json`), regime timeframe overrides, mid coins and funding needs. Each key stores the highest lookback any consumer asks for; `snapshot.frameFor(key, required)` slices it per consumer, so legacy row counts are preserved. `ApplyGeneration` bootstraps new keys and publishes a generation only once every required key is ready or has failed explicitly.
- Accounting funding (`market_feed_accounting.go`): `EnsureAccountingFunding` runs after `EnsureFunding` in both prepare paths, fetches outside `feedMu` through `hlFundingRecordsCoverage` (the pager rejects non-finite rates and conflicting duplicates), and swaps the window in under `feedMu`; `freezeAccountingFundingLocked` copies one entry per coin into the snapshot.
- `hlFetchCandleHistory` mirrors the Python adapter's widening loop, and `hlCandleRowFromRaw` mirrors its row conversion including the exchange close-timestamp preference; one golden fixture proves both converters agree.
- Closed-bar correction (`market_feed_correction.go`): `runCorrection` starts inside `Run`, so both websocket deployments share it. `correctionPass` picks due keys under `feedMu`; each read runs outside it and `mergeCorrectionRows` merges under it, touching closed bars only and never `LastRecvAt`. `feedRestBarDecision` orders REST against socket data by volume and `RecvAt`; `feedBar.RestSeenAt` records the newest confirming read and drives the checkpoints. Correction is in-memory, dies with the generation's key state, and restarts populate from the bootstrap read.
- `sealCycleMarketSnapshot` runs at most one REST recovery per stale key per cycle, then freezes a deep copy. Later socket updates belong to a later snapshot. The payload rides stdin: the batch envelope becomes `v:2` with a `market` object; individual and regime-bundle checks get `--market-stdin` and the same envelope. Go caps the payload at 8 MiB and treats a serialization or size failure as a degraded dispatch, never a private fetch.
- Shared feed: `main` peeks `role` before `LoadConfig` and hands a feed config to `runFeedRole` (`market_feed_role.go`), before any storage layout, lock or probe; `LoadConfig` refuses `role: feed` by name.
  `sealCycleMarketSnapshot` is split into `prepareMarketSnapshot` (recovery and funding, network) and `freezeMarketSnapshot` (under `feedMu`); the websocket path still freezes at its cycle-start time, the feed freezes at the real seal time.
  `feedSealer` (`market_feed_server.go`) owns the schedule, the per-cadence retention ring and the hard-deadline rule under its own mutex, runs network work outside it, and never touches scheduler `mu`; lock order is sealer `mu` then `feedMu`.
  `market_feed_wire.go` is the only encoder and decoder of seal bytes; `feedSealDoc.snapshot()` rebuilds the same `marketSnapshot` every payload builder already reads, so batch, individual, regime-bundle and manual checks need no shared-mode branch.
  `sharedFeedClient` (`market_feed_client.go`) returns a non-nil snapshot on every path.
  `TestSharedFeedParity` is the one socket-level Go test; process-level proof uses `scripts/feed-parity.sh`.
- Backup feed and failover: the sealer calls a `feedPrepareFunc`; `websocketFeedPrepare` wraps `prepareMarketSnapshot`, and `feedRESTSource.prepare` (`market_feed_rest_source.go`) refreshes due keys only (`feedKeyDueAt` over `feedRequirements.KeyCadences`), funding and mids, and marks a failed or refused due key `failed` or `budget_exhausted` through `markKeyUnready`, which `keyReadiness` reports as not ready.
  Its report sets `ReadyOnlyRefreshed`, so `sealOne` runs `markUnrefreshedKeysNotReady` on the frozen snapshot and every key not refreshed for that deadline seals `not_due` without changing owner state.
  `feedRequestLedger` (`market_feed_budget.go`) travels in the feed's root `context.Context`; `feedBudgetAcquire` sits at the top of `fetchHyperliquidCandleSnapshot`, `hlPostInfo` and `fetchHyperliquidMidsCtx`, so every widening pass and page is counted and gated, and a spent context returns its error before it takes a unit, and a context without a ledger (scheduler paths except standalone websocket) is unchanged.
  `fetchAndMerge` tags the context with its `feedRestReason`; `withFeedKeep` reserves requests for the mids read.
  The client bounds every endpoint but the last by the give-up time less `feedClientBackupReserve`, and tracks `servedBy` for the FAILOVER and PRIMARY RESTORED alerts.
  The backup changed neither the seal document, the wire header nor the `describe` schema, so a part 1 consumer read a backup seal; #1637 later moved the seal to v2.
  `TestSharedFeedFailover` is the one failover Go test; it also seals a REST key whose cadence does not divide the deadline and checks `not_due`.
- Open-interest observations (`market_feed_observations.go`, #1637): `feedObservationState` per `feedObservationKey{Host, Namespace, Coin, Kind}` under `feedMu`; `ingest` keeps one sample per right-edge minute bucket (same bucket replaces, Seq increments), rejects non-finite, negative and backwards receipt times, and trims to the window plus two minutes.
  `SetConnected` bumps `obsSession` on connect and opens a `disconnected` gap on disconnect (start = last sample, `DetectedMs` = disconnect time); the next accepted sample closes it.
  `freezeObservation` drops a gap detected after the cutoff; `dropUncollectableObservations` strips observation needs (and each strategy's `OpenInterest` flag) from a consumer on a non-websocket feed source and keeps it loaded with a `Notice`; `feedSealVersionFor` picks seal v2 only when a seal carries observations.
  `freezeMarketSnapshot` takes a separate cutoff (the seal key for the feed, the cycle time in-process) and `freezeObservation` copies only samples at or before it.
  `feedStrategyRequirement.observationNeeds` threads windows through `cycleRequirementsForDue`, `fullCycleRequirements`, `unionFeedRequirements` and `estimateFeedSealBytes`; `attachObservationPayloads` adds them to single and batch payloads and never fails a payload.
  `holdFor` ignores observations by design.
  The recorder (`market_feed_recorder.go`) is fed by non-blocking `offer` calls under `feedMu`; one goroutine writes, rotates, hashes and counts overflow drops, so file I/O never holds `feedMu`.
  `TestOpenInterestObservationPipeline` drives a local socket source through owner, recorder, sealer, Unix socket and client; `scripts/test_observation_replay.sh` adds the Python replay comparison outside the Go suite (run by the `shell-suites` CI job).
- Trade flow (`market_feed_trades.go`, #1638): `feedTradeFlowState` in `marketFeedOwner.tradeFlows` under `feedMu`, keyed like open interest with `Kind=trade_flow`; `applyObservationNeedsLocked` routes by kind. `IngestTradesFrame` / `IngestTradeSubscription` run from `handleSocketMessage` (an undecodable message counts as a malformed trades frame only when trade flows exist); `stopOnSilence` runs for each frame coin before its trades are ingested, so the frame that ends a silence never hides it; `advance` finalizes minutes and also detects silence, called after every frame, by `AdvanceTradeFlows` and by `freezeMarketSnapshot` (recorded as a `tick` only when it emits events). Every recorder event is emitted in state order, so `backtest/trade_flow_replay.py` reproduces it exactly; change both together.
  `TestTradeFlowObservationPipeline` drives owner, recorder, sealer, Unix socket and client through every outcome; `TestTradeFlowCapacityAtMaximumWindow` checks a 26 h four-coin seal and batch payload against `estimateFeedSealBytes` and the caps; `scripts/test_trade_flow_replay.sh` adds the Python replay and tamper refusals (`shell-suites` CI job).
- Volume profiles (`market_feed_profile.go`, #1665): `feedProfileState` hangs off `feedTradeFlowState.profile` under `feedMu`; `ingestTrade`, `advance`, `stopCoverage` and `trim` call its hooks (`addTrade`, `observeNewTrade`, `invalidateMinute`, `finalizeMinute`), and `backtest/volume_profile_replay.py` mirrors them on `trade_flow_replay._Flow`; change both together.
  The canonical artifact hash text (`feedProfileCanonicalText`, `volume_profile_inputs.canonical_text`) and the sealed sessions (`feedProfileSealSessions`, `sealed_sessions`) must stay identical in both languages.
  `TestVolumeProfileObservationPipeline` drives owner, recorder, sealer, Unix socket and client with a hand-calculated day; `TestVolumeProfileCapacityAtLimitsAndEntryOnlyIsolation` checks a 16-coin two-session seal and both payloads at the limits; `scripts/test_volume_profile_replay.sh` adds the Python replay comparison and tamper refusals (`shell-suites` CI job).
- Order-book depth (`market_feed_books.go`, #1635): `feedBookState` in `marketFeedOwner.books` under `feedMu`; `IngestBookFrame`, `IngestMalformedBookEnvelope` and `IngestBookSubscription` run from `handleSocketMessage`; one owner-wide `bookSeq` numbers frames and markers; `tickBooksLocked` runs from the per-session timer (`feedBookTickEvery`) and every freeze. Every recorder record is emitted in state order (markers after their frame, epoch and depth-raise markers before their `book_need`, disconnect markers before the `conn` record), so `backtest/book_replay.py` reproduces outcomes, sequences and markers exactly; change both together.
  `TestL2BookObservationPipeline` drives owner, recorder, sealer, Unix socket and client through every outcome and marker; `TestL2BookRequirementsSurviveEveryUnionAndRefuseConflicts` covers derivation, unions and admission; `TestL2BookCapacityAtTheJointBoundsAndIsolatedOverflow` checks a 16-key maximum seal and the single payload at the bounds; `TestL2BookMixedRecordingKeepsEveryImporter` records open interest, trades and books in one v3 run; `scripts/test_book_replay.sh` adds the Python replay, mixed-run imports and tamper refusals (`shell-suites` CI job).
- Premium samples (`market_feed_premium.go`, #1639): `marketFeedOwner.premiums` keyed like other observations; `record-observations --kinds perp_premium` writes v3 run/segment/recording manifests (every raw `bbo`/`activeAssetCtx` frame with its outcome, each finalized `psample`, each `pinvalidate`, finalizing `ptick` records); `backtest/premium_replay.py` (behind `observation_replay.py import-premium`/`harness-premium`) reruns the state machine from the raw frames and refuses a run whose outcomes or events differ. Change the Go owner and the Python replay together.
  `TestPremiumObservationPipeline` drives owner, recorder, sealer, Unix socket and client through every outcome; `TestPremiumCapacityAtMaximumWindow` checks a 26 h four-coin seal and batch payload; `TestV3RecordingSchemaTypesEachKind` records premium-only, book-only and mixed v3 runs with the real owner and recorder; `TestSealWithBooksAndPremiumsIsV8AndPayloadV4CarriesBoth` seals a book and a premium need together (v8 seal, payload v4 with both sections); `scripts/test_premium_replay.sh` adds the Python replay, decisions, tamper refusals, every importer on those three runs and on the frozen #1639 recording, and kind/contract mismatch refusals (`shell-suites` CI job).
- Macro-event inputs (`market_feed_macro.go`, #1654): `marketFeedOwner.macro` holds prepared immutable artifacts and `macroNeeds` the installed needs, both replaced only in `publishGeneration`; `freezeMacroLocked` copies under `feedMu` with a receipt-sorted index. Record hashes are sha256 of canonical JSON field arrays, identical in Go (`SetEscapeHTML(false)`) and Python; change both readers together. `TestMacroEventFeedPipeline` drives derivation, owner, sealer, Unix socket, client, strict decode, holds and reload refusals; `scripts/test_macro_event_replay.sh` feeds Python-imported artifacts through it and compares websocket, shared and offline decisions (`backtest/macro_event_replay.py`, `shell-suites` CI job).
- The fill resolver is built outside `mu.Lock`; a resolver failure falls back to the modeled fee. Reconcile paths treat unconfirmed SL fills as gaps, never as books.
- `reconcileHyperliquidAccountPositions` sends public trade alerts for reconciliation closes via `sendTradeAlertRows` in the deferred unlock path. `hyperliquidPublicTradeAlertRows` drops hedge-leg rows so a hedge close alerts only its owner DM. A sole-owner reconciled SL close also queues a `ProtectionFillAlert`.
- Filled stops: `applyHyperliquidProtectionSync` keeps `StopLossOID` and `StopLossTriggerPx` on a stop the sync reports filled, so the reconcile attributes the fill as `stop_loss`. `reconcileSoleOwnerOpenStopFill` books a confirmed sole-owner stop fill that leaves the chain open as a `stop_loss` partial close and clears the stop for the next sync. An unconfirmed fill at resync starts an in-memory `hlStopFillWatch` retried for `hlStopFillLookupRetryCycles` (3) cycles; expiry sends one `HL STOP FILL UNCONFIRMED` owner DM, and a restart drops the watch.
- Hyperliquid perps step alerts (`hyperliquid_step_trade_alerts.go`): each strategy step captures a trade-history baseline under `mu.RLock` after `runHyperliquidCheck` and before the paper breach check.
  Every producer binds its summary line to the exact history rows it booked (`bindWindow`/`bindWindowLocked`, and `bindExecuteLocked` for the execute leg, which labels the open row).
  After replay and hedge synchronization, `hlStepTradeAlerts.finish` copies the new rows under `mu.RLock`, drops hedge-leg rows through `hyperliquidPublicTradeAlertRow`, counts each public row once, keeps one summary line per row (a bound label, else a line from the row's side, symbol, quantity and price), and sends them through `sendTradeAlertRows` outside `mu`.
  A stop that fills at placement, a replayed row and a primary unwind after a failed hedge open therefore alert and count even when the execute books nothing.
  A baseline above the history length is an invariant failure that logs an error and selects no rows; there is no tail-count fallback.
  Execute-owned gates (ratchet scale-in ownership, post-trade protection sync, paper post-execute post-take-profit) read `execTrades`, never the step total.
  The per-strategy tail-count consumer `sendTradeAlerts` runs only when no step helper exists (OKX spot and perps through `runOKXStep`, other spot, options, futures). `runOKXStep` returns trades and detail and never sends trade alerts; the OKX branches leave `hlStep` nil, so the tail sends each OKX trade alert once.
  Indices stay valid because `RecordTrade` only appends and every history writer during a step runs on the cycle goroutine.
  The manual case opens the same step window before the protection sync, binds each producer (protection sync, post-take-profit stop, manual trailing stop, close recovery) to its rows, and finishes once after the strategy-type switch through `finishLines`, because the manual case leaves early through `break`; each public row is one alert, one count and one summary line.
  `settleManualCycleClose` queues the close without a count or line, so a queued close reports nothing at queue time.
  `drainPendingManualActions` copies the public rows each applied action appended (`drainedPublicRows`, history index taken before the apply under `mu.Lock`) and returns them in `manualAlert.rows`; the loop sends them through `sendTradeAlertRows` after `mu.Unlock` and keeps them in `pendingDrainReports` until the next iteration that reaches the summary block (the empty-due branch skips it).
  `foldManualDrainTrades` then adds one count and one summary line per row to the total, channel and asset maps, and `summaryChannelActive` lets a channel with drained rows pass the activity gate with no due strategy.
  The rows count as trades, so `ShouldPostSummary` returns true before it reads `summary_frequency` and the channel posts on that pass, as it does for a due strategy that trades.
  A duplicate close, stop edit, stop cancellation, failed action and hedge-leg row add nothing.
  The pending-limit-order consumer keeps the tail count (`manualAlert.trades` only).
  The carry-over is in memory, so a restart drops only the pending summary contribution; delivery is not exactly-once across crashes.

### Regime readiness (issue 1939)

A snapshot carries boolean `ready` and integer `bars` and `need` beside `regime`, `score`, and `metrics`.
Average Directional Index (ADX) needs `2 * period + 1` bars.
Composite needs `max(period, 2 * min(period, 14) + 1)` bars.
Each configured window and comparison view has its own need.
Before that need, its label is empty and its display is `warming (bars/need)`.
Missing or invalid metadata is unavailable; a readable bundle retains valid siblings.
A nonempty short frame is a successful bundle, so normal warm-up does not send a script-failure direct message.
Fetch depth covers both comparison views of every window plus the existing margin and base floor.

Admission checks the gate, effective regime stop, profile, and directional dependencies before position increases.
A known warming dependency holds opens, the new side of a flip, and same-side adds under both entry failure policies. A held flip on a position becomes a full close with no opposite open, as the backtester closes on a reversal and holds only the new entry.
A store outage retains the configured gate failure policy; a regime stop independently requires usable current inputs.
Manual opens and adds check before order execution and remainder cancellation, including dry-run.
Manual limit orders also require ready stop inputs before placement or dry-run. Their pending rows retain the snapshot for later fill adoption; reconciliation still books fills during readiness loss.
The manual market-order ratchet fallback applies only to genuine classification failure; readable invalid readiness holds the order. Limit placement requires usable stop inputs.
Record-only adoption can still record existing exposure.
A position on a regime-stop strategy whose entry evidence has no stop label (record-only adoption, the manual fallback, or a row written before the migration) takes the current stop-window snapshot on the first cycle where that window is ready, including after a restart. A captured stop label is never replaced.
Sealed feed ownership forbids a private candle fetch for a manual command without its sealed frame.
Under `market_feed` `shared` or `websocket`, only the running scheduler holds a fresh sealed regime bundle. The status UI open and add actions read it in process. Command-line `manual-open` (market and `--limit-price`) and `manual-add` read it through the scheduler (issue 1945, below).

### Command-line regime admission under a sealed feed (issue 1945)

`GET /api/regime/manual?strategy=<id>&partition=<live|paper|paper:<source>>` serves one manual strategy's current bundle to a command process. It authenticates with the owner marker secret and never with the status bearer token: the command sends a fresh 32-byte challenge and an HMAC-SHA256 of the challenge, strategy and partition keyed by the marker `secret`, and the scheduler signs every reply (status and exact body bytes) with the same key and challenge in `X-Go-Trader-Manual-Proof`. A request without a valid MAC gets an unsigned 401. The route keeps the draining check, reads configuration under `mu` then `strategiesMu`, copies the store entry under the store lock, and writes with no lock held. `/api/regime` is unchanged.

- **Envelope (protocol 1):** `result`, `owner_nonce`, `strategy_id`, `partition`, `storage_role`, `storage_id`, `contract_fingerprint`, full `key` (platform, symbol, timeframe, window spec), and for a bundle also `generation`, `interval_seconds`, the original `bundle_at` and the normalized `payload`. HTTP 200 = `bundle`; 503 = `unavailable`, `classification_failure`, `draining` or `internal_refusal`; 400 `invalid_request` (bad parameters, an unparseable partition or a method other than GET); 401 owner authentication; 404 `unknown_strategy` (unowned strategy or partition); 409 `config_mismatch`. Every reply to a request with a valid MAC is a signed envelope, and the command reports its result.
- **Owner identity:** after the daemon holds every state-file ownership lock, the layout passes inspection and the store opens, it writes a 32-byte random nonce and a separate 32-byte random secret into `<canonical db path>.manual-regime-owner.json` (mode 0600, temp file and atomic rename) beside each state file. The secret never leaves the file and is never sent; only a process that can read the marker can authenticate to the route or verify a reply. The marker holds no bundle, path or trading authority. Publication refuses a symlinked, irregular or malformed existing marker and rolls back its own markers; a failed publication disables only this route. Shutdown disables the route and removes markers still carrying this nonce before the ownership locks are released. A crash leaves the bytes; the next owner replaces them. Stop the owning process before deleting a marker by hand. The existing lock files and their pid format are unchanged.
- **Generation binding:** each cycle captures a SHA-256 contract fingerprint per due manual strategy (strategy and storage identity, partition, full bundle request, admission and protection settings, regime settings, ATR method, ratchet fallback multiplier, feed mode, interval) when the store generation starts. A result exports only while the current configuration produces the same fingerprint, so a reload inside a generation answers 409 until the next cycle. Startup, a pending or failed population and a restart answer `unavailable`; a classifier error answers `classification_failure`. No bundle is persisted.
- **Carry between cycles:** a manual strategy that is not due in a cycle keeps the contract, generation and bundle (or failure) of its last due cycle in memory until its next due cycle. The fingerprint check still applies, so a reload between its cycles answers 409 until it runs again. A save-blocked cycle drops every carried bundle. The status UI open and add read the same carried bundle in process.
- **Command client:** `GO_TRADER_MANUAL_SCHEDULER_URL` (default `http://localhost:<status_port or 8099>`) must be a bare `http` origin on a literal loopback address or `localhost` with a port. Set it for a `--status-port` override, a fallback listener port or a container port mapping; nearby ports are never scanned. `localhost` resolves to loopback addresses only and the dialer is pinned to them; no environment proxy, no redirects, 1 s connect, 3 s total, 1 MiB body, strict JSON with no trailing data.
  The command checks that a running process holds every configured state-file lock and that all markers carry one nonce and secret, sends no status token, refuses any reply without a valid proof for its own challenge before reading it, compares the reply's nonce, identity, key and fingerprint with its own configuration, re-reads the markers after the reply, and requires `0 <= age <= max(1, cadence)` seconds, where the cadence is the strategy's own `interval_seconds`, else the global one (the status UI path uses the same limit). It re-checks the markers and age after sizing and the manual-action lock, before the first order or remainder cancellation and before a dry-run prints.

| Result | Market open and add | Limit open |
|---|---|---|
| Fresh matching bundle, required stop window ready | Existing order guards | Existing order guards |
| Readable warming or invalid required stop window | Hold (`warming (bars/need)` or unavailable) | Hold |
| Owner authentication (401) or reply proof, owner, strategy, partition, storage, key or configuration mismatch; malformed, oversized, redirected or unsupported reply; missing, future or stale timestamp; draining; unreadable, malformed, symlinked, inconsistent or replaced marker | Refuse, no ratchet fallback | Refuse |
| No process owns the state files, owner not yet published, connection refused, bounded timeout, ownership-checked `unavailable` or `classification_failure` | Existing manual ratchet fallback where eligible, else refuse | Refuse |

**ATR prerequisite.** Under a sealed feed a new command-line open whose protection needs ATR (scalar or ratchet trail, tiered TP, regime stop) needs a finite, positive `--atr` before any order or cancellation, dry-run included, and never fetches candles privately. A market open checks it against the sizing mark when `--notional` or `--margin` produced one; a limit open checks it against the limit price. The value is stored in the queued open or pending limit row. If the confirmed fill price makes the supplied ATR exceed half the fill, protection uses the arithmetic fallback `0.1 * fill / leverage` with a warning that names both values; with no plausible fallback the fill is still queued and a CRITICAL alert asks for a stop. A tracked limit fill whose row ATR is positive uses it with no fetch and the same recovery; older rows with no ATR keep the earlier recovery path.
Under `market_feed: rest`, a tracked limit fill whose row ATR exceeds half the fill discards it and fetches a new ATR (fallback ATR on a failed fetch), as a REST market open does. Record-only adoption needs no ATR.

Example: `GO_TRADER_MANUAL_SCHEDULER_URL=http://127.0.0.1:8100 go-trader manual-open hl-manual-btc --margin 50 --atr 412.5`.

Closes, reductions, stops, trailing stops, ratchets, recovery, and existing protection continue.
Current state is replaced on each result, including all-warming results.
Entry snapshots remain fixed through adds and restart, except the stop-label backfill above; positions, queued manual actions, and pending manual limit orders store optional snapshot JSON through additive migrations.
An explicitly empty named window cannot use the primary label.
Applied dynamic protection retains its recorded label and clears pending consecutive confirmation when current readiness is lost.
Only supported labels enter transition history, alerts, and confirmation.
The configured longest window remains the reversal reference even while it is unavailable.

Closed-bar gates use the selected decision snapshot; stop admission uses the current protection snapshot.
Keep primary frame and candle-closure checks.
Historical preparation carries typed readiness with each label, including the higher-timeframe shift before forward fill.
A fetch limit caps trailing history; a shorter available prefix can become ready at its classifier need.
Every simulated increase checks current readiness, including adds and flips after readiness loss.
Stop warm-up skips remain in the result, and seeded positions retain their safety checks.
Parity and ledger comparisons include readiness and bars/need; trimming never fills cold evidence with another frame's label.
Research excludes cold labels and undefined features and retains gaps when it measures transitions.
Ship Go and Python from one commit with `bash scripts/update.sh --restart`.

### Pause, regime, ratchet, hedge, Hurst (`pause.go`, `regime*.go`, `post_tp_sl.go`, `trailing_tp_ratchet.go`, `hedge.go`, `hurst_gate.go`)

- `StrategyConfig.Paused` (`"paused"`) runs a full manage-only cycle and hot-reloads at any time, including while open.
- Regime store failure displays `regime=-`; the entry empty-label policy comes from `resolveRegimeGateOnFailure` (`"open"`|`"closed"`). `regime_atr.go` treats the v15 `atr_multiple` as canonical. The regime label is display-only; regime transitions are alerting-only.
- The ratchet sets SL through `trailing_stop_atr_mult`/`trailing_stop_atr_mult_regime`. A same-cycle tier tighten replaces the resting SL and bypasses `TrailingStopMinMovePct`. The open DM shows the ratchet or trail block; it is suppressed on scale-in and on a non-default `regime_atr_window`.
- Hedge legs run through one reconciler (`hedgeTargetDecision` then `runHedgeSync`); the collision matrix between hedge and owner positions is load-bearing.
- Hurst gate: sits on top of the label gate; `resolveHurstGateOnFailure` fails closed flat-only. Size multiplier `clamp(|H-0.5|/0.15, floor, 1.0)`; hysteresis lives in `strategies.hurst_gate_state`. Hot-reloadable while open. Backtest counterpart `backtest/hurst_gate.py`.

### Higher-timeframe filter (`htf_filter.go`, `shared_tools/htf_filter.py`)

- `htf_trend_filter` returns `htf_status`/`htf_reason`. `ok` needs at least 50 close values (the EMA period), every close finite and positive, and a finite EMA and final close; a missing frame, fewer rows, a fetch or calculation exception, or unusable numbers give `unavailable` with the cause (row totals, or the exception). The default map, the EMA and the neutral pass-through are unchanged.
- Every successful check with `--htf-filter` emits one top-level `htf_filter` block `{status, timeframe, trend, reason}`; Hyperliquid emits it from `evaluate_signal_slot`, so single checks and batch slots match. `ok` applies the filter (trend 0 passes the signal). `unavailable` skips `apply_htf_filter`, keeps the open signal for Go to gate, still composes closes, and prints one `HTF filter unavailable for <symbol> <htf>: <reason>` stderr line. `skipped` marks the script's own exclusions (`delta_neutral_funding`; Hyperliquid also `funding_skew`). `not_evaluated` marks a closed-bar decision held before the filter ran, or TopStep's closed-market return. A closed-bar HTF fetch or selection failure keeps the existing decision hold and reports `unavailable`. Error payloads and a disabled filter carry no block.
- Go (`StrategyDecisionFields.HTFFilter`) validates the block in `runSpotCheck`, `finishHyperliquidCheck`, `runTopStepCheck`, `runRobinhoodCheck` and `runOKXCheck` after the error and closed-bar checks: with the filter on, a missing block, a missing field, an unknown status, a trend outside -1..1 or a non-`ok` block without a reason is a script error; with it off, a block is a script error. `not_evaluated` is accepted only with a held closed-bar decision that names its reason, or a TopStep result whose `market_open` is present and `false` with signal 0 and no close (`TopStepResult.MarketOpen` is `*bool`, so an omitted flag proves nothing). Go-created degraded Hyperliquid results skip the check and keep their protection path.
- `htfFilterHoldsSignal` runs beside the closed-bar hold at all six gate sites (Hyperliquid perps step, OKX spot and OKX perps through `runOKXStep`, Robinhood, Binance.US spot, TopStep): `unavailable`, `not_evaluated` or no block zero the signal only when `pausedBlocksSignal` blocks it, log `HTF filter: <signal> signal suppressed`, and never end the strategy cycle.
- Alerts: `observeHTFFilterResult` runs once per consumed result (the batch pre-pass only stores results). Separate in-memory trackers keyed by strategy ID reuse `recordScriptFailureAtThreshold`: `unavailable` advances (3 evaluations; 15 evaluations or 75 minutes when `scriptFailureErrorIsTransient` matches the reason; repeat on a changed reason, every 10th evaluation or the throttle interval); `ok` clears and sends one recovery after an alert; `skipped` clears silently; `not_evaluated`, degraded results and script errors leave it unchanged. Ordinary script success never clears it. Alerts go through `SendToPartitionChannels(partitionFor(sc))` and `SendOwnerDM` with the partition prefix; there is no config field or migration, and a restart starts empty.
- Offline: `backtest/run_backtest.py::_htf_trend_series` raises `HTFCacheError` for an empty or shorter-than-50 cache, and for a cache whose first bar opens more than one HTF interval after the first lower-timeframe bar or whose last bar opens more than one HTF interval before the last lower-timeframe bar (the message names both ranges; single mode exits 1; compare and multi record `HTF FILTER REFUSED` and exit 1), so no filtered result is saved; `shared_scripts/simulate_strategy.py` returns the error.
  Before it refuses a non-empty cache, `_top_up_htf_cache` extends it once and checks again: backward in 500-bar chunks that end at the first cached bar (down to 50 HTF intervals before the first lower-timeframe bar) for a short cache or a leading gap, and forward one page at a time from the last cached bar for a trailing gap.
  Each chunk or page is stored as soon as it arrives and always touches the cached rows, so a stopped or failed top-up keeps its progress, never leaves a hole, and the next run continues from the new edge.
  The `/tuning` simulator gives the top-up a `SIMULATOR_HTF_TOP_UP_BUDGET_S` (15 s) budget, checked before each chunk after the first, so it stays inside the 30 s script limit; the CLI has no budget.
  Cache and fetch output goes to stderr so the simulator's stdout stays one JSON line, and a refusal names the fetch, its outcome (budget stop, fetch error, or no more rows) and says to rerun.
  The `shift(1)` before the forward fill and the lower-timeframe warm-up (bars before the first closed HTF bar read trend 0) are unchanged; the HTF index is aligned to the lower-timeframe index's time zone.
- Tests: `TestHTFFilterIntegration*` (`pyintegration`) runs the real binary `--once` on five paper platforms, the real consumers, the Hyperliquid feed single and batch paths and the alert tracker; `shared_scripts/testdata/htf_offline` supplies controlled ccxt, yfinance and TopStep market-hours inputs, logs yfinance requests (`yahoo_log`) and blocks the network. The Robinhood cases use a `30m` primary (`BTC-USD|30m`) and build the `4h` HTF from hourly `BTC-USD|1h` rows (#1822), so a non-finite HTF there reports the swallowed `no 4h candle frame` reason; `shared_scripts/test_robinhood_timeframes.py` covers the Robinhood mapping, ranges, resampling and paper closed-bar path.

### LLM review, scale-in, manual (`llm_entry_analysis.go`, `llm_review.py`, `scale_in.go`, `manual*.go`)

- The LLM verdict is advisory; it is written only to `trade_diagnostics.llm_verdict`.
- `llm_review.py` raises on any `stop_reason` other than `end_turn` (`refusal` names the `stop_details` category) and parses the judge with strict `json.loads` against `JUDGE_SCHEMA`; it never guesses a verdict from free text.
- Token usage: `llm_review.py` sums `usage.input_tokens`, `usage.output_tokens` and a `calls` count (API replies received, refusals and cut-offs included) over every call of one analysis. It prints the totals as `usage` in its stdout JSON on success and beside `error` on failure (still exit 1), and writes the running total to stderr as `llm_review_usage {...}` after each reply, so a run killed at its timeout still shows the usage of its finished replies. The worker logs one line per job: `verdict <v>; usage calls=N input_tokens=N output_tokens=N`, or `analysis failed: <err>; usage ...` (stdout totals, else the last stderr line, else `usage unavailable`). Usage never decides success: a malformed `usage` value is dropped and the verdict stands.
- Scale-in freezes the stop geometry on `RiskAnchorPrice` instead of the blended `AvgCost`; HL perps plus `manual`; backtested.
- Manual actions run under the kill switch and circuit breaker. `force-close` is live HL perps only.

### Liquidation guard and protection (`hyperliquid_liquidation_guard.go`, `hyperliquid_open_trailing.go`, `hyperliquid_protection.go`)

- `hlLiquidationPx` is a NET per-coin map read via `hlLiquidationPxForSide` against `hlNetSideByCoin`. Healing runs through the trailing `trailingReplacePolicy.liquidationPx` or the static/regime `buildHyperliquidProtectionPlan`, strictly tighter only. `runHyperliquidLiquidationAudit` tightens every owner; `hlLiquidationClampReplace` is tri-state (`protection lost` / re-arm / refuse over-virtual-net). One in-cycle retry on a positively rejected cancel-with-nothing-resting; classification comes from what rests. The off-cycle pass runs at `liquidationAuditIntervalSeconds`, floored at 60s. Preflight: `scripts/check-hl-stop-bankruptcy-bound.sh`. `recordPositionOpen` runs after the deferred-open execute leg. Operator view: § Hyperliquid Liquidation Guard.
- On-chain TP suppression never nils `CloseStrategy`: live tiered-TP checks send `close_owner: on_chain_tp` with `closes` and the position context, Python evaluates no close and echoes the owner, and Go holds a signal whose result lacks the echo. `hlCloseOwnerForCheck` hands the exit to the in-process evaluator (one alert) only when nothing rests or is armed and the tiers are unplaceable. Paper never places on-chain TPs.
- `invert_signal` flips only the open signal, inside the Python composer (`invert_open_signal` on the refs, echoed as `open_signal_inverted`). Go never negates a check result. A same-side close (`CloseFraction > 0` on the position side) is zeroed before the gate chain, so it cannot become a scale-in add. A missing or extra echo holds the signal. `type: manual` is not inverted.
- `regime_side` (`regime_side.go`): `resolveRegimeSide` is the one resolver.
  It reads the directional window through `regimeDependencySnapshot`, the base direction, and, for unacknowledged live strategies, the exact current label in the installed regime-side evidence snapshot.
  Go sends `open_side` (`long`/`short`/`none`) on `--strategy-refs` (single and batch slot; the slot fingerprint includes it and the batch outcome keeps the sent request).
  Python maps a non-zero raw trigger to that side before the HTF filter and echoes `open_side_applied`; a missing, extra or different echo is a script error.
  `finishHyperliquidCheck` re-resolves from the returned regime payload; a different flat side zeroes `Signal` only, never `CloseFraction`.
  `hl_perps_step.go` holds any position-increasing signal when the decision started open, including after a same-decision paper stop.
  The evidence snapshot is separate from the `regime_directional_policy` store.
  Missing, malformed or expired evidence only holds new flat entries: no loader refuses it except `probe` and the SIGHUP reload, so startup, `force-close`, manual commands, `inspect` and config writes always load.
  `probe` refuses every live unacknowledged strategy without usable evidence (`GO_TRADER_PROBE_REGIME_SIDE_ADMISSION=off` skips it; only the `update.sh` recovery probe sets it).
  A reload refuses only a strategy whose evidence need or certification cell changed (`regimeSideReloadAdmissionErrors`), so an unchanged strategy's expiry never blocks an unrelated change.
  Startup installs the artifact it read and every SIGHUP reads and installs the artifact before any rejection, so a rejected reload still revokes a removed label; a strategy without usable evidence gets an `entries HELD` line and owner DM.
  `LoadConfigForLedgerExport` skips the artifact read. `update.sh` passes the unit's `GO_TRADER_DIRECTIONAL_CERT_PATH` (Environment then EnvironmentFile) to the update probe.
  Status and the UI show window, label, readiness, requested side and hold reason; `inspect` reports runtime values as unresolved.
  The entry trade carries a `Regime side:` alert note from the decision's own resolution.
- Moved stops: a stop a take-profit `sl_after` rule moved carries `Position.SLAfterMoved` (set on every venue-confirmed move, unified and non-unified, paper included) and `SLAfterTriggerPx` (the last venue-confirmed trigger, column `sl_after_trigger_px`, captured by `noteMovedStopTrigger` and before every clear by `clearRecordedStopLoss`; it never marks an order as resting).
  `buildHyperliquidProtectionPlan` then sends `StopLossTriggerPx` (the resting trigger, else the preserved one; the label only when strictly tighter; liquidation clamp tightens only) with `PreserveMovedStop` as `--stop-loss-trigger-px`/`--preserve-moved-stop`.
  Python rounds it never looser, or places nothing.
  An unrecoverable trigger strips the stop leg before any cancel, returns `hlStopRearmMovedTriggerLost`, and sends one CRITICAL; there is no label fallback.
  The main-loop fixed-ATR arm skips moved positions so the sync is the only re-placer.
  Startup `backfillMovedStopMarkers` marks legacy non-unified positions whose processed tiers held a rule.
  The `--sync-protection` argv is probed at startup (`syncProtectionProbeArgv`).

### Probes, diagnostics, alerts, commands

- `version_probe.go`/`probe_cmd.go`/`exit_codes.go`: every unique check script runs with `--probe-only` at startup; `probeFailureScriptMissing` detects `"can't open file"`; a failure logs, DMs the owner, and exits 78 (`EX_CONFIG`).
- `trade_diagnostics*.go`: eager insert in `recordClosedPosition`; MFE/MAE computed async outside `mu`.
- `hyperliquid_agent_status.go`: sole owner of the Hyperliquid API wallet alerts (see Hyperliquid API wallet lifetime): one in-memory state behind its own mutex, never `mu`; inputs are the startup and hourly checks (`runPythonWithTimeout`, read-only), `observeHLAgentScriptOutput` right after each signed HL runner (`executor.go`, `manual_limit.go`), and `notifyLiveExecFailure` for `platform: hyperliquid`; only a check that started after the last rejection clears the CRITICAL.
- `agent_info.go`: read-only dump.
- `logger.go`: `log_level` gate. A new per-cycle or per-check line uses `logger.Debug`/`logDebugf` or an on-change form (`InfoOnChange`, `logOnChangef`, keyed on a stable state value, never on a price or balance); errors, warnings, trades and non-HOLD signals stay unconditional. Event-driven protection sync lines (after a trade, a limit fill, a failed-close re-arm) stay `logger.Info`.
- `failure_alerts.go`/`script_failure_alerts.go`: primary alert at 3 strikes; transient 429/5xx/timeout stays WARN until 15 strikes or 75 minutes.
- `paper_close.go`/`remove_strategy_guard.go`: `/paper-close` mutating (§ Operator paper close); the remove guard for Discord and the dashboard.
- `discord_commands.go`/`discord_mutating_commands.go`: `/clear-cash-reconcile`, `/clear-operator-close` (`operator_required_close.go`), `/resolve-entry` (`entry_operator.go`), `/resolve-rh-order` (`robinhood_order_recovery.go`) and `/resolve-rh-account` (`robinhood_account_resolve.go`) mutating, `/closing-strategies` read-only.
- `missing_mark_alerts.go`: throttled DM per `(strategy_id, symbol)`. `hl_reconcile_gap_alerts.go`: alerting only. `portfolio_warning.go`, `circuit_breaker_alert.go`: alert routing.
- `model_only_reconcile.go`: § Model-Only Close Reconciliation. `portfolio.go`: `CashReconcileRequired` (§ Cash Reconcile Latch). `kill_switch_close.go` and `*_close.go`: `type=manual` HL positions join the flatten via `hlKillSwitchAll`.

### Shared wallet, cashflow, limit orders (`shared_wallet*.go`, `cashflow_journal.go`, `kill_switch_limit_orders.go`, `orphan_limit_cancel_alerts.go`, `limit_fill_exposure.go`)

- Shared-wallet drift tolerance is $0.01 over 2 cycles. Pool sizing comes from account equity minus deployed margin; switching a strategy between allocated and pool budgeting needs a flat book and a restart.
- Cashflow journal: HL total-drift is live; OKX and TopStep run in shadow. HL expected equity subtracts the gap between frontend `closedPnl` and fill-price realized PnL. Each fill's gap is stored at ingest (`hl_basis_error`) from per-coin books in the same transaction; an unresolvable same-timestamp chain (by `startPosition` from the held size) or unknown entry books a zero gap. Every journal, including a new one, first backfills exchange fill history up to its fill cursor, one page per cycle. The alarm holds `PENDING` for up to `sharedWalletDriftAlertThreshold` cycles, then logs `OFF` and uses the trade ledger until the backfill finishes. A close whose entry is outside exchange history keeps a zero gap and is counted in a warning. Stored fill amounts stay `closedPnl - fee`.
- Kill-switch limit orders: each row goes cancel → `--limit-status` → delete under a 60s pre-flatten deadline; an unresolved row clears `OnChainConfirmedFlat` and blocks `CanAutoResetWithoutOwner`. Operator view: § Portfolio Kill Switch And Latch Ownership.
- Orphan cancel lane: rows in `cancel_requested` or expired that fail `killSwitchLimitOrderAdoptionBlock`; roster from `killSwitchLimitOrderRoster` plus `collectKillSwitchLimitOrderCandidates`. Severity-gated throttle; `operator_required_since` backs off the poll. `applyLimitExposureOperatorRequired` sets the marker on `unbacked`, leaves it on `unreadable`, clears otherwise; the marker is the sole gate for `manual-clear-limit-row <oid> --flattened`.
- The orphan cancel lane is `cancelOrphanedLimitOrder`.
- Limit fill exposure: `hlLiveExposureReader` polls rows, decides per coin, finalizes; `snapshotNewerThan` is the sole reader; `applyCoinLimitFills` aggregates per coin; `classifyLimitFillLiveExposure` requires same-direction and contained. DM severity is gated by outcome.

### Python side (`shared_scripts/`, `platforms/`, `shared_tools/`, `shared_strategies/`, `backtest/`)

- `check_hyperliquid.py` splits into `build_shared_signal_state` and `evaluate_signal_slot` (on `shared["df"].copy()`); single mode and `--batch-check` both run that pair.
  A shared-state failure raises `SharedSignalStateError` → one `error_scope="shared_state"` sentinel; a slot exception stays in its slot.
  Numeric `indicators` are rounded to six decimals except `eom_`, `mfi_`, `ama_`, `erp_`, `stc_`, `trix_`, `cpr_`, `mir_`, `smi_`, `cce_`, `efp_`, `tsi_` and `klinger_` columns (`FULL_PRECISION_INDICATOR_PREFIXES`), which keep full float precision so a `money_flow_index_reversal`, `ease_of_movement_breakout`, `adaptive_moving_average`, `elder_ray_pullback`, `schaff_trend_cycle`, `trix_momentum`, `camarilla_pivot_reversal`, `mass_index_reversal`, `stochastic_momentum_index`, `calendar_conditioned_entry`, `elder_force_pullback`, `true_strength_index` or `klinger_divergence` entry reproduces from the check output (#1658, #1671, #1641, #1648, #1651, #1659, #1669, #1670, #1663, #1653, #1668, #1667, #1662); text columns in `TEXT_INDICATOR_COLUMNS` go to `indicator_labels`.
  Check-time frames carry a `decision_time` attribute (closed-bar cutoff and boundary, empty when untimed) and `_make_dataframe` records out-of-order received timestamps in `received_order_defect_ms` before sorting; only `schaff_trend_cycle`, `camarilla_pivot_reversal`, `stochastic_momentum_index`, `calendar_conditioned_entry` and `klinger_divergence` read them (`mass_index_reversal`, `elder_force_pullback` and `true_strength_index` read `received_order_defect_ms` only), and historical frames carry neither.
  Reference preflight receives the timeframe: a fixed-interval cadence (`expected_interval_ms`, explicit or default) that disagrees with it is refused before any candle fetch, and a batch whose every slot fails preflight returns per-slot refusals without building shared state (#1669).
- `fetch_hl_agent_status.py` reports API wallet facts only (signer, master, role, `valid_until_ms`); Go classifies them. It uses the adapter's `hyperliquid_signer`, `hyperliquid_base_url` and `hyperliquid_info_api` helpers (bounded HTTP timeout, no meta fetch), never prints a traceback, and redacts the key from every error.
- HL adapter caches in `/tmp`, lazy `_ensure_exchange`, sparse indices via `_normalize_spot_meta`. The SDK's `asset_to_sz_decimals` keys by integer asset index; resolve via `name_to_asset(symbol)` (fallback `coin_to_asset`); a direct symbol lookup is a legacy test-mock-only fallback.
- `shared_tools/atr.py` shims `shared_strategies/open/indicators_core.py`; `atr_method` resolves via `resolveATRMethod`. `indicators_core.py` holds ATR/RSI, the per-endpoint `fixed_length_ema` (exactly L values, restarted at each bar's own seed; #1668), plus `hurst_exponent` (DFA, live SSoT) and research-only `hurst_rescaled_range`. Options strategies live in `options/strategies.py`.
- Strategy DSL: config params sit under runtime; `HTFFilter` is not available on options or `delta_neutral_funding`. `no_edge` holds 70 names (`edge_status.go` `noEdgeStrategies` mirrors the Python registry; `scripts/test_go_python_registry_parity.py` checks it with platforms, short names and native close names); admission is `noEdgeAdmissionErrors`, warnings `noEdgeStartupWarnings`. `productionRefusedStrategies` (`production_refusal.go`, mirrored by registry `production_refused=True` and checked by the parity script) marks research names; `researchAdmissionErrors` (`research_admission.go`) admits them only in explicit paper on the Hyperliquid perps sealed-feed route in every loader before the market-feed check, and paper-to-live refuses them at confirmation and again on the current disk config inside `mutateConfigRoot`; Python `admit_strategy_references` applies the same mode, route and same-name close rules before the no-edge gate, in solo and batch checks.
- Backtest files: `backtester.py`, `optimizer.py`, `run_backtest.py`, `backtest_{options,theta,pairs}.py`, `parity_diff.py`. `--config <path> --strategy <id>` reads the single `close_strategy` and applies `user_defaults` by default (`--defaults system` keeps the built-in baseline). `--intrabar-resolution bar_close` is the legacy race resolution. Regime: `--config` threads `allowed_regimes` (the CLI flag is rejected); composite via `regime.windows`; the open name falls back to `args[0]`. `regime_directional_policy` is backtestable behind its flag; scalar `sl_after` and `*_atr_mult_regime` stop dicts are backtestable. Liquidation floor: a sticky equity floor at 0 from the first bust; blown legs report `±LIQUIDATED_METRIC_FLOOR`. `tune_live.py` writes SCHEMA_VERSION=2 `promotion_baseline`.
- Walk-forward warm-up (`optimizer.strategy_warmup_bars`): the fold prefix is the largest integer grid parameter (`max_indicator_lookback`) except for `calendar_conditioned_entry`, whose prefix is `training_bars + refit_bars - 1` (its training span plus the largest distance to the last scheduled refit) maximized over the grid, so the boundary decision that fills on the first scored bar sees a complete table.
  For `elder_force_pullback` the prefix is the largest `required_history_bars` (`max(recursion_length + 2, trend_period + slope_span + 1)`) over the grid, and never less than the largest integer grid parameter (#1668).
  For `klinger_divergence` the prefix is the largest `support_bars` (`replay_bars + left_span + max_separation + right_span + setup_expiry + 2`) over the grid, each row merged over the registry defaults, and never less than the largest integer grid parameter (#1662).
  Names in `RECURSION_HISTORY_STRATEGIES` (`trix_momentum`, `true_strength_index`) take the larger of the largest integer grid parameter and their module's `required_history_bars(recursion_length)` maximized over the grid, so the same boundary decision sees its full recursion window (#1667).
  Names in `UNSEEDED_ENTRY_ONLY_STRATEGIES` (`calendar_conditioned_entry`, `elder_force_pullback`) never get a prefix replay: their -1 opens a short, so a pre-window long would be invented; every fold starts flat (#1653, #1668).
  Every other strategy also starts flat when the scored engine can open a short (`warmup_handoff_prefix`, #1785), whatever its canonical `short_entries` flag says: the optimizer passes no prefix to such an engine, because the #1796 scope has no tested short handoff path; `Backtester.run(warmup_prefix=...)` itself converts and carries either side.
  The scored engine can open a short when its certified `regime_directional_policy` has a `short` or `both` entry, or when it runs the open/close path (a close evaluator, `open_action` or `close_fraction` columns) with a normalized direction other than `long`; an `open_action` column without a policy also counts.
  Each training candidate and the selected test run decide this from their own engine, after the close stacks are built, so a close-stack grid (default `long`) and an explicit `long` replay the prefix, and an omitted direction with a fixed close evaluator does not.
  A run whose engine cannot open a short passes the prefix to `Backtester.run(warmup_prefix=...)` (#1796): the same engine (that candidate's stack, or the selected `best_bt` for the test fold) runs the prefix rows and the scored rows in one bar loop, so closes, take-profit tiers, stops, stop-after moves, trailing high water, time-stop age, regime stamps, entry gates, sizing state and decisions due on the first scored row carry over exactly as in uninterrupted execution.
  At the first scored row the engine records a `warmup_handoff` snapshot (inventory before any synthetic liquidation, which agrees with a prefix-only ledger `interval_end`) and converts capital: with last prefix close P, cash K, quantity Q and scored initial capital C, it scales cash, quantity, initial quantity, scale-in notionals and perps sizing state by C / (K + Q * P), so the remaining fraction and every price-based level stay unchanged.
  The carried trade's scored profit basis is P with no entry fee, while stops and tiers keep the historical average cost; prefix trades, fees, funding, equity and skip counters never enter scored metrics.
  Absolute USD settings keep their configured values: when the factor is not exactly 1, `warmup_handoff.unscaled_usd_settings` lists each one that is set (`perps_sizing.margin_per_trade_usd`, `perps_sizing.pool_available_margin` under `shared_wallet_pool`, `scale_in.add_notional_usd`, `scale_in.max_added_notional_usd`, and `venue_margin.tiers` under `venue_isolated`), so that result is not a pure scaled copy of uninterrupted execution; the list is empty when the factor is 1.
  The handoff refuses non-finite or nonpositive prefix equity, a non-finite last close under an open position, `starting_long`, `record_events` and `execution_spec`; a replay error skips that candidate or test fold under the fold-error policy, never with a substitute seed, and charge-mode unpriced held hours in the prefix (`warmup_handoff.funding_unpriced_held_hours`) reject it through `charge_funding_rejected`.
  Both prefixes are sliced after `rejoin_funding_columns`; there is no replay cache, and equal training metrics pick the smallest (params, close stack) JSON key, so candidate order does not change the pick. `warmup_exit_long_entry` stays for direct callers only.
  Names in `HORIZON_BOUND_TIME_STOP_PARAMS` (`calendar_conditioned_entry` -> `holding_bars`) score each grid combination only on a copy of each close stack whose `time_stop` `max_bars` equals that combination's horizon (`bind_time_stop_horizons`), so a label horizon never meets a different exit horizon; stacks without `time_stop` are unchanged.
- `calendar_conditioned_entry` (#1653, no-edge, futures, both directions, entry-only): at refit boundaries on a `refit_bars x expected_interval_ms` grid anchored at 1970-01-05T00:00:00Z it fits a UTC weekday/hour table (168 cells) from the trailing `training_bars` bars, using only labels `open[e + holding_bars] / open[e] - 1` whose exit open is at or before the refit bar (the simulator's `time_stop(max_bars=holding_bars)` entry and exit opens).
  Each cell averages labels per UTC week, uses a Newey-West error over the weekly series with a conservative effective sample, and qualifies a direction when its cost-adjusted lower bound at Bonferroni `(1 - confidence) / 336` is strictly positive; a closed bar enters when the next bar's cell qualifies in the last scheduled table.
  A decision needs `training_bars + d` clean rows, where `d` (0 to `refit_bars - 1`) is its distance to the last scheduled refit; any defect inside that history, undeclared timing or a stale check frame holds with a `cce_reason`; `calendar_refit_tables` exports the per-refit tables.
  Live and paper closes have no `bars_held`, so `time_stop` never fires there: `calendarConditionedEntryHistoryErrors` (in `validateConfig`, so `LoadConfig`, `LoadConfigReadOnly` and SIGHUP reload) refuses every open or close reference whose `training_bars` (registry default 4368) is at or below the check frame, which is 200 candles, `regimeRequiredOhlcvLimit` when regime is enabled, or a larger `--ohlcv-limit` in `args`, so every scheduled check holds with `insufficient_history`.
  The study `backtest/candidates/calendar_conditioned_entry_1653/` records the proposed versioned table artifact that a future loader would need.
- Close evaluators: `avwap_stop` is a virtual exit only and is absent from `isTieredTPATRCloseName` and `closeStrategiesSuppressedByOnChainProtection`; `atr_stop` (`atr_source` default `entry`) and the `avwap_stop` buffer (`atr_source` default `live`) use `market_ctx["atr"]` for `live` and the position entry ATR for `entry`, with no fallback.
  They do not read `tp_model`, so this rule is the same on every platform, Hyperliquid included.
  The live-ATR tiered evaluators (`tiered_tp_atr_live`, `tiered_tp_atr_live_regime`, `tiered_tp_atr_live_regime_dynamic`) recompute from `market_ctx["atr"]` under `atr_source: live` (entry ATR fallback) only outside the resting-limit model.
  Under `tp_model: resting_limit` (`check_hyperliquid.py`, and the backtester with `platform=hyperliquid`), they use the entry ATR first and read the live ATR only when the position has no entry ATR and `atr_source` is not `entry` (`_resolve_tier_atr`).
  The position context carries `risk_anchor_price` (argv `--position-risk-anchor-price`, batch slot key; every check script parses it, both probe argvs send it).
  Backtest regime gating blocks entries when `bar_regime ∉ allowed_regimes`.

### Notifications and channels

Every send goes through `MultiNotifier`, which fans out to its backends. Channels are `spot`, `options`, `<platform>`, `<platform>-paper`, `<platform>-paper:<id>`. `resolveChannelKey(platform, type, isLive, source)` resolves a paper strategy's key in the order `<platform>-paper:<source>` (only when `source` names a paper source), then `<platform>-paper`; after those, and first for a live strategy, it takes the first backend holding a `<platform>` key, else a `<type>` key. Summaries, leaderboards, and Sharpe groups therefore split by mode only when a paper key is configured. `SendToPartitionChannels(part, msg)` sends a live-partition message to every channel through `SendToAllChannels`. For a paper partition it sends to the channels that partition's own roster resolves to (`partitionChannelValues` with `resolveTradeChannel`, rebuilt on `ReloadConfig`), never by scanning key suffixes, and falls back to `SendToAllChannels` only when that set is empty.

### Build, deploy, and test mechanics

- `scripts/update.sh --restart` is atomic: preflight → `pull --ff-only` (or `--rsync-from <src>`) → `uv sync --no-dev` → build → probe → journald namespace sync (systemd mode) → binary swap (previous kept as `.prev`) → unit install + `daemon-reload` (systemd mode; previous unit kept as `<unit>.prev`) → restart and verify → rollback on timeout, which restores both `.prev` files.
  `--all --restart` discovers deployments via `discover_deployment_dirs_from_systemd` and the glob, and skips a deployment whose restart route is proven unable to succeed (§ Auto-Update, batch mode).
  To rebuild the current commit, run the same `bash scripts/update.sh --restart`: with nothing to pull, `pull --ff-only` is a no-op and the script still runs `uv sync --no-dev`, builds Go from the committed `HEAD` (uncommitted build-input changes stop it; untracked files are never built), probes, swaps and restarts.
  Never rebuild Go alone.
  Config-only reload is `kill -HUP $(pgrep go-trader)` and Python picks the change up next cycle.
- Post-update classification diffs `<running>..HEAD` per § Post-Update Agent Protocol.
- Container images build only from the root `Dockerfile` (§ Docker Container); `scripts/test_container_image.sh` (needs Docker, `GO_TRADER_TEST_IMAGE`, `GO_TRADER_EXPECT_VERSION` and `GO_TRADER_EXPECT_COMMIT`) is their integration suite.
- Test placement: Go `_test.go` beside the file; Python `test_*.py`. Pure helpers to extract from subprocess wrappers include `perpsLiveOrderSize`, `*OrderSkipReason`, `parseXxxCloseOutput`, and the Sharpe computation. `shared_scripts/test_*.py` sits outside pytest `testpaths`; registry and sys.path tests import through `importlib.util.spec_from_file_location`.

### The `@claude` GitHub workflow (`.github/workflows/claude.yml`)

- `classify` plus `review`, `implement` and `fix-pr` callers of `richkuo/rk-skills/.../claude-run.yml@main`. The callers differ in `mode`, `flow` (`implement` only) and `permissions:` (`review` has no `id-token: write`); `fix-pr` also re-checks that the PR author is an owner, member or collaborator, or `claude[bot]`.
- git and gh run on the Claude GitHub App token. Review comments post as `github-actions[bot]`; implement posts as `claude[bot]`. Patch steps key on `RUN_ID`.
- Mode routing: `pull_request_review*` → `review`; issues, non-PR comments, docs-sync, and release → `implement`; otherwise the keyword after `@claude` (untrusted or fork → `review`; trusted → `fix-pr`).
- Comment patching uses `patch_claude_comment.sh` and `compose_claude_comment.py`, staged from `rk-skills` `templates/claude-workflow/scripts/` into `$RUNNER_TEMP` each run. `.github/scripts/test_workflow_logic.py` executes the real `run:` blocks. Concurrency `group: claude-<N>`, `cancel-in-progress: false`. `timeout-minutes: 90`. `issues: types: [opened]` only.
- The CLAUDE.md revision step must `git commit` and `git push origin HEAD`, then diff `HEAD -- CLAUDE.md` against `origin/main` to catch a silent revert.
- Operator lookup for the latest bot review: `gh api repos/richkuo/go-trader/issues/<N>/comments --jq '[.[] | select(.user.login=="claude[bot]" or .user.login=="github-actions[bot]")] | last | .body'`.

---

## Tests

```bash
/opt/homebrew/bin/go -C scheduler test ./...
uv run --no-sync python -m pytest
uv run --no-sync python -m pytest scripts/test_go_python_registry_parity.py
```

If the Go cache needs an explicit writable path: `env GOCACHE=/tmp/go-build-cache /opt/homebrew/bin/go -C scheduler test ./...`.

**Pre-PR checks.** Before opening a pull request, run `bash scripts/check_pre_pr.sh` from any directory (after `uv sync` in the worktree) and fix every failure.
It runs the cheap checks of the CI `checks` job in order (`syntax`, `test-budget`, `agent-docs`, `doc-lines`, `github-scripts`), keeps going after a failure, prints one `PASS:` or `FAIL:` line per check, and exits 1 when any check failed; `bash scripts/check_pre_pr.sh <check>` runs one check.
The CI `checks` job steps call the same subcommands, so the local result and the CI result come from one copy of the logic; change a check only in the script.
It runs under macOS bash 3.2 and BSD awk as well as on the Ubuntu runner, so keep it free of `mapfile` and GNU-only flags.

**Docs-only pull requests.** The `changes` job in `ci.yml` and in `container.yml` runs `scripts/ci_code_changed.sh`, which reads the pull request merge commit and sets `code=false` only when every changed path is one of `CLAUDE.md`, `AGENTS.md`, `README.md`, `SKILL.md`, `docs/POST_UPDATE_HISTORY.md` or `docs/DOCKER.md`.
No test reads these files, and the container image does not hold them.
With `code=false`, every Go, Python-backed, shell-suite, Python and container image job is skipped, and `checks` still runs (it holds the CLAUDE.md size and layout check).
A push to main, a release, a manual run, any other path and any git error all give `code=true`, so the full CI runs; a rename counts both its old and its new path.
When a test starts to read one of these files, remove that file from the list in the script.
Before it decides, the `changes` job runs `scripts/check_ci_code_changed.sh`, which runs the detector on real merge commits in a temporary repository (docs-only, docs and code, push, no event, a test-read doc, renames, a non-merge head) and checks that both workflows wire the `code` output; a failure there fails the run, so a broken detector never skips the suites without a red check.
Each gated suite job carries exactly `needs: changes` and `if: ${{ needs.changes.outputs.code == 'true' }}` at job level; `TestShellSuiteCIWiring` accepts only that pair and still refuses every other condition.

**Python CI and the test budget.** Python CI has two suites: `default` (`uv run pytest` over `testpaths`) and `shared_scripts` (`uv run pytest shared_scripts/`, which sits outside `testpaths`).
- `python-shard` is a matrix that splits each suite by measured duration per test (pytest-split, `--splitting-algorithm least_duration`, durations from `scripts/pytest_durations/<suite>.json`). Each shard runs `-n auto --dist=worksteal --durations=25` and writes a `--report-log` of every test report. The shard count lives only in the matrix rows (`splits`); to change it, edit every row of that suite.
- `checks` runs `scripts/check_pre_pr.sh syntax` and `scripts/check_pre_pr.sh test-budget` (`scripts/check_test_budget.py`), then `scripts/check_pytest_shards.py collect` for each suite, which records the full pytest collection. Its `python` output is `success` only when these Python steps and the artifact upload pass. The job then runs the agent-instructions size and layout check, the SKILL.md line-length check and the `.github/scripts` check (`scripts/check_pre_pr.sh agent-docs`, `doc-lines` and `github-scripts`), each even when an earlier step failed; their results never reach the `python` check.
- `python` (the old check name) needs `changes`, `checks` and `python-shard`, is skipped with them on a docs-only pull request, and runs `scripts/check_pytest_shards.py verify` for each suite. It fails and prints the node ids when a test is in the collection but no shard ran it, when a test ran more than once, when a shard ran a test that the collection does not hold, when a test failed, when a shard log for groups 1 to N is missing or did not finish, or when any Python job did not succeed.
- Durations upkeep: the `python` job uploads a `pytest-durations` artifact made from the shard logs. To refresh, run `tmp=$(mktemp -d) && gh run download <green main run id> -n pytest-durations -D "$tmp" && test -s "$tmp/default.json" && test -s "$tmp/shared_scripts.json" && cp "$tmp/default.json" "$tmp/shared_scripts.json" scripts/pytest_durations/` and commit both files. `gh run download` refuses to replace an existing file, so the command downloads into an empty directory and copies only when the artifact holds both files. A stale or missing durations file only makes shards uneven (a test with no recorded duration gets the average); it never drops a test, because `verify` compares against the full collection. The `verify` output says how many collected tests have no recorded duration.
Under pytest-xdist each worker and each check subprocess has its own process memory, so a test never coordinates shared files through worker globals; use `tmp_path`.
The budget command checks three rules against `scripts/test_budget_baseline.json`, which must be an object with exactly the keys `wording_only_tests`, `refusal_grid_tests` and `contract_copy_tests`, each a list of unique test identifiers:
- `wording_only_tests`: a test whose every assertion only matches message wording. Baseline one only when the wording drives an operator decision.
- `refusal_grid_tests`: a test with at least three no-edge refusal cases, from one `pytest.mark.parametrize` matrix or from the lists its `for` loops and comprehensions iterate (a `subtests` loop included), each list counted once. The command parses test sources with `ast` and never imports or runs a test module. A case is a refusal case when it holds one refusal-class string (`explicit live mode`, `missing mode`, `invalid mode input`, `invalid acknowledgement`) and an argument list that the shared admission parser in `shared_tools/strategy_composition.py` refuses with that class; the empty list is a missing-mode case. Test names and mode tokens alone never classify a case.
- Supported matrix forms: a literal list, a module-level name (or alias chain, or a `from <test module> import` name) that resolves to a literal list, and `pytest.param` entries. A case that carries a strategy reference (a dict, any call such as `json.dumps`, or a `--strategy-refs`, `--open-strategy` or `--close-strategies` token) is mixed effective-reference coverage and never counts; admitted and acknowledged cases and unrelated backtest modes match no refusal class.
- The baseline lists only the full Hyperliquid matrix and the four-class wiring matrix in `shared_scripts/test_no_edge_admission.py`. Per-strategy copies are never baselined (see Add Or Change Strategies, no-edge step).
- `contract_copy_tests`: a test outside the two strategy contract suites (`backtest/tests/test_strategy_backtest_contract.py`, `shared_scripts/test_strategy_check_contract.py`) whose function name is a test name in either suite or a name in either suite's `REPLACED_PER_STRATEGY_TESTS` tuple (the retired per-strategy names). Each suite must bind that tuple at module level as a literal tuple or list of `test` names; bind `()` when none. Contract copies are never baselined: add a case to the suite instead (see Add Or Change Strategies item 7).
- Exit 1: a detected identifier missing from the baseline (`new:`) or a baseline entry no longer detected (`stale:`), with the rule name and, for `refusal_grid_tests` and `contract_copy_tests`, a pointer to Add Or Change Strategies; an unreadable or unparsable test source; a missing strategy contract suite; a contract suite with no `REPLACED_PER_STRATEGY_TESTS` or a malformed one; or an invalid baseline. Exit 0 otherwise.
- `--list` prints `<rule> <identifier>` for every rule (rule order, identifiers sorted) and still validates. `--write-baseline` rewrites every key, sorted, only after a clean scan; review every identifier it adds before committing.

Test fixtures: `stampEntryATRIfOpened` (`scheduler/main.go`) sets `Position.EntryATR` only when the ATR is positive and not NaN and, when `AvgCost` is positive, no more than 50% of `AvgCost`, and never overwrites a set value. A fixture with a positive `AvgCost` and a larger ATR gets no entry ATR; a fixture with `AvgCost` 0 gets any positive ATR.

Go CI must not depend on a Python runtime, so a test for a subprocess-based live helper extracts the pure parser or decision helper rather than invoking Python. A Go test that runs a shell suite or script which starts Python (the bankruptcy-bound preflight parity, the merge-paper-instance and update-helper suites) carries `//go:build pyintegration` and runs in the `go-python-integration` CI jobs (`go -C scheduler test -tags pyintegration -run <names> ./...`); Go-to-Python registry parity lives in `scripts/test_go_python_registry_parity.py`.

**Python-backed Go test shards.** The tests run one after another and many use `t.Setenv`, so CI splits them into parallel jobs by measured time. The `go-python-integration` job holds the only copy of the allowlist in its `-run` regex and passes `-skip` for the families that named shard jobs (`go-python-integration-<shard>`) run; today only `TestMergePaperInstance` runs in a named shard. A new allowlisted test runs in `go-python-integration` unless a named shard selects it. To move a slow family into its own shard, add a `go-python-integration-<shard>` job with the same step shape and add the family to the `-skip` regex of the job that ran it before; `TestShellSuiteCIWiring` proves the result.

**Shell suites in CI.** Every `scripts/test_*.sh` has exactly one wiring in `shellSuiteWirings` (`scheduler/update_sh_syntax_test.go`): a `pyintegration` Go wrapper that the `go-python-integration` allowlist selects (`test_merge_paper_instance.sh`, `test_update_helpers.sh`), a step in a shell-suites CI job (the `shell-suites` job runs `test_observation_replay.sh`, `test_trade_flow_replay.sh`, `test_macro_event_replay.sh`, `test_volume_profile_replay.sh`, `test_book_replay.sh`, `test_premium_replay.sh`, `test_ledger_export.sh`, `test_merge_paper_service_fixture.sh`, `test_robinhood_session_service_fixture.sh`, `test_egress_enforcement.sh` and `test_egress_docker.sh`; the `shell-suites-migrate-<n>` jobs run `test_migrate_service_layout_fixture.sh` in scenario shards), or manual-only with a reason (`test_container_image.sh`, which `.github/workflows/container.yml` runs). Green must mean the suite proved its criteria:
- A shell-suites step runs its suite through `scripts/run_ci_shell_suite.sh`, which fails on a non-zero exit, a missing exact success line, any line that starts with `SKIP:`, or any omission text the step forbids. A Go wrapper calls `assertShellSuiteOutput`, which applies the same rules from the map; the wrapper for `test_update_helpers.sh` builds the binary and passes `GO_TRADER_BIN`, so the effective-cadence drift case runs. Each `go-python-integration` shard step fails on any `--- SKIP` line.
- The systemd fixtures run under `sudo env GO_TRADER_BIN=... [FIXTURE_GO=...|FIXTURE_UV=...]` (secure_path does not find the setup-go toolchain). `MERGE_PAPER_SERVICE_FIXTURE_REQUIRE_RUN=1`, `ROBINHOOD_SESSION_SERVICE_FIXTURE_REQUIRE_RUN=1` and `MIGRATE_SERVICE_LAYOUT_FIXTURE_REQUIRE_RUN=1` turn each SKIP into a failure, as `LEDGER_EXPORT_REQUIRE_CAPTURE=1` does for the ledger suite; unset, the suites still SKIP for manual runs. The merge-paper and Robinhood fixtures run one after the other in `shell-suites`.
- The migrate fixture changes host-wide systemd files and state, so it runs only on ephemeral CI runners, never beside another root fixture. It is the one sharded suite (`shardEnv` in its wiring): each `shell-suites-migrate-<n>` job is a separate runner that builds the binary and runs the fixture once with a literal `FIXTURE_SCENARIOS='<list>'` and the marker `OK: migrate-service-layout fixture passed (<list>)`. The union of the shard lists must equal the default `SCENARIOS` list in the script, each scenario in exactly one shard.
- A new default scenario goes into exactly one shard list (list and marker). To rebalance, read each scenario's time from the `==` section timestamps of a recent green run (`gh run view <run> --log`), and add per-shard setup, build and cleanup time; move scenarios between lists or add a `shell-suites-migrate-<n>` job with the same step shape, so each job stays under 10 minutes. Every CI job with an `actions/setup-go` step passes `cache-dependency-path: scheduler/go.sum` (the module is in `scheduler/`, so the default root `go.sum` finds no key).
- Omission checks match each suite's exact omission text, never a generic `NOTE`/`note:` (the tools print benign notes). Every `echo "NOTE: ..."`/`echo "note: ..."` line in a suite must match exactly one forbidden or permitted omission in the map. Permitted omissions in CI: none. The ledger suite runs as the runner user after `sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0`: with the Ubuntu 24.04 default (1) the capture worker cannot start (`fork/exec /proc/self/exe: permission denied`), and under `sudo` the shared-mount-namespace worker check exits 126 instead of 1. As the runner user, `strace` is installed and the permission-denied refusal runs, so all three ledger omissions are forbidden.
- `TestShellSuiteCIWiring` (the untagged `go` job) fails when a suite has no wiring, when the allowlist does not select its wrapper, when a wrapper lacks the `pyintegration` tag or does not run its script, when a command in the jobs named `shell-suites` or `shell-suites-*` does not run a non-sharded script exactly once (a comment or a `#` on the line does not count), or when a step drops its success line, a forbidden omission, a require flag, or `sudo`.
- For the sharded migrate fixture, `TestShellSuiteCIWiring` parses the default list from the script and fails on a missing, repeated, unknown or empty scenario, a shard without exactly one single-quoted `FIXTURE_SCENARIOS` list, a marker that does not name that list, or any other mention of `FIXTURE_SCENARIOS` in the workflow. It fails closed when it cannot read the workflow (in each shard job, one `-run '...'` argument and at most one `-skip '...'` argument with no `/`, after `set -o pipefail` in the same step, piped to `tee` with the `--- SKIP` gate reading that log; `run:` as a plain value or `|` block; no `continue-on-error`, `if:`, `shell:` or `defaults:` in a shard job or a shell-suites job; no `strategy:`, `matrix:` or `needs:` in a shell-suites job; no top-level `defaults:`; no `GOFLAGS` in the workflow; no `pyintegration` in any other job). `TestUpdateShellScriptSyntax` runs `bash -n` on every `scripts/test_*.sh` and the listed operator scripts.
- `TestShellSuiteCIWiring` also proves the shard partition from the test functions that a `linux` build with the `pyintegration` tag declares: each test the allowlist selects runs in exactly one `go-python-integration` shard job, no shard job runs a test outside the allowlist, and each shard job selects at least one test. A test that only the `pyintegration` build declares must be in the allowlist or in `pyIntegrationCIExemptions` (`scheduler/update_sh_syntax_test.go`) with a written reason and an issue number (none today). An exemption must name a test that only the `pyintegration` build declares and must not be allowlisted.

**Format and layout checks.** These checks replace CLAUDE.md prose, so keep each one exact:
- The `go` job format step runs `gofmt -l` over every tracked Go file (`git ls-files '*.go'`, so `scripts/fixtures/ledger_fixture.go` too) and fails on any output or parse error. Run `gofmt -w` after Go edits.
- The `checks` job fails unless `git ls-files .github/scripts` is exactly `.github/scripts/test_workflow_logic.py`. Put any other script under `scripts/`.
---
LLM: Opus 5.5 | high | Harness: Claude Code

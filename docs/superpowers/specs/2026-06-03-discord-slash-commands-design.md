# Discord Slash Commands for go-trader — Design

**Issue:** [#212 — Add in-app skills for common actions](https://github.com/richkuo/go-trader/issues/212)
**Date:** 2026-06-03
**Status:** Approved (pending implementation plan)

## Context & redirection

Issue #212 originally proposed implementing ~18 common operator actions as Claude Code
`SKILL.md` sections with OpenClaw natural-language intent routing. The chosen direction
instead implements them as **real Discord slash commands** (application commands /
interactions) handled by the in-process Discord bot.

The codebase already runs a Discord bot via `github.com/bwmarrin/discordgo`
(`scheduler/discord.go`): it opens a gateway with `IntentsDirectMessages` and does
two-way DM confirmation (`AskDM` / `messageCreate`). There are **no** slash/application
commands today. This feature adds them.

The bot runs in the same process as the HTTP `StatusServer`, which carries the live
`state *AppState`, `mu *sync.RWMutex`, `stateDB *StateDB`, strategy configs, regime
config, and the live price rails. Read-only commands therefore read in-process state
directly — no HTTP round-trips, no status token / port juggling.

## Scope (first cut)

**Read-only** — usable in the guild *and* in DMs, by anyone:
`/status`, `/health`, `/positions`, `/pnl`, `/leaderboard`, `/circuit-breakers`,
`/dead-strategies`, `/correlation`, `/logs`.

**Ops** — owner-only *and* DM-only: `/restart`, `/backtest`.

### Out of scope (deferred to a follow-up)

All mutating commands from the issue: `/config show`, `/config set`, `/add-strategy`,
`/remove-strategy`, `/add-platform`, `/paper-to-live`. These require config-write
safety, restart orchestration, and stronger destructive-action auth — a separate spec.

## Command → data-source map

| Command | Auth | Data source | Output |
|---|---|---|---|
| `/status` | open | reuse `StatusServer` status aggregation + `formatStatusLine(cash,posCount,value,trades,regime)` | live cash / position count / portfolio value / trades / regime |
| `/health` | open | `state.CycleCount`, `state.LastCycle`, mirrors `handleHealth` | "running, last cycle Xs ago", cycle count |
| `/positions` | open | iterate `state.Strategies[]` positions under `mu.RLock` | open positions grouped by platform |
| `/pnl` | open | `StateDB.LifetimeTradeStatsAll` + portfolio value | total / per-platform / per-strategy P&L |
| `/leaderboard` | open | `BuildLeaderboardSummary(...)` | ranked strategies; optional `top` int option |
| `/circuit-breakers` | open | `RiskState.CircuitBreaker` + `CircuitBreakerUntil` per strategy; `PortfolioRisk` kill-switch | active breakers with until-time; kill-switch state |
| `/dead-strategies` | open | strategies whose lifetime trade count (`#T`) == 0 | inactive strategy list |
| `/correlation` | open | `state.CorrelationSnapshot` | correlation / concentration warnings |
| `/logs` | open | `journalctl -u go-trader -n <N> --no-pager` | last N journal lines; `n` int option, default 50 |
| `/restart` | owner+DM | `systemctl restart go-trader` | ACK "restarting…"; process dies and returns |
| `/backtest` | owner+DM | `run_backtest.py --strategy <s> --symbol <sym> --timeframe <tf> --mode single` | summary embed + full stdout attached as a file |

`/backtest` options: `strategy` (string, required), `symbol` (string, required, e.g.
`BTC/USDT`), `timeframe` (string, optional, default `1h`). `mode` is fixed to `single`.

## Architecture

### New file: `scheduler/discord_commands.go`

Owns, in one focused unit:

- **Command definitions** — `slashCommands() []*discordgo.ApplicationCommand`. Ops
  commands declared with `Contexts: []discordgo.InteractionContextType{discordgo.InteractionContextBotDM}`
  so they do not even appear in guild command pickers.
- **Registration** — `DiscordNotifier.RegisterSlashCommands(ss *StatusServer, cfg *Config)`:
  stores the references it needs (primarily `*StatusServer`, which already aggregates
  state / mu / stateDB / strategies / regime / price rails, plus `ownerID` already on
  the notifier), adds the `interactionCreate` gateway handler, and bulk-registers the
  commands **globally** via `session.ApplicationCommandBulkOverwrite(appID, "", cmds)`.
  Global registration covers every guild the bot is in *and* DMs (the cost is up to
  ~1h propagation on first deploy / command-shape changes — acceptable).
- **Dispatch + auth** — `interactionCreate` routes by command name through
  `authorizeCommand` (below), then to a per-command builder.
- **Response builders** — pure functions returning formatted strings / embeds.

### Wiring (`main.go`)

`RegisterSlashCommands` is called **after** both the notifier and the `StatusServer`
exist. The Discord backend is reached through `MultiNotifier` via a new nil-safe
accessor (returns the `*DiscordNotifier` or nil). If Discord is not configured, the
whole step is a no-op.

`DiscordNotifier` intents stay as-is: interaction events are delivered over the gateway
regardless of message-content/DM intents, so no intent change is required.

## Response flow

- **Fast read-only commands** → immediate `session.InteractionRespond` with a
  `ChannelMessageWithSource` response. Output rendered as a code block / embed and
  truncated to Discord's 2000-char message limit (reuse the existing truncation used by
  `SendMessage`).
- **Slow ops** (`/backtest`, `/restart`) → **deferred response**: ACK within 3s with
  `InteractionResponseDeferredChannelMessageWithSource`, run the work, then deliver the
  result via `FollowupMessageCreate` (or edit the deferred response).
  - `/restart`: the ACK says "restarting…"; the process is replaced by systemd, so the
    final confirmation naturally comes from the restarted instance's normal startup,
    not a bot follow-up. Best-effort only.
  - `/backtest`: parse `run_backtest.py` stdout into a summary (total return, Sharpe,
    trade count, win rate, max drawdown) **and** attach the full stdout as a file.

## Authorization (pure, testable)

```
authorizeCommand(name, invokerID, guildID, ownerID string) (ok bool, reason string)
```

- Read-only command set → always `ok = true`.
- Ops command set → `ok = true` only when `invokerID == ownerID && guildID == ""`
  (DM context). Otherwise `ok = false` with reason "owner-DM only" / "not authorized".
- Unauthorized invocations get an **ephemeral** interaction reply (visible only to the
  invoker).

The handler enforces this even though ops commands are also `Contexts`-restricted to DM
— defense in depth.

## Safety & failure handling

- Slash-command registration failure at startup is **non-fatal**: log + owner DM, the
  bot keeps running. It does not touch the `ExitProbeFailure` (exit 78) startup-probe
  path.
- Commands are **not** deleted on shutdown (deleting them would force re-propagation
  delay on the next start). They persist across restarts; the bulk-overwrite on startup
  keeps them in sync.
- `/backtest` runs through `runPython` (read-only backtest, no trading side effects) and
  respects the existing `scriptTimeout` and `pythonSemaphore`.
- `/logs` shells out to `journalctl`; the bot runs as the service user with journal read
  access.
- **Setup note** (docs / `SKILL.md`): the bot must be invited to the guild with the
  `applications.commands` OAuth scope. No code impact, but required for commands to
  appear.

## Testing

- Pure response builders (`formatPositionsResponse`, `formatHealthResponse`,
  `formatPnLResponse`, `formatCircuitBreakersResponse`, `formatDeadStrategiesResponse`,
  `formatCorrelationResponse`, etc.) unit-tested against synthetic `AppState` — no
  Discord session, no subprocess.
- `authorizeCommand` table-driven tests (each command × owner/non-owner × guild/DM).
- `/backtest` output parser tested against a captured `run_backtest.py` sample.

No Go test depends on spawning Python or opening a Discord gateway.

## Documentation

Update `SKILL.md` to document the available slash commands, their auth model
(read-only = guild+DM for anyone; ops = owner-DM only), and the one-time
`applications.commands` invite-scope requirement.

# go-trader Consolidated Issue Master List

Cross-referenced original review (31 issues) with second audit (59 issues). Each issue categorized as **Bug**, **Security**, **Feature** (missing capability), or **Other** (code quality, process, strategy performance).

**Legend:** `[ORIG]` = original review only, `[NEW]` = from second audit, verified valid, `[BOTH]` = in both lists. Severity: 1=trivial … 5=critical

---

## Bugs


| #   | Issue                                                                                                                                            | Source | Fixed?  | Severity |
| --- | ------------------------------------------------------------------------------------------------------------------------------------------------ | ------ | ------- | -------- |
| 1   | **RecordTradeResult never called** — ConsecutiveLosses always 0, win/loss counters inert, consecutive-loss circuit breaker dead code             | [ORIG] | YES     | 4        |
| 2   | **Double trade counting** — `totalTrades += trades` appears twice per options strategy                                                           | [ORIG] | YES     | 2        |
| 3   | **Go append slice mutation** — `args := append(sc.Args, posJSON)` could mutate backing array                                                     | [ORIG] | YES     | 3        |
| 4   | **Butterfly quantity ignored** — Middle leg quantity=2 from Python ignored; Go hardcodes Quantity:1.0                                            | [ORIG] | needs review | 4        |
| 5   | **DTE uses local time** — `time.Until(expiry)` mixes local Now() with UTC expiry date. 1-day error on non-UTC servers                            | [ORIG] | YES     | 4        |
| 6   | **Protective puts always fire** — Returns signal=1 every cycle regardless of existing holdings                                                   | [ORIG] | YES     | 5        |
| 7   | **Covered calls always fire** — Returns signal=-1 every cycle                                                                                    | [ORIG] | YES     | 5        |
| 8   | **Non-standard IV rank formula** — Was `(recent_vol / hist_vol) * 50` instead of rolling HV percentile                                           | [BOTH] | YES     | 4        |
| 9   | **Status endpoint blocks on subprocess** — `/status` holds RLock during 30s Python FetchPrices call                                              | [ORIG] | YES     | 3        |
| 10  | **State file grows unbounded** — TradeHistory never truncated                                                                                    | [ORIG] | YES     | 4        |
| 11  | **Expired options never cleaned up** — Remain in positions map indefinitely                                                                      | [ORIG] | YES     | 4        |
| 12  | **Inconsistent mutex usage** — CheckRisk, processSpot, processOptions modify state without holding lock. HTTP server reads mid-write             | [BOTH] | YES     | 5        |
| 13  | **Division by zero risk** — `qty := budget / execPrice` in portfolio.go has no guard on execPrice > 0                                            | [NEW]  | YES     | 4        |
| 14  | **Zero-premium trades execute** — If both PremiumUSD and Premium are 0, options trades execute at zero cost                                      | [NEW]  | YES     | 5        |
| 15  | **Python sys.exit(0) on errors** — check_strategy.py exits 0 on exception. Go sees success exit code, errors masked                              | [NEW]  | YES     | 5        |
| 16  | **Phantom circuit breaker** — Drawdown CB triggers with 0 trades if mark-to-market drops portfolio below peak                                    | [NEW]  | YES     | 3        |
| 17  | **State save failure continues execution** — If state.json write fails, scheduler keeps trading. Trades lost on restart                          | [NEW]  | YES     | 5        |
| 18  | **Hardcoded Greeks** — All strategies return static delta/gamma/theta/vega. Portfolio Greeks tracking is decorative                              | [BOTH] | YES     | 2        |
| 19  | **Hardcoded premiums** — Option premiums are hardcoded percentages unrelated to live IV/market quotes. Paper P&L unreliable                      | [ORIG] | YES     | 4        |
| 20  | **Greeks not updated from Deribit** — Stale from entry time, never refreshed despite Deribit ticker providing live Greeks                        | [BOTH] | YES     | 3        |
| 21  | **Pairs strategies broken** — Requires close_b column for second asset. Data fetcher provides single asset only. Degrades to self-mean-reversion | [NEW]  | YES     | 4        |
| 22  | **Wheel strategy incomplete** — Phase 2 (covered calls after assignment) described but never implemented                                         | [ORIG] | YES     | 3        |
| 23  | **Wheel collateral model broken** — Sells puts with strike >> allocated capital. No margin enforcement                                           | [NEW]  | YES     | 5        |
| 24  | **Logger ignores LogDir config** — NewLogManager discards argument, all output to stdout                                                         | [BOTH] | YES     | 2        |
| 25  | **Deribit expiry fallback too loose** — Could match expiry weeks away from target                                                                | [NEW]  | YES     | 4        |
| 26  | **Python data_fetcher infinite retry on rate limit** — `continue` loop on RateLimitExceeded with no max retries                                  | [NEW]  | YES     | 4        |
| 27  | **Daily PnL reset is naive** — Resets on first check after midnight UTC, breaks if check missed at boundary                                      | [NEW]  | YES     | 2        |
| 28  | **No expiry/assignment modeling** — Sold ITM options treated as worthless at expiry instead of modeling assignment                               | [NEW]  | YES     | 4        |
| 29  | **Subprocess orphan risk** — No process group management, no concurrency limit on Python processes                                               | [NEW]  | YES     | 3        |
| 30  | **Global state in Python Order class** — `_id_counter` class variable not thread-safe, resets on restart (in unused exchange_adapter.py)         | [NEW]  | needs review | 1        |


## Security


| #   | Issue                                                                                                 | Source | Fixed? | Severity |
| --- | ----------------------------------------------------------------------------------------------------- | ------ | ------ | -------- |
| 31  | **Service runs as root** — No User= directive, Python scripts execute as root                         | [BOTH] | YES    | 4        |
| 32  | **Compiled binary in repo** — 8.5MB go-trader binary was tracked in git                               | [ORIG] | YES    | 2        |
| 33  | **Discord token storage** — In config.json (gitignored) or env var. Env var preferred                 | [BOTH] | YES    | 3        |
| 34  | **Script path not validated** — Strategy Script field passed directly to exec.Command                 | [ORIG] | YES    | 5        |
| 35  | **Positions passed as CLI args** — Visible in /proc/[pid]/cmdline, could hit ARG_MAX                  | [ORIG] | YES    | 3        |
| 36  | **No config validation** — No validation for negative capital, invalid drawdown %, empty script paths | [ORIG] | YES    | 4        |
| 37  | **State file permissions 0644** — World-readable. Should be 0600                                      | [NEW]  | YES    | 3        |
| 38  | **HTTP status endpoint no auth** — Optional bearer auth exists, but endpoint remains open if token unset | [NEW]  | needs review | 2        |
| 39  | **No state validation on load** — No checks for negative balances, invalid positions, corrupted data  | [NEW]  | YES    | 4        |


## Feature (missing capability)


| #   | Issue                                                                                                 | Source | Fixed? | Severity |
| --- | ----------------------------------------------------------------------------------------------------- | ------ | ------ | -------- |
| 40  | **Health endpoint was always OK** — No liveness check                                                 | [ORIG] | YES    | 2        |
| 41  | **Circuit breakers don't close positions** — CB only pauses new trades; bleeding positions stay open  | [NEW]  | YES    | 5        |
| 42  | **No portfolio-level kill switch** — No aggregate drawdown limit, no global notional cap              | [NEW]  | NO     | 5        |
| 43  | **No notional exposure tracking** — Options notional not tracked or limited. Hidden leverage          | [NEW]  | NO     | 5        |
| 44  | **No stop-loss for sold options** — Without theta_harvest config, sold options have no automatic exit | [NEW]  | NO     | 5        |
| 45  | **No correlation tracking** — Multiple strategies selling BTC puts simultaneously. All lose together  | [NEW]  | NO     | 4        |
| 46  | **No retry logic** — All external calls fail once with no retry                                       | [ORIG] | NO     | 3        |
| 47  | **Deribit rate limiting** — Sequential API calls per position, no backoff                             | [BOTH] | NO     | 4        |
| 48  | **No error alerting** — Failures logged to stdout only, no Discord notification                       | [ORIG] | NO     | 3        |
| 49  | **No circuit breaker Discord alerts** — CB triggers are silent                                        | [NEW]  | NO     | 3        |
| 50  | **Price fetch failure doesn't halt execution** — Strategies run with stale/zero prices                | [NEW]  | NO     | 5        |
| 51  | **No dormant strategy alerting** — Strategies at 0 trades for days go undetected                      | [NEW]  | NO     | 2        |
| 52  | **No lastRun persistence** — On restart all strategies fire immediately                               | [ORIG] | NO     | 3        |
| 53  | **State save only per-cycle** — Crash mid-cycle loses all trades from that cycle                      | [ORIG] | NO     | 4        |
| 54  | **No state file backups** — Corruption or deletion is unrecoverable                                   | [NEW]  | NO     | 4        |
| 55  | **No backtest-to-live validation** — No mechanism to validate strategy params before deployment       | [ORIG] | NO     | 3        |
| 56  | **Theta harvesting not enabled by default** — If not in config.json, sold options have no early exit  | [NEW]  | YES    | 4        |
| 57  | **Discord messages not rate-limited** — Could trigger Discord API rate limits                         | [NEW]  | NO     | 2        |
| 58  | **No fee modeling for maker orders** — Assumes all orders are taker                                   | [NEW]  | NO     | 2        |


## Other (code quality, architecture, process, strategy performance)


| #   | Issue                                                                                                   | Source | Fixed?  | Severity |
| --- | ------------------------------------------------------------------------------------------------------- | ------ | ------- | -------- |
| 59  | **Unused Python modules** — core/risk_manager.py and exchange_adapter.py never imported by Go scheduler | [BOTH] | YES     | 1        |
| 60  | **check_options.py 700+ line mega-file** — 6 strategy functions with duplicated logic                   | [NEW]  | YES     | 2        |
| 61  | **Magic numbers throughout Go code** — 0.95 budget multiplier, hardcoded fee rates, 24h CB duration     | [NEW]  | NO      | 2        |
| 62  | **main.go main() is 200+ lines** — Needs extraction into smaller functions                              | [NEW]  | NO      | 1        |
| 63  | **No interfaces for external deps** — Concrete types, impossible to mock                                | [NEW]  | NO      | 2        |
| 64  | **No structured logging** — Printf-style strings, hard to parse/query                                   | [NEW]  | NO      | 2        |
| 65  | **No correlation IDs across Go to Python** — Subprocess logs disconnected from scheduler                | [NEW]  | NO      | 2        |
| 66  | **No log rotation** — No built-in rotation mechanism                                                    | [NEW]  | NO      | 2        |
| 67  | **Single price source** — All prices from Binance US, no cross-validation or failover                   | [NEW]  | NO      | 4        |
| 68  | **No deployment automation** — Manual build + restart, no CI/CD                                         | [NEW]  | NO      | 2        |
| 69  | **No staging environment** — Changes go straight to production                                          | [NEW]  | NO      | 3        |
| 70  | **No state reconstruction script** — If state.json deleted, no way to rebuild                           | [NEW]  | NO      | 3        |
| 71  | **Momentum ROC threshold too high** — 5% threshold on 14-period 4h candles fires very rarely            | [NEW]  | NO      | 3        |
| 72  | **MACD-BTC definitively bad** — 17 trades, -4.7% vs BTC -0.93%, confirmed negative alpha                | [NEW]  | NO      | 4        |
| 73  | **Most spot strategies fail to beat buy-and-hold**                                                      | [NEW]  | NO      | 3        |
| 74  | **Theta harvesting 70% parameter overfitted** — Live performance 52x better than backtest               | [NEW]  | NO      | 4        |


---

## Issues From Second Audit That Are NOT VALID


| User# | Claim                                  | Verdict                                   |
| ----- | -------------------------------------- | ----------------------------------------- |
| 11    | Logrotate pointing to wrong path       | No logrotate config exists in the repo    |
| 31    | Option close nil pointer risk          | Go range over map never yields nil values |
| 35    | Paper mode doesn't validate stop_price | Applies to unused exchange_adapter.py     |


---

## Summary


| Category  | Total  | Fixed  | Unfixed                |
| --------- | ------ | ------ | ---------------------- |
| Bug       | 30     | 28     | 2 (needs review)       |
| Security  | 9      | 8      | 1 (needs review)       |
| Feature   | 19     | 2      | 17                     |
| Other     | 16     | 2      | 14                     |
| **Total** | **74** | **40** | **34**                 |



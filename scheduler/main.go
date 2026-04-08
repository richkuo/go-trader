package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "init" {
		os.Exit(runInit(os.Args[2:]))
	}

	configPath := flag.String("config", "scheduler/config.json", "Path to config file")
	once := flag.Bool("once", false, "Run one cycle and exit")
	summary := flag.String("summary", "", "Post snapshot summary for the specified channel (e.g., hyperliquid, spot, options) and exit")
	leaderboard := flag.Bool("leaderboard", false, "Post pre-computed daily leaderboard and exit")
	flag.Parse()

	// Load config
	cfg, err := LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Loaded config: %d strategies, interval=%ds\n", len(cfg.Strategies), cfg.IntervalSeconds)

	// Load or initialize state (platform-aware when platforms are configured).
	state, err := LoadPlatformStates(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load state: %v\n", err)
		os.Exit(1)
	}
	ValidateState(state)

	// #87: Resolve capital_pct at startup so initial state gets the right capital.
	resolveCapitalPct(cfg.Strategies)

	// Initialize new strategies and sync config values for existing ones
	for i := range cfg.Strategies {
		sc := &cfg.Strategies[i]
		// For live Hyperliquid strategies without capital_pct, override capital with the real wallet balance.
		if sc.CapitalPct == 0 {
			syncHyperliquidLiveCapital(sc)
		}
		if s, exists := state.Strategies[sc.ID]; !exists {
			state.Strategies[sc.ID] = NewStrategyState(*sc)
			fmt.Printf("  Initialized strategy: %s (type=%s, capital=$%.0f)\n", sc.ID, sc.Type, sc.Capital)
		} else {
			// Sync config → state (config is source of truth).
			if s.RiskState.MaxDrawdownPct != sc.MaxDrawdownPct {
				fmt.Printf("  Updated %s max_drawdown_pct: %.0f%% → %.0f%%\n", sc.ID, s.RiskState.MaxDrawdownPct, sc.MaxDrawdownPct)
				s.RiskState.MaxDrawdownPct = sc.MaxDrawdownPct
			}
			s.Platform = sc.Platform
		}
	}

	// Prune strategies from state that are no longer in config
	configIDs := make(map[string]bool)
	for _, sc := range cfg.Strategies {
		configIDs[sc.ID] = true
	}
	for id := range state.Strategies {
		if !configIDs[id] {
			delete(state.Strategies, id)
			fmt.Printf("  Pruned stale strategy: %s\n", id)
		}
	}

	// #42: Initialize portfolio peak from sum of capitals on first run.
	// For shared wallet strategies (capital_pct > 0), count each platform wallet
	// once using the actual balance (Capital / CapitalPct) to avoid double-counting.
	if state.PortfolioRisk.PeakValue == 0 {
		total := 0.0
		walletCounted := make(map[string]bool)
		for _, sc := range cfg.Strategies {
			if sc.CapitalPct > 0 {
				if !walletCounted[sc.Platform] {
					total += sc.Capital / sc.CapitalPct
					walletCounted[sc.Platform] = true
				}
			} else {
				total += sc.Capital
			}
		}
		state.PortfolioRisk.PeakValue = total
		fmt.Printf("  Portfolio peak initialized: $%.0f\n", total)
	}

	// Setup logging
	logMgr, err := NewLogManager(cfg.LogDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to setup logging: %v\n", err)
		os.Exit(1)
	}
	defer logMgr.Close()

	// Mutex for state access (HTTP server reads)
	var mu sync.RWMutex

	// Start HTTP status server
	server := NewStatusServer(state, &mu, cfg.StatusToken, cfg.Strategies)
	server.Start(8099)

	// Graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	stopCh := make(chan struct{})
	go func() {
		sig := <-sigCh
		fmt.Printf("\nReceived %s, saving state and shutting down...\n", sig)
		mu.Lock()
		if err := SavePlatformStates(state, cfg); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to save state: %v\n", err)
		} else {
			fmt.Println("State saved successfully.")
		}
		mu.Unlock()
		close(stopCh)
	}()

	// Initialize notification backends (Discord and/or Telegram).
	var backends []notifierBackend

	if cfg.Discord.Enabled && cfg.Discord.Token != "" {
		discord, err := NewDiscordNotifier(cfg.Discord.Token, cfg.Discord.OwnerID)
		if err != nil {
			fmt.Printf("[WARN] Discord init failed: %v — continuing without Discord\n", err)
		} else {
			fmt.Printf("Discord gateway connected (%d channels", len(cfg.Discord.Channels))
			if cfg.Discord.OwnerID != "" {
				fmt.Printf(", DM owner enabled")
			}
			fmt.Println(")")
			backends = append(backends, notifierBackend{
				notifier:           discord,
				channels:           cfg.Discord.Channels,
				ownerID:            cfg.Discord.OwnerID,
				dmPaperTrades:      cfg.Discord.DMPaperTrades,
				dmLiveTrades:       cfg.Discord.DMLiveTrades,
				channelPaperTrades: cfg.Discord.ChannelPaperTrades,
				channelLiveTrades:  cfg.Discord.ChannelLiveTrades,
			})
			defer discord.Close()
		}
	}

	if cfg.Telegram.Enabled && cfg.Telegram.BotToken != "" {
		tg, err := NewTelegramNotifier(cfg.Telegram.BotToken, cfg.Telegram.OwnerChatID)
		if err != nil {
			fmt.Printf("[WARN] Telegram init failed: %v — continuing without Telegram\n", err)
		} else {
			fmt.Printf("Telegram bot connected (%d channels", len(cfg.Telegram.Channels))
			if cfg.Telegram.OwnerChatID != "" {
				fmt.Printf(", DM owner enabled")
			}
			fmt.Println(")")
			backends = append(backends, notifierBackend{
				notifier:           tg,
				channels:           cfg.Telegram.Channels,
				ownerID:            cfg.Telegram.OwnerChatID,
				dmPaperTrades:      cfg.Telegram.DMPaperTrades,
				dmLiveTrades:       cfg.Telegram.DMLiveTrades,
				channelPaperTrades: cfg.Telegram.ChannelPaperTrades,
				channelLiveTrades:  cfg.Telegram.ChannelLiveTrades,
				plainText:          true,
			})
			defer tg.Close()
		}
	}

	notifier := NewMultiNotifier(backends...)
	fmt.Printf("Notification backends: %d active\n", notifier.BackendCount())

	// -summary mode: post snapshot summary for the specified channel and exit.
	// Checked early since it only needs config, state, and notifier — avoids
	// launching the config-migration goroutine, update checks, and pricers
	// that would be hard-killed by os.Exit.
	if *summary != "" {
		runSummaryAndExit(*summary, cfg, state, notifier)
	}

	// -leaderboard mode: post pre-computed leaderboard and exit.
	// Reads leaderboard.json (written each cycle) and posts to Discord — no
	// price fetching or PnL computation needed, so it completes in seconds.
	if *leaderboard {
		if !notifier.HasBackends() {
			fmt.Fprintf(os.Stderr, "No notification backends configured\n")
			os.Exit(1)
		}
		if err := PostLeaderboard(cfg, notifier); err != nil {
			fmt.Fprintf(os.Stderr, "Leaderboard post failed: %v\n", err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	// Config migration: DM owner about new fields if config is behind current version.
	if cfg.ConfigVersion < CurrentConfigVersion {
		go runConfigMigrationDM(cfg, notifier, *configPath)
	}

	// Track the last remote hash we notified about to avoid re-notifying on every cycle.
	var lastNotifiedHash string

	// Check for updates on startup (best-effort, non-blocking).
	if cfg.AutoUpdate != "off" {
		checkForUpdates(cfg, notifier, &lastNotifiedHash, &mu, state)
	}

	// Platform pricers: Deribit uses live API; IBKR uses Black-Scholes with cached spot prices.
	deribitPricer := NewDeribitPricer()
	fmt.Println("Option pricers ready (deribit: live API, ibkr: Black-Scholes)")

	// Track last-run time per strategy for per-strategy intervals
	lastRun := make(map[string]time.Time)

	// Determine tick interval: GCD of all strategy intervals, min 60s
	tickSeconds := cfg.IntervalSeconds
	for _, sc := range cfg.Strategies {
		si := sc.IntervalSeconds
		if si <= 0 {
			si = cfg.IntervalSeconds
		}
		if si < tickSeconds {
			tickSeconds = si
		}
	}
	if tickSeconds < 60 {
		tickSeconds = 60
	}
	fmt.Printf("Tick interval: %ds (strategies have individual intervals)\n", tickSeconds)

	// Cycles per day, used for "daily" update check mode.
	dailyCycles := (24 * 3600) / tickSeconds
	if dailyCycles < 1 {
		dailyCycles = 1
	}

	saveFailures := 0
	resetGoroutineRunning := false

	var top10Freq time.Duration
	if cfg.HyperliquidTop10Freq != "" {
		top10Freq, _ = time.ParseDuration(cfg.HyperliquidTop10Freq)
	}

	// Main loop
	for {
		cycleStart := time.Now()
		mu.Lock()
		state.CycleCount++
		cycle := state.CycleCount
		mu.Unlock()
		totalTrades := 0
		channelTrades := make(map[string]int)
		channelTradeDetails := make(map[string][]string)

		// #87: Resolve capital_pct → capital for strategies with dynamic sizing.
		// Must run on cfg.Strategies (not dueStrategies) so resolved capital persists
		// across cycles and is picked up by the value-copies in dueStrategies.
		resolveCapitalPct(cfg.Strategies)

		// Determine which strategies are due this tick
		dueStrategies := make([]StrategyConfig, 0)
		for _, sc := range cfg.Strategies {
			// #100: Skip strategies where capital_pct is set but capital resolved to $0
			// (balance fetch failed and no fallback capital configured).
			if shouldSkipZeroCapital(sc) {
				fmt.Printf("[ERROR] %s: capital_pct set but capital resolved to $0 — skipping\n", sc.ID)
				continue
			}
			interval := sc.IntervalSeconds
			if interval <= 0 {
				interval = cfg.IntervalSeconds
			}
			last, exists := lastRun[sc.ID]
			if !exists || time.Since(last) >= time.Duration(interval)*time.Second {
				dueStrategies = append(dueStrategies, sc)
			}
		}

		if len(dueStrategies) == 0 {
			// Nothing due, wait for next tick
			timer := time.NewTimer(time.Duration(tickSeconds) * time.Second)
			select {
			case <-timer.C:
				continue
			case <-stopCh:
				timer.Stop()
				fmt.Println("Shutdown complete.")
				return
			}
		}

		fmt.Printf("\n=== Cycle %d starting at %s (%d/%d strategies due) ===\n",
			cycle, cycleStart.UTC().Format("2006-01-02 15:04:05 UTC"),
			len(dueStrategies), len(cfg.Strategies))

		// Collect symbols that need prices
		symbolSet := make(map[string]bool)
		for _, sc := range cfg.Strategies {
			if sc.Type == "spot" && len(sc.Args) >= 2 {
				symbolSet[sc.Args[1]] = true
			}
		}
		symbols := make([]string, 0, len(symbolSet))
		for s := range symbolSet {
			symbols = append(symbols, s)
		}

		// Fetch current prices for portfolio valuation
		prices := make(map[string]float64)
		if len(symbols) > 0 {
			p, err := FetchPrices(symbols)
			if err != nil {
				fmt.Printf("[CRITICAL] Price fetch failed: %v — skipping cycle\n", err)
				continue
			}
			// Filter out any zero prices returned by the script
			for sym, price := range p {
				if price > 0 {
					prices[sym] = price
				} else {
					fmt.Printf("[WARN] Skipping zero price for %s\n", sym)
				}
			}
			if len(prices) == 0 {
				fmt.Printf("[CRITICAL] All prices are zero/missing — skipping cycle\n")
				continue
			}
			fmt.Printf("Prices: ")
			for sym, price := range prices {
				fmt.Printf("%s=$%.2f ", sym, price)
			}
			fmt.Println()
		}

		// Process only due strategies
		if saveFailures >= 3 {
			fmt.Println("[CRITICAL] State save failed 3x, skipping trades this cycle")
		} else {
			// #42: Portfolio-level risk check before running any strategy.
			killSwitchFired := false
			notionalBlocked := false
			mu.RLock()
			totalPV := 0.0
			for _, sc := range cfg.Strategies {
				if s, ok := state.Strategies[sc.ID]; ok {
					totalPV += PortfolioValue(s, prices)
				}
			}
			totalNotional := PortfolioNotional(state.Strategies, prices)
			mu.RUnlock()

			mu.Lock()
			portfolioAllowed, nb, portfolioWarning, portfolioReason := CheckPortfolioRisk(&state.PortfolioRisk, cfg.PortfolioRisk, totalPV, totalNotional)
			if !portfolioAllowed {
				killSwitchFired = true
				fmt.Printf("[CRITICAL] Portfolio kill switch: %s\n", portfolioReason)
				for _, sc := range cfg.Strategies {
					if s, ok := state.Strategies[sc.ID]; ok {
						forceCloseAllPositions(s, prices, nil)
					}
				}
			}
			notionalBlocked = nb
			if notionalBlocked {
				fmt.Printf("[WARN] %s\n", portfolioReason)
			}
			mu.Unlock()

			if killSwitchFired && notifier.HasBackends() {
				msg := fmt.Sprintf("**PORTFOLIO KILL SWITCH**\n%s\nAll positions force-closed. Manual reset required.", portfolioReason)
				notifier.SendToAllChannels(msg)
			}

			// Warning alert: drawdown approaching kill switch threshold.
			if portfolioWarning && notifier.HasBackends() {
				mu.Lock()
				addKillSwitchEvent(&state.PortfolioRisk, "warning", state.PortfolioRisk.CurrentDrawdownPct, totalPV, state.PortfolioRisk.PeakValue, portfolioReason)
				mu.Unlock()
				warnMsg := fmt.Sprintf("**PORTFOLIO WARNING**\n%s", portfolioReason)
				notifier.SendToAllChannels(warnMsg)
				notifier.SendOwnerDM(warnMsg)
				fmt.Printf("[WARN] %s\n", portfolioReason)
			}

			// Correlation tracking: compute per-asset directional exposure.
			var corrWarnings []string
			if cfg.Correlation != nil && cfg.Correlation.Enabled {
				mu.RLock()
				corrSnap := ComputeCorrelation(state.Strategies, cfg.Strategies, prices, cfg.Correlation)
				mu.RUnlock()
				corrWarnings = corrSnap.Warnings

				mu.Lock()
				state.CorrelationSnapshot = corrSnap
				mu.Unlock()
			}

			if len(corrWarnings) > 0 && notifier.HasBackends() {
				msg := "**CORRELATION WARNING**\n" + strings.Join(corrWarnings, "\n")
				notifier.SendToAllChannels(msg)
				notifier.SendOwnerDM(msg)
			}

			// Kill switch reset goroutine: prompt owner to reset via DM.
			if killSwitchFired && notifier.HasOwner() && !resetGoroutineRunning {
				resetGoroutineRunning = true
				go func() {
					defer func() { resetGoroutineRunning = false }()
					resp, err := notifier.AskOwnerDM("Kill switch active. Reply 'reset' to resume trading.", 30*time.Minute)
					if err != nil {
						fmt.Printf("[update] Kill switch reset DM timed out or failed: %v\n", err)
						return
					}
					if resp != "reset" {
						fmt.Printf("[update] Kill switch reset DM got unexpected reply: %q\n", resp)
						return
					}
					mu.Lock()
					state.PortfolioRisk.KillSwitchActive = false
					state.PortfolioRisk.KillSwitchAt = time.Time{}
					addKillSwitchEvent(&state.PortfolioRisk, "reset", state.PortfolioRisk.CurrentDrawdownPct, 0, state.PortfolioRisk.PeakValue, "manual reset via DM")
					if err := SavePlatformStates(state, cfg); err != nil {
						fmt.Printf("[CRITICAL] Failed to save state after kill switch reset: %v\n", err)
					}
					mu.Unlock()
					notifier.SendOwnerDM("Kill switch reset. Trading will resume next cycle.")
					fmt.Println("[update] Kill switch reset by owner via DM")
				}()
			}

			if !killSwitchFired {
				// Pre-phase: sync on-chain positions for all live HL strategies at once.
				var hlLiveStrategies []StrategyConfig
				for _, sc := range dueStrategies {
					if sc.Platform == "hyperliquid" && sc.Type == "perps" && hyperliquidIsLive(sc.Args) {
						hlLiveStrategies = append(hlLiveStrategies, sc)
					}
				}
				if len(hlLiveStrategies) > 0 {
					syncHyperliquidAccountPositions(hlLiveStrategies, state, &mu, logMgr)
				}

				for _, sc := range dueStrategies {
					stratState := state.Strategies[sc.ID]
					if stratState == nil {
						continue
					}

					logger, err := logMgr.GetStrategyLogger(sc.ID)
					if err != nil {
						fmt.Printf("[ERROR] Logger for %s: %v\n", sc.ID, err)
						continue
					}

					// Phase 1: RLock — read inputs needed for subprocess
					mu.RLock()
					pv := PortfolioValue(stratState, prices)
					var posJSON string
					if sc.Type == "options" {
						posJSON = EncodeAllPositionsJSON(stratState.OptionPositions, stratState.Positions)
					}
					var hlCash float64
					var hlPosQty float64
					if sc.Type == "perps" && hyperliquidIsLive(sc.Args) {
						hlCash = stratState.Cash
						if sym := hyperliquidSymbol(sc.Args); sym != "" {
							if pos, ok := stratState.Positions[sym]; ok {
								hlPosQty = pos.Quantity
							}
						}
					}
					var okxCash float64
					var okxPosQty float64
					if sc.Platform == "okx" && okxIsLive(sc.Args) {
						okxCash = stratState.Cash
						if sym := okxSymbol(sc.Args); sym != "" {
							if pos, ok := stratState.Positions[sym]; ok {
								okxPosQty = pos.Quantity
							}
						}
					}
					var rhCash float64
					var rhPosQty float64
					if sc.Platform == "robinhood" && robinhoodIsLive(sc.Args) {
						rhCash = stratState.Cash
						if sym := robinhoodSymbol(sc.Args); sym != "" {
							if pos, ok := stratState.Positions[sym]; ok {
								rhPosQty = pos.Quantity
							}
						}
					}
					var tsCash float64
					var tsContracts float64
					if sc.Type == "futures" && topstepIsLive(sc.Args) {
						tsCash = stratState.Cash
						if sym := topstepSymbol(sc.Args); sym != "" {
							if pos, ok := stratState.Positions[sym]; ok {
								tsContracts = pos.Quantity
							}
						}
					}
					mu.RUnlock()

					// Phase 2: Lock — CheckRisk (fast, no I/O)
					mu.Lock()
					allowed, reason := CheckRisk(stratState, pv, prices, logger)
					mu.Unlock()
					if !allowed {
						logger.Warn("Risk block: %s (portfolio=$%.2f)", reason, pv)
						logger.Close()
						lastRun[sc.ID] = time.Now()
						continue
					}

					// #42: Notional cap blocks new trades for this strategy.
					if notionalBlocked {
						logger.Warn("Notional cap exceeded — skipping new trades")
						logger.Close()
						lastRun[sc.ID] = time.Now()
						continue
					}

					// Phase 3 (no lock) + Phase 4 (Lock): subprocess then state mutation
					trades := 0
					var detail string
					switch sc.Type {
					case "spot":
						if sc.Platform == "okx" {
							if result, signalStr, price, ok := runOKXCheck(sc, prices, logger); ok {
								prices[result.Symbol] = price
								var execResult *OKXExecuteResult
								liveExecFailed := false
								if okxIsLive(sc.Args) && result.Signal != 0 {
									if er, ok2 := runOKXExecuteOrder(sc, result, price, okxCash, okxPosQty, logger); ok2 {
										execResult = er
									} else {
										liveExecFailed = true
									}
								}
								if !liveExecFailed {
									mu.Lock()
									trades, detail = executeOKXResult(sc, stratState, result, execResult, signalStr, price, logger)
									mu.Unlock()
								}
							}
						} else if sc.Platform == "robinhood" {
							if result, signalStr, price, ok := runRobinhoodCheck(sc, prices, logger); ok {
								prices[result.Symbol] = price
								var execResult *RobinhoodExecuteResult
								liveExecFailed := false
								if robinhoodIsLive(sc.Args) && result.Signal != 0 {
									if er, ok2 := runRobinhoodExecuteOrder(sc, result, price, rhCash, rhPosQty, logger); ok2 {
										execResult = er
									} else {
										liveExecFailed = true
									}
								}
								if !liveExecFailed {
									mu.Lock()
									trades, detail = executeRobinhoodResult(sc, stratState, result, execResult, signalStr, price, logger)
									mu.Unlock()
								}
							}
						} else if result, signalStr, price, ok := runSpotCheck(sc, prices, logger); ok {
							mu.Lock()
							trades, detail = executeSpotResult(sc, stratState, result, signalStr, price, logger)
							mu.Unlock()
						}
					case "options":
						if result, signalStr, ok := runOptionsCheck(sc, posJSON, logger); ok {
							mu.Lock()
							var harvestDetails []string
							trades, detail, harvestDetails = executeOptionsResult(sc, stratState, result, signalStr, logger)
							mu.Unlock()
							if chKey := notifier.resolveChannelKey(sc.Platform, sc.Type); chKey != "" {
								key := chKey + "|" + extractAsset(sc)
								channelTradeDetails[key] = append(channelTradeDetails[key], harvestDetails...)
							}
						}
					case "perps":
						if sc.Platform == "okx" {
							if result, signalStr, price, ok := runOKXCheck(sc, prices, logger); ok {
								prices[result.Symbol] = price
								var execResult *OKXExecuteResult
								liveExecFailed := false
								if okxIsLive(sc.Args) && result.Signal != 0 {
									if er, ok2 := runOKXExecuteOrder(sc, result, price, okxCash, okxPosQty, logger); ok2 {
										execResult = er
									} else {
										liveExecFailed = true
									}
								}
								if !liveExecFailed {
									mu.Lock()
									trades, detail = executeOKXResult(sc, stratState, result, execResult, signalStr, price, logger)
									mu.Unlock()
								}
							}
						} else if result, signalStr, price, ok := runHyperliquidCheck(sc, prices, logger); ok {
							prices[result.Symbol] = price
							var execResult *HyperliquidExecuteResult
							liveExecFailed := false
							if hyperliquidIsLive(sc.Args) && result.Signal != 0 {
								if er, ok2 := runHyperliquidExecuteOrder(sc, result, price, hlCash, hlPosQty, logger); ok2 {
									execResult = er
								} else {
									liveExecFailed = true
								}
							}
							if !liveExecFailed {
								mu.Lock()
								trades, detail = executeHyperliquidResult(sc, stratState, result, execResult, signalStr, price, logger)
								mu.Unlock()
							}
						}
					case "futures":
						if result, signalStr, price, ok := runTopStepCheck(sc, prices, logger); ok {
							prices[result.Symbol] = price
							var execResult *TopStepExecuteResult
							liveExecFailed := false
							if topstepIsLive(sc.Args) && result.Signal != 0 {
								if er, ok2 := runTopStepExecuteOrder(sc, result, price, tsCash, tsContracts, logger); ok2 {
									execResult = er
								} else {
									liveExecFailed = true
								}
							}
							if !liveExecFailed {
								mu.Lock()
								trades, detail = executeTopStepResult(sc, stratState, result, execResult, signalStr, price, logger)
								mu.Unlock()
							}
						}
					default:
						logger.Error("Unknown strategy type: %s", sc.Type)
					}
					if trades > 0 && detail != "" {
						if chKey := notifier.resolveChannelKey(sc.Platform, sc.Type); chKey != "" {
							channelTrades[chKey] += trades
							key := chKey + "|" + extractAsset(sc)
							channelTradeDetails[key] = append(channelTradeDetails[key], detail)
						}
						// DM trade alerts (Discord + Telegram)
						sendTradeAlerts(sc, stratState, trades, &mu, notifier)
					}

					totalTrades += trades

					// Phase 5: mark option positions with live prices (platform-aware).
					mu.RLock()
					markReqs := collectMarkRequests(stratState)
					mu.RUnlock()
					if len(markReqs) > 0 {
						var pricer OptionPricer
						if sc.Platform == "ibkr" {
							pricer = NewIBKRPricer(prices)
						} else {
							pricer = deribitPricer // also used for OKX options
						}
						markResults := fetchMarkPrices(markReqs, pricer, logger)
						mu.Lock()
						applyMarkResults(stratState, markResults, logger)
						mu.Unlock()
					}

					// Phase 6: RLock — status log
					mu.RLock()
					pv = PortfolioValue(stratState, prices)
					posCount := len(stratState.Positions) + len(stratState.OptionPositions)
					cash := stratState.Cash
					mu.RUnlock()

					logger.Info("Status: cash=$%.2f | positions=%d | value=$%.2f | trades=%d",
						cash, posCount, pv, trades)

					logger.Close()
					lastRun[sc.ID] = time.Now()
				}
			} // end if !killSwitchFired
		}

		// Calculate total portfolio value and per-channel values/strategies.
		// Group by logical channel key (platform or type) so summaries work with any backend.
		mu.RLock()
		totalValue := 0.0
		channelValue := make(map[string]float64)
		channelStrats := make(map[string][]StrategyConfig)
		for _, sc := range cfg.Strategies {
			if s, ok := state.Strategies[sc.ID]; ok {
				pv := PortfolioValue(s, prices)
				totalValue += pv
				if chKey := notifier.resolveChannelKey(sc.Platform, sc.Type); chKey != "" {
					channelValue[chKey] += pv
					channelStrats[chKey] = append(channelStrats[chKey], sc)
				}
			}
		}
		mu.RUnlock()

		elapsed := time.Since(cycleStart)
		logMgr.LogSummary(cycle, elapsed, len(dueStrategies), totalTrades, totalValue)

		// Notification — one message per channel per asset, sent to all backends.
		if notifier.HasBackends() {
			mu.RLock()
			for chKey, chStrats := range channelStrats {
				// Only post if at least one due strategy maps to this channel key.
				chRan := false
				for _, sc := range dueStrategies {
					if notifier.resolveChannelKey(sc.Platform, sc.Type) == chKey {
						chRan = true
						break
					}
				}
				if !chRan {
					continue
				}
				chTrades := channelTrades[chKey]
				// Options/perps/futures: post every run. Spot: hourly or on trade.
				// (cycle-1)%12==0 fires at cycles 1,13,25... so first summary posts on startup.
				if !isOptionsType(chStrats) && !isFuturesType(chStrats) && !isPerpsType(chStrats) && chTrades == 0 && (cycle-1)%12 != 0 {
					continue
				}
				assetGroups, assetKeys := groupByAsset(chStrats)
				if len(assetKeys) <= 1 {
					// Single asset (or none) → backwards-compatible single message without asset label.
					detailKey := chKey + "|"
					if len(assetKeys) == 1 {
						detailKey = chKey + "|" + assetKeys[0]
					}
					chDetails := channelTradeDetails[detailKey]
					chValue := channelValue[chKey]
					msg := FormatCategorySummary(cycle, elapsed, len(dueStrategies), chTrades, chValue, prices, chDetails, chStrats, state, chKey, "")
					notifier.SendToChannel(chKey, chKey, msg)
				} else {
					// Multiple assets → one message per asset.
					for _, asset := range assetKeys {
						assetStrats := assetGroups[asset]
						assetDetails := channelTradeDetails[chKey+"|"+asset]
						assetValue := 0.0
						for _, sc := range assetStrats {
							if s, ok := state.Strategies[sc.ID]; ok {
								assetValue += PortfolioValue(s, prices)
							}
						}
						assetTrades := len(assetDetails)
						msg := FormatCategorySummary(cycle, elapsed, len(dueStrategies), assetTrades, assetValue, prices, assetDetails, assetStrats, state, chKey, asset)
						notifier.SendToChannel(chKey, chKey, msg)
					}
				}
			}
			mu.RUnlock()
		}

		// Save state after each cycle
		mu.Lock()
		state.LastCycle = time.Now().UTC()
		// Pre-compute leaderboard data so the cron job can post without computation.
		if err := PrecomputeLeaderboard(cfg, state, prices); err != nil {
			fmt.Printf("[WARN] Leaderboard pre-compute failed: %v\n", err)
		}

		// Periodic hyperliquid top-10 summary (#176).
		var top10Msg string
		if top10Freq > 0 && time.Since(state.LastTop10Summary) >= top10Freq {
			top10Msg = FormatHyperliquidTop10(cfg, state, prices)
			if top10Msg != "" {
				state.LastTop10Summary = time.Now().UTC()
			}
		}

		if err := SavePlatformStates(state, cfg); err != nil {
			saveFailures++
			fmt.Printf("[CRITICAL] Save state failed (%d/3): %v\n", saveFailures, err)
		} else {
			saveFailures = 0
		}
		// Pre-compute leaderboard data so the cron job can post without computation.
		if err := PrecomputeLeaderboard(cfg, state, prices); err != nil {
			fmt.Printf("[WARN] Leaderboard pre-compute failed: %v\n", err)
		}

		// #175: Decide whether to auto-post daily leaderboard (check inside lock).
		var postLeaderboard bool
		if h, m, ok := ParseLeaderboardPostTime(cfg.LeaderboardPostTime); ok && notifier.HasBackends() {
			now := time.Now().UTC()
			today := now.Format("2006-01-02")
			targetMinute := h*60 + m
			currentMinute := now.Hour()*60 + now.Minute()
			if currentMinute >= targetMinute && state.LastLeaderboardPostDate != today {
				postLeaderboard = true
			}
		}
		mu.Unlock()

		// Post top-10 outside the lock to avoid holding mu during I/O.
		// Route to dedicated leaderboard channel; falls back to platform channel.
		if top10Msg != "" {
			notifier.SendToChannel("hyperliquid-leaderboard", "hyperliquid", top10Msg)
			fmt.Println("[top10] Posted hyperliquid top-10 summary")
		}

		// Post leaderboard outside the lock to avoid holding mu during I/O.
		if postLeaderboard {
			fmt.Printf("[leaderboard] Auto-posting daily leaderboard (configured time: %s UTC)\n", cfg.LeaderboardPostTime)
			if err := PostLeaderboard(cfg, notifier); err != nil {
				fmt.Printf("[WARN] Leaderboard auto-post failed: %v\n", err)
			} else {
				mu.Lock()
				state.LastLeaderboardPostDate = time.Now().UTC().Format("2006-01-02")
				mu.Unlock()
			}
		}

		// Periodic update check (heartbeat: every cycle; daily: once per day).
		if cfg.AutoUpdate == "heartbeat" {
			checkForUpdates(cfg, notifier, &lastNotifiedHash, &mu, state)
		} else if cfg.AutoUpdate == "daily" && cycle%dailyCycles == 0 {
			checkForUpdates(cfg, notifier, &lastNotifiedHash, &mu, state)
		}

		if *once {
			fmt.Println("--once flag set, exiting after single cycle.")
			return
		}

		// Wait for next tick or shutdown
		timer := time.NewTimer(time.Duration(tickSeconds) * time.Second)
		select {
		case <-timer.C:
			// Next tick
		case <-stopCh:
			timer.Stop()
			fmt.Println("Shutdown complete.")
			return
		}
	}
}

// runSummaryAndExit posts a snapshot summary for the given channel key and exits.
// It fetches current prices, formats the summary using the same logic as the hourly
// summaries, posts to all notification backends, and exits immediately.
func runSummaryAndExit(channelKey string, cfg *Config, state *AppState, notifier *MultiNotifier) {
	if !notifier.HasBackends() {
		fmt.Fprintf(os.Stderr, "No notification backends configured\n")
		os.Exit(1)
	}

	if !notifier.HasChannel(channelKey, channelKey) {
		fmt.Fprintf(os.Stderr, "No channel configured for %q\n", channelKey)
		os.Exit(1)
	}

	// Collect strategies for this channel.
	var chStrats []StrategyConfig
	for _, sc := range cfg.Strategies {
		if notifier.resolveChannelKey(sc.Platform, sc.Type) == channelKey {
			chStrats = append(chStrats, sc)
		}
	}
	if len(chStrats) == 0 {
		fmt.Fprintf(os.Stderr, "No strategies found for channel %q\n", channelKey)
		os.Exit(1)
	}

	// Collect symbols that need prices.
	symbolSet := make(map[string]bool)
	for _, sc := range cfg.Strategies {
		if sc.Type == "spot" && len(sc.Args) >= 2 {
			symbolSet[sc.Args[1]] = true
		}
	}
	symbols := make([]string, 0, len(symbolSet))
	for s := range symbolSet {
		symbols = append(symbols, s)
	}

	// Fetch current prices.
	prices := make(map[string]float64)
	if len(symbols) > 0 {
		p, err := FetchPrices(symbols)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Price fetch failed: %v\n", err)
			os.Exit(1)
		}
		for sym, price := range p {
			if price > 0 {
				prices[sym] = price
			}
		}
	}

	// Calculate channel value.
	chValue := 0.0
	for _, sc := range chStrats {
		if s, ok := state.Strategies[sc.ID]; ok {
			chValue += PortfolioValue(s, prices)
		}
	}

	// Format and send summary using the same asset-grouping logic as the main loop.
	assetGroups, assetKeys := groupByAsset(chStrats)
	if len(assetKeys) <= 1 {
		msg := FormatCategorySummary(state.CycleCount, 0, 0, 0, chValue, prices, nil, chStrats, state, channelKey, "")
		notifier.SendToChannel(channelKey, channelKey, msg)
		fmt.Println(msg)
	} else {
		for _, asset := range assetKeys {
			assetStrats := assetGroups[asset]
			assetValue := 0.0
			for _, sc := range assetStrats {
				if s, ok := state.Strategies[sc.ID]; ok {
					assetValue += PortfolioValue(s, prices)
				}
			}
			msg := FormatCategorySummary(state.CycleCount, 0, 0, 0, assetValue, prices, nil, assetStrats, state, channelKey, asset)
			notifier.SendToChannel(channelKey, channelKey, msg)
			fmt.Println(msg)
		}
	}

	fmt.Printf("-summary=%s: posted, exiting.\n", channelKey)
	os.Exit(0)
}

// runSpotCheck runs the spot check subprocess and returns the parsed result.
// No state access. Returns (result, signalStr, price, ok); ok=false means skip execution.
func runSpotCheck(sc StrategyConfig, prices map[string]float64, logger *StrategyLogger) (*SpotResult, string, float64, bool) {
	args := append([]string{}, sc.Args...)
	if sc.HTFFilter {
		args = append(args, "--htf-filter")
	}
	if len(sc.Params) > 0 {
		paramsJSON, err := json.Marshal(sc.Params)
		if err == nil {
			args = append(args, "--params", string(paramsJSON))
		} else {
			logger.Warn("Failed to marshal strategy params: %v", err)
		}
	}
	logger.Info("Running: python3 %s %v", sc.Script, args)

	result, stderr, err := RunSpotCheck(sc.Script, args)
	if err != nil {
		logger.Error("Script failed: %v", err)
		if stderr != "" {
			logger.Error("stderr: %s", stderr)
		}
		return nil, "", 0, false
	}
	if stderr != "" {
		logger.Info("stderr: %s", stderr)
	}

	if result.Error != "" {
		logger.Error("Script returned error: %s", result.Error)
		return nil, "", 0, false
	}

	signalStr := "HOLD"
	if result.Signal == 1 {
		signalStr = "BUY"
	} else if result.Signal == -1 {
		signalStr = "SELL"
	}
	logger.Info("Signal: %s | %s @ $%.2f", signalStr, result.Symbol, result.Price)

	// Use script price, fallback to fetched price
	price := result.Price
	if price <= 0 {
		if p, ok := prices[result.Symbol]; ok {
			price = p
		}
	}

	if price <= 0 {
		logger.Error("No price available for %s", result.Symbol)
		return nil, "", 0, false
	}

	return result, signalStr, price, true
}

// executeSpotResult applies a spot signal to state. Must be called under Lock.
func executeSpotResult(sc StrategyConfig, s *StrategyState, result *SpotResult, signalStr string, price float64, logger *StrategyLogger) (int, string) {
	trades, err := ExecuteSpotSignal(s, result.Signal, result.Symbol, price, logger)
	if err != nil {
		logger.Error("Trade execution failed: %v", err)
		return 0, ""
	}

	detail := ""
	if trades > 0 {
		detail = fmt.Sprintf("[%s] %s %s @ $%.2f", sc.ID, signalStr, result.Symbol, price)
	}
	return trades, detail
}

// runOptionsCheck runs the options check subprocess and returns the parsed result.
// No state access. Returns (result, signalStr, ok); ok=false means skip execution.
func runOptionsCheck(sc StrategyConfig, posJSON string, logger *StrategyLogger) (*OptionsResult, string, bool) {
	logger.Info("Running: python3 %s %v", sc.Script, sc.Args)

	result, stderr, err := RunOptionsCheckWithStdin(sc.Script, sc.Args, posJSON)
	if err != nil {
		logger.Error("Script failed: %v", err)
		if stderr != "" {
			logger.Error("stderr: %s", stderr)
		}
		return nil, "", false
	}
	if stderr != "" {
		logger.Info("stderr: %s", stderr)
	}

	if result.Error != "" {
		logger.Error("Script returned error: %s", result.Error)
		return nil, "", false
	}

	signalStr := "HOLD"
	if result.Signal == 1 {
		signalStr = "BULLISH"
	} else if result.Signal == -1 {
		signalStr = "BEARISH"
	}
	logger.Info("Signal: %s | %s spot=$%.2f | IV rank=%.1f | %d actions",
		signalStr, result.Underlying, result.SpotPrice, result.IVRank, len(result.Actions))

	return result, signalStr, true
}

// executeOptionsResult applies an options signal and theta harvest to state. Must be called under Lock.
// Returns (trades, detail, harvestDetails).
func executeOptionsResult(sc StrategyConfig, s *StrategyState, result *OptionsResult, signalStr string, logger *StrategyLogger) (int, string, []string) {
	trades, err := ExecuteOptionsSignal(s, result, logger)
	if err != nil {
		logger.Error("Options execution failed: %v", err)
		return 0, "", nil
	}

	detail := ""
	if trades > 0 {
		detail = fmt.Sprintf("[%s] %s %s spot=$%.2f IV=%.1f", sc.ID, signalStr, result.Underlying, result.SpotPrice, result.IVRank)
	}

	var harvestDetails []string
	if sc.ThetaHarvest != nil {
		harvestTrades, hDetails := CheckThetaHarvest(s, sc.ThetaHarvest, logger)
		trades += harvestTrades
		harvestDetails = hDetails
	}

	return trades, detail, harvestDetails
}

// shouldSkipZeroCapital reports whether a strategy should be skipped because
// capital_pct is set but capital resolved to $0 (balance fetch failed and
// no fallback capital configured).
func shouldSkipZeroCapital(sc StrategyConfig) bool {
	return sc.CapitalPct > 0 && sc.Capital <= 0
}

// hyperliquidIsLive reports whether --mode=live appears in strategy args.
// isLiveArgs reports whether --mode=live appears in strategy args.
func isLiveArgs(args []string) bool {
	for _, arg := range args {
		if arg == "--mode=live" {
			return true
		}
	}
	return false
}

// sendTradeAlerts sends trade alerts via DM and/or channel for all configured backends.
// trades is the number of new trades appended during this cycle.
func sendTradeAlerts(sc StrategyConfig, stratState *StrategyState, trades int, mu *sync.RWMutex, notifier *MultiNotifier) {
	isLive := isLiveArgs(sc.Args)
	mode := "paper"
	if isLive {
		mode = "live"
	}

	mu.RLock()
	n := len(stratState.TradeHistory)
	if n == 0 || trades <= 0 {
		mu.RUnlock()
		return
	}
	start := n - trades
	if start < 0 {
		start = 0
	}
	newTrades := make([]Trade, trades)
	copy(newTrades, stratState.TradeHistory[start:n])
	mu.RUnlock()

	for _, b := range notifier.backends {
		dmEnabled := b.ownerID != "" && ((isLive && b.dmLiveTrades) || (!isLive && b.dmPaperTrades))
		channelEnabled := (isLive && b.channelLiveTrades) || (!isLive && b.channelPaperTrades)
		if !dmEnabled && !channelEnabled {
			continue
		}

		ch := ""
		if channelEnabled {
			ch = resolveChannel(b.channels, sc.Platform, sc.Type)
			if ch == "" {
				fmt.Printf("[notify] channel trade alerts enabled but no channel configured for platform=%q type=%q\n", sc.Platform, sc.Type)
			}
		}

		// Also post live trades to a dedicated "<platform>-live" channel if configured.
		var liveCh string
		if isLive && channelEnabled {
			liveCh = resolveChannel(b.channels, sc.Platform+"-live", "")
			if liveCh == ch {
				liveCh = "" // avoid double-posting to the same channel
			}
		}

		for _, t := range newTrades {
			var msg string
			if b.plainText {
				msg = FormatTradeDMPlain(sc, t, mode)
			} else {
				msg = FormatTradeDM(sc, t, mode)
			}
			if dmEnabled {
				if err := b.notifier.SendDM(b.ownerID, msg); err != nil {
					fmt.Printf("[notify] DM trade alert failed: %v\n", err)
				}
			}
			if ch != "" {
				if err := b.notifier.SendMessage(ch, msg); err != nil {
					fmt.Printf("[notify] Channel trade alert failed: %v\n", err)
				}
			}
			if liveCh != "" {
				if err := b.notifier.SendMessage(liveCh, msg); err != nil {
					fmt.Printf("[notify] Live channel trade alert failed: %v\n", err)
				}
			}
		}
	}
}

func hyperliquidIsLive(args []string) bool {
	for _, arg := range args {
		if arg == "--mode=live" {
			return true
		}
	}
	return false
}

// hyperliquidSymbol extracts the coin symbol from perps strategy args (e.g. "BTC").
func hyperliquidSymbol(args []string) string {
	if len(args) >= 2 {
		return args[1]
	}
	return ""
}

// runHyperliquidCheck runs check_hyperliquid.py signal-check mode (Phase 3, no lock).
func runHyperliquidCheck(sc StrategyConfig, prices map[string]float64, logger *StrategyLogger) (*HyperliquidResult, string, float64, bool) {
	args := append([]string{}, sc.Args...)
	if sc.HTFFilter {
		args = append(args, "--htf-filter")
	}
	if len(sc.Params) > 0 {
		paramsJSON, err := json.Marshal(sc.Params)
		if err == nil {
			args = append(args, "--params", string(paramsJSON))
		} else {
			logger.Warn("Failed to marshal strategy params: %v", err)
		}
	}
	logger.Info("Running: python3 %s %v", sc.Script, args)

	result, stderr, err := RunHyperliquidCheck(sc.Script, args)
	if err != nil {
		logger.Error("Script failed: %v", err)
		if stderr != "" {
			logger.Error("stderr: %s", stderr)
		}
		return nil, "", 0, false
	}
	if stderr != "" {
		logger.Info("stderr: %s", stderr)
	}
	if result.Error != "" {
		logger.Error("Script returned error: %s", result.Error)
		return nil, "", 0, false
	}

	signalStr := "HOLD"
	if result.Signal == 1 {
		signalStr = "BUY"
	} else if result.Signal == -1 {
		signalStr = "SELL"
	}
	logger.Info("Signal: %s | %s @ $%.2f [%s]", signalStr, result.Symbol, result.Price, result.Mode)

	price := result.Price
	if price <= 0 {
		if p, ok := prices[result.Symbol]; ok {
			price = p
		}
	}
	if price <= 0 {
		logger.Error("No price available for %s", result.Symbol)
		return nil, "", 0, false
	}
	return result, signalStr, price, true
}

// runHyperliquidExecuteOrder places a live market order (Phase 3, no lock).
// Returns (execResult, ok); ok=false means order failed, skip state update.
func runHyperliquidExecuteOrder(sc StrategyConfig, result *HyperliquidResult, price, cash, posQty float64, logger *StrategyLogger) (*HyperliquidExecuteResult, bool) {
	isBuy := result.Signal == 1
	var size float64
	if isBuy {
		budget := cash * 0.95
		if budget < 1 || price <= 0 {
			logger.Info("Insufficient cash ($%.2f) for live buy", cash)
			return nil, false
		}
		size = budget / price
	} else {
		if posQty <= 0 {
			logger.Info("No position to close for %s", result.Symbol)
			return nil, false
		}
		size = posQty
	}

	side := "buy"
	if !isBuy {
		side = "sell"
	}
	logger.Info("Placing live %s %s size=%.6f", side, result.Symbol, size)

	execResult, stderr, err := RunHyperliquidExecute(sc.Script, result.Symbol, side, size)
	if stderr != "" {
		logger.Info("execute stderr: %s", stderr)
	}
	if err != nil {
		logger.Error("Live execute failed: %v", err)
		return nil, false
	}
	if execResult.Error != "" {
		logger.Error("Live execute returned error: %s", execResult.Error)
		return nil, false
	}
	return execResult, true
}

// executeHyperliquidResult applies a hyperliquid result to state. Must be called under Lock.
// execResult is non-nil for successful live orders; nil for paper mode.
func executeHyperliquidResult(sc StrategyConfig, s *StrategyState, result *HyperliquidResult, execResult *HyperliquidExecuteResult, signalStr string, price float64, logger *StrategyLogger) (int, string) {
	fillPrice := price
	if execResult != nil && execResult.Execution != nil && execResult.Execution.Fill != nil && execResult.Execution.Fill.AvgPx > 0 {
		fillPrice = execResult.Execution.Fill.AvgPx
		logger.Info("Live fill at $%.2f (mid was $%.2f)", fillPrice, price)
	}

	trades, err := ExecuteSpotSignal(s, result.Signal, result.Symbol, fillPrice, logger)
	if err != nil {
		logger.Error("Trade execution failed: %v", err)
		return 0, ""
	}

	detail := ""
	if trades > 0 {
		prefix := ""
		if execResult != nil {
			prefix = "LIVE "
		}
		detail = fmt.Sprintf("[%s] %s%s %s @ $%.2f", sc.ID, prefix, signalStr, result.Symbol, fillPrice)
	}
	return trades, detail
}

// topstepIsLive reports whether --mode=live appears in strategy args.
func topstepIsLive(args []string) bool {
	for _, arg := range args {
		if arg == "--mode=live" {
			return true
		}
	}
	return false
}

// topstepSymbol extracts the futures symbol from strategy args (e.g. "ES").
func topstepSymbol(args []string) string {
	if len(args) >= 2 {
		return args[1]
	}
	return ""
}

// runTopStepCheck runs check_topstep.py signal-check mode (Phase 3, no lock).
func runTopStepCheck(sc StrategyConfig, prices map[string]float64, logger *StrategyLogger) (*TopStepResult, string, float64, bool) {
	args := append([]string{}, sc.Args...)
	if sc.HTFFilter {
		args = append(args, "--htf-filter")
	}
	if len(sc.Params) > 0 {
		paramsJSON, err := json.Marshal(sc.Params)
		if err == nil {
			args = append(args, "--params", string(paramsJSON))
		} else {
			logger.Warn("Failed to marshal strategy params: %v", err)
		}
	}
	logger.Info("Running: python3 %s %v", sc.Script, args)

	result, stderr, err := RunTopStepCheck(sc.Script, args)
	if err != nil {
		logger.Error("Script failed: %v", err)
		if stderr != "" {
			logger.Error("stderr: %s", stderr)
		}
		return nil, "", 0, false
	}
	if stderr != "" {
		logger.Info("stderr: %s", stderr)
	}
	if result.Error != "" {
		logger.Error("Script returned error: %s", result.Error)
		return nil, "", 0, false
	}

	if !result.MarketOpen {
		logger.Info("Market closed for %s, skipping", result.Symbol)
		return nil, "", 0, false
	}

	signalStr := "HOLD"
	if result.Signal == 1 {
		signalStr = "BUY"
	} else if result.Signal == -1 {
		signalStr = "SELL"
	}
	logger.Info("Signal: %s | %s @ $%.2f [%s]", signalStr, result.Symbol, result.Price, result.Mode)

	price := result.Price
	if price <= 0 {
		if p, ok := prices[result.Symbol]; ok {
			price = p
		}
	}
	if price <= 0 {
		logger.Error("No price available for %s", result.Symbol)
		return nil, "", 0, false
	}
	return result, signalStr, price, true
}

// runTopStepExecuteOrder places a live futures order (Phase 3, no lock).
func runTopStepExecuteOrder(sc StrategyConfig, result *TopStepResult, price, cash, posQty float64, logger *StrategyLogger) (*TopStepExecuteResult, bool) {
	isBuy := result.Signal == 1
	var contracts int
	if isBuy {
		budget := cash * 0.95
		margin := result.ContractSpec.Margin
		if margin <= 0 {
			margin = price * result.ContractSpec.Multiplier // fallback
		}
		if budget < 1 || price <= 0 || margin <= 0 {
			logger.Info("Insufficient cash ($%.2f) for live buy", cash)
			return nil, false
		}
		contracts = int(budget / margin)
		if sc.FuturesConfig != nil && sc.FuturesConfig.MaxContracts > 0 && contracts > sc.FuturesConfig.MaxContracts {
			contracts = sc.FuturesConfig.MaxContracts
		}
		if contracts < 1 {
			logger.Info("Insufficient cash ($%.2f) for even 1 contract (margin=$%.0f)", cash, margin)
			return nil, false
		}
	} else {
		if posQty <= 0 {
			logger.Info("No position to close for %s", result.Symbol)
			return nil, false
		}
		contracts = int(posQty)
	}

	side := "buy"
	if !isBuy {
		side = "sell"
	}
	logger.Info("Placing live %s %s contracts=%d", side, result.Symbol, contracts)

	execResult, stderr, err := RunTopStepExecute(sc.Script, result.Symbol, side, contracts)
	if stderr != "" {
		logger.Info("execute stderr: %s", stderr)
	}
	if err != nil {
		logger.Error("Live execute failed: %v", err)
		return nil, false
	}
	if execResult.Error != "" {
		logger.Error("Live execute returned error: %s", execResult.Error)
		return nil, false
	}
	return execResult, true
}

// executeTopStepResult applies a TopStep futures result to state. Must be called under Lock.
func executeTopStepResult(sc StrategyConfig, s *StrategyState, result *TopStepResult, execResult *TopStepExecuteResult, signalStr string, price float64, logger *StrategyLogger) (int, string) {
	fillPrice := price
	if execResult != nil && execResult.Execution != nil && execResult.Execution.Fill != nil && execResult.Execution.Fill.AvgPx > 0 {
		fillPrice = execResult.Execution.Fill.AvgPx
		logger.Info("Live fill at $%.2f (signal was $%.2f)", fillPrice, price)
	}

	var feePerContract float64
	var maxContracts int
	if sc.FuturesConfig != nil {
		feePerContract = sc.FuturesConfig.FeePerContract
		maxContracts = sc.FuturesConfig.MaxContracts
	}

	trades, err := ExecuteFuturesSignal(s, result.Signal, result.Symbol, fillPrice, result.ContractSpec, feePerContract, maxContracts, logger)
	if err != nil {
		logger.Error("Trade execution failed: %v", err)
		return 0, ""
	}

	detail := ""
	if trades > 0 {
		prefix := ""
		if execResult != nil {
			prefix = "LIVE "
		}
		detail = fmt.Sprintf("[%s] %s%s %s @ $%.2f", sc.ID, prefix, signalStr, result.Symbol, fillPrice)
	}
	return trades, detail
}

// robinhoodIsLive reports whether --mode=live appears in strategy args.
func robinhoodIsLive(args []string) bool {
	for _, arg := range args {
		if arg == "--mode=live" {
			return true
		}
	}
	return false
}

// robinhoodSymbol extracts the coin symbol from strategy args (e.g. "BTC").
func robinhoodSymbol(args []string) string {
	if len(args) >= 2 {
		return args[1]
	}
	return ""
}

// runRobinhoodCheck runs check_robinhood.py signal-check mode (Phase 3, no lock).
func runRobinhoodCheck(sc StrategyConfig, prices map[string]float64, logger *StrategyLogger) (*RobinhoodResult, string, float64, bool) {
	args := append([]string{}, sc.Args...)
	if sc.HTFFilter {
		args = append(args, "--htf-filter")
	}
	if len(sc.Params) > 0 {
		paramsJSON, err := json.Marshal(sc.Params)
		if err == nil {
			args = append(args, "--params", string(paramsJSON))
		} else {
			logger.Warn("Failed to marshal strategy params: %v", err)
		}
	}
	logger.Info("Running: python3 %s %v", sc.Script, args)

	result, stderr, err := RunRobinhoodCheck(sc.Script, args)
	if err != nil {
		logger.Error("Script failed: %v", err)
		if stderr != "" {
			logger.Error("stderr: %s", stderr)
		}
		return nil, "", 0, false
	}
	if stderr != "" {
		logger.Info("stderr: %s", stderr)
	}
	if result.Error != "" {
		logger.Error("Script returned error: %s", result.Error)
		return nil, "", 0, false
	}

	signalStr := "HOLD"
	if result.Signal == 1 {
		signalStr = "BUY"
	} else if result.Signal == -1 {
		signalStr = "SELL"
	}
	logger.Info("Signal: %s | %s @ $%.2f [%s]", signalStr, result.Symbol, result.Price, result.Mode)

	price := result.Price
	if price <= 0 {
		if p, ok := prices[result.Symbol]; ok {
			price = p
		}
	}
	if price <= 0 {
		logger.Error("No price available for %s", result.Symbol)
		return nil, "", 0, false
	}
	return result, signalStr, price, true
}

// runRobinhoodExecuteOrder places a live crypto order (Phase 3, no lock).
func runRobinhoodExecuteOrder(sc StrategyConfig, result *RobinhoodResult, price, cash, posQty float64, logger *StrategyLogger) (*RobinhoodExecuteResult, bool) {
	isBuy := result.Signal == 1
	var amountUSD float64
	var quantity float64
	side := "buy"

	if isBuy {
		amountUSD = cash * 0.95
		if amountUSD < 1 || price <= 0 {
			logger.Info("Insufficient cash ($%.2f) for live buy", cash)
			return nil, false
		}
	} else {
		side = "sell"
		if posQty <= 0 {
			logger.Info("No position to close for %s", result.Symbol)
			return nil, false
		}
		quantity = posQty
	}

	logger.Info("Placing live %s %s amount_usd=%.2f qty=%.6f", side, result.Symbol, amountUSD, quantity)

	execResult, stderr, err := RunRobinhoodExecute(sc.Script, result.Symbol, side, amountUSD, quantity)
	if stderr != "" {
		logger.Info("execute stderr: %s", stderr)
	}
	if err != nil {
		logger.Error("Live execute failed: %v", err)
		return nil, false
	}
	if execResult.Error != "" {
		logger.Error("Live execute returned error: %s", execResult.Error)
		return nil, false
	}
	return execResult, true
}

// executeRobinhoodResult applies a Robinhood result to state. Must be called under Lock.
func executeRobinhoodResult(sc StrategyConfig, s *StrategyState, result *RobinhoodResult, execResult *RobinhoodExecuteResult, signalStr string, price float64, logger *StrategyLogger) (int, string) {
	fillPrice := price
	if execResult != nil && execResult.Execution != nil && execResult.Execution.Fill != nil && execResult.Execution.Fill.AvgPx > 0 {
		fillPrice = execResult.Execution.Fill.AvgPx
		logger.Info("Live fill at $%.2f (mid was $%.2f)", fillPrice, price)
	}

	trades, err := ExecuteSpotSignal(s, result.Signal, result.Symbol, fillPrice, logger)
	if err != nil {
		logger.Error("Trade execution failed: %v", err)
		return 0, ""
	}

	detail := ""
	if trades > 0 {
		prefix := ""
		if execResult != nil {
			prefix = "LIVE "
		}
		detail = fmt.Sprintf("[%s] %s%s %s @ $%.2f", sc.ID, prefix, signalStr, result.Symbol, fillPrice)
	}
	return trades, detail
}

// okxIsLive reports whether --mode=live appears in strategy args.
func okxIsLive(args []string) bool {
	for _, arg := range args {
		if arg == "--mode=live" {
			return true
		}
	}
	return false
}

// okxSymbol extracts the coin symbol from OKX strategy args (e.g. "BTC").
func okxSymbol(args []string) string {
	if len(args) >= 2 {
		return args[1]
	}
	return ""
}

// okxInstType extracts --inst-type from strategy args (default "swap").
func okxInstType(args []string) string {
	for _, arg := range args {
		if strings.HasPrefix(arg, "--inst-type=") {
			return strings.TrimPrefix(arg, "--inst-type=")
		}
	}
	return "swap"
}

// runOKXCheck runs check_okx.py signal-check mode (Phase 3, no lock).
func runOKXCheck(sc StrategyConfig, prices map[string]float64, logger *StrategyLogger) (*OKXResult, string, float64, bool) {
	args := append([]string{}, sc.Args...)
	if sc.HTFFilter {
		args = append(args, "--htf-filter")
	}
	if len(sc.Params) > 0 {
		paramsJSON, err := json.Marshal(sc.Params)
		if err == nil {
			args = append(args, "--params", string(paramsJSON))
		} else {
			logger.Warn("Failed to marshal strategy params: %v", err)
		}
	}
	logger.Info("Running: python3 %s %v", sc.Script, args)

	result, stderr, err := RunOKXCheck(sc.Script, args)
	if err != nil {
		logger.Error("Script failed: %v", err)
		if stderr != "" {
			logger.Error("stderr: %s", stderr)
		}
		return nil, "", 0, false
	}
	if stderr != "" {
		logger.Info("stderr: %s", stderr)
	}
	if result.Error != "" {
		logger.Error("Script returned error: %s", result.Error)
		return nil, "", 0, false
	}

	signalStr := "HOLD"
	if result.Signal == 1 {
		signalStr = "BUY"
	} else if result.Signal == -1 {
		signalStr = "SELL"
	}
	logger.Info("Signal: %s | %s @ $%.2f [%s]", signalStr, result.Symbol, result.Price, result.Mode)

	price := result.Price
	if price <= 0 {
		if p, ok := prices[result.Symbol]; ok {
			price = p
		}
	}
	if price <= 0 {
		logger.Error("No price available for %s", result.Symbol)
		return nil, "", 0, false
	}
	return result, signalStr, price, true
}

// runOKXExecuteOrder places a live market order on OKX (Phase 3, no lock).
func runOKXExecuteOrder(sc StrategyConfig, result *OKXResult, price, cash, posQty float64, logger *StrategyLogger) (*OKXExecuteResult, bool) {
	isBuy := result.Signal == 1
	var size float64
	if isBuy {
		budget := cash * 0.95
		if budget < 1 || price <= 0 {
			logger.Info("Insufficient cash ($%.2f) for live buy", cash)
			return nil, false
		}
		size = budget / price
	} else {
		if posQty <= 0 {
			logger.Info("No position to close for %s", result.Symbol)
			return nil, false
		}
		size = posQty
	}

	side := "buy"
	if !isBuy {
		side = "sell"
	}
	instType := okxInstType(sc.Args)
	logger.Info("Placing live %s %s size=%.6f inst_type=%s", side, result.Symbol, size, instType)

	execResult, stderr, err := RunOKXExecute(sc.Script, result.Symbol, side, size, instType)
	if stderr != "" {
		logger.Info("execute stderr: %s", stderr)
	}
	if err != nil {
		logger.Error("Live execute failed: %v", err)
		return nil, false
	}
	if execResult.Error != "" {
		logger.Error("Live execute returned error: %s", execResult.Error)
		return nil, false
	}
	return execResult, true
}

// executeOKXResult applies an OKX result to state. Must be called under Lock.
func executeOKXResult(sc StrategyConfig, s *StrategyState, result *OKXResult, execResult *OKXExecuteResult, signalStr string, price float64, logger *StrategyLogger) (int, string) {
	fillPrice := price
	if execResult != nil && execResult.Execution != nil && execResult.Execution.Fill != nil && execResult.Execution.Fill.AvgPx > 0 {
		fillPrice = execResult.Execution.Fill.AvgPx
		logger.Info("Live fill at $%.2f (mid was $%.2f)", fillPrice, price)
	}

	trades, err := ExecuteSpotSignal(s, result.Signal, result.Symbol, fillPrice, logger)
	if err != nil {
		logger.Error("Trade execution failed: %v", err)
		return 0, ""
	}

	detail := ""
	if trades > 0 {
		prefix := ""
		if execResult != nil {
			prefix = "LIVE "
		}
		detail = fmt.Sprintf("[%s] %s%s %s @ $%.2f", sc.ID, prefix, signalStr, result.Symbol, fillPrice)
	}
	return trades, detail
}

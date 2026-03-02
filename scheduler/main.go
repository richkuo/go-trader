package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
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

	// Initialize new strategies and sync config values for existing ones
	for _, sc := range cfg.Strategies {
		if s, exists := state.Strategies[sc.ID]; !exists {
			state.Strategies[sc.ID] = NewStrategyState(sc)
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
	if state.PortfolioRisk.PeakValue == 0 {
		total := 0.0
		for _, sc := range cfg.Strategies {
			total += sc.Capital
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

	// Discord notifier (WebSocket gateway connection for two-way DM support).
	var discord *DiscordNotifier
	if cfg.Discord.Enabled && cfg.Discord.Token != "" {
		var err error
		discord, err = NewDiscordNotifier(cfg.Discord.Token, cfg.Discord.OwnerID)
		if err != nil {
			fmt.Printf("[WARN] Discord init failed: %v — continuing without Discord\n", err)
		} else {
			fmt.Printf("Discord gateway connected (%d channels", len(cfg.Discord.Channels))
			if cfg.Discord.OwnerID != "" {
				fmt.Printf(", DM owner enabled")
			}
			fmt.Println(")")
			defer discord.Close()
		}
	}

	// Config migration: DM owner about new fields if config is behind current version.
	if cfg.ConfigVersion < CurrentConfigVersion {
		go runConfigMigrationDM(cfg, discord, *configPath)
	}

	// Track the last remote hash we notified about to avoid re-notifying on every cycle.
	var lastNotifiedHash string

	// Check for updates on startup (best-effort, non-blocking).
	if cfg.AutoUpdate != "off" {
		checkForUpdates(cfg, discord, &lastNotifiedHash, &mu, state)
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

		// Determine which strategies are due this tick
		dueStrategies := make([]StrategyConfig, 0)
		for _, sc := range cfg.Strategies {
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
			portfolioAllowed, nb, portfolioReason := CheckPortfolioRisk(&state.PortfolioRisk, cfg.PortfolioRisk, totalPV, totalNotional)
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

			if killSwitchFired && discord != nil {
				msg := fmt.Sprintf("**PORTFOLIO KILL SWITCH**\n%s\nAll positions force-closed. Manual reset required.", portfolioReason)
				seen := make(map[string]bool)
				for _, ch := range cfg.Discord.Channels {
					if ch != "" && !seen[ch] {
						seen[ch] = true
						if err := discord.SendMessage(ch, msg); err != nil {
							fmt.Printf("[WARN] Discord kill switch alert failed: %v\n", err)
						}
					}
				}
			}

			if !killSwitchFired {
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
						if result, signalStr, price, ok := runSpotCheck(sc, prices, logger); ok {
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
							if ch := resolveChannel(cfg.Discord.Channels, sc.Platform, sc.Type); ch != "" {
								channelTradeDetails[ch] = append(channelTradeDetails[ch], harvestDetails...)
							}
						}
					case "perps":
						if result, signalStr, price, ok := runHyperliquidCheck(sc, prices, logger); ok {
							prices[result.Symbol] = price
							var execResult *HyperliquidExecuteResult
							if hyperliquidIsLive(sc.Args) && result.Signal != 0 {
								if er, ok2 := runHyperliquidExecuteOrder(sc, result, price, hlCash, hlPosQty, logger); ok2 {
									execResult = er
								}
							}
							mu.Lock()
							trades, detail = executeHyperliquidResult(sc, stratState, result, execResult, signalStr, price, logger)
							mu.Unlock()
						}
					default:
						logger.Error("Unknown strategy type: %s", sc.Type)
					}
					if trades > 0 && detail != "" {
						if ch := resolveChannel(cfg.Discord.Channels, sc.Platform, sc.Type); ch != "" {
							channelTrades[ch] += trades
							channelTradeDetails[ch] = append(channelTradeDetails[ch], detail)
						}
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
							pricer = deribitPricer
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
		mu.RLock()
		totalValue := 0.0
		channelValue := make(map[string]float64)
		channelStrats := make(map[string][]StrategyConfig)
		for _, sc := range cfg.Strategies {
			if s, ok := state.Strategies[sc.ID]; ok {
				pv := PortfolioValue(s, prices)
				totalValue += pv
				if ch := resolveChannel(cfg.Discord.Channels, sc.Platform, sc.Type); ch != "" {
					channelValue[ch] += pv
					channelStrats[ch] = append(channelStrats[ch], sc)
				}
			}
		}
		mu.RUnlock()

		elapsed := time.Since(cycleStart)
		logMgr.LogSummary(cycle, elapsed, len(dueStrategies), totalTrades, totalValue)

		// Discord notification — one message per channel, dynamic platform support.
		if discord != nil {
			mu.RLock()
			for ch, chStrats := range channelStrats {
				// Only post if at least one due strategy maps to this channel.
				chRan := false
				for _, sc := range dueStrategies {
					if resolveChannel(cfg.Discord.Channels, sc.Platform, sc.Type) == ch {
						chRan = true
						break
					}
				}
				if !chRan {
					continue
				}
				chTrades := channelTrades[ch]
				chDetails := channelTradeDetails[ch]
				chValue := channelValue[ch]
				// Options: post every run. Others: hourly or on trade.
				// (cycle-1)%12==0 fires at cycles 1,13,25... so first summary posts on startup.
				if isOptionsType(chStrats) || chTrades > 0 || (cycle-1)%12 == 0 {
					chKey := channelKeyFromID(cfg.Discord.Channels, ch)
					msg := FormatCategorySummary(cycle, elapsed, len(dueStrategies), chTrades, chValue, prices, chDetails, chStrats, state, chKey)
					if err := discord.SendMessage(ch, msg); err != nil {
						fmt.Printf("[WARN] Discord %s summary failed: %v\n", chKey, err)
					}
				}
			}
			mu.RUnlock()
		}

		// Save state after each cycle
		mu.Lock()
		state.LastCycle = time.Now().UTC()
		if err := SavePlatformStates(state, cfg); err != nil {
			saveFailures++
			fmt.Printf("[CRITICAL] Save state failed (%d/3): %v\n", saveFailures, err)
		} else {
			saveFailures = 0
		}
		mu.Unlock()

		// Periodic update check (heartbeat: every cycle; daily: once per day).
		if cfg.AutoUpdate == "heartbeat" {
			checkForUpdates(cfg, discord, &lastNotifiedHash, &mu, state)
		} else if cfg.AutoUpdate == "daily" && cycle%dailyCycles == 0 {
			checkForUpdates(cfg, discord, &lastNotifiedHash, &mu, state)
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

// runSpotCheck runs the spot check subprocess and returns the parsed result.
// No state access. Returns (result, signalStr, price, ok); ok=false means skip execution.
func runSpotCheck(sc StrategyConfig, prices map[string]float64, logger *StrategyLogger) (*SpotResult, string, float64, bool) {
	logger.Info("Running: python3 %s %v", sc.Script, sc.Args)

	result, stderr, err := RunSpotCheck(sc.Script, sc.Args)
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

// hyperliquidIsLive reports whether --mode=live appears in strategy args.
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
	logger.Info("Running: python3 %s %v", sc.Script, sc.Args)

	result, stderr, err := RunHyperliquidCheck(sc.Script, sc.Args)
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

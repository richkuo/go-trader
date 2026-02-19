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

	// Load or initialize state
	state, err := LoadState(cfg.StateFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load state: %v\n", err)
		os.Exit(1)
	}
	ValidateState(state)

	// Initialize strategy states for any new strategies
	for _, sc := range cfg.Strategies {
		if _, exists := state.Strategies[sc.ID]; !exists {
			state.Strategies[sc.ID] = NewStrategyState(sc)
			fmt.Printf("  Initialized strategy: %s (type=%s, capital=$%.0f)\n", sc.ID, sc.Type, sc.Capital)
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
	server := NewStatusServer(state, &mu, cfg.StatusToken)
	server.Start(8099)

	// Graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	stopCh := make(chan struct{})
	go func() {
		sig := <-sigCh
		fmt.Printf("\nReceived %s, saving state and shutting down...\n", sig)
		mu.Lock()
		if err := SaveState(cfg.StateFile, state); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to save state: %v\n", err)
		} else {
			fmt.Println("State saved successfully.")
		}
		mu.Unlock()
		close(stopCh)
	}()

	// Discord notifier
	var discord *DiscordNotifier
	if cfg.Discord.Enabled && cfg.Discord.Token != "" {
		if cfg.Discord.Channels.Spot != "" || cfg.Discord.Channels.Options != "" {
			discord = NewDiscordNotifier(cfg.Discord.Token)
			fmt.Printf("Discord notifications enabled (spot: %s, options: %s)\n",
				cfg.Discord.Channels.Spot, cfg.Discord.Channels.Options)
		}
	}

	// Deribit pricer for live option prices
	deribitPricer := NewDeribitPricer()
	fmt.Println("Deribit live pricing enabled")

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

	saveFailures := 0

	// Main loop
	for {
		cycleStart := time.Now()
		mu.Lock()
		state.CycleCount++
		cycle := state.CycleCount
		mu.Unlock()
		totalTrades := 0
		spotTrades := 0
		optionsTrades := 0
		spotTradeDetails := make([]string, 0)
		optionsTradeDetails := make([]string, 0)

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
				fmt.Printf("[WARN] Price fetch failed: %v\n", err)
			} else {
				prices = p
				fmt.Printf("Prices: ")
				for sym, price := range prices {
					fmt.Printf("%s=$%.2f ", sym, price)
				}
				fmt.Println()
			}
		}

		// Process only due strategies
		if saveFailures >= 3 {
			fmt.Println("[CRITICAL] State save failed 3x, skipping trades this cycle")
		} else {
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
				mu.RUnlock()

				// Phase 2: Lock — CheckRisk (fast, no I/O)
				mu.Lock()
				allowed, reason := CheckRisk(stratState, pv)
				mu.Unlock()
				if !allowed {
					logger.Warn("Risk block: %s (portfolio=$%.2f)", reason, pv)
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
					if trades > 0 && detail != "" {
						spotTradeDetails = append(spotTradeDetails, detail)
					}
					spotTrades += trades
				case "options":
					if result, signalStr, ok := runOptionsCheck(sc, posJSON, logger); ok {
						mu.Lock()
						var harvestDetails []string
						trades, detail, harvestDetails = executeOptionsResult(sc, stratState, result, signalStr, logger)
						mu.Unlock()
						optionsTradeDetails = append(optionsTradeDetails, harvestDetails...)
					}
					if trades > 0 && detail != "" {
						optionsTradeDetails = append(optionsTradeDetails, detail)
					}
					optionsTrades += trades
				default:
					logger.Error("Unknown strategy type: %s", sc.Type)
				}

				totalTrades += trades

				// Phase 5: mark option positions with live Deribit prices
				mu.RLock()
				markReqs := collectMarkRequests(stratState)
				mu.RUnlock()
				if len(markReqs) > 0 {
					markResults := fetchMarkPrices(markReqs, deribitPricer, logger)
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
		}

		// Calculate total portfolio value and separate spot/options values
		mu.RLock()
		totalValue := 0.0
		spotValue := 0.0
		optionsValue := 0.0
		for _, sc := range cfg.Strategies {
			if s, ok := state.Strategies[sc.ID]; ok {
				pv := PortfolioValue(s, prices)
				totalValue += pv
				cat := stratCategory(sc.ID)
				if cat == "spot" {
					spotValue += pv
				} else {
					optionsValue += pv
				}
			}
		}
		mu.RUnlock()

		elapsed := time.Since(cycleStart)
		logMgr.LogSummary(cycle, elapsed, len(dueStrategies), totalTrades, totalValue)

		// Discord notification - separate spot and options reports
		if discord != nil {
			mu.RLock()

			// Check which categories ran
			spotRan := false
			optionsRan := false
			for _, sc := range dueStrategies {
				cat := stratCategory(sc.ID)
				if cat == "spot" {
					spotRan = true
				} else {
					optionsRan = true
				}
			}

			// Send spot summary (hourly or with trades)
			if spotRan && (cycle%12 == 0 || spotTrades > 0) && cfg.Discord.Channels.Spot != "" {
				msg := FormatCategorySummary(cycle, elapsed, len(dueStrategies), spotTrades, spotValue, prices, spotTradeDetails, cfg.Strategies, state, "spot")
				if err := discord.SendMessage(cfg.Discord.Channels.Spot, msg); err != nil {
					fmt.Printf("[WARN] Discord spot summary failed: %v\n", err)
				}
			}

			// Send options summary (every run or with trades)
			if optionsRan && cfg.Discord.Channels.Options != "" {
				msg := FormatCategorySummary(cycle, elapsed, len(dueStrategies), optionsTrades, optionsValue, prices, optionsTradeDetails, cfg.Strategies, state, "options")
				if err := discord.SendMessage(cfg.Discord.Channels.Options, msg); err != nil {
					fmt.Printf("[WARN] Discord options summary failed: %v\n", err)
				}
			}

			mu.RUnlock()
		}

		// Save state after each cycle
		mu.Lock()
		state.LastCycle = time.Now().UTC()
		if err := SaveState(cfg.StateFile, state); err != nil {
			saveFailures++
			fmt.Printf("[CRITICAL] Save state failed (%d/3): %v\n", saveFailures, err)
		} else {
			saveFailures = 0
		}
		mu.Unlock()

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

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

	// Initialize strategy states for any new strategies
	for _, sc := range cfg.Strategies {
		if _, exists := state.Strategies[sc.ID]; !exists {
			state.Strategies[sc.ID] = NewStrategyState(sc)
			fmt.Printf("  Initialized strategy: %s (type=%s, capital=$%.0f)\n", sc.ID, sc.Type, sc.Capital)
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
	server := NewStatusServer(state, &mu)
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

	// Main loop
	for {
		cycleStart := time.Now()
		state.CycleCount++
		cycle := state.CycleCount
		totalTrades := 0

		fmt.Printf("\n=== Cycle %d starting at %s ===\n", cycle, cycleStart.UTC().Format("2006-01-02 15:04:05 UTC"))

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

		// Process each strategy sequentially
		for _, sc := range cfg.Strategies {
			stratState := state.Strategies[sc.ID]
			if stratState == nil {
				continue
			}

			logger, err := logMgr.GetStrategyLogger(sc.ID)
			if err != nil {
				fmt.Printf("[ERROR] Logger for %s: %v\n", sc.ID, err)
				continue
			}

			// Check risk before running
			mu.RLock()
			pv := PortfolioValue(stratState, prices)
			mu.RUnlock()

			allowed, reason := CheckRisk(stratState, pv)
			if !allowed {
				logger.Warn("Risk block: %s (portfolio=$%.2f)", reason, pv)
				logger.Close()
				continue
			}

			// Run appropriate check script
			trades := 0
			switch sc.Type {
			case "spot":
				trades = processSpot(sc, stratState, prices, logger)
			case "options":
				trades = processOptions(sc, stratState, logger)
			default:
				logger.Error("Unknown strategy type: %s", sc.Type)
			}

			// Update option positions
			mu.Lock()
			UpdateOptionPositions(stratState)
			pv = PortfolioValue(stratState, prices)
			mu.Unlock()

			totalTrades += trades

			// Status line
			posCount := len(stratState.Positions) + len(stratState.OptionPositions)
			logger.Info("Status: cash=$%.2f | positions=%d | value=$%.2f | trades=%d",
				stratState.Cash, posCount, pv, trades)

			logger.Close()
		}

		// Calculate total portfolio value
		mu.RLock()
		totalValue := 0.0
		for _, sc := range cfg.Strategies {
			if s, ok := state.Strategies[sc.ID]; ok {
				totalValue += PortfolioValue(s, prices)
			}
		}
		mu.RUnlock()

		elapsed := time.Since(cycleStart)
		logMgr.LogSummary(cycle, elapsed, len(cfg.Strategies), totalTrades, totalValue)

		// Save state after each cycle
		mu.Lock()
		if err := SaveState(cfg.StateFile, state); err != nil {
			fmt.Printf("[ERROR] Save state: %v\n", err)
		}
		mu.Unlock()

		if *once {
			fmt.Println("--once flag set, exiting after single cycle.")
			return
		}

		// Wait for next cycle or shutdown
		timer := time.NewTimer(time.Duration(cfg.IntervalSeconds) * time.Second)
		select {
		case <-timer.C:
			// Next cycle
		case <-stopCh:
			timer.Stop()
			fmt.Println("Shutdown complete.")
			return
		}
	}
}

func processSpot(sc StrategyConfig, s *StrategyState, prices map[string]float64, logger *StrategyLogger) int {
	logger.Info("Running: python3 %s %v", sc.Script, sc.Args)

	result, stderr, err := RunSpotCheck(sc.Script, sc.Args)
	if err != nil {
		logger.Error("Script failed: %v", err)
		if stderr != "" {
			logger.Error("stderr: %s", stderr)
		}
		return 0
	}
	if stderr != "" {
		logger.Info("stderr: %s", stderr)
	}

	if result.Error != "" {
		logger.Error("Script returned error: %s", result.Error)
		return 0
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
		return 0
	}

	trades, err := ExecuteSpotSignal(s, result.Signal, result.Symbol, price, logger)
	if err != nil {
		logger.Error("Trade execution failed: %v", err)
		return 0
	}
	return trades
}

func processOptions(sc StrategyConfig, s *StrategyState, logger *StrategyLogger) int {
	logger.Info("Running: python3 %s %v", sc.Script, sc.Args)

	result, stderr, err := RunOptionsCheck(sc.Script, sc.Args)
	if err != nil {
		logger.Error("Script failed: %v", err)
		if stderr != "" {
			logger.Error("stderr: %s", stderr)
		}
		return 0
	}
	if stderr != "" {
		logger.Info("stderr: %s", stderr)
	}

	if result.Error != "" {
		logger.Error("Script returned error: %s", result.Error)
		return 0
	}

	signalStr := "HOLD"
	if result.Signal == 1 {
		signalStr = "BULLISH"
	} else if result.Signal == -1 {
		signalStr = "BEARISH"
	}
	logger.Info("Signal: %s | %s spot=$%.2f | IV rank=%.1f | %d actions",
		signalStr, result.Underlying, result.SpotPrice, result.IVRank, len(result.Actions))

	trades, err := ExecuteOptionsSignal(s, result, logger)
	if err != nil {
		logger.Error("Options execution failed: %v", err)
		return 0
	}
	return trades
}

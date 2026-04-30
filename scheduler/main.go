package main

import (
	"context"
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
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "init":
			os.Exit(runInit(os.Args[2:]))
		case "export":
			os.Exit(runExport(os.Args[2:]))
		}
	}

	configPath := flag.String("config", "scheduler/config.json", "Path to config file")
	once := flag.Bool("once", false, "Run one cycle and exit")
	summary := flag.String("summary", "", "Post snapshot summary for the specified channel (e.g., hyperliquid, spot, options) and exit")
	leaderboard := flag.Bool("leaderboard", false, "Post pre-computed daily leaderboard and exit")
	statusPortFlag := flag.Int("status-port", 0, fmt.Sprintf("HTTP status server port (overrides config, default: %d)", DefaultStatusPort))
	flag.Parse()

	// Load config
	cfg, err := LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Loaded config: %d strategies, interval=%ds\n", len(cfg.Strategies), cfg.IntervalSeconds)

	// #339: Detect a missing state DB on a live deployment *before* OpenStateDB
	// creates it — a wiped directory (vs. an in-place `git pull`) would otherwise
	// silently produce a fresh empty DB and desync from exchange positions.
	// Captured here and replayed to the owner DM once the notifier is wired.
	var missingStateWarning string
	if msg := CheckStatePresence(cfg.DBFile, cfg.Strategies); msg != "" && !AllowMissingState() {
		fmt.Fprintln(os.Stderr, msg)
		missingStateWarning = msg
	}

	// Open SQLite state database.
	stateDB, err := OpenStateDB(cfg.DBFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to open state DB: %v\n", err)
		os.Exit(1)
	}
	defer stateDB.Close()

	// Wire the immediate trade-persistence hook (#289) so every trade is
	// written to SQLite the moment it is appended to TradeHistory — this
	// survives mid-cycle crashes that would otherwise lose the in-memory batch.
	tradeRecorder = stateDB.InsertTrade

	// Load state: SQLite primary, JSON fallback with auto-migration.
	state, err := LoadStateWithDB(cfg, stateDB)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load state: %v\n", err)
		os.Exit(1)
	}
	ValidateState(state)

	// #87: Resolve capital_pct at startup so initial state gets the right capital.
	resolveCapitalPct(cfg.Strategies)

	// #343: Reconcile any operator-driven initial_capital changes from config
	// against the persisted baseline. Without this, the SaveState guard would
	// silently revert a legitimate "bump initial_capital in config.json" edit
	// on the next cycle. Captured here, surfaced to the owner DM once the
	// notifier is wired below.
	initialCapitalChangeInfos, initialCapitalChangeErrors := ReconcileConfigInitialCapital(cfg, state, stateDB)

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
			// #418 RC3 one-shot self-heal: pre-#418 deployments where
			// reconcile overwrote pos.Leverage with the on-chain margin
			// tier (e.g. configured 2x but stored 20x) would persist the
			// stale value indefinitely under the new `== 0` write-path
			// guard. Risk math reads sc.Leverage now, so this only fixes
			// metadata visible to analytics/dashboards/future readers, but
			// stamping config onto state at startup keeps the two sources
			// of truth aligned.
			if sc.Platform == "hyperliquid" && sc.Type == "perps" && sc.Leverage > 0 {
				for sym, pos := range s.Positions {
					if pos == nil {
						continue
					}
					if pos.Leverage != sc.Leverage {
						fmt.Printf("  hl-heal: %s %s leverage %v → %v (config)\n", sc.ID, sym, pos.Leverage, sc.Leverage)
						pos.Leverage = sc.Leverage
					}
				}
			}
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

	// #336: Detect perps shorts held under AllowShorts=false strategies. The
	// executor can't reconcile this on its own — a fresh-open buy against an
	// existing short nets on the exchange but flips virtually. Collect here,
	// forward to owner DM once the notifier is wired below.
	allowShortsWarnings := ValidatePerpsAllowShortsConfig(state, cfg)

	// #42 / #243: Initialize portfolio peak from sum of capitals on first run.
	// For strategies that share an exchange wallet (e.g. multiple Hyperliquid
	// perps strategies on the same account), use the real on-exchange balance
	// once instead of summing per-strategy capital — otherwise the peak is
	// inflated and the kill switch can fire prematurely.
	if state.PortfolioRisk.PeakValue == 0 {
		total := computeInitialPortfolioPeak(cfg.Strategies, nil)
		state.PortfolioRisk.PeakValue = total
		fmt.Printf("  Portfolio peak initialized: $%.0f\n", total)
	}

	// #244: A latched portfolio kill switch should not survive a restart
	// indefinitely on shared-wallet setups. If the real on-chain balance is
	// fetchable for any shared wallet, treat startup as a safe reset point
	// and unlatch the kill switch. Network failures preserve the latch.
	ClearLatchedKillSwitchSharedWallet(state, cfg.Strategies, defaultSharedWalletBalance)

	// Setup logging
	logMgr, err := NewLogManager(cfg.LogDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to setup logging: %v\n", err)
		os.Exit(1)
	}
	defer logMgr.Close()

	// Mutex for state access (HTTP server reads)
	var mu sync.RWMutex

	// Start HTTP status server. Priority: CLI flag > config > default.
	statusPort := resolveStatusPort(*statusPortFlag, cfg.StatusPort)
	server := NewStatusServer(state, &mu, cfg.StatusToken, cfg.Strategies, stateDB)
	server.Start(statusPort)

	// Graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	stopCh := make(chan struct{})
	go func() {
		sig := <-sigCh
		fmt.Printf("\nReceived %s, saving state and shutting down...\n", sig)
		mu.Lock()
		if err := SaveStateWithDB(state, cfg, stateDB); err != nil {
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
				leaderboardChannel: cfg.Discord.LeaderboardChannel,
				dmChannels:         cfg.Discord.DMChannels,
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
				notifier:   tg,
				channels:   cfg.Telegram.Channels,
				ownerID:    cfg.Telegram.OwnerChatID,
				dmChannels: cfg.Telegram.DMChannels,
				plainText:  true,
			})
			defer tg.Close()
		}
	}

	notifier := NewMultiNotifier(backends...)
	fmt.Printf("Notification backends: %d active\n", notifier.BackendCount())

	// Route trade-persist warnings (#289) to owner DM so operators see
	// immediate trade-DB failures instead of only stderr. Safe to wire after
	// OpenStateDB — any RecordTrade calls between those two points still log
	// to stderr via the nil-check in state.go.
	if notifier.HasOwner() {
		tradePersistWarn = func(msg string) {
			notifier.SendOwnerDM("[state] " + msg)
		}
		// #343: Forward baseline-guard warnings (a SaveState caller tried to
		// rewrite initial_capital) to the owner DM. Dedup is handled inside
		// SaveState — this only fires once per strategy per process lifetime.
		initialCapitalGuardWarn = func(msg string) {
			notifier.SendOwnerDM("[state] " + msg)
		}
	}

	// #343: Forward startup config-driven baseline changes to the owner. Info
	// DMs confirm a deliberate bump; ERROR DMs surface a persist failure where
	// the DB still holds the prior baseline. Both are routine but worth
	// surfacing so the change (or its failure) is visible.
	if notifier.HasOwner() {
		for _, msg := range initialCapitalChangeInfos {
			notifier.SendOwnerDM("[state] " + msg)
		}
		for _, msg := range initialCapitalChangeErrors {
			notifier.SendOwnerDM("[state] ERROR: " + msg)
		}
	}

	// #336: Forward startup AllowShorts warnings to the owner so the desync is
	// surfaced even when the operator isn't tailing stderr.
	if len(allowShortsWarnings) > 0 && notifier.HasOwner() {
		for _, msg := range allowShortsWarnings {
			notifier.SendOwnerDM("[state] " + msg)
		}
	}

	// #339: Forward the missing-state-DB warning to the owner. Captured before
	// OpenStateDB ran (which would have created an empty DB), surfaced here
	// once the notifier is available.
	if missingStateWarning != "" && notifier.HasOwner() {
		notifier.SendOwnerDM("[state] " + missingStateWarning)
	}

	// -summary mode: post snapshot summary for the specified channel and exit.
	// Checked early since it only needs config, state, and notifier — avoids
	// launching the config-migration goroutine, update checks, and pricers
	// that would be hard-killed by os.Exit.
	if *summary != "" {
		runSummaryAndExit(*summary, cfg, state, stateDB, notifier)
	}

	// -leaderboard mode: compute leaderboard on-demand and exit. Issue #313
	// moved this from reading a pre-computed leaderboard.json to fetching fresh
	// prices and building messages at invocation time — data is always current,
	// and the scheduler no longer pays per-cycle compute/IO for data only used
	// by this flag and the daily auto-post.
	if *leaderboard {
		if !notifier.HasBackends() {
			fmt.Fprintf(os.Stderr, "No notification backends configured\n")
			os.Exit(1)
		}
		if len(state.Strategies) == 0 {
			fmt.Fprintln(os.Stderr, "No strategies configured; nothing to post")
			os.Exit(0)
		}
		prices := fetchPricesForSummary(cfg)
		sharpeByStrategy := ComputeSharpeByStrategy(LoadClosedPositionsByStrategy(stateDB, cfg), cfg, state)
		if err := PostLeaderboard(cfg, state, prices, sharpeByStrategy, notifier); err != nil {
			fmt.Fprintf(os.Stderr, "Leaderboard post failed: %v\n", err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	reloadCh := make(chan struct{}, 1)
	hupCh := make(chan os.Signal, 1)
	signal.Notify(hupCh, syscall.SIGHUP)
	defer signal.Stop(hupCh)
	go func() {
		for range hupCh {
			select {
			case reloadCh <- struct{}{}:
			default:
				fmt.Println("[reload] SIGHUP received while reload is pending; coalescing")
			}
		}
	}()

	// Config migration: DM owner about new fields if config is behind current version.
	if cfg.ConfigVersion < CurrentConfigVersion {
		go runConfigMigrationDM(cfg, notifier, *configPath)
	}

	// Track the last remote hash we notified about to avoid re-notifying on every cycle.
	var lastNotifiedHash string

	// Check for updates on startup (best-effort, non-blocking).
	if cfg.AutoUpdate != "off" {
		checkForUpdates(cfg, notifier, &lastNotifiedHash, &mu, state, stateDB)
	}

	// Platform pricers: Deribit uses live API; IBKR uses Black-Scholes with cached spot prices.
	deribitPricer := NewDeribitPricer()
	fmt.Println("Option pricers ready (deribit: live API, ibkr: Black-Scholes)")

	// Track last-run time per strategy for per-strategy intervals.
	// Single-writer invariant: lastRun is only mutated from the scheduler
	// goroutine (this for-loop and the per-strategy continuations below).
	// Reads from the same goroutine (the dueStrategies loop and the
	// schedulerDelay calls) are safe without additional synchronization.
	// If you ever split writes across goroutines, add explicit locking —
	// the existing `mu` lock guards `state`, not `lastRun`.
	lastRun := make(map[string]time.Time)
	// Same single-writer invariant as lastRun; copied into AppState only during
	// the save phase so restart throttling survives without widening state locks.
	lastSummaryPost := cloneTimeMap(state.LastSummaryPost)

	// Determine tick interval from configured strategy intervals, min 60s.
	tickSeconds := schedulerTickSeconds(cfg)
	fmt.Printf("Tick interval: %ds (strategies have individual intervals)\n", tickSeconds)
	drawdownWarnThresholdPct := configuredDrawdownWarnThresholdPct(cfg)

	reloadConfig := func() {
		fmt.Printf("[reload] SIGHUP received; reloading config from %s\n", *configPath)
		nextCfg, err := LoadConfig(*configPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[reload] ERROR: reload failed; keeping previous config: %v\n", err)
			return
		}
		mu.Lock()
		changes, err := applyHotReloadConfig(cfg, nextCfg, state, notifier, server)
		if err != nil {
			mu.Unlock()
			fmt.Fprintf(os.Stderr, "[reload] ERROR: reload rejected; keeping previous config: %v\n", err)
			return
		}
		tickSeconds = schedulerTickSeconds(cfg)
		drawdownWarnThresholdPct = configuredDrawdownWarnThresholdPct(cfg)
		mu.Unlock()

		if len(changes) == 0 {
			fmt.Println("[reload] Config reload applied: no hot-reloadable changes")
		} else {
			fmt.Printf("[reload] Config reload applied (%d changes):\n", len(changes))
			for _, change := range changes {
				fmt.Printf("[reload]   %s\n", change)
			}
		}
		fmt.Printf("[reload] Tick interval now %ds\n", tickSeconds)
	}
	processConfigReloads := func() {
		for {
			select {
			case <-reloadCh:
				reloadConfig()
			default:
				return
			}
		}
	}

	// Wall-clock tracker for cfg.AutoUpdate == "daily". Initialized to now so
	// the first daily check fires after 24h (matching the previous
	// cycle-based behavior). We can't use cycle counts anymore because the
	// scheduler no longer sleeps a fixed tick — schedulerDelay returns a
	// variable delay (1s when due, up to the longest strategy interval
	// otherwise), so cycle increments no longer correspond to wall-clock
	// time.
	lastAutoUpdateCheck := time.Now()

	saveFailures := 0
	resetGoroutineRunning := false

	// Main loop
	for {
		processConfigReloads()

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

		// Compute effective per-strategy intervals once per cycle under
		// RLock; reuse the same map for due-detection and schedulerDelay
		// below to avoid re-entering the lock and recomputing per strategy.
		mu.RLock()
		intervals := effectiveStrategyIntervals(cfg.Strategies, state.Strategies, cfg.IntervalSeconds, drawdownWarnThresholdPct)
		mu.RUnlock()

		// Determine which strategies are due this tick
		dueStrategies := make([]StrategyConfig, 0)
		for _, sc := range cfg.Strategies {
			// #100: Skip strategies where capital_pct is set but capital resolved to $0
			// (balance fetch failed and no fallback capital configured).
			if shouldSkipZeroCapital(sc) {
				fmt.Printf("[ERROR] %s: capital_pct set but capital resolved to $0 — skipping\n", sc.ID)
				continue
			}
			interval := intervals[sc.ID]
			last, exists := lastRun[sc.ID]
			if !exists || cycleStart.Sub(last) >= time.Duration(interval)*time.Second {
				dueStrategies = append(dueStrategies, sc)
			}
		}

		if len(dueStrategies) == 0 {
			// Nothing due, wait for next tick
			delay := schedulerDelay(cfg.Strategies, intervals, lastRun, cfg.IntervalSeconds, time.Now(), tickSeconds)
			timer := time.NewTimer(delay)
			select {
			case <-timer.C:
				continue
			case <-reloadCh:
				timer.Stop()
				reloadConfig()
				processConfigReloads()
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

		// Collect symbols that need prices. Spot strategies use the
		// BinanceUS-formatted symbol directly (e.g. "BTC/USDT").
		// Perps marks now come from the venues they live on — see
		// collectPerpsMarkSymbols below — so perps are intentionally
		// absent from this list, closing the HL-only coin [WARN] noise
		// (#262) as a side effect.
		symbols := collectPriceSymbols(cfg.Strategies)
		// Futures (TopStep CME) on a separate price rail — #261.
		futuresSymbols := collectFuturesMarkSymbols(cfg.Strategies)
		// Perps marks sourced from the venue the position lives on — #263.
		hlPerpsCoins, okxPerpsCoins := collectPerpsMarkSymbols(cfg.Strategies)

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
		}
		// HL perps marks — best-effort; failure falls back to pos.AvgCost.
		if len(hlPerpsCoins) > 0 {
			hlMarks, err := fetchHyperliquidMids(hlPerpsCoins)
			if err != nil {
				fmt.Printf("[WARN] HL perps marks fetch failed for %v: %v — portfolio notional will use entry cost for open HL perps positions\n", hlPerpsCoins, err)
			} else {
				mergePerpsMarks(prices, hlMarks)
				for _, coin := range hlPerpsCoins {
					if _, ok := prices[coin]; !ok {
						fmt.Printf("[WARN] No HL perps mark for %s — PortfolioNotional/Value will fall back to entry cost\n", coin)
					}
				}
			}
		}
		// OKX perps marks — best-effort; failure falls back to pos.AvgCost.
		if len(okxPerpsCoins) > 0 {
			okxMarks, err := fetchOKXPerpsMids(okxPerpsCoins)
			if err != nil {
				fmt.Printf("[WARN] OKX perps marks fetch failed for %v: %v — portfolio notional will use entry cost for open OKX perps positions\n", okxPerpsCoins, err)
			} else {
				mergePerpsMarks(prices, okxMarks)
				for _, coin := range okxPerpsCoins {
					if _, ok := prices[coin]; !ok {
						fmt.Printf("[WARN] No OKX perps mark for %s — PortfolioNotional/Value will fall back to entry cost\n", coin)
					}
				}
			}
		}
		// Futures marks are best-effort: a failed fetch falls back to
		// pos.AvgCost in PortfolioNotional/Value (same as pre-#261), it is
		// NOT a hard cycle skip. Log a [WARN] so stale exposure is visible.
		if len(futuresSymbols) > 0 {
			marks, mode, err := FetchFuturesMarks(futuresSymbols)
			if err != nil {
				fmt.Printf("[WARN] Futures marks fetch failed for %v: %v — portfolio notional will use entry cost for open futures positions\n", futuresSymbols, err)
			} else {
				// Main cycle loop is naturally rate-limited by the tick
				// interval, so the paper_fallback warning can fire
				// unthrottled here — one log per cycle on a sustained
				// downgrade. /status uses a throttled logger instead.
				if mode == FuturesMarkModePaperFallback {
					fmt.Printf("[WARN] fetch_futures_marks: live mode init failed, degraded to paper (yfinance) — check TopStepX creds and network\n")
				}
				mergeFuturesMarks(prices, marks)
				for _, sym := range futuresSymbols {
					if _, ok := prices[sym]; !ok {
						fmt.Printf("[WARN] No futures mark for %s — PortfolioNotional/Value will fall back to entry cost\n", sym)
					}
				}
			}
		}
		if len(prices) > 0 {
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
			// #42 / #243: Portfolio-level risk check before running any strategy.
			//
			// Fetch live Hyperliquid clearinghouseState ONCE per cycle (outside
			// the lock) and reuse it for BOTH the shared-wallet balance (risk
			// check) AND the on-chain position sync below — this halves the HL
			// API round-trips when multiple live HL strategies are configured.
			//
			// Uses real exchange balances for shared wallets so multiple HL
			// perps strategies on the same account don't get double-counted.
			killSwitchFired := false
			notionalBlocked := false

			// Partition HL live strategies up-front: shared-wallet detection
			// must see all strategies in cfg, while position sync only touches
			// due strategies.
			var hlLiveAll []StrategyConfig
			for _, sc := range cfg.Strategies {
				if sc.Platform == "hyperliquid" && sc.Type == "perps" && hyperliquidIsLive(sc.Args) {
					hlLiveAll = append(hlLiveAll, sc)
				}
			}
			var hlLiveDue []StrategyConfig
			for _, sc := range dueStrategies {
				if sc.Platform == "hyperliquid" && sc.Type == "perps" && hyperliquidIsLive(sc.Args) {
					hlLiveDue = append(hlLiveDue, sc)
				}
			}

			// #345: Partition live OKX strategies for the kill-switch close
			// path. Perps and spot are separated because only perps support
			// a reduce-only close; spot is surfaced as a manual-intervention
			// gap in the Discord message (see planKillSwitchClose).
			var okxLivePerps []StrategyConfig
			var okxLiveSpot []StrategyConfig
			for _, sc := range cfg.Strategies {
				if sc.Platform != "okx" || !okxIsLive(sc.Args) {
					continue
				}
				if sc.Type == "perps" {
					okxLivePerps = append(okxLivePerps, sc)
				} else if sc.Type == "spot" {
					okxLiveSpot = append(okxLiveSpot, sc)
				}
			}

			// #346: Partition live Robinhood strategies for the kill-switch
			// close path. Crypto (Type=="spot") is closable via market_sell;
			// options is surfaced as a manual-intervention gap (sell-to-
			// close vs buy-to-close semantics not yet handled).
			var rhLiveCrypto []StrategyConfig
			var rhLiveOptions []StrategyConfig
			for _, sc := range cfg.Strategies {
				if sc.Platform != "robinhood" || !robinhoodIsLive(sc.Args) {
					continue
				}
				if sc.Type == "spot" {
					rhLiveCrypto = append(rhLiveCrypto, sc)
				} else if sc.Type == "options" {
					rhLiveOptions = append(rhLiveOptions, sc)
				}
			}

			// #347: Partition live TopStep futures strategies for the
			// kill-switch close path. TopStepX supports reduce-all via
			// market_close; CME trading-hour restrictions are handled by
			// the latch-until-flat semantic (fires outside RTH stay
			// latched until the venue reopens).
			var tsLiveAll []StrategyConfig
			for _, sc := range cfg.Strategies {
				if sc.Platform == "topstep" && sc.Type == "futures" && topstepIsLive(sc.Args) {
					tsLiveAll = append(tsLiveAll, sc)
				}
			}

			sharedWallets := detectSharedWallets(cfg.Strategies)
			hlAddr := os.Getenv("HYPERLIQUID_ACCOUNT_ADDRESS")
			hlKey := SharedWalletKey{Platform: "hyperliquid", Account: hlAddr}
			_, hlShared := sharedWallets[hlKey]

			// Fetch HL clearinghouseState once if any consumer needs it:
			// - shared-wallet risk check (2+ live HL strategies in cfg)
			// - position sync for at least one due HL strategy
			walletBalances := make(map[SharedWalletKey]float64)
			var hlPositions []HLPosition
			var hlStateFetched bool
			// Fetch clearinghouseState whenever any live HL strategy exists (#356
			// per-strategy circuit closes need fresh positions even if no HL
			// strategy is due this cycle).
			if hlAddr != "" && len(hlLiveAll) > 0 {
				bal, pos, err := fetchHyperliquidState(hlAddr)
				if err != nil {
					fmt.Printf("[WARN] hyperliquid clearinghouseState fetch failed: %v — falling back to per-wallet max and skipping position sync this cycle\n", err)
				} else {
					hlStateFetched = true
					hlPositions = pos
					if hlShared {
						walletBalances[hlKey] = bal
					}
				}
			}

			// #360: Fetch OKX positions once if any live OKX perps strategy
			// exists. Drives per-strategy circuit-breaker pending closes
			// (PlatformRiskAssist.OKXPositions). Gated on OKX_API_KEY so
			// paper-only configs skip the subprocess entirely.
			okxHasCreds := os.Getenv("OKX_API_KEY") != ""
			okxKey := SharedWalletKey{Platform: "okx", Account: os.Getenv("OKX_API_KEY")}
			_, okxShared := sharedWallets[okxKey]
			var okxPositions []OKXPosition
			var okxStateFetched bool
			if okxHasCreds && len(okxLivePerps) > 0 {
				pos, err := defaultOKXPositionsFetcher()
				if err != nil {
					fmt.Printf("[WARN] okx fetch_positions failed: %v — skipping per-strategy OKX circuit enqueue this cycle\n", err)
				} else {
					okxStateFetched = true
					okxPositions = pos
				}
			}
			// #360 phase 2 of #357: fetch the unified USDT balance for the
			// shared-wallet risk check when 2+ live OKX perps strategies share
			// an API key. Independent subprocess so a fetch_positions outage
			// doesn't starve the balance read.
			if okxHasCreds && okxShared {
				if bal, err := defaultSharedWalletBalance("okx"); err != nil {
					fmt.Printf("[WARN] okx balance fetch failed: %v — falling back to per-wallet max this cycle\n", err)
				} else {
					walletBalances[okxKey] = bal
				}
			}
			// #362: Fetch TopStep positions once per cycle when any live TS
			// futures strategy exists, so per-strategy CB enqueue
			// (setTopStepCircuitBreakerPending) has a sizing source. The
			// kill-switch plan builder has its own fetch path; we let it do
			// its own call rather than plumb a pre-fetched TS slice through
			// KillSwitchCloseInputs — the fetch is cheap (one HTTPS call)
			// and keeps the kill-switch plumbing untouched.
			var tsPositions []TopStepPosition
			var tsStateFetched bool
			if len(tsLiveAll) > 0 {
				pos, err := defaultTopStepPositionsFetcher()
				if err != nil {
					fmt.Printf("[WARN] topstep positions fetch failed: %v — per-strategy CB will use stuck-CB recovery on next cycle\n", err)
				} else {
					tsStateFetched = true
					tsPositions = pos
				}
			}

			mu.RLock()
			totalPV, usedPVFallback := computeTotalPortfolioValue(cfg.Strategies, state, prices, walletBalances, sharedWallets)
			totalNotional := PortfolioNotional(state.Strategies, prices)
			// #296: aggregate perps margin drawdown inputs alongside the
			// equity total so the portfolio kill switch can fire on a
			// leveraged margin blow-up that would otherwise hide inside
			// equity-only drawdown for all-perps accounts.
			perpsLoss, perpsMargin := AggregatePerpsMarginInputs(state.Strategies, cfg.Strategies, prices)
			mu.RUnlock()

			mu.Lock()
			// #243: Freeze peak during fallback cycles so a transient HL API
			// blip cannot ratchet the high-water mark (peak is sticky, so a
			// false peak would persist and could later trip a false drawdown).
			// CheckPortfolioRisk auto-ratchets PeakValue when totalValue > peak;
			// we snapshot before the call and restore if we're on a fallback
			// cycle. Drawdown detection still runs against the frozen peak.
			origPeak := state.PortfolioRisk.PeakValue
			prevWarningSent := state.PortfolioRisk.WarningSent
			portfolioAllowed, nb, portfolioWarning, portfolioReason := CheckPortfolioRisk(&state.PortfolioRisk, cfg.PortfolioRisk, totalPV, totalNotional, perpsLoss, perpsMargin)
			// True only on the cycle that first enters the warn band; false on
			// repeat cycles while still in band. Used to gate kill-switch event
			// log appends — notifications still fire every cycle via portfolioWarning.
			portfolioWarnBandEntered := portfolioWarning && !prevWarningSent
			if usedPVFallback && state.PortfolioRisk.PeakValue > origPeak {
				state.PortfolioRisk.PeakValue = origPeak
			}
			if !portfolioAllowed {
				killSwitchFired = true
				fmt.Printf("[CRITICAL] Portfolio kill switch: %s\n", portfolioReason)
				// Virtual state mutation deferred until live closes confirm
				// flat — see below. Without that gate, virtual state diverged
				// from the exchange (#341): closing virtually but never sending
				// the reduce-only order left on-chain positions live, and once
				// virtual was empty no future cycle could detect the leak.
				// Portfolio kill owns all live closes — drop per-strategy pending
				// so per-strategy drains don't double-submit against an
				// already-flattening venue. Operator-required pendings (OKX
				// spot / RH options, #363) are also cleared: the portfolio
				// kill path already surfaces those gaps via
				// formatKillSwitchMessage, so leaving both sets of warnings
				// in place would double-notify the operator.
				for _, ss := range state.Strategies {
					if ss != nil {
						ss.RiskState.clearPendingCircuitClose(PlatformPendingCloseHyperliquid)
						ss.RiskState.clearPendingCircuitClose(PlatformPendingCloseOKX)
						ss.RiskState.clearPendingCircuitClose(PlatformPendingCloseOKXSpot)
						ss.RiskState.clearPendingCircuitClose(PlatformPendingCloseRobinhood)
						ss.RiskState.clearPendingCircuitClose(PlatformPendingCloseRobinhoodOptions)
						ss.RiskState.clearPendingCircuitClose(PlatformPendingCloseTopStep)
					}
				}
			}
			notionalBlocked = nb
			if notionalBlocked {
				fmt.Printf("[WARN] %s\n", portfolioReason)
			}
			mu.Unlock()

			// #341 / #345 / #346 / #347: Submit market closes to
			// Hyperliquid, OKX, Robinhood, AND TopStep for every non-zero
			// on-chain / on-account position belonging to a configured
			// live strategy. The planning step (planKillSwitchClose)
			// runs OUTSIDE the lock — the close scripts are subprocesses
			// that can take seconds. OKX spot and Robinhood options are
			// surfaced as known gaps (no unified close primitive).
			//
			// Latch semantics: virtual state is mutated only when
			// plan.OnChainConfirmedFlat is true (either platform failing
			// flips the flag). Otherwise the kill switch stays latched and
			// the next cycle re-enters this branch (CheckPortfolioRisk
			// early-returns false while KillSwitchActive is true) and retries.
			var plan KillSwitchClosePlan
			var hlVirtualQty hlVirtualQuantitySnapshot
			if killSwitchFired {
				// Snapshot per-coin StopLossOIDs so the kill-switch close
				// path can cancel resting SLs before flattening, freeing
				// HL's open-order cap (#421 review point 1, #479).
				// Sole-source: every live HL strategy's Position for the
				// coin it trades. Shared coins may have multiple
				// per-strategy SL triggers, so preserve every OID.
				hlSLOIDs := map[string][]int64{}
				mu.RLock()
				for _, sc := range hlLiveAll {
					sym := hyperliquidSymbol(sc.Args)
					if sym == "" {
						continue
					}
					if ss, ok := state.Strategies[sc.ID]; ok && ss != nil {
						if pos, pok := ss.Positions[sym]; pok && pos != nil && pos.StopLossOID > 0 {
							hlSLOIDs[sym] = appendUniquePositiveStopLossOID(hlSLOIDs[sym], pos.StopLossOID)
						}
					}
				}
				hlVirtualQty = snapshotHyperliquidVirtualQuantities(state.Strategies, hlLiveAll)
				mu.RUnlock()

				inputs := KillSwitchCloseInputs{
					HLAddr:          hlAddr,
					HLStateFetched:  hlStateFetched,
					HLPositions:     hlPositions,
					HLLiveAll:       hlLiveAll,
					HLCloser:        defaultHyperliquidLiveCloser,
					HLFetcher:       defaultHLStateFetcher,
					HLStopLossOIDs:  hlSLOIDs,
					OKXLiveAllPerps: okxLivePerps,
					OKXLiveAllSpot:  okxLiveSpot,
					OKXCloser:       defaultOKXLiveCloser,
					OKXFetcher:      defaultOKXPositionsFetcher,
					RHLiveCrypto:    rhLiveCrypto,
					RHLiveOptions:   rhLiveOptions,
					RHCloser:        defaultRobinhoodLiveCloser,
					RHFetcher:       defaultRobinhoodPositionsFetcher,
					TSLiveAll:       tsLiveAll,
					TSCloser:        defaultTopStepLiveCloser,
					TSFetcher:       defaultTopStepPositionsFetcher,
					PortfolioReason: portfolioReason,
					CloseTimeout:    90 * time.Second,
					// Per-platform overrides: each platform gets its own
					// independent context.WithTimeout so a slow platform
					// cannot starve the others. Robinhood adds TOTP login
					// overhead to every submit, so it gets a wider budget;
					// the rest stay at the 90s default. (#350)
					RHCloseTimeout: 150 * time.Second,
				}
				plan = planKillSwitchClose(inputs)
				for _, line := range plan.LogLines {
					fmt.Println(line)
				}
			}

			killSwitchAutoReset := false
			if killSwitchFired && plan.OnChainConfirmedFlat {
				mu.Lock()
				for _, sc := range cfg.Strategies {
					if s, ok := state.Strategies[sc.ID]; ok {
						forceCloseKillSwitchPositions(s, sc, prices, plan.CloseReport.Fills, hlLiveAll, hlVirtualQty, nil)
						// Pending HL circuit close was already cleared above
						// when portfolio kill fired (line ~611); nothing to do
						// here. The per-strategy pending field is owned by the
						// portfolio kill path once it takes over flattening.
					}
				}
				if !notifier.HasOwner() {
					if plan.CanAutoResetWithoutOwner() {
						killSwitchAutoReset = AutoResetConfirmedFlatKillSwitch(&state.PortfolioRisk, totalPV,
							"confirmed flat after portfolio kill-switch close; no DM owner configured, latch auto-cleared")
						if killSwitchAutoReset {
							fmt.Printf("[CRITICAL] Portfolio kill switch auto-reset after confirmed flat close (no owner configured, peak re-baselined to $%.2f)\n", totalPV)
						}
					} else {
						fmt.Println("[CRITICAL] Portfolio kill switch auto-reset suppressed: operator-required close gaps remain")
					}
				}
				mu.Unlock()
			}

			if killSwitchFired && notifier.HasBackends() && plan.DiscordMessage != "" {
				killSwitchMsg := plan.DiscordMessage
				if killSwitchAutoReset {
					killSwitchMsg = formatKillSwitchAutoResetMessage(killSwitchMsg)
				}
				notifier.SendToAllChannels(killSwitchMsg)
			}

			// Warning alert: drawdown approaching kill switch threshold.
			if portfolioWarning && notifier.HasBackends() {
				mu.Lock()
				// Source mirrors CheckPortfolioRisk's tie-break: margin wins
				// ties so the newer #296 signal is surfaced preferentially.
				source := "equity"
				warnDD := state.PortfolioRisk.CurrentDrawdownPct
				if state.PortfolioRisk.CurrentMarginDrawdownPct >= warnDD {
					source = "margin"
					warnDD = state.PortfolioRisk.CurrentMarginDrawdownPct
				}
				// Only append a kill-switch event on the transition INTO the
				// warn band. Notifications repeat every cycle (portfolioWarning)
				// but the 50-entry ring buffer must not be evicted by repeating
				// "warning" entries that drown out triggered/reset transitions.
				if portfolioWarnBandEntered {
					addKillSwitchEvent(&state.PortfolioRisk, "warning", source, warnDD, totalPV, state.PortfolioRisk.PeakValue, portfolioReason)
				}
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
					addKillSwitchEvent(&state.PortfolioRisk, "reset", "", state.PortfolioRisk.CurrentDrawdownPct, 0, state.PortfolioRisk.PeakValue, "manual reset via DM")
					if err := SaveStateWithDB(state, cfg, stateDB); err != nil {
						fmt.Printf("[CRITICAL] Failed to save state after kill switch reset: %v\n", err)
					}
					mu.Unlock()
					notifier.SendOwnerDM("Kill switch reset. Trading will resume next cycle.")
					fmt.Println("[update] Kill switch reset by owner via DM")
				}()
			}

			if !killSwitchFired {
				// #356: Live HL per-strategy circuit breaker closes (reduce-only,
				// proportional on shared coins). Runs before reconcile so the next
				// sync sees updated on-chain sizes.
				if len(hlLiveAll) > 0 {
					runPendingHyperliquidCircuitCloses(
						context.Background(),
						state,
						cfg.Strategies,
						hlAddr,
						hlPositions,
						hlStateFetched,
						defaultHLStateFetcher,
						defaultHyperliquidLiveCloser,
						90*time.Second,
						&mu,
						notifier.SendOwnerDM,
					)
				}
				// #360: Live OKX per-strategy circuit breaker closes. Same shape
				// as the HL drain — the pending map is keyed per platform.
				if len(okxLivePerps) > 0 && okxHasCreds {
					runPendingOKXCircuitCloses(
						context.Background(),
						state,
						cfg.Strategies,
						okxHasCreds,
						okxPositions,
						okxStateFetched,
						defaultOKXPositionsFetcher,
						defaultOKXLiveCloser,
						90*time.Second,
						&mu,
						notifier.SendOwnerDM,
					)
				}
				// #362: Live TopStep per-strategy circuit breaker closes
				// (market_close full-flatten, sole-peer only — whole-contract
				// futures have no partial-close primitive). Outside-RTH
				// rejections and other TopStepX errors keep the pending
				// latched; the drain retries on the next cycle.
				if len(tsLiveAll) > 0 {
					runPendingTopStepCircuitCloses(
						context.Background(),
						state,
						cfg.Strategies,
						tsPositions,
						tsStateFetched,
						defaultTopStepPositionsFetcher,
						defaultTopStepLiveCloser,
						90*time.Second,
						&mu,
						notifier.SendOwnerDM,
					)
				}
				// #361 phase 3: Live Robinhood crypto per-strategy circuit breaker
				// closes. RH crypto has no reduce-only primitive, so each pending
				// leg is a full-account market_sell guarded by a sole-ownership
				// gate (DM the owner when a shared-coin config prevents a safe
				// close). Lazy fetch — drain only calls the positions fetcher
				// when pending/stuck-CB work is present, so idle cycles skip the
				// TOTP login round-trip entirely.
				if len(rhLiveCrypto) > 0 {
					runPendingRobinhoodCircuitCloses(
						context.Background(),
						state,
						cfg.Strategies,
						nil,
						false,
						defaultRobinhoodPositionsFetcher,
						defaultRobinhoodLiveCloser,
						notifier.SendOwnerDM,
						150*time.Second,
						&mu,
					)
				}
				// #363 phase 5: operator-gap per-strategy CB pending closes.
				// OKX spot and Robinhood options have no safe automated close
				// primitive — the drain emits a CRITICAL warning each cycle
				// instead of submitting orders. Pending stays set until the
				// operator flattens manually.
				drainOperatorRequiredPendingCloses(state, notifier, &mu)
				// Pre-phase: sync on-chain positions for due live HL strategies.
				// Reuses the clearinghouseState already fetched above for the
				// shared-wallet risk check (#243 review feedback) so we don't
				// pay two HL API round-trips per cycle.
				if len(hlLiveDue) > 0 && hlStateFetched {
					reconcileHyperliquidAccountPositions(hlLiveDue, hlLiveAll, state, &mu, logMgr, hlPositions)
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

					hlLiveStrategy := sc.Type == "perps" && sc.Platform == "hyperliquid" && hyperliquidIsLive(sc.Args)
					// OKX serves both spot and perps in this snapshot block (see
					// dispatch at the spot/perps cases below), so the live flag
					// stays platform-only — adding sc.Type would regress one of
					// the two paths.
					okxLiveStrategy := sc.Platform == "okx" && okxIsLive(sc.Args)
					rhLiveStrategy := sc.Type == "spot" && sc.Platform == "robinhood" && robinhoodIsLive(sc.Args)
					tsLiveStrategy := sc.Type == "futures" && sc.Platform == "topstep" && topstepIsLive(sc.Args)

					// Phase 1: RLock — read inputs needed for subprocess
					mu.RLock()
					pv := PortfolioValue(stratState, prices)
					var posJSON string
					if sc.Type == "options" {
						posJSON = EncodeAllPositionsJSON(stratState.OptionPositions, stratState.Positions)
					}
					var spotPosCtx PositionCtx
					if sc.Type == "spot" && sc.Platform != "okx" && sc.Platform != "robinhood" {
						if sym := spotSymbol(sc.Args); sym != "" {
							spotPosCtx = positionCtxForSymbol(stratState, sym)
						}
					}
					var hlCash float64
					var hlPosQty float64
					var hlPosSide string
					var hlAvgCost float64
					var hlEntryATR float64
					var hlPosCtx PositionCtx
					var hlStopLossOID int64
					var hlStopLossTriggerPx float64
					var hlStopLossHighWaterPx float64
					if sc.Type == "perps" && sc.Platform == "hyperliquid" {
						if hlLiveStrategy {
							hlCash = stratState.Cash
						}
						// Live-order sizing/cancel snapshots below are intentionally
						// consumed only inside live execution branches. Paper paths
						// should continue using PositionCtx only for close evaluation.
						if sym := hyperliquidSymbol(sc.Args); sym != "" {
							if pos, ok := stratState.Positions[sym]; ok {
								hlPosCtx = positionCtxFromPosition(pos)
								hlPosSide = hlPosCtx.Side
								hlPosQty = hlPosCtx.Quantity
								hlAvgCost = hlPosCtx.AvgCost
								hlEntryATR = pos.EntryATR
								hlStopLossOID = pos.StopLossOID
								hlStopLossTriggerPx = pos.StopLossTriggerPx
								hlStopLossHighWaterPx = pos.StopLossHighWaterPx
							}
						}
					}
					var okxCash float64
					var okxPosQty float64
					var okxPosSide string
					var okxAvgCost float64
					var okxPosCtx PositionCtx
					if sc.Platform == "okx" {
						if okxLiveStrategy {
							okxCash = stratState.Cash
						}
						if sym := okxSymbol(sc.Args); sym != "" {
							if pos, ok := stratState.Positions[sym]; ok {
								okxPosCtx = positionCtxFromPosition(pos)
								okxPosSide = okxPosCtx.Side
								okxPosQty = okxPosCtx.Quantity
								okxAvgCost = okxPosCtx.AvgCost
							}
						}
					}
					var rhCash float64
					var rhPosQty float64
					var rhPosSide string
					var rhPosCtx PositionCtx
					if sc.Platform == "robinhood" {
						if rhLiveStrategy {
							rhCash = stratState.Cash
						}
						if sym := robinhoodSymbol(sc.Args); sym != "" {
							if pos, ok := stratState.Positions[sym]; ok {
								rhPosCtx = positionCtxFromPosition(pos)
								rhPosSide = rhPosCtx.Side
								rhPosQty = rhPosCtx.Quantity
							}
						}
					}
					var tsCash float64
					var tsContracts float64
					var tsPosSide string
					var tsPosCtx PositionCtx
					if sc.Type == "futures" {
						if tsLiveStrategy {
							tsCash = stratState.Cash
						}
						if sym := topstepSymbol(sc.Args); sym != "" {
							if pos, ok := stratState.Positions[sym]; ok {
								tsPosCtx = positionCtxFromPosition(pos)
								tsPosSide = tsPosCtx.Side
								tsContracts = tsPosCtx.Quantity
							}
						}
					}
					mu.RUnlock()

					// Phase 2: Lock — CheckRisk (fast, no I/O)
					var riskAssist *PlatformRiskAssist
					needHL := hlStateFetched && len(hlLiveAll) > 0
					needOKX := okxStateFetched && len(okxLivePerps) > 0
					needTS := tsStateFetched && len(tsLiveAll) > 0
					if needHL || needOKX || needTS {
						riskAssist = &PlatformRiskAssist{}
						if needHL {
							riskAssist.HLPositions = hlPositions
							riskAssist.HLLiveAll = hlLiveAll
						}
						if needOKX {
							riskAssist.OKXPositions = okxPositions
							riskAssist.OKXLiveAll = okxLivePerps
						}
						if needTS {
							riskAssist.TSPositions = tsPositions
							riskAssist.TSLiveAll = tsLiveAll
						}
					}
					mu.Lock()
					allowed, reason := CheckRisk(&sc, stratState, pv, prices, logger, riskAssist)
					mu.Unlock()
					if !allowed {
						notifyPerStrategyCircuitBreaker(sc, reason, pv, notifier, killSwitchFired)
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
							if result, signalStr, price, ok := runOKXCheck(sc, prices, okxPosCtx, logger); ok {
								prices[result.Symbol] = price
								var execResult *OKXExecuteResult
								liveExecFailed := false
								if okxIsLive(sc.Args) && result.Signal != 0 {
									if er, ok2 := runOKXExecuteOrder(sc, result, price, okxCash, okxPosQty, okxPosSide, okxAvgCost, notifier, logger); ok2 {
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
							if result, signalStr, price, ok := runRobinhoodCheck(sc, prices, rhPosCtx, logger); ok {
								prices[result.Symbol] = price
								var execResult *RobinhoodExecuteResult
								liveExecFailed := false
								if robinhoodIsLive(sc.Args) && result.Signal != 0 {
									if er, ok2 := runRobinhoodExecuteOrder(sc, result, price, rhCash, rhPosQty, rhPosSide, notifier, logger); ok2 {
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
						} else if result, signalStr, price, ok := runSpotCheck(sc, prices, spotPosCtx, logger); ok {
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
							if result, signalStr, price, ok := runOKXCheck(sc, prices, okxPosCtx, logger); ok {
								prices[result.Symbol] = price
								var execResult *OKXExecuteResult
								liveExecFailed := false
								if okxIsLive(sc.Args) && result.Signal != 0 {
									if er, ok2 := runOKXExecuteOrder(sc, result, price, okxCash, okxPosQty, okxPosSide, okxAvgCost, notifier, logger); ok2 {
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
						} else if result, signalStr, price, ok := runHyperliquidCheck(sc, prices, hlPosCtx, logger); ok {
							prices[result.Symbol] = price
							var execResult *HyperliquidExecuteResult
							liveExecFailed := false
							if hyperliquidIsLive(sc.Args) && result.Signal == 0 && hlPosQty > 0 && effectiveTrailingStopPct(sc, &Position{AvgCost: hlAvgCost, EntryATR: hlEntryATR}) > 0 {
								newHighWater, slUpdate, updateConfirmed := runHyperliquidTrailingStopUpdate(sc, result.Symbol, hlPosSide, hlPosQty, hlAvgCost, hlEntryATR, price, hlStopLossHighWaterPx, hlStopLossTriggerPx, hlStopLossOID, notifier, logger)
								mu.Lock()
								if pos, ok3 := stratState.Positions[result.Symbol]; ok3 && pos.Quantity > 0 && pos.Side == hlPosSide {
									if newHighWater > 0 && updateConfirmed {
										pos.StopLossHighWaterPx = newHighWater
									}
									if slUpdate != nil {
										if slUpdate.StopLossFilledImmediately && slUpdate.StopLossTriggerPx > 0 {
											if recordPerpsStopLossClose(stratState, result.Symbol, slUpdate.StopLossTriggerPx, "trailing_stop_loss_immediate", logger) {
												trades++
												detail = fmt.Sprintf("[%s] LIVE TRAILING SL %s @ $%.2f", sc.ID, result.Symbol, slUpdate.StopLossTriggerPx)
											}
										} else if slUpdate.StopLossOID > 0 {
											pos.StopLossOID = slUpdate.StopLossOID
											pos.StopLossTriggerPx = slUpdate.StopLossTriggerPx
											logger.Info("Trailing SL trigger updated oid=%d @ $%.4f", slUpdate.StopLossOID, slUpdate.StopLossTriggerPx)
										} else if slUpdate.CancelStopLossSucceeded && hlStopLossOID > 0 && pos.StopLossOID == hlStopLossOID {
											pos.StopLossOID = 0
											pos.StopLossTriggerPx = 0
											logger.Warn("Trailing SL old OID=%d was cancelled but replacement did not rest", hlStopLossOID)
										}
									}
								}
								mu.Unlock()
							}
							if hyperliquidIsLive(sc.Args) && result.Signal != 0 {
								er, ok2 := runHyperliquidExecuteOrder(sc, result, price, hlCash, hlPosQty, hlPosSide, hlAvgCost, hlStopLossOID, notifier, logger)
								if ok2 {
									execResult = er
								} else {
									liveExecFailed = true
									// Even on failure, if the Python side
									// confirmed the stale-SL cancel went
									// through, drop the dead OID so the next
									// cycle doesn't try to cancel it again.
									if er != nil && er.CancelStopLossSucceeded && hlStopLossOID > 0 {
										sym := hyperliquidSymbol(sc.Args)
										if sym != "" {
											mu.Lock()
											if pos, ok3 := stratState.Positions[sym]; ok3 && pos.StopLossOID == hlStopLossOID {
												pos.StopLossOID = 0
												logger.Info("cleared stale SL OID=%d after open failed but cancel succeeded", hlStopLossOID)
											}
											mu.Unlock()
										}
									}
								}
							}
							if !liveExecFailed {
								mu.Lock()
								trades, detail = executeHyperliquidResult(sc, stratState, result, execResult, signalStr, price, logger)
								mu.Unlock()
							}
						}
					case "futures":
						if result, signalStr, price, ok := runTopStepCheck(sc, prices, tsPosCtx, logger); ok {
							prices[result.Symbol] = price
							var execResult *TopStepExecuteResult
							liveExecFailed := false
							if topstepIsLive(sc.Args) && result.Signal != 0 {
								if er, ok2 := runTopStepExecuteOrder(sc, result, price, tsCash, tsContracts, tsPosSide, notifier, logger); ok2 {
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

		// Pre-compute closed-position history once per cycle so per-channel /
		// per-asset Sharpe calls (and the later ComputeSharpeByStrategy for
		// leaderboard summaries) don't each re-query the DB. Nil when stateDB
		// is nil — downstream callers treat that as "Sharpe unavailable".
		closedByStrategy := LoadClosedPositionsByStrategy(stateDB, cfg)
		rfr := RiskFreeRateOrDefault(cfg)

		// Lifetime round-trip / W-L stats sourced from the trades table (#455).
		// One DB round-trip per cycle; missing keys render as zero inside
		// FormatCategorySummary. Errors are downgraded to a nil map so the
		// summary still posts without in-memory lifetime counters (#472).
		var lifetimeStats map[string]LifetimeTradeStats
		if stateDB != nil {
			if ls, err := stateDB.LifetimeTradeStatsAll(); err != nil {
				fmt.Printf("[summary] lifetime trade stats unavailable: %v\n", err)
			} else {
				lifetimeStats = ls
			}
		}

		// Notification — one message per channel per asset, sent to all backends.
		if notifier.HasBackends() {
			summaryNow := time.Now().UTC()
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
				// Per-channel summary cadence (#30). Legacy default: continuous
				// channel types (options/perps/futures) post every channel run; spot
				// posts hourly. Override per channel via cfg.SummaryFrequency.
				// Trades always force a post so operators see executions
				// immediately regardless of cadence.
				continuous := isOptionsType(chStrats) || isFuturesType(chStrats) || isPerpsType(chStrats)
				if !ShouldPostSummary(cfg.SummaryFrequency[chKey], continuous, chTrades > 0, lastSummaryPost[chKey], summaryNow) {
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
					chSharpe := aggregateSharpe(closedByStrategy, chStrats, state, rfr)
					msgs := FormatCategorySummary(cycle, elapsed, len(dueStrategies), chTrades, chValue, prices, chDetails, chStrats, state, chKey, "", cfg.IntervalSeconds, chSharpe, lifetimeStats)
					for _, msg := range msgs {
						notifier.SendToChannel(chKey, chKey, msg)
					}
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
						assetSharpe := aggregateSharpe(closedByStrategy, assetStrats, state, rfr)
						msgs := FormatCategorySummary(cycle, elapsed, len(dueStrategies), assetTrades, assetValue, prices, assetDetails, assetStrats, state, chKey, asset, cfg.IntervalSeconds, assetSharpe, lifetimeStats)
						for _, msg := range msgs {
							notifier.SendToChannel(chKey, chKey, msg)
						}
					}
				}
				lastSummaryPost[chKey] = summaryNow
			}
			mu.RUnlock()
		}

		// Save state after each cycle
		mu.Lock()
		state.LastCycle = time.Now().UTC()
		state.LastSummaryPost = cloneTimeMap(lastSummaryPost)

		// Periodic configurable leaderboard summaries (#308). Compute + update
		// state.LastLeaderboardSummaries under Lock; post outside so Discord
		// HTTPS latency can't stall the scheduler cycle.
		var duePending []pendingLeaderboardSummary
		if notifier.HasBackends() {
			duePending = collectDueLeaderboardSummaries(cfg, state, prices, ComputeSharpeByStrategy(closedByStrategy, cfg, state))
		}

		if err := SaveStateWithDB(state, cfg, stateDB); err != nil {
			saveFailures++
			fmt.Printf("[CRITICAL] Save state failed (%d/3): %v\n", saveFailures, err)
		} else {
			saveFailures = 0
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

		// Post any configurable leaderboard summaries (#308) outside the lock.
		for _, p := range duePending {
			if err := notifier.SendMessage(p.channel, p.msg); err != nil {
				fmt.Printf("[WARN] Leaderboard summary send to channel %s failed: %v\n", p.channel, err)
				continue
			}
			fmt.Printf("[leaderboard-summary] Posted key=%s top_n=%d channel=%s\n",
				p.key, p.topN, p.channel)
		}

		// Post leaderboard outside the lock to avoid holding mu during I/O.
		// Issue #313: compute on-demand at post time instead of reading a
		// pre-computed file. Build messages under RLock (state read), then
		// post without the lock held so Discord HTTPS latency can't stall
		// other goroutines.
		if postLeaderboard {
			// Persist LastLeaderboardPostDate immediately so a crash before
			// the next cycle's SaveState cannot cause a duplicate daily post
			// on restart.
			stampDate := func() {
				mu.Lock()
				state.LastLeaderboardPostDate = time.Now().UTC().Format("2006-01-02")
				if err := SaveStateWithDB(state, cfg, stateDB); err != nil {
					fmt.Printf("[WARN] Leaderboard post-date save failed: %v\n", err)
				}
				mu.Unlock()
			}
			if len(cfg.Strategies) == 0 {
				fmt.Println("[leaderboard] Auto-post skipped: no strategies configured")
				stampDate()
			} else {
				sharpeByStrategy := ComputeSharpeByStrategy(closedByStrategy, cfg, state)
				mu.RLock()
				lbMessages := BuildLeaderboardMessages(cfg, state, prices, sharpeByStrategy)
				mu.RUnlock()
				if len(lbMessages) == 0 {
					fmt.Println("[leaderboard] Auto-post skipped: no strategy state to leaderboard yet")
					stampDate()
				} else {
					fmt.Printf("[leaderboard] Auto-posting daily leaderboard (configured time: %s UTC)\n", cfg.LeaderboardPostTime)
					if err := postLeaderboardMessages(lbMessages, notifier); err != nil {
						fmt.Printf("[WARN] Leaderboard auto-post failed: %v\n", err)
					} else {
						stampDate()
					}
				}
			}
		}

		// Periodic update check (heartbeat: every cycle; daily: once per
		// 24h wall-clock — was cycle-based, broke when schedulerDelay
		// became variable, see lastAutoUpdateCheck above).
		if cfg.AutoUpdate == "heartbeat" {
			checkForUpdates(cfg, notifier, &lastNotifiedHash, &mu, state, stateDB)
		} else if cfg.AutoUpdate == "daily" && time.Since(lastAutoUpdateCheck) >= 24*time.Hour {
			checkForUpdates(cfg, notifier, &lastNotifiedHash, &mu, state, stateDB)
			lastAutoUpdateCheck = time.Now()
		}

		if *once {
			fmt.Println("--once flag set, exiting after single cycle.")
			return
		}

		// Wait for next tick or shutdown. Recompute intervals here under
		// RLock — drawdown state may have changed during the cycle, so
		// re-evaluating ensures a strategy that just entered (or exited)
		// the warning band gets the fast (or slow) cadence immediately.
		mu.RLock()
		endIntervals := effectiveStrategyIntervals(cfg.Strategies, state.Strategies, cfg.IntervalSeconds, drawdownWarnThresholdPct)
		mu.RUnlock()
		delay := schedulerDelay(cfg.Strategies, endIntervals, lastRun, cfg.IntervalSeconds, time.Now(), tickSeconds)
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
			// Next tick
		case <-reloadCh:
			timer.Stop()
			reloadConfig()
			processConfigReloads()
		case <-stopCh:
			timer.Stop()
			fmt.Println("Shutdown complete.")
			return
		}
	}
}

// runSummaryAndExit posts a snapshot summary for the given channel key and exits.
//
// Lookup order (#308):
//  1. If channelKey matches a cfg.LeaderboardSummaries[].Channel, build and
//     post that configured leaderboard (platform + optional ticker + topN).
//  2. Otherwise fall back to the legacy asset-grouped category summary, which
//     requires strategies whose notifier-resolved channel key equals channelKey.
//
// It fetches current prices, formats the summary, posts to all notification
// backends, and exits immediately.
func runSummaryAndExit(channelKey string, cfg *Config, state *AppState, sdb *StateDB, notifier *MultiNotifier) {
	if !notifier.HasBackends() {
		fmt.Fprintf(os.Stderr, "No notification backends configured\n")
		os.Exit(1)
	}

	// #308: Manual trigger for configured leaderboard summaries.
	if lcs := findLeaderboardSummariesByChannel(cfg, channelKey); len(lcs) > 0 {
		runLeaderboardSummariesAndExit(lcs, cfg, state, sdb, notifier)
		return
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

	// Collect spot symbols; perps/futures go through augmentMarksBestEffort.
	symbols := collectPriceSymbols(cfg.Strategies)

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
	augmentMarksBestEffort(cfg, prices)

	// Calculate channel value.
	chValue := 0.0
	for _, sc := range chStrats {
		if s, ok := state.Strategies[sc.ID]; ok {
			chValue += PortfolioValue(s, prices)
		}
	}

	// Format and send summary using the same asset-grouping logic as the main loop.
	closedByStrategy := LoadClosedPositionsByStrategy(sdb, cfg)
	rfr := RiskFreeRateOrDefault(cfg)
	var lifetimeStats map[string]LifetimeTradeStats
	if sdb != nil {
		if ls, err := sdb.LifetimeTradeStatsAll(); err != nil {
			fmt.Printf("[summary] lifetime trade stats unavailable: %v\n", err)
		} else {
			lifetimeStats = ls
		}
	}
	assetGroups, assetKeys := groupByAsset(chStrats)
	if len(assetKeys) <= 1 {
		chSharpe := aggregateSharpe(closedByStrategy, chStrats, state, rfr)
		msgs := FormatCategorySummary(state.CycleCount, 0, 0, 0, chValue, prices, nil, chStrats, state, channelKey, "", cfg.IntervalSeconds, chSharpe, lifetimeStats)
		for _, msg := range msgs {
			notifier.SendToChannel(channelKey, channelKey, msg)
			fmt.Println(msg)
		}
	} else {
		for _, asset := range assetKeys {
			assetStrats := assetGroups[asset]
			assetValue := 0.0
			for _, sc := range assetStrats {
				if s, ok := state.Strategies[sc.ID]; ok {
					assetValue += PortfolioValue(s, prices)
				}
			}
			assetSharpe := aggregateSharpe(closedByStrategy, assetStrats, state, rfr)
			msgs := FormatCategorySummary(state.CycleCount, 0, 0, 0, assetValue, prices, nil, assetStrats, state, channelKey, asset, cfg.IntervalSeconds, assetSharpe, lifetimeStats)
			for _, msg := range msgs {
				notifier.SendToChannel(channelKey, channelKey, msg)
				fmt.Println(msg)
			}
		}
	}

	fmt.Printf("-summary=%s: posted, exiting.\n", channelKey)
	os.Exit(0)
}

// spotSymbol extracts the spot symbol from strategy args (e.g. "BTC/USDT").
func spotSymbol(args []string) string {
	if len(args) >= 2 {
		return args[1]
	}
	return ""
}

// runSpotCheck runs the spot check subprocess and returns the parsed result.
// No state access. Returns (result, signalStr, price, ok); ok=false means skip execution.
func runSpotCheck(sc StrategyConfig, prices map[string]float64, posCtx PositionCtx, logger *StrategyLogger) (*SpotResult, string, float64, bool) {
	args := append([]string{}, sc.Args...)
	args = appendOpenCloseArgs(args, sc, posCtx)
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
	trades, err := ExecuteSpotSignal(s, result.Signal, result.Symbol, price, 0, logger)
	if err != nil {
		logger.Error("Trade execution failed: %v", err)
		return 0, ""
	}
	stampEntryATRIfOpened(s, result.Symbol, result.Indicators, trades)

	detail := ""
	if trades > 0 {
		detail = fmt.Sprintf("[%s] %s %s @ $%.2f", sc.ID, signalStr, result.Symbol, price)
	}
	return trades, detail
}

func stampEntryATRIfOpened(s *StrategyState, symbol string, indicators map[string]interface{}, trades int) {
	if trades <= 0 || s == nil {
		return
	}
	atr, ok := indicatorFloat(indicators, "atr")
	if !ok || atr <= 0 {
		return
	}
	if pos, exists := s.Positions[symbol]; exists && pos != nil && pos.EntryATR == 0 {
		pos.EntryATR = atr
	}
}

func indicatorFloat(indicators map[string]interface{}, key string) (float64, bool) {
	if indicators == nil {
		return 0, false
	}
	value, ok := indicators[key]
	if !ok {
		return 0, false
	}
	switch v := value.(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	default:
		return 0, false
	}
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

func notifyPerStrategyCircuitBreaker(sc StrategyConfig, reason string, portfolioValue float64, notifier *MultiNotifier, portfolioKillSwitchFired bool) {
	if notifier == nil || !notifier.HasBackends() || portfolioKillSwitchFired || !isFreshPerStrategyCircuitBreaker(reason) {
		return
	}
	msg := formatPerStrategyCircuitBreakerMessage(sc.ID, reason, portfolioValue)
	notifier.SendToAllChannels(msg)
	notifier.SendOwnerDM(msg)
}

func isFreshPerStrategyCircuitBreaker(reason string) bool {
	if reason == "" || reason == RiskReasonCircuitBreakerActive {
		return false
	}
	return strings.HasPrefix(reason, RiskReasonMaxDrawdownExceeded) ||
		strings.HasPrefix(reason, RiskReasonConsecutiveLosses)
}

func formatPerStrategyCircuitBreakerMessage(strategyID, reason string, portfolioValue float64) string {
	// The max-drawdown reason already embeds a portfolio=$... token, so don't
	// append a duplicate trailing value. Consecutive-losses reasons carry no
	// portfolio context, so include it there.
	if strings.Contains(reason, "portfolio=$") {
		return fmt.Sprintf("**CIRCUIT BREAKER** [%s] %s", strategyID, reason)
	}
	return fmt.Sprintf("**CIRCUIT BREAKER** [%s] %s (portfolio=$%.2f)", strategyID, reason, portfolioValue)
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

	for _, route := range notifier.tradeAlertRoutes(sc.Platform, sc.Type, isLive) {
		for _, t := range newTrades {
			var msg string
			if route.plainText {
				msg = FormatTradeDMPlain(sc, t, mode)
			} else {
				msg = FormatTradeDM(sc, t, mode)
			}
			if route.dmDest != "" {
				if err := sendTradeDestination(route.notifier, route.dmDest, msg); err != nil {
					fmt.Printf("[notify] DM trade alert failed: %v\n", err)
				}
			}
			if route.channel != "" {
				if err := route.notifier.SendMessage(route.channel, msg); err != nil {
					fmt.Printf("[notify] Channel trade alert failed: %v\n", err)
				}
			}
			if route.liveChan != "" {
				if err := route.notifier.SendMessage(route.liveChan, msg); err != nil {
					fmt.Printf("[notify] Live channel trade alert failed: %v\n", err)
				}
			}
		}
	}
}

func hyperliquidIsLive(args []string) bool {
	return isLiveArgs(args)
}

// hyperliquidSymbol extracts the coin symbol from perps strategy args (e.g. "BTC").
func hyperliquidSymbol(args []string) string {
	if len(args) >= 2 {
		return args[1]
	}
	return ""
}

// runHyperliquidCheck runs check_hyperliquid.py signal-check mode (Phase 3, no lock).
func runHyperliquidCheck(sc StrategyConfig, prices map[string]float64, posCtx PositionCtx, logger *StrategyLogger) (*HyperliquidResult, string, float64, bool) {
	args := append([]string{}, sc.Args...)
	args = appendOpenCloseArgs(args, sc, posCtx)
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
// Returns (execResult, ok); ok=false means order failed or was skipped, so
// caller must not apply state updates.
//
// posSide is the current position side captured under RLock in Phase 1
// ("long", "short", or "" for flat). We consult PerpsOrderSkipReason BEFORE
// calling the Python executor: if ExecutePerpsSignal would treat the result
// as a no-op, placing the live order would fill on-chain but never produce a
// Trade record, leaving state silently behind actual exchange holdings. See
// issue #298 — 0.716 ETH of live fills were lost this way because the
// "already long, skipping buy" branch sat AFTER RunHyperliquidExecute.
func runHyperliquidExecuteOrder(sc StrategyConfig, result *HyperliquidResult, price, cash, posQty float64, posSide string, avgCost float64, existingStopLossOID int64, notifier *MultiNotifier, logger *StrategyLogger) (*HyperliquidExecuteResult, bool) {
	if reason := PerpsOrderSkipReason(result.Signal, posSide, sc.AllowShorts); reason != "" {
		logger.Info("Skipping live order for %s: %s", result.Symbol, reason)
		return nil, false
	}
	isBuy := result.Signal == 1
	// #254/#497: perps use sizing_leverage to size notional; mirror OKX's guard so any
	// future HL spot mode can't accidentally over-size.
	sizingLeverage := EffectiveSizingLeverage(sc)
	size, ok, reason := perpsLiveOrderSize(result.Signal, price, cash, posQty, avgCost, sizingLeverage, posSide, sc.AllowShorts)
	if !ok {
		logger.Info("%s for %s", reason, result.Symbol)
		return nil, false
	}

	side := "buy"
	if !isBuy {
		side = "sell"
	}

	// Stop-loss wiring (#412):
	//   - cancel stale SL whenever a position exists with a known OID
	//     (close path must free the trigger slot; flip path too, since the
	//     new side gets a fresh SL below).
	//   - place a new SL after the open leg unless the action is a pure close
	//     (signal=-1 on a long without AllowShorts → no new position to
	//     protect). Skip for non-HL platforms or when pct<=0.
	//   - on a flip, pass prev_pos_qty so the SL is sized against the new
	//     net position (#421) — total_sz alone is closeQty+newQty.
	pureClose := result.Signal == -1 && posSide == "long" && !sc.AllowShorts
	// flipping predicate must mirror perpsLiveOrderSize exactly — both branches
	// require sc.AllowShorts. A long-only strategy that inherited a short
	// position (e.g. AllowShorts toggled true→false between restarts) would
	// otherwise see prevPosQty=posQty here while perpsLiveOrderSize sized it
	// as a fresh open without that offset, leaving net_new_sz negative and
	// the SL silently undersized (#421 review point 6).
	flipping := sc.AllowShorts && posQty > 0 && ((result.Signal == 1 && posSide == "short") || (result.Signal == -1 && posSide == "long"))
	var cancelOID int64
	if existingStopLossOID > 0 && posQty > 0 {
		cancelOID = existingStopLossOID
	}
	var slPct float64
	if !pureClose {
		// EffectiveStopLossPct self-guards on platform/type and returns the
		// explicit price %, derives it from stop_loss_margin_pct / leverage
		// (#487), or falls back to max_drawdown_pct capped at 50% (#484).
		// Validation in config.go guarantees stop_loss_pct and
		// stop_loss_margin_pct are mutually exclusive.
		slPct = EffectiveStopLossPct(sc)
	}
	var prevPosQty float64
	if flipping {
		prevPosQty = posQty
	}

	// #486: enforce margin mode + leverage on fresh opens only. HL rejects
	// updateLeverage on an open position, so flip/add legs (posQty > 0)
	// inherit whatever mode was set when the position first opened. The
	// initial open always lands here with posQty == 0 because perpsLiveOrderSize
	// would have returned ok=false otherwise on a flat→close attempt.
	marginMode := ""
	leverageForOpen := 0.0
	if posQty == 0 && sc.Platform == "hyperliquid" && sc.Type == "perps" && sc.MarginMode != "" {
		marginMode = sc.MarginMode
		leverageForOpen = EffectiveExchangeLeverage(sc)
		if leverageForOpen <= 0 {
			leverageForOpen = 1
		}
	}

	// Only log SL fields when at least one is set, to keep the common
	// no-stop-loss case quiet.
	if slPct > 0 || cancelOID > 0 || prevPosQty > 0 || marginMode != "" {
		logger.Info("Placing live %s %s size=%.6f (sl_pct=%.2f cancel_oid=%d prev_pos_qty=%.6f margin_mode=%q leverage=%g)",
			side, result.Symbol, size, slPct, cancelOID, prevPosQty, marginMode, leverageForOpen)
	} else {
		logger.Info("Placing live %s %s size=%.6f", side, result.Symbol, size)
	}

	execResult, stderr, err := RunHyperliquidExecute(sc.Script, result.Symbol, side, size, slPct, cancelOID, prevPosQty, marginMode, leverageForOpen)
	if stderr != "" {
		logger.Info("execute stderr: %s", stderr)
	}
	// On failure, the Python script may still report cancel_stop_loss_succeeded
	// — propagate execResult to the caller so the stale OID can be cleared
	// even when the open leg fails (#421). Caller treats ok=false as "do not
	// apply state mutations" but inspects execResult.CancelStopLossSucceeded
	// before discarding it.
	direction := directionOpen
	if side == "sell" {
		direction = directionClose
	}
	if err != nil {
		logger.Error("Live execute failed: %v", err)
		notifyLiveExecFailure(notifier, sc, direction, result.Symbol, err.Error())
		return execResult, false
	}
	if execResult.Error != "" {
		logger.Error("Live execute returned error: %s", execResult.Error)
		notifyLiveExecFailure(notifier, sc, direction, result.Symbol, execResult.Error)
		return execResult, false
	}
	clearLiveExecThrottle(sc, direction, result.Symbol)
	if execResult.CancelStopLossError != "" {
		logger.Warn("SL cancel failed (non-fatal): %s", execResult.CancelStopLossError)
	}
	if execResult.StopLossError != "" {
		// Surface HL open-order-cap rejection as CRITICAL — the position is
		// live without protection. HL rejects new trigger orders when the
		// account has ≥1000 open orders (scales to 5000 with volume) (#479).
		// Also route to notifier so operators see the unprotected-position
		// state in chat, not just in stderr logs (#421 review point 7,
		// mirrors the per-strategy CB notifier precedent in #415).
		if isHLOpenOrderCapRejection(execResult.StopLossError) {
			logger.Error("CRITICAL: HL open-order-cap rejected SL placement for %s — position is unprotected: %s",
				result.Symbol, execResult.StopLossError)
			if notifier != nil && notifier.HasBackends() {
				msg := fmt.Sprintf("**HL OPEN-ORDER CAP HIT** [%s] %s position is UNPROTECTED — SL placement rejected: %s",
					sc.ID, result.Symbol, execResult.StopLossError)
				notifier.SendToAllChannels(msg)
				notifier.SendOwnerDM(msg)
			}
		} else {
			logger.Warn("SL placement failed (non-fatal): %s", execResult.StopLossError)
		}
	}
	if execResult.StopLossFilledImmediately {
		logger.Warn("SL trigger filled at submit (price was already through the level) for %s — position is flat on-chain", result.Symbol)
	}
	return execResult, true
}

// isHLOpenOrderCapRejection detects HL's open-order-cap rejection strings so
// the scheduler can escalate them above WARN. HL rejects new orders (including
// trigger/reduce-only) when the account has ≥1000 open orders (scales to 5000
// with volume) (#479). Observed wordings include trigger-specific phrasings
// like "Too many open trigger orders" / "trigger order rate limit" and the
// generic "Too many open orders" form. We match any of these case-insensitively.
// Conservative rather than exhaustive — false negatives (logged as WARN) are
// acceptable; we only escalate on confirmed cap-rejection language to avoid
// CRITICAL noise on unrelated failures.
func isHLOpenOrderCapRejection(errStr string) bool {
	lower := strings.ToLower(errStr)
	hasCapVerb := strings.Contains(lower, "too many") || strings.Contains(lower, "rate limit") || strings.Contains(lower, "max") || strings.Contains(lower, "limit") || strings.Contains(lower, "exceed")
	if !hasCapVerb {
		return false
	}
	return strings.Contains(lower, "trigger order") || strings.Contains(lower, "open order") || strings.Contains(lower, "open orders")
}

// executeHyperliquidResult applies a hyperliquid result to state. Must be called under Lock.
// execResult is non-nil for successful live orders; nil for paper mode.
func executeHyperliquidResult(sc StrategyConfig, s *StrategyState, result *HyperliquidResult, execResult *HyperliquidExecuteResult, signalStr string, price float64, logger *StrategyLogger) (int, string) {
	fillPrice := price
	var fillQty float64
	if execResult != nil && execResult.Execution != nil && execResult.Execution.Fill != nil && execResult.Execution.Fill.AvgPx > 0 {
		fillPrice = execResult.Execution.Fill.AvgPx
		fillQty = execResult.Execution.Fill.TotalSz
		logger.Info("Live fill at $%.2f qty=%.6f (mid was $%.2f)", fillPrice, fillQty, price)
	}

	exchangeLeverage := EffectiveExchangeLeverage(sc)
	sizingLeverage := EffectiveSizingLeverage(sc)

	// Thread exchange metadata into ExecutePerpsSignal so each Trade is built
	// with the OID and fee before RecordTrade persists it (#289). Stamping the
	// fields onto s.TradeHistory after the fact would never reach SQLite — the
	// eager INSERT has already happened and SaveState's timestamp dedup skips
	// re-inserts for the same trade.
	var fillOID string
	var fillFee float64
	if execResult != nil && execResult.Execution != nil && execResult.Execution.Fill != nil {
		fill := execResult.Execution.Fill
		if fill.OID != 0 {
			fillOID = fmt.Sprintf("%d", fill.OID)
		}
		fillFee = fill.Fee
	}

	trades, err := ExecutePerpsSignalWithLeverage(s, result.Signal, result.Symbol, fillPrice, sizingLeverage, exchangeLeverage, fillQty, fillOID, fillFee, sc.AllowShorts, logger)
	if err != nil {
		logger.Error("Trade execution failed: %v", err)
		return 0, ""
	}
	stampEntryATRIfOpened(s, result.Symbol, result.Indicators, trades)
	if trades > 0 && fillOID != "" {
		logger.Info("Exchange order ID: %s", fillOID)
	}
	if trades > 0 {
		if pos, ok := s.Positions[result.Symbol]; ok && effectiveTrailingStopPct(sc, pos) > 0 {
			// Partial closes may reset this hint, but StopLossTriggerPx is the
			// durable ratchet. The helper never lowers a favorable trigger.
			pos.StopLossHighWaterPx = fillPrice
		}
	}

	// Stamp the SL trigger OID onto the freshly-opened Position so the next
	// signal-based close can cancel it (#412). Only the open side of a flip
	// carries a new SL — the close leg deleted its Position before the open
	// leg created the new one, so we attach to whatever Position sits at the
	// symbol now.
	if trades > 0 && execResult != nil && execResult.Execution != nil && execResult.Execution.Fill != nil {
		if slOID := execResult.Execution.Fill.StopLossOID; slOID > 0 {
			if pos, ok := s.Positions[result.Symbol]; ok {
				pos.StopLossOID = slOID
				pos.StopLossTriggerPx = execResult.Execution.Fill.StopLossTriggerPx
				logger.Info("SL trigger placed oid=%d @ $%.4f", slOID, execResult.Execution.Fill.StopLossTriggerPx)
			}
		}
	}

	// Reconcile instant-fill stop-loss: when price was already through the
	// trigger at submit, HL fills the SL immediately and the on-chain
	// position is flat. Without this branch, virtual state would carry a
	// phantom open position with StopLossOID=0 until the next reconcile
	// cycle silently delete()s it via recordClosedPosition with PnL=0,
	// losing the actual stop-loss in trade history (#421 review point 2).
	// We synthesize the close at trigger_px so virtual state matches and
	// the realized loss is booked correctly.
	if trades > 0 && execResult != nil && execResult.StopLossFilledImmediately &&
		execResult.Execution != nil && execResult.Execution.Fill != nil {
		triggerPx := execResult.Execution.Fill.StopLossTriggerPx
		if recordPerpsStopLossClose(s, result.Symbol, triggerPx, "stop_loss_immediate", logger) {
			trades++
		}
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
	return isLiveArgs(args)
}

// topstepSymbol extracts the futures symbol from strategy args (e.g. "ES").
func topstepSymbol(args []string) string {
	if len(args) >= 2 {
		return args[1]
	}
	return ""
}

// runTopStepCheck runs check_topstep.py signal-check mode (Phase 3, no lock).
func runTopStepCheck(sc StrategyConfig, prices map[string]float64, posCtx PositionCtx, logger *StrategyLogger) (*TopStepResult, string, float64, bool) {
	args := append([]string{}, sc.Args...)
	args = appendOpenCloseArgs(args, sc, posCtx)
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
//
// posSide is the current position side captured under RLock in Phase 1
// ("long", "short", or "" for flat). We consult FuturesOrderSkipReason BEFORE
// calling the Python executor: without this guard a live sell fires while
// posSide=="short" (Quantity is always positive so posQty<=0 cannot
// distinguish short from flat) but ExecuteFuturesSignal is a no-op in that
// state — producing a silent state drift identical in shape to #298/#300.
func runTopStepExecuteOrder(sc StrategyConfig, result *TopStepResult, price, cash, posQty float64, posSide string, notifier *MultiNotifier, logger *StrategyLogger) (*TopStepExecuteResult, bool) {
	if reason := FuturesOrderSkipReason(result.Signal, posSide); reason != "" {
		logger.Info("Skipping live order for %s: %s", result.Symbol, reason)
		return nil, false
	}
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
	direction := directionOpen
	if side == "sell" {
		direction = directionClose
	}
	if err != nil {
		logger.Error("Live execute failed: %v", err)
		notifyLiveExecFailure(notifier, sc, direction, result.Symbol, err.Error())
		return nil, false
	}
	if execResult.Error != "" {
		logger.Error("Live execute returned error: %s", execResult.Error)
		notifyLiveExecFailure(notifier, sc, direction, result.Symbol, execResult.Error)
		return nil, false
	}
	clearLiveExecThrottle(sc, direction, result.Symbol)
	return execResult, true
}

// executeTopStepResult applies a TopStep futures result to state. Must be called under Lock.
func executeTopStepResult(sc StrategyConfig, s *StrategyState, result *TopStepResult, execResult *TopStepExecuteResult, signalStr string, price float64, logger *StrategyLogger) (int, string) {
	fillPrice := price
	var fillContracts int
	var fillFee float64
	var fillOID string
	if execResult != nil && execResult.Execution != nil && execResult.Execution.Fill != nil && execResult.Execution.Fill.AvgPx > 0 {
		fillPrice = execResult.Execution.Fill.AvgPx
		fillContracts = execResult.Execution.Fill.TotalContracts
		fillFee = execResult.Execution.Fill.Fee
		fillOID = execResult.Execution.Fill.OID
		logger.Info("Live fill at $%.2f contracts=%d (signal was $%.2f)", fillPrice, fillContracts, price)
	}

	var feePerContract float64
	var maxContracts int
	if sc.FuturesConfig != nil {
		feePerContract = sc.FuturesConfig.FeePerContract
		maxContracts = sc.FuturesConfig.MaxContracts
	}

	trades, err := ExecuteFuturesSignalWithFillFee(s, result.Signal, result.Symbol, fillPrice, result.ContractSpec, feePerContract, maxContracts, fillContracts, fillFee, fillOID, logger)
	if err != nil {
		logger.Error("Trade execution failed: %v", err)
		return 0, ""
	}
	stampEntryATRIfOpened(s, result.Symbol, result.Indicators, trades)

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
	return isLiveArgs(args)
}

// robinhoodSymbol extracts the coin symbol from strategy args (e.g. "BTC").
func robinhoodSymbol(args []string) string {
	if len(args) >= 2 {
		return args[1]
	}
	return ""
}

// runRobinhoodCheck runs check_robinhood.py signal-check mode (Phase 3, no lock).
func runRobinhoodCheck(sc StrategyConfig, prices map[string]float64, posCtx PositionCtx, logger *StrategyLogger) (*RobinhoodResult, string, float64, bool) {
	args := append([]string{}, sc.Args...)
	args = appendOpenCloseArgs(args, sc, posCtx)
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
//
// posSide is the current position side captured under RLock in Phase 1
// ("long", "short", or "" for flat). We consult SpotOrderSkipReason BEFORE
// calling the Python executor: otherwise a no-op ExecuteSpotSignal (e.g.
// already-long with signal=1) would not record the live fill — the same bug
// class as #298. See #300.
func runRobinhoodExecuteOrder(sc StrategyConfig, result *RobinhoodResult, price, cash, posQty float64, posSide string, notifier *MultiNotifier, logger *StrategyLogger) (*RobinhoodExecuteResult, bool) {
	if reason := SpotOrderSkipReason(result.Signal, posSide); reason != "" {
		logger.Info("Skipping live order for %s: %s", result.Symbol, reason)
		return nil, false
	}
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
	direction := directionOpen
	if side == "sell" {
		direction = directionClose
	}
	if err != nil {
		logger.Error("Live execute failed: %v", err)
		notifyLiveExecFailure(notifier, sc, direction, result.Symbol, err.Error())
		return nil, false
	}
	if execResult.Error != "" {
		logger.Error("Live execute returned error: %s", execResult.Error)
		notifyLiveExecFailure(notifier, sc, direction, result.Symbol, execResult.Error)
		return nil, false
	}
	clearLiveExecThrottle(sc, direction, result.Symbol)
	return execResult, true
}

// executeRobinhoodResult applies a Robinhood result to state. Must be called under Lock.
func executeRobinhoodResult(sc StrategyConfig, s *StrategyState, result *RobinhoodResult, execResult *RobinhoodExecuteResult, signalStr string, price float64, logger *StrategyLogger) (int, string) {
	fillPrice := price
	var fillQty float64
	var fillFee float64
	var fillOID string
	if execResult != nil && execResult.Execution != nil && execResult.Execution.Fill != nil && execResult.Execution.Fill.AvgPx > 0 {
		fillPrice = execResult.Execution.Fill.AvgPx
		fillQty = execResult.Execution.Fill.Quantity
		fillFee = execResult.Execution.Fill.Fee
		fillOID = execResult.Execution.Fill.OID
		logger.Info("Live fill at $%.2f qty=%.6f (mid was $%.2f)", fillPrice, fillQty, price)
	}

	trades, err := ExecuteSpotSignalWithFillFee(s, result.Signal, result.Symbol, fillPrice, fillQty, fillFee, fillOID, logger)
	if err != nil {
		logger.Error("Trade execution failed: %v", err)
		return 0, ""
	}
	stampEntryATRIfOpened(s, result.Symbol, result.Indicators, trades)

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
	return isLiveArgs(args)
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
func runOKXCheck(sc StrategyConfig, prices map[string]float64, posCtx PositionCtx, logger *StrategyLogger) (*OKXResult, string, float64, bool) {
	args := append([]string{}, sc.Args...)
	args = appendOpenCloseArgs(args, sc, posCtx)
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
//
// posSide is the current position side captured under RLock in Phase 1
// ("long", "short", or "" for flat). We consult Perps/SpotOrderSkipReason
// BEFORE calling the Python executor — OKX covers both spot and perps, and
// each has its own side-based no-op branches in ExecuteSpotSignal /
// ExecutePerpsSignal that must be mirrored to avoid the #298 bug class
// (live fill placed but no Trade recorded because the in-memory execution
// returned 0). See #300.
func runOKXExecuteOrder(sc StrategyConfig, result *OKXResult, price, cash, posQty float64, posSide string, avgCost float64, notifier *MultiNotifier, logger *StrategyLogger) (*OKXExecuteResult, bool) {
	var skip string
	if sc.Type == "perps" {
		skip = PerpsOrderSkipReason(result.Signal, posSide, sc.AllowShorts)
	} else {
		skip = SpotOrderSkipReason(result.Signal, posSide)
	}
	if skip != "" {
		logger.Info("Skipping live order for %s: %s", result.Symbol, skip)
		return nil, false
	}
	isBuy := result.Signal == 1
	// #254/#497: perps use sizing_leverage to size notional; EffectiveSizingLeverage
	// returns 1 for spot, so no perps gate needed here.
	sizingLeverage := EffectiveSizingLeverage(sc)
	var size float64
	if sc.Type == "perps" {
		var ok bool
		var reason string
		size, ok, reason = perpsLiveOrderSize(result.Signal, price, cash, posQty, avgCost, sizingLeverage, posSide, sc.AllowShorts)
		if !ok {
			logger.Info("%s for %s", reason, result.Symbol)
			return nil, false
		}
	} else {
		// Spot sizing: buy opens from cash, sell closes posQty. AllowShorts
		// does not apply to spot — SpotOrderSkipReason already blocked any
		// signal=-1 without a long above.
		if isBuy {
			budget := cash * sizingLeverage * 0.95
			if budget < 1 || price <= 0 {
				logger.Info("Insufficient cash ($%.2f) for live buy %s", cash, result.Symbol)
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
	direction := directionOpen
	if side == "sell" {
		direction = directionClose
	}
	if err != nil {
		logger.Error("Live execute failed: %v", err)
		notifyLiveExecFailure(notifier, sc, direction, result.Symbol, err.Error())
		return nil, false
	}
	if execResult.Error != "" {
		logger.Error("Live execute returned error: %s", execResult.Error)
		notifyLiveExecFailure(notifier, sc, direction, result.Symbol, execResult.Error)
		return nil, false
	}
	clearLiveExecThrottle(sc, direction, result.Symbol)
	return execResult, true
}

// executeOKXResult applies an OKX result to state. Must be called under Lock.
func executeOKXResult(sc StrategyConfig, s *StrategyState, result *OKXResult, execResult *OKXExecuteResult, signalStr string, price float64, logger *StrategyLogger) (int, string) {
	fillPrice := price
	var fillQty float64
	var fillFee float64
	var fillOID string
	if execResult != nil && execResult.Execution != nil && execResult.Execution.Fill != nil && execResult.Execution.Fill.AvgPx > 0 {
		fillPrice = execResult.Execution.Fill.AvgPx
		fillQty = execResult.Execution.Fill.TotalSz
		fillFee = execResult.Execution.Fill.Fee
		fillOID = execResult.Execution.Fill.OID
		logger.Info("Live fill at $%.2f qty=%.6f (mid was $%.2f)", fillPrice, fillQty, price)
	}

	// Thread fillOID/fillFee into the signal handlers so each Trade is built
	// with the OID and fee before RecordTrade persists it (#456). Stamping the
	// fields onto s.TradeHistory after the fact would never reach SQLite — the
	// eager INSERT has already happened and SaveState's timestamp dedup skips
	// re-inserts for the same trade.
	var trades int
	var err error
	if sc.Type == "perps" {
		trades, err = ExecutePerpsSignalWithLeverage(s, result.Signal, result.Symbol, fillPrice, EffectiveSizingLeverage(sc), EffectiveExchangeLeverage(sc), fillQty, fillOID, fillFee, sc.AllowShorts, logger)
	} else {
		trades, err = ExecuteSpotSignalWithFillFee(s, result.Signal, result.Symbol, fillPrice, fillQty, fillFee, fillOID, logger)
	}
	if err != nil {
		logger.Error("Trade execution failed: %v", err)
		return 0, ""
	}
	stampEntryATRIfOpened(s, result.Symbol, result.Indicators, trades)

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

// findLeaderboardSummariesByChannel returns every LeaderboardSummaryConfig
// whose Channel matches channelID, preserving config order. A single channel
// may have multiple entries (e.g. one unfiltered + one ticker-scoped); all are
// returned so -summary posts what the operator configured. (#308, review item
// 3 on #309)
func findLeaderboardSummariesByChannel(cfg *Config, channelID string) []LeaderboardSummaryConfig {
	var out []LeaderboardSummaryConfig
	for _, lc := range cfg.LeaderboardSummaries {
		if lc.Channel == channelID {
			out = append(out, lc)
		}
	}
	return out
}

// augmentMarksBestEffort fills prices with HL perps, OKX perps, and futures
// marks for every position referenced by cfg.Strategies. Failures log [WARN]
// to stderr; missing marks fall back to entry cost via PortfolioValue.
// Shared by the one-shot channel-summary path and the configurable
// leaderboard-summary path. (#308)
func augmentMarksBestEffort(cfg *Config, prices map[string]float64) {
	hlPerpsCoins, okxPerpsCoins := collectPerpsMarkSymbols(cfg.Strategies)
	futuresSymbols := collectFuturesMarkSymbols(cfg.Strategies)

	if len(hlPerpsCoins) > 0 {
		if marks, err := fetchHyperliquidMids(hlPerpsCoins); err != nil {
			fmt.Fprintf(os.Stderr, "[WARN] HL perps marks fetch failed for %v: %v — summary will use entry cost\n", hlPerpsCoins, err)
		} else {
			mergePerpsMarks(prices, marks)
		}
	}
	if len(okxPerpsCoins) > 0 {
		if marks, err := fetchOKXPerpsMids(okxPerpsCoins); err != nil {
			fmt.Fprintf(os.Stderr, "[WARN] OKX perps marks fetch failed for %v: %v — summary will use entry cost\n", okxPerpsCoins, err)
		} else {
			mergePerpsMarks(prices, marks)
		}
	}
	if len(futuresSymbols) > 0 {
		if marks, mode, err := FetchFuturesMarks(futuresSymbols); err != nil {
			fmt.Fprintf(os.Stderr, "[WARN] Futures marks fetch failed for %v: %v — summary will use entry cost\n", futuresSymbols, err)
		} else {
			if mode == FuturesMarkModePaperFallback {
				fmt.Fprintf(os.Stderr, "[WARN] fetch_futures_marks: live mode init failed, degraded to paper (yfinance) — check TopStepX creds and network\n")
			}
			mergeFuturesMarks(prices, marks)
		}
	}
}

// fetchPricesForSummary fetches spot + best-effort perps/futures marks needed
// to revalue positions for the leaderboard summary. Failures are logged but
// non-fatal — positions fall back to entry cost. (#308)
func fetchPricesForSummary(cfg *Config) map[string]float64 {
	prices := make(map[string]float64)
	symbols := collectPriceSymbols(cfg.Strategies)

	if len(symbols) > 0 {
		if p, err := FetchPrices(symbols); err == nil {
			for sym, price := range p {
				if price > 0 {
					prices[sym] = price
				}
			}
		} else {
			fmt.Fprintf(os.Stderr, "[WARN] Price fetch failed: %v — summary will use entry cost\n", err)
		}
	}
	augmentMarksBestEffort(cfg, prices)
	return prices
}

// runLeaderboardSummariesAndExit posts every matching LeaderboardSummaryConfig
// and exits. Prices are fetched once and shared across all entries. Each
// empty-result entry is reported to stderr but does not abort siblings; exits
// 1 only if every entry produced no message. (#308, review item 3 on #309)
func runLeaderboardSummariesAndExit(lcs []LeaderboardSummaryConfig, cfg *Config, state *AppState, sdb *StateDB, notifier *MultiNotifier) {
	prices := fetchPricesForSummary(cfg)
	sharpeByStrategy := ComputeSharpeByStrategy(LoadClosedPositionsByStrategy(sdb, cfg), cfg, state)
	posted := 0
	for _, lc := range lcs {
		msg := BuildLeaderboardSummary(lc, cfg, state, prices, sharpeByStrategy)
		if msg == "" {
			fmt.Fprintf(os.Stderr, "No strategies match leaderboard summary platform=%s ticker=%s\n", lc.Platform, lc.Ticker)
			continue
		}
		if err := notifier.SendMessage(lc.Channel, msg); err != nil {
			fmt.Fprintf(os.Stderr, "[WARN] Send to channel %s failed: %v\n", lc.Channel, err)
		}
		fmt.Println(msg)
		fmt.Printf("-summary=%s: posted leaderboard summary (platform=%s, ticker=%s)\n", lc.Channel, lc.Platform, lc.Ticker)
		posted++
	}
	if posted == 0 {
		os.Exit(1)
	}
	fmt.Printf("-summary=%s: posted %d leaderboard summaries, exiting.\n", lcs[0].Channel, posted)
	os.Exit(0)
}

func cloneTimeMap(in map[string]time.Time) map[string]time.Time {
	out := make(map[string]time.Time, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// pendingLeaderboardSummary carries a computed summary from under-lock
// computation to post-unlock I/O. (#308)
type pendingLeaderboardSummary struct {
	channel string
	msg     string
	key     string
	topN    int
}

// collectDueLeaderboardSummaries builds summaries for LeaderboardSummaries
// entries whose Frequency has elapsed. Marks state.LastLeaderboardSummaries
// optimistically so duplicate posts are avoided if the caller's Discord send
// fails; same semantics as the previous in-lock implementation. Caller must
// hold the write lock on state. (#308)
func collectDueLeaderboardSummaries(cfg *Config, state *AppState, prices map[string]float64, sharpeByStrategy map[string]float64) []pendingLeaderboardSummary {
	if len(cfg.LeaderboardSummaries) == 0 {
		return nil
	}
	if state.LastLeaderboardSummaries == nil {
		state.LastLeaderboardSummaries = make(map[string]time.Time)
	}
	now := time.Now().UTC()
	var pending []pendingLeaderboardSummary
	for _, lc := range cfg.LeaderboardSummaries {
		freq := lc.ParsedFrequency()
		if freq <= 0 {
			continue
		}
		key := lc.Key()
		last := state.LastLeaderboardSummaries[key]
		if !last.IsZero() && now.Sub(last) < freq {
			continue
		}
		msg := BuildLeaderboardSummary(lc, cfg, state, prices, sharpeByStrategy)
		if msg == "" {
			continue
		}
		state.LastLeaderboardSummaries[key] = now
		pending = append(pending, pendingLeaderboardSummary{
			channel: lc.Channel,
			msg:     msg,
			key:     key,
			topN:    lc.TopN,
		})
	}
	return pending
}

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

	// Start HTTP status server
	server := NewStatusServer(state, &mu, cfg.StatusToken, cfg.Strategies, stateDB)
	server.Start(8099)

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
		runSummaryAndExit(*summary, cfg, state, notifier)
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
		if err := PostLeaderboard(cfg, state, prices, notifier); err != nil {
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
		checkForUpdates(cfg, notifier, &lastNotifiedHash, &mu, state, stateDB)
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

			mu.RLock()
			totalPV, usedPVFallback := computeTotalPortfolioValue(cfg.Strategies, state, prices, walletBalances, sharedWallets)
			totalNotional := PortfolioNotional(state.Strategies, prices)
			// #296: aggregate perps margin drawdown inputs alongside the
			// equity total so the portfolio kill switch can fire on a
			// leveraged margin blow-up that would otherwise hide inside
			// equity-only drawdown for all-perps accounts.
			perpsLoss, perpsMargin := AggregatePerpsMarginInputs(state.Strategies, prices)
			mu.RUnlock()

			mu.Lock()
			// #243: Freeze peak during fallback cycles so a transient HL API
			// blip cannot ratchet the high-water mark (peak is sticky, so a
			// false peak would persist and could later trip a false drawdown).
			// CheckPortfolioRisk auto-ratchets PeakValue when totalValue > peak;
			// we snapshot before the call and restore if we're on a fallback
			// cycle. Drawdown detection still runs against the frozen peak.
			origPeak := state.PortfolioRisk.PeakValue
			portfolioAllowed, nb, portfolioWarning, portfolioReason := CheckPortfolioRisk(&state.PortfolioRisk, cfg.PortfolioRisk, totalPV, totalNotional, perpsLoss, perpsMargin)
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
				// Portfolio kill owns all platform closes — drop per-strategy pending.
				for _, ss := range state.Strategies {
					if ss != nil {
						ss.RiskState.clearPendingCircuitClose(PlatformPendingCloseHyperliquid)
						ss.RiskState.clearPendingCircuitClose(PlatformPendingCloseOKX)
						ss.RiskState.clearPendingCircuitClose(PlatformPendingCloseRobinhood)
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
			if killSwitchFired {
				inputs := KillSwitchCloseInputs{
					HLAddr:          hlAddr,
					HLStateFetched:  hlStateFetched,
					HLPositions:     hlPositions,
					HLLiveAll:       hlLiveAll,
					HLCloser:        defaultHyperliquidLiveCloser,
					HLFetcher:       defaultHLStateFetcher,
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

			if killSwitchFired && plan.OnChainConfirmedFlat {
				mu.Lock()
				for _, sc := range cfg.Strategies {
					if s, ok := state.Strategies[sc.ID]; ok {
						forceCloseAllPositions(s, prices, nil)
						// Pending HL circuit close was already cleared above
						// when portfolio kill fired (line ~611); nothing to do
						// here. The per-strategy pending field is owned by the
						// portfolio kill path once it takes over flattening.
					}
				}
				mu.Unlock()
			}

			if killSwitchFired && notifier.HasBackends() && plan.DiscordMessage != "" {
				notifier.SendToAllChannels(plan.DiscordMessage)
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
				addKillSwitchEvent(&state.PortfolioRisk, "warning", source, warnDD, totalPV, state.PortfolioRisk.PeakValue, portfolioReason)
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

					// Phase 1: RLock — read inputs needed for subprocess
					mu.RLock()
					pv := PortfolioValue(stratState, prices)
					var posJSON string
					if sc.Type == "options" {
						posJSON = EncodeAllPositionsJSON(stratState.OptionPositions, stratState.Positions)
					}
					var hlCash float64
					var hlPosQty float64
					var hlPosSide string
					var hlAvgCost float64
					if sc.Type == "perps" && hyperliquidIsLive(sc.Args) {
						hlCash = stratState.Cash
						if sym := hyperliquidSymbol(sc.Args); sym != "" {
							if pos, ok := stratState.Positions[sym]; ok {
								hlPosQty = pos.Quantity
								hlPosSide = pos.Side
								hlAvgCost = pos.AvgCost
							}
						}
					}
					var okxCash float64
					var okxPosQty float64
					var okxPosSide string
					var okxAvgCost float64
					if sc.Platform == "okx" && okxIsLive(sc.Args) {
						okxCash = stratState.Cash
						if sym := okxSymbol(sc.Args); sym != "" {
							if pos, ok := stratState.Positions[sym]; ok {
								okxPosQty = pos.Quantity
								okxPosSide = pos.Side
								okxAvgCost = pos.AvgCost
							}
						}
					}
					var rhCash float64
					var rhPosQty float64
					var rhPosSide string
					if sc.Platform == "robinhood" && robinhoodIsLive(sc.Args) {
						rhCash = stratState.Cash
						if sym := robinhoodSymbol(sc.Args); sym != "" {
							if pos, ok := stratState.Positions[sym]; ok {
								rhPosQty = pos.Quantity
								rhPosSide = pos.Side
							}
						}
					}
					var tsCash float64
					var tsContracts float64
					var tsPosSide string
					if sc.Type == "futures" && topstepIsLive(sc.Args) {
						tsCash = stratState.Cash
						if sym := topstepSymbol(sc.Args); sym != "" {
							if pos, ok := stratState.Positions[sym]; ok {
								tsContracts = pos.Quantity
								tsPosSide = pos.Side
							}
						}
					}
					mu.RUnlock()

					// Phase 2: Lock — CheckRisk (fast, no I/O)
					var riskAssist *PlatformRiskAssist
					if (hlStateFetched && len(hlLiveAll) > 0) || (okxStateFetched && len(okxLivePerps) > 0) {
						riskAssist = &PlatformRiskAssist{}
						if hlStateFetched && len(hlLiveAll) > 0 {
							riskAssist.HLPositions = hlPositions
							riskAssist.HLLiveAll = hlLiveAll
						}
						if okxStateFetched && len(okxLivePerps) > 0 {
							riskAssist.OKXPositions = okxPositions
							riskAssist.OKXLiveAll = okxLivePerps
						}
					}
					mu.Lock()
					allowed, reason := CheckRisk(&sc, stratState, pv, prices, logger, riskAssist)
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
									if er, ok2 := runOKXExecuteOrder(sc, result, price, okxCash, okxPosQty, okxPosSide, okxAvgCost, logger); ok2 {
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
									if er, ok2 := runRobinhoodExecuteOrder(sc, result, price, rhCash, rhPosQty, rhPosSide, logger); ok2 {
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
									if er, ok2 := runOKXExecuteOrder(sc, result, price, okxCash, okxPosQty, okxPosSide, okxAvgCost, logger); ok2 {
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
								if er, ok2 := runHyperliquidExecuteOrder(sc, result, price, hlCash, hlPosQty, hlPosSide, hlAvgCost, logger); ok2 {
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
								if er, ok2 := runTopStepExecuteOrder(sc, result, price, tsCash, tsContracts, tsPosSide, logger); ok2 {
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
					msgs := FormatCategorySummary(cycle, elapsed, len(dueStrategies), chTrades, chValue, prices, chDetails, chStrats, state, chKey, "", cfg.IntervalSeconds)
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
						msgs := FormatCategorySummary(cycle, elapsed, len(dueStrategies), assetTrades, assetValue, prices, assetDetails, assetStrats, state, chKey, asset, cfg.IntervalSeconds)
						for _, msg := range msgs {
							notifier.SendToChannel(chKey, chKey, msg)
						}
					}
				}
			}
			mu.RUnlock()
		}

		// Save state after each cycle
		mu.Lock()
		state.LastCycle = time.Now().UTC()

		// Periodic configurable leaderboard summaries (#308). Compute + update
		// state.LastLeaderboardSummaries under Lock; post outside so Discord
		// HTTPS latency can't stall the scheduler cycle.
		var duePending []pendingLeaderboardSummary
		if notifier.HasBackends() {
			duePending = collectDueLeaderboardSummaries(cfg, state, prices)
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
				mu.RLock()
				lbMessages := BuildLeaderboardMessages(cfg, state, prices)
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

		// Periodic update check (heartbeat: every cycle; daily: once per day).
		if cfg.AutoUpdate == "heartbeat" {
			checkForUpdates(cfg, notifier, &lastNotifiedHash, &mu, state, stateDB)
		} else if cfg.AutoUpdate == "daily" && cycle%dailyCycles == 0 {
			checkForUpdates(cfg, notifier, &lastNotifiedHash, &mu, state, stateDB)
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
//
// Lookup order (#308):
//  1. If channelKey matches a cfg.LeaderboardSummaries[].Channel, build and
//     post that configured leaderboard (platform + optional ticker + topN).
//  2. Otherwise fall back to the legacy asset-grouped category summary, which
//     requires strategies whose notifier-resolved channel key equals channelKey.
//
// It fetches current prices, formats the summary, posts to all notification
// backends, and exits immediately.
func runSummaryAndExit(channelKey string, cfg *Config, state *AppState, notifier *MultiNotifier) {
	if !notifier.HasBackends() {
		fmt.Fprintf(os.Stderr, "No notification backends configured\n")
		os.Exit(1)
	}

	// #308: Manual trigger for configured leaderboard summaries.
	if lcs := findLeaderboardSummariesByChannel(cfg, channelKey); len(lcs) > 0 {
		runLeaderboardSummariesAndExit(lcs, cfg, state, notifier)
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
	assetGroups, assetKeys := groupByAsset(chStrats)
	if len(assetKeys) <= 1 {
		msgs := FormatCategorySummary(state.CycleCount, 0, 0, 0, chValue, prices, nil, chStrats, state, channelKey, "", cfg.IntervalSeconds)
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
			msgs := FormatCategorySummary(state.CycleCount, 0, 0, 0, assetValue, prices, nil, assetStrats, state, channelKey, asset, cfg.IntervalSeconds)
			for _, msg := range msgs {
				notifier.SendToChannel(channelKey, channelKey, msg)
				fmt.Println(msg)
			}
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
	trades, err := ExecuteSpotSignal(s, result.Signal, result.Symbol, price, 0, logger)
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
		dmKey := sc.Platform
		if !isLive {
			dmKey = sc.Platform + "-paper"
		}
		dmDest := ""
		if b.dmChannels != nil {
			dmDest = b.dmChannels[dmKey]
		}

		// Channel routing: presence of a channel ID means enabled, absence means disabled.
		// Paper trades use "<platform>-paper" key with fallback to base platform key.
		// Live trades use the base platform key.
		ch := resolveTradeChannel(b.channels, sc.Platform, sc.Type, isLive)

		// Also post live trades to a dedicated "<platform>-live" channel if configured.
		var liveCh string
		if isLive {
			liveCh = resolveChannel(b.channels, sc.Platform+"-live", "")
			if liveCh == ch {
				liveCh = "" // avoid double-posting to the same channel
			}
		}

		if dmDest == "" && ch == "" && liveCh == "" {
			continue
		}

		for _, t := range newTrades {
			var msg string
			if b.plainText {
				msg = FormatTradeDMPlain(sc, t, mode)
			} else {
				msg = FormatTradeDM(sc, t, mode)
			}
			if dmDest != "" {
				if err := sendTradeDestination(b.notifier, dmDest, msg); err != nil {
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
func runHyperliquidExecuteOrder(sc StrategyConfig, result *HyperliquidResult, price, cash, posQty float64, posSide string, avgCost float64, logger *StrategyLogger) (*HyperliquidExecuteResult, bool) {
	if reason := PerpsOrderSkipReason(result.Signal, posSide, sc.AllowShorts); reason != "" {
		logger.Info("Skipping live order for %s: %s", result.Symbol, reason)
		return nil, false
	}
	isBuy := result.Signal == 1
	// #254: perps use leverage to size notional; mirror OKX's guard so any
	// future HL spot mode can't accidentally over-size.
	sizingLeverage := 1.0
	if sc.Type == "perps" && sc.Leverage > 0 {
		sizingLeverage = sc.Leverage
	}
	size, ok, reason := perpsLiveOrderSize(result.Signal, price, cash, posQty, avgCost, sizingLeverage, posSide, sc.AllowShorts)
	if !ok {
		logger.Info("%s for %s", reason, result.Symbol)
		return nil, false
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
	var fillQty float64
	if execResult != nil && execResult.Execution != nil && execResult.Execution.Fill != nil && execResult.Execution.Fill.AvgPx > 0 {
		fillPrice = execResult.Execution.Fill.AvgPx
		fillQty = execResult.Execution.Fill.TotalSz
		logger.Info("Live fill at $%.2f qty=%.6f (mid was $%.2f)", fillPrice, fillQty, price)
	}

	leverage := sc.Leverage
	if leverage <= 0 {
		leverage = 1
	}

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

	trades, err := ExecutePerpsSignal(s, result.Signal, result.Symbol, fillPrice, leverage, fillQty, fillOID, fillFee, sc.AllowShorts, logger)
	if err != nil {
		logger.Error("Trade execution failed: %v", err)
		return 0, ""
	}
	if trades > 0 && fillOID != "" {
		logger.Info("Exchange order ID: %s", fillOID)
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
//
// posSide is the current position side captured under RLock in Phase 1
// ("long", "short", or "" for flat). We consult FuturesOrderSkipReason BEFORE
// calling the Python executor: without this guard a live sell fires while
// posSide=="short" (Quantity is always positive so posQty<=0 cannot
// distinguish short from flat) but ExecuteFuturesSignal is a no-op in that
// state — producing a silent state drift identical in shape to #298/#300.
func runTopStepExecuteOrder(sc StrategyConfig, result *TopStepResult, price, cash, posQty float64, posSide string, logger *StrategyLogger) (*TopStepExecuteResult, bool) {
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
	var fillContracts int
	if execResult != nil && execResult.Execution != nil && execResult.Execution.Fill != nil && execResult.Execution.Fill.AvgPx > 0 {
		fillPrice = execResult.Execution.Fill.AvgPx
		fillContracts = execResult.Execution.Fill.TotalContracts
		logger.Info("Live fill at $%.2f contracts=%d (signal was $%.2f)", fillPrice, fillContracts, price)
	}

	var feePerContract float64
	var maxContracts int
	if sc.FuturesConfig != nil {
		feePerContract = sc.FuturesConfig.FeePerContract
		maxContracts = sc.FuturesConfig.MaxContracts
	}

	trades, err := ExecuteFuturesSignal(s, result.Signal, result.Symbol, fillPrice, result.ContractSpec, feePerContract, maxContracts, fillContracts, logger)
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
//
// posSide is the current position side captured under RLock in Phase 1
// ("long", "short", or "" for flat). We consult SpotOrderSkipReason BEFORE
// calling the Python executor: otherwise a no-op ExecuteSpotSignal (e.g.
// already-long with signal=1) would not record the live fill — the same bug
// class as #298. See #300.
func runRobinhoodExecuteOrder(sc StrategyConfig, result *RobinhoodResult, price, cash, posQty float64, posSide string, logger *StrategyLogger) (*RobinhoodExecuteResult, bool) {
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
	var fillQty float64
	if execResult != nil && execResult.Execution != nil && execResult.Execution.Fill != nil && execResult.Execution.Fill.AvgPx > 0 {
		fillPrice = execResult.Execution.Fill.AvgPx
		fillQty = execResult.Execution.Fill.Quantity
		logger.Info("Live fill at $%.2f qty=%.6f (mid was $%.2f)", fillPrice, fillQty, price)
	}

	trades, err := ExecuteSpotSignal(s, result.Signal, result.Symbol, fillPrice, fillQty, logger)
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
//
// posSide is the current position side captured under RLock in Phase 1
// ("long", "short", or "" for flat). We consult Perps/SpotOrderSkipReason
// BEFORE calling the Python executor — OKX covers both spot and perps, and
// each has its own side-based no-op branches in ExecuteSpotSignal /
// ExecutePerpsSignal that must be mirrored to avoid the #298 bug class
// (live fill placed but no Trade recorded because the in-memory execution
// returned 0). See #300.
func runOKXExecuteOrder(sc StrategyConfig, result *OKXResult, price, cash, posQty float64, posSide string, avgCost float64, logger *StrategyLogger) (*OKXExecuteResult, bool) {
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
	// #254: perps use leverage to size notional; spot is unaffected.
	sizingLeverage := 1.0
	if sc.Type == "perps" && sc.Leverage > 0 {
		sizingLeverage = sc.Leverage
	}
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
	var fillQty float64
	if execResult != nil && execResult.Execution != nil && execResult.Execution.Fill != nil && execResult.Execution.Fill.AvgPx > 0 {
		fillPrice = execResult.Execution.Fill.AvgPx
		fillQty = execResult.Execution.Fill.TotalSz
		logger.Info("Live fill at $%.2f qty=%.6f (mid was $%.2f)", fillPrice, fillQty, price)
	}

	var trades int
	var err error
	if sc.Type == "perps" {
		leverage := sc.Leverage
		if leverage <= 0 {
			leverage = 1
		}
		// OKXFill does not carry OID/fee today; pass empties and let SaveState
		// backfill from any future adapter extension via the usual path.
		trades, err = ExecutePerpsSignal(s, result.Signal, result.Symbol, fillPrice, leverage, fillQty, "", 0, sc.AllowShorts, logger)
	} else {
		trades, err = ExecuteSpotSignal(s, result.Signal, result.Symbol, fillPrice, fillQty, logger)
	}
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
func runLeaderboardSummariesAndExit(lcs []LeaderboardSummaryConfig, cfg *Config, state *AppState, notifier *MultiNotifier) {
	prices := fetchPricesForSummary(cfg)
	posted := 0
	for _, lc := range lcs {
		msg := BuildLeaderboardSummary(lc, cfg, state, prices)
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
func collectDueLeaderboardSummaries(cfg *Config, state *AppState, prices map[string]float64) []pendingLeaderboardSummary {
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
		msg := BuildLeaderboardSummary(lc, cfg, state, prices)
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

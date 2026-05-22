Total output lines: 3613

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

// knownSubcommands lists the leading-position subcommand tokens dispatched in
// main() before flag.Parse(). Kept in sync with that switch so
// validateDaemonInvocation can detect a misplaced one and produce a useful
// error instead of silently starting the daemon (#700).
var knownSubcommands = []string{
	"init",
	"export",
	"manual-open",
	"manual-close",
	"backfill",
	"probe",
	"inspect",
	"version",
}

// validateDaemonInvocation rejects stray positional args that survived
// flag.Parse() on the daemon path. The most common cause is a misplaced
// subcommand, e.g. `./go-trader -config foo manual-open ...`, where
// `-config foo` is consumed as a global flag and the remaining args are
// silently dropped — without this guard the daemon would boot a second
// scheduler instead of running the requested subcommand (#700).
func validateDaemonInvocation(extra []string) error {
	if len(extra) == 0 {
		return nil
	}
	for _, a := range extra {
		for _, sub := range knownSubcommands {
			if a == sub {
				return fmt.Errorf("subcommand %q must appear before any global flags (got: %v)", sub, extra)
			}
		}
	}
	return fmt.Errorf("unexpected positional arguments after flags: %v", extra)
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "init":
			os.Exit(runInit(os.Args[2:]))
		case "export":
			os.Exit(runExport(os.Args[2:]))
		case "manual-open":
			os.Exit(runManualOpen(os.Args[2:]))
		case "manual-close":
			os.Exit(runManualClose(os.Args[2:]))
		case "backfill":
			os.Exit(runBackfill(os.Args[2:]))
		case "probe":
			os.Exit(runProbe(os.Args[2:]))
		case "inspect":
			os.Exit(runInspect(os.Args[2:]))
		case "version", "--version", "-version":
			fmt.Println(Version)
			os.Exit(0)
		}
	}

	configPath := flag.String("config", "scheduler/config.json", "Path to config file")
	once := flag.Bool("once", false, "Run one cycle and exit")
	summary := flag.String("summary", "", "Post snapshot summary for the specified channel (e.g., hyperliquid, spot, options) and exit")
	leaderboard := flag.Bool("leaderboard", false, "Post pre-computed daily leaderboard and exit")
	statusPortFlag := flag.Int("status-port", 0, fmt.Sprintf("HTTP status server port (overrides config, default: %d)", DefaultStatusPort))
	flag.Parse()

	if err := validateDaemonInvocation(flag.Args()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	// Load config
	cfg, err := LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Loaded config: %d strategies, interval=%ds\n", len(cfg.Strategies), cfg.IntervalSeconds)

	// #704: emit a one-line resolved summary per strategy so operators can
	// audit close/SL/TP wiring without grepping the JSON. Best-effort — a
	// failed re-read just means we can't mark explicit-vs-default but the
	// summary still shows the resolved source.
	explicitKeys, _ := loadStrategyExplicitKeys(*configPath)
	for _, sc := range cfg.Strategies {
		fmt.Println(formatStrategySummaryLine(sc, explicitKeys[sc.ID]))
	}

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
	pruned := false
	for id := range state.Strategies {
		if !configIDs[id] {
			delete(state.Strategies, id)
			fmt.Printf("  Pruned stale strategy: %s\n", id)
			pruned = true
		}
	}

	// #650: After prune, rebaseline portfolio peak from surviving strategies'
	// per-strategy peaks. Without this, the stale portfolio peak (sized for the
	// pre-prune strategy set) can immediately latch the kill switch on the
	// first risk-check cycle once current value drops to the surviving subset.
	if pruned && state.PortfolioRisk.PeakValue > 0 {
		oldPeak := state.PortfolioRisk.PeakValue
		newPeak := rebaselinePortfolioPeakAfterPrune(state, cfg)
		if newPeak != oldPeak {
			state.PortfolioRisk.PeakValue = newPeak
			fmt.Printf("  Portfolio peak rebaselined after prune: $%.0f -> $%.0f\n", oldPeak, newPeak)
		}
	}

	// #336/#656: Detect perps positions whose side conflicts with the
	// configured direction (e.g. a short under direction="long", or a long
	// under direction="short"). The executor can't reconcile this on its
	// own — a fresh-open signal against the conflicting position nets on
	// the exchange but flips virtually. Collect here, forward to owner DM
	// once the notifier is wired below.
	directionConfigWarnings := ValidatePerpsDirectionConfig(state, cfg)

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

	// Graceful shutdown — two-phase drain (see scheduler/shutdown.go).
	//
	// Phase 1 (signal goroutine below): set draining flag, cancel
	// shutdownReadOnlyCtx, close stopCh. Do not save state, take locks, or
	// call I/O — a panic in this goroutine would leave the daemon wedged.
	//
	// Phase 2 + 3 run on the main goroutine: the trading loop returns when
	// it sees stopCh closed, then the deferred shutdown sequence further
	// down (registered AFTER buildNotifierFromConfig so it runs BEFORE
	// cleanupNotifier in LIFO order) waits for in-flight side-effecting
	// subprocesses and persists state before the notifier flushes.
	initShutdownContexts()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	stopCh := make(chan struct{})
	go func() {
		sig := <-sigCh
		fmt.Printf("\nReceived %s, draining...\n", sig)
		beginDrain()
		close(stopCh)
	}()

	// Initialize notification backends (Discord and/or Telegram).
	notifier, cleanupNotifier := buildNotifierFromConfig(cfg)
	defer cleanupNotifier()
	fmt.Printf("Notification backends: %d active\n", notifier.BackendCount())

	// Phase 2 + 3 of graceful shutdown — registered AFTER cleanupNotifier so
	// LIFO ordering puts this defer BEFORE notifier flush. Sequence on
	// natural return from the trading loop:
	//
	//   1. runDrain() — wait up to shutdownDrainCap for in-flight
	//      side-effecting subprocesses (--execute, close_*.py,
	//      sync-protection); cap fires → SIGKILL backstop.
	//   2. SaveStateWithDB — final state persist.
	//   3. cleanupNotifier (registered above, runs after this defer
	//      returns) — Discord/Telegram HTTP flush so any "[shutdown]" DM
	//      lands. stateDB.Close (line 63) and logMgr.Close (line 185) run
	//      after.
	//
	// os.Exit code paths (--once with exit, --summary, --leaderboard, probe
	// failure) bypass this defer; those paths are short-lived and don't
	// leave side-effecting subprocesses in flight.
	defer func() {
		runDrain()
		mu.Lock()
		if err := SaveStateWithDB(state, cfg, stateDB); err != nil {
			fmt.Fprintf(os.Stderr, "[shutdown] Failed to save state: %v\n", err)
		} else {
			fmt.Println("[shutdown] State saved.")
		}
		mu.Unlock()
		fmt.Println("[shutdown] Complete.")
	}()

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

	// #336/#656: Forward startup direction-vs-position warnings to the owner
	// so the desync is surfaced even when the operator isn't tailing stderr.
	if len(directionConfigWarnings) > 0 && notifier.HasOwner() {
		for _, msg := range directionConfigWarnings {
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
		lifetimeStats := loadLifetimeStatsBestEffort(stateDB, "[leaderboard]")
		if err := PostLeaderboard(cfg, state, prices, sharpeByStrategy, lifetimeStats, notifier); err != nil {
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

	// #645: Verify each configured check script accepts the binary's argv
	// shape before entering the trading loop. An asymmetric deploy (binary
	// from one commit, Python scripts from an older one) caused 18h of
	// silent argparse crashes after #642 — fail fast instead so the
	// operator is paged immediately and systemd's restart loop makes the
	// breakage visible in `systemctl status`.
	if err := probeCheckScripts(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "[startup] check-script compatibility probe failed: %v\n", err)
		if notifier != nil && notifier.HasOwner() {
			notifier.SendOwnerDM(fmt.Sprintf("**Startup probe failed** — check-script CLI mismatch:\n```\n%v\n```\nLikely cause: binary and Python scripts deployed from different commits. Refusing to start.", err))
		}
		os.Exit(1)
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
		// Refuse new cycles once SIGTERM/SIGINT has fired. The signal handler
		// closes stopCh; the inter-cycle wait at the bottom of this loop
		// returns on stopCh, so under normal pacing we never reach here while
		// draining. This early-out covers the race where SIGTERM arrives
		// between the inter-cycle wait returning and the next iteration top.
		if isDraining() {
			fmt.Println("[shutdown] draining, exiting trading loop.")
			return
		}

		processConfigReloads()

		cycleStart := time.Now()
		mu.Lock()
		state.CycleCount++
		cycle := state.CycleCount
		mu.Unlock()
		totalTrades := 0
		channelTrades :…25738 tokens truncated…tail
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
func runRobinhoodCheck(sc StrategyConfig, prices map[string]float64, posCtx PositionCtx, regime *RegimeConfig, logger *StrategyLogger) (*RobinhoodResult, string, float64, bool) {
	args := append([]string{}, sc.Args...)
	args = appendOpenCloseArgs(args, sc, posCtx)
	if sc.HTFFilter {
		args = append(args, "--htf-filter")
	}
	args = appendRegimeArgs(args, regime)
	if refsArgs, err := buildStrategyRefsArg(sc); err != nil {
		logger.Warn("Failed to marshal strategy refs: %v", err)
	} else if len(refsArgs) > 0 {
		args = append(args, refsArgs...)
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
		// #518: removed hardcoded 0.95 buffer for spot live buy.
		amountUSD = cash
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
		if result.CloseFraction > 0 && result.CloseFraction < 1 {
			// #519: partial close from the open/close registry sizes the
			// live order to the fraction so the exchange and virtual state
			// agree on the close leg.
			quantity = posQty * result.CloseFraction
		}
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
func executeRobinhoodResult(sc StrategyConfig, s *StrategyState, db *StateDB, result *RobinhoodResult, execResult *RobinhoodExecuteResult, signalStr string, price float64, logger *StrategyLogger) (int, string) {
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

	exec, err := ExecuteSpotSignalWithFillFeeDeferredOpen(s, result.Signal, result.Symbol, fillPrice, fillQty, fillFee, fillOID, result.CloseFraction, logger)
	if err != nil {
		logger.Error("Trade execution failed: %v", err)
		return 0, ""
	}
	trades := exec.TradesExecuted
	stampEntryATRIfOpened(s, result.Symbol, result.Indicators)
	stampPositionRegimeIfOpened(s, result.Symbol, result.Regime)
	if pos, ok := s.Positions[result.Symbol]; ok {
		recordPositionOpen(s, sc, exec.OpenTrade, pos)
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
func runOKXCheck(sc StrategyConfig, prices map[string]float64, posCtx PositionCtx, regime *RegimeConfig, logger *StrategyLogger) (*OKXResult, string, float64, bool) {
	args := append([]string{}, sc.Args...)
	args = appendOpenCloseArgs(args, sc, posCtx)
	if sc.HTFFilter {
		args = append(args, "--htf-filter")
	}
	args = appendRegimeArgs(args, regime)
	if refsArgs, err := buildStrategyRefsArg(sc); err != nil {
		logger.Warn("Failed to marshal strategy refs: %v", err)
	} else if len(refsArgs) > 0 {
		args = append(args, refsArgs...)
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
		skip = PerpsOrderSkipReason(result.Signal, posSide, EffectiveDirection(sc))
	} else {
		skip = SpotOrderSkipReason(result.Signal, posSide)
	}
	if skip != "" {
		logger.Info("Skipping live order for %s: %s", result.Symbol, skip)
		return nil, false
	}
	isBuy := result.Signal == 1
	// #254/#497/#518: perps sizing uses PerpsOpenNotional (sizing_leverage or
	// margin_per_trade_usd). EffectiveSizingLeverage returns 1 for spot, so
	// the spot branch below remains a simple cash buy. #518 removed the
	// hardcoded 0.95 safety buffer.
	sizingLeverage := EffectiveSizingLeverage(sc)
	exchangeLeverage := EffectiveExchangeLeverage(sc)
	marginPerTradeUSD := EffectiveMarginPerTradeUSD(sc)
	var size float64
	if sc.Type == "perps" {
		var ok bool
		var reason string
		size, ok, reason = perpsLiveOrderSize(result.Signal, price, cash, posQty, avgCost, sizingLeverage, exchangeLeverage, marginPerTradeUSD, posSide, EffectiveDirection(sc), result.CloseFraction)
		if !ok {
			logger.Info("%s for %s", reason, result.Symbol)
			return nil, false
		}
	} else {
		// Spot sizing: buy opens from cash, sell closes posQty. AllowShorts
		// does not apply to spot — SpotOrderSkipReason already blocked any
		// signal=-1 without a long above.
		if isBuy {
			budget := cash
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
			if result.CloseFraction > 0 && result.CloseFraction < 1 {
				// #519: partial close on OKX spot from the open/close registry.
				size = posQty * result.CloseFraction
			}
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
func executeOKXResult(sc StrategyConfig, s *StrategyState, db *StateDB, result *OKXResult, execResult *OKXExecuteResult, signalStr string, price float64, logger *StrategyLogger) (int, string) {
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
	var exec SignalExecutionResult
	var err error
	if sc.Type == "perps" {
		exec, err = ExecutePerpsSignalWithLeverageDeferredOpen(s, result.Signal, result.Symbol, fillPrice, EffectiveSizingLeverage(sc), EffectiveExchangeLeverage(sc), EffectiveMarginPerTradeUSD(sc), fillQty, fillOID, fillFee, EffectiveDirection(sc), result.CloseFraction, logger)
	} else {
		exec, err = ExecuteSpotSignalWithFillFeeDeferredOpen(s, result.Signal, result.Symbol, fillPrice, fillQty, fillFee, fillOID, result.CloseFraction, logger)
	}
	if err != nil {
		logger.Error("Trade execution failed: %v", err)
		return 0, ""
	}
	trades := exec.TradesExecuted
	stampEntryATRIfOpened(s, result.Symbol, result.Indicators)
	stampPositionRegimeIfOpened(s, result.Symbol, result.Regime)
	if pos, ok := s.Positions[result.Symbol]; ok {
		recordPositionOpen(s, sc, exec.OpenTrade, pos)
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
func runLeaderboardSummariesAndExit(lcs []LeaderboardSummaryConfig, cfg *Config, state *AppState, sdb *StateDB, notifier *MultiNotifier) {
	prices := fetchPricesForSummary(cfg)
	sharpeByStrategy := ComputeSharpeByStrategy(LoadClosedPositionsByStrategy(sdb, cfg), cfg, state)
	lifetimeStats := loadLifetimeStatsBestEffort(sdb, "[leaderboard]")
	posted := 0
	for _, lc := range lcs {
		msg := BuildLeaderboardSummary(lc, cfg, state, prices, sharpeByStrategy, lifetimeStats)
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

// loadLifetimeStatsBestEffort fetches per-strategy lifetime round-trip stats
// from the trades table, downgrading errors to a nil map (the same fallback
// FormatCategorySummary uses) so leaderboard #T / W/L columns render zero
// instead of failing the post. logPrefix tags the warning when the DB read
// fails ("[summary]" for the per-cycle paths, "[leaderboard]" for the
// on-demand paths). (#580)
func loadLifetimeStatsBestEffort(sdb *StateDB, logPrefix string) map[string]LifetimeTradeStats {
	if sdb == nil {
		return nil
	}
	stats, err := sdb.LifetimeTradeStatsAll()
	if err != nil {
		fmt.Printf("%s lifetime trade stats unavailable: %v\n", logPrefix, err)
		return nil
	}
	return stats
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
func collectDueLeaderboardSummaries(cfg *Config, state *AppState, prices map[string]float64, sharpeByStrategy map[string]float64, lifetimeStats map[string]LifetimeTradeStats) []pendingLeaderboardSummary {
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
		msg := BuildLeaderboardSummary(lc, cfg, state, prices, sharpeByStrategy, lifetimeStats)
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

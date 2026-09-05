package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

var knownSubcommands = []string{
	"init",
	"export",
	"manual-open",
	"manual-add",
	"manual-close",
	"force-close",
	"manual-cancel",
	"manual-clear-limit-row",
	"manual-update-sl",
	"manual-cancel-sl",
	"backfill",
	"probe",
	"inspect",
	"storage-inspect",
	"agent-info",
	"diagnostics",
	"version",
}

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
		case "manual-add":
			os.Exit(runManualAdd(os.Args[2:]))
		case "manual-close":
			os.Exit(runManualClose(os.Args[2:]))
		case "force-close":
			os.Exit(runForceClose(os.Args[2:]))
		case "manual-cancel":
			os.Exit(runManualCancel(os.Args[2:]))
		case "manual-clear-limit-row":
			os.Exit(runManualClearLimitRow(os.Args[2:]))
		case "manual-update-sl":
			os.Exit(runManualUpdateSL(os.Args[2:]))
		case "manual-cancel-sl":
			os.Exit(runManualCancelSL(os.Args[2:]))
		case "backfill":
			os.Exit(runBackfill(os.Args[2:]))
		case "probe":
			os.Exit(runProbe(os.Args[2:]))
		case "inspect":
			os.Exit(runInspect(os.Args[2:]))
		case "storage-inspect":
			os.Exit(runStorageInspect(os.Args[2:]))
		case "agent-info":
			os.Exit(runAgentInfo(os.Args[2:]))
		case "diagnostics":
			os.Exit(runDiagnostics(os.Args[2:]))
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

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}
	if err := applyAlertThrottleFromConfig(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to apply alert throttle interval: %v\n", err)
		os.Exit(1)
	}
	if err := applyKillSwitchResetDMTimeoutFromConfig(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to apply kill-switch reset DM timeout: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Loaded config: %d strategies, interval=%ds\n", len(cfg.Strategies), cfg.IntervalSeconds)

	setDirectionalCertStore(LoadDirectionalCertSetFailClosed(directionalCertPath(), func(f string, a ...interface{}) {
		fmt.Fprintf(os.Stderr, f+"\n", a...)
	}))
	directionalCertSummaryLines := directionalCertStartupSummary(cfg)
	for _, line := range directionalCertSummaryLines {
		fmt.Println(line)
	}

	explicitKeys, _ := loadStrategyExplicitKeys(*configPath)
	for _, sc := range cfg.Strategies {
		fmt.Println(formatStrategySummaryLine(sc, explicitKeys[sc.ID], cfg))
	}
	deprecatedEdgeWarnings := deprecatedEdgeStartupWarnings(cfg.Strategies)
	for _, msg := range deprecatedEdgeWarnings {
		fmt.Fprintln(os.Stderr, "[config] "+msg)
	}
	if line := dailyLossStartupSummaryLine(cfg.PortfolioRisk); line != "" {
		fmt.Println(line)
	}
	if line := dailyLossPaperStartupSummaryLine(cfg); line != "" {
		fmt.Println(line)
	}
	if line := exposureCapStartupSummaryLine(cfg.PortfolioRisk); line != "" {
		fmt.Println(line)
	}
	if line := exposureCapPaperStartupSummaryLine(cfg); line != "" {
		fmt.Println(line)
	}

	storageLayoutCfg, err := resolveStorageLayout(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[storage] CRITICAL: %v\n", err)
		os.Exit(ExitStorageOwnership)
	}
	storageIdent, err := buildStorageIdentityMap(cfg, storageLayoutCfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[storage] CRITICAL: %v\n", err)
		os.Exit(ExitStorageOwnership)
	}
	fmt.Printf("[storage] layout: %s\n", storageLayoutCfg.describe())
	for _, spec := range storageLayoutCfg.Files {
		fmt.Printf("[storage]   %s -> %s\n", spec.Role, spec.Canonical)
	}

	// One-shot reporting modes read only: no migration, no startup write, no
	// ownership lock, so they can run beside a live daemon.
	readOnlyReport := *summary != "" || *leaderboard

	// The notifier exists before any refusal that RestartPreventExitStatus
	// keeps down, so the owner is paged instead of reading stderr.
	notifier, cleanupNotifier := buildNotifierFromConfig(cfg)
	defer cleanupNotifier()

	var ownership *stateOwnership
	if !readOnlyReport && schedulerNeedsOwnership(*once) {
		owned, lockErr := acquireStateOwnership(storageLayoutCfg.Files)
		if lockErr != nil {
			msg := reportStateOwnershipFailure(lockErr)
			sendStartupRefusalDM(notifier, "Singleton guard", msg)
			cleanupNotifier()
			os.Exit(ExitSingletonLock)
		}
		ownership = owned
		defer ownership.Release()
	}

	if !readOnlyReport {
		si, inspectErr := inspectStorageOwnership(storageLayoutCfg, storageIdent, cfg, false)
		if inspectErr != nil {
			msg := fmt.Sprintf("ownership inspection failed: %v", inspectErr)
			fmt.Fprintf(os.Stderr, "[storage] CRITICAL: %s\n", msg)
			sendStartupRefusalDM(notifier, "Storage layout", fmt.Sprintf("%s (exit %d)", msg, ExitStorageOwnership))
			cleanupNotifier()
			os.Exit(ExitStorageOwnership)
		}
		if !si.OK() {
			fmt.Fprint(os.Stderr, formatStorageInspection(si))
			fmt.Fprintf(os.Stderr, "[storage] CRITICAL: refusing to start with a rejected storage layout (exit %d)\n", ExitStorageOwnership)
			sendStartupRefusalDM(notifier, "Storage layout", storageRefusalDMBody(si))
			cleanupNotifier()
			os.Exit(ExitStorageOwnership)
		}
	}

	var missingStateWarning string
	if msg := CheckStatePresence(cfg.DBFile, cfg.Strategies); msg != "" && !AllowMissingState() {
		fmt.Fprintln(os.Stderr, msg)
		missingStateWarning = msg
	}
	if paper, ok := storageLayoutCfg.spec(storageRolePaper); ok {
		if _, statErr := os.Stat(paper.Canonical); statErr != nil {
			fmt.Printf("[storage] paper state file %q is absent; it will be created on the first save\n", paper.Path)
		}
	}

	openStore := OpenStateStore
	if readOnlyReport {
		openStore = OpenStateStoreReadOnly
	}
	store, err := openStore(storageLayoutCfg, storageIdent)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to open state DB: %v\n", err)
		if readOnlyReport {
			fmt.Fprintln(os.Stderr, "One-shot reporting opens the state files read-only. If the schema predates this build, start the daemon once so migrations run, then retry.")
		}
		os.Exit(1)
	}
	defer store.Close()

	tradeRecorder = store.InsertTrade
	modelOnlyCloseUpdater = store.ReconcileModelOnlyClose
	modelOnlyCloseBasisLoader = store.LoadModelOnlyCloseBasis
	modelOnlyCloseAbandonMarker = store.MarkModelOnlyCloseAbandoned

	if strings.TrimSpace(cfg.ReplayLogPath) != "" {
		dl, dlErr := OpenDecisionLogDB(cfg.ReplayLogPath)
		if dlErr != nil {
			fmt.Fprintf(os.Stderr, "Failed to open replay decision log: %v\n", dlErr)
			os.Exit(1)
		}
		decisionLog = dl
		defer decisionLog.Close()
		decisionLogWriter = decisionLog.InsertDecision
	}
	rebuildReplayLiveSources(cfg)

	state, storageOrphans, err := LoadStateWithStore(cfg, store)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load state: %v\n", err)
		os.Exit(1)
	}
	ValidateState(state, cfg.Strategies)
	persistedStrategyIDs := make(map[string]bool, len(state.Strategies))
	for id := range state.Strategies {
		persistedStrategyIDs[id] = true
	}

	resolveCapitalPct(cfg.Strategies)

	var initialCapitalChangeInfos, initialCapitalChangeErrors []string
	if !readOnlyReport {
		initialCapitalChangeInfos, initialCapitalChangeErrors = ReconcileConfigInitialCapital(cfg, state, store)
	}

	sharedWalletTransitionBlockers := sharedWalletPoolTransitionBlockers(cfg.Strategies, state)
	var sharedWalletTransitionWarnings []string
	for i := range cfg.Strategies {
		sc := &cfg.Strategies[i]
		if sc.CapitalPct == 0 {
			syncHyperliquidLiveCapital(sc)
		}
		s, stateExisted := state.Strategies[sc.ID]
		if !stateExisted {
			s = NewStrategyState(*sc)
			state.Strategies[sc.ID] = s
			fmt.Printf("  Initialized strategy: %s (type=%s, capital=$%.0f)\n", sc.ID, sc.Type, sc.Capital)
		} else {
			if s.RiskState.MaxDrawdownPct != sc.MaxDrawdownPct {
				fmt.Printf("  Updated %s max_drawdown_pct: %.0f%% → %.0f%%\n", sc.ID, s.RiskState.MaxDrawdownPct, sc.MaxDrawdownPct)
				s.RiskState.MaxDrawdownPct = sc.MaxDrawdownPct
			}
			s.Platform = sc.Platform
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
		transitionErr := sharedWalletTransitionBlockers[sc.ID]
		poolTransition := sharedWalletPoolStateUnchanged
		if transitionErr == nil {
			poolTransition, transitionErr = applySharedWalletPoolStateMode(*sc, s)
		}
		if transitionErr != nil {
			msg := deferSharedWalletPoolTransition(sc, transitionErr)
			fmt.Fprintf(os.Stderr, "[CRITICAL] %s\n", msg)
			sharedWalletTransitionWarnings = append(sharedWalletTransitionWarnings, msg)
			continue
		}
		if poolTransition != sharedWalletPoolStateUnchanged {
			if stateExisted && !readOnlyReport {
				if err := store.PersistSharedWalletPoolStateTransition(s); err != nil {
					fmt.Fprintf(os.Stderr, "Failed to persist shared-wallet pool state for %s: %v\n", sc.ID, err)
					os.Exit(1)
				}
			}
			switch poolTransition {
			case sharedWalletPoolStateEntered:
				fmt.Printf("  Reset %s virtual allocation for shared-wallet pool budgeting\n", sc.ID)
			case sharedWalletPoolStateLeft:
				fmt.Printf("  Reseeded %s allocated cash book at $%.2f after leaving shared-wallet pool budgeting\n", sc.ID, s.InitialCapital)
			}
		}
	}

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
	for _, orphan := range storageOrphans {
		fmt.Printf("  Pruned stale strategy: %s [%s state file, %d position(s)]\n", orphan.StorageID, orphan.Role, orphan.PositionCount)
		if orphan.PositionCount > 0 {
			fmt.Fprintf(os.Stderr, "[storage] WARN: stale strategy %s in the %s state file still holds %d position(s); the next save of that file drops the row\n",
				orphan.StorageID, orphan.Role, orphan.PositionCount)
		}
		pruned = true
	}

	startupScopes := activeScopes(cfg.Strategies)
	liveCount, paperCount := scopeStrategyCounts(cfg.Strategies)
	fmt.Printf("[config] portfolio scopes: live=%d paper=%d strategies\n", liveCount, paperCount)

	if pruned {
		for _, scope := range startupScopes {
			prs := state.scopeRisk(scope)
			if prs.PeakValue <= 0 {
				continue
			}
			oldPeak := prs.PeakValue
			newPeak := rebaselinePortfolioPeakAfterPruneForScope(state, cfg, scope, nil)
			if newPeak != oldPeak {
				prs.PeakValue = newPeak
				fmt.Printf("  Portfolio peak rebaselined after prune [%s]: $%.0f -> $%.0f\n", scopeLabel(scope), oldPeak, newPeak)
			}
		}
	}

	directionConfigWarnings := ValidatePerpsDirectionConfig(state, cfg)

	atrMethodDriftWarnings := checkATRMethodDriftAtStartup(state, cfg)

	hedgeStateWarnings := validateHedgeStateConsistency(state, cfg)

	for _, scope := range startupScopes {
		prs := state.scopeRisk(scope)
		if prs.PeakValue != 0 {
			continue
		}
		if scopeHasPersistedState(cfg.Strategies, scope, persistedStrategyIDs) {
			fmt.Printf("  Portfolio peak for the %s scope will seed from its current book value on the first cycle (strategies already carry state)\n", scopeLabel(scope))
			continue
		}
		total := computeInitialPortfolioPeakForScope(cfg.Strategies, scope, nil)
		prs.PeakValue = total
		fmt.Printf("  Portfolio peak initialized [%s]: $%.0f\n", scopeLabel(scope), total)
	}

	ClearLatchedKillSwitchSharedWallet(state, cfg.Strategies, defaultSharedWalletBalance)

	logMgr, err := NewLogManager(cfg.LogDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to setup logging: %v\n", err)
		os.Exit(1)
	}
	defer logMgr.Close()

	var mu sync.RWMutex

	initShutdownContexts()

	statusPort := resolveStatusPort(*statusPortFlag, cfg.StatusPort)
	server := NewStatusServer(state, &mu, cfg.StatusToken, cfg.Strategies, store)
	server.SetConfigContext(*configPath, cfg)
	tuningManager, tuningErr := newTuningRunManager(*configPath, nil, cfg.tuningMaxRetainedRuns())
	if tuningErr != nil {
		fmt.Printf("[WARN] tuning API unavailable: %v\n", tuningErr)
	} else {
		server.tuning = tuningManager
	}
	markSchedulerStarted()
	if tuningManager != nil {
		go tuningManager.run(shutdownReadOnlyCtx)
	}
	server.Start(statusPort)

	diagWorker := newTradeDiagnosticsWorker(FetchUICandles, store.UpdateTradeDiagnosticsMetrics)
	diagWorker.splitStorage = store.Split()
	diagWorker.UpdateStrategies(cfg.Strategies)
	tradeDiagnosticsRecorder = store.InsertTradeDiagnostics
	tradeDiagnosticsEnqueue = diagWorker.Enqueue
	go diagWorker.run(shutdownReadOnlyCtx)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	stopCh := make(chan struct{})
	go func() {
		sig := <-sigCh
		fmt.Printf("\nReceived %s, draining...\n", sig)
		beginDrain()
		close(stopCh)
	}()

	fmt.Printf("Notification backends: %d active\n", notifier.BackendCount())
	server.SetNotifier(notifier)

	llmWorker := newLLMEntryAnalysisWorker(
		runLLMEntryAnalysisScript,
		func(job llmEntryAnalysisJob, verdict string) {
			mu.Lock()
			defer mu.Unlock()
			s, ok := state.Strategies[job.StrategyID]
			if !ok {
				return
			}
			if pos := s.Positions[job.Symbol]; pos != nil && pos.TradePositionID == job.PositionID {
				pos.LLMVerdict = verdict
			}
		},
		func(job llmEntryAnalysisJob, res *LLMEntryAnalysisResult) {
			for _, route := range notifier.tradeAlertRoutes(job.Platform, job.StratType, job.IsLive) {
				msg := formatLLMEntryAnalysisDigest(job, res, route.plainText)
				if job.Params.NotifyDM && route.dmDest != "" {
					if err := sendTradeDestination(route.notifier, route.dmDest, msg); err != nil {
						fmt.Printf("[notify] LLM analysis DM failed: %v\n", err)
					}
				}
				if job.Params.NotifyChannel {
					if route.channel != "" {
						if err := route.notifier.SendMessage(route.channel, msg); err != nil {
							fmt.Printf("[notify] LLM analysis digest failed: %v\n", err)
						}
					}
					if route.liveChan != "" {
						if err := route.notifier.SendMessage(route.liveChan, msg); err != nil {
							fmt.Printf("[notify] LLM analysis digest (live channel) failed: %v\n", err)
						}
					}
				}
			}
		},
	)
	llmEntryAnalysisEnqueue = llmWorker.Enqueue
	go llmWorker.run(shutdownReadOnlyCtx)
	if anyStrategyUsesLLMEntryAnalysis(cfg) && os.Getenv(llmEntryAnalysisAPIKeyEnv) == "" {
		fmt.Printf("[WARN] llm_entry_analysis enabled but %s is not set — analyses will fail (advisory only, trading unaffected)\n", llmEntryAnalysisAPIKeyEnv)
	}

	if d := notifier.DiscordBackend(); d != nil {
		if err := d.RegisterSlashCommands(server, cfg); err != nil {
			fmt.Printf("[WARN] Discord slash command registration failed: %v\n", err)
			if notifier.HasOwner() {
				notifier.SendOwnerDM("[slash] registration failed: " + err.Error())
			}
		} else {
			fmt.Println("Discord slash commands registered")
		}
	}

	defer func() {
		runDrain()
		mu.Lock()
		if err := SaveStateWithStore(state, store); err != nil {
			fmt.Fprintf(os.Stderr, "[shutdown] Failed to save state: %v\n", err)
		} else {
			fmt.Println("[shutdown] State saved.")
		}
		mu.Unlock()
		fmt.Println("[shutdown] Complete.")
	}()

	if notifier.HasOwner() {
		tradePersistWarn = func(msg string) {
			notifier.SendOwnerDM("[state] " + msg)
		}
		decisionLogPersistWarn = func(msg string) {
			notifier.SendOwnerDM("[replay] " + msg)
		}
		replayDriftWarn = func(msg string) {
			notifier.SendOwnerDM("[replay] " + msg)
		}
		initialCapitalGuardWarn = func(msg string) {
			notifier.SendOwnerDM("[state] " + msg)
		}
	}

	if notifier.HasOwner() {
		for _, msg := range initialCapitalChangeInfos {
			notifier.SendOwnerDM("[state] " + msg)
		}
		for _, msg := range initialCapitalChangeErrors {
			notifier.SendOwnerDM("[state] ERROR: " + msg)
		}
	}

	if len(sharedWalletTransitionWarnings) > 0 && notifier.HasOwner() {
		for _, msg := range sharedWalletTransitionWarnings {
			notifier.SendOwnerDM("[state] CRITICAL: " + msg)
		}
	}

	if len(directionConfigWarnings) > 0 && notifier.HasOwner() {
		for _, msg := range directionConfigWarnings {
			notifier.SendOwnerDM("[state] " + msg)
		}
	}

	if len(atrMethodDriftWarnings) > 0 && notifier.HasOwner() {
		for _, msg := range atrMethodDriftWarnings {
			notifier.SendOwnerDM("[state] " + msg)
		}
	}

	if len(hedgeStateWarnings) > 0 && notifier.HasOwner() {
		for _, msg := range hedgeStateWarnings {
			notifier.SendOwnerDM("[state] " + msg)
		}
	}

	notifyDirectionalCertStartupSummary(notifier, directionalCertSummaryLines)

	if len(deprecatedEdgeWarnings) > 0 && notifier.HasOwner() {
		for _, msg := range deprecatedEdgeWarnings {
			notifier.SendOwnerDM("[config] " + msg)
		}
	}

	if missingStateWarning != "" && notifier.HasOwner() {
		notifier.SendOwnerDM("[state] " + missingStateWarning)
	}

	if *summary != "" {
		runSummaryAndExit(*summary, cfg, state, store, notifier)
	}

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
		sharpeByStrategy := ComputeSharpeByStrategy(LoadClosedPositionsByStrategy(store, cfg), cfg, state)
		lifetimeStats := loadLifetimeStatsBestEffort(store, "[leaderboard]")
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

	if cfg.MigrationBaseVersion() < CurrentConfigVersion {
		go runConfigMigrationDM(cfg, notifier, *configPath)
	}

	if err := probeCheckScripts(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "[startup] check-script compatibility probe failed: %v\n", err)
		if notifier != nil && notifier.HasOwner() {
			notifier.SendOwnerDM(fmt.Sprintf("**Startup probe failed** — refusing to start (exit %d; fix deploy, then restart):\n```\n%v\n```", ExitProbeFailure, err))
		}
		os.Exit(ExitProbeFailure)
	}

	var lastNotifiedHash string

	if cfg.AutoUpdate != "off" {
		checkForUpdates(cfg, notifier, &lastNotifiedHash, &mu, state, store)
	}

	deribitPricer := NewDeribitPricer()
	fmt.Println("Option pricers ready (deribit: live API, ibkr: Black-Scholes)")

	lastRun := make(map[string]time.Time)
	var lastLiquidationAudit time.Time
	offCycleAuditSaveDirty := false
	lastSummaryPost := cloneTimeMap(state.LastSummaryPost)

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
		prevStrategies := append([]StrategyConfig(nil), cfg.Strategies...)
		changes, err := applyHotReloadConfig(cfg, nextCfg, state, notifier, server)
		if err != nil {
			mu.Unlock()
			fmt.Fprintf(os.Stderr, "[reload] ERROR: reload rejected; keeping previous config: %v\n", err)
			return
		}
		tickSeconds = schedulerTickSeconds(cfg)
		drawdownWarnThresholdPct = configuredDrawdownWarnThresholdPct(cfg)
		mu.Unlock()

		diagWorker.UpdateStrategies(cfg.Strategies)

		setDirectionalCertStore(LoadDirectionalCertSetFailClosed(directionalCertPath(), func(f string, a ...interface{}) {
			fmt.Fprintf(os.Stderr, "[reload] "+f+"\n", a...)
		}))
		reloadCertLines := directionalCertStartupSummary(cfg)
		for _, line := range reloadCertLines {
			fmt.Printf("[reload] %s\n", line)
		}
		notifyDirectionalCertStartupSummary(notifier, reloadCertLines)

		for _, msg := range newlyDeprecatedEdgeWarnings(prevStrategies, cfg.Strategies) {
			fmt.Fprintln(os.Stderr, "[reload] "+msg)
			if notifier.HasOwner() {
				notifier.SendOwnerDM("[reload] " + msg)
			}
		}

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

	lastAutoUpdateCheck := time.Now()

	var resetGoroutineRunning atomic.Bool
	sharedWalletRiskBalances := make(map[SharedWalletKey]sharedWalletRiskBalanceSnapshot)
	sharedWalletRiskGeneration := 0

	for {
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
		channelTrades := make(map[string]int)
		channelTradeDetails := make(map[string][]string)

		mu.Lock()
		manualAlerts := drainPendingManualActions(state, cfg, store)
		mu.Unlock()
		for _, ma := range manualAlerts {
			sendTradeAlerts(ma.sc, ma.ss, ma.trades, &mu, notifier)
		}

		limitAlerts := reconcilePendingLimitOrders(state, cfg, store, &mu, notifier, logMgr)
		for _, ma := range limitAlerts {
			sendTradeAlerts(ma.sc, ma.ss, ma.trades, &mu, notifier)
		}

		resolveCapitalPct(cfg.Strategies)

		mu.RLock()
		intervals := effectiveStrategyIntervals(cfg.Strategies, state.Strategies, cfg.IntervalSeconds, drawdownWarnThresholdPct)
		mu.RUnlock()

		dueStrategies := make([]StrategyConfig, 0)
		for _, sc := range cfg.Strategies {
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

		dueStrategies = orderReplaySourcesBeforeMirrors(dueStrategies)

		if len(dueStrategies) == 0 {
			if audSec := liquidationAuditIntervalSeconds(cfg.Strategies, intervals); audSec > 0 && os.Getenv("HYPERLIQUID_ACCOUNT_ADDRESS") != "" {
				now := time.Now().UTC()
				if lastLiquidationAudit.IsZero() || now.Sub(lastLiquidationAudit) >= time.Duration(audSec)*time.Second {
					if allScopesSaveBlocked(store, cfg) {
						fmt.Println("[CRITICAL] State save failed 3x, skipping off-cycle liquidation audit this pass")
						offCycleAuditSaveDirty = flushOffCycleLiquidationAuditState(state, cfg, store, &mu, 0, offCycleAuditSaveDirty, true)
						lastLiquidationAudit = time.Now().UTC()
						continue
					}
					mutations := runOffCycleLiquidationAudit(cfg.Strategies, state, &mu, notifier, logMgr)
					lastLiquidationAudit = time.Now().UTC()
					offCycleAuditSaveDirty = flushOffCycleLiquidationAuditState(state, cfg, store, &mu, mutations, offCycleAuditSaveDirty, false)
					continue
				}
				delay := schedulerDelay(cfg.Strategies, intervals, lastRun, cfg.IntervalSeconds, time.Now(), tickSeconds)
				if wait := time.Until(lastLiquidationAudit.Add(time.Duration(audSec) * time.Second)); wait < delay {
					delay = wait
				}
				if minTick := time.Duration(tickSeconds) * time.Second; delay < minTick {
					delay = minTick
				}
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
					fmt.Println("[shutdown] exiting trading loop.")
					return
				}
			}
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
				fmt.Println("[shutdown] exiting trading loop.")
				return
			}
		}

		fmt.Printf("\n=== Cycle %d starting at %s (%d/%d strategies due) ===\n",
			cycle, cycleStart.UTC().Format("2006-01-02 15:04:05 UTC"),
			len(dueStrategies), len(cfg.Strategies))

		symbols := collectPriceSymbols(cfg.Strategies)
		futuresSymbols := collectFuturesMarkSymbols(cfg.Strategies)
		hlPerpsCoins, okxPerpsCoins := collectPerpsMarkSymbols(cfg.Strategies)

		prices := make(map[string]float64)
		if len(symbols) > 0 {
			p, err := FetchPrices(symbols)
			if err != nil {
				fmt.Printf("[CRITICAL] Price fetch failed: %v — skipping cycle\n", err)
				continue
			}
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
		if len(futuresSymbols) > 0 {
			marks, mode, err := FetchFuturesMarks(futuresSymbols)
			if err != nil {
				fmt.Printf("[WARN] Futures marks fetch failed for %v: %v — portfolio notional will use entry cost for open futures positions\n", futuresSymbols, err)
			} else {
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

		var totalPV float64
		sharedWallets := detectSharedWallets(cfg.Strategies)
		walletBalances := make(map[SharedWalletKey]float64)

		if allScopesSaveBlocked(store, cfg) {
			fmt.Println("[CRITICAL] State save failed 3x, skipping trades this cycle")
			globalRegimeStore.resetForCycle(time.Now().UTC())
			mu.Lock()
			for _, ss := range state.Strategies {
				if ss != nil {
					ss.SharedWalletValueSet = false
					ss.SharedWalletPerformanceOnly = false
				}
			}
			state.LatestSharedWalletBalances = nil
			state.LatestSharedWalletMembers = nil
			mu.Unlock()
			mu.RLock()
			totalPV, _ = computeTotalPortfolioValue(cfg.Strategies, state, prices, nil, sharedWallets)
			mu.RUnlock()
		} else {
			regimeStoreReady := startRegimeStorePopulation(globalRegimeStore, dueStrategies, cfg.Regime, notifier)
			cycleScopes := activeScopes(cfg.Strategies)
			scopeRisk := make(map[PortfolioScope]*scopeCycleRisk, len(cycleScopes))
			for _, scope := range cycleScopes {
				scopeRisk[scope] = &scopeCycleRisk{Scope: scope, Config: scopeRiskConfig(cfg, scope)}
			}

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
			var hlReconcileAll []StrategyConfig
			for _, sc := range cfg.Strategies {
				if isHLLiveReconcilable(sc) {
					hlReconcileAll = append(hlReconcileAll, sc)
				}
			}
			var hlReconcileDue []StrategyConfig
			for _, sc := range dueStrategies {
				if isHLLiveReconcilable(sc) {
					hlReconcileDue = append(hlReconcileDue, sc)
				}
			}
			hlKillSwitchAll := hlReconcileAll

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

			var tsLiveAll []StrategyConfig
			for _, sc := range cfg.Strategies {
				if sc.Platform == "topstep" && sc.Type == "futures" && topstepIsLive(sc.Args) {
					tsLiveAll = append(tsLiveAll, sc)
				}
			}

			hlAddr := os.Getenv("HYPERLIQUID_ACCOUNT_ADDRESS")
			hlKey := SharedWalletKey{Platform: "hyperliquid", Account: hlAddr}
			_, hlShared := sharedWallets[hlKey]

			var hlPositions []HLPosition
			var hlStateFetched bool
			var hlSnapshotAt time.Time
			if hlAddr != "" && len(hlLiveAll) > 0 {
				bal, pos, err := fetchHyperliquidState(hlAddr)
				if err != nil {
					fmt.Printf("[WARN] hyperliquid clearinghouseState fetch failed: %v — falling back to per-wallet max and skipping position sync this cycle\n", err)
				} else {
					hlStateFetched = true
					hlSnapshotAt = time.Now().UTC()
					hlPositions = pos
					if hlShared {
						walletBalances[hlKey] = bal
					}
				}
			}

			var walletLedgerFetches []walletLedgerFetchResult
			if hlShared && hlStateFetched {
				walletLedgerFetches = append(walletLedgerFetches,
					fetchWalletLedgerEvents(storeLiveDB(store), hlKey, time.Now().UTC()))
			}

			okxHasCreds := os.Getenv("OKX_API_KEY") != ""
			okxKey := SharedWalletKey{Platform: "okx", Account: os.Getenv("OKX_API_KEY")}
			_, okxShared := sharedWallets[okxKey]
			var okxPositions []OKXPosition
			var okxStateFetched bool
			var okxBalanceFetched bool
			var okxSnapshotUPnL float64
			var okxSnapshotAt time.Time
			if okxHasCreds && len(okxLivePerps) > 0 {
				pos, err := defaultOKXPositionsFetcher()
				if err != nil {
					fmt.Printf("[WARN] okx fetch_positions failed: %v — skipping per-strategy OKX circuit enqueue this cycle\n", err)
				} else {
					okxStateFetched = true
					okxPositions = pos
				}
			}
			if okxHasCreds && okxShared {
				if eq, upnl, err := defaultOKXEquitySnapshot(); err != nil {
					fmt.Printf("[WARN] okx balance fetch failed: %v — falling back to per-wallet max this cycle\n", err)
				} else {
					walletBalances[okxKey] = eq
					okxBalanceFetched = true
					okxSnapshotUPnL = upnl
					okxSnapshotAt = time.Now().UTC()
				}
			}
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
			tsKey, tsShared := detectTopStepSharedWallet(cfg.Strategies)
			var tsBalanceFetched bool
			var tsSnapshotEquity float64
			var tsSnapshotUPnL float64
			var tsSnapshotAt time.Time
			if tsShared {
				if eq, upnl, err := defaultTopStepEquitySnapshot(); err != nil {
					fmt.Printf("[WARN] topstep balance fetch failed: %v — shadow cash-flow journal skipped this cycle\n", err)
				} else {
					tsBalanceFetched = true
					tsSnapshotEquity = eq
					tsSnapshotUPnL = upnl
					tsSnapshotAt = time.Now().UTC()
				}
			}

			sharedWalletRiskGeneration++
			mu.RLock()
			riskWalletBalances, usedStaleRiskBalance, pooledEquityComplete := resolveSharedWalletRiskBalances(
				cfg.Strategies, state.Strategies, sharedWallets, walletBalances,
				sharedWalletRiskBalances, sharedWalletRiskGeneration)
			riskOpenSymbols := snapshotOpenSymbolsByStrategy(state)
			totalPV = 0
			for _, scope := range cycleScopes {
				sr := measureScopeCycleRisk(scope, scopeRisk[scope].Config, cfg.Strategies, state, prices,
					riskWalletBalances, sharedWallets, pooledEquityComplete, usedStaleRiskBalance, time.Now().UTC())
				scopeRisk[scope] = sr
				totalPV += sr.TotalPV
			}
			mu.RUnlock()

			var manualBasisRebaselineDM string
			mu.Lock()
			if liveSR := scopeRisk[ScopeLive]; liveSR != nil {
				livePrs := state.scopeRisk(ScopeLive)
				liveCfgs := strategiesInScope(cfg.Strategies, ScopeLive)
				if !livePrs.ManualMarkBasisRebaselined {
					if unmarked := missingManualOnlyMarks(liveCfgs, riskOpenSymbols, prices); len(unmarked) > 0 {
						fmt.Printf("[INFO] manual mark valuation-basis peak migration deferred: no live mark this cycle for open manual-only coin(s) %s\n", strings.Join(unmarked, ", "))
					} else {
						legacyPrices := pricesWithoutSymbols(prices, manualOnlyMarkSymbols(liveCfgs))
						legacyPV, _ := computeSubsetPortfolioValue(liveCfgs, state, legacyPrices, riskWalletBalances, sharedWallets)
						priorPeak := livePrs.PeakValue
						if newPeak, apply := manualMarkBasisPeakAdjustment(priorPeak, liveSR.TotalPV, legacyPV); apply {
							livePrs.PeakValue = newPeak
							details := fmt.Sprintf("#1444 manual valuation-basis migration: peak $%.2f -> $%.2f (live total $%.2f vs pre-#1444 basis $%.2f, delta $%.2f); drawdown untouched",
								priorPeak, newPeak, liveSR.TotalPV, legacyPV, liveSR.TotalPV-legacyPV)
							addKillSwitchEvent(livePrs, "basis_rebaseline", "equity", livePrs.CurrentDrawdownPct, liveSR.TotalPV, newPeak, details)
							fmt.Printf("[CRITICAL] %s\n", details)
							manualBasisRebaselineDM = formatManualMarkBasisRebaselineDM(priorPeak, newPeak, liveSR.TotalPV, legacyPV)
						} else {
							fmt.Printf("[INFO] #1444 manual valuation-basis migration: no peak change (peak $%.2f, live total $%.2f, pre-#1444 basis $%.2f)\n",
								priorPeak, liveSR.TotalPV, legacyPV)
						}
						livePrs.ManualMarkBasisRebaselined = true
					}
				}
			}

			for _, scope := range cycleScopes {
				sr := scopeRisk[scope]
				applyScopeCycleRisk(sr, state.scopeRisk(scope))
				if sr.KillSwitchFired {
					fmt.Printf("[CRITICAL] Portfolio kill switch [%s]: %s\n", scopeLabel(scope), sr.Reason)
					for _, id := range stateIDsInScope(state.Strategies, cfg.Strategies, scope) {
						ss := state.Strategies[id]
						if ss == nil {
							continue
						}
						ss.RiskState.clearPendingCircuitClose(PlatformPendingCloseHyperliquid)
						ss.RiskState.clearPendingCircuitClose(PlatformPendingCloseOKX)
						ss.RiskState.clearPendingCircuitClose(PlatformPendingCloseOKXSpot)
						ss.RiskState.clearPendingCircuitClose(PlatformPendingCloseRobinhood)
						ss.RiskState.clearPendingCircuitClose(PlatformPendingCloseRobinhoodOptions)
						ss.RiskState.clearPendingCircuitClose(PlatformPendingCloseTopStep)
					}
				}
				if sr.NotionalBlocked {
					fmt.Printf("[WARN] [%s] %s\n", scopeLabel(scope), sr.Reason)
				}
				if sr.DailyLossEntriesHeld {
					fmt.Printf("[WARN] [%s] %s — entries held until UTC rollover\n", scopeLabel(scope), dailyLossHoldDetail(sr.DailyLossStatus))
				}
				if sr.DailyLossStatus.PctBasisMiss {
					fmt.Printf("[WARN] [%s] %s\n", scopeLabel(scope), dailyLossPctBasisMissWarning)
				}
			}
			ingestSharedWalletLedgers(storeLiveDB(store), state, cfg.Strategies, sharedWallets, walletLedgerFetches)
			driftResults := reconcileSharedWalletDisplayValues(cfg.Strategies, state, storeLiveDB(store), sharedWallets, walletBalances, hlPositions, okxPositions, okxStateFetched)
			mu.Unlock()

			if manualBasisRebaselineDM != "" {
				notifier.SendOwnerDM(manualBasisRebaselineDM)
			}
			for _, scope := range cycleScopes {
				sr := scopeRisk[scope]
				today := time.Now().UTC().Format("2006-01-02")
				if sr.DailyLossEntriesHeld {
					if dailyLossAlertDue(true, dailyLossLastAlertDate[scope], today) {
						dailyLossLastAlertDate[scope] = today
						notifier.SendOwnerDM(scopePrefixedDM(scope, formatDailyLossTripDM(sr.DailyLossStatus, time.Now().UTC())))
					}
				}
				if sr.DailyLossStatus.PctBasisMiss {
					if dailyLossAlertDue(true, dailyLossPctBasisMissAlertDate[scope], today) {
						dailyLossPctBasisMissAlertDate[scope] = today
						notifier.SendOwnerDM(scopePrefixedDM(scope, formatDailyLossPctBasisMissDM(sr.DailyLossStatus, time.Now().UTC())))
					}
				}
				if warnMsg := exposureCapCycleWarning(sr.ExposureCapStatus); warnMsg != "" {
					fmt.Printf("[WARN] [%s] %s\n", scopeLabel(scope), warnMsg)
				}
				if skipMsg := exposureCapSkippedWarning(sr.ExposureCapStatus); skipMsg != "" {
					fmt.Printf("[WARN] [%s] %s\n", scopeLabel(scope), skipMsg)
				}
				if sr.ExposureCapStatus.PVBasisMiss {
					fmt.Printf("[WARN] [%s] %s\n", scopeLabel(scope), exposureCapPVBasisMissWarning)
				}
				exposureCapDM, exposureCapNextAlerts := exposureCapAlertMessage(sr.ExposureCapStatus, exposureCapAlerts[scope], time.Now().UTC())
				exposureCapAlerts[scope] = exposureCapNextAlerts
				if exposureCapDM != "" {
					notifier.SendOwnerDM(scopePrefixedDM(scope, exposureCapDM))
				}
			}

			if hlShared && hlStateFetched {
				rec := reconcileCashflowJournal(storeLiveDB(store), hlKey, walletBalances[hlKey], sumHLAccountUPnL(hlPositions), hlSnapshotAt)
				applyCashflowJournalDriftBasis(driftResults, hlKey, rec, cashflowJournalAlarmEnabled())
			}

			if okxShared && okxBalanceFetched {
				okxRec := reconcileOKXCashflowJournal(storeLiveDB(store), okxKey, walletBalances[okxKey], okxSnapshotUPnL, okxSnapshotAt)
				logOKXCashflowJournalShadow(driftResults, okxKey, okxRec)
			}

			if tsShared && tsBalanceFetched {
				tsRec := reconcileTopStepCashflowJournal(storeLiveDB(store), tsKey, tsSnapshotEquity, tsSnapshotUPnL, tsSnapshotAt)
				logTopStepCashflowJournalShadow(driftResults, tsKey, tsRec)
			}

			reportSharedWalletDrift(notifier, driftResults)

			liveKillSwitchFired := scopeCycleRiskFired(scopeRisk, ScopeLive)
			var liveReason string
			if liveSR := scopeRisk[ScopeLive]; liveSR != nil {
				liveReason = liveSR.Reason
			}

			var plan KillSwitchClosePlan
			var hlVirtualQty hlVirtualQuantitySnapshot
			if liveKillSwitchFired {
				mu.RLock()
				hlSLOIDs := collectHLKillSwitchStopOIDs(state.Strategies, hlKillSwitchAll)
				hlHedgeCoins := map[string]bool{}
				for _, sc := range hlLiveAll {
					if ss, ok := state.Strategies[sc.ID]; ok {
						if coin := heldHedgeCoin(sc, ss); coin != "" {
							hlHedgeCoins[coin] = true
						}
					}
				}
				hlVirtualQty = snapshotHyperliquidVirtualQuantities(state.Strategies, hlKillSwitchAll)
				mu.RUnlock()

				inputs := KillSwitchCloseInputs{
					HLAddr:            hlAddr,
					HLStateFetched:    hlStateFetched,
					HLPositions:       hlPositions,
					HLLiveAll:         hlKillSwitchAll,
					HLHedgeCoins:      hlHedgeCoins,
					HLCloser:          defaultHyperliquidLiveCloser,
					HLFetcher:         defaultHLStateFetcher,
					HLNoFillRecoverer: defaultHLKillSwitchNoFillRecoverer,
					HLStopLossOIDs:    hlSLOIDs,
					HLLimitOrderLoader: func() ([]PendingLimitOrder, error) {
						if store == nil {
							return nil, nil
						}
						return store.LoadPendingLimitOrders()
					},
					HLLimitOrderRoster: killSwitchLimitOrderRoster(cfg.Strategies),
					HLLimitOrderDeps: killSwitchLimitOrderDeps{
						Cancel: func(script, symbol string, oid int64) (*HyperliquidCancelOrderResult, string, error) {
							return runHyperliquidCancelOrderFn(script, symbol, oid)
						},
						Status: func(script, symbol string, oids []int64, sinceMs int64) (*HyperliquidLimitStatusResult, string, error) {
							return runHyperliquidLimitStatusFn(script, symbol, oids, sinceMs)
						},
						Delete: func(id int64) error {
							if store == nil {
								return fmt.Errorf("state db unavailable")
							}
							return store.DeletePendingLimitOrder(id)
						},
						Flush: func() error {
							mu.Lock()
							defer mu.Unlock()
							return SaveStateWithStore(state, store)
						},
						MarkCancelRequested: func(strategyID, symbol string) (int64, error) {
							if store == nil {
								return 0, fmt.Errorf("state db unavailable")
							}
							return store.MarkPendingLimitOrderCancelRequested(strategyID, symbol)
						},
					},
					HLLimitOrderTimeout: 60 * time.Second,
					OKXLiveAllPerps:     okxLivePerps,
					OKXLiveAllSpot:      okxLiveSpot,
					OKXCloser:           defaultOKXLiveCloser,
					OKXFetcher:          defaultOKXPositionsFetcher,
					RHLiveCrypto:        rhLiveCrypto,
					RHLiveOptions:       rhLiveOptions,
					RHCloser:            defaultRobinhoodLiveCloser,
					RHFetcher:           defaultRobinhoodPositionsFetcher,
					TSLiveAll:           tsLiveAll,
					TSCloser:            defaultTopStepLiveCloser,
					TSFetcher:           defaultTopStepPositionsFetcher,
					PortfolioReason:     liveReason,
					CloseTimeout:        90 * time.Second,
					RHCloseTimeout:      150 * time.Second,
				}
				plan = planKillSwitchClose(inputs)
				for _, line := range plan.LogLines {
					fmt.Println(line)
				}
			}

			if liveKillSwitchFired && !plan.OnChainConfirmedFlat {
				mu.Lock()
				applyKillSwitchSettledLegsWhileLatched(state.Strategies, strategiesInScope(cfg.Strategies, ScopeLive), &plan, hlKillSwitchAll, hlVirtualQty, prices, nil)
				mu.Unlock()
			}

			killSwitchAutoReset := false
			if liveKillSwitchFired && plan.OnChainConfirmedFlat {
				liveSR := scopeRisk[ScopeLive]
				mu.Lock()
				for _, sc := range strategiesInScope(cfg.Strategies, ScopeLive) {
					if s, ok := state.Strategies[sc.ID]; ok {
						forceCloseKillSwitchPositions(s, sc, prices, plan.CloseReport.Fills, hlKillSwitchAll, hlVirtualQty, nil)
					}
				}
				if !notifier.HasOwner() {
					if plan.CanAutoResetWithoutOwner() {
						peakRebaselineAvailable := liveSR.PeakRebaselineAvailable
						killSwitchAutoReset = AutoResetConfirmedFlatKillSwitch(state.scopeRisk(ScopeLive), liveSR.TotalPV,
							peakRebaselineAvailable,
							"confirmed flat after portfolio kill-switch close; no DM owner configured, latch auto-cleared")
						if killSwitchAutoReset {
							if peakRebaselineAvailable {
								fmt.Printf("[CRITICAL] Portfolio kill switch auto-reset after confirmed flat close (no owner configured, peak re-baselined to $%.2f)\n", liveSR.TotalPV)
							} else {
								fmt.Printf("[CRITICAL] Portfolio kill switch auto-reset after confirmed flat close (no owner configured, prior peak retained because current equity is not trustworthy)\n")
							}
						}
					} else {
						fmt.Println("[CRITICAL] Portfolio kill switch auto-reset suppressed: operator-required close gaps remain")
					}
				}
				mu.Unlock()
			}

			if liveKillSwitchFired && notifier.HasBackends() && plan.DiscordMessage != "" {
				killSwitchMsg := plan.DiscordMessage
				if killSwitchAutoReset {
					killSwitchMsg = formatKillSwitchAutoResetMessage(killSwitchMsg)
				}
				notifier.SendToAllChannels(killSwitchMsg)
			}

			var paperKillSwitch paperKillSwitchOutcome
			if paperSR := scopeRisk[ScopePaper]; paperSR != nil && paperSR.KillSwitchFired {
				mu.Lock()
				paperKillSwitch = applyPaperKillSwitchCycle(state, cfg, prices, paperSR, notifier.HasOwner())
				mu.Unlock()
				if paperKillSwitch.CloseApplied {
					fmt.Printf("[CRITICAL] Portfolio kill switch [paper]: force-closed %d paper strategy book(s) at mark\n", len(paperKillSwitch.Closed))
				}
				if paperKillSwitch.AutoReset {
					fmt.Printf("[CRITICAL] Portfolio kill switch [paper] auto-reset after the virtual close (no owner configured, peak re-baselined to $%.2f)\n", paperSR.TotalPV)
				}
			}
			if paperKillSwitch.Message != "" && notifier.HasBackends() {
				notifier.SendToScopeChannels(ScopePaper, paperKillSwitch.Message)
			}

			for _, moAlert := range drainModelOnlyCloseAlerts() {
				if notifier.HasOwner() {
					notifier.SendOwnerDM(formatModelOnlyCloseDM(moAlert))
				}
			}

			for _, scope := range cycleScopes {
				sr := scopeRisk[scope]
				if !sr.Warning {
					portfolioWarningAlertsReset(scope)
					continue
				}
				if !notifier.HasBackends() {
					continue
				}
				warnNow := time.Now().UTC()
				mu.RLock()
				prs := state.scopeRiskIfPresent(scope)
				if prs == nil {
					mu.RUnlock()
					continue
				}
				warnEquityInBand, warnMarginInBand := portfolioWarnBandSignals(sr.Config, prs, sr.EquityAvailable)
				warnEquityDD := prs.CurrentDrawdownPct
				warnMarginDD := prs.CurrentMarginDrawdownPct
				warnLatchDeferredSince := prs.UntrustedOverLimitSince
				mu.RUnlock()
				notifyWarn, nextWarnAlerts := portfolioWarningShouldNotify(
					portfolioWarningAlerts[scope], warnEquityInBand, warnMarginInBand, warnEquityDD, warnMarginDD, warnNow)
				portfolioWarningAlerts[scope] = nextWarnAlerts
				if !warnLatchDeferredSince.IsZero() {
					notifyWarn = true
				}

				var recentTrades []Trade
				if notifyWarn && store != nil {
					if rows, err := store.RecentTrades(warnNow.Add(-portfolioWarningRecentWindow), portfolioWarningMaxRows); err != nil {
						fmt.Printf("[WARN] portfolio warning recent-trade lookup failed: %v\n", err)
					} else {
						recentTrades = rows
					}
				}
				var warnMsg string
				mu.Lock()
				source := "equity"
				warnDD := prs.CurrentDrawdownPct
				if prs.CurrentMarginDrawdownPct >= warnDD {
					source = "margin"
					warnDD = prs.CurrentMarginDrawdownPct
				}
				if sr.WarnBandEntered {
					addKillSwitchEvent(prs, "warning", source, warnDD, sr.TotalPV, prs.PeakValue, sr.Reason)
					prs.Events[len(prs.Events)-1].Scope = scope
				}
				if notifyWarn {
					warnMsg = BuildPortfolioWarningMessage(PortfolioWarningMessageInputs{
						Reason:           sr.Reason,
						Config:           sr.Config,
						State:            state,
						Scope:            scope,
						CfgStrategies:    cfg.Strategies,
						Prices:           prices,
						TotalValue:       sr.TotalPV,
						PerpsLoss:        sr.PerpsLoss,
						PerpsMargin:      sr.PerpsMargin,
						Recent:           recentTrades,
						Now:              warnNow,
						EquityGuardArmed: sr.EquityAvailable && prs.PeakValue > 0,
					})
				}
				mu.Unlock()
				if notifyWarn {
					notifier.SendToScopeChannels(scope, warnMsg)
					notifier.SendOwnerDM(warnMsg)
				}
				if warnLatchDeferredSince.IsZero() {
					fmt.Printf("[WARN] [%s] %s\n", scopeLabel(scope), sr.Reason)
				} else {
					fmt.Printf("[CRITICAL] [%s] %s\n", scopeLabel(scope), sr.Reason)
				}
			}

			if cfg.Correlation != nil && cfg.Correlation.Enabled {
				for _, scope := range cycleScopes {
					mu.RLock()
					corrSnap := ComputeCorrelation(
						filterStatesByScope(state.Strategies, cfg.Strategies, scope),
						strategiesInScope(cfg.Strategies, scope),
						prices, cfg.Correlation)
					mu.RUnlock()

					mu.Lock()
					state.setScopeCorrelation(scope, corrSnap)
					mu.Unlock()

					if len(corrSnap.Warnings) > 0 && notifier.HasBackends() {
						msg := fmt.Sprintf("**CORRELATION WARNING [%s]**\n%s", scopeLabel(scope), strings.Join(corrSnap.Warnings, "\n"))
						notifier.SendToScopeChannels(scope, msg)
						notifier.SendOwnerDM(msg)
					}
				}
			}

			mu.RLock()
			latchedNow := state.latchedScopes()
			mu.RUnlock()
			var promptScopes []PortfolioScope
			promptPlans := map[PortfolioScope]KillSwitchClosePlan{}
			for _, scope := range cycleScopes {
				sr := scopeRisk[scope]
				if !sr.KillSwitchFired || !scopeInList(scope, latchedNow) {
					continue
				}
				promptScopes = append(promptScopes, scope)
				if scope == ScopeLive {
					promptPlans[scope] = plan
				} else {
					promptPlans[scope] = KillSwitchClosePlan{OnChainConfirmedFlat: true, DiscordMessage: formatPaperKillSwitchPromptMessage(sr.Reason)}
				}
			}
			if len(promptScopes) > 0 && notifier.HasOwner() && tryClaimKillSwitchResetPrompt(&resetGoroutineRunning) {
				resetPrompt := formatKillSwitchResetPromptForScopes(killSwitchInstanceLabel(*configPath), hlAddr, promptPlans, promptScopes, latchedNow)
				resetDMTimeout := effectiveKillSwitchResetDMTimeout()
				go func() {
					defer releaseKillSwitchResetPrompt(&resetGoroutineRunning)
					resp, err := notifier.AskOwnerDM(resetPrompt, resetDMTimeout)
					if err != nil {
						fmt.Printf("[update] Kill switch reset DM timed out or failed: %v\n", err)
						return
					}
					mu.RLock()
					latched := state.latchedScopes()
					mu.RUnlock()
					target, parseErr := parseKillSwitchResetReply(resp, latched)
					if parseErr != nil {
						fmt.Printf("[update] Kill switch reset DM got unexpected reply: %q (%v)\n", resp, parseErr)
						notifier.SendOwnerDM(parseErr.Error())
						return
					}
					mu.Lock()
					prs := state.scopeRisk(target)
					resetDrawdownPct := ResetPortfolioKillSwitchManual(prs)
					addKillSwitchEvent(prs, "reset", "", resetDrawdownPct, 0, prs.PeakValue,
						fmt.Sprintf("manual reset via DM (%s scope)", scopeLabel(target)))
					prs.Events[len(prs.Events)-1].Scope = target
					if err := SaveStateWithStore(state, store); err != nil {
						fmt.Printf("[CRITICAL] Failed to save state after kill switch reset: %v\n", err)
					}
					remaining := state.latchedScopes()
					mu.Unlock()
					ack := fmt.Sprintf("Kill switch reset (%s scope). Trading will resume next cycle.", scopeLabel(target))
					if len(remaining) > 0 {
						ack += fmt.Sprintf(" The %s scope stays latched; a new prompt follows next cycle.", joinScopeLabels(remaining))
					}
					notifier.SendOwnerDM(ack)
					fmt.Printf("[update] Kill switch reset by owner via DM (%s scope)\n", scopeLabel(target))
					return
				}()
			}

			var hlReconcileFillHintsJSON []byte
			hlOnChainAbsQty, hlLiquidationPx, hlNetSideByCoin := buildHLLiquidationMaps(hlPositions)

			if !liveKillSwitchFired {
				if len(hlLiveAll) > 0 {
					runPendingHyperliquidCircuitCloses(
						shutdownSideEffectCtx,
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
				if len(okxLivePerps) > 0 && okxHasCreds {
					runPendingOKXCircuitCloses(
						shutdownSideEffectCtx,
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
				if len(tsLiveAll) > 0 {
					runPendingTopStepCircuitCloses(
						shutdownSideEffectCtx,
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
				if len(rhLiveCrypto) > 0 {
					runPendingRobinhoodCircuitCloses(
						shutdownSideEffectCtx,
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
				drainOperatorRequiredPendingCloses(state, notifier, &mu)
				if len(hlReconcileDue) > 0 && hlStateFetched {
					_, fillHints, orphanCloseJobs := reconcileHyperliquidAccountPositions(hlReconcileDue, hlReconcileAll, state, &mu, logMgr, hlPositions, prices, os.Getenv("HYPERLIQUID_ACCOUNT_ADDRESS"), notifier, cfg.NotifyTPSLFillsEnabled())
					if len(orphanCloseJobs) > 0 {
						runRegimeDirectionOrphanCloses(
							shutdownSideEffectCtx,
							state,
							cfg.Strategies,
							orphanCloseJobs,
							hlPositions,
							defaultHyperliquidLiveCloser,
							&mu,
							notifier.SendOwnerDM,
						)
					}
					if len(fillHints) > 0 {
						if b, err := json.Marshal(fillHints); err == nil {
							hlReconcileFillHintsJSON = b
						} else {
							fmt.Fprintf(os.Stderr, "[WARN] hl-sync: json.Marshal(fillHints): %v\n", err)
						}
					}
					reportHLReconcileGaps(notifier, collectHLReconcileGapResults(state, &mu))
				}

				auditRes := runHyperliquidLiquidationAudit(cfg.Strategies, state, hlLiquidationPx, hlNetSideByCoin, hlOnChainAbsQty, hlStateFetched, &mu, notifier, time.Now().UTC())
				if hlStateFetched {
					lastLiquidationAudit = time.Now().UTC()
				}
				if auditRes.ImmediateFills > 0 {
					fmt.Printf("[WARN] #1450 liquidation audit: %d position(s) exited on a clamped stop this cycle\n", auditRes.ImmediateFills)
					for _, cd := range auditRes.CloseDetails {
						if chKey := notifier.resolveChannelKey(cd.SC.Platform, cd.SC.Type, isLiveArgs(cd.SC.Args)); chKey != "" {
							channelTrades[chKey]++
							channelTradeDetails[chKey+"|"+extractAsset(cd.SC)] = append(channelTradeDetails[chKey+"|"+extractAsset(cd.SC)], cd.Detail)
						}
					}
					sendAuditCloseAlerts(auditRes.CloseDetails, state.Strategies, &mu, notifier)
					totalTrades += len(auditRes.CloseDetails)
					if n := convergeHedgesAfterAuditClose(auditRes.CloseDetails, state.Strategies, &mu, prices, notifier, logMgr.GetStrategyLogger); n > 0 {
						fmt.Printf("[WARN] #1450 liquidation audit: converged %d hedge leg(s) post-close\n", n)
					}
				}
			}

			regimeStoreReady()
			processRegimeTransitionAlerts(store.primary(), globalRegimeStore, cfg.Regime, notifier, time.Now().UTC())
			dueUnlatched := dueStrategiesPersistable(store, dueStrategiesNotLatched(dueStrategies, scopeRisk))
			hlBatchResults := runHyperliquidBatchPrePass(dueUnlatched, state, &mu, cfg, prices, notifier, func(format string, a ...any) {
				fmt.Printf(format+"\n", a...)
			})
			for _, sc := range dueUnlatched {
				stratState := state.Strategies[sc.ID]
				if stratState == nil {
					continue
				}
				stratDB, stratDBErr := store.dbForStrategy(sc.ID)
				if stratDBErr != nil {
					fmt.Fprintf(os.Stderr, "[storage] CRITICAL: %v — skipping %s this cycle\n", stratDBErr, sc.ID)
					continue
				}
				stratScope := portfolioScopeFor(sc)
				persistenceHold := store.persistenceHoldsScope(stratScope)
				sr := scopeRisk[portfolioScopeFor(sc)]
				if sr == nil {
					sr = &scopeCycleRisk{Scope: portfolioScopeFor(sc), Config: scopeRiskConfig(cfg, portfolioScopeFor(sc))}
				}

				logger, err := logMgr.GetStrategyLogger(sc.ID)
				if err != nil {
					fmt.Printf("[ERROR] Logger for %s: %v\n", sc.ID, err)
					continue
				}

				hlLiveStrategy := sc.Type == "perps" && sc.Platform == "hyperliquid" && hyperliquidIsLive(sc.Args)
				okxLiveStrategy := sc.Platform == "okx" && okxIsLive(sc.Args)
				rhLiveStrategy := sc.Type == "spot" && sc.Platform == "robinhood" && robinhoodIsLive(sc.Args)
				tsLiveStrategy := sc.Type == "futures" && sc.Platform == "topstep" && topstepIsLive(sc.Args)

				mu.RLock()
				pv := PortfolioValue(stratState, prices)
				var posJSON string
				if sc.Type == "options" {
					posJSON = EncodeAllPositionsJSON(stratState.OptionPositions, stratState.Positions)
				}
				var spotPosCtx PositionCtx
				if sc.Type == "spot" && sc.Platform != "okx" && sc.Platform != "robinhood" {
					if sym := spotSymbol(sc.Args); sym != "" {
						spotPosCtx = positionCtxForSymbol(stratState, sym, sc, cfg.Regime)
					}
				}
				var hlCash float64
				var hlPosQty float64
				var hlPosSide string
				var hlAvgCost float64
				var hlPosLeverage float64
				var hlEntryATR float64
				var hlPosCtx PositionCtx
				var hlStopLossOID int64
				var hlTPOIDs []int64
				var hlStopLossTriggerPx float64
				var hlStopLossHighWaterPx float64
				var hlPosSnapshot *Position
				var hlScaleInCount int
				var hlLastAddPrice float64
				var hlAddedNotionalUSD float64
				var hlScaleInCash float64
				var hlScaleInResizePending bool
				var hlPoolBalanceKnown bool
				var hlProfileState *RegimeProfileState
				if sc.Type == "perps" && sc.Platform == "hyperliquid" {
					if hlLiveStrategy {
						if poolCash, pooled, balanceKnown := sharedWalletPoolAvailableMargin(sc, cfg.Strategies, state, prices, sharedWallets, walletBalances); pooled {
							hlCash = poolCash
							hlPoolBalanceKnown = balanceKnown
						} else {
							hlCash = stratState.Cash
						}
					}
					if stratState.RegimeProfile != nil {
						cp := *stratState.RegimeProfile
						hlProfileState = &cp
					}
					hlScaleInCash = stratState.Cash
					if hlLiveStrategy {
						if poolCash, pooled, _ := sharedWalletPoolAvailableMargin(sc, cfg.Strategies, state, prices, sharedWallets, walletBalances); pooled {
							hlScaleInCash = poolCash
						}
					}
					if sym := hyperliquidSymbol(sc.Args); sym != "" {
						if pos, ok := stratState.Positions[sym]; ok {
							hlPosCtx = positionCtxForCheck(sc, pos, cfg.Regime)
							hlPosSide = hlPosCtx.Side
							hlPosQty = hlPosCtx.Quantity
							hlAvgCost = hlPosCtx.AvgCost
							hlPosLeverage = pos.Leverage
							hlEntryATR = pos.EntryATR
							hlStopLossOID = pos.StopLossOID
							hlTPOIDs = cloneInt64s(pos.TPOIDs)
							hlStopLossTriggerPx = pos.StopLossTriggerPx
							hlStopLossHighWaterPx = pos.StopLossHighWaterPx
							hlPosSnapshot = hyperliquidProtectionPositionSnapshot(pos)
							hlScaleInCount = pos.ScaleInCount
							hlLastAddPrice = pos.LastAddPrice
							hlAddedNotionalUSD = pos.AddedNotionalUSD
							hlScaleInResizePending = pos.ScaleInResizePending
						}
					}
				}
				var okxCash float64
				var okxCashReconcile bool
				var okxPosQty float64
				var okxPosSide string
				var okxAvgCost float64
				var okxPosLeverage float64
				var okxPoolBalanceKnown bool
				var okxPosCtx PositionCtx
				if sc.Platform == "okx" {
					if okxLiveStrategy {
						if sc.Type == "perps" {
							if poolCash, pooled, balanceKnown := sharedWalletPoolAvailableMargin(sc, cfg.Strategies, state, prices, sharedWallets, walletBalances); pooled {
								okxCash = poolCash
								okxPoolBalanceKnown = balanceKnown
							} else {
								okxCash = stratState.Cash
							}
						} else {
							okxCash = stratState.Cash
						}
						okxCashReconcile = stratState.CashReconcileRequired
					}
					if sym := okxSymbol(sc.Args); sym != "" {
						if pos, ok := stratState.Positions[sym]; ok {
							okxPosCtx = positionCtxForCheck(sc, pos, cfg.Regime)
							okxPosSide = okxPosCtx.Side
							okxPosQty = okxPosCtx.Quantity
							okxAvgCost = okxPosCtx.AvgCost
							okxPosLeverage = pos.Leverage
						}
					}
				}
				var rhCash float64
				var rhCashReconcile bool
				var rhPosQty float64
				var rhPosSide string
				var rhPosCtx PositionCtx
				if sc.Platform == "robinhood" {
					if rhLiveStrategy {
						rhCash = stratState.Cash
						rhCashReconcile = stratState.CashReconcileRequired
					}
					if sym := robinhoodSymbol(sc.Args); sym != "" {
						if pos, ok := stratState.Positions[sym]; ok {
							rhPosCtx = positionCtxForCheck(sc, pos, cfg.Regime)
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
							tsPosCtx = positionCtxForCheck(sc, pos, cfg.Regime)
							tsPosSide = tsPosCtx.Side
							tsContracts = tsPosCtx.Quantity
						}
					}
				}
				mu.RUnlock()

				var riskAssist *PlatformRiskAssist
				needHL := len(hlLiveAll) > 0
				needOKX := okxStateFetched && len(okxLivePerps) > 0
				needTS := tsStateFetched && len(tsLiveAll) > 0
				if needHL || needOKX || needTS {
					riskAssist = &PlatformRiskAssist{}
					if needHL {
						riskAssist.HLLiveAll = hlLiveAll
						if hlStateFetched {
							riskAssist.HLPositions = hlPositions
						}
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
				cbSnapshot := perStrategyCircuitBreakerSnapshot{}
				if !allowed && isFreshPerStrategyCircuitBreaker(reason) {
					cbSnapshot = snapshotPerStrategyCircuitBreaker(stratState, prices)
				}
				mu.Unlock()
				for _, cbAlert := range drainCircuitBreakerSuppressionAlerts() {
					if notifier.HasOwner() {
						notifier.SendOwnerDM(formatCircuitBreakerSuppressionDM(cbAlert))
					}
				}
				for _, moAlert := range drainModelOnlyCloseAlerts() {
					if notifier.HasOwner() {
						notifier.SendOwnerDM(formatModelOnlyCloseDM(moAlert))
					}
				}
				cbManageOnly := false
				if !allowed {
					notifyPerStrategyCircuitBreakerWithSnapshot(sc, cbSnapshot, reason, pv, sr.TotalPV, stratDB, notifier, sr.KillSwitchFired)
					logger.Warn("Risk block: %s (portfolio=$%.2f)", reason, pv)
					if circuitBreakerPermitsManagement(reason, sc.Platform, sc.Type, hlPosQty) {
						cbManageOnly = true
						logger.Info("Circuit breaker latched — suppressing new entries but continuing trailing-SL/TP management for open position (#1046)")
					} else {
						logger.Close()
						lastRun[sc.ID] = time.Now()
						continue
					}
				}

				if notionalCapSkipsStrategyCycle(sr.NotionalBlocked) {
					logger.Warn("Notional cap exceeded — skipping strategy cycle")
					logger.Close()
					lastRun[sc.ID] = time.Now()
					continue
				}

				trades := 0
				var detail string
				switch sc.Type {
				case "spot":
					if sc.Platform == "okx" {
						if result, signalStr, price, ok := runOKXCheck(sc, prices, okxPosCtx, cfg.Regime, resolveATRMethod(sc, cfg), notifier, logger); ok {
							prices[result.Symbol] = price
							storeRegime := globalRegimeStore.PayloadForStrategy(sc, cfg.Regime)
							result.Regime = &storeRegime
							if gateRegime, regimeBlocked := applyRegimeGate(sc, storeRegime, cfg.Regime, okxPosQty); regimeBlocked {
								logger.Info("Regime gate: open signal blocked (%s)", regimeGateBlockDetail(gateRegime))
								result.Signal = 0
							}
							hurstDecision := advanceHurstGate(sc, storeRegime, cfg.Regime, stratState, &mu, okxPosQty)
							if hurstDecision.Holds && pausedBlocksSignal(result.Signal, result.CloseFraction, okxPosQty, okxPosSide, true, false) {
								logger.Info("Hurst gate: %s signal suppressed — %s (#1411)", signalStr, hurstDecision.Detail)
								result.Signal = 0
							}
							if sc.Paused && pausedBlocksSignal(result.Signal, result.CloseFraction, okxPosQty, okxPosSide, true, false) {
								logger.Info("Paused: %s signal suppressed — position-increasing actions held while paused (#1150)", signalStr)
								result.Signal = 0
							}
							if sr.DailyLossEntriesHeld && pausedBlocksSignal(result.Signal, result.CloseFraction, okxPosQty, okxPosSide, true, false) {
								logger.Info("Daily loss limit: %s signal suppressed — entries held until UTC rollover (#1269)", signalStr)
								result.Signal = 0
							}
							if sr.NotionalBlocked && pausedBlocksSignal(result.Signal, result.CloseFraction, okxPosQty, okxPosSide, true, false) {
								logger.Warn("Notional cap: %s signal suppressed — new opens blocked, exits continue (#1344)", signalStr)
								result.Signal = 0
							}
							if persistenceHold && pausedBlocksSignal(result.Signal, result.CloseFraction, okxPosQty, okxPosSide, true, false) {
								logger.Warn("Persistence hold: %s signal suppressed — the %s scope has an unacknowledged state save failure, so no new exposure is taken; exits, stops, ratchet and protection continue (#1523)", signalStr, scopeLabel(stratScope))
								result.Signal = 0
							}
							if capBlocked, capWhy := exposureCapBlocksSignal(sr.ExposureCapStatus, extractAsset(sc), result.Signal, result.CloseFraction, okxPosQty, okxPosSide, true, false); capBlocked {
								logger.Warn("Exposure cap: %s signal suppressed — %s (#1270)", signalStr, capWhy)
								result.Signal = 0
							}
							mu.Lock()
							syncStrategyRegimeState(stratState, storeRegime, cfg.Regime)
							mu.Unlock()
							var execResult *OKXExecuteResult
							liveExecFailed := false
							if okxIsLive(sc.Args) && result.Signal != 0 {
								if er, ok2 := runOKXExecuteOrder(sc, result, price, okxCash, okxPoolBalanceKnown, okxCashReconcile, okxPosQty, okxPosSide, okxAvgCost, okxPosLeverage, hurstDecision, notifier, logger); ok2 {
									execResult = er
								} else {
									liveExecFailed = true
								}
							}
							if !liveExecFailed {
								var cashAlert string
								mu.Lock()
								trades, detail, cashAlert = executeOKXResult(sc, stratState, stratDB, result, execResult, signalStr, price, cfg.Regime, cfg, hurstDecision, logger)
								mu.Unlock()
								if cashAlert != "" {
									notifySpotLiveCashOverBudget(notifier, cashAlert)
									mu.RLock()
									ids, _ := collectCashReconcileRequiredSnapshots(state)
									mu.RUnlock()
									globalSpotCashReconcileReminder.MarkNotified(strings.Join(ids, ","), time.Now().UTC())
								}
							}
						}
					} else if sc.Platform == "robinhood" {
						if result, signalStr, price, ok := runRobinhoodCheck(sc, prices, rhPosCtx, cfg.Regime, resolveATRMethod(sc, cfg), notifier, logger); ok {
							prices[result.Symbol] = price
							storeRegime := globalRegimeStore.PayloadForStrategy(sc, cfg.Regime)
							result.Regime = &storeRegime
							if gateRegime, regimeBlocked := applyRegimeGate(sc, storeRegime, cfg.Regime, rhPosQty); regimeBlocked {
								logger.Info("Regime gate: open signal blocked (%s)", regimeGateBlockDetail(gateRegime))
								result.Signal = 0
							}
							hurstDecision := advanceHurstGate(sc, storeRegime, cfg.Regime, stratState, &mu, rhPosQty)
							if hurstDecision.Holds && pausedBlocksSignal(result.Signal, result.CloseFraction, rhPosQty, rhPosSide, true, false) {
								logger.Info("Hurst gate: %s signal suppressed — %s (#1411)", signalStr, hurstDecision.Detail)
								result.Signal = 0
							}
							if sc.Paused && pausedBlocksSignal(result.Signal, result.CloseFraction, rhPosQty, rhPosSide, true, false) {
								logger.Info("Paused: %s signal suppressed — position-increasing actions held while paused (#1150)", signalStr)
								result.Signal = 0
							}
							if sr.DailyLossEntriesHeld && pausedBlocksSignal(result.Signal, result.CloseFraction, rhPosQty, rhPosSide, true, false) {
								logger.Info("Daily loss limit: %s signal suppressed — entries held until UTC rollover (#1269)", signalStr)
								result.Signal = 0
							}
							if sr.NotionalBlocked && pausedBlocksSignal(result.Signal, result.CloseFraction, rhPosQty, rhPosSide, true, false) {
								logger.Warn("Notional cap: %s signal suppressed — new opens blocked, exits continue (#1344)", signalStr)
								result.Signal = 0
							}
							if persistenceHold && pausedBlocksSignal(result.Signal, result.CloseFraction, rhPosQty, rhPosSide, true, false) {
								logger.Warn("Persistence hold: %s signal suppressed — the %s scope has an unacknowledged state save failure, so no new exposure is taken; exits, stops, ratchet and protection continue (#1523)", signalStr, scopeLabel(stratScope))
								result.Signal = 0
							}
							if capBlocked, capWhy := exposureCapBlocksSignal(sr.ExposureCapStatus, extractAsset(sc), result.Signal, result.CloseFraction, rhPosQty, rhPosSide, true, false); capBlocked {
								logger.Warn("Exposure cap: %s signal suppressed — %s (#1270)", signalStr, capWhy)
								result.Signal = 0
							}
							mu.Lock()
							syncStrategyRegimeState(stratState, storeRegime, cfg.Regime)
							mu.Unlock()
							var execResult *RobinhoodExecuteResult
							liveExecFailed := false
							if robinhoodIsLive(sc.Args) && result.Signal != 0 {
								if er, ok2 := runRobinhoodExecuteOrder(sc, result, price, rhCash, rhCashReconcile, rhPosQty, rhPosSide, hurstDecision, notifier, logger); ok2 {
									execResult = er
								} else {
									liveExecFailed = true
								}
							}
							if !liveExecFailed {
								var cashAlert string
								mu.Lock()
								trades, detail, cashAlert = executeRobinhoodResult(sc, stratState, stratDB, result, execResult, signalStr, price, cfg.Regime, cfg, hurstDecision, logger)
								mu.Unlock()
								if cashAlert != "" {
									notifySpotLiveCashOverBudget(notifier, cashAlert)
									mu.RLock()
									ids, _ := collectCashReconcileRequiredSnapshots(state)
									mu.RUnlock()
									globalSpotCashReconcileReminder.MarkNotified(strings.Join(ids, ","), time.Now().UTC())
								}
							}
						}
					} else if result, signalStr, price, ok := runSpotCheck(sc, prices, spotPosCtx, cfg.Regime, resolveATRMethod(sc, cfg), notifier, logger); ok {
						storeRegime := globalRegimeStore.PayloadForStrategy(sc, cfg.Regime)
						result.Regime = &storeRegime
						if gateRegime, regimeBlocked := applyRegimeGate(sc, storeRegime, cfg.Regime, spotPosCtx.Quantity); regimeBlocked {
							logger.Info("Regime gate: open signal blocked (%s)", regimeGateBlockDetail(gateRegime))
							result.Signal = 0
						}
						hurstDecision := advanceHurstGate(sc, storeRegime, cfg.Regime, stratState, &mu, spotPosCtx.Quantity)
						if hurstDecision.Holds && pausedBlocksSignal(result.Signal, result.CloseFraction, spotPosCtx.Quantity, spotPosCtx.Side, true, false) {
							logger.Info("Hurst gate: %s signal suppressed — %s (#1411)", signalStr, hurstDecision.Detail)
							result.Signal = 0
						}
						if sc.Paused && pausedBlocksSignal(result.Signal, result.CloseFraction, spotPosCtx.Quantity, spotPosCtx.Side, true, false) {
							logger.Info("Paused: %s signal suppressed — position-increasing actions held while paused (#1150)", signalStr)
							result.Signal = 0
						}
						if sr.DailyLossEntriesHeld && pausedBlocksSignal(result.Signal, result.CloseFraction, spotPosCtx.Quantity, spotPosCtx.Side, true, false) {
							logger.Info("Daily loss limit: %s signal suppressed — entries held until UTC rollover (#1269)", signalStr)
							result.Signal = 0
						}
						if sr.NotionalBlocked && pausedBlocksSignal(result.Signal, result.CloseFraction, spotPosCtx.Quantity, spotPosCtx.Side, true, false) {
							logger.Warn("Notional cap: %s signal suppressed — new opens blocked, exits continue (#1344)", signalStr)
							result.Signal = 0
						}
						if persistenceHold && pausedBlocksSignal(result.Signal, result.CloseFraction, spotPosCtx.Quantity, spotPosCtx.Side, true, false) {
							logger.Warn("Persistence hold: %s signal suppressed — the %s scope has an unacknowledged state save failure, so no new exposure is taken; exits, stops, ratchet and protection continue (#1523)", signalStr, scopeLabel(stratScope))
							result.Signal = 0
						}
						if capBlocked, capWhy := exposureCapBlocksSignal(sr.ExposureCapStatus, extractAsset(sc), result.Signal, result.CloseFraction, spotPosCtx.Quantity, spotPosCtx.Side, true, false); capBlocked {
							logger.Warn("Exposure cap: %s signal suppressed — %s (#1270)", signalStr, capWhy)
							result.Signal = 0
						}
						mu.Lock()
						syncStrategyRegimeState(stratState, storeRegime, cfg.Regime)
						trades, detail = executeSpotResult(sc, stratState, stratDB, result, signalStr, price, cfg.Regime, cfg, hurstDecision, logger)
						mu.Unlock()
					}
				case "options":
					if result, signalStr, ok := runOptionsCheck(sc, posJSON, notifier, logger); ok {
						if sc.Paused {
							kept, dropped := pausedOptionsActions(result.Actions)
							if dropped > 0 {
								logger.Info("Paused: %d option open action(s) dropped — close actions still execute (#1150)", dropped)
							}
							result.Actions = kept
						}
						if sr.DailyLossEntriesHeld {
							kept, dropped := pausedOptionsActions(result.Actions)
							if dropped > 0 {
								logger.Info("Daily loss limit: %d option open action(s) dropped — entries held until UTC rollover (#1269)", dropped)
							}
							result.Actions = kept
						}
						if sr.NotionalBlocked {
							kept, dropped := pausedOptionsActions(result.Actions)
							if dropped > 0 {
								logger.Warn("Notional cap: %d option open action(s) dropped — new opens blocked, exits continue (#1344)", dropped)
							}
							result.Actions = kept
						}
						if persistenceHold {
							kept, dropped := pausedOptionsActions(result.Actions)
							if dropped > 0 {
								logger.Warn("Persistence hold: %d option open action(s) dropped — the %s scope has an unacknowledged state save failure, so no new exposure is taken; closes continue (#1523)", dropped, scopeLabel(stratScope))
							}
							result.Actions = kept
						}
						if kept, dropped, capWhy := exposureCapOptionsActions(sr.ExposureCapStatus, extractAsset(sc), result.Actions); dropped > 0 {
							logger.Warn("Exposure cap: %d option open action(s) dropped — %s (#1270)", dropped, capWhy)
							result.Actions = kept
						}
						optionsRegime := globalRegimeStore.PayloadForStrategy(sc, cfg.Regime)
						mu.Lock()
						stratState.Regime = optionsRegime.PrimaryLabel(nil)
						var harvestDetails []string
						trades, detail, harvestDetails = executeOptionsResult(sc, stratState, result, signalStr, logger)
						mu.Unlock()
						if chKey := notifier.resolveChannelKey(sc.Platform, sc.Type, isLiveArgs(sc.Args)); chKey != "" {
							key := chKey + "|" + extractAsset(sc)
							channelTradeDetails[key] = append(channelTradeDetails[key], harvestDetails...)
						}
					}
				case "perps":
					var hlProfileNext RegimeProfileState
					var hlProfileActive string
					hlProfileResolved := false
					if sc.Platform == "hyperliquid" && sc.RegimeProfileAllocation.IsConfigured() {
						palPayload := globalRegimeStore.PayloadForStrategy(sc, cfg.Regime)
						palBarTime := globalRegimeStore.BarTimeForStrategy(sc, cfg.Regime)
						palLabel := palPayload.Label(sc.RegimeProfileAllocation.Window, cfg.Regime)
						hlProfileActive, hlProfileNext = resolveRegimeProfile(sc.RegimeProfileAllocation, palLabel, palBarTime, hlProfileState, hlPosQty, hlPosCtx.Profile)
						applyRegimeProfileParams(&sc, sc.RegimeProfileAllocation, hlProfileActive)
						hlProfileResolved = true
						logger.Info("Regime profile: window=%s label=%s active=%q (pending=%q seen=%d)",
							sc.RegimeProfileAllocation.Window, palLabel, hlProfileActive, hlProfileNext.PendingProfile, hlProfileNext.PendingBarsSeen)
					}
					if sc.Platform == "okx" {
						if result, signalStr, price, ok := runOKXCheck(sc, prices, okxPosCtx, cfg.Regime, resolveATRMethod(sc, cfg), notifier, logger); ok {
							prices[result.Symbol] = price
							storeRegime := globalRegimeStore.PayloadForStrategy(sc, cfg.Regime)
							result.Regime = &storeRegime
							if gateRegime, regimeBlocked := applyRegimeGate(sc, storeRegime, cfg.Regime, okxPosQty); regimeBlocked {
								logger.Info("Regime gate: open signal blocked (%s)", regimeGateBlockDetail(gateRegime))
								result.Signal = 0
							}
							hurstDecision := advanceHurstGate(sc, storeRegime, cfg.Regime, stratState, &mu, okxPosQty)
							if hurstDecision.Holds && pausedBlocksSignal(result.Signal, result.CloseFraction, okxPosQty, okxPosSide, PerpsAllowsLong(sc), PerpsAllowsShort(sc)) {
								logger.Info("Hurst gate: %s signal suppressed — %s (#1411)", signalStr, hurstDecision.Detail)
								result.Signal = 0
							}
							if sc.Paused && pausedBlocksSignal(result.Signal, result.CloseFraction, okxPosQty, okxPosSide, PerpsAllowsLong(sc), PerpsAllowsShort(sc)) {
								logger.Info("Paused: %s signal suppressed — position-increasing actions held while paused (#1150)", signalStr)
								result.Signal = 0
							}
							if sr.DailyLossEntriesHeld && pausedBlocksSignal(result.Signal, result.CloseFraction, okxPosQty, okxPosSide, PerpsAllowsLong(sc), PerpsAllowsShort(sc)) {
								logger.Info("Daily loss limit: %s signal suppressed — entries held until UTC rollover (#1269)", signalStr)
								result.Signal = 0
							}
							if sr.NotionalBlocked && pausedBlocksSignal(result.Signal, result.CloseFraction, okxPosQty, okxPosSide, PerpsAllowsLong(sc), PerpsAllowsShort(sc)) {
								logger.Warn("Notional cap: %s signal suppressed — new opens blocked, exits continue (#1344)", signalStr)
								result.Signal = 0
							}
							if persistenceHold && pausedBlocksSignal(result.Signal, result.CloseFraction, okxPosQty, okxPosSide, PerpsAllowsLong(sc), PerpsAllowsShort(sc)) {
								logger.Warn("Persistence hold: %s signal suppressed — the %s scope has an unacknowledged state save failure, so no new exposure is taken; exits, stops, ratchet and protection continue (#1523)", signalStr, scopeLabel(stratScope))
								result.Signal = 0
							}
							if capBlocked, capWhy := exposureCapBlocksSignal(sr.ExposureCapStatus, extractAsset(sc), result.Signal, result.CloseFraction, okxPosQty, okxPosSide, PerpsAllowsLong(sc), PerpsAllowsShort(sc)); capBlocked {
								logger.Warn("Exposure cap: %s signal suppressed — %s (#1270)", signalStr, capWhy)
								result.Signal = 0
							}
							mu.Lock()
							syncStrategyRegimeState(stratState, storeRegime, cfg.Regime)
							mu.Unlock()
							var execResult *OKXExecuteResult
							liveExecFailed := false
							if okxIsLive(sc.Args) && result.Signal != 0 {
								if er, ok2 := runOKXExecuteOrder(sc, result, price, okxCash, okxPoolBalanceKnown, okxCashReconcile, okxPosQty, okxPosSide, okxAvgCost, okxPosLeverage, hurstDecision, notifier, logger); ok2 {
									execResult = er
								} else {
									liveExecFailed = true
								}
							}
							if !liveExecFailed {
								var cashAlert string
								mu.Lock()
								trades, detail, cashAlert = executeOKXResult(sc, stratState, stratDB, result, execResult, signalStr, price, cfg.Regime, cfg, hurstDecision, logger)
								mu.Unlock()
								if cashAlert != "" {
									notifySpotLiveCashOverBudget(notifier, cashAlert)
									mu.RLock()
									ids, _ := collectCashReconcileRequiredSnapshots(state)
									mu.RUnlock()
									globalSpotCashReconcileReminder.MarkNotified(strings.Join(ids, ","), time.Now().UTC())
								}
							}
						}
					} else if result, signalStr, price, ok := runHyperliquidCheck(&sc, prices, hlPosCtx, cfg.Regime, resolveATRMethod(sc, cfg), notifier, logger, hlBatchResults); ok {
						prices[result.Symbol] = price
						if cbManageOnly {
							result.Signal = 0
						}
						storeRegime := globalRegimeStore.PayloadForStrategy(sc, cfg.Regime)
						result.Regime = &storeRegime
						if gateRegime, regimeBlocked := applyRegimeGate(sc, storeRegime, cfg.Regime, hlPosQty); regimeBlocked {
							logger.Info("Regime gate: open signal blocked (%s)", regimeGateBlockDetail(gateRegime))
							result.Signal = 0
						}
						hurstDecision := advanceHurstGate(sc, storeRegime, cfg.Regime, stratState, &mu, hlPosQty)
						if hurstDecision.Holds && pausedBlocksSignal(result.Signal, result.CloseFraction, hlPosQty, hlPosSide, PerpsAllowsLong(sc), PerpsAllowsShort(sc)) {
							logger.Info("Hurst gate: %s signal suppressed — %s (#1411)", signalStr, hurstDecision.Detail)
							result.Signal = 0
						}
						if sc.Paused && pausedBlocksSignal(result.Signal, result.CloseFraction, hlPosQty, hlPosSide, PerpsAllowsLong(sc), PerpsAllowsShort(sc)) {
							logger.Info("Paused: %s signal suppressed — position-increasing actions held while paused (#1150)", signalStr)
							result.Signal = 0
						}
						if sr.DailyLossEntriesHeld && pausedBlocksSignal(result.Signal, result.CloseFraction, hlPosQty, hlPosSide, PerpsAllowsLong(sc), PerpsAllowsShort(sc)) {
							logger.Info("Daily loss limit: %s signal suppressed — entries held until UTC rollover (#1269)", signalStr)
							result.Signal = 0
						}
						if sr.NotionalBlocked && pausedBlocksSignal(result.Signal, result.CloseFraction, hlPosQty, hlPosSide, PerpsAllowsLong(sc), PerpsAllowsShort(sc)) {
							logger.Warn("Notional cap: %s signal suppressed — new opens blocked, exits continue (#1344)", signalStr)
							result.Signal = 0
						}
						if persistenceHold && pausedBlocksSignal(result.Signal, result.CloseFraction, hlPosQty, hlPosSide, PerpsAllowsLong(sc), PerpsAllowsShort(sc)) {
							logger.Warn("Persistence hold: %s signal suppressed — the %s scope has an unacknowledged state save failure, so no new exposure is taken; exits, stops, ratchet and protection continue (#1523)", signalStr, scopeLabel(stratScope))
							result.Signal = 0
						}
						if capBlocked, capWhy := exposureCapBlocksSignal(sr.ExposureCapStatus, extractAsset(sc), result.Signal, result.CloseFraction, hlPosQty, hlPosSide, PerpsAllowsLong(sc), PerpsAllowsShort(sc)); capBlocked {
							logger.Warn("Exposure cap: %s signal suppressed — %s (#1270)", signalStr, capWhy)
							result.Signal = 0
						}
						if replayMirrorPaperActive(sc) && pausedBlocksSignal(result.Signal, result.CloseFraction, hlPosQty, hlPosSide, PerpsAllowsLong(sc), PerpsAllowsShort(sc)) {
							logger.Info("Replay mirror: %s signal suppressed — entries mirror the live decision log (#1431)", signalStr)
							result.Signal = 0
						}
						mu.Lock()
						syncStrategyRegimeState(stratState, storeRegime, cfg.Regime)
						updateStrategyDivergenceState(stratState, result.Divergence)
						mu.Unlock()
						var execResult *HyperliquidExecuteResult
						liveExecFailed := false
						hedgeFreshExposureQty := 0.0
						manageRatchetTightened := false
						if result.Signal == 0 && hlPosQty > 0 && strategyUsesTrailingTPRatchetClose(sc) {
							ratchetAlert := applyTrailingTPRatchet(sc, stratState, result.Symbol, price, &mu, logger)
							notifyRatchetTrigger(notifier, sc.NotifyRatchetTriggersEnabled(cfg), ratchetAlert)
							manageRatchetTightened = ratchetAlert != nil
							mu.RLock()
							if pos, ok3 := stratState.Positions[result.Symbol]; ok3 && pos != nil {
								hlPosSnapshot = hyperliquidProtectionPositionSnapshot(pos)
							}
							mu.RUnlock()
						}
						if result.Signal == 0 && hlPosQty > 0 && atrMultMissingEntryATR(sc, hlPosSnapshot) {
							notifyATRMultMissingEntryATROnce(sc, result.Symbol, notifier, logger)
						}
						if result.Signal == 0 && hlPosQty > 0 && tieredTPATRMissingEntryATR(sc, hlPosSnapshot) {
							notifyTieredTPATRMissingEntryATROnce(sc, result.Symbol, notifier, logger)
						}
						if !hyperliquidIsLive(sc.Args) && result.Signal == 0 && hlPosQty > 0 && effectiveTrailingStopPct(sc, hlPosSnapshot) > 0 {
							newHighWater, newTrigger, breach, breachPx := runHyperliquidTrailingStopPaper(sc, hlPosSide, hlPosSnapshot, price, hlStopLossHighWaterPx, hlStopLossTriggerPx, trailingReplacePolicy{ratchetTightened: manageRatchetTightened})
							mu.Lock()
							if pos, ok3 := stratState.Positions[result.Symbol]; ok3 && pos.Quantity > 0 && pos.Side == hlPosSide {
								if breach {
									if recordPerpsStopLossClose(stratState, result.Symbol, breachPx, "trailing_stop_loss_paper", logger) {
										trades++
										detail = fmt.Sprintf("[%s] PAPER TRAILING SL %s @ $%.2f", sc.ID, result.Symbol, breachPx)
									}
								} else {
									if newHighWater > 0 {
										pos.StopLossHighWaterPx = newHighWater
									}
									if newTrigger > 0 {
										pos.StopLossTriggerPx = newTrigger
										pos.RatchetFallbackNormalizePending = false
										logger.Info("Paper trailing SL trigger updated @ $%.4f (high_water=$%.4f)", newTrigger, newHighWater)
									}
								}
							}
							mu.Unlock()
						}
						if hyperliquidIsLive(sc.Args) && result.Signal == 0 && hlPosQty > 0 && effectiveTrailingStopPct(sc, hlPosSnapshot) > 0 {
							slEffectiveQty, capped := hlSLEffectiveQty(result.Symbol, hlPosQty, hlOnChainAbsQty)
							if capped {
								logger.Warn("trailing SL arm: virtual qty %.6f > on-chain %.6f for %s; capping SL size to on-chain qty (#621)", hlPosQty, slEffectiveQty, result.Symbol)
							}
							forceResize := hlScaleInResizePending && !capped
							newHighWater, slUpdate, updateConfirmed := runHyperliquidTrailingStopUpdate(sc, result.Symbol, hlPosSide, slEffectiveQty, hlPosSnapshot, price, hlStopLossHighWaterPx, hlStopLossTriggerPx, hlStopLossOID, trailingReplacePolicy{forceResize: forceResize, ratchetTightened: manageRatchetTightened, liquidationPx: hlLiquidationPxForSide(hlLiquidationPx, hlNetSideByCoin, result.Symbol, hlPosSide)}, notifier, logger)
							mu.Lock()
							if immediateFill, fillPx := applyTrailingStopUpdateResult(stratState, result.Symbol, hlPosSide, hlStopLossOID, newHighWater, updateConfirmed, slUpdate, "trailing_stop_loss_immediate", logger, 0); immediateFill {
								trades++
								detail = fmt.Sprintf("[%s] LIVE TRAILING SL %s @ $%.2f", sc.ID, result.Symbol, fillPx)
							}
							if forceResize && updateConfirmed {
								if p, ok := stratState.Positions[result.Symbol]; ok && p != nil {
									p.ScaleInResizePending = false
								}
							}
							mu.Unlock()
						}
						if !hyperliquidIsLive(sc.Args) && result.Signal == 0 && hlPosQty > 0 && sc.StopLossATRMult != nil && *sc.StopLossATRMult > 0 {
							newTrigger, breach, breachPx := runHyperliquidFixedATRStopLossPaper(sc, hlPosSide, hlPosSnapshot, price, hlStopLossTriggerPx)
							mu.Lock()
							if pos, ok3 := stratState.Positions[result.Symbol]; ok3 && pos.Quantity > 0 && pos.Side == hlPosSide {
								if breach {
									if recordPerpsStopLossClose(stratState, result.Symbol, breachPx, "stop_loss_atr_paper", logger) {
										trades++
										detail = fmt.Sprintf("[%s] PAPER FIXED ATR SL %s @ $%.2f", sc.ID, result.Symbol, breachPx)
									}
								} else if newTrigger > 0 && pos.StopLossTriggerPx == 0 {
									pos.StopLossTriggerPx = newTrigger
									stampOpenTradeWithProtectionSnapshot(stratState, stratDB, sc, result.Symbol, pos)
									logger.Info("Paper fixed ATR SL armed @ $%.4f (%.2f%% from entry $%.4f)",
										newTrigger, effectiveFixedStopLossATRPct(sc, hlPosSnapshot), pos.AvgCost)
								}
							}
							mu.Unlock()
						}
						if hyperliquidIsLive(sc.Args) && result.Signal == 0 && hlPosQty > 0 && sc.StopLossATRMult != nil && *sc.StopLossATRMult > 0 && hlStopLossOID == 0 && hlStopLossTriggerPx == 0 {
							triggerPx := fixedStopLossATRTriggerPx(sc, hlPosSide, hlPosSnapshot)
							clampOffendingPx := 0.0
							clampedTriggerPx := 0.0
							clampArmAction := hlLiquidationActionRearmFailed
							armLiqPx := hlLiquidationPxForSide(hlLiquidationPx, hlNetSideByCoin, result.Symbol, hlPosSide)
							if clamped, wasClamped := clampStopInsideLiquidation(hlPosSide, triggerPx, armLiqPx); wasClamped {
								logger.Warn("#1450 fixed ATR SL for %s would rest past liquidation $%.4f; tightening $%.4f -> $%.4f",
									result.Symbol, armLiqPx, triggerPx, clamped)
								clampOffendingPx = triggerPx
								clampedTriggerPx = clamped
								triggerPx = clamped
							}
							if triggerPx > 0 {
								slEffectiveQty, capped := hlSLEffectiveQty(result.Symbol, hlPosQty, hlOnChainAbsQty)
								if capped {
									logger.Warn("fixed ATR SL arm: virtual qty %.6f > on-chain %.6f for %s; capping SL size to on-chain qty (#621)", hlPosQty, slEffectiveQty, result.Symbol)
								}
								slResult, ok2 := hyperliquidArmFixedATRStopLossLive(sc, result.Symbol, hlPosSide, slEffectiveQty, triggerPx, notifier, logger)
								clampArmAction = hlLiquidationArmClampAction(slResult, ok2)
								if ok2 && slResult != nil {
									mu.Lock()
									if pos, ok3 := stratState.Positions[result.Symbol]; ok3 && pos.Quantity > 0 && pos.Side == hlPosSide && pos.StopLossOID == 0 {
										if slResult.StopLossFilledImmediately && slResult.StopLossTriggerPx > 0 {
											if recordPerpsStopLossClose(stratState, result.Symbol, slResult.StopLossTriggerPx, "stop_loss_atr_immediate", logger) {
												trades++
												detail = fmt.Sprintf("[%s] LIVE FIXED ATR SL %s @ $%.2f", sc.ID, result.Symbol, slResult.StopLossTriggerPx)
											}
										} else if slResult.StopLossOID > 0 {
											pos.StopLossOID = slResult.StopLossOID
											pos.StopLossTriggerPx = slResult.StopLossTriggerPx
											stampOpenTradeWithProtectionSnapshot(stratState, stratDB, sc, result.Symbol, pos)
											logger.Info("Fixed ATR SL armed oid=%d @ $%.4f", slResult.StopLossOID, slResult.StopLossTriggerPx)
										} else if slResult.StopLossOutcomeUnknown && slResult.StopLossTriggerPx > 0 {
											pos.StopLossTriggerPx = slResult.StopLossTriggerPx
											stampOpenTradeWithProtectionSnapshot(stratState, stratDB, sc, result.Symbol, pos)
											logger.Warn("Fixed ATR SL outcome unreadable for %s: requested trigger $%.4f recorded (oid unknown)", result.Symbol, slResult.StopLossTriggerPx)
										}
									}
									mu.Unlock()
								}
							}
							if clampedTriggerPx > 0 {
								notifyHLStopPastLiquidation(sc, result.Symbol, hlPosSide, clampOffendingPx, clampedTriggerPx, armLiqPx, clampArmAction, notifier, logger, time.Now().UTC())
							}
						}
						if hyperliquidIsLive(sc.Args) && result.Signal == 0 && hlPosQty > 0 {
							if _, fillPx := runHyperliquidProtectionSync(sc, stratState, stratDB, result.Symbol, &mu, notifier, logger, "HL protection synced", hlReconcileFillHintsJSON, hlLiquidationPx, hlNetSideByCoin); fillPx > 0 {
								trades++
								detail = fmt.Sprintf("[%s] LIVE PROTECTION SYNC SL %s @ $%.2f", sc.ID, result.Symbol, fillPx)
							}
							runPostTPStopLossAdjustment(sc, stratState, result.Symbol, price, cfg, &mu, notifier, logger, hlOnChainAbsQty)
						}
						scaleInAddQty := 0.0
						if result.Signal != 0 && sc.Type == "perps" && sc.AllowScaleIn {
							defOpenNotional := PerpsOpenNotional(hlScaleInCash, EffectiveSizingLeverage(sc), EffectiveExchangeLeverage(sc), EffectiveMarginPerTradeUSD(sc))
							snap := scaleInSnapshot{Side: hlPosSide, Quantity: hlPosQty, AvgCost: hlAvgCost, EntryATR: hlEntryATR, ScaleInCount: hlScaleInCount, AddedNotionalUSD: hlAddedNotionalUSD, LastAddPrice: hlLastAddPrice}
							if q, okAdd, reason := perpsScaleInDecision(sc, snap, result.Signal, price, defOpenNotional); okAdd {
								scaleInAddQty = q * hurstDecision.OpenSizeMult()
							} else if reason != "" && reason != "not a same-direction add" {
								logger.Info("Scale-in not taken for %s: %s", result.Symbol, reason)
							}
						}
						if hyperliquidIsLive(sc.Args) && result.Signal != 0 {
							walletSnapshot := hlExecuteSnapshotForCoin(hlPositions, result.Symbol)
							if scaleInAddQty > 0 {
								if er, ok2 := runHyperliquidScaleInOrder(sc, result, scaleInAddQty, walletSnapshot, notifier, logger); ok2 {
									execResult = er
								} else {
									liveExecFailed = true
								}
							} else {
								er, ok2 := runHyperliquidExecuteOrder(sc, result, price, hlCash, hlPoolBalanceKnown, hlPosQty, hlPosSide, hlAvgCost, hlPosLeverage, hlStopLossOID, hlTPOIDs, hlReconcileAll, walletSnapshot, hurstDecision, notifier, logger)
								if ok2 {
									execResult = er
								} else {
									liveExecFailed = true
									canceledOIDs := append([]int64{hlStopLossOID}, hlTPOIDs...)
									canceledOIDs = hyperliquidExecuteSucceededCancelOIDs(er, canceledOIDs)
									if len(canceledOIDs) > 0 {
										sym := hyperliquidSymbol(sc.Args)
										if sym != "" {
											mu.Lock()
											if pos, ok3 := stratState.Positions[sym]; ok3 {
												clearHyperliquidProtectionOIDsMatching(pos, canceledOIDs)
												logger.Info("cleared canceled protection OIDs=%v after live execute failed", canceledOIDs)
											}
											mu.Unlock()
										}
									}
								}
							}
						}
						if !liveExecFailed {
							mu.Lock()
							var openTrade *Trade
							var ratchetAlert *RatchetTriggerAlert
							if scaleInAddQty > 0 {
								trades, detail, openTrade, ratchetAlert = executeHyperliquidScaleInDeferredOpen(sc, stratState, result, execResult, signalStr, price, scaleInAddQty, logger)
							} else {
								trades, detail, openTrade, ratchetAlert = executeHyperliquidResultDeferredOpen(sc, stratState, result, execResult, signalStr, price, cfg.Regime, cfg, hurstDecision, logger)
							}
							if openTrade != nil {
								var pos *Position
								if p, ok := stratState.Positions[result.Symbol]; ok {
									pos = p
								}
								recordPositionOpen(stratState, sc, openTrade, pos)
							}
							mu.Unlock()
							notifyRatchetTrigger(notifier, sc.NotifyRatchetTriggersEnabled(cfg), ratchetAlert)
							ratchetWalkerOwnedByScaleIn := scaleInAddQty > 0 && execResult != nil && trades > 0
							if ratchetAlert != nil && !ratchetWalkerOwnedByScaleIn {
								if extraTrades, slDetail := runTrailingStopUpdateAfterRatchetTighten(sc, stratState, result.Symbol, price, hlOnChainAbsQty, hlLiquidationPx, hlNetSideByCoin, &mu, notifier, logger); extraTrades > 0 {
									trades += extraTrades
									detail = slDetail
								}
							}
							if execResult != nil && trades > 0 {
								if _, fillPx := runHyperliquidProtectionSync(sc, stratState, stratDB, result.Symbol, &mu, notifier, logger, "HL protection synced after trade", hlReconcileFillHintsJSON, hlLiquidationPx, hlNetSideByCoin); fillPx > 0 {
									trades++
									detail = fmt.Sprintf("[%s] LIVE PROTECTION SYNC SL %s @ $%.2f", sc.ID, result.Symbol, fillPx)
								}
								runPostTPStopLossAdjustment(sc, stratState, result.Symbol, price, cfg, &mu, notifier, logger, hlOnChainAbsQty)
								if scaleInAddQty > 0 {
									filledAddQty := scaleInAddQty
									if execResult.Execution != nil && execResult.Execution.Fill != nil && execResult.Execution.Fill.TotalSz > 0 {
										filledAddQty = execResult.Execution.Fill.TotalSz
									}
									hedgeFreshExposureQty = filledAddQty
									if extraTrades, slDetail := scaleInResizeTrailingSLNow(sc, stratState, result.Symbol, price, hlOnChainAbsQty, hlLiquidationPx, hlNetSideByCoin, filledAddQty, ratchetAlert != nil, &mu, notifier, logger); extraTrades > 0 {
										trades += extraTrades
										detail = slDetail
									}
								} else {
									filledQty := 0.0
									if execResult.Execution != nil && execResult.Execution.Fill != nil && execResult.Execution.Fill.TotalSz > 0 {
										filledQty = execResult.Execution.Fill.TotalSz
									}
									if extraTrades, slDetail := armTrailingStopAtOpenNow(sc, stratState, result.Symbol, price, hlOnChainAbsQty, filledQty, &mu, notifier, logger); extraTrades > 0 {
										trades += extraTrades
										detail = slDetail
									}
								}
								if openTrade != nil {
									mu.Lock()
									if pos, okPos := stratState.Positions[result.Symbol]; okPos && pos != nil && pos.Quantity > 0 {
										stampOpenTradeWithProtectionSnapshot(stratState, stratDB, sc, result.Symbol, pos)
									}
									mu.Unlock()
								}
								if scaleInAddQty <= 0 && openTrade != nil && openTrade.Quantity > 0 {
									hedgeFreshExposureQty = openTrade.Quantity
								}
							}
						}
						if hlProfileResolved {
							mu.Lock()
							stampPositionProfileIfOpened(stratState, result.Symbol, hlProfileActive)
							updateStrategyProfileState(stratState, hlProfileNext)
							mu.Unlock()
						}
						if replaySourceID := replayMirrorSourceID(sc); replaySourceID != "" && decisionLog != nil {
							mu.Lock()
							sourceReset := syncReplayMirrorWatermarkSource(sc, stratState, replaySourceID, logger)
							var sourceResetSaveErr error
							if sourceReset {
								sourceResetSaveErr = store.SaveStrategyBook(stratState)
							}
							mu.Unlock()
							if sourceResetSaveErr != nil {
								logger.Error("Replay mirror: state save failed after resetting the watermark for source %q: %v (#1510)", replaySourceID, sourceResetSaveErr)
							}
							pending, perr := decisionLog.PendingDecisions(replaySourceID)
							switch {
							case perr != nil:
								logger.Error("Replay mirror: failed to read decision log: %v — holding last replayed state (#1431)", perr)
							case len(pending) > 0:
								mu.Lock()
								appliedIDs, replayTrades, replayDetails, driftDMs := applyReplayedLiveDecisions(sc, stratState, pending, price, result, cfg, logger)
								var saveErr error
								if len(appliedIDs) > 0 {
									saveErr = store.SaveStrategyBook(stratState)
								}
								mu.Unlock()
								sendReplayDriftWarns(driftDMs)
								if replayTrades > 0 {
									trades += replayTrades
									detail = mergeTradeDetails(detail, replayDetails...)
								}
								if len(appliedIDs) > 0 {
									switch {
									case saveErr != nil:
										logger.Error("Replay mirror: state save failed after applying %d decision(s): %v — rows stay pending for retry (#1431)", len(appliedIDs), saveErr)
									default:
										if err := decisionLog.MarkDecisionsApplied(appliedIDs); err != nil {
											logger.Error("Replay mirror: failed to mark %d decision(s) applied: %v (#1431)", len(appliedIDs), err)
										}
									}
								}
							}
						}
						if HedgeEnabled(sc) {
							runHedgeSync(sc, stratState, &mu, defaultHedgeExecutor(), hedgeSyncInputs{
								PrimaryPx:         price,
								HedgePx:           prices[hedgeCoin(sc)],
								FreshExposureQty:  hedgeFreshExposureQty,
								PrimaryCancelOIDs: hedgeUnwindCancelOIDs(stratState, &mu, result.Symbol),
								Live:              hyperliquidIsLive(sc.Args),
							}, notifier, logger)
						}
					}
				case "futures":
					if result, signalStr, price, ok := runTopStepCheck(sc, prices, tsPosCtx, cfg.Regime, resolveATRMethod(sc, cfg), notifier, logger); ok {
						prices[result.Symbol] = price
						storeRegime := globalRegimeStore.PayloadForStrategy(sc, cfg.Regime)
						result.Regime = &storeRegime
						if gateRegime, regimeBlocked := applyRegimeGate(sc, storeRegime, cfg.Regime, tsContracts); regimeBlocked {
							logger.Info("Regime gate: open signal blocked (%s)", regimeGateBlockDetail(gateRegime))
							result.Signal = 0
						}
						hurstDecision := advanceHurstGate(sc, storeRegime, cfg.Regime, stratState, &mu, tsContracts)
						if hurstDecision.Holds && pausedBlocksSignal(result.Signal, result.CloseFraction, tsContracts, tsPosSide, true, true) {
							logger.Info("Hurst gate: %s signal suppressed — %s (#1411)", signalStr, hurstDecision.Detail)
							result.Signal = 0
						}
						if sc.Paused && pausedBlocksSignal(result.Signal, result.CloseFraction, tsContracts, tsPosSide, true, true) {
							logger.Info("Paused: %s signal suppressed — position-increasing actions held while paused (#1150)", signalStr)
							result.Signal = 0
						}
						if sr.DailyLossEntriesHeld && pausedBlocksSignal(result.Signal, result.CloseFraction, tsContracts, tsPosSide, true, true) {
							logger.Info("Daily loss limit: %s signal suppressed — entries held until UTC rollover (#1269)", signalStr)
							result.Signal = 0
						}
						if sr.NotionalBlocked && pausedBlocksSignal(result.Signal, result.CloseFraction, tsContracts, tsPosSide, true, true) {
							logger.Warn("Notional cap: %s signal suppressed — new opens blocked, exits continue (#1344)", signalStr)
							result.Signal = 0
						}
						if persistenceHold && pausedBlocksSignal(result.Signal, result.CloseFraction, tsContracts, tsPosSide, true, true) {
							logger.Warn("Persistence hold: %s signal suppressed — the %s scope has an unacknowledged state save failure, so no new exposure is taken; exits, stops, ratchet and protection continue (#1523)", signalStr, scopeLabel(stratScope))
							result.Signal = 0
						}
						mu.Lock()
						syncStrategyRegimeState(stratState, storeRegime, cfg.Regime)
						mu.Unlock()
						var execResult *TopStepExecuteResult
						liveExecFailed := false
						if topstepIsLive(sc.Args) && result.Signal != 0 {
							if er, ok2 := runTopStepExecuteOrder(sc, result, price, tsCash, tsContracts, tsPosSide, hurstDecision, notifier, logger); ok2 {
								execResult = er
							} else {
								liveExecFailed = true
							}
						}
						if !liveExecFailed {
							mu.Lock()
							trades, detail = executeTopStepResult(sc, stratState, stratDB, result, execResult, signalStr, price, cfg.Regime, cfg, hurstDecision, logger)
							mu.Unlock()
						}
					}
				case "manual":
					mu.RLock()
					pos := stratState.Positions[sc.Symbol]
					mu.RUnlock()
					if pos != nil && !manualPositionOwnedByStrategy(pos, sc.ID) {
						logger.Error("manual position owner mismatch for %s/%s: owner_strategy_id=%q", sc.ID, sc.Symbol, pos.OwnerStrategyID)
						break
					}
					if pos != nil && hyperliquidIsLive(sc.Args) {
						if _, fillPx := runHyperliquidProtectionSync(sc, stratState, stratDB, sc.Symbol, &mu, notifier, logger, "HL manual protection synced", hlReconcileFillHintsJSON, hlLiquidationPx, hlNetSideByCoin); fillPx > 0 {
							trades++
							detail = fmt.Sprintf("[%s] LIVE PROTECTION SYNC SL %s @ $%.2f", sc.ID, sc.Symbol, fillPx)
						}
					}
					closeFraction, _, manualOK := runManualCloseEval(sc, stratState, cfg, notifier, logger)
					manualRegime := globalRegimeStore.PayloadForStrategy(sc, cfg.Regime)
					if manualOK {
						mu.Lock()
						syncStrategyRegimeState(stratState, manualRegime, cfg.Regime)
						stampPositionRegimeIfOpened(stratState, sc.Symbol, manualRegime, sc, cfg.Regime)
						mu.Unlock()
					}
					mu.RLock()
					driftPos := stratState.Positions[sc.Symbol]
					drifted := manualCloseEvaluatorDriftedFromTPs(sc, driftPos)
					var staleTPOIDs []int64
					if drifted {
						staleTPOIDs = cloneInt64s(driftPos.TPOIDs)
					}
					mu.RUnlock()
					if drifted {
						if _, loaded := manualCloseEvaluatorDriftWarned.LoadOrStore(sc.ID+"|"+sc.Symbol, struct{}{}); !loaded {
							logger.Error("manual close-evaluator drift for %s/%s: opened with tiered on-chain TP OIDs=%v but close now resolves to %s (no on-chain TP)", sc.ID, sc.Symbol, staleTPOIDs, trailingTPRatchetRegimeCloseName)
							if notifier != nil {
								notifier.SendOwnerDM(fmt.Sprintf("**MANUAL CLOSE-EVALUATOR DRIFT** [%s] %s was opened under a tiered-TP close (resting TP OIDs=%v) but the strategy now resolves to %s, which places no on-chain TPs. The stop-loss is still protected (the regime trail re-arms), but those TP orders are no longer managed by the close evaluator — they still rest on-chain and cancel on a full/manual close, but can fire mid-position under this let-it-ride config. Pin close_strategy=tiered_tp_atr_live and restart to keep the original exit, or cancel the stale TPs on the HL UI to fully adopt the ratchet.", sc.ID, sc.Symbol, staleTPOIDs, trailingTPRatchetRegimeCloseName))
							}
						}
					}
					if pos != nil && hyperliquidIsLive(sc.Args) {
						runPostTPStopLossAdjustment(sc, stratState, sc.Symbol, prices[sc.Symbol], cfg, &mu, notifier, logger, hlOnChainAbsQty)
						mark := prices[sc.Symbol]
						manualRatchetTightened := false
						if mark > 0 && strategyUsesTrailingTPRatchetClose(sc) {
							ratchetAlert := applyTrailingTPRatchet(sc, stratState, sc.Symbol, mark, &mu, logger)
							notifyRatchetTrigger(notifier, sc.NotifyRatchetTriggersEnabled(cfg), ratchetAlert)
							manualRatchetTightened = ratchetAlert != nil
						}
						mu.RLock()
						pos = stratState.Positions[sc.Symbol]
						mu.RUnlock()
						if pos != nil && mark > 0 && strategyUsesTrailingTPRatchetClose(sc) && effectiveTrailingStopPct(sc, pos) > 0 {
							slEffectiveQty, capped := hlSLEffectiveQty(sc.Symbol, pos.Quantity, hlOnChainAbsQty)
							if capped {
								logger.Warn("manual trailing SL: virtual qty %.6f > on-chain %.6f for %s; capping (#621)", pos.Quantity, slEffectiveQty, sc.Symbol)
							}
							prevSLOID := pos.StopLossOID
							forceResize := pos.ScaleInResizePending && !capped
							newHighWater, slUpdate, updateConfirmed := runHyperliquidTrailingStopUpdate(sc, sc.Symbol, pos.Side, slEffectiveQty, pos, mark, pos.StopLossHighWaterPx, pos.StopLossTriggerPx, pos.StopLossOID, trailingReplacePolicy{forceResize: forceResize, ratchetTightened: manualRatchetTightened, liquidationPx: hlLiquidationPxForSide(hlLiquidationPx, hlNetSideByCoin, sc.Symbol, pos.Side)}, notifier, logger)
							mu.Lock()
							if immediateFill, fillPx := applyTrailingStopUpdateResult(stratState, sc.Symbol, pos.Side, prevSLOID, newHighWater, updateConfirmed, slUpdate, "trailing_stop_loss_immediate", logger, 0); immediateFill {
								logger.Info("[%s] manual trailing SL filled immediately %s @ $%.2f", sc.ID, sc.Symbol, fillPx)
							}
							if forceResize && updateConfirmed {
								if p, ok := stratState.Positions[sc.Symbol]; ok && p != nil {
									p.ScaleInResizePending = false
								}
							}
							mu.Unlock()
						}
					}
					if manualOK && closeFraction > 0 {
						mu.RLock()
						pos = stratState.Positions[sc.Symbol]
						mu.RUnlock()
						if pos != nil {
							if !manualPositionOwnedByStrategy(pos, sc.ID) {
								logger.Error("manual close skipped for %s/%s: owner_strategy_id=%q", sc.ID, sc.Symbol, pos.OwnerStrategyID)
								break
							}
							closeQty := pos.Quantity * closeFraction
							closeSide := "sell"
							if pos.Side == "short" {
								closeSide = "buy"
							}
							intentFullClose := closeFraction >= 1.0
							cancelOID := int64(0)
							if intentFullClose {
								cancelOID = pos.StopLossOID
							}
							closeFullPosition := false
							if intentFullClose {
								closeFullPosition = shouldCloseFullPosition(1.0, sc.Symbol, hlReconcileAll)
							}
							if closeFullPosition {
								logger.Info("Manual full close %s (close_fraction=1.0) — using market_close(sz=None)", sc.Symbol)
							} else if intentFullClose {
								logger.Info("Manual full close %s shares coin with HL peers — using sized close to preserve peer exposure", sc.Symbol)
							}
							var extraCancelOIDs []int64
							if intentFullClose {
								extraCancelOIDs = cloneInt64s(pos.TPOIDs)
							}
							execResult, execStderr, execErr := runHyperliquidExecuteFn(
								sc.Script, sc.Symbol, closeSide, closeQty,
								0, cancelOID, 0, "", 0, closeFullPosition, hlExecuteSnapshot{}, extraCancelOIDs...,
							)
							if execStderr != "" {
								logger.Info("HL manual close stderr: %s", execStderr)
							}
							requestedCancelOIDs := append([]int64{cancelOID}, extraCancelOIDs...)
							execResult, execErr = confirmHyperliquidExecuteFill(execResult, execErr)
							if execErr != nil {
								logger.Error("manual close execute failed: %v", execErr)
								canceledOIDs := hyperliquidExecuteSucceededCancelOIDs(execResult, requestedCancelOIDs)
								if len(canceledOIDs) > 0 {
									mu.Lock()
									clearHyperliquidProtectionOIDsMatching(stratState.Positions[sc.Symbol], canceledOIDs)
									mu.Unlock()
								}
								break
							}
							if execResult.CancelStopLossError != "" {
								logger.Warn("manual close cancel failed (non-fatal) for %s/%s: %s (sl_oid=%d tp_oids=%v) — verify HL on-chain triggers",
									sc.ID, sc.Symbol, execResult.CancelStopLossError, cancelOID, extraCancelOIDs)
							}
							if execResult.Execution != nil && execResult.Execution.Fill != nil {
								fill := execResult.Execution.Fill
								var oid string
								if fill.OID != 0 {
									oid = fmt.Sprintf("%d", fill.OID)
								}
								var realizedPnL float64
								if pos.Side == "long" {
									realizedPnL = closeQty * (fill.AvgPx - pos.AvgCost)
								} else {
									realizedPnL = closeQty * (pos.AvgCost - fill.AvgPx)
								}
								realizedPnL -= fill.Fee
								action := PendingManualAction{
									StrategyID:      sc.ID,
									Action:          "close",
									Symbol:          sc.Symbol,
									Side:            closeSide,
									Quantity:        closeQty,
									FillPrice:       fill.AvgPx,
									FillFee:         fill.Fee,
									ExchangeOrderID: oid,
									RealizedPnL:     realizedPnL,
									IsFullClose:     intentFullClose,
									CreatedAt:       time.Now().UTC(),
								}
								if err := stratDB.InsertPendingManualAction(action); err != nil {
									logger.Error("failed to queue manual close action: %v", err)
								} else {
									prices[sc.Symbol] = fill.AvgPx
									trades = 1
									detail = fmt.Sprintf("manual close %.4f %s @ $%.2f | PnL=$%.2f", closeQty, sc.Symbol, fill.AvgPx, realizedPnL)
									logger.Info("Queued manual close: %s", detail)
								}
							}
						}
					}
				default:
					logger.Error("Unknown strategy type: %s", sc.Type)
				}
				if trades > 0 && detail != "" {
					if chKey := notifier.resolveChannelKey(sc.Platform, sc.Type, isLiveArgs(sc.Args)); chKey != "" {
						channelTrades[chKey] += trades
						key := chKey + "|" + extractAsset(sc)
						channelTradeDetails[key] = append(channelTradeDetails[key], detail)
					}
					sendTradeAlerts(sc, stratState, trades, &mu, notifier)
				}

				totalTrades += trades

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

				mu.RLock()
				pv = displayStrategyValue(stratState, prices)
				posCount := len(stratState.Positions) + len(stratState.OptionPositions)
				cash := stratState.Cash
				regimeLabel := strategyDisplayRegimeLabel(stratState, sc, cfg.Regime)
				if regimeGateFailClosedActive(sc, stratState, cfg.Regime) {
					regimeLabel = decorateRegimeLabelGateClosed(regimeLabel)
				}
				mu.RUnlock()

				statusLine := formatStatusLine(cash, posCount, pv, trades, regimeLabel)
				mu.RLock()
				cashReconcile := stratState.CashReconcileRequired
				mu.RUnlock()
				if cashReconcile {
					statusLine += " | CASH RECONCILE REQUIRED"
				}
				if marker := hurstGateStatusMarkerForStrategy(sc, stratState, cfg.Regime, &mu); marker != "" {
					statusLine += " | " + marker
				}
				logger.Info("%s", statusLine)

				logger.Close()
				lastRun[sc.ID] = time.Now()
			}
		}

		mu.RLock()
		openSymbolsByStrategy := snapshotOpenSymbolsByStrategy(state)
		mu.RUnlock()
		misses := collectMissingMarkPositions(cfg.Strategies, openSymbolsByStrategy, prices)
		missingMarkAlerts.Retain(misses)
		alertNow := time.Now().UTC()
		var missingMarkDMs []string
		for _, miss := range misses {
			if miss.Live {
				disabled := "no mark-gated manager runs on this venue"
				if len(miss.DisabledManagers) > 0 {
					disabled = strings.Join(miss.DisabledManagers, ", ") + " cannot run this cycle"
				}
				fmt.Printf("[WARN] No live mark for %s/%s while a LIVE %s position on %s is open — portfolio value falls back to entry cost and %s\n", miss.StrategyID, miss.Symbol, miss.Type, miss.Platform, disabled)
				if missingMarkAlerts.Record(miss, alertNow) {
					missingMarkDMs = append(missingMarkDMs, formatMissingMarkDM(miss))
				}
				continue
			}
			fmt.Printf("[WARN] No live mark for %s/%s while a record-only position is open — its value falls back to entry cost inside the portfolio drawdown input (no management path is affected)\n", miss.StrategyID, miss.Symbol)
		}
		for _, dm := range missingMarkDMs {
			notifier.SendOwnerDM(dm)
		}

		mu.Lock()
		for _, s := range state.Strategies {
			maybeClearCashReconcileRequired(s)
		}
		mu.Unlock()
		mu.RLock()
		reconcileIDs, reconcileCash := collectCashReconcileRequiredSnapshots(state)
		mu.RUnlock()
		if len(reconcileIDs) > 0 {
			sig := strings.Join(reconcileIDs, ",")
			if globalSpotCashReconcileReminder.ShouldNotify(sig, time.Now().UTC()) {
				msg := formatSpotLiveCashReconcileReminder(reconcileIDs, reconcileCash)
				fmt.Printf("[CRITICAL] %s\n", msg)
				notifySpotLiveCashOverBudget(notifier, msg)
			}
		} else {
			globalSpotCashReconcileReminder.ShouldNotify("", time.Now().UTC())
		}

		mu.RLock()
		channelStrats := make(map[string][]StrategyConfig)
		for _, sc := range cfg.Strategies {
			if _, ok := state.Strategies[sc.ID]; ok {
				if chKey := notifier.resolveChannelKey(sc.Platform, sc.Type, isLiveArgs(sc.Args)); chKey != "" {
					channelStrats[chKey] = append(channelStrats[chKey], sc)
				}
			}
		}
		mu.RUnlock()

		elapsed := time.Since(cycleStart)
		logMgr.LogSummary(cycle, elapsed, len(dueStrategies), totalTrades, totalPV)

		closedByStrategy := LoadClosedPositionsByStrategy(store, cfg)
		rfr := RiskFreeRateOrDefault(cfg)

		lifetimeStats := loadLifetimeStatsBestEffort(store, "[summary]")

		if notifier.HasBackends() {
			summaryNow := time.Now().UTC()
			mu.RLock()
			for chKey, chStrats := range channelStrats {
				chRan := false
				for _, sc := range dueStrategies {
					if notifier.resolveChannelKey(sc.Platform, sc.Type, isLiveArgs(sc.Args)) == chKey {
						chRan = true
						break
					}
				}
				if !chRan {
					continue
				}
				chTrades := channelTrades[chKey]
				continuous := isOptionsType(chStrats) || isFuturesType(chStrats) || isPerpsType(chStrats)
				if !ShouldPostSummary(cfg.SummaryFrequency[chKey], continuous, chTrades > 0, lastSummaryPost[chKey], summaryNow) {
					continue
				}
				assetGroups, assetKeys := groupByAsset(chStrats)
				if len(assetKeys) <= 1 {
					detailKey := chKey + "|"
					if len(assetKeys) == 1 {
						detailKey = chKey + "|" + assetKeys[0]
					}
					chDetails := channelTradeDetails[detailKey]
					chAdj, _ := computeSubsetDisplayValue(chStrats, state, prices, walletBalances, sharedWallets)
					chSharpe := aggregateSharpe(closedByStrategy, chStrats, state, rfr)
					msgs := FormatCategorySummary(cycle, elapsed, len(dueStrategies), chTrades, chAdj, prices, chDetails, chStrats, state, chKey, "", cfg.IntervalSeconds, chSharpe, lifetimeStats, cfg.Regime)
					for _, msg := range msgs {
						notifier.SendToChannel(chKey, chKey, msg)
					}
				} else {
					for _, asset := range assetKeys {
						assetStrats := assetGroups[asset]
						assetDetails := channelTradeDetails[chKey+"|"+asset]
						assetAdj, _ := computeSubsetDisplayValue(assetStrats, state, prices, walletBalances, sharedWallets)
						assetTrades := len(assetDetails)
						assetSharpe := aggregateSharpe(closedByStrategy, assetStrats, state, rfr)
						msgs := FormatCategorySummary(cycle, elapsed, len(dueStrategies), assetTrades, assetAdj, prices, assetDetails, assetStrats, state, chKey, asset, cfg.IntervalSeconds, assetSharpe, lifetimeStats, cfg.Regime)
						for _, msg := range msgs {
							notifier.SendToChannel(chKey, chKey, msg)
						}
					}
				}
				lastSummaryPost[chKey] = summaryNow
			}
			mu.RUnlock()
		}

		mu.Lock()
		state.LastCycle = time.Now().UTC()
		state.LastSummaryPost = cloneTimeMap(lastSummaryPost)

		var duePending []pendingLeaderboardSummary
		if notifier.HasBackends() {
			duePending = collectDueLeaderboardSummaries(cfg, state, prices, ComputeSharpeByStrategy(closedByStrategy, cfg, state), lifetimeStats, walletBalances, sharedWallets)
		}

		saveOutcomes := store.SaveAll(state)
		savedAll := true
		for _, out := range sortedScopeErrors(saveOutcomes) {
			if out.err == nil {
				continue
			}
			savedAll = false
			fmt.Printf("[CRITICAL] Save state failed for the %s scope (%d/3): %v\n",
				scopeLabel(out.scope), store.saveFailures(out.scope), out.err)
		}
		if savedAll {
			offCycleAuditSaveDirty = false
		}

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

		for _, p := range duePending {
			if err := notifier.SendMessage(p.channel, p.msg); err != nil {
				fmt.Printf("[WARN] Leaderboard summary send to channel %s failed: %v\n", p.channel, err)
				continue
			}
			fmt.Printf("[leaderboard-summary] Posted key=%s top_n=%d channel=%s\n",
				p.key, p.topN, p.channel)
		}

		if postLeaderboard {
			stampDate := func() {
				mu.Lock()
				state.LastLeaderboardPostDate = time.Now().UTC().Format("2006-01-02")
				if err := SaveStateWithStore(state, store); err != nil {
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
				lbMessages := BuildLeaderboardMessages(cfg, state, prices, sharpeByStrategy, lifetimeStats, walletBalances, sharedWallets)
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

		if cfg.AutoUpdate == "heartbeat" {
			checkForUpdates(cfg, notifier, &lastNotifiedHash, &mu, state, store)
		} else if cfg.AutoUpdate == "daily" && time.Since(lastAutoUpdateCheck) >= 24*time.Hour {
			checkForUpdates(cfg, notifier, &lastNotifiedHash, &mu, state, store)
			lastAutoUpdateCheck = time.Now()
		}

		if *once {
			fmt.Println("--once flag set, exiting after single cycle.")
			return
		}

		mu.RLock()
		endIntervals := effectiveStrategyIntervals(cfg.Strategies, state.Strategies, cfg.IntervalSeconds, drawdownWarnThresholdPct)
		mu.RUnlock()
		delay := schedulerDelay(cfg.Strategies, endIntervals, lastRun, cfg.IntervalSeconds, time.Now(), tickSeconds)
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-reloadCh:
			timer.Stop()
			reloadConfig()
			processConfigReloads()
		case <-stopCh:
			timer.Stop()
			fmt.Println("[shutdown] exiting trading loop.")
			return
		}
	}
}

func runSummaryAndExit(channelKey string, cfg *Config, state *AppState, sdb *StateStore, notifier *MultiNotifier) {
	if !notifier.HasBackends() {
		fmt.Fprintf(os.Stderr, "No notification backends configured\n")
		os.Exit(1)
	}

	if lcs := findLeaderboardSummariesByChannel(cfg, channelKey); len(lcs) > 0 {
		runLeaderboardSummariesAndExit(lcs, cfg, state, sdb, notifier)
		return
	}

	if !notifier.HasChannel(channelKey, channelKey) {
		fmt.Fprintf(os.Stderr, "No channel configured for %q\n", channelKey)
		os.Exit(1)
	}

	var chStrats []StrategyConfig
	for _, sc := range cfg.Strategies {
		if notifier.resolveChannelKey(sc.Platform, sc.Type, isLiveArgs(sc.Args)) == channelKey {
			chStrats = append(chStrats, sc)
		}
	}
	if len(chStrats) == 0 {
		fmt.Fprintf(os.Stderr, "No strategies found for channel %q\n", channelKey)
		os.Exit(1)
	}

	symbols := collectPriceSymbols(cfg.Strategies)

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

	summaryWalletBalances, _ := fetchSharedWalletBalances(cfg.Strategies, nil)
	summaryAccountShared := detectSharedWallets(cfg.Strategies)

	closedByStrategy := LoadClosedPositionsByStrategy(sdb, cfg)
	rfr := RiskFreeRateOrDefault(cfg)
	lifetimeStats := loadLifetimeStatsBestEffort(sdb, "[summary]")
	assetGroups, assetKeys := groupByAsset(chStrats)
	if len(assetKeys) <= 1 {
		chAdj, _ := computeSubsetDisplayValue(chStrats, state, prices, summaryWalletBalances, summaryAccountShared)
		chSharpe := aggregateSharpe(closedByStrategy, chStrats, state, rfr)
		msgs := FormatCategorySummary(state.CycleCount, 0, 0, 0, chAdj, prices, nil, chStrats, state, channelKey, "", cfg.IntervalSeconds, chSharpe, lifetimeStats, cfg.Regime)
		for _, msg := range msgs {
			notifier.SendToChannel(channelKey, channelKey, msg)
			fmt.Println(msg)
		}
	} else {
		for _, asset := range assetKeys {
			assetStrats := assetGroups[asset]
			assetAdj, _ := computeSubsetDisplayValue(assetStrats, state, prices, summaryWalletBalances, summaryAccountShared)
			assetSharpe := aggregateSharpe(closedByStrategy, assetStrats, state, rfr)
			msgs := FormatCategorySummary(state.CycleCount, 0, 0, 0, assetAdj, prices, nil, assetStrats, state, channelKey, asset, cfg.IntervalSeconds, assetSharpe, lifetimeStats, cfg.Regime)
			for _, msg := range msgs {
				notifier.SendToChannel(channelKey, channelKey, msg)
				fmt.Println(msg)
			}
		}
	}

	fmt.Printf("-summary=%s: posted, exiting.\n", channelKey)
	os.Exit(0)
}

func spotSymbol(args []string) string {
	if len(args) >= 2 {
		return args[1]
	}
	return ""
}

func runSpotCheck(sc StrategyConfig, prices map[string]float64, posCtx PositionCtx, regime *RegimeConfig, atrMethod string, notifier *MultiNotifier, logger *StrategyLogger) (*SpotResult, string, float64, bool) {
	args := append([]string{}, sc.Args...)
	args = appendOpenCloseArgs(args, sc, posCtx)
	if sc.HTFFilter {
		args = append(args, "--htf-filter")
	}
	args = appendRegimeArgs(args, regime)
	args = appendStrategyRegimeWindowArgs(args, sc, regime)
	args = appendRegimePayloadArg(args, sc, regime)
	args = appendATRMethodArg(args, atrMethod)
	if refsArgs, err := buildStrategyRefsArg(sc); err != nil {
		logger.Warn("Failed to marshal strategy refs: %v", err)
	} else if len(refsArgs) > 0 {
		args = append(args, refsArgs...)
	}
	logger.Info("Running: python3 %s %v", sc.Script, args)

	result, stderr, err := RunSpotCheck(sc.Script, args)
	if err != nil {
		logger.Error("Script failed: %v", err)
		if stderr != "" {
			logger.Error("stderr: %s", stderr)
		}
		notifyScriptFailure(notifier, sc, scriptFailureCrash, err.Error())
		return nil, "", 0, false
	}
	if stderr != "" {
		logger.Info("stderr: %s", stderr)
	}

	if result.Error != "" {
		logger.Error("Script returned error: %s", result.Error)
		notifyScriptFailure(notifier, sc, scriptFailureError, result.Error)
		return nil, "", 0, false
	}
	clearScriptFailure(notifier, sc)

	signalStr := "HOLD"
	if result.Signal == 1 {
		signalStr = "BUY"
	} else if result.Signal == -1 {
		signalStr = "SELL"
	}
	logger.Info("Signal: %s | %s @ $%.2f", signalStr, result.Symbol, result.Price)

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

func executeSpotResult(sc StrategyConfig, s *StrategyState, db *StateDB, result *SpotResult, signalStr string, price float64, regime *RegimeConfig, cfg *Config, hurst HurstGateDecision, logger *StrategyLogger) (int, string) {
	exec, err := ExecuteSpotSignalWithFillFeeSizedDeferredOpen(s, result.Signal, result.Symbol, price, 0, 0, "", result.CloseFraction, hurst.OpenSizeMult(), logger)
	if err != nil {
		logger.Error("Trade execution failed: %v", err)
		return 0, ""
	}
	trades := exec.TradesExecuted
	stampEntryATRIfOpened(s, result.Symbol, result.Indicators)
	stampPositionRegimeIfOpened(s, result.Symbol, regimePayloadValue(result.Regime), sc, regime)
	stampDirectionCertifiedAtOpenIfOpened(s, result.Symbol, exec.OpenTrade != nil, sc, regime)
	stampATRMethodAtOpenIfOpened(s, result.Symbol, exec.OpenTrade != nil, sc, cfg)
	stampHurstGateAtOpenIfOpened(s, result.Symbol, exec.OpenTrade != nil, hurst)
	if pos, ok := s.Positions[result.Symbol]; ok {
		recordPositionOpen(s, sc, exec.OpenTrade, pos)
	}
	queueLLMEntryAnalysisIfOpened(sc, s, result.Symbol, trades, exec.OpenTrade, result.Indicators)

	detail := ""
	if trades > 0 {
		detail = fmt.Sprintf("[%s] %s %s @ $%.2f", sc.ID, signalStr, result.Symbol, price)
	}
	return trades, detail
}

func stampPositionRegimeIfOpened(s *StrategyState, symbol string, payload RegimePayload, sc StrategyConfig, regime *RegimeConfig) {
	stampPositionRegimeFromPayload(s, symbol, payload, sc, regime)
}

func stampEntryATRIfOpened(s *StrategyState, symbol string, indicators map[string]interface{}) {
	if s == nil {
		return
	}
	atr, ok := indicatorFloat(indicators, "atr")
	if !ok || atr <= 0 {
		return
	}
	pos, exists := s.Positions[symbol]
	if !exists || pos == nil || pos.EntryATR != 0 {
		return
	}
	if atr != atr || (pos.AvgCost > 0 && atr > pos.AvgCost*0.5) {
		return
	}
	pos.EntryATR = atr
}

func indicatorsATRValue(indicators map[string]interface{}) float64 {
	atr, ok := indicatorFloat(indicators, "atr")
	if !ok || atr <= 0 {
		return 0
	}
	return atr
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

func runOptionsCheck(sc StrategyConfig, posJSON string, notifier *MultiNotifier, logger *StrategyLogger) (*OptionsResult, string, bool) {
	args := append([]string{}, sc.Args...)
	if raw, ok := globalRegimeStore.InjectionJSONForStrategy(sc, nil); ok {
		args = append(args, "--regime-payload-json="+raw)
	}
	logger.Info("Running: python3 %s %v", sc.Script, args)

	result, stderr, err := RunOptionsCheckWithStdin(sc.Script, args, posJSON)
	if err != nil {
		logger.Error("Script failed: %v", err)
		if stderr != "" {
			logger.Error("stderr: %s", stderr)
		}
		notifyScriptFailure(notifier, sc, scriptFailureCrash, err.Error())
		return nil, "", false
	}
	if stderr != "" {
		logger.Info("stderr: %s", stderr)
	}

	if result.Error != "" {
		logger.Error("Script returned error: %s", result.Error)
		notifyScriptFailure(notifier, sc, scriptFailureError, result.Error)
		return nil, "", false
	}
	clearScriptFailure(notifier, sc)

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

func shouldSkipZeroCapital(sc StrategyConfig) bool {
	return sc.CapitalPct > 0 && sc.Capital <= 0 && !sc.sharedWalletModeDeferred
}

func notifyPerStrategyCircuitBreaker(sc StrategyConfig, reason string, portfolioValue float64, notifier *MultiNotifier, portfolioKillSwitchFired bool) {
	notifyPerStrategyCircuitBreakerWithSnapshot(sc, perStrategyCircuitBreakerSnapshot{}, reason, portfolioValue, 0, nil, notifier, portfolioKillSwitchFired)
}

func notifyPerStrategyCircuitBreakerWithSnapshot(sc StrategyConfig, snap perStrategyCircuitBreakerSnapshot, reason string, strategyValue, totalPortfolioValue float64, sdb *StateDB, notifier *MultiNotifier, portfolioKillSwitchFired bool) {
	if notifier == nil || !notifier.HasBackends() || portfolioKillSwitchFired || !isFreshPerStrategyCircuitBreaker(reason) {
		return
	}
	var recent []Trade
	if sdb != nil {
		rows, err := sdb.RecentTradesForStrategy(sc.ID, circuitBreakerAlertMaxRows)
		if err != nil {
			fmt.Printf("[WARN] circuit-breaker recent-trade lookup failed for %s: %v\n", sc.ID, err)
		} else {
			recent = rows
		}
	}
	msg := formatPerStrategyCircuitBreakerBlock(perStrategyCircuitBreakerFormatInput{
		Strategy:            sc,
		Snapshot:            snap,
		Reason:              reason,
		StrategyValue:       strategyValue,
		TotalPortfolioValue: totalPortfolioValue,
		RecentTrades:        recent,
	})
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

func sendTradeAlerts(sc StrategyConfig, stratState *StrategyState, trades int, mu *sync.RWMutex, notifier tradeAlertRouter) {
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
	sendTradeAlertRows(sc, newTrades, notifier)
}

func sendTradeAlertRows(sc StrategyConfig, newTrades []Trade, notifier tradeAlertRouter) {
	isLive := isLiveArgs(sc.Args)
	mode := "paper"
	if isLive {
		mode = "live"
	}

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

func hyperliquidSymbol(args []string) string {
	if len(args) >= 2 {
		return args[1]
	}
	return ""
}

func isHLLiveReconcilable(sc StrategyConfig) bool {
	return sc.Platform == "hyperliquid" &&
		(sc.Type == "perps" || sc.Type == "manual") &&
		hyperliquidIsLive(sc.Args)
}

func runHyperliquidCheck(sc *StrategyConfig, prices map[string]float64, posCtx PositionCtx, regime *RegimeConfig, atrMethod string, notifier *MultiNotifier, logger *StrategyLogger, batch *hlBatchCycleResults) (*HyperliquidResult, string, float64, bool) {
	if outcome, ok := batch.lookup(sc.ID); ok {
		fp, fpErr := hyperliquidBatchSlotFingerprint(*sc, posCtx, regime)
		if fpErr == nil && fp == outcome.Fingerprint {
			result := outcome.Result
			if result != nil {
				res := *result
				if sym := hyperliquidSymbol(sc.Args); sym != "" {
					if mid, ok := prices[sym]; ok && mid > 0 {
						res.Price = hyperliquidBatchDisplayPrice(mid)
					}
				}
				result = &res
			}
			return finishHyperliquidCheck(sc, prices, posCtx, regime, notifier, logger,
				result, outcome.Stderr, outcome.Err, outcome.Mode)
		}
		logger.Warn("Batched check inputs changed since the pre-pass snapshot; running this strategy's own check (#1442)")
	}
	args := append([]string{}, sc.Args...)
	scForCheck := strategyConfigWithOnChainProtectionFilter(*sc)
	args = appendOpenCloseArgs(args, scForCheck, posCtx)
	if sc.HTFFilter {
		args = append(args, "--htf-filter")
	}
	args = appendRegimeArgs(args, regime)
	args = appendStrategyRegimeWindowArgs(args, *sc, regime)
	args = appendRegimePayloadArg(args, *sc, regime)
	args = appendATRMethodArg(args, atrMethod)
	if refsArgs, err := buildStrategyRefsArg(scForCheck); err != nil {
		logger.Warn("Failed to marshal strategy refs: %v", err)
	} else if len(refsArgs) > 0 {
		args = append(args, refsArgs...)
	}
	if sym := hyperliquidSymbol(sc.Args); sym != "" {
		if mid, ok := prices[sym]; ok && mid > 0 {
			args = append(args, fmt.Sprintf("--mark-price=%g", mid))
		}
	}
	logger.Info("Running: python3 %s %v", sc.Script, args)

	result, stderr, err := RunHyperliquidCheck(sc.Script, args)
	errMsg, mode := "", scriptFailureCrash
	switch {
	case err != nil:
		errMsg, result = err.Error(), nil
	case result.Error != "":
		errMsg, mode, result = result.Error, scriptFailureError, nil
	}
	return finishHyperliquidCheck(sc, prices, posCtx, regime, notifier, logger, result, stderr, errMsg, mode)
}

func finishHyperliquidCheck(sc *StrategyConfig, prices map[string]float64, posCtx PositionCtx, regime *RegimeConfig, notifier *MultiNotifier, logger *StrategyLogger, result *HyperliquidResult, stderr, errMsg string, mode scriptFailureMode) (*HyperliquidResult, string, float64, bool) {
	if errMsg != "" {
		switch {
		case mode == scriptFailureCrash:
			logger.Error("Script failed: %v", errMsg)
			if stderr != "" {
				logger.Error("stderr: %s", stderr)
			}
			notifyScriptFailure(notifier, *sc, scriptFailureCrash, errMsg)
		default:
			if stderr != "" {
				logger.Info("stderr: %s", stderr)
			}
			logger.Error("Script returned error: %s", errMsg)
			notifyScriptFailure(notifier, *sc, scriptFailureError, errMsg)
		}
		return nil, "", 0, false
	}
	if stderr != "" {
		logger.Info("stderr: %s", stderr)
	}
	clearScriptFailure(notifier, *sc)
	currentDirRegime := regimeDirectionalLabel(*sc, regimePayloadValue(result.Regime), regime)
	posDirRegime := posCtx.DirectionalRegime
	var dirCertStates map[string]string
	if sc.RegimeDirectionalPolicy.IsConfigured() {
		if posCtx.Quantity > 0 {
			dirCertStates = posCtx.DirectionCertifiedStatesAtOpen
		} else {
			dirCertStates, _ = strategyDirectionalCertified(*sc, regime, time.Now().UTC())
		}
	}
	if entry, applied, legacyFallback := applyRegimeDirectionalPolicy(sc, currentDirRegime, posDirRegime, posCtx.Quantity, dirCertStates); applied {
		regimeKey := effectiveRegimeForPolicy(currentDirRegime, posDirRegime, posCtx.Quantity)
		logger.Info("Regime directional policy: regime=%s -> direction=%q invert_signal=%t",
			regimeKey, entry.Direction, entry.InvertSignal)
		if legacyFallback {
			if _, loaded := regimeDirectionalLegacyWarned.LoadOrStore(sc.ID, struct{}{}); !loaded {
				logger.Warn("Regime directional policy: open position has no stamped regime (legacy pre-#741); resolving against current regime=%q. Hold-on-transition not guaranteed for this position; self-heals on next entry.", regimeKey)
			}
		}
	}
	if sc.RegimeWindowDivergence.IsConfigured() {
		payload := regimePayloadValue(result.Regime)
		divResult := applyRegimeDivergenceOverride(sc, payload, regime, posCtx.Quantity)
		result.Divergence = divResult
		if divResult.IsActive() && posCtx.Quantity <= 0 {
			logger.Info("Regime divergence override: short=%s medium=%s -> direction=%q (was policy-resolved)",
				divResult.ShortLabel, divResult.MediumLabel, divResult.OverrideDir)
		} else if divResult.Kind == DivergenceHard {
			logger.Info("Regime divergence: hard divergence short=%s medium=%s (position open, holding direction)",
				divResult.ShortLabel, divResult.MediumLabel)
		} else if divResult.Kind == DivergenceSoft {
			logger.Info("Regime divergence: soft divergence short=%s medium=%s (no override)",
				divResult.ShortLabel, divResult.MediumLabel)
		}
	}
	applySignalInversion(*sc, result, logger)

	signalStr := signalLabel(result.Signal)
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

func applySignalInversion(sc StrategyConfig, result *HyperliquidResult, logger *StrategyLogger) {
	if !sc.InvertSignal || result == nil || result.Signal == 0 {
		return
	}
	original := result.Signal
	result.Signal = -result.Signal
	logger.Info("Signal inversion enabled: %s -> %s", signalLabel(original), signalLabel(result.Signal))
}

func signalLabel(signal int) string {
	switch signal {
	case 1:
		return "BUY"
	case -1:
		return "SELL"
	default:
		return "HOLD"
	}
}

func shouldCloseFullPosition(closeFraction float64, symbol string, hlLiveAll []StrategyConfig) bool {
	if closeFraction != 1.0 {
		return false
	}
	return len(hlLiveStrategiesForCoin(symbol, hlLiveAll)) <= 1
}

func runHyperliquidExecuteOrder(sc StrategyConfig, result *HyperliquidResult, price, cash float64, poolBalanceKnown bool, posQty float64, posSide string, avgCost, posLeverage float64, existingStopLossOID int64, existingTPOIDs []int64, hlLiveAll []StrategyConfig, walletSnapshot hlExecuteSnapshot, hurst HurstGateDecision, notifier *MultiNotifier, logger *StrategyLogger) (*HyperliquidExecuteResult, bool) {
	directionEnum := EffectiveDirection(sc)
	if reason := PerpsOrderSkipReason(result.Signal, posSide, directionEnum); reason != "" {
		logger.Info("Skipping live order for %s: %s", result.Symbol, reason)
		return nil, false
	}
	isBuy := result.Signal == 1
	sizing := withEntrySizeMult(withSharedWalletPoolSizing(
		sc,
		PerpsSizingFor(sc, price, indicatorsATRValue(result.Indicators)),
		posQty,
		price,
		avgCost,
		posLeverage,
		poolBalanceKnown,
	), hurst.OpenSizeMult())
	size, ok, reason := perpsLiveOrderSize(result.Signal, price, cash, posQty, avgCost, sizing, posSide, directionEnum, result.CloseFraction)
	if !ok {
		logger.Info("%s for %s", reason, result.Symbol)
		return nil, false
	}

	side := "buy"
	if !isBuy {
		side = "sell"
	}

	pureClose := perpsCloseActionSuppressesNewSL(result.Signal, posSide, PerpsAllowsLong(sc), PerpsAllowsShort(sc), result.CloseFraction)
	partialClose := result.CloseFraction > 0 && result.CloseFraction < 1
	flipping := EffectiveDirection(sc) == DirectionBoth && posQty > 0 && result.CloseFraction == 0 && ((result.Signal == 1 && posSide == "short") || (result.Signal == -1 && posSide == "long"))
	var cancelOID int64
	if existingStopLossOID > 0 && posQty > 0 && !partialClose {
		cancelOID = existingStopLossOID
	}
	var extraCancelOIDs []int64
	if posQty > 0 && !partialClose {
		extraCancelOIDs = append(extraCancelOIDs, existingTPOIDs...)
	}
	var slPct float64
	if !pureClose && !partialClose {
		slPct = EffectiveStopLossPct(sc)
	}
	var prevPosQty float64
	if flipping {
		prevPosQty = posQty
	}

	marginMode := ""
	leverageForOpen := 0.0
	if posQty == 0 && sc.Platform == "hyperliquid" && sc.Type == "perps" && sc.MarginMode != "" {
		marginMode = sc.MarginMode
		leverageForOpen = EffectiveExchangeLeverage(sc)
		if leverageForOpen <= 0 {
			leverageForOpen = 1
		}
	}

	if slPct > 0 || cancelOID > 0 || prevPosQty > 0 || marginMode != "" {
		logger.Info("Placing live %s %s size=%.6f (sl_pct=%.2f cancel_oid=%d prev_pos_qty=%.6f margin_mode=%q leverage=%g)",
			side, result.Symbol, size, slPct, cancelOID, prevPosQty, marginMode, leverageForOpen)
	} else {
		logger.Info("Placing live %s %s size=%.6f", side, result.Symbol, size)
	}

	closeFullPosition := shouldCloseFullPosition(result.CloseFraction, result.Symbol, hlLiveAll)
	if closeFullPosition {
		logger.Info("Final-tier full close %s (close_fraction=1.0) — using market_close(sz=None)", result.Symbol)
	} else if result.CloseFraction == 1.0 {
		logger.Info("Final-tier close %s shares coin with HL perps peers — using sized close to preserve peer exposure", result.Symbol)
	}
	execResult, stderr, err := runHyperliquidExecuteFn(sc.Script, result.Symbol, side, size, slPct, cancelOID, prevPosQty, marginMode, leverageForOpen, closeFullPosition, walletSnapshot, extraCancelOIDs...)
	if stderr != "" {
		logger.Info("execute stderr: %s", stderr)
	}
	direction := directionOpen
	if side == "sell" {
		direction = directionClose
	}
	execResult, err = confirmHyperliquidExecuteFill(execResult, err)
	if err != nil {
		logger.Error("Live execute failed: %v", err)
		notifyLiveExecFailure(notifier, sc, direction, result.Symbol, err.Error())
		return execResult, false
	}
	clearLiveExecThrottle(sc, direction, result.Symbol)
	if execResult.CancelStopLossError != "" {
		logger.Warn("SL cancel failed (non-fatal): %s", execResult.CancelStopLossError)
	}
	if execResult.StopLossError != "" {
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

func isHLOpenOrderCapRejection(errStr string) bool {
	lower := strings.ToLower(errStr)
	hasCapVerb := strings.Contains(lower, "too many") || strings.Contains(lower, "rate limit") || strings.Contains(lower, "max") || strings.Contains(lower, "limit") || strings.Contains(lower, "exceed")
	if !hasCapVerb {
		return false
	}
	return strings.Contains(lower, "trigger order") || strings.Contains(lower, "open order") || strings.Contains(lower, "open orders")
}

func executeHyperliquidResult(sc StrategyConfig, s *StrategyState, result *HyperliquidResult, execResult *HyperliquidExecuteResult, signalStr string, price float64, regime *RegimeConfig, cfg *Config, hurst HurstGateDecision, logger *StrategyLogger) (int, string) {
	trades, detail, openTrade, _ := executeHyperliquidResultDeferredOpen(sc, s, result, execResult, signalStr, price, regime, cfg, hurst, logger)
	if openTrade != nil {
		var pos *Position
		if p, ok := s.Positions[result.Symbol]; ok {
			pos = p
		}
		recordPositionOpen(s, sc, openTrade, pos)
	}
	return trades, detail
}

func executeHyperliquidResultDeferredOpen(sc StrategyConfig, s *StrategyState, result *HyperliquidResult, execResult *HyperliquidExecuteResult, signalStr string, price float64, regime *RegimeConfig, cfg *Config, hurst HurstGateDecision, logger *StrategyLogger) (int, string, *Trade, *RatchetTriggerAlert) {
	if execResult != nil {
		if _, err := confirmHyperliquidExecuteFill(execResult, nil); err != nil {
			return 0, "", nil, nil
		}
	}
	fillPrice := price
	var fillQty float64
	if execResult != nil && execResult.Execution != nil && execResult.Execution.Fill != nil && execResult.Execution.Fill.AvgPx > 0 {
		fillPrice = execResult.Execution.Fill.AvgPx
		fillQty = execResult.Execution.Fill.TotalSz
		logger.Info("Live fill at $%.2f qty=%.6f (mid was $%.2f)", fillPrice, fillQty, price)
	}

	sizing := withEntrySizeMult(PerpsSizingFor(sc, fillPrice, indicatorsATRValue(result.Indicators)), hurst.OpenSizeMult())

	var fillOID string
	var fillFee float64
	if execResult != nil && execResult.Execution != nil && execResult.Execution.Fill != nil {
		fill := execResult.Execution.Fill
		if fill.OID != 0 {
			fillOID = fmt.Sprintf("%d", fill.OID)
		}
		fillFee = fill.Fee
	}

	exec, err := ExecutePerpsSignalWithLeverageDeferredOpen(s, result.Signal, result.Symbol, fillPrice, sizing, fillQty, fillOID, fillFee, EffectiveDirection(sc), result.CloseFraction, logger)
	if err != nil {
		logger.Error("Trade execution failed: %v", err)
		return 0, "", nil, nil
	}
	trades := exec.TradesExecuted
	openTrade := exec.OpenTrade
	stampEntryATRIfOpened(s, result.Symbol, result.Indicators)
	stampPositionRegimeIfOpened(s, result.Symbol, regimePayloadValue(result.Regime), sc, regime)
	stampDirectionCertifiedAtOpenIfOpened(s, result.Symbol, openTrade != nil, sc, regime)
	stampATRMethodAtOpenIfOpened(s, result.Symbol, openTrade != nil, sc, cfg)
	stampHurstGateAtOpenIfOpened(s, result.Symbol, openTrade != nil, hurst)
	if pos, ok := s.Positions[result.Symbol]; ok {
		stampPositionProtectionSnapshot(pos, sc)
	}
	var ratchetAlert *RatchetTriggerAlert
	if trades > 0 {
		if pos, ok := s.Positions[result.Symbol]; ok {
			_, ratchetAlert = applyTrailingTPRatchetToPosition(sc, pos, result.Symbol, price, logger)
		}
	}
	if trades > 0 && fillOID != "" {
		logger.Info("Exchange order ID: %s", fillOID)
	}
	if trades > 0 {
		if pos, ok := s.Positions[result.Symbol]; ok && effectiveTrailingStopPct(sc, pos) > 0 {
			pos.StopLossHighWaterPx = fillPrice
		}
	}

	if trades > 0 && execResult != nil && execResult.Execution != nil && execResult.Execution.Fill != nil {
		if slOID := execResult.Execution.Fill.StopLossOID; slOID > 0 {
			if pos, ok := s.Positions[result.Symbol]; ok {
				pos.StopLossOID = slOID
				pos.StopLossTriggerPx = execResult.Execution.Fill.StopLossTriggerPx
				logger.Info("SL trigger placed oid=%d @ $%.4f", slOID, execResult.Execution.Fill.StopLossTriggerPx)
			}
		}
	}

	if trades > 0 && execResult != nil && execResult.StopLossFilledImmediately &&
		execResult.Execution != nil && execResult.Execution.Fill != nil {
		var pos *Position
		if p, ok := s.Positions[result.Symbol]; ok {
			pos = p
		}
		if recordPositionOpen(s, sc, openTrade, pos) {
			openTrade = nil
		}
		triggerPx := execResult.Execution.Fill.StopLossTriggerPx
		if recordPerpsStopLossClose(s, result.Symbol, triggerPx, "stop_loss_immediate", logger) {
			trades++
		}
	}

	queueLLMEntryAnalysisIfOpened(sc, s, result.Symbol, trades, openTrade, result.Indicators)

	detail := ""
	if trades > 0 {
		prefix := ""
		if execResult != nil {
			prefix = "LIVE "
		}
		detail = fmt.Sprintf("[%s] %s%s %s @ $%.2f", sc.ID, prefix, signalStr, result.Symbol, fillPrice)
	}
	if execResult == nil {
		var pos *Position
		if p, ok := s.Positions[result.Symbol]; ok {
			pos = p
		}
		if recordPositionOpen(s, sc, openTrade, pos) {
			openTrade = nil
		}
	}
	return trades, detail, openTrade, ratchetAlert
}

func topstepIsLive(args []string) bool {
	return isLiveArgs(args)
}

func topstepSymbol(args []string) string {
	if len(args) >= 2 {
		return args[1]
	}
	return ""
}

func runTopStepCheck(sc StrategyConfig, prices map[string]float64, posCtx PositionCtx, regime *RegimeConfig, atrMethod string, notifier *MultiNotifier, logger *StrategyLogger) (*TopStepResult, string, float64, bool) {
	args := append([]string{}, sc.Args...)
	args = appendOpenCloseArgs(args, sc, posCtx)
	if sc.HTFFilter {
		args = append(args, "--htf-filter")
	}
	args = appendRegimeArgs(args, regime)
	args = appendStrategyRegimeWindowArgs(args, sc, regime)
	args = appendRegimePayloadArg(args, sc, regime)
	args = appendATRMethodArg(args, atrMethod)
	if refsArgs, err := buildStrategyRefsArg(sc); err != nil {
		logger.Warn("Failed to marshal strategy refs: %v", err)
	} else if len(refsArgs) > 0 {
		args = append(args, refsArgs...)
	}
	logger.Info("Running: python3 %s %v", sc.Script, args)

	result, stderr, err := RunTopStepCheck(sc.Script, args)
	if err != nil {
		logger.Error("Script failed: %v", err)
		if stderr != "" {
			logger.Error("stderr: %s", stderr)
		}
		notifyScriptFailure(notifier, sc, scriptFailureCrash, err.Error())
		return nil, "", 0, false
	}
	if stderr != "" {
		logger.Info("stderr: %s", stderr)
	}
	if result.Error != "" {
		logger.Error("Script returned error: %s", result.Error)
		notifyScriptFailure(notifier, sc, scriptFailureError, result.Error)
		return nil, "", 0, false
	}
	clearScriptFailure(notifier, sc)

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

func runTopStepExecuteOrder(sc StrategyConfig, result *TopStepResult, price, cash, posQty float64, posSide string, hurst HurstGateDecision, notifier *MultiNotifier, logger *StrategyLogger) (*TopStepExecuteResult, bool) {
	if reason := FuturesOrderSkipReason(result.Signal, posSide); reason != "" {
		logger.Info("Skipping live order for %s: %s", result.Symbol, reason)
		return nil, false
	}
	isBuy := result.Signal == 1
	var contracts int
	if isBuy {
		budget := cash
		margin := result.ContractSpec.Margin
		if margin <= 0 {
			margin = price * result.ContractSpec.Multiplier
		}
		if budget < 1 || price <= 0 || margin <= 0 {
			logger.Info("Insufficient cash ($%.2f) for live buy", cash)
			return nil, false
		}
		contracts = int(budget * hurst.OpenSizeMult() / margin)
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
		if result.CloseFraction > 0 && result.CloseFraction < 1 {
			partial := int(float64(contracts) * result.CloseFraction)
			if partial < 1 {
				logger.Info("Partial-close fraction %.4f rounds to 0 contracts for %s; skipping live order", result.CloseFraction, result.Symbol)
				return nil, false
			}
			if partial < contracts {
				contracts = partial
			}
		}
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

func executeTopStepResult(sc StrategyConfig, s *StrategyState, db *StateDB, result *TopStepResult, execResult *TopStepExecuteResult, signalStr string, price float64, regime *RegimeConfig, cfg *Config, hurst HurstGateDecision, logger *StrategyLogger) (int, string) {
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

	exec, err := ExecuteFuturesSignalWithFillFeeSizedDeferredOpen(s, result.Signal, result.Symbol, fillPrice, result.ContractSpec, feePerContract, maxContracts, fillContracts, fillFee, fillOID, result.CloseFraction, hurst.OpenSizeMult(), logger)
	if err != nil {
		logger.Error("Trade execution failed: %v", err)
		return 0, ""
	}
	trades := exec.TradesExecuted
	stampEntryATRIfOpened(s, result.Symbol, result.Indicators)
	stampPositionRegimeIfOpened(s, result.Symbol, regimePayloadValue(result.Regime), sc, regime)
	stampDirectionCertifiedAtOpenIfOpened(s, result.Symbol, exec.OpenTrade != nil, sc, regime)
	stampATRMethodAtOpenIfOpened(s, result.Symbol, exec.OpenTrade != nil, sc, cfg)
	stampHurstGateAtOpenIfOpened(s, result.Symbol, exec.OpenTrade != nil, hurst)
	if pos, ok := s.Positions[result.Symbol]; ok {
		recordPositionOpen(s, sc, exec.OpenTrade, pos)
	}
	queueLLMEntryAnalysisIfOpened(sc, s, result.Symbol, trades, exec.OpenTrade, result.Indicators)

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

func robinhoodIsLive(args []string) bool {
	return isLiveArgs(args)
}

func robinhoodSymbol(args []string) string {
	if len(args) >= 2 {
		return args[1]
	}
	return ""
}

func runRobinhoodCheck(sc StrategyConfig, prices map[string]float64, posCtx PositionCtx, regime *RegimeConfig, atrMethod string, notifier *MultiNotifier, logger *StrategyLogger) (*RobinhoodResult, string, float64, bool) {
	args := append([]string{}, sc.Args...)
	args = appendOpenCloseArgs(args, sc, posCtx)
	if sc.HTFFilter {
		args = append(args, "--htf-filter")
	}
	args = appendRegimeArgs(args, regime)
	args = appendStrategyRegimeWindowArgs(args, sc, regime)
	args = appendRegimePayloadArg(args, sc, regime)
	args = appendATRMethodArg(args, atrMethod)
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
		notifyScriptFailure(notifier, sc, scriptFailureCrash, err.Error())
		return nil, "", 0, false
	}
	if stderr != "" {
		logger.Info("stderr: %s", stderr)
	}
	if result.Error != "" {
		logger.Error("Script returned error: %s", result.Error)
		notifyScriptFailure(notifier, sc, scriptFailureError, result.Error)
		return nil, "", 0, false
	}
	clearScriptFailure(notifier, sc)

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

var robinhoodExecuteFn = RunRobinhoodExecute
var okxExecuteFn = RunOKXExecute

func runRobinhoodExecuteOrder(sc StrategyConfig, result *RobinhoodResult, price, cash float64, cashReconcileRequired bool, posQty float64, posSide string, hurst HurstGateDecision, notifier *MultiNotifier, logger *StrategyLogger) (*RobinhoodExecuteResult, bool) {
	if reason := SpotOrderSkipReason(result.Signal, posSide); reason != "" {
		logger.Info("Skipping live order for %s: %s", result.Symbol, reason)
		return nil, false
	}
	isBuy := result.Signal == 1
	var amountUSD float64
	var quantity float64
	side := "buy"

	if isBuy {
		if cashReconcileBlocksLiveBuy(cashReconcileRequired, true) {
			logger.Warn("Skipping live buy for %s: cash reconcile required (#1394)", result.Symbol)
			return nil, false
		}
		amountUSD = cash * hurst.OpenSizeMult()
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
			quantity = posQty * result.CloseFraction
		}
	}

	logger.Info("Placing live %s %s amount_usd=%.2f qty=%.6f", side, result.Symbol, amountUSD, quantity)

	execResult, stderr, err := robinhoodExecuteFn(sc.Script, result.Symbol, side, amountUSD, quantity)
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

func executeRobinhoodResult(sc StrategyConfig, s *StrategyState, db *StateDB, result *RobinhoodResult, execResult *RobinhoodExecuteResult, signalStr string, price float64, regime *RegimeConfig, cfg *Config, hurst HurstGateDecision, logger *StrategyLogger) (int, string, string) {
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

	exec, err := ExecuteSpotSignalWithFillFeeSizedDeferredOpen(s, result.Signal, result.Symbol, fillPrice, fillQty, fillFee, fillOID, result.CloseFraction, hurst.OpenSizeMult(), logger)
	if err != nil {
		logger.Error("Trade execution failed: %v", err)
		return 0, "", ""
	}
	trades := exec.TradesExecuted
	stampEntryATRIfOpened(s, result.Symbol, result.Indicators)
	stampPositionRegimeIfOpened(s, result.Symbol, regimePayloadValue(result.Regime), sc, regime)
	stampDirectionCertifiedAtOpenIfOpened(s, result.Symbol, exec.OpenTrade != nil, sc, regime)
	stampATRMethodAtOpenIfOpened(s, result.Symbol, exec.OpenTrade != nil, sc, cfg)
	stampHurstGateAtOpenIfOpened(s, result.Symbol, exec.OpenTrade != nil, hurst)
	if pos, ok := s.Positions[result.Symbol]; ok {
		recordPositionOpen(s, sc, exec.OpenTrade, pos)
	}
	queueLLMEntryAnalysisIfOpened(sc, s, result.Symbol, trades, exec.OpenTrade, result.Indicators)

	detail := ""
	if trades > 0 {
		prefix := ""
		if execResult != nil {
			prefix = "LIVE "
		}
		detail = fmt.Sprintf("[%s] %s%s %s @ $%.2f", sc.ID, prefix, signalStr, result.Symbol, fillPrice)
	}
	cashAlert := ""
	if exec.CashReconcileRequired {
		cashAlert = exec.CashOverBudgetAlert
	}
	return trades, detail, cashAlert
}

func okxIsLive(args []string) bool {
	return isLiveArgs(args)
}

func okxSymbol(args []string) string {
	if len(args) >= 2 {
		return args[1]
	}
	return ""
}

func okxInstType(args []string) string {
	for _, arg := range args {
		if strings.HasPrefix(arg, "--inst-type=") {
			return strings.TrimPrefix(arg, "--inst-type=")
		}
	}
	return "swap"
}

func runOKXCheck(sc StrategyConfig, prices map[string]float64, posCtx PositionCtx, regime *RegimeConfig, atrMethod string, notifier *MultiNotifier, logger *StrategyLogger) (*OKXResult, string, float64, bool) {
	args := append([]string{}, sc.Args...)
	args = appendOpenCloseArgs(args, sc, posCtx)
	if sc.HTFFilter {
		args = append(args, "--htf-filter")
	}
	args = appendRegimeArgs(args, regime)
	args = appendStrategyRegimeWindowArgs(args, sc, regime)
	args = appendRegimePayloadArg(args, sc, regime)
	args = appendATRMethodArg(args, atrMethod)
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
		notifyScriptFailure(notifier, sc, scriptFailureCrash, err.Error())
		return nil, "", 0, false
	}
	if stderr != "" {
		logger.Info("stderr: %s", stderr)
	}
	if result.Error != "" {
		logger.Error("Script returned error: %s", result.Error)
		notifyScriptFailure(notifier, sc, scriptFailureError, result.Error)
		return nil, "", 0, false
	}
	clearScriptFailure(notifier, sc)

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

func runOKXExecuteOrder(sc StrategyConfig, result *OKXResult, price, cash float64, poolBalanceKnown, cashReconcileRequired bool, posQty float64, posSide string, avgCost, posLeverage float64, hurst HurstGateDecision, notifier *MultiNotifier, logger *StrategyLogger) (*OKXExecuteResult, bool) {
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
	if sc.Type != "perps" && cashReconcileBlocksLiveBuy(cashReconcileRequired, isBuy) {
		logger.Warn("Skipping live buy for %s: cash reconcile required (#1394)", result.Symbol)
		return nil, false
	}
	sizing := withEntrySizeMult(withSharedWalletPoolSizing(
		sc,
		PerpsSizingFor(sc, price, indicatorsATRValue(result.Indicators)),
		posQty,
		price,
		avgCost,
		posLeverage,
		poolBalanceKnown,
	), hurst.OpenSizeMult())
	var size float64
	if sc.Type == "perps" {
		var ok bool
		var reason string
		size, ok, reason = perpsLiveOrderSize(result.Signal, price, cash, posQty, avgCost, sizing, posSide, EffectiveDirection(sc), result.CloseFraction)
		if !ok {
			logger.Info("%s for %s", reason, result.Symbol)
			return nil, false
		}
	} else {
		if isBuy {
			budget := cash * hurst.OpenSizeMult()
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

	execResult, stderr, err := okxExecuteFn(sc.Script, result.Symbol, side, size, instType)
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

func executeOKXResult(sc StrategyConfig, s *StrategyState, db *StateDB, result *OKXResult, execResult *OKXExecuteResult, signalStr string, price float64, regime *RegimeConfig, cfg *Config, hurst HurstGateDecision, logger *StrategyLogger) (int, string, string) {
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

	var exec SignalExecutionResult
	var err error
	if sc.Type == "perps" {
		exec, err = ExecutePerpsSignalWithLeverageDeferredOpen(s, result.Signal, result.Symbol, fillPrice, withEntrySizeMult(PerpsSizingFor(sc, fillPrice, indicatorsATRValue(result.Indicators)), hurst.OpenSizeMult()), fillQty, fillOID, fillFee, EffectiveDirection(sc), result.CloseFraction, logger)
	} else {
		exec, err = ExecuteSpotSignalWithFillFeeSizedDeferredOpen(s, result.Signal, result.Symbol, fillPrice, fillQty, fillFee, fillOID, result.CloseFraction, hurst.OpenSizeMult(), logger)
	}
	if err != nil {
		logger.Error("Trade execution failed: %v", err)
		return 0, "", ""
	}
	trades := exec.TradesExecuted
	stampEntryATRIfOpened(s, result.Symbol, result.Indicators)
	stampPositionRegimeIfOpened(s, result.Symbol, regimePayloadValue(result.Regime), sc, regime)
	stampDirectionCertifiedAtOpenIfOpened(s, result.Symbol, exec.OpenTrade != nil, sc, regime)
	stampATRMethodAtOpenIfOpened(s, result.Symbol, exec.OpenTrade != nil, sc, cfg)
	stampHurstGateAtOpenIfOpened(s, result.Symbol, exec.OpenTrade != nil, hurst)
	if pos, ok := s.Positions[result.Symbol]; ok {
		recordPositionOpen(s, sc, exec.OpenTrade, pos)
	}
	queueLLMEntryAnalysisIfOpened(sc, s, result.Symbol, trades, exec.OpenTrade, result.Indicators)

	detail := ""
	if trades > 0 {
		prefix := ""
		if execResult != nil {
			prefix = "LIVE "
		}
		detail = fmt.Sprintf("[%s] %s%s %s @ $%.2f", sc.ID, prefix, signalStr, result.Symbol, fillPrice)
	}
	cashAlert := ""
	if exec.CashReconcileRequired {
		cashAlert = exec.CashOverBudgetAlert
	}
	return trades, detail, cashAlert
}

func findLeaderboardSummariesByChannel(cfg *Config, channelID string) []LeaderboardSummaryConfig {
	var out []LeaderboardSummaryConfig
	for _, lc := range cfg.LeaderboardSummaries {
		if lc.Channel == channelID {
			out = append(out, lc)
		}
	}
	return out
}

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

func runLeaderboardSummariesAndExit(lcs []LeaderboardSummaryConfig, cfg *Config, state *AppState, sdb *StateStore, notifier *MultiNotifier) {
	prices := fetchPricesForSummary(cfg)
	sharpeByStrategy := ComputeSharpeByStrategy(LoadClosedPositionsByStrategy(sdb, cfg), cfg, state)
	lifetimeStats := loadLifetimeStatsBestEffort(sdb, "[leaderboard]")
	walletBalances, _ := fetchSharedWalletBalances(cfg.Strategies, nil)
	accountShared := detectSharedWallets(cfg.Strategies)
	posted := 0
	for _, lc := range lcs {
		msg := BuildLeaderboardSummary(lc, cfg, state, prices, sharpeByStrategy, lifetimeStats, walletBalances, accountShared)
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

func loadLifetimeStatsBestEffort(sdb *StateStore, logPrefix string) map[string]LifetimeTradeStats {
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

type pendingLeaderboardSummary struct {
	channel string
	msg     string
	key     string
	topN    int
}

func collectDueLeaderboardSummaries(cfg *Config, state *AppState, prices map[string]float64, sharpeByStrategy map[string]float64, lifetimeStats map[string]LifetimeTradeStats, walletBalances map[SharedWalletKey]float64, accountShared map[SharedWalletKey][]string) []pendingLeaderboardSummary {
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
		msg := BuildLeaderboardSummary(lc, cfg, state, prices, sharpeByStrategy, lifetimeStats, walletBalances, accountShared)
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

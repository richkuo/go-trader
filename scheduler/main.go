package main

import (
	"encoding/json"
	"errors"
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
	"manual-add",
	"manual-close",
	"force-close",
	"manual-cancel",
	"manual-update-sl",
	"manual-cancel-sl",
	"backfill",
	"probe",
	"inspect",
	"agent-info",
	"diagnostics",
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
		case "manual-add":
			os.Exit(runManualAdd(os.Args[2:]))
		case "manual-close":
			os.Exit(runManualClose(os.Args[2:]))
		case "force-close":
			os.Exit(runForceClose(os.Args[2:]))
		case "manual-cancel":
			os.Exit(runManualCancel(os.Args[2:]))
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

	// Load config
	cfg, err := LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}
	if err := applyAlertThrottleFromConfig(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to apply alert throttle interval: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Loaded config: %d strategies, interval=%ds\n", len(cfg.Strategies), cfg.IntervalSeconds)

	// #1085: load the directional-certification artifact (SSoT for the
	// regime->direction edge gate). Fail-closed — a missing/malformed artifact
	// runs every regime_directional_policy strategy DEFAULT-OFF (base
	// direction), never a wrong-side bet, and never crashes the daemon.
	setDirectionalCertStore(LoadDirectionalCertSetFailClosed(directionalCertPath(), func(f string, a ...interface{}) {
		fmt.Fprintf(os.Stderr, f+"\n", a...)
	}))
	directionalCertSummaryLines := directionalCertStartupSummary(cfg)
	for _, line := range directionalCertSummaryLines {
		fmt.Println(line)
	}

	// #704: emit a one-line resolved summary per strategy so operators can
	// audit close/SL/TP wiring without grepping the JSON. Best-effort — a
	// failed re-read just means we can't mark explicit-vs-default but the
	// summary still shows the resolved source.
	explicitKeys, _ := loadStrategyExplicitKeys(*configPath)
	for _, sc := range cfg.Strategies {
		fmt.Println(formatStrategySummaryLine(sc, explicitKeys[sc.ID]))
	}
	// #1275: warn loudly when a configured open strategy carries the M5
	// fee-audit deprecate verdict (documented gross edge <= 0). Advisory only
	// — the config keeps loading and trading; the same lines are replayed to
	// the owner DM once the notifier is wired.
	deprecatedEdgeWarnings := deprecatedEdgeStartupWarnings(cfg.Strategies)
	for _, msg := range deprecatedEdgeWarnings {
		fmt.Fprintln(os.Stderr, "[config] "+msg)
	}
	// #1269: surface a configured daily loss limit at startup so the operator
	// can audit the portfolio-wide entry gate without grepping the JSON.
	if line := dailyLossStartupSummaryLine(cfg.PortfolioRisk); line != "" {
		fmt.Println(line)
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
		newPeak := rebaselinePortfolioPeakAfterPrune(state, cfg, nil)
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
	server.SetConfigContext(*configPath, cfg)
	// #1272: end single-threaded startup before the first state-reading
	// goroutine (http.Serve). ClearLatchedKillSwitchSharedWallet above must
	// stay before this call.
	markSchedulerStarted()
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

	// #1147 trade-quality diagnostics: eager row insert on every full close
	// (hook fires inside recordClosedPosition, under mu — same cost class as
	// the tradeRecorder insert), async MFE/MAE enrichment outside mu. The
	// worker's candle fetches ride runPythonReadOnly, so they cancel on drain.
	diagWorker := newTradeDiagnosticsWorker(FetchUICandles, stateDB.UpdateTradeDiagnosticsMetrics)
	diagWorker.UpdateStrategies(cfg.Strategies)
	tradeDiagnosticsRecorder = stateDB.InsertTradeDiagnostics
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

	// Initialize notification backends (Discord and/or Telegram).
	notifier, cleanupNotifier := buildNotifierFromConfig(cfg)
	defer cleanupNotifier()
	fmt.Printf("Notification backends: %d active\n", notifier.BackendCount())
	// #1257: the dashboard trade-action cores share the daemon notifier so
	// their protection warnings reach the operator like the manual CLI's do.
	server.SetNotifier(notifier)

	// #1137 LLM entry analysis: dedicated async lane (own queue + concurrency
	// cap, own per-job deadline — never the shared pythonSemaphore path). Rides
	// shutdownReadOnlyCtx so in-flight analyses are cancelled immediately at
	// SIGTERM instead of joining the side-effect drain. Advisory-only: the
	// verdict stamp and channel digest are the only outputs.
	llmWorker := newLLMEntryAnalysisWorker(
		runLLMEntryAnalysisScript,
		func(job llmEntryAnalysisJob, verdict string) {
			mu.Lock()
			defer mu.Unlock()
			s, ok := state.Strategies[job.StrategyID]
			if !ok {
				return
			}
			// Stamp only the exact position the analysis was dispatched for; a
			// close/flip in the interim means the verdict has no home (the
			// diagnostics row for that close keeps llm_verdict NULL).
			if pos := s.Positions[job.Symbol]; pos != nil && pos.TradePositionID == job.PositionID {
				pos.LLMVerdict = verdict
			}
		},
		func(job llmEntryAnalysisJob, res *LLMEntryAnalysisResult) {
			// Routing is per-strategy (#1137): DM on by default, channel off by
			// default. Both resolved into job.Params at dispatch so the async
			// digest honors the config in effect at open time.
			for _, route := range notifier.tradeAlertRoutes(job.Platform, job.StratType, job.IsLive) {
				// Per-route rendering mirrors sendTradeAlerts: plainText
				// backends (e.g. Telegram) must not see literal markdown
				// bold in the verdict line.
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

	// Attach Discord slash commands (issue #212). Non-fatal: registration
	// failures are logged + DM'd to the owner but never stop the daemon.
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

	// #1157: surface uncertified/expired directional policy to owner DM at startup.
	notifyDirectionalCertStartupSummary(notifier, directionalCertSummaryLines)

	// #1275: replay M5-deprecated-strategy warnings to the owner DM so an
	// operator live-trading a documented negative-gross-edge strategy sees it
	// even when not tailing stderr. One-time per startup; suppressed
	// per-strategy by allow_deprecated.
	if len(deprecatedEdgeWarnings) > 0 && notifier.HasOwner() {
		for _, msg := range deprecatedEdgeWarnings {
			notifier.SendOwnerDM("[config] " + msg)
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
	// shape before entering the trading loop. Exit ExitProbeFailure so
	// systemd RestartPreventExitStatus= stops restart spam (one DM, stay down).
	if err := probeCheckScripts(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "[startup] check-script compatibility probe failed: %v\n", err)
		if notifier != nil && notifier.HasOwner() {
			notifier.SendOwnerDM(fmt.Sprintf("**Startup probe failed** — refusing to start (exit %d; fix deploy, then restart):\n```\n%v\n```", ExitProbeFailure, err))
		}
		os.Exit(ExitProbeFailure)
	}

	// #849: Singleton guard — claim an exclusive lock on the resolved state-DB
	// path so a second daemon (e.g. an operator manually launching the binary
	// alongside the systemd-managed instance, or an out-of-cgroup process from a
	// signal-mode update) refuses to start instead of silently double-trading
	// against the same state.db / exchange account.
	//
	// Scoped to the persistent daemon loop only: CLI subcommands and --summary /
	// --leaderboard have already exited above, and --once is intentionally left
	// unguarded (it's update.sh's post-deploy smoke and the operator's
	// responsibility). The probe ran first because update.sh invokes the
	// `probe` subcommand against the still-live old daemon during a deploy;
	// that path never reaches here. The lock is a kernel flock the OS releases
	// on exit/crash, so a SIGKILLed daemon leaves nothing stale to block the
	// next start (see singleton_lock.go).
	if !*once {
		lock, lockErr := acquireStateDBLock(cfg.DBFile)
		if lockErr != nil {
			var locked *stateDBLockedError
			if errors.As(lockErr, &locked) {
				msg := fmt.Sprintf("CRITICAL: %s — refusing to start so this process can't double-trade against the same state DB. If no other go-trader is actually running, the lock auto-releases on exit; check `pgrep -af go-trader`.", locked.Error())
				fmt.Fprintln(os.Stderr, "[singleton] "+msg)
				if notifier != nil && notifier.HasOwner() {
					notifier.SendOwnerDM("**Singleton guard** — " + msg)
				}
			} else {
				fmt.Fprintf(os.Stderr, "[singleton] CRITICAL: could not acquire state DB lock: %v — refusing to start.\n", lockErr)
				if notifier != nil && notifier.HasOwner() {
					notifier.SendOwnerDM(fmt.Sprintf("**Singleton guard** — could not acquire state DB lock, refusing to start: %v", lockErr))
				}
			}
			os.Exit(ExitSingletonLock)
		}
		// Hold for the entire process lifetime. We deliberately do NOT release
		// on the happy path: the OS drops the flock when the process exits,
		// which is strictly after the deferred final SaveState + stateDB.Close
		// have run. Releasing earlier (e.g. via defer) would run BEFORE those
		// in LIFO order and open a window where a duplicate could grab the lock
		// and start trading while we're still flushing state. Stashing it in a
		// package var also keeps the fd reachable so its os.File finalizer
		// can't close (and release) it mid-run.
		heldStateDBLock = lock
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

		// #1147: refresh the diagnostics worker's strategy-ID → config
		// snapshot so post-reload closes resolve the right fetch metadata.
		diagWorker.UpdateStrategies(cfg.Strategies)

		// #1085: refresh the directional-certification artifact on SIGHUP so a
		// re-run of regime_1076_certify.py takes effect without a restart.
		// Fail-closed on error (keeps default-off). Certification status changes
		// never disturb an OPEN position — the entry gate keys on the live
		// verdict only when flat; open positions ride under their open stamp.
		setDirectionalCertStore(LoadDirectionalCertSetFailClosed(directionalCertPath(), func(f string, a ...interface{}) {
			fmt.Fprintf(os.Stderr, "[reload] "+f+"\n", a...)
		}))
		reloadCertLines := directionalCertStartupSummary(cfg)
		for _, line := range reloadCertLines {
			fmt.Printf("[reload] %s\n", line)
		}
		notifyDirectionalCertStartupSummary(notifier, reloadCertLines)

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
		channelTrades := make(map[string]int)
		channelTradeDetails := make(map[string][]string)

		// #569: Drain pending manual-open / manual-close actions before the
		// dueStrategies loop so newly-materialised positions are visible this cycle.
		mu.Lock()
		manualAlerts := drainPendingManualActions(state, cfg, stateDB)
		mu.Unlock()
		// #880: Emit trade alerts for adopted manual fills AFTER releasing mu.
		// sendTradeAlerts re-acquires mu.RLock, so alerting inside the drain
		// (which runs under mu.Lock) would self-deadlock. This gives manual fills
		// the same DM/channel routing as normal live trades.
		for _, ma := range manualAlerts {
			sendTradeAlerts(ma.sc, ma.ss, ma.trades, &mu, notifier)
		}

		// #883: poll resting limit orders, adopt fills into tracked positions
		// (arming protection immediately), and finalize filled/cancelled/expired
		// orders. Runs after the manual drain so a same-cycle market action and a
		// limit fill are both visible. Manages its own locking (network calls run
		// outside mu); alerts fire after, like the drain (#880).
		limitAlerts := reconcilePendingLimitOrders(state, cfg, stateDB, &mu, notifier, logMgr)
		for _, ma := range limitAlerts {
			sendTradeAlerts(ma.sc, ma.ss, ma.trades, &mu, notifier)
		}

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
				fmt.Println("[shutdown] exiting trading loop.")
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

		// totalPV holds the shared-wallet-adjusted portfolio value computed during
		// the portfolio risk check; it is reused in the cycle summary log below
		// so the summary doesn't double-count virtual cash in shared-wallet
		// setups (#908).
		var totalPV float64
		sharedWallets := detectSharedWallets(cfg.Strategies)
		// walletBalances is populated in the else branch below (where HL/OKX
		// state is fetched) and reused by the summary and leaderboard paths
		// that follow. Empty in the saveFailures>=3 fast-exit branch — those
		// paths fall back to virtual-sum as before.
		walletBalances := make(map[SharedWalletKey]float64)

		// Process only due strategies
		if saveFailures >= 3 {
			fmt.Println("[CRITICAL] State save failed 3x, skipping trades this cycle")
			// #879: the fan-out below is skipped, so clear the regime store —
			// /api/regime must not keep serving the prior cycle's labels as
			// fake-live while trading is suspended.
			globalRegimeStore.resetForCycle(time.Now().UTC())
			// #918: no balance fetch on this degenerate cycle, so clear any prior
			// exchange-derived shared-wallet values to avoid serving stale equity
			// from /status; display falls back to PortfolioValue.
			mu.Lock()
			for _, ss := range state.Strategies {
				if ss != nil {
					ss.SharedWalletValueSet = false
				}
			}
			mu.Unlock()
			mu.RLock()
			totalPV, _ = computeTotalPortfolioValue(cfg.Strategies, state, prices, nil, sharedWallets)
			mu.RUnlock()
		} else {
			// #879: kick off the per-cycle global regime store — one regime
			// subprocess per distinct (platform, symbol, timeframe, spec)
			// signature among due strategies. Runs CONCURRENTLY with the
			// portfolio risk / kill-switch phase below so a regime hang can
			// never delay risk management; regimeStoreReady() blocks (with a
			// phase budget) right before the check fan-out, the first store
			// consumer. Every regime consumer this cycle (entry gates,
			// stratState sync, stamp-at-open, check-script injection, flat
			// manual, options, dashboard) reads this map; check scripts no
			// longer compute regime inline.
			regimeStoreReady := startRegimeStorePopulation(globalRegimeStore, dueStrategies, cfg.Regime, notifier)
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
			dailyLossEntriesHeld := false
			usedPVFallback := false

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
			// Reconcile lists extend hlLive* to include type=manual: manual
			// positions are real on-chain HL positions that can be closed
			// externally and must be reconciled. Other hlLiveAll consumers
			// (kill-switch, trailing-stop, risk math) remain perps-only (#576).
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

			hlAddr := os.Getenv("HYPERLIQUID_ACCOUNT_ADDRESS")
			hlKey := SharedWalletKey{Platform: "hyperliquid", Account: hlAddr}
			_, hlShared := sharedWallets[hlKey]

			// Fetch HL clearinghouseState once if any consumer needs it:
			// - shared-wallet risk check (2+ live HL strategies in cfg)
			// - position sync for at least one due HL strategy
			// walletBalances is declared at cycle scope (above) and populated here.
			var hlPositions []HLPosition
			var hlStateFetched bool
			// hlSnapshotAt stamps when the accountValue/uPnL snapshot below was
			// taken; the #1100 cash-flow journal bounds its settled-event
			// ingestion to this instant so an in-flight fill cannot read as drift.
			var hlSnapshotAt time.Time
			// Fetch clearinghouseState whenever any live HL strategy exists (#356
			// per-strategy circuit closes need fresh positions even if no HL
			// strategy is due this cycle).
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

			// #954: pull funding payments + non-trade cash flows for each HL
			// shared wallet (two HTTP POSTs, outside the state lock). Booked
			// under the risk-phase write lock below, before the display
			// reconcile, so this cycle's ledger sums include them. Skipped
			// when the balance fetch failed — the reconcile skips the wallet
			// then too, and the watermark keeps the events for next cycle.
			var walletLedgerFetches []walletLedgerFetchResult
			if hlShared && hlStateFetched {
				walletLedgerFetches = append(walletLedgerFetches,
					fetchWalletLedgerEvents(stateDB, hlKey, time.Now().UTC()))
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
			// okxBalanceFetched / okxSnapshotAt mirror hlStateFetched / hlSnapshotAt:
			// the #1105 OKX cash-flow journal bounds its bill ingestion to the eq
			// snapshot instant so an in-flight bill cannot read as drift.
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
			// #360 phase 2 of #357: fetch the unified USDT balance for the
			// shared-wallet risk check when 2+ live OKX perps strategies share
			// an API key. Independent subprocess so a fetch_positions outage
			// doesn't starve the balance read.
			if okxHasCreds && okxShared {
				// #1105: one fetch_balance read yields a COHERENT (eq, uPnL) pair —
				// eq feeds the #918 split (walletBalances) unchanged, and the same
				// snapshot's uPnL (eq − cashBal) feeds the cash-flow journal, so its
				// expected-equity and reconciled eq cancel the uPnL term exactly
				// (no jitter from a separately-timed fetch_positions read).
				if eq, upnl, err := defaultOKXEquitySnapshot(); err != nil {
					fmt.Printf("[WARN] okx balance fetch failed: %v — falling back to per-wallet max this cycle\n", err)
				} else {
					walletBalances[okxKey] = eq
					okxBalanceFetched = true
					okxSnapshotUPnL = upnl
					okxSnapshotAt = time.Now().UTC()
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
			// #1106 phase 4 of #1100: fetch the TopStep account equity for the
			// shared-wallet cash-flow journal when 2+ live TopStep futures strategies
			// share an account. One fetch_topstep_balance.py read yields a COHERENT
			// (equity, uPnL) pair — both feed the SHADOW journal only.
			//
			// Unlike the HL/OKX blocks, the equity is NOT written into walletBalances:
			// the TopStep /v1/account/balance feed is unverified, and routing it into
			// computeTotalPortfolioValue would put it on the live all-platform kill
			// switch (CheckPortfolioRisk) behind only a >0 check. The journal uses its
			// own detectTopStepSharedWallet grouping (not the kill-switch sharedWallets
			// map), so portfolio risk stays on the pre-PR per-strategy member-PV path
			// until Phase 4b verifies the feed and promotes it.
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

			mu.RLock()
			totalPV, usedPVFallback = computeTotalPortfolioValue(cfg.Strategies, state, prices, walletBalances, sharedWallets)
			totalNotional := PortfolioNotional(state.Strategies, prices)
			// #296: aggregate perps margin drawdown inputs alongside the
			// equity total so the portfolio kill switch can fire on a
			// leveraged margin blow-up that would otherwise hide inside
			// equity-only drawdown for all-perps accounts.
			perpsLoss, perpsMargin := AggregatePerpsMarginInputs(state.Strategies, cfg.Strategies, prices)
			// #1269: evaluate the portfolio-wide daily loss limit once per
			// cycle (pure read — stale per-strategy days count as 0, matching
			// what rolloverDailyPnL would reset them to).
			dailyLossStatus := evaluateDailyLossLimit(cfg.PortfolioRisk, state.Strategies, time.Now().UTC())
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
			// #1269: hold position-increasing signals for the rest of the UTC
			// day once the aggregate daily realized loss reaches the limit.
			// Entry-suppression only — the manage-only paths below keep
			// running, nothing is force-closed, and the gate clears itself at
			// the UTC rollover (DailyPnL date keys roll per strategy). The
			// once-per-day owner DM fires after mu is released below (#880
			// convention: no notifier I/O under the state lock).
			dailyLossEntriesHeld = dailyLossStatus.Tripped
			if dailyLossEntriesHeld {
				fmt.Printf("[WARN] %s — entries held until UTC rollover\n", dailyLossHoldDetail(dailyLossStatus))
			}
			// #1291 review: a configured pct arm that cannot evaluate is an
			// auto-protective gap — surface it every cycle, not only in the
			// pull-based /status (even while the USD arm still enforces).
			if dailyLossStatus.PctBasisMiss {
				fmt.Printf("[WARN] %s\n", dailyLossPctBasisMissWarning)
			}
			// #954: book this cycle's funding payments + non-trade flows into
			// the ledger BEFORE the display reconcile reads the ledger sums —
			// the wallet balance being reconciled already includes them.
			ingestSharedWalletLedgers(stateDB, state, cfg.Strategies, sharedWallets, walletLedgerFetches)
			// #918/#954: shared-wallet display reconciliation. HL wallets derive
			// each member's display value from the local trades ledger
			// (initial_capital + ledger PnL + owned uPnL); the real balance is a
			// pure drift alarm. OKX wallets keep the #918 capital-weight split.
			// Runs under the same write lock the risk check holds (mutates
			// StrategyState.SharedWalletValue*).
			driftResults := reconcileSharedWalletDisplayValues(cfg.Strategies, state, stateDB, sharedWallets, walletBalances, hlPositions, okxPositions, okxStateFetched)
			mu.Unlock()

			// #1269: once-per-UTC-day owner DM on a tripped daily loss limit.
			// Outside mu (notifier I/O never runs under the state lock, #880).
			if dailyLossEntriesHeld {
				today := time.Now().UTC().Format("2006-01-02")
				if dailyLossAlertDue(true, dailyLossLastAlertDate, today) {
					dailyLossLastAlertDate = today
					notifier.SendOwnerDM(formatDailyLossTripDM(dailyLossStatus, time.Now().UTC()))
				}
			}
			// #1291 review: once-per-UTC-day owner DM while a configured pct
			// arm cannot evaluate (initial_capital basis is 0) — a silently
			// inert protection must reach an active operator channel.
			if dailyLossStatus.PctBasisMiss {
				today := time.Now().UTC().Format("2006-01-02")
				if dailyLossAlertDue(true, dailyLossPctBasisMissAlertDate, today) {
					dailyLossPctBasisMissAlertDate = today
					notifier.SendOwnerDM(formatDailyLossPctBasisMissDM(dailyLossStatus, time.Now().UTC()))
				}
			}

			// #1100: switch the HL shared-wallet drift alarm onto the
			// exchange-sourced cash-flow journal — the wallet TOTAL is
			// reconstructed from on-chain fills + funding + transfers instead of
			// the internal trade ledger, so modeled-fee / fallback-price / model-
			// only-cleanup rows no longer read as drift. Fails closed to the
			// trade-ledger drift when the journal is not usable (incomplete,
			// fetch miss, or operator opt-out via GO_TRADER_CASHFLOW_JOURNAL_ALARM).
			// Runs outside the lock: HTTP fetch + DB-only journal writes, no
			// StrategyState mutation. Reuses the reconcile's coherent accountValue
			// / position snapshot (walletBalances[hlKey] + hlPositions @
			// hlSnapshotAt) and bounds the journal to that snapshot. OKX/TopStep
			// keep the trade-ledger / capital-weight path untouched.
			if hlShared && hlStateFetched {
				rec := reconcileCashflowJournal(stateDB, hlKey, walletBalances[hlKey], sumHLAccountUPnL(hlPositions), hlSnapshotAt)
				applyCashflowJournalDriftBasis(driftResults, hlKey, rec, cashflowJournalAlarmEnabled())
			}

			// #1105: OKX cash-flow journal — SHADOW phase (Phase 3a of #1100). The
			// wallet TOTAL is reconstructed from OKX's account-bills feed (every
			// settled-cash movement is a bill carrying balChg) and reconciled
			// against eq, exactly as the HL journal does, but it ONLY logs the
			// journal-vs-capital-weight comparison each cycle — it never drives the
			// OKX drift alarm, which stays on the #918 capital-weight split.
			// Flipping the OKX alarm onto the journal is Phase 3b, gated on this
			// shadow log proving the OKX bills feed / eq field in production. Runs
			// outside the lock (subprocess fetch + DB-only writes) and is bounded to
			// the COHERENT eq/uPnL snapshot from the single balance read above
			// (walletBalances[okxKey] + okxSnapshotUPnL @ okxSnapshotAt) — no
			// positions dependency, so eq and uPnL share one instant.
			if okxShared && okxBalanceFetched {
				okxRec := reconcileOKXCashflowJournal(stateDB, okxKey, walletBalances[okxKey], okxSnapshotUPnL, okxSnapshotAt)
				logOKXCashflowJournalShadow(driftResults, okxKey, okxRec)
			}

			// #1106 phase 4 of #1100: TopStep cash-flow journal — SHADOW phase. The
			// wallet equity is reconstructed from TopStep's settled fills (gross realized
			// PnL − commission per fill) and reconciled against the account equity, exactly
			// as the HL/OKX journals do, but it ONLY logs each cycle — there is no live
			// TopStep shared-wallet alarm to drive (TopStep is display-skipped). Flipping a
			// TopStep alarm onto the journal is Phase 4b, gated on this shadow log proving
			// the feed/equity field AND the TopStepX endpoint contracts in production. Runs
			// outside the lock (subprocess fetch + DB-only writes), bounded to the COHERENT
			// equity/uPnL snapshot from the single balance read above.
			if tsShared && tsBalanceFetched {
				tsRec := reconcileTopStepCashflowJournal(stateDB, tsKey, tsSnapshotEquity, tsSnapshotUPnL, tsSnapshotAt)
				logTopStepCashflowJournalShadow(driftResults, tsKey, tsRec)
			}

			// Fire throttled drift alarms outside the lock (notifier I/O). For HL
			// this keys off the journal drift when the switch above applied it.
			reportSharedWalletDrift(notifier, driftResults)

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
						if pos, pok := ss.Positions[sym]; pok && pos != nil {
							hlSLOIDs[sym] = appendUniquePositiveStopLossOID(hlSLOIDs[sym], pos.StopLossOID)
							for _, tpOID := range pos.TPOIDs {
								hlSLOIDs[sym] = appendUniquePositiveStopLossOID(hlSLOIDs[sym], tpOID)
							}
						}
					}
				}
				hlVirtualQty = snapshotHyperliquidVirtualQuantities(state.Strategies, hlLiveAll)
				mu.RUnlock()

				inputs := KillSwitchCloseInputs{
					HLAddr:            hlAddr,
					HLStateFetched:    hlStateFetched,
					HLPositions:       hlPositions,
					HLLiveAll:         hlLiveAll,
					HLCloser:          defaultHyperliquidLiveCloser,
					HLFetcher:         defaultHLStateFetcher,
					HLNoFillRecoverer: defaultHLKillSwitchNoFillRecoverer,
					HLStopLossOIDs:    hlSLOIDs,
					OKXLiveAllPerps:   okxLivePerps,
					OKXLiveAllSpot:    okxLiveSpot,
					OKXCloser:         defaultOKXLiveCloser,
					OKXFetcher:        defaultOKXPositionsFetcher,
					RHLiveCrypto:      rhLiveCrypto,
					RHLiveOptions:     rhLiveOptions,
					RHCloser:          defaultRobinhoodLiveCloser,
					RHFetcher:         defaultRobinhoodPositionsFetcher,
					TSLiveAll:         tsLiveAll,
					TSCloser:          defaultTopStepLiveCloser,
					TSFetcher:         defaultTopStepPositionsFetcher,
					PortfolioReason:   portfolioReason,
					CloseTimeout:      90 * time.Second,
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
				warnNow := time.Now().UTC()
				var recentTrades []Trade
				if stateDB != nil {
					if rows, err := stateDB.RecentTrades(warnNow.Add(-portfolioWarningRecentWindow), portfolioWarningMaxRows); err != nil {
						fmt.Printf("[WARN] portfolio warning recent-trade lookup failed: %v\n", err)
					} else {
						recentTrades = rows
					}
				}
				var warnMsg string
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
				warnMsg = BuildPortfolioWarningMessage(PortfolioWarningMessageInputs{
					Reason:      portfolioReason,
					Config:      cfg.PortfolioRisk,
					State:       state,
					Prices:      prices,
					TotalValue:  totalPV,
					PerpsLoss:   perpsLoss,
					PerpsMargin: perpsMargin,
					Recent:      recentTrades,
					Now:         warnNow,
				})
				mu.Unlock()
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
				resetPrompt := formatKillSwitchResetPrompt(killSwitchInstanceLabel(*configPath), hlAddr, plan)
				go func() {
					defer func() { resetGoroutineRunning = false }()
					resp, err := notifier.AskOwnerDM(resetPrompt, 30*time.Minute)
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
				// #360: Live OKX per-strategy circuit breaker closes. Same shape
				// as the HL drain — the pending map is keyed per platform.
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
				// #362: Live TopStep per-strategy circuit breaker closes
				// (market_close full-flatten, sole-peer only — whole-contract
				// futures have no partial-close primitive). Outside-RTH
				// rejections and other TopStepX errors keep the pending
				// latched; the drain retries on the next cycle.
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
				// #361 phase 3: Live Robinhood crypto per-strategy circuit breaker
				// closes. RH crypto has no reduce-only primitive, so each pending
				// leg is a full-account market_sell guarded by a sole-ownership
				// gate (DM the owner when a shared-coin config prevents a safe
				// close). Lazy fetch — drain only calls the positions fetcher
				// when pending/stuck-CB work is present, so idle cycles skip the
				// TOTP login round-trip entirely.
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
				var hlReconcileFillHintsJSON []byte
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
					// #971: surface persistent shared-coin reconciliation gaps
					// (fail-closed residuals the reconciler could not confirm by
					// exact OID, leaving a phantom virtual position) to the
					// operator after a short confirmation window. Alerting only —
					// never books or guesses, so the fail-closed invariant holds.
					reportHLReconcileGaps(notifier, collectHLReconcileGapResults(state, &mu))
				}

				// #621: Build a coin→|on-chain qty| map from the pre-fetched positions
				// so SL placement can cap its size when virtual qty > on-chain qty
				// (e.g. after a manual TP reduced the position without the bot's knowledge).
				hlOnChainAbsQty := make(map[string]float64, len(hlPositions))
				for _, p := range hlPositions {
					sz := p.Size
					if sz < 0 {
						sz = -sz
					}
					if sz > 1e-9 {
						hlOnChainAbsQty[p.Coin] = sz
					}
				}

				// #879: the dispatch loop below is the first regime-store
				// consumer — wait (bounded by regimeStorePhaseBudget) for the
				// population kicked off before the risk phase.
				regimeStoreReady()
				// #1224: persist per-window labels, detect transitions, and
				// alert on cross-window reversals. Sequential main loop,
				// outside mu; fail-open — never blocks the dispatch below.
				processRegimeTransitionAlerts(stateDB, globalRegimeStore, cfg.Regime, notifier, time.Now().UTC())
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
							spotPosCtx = positionCtxForSymbol(stratState, sym, sc, cfg.Regime)
						}
					}
					var hlCash float64
					var hlPosQty float64
					var hlPosSide string
					var hlAvgCost float64
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
					var hlProfileState *RegimeProfileState
					if sc.Type == "perps" && sc.Platform == "hyperliquid" {
						if hlLiveStrategy {
							hlCash = stratState.Cash
						}
						// #998: snapshot the regime-profile switch state under the
						// Phase-1 RLock so the lock-free resolution reads a stable copy.
						if stratState.RegimeProfile != nil {
							cp := *stratState.RegimeProfile
							hlProfileState = &cp
						}
						// #873: scale-in's default per-add notional uses the
						// strategy cash like a fresh open — captured for paper too
						// (hlCash above is live-only), under the same RLock.
						hlScaleInCash = stratState.Cash
						// Live-order sizing/cancel snapshots below are intentionally
						// consumed only inside live execution branches. Paper paths
						// should continue using PositionCtx only for close evaluation.
						if sym := hyperliquidSymbol(sc.Args); sym != "" {
							if pos, ok := stratState.Positions[sym]; ok {
								hlPosCtx = positionCtxForCheck(sc, pos, cfg.Regime)
								hlPosSide = hlPosCtx.Side
								hlPosQty = hlPosCtx.Quantity
								hlAvgCost = hlPosCtx.AvgCost
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
								okxPosCtx = positionCtxForCheck(sc, pos, cfg.Regime)
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

					// Phase 2: Lock — CheckRisk (fast, no I/O)
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
					// #1046: a latched per-strategy circuit breaker on an HL perps
					// strategy with an open position falls through in manage-only
					// mode rather than skipping — the dispatch below forces the
					// signal to hold (0), which suppresses every entry/add/flip/
					// close path while still running the Signal==0 trailing-SL/TP
					// management so a stranded (e.g. shared-coin) position keeps
					// ratcheting its stop-loss. All other blocks skip as before.
					cbManageOnly := false
					if !allowed {
						notifyPerStrategyCircuitBreakerWithSnapshot(sc, cbSnapshot, reason, pv, totalPV, stateDB, notifier, killSwitchFired)
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

					// #42: Notional cap blocks new trades for this strategy. Under
					// manage-only the position is never grown anyway, so let the
					// trailing-SL/TP management run instead of skipping.
					if !cbManageOnly && notionalBlocked {
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
							if result, signalStr, price, ok := runOKXCheck(sc, prices, okxPosCtx, cfg.Regime, notifier, logger); ok {
								prices[result.Symbol] = price
								// #879: single-source regime — read the global store for this
								// strategy's signature instead of the check output, and point
								// result.Regime at it so stamp-at-open inside execute* shares it.
								storeRegime := globalRegimeStore.PayloadForStrategy(sc, cfg.Regime)
								result.Regime = &storeRegime
								if gateRegime, regimeBlocked := applyRegimeGate(sc, storeRegime, cfg.Regime, okxPosQty); regimeBlocked {
									logger.Info("Regime gate: open signal blocked (regime=%s)", gateRegime)
									result.Signal = 0
								}
								// #1150: paused — hold position-increasing signals (fresh open, add,
								// flip); position-reducing actions pass so open positions ride their
								// natural exit. The Signal==0 manage path below keeps running.
								if sc.Paused && pausedBlocksSignal(result.Signal, result.CloseFraction, okxPosQty, okxPosSide, true, false) {
									logger.Info("Paused: %s signal suppressed — position-increasing actions held while paused (#1150)", signalStr)
									result.Signal = 0
								}
								// #1269: daily loss limit tripped — identical hold semantics to pause:
								// position-increasing signals held, position-reducing actions pass.
								if dailyLossEntriesHeld && pausedBlocksSignal(result.Signal, result.CloseFraction, okxPosQty, okxPosSide, true, false) {
									logger.Info("Daily loss limit: %s signal suppressed — entries held until UTC rollover (#1269)", signalStr)
									result.Signal = 0
								}
								mu.Lock()
								syncStrategyRegimeState(stratState, storeRegime, cfg.Regime)
								mu.Unlock()
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
									trades, detail = executeOKXResult(sc, stratState, stateDB, result, execResult, signalStr, price, cfg.Regime, logger)
									mu.Unlock()
								}
							}
						} else if sc.Platform == "robinhood" {
							if result, signalStr, price, ok := runRobinhoodCheck(sc, prices, rhPosCtx, cfg.Regime, notifier, logger); ok {
								prices[result.Symbol] = price
								// #879: single-source regime — read the global store for this
								// strategy's signature instead of the check output, and point
								// result.Regime at it so stamp-at-open inside execute* shares it.
								storeRegime := globalRegimeStore.PayloadForStrategy(sc, cfg.Regime)
								result.Regime = &storeRegime
								if gateRegime, regimeBlocked := applyRegimeGate(sc, storeRegime, cfg.Regime, rhPosQty); regimeBlocked {
									logger.Info("Regime gate: open signal blocked (regime=%s)", gateRegime)
									result.Signal = 0
								}
								// #1150: paused — hold position-increasing signals (fresh open, add,
								// flip); position-reducing actions pass so open positions ride their
								// natural exit. The Signal==0 manage path below keeps running.
								if sc.Paused && pausedBlocksSignal(result.Signal, result.CloseFraction, rhPosQty, rhPosSide, true, false) {
									logger.Info("Paused: %s signal suppressed — position-increasing actions held while paused (#1150)", signalStr)
									result.Signal = 0
								}
								// #1269: daily loss limit tripped — identical hold semantics to pause:
								// position-increasing signals held, position-reducing actions pass.
								if dailyLossEntriesHeld && pausedBlocksSignal(result.Signal, result.CloseFraction, rhPosQty, rhPosSide, true, false) {
									logger.Info("Daily loss limit: %s signal suppressed — entries held until UTC rollover (#1269)", signalStr)
									result.Signal = 0
								}
								mu.Lock()
								syncStrategyRegimeState(stratState, storeRegime, cfg.Regime)
								mu.Unlock()
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
									trades, detail = executeRobinhoodResult(sc, stratState, stateDB, result, execResult, signalStr, price, cfg.Regime, logger)
									mu.Unlock()
								}
							}
						} else if result, signalStr, price, ok := runSpotCheck(sc, prices, spotPosCtx, cfg.Regime, notifier, logger); ok {
							// #879: single-source regime — read the global store for this
							// strategy's signature instead of the check output, and point
							// result.Regime at it so stamp-at-open inside execute* shares it.
							storeRegime := globalRegimeStore.PayloadForStrategy(sc, cfg.Regime)
							result.Regime = &storeRegime
							if gateRegime, regimeBlocked := applyRegimeGate(sc, storeRegime, cfg.Regime, spotPosCtx.Quantity); regimeBlocked {
								logger.Info("Regime gate: open signal blocked (regime=%s)", gateRegime)
								result.Signal = 0
							}
							// #1150: paused — hold position-increasing signals (fresh open, add,
							// flip); position-reducing actions pass so open positions ride their
							// natural exit. The Signal==0 manage path below keeps running.
							if sc.Paused && pausedBlocksSignal(result.Signal, result.CloseFraction, spotPosCtx.Quantity, spotPosCtx.Side, true, false) {
								logger.Info("Paused: %s signal suppressed — position-increasing actions held while paused (#1150)", signalStr)
								result.Signal = 0
							}
							// #1269: daily loss limit tripped — identical hold semantics to pause:
							// position-increasing signals held, position-reducing actions pass.
							if dailyLossEntriesHeld && pausedBlocksSignal(result.Signal, result.CloseFraction, spotPosCtx.Quantity, spotPosCtx.Side, true, false) {
								logger.Info("Daily loss limit: %s signal suppressed — entries held until UTC rollover (#1269)", signalStr)
								result.Signal = 0
							}
							mu.Lock()
							syncStrategyRegimeState(stratState, storeRegime, cfg.Regime)
							trades, detail = executeSpotResult(sc, stratState, stateDB, result, signalStr, price, cfg.Regime, logger)
							mu.Unlock()
						}
					case "options":
						if result, signalStr, ok := runOptionsCheck(sc, posJSON, notifier, logger); ok {
							// #1150: paused — drop open actions ("buy"/"sell" both OPEN
							// option legs); "close" actions and the theta-harvest walker
							// below still manage existing positions.
							if sc.Paused {
								kept, dropped := pausedOptionsActions(result.Actions)
								if dropped > 0 {
									logger.Info("Paused: %d option open action(s) dropped — close actions still execute (#1150)", dropped)
								}
								result.Actions = kept
							}
							// #1269: daily loss limit tripped — drop option open actions
							// ("buy"/"sell" both OPEN legs); close actions and the
							// theta-harvest walker still manage existing positions.
							if dailyLossEntriesHeld {
								kept, dropped := pausedOptionsActions(result.Actions)
								if dropped > 0 {
									logger.Info("Daily loss limit: %d option open action(s) dropped — entries held until UTC rollover (#1269)", dropped)
								}
								result.Actions = kept
							}
							// #879: options regime now comes from the global store's
							// (underlying, 4h, ADX-default) bundle instead of the
							// check script's inline fetch; the injected payload keeps
							// the script's own emitted label identical.
							optionsRegime := globalRegimeStore.PayloadForStrategy(sc, cfg.Regime)
							mu.Lock()
							stratState.Regime = optionsRegime.PrimaryLabel(nil)
							var harvestDetails []string
							trades, detail, harvestDetails = executeOptionsResult(sc, stratState, result, signalStr, logger)
							mu.Unlock()
							if chKey := notifier.resolveChannelKey(sc.Platform, sc.Type); chKey != "" {
								key := chKey + "|" + extractAsset(sc)
								channelTradeDetails[key] = append(channelTradeDetails[key], harvestDetails...)
							}
						}
					case "perps":
						// #998: regime-profile allocation resolves BEFORE the check
						// subprocess so the active profile's params shape the signal
						// itself (the merged params ride the --strategy-refs JSON).
						// HL perps only; the switch reads the global regime store at
						// the configured long window and the closed-bar hysteresis
						// counter advances only when the bundle's BarTime moves.
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
							if result, signalStr, price, ok := runOKXCheck(sc, prices, okxPosCtx, cfg.Regime, notifier, logger); ok {
								prices[result.Symbol] = price
								// #879: single-source regime — read the global store for this
								// strategy's signature instead of the check output, and point
								// result.Regime at it so stamp-at-open inside execute* shares it.
								storeRegime := globalRegimeStore.PayloadForStrategy(sc, cfg.Regime)
								result.Regime = &storeRegime
								if gateRegime, regimeBlocked := applyRegimeGate(sc, storeRegime, cfg.Regime, okxPosQty); regimeBlocked {
									logger.Info("Regime gate: open signal blocked (regime=%s)", gateRegime)
									result.Signal = 0
								}
								// #1150: paused — hold position-increasing signals (fresh open, add,
								// flip); position-reducing actions pass so open positions ride their
								// natural exit. The Signal==0 manage path below keeps running.
								if sc.Paused && pausedBlocksSignal(result.Signal, result.CloseFraction, okxPosQty, okxPosSide, PerpsAllowsLong(sc), PerpsAllowsShort(sc)) {
									logger.Info("Paused: %s signal suppressed — position-increasing actions held while paused (#1150)", signalStr)
									result.Signal = 0
								}
								// #1269: daily loss limit tripped — identical hold semantics to pause:
								// position-increasing signals held, position-reducing actions pass.
								if dailyLossEntriesHeld && pausedBlocksSignal(result.Signal, result.CloseFraction, okxPosQty, okxPosSide, PerpsAllowsLong(sc), PerpsAllowsShort(sc)) {
									logger.Info("Daily loss limit: %s signal suppressed — entries held until UTC rollover (#1269)", signalStr)
									result.Signal = 0
								}
								mu.Lock()
								syncStrategyRegimeState(stratState, storeRegime, cfg.Regime)
								mu.Unlock()
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
									trades, detail = executeOKXResult(sc, stratState, stateDB, result, execResult, signalStr, price, cfg.Regime, logger)
									mu.Unlock()
								}
							}
						} else if result, signalStr, price, ok := runHyperliquidCheck(&sc, prices, hlPosCtx, cfg.Regime, notifier, logger); ok {
							prices[result.Symbol] = price
							// #1046: circuit breaker latched — force hold so no entry/
							// add/flip/close executes (every execution path below gates
							// on Signal != 0, and executePerpsSignalWithLeverage returns
							// early on signal==0). The Signal==0 trailing-SL/TP-ratchet
							// management blocks below still run, keeping the open
							// position's stop-loss ratcheting through the latch window.
							if cbManageOnly {
								result.Signal = 0
							}
							// #879: single-source regime — read the global store for this
							// strategy's signature instead of the check output, and point
							// result.Regime at it so stamp-at-open inside execute* shares it.
							storeRegime := globalRegimeStore.PayloadForStrategy(sc, cfg.Regime)
							result.Regime = &storeRegime
							if gateRegime, regimeBlocked := applyRegimeGate(sc, storeRegime, cfg.Regime, hlPosQty); regimeBlocked {
								logger.Info("Regime gate: open signal blocked (regime=%s)", gateRegime)
								result.Signal = 0
							}
							// #1150: paused — hold position-increasing signals (fresh open, add,
							// flip); position-reducing actions pass so open positions ride their
							// natural exit. The Signal==0 manage path below keeps running.
							if sc.Paused && pausedBlocksSignal(result.Signal, result.CloseFraction, hlPosQty, hlPosSide, PerpsAllowsLong(sc), PerpsAllowsShort(sc)) {
								logger.Info("Paused: %s signal suppressed — position-increasing actions held while paused (#1150)", signalStr)
								result.Signal = 0
							}
							// #1269: daily loss limit tripped — identical hold semantics to pause:
							// position-increasing signals held, position-reducing actions pass.
							if dailyLossEntriesHeld && pausedBlocksSignal(result.Signal, result.CloseFraction, hlPosQty, hlPosSide, PerpsAllowsLong(sc), PerpsAllowsShort(sc)) {
								logger.Info("Daily loss limit: %s signal suppressed — entries held until UTC rollover (#1269)", signalStr)
								result.Signal = 0
							}
							mu.Lock()
							syncStrategyRegimeState(stratState, storeRegime, cfg.Regime)
							// #907: update per-strategy divergence state after regime sync.
							// result.Divergence is populated by runHyperliquidCheck when
							// regime_window_divergence is configured on sc.
							updateStrategyDivergenceState(stratState, result.Divergence)
							mu.Unlock()
							var execResult *HyperliquidExecuteResult
							liveExecFailed := false
							if result.Signal == 0 && hlPosQty > 0 && strategyUsesTrailingTPRatchetClose(sc) {
								ratchetAlert := applyTrailingTPRatchet(sc, stratState, result.Symbol, price, &mu, logger)
								notifyRatchetTrigger(notifier, sc.NotifyRatchetTriggersEnabled(cfg), ratchetAlert)
								mu.RLock()
								if pos, ok3 := stratState.Positions[result.Symbol]; ok3 && pos != nil {
									hlPosSnapshot = hyperliquidProtectionPositionSnapshot(pos)
								}
								mu.RUnlock()
							}
							if result.Signal == 0 && hlPosQty > 0 && atrMultMissingEntryATR(sc, hlPosSnapshot) {
								// ATR-mult is configured but the position is missing EntryATR — the
								// open candle did not produce an ATR indicator, so the trailing loop
								// will keep no-opping until the position closes. Emit a one-shot
								// operator alert; the position is running unprotected. Fires for
								// both live and paper since paper now runs the same trailing loop
								// (#532).
								notifyATRMultMissingEntryATROnce(sc, result.Symbol, notifier, logger)
							}
							if result.Signal == 0 && hlPosQty > 0 && tieredTPATRMissingEntryATR(sc, hlPosSnapshot) {
								notifyTieredTPATRMissingEntryATROnce(sc, result.Symbol, notifier, logger)
							}
							if !hyperliquidIsLive(sc.Args) && result.Signal == 0 && hlPosQty > 0 && effectiveTrailingStopPct(sc, hlPosSnapshot) > 0 {
								// Paper-mode trailing stop (#532): no exchange trigger order; the
								// scheduler advances the high-water mark each cycle and books a
								// synthetic close when mark crosses the trigger. Each strategy's
								// virtual position is isolated in stratState.Positions, so peers
								// on the same coin are unaffected by this strategy's breach.
								newHighWater, newTrigger, breach, breachPx := runHyperliquidTrailingStopPaper(sc, hlPosSide, hlPosSnapshot, price, hlStopLossHighWaterPx, hlStopLossTriggerPx)
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
								// #873: after a scale-in grew the position, force a
								// cancel+replace once the reconcile has confirmed the new
								// on-chain size (!capped) so the trailing SL covers the
								// new total without waiting for a trailing trigger move.
								forceResize := hlScaleInResizePending && !capped
								newHighWater, slUpdate, updateConfirmed := runHyperliquidTrailingStopUpdate(sc, result.Symbol, hlPosSide, slEffectiveQty, hlPosSnapshot, price, hlStopLossHighWaterPx, hlStopLossTriggerPx, hlStopLossOID, forceResize, notifier, logger)
								mu.Lock()
								if immediateFill, fillPx := applyTrailingStopUpdateResult(stratState, result.Symbol, hlPosSide, hlStopLossOID, newHighWater, updateConfirmed, slUpdate, logger); immediateFill {
									trades++
									detail = fmt.Sprintf("[%s] LIVE TRAILING SL %s @ $%.2f", sc.ID, result.Symbol, fillPx)
								}
								// The trailing walker owns the SL re-size whenever it fires
								// (this block only runs when effectiveTrailingStopPct > 0), so
								// it always clears the flag — including when the strategy also
								// places on-chain TPs (the sync re-sizes those but defers the
								// flag clear to here, #882 review). The sync clears the flag
								// only for non-trailing SL owners.
								if forceResize && updateConfirmed {
									if p, ok := stratState.Positions[result.Symbol]; ok && p != nil {
										p.ScaleInResizePending = false
									}
								}
								mu.Unlock()
							}
							// #562: Fixed ATR-derived stop-loss arming. EffectiveStopLossPct
							// returns 0 at order-placement time when StopLossATRMult > 0, so
							// the open leg lands without a trigger. On the cycle after open
							// (Signal == 0, position exists, EntryATR stamped, no OID/trigger
							// yet) we arm a one-shot fixed trigger at AvgCost ± mult*EntryATR.
							// Subsequent cycles short-circuit because the position carries an
							// OID (live) or TriggerPx (paper) — the trigger is fixed for the
							// life of the position and never re-armed.
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
										stampOpenTradeWithProtectionSnapshot(stratState, stateDB, sc, result.Symbol, pos)
										logger.Info("Paper fixed ATR SL armed @ $%.4f (%.2f%% from entry $%.4f)",
											newTrigger, effectiveFixedStopLossATRPct(sc, hlPosSnapshot), pos.AvgCost)
									}
								}
								mu.Unlock()
							}
							if hyperliquidIsLive(sc.Args) && result.Signal == 0 && hlPosQty > 0 && sc.StopLossATRMult != nil && *sc.StopLossATRMult > 0 && hlStopLossOID == 0 {
								triggerPx := fixedStopLossATRTriggerPx(sc, hlPosSide, hlPosSnapshot)
								if triggerPx > 0 {
									slEffectiveQty, capped := hlSLEffectiveQty(result.Symbol, hlPosQty, hlOnChainAbsQty)
									if capped {
										logger.Warn("fixed ATR SL arm: virtual qty %.6f > on-chain %.6f for %s; capping SL size to on-chain qty (#621)", hlPosQty, slEffectiveQty, result.Symbol)
									}
									slResult, ok2 := hyperliquidArmFixedATRStopLossLive(sc, result.Symbol, hlPosSide, slEffectiveQty, triggerPx, notifier, logger)
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
												stampOpenTradeWithProtectionSnapshot(stratState, stateDB, sc, result.Symbol, pos)
												logger.Info("Fixed ATR SL armed oid=%d @ $%.4f", slResult.StopLossOID, slResult.StopLossTriggerPx)
											}
										}
										mu.Unlock()
									}
								}
							}
							if hyperliquidIsLive(sc.Args) && result.Signal == 0 && hlPosQty > 0 {
								runHyperliquidProtectionSync(sc, stratState, stateDB, result.Symbol, &mu, notifier, logger, "HL protection synced", hlReconcileFillHintsJSON)
								runPostTPStopLossAdjustment(sc, stratState, result.Symbol, price, cfg, &mu, notifier, logger, hlOnChainAbsQty)
							}
							// #873 scale-in: a same-direction signal on an open HL
							// perps position ADDS size when allow_scale_in is
							// enabled and the caps/spacing allow. Computed once
							// from the Phase-1 snapshot so the live order and the
							// Trade record agree (closing the #298 fill-without-
							// Trade gap). Applies to live and paper; paper just
							// blends (no on-chain order / re-size).
							scaleInAddQty := 0.0
							if result.Signal != 0 && sc.Type == "perps" && sc.AllowScaleIn {
								defOpenNotional := PerpsOpenNotional(hlScaleInCash, EffectiveSizingLeverage(sc), EffectiveExchangeLeverage(sc), EffectiveMarginPerTradeUSD(sc))
								snap := scaleInSnapshot{Side: hlPosSide, Quantity: hlPosQty, AvgCost: hlAvgCost, EntryATR: hlEntryATR, ScaleInCount: hlScaleInCount, AddedNotionalUSD: hlAddedNotionalUSD, LastAddPrice: hlLastAddPrice}
								if q, okAdd, reason := perpsScaleInDecision(sc, snap, result.Signal, price, defOpenNotional); okAdd {
									scaleInAddQty = q
								} else if reason != "" && reason != "not a same-direction add" {
									logger.Info("Scale-in not taken for %s: %s", result.Symbol, reason)
								}
							}
							if hyperliquidIsLive(sc.Args) && result.Signal != 0 {
								// #768 fix #4: Forward Go's clearinghouseState
								// snapshot for this coin so Python can skip its
								// duplicate get_position_leverage /info call.
								// Only meaningful when a peer/prior position
								// has the leverage already pinned on-chain.
								walletSnapshot := hlExecuteSnapshotForCoin(hlPositions, result.Symbol)
								if scaleInAddQty > 0 {
									// Add path: same-side market order; no SL/TP
									// cancel, no update_leverage. The post-add
									// protection sync re-sizes SL + un-cleared TPs.
									if er, ok2 := runHyperliquidScaleInOrder(sc, result, scaleInAddQty, walletSnapshot, notifier, logger); ok2 {
										execResult = er
									} else {
										liveExecFailed = true
									}
								} else {
									er, ok2 := runHyperliquidExecuteOrder(sc, result, price, hlCash, hlPosQty, hlPosSide, hlAvgCost, hlStopLossOID, hlTPOIDs, hlReconcileAll, walletSnapshot, notifier, logger)
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
							}
							if !liveExecFailed {
								mu.Lock()
								var openTrade *Trade
								var ratchetAlert *RatchetTriggerAlert
								if scaleInAddQty > 0 {
									trades, detail, openTrade = executeHyperliquidScaleInDeferredOpen(sc, stratState, result, execResult, signalStr, price, scaleInAddQty, logger)
								} else {
									trades, detail, openTrade, ratchetAlert = executeHyperliquidResultDeferredOpen(sc, stratState, result, execResult, signalStr, price, cfg.Regime, logger)
								}
								mu.Unlock()
								// #1110: deliver any ratchet-tighten DM after releasing the lock
								// (Discord/Telegram HTTP must not run under mu). Nil-safe no-op
								// for the scale-in branch and when no tier tightened.
								notifyRatchetTrigger(notifier, sc.NotifyRatchetTriggersEnabled(cfg), ratchetAlert)
								if execResult != nil && trades > 0 {
									runHyperliquidProtectionSync(sc, stratState, stateDB, result.Symbol, &mu, notifier, logger, "HL protection synced after trade", hlReconcileFillHintsJSON)
									runPostTPStopLossAdjustment(sc, stratState, result.Symbol, price, cfg, &mu, notifier, logger, hlOnChainAbsQty)
									// #873/#882: for a trailing-SL owner the post-trade sync
									// re-sized only the on-chain TPs (the walker owns the SL).
									// Grow the trailing SL NOW via the same walker primitive
									// instead of deferring to the next Signal==0 cycle, so a
									// scale-in's added size is covered immediately even on a
									// long strategy interval. No-op for non-trailing owners
									// (their SL was already re-sized + flag cleared by the sync)
									// and when the add wasn't a scale-in (flag unset).
									if scaleInAddQty > 0 {
										filledAddQty := scaleInAddQty
										if execResult.Execution != nil && execResult.Execution.Fill != nil && execResult.Execution.Fill.TotalSz > 0 {
											filledAddQty = execResult.Execution.Fill.TotalSz
										}
										if extraTrades, slDetail := scaleInResizeTrailingSLNow(sc, stratState, result.Symbol, price, hlOnChainAbsQty, filledAddQty, &mu, notifier, logger); extraTrades > 0 {
											trades += extraTrades
											detail = slDetail
										}
									} else {
										// #885: arm the initial trailing SL inline on the open
										// cycle for ATR-trailing owners. They get no inline SL at
										// the execute order (EffectiveStopLossPct defers to 0) and
										// the sync above doesn't place one (the plan never reads
										// the trailing fields), so without this they stay naked
										// until the next Signal==0 walker cycle. No-op for every
										// other owner and for close/partial-close legs (the
										// helper's internal guards: live + trailing owner + no
										// resting SL).
										filledQty := 0.0
										if execResult.Execution != nil && execResult.Execution.Fill != nil && execResult.Execution.Fill.TotalSz > 0 {
											filledQty = execResult.Execution.Fill.TotalSz
										}
										if extraTrades, slDetail := armTrailingStopAtOpenNow(sc, stratState, result.Symbol, price, hlOnChainAbsQty, filledQty, &mu, notifier, logger); extraTrades > 0 {
											trades += extraTrades
											detail = slDetail
										}
									}
									mu.Lock()
									var pos *Position
									if p, ok := stratState.Positions[result.Symbol]; ok {
										pos = p
									}
									recordPositionOpen(stratState, sc, openTrade, pos)
									mu.Unlock()
								}
							}
							// #998: stamp the active profile on a freshly opened
							// position (freezes it for the position's life) and commit
							// the resolved switch state. Runs whenever the check
							// succeeded — independent of whether a trade executed — so
							// the flat hysteresis counter advances every cycle.
							if hlProfileResolved {
								mu.Lock()
								stampPositionProfileIfOpened(stratState, result.Symbol, hlProfileActive)
								updateStrategyProfileState(stratState, hlProfileNext)
								mu.Unlock()
							}
						}
					case "futures":
						if result, signalStr, price, ok := runTopStepCheck(sc, prices, tsPosCtx, cfg.Regime, notifier, logger); ok {
							prices[result.Symbol] = price
							// #879: single-source regime — read the global store for this
							// strategy's signature instead of the check output, and point
							// result.Regime at it so stamp-at-open inside execute* shares it.
							storeRegime := globalRegimeStore.PayloadForStrategy(sc, cfg.Regime)
							result.Regime = &storeRegime
							if gateRegime, regimeBlocked := applyRegimeGate(sc, storeRegime, cfg.Regime, tsContracts); regimeBlocked {
								logger.Info("Regime gate: open signal blocked (regime=%s)", gateRegime)
								result.Signal = 0
							}
							// #1150: paused — hold position-increasing signals (fresh open, add,
							// flip); position-reducing actions pass so open positions ride their
							// natural exit. The Signal==0 manage path below keeps running.
							// allowsLong=allowsShort=true: ExecuteFuturesSignalWithFillFee is
							// unconditionally bidirectional — a sell on a long (closeFraction
							// 0) closes AND opens a fresh short ("Open short … after closing
							// long"), and a buy on a short mirrors it — so an opposite-side
							// signal is never a pure close; only close-registry actions
							// (closeFraction>0) reduce without reopening.
							if sc.Paused && pausedBlocksSignal(result.Signal, result.CloseFraction, tsContracts, tsPosSide, true, true) {
								logger.Info("Paused: %s signal suppressed — position-increasing actions held while paused (#1150)", signalStr)
								result.Signal = 0
							}
							// #1269: daily loss limit tripped — identical hold semantics to pause:
							// position-increasing signals held, position-reducing actions pass.
							if dailyLossEntriesHeld && pausedBlocksSignal(result.Signal, result.CloseFraction, tsContracts, tsPosSide, true, true) {
								logger.Info("Daily loss limit: %s signal suppressed — entries held until UTC rollover (#1269)", signalStr)
								result.Signal = 0
							}
							mu.Lock()
							syncStrategyRegimeState(stratState, storeRegime, cfg.Regime)
							mu.Unlock()
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
								trades, detail = executeTopStepResult(sc, stratState, stateDB, result, execResult, signalStr, price, cfg.Regime, logger)
								mu.Unlock()
							}
						}
					case "manual":
						// #569: manual strategies have no open signal; only run
						// close evaluators when a position is open.
						mu.RLock()
						pos := stratState.Positions[sc.Symbol]
						mu.RUnlock()
						if pos != nil && !manualPositionOwnedByStrategy(pos, sc.ID) {
							logger.Error("manual position owner mismatch for %s/%s: owner_strategy_id=%q", sc.ID, sc.Symbol, pos.OwnerStrategyID)
							break
						}
						if pos != nil && hyperliquidIsLive(sc.Args) {
							// Protection-sync must run before the close evaluator so
							// the latter sees on-chain-reconciled qty. Post-TP SL
							// adjustment is deferred to the post-stamp block below so
							// regime-keyed *_atr_regime SL sees pos.Regime (#878 review).
							runHyperliquidProtectionSync(sc, stratState, stateDB, sc.Symbol, &mu, notifier, logger, "HL manual protection synced", hlReconcileFillHintsJSON)
						}
						// #872: run the close evaluator and stamp the regime BEFORE
						// arming the post-TP SL / ratchet / trailing walker below. The
						// regime is the close-eval's classifier output, and the
						// regime-keyed close features (trailing_tp_ratchet_regime,
						// *_atr_regime SL/TP) resolve their tier table / trail distance
						// from pos.Regime. Arming first (the previous order) left
						// pos.Regime == "" on the first post-open cycle, so the regime
						// trail no-op'd for one interval; stamping first arms it
						// correctly on cycle 1.
						closeFraction, _, manualOK := runManualCloseEval(sc, stratState, cfg, notifier, logger)
						// #879: the live regime comes from the global store, not the
						// close-eval's check output — so a FLAT manual strategy now
						// shows a live regime too (pre-#879 the HL check only ran
						// with an open position, leaving regime=- while flat).
						// If the bundle failed on the very cycle a manual position
						// opened, the stamp below is an empty no-op and regime-keyed
						// closes stay unarmed until a later cycle's bundle succeeds —
						// the stamp is idempotent on pos.Regime == "", so it
						// self-heals (documented fail-open tradeoff).
						manualRegime := globalRegimeStore.PayloadForStrategy(sc, cfg.Regime)
						if manualOK {
							mu.Lock()
							// Refresh the strategy-level live regime every cycle, like
							// the other five dispatches do — this is what the Phase 6
							// status line and dashboard read (stratState.Regime). Without
							// it a manual strategy reports regime=- even with an open,
							// correctly-stamped position (#872 review).
							syncStrategyRegimeState(stratState, manualRegime, cfg.Regime)
							// Stamp the current regime onto the position the first
							// time we observe one. Idempotent — stampPosition-
							// RegimeFromPayload only writes when pos.Regime == "" (and
							// pos.RegimeWindows is empty) — so this fires exactly once,
							// on the first close-eval cycle after open, regardless of
							// live vs --record-only.
							stampPositionRegimeIfOpened(stratState, sc.Symbol, manualRegime, sc, cfg.Regime)
							mu.Unlock()
						}
						// #1115: alert (once per strategy/symbol) when an open manual
						// position drifted across a close-evaluator default flip — opened
						// under a tiered-TP close (resting TP OIDs) but the strategy now
						// resolves to the ratchet (no on-chain TP). The SL stays protected
						// (regime trail re-arms), but the stale TPs are no longer managed
						// by the close evaluator, so surface it rather than silently
						// changing the position's protection surface across a reload.
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
							// Manual ratchet + trailing walker run live-only by design
							// (gated on hyperliquidIsLive): manual is a live trading
							// tool, so a record-only manual config intentionally does
							// not ratchet (unlike perps, which also runs a paper
							// trailing path at main.go ~1537). Runs after the stamp
							// above so regime-keyed trails / post-TP SL see pos.Regime
							// on cycle 1.
							runPostTPStopLossAdjustment(sc, stratState, sc.Symbol, prices[sc.Symbol], cfg, &mu, notifier, logger, hlOnChainAbsQty)
							mark := prices[sc.Symbol]
							if mark > 0 && strategyUsesTrailingTPRatchetClose(sc) {
								ratchetAlert := applyTrailingTPRatchet(sc, stratState, sc.Symbol, mark, &mu, logger)
								notifyRatchetTrigger(notifier, sc.NotifyRatchetTriggersEnabled(cfg), ratchetAlert)
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
								// #873: force a re-size when a manual scale-in grew the
								// position (the trailing SL otherwise covers only the
								// pre-add size until the next trigger move).
								forceResize := pos.ScaleInResizePending && !capped
								newHighWater, slUpdate, updateConfirmed := runHyperliquidTrailingStopUpdate(sc, sc.Symbol, pos.Side, slEffectiveQty, pos, mark, pos.StopLossHighWaterPx, pos.StopLossTriggerPx, pos.StopLossOID, forceResize, notifier, logger)
								mu.Lock()
								// Shared handler with the perps path — books an immediate fill,
								// updates a resting replacement, or clears a cancelled-without-rest
								// OID. Previously this only handled the resting-replacement case.
								if immediateFill, fillPx := applyTrailingStopUpdateResult(stratState, sc.Symbol, pos.Side, prevSLOID, newHighWater, updateConfirmed, slUpdate, logger); immediateFill {
									logger.Info("[%s] manual trailing SL filled immediately %s @ $%.2f", sc.ID, sc.Symbol, fillPx)
								}
								// The manual trailing walker owns the SL re-size when it fires
								// (ratchet closes place no on-chain TPs), so it always clears
								// the flag; the sync defers the clear to here for trailing
								// owners (#882 review).
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
								// Fix #2: only cancel the SL on a full close; leave it resting on partial.
								// Intent is full-close iff the close-eval returned closeFraction >= 1.
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
								execResult, execStderr, execErr := RunHyperliquidExecute(
									sc.Script, sc.Symbol, closeSide, closeQty,
									0, cancelOID, 0, "", 0, closeFullPosition, hlExecuteSnapshot{}, extraCancelOIDs...,
								)
								if execStderr != "" {
									logger.Info("HL manual close stderr: %s", execStderr)
								}
								if execErr != nil {
									logger.Error("manual close execute failed: %v", execErr)
									break
								}
								if execResult.Error != "" {
									logger.Error("manual close HL error: %s", execResult.Error)
									break
								}
								// Cancel failures are non-fatal but leave reduce-only OIDs
								// resting on-chain after the strategy is virtually flat —
								// surface them so operators can verify TP/SL state.
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
									if err := stateDB.InsertPendingManualAction(action); err != nil {
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
					pv = displayStrategyValue(stratState, prices)
					posCount := len(stratState.Positions) + len(stratState.OptionPositions)
					cash := stratState.Cash
					regimeLabel := strategyDisplayRegimeLabel(stratState, sc, cfg.Regime)
					mu.RUnlock()

					logger.Info("%s", formatStatusLine(cash, posCount, pv, trades, regimeLabel))

					logger.Close()
					lastRun[sc.ID] = time.Now()
				}
			} // end if !killSwitchFired
		}

		// Build per-channel strategy lists for channel-level summaries.
		// Adjusted TOTAL rows are computed per-channel/per-asset below via
		// computeSubsetDisplayValue using the hoisted walletBalances map so
		// shared wallets are not double-counted in TOTAL rows (#915) and the
		// TOTAL reconciles with exchange-derived per-strategy rows (#918).
		mu.RLock()
		channelStrats := make(map[string][]StrategyConfig)
		for _, sc := range cfg.Strategies {
			if _, ok := state.Strategies[sc.ID]; ok {
				if chKey := notifier.resolveChannelKey(sc.Platform, sc.Type); chKey != "" {
					channelStrats[chKey] = append(channelStrats[chKey], sc)
				}
			}
		}
		mu.RUnlock()

		elapsed := time.Since(cycleStart)
		logMgr.LogSummary(cycle, elapsed, len(dueStrategies), totalTrades, totalPV)

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
		lifetimeStats := loadLifetimeStatsBestEffort(stateDB, "[summary]")

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
				// channel types (options/perps/futures/manual) post every channel run; spot
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
					// Compute shared-wallet-adjusted total for TOTAL row (#915);
					// gated members sum exchange-derived values so the TOTAL
					// reconciles with the per-strategy rows (#918).
					chAdj, _ := computeSubsetDisplayValue(chStrats, state, prices, walletBalances, sharedWallets)
					chSharpe := aggregateSharpe(closedByStrategy, chStrats, state, rfr)
					msgs := FormatCategorySummary(cycle, elapsed, len(dueStrategies), chTrades, chAdj, prices, chDetails, chStrats, state, chKey, "", cfg.IntervalSeconds, chSharpe, lifetimeStats, cfg.Regime)
					for _, msg := range msgs {
						notifier.SendToChannel(chKey, chKey, msg)
					}
				} else {
					// Multiple assets → one message per asset.
					for _, asset := range assetKeys {
						assetStrats := assetGroups[asset]
						assetDetails := channelTradeDetails[chKey+"|"+asset]
						// Subset-adjusted total. Per-asset groups always straddle a
						// multi-coin shared wallet; gated members sum their
						// exchange-derived values so this one-row TOTAL matches
						// the rows above it (#918), ungated keep #915 semantics.
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

		// Save state after each cycle
		mu.Lock()
		state.LastCycle = time.Now().UTC()
		state.LastSummaryPost = cloneTimeMap(lastSummaryPost)

		// Periodic configurable leaderboard summaries (#308). Compute + update
		// state.LastLeaderboardSummaries under Lock; post outside so Discord
		// HTTPS latency can't stall the scheduler cycle.
		var duePending []pendingLeaderboardSummary
		if notifier.HasBackends() {
			duePending = collectDueLeaderboardSummaries(cfg, state, prices, ComputeSharpeByStrategy(closedByStrategy, cfg, state), lifetimeStats, walletBalances, sharedWallets)
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
				// Pass hoisted walletBalances so the leaderboard TOTAL rows use
				// shared-wallet-adjusted values (#915).
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
			fmt.Println("[shutdown] exiting trading loop.")
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

	// Fetch shared-wallet balances for adjusted TOTAL rows (#915). Must be
	// done without holding any state lock; best-effort — failure falls back
	// to the per-strategy virtual sum for the TOTAL row.
	summaryWalletBalances, _ := fetchSharedWalletBalances(cfg.Strategies, nil)
	summaryAccountShared := detectSharedWallets(cfg.Strategies)

	// Format and send summary using the same asset-grouping logic as the main loop.
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

// spotSymbol extracts the spot symbol from strategy args (e.g. "BTC/USDT").
func spotSymbol(args []string) string {
	if len(args) >= 2 {
		return args[1]
	}
	return ""
}

// runSpotCheck runs the spot check subprocess and returns the parsed result.
// No state access. Returns (result, signalStr, price, ok); ok=false means skip execution.
func runSpotCheck(sc StrategyConfig, prices map[string]float64, posCtx PositionCtx, regime *RegimeConfig, notifier *MultiNotifier, logger *StrategyLogger) (*SpotResult, string, float64, bool) {
	args := append([]string{}, sc.Args...)
	args = appendOpenCloseArgs(args, sc, posCtx)
	if sc.HTFFilter {
		args = append(args, "--htf-filter")
	}
	args = appendRegimeArgs(args, regime)
	args = appendStrategyRegimeWindowArgs(args, sc, regime)
	args = appendRegimePayloadArg(args, sc, regime)
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
func executeSpotResult(sc StrategyConfig, s *StrategyState, db *StateDB, result *SpotResult, signalStr string, price float64, regime *RegimeConfig, logger *StrategyLogger) (int, string) {
	exec, err := ExecuteSpotSignalWithFillFeeDeferredOpen(s, result.Signal, result.Symbol, price, 0, 0, "", result.CloseFraction, logger)
	if err != nil {
		logger.Error("Trade execution failed: %v", err)
		return 0, ""
	}
	trades := exec.TradesExecuted
	stampEntryATRIfOpened(s, result.Symbol, result.Indicators)
	stampPositionRegimeIfOpened(s, result.Symbol, regimePayloadValue(result.Regime), sc, regime)
	stampDirectionCertifiedAtOpenIfOpened(s, result.Symbol, exec.OpenTrade != nil, sc, regime)
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

// stampPositionRegimeIfOpened stamps regime on the position the first time
// we observe one with a non-empty label (#733/#792).
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
	// Plausibility: reject NaN and ATR > 50% of entry price when we have a cost
	// baseline (almost certainly a unit mismatch or error in the strategy dataframe).
	if atr != atr || (pos.AvgCost > 0 && atr > pos.AvgCost*0.5) {
		return
	}
	pos.EntryATR = atr
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
func runOptionsCheck(sc StrategyConfig, posJSON string, notifier *MultiNotifier, logger *StrategyLogger) (*OptionsResult, string, bool) {
	args := append([]string{}, sc.Args...)
	// #879: inject the global store's (underlying, 4h, ADX-default) bundle so
	// check_options.py skips its inline regime fetch. The options signature
	// ignores cfg.Regime — the inline path was never gated on it.
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

// isHLLiveReconcilable reports whether sc should participate in on-chain
// reconciliation. Both type=perps and type=manual are live HL positions that
// can be closed externally; the reconciler is type-agnostic so both are safe.
// Other consumers of hlLiveAll (kill-switch, trailing-stop arming, risk math)
// intentionally stay perps-only.
func isHLLiveReconcilable(sc StrategyConfig) bool {
	return sc.Platform == "hyperliquid" &&
		(sc.Type == "perps" || sc.Type == "manual") &&
		hyperliquidIsLive(sc.Args)
}

// runHyperliquidCheck runs check_hyperliquid.py signal-check mode (Phase 3, no lock).
//
// sc is a pointer because the regime-aware directional policy (#779) needs
// to mutate Direction + InvertSignal in the caller's local sc copy after
// result.Regime is known, so downstream EffectiveDirection / perpsLiveOrderSize
// / PerpsOrderSkipReason calls in execute paths see the effective values.
// Mutation is scoped to the loop-local sc; cfg.Strategies is never touched.
func runHyperliquidCheck(sc *StrategyConfig, prices map[string]float64, posCtx PositionCtx, regime *RegimeConfig, notifier *MultiNotifier, logger *StrategyLogger) (*HyperliquidResult, string, float64, bool) {
	args := append([]string{}, sc.Args...)
	// Suppress in-process close evaluators that overlap on-chain reduce-only
	// protection — running both races on the shared on-chain position
	// (#604 review #2). Filter only changes the argv passed to Python; the
	// stored config is untouched.
	scForCheck := strategyConfigWithOnChainProtectionFilter(*sc)
	args = appendOpenCloseArgs(args, scForCheck, posCtx)
	if sc.HTFFilter {
		args = append(args, "--htf-filter")
	}
	args = appendRegimeArgs(args, regime)
	args = appendStrategyRegimeWindowArgs(args, *sc, regime)
	args = appendRegimePayloadArg(args, *sc, regime)
	if refsArgs, err := buildStrategyRefsArg(scForCheck); err != nil {
		logger.Warn("Failed to marshal strategy refs: %v", err)
	} else if len(refsArgs) > 0 {
		args = append(args, refsArgs...)
	}
	// #768 fix #3: Forward Go's cycle-local allMids snapshot so Python skips
	// its duplicate adapter.get_spot_price /info call. Same source, seconds
	// old, used only to freshen the display price.
	if sym := hyperliquidSymbol(sc.Args); sym != "" {
		if mid, ok := prices[sym]; ok && mid > 0 {
			args = append(args, fmt.Sprintf("--mark-price=%g", mid))
		}
	}
	logger.Info("Running: python3 %s %v", sc.Script, args)

	result, stderr, err := RunHyperliquidCheck(sc.Script, args)
	if err != nil {
		logger.Error("Script failed: %v", err)
		if stderr != "" {
			logger.Error("stderr: %s", stderr)
		}
		notifyScriptFailure(notifier, *sc, scriptFailureCrash, err.Error())
		return nil, "", 0, false
	}
	if stderr != "" {
		logger.Info("stderr: %s", stderr)
	}
	if result.Error != "" {
		logger.Error("Script returned error: %s", result.Error)
		notifyScriptFailure(notifier, *sc, scriptFailureError, result.Error)
		return nil, "", 0, false
	}
	clearScriptFailure(notifier, *sc)
	// #779: resolve regime-aware directional policy BEFORE applySignalInversion
	// so the invert decision uses the effective sc.InvertSignal. When flat,
	// resolves from result.Regime (current cycle); while a position is open,
	// uses posCtx.Regime (the regime stamped at open) so the position runs
	// to its natural exit under the policy that opened it.
	currentDirRegime := regimeDirectionalLabel(*sc, regimePayloadValue(result.Regime), regime)
	posDirRegime := posCtx.DirectionalRegime
	// #1085: evidence gate (PER STATE). When FLAT, key the entry side on the LIVE
	// certified per-state direction map for this strategy's (asset,timeframe,classifier).
	// When a position is OPEN, ride under the map frozen at open so an artifact
	// expiry/refresh never re-gates it mid-position; a state whose configured side
	// contradicts the certified sign (or is uncertified) resolves to base, so a
	// certified cell can never bet opposite the evidence. Then per-position:
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
		// One-shot operator alert for pre-#741 legacy positions opened before
		// regime stamping landed. The policy still applies, but the
		// hold-on-transition contract (position runs under the regime it
		// opened in) can't be honored for that position — it instead floats
		// with the current regime until it closes naturally. Self-heals once
		// the next entry stamps regime at open.
		if legacyFallback {
			if _, loaded := regimeDirectionalLegacyWarned.LoadOrStore(sc.ID, struct{}{}); !loaded {
				logger.Warn("Regime directional policy: open position has no stamped regime (legacy pre-#741); resolving against current regime=%q. Hold-on-transition not guaranteed for this position; self-heals on next entry.", regimeKey)
			}
		}
	}
	// #907: regime window divergence override — runs AFTER applyRegimeDirectionalPolicy
	// so the divergence wins when both are configured (medium-window policy entry is
	// superseded by the short-window hard-flip). Only affects new-entry direction when flat;
	// open positions keep hold-on-transition freeze (applyRegimeDivergenceOverride guards posQty).
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

// applySignalInversion flips a non-zero signal in place when InvertSignal is
// set on the strategy. HOLD (0) is never flipped — only the BUY (+1) / SELL
// (-1) sign is mirrored, so reverse-trend variants can reuse the same open
// and close strategy refs without forking the Python module. LoadConfig
// restricts InvertSignal to HL perps/manual strategies, so this helper is
// only invoked from runHyperliquidCheck.
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

// shouldCloseFullPosition decides whether a HL close leg should call
// adapter.market_close(sz=None) (close entire on-chain residual, no dust) vs.
// a sized market_open in the opposite direction.
//
// Sole-peer guard: market_close(sz=None) flattens the entire wallet position,
// not just this strategy's virtual share. When multiple configured live HL
// strategies share a coin (#491/#494/#619), a final-tier TP/manual close on
// strategy A would otherwise zero peer B's exposure too. Fall back to the
// sized close path in that case — virtual tracking on-chain via fillQty makes
// that path dust-tolerant within a single strategy's lifecycle (#592 review #1).
//
// Returns true only when close_fraction == 1.0 (final tier) AND the strategy
// is the sole configured live HL strategy on this coin. Callers decide which
// live strategy set is relevant; perps/manual close paths pass a list that
// includes both types so manual and automated peers are isolated.
func shouldCloseFullPosition(closeFraction float64, symbol string, hlLiveAll []StrategyConfig) bool {
	if closeFraction != 1.0 {
		return false
	}
	return len(hlLiveStrategiesForCoin(symbol, hlLiveAll)) <= 1
}

// runHyperliquidExecuteOrder places a live market order (Phase 3, no lock).
// Returns (execResult, ok); ok=false means order failed or was skipped, so
// caller must not apply state updates.
//
// posSide is the current position side captured under RLock in Phase 1
// ("long", "short", or "" for flat). We consult PerpsOrderSkipReason BEFORE
// calling the Python executor: if ExecutePerpsSignalWithLeverage would treat the result
// as a no-op, placing the live order would fill on-chain but never produce a
// Trade record, leaving state silently behind actual exchange holdings. See
// issue #298 — 0.716 ETH of live fills were lost this way because the
// "already long, skipping buy" branch sat AFTER RunHyperliquidExecute.
func runHyperliquidExecuteOrder(sc StrategyConfig, result *HyperliquidResult, price, cash, posQty float64, posSide string, avgCost float64, existingStopLossOID int64, existingTPOIDs []int64, hlLiveAll []StrategyConfig, walletSnapshot hlExecuteSnapshot, notifier *MultiNotifier, logger *StrategyLogger) (*HyperliquidExecuteResult, bool) {
	directionEnum := EffectiveDirection(sc)
	if reason := PerpsOrderSkipReason(result.Signal, posSide, directionEnum); reason != "" {
		logger.Info("Skipping live order for %s: %s", result.Symbol, reason)
		return nil, false
	}
	isBuy := result.Signal == 1
	// #254/#497/#518: perps sizing — sizing_leverage scales cash → notional in
	// the legacy formula; margin_per_trade_usd (when set) overrides to a
	// margin-space formula so high exchange leverage doesn't shrink intent.
	sizingLeverage := EffectiveSizingLeverage(sc)
	exchangeLeverage := EffectiveExchangeLeverage(sc)
	marginPerTradeUSD := EffectiveMarginPerTradeUSD(sc)
	size, ok, reason := perpsLiveOrderSize(result.Signal, price, cash, posQty, avgCost, sizingLeverage, exchangeLeverage, marginPerTradeUSD, posSide, directionEnum, result.CloseFraction)
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
	//     (no new position will follow — see pureClose predicate below).
	//     Skip for non-HL platforms or when pct<=0.
	//   - on a flip, pass prev_pos_qty so the SL is sized against the new
	//     net position (#421) — total_sz alone is closeQty+newQty.
	// pureClose: the signal will close an existing position with no new open
	// to follow. Holds for direction="long" + signal=-1 + long (close-only),
	// direction="short" + signal=1 + short (close-only), and direction="both"
	// only when there's nothing to flip into (never true here — flips always
	// open a new side). Direction="short" + signal=-1 with orphan long is
	// blocked by PerpsOrderSkipReason so it never reaches this code (#656).
	pureClose := perpsCloseActionSuppressesNewSL(result.Signal, posSide, PerpsAllowsLong(sc), PerpsAllowsShort(sc), result.CloseFraction)
	// Partial close (#519): a fractional close from the open/close registry
	// must NOT cancel the resting stop-loss — the SL is reduce-only and will
	// continue to protect the residual position; cancelling without
	// re-placing would leave the remainder unprotected. Skip both the cancel
	// and the new-SL placement on partial close. The trailing-stop loop
	// resizes the SL on its own cadence (#502).
	partialClose := result.CloseFraction > 0 && result.CloseFraction < 1
	// flipping predicate must mirror perpsLiveOrderSize exactly — flips are
	// only emitted under direction="both" (both directions allowed AND there's
	// an opposite-side position to flip away from). A long-only strategy that
	// inherited a short position would otherwise see prevPosQty=posQty here
	// while perpsLiveOrderSize sized it as a fresh open without that offset,
	// leaving net_new_sz negative and the SL silently undersized (#421 review).
	// #1009: also require CloseFraction == 0 — a close action (any fraction > 0)
	// is close-only, never a flip; the sizer's flip branch carries the same
	// guard, so this mirror must too or prevPosQty diverges from the order size.
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

	closeFullPosition := shouldCloseFullPosition(result.CloseFraction, result.Symbol, hlLiveAll)
	if closeFullPosition {
		logger.Info("Final-tier full close %s (close_fraction=1.0) — using market_close(sz=None)", result.Symbol)
	} else if result.CloseFraction == 1.0 {
		logger.Info("Final-tier close %s shares coin with HL perps peers — using sized close to preserve peer exposure", result.Symbol)
	}
	execResult, stderr, err := RunHyperliquidExecute(sc.Script, result.Symbol, side, size, slPct, cancelOID, prevPosQty, marginMode, leverageForOpen, closeFullPosition, walletSnapshot, extraCancelOIDs...)
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

func executeHyperliquidResult(sc StrategyConfig, s *StrategyState, result *HyperliquidResult, execResult *HyperliquidExecuteResult, signalStr string, price float64, regime *RegimeConfig, logger *StrategyLogger) (int, string) {
	trades, detail, openTrade, _ := executeHyperliquidResultDeferredOpen(sc, s, result, execResult, signalStr, price, regime, logger)
	if openTrade != nil {
		var pos *Position
		if p, ok := s.Positions[result.Symbol]; ok {
			pos = p
		}
		recordPositionOpen(s, sc, openTrade, pos)
	}
	return trades, detail
}

// executeHyperliquidResultDeferredOpen applies a hyperliquid result to state.
// Must be called under Lock. execResult is non-nil for successful live orders;
// nil for paper mode. Live open trades are returned so the caller can run
// same-cycle protection sync before the single INSERT.
func executeHyperliquidResultDeferredOpen(sc StrategyConfig, s *StrategyState, result *HyperliquidResult, execResult *HyperliquidExecuteResult, signalStr string, price float64, regime *RegimeConfig, logger *StrategyLogger) (int, string, *Trade, *RatchetTriggerAlert) {
	fillPrice := price
	var fillQty float64
	if execResult != nil && execResult.Execution != nil && execResult.Execution.Fill != nil && execResult.Execution.Fill.AvgPx > 0 {
		fillPrice = execResult.Execution.Fill.AvgPx
		fillQty = execResult.Execution.Fill.TotalSz
		logger.Info("Live fill at $%.2f qty=%.6f (mid was $%.2f)", fillPrice, fillQty, price)
	}

	exchangeLeverage := EffectiveExchangeLeverage(sc)
	sizingLeverage := EffectiveSizingLeverage(sc)
	marginPerTradeUSD := EffectiveMarginPerTradeUSD(sc)

	// Thread exchange metadata into ExecutePerpsSignalWithLeverage so each Trade is built
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

	exec, err := ExecutePerpsSignalWithLeverageDeferredOpen(s, result.Signal, result.Symbol, fillPrice, sizingLeverage, exchangeLeverage, marginPerTradeUSD, fillQty, fillOID, fillFee, EffectiveDirection(sc), result.CloseFraction, logger)
	if err != nil {
		logger.Error("Trade execution failed: %v", err)
		return 0, "", nil, nil
	}
	trades := exec.TradesExecuted
	openTrade := exec.OpenTrade
	stampEntryATRIfOpened(s, result.Symbol, result.Indicators)
	stampPositionRegimeIfOpened(s, result.Symbol, regimePayloadValue(result.Regime), sc, regime)
	stampDirectionCertifiedAtOpenIfOpened(s, result.Symbol, openTrade != nil, sc, regime)
	if pos, ok := s.Positions[result.Symbol]; ok {
		stampPositionProtectionSnapshot(pos, sc)
	}
	var ratchetAlert *RatchetTriggerAlert
	if trades > 0 {
		if pos, ok := s.Positions[result.Symbol]; ok {
			// #1110: capture the tighten snapshot here (under the caller's lock) and
			// return it so the caller can DM the owner after releasing mu.
			_, ratchetAlert = applyTrailingTPRatchetToPosition(sc, pos, result.Symbol, price, logger)
		}
	}
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
		var pos *Position
		if p, ok := s.Positions[result.Symbol]; ok {
			pos = p
		}
		// The immediate-close leg records next; nil openTrade prevents the
		// wrapper/caller from inserting the open a second time.
		if recordPositionOpen(s, sc, openTrade, pos) {
			openTrade = nil
		}
		triggerPx := execResult.Execution.Fill.StopLossTriggerPx
		if recordPerpsStopLossClose(s, result.Symbol, triggerPx, "stop_loss_immediate", logger) {
			trades++
		}
	}

	// #1137: fresh-open LLM analysis. Placed after the immediate-SL branch so
	// an open that stopped out at submit (trades==2, position already gone)
	// never dispatches.
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
		// Paper mode has no post-unlock protection sync; record now and nil the
		// deferred trade so the legacy wrapper cannot double-insert it.
		if recordPositionOpen(s, sc, openTrade, pos) {
			openTrade = nil
		}
	}
	return trades, detail, openTrade, ratchetAlert
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
func runTopStepCheck(sc StrategyConfig, prices map[string]float64, posCtx PositionCtx, regime *RegimeConfig, notifier *MultiNotifier, logger *StrategyLogger) (*TopStepResult, string, float64, bool) {
	args := append([]string{}, sc.Args...)
	args = appendOpenCloseArgs(args, sc, posCtx)
	if sc.HTFFilter {
		args = append(args, "--htf-filter")
	}
	args = appendRegimeArgs(args, regime)
	args = appendStrategyRegimeWindowArgs(args, sc, regime)
	args = appendRegimePayloadArg(args, sc, regime)
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

// runTopStepExecuteOrder places a live futures order (Phase 3, no lock).
//
// posSide is the current position side captured under RLock in Phase 1
// ("long", "short", or "" for flat). We consult FuturesOrderSkipReason BEFORE
// calling the Python executor: without this guard a live sell fires while
// posSide=="short" (Quantity is always positive so posQty<=0 cannot
// distinguish short from flat) but ExecuteFuturesSignalWithFillFee is a no-op in that
// state — producing a silent state drift identical in shape to #298/#300.
func runTopStepExecuteOrder(sc StrategyConfig, result *TopStepResult, price, cash, posQty float64, posSide string, notifier *MultiNotifier, logger *StrategyLogger) (*TopStepExecuteResult, bool) {
	if reason := FuturesOrderSkipReason(result.Signal, posSide); reason != "" {
		logger.Info("Skipping live order for %s: %s", result.Symbol, reason)
		return nil, false
	}
	isBuy := result.Signal == 1
	var contracts int
	if isBuy {
		// #518: removed hardcoded 0.95 buffer; max_contracts caps headroom.
		budget := cash
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
		if result.CloseFraction > 0 && result.CloseFraction < 1 {
			// #519: partial close from the open/close registry, rounded
			// DOWN to whole contracts so the residual position stays at
			// least one contract.
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

// executeTopStepResult applies a TopStep futures result to state. Must be called under Lock.
func executeTopStepResult(sc StrategyConfig, s *StrategyState, db *StateDB, result *TopStepResult, execResult *TopStepExecuteResult, signalStr string, price float64, regime *RegimeConfig, logger *StrategyLogger) (int, string) {
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

	exec, err := ExecuteFuturesSignalWithFillFeeDeferredOpen(s, result.Signal, result.Symbol, fillPrice, result.ContractSpec, feePerContract, maxContracts, fillContracts, fillFee, fillOID, result.CloseFraction, logger)
	if err != nil {
		logger.Error("Trade execution failed: %v", err)
		return 0, ""
	}
	trades := exec.TradesExecuted
	stampEntryATRIfOpened(s, result.Symbol, result.Indicators)
	stampPositionRegimeIfOpened(s, result.Symbol, regimePayloadValue(result.Regime), sc, regime)
	stampDirectionCertifiedAtOpenIfOpened(s, result.Symbol, exec.OpenTrade != nil, sc, regime)
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
func runRobinhoodCheck(sc StrategyConfig, prices map[string]float64, posCtx PositionCtx, regime *RegimeConfig, notifier *MultiNotifier, logger *StrategyLogger) (*RobinhoodResult, string, float64, bool) {
	args := append([]string{}, sc.Args...)
	args = appendOpenCloseArgs(args, sc, posCtx)
	if sc.HTFFilter {
		args = append(args, "--htf-filter")
	}
	args = appendRegimeArgs(args, regime)
	args = appendStrategyRegimeWindowArgs(args, sc, regime)
	args = appendRegimePayloadArg(args, sc, regime)
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

// runRobinhoodExecuteOrder places a live crypto order (Phase 3, no lock).
//
// posSide is the current position side captured under RLock in Phase 1
// ("long", "short", or "" for flat). We consult SpotOrderSkipReason BEFORE
// calling the Python executor: otherwise a no-op ExecuteSpotSignalWithFillFee (e.g.
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
func executeRobinhoodResult(sc StrategyConfig, s *StrategyState, db *StateDB, result *RobinhoodResult, execResult *RobinhoodExecuteResult, signalStr string, price float64, regime *RegimeConfig, logger *StrategyLogger) (int, string) {
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
	stampPositionRegimeIfOpened(s, result.Symbol, regimePayloadValue(result.Regime), sc, regime)
	stampDirectionCertifiedAtOpenIfOpened(s, result.Symbol, exec.OpenTrade != nil, sc, regime)
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
func runOKXCheck(sc StrategyConfig, prices map[string]float64, posCtx PositionCtx, regime *RegimeConfig, notifier *MultiNotifier, logger *StrategyLogger) (*OKXResult, string, float64, bool) {
	args := append([]string{}, sc.Args...)
	args = appendOpenCloseArgs(args, sc, posCtx)
	if sc.HTFFilter {
		args = append(args, "--htf-filter")
	}
	args = appendRegimeArgs(args, regime)
	args = appendStrategyRegimeWindowArgs(args, sc, regime)
	args = appendRegimePayloadArg(args, sc, regime)
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

// runOKXExecuteOrder places a live market order on OKX (Phase 3, no lock).
//
// posSide is the current position side captured under RLock in Phase 1
// ("long", "short", or "" for flat). We consult Perps/SpotOrderSkipReason
// BEFORE calling the Python executor — OKX covers both spot and perps, and
// each has its own side-based no-op branches in ExecuteSpotSignalWithFillFee /
// ExecutePerpsSignalWithLeverage that must be mirrored to avoid the #298 bug class
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
func executeOKXResult(sc StrategyConfig, s *StrategyState, db *StateDB, result *OKXResult, execResult *OKXExecuteResult, signalStr string, price float64, regime *RegimeConfig, logger *StrategyLogger) (int, string) {
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
	stampPositionRegimeIfOpened(s, result.Symbol, regimePayloadValue(result.Regime), sc, regime)
	stampDirectionCertifiedAtOpenIfOpened(s, result.Symbol, exec.OpenTrade != nil, sc, regime)
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
	// CLI exit path holds no state lock — safe to fetch wallet balances once
	// here and reuse across every summary (#915).
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

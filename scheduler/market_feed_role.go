package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	feedStatusStaleAfter  = 10 * time.Minute
	feedEstimateBarBytes  = 160
	feedEstimateKeyBytes  = 1024
	feedEstimateCoinBytes = 256

	feedEstimateObservationBytes = 96
)

type FeedRoleConfig struct {
	Source          string            `json:"source"`
	SocketPath      string            `json:"socket_path"`
	ConsumerConfigs []string          `json:"consumer_configs"`
	RequestBudget   *feedBudgetConfig `json:"request_budget,omitempty"`
}

var feedRoleRootKeys = map[string]bool{
	"config_version":          true,
	"role":                    true,
	"feed":                    true,
	"status_port":             true,
	"log_level":               true,
	"discord":                 true,
	"telegram":                true,
	"alert_throttle_interval": true,
	"strategies":              true,
}

func cleanFeedPath(p string) string {
	return filepath.Clean(p)
}

func validateSchedulerRole(cfg *Config) error {
	switch role := strings.TrimSpace(cfg.Role); role {
	case "", configRoleScheduler:
	case configRoleFeed:
		return fmt.Errorf("config role is %q: this file configures a market feed service, not a scheduler; run it as the daemon with --config and no subcommand", role)
	default:
		return fmt.Errorf("role must be %q or %q, got %q", configRoleScheduler, configRoleFeed, cfg.Role)
	}
	if cfg.Feed != nil {
		return fmt.Errorf("the feed block belongs to a role=%q config; a scheduler config must not carry it", configRoleFeed)
	}
	return nil
}

func sharedMarketFeedConfigErrors(cfg *Config) []string {
	if cfg == nil {
		return nil
	}
	if !cfg.marketFeedSharedEnabled() {
		if cfg.SharedMarketFeed != nil {
			return []string{fmt.Sprintf("shared_market_feed is set but market_feed is %q; set market_feed to %q or remove the block", cfg.marketFeedMode(), marketFeedShared)}
		}
		return nil
	}
	if cfg.SharedMarketFeed == nil {
		return []string{fmt.Sprintf("market_feed=%q needs a shared_market_feed block with primary_socket", marketFeedShared)}
	}
	primary, backup := cfg.sharedMarketFeedSockets()
	errs := feedSocketPathErrors("shared_market_feed.primary_socket", primary)
	if backup != "" {
		backupErrs := feedSocketPathErrors("shared_market_feed.backup_socket", backup)
		errs = append(errs, backupErrs...)
		if len(backupErrs) == 0 && cleanFeedPath(backup) == cleanFeedPath(primary) {
			errs = append(errs, fmt.Sprintf("shared_market_feed.backup_socket %q is the primary socket; the backup must be a separate feed service", backup))
		}
	}
	return errs
}

func peekConfigRole(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var head struct {
		Role *string `json:"role"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return "", fmt.Errorf("parse config: %w", err)
	}
	if head.Role == nil {
		return "", nil
	}
	return strings.TrimSpace(*head.Role), nil
}

func loadFeedRoleConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	var errs []string
	rootKeys := make([]string, 0, len(raw))
	for k := range raw {
		rootKeys = append(rootKeys, k)
	}
	sort.Strings(rootKeys)
	for _, k := range rootKeys {
		if !feedRoleRootKeys[k] {
			errs = append(errs, fmt.Sprintf("root key %q is not allowed in a role=%q config (a feed service owns no strategies, state files or trading settings)", k, configRoleFeed))
		}
	}
	if blob, ok := raw["strategies"]; ok {
		var list []json.RawMessage
		if err := json.Unmarshal(blob, &list); err != nil || len(list) > 0 {
			errs = append(errs, fmt.Sprintf("a role=%q config must have zero strategies", configRoleFeed))
		}
	}
	if blob, ok := raw["feed"]; ok {
		dec := json.NewDecoder(strings.NewReader(string(blob)))
		dec.DisallowUnknownFields()
		var probe FeedRoleConfig
		if err := dec.Decode(&probe); err != nil {
			errs = append(errs, fmt.Sprintf("feed block: %v", err))
		}
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("feed config validation errors:\n  %s", strings.Join(errs, "\n  "))
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	cfg.Role = strings.TrimSpace(cfg.Role)
	if cfg.Role != configRoleFeed {
		errs = append(errs, fmt.Sprintf("role must be %q for a feed service config, got %q", configRoleFeed, cfg.Role))
	}
	if cfg.Feed == nil {
		errs = append(errs, "feed block is missing (source, socket_path, consumer_configs)")
	} else {
		cfg.Feed.Source = strings.TrimSpace(cfg.Feed.Source)
		cfg.Feed.SocketPath = strings.TrimSpace(cfg.Feed.SocketPath)
		switch cfg.Feed.Source {
		case feedSourceWebsocket:
			errs = append(errs, feedBudgetConfigErrors("feed.request_budget", cfg.Feed.RequestBudget, false)...)
		case feedSourceREST:
			errs = append(errs, feedBudgetConfigErrors("feed.request_budget", cfg.Feed.RequestBudget, true)...)
		default:
			errs = append(errs, fmt.Sprintf("feed.source must be %q or %q, got %q", feedSourceWebsocket, feedSourceREST, cfg.Feed.Source))
		}
		errs = append(errs, feedSocketPathErrors("feed.socket_path", cfg.Feed.SocketPath)...)
		if len(cfg.Feed.ConsumerConfigs) == 0 {
			errs = append(errs, "feed.consumer_configs lists no consumer scheduler configs")
		}
		seen := make(map[string]bool, len(cfg.Feed.ConsumerConfigs))
		for i, p := range cfg.Feed.ConsumerConfigs {
			p = strings.TrimSpace(p)
			if p == "" {
				errs = append(errs, fmt.Sprintf("feed.consumer_configs[%d] is empty", i))
				continue
			}
			abs, absErr := filepath.Abs(p)
			if absErr != nil {
				errs = append(errs, fmt.Sprintf("feed.consumer_configs[%d] %q: %v", i, p, absErr))
				continue
			}
			abs = filepath.Clean(abs)
			if abs == filepath.Clean(mustAbs(path)) {
				errs = append(errs, fmt.Sprintf("feed.consumer_configs[%d] names this feed config itself", i))
				continue
			}
			if seen[abs] {
				errs = append(errs, fmt.Sprintf("feed.consumer_configs[%d] %q is listed twice", i, abs))
				continue
			}
			seen[abs] = true
			cfg.Feed.ConsumerConfigs[i] = abs
		}
	}
	if cfg.StatusPort != 0 && (cfg.StatusPort < 1024 || cfg.StatusPort > 65535) {
		errs = append(errs, fmt.Sprintf("status_port %d must be between 1024 and 65535", cfg.StatusPort))
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("feed config validation errors:\n  %s", strings.Join(errs, "\n  "))
	}
	if cfg.LogDir == "" {
		cfg.LogDir = "logs"
	}
	applyNotifierEnvOverrides(&cfg)
	cfg.StatusToken = os.Getenv("STATUS_AUTH_TOKEN")
	return &cfg, nil
}

func mustAbs(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return abs
}

func loadConsumerConfigForFeed(path string) (*Config, error) {
	return loadConfig(path, true, true)
}

type feedConsumer struct {
	Path       string
	Loaded     bool
	Retained   bool
	Err        string
	Req        feedRequirements
	Cadences   []int
	Strategies int
	LoadedAt   time.Time
}

func loadFeedConsumer(path, socketPath string, now time.Time) feedConsumer {
	out := feedConsumer{Path: path}
	cfg, err := loadConsumerConfigForFeed(path)
	if err != nil {
		out.Err = err.Error()
		return out
	}
	if !cfg.marketFeedSharedEnabled() {
		out.Err = fmt.Sprintf("market_feed is %q; a feed consumer must use %q", cfg.marketFeedMode(), marketFeedShared)
		return out
	}
	primary, backup := cfg.sharedMarketFeedSockets()
	if cleanFeedPath(primary) != socketPath && (backup == "" || cleanFeedPath(backup) != socketPath) {
		out.Err = fmt.Sprintf("shared_market_feed names %q (primary) and %q (backup), not this feed's socket %s", primary, backup, socketPath)
		return out
	}
	req, err := deriveFeedRequirements(cfg)
	if err != nil {
		out.Err = err.Error()
		return out
	}
	out.Loaded = true
	out.Req = req
	out.Cadences = feedConsumerCadences(cfg)
	out.Strategies = len(req.Strategies)
	out.LoadedAt = now
	return out
}

func loadFeedConsumers(paths []string, socketPath string, previous map[string]feedConsumer, now time.Time) []feedConsumer {
	out := make([]feedConsumer, 0, len(paths))
	for _, p := range paths {
		c := loadFeedConsumer(p, socketPath, now)
		if !c.Loaded {
			if prev, ok := previous[p]; ok && prev.Loaded {
				prev.Retained = true
				prev.Err = c.Err
				out = append(out, prev)
				continue
			}
		}
		out = append(out, c)
	}
	return out
}

func feedConsumerCadences(cfg *Config) []int {
	set := make(map[int]bool)
	warn := configuredDrawdownWarnThresholdPct(cfg)
	for _, sc := range cfg.Strategies {
		if !feedScopedStrategy(sc) {
			continue
		}
		for _, c := range feedPossibleIntervals(sc, cfg.IntervalSeconds, warn) {
			set[c] = true
		}
	}
	out := make([]int, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Ints(out)
	return out
}

func unionFeedRequirements(consumers []feedConsumer) feedRequirements {
	union := feedRequirements{
		Keys:         make(map[marketFeedKey]int),
		Funding:      make(map[string]feedFundingNeed),
		Observations: make(map[feedObservationKey]int64),
		Strategies:   make(map[string]feedStrategyRequirement),
		KeyCadences:  make(map[marketFeedKey][]int),
		SignalKeys:   make(map[marketFeedKey]bool),
	}
	coins := make(map[string]bool)
	cadenceSets := make(map[marketFeedKey]map[int]bool)
	for _, c := range consumers {
		if !c.Loaded {
			continue
		}
		for key, lookback := range c.Req.Keys {
			union.addKey(key, lookback)
			set := cadenceSets[key]
			if set == nil {
				set = make(map[int]bool)
				cadenceSets[key] = set
			}
			for _, cad := range c.Cadences {
				set[cad] = true
			}
		}
		for _, entry := range c.Req.Strategies {
			union.SignalKeys[entry.Signal] = true
		}
		for _, coin := range c.Req.MidCoins {
			coins[coin] = true
		}
		for coin, need := range c.Req.Funding {
			f := union.Funding[coin]
			f.Scalar = f.Scalar || need.Scalar
			f.Records = f.Records || need.Records
			union.Funding[coin] = f
		}
		for key, window := range c.Req.Observations {
			union.addObservation(key, window)
		}
	}
	for coin := range coins {
		union.MidCoins = append(union.MidCoins, coin)
	}
	for key, set := range cadenceSets {
		cads := make([]int, 0, len(set))
		for cad := range set {
			cads = append(cads, cad)
		}
		sort.Ints(cads)
		union.KeyCadences[key] = cads
	}
	union.finalize()
	return union
}

func unionFeedCadences(consumers []feedConsumer) []int {
	set := make(map[int]bool)
	for _, c := range consumers {
		if !c.Loaded {
			continue
		}
		for _, v := range c.Cadences {
			set[v] = true
		}
	}
	out := make([]int, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Ints(out)
	return out
}

func estimateFeedSealBytes(union feedRequirements) int {
	total := 4096
	for _, key := range union.Order {
		total += feedEstimateKeyBytes + (union.Keys[key]+hlFeedHistoryMargin)*feedEstimateBarBytes
	}
	total += len(union.MidCoins) * feedEstimateCoinBytes
	for range union.Funding {
		total += 7 * 24 * 64
	}
	for _, window := range union.Observations {
		total += feedEstimateKeyBytes + int(window/feedObservationCadenceMs+2)*feedEstimateObservationBytes
	}
	return total
}

func feedConsumersLoaded(consumers []feedConsumer) int {
	n := 0
	for _, c := range consumers {
		if c.Loaded {
			n++
		}
	}
	return n
}

func formatFeedConsumerLines(prefix string, consumers []feedConsumer) []string {
	out := make([]string, 0, len(consumers))
	for _, c := range consumers {
		switch {
		case c.Loaded && c.Retained:
			out = append(out, fmt.Sprintf("%s consumer %s: unreadable (%s); keeping its previous contribution (%d strategies, cadences %s)",
				prefix, c.Path, c.Err, c.Strategies, formatCadences(c.Cadences)))
		case c.Loaded:
			out = append(out, fmt.Sprintf("%s consumer %s: %d feed strategies, %d keys, cadences %s",
				prefix, c.Path, c.Strategies, len(c.Req.Order), formatCadences(c.Cadences)))
		default:
			out = append(out, fmt.Sprintf("%s consumer %s: skipped (%s)", prefix, c.Path, c.Err))
		}
	}
	return out
}

func formatCadences(cadences []int) string {
	if len(cadences) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(cadences))
	for _, c := range cadences {
		parts = append(parts, fmt.Sprintf("%ds", c))
	}
	return strings.Join(parts, ",")
}

type feedRuntime struct {
	configPath string
	cfg        *Config
	notifier   *MultiNotifier
	owner      *marketFeedOwner
	sealer     *feedSealer
	ledger     *feedRequestLedger
	instance   string
	startedAt  time.Time
	ctx        context.Context

	mu        sync.Mutex
	consumers []feedConsumer
	draining  bool
	reloads   int
}

func formatFeedBudgetConfig(source string, b *feedBudgetConfig) string {
	if b == nil {
		return fmt.Sprintf("request budget: none configured (source %s counts requests without a limit)", source)
	}
	mode := "enforced"
	if source != feedSourceREST {
		mode = "logged only"
	}
	return fmt.Sprintf("request budget: per_minute=%d startup=%d (%s, source %s)", b.PerMinute, b.Startup, mode, source)
}

func newFeedInstanceID(now time.Time) string {
	host, _ := os.Hostname()
	if host == "" {
		host = "host"
	}
	return fmt.Sprintf("%s-%d-%d", host, os.Getpid(), now.UnixMilli())
}

func feedLogf(format string, a ...any) {
	fmt.Printf(format+"\n", a...)
}

func runFeedRole(configPath string, statusPortFlag int, once bool, summary string, leaderboard bool) int {
	if once || summary != "" || leaderboard {
		fmt.Fprintln(os.Stderr, "[feed] --once, --summary and --leaderboard are scheduler modes; a role=feed config runs only as the feed service")
		return 2
	}
	cfg, err := loadFeedRoleConfig(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		return 1
	}
	if err := applyAlertThrottleFromConfig(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to apply alert throttle interval: %v\n", err)
		return 1
	}
	if err := applyLogLevelFromConfig(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to apply log level: %v\n", err)
		return 1
	}
	port := statusPortFlag
	if port <= 0 {
		port = cfg.StatusPort
	}
	if port <= 0 {
		fmt.Fprintln(os.Stderr, "[feed] a role=feed config needs an explicit status_port (or --status-port); the feed binds it exactly with no fallback")
		return 1
	}
	startedAt := time.Now().UTC()
	instance := newFeedInstanceID(startedAt)
	fmt.Printf("[feed] role=feed version=%s instance=%s source=%s socket=%s consumers=%d status_port=%d\n",
		Version, instance, cfg.Feed.Source, cfg.Feed.SocketPath, len(cfg.Feed.ConsumerConfigs), port)

	notifier, cleanupNotifier := buildNotifierFromConfig(cfg)
	defer cleanupNotifier()

	lock, err := acquireStateDBLock(cfg.Feed.SocketPath)
	if err != nil {
		msg := fmt.Sprintf("feed socket %s is owned by another process: %v", cfg.Feed.SocketPath, err)
		fmt.Fprintf(os.Stderr, "[feed] CRITICAL: %s (exit %d)\n", msg, ExitSingletonLock)
		sendStartupRefusalDM(notifier, "Feed socket guard", msg)
		cleanupNotifier()
		os.Exit(ExitSingletonLock)
	}
	defer lock.Release()

	consumers := skipObservationConsumers(cfg.Feed.Source, loadFeedConsumers(cfg.Feed.ConsumerConfigs, cfg.Feed.SocketPath, nil, startedAt))
	for _, line := range formatFeedConsumerLines("[feed]", consumers) {
		fmt.Println(line)
	}
	for _, c := range consumers {
		if !c.Loaded {
			sendStartupRefusalDM(notifier, "Feed consumer skipped", fmt.Sprintf("%s: %s", c.Path, c.Err))
		}
	}
	if feedConsumersLoaded(consumers) == 0 {
		msg := "no consumer config could be loaded; refusing to start a feed that serves nobody"
		fmt.Fprintf(os.Stderr, "[feed] CRITICAL: %s (exit %d)\n", msg, ExitProbeFailure)
		sendStartupRefusalDM(notifier, "Feed startup", msg)
		cleanupNotifier()
		os.Exit(ExitProbeFailure)
	}
	union := unionFeedRequirements(consumers)
	cadences := unionFeedCadences(consumers)
	if est := estimateFeedSealBytes(union); est > feedSealMaxBytes {
		msg := fmt.Sprintf("the consumer union (%d keys) needs about %d bytes per seal, over the %d-byte transport cap", len(union.Order), est, feedSealMaxBytes)
		fmt.Fprintf(os.Stderr, "[feed] CRITICAL: %s (exit %d)\n", msg, ExitProbeFailure)
		sendStartupRefusalDM(notifier, "Feed startup", msg)
		cleanupNotifier()
		os.Exit(ExitProbeFailure)
	}
	fmt.Printf("[feed] union: %d keys, %d mid coins, %d funding coins, cadences %s, estimated seal size %d bytes (cap %d)\n",
		len(union.Order), len(union.MidCoins), len(union.Funding), formatCadences(cadences), estimateFeedSealBytes(union), feedSealMaxBytes)

	baseCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ledger := newFeedRequestLedger(cfg.Feed.RequestBudget, cfg.Feed.Source == feedSourceREST, nil)
	ctx := withFeedLedger(baseCtx, ledger)
	fmt.Printf("[feed] %s\n", formatFeedBudgetConfig(cfg.Feed.Source, cfg.Feed.RequestBudget))

	owner := newMarketFeedOwner(nil, feedLogf)
	globalMarketFeedStatus.setOwner(owner)
	sealer := newFeedSealer(owner, cfg.Feed.Source, instance, nil, feedLogf)
	sealer.ledger = ledger
	if cfg.Feed.Source == feedSourceREST {
		sealer.prepareFn = newFeedRESTSource(owner, feedLogf).prepare
	} else {
		sealer.prepareFn = websocketFeedPrepare(owner)
	}
	sealer.alertHook = func(msg string) {
		fmt.Printf("[feed] %s\n", msg)
		if notifier != nil && notifier.HasOwner() {
			notifier.SendOwnerDM(msg)
		}
	}
	sealer.setGeneration(feedRequirements{}, cadences, startedAt)

	rt := &feedRuntime{
		configPath: configPath,
		cfg:        cfg,
		notifier:   notifier,
		owner:      owner,
		sealer:     sealer,
		ledger:     ledger,
		instance:   instance,
		startedAt:  startedAt,
		ctx:        ctx,
		consumers:  consumers,
	}

	statusLn, err := net.Listen("tcp", fmt.Sprintf("localhost:%d", port))
	if err != nil {
		fmt.Fprintf(os.Stderr, "[feed] CRITICAL: status port %d: %v; the feed binds its configured port exactly\n", port, err)
		sendStartupRefusalDM(notifier, "Feed startup", fmt.Sprintf("status port %d: %v", port, err))
		return 1
	}
	go func() {
		if err := http.Serve(statusLn, rt.statusMux()); err != nil && !errors.Is(err, net.ErrClosed) {
			fmt.Printf("[feed] status server error: %v\n", err)
		}
	}()
	fmt.Printf("[feed] status endpoint at http://localhost:%d/status\n", port)

	srv, err := listenFeedSocket(cfg.Feed.SocketPath, sealer, feedLogf)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[feed] CRITICAL: %v\n", err)
		sendStartupRefusalDM(notifier, "Feed startup", err.Error())
		statusLn.Close()
		return 1
	}
	go srv.serve()
	fmt.Printf("[feed] listening on %s (mode %o)\n", cfg.Feed.SocketPath, feedSocketFileMode)

	ledger.openStartup()
	ready := owner.ApplyGeneration(ctx, union)
	if cfg.Feed.Source == feedSourceWebsocket {
		fmt.Printf("[feed] closed-bar correction: re-read at close%s, counted as request reason correction\n",
			formatFeedCorrectionOffsets(feedCorrectionOffsets))
		go owner.Run(ctx)
	}
	select {
	case <-ready:
		sealer.setGeneration(union, cadences, startedAt)
	case <-time.After(feedStartupBudget):
		fmt.Printf("[feed] startup budget %s elapsed before every key was ready; seals carry no candle keys until the generation publishes\n", feedStartupBudget)
		go func() {
			select {
			case <-ready:
				rt.mu.Lock()
				stale := rt.reloads > 0
				if !stale {
					sealer.setGeneration(union, cadences, startedAt)
				}
				rt.mu.Unlock()
				if stale {
					fmt.Println("[feed] the startup generation finished after a reload replaced it; keeping the reloaded requirements")
					return
				}
				fmt.Printf("[feed] generation %d published after the startup budget (%d keys)\n", owner.Generation(), len(union.Order))
			case <-ctx.Done():
			}
		}()
	}
	for _, r := range owner.Readiness() {
		fmt.Printf("[feed] %s: %s (%d/%d bars)\n", r.Key, r.Status, r.Bars, r.Required)
	}
	servingAt := time.Now().UTC()
	sealer.startServing(servingAt)
	go sealer.run(ctx)
	d := sealer.describe()
	fmt.Printf("[feed] serving: generation=%d first_deadline=%d cadences=%s settle=%s prepare=%s grace=%s retain_per_cadence=%d\n",
		owner.Generation(), d.FirstDeadline, formatCadences(cadences), feedSealSettleDelay, feedSealPrepareBudget, feedSealPublishGrace, feedSealRetainPerCadence)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	hupCh := make(chan os.Signal, 1)
	signal.Notify(hupCh, syscall.SIGHUP)
	defer signal.Stop(sigCh)
	defer signal.Stop(hupCh)

	reloadCh := make(chan struct{}, 1)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-reloadCh:
				rt.reload()
			}
		}
	}()

	for {
		select {
		case sig := <-sigCh:
			fmt.Printf("[feed] received %s; closing the socket and stopping\n", sig)
			rt.mu.Lock()
			rt.draining = true
			rt.mu.Unlock()
			srv.close()
			cancel()
			statusLn.Close()
			fmt.Println("[feed] shutdown complete")
			return 0
		case <-hupCh:
			select {
			case reloadCh <- struct{}{}:
			default:
				fmt.Println("[reload] SIGHUP received while a reload is pending; coalescing")
			}
		}
	}
}

func (rt *feedRuntime) reload() {
	fmt.Printf("[reload] SIGHUP received; reloading feed config from %s\n", rt.configPath)
	next, err := loadFeedRoleConfig(rt.configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[reload] ERROR: reload failed; keeping the previous feed generation: %v\n", err)
		return
	}
	var errs []string
	if next.Feed.SocketPath != rt.cfg.Feed.SocketPath {
		errs = append(errs, fmt.Sprintf("feed.socket_path changed (%q -> %q; restart required)", rt.cfg.Feed.SocketPath, next.Feed.SocketPath))
	}
	if next.Feed.Source != rt.cfg.Feed.Source {
		errs = append(errs, fmt.Sprintf("feed.source changed (%q -> %q; restart required)", rt.cfg.Feed.Source, next.Feed.Source))
	}
	if next.StatusPort != rt.cfg.StatusPort {
		errs = append(errs, fmt.Sprintf("status_port changed (%d -> %d; restart required)", rt.cfg.StatusPort, next.StatusPort))
	}
	if len(errs) > 0 {
		fmt.Fprintf(os.Stderr, "[reload] ERROR: reload rejected; keeping the previous feed generation: %s\n", strings.Join(errs, "; "))
		return
	}
	rt.mu.Lock()
	rt.reloads++
	previous := make(map[string]feedConsumer, len(rt.consumers))
	for _, c := range rt.consumers {
		previous[c.Path] = c
	}
	rt.mu.Unlock()
	now := time.Now().UTC()
	consumers := skipObservationConsumers(next.Feed.Source, loadFeedConsumers(next.Feed.ConsumerConfigs, next.Feed.SocketPath, previous, now))
	for _, line := range formatFeedConsumerLines("[reload]", consumers) {
		fmt.Println(line)
	}
	for _, c := range consumers {
		if c.Err != "" && rt.notifier != nil && rt.notifier.HasOwner() {
			if c.Retained {
				rt.notifier.SendOwnerDM(fmt.Sprintf("**Feed reload** consumer %s is unreadable (%s); the feed keeps its previous contribution.", c.Path, c.Err))
			} else {
				rt.notifier.SendOwnerDM(fmt.Sprintf("**Feed reload** consumer %s skipped: %s", c.Path, c.Err))
			}
		}
	}
	if feedConsumersLoaded(consumers) == 0 {
		fmt.Fprintln(os.Stderr, "[reload] ERROR: no consumer config could be loaded; keeping the previous feed generation")
		return
	}
	union := unionFeedRequirements(consumers)
	cadences := unionFeedCadences(consumers)
	if est := estimateFeedSealBytes(union); est > feedSealMaxBytes {
		fmt.Fprintf(os.Stderr, "[reload] ERROR: the new union needs about %d bytes per seal, over the %d-byte cap; keeping the previous feed generation\n", est, feedSealMaxBytes)
		return
	}
	rt.ledger.setLimits(next.Feed.RequestBudget)
	fmt.Printf("[reload] %s\n", formatFeedBudgetConfig(next.Feed.Source, next.Feed.RequestBudget))
	rt.ledger.openStartup()
	select {
	case <-rt.owner.ApplyGeneration(rt.ctx, union):
	case <-rt.ctx.Done():
		return
	}
	rt.sealer.setGeneration(union, cadences, now)
	rt.mu.Lock()
	rt.cfg = next
	rt.consumers = consumers
	rt.mu.Unlock()
	fmt.Printf("[reload] feed generation %d published (%d keys, %d mid coins, cadences %s); it applies to deadlines sealed from now on\n",
		rt.owner.Generation(), len(union.Order), len(union.MidCoins), formatCadences(cadences))
}

func (rt *feedRuntime) statusMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", rt.handleHealth)
	mux.HandleFunc("/status", rt.handleStatus)
	return mux
}

func (rt *feedRuntime) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	rt.mu.Lock()
	draining := rt.draining
	rt.mu.Unlock()
	resp := map[string]any{
		"status":   "ok",
		"version":  Version,
		"pid":      os.Getpid(),
		"role":     configRoleFeed,
		"instance": rt.instance,
		"source":   rt.sealer.source,
	}
	if draining {
		resp["status"] = "draining"
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(resp)
		return
	}
	d := rt.sealer.describe()
	resp["serving"] = d.Serving
	resp["generation"] = rt.owner.Generation()
	resp["last_seal_key"] = d.LastSealKey
	resp["last_seal_ready_keys"] = d.LastSealReady
	resp["last_seal_keys"] = d.LastSealKeysTotal
	resp["request_budget"] = rt.ledger.status()
	if last := rt.sealer.lastSealSnapshot(); last != nil {
		age := time.Since(last.SealedAt)
		resp["last_seal_age_s"] = int64(age.Seconds())
		if age > feedStatusStaleAfter {
			resp["status"] = "unhealthy"
			resp["reason"] = "no seal stored in the last " + feedStatusStaleAfter.String()
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	} else if d.Serving && time.Since(rt.startedAt) > feedStatusStaleAfter {
		resp["status"] = "unhealthy"
		resp["reason"] = "no seal stored since startup"
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	json.NewEncoder(w).Encode(resp)
}

type feedStatusConsumer struct {
	Path       string `json:"path"`
	Loaded     bool   `json:"loaded"`
	Retained   bool   `json:"retained,omitempty"`
	Error      string `json:"error,omitempty"`
	Strategies int    `json:"strategies"`
	Keys       int    `json:"keys"`
	Cadences   []int  `json:"cadences"`
}

func (rt *feedRuntime) handleStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	rt.mu.Lock()
	cfg := rt.cfg
	rt.mu.Unlock()
	if tok := cfg.StatusToken; tok != "" && r.Header.Get("Authorization") != "Bearer "+tok {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"unauthorized"}`))
		return
	}
	rt.mu.Lock()
	consumers := make([]feedStatusConsumer, 0, len(rt.consumers))
	for _, c := range rt.consumers {
		consumers = append(consumers, feedStatusConsumer{
			Path: c.Path, Loaded: c.Loaded, Retained: c.Retained, Error: c.Err,
			Strategies: c.Strategies, Keys: len(c.Req.Order), Cadences: append([]int{}, c.Cadences...),
		})
	}
	rt.mu.Unlock()
	lastID := ""
	if last := rt.sealer.lastSealSnapshot(); last != nil {
		lastID = feedSealEvaluationID(last.Key)
	}
	health := rt.owner.Health(lastID)
	health.Mode = rt.sealer.source
	resp := map[string]any{
		"role":           configRoleFeed,
		"version":        Version,
		"instance":       rt.instance,
		"source":         rt.sealer.source,
		"socket_path":    cfg.Feed.SocketPath,
		"market_feed":    health,
		"sealer":         rt.sealer.describe(),
		"consumers":      consumers,
		"request_budget": rt.ledger.status(),
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(resp)
}

func runFeedProbe(configPath string) int {
	cfg, err := loadFeedRoleConfig(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "probe: failed to load feed config %s: %v\n", configPath, err)
		return 1
	}
	if cfg.StatusPort <= 0 {
		fmt.Fprintf(os.Stderr, "probe: feed config %s has no status_port; the feed needs an explicit port\n", configPath)
		return 1
	}
	consumers := skipObservationConsumers(cfg.Feed.Source, loadFeedConsumers(cfg.Feed.ConsumerConfigs, cfg.Feed.SocketPath, nil, time.Now().UTC()))
	for _, line := range formatFeedConsumerLines("probe:", consumers) {
		fmt.Println(line)
	}
	if feedConsumersLoaded(consumers) == 0 {
		fmt.Fprintln(os.Stderr, "probe: no consumer config could be loaded")
		return ExitProbeFailure
	}
	union := unionFeedRequirements(consumers)
	if est := estimateFeedSealBytes(union); est > feedSealMaxBytes {
		fmt.Fprintf(os.Stderr, "probe: the consumer union needs about %d bytes per seal, over the %d-byte cap\n", est, feedSealMaxBytes)
		return ExitProbeFailure
	}
	fmt.Printf("probe: %s\n", formatFeedBudgetConfig(cfg.Feed.Source, cfg.Feed.RequestBudget))
	for _, key := range union.Order {
		fmt.Printf("probe: key %s lookback=%d cadences=%s\n", key.PayloadID(), union.Keys[key], formatCadences(union.KeyCadences[key]))
	}
	for _, key := range union.observationKeys() {
		fmt.Printf("probe: observation %s window_ms=%d source=%s\n", key.PayloadID(), union.Observations[key], feedObservationSourceHLWS)
	}
	fmt.Printf("probe: OK (role=feed, source=%s, %d of %d consumer configs, %d keys, cadences %s, check scripts not probed, version=%s)\n",
		cfg.Feed.Source, feedConsumersLoaded(consumers), len(consumers), len(union.Order), formatCadences(unionFeedCadences(consumers)), Version)
	return 0
}

func skipObservationConsumers(source string, consumers []feedConsumer) []feedConsumer {
	if source == feedSourceWebsocket {
		return consumers
	}
	for i := range consumers {
		c := &consumers[i]
		if !c.Loaded || len(c.Req.Observations) == 0 {
			continue
		}
		keys := c.Req.observationKeys()
		names := make([]string, 0, len(keys))
		for _, k := range keys {
			names = append(names, k.PayloadID())
		}
		c.Loaded = false
		c.Retained = false
		c.Err = fmt.Sprintf("feed.source=%q cannot collect observations %s for this consumer; only feed.source=%q subscribes to the venue open-interest stream",
			source, strings.Join(names, ", "), feedSourceWebsocket)
	}
	return consumers
}

package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// #1442 — batched Hyperliquid signal checks.
//
// A deployment runs many strategies against one coin. The regime bundle
// (#879) and the OHLCV disk cache (#839) already deduplicate the regime
// computation and the candle fetch; what stays duplicated per strategy is the
// interpreter start, the module imports, the DataFrame build and the indicator
// base. This lane evaluates every DUE strategy that shares one market-data key
// in a single check_hyperliquid.py --batch-check call.
//
// Invariants this file exists to keep:
//   - Grouping (partitionHyperliquidBatchGroups) is PURE — no locks, no global
//     reads, no I/O. It runs over dueStrategies only, so a strategy whose
//     interval has not elapsed is never batched.
//   - The per-slot position snapshot takes ONE mu.RLock read, in the Phase-1
//     pattern, AFTER reconcileHyperliquidAccountPositions has booked external
//     closes. Snapshotting earlier would feed pre-reconcile position contexts
//     into close evaluators. No lock is held across the subprocess and
//     strategiesMu is never involved, so the lock ORDER is unchanged.
//   - Only the subprocess is replaced. runHyperliquidCheck consumes a cached
//     slot and runs the identical post-parse pipeline, so everything
//     downstream — risk, execution, the #1431 replay choke points, hedging —
//     is byte-identical to the per-strategy path.
//   - A group of one produces no batch: that strategy takes the existing
//     spawn path untouched.

const hyperliquidCheckScript = "shared_scripts/check_hyperliquid.py"

// hlBatchPythonDefaultOhlcvLimit mirrors check_hyperliquid.py's --ohlcv-limit
// argparse default. When regime is disabled Go appends no --ohlcv-limit flag,
// so the key must carry the value the script would actually use.
const hlBatchPythonDefaultOhlcvLimit = 200

// hlBatchProtocolVersion mirrors BATCH_PROTOCOL_VERSION in check_hyperliquid.py.
const hlBatchProtocolVersion = 1

// hlBatchTimeout is the deadline for one batched call. A var so tests can
// shrink it. Batching concentrates N strategies' compute under one deadline,
// which is the one place it adds risk; a timeout renders as the existing
// "script timed out after %s" text, so it classifies transient exactly like a
// per-strategy timeout does (script_failure_alerts.go).
var hlBatchTimeout = scriptTimeout

// hlBatchDisabledEnv is the operator escape hatch. Setting it to "0" (or
// "false"/"off") reverts every group to the proven per-strategy path without a
// rebuild and without a config key.
const hlBatchDisabledEnv = "GO_TRADER_HL_BATCH"

// hlBatchSharedFailureFallbackThreshold is the number of CONSECUTIVE
// shared-state failures on one group before the pre-pass stops batching it.
// Without this, a batch-path defect blinds every strategy on the coin for as
// long as it persists; the fallback bounds that to three cycles and then runs
// the per-strategy path, which is known good.
const hlBatchSharedFailureFallbackThreshold = 3

// hlBatchFallbackRetryEvery is how often a fallen-back group re-attempts the
// batched path (every Nth cycle it would otherwise have batched), so a
// transient outage self-heals without operator action.
const hlBatchFallbackRetryEvery = 10

// hlBatchKey identifies one shared market-data computation. DataPlatform
// mirrors regimeBundleKey.Platform's rationale: it is the OHLCV SOURCE, not
// sc.Platform. Every argument outside this key travels per slot.
type hlBatchKey struct {
	DataPlatform string
	Symbol       string
	Timeframe    string
	OhlcvLimit   int
	ATRMethod    string
}

func (k hlBatchKey) String() string {
	return fmt.Sprintf("%s/%s/%s/limit=%d/atr=%s", k.DataPlatform, k.Symbol, k.Timeframe, k.OhlcvLimit, k.ATRMethod)
}

// hlBatchGroup is one key and the due strategies that share it, in due order.
type hlBatchGroup struct {
	Key     hlBatchKey
	Members []StrategyConfig
}

// Batchable reports whether the group is worth one batched call. A single
// member is dispatched by the untouched per-strategy path.
func (g hlBatchGroup) Batchable() bool { return len(g.Members) >= 2 }

// MemberIDs returns the group's strategy IDs in due order.
func (g hlBatchGroup) MemberIDs() []string {
	out := make([]string, 0, len(g.Members))
	for _, sc := range g.Members {
		out = append(out, sc.ID)
	}
	return out
}

// hyperliquidBatchEligible reports whether sc's check can ride a batched call.
//
// The argv allowlist is deliberately strict: sc.Args must be exactly the three
// positionals the script expects plus an optional --mode. Any other configured
// flag would have to be translated into the slot envelope, and a flag the
// translator does not know about would be silently dropped — so an unknown
// argv shape falls back to the per-strategy path instead.
func hyperliquidBatchEligible(sc StrategyConfig) bool {
	if sc.Type != "perps" || sc.Platform != "hyperliquid" {
		return false
	}
	if strings.TrimSpace(sc.Script) != hyperliquidCheckScript {
		return false
	}
	return hyperliquidBatchArgsSupported(sc.Args)
}

func hyperliquidBatchArgsSupported(args []string) bool {
	if len(args) < 3 {
		return false
	}
	for _, positional := range args[:3] {
		trimmed := strings.TrimSpace(positional)
		if trimmed == "" || strings.HasPrefix(trimmed, "-") {
			return false
		}
	}
	for i := 3; i < len(args); i++ {
		arg := args[i]
		switch {
		case strings.HasPrefix(arg, "--mode="):
		case arg == "--mode":
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
				return false
			}
			i++
		default:
			return false
		}
	}
	return true
}

// hyperliquidModeFromArgs extracts --mode from sc.Args, matching the check
// script's argparse default of "paper".
func hyperliquidModeFromArgs(args []string) string {
	for i, arg := range args {
		if strings.HasPrefix(arg, "--mode=") {
			if v := strings.TrimSpace(strings.TrimPrefix(arg, "--mode=")); v != "" {
				return v
			}
		}
		if arg == "--mode" && i+1 < len(args) {
			if v := strings.TrimSpace(args[i+1]); v != "" {
				return v
			}
		}
	}
	return "paper"
}

// hyperliquidTimeframe extracts the timeframe positional from perps args.
func hyperliquidTimeframe(args []string) string {
	if len(args) >= 3 {
		return strings.TrimSpace(args[2])
	}
	return ""
}

// hlBatchKeyForStrategy derives sc's batch key. ok=false means sc is not
// batchable at all.
func hlBatchKeyForStrategy(sc StrategyConfig, cfg *Config) (hlBatchKey, bool) {
	if !hyperliquidBatchEligible(sc) {
		return hlBatchKey{}, false
	}
	symbol := hyperliquidSymbol(sc.Args)
	timeframe := hyperliquidTimeframe(sc.Args)
	if symbol == "" || timeframe == "" {
		return hlBatchKey{}, false
	}
	var rc *RegimeConfig
	if cfg != nil {
		rc = cfg.Regime
	}
	limit := hlBatchPythonDefaultOhlcvLimit
	if rc != nil && rc.Enabled {
		limit = regimeRequiredOhlcvLimit(rc)
	}
	return hlBatchKey{
		DataPlatform: strategyRegimeDataPlatform(sc),
		Symbol:       symbol,
		Timeframe:    timeframe,
		OhlcvLimit:   limit,
		ATRMethod:    resolveATRMethod(sc, cfg),
	}, true
}

// partitionHyperliquidBatchGroups partitions the DUE strategies by batch key.
// Pure: no locks, no globals, no I/O — call it with the already-built due list
// so a strategy whose interval has not elapsed can never enter a batch. Groups
// come back sorted by key so operator logs and tests are deterministic;
// members keep due order.
func partitionHyperliquidBatchGroups(due []StrategyConfig, cfg *Config) []hlBatchGroup {
	byKey := make(map[hlBatchKey][]StrategyConfig)
	for _, sc := range due {
		key, ok := hlBatchKeyForStrategy(sc, cfg)
		if !ok {
			continue
		}
		byKey[key] = append(byKey[key], sc)
	}
	out := make([]hlBatchGroup, 0, len(byKey))
	for key, members := range byKey {
		out = append(out, hlBatchGroup{Key: key, Members: members})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key.String() < out[j].Key.String() })
	return out
}

// hlBatchSlot is one strategy's per-slot payload inside the stdin envelope.
// Every field here is deliberately OUTSIDE hlBatchKey.
type hlBatchSlot struct {
	ID              string          `json:"id"`
	Strategy        string          `json:"strategy"`
	Mode            string          `json:"mode"`
	HTFFilter       bool            `json:"htf_filter,omitempty"`
	StrategyRefs    json.RawMessage `json:"strategy_refs,omitempty"`
	RegimeATRWindow string          `json:"regime_atr_window,omitempty"`
	PositionSide    string          `json:"position_side,omitempty"`
	PositionCtx     map[string]any  `json:"position_ctx,omitempty"`
}

// hlBatchRequest is the stdin envelope check_hyperliquid.py --batch-check reads.
type hlBatchRequest struct {
	Version int           `json:"v"`
	Slots   []hlBatchSlot `json:"slots"`
}

// HyperliquidBatchSlotResult is one slot's decision inside the batch response.
// It embeds HyperliquidResult so a slot flows into the existing per-strategy
// pipeline verbatim.
type HyperliquidBatchSlotResult struct {
	ID string `json:"id"`
	HyperliquidResult
}

// HyperliquidBatchResult is the batch response envelope. ErrorScope ==
// "shared_state" means the shared market data failed and no slot ran.
type HyperliquidBatchResult struct {
	Platform   string                       `json:"platform"`
	Symbol     string                       `json:"symbol"`
	Timeframe  string                       `json:"timeframe"`
	Timestamp  string                       `json:"timestamp"`
	Error      string                       `json:"error"`
	ErrorScope string                       `json:"error_scope"`
	Results    []HyperliquidBatchSlotResult `json:"results"`
}

const hlBatchSharedStateScope = "shared_state"

// buildHyperliquidBatchSlot builds one slot from the same inputs the
// per-strategy argv builder consumes. scForCheck must already carry the #998
// profile params and the on-chain-protection close filter, exactly as
// runHyperliquidCheck applies them.
func buildHyperliquidBatchSlot(sc StrategyConfig, posCtx PositionCtx, regime *RegimeConfig) (hlBatchSlot, error) {
	slot := hlBatchSlot{
		ID:              sc.ID,
		Strategy:        strategyNameFromArgs(sc.Args),
		Mode:            hyperliquidModeFromArgs(sc.Args),
		HTFFilter:       sc.HTFFilter,
		RegimeATRWindow: hlBatchRegimeATRWindow(sc, regime),
	}
	scForCheck := strategyConfigWithOnChainProtectionFilter(sc)
	refsArgs, err := buildStrategyRefsArg(scForCheck)
	if err != nil {
		return hlBatchSlot{}, fmt.Errorf("marshal strategy refs: %w", err)
	}
	if len(refsArgs) == 2 {
		slot.StrategyRefs = json.RawMessage(refsArgs[1])
	}
	// Mirror appendOpenCloseArgs exactly: position context only travels when
	// the strategy uses the open/close composition model, and a zero float is
	// omitted (the flag would not be appended, so Python sees argparse's None).
	if usesOpenCloseConfig(scForCheck) {
		ctx := map[string]any{}
		if side := strings.TrimSpace(posCtx.Side); side != "" {
			slot.PositionSide = side
			ctx["side"] = strings.ToLower(side)
		}
		hlBatchPutFloat(ctx, "avg_cost", posCtx.AvgCost)
		hlBatchPutFloat(ctx, "current_quantity", posCtx.Quantity)
		hlBatchPutFloat(ctx, "initial_quantity", posCtx.InitialQuantity)
		hlBatchPutFloat(ctx, "entry_atr", posCtx.EntryATR)
		if r := strings.TrimSpace(posCtx.Regime); r != "" {
			ctx["regime"] = r
		}
		if len(ctx) > 0 {
			slot.PositionCtx = ctx
		}
	}
	return slot, nil
}

// hlBatchPutFloat mirrors appendPositionFloatArg's zero-skip so a batched slot
// and the per-strategy argv carry the same set of position fields. The value
// is round-tripped through the same 'f'/-1 formatting the argv uses, so the
// two paths hand Python identical decimal text.
func hlBatchPutFloat(ctx map[string]any, key string, value float64) {
	if value == 0 {
		return
	}
	parsed, err := strconv.ParseFloat(strconv.FormatFloat(value, 'f', -1, 64), 64)
	if err != nil {
		return
	}
	ctx[key] = parsed
}

// hlBatchRegimeATRWindow mirrors appendStrategyRegimeWindowArgs.
func hlBatchRegimeATRWindow(sc StrategyConfig, regime *RegimeConfig) string {
	if regime == nil || !regime.Enabled || !regimeMultiWindowEnabled(regime) {
		return ""
	}
	if key := resolveStrategyRegimeWindow(sc, "atr", regime); key != "" && key != regimeWindowDefaultKey {
		return key
	}
	return ""
}

// hlBatchSharedArgs builds the shared argv for one group. markPrice <= 0 omits
// the flag, matching runHyperliquidCheck.
func hlBatchSharedArgs(key hlBatchKey, regime *RegimeConfig, regimePayloadJSON string, hasRegimePayload bool, markPrice float64) []string {
	args := []string{
		"--batch-check",
		"--symbol=" + key.Symbol,
		"--timeframe=" + key.Timeframe,
		"--ohlcv-limit", strconv.Itoa(key.OhlcvLimit),
		"--atr-method=" + key.ATRMethod,
	}
	if regime != nil && regime.Enabled {
		args = append(args, "--regime-enabled")
		if blob := regimeWindowsSpecJSON(regime); blob != "" {
			args = append(args, "--regime-windows-spec-json", blob)
		}
	}
	if hasRegimePayload {
		args = append(args, "--regime-payload-json", regimePayloadJSON)
	}
	if markPrice > 0 {
		args = append(args, fmt.Sprintf("--mark-price=%g", markPrice))
	}
	return args
}

// hlBatchAlertConfig is the synthetic StrategyConfig identity a shared-state
// failure is tracked under, so the existing 3-strike / transient / recovery
// machinery throttles per GROUP rather than firing one DM per member
// (regimeBundleAlertConfig pattern, #829).
func hlBatchAlertConfig(key hlBatchKey) StrategyConfig {
	return StrategyConfig{
		ID:       "hl-batch[" + key.Symbol + "/" + key.Timeframe + "]",
		Platform: key.DataPlatform,
		Script:   hyperliquidCheckScript,
	}
}

// parseHyperliquidBatchOutput parses a batch response. It mirrors
// RunHyperliquidCheck's contract: JSON is parsed even on a non-zero exit,
// because the script prints its envelope and then exits 1 when any slot failed.
func parseHyperliquidBatchOutput(stdout []byte) (*HyperliquidBatchResult, error) {
	var out HyperliquidBatchResult
	if err := json.Unmarshal(stdout, &out); err != nil {
		return nil, fmt.Errorf("parse batch output: %w (stdout: %s)", err, string(stdout))
	}
	return &out, nil
}

// hyperliquidBatchSlotFingerprint renders the exact per-slot inputs a check
// would carry, as canonical JSON. The pre-pass records it alongside each
// member's outcome and runHyperliquidCheck recomputes it at dispatch time: if
// anything the check reads changed between the snapshot and the strategy's own
// loop iteration, the cached slot is discarded and that strategy spawns its own
// check. That makes "the batched decision used this strategy's current inputs"
// a checked fact rather than an argument about what else the cycle can touch.
//
// The cycle mark price is deliberately NOT part of the signature. --mark-price
// only freshens the REPORTED price (check_hyperliquid.py resolves it into
// price_override, which overwrites output["price"] and nothing else); the
// decision reads market_ctx["mark_price"] = the last closed bar's close, which
// travels in the candles. Folding the mark in made the first member's rounded
// write-back (prices[symbol] = round(mid, 2)) invalidate every later member of
// the same group, so the cycle paid for the batch AND still spawned N-1 checks.
// The dispatch-time mark is applied to the cached decision instead — see
// hyperliquidBatchDisplayPrice.
func hyperliquidBatchSlotFingerprint(sc StrategyConfig, posCtx PositionCtx, regime *RegimeConfig) (string, error) {
	slot, err := buildHyperliquidBatchSlot(sc, posCtx, regime)
	if err != nil {
		return "", err
	}
	blob, err := json.Marshal(struct {
		Slot hlBatchSlot `json:"slot"`
	}{Slot: slot})
	if err != nil {
		return "", err
	}
	return string(blob), nil
}

// hyperliquidBatchDisplayPrice renders the dispatch-time mark exactly as the
// per-strategy path would report it. check_hyperliquid.py emits
// round(price, 2), so a batched member that adopts the cycle's current mark
// must round identically or the two lanes would report — and paper-fill at —
// different prices for the same decision.
func hyperliquidBatchDisplayPrice(markPrice float64) float64 {
	return math.Round(markPrice*100) / 100
}

// hlBatchMemberOutcome is one member's cached result for this cycle.
// Result != nil means the slot decided; Err != "" means the member must take
// the failure branch runHyperliquidCheck would have taken.
type hlBatchMemberOutcome struct {
	Result *HyperliquidResult
	// Err is the failure message when Result is nil.
	Err string
	// Mode selects which per-strategy alert branch the member takes.
	Mode scriptFailureMode
	// Stderr is the batch call's stderr, logged per member.
	Stderr string
	// Fingerprint is the slot input signature recorded at snapshot time; see
	// hyperliquidBatchSlotFingerprint.
	Fingerprint string
}

// hlBatchCycleResults is the cycle-local map the dispatch loop consumes. Built
// before the loop and read-only afterwards, so it needs no lock.
type hlBatchCycleResults struct {
	byStrategy map[string]hlBatchMemberOutcome
}

func (r *hlBatchCycleResults) lookup(id string) (hlBatchMemberOutcome, bool) {
	if r == nil || r.byStrategy == nil {
		return hlBatchMemberOutcome{}, false
	}
	out, ok := r.byStrategy[id]
	return out, ok
}

func (r *hlBatchCycleResults) put(id string, outcome hlBatchMemberOutcome) {
	if r.byStrategy == nil {
		r.byStrategy = make(map[string]hlBatchMemberOutcome)
	}
	r.byStrategy[id] = outcome
}

// hlBatchFallbackTracker records consecutive shared-state failures per group
// key and decides whether a group may batch this cycle.
type hlBatchFallbackTracker struct {
	mu      sync.Mutex
	entries map[hlBatchKey]*hlBatchFallbackEntry
}

type hlBatchFallbackEntry struct {
	consecutiveFailures int
	fallenBack          bool
	cyclesSinceRetry    int
}

var hlBatchFallback = &hlBatchFallbackTracker{}

// Allow reports whether key may run a batched call this cycle. A group in
// fallback retries every hlBatchFallbackRetryEvery cycles.
func (t *hlBatchFallbackTracker) Allow(key hlBatchKey) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.entries[key]
	if e == nil || !e.fallenBack {
		return true
	}
	e.cyclesSinceRetry++
	if e.cyclesSinceRetry >= hlBatchFallbackRetryEvery {
		e.cyclesSinceRetry = 0
		return true
	}
	return false
}

// RecordSharedFailure counts one shared-state failure and reports whether this
// failure is the one that trips the fallback.
func (t *hlBatchFallbackTracker) RecordSharedFailure(key hlBatchKey) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.entries == nil {
		t.entries = make(map[hlBatchKey]*hlBatchFallbackEntry)
	}
	e := t.entries[key]
	if e == nil {
		e = &hlBatchFallbackEntry{}
		t.entries[key] = e
	}
	e.consecutiveFailures++
	if e.fallenBack {
		return false
	}
	if e.consecutiveFailures >= hlBatchSharedFailureFallbackThreshold {
		e.fallenBack = true
		e.cyclesSinceRetry = 0
		return true
	}
	return false
}

// RecordSuccess clears the streak and reports whether the group just came back
// from fallback.
func (t *hlBatchFallbackTracker) RecordSuccess(key hlBatchKey) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.entries[key]
	if e == nil {
		return false
	}
	recovered := e.fallenBack
	delete(t.entries, key)
	return recovered
}

// hyperliquidBatchEnabled reports whether batching is permitted at all.
// GO_TRADER_HL_BATCH=0 is the operator rollback: instant, no rebuild, no
// config key, every group reverts to the per-strategy path.
func hyperliquidBatchEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(hlBatchDisabledEnv))) {
	case "0", "false", "off", "no":
		return false
	}
	return true
}

// runHyperliquidBatchCheckFn is the subprocess invoker — a package var so Go
// tests never spawn Python (runRegimeBundleCheckFn precedent).
var runHyperliquidBatchCheckFn = runHyperliquidBatchCheck

func runHyperliquidBatchCheck(script string, args []string, stdinJSON []byte) (*HyperliquidBatchResult, string, error) {
	stdout, stderr, err := runPythonWithTimeout(shutdownReadOnlyCtx, script, args, stdinJSON, hlBatchTimeout)
	stderrStr := string(stderr)
	parsed, parseErr := parseHyperliquidBatchOutput(stdout)
	if err != nil {
		if parseErr == nil && (parsed.Error != "" || len(parsed.Results) > 0) {
			return parsed, stderrStr, nil
		}
		return nil, stderrStr, fmt.Errorf("batch script error: %w (stderr: %s)", err, stderrStr)
	}
	if parseErr != nil {
		return nil, stderrStr, parseErr
	}
	return parsed, stderrStr, nil
}

// hlBatchGroupInput is one group plus the per-member snapshot the slot builder
// needs, captured under the Phase-1 RLock.
type hlBatchGroupInput struct {
	Key       hlBatchKey
	Members   []StrategyConfig
	PosCtx    map[string]PositionCtx
	MarkPrice float64
}

// runHyperliquidBatchGroups executes one batched call per batchable group and
// returns the cycle-local result map. It never mutates state and never holds a
// lock; groups run sequentially through the existing read-only Python lane.
func runHyperliquidBatchGroups(inputs []hlBatchGroupInput, cfg *Config, notifier *MultiNotifier, logf func(string, ...any)) *hlBatchCycleResults {
	results := &hlBatchCycleResults{}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	var rc *RegimeConfig
	if cfg != nil {
		rc = cfg.Regime
	}
	for _, in := range inputs {
		payloadJSON, hasPayload, uniform := hlBatchGroupRegimePayload(in.Members, rc)
		if !uniform {
			// Impossible for a well-formed group (every member shares the
			// regime signature the key implies), so a mismatch means an
			// assumption broke: do not batch, and say so loudly.
			logf("[WARN] hl-batch %s: members disagree on the injected regime payload; falling back to per-strategy checks", in.Key)
			continue
		}
		args := hlBatchSharedArgs(in.Key, rc, payloadJSON, hasPayload, in.MarkPrice)
		req := hlBatchRequest{Version: hlBatchProtocolVersion}
		fingerprints := make(map[string]string, len(in.Members))
		slotErr := false
		for _, sc := range in.Members {
			slot, err := buildHyperliquidBatchSlot(sc, in.PosCtx[sc.ID], rc)
			if err != nil {
				logf("[WARN] hl-batch %s: slot build failed for %s: %v; falling back to per-strategy checks", in.Key, sc.ID, err)
				slotErr = true
				break
			}
			fp, err := hyperliquidBatchSlotFingerprint(sc, in.PosCtx[sc.ID], rc)
			if err != nil {
				logf("[WARN] hl-batch %s: fingerprint failed for %s: %v; falling back to per-strategy checks", in.Key, sc.ID, err)
				slotErr = true
				break
			}
			fingerprints[sc.ID] = fp
			req.Slots = append(req.Slots, slot)
		}
		if slotErr {
			continue
		}
		stdin, err := json.Marshal(req)
		if err != nil {
			logf("[WARN] hl-batch %s: marshal slots: %v; falling back to per-strategy checks", in.Key, err)
			continue
		}
		logf("[INFO] hl-batch %s: %d strategies in one call (%s)", in.Key, len(in.Members), strings.Join(in.MemberIDsOrdered(), ", "))
		started := time.Now()
		out, stderr, err := runHyperliquidBatchCheckFn(hyperliquidCheckScript, args, stdin)
		elapsed := time.Since(started)
		if err != nil {
			hlBatchApplySharedFailure(in, err.Error(), stderr, notifier, logf)
			continue
		}
		if out.ErrorScope == hlBatchSharedStateScope || (out.Error != "" && len(out.Results) == 0) {
			msg := out.Error
			if msg == "" {
				msg = "shared state failed with no detail"
			}
			hlBatchApplySharedFailure(in, msg, stderr, notifier, logf)
			continue
		}
		hlBatchApplySlots(results, in, fingerprints, out, stderr, logf)
		clearBatchSharedStateFailure(notifier, hlBatchAlertConfig(in.Key))
		if recovered := hlBatchFallback.RecordSuccess(in.Key); recovered {
			logf("[INFO] hl-batch %s: shared state recovered; batching resumed", in.Key)
		}
		logf("[INFO] hl-batch %s: %d slots returned in %s", in.Key, len(out.Results), elapsed.Round(time.Millisecond))
	}
	return results
}

// MemberIDsOrdered returns the group's member IDs in due order.
func (in hlBatchGroupInput) MemberIDsOrdered() []string {
	out := make([]string, 0, len(in.Members))
	for _, sc := range in.Members {
		out = append(out, sc.ID)
	}
	return out
}

// hlBatchApplySharedFailure records ONE alert on the synthetic group identity
// and leaves every member as a map MISS.
//
// A miss is the whole point. The member's own spawn then runs in the SAME
// cycle, so a fault confined to the batching path — envelope drift, an
// OOM-killed child, N slots blowing the single shared deadline that each
// strategy used to get to itself — cannot blank close evaluation, trailing-SL
// cancel+replace, the ratchet, protection sync or hedge sync for the whole
// coin. Caching the failure instead would have skipped all of that for every
// member, for up to hlBatchSharedFailureFallbackThreshold cycles. Falling
// through is never worse than the unbatched path: a genuine upstream outage
// fails those spawns exactly as it does today.
//
// Member failure trackers stay untouched here for the same reason as before —
// a shared outage is not the member's script failing, so the group identity
// carries the alert and any per-member DM comes from that member's own spawn.
func hlBatchApplySharedFailure(in hlBatchGroupInput, errMsg, stderr string, notifier *MultiNotifier, logf func(string, ...any)) {
	logf("[ERROR] hl-batch %s: shared state failed (%d strategies fall back to their own checks this cycle): %s", in.Key, len(in.Members), errMsg)
	if stderr != "" {
		logf("[ERROR] hl-batch %s: stderr: %s", in.Key, stderr)
	}
	notifyBatchSharedStateFailure(notifier, hlBatchAlertConfig(in.Key), errMsg, in.MemberIDsOrdered())
	if tripped := hlBatchFallback.RecordSharedFailure(in.Key); tripped {
		logf("[WARN] hl-batch %s: %d consecutive shared-state failures; reverting this group to per-strategy checks (retry every %d cycles)",
			in.Key, hlBatchSharedFailureFallbackThreshold, hlBatchFallbackRetryEvery)
	}
}

// hlBatchApplySlots maps the response's slots back onto members. A member with
// no slot, or a duplicated slot id, is treated as a hard crash for that member
// alone — never silently skipped, because a skipped member would run its whole
// downstream block on a stale decision.
func hlBatchApplySlots(results *hlBatchCycleResults, in hlBatchGroupInput, fingerprints map[string]string, out *HyperliquidBatchResult, stderr string, logf func(string, ...any)) {
	byID := make(map[string]*HyperliquidResult, len(out.Results))
	dupes := map[string]bool{}
	for i := range out.Results {
		slot := out.Results[i]
		id := strings.TrimSpace(slot.ID)
		if id == "" {
			continue
		}
		if _, seen := byID[id]; seen {
			dupes[id] = true
			continue
		}
		res := slot.HyperliquidResult
		byID[id] = &res
	}
	expected := make(map[string]bool, len(in.Members))
	for _, sc := range in.Members {
		expected[sc.ID] = true
		res, ok := byID[sc.ID]
		switch {
		case dupes[sc.ID]:
			results.put(sc.ID, hlBatchMemberOutcome{
				Err:         "batch response carried duplicate slots for this strategy",
				Mode:        scriptFailureCrash,
				Stderr:      stderr,
				Fingerprint: fingerprints[sc.ID],
			})
		case !ok:
			results.put(sc.ID, hlBatchMemberOutcome{
				Err:         "batch response missing slot for this strategy",
				Mode:        scriptFailureCrash,
				Stderr:      stderr,
				Fingerprint: fingerprints[sc.ID],
			})
		case res.Error != "":
			// Soft slot error: exactly the per-strategy "Script returned
			// error" branch. Result is cleared so no downstream code can read
			// a zero-signal error payload as a decision to hold.
			results.put(sc.ID, hlBatchMemberOutcome{
				Err:         res.Error,
				Mode:        scriptFailureError,
				Stderr:      stderr,
				Fingerprint: fingerprints[sc.ID],
			})
		default:
			results.put(sc.ID, hlBatchMemberOutcome{Result: res, Stderr: stderr, Fingerprint: fingerprints[sc.ID]})
		}
	}
	for id := range byID {
		if !expected[id] {
			logf("[WARN] hl-batch %s: response carried unexpected slot id %q", in.Key, id)
		}
	}
}

// snapshotHyperliquidBatchGroups captures, under ONE mu.RLock read, the
// per-member position context each slot needs.
//
// Placement matters: this must run AFTER reconcileHyperliquidAccountPositions
// has booked externally-closed positions, or a close evaluator would be handed
// a position the exchange no longer holds. It therefore cannot be folded into
// the earlier due-list RLock. It is a read-only Phase-1-shaped block: no
// writes, no strategiesMu, no lock held across the subprocess.
//
// The #998 regime-profile params are resolved here on a LOCAL copy so the slot
// carries the same merged params the per-strategy argv would. resolveRegimeProfile
// is pure and the dispatch loop remains the sole committer of the switch state.
func snapshotHyperliquidBatchGroups(groups []hlBatchGroup, state *AppState, mu *sync.RWMutex, cfg *Config, prices map[string]float64) []hlBatchGroupInput {
	var rc *RegimeConfig
	if cfg != nil {
		rc = cfg.Regime
	}
	type memberSnapshot struct {
		sc           StrategyConfig
		posCtx       PositionCtx
		profileState *RegimeProfileState
	}
	pending := make([][]memberSnapshot, len(groups))

	mu.RLock()
	for gi, group := range groups {
		if !group.Batchable() {
			continue
		}
		for _, sc := range group.Members {
			stratState := state.Strategies[sc.ID]
			if stratState == nil {
				// The dispatch loop skips a strategy with no state; a batch
				// must not evaluate one either.
				continue
			}
			snap := memberSnapshot{sc: sc}
			if sym := hyperliquidSymbol(sc.Args); sym != "" {
				if pos, ok := stratState.Positions[sym]; ok {
					snap.posCtx = positionCtxForCheck(sc, pos, rc)
				}
			}
			if stratState.RegimeProfile != nil {
				cp := *stratState.RegimeProfile
				snap.profileState = &cp
			}
			pending[gi] = append(pending[gi], snap)
		}
	}
	mu.RUnlock()

	out := make([]hlBatchGroupInput, 0, len(groups))
	for gi, group := range groups {
		snaps := pending[gi]
		if len(snaps) < 2 {
			continue
		}
		in := hlBatchGroupInput{
			Key:    group.Key,
			PosCtx: make(map[string]PositionCtx, len(snaps)),
		}
		if prices != nil {
			if mid, ok := prices[group.Key.Symbol]; ok && mid > 0 {
				in.MarkPrice = mid
			}
		}
		for _, snap := range snaps {
			sc := snap.sc
			if sc.Platform == "hyperliquid" && sc.RegimeProfileAllocation.IsConfigured() {
				palPayload := globalRegimeStore.PayloadForStrategy(sc, rc)
				palBarTime := globalRegimeStore.BarTimeForStrategy(sc, rc)
				palLabel := palPayload.Label(sc.RegimeProfileAllocation.Window, rc)
				active, _ := resolveRegimeProfile(sc.RegimeProfileAllocation, palLabel, palBarTime, snap.profileState, snap.posCtx.Quantity, snap.posCtx.Profile)
				applyRegimeProfileParams(&sc, sc.RegimeProfileAllocation, active)
			}
			in.Members = append(in.Members, sc)
			in.PosCtx[sc.ID] = snap.posCtx
		}
		out = append(out, in)
	}
	return out
}

// runHyperliquidBatchPrePass is the whole #1442 pre-pass: group the due
// strategies, snapshot their inputs, and run one batched call per group.
// Returns nil when batching is disabled or nothing is batchable, which makes
// every dispatch a map miss and the cycle byte-identical to before this issue.
func runHyperliquidBatchPrePass(due []StrategyConfig, state *AppState, mu *sync.RWMutex, cfg *Config, prices map[string]float64, notifier *MultiNotifier, logf func(string, ...any)) *hlBatchCycleResults {
	if !hyperliquidBatchEnabled() {
		return nil
	}
	groups := partitionHyperliquidBatchGroups(due, cfg)
	batchable := make([]hlBatchGroup, 0, len(groups))
	for _, g := range groups {
		if !g.Batchable() {
			continue
		}
		if !hlBatchFallback.Allow(g.Key) {
			if logf != nil {
				logf("[INFO] hl-batch %s: in shared-failure fallback; running per-strategy checks this cycle", g.Key)
			}
			continue
		}
		batchable = append(batchable, g)
	}
	if len(batchable) == 0 {
		return nil
	}
	inputs := snapshotHyperliquidBatchGroups(batchable, state, mu, cfg, prices)
	if len(inputs) == 0 {
		return nil
	}
	return runHyperliquidBatchGroups(inputs, cfg, notifier, logf)
}

// hlBatchGroupRegimePayload resolves the group's shared --regime-payload-json.
// uniform=false means the members disagreed, which the caller treats as a
// reason not to batch rather than a reason to guess.
func hlBatchGroupRegimePayload(members []StrategyConfig, rc *RegimeConfig) (payload string, has bool, uniform bool) {
	for i, sc := range members {
		raw, ok := globalRegimeStore.InjectionJSONForStrategy(sc, rc)
		if i == 0 {
			payload, has = raw, ok
			continue
		}
		if ok != has || raw != payload {
			return "", false, false
		}
	}
	return payload, has, true
}

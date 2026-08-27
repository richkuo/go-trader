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

const hyperliquidCheckScript = "shared_scripts/check_hyperliquid.py"

const hlBatchPythonDefaultOhlcvLimit = 200

const hlBatchProtocolVersion = 1

var hlBatchTimeout = scriptTimeout

const hlBatchDisabledEnv = "GO_TRADER_HL_BATCH"

const hlBatchSharedFailureFallbackThreshold = 3

const hlBatchFallbackRetryEvery = 10

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

type hlBatchGroup struct {
	Key     hlBatchKey
	Members []StrategyConfig
}

func (g hlBatchGroup) Batchable() bool { return len(g.Members) >= 2 }

func (g hlBatchGroup) MemberIDs() []string {
	out := make([]string, 0, len(g.Members))
	for _, sc := range g.Members {
		out = append(out, sc.ID)
	}
	return out
}

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

func hyperliquidTimeframe(args []string) string {
	if len(args) >= 3 {
		return strings.TrimSpace(args[2])
	}
	return ""
}

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

type hlBatchRequest struct {
	Version int           `json:"v"`
	Slots   []hlBatchSlot `json:"slots"`
}

type HyperliquidBatchSlotResult struct {
	ID string `json:"id"`
	HyperliquidResult
}

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

func hlBatchRegimeATRWindow(sc StrategyConfig, regime *RegimeConfig) string {
	if regime == nil || !regime.Enabled || !regimeMultiWindowEnabled(regime) {
		return ""
	}
	if key := resolveStrategyRegimeWindow(sc, "atr", regime); key != "" && key != regimeWindowDefaultKey {
		return key
	}
	return ""
}

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

func hlBatchAlertConfig(key hlBatchKey) StrategyConfig {
	return StrategyConfig{
		ID:       "hl-batch[" + key.Symbol + "/" + key.Timeframe + "]",
		Platform: key.DataPlatform,
		Script:   hyperliquidCheckScript,
	}
}

func parseHyperliquidBatchOutput(stdout []byte) (*HyperliquidBatchResult, error) {
	var out HyperliquidBatchResult
	if err := json.Unmarshal(stdout, &out); err != nil {
		return nil, fmt.Errorf("parse batch output: %w (stdout: %s)", err, string(stdout))
	}
	return &out, nil
}

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

func hyperliquidBatchDisplayPrice(markPrice float64) float64 {
	return math.Round(markPrice*100) / 100
}

type hlBatchMemberOutcome struct {
	Result      *HyperliquidResult
	Err         string
	Mode        scriptFailureMode
	Stderr      string
	Fingerprint string
}

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

func hyperliquidBatchEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(hlBatchDisabledEnv))) {
	case "0", "false", "off", "no":
		return false
	}
	return true
}

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

type hlBatchGroupInput struct {
	Key       hlBatchKey
	Members   []StrategyConfig
	PosCtx    map[string]PositionCtx
	MarkPrice float64
}

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
		drift := hlBatchApplySlots(results, in, fingerprints, out, stderr, logf)
		if stderr != "" {
			logf("[INFO] hl-batch %s: stderr: %s", in.Key, stderr)
		}
		if drift > 0 {
			msg := fmt.Sprintf("batch response accounted for %d of %d strategies; the rest ran their own checks",
				len(in.Members)-drift, len(in.Members))
			notifyBatchSharedStateFailure(notifier, hlBatchAlertConfig(in.Key), msg, in.MemberIDsOrdered())
			if tripped := hlBatchFallback.RecordSharedFailure(in.Key); tripped {
				logf("[WARN] hl-batch %s: %d consecutive incomplete batch responses; reverting this group to per-strategy checks (retry every %d cycles)",
					in.Key, hlBatchSharedFailureFallbackThreshold, hlBatchFallbackRetryEvery)
			}
		} else {
			clearBatchSharedStateFailure(notifier, hlBatchAlertConfig(in.Key))
			if recovered := hlBatchFallback.RecordSuccess(in.Key); recovered {
				logf("[INFO] hl-batch %s: shared state recovered; batching resumed", in.Key)
			}
		}
		logf("[INFO] hl-batch %s: %d slots returned in %s", in.Key, len(out.Results), elapsed.Round(time.Millisecond))
	}
	return results
}

func (in hlBatchGroupInput) MemberIDsOrdered() []string {
	out := make([]string, 0, len(in.Members))
	for _, sc := range in.Members {
		out = append(out, sc.ID)
	}
	return out
}

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

func hlBatchApplySlots(results *hlBatchCycleResults, in hlBatchGroupInput, fingerprints map[string]string, out *HyperliquidBatchResult, stderr string, logf func(string, ...any)) int {
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
	drift := 0
	for _, sc := range in.Members {
		expected[sc.ID] = true
		res, ok := byID[sc.ID]
		switch {
		case dupes[sc.ID]:
			logf("[ERROR] hl-batch %s: response carried duplicate slots for %s; that strategy runs its own check this cycle", in.Key, sc.ID)
			drift++
		case !ok:
			logf("[ERROR] hl-batch %s: response is missing the slot for %s; that strategy runs its own check this cycle", in.Key, sc.ID)
			drift++
		case res.Error != "":
			results.put(sc.ID, hlBatchMemberOutcome{
				Err:         res.Error,
				Mode:        scriptFailureError,
				Stderr:      stderr,
				Fingerprint: fingerprints[sc.ID],
			})
		default:
			results.put(sc.ID, hlBatchMemberOutcome{Result: res, Fingerprint: fingerprints[sc.ID]})
		}
	}
	for id := range byID {
		if !expected[id] {
			logf("[WARN] hl-batch %s: response carried unexpected slot id %q", in.Key, id)
		}
	}
	return drift
}

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

package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	researchRoutePlatform = "hyperliquid"
	researchRouteType     = "perps"
	researchRouteScript   = hyperliquidCheckScript

	quoteEngineHoldReason      = "quote_engine_unavailable"
	quoteEngineHoldTrackingURL = "https://github.com/richkuo/go-trader/issues/1923"
)

var researchRouteMarketFeeds = []string{"shared", "websocket"}

var researchPaperRouteStrategies = map[string]bool{
	"cumulative_volume_delta_divergence": true,
	"macro_event_breakout":               true,
	"bitcoin_lead_lag":                   true,
	"volume_profile_reclaim":             true,
	"perpetual_premium_reversion":        true,
	"order_book_imbalance":               true,
	"inventory_market_making":            true,
}

const (
	researchRolePositionalOpen = "positional open"
	researchRoleEffectiveOpen  = "effective open"
	researchRoleFallbackClose  = "fallback close"
	researchRoleNativeClose    = "native close"
	researchRoleQuoteBlock     = "quote block"
)

type researchReference struct {
	Role string
	Name string
}

func (r researchReference) label() string {
	if r.Role == researchRoleQuoteBlock {
		return "quote block"
	}
	return fmt.Sprintf("%s strategy %q", r.Role, r.Name)
}

func researchReferences(sc StrategyConfig) []researchReference {
	if sc.Type == "options" {
		return nil
	}
	var out []researchReference
	seen := map[researchReference]bool{}
	add := func(ref researchReference) {
		ref.Name = strings.TrimSpace(ref.Name)
		if seen[ref] {
			return
		}
		seen[ref] = true
		out = append(out, ref)
	}
	if name := strategyNameFromArgs(sc.Args); name != "" {
		if _, refused := productionRefusedStrategies[name]; refused {
			add(researchReference{Role: researchRolePositionalOpen, Name: name})
		}
	}
	if name := effectiveOpenStrategy(sc); name != "" {
		if _, refused := productionRefusedStrategies[name]; refused {
			add(researchReference{Role: researchRoleEffectiveOpen, Name: name})
		}
	}
	for _, ref := range sc.closeRefs() {
		name := canonicalCloseStrategyName(ref.Name)
		if name == "" {
			continue
		}
		if researchOnlyCloseStrategies[name] {
			add(researchReference{Role: researchRoleNativeClose, Name: name})
			continue
		}
		if isNativeCloseStrategy(name) {
			continue
		}
		if _, refused := productionRefusedStrategies[name]; refused {
			add(researchReference{Role: researchRoleFallbackClose, Name: name})
		}
	}
	if sc.Quote != nil {
		add(researchReference{Role: researchRoleQuoteBlock})
	}
	return out
}

func hasResearchReferences(sc StrategyConfig) bool {
	return len(researchReferences(sc)) > 0
}

func researchModeText(mode edgeGateMode) string {
	switch mode.Kind {
	case edgeGateModeMissing:
		return "missing --mode"
	case edgeGateModeLive:
		return "explicit live mode"
	case edgeGateModeInvalid:
		return "invalid mode (" + mode.Detail + ")"
	}
	return mode.Kind
}

func researchEvidence(ref researchReference) string {
	if ev, ok := productionRefusedStrategies[ref.Name]; ok {
		return ev
	}
	if ref.Role == researchRoleQuoteBlock || researchOnlyCloseStrategies[ref.Name] {
		return productionRefusedStrategies[inventoryMarketMakingStrategyName]
	}
	return "unknown"
}

func researchRouteErrors(cfg *Config, sc StrategyConfig, ref researchReference) []string {
	var errs []string
	prefix := fmt.Sprintf("strategy %s: %s is research-only and runs only on the Hyperliquid perps paper route", sc.ID, ref.label())
	if sc.Platform != researchRoutePlatform {
		errs = append(errs, fmt.Sprintf("%s: set \"platform\": %q (got %q)", prefix, researchRoutePlatform, sc.Platform))
	}
	if sc.Type != researchRouteType {
		errs = append(errs, fmt.Sprintf("%s: set \"type\": %q (got %q)", prefix, researchRouteType, sc.Type))
	}
	if strings.TrimSpace(sc.Script) != researchRouteScript {
		errs = append(errs, fmt.Sprintf("%s: set \"script\": %q (got %q)", prefix, researchRouteScript, sc.Script))
	}
	mode := cfg.marketFeedMode()
	feedOK := false
	for _, m := range researchRouteMarketFeeds {
		if mode == m {
			feedOK = true
		}
	}
	if !feedOK {
		errs = append(errs, fmt.Sprintf("%s: its side inputs come only from the sealed market snapshot; set root \"market_feed\" to %q (got %q)",
			prefix, strings.Join(researchRouteMarketFeeds, "\" or \""), mode))
	}
	return errs
}

func researchAdmissionErrors(cfg *Config) []string {
	if cfg == nil {
		return nil
	}
	var errs []string
	for _, sc := range cfg.Strategies {
		errs = append(errs, strategyResearchAdmissionErrors(cfg, sc)...)
	}
	return errs
}

func strategyResearchAdmissionErrors(cfg *Config, sc StrategyConfig) []string {
	refs := researchReferences(sc)
	if len(refs) == 0 {
		return nil
	}
	var errs []string
	mode := rawEdgeGateMode(sc.Args)
	if mode.Kind != edgeGateModePaper {
		for _, ref := range refs {
			errs = append(errs, fmt.Sprintf("strategy %s: %s is production_refused (research-only, evidence %s): %s is refused, with or without allow_no_edge; research-only: explicit paper mode only (exactly one \"--mode=paper\" or \"--mode\", \"paper\" in args); live is refused until a separate reviewed promotion",
				sc.ID, ref.label(), researchEvidence(ref), researchModeText(mode)))
		}
		return errs
	}
	if quoteOwnedStrategy(sc) {
		return heldQuoteOwnerAdmissionErrors(cfg, sc, refs)
	}
	effOpen := effectiveOpenStrategy(sc)
	routeChecked := false
	for _, ref := range refs {
		if ref.Role == researchRoleFallbackClose && ref.Name != effOpen {
			errs = append(errs, fmt.Sprintf("strategy %s: %s cannot evaluate under open strategy %q: sealed side inputs attach only to the open strategy, so a research close slot must name the same strategy as the effective open",
				sc.ID, ref.label(), effOpen))
			continue
		}
		if !routeChecked {
			routeChecked = true
			errs = append(errs, researchRouteErrors(cfg, sc, ref)...)
		}
	}
	return errs
}

func heldQuoteOwnerAdmissionErrors(cfg *Config, sc StrategyConfig, refs []researchReference) []string {
	var errs []string
	open := effectiveOpenStrategy(sc)
	spec, isCapability := quoteCapabilityStrategies[open]
	if sc.Quote == nil || !isCapability {
		for _, ref := range strategyQuoteReferences(sc) {
			errs = append(errs, fmt.Sprintf("strategy %s: %s is quote-owned (research-only): it is refused in every mode unless the strategy is a complete quote owner (a quote block with a quote capability open strategy)", sc.ID, ref))
		}
		return errs
	}
	for _, ref := range refs {
		switch {
		case ref.Role == researchRoleQuoteBlock:
		case ref.Role == researchRoleNativeClose && ref.Name == inventoryQuoteExitCloseName:
		case (ref.Role == researchRoleEffectiveOpen || ref.Role == researchRolePositionalOpen) && ref.Name == open:
		default:
			errs = append(errs, fmt.Sprintf("strategy %s: %s is refused on a quote owner; a quote owner references only its quote capability, quote block and %s close", sc.ID, ref.label(), inventoryQuoteExitCloseName))
		}
	}
	errs = append(errs, quoteOwnerConfigErrors(sc)...)
	if open == inventoryMarketMakingStrategyName {
		for _, e := range inventoryMarketMakingConfigErrors(sc, cfg.IntervalSeconds) {
			errs = append(errs, fmt.Sprintf("strategy %s: %s policy: %s", sc.ID, open, e))
		}
	} else if spec.Validate != nil {
		if err := spec.Validate(sc.OpenStrategy.Params); err != nil {
			errs = append(errs, fmt.Sprintf("strategy %s: open strategy %s params: %v", sc.ID, open, err))
		}
	}
	if part := partitionFor(sc); part.Scope != ScopePaper {
		errs = append(errs, fmt.Sprintf("strategy %s: a held quote owner needs a paper partition, got %s", sc.ID, partitionLabel(part)))
	}
	errs = append(errs, researchRouteErrors(cfg, sc, researchReference{Role: researchRoleEffectiveOpen, Name: open})...)
	if err := quoteExclusiveOwnershipError(cfg, sc); err != nil {
		errs = append(errs, fmt.Sprintf("strategy %s: %v", sc.ID, err))
	}
	return errs
}

func quoteExclusiveOwnershipError(cfg *Config, sc StrategyConfig) error {
	if cfg == nil {
		return nil
	}
	coin := quoteCoinOf(sc)
	part := partitionFor(sc)
	for _, other := range cfg.Strategies {
		if other.ID == sc.ID {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(hyperliquidSymbol(other.Args)), coin) || other.Platform != "hyperliquid" {
			continue
		}
		op := partitionFor(other)
		if part.Scope == ScopeLive && op.Scope == ScopeLive {
			return fmt.Errorf("quote owner %s needs exclusive ownership of the live %s namespace, but %s also trades it", sc.ID, coin, other.ID)
		}
		if part.Scope == ScopePaper && op == part {
			return fmt.Errorf("quote owner %s needs exclusive ownership of %s in the %s partition, but %s also trades it", sc.ID, coin, partitionLabel(part), other.ID)
		}
	}
	return nil
}

type engineHold struct {
	StrategyID  string
	Reason      string
	Text        string
	TrackingURL string
}

func engineHoldFor(sc StrategyConfig) (engineHold, bool) {
	if !quoteOwnedStrategy(sc) {
		return engineHold{}, false
	}
	return engineHold{
		StrategyID:  sc.ID,
		Reason:      quoteEngineHoldReason,
		Text:        fmt.Sprintf("quote owner %s is held: no forward paper quote engine exists yet, so the scheduler runs no evaluation, order, fill or protection work for it", sc.ID),
		TrackingURL: quoteEngineHoldTrackingURL,
	}, true
}

func (h engineHold) message() string {
	return fmt.Sprintf("%s: %s (tracking %s)", h.Reason, h.Text, h.TrackingURL)
}

func engineHeldStrategyIDs(strategies []StrategyConfig) map[string]engineHold {
	out := map[string]engineHold{}
	for _, sc := range strategies {
		if h, ok := engineHoldFor(sc); ok {
			out[sc.ID] = h
		}
	}
	return out
}

func sortedEngineHoldIDs(holds map[string]engineHold) []string {
	ids := make([]string, 0, len(holds))
	for id := range holds {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func researchPromotionRefusal(sc StrategyConfig) string {
	refs := researchReferences(sc)
	labels := make([]string, 0, len(refs))
	for _, ref := range refs {
		labels = append(labels, ref.label())
	}
	return fmt.Sprintf("strategy %q references research-only %s — research strategies run only in explicit paper mode; paper-to-live is refused until a separate reviewed promotion", sc.ID, strings.Join(labels, ", "))
}

func diskStrategyPromotionRefusal(root map[string]json.RawMessage, id string) error {
	data, err := json.Marshal(root)
	if err != nil {
		return err
	}
	cfg, err := loadConfigDataOpts("", data, true, true, configLoadOptions{skipRegimeSideArtifact: true})
	if err != nil {
		return fmt.Errorf("paper-to-live refused: the current config on disk does not validate, so its effective references cannot be checked: %v", err)
	}
	for _, sc := range cfg.Strategies {
		if sc.ID != id {
			continue
		}
		if hasResearchReferences(sc) {
			return fmt.Errorf("%s; nothing changed", researchPromotionRefusal(sc))
		}
		return nil
	}
	return fmt.Errorf("strategy %q not found in the current config on disk", id)
}

func engineHoldReloadErrors(cfg, next *Config) []string {
	if cfg == nil || next == nil {
		return nil
	}
	cur := engineHeldStrategyIDs(cfg.Strategies)
	nxt := engineHeldStrategyIDs(next.Strategies)
	ids := map[string]bool{}
	for id := range cur {
		ids[id] = true
	}
	for id := range nxt {
		ids[id] = true
	}
	sorted := make([]string, 0, len(ids))
	for id := range ids {
		sorted = append(sorted, id)
	}
	sort.Strings(sorted)
	var errs []string
	for _, id := range sorted {
		_, was := cur[id]
		_, is := nxt[id]
		if was != is {
			errs = append(errs, fmt.Sprintf("strategy[%s] %s engine hold changed (%t -> %t; restart required so the saved-state check runs before the hold applies)", id, quoteEngineHoldReason, was, is))
		}
	}
	return errs
}

func splitEngineHeldDue(due []StrategyConfig) (supported, held []StrategyConfig) {
	supported = due[:0:0]
	for _, sc := range due {
		if _, ok := engineHoldFor(sc); ok {
			held = append(held, sc)
			continue
		}
		supported = append(supported, sc)
	}
	return supported, held
}

func reportEngineHeldDue(held []StrategyConfig, websocketFeed bool, marks map[string]feedEvaluationMark,
	lastRun map[string]time.Time, lastEvaluated map[string]feedEvaluationMark, now time.Time) {
	for _, sc := range held {
		h, _ := engineHoldFor(sc)
		fmt.Printf("[hold] %s: %s\n", sc.ID, h.message())
		globalRunEvidence.markEngineHeld(h, now)
		markStrategyHeldDue(sc, websocketFeed, marks, lastRun, lastEvaluated, now)
	}
}

func markStrategyHeldDue(sc StrategyConfig, websocketFeed bool, marks map[string]feedEvaluationMark,
	lastRun map[string]time.Time, lastEvaluated map[string]feedEvaluationMark, now time.Time) {
	if websocketFeed && feedScopedStrategy(sc) {
		if mark, ok := marks[sc.ID]; ok {
			lastEvaluated[sc.ID] = mark
			delete(marks, sc.ID)
			return
		}
	}
	lastRun[sc.ID] = now
}

func engineHoldStartupLines(strategies []StrategyConfig) []string {
	holds := engineHeldStrategyIDs(strategies)
	var lines []string
	for _, id := range sortedEngineHoldIDs(holds) {
		lines = append(lines, fmt.Sprintf("[hold] %s: %s", id, holds[id].message()))
	}
	return lines
}

type engineHoldView struct {
	Reason      string `json:"reason"`
	Text        string `json:"text"`
	TrackingURL string `json:"tracking_url"`
	LastAt      string `json:"last_at,omitempty"`
}

func engineHoldViewFor(sc StrategyConfig) *engineHoldView {
	h, ok := engineHoldFor(sc)
	if !ok {
		return nil
	}
	v := &engineHoldView{Reason: h.Reason, Text: h.Text, TrackingURL: h.TrackingURL}
	if rec, ok := globalRunEvidence.engineHoldFor(sc.ID); ok {
		v.LastAt = rec.at.Format(time.RFC3339Nano)
	}
	return v
}

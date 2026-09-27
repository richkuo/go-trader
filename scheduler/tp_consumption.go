package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	tpConsumptionDiscovered = "discovered"
	tpConsumptionBooked     = "booked"
	tpConsumptionDone       = "done"
	tpConsumptionDeferred   = "deferred"

	tpDeferLabelConflict   = "label_conflict"
	tpDeferUnattributed    = "unattributed"
	tpDeferTierOutside     = "tier_outside_ladder"
	tpDeferLabelUnresolved = "label_unresolved"
	tpDeferRuleUnresolved  = "rule_unresolved"
)

// TPConsumption is position-owned evidence of one take-profit tier on a unified
// per-regime close. Discovered entries are not consumable. A booked entry is
// pending work under the label recorded at booking. Done entries have had their
// rule applied or explicitly completed. Deferred entries stay visible and never
// run a rule or hold regime advancement.
type TPConsumption struct {
	Label       string    `json:"label,omitempty"`
	Tier        int       `json:"tier"`
	OID         int64     `json:"oid,omitempty"`
	Stage       string    `json:"stage"`
	DeferReason string    `json:"defer_reason,omitempty"`
	BookedQty   float64   `json:"booked_qty,omitempty"`
	Count       int       `json:"count,omitempty"`
	UpdatedAt   time.Time `json:"updated_at,omitempty"`
}

func cloneTPConsumptions(in []TPConsumption) []TPConsumption {
	if in == nil {
		return nil
	}
	out := make([]TPConsumption, len(in))
	copy(out, in)
	return out
}

func marshalTPConsumptionsJSON(recs []TPConsumption) string {
	if len(recs) == 0 {
		return ""
	}
	b, err := json.Marshal(recs)
	if err != nil {
		return ""
	}
	return string(b)
}

func parseTPConsumptionsJSON(raw string) []TPConsumption {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var recs []TPConsumption
	if err := json.Unmarshal([]byte(raw), &recs); err != nil {
		return nil
	}
	return recs
}

func unifiedCloseConfiguredLabels(sc StrategyConfig) []string {
	params := unifiedCloseRefParams(sc)
	if params == nil {
		return nil
	}
	trend, ok := params[regimeClassifierKey].(map[string]interface{})
	if !ok {
		return nil
	}
	labels := make([]string, 0, len(trend))
	for label, raw := range trend {
		if _, isMap := raw.(map[string]interface{}); isMap {
			labels = append(labels, label)
		}
	}
	sort.Strings(labels)
	return labels
}

func unifiedCloseLabelResolves(sc StrategyConfig, label string) bool {
	params := unifiedCloseRefParams(sc)
	if params == nil {
		return false
	}
	_, _, ok := unifiedRegimeScalarParams(params, label)
	return ok
}

func strategyHasPostTPStopRules(sc StrategyConfig) bool {
	if strategyUsesUnifiedRegimeClose(sc) {
		for _, label := range unifiedCloseConfiguredLabels(sc) {
			rules, _ := parseStrategyTPSLAfterRulesForRegime(sc, nil, label)
			if rules.HasAny() {
				return true
			}
		}
		return false
	}
	rules, _ := parseStrategyTPSLAfterRules(sc)
	return rules.HasAny()
}

func tpConsumptionHoldsRegime(pos *Position) bool {
	if pos == nil {
		return false
	}
	for _, rec := range pos.TPConsumptions {
		if rec.Stage == tpConsumptionBooked {
			return true
		}
	}
	return false
}

func tpConsumptionStatusCounts(pos *Position) (pending, deferred int) {
	if pos == nil {
		return 0, 0
	}
	for _, rec := range pos.TPConsumptions {
		switch rec.Stage {
		case tpConsumptionBooked:
			pending++
		case tpConsumptionDeferred:
			deferred++
		}
	}
	return pending, deferred
}

func nextBookedConsumptionGroup(pos *Position) (label string, highestTier int, ok bool) {
	if pos == nil {
		return "", 0, false
	}
	bestIdx := -1
	var bestTime time.Time
	for i, rec := range pos.TPConsumptions {
		if rec.Stage != tpConsumptionBooked {
			continue
		}
		if bestIdx < 0 || rec.UpdatedAt.Before(bestTime) || (rec.UpdatedAt.Equal(bestTime) && i < bestIdx) {
			bestIdx = i
			bestTime = rec.UpdatedAt
		}
	}
	if bestIdx < 0 {
		return "", 0, false
	}
	label = pos.TPConsumptions[bestIdx].Label
	highestTier = -1
	for _, rec := range pos.TPConsumptions {
		if rec.Stage == tpConsumptionBooked && rec.Label == label && rec.Tier > highestTier {
			highestTier = rec.Tier
		}
	}
	if highestTier < 0 {
		return label, -1, false
	}
	return label, highestTier, true
}

func completeBookedConsumptionGroup(pos *Position, label string, setMarker bool) {
	if pos == nil {
		return
	}
	now := time.Now().UTC()
	highest := -1
	for i := range pos.TPConsumptions {
		rec := &pos.TPConsumptions[i]
		if rec.Stage != tpConsumptionBooked || rec.Label != label {
			continue
		}
		rec.Stage = tpConsumptionDone
		rec.UpdatedAt = now
		if rec.Tier > highest {
			highest = rec.Tier
		}
	}
	if setMarker {
		pos.SLAfterMoved = true
	}
	if highest >= 0 && highest+1 > pos.SLAdjustedTiersProcessed {
		pos.SLAdjustedTiersProcessed = highest + 1
	}
}

func deferBookedConsumptionGroup(pos *Position, strategyID, symbol, label, reason string) {
	if pos == nil {
		return
	}
	now := time.Now().UTC()
	changed := false
	for i := range pos.TPConsumptions {
		rec := &pos.TPConsumptions[i]
		if rec.Stage != tpConsumptionBooked || rec.Label != label {
			continue
		}
		rec.Stage = tpConsumptionDeferred
		rec.DeferReason = reason
		rec.UpdatedAt = now
		changed = true
	}
	if changed {
		queueTPConsumptionDeferNotice(strategyID, symbol, tpConsumptionPositionKey(pos), reason)
	}
}

// unifiedCrossLabelDone reports that a profit rule under a different label
// already moved this stop. A later label's rule may tighten that stop and
// must not loosen it.
func unifiedCrossLabelDone(pos *Position, consumptionLabel string) bool {
	if pos == nil || !pos.SLAfterMoved {
		return false
	}
	for _, rec := range pos.TPConsumptions {
		if rec.Stage == tpConsumptionDone && rec.Tier >= 0 && rec.Label != consumptionLabel {
			return true
		}
	}
	return false
}

var (
	tpConsumptionDeferOnce  sync.Map
	tpConsumptionDeferMu    sync.Mutex
	tpConsumptionDeferQueue []string
	legacySLAfterNoted      sync.Map
)

func formatTPConsumptionDeferNotice(strategyID, symbol, reason string) string {
	return fmt.Sprintf("**HL POST-TP SL DEFERRED** [%s] %s: take-profit evidence (%s) does not resolve to a stop rule. The evidence stays on the position. No stop is moved.", strategyID, symbol, reason)
}

func tpConsumptionPositionKey(pos *Position) string {
	if pos == nil {
		return ""
	}
	if id := strings.TrimSpace(pos.TradePositionID); id != "" {
		return id
	}
	if !pos.OpenedAt.IsZero() {
		return pos.OpenedAt.UTC().Format(time.RFC3339Nano)
	}
	return ""
}

func queueTPConsumptionDeferNotice(strategyID, symbol, positionKey, reason string) {
	if strategyID == "" || symbol == "" || reason == "" {
		return
	}
	key := strategyID + "|" + symbol + "|" + positionKey + "|" + reason
	if _, loaded := tpConsumptionDeferOnce.LoadOrStore(key, true); loaded {
		return
	}
	msg := formatTPConsumptionDeferNotice(strategyID, symbol, reason)
	fmt.Printf("[WARN] %s\n", msg)
	tpConsumptionDeferMu.Lock()
	tpConsumptionDeferQueue = append(tpConsumptionDeferQueue, msg)
	tpConsumptionDeferMu.Unlock()
}

func takeTPConsumptionDeferNotices() []string {
	tpConsumptionDeferMu.Lock()
	out := tpConsumptionDeferQueue
	tpConsumptionDeferQueue = nil
	tpConsumptionDeferMu.Unlock()
	return out
}

func sendTPConsumptionDeferNotices(notifier *MultiNotifier) {
	for _, msg := range takeTPConsumptionDeferNotices() {
		if notifier != nil && notifier.HasBackends() {
			notifier.SendOwnerDM(msg)
		}
	}
}

func tierOutcomeUnknown(result *HyperliquidProtectionSyncResult, idx int) bool {
	return result != nil && idx >= 0 && idx < len(result.TPOutcomeUnknown) && result.TPOutcomeUnknown[idx]
}

func recordDiscoveredTPConsumptions(pos *Position, label string, planTPOIDs []int64, result *HyperliquidProtectionSyncResult) {
	if pos == nil || result == nil {
		return
	}
	now := time.Now().UTC()
	for idx, filled := range result.TPFilledExternally {
		if !filled || tierOutcomeUnknown(result, idx) {
			continue
		}
		oid := int64(0)
		if idx >= 0 && idx < len(planTPOIDs) {
			oid = planTPOIDs[idx]
		}
		upsertDiscoveredConsumption(pos, label, idx, oid, now)
	}
	for idx, filled := range result.TPFilledImmediately {
		if !filled || tierOutcomeUnknown(result, idx) {
			continue
		}
		upsertDiscoveredConsumption(pos, label, idx, 0, now)
	}
	for _, oid := range result.TPCancelFilledOIDs {
		if oid <= 0 {
			continue
		}
		idx := -1
		for i, existing := range pos.TPOIDs {
			if existing == oid {
				idx = i
				break
			}
		}
		if idx < 0 || tierOutcomeUnknown(result, idx) {
			continue
		}
		upsertDiscoveredConsumption(pos, label, idx, oid, now)
	}
}

func upsertDiscoveredConsumption(pos *Position, label string, tier int, oid int64, now time.Time) {
	if pos == nil || tier < 0 {
		return
	}
	for i := range pos.TPConsumptions {
		rec := &pos.TPConsumptions[i]
		if rec.Label != label || rec.Tier != tier {
			continue
		}
		if rec.Stage == tpConsumptionDiscovered && rec.OID == 0 && oid > 0 {
			rec.OID = oid
			rec.UpdatedAt = now
		}
		return
	}
	pos.TPConsumptions = append(pos.TPConsumptions, TPConsumption{
		Label:     label,
		Tier:      tier,
		OID:       oid,
		Stage:     tpConsumptionDiscovered,
		UpdatedAt: now,
	})
}

func recordTPConsumptionAtBooking(sc StrategyConfig, pos *Position, bookedQty float64, lookupOID int64, candidateTier int) {
	if pos == nil || bookedQty <= 0 || !strategyUsesUnifiedRegimeClose(sc) {
		return
	}
	label := protectionATRRegimeLabel(pos, sc)
	now := time.Now().UTC()
	tier, discLabel, fromDiscovery := attributeConsumptionTier(pos, lookupOID, candidateTier)
	if !unifiedCloseLabelResolves(sc, label) {
		promoteDiscoveredConsumptions(pos, sc.ID, pos.Symbol, label, now, tpDeferLabelUnresolved)
		addUnattributedDeferred(pos, tpDeferLabelUnresolved, bookedQty, now)
		queueTPConsumptionDeferNotice(sc.ID, pos.Symbol, tpConsumptionPositionKey(pos), tpDeferLabelUnresolved)
		return
	}
	promoteDiscoveredConsumptions(pos, sc.ID, pos.Symbol, label, now, "")
	if fromDiscovery && discLabel != label {
		for i := range pos.TPConsumptions {
			rec := &pos.TPConsumptions[i]
			if rec.Label == discLabel && rec.Tier == tier && rec.DeferReason == tpDeferLabelConflict {
				rec.BookedQty += bookedQty
				if lookupOID > 0 && rec.OID == 0 {
					rec.OID = lookupOID
				}
				break
			}
		}
		return
	}
	if tier < 0 {
		addUnattributedDeferred(pos, tpDeferUnattributed, bookedQty, now)
		queueTPConsumptionDeferNotice(sc.ID, pos.Symbol, tpConsumptionPositionKey(pos), tpDeferUnattributed)
		return
	}
	ladder := strategyTPTiersForRegime(sc, label)
	if tier >= len(ladder) {
		upsertDeferredConsumption(pos, label, tier, lookupOID, tpDeferTierOutside, bookedQty, now)
		queueTPConsumptionDeferNotice(sc.ID, pos.Symbol, tpConsumptionPositionKey(pos), tpDeferTierOutside)
		return
	}
	upsertBookedConsumption(pos, label, tier, lookupOID, bookedQty, now)
}

func attributeConsumptionTier(pos *Position, lookupOID int64, candidateTier int) (tier int, discoveredLabel string, fromDiscovery bool) {
	if pos == nil {
		return -1, "", false
	}
	if lookupOID > 0 {
		for i, oid := range pos.TPOIDs {
			if oid == lookupOID {
				return i, "", false
			}
		}
		for _, rec := range pos.TPConsumptions {
			if rec.Stage == tpConsumptionDiscovered && rec.OID == lookupOID && rec.Tier >= 0 {
				return rec.Tier, rec.Label, true
			}
		}
	}
	if candidateTier >= 0 {
		for _, rec := range pos.TPConsumptions {
			if rec.Stage == tpConsumptionDiscovered && rec.OID == 0 && rec.Tier == candidateTier {
				return candidateTier, rec.Label, true
			}
		}
	}
	best := -1
	bestLabel := ""
	for _, rec := range pos.TPConsumptions {
		if rec.Stage == tpConsumptionDiscovered && rec.OID == 0 && rec.Tier >= 0 && (best < 0 || rec.Tier < best) {
			best = rec.Tier
			bestLabel = rec.Label
		}
	}
	if best < 0 {
		return -1, "", false
	}
	return best, bestLabel, true
}

func promoteDiscoveredConsumptions(pos *Position, strategyID, symbol, bookingLabel string, now time.Time, forceReason string) {
	if pos == nil {
		return
	}
	conflict := false
	for i := range pos.TPConsumptions {
		rec := &pos.TPConsumptions[i]
		if rec.Stage != tpConsumptionDiscovered {
			continue
		}
		switch {
		case forceReason != "":
			rec.Stage = tpConsumptionDeferred
			rec.DeferReason = forceReason
			rec.UpdatedAt = now
		case rec.Label != bookingLabel:
			rec.Stage = tpConsumptionDeferred
			rec.DeferReason = tpDeferLabelConflict
			rec.UpdatedAt = now
			conflict = true
		default:
			rec.Stage = tpConsumptionBooked
			rec.UpdatedAt = now
		}
	}
	if conflict {
		queueTPConsumptionDeferNotice(strategyID, symbol, tpConsumptionPositionKey(pos), tpDeferLabelConflict)
	}
}

func upsertBookedConsumption(pos *Position, label string, tier int, oid int64, qty float64, now time.Time) {
	if pos == nil || tier < 0 {
		return
	}
	for i := range pos.TPConsumptions {
		rec := &pos.TPConsumptions[i]
		if rec.Label != label || rec.Tier != tier {
			continue
		}
		rec.BookedQty += qty
		if oid > 0 && rec.OID == 0 {
			rec.OID = oid
		}
		if rec.Stage == tpConsumptionDiscovered || rec.Stage == "" {
			rec.Stage = tpConsumptionBooked
			rec.UpdatedAt = now
		}
		return
	}
	pos.TPConsumptions = append(pos.TPConsumptions, TPConsumption{
		Label:     label,
		Tier:      tier,
		OID:       oid,
		Stage:     tpConsumptionBooked,
		BookedQty: qty,
		UpdatedAt: now,
	})
}

func upsertDeferredConsumption(pos *Position, label string, tier int, oid int64, reason string, qty float64, now time.Time) {
	if pos == nil {
		return
	}
	for i := range pos.TPConsumptions {
		rec := &pos.TPConsumptions[i]
		if rec.Label != label || rec.Tier != tier {
			continue
		}
		rec.Stage = tpConsumptionDeferred
		rec.DeferReason = reason
		rec.BookedQty += qty
		if oid > 0 && rec.OID == 0 {
			rec.OID = oid
		}
		rec.UpdatedAt = now
		return
	}
	pos.TPConsumptions = append(pos.TPConsumptions, TPConsumption{
		Label:       label,
		Tier:        tier,
		OID:         oid,
		Stage:       tpConsumptionDeferred,
		DeferReason: reason,
		BookedQty:   qty,
		UpdatedAt:   now,
	})
}

func addUnattributedDeferred(pos *Position, reason string, qty float64, now time.Time) {
	if pos == nil {
		return
	}
	for i := range pos.TPConsumptions {
		rec := &pos.TPConsumptions[i]
		if rec.Tier >= 0 || rec.Stage != tpConsumptionDeferred {
			continue
		}
		if rec.Count < 1 {
			rec.Count = 1
		}
		rec.Count++
		rec.BookedQty += qty
		if rec.DeferReason == "" {
			rec.DeferReason = reason
		}
		rec.UpdatedAt = now
		return
	}
	pos.TPConsumptions = append(pos.TPConsumptions, TPConsumption{
		Tier:        -1,
		Stage:       tpConsumptionDeferred,
		DeferReason: reason,
		BookedQty:   qty,
		Count:       1,
		UpdatedAt:   now,
	})
}

func recordPaperUnifiedTPConsumption(sc StrategyConfig, pos *Position, preQty, preInit float64) {
	if pos == nil || pos.Quantity <= 0 || !strategyUsesUnifiedRegimeClose(sc) {
		return
	}
	initQty := preInit
	if initQty <= 0 {
		initQty = pos.InitialQuantity
	}
	if initQty <= 0 {
		return
	}
	postInit := pos.InitialQuantity
	if postInit <= 0 {
		postInit = initQty
	}
	preRatio := 1 - preQty/initQty
	if preRatio < 0 {
		preRatio = 0
	}
	postRatio := 1 - pos.Quantity/postInit
	label := protectionATRRegimeLabel(pos, sc)
	now := time.Now().UTC()
	delta := preQty - pos.Quantity
	if delta < 0 {
		delta = 0
	}
	if !unifiedCloseLabelResolves(sc, label) {
		addUnattributedDeferred(pos, tpDeferLabelUnresolved, delta, now)
		queueTPConsumptionDeferNotice(sc.ID, pos.Symbol, tpConsumptionPositionKey(pos), tpDeferLabelUnresolved)
		return
	}
	thresholds := paperSLAfterTierThresholds(sc, label)
	for i, th := range thresholds {
		if th > preRatio+1e-9 && postRatio+1e-9 >= th {
			upsertBookedConsumption(pos, label, i, 0, delta, now)
		}
	}
}

func paperSignalCloseOwnsUnifiedTier(sc StrategyConfig, result *HyperliquidResult) bool {
	if result == nil || hyperliquidIsLive(sc.Args) || !strategyUsesUnifiedRegimeClose(sc) {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(result.CloseStrategy)) {
	case "tiered_tp_atr_regime", "tiered_tp_atr_live_regime", dynamicCloseStrategyName:
		return true
	}
	return result.CloseOwner == hlCloseOwnerOnChainTP
}

func replayCloseReasonIsTakeProfit(reason string) bool {
	reason = strings.TrimSpace(reason)
	if reason == "hl_sync_external_partial" {
		return true
	}
	if !strings.HasPrefix(reason, "hl_sync_tp") || !strings.HasSuffix(reason, "_fill") {
		return false
	}
	mid := strings.TrimSuffix(strings.TrimPrefix(reason, "hl_sync_tp"), "_fill")
	if mid == "" {
		return false
	}
	for _, c := range mid {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func formatLegacySLAfterNotice(strategyID, symbol string) string {
	return fmt.Sprintf("**HL POST-TP SL** [%s] %s: earlier take-profit fills do not move the stop. A manual stop edit is the remedy.", strategyID, symbol)
}

func positionNeedsLegacySLAfterNotice(sc StrategyConfig, pos *Position) bool {
	if pos == nil || !strategyUsesUnifiedRegimeClose(sc) || !strategyHasPostTPStopRules(sc) {
		return false
	}
	if len(pos.TPConsumptions) > 0 || pos.InitialQuantity <= 0 {
		return false
	}
	if pos.Quantity+1e-9 >= pos.InitialQuantity {
		return false
	}
	_, cleared := findHighestClearedTier(pos.TPOIDs, pos.TPArmedTiers, 0)
	return cleared
}

func reportLegacyUnifiedSLAfterGaps(cfg *Config, state *AppState, mu *sync.RWMutex, notifier *MultiNotifier, loggerFor func(string) *StrategyLogger) {
	if cfg == nil || state == nil || mu == nil {
		return
	}
	type hit struct {
		id     string
		symbol string
	}
	mu.RLock()
	var hits []hit
	for _, sc := range cfg.Strategies {
		ss := state.Strategies[sc.ID]
		if ss == nil {
			continue
		}
		syms := make([]string, 0, len(ss.Positions))
		for sym, pos := range ss.Positions {
			if positionNeedsLegacySLAfterNotice(sc, pos) {
				syms = append(syms, sym)
			}
		}
		sort.Strings(syms)
		for _, sym := range syms {
			hits = append(hits, hit{id: sc.ID, symbol: sym})
		}
	}
	mu.RUnlock()
	for _, h := range hits {
		key := h.id + "|" + h.symbol
		if _, loaded := legacySLAfterNoted.LoadOrStore(key, true); loaded {
			continue
		}
		msg := formatLegacySLAfterNotice(h.id, h.symbol)
		logged := false
		if loggerFor != nil {
			if lg := loggerFor(h.id); lg != nil && lg.writer != nil {
				lg.Warn("%s", msg)
				logged = true
			}
		}
		if !logged {
			fmt.Printf("[WARN] %s\n", msg)
		}
		if notifier != nil && notifier.HasBackends() {
			notifier.SendOwnerDM(msg)
		}
	}
}

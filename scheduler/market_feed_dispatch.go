package main

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

const marketStdinFlag = "--market-stdin"

type marketFeedContext struct {
	Enabled      bool
	Requirements feedRequirements
	Snapshot     *marketSnapshot
	Interval     int
}

func (c *marketFeedContext) active() bool {
	return c != nil && c.Enabled && c.Snapshot != nil
}

func (c *marketFeedContext) entryFor(id string) (feedStrategyRequirement, bool) {
	if c == nil {
		return feedStrategyRequirement{}, false
	}
	entry, ok := c.Requirements.Strategies[id]
	return entry, ok
}

func (c *marketFeedContext) covers(sc StrategyConfig) bool {
	if !c.active() {
		return false
	}
	_, ok := c.entryFor(sc.ID)
	return ok
}

func (c *marketFeedContext) frameSpecs(sc StrategyConfig) ([]marketPayloadFrameSpec, string, bool) {
	entry, ok := c.entryFor(sc.ID)
	if !ok {
		return nil, "", false
	}
	specs := []marketPayloadFrameSpec{{Key: entry.Signal, Required: entry.SignalLookback}}
	if entry.HasHTF {
		specs = append(specs, marketPayloadFrameSpec{Key: entry.HTF, Required: hlFeedHTFLookback})
	}
	if entry.HasRegime {
		specs = append(specs, marketPayloadFrameSpec{Key: entry.Regime, Required: entry.RegimeLookback})
	}
	return specs, entry.Coin, true
}

func (c *marketFeedContext) singleCheckPayload(sc StrategyConfig) ([]byte, error) {
	specs, coin, ok := c.frameSpecs(sc)
	if !ok {
		return nil, fmt.Errorf("strategy %s is outside the market feed contract", sc.ID)
	}
	payload, err := marketPayloadFor(c.Snapshot, specs, []string{coin})
	if err != nil {
		return nil, err
	}
	return marketStdinJSON(payload)
}

func (c *marketFeedContext) batchPayload(key hlBatchKey, members []StrategyConfig) (*marketPayload, error) {
	seen := make(map[marketFeedKey]int)
	var specs []marketPayloadFrameSpec
	raise := func(fk marketFeedKey, required int) {
		if existing, ok := seen[fk]; ok {
			if required > existing {
				seen[fk] = required
			}
			return
		}
		seen[fk] = required
		specs = append(specs, marketPayloadFrameSpec{Key: fk})
	}
	signalKey := feedKeyFor(key.Symbol, key.Timeframe)
	raise(signalKey, key.OhlcvLimit)
	for _, sc := range members {
		entry, ok := c.entryFor(sc.ID)
		if !ok {
			return nil, fmt.Errorf("strategy %s is outside the market feed contract", sc.ID)
		}
		if entry.Signal != signalKey {
			return nil, fmt.Errorf("strategy %s signal key %s does not match the batch key %s", sc.ID, entry.Signal, signalKey)
		}
		if entry.HasHTF {
			raise(entry.HTF, hlFeedHTFLookback)
		}
		if entry.HasRegime {
			raise(entry.Regime, entry.RegimeLookback)
		}
		if held, why := c.Snapshot.fundingHold(entry.Coin, entry.FundingScalar, entry.FundingRecords); held {
			return nil, fmt.Errorf("strategy %s: %s", sc.ID, why)
		}
	}
	for i := range specs {
		specs[i].Required = seen[specs[i].Key]
	}
	return marketPayloadFor(c.Snapshot, specs, []string{key.Symbol})
}

func (c *marketFeedContext) regimeBundlePayload(req regimeBundleRequest) ([]byte, error) {
	key := feedKeyFor(req.Key.Symbol, req.Key.Timeframe)
	payload, err := marketPayloadFor(c.Snapshot, []marketPayloadFrameSpec{{Key: key, Required: req.OhlcvLimit}}, nil)
	if err != nil {
		return nil, err
	}
	return marketStdinJSON(payload)
}

func (c *marketFeedContext) ownsRegimeBundle(req regimeBundleRequest) bool {
	if !c.active() {
		return false
	}
	if req.Key.Platform != "hyperliquid" {
		return false
	}
	_, ok := c.Requirements.Keys[feedKeyFor(req.Key.Symbol, req.Key.Timeframe)]
	return ok
}

type feedHold struct {
	Held   bool
	Key    marketFeedKey
	Reason string
}

func (c *marketFeedContext) feedHoldsSignal(sc StrategyConfig) (bool, string) {
	hold := c.holdFor(sc)
	return hold.Held, hold.Reason
}

func (c *marketFeedContext) holdFor(sc StrategyConfig) feedHold {
	if !c.active() {
		return feedHold{}
	}
	entry, ok := c.entryFor(sc.ID)
	if !ok {
		return feedHold{}
	}
	if c.Snapshot.keyFailed(entry.Signal) {
		return feedHold{Held: true, Key: entry.Signal, Reason: fmt.Sprintf("no ready candle frame for %s", entry.Signal.PayloadID())}
	}
	if entry.HasHTF && c.Snapshot.keyFailed(entry.HTF) {
		return feedHold{Held: true, Key: entry.HTF, Reason: fmt.Sprintf("no ready higher-timeframe frame for %s", entry.HTF.PayloadID())}
	}
	if held, why := c.Snapshot.fundingHold(entry.Coin, entry.FundingScalar, entry.FundingRecords); held {
		return feedHold{Held: true, Key: entry.Signal, Reason: why}
	}
	return feedHold{}
}

func (c *marketFeedContext) snapshotID() string {
	if c == nil || c.Snapshot == nil {
		return ""
	}
	return c.Snapshot.EvaluationID
}

func (c *marketFeedContext) clearDegradedFor(notifier *MultiNotifier, id string) {
	entry, ok := c.entryFor(id)
	if !ok {
		return
	}
	clearMarketFeedDegraded(notifier, entry.Signal)
	if entry.HasHTF {
		clearMarketFeedDegraded(notifier, entry.HTF)
	}
}

func (c *marketFeedContext) decisionTooOld(now time.Time, intervalSeconds int) (bool, string) {
	if !c.active() {
		return false, ""
	}
	if intervalSeconds <= 0 {
		intervalSeconds = c.Interval
	}
	limit := feedDecisionAgeLimit(intervalSeconds)
	age := c.Snapshot.age(now)
	if age > limit {
		return true, fmt.Sprintf("sealed snapshot %s is %s old, over the %s decision-age limit",
			c.Snapshot.EvaluationID, age.Round(time.Second), limit)
	}
	return false, ""
}

func degradedFeedPrice(c *marketFeedContext, prices map[string]float64, symbol string) float64 {
	if c.active() {
		if px, ok := c.Snapshot.midFor(symbol); ok {
			return px
		}
	}
	if px, ok := prices[symbol]; ok && px > 0 {
		return px
	}
	return 0
}

func cycleEvaluationID(marks map[string]feedEvaluationMark, cycle int) string {
	seen := make(map[string]bool, len(marks))
	ids := make([]string, 0, len(marks))
	for _, mark := range marks {
		if seen[mark.ID] {
			continue
		}
		seen[mark.ID] = true
		ids = append(ids, mark.ID)
	}
	if len(ids) == 0 {
		return fmt.Sprintf("cycle/%d", cycle)
	}
	sort.Strings(ids)
	return strings.Join(ids, "+")
}

func degradedHyperliquidResult(sc StrategyConfig, symbol, mode, reason string, price float64) *HyperliquidResult {
	return &HyperliquidResult{
		Strategy:   strategyNameFromArgs(sc.Args),
		Symbol:     symbol,
		Timeframe:  hyperliquidTimeframe(sc.Args),
		Signal:     0,
		Price:      price,
		Indicators: map[string]interface{}{},
		Mode:       mode,
		Platform:   "hyperliquid",
		Timestamp:  time.Now().UTC().Format(time.RFC3339),
		Degraded:   reason,
	}
}

var marketFeedDegradedTracker = &ScriptFailureTracker{}

var marketFeedDegradedCycle = struct {
	sync.Mutex
	seen map[string]string
}{seen: map[string]string{}}

func marketFeedDegradedCountsThisCycle(key marketFeedKey, snapshotID string) bool {
	marketFeedDegradedCycle.Lock()
	defer marketFeedDegradedCycle.Unlock()
	id := key.String()
	if snapshotID != "" && marketFeedDegradedCycle.seen[id] == snapshotID {
		return false
	}
	marketFeedDegradedCycle.seen[id] = snapshotID
	return true
}

func marketFeedDegradedForget(key marketFeedKey) {
	marketFeedDegradedCycle.Lock()
	defer marketFeedDegradedCycle.Unlock()
	delete(marketFeedDegradedCycle.seen, key.String())
}

func marketFeedAlertConfig(key marketFeedKey) StrategyConfig {
	return StrategyConfig{
		ID:       "market-feed[" + key.PayloadID() + "]",
		Platform: "hyperliquid",
		Script:   hyperliquidCheckScript,
	}
}

func notifyMarketFeedDegraded(notifier *MultiNotifier, key marketFeedKey, snapshotID, detail string) {
	if !marketFeedDegradedCountsThisCycle(key, snapshotID) {
		return
	}
	now := time.Now().UTC()
	shouldNotify, count := marketFeedDegradedTracker.Record(key.String(), detail, now)
	if !shouldNotify || notifier == nil || !notifier.HasBackends() {
		return
	}
	msg := fmt.Sprintf("**MARKET FEED DEGRADED** [%s] %s (%d consecutive cycles): entries are held; closes, stops, ratchet and protection continue on verified inputs.",
		key.PayloadID(), detail, count)
	notifier.SendToAllChannels(msg)
	notifier.SendOwnerDM(msg)
}

func clearMarketFeedDegraded(notifier *MultiNotifier, key marketFeedKey) {
	marketFeedDegradedForget(key)
	recovered, _ := marketFeedDegradedTracker.Clear(key.String())
	if !recovered || notifier == nil || !notifier.HasBackends() {
		return
	}
	msg := fmt.Sprintf("**MARKET FEED RECOVERED** [%s]: the sealed snapshot carries a ready frame again.", key.PayloadID())
	notifier.SendToAllChannels(msg)
	notifier.SendOwnerDM(msg)
}

func formatFeedAlerts(alerts []feedAlert) []string {
	out := make([]string, 0, len(alerts))
	for _, a := range alerts {
		out = append(out, fmt.Sprintf("[feed] %s %s: %s", a.Key.PayloadID(), a.Kind, strings.TrimSpace(a.Detail)))
	}
	return out
}

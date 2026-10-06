package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	hlFeedHTFDefault  = "4h"
	hlFeedHTFLookback = 60
)

var hlFeedHTFMap = map[string]string{
	"1m":  "15m",
	"5m":  "1h",
	"15m": "1h",
	"30m": "4h",
	"1h":  "4h",
	"4h":  "1d",
	"1d":  "1w",
	"1w":  "1M",
}

func hlFeedHTFTimeframe(timeframe string) string {
	if out, ok := hlFeedHTFMap[timeframe]; ok {
		return out
	}
	return hlFeedHTFDefault
}

const (
	fundingScalarStrategyName  = "delta_neutral_funding"
	fundingRecordsStrategyName = "funding_skew"
)

func feedScopedStrategy(sc StrategyConfig) bool {
	if sc.Platform != "hyperliquid" {
		return false
	}
	if sc.Type != "perps" && sc.Type != "manual" {
		return false
	}
	if strings.TrimSpace(sc.Script) != hyperliquidCheckScript {
		return false
	}
	symbol, timeframe := strategyArgSymbolTimeframe(sc.Args)
	return symbol != "" && timeframe != ""
}

func feedSignalLookback(rc *RegimeConfig) int {
	limit := hlBatchPythonDefaultOhlcvLimit
	if rc != nil && rc.Enabled {
		if regimeLimit := regimeRequiredOhlcvLimit(rc); regimeLimit > limit {
			limit = regimeLimit
		}
	}
	return limit
}

func feedStrategyUsesHTF(sc StrategyConfig) bool {
	if !sc.HTFFilter {
		return false
	}
	name := effectiveOpenStrategy(sc)
	return name != fundingScalarStrategyName && name != fundingRecordsStrategyName
}

type feedStrategyRequirement struct {
	ID             string
	Signal         marketFeedKey
	SignalLookback int
	HTF            marketFeedKey
	HasHTF         bool
	HTFLookback    int
	Regime         marketFeedKey
	HasRegime      bool
	RegimeLookback int
	Coin           string
	FundingScalar  bool
	FundingRecords bool
	OpenInterest   bool
	ObsWindowMs    int64
	ClosedBar      bool
}

func (e feedStrategyRequirement) observationNeeds() map[feedObservationKey]int64 {
	if !e.OpenInterest {
		return nil
	}
	return map[feedObservationKey]int64{openInterestKeyFor(e.Coin): e.ObsWindowMs}
}

type feedRequirements struct {
	Keys         map[marketFeedKey]int
	Order        []marketFeedKey
	MidCoins     []string
	Funding      map[string]feedFundingNeed
	Observations map[feedObservationKey]int64
	Strategies   map[string]feedStrategyRequirement
	KeyCadences  map[marketFeedKey][]int
	SignalKeys   map[marketFeedKey]bool
}

func (r *feedRequirements) addObservation(key feedObservationKey, windowMs int64) {
	if r.Observations == nil {
		r.Observations = make(map[feedObservationKey]int64)
	}
	if existing, ok := r.Observations[key]; !ok || windowMs > existing {
		r.Observations[key] = windowMs
	}
}

func (r feedRequirements) observationKeys() []feedObservationKey {
	out := make([]feedObservationKey, 0, len(r.Observations))
	for k := range r.Observations {
		out = append(out, k)
	}
	sortFeedObservationKeys(out)
	return out
}

func (r feedRequirements) requires(key marketFeedKey) bool {
	_, ok := r.Keys[key]
	return ok
}

func (r *feedRequirements) addKey(key marketFeedKey, lookback int) {
	if r.Keys == nil {
		r.Keys = make(map[marketFeedKey]int)
	}
	if existing, ok := r.Keys[key]; !ok || lookback > existing {
		r.Keys[key] = lookback
	}
}

func (r *feedRequirements) finalize() {
	r.Order = make([]marketFeedKey, 0, len(r.Keys))
	for k := range r.Keys {
		r.Order = append(r.Order, k)
	}
	sortMarketFeedKeys(r.Order)
	sort.Strings(r.MidCoins)
}

func feedKeyFor(symbol, timeframe string) marketFeedKey {
	return marketFeedKey{
		Host:      hlMainnetURL,
		Namespace: feedNamespacePerps,
		Symbol:    symbol,
		Timeframe: timeframe,
	}
}

func deriveFeedRequirements(cfg *Config) (feedRequirements, error) {
	req := feedRequirements{
		Keys:         make(map[marketFeedKey]int),
		Funding:      make(map[string]feedFundingNeed),
		Observations: make(map[feedObservationKey]int64),
		Strategies:   make(map[string]feedStrategyRequirement),
	}
	if cfg == nil {
		req.finalize()
		return req, nil
	}
	var rc *RegimeConfig
	if cfg != nil {
		rc = cfg.Regime
	}
	var errs []string
	for _, sc := range cfg.Strategies {
		if !feedScopedStrategy(sc) {
			continue
		}
		symbol, timeframe := strategyArgSymbolTimeframe(sc.Args)
		if _, ok := hlCandleIntervalMs(timeframe); !ok {
			errs = append(errs, fmt.Sprintf("strategy[%s] timeframe %q is not a Hyperliquid candle interval (supported: %s)",
				sc.ID, timeframe, strings.Join(hlSupportedCandleIntervals(), ", ")))
			continue
		}
		closedBarExtra := 0
		if sc.ClosedBarDecisions {
			closedBarExtra = 1
		}
		entry := feedStrategyRequirement{
			ID:             sc.ID,
			Signal:         feedKeyFor(symbol, timeframe),
			SignalLookback: feedSignalLookback(rc) + closedBarExtra,
			Coin:           symbol,
			ClosedBar:      sc.ClosedBarDecisions,
		}
		req.addKey(entry.Signal, entry.SignalLookback)

		if feedStrategyUsesHTF(sc) {
			htfTimeframe := hlFeedHTFTimeframe(timeframe)
			if _, ok := hlCandleIntervalMs(htfTimeframe); !ok {
				errs = append(errs, fmt.Sprintf("strategy[%s] higher-timeframe filter resolves to %q, which is not a Hyperliquid candle interval",
					sc.ID, htfTimeframe))
			} else {
				entry.HTF = feedKeyFor(symbol, htfTimeframe)
				entry.HasHTF = true
				entry.HTFLookback = hlFeedHTFLookback + closedBarExtra
				req.addKey(entry.HTF, entry.HTFLookback)
			}
		}

		if bundleReq, ok := strategyRegimeBundleRequest(sc, rc); ok {
			regimeTimeframe := bundleReq.Key.Timeframe
			if _, supported := hlCandleIntervalMs(regimeTimeframe); !supported {
				errs = append(errs, fmt.Sprintf("strategy[%s] regime timeframe %q is not a Hyperliquid candle interval (supported: %s)",
					sc.ID, regimeTimeframe, strings.Join(hlSupportedCandleIntervals(), ", ")))
			} else {
				entry.Regime = feedKeyFor(bundleReq.Key.Symbol, regimeTimeframe)
				entry.HasRegime = true
				entry.RegimeLookback = bundleReq.OhlcvLimit + closedBarExtra
				req.addKey(entry.Regime, entry.RegimeLookback)
			}
		}

		slotName := strategyNameFromArgs(sc.Args)
		openName := effectiveOpenStrategy(sc)
		entry.FundingScalar = slotName == fundingScalarStrategyName
		entry.FundingRecords = openName == fundingRecordsStrategyName
		if entry.FundingScalar || entry.FundingRecords {
			need := req.Funding[symbol]
			need.Scalar = need.Scalar || entry.FundingScalar
			need.Records = need.Records || entry.FundingRecords
			req.Funding[symbol] = need
		}

		if feedStrategyUsesOpenInterest(sc) {
			intervalMs, _ := hlCandleIntervalMs(timeframe)
			window, werr := openInterestWindowMs(sc, intervalMs)
			if werr != nil {
				errs = append(errs, fmt.Sprintf("strategy[%s] %s: %v", sc.ID, openInterestBreakoutStrategyName, werr))
			} else {
				entry.OpenInterest = true
				entry.ObsWindowMs = window
				req.addObservation(openInterestKeyFor(symbol), window)
			}
		}

		req.Strategies[sc.ID] = entry
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		return feedRequirements{}, fmt.Errorf("market_feed=%s rejects this config:\n  %s", cfg.marketFeedMode(), strings.Join(errs, "\n  "))
	}
	hlCoins, _ := collectPerpsMarkSymbols(cfg.Strategies)
	req.MidCoins = hlCoins
	req.finalize()
	return req, nil
}

func validateMarketFeedConfig(cfg *Config) error {
	if cfg == nil {
		return nil
	}
	switch cfg.marketFeedMode() {
	case marketFeedREST:
		if errs := sharedMarketFeedConfigErrors(cfg); len(errs) > 0 {
			return fmt.Errorf("%s", strings.Join(errs, "; "))
		}
		if ids := openInterestStrategyIDs(cfg); len(ids) > 0 {
			return fmt.Errorf("market_feed=%q cannot supply open-interest observations for %s (strategies %s); use market_feed %q or %q",
				marketFeedREST, openInterestBreakoutStrategyName, strings.Join(ids, ", "), marketFeedWebsocket, marketFeedShared)
		}
		return nil
	case marketFeedWebsocket:
		if errs := sharedMarketFeedConfigErrors(cfg); len(errs) > 0 {
			return fmt.Errorf("%s", strings.Join(errs, "; "))
		}
		_, err := deriveFeedRequirements(cfg)
		return err
	case marketFeedShared:
		if errs := sharedMarketFeedConfigErrors(cfg); len(errs) > 0 {
			return fmt.Errorf("%s", strings.Join(errs, "; "))
		}
		_, err := deriveFeedRequirements(cfg)
		return err
	default:
		return fmt.Errorf("market_feed must be %q, %q or %q, got %q", marketFeedREST, marketFeedWebsocket, marketFeedShared, cfg.MarketFeed)
	}
}

func (o *marketFeedOwner) ApplyGeneration(ctx context.Context, req feedRequirements) <-chan struct{} {
	done := make(chan struct{})

	o.feedMu.Lock()
	for _, key := range req.Order {
		lookback := req.Keys[key]
		intervalMs, ok := hlCandleIntervalMs(key.Timeframe)
		if !ok {
			continue
		}
		if st := o.keys[key]; st != nil {
			st.raiseRequired(lookback)
			continue
		}
		o.keys[key] = newFeedKeyState(key, intervalMs, lookback)
	}
	o.midCoins = make(map[string]bool, len(req.MidCoins))
	for _, coin := range req.MidCoins {
		o.midCoins[coin] = true
	}
	o.fundingNeeds = make(map[string]feedFundingNeed, len(req.Funding))
	for coin, need := range req.Funding {
		o.fundingNeeds[coin] = need
	}
	o.applyObservationNeedsLocked(req.Observations)
	o.subVersion++
	o.applySeq++
	token := o.applySeq
	pending := make([]marketFeedKey, 0, len(req.Order))
	for _, key := range req.Order {
		st := o.keys[key]
		if st == nil {
			continue
		}
		if st.Status == feedStatusBootstrapping || len(st.Bars) < feedReadyFloor(st.Required) {
			pending = append(pending, key)
		}
	}
	o.feedMu.Unlock()

	go func() {
		defer close(done)
		var wg sync.WaitGroup
		for _, key := range pending {
			key := key
			wg.Add(1)
			go func() {
				defer wg.Done()
				bootstrapCtx, cancel := context.WithTimeout(ctx, feedBootstrapKeyBudget)
				defer cancel()
				if err := o.fetchAndMerge(bootstrapCtx, key, feedRestBootstrap); err != nil {
					o.logf("[feed] %s: bootstrap failed: %v", key, err)
					o.raiseAlert(key, "bootstrap_failed", err.Error())
					return
				}
				o.logf("[feed] %s: bootstrap complete", key)
			}()
		}
		wg.Wait()
		o.publishGeneration(req, token)
	}()

	return done
}

func (o *marketFeedOwner) publishGeneration(req feedRequirements, token uint64) bool {
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	if token != o.applySeq {
		o.logf("[feed] generation request %d finished after request %d superseded it; keeping the newer key set", token, o.applySeq)
		return false
	}
	for key := range o.keys {
		if !req.requires(key) {
			delete(o.keys, key)
		}
	}
	o.dropUnneededObservationsLocked(req.Observations)
	o.published = make(map[marketFeedKey]bool, len(req.Order))
	for _, key := range req.Order {
		o.published[key] = true
	}
	o.gen++
	return true
}

func (o *marketFeedOwner) fundingWork() map[string]feedFundingNeed {
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	out := make(map[string]feedFundingNeed, len(o.fundingNeeds))
	for coin, need := range o.fundingNeeds {
		out[coin] = need
	}
	return out
}

const feedFundingTTL = 5 * time.Minute

func (o *marketFeedOwner) EnsureFunding(ctx context.Context, earliestBarMs map[string]int64) {
	needs := o.fundingWork()
	if len(needs) == 0 {
		return
	}
	ctx = withFeedReason(ctx, string(feedRestFunding))
	coins := make([]string, 0, len(needs))
	for coin := range needs {
		coins = append(coins, coin)
	}
	sort.Strings(coins)
	now := o.now()
	for _, coin := range coins {
		need := needs[coin]
		o.feedMu.Lock()
		existing := o.funding[coin]
		fresh := existing != nil && existing.Err == "" && now.Sub(existing.FetchedAt) < feedFundingTTL &&
			(!need.Scalar || existing.HasScalar) && (!need.Records || existing.HasRecords)
		o.feedMu.Unlock()
		if fresh {
			continue
		}
		entry := &feedFunding{FetchedAt: now, Source: "rest"}
		if need.Scalar {
			current, err := fetchHyperliquidFundingRateFn(ctx, coin)
			if err != nil {
				entry.Err = err.Error()
			} else {
				avg, avgErr := hlFundingAverage7d(ctx, coin, now)
				if avgErr != nil {
					entry.Err = avgErr.Error()
				} else {
					entry.Current, entry.Avg7d, entry.HasScalar = current, avg, true
				}
			}
		}
		if need.Records && entry.Err == "" {
			startMs := earliestBarMs[coin]
			records, err := hlFundingRecordsSince(ctx, coin, startMs, now)
			if err != nil {
				entry.Err = err.Error()
			} else {
				entry.Records, entry.HasRecords = records, true
			}
		}
		if entry.Err != "" {
			o.logf("[feed] funding %s: %s", coin, entry.Err)
			o.raiseAlert(feedKeyFor(coin, ""), "funding_failed", entry.Err)
		}
		o.feedMu.Lock()
		o.funding[coin] = entry
		o.feedMu.Unlock()
	}
}

func openInterestStrategyIDs(cfg *Config) []string {
	var ids []string
	for _, sc := range cfg.Strategies {
		if feedScopedStrategy(sc) && feedStrategyUsesOpenInterest(sc) {
			ids = append(ids, sc.ID)
		}
	}
	sort.Strings(ids)
	return ids
}

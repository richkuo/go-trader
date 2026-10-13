package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	basketMemberStrategyToken = "basket_member"
	basketMemberFlag          = "--basket-member"

	basketRankerScript = "shared_scripts/run_basket_ranker.py"

	basketContractVersion = 1

	basketMaxRankerTimeoutSeconds = 600
	basketMaxLookbackBars         = 5000
	basketMinMembers              = 2

	basketMaxHoldingHorizonSeconds = 2_147_483_647
)

var basketRankerStrategies = map[string]bool{
	"basket_scripted_fixture":  true,
	"cross_sectional_momentum": true,
}

var basketRankerParamKeys = map[string][]string{
	"basket_scripted_fixture":  {"hold", "raw", "scores", "sleep_seconds", "weights"},
	"cross_sectional_momentum": {"quantile", "return_lookback_bars", "volatility_lookback_bars"},
}

var basketIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

type BasketListingFact struct {
	ListedAt   string `json:"listed_at"`
	DelistedAt string `json:"delisted_at,omitempty"`
	KnownAt    string `json:"known_at"`
}

type BasketEligibility struct {
	Source         string                       `json:"source"`
	Version        string                       `json:"version"`
	MinHistoryBars int                          `json:"min_history_bars"`
	Listings       map[string]BasketListingFact `json:"listings"`
}

type BasketConfig struct {
	ID                       string                 `json:"id"`
	Ranker                   string                 `json:"ranker"`
	Params                   map[string]interface{} `json:"params,omitempty"`
	Members                  []string               `json:"members"`
	RebalanceIntervalSeconds int                    `json:"rebalance_interval_seconds"`
	EvaluationWindowSeconds  int                    `json:"evaluation_window_seconds"`
	RankerTimeoutSeconds     int                    `json:"ranker_timeout_seconds"`
	LookbackBars             int                    `json:"lookback_bars"`
	GrossBudgetUSD           float64                `json:"gross_budget_usd"`
	MaxTransitionNetUSD      float64                `json:"max_transition_net_usd"`
	MaxCoinNotionalUSD       float64                `json:"max_coin_notional_usd"`
	MinEligibleMembers       int                    `json:"min_eligible_members"`
	HoldingHorizonSeconds    int64                  `json:"holding_horizon_seconds,omitempty"`
	Eligibility              *BasketEligibility     `json:"eligibility"`
}

type basketListingMs struct {
	ListedAtMs   int64 `json:"listed_at_ms"`
	DelistedAtMs int64 `json:"delisted_at_ms,omitempty"`
	KnownAtMs    int64 `json:"known_at_ms"`
}

type basketContractMember struct {
	StrategyID string `json:"strategy_id"`
	Coin       string `json:"coin"`
	Timeframe  string `json:"timeframe"`
	IntervalMs int64  `json:"interval_ms"`
}

type basketContract struct {
	Version                  int                        `json:"v"`
	BasketID                 string                     `json:"basket_id"`
	Partition                string                     `json:"partition"`
	Ranker                   string                     `json:"ranker"`
	Params                   map[string]interface{}     `json:"params,omitempty"`
	RebalanceIntervalSeconds int                        `json:"rebalance_interval_seconds"`
	EvaluationWindowSeconds  int                        `json:"evaluation_window_seconds"`
	RankerTimeoutSeconds     int                        `json:"ranker_timeout_seconds"`
	LookbackBars             int                        `json:"lookback_bars"`
	GrossBudgetUSD           float64                    `json:"gross_budget_usd"`
	MaxTransitionNetUSD      float64                    `json:"max_transition_net_usd"`
	MaxCoinNotionalUSD       float64                    `json:"max_coin_notional_usd"`
	MinEligibleMembers       int                        `json:"min_eligible_members"`
	HoldingHorizonSeconds    int64                      `json:"holding_horizon_seconds,omitempty"`
	EligibilitySource        string                     `json:"eligibility_source"`
	EligibilityVersion       string                     `json:"eligibility_version"`
	MinHistoryBars           int                        `json:"min_history_bars"`
	Listings                 map[string]basketListingMs `json:"listings"`
	Members                  []basketContractMember     `json:"members"`
}

func (c basketContract) member(id string) (basketContractMember, bool) {
	for _, m := range c.Members {
		if m.StrategyID == id {
			return m, true
		}
	}
	return basketContractMember{}, false
}

func (c basketContract) memberByCoin(coin string) (basketContractMember, bool) {
	for _, m := range c.Members {
		if m.Coin == coin {
			return m, true
		}
	}
	return basketContractMember{}, false
}

func (c basketContract) coins() []string {
	out := make([]string, 0, len(c.Members))
	for _, m := range c.Members {
		out = append(out, m.Coin)
	}
	sort.Strings(out)
	return out
}

func (c basketContract) JSON() string {
	b, err := json.Marshal(c)
	if err != nil {
		return ""
	}
	return string(b)
}

func (c basketContract) Fingerprint() string {
	sum := sha256.Sum256([]byte(c.JSON()))
	return hex.EncodeToString(sum[:])
}

func parseBasketContract(raw string) (basketContract, error) {
	var c basketContract
	if strings.TrimSpace(raw) == "" {
		return c, fmt.Errorf("saved basket contract is empty")
	}
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return c, fmt.Errorf("saved basket contract is unreadable: %w", err)
	}
	if c.Version != basketContractVersion {
		return c, fmt.Errorf("saved basket contract version %d is not supported", c.Version)
	}
	return c, nil
}

func isBasketMember(sc StrategyConfig) bool {
	return strings.TrimSpace(sc.BasketID) != ""
}

func basketMemberCoin(sc StrategyConfig) string {
	symbol, _ := strategyArgSymbolTimeframe(sc.Args)
	return symbol
}

func (c *Config) basketByID(id string) (*BasketConfig, bool) {
	if c == nil {
		return nil, false
	}
	for i := range c.Baskets {
		if c.Baskets[i].ID == id {
			return &c.Baskets[i], true
		}
	}
	return nil, false
}

func basketMemberConfigs(cfg *Config, b BasketConfig) []StrategyConfig {
	byID := make(map[string]StrategyConfig, len(cfg.Strategies))
	for _, sc := range cfg.Strategies {
		byID[sc.ID] = sc
	}
	out := make([]StrategyConfig, 0, len(b.Members))
	for _, id := range b.Members {
		if sc, ok := byID[id]; ok {
			out = append(out, sc)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func parseBasketTimestampMs(raw string) (int64, error) {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(raw))
	if err != nil {
		return 0, err
	}
	return t.UTC().UnixMilli(), nil
}

func buildBasketContract(cfg *Config, b BasketConfig) (basketContract, error) {
	members := basketMemberConfigs(cfg, b)
	if len(members) != len(b.Members) {
		return basketContract{}, fmt.Errorf("basket %s lists a member that is not a configured strategy", b.ID)
	}
	c := basketContract{
		Version:                  basketContractVersion,
		BasketID:                 b.ID,
		Ranker:                   b.Ranker,
		Params:                   b.Params,
		RebalanceIntervalSeconds: b.RebalanceIntervalSeconds,
		EvaluationWindowSeconds:  b.EvaluationWindowSeconds,
		RankerTimeoutSeconds:     b.RankerTimeoutSeconds,
		LookbackBars:             b.LookbackBars,
		GrossBudgetUSD:           b.GrossBudgetUSD,
		MaxTransitionNetUSD:      b.MaxTransitionNetUSD,
		MaxCoinNotionalUSD:       b.MaxCoinNotionalUSD,
		MinEligibleMembers:       b.MinEligibleMembers,
		HoldingHorizonSeconds:    b.HoldingHorizonSeconds,
		Listings:                 map[string]basketListingMs{},
	}
	if len(members) > 0 {
		c.Partition = partitionFor(members[0]).String()
	}
	if b.Eligibility != nil {
		c.EligibilitySource = b.Eligibility.Source
		c.EligibilityVersion = b.Eligibility.Version
		c.MinHistoryBars = b.Eligibility.MinHistoryBars
		for coin, fact := range b.Eligibility.Listings {
			var ms basketListingMs
			var err error
			if ms.ListedAtMs, err = parseBasketTimestampMs(fact.ListedAt); err != nil {
				return basketContract{}, fmt.Errorf("listing %s listed_at: %w", coin, err)
			}
			if ms.KnownAtMs, err = parseBasketTimestampMs(fact.KnownAt); err != nil {
				return basketContract{}, fmt.Errorf("listing %s known_at: %w", coin, err)
			}
			if strings.TrimSpace(fact.DelistedAt) != "" {
				if ms.DelistedAtMs, err = parseBasketTimestampMs(fact.DelistedAt); err != nil {
					return basketContract{}, fmt.Errorf("listing %s delisted_at: %w", coin, err)
				}
			}
			c.Listings[coin] = ms
		}
	}
	for _, sc := range members {
		symbol, timeframe := strategyArgSymbolTimeframe(sc.Args)
		c.Members = append(c.Members, basketContractMember{
			StrategyID: sc.ID,
			Coin:       symbol,
			Timeframe:  timeframe,
			IntervalMs: closedBarDecisionIntervalsMs[timeframe],
		})
	}
	return c, nil
}

func basketLiveRefusal(sc StrategyConfig) string {
	if !isBasketMember(sc) || !isLiveArgs(sc.Args) {
		return ""
	}
	ack := ""
	if sc.AllowNoEdgeAcknowledged() {
		ack = "; allow_no_edge: true does not lift this refusal"
	}
	return fmt.Sprintf("strategy[%s]: live basket dispatch is refused for basket %q until a separate promotion change%s", sc.ID, sc.BasketID, ack)
}

func basketRankerReferenceErrors(sc StrategyConfig) []string {
	if sc.Type == "options" {
		return nil
	}
	var errs []string
	seen := map[string]bool{}
	add := func(role, name string) {
		name = strings.TrimSpace(name)
		if name == "" || !basketRankerStrategies[name] || seen[role+"="+name] {
			return
		}
		seen[role+"="+name] = true
		errs = append(errs, fmt.Sprintf("strategy[%s]: %s strategy %q is a basket evaluator; it ranks a basket universe and cannot run on one symbol", sc.ID, role, name))
	}
	add("positional", strategyNameFromArgs(sc.Args))
	add("open", sc.OpenStrategy.Name)
	for _, ref := range sc.closeRefs() {
		add("close-fallback", canonicalCloseStrategyName(ref.Name))
	}
	return errs
}

func basketMemberFieldErrors(sc StrategyConfig) []string {
	prefix := fmt.Sprintf("strategy[%s]: basket member of %q", sc.ID, sc.BasketID)
	var errs []string
	add := func(format string, a ...interface{}) {
		errs = append(errs, prefix+" "+fmt.Sprintf(format, a...))
	}
	if sc.Platform != "hyperliquid" || sc.Type != "perps" {
		add("must be platform hyperliquid type perps (got platform=%s type=%s)", sc.Platform, sc.Type)
	}
	if filepath.Clean(strings.TrimSpace(sc.Script)) != hyperliquidCheckScript {
		add("must use the built-in %s check, got script %q", hyperliquidCheckScript, sc.Script)
	}
	if strategyNameFromArgs(sc.Args) != basketMemberStrategyToken {
		add("must name the %q positional strategy (args[0]); a member has no independent entry evaluator", basketMemberStrategyToken)
	}
	symbol, timeframe := strategyArgSymbolTimeframe(sc.Args)
	if symbol == "" || timeframe == "" {
		add("needs args [%q, <coin>, <timeframe>, ...]", basketMemberStrategyToken)
	} else if _, ok := closedBarDecisionIntervalsMs[timeframe]; !ok {
		add("timeframe %q has no fixed decision interval", timeframe)
	} else if _, ok := hlCandleIntervalMs(timeframe); !ok {
		add("timeframe %q is not a Hyperliquid candle interval", timeframe)
	}
	if strings.ContainsAny(symbol, "/:") {
		add("coin %q must be a bare Hyperliquid perps coin", symbol)
	}
	if msg := basketLiveRefusal(sc); msg != "" {
		errs = append(errs, msg)
	} else if rawEdgeGateMode(sc.Args).Kind != edgeGateModePaper {
		add("must declare --mode=paper explicitly in args")
	}
	if !sc.ClosedBarDecisions {
		add("requires \"closed_bar_decisions\": true so every increase carries a closed-bar decision identity")
	}
	if name := strings.TrimSpace(sc.OpenStrategy.Name); name != "" && name != basketMemberStrategyToken {
		add("must not set open_strategy (%q); only the basket coordinator places member increases", sc.OpenStrategy.Name)
	}
	if sc.CloseStrategy == nil || strings.TrimSpace(sc.CloseStrategy.Name) == "" {
		add("needs an explicit close_strategy")
	} else if name := canonicalCloseStrategyName(sc.CloseStrategy.Name); !isNativeCloseStrategy(name) {
		add("close_strategy %q is an open-strategy close fallback; a member needs a native close strategy so no entry evaluator runs", name)
	} else if name == "avwap_stop" {
		add("close_strategy avwap_stop needs an open-strategy avwap column, which a member does not evaluate")
	}
	if EffectiveDirection(sc) != DirectionBoth {
		add("must set direction \"both\" so the basket can hold either side (got %q)", EffectiveDirection(sc))
	}
	if sc.Capital <= 0 || math.IsNaN(sc.Capital) || math.IsInf(sc.Capital, 0) {
		add("needs a positive capital for its paper margin")
	}
	if sc.CapitalPct != 0 {
		add("must not set capital_pct")
	}
	conflicts := []struct {
		set  bool
		name string
	}{
		{sc.AllowScaleIn, "allow_scale_in"},
		{sc.ScaleIn != nil, "scale_in"},
		{sc.InvertSignal, "invert_signal"},
		{strings.TrimSpace(sc.ReplaySharing) != "", "replay_sharing"},
		{strings.TrimSpace(sc.ReplaySourceID) != "", "replay_source_id"},
		{HedgeEnabled(sc) || sc.Hedge != nil, "hedge"},
		{sc.HurstGate != nil, "hurst_gate"},
		{sc.HTFFilter, "htf_filter"},
		{len(sc.AllowedRegimes) > 0, "allowed_regimes"},
		{sc.RegimeDirectionalPolicy.IsConfigured(), "regime_directional_policy"},
		{sc.RegimeWindowDivergence.IsConfigured(), "regime_window_divergence"},
		{sc.RegimeProfileAllocation.IsConfigured(), "regime_profile_allocation"},
		{sc.LLMEntryAnalysis != nil, "llm_entry_analysis"},
		{sc.RiskPerTradePct != nil, "risk_per_trade_pct"},
		{sc.MarginPerTradeUSD != nil, "margin_per_trade_usd"},
	}
	for _, c := range conflicts {
		if c.set {
			add("must not set %s: it configures an independent entry route, and basket increases use the frozen target quantity", c.name)
		}
	}
	return errs
}

func basketParamErrors(prefix string, b BasketConfig) []string {
	allowed, ok := basketRankerParamKeys[b.Ranker]
	if !ok {
		return nil
	}
	known := make(map[string]bool, len(allowed))
	for _, k := range allowed {
		known[k] = true
	}
	keys := make([]string, 0, len(b.Params))
	for k := range b.Params {
		if !known[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, fmt.Sprintf("%s: unknown field %q in params for ranker %s", prefix, k, b.Ranker))
	}
	return out
}

func basketFinitePositive(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v > 0
}

func basketConfigErrors(cfg *Config) []string {
	if cfg == nil {
		return nil
	}
	var errs []string
	byID := make(map[string]StrategyConfig, len(cfg.Strategies))
	for _, sc := range cfg.Strategies {
		byID[sc.ID] = sc
	}
	basketIDs := make(map[string]bool)
	memberOf := make(map[string]string)
	basketCoins := make(map[string]map[string]bool)
	basketPartition := make(map[string]RiskPartition)
	for i, b := range cfg.Baskets {
		prefix := fmt.Sprintf("baskets[%d]", i)
		if b.ID != "" {
			prefix = fmt.Sprintf("baskets[%s]", b.ID)
		}
		add := func(format string, a ...interface{}) {
			errs = append(errs, prefix+": "+fmt.Sprintf(format, a...))
		}
		if !basketIDPattern.MatchString(b.ID) {
			add("id %q must match %s", b.ID, basketIDPattern.String())
		}
		if basketIDs[b.ID] {
			add("duplicate basket id %q", b.ID)
		}
		basketIDs[b.ID] = true
		if strings.TrimSpace(b.Ranker) == "" {
			add("needs a ranker")
		} else if !basketRankerStrategies[b.Ranker] {
			add("ranker %q is not a registered basket evaluator", b.Ranker)
		}
		errs = append(errs, basketParamErrors(prefix, b)...)
		if b.RebalanceIntervalSeconds <= 0 || b.RebalanceIntervalSeconds%60 != 0 {
			add("rebalance_interval_seconds must be a positive multiple of 60, got %d", b.RebalanceIntervalSeconds)
		}
		if b.EvaluationWindowSeconds <= 0 || (b.RebalanceIntervalSeconds > 0 && b.EvaluationWindowSeconds >= b.RebalanceIntervalSeconds) {
			add("evaluation_window_seconds must be positive and shorter than the rebalance interval, got %d", b.EvaluationWindowSeconds)
		}
		if b.RankerTimeoutSeconds <= 0 || b.RankerTimeoutSeconds > basketMaxRankerTimeoutSeconds || (b.EvaluationWindowSeconds > 0 && b.RankerTimeoutSeconds > b.EvaluationWindowSeconds) {
			add("ranker_timeout_seconds must be in [1, min(evaluation_window_seconds, %d)], got %d", basketMaxRankerTimeoutSeconds, b.RankerTimeoutSeconds)
		}
		if b.LookbackBars < 2 || b.LookbackBars > basketMaxLookbackBars {
			add("lookback_bars must be in [2, %d], got %d", basketMaxLookbackBars, b.LookbackBars)
		}
		if !basketFinitePositive(b.GrossBudgetUSD) {
			add("gross_budget_usd must be a finite positive number, got %v", b.GrossBudgetUSD)
		}
		if !basketFinitePositive(b.MaxTransitionNetUSD) {
			add("max_transition_net_usd must be a finite positive number, got %v", b.MaxTransitionNetUSD)
		} else if basketFinitePositive(b.GrossBudgetUSD) && b.MaxTransitionNetUSD > b.GrossBudgetUSD {
			add("max_transition_net_usd %v exceeds gross_budget_usd %v", b.MaxTransitionNetUSD, b.GrossBudgetUSD)
		}
		if !basketFinitePositive(b.MaxCoinNotionalUSD) {
			add("max_coin_notional_usd must be a finite positive number, got %v", b.MaxCoinNotionalUSD)
		} else if basketFinitePositive(b.GrossBudgetUSD) && b.MaxCoinNotionalUSD > b.GrossBudgetUSD {
			add("max_coin_notional_usd %v exceeds gross_budget_usd %v", b.MaxCoinNotionalUSD, b.GrossBudgetUSD)
		}
		if len(b.Members) < basketMinMembers {
			add("needs at least %d members, got %d", basketMinMembers, len(b.Members))
		}
		if b.MinEligibleMembers < basketMinMembers || b.MinEligibleMembers > len(b.Members) {
			add("min_eligible_members must be in [%d, %d], got %d", basketMinMembers, len(b.Members), b.MinEligibleMembers)
		}
		if b.HoldingHorizonSeconds < 0 || b.HoldingHorizonSeconds > basketMaxHoldingHorizonSeconds {
			add("holding_horizon_seconds must be an integer in [0, %d] (0 or absent turns expiry off), got %d", basketMaxHoldingHorizonSeconds, b.HoldingHorizonSeconds)
		}
		if !cfg.marketFeedDeadlineScheduled() {
			add("needs market_feed %q or %q: the rest feed has no sealed deadline snapshot (got %q)", marketFeedWebsocket, marketFeedShared, cfg.marketFeedMode())
		}
		coins := make(map[string]bool)
		seenMember := make(map[string]bool)
		var partition RiskPartition
		partitionSet := false
		for _, id := range b.Members {
			if seenMember[id] {
				add("member %q is listed twice", id)
				continue
			}
			seenMember[id] = true
			sc, ok := byID[id]
			if !ok {
				add("member %q is not a configured strategy", id)
				continue
			}
			if other, taken := memberOf[id]; taken && other != b.ID {
				add("member %q already belongs to basket %q", id, other)
			}
			memberOf[id] = b.ID
			if sc.BasketID != b.ID {
				add("member %q must set \"basket_id\": %q (got %q)", id, b.ID, sc.BasketID)
			}
			coin := basketMemberCoin(sc)
			if coin != "" {
				if coins[coin] {
					add("duplicate coin %s: two members would trade one instrument", coin)
				}
				coins[coin] = true
			}
			p := partitionFor(sc)
			if !partitionSet {
				partition, partitionSet = p, true
			} else if p != partition {
				add("members span risk partitions %s and %s (mixed partition or paper_source)", partitionLabel(partition), partitionLabel(p))
			}
			_, timeframe := strategyArgSymbolTimeframe(sc.Args)
			if ms, ok := closedBarDecisionIntervalsMs[timeframe]; ok && b.RebalanceIntervalSeconds > 0 {
				if (int64(b.RebalanceIntervalSeconds)*1000)%ms != 0 {
					add("rebalance_interval_seconds %d is not a multiple of member %s candle interval %s", b.RebalanceIntervalSeconds, id, timeframe)
				}
			}
		}
		basketCoins[b.ID] = coins
		basketPartition[b.ID] = partition
		if b.Eligibility == nil {
			add("needs an eligibility block with source, version, min_history_bars and listings")
		} else {
			e := b.Eligibility
			if strings.TrimSpace(e.Source) == "" {
				add("eligibility.source must name the operator-declared evidence source")
			}
			if strings.TrimSpace(e.Version) == "" {
				add("eligibility.version must be set")
			}
			if e.MinHistoryBars < 1 || (b.LookbackBars > 0 && e.MinHistoryBars > b.LookbackBars) {
				add("eligibility.min_history_bars must be in [1, lookback_bars], got %d", e.MinHistoryBars)
			}
			listingCoins := make([]string, 0, len(e.Listings))
			for coin := range e.Listings {
				listingCoins = append(listingCoins, coin)
			}
			sort.Strings(listingCoins)
			for _, coin := range listingCoins {
				fact := e.Listings[coin]
				if !coins[coin] {
					add("eligibility.listings names %s, which is not a member coin", coin)
				}
				listed, err := parseBasketTimestampMs(fact.ListedAt)
				if err != nil {
					add("eligibility.listings[%s].listed_at %q is not RFC 3339", coin, fact.ListedAt)
				}
				if _, err := parseBasketTimestampMs(fact.KnownAt); err != nil {
					add("eligibility.listings[%s].known_at %q is not RFC 3339", coin, fact.KnownAt)
				}
				if strings.TrimSpace(fact.DelistedAt) != "" {
					delisted, derr := parseBasketTimestampMs(fact.DelistedAt)
					if derr != nil {
						add("eligibility.listings[%s].delisted_at %q is not RFC 3339", coin, fact.DelistedAt)
					} else if err == nil && delisted <= listed {
						add("eligibility.listings[%s].delisted_at is not after listed_at", coin)
					}
				}
			}
			memberCoins := make([]string, 0, len(coins))
			for coin := range coins {
				memberCoins = append(memberCoins, coin)
			}
			sort.Strings(memberCoins)
			for _, coin := range memberCoins {
				if _, ok := e.Listings[coin]; !ok {
					add("eligibility.listings has no listing fact for member coin %s", coin)
				}
			}
		}
	}
	for _, sc := range cfg.Strategies {
		errs = append(errs, basketRankerReferenceErrors(sc)...)
		if isBasketMember(sc) {
			if !basketIDs[sc.BasketID] {
				errs = append(errs, fmt.Sprintf("strategy[%s]: basket_id %q names no configured basket", sc.ID, sc.BasketID))
				continue
			}
			if memberOf[sc.ID] != sc.BasketID {
				errs = append(errs, fmt.Sprintf("strategy[%s]: basket_id %q but that basket does not list it as a member", sc.ID, sc.BasketID))
			}
			errs = append(errs, basketMemberFieldErrors(sc)...)
			continue
		}
		if strategyNameFromArgs(sc.Args) == basketMemberStrategyToken || strings.TrimSpace(sc.OpenStrategy.Name) == basketMemberStrategyToken {
			errs = append(errs, fmt.Sprintf("strategy[%s]: %q is reserved for basket members (set basket_id)", sc.ID, basketMemberStrategyToken))
		}
		part := partitionFor(sc)
		held := []string{extractAsset(sc), hedgeCoin(sc)}
		if sc.Type == "manual" {
			held = append(held, strings.ToUpper(strings.TrimSpace(sc.Symbol)))
		}
		ids := make([]string, 0, len(basketCoins))
		for id := range basketCoins {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			if basketPartition[id] != part {
				continue
			}
			for _, coin := range held {
				if coin != "" && basketCoins[id][coin] {
					errs = append(errs, fmt.Sprintf("strategy[%s]: holds %s in the %s partition, which basket %q owns exclusively", sc.ID, coin, partitionLabel(part), id))
				}
			}
		}
	}
	sort.Strings(errs)
	return dedupeSortedStrings(errs)
}

func dedupeSortedStrings(in []string) []string {
	out := in[:0]
	for i, s := range in {
		if i > 0 && s == in[i-1] {
			continue
		}
		out = append(out, s)
	}
	return out
}

func (c basketContract) horizonMs() int64 {
	if c.HoldingHorizonSeconds <= 0 || c.HoldingHorizonSeconds > basketMaxHoldingHorizonSeconds {
		return 0
	}
	return c.HoldingHorizonSeconds * 1000
}

func validateBasketRawFields(rawData []byte) []string {
	var envelope struct {
		Baskets []map[string]json.RawMessage `json:"baskets"`
	}
	if err := json.Unmarshal(rawData, &envelope); err != nil {
		return nil
	}
	var errs []string
	for i, b := range envelope.Baskets {
		prefix := fmt.Sprintf("baskets[%d]", i)
		if raw, ok := b["id"]; ok {
			var id string
			if json.Unmarshal(raw, &id) == nil && id != "" {
				prefix = fmt.Sprintf("baskets[%s]", id)
			}
		}
		raw, ok := b["holding_horizon_seconds"]
		if !ok {
			continue
		}
		if why := basketHorizonRawError(raw); why != "" {
			errs = append(errs, fmt.Sprintf("%s: holding_horizon_seconds %s", prefix, why))
		}
	}
	return errs
}

func basketHorizonRawError(raw json.RawMessage) string {
	text := strings.TrimSpace(string(raw))
	if text == "" || text == "null" {
		return "must be an integer number of seconds, got null"
	}
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	var v interface{}
	if err := dec.Decode(&v); err != nil {
		return fmt.Sprintf("is unreadable: %v", err)
	}
	n, ok := v.(json.Number)
	if !ok {
		return fmt.Sprintf("must be an integer number of seconds, got %s", text)
	}
	i, err := n.Int64()
	if err != nil {
		return fmt.Sprintf("must be an integer number of seconds in [0, %d], got %s", basketMaxHoldingHorizonSeconds, text)
	}
	if i < 0 || i > basketMaxHoldingHorizonSeconds {
		return fmt.Sprintf("must be in [0, %d] (0 or absent turns expiry off), got %d", basketMaxHoldingHorizonSeconds, i)
	}
	return ""
}

func knownBasketConfigKeys() map[string]bool {
	return knownJSONKeys(reflect.TypeOf(BasketConfig{}))
}

func validateBasketJSONKeys(rawData []byte) []string {
	var envelope struct {
		Baskets []map[string]json.RawMessage `json:"baskets"`
	}
	if err := json.Unmarshal(rawData, &envelope); err != nil {
		return nil
	}
	known := knownBasketConfigKeys()
	eligKnown := knownJSONKeys(reflect.TypeOf(BasketEligibility{}))
	factKnown := knownJSONKeys(reflect.TypeOf(BasketListingFact{}))
	var errs []string
	for i, b := range envelope.Baskets {
		prefix := fmt.Sprintf("baskets[%d]", i)
		if raw, ok := b["id"]; ok {
			var id string
			if json.Unmarshal(raw, &id) == nil && id != "" {
				prefix = fmt.Sprintf("baskets[%s]", id)
			}
		}
		keys := make([]string, 0, len(b))
		for k := range b {
			if !known[k] {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			errs = append(errs, fmt.Sprintf("%s: unknown field %q", prefix, k))
		}
		errs = append(errs, nestedObjectUnknownKeyErrors(b, "eligibility", eligKnown, prefix)...)
		rawElig, ok := b["eligibility"]
		if !ok {
			continue
		}
		var elig map[string]json.RawMessage
		if json.Unmarshal(rawElig, &elig) != nil || elig == nil {
			continue
		}
		rawListings, ok := elig["listings"]
		if !ok {
			continue
		}
		var listings map[string]map[string]json.RawMessage
		if json.Unmarshal(rawListings, &listings) != nil {
			continue
		}
		coins := make([]string, 0, len(listings))
		for coin := range listings {
			coins = append(coins, coin)
		}
		sort.Strings(coins)
		for _, coin := range coins {
			fields := make([]string, 0, len(listings[coin]))
			for k := range listings[coin] {
				if !factKnown[k] {
					fields = append(fields, k)
				}
			}
			sort.Strings(fields)
			for _, k := range fields {
				errs = append(errs, fmt.Sprintf("%s: unknown field %q in eligibility.listings[%s]", prefix, k, coin))
			}
		}
	}
	return errs
}

func basketReloadErrors(cfg, next *Config) []string {
	var errs []string
	if !reflect.DeepEqual(cfg.Baskets, next.Baskets) {
		errs = append(errs, "baskets changed (membership, ranker, schedule, evaluation window, ranker timeout, budgets and holding_horizon_seconds are restart-required)")
	}
	nextByID := strategyConfigByID(next.Strategies)
	for _, sc := range cfg.Strategies {
		ns, ok := nextByID[sc.ID]
		if !ok {
			continue
		}
		if strings.TrimSpace(sc.BasketID) != strings.TrimSpace(ns.BasketID) {
			errs = append(errs, fmt.Sprintf("strategy[%s] basket_id changed (%q -> %q; restart required)", sc.ID, sc.BasketID, ns.BasketID))
		}
	}
	return errs
}

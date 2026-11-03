package main

import (
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
)

const optionPremiumUnitUnderlying = "underlying_unit"

const (
	optionSettlementCash     = "cash"
	optionSettlementPhysical = "physical"
)

type optionContractSpec struct {
	Platform            string
	Multiplier          float64
	RequireCoveredCalls bool
	Settlement          string
}

var optionContractSpecs = map[string]optionContractSpec{
	"robinhood": {Platform: "robinhood", Multiplier: 100, RequireCoveredCalls: true, Settlement: optionSettlementPhysical},
	"deribit":   {Platform: "deribit", Multiplier: 1, Settlement: optionSettlementCash},
	"okx":       {Platform: "okx", Multiplier: 1, Settlement: optionSettlementCash},
	"ibkr":      {Platform: "ibkr", Multiplier: 1, Settlement: optionSettlementCash},
}

func optionSettlementStyle(platform string) (string, error) {
	spec, ok := optionContractSpecs[platform]
	if !ok {
		return "", fmt.Errorf("no option settlement style for platform %q", platform)
	}
	switch spec.Settlement {
	case optionSettlementCash, optionSettlementPhysical:
		return spec.Settlement, nil
	case "":
		return "", fmt.Errorf("option settlement style for platform %q is not set", platform)
	}
	return "", fmt.Errorf("option settlement style %q for platform %q is not cash or physical", spec.Settlement, platform)
}

var optionContractUnderlyingMultipliers = map[string]map[string]float64{}

func lookupOptionContractSpec(platform, underlying string) (optionContractSpec, error) {
	spec, ok := optionContractSpecs[platform]
	if !ok {
		return optionContractSpec{}, fmt.Errorf("no option contract size for platform %q", platform)
	}
	if overrides := optionContractUnderlyingMultipliers[platform]; overrides != nil {
		if m, ok := overrides[strings.ToUpper(underlying)]; ok {
			spec.Multiplier = m
		}
	}
	if !finitePositive(spec.Multiplier) {
		return optionContractSpec{}, fmt.Errorf("option contract size for %s %s is not positive", platform, underlying)
	}
	return spec, nil
}

var optionInverseFamilyUnderlyings = map[string]bool{"BTC": true, "ETH": true}

func supportedOptionPlatforms() []string {
	out := make([]string, 0, len(optionContractSpecs))
	for p := range optionContractSpecs {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func optionPricingPolicyError(platform, underlying string) error {
	if _, ok := optionContractSpecs[platform]; !ok {
		return fmt.Errorf("options platform %q has no option pricer (supported: %s)", platform, strings.Join(supportedOptionPlatforms(), ", "))
	}
	u := strings.ToUpper(strings.TrimSpace(underlying))
	switch platform {
	case "deribit", "okx":
		if !optionInverseFamilyUnderlyings[u] {
			return fmt.Errorf("%s options price only the inverse BTC and ETH option families (instrument UNDERLYING-DDMONYY-STRIKE-C|P); underlying %q is outside that local naming path", platform, underlying)
		}
	case "robinhood":
		if u == "" || robinhoodYahooCryptoPair(u) {
			return fmt.Errorf("robinhood options price only listed equity and ETF underlyings; %q resolves to the Yahoo crypto pair %s, which has no listed option to mark or settle", underlying, robinhoodYahooSymbol(u))
		}
	}
	return nil
}

func optionsConfigUnderlying(args []string) string {
	var positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case strings.HasPrefix(a, "--mode="), strings.HasPrefix(a, "--platform="), strings.HasPrefix(a, "--regime-payload-json"):
		case a == "--mode":
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
				i++
			}
		default:
			positional = append(positional, a)
		}
	}
	if len(positional) < 2 {
		return ""
	}
	return strings.ToUpper(positional[1])
}

func optionsConfigPolicyError(sc StrategyConfig) error {
	if sc.Type != "options" {
		return nil
	}
	if _, ok := optionContractSpecs[sc.Platform]; !ok {
		return optionPricingPolicyError(sc.Platform, "")
	}
	u := optionsConfigUnderlying(sc.Args)
	if u == "" {
		return nil
	}
	return optionPricingPolicyError(sc.Platform, u)
}

func optionStrategyCannotCoverCalls(strategy, platform string) bool {
	spec, ok := optionContractSpecs[platform]
	return ok && spec.RequireCoveredCalls && (strategy == "covered_calls" || strategy == "butterfly")
}

func coveredCallsSharesWarnings(cfg *Config) []string {
	if cfg == nil {
		return nil
	}
	var out []string
	for _, sc := range cfg.Strategies {
		if sc.Type != "options" || len(sc.Args) == 0 || !optionStrategyCannotCoverCalls(sc.Args[0], sc.Platform) {
			continue
		}
		spec := optionContractSpecs[sc.Platform]
		out = append(out, fmt.Sprintf("[WARN] %s: %s on %s sells short calls and never buys shares, but a %s short call needs %g held long shares of the underlying per contract in this same strategy, so it cannot open a leg; use wheel (it gets shares from a put assignment) or remove the strategy", sc.ID, sc.Args[0], sc.Platform, sc.Platform, spec.Multiplier))
	}
	return out
}

func maxOptionContractMultiplier() float64 {
	max := 1.0
	for _, spec := range optionContractSpecs {
		if spec.Multiplier > max {
			max = spec.Multiplier
		}
	}
	for _, overrides := range optionContractUnderlyingMultipliers {
		for _, m := range overrides {
			if m > max {
				max = m
			}
		}
	}
	return max
}

func optionOwnerPlatform(s *StrategyState) string {
	if s == nil {
		return ""
	}
	if s.Platform != "" {
		return s.Platform
	}
	sc := StrategyConfig{ID: s.ID, Type: "options"}
	inferStrategyPlatform(&sc)
	return sc.Platform
}

func optionPositionMultiplier(platform string, pos *OptionPosition) (float64, error) {
	if pos == nil {
		return 0, fmt.Errorf("missing option leg")
	}
	m := pos.ContractMultiplier
	if math.IsNaN(m) || math.IsInf(m, 0) || m < 0 {
		return 0, fmt.Errorf("invalid contract multiplier %v", m)
	}
	if m > 0 {
		return m, nil
	}
	spec, err := lookupOptionContractSpec(platform, pos.Underlying)
	if err != nil {
		return 0, err
	}
	return spec.Multiplier, nil
}

func strategyOptionMultiplier(s *StrategyState, key string, pos *OptionPosition) (float64, error) {
	m, err := optionPositionMultiplier(optionOwnerPlatform(s), pos)
	if err != nil {
		id := ""
		if s != nil {
			id = s.ID
		}
		return 0, fmt.Errorf("strategy %s option leg %s: %w", id, key, err)
	}
	return m, nil
}

func optionRiskMultiplier(platform string, pos *OptionPosition) float64 {
	m, err := optionPositionMultiplier(platform, pos)
	if err != nil {
		fallback := maxOptionContractMultiplier()
		fmt.Printf("[WARN] option risk: leg %s on %q has no usable contract size (%v); using the largest known size %g\n", pos.ID, platform, err, fallback)
		return fallback
	}
	return m
}

func optionContractValueUSD(perUnitUSD, multiplier, quantity float64) (float64, error) {
	if math.IsNaN(perUnitUSD) || math.IsInf(perUnitUSD, 0) || perUnitUSD < 0 {
		return 0, fmt.Errorf("per-unit value %v is not a finite non-negative amount", perUnitUSD)
	}
	if !finitePositive(multiplier) {
		return 0, fmt.Errorf("contract multiplier %v is not positive", multiplier)
	}
	if !finitePositive(quantity) {
		return 0, fmt.Errorf("contract quantity %v is not positive", quantity)
	}
	v := perUnitUSD * multiplier * quantity
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, fmt.Errorf("contract value overflows (%v x %v x %v)", perUnitUSD, multiplier, quantity)
	}
	return v, nil
}

func sortedOptionLegKeys(m map[string]*OptionPosition) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func reservedShortPutCollateralUSD(s *StrategyState) (float64, error) {
	total := 0.0
	for _, key := range sortedOptionLegKeys(s.OptionPositions) {
		pos := s.OptionPositions[key]
		if pos == nil || pos.Action != "sell" || pos.OptionType != "put" {
			continue
		}
		m, err := strategyOptionMultiplier(s, key, pos)
		if err != nil {
			return 0, err
		}
		v, err := optionContractValueUSD(pos.Strike, m, pos.Quantity)
		if err != nil {
			return 0, fmt.Errorf("strategy %s option leg %s collateral: %w", s.ID, key, err)
		}
		total += v
	}
	if math.IsInf(total, 0) {
		return 0, fmt.Errorf("strategy %s reserved put collateral overflows", s.ID)
	}
	return total, nil
}

func reservedShortCallShares(s *StrategyState, underlying string) (float64, error) {
	total := 0.0
	for _, key := range sortedOptionLegKeys(s.OptionPositions) {
		pos := s.OptionPositions[key]
		if pos == nil || pos.Action != "sell" || pos.OptionType != "call" || !strings.EqualFold(pos.Underlying, underlying) {
			continue
		}
		m, err := strategyOptionMultiplier(s, key, pos)
		if err != nil {
			return 0, err
		}
		shares, err := optionContractValueUSD(1, m, pos.Quantity)
		if err != nil {
			return 0, fmt.Errorf("strategy %s option leg %s cover: %w", s.ID, key, err)
		}
		total += shares
	}
	return total, nil
}

func heldLongUnderlyingShares(s *StrategyState, underlying string) float64 {
	pos := s.Positions[strings.ToUpper(underlying)]
	if pos == nil || pos.Side != "long" || !finitePositive(pos.Quantity) {
		return 0
	}
	return pos.Quantity
}

func legacyCashSettledUnderlyingWarnings(state *AppState, strategies []StrategyConfig) []string {
	if state == nil {
		return nil
	}
	type hit struct {
		id, platform, symbol string
		pos                  *Position
	}
	var hits []hit
	for _, sc := range strategies {
		if sc.Type != "options" {
			continue
		}
		c := sc
		inferStrategyPlatform(&c)
		if style, err := optionSettlementStyle(c.Platform); err != nil || style != optionSettlementCash {
			continue
		}
		symbol := optionsConfigUnderlying(sc.Args)
		s := state.Strategies[sc.ID]
		if symbol == "" || s == nil {
			continue
		}
		if pos := s.Positions[symbol]; pos != nil && pos.Side == "long" && pos.Quantity > 0 {
			hits = append(hits, hit{id: sc.ID, platform: c.Platform, symbol: symbol, pos: pos})
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].id != hits[j].id {
			return hits[i].id < hits[j].id
		}
		return hits[i].symbol < hits[j].symbol
	})
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, fmt.Sprintf("options strategy %s on %s settles expiry in cash but holds a legacy long %s quantity=%g cost_basis=%g from an earlier physical assignment; it is kept and not repriced, and a cycle with no %s price key values it at cost basis (#1917)",
			h.id, h.platform, h.symbol, h.pos.Quantity, h.pos.AvgCost, h.symbol))
	}
	return out
}

func optionsResultContractError(platform string, result *OptionsResult) error {
	if result == nil {
		return fmt.Errorf("options result contract: missing result")
	}
	if result.PremiumUnit != optionPremiumUnitUnderlying {
		return fmt.Errorf("options result contract: premium_unit %q, want %q", result.PremiumUnit, optionPremiumUnitUnderlying)
	}
	if result.Platform == "" || result.Platform != platform {
		return fmt.Errorf("options result contract: check priced on platform %q but the strategy platform is %q; set the strategy platform and its --platform argument to the same venue", result.Platform, platform)
	}
	return nil
}

func stampLoadedOptionContractMultipliers(state *AppState, cfg *Config) (int, error) {
	if state == nil {
		return 0, nil
	}
	configured := make(map[string]string)
	if cfg != nil {
		for _, sc := range cfg.Strategies {
			c := sc
			inferStrategyPlatform(&c)
			configured[sc.ID] = c.Platform
		}
	}
	ids := make([]string, 0, len(state.Strategies))
	for id := range state.Strategies {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	stamped := 0
	for _, id := range ids {
		s := state.Strategies[id]
		if s == nil || len(s.OptionPositions) == 0 {
			continue
		}
		platform, ok := configured[id]
		if !ok {
			platform = optionOwnerPlatform(s)
		}
		for _, key := range sortedOptionLegKeys(s.OptionPositions) {
			pos := s.OptionPositions[key]
			if pos == nil {
				continue
			}
			m, err := optionPositionMultiplier(platform, pos)
			if err != nil {
				return stamped, fmt.Errorf("strategy %s option leg %s: %w", id, key, err)
			}
			if pos.ContractMultiplier == 0 {
				pos.ContractMultiplier = m
				stamped++
			}
		}
	}
	return stamped, nil
}

func validStoredOptionMultiplier(strategyID, key string, m float64) error {
	if math.IsNaN(m) || math.IsInf(m, 0) || m < 0 {
		return fmt.Errorf("strategy %s option leg %s: invalid contract multiplier %v", strategyID, key, m)
	}
	return nil
}

const maxOptionPositionsPerStrategy = 4

func optionStrikeText(strike float64) string {
	return strconv.FormatFloat(strike, 'f', -1, 64)
}

func optionContractKey(underlying, optionType, action string, strike float64, expiry string) string {
	return fmt.Sprintf("%s-%s-%s-%s-%s", underlying, optionType, action, optionStrikeText(strike), expiry)
}

func legacyOptionContractKey(underlying, optionType, action string, strike float64, expiry string) string {
	return fmt.Sprintf("%s-%s-%s-%.0f-%s", underlying, optionType, action, strike, expiry)
}

type optionContractIdentity struct {
	Underlying string
	OptionType string
	Action     string
	Strike     float64
	Expiry     string
}

func optionLegIdentity(p *OptionPosition) optionContractIdentity {
	return optionContractIdentity{Underlying: strings.ToUpper(p.Underlying), OptionType: p.OptionType, Action: p.Action, Strike: p.Strike, Expiry: p.Expiry}
}

func optionIdentityKeyable(p *OptionPosition) error {
	switch {
	case p == nil:
		return fmt.Errorf("missing leg")
	case strings.TrimSpace(p.Underlying) == "":
		return fmt.Errorf("underlying is empty")
	case p.OptionType != "call" && p.OptionType != "put":
		return fmt.Errorf("option type %q is not call or put", p.OptionType)
	case p.Action != "buy" && p.Action != "sell":
		return fmt.Errorf("action %q is not buy or sell", p.Action)
	case !finitePositive(p.Strike):
		return fmt.Errorf("strike %v is not a finite positive number", p.Strike)
	case strings.TrimSpace(p.Expiry) == "":
		return fmt.Errorf("expiry is empty")
	}
	return nil
}

type optionKeyNormalization struct {
	Renamed  map[string]string
	Warnings []string
}

func planOptionKeyNormalization(legs map[string]*OptionPosition) optionKeyNormalization {
	out := optionKeyNormalization{Renamed: map[string]string{}}
	ids := sortedOptionLegKeys(legs)
	byIdentity := make(map[optionContractIdentity][]string)
	keyable := make(map[string]bool, len(ids))
	for _, id := range ids {
		leg := legs[id]
		if err := optionIdentityKeyable(leg); err != nil {
			if leg != nil {
				out.Warnings = append(out.Warnings, fmt.Sprintf("option leg %s keeps its stored id: %v", id, err))
			}
			continue
		}
		keyable[id] = true
		ident := optionLegIdentity(leg)
		byIdentity[ident] = append(byIdentity[ident], id)
	}
	retained := make(map[string]bool)
	var dupGroups []string
	for _, group := range byIdentity {
		if len(group) < 2 {
			continue
		}
		sort.Strings(group)
		for _, id := range group {
			retained[id] = true
		}
		dupGroups = append(dupGroups, strings.Join(group, ", "))
	}
	sort.Strings(dupGroups)
	for _, g := range dupGroups {
		out.Warnings = append(out.Warnings, fmt.Sprintf("option legs [%s] hold the same contract; each keeps its stored id", g))
	}
	for _, id := range ids {
		if !keyable[id] || retained[id] {
			continue
		}
		leg := legs[id]
		canonical := optionContractKey(leg.Underlying, leg.OptionType, leg.Action, leg.Strike, leg.Expiry)
		if canonical == id || legacyOptionContractKey(leg.Underlying, leg.OptionType, leg.Action, leg.Strike, leg.Expiry) != id {
			continue
		}
		out.Renamed[id] = canonical
	}
	for {
		owners := make(map[string][]string, len(ids))
		for _, id := range ids {
			final := id
			if dest, ok := out.Renamed[id]; ok {
				final = dest
			}
			owners[final] = append(owners[final], id)
		}
		finals := make([]string, 0, len(owners))
		for f := range owners {
			finals = append(finals, f)
		}
		sort.Strings(finals)
		changed := false
		for _, f := range finals {
			if len(owners[f]) < 2 {
				continue
			}
			for _, id := range owners[f] {
				if _, ok := out.Renamed[id]; ok {
					delete(out.Renamed, id)
					out.Warnings = append(out.Warnings, fmt.Sprintf("option leg %s keeps its stored id: its canonical key %s collides with another leg", id, f))
					changed = true
				}
			}
		}
		if !changed {
			break
		}
	}
	return out
}

func normalizeLoadedOptionKeys(s *StrategyState) optionKeyNormalization {
	if s == nil || len(s.OptionPositions) == 0 {
		return optionKeyNormalization{}
	}
	plan := planOptionKeyNormalization(s.OptionPositions)
	if len(plan.Renamed) > 0 {
		next := make(map[string]*OptionPosition, len(s.OptionPositions))
		for id, leg := range s.OptionPositions {
			if dest, ok := plan.Renamed[id]; ok {
				leg.ID = dest
				next[dest] = leg
				continue
			}
			next[id] = leg
		}
		s.OptionPositions = next
		if pending := s.RiskState.getPendingCircuitClose(PlatformPendingCloseRobinhoodOptions); pending != nil {
			for i := range pending.Symbols {
				if dest, ok := plan.Renamed[pending.Symbols[i].Symbol]; ok {
					pending.Symbols[i].Symbol = dest
				}
			}
		}
	}
	renamed := make([]string, 0, len(plan.Renamed))
	for old := range plan.Renamed {
		renamed = append(renamed, old)
	}
	sort.Strings(renamed)
	for _, old := range renamed {
		fmt.Fprintf(os.Stderr, "[state] strategy %s option leg %s renamed to %s (lossless strike key)\n", s.ID, old, plan.Renamed[old])
	}
	for _, w := range plan.Warnings {
		fmt.Fprintf(os.Stderr, "[state] WARN: strategy %s %s\n", s.ID, w)
	}
	return plan
}

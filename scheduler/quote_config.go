package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"
)

const (
	quoteCapabilityVersion      = 1
	inventoryQuoteExitCloseName = "inventory_quote_exit"

	quoteVerdictHealthy   = "healthy"
	quoteVerdictUnhealthy = "unhealthy"

	quoteExitKindNone     = ""
	quoteExitKindReducing = "reducing"
	quoteExitKindExit     = "exit"

	quoteExitReasonAge = "inventory_age"
	quoteExitReasonCap = "inventory_cap"

	quoteMaxRequestExpiryMs = 600_000
)

var researchOnlyCloseStrategies = map[string]bool{
	inventoryQuoteExitCloseName: true,
}

type quoteCapabilitySpec struct {
	Version  int
	Validate func(params map[string]interface{}) error
}

var quoteCapabilityStrategies = map[string]quoteCapabilitySpec{}

type QuoteConfig struct {
	Coin                 string  `json:"coin"`
	MaxInventoryUSD      float64 `json:"max_inventory_usd"`
	MaxOrderQty          float64 `json:"max_order_qty"`
	RequestExpiryMs      int64   `json:"request_expiry_ms"`
	FeeReserveRate       float64 `json:"fee_reserve_rate"`
	MarginPriceBufferPct float64 `json:"margin_price_buffer_pct"`
}

func knownQuoteConfigKeys() map[string]bool {
	return knownJSONKeys(reflect.TypeOf(QuoteConfig{}))
}

var quoteNumericKeys = map[string]bool{
	"max_inventory_usd":       true,
	"max_order_qty":           true,
	"request_expiry_ms":       true,
	"fee_reserve_rate":        true,
	"margin_price_buffer_pct": true,
}

func validateQuoteRawFields(rawData []byte) []string {
	var envelope struct {
		Strategies []map[string]json.RawMessage `json:"strategies"`
	}
	if err := json.Unmarshal(rawData, &envelope); err != nil {
		return nil
	}
	var errs []string
	for i, s := range envelope.Strategies {
		raw, ok := s["quote"]
		if !ok {
			continue
		}
		prefix := fmt.Sprintf("strategy[%d]", i)
		if idRaw, ok := s["id"]; ok {
			var id string
			if json.Unmarshal(idRaw, &id) == nil && id != "" {
				prefix = fmt.Sprintf("strategy[%s]", id)
			}
		}
		errs = append(errs, quoteRawBlockErrors(prefix, raw)...)
	}
	return errs
}

func quoteRawBlockErrors(prefix string, raw json.RawMessage) []string {
	text := strings.TrimSpace(string(raw))
	if text == "" || text == "null" || !strings.HasPrefix(text, "{") {
		return []string{fmt.Sprintf("%s: quote must be an object, got %s", prefix, text)}
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return []string{fmt.Sprintf("%s: quote block is unreadable: %v", prefix, err)}
	}
	var errs []string
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	known := knownQuoteConfigKeys()
	for _, k := range keys {
		if !known[k] {
			errs = append(errs, fmt.Sprintf("%s: unknown field %q in quote block", prefix, k))
			continue
		}
		v := strings.TrimSpace(string(obj[k]))
		if k == "coin" {
			var coin string
			if err := json.Unmarshal(obj[k], &coin); err != nil || strings.TrimSpace(coin) == "" {
				errs = append(errs, fmt.Sprintf("%s: quote.coin must be a non-empty string, got %s", prefix, v))
			}
			continue
		}
		if !quoteNumericKeys[k] {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(v))
		dec.UseNumber()
		var parsed interface{}
		if err := dec.Decode(&parsed); err != nil {
			errs = append(errs, fmt.Sprintf("%s: quote.%s is unreadable: %v", prefix, k, err))
			continue
		}
		num, ok := parsed.(json.Number)
		if !ok {
			errs = append(errs, fmt.Sprintf("%s: quote.%s must be a JSON number, got %s", prefix, k, v))
			continue
		}
		f, err := num.Float64()
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			errs = append(errs, fmt.Sprintf("%s: quote.%s must be a finite number, got %s", prefix, k, v))
			continue
		}
		if k == "request_expiry_ms" {
			if _, err := num.Int64(); err != nil {
				errs = append(errs, fmt.Sprintf("%s: quote.request_expiry_ms must be an integer number of milliseconds, got %s", prefix, v))
			}
		}
	}
	for _, req := range []string{"coin", "max_inventory_usd", "max_order_qty", "request_expiry_ms"} {
		if _, ok := obj[req]; !ok {
			errs = append(errs, fmt.Sprintf("%s: quote.%s is required", prefix, req))
		}
	}
	return errs
}

func quoteCoinOf(sc StrategyConfig) string {
	if sc.Quote == nil {
		return ""
	}
	return strings.ToUpper(strings.TrimSpace(sc.Quote.Coin))
}

func strategyQuoteReferences(sc StrategyConfig) []string {
	var refs []string
	if sc.Quote != nil {
		refs = append(refs, "quote block")
	}
	seen := map[string]bool{}
	for _, name := range []string{strings.TrimSpace(sc.OpenStrategy.Name), strategyNameFromArgs(sc.Args)} {
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		if _, ok := quoteCapabilityStrategies[name]; ok {
			refs = append(refs, fmt.Sprintf("quote capability open strategy %q", name))
		}
	}
	for _, ref := range sc.closeRefs() {
		name := canonicalCloseStrategyName(ref.Name)
		if researchOnlyCloseStrategies[name] {
			refs = append(refs, fmt.Sprintf("research-only close strategy %q", name))
		}
	}
	return refs
}

func quoteOwnedStrategy(sc StrategyConfig) bool {
	return len(strategyQuoteReferences(sc)) > 0
}

func quoteDispatchRefusal(sc StrategyConfig) string {
	refs := strategyQuoteReferences(sc)
	if len(refs) == 0 {
		return ""
	}
	return fmt.Sprintf("strategy %s: %s is quote-owned; scalar, batch, fallback, composer and live dispatch refuse it in every mode (only the admitted quote coordinator runs it)", sc.ID, strings.Join(refs, ", "))
}

func quoteProductionRefusalErrors(strategies []StrategyConfig) []string {
	var errs []string
	for _, sc := range strategies {
		for _, ref := range strategyQuoteReferences(sc) {
			errs = append(errs, fmt.Sprintf("strategy %s: %s is quote-owned (research-only, issue 1860): production initialization refuses it in every mode, with or without allow_no_edge, until a separate reviewed promotion", sc.ID, ref))
		}
	}
	return errs
}

func quoteOwnerConfigErrors(sc StrategyConfig) []string {
	var errs []string
	add := func(format string, args ...interface{}) {
		errs = append(errs, fmt.Sprintf("strategy %s: ", sc.ID)+fmt.Sprintf(format, args...))
	}
	q := sc.Quote
	if q == nil {
		add("a quote owner needs a quote block")
		return errs
	}
	if sc.Type != "perps" || sc.Platform != "hyperliquid" {
		add("quote ownership supports only Hyperliquid perps, got type %q platform %q", sc.Type, sc.Platform)
	}
	coin := quoteCoinOf(sc)
	if coin == "" {
		add("quote.coin is required")
	} else if sym := strings.ToUpper(strings.TrimSpace(hyperliquidSymbol(sc.Args))); sym != coin {
		add("quote.coin %q must equal the strategy symbol %q", coin, sym)
	}
	if sc.Symbol != "" && sc.Symbol != coin {
		add("symbol %q must be empty or exactly the quote coin %q", sc.Symbol, coin)
	}
	if !(q.MaxInventoryUSD > 0) || math.IsInf(q.MaxInventoryUSD, 0) {
		add("quote.max_inventory_usd must be > 0, got %g", q.MaxInventoryUSD)
	}
	if !(q.MaxOrderQty > 0) || math.IsInf(q.MaxOrderQty, 0) {
		add("quote.max_order_qty must be > 0, got %g", q.MaxOrderQty)
	}
	if q.RequestExpiryMs <= 0 || q.RequestExpiryMs > quoteMaxRequestExpiryMs {
		add("quote.request_expiry_ms must be in (0, %d], got %d", quoteMaxRequestExpiryMs, q.RequestExpiryMs)
	}
	if q.FeeReserveRate < 0 || math.IsNaN(q.FeeReserveRate) || math.IsInf(q.FeeReserveRate, 0) {
		add("quote.fee_reserve_rate must be >= 0, got %g", q.FeeReserveRate)
	}
	if q.MarginPriceBufferPct < 0 || math.IsNaN(q.MarginPriceBufferPct) || math.IsInf(q.MarginPriceBufferPct, 0) {
		add("quote.margin_price_buffer_pct must be >= 0, got %g", q.MarginPriceBufferPct)
	}
	if sc.CloseStrategy == nil || canonicalCloseStrategyName(sc.CloseStrategy.Name) != inventoryQuoteExitCloseName {
		add("a quote owner needs close_strategy %q as its sole elective close owner", inventoryQuoteExitCloseName)
	} else if len(sc.CloseStrategy.Params) > 0 {
		add("close_strategy %s takes no params; the quote policy supplies age and reducing prices through the coordinator inputs", inventoryQuoteExitCloseName)
	}
	if len(sc.closeStrategiesLegacy) > 1 {
		add("a quote owner refuses a second elective close evaluator")
	}
	pctOwners := 0
	if sc.StopLossPct != nil && *sc.StopLossPct > 0 {
		pctOwners++
	}
	if sc.StopLossMarginPct != nil && *sc.StopLossMarginPct > 0 {
		pctOwners++
	}
	refusedStop := func(field string) {
		add("stop owner %s is refused for a quote owner; use exactly one static percent stop (stop_loss_pct or stop_loss_margin_pct)", field)
	}
	if sc.StopLossATRMult != nil && *sc.StopLossATRMult > 0 {
		refusedStop("stop_loss_atr_mult")
	}
	if sc.StopLossATRMultRegime.IsConfigured() {
		refusedStop("stop_loss_atr_mult_regime")
	}
	if sc.TrailingStopATRMult != nil && *sc.TrailingStopATRMult > 0 {
		refusedStop("trailing_stop_atr_mult")
	}
	if sc.TrailingStopATRMultRegime.IsConfigured() {
		refusedStop("trailing_stop_atr_mult_regime")
	}
	if sc.TrailingStopPct != nil {
		refusedStop("trailing_stop_pct")
	}
	if strategyUsesUnifiedRegimeClose(sc) {
		refusedStop("the unified regime close")
	}
	if pctOwners != 1 {
		add("a quote owner needs exactly one static percent stop owner (stop_loss_pct or stop_loss_margin_pct), got %d; the max_drawdown_pct fallback is refused", pctOwners)
	} else if EffectiveStopLossPct(sc) <= 0 {
		add("the percent stop does not resolve through EffectiveStopLossPct")
	}
	if !strings.EqualFold(strings.TrimSpace(sc.MarginMode), "cross") && pctOwners == 1 {
		lev := EffectiveExchangeLeverage(sc)
		bound := hlBankruptcyStopBoundPct(lev)
		if pct := EffectiveStopLossPct(sc); pct >= bound {
			add("stop distance %g%% is at or beyond the isolated-margin bankruptcy distance %g%% at leverage %g", pct, bound, math.Max(lev, 1))
		}
	}
	if sc.AllowScaleIn || sc.ScaleIn != nil {
		add("scale-in is refused for a quote owner")
	}
	if HedgeEnabled(sc) || sc.Hedge != nil {
		add("hedge is refused for a quote owner")
	}
	if strings.TrimSpace(sc.BasketID) != "" {
		add("basket ownership is refused for a quote owner")
	}
	if sc.RegimeProfileAllocation != nil || sc.RegimeDirectionalPolicy != nil || sc.RegimeWindowDivergence != nil {
		add("regime close and allocation features are refused for a quote owner")
	}
	if sc.ReplaySharing != "" || sc.ReplaySourceID != "" {
		add("replay sharing is refused for a quote owner")
	}
	if sc.HurstGate != nil {
		add("the Hurst entry gate is refused for a quote owner")
	}
	return errs
}

type quoteSideRequest struct {
	Px         string `json:"px"`
	Size       string `json:"size"`
	ReduceOnly bool   `json:"reduce_only,omitempty"`
}

func (r *quoteSideRequest) same(o *quoteSideRequest) bool {
	if r == nil || o == nil {
		return r == nil && o == nil
	}
	if r.ReduceOnly != o.ReduceOnly {
		return false
	}
	a, errA := parseQuoteDecimal(r.Px)
	b, errB := parseQuoteDecimal(o.Px)
	c, errC := parseQuoteDecimal(r.Size)
	d, errD := parseQuoteDecimal(o.Size)
	if errA != nil || errB != nil || errC != nil || errD != nil {
		return false
	}
	return a.Cmp(b) == 0 && c.Cmp(d) == 0
}

type quoteDesiredPair struct {
	Bid               *quoteSideRequest `json:"bid,omitempty"`
	Ask               *quoteSideRequest `json:"ask,omitempty"`
	PolicyFingerprint string            `json:"policy_fingerprint"`
}

type quoteExitRequest struct {
	Kind        string            `json:"kind,omitempty"`
	ReducingBid *quoteSideRequest `json:"reducing_bid,omitempty"`
	ReducingAsk *quoteSideRequest `json:"reducing_ask,omitempty"`
	ExitReason  string            `json:"exit_reason,omitempty"`
}

type quoteInputVerdict struct {
	OwnerID           string   `json:"owner_id"`
	Status            string   `json:"status"`
	Reasons           []string `json:"reasons,omitempty"`
	Source            string   `json:"source"`
	SealFingerprint   string   `json:"seal_fingerprint"`
	ConfigFingerprint string   `json:"config_fingerprint"`
	PolicyFingerprint string   `json:"policy_fingerprint"`
	DeadlineMs        int64    `json:"deadline_ms"`
}

type quoteDeadlineMark struct {
	Coin       string  `json:"coin"`
	Price      float64 `json:"price"`
	EvidenceMs int64   `json:"evidence_ms"`
	MaxAgeMs   int64   `json:"max_age_ms"`
}

func quoteMarkFresh(m quoteDeadlineMark, deadlineMs int64) bool {
	if !(m.Price > 0) || math.IsInf(m.Price, 0) || m.MaxAgeMs < 0 {
		return false
	}
	return m.EvidenceMs <= deadlineMs && deadlineMs-m.EvidenceMs <= m.MaxAgeMs
}

func quoteVerdictHoldReasons(v *quoteInputVerdict, ownerID, configFP, policyFP string, deadlineMs int64) []string {
	if v == nil {
		return []string{"input verdict missing"}
	}
	var out []string
	if v.Status != quoteVerdictHealthy {
		reason := fmt.Sprintf("input verdict %q", v.Status)
		if len(v.Reasons) > 0 {
			reason += ": " + strings.Join(v.Reasons, "; ")
		}
		out = append(out, reason)
	}
	if v.OwnerID != ownerID {
		out = append(out, fmt.Sprintf("input verdict names owner %q, not %q", v.OwnerID, ownerID))
	}
	if v.DeadlineMs != deadlineMs {
		out = append(out, fmt.Sprintf("input verdict is for deadline %d, not %d", v.DeadlineMs, deadlineMs))
	}
	if v.ConfigFingerprint != configFP {
		out = append(out, "input verdict configuration fingerprint is incompatible")
	}
	if v.PolicyFingerprint != policyFP {
		out = append(out, "input verdict policy fingerprint is incompatible")
	}
	if strings.TrimSpace(v.Source) == "" {
		out = append(out, "input verdict names no source")
	}
	return out
}

type quoteCommitSummary struct {
	SettlementKind    string   `json:"settlement_kind"`
	Revision          int64    `json:"revision"`
	AppliedBoundaryMs int64    `json:"applied_boundary_ms"`
	AppliedFillKeys   []string `json:"applied_fill_keys,omitempty"`
	DeadlineMs        int64    `json:"deadline_ms"`
}

type quoteProgressRecord interface {
	WriteQuoteProgress(tx *sql.Tx, summary quoteCommitSummary) error
}

type quoteDecisionInput struct {
	Snapshot        quoteSnapshot
	SettlementHeld  bool
	SettlementHolds []string
}

type quoteDecision struct {
	Desired  quoteDesiredPair
	Exit     quoteExitRequest
	Progress quoteProgressRecord
}

type quoteDecisionFunc func(in quoteDecisionInput) (quoteDecision, error)

func quotePolicyFingerprint(sc StrategyConfig) (string, error) {
	open := effectiveOpenStrategy(sc)
	version := 0
	if spec, ok := quoteCapabilityStrategies[open]; ok {
		version = spec.Version
	}
	params := sc.OpenStrategy.Params
	if params == nil {
		params = map[string]interface{}{}
	}
	blob, err := json.Marshal(struct {
		Open    string                 `json:"open"`
		Version int                    `json:"spec_version"`
		Params  map[string]interface{} `json:"params"`
		Quote   *QuoteConfig           `json:"quote"`
		Cap     int                    `json:"capability_version"`
	}{open, version, params, sc.Quote, quoteCapabilityVersion})
	if err != nil {
		return "", err
	}
	return sha256Hex(blob), nil
}

func withoutQuoteOwned(strategies []StrategyConfig, state *AppState) []StrategyConfig {
	var out []StrategyConfig
	for _, sc := range strategies {
		if quoteOwnedStrategy(sc) {
			continue
		}
		if state != nil {
			if s := state.Strategies[sc.ID]; s != nil && s.Quote != nil {
				continue
			}
		}
		out = append(out, sc)
	}
	return out
}

func quoteReloadErrors(cfg, next *Config, state *AppState) []string {
	if cfg == nil || next == nil {
		return nil
	}
	cur := strategyConfigByID(cfg.Strategies)
	nxt := strategyConfigByID(next.Strategies)
	ids := map[string]bool{}
	for _, sc := range cfg.Strategies {
		if quoteOwnedStrategy(sc) {
			ids[sc.ID] = true
		}
	}
	for _, sc := range next.Strategies {
		if quoteOwnedStrategy(sc) {
			ids[sc.ID] = true
		}
	}
	if state != nil {
		for id, s := range state.Strategies {
			if s != nil && s.Quote != nil {
				ids[id] = true
			}
		}
	}
	sorted := make([]string, 0, len(ids))
	for id := range ids {
		sorted = append(sorted, id)
	}
	sort.Strings(sorted)
	var errs []string
	for _, id := range sorted {
		s := stateStrategy(state, id)
		busy := strategyHasOpenPositions(s) || (s != nil && s.Quote.busy())
		if !busy {
			continue
		}
		a, aok := cur[id]
		b, bok := nxt[id]
		var changed []string
		switch {
		case !aok || !bok:
			changed = append(changed, "quote ownership (strategy added or removed)")
		default:
			if !reflect.DeepEqual(a.Quote, b.Quote) {
				changed = append(changed, "quote block")
			}
			pa, _ := quotePolicyFingerprint(a)
			pb, _ := quotePolicyFingerprint(b)
			if pa != pb {
				changed = append(changed, "quote policy")
			}
			if a.IntervalSeconds != b.IntervalSeconds || cfg.IntervalSeconds != next.IntervalSeconds {
				changed = append(changed, "cadence")
			}
			if !reflect.DeepEqual(a.CloseStrategy, b.CloseStrategy) {
				changed = append(changed, "close owner")
			}
			if !reflect.DeepEqual(a.StopLossPct, b.StopLossPct) || !reflect.DeepEqual(a.StopLossMarginPct, b.StopLossMarginPct) || a.Leverage != b.Leverage || a.MarginMode != b.MarginMode {
				changed = append(changed, "stop owner")
			}
			if effectiveStorageStrategyID(a) != effectiveStorageStrategyID(b) || a.PaperSource != b.PaperSource || cfg.DBFile != next.DBFile || cfg.PaperDBFile != next.PaperDBFile {
				changed = append(changed, "storage")
			}
		}
		if len(changed) > 0 {
			errs = append(errs, fmt.Sprintf("strategy[%s] quote %s changed while quotes, unresolved intents or inventory exist (restart after the owner is flat and resolved)", id, strings.Join(changed, ", ")))
		}
	}
	return errs
}

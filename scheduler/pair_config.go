package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const (
	pairMemberStrategyToken = "pair_member"
	pairMemberFlag          = "--pair-member-json"
	okxCheckScriptPath      = "shared_scripts/check_okx.py"

	pairContractVersion = 2

	basketGroupMode = "basket"
	pairGroupMode   = "pair"

	pairManagementOnlyMode = "management_only"

	pairSyntheticAccountPrefix = "synthetic:"

	pairMaxIntervalMs int64 = 2_147_483_647

	pairMaxEnvelopeBytes   = 4 << 20
	pairMaxProtectiveWidth = 10000.0

	pairResearchManifestName  = "pair-research.json"
	pairResearchManifestOwner = "go-trader pair research run"
)

const pairPolicyCrossExchangePerpArbitrage = "cross_exchange_perp_arbitrage"

var pairPolicyStrategies = map[string]bool{pairPolicyCrossExchangePerpArbitrage: true}

var pairPolicyParamKeys = map[string][]string{
	pairPolicyCrossExchangePerpArbitrage: {"adverse_band_bps", "entry_threshold_bps", "exit_threshold_bps", "funding_allowance_bps"},
}

var pairPolicyParamValidators = map[string]func(path string, params map[string]json.RawMessage) []string{
	pairPolicyCrossExchangePerpArbitrage: pairCrossExchangeParamErrors,
}

const pairMaxThresholdBps = 10000.0

func pairCrossExchangeParamErrors(path string, params map[string]json.RawMessage) []string {
	var errs []string
	add := func(format string, a ...interface{}) {
		errs = append(errs, path+": "+fmt.Sprintf(format, a...))
	}
	keys := pairPolicyParamKeys[pairPolicyCrossExchangePerpArbitrage]
	known := make(map[string]bool, len(keys))
	vals := map[string]float64{}
	for _, k := range keys {
		known[k] = true
		v, ok := params[k]
		if !ok {
			add("%s is missing; policy %s has no parameter defaults", k, pairPolicyCrossExchangePerpArbitrage)
			continue
		}
		text := strings.TrimSpace(string(v))
		if kind := pairRawJSONKind(v); kind != "number" {
			add("%s must be a finite JSON number, got %s %s", k, kind, text)
			continue
		}
		var f float64
		if err := json.Unmarshal(v, &f); err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			add("%s must be a finite number, got %s", k, text)
			continue
		}
		vals[k] = f
	}
	var unknown []string
	for k := range params {
		if !known[k] {
			unknown = append(unknown, k)
		}
	}
	sort.Strings(unknown)
	for _, k := range unknown {
		add("unknown field %q in params for policy %s", k, pairPolicyCrossExchangePerpArbitrage)
	}
	entry, entryOK := vals["entry_threshold_bps"]
	exit, exitOK := vals["exit_threshold_bps"]
	if exitOK && exit < 0 {
		add("exit_threshold_bps %v must be at least 0", exit)
	}
	if entryOK && entry > pairMaxThresholdBps {
		add("entry_threshold_bps %v must not exceed %v", entry, pairMaxThresholdBps)
	}
	if entryOK && exitOK && !(exit < entry) {
		add("exit_threshold_bps %v must be below entry_threshold_bps %v", exit, entry)
	}
	if v, ok := vals["adverse_band_bps"]; ok && !(v >= 0 && v < pairMaxThresholdBps) {
		add("adverse_band_bps %v must be at least 0 and below %v", v, pairMaxThresholdBps)
	}
	if v, ok := vals["funding_allowance_bps"]; ok && v < 0 {
		add("funding_allowance_bps %v must be at least 0", v)
	}
	return errs
}

func pairRawPolicyName(fields map[string]json.RawMessage) (string, bool) {
	v, ok := fields["policy"]
	if !ok || pairRawJSONKind(v) != "string" {
		return "", false
	}
	var name string
	if err := json.Unmarshal(v, &name); err != nil {
		return "", false
	}
	return strings.TrimSpace(name), true
}

func pairRawParamErrors(fields map[string]json.RawMessage) []string {
	name, ok := pairRawPolicyName(fields)
	if !ok {
		return nil
	}
	validate := pairPolicyParamValidators[name]
	if validate == nil {
		return nil
	}
	raw, present := fields["params"]
	if !present {
		return validate("pairs[0].params", map[string]json.RawMessage{})
	}
	if pairRawJSONKind(raw) != "object" {
		return nil
	}
	var params map[string]json.RawMessage
	if err := json.Unmarshal(raw, &params); err != nil {
		return []string{fmt.Sprintf("pairs[0].params: is unreadable: %v", err)}
	}
	return validate("pairs[0].params", params)
}

var pairProviderPlatforms = map[string]string{
	"hyperliquid": entryProviderHyperliquid,
	"okx":         entryProviderOKX,
}

type pairInstrumentMeta struct {
	NativeUnit            string   `json:"native_unit"`
	BaseAsset             string   `json:"base_asset"`
	ContractSize          float64  `json:"contract_size"`
	QuoteCurrency         string   `json:"quote_currency"`
	SettlementCurrency    string   `json:"settlement_currency"`
	TickStep              float64  `json:"tick_step"`
	LotStep               float64  `json:"lot_step"`
	MinQty                float64  `json:"min_qty"`
	MinNotional           float64  `json:"min_notional"`
	TradingStatus         string   `json:"trading_status"`
	Linear                bool     `json:"linear"`
	PositionMode          string   `json:"position_mode"`
	MarginMode            string   `json:"margin_mode"`
	MaxLeverage           float64  `json:"max_leverage"`
	InitialMarginRate     float64  `json:"initial_margin_rate"`
	MaintenanceMarginRate float64  `json:"maintenance_margin_rate"`
	PriceCurrency         string   `json:"price_currency"`
	PriceRule             string   `json:"price_rule"`
	SzDecimals            int      `json:"sz_decimals"`
	TickStepText          string   `json:"tick_step_text"`
	LotStepText           string   `json:"lot_step_text"`
	FeeCurrencies         []string `json:"fee_currencies"`
}

type pairBookNeed struct {
	Depth    int   `json:"depth"`
	WindowMs int64 `json:"window_ms"`
	Capacity int   `json:"capacity"`
}

type pairOperationalLimits struct {
	GrossBudgetUSD            float64 `json:"gross_budget_usd"`
	HyperliquidNotionalCapUSD float64 `json:"hyperliquid_notional_cap_usd"`
	OKXNotionalCapUSD         float64 `json:"okx_notional_cap_usd"`
	UnmatchedCapUSD           float64 `json:"unmatched_cap_usd"`
	DecisionCadenceMs         int64   `json:"decision_cadence_ms"`
	AcceptanceDeadlineMs      int64   `json:"acceptance_deadline_ms"`
	RecoveryAgeMs             int64   `json:"recovery_age_ms"`
	MaxHoldAgeMs              int64   `json:"max_hold_age_ms"`
	RetryInitialMs            int64   `json:"retry_initial_ms"`
	RetryMaxMs                int64   `json:"retry_max_ms"`
	QuoteAgeMs                int64   `json:"quote_age_ms"`
	CollectionTimeoutMs       int64   `json:"collection_timeout_ms"`
	ReceiveSkewMs             int64   `json:"receive_skew_ms"`
	EnvelopeMaxBytes          int     `json:"envelope_max_bytes"`
	ProtectiveWidthBps        float64 `json:"protective_width_bps"`
}

func (l pairOperationalLimits) venueCapUSD(provider string) float64 {
	switch provider {
	case entryProviderHyperliquid:
		return l.HyperliquidNotionalCapUSD
	case entryProviderOKX:
		return l.OKXNotionalCapUSD
	}
	return 0
}

var pairLimitDollarFields = []string{"gross_budget_usd", "hyperliquid_notional_cap_usd", "okx_notional_cap_usd", "unmatched_cap_usd"}

var pairLimitTimingFields = []string{"decision_cadence_ms", "acceptance_deadline_ms", "recovery_age_ms", "max_hold_age_ms", "retry_initial_ms", "retry_max_ms", "quote_age_ms", "collection_timeout_ms", "receive_skew_ms"}

type PairMemberConfig struct {
	StrategyID        string             `json:"strategy_id"`
	AccountKey        string             `json:"account_key"`
	Instrument        string             `json:"instrument"`
	Meta              pairInstrumentMeta `json:"meta"`
	Source            string             `json:"source"`
	ModeledDeliveryMs int64              `json:"modeled_delivery_ms"`
	CostPolicy        fillCostPolicy     `json:"cost_policy"`
}

type PairConfig struct {
	ID         string                 `json:"id"`
	Underlying string                 `json:"underlying"`
	Policy     string                 `json:"policy"`
	Params     map[string]interface{} `json:"params,omitempty"`
	Members    []PairMemberConfig     `json:"members"`
	Limits     json.RawMessage        `json:"limits"`
	BookNeed   pairBookNeed           `json:"book_need"`
}

type pairContractMember struct {
	MemberID          string             `json:"member_id"`
	Platform          string             `json:"platform"`
	Type              string             `json:"type"`
	Provider          string             `json:"provider"`
	AccountKey        string             `json:"account_key"`
	Instrument        string             `json:"instrument"`
	Symbol            string             `json:"symbol"`
	Timeframe         string             `json:"timeframe"`
	CloseStrategy     string             `json:"close_strategy"`
	Meta              pairInstrumentMeta `json:"meta"`
	Source            string             `json:"source"`
	ModeledDeliveryMs int64              `json:"modeled_delivery_ms"`
	CostPolicy        fillCostPolicy     `json:"cost_policy"`
	Leverage          float64            `json:"leverage"`
}

type pairContractBody struct {
	Underlying string                 `json:"underlying"`
	Policy     string                 `json:"policy"`
	Params     map[string]interface{} `json:"params,omitempty"`
	Limits     pairOperationalLimits  `json:"limits"`
	BookNeed   pairBookNeed           `json:"book_need"`
	Members    []pairContractMember   `json:"members"`
}

type pairMemberKey struct {
	Partition  RiskPartition
	Provider   string
	AccountKey string
	Instrument string
	MemberID   string
}

func (k pairMemberKey) String() string {
	return strings.Join([]string{k.Partition.String(), k.Provider, k.AccountKey, k.Instrument, k.MemberID}, "|")
}

func (c basketContract) isPair() bool {
	return c.Mode == pairGroupMode && c.Pair != nil
}

func (c basketContract) pairMember(id string) (pairContractMember, bool) {
	if c.Pair == nil {
		return pairContractMember{}, false
	}
	for _, m := range c.Pair.Members {
		if m.MemberID == id {
			return m, true
		}
	}
	return pairContractMember{}, false
}

func (c basketContract) pairMemberKey(m pairContractMember) pairMemberKey {
	p, _ := parseRiskPartition(c.Partition)
	return pairMemberKey{Partition: p, Provider: m.Provider, AccountKey: m.AccountKey, Instrument: m.Instrument, MemberID: m.MemberID}
}

func (c basketContract) pairMembersSorted() []pairContractMember {
	if c.Pair == nil {
		return nil
	}
	out := append([]pairContractMember(nil), c.Pair.Members...)
	sort.Slice(out, func(i, j int) bool { return c.pairMemberKey(out[i]).String() < c.pairMemberKey(out[j]).String() })
	return out
}

func isPairMember(sc StrategyConfig) bool {
	return strings.TrimSpace(sc.PairID) != ""
}

func pairAddMs(a, b int64) (int64, bool) {
	if b > 0 && a > math.MaxInt64-b {
		return 0, false
	}
	if b < 0 && a < math.MinInt64-b {
		return 0, false
	}
	return a + b, true
}

func pairOrdinaryRefusals(raw []byte) []string {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil
	}
	var errs []string
	if _, ok := root["pairs"]; ok {
		errs = append(errs, "config root: \"pairs\" is a research-only pair contract; no ordinary load, read-only load, probe, inspection, reload or operator route activates it in any mode, paper with acknowledgement included")
	}
	rawStrategies, ok := root["strategies"]
	if !ok {
		return errs
	}
	var strategies []map[string]json.RawMessage
	if err := json.Unmarshal(rawStrategies, &strategies); err != nil {
		return errs
	}
	for i, s := range strategies {
		prefix := fmt.Sprintf("strategy[%d]", i)
		if idRaw, ok := s["id"]; ok {
			var id string
			if json.Unmarshal(idRaw, &id) == nil && id != "" {
				prefix = fmt.Sprintf("strategy[%s]", id)
			}
		}
		if _, ok := s["pair_id"]; ok {
			errs = append(errs, prefix+": \"pair_id\" names a research-only pair member; ordinary routes refuse it in every mode")
		}
		var args []string
		if rawArgs, ok := s["args"]; ok && json.Unmarshal(rawArgs, &args) == nil {
			if len(args) > 0 && strings.TrimSpace(args[0]) == pairMemberStrategyToken {
				errs = append(errs, fmt.Sprintf("%s: %q is reserved for research pair members; ordinary routes refuse it in every mode", prefix, pairMemberStrategyToken))
			}
			for _, a := range args {
				if a == pairMemberFlag || strings.HasPrefix(a, pairMemberFlag+"=") {
					errs = append(errs, fmt.Sprintf("%s: %s is reserved for research pair members; ordinary routes refuse it in every mode", prefix, pairMemberFlag))
					break
				}
			}
		}
		if rawOpen, ok := s["open_strategy"]; ok {
			var ref struct {
				Name string `json:"name"`
			}
			var name string
			if json.Unmarshal(rawOpen, &ref) == nil {
				name = ref.Name
			} else if json.Unmarshal(rawOpen, &name) != nil {
				name = ""
			}
			if strings.TrimSpace(name) == pairMemberStrategyToken {
				errs = append(errs, fmt.Sprintf("%s: open_strategy %q is reserved for research pair members; ordinary routes refuse it in every mode", prefix, pairMemberStrategyToken))
			}
		}
	}
	return errs
}

func pairRawJSONKind(raw json.RawMessage) string {
	text := strings.TrimSpace(string(raw))
	switch {
	case text == "":
		return "missing"
	case text == "null":
		return "null"
	case text == "true" || text == "false":
		return "boolean"
	case strings.HasPrefix(text, "\""):
		return "string"
	case strings.HasPrefix(text, "{"):
		return "object"
	case strings.HasPrefix(text, "["):
		return "array"
	}
	return "number"
}

func pairOperationalLimitErrors(raw json.RawMessage) (pairOperationalLimits, []string) {
	var out pairOperationalLimits
	var errs []string
	add := func(format string, a ...interface{}) {
		errs = append(errs, "limits: "+fmt.Sprintf(format, a...))
	}
	if kind := pairRawJSONKind(raw); kind != "object" {
		add("must be a JSON object, got %s", kind)
		return out, errs
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		add("is unreadable: %v", err)
		return out, errs
	}
	known := knownJSONKeys(reflect.TypeOf(pairOperationalLimits{}))
	var unknown []string
	for k := range fields {
		if !known[k] {
			unknown = append(unknown, k)
		}
	}
	sort.Strings(unknown)
	for _, k := range unknown {
		add("unknown field %q", k)
	}
	dollars := map[string]*float64{
		"gross_budget_usd":             &out.GrossBudgetUSD,
		"hyperliquid_notional_cap_usd": &out.HyperliquidNotionalCapUSD,
		"okx_notional_cap_usd":         &out.OKXNotionalCapUSD,
		"unmatched_cap_usd":            &out.UnmatchedCapUSD,
	}
	dollarOK := map[string]bool{}
	for _, name := range pairLimitDollarFields {
		v, ok := fields[name]
		if !ok {
			add("%s is missing", name)
			continue
		}
		if kind := pairRawJSONKind(v); kind != "number" {
			add("%s must be a JSON number, got %s %s", name, kind, strings.TrimSpace(string(v)))
			continue
		}
		var f float64
		if err := json.Unmarshal(v, &f); err != nil {
			add("%s %s is not a finite number", name, strings.TrimSpace(string(v)))
			continue
		}
		if !basketFinitePositive(f) {
			add("%s must be a finite number above zero, got %v", name, f)
			continue
		}
		*dollars[name] = f
		dollarOK[name] = true
	}
	if dollarOK["gross_budget_usd"] {
		for _, name := range []string{"hyperliquid_notional_cap_usd", "okx_notional_cap_usd", "unmatched_cap_usd"} {
			if dollarOK[name] && *dollars[name] > out.GrossBudgetUSD {
				add("%s %v exceeds gross_budget_usd %v", name, *dollars[name], out.GrossBudgetUSD)
			}
		}
	}
	timings := map[string]*int64{
		"decision_cadence_ms":    &out.DecisionCadenceMs,
		"acceptance_deadline_ms": &out.AcceptanceDeadlineMs,
		"recovery_age_ms":        &out.RecoveryAgeMs,
		"max_hold_age_ms":        &out.MaxHoldAgeMs,
		"retry_initial_ms":       &out.RetryInitialMs,
		"retry_max_ms":           &out.RetryMaxMs,
		"quote_age_ms":           &out.QuoteAgeMs,
		"collection_timeout_ms":  &out.CollectionTimeoutMs,
		"receive_skew_ms":        &out.ReceiveSkewMs,
	}
	timingOK := map[string]bool{}
	for _, name := range pairLimitTimingFields {
		v, ok := fields[name]
		if !ok {
			add("%s is missing", name)
			continue
		}
		if kind := pairRawJSONKind(v); kind != "number" {
			add("%s must be an integer number of milliseconds, got %s %s", name, kind, strings.TrimSpace(string(v)))
			continue
		}
		i, ok := pairRawInteger(v)
		if !ok {
			add("%s must be an integer number of milliseconds in [1, %d], got %s", name, pairMaxIntervalMs, strings.TrimSpace(string(v)))
			continue
		}
		if i < 1 || i > pairMaxIntervalMs {
			add("%s must be in [1, %d], got %d", name, pairMaxIntervalMs, i)
			continue
		}
		*timings[name] = i
		timingOK[name] = true
	}
	if timingOK["retry_initial_ms"] && timingOK["retry_max_ms"] && out.RetryInitialMs > out.RetryMaxMs {
		add("retry_initial_ms %d exceeds retry_max_ms %d", out.RetryInitialMs, out.RetryMaxMs)
	}
	if timingOK["recovery_age_ms"] && timingOK["acceptance_deadline_ms"] {
		grace := entryRecoveryGrace.Milliseconds()
		need, ok := pairAddMs(out.AcceptanceDeadlineMs, grace)
		if !ok || out.RecoveryAgeMs < need {
			add("recovery_age_ms %d is below acceptance_deadline_ms %d plus the %d ms absence grace", out.RecoveryAgeMs, out.AcceptanceDeadlineMs, grace)
		}
	}
	for _, name := range []string{"collection_timeout_ms", "receive_skew_ms"} {
		if timingOK[name] && timingOK["quote_age_ms"] && *timings[name] > out.QuoteAgeMs {
			add("%s %d exceeds quote_age_ms %d", name, *timings[name], out.QuoteAgeMs)
		}
	}
	if v, ok := fields["envelope_max_bytes"]; !ok {
		add("envelope_max_bytes is missing")
	} else if i, isInt := pairRawInteger(v); pairRawJSONKind(v) != "number" || !isInt {
		add("envelope_max_bytes must be an integer number of bytes in [1, %d], got %s %s", pairMaxEnvelopeBytes, pairRawJSONKind(v), strings.TrimSpace(string(v)))
	} else if i < 1 || i > pairMaxEnvelopeBytes {
		add("envelope_max_bytes must be in [1, %d], got %d", pairMaxEnvelopeBytes, i)
	} else {
		out.EnvelopeMaxBytes = int(i)
	}
	if v, ok := fields["protective_width_bps"]; !ok {
		add("protective_width_bps is missing")
	} else if kind := pairRawJSONKind(v); kind != "number" {
		add("protective_width_bps must be a JSON number, got %s %s", kind, strings.TrimSpace(string(v)))
	} else {
		var f float64
		if err := json.Unmarshal(v, &f); err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 || f >= pairMaxProtectiveWidth {
			add("protective_width_bps must be finite, above 0 and below %v, got %s", pairMaxProtectiveWidth, strings.TrimSpace(string(v)))
		} else {
			out.ProtectiveWidthBps = f
		}
	}
	return out, errs
}

func pairRawInteger(v json.RawMessage) (int64, bool) {
	text := strings.TrimSpace(string(v))
	if !pairIntegerPattern.MatchString(text) {
		return 0, false
	}
	i, err := strconv.ParseInt(text, 10, 64)
	return i, err == nil
}

type pairRawField struct {
	name     string
	kind     string
	optional bool
}

var pairRawPairFields = []pairRawField{
	{"id", "string", false}, {"underlying", "string", false}, {"policy", "string", false}, {"params", "object", true},
	{"members", "array", false}, {"limits", "object", false}, {"book_need", "object", false},
}

var pairRawMemberFields = []pairRawField{
	{"strategy_id", "string", false}, {"account_key", "string", false}, {"instrument", "string", false}, {"meta", "object", false},
	{"source", "string", false}, {"modeled_delivery_ms", "integer", false}, {"cost_policy", "object", false},
}

var pairRawMetaFields = []pairRawField{
	{"native_unit", "string", false}, {"base_asset", "string", false}, {"contract_size", "number", false},
	{"quote_currency", "string", false}, {"settlement_currency", "string", false}, {"price_currency", "string", false},
	{"tick_step", "number", false}, {"lot_step", "number", false}, {"min_qty", "number", false}, {"min_notional", "number", false},
	{"trading_status", "string", false}, {"linear", "boolean", false}, {"position_mode", "string", false}, {"margin_mode", "string", false},
	{"max_leverage", "number", false}, {"initial_margin_rate", "number", false}, {"maintenance_margin_rate", "number", false},
	{"price_rule", "string", false}, {"sz_decimals", "integer", false}, {"tick_step_text", "string", false}, {"lot_step_text", "string", false},
	{"fee_currencies", "string_array", false},
}

var pairRawCostPolicyFields = []pairRawField{
	{"taker_fee_pct", "number", false}, {"slippage_pct", "number", false}, {"fill_role", "string", false}, {"spread", "string", false},
}

var pairRawBookNeedFields = []pairRawField{
	{"depth", "integer", false}, {"window_ms", "integer", false}, {"capacity", "integer", false},
}

func pairRawObjectErrors(path string, raw json.RawMessage, schema []pairRawField) (map[string]json.RawMessage, []string) {
	var errs []string
	if kind := pairRawJSONKind(raw); kind != "object" {
		return nil, []string{fmt.Sprintf("%s: must be a JSON object, got %s", path, kind)}
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, []string{fmt.Sprintf("%s: is unreadable: %v", path, err)}
	}
	known := map[string]bool{}
	for _, f := range schema {
		known[f.name] = true
		v, ok := fields[f.name]
		if !ok {
			if !f.optional {
				errs = append(errs, fmt.Sprintf("%s: %s is missing", path, f.name))
			}
			continue
		}
		if msg := pairRawKindError(v, f.kind); msg != "" {
			errs = append(errs, fmt.Sprintf("%s: %s %s", path, f.name, msg))
		}
	}
	var unknown []string
	for k := range fields {
		if !known[k] {
			unknown = append(unknown, k)
		}
	}
	sort.Strings(unknown)
	for _, k := range unknown {
		errs = append(errs, fmt.Sprintf("%s: unknown field %q", path, k))
	}
	return fields, errs
}

func pairRawKindError(v json.RawMessage, want string) string {
	kind := pairRawJSONKind(v)
	text := strings.TrimSpace(string(v))
	switch want {
	case "integer":
		if kind != "number" {
			return fmt.Sprintf("must be a JSON integer, got %s %s", kind, text)
		}
		if _, ok := pairRawInteger(v); !ok {
			return fmt.Sprintf("must be a JSON integer (no fraction or exponent), got %s", text)
		}
	case "number":
		if kind != "number" {
			return fmt.Sprintf("must be a JSON number, got %s %s", kind, text)
		}
		var f float64
		if err := json.Unmarshal(v, &f); err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return fmt.Sprintf("must be a finite number, got %s", text)
		}
	case "string_array":
		if kind != "array" {
			return fmt.Sprintf("must be a JSON array of strings, got %s %s", kind, text)
		}
		var items []json.RawMessage
		if err := json.Unmarshal(v, &items); err != nil {
			return fmt.Sprintf("is unreadable: %v", err)
		}
		for i, it := range items {
			if k := pairRawJSONKind(it); k != "string" {
				return fmt.Sprintf("[%d] must be a JSON string, got %s", i, k)
			}
		}
	default:
		if kind != want {
			return fmt.Sprintf("must be a JSON %s, got %s %s", want, kind, text)
		}
	}
	return ""
}

func pairRawPairErrors(raw json.RawMessage) []string {
	fields, errs := pairRawObjectErrors("pairs[0]", raw, pairRawPairFields)
	if fields == nil {
		return errs
	}
	errs = append(errs, pairRawParamErrors(fields)...)
	if v, ok := fields["book_need"]; ok && pairRawJSONKind(v) == "object" {
		_, e := pairRawObjectErrors("pairs[0].book_need", v, pairRawBookNeedFields)
		errs = append(errs, e...)
	}
	if v, ok := fields["members"]; ok && pairRawJSONKind(v) == "array" {
		var members []json.RawMessage
		if err := json.Unmarshal(v, &members); err != nil {
			return append(errs, fmt.Sprintf("pairs[0].members: is unreadable: %v", err))
		}
		for i, m := range members {
			mp := fmt.Sprintf("pairs[0].members[%d]", i)
			mf, e := pairRawObjectErrors(mp, m, pairRawMemberFields)
			errs = append(errs, e...)
			if mf == nil {
				continue
			}
			if mv, ok := mf["meta"]; ok && pairRawJSONKind(mv) == "object" {
				_, e := pairRawObjectErrors(mp+".meta", mv, pairRawMetaFields)
				errs = append(errs, e...)
			}
			if cv, ok := mf["cost_policy"]; ok && pairRawJSONKind(cv) == "object" {
				_, e := pairRawObjectErrors(mp+".cost_policy", cv, pairRawCostPolicyFields)
				errs = append(errs, e...)
			}
		}
	}
	return errs
}

func pairInstrumentMetaErrors(prefix, provider, underlying string, m PairMemberConfig, sc StrategyConfig) []string {
	var errs []string
	add := func(format string, a ...interface{}) {
		errs = append(errs, prefix+": "+fmt.Sprintf(format, a...))
	}
	meta := m.Meta
	for _, f := range []struct {
		name string
		v    float64
	}{{"meta.contract_size", meta.ContractSize}, {"meta.tick_step", meta.TickStep}, {"meta.lot_step", meta.LotStep}} {
		if !basketFinitePositive(f.v) {
			add("%s must be a finite number above zero, got %v", f.name, f.v)
		}
	}
	if math.IsNaN(meta.MinQty) || math.IsInf(meta.MinQty, 0) || meta.MinQty < 0 {
		add("meta.min_qty must be finite and not negative, got %v", meta.MinQty)
	}
	if math.IsNaN(meta.MinNotional) || math.IsInf(meta.MinNotional, 0) || meta.MinNotional < 0 {
		add("meta.min_notional must be finite and not negative, got %v", meta.MinNotional)
	}
	if !meta.Linear {
		add("meta.linear must be true: only linear perpetuals are supported")
	}
	if meta.TradingStatus != "live" {
		add("meta.trading_status must be \"live\" (an active instrument), got %q", meta.TradingStatus)
	}
	if strings.TrimSpace(meta.QuoteCurrency) == "" || strings.TrimSpace(meta.SettlementCurrency) == "" || strings.TrimSpace(meta.PriceCurrency) == "" {
		add("meta.quote_currency, meta.settlement_currency and meta.price_currency must be set")
	}
	if len(meta.FeeCurrencies) == 0 {
		add("meta.fee_currencies must list at least one currency")
	}
	seenFee := map[string]bool{}
	for _, c := range meta.FeeCurrencies {
		if !pairCurrencyPattern.MatchString(c) {
			add("meta.fee_currencies entry %q must be an upper-case currency code", c)
		}
		if seenFee[c] {
			add("meta.fee_currencies lists %q twice", c)
		}
		seenFee[c] = true
	}
	if want := pairPriceRules[provider]; meta.PriceRule != want {
		add("meta.price_rule must be %q for a %s member, got %q", want, provider, meta.PriceRule)
	}
	if !strings.EqualFold(strings.TrimSpace(meta.BaseAsset), underlying) {
		add("meta.base_asset %q must equal the pair underlying %q", meta.BaseAsset, underlying)
	}
	if !basketFinitePositive(meta.MaxLeverage) {
		add("meta.max_leverage must be a finite number above zero, got %v", meta.MaxLeverage)
	} else if lev := EffectiveExchangeLeverage(sc); lev > meta.MaxLeverage {
		add("leverage %v exceeds meta.max_leverage %v", lev, meta.MaxLeverage)
	}
	if !(meta.InitialMarginRate > 0 && meta.InitialMarginRate <= 1) {
		add("meta.initial_margin_rate must be in (0, 1], got %v", meta.InitialMarginRate)
	}
	if !(meta.MaintenanceMarginRate > 0 && meta.MaintenanceMarginRate < meta.InitialMarginRate) {
		add("meta.maintenance_margin_rate must be above zero and below meta.initial_margin_rate, got %v", meta.MaintenanceMarginRate)
	}
	switch provider {
	case entryProviderHyperliquid:
		if meta.NativeUnit != "base" {
			add("a Hyperliquid perpetual trades base units: meta.native_unit must be \"base\", got %q", meta.NativeUnit)
		}
		if meta.ContractSize != 1 {
			add("a Hyperliquid perpetual has contract size 1, got %v", meta.ContractSize)
		}
		if meta.PositionMode != "one_way" {
			add("a Hyperliquid account holds one net position per coin: meta.position_mode must be \"one_way\", got %q", meta.PositionMode)
		}
		if m.Instrument != underlying {
			add("instrument %q must be the bare Hyperliquid coin %q", m.Instrument, underlying)
		}
		if meta.SzDecimals < 0 || meta.SzDecimals > hlLotMetadataMaxSzDecimals {
			add("meta.sz_decimals must be an integer in [0, %d], got %d", hlLotMetadataMaxSzDecimals, meta.SzDecimals)
		}
		if meta.TickStepText != "" || meta.LotStepText != "" {
			add("meta.tick_step_text and meta.lot_step_text are OKX-only and must be empty for a Hyperliquid member (prices round through verified sz_decimals)")
		}
		for _, c := range [][2]string{{"meta.quote_currency", meta.QuoteCurrency}, {"meta.settlement_currency", meta.SettlementCurrency}, {"meta.price_currency", meta.PriceCurrency}} {
			if c[1] != pairHyperliquidCurrency {
				add("%s must be %q: a Hyperliquid perpetual is quoted, priced and settled in USDC, got %q", c[0], pairHyperliquidCurrency, c[1])
			}
		}
		if meta.MarginMode != "cross" && meta.MarginMode != "isolated" {
			add("meta.margin_mode must be \"cross\" or \"isolated\" for a Hyperliquid member, got %q", meta.MarginMode)
		}
	case entryProviderOKX:
		if meta.NativeUnit != "contract" {
			add("an OKX swap trades contracts: meta.native_unit must be \"contract\", got %q", meta.NativeUnit)
		}
		if meta.PositionMode != "net" {
			add("the OKX account must be in verified net position mode: meta.position_mode must be \"net\", got %q", meta.PositionMode)
		}
		if meta.MarginMode != "cross" {
			add("initial OKX support uses the existing cross-margin transport: meta.margin_mode must be \"cross\", got %q", meta.MarginMode)
		}
		for _, c := range [][2]string{{"meta.quote_currency", meta.QuoteCurrency}, {"meta.settlement_currency", meta.SettlementCurrency}, {"meta.price_currency", meta.PriceCurrency}} {
			if c[1] != pairOKXCurrency {
				add("%s must be %q: the OKX order, close, query, candle, funding and balance routes all select USDT swaps, got %q", c[0], pairOKXCurrency, c[1])
			}
		}
		want := underlying + "-" + pairOKXCurrency + "-SWAP"
		if m.Instrument != want {
			add("instrument %q must be the OKX linear swap %q (the USDT swap)", m.Instrument, want)
		}
		if meta.SzDecimals != 0 {
			add("meta.sz_decimals must be explicit 0 for an OKX member (the fixed tick rule uses tick_step_text), got %d", meta.SzDecimals)
		}
		for _, f := range []struct {
			name, text string
			v          float64
		}{{"tick_step", meta.TickStepText, meta.TickStep}, {"lot_step", meta.LotStepText, meta.LotStep}} {
			text, err := pairParseDecimalInput(f.text)
			if !pairPlainDecimalPattern.MatchString(f.text) || err != nil || text.Sign() <= 0 {
				add("meta.%s_text must be a positive plain decimal string, got %q", f.name, f.text)
				continue
			}
			num, err := quoteRatFromFloatDecimal(f.v)
			if err != nil || num.Cmp(text) != 0 {
				add("meta.%s_text %q differs from meta.%s %v as an exact decimal", f.name, f.text, f.name, f.v)
			}
		}
	}
	return errs
}

func pairMemberContractErrors(prefix, provider string, m PairMemberConfig) []string {
	var errs []string
	add := func(format string, a ...interface{}) {
		errs = append(errs, prefix+": "+fmt.Sprintf(format, a...))
	}
	allowed := pairMemberSources[provider]
	okSource := false
	for _, s := range allowed {
		if m.Source == s {
			okSource = true
		}
	}
	if !okSource {
		add("source %q is not supported for a %s member (allowed: %s)", m.Source, provider, strings.Join(allowed, ", "))
	}
	if m.Source == pairSourceArchive {
		if m.ModeledDeliveryMs < 1 || m.ModeledDeliveryMs > pairMaxIntervalMs {
			add("modeled_delivery_ms must be in [1, %d] for the archive source, got %d", pairMaxIntervalMs, m.ModeledDeliveryMs)
		}
	} else if m.ModeledDeliveryMs != 0 {
		add("modeled_delivery_ms must be explicit 0 for the %q source, got %d", m.Source, m.ModeledDeliveryMs)
	}
	if err := validateFillCostRates(m.CostPolicy); err != nil {
		add("cost_policy: %v", err)
	}
	return errs
}

func pairMemberFieldErrors(sc StrategyConfig, p PairConfig, liveShapedFixture bool) []string {
	prefix := fmt.Sprintf("strategy[%s]: pair member of %q", sc.ID, p.ID)
	var errs []string
	add := func(format string, a ...interface{}) {
		errs = append(errs, prefix+" "+fmt.Sprintf(format, a...))
	}
	switch {
	case sc.Platform == "hyperliquid" && sc.Type == "perps":
		if filepath.Clean(strings.TrimSpace(sc.Script)) != hyperliquidCheckScript {
			add("must use the built-in %s check, got script %q", hyperliquidCheckScript, sc.Script)
		}
	case sc.Platform == "okx" && sc.Type == "perps":
		if filepath.Clean(strings.TrimSpace(sc.Script)) != okxCheckScriptPath {
			add("must use the built-in %s check, got script %q", okxCheckScriptPath, sc.Script)
		}
		if okxInstType(sc.Args) != "swap" {
			add("must trade the OKX swap instrument type, got %q", okxInstType(sc.Args))
		}
	default:
		add("must be platform hyperliquid type perps or platform okx type perps (got platform=%s type=%s)", sc.Platform, sc.Type)
	}
	if strategyNameFromArgs(sc.Args) != pairMemberStrategyToken {
		add("must name the %q positional strategy (args[0]); a member has no independent entry evaluator", pairMemberStrategyToken)
	}
	symbol, timeframe := strategyArgSymbolTimeframe(sc.Args)
	if symbol == "" || timeframe == "" {
		add("needs args [%q, <coin>, <timeframe>, ...] so management keeps its candle context", pairMemberStrategyToken)
	} else if symbol != p.Underlying {
		add("symbol %q must be the pair underlying %q", symbol, p.Underlying)
	}
	switch mode := rawEdgeGateMode(sc.Args).Kind; {
	case liveShapedFixture:
		if mode != edgeGateModeLive {
			add("the controlled live-shaped fixture needs --mode=live explicitly in args")
		}
	case isLiveArgs(sc.Args):
		add("must not be live: pair dispatch is research-only")
	case mode != edgeGateModePaper:
		add("must declare --mode=paper explicitly in args")
	}
	if name := strings.TrimSpace(sc.OpenStrategy.Name); name != "" && name != pairMemberStrategyToken {
		add("must not set open_strategy (%q); only the pair coordinator places member increases", sc.OpenStrategy.Name)
	}
	if sc.CloseStrategy == nil || strings.TrimSpace(sc.CloseStrategy.Name) == "" {
		add("needs an explicit native close_strategy")
	} else if name := canonicalCloseStrategyName(sc.CloseStrategy.Name); !isNativeCloseStrategy(name) {
		add("close_strategy %q is an open-strategy close fallback; a member needs a native close strategy so no entry evaluator runs", name)
	} else if name == "avwap_stop" {
		add("close_strategy avwap_stop needs an open-strategy avwap column, which a member does not evaluate")
	}
	if !pairMemberStopOwner(sc) {
		add("needs a resolvable stop owner before the coordinator admits any increase (Hyperliquid: a stop field or the default ATR stop; OKX: the atr_stop close strategy or a close with sl_after)")
	}
	if EffectiveDirection(sc) != DirectionBoth {
		add("must set direction \"both\" so the pair can hold either orientation (got %q)", EffectiveDirection(sc))
	}
	if !basketFinitePositive(sc.Capital) {
		add("needs a finite capital above zero for its paper collateral, got %v", sc.Capital)
	}
	if sc.CapitalPct != 0 {
		add("must not set capital_pct")
	}
	conflicts := []struct {
		set  bool
		name string
	}{
		{strings.TrimSpace(sc.BasketID) != "", "basket_id"},
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
			add("must not set %s: it configures an independent entry route or sizing that would bypass the pair owner", c.name)
		}
	}
	if sc.ClosedBarDecisions {
		add("must not set closed_bar_decisions: pair increases come from exact quote events, which have no candle boundary")
	}
	return errs
}

func pairMemberStopOwner(sc StrategyConfig) bool {
	if sc.Platform == "hyperliquid" {
		return basketMemberHasStopOwner(sc)
	}
	if sc.CloseStrategy == nil {
		return false
	}
	if canonicalCloseStrategyName(sc.CloseStrategy.Name) == "atr_stop" {
		return true
	}
	_, ok := sc.CloseStrategy.Params["sl_after"]
	return ok
}

func pairParamErrors(prefix string, p PairConfig) []string {
	allowed, ok := pairPolicyParamKeys[p.Policy]
	if !ok {
		return nil
	}
	known := make(map[string]bool, len(allowed))
	for _, k := range allowed {
		known[k] = true
	}
	keys := make([]string, 0, len(p.Params))
	for k := range p.Params {
		if !known[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, fmt.Sprintf("%s: unknown field %q in params for policy %s", prefix, k, p.Policy))
	}
	return out
}

func pairConfigErrors(cfg *Config, raw json.RawMessage, liveShapedFixture bool) (PairConfig, pairOperationalLimits, []string) {
	var p PairConfig
	var limits pairOperationalLimits
	if rawErrs := pairRawPairErrors(raw); len(rawErrs) > 0 {
		sort.Strings(rawErrs)
		return p, limits, dedupeSortedStrings(rawErrs)
	}
	var errs []string
	if err := json.Unmarshal(raw, &p); err != nil {
		errs = append(errs, fmt.Sprintf("pairs[0]: %v", err))
		return p, limits, errs
	}
	p.ID = strings.TrimSpace(p.ID)
	p.Underlying = strings.TrimSpace(p.Underlying)
	p.Policy = strings.TrimSpace(p.Policy)
	prefix := "pairs[0]"
	if p.ID != "" {
		prefix = fmt.Sprintf("pairs[%s]", p.ID)
	}
	add := func(format string, a ...interface{}) {
		errs = append(errs, prefix+": "+fmt.Sprintf(format, a...))
	}
	limits, limitErrs := pairOperationalLimitErrors(p.Limits)
	for _, e := range limitErrs {
		errs = append(errs, prefix+": "+e)
	}
	if !basketIDPattern.MatchString(p.ID) {
		add("id %q must match %s", p.ID, basketIDPattern.String())
	}
	if p.Underlying == "" || strings.ContainsAny(p.Underlying, "/:- ") || strings.ToUpper(p.Underlying) != p.Underlying {
		add("underlying %q must be one upper-case base asset such as BTC", p.Underlying)
	}
	if p.Policy == "" {
		add("needs a policy")
	} else if !pairPolicyStrategies[p.Policy] {
		add("policy %q is not a registered pair policy", p.Policy)
	}
	errs = append(errs, pairParamErrors(prefix, p)...)
	if err := p.BookNeed.validate(); err != nil {
		add("book_need: %v", err)
	}
	if cfg == nil {
		return p, limits, errs
	}
	for _, b := range cfg.Baskets {
		if b.ID == p.ID {
			add("id %q collides with a basket id", p.ID)
		}
	}
	if len(cfg.Baskets) > 0 {
		add("a pair research run admits one isolated pair; the config must not also declare baskets")
	}
	byID := strategyConfigByID(cfg.Strategies)
	if len(p.Members) != 2 {
		add("needs exactly two native members, one Hyperliquid perpetual and one OKX linear swap, got %d", len(p.Members))
	}
	seenMember := map[string]bool{}
	seenProvider := map[string]bool{}
	var partition RiskPartition
	partitionSet := false
	owned := map[string]string{}
	for i, m := range p.Members {
		mp := fmt.Sprintf("%s.members[%d]", prefix, i)
		id := strings.TrimSpace(m.StrategyID)
		if id == "" {
			errs = append(errs, mp+": strategy_id is missing")
			continue
		}
		if seenMember[id] {
			errs = append(errs, fmt.Sprintf("%s: member %q is listed twice", mp, id))
			continue
		}
		seenMember[id] = true
		sc, ok := byID[id]
		if !ok {
			errs = append(errs, fmt.Sprintf("%s: member %q is not a configured strategy", mp, id))
			continue
		}
		if strings.TrimSpace(sc.PairID) != p.ID {
			errs = append(errs, fmt.Sprintf("%s: member %q must set \"pair_id\": %q (got %q)", mp, id, p.ID, sc.PairID))
		}
		provider := pairProviderPlatforms[sc.Platform]
		if provider == "" {
			errs = append(errs, fmt.Sprintf("%s: member %q runs on platform %q, which has no pair venue", mp, id, sc.Platform))
			continue
		}
		if seenProvider[provider] {
			errs = append(errs, fmt.Sprintf("%s: two members use the %s venue; a pair needs one member on each of two native venues", mp, provider))
		}
		seenProvider[provider] = true
		if !strings.HasPrefix(m.AccountKey, pairSyntheticAccountPrefix) || len(m.AccountKey) == len(pairSyntheticAccountPrefix) {
			errs = append(errs, fmt.Sprintf("%s: account_key %q must name an explicit synthetic research account (%s<name>)", mp, m.AccountKey, pairSyntheticAccountPrefix))
		}
		if strings.TrimSpace(m.Instrument) == "" {
			errs = append(errs, mp+": instrument is missing")
		}
		errs = append(errs, pairInstrumentMetaErrors(mp, provider, p.Underlying, m, sc)...)
		errs = append(errs, pairMemberContractErrors(mp, provider, m)...)
		part := partitionFor(sc)
		if part.IsLive() != liveShapedFixture {
			if liveShapedFixture {
				errs = append(errs, fmt.Sprintf("%s: member %q must be in the live partition for the controlled live-shaped fixture", mp, id))
			} else {
				errs = append(errs, fmt.Sprintf("%s: member %q is in the live partition; a research pair runs in one paper partition or one paper source", mp, id))
			}
		}
		if !partitionSet {
			partition, partitionSet = part, true
		} else if part != partition {
			errs = append(errs, fmt.Sprintf("%s: members span risk partitions %s and %s (mixed partition or paper_source)", prefix, partitionLabel(partition), partitionLabel(part)))
		}
		ownerKey := part.String() + "|" + provider + "|" + m.AccountKey + "|" + m.Instrument
		if other, taken := owned[ownerKey]; taken {
			errs = append(errs, fmt.Sprintf("%s: %s and %s claim the same venue account and instrument", prefix, other, id))
		}
		owned[ownerKey] = id
		errs = append(errs, pairMemberFieldErrors(sc, p, liveShapedFixture)...)
	}
	if len(p.Members) == 2 && (!seenProvider[entryProviderHyperliquid] || !seenProvider[entryProviderOKX]) {
		add("needs one Hyperliquid perpetual member and one OKX linear swap member")
	}
	if len(errs) == 0 && partitionSet {
		var ms []pairContractMember
		for _, m := range p.Members {
			sc := byID[strings.TrimSpace(m.StrategyID)]
			ms = append(ms, pairContractMember{MemberID: sc.ID, Provider: pairProviderPlatforms[sc.Platform], AccountKey: m.AccountKey, Instrument: m.Instrument,
				Meta: m.Meta, Source: m.Source, ModeledDeliveryMs: m.ModeledDeliveryMs})
		}
		if worst := pairEnvelopeWorstCaseBytes(partition.String(), p.ID, strings.Repeat("0", 64), ms, p.BookNeed); worst > limits.EnvelopeMaxBytes {
			add("limits.envelope_max_bytes %d is below the %d-byte worst-case quote envelope for book_need %d levels, %d observations and these member sources; no cadence slot could ever freeze", limits.EnvelopeMaxBytes, worst, p.BookNeed.Depth, p.BookNeed.Capacity)
		}
	}
	for _, sc := range cfg.Strategies {
		if seenMember[sc.ID] {
			continue
		}
		if isPairMember(sc) {
			errs = append(errs, fmt.Sprintf("strategy[%s]: pair_id %q but pair %s does not list it as a member", sc.ID, sc.PairID, p.ID))
			continue
		}
		if strategyNameFromArgs(sc.Args) == pairMemberStrategyToken || strings.TrimSpace(sc.OpenStrategy.Name) == pairMemberStrategyToken {
			errs = append(errs, fmt.Sprintf("strategy[%s]: %q is reserved for pair members (set pair_id)", sc.ID, pairMemberStrategyToken))
		}
		if !partitionSet || partitionFor(sc) != partition {
			continue
		}
		symbol, _ := strategyArgSymbolTimeframe(sc.Args)
		if (sc.Platform == "hyperliquid" || sc.Platform == "okx") && strings.EqualFold(symbol, p.Underlying) {
			errs = append(errs, fmt.Sprintf("strategy[%s]: trades %s on %s in the %s partition, which pair %q owns exclusively", sc.ID, p.Underlying, sc.Platform, partitionLabel(partition), p.ID))
		}
		if HedgeEnabled(sc) && strings.EqualFold(hedgeCoin(sc), p.Underlying) {
			errs = append(errs, fmt.Sprintf("strategy[%s]: hedges %s in the %s partition, which pair %q owns exclusively", sc.ID, p.Underlying, partitionLabel(partition), p.ID))
		}
	}
	sort.Strings(errs)
	return p, limits, dedupeSortedStrings(errs)
}

func buildPairContract(cfg *Config, p PairConfig, limits pairOperationalLimits) (basketContract, error) {
	byID := strategyConfigByID(cfg.Strategies)
	c := basketContract{
		Version:     basketContractVersion,
		BasketID:    p.ID,
		Mode:        pairGroupMode,
		PairVersion: pairContractVersion,
		Listings:    map[string]basketListingMs{},
		Pair: &pairContractBody{
			Underlying: p.Underlying,
			Policy:     p.Policy,
			Params:     p.Params,
			Limits:     limits,
			BookNeed:   p.BookNeed,
		},
	}
	var members []StrategyConfig
	for _, m := range p.Members {
		sc, ok := byID[m.StrategyID]
		if !ok {
			return basketContract{}, fmt.Errorf("pair %s lists member %s, which is not a configured strategy", p.ID, m.StrategyID)
		}
		members = append(members, sc)
		symbol, timeframe := strategyArgSymbolTimeframe(sc.Args)
		closeName := ""
		if sc.CloseStrategy != nil {
			closeName = canonicalCloseStrategyName(sc.CloseStrategy.Name)
		}
		meta := m.Meta
		meta.FeeCurrencies = append([]string{}, m.Meta.FeeCurrencies...)
		sort.Strings(meta.FeeCurrencies)
		c.Pair.Members = append(c.Pair.Members, pairContractMember{
			MemberID:          sc.ID,
			Platform:          sc.Platform,
			Type:              sc.Type,
			Provider:          pairProviderPlatforms[sc.Platform],
			AccountKey:        m.AccountKey,
			Instrument:        m.Instrument,
			Symbol:            symbol,
			Timeframe:         timeframe,
			CloseStrategy:     closeName,
			Meta:              meta,
			Source:            m.Source,
			ModeledDeliveryMs: m.ModeledDeliveryMs,
			CostPolicy:        m.CostPolicy.rates(),
			Leverage:          EffectiveExchangeLeverage(sc),
		})
	}
	if len(members) == 0 {
		return basketContract{}, fmt.Errorf("pair %s has no members", p.ID)
	}
	c.Partition = partitionFor(members[0]).String()
	sort.Slice(members, func(i, j int) bool { return members[i].ID < members[j].ID })
	for _, sc := range members {
		symbol, timeframe := strategyArgSymbolTimeframe(sc.Args)
		c.Members = append(c.Members, basketContractMember{StrategyID: sc.ID, Coin: symbol, Timeframe: timeframe, IntervalMs: closedBarDecisionIntervalsMs[timeframe]})
	}
	sorted := c.pairMembersSorted()
	c.Pair.Members = sorted
	return c, nil
}

func pairReloadErrors(cfg, next *Config) []string {
	var errs []string
	if len(cfg.Pairs) > 0 || len(next.Pairs) > 0 {
		if !reflect.DeepEqual(cfg.Pairs, next.Pairs) {
			errs = append(errs, "pairs changed (identity, membership, policy, parameters, limits, venue accounts and instrument mappings are restart-required)")
		}
	}
	nextByID := strategyConfigByID(next.Strategies)
	for _, sc := range cfg.Strategies {
		ns, ok := nextByID[sc.ID]
		if !ok {
			continue
		}
		if strings.TrimSpace(sc.PairID) != strings.TrimSpace(ns.PairID) {
			errs = append(errs, fmt.Sprintf("strategy[%s] pair_id changed (%q -> %q; restart required)", sc.ID, sc.PairID, ns.PairID))
		}
		if isPairMember(sc) && EffectiveExchangeLeverage(sc) != EffectiveExchangeLeverage(ns) {
			errs = append(errs, fmt.Sprintf("strategy[%s] leverage changed (%v -> %v); the pair contract freezes the selected leverage (restart required)", sc.ID, EffectiveExchangeLeverage(sc), EffectiveExchangeLeverage(ns)))
		}
	}
	return errs
}

const (
	pairAdmissionResearch          = "research"
	pairAdmissionLiveShapedFixture = "live_shaped_fixture"
)

type pairResearchDeps struct {
	VenueFor func(StrategyConfig) (basketVenue, error)
	Evidence pairEvidenceSource
	Quotes   pairQuoteSource
	Funding  pairFundingSource
}

type pairResearchAdmission struct {
	nonce    string
	kind     string
	runID    string
	stateDir string
	resume   bool
	deps     pairResearchDeps
	pair     PairConfig
	limits   pairOperationalLimits
	raw      json.RawMessage
	contract string
	evidence *pairFrozenEvidenceSource
	costMu   sync.RWMutex
	costs    *pairAdmissionCosts

	funding        pairFundingSource
	fundingBinding []byte
	fundingPart    string
}

func (a *pairResearchAdmission) evidenceSource() pairEvidenceSource {
	if a == nil || a.evidence == nil {
		return nil
	}
	return a.evidence
}

func (a *pairResearchAdmission) quoteSource() pairQuoteSource {
	if a == nil {
		return nil
	}
	return a.deps.Quotes
}

var pairAdmissionRegistry sync.Map

func newPairAdmissionNonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func (a *pairResearchAdmission) valid() bool {
	if a == nil || a.nonce == "" {
		return false
	}
	v, ok := pairAdmissionRegistry.Load(a.nonce)
	return ok && v == a
}

func (a *pairResearchAdmission) release() {
	if a != nil && a.nonce != "" {
		pairAdmissionRegistry.CompareAndDelete(a.nonce, a)
	}
}

type pairResearchManifest struct {
	Owner          string          `json:"owner"`
	RunID          string          `json:"run_id"`
	PairID         string          `json:"pair_id"`
	ContractFP     string          `json:"contract_fingerprint"`
	PairVersion    int             `json:"pair_v"`
	EvidenceDigest string          `json:"evidence_digest"`
	UID            int             `json:"uid"`
	FundingBinding json.RawMessage `json:"funding_binding,omitempty"`
}

func pairResearchRawPair(doc []byte) (json.RawMessage, error) {
	if _, err := pairStrictParse(doc); err != nil {
		return nil, fmt.Errorf("research config is refused before decoding: %w", err)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(doc, &root); err != nil {
		return nil, fmt.Errorf("research config is unreadable: %w", err)
	}
	rawPairs, ok := root["pairs"]
	if !ok {
		return nil, fmt.Errorf("research config has no \"pairs\" entry")
	}
	var pairs []json.RawMessage
	if err := json.Unmarshal(rawPairs, &pairs); err != nil {
		return nil, fmt.Errorf("research config \"pairs\" must be an array: %w", err)
	}
	if len(pairs) != 1 {
		return nil, fmt.Errorf("a research run admits exactly one pair, got %d", len(pairs))
	}
	if errs := pairRawPairErrors(pairs[0]); len(errs) > 0 {
		sort.Strings(errs)
		return nil, fmt.Errorf("pair config validation errors:\n  %s", strings.Join(dedupeSortedStrings(errs), "\n  "))
	}
	return pairs[0], nil
}

func newPairResearchAdmission(doc []byte, runID, stateDir string, resume bool, deps pairResearchDeps) (*pairResearchAdmission, *Config, error) {
	runID = strings.TrimSpace(runID)
	if runID == "" || !basketIDPattern.MatchString(runID) {
		return nil, nil, fmt.Errorf("pair research run id %q must match %s", runID, basketIDPattern.String())
	}
	if deps.VenueFor == nil {
		return nil, nil, fmt.Errorf("pair research needs a virtual transport")
	}
	if _, ok := deps.Evidence.(*pairFrozenEvidenceSource); !ok || deps.Evidence.(*pairFrozenEvidenceSource) == nil {
		return nil, nil, fmt.Errorf("pair research needs the immutable frozen evidence source (newPairFrozenEvidenceSource)")
	}
	if !filepath.IsAbs(stateDir) {
		return nil, nil, fmt.Errorf("pair research state directory %q must be absolute", stateDir)
	}
	raw, err := pairResearchRawPair(doc)
	if err != nil {
		return nil, nil, err
	}
	nonce, err := newPairAdmissionNonce()
	if err != nil {
		return nil, nil, err
	}
	adm := &pairResearchAdmission{nonce: nonce, kind: pairAdmissionResearch, runID: runID, stateDir: filepath.Clean(stateDir), resume: resume, deps: deps, raw: raw}
	pairAdmissionRegistry.Store(nonce, adm)
	ok := false
	defer func() {
		if !ok {
			adm.release()
		}
	}()
	cfg, err := loadPairResearchConfig(adm, doc)
	if err != nil {
		return nil, nil, err
	}
	contract, err := buildPairContract(cfg, adm.pair, adm.limits)
	if err != nil {
		return nil, nil, err
	}
	if err := pairQuoteSourceErrors(contract, deps.Quotes); err != nil {
		return nil, nil, fmt.Errorf("pair research quote source is refused: %w", err)
	}
	if err := adm.bindFunding(contract, deps.Funding); err != nil {
		return nil, nil, fmt.Errorf("pair research funding input is refused: %w", err)
	}
	for _, sc := range cfg.Strategies {
		if !isPairMember(sc) {
			continue
		}
		v, verr := deps.VenueFor(sc)
		if verr != nil {
			return nil, nil, fmt.Errorf("pair research member %s has no virtual transport: %w", sc.ID, verr)
		}
		pv, isPair := v.(pairVenue)
		if !isPair || !pv.Virtual() || v.LiveShaped() {
			return nil, nil, fmt.Errorf("pair research member %s transport is not a virtual research transport", sc.ID)
		}
		if !strings.HasPrefix(v.AccountKey(partitionFor(sc)), pairSyntheticAccountPrefix) {
			return nil, nil, fmt.Errorf("pair research member %s transport does not bind a synthetic account", sc.ID)
		}
	}
	if err := adm.checkStateDir(cfg); err != nil {
		return nil, nil, err
	}
	ok = true
	return adm, cfg, nil
}

func loadPairResearchConfig(adm *pairResearchAdmission, doc []byte) (*Config, error) {
	if !adm.valid() {
		return nil, fmt.Errorf("pair activation needs the internal research admission")
	}
	cfg, err := loadConfigDataWith("", append([]byte(nil), doc...), true, true, adm)
	if err != nil {
		return nil, err
	}
	contract, err := buildPairContract(cfg, adm.pair, adm.limits)
	if err != nil {
		return nil, err
	}
	src, isFrozen := adm.deps.Evidence.(*pairFrozenEvidenceSource)
	if !isFrozen || src == nil {
		return nil, fmt.Errorf("pair activation needs the immutable frozen evidence source")
	}
	if adm.kind == pairAdmissionLiveShapedFixture && adm.deps.Funding != nil {
		return nil, fmt.Errorf("pair live-shaped funding evidence has no owner in this build: a confirmed venue funding charge never receives an additional modeled paper charge, and attributed live funding ingestion is refused")
	}
	if err := src.bind(contract); err != nil {
		return nil, fmt.Errorf("pair research evidence is refused: %w", err)
	}
	adm.contract = contract.Fingerprint()
	adm.evidence = src
	if err := adm.bindCosts(cfg, contract); err != nil {
		return nil, fmt.Errorf("pair research member cost binding is refused: %w", err)
	}
	return cfg, nil
}

func (a *pairResearchAdmission) checkStateDir(cfg *Config) error {
	layout, err := resolveStorageLayout(cfg)
	if err != nil {
		return err
	}
	for _, f := range layout.Files {
		if f.InMemory || filepath.Dir(filepath.Clean(f.Path)) != a.stateDir {
			return fmt.Errorf("pair research state file %q is outside the run state directory %s", f.Path, a.stateDir)
		}
	}
	manifestPath := filepath.Join(a.stateDir, pairResearchManifestName)
	if a.resume {
		blob, err := os.ReadFile(manifestPath)
		if err != nil {
			return fmt.Errorf("pair research resume needs the saved run manifest: %w", err)
		}
		var saved pairResearchManifest
		if err := json.Unmarshal(blob, &saved); err != nil {
			return fmt.Errorf("pair research manifest is unreadable: %w", err)
		}
		savedVersion := saved.PairVersion
		if savedVersion == 0 {
			savedVersion = 1
		}
		if saved.Owner == pairResearchManifestOwner && savedVersion != pairContractVersion {
			return fmt.Errorf("pair research resume refused: the saved run %q uses pair contract version %d, which this build does not support (version %d); its state is kept unchanged and needs a new research run in a fresh directory", saved.RunID, savedVersion, pairContractVersion)
		}
		if saved.Owner != pairResearchManifestOwner || saved.RunID != a.runID || saved.PairID != a.pair.ID || saved.ContractFP != a.contract {
			return fmt.Errorf("pair research resume refused: the saved run %q (pair %s, contract %s) is not this run %q (pair %s, contract %s)", saved.RunID, saved.PairID, shortFingerprint(saved.ContractFP), a.runID, a.pair.ID, shortFingerprint(a.contract))
		}
		if saved.EvidenceDigest != a.evidence.Digest() {
			return fmt.Errorf("pair research resume refused: the evidence document digest %s is not the saved run's %s", shortFingerprint(a.evidence.Digest()), shortFingerprint(saved.EvidenceDigest))
		}
		if len(saved.FundingBinding) == 0 {
			return fmt.Errorf("pair research resume refused: the saved run %q has no funding binding; a pre-funding run needs a new research run, and its history is never charged retroactively", saved.RunID)
		}
		if err := pairFundingCompareBinding(a.fundingBinding, saved.FundingBinding); err != nil {
			return fmt.Errorf("pair research resume refused: %w; the saved state is kept unchanged", err)
		}
		if err := a.checkSavedFundingChain(layout); err != nil {
			return fmt.Errorf("pair research resume refused: %w; the saved state is kept unchanged", err)
		}
		if saved.UID != os.Getuid() {
			return fmt.Errorf("pair research state directory belongs to uid %d", saved.UID)
		}
		return nil
	}
	entries, err := os.ReadDir(a.stateDir)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read pair research state directory: %w", err)
	}
	if len(entries) > 0 {
		return fmt.Errorf("pair research state directory %s is not fresh: a new run needs an empty, freshly owned directory", a.stateDir)
	}
	if err := os.MkdirAll(a.stateDir, 0o700); err != nil {
		return fmt.Errorf("create pair research state directory: %w", err)
	}
	blob, err := json.Marshal(pairResearchManifest{Owner: pairResearchManifestOwner, RunID: a.runID, PairID: a.pair.ID, ContractFP: a.contract, PairVersion: pairContractVersion, EvidenceDigest: a.evidence.Digest(), UID: os.Getuid(), FundingBinding: json.RawMessage(a.fundingBinding)})
	if err != nil {
		return err
	}
	return os.WriteFile(manifestPath, blob, 0o600)
}

func (a *pairResearchAdmission) bindFunding(c basketContract, src pairFundingSource) error {
	switch s := src.(type) {
	case *pairFrozenFundingSource:
		if s == nil {
			src = nil
		}
	case *pairRecordedFundingSource:
		if s == nil {
			src = nil
		}
	case nil:
	default:
		return fmt.Errorf("pair research accepts only the immutable frozen funding source (newPairFrozenFundingSource) or the append-only recorded funding source (newPairRecordedFundingSource)")
	}
	if src == nil {
		return fmt.Errorf("pair research needs a funding source (newPairFrozenFundingSource or newPairRecordedFundingSource); missing funding input is never settled as zero")
	}
	_, raw, err := pairFundingBindingFor(c, src)
	if err != nil {
		return err
	}
	a.funding, a.fundingBinding, a.fundingPart = src, raw, c.Partition
	return nil
}

func (a *pairResearchAdmission) fundingSource() pairFundingSource {
	if a == nil {
		return nil
	}
	return a.funding
}

func (a *pairResearchAdmission) checkSavedFundingChain(layout storageLayout) error {
	rs, recorded := a.funding.(*pairRecordedFundingSource)
	part, err := parseRiskPartition(a.fundingPart)
	if err != nil {
		return err
	}
	for _, f := range layout.Files {
		saved, err := pairFundingReadSavedChain(f.Path, part, a.pair.ID)
		if err != nil {
			return fmt.Errorf("read the saved funding chain in %s: %w", f.Path, err)
		}
		if len(saved) == 0 {
			continue
		}
		if !recorded {
			return fmt.Errorf("the saved run holds %d recorded funding batches, but this run uses a %s funding source", len(saved), a.funding.Kind())
		}
		if err := pairFundingCheckChain(rs, saved); err != nil {
			return err
		}
	}
	return nil
}

func pairResearchConfigErrors(cfg *Config, adm *pairResearchAdmission) []string {
	if len(cfg.Pairs) != 1 {
		return []string{fmt.Sprintf("a research run admits exactly one pair, got %d", len(cfg.Pairs))}
	}
	p, limits, errs := pairConfigErrors(cfg, adm.raw, adm.kind == pairAdmissionLiveShapedFixture)
	if len(errs) > 0 {
		return errs
	}
	adm.pair, adm.limits = p, limits
	return nil
}

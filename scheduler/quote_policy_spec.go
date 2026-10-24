package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"
)

const (
	inventoryMarketMakingStrategyName = "inventory_market_making"
	quotePolicySpecVersion            = 1
	quotePolicySpecSchema             = "go-trader.quote-policy-spec"
)

const quotePolicySpecJSON = `{
  "schema": "go-trader.quote-policy-spec",
  "version": 1,
  "strategy": "inventory_market_making",
  "capability": {"input": "deadline_book_evidence", "output": "two_sided_quotes"},
  "params": [
    {"name": "quote_notional_usd", "type": "number", "default": 100, "min": 20, "max": 500, "unit": "USD per side"},
    {"name": "target_inventory_usd", "type": "number", "default": 0, "min": -2500, "max": 2500, "unit": "USD marked"},
    {"name": "risk_aversion", "type": "number", "default": 1, "min": 0, "max": 4, "unit": "dimensionless"},
    {"name": "volatility_multiplier", "type": "number", "default": 1, "min": 0, "max": 4, "unit": "dimensionless"},
    {"name": "volatility_window_seconds", "type": "integer", "default": 300, "min": 60, "max": 600, "unit": "seconds"},
    {"name": "min_spread_bps", "type": "number", "default": 10, "min": 2, "max": 50, "unit": "basis points, full spread"},
    {"name": "max_spread_bps", "type": "number", "default": 100, "min": 20, "max": 200, "unit": "basis points, full spread"},
    {"name": "max_skew_bps", "type": "number", "default": 50, "min": 0, "max": 100, "unit": "basis points"},
    {"name": "taper_band_pct", "type": "number", "default": 25, "min": 0, "min_exclusive": true, "max": 100, "unit": "percent of the cap"},
    {"name": "quote_lifetime_seconds", "type": "integer", "default": 30, "min": 5, "max": 120, "unit": "seconds"},
    {"name": "max_input_age_ms", "type": "integer", "default": 7500, "min": 1000, "max": 10000, "unit": "milliseconds"},
    {"name": "refresh_threshold_bps", "type": "number", "default": 2, "min": 0, "max": 20, "unit": "basis points of the midpoint"},
    {"name": "max_inventory_age_seconds", "type": "integer", "default": 300, "min": 30, "max": 1800, "unit": "seconds"},
    {"name": "round_trip_reserve_bps", "type": "number", "default": 2, "min": 0, "max": 20, "unit": "basis points"},
    {"name": "maker_fee_bps", "type": "number", "default": 1.5, "min": -10, "max": 10, "unit": "basis points per fill; a negative value never narrows the spread floor"},
    {"name": "min_order_value_usd", "type": "number", "default": 10, "min": 0, "max": 1000, "unit": "USD per risk-increasing order"}
  ],
  "param_rules": [
    {"kind": "le", "left": "min_spread_bps", "right": "max_spread_bps"}
  ],
  "config": [
    {"name": "interval_seconds", "type": "integer", "default": 5, "min": 1, "max": 60, "unit": "seconds, the due-deadline cycle"},
    {"name": "max_inventory_usd", "type": "number", "default": 1000, "min": 200, "max": 5000, "unit": "USD marked, quote.max_inventory_usd"},
    {"name": "stop_loss_pct", "type": "number", "default": 2, "min": 0, "min_exclusive": true, "max": 20, "unit": "percent from the risk anchor"}
  ],
  "config_rules": [
    {"kind": "le_fraction", "left": "quote_notional_usd", "right": "max_inventory_usd", "fraction": "1/4"},
    {"kind": "abs_le_fraction", "left": "target_inventory_usd", "right": "max_inventory_usd", "fraction": "1/2"},
    {"kind": "ge", "left": "quote_lifetime_seconds", "right": "interval_seconds"}
  ],
  "fixed": {"capital": 10000, "leverage": 1, "margin_mode": "isolated", "platform": "hyperliquid", "type": "perps", "close_strategy": "inventory_quote_exit"}
}`

type quotePolicySpecField struct {
	Name         string      `json:"name"`
	Type         string      `json:"type"`
	Default      json.Number `json:"default"`
	Min          json.Number `json:"min"`
	MinExclusive bool        `json:"min_exclusive,omitempty"`
	Max          json.Number `json:"max"`
	Unit         string      `json:"unit"`
}

type quotePolicySpecRule struct {
	Kind     string `json:"kind"`
	Left     string `json:"left"`
	Right    string `json:"right"`
	Fraction string `json:"fraction,omitempty"`
}

type quotePolicySpecDoc struct {
	Schema      string                 `json:"schema"`
	Version     int                    `json:"version"`
	Strategy    string                 `json:"strategy"`
	Capability  map[string]string      `json:"capability"`
	Params      []quotePolicySpecField `json:"params"`
	ParamRules  []quotePolicySpecRule  `json:"param_rules"`
	Config      []quotePolicySpecField `json:"config"`
	ConfigRules []quotePolicySpecRule  `json:"config_rules"`
	Fixed       map[string]any         `json:"fixed"`
}

var quotePolicySpec = mustDecodeQuotePolicySpec(quotePolicySpecJSON)

func decodeQuotePolicySpec(raw string) (*quotePolicySpecDoc, error) {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	dec.UseNumber()
	var doc quotePolicySpecDoc
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("quote policy spec: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("quote policy spec: trailing data")
	}
	if doc.Schema != quotePolicySpecSchema || doc.Version != quotePolicySpecVersion || doc.Strategy != inventoryMarketMakingStrategyName {
		return nil, fmt.Errorf("quote policy spec names schema %q version %d strategy %q", doc.Schema, doc.Version, doc.Strategy)
	}
	seen := map[string]bool{}
	for _, group := range [][]quotePolicySpecField{doc.Params, doc.Config} {
		for _, f := range group {
			if f.Name == "" || seen[f.Name] {
				return nil, fmt.Errorf("quote policy spec field %q is empty or repeated", f.Name)
			}
			seen[f.Name] = true
			if f.Type != "number" && f.Type != "integer" {
				return nil, fmt.Errorf("quote policy spec field %s has type %q", f.Name, f.Type)
			}
			d, lo, hi := quoteSpecRat(f.Default), quoteSpecRat(f.Min), quoteSpecRat(f.Max)
			if d == nil || lo == nil || hi == nil || lo.Cmp(hi) > 0 {
				return nil, fmt.Errorf("quote policy spec field %s has unreadable bounds", f.Name)
			}
			if err := quoteSpecCheck(f, d); err != nil {
				return nil, fmt.Errorf("quote policy spec default: %w", err)
			}
		}
	}
	for _, r := range append(append([]quotePolicySpecRule(nil), doc.ParamRules...), doc.ConfigRules...) {
		if !seen[r.Left] || !seen[r.Right] {
			return nil, fmt.Errorf("quote policy spec rule %s names an unknown field", r.Kind)
		}
		switch r.Kind {
		case "le", "ge":
		case "le_fraction", "abs_le_fraction":
			if _, ok := new(big.Rat).SetString(r.Fraction); !ok {
				return nil, fmt.Errorf("quote policy spec rule %s has fraction %q", r.Kind, r.Fraction)
			}
		default:
			return nil, fmt.Errorf("quote policy spec rule kind %q is unknown", r.Kind)
		}
	}
	return &doc, nil
}

func mustDecodeQuotePolicySpec(raw string) *quotePolicySpecDoc {
	doc, err := decodeQuotePolicySpec(raw)
	if err != nil {
		panic(err)
	}
	return doc
}

func quotePolicySpecCanonical() ([]byte, error) {
	dec := json.NewDecoder(strings.NewReader(quotePolicySpecJSON))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func quotePolicySpecSHA256() string {
	b, err := quotePolicySpecCanonical()
	if err != nil {
		return ""
	}
	return sha256Hex(b)
}

func quoteSpecRat(n json.Number) *big.Rat {
	r, ok := new(big.Rat).SetString(string(n))
	if !ok {
		return nil
	}
	return r
}

func quoteSpecField(name string) (quotePolicySpecField, bool) {
	for _, group := range [][]quotePolicySpecField{quotePolicySpec.Params, quotePolicySpec.Config} {
		for _, f := range group {
			if f.Name == name {
				return f, true
			}
		}
	}
	return quotePolicySpecField{}, false
}

func quoteSpecCheck(f quotePolicySpecField, v *big.Rat) error {
	if f.Type == "integer" && !v.IsInt() {
		return fmt.Errorf("%s must be an integer, got %s", f.Name, quoteDecimalText(v))
	}
	lo, hi := quoteSpecRat(f.Min), quoteSpecRat(f.Max)
	if f.MinExclusive && v.Cmp(lo) <= 0 {
		return fmt.Errorf("%s must be > %s, got %s", f.Name, f.Min, quoteDecimalText(v))
	}
	if !f.MinExclusive && v.Cmp(lo) < 0 {
		return fmt.Errorf("%s must be >= %s, got %s", f.Name, f.Min, quoteDecimalText(v))
	}
	if v.Cmp(hi) > 0 {
		return fmt.Errorf("%s must be <= %s, got %s", f.Name, f.Max, quoteDecimalText(v))
	}
	return nil
}

func quoteSpecValue(name string, raw any) (*big.Rat, error) {
	switch t := raw.(type) {
	case bool:
		return nil, fmt.Errorf("%s must be a number, got a boolean", name)
	case float64:
		if math.IsNaN(t) || math.IsInf(t, 0) {
			return nil, fmt.Errorf("%s must be finite, got %v", name, t)
		}
		return parseQuoteDecimal(strconv.FormatFloat(t, 'g', -1, 64))
	case int:
		return big.NewRat(int64(t), 1), nil
	case int64:
		return big.NewRat(t, 1), nil
	case json.Number:
		r, err := parseQuoteDecimal(t.String())
		if err != nil {
			return nil, fmt.Errorf("%s must be a finite number, got %s", name, t.String())
		}
		return r, nil
	}
	return nil, fmt.Errorf("%s must be a number, got %v", name, raw)
}

func quoteSpecResolve(fields []quotePolicySpecField, values map[string]any) (map[string]*big.Rat, error) {
	known := map[string]bool{}
	for _, f := range fields {
		known[f.Name] = true
	}
	var unknown []string
	for k := range values {
		if !known[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, fmt.Errorf("%s: unknown parameters %v", inventoryMarketMakingStrategyName, unknown)
	}
	out := make(map[string]*big.Rat, len(fields))
	for _, f := range fields {
		raw, ok := values[f.Name]
		var v *big.Rat
		if !ok {
			v = quoteSpecRat(f.Default)
		} else {
			parsed, err := quoteSpecValue(f.Name, raw)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", inventoryMarketMakingStrategyName, err)
			}
			v = parsed
		}
		if err := quoteSpecCheck(f, v); err != nil {
			return nil, fmt.Errorf("%s: %w", inventoryMarketMakingStrategyName, err)
		}
		out[f.Name] = v
	}
	return out, nil
}

func quoteSpecRuleErrors(rules []quotePolicySpecRule, values map[string]*big.Rat) []string {
	var errs []string
	for _, r := range rules {
		a, b := values[r.Left], values[r.Right]
		if a == nil || b == nil {
			errs = append(errs, fmt.Sprintf("rule %s %s/%s has no value", r.Kind, r.Left, r.Right))
			continue
		}
		switch r.Kind {
		case "le":
			if a.Cmp(b) > 0 {
				errs = append(errs, fmt.Sprintf("%s (%s) must be <= %s (%s)", r.Left, quoteDecimalText(a), r.Right, quoteDecimalText(b)))
			}
		case "ge":
			if a.Cmp(b) < 0 {
				errs = append(errs, fmt.Sprintf("%s (%s) must be >= %s (%s)", r.Left, quoteDecimalText(a), r.Right, quoteDecimalText(b)))
			}
		case "le_fraction", "abs_le_fraction":
			frac, _ := new(big.Rat).SetString(r.Fraction)
			limit := new(big.Rat).Mul(b, frac)
			lhs := a
			if r.Kind == "abs_le_fraction" {
				lhs = new(big.Rat).Abs(a)
			}
			if lhs.Cmp(limit) > 0 {
				label := r.Left
				if r.Kind == "abs_le_fraction" {
					label = "|" + r.Left + "|"
				}
				errs = append(errs, fmt.Sprintf("%s (%s) must be <= %s x %s (%s)", label, quoteDecimalText(lhs), r.Fraction, r.Right, quoteDecimalText(limit)))
			}
		}
	}
	return errs
}

func validateInventoryMarketMakingParams(params map[string]interface{}) error {
	values, err := quoteSpecResolve(quotePolicySpec.Params, params)
	if err != nil {
		return err
	}
	if errs := quoteSpecRuleErrors(quotePolicySpec.ParamRules, values); len(errs) > 0 {
		return fmt.Errorf("%s: %s", inventoryMarketMakingStrategyName, strings.Join(errs, "; "))
	}
	return nil
}

func inventoryMarketMakingConfigErrors(sc StrategyConfig, globalInterval int) []string {
	values, err := quoteSpecResolve(quotePolicySpec.Params, sc.OpenStrategy.Params)
	if err != nil {
		return []string{err.Error()}
	}
	var errs []string
	config := map[string]any{}
	interval := configuredStrategyIntervalSeconds(sc, globalInterval)
	config["interval_seconds"] = interval
	if sc.Quote != nil {
		config["max_inventory_usd"] = sc.Quote.MaxInventoryUSD
	}
	if sc.StopLossPct != nil {
		config["stop_loss_pct"] = *sc.StopLossPct
	} else {
		errs = append(errs, "stop_loss_pct is required")
	}
	for _, f := range quotePolicySpec.Config {
		if _, ok := config[f.Name]; !ok {
			errs = append(errs, fmt.Sprintf("%s is required", f.Name))
		}
	}
	if len(errs) > 0 {
		return errs
	}
	resolved, err := quoteSpecResolve(quotePolicySpec.Config, config)
	if err != nil {
		return []string{err.Error()}
	}
	for k, v := range resolved {
		values[k] = v
	}
	errs = append(errs, quoteSpecRuleErrors(quotePolicySpec.ParamRules, values)...)
	errs = append(errs, quoteSpecRuleErrors(quotePolicySpec.ConfigRules, values)...)
	return errs
}

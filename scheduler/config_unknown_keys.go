package main

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// knownStrategyConfigKeys returns the JSON tag names declared on
// StrategyConfig. Used by validateStrategyJSONKeys to flag operator typos
// (e.g. `take_profit_atr_mult`) instead of silently dropping invented fields
// that would otherwise produce a config that looks correct but is missing the
// requested protection (#704).
func knownStrategyConfigKeys() map[string]bool {
	known := make(map[string]bool)
	t := reflect.TypeOf(StrategyConfig{})
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		name := strings.SplitN(tag, ",", 2)[0]
		if name != "" {
			known[name] = true
		}
	}
	// #842: close_strategies (array) was collapsed to the single close_strategy
	// ref, but UnmarshalJSON still reads the legacy array for back-compat, so it
	// must not trip the unknown-field guard. A len>1 array is rejected with the
	// strategy id in validateConfig; a len-1 array is lifted to close_strategy.
	known["close_strategies"] = true
	return known
}

// unknownKeyHint returns a one-line suggestion for an unknown strategy field,
// chosen by substring match against the most commonly mistyped categories. The
// goal is to make the most frequent miss — TP/SL fields that look plausible
// but never existed — fail loudly with actionable guidance (#704).
func unknownKeyHint(key string) string {
	lk := strings.ToLower(key)
	switch {
	case strings.Contains(lk, "take_profit") || strings.Contains(lk, "tp_tier") || lk == "tp_tiers":
		return "TP logic lives under close_strategy (e.g. {\"name\":\"tiered_tp_atr\",\"params\":{\"tp_tiers\":[...]}}); user_defaults.manual.tp_tiers only seeds defaults for type=manual"
	case strings.HasPrefix(lk, "stop_loss") || strings.Contains(lk, "stoploss"):
		return "valid SL fields: stop_loss_atr_mult, stop_loss_pct, stop_loss_margin_pct, trailing_stop_pct, trailing_stop_atr_mult (mutually exclusive)"
	case lk == "params" || lk == "open" || lk == "close":
		return "pre-v13 flat shape; use open_strategy: {name, params} and close_strategy: {name, params}"
	default:
		return ""
	}
}

// validateStrategyJSONKeys re-parses the raw config bytes and flags any key
// inside an entry of `strategies[]` that isn't declared on StrategyConfig.
// json.Unmarshal silently drops unknown keys, so without this check an
// operator typo (e.g. `take_profit_atr_mult`) loads as a stripped struct
// indistinguishable from "no TP configured" — which is exactly the misdiagnosis
// path that led to #704.
//
// Scoped to the strategies array only. We don't enable DisallowUnknownFields
// globally because the surrounding config has optional/extension fields
// (platforms, notifier backends) that intentionally tolerate forward-compat
// keys. Returns a sorted list of "strategy[id]: unknown field %q" errors.
func validateStrategyJSONKeys(rawData []byte) []string {
	var envelope struct {
		Strategies []map[string]json.RawMessage `json:"strategies"`
	}
	if err := json.Unmarshal(rawData, &envelope); err != nil {
		// Top-level shape errors are reported by the main json.Unmarshal in
		// LoadConfig; skip here so we don't double-report.
		return nil
	}
	known := knownStrategyConfigKeys()
	var errs []string
	for i, s := range envelope.Strategies {
		prefix := fmt.Sprintf("strategy[%d]", i)
		if idRaw, ok := s["id"]; ok {
			var id string
			if json.Unmarshal(idRaw, &id) == nil && id != "" {
				prefix = fmt.Sprintf("strategy[%s]", id)
			}
		}
		keys := make([]string, 0, len(s))
		for k := range s {
			if !known[k] {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			msg := fmt.Sprintf("%s: unknown field %q", prefix, k)
			if hint := unknownKeyHint(k); hint != "" {
				msg += " — " + hint
			}
			errs = append(errs, msg)
		}
	}
	return errs
}

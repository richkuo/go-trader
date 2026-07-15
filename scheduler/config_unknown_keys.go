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
// knownHedgeConfigKeys returns the accepted JSON keys of the nested hedge block
// (#1159), derived reflectively so the set stays in sync with HedgeConfig.
func knownHedgeConfigKeys() map[string]bool {
	return knownJSONKeys(reflect.TypeOf(HedgeConfig{}))
}

func knownStrategyConfigKeys() map[string]bool {
	known := knownJSONKeys(reflect.TypeOf(StrategyConfig{}))
	// #842: close_strategies (array) was collapsed to the single close_strategy
	// ref, but UnmarshalJSON still reads the legacy array for back-compat, so it
	// must not trip the unknown-field guard. A len>1 array is rejected with the
	// strategy id in validateConfig; a len-1 array is lifted to close_strategy.
	known["close_strategies"] = true
	return known
}

func knownUserDefaultsKeys() map[string]bool {
	return knownJSONKeys(reflect.TypeOf(UserDefaultsConfig{}))
}

func knownManualDefaultsKeys() map[string]bool {
	return knownJSONKeys(reflect.TypeOf(ManualDefaultsConfig{}))
}

func knownJSONKeys(t reflect.Type) map[string]bool {
	known := make(map[string]bool)
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
		// #1159: the hedge sub-block is a nested object; a typo'd leaf key
		// (e.g. "ration") would otherwise be silently dropped and default. Fail
		// it loudly like the user_defaults tree does.
		if rawHedge, ok := s["hedge"]; ok {
			var hedge map[string]json.RawMessage
			if err := json.Unmarshal(rawHedge, &hedge); err == nil && hedge != nil {
				hedgeKnown := knownHedgeConfigKeys()
				hkeys := make([]string, 0, len(hedge))
				for k := range hedge {
					if !hedgeKnown[k] {
						hkeys = append(hkeys, k)
					}
				}
				sort.Strings(hkeys)
				for _, k := range hkeys {
					errs = append(errs, fmt.Sprintf("%s.hedge: unknown field %q", prefix, k))
				}
			}
		}
	}
	return errs
}

// validateUserDefaultsJSONKeys flags typos inside the canonical #1135
// user_defaults tree. Top-level config keys stay forward-compatible, but once
// an operator opts into the stop-loss-adjacent defaults block, unknown siblings
// and manual leaf keys must fail loudly instead of silently falling back.
func validateUserDefaultsJSONKeys(rawData []byte) []string {
	var envelope struct {
		UserDefaults map[string]json.RawMessage `json:"user_defaults"`
	}
	if err := json.Unmarshal(rawData, &envelope); err != nil {
		return nil
	}
	if envelope.UserDefaults == nil {
		return nil
	}

	var errs []string
	userKnown := knownUserDefaultsKeys()
	keys := make([]string, 0, len(envelope.UserDefaults))
	for k := range envelope.UserDefaults {
		if !userKnown[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		errs = append(errs, fmt.Sprintf("user_defaults: unknown field %q", k))
	}

	if rawManual, ok := envelope.UserDefaults["manual"]; ok {
		var manual map[string]json.RawMessage
		if err := json.Unmarshal(rawManual, &manual); err == nil && manual != nil {
			manualKnown := knownManualDefaultsKeys()
			keys = keys[:0]
			for k := range manual {
				if !manualKnown[k] {
					keys = append(keys, k)
				}
			}
			sort.Strings(keys)
			for _, k := range keys {
				errs = append(errs, fmt.Sprintf("user_defaults.manual: unknown field %q", k))
			}
		}
	}

	return errs
}

package main

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

func knownStrategyConfigKeys() map[string]bool {
	known := knownJSONKeys(reflect.TypeOf(StrategyConfig{}))
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

func unknownKeyHint(key string) string {
	lk := strings.ToLower(key)
	switch {
	case lk == legacyTrailStopATRRegimeKey:
		return "renamed to " + trailStopATRRegimeKey + " in #1465; the v18 migration rewrites it on disk"
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

func validateStrategyJSONKeys(rawData []byte) []string {
	var envelope struct {
		Strategies []map[string]json.RawMessage `json:"strategies"`
	}
	if err := json.Unmarshal(rawData, &envelope); err != nil {
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
		errs = append(errs, nestedObjectUnknownKeyErrors(s, "hedge", knownHedgeConfigKeys(), prefix)...)
		errs = append(errs, nestedObjectUnknownKeyErrors(s, "hurst_gate", knownHurstGateKeys(), prefix)...)
	}
	return errs
}

func knownHedgeConfigKeys() map[string]bool {
	return knownJSONKeys(reflect.TypeOf(HedgeConfig{}))
}

func knownHurstGateKeys() map[string]bool {
	return knownJSONKeys(reflect.TypeOf(HurstGateConfig{}))
}

func nestedObjectUnknownKeyErrors(entry map[string]json.RawMessage, field string, known map[string]bool, prefix string) []string {
	raw, ok := entry[field]
	if !ok {
		return nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		if !known[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	errs := make([]string, 0, len(keys))
	for _, k := range keys {
		errs = append(errs, fmt.Sprintf("%s: unknown field %q in %s block", prefix, k, field))
	}
	return errs
}

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

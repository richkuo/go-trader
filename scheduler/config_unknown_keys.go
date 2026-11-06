package main

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strconv"
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
		errs = append(errs, restingTPTradeThroughRawErrors(prefix, s)...)
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

const retiredResearchArchiveTag = "archive/basket-pair-research"

const (
	retiredBasketMemberToken = "basket_member"
	retiredPairMemberToken   = "pair_member"
	retiredBasketMemberFlag  = "--basket-member"
	retiredPairMemberFlag    = "--pair-member-json"
	retiredManagementFlag    = "--management-stdin"
)

func retiredResearchRefusal(subject, what string) string {
	return fmt.Sprintf("%s: %s belongs to the basket and pair research feature removed in issue 1955; its code is kept in git tag %s and no load route accepts it", subject, what, retiredResearchArchiveTag)
}

func retiredResearchConfigRefusals(raw []byte) []string {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil
	}
	var errs []string
	for _, key := range []string{"baskets", "pairs"} {
		if _, ok := root[key]; ok {
			errs = append(errs, retiredResearchRefusal("config root", strconv.Quote(key)))
		}
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
		subject := fmt.Sprintf("strategy[%d]", i)
		if idRaw, ok := s["id"]; ok {
			var id string
			if json.Unmarshal(idRaw, &id) == nil && id != "" {
				subject = fmt.Sprintf("strategy[%s]", id)
			}
		}
		for _, key := range []string{"basket_id", "pair_id"} {
			if _, ok := s[key]; ok {
				errs = append(errs, retiredResearchRefusal(subject, strconv.Quote(key)))
			}
		}
		var args []string
		if rawArgs, ok := s["args"]; ok && json.Unmarshal(rawArgs, &args) == nil {
			if len(args) > 0 {
				if token := strings.TrimSpace(args[0]); token == retiredBasketMemberToken || token == retiredPairMemberToken {
					errs = append(errs, retiredResearchRefusal(subject, "args[0] "+strconv.Quote(token)))
				}
			}
			for _, flag := range []string{retiredBasketMemberFlag, retiredPairMemberFlag, retiredManagementFlag} {
				for _, a := range args {
					if a == flag || strings.HasPrefix(a, flag+"=") {
						errs = append(errs, retiredResearchRefusal(subject, "the flag "+flag))
						break
					}
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
			if name = strings.TrimSpace(name); name == retiredBasketMemberToken || name == retiredPairMemberToken {
				errs = append(errs, retiredResearchRefusal(subject, "open_strategy "+strconv.Quote(name)))
			}
		}
	}
	return errs
}

func validateTradeFeedbackAnalysisJSON(rawData []byte) []string {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(rawData, &root); err != nil {
		return nil
	}
	raw, ok := root[tradeFeedbackAnalysisConfigKey]
	if !ok {
		return nil
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "null" {
		return []string{tradeFeedbackAnalysisConfigKey + ": must be an object; omit the block to use the defaults"}
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return []string{tradeFeedbackAnalysisConfigKey + ": must be an object"}
	}
	known := knownJSONKeys(reflect.TypeOf(TradeFeedbackAnalysisConfig{}))
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var errs []string
	for _, k := range keys {
		if !known[k] {
			errs = append(errs, fmt.Sprintf("%s: unknown field %q", tradeFeedbackAnalysisConfigKey, k))
			continue
		}
		lo, hi := tradeFeedbackAnalysisFieldBounds(k)
		text := strings.TrimSpace(string(obj[k]))
		n, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s.%s: must be a whole number from %d to %d, got %s", tradeFeedbackAnalysisConfigKey, k, lo, hi, text))
			continue
		}
		if n < int64(lo) || n > int64(hi) {
			errs = append(errs, fmt.Sprintf("%s.%s: must be from %d to %d, got %d", tradeFeedbackAnalysisConfigKey, k, lo, hi, n))
		}
	}
	return errs
}

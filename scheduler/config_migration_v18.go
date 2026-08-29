package main

import (
	"encoding/json"
	"fmt"
	"sort"
)

const (
	legacyTrailStopATRRegimeKey = "trailing_stop_atr_regime"
	trailStopATRRegimeKey       = "trail_stop_atr_regime"
)

func needsV18TrailStopKeyMigration(data []byte) bool {
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return false
	}
	if version, ok := raw["config_version"].(float64); !ok || int(version) < 18 {
		return true
	}
	return hasLegacyTrailStopATRRegimeKey(raw)
}

func hasLegacyTrailStopATRRegimeKey(raw map[string]interface{}) bool {
	if raw == nil {
		return false
	}
	if strategies, ok := raw["strategies"].([]interface{}); ok {
		for _, item := range strategies {
			sc, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			if _, present := sc[legacyTrailStopATRRegimeKey]; present {
				return true
			}
		}
	}
	for _, section := range v18UserDefaultsSections(raw) {
		if _, present := section[legacyTrailStopATRRegimeKey]; present {
			return true
		}
	}
	return false
}

func v18UserDefaultsSections(raw map[string]interface{}) []map[string]interface{} {
	userDefaults, ok := raw["user_defaults"].(map[string]interface{})
	if !ok {
		return nil
	}
	var out []map[string]interface{}
	if regimeATR, ok := userDefaults[userCloseDefaultRegimeATRKey].(map[string]interface{}); ok {
		out = append(out, regimeATR)
	}
	if manual, ok := userDefaults["manual"].(map[string]interface{}); ok {
		out = append(out, manual)
	}
	if closes, ok := userDefaults["close"].(map[string]interface{}); ok {
		names := make([]string, 0, len(closes))
		for name := range closes {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if entry, ok := closes[name].(map[string]interface{}); ok {
				out = append(out, entry)
			}
		}
	}
	return out
}

func migrateV18TrailStopATRRegimeKey(raw map[string]interface{}) error {
	if raw == nil {
		return nil
	}
	renamed := false
	if strategies, ok := raw["strategies"].([]interface{}); ok {
		for i, item := range strategies {
			sc, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			id := strictStringFromJSON(sc["id"])
			if id == "" {
				id = fmt.Sprintf("%d", i)
			}
			did, err := renameV18TrailStopKey(sc, fmt.Sprintf("strategy[%s]", id))
			if err != nil {
				return err
			}
			if did {
				fmt.Printf("[migration] strategy[%s]: renamed config key %q -> %q\n", id, legacyTrailStopATRRegimeKey, trailStopATRRegimeKey)
				renamed = true
			}
		}
	}
	for _, section := range v18UserDefaultsSections(raw) {
		did, err := renameV18TrailStopKey(section, "user_defaults")
		if err != nil {
			return err
		}
		if did {
			fmt.Printf("[migration] user_defaults: renamed config key %q -> %q\n", legacyTrailStopATRRegimeKey, trailStopATRRegimeKey)
			renamed = true
		}
	}
	if renamed {
		warnDeprecatedConfigKey(legacyTrailStopATRRegimeKey, trailStopATRRegimeKey)
	}
	return nil
}

func renameV18TrailStopKey(block map[string]interface{}, ctx string) (bool, error) {
	legacy, hasLegacy := block[legacyTrailStopATRRegimeKey]
	if !hasLegacy {
		return false, nil
	}
	if current, hasCurrent := block[trailStopATRRegimeKey]; hasCurrent {
		if !jsonEquivalent(current, legacy) {
			return false, fmt.Errorf("%s: %q conflicts with %q; keep one canonical block",
				ctx, legacyTrailStopATRRegimeKey, trailStopATRRegimeKey)
		}
		delete(block, legacyTrailStopATRRegimeKey)
		return true, nil
	}
	block[trailStopATRRegimeKey] = legacy
	delete(block, legacyTrailStopATRRegimeKey)
	return true, nil
}

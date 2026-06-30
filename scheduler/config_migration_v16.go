package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

const (
	legacyUserCloseDefaultsKey = "user_close_defaults"
	legacyManualDefaultsKey    = "manual_defaults"
)

// needsV16UserDefaultsMigration reports whether the on-disk config still needs
// the #1135 operator-defaults rewrite. Besides version<16, keep accepting the
// deprecated top-level aliases in hand-edited v16 files so load rewrites them
// back to the canonical tree before normal unmarshalling.
func needsV16UserDefaultsMigration(data []byte) bool {
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return false
	}
	if version, ok := raw["config_version"].(float64); !ok || int(version) < 16 {
		return true
	}
	return hasLegacyUserDefaultAliases(raw)
}

func hasLegacyUserDefaultAliases(raw map[string]interface{}) bool {
	if raw == nil {
		return false
	}
	_, hasClose := raw[legacyUserCloseDefaultsKey]
	_, hasManual := raw[legacyManualDefaultsKey]
	return hasClose || hasManual
}

func migrateV16UserDefaults(raw map[string]interface{}) error {
	if raw == nil {
		return nil
	}
	closeRaw, hasLegacyClose := raw[legacyUserCloseDefaultsKey]
	manualRaw, hasLegacyManual := raw[legacyManualDefaultsKey]
	if !hasLegacyClose && !hasLegacyManual {
		return nil
	}

	userDefaults, created, err := ensureV16UserDefaultsObject(raw)
	if err != nil {
		return err
	}
	if hasLegacyClose {
		warnDeprecatedConfigKey(legacyUserCloseDefaultsKey, "user_defaults.close / user_defaults.regime_atr")
		closeSection, regimeATR, regimePresent, err := splitLegacyUserCloseDefaults(closeRaw)
		if err != nil {
			return err
		}
		if len(closeSection) > 0 {
			if err := setV16UserDefaultSection(userDefaults, "close", closeSection, legacyUserCloseDefaultsKey); err != nil {
				return err
			}
		}
		if regimePresent {
			if err := setV16UserDefaultSection(userDefaults, "regime_atr", regimeATR, legacyUserCloseDefaultsKey+".regime_atr"); err != nil {
				return err
			}
		}
		delete(raw, legacyUserCloseDefaultsKey)
	}
	if hasLegacyManual {
		warnDeprecatedConfigKey(legacyManualDefaultsKey, "user_defaults.manual")
		manualSection, present, err := legacyJSONSectionObject(manualRaw, legacyManualDefaultsKey)
		if err != nil {
			return err
		}
		if present && len(manualSection) > 0 {
			if err := setV16UserDefaultSection(userDefaults, "manual", manualSection, legacyManualDefaultsKey); err != nil {
				return err
			}
		}
		delete(raw, legacyManualDefaultsKey)
	}
	if created && len(userDefaults) == 0 {
		delete(raw, "user_defaults")
	}
	return nil
}

func ensureV16UserDefaultsObject(raw map[string]interface{}) (map[string]interface{}, bool, error) {
	current, ok := raw["user_defaults"]
	if !ok || current == nil {
		userDefaults := make(map[string]interface{})
		raw["user_defaults"] = userDefaults
		return userDefaults, true, nil
	}
	userDefaults, ok := current.(map[string]interface{})
	if !ok {
		return nil, false, fmt.Errorf("user_defaults must be an object to merge deprecated %s/%s aliases", legacyUserCloseDefaultsKey, legacyManualDefaultsKey)
	}
	return userDefaults, false, nil
}

func splitLegacyUserCloseDefaults(raw interface{}) (map[string]interface{}, interface{}, bool, error) {
	legacy, present, err := legacyJSONSectionObject(raw, legacyUserCloseDefaultsKey)
	if err != nil || !present {
		return nil, nil, false, err
	}
	closeSection := make(map[string]interface{})
	var regimeATR interface{}
	regimePresent := false
	for key, value := range legacy {
		norm := strings.ToLower(strings.TrimSpace(key))
		if norm == "" {
			norm = key
		}
		if norm == userCloseDefaultRegimeATRKey {
			if regimePresent && !jsonEquivalent(regimeATR, value) {
				return nil, nil, false, fmt.Errorf("%s contains conflicting regime_atr entries", legacyUserCloseDefaultsKey)
			}
			regimeATR = value
			regimePresent = true
			continue
		}
		if existing, ok := closeSection[norm]; ok && !jsonEquivalent(existing, value) {
			return nil, nil, false, fmt.Errorf("%s contains conflicting %q close-default entries", legacyUserCloseDefaultsKey, norm)
		}
		closeSection[norm] = value
	}
	return closeSection, regimeATR, regimePresent, nil
}

func legacyJSONSectionObject(raw interface{}, legacyKey string) (map[string]interface{}, bool, error) {
	if raw == nil {
		return nil, false, nil
	}
	obj, ok := raw.(map[string]interface{})
	if !ok {
		return nil, false, fmt.Errorf("%s must be an object", legacyKey)
	}
	return obj, true, nil
}

func setV16UserDefaultSection(userDefaults map[string]interface{}, section string, legacyValue interface{}, legacyKey string) error {
	if userDefaults == nil {
		return nil
	}
	if existing, ok := userDefaults[section]; ok && existing != nil {
		if !jsonEquivalent(existing, legacyValue) {
			return fmt.Errorf("user_defaults.%s conflicts with deprecated %s; keep one canonical value", section, legacyKey)
		}
		return nil
	}
	userDefaults[section] = legacyValue
	return nil
}

func jsonEquivalent(a, b interface{}) bool {
	aj, err := json.Marshal(a)
	if err != nil {
		return false
	}
	bj, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return bytes.Equal(aj, bj)
}

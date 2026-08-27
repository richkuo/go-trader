package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

const CurrentConfigVersion = 17

const MinSupportedConfigVersion = 13

func errUnsupportedConfigVersion(ver int) error {
	return fmt.Errorf("config_version %d is no longer supported (minimum %d, current %d): the pre-v%d migration handlers were removed in #1285. Load this config once with an older go-trader build that still ships the full migration ladder — e.g. the pre-update binary preserved as ./go-trader.prev by scripts/update.sh — then restart the current binary",
		ver, MinSupportedConfigVersion, CurrentConfigVersion, MinSupportedConfigVersion)
}

var v7DMTranslationKeys = []string{"dm_paper_trades", "dm_live_trades"}

func errVersionlessRemovedTranslationKey(key string) error {
	return fmt.Errorf("config has no config_version stamp but still carries the legacy key %q, whose v7 migration into the dm_channels map was removed in #1285. A version-less config is treated as current-shape and no longer runs that translation, so DM trade-alert routing would be silently lost. Replace it with the current discord/telegram dm_channels map, or load this config once with an older go-trader build that still ships the full migration ladder — e.g. the pre-update binary preserved as ./go-trader.prev by scripts/update.sh — then restart the current binary",
		key)
}

func versionlessConfigRemovedTranslationKey(data []byte) (string, bool) {
	var meta struct {
		ConfigVersion int                        `json:"config_version"`
		Discord       map[string]json.RawMessage `json:"discord"`
		Telegram      map[string]json.RawMessage `json:"telegram"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return "", false
	}
	if meta.ConfigVersion != 0 {
		return "", false
	}
	for _, section := range []struct {
		name string
		raw  map[string]json.RawMessage
	}{
		{"discord", meta.Discord},
		{"telegram", meta.Telegram},
	} {
		for _, key := range v7DMTranslationKeys {
			if _, present := section.raw[key]; present {
				return section.name + "." + key, true
			}
		}
	}
	return "", false
}

func checkRawConfigVersionSupported(data []byte) error {
	var meta struct {
		ConfigVersion int `json:"config_version"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil
	}
	if meta.ConfigVersion != 0 && meta.ConfigVersion < MinSupportedConfigVersion {
		return errUnsupportedConfigVersion(meta.ConfigVersion)
	}
	if key, found := versionlessConfigRemovedTranslationKey(data); found {
		return errVersionlessRemovedTranslationKey(key)
	}
	return nil
}

type ConfigField struct {
	Version     int
	JSONPath    string
	Description string
	Default     string
	FieldType   string
}

var configFieldRegistry = []ConfigField{}

const v14DeprecationNotice = "**Note:** perps configs now use `direction: \"long\"|\"short\"|\"both\"` instead of `allow_shorts`. " +
	"Migration converts `allow_shorts: false` → `direction: \"long\"` and `allow_shorts: true` → `direction: \"both\"`. " +
	"The new `\"short\"` value lets you run a bidirectional strategy as a dedicated short-only instrument " +
	"(useful with `allowed_regimes: [\"trending_down\"]`). See issue #656."

const v15DeprecationNotice = "**Note:** close-strategy params now use canonical keys (#841). " +
	"Migration rewrites on disk: `tiers`→`tp_tiers`, `atr`/`multiple`→`atr_multiple`, " +
	"`fraction`→`close_fraction`, tier `pct`→`profit_pct`, `tp_at_pct`→single-tier `tiered_tp_pct`, " +
	"and legacy tier-keyed `tiered_tp_atr_regime` blocks→unified top-level `trend_regime` " +
	"(with per-label `stop_loss_atr` + `tp_tiers`). See issue #841."

const v17ATRMethodNotice = "**Note:** ATR smoothing is now configurable (#1277). " +
	"`atr_method: \"wilder\"` (global, or per-strategy) switches the standard-ATR surface " +
	"(entry-ATR stamping, live close-evaluator ATR, manual fetch-atr) from the legacy simple " +
	"rolling mean to the published Wilder RMA and drops the >=100 integer rounding. " +
	"Default (\"simple\") is byte-identical to previous behavior. " +
	"SIGHUP hot-reload refuses an effective-method switch while the strategy holds an open " +
	"position — flatten first, or wait until flat before restarting with the change: a full " +
	"process restart has no prior config to diff against, so it still adopts a changed " +
	"atr_method for a position that stayed open, re-basing any live-recomputed close evaluator " +
	"(tiered_tp_atr_live, atr_stop/avwap_stop with atr_source=live) mid-flight even though " +
	"frozen entry-ATR and on-chain protection are unaffected. A startup check DMs the owner if " +
	"this happens. " +
	"Backtest baselines were established under simple; re-validate before promoting Wilder-based results."

func NewFieldsSince(version int) []ConfigField {
	var fields []ConfigField
	for _, f := range configFieldRegistry {
		if f.Version > version {
			fields = append(fields, f)
		}
	}
	return fields
}

func MigrateConfig(configPath string, fieldValues map[string]string, cfg *Config) error {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}

	oldVer := 0
	if v, ok := raw["config_version"].(float64); ok {
		oldVer = int(v)
	}

	if oldVer != 0 && oldVer < MinSupportedConfigVersion {
		return errUnsupportedConfigVersion(oldVer)
	}

	if oldVer == 0 {
		if key, found := versionlessConfigRemovedTranslationKey(data); found {
			return errVersionlessRemovedTranslationKey(key)
		}
	}

	for path, value := range fieldValues {
		setNestedField(raw, path, value)
	}

	if oldVer < 13 {
		migrateV13StrategyShape(raw)
	}

	if oldVer < 14 {
		migrateV14Direction(raw)
	}

	if oldVer < 15 {
		migrateV15CloseKeys(raw)
	}

	if oldVer < 16 || hasLegacyUserDefaultAliases(raw) {
		if err := migrateV16UserDefaults(raw); err != nil {
			return err
		}
	}

	raw["config_version"] = CurrentConfigVersion

	newData, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	tmpPath := configPath + ".tmp"
	if err := os.WriteFile(tmpPath, newData, 0600); err != nil {
		return fmt.Errorf("write tmp: %w", err)
	}
	return os.Rename(tmpPath, configPath)
}

func setNestedField(obj map[string]interface{}, path string, value string) {
	parts := strings.SplitN(path, ".", 2)
	if len(parts) == 1 {
		obj[parts[0]] = value
		return
	}
	nested, ok := obj[parts[0]].(map[string]interface{})
	if !ok {
		nested = make(map[string]interface{})
		obj[parts[0]] = nested
	}
	setNestedField(nested, parts[1], value)
}

func jsonBoolish(v interface{}) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return strings.EqualFold(strings.TrimSpace(t), "true") || strings.TrimSpace(t) == "1"
	case float64:
		return t != 0
	default:
		return false
	}
}

func stringFromJSON(v interface{}) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	return strings.TrimSpace(fmt.Sprint(v))
}

func strictStringFromJSON(v interface{}) string {
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(s)
}

func cloneOrNewJSONMap(v interface{}) map[string]interface{} {
	out := make(map[string]interface{})
	if m, ok := v.(map[string]interface{}); ok {
		for k, val := range m {
			out[k] = val
		}
	}
	return out
}

func runConfigMigrationDM(cfg *Config, notifier *MultiNotifier, configPath string) {
	fields := NewFieldsSince(cfg.ConfigVersion)

	if len(fields) == 0 {

		if err := MigrateConfig(configPath, nil, cfg); err != nil {
			fmt.Printf("[migration] Failed to bump config version: %v\n", err)
		}

		if cfg.ConfigVersion < 14 {
			if notifier != nil && notifier.HasOwner() {
				notifier.SendOwnerDM(v14DeprecationNotice)
			} else {
				fmt.Printf("[migration] %s\n", v14DeprecationNotice)
			}
		}

		if cfg.ConfigVersion < 17 {
			if notifier != nil && notifier.HasOwner() {
				notifier.SendOwnerDM(v17ATRMethodNotice)
			} else {
				fmt.Printf("[migration] %s\n", v17ATRMethodNotice)
			}
		}
		return
	}

	values := make(map[string]string)

	if notifier == nil || !notifier.HasOwner() {

		fmt.Printf("[migration] %d new config field(s) — applying defaults (no DM configured)\n", len(fields))
		for _, f := range fields {
			if f.Default != "" {
				values[f.JSONPath] = f.Default
			}
		}
		if err := MigrateConfig(configPath, values, cfg); err != nil {
			fmt.Printf("[migration] Failed to migrate config: %v\n", err)
		}
		if cfg.ConfigVersion < 14 {
			fmt.Printf("[migration] %s\n", v14DeprecationNotice)
		}
		if cfg.ConfigVersion < 15 {
			fmt.Printf("[migration] %s\n", v15DeprecationNotice)
		}
		if cfg.ConfigVersion < 17 {
			fmt.Printf("[migration] %s\n", v17ATRMethodNotice)
		}
		return
	}

	intro := fmt.Sprintf("**go-trader upgraded!** %d new config field(s) to set.", len(fields))
	notifier.SendOwnerDM(intro)

	for _, f := range fields {
		defaultHint := "none"
		if f.Default != "" {
			defaultHint = f.Default
		}
		prompt := fmt.Sprintf("**%s** — %s\nDefault: `%s`\nReply with a value, or `default` to use the default:", f.JSONPath, f.Description, defaultHint)
		resp, err := notifier.AskOwnerDM(prompt, 10*time.Minute)
		if err != nil || strings.EqualFold(strings.TrimSpace(resp), "default") || resp == "" {
			if f.Default != "" {
				values[f.JSONPath] = f.Default
			}
		} else {
			values[f.JSONPath] = strings.TrimSpace(resp)
		}
	}

	if err := MigrateConfig(configPath, values, cfg); err != nil {
		notifier.SendOwnerDM(fmt.Sprintf("**Migration failed**: %v", err))
		return
	}

	notifier.SendOwnerDM("Config updated. Changes take effect next restart.")

	if cfg.ConfigVersion < 14 {
		notifier.SendOwnerDM(v14DeprecationNotice)
	}

	if cfg.ConfigVersion < 15 {
		notifier.SendOwnerDM(v15DeprecationNotice)
	}

	if cfg.ConfigVersion < 17 {
		notifier.SendOwnerDM(v17ATRMethodNotice)
	}
}

func needsV13SchemaMigration(data []byte) bool {
	var meta struct {
		ConfigVersion int `json:"config_version"`
	}
	if err := json.Unmarshal(data, &meta); err == nil && meta.ConfigVersion >= 13 {
		return false
	}
	return true
}

var closeStrategyOwnedKeys = map[string]map[string]struct{}{
	"tiered_tp_atr":      {"tp_tiers": {}, "tiers": {}},
	"tiered_tp_atr_live": {"tp_tiers": {}, "tiers": {}, "atr_source": {}},

	"tiered_tp_atr_regime":              {"tp_tiers": {}, "tiers": {}, "use_defaults": {}, "sl_after": {}},
	"tiered_tp_atr_live_regime":         {"tp_tiers": {}, "tiers": {}, "use_defaults": {}, "atr_source": {}, "sl_after": {}},
	"tiered_tp_atr_live_regime_dynamic": {"trend_regime": {}, "atr_source": {}, "regime_confirm_cycles": {}},
	"trailing_tp_ratchet":               {"tp_tiers": {}, "use_defaults": {}},
	"trailing_tp_ratchet_regime":        {"tp_tiers": {}, "use_defaults": {}},
	"tiered_tp_pct":                     {"tp_tiers": {}, "tiers": {}},
	"tp_at_pct":                         {"pct": {}},

	"time_stop":     {"max_bars": {}},
	"atr_stop":      {"atr_mult": {}, "atr_source": {}},
	"zscore_target": {"lookback": {}, "z_target": {}},

	"avwap_stop": {"buffer_atr_mult": {}, "atr_source": {}},
}

func migrateV14Direction(raw map[string]interface{}) {
	strategies, ok := raw["strategies"].([]interface{})
	if !ok {
		return
	}
	for _, item := range strategies {
		sc, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		legacyAllow, hadLegacy := sc["allow_shorts"]

		delete(sc, "allow_shorts")
		if !hadLegacy {
			continue
		}

		switch stringFromJSON(sc["type"]) {
		case "perps", "manual":
		default:
			continue
		}

		if existing := strictStringFromJSON(sc["direction"]); existing != "" {
			continue
		}
		if jsonBoolish(legacyAllow) {
			sc["direction"] = "both"
		} else {
			sc["direction"] = "long"
		}
	}
}

func migrateV13StrategyShape(raw map[string]interface{}) {
	strategies, ok := raw["strategies"].([]interface{})
	if !ok {
		return
	}
	for _, item := range strategies {
		sc, ok := item.(map[string]interface{})
		if !ok {
			continue
		}

		legacyOpen := strictStringFromJSON(sc["open_strategy"])
		legacyClosesRaw, _ := sc["close_strategies"].([]interface{})
		legacyParams := cloneOrNewJSONMap(sc["params"])

		openName := legacyOpen
		if openName == "" {
			if argsList, ok := sc["args"].([]interface{}); ok && len(argsList) > 0 {
				openName = strictStringFromJSON(argsList[0])
			}
		}
		if openName == "" && strictStringFromJSON(sc["type"]) == "manual" {
			openName = "hold"
		}

		closeRefs := make([]interface{}, 0, len(legacyClosesRaw))
		for _, entry := range legacyClosesRaw {
			name := strictStringFromJSON(entry)
			if name == "" {
				continue
			}
			ref := map[string]interface{}{"name": name}
			if owned, ok := closeStrategyOwnedKeys[name]; ok {
				params := map[string]interface{}{}
				for key := range owned {
					if val, present := legacyParams[key]; present {
						params[key] = val
						delete(legacyParams, key)
					}
				}
				if len(params) > 0 {
					ref["params"] = params
				}
			}
			closeRefs = append(closeRefs, ref)
		}

		openRef := map[string]interface{}{"name": openName}
		if len(legacyParams) > 0 {
			openRef["params"] = legacyParams
		}

		sc["open_strategy"] = openRef
		if len(closeRefs) > 0 {
			sc["close_strategies"] = closeRefs
		} else {
			delete(sc, "close_strategies")
		}
		delete(sc, "params")
	}
}

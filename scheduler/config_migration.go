package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// CurrentConfigVersion is the version embedded in newly generated configs.
// When the binary starts and cfg.ConfigVersion < CurrentConfigVersion, migration runs.
const CurrentConfigVersion = 8

// ConfigField describes a config field introduced in a specific version.
type ConfigField struct {
	Version     int    // version this field was added
	JSONPath    string // dot-separated path in the config JSON (e.g. "discord.owner_id")
	Description string // human-readable description shown to user in DM
	Default     string // default value (empty string = no default / user must set manually)
	FieldType   string // "string", "bool", "int", "float"
}

// configFieldRegistry lists all fields added since v1.
var configFieldRegistry = []ConfigField{
	{
		Version:     2,
		JSONPath:    "discord.owner_id",
		Description: "Your Discord user ID (for upgrade DMs and config prompts). Right-click your username in Discord → Copy User ID.",
		Default:     "",
		FieldType:   "string",
	},
	{
		Version:     3,
		JSONPath:    "portfolio_risk.warn_threshold_pct",
		Description: "Percentage of max_drawdown_pct at which to send a warning alert (e.g. 80 means warn at 80% of the kill switch threshold).",
		Default:     "80",
		FieldType:   "float",
	},
}

// v6DeprecatedFields lists fields removed in v6 (channel boolean routing replaced by
// <platform>-paper channel key convention). These are cleaned up during migration.
var v6DeprecatedFields = []string{
	"discord.channel_live_trades",
	"discord.channel_paper_trades",
	"telegram.channel_live_trades",
	"telegram.channel_paper_trades",
}

const v6DeprecationNotice = "**Note:** `channel_paper_trades` and `channel_live_trades` have been removed. " +
	"Channel trade alerts are now controlled by channel presence: add `\"<platform>-paper\"` keys to " +
	"your channels map for paper-specific routing. See issue #247."

// v7DeprecatedFields lists fields removed in v7 (replaced by dm_channels map). Cleaned during migration.
var v7DeprecatedFields = []string{
	"discord.dm_paper_trades",
	"discord.dm_live_trades",
	"telegram.dm_paper_trades",
	"telegram.dm_live_trades",
}

// v8DeprecatedFields lists fields removed in v8 (replaced by top-level
// summary_frequency map, #30). The old spot_summary_freq / options_summary_freq
// fields under discord were never wired to the main loop.
var v8DeprecatedFields = []string{
	"discord.spot_summary_freq",
	"discord.options_summary_freq",
}

const v8DeprecationNotice = "**Note:** `discord.spot_summary_freq` and `discord.options_summary_freq` have been replaced by " +
	"a top-level `summary_frequency` map keyed by channel (e.g. `\"spot\": \"hourly\"`, `\"options\": \"every\"`). " +
	"Values may be `\"every\"`/`\"per_check\"`/`\"always\"` (every cycle), `\"hourly\"` (1h), `\"daily\"` (24h), or any Go duration like `\"30m\"`. " +
	"Empty/missing falls back to legacy defaults (options/perps/futures every cycle, spot hourly). See issue #30."

const v7DeprecationNotice = "**Note:** `dm_paper_trades` and `dm_live_trades` have been replaced by a `dm_channels` map. " +
	"Paper trades use `dm_channels[\"<platform>-paper\"]`; live trades use `dm_channels[\"<platform>\"]`. " +
	"Absent keys disable DM-style trade alerts for that platform. Values may be a user ID (delivered as a DM) " +
	"or a channel/chat ID (delivered as a channel message) — the name \"dm_channels\" reflects the fallback behavior, " +
	"not a restriction. Migration populates `dm_channels` only for platforms currently configured; new platforms " +
	"added later will not auto-enroll — add an entry manually. See issue #248."

// NewFieldsSince returns all ConfigFields added after the given version number.
func NewFieldsSince(version int) []ConfigField {
	var fields []ConfigField
	for _, f := range configFieldRegistry {
		if f.Version > version {
			fields = append(fields, f)
		}
	}
	return fields
}

// MigrateConfig loads the config as a raw JSON map, applies fieldValues at dot-paths,
// removes deprecated fields, bumps config_version to CurrentConfigVersion, and writes back atomically.
// cfg is optional; when non-nil it is used to translate pre-v7 dm_* booleans into dm_channels per strategy platform.
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

	for path, value := range fieldValues {
		setNestedField(raw, path, value)
	}

	// v6: remove deprecated channel boolean fields (replaced by <platform>-paper convention).
	if oldVer < 6 {
		for _, path := range v6DeprecatedFields {
			removeNestedField(raw, path)
		}
	}

	// v7: translate dm_paper_trades / dm_live_trades into dm_channels, then remove deprecated booleans.
	if oldVer < 7 {
		translateV7DMChannels(raw, cfg)
		for _, path := range v7DeprecatedFields {
			removeNestedField(raw, path)
		}
	}

	// v8: remove dead discord.spot_summary_freq / options_summary_freq (#30).
	// No translation — the old fields were never wired to the main loop, so
	// silently dropping them changes no runtime behavior. Users opting in to
	// per-channel cadence populate the top-level summary_frequency map.
	if oldVer < 8 {
		for _, path := range v8DeprecatedFields {
			removeNestedField(raw, path)
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

// setNestedField sets a value at a dot-path in a nested map[string]interface{}.
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

// removeNestedField removes a field at a dot-path from a nested map[string]interface{}.
func removeNestedField(obj map[string]interface{}, path string) {
	parts := strings.SplitN(path, ".", 2)
	if len(parts) == 1 {
		delete(obj, parts[0])
		return
	}
	nested, ok := obj[parts[0]].(map[string]interface{})
	if !ok {
		return
	}
	removeNestedField(nested, parts[1])
}

// translateV7DMChannels merges legacy dm_paper_trades / dm_live_trades booleans into dm_channels (#248).
func translateV7DMChannels(raw map[string]interface{}, cfg *Config) {
	if cfg == nil {
		return
	}
	platforms := collectUniquePlatforms(cfg)
	translateSection := func(section, ownerKey string) {
		sec, ok := raw[section].(map[string]interface{})
		if !ok {
			return
		}
		liveB := jsonBoolish(sec["dm_live_trades"])
		paperB := jsonBoolish(sec["dm_paper_trades"])
		if !liveB && !paperB {
			return
		}
		owner := stringFromJSON(sec[ownerKey])
		if owner == "" {
			fmt.Printf("[migration] %s: dm_paper_trades/dm_live_trades enabled but %s is empty — cannot populate dm_channels\n", section, ownerKey)
			return
		}
		if len(platforms) == 0 {
			fmt.Printf("[migration] %s: dm trade booleans set but no strategies found — add dm_channels manually\n", section)
			return
		}
		dm := cloneOrNewJSONMap(sec["dm_channels"])
		for _, p := range platforms {
			if paperB {
				k := p + "-paper"
				if _, exists := dm[k]; !exists {
					dm[k] = owner
				}
			}
			if liveB {
				if _, exists := dm[p]; !exists {
					dm[p] = owner
				}
			}
		}
		sec["dm_channels"] = dm
	}
	translateSection("discord", "owner_id")
	translateSection("telegram", "owner_chat_id")
}

func collectUniquePlatforms(cfg *Config) []string {
	seen := make(map[string]bool)
	var out []string
	for _, sc := range cfg.Strategies {
		p := strings.TrimSpace(sc.Platform)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
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

// cloneOrNewJSONMap returns a shallow copy of v if it is a JSON object (map[string]interface{}),
// or a fresh empty map otherwise. Values remain interface{} — this is not a string-keyed string map.
func cloneOrNewJSONMap(v interface{}) map[string]interface{} {
	out := make(map[string]interface{})
	if m, ok := v.(map[string]interface{}); ok {
		for k, val := range m {
			out[k] = val
		}
	}
	return out
}

// runConfigMigrationDM prompts the owner via DM for any new config fields introduced
// since cfg.ConfigVersion. Falls back to applying defaults silently when DM is unavailable.
func runConfigMigrationDM(cfg *Config, notifier *MultiNotifier, configPath string) {
	fields := NewFieldsSince(cfg.ConfigVersion)

	if len(fields) == 0 {
		// Already at current version — just bump silently.
		if err := MigrateConfig(configPath, nil, cfg); err != nil {
			fmt.Printf("[migration] Failed to bump config version: %v\n", err)
		}
		// v6: notify about deprecated channel boolean removal.
		if cfg.ConfigVersion < 6 {
			if notifier != nil && notifier.HasOwner() {
				notifier.SendOwnerDM(v6DeprecationNotice)
			} else {
				fmt.Printf("[migration] %s\n", v6DeprecationNotice)
			}
		}
		// v7: notify about dm_channels migration (#248).
		if cfg.ConfigVersion < 7 {
			if notifier != nil && notifier.HasOwner() {
				notifier.SendOwnerDM(v7DeprecationNotice)
			} else {
				fmt.Printf("[migration] %s\n", v7DeprecationNotice)
			}
		}
		// v8: notify about summary_frequency migration (#30).
		if cfg.ConfigVersion < 8 {
			if notifier != nil && notifier.HasOwner() {
				notifier.SendOwnerDM(v8DeprecationNotice)
			} else {
				fmt.Printf("[migration] %s\n", v8DeprecationNotice)
			}
		}
		return
	}

	values := make(map[string]string)

	if notifier == nil || !notifier.HasOwner() {
		// No DM capability — apply defaults and bump version.
		fmt.Printf("[migration] %d new config field(s) — applying defaults (no DM configured)\n", len(fields))
		for _, f := range fields {
			if f.Default != "" {
				values[f.JSONPath] = f.Default
			}
		}
		if err := MigrateConfig(configPath, values, cfg); err != nil {
			fmt.Printf("[migration] Failed to migrate config: %v\n", err)
		}
		if cfg.ConfigVersion < 6 {
			fmt.Printf("[migration] %s\n", v6DeprecationNotice)
		}
		if cfg.ConfigVersion < 7 {
			fmt.Printf("[migration] %s\n", v7DeprecationNotice)
		}
		if cfg.ConfigVersion < 8 {
			fmt.Printf("[migration] %s\n", v8DeprecationNotice)
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

	// v6: notify about deprecated channel boolean removal.
	if cfg.ConfigVersion < 6 {
		notifier.SendOwnerDM(v6DeprecationNotice)
	}
	// v7: notify about dm_channels migration (#248).
	if cfg.ConfigVersion < 7 {
		notifier.SendOwnerDM(v7DeprecationNotice)
	}
	// v8: notify about summary_frequency migration (#30).
	if cfg.ConfigVersion < 8 {
		notifier.SendOwnerDM(v8DeprecationNotice)
	}
}

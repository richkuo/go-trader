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
const CurrentConfigVersion = 6

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
	{
		Version:     4,
		JSONPath:    "discord.dm_live_trades",
		Description: "Send a DM to the bot owner on every live trade execution (true/false).",
		Default:     "false",
		FieldType:   "bool",
	},
	{
		Version:     4,
		JSONPath:    "discord.dm_paper_trades",
		Description: "Send a DM to the bot owner on every paper trade execution (true/false).",
		Default:     "false",
		FieldType:   "bool",
	},
	{
		Version:     4,
		JSONPath:    "telegram.dm_live_trades",
		Description: "Send a Telegram message on every live trade execution (true/false).",
		Default:     "false",
		FieldType:   "bool",
	},
	{
		Version:     4,
		JSONPath:    "telegram.dm_paper_trades",
		Description: "Send a Telegram message on every paper trade execution (true/false).",
		Default:     "false",
		FieldType:   "bool",
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
func MigrateConfig(configPath string, fieldValues map[string]string) error {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}

	for path, value := range fieldValues {
		setNestedField(raw, path, value)
	}

	// v6: remove deprecated channel boolean fields (replaced by <platform>-paper convention).
	if version, ok := raw["config_version"].(float64); !ok || int(version) < 6 {
		for _, path := range v6DeprecatedFields {
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

// runConfigMigrationDM prompts the owner via DM for any new config fields introduced
// since cfg.ConfigVersion. Falls back to applying defaults silently when DM is unavailable.
func runConfigMigrationDM(cfg *Config, notifier *MultiNotifier, configPath string) {
	fields := NewFieldsSince(cfg.ConfigVersion)

	if len(fields) == 0 {
		// Already at current version — just bump silently.
		if err := MigrateConfig(configPath, nil); err != nil {
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
		if err := MigrateConfig(configPath, values); err != nil {
			fmt.Printf("[migration] Failed to migrate config: %v\n", err)
		}
		if cfg.ConfigVersion < 6 {
			fmt.Printf("[migration] %s\n", v6DeprecationNotice)
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

	if err := MigrateConfig(configPath, values); err != nil {
		notifier.SendOwnerDM(fmt.Sprintf("**Migration failed**: %v", err))
		return
	}

	notifier.SendOwnerDM("Config updated. Changes take effect next restart.")

	// v6: notify about deprecated channel boolean removal.
	if cfg.ConfigVersion < 6 {
		notifier.SendOwnerDM(v6DeprecationNotice)
	}
}

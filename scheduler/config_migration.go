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
const CurrentConfigVersion = 2

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
}

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
// bumps config_version to CurrentConfigVersion, and writes back atomically.
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

// runConfigMigrationDM prompts the owner via DM for any new config fields introduced
// since cfg.ConfigVersion. Falls back to applying defaults silently when DM is unavailable.
func runConfigMigrationDM(cfg *Config, discord *DiscordNotifier, configPath string) {
	fields := NewFieldsSince(cfg.ConfigVersion)

	if len(fields) == 0 {
		// Already at current version — just bump silently.
		if err := MigrateConfig(configPath, nil); err != nil {
			fmt.Printf("[migration] Failed to bump config version: %v\n", err)
		}
		return
	}

	values := make(map[string]string)

	if discord == nil || cfg.Discord.OwnerID == "" {
		// No DM capability — apply defaults and bump version.
		fmt.Printf("[migration] %d new config field(s) — applying defaults (no Discord DM configured)\n", len(fields))
		for _, f := range fields {
			if f.Default != "" {
				values[f.JSONPath] = f.Default
			}
		}
		if err := MigrateConfig(configPath, values); err != nil {
			fmt.Printf("[migration] Failed to migrate config: %v\n", err)
		}
		return
	}

	intro := fmt.Sprintf("**go-trader upgraded!** %d new config field(s) to set.", len(fields))
	if err := discord.SendDM(cfg.Discord.OwnerID, intro); err != nil {
		fmt.Printf("[migration] Failed to send intro DM: %v\n", err)
		return
	}

	for _, f := range fields {
		defaultHint := "none"
		if f.Default != "" {
			defaultHint = f.Default
		}
		prompt := fmt.Sprintf("**%s** — %s\nDefault: `%s`\nReply with a value, or `default` to use the default:", f.JSONPath, f.Description, defaultHint)
		resp, err := discord.AskDM(cfg.Discord.OwnerID, prompt, 10*time.Minute)
		if err != nil || strings.EqualFold(strings.TrimSpace(resp), "default") || resp == "" {
			if f.Default != "" {
				values[f.JSONPath] = f.Default
			}
		} else {
			values[f.JSONPath] = strings.TrimSpace(resp)
		}
	}

	if err := MigrateConfig(configPath, values); err != nil {
		_ = discord.SendDM(cfg.Discord.OwnerID, fmt.Sprintf("**Migration failed**: %v", err))
		return
	}

	_ = discord.SendDM(cfg.Discord.OwnerID, "Config updated. Changes take effect next restart.")
}

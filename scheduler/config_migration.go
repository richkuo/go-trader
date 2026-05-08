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
const CurrentConfigVersion = 14

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
		Description: "Percentage of max_drawdown_pct at which to send a warning alert (e.g. 60 means warn at 60% of the kill switch threshold).",
		Default:     "60",
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

// v9 introduced auto-derivation of HL perps per-trade stop-loss from
// max_drawdown_pct (#484). The field types changed from float64 to *float64
// so omitted (nil) is distinguishable from explicit 0. Behavior change for
// existing configs: a single HL perps strategy on a coin without an explicit
// `stop_loss_pct` or `stop_loss_margin_pct` will now place an exchange-side
// reduce-only trigger on every fresh open (capped at 50%). Same-coin peer
// groups are normalized at LoadConfig time so omitted fields opt out unless
// the operator chooses one explicit stop-loss owner (#494).
const v9DeprecationNotice = "**Note:** HL perps strategies now auto-derive a per-trade stop-loss from `max_drawdown_pct` " +
	"(capped at 50%) when neither `stop_loss_pct` nor `stop_loss_margin_pct` is set and the strategy is the only " +
	"HL perps strategy for that coin. Same-coin peer groups keep omitted fields as an exchange-side stop opt-out; " +
	"choose one explicit positive stop-loss owner if you want a shared-position trigger. See issues #484 and #494."

const v10DeprecationNotice = "**Note:** perps configs now distinguish `sizing_leverage` from exchange `leverage`. " +
	"Migration copies existing perps `leverage` into `sizing_leverage` so old configs keep identical order sizing; " +
	"`leverage` now represents the exchange leverage used for margin drawdown and HL `update_leverage`. See issue #497."

// v14 replaces the legacy `allow_shorts: bool` field with `direction: "long"|"short"|"both"`,
// unlocking dedicated short-only strategies (#656). Migration translates
// allow_shorts=false→"long", allow_shorts=true→"both", deletes the old key.
const v14DeprecationNotice = "**Note:** perps configs now use `direction: \"long\"|\"short\"|\"both\"` instead of `allow_shorts`. " +
	"Migration converts `allow_shorts: false` → `direction: \"long\"` and `allow_shorts: true` → `direction: \"both\"`. " +
	"The new `\"short\"` value lets you run a bidirectional strategy as a dedicated short-only instrument " +
	"(useful with `allowed_regimes: [\"trending_down\"]`). See issue #656."

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

	// v10: split position-sizing leverage from exchange leverage (#497). Copy
	// existing perps leverage to sizing_leverage so legacy configs keep the
	// exact same notional sizing after `leverage` becomes exchange/risk leverage.
	if oldVer < 10 {
		addV10SizingLeverage(raw)
	}

	// v12: expose the hardcoded HL default ATR stop multiplier as a top-level
	// config knob (#605). Existing behavior remains 1.0 unless the operator
	// edits default_stop_loss_atr_mult.
	if oldVer < 12 {
		if _, ok := raw["default_stop_loss_atr_mult"]; !ok {
			raw["default_stop_loss_atr_mult"] = DefaultStopLossATRMult
		}
	}

	// v13: co-locate strategy name + params via StrategyRef (#640). Three
	// type-changes happen in one pass on each strategy entry:
	//   - open_strategy: string  → {name, params}
	//   - close_strategies: []string → [{name, params}, ...]
	//   - params: flat map → split between open ref params and per-close ref params
	// Legacy keys claimed by a close registry's default_params (see
	// closeStrategyOwnedKeys) move into that close ref; everything else stays
	// on the open ref. Any remaining empty `params` field is removed.
	if oldVer < 13 {
		migrateV13StrategyShape(raw)
	}

	// v14: convert legacy allow_shorts (bool) to direction (string enum)
	// per #656. Each perps strategy gets `direction: "long"` or
	// `direction: "both"` based on the old boolean; the field is removed.
	if oldVer < 14 {
		migrateV14Direction(raw)
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

func addV10SizingLeverage(raw map[string]interface{}) {
	strategies, ok := raw["strategies"].([]interface{})
	if !ok {
		return
	}
	for _, item := range strategies {
		sc, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		if _, exists := sc["sizing_leverage"]; exists {
			continue
		}
		if stringFromJSON(sc["type"]) != "perps" {
			continue
		}
		if lev, ok := sc["leverage"]; ok {
			sc["sizing_leverage"] = lev
		}
	}
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

// strictStringFromJSON refuses to coerce non-string JSON values. The v13
// migration uses this for fields where a non-string would be a hand-edit
// mistake (e.g. open_strategy already shaped as the post-v13 object); falling
// back to "" lets downstream args[0]/type=manual recovery paths take over
// instead of writing a corrupted "map[name:foo]" name (#640 review #3).
func strictStringFromJSON(v interface{}) string {
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(s)
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
		// v9: notify about HL perps auto-SL fallback to max_drawdown_pct (#484).
		if cfg.ConfigVersion < 9 {
			if notifier != nil && notifier.HasOwner() {
				notifier.SendOwnerDM(v9DeprecationNotice)
			} else {
				fmt.Printf("[migration] %s\n", v9DeprecationNotice)
			}
		}
		// v10: notify about sizing_leverage split (#497).
		if cfg.ConfigVersion < 10 {
			if notifier != nil && notifier.HasOwner() {
				notifier.SendOwnerDM(v10DeprecationNotice)
			} else {
				fmt.Printf("[migration] %s\n", v10DeprecationNotice)
			}
		}
		// v14: notify about allow_shorts → direction migration (#656).
		if cfg.ConfigVersion < 14 {
			if notifier != nil && notifier.HasOwner() {
				notifier.SendOwnerDM(v14DeprecationNotice)
			} else {
				fmt.Printf("[migration] %s\n", v14DeprecationNotice)
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
		if cfg.ConfigVersion < 9 {
			fmt.Printf("[migration] %s\n", v9DeprecationNotice)
		}
		if cfg.ConfigVersion < 10 {
			fmt.Printf("[migration] %s\n", v10DeprecationNotice)
		}
		if cfg.ConfigVersion < 14 {
			fmt.Printf("[migration] %s\n", v14DeprecationNotice)
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
	// v9: notify about HL perps auto-SL fallback to max_drawdown_pct (#484).
	if cfg.ConfigVersion < 9 {
		notifier.SendOwnerDM(v9DeprecationNotice)
	}
	// v10: notify about sizing_leverage split (#497).
	if cfg.ConfigVersion < 10 {
		notifier.SendOwnerDM(v10DeprecationNotice)
	}
	// v14: notify about allow_shorts → direction migration (#656).
	if cfg.ConfigVersion < 14 {
		notifier.SendOwnerDM(v14DeprecationNotice)
	}
}

// needsV13SchemaMigration reports whether the on-disk config still uses the
// pre-v13 flat shape (string open_strategy / []string close_strategies / flat
// params). LoadConfig calls this with the raw bytes BEFORE Unmarshal — once
// the schema is rewritten, the standard unmarshal handles the new shape.
func needsV13SchemaMigration(data []byte) bool {
	var meta struct {
		ConfigVersion int `json:"config_version"`
	}
	if err := json.Unmarshal(data, &meta); err == nil && meta.ConfigVersion >= 13 {
		return false
	}
	return true
}

// closeStrategyOwnedKeys lists the legacy `params` keys that the v13 migration
// should move under the matching close ref's params. Source of truth is the
// Python close registry's `default_params` for each evaluator
// (shared_strategies/close/registry.py). When adding a new close evaluator
// with its own params, mirror its registry default_params keys here so legacy
// configs migrate cleanly. Anything not listed stays on the open ref.
var closeStrategyOwnedKeys = map[string]map[string]struct{}{
	"tiered_tp_atr":      {"tiers": {}},
	"tiered_tp_atr_live": {"tiers": {}, "atr_source": {}},
	"tiered_tp_pct":      {"tiers": {}},
	"tp_at_pct":          {"pct": {}},
}

// migrateV14Direction translates the legacy boolean `allow_shorts` field on
// each strategy into the new `direction` enum (#656). Only perps strategies
// receive the new field; non-perps strategies have `allow_shorts` deleted
// silently if present (it was always meaningless for non-perps types).
//
// Behavior preservation:
//   - allow_shorts: false → direction: "long"
//   - allow_shorts: true  → direction: "both"
//
// If the strategy already has a `direction` field (because the operator
// pre-emptively set it before upgrading), we preserve it and just delete
// the legacy key.
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
		// Always drop the legacy key — it has no v14 semantics.
		delete(sc, "allow_shorts")
		if !hadLegacy {
			continue
		}
		// Skip types that never honored allow_shorts. Both perps and manual
		// run on HL perps and consult Direction/EffectiveDirection, so both
		// must translate; everything else is meaningless and dropped above.
		switch stringFromJSON(sc["type"]) {
		case "perps", "manual":
		default:
			continue
		}
		// If direction is already set explicitly, preserve it.
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

// migrateV13StrategyShape rewrites each strategy in-place from the legacy flat
// shape (open_strategy: string, close_strategies: []string, params: map) to
// the co-located ref shape (open_strategy: {name, params}, close_strategies:
// [{name, params}], no top-level params). See #640 for the design.
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

		// Snapshot legacy fields before we overwrite them. Use strict string
		// coercion so a hand-edited object (e.g. someone shaped the file post-v13
		// without bumping config_version) doesn't get fmt.Sprint'd into a
		// corrupted "map[...]" name (#640 review #3).
		legacyOpen := strictStringFromJSON(sc["open_strategy"])
		legacyClosesRaw, _ := sc["close_strategies"].([]interface{})
		legacyParams := cloneOrNewJSONMap(sc["params"])

		// Resolve open name: prefer legacy open_strategy, else fall back to
		// args[0] so configs that relied on the positional script arg keep
		// working post-migration. type=manual strategies in v12 typically have
		// empty `args` (LoadConfig auto-fills args[0]="hold" *after* unmarshal,
		// but migration runs *before* unmarshal). Default the open name to
		// "hold" for type=manual so the persisted v13 shape isn't an empty
		// name that relies on runtime fallback (#640 review #2).
		openName := legacyOpen
		if openName == "" {
			if argsList, ok := sc["args"].([]interface{}); ok && len(argsList) > 0 {
				openName = strictStringFromJSON(argsList[0])
			}
		}
		if openName == "" && strictStringFromJSON(sc["type"]) == "manual" {
			openName = "hold"
		}

		// Build close refs while moving owned legacy params keys into each ref.
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

		// Whatever legacy params remain belong to the open ref.
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

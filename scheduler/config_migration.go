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
const CurrentConfigVersion = 18

// MinSupportedConfigVersion is the oldest stamped config_version the migration
// ladder can still bring forward (#1285). The v6–v12 rewrite handlers (channel
// booleans, dm_channels translation, summary-freq cleanup, sizing_leverage
// backfill, ATR-stop knob) were deleted after fleet verification, so a config
// stamped below this floor is rejected loudly at load — never partially
// migrated. A config with NO config_version stamp (0) is treated as a
// hand-authored file in the current shape: it takes the v13 shape synthesis +
// v14–v16 passes but NOT the deleted v6–v12 handlers. Most of those handlers
// were inert for a current-shape file — channel/summary-freq keys are ignored
// by the runtime, and sizing_leverage/default_stop_loss_atr_mult are covered by
// runtime fallbacks — so their loss changes nothing. The one exception is the
// v7 dm_paper_trades/dm_live_trades → dm_channels translation, which has no
// runtime substitute: a version-less config still carrying those keys is
// rejected loudly (versionlessConfigRemovedTranslationKey) rather than silently
// losing DM trade-alert routing (#1285 review).
const MinSupportedConfigVersion = 13

// errUnsupportedConfigVersion is the fail-loud rejection for configs stamped
// below MinSupportedConfigVersion. It names the floor and the migration path.
func errUnsupportedConfigVersion(ver int) error {
	return fmt.Errorf("config_version %d is no longer supported (minimum %d, current %d): the pre-v%d migration handlers were removed in #1285. Load this config once with an older go-trader build that still ships the full migration ladder — e.g. the pre-update binary preserved as ./go-trader.prev by scripts/update.sh — then restart the current binary",
		ver, MinSupportedConfigVersion, CurrentConfigVersion, MinSupportedConfigVersion)
}

// v7DMTranslationKeys are the legacy per-section DM-routing booleans (#248) that
// the deleted v7 handler translated into a dm_channels map. Unlike the other
// deleted pre-floor handlers — the v6 channel_* and v8 summary-freq passes only
// dropped inert keys — this translation has NO runtime substitute: a config
// that skips it silently loses DM trade-alert routing. Both the discord and
// telegram sections carried this pair.
var v7DMTranslationKeys = []string{"dm_paper_trades", "dm_live_trades"}

// errVersionlessRemovedTranslationKey rejects a version-less config that still
// carries a v7 DM-routing key whose translation handler was removed in #1285.
// It names the offending key and the same ./go-trader.prev recovery path as the
// stamped-floor rejection.
func errVersionlessRemovedTranslationKey(key string) error {
	return fmt.Errorf("config has no config_version stamp but still carries the legacy key %q, whose v7 migration into the dm_channels map was removed in #1285. A version-less config is treated as current-shape and no longer runs that translation, so DM trade-alert routing would be silently lost. Replace it with the current discord/telegram dm_channels map, or load this config once with an older go-trader build that still ships the full migration ladder — e.g. the pre-update binary preserved as ./go-trader.prev by scripts/update.sh — then restart the current binary",
		key)
}

// versionlessConfigRemovedTranslationKey reports the first pre-floor legacy key
// that a version-less config (no config_version stamp) still carries and whose
// migration handler — deleted in #1285 — performed a runtime-affecting
// translation with no substitute (the v7 dm_* → dm_channels routing). It
// returns ("", false) for a stamped config (the version floor covers those) or
// one carrying no such key. Operates on raw bytes so it runs before any
// migration/rewrite, and a JSON parse error is deferred to the normal parse
// path. Only the v7 dm_* keys qualify — the v6 channel_* / v8 summary-freq keys
// were inert cleanup with no runtime effect and stay accepted (and inert).
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

// checkRawConfigVersionSupported enforces the MinSupportedConfigVersion floor
// on raw config bytes before any migration pass runs. A missing/zero
// config_version is allowed (hand-authored current-shape config), except when
// it still carries a v7 DM-routing key the deleted handlers can no longer
// translate — that is rejected loudly (see versionlessConfigRemovedTranslationKey).
// A JSON parse error is deferred to the normal parse path so its message stays
// primary.
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

// ConfigField describes a config field introduced in a specific version.
type ConfigField struct {
	Version     int    // version this field was added
	JSONPath    string // dot-separated path in the config JSON (e.g. "discord.owner_id")
	Description string // human-readable description shown to user in DM
	Default     string // default value (empty string = no default / user must set manually)
	FieldType   string // "string", "bool", "int", "float"
}

// configFieldRegistry lists all operator-promptable fields added since the
// migration floor. The v2 (discord.owner_id) and v3
// (portfolio_risk.warn_threshold_pct) entries were removed with the #1285
// floor — every supported config (v13+) already went through those prompts.
// Add new entries here when a future version introduces an operator-set field.
var configFieldRegistry = []ConfigField{}

// v14 replaces the legacy `allow_shorts: bool` field with `direction: "long"|"short"|"both"`,
// unlocking dedicated short-only strategies (#656). Migration translates
// allow_shorts=false→"long", allow_shorts=true→"both", deletes the old key.
const v14DeprecationNotice = "**Note:** perps configs now use `direction: \"long\"|\"short\"|\"both\"` instead of `allow_shorts`. " +
	"Migration converts `allow_shorts: false` → `direction: \"long\"` and `allow_shorts: true` → `direction: \"both\"`. " +
	"The new `\"short\"` value lets you run a bidirectional strategy as a dedicated short-only instrument " +
	"(useful with `allowed_regimes: [\"trending_down\"]`). See issue #656."

const v15DeprecationNotice = "**Note:** close-strategy params now use canonical keys (#841). " +
	"Migration rewrites on disk: `tiers`→`tp_tiers`, `atr`/`multiple`→`atr_multiple`, " +
	"`fraction`→`close_fraction`, tier `pct`→`profit_pct`, `tp_at_pct`→single-tier `tiered_tp_pct`, " +
	"and legacy tier-keyed `tiered_tp_atr_regime` blocks→unified top-level `trend_regime` " +
	"(with per-label `stop_loss_atr` + `tp_tiers`). See issue #841."

// v17 introduces the atr_method cutover surface (#1277). Purely additive — no
// on-disk rewrite: an absent field means "simple" (the frozen legacy math), so
// migration only bumps the stamp and informs the operator that the Wilder
// option exists.
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

// v18 introduces the opt-in per-strategy correlated hedge leg surface
// (#1159). Purely additive — no on-disk rewrite: an absent `hedge` block
// means "no hedge" (identical to previous behavior), so migration only
// bumps the stamp and informs the operator that the surface exists.
const v18HedgeNotice = "**Note:** per-strategy correlated hedge legs are now available (#1159). " +
	"An opt-in `hedge` block on an HL perps strategy (direction != \"both\", live mode only) opens a " +
	"second, scheduler-managed inverse position on a different coin alongside every primary open/add, " +
	"sized on notional exposure × `ratio`. The hedge is strictly slaved to the primary lifecycle — no " +
	"independent SL/TP, no check script — and mirrors scale-in, partial/full close, force-close, kill " +
	"switch, and circuit-breaker events, with a per-cycle coherence pass as a reduce-only safety net. " +
	"Config validation rejects a hedge coin that collides with any configured strategy's coin or " +
	"another strategy's hedge coin (HL aggregates per coin per account — hedge coins must be " +
	"sole-owned). If the primary fill confirms but the hedge open fails, the primary is immediately " +
	"reduce-only closed and the owner alerted — hedging never runs unhedged silently. " +
	"SIGHUP hot-reload refuses any hedge-block change while the primary or the hedge leg is open " +
	"(flatten first, or restart after close). The backtester rejects hedge-enabled configs loudly — " +
	"there is no hedge PnL/fee/slippage model yet. Default (no `hedge` block) is unchanged behavior."

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
// Configs stamped below MinSupportedConfigVersion are rejected before any pass
// runs — never partially migrated (#1285).
// cfg is unused since the pre-v7 dm_channels translation was removed (#1285);
// it is retained so future migrations can consult the parsed config. Callers
// may pass nil.
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

	// #1285: fail loudly BEFORE any rewrite when the config predates the
	// migration floor — the sub-floor handlers are gone, so falling through
	// would silently drop their translations and mis-load the config.
	if oldVer != 0 && oldVer < MinSupportedConfigVersion {
		return errUnsupportedConfigVersion(oldVer)
	}
	// #1285: likewise reject a version-less config that still carries a v7
	// DM-routing key — that translation was deleted, so a rewrite would preserve
	// the dead key on disk while the runtime silently ignores it (lost DM
	// routing). Mirrors the loadConfig-level guard in
	// checkRawConfigVersionSupported; reject before any rewrite.
	if oldVer == 0 {
		if key, found := versionlessConfigRemovedTranslationKey(data); found {
			return errVersionlessRemovedTranslationKey(key)
		}
	}

	for path, value := range fieldValues {
		setNestedField(raw, path, value)
	}

	// v13: co-locate strategy name + params via StrategyRef (#640). Three
	// type-changes happen in one pass on each strategy entry:
	//   - open_strategy: string  → {name, params}
	//   - close_strategies: []string → [{name, params}, ...]
	//   - params: flat map → split between open ref params and per-close ref params
	// Legacy keys claimed by a close registry's default_params (see
	// closeStrategyOwnedKeys) move into that close ref; everything else stays
	// on the open ref. Any remaining empty `params` field is removed.
	// Post-#1285 only version-less configs (oldVer==0, hand-authored without a
	// config_version stamp) reach this pass — stamped pre-v13 configs are
	// rejected above.
	if oldVer < 13 {
		migrateV13StrategyShape(raw)
	}

	// v14: convert legacy allow_shorts (bool) to direction (string enum)
	// per #656. Each perps strategy gets `direction: "long"` or
	// `direction: "both"` based on the old boolean; the field is removed.
	if oldVer < 14 {
		migrateV14Direction(raw)
	}

	// v15: canonicalize close-strategy keys (#841): tp_tiers, atr_multiple,
	// unified per-regime block, tp_at_pct → tiered_tp_pct.
	if oldVer < 15 {
		migrateV15CloseKeys(raw)
	}

	// v16: consolidate operator-tunable defaults under user_defaults (#1135).
	// The old top-level aliases are accepted only when they do not conflict
	// with the canonical section they map to.
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
		// v14: notify about allow_shorts → direction migration (#656).
		if cfg.ConfigVersion < 14 {
			if notifier != nil && notifier.HasOwner() {
				notifier.SendOwnerDM(v14DeprecationNotice)
			} else {
				fmt.Printf("[migration] %s\n", v14DeprecationNotice)
			}
		}
		// v17: notify about the atr_method surface (#1277).
		if cfg.ConfigVersion < 17 {
			if notifier != nil && notifier.HasOwner() {
				notifier.SendOwnerDM(v17ATRMethodNotice)
			} else {
				fmt.Printf("[migration] %s\n", v17ATRMethodNotice)
			}
		}
		// v18: notify about the correlated hedge leg surface (#1159).
		if cfg.ConfigVersion < 18 {
			if notifier != nil && notifier.HasOwner() {
				notifier.SendOwnerDM(v18HedgeNotice)
			} else {
				fmt.Printf("[migration] %s\n", v18HedgeNotice)
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
		if cfg.ConfigVersion < 14 {
			fmt.Printf("[migration] %s\n", v14DeprecationNotice)
		}
		if cfg.ConfigVersion < 15 {
			fmt.Printf("[migration] %s\n", v15DeprecationNotice)
		}
		if cfg.ConfigVersion < 17 {
			fmt.Printf("[migration] %s\n", v17ATRMethodNotice)
		}
		if cfg.ConfigVersion < 18 {
			fmt.Printf("[migration] %s\n", v18HedgeNotice)
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

	// v14: notify about allow_shorts → direction migration (#656).
	if cfg.ConfigVersion < 14 {
		notifier.SendOwnerDM(v14DeprecationNotice)
	}
	// v15: notify about close-strategy canonical keys (#841).
	if cfg.ConfigVersion < 15 {
		notifier.SendOwnerDM(v15DeprecationNotice)
	}
	// v17: notify about the atr_method surface (#1277).
	if cfg.ConfigVersion < 17 {
		notifier.SendOwnerDM(v17ATRMethodNotice)
	}
	// v18: notify about the correlated hedge leg surface (#1159).
	if cfg.ConfigVersion < 18 {
		notifier.SendOwnerDM(v18HedgeNotice)
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
	"tiered_tp_atr":      {"tp_tiers": {}, "tiers": {}},
	"tiered_tp_atr_live": {"tp_tiers": {}, "tiers": {}, "atr_source": {}},
	// Regime variants are post-v13 — they have no legacy migration story —
	// but list every operator-facing key here for symmetry so future
	// unknown-key suggestion hints can index off this table without missing
	// the regime evaluator params (review #735.5).
	"tiered_tp_atr_regime":              {"tp_tiers": {}, "tiers": {}, "use_defaults": {}, "sl_after": {}},
	"tiered_tp_atr_live_regime":         {"tp_tiers": {}, "tiers": {}, "use_defaults": {}, "atr_source": {}, "sl_after": {}},
	"tiered_tp_atr_live_regime_dynamic": {"trend_regime": {}, "atr_source": {}, "regime_confirm_cycles": {}},
	"trailing_tp_ratchet":               {"tp_tiers": {}, "use_defaults": {}},
	"trailing_tp_ratchet_regime":        {"tp_tiers": {}, "use_defaults": {}},
	"tiered_tp_pct":                     {"tp_tiers": {}, "tiers": {}},
	"tp_at_pct":                         {"pct": {}}, // v15 migrates to tiered_tp_pct; kept for v13 legacy param routing only
	// #997 M3 exit-quality knobs. No legacy migration story (post-v13); listed
	// for symmetry so unknown-key hints index off this table and the Python
	// registry mirror test (TestCloseStrategyOwnedKeysMirrorsPythonRegistry)
	// stays green. Backtest-wired; live wiring of bars_held/zscore context is
	// deferred (see #997), so a live config using these fails safe (no-op).
	"time_stop":     {"max_bars": {}},
	"atr_stop":      {"atr_mult": {}, "atr_source": {}},
	"zscore_target": {"lookback": {}, "z_target": {}},
	// #1196 AVWAP loss-of-line exit. Post-v13 (no legacy migration story);
	// listed for the Python registry mirror test + unknown-key hints. Virtual
	// exit only — never added to the on-chain TP sets in
	// hyperliquid_protection.go (the line moves every bar; no static trigger).
	"avwap_stop": {"buffer_atr_mult": {}, "atr_source": {}},
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

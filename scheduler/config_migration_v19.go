package main

import (
	"encoding/json"
	"fmt"
	"sort"
)

const (
	v19LegacyStopLossATRRegimeKey   = "stop_loss_atr_regime"
	v19LegacyTrailStopATRRegimeKey  = "trail_stop_atr_regime"
	v19StopLossATRMultRegimeKey     = "stop_loss_atr_mult_regime"
	v19TrailingStopATRMultRegimeKey = "trailing_stop_atr_mult_regime"
)

var v19RenameMap = []struct {
	LegacyKey string
	CanonKey  string
}{
	{LegacyKey: v19LegacyStopLossATRRegimeKey, CanonKey: v19StopLossATRMultRegimeKey},
	{LegacyKey: v19LegacyTrailStopATRRegimeKey, CanonKey: v19TrailingStopATRMultRegimeKey},
}

const v19AtrMultRenameNotice = "**Note:** the per-regime stop fields were renamed (#1475). " +
	"`stop_loss_atr_regime` is now `stop_loss_atr_mult_regime` and `trail_stop_atr_regime` is now " +
	"`trailing_stop_atr_mult_regime`, so each field's name says it holds a per-regime ATR multiplier " +
	"and reads as `<owner>_atr_mult_regime`, the per-regime form of `<owner>_atr_mult`. " +
	"Migration rewrites both keys on disk in strategy blocks, `user_defaults.regime_atr`, " +
	"`user_defaults.manual` and every `user_defaults.close` entry. Runtime behavior is unchanged — " +
	"`effectiveTrailingStopPct` and the fixed ATR stop trigger resolve to the same numbers before " +
	"and after the rename. Where `scheduler/config.json` is still the #1056 transition symlink, " +
	"this on-disk rewrite replaces the symlink with a regular in-tree file; deployments already " +
	"pointed at `/var/lib/go-trader[/<instance>]/config.json` via `ExecStart --config` are unaffected."

func needsV19AtrMultRegimeRename(data []byte) bool {
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return false
	}
	return hasLegacyV19AtrMultRegimeKey(raw)
}

func hasLegacyV19AtrMultRegimeKey(raw map[string]interface{}) bool {
	if raw == nil {
		return false
	}
	if strategies, ok := raw["strategies"].([]interface{}); ok {
		for _, item := range strategies {
			sc, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			for _, pair := range v19RenameMap {
				if _, present := sc[pair.LegacyKey]; present {
					return true
				}
			}
		}
	}
	for _, section := range v18UserDefaultsSections(raw) {
		for _, pair := range v19RenameMap {
			if _, present := section[pair.LegacyKey]; present {
				return true
			}
		}
	}
	return false
}

func migrateV19AtrMultRegimeKey(raw map[string]interface{}) error {
	if raw == nil {
		return nil
	}
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
			if _, err := renameV19AtrMultRegimeKeys(sc, fmt.Sprintf("strategy[%s]", id)); err != nil {
				return err
			}
		}
	}
	for _, section := range v18UserDefaultsSections(raw) {
		if _, err := renameV19AtrMultRegimeKeys(section, "user_defaults"); err != nil {
			return err
		}
	}
	return nil
}

func renameV19AtrMultRegimeKeys(block map[string]interface{}, ctx string) (bool, error) {
	changed := false
	for _, pair := range v19RenameMap {
		legacy, hasLegacy := block[pair.LegacyKey]
		if !hasLegacy {
			continue
		}
		if canonical, hasCanonical := block[pair.CanonKey]; hasCanonical {
			if !jsonEquivalent(canonical, legacy) {
				return false, fmt.Errorf("%s: %q conflicts with %q; keep one canonical block",
					ctx, pair.LegacyKey, pair.CanonKey)
			}
			delete(block, pair.LegacyKey)
			fmt.Printf("[migration] %s: dropped redundant legacy key %q (canonical %q carried an equivalent block)\n", ctx, pair.LegacyKey, pair.CanonKey)
			changed = true
			continue
		}
		block[pair.CanonKey] = legacy
		delete(block, pair.LegacyKey)
		fmt.Printf("[migration] %s: renamed config key %q -> %q\n", ctx, pair.LegacyKey, pair.CanonKey)
		changed = true
	}
	return changed, nil
}

var v19AtomicRenameSites = []string{
	"strategies[]",
	"user_defaults.regime_atr",
	"user_defaults.manual",
	"user_defaults.close[]",
}

func sortedV19RenameMap() []struct {
	LegacyKey string
	CanonKey  string
} {
	out := make([]struct {
		LegacyKey string
		CanonKey  string
	}, len(v19RenameMap))
	copy(out, v19RenameMap)
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].LegacyKey < out[j].LegacyKey
	})
	return out
}

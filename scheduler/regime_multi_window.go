package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

const (
	regimeWindowDefaultKey   = "default"
	regimeWindowReservedName = "regime"
	regimeOhlcvBaseLimit     = 200
	regimeOhlcvMargin        = 10
)

// RegimeSnapshot is one window's latest regime reading from a check script.
type RegimeSnapshot struct {
	Regime  string             `json:"regime"`
	Score   float64            `json:"score"`
	Metrics map[string]float64 `json:"metrics,omitempty"`
}

// RegimePayload holds either a legacy single label or a multi-window map.
// JSON from check scripts is either a string or {"short": {...}, ...}.
type RegimePayload struct {
	Legacy    string
	Windows   map[string]RegimeSnapshot
	MultiMode bool
}

func (p RegimePayload) IsEmpty() bool {
	if p.MultiMode {
		return len(p.Windows) == 0
	}
	return strings.TrimSpace(p.Legacy) == ""
}

// PrimaryLabel returns the display label for status/summary surfaces.
// Legacy mode: the single label. Multi-window: medium window if configured,
// else the smallest bar-count window name.
func (p RegimePayload) PrimaryLabel(rc *RegimeConfig) string {
	if !p.MultiMode {
		return strings.TrimSpace(p.Legacy)
	}
	key := primaryRegimeWindowKey(rc)
	if key != "" {
		if snap, ok := p.Windows[key]; ok {
			return strings.TrimSpace(snap.Regime)
		}
	}
	for _, name := range sortedRegimeWindowNames(p.Windows) {
		if snap, ok := p.Windows[name]; ok {
			return strings.TrimSpace(snap.Regime)
		}
	}
	return ""
}

// Label resolves the regime label for a consumer window key.
// Empty or "default" in legacy mode uses the single label; in multi-window
// mode uses primaryRegimeWindowKey when unset.
func (p RegimePayload) Label(windowKey string, rc *RegimeConfig) string {
	key := normalizeRegimeWindowKey(windowKey)
	if !p.MultiMode {
		return strings.TrimSpace(p.Legacy)
	}
	if key == "" || key == regimeWindowDefaultKey {
		// With explicit regime.windows, the default selector maps to the
		// primary window. Without them, the check script still emits a
		// single-window payload keyed by "default" (regimeWindowsSpecJSON's
		// empty-windows branch), so fall back to that literal key rather than
		// no-op'ing to an empty label — an empty label silently disables both
		// regime_directional_policy and the allowed_regimes gate (#797).
		if regimeMultiWindowEnabled(rc) {
			key = primaryRegimeWindowKey(rc)
		} else {
			key = regimeWindowDefaultKey
		}
	}
	if key == "" {
		return ""
	}
	if snap, ok := p.Windows[key]; ok {
		return strings.TrimSpace(snap.Regime)
	}
	return ""
}

// WindowLabels returns window name -> label for stamping at open.
func (p RegimePayload) WindowLabels() map[string]string {
	if !p.MultiMode {
		label := strings.TrimSpace(p.Legacy)
		if label == "" {
			return nil
		}
		return map[string]string{regimeWindowDefaultKey: label}
	}
	out := make(map[string]string, len(p.Windows))
	for name, snap := range p.Windows {
		if label := strings.TrimSpace(snap.Regime); label != "" {
			out[name] = label
		}
	}
	return out
}

func (p RegimePayload) MarshalJSON() ([]byte, error) {
	if p.MultiMode {
		return json.Marshal(p.Windows)
	}
	return json.Marshal(p.Legacy)
}

func (p *RegimePayload) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || string(data) == "null" {
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		p.Legacy = s
		p.MultiMode = false
		p.Windows = nil
		return nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("regime: expected string or object, got %s", string(data))
	}
	if len(raw) == 0 {
		return nil
	}
	if _, ok := raw["regime"]; ok {
		// Flat legacy snapshot: {"regime":"trending_up","score":...} — not a
		// multi-window map whose sole key happens to be named "regime".
		if _, hasScore := raw["score"]; hasScore {
			var snap RegimeSnapshot
			if err := json.Unmarshal(data, &snap); err != nil {
				return err
			}
			p.Legacy = snap.Regime
			p.MultiMode = false
			p.Windows = nil
			return nil
		}
		if _, hasMetrics := raw["metrics"]; hasMetrics {
			var snap RegimeSnapshot
			if err := json.Unmarshal(data, &snap); err != nil {
				return err
			}
			p.Legacy = snap.Regime
			p.MultiMode = false
			p.Windows = nil
			return nil
		}
		var label string
		if err := json.Unmarshal(raw["regime"], &label); err == nil {
			var snap RegimeSnapshot
			if err := json.Unmarshal(data, &snap); err != nil {
				return err
			}
			p.Legacy = snap.Regime
			p.MultiMode = false
			p.Windows = nil
			return nil
		}
	}
	windows := make(map[string]RegimeSnapshot, len(raw))
	for name, blob := range raw {
		trimmed := strings.TrimSpace(name)
		if trimmed == "" {
			return fmt.Errorf("regime: window names must be non-empty")
		}
		var snap RegimeSnapshot
		if err := json.Unmarshal(blob, &snap); err != nil {
			return fmt.Errorf("regime window %q: %w", trimmed, err)
		}
		windows[trimmed] = snap
	}
	p.Windows = windows
	p.MultiMode = true
	p.Legacy = ""
	return nil
}

func normalizeRegimeWindowKey(key string) string {
	return strings.TrimSpace(strings.ToLower(key))
}

func primaryRegimeWindowKey(rc *RegimeConfig) string {
	if rc == nil || len(rc.Windows) == 0 {
		return ""
	}
	if _, ok := rc.Windows["medium"]; ok {
		return "medium"
	}
	names := sortedRegimeWindowNamesFromConfig(rc.Windows)
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

func sortedRegimeWindowNames(windows map[string]RegimeSnapshot) []string {
	names := make([]string, 0, len(windows))
	for name := range windows {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func sortedRegimeWindowNamesFromConfig(windows RegimeWindowsMap) []string {
	names := make([]string, 0, len(windows))
	for name := range windows {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func regimeMultiWindowEnabled(rc *RegimeConfig) bool {
	return rc != nil && rc.Enabled && len(rc.Windows) > 0
}

func resolveStrategyRegimeWindow(sc StrategyConfig, field string, rc *RegimeConfig) string {
	var configured string
	switch field {
	case "gate":
		configured = sc.RegimeGateWindow
	case "atr":
		configured = sc.RegimeATRWindow
	case "directional":
		configured = sc.RegimeDirectionalWindow
	default:
		return ""
	}
	key := normalizeRegimeWindowKey(configured)
	if key == "" || key == regimeWindowDefaultKey {
		if regimeMultiWindowEnabled(rc) {
			return primaryRegimeWindowKey(rc)
		}
		return regimeWindowDefaultKey
	}
	return key
}

func strategyRegimeWindowConfigured(sc StrategyConfig, field string) string {
	switch field {
	case "gate":
		return sc.RegimeGateWindow
	case "atr":
		return sc.RegimeATRWindow
	case "directional":
		return sc.RegimeDirectionalWindow
	default:
		return ""
	}
}

func formatRegimeWindowSelectorInspect(sc StrategyConfig, field string, rc *RegimeConfig) string {
	configured := strategyRegimeWindowConfigured(sc, field)
	resolved := resolveStrategyRegimeWindow(sc, field, rc)
	key := normalizeRegimeWindowKey(configured)
	if key == "" || key == regimeWindowDefaultKey {
		if strings.TrimSpace(configured) == "" {
			return fmt.Sprintf("(default) → %q", resolved)
		}
		return fmt.Sprintf("%q (default) → %q", configured, resolved)
	}
	return fmt.Sprintf("%q → %q", configured, resolved)
}

func regimeWindowSelectorJSON(sc StrategyConfig, field string, rc *RegimeConfig) map[string]string {
	return map[string]string{
		"configured": strategyRegimeWindowConfigured(sc, field),
		"resolved":   resolveStrategyRegimeWindow(sc, field, rc),
	}
}

func regimeRequiredOhlcvLimit(rc *RegimeConfig) int {
	maxPeriod := 14
	if rc != nil {
		if rc.Period > maxPeriod {
			maxPeriod = rc.Period
		}
		for _, spec := range rc.Windows {
			if spec.Period > maxPeriod {
				maxPeriod = spec.Period
			}
		}
	}
	warmup := 2*maxPeriod - 1
	limit := warmup + regimeOhlcvMargin
	if limit < regimeOhlcvBaseLimit {
		limit = regimeOhlcvBaseLimit
	}
	return limit
}

func regimeLabelAtOpen(pos *Position, windowKey string, rc *RegimeConfig) string {
	if pos == nil {
		return ""
	}
	key := normalizeRegimeWindowKey(windowKey)
	if key == "" || key == regimeWindowDefaultKey {
		if regimeMultiWindowEnabled(rc) {
			key = primaryRegimeWindowKey(rc)
		}
	}
	if key != "" && key != regimeWindowDefaultKey && len(pos.RegimeWindows) > 0 {
		if label, ok := pos.RegimeWindows[key]; ok && strings.TrimSpace(label) != "" {
			return strings.TrimSpace(label)
		}
	}
	return strings.TrimSpace(pos.Regime)
}

func regimeLabelFromWindows(windows map[string]string, windowKey string, rc *RegimeConfig) string {
	if len(windows) == 0 {
		return ""
	}
	key := normalizeRegimeWindowKey(windowKey)
	if key == "" || key == regimeWindowDefaultKey {
		if regimeMultiWindowEnabled(rc) {
			key = primaryRegimeWindowKey(rc)
		}
	}
	if key == regimeWindowDefaultKey {
		if label, ok := windows[regimeWindowDefaultKey]; ok {
			return strings.TrimSpace(label)
		}
		return ""
	}
	if label, ok := windows[key]; ok {
		return strings.TrimSpace(label)
	}
	return ""
}

func validateRegimeWindowsConfig(cfg *Config) []string {
	if cfg == nil || cfg.Regime == nil {
		return nil
	}
	rc := cfg.Regime
	var errs []string
	if len(rc.Windows) > 0 && !rc.Enabled {
		errs = append(errs, "regime.windows requires regime.enabled=true")
	}
	seen := make(map[string]bool)
	for name, spec := range rc.Windows {
		trimmed := strings.TrimSpace(name)
		if trimmed == "" {
			errs = append(errs, "regime.windows: window names must be non-empty")
			continue
		}
		if seen[strings.ToLower(trimmed)] {
			errs = append(errs, fmt.Sprintf("regime.windows: duplicate window name %q", trimmed))
		}
		if normalizeRegimeWindowKey(trimmed) == regimeWindowReservedName {
			errs = append(errs, fmt.Sprintf("regime.windows: window name %q is reserved (conflicts with legacy regime snapshot JSON)", trimmed))
		}
		seen[strings.ToLower(trimmed)] = true
		errs = append(errs, validateRegimeWindowSpec(trimmed, spec, rc)...)
	}
	multi := regimeMultiWindowEnabled(rc)
	for _, sc := range cfg.Strategies {
		prefix := fmt.Sprintf("strategy[%s]", sc.ID)
		for _, pair := range []struct {
			field string
			value string
		}{
			{"regime_gate_window", sc.RegimeGateWindow},
			{"regime_atr_window", sc.RegimeATRWindow},
			{"regime_directional_window", sc.RegimeDirectionalWindow},
		} {
			key := normalizeRegimeWindowKey(pair.value)
			if key == "" || key == regimeWindowDefaultKey {
				continue
			}
			if !multi {
				errs = append(errs, fmt.Sprintf("%s: %s=%q requires regime.windows to be configured", prefix, pair.field, pair.value))
				continue
			}
			if !regimeWindowExists(rc, key) {
				errs = append(errs, fmt.Sprintf("%s: %s=%q not found in regime.windows (valid: %s)", prefix, pair.field, pair.value, strings.Join(sortedRegimeWindowNamesFromConfig(rc.Windows), ", ")))
			}
		}
		// #907: regime_window_divergence window-existence is validated in
		// validateStrategyRegimeVocabulary (after ResolveRaw populates the typed
		// fields) — not here, because this function runs first and the fields are
		// still empty at this point.
	}
	return errs
}

func regimeWindowSpec(rc *RegimeConfig, key string) (RegimeWindowSpec, bool) {
	if rc == nil {
		return RegimeWindowSpec{}, false
	}
	normalized := normalizeRegimeWindowKey(key)
	for name, spec := range rc.Windows {
		if normalizeRegimeWindowKey(name) == normalized {
			return spec, true
		}
	}
	return RegimeWindowSpec{}, false
}

func regimeWindowExists(rc *RegimeConfig, key string) bool {
	_, ok := regimeWindowSpec(rc, key)
	return ok
}

// regimeWindowsJSON forwards to regimeWindowsSpecJSON (#795).
func regimeWindowsJSON(rc *RegimeConfig) string {
	return regimeWindowsSpecJSON(rc)
}

func syncStrategyRegimeState(stratState *StrategyState, payload RegimePayload, rc *RegimeConfig) {
	if stratState == nil {
		return
	}
	stratState.Regime = payload.PrimaryLabel(rc)
	if labels := payload.WindowLabels(); len(labels) > 0 {
		stratState.RegimeWindows = cloneStringMap(labels)
	}
}

func positionRegimeForFeature(pos *Position, sc StrategyConfig, rc *RegimeConfig, feature string) string {
	return regimeLabelAtOpen(pos, resolveStrategyRegimeWindow(sc, feature, rc), rc)
}

func strategyRegimeWindowField(sc StrategyConfig, field string) string {
	switch field {
	case "gate":
		return sc.RegimeGateWindow
	case "atr":
		return sc.RegimeATRWindow
	case "directional":
		return sc.RegimeDirectionalWindow
	default:
		return ""
	}
}

// positionFeatureRegimeLabel resolves a stamped regime label for ATR/directional
// features using pos.RegimeWindows when present, without needing RegimeConfig.
func positionFeatureRegimeLabel(pos *Position, sc StrategyConfig, feature string) string {
	if pos == nil {
		return ""
	}
	key := normalizeRegimeWindowKey(strategyRegimeWindowField(sc, feature))
	if key != "" && key != regimeWindowDefaultKey && len(pos.RegimeWindows) > 0 {
		if label, ok := pos.RegimeWindows[key]; ok && strings.TrimSpace(label) != "" {
			return strings.TrimSpace(label)
		}
	}
	return strings.TrimSpace(pos.Regime)
}

func positionATRRegimeLabel(pos *Position, sc StrategyConfig) string {
	return positionFeatureRegimeLabel(pos, sc, "atr")
}

func positionDirectionalRegimeLabel(pos *Position, sc StrategyConfig) string {
	return positionFeatureRegimeLabel(pos, sc, "directional")
}

func strategyCurrentDirectionalRegime(stratState *StrategyState, sc StrategyConfig) string {
	if stratState == nil {
		return ""
	}
	key := normalizeRegimeWindowKey(sc.RegimeDirectionalWindow)
	if key != "" && key != regimeWindowDefaultKey && len(stratState.RegimeWindows) > 0 {
		if label, ok := stratState.RegimeWindows[key]; ok && strings.TrimSpace(label) != "" {
			return strings.TrimSpace(label)
		}
	}
	return strings.TrimSpace(stratState.Regime)
}

func positionCtxForCheck(sc StrategyConfig, pos *Position, regime *RegimeConfig) PositionCtx {
	ctx := positionCtxFromPosition(pos)
	if pos == nil {
		return ctx
	}
	if label := positionATRRegimeLabel(pos, sc); label != "" {
		ctx.Regime = label
	}
	if label := positionDirectionalRegimeLabel(pos, sc); label != "" {
		ctx.DirectionalRegime = label
	} else {
		ctx.DirectionalRegime = ctx.Regime
	}
	return ctx
}

func regimePayloadValue(p *RegimePayload) RegimePayload {
	if p == nil {
		return RegimePayload{}
	}
	return *p
}

func regimeGateLabel(sc StrategyConfig, payload RegimePayload, rc *RegimeConfig) string {
	return payload.Label(resolveStrategyRegimeWindow(sc, "gate", rc), rc)
}

func regimeDirectionalLabel(sc StrategyConfig, payload RegimePayload, rc *RegimeConfig) string {
	return payload.Label(resolveStrategyRegimeWindow(sc, "directional", rc), rc)
}

func applyRegimeGate(sc StrategyConfig, payload RegimePayload, rc *RegimeConfig, posQty float64) (gateLabel string, blocked bool) {
	gateLabel = regimeGateLabel(sc, payload, rc)
	return gateLabel, regimeBlocksOpen(sc.AllowedRegimes, gateLabel, posQty)
}

func stampPositionRegimeFromPayload(s *StrategyState, symbol string, payload RegimePayload, sc StrategyConfig, rc *RegimeConfig) {
	if s == nil || payload.IsEmpty() {
		return
	}
	pos, exists := s.Positions[symbol]
	if !exists || pos == nil {
		return
	}
	if len(pos.RegimeWindows) == 0 {
		if labels := payload.WindowLabels(); len(labels) > 0 {
			pos.RegimeWindows = cloneStringMap(labels)
		}
	}
	if pos.Regime != "" {
		return
	}
	gateKey := resolveStrategyRegimeWindow(sc, "gate", rc)
	if label := payload.Label(gateKey, rc); label != "" {
		pos.Regime = label
		return
	}
	if label := payload.PrimaryLabel(rc); label != "" {
		pos.Regime = label
	}
}

func regimeWindowFieldsEqual(a, b StrategyConfig) bool {
	return normalizeRegimeWindowKey(a.RegimeGateWindow) == normalizeRegimeWindowKey(b.RegimeGateWindow) &&
		normalizeRegimeWindowKey(a.RegimeATRWindow) == normalizeRegimeWindowKey(b.RegimeATRWindow) &&
		normalizeRegimeWindowKey(a.RegimeDirectionalWindow) == normalizeRegimeWindowKey(b.RegimeDirectionalWindow)
}

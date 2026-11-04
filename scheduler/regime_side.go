package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	regimeSideLong  = "long"
	regimeSideShort = "short"
	regimeSideNone  = "none"
)

const (
	regimeSideEvidenceLoaded     = "loaded"
	regimeSideEvidenceMissing    = "missing"
	regimeSideEvidenceMalformed  = "malformed"
	regimeSideEvidenceUnreadable = "unreadable"
)

func regimeSideForLabel(label string) string {
	switch strings.TrimSpace(label) {
	case "trending_up_clean", "trending_up_choppy", "ranging_directional_up", "trending_up":
		return regimeSideLong
	case "trending_down_clean", "trending_down_choppy", "ranging_directional_down", "trending_down":
		return regimeSideShort
	}
	return ""
}

func validRegimeSideRequest(side string) bool {
	switch side {
	case regimeSideLong, regimeSideShort, regimeSideNone:
		return true
	}
	return false
}

type regimeSideEvidence struct {
	Path   string
	State  string
	Detail string
	Set    *DirectionalCertSet
	ReadAt time.Time
}

func readRegimeSideEvidence(path string) *regimeSideEvidence {
	ev := &regimeSideEvidence{Path: path, ReadAt: time.Now().UTC(), Set: emptyDirectionalCertSet()}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			ev.State = regimeSideEvidenceMissing
			return ev
		}
		ev.State = regimeSideEvidenceUnreadable
		ev.Detail = err.Error()
		return ev
	}
	set, err := parseDirectionalCertSet(data)
	if err != nil {
		ev.State = regimeSideEvidenceMalformed
		ev.Detail = err.Error()
		return ev
	}
	ev.State = regimeSideEvidenceLoaded
	ev.Set = set
	return ev
}

func (ev *regimeSideEvidence) describe() string {
	if ev == nil {
		return "no certification artifact was read"
	}
	switch ev.State {
	case regimeSideEvidenceMissing:
		return fmt.Sprintf("certification artifact %q is missing", ev.Path)
	case regimeSideEvidenceMalformed:
		return fmt.Sprintf("certification artifact %q is malformed (%s)", ev.Path, ev.Detail)
	case regimeSideEvidenceUnreadable:
		return fmt.Sprintf("certification artifact %q is unreadable (%s)", ev.Path, ev.Detail)
	}
	return fmt.Sprintf("certification artifact %q", ev.Path)
}

var (
	regimeSideEvidenceMu    sync.RWMutex
	regimeSideEvidenceStore *regimeSideEvidence
)

func installRegimeSideEvidence(ev *regimeSideEvidence) {
	regimeSideEvidenceMu.Lock()
	regimeSideEvidenceStore = ev
	regimeSideEvidenceMu.Unlock()
}

func currentRegimeSideEvidence() *regimeSideEvidence {
	regimeSideEvidenceMu.RLock()
	defer regimeSideEvidenceMu.RUnlock()
	return regimeSideEvidenceStore
}

func regimeSideLive(sc StrategyConfig) bool {
	return portfolioScopeFor(sc) == ScopeLive
}

func regimeSideNeedsEvidence(sc StrategyConfig) bool {
	return sc.RegimeSide && regimeSideLive(sc) && !sc.AllowUncertifiedRegimeSide
}

func regimeSideCellEvidence(sc StrategyConfig, rc *RegimeConfig, ev *regimeSideEvidence, now time.Time) (states map[string]string, cell string, reason string) {
	asset, tf, classifier, ok := directionalCertIdentity(sc, rc)
	if !ok {
		return nil, "(no symbol/timeframe)", "the directional certification cell is unresolvable (no symbol/timeframe)"
	}
	cell = fmt.Sprintf("(%s,%s,%s)", asset, tf, classifier)
	if ev == nil || ev.State != regimeSideEvidenceLoaded {
		return nil, cell, ev.describe()
	}
	switch ev.Set.Status(asset, tf, classifier, now) {
	case CertExpired:
		return nil, cell, fmt.Sprintf("%s has an expired cell %s", ev.describe(), cell)
	case CertNever:
		return nil, cell, fmt.Sprintf("%s has no cell %s", ev.describe(), cell)
	}
	states, _ = ev.Set.Certified(asset, tf, classifier, now)
	return states, cell, ""
}

func regimeSideMatchingCertifiedLabels(states map[string]string) []string {
	var out []string
	for label, side := range states {
		if want := regimeSideForLabel(label); want != "" && want == side {
			out = append(out, label)
		}
	}
	sort.Strings(out)
	return out
}

func regimeSideAdmissionError(sc StrategyConfig, rc *RegimeConfig, ev *regimeSideEvidence, now time.Time) string {
	if !regimeSideNeedsEvidence(sc) {
		return ""
	}
	prefix := fmt.Sprintf("strategy[%s].regime_side", sc.ID)
	hint := "set allow_uncertified_regime_side: true to acknowledge the #1076 negative result, or run it in paper"
	states, cell, reason := regimeSideCellEvidence(sc, rc, ev, now)
	if reason != "" {
		return fmt.Sprintf("%s: live admission refused: %s; %s", prefix, reason, hint)
	}
	if len(regimeSideMatchingCertifiedLabels(states)) == 0 {
		return fmt.Sprintf("%s: live admission refused: %s cell %s has no exact label certified for the side regime_side would trade; %s", prefix, ev.describe(), cell, hint)
	}
	return ""
}

func regimeSideAdmissionErrors(cfg *Config, ev *regimeSideEvidence, now time.Time) []string {
	if cfg == nil {
		return nil
	}
	var errs []string
	for _, sc := range cfg.Strategies {
		if msg := regimeSideAdmissionError(sc, cfg.Regime, ev, now); msg != "" {
			errs = append(errs, msg)
		}
	}
	return errs
}

const regimeSideProbeAdmissionEnv = "GO_TRADER_PROBE_REGIME_SIDE_ADMISSION"

func regimeSideProbeAdmissionEnforced() bool {
	return strings.TrimSpace(os.Getenv(regimeSideProbeAdmissionEnv)) != "off"
}

func regimeSideAdmissionCell(sc StrategyConfig, rc *RegimeConfig) (string, bool) {
	if !regimeSideNeedsEvidence(sc) {
		return "", false
	}
	asset, tf, classifier, _ := directionalCertIdentity(sc, rc)
	return asset + "|" + tf + "|" + classifier, true
}

func regimeSideReloadAdmissionErrors(cfg, next *Config, ev *regimeSideEvidence, now time.Time) []string {
	if next == nil {
		return nil
	}
	var prevRegime *RegimeConfig
	prev := map[string]StrategyConfig{}
	if cfg != nil {
		prevRegime = cfg.Regime
		prev = strategyConfigByID(cfg.Strategies)
	}
	var errs []string
	for _, ns := range next.Strategies {
		nextCell, needs := regimeSideAdmissionCell(ns, next.Regime)
		if !needs {
			continue
		}
		if old, ok := prev[ns.ID]; ok {
			if oldCell, oldNeeds := regimeSideAdmissionCell(old, prevRegime); oldNeeds && oldCell == nextCell {
				continue
			}
		}
		if msg := regimeSideAdmissionError(ns, next.Regime, ev, now); msg != "" {
			errs = append(errs, msg)
		}
	}
	return errs
}

func regimeSideConfigErrors(cfg *Config) []string {
	if cfg == nil {
		return nil
	}
	rc := cfg.Regime
	var errs []string
	for _, sc := range cfg.Strategies {
		if !sc.RegimeSide && !sc.AllowUncertifiedRegimeSide {
			continue
		}
		prefix := fmt.Sprintf("strategy[%s]", sc.ID)
		if !sc.RegimeSide {
			errs = append(errs, fmt.Sprintf("%s.allow_uncertified_regime_side: requires regime_side: true", prefix))
			continue
		}
		if sc.Platform != "hyperliquid" || sc.Type != "perps" {
			errs = append(errs, fmt.Sprintf("%s.regime_side: only supported for HL perps strategies (got platform=%q type=%q)", prefix, sc.Platform, sc.Type))
		}
		if rc == nil || !rc.Enabled {
			errs = append(errs, fmt.Sprintf("%s.regime_side: requires top-level regime.enabled=true", prefix))
		} else {
			labels := regimeLabelsForStrategyWindow(sc, rc, "directional")
			directional := false
			for _, label := range labels {
				if regimeSideForLabel(label) != "" {
					directional = true
					break
				}
			}
			if !directional {
				errs = append(errs, fmt.Sprintf("%s.regime_side: directional window %q (classifier %q) has no label that selects a side (labels: %s)", prefix, resolveStrategyRegimeWindow(sc, "directional", rc), regimeClassifierForWindow(rc, resolveStrategyRegimeWindow(sc, "directional", rc)), strings.Join(labels, ", ")))
			}
		}
		for _, conflict := range []struct {
			field string
			set   bool
		}{
			{"regime_directional_policy", sc.RegimeDirectionalPolicy.IsConfigured()},
			{"regime_window_divergence", sc.RegimeWindowDivergence.IsConfigured()},
			{"regime_profile_allocation", sc.RegimeProfileAllocation.IsConfigured()},
			{"invert_signal", sc.InvertSignal},
			{"closed_bar_decisions", sc.ClosedBarDecisions},
		} {
			if conflict.set {
				errs = append(errs, fmt.Sprintf("%s.regime_side: cannot be combined with %s", prefix, conflict.field))
			}
		}
		closeName := ""
		if sc.CloseStrategy != nil {
			closeName = strings.TrimSpace(sc.CloseStrategy.Name)
		}
		openName := effectiveOpenStrategy(sc)
		switch {
		case closeName == "":
			errs = append(errs, fmt.Sprintf("%s.regime_side: requires an explicit close_strategy separate from the open strategy (open-as-close fallback is refused)", prefix))
		case closeName == openName:
			errs = append(errs, fmt.Sprintf("%s.regime_side: close_strategy %q is the effective open strategy; set a separate close strategy", prefix, closeName))
		}
	}
	return errs
}

type regimeSideResolution struct {
	Enabled    bool   `json:"enabled"`
	Side       string `json:"requested_side,omitempty"`
	Window     string `json:"window,omitempty"`
	Label      string `json:"label,omitempty"`
	Available  bool   `json:"available"`
	Ready      bool   `json:"ready"`
	Bars       int    `json:"bars,omitempty"`
	Need       int    `json:"need,omitempty"`
	Evidence   string `json:"evidence,omitempty"`
	HoldReason string `json:"hold_reason,omitempty"`
}

func (r regimeSideResolution) sentSide() string {
	if !r.Enabled {
		return ""
	}
	return r.Side
}

func resolveRegimeSide(sc StrategyConfig, posQty float64, payload RegimePayload, rc *RegimeConfig, ev *regimeSideEvidence, now time.Time) regimeSideResolution {
	if !sc.RegimeSide {
		return regimeSideResolution{}
	}
	out := regimeSideResolution{Enabled: true, Side: regimeSideNone}
	key, snap := regimeDependencySnapshot(resolveStrategyRegimeWindow(sc, "directional", rc), payload, rc)
	out.Window = key
	out.Available, out.Ready, out.Bars, out.Need = snap.Available, snap.Ready, snap.Bars, snap.Need
	out.Label = strings.TrimSpace(snap.Regime)
	if out.Label == "" {
		out.Label = strings.TrimSpace(regimeDirectionalLabel(sc, payload, rc))
	}
	switch {
	case !regimeSideLive(sc):
		out.Evidence = "paper (no certification needed)"
	case sc.AllowUncertifiedRegimeSide:
		out.Evidence = "acknowledged uncertified (#1076)"
	default:
		out.Evidence = "certified exact label required"
	}
	if posQty > 0 {
		out.HoldReason = "position open: regime_side opens only from a flat start, so no add, reversal or re-entry this decision"
		return out
	}
	if !snap.Available {
		out.HoldReason = fmt.Sprintf("regime directional window %s unavailable", key)
		return out
	}
	if !snap.Ready {
		out.HoldReason = fmt.Sprintf("regime directional window %s warming (%d/%d)", key, snap.Bars, snap.Need)
		return out
	}
	side := regimeSideForLabel(out.Label)
	if side == "" {
		out.HoldReason = fmt.Sprintf("label %q on window %s selects no side", out.Label, key)
		return out
	}
	if (side == regimeSideLong && !PerpsAllowsLong(sc)) || (side == regimeSideShort && !PerpsAllowsShort(sc)) {
		out.HoldReason = fmt.Sprintf("base direction %q does not permit %s", EffectiveDirection(sc), side)
		return out
	}
	if regimeSideNeedsEvidence(sc) {
		states, cell, reason := regimeSideCellEvidence(sc, rc, ev, now)
		if reason != "" {
			out.HoldReason = "uncertified: " + reason
			return out
		}
		certSide, ok := states[out.Label]
		if !ok {
			out.HoldReason = fmt.Sprintf("uncertified: cell %s does not certify the exact label %q", cell, out.Label)
			return out
		}
		if certSide != side {
			out.HoldReason = fmt.Sprintf("uncertified: cell %s certifies %q as %s, not %s", cell, out.Label, certSide, side)
			return out
		}
		out.Evidence = fmt.Sprintf("certified %s=%s in %s", out.Label, certSide, cell)
	}
	out.Side = side
	return out
}

func regimeSideRequestForCheck(sc StrategyConfig, posCtx PositionCtx, rc *RegimeConfig) regimeSideResolution {
	if !sc.RegimeSide {
		return regimeSideResolution{}
	}
	return resolveRegimeSide(sc, posCtx.Quantity, globalRegimeStore.PayloadForStrategy(sc, rc), rc, currentRegimeSideEvidence(), time.Now().UTC())
}

func regimeSideContractError(sc StrategyConfig, sent regimeSideResolution, fields StrategyDecisionFields) string {
	want := sent.sentSide()
	switch {
	case want == "" && fields.OpenSideApplied != nil:
		return fmt.Sprintf("regime-side contract mismatch: no open_side was sent but the check returned open_side_applied=%q (redeploy with scripts/update.sh so Go and Python match)", *fields.OpenSideApplied)
	case want != "" && fields.OpenSideApplied == nil:
		return fmt.Sprintf("regime-side contract mismatch: sent open_side=%q but the check returned no open_side_applied echo; holding this signal because the check may have opened on the raw trigger side (redeploy with scripts/update.sh so Go and Python match)", want)
	case want != "" && *fields.OpenSideApplied != want:
		return fmt.Sprintf("regime-side contract mismatch: sent open_side=%q but the check returned open_side_applied=%q; holding this signal", want, *fields.OpenSideApplied)
	}
	if sc.RegimeSide && want == "" {
		return "regime-side contract mismatch: regime_side is on but no open_side request was resolved for this check"
	}
	return ""
}

func regimeSideStatus(sc StrategyConfig, posQty float64, rc *RegimeConfig) *regimeSideResolution {
	if !sc.RegimeSide {
		return nil
	}
	res := resolveRegimeSide(sc, posQty, globalRegimeStore.PayloadForStrategy(sc, rc), rc, currentRegimeSideEvidence(), time.Now().UTC())
	return &res
}

func formatRegimeSideNote(r regimeSideResolution) string {
	if !r.Enabled || r.Side == "" || r.Side == regimeSideNone {
		return ""
	}
	return fmt.Sprintf("Regime side: %s window %s -> %s", r.Label, r.Window, r.Side)
}

func regimeSideStartupLines(cfg *Config) []string {
	if cfg == nil {
		return nil
	}
	now := time.Now().UTC()
	ev := currentRegimeSideEvidence()
	var out []string
	for _, sc := range cfg.Strategies {
		if !sc.RegimeSide {
			continue
		}
		window := resolveStrategyRegimeWindow(sc, "directional", cfg.Regime)
		switch {
		case !regimeSideLive(sc):
			out = append(out, fmt.Sprintf("[#1906] %s: regime_side on (paper) — non-zero triggers open on the side window %s selects; no certification needed", sc.ID, window))
		case sc.AllowUncertifiedRegimeSide:
			out = append(out, fmt.Sprintf("[#1906] %s: regime_side LIVE UNCERTIFIED (allow_uncertified_regime_side) — window %s selects the side without certified evidence; #1076 found no supported forward-direction edge", sc.ID, window))
		default:
			states, cell, reason := regimeSideCellEvidence(sc, cfg.Regime, ev, now)
			if reason != "" {
				out = append(out, fmt.Sprintf("[#1906] %s: regime_side live entries HELD — %s", sc.ID, reason))
				continue
			}
			matching := regimeSideMatchingCertifiedLabels(states)
			if len(matching) == 0 {
				out = append(out, fmt.Sprintf("[#1906] %s: regime_side live entries HELD — %s cell %s has no exact label certified for the side regime_side would trade", sc.ID, ev.describe(), cell))
				continue
			}
			out = append(out, fmt.Sprintf("[#1906] %s: regime_side live on window %s; certified labels in %s: %s", sc.ID, window, cell, strings.Join(matching, ", ")))
		}
	}
	return out
}

func regimeSideStartupLinesNeedingOwnerDM(lines []string) []string {
	var out []string
	for _, line := range lines {
		if strings.Contains(line, "LIVE UNCERTIFIED") || strings.Contains(line, "entries HELD") {
			out = append(out, line)
		}
	}
	return out
}

func configUsesRegimeSide(cfg *Config) bool {
	if cfg == nil {
		return false
	}
	for _, sc := range cfg.Strategies {
		if sc.RegimeSide {
			return true
		}
	}
	return false
}

func startupRegimeSideEvidence(cfg *Config) *regimeSideEvidence {
	if cfg != nil && cfg.regimeSideEvidence != nil {
		return cfg.regimeSideEvidence
	}
	return readRegimeSideEvidence(directionalCertPath())
}

func regimeSideStatusForState(sc StrategyConfig, s *StrategyState, rc *RegimeConfig) *regimeSideResolution {
	if !sc.RegimeSide {
		return nil
	}
	posQty := 0.0
	if s != nil {
		for _, p := range s.Positions {
			if p != nil && p.Quantity > 0 && !p.isHedgeLeg() {
				posQty = p.Quantity
				break
			}
		}
	}
	return regimeSideStatus(sc, posQty, rc)
}

func regimeSideInspectEvidence(sc StrategyConfig, cfg *Config) string {
	if !regimeSideLive(sc) {
		return "paper: no certification needed"
	}
	if sc.AllowUncertifiedRegimeSide {
		return "live, acknowledged uncertified (allow_uncertified_regime_side; #1076)"
	}
	var rc *RegimeConfig
	var ev *regimeSideEvidence
	if cfg != nil {
		rc, ev = cfg.Regime, cfg.regimeSideEvidence
	}
	states, cell, reason := regimeSideCellEvidence(sc, rc, ev, time.Now().UTC())
	if reason != "" {
		return "live, entries held: " + reason
	}
	matching := regimeSideMatchingCertifiedLabels(states)
	if len(matching) == 0 {
		return fmt.Sprintf("live, entries held: %s cell %s has no exact label certified for the side regime_side would trade", ev.describe(), cell)
	}
	return fmt.Sprintf("live, certified labels in %s: %s", cell, strings.Join(matching, ", "))
}

func regimeSideInspectPersistedLabel(sc StrategyConfig, cfg *Config, state *AppState) (window, label string) {
	var rc *RegimeConfig
	if cfg != nil {
		rc = cfg.Regime
	}
	window = resolveStrategyRegimeWindow(sc, "directional", rc)
	if state == nil {
		return window, ""
	}
	if ss := state.Strategies[sc.ID]; ss != nil {
		label = strategyCurrentDirectionalRegime(ss, sc)
	}
	return window, label
}

const regimeSideInspectUnresolved = "unresolved (inspect reads no live regime payload; the status API resolves the requested side and hold reason)"

func appendRegimeSideInspectLines(b *strings.Builder, sc StrategyConfig, cfg *Config, state *AppState) {
	if !sc.RegimeSide {
		return
	}
	window, label := regimeSideInspectPersistedLabel(sc, cfg, state)
	fmt.Fprintf(b, "  regime_side:         on (window %s; %s)\n", window, regimeSideInspectEvidence(sc, cfg))
	if label != "" {
		fmt.Fprintf(b, "    last saved label:  %s (selects %s; saved state, not a decision)\n", label, regimeSideInspectSide(label))
	}
	fmt.Fprintf(b, "    requested side:    %s\n", regimeSideInspectUnresolved)
}

func regimeSideInspectSide(label string) string {
	if side := regimeSideForLabel(label); side != "" {
		return side
	}
	return regimeSideNone
}

func regimeSideInspectJSON(sc StrategyConfig, cfg *Config, state *AppState) map[string]interface{} {
	if !sc.RegimeSide {
		return nil
	}
	window, label := regimeSideInspectPersistedLabel(sc, cfg, state)
	out := map[string]interface{}{
		"enabled":                       true,
		"allow_uncertified_regime_side": sc.AllowUncertifiedRegimeSide,
		"window":                        window,
		"evidence":                      regimeSideInspectEvidence(sc, cfg),
		"requested_side":                regimeSideInspectUnresolved,
		"hold_reason":                   regimeSideInspectUnresolved,
	}
	if label != "" {
		out["last_saved_label"] = label
		out["last_saved_label_side"] = regimeSideInspectSide(label)
	}
	return out
}

func refreshRegimeSideEvidenceForReload() *regimeSideEvidence {
	ev := readRegimeSideEvidence(directionalCertPath())
	installRegimeSideEvidence(ev)
	return ev
}

func regimeSidePreviewCertification(sc StrategyConfig, rc *RegimeConfig, ev *regimeSideEvidence, now time.Time) map[string]interface{} {
	if !regimeSideLive(sc) {
		return map[string]interface{}{"context": "paper"}
	}
	out := map[string]interface{}{"context": "live", "states": map[string]string{}, "active_from": nil, "expires_at": nil}
	asset, tf, classifier, ok := directionalCertIdentity(sc, rc)
	if !ok {
		return out
	}
	out["cell"] = asset + "|" + tf + "|" + classifier
	if ev == nil || ev.State != regimeSideEvidenceLoaded || ev.Set == nil {
		return out
	}
	out["source"] = ev.Path
	entry, found := ev.Set.byKey[certKey(asset, tf, classifier)]
	if !found || ev.Set.Status(asset, tf, classifier, now) != CertActive {
		return out
	}
	out["states"] = cloneStringMap(entry.States)
	switch {
	case !entry.GeneratedAt.IsZero():
		out["active_from"] = entry.GeneratedAt.UTC().Format(time.RFC3339)
	case !ev.Set.GeneratedAt.IsZero():
		out["active_from"] = ev.Set.GeneratedAt.UTC().Format(time.RFC3339)
	}
	if entry.ExpiresAt != nil {
		out["expires_at"] = entry.ExpiresAt.UTC().Format(time.RFC3339)
	}
	return out
}

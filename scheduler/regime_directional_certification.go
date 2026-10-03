package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

const defaultDirectionalCertPath = "backtest/research/regime_directional_certifications.json"

type DirectionalCertEntry struct {
	Asset       string            `json:"asset"`
	Timeframe   string            `json:"timeframe"`
	Classifier  string            `json:"classifier"`
	GeneratedAt time.Time         `json:"generated_at,omitempty"`
	ExpiresAt   *time.Time        `json:"expires_at,omitempty"`
	States      map[string]string `json:"states"`
}

type DirectionalCertSet struct {
	SchemaVersion  int                    `json:"schema_version"`
	GeneratedAt    time.Time              `json:"generated_at,omitempty"`
	Generator      string                 `json:"generator,omitempty"`
	SourceEvidence string                 `json:"source_evidence,omitempty"`
	Criteria       map[string]interface{} `json:"criteria,omitempty"`
	DefaultTTLDays int                    `json:"default_ttl_days,omitempty"`
	Entries        []DirectionalCertEntry `json:"certified"`

	byKey map[string]DirectionalCertEntry
}

type DirectionalCertStatus int

const (
	CertNever DirectionalCertStatus = iota
	CertActive
	CertExpired
)

func (s DirectionalCertStatus) String() string {
	switch s {
	case CertActive:
		return "certified"
	case CertExpired:
		return "expired"
	default:
		return "uncertified"
	}
}

func emptyDirectionalCertSet() *DirectionalCertSet {
	return &DirectionalCertSet{SchemaVersion: 1, byKey: map[string]DirectionalCertEntry{}}
}

func normalizeCertAsset(symbol string) string {
	s := strings.ToUpper(strings.TrimSpace(symbol))
	if s == "" {
		return ""
	}
	for _, sep := range []string{"/", ":", "-", "_"} {
		if i := strings.Index(s, sep); i > 0 {
			s = s[:i]
			break
		}
	}
	return s
}

func certKey(asset, timeframe, classifier string) string {
	return normalizeCertAsset(asset) + "|" + strings.TrimSpace(timeframe) + "|" + strings.TrimSpace(strings.ToLower(classifier))
}

func (s *DirectionalCertSet) buildIndex() {
	s.byKey = make(map[string]DirectionalCertEntry, len(s.Entries))
	for _, e := range s.Entries {
		s.byKey[certKey(e.Asset, e.Timeframe, e.Classifier)] = e
	}
}

func parseDirectionalCertSet(data []byte) (*DirectionalCertSet, error) {
	var s DirectionalCertSet
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("regime_directional_certifications: %w", err)
	}
	if s.SchemaVersion != 1 {
		return nil, fmt.Errorf("regime_directional_certifications: unsupported schema_version %d (want 1)", s.SchemaVersion)
	}
	for i, e := range s.Entries {
		if normalizeCertAsset(e.Asset) == "" || strings.TrimSpace(e.Timeframe) == "" || strings.TrimSpace(e.Classifier) == "" {
			return nil, fmt.Errorf("regime_directional_certifications: certified[%d] missing asset/timeframe/classifier", i)
		}
		for label, dir := range e.States {
			switch dir {
			case DirectionLong, DirectionShort:
			default:
				return nil, fmt.Errorf("regime_directional_certifications: certified[%d].states[%q] must be %q or %q (got %q)",
					i, label, DirectionLong, DirectionShort, dir)
			}
		}
	}
	s.buildIndex()
	return &s, nil
}

func LoadDirectionalCertSet(path string) (*DirectionalCertSet, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return emptyDirectionalCertSet(), nil
		}
		return nil, err
	}
	return parseDirectionalCertSet(data)
}

func LoadDirectionalCertSetFailClosed(path string, warnf func(string, ...interface{})) *DirectionalCertSet {
	set, err := LoadDirectionalCertSet(path)
	if err != nil {
		if warnf != nil {
			warnf("[CRITICAL] directional certification artifact %q failed to load (%v) — failing closed: all regime_directional_policy strategies run DEFAULT-OFF (base direction)", path, err)
		}
		return emptyDirectionalCertSet()
	}
	return set
}

func directionalCertPath() string {
	if p := strings.TrimSpace(os.Getenv("GO_TRADER_DIRECTIONAL_CERT_PATH")); p != "" {
		return p
	}
	return defaultDirectionalCertPath
}

func (s *DirectionalCertSet) Status(asset, timeframe, classifier string, now time.Time) DirectionalCertStatus {
	if s == nil {
		return CertNever
	}
	entry, ok := s.byKey[certKey(asset, timeframe, classifier)]
	if !ok {
		return CertNever
	}
	if entry.ExpiresAt != nil && !now.Before(*entry.ExpiresAt) {
		return CertExpired
	}
	return CertActive
}

func (s *DirectionalCertSet) Certified(asset, timeframe, classifier string, now time.Time) (states map[string]string, ok bool) {
	if s == nil {
		return nil, false
	}
	entry, found := s.byKey[certKey(asset, timeframe, classifier)]
	if !found {
		return nil, false
	}
	if entry.ExpiresAt != nil && !now.Before(*entry.ExpiresAt) {
		return nil, false
	}
	return entry.States, true
}

var (
	directionalCertMu    sync.RWMutex
	directionalCertStore = emptyDirectionalCertSet()
)

func setDirectionalCertStore(s *DirectionalCertSet) {
	if s == nil {
		s = emptyDirectionalCertSet()
	}
	directionalCertMu.Lock()
	directionalCertStore = s
	directionalCertMu.Unlock()
}

func getDirectionalCertStore() *DirectionalCertSet {
	directionalCertMu.RLock()
	defer directionalCertMu.RUnlock()
	return directionalCertStore
}

func directionalCertIdentity(sc StrategyConfig, rc *RegimeConfig) (asset, timeframe, classifier string, ok bool) {
	symbol, tf := strategyRegimeSymbolTimeframe(sc.Args, rc)
	if symbol == "" || tf == "" {
		return "", "", "", false
	}
	window := resolveStrategyRegimeWindow(sc, "directional", rc)
	classifier = regimeClassifierForWindow(rc, window)
	return normalizeCertAsset(symbol), tf, classifier, true
}

func strategyDirectionalCertified(sc StrategyConfig, rc *RegimeConfig, now time.Time) (states map[string]string, ok bool) {
	asset, tf, classifier, idOK := directionalCertIdentity(sc, rc)
	if !idOK {
		return nil, false
	}
	return getDirectionalCertStore().Certified(asset, tf, classifier, now)
}

func stampDirectionCertifiedAtOpenIfOpened(s *StrategyState, symbol string, opened bool, sc StrategyConfig, rc *RegimeConfig) {
	if s == nil || !opened {
		return
	}
	pos, ok := s.Positions[symbol]
	if !ok || pos == nil {
		return
	}
	if !sc.RegimeDirectionalPolicy.IsConfigured() {
		pos.DirectionCertifiedAtOpen = false
		pos.DirectionCertifiedStatesAtOpen = nil
		return
	}
	states, certified := strategyDirectionalCertified(sc, rc, time.Now().UTC())
	pos.DirectionCertifiedAtOpen = certified
	pos.DirectionCertifiedStatesAtOpen = cloneStringMap(states)
}

func strategyDirectionalCertStatus(sc StrategyConfig, rc *RegimeConfig, now time.Time) DirectionalCertStatus {
	asset, tf, classifier, idOK := directionalCertIdentity(sc, rc)
	if !idOK {
		return CertNever
	}
	return getDirectionalCertStore().Status(asset, tf, classifier, now)
}

func directionalCertStartupSummary(cfg *Config) []string {
	if cfg == nil {
		return nil
	}
	now := time.Now().UTC()
	store := getDirectionalCertStore()
	var out []string
	for _, sc := range cfg.Strategies {
		if !sc.RegimeDirectionalPolicy.IsConfigured() {
			continue
		}
		asset, tf, classifier, idOK := directionalCertIdentity(sc, cfg.Regime)
		if !idOK {
			out = append(out, fmt.Sprintf("[#1085] %s: regime_directional_policy configured but symbol/timeframe unresolvable — policy inert (base direction)", sc.ID))
			continue
		}
		cell := fmt.Sprintf("(%s,%s,%s)", asset, tf, classifier)
		switch store.Status(asset, tf, classifier, now) {
		case CertActive:
			states, _ := store.Certified(asset, tf, classifier, now)
			line := fmt.Sprintf("[#1085] %s: regime_directional_policy CERTIFIED for %s — directional selection ACTIVE", sc.ID, cell)
			if mm := directionalCertSignMismatches(sc, states); len(mm) > 0 {
				line += fmt.Sprintf("; [WARN] config contradicts certified evidence — these states resolve to BASE direction (the configured side is NOT traded): %s", strings.Join(mm, ", "))
			}
			out = append(out, line)
		case CertExpired:
			out = append(out, fmt.Sprintf("[#1085] %s: regime_directional_policy certification EXPIRED for %s — DEFAULT-OFF for new entries (base direction); open positions ride under their open stamp. Re-run regime_1076_certify.py to refresh.", sc.ID, cell))
		default:
			out = append(out, fmt.Sprintf("[#1085] %s: regime_directional_policy DEFAULT-OFF — no certified directional edge for %s (#1076 negative result); resolves to base direction. Use the regime for ATR-scaled SL/TP sizing (#1078); disable from flat.", sc.ID, cell))
		}
	}
	return out
}

func directionalCertInspectStatus(sc StrategyConfig, cfg *Config) (status, cell string) {
	var rc *RegimeConfig
	if cfg != nil {
		rc = cfg.Regime
	}
	asset, tf, classifier, ok := directionalCertIdentity(sc, rc)
	if !ok {
		return "unresolvable (policy inert → base direction)", "(no symbol/timeframe)"
	}
	st := getDirectionalCertStore().Status(asset, tf, classifier, time.Now().UTC())
	cell = fmt.Sprintf("(%s,%s,%s)", asset, tf, classifier)
	switch st {
	case CertActive:
		return "certified — directional selection ACTIVE", cell
	case CertExpired:
		return "expired — DEFAULT-OFF for new entries (base direction); open positions ride their open stamp", cell
	default:
		return "uncertified — DEFAULT-OFF (base direction; #1076 negative result)", cell
	}
}

func directionalCertSignMismatches(sc StrategyConfig, certStates map[string]string) []string {
	if sc.RegimeDirectionalPolicy == nil || len(certStates) == 0 {
		return nil
	}
	var out []string
	for label, certDir := range certStates {
		entry, ok := sc.RegimeDirectionalPolicy.Resolve(label)
		if !ok {
			continue
		}
		if entry.Direction == DirectionBoth {
			continue
		}
		if entry.Direction != certDir {
			out = append(out, fmt.Sprintf("%s: config=%s certified=%s", label, entry.Direction, certDir))
		}
	}
	sort.Strings(out)
	return out
}

func directionalCertStartupLinesNeedingOwnerDM(lines []string) []string {
	var out []string
	for _, line := range lines {
		if strings.Contains(line, "DEFAULT-OFF") ||
			strings.Contains(line, "EXPIRED") ||
			strings.Contains(line, "policy inert") {
			out = append(out, line)
		}
	}
	return out
}

func directionalCertOwnerDMSnapshot(lines []string) string {
	filtered := directionalCertStartupLinesNeedingOwnerDM(lines)
	if len(filtered) == 0 {
		return ""
	}
	cp := append([]string(nil), filtered...)
	sort.Strings(cp)
	return strings.Join(cp, "\n")
}

var (
	directionalCertOwnerDMMu       sync.Mutex
	directionalCertOwnerDMLastSnap string
)

func notifyDirectionalCertStartupSummary(notifier *MultiNotifier, lines []string) {
	if notifier == nil || !notifier.HasOwner() {
		return
	}
	filtered := directionalCertStartupLinesNeedingOwnerDM(lines)
	snap := directionalCertOwnerDMSnapshot(lines)
	directionalCertOwnerDMMu.Lock()
	if snap == directionalCertOwnerDMLastSnap {
		directionalCertOwnerDMMu.Unlock()
		return
	}
	prevSnap := directionalCertOwnerDMLastSnap
	directionalCertOwnerDMLastSnap = snap
	directionalCertOwnerDMMu.Unlock()

	if snap == "" {
		return
	}
	toSend := filtered
	if prevSnap != "" {
		prevSet := make(map[string]struct{}, strings.Count(prevSnap, "\n")+1)
		for _, line := range strings.Split(prevSnap, "\n") {
			prevSet[line] = struct{}{}
		}
		toSend = nil
		for _, line := range filtered {
			if _, seen := prevSet[line]; !seen {
				toSend = append(toSend, line)
			}
		}
	}
	for _, line := range toSend {
		notifier.SendOwnerDM("[state] " + line)
	}
}

func directionalCertOperatorNotes(strategies []StrategyConfig, rc *RegimeConfig) string {
	if len(strategies) == 0 {
		return ""
	}
	now := time.Now().UTC()
	cfg := &Config{Regime: rc}
	var parts []string
	for _, sc := range strategies {
		if !sc.RegimeDirectionalPolicy.IsConfigured() {
			continue
		}
		switch strategyDirectionalCertStatus(sc, rc, now) {
		case CertActive:
			continue
		case CertExpired:
			_, cell := directionalCertInspectStatus(sc, cfg)
			parts = append(parts, fmt.Sprintf("%s=EXPIRED %s", sc.ID, cell))
		default:
			_, cell := directionalCertInspectStatus(sc, cfg)
			parts = append(parts, fmt.Sprintf("%s=DEFAULT-OFF %s", sc.ID, cell))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	sort.Strings(parts)
	return " | directional_policy: " + strings.Join(parts, "; ")
}

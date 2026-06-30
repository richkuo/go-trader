package main

// Evidence-gated directional certification (#1085).
//
// #1076 validated the premise behind regime_directional_policy (#779) — regime
// label -> forward DIRECTION — and found it empirically false across the tested
// universe (0/2121 per-state forward-return tests survive global FDR/Bonferroni;
// the regime-timing book never beats its block-shuffled-label null, 0/60 after
// FDR). #1084 shipped a non-breaking [WARN]. This file is the principled
// end-state: the directional-selection surface is DEFAULT-OFF and falls to the
// base direction, and is honored for a strategy ONLY where a per-(asset,
// timeframe, classifier) certification gate proves real, multiplicity-honest
// directional edge.
//
// SSoT contract: the Python research harness (regime_1076_certify.py) emits the
// certification artifact (regime_directional_certifications.json) as the single
// source of truth for the statistical test; Go consumes it as data and never
// reimplements the test. The current artifact certifies NOTHING (#1076), so
// every configured policy is inert and resolves to base direction.
//
// Fail-closed everywhere: a missing/malformed/expired certification yields "not
// certified" (base direction), never a wrong-side bet. A malformed artifact is
// loud but NEVER fatal — taking down live trading over a research sidecar would
// be the less safe outcome.
//
// Migration safety (#1085 req 1/2): the live entry gate keys on the LIVE
// certification verdict only when FLAT. An open position rides under the
// certification status stamped at its open (Position.DirectionCertifiedAtOpen),
// so a time-based expiry/refresh can never silently flip an open position's
// effective direction or trip the #822 orphan-close — operators migrate from
// flat. Legacy positions (pre-#1085, stamp false) resolve to base, which is the
// intended from-flat migration: #822 auto-closes sole-owner conflicts and
// shared-coin conflicts are surfaced to the operator, never silently flipped.

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// defaultDirectionalCertPath is the artifact location relative to the working
// directory (always the code checkout — see CLAUDE.md). Overridable via
// GO_TRADER_DIRECTIONAL_CERT_PATH (registered in agent_info.go env registry).
const defaultDirectionalCertPath = "backtest/research/regime_directional_certifications.json"

// DirectionalCertEntry certifies one (asset, timeframe, classifier) triple.
// States maps each canonical regime label to its certified direction
// ("long"/"short"). The asset is the normalized base symbol (see
// normalizeCertAsset) so "BTC/USDT" (research universe) and "BTC" (live HL
// args) reconcile to the same key.
type DirectionalCertEntry struct {
	Asset       string            `json:"asset"`
	Timeframe   string            `json:"timeframe"`
	Classifier  string            `json:"classifier"`
	GeneratedAt time.Time         `json:"generated_at,omitempty"`
	ExpiresAt   *time.Time        `json:"expires_at,omitempty"`
	States      map[string]string `json:"states"`
}

// DirectionalCertSet is the parsed certification artifact plus a lookup index.
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

// DirectionalCertStatus distinguishes the reasons a strategy is/ isn't honored,
// for operator messaging and migration safety.
type DirectionalCertStatus int

const (
	// CertNever — no certification entry for this (asset, tf, classifier).
	// Steady-state for the whole tested universe (#1076). Default-off.
	CertNever DirectionalCertStatus = iota
	// CertActive — a non-expired certification exists; the policy is honored.
	CertActive
	// CertExpired — a certification existed but its expiry passed. Default-off
	// for new entries; advisory for open positions (never auto-closed via
	// expiry — they rode in under CertActive and keep their open stamp).
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

// emptyDirectionalCertSet is the fail-closed default: nothing certified.
func emptyDirectionalCertSet() *DirectionalCertSet {
	return &DirectionalCertSet{SchemaVersion: 1, byKey: map[string]DirectionalCertEntry{}}
}

// normalizeCertAsset reduces a symbol to its base asset for certification
// matching: strips a quote/perp suffix and upper-cases. "BTC/USDT" -> "BTC",
// "btc" -> "BTC", "BTC-PERP" -> "BTC", "ETH/USD" -> "ETH". Pure; mirrored in
// regime_1076_certify.py so both sides key identically.
func normalizeCertAsset(symbol string) string {
	s := strings.ToUpper(strings.TrimSpace(symbol))
	if s == "" {
		return ""
	}
	// Split on the first quote/pair separator.
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

// parseDirectionalCertSet parses raw artifact bytes. Returns an error on
// malformed JSON or schema (callers fail closed, never fatal).
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

// LoadDirectionalCertSet reads + parses the artifact. A missing file is NOT an
// error — it yields the empty (fail-closed) set so a deploy without the sidecar
// simply runs default-off. Malformed content IS an error (caller logs + falls
// back to empty).
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

// LoadDirectionalCertSetFailClosed always returns a usable set: on any error it
// logs via warnf and returns the empty (nothing-certified) set. Used at startup
// and SIGHUP so a bad research sidecar degrades to base direction, never a crash
// and never a wrong-side bet.
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

// directionalCertPath resolves the artifact path (env override or default).
// The literal env name keeps the agent_info env-registry test enforcing
// registration (#1051 coverage guard).
func directionalCertPath() string {
	if p := strings.TrimSpace(os.Getenv("GO_TRADER_DIRECTIONAL_CERT_PATH")); p != "" {
		return p
	}
	return defaultDirectionalCertPath
}

// Status reports whether (asset, tf, classifier) is certified/expired/never as
// of now.
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

// Certified returns the certified per-state direction map if (asset, tf,
// classifier) is currently certified (present and not expired). ok=false ->
// default-off (use base direction).
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

// --- package-level store (loaded at startup + SIGHUP) -----------------------

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

// directionalCertIdentity derives the certification key for a strategy:
// (normalized asset, timeframe, directional-window classifier). ok=false when
// the strategy carries no resolvable symbol/timeframe (regime can't run, so the
// policy is inert anyway).
func directionalCertIdentity(sc StrategyConfig, rc *RegimeConfig) (asset, timeframe, classifier string, ok bool) {
	symbol, tf := strategyRegimeSymbolTimeframe(sc.Args, rc)
	if symbol == "" || tf == "" {
		return "", "", "", false
	}
	window := resolveStrategyRegimeWindow(sc, "directional", rc)
	classifier = regimeClassifierForWindow(rc, window)
	return normalizeCertAsset(symbol), tf, classifier, true
}

// strategyDirectionalCertified reports whether sc's exact (asset, tf,
// classifier) is currently certified (non-expired), consulting the package
// store. Gates flat-entry direction selection.
func strategyDirectionalCertified(sc StrategyConfig, rc *RegimeConfig, now time.Time) (states map[string]string, ok bool) {
	asset, tf, classifier, idOK := directionalCertIdentity(sc, rc)
	if !idOK {
		return nil, false
	}
	return getDirectionalCertStore().Certified(asset, tf, classifier, now)
}

// stampDirectionCertifiedAtOpenIfOpened freezes the directional-certification
// verdict onto a freshly-opened position, write-once at the ENTRY INSTANT (#1085
// req 2). The verdict must be frozen here — at the moment the side is decided —
// rather than deferred to whenever the regime LABEL happens to record, because:
//   - the label warms up lazily over several cycles in multi-window mode (a
//     short-period window populates first, so the gate/primary window's Label is
//     "" for a while even though payload.IsEmpty() is false), and
//   - the verdict does NOT depend on the live label at all — it keys on the
//     config-derived (asset, timeframe, classifier) cell, which is known the
//     instant the position opens.
//
// Coupling the stamp to the label-recording cycle (the prior approach) let a
// SIGHUP cert change land between open and label-record, so the eventual stamp
// captured the CHANGED verdict — corrupting an already-open position's
// #822 orphan-close decision. opened is true only when a genuine open Trade was
// produced this cycle (Execute*Signal's OpenTrade != nil): flat→open or a flip's
// fresh leg — NEVER a scale-in add (a distinct dispatch that preserves the
// original entry's stamp), so this is naturally write-once for the life of the
// position. Default false (base direction) when the policy isn't configured and
// for positions that appear via reconciliation rather than our execute path —
// the fail-closed from-flat migration the issue requires.
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
	// Freeze a COPY of the certified per-state direction map at the entry
	// instant so PER-STATE sign gating of the open position (hold-on-transition
	// AND the #822 orphan check) consults the open-time evidence, never the live
	// artifact a SIGHUP/expiry could change mid-position (#1085 req 2). nil when
	// the cell is uncertified → every state resolves to base direction.
	pos.DirectionCertifiedStatesAtOpen = cloneStringMap(states)
}

// strategyDirectionalCertStatus is the messaging-oriented sibling of
// strategyDirectionalCertified (certified/expired/never).
func strategyDirectionalCertStatus(sc StrategyConfig, rc *RegimeConfig, now time.Time) DirectionalCertStatus {
	asset, tf, classifier, idOK := directionalCertIdentity(sc, rc)
	if !idOK {
		return CertNever
	}
	return getDirectionalCertStore().Status(asset, tf, classifier, now)
}

// directionalCertStartupSummary returns one operator line per strategy that
// configures regime_directional_policy, reporting its certification status
// against the loaded artifact (active/expired/never). Printed at startup and on
// SIGHUP so the default-off state (#1085) and any config-vs-evidence sign
// mismatch are operator-visible. Consults the package store (loaded by then).
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
		default: // CertNever
			out = append(out, fmt.Sprintf("[#1085] %s: regime_directional_policy DEFAULT-OFF — no certified directional edge for %s (#1076 negative result); resolves to base direction. Use the regime for ATR-scaled SL/TP sizing (#1078); disable from flat.", sc.ID, cell))
		}
	}
	return out
}

// directionalCertInspectStatus returns the certification status string and the
// (asset,timeframe,classifier) cell for a strategy, for `go-trader inspect`.
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

// directionalCertSignMismatches returns, sorted, the regime labels where the
// operator's configured policy direction contradicts the certified direction for
// an otherwise-certified cell. A non-empty result means the config asks to bet a
// side the evidence does not support for that state; the PER-STATE runtime gate
// (gatedDirectionalEntry) resolves those states to BASE direction, so this is an
// operator-visible heads-up that the configured side is inert there — never a
// state that actually trades the contradicting side.
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
		// "both" never contradicts a directional certification.
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

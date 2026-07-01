package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestNormalizeCertAsset(t *testing.T) {
	cases := map[string]string{
		"BTC/USDT": "BTC",
		"btc":      "BTC",
		"BTC-PERP": "BTC",
		"ETH/USD":  "ETH",
		"SOL_USDT": "SOL",
		"BTC:USDC": "BTC",
		"  xrp  ":  "XRP",
		"":         "",
	}
	for in, want := range cases {
		if got := normalizeCertAsset(in); got != want {
			t.Errorf("normalizeCertAsset(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseEmptyCertSetCertifiesNothing(t *testing.T) {
	data := []byte(`{"schema_version":1,"certified":[]}`)
	set, err := parseDirectionalCertSet(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, ok := set.Certified("BTC", "1h", "composite", time.Now()); ok {
		t.Fatal("empty set must certify nothing")
	}
	if st := set.Status("BTC", "1h", "composite", time.Now()); st != CertNever {
		t.Fatalf("status = %v, want CertNever", st)
	}
}

func TestCertifiedActiveAndExpired(t *testing.T) {
	now := time.Date(2026, 6, 19, 0, 0, 0, 0, time.UTC)
	future := now.Add(48 * time.Hour).Format(time.RFC3339)
	past := now.Add(-48 * time.Hour).Format(time.RFC3339)

	active := []byte(`{"schema_version":1,"certified":[
		{"asset":"BTC/USDT","timeframe":"1h","classifier":"composite","expires_at":"` + future + `",
		 "states":{"trending_up":"long","trending_down":"short"}}]}`)
	set, err := parseDirectionalCertSet(active)
	if err != nil {
		t.Fatalf("parse active: %v", err)
	}
	// Live HL args use "BTC"; artifact uses "BTC/USDT" — must reconcile.
	states, ok := set.Certified("BTC", "1h", "composite", now)
	if !ok {
		t.Fatal("expected active certification for BTC/1h/composite")
	}
	if states["trending_up"] != DirectionLong || states["trending_down"] != DirectionShort {
		t.Fatalf("unexpected states: %v", states)
	}
	if st := set.Status("btc", "1h", "composite", now); st != CertActive {
		t.Fatalf("status = %v, want CertActive", st)
	}
	// Wrong classifier / timeframe / asset must all miss.
	if _, ok := set.Certified("BTC", "1h", "adx", now); ok {
		t.Fatal("classifier mismatch must not certify")
	}
	if _, ok := set.Certified("BTC", "4h", "composite", now); ok {
		t.Fatal("timeframe mismatch must not certify")
	}
	if _, ok := set.Certified("ETH", "1h", "composite", now); ok {
		t.Fatal("asset mismatch must not certify")
	}

	expired := []byte(`{"schema_version":1,"certified":[
		{"asset":"BTC","timeframe":"1h","classifier":"composite","expires_at":"` + past + `",
		 "states":{"trending_up":"long"}}]}`)
	eset, err := parseDirectionalCertSet(expired)
	if err != nil {
		t.Fatalf("parse expired: %v", err)
	}
	if _, ok := eset.Certified("BTC", "1h", "composite", now); ok {
		t.Fatal("expired certification must not certify")
	}
	if st := eset.Status("BTC", "1h", "composite", now); st != CertExpired {
		t.Fatalf("status = %v, want CertExpired", st)
	}
}

func TestParseRejectsBadDirectionAndSchema(t *testing.T) {
	bad := []byte(`{"schema_version":1,"certified":[
		{"asset":"BTC","timeframe":"1h","classifier":"composite","states":{"trending_up":"sideways"}}]}`)
	if _, err := parseDirectionalCertSet(bad); err == nil {
		t.Fatal("expected error for invalid direction")
	}
	wrongSchema := []byte(`{"schema_version":2,"certified":[]}`)
	if _, err := parseDirectionalCertSet(wrongSchema); err == nil {
		t.Fatal("expected error for unsupported schema_version")
	}
	malformed := []byte(`{not json`)
	if _, err := parseDirectionalCertSet(malformed); err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

func TestLoadFailClosedOnMissingAndMalformed(t *testing.T) {
	// Missing file -> empty set, no warn, no error.
	set, err := LoadDirectionalCertSet("/nonexistent/regime_directional_certifications.json")
	if err != nil {
		t.Fatalf("missing file should not error, got %v", err)
	}
	if len(set.byKey) != 0 {
		t.Fatal("missing file should yield empty set")
	}

	// Malformed -> fail-closed wrapper returns empty + warns.
	dir := t.TempDir()
	path := dir + "/cert.json"
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	warned := false
	got := LoadDirectionalCertSetFailClosed(path, func(string, ...interface{}) { warned = true })
	if !warned {
		t.Fatal("malformed artifact must warn")
	}
	if _, ok := got.Certified("BTC", "1h", "composite", time.Now()); ok {
		t.Fatal("fail-closed set must certify nothing")
	}
}

func TestDirectionalCertIdentity(t *testing.T) {
	sc := StrategyConfig{
		Type: "perps",
		Args: []string{"hold", "BTC", "1h"},
	}
	asset, tf, classifier, ok := directionalCertIdentity(sc, nil)
	if !ok {
		t.Fatal("expected resolvable identity")
	}
	if asset != "BTC" || tf != "1h" || classifier != regimeClassifierADX {
		t.Fatalf("identity = (%q,%q,%q), want (BTC,1h,adx)", asset, tf, classifier)
	}

	rc := &RegimeConfig{
		Enabled:   true,
		Timeframe: "1d",
		Windows: RegimeWindowsMap{
			"medium": {Classifier: regimeClassifierComposite, Period: 30},
		},
	}
	asset, tf, classifier, ok = directionalCertIdentity(sc, rc)
	if !ok {
		t.Fatal("expected override-backed identity")
	}
	if asset != "BTC" || tf != "1d" || classifier != regimeClassifierComposite {
		t.Fatalf("override identity = (%q,%q,%q), want (BTC,1d,composite)", asset, tf, classifier)
	}

	// No symbol/timeframe -> not resolvable.
	if _, _, _, ok := directionalCertIdentity(StrategyConfig{Args: []string{"hold"}}, nil); ok {
		t.Fatal("expected unresolvable identity for short args")
	}
}

func TestApplyRegimeDirectionalPolicyDefaultOffWhenUncertified(t *testing.T) {
	sc := StrategyConfig{Direction: DirectionLong, InvertSignal: false, RegimeDirectionalPolicy: &RegimeDirectionalPolicy{
		TrendRegime: map[string]RegimeDirectionalEntry{
			"trending_down": {Direction: DirectionShort, InvertSignal: true},
			"trending_up":   {Direction: DirectionLong},
			"ranging":       {Direction: DirectionLong},
		}}}
	// certified=false → default-off: policy not applied, base config preserved.
	_, applied, _ := applyRegimeDirectionalPolicy(&sc, "trending_down", "", 0, nil)
	if applied {
		t.Fatal("uncertified policy must not apply (default-off)")
	}
	if sc.Direction != DirectionLong || sc.InvertSignal {
		t.Fatalf("sc must stay at base, got dir=%q invert=%t", sc.Direction, sc.InvertSignal)
	}
	// certified=true → applied, sc mutated to the regime's direction.
	entry, applied, _ := applyRegimeDirectionalPolicy(&sc, "trending_down", "", 0, map[string]string{"trending_down": DirectionShort, "trending_up": DirectionLong, "ranging": DirectionLong})
	if !applied || entry.Direction != DirectionShort || sc.Direction != DirectionShort {
		t.Fatalf("certified policy must apply short, got applied=%t entry=%+v dir=%q", applied, entry, sc.Direction)
	}
}

func TestStrategyDirectionalCertifiedUsesStore(t *testing.T) {
	now := time.Date(2026, 6, 19, 0, 0, 0, 0, time.UTC)
	future := now.Add(24 * time.Hour).Format(time.RFC3339)
	set, err := parseDirectionalCertSet([]byte(`{"schema_version":1,"certified":[
		{"asset":"BTC","timeframe":"1h","classifier":"adx","expires_at":"` + future + `",
		 "states":{"trending_up":"long","trending_down":"short","ranging":"long"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	prev := getDirectionalCertStore()
	setDirectionalCertStore(set)
	defer setDirectionalCertStore(prev)

	// rc=nil → classifier defaults to adx, matching the entry.
	sc := StrategyConfig{Type: "perps", Args: []string{"vwap", "BTC", "1h"}}
	if _, ok := strategyDirectionalCertified(sc, nil, now); !ok {
		t.Fatal("BTC/1h/adx should be certified via store")
	}
	if st := strategyDirectionalCertStatus(sc, nil, now); st != CertActive {
		t.Fatalf("status = %v, want CertActive", st)
	}
	scEth := StrategyConfig{Type: "perps", Args: []string{"vwap", "ETH", "1h"}}
	if _, ok := strategyDirectionalCertified(scEth, nil, now); ok {
		t.Fatal("ETH should not be certified")
	}
}

func TestDirectionalCertStartupSummary(t *testing.T) {
	prev := getDirectionalCertStore()
	setDirectionalCertStore(emptyDirectionalCertSet())
	defer setDirectionalCertStore(prev)

	cfg := &Config{Strategies: []StrategyConfig{
		{ID: "plain", Type: "perps", Args: []string{"vwap", "BTC", "1h"}}, // no policy → no line
		{ID: "dir", Type: "perps", Args: []string{"vwap", "BTC", "1h"}, RegimeDirectionalPolicy: &RegimeDirectionalPolicy{
			TrendRegime: map[string]RegimeDirectionalEntry{"trending_up": {Direction: DirectionLong}},
		}},
	}}
	lines := directionalCertStartupSummary(cfg)
	if len(lines) != 1 {
		t.Fatalf("want one summary line for the configured strategy, got %d: %v", len(lines), lines)
	}
	if !strings.Contains(lines[0], "dir") || !strings.Contains(lines[0], "DEFAULT-OFF") {
		t.Fatalf("uncertified strategy line should be default-off, got: %q", lines[0])
	}
}

func TestDirectionalCertStartupLinesNeedingOwnerDM(t *testing.T) {
	lines := []string{
		"[#1085] active: regime_directional_policy CERTIFIED for (BTC,1h,adx) — directional selection ACTIVE",
		"[#1085] dir: regime_directional_policy DEFAULT-OFF — no certified directional edge for (BTC,1h,adx)",
		"[#1085] dir: regime_directional_policy certification EXPIRED for (ETH,1h,adx)",
		"[#1085] dir: regime_directional_policy configured but symbol/timeframe unresolvable — policy inert (base direction)",
	}
	got := directionalCertStartupLinesNeedingOwnerDM(lines)
	if len(got) != 3 {
		t.Fatalf("want 3 owner-DM lines, got %d: %v", len(got), got)
	}
}

func TestNotifyDirectionalCertStartupSummary(t *testing.T) {
	mock := &mockNotifier{}
	mn := NewMultiNotifier(notifierBackend{notifier: mock, ownerID: "owner1"})
	lines := []string{
		"[#1085] ok: regime_directional_policy CERTIFIED for (BTC,1h,adx) — directional selection ACTIVE",
		"[#1085] dir: regime_directional_policy DEFAULT-OFF — no certified directional edge for (BTC,1h,adx)",
	}
	notifyDirectionalCertStartupSummary(mn, lines)
	if len(mock.dms) != 1 {
		t.Fatalf("want 1 owner DM, got %d: %v", len(mock.dms), mock.dms)
	}
	if !strings.Contains(mock.dms[0].content, "DEFAULT-OFF") {
		t.Fatalf("DM should carry DEFAULT-OFF line, got: %q", mock.dms[0].content)
	}
}

func TestDirectionalCertOperatorNotes(t *testing.T) {
	prev := getDirectionalCertStore()
	setDirectionalCertStore(emptyDirectionalCertSet())
	defer setDirectionalCertStore(prev)

	rc := &RegimeConfig{Enabled: true, Period: 14, ADXThreshold: 20}
	strategies := []StrategyConfig{{
		ID: "hl-eth", Type: "perps", Args: []string{"vwap", "ETH", "1h"},
		RegimeDirectionalPolicy: &RegimeDirectionalPolicy{
			TrendRegime: map[string]RegimeDirectionalEntry{"trending_down": {Direction: DirectionShort}},
		},
	}}
	note := directionalCertOperatorNotes(strategies, rc)
	if !strings.Contains(note, "hl-eth=DEFAULT-OFF") {
		t.Fatalf("expected DEFAULT-OFF note, got: %q", note)
	}
	if directionalCertOperatorNotes(nil, rc) != "" {
		t.Fatal("nil strategies should yield empty note")
	}
}

// Review finding 1: DirectionCertifiedAtOpen must survive a daemon restart
// (SQLite round-trip). Without persistence a CERTIFIED-at-open position reloads
// as false and is migrated to base direction (req-2 violation). Covers both
// the true and false ("inverse") cases in one save/load.
func TestDirectionCertifiedAtOpenDBRoundTrip(t *testing.T) {
	dir := t.TempDir()
	sdb, err := OpenStateDB(dir + "/state.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer sdb.Close()
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-x": {
			ID: "hl-x", Type: "perps", Platform: "hyperliquid", Cash: 1000,
			Positions: map[string]*Position{
				"BTC": {Symbol: "BTC", Quantity: 1, AvgCost: 50000, Side: "short",
					Multiplier: 1, Regime: "trending_down", DirectionCertifiedAtOpen: true,
					DirectionCertifiedStatesAtOpen: map[string]string{"trending_down": "short", "trending_up": "long", "ranging": "long"}},
				"ETH": {Symbol: "ETH", Quantity: 1, AvgCost: 3000, Side: "short",
					Multiplier: 1, Regime: "trending_down", DirectionCertifiedAtOpen: false},
			},
			OptionPositions: map[string]*OptionPosition{},
		},
	}}
	if err := sdb.SaveState(state); err != nil {
		t.Fatalf("save: %v", err)
	}
	loaded, err := sdb.LoadState()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	s := loaded.Strategies["hl-x"]
	if s == nil {
		t.Fatal("strategy not restored")
	}
	if pos := s.Positions["BTC"]; pos == nil || !pos.DirectionCertifiedAtOpen {
		t.Fatalf("certified-at-open stamp must survive restart, got %+v", pos)
	} else if pos.DirectionCertifiedStatesAtOpen["trending_down"] != "short" || len(pos.DirectionCertifiedStatesAtOpen) != 3 {
		t.Fatalf("frozen per-state cert map must survive restart, got %v", pos.DirectionCertifiedStatesAtOpen)
	}
	if pos := s.Positions["ETH"]; pos == nil || pos.DirectionCertifiedAtOpen {
		t.Fatalf("uncertified stamp must stay false across restart, got %+v", pos)
	} else if len(pos.DirectionCertifiedStatesAtOpen) != 0 {
		t.Fatalf("uncertified position must have no frozen cert map, got %v", pos.DirectionCertifiedStatesAtOpen)
	}
}

// Review finding 2 (re-review): the directional-cert stamp must be WRITE-ONCE at
// the ENTRY INSTANT — frozen by stampDirectionCertifiedAtOpenIfOpened the cycle a
// genuine open Trade is produced (OpenTrade != nil), and NEVER re-derived after.
// The prior approach tied the stamp to stampPositionRegimeFromPayload (the regime
// LABEL stamp); in multi-window mode the label warms up over several cycles, so a
// SIGHUP cert change landing between open and label-record corrupted the stamp.
// The stamp is now fully decoupled from the label: NO stampPositionRegimeIfOpened
// call (any payload, any store state) may move it. Covers the bot's must-survive
// cases (a)/(b)/(c) plus (d) the exact multi-window warmup race.
func TestDirectionCertifiedAtOpenIsWriteOnce(t *testing.T) {
	rc := &RegimeConfig{Enabled: true, Period: 14, ADXThreshold: 20}
	policy := &RegimeDirectionalPolicy{TrendRegime: map[string]RegimeDirectionalEntry{
		"trending_up":   {Direction: DirectionLong},
		"trending_down": {Direction: DirectionShort},
		"ranging":       {Direction: DirectionLong},
	}}
	sc := StrategyConfig{ID: "hl-d-btc", Type: "perps", Platform: "hyperliquid",
		Symbol: "BTC", Args: []string{"vwap", "BTC", "1h"}, RegimeDirectionalPolicy: policy}

	certifiedStore := func() *DirectionalCertSet {
		s, err := parseDirectionalCertSet([]byte(`{"schema_version":1,"certified":[
			{"asset":"BTC","timeframe":"1h","classifier":"adx",
			 "states":{"trending_up":"long","trending_down":"short","ranging":"long"}}]}`))
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	newState := func() *StrategyState {
		return &StrategyState{ID: sc.ID, Positions: map[string]*Position{
			"BTC": {Symbol: "BTC", Quantity: 1, AvgCost: 50000, Side: "short", OwnerStrategyID: sc.ID},
		}}
	}
	prev := getDirectionalCertStore()
	defer setDirectionalCertStore(prev)

	// (a) Uncertified at open → false; a later SIGHUP certification AND the cycle
	// that finally records the regime label must NOT flip it.
	setDirectionalCertStore(emptyDirectionalCertSet())
	ssA := newState()
	stampDirectionCertifiedAtOpenIfOpened(ssA, "BTC", true /*opened*/, sc, rc)
	if ssA.Positions["BTC"].DirectionCertifiedAtOpen {
		t.Fatal("(a) uncertified-at-open must stamp false")
	}
	if len(ssA.Positions["BTC"].DirectionCertifiedStatesAtOpen) != 0 {
		t.Fatal("(a) uncertified-at-open must freeze a nil per-state map")
	}
	setDirectionalCertStore(certifiedStore())
	stampDirectionCertifiedAtOpenIfOpened(ssA, "BTC", false /*no new open*/, sc, rc)
	stampPositionRegimeIfOpened(ssA, "BTC", RegimePayload{Legacy: "ranging"}, sc, rc)
	if ssA.Positions["BTC"].DirectionCertifiedAtOpen {
		t.Fatal("(a) a later SIGHUP certification / label-record cycle must NOT flip an open position to true")
	}
	if ssA.Positions["BTC"].Regime != "ranging" {
		t.Fatalf("(a) the regime label must still record independently, got %q", ssA.Positions["BTC"].Regime)
	}
	if len(ssA.Positions["BTC"].DirectionCertifiedStatesAtOpen) != 0 {
		t.Fatal("(a) a later SIGHUP/label cycle must not populate the frozen per-state map")
	}

	// (b) Certified at open → true; later de-certification + label-record must NOT flip it.
	setDirectionalCertStore(certifiedStore())
	ssB := newState()
	stampDirectionCertifiedAtOpenIfOpened(ssB, "BTC", true, sc, rc)
	if !ssB.Positions["BTC"].DirectionCertifiedAtOpen {
		t.Fatal("(b) certified-at-open must stamp true")
	}
	if ssB.Positions["BTC"].DirectionCertifiedStatesAtOpen["ranging"] != "long" {
		t.Fatalf("(b) certified-at-open must freeze the per-state map, got %v", ssB.Positions["BTC"].DirectionCertifiedStatesAtOpen)
	}
	setDirectionalCertStore(emptyDirectionalCertSet())
	stampDirectionCertifiedAtOpenIfOpened(ssB, "BTC", false, sc, rc)
	stampPositionRegimeIfOpened(ssB, "BTC", RegimePayload{Legacy: "ranging"}, sc, rc)
	if !ssB.Positions["BTC"].DirectionCertifiedAtOpen {
		t.Fatal("(b) de-certification / label-record cycle must NOT flip an open position to false")
	}
	if ssB.Positions["BTC"].DirectionCertifiedStatesAtOpen["ranging"] != "long" {
		t.Fatal("(b) de-cert/label cycle must not clear the frozen per-state map")
	}

	// (c) A position whose regime label records on the first post-open cycle still
	// stamps the OPEN-cycle verdict (verdict frozen at open, label stamps alongside).
	setDirectionalCertStore(certifiedStore())
	ssC := newState()
	stampDirectionCertifiedAtOpenIfOpened(ssC, "BTC", true, sc, rc)
	stampPositionRegimeIfOpened(ssC, "BTC", RegimePayload{Legacy: "trending_down"}, sc, rc)
	if ssC.Positions["BTC"].Regime != "trending_down" {
		t.Fatalf("(c) the regime label must record on the first post-open cycle, got %q", ssC.Positions["BTC"].Regime)
	}
	if !ssC.Positions["BTC"].DirectionCertifiedAtOpen {
		t.Fatal("(c) the stamp must equal the open-cycle verdict (certified)")
	}

	// (d) The exact finding-2 multi-window warmup race: a position opens while the
	// gate/primary window label is still empty (payload non-empty because a
	// shorter window is present but unlabeled), a SIGHUP certifies the cell, and
	// THEN the primary window fills and the label records. The stamp must reflect
	// the OPEN-instant (uncertified) verdict, not the SIGHUP-changed one.
	rcMW := &RegimeConfig{Enabled: true, Period: 14, Windows: RegimeWindowsMap{
		"short":  {Classifier: "adx", Period: 14},
		"medium": {Classifier: "composite", Period: 48},
	}}
	certifiedMW := func() *DirectionalCertSet {
		s, err := parseDirectionalCertSet([]byte(`{"schema_version":1,"certified":[
			{"asset":"BTC","timeframe":"1h","classifier":"composite",
			 "states":{"trending_up_clean":"long","trending_down_clean":"short"}}]}`))
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	// This multi-window config keys the cert on the composite directional window —
	// so a wrongful re-evaluation against certifiedMW WOULD flip the stamp; the fix
	// prevents that.
	if _, _, cls, ok := directionalCertIdentity(sc, rcMW); !ok || cls != "composite" {
		t.Fatalf("(d) expected composite directional classifier, got %q ok=%v", cls, ok)
	}
	setDirectionalCertStore(emptyDirectionalCertSet())
	ssD := newState()
	stampDirectionCertifiedAtOpenIfOpened(ssD, "BTC", true /*open instant*/, sc, rcMW)
	// Warmup cycle: only "short" present and unlabeled → gate AND primary labels
	// resolve empty → the regime label must NOT record yet, cert untouched.
	warmup := RegimePayload{MultiMode: true, Windows: map[string]RegimeSnapshot{"short": {Regime: ""}}}
	stampPositionRegimeIfOpened(ssD, "BTC", warmup, sc, rcMW)
	if ssD.Positions["BTC"].Regime != "" {
		t.Fatalf("(d) warmup must not record a regime label, got %q", ssD.Positions["BTC"].Regime)
	}
	// SIGHUP certifies the cell mid-warmup, then the primary window fills.
	setDirectionalCertStore(certifiedMW())
	filled := RegimePayload{MultiMode: true, Windows: map[string]RegimeSnapshot{"medium": {Regime: "trending_down_clean"}}}
	stampPositionRegimeIfOpened(ssD, "BTC", filled, sc, rcMW)
	if ssD.Positions["BTC"].Regime != "trending_down_clean" {
		t.Fatalf("(d) the regime label must record once the primary window fills, got %q", ssD.Positions["BTC"].Regime)
	}
	if ssD.Positions["BTC"].DirectionCertifiedAtOpen {
		t.Fatal("(d) warmup→SIGHUP-certify→label-record must NOT flip the open stamp to true")
	}
	if len(ssD.Positions["BTC"].DirectionCertifiedStatesAtOpen) != 0 {
		t.Fatal("(d) the frozen per-state map must stay empty (opened uncertified)")
	}
}

func TestDirectionalCertSignMismatches(t *testing.T) {
	sc := StrategyConfig{}
	pol := &RegimeDirectionalPolicy{TrendRegime: map[string]RegimeDirectionalEntry{
		"trending_up":   {Direction: DirectionShort}, // contradicts certified long
		"trending_down": {Direction: DirectionShort}, // matches
		"ranging":       {Direction: DirectionBoth},  // never contradicts
	}}
	sc.RegimeDirectionalPolicy = pol
	certStates := map[string]string{
		"trending_up":   DirectionLong,
		"trending_down": DirectionShort,
		"ranging":       DirectionLong,
	}
	got := directionalCertSignMismatches(sc, certStates)
	if len(got) != 1 {
		t.Fatalf("expected 1 mismatch, got %v", got)
	}
	if got[0] != "trending_up: config=short certified=long" {
		t.Fatalf("unexpected mismatch text: %q", got[0])
	}
}

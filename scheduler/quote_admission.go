package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type quoteAdmissionKind string

const (
	quoteAdmissionResearch quoteAdmissionKind = "research"
	quoteAdmissionFixture  quoteAdmissionKind = "fixture"
)

type quoteLoadAdmission struct {
	kind         quoteAdmissionKind
	ownerID      string
	stateDir     string
	configDigest string

	mu    sync.Mutex
	bound bool
}

func (a *quoteLoadAdmission) Kind() quoteAdmissionKind {
	if a == nil {
		return ""
	}
	return a.kind
}

func (a *quoteLoadAdmission) OwnerID() string {
	if a == nil {
		return ""
	}
	return a.ownerID
}

func (a *quoteLoadAdmission) ConfigFingerprint() string {
	if a == nil {
		return ""
	}
	return a.configDigest
}

func quoteConfigDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func newQuoteResearchLoadAdmission(ownerID, stateDir string, configBytes []byte) (*quoteLoadAdmission, error) {
	return newQuoteLoadAdmission(quoteAdmissionResearch, ownerID, stateDir, configBytes)
}

func newQuoteLoadAdmission(kind quoteAdmissionKind, ownerID, stateDir string, configBytes []byte) (*quoteLoadAdmission, error) {
	ownerID = strings.TrimSpace(ownerID)
	if ownerID == "" {
		return nil, fmt.Errorf("quote admission needs an owner strategy id")
	}
	if strings.TrimSpace(stateDir) == "" {
		return nil, fmt.Errorf("quote admission needs a run state directory")
	}
	abs, err := filepath.Abs(stateDir)
	if err != nil {
		return nil, fmt.Errorf("resolve quote run state directory: %w", err)
	}
	if eval, err := filepath.EvalSymlinks(abs); err == nil {
		abs = eval
	}
	if len(configBytes) == 0 {
		return nil, fmt.Errorf("quote admission needs the exact configuration bytes")
	}
	return &quoteLoadAdmission{kind: kind, ownerID: ownerID, stateDir: abs, configDigest: quoteConfigDigest(configBytes)}, nil
}

type configLoadOptions struct {
	quoteLoad     *quoteLoadAdmission
	pairAdmission *pairResearchAdmission
}

func loadQuoteAdmittedConfigData(data []byte, adm *quoteLoadAdmission) (*Config, error) {
	if adm == nil {
		return nil, fmt.Errorf("the quote-admitted loader needs a load admission")
	}
	if quoteConfigDigest(data) != adm.configDigest {
		return nil, fmt.Errorf("the configuration bytes do not match the admitted digest")
	}
	return loadConfigDataOpts("", append([]byte(nil), data...), true, true, configLoadOptions{quoteLoad: adm})
}

func quoteAdmissionErrors(cfg *Config) []string {
	if cfg == nil {
		return nil
	}
	adm := cfg.quoteLoad
	if adm == nil {
		errs := productionRefusedAdmissionErrors(cfg.Strategies)
		return append(errs, quoteProductionRefusalErrors(cfg.Strategies)...)
	}
	var errs []string
	owners := 0
	var owner *StrategyConfig
	for i := range cfg.Strategies {
		sc := &cfg.Strategies[i]
		if sc.Quote != nil {
			owners++
		}
		if sc.ID == adm.ownerID {
			owner = sc
			continue
		}
		for _, ref := range strategyQuoteReferences(*sc) {
			errs = append(errs, fmt.Sprintf("strategy %s: %s is refused; the quote admission covers only owner %s", sc.ID, ref, adm.ownerID))
		}
	}
	if owner == nil {
		errs = append(errs, fmt.Sprintf("the quote admission names owner %s, which the configuration does not hold", adm.ownerID))
	} else {
		if owner.Quote == nil {
			errs = append(errs, fmt.Sprintf("strategy %s: the admitted quote owner has no quote block", owner.ID))
		}
		errs = append(errs, quoteAdmittedOwnerErrors(adm, *owner)...)
	}
	if owners != 1 {
		errs = append(errs, fmt.Sprintf("a quote-admitted configuration holds exactly one quote block, got %d", owners))
	}
	for _, sc := range cfg.Strategies {
		for _, ref := range productionRefusedReferences(sc) {
			if owner != nil && sc.ID == owner.ID && ref.Role == edgeRefRoleOpen {
				if _, seam := quoteCapabilityStrategies[ref.Name]; seam {
					continue
				}
			}
			errs = append(errs, fmt.Sprintf("strategy %s: %s strategy %q is production_refused (research-only, evidence %s): every config is refused in any mode, with or without allow_no_edge, until a separate reviewed promotion",
				sc.ID, ref.Role, ref.Name, productionRefusedStrategies[ref.Name]))
		}
	}
	return errs
}

func quoteExplicitPaperModeCount(args []string) (paper, other int) {
	for _, a := range args {
		switch {
		case a == "--mode=paper":
			paper++
		case strings.HasPrefix(a, "--mode=") || a == "--mode":
			other++
		}
	}
	return paper, other
}

func quoteAdmittedOwnerErrors(adm *quoteLoadAdmission, sc StrategyConfig) []string {
	errs := quoteOwnerConfigErrors(sc)
	open := effectiveOpenStrategy(sc)
	spec, inSeam := quoteCapabilityStrategies[open]
	part := partitionFor(sc)
	switch adm.kind {
	case quoteAdmissionResearch:
		if part.Scope != ScopePaper {
			errs = append(errs, fmt.Sprintf("strategy %s: the research quote admission needs a paper partition, got %s", sc.ID, partitionLabel(part)))
		}
		if paper, other := quoteExplicitPaperModeCount(sc.Args); paper != 1 || other != 0 {
			errs = append(errs, fmt.Sprintf("strategy %s: the research quote admission needs exactly one explicit --mode=paper argument", sc.ID))
		}
		if !inSeam {
			errs = append(errs, fmt.Sprintf("strategy %s: open strategy %q is not a quote capability", sc.ID, open))
		} else if spec.Validate != nil {
			if err := spec.Validate(sc.OpenStrategy.Params); err != nil {
				errs = append(errs, fmt.Sprintf("strategy %s: open strategy %s params: %v", sc.ID, open, err))
			}
		}
	case quoteAdmissionFixture:
		if inSeam {
			if spec.Validate != nil {
				if err := spec.Validate(sc.OpenStrategy.Params); err != nil {
					errs = append(errs, fmt.Sprintf("strategy %s: open strategy %s params: %v", sc.ID, open, err))
				}
			}
		} else {
			if _, registered := registeredOpenStrategyPlatforms[open]; registered {
				errs = append(errs, fmt.Sprintf("strategy %s: a fixture quote owner outside the capability seam must not name a registered open strategy (%q)", sc.ID, open))
			}
			if len(sc.OpenStrategy.Params) > 0 {
				errs = append(errs, fmt.Sprintf("strategy %s: a fixture quote owner outside the capability seam takes empty params", sc.ID))
			}
		}
	default:
		errs = append(errs, fmt.Sprintf("strategy %s: unknown quote admission kind %q", sc.ID, adm.kind))
	}
	return errs
}

type quoteAdmission struct {
	kind      quoteAdmissionKind
	load      *quoteLoadAdmission
	cfg       *Config
	sc        StrategyConfig
	partition RiskPartition
	store     *StateStore
	db        *StateDB
	venue     quoteVenue
	clock     func() time.Time
	state     *AppState
	mu        *sync.RWMutex
	notifier  *MultiNotifier
	policyFP  string
	configFP  string
	prices    quotePriceRule

	opMu       sync.Mutex
	shutdown   bool
	outbox     []func()
	closed     bool
	holdAlerts map[string]bool
}

func (a *quoteAdmission) Now() time.Time {
	return a.clock().UTC()
}

func (a *quoteAdmission) OwnerID() string { return a.sc.ID }

func (a *quoteAdmission) Partition() RiskPartition { return a.partition }

func (a *quoteAdmission) State() *AppState { return a.state }

func (a *quoteAdmission) Mutex() *sync.RWMutex { return a.mu }

func (a *quoteAdmission) Store() *StateStore { return a.store }

func (a *quoteAdmission) PolicyFingerprint() string { return a.policyFP }

func (a *quoteAdmission) ConfigFingerprint() string { return a.configFP }

func (a *quoteAdmission) SetNotifier(n *MultiNotifier) { a.notifier = n }

var quoteBoundAdmissions sync.Map

func quoteAdmissionForStore(store *StateStore) *quoteAdmission {
	if store == nil {
		return nil
	}
	v, ok := quoteBoundAdmissions.Load(store)
	if !ok {
		return nil
	}
	return v.(*quoteAdmission)
}

func (a *quoteAdmission) Close() {
	if a == nil {
		return
	}
	a.opMu.Lock()
	defer a.opMu.Unlock()
	if a.closed {
		return
	}
	a.closed = true
	quoteBoundAdmissions.CompareAndDelete(a.store, a)
}

func quoteOwnershipHolds(owned *stateOwnership, spec storageFileSpec) bool {
	if owned == nil {
		return false
	}
	want := stateDBLockPath(spec.Path)
	for _, l := range owned.locks {
		if l != nil && l.f != nil && l.path == want {
			return true
		}
	}
	return false
}

func bindQuoteAdmission(load *quoteLoadAdmission, cfg *Config, store *StateStore, ownership *stateOwnership, venue quoteVenue, clock func() time.Time) (*quoteAdmission, error) {
	if load == nil || cfg == nil || store == nil || venue == nil || clock == nil {
		return nil, fmt.Errorf("quote admission binding needs a load admission, configuration, store, venue and clock")
	}
	if cfg.quoteLoad != load {
		return nil, fmt.Errorf("the configuration was not loaded through this quote admission")
	}
	if errs := quoteAdmissionErrors(cfg); len(errs) > 0 {
		return nil, fmt.Errorf("quote admission refused: %s", strings.Join(errs, "; "))
	}
	var sc *StrategyConfig
	for i := range cfg.Strategies {
		if cfg.Strategies[i].ID == load.ownerID {
			sc = &cfg.Strategies[i]
		}
	}
	if sc == nil {
		return nil, fmt.Errorf("quote owner %s is missing from the configuration", load.ownerID)
	}
	layout, err := resolveStorageLayout(cfg)
	if err != nil {
		return nil, err
	}
	storeLayout := store.Layout()
	if len(storeLayout.Files) != len(layout.Files) {
		return nil, fmt.Errorf("the store was not opened from this configuration's storage layout")
	}
	for i, f := range layout.Files {
		sf := storeLayout.Files[i]
		if sf.Role != f.Role || canonicalDBPath(sf.Path) != canonicalDBPath(f.Path) {
			return nil, fmt.Errorf("the store file %s (%s) does not match the configured %s file %s", sf.Role, sf.Path, f.Role, f.Path)
		}
		if f.InMemory {
			return nil, fmt.Errorf("quote storage needs real files, got an in-memory %s file", f.Role)
		}
		if filepath.Dir(canonicalDBPath(f.Path)) != load.stateDir {
			return nil, fmt.Errorf("state file %q is outside the admitted run state directory", f.Path)
		}
		if st, err := os.Stat(f.Path); err != nil || !st.Mode().IsRegular() {
			return nil, fmt.Errorf("state file %q is not a regular file", f.Path)
		}
		if !quoteOwnershipHolds(ownership, f) {
			return nil, fmt.Errorf("ownership of state file %q is not held", f.Path)
		}
	}
	ident, err := buildStorageIdentityMap(cfg, layout)
	if err != nil {
		return nil, err
	}
	want, ok := ident.storageFor(sc.ID)
	got, gotOK := store.ident.storageFor(sc.ID)
	if store.Split() && (!ok || !gotOK || want != got) {
		return nil, fmt.Errorf("the store identity map does not own quote owner %s the way the configuration does", sc.ID)
	}
	part := partitionFor(*sc)
	db, err := store.dbForStrategy(sc.ID)
	if err != nil {
		return nil, err
	}
	if role := layout.roleForPartition(part); role != db.storageRoleOf() {
		return nil, fmt.Errorf("quote owner %s resolves to the %s file, its partition to %s", sc.ID, db.storageRoleOf(), role)
	}
	acct := venue.AccountKey(part)
	if strings.TrimSpace(acct) == "" || strings.TrimSpace(venue.Provider()) == "" {
		return nil, fmt.Errorf("the quote venue names no provider or account for %s", partitionLabel(part))
	}
	switch load.kind {
	case quoteAdmissionResearch:
		if venue.LiveShaped() || venue.Provider() != "paper" || acct != "paper:"+part.String() {
			return nil, fmt.Errorf("the research quote admission needs a paper venue with account paper:%s, got provider %q account %q live-shaped %v", part.String(), venue.Provider(), acct, venue.LiveShaped())
		}
	case quoteAdmissionFixture:
		if (part.Scope == ScopeLive) != venue.LiveShaped() {
			return nil, fmt.Errorf("the quote venue live-shaped flag %v does not match the %s partition", venue.LiveShaped(), partitionLabel(part))
		}
	default:
		return nil, fmt.Errorf("unknown quote admission kind %q", load.kind)
	}
	policyFP, err := quotePolicyFingerprint(*sc)
	if err != nil {
		return nil, err
	}
	load.mu.Lock()
	if load.bound {
		load.mu.Unlock()
		return nil, fmt.Errorf("this quote load admission is already bound")
	}
	load.bound = true
	load.mu.Unlock()
	state, orphans, err := LoadStateWithStore(cfg, store)
	if err != nil {
		return nil, err
	}
	if len(orphans) > 0 {
		var ids []string
		for _, o := range orphans {
			ids = append(ids, fmt.Sprintf("%v", o))
		}
		sort.Strings(ids)
		fmt.Printf("[quote] storage orphans while binding %s: %s\n", sc.ID, strings.Join(ids, "; "))
	}
	adm := &quoteAdmission{
		kind:       load.kind,
		load:       load,
		cfg:        cfg,
		sc:         *sc,
		partition:  part,
		store:      store,
		db:         db,
		venue:      venue,
		clock:      clock,
		state:      state,
		mu:         &sync.RWMutex{},
		policyFP:   policyFP,
		configFP:   load.configDigest,
		holdAlerts: map[string]bool{},
	}
	adm.prices = newQuotePriceRule(hlLotMetadata.Peek(quoteCoinOf(*sc)))
	if err := adm.checkExclusiveOwnership(); err != nil {
		return nil, err
	}
	if err := adm.attachRuntime(); err != nil {
		return nil, err
	}
	quoteBoundAdmissions.Store(store, adm)
	return adm, nil
}

func (a *quoteAdmission) checkExclusiveOwnership() error {
	coin := quoteCoinOf(a.sc)
	for _, other := range a.cfg.Strategies {
		if other.ID == a.sc.ID {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(hyperliquidSymbol(other.Args)), coin) || other.Platform != "hyperliquid" {
			continue
		}
		op := partitionFor(other)
		if a.partition.Scope == ScopeLive && op.Scope == ScopeLive {
			return fmt.Errorf("quote owner %s needs exclusive ownership of the live %s namespace, but %s also trades it", a.sc.ID, coin, other.ID)
		}
		if a.partition.Scope == ScopePaper && op == a.partition {
			return fmt.Errorf("quote owner %s needs exclusive ownership of %s in the %s partition, but %s also trades it", a.sc.ID, coin, partitionLabel(a.partition), other.ID)
		}
	}
	return nil
}

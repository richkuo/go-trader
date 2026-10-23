package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func quoteTestWriteConfig(t *testing.T, dir string, blob []byte) string {
	t.Helper()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestQuoteProductionEntryPathsRefuseEveryQuoteOwnedConfig(t *testing.T) {
	useHLLotMetadataForTest(t, map[string]int{"BTC": 3})
	dir := t.TempDir()
	variants := map[string][]byte{
		"quote block":          quoteTestConfigBytes(t, dir, quoteTestOptions{}),
		"allow_no_edge":        quoteTestConfigBytes(t, dir, quoteTestOptions{strategy: map[string]any{"allow_no_edge": true}}),
		"live args":            quoteTestConfigBytes(t, dir, quoteTestOptions{partition: "live"}),
		"close reference only": quoteTestConfigBytes(t, dir, quoteTestOptions{strategy: map[string]any{"quote": nil}}),
	}
	for name, blob := range variants {
		path := quoteTestWriteConfig(t, t.TempDir(), blob)
		loaders := map[string]func() (*Config, error){
			"LoadConfig":                 func() (*Config, error) { return LoadConfig(path) },
			"LoadConfigReadOnly":         func() (*Config, error) { return LoadConfigReadOnly(path) },
			"LoadConfigForProbe":         func() (*Config, error) { return LoadConfigForProbe(path) },
			"LoadConfigForLedgerExport":  func() (*Config, error) { return LoadConfigForLedgerExport(blob) },
			"loadConfigData basket form": func() (*Config, error) { return loadConfigData("", append([]byte(nil), blob...), true, true) },
		}
		for lname, load := range loaders {
			if cfg, err := load(); err == nil || cfg != nil {
				t.Fatalf("%s via %s must refuse the quote-owned configuration, got cfg=%v err=%v", name, lname, cfg != nil, err)
			}
		}
		var direct Config
		if err := json.Unmarshal(blob, &direct); err != nil {
			t.Fatal(err)
		}
		if errs := quoteAdmissionErrors(&direct); len(errs) == 0 {
			t.Fatalf("%s: a directly built configuration has no admission and must be refused", name)
		}
		if err := validateConfig(&direct, true); err == nil {
			t.Fatalf("%s: validateConfig must refuse a directly built quote owner", name)
		}
		sc := direct.Strategies[0]
		inferStrategyPlatform(&sc)
		if hyperliquidBatchEligible(sc) {
			t.Fatalf("%s: a quote owner is never batch eligible", name)
		}
		prevCheck := runHyperliquidCheckFn
		runHyperliquidCheckFn = func(string, []string) (*HyperliquidResult, string, error) {
			t.Fatalf("%s: scalar dispatch spawned a check for a quote owner", name)
			return nil, "", nil
		}
		if _, _, _, ok := runHyperliquidCheck(&sc, map[string]float64{"BTC": 100}, PositionCtx{}, nil, "simple", nil, newTestLogger(t), nil, nil); ok {
			t.Fatalf("%s: scalar and composer dispatch must refuse a quote owner", name)
		}
		runHyperliquidCheckFn = prevCheck
		if res, ok := runHyperliquidExecuteOrder(sc, &HyperliquidResult{Signal: 1, Symbol: "BTC"}, 100, 1000, true, 0, "", 0, 1, 0, nil, nil, hlExecuteSnapshot{}, hlCloseContext{}, HurstGateDecision{}, nil, newTestLogger(t)); ok || res != nil {
			t.Fatalf("%s: live execution must refuse a quote owner", name)
		}
		if out := runHyperliquidPerpsStep(&hlPerpsStep{sc: sc, logger: newTestLogger(t), stratState: &StrategyState{ID: sc.ID, Quote: &quoteOwnerState{}}}); out.Ran || out.Trades != 0 {
			t.Fatalf("%s: the scalar perps step must not run for a quote owner: %+v", name, out)
		}
		if _, _, err := lookupForceCloseStrategy(&Config{Strategies: []StrategyConfig{withLiveArgs(sc)}}, sc.ID); err == nil {
			t.Fatalf("%s: force-close must refuse a quote owner", name)
		}
	}
	if _, _, err := buildAddStrategyEntry(inventoryQuoteExitCloseName, "hyperliquid", "BTC"); err == nil {
		t.Fatal("operator add-strategy must refuse the research-only close")
	}
	quoteCapabilityStrategies["quote_fixture_seam"] = quoteCapabilitySpec{Version: 1}
	defer delete(quoteCapabilityStrategies, "quote_fixture_seam")
	if openStrategyRegisteredFor("quote_fixture_seam", "futures") {
		t.Fatal("a seam name must never register as an ordinary open strategy")
	}
	if _, _, err := buildAddStrategyEntry("quote_fixture_seam", "hyperliquid", "BTC"); err == nil {
		t.Fatal("operator add-strategy must refuse a seam name")
	}
}

func withLiveArgs(sc StrategyConfig) StrategyConfig {
	out := sc
	out.Args = []string{"quote_fixture", "BTC", "1m", "--mode=live"}
	return out
}

func TestQuoteAdmittedLoaderAndBindingRefuseEveryMismatch(t *testing.T) {
	useHLLotMetadataForTest(t, map[string]int{"BTC": 3})
	dir := t.TempDir()
	if eval, err := filepath.EvalSymlinks(dir); err == nil {
		dir = eval
	}
	good := quoteTestConfigBytes(t, dir, quoteTestOptions{})
	load, err := newQuoteFixtureLoadAdmission(quoteTestOwner, dir, good)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loadQuoteAdmittedConfigData(append(append([]byte(nil), good...), ' '), load); err == nil {
		t.Fatal("a digest mismatch must refuse the admitted load")
	}
	refused := map[string][]byte{
		"second quote owner": quoteTestConfigBytes(t, dir, quoteTestOptions{extra: []map[string]any{{
			"id": "hl-quote-eth", "type": "perps", "platform": "hyperliquid", "script": "shared_scripts/check_hyperliquid.py",
			"args": []string{"quote_fixture", "ETH", "1m", "--mode=paper"}, "open_strategy": map[string]any{"name": "quote_fixture"},
			"close_strategy": map[string]any{"name": inventoryQuoteExitCloseName}, "capital": 1000, "leverage": 5, "stop_loss_pct": 5,
			"quote": map[string]any{"coin": "ETH", "max_inventory_usd": 100, "max_order_qty": 1, "request_expiry_ms": 1000},
		}}}),
		"ATR stop owner":       quoteTestConfigBytes(t, dir, quoteTestOptions{stopFields: map[string]any{"stop_loss_atr_mult": 2}}),
		"trailing stop owner":  quoteTestConfigBytes(t, dir, quoteTestOptions{stopFields: map[string]any{"trailing_stop_pct": 3}}),
		"default ATR fallback": quoteTestConfigBytes(t, dir, quoteTestOptions{stopFields: map[string]any{}}),
		"two percent owners":   quoteTestConfigBytes(t, dir, quoteTestOptions{stopFields: map[string]any{"stop_loss_pct": 3, "stop_loss_margin_pct": 10}}),
		"bankruptcy bound":     quoteTestConfigBytes(t, dir, quoteTestOptions{stopFields: map[string]any{"stop_loss_pct": 25}}),
		"scale-in":             quoteTestConfigBytes(t, dir, quoteTestOptions{strategy: map[string]any{"allow_scale_in": true}}),
		"wrong close owner":    quoteTestConfigBytes(t, dir, quoteTestOptions{strategy: map[string]any{"close_strategy": map[string]any{"name": "tiered_tp_atr"}}}),
		"boolean as number":    quoteTestConfigBytes(t, dir, quoteTestOptions{quote: map[string]any{"max_order_qty": true}}),
		"unknown quote key":    quoteTestConfigBytes(t, dir, quoteTestOptions{quote: map[string]any{"spread_bps": 3}}),
		"missing quote coin":   quoteTestConfigBytes(t, dir, quoteTestOptions{quote: map[string]any{"coin": nil}}),
	}
	for name, blob := range refused {
		l, err := newQuoteFixtureLoadAdmission(quoteTestOwner, dir, blob)
		if err != nil {
			t.Fatal(err)
		}
		if cfg, err := loadQuoteAdmittedConfigData(blob, l); err == nil || cfg != nil {
			t.Fatalf("%s must be refused by the admitted loader", name)
		}
	}
	research, err := newQuoteResearchLoadAdmission(quoteTestOwner, dir, good)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loadQuoteAdmittedConfigData(good, research); err == nil {
		t.Fatal("the research admission must refuse an open name outside the capability seam")
	}
	seamCalls := 0
	quoteCapabilityStrategies["quote_fixture"] = quoteCapabilitySpec{Version: 3, Validate: func(params map[string]interface{}) error {
		seamCalls++
		if params["bad"] != nil {
			return errors.New("bad parameter")
		}
		return nil
	}}
	defer delete(quoteCapabilityStrategies, "quote_fixture")
	research2, _ := newQuoteResearchLoadAdmission(quoteTestOwner, dir, good)
	if cfg, err := loadQuoteAdmittedConfigData(good, research2); err != nil || cfg == nil || seamCalls == 0 {
		t.Fatalf("the research admission must accept a seam owner in paper: %v (validator calls %d)", err, seamCalls)
	}
	liveBlob := quoteTestConfigBytes(t, dir, quoteTestOptions{partition: "live"})
	research3, _ := newQuoteResearchLoadAdmission(quoteTestOwner, dir, liveBlob)
	if _, err := loadQuoteAdmittedConfigData(liveBlob, research3); err == nil {
		t.Fatal("the research admission must refuse a live partition")
	}
	badParams := quoteTestConfigBytes(t, dir, quoteTestOptions{strategy: map[string]any{"open_strategy": map[string]any{"name": "quote_fixture", "params": map[string]any{"bad": 1}}}})
	research4, _ := newQuoteResearchLoadAdmission(quoteTestOwner, dir, badParams)
	if _, err := loadQuoteAdmittedConfigData(badParams, research4); err == nil {
		t.Fatal("the seam validator must refuse bad parameters")
	}
	delete(quoteCapabilityStrategies, "quote_fixture")

	cfg, err := loadQuoteAdmittedConfigData(good, load)
	if err != nil {
		t.Fatal(err)
	}
	layout, _ := resolveStorageLayout(cfg)
	ident, _ := buildStorageIdentityMap(cfg, layout)
	store, err := OpenStateStore(layout, ident)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	clock := &quoteTestClock{now: quoteTestStart}
	part := partitionFor(cfg.Strategies[0])
	venue := newQuoteFixtureVenue(clock.Now, false, part)
	if _, err := bindQuoteAdmission(load, cfg, store, nil, venue, clock.Now); err == nil {
		t.Fatal("binding without held ownership must be refused")
	}
	owned, err := acquireStateOwnership(layout.Files)
	if err != nil {
		t.Fatal(err)
	}
	defer owned.Release()
	if _, err := bindQuoteAdmission(load, cfg, store, owned, newQuoteFixtureVenue(clock.Now, true, part), clock.Now); err == nil {
		t.Fatal("a live-shaped venue must not bind a paper partition")
	}
	other, _ := newQuoteFixtureLoadAdmission(quoteTestOwner, dir, good)
	if _, err := bindQuoteAdmission(other, cfg, store, owned, venue, clock.Now); err == nil {
		t.Fatal("a configuration from another load admission must be refused")
	}
	adm, err := bindQuoteAdmission(load, cfg, store, owned, venue, clock.Now)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	defer adm.Close()
	if _, err := bindQuoteAdmission(load, cfg, store, owned, venue, clock.Now); err == nil {
		t.Fatal("a load admission binds once")
	}

	outside := t.TempDir()
	stray := quoteTestConfigBytes(t, dir, quoteTestOptions{root: map[string]any{"db_file": filepath.Join(outside, "state.db")}})
	l2, _ := newQuoteFixtureLoadAdmission(quoteTestOwner, dir, stray)
	cfg2, err := loadQuoteAdmittedConfigData(stray, l2)
	if err != nil {
		t.Fatal(err)
	}
	layout2, _ := resolveStorageLayout(cfg2)
	ident2, _ := buildStorageIdentityMap(cfg2, layout2)
	store2, err := OpenStateStore(layout2, ident2)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	owned2, err := acquireStateOwnership(layout2.Files)
	if err != nil {
		t.Fatal(err)
	}
	defer owned2.Release()
	if _, err := bindQuoteAdmission(l2, cfg2, store2, owned2, venue, clock.Now); err == nil {
		t.Fatal("a state file outside the admitted directory must be refused")
	}
}

func TestQuoteReloadRefusesOwnershipChangesWhileEvidenceIsUnresolved(t *testing.T) {
	e := newQuoteTestEnv(t, quoteTestOptions{})
	e.mustRun(1, 100, e.pair("99", "", "0.2"), quoteExitRequest{})
	next := *e.cfg
	next.Strategies = append([]StrategyConfig(nil), e.cfg.Strategies...)
	changed := *next.Strategies[0].Quote
	changed.MaxInventoryUSD = 1
	next.Strategies[0].Quote = &changed
	if errs := quoteReloadErrors(e.cfg, &next, e.adm.state); len(errs) != 1 {
		t.Fatalf("flat inventory with an unresolved quote must refuse a quote-block reload: %v", errs)
	}
	removed := *e.cfg
	removed.Strategies = nil
	if errs := quoteReloadErrors(e.cfg, &removed, e.adm.state); len(errs) != 1 {
		t.Fatalf("removing a busy quote owner must be refused: %v", errs)
	}
	if errs := quoteReloadErrors(e.cfg, e.cfg, e.adm.state); len(errs) != 0 {
		t.Fatalf("an unchanged reload must pass: %v", errs)
	}
}

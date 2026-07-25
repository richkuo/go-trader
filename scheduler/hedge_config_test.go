package main

import (
	"reflect"
	"strings"
	"testing"
)

func hedgedPerpsStrategy(id, coin, hedge string) StrategyConfig {
	return StrategyConfig{
		ID:       id,
		Type:     "perps",
		Platform: "hyperliquid",
		Script:   "shared_scripts/check_hyperliquid.py",
		Args:     []string{"--mode=paper", coin},
		Hedge:    &HedgeConfig{Enabled: true, Symbol: hedge},
	}
}

func joinErrs(errs []string) string { return strings.Join(errs, "\n") }

func TestValidateHedgeConfigsAcceptsAValidBlock(t *testing.T) {
	sc := hedgedPerpsStrategy("eth", "ETH", "BTC")
	sc.Hedge.Ratio = 1.5
	sc.Hedge.MarginMode = "cross"
	sc.Hedge.Leverage = 3
	sc.Hedge.Platform = "hyperliquid"
	sc.Hedge.Type = "perps"
	sc.Hedge.Side = "inverse"
	if errs := validateHedgeConfigs([]StrategyConfig{sc}); len(errs) != 0 {
		t.Fatalf("valid hedge block rejected:\n%s", joinErrs(errs))
	}
}

func TestValidateHedgeConfigsNoBlockIsSilent(t *testing.T) {
	sc := hedgedPerpsStrategy("eth", "ETH", "BTC")
	sc.Hedge = nil
	if errs := validateHedgeConfigs([]StrategyConfig{sc}); len(errs) != 0 {
		t.Fatalf("a strategy with no hedge block must produce no errors:\n%s", joinErrs(errs))
	}
}

// A parked (disabled) block is still shape-validated — an operator who later
// flips enabled=true must not discover the typo at trade time.
func TestValidateHedgeConfigsValidatesDisabledBlocks(t *testing.T) {
	sc := hedgedPerpsStrategy("eth", "ETH", "BTC")
	sc.Hedge.Enabled = false
	sc.Hedge.Side = "correlated"
	errs := validateHedgeConfigs([]StrategyConfig{sc})
	if !strings.Contains(joinErrs(errs), "side must be") {
		t.Fatalf("a disabled block must still be shape-validated, got:\n%s", joinErrs(errs))
	}
}

func TestValidateHedgeConfigsRejectsNonHLPerps(t *testing.T) {
	cases := []struct {
		name     string
		platform string
		typ      string
	}{
		{"okx perps", "okx", "perps"},
		{"hl spot", "hyperliquid", "spot"},
		{"hl manual", "hyperliquid", "manual"},
		{"hl options", "hyperliquid", "options"},
		{"topstep futures", "topstep", "futures"},
	}
	for _, c := range cases {
		sc := hedgedPerpsStrategy("s", "ETH", "BTC")
		sc.Platform = c.platform
		sc.Type = c.typ
		errs := validateHedgeConfigs([]StrategyConfig{sc})
		if !strings.Contains(joinErrs(errs), "Hyperliquid perps only") {
			t.Errorf("%s: expected an HL-perps-only rejection, got:\n%s", c.name, joinErrs(errs))
		}
	}
}

func TestValidateHedgeConfigsVocabularyAndBounds(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*HedgeConfig)
		wantSub string
	}{
		{"bad side", func(h *HedgeConfig) { h.Side = "correlated" }, "side must be"},
		{"bad platform", func(h *HedgeConfig) { h.Platform = "okx" }, "platform must be"},
		{"bad type", func(h *HedgeConfig) { h.Type = "spot" }, "type must be"},
		{"ratio too high", func(h *HedgeConfig) { h.Ratio = 11 }, "ratio must be"},
		{"negative ratio", func(h *HedgeConfig) { h.Ratio = -1 }, "ratio must be"},
		{"bad margin mode", func(h *HedgeConfig) { h.MarginMode = "portfolio" }, "margin_mode must be"},
		{"negative leverage", func(h *HedgeConfig) { h.Leverage = -2 }, "leverage must be"},
		{"absurd leverage", func(h *HedgeConfig) { h.Leverage = 500 }, "leverage must be"},
		{"empty symbol", func(h *HedgeConfig) { h.Symbol = "" }, "symbol is empty"},
		{"unparseable symbol", func(h *HedgeConfig) { h.Symbol = "/USDC" }, "symbol is empty"},
	}
	for _, c := range cases {
		sc := hedgedPerpsStrategy("s", "ETH", "BTC")
		c.mutate(sc.Hedge)
		errs := validateHedgeConfigs([]StrategyConfig{sc})
		if !strings.Contains(joinErrs(errs), c.wantSub) {
			t.Errorf("%s: expected %q in errors, got:\n%s", c.name, c.wantSub, joinErrs(errs))
		}
	}
}

// Collision matrix — phase-1 constraint 2. Each arm is what makes hedge coins
// sole-owned, which is what lets every shared-coin mechanism stay untouched.
func TestValidateHedgeConfigsRejectsOwnCoinCollision(t *testing.T) {
	sc := hedgedPerpsStrategy("eth", "ETH", "ETH")
	errs := validateHedgeConfigs([]StrategyConfig{sc})
	if !strings.Contains(joinErrs(errs), "own primary coin") {
		t.Fatalf("a same-coin hedge must be rejected, got:\n%s", joinErrs(errs))
	}
}

func TestValidateHedgeConfigsRejectsPeerCoinCollision(t *testing.T) {
	eth := hedgedPerpsStrategy("eth", "ETH", "BTC")
	btc := hedgedPerpsStrategy("btc", "BTC", "SOL")
	errs := validateHedgeConfigs([]StrategyConfig{eth, btc})
	joined := joinErrs(errs)
	if !strings.Contains(joined, "configured trading coin of strategy/strategies btc") {
		t.Fatalf("hedging into a peer's trading coin must be rejected, got:\n%s", joined)
	}
}

// A PAPER peer still collides: the coin is shared on the wallet the moment
// either side goes live, and flipping --mode is a one-word edit.
func TestValidateHedgeConfigsRejectsPaperPeerCoinCollision(t *testing.T) {
	eth := hedgedPerpsStrategy("eth", "ETH", "BTC")
	eth.Args = []string{"--mode=live", "ETH"}
	btcPaper := StrategyConfig{
		ID: "btc-paper", Type: "perps", Platform: "hyperliquid",
		Script: "shared_scripts/check_hyperliquid.py", Args: []string{"--mode=paper", "BTC"},
	}
	errs := validateHedgeConfigs([]StrategyConfig{eth, btcPaper})
	if !strings.Contains(joinErrs(errs), "btc-paper") {
		t.Fatalf("a paper peer on the hedge coin must still collide, got:\n%s", joinErrs(errs))
	}
}

// A MANUAL strategy's coin is equally off-limits — it trades the same wallet.
func TestValidateHedgeConfigsRejectsManualPeerCoinCollision(t *testing.T) {
	eth := hedgedPerpsStrategy("eth", "ETH", "BTC")
	manual := StrategyConfig{
		ID: "btc-manual", Type: "manual", Platform: "hyperliquid",
		Symbol: "BTC", Args: []string{"--mode=live", "BTC"},
	}
	errs := validateHedgeConfigs([]StrategyConfig{eth, manual})
	if !strings.Contains(joinErrs(errs), "btc-manual") {
		t.Fatalf("a manual peer on the hedge coin must collide, got:\n%s", joinErrs(errs))
	}
}

func TestValidateHedgeConfigsRejectsHedgeVsHedgeCollision(t *testing.T) {
	a := hedgedPerpsStrategy("a", "ETH", "BTC")
	b := hedgedPerpsStrategy("b", "SOL", "btc/usdc:usdc") // same coin, different spelling
	errs := validateHedgeConfigs([]StrategyConfig{a, b})
	joined := joinErrs(errs)
	if !strings.Contains(joined, "declared by more than one strategy") {
		t.Fatalf("two hedges on one coin must be rejected, got:\n%s", joined)
	}
	if !strings.Contains(joined, "a, b") {
		t.Errorf("the error must name both owners in sorted order, got:\n%s", joined)
	}
}

// Two hedges on the same coin must be reported ONCE, not once per owner.
func TestValidateHedgeConfigsHedgeCollisionReportedOncePerCoin(t *testing.T) {
	a := hedgedPerpsStrategy("a", "ETH", "BTC")
	b := hedgedPerpsStrategy("b", "SOL", "BTC")
	c := hedgedPerpsStrategy("c", "AVAX", "BTC")
	errs := validateHedgeConfigs([]StrategyConfig{a, b, c})
	n := 0
	for _, e := range errs {
		if strings.Contains(e, "declared by more than one strategy") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("expected exactly one hedge-vs-hedge error, got %d:\n%s", n, joinErrs(errs))
	}
}

func TestValidateHedgeConfigsErrorsAreSorted(t *testing.T) {
	a := hedgedPerpsStrategy("zzz", "ETH", "ETH")
	b := hedgedPerpsStrategy("aaa", "SOL", "SOL")
	errs := validateHedgeConfigs([]StrategyConfig{a, b})
	if len(errs) < 2 {
		t.Fatalf("expected at least two errors, got:\n%s", joinErrs(errs))
	}
	for i := 1; i < len(errs); i++ {
		if errs[i-1] > errs[i] {
			t.Fatalf("errors must be sorted for deterministic operator output:\n%s", joinErrs(errs))
		}
	}
}

// ---------------------------------------------------------------------------
// Unknown-key guard
// ---------------------------------------------------------------------------

func TestValidateHedgeJSONKeysFlagsTypos(t *testing.T) {
	raw := []byte(`{"strategies":[{"id":"eth","hedge":{"enabled":true,"symbol":"BTC","ration":2,"leverge":3}}]}`)
	errs := validateHedgeJSONKeys(raw)
	joined := joinErrs(errs)
	if !strings.Contains(joined, `"leverge"`) || !strings.Contains(joined, `"ration"`) {
		t.Fatalf("both typos must be flagged, got:\n%s", joined)
	}
	if !strings.Contains(joined, "strategy[eth].hedge") {
		t.Errorf("errors must be keyed by strategy id, got:\n%s", joined)
	}
}

func TestValidateHedgeJSONKeysAcceptsEveryDeclaredField(t *testing.T) {
	raw := []byte(`{"strategies":[{"id":"eth","hedge":{"enabled":true,"symbol":"BTC","side":"inverse","ratio":1,"platform":"hyperliquid","type":"perps","margin_mode":"cross","leverage":3}}]}`)
	if errs := validateHedgeJSONKeys(raw); len(errs) != 0 {
		t.Fatalf("every declared field must pass, got:\n%s", joinErrs(errs))
	}
}

func TestKnownHedgeConfigKeysCoversTheStruct(t *testing.T) {
	known := knownHedgeConfigKeys()
	for _, k := range []string{"enabled", "symbol", "side", "ratio", "platform", "type", "margin_mode", "leverage"} {
		if !known[k] {
			t.Errorf("knownHedgeConfigKeys missing %q — did a HedgeConfig json tag get renamed?", k)
		}
	}
}

func TestKnownStrategyConfigKeysIncludesHedge(t *testing.T) {
	if !knownStrategyConfigKeys()["hedge"] {
		t.Fatal("the top-level hedge key must be recognized by the strategy unknown-key guard")
	}
}

// ---------------------------------------------------------------------------
// Hot reload
// ---------------------------------------------------------------------------

func TestHedgeConfigEqual(t *testing.T) {
	a := &HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 1}
	b := &HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 1}
	if !hedgeConfigEqual(a, b) {
		t.Error("identical blocks must compare equal")
	}
	if hedgeConfigEqual(a, &HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 2}) {
		t.Error("a ratio change must compare unequal")
	}
	if hedgeConfigEqual(a, nil) || hedgeConfigEqual(nil, a) {
		t.Error("nil vs non-nil must compare unequal (adding/removing a hedge is a change)")
	}
	if !hedgeConfigEqual(nil, nil) {
		t.Error("nil vs nil must compare equal")
	}
}

func TestCloneHedgeConfigIsDeep(t *testing.T) {
	src := &HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 2}
	clone := cloneHedgeConfig(src)
	clone.Ratio = 9
	if src.Ratio != 2 {
		t.Error("cloneHedgeConfig must not alias the source")
	}
	if cloneHedgeConfig(nil) != nil {
		t.Error("cloning nil must yield nil")
	}
}

// strategyRestartShape must MASK the hedge block, otherwise a pure hedge edit
// is misreported as "restart required" instead of being hot-reloaded when flat.
func TestStrategyRestartShapeMasksHedge(t *testing.T) {
	a := hedgedPerpsStrategy("eth", "ETH", "BTC")
	b := hedgedPerpsStrategy("eth", "ETH", "SOL")
	b.Hedge.Ratio = 2
	if !reflect.DeepEqual(strategyRestartShape(a), strategyRestartShape(b)) {
		t.Fatal("a hedge-only change must not be flagged restart-required")
	}
}

func hedgeReloadState(open bool) *AppState {
	st := NewAppState()
	ss := &StrategyState{ID: "eth", Type: "perps", Platform: "hyperliquid", Positions: map[string]*Position{}}
	if open {
		ss.Positions["ETH"] = &Position{Symbol: "ETH", Quantity: 2, Side: "long", AvgCost: 3000, Multiplier: 1}
	}
	st.Strategies["eth"] = ss
	return st
}

func TestHedgeHotReloadBlockedWhileOpen(t *testing.T) {
	cur := &Config{Strategies: []StrategyConfig{hedgedPerpsStrategy("eth", "ETH", "BTC")}}
	next := &Config{Strategies: []StrategyConfig{hedgedPerpsStrategy("eth", "ETH", "BTC")}}
	next.Strategies[0].Hedge.Ratio = 2

	if err := validateHotReloadStateCompatible(cur, next, hedgeReloadState(true)); err == nil {
		t.Fatal("a hedge-block change with an open position must be rejected")
	} else if !strings.Contains(err.Error(), "hedge block changed with open positions") {
		t.Fatalf("unexpected rejection message: %v", err)
	}
	if err := validateHotReloadStateCompatible(cur, next, hedgeReloadState(false)); err != nil {
		t.Fatalf("a hedge-block change while FLAT must be accepted, got: %v", err)
	}
}

// The guard must also fire when only the HEDGE LEG is still open (primary
// already flat) — that is exactly the state the reconciler is mid-unwind in.
func TestHedgeHotReloadBlockedWhileOnlyTheHedgeLegIsOpen(t *testing.T) {
	cur := &Config{Strategies: []StrategyConfig{hedgedPerpsStrategy("eth", "ETH", "BTC")}}
	next := &Config{Strategies: []StrategyConfig{hedgedPerpsStrategy("eth", "ETH", "BTC")}}
	next.Strategies[0].Hedge.Symbol = "SOL"

	st := hedgeReloadState(false)
	st.Strategies["eth"].Positions["BTC"] = &Position{
		Symbol: "BTC", Quantity: 0.1, Side: "short", AvgCost: 60000, Multiplier: 1, HedgeFor: "ETH",
	}
	if err := validateHotReloadStateCompatible(cur, next, st); err == nil {
		t.Fatal("a residual hedge leg must still block a hedge-block change")
	}
}

func TestHedgeHotReloadAddAndRemoveBlockedWhileOpen(t *testing.T) {
	withHedge := &Config{Strategies: []StrategyConfig{hedgedPerpsStrategy("eth", "ETH", "BTC")}}
	noHedge := &Config{Strategies: []StrategyConfig{hedgedPerpsStrategy("eth", "ETH", "BTC")}}
	noHedge.Strategies[0].Hedge = nil

	if err := validateHotReloadStateCompatible(withHedge, noHedge, hedgeReloadState(true)); err == nil {
		t.Error("REMOVING a hedge block while open must be rejected")
	}
	if err := validateHotReloadStateCompatible(noHedge, withHedge, hedgeReloadState(true)); err == nil {
		t.Error("ADDING a hedge block while open must be rejected")
	}
}

// A reload must not be able to introduce a collision startup would have caught.
func TestHedgeHotReloadRejectsIntroducedCollision(t *testing.T) {
	cur := &Config{Strategies: []StrategyConfig{
		hedgedPerpsStrategy("eth", "ETH", "BTC"),
		{ID: "sol", Type: "perps", Platform: "hyperliquid", Script: "s.py", Args: []string{"--mode=paper", "SOL"}},
	}}
	next := &Config{Strategies: []StrategyConfig{
		hedgedPerpsStrategy("eth", "ETH", "SOL"), // now collides with the sol strategy
		{ID: "sol", Type: "perps", Platform: "hyperliquid", Script: "s.py", Args: []string{"--mode=paper", "SOL"}},
	}}
	err := validateHotReloadCompatible(cur, next)
	if err == nil || !strings.Contains(err.Error(), "configured trading coin") {
		t.Fatalf("a reload-introduced hedge collision must be rejected, got: %v", err)
	}
}

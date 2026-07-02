package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// #1150: pausedBlocksSignal must hold everything that grows exposure (fresh
// open, add, flip, the legacy fresh-open edge) and pass everything that can
// only reduce it (close-registry actions, pure-close directional exits).
func TestPausedBlocksSignal(t *testing.T) {
	cases := []struct {
		name          string
		signal        int
		closeFraction float64
		posQty        float64
		posSide       string
		allowsLong    bool
		allowsShort   bool
		want          bool
	}{
		// Hold signal: nothing to block, manage path runs as normal.
		{"hold flat", 0, 0, 0, "", true, false, false},
		{"hold open", 0, 0, 1, "long", true, true, false},

		// Flat: every non-zero signal is a fresh open.
		{"flat buy", 1, 0, 0, "", true, false, true},
		{"flat sell short-capable", -1, 0, 0, "", true, true, true},
		{"flat stale close action", -1, 1.0, 0, "", true, false, true},

		// Close-registry actions on an open position pass (partial and full).
		{"open long partial close", -1, 0.5, 2, "long", true, true, false},
		{"open long full close", -1, 1.0, 2, "long", true, true, false},
		{"open short partial close", 1, 0.25, 3, "short", true, true, false},

		// Pure-close directional exits pass (mirrors perpsCloseActionSuppressesNewSL).
		{"long-only sell exit", -1, 0, 2, "long", true, false, false},
		{"short-only buy exit", 1, 0, 2, "short", false, true, false},
		{"spot sell", -1, 0, 1.5, "long", true, false, false},

		// Adds / re-affirms on an open position are held.
		{"open long buy add", 1, 0, 2, "long", true, false, true},
		{"open long buy add both", 1, 0, 2, "long", true, true, true},
		{"open short sell add", -1, 0, 2, "short", true, true, true},
		{"spot buy while long", 1, 0, 1.5, "long", true, false, true},

		// Flips (direction=both, opposite side, closeFraction 0) are held —
		// a flip closes AND reopens the other side.
		{"long flip to short", -1, 0, 2, "long", true, true, true},
		{"short flip to long", 1, 0, 2, "short", true, true, true},

		// Legacy edge (#656): buy on a short under direction="long" fresh-sizes
		// a new long without offset — exposure grows, so it is held.
		{"legacy buy on short under long", 1, 0, 2, "short", true, false, true},

		// Futures dispatch args (allowsLong=allowsShort=true):
		// ExecuteFuturesSignal is unconditionally bidirectional — a sell on a
		// long (closeFraction 0) closes AND opens a fresh short, a buy on a
		// short mirrors it — so opposite-side signals are held; only
		// close-registry actions pass, and flat signals are fresh opens.
		{"futures long sell flip", -1, 0, 3, "long", true, true, true},
		{"futures short buy flip", 1, 0, 3, "short", true, true, true},
		{"futures long partial registry close", -1, 0.5, 3, "long", true, true, false},
		{"futures long full registry close", -1, 1.0, 3, "long", true, true, false},
		{"futures flat sell fresh short", -1, 0, 0, "", true, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := pausedBlocksSignal(tc.signal, tc.closeFraction, tc.posQty, tc.posSide, tc.allowsLong, tc.allowsShort)
			if got != tc.want {
				t.Fatalf("pausedBlocksSignal(signal=%d cf=%.2f qty=%.1f side=%q long=%t short=%t) = %t, want %t",
					tc.signal, tc.closeFraction, tc.posQty, tc.posSide, tc.allowsLong, tc.allowsShort, got, tc.want)
			}
		})
	}
}

// #1150: only "close" options actions survive a pause — "buy" and "sell" both
// OPEN option legs.
func TestPausedOptionsActions(t *testing.T) {
	actions := []OptionsAction{
		{Action: "buy", Strike: 100},
		{Action: "close", Strike: 110},
		{Action: "sell", Strike: 120},
		{Action: "close", Strike: 130},
	}
	kept, dropped := pausedOptionsActions(actions)
	if dropped != 2 {
		t.Fatalf("expected 2 dropped open actions, got %d", dropped)
	}
	if len(kept) != 2 || kept[0].Strike != 110 || kept[1].Strike != 130 {
		t.Fatalf("expected the two close actions in order, got %+v", kept)
	}

	kept, dropped = pausedOptionsActions(nil)
	if kept != nil || dropped != 0 {
		t.Fatalf("expected nil/0 for empty input, got %+v/%d", kept, dropped)
	}
}

// #1150: /status paused note lists paused strategy IDs sorted; empty when none.
func TestPausedStrategiesNote(t *testing.T) {
	if note := pausedStrategiesNote([]StrategyConfig{{ID: "a"}, {ID: "b"}}); note != "" {
		t.Fatalf("expected empty note with no paused strategies, got %q", note)
	}
	note := pausedStrategiesNote([]StrategyConfig{
		{ID: "z-strat", Paused: true},
		{ID: "a-strat"},
		{ID: "m-strat", Paused: true},
	})
	if !strings.Contains(note, "m-strat, z-strat") {
		t.Fatalf("expected sorted paused IDs, got %q", note)
	}
	if !strings.Contains(note, "⏸️") {
		t.Fatalf("expected pause marker in note, got %q", note)
	}
}

// #1150: `paused` unmarshals from strategy JSON.
func TestPausedConfigUnmarshal(t *testing.T) {
	var sc StrategyConfig
	if err := json.Unmarshal([]byte(`{"id":"x","paused":true}`), &sc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !sc.Paused {
		t.Fatal("expected paused=true after unmarshal")
	}
	var sc2 StrategyConfig
	if err := json.Unmarshal([]byte(`{"id":"x"}`), &sc2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if sc2.Paused {
		t.Fatal("expected paused=false when omitted")
	}
}

// #1150: a paused-only change must not register in the restart shape (else
// validateHotReloadCompatible would flag it as restart-required).
func TestStrategyRestartShape_PausedOnlyChange(t *testing.T) {
	a := StrategyConfig{ID: "hl-a", Paused: true}
	b := StrategyConfig{ID: "hl-a", Paused: false}
	if !reflect.DeepEqual(strategyRestartShape(a), strategyRestartShape(b)) {
		t.Fatal("paused-only change should not affect restart shape")
	}
}

// #1150: the pause toggle is hot-reloadable always, including while a position
// is open — it must NOT be rejected by the reload validators, and the new
// value must actually be applied to the running config.
func TestApplyHotReloadConfig_PausedToggleWhileOpen(t *testing.T) {
	base := func(paused bool) []StrategyConfig {
		return []StrategyConfig{{
			ID: "hl-eth", Type: "perps", Platform: "hyperliquid",
			Script:  "shared_scripts/check_hyperliquid.py",
			Args:    []string{"momentum", "ETH", "1h", "--mode=paper"},
			Capital: 1000, MaxDrawdownPct: 10, Leverage: 2, Direction: DirectionLong,
			Paused: paused,
		}}
	}
	openState := func() *AppState {
		return &AppState{Strategies: map[string]*StrategyState{
			"hl-eth": {
				ID: "hl-eth", Cash: 900,
				RiskState: RiskState{MaxDrawdownPct: 10},
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: 1, Side: "long", AvgCost: 3000, Leverage: 2},
				},
			},
		}}
	}

	// pause while a position is open: accepted, value applied, change logged.
	cfg := minimalReloadConfig(base(false))
	next := minimalReloadConfig(base(true))
	changes, err := applyHotReloadConfig(cfg, next, openState(), nil, nil)
	if err != nil {
		t.Fatalf("paused false->true while open should be hot-reloadable: %v", err)
	}
	if !cfg.Strategies[0].Paused {
		t.Fatal("expected strategy paused after reload")
	}
	if !strings.Contains(strings.Join(changes, "\n"), "paused") {
		t.Fatalf("expected a paused change entry, got %v", changes)
	}

	// resume while a position is open: accepted, value applied.
	cfg = minimalReloadConfig(base(true))
	next = minimalReloadConfig(base(false))
	if _, err := applyHotReloadConfig(cfg, next, openState(), nil, nil); err != nil {
		t.Fatalf("paused true->false while open should be hot-reloadable: %v", err)
	}
	if cfg.Strategies[0].Paused {
		t.Fatal("expected strategy resumed after reload")
	}
}

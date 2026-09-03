package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func replaySourceIDPairConfig() *Config {
	return &Config{
		ReplayLogPath: "/var/lib/go-trader/shared/replay.db",
		Strategies: []StrategyConfig{
			{
				ID: "hl-x-live", Type: "perps", Platform: "hyperliquid",
				Script:        "shared_scripts/check_hyperliquid.py",
				Args:          []string{"vwap", "ETH", "1h", "--mode=live"},
				ReplaySharing: ReplaySharingLiveMirror,
			},
			{
				ID: "hl-x-paper", Type: "perps", Platform: "hyperliquid",
				Script:         "shared_scripts/check_hyperliquid.py",
				Args:           []string{"vwap", "ETH", "1h", "--mode=paper"},
				ReplaySharing:  ReplaySharingLiveMirror,
				ReplaySourceID: "hl-x-live",
			},
		},
	}
}

func TestReplayMirrorSourceIDResolution(t *testing.T) {
	paper := func(mut func(*StrategyConfig)) StrategyConfig {
		sc := StrategyConfig{
			ID: "hl-x-paper", Type: "perps", Platform: "hyperliquid",
			Args: []string{"vwap", "ETH", "1h", "--mode=paper"}, ReplaySharing: ReplaySharingLiveMirror,
		}
		if mut != nil {
			mut(&sc)
		}
		return sc
	}

	cases := []struct {
		name       string
		sc         StrategyConfig
		wantSource string
		wantActive bool
	}{
		{"paper mirror without source id keys on its own id", paper(nil), "hl-x-paper", true},
		{"paper mirror names another source", paper(func(sc *StrategyConfig) { sc.ReplaySourceID = "hl-x-live" }), "hl-x-live", true},
		{"blank source id falls back to own id", paper(func(sc *StrategyConfig) { sc.ReplaySourceID = "   " }), "hl-x-paper", true},
		{"live strategy is never a mirror", paper(func(sc *StrategyConfig) { sc.Args = []string{"vwap", "ETH", "1h", "--mode=live"} }), "", false},
		{"replay sharing off", paper(func(sc *StrategyConfig) { sc.ReplaySharing = ReplaySharingNone; sc.ReplaySourceID = "hl-x-live" }), "", false},
		{"spot is out of scope", paper(func(sc *StrategyConfig) { sc.Type = "spot" }), "", false},
		{"non-hyperliquid is out of scope", paper(func(sc *StrategyConfig) { sc.Platform = "okx" }), "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := replayMirrorSourceID(tc.sc); got != tc.wantSource {
				t.Errorf("replayMirrorSourceID = %q, want %q", got, tc.wantSource)
			}
			if got := replayMirrorPaperActive(tc.sc); got != tc.wantActive {
				t.Errorf("replayMirrorPaperActive = %t, want %t", got, tc.wantActive)
			}
		})
	}
}

func TestReplayMirrorCrossProcessSameIDUnchanged(t *testing.T) {
	db, err := OpenDecisionLogDB(filepath.Join(t.TempDir(), "replay.db"))
	if err != nil {
		t.Fatalf("OpenDecisionLogDB: %v", err)
	}
	defer db.Close()

	sc, s, logger := replayMirrorTestSetup(t, "hl-vwap-eth-60")
	if sc.ReplaySourceID != "" {
		t.Fatalf("fixture must not set replay_source_id, got %q", sc.ReplaySourceID)
	}
	decidedAt := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	if err := db.InsertDecision(ReplayDecision{
		StrategyID: "hl-vwap-eth-60", DecisionType: ReplayDecisionOpen, DecidedAt: decidedAt,
		Symbol: "ETH", Side: "long", Quantity: 0.5, ReferencePrice: 1908.25,
	}); err != nil {
		t.Fatalf("InsertDecision: %v", err)
	}

	sourceID := replayMirrorSourceID(sc)
	if sourceID != sc.ID {
		t.Fatalf("source id = %q, want the strategy's own id %q", sourceID, sc.ID)
	}
	if reset := syncReplayMirrorWatermarkSource(sc, s, sourceID, logger); reset {
		t.Fatal("a same-id mirror must not reset its watermark")
	}
	pending, err := db.PendingDecisions(sourceID)
	if err != nil {
		t.Fatalf("PendingDecisions: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending = %d, want 1", len(pending))
	}
	applied, trades, _, _ := applyReplayedLiveDecisions(sc, s, pending, 1910.0, replayTestResult(), &Config{}, logger)
	if trades != 1 || len(applied) != 1 {
		t.Fatalf("trades=%d applied=%v, want 1 trade booked", trades, applied)
	}
	pos := s.Positions["ETH"]
	if pos == nil || pos.Quantity != 0.5 || pos.Side != "long" {
		t.Fatalf("position mismatch: %+v", pos)
	}
}

func TestReplayMirrorAppliesDecisionsFromNamedSource(t *testing.T) {
	db, err := OpenDecisionLogDB(filepath.Join(t.TempDir(), "replay.db"))
	if err != nil {
		t.Fatalf("OpenDecisionLogDB: %v", err)
	}
	defer db.Close()

	sc, s, logger := replayMirrorTestSetup(t, "hl-x-paper")
	sc.ReplaySourceID = "hl-x-live"
	decidedAt := time.Date(2026, 9, 1, 11, 0, 0, 0, time.UTC)
	if err := db.InsertDecision(ReplayDecision{
		StrategyID: "hl-x-live", DecisionType: ReplayDecisionOpen, DecidedAt: decidedAt,
		Symbol: "ETH", Side: "long", Quantity: 0.4, ReferencePrice: 1900,
	}); err != nil {
		t.Fatalf("InsertDecision: %v", err)
	}

	if rows, err := db.PendingDecisions(sc.ID); err != nil || len(rows) != 0 {
		t.Fatalf("pending under the paper id = %v (err %v), want none — the live source owns the rows", rows, err)
	}

	sourceID := replayMirrorSourceID(sc)
	if sourceID != "hl-x-live" {
		t.Fatalf("source id = %q, want hl-x-live", sourceID)
	}
	syncReplayMirrorWatermarkSource(sc, s, sourceID, logger)
	pending, err := db.PendingDecisions(sourceID)
	if err != nil {
		t.Fatalf("PendingDecisions: %v", err)
	}
	applied, trades, _, _ := applyReplayedLiveDecisions(sc, s, pending, 1901.0, replayTestResult(), &Config{}, logger)
	if trades != 1 || len(applied) != 1 {
		t.Fatalf("trades=%d applied=%v, want the live decision booked into the paper book", trades, applied)
	}
	pos := s.Positions["ETH"]
	if pos == nil || pos.Quantity != 0.4 || pos.AvgCost != 1900 {
		t.Fatalf("position mismatch: %+v", pos)
	}
	if err := db.MarkDecisionsApplied(applied); err != nil {
		t.Fatalf("MarkDecisionsApplied: %v", err)
	}
	rest, err := db.PendingDecisions(sourceID)
	if err != nil {
		t.Fatalf("PendingDecisions after mark: %v", err)
	}
	if len(rest) != 0 {
		t.Fatalf("pending after mark = %d, want 0", len(rest))
	}
}

func TestReplaySourceIDValidation(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(cfg *Config)
		wantErr string
	}{
		{"in-process live/paper pair loads", nil, ""},
		{
			"missing source",
			func(cfg *Config) { cfg.Strategies[1].ReplaySourceID = "hl-typo-live" },
			"names no strategy in this config",
		},
		{
			"source is paper",
			func(cfg *Config) { cfg.Strategies[0].Args = []string{"vwap", "ETH", "1h", "--mode=paper"} },
			"the replay source must run live",
		},
		{
			"source trades another symbol",
			func(cfg *Config) { cfg.Strategies[0].Args = []string{"vwap", "BTC", "1h", "--mode=live"} },
			"the mirror must track the same symbol and timeframe",
		},
		{
			"source trades another timeframe",
			func(cfg *Config) { cfg.Strategies[0].Args = []string{"vwap", "ETH", "15m", "--mode=live"} },
			"the mirror must track the same symbol and timeframe",
		},
		{
			"source does not share its decisions",
			func(cfg *Config) { cfg.Strategies[0].ReplaySharing = ReplaySharingNone },
			"it never writes decisions",
		},
		{
			"mirror without replay_sharing",
			func(cfg *Config) { cfg.Strategies[1].ReplaySharing = ReplaySharingNone },
			"replay_source_id requires replay_sharing",
		},
		{
			"source id on a live strategy",
			func(cfg *Config) { cfg.Strategies[0].ReplaySourceID = "hl-x-paper" },
			"only valid on a paper strategy",
		},
		{
			"source id on a spot strategy",
			func(cfg *Config) { cfg.Strategies[1].Type = "spot" },
			"HL perps strategies only",
		},
		{
			"self reference",
			func(cfg *Config) { cfg.Strategies[1].ReplaySourceID = "hl-x-paper" },
			"must name another strategy",
		},
		{
			"two mirrors of one source",
			func(cfg *Config) {
				second := cfg.Strategies[1]
				second.ID = "hl-x-paper-2"
				cfg.Strategies = append(cfg.Strategies, second)
			},
			"one paper mirror per live source",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := replaySourceIDPairConfig()
			if tc.mutate != nil {
				tc.mutate(cfg)
			}
			err := validateConfig(cfg, true)
			if tc.wantErr == "" {
				if err != nil && strings.Contains(err.Error(), "replay_source_id") {
					t.Fatalf("valid pair rejected: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestOrderReplaySourcesBeforeMirrors(t *testing.T) {
	live := func(id string) StrategyConfig {
		return StrategyConfig{ID: id, Type: "perps", Platform: "hyperliquid",
			Args: []string{"vwap", "ETH", "1h", "--mode=live"}, ReplaySharing: ReplaySharingLiveMirror}
	}
	mirror := func(id, source string) StrategyConfig {
		return StrategyConfig{ID: id, Type: "perps", Platform: "hyperliquid",
			Args: []string{"vwap", "ETH", "1h", "--mode=paper"}, ReplaySharing: ReplaySharingLiveMirror, ReplaySourceID: source}
	}
	plain := func(id string) StrategyConfig {
		return StrategyConfig{ID: id, Type: "perps", Platform: "hyperliquid", Args: []string{"sma", "BTC", "1h", "--mode=paper"}}
	}

	cases := []struct {
		name string
		due  []StrategyConfig
		want []string
	}{
		{
			"mirror ahead of its source moves behind it",
			[]StrategyConfig{mirror("p", "l"), plain("a"), live("l"), plain("b")},
			[]string{"a", "l", "p", "b"},
		},
		{
			"source already ahead keeps the order",
			[]StrategyConfig{live("l"), mirror("p", "l"), plain("a")},
			[]string{"l", "p", "a"},
		},
		{
			"cross-process mirror with no source id keeps the order",
			[]StrategyConfig{mirror("p", ""), plain("a")},
			[]string{"p", "a"},
		},
		{
			"source outside the due list keeps the order",
			[]StrategyConfig{mirror("p", "l"), plain("a")},
			[]string{"p", "a"},
		},
		{
			"two mirrors of two sources each land behind their own source",
			[]StrategyConfig{mirror("p1", "l1"), mirror("p2", "l2"), live("l2"), live("l1")},
			[]string{"l2", "p2", "l1", "p1"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := orderReplaySourcesBeforeMirrors(tc.due)
			ids := make([]string, 0, len(got))
			for _, sc := range got {
				ids = append(ids, sc.ID)
			}
			if strings.Join(ids, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("order = %v, want %v", ids, tc.want)
			}
			if len(got) != len(tc.due) {
				t.Fatalf("len = %d, want %d — no strategy may be dropped", len(got), len(tc.due))
			}
		})
	}
}

func TestSyncReplayMirrorWatermarkSourceResetsOnChange(t *testing.T) {
	sc, s, _ := replayMirrorTestSetup(t, "hl-x-paper")
	var buf bytes.Buffer
	logger := &StrategyLogger{stratID: sc.ID, writer: &buf}

	s.ReplayMirrorWatermark = 41
	replayMirrorSetLastApplied(sc.ID, 41)
	if reset := syncReplayMirrorWatermarkSource(sc, s, sc.ID, logger); reset {
		t.Fatal("a legacy row with no recorded source must keep its watermark under the same id")
	}
	if s.ReplayMirrorWatermark != 41 || s.ReplayMirrorWatermarkSource != sc.ID {
		t.Fatalf("watermark=%d source=%q, want 41 / %q", s.ReplayMirrorWatermark, s.ReplayMirrorWatermarkSource, sc.ID)
	}

	if reset := syncReplayMirrorWatermarkSource(sc, s, "hl-x-live", logger); !reset {
		t.Fatal("source change must reset the watermark")
	}
	if s.ReplayMirrorWatermark != 0 {
		t.Fatalf("watermark = %d after source change, want 0", s.ReplayMirrorWatermark)
	}
	if s.ReplayMirrorWatermarkSource != "hl-x-live" {
		t.Fatalf("recorded source = %q, want hl-x-live", s.ReplayMirrorWatermarkSource)
	}
	if got := replayMirrorLastApplied(sc.ID); got != 0 {
		t.Fatalf("in-memory progress = %d after source change, want 0", got)
	}
	out := buf.String()
	if !strings.Contains(out, "WARN") || !strings.Contains(out, "source changed") || !strings.Contains(out, "hl-x-live") {
		t.Fatalf("missing source-change WARN, got: %q", out)
	}

	buf.Reset()
	if reset := syncReplayMirrorWatermarkSource(sc, s, "hl-x-live", logger); reset {
		t.Fatal("an unchanged source must not reset the watermark")
	}
	if buf.Len() != 0 {
		t.Fatalf("unchanged source logged %q", buf.String())
	}
}

func TestReplayMirrorWatermarkSourceStateRoundTrip(t *testing.T) {
	sdb, err := OpenStateDB(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("OpenStateDB: %v", err)
	}
	defer sdb.Close()

	state := NewAppState()
	s := replayTestStrategyState("hl-x-paper")
	s.ReplayMirrorWatermark = 12
	s.ReplayMirrorWatermarkSource = "hl-x-live"
	state.Strategies[s.ID] = s
	if err := sdb.SaveState(state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	loaded, err := sdb.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	got := loaded.Strategies["hl-x-paper"]
	if got == nil {
		t.Fatal("strategy missing after reload")
	}
	if got.ReplayMirrorWatermarkSource != "hl-x-live" || got.ReplayMirrorWatermark != 12 {
		t.Fatalf("reloaded watermark=%d source=%q, want 12 / hl-x-live", got.ReplayMirrorWatermark, got.ReplayMirrorWatermarkSource)
	}
	sc := StrategyConfig{ID: "hl-x-paper", Type: "perps", Platform: "hyperliquid",
		Args: []string{"vwap", "ETH", "1h", "--mode=paper"}, ReplaySharing: ReplaySharingLiveMirror, ReplaySourceID: "hl-x-live"}
	if reset := syncReplayMirrorWatermarkSource(sc, got, replayMirrorSourceID(sc), silentStrategyLogger(sc.ID)); reset {
		t.Fatal("a restart must not reset the watermark of an unchanged source")
	}
	if got.ReplayMirrorWatermark != 12 {
		t.Fatalf("watermark = %d after restart, want 12 — replayed rows would double-book", got.ReplayMirrorWatermark)
	}
}

func TestReplaySourceIDHotReloadFlatOnly(t *testing.T) {
	mk := func(source string) *Config {
		return &Config{Strategies: []StrategyConfig{{
			ID: "hl-x-paper", Type: "perps", Platform: "hyperliquid",
			Args: []string{"vwap", "ETH", "1h", "--mode=paper"}, ReplaySharing: ReplaySharingLiveMirror,
			ReplaySourceID: source,
		}}}
	}
	state := NewAppState()
	ss := replayTestStrategyState("hl-x-paper")
	ss.Positions["ETH"] = &Position{Symbol: "ETH", Quantity: 0.5, AvgCost: 1900, Side: "long", Multiplier: 1}
	state.Strategies["hl-x-paper"] = ss

	err := validateHotReloadStateCompatible(mk(""), mk("hl-x-live"), state)
	if err == nil || !strings.Contains(err.Error(), "replay_source_id changed with open positions") {
		t.Fatalf("source change while open not refused: %v", err)
	}
	ss.Positions = map[string]*Position{}
	if err := validateHotReloadStateCompatible(mk(""), mk("hl-x-live"), state); err != nil {
		t.Fatalf("source change while flat refused: %v", err)
	}
	if err := validateHotReloadCompatible(mk(""), mk("hl-x-live")); err != nil {
		t.Fatalf("replay_source_id change flagged restart-required: %v", err)
	}
}

func TestReplaySourceIDUnknownKeyGuardAcceptsField(t *testing.T) {
	raw := []byte(`{"strategies":[{"id":"s1","type":"perps","platform":"hyperliquid","replay_sharing":"live_mirror","replay_source_id":"s0"}]}`)
	for _, e := range validateStrategyJSONKeys(raw) {
		if strings.Contains(e, "replay_source_id") {
			t.Fatalf("replay_source_id flagged as unknown: %s", e)
		}
	}
}

func TestLoadConfigAcceptsInProcessReplayPair(t *testing.T) {
	t.Setenv("HYPERLIQUID_SECRET_KEY", "shh")
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xTEST")
	dir := t.TempDir()
	cfgJSON := `{
		"config_version": 19,
		"replay_log_path": "/var/lib/go-trader/shared/replay.db",
		"strategies": [
			{
				"id": "hl-x-live",
				"type": "perps",
				"platform": "hyperliquid",
				"script": "shared_scripts/check_hyperliquid.py",
				"args": ["sma_crossover", "ETH", "1h", "--mode=live"],
				"capital": 1000,
				"replay_sharing": "live_mirror",
				"close_strategy": {"name": "tiered_tp_atr"}
			},
			{
				"id": "hl-x-paper",
				"type": "perps",
				"platform": "hyperliquid",
				"script": "shared_scripts/check_hyperliquid.py",
				"args": ["sma_crossover", "ETH", "1h", "--mode=paper"],
				"capital": 1000,
				"replay_sharing": "live_mirror",
				"replay_source_id": "hl-x-live",
				"close_strategy": {"name": "tiered_tp_atr"}
			}
		]
	}`
	cfg, err := LoadConfig(writeTestConfig(t, dir, cfgJSON))
	if err != nil {
		t.Fatalf("LoadConfig failed for an in-process replay pair: %v", err)
	}
	if got := replayMirrorSourceID(cfg.Strategies[1]); got != "hl-x-live" {
		t.Fatalf("mirror source = %q, want hl-x-live", got)
	}
	if !replaySharingSourceEnabled(cfg.Strategies[0]) {
		t.Fatal("live twin is not registered as a replay source")
	}
	ordered := orderReplaySourcesBeforeMirrors([]StrategyConfig{cfg.Strategies[1], cfg.Strategies[0]})
	if ordered[0].ID != "hl-x-live" || ordered[1].ID != "hl-x-paper" {
		t.Fatalf("cycle order = %s,%s — the live source must evaluate first", ordered[0].ID, ordered[1].ID)
	}
}

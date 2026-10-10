package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func noEdgeHLStrategy(id string, args ...string) map[string]interface{} {
	return map[string]interface{}{
		"id":               id,
		"type":             "perps",
		"platform":         "hyperliquid",
		"script":           "shared_scripts/check_hyperliquid.py",
		"args":             args,
		"capital":          100.0,
		"max_drawdown_pct": 10.0,
		"leverage":         1.0,
		"stop_loss_pct":    3.0,
	}
}

func writeNoEdgeConfig(t *testing.T, version int, strategies ...map[string]interface{}) string {
	t.Helper()
	return writeNoEdgeConfigRoot(t, version, nil, strategies...)
}

func writeNoEdgeConfigRoot(t *testing.T, version int, extra map[string]interface{}, strategies ...map[string]interface{}) string {
	t.Helper()
	dir := t.TempDir()
	root := map[string]interface{}{
		"interval_seconds": 3600,
		"db_file":          filepath.Join(dir, "state.db"),
		"strategies":       strategies,
	}
	for k, v := range extra {
		root[k] = v
	}
	if version > 0 {
		root["config_version"] = version
	}
	data, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func setNoEdgeLiveCredentials(t *testing.T) {
	t.Helper()
	t.Setenv("HYPERLIQUID_SECRET_KEY", "0x0000000000000000000000000000000000000000000000000000000000000001")
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0x0000000000000000000000000000000000000001")
	t.Setenv("OKX_API_KEY", "k")
	t.Setenv("OKX_API_SECRET", "s")
	t.Setenv("OKX_PASSPHRASE", "p")
}

func rawStrategiesByID(t *testing.T, path string) map[string]map[string]interface{} {
	t.Helper()
	raw := readRawConfig(t, path)
	out := map[string]map[string]interface{}{}
	for _, item := range raw["strategies"].([]interface{}) {
		sc := item.(map[string]interface{})
		out[sc["id"].(string)] = sc
	}
	return out
}

func TestNoEdgeAdmissionMatrixThroughRealLoaders(t *testing.T) {
	setNoEdgeLiveCredentials(t)
	ack := func(sc map[string]interface{}, v interface{}) map[string]interface{} {
		sc["allow_no_edge"] = v
		return sc
	}
	withClose := func(sc map[string]interface{}, name string) map[string]interface{} {
		sc["close_strategy"] = map[string]interface{}{"name": name}
		return sc
	}
	withOpen := func(sc map[string]interface{}, name string) map[string]interface{} {
		sc["open_strategy"] = map[string]interface{}{"name": name}
		return sc
	}
	spot := func(platform string, args ...string) map[string]interface{} {
		return map[string]interface{}{
			"id": "spot-x", "type": "spot", "platform": platform, "script": "shared_scripts/check_strategy.py",
			"args": args, "capital": 1000.0, "max_drawdown_pct": 10.0,
		}
	}
	cases := []struct {
		name      string
		sc        map[string]interface{}
		wantErr   []string
		wantScope PortfolioScope
	}{
		{"explicit paper equals form", noEdgeHLStrategy("hl-x", "rsi", "ETH", "1h", "--mode=paper"), nil, ScopePaper},
		{"explicit paper space form", noEdgeHLStrategy("hl-x", "rsi", "ETH", "1h", "--mode", "paper"), nil, ScopePaper},
		{"explicit live refused", noEdgeHLStrategy("hl-x", "rsi", "ETH", "1h", "--mode=live"),
			[]string{"strategy hl-x", `open strategy "rsi"`, "source fee_audit_m5", "docs/research/fee-audit-m5.md", "explicit live mode", `"allow_no_edge": true`}, ""},
		{"explicit live space form refused", noEdgeHLStrategy("hl-x", "rsi", "ETH", "1h", "--mode", "live"), []string{"explicit live mode"}, ""},
		{"explicit live acknowledged", ack(noEdgeHLStrategy("hl-x", "rsi", "ETH", "1h", "--mode=live"), true), nil, ScopeLive},
		{"explicit false stays refused", ack(noEdgeHLStrategy("hl-x", "rsi", "ETH", "1h", "--mode=live"), false), []string{"explicit live mode"}, ""},
		{"missing mode refused", noEdgeHLStrategy("hl-x", "rsi", "ETH", "1h"), []string{"missing --mode"}, ""},
		{"missing mode acknowledged keeps paper scope", ack(noEdgeHLStrategy("hl-x", "rsi", "ETH", "1h"), true), nil, ScopePaper},
		{"empty mode refused", noEdgeHLStrategy("hl-x", "rsi", "ETH", "1h", "--mode="), []string{"invalid mode", `--mode value ""`}, ""},
		{"unknown mode refused", noEdgeHLStrategy("hl-x", "rsi", "ETH", "1h", "--mode=PAPER"), []string{"invalid mode", `"PAPER"`}, ""},
		{"dangling mode refused", noEdgeHLStrategy("hl-x", "rsi", "ETH", "1h", "--mode"), []string{"invalid mode", "dangling --mode"}, ""},
		{"repeated equal mode refused even acknowledged", ack(noEdgeHLStrategy("hl-x", "rsi", "ETH", "1h", "--mode=paper", "--mode=paper"), true), []string{"invalid mode", "given 2 times"}, ""},
		{"conflicting mode refused", noEdgeHLStrategy("hl-x", "rsi", "ETH", "1h", "--mode=paper", "--mode", "live"), []string{"invalid mode", "given 2 times"}, ""},
		{"close-only fallback refused", withClose(noEdgeHLStrategy("hl-x", "breakout", "ETH", "1h", "--mode=live"), "ema_crossover"),
			[]string{`close strategy "ema_crossover"`, "explicit live mode"}, ""},
		{"close-only fallback acknowledged", ack(withClose(noEdgeHLStrategy("hl-x", "breakout", "ETH", "1h", "--mode=live"), "ema_crossover"), true), nil, ScopeLive},
		{"native close not gated", withClose(noEdgeHLStrategy("hl-x", "breakout", "ETH", "1h", "--mode=live"), "tiered_tp_atr"), nil, ScopeLive},
		{"native close alias not gated", withClose(noEdgeHLStrategy("hl-x", "breakout", "ETH", "1h", "--mode=live"), "tp_at_pct"), nil, ScopeLive},
		{"explicit open replaces positional", withOpen(noEdgeHLStrategy("hl-x", "rsi", "ETH", "1h", "--mode=live"), "breakout"), nil, ScopeLive},
		{"former research name acknowledged live", ack(noEdgeHLStrategy("hl-x", "awesome_oscillator", "ETH", "4h", "--mode=live"), true), nil, ScopeLive},
		{"former research name refused live", noEdgeHLStrategy("hl-x", "awesome_oscillator", "ETH", "4h", "--mode=live"),
			[]string{`"awesome_oscillator"`, "source study_fail", "backtest/candidates/awesome_oscillator_1660/REPORT.md"}, ""},
		{"non-no-edge live unchanged", noEdgeHLStrategy("hl-x", "breakout", "ETH", "1h", "--mode=live"), nil, ScopeLive},
		{"go spot paper dispatch supplies paper", spot("binanceus", "rsi", "BTC/USDT", "1h"), nil, ScopePaper},
		{"go spot explicit live refused", spot("binanceus", "rsi", "BTC/USDT", "1h", "--mode=live"), []string{"explicit live mode"}, ""},
		{"okx spot missing mode refused", spot("okx", "rsi", "BTC/USDT", "1h"), []string{"missing --mode"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeNoEdgeConfig(t, CurrentConfigVersion, tc.sc)
			cfg, loadErr := LoadConfig(path)
			_, probeErr := LoadConfigForProbe(path)
			if (loadErr == nil) != (probeErr == nil) {
				t.Fatalf("LoadConfig err=%v but LoadConfigForProbe err=%v", loadErr, probeErr)
			}
			if len(tc.wantErr) == 0 {
				if loadErr != nil {
					t.Fatalf("unexpected refusal: %v", loadErr)
				}
				if got := portfolioScopeFor(cfg.Strategies[0]); got != tc.wantScope {
					t.Fatalf("portfolio scope = %q, want %q (edge admission must not move storage scope)", got, tc.wantScope)
				}
				return
			}
			if loadErr == nil {
				t.Fatalf("expected refusal containing %q", tc.wantErr)
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(loadErr.Error(), want) || !strings.Contains(probeErr.Error(), want) {
					t.Fatalf("errors missing %q:\nload:  %v\nprobe: %v", want, loadErr, probeErr)
				}
			}
		})
	}
}

func TestNoEdgeAcknowledgementMustBeAJSONBoolean(t *testing.T) {
	setNoEdgeLiveCredentials(t)
	for _, v := range []interface{}{"true", 1, "yes"} {
		sc := noEdgeHLStrategy("hl-x", "rsi", "ETH", "1h", "--mode=live")
		sc["allow_no_edge"] = v
		if cfg, err := LoadConfig(writeNoEdgeConfig(t, CurrentConfigVersion, sc)); err == nil {
			t.Fatalf("allow_no_edge=%#v loaded (ack=%v); a non-boolean must never grant live admission", v, cfg.Strategies[0].AllowNoEdgeAcknowledged())
		}
	}
}

func v19NoEdgeFixture() []map[string]interface{} {
	rsi := noEdgeHLStrategy("hl-rsi-btc", "rsi", "BTC", "1h", "--mode=live")
	rsi["allow_deprecated"] = true
	macd := noEdgeHLStrategy("hl-macd-sol", "macd", "SOL", "1h", "--mode=paper")
	macd["allow_deprecated"] = false
	doge := noEdgeHLStrategy("hl-bo-doge", "breakout", "DOGE", "1h", "--mode=live")
	doge["close_strategy"] = map[string]interface{}{"name": "ema_crossover"}
	link := noEdgeHLStrategy("hl-bo-link", "breakout", "LINK", "1h", "--mode=live")
	link["close_strategy"] = map[string]interface{}{"name": "tiered_tp_atr"}
	preAck := noEdgeHLStrategy("hl-pre-ack", "supertrend", "XRP", "1h", "--mode=live")
	preAck["allow_no_edge"] = true
	manual := map[string]interface{}{
		"id": "hl-manual-eth", "type": "manual", "platform": "hyperliquid", "symbol": "ETH", "timeframe": "1h",
		"open_strategy": map[string]interface{}{"name": "stoch_rsi"}, "capital": 100.0, "leverage": 1.0,
	}
	return []map[string]interface{}{
		rsi,
		noEdgeHLStrategy("hl-dbo-eth", "donchian_breakout", "ETH", "1h", "--mode", "live"),
		macd,
		doge,
		link,
		preAck,
		manual,
		{"id": "sma-btc", "type": "spot", "platform": "binanceus", "script": "shared_scripts/check_strategy.py",
			"args": []string{"sma_crossover", "BTC/USDT", "1h"}, "capital": 1000.0, "max_drawdown_pct": 10.0},
	}
}

func stampedIDs(r *noEdgeMigrationReport) []string {
	if r == nil {
		return nil
	}
	out := make([]string, 0, len(r.Stamped))
	for _, st := range r.Stamped {
		out = append(out, st.ID)
	}
	return out
}

func TestV20NoEdgeMigrationStampsOnlyWorkingLiveConfigsAndIsIdempotent(t *testing.T) {
	setNoEdgeLiveCredentials(t)
	path := writeNoEdgeConfig(t, 19, v19NoEdgeFixture()...)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("a v19 configuration that ran before the update must still load: %v", err)
	}
	if len(cfg.Strategies) != 8 {
		t.Fatalf("strategies = %d, want all 8 kept", len(cfg.Strategies))
	}
	report := cfg.NoEdgeMigrationReport()
	wantStamped := []string{"hl-bo-doge", "hl-dbo-eth", "hl-manual-eth", "hl-rsi-btc"}
	if got := stampedIDs(report); !reflect.DeepEqual(got, wantStamped) {
		t.Fatalf("stamped = %v, want %v (sorted, newly stamped only)", got, wantStamped)
	}
	if !reflect.DeepEqual(report.Stamped[0].Refs, []edgeReference{{Role: "close", Name: "ema_crossover"}}) {
		t.Fatalf("close-only reference not recorded: %+v", report.Stamped[0].Refs)
	}
	if !reflect.DeepEqual(report.RemovedKeyIDs, []string{"hl-macd-sol", "hl-rsi-btc"}) {
		t.Fatalf("removed legacy keys = %v", report.RemovedKeyIDs)
	}
	if report.InputVersion != 19 || !report.changedLegacyInput() {
		t.Fatalf("report must schedule the notice for v19 input: %+v", report)
	}
	raw := rawStrategiesByID(t, path)
	for id, sc := range raw {
		if _, legacy := sc["allow_deprecated"]; legacy {
			t.Fatalf("%s still carries allow_deprecated", id)
		}
		_, has := sc["allow_no_edge"]
		want := id == "hl-pre-ack"
		for _, stamped := range wantStamped {
			want = want || stamped == id
		}
		if has != want {
			t.Fatalf("%s allow_no_edge present=%v, want %v", id, has, want)
		}
	}
	if v := readRawConfig(t, path)["config_version"].(float64); int(v) != 20 {
		t.Fatalf("config_version = %v, want 20", v)
	}
	before, _ := os.ReadFile(path)
	again, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) || again.NoEdgeMigrationReport() != nil {
		t.Fatalf("second load changed the config or re-ran v20 (report=%+v)", again.NoEdgeMigrationReport())
	}
}

func TestV20NoEdgeMigrationNeverGrantsRefusedOrNonLiveReferences(t *testing.T) {
	setNoEdgeLiveCredentials(t)
	mixed := noEdgeHLStrategy("hl-mix", "sma_crossover", "AVAX", "1h", "--mode=live")
	mixed["close_strategy"] = map[string]interface{}{"name": "vortex_trend"}
	allowedOpenRefusedClose := noEdgeHLStrategy("hl-bo-cci", "breakout", "BTC", "1h", "--mode=live")
	allowedOpenRefusedClose["close_strategy"] = map[string]interface{}{"name": "commodity_channel_trend"}
	explicitFalse := noEdgeHLStrategy("hl-false", "rsi", "SOL", "1h", "--mode=live")
	explicitFalse["allow_no_edge"] = false
	explicitFalse["allow_deprecated"] = true
	path := writeNoEdgeConfig(t, 19,
		mixed,
		allowedOpenRefusedClose,
		noEdgeHLStrategy("hl-ao", "awesome_oscillator", "ETH", "4h", "--mode=live"),
		noEdgeHLStrategy("hl-missing", "rsi", "DOGE", "1h"),
		noEdgeHLStrategy("hl-invalid", "rsi", "LINK", "1h", "--mode=live", "--mode=live"),
		explicitFalse,
	)
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("refused references must stay refused after migration")
	}
	for _, id := range []string{"hl-mix", "hl-bo-cci", "hl-ao", "hl-missing", "hl-invalid", "hl-false"} {
		if !strings.Contains(err.Error(), "strategy "+id+":") {
			t.Fatalf("%s should be refused after migration: %v", id, err)
		}
	}
	raw := rawStrategiesByID(t, path)
	for id, sc := range raw {
		if v, has := sc["allow_no_edge"]; has && v != false {
			t.Fatalf("%s gained allow_no_edge=%v", id, v)
		}
	}
	if raw["hl-false"]["allow_no_edge"] != false {
		t.Fatal("an existing explicit false must be preserved")
	}
}

func TestV20NoEdgeMigrationOrderingAndOwnership(t *testing.T) {
	setNoEdgeLiveCredentials(t)
	live := func(id string) map[string]interface{} {
		sc := noEdgeHLStrategy(id, "rsi", "BTC", "1h", "--mode=live")
		sc["allow_deprecated"] = true
		return sc
	}

	t.Run("versionless legacy input stamps and schedules the notice", func(t *testing.T) {
		cfg, err := LoadConfig(writeNoEdgeConfig(t, 0, live("hl-a")))
		if err != nil {
			t.Fatal(err)
		}
		r := cfg.NoEdgeMigrationReport()
		if r.InputVersion != 0 || !reflect.DeepEqual(stampedIDs(r), []string{"hl-a"}) || !r.changedLegacyInput() {
			t.Fatalf("versionless report = %+v", r)
		}
		if cfg.MigrationBaseVersion() < CurrentConfigVersion {
			t.Fatalf("versionless base version = %d; the notice must be scheduled by the report, not the base version", cfg.MigrationBaseVersion())
		}
	})

	t.Run("an earlier migration trigger that runs the full ladder keeps the report", func(t *testing.T) {
		cfg, err := LoadConfig(writeNoEdgeConfig(t, 15, live("hl-b")))
		if err != nil {
			t.Fatal(err)
		}
		if got := stampedIDs(cfg.NoEdgeMigrationReport()); !reflect.DeepEqual(got, []string{"hl-b"}) {
			t.Fatalf("report lost after the v16 trigger applied v20 first: %v", got)
		}
	})

	t.Run("current v20 input gets no automatic grant", func(t *testing.T) {
		sc := noEdgeHLStrategy("hl-c", "rsi", "BTC", "1h", "--mode=live")
		path := writeNoEdgeConfig(t, 20, sc)
		before, _ := os.ReadFile(path)
		if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "strategy hl-c:") {
			t.Fatalf("v20 live no-edge without acknowledgement must be refused, got %v", err)
		}
		after, _ := os.ReadFile(path)
		if !bytes.Equal(before, after) {
			t.Fatal("current v20 input was rewritten")
		}
	})

	t.Run("a legacy key on v20 input is removed without a grant", func(t *testing.T) {
		path := writeNoEdgeConfig(t, 20, live("hl-d"))
		_, err := LoadConfig(path)
		if err == nil || !strings.Contains(err.Error(), "strategy hl-d:") {
			t.Fatalf("expected refusal, got %v", err)
		}
		sc := rawStrategiesByID(t, path)["hl-d"]
		if _, legacy := sc["allow_deprecated"]; legacy {
			t.Fatal("legacy key kept")
		}
		if _, granted := sc["allow_no_edge"]; granted {
			t.Fatal("v20 input gained an automatic grant")
		}
	})

	t.Run("read-only loaders migrate in memory only", func(t *testing.T) {
		path := writeNoEdgeConfig(t, 19, live("hl-e"))
		before, _ := os.ReadFile(path)
		for name, load := range map[string]func(string) (*Config, error){"readonly": LoadConfigReadOnly, "probe": LoadConfigForProbe} {
			cfg, err := load(path)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if !cfg.Strategies[0].AllowNoEdgeAcknowledged() || !reflect.DeepEqual(stampedIDs(cfg.NoEdgeMigrationReport()), []string{"hl-e"}) {
				t.Fatalf("%s: in-memory migration did not apply", name)
			}
		}
		after, _ := os.ReadFile(path)
		if !bytes.Equal(before, after) {
			t.Fatal("read-only loaders changed source bytes")
		}
	})

	t.Run("a write failure propagates and leaves the source unchanged", func(t *testing.T) {
		path := writeNoEdgeConfig(t, 19, live("hl-f"))
		before, _ := os.ReadFile(path)
		dir := filepath.Dir(path)
		if err := os.Chmod(dir, 0500); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(dir, 0700)
		if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "v20 no-edge acknowledgement migration") {
			t.Fatalf("expected the v20 write failure, got %v", err)
		}
		after, _ := os.ReadFile(path)
		if !bytes.Equal(before, after) {
			t.Fatal("source changed after a failed write")
		}
	})
}

func TestNoEdgeMigrationNoticeReachesOwnerOrLog(t *testing.T) {
	setNoEdgeLiveCredentials(t)
	preAck := noEdgeHLStrategy("hl-pre", "macd", "SOL", "1h", "--mode=live")
	preAck["allow_no_edge"] = true
	b := noEdgeHLStrategy("hl-b", "rsi", "ETH", "1h", "--mode=live")
	a := noEdgeHLStrategy("hl-a", "breakout", "BTC", "1h", "--mode=live")
	a["close_strategy"] = map[string]interface{}{"name": "macd"}
	path := writeNoEdgeConfig(t, 19, b, preAck, a)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	mock := &mockNotifier{}
	runConfigMigrationDM(cfg, NewMultiNotifier(notifierBackend{notifier: mock, ownerID: "owner"}), path)
	var notice string
	for _, dm := range mock.dms {
		if strings.Contains(dm.content, "edge_status: no_edge") {
			notice = dm.content
		}
	}
	if notice == "" {
		t.Fatalf("owner received no v20 notice: %+v", mock.dms)
	}
	ia, ib := strings.Index(notice, "hl-a: close=macd"), strings.Index(notice, "hl-b: open=rsi")
	if ia < 0 || ib < 0 || ia > ib || strings.Contains(notice, "hl-pre") {
		t.Fatalf("notice must list only newly stamped ids in sorted order with references:\n%s", notice)
	}
	if !strings.Contains(notice, "no successor") {
		t.Fatalf("notice must state that the paper-warning option has no successor:\n%s", notice)
	}

	orig := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	deliverNoEdgeMigrationNotice(cfg.NoEdgeMigrationReport(), nil)
	w.Close()
	os.Stdout = orig
	logged, _ := io.ReadAll(r)
	for _, line := range strings.Split(notice, "\n") {
		if !strings.Contains(string(logged), "[migration] "+line) {
			t.Fatalf("log fallback missing %q:\n%s", line, logged)
		}
	}
}

func loadedNoEdgeConfig(t *testing.T, strategies ...map[string]interface{}) *Config {
	t.Helper()
	cfg, err := LoadConfig(writeNoEdgeConfigRoot(t, CurrentConfigVersion, map[string]interface{}{"db_file": "noedge-state.db"}, strategies...))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestNoEdgeReloadRefusesLiveAcknowledgementChanges(t *testing.T) {
	setNoEdgeLiveCredentials(t)
	liveAcked := noEdgeHLStrategy("hl-live", "rsi", "BTC", "1h", "--mode=live")
	liveAcked["allow_no_edge"] = true
	paper := noEdgeHLStrategy("hl-paper", "rsi", "ETH", "1h", "--mode=paper")
	liveOther := noEdgeHLStrategy("hl-bo", "breakout", "SOL", "1h", "--mode=live")

	t.Run("removal is refused before loader admission and leaves active config", func(t *testing.T) {
		active := loadedNoEdgeConfig(t, liveAcked, paper)
		removed := noEdgeHLStrategy("hl-live", "rsi", "BTC", "1h", "--mode=live")
		path := writeNoEdgeConfig(t, CurrentConfigVersion, removed, paper)
		data, _ := os.ReadFile(path)
		if _, err := LoadConfig(path); err == nil {
			t.Fatal("removal must also fail ordinary admission")
		}
		err := reloadNoEdgeAcknowledgementPreflight(active, data)
		if err == nil || !strings.Contains(err.Error(), "strategy[hl-live].allow_no_edge removed on a live strategy (restart required") {
			t.Fatalf("expected restart-required refusal, got %v", err)
		}
		if !active.Strategies[0].AllowNoEdgeAcknowledged() {
			t.Fatal("active config changed")
		}
	})

	t.Run("addition is refused by the compatibility check", func(t *testing.T) {
		active := loadedNoEdgeConfig(t, liveOther)
		addedSC := noEdgeHLStrategy("hl-bo", "breakout", "SOL", "1h", "--mode=live")
		addedSC["allow_no_edge"] = true
		next := loadedNoEdgeConfig(t, addedSC)
		state := &AppState{Strategies: map[string]*StrategyState{}}
		_, err := applyHotReloadConfig(active, next, state, nil, nil)
		if err == nil || !strings.Contains(err.Error(), "strategy[hl-bo].allow_no_edge added on a live strategy (restart required") {
			t.Fatalf("expected restart-required refusal, got %v", err)
		}
		if active.Strategies[0].AllowNoEdge != nil {
			t.Fatal("active config mutated by a rejected reload")
		}
		data, _ := json.Marshal(map[string]interface{}{"strategies": []interface{}{addedSC}})
		if err := reloadNoEdgeAcknowledgementPreflight(active, data); err == nil {
			t.Fatal("preflight must refuse the addition too")
		}
	})

	t.Run("paper acknowledgement changes reload into the active config", func(t *testing.T) {
		active := loadedNoEdgeConfig(t, paper)
		ackedPaper := noEdgeHLStrategy("hl-paper", "rsi", "ETH", "1h", "--mode=paper")
		ackedPaper["allow_no_edge"] = true
		next := loadedNoEdgeConfig(t, ackedPaper)
		data, _ := json.Marshal(map[string]interface{}{"strategies": []interface{}{ackedPaper}})
		if err := reloadNoEdgeAcknowledgementPreflight(active, data); err != nil {
			t.Fatal(err)
		}
		state := &AppState{Strategies: map[string]*StrategyState{}}
		changes, err := applyHotReloadConfig(active, next, state, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !active.Strategies[0].AllowNoEdgeAcknowledged() || !reflect.DeepEqual(changes, []string{"strategy[hl-paper].allow_no_edge: unset -> true"}) {
			t.Fatalf("paper change not applied: %v", changes)
		}
	})

	t.Run("unchanged reloads do not repeat the live warning", func(t *testing.T) {
		active := loadedNoEdgeConfig(t, liveAcked, paper)
		if got := noEdgeStartupWarnings(active.Strategies); len(got) != 1 || !strings.Contains(got[0], "hl-live") || !strings.Contains(got[0], "docs/research/fee-audit-m5.md") {
			t.Fatalf("startup warnings = %v, want one live warning with evidence", got)
		}
		if got := newlyIntroducedNoEdgeWarnings(active.Strategies, loadedNoEdgeConfig(t, liveAcked, paper).Strategies); len(got) != 0 {
			t.Fatalf("unchanged reload warned again: %v", got)
		}
		moved := noEdgeHLStrategy("hl-live", "breakout", "BTC", "1h", "--mode=live")
		moved["allow_no_edge"] = true
		moved["close_strategy"] = map[string]interface{}{"name": "macd"}
		if got := newlyIntroducedNoEdgeWarnings(active.Strategies, loadedNoEdgeConfig(t, moved, paper).Strategies); len(got) != 1 || !strings.Contains(got[0], "close strategy macd") {
			t.Fatalf("a new warning identity must warn once: %v", got)
		}
	})
}

func TestNoEdgeSummaryAndInspectSurfaces(t *testing.T) {
	setNoEdgeLiveCredentials(t)
	liveAcked := noEdgeHLStrategy("hl-live", "rsi", "BTC", "1h", "--mode=live")
	liveAcked["allow_no_edge"] = true
	closeOnly := noEdgeHLStrategy("hl-close", "breakout", "SOL", "1h", "--mode=live")
	closeOnly["allow_no_edge"] = true
	closeOnly["close_strategy"] = map[string]interface{}{"name": "vortex_trend"}
	cfg := loadedNoEdgeConfig(t, liveAcked, closeOnly, noEdgeHLStrategy("hl-paper", "awesome_oscillator", "ETH", "4h", "--mode=paper"))
	want := map[string]string{
		"hl-live":  "edge=no_edge:fee_audit_m5(ack)",
		"hl-close": "edge=no_edge:study_fail(ack)[close=vortex_trend]",
		"hl-paper": "edge=no_edge:study_fail(paper)",
	}
	for _, sc := range cfg.Strategies {
		if !strings.HasSuffix(formatStrategySummaryLine(sc, nil, cfg), " "+want[sc.ID]) && !strings.Contains(formatStrategySummaryLine(sc, nil, cfg), want[sc.ID]+" ") {
			t.Fatalf("%s summary %q lacks %q", sc.ID, formatStrategySummaryLine(sc, nil, cfg), want[sc.ID])
		}
		edge, ok := buildStrategyInspectionJSON(sc, nil, cfg, nil)["edge"].(map[string]interface{})
		if !ok || edge["tag"] != want[sc.ID] {
			t.Fatalf("%s inspect edge = %#v", sc.ID, edge)
		}
	}
	warnings := noEdgeStartupWarnings(cfg.Strategies)
	if len(warnings) != 2 || !strings.Contains(strings.Join(warnings, "\n"), "close strategy vortex_trend") {
		t.Fatalf("live warnings must cover the close-only reference and skip paper: %v", warnings)
	}
}

func TestNoEdgePaperAddAcceptsEveryRegisteredPlatformName(t *testing.T) {
	setNoEdgeLiveCredentials(t)
	seen := map[string]string{}
	names := make([]string, 0, len(registeredOpenStrategyPlatforms))
	for name := range registeredOpenStrategyPlatforms {
		names = append(names, name)
	}
	sort.Strings(names)
	var hl []map[string]interface{}
	for _, name := range names {
		id, entry, err := buildAddStrategyEntry(name, "hyperliquid", "BTC")
		if !openStrategyRegisteredFor(name, "futures") {
			if err == nil {
				t.Fatalf("%s is not registered for futures but was added to hyperliquid", name)
			}
			continue
		}
		if err != nil {
			t.Fatalf("hyperliquid add refused registered futures strategy %s: %v", name, err)
		}
		if prev, dup := seen[id]; dup {
			t.Fatalf("%s and %s share id %s", prev, name, id)
		}
		seen[id] = name
		var sc map[string]interface{}
		if err := json.Unmarshal(entry, &sc); err != nil {
			t.Fatal(err)
		}
		if _, ack := sc["allow_no_edge"]; ack {
			t.Fatalf("%s: paper add must not set an acknowledgement", name)
		}
		hl = append(hl, sc)
	}
	cfg, err := LoadConfig(writeNoEdgeConfigRoot(t, CurrentConfigVersion, map[string]interface{}{"market_feed": "websocket"}, hl...))
	if err != nil {
		t.Fatalf("added paper strategies must load without acknowledgement: %v", err)
	}
	for _, sc := range cfg.Strategies {
		if portfolioScopeFor(sc) != ScopePaper || edgeGateModeForStrategy(sc).Kind != edgeGateModePaper {
			t.Fatalf("%s is not explicit paper", sc.ID)
		}
	}
	if _, _, err := buildAddStrategyEntry("pairs_spread", "binanceus", "BTC"); err != nil {
		t.Fatalf("pairs_spread is spot-registered: %v", err)
	}
	for _, name := range []string{"awesome_oscillator", "funding_skew"} {
		if _, _, err := buildAddStrategyEntry(name, "binanceus", "BTC"); err == nil {
			t.Fatalf("%s is futures-only but binanceus add accepted it", name)
		}
	}
}

func TestNoEdgeBatchSlotCarriesRawModeEvidenceAndAcknowledgement(t *testing.T) {
	sc := StrategyConfig{ID: "hl-x", Type: "perps", Platform: "hyperliquid", Script: hyperliquidCheckScript,
		Args: []string{"vortex_trend", "BTC", "4h", "--mode", "paper"}}
	if !hyperliquidBatchEligible(sc) {
		t.Fatal("valid explicit-mode strategy lost batch eligibility")
	}
	slot, err := buildHyperliquidBatchSlot(sc, PositionCtx{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(slot.ModeArgs, []string{"--mode", "paper"}) || slot.AllowNoEdge {
		t.Fatalf("slot evidence = %+v", slot)
	}
	missing := sc
	missing.Args = []string{"vortex_trend", "BTC", "4h"}
	slot, _ = buildHyperliquidBatchSlot(missing, PositionCtx{}, nil)
	blob, _ := json.Marshal(slot)
	var wire map[string]interface{}
	_ = json.Unmarshal(blob, &wire)
	if _, has := wire["mode_args"]; has || wire["mode"] != "paper" {
		t.Fatalf("missing mode must reach Python as absent evidence beside the normalized mode: %s", blob)
	}
	acked := sc
	ack := true
	acked.AllowNoEdge = &ack
	fpPlain, _ := hyperliquidBatchSlotFingerprint(sc, PositionCtx{}, nil)
	fpAcked, _ := hyperliquidBatchSlotFingerprint(acked, PositionCtx{}, nil)
	if fpPlain == fpAcked {
		t.Fatal("acknowledgement must be part of the slot fingerprint")
	}
}

func TestMoneyFlowIndexReversalAdmissionAndPaperAdd(t *testing.T) {
	setNoEdgeLiveCredentials(t)
	const name = "money_flow_index_reversal"
	const ref = "backtest/candidates/money_flow_index_reversal_1658/REPORT.md"
	mfi := func(args ...string) map[string]interface{} {
		return noEdgeHLStrategy("hl-mfi-eth", append([]string{name, "ETH", "4h"}, args...)...)
	}
	ack := func(sc map[string]interface{}) map[string]interface{} {
		sc["allow_no_edge"] = true
		return sc
	}
	openRef := func(sc map[string]interface{}) map[string]interface{} {
		sc["open_strategy"] = map[string]interface{}{"name": name}
		return sc
	}
	closeRef := func(sc map[string]interface{}) map[string]interface{} {
		sc["close_strategy"] = map[string]interface{}{"name": name}
		return sc
	}
	withArgs := func(sc map[string]interface{}, args ...string) map[string]interface{} {
		sc["args"] = append([]string{"breakout", "ETH", "4h"}, args...)
		return sc
	}
	cases := []struct {
		name    string
		sc      map[string]interface{}
		wantErr []string
		scope   PortfolioScope
	}{
		{"explicit paper", mfi("--mode=paper"), nil, ScopePaper},
		{"explicit paper space form", mfi("--mode", "paper"), nil, ScopePaper},
		{"live refused", mfi("--mode=live"), []string{`open strategy "` + name + `"`, "source study_fail", ref, "explicit live mode"}, ""},
		{"missing mode refused", mfi(), []string{"missing --mode"}, ""},
		{"live acknowledged", ack(mfi("--mode=live")), nil, ScopeLive},
		{"missing mode acknowledged stays paper", ack(mfi()), nil, ScopePaper},
		{"repeated mode refused", ack(mfi("--mode=paper", "--mode=paper")), []string{"invalid mode", "given 2 times"}, ""},
		{"unknown mode refused", mfi("--mode=Paper"), []string{"invalid mode"}, ""},
		{"effective open live refused", withArgs(openRef(mfi()), "--mode=live"), []string{`open strategy "` + name + `"`, "explicit live mode"}, ""},
		{"effective open missing mode refused", withArgs(openRef(mfi())), []string{"missing --mode"}, ""},
		{"effective open paper", withArgs(openRef(mfi()), "--mode=paper"), nil, ScopePaper},
		{"close fallback live refused", withArgs(closeRef(mfi()), "--mode=live"), []string{`close strategy "` + name + `"`, "explicit live mode"}, ""},
		{"close fallback missing mode refused", withArgs(closeRef(mfi())), []string{"missing --mode"}, ""},
		{"close fallback acknowledged", ack(withArgs(closeRef(mfi()), "--mode=live")), nil, ScopeLive},
		{"native close keeps live admission", func() map[string]interface{} {
			sc := withArgs(mfi(), "--mode=live")
			sc["close_strategy"] = map[string]interface{}{"name": "tiered_tp_atr"}
			return sc
		}(), nil, ScopeLive},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeNoEdgeConfig(t, CurrentConfigVersion, tc.sc)
			cfg, loadErr := LoadConfig(path)
			_, probeErr := LoadConfigForProbe(path)
			if (loadErr == nil) != (probeErr == nil) {
				t.Fatalf("LoadConfig err=%v but LoadConfigForProbe err=%v", loadErr, probeErr)
			}
			if len(tc.wantErr) == 0 {
				if loadErr != nil {
					t.Fatalf("unexpected refusal: %v", loadErr)
				}
				if got := portfolioScopeFor(cfg.Strategies[0]); got != tc.scope {
					t.Fatalf("portfolio scope = %q, want %q", got, tc.scope)
				}
				return
			}
			if loadErr == nil {
				t.Fatalf("expected refusal containing %q", tc.wantErr)
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(loadErr.Error(), want) || !strings.Contains(probeErr.Error(), want) {
					t.Fatalf("errors missing %q:\nload:  %v\nprobe: %v", want, loadErr, probeErr)
				}
			}
		})
	}

	id, entry, err := buildAddStrategyEntry(name, "hyperliquid", "sol")
	if err != nil {
		t.Fatalf("paper add refused: %v", err)
	}
	if id != "hl-mfi-sol" {
		t.Fatalf("add id = %q, want hl-mfi-sol", id)
	}
	var sc map[string]interface{}
	if err := json.Unmarshal(entry, &sc); err != nil {
		t.Fatal(err)
	}
	if sc["direction"] != DirectionBoth {
		t.Fatalf("direction = %v, want both", sc["direction"])
	}
	if got := sc["args"].([]interface{}); len(got) != 4 || got[0] != name || got[3] != "--mode=paper" {
		t.Fatalf("args = %v, want explicit paper", got)
	}
	if _, has := sc["allow_no_edge"]; has {
		t.Fatal("paper add must not write an acknowledgement")
	}
	if _, _, err := buildAddStrategyEntry(name, "binanceus", "BTC"); err == nil {
		t.Fatal("futures-only candidate must not be addable to binanceus")
	}
	root := map[string]json.RawMessage{"strategies": json.RawMessage(`[]`)}
	addedID, err := addStrategyToRoot(root, name, "hyperliquid", "SOL")
	if err != nil || addedID != id {
		t.Fatalf("shared add path = %q, %v", addedID, err)
	}
	if _, err := addStrategyToRoot(root, name, "hyperliquid", "SOL"); err == nil {
		t.Fatal("duplicate add must be refused")
	}
	list, err := configStrategies(root)
	if err != nil || len(list) != 1 {
		t.Fatalf("strategies after add = %v, %v", list, err)
	}
	var added map[string]interface{}
	if err := json.Unmarshal(list[0], &added); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(writeNoEdgeConfigRoot(t, CurrentConfigVersion, map[string]interface{}{"market_feed": "websocket"}, added))
	if err != nil {
		t.Fatalf("added paper entry must load without acknowledgement: %v", err)
	}
	got := cfg.Strategies[0]
	if portfolioScopeFor(got) != ScopePaper || edgeGateModeForStrategy(got).Kind != edgeGateModePaper || got.AllowNoEdgeAcknowledged() {
		t.Fatalf("added entry is not explicit unacknowledged paper: %+v", got)
	}
	if got.Direction != DirectionBoth {
		t.Fatalf("loaded direction = %q, want both", got.Direction)
	}
}

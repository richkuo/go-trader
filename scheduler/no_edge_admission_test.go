package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
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
		{"research candidate explicit paper", noEdgeHLStrategy("hl-x", "ultimate_oscillator", "SOL", "1h", "--mode=paper"), nil, ScopePaper},
		{"research candidate refused live", noEdgeHLStrategy("hl-x", "ultimate_oscillator", "SOL", "1h", "--mode=live"),
			[]string{`open strategy "ultimate_oscillator"`, "source study_inconclusive", "backtest/candidates/ultimate_oscillator_1664/REPORT.md", "explicit live mode"}, ""},
		{"research candidate missing mode refused", noEdgeHLStrategy("hl-x", "ultimate_oscillator", "SOL", "1h"), []string{"missing --mode"}, ""},
		{"research candidate invalid mode refused", noEdgeHLStrategy("hl-x", "ultimate_oscillator", "SOL", "1h", "--mode=paper", "--mode=live"), []string{"invalid mode", "given 2 times"}, ""},
		{"research candidate close fallback refused", withClose(noEdgeHLStrategy("hl-x", "breakout", "SOL", "1h", "--mode=live"), "ultimate_oscillator"),
			[]string{`close strategy "ultimate_oscillator"`, "explicit live mode"}, ""},
		{"research candidate acknowledged live", ack(noEdgeHLStrategy("hl-x", "ultimate_oscillator", "SOL", "1h", "--mode=live"), true), nil, ScopeLive},
		{"non-no-edge live unchanged", noEdgeHLStrategy("hl-x", "breakout", "ETH", "1h", "--mode=live"), nil, ScopeLive},
		{"chande paper equals form", noEdgeHLStrategy("hl-x", "chande_momentum", "ETH", "1h", "--mode=paper"), nil, ScopePaper},
		{"chande paper space form", noEdgeHLStrategy("hl-x", "chande_momentum", "ETH", "1h", "--mode", "paper"), nil, ScopePaper},
		{"chande live refused", noEdgeHLStrategy("hl-x", "chande_momentum", "ETH", "1h", "--mode=live"),
			[]string{`open strategy "chande_momentum"`, "source study_fail", "backtest/candidates/chande_momentum_1661/REPORT.md", "explicit live mode", `"allow_no_edge": true`}, ""},
		{"chande live acknowledged", ack(noEdgeHLStrategy("hl-x", "chande_momentum", "ETH", "1h", "--mode=live"), true), nil, ScopeLive},
		{"chande explicit false refused", ack(noEdgeHLStrategy("hl-x", "chande_momentum", "ETH", "1h", "--mode=live"), false), []string{"explicit live mode"}, ""},
		{"chande missing mode refused", noEdgeHLStrategy("hl-x", "chande_momentum", "ETH", "1h"), []string{`open strategy "chande_momentum"`, "missing --mode"}, ""},
		{"chande missing mode acknowledged keeps paper scope", ack(noEdgeHLStrategy("hl-x", "chande_momentum", "ETH", "1h"), true), nil, ScopePaper},
		{"chande repeated mode refused even acknowledged", ack(noEdgeHLStrategy("hl-x", "chande_momentum", "ETH", "1h", "--mode=paper", "--mode=paper"), true), []string{"invalid mode", "given 2 times"}, ""},
		{"chande unknown mode refused", noEdgeHLStrategy("hl-x", "chande_momentum", "ETH", "1h", "--mode=Paper"), []string{"invalid mode", `"Paper"`}, ""},
		{"chande close fallback paper", withClose(noEdgeHLStrategy("hl-x", "breakout", "ETH", "1h", "--mode=paper"), "chande_momentum"), nil, ScopePaper},
		{"chande close fallback live refused", withClose(noEdgeHLStrategy("hl-x", "breakout", "ETH", "1h", "--mode=live"), "chande_momentum"),
			[]string{`close strategy "chande_momentum"`, "source study_fail", "explicit live mode"}, ""},
		{"chande close fallback missing mode refused", withClose(noEdgeHLStrategy("hl-x", "breakout", "ETH", "1h"), "chande_momentum"),
			[]string{`close strategy "chande_momentum"`, "missing --mode"}, ""},
		{"chande close fallback acknowledged", ack(withClose(noEdgeHLStrategy("hl-x", "breakout", "ETH", "1h", "--mode=live"), "chande_momentum"), true), nil, ScopeLive},
		{"chande open with native close live refused", withClose(noEdgeHLStrategy("hl-x", "chande_momentum", "ETH", "1h", "--mode=live"), "tiered_tp_pct"),
			[]string{`open strategy "chande_momentum"`, "explicit live mode"}, ""},
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
	for _, name := range []string{"rsi", "chande_momentum"} {
		for _, v := range []interface{}{"true", 1, "yes"} {
			sc := noEdgeHLStrategy("hl-x", name, "ETH", "1h", "--mode=live")
			sc["allow_no_edge"] = v
			if cfg, err := LoadConfig(writeNoEdgeConfig(t, CurrentConfigVersion, sc)); err == nil {
				t.Fatalf("%s allow_no_edge=%#v loaded (ack=%v); a non-boolean must never grant live admission", name, v, cfg.Strategies[0].AllowNoEdgeAcknowledged())
			}
		}
	}
}

func chandeLegacyFixture() []map[string]interface{} {
	openLive := noEdgeHLStrategy("hl-cmo-open", "chande_momentum", "BTC", "1h", "--mode=live")
	openLive["allow_deprecated"] = true
	closeOnly := noEdgeHLStrategy("hl-cmo-close", "breakout", "ETH", "1h", "--mode", "live")
	closeOnly["close_strategy"] = map[string]interface{}{"name": "chande_momentum"}
	mixed := noEdgeHLStrategy("hl-cmo-mixed", "rsi", "SOL", "1h", "--mode=live")
	mixed["close_strategy"] = map[string]interface{}{"name": "chande_momentum"}
	explicitFalse := noEdgeHLStrategy("hl-cmo-false", "chande_momentum", "AVAX", "1h", "--mode=live")
	explicitFalse["allow_no_edge"] = false
	preAck := noEdgeHLStrategy("hl-cmo-ack", "chande_momentum", "LINK", "1h", "--mode=live")
	preAck["allow_no_edge"] = true
	manual := map[string]interface{}{
		"id": "hl-cmo-manual", "type": "manual", "platform": "hyperliquid", "symbol": "DOGE", "timeframe": "1h",
		"open_strategy": map[string]interface{}{"name": "chande_momentum"}, "capital": 100.0, "leverage": 1.0,
	}
	return []map[string]interface{}{
		openLive,
		closeOnly,
		mixed,
		explicitFalse,
		preAck,
		manual,
		noEdgeHLStrategy("hl-cmo-paper", "chande_momentum", "XRP", "1h", "--mode=paper"),
		noEdgeHLStrategy("hl-control", "macd", "BTC", "4h", "--mode=live"),
	}
}

func TestV20NoEdgeMigrationNeverGrantsChandeMomentumAndReportVetoesIt(t *testing.T) {
	setNoEdgeLiveCredentials(t)
	for _, version := range []int{0, 19} {
		refusedIDs := []string{"hl-cmo-close", "hl-cmo-false", "hl-cmo-mixed", "hl-cmo-open"}
		if version == 19 {
			refusedIDs = append(refusedIDs, "hl-cmo-manual")
		}
		t.Run(map[int]string{0: "versionless", 19: "v19"}[version], func(t *testing.T) {
			path := writeNoEdgeConfig(t, version, chandeLegacyFixture()...)
			original, _ := os.ReadFile(path)

			for name, load := range map[string]func(string) (*Config, error){"readonly": LoadConfigReadOnly, "probe": LoadConfigForProbe} {
				cfg, err := load(path)
				if err == nil {
					for _, sc := range cfg.Strategies {
						for _, id := range refusedIDs {
							if sc.ID == id && sc.AllowNoEdgeAcknowledged() {
								t.Fatalf("%s: %s gained an in-memory acknowledgement", name, id)
							}
						}
					}
				} else {
					for _, id := range refusedIDs {
						if !strings.Contains(err.Error(), "strategy "+id+":") {
							t.Fatalf("%s: %s should be refused: %v", name, id, err)
						}
					}
				}
				if after, _ := os.ReadFile(path); !bytes.Equal(original, after) {
					t.Fatalf("%s loader changed source bytes", name)
				}
			}

			_, err := LoadConfig(path)
			if err == nil {
				t.Fatal("unacknowledged live chande_momentum references must stay refused after migration")
			}
			for _, id := range refusedIDs {
				if !strings.Contains(err.Error(), "strategy "+id+":") {
					t.Fatalf("%s should be refused after migration: %v", id, err)
				}
			}
			for _, id := range []string{"hl-cmo-ack", "hl-cmo-paper", "hl-control"} {
				if strings.Contains(err.Error(), "strategy "+id+":") {
					t.Fatalf("%s should be admitted: %v", id, err)
				}
			}
			raw := rawStrategiesByID(t, path)
			for _, id := range []string{"hl-cmo-open", "hl-cmo-close", "hl-cmo-mixed", "hl-cmo-manual", "hl-cmo-paper"} {
				if v, has := raw[id]["allow_no_edge"]; has {
					t.Fatalf("%s gained allow_no_edge=%v from the legacy migration", id, v)
				}
			}
			if raw["hl-cmo-false"]["allow_no_edge"] != false {
				t.Fatal("an existing explicit false must be preserved")
			}
			if raw["hl-cmo-ack"]["allow_no_edge"] != true {
				t.Fatal("an existing acknowledgement must be preserved")
			}
			if raw["hl-control"]["allow_no_edge"] != true {
				t.Fatal("the legacy migration did not run: the non-candidate control was not stamped")
			}
			if _, legacy := raw["hl-cmo-open"]["allow_deprecated"]; legacy {
				t.Fatal("legacy allow_deprecated kept")
			}
			if v := readRawConfig(t, path)["config_version"].(float64); int(v) < 20 {
				t.Fatalf("config_version = %v, want at least 20", v)
			}

			migrated, _ := os.ReadFile(path)
			_, again := LoadConfig(path)
			if again == nil || again.Error() != err.Error() {
				t.Fatalf("second load changed the refusal:\nfirst:  %v\nsecond: %v", err, again)
			}
			if after, _ := os.ReadFile(path); !bytes.Equal(migrated, after) {
				t.Fatal("second load changed the migrated config")
			}
		})
	}

	raw := map[string]interface{}{"strategies": []interface{}{}}
	for _, sc := range chandeLegacyFixture() {
		blob, _ := json.Marshal(sc)
		var generic map[string]interface{}
		_ = json.Unmarshal(blob, &generic)
		raw["strategies"] = append(raw["strategies"].([]interface{}), generic)
	}
	report, err := migrateV20NoEdge(raw, 19)
	if err != nil {
		t.Fatalf("report vetoes: %v", err)
	}
	if got := stampedIDs(report); !reflect.DeepEqual(got, []string{"hl-control"}) {
		t.Fatalf("report vetoes: stamped = %v, want only the non-candidate control", got)
	}
	if want := []string{"hl-cmo-close", "hl-cmo-manual", "hl-cmo-mixed", "hl-cmo-open"}; !reflect.DeepEqual(report.VetoedResearch, want) {
		t.Fatalf("report vetoes: vetoed = %v, want %v", report.VetoedResearch, want)
	}
	again, err := migrateV20NoEdge(raw, 19)
	if err != nil {
		t.Fatalf("report vetoes: second pass: %v", err)
	}
	if len(again.Stamped) != 0 || len(again.VetoedResearch) != 4 {
		t.Fatalf("report vetoes: second in-memory pass = %+v; it must grant nothing new", again)
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
	wrrClose := noEdgeHLStrategy("hl-wrr-close", "breakout", "ETH", "1h", "--mode=live")
	wrrClose["close_strategy"] = map[string]interface{}{"name": "williams_r_reversal"}
	mfiClose := noEdgeHLStrategy("hl-bo-mfi", "breakout", "SOL", "4h", "--mode=live")
	mfiClose["close_strategy"] = map[string]interface{}{"name": "money_flow_index_reversal"}
	explicitFalse := noEdgeHLStrategy("hl-false", "rsi", "SOL", "1h", "--mode=live")
	explicitFalse["allow_no_edge"] = false
	explicitFalse["allow_deprecated"] = true
	allowedOpenUOClose := noEdgeHLStrategy("hl-bo-uo", "breakout", "SOL", "1h", "--mode=live")
	allowedOpenUOClose["close_strategy"] = map[string]interface{}{"name": "ultimate_oscillator"}
	uoOpen := noEdgeHLStrategy("hl-uo", "ultimate_oscillator", "SOL", "1h", "--mode=live")
	eomOpen := noEdgeHLStrategy("hl-eombo", "ease_of_movement_breakout", "ARB", "1h", "--mode=live")
	eomOpen["allow_deprecated"] = true
	eomClose := noEdgeHLStrategy("hl-bo-eom", "breakout", "OP", "1h", "--mode=live")
	eomClose["close_strategy"] = map[string]interface{}{"name": "ease_of_movement_breakout"}
	obvOpen := noEdgeHLStrategy("hl-obv", "on_balance_volume_divergence", "ARB", "4h", "--mode=live")
	obvOpen["allow_deprecated"] = true
	obvClose := noEdgeHLStrategy("hl-bo-obv", "breakout", "OP", "4h", "--mode=live")
	obvClose["close_strategy"] = map[string]interface{}{"name": "on_balance_volume_divergence"}
	fisherCloseFallback := noEdgeHLStrategy("hl-bo-fisher", "breakout", "BTC", "4h", "--mode=live")
	fisherCloseFallback["close_strategy"] = map[string]interface{}{"name": "fisher_reversal"}
	aroonClose := noEdgeHLStrategy("hl-bo-aroon", "breakout", "ETH", "4h", "--mode=live")
	aroonClose["close_strategy"] = map[string]interface{}{"name": "aroon_trend"}
	path := writeNoEdgeConfig(t, 19,
		mixed,
		allowedOpenRefusedClose,
		eomOpen,
		eomClose,
		obvOpen,
		obvClose,
		noEdgeHLStrategy("hl-ao", "awesome_oscillator", "ETH", "4h", "--mode=live"),
		noEdgeHLStrategy("hl-wrr", "williams_r_reversal", "BTC", "4h", "--mode=live"),
		wrrClose,
		noEdgeHLStrategy("hl-mfi", "money_flow_index_reversal", "ETH", "4h", "--mode=live"),
		mfiClose,
		uoOpen,
		allowedOpenUOClose,
		noEdgeHLStrategy("hl-fisher", "fisher_reversal", "ETH", "4h", "--mode=live"),
		fisherCloseFallback,
		noEdgeHLStrategy("hl-aroon", "aroon_trend", "BTC", "4h", "--mode=live"),
		aroonClose,
		noEdgeHLStrategy("hl-missing", "rsi", "DOGE", "1h"),
		noEdgeHLStrategy("hl-invalid", "rsi", "LINK", "1h", "--mode=live", "--mode=live"),
		explicitFalse,
	)
	report, err := migrateV20NoEdge(readRawConfig(t, path), 19)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"hl-ao", "hl-aroon", "hl-bo-aroon", "hl-bo-cci", "hl-bo-eom", "hl-bo-fisher", "hl-bo-mfi", "hl-bo-obv", "hl-bo-uo", "hl-eombo", "hl-fisher", "hl-mfi", "hl-mix", "hl-obv", "hl-uo", "hl-wrr", "hl-wrr-close"}; !reflect.DeepEqual(report.VetoedResearch, want) {
		t.Fatalf("vetoed research = %v, want %v", report.VetoedResearch, want)
	}
	if len(report.Stamped) != 0 {
		t.Fatalf("stamped = %v, want none", stampedIDs(report))
	}
	_, err = LoadConfig(path)
	if err == nil {
		t.Fatal("refused references must stay refused after migration")
	}
	for _, id := range []string{"hl-mix", "hl-bo-cci", "hl-eombo", "hl-bo-eom", "hl-obv", "hl-bo-obv", "hl-ao", "hl-wrr", "hl-wrr-close", "hl-mfi", "hl-bo-mfi", "hl-fisher", "hl-bo-fisher", "hl-uo", "hl-bo-uo", "hl-aroon", "hl-bo-aroon", "hl-missing", "hl-invalid", "hl-false"} {
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
	for _, id := range []string{"hl-aroon", "hl-bo-aroon"} {
		if _, has := raw[id]["allow_no_edge"]; has {
			t.Fatalf("%s names aroon_trend and must be vetoed, never stamped", id)
		}
	}
	report, err = migrateV20NoEdge(readRawConfig(t, writeNoEdgeConfig(t, 19,
		noEdgeHLStrategy("hl-aroon", "aroon_trend", "BTC", "4h", "--mode=live"), aroonClose)), 19)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Stamped) != 0 || !reflect.DeepEqual(report.VetoedResearch, []string{"hl-aroon", "hl-bo-aroon"}) {
		t.Fatalf("aroon_trend references must be vetoed: stamped=%v vetoed=%v", stampedIDs(report), report.VetoedResearch)
	}
	legacy := map[string]interface{}{"strategies": []interface{}{
		noEdgeHLStrategy("hl-uo", "ultimate_oscillator", "SOL", "1h", "--mode=live"),
		func() map[string]interface{} {
			sc := noEdgeHLStrategy("hl-bo-uo", "breakout", "SOL", "1h", "--mode=live")
			sc["close_strategy"] = map[string]interface{}{"name": "ultimate_oscillator"}
			return sc
		}(),
	}}
	report, err = migrateV20NoEdge(legacy, 19)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(report.VetoedResearch, []string{"hl-bo-uo", "hl-uo"}) || len(report.Stamped) != 0 {
		t.Fatalf("ultimate_oscillator open and close-fallback references must be vetoed, never stamped: vetoed=%v stamped=%+v", report.VetoedResearch, report.Stamped)
	}
	for _, item := range legacy["strategies"].([]interface{}) {
		if _, has := item.(map[string]interface{})["allow_no_edge"]; has {
			t.Fatalf("%v gained allow_no_edge", item.(map[string]interface{})["id"])
		}
	}
}

func TestV20NoEdgeMigrationStampsOnlyNamesThatRanLiveBeforeV20(t *testing.T) {
	setNoEdgeLiveCredentials(t)
	assertUnstamped := func(t *testing.T, path string, version int, id string) {
		t.Helper()
		report, err := migrateV20NoEdge(readRawConfig(t, path), version)
		if err != nil {
			t.Fatal(err)
		}
		if len(report.Stamped) != 0 || !reflect.DeepEqual(report.VetoedResearch, []string{id}) {
			t.Fatalf("report stamped=%v unstamped=%v, want no stamp and %s listed unstamped", stampedIDs(report), report.VetoedResearch, id)
		}
		if !strings.Contains(strings.Join(report.lines(), "\n"), "did not stamp "+id+":") {
			t.Fatalf("notice does not name %s as unstamped: %v", id, report.lines())
		}
		_, err = LoadConfig(path)
		if err == nil || !strings.Contains(err.Error(), "strategy "+id+":") {
			t.Fatalf("%s must be refused by the no-edge admission after migration, got %v", id, err)
		}
		if v, has := rawStrategiesByID(t, path)[id]["allow_no_edge"]; has {
			t.Fatalf("%s gained allow_no_edge=%v", id, v)
		}
	}

	t.Run("versionless live williams_r_reversal open", func(t *testing.T) {
		path := writeNoEdgeConfig(t, 0, noEdgeHLStrategy("hl-wrr", "williams_r_reversal", "BTC", "4h", "--mode=live"))
		assertUnstamped(t, path, 0, "hl-wrr")
	})

	t.Run("v19 live breakout with a williams_r_reversal close_strategies fallback", func(t *testing.T) {
		sc := noEdgeHLStrategy("hl-bo-wrr", "breakout", "BTC", "4h", "--mode=live")
		sc["close_strategies"] = []interface{}{map[string]interface{}{"name": "williams_r_reversal"}}
		path := writeNoEdgeConfig(t, 19, sc)
		assertUnstamped(t, path, 19, "hl-bo-wrr")
	})

	t.Run("a no-edge name registered after v20 is unstamped without a deny-set entry", func(t *testing.T) {
		const later = "later_no_edge_candidate"
		if _, research := v20FormerResearchStrategies[later]; research {
			t.Fatalf("%s must stay outside the research deny set for this case", later)
		}
		noEdgeStrategies[later] = noEdgeEvidence{Source: "study_fail", Ref: "backtest/candidates/later/REPORT.md"}
		t.Cleanup(func() { delete(noEdgeStrategies, later) })
		openRef := noEdgeHLStrategy("hl-later", later, "BTC", "4h", "--mode=live")
		closeRef := noEdgeHLStrategy("hl-rsi-later", "rsi", "ETH", "4h", "--mode=live")
		closeRef["close_strategy"] = map[string]interface{}{"name": later}
		eligible := noEdgeHLStrategy("hl-rsi", "rsi", "SOL", "4h", "--mode=live")
		raw := readRawConfig(t, writeNoEdgeConfig(t, 19, openRef, closeRef, eligible))
		report, err := migrateV20NoEdge(raw, 19)
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{"hl-later", "hl-rsi-later"}; !reflect.DeepEqual(report.VetoedResearch, want) {
			t.Fatalf("unstamped = %v, want %v", report.VetoedResearch, want)
		}
		if got := stampedIDs(report); !reflect.DeepEqual(got, []string{"hl-rsi"}) {
			t.Fatalf("stamped = %v, want only the pre-v20 rsi strategy", got)
		}
		for _, item := range raw["strategies"].([]interface{}) {
			sc := item.(map[string]interface{})
			_, has := sc["allow_no_edge"]
			if want := sc["id"] == "hl-rsi"; has != want {
				t.Fatalf("%v allow_no_edge present=%v, want %v", sc["id"], has, want)
			}
		}
	})

	t.Run("the stamp allowlist and the research deny set are disjoint", func(t *testing.T) {
		for name := range v20StampEligibleStrategies {
			if _, research := v20FormerResearchStrategies[name]; research {
				t.Fatalf("%s is both stamp-eligible and research-only", name)
			}
		}
	})
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
		if _, refused := productionRefusedStrategies[name]; refused {
			if err == nil || !strings.Contains(err.Error(), "production_refused") {
				t.Fatalf("%s is production_refused but the add returned %v", name, err)
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
	for _, name := range []string{"awesome_oscillator", "chande_momentum", "funding_skew"} {
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

func TestChandeMomentumAndDualThrustOperatorAddCreateBothDirectionHourlyPaperEntries(t *testing.T) {
	setNoEdgeLiveCredentials(t)
	dualThrustClose := map[string]interface{}{"name": "tiered_tp_pct", "params": map[string]interface{}{
		"tp_tiers": []interface{}{map[string]interface{}{"profit_pct": 0.03, "close_fraction": 1.0}}}}
	cases := []struct {
		name      string
		wantID    string
		closedBar bool
		wantClose map[string]interface{}
	}{
		{name: "chande_momentum", wantID: "hl-cmo-eth"},
		{name: "dual_thrust", wantID: "hl-dt-eth", closedBar: true, wantClose: dualThrustClose},
	}
	for _, tc := range cases {
		id, entry, err := buildAddStrategyEntry(tc.name, "hyperliquid", "eth")
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if id != tc.wantID {
			t.Fatalf("%s: id = %q, want %s", tc.name, id, tc.wantID)
		}
		var sc map[string]interface{}
		if err := json.Unmarshal(entry, &sc); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if sc["direction"] != DirectionBoth {
			t.Fatalf("%s: direction = %v, want %q", tc.name, sc["direction"], DirectionBoth)
		}
		if _, ack := sc["allow_no_edge"]; ack {
			t.Fatalf("%s: operator add must not acknowledge no-edge live use", tc.name)
		}
		if tc.closedBar && sc["closed_bar_decisions"] != true {
			t.Fatalf("%s: closed_bar_decisions = %v, want true", tc.name, sc["closed_bar_decisions"])
		}
		if tc.wantClose != nil && !reflect.DeepEqual(sc["close_strategy"], tc.wantClose) {
			t.Fatalf("%s: close_strategy = %v, want the study close %v", tc.name, sc["close_strategy"], tc.wantClose)
		}
		args, _ := sc["args"].([]interface{})
		if !reflect.DeepEqual(args, []interface{}{tc.name, "ETH", "1h", "--mode=paper"}) {
			t.Fatalf("%s: args = %v, want the 1h explicit-paper positional form", tc.name, args)
		}
		cfg, err := LoadConfig(writeNoEdgeConfigRoot(t, CurrentConfigVersion, map[string]interface{}{"market_feed": "websocket"}, sc))
		if err != nil {
			t.Fatalf("%s: operator-added entry must load as paper without acknowledgement: %v", tc.name, err)
		}
		got := cfg.Strategies[0]
		if portfolioScopeFor(got) != ScopePaper || edgeGateModeForStrategy(got).Kind != edgeGateModePaper || got.AllowNoEdgeAcknowledged() {
			t.Fatalf("%s: operator-added entry is not explicit unacknowledged paper: scope=%q", tc.name, portfolioScopeFor(got))
		}
		if tc.closedBar && !got.ClosedBarDecisions {
			t.Fatalf("%s: operator-added entry must load with closed_bar_decisions", tc.name)
		}
		if _, _, err := buildAddStrategyEntry(tc.name, "binanceus", "ETH"); err == nil {
			t.Fatalf("%s: spot add must refuse the futures-only strategy", tc.name)
		}
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

func TestEaseOfMovementAdmissionThroughRealLoaders(t *testing.T) {
	setNoEdgeLiveCredentials(t)
	const name = "ease_of_movement_breakout"
	const ref = "backtest/candidates/ease_of_movement_1671/REPORT.md"
	ack := func(sc map[string]interface{}) map[string]interface{} {
		sc["allow_no_edge"] = true
		return sc
	}
	asClose := func(sc map[string]interface{}) map[string]interface{} {
		sc["close_strategy"] = map[string]interface{}{"name": name}
		return sc
	}
	cases := []struct {
		label     string
		sc        map[string]interface{}
		wantErr   []string
		wantScope PortfolioScope
	}{
		{"explicit paper", noEdgeHLStrategy("hl-eombo-btc", name, "BTC", "1h", "--mode=paper"), nil, ScopePaper},
		{"explicit live refused", noEdgeHLStrategy("hl-eombo-btc", name, "BTC", "1h", "--mode=live"),
			[]string{`open strategy "` + name + `"`, "source study_fail", ref, "explicit live mode"}, ""},
		{"missing mode refused", noEdgeHLStrategy("hl-eombo-btc", name, "BTC", "1h"), []string{"missing --mode"}, ""},
		{"invalid mode refused", noEdgeHLStrategy("hl-eombo-btc", name, "BTC", "1h", "--mode=paper", "--mode=live"), []string{"invalid mode"}, ""},
		{"explicit live acknowledged", ack(noEdgeHLStrategy("hl-eombo-btc", name, "BTC", "1h", "--mode=live")), nil, ScopeLive},
		{"close fallback refused live", asClose(noEdgeHLStrategy("hl-bo-btc", "breakout", "BTC", "1h", "--mode=live")),
			[]string{`close strategy "` + name + `"`, "explicit live mode"}, ""},
		{"close fallback refused missing mode", asClose(noEdgeHLStrategy("hl-bo-btc", "breakout", "BTC", "1h")),
			[]string{`close strategy "` + name + `"`, "missing --mode"}, ""},
		{"close fallback paper", asClose(noEdgeHLStrategy("hl-bo-btc", "breakout", "BTC", "1h", "--mode=paper")), nil, ScopePaper},
		{"close fallback acknowledged", ack(asClose(noEdgeHLStrategy("hl-bo-btc", "breakout", "BTC", "1h", "--mode=live"))), nil, ScopeLive},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
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
					t.Fatalf("portfolio scope = %q, want %q", got, tc.wantScope)
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

func assertEaseOfMovementPaperEntry(t *testing.T, sc StrategyConfig) {
	t.Helper()
	if sc.ID != "hl-eombo-btc" || sc.Direction != DirectionBoth || sc.MarginMode != "isolated" {
		t.Fatalf("entry identity = %s direction=%q margin=%q", sc.ID, sc.Direction, sc.MarginMode)
	}
	if sc.AllowNoEdge != nil || portfolioScopeFor(sc) != ScopePaper || edgeGateModeForStrategy(sc).Kind != edgeGateModePaper {
		t.Fatalf("%s must be explicit paper with no acknowledgement", sc.ID)
	}
	if !sc.ClosedBarDecisions {
		t.Fatalf("%s must decide on closed bars", sc.ID)
	}
	if args := appendClosedBarDecisionArgs(sc.Args, sc, nil); !slices.Contains(args, closedBarDecisionsFlag) {
		t.Fatalf("%s check args %v lack %s", sc.ID, args, closedBarDecisionsFlag)
	}
	refs := sc.closeRefs()
	if len(refs) != 1 || refs[0].Name != "tiered_tp_atr" {
		t.Fatalf("close owners = %+v, want exactly tiered_tp_atr", refs)
	}
	if sc.StopLossATRMult == nil || *sc.StopLossATRMult != DefaultStopLossATRMult {
		t.Fatalf("stop owner = %v, want the loader default stop_loss_atr_mult %g", sc.StopLossATRMult, DefaultStopLossATRMult)
	}
	if sc.StopLossPct != nil || sc.StopLossMarginPct != nil || sc.TrailingStopPct != nil || sc.TrailingStopATRMult != nil ||
		sc.StopLossATRMultRegime.IsConfigured() || sc.TrailingStopATRMultRegime.IsConfigured() || strategyUsesUnifiedRegimeClose(sc) {
		t.Fatalf("%s has a second stop owner", sc.ID)
	}
	if refsEdge := noEdgeReferences(sc); len(refsEdge) != 1 || refsEdge[0] != (edgeReference{Role: edgeRefRoleOpen, Name: "ease_of_movement_breakout"}) {
		t.Fatalf("no-edge references = %+v", refsEdge)
	}
}

func TestEaseOfMovementOperatorAddPathsCreateExplicitPaperEntries(t *testing.T) {
	setNoEdgeLiveCredentials(t)
	id, entry, err := buildAddStrategyEntry("ease_of_movement_breakout", "hyperliquid", "btc")
	if err != nil || id != "hl-eombo-btc" {
		t.Fatalf("build = %q, %v", id, err)
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(entry, &raw); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(raw["args"], []interface{}{"ease_of_movement_breakout", "BTC", "1h", "--mode=paper"}) {
		t.Fatalf("args = %v", raw["args"])
	}
	if !reflect.DeepEqual(raw["close_strategy"], map[string]interface{}{"name": "tiered_tp_atr"}) {
		t.Fatalf("close_strategy = %v", raw["close_strategy"])
	}
	if raw["closed_bar_decisions"] != true {
		t.Fatalf("closed_bar_decisions = %v, want true", raw["closed_bar_decisions"])
	}
	if _, _, err := buildAddStrategyEntry("ease_of_movement_breakout", "binanceus", "BTC"); err == nil {
		t.Fatal("futures-only candidate was accepted for binanceus")
	}
	if _, momentum, err := buildAddStrategyEntry("momentum", "hyperliquid", "BTC"); err != nil || strings.Contains(string(momentum), "close_strategy") || strings.Contains(string(momentum), "closed_bar_decisions") {
		t.Fatalf("other strategies must keep their existing add entry: %s %v", momentum, err)
	}

	t.Run("discord", func(t *testing.T) {
		path := writeNoEdgeConfig(t, CurrentConfigVersion, noEdgeHLStrategy("hl-rsi-eth", "rsi", "ETH", "1h", "--mode=paper"))
		root := readConfigRoot(t, path)
		if _, err := addStrategyToRoot(root, "ease_of_movement_breakout", "hyperliquid", "BTC"); err != nil {
			t.Fatal(err)
		}
		data, err := json.MarshalIndent(root, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("Discord-added candidate must load without acknowledgement: %v", err)
		}
		assertEaseOfMovementPaperEntry(t, cfg.Strategies[1])
	})

	t.Run("status page", func(t *testing.T) {
		ss, path, _ := newStructuralTestServer(t)
		params := `{"name":"ease_of_movement_breakout","platform":"hyperliquid","asset":"BTC"}`
		confirm := structuralConfirm(t, ss, "add-strategy", "", params)
		if confirm.ConfirmPhrase != "hl-eombo-btc" {
			t.Fatalf("confirm phrase = %q", confirm.ConfirmPhrase)
		}
		body := `{"nonce":"` + confirm.Nonce + `","params":` + params + `}`
		if w := mutationPost(ss, ss.handleAPIAddStrategy, "/api/config/add-strategy", body, nil); w.Code != 200 {
			t.Fatalf("add-strategy status = %d, body %s", w.Code, w.Body.String())
		}
		cfg, err := LoadConfigForProbe(path)
		if err != nil {
			t.Fatalf("status-page-added candidate must load: %v", err)
		}
		var found bool
		for _, sc := range cfg.Strategies {
			if sc.ID == "hl-eombo-btc" {
				found = true
				assertEaseOfMovementPaperEntry(t, sc)
			}
		}
		if !found {
			t.Fatal("status-page add did not write hl-eombo-btc")
		}
	})
}

func aroonStudyStrategy(id string, params map[string]interface{}, modeArgs ...string) map[string]interface{} {
	sc := noEdgeHLStrategy(id, append([]string{"aroon_trend", "BTC", "4h"}, modeArgs...)...)
	delete(sc, "stop_loss_pct")
	sc["direction"] = DirectionBoth
	sc["stop_loss_atr_mult"] = 1.0
	sc["open_strategy"] = map[string]interface{}{"name": "aroon_trend", "params": params}
	sc["close_strategy"] = map[string]interface{}{
		"name":   "tiered_tp_pct",
		"params": map[string]interface{}{"tp_tiers": []interface{}{map[string]interface{}{"profit_pct": 0.03, "close_fraction": 1.0}}},
	}
	return sc
}

func TestAroonTrendAddAdmissionAndParamPreflightThroughRealLoaders(t *testing.T) {
	setNoEdgeLiveCredentials(t)
	id, entry, err := buildAddStrategyEntry("aroon_trend", "hyperliquid", "BTC")
	if err != nil {
		t.Fatal(err)
	}
	if id != "hl-aroon-btc" {
		t.Fatalf("generated id = %q, want hl-aroon-btc", id)
	}
	owners := 0
	for name, short := range knownShortNames {
		if short == "aroon" {
			owners++
			if name != "aroon_trend" {
				t.Fatalf("short name aroon also owned by %s", name)
			}
		}
	}
	if owners != 1 {
		t.Fatalf("short name aroon owners = %d", owners)
	}
	if _, _, err := buildAddStrategyEntry("aroon_trend", "binanceus", "BTC"); err == nil {
		t.Fatal("aroon_trend is futures-only but binanceus add accepted it")
	}
	var added map[string]interface{}
	if err := json.Unmarshal(entry, &added); err != nil {
		t.Fatal(err)
	}
	path := writeNoEdgeConfig(t, CurrentConfigVersion, added)
	cfg, loadErr := LoadConfig(path)
	if _, probeErr := LoadConfigForProbe(path); loadErr != nil || probeErr != nil {
		t.Fatalf("generated entry must load: load=%v probe=%v", loadErr, probeErr)
	}
	got := cfg.Strategies[0]
	if got.Direction != DirectionBoth || portfolioScopeFor(got) != ScopePaper || edgeGateModeForStrategy(got).Kind != edgeGateModePaper || got.AllowNoEdgeAcknowledged() {
		t.Fatalf("generated entry direction=%q scope=%q gate=%q ack=%v", got.Direction, portfolioScopeFor(got), edgeGateModeForStrategy(got).Kind, got.AllowNoEdgeAcknowledged())
	}
	if tag := edgeStatusSummaryTag(got); tag != "edge=no_edge:study_inconclusive(paper)" {
		t.Fatalf("summary tag = %q", tag)
	}

	seed := map[string]interface{}{"period": 25.0, "strength": 70.0}
	study := aroonStudyStrategy("hl-aroon-study", seed, "--mode=paper")
	cfg, err = LoadConfig(writeNoEdgeConfig(t, CurrentConfigVersion, study))
	if err != nil {
		t.Fatalf("study config must load: %v", err)
	}
	if sc := cfg.Strategies[0]; sc.CloseStrategy == nil || sc.CloseStrategy.Name != "tiered_tp_pct" || sc.StopLossATRMult == nil || *sc.StopLossATRMult != 1.0 {
		t.Fatalf("study close/stop owners = %+v / %v", sc.CloseStrategy, sc.StopLossATRMult)
	}

	ack := func(sc map[string]interface{}) map[string]interface{} {
		sc["allow_no_edge"] = true
		return sc
	}
	fallback := func(params map[string]interface{}) map[string]interface{} {
		sc := noEdgeHLStrategy("hl-aroon-fb", "breakout", "BTC", "4h", "--mode=paper")
		sc["close_strategy"] = map[string]interface{}{"name": "aroon_trend", "params": params}
		return sc
	}
	legacyArgs := aroonStudyStrategy("hl-aroon-legacy", seed, "--mode=paper")
	legacyArgs["args"] = []string{"aroon_trend", "BTC", "4h", "--mode=paper", "--params", `{"period":true}`}
	cases := []struct {
		name    string
		sc      map[string]interface{}
		wantErr []string
	}{
		{"live refused without acknowledgement", aroonStudyStrategy("hl-a", seed, "--mode=live"), []string{`open strategy "aroon_trend"`, "source study_inconclusive", "backtest/candidates/aroon_trend_1643/REPORT.md", "explicit live mode"}},
		{"live acknowledged", ack(aroonStudyStrategy("hl-a", seed, "--mode=live")), nil},
		{"missing mode refused", aroonStudyStrategy("hl-a", seed), []string{"missing --mode"}},
		{"boundary low", aroonStudyStrategy("hl-a", map[string]interface{}{"period": 2.0, "strength": 0.0}, "--mode=paper"), nil},
		{"boundary high", aroonStudyStrategy("hl-a", map[string]interface{}{"period": 198.0, "strength": 100.0}, "--mode=paper"), nil},
		{"period below range", aroonStudyStrategy("hl-a", map[string]interface{}{"period": 1.0}, "--mode=paper"), []string{"open strategy aroon_trend params", "period must be in [2, 198]"}},
		{"period above range", aroonStudyStrategy("hl-a", map[string]interface{}{"period": 199.0}, "--mode=paper"), []string{"period must be in [2, 198]"}},
		{"fractional period", aroonStudyStrategy("hl-a", map[string]interface{}{"period": 2.5}, "--mode=paper"), []string{"period must be an integer"}},
		{"boolean period", aroonStudyStrategy("hl-a", map[string]interface{}{"period": true}, "--mode=paper"), []string{"period must be a number"}},
		{"string period", aroonStudyStrategy("hl-a", map[string]interface{}{"period": "25"}, "--mode=paper"), []string{"period must be a number"}},
		{"null strength", aroonStudyStrategy("hl-a", map[string]interface{}{"strength": nil}, "--mode=paper"), []string{"strength must be a number"}},
		{"strength above range", aroonStudyStrategy("hl-a", map[string]interface{}{"strength": 100.5}, "--mode=paper"), []string{"strength must be finite and in [0, 100]"}},
		{"negative strength", aroonStudyStrategy("hl-a", map[string]interface{}{"strength": -1.0}, "--mode=paper"), []string{"strength must be finite"}},
		{"close fallback params checked", fallback(map[string]interface{}{"period": 0.0}), []string{"close strategy aroon_trend params", "period must be in"}},
		{"close fallback valid", fallback(map[string]interface{}{"period": 14.0}), nil},
		{"ignored legacy params are not active inputs", legacyArgs, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeNoEdgeConfig(t, CurrentConfigVersion, tc.sc)
			_, loadErr := LoadConfig(path)
			_, probeErr := LoadConfigForProbe(path)
			if len(tc.wantErr) == 0 {
				if loadErr != nil || probeErr != nil {
					t.Fatalf("unexpected refusal: load=%v probe=%v", loadErr, probeErr)
				}
				return
			}
			if loadErr == nil || probeErr == nil {
				t.Fatalf("expected refusal containing %q: load=%v probe=%v", tc.wantErr, loadErr, probeErr)
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(loadErr.Error(), want) || !strings.Contains(probeErr.Error(), want) {
					t.Fatalf("errors missing %q:\nload:  %v\nprobe: %v", want, loadErr, probeErr)
				}
			}
		})
	}
}

func TestChandeMomentumIntervalMustMatchTheConfiguredTimeframe(t *testing.T) {
	setNoEdgeLiveCredentials(t)
	t.Setenv("TOPSTEP_API_KEY", "k")
	t.Setenv("TOPSTEP_API_SECRET", "s")
	t.Setenv("TOPSTEP_ACCOUNT_ID", "1")
	const cmo = "chande_momentum"
	withOpenParams := func(sc map[string]interface{}, params map[string]interface{}) map[string]interface{} {
		sc["open_strategy"] = map[string]interface{}{"name": cmo, "params": params}
		return sc
	}
	withCloseParams := func(sc map[string]interface{}, params map[string]interface{}) map[string]interface{} {
		ref := map[string]interface{}{"name": cmo}
		if params != nil {
			ref["params"] = params
		}
		sc["close_strategy"] = ref
		return sc
	}
	okx := func(timeframe string) map[string]interface{} {
		return map[string]interface{}{
			"id": "okx-cmo", "type": "perps", "platform": "okx", "script": "shared_scripts/check_okx.py",
			"args": []string{cmo, "ETH", timeframe, "--mode=paper"}, "capital": 100.0, "max_drawdown_pct": 10.0,
			"leverage": 1.0,
		}
	}
	topstep := map[string]interface{}{
		"id": "ts-cmo", "type": "futures", "platform": "topstep", "script": "shared_scripts/check_topstep.py",
		"args": []string{cmo, "ES", "1h", "--mode=paper"}, "capital": 1000.0, "max_drawdown_pct": 10.0,
	}
	interval := func(ms interface{}) map[string]interface{} {
		return map[string]interface{}{"expected_interval_ms": ms}
	}
	manual := func(closeParams map[string]interface{}) map[string]interface{} {
		ref := map[string]interface{}{"name": cmo}
		if closeParams != nil {
			ref["params"] = closeParams
		}
		return map[string]interface{}{
			"id": "hl-cmo-manual", "type": "manual", "platform": "hyperliquid", "symbol": "ETH", "timeframe": "4h",
			"leverage": 1.0, "capital": 100.0, "allow_no_edge": true, "close_strategy": ref,
		}
	}
	cases := []struct {
		name    string
		sc      map[string]interface{}
		wantErr []string
		wantRef string
	}{
		{"hourly registry default loads", noEdgeHLStrategy("hl-cmo", cmo, "ETH", "1h", "--mode=paper"), nil, ""},
		{"4h registry default refused", noEdgeHLStrategy("hl-cmo", cmo, "ETH", "4h", "--mode=paper"),
			[]string{`strategy hl-cmo: open strategy "chande_momentum": registry default expected_interval_ms=3600000 does not match timeframe 4h (14400000 ms)`, `"expected_interval_ms": 14400000`}, ""},
		{"daily registry default refused", noEdgeHLStrategy("hl-cmo", cmo, "ETH", "1d", "--mode=paper"),
			[]string{"does not match timeframe 1d (86400000 ms)"}, ""},
		{"4h explicit match loads", withOpenParams(noEdgeHLStrategy("hl-cmo", cmo, "ETH", "4h", "--mode=paper"), interval(14400000)), nil, `"expected_interval_ms":14400000`},
		{"hourly explicit 4h value refused", withOpenParams(noEdgeHLStrategy("hl-cmo", cmo, "ETH", "1h", "--mode=paper"), interval(14400000)),
			[]string{"explicit expected_interval_ms=14400000 does not match timeframe 1h (3600000 ms)"}, ""},
		{"non-integer value refused", withOpenParams(noEdgeHLStrategy("hl-cmo", cmo, "ETH", "4h", "--mode=paper"), interval(14400000.5)),
			[]string{"params.expected_interval_ms must be a positive integer"}, ""},
		{"string value refused", withOpenParams(noEdgeHLStrategy("hl-cmo", cmo, "ETH", "4h", "--mode=paper"), interval("14400000")),
			[]string{"params.expected_interval_ms must be a positive integer"}, ""},
		{"close fallback registry default refused on 4h", withCloseParams(noEdgeHLStrategy("hl-cmo", "breakout", "ETH", "4h", "--mode=paper"), nil),
			[]string{`close strategy "chande_momentum": registry default expected_interval_ms=3600000 does not match timeframe 4h`}, ""},
		{"close fallback explicit match loads", withCloseParams(noEdgeHLStrategy("hl-cmo", "breakout", "ETH", "4h", "--mode=paper"), interval(14400000)), nil, ""},
		{"close fallback inherits the open params", withCloseParams(withOpenParams(noEdgeHLStrategy("hl-cmo", cmo, "ETH", "4h", "--mode=paper"), interval(14400000)), nil), nil, ""},
		{"okx perps 4h explicit match loads", withOpenParams(okx("4h"), interval(14400000)), nil, ""},
		{"okx perps 4h registry default refused", okx("4h"), []string{`strategy okx-cmo: open strategy "chande_momentum": registry default`}, ""},
		{"topstep refused for session breaks", topstep, []string{`strategy ts-cmo: open strategy "chande_momentum" holds every signal`, "type=futures platform=topstep", "runs only on perps"}, ""},
		{"manual close fallback registry default refused on 4h", manual(nil),
			[]string{`strategy hl-cmo-manual: close strategy "chande_momentum": registry default expected_interval_ms=3600000 does not match timeframe 4h`, `"expected_interval_ms": 14400000`}, ""},
		{"manual close fallback explicit match loads", manual(interval(14400000)), nil, ""},
		{"other strategies on 4h unchanged", noEdgeHLStrategy("hl-x", "breakout", "ETH", "4h", "--mode=paper"), nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeNoEdgeConfig(t, CurrentConfigVersion, tc.sc)
			original, _ := os.ReadFile(path)
			cfg, loadErr := LoadConfig(path)
			_, probeErr := LoadConfigForProbe(path)
			_, readErr := LoadConfigReadOnly(path)
			for name, err := range map[string]error{"load": loadErr, "probe": probeErr, "readonly": readErr} {
				if len(tc.wantErr) == 0 {
					if err != nil {
						t.Fatalf("%s: unexpected refusal: %v", name, err)
					}
					continue
				}
				if err == nil {
					t.Fatalf("%s: expected refusal containing %q", name, tc.wantErr)
				}
				for _, want := range tc.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("%s: error missing %q: %v", name, want, err)
					}
				}
			}
			if len(tc.wantErr) > 0 {
				if after, _ := os.ReadFile(path); !bytes.Equal(original, after) {
					t.Fatal("a refused config must stay byte-identical")
				}
				return
			}
			if tc.wantRef != "" {
				args, err := buildStrategyRefsArg(cfg.Strategies[0], "", false)
				if err != nil || len(args) != 2 || !strings.Contains(args[1], tc.wantRef) {
					t.Fatalf("strategy refs = %v, %v; want %s", args, err, tc.wantRef)
				}
			}
		})
	}
}

func TestChandeMomentumAndDualThrustStudyParityConfigsStillLoad(t *testing.T) {
	setNoEdgeLiveCredentials(t)
	cases := []struct {
		name      string
		candidate string
		closedBar bool
	}{
		{"chande_momentum", "chande_momentum_1661", false},
		{"dual_thrust", "dual_thrust_1642", true},
	}
	for _, tc := range cases {
		for timeframe, want := range map[string]string{"1h": `"expected_interval_ms":3600000`, "4h": `"expected_interval_ms":14400000`} {
			t.Run(tc.name+"/"+timeframe, func(t *testing.T) {
				src, err := os.ReadFile(filepath.Join("..", "backtest", "candidates", tc.candidate, "paper_parity_"+timeframe+".json"))
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(t.TempDir(), "config.json")
				if err := os.WriteFile(path, src, 0600); err != nil {
					t.Fatal(err)
				}
				cfg, err := LoadConfig(path)
				if err != nil {
					t.Fatalf("study parity config refused: %v", err)
				}
				got := cfg.Strategies[0]
				if tc.closedBar && (portfolioScopeFor(got) != ScopePaper || got.AllowNoEdgeAcknowledged() || !got.ClosedBarDecisions) {
					t.Fatalf("study parity config must be unacknowledged paper with closed_bar_decisions: scope=%q closed=%t", portfolioScopeFor(got), got.ClosedBarDecisions)
				}
				args, err := buildStrategyRefsArg(got, "", false)
				if err != nil || len(args) != 2 || !strings.Contains(args[1], want) {
					t.Fatalf("strategy refs = %v, %v; want %s", args, err, want)
				}
			})
		}
	}
}

func dualThrustHLStrategy(id string, args ...string) map[string]interface{} {
	sc := noEdgeHLStrategy(id, args...)
	sc["closed_bar_decisions"] = true
	return sc
}

func TestDualThrustAdmissionMatrixThroughRealLoaders(t *testing.T) {
	setNoEdgeLiveCredentials(t)
	const dt = "dual_thrust"
	ack := func(sc map[string]interface{}, v interface{}) map[string]interface{} {
		sc["allow_no_edge"] = v
		return sc
	}
	withClose := func(sc map[string]interface{}, name string) map[string]interface{} {
		sc["close_strategy"] = map[string]interface{}{"name": name}
		return sc
	}
	cases := []struct {
		name      string
		sc        map[string]interface{}
		wantErr   []string
		wantScope PortfolioScope
	}{
		{"paper equals form", dualThrustHLStrategy("hl-dt", dt, "ETH", "1h", "--mode=paper"), nil, ScopePaper},
		{"paper space form", dualThrustHLStrategy("hl-dt", dt, "ETH", "1h", "--mode", "paper"), nil, ScopePaper},
		{"live refused", dualThrustHLStrategy("hl-dt", dt, "ETH", "1h", "--mode=live"),
			[]string{`open strategy "dual_thrust"`, "source study_fail", "backtest/candidates/dual_thrust_1642/REPORT.md", "explicit live mode", `"allow_no_edge": true`}, ""},
		{"live acknowledged", ack(dualThrustHLStrategy("hl-dt", dt, "ETH", "1h", "--mode=live"), true), nil, ScopeLive},
		{"explicit false refused", ack(dualThrustHLStrategy("hl-dt", dt, "ETH", "1h", "--mode=live"), false), []string{"explicit live mode"}, ""},
		{"missing mode refused", dualThrustHLStrategy("hl-dt", dt, "ETH", "1h"), []string{`open strategy "dual_thrust"`, "missing --mode"}, ""},
		{"missing mode acknowledged keeps paper scope", ack(dualThrustHLStrategy("hl-dt", dt, "ETH", "1h"), true), nil, ScopePaper},
		{"repeated mode refused even acknowledged", ack(dualThrustHLStrategy("hl-dt", dt, "ETH", "1h", "--mode=paper", "--mode=paper"), true), []string{"invalid mode", "given 2 times"}, ""},
		{"conflicting mode refused", dualThrustHLStrategy("hl-dt", dt, "ETH", "1h", "--mode=paper", "--mode", "live"), []string{"invalid mode", "given 2 times"}, ""},
		{"dangling mode refused", dualThrustHLStrategy("hl-dt", dt, "ETH", "1h", "--mode"), []string{"invalid mode", "dangling --mode"}, ""},
		{"unknown mode refused", dualThrustHLStrategy("hl-dt", dt, "ETH", "1h", "--mode=Paper"), []string{"invalid mode", `"Paper"`}, ""},
		{"close fallback paper", withClose(dualThrustHLStrategy("hl-dt", "breakout", "ETH", "1h", "--mode=paper"), dt), nil, ScopePaper},
		{"close fallback live refused", withClose(dualThrustHLStrategy("hl-dt", "breakout", "ETH", "1h", "--mode=live"), dt),
			[]string{`close strategy "dual_thrust"`, "source study_fail", "explicit live mode"}, ""},
		{"close fallback missing mode refused", withClose(dualThrustHLStrategy("hl-dt", "breakout", "ETH", "1h"), dt),
			[]string{`close strategy "dual_thrust"`, "missing --mode"}, ""},
		{"close fallback acknowledged", ack(withClose(dualThrustHLStrategy("hl-dt", "breakout", "ETH", "1h", "--mode=live"), dt), true), nil, ScopeLive},
		{"open with native close live refused", withClose(dualThrustHLStrategy("hl-dt", dt, "ETH", "1h", "--mode=live"), "tiered_tp_pct"),
			[]string{`open strategy "dual_thrust"`, "explicit live mode"}, ""},
		{"open without closed bar decisions refused", noEdgeHLStrategy("hl-dt", dt, "ETH", "1h", "--mode=paper"),
			[]string{`strategy hl-dt: open strategy "dual_thrust" decides only on closed candles`, `"closed_bar_decisions": true`}, ""},
		{"open with closed bar decisions false refused", func() map[string]interface{} {
			sc := dualThrustHLStrategy("hl-dt", dt, "ETH", "1h", "--mode=paper")
			sc["closed_bar_decisions"] = false
			return sc
		}(), []string{`strategy hl-dt: open strategy "dual_thrust" decides only on closed candles`}, ""},
		{"acknowledged live without closed bar decisions refused", ack(noEdgeHLStrategy("hl-dt", dt, "ETH", "1h", "--mode=live"), true),
			[]string{`open strategy "dual_thrust" decides only on closed candles`}, ""},
		{"close fallback without closed bar decisions refused", withClose(noEdgeHLStrategy("hl-dt", "breakout", "ETH", "1h", "--mode=paper"), dt),
			[]string{`strategy hl-dt: close strategy "dual_thrust" decides only on closed candles`}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeNoEdgeConfig(t, CurrentConfigVersion, tc.sc)
			original, _ := os.ReadFile(path)
			cfg, loadErr := LoadConfig(path)
			_, probeErr := LoadConfigForProbe(path)
			_, readErr := LoadConfigReadOnly(path)
			for loader, err := range map[string]error{"load": loadErr, "probe": probeErr, "readonly": readErr} {
				if (err == nil) != (len(tc.wantErr) == 0) {
					t.Fatalf("%s: err=%v, want refusal %v", loader, err, tc.wantErr)
				}
				for _, want := range tc.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("%s: error missing %q: %v", loader, want, err)
					}
				}
			}
			if len(tc.wantErr) > 0 {
				if after, _ := os.ReadFile(path); !bytes.Equal(original, after) {
					t.Fatal("a refused config must stay byte-identical")
				}
				return
			}
			got := cfg.Strategies[0]
			if scope := portfolioScopeFor(got); scope != tc.wantScope {
				t.Fatalf("portfolio scope = %q, want %q", scope, tc.wantScope)
			}
			if _, gated := noEdgeEvidenceFor(dt); !gated {
				t.Fatal("dual_thrust must stay a no-edge name")
			}
		})
	}
	for _, v := range []interface{}{"true", 1, "yes"} {
		sc := dualThrustHLStrategy("hl-dt", dt, "ETH", "1h", "--mode=live")
		sc["allow_no_edge"] = v
		if cfg, err := LoadConfig(writeNoEdgeConfig(t, CurrentConfigVersion, sc)); err == nil {
			t.Fatalf("allow_no_edge=%#v loaded (ack=%v); a non-boolean must never grant live admission", v, cfg.Strategies[0].AllowNoEdgeAcknowledged())
		}
	}
}

func TestDualThrustIntervalMustMatchTheConfiguredTimeframe(t *testing.T) {
	setNoEdgeLiveCredentials(t)
	const dt = "dual_thrust"
	withOpenParams := func(sc map[string]interface{}, params map[string]interface{}) map[string]interface{} {
		sc["open_strategy"] = map[string]interface{}{"name": dt, "params": params}
		return sc
	}
	withClose := func(sc map[string]interface{}) map[string]interface{} {
		sc["close_strategy"] = map[string]interface{}{"name": dt}
		return sc
	}
	manual := map[string]interface{}{
		"id": "hl-dt-manual", "type": "manual", "platform": "hyperliquid", "symbol": "ETH", "timeframe": "4h",
		"leverage": 1.0, "capital": 100.0, "allow_no_edge": true, "close_strategy": map[string]interface{}{"name": dt},
	}
	manualMatched := map[string]interface{}{
		"id": "hl-dt-manual", "type": "manual", "platform": "hyperliquid", "symbol": "ETH", "timeframe": "4h",
		"leverage": 1.0, "capital": 100.0, "allow_no_edge": true,
		"close_strategy": map[string]interface{}{"name": dt, "params": map[string]interface{}{"expected_interval_ms": 14400000}},
	}
	withCloseParams := func(sc map[string]interface{}, params map[string]interface{}) map[string]interface{} {
		sc["close_strategy"] = map[string]interface{}{"name": dt, "params": params}
		return sc
	}
	cases := []struct {
		name    string
		sc      map[string]interface{}
		wantErr string
		wantRef string
	}{
		{"hourly registry default loads", dualThrustHLStrategy("hl-dt", dt, "ETH", "1h", "--mode=paper"), "", ""},
		{"4h registry default refused", dualThrustHLStrategy("hl-dt", dt, "ETH", "4h", "--mode=paper"),
			`open strategy "dual_thrust": registry default expected_interval_ms=3600000 does not match timeframe 4h (14400000 ms)`, ""},
		{"4h explicit match loads", withOpenParams(dualThrustHLStrategy("hl-dt", dt, "ETH", "4h", "--mode=paper"),
			map[string]interface{}{"expected_interval_ms": 14400000, "lookback_sessions": 4}), "", `"expected_interval_ms":14400000`},
		{"hourly explicit 4h value refused", withOpenParams(dualThrustHLStrategy("hl-dt", dt, "ETH", "1h", "--mode=paper"),
			map[string]interface{}{"expected_interval_ms": 14400000}), "explicit expected_interval_ms=14400000 does not match timeframe 1h (3600000 ms)", ""},
		{"4h close fallback default refused", withClose(dualThrustHLStrategy("hl-dt", "breakout", "ETH", "4h", "--mode=paper")),
			`close strategy "dual_thrust": registry default expected_interval_ms=3600000 does not match timeframe 4h`, ""},
		{"hl manual 4h default refused", manual, `strategy hl-dt-manual: close strategy "dual_thrust"`, ""},
		{"hl manual with matching interval refused for closed bars", manualMatched,
			`strategy hl-dt-manual: close strategy "dual_thrust" decides only on closed candles`, ""},
		{"30m default lookback refused", withOpenParams(dualThrustHLStrategy("hl-dt", dt, "ETH", "30m", "--mode=paper"),
			map[string]interface{}{"expected_interval_ms": 1800000}),
			"open strategy dual_thrust params: (lookback_sessions + 1) x bars per session = (4 + 1) x 48 = 240 exceeds the 200-bar check window", ""},
		{"30m lookback 3 loads", withOpenParams(dualThrustHLStrategy("hl-dt", dt, "ETH", "30m", "--mode=paper"),
			map[string]interface{}{"expected_interval_ms": 1800000, "lookback_sessions": 3}), "", `"lookback_sessions":3`},
		{"4h off-grid session hour refused", withOpenParams(dualThrustHLStrategy("hl-dt", dt, "ETH", "4h", "--mode=paper"),
			map[string]interface{}{"expected_interval_ms": 14400000, "session_start_hour": 1}),
			"open strategy dual_thrust params: session_start_hour=1 is not on the 14400000 ms candle grid", ""},
		{"4h on-grid session hour loads", withOpenParams(dualThrustHLStrategy("hl-dt", dt, "ETH", "4h", "--mode=paper"),
			map[string]interface{}{"expected_interval_ms": 14400000, "session_start_hour": 4}), "", `"session_start_hour":4`},
		{"30m close fallback default lookback refused", withCloseParams(dualThrustHLStrategy("hl-dt", "breakout", "ETH", "30m", "--mode=paper"),
			map[string]interface{}{"expected_interval_ms": 1800000}),
			"close strategy dual_thrust params: (lookback_sessions + 1) x bars per session = (4 + 1) x 48 = 240 exceeds the 200-bar check window", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeNoEdgeConfig(t, CurrentConfigVersion, tc.sc)
			cfg, loadErr := LoadConfig(path)
			_, readErr := LoadConfigReadOnly(path)
			for loader, err := range map[string]error{"load": loadErr, "readonly": readErr} {
				if (err == nil) != (tc.wantErr == "") {
					t.Fatalf("%s: err=%v, want refusal %q", loader, err, tc.wantErr)
				}
				if err != nil && !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("%s: error missing %q: %v", loader, tc.wantErr, err)
				}
			}
			if tc.wantErr != "" {
				return
			}
			if portfolioScopeFor(cfg.Strategies[0]) != ScopePaper {
				t.Fatalf("scope = %q, want paper", portfolioScopeFor(cfg.Strategies[0]))
			}
			if tc.wantRef != "" {
				args, err := buildStrategyRefsArg(cfg.Strategies[0], "", false)
				if err != nil || len(args) != 2 || !strings.Contains(args[1], tc.wantRef) {
					t.Fatalf("strategy refs = %v, %v; want %s", args, err, tc.wantRef)
				}
			}
		})
	}
}

func dualThrustLegacyFixture() []map[string]interface{} {
	openLive := dualThrustHLStrategy("hl-dt-open", "dual_thrust", "BTC", "1h", "--mode=live")
	openLive["allow_deprecated"] = true
	closeOnly := dualThrustHLStrategy("hl-dt-close", "breakout", "ETH", "1h", "--mode", "live")
	closeOnly["close_strategy"] = map[string]interface{}{"name": "dual_thrust"}
	mixed := dualThrustHLStrategy("hl-dt-mixed", "rsi", "SOL", "1h", "--mode=live")
	mixed["close_strategy"] = map[string]interface{}{"name": "dual_thrust"}
	explicitFalse := dualThrustHLStrategy("hl-dt-false", "dual_thrust", "AVAX", "1h", "--mode=live")
	explicitFalse["allow_no_edge"] = false
	preAck := dualThrustHLStrategy("hl-dt-ack", "dual_thrust", "LINK", "1h", "--mode=live")
	preAck["allow_no_edge"] = true
	return []map[string]interface{}{
		openLive,
		closeOnly,
		mixed,
		explicitFalse,
		preAck,
		dualThrustHLStrategy("hl-dt-paper", "dual_thrust", "XRP", "1h", "--mode=paper"),
		noEdgeHLStrategy("hl-control", "macd", "BTC", "4h", "--mode=live"),
	}
}

func TestV20NoEdgeMigrationNeverGrantsDualThrust(t *testing.T) {
	setNoEdgeLiveCredentials(t)
	if _, eligible := v20StampEligibleStrategies["dual_thrust"]; eligible {
		t.Fatal("dual_thrust must never be v20 stamp-eligible")
	}
	refusedIDs := []string{"hl-dt-close", "hl-dt-false", "hl-dt-mixed", "hl-dt-open"}
	for _, version := range []int{0, 19} {
		t.Run(map[int]string{0: "versionless", 19: "v19"}[version], func(t *testing.T) {
			path := writeNoEdgeConfig(t, version, dualThrustLegacyFixture()...)
			original, _ := os.ReadFile(path)
			if cfg, err := LoadConfigReadOnly(path); err == nil {
				for _, sc := range cfg.Strategies {
					if slices.Contains(refusedIDs, sc.ID) && sc.AllowNoEdgeAcknowledged() {
						t.Fatalf("readonly: %s gained an in-memory acknowledgement", sc.ID)
					}
				}
			}
			if after, _ := os.ReadFile(path); !bytes.Equal(original, after) {
				t.Fatal("the read-only loader changed source bytes")
			}
			_, err := LoadConfig(path)
			if err == nil {
				t.Fatal("unacknowledged live dual_thrust references must stay refused after migration")
			}
			for _, id := range refusedIDs {
				if !strings.Contains(err.Error(), "strategy "+id+":") {
					t.Fatalf("%s should be refused after migration: %v", id, err)
				}
			}
			for _, id := range []string{"hl-dt-ack", "hl-dt-paper", "hl-control"} {
				if strings.Contains(err.Error(), "strategy "+id+":") {
					t.Fatalf("%s should be admitted: %v", id, err)
				}
			}
			raw := rawStrategiesByID(t, path)
			for _, id := range []string{"hl-dt-open", "hl-dt-close", "hl-dt-mixed", "hl-dt-paper"} {
				if v, has := raw[id]["allow_no_edge"]; has {
					t.Fatalf("%s gained allow_no_edge=%v from the legacy migration", id, v)
				}
			}
			if raw["hl-dt-false"]["allow_no_edge"] != false || raw["hl-dt-ack"]["allow_no_edge"] != true {
				t.Fatal("existing explicit acknowledgement values must be preserved")
			}
			if raw["hl-control"]["allow_no_edge"] != true {
				t.Fatal("the legacy migration did not run: the non-candidate control was not stamped")
			}
		})
	}
	raw := map[string]interface{}{"strategies": []interface{}{}}
	for _, sc := range dualThrustLegacyFixture() {
		blob, _ := json.Marshal(sc)
		var generic map[string]interface{}
		_ = json.Unmarshal(blob, &generic)
		raw["strategies"] = append(raw["strategies"].([]interface{}), generic)
	}
	report, err := migrateV20NoEdge(raw, 19)
	if err != nil {
		t.Fatal(err)
	}
	if got := stampedIDs(report); !reflect.DeepEqual(got, []string{"hl-control"}) {
		t.Fatalf("stamped = %v, want only the non-candidate control", got)
	}
	if want := []string{"hl-dt-close", "hl-dt-mixed", "hl-dt-open"}; !reflect.DeepEqual(report.VetoedResearch, want) {
		t.Fatalf("vetoed = %v, want %v", report.VetoedResearch, want)
	}
}

func TestRegressionChannelReversionLegacyConfigsGainNoGeneratedAcknowledgement(t *testing.T) {
	setNoEdgeLiveCredentials(t)
	rcrOpen := noEdgeHLStrategy("hl-rcr", "regression_channel_reversion", "BTC", "4h", "--mode=live")
	eligibleOpenRCRClose := noEdgeHLStrategy("hl-macd-rcr", "macd", "ETH", "4h", "--mode=live")
	eligibleOpenRCRClose["close_strategy"] = map[string]interface{}{"name": "regression_channel_reversion"}
	rcrOpenEligibleClose := noEdgeHLStrategy("hl-rcr-adx", "regression_channel_reversion", "SOL", "4h", "--mode=live")
	rcrOpenEligibleClose["close_strategy"] = map[string]interface{}{"name": "adx_trend"}
	control := noEdgeHLStrategy("hl-macd", "macd", "BTC", "1h", "--mode=live")

	report, err := migrateV20NoEdge(readRawConfig(t, writeNoEdgeConfig(t, 19, rcrOpen)), 19)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Stamped) != 0 || !reflect.DeepEqual(report.VetoedResearch, []string{"hl-rcr"}) {
		t.Fatalf("candidate alone: stamped=%v vetoed=%v", stampedIDs(report), report.VetoedResearch)
	}

	path := writeNoEdgeConfig(t, 19, rcrOpen, eligibleOpenRCRClose, rcrOpenEligibleClose, control)
	report, err = migrateV20NoEdge(readRawConfig(t, path), 19)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"hl-macd-rcr", "hl-rcr", "hl-rcr-adx"}; !reflect.DeepEqual(report.VetoedResearch, want) {
		t.Fatalf("vetoed research = %v, want %v", report.VetoedResearch, want)
	}
	if got := stampedIDs(report); !reflect.DeepEqual(got, []string{"hl-macd"}) {
		t.Fatalf("stamped = %v, want only the eligible control hl-macd", got)
	}

	_, err = LoadConfig(path)
	if err == nil {
		t.Fatal("legacy explicit-live candidate references must stay refused after migration")
	}
	for _, id := range []string{"hl-rcr", "hl-macd-rcr", "hl-rcr-adx"} {
		if !strings.Contains(err.Error(), "strategy "+id+":") {
			t.Fatalf("%s should be refused after migration: %v", id, err)
		}
	}
	if strings.Contains(err.Error(), "strategy hl-macd:") {
		t.Fatalf("the stamped eligible control must load: %v", err)
	}
	raw := rawStrategiesByID(t, path)
	for _, id := range []string{"hl-rcr", "hl-macd-rcr", "hl-rcr-adx"} {
		if _, has := raw[id]["allow_no_edge"]; has {
			t.Fatalf("%s names regression_channel_reversion and gained allow_no_edge=%v", id, raw[id]["allow_no_edge"])
		}
	}
	if raw["hl-macd"]["allow_no_edge"] != true {
		t.Fatalf("eligible control allow_no_edge = %v, want true", raw["hl-macd"]["allow_no_edge"])
	}
}

func rcrStudyStrategy(id string, params map[string]interface{}, modeArgs ...string) map[string]interface{} {
	sc := noEdgeHLStrategy(id, append([]string{"regression_channel_reversion", "BTC", "4h"}, modeArgs...)...)
	delete(sc, "stop_loss_pct")
	sc["direction"] = DirectionBoth
	sc["stop_loss_atr_mult"] = 1.0
	sc["open_strategy"] = map[string]interface{}{"name": "regression_channel_reversion", "params": params}
	sc["close_strategy"] = map[string]interface{}{
		"name":   "tiered_tp_pct",
		"params": map[string]interface{}{"tp_tiers": []interface{}{map[string]interface{}{"profit_pct": 0.03, "close_fraction": 1.0}}},
	}
	return sc
}

func TestRegressionChannelReversionAddAdmissionAndParamPreflightThroughRealLoaders(t *testing.T) {
	setNoEdgeLiveCredentials(t)
	id, entry, err := buildAddStrategyEntry("regression_channel_reversion", "hyperliquid", "BTC")
	if err != nil {
		t.Fatal(err)
	}
	if id != "hl-rcr-btc" {
		t.Fatalf("generated id = %q, want hl-rcr-btc", id)
	}
	owners := 0
	for name, short := range knownShortNames {
		if short == "rcr" {
			owners++
			if name != "regression_channel_reversion" {
				t.Fatalf("short name rcr also owned by %s", name)
			}
		}
	}
	if owners != 1 {
		t.Fatalf("short name rcr owners = %d", owners)
	}
	if !isBidirectionalPerpsStrategy("regression_channel_reversion") {
		t.Fatal("regression_channel_reversion must be in the bidirectional perps set")
	}
	if _, _, err := buildAddStrategyEntry("regression_channel_reversion", "binanceus", "BTC"); err == nil {
		t.Fatal("regression_channel_reversion is futures-only but binanceus add accepted it")
	}
	var added map[string]interface{}
	if err := json.Unmarshal(entry, &added); err != nil {
		t.Fatal(err)
	}
	path := writeNoEdgeConfig(t, CurrentConfigVersion, added)
	cfg, loadErr := LoadConfig(path)
	if _, probeErr := LoadConfigForProbe(path); loadErr != nil || probeErr != nil {
		t.Fatalf("generated entry must load: load=%v probe=%v", loadErr, probeErr)
	}
	got := cfg.Strategies[0]
	if got.Direction != DirectionBoth || portfolioScopeFor(got) != ScopePaper || edgeGateModeForStrategy(got).Kind != edgeGateModePaper || got.AllowNoEdgeAcknowledged() {
		t.Fatalf("generated entry direction=%q scope=%q gate=%q ack=%v", got.Direction, portfolioScopeFor(got), edgeGateModeForStrategy(got).Kind, got.AllowNoEdgeAcknowledged())
	}
	ev, ok := noEdgeEvidenceFor("regression_channel_reversion")
	if !ok || ev.Ref != "backtest/candidates/regression_channel_reversion_1644/REPORT.md" {
		t.Fatalf("no-edge evidence = %+v ok=%v", ev, ok)
	}
	if tag := edgeStatusSummaryTag(got); tag != "edge=no_edge:"+ev.Source+"(paper)" {
		t.Fatalf("summary tag = %q", tag)
	}

	seed := map[string]interface{}{"fit_window": 50.0, "band_multiplier": 2.0, "slope_ceiling": 0.1, "min_residual_std": 0.0}
	study := rcrStudyStrategy("hl-rcr-study", seed, "--mode=paper")
	cfg, err = LoadConfig(writeNoEdgeConfig(t, CurrentConfigVersion, study))
	if err != nil {
		t.Fatalf("study config must load: %v", err)
	}
	if sc := cfg.Strategies[0]; sc.CloseStrategy == nil || sc.CloseStrategy.Name != "tiered_tp_pct" || sc.StopLossATRMult == nil || *sc.StopLossATRMult != 1.0 {
		t.Fatalf("study close/stop owners = %+v / %v", sc.CloseStrategy, sc.StopLossATRMult)
	}

	ack := func(sc map[string]interface{}) map[string]interface{} {
		sc["allow_no_edge"] = true
		return sc
	}
	fallback := func(params map[string]interface{}, modeArgs ...string) map[string]interface{} {
		sc := noEdgeHLStrategy("hl-rcr-fb", append([]string{"breakout", "BTC", "4h"}, modeArgs...)...)
		sc["close_strategy"] = map[string]interface{}{"name": "regression_channel_reversion", "params": params}
		return sc
	}
	legacyArgs := rcrStudyStrategy("hl-rcr-legacy", seed, "--mode=paper")
	legacyArgs["args"] = []string{"regression_channel_reversion", "BTC", "4h", "--mode=paper", "--params", `{"fit_window":true}`}
	cases := []struct {
		name    string
		sc      map[string]interface{}
		wantErr []string
	}{
		{"live refused without acknowledgement", rcrStudyStrategy("hl-r", seed, "--mode=live"), []string{`open strategy "regression_channel_reversion"`, "backtest/candidates/regression_channel_reversion_1644/REPORT.md", "explicit live mode"}},
		{"live acknowledged", ack(rcrStudyStrategy("hl-r", seed, "--mode=live")), nil},
		{"missing mode refused", rcrStudyStrategy("hl-r", seed), []string{"missing --mode"}},
		{"repeated mode refused", rcrStudyStrategy("hl-r", seed, "--mode=paper", "--mode=paper"), []string{"regression_channel_reversion"}},
		{"invalid acknowledgement type", func() map[string]interface{} {
			sc := rcrStudyStrategy("hl-r", seed, "--mode=live")
			sc["allow_no_edge"] = "yes"
			return sc
		}(), []string{"allow_no_edge"}},
		{"boundary low", rcrStudyStrategy("hl-r", map[string]interface{}{"fit_window": 2.0, "band_multiplier": 1e-9, "slope_ceiling": 0.0, "min_residual_std": 0.0}, "--mode=paper"), nil},
		{"boundary high", rcrStudyStrategy("hl-r", map[string]interface{}{"fit_window": 197.0, "band_multiplier": 3.0, "slope_ceiling": 0.2, "min_residual_std": 1.0}, "--mode=paper"), nil},
		{"fit window below range", rcrStudyStrategy("hl-r", map[string]interface{}{"fit_window": 1.0}, "--mode=paper"), []string{"open strategy regression_channel_reversion params", "fit_window must be in [2, 197]"}},
		{"fit window above range", rcrStudyStrategy("hl-r", map[string]interface{}{"fit_window": 198.0}, "--mode=paper"), []string{"fit_window must be in [2, 197]"}},
		{"fractional fit window", rcrStudyStrategy("hl-r", map[string]interface{}{"fit_window": 2.5}, "--mode=paper"), []string{"fit_window must be an integer"}},
		{"boolean fit window", rcrStudyStrategy("hl-r", map[string]interface{}{"fit_window": true}, "--mode=paper"), []string{"fit_window must be a number"}},
		{"string fit window", rcrStudyStrategy("hl-r", map[string]interface{}{"fit_window": "50"}, "--mode=paper"), []string{"fit_window must be a number"}},
		{"zero band multiplier", rcrStudyStrategy("hl-r", map[string]interface{}{"band_multiplier": 0.0}, "--mode=paper"), []string{"band_multiplier must be finite and greater than 0"}},
		{"null band multiplier", rcrStudyStrategy("hl-r", map[string]interface{}{"band_multiplier": nil}, "--mode=paper"), []string{"band_multiplier must be a number"}},
		{"negative slope ceiling", rcrStudyStrategy("hl-r", map[string]interface{}{"slope_ceiling": -0.01}, "--mode=paper"), []string{"slope_ceiling must be finite and at least 0"}},
		{"negative deviation floor", rcrStudyStrategy("hl-r", map[string]interface{}{"min_residual_std": -1.0}, "--mode=paper"), []string{"min_residual_std must be finite and at least 0"}},
		{"close fallback params checked", fallback(map[string]interface{}{"fit_window": 0.0}, "--mode=paper"), []string{"close strategy regression_channel_reversion params", "fit_window must be in"}},
		{"close fallback valid in paper", fallback(map[string]interface{}{"fit_window": 20.0}, "--mode=paper"), nil},
		{"close fallback live refused", fallback(map[string]interface{}{"fit_window": 20.0}, "--mode=live"), []string{`close strategy "regression_channel_reversion"`, "explicit live mode"}},
		{"close fallback missing mode refused", fallback(map[string]interface{}{"fit_window": 20.0}), []string{"missing --mode"}},
		{"ignored legacy params are not active inputs", legacyArgs, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeNoEdgeConfig(t, CurrentConfigVersion, tc.sc)
			_, loadErr := LoadConfig(path)
			_, probeErr := LoadConfigForProbe(path)
			if len(tc.wantErr) == 0 {
				if loadErr != nil || probeErr != nil {
					t.Fatalf("unexpected refusal: load=%v probe=%v", loadErr, probeErr)
				}
				return
			}
			if loadErr == nil || probeErr == nil {
				t.Fatalf("expected refusal containing %q: load=%v probe=%v", tc.wantErr, loadErr, probeErr)
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(loadErr.Error(), want) || !strings.Contains(probeErr.Error(), want) {
					t.Fatalf("errors missing %q:\nload:  %v\nprobe: %v", want, loadErr, probeErr)
				}
			}
		})
	}
}

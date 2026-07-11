package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// minimalConfigJSON is a current-version config that LoadConfigForProbe accepts,
// used by the round-trip tests. One BinanceUS spot strategy keeps it credential-free.
const minimalConfigJSON = `{
  "config_version": 15,
  "interval_seconds": 300,
  "log_dir": "logs",
  "db_file": "scheduler/state.db",
  "auto_update": "off",
  "discord": {"enabled": false, "token": "discord-secret", "channels": {}, "leaderboard_top_n": 5},
  "telegram": {"enabled": false, "bot_token": "tg-secret", "channels": {}},
  "strategies": [
    {"id": "sma-btc", "type": "spot", "platform": "binanceus", "script": "shared_scripts/check_strategy.py", "args": ["sma_crossover", "BTC/USDT", "1h"], "capital": 1000, "max_drawdown_pct": 5},
    {"id": "hl-momentum-eth", "type": "perps", "platform": "hyperliquid", "script": "shared_scripts/check_hyperliquid.py", "args": ["momentum", "ETH", "1h", "--mode=paper"], "capital": 100, "max_drawdown_pct": 10, "leverage": 1, "direction": "long", "margin_mode": "isolated"}
  ]
}`

func rootFromJSON(t *testing.T, s string) map[string]json.RawMessage {
	t.Helper()
	var root map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &root); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	return root
}

func TestRedactConfigForDisplay(t *testing.T) {
	out, err := redactConfigForDisplay([]byte(minimalConfigJSON))
	if err != nil {
		t.Fatalf("redact: %v", err)
	}
	if strings.Contains(out, "discord-secret") || strings.Contains(out, "tg-secret") {
		t.Errorf("secret leaked into redacted output:\n%s", out)
	}
	if strings.Count(out, configSecretReplacement) != 2 {
		t.Errorf("expected 2 redactions, got: %s", out)
	}
	// Non-secret fields preserved.
	if !strings.Contains(out, "\"interval_seconds\": 300") {
		t.Errorf("non-secret field not preserved: %s", out)
	}
}

func TestRedactConfigForDisplayEmptyTokenUntouched(t *testing.T) {
	// An empty token must not be turned into a redaction placeholder.
	out, err := redactConfigForDisplay([]byte(`{"discord":{"enabled":false,"token":"","channels":{}}}`))
	if err != nil {
		t.Fatalf("redact: %v", err)
	}
	if strings.Contains(out, configSecretReplacement) {
		t.Errorf("empty token should not be redacted, got: %s", out)
	}
}

func TestBuildAddStrategyEntryHyperliquid(t *testing.T) {
	id, raw, err := buildAddStrategyEntry("momentum", "hyperliquid", "eth")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if id != "hl-momentum-eth" {
		t.Errorf("unexpected id: %q", id)
	}
	var obj map[string]interface{}
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("unmarshal entry: %v", err)
	}
	if obj["type"] != "perps" || obj["platform"] != "hyperliquid" {
		t.Errorf("unexpected type/platform: %v", obj)
	}
	args, _ := json.Marshal(obj["args"])
	if !strings.Contains(string(args), "--mode=paper") {
		t.Errorf("new perps strategy must be paper mode, got args: %s", args)
	}
	if !strings.Contains(string(args), "\"ETH\"") {
		t.Errorf("asset should be uppercased in args: %s", args)
	}
}

func TestBuildAddStrategyEntryBidirectionalDirection(t *testing.T) {
	_, raw, err := buildAddStrategyEntry("triple_ema_bidir", "hyperliquid", "btc")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var obj map[string]interface{}
	_ = json.Unmarshal(raw, &obj)
	if obj["direction"] != DirectionBoth {
		t.Errorf("bidirectional strategy should get direction=both, got %v", obj["direction"])
	}
}

func TestBuildAddStrategyEntrySpot(t *testing.T) {
	id, raw, err := buildAddStrategyEntry("sma_crossover", "binanceus", "btc")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if id != "sma-btc" {
		t.Errorf("unexpected id: %q", id)
	}
	var obj map[string]interface{}
	_ = json.Unmarshal(raw, &obj)
	if obj["type"] != "spot" {
		t.Errorf("expected spot type, got %v", obj["type"])
	}
	args, _ := json.Marshal(obj["args"])
	if !strings.Contains(string(args), "BTC/USDT") {
		t.Errorf("spot args should carry BTC/USDT symbol: %s", args)
	}
}

func TestBuildAddStrategyEntryRejections(t *testing.T) {
	if _, _, err := buildAddStrategyEntry("not_a_real_strategy", "hyperliquid", "eth"); err == nil {
		t.Error("expected error for unknown strategy name")
	}
	if _, _, err := buildAddStrategyEntry("momentum", "deribit", "eth"); err == nil {
		t.Error("expected error for unsupported platform")
	}
	if _, _, err := buildAddStrategyEntry("momentum", "hyperliquid", "ETH/USDT"); err == nil {
		t.Error("expected error for non-plain asset token")
	}
	if _, _, err := buildAddStrategyEntry("", "hyperliquid", "eth"); err == nil {
		t.Error("expected error for empty name")
	}
}

func TestAddStrategyToRoot(t *testing.T) {
	root := rootFromJSON(t, minimalConfigJSON)
	id, err := addStrategyToRoot(root, "rsi", "hyperliquid", "sol")
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if id != "hl-rsi-sol" {
		t.Errorf("unexpected id: %q", id)
	}
	list, _ := configStrategies(root)
	if len(list) != 3 {
		t.Errorf("expected 3 strategies after add, got %d", len(list))
	}
	// Duplicate add is rejected.
	if _, err := addStrategyToRoot(root, "rsi", "hyperliquid", "sol"); err == nil {
		t.Error("expected duplicate-id error on second add")
	}
}

func TestRemoveStrategyFromRoot(t *testing.T) {
	root := rootFromJSON(t, minimalConfigJSON)
	if err := removeStrategyFromRoot(root, "sma-btc"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	list, _ := configStrategies(root)
	if len(list) != 1 || strategyRawID(list[0]) != "hl-momentum-eth" {
		t.Errorf("unexpected strategies after remove: %d", len(list))
	}
	if err := removeStrategyFromRoot(root, "does-not-exist"); err == nil {
		t.Error("expected not-found error")
	}
	// Removing the last remaining strategy is refused.
	if err := removeStrategyFromRoot(root, "hl-momentum-eth"); err == nil {
		t.Error("expected refusal to remove the only strategy")
	}
}

func TestFlipStrategyToLive(t *testing.T) {
	root := rootFromJSON(t, minimalConfigJSON)
	before, after, err := flipStrategyToLive(root, "hl-momentum-eth")
	if err != nil {
		t.Fatalf("flip: %v", err)
	}
	if !strings.Contains(strings.Join(before, " "), "--mode=paper") {
		t.Errorf("expected paper in before: %v", before)
	}
	if !strings.Contains(strings.Join(after, " "), "--mode=live") || strings.Contains(strings.Join(after, " "), "--mode=paper") {
		t.Errorf("expected live (not paper) in after: %v", after)
	}
	// Persisted in root.
	list, _ := configStrategies(root)
	var found bool
	for _, raw := range list {
		if strategyRawID(raw) == "hl-momentum-eth" {
			if strings.Contains(string(raw), "--mode=live") {
				found = true
			}
		}
	}
	if !found {
		t.Error("flip not persisted into root")
	}
	// Second flip errors: already live.
	if _, _, err := flipStrategyToLive(root, "hl-momentum-eth"); err == nil {
		t.Error("expected already-live error")
	}
	// Spot strategy has no --mode arg → error.
	if _, _, err := flipStrategyToLive(root, "sma-btc"); err == nil {
		t.Error("expected no-mode error for spot strategy")
	}
	// Missing strategy → error.
	if _, _, err := flipStrategyToLive(root, "ghost"); err == nil {
		t.Error("expected not-found error")
	}
}

func TestClassifyConfigSetKey(t *testing.T) {
	cases := []struct {
		key, kind, id, field string
	}{
		{"interval_seconds", "top", "", ""},
		{"discord.leaderboard_top_n", "top", "", ""},
		{"strategies.hl-rmc-eth.leverage", "strategy", "hl-rmc-eth", "leverage"},
		{"strategies.foo", "strategy", "", ""},
		{"strategies.", "strategy", "", ""},
	}
	for _, c := range cases {
		kind, id, field := classifyConfigSetKey(c.key)
		if kind != c.kind || id != c.id || field != c.field {
			t.Errorf("classify(%q) = (%q,%q,%q), want (%q,%q,%q)", c.key, kind, id, field, c.kind, c.id, c.field)
		}
	}
}

func TestBuildTunerOverride(t *testing.T) {
	// interval_seconds → integer.
	ov, err := buildTunerOverride("interval_seconds", "300")
	if err != nil || string(ov["interval_seconds"]) != "300" {
		t.Errorf("interval_seconds override = %v, err %v", ov, err)
	}
	// direction → JSON string, lowercased.
	ov, err = buildTunerOverride("direction", "Long")
	if err != nil || string(ov["direction"]) != `"long"` {
		t.Errorf("direction override = %v, err %v", ov, err)
	}
	// invert_signal → bool.
	ov, _ = buildTunerOverride("invert_signal", "true")
	if string(ov["invert_signal"]) != "true" {
		t.Errorf("invert_signal override = %v", ov)
	}
	// stop_loss_pct null clears.
	ov, _ = buildTunerOverride("stop_loss_pct", "null")
	if string(ov["stop_loss_pct"]) != "null" {
		t.Errorf("stop_loss_pct null override = %v", ov)
	}
	ov, _ = buildTunerOverride("stop_loss_atr_mult", "1.5")
	if string(ov["stop_loss_atr_mult"]) != "1.5" {
		t.Errorf("stop_loss_atr_mult override = %v", ov)
	}
	// Rejections.
	for _, bad := range []struct{ field, value string }{
		{"direction", "sideways"},
		{"leverage", "0"},
		{"interval_seconds", "0"},
		{"invert_signal", "maybe"},
		{"unknown_field", "1"},
	} {
		if _, err := buildTunerOverride(bad.field, bad.value); err == nil {
			t.Errorf("expected error for %s=%q", bad.field, bad.value)
		}
	}
}

func TestApplyTopLevelConfigSet(t *testing.T) {
	root := rootFromJSON(t, minimalConfigJSON)

	restart, summary, err := applyTopLevelConfigSet(root, "interval_seconds", "600")
	if err != nil || restart || !strings.Contains(summary, "600") {
		t.Errorf("interval_seconds: restart=%v summary=%q err=%v", restart, summary, err)
	}
	if string(root["interval_seconds"]) != "600" {
		t.Errorf("interval_seconds not patched: %s", root["interval_seconds"])
	}

	restart, _, err = applyTopLevelConfigSet(root, "auto_update", "daily")
	if err != nil || !restart {
		t.Errorf("auto_update should require restart: restart=%v err=%v", restart, err)
	}
	if _, _, err := applyTopLevelConfigSet(root, "auto_update", "weekly"); err == nil {
		t.Error("expected error for invalid auto_update value")
	}

	restart, _, err = applyTopLevelConfigSet(root, "leaderboard_post_time", "09:30")
	if err != nil || !restart {
		t.Errorf("leaderboard_post_time: restart=%v err=%v", restart, err)
	}
	if _, _, err := applyTopLevelConfigSet(root, "leaderboard_post_time", "25:99"); err == nil {
		t.Error("expected error for invalid leaderboard_post_time")
	}

	restart, _, err = applyTopLevelConfigSet(root, "discord.leaderboard_top_n", "10")
	if err != nil || restart {
		t.Errorf("discord.leaderboard_top_n: restart=%v err=%v", restart, err)
	}
	var discord map[string]json.RawMessage
	_ = json.Unmarshal(root["discord"], &discord)
	if string(discord["leaderboard_top_n"]) != "10" {
		t.Errorf("nested discord.leaderboard_top_n not patched: %s", discord["leaderboard_top_n"])
	}

	if _, _, err := applyTopLevelConfigSet(root, "definitely_not_a_key", "1"); err == nil {
		t.Error("expected error for unsupported key")
	}
}

func TestConfirmYes(t *testing.T) {
	for _, ok := range []string{"yes", "Y", " confirm ", "OK"} {
		if !confirmYes(ok) {
			t.Errorf("expected %q to confirm", ok)
		}
	}
	for _, no := range []string{"no", "n", "", "cancel", "yeah nah"} {
		if confirmYes(no) {
			t.Errorf("expected %q to NOT confirm", no)
		}
	}
}

func TestPlatformSetupGuide(t *testing.T) {
	guide, err := platformSetupGuide("hyperliquid")
	if err != nil {
		t.Fatalf("guide: %v", err)
	}
	if !strings.Contains(guide, "/opt/go-trader/.env") || !strings.Contains(guide, "/go-trader-add-strategy") {
		t.Errorf("hyperliquid guide missing setup steps: %s", guide)
	}
	// Non-addable platform still produces a guide but points to the wizard.
	guide, err = platformSetupGuide("deribit")
	if err != nil {
		t.Fatalf("guide: %v", err)
	}
	if !strings.Contains(guide, "init wizard") {
		t.Errorf("deribit guide should point to wizard: %s", guide)
	}
	if _, err := platformSetupGuide("not-a-platform"); err == nil {
		t.Error("expected error for unknown platform")
	}
}

func TestWriteValidatedConfigRootRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(minimalConfigJSON), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	raw, _ := os.ReadFile(path)
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, _, err := applyTopLevelConfigSet(root, "interval_seconds", "777"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := writeValidatedConfigRoot(path, root); err != nil {
		t.Fatalf("writeValidatedConfigRoot: %v", err)
	}
	cfg, err := LoadConfigForProbe(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if cfg.IntervalSeconds != 777 {
		t.Errorf("expected interval 777 after write, got %d", cfg.IntervalSeconds)
	}
}

func TestAddStrategyRoundTripValidates(t *testing.T) {
	// A generated /add-strategy entry must pass the real config validator
	// (LoadConfigForProbe), not just the pure JSON shaping.
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(minimalConfigJSON), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	raw, _ := os.ReadFile(path)
	var root map[string]json.RawMessage
	_ = json.Unmarshal(raw, &root)
	id, err := addStrategyToRoot(root, "rsi", "hyperliquid", "sol")
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := writeValidatedConfigRoot(path, root); err != nil {
		t.Fatalf("generated strategy failed validation: %v", err)
	}
	cfg, err := LoadConfigForProbe(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	found := false
	for _, sc := range cfg.Strategies {
		if sc.ID == id {
			found = true
			if sc.Type != "perps" || sc.Platform != "hyperliquid" {
				t.Errorf("generated strategy has wrong type/platform: %+v", sc)
			}
		}
	}
	if !found {
		t.Errorf("generated strategy %q not present after reload", id)
	}
}

func TestWriteValidatedConfigRootRejectsInvalid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(minimalConfigJSON), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	raw, _ := os.ReadFile(path)
	var root map[string]json.RawMessage
	_ = json.Unmarshal(raw, &root)
	// Corrupt: interval_seconds must be an integer; a string breaks LoadConfig parse.
	root["interval_seconds"] = json.RawMessage(`"not-an-int"`)
	if err := writeValidatedConfigRoot(path, root); err == nil {
		t.Fatal("expected writeValidatedConfigRoot to reject an invalid config")
	}
	// Original file must be untouched (atomic rename never happened).
	after, _ := os.ReadFile(path)
	if string(after) != minimalConfigJSON {
		t.Errorf("original config was modified despite validation failure:\n%s", after)
	}
}

// paperToLiveFlatChecks exercises the shared StatusServer seam the Discord
// /paper-to-live handler delegates to (paperToLiveBlockedReason +
// executePaperToLive) without a live discordgo session.
func TestPaperToLiveFlatChecks(t *testing.T) {
	openPos := &StrategyState{Positions: map[string]*Position{"ETH": {Quantity: 1, AvgCost: 2000}}}

	// Open at pre-confirm → blocked before any write.
	ss, path, _ := newStructuralTestServer(t)
	ss.state.Strategies["hl-momentum-eth"] = openPos
	if reason := ss.paperToLiveBlockedReason("hl-momentum-eth", false); reason == "" || !strings.Contains(reason, "OPEN position") {
		t.Fatalf("pre-confirm blocked reason = %q, want OPEN-position refusal", reason)
	}

	// Flat → flips to live.
	delete(ss.state.Strategies, "hl-momentum-eth")
	msg, err := ss.executePaperToLive("hl-momentum-eth")
	if err != nil {
		t.Fatalf("flat executePaperToLive: %v", err)
	}
	if !strings.Contains(msg, "LIVE") {
		t.Fatalf("success message = %q", msg)
	}
	if raw := string(mustReadFile(t, path)); !strings.Contains(raw, "--mode=live") {
		t.Fatalf("config not flipped:\n%s", raw)
	}

	// Flat at execute start, opens before write → refused, args untouched.
	ssOpen, pathOpen, _ := newStructuralTestServer(t)
	ssOpen.state.Strategies["hl-momentum-eth"] = openPos
	_, err = ssOpen.executePaperToLive("hl-momentum-eth")
	if err == nil || !strings.Contains(err.Error(), "opened a position") {
		t.Fatalf("execute while open err = %v, want opened-a-position refusal", err)
	}
	if raw := string(mustReadFile(t, pathOpen)); strings.Contains(raw, "--mode=live") {
		t.Fatalf("refused execute must not flip args:\n%s", raw)
	}
}

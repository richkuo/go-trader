//go:build pyintegration

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestOrphanOptionPositionsCLIAndStartupIntegration(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	const orphanID = "rh-orphan-options"
	orphan := optionsMigrationOptionsStrategy(orphanID, "--mode=live")
	before := &Config{DBFile: dbPath, Strategies: []StrategyConfig{orphan}}
	store := openSplitStore(t, before)
	state := NewAppState()
	state.Strategies[orphanID] = &StrategyState{
		ID: orphanID, Type: "options", Platform: "robinhood", Cash: 1000, InitialCapital: 1000,
		Positions: map[string]*Position{},
		OptionPositions: map[string]*OptionPosition{
			"SPY-call": {ID: "SPY-call", Underlying: "SPY", OptionType: "call", Strike: 600, Expiry: "2026-12-18", Action: "sell", Quantity: 1, EntryPremium: 1.5, EntryPremiumUSD: 150, CurrentValueUSD: 150, OpenedAt: time.Unix(10, 0).UTC()},
			"SPY-put":  {ID: "SPY-put", Underlying: "SPY", OptionType: "put", Strike: 560, Expiry: "2026-12-18", Action: "buy", Quantity: 1, EntryPremium: 1.2, EntryPremiumUSD: 120, CurrentValueUSD: 120, OpenedAt: time.Unix(11, 0).UTC()},
		},
	}
	RecordTrade(state.Strategies[orphanID], Trade{StrategyID: orphanID, Symbol: "SPY-call", Side: "sell", Quantity: 1, Price: 150, Timestamp: time.Unix(10, 0).UTC()})
	if err := SaveStateWithStore(state, store); err != nil {
		t.Fatalf("save fixture: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close fixture: %v", err)
	}

	cfgPath := filepath.Join(dir, "config.json")
	blob, err := json.MarshalIndent(map[string]any{
		"config_version": CurrentConfigVersion, "interval_seconds": 60,
		"db_file": dbPath, "log_dir": filepath.Join(dir, "logs"),
		"strategies": []any{map[string]any{
			"id": "active-offline", "type": "spot", "platform": "binanceus", "script": "shared_scripts/check_strategy.py",
			"args": []string{"breakout", "BTC/USDT", "1h", "--mode=paper"}, "capital": 1000,
		}},
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	bin := pyintegrationBinary(t)
	cmd := exec.Command(bin, "storage-inspect", "--config", cfgPath)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("storage-inspect: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "ORPHAN "+orphanID+" (2 position(s))  ** holds positions **") {
		t.Fatalf("storage-inspect lacks the option orphan and marker:\n%s", out)
	}
	cmd = exec.Command(bin, "storage-inspect", "--json", "--config", cfgPath)
	cmd.Dir = root
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("storage-inspect --json: %v\n%s", err, out)
	}
	var si storageInspection
	if err := json.Unmarshal(out, &si); err != nil {
		t.Fatalf("decode inspection: %v\n%s", err, out)
	}
	if len(si.Files) != 1 || len(si.Files[0].Orphans) != 1 || si.Files[0].Orphans[0].PositionCount != 2 {
		t.Fatalf("inspection JSON = %+v, want one orphan with 2 positions", si)
	}
	if n := optionsMigrationCount(t, dbPath, "SELECT COUNT(*) FROM option_positions WHERE strategy_id = ?", orphanID); n != 2 {
		t.Fatalf("option rows after inspection = %d, want 2 (read-only)", n)
	}

	fixture := htfITWriteFixture(t, filepath.Join(dir, "market.json"), htfITFixture{HTF: "neutral", Spot: "up", Fut: "up"})
	cmd = exec.Command(bin, "--config", cfgPath, "--once", "--status-port", strconv.Itoa(htfITFreePort(t)))
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"PYTHONPATH="+filepath.Join(root, "shared_scripts", "testdata", "htf_offline"),
		"HTF_OFFLINE_FIXTURE="+fixture,
		"DISCORD_BOT_TOKEN=", "TELEGRAM_BOT_TOKEN=")
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("scheduler --once: %v\n%s", err, out)
	}
	log := string(out)
	for _, want := range []string{
		"Pruned stale strategy: " + orphanID + " [primary state file, 2 position(s)]",
		"[storage] WARN: stale strategy " + orphanID + " in the primary state file still holds 2 position(s); the next save of that file drops the row",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("startup output lacks %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, "network blocked by the htf offline fixture") {
		t.Fatalf("startup attempted an uncontrolled network call:\n%s", log)
	}
	if n := optionsMigrationCount(t, dbPath, "SELECT COUNT(*) FROM option_positions WHERE strategy_id = ?", orphanID); n != 0 {
		t.Errorf("option rows after startup save = %d, want 0", n)
	}
	if n := optionsMigrationCount(t, dbPath, "SELECT COUNT(*) FROM trades WHERE strategy_id = ?", orphanID); n != 1 {
		t.Errorf("trade rows after startup save = %d, want the historical row retained", n)
	}
}

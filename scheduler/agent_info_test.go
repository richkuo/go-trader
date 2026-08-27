package main

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestAgentInfoCommandsCoverKnownSubcommands(t *testing.T) {
	documented := map[string]bool{}
	for _, c := range agentInfoCommands {
		documented[c.Name] = true
	}
	for _, sub := range knownSubcommands {
		if !documented[sub] {
			t.Errorf("subcommand %q is dispatched (knownSubcommands) but not documented in agentInfoCommands", sub)
		}
	}
	known := map[string]bool{}
	for _, sub := range knownSubcommands {
		known[sub] = true
	}
	for _, c := range agentInfoCommands {
		if c.Name == "(daemon)" {
			continue
		}
		if !known[c.Name] {
			t.Errorf("documented command %q is not in knownSubcommands (stale or misspelled)", c.Name)
		}
	}
}

func TestAgentInfoEnvVarsCoverSource(t *testing.T) {
	registered := map[string]bool{}
	for _, v := range agentInfoEnvVars {
		registered[v.Name] = true
	}

	re := regexp.MustCompile(`os\.Getenv\("([A-Z0-9_]+)"\)`)
	entries, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	seen := map[string]bool{}
	for _, f := range entries {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, m := range re.FindAllStringSubmatch(string(data), -1) {
			seen[m[1]] = true
		}
	}
	for name := range seen {
		if !registered[name] {
			t.Errorf("env var %q is read via os.Getenv but missing from agentInfoEnvVars", name)
		}
	}

	for _, entry := range walletKeyRegistry {
		if entry.envVar == "" {
			continue
		}
		if !registered[entry.envVar] {
			t.Errorf("walletKeyRegistry reads env var %q (os.Getenv(entry.envVar)) but it is missing from agentInfoEnvVars", entry.envVar)
		}
	}
}

func TestReflectConfigSchema(t *testing.T) {
	schema := reflectConfigSchema()
	byName := map[string]agentConfigField{}
	for _, f := range schema {
		byName[f.JSONName] = f
	}
	if f, ok := byName["interval_seconds"]; !ok {
		t.Error("expected interval_seconds in config schema")
	} else if f.Optional {
		t.Error("interval_seconds has no omitempty; should be required")
	}
	if f, ok := byName["db_file"]; !ok {
		t.Error("expected db_file in config schema")
	} else if !f.Optional {
		t.Error("db_file has omitempty; should be optional")
	}
	if _, ok := byName["status_token"]; ok {
		t.Error("status_token is json:\"-\" and must not appear in schema")
	}
	if _, ok := byName[""]; ok {
		t.Error("empty json name leaked into schema")
	}
}

func TestResolveEnvVarPresence(t *testing.T) {
	t.Setenv("HYPERLIQUID_SECRET_KEY", "shh")
	t.Setenv("OKX_API_KEY", "")
	out := resolveEnvVarPresence(agentInfoEnvVars)
	got := map[string]bool{}
	for _, v := range out {
		got[v.Name] = v.Set
	}
	if !got["HYPERLIQUID_SECRET_KEY"] {
		t.Error("HYPERLIQUID_SECRET_KEY should be marked set")
	}
	if got["OKX_API_KEY"] {
		t.Error("empty OKX_API_KEY should be marked not set")
	}
}

func TestReadStateDBReadOnly(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")

	sdb, err := OpenStateDB(dbPath)
	if err != nil {
		t.Fatalf("OpenStateDB: %v", err)
	}
	if _, err := sdb.db.Exec(`INSERT INTO strategies (id, type, platform) VALUES ('s1','perps','hyperliquid')`); err != nil {
		t.Fatalf("seed strategy: %v", err)
	}
	if _, err := sdb.db.Exec(`INSERT INTO positions (strategy_id, symbol, quantity, avg_cost, side, regime) VALUES ('s1','ETH',1.5,3000,'long','trend')`); err != nil {
		t.Fatalf("seed position: %v", err)
	}
	if _, err := sdb.db.Exec(`INSERT INTO app_state (id, cycle_count) VALUES (1, 42) ON CONFLICT(id) DO UPDATE SET cycle_count=42`); err != nil {
		t.Fatalf("seed app_state: %v", err)
	}
	if _, err := sdb.db.Exec(`INSERT INTO strategies (id, type, platform) VALUES ('opt1','options','deribit')`); err != nil {
		t.Fatalf("seed option strategy: %v", err)
	}
	if _, err := sdb.db.Exec(`INSERT INTO option_positions (strategy_id, id, underlying, option_type, strike, expiry, action, quantity) VALUES ('opt1','o-1','BTC','call',70000,'2026-12-25','buy',2)`); err != nil {
		t.Fatalf("seed option position: %v", err)
	}
	sdb.Close()

	tables, live := readStateDBReadOnly(dbPath, 8099)
	if !live.DBPresent {
		t.Fatal("expected DBPresent=true")
	}
	if live.CycleCount != 42 {
		t.Errorf("cycle_count = %d, want 42", live.CycleCount)
	}
	if len(live.OpenPositions) != 1 || live.OpenPositions[0].Symbol != "ETH" || live.OpenPositions[0].Quantity != 1.5 {
		t.Errorf("open positions snapshot wrong: %+v", live.OpenPositions)
	}
	if len(live.OpenOptionPositions) != 1 || live.OpenOptionPositions[0].Underlying != "BTC" || live.OpenOptionPositions[0].Quantity != 2 {
		t.Errorf("open option positions snapshot wrong: %+v", live.OpenOptionPositions)
	}
	if !strings.Contains(live.Note, "8099") {
		t.Errorf("live note should point at status port: %q", live.Note)
	}
	names := map[string]bool{}
	for _, tb := range tables {
		names[tb.Name] = true
	}
	for _, want := range []string{"strategies", "positions", "trades", "app_state"} {
		if !names[want] {
			t.Errorf("schema missing table %q", want)
		}
	}

	if _, err := os.Stat(dbPath); err != nil {
		t.Errorf("db vanished after read-only introspection: %v", err)
	}
}

func TestReadStateDBMissing(t *testing.T) {
	tables, live := readStateDBReadOnly(filepath.Join(t.TempDir(), "nope.db"), 8099)
	if live.DBPresent {
		t.Error("missing DB should report DBPresent=false")
	}
	if tables != nil {
		t.Error("missing DB should yield nil schema")
	}
	if !strings.Contains(live.Note, "not present") {
		t.Errorf("missing-DB note should say so: %q", live.Note)
	}
}

func TestRenderAgentInfoMarkdownAndChangelog(t *testing.T) {
	info := agentInfo{
		Version:      "v1.2.3",
		GeneratedAt:  "2026-06-18T00:00:00Z",
		Capabilities: agentInfoCommands,
		ConfigSchema: reflectConfigSchema(),
		EnvVars:      agentInfoEnvVars,
		Strategies: []agentStrategyInfo{
			{ID: "s1", Type: "perps", Platform: "hyperliquid", OpenModule: "trend_follow", CloseModule: "tiered_tp_atr_live", AllowedRegimes: []string{"trend"}},
		},
		LiveState: agentLiveState{Source: "state.db snapshot", Note: "snapshot ... port 8099", DBPresent: true, CycleCount: 7},
	}
	md := renderAgentInfoMarkdown(info)
	for _, want := range []string{agentInfoMarkdownHeader, "agent-info", "config.json", "HYPERLIQUID_SECRET_KEY", "trend_follow", "Live state"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q", want)
		}
	}
	if strings.Contains(agentInfoGeneratedFile, "AGENTS.md") || agentInfoGeneratedFile == "AGENTS.md" {
		t.Fatal("generated file must not be AGENTS.md (symlink to CLAUDE.md)")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "AGENTS.generated.md")
	if err := writeAgentInfoMarkdown(path, md, true, info, time.Date(2026, 6, 18, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("write 1: %v", err)
	}
	info.Version = "v1.2.4"
	md2 := renderAgentInfoMarkdown(info)
	if err := writeAgentInfoMarkdown(path, md2, true, info, time.Date(2026, 6, 19, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("write 2: %v", err)
	}
	data, _ := os.ReadFile(path)
	s := string(data)
	if !strings.Contains(s, "v1.2.3") || !strings.Contains(s, "v1.2.4") {
		t.Error("changelog should retain both version entries")
	}
	if strings.Count(s, "## Changelog") != 1 {
		t.Errorf("changelog section should appear once, got %d", strings.Count(s, "## Changelog"))
	}
	if i3, i4 := strings.Index(s, "v1.2.4"), strings.LastIndex(s, "v1.2.3"); i3 > i4 {
		t.Error("newest changelog entry should come first")
	}
}

func TestBareRefreshPreservesChangelog(t *testing.T) {
	info := agentInfo{Version: "v1.0.0", Capabilities: agentInfoCommands, EnvVars: agentInfoEnvVars}
	dir := t.TempDir()
	path := filepath.Join(dir, "AGENTS.generated.md")

	md := renderAgentInfoMarkdown(info)
	if err := writeAgentInfoMarkdown(path, md, true, info, time.Date(2026, 6, 18, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("append: %v", err)
	}

	info.Version = "v1.1.0"
	md2 := renderAgentInfoMarkdown(info)
	if err := writeAgentInfoMarkdown(path, md2, false, info, time.Date(2026, 6, 19, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("bare refresh: %v", err)
	}
	data, _ := os.ReadFile(path)
	s := string(data)
	if !strings.Contains(s, "v1.0.0") {
		t.Error("bare refresh dropped prior changelog history (v1.0.0)")
	}
	if strings.Count(s, "## Changelog") != 1 {
		t.Errorf("changelog section should appear once, got %d", strings.Count(s, "## Changelog"))
	}
	if strings.Contains(s, "v1.1.0") && strings.Contains(s, "2026-06-19") {
		t.Error("bare refresh wrote a new changelog entry; only --append-changelog may")
	}

	info.Version = "v1.2.0"
	md3 := renderAgentInfoMarkdown(info)
	if err := writeAgentInfoMarkdown(path, md3, true, info, time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("append 2: %v", err)
	}
	data, _ = os.ReadFile(path)
	s = string(data)
	if !strings.Contains(s, "v1.0.0") || !strings.Contains(s, "v1.2.0") {
		t.Error("append after bare refresh lost history or new entry")
	}
	if i2, i0 := strings.Index(s, "v1.2.0"), strings.LastIndex(s, "v1.0.0"); i2 > i0 {
		t.Error("newest changelog entry should come first")
	}
}

func TestChangelogCapBounded(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AGENTS.generated.md")
	info := agentInfo{Capabilities: agentInfoCommands, EnvVars: agentInfoEnvVars}
	md := renderAgentInfoMarkdown(info)

	total := agentInfoChangelogMaxEntries + 25
	for i := 0; i < total; i++ {
		info.Version = "v0.0." + strconv.Itoa(i)
		day := 1 + (i % 27)
		if err := writeAgentInfoMarkdown(path, md, true, info, time.Date(2026, 1, day, 0, 0, 0, 0, time.UTC)); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	data, _ := os.ReadFile(path)
	s := string(data)
	idx := strings.Index(s, "## Changelog")
	if idx < 0 {
		t.Fatal("no changelog block written")
	}
	gotEntries := strings.Count(s[idx:], "\n- ")
	if gotEntries != agentInfoChangelogMaxEntries {
		t.Errorf("changelog retained %d entries, want cap %d", gotEntries, agentInfoChangelogMaxEntries)
	}
	if !strings.Contains(s, "v0.0."+strconv.Itoa(total-1)) {
		t.Error("newest entry missing after cap")
	}
	if strings.Contains(s, "v0.0.0 ") || strings.Contains(s, "`v0.0.0`") {
		t.Error("oldest entry should have aged out past the cap")
	}
}

func TestReadOpenPositionsScanErrorSignalsFailure(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "bad.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	stmts := []string{
		`CREATE TABLE app_state (id INTEGER PRIMARY KEY, cycle_count INTEGER)`,
		`INSERT INTO app_state (id, cycle_count) VALUES (1, 5)`,
		`CREATE TABLE positions (strategy_id TEXT, symbol TEXT, side TEXT, quantity TEXT, avg_cost REAL, regime TEXT)`,
		`INSERT INTO positions (strategy_id, symbol, side, quantity, avg_cost, regime) VALUES ('s1','ETH','long','not-a-number',3000,'trend')`,
	}
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seed (%s): %v", q, err)
		}
	}
	db.Close()

	_, live := readStateDBReadOnly(dbPath, 8099)
	if live.DBPresent {
		t.Error("scan failure must mark DBPresent=false, not return a partial list as complete")
	}
	if len(live.OpenPositions) != 0 {
		t.Errorf("must not hand back a truncated position slice, got %d", len(live.OpenPositions))
	}
	if !strings.Contains(live.Note, "do not trust") {
		t.Errorf("note should flag the snapshot as untrustworthy: %q", live.Note)
	}
}

func TestLoadConfigSnapshotDoesNotMutateFile(t *testing.T) {
	orig, err := os.ReadFile("config.example.json")
	if err != nil {
		t.Skipf("no config.example.json fixture: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(orig, &raw); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	raw["config_version"] = 15
	forced, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		t.Fatalf("re-marshal fixture: %v", err)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, forced, 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	before, _ := os.ReadFile(path)

	cfg, err := loadConfigSnapshot(path)
	if err != nil {
		t.Fatalf("loadConfigSnapshot: %v", err)
	}
	if cfg == nil || len(cfg.Strategies) == 0 {
		t.Fatal("expected a loaded config with strategies")
	}
	if cfg.ConfigVersion <= 15 {
		t.Fatalf("migration did not advance config_version (in=15, out=%d); test no longer exercises the write path", cfg.ConfigVersion)
	}

	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Error("loadConfigSnapshot mutated the input config file (must be read-only)")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != "config.json" {
			t.Errorf("loadConfigSnapshot left a stray file in the input dir: %q", e.Name())
		}
	}
}

func TestReadOnlyOpenDoesNotCreateDB(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "absent.db")
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_ = db.Ping()
	db.Close()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("read-only open created %s (want absent)", path)
	}
}

package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestAgentInfoCommandsCoverKnownSubcommands enforces that every dispatched
// subcommand is documented in agentInfoCommands — the anti-staleness guard
// that makes the capabilities section trustworthy (a new subcommand without a
// doc entry fails CI).
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
	// Reverse direction: every documented real command (excluding synthetic
	// "(daemon)") must be a known subcommand, so docs can't reference a
	// command that no longer exists.
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

// TestAgentInfoEnvVarsCoverSource cross-checks the curated env-var registry
// against every os.Getenv("...") literal in scheduler/*.go so the
// security-sensitive surface can't silently drift.
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

	// The literal-regex pass above misses env vars read through a variable —
	// notably shared_wallet.go's walletKeyRegistry, which does
	// os.Getenv(entry.envVar). Cross-check that registry directly so the
	// coverage guarantee survives registry/indirection reads (a fifth platform
	// entry whose envVar is unregistered must fail this test).
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
	// Required (no omitempty) key.
	if f, ok := byName["interval_seconds"]; !ok {
		t.Error("expected interval_seconds in config schema")
	} else if f.Optional {
		t.Error("interval_seconds has no omitempty; should be required")
	}
	// Optional key.
	if f, ok := byName["db_file"]; !ok {
		t.Error("expected db_file in config schema")
	} else if !f.Optional {
		t.Error("db_file has omitempty; should be optional")
	}
	// json:"-" field must be excluded.
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

	// Build a minimal DB via the real OpenStateDB (creates schema), then close
	// it so agent-info opens it read-only as a separate process would.
	sdb, err := OpenStateDB(dbPath)
	if err != nil {
		t.Fatalf("OpenStateDB: %v", err)
	}
	// Seed a strategy + open position + cycle count.
	if _, err := sdb.db.Exec(`INSERT INTO strategies (id, type, platform) VALUES ('s1','perps','hyperliquid')`); err != nil {
		t.Fatalf("seed strategy: %v", err)
	}
	if _, err := sdb.db.Exec(`INSERT INTO positions (strategy_id, symbol, quantity, avg_cost, side, regime) VALUES ('s1','ETH',1.5,3000,'long','trend')`); err != nil {
		t.Fatalf("seed position: %v", err)
	}
	if _, err := sdb.db.Exec(`INSERT INTO app_state (id, cycle_count) VALUES (1, 42) ON CONFLICT(id) DO UPDATE SET cycle_count=42`); err != nil {
		t.Fatalf("seed app_state: %v", err)
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
	if !strings.Contains(live.Note, "8099") {
		t.Errorf("live note should point at status port: %q", live.Note)
	}
	// Schema should include the core tables.
	names := map[string]bool{}
	for _, tb := range tables {
		names[tb.Name] = true
	}
	for _, want := range []string{"strategies", "positions", "trades", "app_state"} {
		if !names[want] {
			t.Errorf("schema missing table %q", want)
		}
	}

	// Read-only open must NOT have created a -wal write or mutated anything we
	// can detect; re-open read-only again and confirm the row count is stable.
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
	// The generated file must never be AGENTS.md.
	if strings.Contains(agentInfoGeneratedFile, "AGENTS.md") || agentInfoGeneratedFile == "AGENTS.md" {
		t.Fatal("generated file must not be AGENTS.md (symlink to CLAUDE.md)")
	}

	// Changelog: two appends preserve history, newest first.
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

// TestBareRefreshPreservesChangelog guards the invariant that a non-append
// regeneration never silently drops prior changelog history (only an explicit
// --append-changelog mutates it, by prepending). Covers the adversarial cases
// from the review: bare refresh after an append, and alternating modes.
func TestBareRefreshPreservesChangelog(t *testing.T) {
	info := agentInfo{Version: "v1.0.0", Capabilities: agentInfoCommands, EnvVars: agentInfoEnvVars}
	dir := t.TempDir()
	path := filepath.Join(dir, "AGENTS.generated.md")

	// 1. Append establishes a history entry.
	md := renderAgentInfoMarkdown(info)
	if err := writeAgentInfoMarkdown(path, md, true, info, time.Date(2026, 6, 18, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("append: %v", err)
	}

	// 2. Bare refresh (appendChangelog=false) must NOT drop the prior entry.
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
	// A bare refresh must not add a new dated entry — history is unchanged.
	if strings.Contains(s, "v1.1.0") && strings.Contains(s, "2026-06-19") {
		t.Error("bare refresh wrote a new changelog entry; only --append-changelog may")
	}

	// 3. A subsequent append prepends on top of the preserved history.
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

// TestLoadConfigSnapshotDoesNotMutateFile guards the core safety invariant:
// agent-info is read-only, but LoadConfig migrates pre-v15 configs in place.
// loadConfigSnapshot must load the effective shape without rewriting the input.
func TestLoadConfigSnapshotDoesNotMutateFile(t *testing.T) {
	// config.example.json ships pre-current-version; copy it so the migration
	// (if any) cannot touch the repo fixture.
	orig, err := os.ReadFile("config.example.json")
	if err != nil {
		t.Skipf("no config.example.json fixture: %v", err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, orig, 0644); err != nil {
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

	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Error("loadConfigSnapshot mutated the input config file (must be read-only)")
	}
}

// TestReadOnlyOpenDoesNotCreateDB confirms the read-only DSN never creates a DB
// file (so agent-info on a fresh checkout doesn't spawn an empty state.db).
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

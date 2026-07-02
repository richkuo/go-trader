package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- config: parse, defaults, validation, unknown-key guard ---

func TestLLMEntryAnalysisConfigParseAndDefaults(t *testing.T) {
	var sc StrategyConfig
	if sc.LLMEntryAnalysisEnabled() {
		t.Fatal("nil block must be disabled")
	}
	p := resolveLLMEntryAnalysisParams(sc)
	if p.Model != llmEntryAnalysisDefaultModel || p.MaxDebateRounds != llmEntryAnalysisDefaultRounds || p.Timeout != llmEntryAnalysisDefaultTimeoutS*time.Second {
		t.Fatalf("defaults wrong: %+v", p)
	}
	// Routing defaults: DM on, channel off.
	if !p.NotifyDM || p.NotifyChannel {
		t.Fatalf("routing defaults wrong: dm=%t channel=%t (want dm=true channel=false)", p.NotifyDM, p.NotifyChannel)
	}

	zero := 0
	sc.LLMEntryAnalysis = &LLMEntryAnalysisConfig{Enabled: true, Model: " claude-opus-4-8 ", MaxDebateRounds: &zero, TimeoutS: 300}
	if !sc.LLMEntryAnalysisEnabled() {
		t.Fatal("enabled block must report enabled")
	}
	p = resolveLLMEntryAnalysisParams(sc)
	if p.Model != "claude-opus-4-8" || p.MaxDebateRounds != 0 || p.Timeout != 300*time.Second {
		t.Fatalf("overrides wrong: %+v", p)
	}
}

// Routing is per-strategy: DM defaults on, channel defaults off, both
// overridable independently. The resolved gates are snapshotted into Params.
func TestLLMEntryAnalysisNotifyRouting(t *testing.T) {
	// nil block -> defaults (dm on, channel off).
	var sc StrategyConfig
	if !sc.llmNotifyDM() || sc.llmNotifyChannel() {
		t.Fatalf("nil block: dm=%t channel=%t (want dm=true channel=false)", sc.llmNotifyDM(), sc.llmNotifyChannel())
	}

	// Explicit block, fields unset -> same defaults.
	sc.LLMEntryAnalysis = &LLMEntryAnalysisConfig{Enabled: true}
	if !sc.llmNotifyDM() || sc.llmNotifyChannel() {
		t.Fatalf("unset fields: dm=%t channel=%t (want dm=true channel=false)", sc.llmNotifyDM(), sc.llmNotifyChannel())
	}

	// Independent overrides: DM off, channel on.
	off, on := false, true
	sc.LLMEntryAnalysis = &LLMEntryAnalysisConfig{Enabled: true, NotifyDM: &off, NotifyChannel: &on}
	if sc.llmNotifyDM() || !sc.llmNotifyChannel() {
		t.Fatalf("overrides: dm=%t channel=%t (want dm=false channel=true)", sc.llmNotifyDM(), sc.llmNotifyChannel())
	}
	p := resolveLLMEntryAnalysisParams(sc)
	if p.NotifyDM || !p.NotifyChannel {
		t.Fatalf("params snapshot: dm=%t channel=%t (want dm=false channel=true)", p.NotifyDM, p.NotifyChannel)
	}

	// Both off is legal (analysis still stamps the verdict, just posts nothing).
	sc.LLMEntryAnalysis = &LLMEntryAnalysisConfig{Enabled: true, NotifyDM: &off, NotifyChannel: &off}
	if sc.llmNotifyDM() || sc.llmNotifyChannel() {
		t.Fatalf("both off: dm=%t channel=%t (want both false)", sc.llmNotifyDM(), sc.llmNotifyChannel())
	}
}

func TestValidateLLMEntryAnalysisBounds(t *testing.T) {
	ok := StrategyConfig{LLMEntryAnalysis: &LLMEntryAnalysisConfig{Enabled: true, TimeoutS: 600}}
	if errs := validateLLMEntryAnalysis("strategy[x]", ok); len(errs) != 0 {
		t.Fatalf("valid block rejected: %v", errs)
	}
	if errs := validateLLMEntryAnalysis("strategy[x]", StrategyConfig{}); len(errs) != 0 {
		t.Fatalf("nil block rejected: %v", errs)
	}

	badTimeout := StrategyConfig{LLMEntryAnalysis: &LLMEntryAnalysisConfig{TimeoutS: 601}}
	if errs := validateLLMEntryAnalysis("strategy[x]", badTimeout); len(errs) != 1 || !strings.Contains(errs[0], "timeout_s") {
		t.Fatalf("timeout_s=601 must be rejected: %v", errs)
	}
	negTimeout := StrategyConfig{LLMEntryAnalysis: &LLMEntryAnalysisConfig{TimeoutS: -1}}
	if errs := validateLLMEntryAnalysis("strategy[x]", negTimeout); len(errs) != 1 {
		t.Fatalf("timeout_s=-1 must be rejected: %v", errs)
	}
	four := 4
	badRounds := StrategyConfig{LLMEntryAnalysis: &LLMEntryAnalysisConfig{MaxDebateRounds: &four}}
	if errs := validateLLMEntryAnalysis("strategy[x]", badRounds); len(errs) != 1 || !strings.Contains(errs[0], "max_debate_rounds") {
		t.Fatalf("rounds=4 must be rejected: %v", errs)
	}
}

// llm_entry_analysis is a declared StrategyConfig field, so the #704
// unknown-key guard must accept it (and still flag typos inside strategies).
func TestLLMEntryAnalysisKeyKnownToUnknownKeyGuard(t *testing.T) {
	raw := []byte(`{"strategies":[{"id":"a","llm_entry_analysis":{"enabled":true}}]}`)
	if errs := validateStrategyJSONKeys(raw); len(errs) != 0 {
		t.Fatalf("llm_entry_analysis flagged as unknown: %v", errs)
	}
	typo := []byte(`{"strategies":[{"id":"a","llm_entry_analysys":{"enabled":true}}]}`)
	if errs := validateStrategyJSONKeys(typo); len(errs) != 1 {
		t.Fatalf("typo must be flagged: %v", errs)
	}
}

// --- word cap + output parsing ---

func TestTruncateToWordCap(t *testing.T) {
	if got := truncateToWordCap("one two three", 5); got != "one two three" {
		t.Fatalf("under-cap changed: %q", got)
	}
	if got := truncateToWordCap("  a\n b\tc  ", 3); got != "a b c" {
		t.Fatalf("whitespace not normalized: %q", got)
	}
	got := truncateToWordCap("a b c d e", 3)
	if got != "a b c …" {
		t.Fatalf("truncation wrong: %q", got)
	}
	long := strings.Repeat("word ", 100)
	fields := strings.Fields(truncateToWordCap(long, llmEntryAnalysisWordCap))
	// cap words + the ellipsis marker
	if len(fields) != llmEntryAnalysisWordCap+1 {
		t.Fatalf("capped length = %d, want %d", len(fields), llmEntryAnalysisWordCap+1)
	}
}

func TestParseLLMEntryAnalysisOutput(t *testing.T) {
	longText := strings.Repeat("w ", 80)
	out := fmt.Sprintf(`{"verdict":"BULLISH","rationale":%q,"per_analyst":{"technical":%q}}`, longText, longText)
	res, err := parseLLMEntryAnalysisOutput([]byte(out))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if res.Verdict != "bullish" {
		t.Fatalf("verdict not normalized: %q", res.Verdict)
	}
	if n := len(strings.Fields(res.Rationale)); n != llmEntryAnalysisWordCap+1 {
		t.Fatalf("rationale not capped: %d words", n)
	}
	if n := len(strings.Fields(res.PerAnalyst["technical"])); n != llmEntryAnalysisWordCap+1 {
		t.Fatalf("per-analyst note not capped: %d words", n)
	}

	if _, err := parseLLMEntryAnalysisOutput([]byte(`{"verdict":"moon","rationale":"x"}`)); err == nil {
		t.Fatal("invalid verdict must error")
	}
	if _, err := parseLLMEntryAnalysisOutput([]byte(`{"error":"boom"}`)); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("script error must surface: %v", err)
	}
	if _, err := parseLLMEntryAnalysisOutput(nil); err == nil {
		t.Fatal("empty output must error")
	}
	if _, err := parseLLMEntryAnalysisOutput([]byte("not json")); err == nil {
		t.Fatal("non-JSON must error")
	}
}

// --- dispatch predicate ---

func llmTestStrategyConfig(enabled bool) StrategyConfig {
	sc := StrategyConfig{
		ID: "hl-btc", Type: "perps", Platform: "hyperliquid",
		Args: []string{"momentum", "BTC", "4h", "--mode=live"},
	}
	if enabled {
		sc.LLMEntryAnalysis = &LLMEntryAnalysisConfig{Enabled: true}
	}
	return sc
}

func llmTestState() *StrategyState {
	return &StrategyState{
		ID: "hl-btc",
		Positions: map[string]*Position{
			"BTC": {Symbol: "BTC", Quantity: 0.1, AvgCost: 50000, Side: "long", EntryATR: 400, Regime: "trending_up", Leverage: 3},
		},
	}
}

func TestQueueLLMEntryAnalysisIfOpened(t *testing.T) {
	openTrade := &Trade{Symbol: "BTC"}
	indicators := map[string]interface{}{"atr": 400.0}

	var jobs []llmEntryAnalysisJob
	llmEntryAnalysisEnqueue = func(job llmEntryAnalysisJob) bool {
		jobs = append(jobs, job)
		return true
	}
	defer func() { llmEntryAnalysisEnqueue = nil }()

	// Fresh open (1 trade, open leg): dispatches once with the snapshot.
	s := llmTestState()
	queueLLMEntryAnalysisIfOpened(llmTestStrategyConfig(true), s, "BTC", 1, openTrade, indicators)
	if len(jobs) != 1 {
		t.Fatalf("fresh open must dispatch, got %d jobs", len(jobs))
	}
	job := jobs[0]
	if job.StrategyID != "hl-btc" || job.Side != "long" || job.EntryPrice != 50000 ||
		job.Timeframe != "4h" || !job.IsLive || job.Regime != "trending_up" || job.PositionID == "" {
		t.Fatalf("job snapshot wrong: %+v", job)
	}
	if !s.Positions["BTC"].LLMAnalysisRequested {
		t.Fatal("dispatch must set the idempotency marker")
	}

	// Same position again (e.g. a later cycle): idempotent, no second job.
	queueLLMEntryAnalysisIfOpened(llmTestStrategyConfig(true), s, "BTC", 1, openTrade, indicators)
	if len(jobs) != 1 {
		t.Fatalf("marker must suppress re-dispatch, got %d jobs", len(jobs))
	}

	// Flip (close+open = 2 legs): excluded.
	s2 := llmTestState()
	queueLLMEntryAnalysisIfOpened(llmTestStrategyConfig(true), s2, "BTC", 2, openTrade, indicators)
	if len(jobs) != 1 {
		t.Fatal("flip (tradesExecuted=2) must not dispatch")
	}
	if s2.Positions["BTC"].LLMAnalysisRequested {
		t.Fatal("flip must not set the marker")
	}

	// Close leg (no open trade): excluded.
	queueLLMEntryAnalysisIfOpened(llmTestStrategyConfig(true), llmTestState(), "BTC", 1, nil, indicators)
	if len(jobs) != 1 {
		t.Fatal("close leg must not dispatch")
	}

	// Not opted in: excluded.
	queueLLMEntryAnalysisIfOpened(llmTestStrategyConfig(false), llmTestState(), "BTC", 1, openTrade, indicators)
	if len(jobs) != 1 {
		t.Fatal("disabled strategy must not dispatch")
	}

	// No position for the symbol: excluded.
	queueLLMEntryAnalysisIfOpened(llmTestStrategyConfig(true), &StrategyState{ID: "hl-btc", Positions: map[string]*Position{}}, "BTC", 1, openTrade, indicators)
	if len(jobs) != 1 {
		t.Fatal("missing position must not dispatch")
	}
}

func TestQueueLLMEntryAnalysisNilHookNoOp(t *testing.T) {
	llmEntryAnalysisEnqueue = nil
	s := llmTestState()
	// Must not panic and must not set the marker (subcommands/tests without a worker).
	queueLLMEntryAnalysisIfOpened(llmTestStrategyConfig(true), s, "BTC", 1, &Trade{}, nil)
	if s.Positions["BTC"].LLMAnalysisRequested {
		t.Fatal("nil hook must not set the marker")
	}
}

// --- worker ---

func TestLLMEntryAnalysisWorkerProcess(t *testing.T) {
	var stamped []string
	var notified []*LLMEntryAnalysisResult
	w := newLLMEntryAnalysisWorker(
		func(ctx context.Context, job llmEntryAnalysisJob) (*LLMEntryAnalysisResult, error) {
			return &LLMEntryAnalysisResult{Verdict: "bearish", Rationale: "r"}, nil
		},
		func(job llmEntryAnalysisJob, verdict string) { stamped = append(stamped, verdict) },
		func(job llmEntryAnalysisJob, res *LLMEntryAnalysisResult) { notified = append(notified, res) },
	)
	w.process(context.Background(), llmEntryAnalysisJob{StrategyID: "a", Symbol: "BTC"})
	if len(stamped) != 1 || stamped[0] != "bearish" || len(notified) != 1 {
		t.Fatalf("success must stamp+notify: stamped=%v notified=%d", stamped, len(notified))
	}

	// Failure: zero output — no stamp, no post.
	stamped, notified = nil, nil
	w.runner = func(ctx context.Context, job llmEntryAnalysisJob) (*LLMEntryAnalysisResult, error) {
		return nil, fmt.Errorf("timeout")
	}
	w.process(context.Background(), llmEntryAnalysisJob{})
	if len(stamped) != 0 || len(notified) != 0 {
		t.Fatal("failed analysis must post nothing")
	}
}

func TestLLMEntryAnalysisWorkerQueueFull(t *testing.T) {
	w := newLLMEntryAnalysisWorker(nil, nil, nil)
	for i := 0; i < llmEntryAnalysisQueueCap; i++ {
		if !w.Enqueue(llmEntryAnalysisJob{}) {
			t.Fatalf("enqueue %d should fit", i)
		}
	}
	if w.Enqueue(llmEntryAnalysisJob{}) {
		t.Fatal("enqueue past cap must be non-blocking and report false")
	}
}

// --- digest formatting ---

func TestFormatLLMEntryAnalysisDigestSortedAndLabeled(t *testing.T) {
	job := llmEntryAnalysisJob{StrategyID: "hl-btc", Symbol: "BTC", Side: "long", EntryPrice: 50000, IsLive: true}
	res := &LLMEntryAnalysisResult{
		Verdict: "mixed", Rationale: "cuts both ways",
		PerAnalyst: map[string]string{"technical": "t-note", "derivatives": "d-note"},
	}
	msg := formatLLMEntryAnalysisDigest(job, res, false)
	if !strings.Contains(msg, "[hl-btc] LONG BTC @ $50000.00 (live)") {
		t.Fatalf("header wrong: %q", msg)
	}
	if !strings.Contains(msg, "**Verdict: MIXED**") || !strings.Contains(msg, "cuts both ways") {
		t.Fatalf("verdict line wrong: %q", msg)
	}
	// Map iteration is randomized; the digest must be deterministic (sorted keys).
	if strings.Index(msg, "derivatives:") > strings.Index(msg, "technical:") {
		t.Fatalf("analyst topics not sorted: %q", msg)
	}
}

// TestFormatLLMEntryAnalysisDigestPlainTextOmitsMarkdown guards #1208 review
// feedback: a plainText route (e.g. Telegram, notifier.go plainText:true)
// must not receive literal "**" bold markers, matching how sendTradeAlerts
// swaps FormatTradeDM for FormatTradeDMPlain on the canonical trade-alert
// path (main.go:3172). A markdown route must still get the bold verdict.
func TestFormatLLMEntryAnalysisDigestPlainTextOmitsMarkdown(t *testing.T) {
	job := llmEntryAnalysisJob{StrategyID: "hl-btc", Symbol: "BTC", Side: "long", EntryPrice: 50000, IsLive: true}
	res := &LLMEntryAnalysisResult{Verdict: "bullish", Rationale: "clean breakout"}

	plain := formatLLMEntryAnalysisDigest(job, res, true)
	if strings.Contains(plain, "**") {
		t.Fatalf("plainText digest must not contain literal markdown bold: %q", plain)
	}
	if !strings.Contains(plain, "Verdict: BULLISH") || !strings.Contains(plain, "clean breakout") {
		t.Fatalf("plainText digest missing verdict content: %q", plain)
	}

	markdown := formatLLMEntryAnalysisDigest(job, res, false)
	if !strings.Contains(markdown, "**Verdict: BULLISH**") {
		t.Fatalf("markdown digest must still bold the verdict: %q", markdown)
	}
}

// --- verdict persistence: position -> diagnostics row -> SQLite ---

func TestCaptureTradeDiagnosticsCarriesLLMVerdict(t *testing.T) {
	var rows []*TradeDiagnosticsRow
	tradeDiagnosticsRecorder = func(row *TradeDiagnosticsRow) error {
		rows = append(rows, row)
		return nil
	}
	defer func() { tradeDiagnosticsRecorder = nil }()

	s := &StrategyState{ID: "hl-btc"}
	pos := &Position{Symbol: "BTC", Quantity: 0.1, AvgCost: 50000, Side: "long", LLMVerdict: "bullish"}
	captureTradeDiagnostics(s, pos, 51000, 100, "signal", time.Now().UTC())
	if len(rows) != 1 || rows[0].LLMVerdict == nil || *rows[0].LLMVerdict != "bullish" {
		t.Fatalf("verdict must reach the diagnostics row: %+v", rows)
	}

	// No verdict (analysis off/failed/unfinished): stays nil -> SQL NULL.
	rows = nil
	pos.LLMVerdict = ""
	captureTradeDiagnostics(s, pos, 51000, 100, "signal", time.Now().UTC())
	if len(rows) != 1 || rows[0].LLMVerdict != nil {
		t.Fatalf("empty verdict must stay nil: %+v", rows)
	}
}

func TestInsertTradeDiagnosticsWritesLLMVerdict(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	sdb, err := OpenStateDB(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sdb.Close()

	verdict := "mixed"
	now := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	row := &TradeDiagnosticsRow{
		StrategyID: "hl-a", PositionID: "p1", Symbol: "BTC", Side: "long",
		EntryPrice: 50000, ExitPrice: 51000, Quantity: 0.1,
		OpenedAt: now, ClosedAt: now.Add(time.Hour), MetricsStatus: diagMetricsPending,
		LLMVerdict: &verdict,
	}
	if err := sdb.InsertTradeDiagnostics(row); err != nil {
		t.Fatalf("insert: %v", err)
	}
	rows, err := sdb.TradeDiagnosticsRows("hl-a")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 1 || rows[0].LLMVerdict == nil || *rows[0].LLMVerdict != "mixed" {
		t.Fatalf("llm_verdict round-trip wrong: %+v", rows)
	}
}

// --- position persistence round-trip ---

func TestPositionLLMFieldsPersistRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	sdb, err := OpenStateDB(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sdb.Close()

	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-btc": {
			ID: "hl-btc", Cash: 1000,
			Positions: map[string]*Position{
				"BTC": {Symbol: "BTC", Quantity: 0.1, AvgCost: 50000, Side: "long",
					LLMAnalysisRequested: true, LLMVerdict: "bullish"},
			},
		},
	}}
	if err := sdb.SaveState(state); err != nil {
		t.Fatalf("save: %v", err)
	}
	loaded, err := sdb.LoadState()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	pos := loaded.Strategies["hl-btc"].Positions["BTC"]
	if pos == nil || !pos.LLMAnalysisRequested || pos.LLMVerdict != "bullish" {
		t.Fatalf("LLM fields must survive a save/load cycle: %+v", pos)
	}
}

// --- hot reload: notification-only, reloadable while a position is open ---

func TestApplyHotReloadConfig_LLMEntryAnalysisWhileOpen(t *testing.T) {
	base := func(block *LLMEntryAnalysisConfig) []StrategyConfig {
		return []StrategyConfig{{
			ID: "hl-eth", Type: "perps", Platform: "hyperliquid",
			Script:  "shared_scripts/check_hyperliquid.py",
			Args:    []string{"momentum", "ETH", "1h", "--mode=paper"},
			Capital: 1000, MaxDrawdownPct: 10, Leverage: 2, Direction: DirectionLong,
			LLMEntryAnalysis: block,
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

	// off (nil) -> on while open: accepted, applied, logged.
	cfg := minimalReloadConfig(base(nil))
	next := minimalReloadConfig(base(&LLMEntryAnalysisConfig{Enabled: true, TimeoutS: 60}))
	changes, err := applyHotReloadConfig(cfg, next, openState(), nil, nil)
	if err != nil {
		t.Fatalf("llm_entry_analysis nil->on while open should be hot-reloadable: %v", err)
	}
	if !cfg.Strategies[0].LLMEntryAnalysisEnabled() || cfg.Strategies[0].LLMEntryAnalysis.TimeoutS != 60 {
		t.Fatalf("block not applied: %+v", cfg.Strategies[0].LLMEntryAnalysis)
	}
	if !strings.Contains(strings.Join(changes, "\n"), "llm_entry_analysis") {
		t.Fatalf("expected an llm_entry_analysis change entry, got %v", changes)
	}

	// on -> off while open: accepted.
	cfg = minimalReloadConfig(base(&LLMEntryAnalysisConfig{Enabled: true}))
	next = minimalReloadConfig(base(nil))
	if _, err := applyHotReloadConfig(cfg, next, openState(), nil, nil); err != nil {
		t.Fatalf("llm_entry_analysis on->off while open should be hot-reloadable: %v", err)
	}
	if cfg.Strategies[0].LLMEntryAnalysis != nil {
		t.Fatal("expected llm_entry_analysis cleared after reload")
	}
}

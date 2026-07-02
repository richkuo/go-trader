package main

// llm_entry_analysis.go implements #1137: optional, per-strategy LLM
// multi-agent entry analysis (TradingAgents-inspired). After a fresh
// position-open is confirmed, an async job runs shared_scripts/llm_review.py
// (analysts -> bounded bull/bear debate -> verdict) and posts a short
// plain-language digest to the strategy's trade-alert channel.
//
// Advisory by construction: nothing here gates, sizes, or closes anything.
// A timeout/error posts nothing and has zero trade impact.
//
// Execution lane (hard requirement from the issue): LLM jobs never touch the
// shared pythonSemaphore/scriptTimeout path — a multi-agent debate routinely
// exceeds both and would starve trading-path subprocesses. The worker has its
// own queue + concurrency cap and spawns via spawnPythonProcess with the
// per-strategy timeout_s deadline. Jobs ride shutdownReadOnlyCtx, so they are
// cancelled immediately at SIGTERM instead of joining the side-effect drain.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"reflect"
	"sort"
	"strings"
	"time"
)

const (
	llmEntryAnalysisScript = "shared_scripts/llm_review.py"
	// llmEntryAnalysisAPIKeyEnv is read by llm_review.py; Go only checks
	// presence to warn early when analysis is enabled without a key.
	llmEntryAnalysisAPIKeyEnv = "ANTHROPIC_API_KEY"

	llmEntryAnalysisDefaultModel    = "claude-sonnet-5"
	llmEntryAnalysisDefaultRounds   = 1
	llmEntryAnalysisMaxRounds       = 3
	llmEntryAnalysisDefaultTimeoutS = 120
	llmEntryAnalysisMaxTimeoutS     = 600

	// llmEntryAnalysisWordCap is the per-topic ELI18 word cap, enforced in the
	// prompt AND re-enforced here so an over-long model response can't leak
	// through to Discord.
	llmEntryAnalysisWordCap = 55

	// Queue/concurrency bounds: a burst of opens must not spawn unbounded LLM
	// jobs. A full queue drops the job with a WARN (analysis is advisory; a
	// dropped job is a missing comment, never a trading problem).
	llmEntryAnalysisQueueCap      = 16
	llmEntryAnalysisMaxConcurrent = 2
)

// LLMEntryAnalysisConfig is the per-strategy opt-in block (#1137). Default
// off; notification-only, so it hot-reloads unconditionally (see
// strategyRestartShape / applyHotReloadConfig).
type LLMEntryAnalysisConfig struct {
	Enabled bool `json:"enabled,omitempty"`
	// Model passed to llm_review.py; empty = llmEntryAnalysisDefaultModel.
	Model string `json:"model,omitempty"`
	// MaxDebateRounds bounds the bull/bear debate. nil = default 1; explicit 0
	// skips the debate (analysts -> verdict directly).
	MaxDebateRounds *int `json:"max_debate_rounds,omitempty"`
	// TimeoutS is the whole-pipeline subprocess deadline. 0 = default 120s.
	TimeoutS int `json:"timeout_s,omitempty"`
}

// LLMEntryAnalysisEnabled reports whether the strategy opted in.
func (sc *StrategyConfig) LLMEntryAnalysisEnabled() bool {
	return sc != nil && sc.LLMEntryAnalysis != nil && sc.LLMEntryAnalysis.Enabled
}

// llmEntryAnalysisParams is the resolved (defaults applied) job parameter set,
// snapshotted at dispatch so a hot-reload never mutates an in-flight job.
type llmEntryAnalysisParams struct {
	Model           string
	MaxDebateRounds int
	Timeout         time.Duration
}

func resolveLLMEntryAnalysisParams(sc StrategyConfig) llmEntryAnalysisParams {
	p := llmEntryAnalysisParams{
		Model:           llmEntryAnalysisDefaultModel,
		MaxDebateRounds: llmEntryAnalysisDefaultRounds,
		Timeout:         llmEntryAnalysisDefaultTimeoutS * time.Second,
	}
	c := sc.LLMEntryAnalysis
	if c == nil {
		return p
	}
	if strings.TrimSpace(c.Model) != "" {
		p.Model = strings.TrimSpace(c.Model)
	}
	if c.MaxDebateRounds != nil {
		p.MaxDebateRounds = *c.MaxDebateRounds
	}
	if c.TimeoutS > 0 {
		p.Timeout = time.Duration(c.TimeoutS) * time.Second
	}
	return p
}

// validateLLMEntryAnalysis returns config errors for the block (nil block is
// valid — feature off).
func validateLLMEntryAnalysis(prefix string, sc StrategyConfig) []string {
	c := sc.LLMEntryAnalysis
	if c == nil {
		return nil
	}
	var errs []string
	if c.TimeoutS < 0 || c.TimeoutS > llmEntryAnalysisMaxTimeoutS {
		errs = append(errs, fmt.Sprintf("%s: llm_entry_analysis.timeout_s must be in [0, %d] (0 = default %ds), got %d",
			prefix, llmEntryAnalysisMaxTimeoutS, llmEntryAnalysisDefaultTimeoutS, c.TimeoutS))
	}
	if c.MaxDebateRounds != nil && (*c.MaxDebateRounds < 0 || *c.MaxDebateRounds > llmEntryAnalysisMaxRounds) {
		errs = append(errs, fmt.Sprintf("%s: llm_entry_analysis.max_debate_rounds must be in [0, %d], got %d",
			prefix, llmEntryAnalysisMaxRounds, *c.MaxDebateRounds))
	}
	return errs
}

// llmEntryAnalysisConfigEqual compares blocks for hot-reload change detection.
func llmEntryAnalysisConfigEqual(a, b *LLMEntryAnalysisConfig) bool {
	return reflect.DeepEqual(a, b)
}

func formatLLMEntryAnalysis(c *LLMEntryAnalysisConfig) string {
	if c == nil || !c.Enabled {
		return "off"
	}
	p := resolveLLMEntryAnalysisParams(StrategyConfig{LLMEntryAnalysis: c})
	return fmt.Sprintf("on(model=%s, rounds=%d, timeout=%s)", p.Model, p.MaxDebateRounds, p.Timeout)
}

// llmEntryAnalysisJob is the immutable context snapshot for one analysis,
// captured under mu at dispatch time.
type llmEntryAnalysisJob struct {
	StrategyID string
	Symbol     string
	Platform   string
	StratType  string
	PositionID string
	Side       string
	EntryPrice float64
	Quantity   float64
	Leverage   float64
	EntryATR   float64
	Timeframe  string
	Regime     string
	IsLive     bool
	Indicators map[string]interface{}
	Params     llmEntryAnalysisParams
}

// llmEntryAnalysisEnqueue is the package-level dispatch hook, set in main()
// to the worker's Enqueue (nil in subcommands and tests that don't wire it —
// the queue helper no-ops). Mirrors the tradeDiagnostics hooks.
var llmEntryAnalysisEnqueue func(job llmEntryAnalysisJob) bool

// queueLLMEntryAnalysisIfOpened dispatches one analysis for a strategy that
// opted in, right after a FRESH position-open is confirmed. Must be called
// under the caller's state Lock, after the entry stamps (EntryATR/regime) so
// the job snapshot carries them.
//
// Trigger scope (#1137): opens only. openTrade != nil excludes closes and
// partial closes; tradesExecuted == 1 excludes flips (a flip synthesizes a
// close+open pair, 2 legs) and the HL immediate-SL-fill open+close pair;
// scale-in adds and manual opens route through separate apply paths that
// never call this. The per-position LLMAnalysisRequested marker makes the
// dispatch idempotent (at most one analysis per opened position, surviving
// restarts via the positions table).
func queueLLMEntryAnalysisIfOpened(sc StrategyConfig, s *StrategyState, symbol string, tradesExecuted int, openTrade *Trade, indicators map[string]interface{}) {
	if llmEntryAnalysisEnqueue == nil || s == nil || !sc.LLMEntryAnalysisEnabled() {
		return
	}
	if openTrade == nil || tradesExecuted != 1 {
		return
	}
	pos, ok := s.Positions[symbol]
	if !ok || pos == nil || pos.LLMAnalysisRequested {
		return
	}
	pos.LLMAnalysisRequested = true
	job := llmEntryAnalysisJob{
		StrategyID: s.ID,
		Symbol:     symbol,
		Platform:   sc.Platform,
		StratType:  sc.Type,
		PositionID: ensurePositionTradeID(s.ID, symbol, pos),
		Side:       pos.Side,
		EntryPrice: pos.AvgCost,
		Quantity:   pos.Quantity,
		Leverage:   pos.Leverage,
		EntryATR:   pos.EntryATR,
		Timeframe:  llmEntryAnalysisTimeframe(sc),
		Regime:     pos.Regime,
		IsLive:     isLiveArgs(sc.Args),
		Indicators: indicators,
		Params:     resolveLLMEntryAnalysisParams(sc),
	}
	if !llmEntryAnalysisEnqueue(job) {
		log.Printf("[WARN] [llm-analysis] queue full, dropping analysis for %s %s (advisory only, no trade impact)", s.ID, symbol)
	}
}

// llmEntryAnalysisTimeframe resolves the strategy's candle timeframe: the
// explicit config field (manual/regime bundles), else the check-script argv
// convention <strategy> <symbol> <timeframe>.
func llmEntryAnalysisTimeframe(sc StrategyConfig) string {
	if sc.Timeframe != "" {
		return sc.Timeframe
	}
	if len(sc.Args) >= 3 {
		return sc.Args[2]
	}
	return "1h"
}

// LLMEntryAnalysisResult is the typed JSON contract with llm_review.py.
type LLMEntryAnalysisResult struct {
	Verdict    string            `json:"verdict"`
	Rationale  string            `json:"rationale"`
	PerAnalyst map[string]string `json:"per_analyst,omitempty"`
	Model      string            `json:"model,omitempty"`
	Error      string            `json:"error,omitempty"`
}

var llmEntryAnalysisVerdicts = map[string]bool{"bullish": true, "bearish": true, "mixed": true}

// parseLLMEntryAnalysisOutput parses and sanitizes the pipeline's stdout.
// Enforces the verdict vocabulary and re-applies the per-topic word cap
// server-side (the prompt asks for it; this guarantees it).
func parseLLMEntryAnalysisOutput(stdout []byte) (*LLMEntryAnalysisResult, error) {
	trimmed := strings.TrimSpace(string(stdout))
	if trimmed == "" {
		return nil, fmt.Errorf("empty output")
	}
	var res LLMEntryAnalysisResult
	if err := json.Unmarshal([]byte(trimmed), &res); err != nil {
		return nil, fmt.Errorf("parse output: %w", err)
	}
	if res.Error != "" {
		return nil, fmt.Errorf("pipeline error: %s", res.Error)
	}
	res.Verdict = strings.ToLower(strings.TrimSpace(res.Verdict))
	if !llmEntryAnalysisVerdicts[res.Verdict] {
		return nil, fmt.Errorf("invalid verdict %q (want bullish/bearish/mixed)", res.Verdict)
	}
	res.Rationale = truncateToWordCap(res.Rationale, llmEntryAnalysisWordCap)
	for k, v := range res.PerAnalyst {
		res.PerAnalyst[k] = truncateToWordCap(v, llmEntryAnalysisWordCap)
	}
	return &res, nil
}

// truncateToWordCap hard-caps text at n words, appending an ellipsis when it
// truncates. Whitespace-normalizing by construction (strings.Fields).
func truncateToWordCap(text string, n int) string {
	words := strings.Fields(text)
	if len(words) <= n {
		return strings.Join(words, " ")
	}
	return strings.Join(words[:n], " ") + " …"
}

// formatLLMEntryAnalysisDigest renders the Discord/Telegram digest: one tight
// blurb per topic, analyst keys sorted (map iteration is randomized).
func formatLLMEntryAnalysisDigest(job llmEntryAnalysisJob, res *LLMEntryAnalysisResult) string {
	mode := "paper"
	if job.IsLive {
		mode = "live"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "🧠 LLM entry analysis — [%s] %s %s @ $%.2f (%s)\n",
		job.StrategyID, strings.ToUpper(job.Side), job.Symbol, job.EntryPrice, mode)
	fmt.Fprintf(&b, "**Verdict: %s** — %s", strings.ToUpper(res.Verdict), res.Rationale)
	keys := make([]string, 0, len(res.PerAnalyst))
	for k := range res.PerAnalyst {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "\n• %s: %s", k, res.PerAnalyst[k])
	}
	return b.String()
}

// llmReviewInput is the stdin JSON contract with llm_review.py.
type llmReviewInput struct {
	StrategyID      string                 `json:"strategy_id"`
	Symbol          string                 `json:"symbol"`
	Platform        string                 `json:"platform"`
	Type            string                 `json:"type"`
	Side            string                 `json:"side"`
	EntryPrice      float64                `json:"entry_price"`
	Quantity        float64                `json:"quantity"`
	Leverage        float64                `json:"leverage,omitempty"`
	EntryATR        float64                `json:"entry_atr,omitempty"`
	Timeframe       string                 `json:"timeframe"`
	Regime          string                 `json:"regime,omitempty"`
	IsLive          bool                   `json:"is_live"`
	Indicators      map[string]interface{} `json:"indicators,omitempty"`
	Model           string                 `json:"model"`
	MaxDebateRounds int                    `json:"max_debate_rounds"`
	WordCap         int                    `json:"word_cap"`
}

// runLLMEntryAnalysisScript spawns llm_review.py on the dedicated lane (own
// deadline, no shared pythonSemaphore) and parses its JSON output.
func runLLMEntryAnalysisScript(ctx context.Context, job llmEntryAnalysisJob) (*LLMEntryAnalysisResult, error) {
	stdin, err := json.Marshal(llmReviewInput{
		StrategyID:      job.StrategyID,
		Symbol:          job.Symbol,
		Platform:        job.Platform,
		Type:            job.StratType,
		Side:            job.Side,
		EntryPrice:      job.EntryPrice,
		Quantity:        job.Quantity,
		Leverage:        job.Leverage,
		EntryATR:        job.EntryATR,
		Timeframe:       job.Timeframe,
		Regime:          job.Regime,
		IsLive:          job.IsLive,
		Indicators:      job.Indicators,
		Model:           job.Params.Model,
		MaxDebateRounds: job.Params.MaxDebateRounds,
		WordCap:         llmEntryAnalysisWordCap,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal input: %w", err)
	}
	stdout, stderr, runErr := spawnPythonProcess(ctx, llmEntryAnalysisScript, nil, stdin, job.Params.Timeout)
	// Subprocess contract: JSON to stdout even on error — prefer the script's
	// own error message over the bare exit status.
	res, parseErr := parseLLMEntryAnalysisOutput(stdout)
	if parseErr != nil {
		if runErr != nil {
			return nil, fmt.Errorf("%v (stderr: %s)", runErr, firstLine(stderr))
		}
		return nil, parseErr
	}
	if runErr != nil {
		return nil, fmt.Errorf("%v (stderr: %s)", runErr, firstLine(stderr))
	}
	return res, nil
}

func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// llmEntryAnalysisWorker drains the job queue on its own lane. runner,
// stampVerdict, and notify are injected so tests never spawn Python.
type llmEntryAnalysisWorker struct {
	jobs         chan llmEntryAnalysisJob
	runner       func(ctx context.Context, job llmEntryAnalysisJob) (*LLMEntryAnalysisResult, error)
	stampVerdict func(job llmEntryAnalysisJob, verdict string)
	notify       func(job llmEntryAnalysisJob, res *LLMEntryAnalysisResult)
}

func newLLMEntryAnalysisWorker(
	runner func(ctx context.Context, job llmEntryAnalysisJob) (*LLMEntryAnalysisResult, error),
	stampVerdict func(job llmEntryAnalysisJob, verdict string),
	notify func(job llmEntryAnalysisJob, res *LLMEntryAnalysisResult),
) *llmEntryAnalysisWorker {
	return &llmEntryAnalysisWorker{
		jobs:         make(chan llmEntryAnalysisJob, llmEntryAnalysisQueueCap),
		runner:       runner,
		stampVerdict: stampVerdict,
		notify:       notify,
	}
}

// Enqueue is non-blocking; false = queue full (caller logs and drops).
func (w *llmEntryAnalysisWorker) Enqueue(job llmEntryAnalysisJob) bool {
	select {
	case w.jobs <- job:
		return true
	default:
		return false
	}
}

// run drains jobs until ctx is cancelled (shutdownReadOnlyCtx: SIGTERM kills
// in-flight analyses immediately — advisory output is safe to abandon and
// must never hold up shutdown).
func (w *llmEntryAnalysisWorker) run(ctx context.Context) {
	sem := make(chan struct{}, llmEntryAnalysisMaxConcurrent)
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-w.jobs:
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			go func(job llmEntryAnalysisJob) {
				defer func() { <-sem }()
				w.process(ctx, job)
			}(job)
		}
	}
}

func (w *llmEntryAnalysisWorker) process(ctx context.Context, job llmEntryAnalysisJob) {
	res, err := w.runner(ctx, job)
	if err != nil || res == nil {
		// Zero trade impact by construction: log and post nothing.
		log.Printf("[llm-analysis] %s %s: analysis failed: %v", job.StrategyID, job.Symbol, err)
		return
	}
	if w.stampVerdict != nil {
		w.stampVerdict(job, res.Verdict)
	}
	if w.notify != nil {
		w.notify(job, res)
	}
}

// anyStrategyUsesLLMEntryAnalysis gates the startup probe of llm_review.py
// and the missing-API-key warning.
func anyStrategyUsesLLMEntryAnalysis(cfg *Config) bool {
	if cfg == nil {
		return false
	}
	for i := range cfg.Strategies {
		if cfg.Strategies[i].LLMEntryAnalysisEnabled() {
			return true
		}
	}
	return false
}

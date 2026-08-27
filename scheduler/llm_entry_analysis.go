package main


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
	llmEntryAnalysisAPIKeyEnv = "ANTHROPIC_API_KEY"

	llmEntryAnalysisDefaultModel    = "claude-sonnet-5"
	llmEntryAnalysisDefaultRounds   = 1
	llmEntryAnalysisMaxRounds       = 3
	llmEntryAnalysisDefaultTimeoutS = 120
	llmEntryAnalysisMaxTimeoutS     = 600

	llmEntryAnalysisWordCap = 55

	llmEntryAnalysisQueueCap      = 16
	llmEntryAnalysisMaxConcurrent = 2
)

type LLMEntryAnalysisConfig struct {
	Enabled bool `json:"enabled,omitempty"`
	Model string `json:"model,omitempty"`
	MaxDebateRounds *int `json:"max_debate_rounds,omitempty"`
	TimeoutS int `json:"timeout_s,omitempty"`
	NotifyDM *bool `json:"notify_dm,omitempty"`
	NotifyChannel *bool `json:"notify_channel,omitempty"`
}

func (sc *StrategyConfig) llmNotifyDM() bool {
	if sc == nil || sc.LLMEntryAnalysis == nil || sc.LLMEntryAnalysis.NotifyDM == nil {
		return true
	}
	return *sc.LLMEntryAnalysis.NotifyDM
}

func (sc *StrategyConfig) llmNotifyChannel() bool {
	if sc == nil || sc.LLMEntryAnalysis == nil || sc.LLMEntryAnalysis.NotifyChannel == nil {
		return false
	}
	return *sc.LLMEntryAnalysis.NotifyChannel
}

func (sc *StrategyConfig) LLMEntryAnalysisEnabled() bool {
	return sc != nil && sc.LLMEntryAnalysis != nil && sc.LLMEntryAnalysis.Enabled
}

type llmEntryAnalysisParams struct {
	Model           string
	MaxDebateRounds int
	Timeout         time.Duration
	NotifyDM      bool
	NotifyChannel bool
}

func resolveLLMEntryAnalysisParams(sc StrategyConfig) llmEntryAnalysisParams {
	p := llmEntryAnalysisParams{
		Model:           llmEntryAnalysisDefaultModel,
		MaxDebateRounds: llmEntryAnalysisDefaultRounds,
		Timeout:         llmEntryAnalysisDefaultTimeoutS * time.Second,
		NotifyDM:        sc.llmNotifyDM(),
		NotifyChannel:   sc.llmNotifyChannel(),
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

func llmEntryAnalysisConfigEqual(a, b *LLMEntryAnalysisConfig) bool {
	return reflect.DeepEqual(a, b)
}

func formatLLMEntryAnalysis(c *LLMEntryAnalysisConfig) string {
	if c == nil || !c.Enabled {
		return "off"
	}
	p := resolveLLMEntryAnalysisParams(StrategyConfig{LLMEntryAnalysis: c})
	return fmt.Sprintf("on(model=%s, rounds=%d, timeout=%s, dm=%t, channel=%t)",
		p.Model, p.MaxDebateRounds, p.Timeout, p.NotifyDM, p.NotifyChannel)
}

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

var llmEntryAnalysisEnqueue func(job llmEntryAnalysisJob) bool

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

func llmEntryAnalysisTimeframe(sc StrategyConfig) string {
	if sc.Timeframe != "" {
		return sc.Timeframe
	}
	if len(sc.Args) >= 3 {
		return sc.Args[2]
	}
	return "1h"
}

type LLMEntryAnalysisResult struct {
	Verdict    string            `json:"verdict"`
	Rationale  string            `json:"rationale"`
	PerAnalyst map[string]string `json:"per_analyst,omitempty"`
	Model      string            `json:"model,omitempty"`
	Error      string            `json:"error,omitempty"`
}

var llmEntryAnalysisVerdicts = map[string]bool{"bullish": true, "bearish": true, "mixed": true}

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

func truncateToWordCap(text string, n int) string {
	words := strings.Fields(text)
	if len(words) <= n {
		return strings.Join(words, " ")
	}
	return strings.Join(words[:n], " ") + " …"
}

func formatLLMEntryAnalysisDigest(job llmEntryAnalysisJob, res *LLMEntryAnalysisResult, plainText bool) string {
	mode := "paper"
	if job.IsLive {
		mode = "live"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "🧠 LLM entry analysis — [%s] %s %s @ $%.2f (%s)\n",
		job.StrategyID, strings.ToUpper(job.Side), job.Symbol, job.EntryPrice, mode)
	if plainText {
		fmt.Fprintf(&b, "Verdict: %s — %s", strings.ToUpper(res.Verdict), res.Rationale)
	} else {
		fmt.Fprintf(&b, "**Verdict: %s** — %s", strings.ToUpper(res.Verdict), res.Rationale)
	}
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

func (w *llmEntryAnalysisWorker) Enqueue(job llmEntryAnalysisJob) bool {
	select {
	case w.jobs <- job:
		return true
	default:
		return false
	}
}

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

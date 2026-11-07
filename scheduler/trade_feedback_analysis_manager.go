package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	tradeFeedbackAnalysisQueueCap        = 4
	tradeFeedbackValidatorScript         = "shared_scripts/llm_strategy_feedback.py"
	tradeFeedbackAnalysisShutdownMessage = "the scheduler stopped while this analysis was queued or running; it was cancelled"
)

var (
	tradeFeedbackValidatorStdoutCap = 1 << 20
	tradeFeedbackValidatorStderrCap = 64 << 10
)

type tradeFeedbackAnalysisJob struct {
	RequestID    string
	StrategyID   string
	Provider     string
	Adapter      tradeFeedbackAnalysisAdapter
	Admission    tradeFeedbackAdmission
	Bundle       []byte
	BundleSHA256 string
	Deadline     time.Time
	Settings     tradeFeedbackAnalysisSettings
	db           *StateDB
}

type tradeFeedbackValidator func(ctx context.Context, bundle []byte, result json.RawMessage, timeout time.Duration) ([]byte, string, error)

type tradeFeedbackReservation struct{}

// tradeFeedbackAnalysisManager is the one daemon-owned lane for provider
// calls and result validation. One job executes at a time and at most
// tradeFeedbackAnalysisQueueCap more wait. Capacity is reserved before the
// accepted event is written and released if that write fails, so a full queue
// never leaves an accepted request without a lane.
type tradeFeedbackAnalysisManager struct {
	mu          sync.Mutex
	closed      bool
	waiting     int
	capacity    int
	queue       chan *tradeFeedbackAnalysisJob
	progress    map[string]string
	unresolved  map[string]string
	validate    tradeFeedbackValidator
	statusToken string
	ctx         context.Context
	cancel      context.CancelFunc
	wg          sync.WaitGroup
	started     bool
	stopped     chan struct{}
}

func newTradeFeedbackAnalysisManager(validate tradeFeedbackValidator, statusToken string) *tradeFeedbackAnalysisManager {
	ctx, cancel := context.WithCancel(context.Background())
	return &tradeFeedbackAnalysisManager{
		capacity:    tradeFeedbackAnalysisQueueCap,
		queue:       make(chan *tradeFeedbackAnalysisJob, tradeFeedbackAnalysisQueueCap),
		progress:    map[string]string{},
		unresolved:  map[string]string{},
		validate:    validate,
		statusToken: statusToken,
		ctx:         ctx,
		cancel:      cancel,
		stopped:     make(chan struct{}),
	}
}

func (m *tradeFeedbackAnalysisManager) start() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.started || m.closed {
		return
	}
	m.started = true
	m.wg.Add(1)
	go m.loop()
}

func (m *tradeFeedbackAnalysisManager) reserve() (*tradeFeedbackReservation, int, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || isDraining() {
		return nil, http.StatusServiceUnavailable, "the scheduler is draining; nothing was started"
	}
	if m.waiting >= m.capacity {
		return nil, http.StatusTooManyRequests, fmt.Sprintf("the analysis queue is full (%d waiting); nothing was started", m.waiting)
	}
	m.waiting++
	return &tradeFeedbackReservation{}, 0, ""
}

func (m *tradeFeedbackAnalysisManager) release(res *tradeFeedbackReservation) {
	if res == nil {
		return
	}
	m.mu.Lock()
	m.waiting--
	m.mu.Unlock()
}

func (m *tradeFeedbackAnalysisManager) enqueue(res *tradeFeedbackReservation, job *tradeFeedbackAnalysisJob) {
	m.mu.Lock()
	if m.closed {
		m.waiting--
		m.mu.Unlock()
		m.finish(job, tradeFeedbackAnalysisTerminal{Status: tradeFeedbackAnalysisFailed, Error: tradeFeedbackAnalysisShutdownMessage})
		return
	}
	m.progress[job.RequestID] = "queued"
	m.queue <- job
	m.mu.Unlock()
}

func (m *tradeFeedbackAnalysisManager) loop() {
	defer m.wg.Done()
	for {
		select {
		case <-m.ctx.Done():
			return
		case job := <-m.queue:
			m.mu.Lock()
			m.waiting--
			m.progress[job.RequestID] = "running"
			m.mu.Unlock()
			m.process(job)
		}
	}
}

// shutdown stops admissions, cancels the running job, joins the lane and
// writes a failed outcome for every queued job while the owning databases
// are still open. It runs before the state store closes.
func (m *tradeFeedbackAnalysisManager) shutdown() {
	if m == nil {
		return
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		<-m.stopped
		return
	}
	m.closed = true
	m.mu.Unlock()
	m.cancel()
	m.wg.Wait()
	var left []*tradeFeedbackAnalysisJob
	m.mu.Lock()
	for {
		select {
		case job := <-m.queue:
			m.waiting--
			left = append(left, job)
			continue
		default:
		}
		break
	}
	m.mu.Unlock()
	for _, job := range left {
		m.finish(job, tradeFeedbackAnalysisTerminal{Status: tradeFeedbackAnalysisFailed, Error: tradeFeedbackAnalysisShutdownMessage})
	}
	close(m.stopped)
}

func (m *tradeFeedbackAnalysisManager) progressSnapshot() map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]string, len(m.progress)+len(m.unresolved))
	for k, v := range m.progress {
		out[k] = v
	}
	for k, v := range m.unresolved {
		out[k] = "outcome write failed: " + v
	}
	return out
}

func (m *tradeFeedbackAnalysisManager) process(job *tradeFeedbackAnalysisJob) {
	ctx, cancel := context.WithDeadline(m.ctx, job.Deadline)
	defer cancel()
	fail := func(msg string) {
		m.finish(job, tradeFeedbackAnalysisTerminal{Status: tradeFeedbackAnalysisFailed, Error: msg})
	}
	cause := func(err error) string {
		switch {
		case errors.Is(m.ctx.Err(), context.Canceled):
			return tradeFeedbackAnalysisShutdownMessage
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			return fmt.Sprintf("the analysis passed its %ds deadline: %v", job.Settings.TimeoutS, err)
		}
		return err.Error()
	}
	if ctx.Err() != nil {
		fail(cause(ctx.Err()))
		return
	}
	out, err := job.Adapter.Run(ctx, job)
	if err != nil {
		term := tradeFeedbackAnalysisTerminal{Status: tradeFeedbackAnalysisFailed, Error: "provider: " + cause(err)}
		if len(out.Usage) > 0 && json.Valid(out.Usage) {
			term.UsageJSON = string(out.Usage)
		}
		m.finish(job, term)
		return
	}
	term := tradeFeedbackAnalysisTerminal{Model: out.Model, ModelVerification: out.ModelVerification}
	if len(out.Usage) > 0 && json.Valid(out.Usage) {
		term.UsageJSON = string(out.Usage)
	}
	raw := out.Result
	term.ResultJSON = truncateUTF8(tradeFeedbackRedact(string(raw), m.statusToken), tradeFeedbackAnalysisResultLimit)
	if len(raw) > tradeFeedbackAnalysisResultLimit {
		term.Status = tradeFeedbackAnalysisInvalid
		term.Error = fmt.Sprintf("the result is larger than %d bytes", tradeFeedbackAnalysisResultLimit)
		v, _ := json.Marshal(map[string]interface{}{"outcome": "invalid", "reasons": []string{term.Error}, "proposals": []interface{}{}})
		term.ValidationJSON = string(v)
		m.finish(job, term)
		return
	}
	wrapped, err := json.Marshal(string(raw))
	if err != nil {
		term.Status = tradeFeedbackAnalysisFailed
		term.Error = "encode the result for the validator: " + err.Error()
		m.finish(job, term)
		return
	}
	validation, outcome, err := m.validate(ctx, job.Bundle, wrapped, time.Until(job.Deadline))
	if err != nil {
		term.Status = tradeFeedbackAnalysisFailed
		term.Error = truncateUTF8(tradeFeedbackRedact("validator: "+cause(err), m.statusToken), tradeFeedbackAnalysisErrorLimit)
		m.finish(job, term)
		return
	}
	term.Status = outcome
	term.ValidationJSON = string(validation)
	m.finish(job, term)
}

func (m *tradeFeedbackAnalysisManager) finish(job *tradeFeedbackAnalysisJob, term tradeFeedbackAnalysisTerminal) {
	term.Error = truncateUTF8(tradeFeedbackRedact(term.Error, m.statusToken), tradeFeedbackAnalysisErrorLimit)
	if term.CreatedAt.IsZero() {
		term.CreatedAt = time.Now().UTC()
	}
	_, err := job.db.AppendTradeFeedbackAnalysisTerminal(job.RequestID, term)
	m.mu.Lock()
	delete(m.progress, job.RequestID)
	if err != nil {
		m.unresolved[job.RequestID] = tradeFeedbackRedact(err.Error(), m.statusToken)
	}
	m.mu.Unlock()
	if err != nil {
		log.Printf("[trade-feedback-analysis] %s %s: outcome write failed; the accepted request stays unresolved for restart recovery: %v", job.StrategyID, job.RequestID, err)
		return
	}
	log.Printf("[trade-feedback-analysis] %s %s: %s", job.StrategyID, job.RequestID, term.Status)
}

func runTradeFeedbackValidator(ctx context.Context, bundle []byte, result json.RawMessage, timeout time.Duration) ([]byte, string, error) {
	if timeout <= 0 {
		return nil, "", fmt.Errorf("no time left before the analysis deadline")
	}
	stdin, err := json.Marshal(struct {
		Bundle json.RawMessage `json:"bundle"`
		Result json.RawMessage `json:"result"`
	}{bundle, result})
	if err != nil {
		return nil, "", fmt.Errorf("encode validator input: %w", err)
	}
	stdout, stderr, runErr := spawnPythonProcessWithEnvLimited(ctx, tradeFeedbackValidatorScript, []string{"--validate-only"}, stdin, timeout, nil,
		pythonOutputLimits{Stdout: tradeFeedbackValidatorStdoutCap, Stderr: tradeFeedbackValidatorStderrCap})
	var resp struct {
		Error   string `json:"error"`
		Outcome string `json:"outcome"`
	}
	parseErr := json.Unmarshal(stdout, &resp)
	var overflow *pythonOutputOverflowError
	switch {
	case errors.As(runErr, &overflow):
		return nil, "", runErr
	case runErr != nil && parseErr == nil && resp.Error != "":
		return nil, "", fmt.Errorf("%s", resp.Error)
	case runErr != nil:
		return nil, "", fmt.Errorf("%v (stderr: %s)", runErr, firstLine(stderr))
	case parseErr != nil:
		return nil, "", fmt.Errorf("unreadable validator output: %v", parseErr)
	case resp.Outcome != tradeFeedbackAnalysisOK && resp.Outcome != tradeFeedbackAnalysisInvalid:
		return nil, "", fmt.Errorf("validator returned outcome %q", resp.Outcome)
	}
	return []byte(strings.TrimSpace(string(stdout))), resp.Outcome, nil
}

// startTradeFeedbackAnalysis recovers unresolved accepted requests, then
// starts the manager. A recovery error keeps the feature unavailable with a
// visible reason; it never blocks trading.
func startTradeFeedbackAnalysis(server *StatusServer, store *StateStore, statusToken string, validate tradeFeedbackValidator) *tradeFeedbackAnalysisManager {
	if server == nil {
		return nil
	}
	if server.feedbackAnalysisAdapters == nil {
		server.feedbackAnalysisAdapters = tradeFeedbackProductionAdapters(server)
	}
	if store == nil {
		server.feedbackAnalysisDown = "the state store is unavailable"
		return nil
	}
	for _, role := range store.order {
		if db := store.files[role]; db != nil && db.readOnly {
			server.feedbackAnalysisDown = "the state files are open read-only"
			return nil
		}
	}
	n, err := store.RecoverTradeFeedbackAnalyses(tradeFeedbackInterruptedError, time.Now().UTC())
	if n > 0 {
		log.Printf("[trade-feedback-analysis] recovered %d interrupted analysis request(s) as failed", n)
	}
	if err != nil {
		server.feedbackAnalysisDown = "restart recovery failed: " + err.Error()
		log.Printf("[trade-feedback-analysis] WARN: analysis stays unavailable: %v", err)
		return nil
	}
	m := newTradeFeedbackAnalysisManager(validate, statusToken)
	m.start()
	server.feedbackAnalysis = m
	return m
}

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

// pythonSemaphore limits concurrent Python subprocess executions.
var pythonSemaphore = make(chan struct{}, 4)

const scriptTimeout = 30 * time.Second

// SpotResult is the JSON output from check_strategy.py.
type SpotResult struct {
	Strategy   string                 `json:"strategy"`
	Symbol     string                 `json:"symbol"`
	Timeframe  string                 `json:"timeframe"`
	Signal     int                    `json:"signal"`
	Price      float64                `json:"price"`
	Indicators map[string]interface{} `json:"indicators"`
	Timestamp  string                 `json:"timestamp"`
	Error      string                 `json:"error,omitempty"`
}

// HyperliquidResult is the JSON output from check_hyperliquid.py (signal check mode).
type HyperliquidResult struct {
	Strategy   string                 `json:"strategy"`
	Symbol     string                 `json:"symbol"`
	Timeframe  string                 `json:"timeframe"`
	Signal     int                    `json:"signal"`
	Price      float64                `json:"price"`
	Indicators map[string]interface{} `json:"indicators"`
	Mode       string                 `json:"mode"`
	Platform   string                 `json:"platform"`
	Timestamp  string                 `json:"timestamp"`
	Error      string                 `json:"error,omitempty"`
}

// HyperliquidFill holds fill details from a live Hyperliquid order.
type HyperliquidFill struct {
	AvgPx             float64 `json:"avg_px"`
	TotalSz           float64 `json:"total_sz"`
	OID               int64   `json:"oid,omitempty"`                  // exchange order ID
	Fee               float64 `json:"fee,omitempty"`                  // exchange fee (if available)
	StopLossOID       int64   `json:"stop_loss_oid,omitempty"`        // resting trigger OID for the per-trade SL placed alongside the fill (#412)
	StopLossTriggerPx float64 `json:"stop_loss_trigger_px,omitempty"` // SL trigger price (for logs/audit) (#412)
}

// HyperliquidExecution is the execution block from check_hyperliquid.py --execute output.
type HyperliquidExecution struct {
	Action string           `json:"action"`
	Symbol string           `json:"symbol"`
	Size   float64          `json:"size"`
	Fill   *HyperliquidFill `json:"fill,omitempty"`
}

// HyperliquidExecuteResult is the top-level JSON from check_hyperliquid.py --execute.
type HyperliquidExecuteResult struct {
	Execution                 *HyperliquidExecution `json:"execution"`
	Platform                  string                `json:"platform"`
	Timestamp                 string                `json:"timestamp"`
	Error                     string                `json:"error,omitempty"`
	CancelStopLossError       string                `json:"cancel_stop_loss_error,omitempty"`       // non-fatal: SL cancel before order failed (#412)
	CancelStopLossSucceeded   bool                  `json:"cancel_stop_loss_succeeded,omitempty"`   // SL cancel went through (set even if subsequent open failed) so caller can clear stale pos.StopLossOID (#421)
	StopLossError             string                `json:"stop_loss_error,omitempty"`              // non-fatal: SL placement after fill failed (#412)
	StopLossFilledImmediately bool                  `json:"stop_loss_filled_immediately,omitempty"` // SL trigger filled at submit (price already through the level) — position is flat on-chain (#421)
}

// RunPythonScript executes a Python script and returns stdout/stderr.
func RunPythonScript(script string, args []string) ([]byte, []byte, error) {
	pythonSemaphore <- struct{}{}
	defer func() { <-pythonSemaphore }()

	ctx, cancel := context.WithTimeout(context.Background(), scriptTimeout)
	defer cancel()

	cmdArgs := append([]string{script}, args...)
	cmd := exec.CommandContext(ctx, ".venv/bin/python3", cmdArgs...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		if cmd.Process != nil {
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return stdout.Bytes(), stderr.Bytes(), fmt.Errorf("script timed out after %s", scriptTimeout)
	}
	return stdout.Bytes(), stderr.Bytes(), err
}

// RunSpotCheck runs check_strategy.py and parses the result.
func RunSpotCheck(script string, args []string) (*SpotResult, string, error) {
	stdout, stderr, err := RunPythonScript(script, args)
	stderrStr := string(stderr)
	if err != nil {
		// Try to parse JSON even on non-zero exit (script may exit(1) with JSON error output)
		var result SpotResult
		if jsonErr := json.Unmarshal(stdout, &result); jsonErr == nil && result.Error != "" {
			return &result, stderrStr, nil
		}
		return nil, stderrStr, fmt.Errorf("script error: %w (stderr: %s)", err, stderrStr)
	}

	var result SpotResult
	if err := json.Unmarshal(stdout, &result); err != nil {
		return nil, stderrStr, fmt.Errorf("parse output: %w (stdout: %s)", err, string(stdout))
	}
	return &result, stderrStr, nil
}

// RunPythonScriptWithStdin executes a Python script, piping stdinData to its stdin.
func RunPythonScriptWithStdin(script string, args []string, stdinData []byte) ([]byte, []byte, error) {
	pythonSemaphore <- struct{}{}
	defer func() { <-pythonSemaphore }()

	ctx, cancel := context.WithTimeout(context.Background(), scriptTimeout)
	defer cancel()

	cmdArgs := append([]string{script}, args...)
	cmd := exec.CommandContext(ctx, ".venv/bin/python3", cmdArgs...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdin = bytes.NewReader(stdinData)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		if cmd.Process != nil {
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return stdout.Bytes(), stderr.Bytes(), fmt.Errorf("script timed out after %s", scriptTimeout)
	}
	return stdout.Bytes(), stderr.Bytes(), err
}

// RunOptionsCheckWithStdin runs check_options.py, passing positionsJSON via stdin.
func RunOptionsCheckWithStdin(script string, args []string, positionsJSON string) (*OptionsResult, string, error) {
	stdout, stderr, err := RunPythonScriptWithStdin(script, args, []byte(positionsJSON))
	stderrStr := string(stderr)
	if err != nil {
		var result OptionsResult
		if jsonErr := json.Unmarshal(stdout, &result); jsonErr == nil && result.Error != "" {
			return &result, stderrStr, nil
		}
		return nil, stderrStr, fmt.Errorf("script error: %w (stderr: %s)", err, stderrStr)
	}

	var result OptionsResult
	if err := json.Unmarshal(stdout, &result); err != nil {
		return nil, stderrStr, fmt.Errorf("parse output: %w (stdout: %s)", err, string(stdout))
	}
	return &result, stderrStr, nil
}

// RunOptionsCheck runs check_options.py and parses the result.
func RunOptionsCheck(script string, args []string) (*OptionsResult, string, error) {
	stdout, stderr, err := RunPythonScript(script, args)
	stderrStr := string(stderr)
	if err != nil {
		// Try to parse JSON even on non-zero exit (script may exit(1) with JSON error output)
		var result OptionsResult
		if jsonErr := json.Unmarshal(stdout, &result); jsonErr == nil && result.Error != "" {
			return &result, stderrStr, nil
		}
		return nil, stderrStr, fmt.Errorf("script error: %w (stderr: %s)", err, stderrStr)
	}

	var result OptionsResult
	if err := json.Unmarshal(stdout, &result); err != nil {
		return nil, stderrStr, fmt.Errorf("parse output: %w (stdout: %s)", err, string(stdout))
	}
	return &result, stderrStr, nil
}

// RunHyperliquidCheck runs check_hyperliquid.py in signal check mode and parses the result.
func RunHyperliquidCheck(script string, args []string) (*HyperliquidResult, string, error) {
	stdout, stderr, err := RunPythonScript(script, args)
	stderrStr := string(stderr)
	if err != nil {
		var result HyperliquidResult
		if jsonErr := json.Unmarshal(stdout, &result); jsonErr == nil && result.Error != "" {
			return &result, stderrStr, nil
		}
		return nil, stderrStr, fmt.Errorf("script error: %w (stderr: %s)", err, stderrStr)
	}

	var result HyperliquidResult
	if err := json.Unmarshal(stdout, &result); err != nil {
		return nil, stderrStr, fmt.Errorf("parse output: %w (stdout: %s)", err, string(stdout))
	}
	return &result, stderrStr, nil
}

// RunHyperliquidExecute runs check_hyperliquid.py in execute mode (live orders).
// stopLossPct > 0 requests a reduce-only SL trigger after a successful open.
// cancelStopLossOID > 0 cancels an existing trigger before placing the new order
// (used on signal-based closes so the stale SL doesn't race the close fill).
// prevPosQty > 0 indicates a flip is in progress: total_sz from the fill is
// closeQty + newQty, and the SL must be sized against (total_sz - prevPosQty)
// or HL will reject the oversized reduce-only trigger (#421). These trailing
// args are HL-specific: OKX/TopStep have their own execute helpers and signatures.
func RunHyperliquidExecute(script, symbol, side string, size, stopLossPct float64, cancelStopLossOID int64, prevPosQty float64) (*HyperliquidExecuteResult, string, error) {
	args := []string{
		"--execute",
		fmt.Sprintf("--symbol=%s", symbol),
		fmt.Sprintf("--side=%s", side),
		fmt.Sprintf("--size=%g", size),
		"--mode=live",
	}
	if stopLossPct > 0 {
		args = append(args, fmt.Sprintf("--stop-loss-pct=%g", stopLossPct))
	}
	if cancelStopLossOID > 0 {
		args = append(args, fmt.Sprintf("--cancel-stop-loss-oid=%d", cancelStopLossOID))
	}
	if prevPosQty > 0 {
		args = append(args, fmt.Sprintf("--prev-pos-qty=%g", prevPosQty))
	}
	stdout, stderr, err := RunPythonScript(script, args)
	return parseHyperliquidExecuteOutput(stdout, string(stderr), err)
}

// parseHyperliquidExecuteOutput turns subprocess output into
// (*HyperliquidExecuteResult, stderr, error). Extracted from RunHyperliquidExecute
// so Go CI (no .venv) can test the parsing contract without spawning Python
// — same pattern as parseHyperliquidCloseOutput (#341).
func parseHyperliquidExecuteOutput(stdout []byte, stderrStr string, runErr error) (*HyperliquidExecuteResult, string, error) {
	if runErr != nil {
		var result HyperliquidExecuteResult
		if jsonErr := json.Unmarshal(stdout, &result); jsonErr == nil && result.Error != "" {
			return &result, stderrStr, nil
		}
		return nil, stderrStr, fmt.Errorf("execute error: %w (stderr: %s)", runErr, stderrStr)
	}

	var result HyperliquidExecuteResult
	if err := json.Unmarshal(stdout, &result); err != nil {
		return nil, stderrStr, fmt.Errorf("parse execute output: %w (stdout: %s)", err, string(stdout))
	}
	return &result, stderrStr, nil
}

// HyperliquidCloseFill is the parsed fill block from close_hyperliquid_position.py.
// Mirrors HyperliquidFill; Fee is included so kill-switch close accounting
// (fee totals, post-mortem PnL) can capture exchange fees just like the
// normal execute path does.
type HyperliquidCloseFill struct {
	AvgPx   float64 `json:"avg_px"`
	TotalSz float64 `json:"total_sz"`
	OID     int64   `json:"oid,omitempty"`
	Fee     float64 `json:"fee,omitempty"`
}

// HyperliquidClose is the close block from close_hyperliquid_position.py.
// AlreadyFlat is set by the Python script when the SDK reports an empty
// statuses list (no position to close at submit time, eventual-consistency
// window after the Go-side fetch). The Go side reads this to route the
// outcome through AlreadyFlat instead of ClosedCoins so operator messaging
// distinguishes "we sent a close order" from "nothing to close" (#350).
type HyperliquidClose struct {
	Symbol      string                `json:"symbol"`
	Fill        *HyperliquidCloseFill `json:"fill,omitempty"`
	AlreadyFlat bool                  `json:"already_flat,omitempty"`
}

// HyperliquidCloseResult is the top-level JSON from close_hyperliquid_position.py.
// Used by the portfolio kill switch to liquidate on-chain positions (#341).
// CancelStopLossSucceeded / CancelStopLossError surface the optional
// pre-close trigger-cancel that frees `Position.StopLossOID` when a CB or
// kill switch flattens a strategy carrying a resting SL (#421).
type HyperliquidCloseResult struct {
	Close                   *HyperliquidClose `json:"close"`
	Platform                string            `json:"platform"`
	Timestamp               string            `json:"timestamp"`
	Error                   string            `json:"error,omitempty"`
	CancelStopLossError     string            `json:"cancel_stop_loss_error,omitempty"`
	CancelStopLossSucceeded bool              `json:"cancel_stop_loss_succeeded,omitempty"`
}

// RunHyperliquidClose runs close_hyperliquid_position.py to submit a reduce-only
// market close for a single coin (#341). When partialSz is non-nil, submits a
// partial close for that coin quantity (#356 shared-wallet circuit breakers).
// When cancelStopLossOIDs is non-empty, the script first cancels those resting
// trigger orders so per-strategy CB / kill-switch closes don't leave orphaned SLs
// burning HL's open-order cap (#421, #479).
//
// Contract (load-bearing for kill-switch correctness): a non-nil error is
// returned for ANY failure path — non-zero subprocess exit, malformed JSON,
// or a JSON envelope with `error` populated. Callers that see (result, nil)
// can treat the close as confirmed by the SDK. The previous contract returned
// (result, nil) for "exit 1 + parseable JSON with error" which forced every
// caller to also inspect result.Error and conflated subprocess success with
// JSON-error success.
func RunHyperliquidClose(script, symbol string, partialSz *float64, cancelStopLossOIDs []int64) (*HyperliquidCloseResult, string, error) {
	args := []string{
		fmt.Sprintf("--symbol=%s", symbol),
		"--mode=live",
	}
	if partialSz != nil {
		args = append(args, fmt.Sprintf("--sz=%s", strconv.FormatFloat(*partialSz, 'f', -1, 64)))
	}
	for _, oid := range cancelStopLossOIDs {
		if oid > 0 {
			args = append(args, fmt.Sprintf("--cancel-stop-loss-oid=%d", oid))
		}
	}
	stdout, stderr, runErr := RunPythonScript(script, args)
	return parseHyperliquidCloseOutput(stdout, string(stderr), runErr)
}

// parseHyperliquidCloseOutput turns the raw subprocess result into
// (*HyperliquidCloseResult, stderr, error) following the RunHyperliquidClose
// contract. Extracted from RunHyperliquidClose so the decision logic can be
// tested without spawning .venv/bin/python3 (absent in the Go CI job).
func parseHyperliquidCloseOutput(stdout []byte, stderrStr string, runErr error) (*HyperliquidCloseResult, string, error) {
	var result HyperliquidCloseResult
	parseErr := json.Unmarshal(stdout, &result)

	switch {
	case runErr == nil && parseErr == nil && result.Error == "":
		// Clean success: exit 0, valid JSON, no error field.
		return &result, stderrStr, nil

	case runErr == nil && parseErr == nil && result.Error != "":
		// Exit 0 but the script reported an error — should not happen with
		// the current Python contract (every error path also exits 1) but we
		// honor the JSON envelope as authoritative.
		return &result, stderrStr, fmt.Errorf("close reported error despite exit 0: %s", result.Error)

	case parseErr == nil && result.Error != "":
		// Exit non-zero with valid JSON error envelope — the expected error
		// path. Surface as a non-nil error so callers don't need to also
		// check result.Error.
		return &result, stderrStr, fmt.Errorf("close failed: %s", result.Error)

	case parseErr == nil && runErr != nil:
		// Exit non-zero with valid JSON but no error field — unexpected. Treat
		// as failure to avoid silently reporting success on a non-zero exit.
		return &result, stderrStr, fmt.Errorf("close subprocess exit %v with no error field (stderr: %s)", runErr, stderrStr)

	default:
		// Malformed JSON. Always a failure regardless of exit code.
		return nil, stderrStr, fmt.Errorf("parse close output: %v (run err: %v, stdout: %s)", parseErr, runErr, string(stdout))
	}
}

// ContractSpec holds CME futures contract specifications from check_topstep.py.
type ContractSpec struct {
	TickSize   float64 `json:"tick_size"`
	TickValue  float64 `json:"tick_value"`
	Multiplier float64 `json:"multiplier"`
	Margin     float64 `json:"margin"`
}

// TopStepResult is the JSON output from check_topstep.py (signal check mode).
type TopStepResult struct {
	Strategy     string                 `json:"strategy"`
	Symbol       string                 `json:"symbol"`
	Timeframe    string                 `json:"timeframe"`
	Signal       int                    `json:"signal"`
	Price        float64                `json:"price"`
	ContractSpec ContractSpec           `json:"contract_spec"`
	MarketOpen   bool                   `json:"market_open"`
	Indicators   map[string]interface{} `json:"indicators"`
	Mode         string                 `json:"mode"`
	Platform     string                 `json:"platform"`
	Timestamp    string                 `json:"timestamp"`
	Error        string                 `json:"error,omitempty"`
}

// TopStepFill holds fill details from a live TopStep order.
type TopStepFill struct {
	AvgPx          float64 `json:"avg_px"`
	TotalContracts int     `json:"total_contracts"`
	OID            string  `json:"oid,omitempty"`
	Fee            float64 `json:"fee,omitempty"`
}

// TopStepExecution is the execution block from check_topstep.py --execute output.
type TopStepExecution struct {
	Action    string       `json:"action"`
	Symbol    string       `json:"symbol"`
	Contracts int          `json:"contracts"`
	Fill      *TopStepFill `json:"fill,omitempty"`
}

// TopStepExecuteResult is the top-level JSON from check_topstep.py --execute.
type TopStepExecuteResult struct {
	Execution *TopStepExecution `json:"execution"`
	Platform  string            `json:"platform"`
	Timestamp string            `json:"timestamp"`
	Error     string            `json:"error,omitempty"`
}

// RunTopStepCheck runs check_topstep.py in signal check mode and parses the result.
func RunTopStepCheck(script string, args []string) (*TopStepResult, string, error) {
	stdout, stderr, err := RunPythonScript(script, args)
	stderrStr := string(stderr)
	if err != nil {
		var result TopStepResult
		if jsonErr := json.Unmarshal(stdout, &result); jsonErr == nil && result.Error != "" {
			return &result, stderrStr, nil
		}
		return nil, stderrStr, fmt.Errorf("script error: %w (stderr: %s)", err, stderrStr)
	}

	var result TopStepResult
	if err := json.Unmarshal(stdout, &result); err != nil {
		return nil, stderrStr, fmt.Errorf("parse output: %w (stdout: %s)", err, string(stdout))
	}
	return &result, stderrStr, nil
}

// RunTopStepExecute runs check_topstep.py in execute mode (live orders).
func RunTopStepExecute(script, symbol, side string, contracts int) (*TopStepExecuteResult, string, error) {
	args := []string{
		"--execute",
		fmt.Sprintf("--symbol=%s", symbol),
		fmt.Sprintf("--side=%s", side),
		fmt.Sprintf("--contracts=%d", contracts),
		"--mode=live",
	}
	stdout, stderr, err := RunPythonScript(script, args)
	stderrStr := string(stderr)
	if err != nil {
		var result TopStepExecuteResult
		if jsonErr := json.Unmarshal(stdout, &result); jsonErr == nil && result.Error != "" {
			return &result, stderrStr, nil
		}
		return nil, stderrStr, fmt.Errorf("execute error: %w (stderr: %s)", err, stderrStr)
	}

	var result TopStepExecuteResult
	if err := json.Unmarshal(stdout, &result); err != nil {
		return nil, stderrStr, fmt.Errorf("parse execute output: %w (stdout: %s)", err, string(stdout))
	}
	return &result, stderrStr, nil
}

// TopStepCloseFill is the parsed fill block from close_topstep_position.py.
// Mirrors TopStepFill but fields are optional (empty {} means already-flat
// success) and OID is a string (TopStepX order IDs are opaque).
type TopStepCloseFill struct {
	AvgPx          float64 `json:"avg_px,omitempty"`
	TotalContracts int     `json:"total_contracts,omitempty"`
	OID            string  `json:"oid,omitempty"`
}

// TopStepClose is the close block from close_topstep_position.py.
type TopStepClose struct {
	Symbol string            `json:"symbol"`
	Fill   *TopStepCloseFill `json:"fill,omitempty"`
}

// TopStepCloseResult is the top-level JSON from close_topstep_position.py.
// Used by the portfolio kill switch to liquidate live TopStep futures
// exposure (#347).
type TopStepCloseResult struct {
	Close     *TopStepClose `json:"close"`
	Platform  string        `json:"platform"`
	Timestamp string        `json:"timestamp"`
	Error     string        `json:"error,omitempty"`
}

// RunTopStepClose runs close_topstep_position.py to submit a market-flatten
// for a single TopStep futures symbol (#347). Contract mirrors
// RunHyperliquidClose / RunOKXClose / RunRobinhoodClose: any failure path
// returns a non-nil error so the kill switch stays latched on ambiguous
// responses.
func RunTopStepClose(script, symbol string) (*TopStepCloseResult, string, error) {
	args := []string{
		fmt.Sprintf("--symbol=%s", symbol),
		"--mode=live",
	}
	stdout, stderr, runErr := RunPythonScript(script, args)
	return parseTopStepCloseOutput(stdout, string(stderr), runErr)
}

// parseTopStepCloseOutput turns raw subprocess output into
// (*TopStepCloseResult, stderr, error) following the RunTopStepClose
// contract. Extracted so decision logic can be tested without spawning
// .venv/bin/python3 — same pattern as parseHyperliquidCloseOutput /
// parseOKXCloseOutput / parseRobinhoodCloseOutput (#341/#342/#345/#346).
func parseTopStepCloseOutput(stdout []byte, stderrStr string, runErr error) (*TopStepCloseResult, string, error) {
	var result TopStepCloseResult
	parseErr := json.Unmarshal(stdout, &result)

	switch {
	case runErr == nil && parseErr == nil && result.Error == "":
		return &result, stderrStr, nil

	case runErr == nil && parseErr == nil && result.Error != "":
		return &result, stderrStr, fmt.Errorf("close reported error despite exit 0: %s", result.Error)

	case parseErr == nil && result.Error != "":
		return &result, stderrStr, fmt.Errorf("close failed: %s", result.Error)

	case parseErr == nil && runErr != nil:
		return &result, stderrStr, fmt.Errorf("close subprocess exit %v with no error field (stderr: %s)", runErr, stderrStr)

	default:
		return nil, stderrStr, fmt.Errorf("parse close output: %v (run err: %v, stdout: %s)", parseErr, runErr, string(stdout))
	}
}

// TopStepPositionsResult is the JSON output from fetch_topstep_positions.py.
// Size is signed (positive = long, negative = short) to mirror OKX/HL so
// the kill-switch plan builder can treat all platforms symmetrically.
type TopStepPositionsResult struct {
	Positions []TopStepPositionJSON `json:"positions"`
	Platform  string                `json:"platform"`
	Timestamp string                `json:"timestamp"`
	Error     string                `json:"error,omitempty"`
}

// TopStepPositionJSON is the per-position payload from
// fetch_topstep_positions.py. Size is integer contracts (futures have no
// fractional sizing).
type TopStepPositionJSON struct {
	Coin     string  `json:"coin"`
	Size     int     `json:"size"`
	AvgPrice float64 `json:"avg_price"`
	Side     string  `json:"side"`
}

// RunTopStepFetchPositions runs fetch_topstep_positions.py and returns the
// parsed result (#347). Like RunTopStepClose, any failure path returns a
// non-nil error so the kill switch can latch and retry — a silent parse
// failure would otherwise look like "no positions" and clear virtual state
// while live exposure remained.
func RunTopStepFetchPositions(script string) (*TopStepPositionsResult, string, error) {
	stdout, stderr, runErr := RunPythonScript(script, nil)
	return parseTopStepPositionsOutput(stdout, string(stderr), runErr)
}

// parseTopStepPositionsOutput is the pure parser, extracted from
// RunTopStepFetchPositions so decision logic can be tested without
// spawning Python. Mirrors parseOKXPositionsOutput / parseRobinhoodPositionsOutput
// 5-case matrix.
func parseTopStepPositionsOutput(stdout []byte, stderrStr string, runErr error) (*TopStepPositionsResult, string, error) {
	var result TopStepPositionsResult
	parseErr := json.Unmarshal(stdout, &result)

	switch {
	case runErr == nil && parseErr == nil && result.Error == "":
		return &result, stderrStr, nil

	case runErr == nil && parseErr == nil && result.Error != "":
		return &result, stderrStr, fmt.Errorf("fetch positions reported error despite exit 0: %s", result.Error)

	case parseErr == nil && result.Error != "":
		return &result, stderrStr, fmt.Errorf("fetch positions failed: %s", result.Error)

	case parseErr == nil && runErr != nil:
		return &result, stderrStr, fmt.Errorf("fetch positions subprocess exit %v with no error field (stderr: %s)", runErr, stderrStr)

	default:
		return nil, stderrStr, fmt.Errorf("parse positions output: %v (run err: %v, stdout: %s)", parseErr, runErr, string(stdout))
	}
}

// RobinhoodResult is the JSON output from check_robinhood.py (signal check mode).
type RobinhoodResult struct {
	Strategy   string                 `json:"strategy"`
	Symbol     string                 `json:"symbol"`
	Timeframe  string                 `json:"timeframe"`
	Signal     int                    `json:"signal"`
	Price      float64                `json:"price"`
	Indicators map[string]interface{} `json:"indicators"`
	Mode       string                 `json:"mode"`
	Platform   string                 `json:"platform"`
	Timestamp  string                 `json:"timestamp"`
	Error      string                 `json:"error,omitempty"`
}

// RobinhoodFill holds fill details from a live Robinhood order.
type RobinhoodFill struct {
	AvgPx    float64 `json:"avg_px"`
	Quantity float64 `json:"quantity"`
	OID      string  `json:"oid,omitempty"`
	Fee      float64 `json:"fee,omitempty"`
}

// RobinhoodExecution is the execution block from check_robinhood.py --execute output.
type RobinhoodExecution struct {
	Action    string         `json:"action"`
	Symbol    string         `json:"symbol"`
	AmountUSD float64        `json:"amount_usd,omitempty"`
	Quantity  float64        `json:"quantity,omitempty"`
	Fill      *RobinhoodFill `json:"fill,omitempty"`
}

// RobinhoodExecuteResult is the top-level JSON from check_robinhood.py --execute.
type RobinhoodExecuteResult struct {
	Execution *RobinhoodExecution `json:"execution"`
	Platform  string              `json:"platform"`
	Timestamp string              `json:"timestamp"`
	Error     string              `json:"error,omitempty"`
}

// RunRobinhoodCheck runs check_robinhood.py in signal check mode and parses the result.
func RunRobinhoodCheck(script string, args []string) (*RobinhoodResult, string, error) {
	stdout, stderr, err := RunPythonScript(script, args)
	stderrStr := string(stderr)
	if err != nil {
		var result RobinhoodResult
		if jsonErr := json.Unmarshal(stdout, &result); jsonErr == nil && result.Error != "" {
			return &result, stderrStr, nil
		}
		return nil, stderrStr, fmt.Errorf("script error: %w (stderr: %s)", err, stderrStr)
	}

	var result RobinhoodResult
	if err := json.Unmarshal(stdout, &result); err != nil {
		return nil, stderrStr, fmt.Errorf("parse output: %w (stdout: %s)", err, string(stdout))
	}
	return &result, stderrStr, nil
}

// RunRobinhoodExecute runs check_robinhood.py in execute mode (live orders).
func RunRobinhoodExecute(script, symbol, side string, amountUSD, quantity float64) (*RobinhoodExecuteResult, string, error) {
	args := []string{
		"--execute",
		fmt.Sprintf("--symbol=%s", symbol),
		fmt.Sprintf("--side=%s", side),
		"--mode=live",
	}
	if side == "buy" {
		args = append(args, fmt.Sprintf("--amount_usd=%g", amountUSD))
	} else {
		args = append(args, fmt.Sprintf("--quantity=%g", quantity))
	}
	stdout, stderr, err := RunPythonScript(script, args)
	stderrStr := string(stderr)
	if err != nil {
		var result RobinhoodExecuteResult
		if jsonErr := json.Unmarshal(stdout, &result); jsonErr == nil && result.Error != "" {
			return &result, stderrStr, nil
		}
		return nil, stderrStr, fmt.Errorf("execute error: %w (stderr: %s)", err, stderrStr)
	}

	var result RobinhoodExecuteResult
	if err := json.Unmarshal(stdout, &result); err != nil {
		return nil, stderrStr, fmt.Errorf("parse execute output: %w (stdout: %s)", err, string(stdout))
	}
	return &result, stderrStr, nil
}

// OKXResult is the JSON output from check_okx.py (signal check mode).
type OKXResult struct {
	Strategy   string                 `json:"strategy"`
	Symbol     string                 `json:"symbol"`
	Timeframe  string                 `json:"timeframe"`
	Signal     int                    `json:"signal"`
	Price      float64                `json:"price"`
	Indicators map[string]interface{} `json:"indicators"`
	Mode       string                 `json:"mode"`
	Platform   string                 `json:"platform"`
	Timestamp  string                 `json:"timestamp"`
	Error      string                 `json:"error,omitempty"`
}

// OKXFill holds fill details from a live OKX order.
type OKXFill struct {
	AvgPx   float64 `json:"avg_px"`
	TotalSz float64 `json:"total_sz"`
	OID     string  `json:"oid,omitempty"`
	Fee     float64 `json:"fee,omitempty"`
}

// OKXExecution is the execution block from check_okx.py --execute output.
type OKXExecution struct {
	Action string   `json:"action"`
	Symbol string   `json:"symbol"`
	Size   float64  `json:"size"`
	Fill   *OKXFill `json:"fill,omitempty"`
}

// OKXExecuteResult is the top-level JSON from check_okx.py --execute.
type OKXExecuteResult struct {
	Execution *OKXExecution `json:"execution"`
	Platform  string        `json:"platform"`
	Timestamp string        `json:"timestamp"`
	Error     string        `json:"error,omitempty"`
}

// RunOKXCheck runs check_okx.py in signal check mode and parses the result.
func RunOKXCheck(script string, args []string) (*OKXResult, string, error) {
	stdout, stderr, err := RunPythonScript(script, args)
	stderrStr := string(stderr)
	if err != nil {
		var result OKXResult
		if jsonErr := json.Unmarshal(stdout, &result); jsonErr == nil && result.Error != "" {
			return &result, stderrStr, nil
		}
		return nil, stderrStr, fmt.Errorf("script error: %w (stderr: %s)", err, stderrStr)
	}

	var result OKXResult
	if err := json.Unmarshal(stdout, &result); err != nil {
		return nil, stderrStr, fmt.Errorf("parse output: %w (stdout: %s)", err, string(stdout))
	}
	return &result, stderrStr, nil
}

// RunOKXExecute runs check_okx.py in execute mode (live orders).
func RunOKXExecute(script, symbol, side string, size float64, instType string) (*OKXExecuteResult, string, error) {
	args := []string{
		"--execute",
		fmt.Sprintf("--symbol=%s", symbol),
		fmt.Sprintf("--side=%s", side),
		fmt.Sprintf("--size=%g", size),
		"--mode=live",
		fmt.Sprintf("--inst-type=%s", instType),
	}
	stdout, stderr, err := RunPythonScript(script, args)
	stderrStr := string(stderr)
	if err != nil {
		var result OKXExecuteResult
		if jsonErr := json.Unmarshal(stdout, &result); jsonErr == nil && result.Error != "" {
			return &result, stderrStr, nil
		}
		return nil, stderrStr, fmt.Errorf("execute error: %w (stderr: %s)", err, stderrStr)
	}

	var result OKXExecuteResult
	if err := json.Unmarshal(stdout, &result); err != nil {
		return nil, stderrStr, fmt.Errorf("parse execute output: %w (stdout: %s)", err, string(stdout))
	}
	return &result, stderrStr, nil
}

// OKXCloseFill is the parsed fill block from close_okx_position.py.
// Mirrors HyperliquidCloseFill so kill-switch accounting is symmetric
// across platforms. OID is a string (ccxt order IDs are opaque strings,
// unlike HL's int64) and all fields are optional — empty {} means the
// adapter found no position to close (already-flat success).
type OKXCloseFill struct {
	AvgPx   float64 `json:"avg_px,omitempty"`
	TotalSz float64 `json:"total_sz,omitempty"`
	OID     string  `json:"oid,omitempty"`
	Fee     float64 `json:"fee,omitempty"`
}

// OKXClose is the close block from close_okx_position.py. AlreadyFlat is
// set by the Python script when adapter.market_close returns {} (adapter
// found no open position at submit time, eventual-consistency window
// after the Go-side fetch). See HyperliquidClose for the reasoning (#350).
type OKXClose struct {
	Symbol      string        `json:"symbol"`
	Fill        *OKXCloseFill `json:"fill,omitempty"`
	AlreadyFlat bool          `json:"already_flat,omitempty"`
}

// OKXCloseResult is the top-level JSON from close_okx_position.py.
// Used by the portfolio kill switch to liquidate on-chain OKX perps
// positions (#345).
type OKXCloseResult struct {
	Close     *OKXClose `json:"close"`
	Platform  string    `json:"platform"`
	Timestamp string    `json:"timestamp"`
	Error     string    `json:"error,omitempty"`
}

// RunOKXClose runs close_okx_position.py to submit a reduce-only market
// close for a single OKX swap coin (#345). When partialSz is non-nil, submits
// a partial reduce-only close for that coin quantity (#360 shared-wallet
// per-strategy circuit breakers).
//
// Contract mirrors RunHyperliquidClose: a non-nil error is returned for
// ANY failure — non-zero subprocess exit, malformed JSON, or a JSON
// envelope with `error` populated. Callers that see (result, nil) can
// treat the close as confirmed by the adapter. Kill-switch correctness
// depends on this: any ambiguous response must surface as error so the
// switch stays latched and retries next cycle.
func RunOKXClose(script, symbol string, partialSz *float64) (*OKXCloseResult, string, error) {
	args := []string{
		fmt.Sprintf("--symbol=%s", symbol),
		"--mode=live",
	}
	if partialSz != nil {
		args = append(args, fmt.Sprintf("--sz=%s", strconv.FormatFloat(*partialSz, 'f', -1, 64)))
	}
	stdout, stderr, runErr := RunPythonScript(script, args)
	return parseOKXCloseOutput(stdout, string(stderr), runErr)
}

// parseOKXCloseOutput turns raw subprocess output into
// (*OKXCloseResult, stderr, error) following the RunOKXClose contract.
// Extracted so the decision logic can be tested without spawning
// .venv/bin/python3 (absent in the Go CI job — same reason as
// parseHyperliquidCloseOutput, #341/#342).
func parseOKXCloseOutput(stdout []byte, stderrStr string, runErr error) (*OKXCloseResult, string, error) {
	var result OKXCloseResult
	parseErr := json.Unmarshal(stdout, &result)

	switch {
	case runErr == nil && parseErr == nil && result.Error == "":
		return &result, stderrStr, nil

	case runErr == nil && parseErr == nil && result.Error != "":
		return &result, stderrStr, fmt.Errorf("close reported error despite exit 0: %s", result.Error)

	case parseErr == nil && result.Error != "":
		return &result, stderrStr, fmt.Errorf("close failed: %s", result.Error)

	case parseErr == nil && runErr != nil:
		return &result, stderrStr, fmt.Errorf("close subprocess exit %v with no error field (stderr: %s)", runErr, stderrStr)

	default:
		return nil, stderrStr, fmt.Errorf("parse close output: %v (run err: %v, stdout: %s)", parseErr, runErr, string(stdout))
	}
}

// OKXPositionsResult is the JSON output from fetch_okx_positions.py.
// Size is signed (positive = long, negative = short) to mirror HLPosition.
type OKXPositionsResult struct {
	Positions []OKXPositionJSON `json:"positions"`
	Platform  string            `json:"platform"`
	Timestamp string            `json:"timestamp"`
	Error     string            `json:"error,omitempty"`
}

// OKXPositionJSON is the per-position payload from fetch_okx_positions.py.
type OKXPositionJSON struct {
	Coin       string  `json:"coin"`
	Size       float64 `json:"size"`
	EntryPrice float64 `json:"entry_price"`
	Side       string  `json:"side"`
}

// RunOKXFetchPositions runs fetch_okx_positions.py and returns the parsed
// result (#345). Like RunOKXClose, any failure path returns a non-nil
// error so the kill switch can latch and retry — a silent parse failure
// would otherwise look like "no positions" and clear virtual state while
// on-chain exposure remained.
func RunOKXFetchPositions(script string) (*OKXPositionsResult, string, error) {
	stdout, stderr, runErr := RunPythonScript(script, nil)
	return parseOKXPositionsOutput(stdout, string(stderr), runErr)
}

// parseOKXPositionsOutput is the pure parser, extracted from
// RunOKXFetchPositions so the decision logic can be tested without
// spawning Python. Mirrors parseOKXCloseOutput / parseHyperliquidCloseOutput
// 5-case matrix (contract drift here would be bad — the kill switch reads
// every parser result the same way).
func parseOKXPositionsOutput(stdout []byte, stderrStr string, runErr error) (*OKXPositionsResult, string, error) {
	var result OKXPositionsResult
	parseErr := json.Unmarshal(stdout, &result)

	switch {
	case runErr == nil && parseErr == nil && result.Error == "":
		// Clean success: exit 0, valid JSON, no error field.
		return &result, stderrStr, nil

	case runErr == nil && parseErr == nil && result.Error != "":
		// Exit 0 but the script reported an error — shouldn't happen with
		// the current Python contract (every error path exits 1) but we
		// honor the JSON envelope as authoritative and surface it as a
		// contract-drift diagnostic.
		return &result, stderrStr, fmt.Errorf("fetch positions reported error despite exit 0: %s", result.Error)

	case parseErr == nil && result.Error != "":
		// Expected error path: exit non-zero, valid JSON envelope. Surface
		// as a non-nil error so callers don't need to double-check.
		return &result, stderrStr, fmt.Errorf("fetch positions failed: %s", result.Error)

	case parseErr == nil && runErr != nil:
		// Exit non-zero with valid JSON but no error field — unexpected.
		// Treat as failure to avoid silently reporting "no positions" on a
		// non-zero exit (kill switch would clear virtual state while
		// on-chain exposure remained — the #345 bug class).
		return &result, stderrStr, fmt.Errorf("fetch positions subprocess exit %v with no error field (stderr: %s)", runErr, stderrStr)

	default:
		return nil, stderrStr, fmt.Errorf("parse positions output: %v (run err: %v, stdout: %s)", parseErr, runErr, string(stdout))
	}
}

// OKXBalanceResult is the JSON output from fetch_okx_balance.py (#360).
type OKXBalanceResult struct {
	Balance   float64 `json:"balance"`
	Platform  string  `json:"platform"`
	Timestamp string  `json:"timestamp"`
	Error     string  `json:"error,omitempty"`
}

// RunOKXFetchBalance runs fetch_okx_balance.py and returns the parsed result
// (#360 phase 2 of #357). Used by defaultSharedWalletBalance to unlock
// multi-strategy OKX portfolio value correctness. Follows the same
// contract as RunOKXClose / RunOKXFetchPositions: non-nil error on ANY
// failure path so callers preserve the kill switch on uncertainty.
func RunOKXFetchBalance(script string) (*OKXBalanceResult, string, error) {
	stdout, stderr, runErr := RunPythonScript(script, nil)
	return parseOKXBalanceOutput(stdout, string(stderr), runErr)
}

// parseOKXBalanceOutput is the pure parser for RunOKXFetchBalance. Extracted
// so the decision logic can be tested without spawning .venv/bin/python3
// (absent in the Go CI job). Mirrors parseOKXPositionsOutput's 5-case
// matrix — contract drift across fetch parsers would be bad.
func parseOKXBalanceOutput(stdout []byte, stderrStr string, runErr error) (*OKXBalanceResult, string, error) {
	var result OKXBalanceResult
	parseErr := json.Unmarshal(stdout, &result)

	switch {
	case runErr == nil && parseErr == nil && result.Error == "":
		return &result, stderrStr, nil

	case runErr == nil && parseErr == nil && result.Error != "":
		return &result, stderrStr, fmt.Errorf("fetch balance reported error despite exit 0: %s", result.Error)

	case parseErr == nil && result.Error != "":
		return &result, stderrStr, fmt.Errorf("fetch balance failed: %s", result.Error)

	case parseErr == nil && runErr != nil:
		return &result, stderrStr, fmt.Errorf("fetch balance subprocess exit %v with no error field (stderr: %s)", runErr, stderrStr)

	default:
		return nil, stderrStr, fmt.Errorf("parse balance output: %v (run err: %v, stdout: %s)", parseErr, runErr, string(stdout))
	}
}

// RobinhoodCloseFill is the parsed fill block from close_robinhood_position.py.
// Mirrors OKXCloseFill. OID is a string (robin_stocks order IDs are opaque
// UUIDs), fields are optional — empty {} means the adapter found no position
// to close (already-flat success).
type RobinhoodCloseFill struct {
	AvgPx   float64 `json:"avg_px,omitempty"`
	TotalSz float64 `json:"total_sz,omitempty"`
	OID     string  `json:"oid,omitempty"`
}

// RobinhoodClose is the close block from close_robinhood_position.py.
// AlreadyFlat is set by the Python script when adapter.get_crypto_positions
// returns qty<=0 at submit time (eventual-consistency window after the
// Go-side fetch). See HyperliquidClose for the reasoning (#350).
type RobinhoodClose struct {
	Symbol      string              `json:"symbol"`
	Fill        *RobinhoodCloseFill `json:"fill,omitempty"`
	AlreadyFlat bool                `json:"already_flat,omitempty"`
}

// RobinhoodCloseResult is the top-level JSON from close_robinhood_position.py.
// Used by the portfolio kill switch to liquidate live Robinhood crypto
// exposure (#346).
type RobinhoodCloseResult struct {
	Close     *RobinhoodClose `json:"close"`
	Platform  string          `json:"platform"`
	Timestamp string          `json:"timestamp"`
	Error     string          `json:"error,omitempty"`
}

// RunRobinhoodClose runs close_robinhood_position.py to submit a market close
// for a single Robinhood crypto coin (#346). Contract mirrors
// RunHyperliquidClose / RunOKXClose: any failure path returns a non-nil
// error so the kill switch stays latched on ambiguous responses.
func RunRobinhoodClose(script, symbol string) (*RobinhoodCloseResult, string, error) {
	args := []string{
		fmt.Sprintf("--symbol=%s", symbol),
		"--mode=live",
	}
	stdout, stderr, runErr := RunPythonScript(script, args)
	return parseRobinhoodCloseOutput(stdout, string(stderr), runErr)
}

// parseRobinhoodCloseOutput turns raw subprocess output into
// (*RobinhoodCloseResult, stderr, error) following the RunRobinhoodClose
// contract. Extracted so decision logic can be tested without spawning
// .venv/bin/python3 — same pattern as parseHyperliquidCloseOutput /
// parseOKXCloseOutput (#341/#342/#345).
func parseRobinhoodCloseOutput(stdout []byte, stderrStr string, runErr error) (*RobinhoodCloseResult, string, error) {
	var result RobinhoodCloseResult
	parseErr := json.Unmarshal(stdout, &result)

	switch {
	case runErr == nil && parseErr == nil && result.Error == "":
		return &result, stderrStr, nil

	case runErr == nil && parseErr == nil && result.Error != "":
		return &result, stderrStr, fmt.Errorf("close reported error despite exit 0: %s", result.Error)

	case parseErr == nil && result.Error != "":
		return &result, stderrStr, fmt.Errorf("close failed: %s", result.Error)

	case parseErr == nil && runErr != nil:
		return &result, stderrStr, fmt.Errorf("close subprocess exit %v with no error field (stderr: %s)", runErr, stderrStr)

	default:
		return nil, stderrStr, fmt.Errorf("parse close output: %v (run err: %v, stdout: %s)", parseErr, runErr, string(stdout))
	}
}

// RobinhoodPositionsResult is the JSON output from fetch_robinhood_positions.py.
// Size is unsigned — Robinhood crypto is spot, no short positions.
type RobinhoodPositionsResult struct {
	Positions []RobinhoodPositionJSON `json:"positions"`
	Platform  string                  `json:"platform"`
	Timestamp string                  `json:"timestamp"`
	Error     string                  `json:"error,omitempty"`
}

// RobinhoodPositionJSON is the per-position payload from
// fetch_robinhood_positions.py.
type RobinhoodPositionJSON struct {
	Coin     string  `json:"coin"`
	Size     float64 `json:"size"`
	AvgPrice float64 `json:"avg_price"`
}

// RunRobinhoodFetchPositions runs fetch_robinhood_positions.py and returns
// the parsed result (#346). Like RunRobinhoodClose, any failure path
// returns a non-nil error so the kill switch can latch and retry.
func RunRobinhoodFetchPositions(script string) (*RobinhoodPositionsResult, string, error) {
	stdout, stderr, runErr := RunPythonScript(script, nil)
	return parseRobinhoodPositionsOutput(stdout, string(stderr), runErr)
}

// parseRobinhoodPositionsOutput is the pure parser, extracted from
// RunRobinhoodFetchPositions so the decision logic can be tested without
// spawning Python. Mirrors parseOKXPositionsOutput's 5-case matrix.
func parseRobinhoodPositionsOutput(stdout []byte, stderrStr string, runErr error) (*RobinhoodPositionsResult, string, error) {
	var result RobinhoodPositionsResult
	parseErr := json.Unmarshal(stdout, &result)

	switch {
	case runErr == nil && parseErr == nil && result.Error == "":
		return &result, stderrStr, nil

	case runErr == nil && parseErr == nil && result.Error != "":
		return &result, stderrStr, fmt.Errorf("fetch positions reported error despite exit 0: %s", result.Error)

	case parseErr == nil && result.Error != "":
		return &result, stderrStr, fmt.Errorf("fetch positions failed: %s", result.Error)

	case parseErr == nil && runErr != nil:
		return &result, stderrStr, fmt.Errorf("fetch positions subprocess exit %v with no error field (stderr: %s)", runErr, stderrStr)

	default:
		return nil, stderrStr, fmt.Errorf("parse positions output: %v (run err: %v, stdout: %s)", parseErr, runErr, string(stdout))
	}
}

// FetchPrices runs check_price.py and returns a map of symbol→price.
func FetchPrices(symbols []string) (map[string]float64, error) {
	stdout, stderr, err := RunPythonScript("shared_scripts/check_price.py", symbols)
	if err != nil {
		return nil, fmt.Errorf("price fetch error: %w (stderr: %s)", err, string(stderr))
	}

	var prices map[string]float64
	if err := json.Unmarshal(stdout, &prices); err != nil {
		return nil, fmt.Errorf("parse prices: %w (stdout: %s)", err, string(stdout))
	}
	return prices, nil
}

// FuturesMarkModePaperFallback is the mode string returned by
// fetch_futures_marks.py when live mode init failed (e.g. missing deps,
// network error) and the script silently degraded to yfinance paper quotes.
// Callers that care about surfacing the downgrade compare against this
// constant. "live" and "paper" are also valid mode strings but represent
// expected states, so the Go side does not act on them.
const FuturesMarkModePaperFallback = "paper_fallback"

// FetchFuturesMarks runs fetch_futures_marks.py and returns a map of
// contract-symbol→mark-price for CME futures (TopStep), plus the mode
// string embedded by the Python script. Mirrors FetchPrices but routes
// through the TopStep adapter (yfinance in paper mode, TopStepX REST in
// live mode) because BinanceUS does not quote ES/NQ/MES/MNQ/CL. See
// issue #261: without this, PortfolioNotional revalued futures positions
// at pos.AvgCost, freezing exposure at entry cost.
//
// The script embeds a reserved "_mode" metadata key in its JSON output
// (one of "live", "paper", "paper_fallback"). We strip it from the
// returned marks map (this is also the *only* filter site for "_mode" —
// mergeFuturesMarks never sees it) and return it as a separate value so
// callers can decide how to surface paper_fallback. Logging is NOT done
// here because this function is called from both the main cycle loop
// (naturally rate-limited) and /status (polled frequently, needs
// throttled logging to avoid spam during sustained downgrades).
func FetchFuturesMarks(symbols []string) (map[string]float64, string, error) {
	if len(symbols) == 0 {
		return map[string]float64{}, "", nil
	}
	stdout, stderr, err := RunPythonScript("shared_scripts/fetch_futures_marks.py", symbols)
	if err != nil {
		return nil, "", fmt.Errorf("futures marks fetch error: %w (stderr: %s)", err, string(stderr))
	}

	// The script mixes float prices with a string "_mode" metadata key,
	// so decode into interface{} first, then split into the
	// float-keyed marks map and the mode string. This loop is the only
	// place "_mode" is filtered out — downstream code (mergeFuturesMarks,
	// PortfolioNotional) operates on the already-clean map[string]float64
	// and never has to defend against the string key. If a future refactor
	// changes this return type, the filter must move with it.
	var raw map[string]interface{}
	if err := json.Unmarshal(stdout, &raw); err != nil {
		return nil, "", fmt.Errorf("parse futures marks: %w (stdout: %s)", err, string(stdout))
	}

	marks := make(map[string]float64, len(raw))
	// mode is parsed on every call so callers can detect silent downgrades
	// (paper_fallback); "live" and "paper" are expected happy-path states
	// and are intentionally not logged anywhere — see callers in main.go
	// and server.go which only branch on FuturesMarkModePaperFallback.
	mode := ""
	for k, v := range raw {
		if k == "_mode" {
			if s, ok := v.(string); ok {
				mode = s
			}
			continue
		}
		if f, ok := v.(float64); ok {
			marks[k] = f
		}
	}
	return marks, mode, nil
}

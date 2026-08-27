package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type pythonScriptTimeoutError struct {
	d time.Duration
}

func (e *pythonScriptTimeoutError) Error() string {
	return fmt.Sprintf("script timed out after %s", e.d)
}

var pythonSemaphore = make(chan struct{}, 4)

const scriptTimeout = 30 * time.Second

type SpotResult struct {
	StrategyDecisionFields
	Strategy   string                 `json:"strategy"`
	Symbol     string                 `json:"symbol"`
	Timeframe  string                 `json:"timeframe"`
	Signal     int                    `json:"signal"`
	Price      float64                `json:"price"`
	Indicators map[string]interface{} `json:"indicators"`
	Timestamp  string                 `json:"timestamp"`
	Error      string                 `json:"error,omitempty"`
}

type HyperliquidResult struct {
	StrategyDecisionFields
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
	Divergence DivergenceResult `json:"-"`
}

type HyperliquidFill struct {
	AvgPx             float64 `json:"avg_px"`
	TotalSz           float64 `json:"total_sz"`
	OID               int64   `json:"oid,omitempty"`
	Fee               float64 `json:"fee,omitempty"`
	StopLossOID       int64   `json:"stop_loss_oid,omitempty"`
	StopLossTriggerPx float64 `json:"stop_loss_trigger_px,omitempty"`
}

type HyperliquidExecution struct {
	Action string           `json:"action"`
	Symbol string           `json:"symbol"`
	Size   float64          `json:"size"`
	Fill   *HyperliquidFill `json:"fill,omitempty"`
}

type HyperliquidExecuteResult struct {
	Execution                 *HyperliquidExecution `json:"execution"`
	Platform                  string                `json:"platform"`
	Timestamp                 string                `json:"timestamp"`
	Error                     string                `json:"error,omitempty"`
	CancelStopLossError       string                `json:"cancel_stop_loss_error,omitempty"`
	CancelStopLossSucceeded   bool                  `json:"cancel_stop_loss_succeeded,omitempty"`
	StopLossError             string                `json:"stop_loss_error,omitempty"`
	StopLossFilledImmediately bool                  `json:"stop_loss_filled_immediately,omitempty"`
}

type HyperliquidStopLossUpdateResult struct {
	Platform                  string  `json:"platform"`
	Timestamp                 string  `json:"timestamp"`
	Error                     string  `json:"error,omitempty"`
	StopLossOID               int64   `json:"stop_loss_oid,omitempty"`
	StopLossTriggerPx         float64 `json:"stop_loss_trigger_px,omitempty"`
	CancelStopLossError       string  `json:"cancel_stop_loss_error,omitempty"`
	CancelStopLossSucceeded   bool    `json:"cancel_stop_loss_succeeded,omitempty"`
	StopLossError             string  `json:"stop_loss_error,omitempty"`
	StopLossFilledImmediately bool    `json:"stop_loss_filled_immediately,omitempty"`
	StopLossFilledExternally  bool    `json:"stop_loss_filled_externally,omitempty"`
	StopLossOutcomeUnknown bool   `json:"stop_loss_outcome_unknown,omitempty"`
	OpenOrderCheckError    string `json:"open_order_check_error,omitempty"`
}

type HyperliquidProtectionSyncResult struct {
	Platform                 string    `json:"platform"`
	Timestamp                string    `json:"timestamp"`
	Error                    string    `json:"error,omitempty"`
	StopLossOID              int64     `json:"stop_loss_oid,omitempty"`
	StopLossTriggerPx        float64   `json:"stop_loss_trigger_px,omitempty"`
	TPOIDs                   []int64   `json:"tp_oids,omitempty"`
	TPPxs                    []float64 `json:"tp_pxs,omitempty"`
	TPErrors                 []string  `json:"tp_errors,omitempty"`
	TPFilledExternally       []bool    `json:"tp_filled_externally,omitempty"`
	TP1OID                   int64     `json:"tp1_oid,omitempty"`
	TP2OID                   int64     `json:"tp2_oid,omitempty"`
	TP1Px                    float64   `json:"tp1_px,omitempty"`
	TP2Px                    float64   `json:"tp2_px,omitempty"`
	StopLossError            string    `json:"stop_loss_error,omitempty"`
	TP1Error                 string    `json:"tp1_error,omitempty"`
	TP2Error                 string    `json:"tp2_error,omitempty"`
	OpenOrderCheckError      string    `json:"open_order_check_error,omitempty"`
	StopLossFilledExternally bool      `json:"stop_loss_filled_externally,omitempty"`
	StopLossFilledImmediately bool `json:"stop_loss_filled_immediately,omitempty"`
	TP1FilledExternally       bool `json:"tp1_filled_externally,omitempty"`
	TP2FilledExternally       bool `json:"tp2_filled_externally,omitempty"`
	TPCancelFailedOIDs []int64 `json:"tp_cancel_failed_oids,omitempty"`
	TPCancelFilledOIDs []int64 `json:"tp_cancel_filled_oids,omitempty"`
	CancelStopLossSucceeded bool `json:"cancel_stop_loss_succeeded,omitempty"`
	StopLossOutcomeUnknown bool `json:"stop_loss_outcome_unknown,omitempty"`
}

func runPython(parentCtx context.Context, script string, args []string, stdinData []byte) ([]byte, []byte, error) {
	return runPythonWithTimeout(parentCtx, script, args, stdinData, scriptTimeout)
}

func runPythonWithTimeout(parentCtx context.Context, script string, args []string, stdinData []byte, timeout time.Duration) ([]byte, []byte, error) {
	pythonSemaphore <- struct{}{}
	defer func() { <-pythonSemaphore }()
	return spawnPythonProcess(parentCtx, script, args, stdinData, timeout)
}

func spawnPythonProcess(parentCtx context.Context, script string, args []string, stdinData []byte, timeout time.Duration) ([]byte, []byte, error) {
	return spawnPythonProcessWithEnv(parentCtx, script, args, stdinData, timeout, nil)
}

func spawnPythonProcessWithEnv(parentCtx context.Context, script string, args []string, stdinData []byte, timeout time.Duration, envOverrides map[string]string) ([]byte, []byte, error) {
	var ctx context.Context
	var cancel context.CancelFunc
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(parentCtx, timeout)
	} else {
		ctx, cancel = context.WithCancel(parentCtx)
	}
	defer cancel()

	cmdArgs := append([]string{script}, args...)
	cmd := exec.CommandContext(ctx, ".venv/bin/python3", cmdArgs...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if len(envOverrides) > 0 {
		cmd.Env = processEnvironment(envOverrides)
	}
	if stdinData != nil {
		cmd.Stdin = bytes.NewReader(stdinData)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if ctx.Err() != nil {
		if cmd.Process != nil {
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	}
	if ctx.Err() == context.DeadlineExceeded {
		return stdout.Bytes(), stderr.Bytes(), &pythonScriptTimeoutError{d: timeout}
	}
	return stdout.Bytes(), stderr.Bytes(), err
}

func processEnvironment(overrides map[string]string) []string {
	keys := make([]string, 0, len(overrides))
	for key := range overrides {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	prefixes := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		prefixes[key+"="] = struct{}{}
	}
	env := make([]string, 0, len(os.Environ())+len(keys))
	for _, item := range os.Environ() {
		replaced := false
		for prefix := range prefixes {
			if strings.HasPrefix(item, prefix) {
				replaced = true
				break
			}
		}
		if !replaced {
			env = append(env, item)
		}
	}
	for _, key := range keys {
		env = append(env, key+"="+overrides[key])
	}
	return env
}

func runPythonReadOnly(script string, args []string) ([]byte, []byte, error) {
	return runPython(shutdownReadOnlyCtx, script, args, nil)
}

func runPythonReadOnlyWithStdin(script string, args []string, stdinData []byte) ([]byte, []byte, error) {
	return runPython(shutdownReadOnlyCtx, script, args, stdinData)
}

func runPythonSideEffect(script string, args []string) ([]byte, []byte, error) {
	sideEffectWG.Add(1)
	defer sideEffectWG.Done()
	return runPython(shutdownSideEffectCtx, script, args, nil)
}

func RunPythonScript(script string, args []string) ([]byte, []byte, error) {
	return runPythonReadOnly(script, args)
}

func RunSpotCheck(script string, args []string) (*SpotResult, string, error) {
	stdout, stderr, err := RunPythonScript(script, args)
	stderrStr := string(stderr)
	if err != nil {
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

func RunPythonScriptWithStdin(script string, args []string, stdinData []byte) ([]byte, []byte, error) {
	return runPythonReadOnlyWithStdin(script, args, stdinData)
}

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

type hlExecuteSnapshot struct {
	AccountLeverage   int
	AccountMarginMode string
}

func buildHyperliquidExecuteArgs(symbol, side string, size, stopLossPct float64, cancelStopLossOID int64, prevPosQty float64, marginMode string, leverage float64, closeFullPosition bool, snapshot hlExecuteSnapshot, extraCancelOIDs ...int64) []string {
	args := []string{
		"--execute",
		fmt.Sprintf("--symbol=%s", symbol),
		fmt.Sprintf("--side=%s", side),
		"--mode=live",
	}
	if closeFullPosition {
		args = append(args, "--close-full-position")
	} else {
		args = append(args, fmt.Sprintf("--size=%g", size))
	}
	if stopLossPct > 0 {
		args = append(args, fmt.Sprintf("--stop-loss-pct=%g", stopLossPct))
	}
	if cancelStopLossOID > 0 {
		args = append(args, fmt.Sprintf("--cancel-stop-loss-oid=%d", cancelStopLossOID))
	}
	for _, oid := range extraCancelOIDs {
		if oid > 0 && oid != cancelStopLossOID {
			args = append(args, fmt.Sprintf("--cancel-stop-loss-oid=%d", oid))
		}
	}
	if prevPosQty > 0 {
		args = append(args, fmt.Sprintf("--prev-pos-qty=%g", prevPosQty))
	}
	if marginMode != "" {
		args = append(args, fmt.Sprintf("--margin-mode=%s", marginMode))
		if leverage > 0 {
			args = append(args, fmt.Sprintf("--leverage=%g", leverage))
		}
		if snapshot.AccountLeverage > 0 && (snapshot.AccountMarginMode == "isolated" || snapshot.AccountMarginMode == "cross") {
			args = append(args, fmt.Sprintf("--account-leverage=%d", snapshot.AccountLeverage))
			args = append(args, fmt.Sprintf("--account-margin-mode=%s", snapshot.AccountMarginMode))
		}
	}
	return args
}

func RunHyperliquidExecute(script, symbol, side string, size, stopLossPct float64, cancelStopLossOID int64, prevPosQty float64, marginMode string, leverage float64, closeFullPosition bool, snapshot hlExecuteSnapshot, extraCancelOIDs ...int64) (*HyperliquidExecuteResult, string, error) {
	args := buildHyperliquidExecuteArgs(symbol, side, size, stopLossPct, cancelStopLossOID, prevPosQty, marginMode, leverage, closeFullPosition, snapshot, extraCancelOIDs...)
	stdout, stderr, err := runPythonSideEffect(script, args)
	return parseHyperliquidExecuteOutput(stdout, string(stderr), err)
}

func RunHyperliquidUpdateStopLoss(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
	args := []string{
		"--update-stop-loss",
		fmt.Sprintf("--symbol=%s", symbol),
		fmt.Sprintf("--side=%s", side),
		fmt.Sprintf("--size=%g", size),
		fmt.Sprintf("--trigger-px=%g", triggerPx),
		"--mode=live",
	}
	if cancelStopLossOID > 0 {
		args = append(args, fmt.Sprintf("--cancel-stop-loss-oid=%d", cancelStopLossOID))
	}
	stdout, stderr, err := runPythonSideEffect(script, args)
	return parseHyperliquidUpdateStopLossOutput(stdout, string(stderr), err)
}

func buildHyperliquidSyncProtectionArgv(symbol, side string, size, avgCost, entryATR, stopLossATRMult float64, tiers []hlProtectionTier, stopLossOID int64, tpOIDs []int64, tpArmedTiers []bool, forceSLReplace bool, forceTPReplace []bool, cancelTPOIDs []int64, reconcileFillHintsJSON []byte) []string {
	args := []string{
		"--sync-protection",
		fmt.Sprintf("--symbol=%s", symbol),
		fmt.Sprintf("--side=%s", side),
		fmt.Sprintf("--size=%g", size),
		fmt.Sprintf("--avg-cost=%g", avgCost),
		fmt.Sprintf("--entry-atr=%g", entryATR),
		fmt.Sprintf("--stop-loss-atr-mult=%g", stopLossATRMult),
		"--mode=live",
	}
	if len(tiers) > 0 {
		tierArgs := make([]map[string]float64, 0, len(tiers))
		for _, tier := range tiers {
			tierArgs = append(tierArgs, map[string]float64{
				"atr_multiple":   tier.Multiple,
				"close_fraction": tier.Fraction,
			})
		}
		if b, err := json.Marshal(tierArgs); err == nil {
			args = append(args, fmt.Sprintf("--tp-tiers-json=%s", string(b)))
		}
	}
	if stopLossOID > 0 {
		args = append(args, fmt.Sprintf("--stop-loss-oid=%d", stopLossOID))
	}
	if len(tpOIDs) > 0 {
		if b, err := json.Marshal(tpOIDs); err == nil {
			args = append(args, fmt.Sprintf("--tp-oids-json=%s", string(b)))
		}
	}
	if len(tiers) > 0 {
		armed := tpArmedTiersForTierCount(tpArmedTiers, len(tiers))
		if b, err := json.Marshal(armed); err == nil {
			args = append(args, fmt.Sprintf("--tp-armed-tiers-json=%s", string(b)))
		} else {
			fmt.Fprintf(os.Stderr, "[WARN] json.Marshal(tp armed tiers) failed: %v — sync-protection omitting --tp-armed-tiers-json\n", err)
		}
	}
	if forceSLReplace {
		args = append(args, "--force-sl-replace")
	}
	if len(forceTPReplace) > 0 {
		if b, err := json.Marshal(forceTPReplace); err == nil {
			args = append(args, fmt.Sprintf("--force-tp-replace-json=%s", string(b)))
		}
	}
	if len(cancelTPOIDs) > 0 {
		if b, err := json.Marshal(cancelTPOIDs); err == nil {
			args = append(args, fmt.Sprintf("--cancel-tp-oids-json=%s", string(b)))
		}
	}
	if len(reconcileFillHintsJSON) > 0 {
		args = append(args, fmt.Sprintf("--reconcile-fill-hints-json=%s", string(reconcileFillHintsJSON)))
	}
	return args
}

func RunHyperliquidSyncProtection(script, symbol, side string, size, avgCost, entryATR, stopLossATRMult float64, tiers []hlProtectionTier, stopLossOID int64, tpOIDs []int64, tpArmedTiers []bool, forceSLReplace bool, forceTPReplace []bool, cancelTPOIDs []int64, reconcileFillHintsJSON []byte) (*HyperliquidProtectionSyncResult, string, error) {
	args := buildHyperliquidSyncProtectionArgv(symbol, side, size, avgCost, entryATR, stopLossATRMult, tiers, stopLossOID, tpOIDs, tpArmedTiers, forceSLReplace, forceTPReplace, cancelTPOIDs, reconcileFillHintsJSON)
	stdout, stderr, err := runPythonSideEffect(script, args)
	stderrStr := string(stderr)
	var result HyperliquidProtectionSyncResult
	if jsonErr := json.Unmarshal(stdout, &result); jsonErr != nil {
		if err != nil {
			return nil, stderrStr, fmt.Errorf("script error: %w (stderr: %s; stdout: %s)", err, stderrStr, string(stdout))
		}
		return nil, stderrStr, fmt.Errorf("parse output: %w (stdout: %s)", jsonErr, string(stdout))
	}
	if err != nil && result.Error == "" {
		return &result, stderrStr, fmt.Errorf("script error: %w (stderr: %s)", err, stderrStr)
	}
	return &result, stderrStr, nil
}

func parseHyperliquidUpdateStopLossOutput(stdout []byte, stderrStr string, runErr error) (*HyperliquidStopLossUpdateResult, string, error) {
	if runErr != nil {
		var result HyperliquidStopLossUpdateResult
		if jsonErr := json.Unmarshal(stdout, &result); jsonErr == nil && result.Error != "" {
			return &result, stderrStr, nil
		}
		return nil, stderrStr, fmt.Errorf("update stop-loss error: %w (stderr: %s)", runErr, stderrStr)
	}

	var result HyperliquidStopLossUpdateResult
	if err := json.Unmarshal(stdout, &result); err != nil {
		return nil, stderrStr, fmt.Errorf("parse output: %w (stdout: %s)", err, string(stdout))
	}
	return &result, stderrStr, nil
}

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

type HyperliquidCloseFill struct {
	AvgPx   float64 `json:"avg_px"`
	TotalSz float64 `json:"total_sz"`
	OID     int64   `json:"oid,omitempty"`
	Fee     float64 `json:"fee,omitempty"`
}

type HyperliquidClose struct {
	Symbol      string                `json:"symbol"`
	Fill        *HyperliquidCloseFill `json:"fill,omitempty"`
	AlreadyFlat bool                  `json:"already_flat,omitempty"`
}

type HyperliquidCloseResult struct {
	Close                       *HyperliquidClose `json:"close"`
	Platform                    string            `json:"platform"`
	Timestamp                   string            `json:"timestamp"`
	Error                       string            `json:"error,omitempty"`
	CancelStopLossError         string            `json:"cancel_stop_loss_error,omitempty"`
	CancelStopLossSucceeded     bool              `json:"cancel_stop_loss_succeeded,omitempty"`
	CancelStopLossSucceededOIDs []int64           `json:"cancel_stop_loss_succeeded_oids,omitempty"`
	CancelStopLossFailedOIDs    []int64           `json:"cancel_stop_loss_failed_oids,omitempty"`
}

func RunHyperliquidClose(script, symbol string, partialSz *float64, cancelStopLossOIDs []int64) (*HyperliquidCloseResult, string, error) {
	return runHyperliquidClose(script, symbol, partialSz, cancelStopLossOIDs, false)
}

func RunHyperliquidCloseCancelAfterFill(script, symbol string, partialSz *float64, cancelStopLossOIDs []int64) (*HyperliquidCloseResult, string, error) {
	return runHyperliquidClose(script, symbol, partialSz, cancelStopLossOIDs, true)
}

func runHyperliquidClose(script, symbol string, partialSz *float64, cancelStopLossOIDs []int64, cancelProtectionAfterClose bool) (*HyperliquidCloseResult, string, error) {
	args := buildHyperliquidCloseArgs(symbol, partialSz, cancelStopLossOIDs, cancelProtectionAfterClose)
	stdout, stderr, runErr := runPythonSideEffect(script, args)
	return parseHyperliquidCloseOutput(stdout, string(stderr), runErr)
}

func buildHyperliquidCloseArgs(symbol string, partialSz *float64, cancelStopLossOIDs []int64, cancelProtectionAfterClose bool) []string {
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
	if cancelProtectionAfterClose {
		args = append(args, "--cancel-protection-after-close")
	}
	return args
}

func parseHyperliquidCloseOutput(stdout []byte, stderrStr string, runErr error) (*HyperliquidCloseResult, string, error) {
	var result HyperliquidCloseResult
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

type ContractSpec struct {
	TickSize   float64 `json:"tick_size"`
	TickValue  float64 `json:"tick_value"`
	Multiplier float64 `json:"multiplier"`
	Margin     float64 `json:"margin"`
}

type TopStepResult struct {
	StrategyDecisionFields
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

type TopStepFill struct {
	AvgPx          float64 `json:"avg_px"`
	TotalContracts int     `json:"total_contracts"`
	OID            string  `json:"oid,omitempty"`
	Fee            float64 `json:"fee,omitempty"`
}

type TopStepExecution struct {
	Action    string       `json:"action"`
	Symbol    string       `json:"symbol"`
	Contracts int          `json:"contracts"`
	Fill      *TopStepFill `json:"fill,omitempty"`
}

type TopStepExecuteResult struct {
	Execution *TopStepExecution `json:"execution"`
	Platform  string            `json:"platform"`
	Timestamp string            `json:"timestamp"`
	Error     string            `json:"error,omitempty"`
}

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

func RunTopStepExecute(script, symbol, side string, contracts int) (*TopStepExecuteResult, string, error) {
	args := []string{
		"--execute",
		fmt.Sprintf("--symbol=%s", symbol),
		fmt.Sprintf("--side=%s", side),
		fmt.Sprintf("--contracts=%d", contracts),
		"--mode=live",
	}
	stdout, stderr, err := runPythonSideEffect(script, args)
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

type TopStepCloseFill struct {
	AvgPx          float64 `json:"avg_px,omitempty"`
	TotalContracts int     `json:"total_contracts,omitempty"`
	OID            string  `json:"oid,omitempty"`
}

type TopStepClose struct {
	Symbol string            `json:"symbol"`
	Fill   *TopStepCloseFill `json:"fill,omitempty"`
}

type TopStepCloseResult struct {
	Close     *TopStepClose `json:"close"`
	Platform  string        `json:"platform"`
	Timestamp string        `json:"timestamp"`
	Error     string        `json:"error,omitempty"`
}

func RunTopStepClose(script, symbol string) (*TopStepCloseResult, string, error) {
	args := []string{
		fmt.Sprintf("--symbol=%s", symbol),
		"--mode=live",
	}
	stdout, stderr, runErr := runPythonSideEffect(script, args)
	return parseTopStepCloseOutput(stdout, string(stderr), runErr)
}

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

type TopStepPositionsResult struct {
	Positions []TopStepPositionJSON `json:"positions"`
	Platform  string                `json:"platform"`
	Timestamp string                `json:"timestamp"`
	Error     string                `json:"error,omitempty"`
}

type TopStepPositionJSON struct {
	Coin     string  `json:"coin"`
	Size     int     `json:"size"`
	AvgPrice float64 `json:"avg_price"`
	Side     string  `json:"side"`
}

func RunTopStepFetchPositions(script string) (*TopStepPositionsResult, string, error) {
	stdout, stderr, runErr := RunPythonScript(script, nil)
	return parseTopStepPositionsOutput(stdout, string(stderr), runErr)
}

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

type TopStepBalanceResult struct {
	Balance       float64 `json:"balance"`
	UnrealizedPnL float64 `json:"unrealized_pnl"`
	Platform      string  `json:"platform"`
	Timestamp     string  `json:"timestamp"`
	Error         string  `json:"error,omitempty"`
}

func RunTopStepFetchBalance(script string) (*TopStepBalanceResult, string, error) {
	stdout, stderr, runErr := RunPythonScript(script, nil)
	return parseTopStepBalanceOutput(stdout, string(stderr), runErr)
}

func parseTopStepBalanceOutput(stdout []byte, stderrStr string, runErr error) (*TopStepBalanceResult, string, error) {
	var result TopStepBalanceResult
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

type TopStepFillsResult struct {
	Fills     []topstepFillRecord `json:"fills"`
	Capped    bool                `json:"capped"`
	Platform  string              `json:"platform"`
	Timestamp string              `json:"timestamp"`
	Error     string              `json:"error,omitempty"`
}

func RunTopStepFetchFills(script string, sinceMs int64) (*TopStepFillsResult, string, error) {
	args := []string{fmt.Sprintf("--since-ms=%d", sinceMs)}
	stdout, stderr, runErr := RunPythonScript(script, args)
	return parseTopStepFillsOutput(stdout, string(stderr), runErr)
}

func parseTopStepFillsOutput(stdout []byte, stderrStr string, runErr error) (*TopStepFillsResult, string, error) {
	var result TopStepFillsResult
	parseErr := json.Unmarshal(stdout, &result)

	switch {
	case runErr == nil && parseErr == nil && result.Error == "":
		return &result, stderrStr, nil

	case runErr == nil && parseErr == nil && result.Error != "":
		return &result, stderrStr, fmt.Errorf("fetch fills reported error despite exit 0: %s", result.Error)

	case parseErr == nil && result.Error != "":
		return &result, stderrStr, fmt.Errorf("fetch fills failed: %s", result.Error)

	case parseErr == nil && runErr != nil:
		return &result, stderrStr, fmt.Errorf("fetch fills subprocess exit %v with no error field (stderr: %s)", runErr, stderrStr)

	default:
		return nil, stderrStr, fmt.Errorf("parse fills output: %v (run err: %v, stdout: %s)", parseErr, runErr, string(stdout))
	}
}

type RobinhoodResult struct {
	StrategyDecisionFields
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

type RobinhoodFill struct {
	AvgPx    float64 `json:"avg_px"`
	Quantity float64 `json:"quantity"`
	OID      string  `json:"oid,omitempty"`
	Fee      float64 `json:"fee,omitempty"`
}

type RobinhoodExecution struct {
	Action    string         `json:"action"`
	Symbol    string         `json:"symbol"`
	AmountUSD float64        `json:"amount_usd,omitempty"`
	Quantity  float64        `json:"quantity,omitempty"`
	Fill      *RobinhoodFill `json:"fill,omitempty"`
}

type RobinhoodExecuteResult struct {
	Execution *RobinhoodExecution `json:"execution"`
	Platform  string              `json:"platform"`
	Timestamp string              `json:"timestamp"`
	Error     string              `json:"error,omitempty"`
}

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
	stdout, stderr, err := runPythonSideEffect(script, args)
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

type OKXResult struct {
	StrategyDecisionFields
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

type OKXFill struct {
	AvgPx   float64 `json:"avg_px"`
	TotalSz float64 `json:"total_sz"`
	OID     string  `json:"oid,omitempty"`
	Fee     float64 `json:"fee,omitempty"`
}

type OKXExecution struct {
	Action string   `json:"action"`
	Symbol string   `json:"symbol"`
	Size   float64  `json:"size"`
	Fill   *OKXFill `json:"fill,omitempty"`
}

type OKXExecuteResult struct {
	Execution *OKXExecution `json:"execution"`
	Platform  string        `json:"platform"`
	Timestamp string        `json:"timestamp"`
	Error     string        `json:"error,omitempty"`
}

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

func RunOKXExecute(script, symbol, side string, size float64, instType string) (*OKXExecuteResult, string, error) {
	args := []string{
		"--execute",
		fmt.Sprintf("--symbol=%s", symbol),
		fmt.Sprintf("--side=%s", side),
		fmt.Sprintf("--size=%g", size),
		"--mode=live",
		fmt.Sprintf("--inst-type=%s", instType),
	}
	stdout, stderr, err := runPythonSideEffect(script, args)
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

type OKXCloseFill struct {
	AvgPx   float64 `json:"avg_px,omitempty"`
	TotalSz float64 `json:"total_sz,omitempty"`
	OID     string  `json:"oid,omitempty"`
	Fee     float64 `json:"fee,omitempty"`
}

type OKXClose struct {
	Symbol      string        `json:"symbol"`
	Fill        *OKXCloseFill `json:"fill,omitempty"`
	AlreadyFlat bool          `json:"already_flat,omitempty"`
}

type OKXCloseResult struct {
	Close     *OKXClose `json:"close"`
	Platform  string    `json:"platform"`
	Timestamp string    `json:"timestamp"`
	Error     string    `json:"error,omitempty"`
}

func RunOKXClose(script, symbol string, partialSz *float64) (*OKXCloseResult, string, error) {
	args := []string{
		fmt.Sprintf("--symbol=%s", symbol),
		"--mode=live",
	}
	if partialSz != nil {
		args = append(args, fmt.Sprintf("--sz=%s", strconv.FormatFloat(*partialSz, 'f', -1, 64)))
	}
	stdout, stderr, runErr := runPythonSideEffect(script, args)
	return parseOKXCloseOutput(stdout, string(stderr), runErr)
}

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

type OKXPositionsResult struct {
	Positions []OKXPositionJSON `json:"positions"`
	Platform  string            `json:"platform"`
	Timestamp string            `json:"timestamp"`
	Error     string            `json:"error,omitempty"`
}

type OKXPositionJSON struct {
	Coin          string  `json:"coin"`
	Size          float64 `json:"size"`
	EntryPrice    float64 `json:"entry_price"`
	Side          string  `json:"side"`
	UnrealizedPnL float64 `json:"unrealized_pnl"`
}

func RunOKXFetchPositions(script string) (*OKXPositionsResult, string, error) {
	stdout, stderr, runErr := RunPythonScript(script, nil)
	return parseOKXPositionsOutput(stdout, string(stderr), runErr)
}

func parseOKXPositionsOutput(stdout []byte, stderrStr string, runErr error) (*OKXPositionsResult, string, error) {
	var result OKXPositionsResult
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

type OKXBalanceResult struct {
	Balance       float64 `json:"balance"`
	UnrealizedPnL float64 `json:"unrealized_pnl"`
	Platform      string  `json:"platform"`
	Timestamp     string  `json:"timestamp"`
	Error         string  `json:"error,omitempty"`
}

func RunOKXFetchBalance(script string) (*OKXBalanceResult, string, error) {
	stdout, stderr, runErr := RunPythonScript(script, nil)
	return parseOKXBalanceOutput(stdout, string(stderr), runErr)
}

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

type OKXBillsResult struct {
	Bills     []okxBillRecord `json:"bills"`
	Capped    bool            `json:"capped"`
	Platform  string          `json:"platform"`
	Timestamp string          `json:"timestamp"`
	Error     string          `json:"error,omitempty"`
}

func RunOKXFetchBills(script string, sinceMs int64) (*OKXBillsResult, string, error) {
	args := []string{fmt.Sprintf("--since-ms=%d", sinceMs)}
	stdout, stderr, runErr := RunPythonScript(script, args)
	return parseOKXBillsOutput(stdout, string(stderr), runErr)
}

func parseOKXBillsOutput(stdout []byte, stderrStr string, runErr error) (*OKXBillsResult, string, error) {
	var result OKXBillsResult
	parseErr := json.Unmarshal(stdout, &result)

	switch {
	case runErr == nil && parseErr == nil && result.Error == "":
		return &result, stderrStr, nil

	case runErr == nil && parseErr == nil && result.Error != "":
		return &result, stderrStr, fmt.Errorf("fetch bills reported error despite exit 0: %s", result.Error)

	case parseErr == nil && result.Error != "":
		return &result, stderrStr, fmt.Errorf("fetch bills failed: %s", result.Error)

	case parseErr == nil && runErr != nil:
		return &result, stderrStr, fmt.Errorf("fetch bills subprocess exit %v with no error field (stderr: %s)", runErr, stderrStr)

	default:
		return nil, stderrStr, fmt.Errorf("parse bills output: %v (run err: %v, stdout: %s)", parseErr, runErr, string(stdout))
	}
}

type RobinhoodCloseFill struct {
	AvgPx   float64 `json:"avg_px,omitempty"`
	TotalSz float64 `json:"total_sz,omitempty"`
	OID     string  `json:"oid,omitempty"`
}

type RobinhoodClose struct {
	Symbol      string              `json:"symbol"`
	Fill        *RobinhoodCloseFill `json:"fill,omitempty"`
	AlreadyFlat bool                `json:"already_flat,omitempty"`
}

type RobinhoodCloseResult struct {
	Close     *RobinhoodClose `json:"close"`
	Platform  string          `json:"platform"`
	Timestamp string          `json:"timestamp"`
	Error     string          `json:"error,omitempty"`
}

func RunRobinhoodClose(script, symbol string) (*RobinhoodCloseResult, string, error) {
	args := []string{
		fmt.Sprintf("--symbol=%s", symbol),
		"--mode=live",
	}
	stdout, stderr, runErr := runPythonSideEffect(script, args)
	return parseRobinhoodCloseOutput(stdout, string(stderr), runErr)
}

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

type RobinhoodPositionsResult struct {
	Positions []RobinhoodPositionJSON `json:"positions"`
	Platform  string                  `json:"platform"`
	Timestamp string                  `json:"timestamp"`
	Error     string                  `json:"error,omitempty"`
}

type RobinhoodPositionJSON struct {
	Coin     string  `json:"coin"`
	Size     float64 `json:"size"`
	AvgPrice float64 `json:"avg_price"`
}

func RunRobinhoodFetchPositions(script string) (*RobinhoodPositionsResult, string, error) {
	stdout, stderr, runErr := RunPythonScript(script, nil)
	return parseRobinhoodPositionsOutput(stdout, string(stderr), runErr)
}

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

const FuturesMarkModePaperFallback = "paper_fallback"

func FetchFuturesMarks(symbols []string) (map[string]float64, string, error) {
	if len(symbols) == 0 {
		return map[string]float64{}, "", nil
	}
	stdout, stderr, err := RunPythonScript("shared_scripts/fetch_futures_marks.py", symbols)
	if err != nil {
		return nil, "", fmt.Errorf("futures marks fetch error: %w (stderr: %s)", err, string(stderr))
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(stdout, &raw); err != nil {
		return nil, "", fmt.Errorf("parse futures marks: %w (stdout: %s)", err, string(stdout))
	}

	marks := make(map[string]float64, len(raw))
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

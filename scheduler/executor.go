package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
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
	AvgPx   float64 `json:"avg_px"`
	TotalSz float64 `json:"total_sz"`
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
	Execution *HyperliquidExecution `json:"execution"`
	Platform  string                `json:"platform"`
	Timestamp string                `json:"timestamp"`
	Error     string                `json:"error,omitempty"`
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
func RunHyperliquidExecute(script, symbol, side string, size float64) (*HyperliquidExecuteResult, string, error) {
	args := []string{
		"--execute",
		fmt.Sprintf("--symbol=%s", symbol),
		fmt.Sprintf("--side=%s", side),
		fmt.Sprintf("--size=%g", size),
		"--mode=live",
	}
	stdout, stderr, err := RunPythonScript(script, args)
	stderrStr := string(stderr)
	if err != nil {
		var result HyperliquidExecuteResult
		if jsonErr := json.Unmarshal(stdout, &result); jsonErr == nil && result.Error != "" {
			return &result, stderrStr, nil
		}
		return nil, stderrStr, fmt.Errorf("execute error: %w (stderr: %s)", err, stderrStr)
	}

	var result HyperliquidExecuteResult
	if err := json.Unmarshal(stdout, &result); err != nil {
		return nil, stderrStr, fmt.Errorf("parse execute output: %w (stdout: %s)", err, string(stdout))
	}
	return &result, stderrStr, nil
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

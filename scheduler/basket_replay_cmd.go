package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

const (
	basketReplayExitOK      = 0
	basketReplayExitError   = 1
	basketReplayExitUsage   = 2
	basketReplayExitRefused = 3
	basketReplayExitFault   = 75
)

var basketReplayScrubbedEnvPrefixes = []string{"HYPERLIQUID_", "OKX_", "DISCORD_", "TELEGRAM_", "ROBINHOOD_", "TOPSTEP_", "DERIBIT_", "IBKR_", "BINANCE_", "LUNO_", "ANTHROPIC_", "OPENAI_", "SLACK_"}

var basketReplayScrubbedEnvWords = []string{"SECRET", "TOKEN", "PRIVATE", "PASSWORD", "API_KEY", "WALLET", "MNEMONIC", "WEBHOOK"}

func scrubBasketReplayEnvironment() []string {
	var removed []string
	for _, kv := range os.Environ() {
		name := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			name = kv[:i]
		}
		upper := strings.ToUpper(name)
		drop := false
		for _, p := range basketReplayScrubbedEnvPrefixes {
			if strings.HasPrefix(upper, p) {
				drop = true
			}
		}
		for _, w := range basketReplayScrubbedEnvWords {
			if strings.Contains(upper, w) {
				drop = true
			}
		}
		if drop {
			os.Unsetenv(name)
			removed = append(removed, name)
		}
	}
	return removed
}

var basketReplayFaultKinds = map[string]int{
	"exit-after-submit":          1,
	"exit-after-collection":      pairReplayProtocolVersion,
	"exit-after-envelope-freeze": pairReplayProtocolVersion,
	"exit-after-acceptance":      pairReplayProtocolVersion,
	"exit-after-book-write":      pairReplayProtocolVersion,
	"exit-after-book-commit":     pairReplayProtocolVersion,
}

func parseBasketReplayFault(raw string) (basketReplayFault, error) {
	var f basketReplayFault
	if raw == "" {
		return f, nil
	}
	name, val, ok := strings.Cut(raw, ":")
	if _, known := basketReplayFaultKinds[name]; !ok || !known {
		return f, fmt.Errorf("--fault %q must be <kind>:<n> with kind exit-after-submit (version 1) or exit-after-collection, exit-after-envelope-freeze, exit-after-acceptance, exit-after-book-write or exit-after-book-commit (version 2)", raw)
	}
	n, err := strconv.Atoi(val)
	if err != nil || n <= 0 {
		return f, fmt.Errorf("--fault %q needs a positive occurrence count", raw)
	}
	f.Kind, f.N = name, n
	if name == "exit-after-submit" {
		f.ExitAfterSubmit = n
	}
	return f, nil
}

func basketReplayFaultVersionError(f basketReplayFault, version int) error {
	if f.Kind == "" || basketReplayFaultKinds[f.Kind] == version {
		return nil
	}
	return fmt.Errorf("--fault %s is a version-%d fault; the specification is version %d", f.Kind, basketReplayFaultKinds[f.Kind], version)
}

func runBasketReplay(args []string) int {
	protocolOut := os.Stdout
	os.Stdout = os.Stderr
	scrubBasketReplayEnvironment()
	return runBasketReplayWith(args, os.Stdin, protocolOut)
}

func runBasketReplayWith(args []string, stdin io.Reader, out io.Writer) int {
	fs := flag.NewFlagSet("basket-replay", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	specPath := fs.String("spec", "", "path to the versioned step script (JSON), or - for standard input")
	maxSteps := fs.Int("max-steps", 0, "apply at most this many further steps, then stop with the run resumable (0 = all)")
	faultRaw := fs.String("fault", "", "controlled fault for restart tests: exit-after-submit:<n> (version 1) or exit-after-collection|exit-after-envelope-freeze|exit-after-acceptance|exit-after-book-write|exit-after-book-commit:<n> (version 2)")
	if err := fs.Parse(args); err != nil {
		return basketReplayExitUsage
	}
	if fs.NArg() > 0 || *specPath == "" || *maxSteps < 0 {
		fmt.Fprintln(os.Stderr, "usage: go-trader basket-replay --spec <step-script.json|-> [--max-steps N] [--fault <kind>:N]")
		return basketReplayExitUsage
	}
	fault, err := parseBasketReplayFault(*faultRaw)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return basketReplayExitUsage
	}
	emit := func(v basketReplayOutput, code int) int {
		enc := json.NewEncoder(out)
		if err := enc.Encode(v); err != nil {
			fmt.Fprintf(os.Stderr, "[basket-replay] could not write the result: %v\n", err)
			return basketReplayExitError
		}
		return code
	}
	var src io.Reader = stdin
	if *specPath != "-" {
		f, err := os.Open(*specPath)
		if err != nil {
			o, code := basketReplayErrorOutput(refuseReplay("spec_unreadable", err.Error()))
			return emit(o, code)
		}
		defer f.Close()
		src = f
	}
	raw, err := io.ReadAll(io.LimitReader(src, basketReplayMaxSpecBytes+1))
	if err != nil {
		o, code := basketReplayErrorOutput(refuseReplay("spec_unreadable", err.Error()))
		return emit(o, code)
	}
	if _, version, err := pairReplayPeekVersion(raw); err == nil && version == pairReplayProtocolVersion {
		if err := basketReplayFaultVersionError(fault, version); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return basketReplayExitUsage
		}
		return runPairReplayWith(raw, basketReplayOptions{MaxSteps: *maxSteps, Fault: fault}, out)
	}
	if err := basketReplayFaultVersionError(fault, basketReplayProtocolVersion); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return basketReplayExitUsage
	}
	spec, raw, err := decodeBasketReplaySpec(bytes.NewReader(raw))
	if err != nil {
		o, code := basketReplayErrorOutput(err)
		return emit(o, code)
	}
	if err := validateBasketReplaySpec(spec); err != nil {
		o, code := basketReplayErrorOutput(err)
		return emit(o, code)
	}
	required := []string{hyperliquidCheckScript, ".venv/bin/python3"}
	if spec.TargetInput.Mode == basketReplayTargetsRanker {
		required = append(required, basketRankerScript)
	}
	for _, p := range required {
		if _, err := os.Stat(p); err != nil {
			o, code := basketReplayErrorOutput(refuseReplay("runtime_missing", fmt.Sprintf("the real member check and ranker run from the repository root; %s is missing: %v", p, err)))
			return emit(o, code)
		}
	}
	rt, err := openBasketReplay(spec, raw, basketReplayOptions{MaxSteps: *maxSteps, Fault: fault})
	if err != nil {
		o, code := basketReplayErrorOutput(err)
		return emit(o, code)
	}
	defer rt.Close()
	if err := rt.Run(); err != nil {
		o, code := basketReplayErrorOutput(err)
		return emit(o, code)
	}
	result, err := rt.output()
	if err != nil {
		o, code := basketReplayErrorOutput(err)
		return emit(o, code)
	}
	return emit(result, basketReplayExitOK)
}

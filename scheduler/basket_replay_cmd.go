package main

import (
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

func parseBasketReplayFault(raw string) (basketReplayFault, error) {
	var f basketReplayFault
	if raw == "" {
		return f, nil
	}
	name, val, ok := strings.Cut(raw, ":")
	if !ok || name != "exit-after-submit" {
		return f, fmt.Errorf("--fault %q must be exit-after-submit:<n>", raw)
	}
	n, err := strconv.Atoi(val)
	if err != nil || n <= 0 {
		return f, fmt.Errorf("--fault %q needs a positive submit count", raw)
	}
	f.ExitAfterSubmit = n
	return f, nil
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
	faultRaw := fs.String("fault", "", "controlled fault for restart tests: exit-after-submit:<n>")
	if err := fs.Parse(args); err != nil {
		return basketReplayExitUsage
	}
	if fs.NArg() > 0 || *specPath == "" || *maxSteps < 0 {
		fmt.Fprintln(os.Stderr, "usage: go-trader basket-replay --spec <step-script.json|-> [--max-steps N] [--fault exit-after-submit:N]")
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
	spec, raw, err := decodeBasketReplaySpec(src)
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

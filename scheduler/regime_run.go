package main

// #879: regime subprocess invocation + per-cycle store build orchestration.

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

const regimeFetchScript = "shared_scripts/fetch_regime.py"

// optionsRegimeInterval mirrors check_options.py REGIME_TIMEFRAME. Options
// regime is advisory (3-state ADX, no gate) and computed inside the options
// evaluators, so #879 populates the store FROM the check result rather than
// injecting — preserving exact options behavior while still surfacing options
// regime in the portfolio/dashboard view.
const optionsRegimeInterval = "4h"

func optionsRegimeSignature(sc StrategyConfig, underlying string, rc *RegimeConfig) RegimeSignature {
	return RegimeSignature{
		Platform: regimePlatformForStrategy(sc),
		Symbol:   strings.TrimSpace(underlying),
		Interval: optionsRegimeInterval,
		SpecHash: regimeSpecHash(rc),
		Kind:     regimeSignatureKindOptions,
	}
}

// regimeSubprocessArgv builds the argv for the dedicated read-only regime
// subprocess. instType/mode are forwarded only when non-empty (OKX inst-type;
// RH/TopStep adapter mode).
func regimeSubprocessArgv(platform, symbol, interval, specJSON string, ohlcvLimit int, instType, mode string) []string {
	argv := []string{
		"--platform=" + platform,
		"--symbol=" + symbol,
		"--interval=" + interval,
		"--regime-windows-spec-json", specJSON,
		fmt.Sprintf("--ohlcv-limit=%d", ohlcvLimit),
	}
	if strings.TrimSpace(instType) != "" {
		argv = append(argv, "--inst-type="+instType)
	}
	if strings.TrimSpace(mode) != "" {
		argv = append(argv, "--mode="+mode)
	}
	return argv
}

type regimeSubprocessOutput struct {
	Regime  RegimePayload `json:"regime"`
	BarTime int64         `json:"bar_time"`
	Error   string        `json:"error"`
}

// runRegimeSubprocessFn is a package var so tests stub it (Go CI has no .venv).
var runRegimeSubprocessFn = runRegimeSubprocess

// runRegimeSubprocess runs fetch_regime.py read-only (runPythonReadOnly acquires
// pythonSemaphore + scriptTimeout internally) and parses its payload.
func runRegimeSubprocess(platform, symbol, interval, specJSON string, ohlcvLimit int, instType, mode string) (RegimePayload, error) {
	argv := regimeSubprocessArgv(platform, symbol, interval, specJSON, ohlcvLimit, instType, mode)
	stdout, stderr, err := runPythonReadOnly(regimeFetchScript, argv)
	if err != nil {
		return RegimePayload{}, fmt.Errorf("regime subprocess %s/%s/%s: %w (stderr: %s)", platform, symbol, interval, err, strings.TrimSpace(string(stderr)))
	}
	var out regimeSubprocessOutput
	if jerr := json.Unmarshal(stdout, &out); jerr != nil {
		return RegimePayload{}, fmt.Errorf("regime subprocess %s/%s/%s: bad json: %w (out: %s)", platform, symbol, interval, jerr, strings.TrimSpace(string(stdout)))
	}
	if out.Error != "" {
		return RegimePayload{}, fmt.Errorf("regime subprocess %s/%s/%s: %s", platform, symbol, interval, out.Error)
	}
	return out.Regime, nil
}

// regimePlatformForStrategy maps a strategy to the --platform token fetch_regime.py
// dispatches on. BinanceUS spot strategies have no explicit Platform; default to
// "binanceus" so the subprocess uses data_fetcher.fetch_ohlcv (same source as
// check_strategy.py).
func regimePlatformForStrategy(sc StrategyConfig) string {
	p := strings.TrimSpace(strings.ToLower(sc.Platform))
	switch p {
	case "hyperliquid", "okx", "robinhood", "topstep", "binanceus":
		return p
	case "":
		return "binanceus"
	default:
		return p
	}
}

// regimeInstTypeForStrategy returns the OKX inst-type (swap/spot); empty for
// other platforms.
func regimeInstTypeForStrategy(sc StrategyConfig) string {
	if strings.EqualFold(sc.Platform, "okx") {
		return okxInstType(sc.Args)
	}
	return ""
}

// regimeModeForStrategy extracts --mode= (paper/live) from the strategy args;
// needed only for RH/TopStep adapter instantiation. Defaults to "paper".
func regimeModeForStrategy(sc StrategyConfig) string {
	for _, a := range sc.Args {
		if strings.HasPrefix(a, "--mode=") {
			if m := strings.TrimSpace(strings.TrimPrefix(a, "--mode=")); m != "" {
				return m
			}
		}
	}
	return "paper"
}

// strategyParticipatesInRegime reports whether a strategy reads the regime store
// this cycle. Spot/perps/futures/manual all fetch OHLCV at a timeframe and run
// the per-window regime gate/ATR/directional features. Options use a separate
// signature (4h ADX) handled by collectRegimeSignatures directly.
func strategyParticipatesInRegime(sc StrategyConfig) bool {
	switch sc.Type {
	case "spot", "perps", "futures", "manual":
		return true
	default:
		return false
	}
}

// collectRegimeSignatures returns the distinct regime signatures among due
// strategies, each mapped to a representative strategy (supplies platform +
// inst-type for the subprocess). Peers on the same asset/interval/spec collapse
// to one signature.
func collectRegimeSignatures(due []StrategyConfig, rc *RegimeConfig) map[RegimeSignature]StrategyConfig {
	out := make(map[RegimeSignature]StrategyConfig)
	if rc == nil || !rc.Enabled {
		return out
	}
	for _, sc := range due {
		if !strategyParticipatesInRegime(sc) {
			continue
		}
		sig := regimeSignatureForStrategy(sc, rc)
		if sig.Symbol == "" || sig.Interval == "" {
			continue
		}
		if _, seen := out[sig]; !seen {
			out[sig] = sc
		}
	}
	return out
}

// regimeFailureTracker throttles operator alerts for sustained regime-subprocess
// failures, keyed by signature (separate from the per-strategy signal-script
// tracker so a regime outage doesn't perturb signal-script alert state). #879
// failure policy: fail-open + log + throttled alert.
var regimeFailureTracker = &ScriptFailureTracker{}

func regimeSignatureKey(sig RegimeSignature) string {
	return sig.Platform + "/" + sig.Symbol + "/" + sig.Interval + "/" + sig.SpecHash + "/" + sig.Kind
}

func notifyRegimeSubprocessFailure(notifier *MultiNotifier, sig RegimeSignature, errMsg string) {
	shouldNotify, count := regimeFailureTracker.Record(regimeSignatureKey(sig), errMsg, time.Now().UTC())
	fmt.Fprintf(os.Stderr, "[WARN] regime subprocess failed for %s/%s (%d consecutive): %s\n", sig.Symbol, sig.Interval, count, errMsg)
	if !shouldNotify || notifier == nil || !notifier.HasBackends() {
		return
	}
	msg := fmt.Sprintf("**REGIME SUBPROCESS FAILING** %s/%s (pid=%d, %d consecutive): regime cleared to empty (fail-open): %s",
		sig.Symbol, sig.Interval, os.Getpid(), count, errMsg)
	notifier.SendOwnerDM(msg)
}

func clearRegimeSubprocessFailure(notifier *MultiNotifier, sig RegimeSignature) {
	recovered, priorCount := regimeFailureTracker.Clear(regimeSignatureKey(sig))
	if !recovered || notifier == nil || !notifier.HasBackends() {
		return
	}
	notifier.SendOwnerDM(fmt.Sprintf("**REGIME SUBPROCESS RECOVERED** %s/%s (pid=%d): succeeded after %d consecutive failures",
		sig.Symbol, sig.Interval, os.Getpid(), priorCount))
}

// buildRegimeStore computes the per-cycle global regime store: one read-only
// subprocess per distinct signature, fanned out concurrently (bounded by the
// shared pythonSemaphore inside runPythonReadOnly). A failed subprocess clears
// that signature to empty (fail-open) and fires a throttled operator alert.
func buildRegimeStore(due []StrategyConfig, rc *RegimeConfig, notifier *MultiNotifier) *RegimeStore {
	store := newRegimeStore()
	sigs := collectRegimeSignatures(due, rc)
	if len(sigs) == 0 {
		return store
	}
	specJSON := regimeWindowsSpecJSON(rc)
	limit := regimeRequiredOhlcvLimit(rc)
	var wg sync.WaitGroup
	for sig, rep := range sigs {
		wg.Add(1)
		go func(sig RegimeSignature, rep StrategyConfig) {
			defer wg.Done()
			pl, err := runRegimeSubprocessFn(
				regimePlatformForStrategy(rep),
				sig.Symbol, sig.Interval, specJSON, limit,
				regimeInstTypeForStrategy(rep),
				regimeModeForStrategy(rep),
			)
			store.put(sig, pl, err)
			if err != nil {
				notifyRegimeSubprocessFailure(notifier, sig, err.Error())
			} else {
				clearRegimeSubprocessFailure(notifier, sig)
			}
		}(sig, rep)
	}
	wg.Wait()
	return store
}

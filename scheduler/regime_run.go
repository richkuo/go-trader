package main

// #879: regime subprocess invocation + per-cycle store build orchestration.

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

const regimeFetchScript = "shared_scripts/fetch_regime.py"

// regimeSubprocessArgv builds the argv for the dedicated read-only regime
// subprocess. instType is forwarded only when non-empty (OKX swap/spot).
func regimeSubprocessArgv(platform, symbol, interval, specJSON string, ohlcvLimit int, instType string) []string {
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
func runRegimeSubprocess(platform, symbol, interval, specJSON string, ohlcvLimit int, instType string) (RegimePayload, error) {
	argv := regimeSubprocessArgv(platform, symbol, interval, specJSON, ohlcvLimit, instType)
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

// buildRegimeStore computes the per-cycle global regime store: one read-only
// subprocess per distinct signature, fanned out concurrently (bounded by the
// shared pythonSemaphore inside runPythonReadOnly). A failed subprocess clears
// that signature to empty (fail-open).
func buildRegimeStore(due []StrategyConfig, rc *RegimeConfig) *RegimeStore {
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
			)
			store.put(sig, pl, err)
		}(sig, rep)
	}
	wg.Wait()
	return store
}

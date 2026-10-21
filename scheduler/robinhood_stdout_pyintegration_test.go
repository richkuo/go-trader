//go:build pyintegration

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func robinhoodStdoutPyRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, ".venv", "bin", "python3")); err != nil {
		t.Skipf("repository virtualenv missing (%v); run uv sync", err)
	}
	return root
}

func robinhoodStdoutOfflineEnv(t *testing.T, root, mode string) {
	t.Helper()
	t.Setenv("PYTHONPATH", filepath.Join(root, "shared_scripts", "testdata", "robinhood_offline"))
	t.Setenv("RH_OFFLINE_STUB_MODE", mode)
	t.Setenv("RH_OFFLINE_STUB_STDOUT_NOISE", "")
	t.Setenv("ROBINHOOD_USERNAME", "offline-user")
	t.Setenv("ROBINHOOD_PASSWORD", "offline-pass")
	t.Setenv("ROBINHOOD_TOTP_SECRET", "JBSWY3DPEHPK3PXP")
}

func robinhoodStdoutFreshHome(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
}

func robinhoodStdoutRequireStubbedLogin(t *testing.T, call, mode, stderr string) {
	t.Helper()
	for _, want := range []string{"robinhood offline stub active: " + mode, "Starting login process..."} {
		if !strings.Contains(stderr, want) {
			t.Errorf("%s (%s): stderr is missing %q; stderr:\n%s", call, mode, want, stderr)
		}
	}
}

func TestRobinhoodScriptStdoutParsesInGo(t *testing.T) {
	root := robinhoodStdoutPyRoot(t)
	t.Chdir(root)
	checkScript := "shared_scripts/check_robinhood.py"

	for _, mode := range []string{"login_ok", "login_rejected", "transport_error"} {
		t.Run(mode, func(t *testing.T) {
			robinhoodStdoutOfflineEnv(t, root, mode)

			for _, checkMode := range []string{"--mode=paper", "--mode=live"} {
				robinhoodStdoutFreshHome(t)
				result, stderr, err := RunRobinhoodCheck(checkScript, []string{"momentum_pro", "BTC", "1h", checkMode})
				if err != nil || result == nil {
					t.Errorf("RunRobinhoodCheck %s (%s): result=%v err=%v stderr:\n%s", checkMode, mode, result, err, stderr)
				}
				robinhoodStdoutRequireStubbedLogin(t, "RunRobinhoodCheck "+checkMode, mode, stderr)
			}

			robinhoodStdoutFreshHome(t)
			execResult, stderr, err := RunRobinhoodExecute(checkScript, "BTC", "buy", 10, 0)
			if err != nil || execResult == nil {
				t.Errorf("RunRobinhoodExecute (%s): result=%v err=%v stderr:\n%s", mode, execResult, err, stderr)
			}
			robinhoodStdoutRequireStubbedLogin(t, "RunRobinhoodExecute", mode, stderr)

			robinhoodStdoutFreshHome(t)
			closeResult, stderr, closeErr := RunRobinhoodClose(robinhoodLiveCloseScript, "BTC")
			if closeResult == nil {
				t.Errorf("RunRobinhoodClose (%s): stdout did not parse: err=%v stderr:\n%s", mode, closeErr, stderr)
			}
			robinhoodStdoutRequireStubbedLogin(t, "RunRobinhoodClose", mode, stderr)

			robinhoodStdoutFreshHome(t)
			positions, stderr, posErr := RunRobinhoodFetchPositions(robinhoodFetchPositionsScript)
			if positions == nil {
				t.Errorf("RunRobinhoodFetchPositions (%s): stdout did not parse: err=%v stderr:\n%s", mode, posErr, stderr)
			}
			robinhoodStdoutRequireStubbedLogin(t, "RunRobinhoodFetchPositions", mode, stderr)

			robinhoodStdoutFreshHome(t)
			options, stderr, err := RunOptionsCheckWithStdin("shared_scripts/check_options.py", []string{"vol_mean_reversion", "BTC", "--platform=robinhood"}, "[]")
			if err != nil || options == nil {
				t.Errorf("RunOptionsCheckWithStdin (%s): result=%v err=%v stderr:\n%s", mode, options, err, stderr)
			}
			robinhoodStdoutRequireStubbedLogin(t, "RunOptionsCheckWithStdin", mode, stderr)

			robinhoodStdoutFreshHome(t)
			balanceStdout, balanceStderr, _ := RunPythonScript("shared_scripts/check_balance.py", []string{"--platform=robinhood"})
			var balance balanceResult
			if err := json.Unmarshal(balanceStdout, &balance); err != nil {
				t.Errorf("check_balance.py (%s): stdout is not one JSON document: %v\nstdout:\n%s\nstderr:\n%s", mode, err, balanceStdout, balanceStderr)
			}
			robinhoodStdoutRequireStubbedLogin(t, "check_balance.py", mode, string(balanceStderr))

			robinhoodStdoutFreshHome(t)
			candles, source, err := FetchUICandles(UICandleRequest{
				Strategy: StrategyConfig{
					ID:       "rh-stdout",
					Platform: "robinhood",
					Type:     "spot",
					Args:     []string{"momentum_pro", "BTC", "1h", "--mode=paper"},
				},
				Limit: 50,
			})
			if err != nil {
				t.Errorf("FetchUICandles (%s): %v", mode, err)
			} else if len(candles) == 0 || source != "robinhood:adapter" {
				t.Errorf("FetchUICandles (%s): got %d candles from source %q, want candles from robinhood:adapter", mode, len(candles), source)
			}
		})
	}
}

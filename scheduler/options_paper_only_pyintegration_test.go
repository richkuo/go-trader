//go:build pyintegration

package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

const optionsModeFourPositions = `[{"option_type":"call","strike":500,"expiry":"2026-12-18"},{"option_type":"call","strike":510,"expiry":"2026-12-18"},{"option_type":"put","strike":450,"expiry":"2026-12-18"},{"option_type":"put","strike":440,"expiry":"2026-12-18"}]`

func optionsModeJSONDocuments(t *testing.T, stdout []byte) []map[string]interface{} {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(stdout))
	var docs []map[string]interface{}
	for dec.More() {
		var doc map[string]interface{}
		if err := dec.Decode(&doc); err != nil {
			t.Fatalf("stdout is not a JSON document stream: %v\nstdout:\n%s", err, stdout)
		}
		docs = append(docs, doc)
	}
	return docs
}

func TestOptionsLiveModeRealCheckScriptMatchesIsLiveArgs(t *testing.T) {
	root := robinhoodStdoutPyRoot(t)
	t.Chdir(root)
	for _, v := range []string{"ROBINHOOD_USERNAME", "ROBINHOOD_PASSWORD", "ROBINHOOD_TOTP_SECRET", "OKX_API_KEY", "OKX_API_SECRET", "OKX_PASSPHRASE"} {
		t.Setenv(v, "")
	}
	const script = "shared_scripts/check_options.py"
	base := []string{"covered_calls", "SPY", "--platform=deribit"}
	tails := [][]string{
		{"--mode=live"},
		{"--mode", "live"},
		{"--mode=paper"},
		{"--mode", "paper"},
		nil,
		{"--mode=paper", "--mode=live"},
		{"--mode=LIVE"},
		{"--mode"},
		{"--mode", "--mode", "live"},
	}
	sawLive, sawPaper := 0, 0
	for _, tail := range tails {
		args := append(append([]string(nil), base...), tail...)
		t.Run(strings.Join(append([]string{"tail"}, tail...), "_"), func(t *testing.T) {
			stdout, stderr, runErr := RunPythonScriptWithStdin(script, args, []byte(optionsModeFourPositions))
			docs := optionsModeJSONDocuments(t, stdout)
			if len(docs) != 1 {
				t.Fatalf("stdout holds %d JSON documents, want 1\nstdout:\n%s\nstderr:\n%s", len(docs), stdout, stderr)
			}
			if isLiveArgs(args) {
				sawLive++
				if runErr == nil {
					t.Fatalf("live args %v exited 0, want a nonzero exit", args)
				}
				result, _, err := RunOptionsCheckWithStdin(script, args, optionsModeFourPositions)
				if err != nil || result == nil {
					t.Fatalf("RunOptionsCheckWithStdin(%v) = %v, %v; want a parsed structured error", args, result, err)
				}
				if result.Error != optionsLiveModeConfigError("deribit") {
					t.Errorf("error = %q, want %q", result.Error, optionsLiveModeConfigError("deribit"))
				}
				if result.Signal != 0 || len(result.Actions) != 0 {
					t.Errorf("refusal carried signal=%d actions=%d, want 0 and none", result.Signal, len(result.Actions))
				}
				if _, has := docs[0]["skip_reason"]; has {
					t.Errorf("refusal reached the position cap: %v", docs[0])
				}
				return
			}
			sawPaper++
			if runErr != nil {
				t.Fatalf("paper args %v failed: %v\nstdout:\n%s\nstderr:\n%s", args, runErr, stdout, stderr)
			}
			if got := docs[0]["skip_reason"]; got != "Max positions reached (4/4)" {
				t.Errorf("skip_reason = %v, want the 4-position cap (stdin positions read)", got)
			}
			if got := docs[0]["underlying"]; got != "SPY" {
				t.Errorf("underlying = %v, want SPY (mode tokens kept out of the positional arguments)", got)
			}
		})
	}
	if sawLive != 4 || sawPaper != 5 {
		t.Errorf("classified %d live and %d paper tails, want 4 and 5", sawLive, sawPaper)
	}
}

func TestOptionsLiveModeRealCheckScriptRefusesBeforeRobinhoodLogin(t *testing.T) {
	root := robinhoodStdoutPyRoot(t)
	t.Chdir(root)
	robinhoodStdoutOfflineEnv(t, root, "login_ok")
	const script = "shared_scripts/check_options.py"
	base := []string{"vol_mean_reversion", "BTC", "--platform=robinhood"}

	for _, tail := range [][]string{{"--mode", "live"}, {"--mode=live"}} {
		robinhoodStdoutFreshHome(t)
		args := append(append([]string(nil), base...), tail...)
		result, stderr, err := RunOptionsCheckWithStdin(script, args, "[]")
		if err != nil || result == nil {
			t.Fatalf("RunOptionsCheckWithStdin(%v) = %v, %v; stderr:\n%s", args, result, err, stderr)
		}
		if result.Error != optionsLiveModeConfigError("robinhood") {
			t.Errorf("error = %q, want %q", result.Error, optionsLiveModeConfigError("robinhood"))
		}
		if strings.Contains(stderr, "Starting login process...") {
			t.Errorf("live refusal built the Robinhood adapter and logged in; stderr:\n%s", stderr)
		}
	}

	robinhoodStdoutFreshHome(t)
	result, stderr, err := RunOptionsCheckWithStdin(script, append(append([]string(nil), base...), "--mode=paper"), "[]")
	if err != nil || result == nil {
		t.Fatalf("paper control: result=%v err=%v stderr:\n%s", result, err, stderr)
	}
	if result.Error == optionsLiveModeConfigError("robinhood") {
		t.Fatalf("paper control was refused as live")
	}
	robinhoodStdoutRequireStubbedLogin(t, "RunOptionsCheckWithStdin --mode=paper", "login_ok", stderr)
}

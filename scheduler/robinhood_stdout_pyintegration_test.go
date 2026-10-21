//go:build pyintegration

package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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
			execResult, stderr, err := RunRobinhoodExecute(checkScript, "BTC", "buy", 10, 0, "11111111-1111-4111-8111-111111111111", RobinhoodSellArgs{})
			if execResult == nil || err == nil || execResult.OrderOutcome != robinhoodOutcomeNotSent {
				t.Errorf("RunRobinhoodExecute (%s): an order whose venue reads fail must parse as not_sent: result=%+v err=%v stderr:\n%s", mode, execResult, err, stderr)
			}
			robinhoodStdoutRequireStubbedLogin(t, "RunRobinhoodExecute", mode, stderr)

			robinhoodStdoutFreshHome(t)
			closeResult, stderr, closeErr := RunRobinhoodClose(robinhoodLiveCloseScript, "BTC", "")
			if closeResult == nil {
				t.Errorf("RunRobinhoodClose (%s): stdout did not parse: err=%v stderr:\n%s", mode, closeErr, stderr)
			} else if closeErr == nil || closeResult.Error == "" || (closeResult.Close != nil && closeResult.Close.AlreadyFlat) {
				t.Errorf("RunRobinhoodClose (%s): a close whose position read failed must report an error, got err=%v result=%+v", mode, closeErr, closeResult)
			}
			robinhoodStdoutRequireStubbedLogin(t, "RunRobinhoodClose", mode, stderr)

			robinhoodStdoutFreshHome(t)
			positions, stderr, posErr := RunRobinhoodFetchPositions(robinhoodFetchPositionsScript)
			if positions == nil {
				t.Errorf("RunRobinhoodFetchPositions (%s): stdout did not parse: err=%v stderr:\n%s", mode, posErr, stderr)
			} else if posErr == nil || positions.Error == "" {
				t.Errorf("RunRobinhoodFetchPositions (%s): a failed position read must report an error, got err=%v result=%+v", mode, posErr, positions)
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

func robinhoodStdoutCloser(symbol string) (*RobinhoodCloseResult, error) {
	result, _, err := RunRobinhoodClose(robinhoodLiveCloseScript, symbol, "")
	return result, err
}

func robinhoodStdoutStore(t *testing.T, strategies []StrategyConfig) (*StateStore, *AppState) {
	t.Helper()
	cfg := &Config{DBFile: filepath.Join(t.TempDir(), "state.db"), Strategies: strategies}
	store := openSplitStore(t, cfg)
	state := NewAppState()
	for _, sc := range strategies {
		state.Strategies[sc.ID] = NewStrategyState(sc)
	}
	if err := store.LoadRobinhoodOrderGuards(state, strategies); err != nil {
		t.Fatalf("load robinhood guards: %v", err)
	}
	return store, state
}

func robinhoodStdoutTradeRows(t *testing.T, store *StateStore) int {
	t.Helper()
	var n int
	if err := store.primary().db.QueryRow("SELECT COUNT(*) FROM trades").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRobinhoodScriptStdoutOrderAndCloseOutcomes(t *testing.T) {
	root := robinhoodStdoutPyRoot(t)
	t.Chdir(root)
	origTimeout := robinhoodOrderScriptTimeout
	robinhoodOrderScriptTimeout = 14 * time.Second
	t.Cleanup(func() { robinhoodOrderScriptTimeout = origTimeout })
	logger := &StrategyLogger{stratID: "rh-stdout", writer: io.Discard}
	sc := StrategyConfig{ID: "rh-stdout", Type: "spot", Platform: "robinhood", Script: "shared_scripts/check_robinhood.py", Args: []string{"momentum_pro", "BTC", "1h", "--mode=live"}, Capital: 50}
	ethSC := StrategyConfig{ID: "rh-stdout-eth", Type: "spot", Platform: "robinhood", Args: []string{"momentum_pro", "ETH", "1h", "--mode=live"}}
	liveStrategies := []StrategyConfig{sc, ethSC}

	for _, mode := range []string{"login_ok", "login_rejected", "transport_error"} {
		t.Run("close_read_fails_"+mode, func(t *testing.T) {
			robinhoodStdoutOfflineEnv(t, root, mode)
			robinhoodStdoutFreshHome(t)
			report := forceCloseRobinhoodLive(context.Background(), []RobinhoodPosition{{Coin: "BTC", Size: 0.5}}, liveStrategies, robinhoodStdoutCloser, defaultRobinhoodPositionsFetcher, nil, nil)
			if report.Errors["BTC"] == nil || report.ConfirmedFlat() || len(report.AlreadyFlat) != 0 || len(report.ClosedCoins) != 0 {
				t.Fatalf("%s: close with a failed position read must land in Errors, got %+v", mode, report)
			}
		})
	}

	t.Run("close_no_holding_is_flat", func(t *testing.T) {
		robinhoodStdoutOfflineEnv(t, root, "order_accepted")
		robinhoodStdoutFreshHome(t)
		report := forceCloseRobinhoodLive(context.Background(), []RobinhoodPosition{{Coin: "ETH", Size: 0.5}}, []StrategyConfig{ethSC}, robinhoodStdoutCloser, defaultRobinhoodPositionsFetcher, nil, nil)
		if len(report.Errors) != 0 || len(report.AlreadyFlat) != 1 || report.AlreadyFlat[0] != "ETH" || !report.ConfirmedFlat() {
			t.Fatalf("close after a successful read with no ETH row must be already flat, got %+v", report)
		}
	})

	for _, mode := range []string{"order_rejected", "order_unknown"} {
		t.Run("execute_fails_"+mode, func(t *testing.T) {
			robinhoodStdoutOfflineEnv(t, root, mode)
			robinhoodStdoutFreshHome(t)
			store, state := robinhoodStdoutStore(t, liveStrategies)
			var mu sync.RWMutex
			s := state.Strategies[sc.ID]
			if er, ok := runRobinhoodExecuteOrder(store, state, &mu, sc, s, &RobinhoodResult{Symbol: "BTC", Signal: 1}, "BUY", 100, 50, false, 0, "", HurstGateDecision{}, liveStrategies, nil, logger); ok || er != nil {
				t.Errorf("%s buy: got ok=%v result=%+v, want the live-execute-failed path", mode, ok, er)
			}
			if robinhoodStdoutTradeRows(t, store) != 0 || len(s.Positions) != 0 || s.Cash != 50 {
				t.Errorf("%s buy: a refused or unknown order must book nothing", mode)
			}
			robinhoodStdoutFreshHome(t)
			store2, state2 := robinhoodStdoutStore(t, liveStrategies)
			deps := robinhoodEmergencyDeps{store: store2, state: state2, mu: &mu, cfgs: liveStrategies}
			report := forceCloseRobinhoodLive(context.Background(), []RobinhoodPosition{{Coin: "BTC", Size: 0.5}}, []StrategyConfig{sc}, newRobinhoodEmergencyCloser(deps, robinhoodOriginKillSwitch), defaultRobinhoodPositionsFetcher, nil, nil)
			if report.Errors["BTC"] == nil || report.ConfirmedFlat() || len(report.ClosedCoins) != 0 {
				t.Errorf("%s close: a refused or unknown sell must land in Errors, got %+v", mode, report)
			}
		})
	}

	t.Run("execute_accepted", func(t *testing.T) {
		robinhoodStdoutOfflineEnv(t, root, "order_accepted")
		robinhoodStdoutFreshHome(t)
		store, state := robinhoodStdoutStore(t, liveStrategies)
		var mu sync.RWMutex
		s := state.Strategies[sc.ID]
		er, ok := runRobinhoodExecuteOrder(store, state, &mu, sc, s, &RobinhoodResult{Symbol: "BTC", Signal: 1}, "BUY", 100, 50, false, 0, "", HurstGateDecision{}, liveStrategies, nil, logger)
		if ok || er != nil {
			t.Fatalf("accepted buy with no confirmed fill: got ok=%v result=%+v, want an unresolved hold", ok, er)
		}
		if robinhoodStdoutTradeRows(t, store) != 0 || len(s.Positions) != 0 || s.Cash != 50 {
			t.Fatal("an accepted order with no confirmed fill must book nothing")
		}
		held := robinhoodOrderStartupLines(state)
		if len(held) != 1 {
			t.Fatalf("an accepted order with no confirmed fill must hold the coin, got %v", held)
		}
		robinhoodStdoutFreshHome(t)
		store2, state2 := robinhoodStdoutStore(t, liveStrategies)
		deps := robinhoodEmergencyDeps{store: store2, state: state2, mu: &mu, cfgs: liveStrategies}
		report := forceCloseRobinhoodLive(context.Background(), []RobinhoodPosition{{Coin: "BTC", Size: 0.5}}, []StrategyConfig{sc}, newRobinhoodEmergencyCloser(deps, robinhoodOriginKillSwitch), defaultRobinhoodPositionsFetcher, nil, nil)
		if report.Errors["BTC"] == nil || len(report.ClosedCoins) != 0 || report.ConfirmedFlat() {
			t.Fatalf("accepted close with no confirmed fill: got %+v, want BTC in Errors", report)
		}
	})
}

func TestRobinhoodScriptStdoutFailedLoginRefusesLiveWork(t *testing.T) {
	root := robinhoodStdoutPyRoot(t)
	t.Chdir(root)
	const loginFailed = "Robinhood login failed:"
	checkScript := "shared_scripts/check_robinhood.py"
	secrets := []string{"offline-user", "offline-pass", "JBSWY3DPEHPK3PXP", "offline-access-token", "offline-refresh-token"}
	logger := &StrategyLogger{stratID: "rh-login", writer: io.Discard}
	sc := StrategyConfig{ID: "rh-login", Type: "spot", Platform: "robinhood", Script: checkScript, Args: []string{"momentum_pro", "BTC", "1h", "--mode=live"}}
	liveStrategies := []StrategyConfig{sc}
	modes := []string{"login_rejected", "token_cache_unwritable", "transport_error_secret"}

	for _, mode := range modes {
		t.Run(mode, func(t *testing.T) {
			robinhoodStdoutOfflineEnv(t, root, mode)
			var texts []string
			keep := func(parts ...string) { texts = append(texts, parts...) }

			robinhoodStdoutFreshHome(t)
			live, stderr, err := RunRobinhoodCheck(checkScript, []string{"momentum_pro", "BTC", "1h", "--mode=live"})
			keep(stderr)
			if err != nil || live == nil || !strings.HasPrefix(live.Error, loginFailed) || live.Signal != 0 {
				t.Errorf("live check: got result=%+v err=%v, want an %q envelope with signal 0", live, err, loginFailed)
			}

			robinhoodStdoutFreshHome(t)
			paper, stderr, err := RunRobinhoodCheck(checkScript, []string{"momentum_pro", "BTC", "1h", "--mode=paper"})
			keep(stderr)
			if err != nil || paper == nil || paper.Error != "" || paper.Price <= 0 || strings.Contains(stderr, "can only be called when logged in") {
				t.Errorf("paper check: got result=%+v err=%v, want a logged-out paper result on public data; stderr:\n%s", paper, err, stderr)
			}

			for _, side := range []string{"buy", "sell"} {
				robinhoodStdoutFreshHome(t)
				execResult, stderr, err := RunRobinhoodExecute(checkScript, "BTC", side, 10, 0.1, "11111111-1111-4111-8111-111111111111", RobinhoodSellArgs{})
				keep(stderr)
				if err == nil || execResult == nil || !strings.HasPrefix(execResult.Error, loginFailed) || execResult.OrderOutcome != "not_sent" || (execResult.Execution != nil && execResult.Execution.Fill != nil) {
					t.Errorf("execute %s: got result=%+v err=%v, want an %q envelope, a script error, order_outcome not_sent and no fill", side, execResult, err, loginFailed)
				}
			}

			robinhoodStdoutFreshHome(t)
			buyStore, buyState := robinhoodStdoutStore(t, liveStrategies)
			var buyMu sync.RWMutex
			buyS := buyState.Strategies[sc.ID]
			buyCash := buyS.Cash
			if er, ok := runRobinhoodExecuteOrder(buyStore, buyState, &buyMu, sc, buyS, &RobinhoodResult{Symbol: "BTC", Signal: 1}, "BUY", 100, 50, false, 0, "", HurstGateDecision{}, liveStrategies, nil, logger); ok || er != nil {
				t.Errorf("execute order buy: got ok=%v result=%+v, want the live-execute-failed path", ok, er)
			}
			if robinhoodStdoutTradeRows(t, buyStore) != 0 || len(buyS.Positions) != 0 || buyS.Cash != buyCash {
				t.Errorf("execute order buy: a failed login must book nothing")
			}
			robinhoodStdoutFreshHome(t)
			sellStore, sellState := robinhoodStdoutStore(t, liveStrategies)
			var sellMu sync.RWMutex
			sellS := sellState.Strategies[sc.ID]
			if er, ok := runRobinhoodExecuteOrder(sellStore, sellState, &sellMu, sc, sellS, &RobinhoodResult{Symbol: "BTC", Signal: -1}, "SELL", 100, 0, false, 0.1, "long", HurstGateDecision{}, liveStrategies, nil, logger); ok || er != nil {
				t.Errorf("execute order sell: got ok=%v result=%+v, want the live-execute-failed path", ok, er)
			}
			if robinhoodStdoutTradeRows(t, sellStore) != 0 {
				t.Errorf("execute order sell: a failed login must book nothing")
			}

			robinhoodStdoutFreshHome(t)
			closeResult, stderr, closeErr := RunRobinhoodClose(robinhoodLiveCloseScript, "BTC", "")
			keep(stderr)
			if closeErr != nil {
				keep(closeErr.Error())
			}
			if closeErr == nil || closeResult == nil || !strings.HasPrefix(closeResult.Error, loginFailed) || (closeResult.Close != nil && closeResult.Close.AlreadyFlat) {
				t.Errorf("close: got result=%+v err=%v, want an %q error and no already-flat", closeResult, closeErr, loginFailed)
			}

			robinhoodStdoutFreshHome(t)
			killStore, killState := robinhoodStdoutStore(t, liveStrategies)
			var killMu sync.RWMutex
			killDeps := robinhoodEmergencyDeps{store: killStore, state: killState, mu: &killMu, cfgs: liveStrategies}
			report := forceCloseRobinhoodLive(context.Background(), []RobinhoodPosition{{Coin: "BTC", Size: 0.5}}, liveStrategies, newRobinhoodEmergencyCloser(killDeps, robinhoodOriginKillSwitch), defaultRobinhoodPositionsFetcher, nil, nil)
			if report.Errors["BTC"] != nil {
				keep(report.Errors["BTC"].Error())
			}
			if report.Errors["BTC"] == nil || !strings.Contains(report.Errors["BTC"].Error(), loginFailed) || len(report.AlreadyFlat) != 0 || len(report.ClosedCoins) != 0 || report.ConfirmedFlat() {
				t.Errorf("kill-switch close: got %+v, want a BTC login error and no confirmed flat", report)
			}

			robinhoodStdoutFreshHome(t)
			cbStore, state := robinhoodStdoutStore(t, liveStrategies)
			var mu sync.RWMutex
			state.Strategies[sc.ID].RiskState.setPendingCircuitClose(PlatformPendingCloseRobinhood, &PendingCircuitClose{Symbols: []PendingCircuitCloseSymbol{{Symbol: "BTC", Size: 0.5}}})
			cbDeps := robinhoodEmergencyDeps{store: cbStore, state: state, mu: &mu, cfgs: liveStrategies}
			runPendingRobinhoodCircuitCloses(context.Background(), state, liveStrategies, []RobinhoodPosition{{Coin: "BTC", Size: 0.5}}, true, nil, newRobinhoodEmergencyCloser(cbDeps, robinhoodOriginCircuitBreaker), nil, 60*time.Second, &mu, robinhoodCircuitCloseDeps{store: cbStore, prices: map[string]float64{"BTC": 50}})
			pending := state.Strategies[sc.ID].RiskState.getPendingCircuitClose(PlatformPendingCloseRobinhood)
			if pending == nil || len(pending.Symbols) != 1 || pending.Symbols[0].Symbol != "BTC" {
				t.Errorf("circuit-breaker pending close: got %+v, want the BTC pending close kept", pending)
			}

			robinhoodStdoutFreshHome(t)
			positions, stderr, posErr := RunRobinhoodFetchPositions(robinhoodFetchPositionsScript)
			keep(stderr)
			if posErr == nil || positions == nil || !strings.HasPrefix(positions.Error, loginFailed) || len(positions.Positions) != 0 {
				t.Errorf("positions: got result=%+v err=%v, want an %q error and no positions", positions, posErr, loginFailed)
			}

			robinhoodStdoutFreshHome(t)
			_, _, liveCandlesErr := FetchUICandles(UICandleRequest{Strategy: sc, Limit: 50})
			if liveCandlesErr != nil {
				keep(liveCandlesErr.Error())
			}
			if liveCandlesErr == nil || !strings.Contains(liveCandlesErr.Error(), loginFailed) {
				t.Errorf("live candles: got err=%v, want an %q error", liveCandlesErr, loginFailed)
			}

			robinhoodStdoutFreshHome(t)
			paperCandles, source, err := FetchUICandles(UICandleRequest{
				Strategy: StrategyConfig{ID: "rh-login-paper", Platform: "robinhood", Type: "spot", Args: []string{"momentum_pro", "BTC", "1h", "--mode=paper"}},
				Limit:    50,
			})
			if err != nil || len(paperCandles) == 0 || source != "robinhood:adapter" {
				t.Errorf("paper candles: got %d candles from %q err=%v, want candles from robinhood:adapter", len(paperCandles), source, err)
			}

			robinhoodStdoutFreshHome(t)
			options, stderr, err := RunOptionsCheckWithStdin("shared_scripts/check_options.py", []string{"vol_mean_reversion", "BTC", "--platform=robinhood"}, "[]")
			keep(stderr)
			if err != nil || options == nil || options.Error != "" {
				t.Errorf("options: got result=%+v err=%v, want a paper result with no error", options, err)
			}

			robinhoodStdoutFreshHome(t)
			balance, balanceErr := FetchPlatformBalance("robinhood")
			if balanceErr != nil {
				keep(balanceErr.Error())
			}
			if balanceErr == nil || balance != 0 || !strings.Contains(balanceErr.Error(), loginFailed) {
				t.Errorf("balance: got balance=%v err=%v, want an %q error", balance, balanceErr, loginFailed)
			}

			for _, text := range texts {
				for _, secret := range secrets {
					if strings.Contains(text, secret) {
						t.Errorf("a returned stderr or error text holds the fake secret %q:\n%s", secret, text)
					}
				}
			}
		})
	}
}

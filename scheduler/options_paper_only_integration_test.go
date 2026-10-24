package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type optionsModeCase struct {
	id       string
	platform string
	explicit bool
	underly  string
}

func optionsModeCases() []optionsModeCase {
	return []optionsModeCase{
		{id: "rh-calls-spy", platform: "robinhood", explicit: true, underly: "SPY"},
		{id: "deribit-calls-btc", platform: "deribit", explicit: true, underly: "BTC"},
		{id: "ibkr-calls-btc", platform: "ibkr", explicit: true, underly: "BTC"},
		{id: "okx-calls-btc", platform: "okx", explicit: true, underly: "BTC"},
		{id: "rh-calls-spy", platform: "robinhood", explicit: false, underly: "SPY"},
		{id: "calls-btc", platform: "deribit", explicit: false, underly: "BTC"},
	}
}

func optionsModeConfigJSON(t *testing.T, dir string, c optionsModeCase, modeTokens []string) string {
	t.Helper()
	args := []string{"covered_calls", c.underly}
	if c.explicit {
		args = append(args, "--platform="+c.platform)
	}
	args = append(args, modeTokens...)
	strategy := map[string]interface{}{
		"id":               c.id,
		"type":             "options",
		"script":           "shared_scripts/check_options.py",
		"args":             args,
		"capital":          1000,
		"max_drawdown_pct": 40,
	}
	if c.explicit {
		strategy["platform"] = c.platform
	}
	root := map[string]interface{}{
		"config_version":   CurrentConfigVersion,
		"interval_seconds": 14400,
		"log_dir":          filepath.Join(dir, "logs"),
		"db_file":          filepath.Join(dir, "state.db"),
		"auto_update":      "off",
		"strategies":       []interface{}{strategy},
	}
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	return string(out)
}

func optionsModeSetCredentials(t *testing.T) {
	t.Helper()
	for _, v := range []string{"ROBINHOOD_USERNAME", "ROBINHOOD_PASSWORD", "ROBINHOOD_TOTP_SECRET", "OKX_API_KEY", "OKX_API_SECRET", "OKX_PASSPHRASE"} {
		t.Setenv(v, "set-for-test")
	}
}

type optionsModeLoader struct {
	name string
	load func(path string) (*Config, error)
}

func optionsModeLoaders() []optionsModeLoader {
	return []optionsModeLoader{
		{name: "LoadConfig", load: LoadConfig},
		{name: "LoadConfigForProbe", load: LoadConfigForProbe},
		{name: "LoadConfigReadOnly", load: LoadConfigReadOnly},
		{name: "LoadConfigForLedgerExport", load: func(path string) (*Config, error) {
			data, err := os.ReadFile(path)
			if err != nil {
				return nil, err
			}
			return LoadConfigForLedgerExport(data)
		}},
	}
}

func TestOptionsLiveModeRefusedOnEveryConfigLoad(t *testing.T) {
	for _, withCreds := range []bool{true, false} {
		for _, c := range optionsModeCases() {
			for _, mode := range [][]string{{"--mode=live"}, {"--mode", "live"}} {
				name := fmt.Sprintf("creds=%v/%s/explicit=%v/%s", withCreds, c.id, c.explicit, strings.Join(mode, "_"))
				t.Run(name, func(t *testing.T) {
					if withCreds {
						optionsModeSetCredentials(t)
					} else {
						for _, v := range []string{"ROBINHOOD_USERNAME", "ROBINHOOD_PASSWORD", "ROBINHOOD_TOTP_SECRET", "OKX_API_KEY", "OKX_API_SECRET", "OKX_PASSPHRASE"} {
							t.Setenv(v, "")
						}
					}
					dir := t.TempDir()
					path := writeTestConfig(t, dir, optionsModeConfigJSON(t, dir, c, mode))
					original := mustReadFile(t, path)
					want := fmt.Sprintf("strategy[%s]: %s", c.id, optionsLiveModeConfigError(c.platform))
					for _, l := range optionsModeLoaders() {
						cfg, err := l.load(path)
						if cfg != nil {
							t.Errorf("%s returned a config for a live options strategy", l.name)
						}
						if err == nil {
							t.Fatalf("%s accepted a live options strategy", l.name)
						}
						if !strings.Contains(err.Error(), want) {
							t.Errorf("%s error = %v, want it to hold %q", l.name, err, want)
						}
					}
					if after := mustReadFile(t, path); !bytes.Equal(after, original) {
						t.Errorf("config file changed during refused loads")
					}
				})
			}
		}
	}
}

func TestOptionsPaperModesLoadOnEveryConfigLoad(t *testing.T) {
	optionsModeSetCredentials(t)
	for _, c := range optionsModeCases() {
		for _, mode := range [][]string{{"--mode=paper"}, {"--mode", "paper"}, nil} {
			name := fmt.Sprintf("%s/explicit=%v/%s", c.id, c.explicit, strings.Join(mode, "_"))
			t.Run(name, func(t *testing.T) {
				dir := t.TempDir()
				path := writeTestConfig(t, dir, optionsModeConfigJSON(t, dir, c, mode))
				for _, l := range optionsModeLoaders() {
					cfg, err := l.load(path)
					if err != nil {
						t.Fatalf("%s refused a paper options strategy: %v", l.name, err)
					}
					if len(cfg.Strategies) != 1 {
						t.Fatalf("%s loaded %d strategies, want 1", l.name, len(cfg.Strategies))
					}
					sc := cfg.Strategies[0]
					if sc.Platform != c.platform {
						t.Errorf("%s platform = %q, want %q", l.name, sc.Platform, c.platform)
					}
					if p := partitionFor(sc); p != paperSourcePartition("") {
						t.Errorf("%s partition = %+v, want the default paper partition", l.name, p)
					}
				}
			})
		}
	}
}

func optionsPromotionConfigJSON(t *testing.T, dir string, modeTokens []string) string {
	t.Helper()
	var root map[string]interface{}
	if err := json.Unmarshal([]byte(minimalConfigJSON), &root); err != nil {
		t.Fatalf("parse minimal config: %v", err)
	}
	root["config_version"] = CurrentConfigVersion
	root["db_file"] = filepath.Join(dir, "state.db")
	root["log_dir"] = filepath.Join(dir, "logs")
	strategies := root["strategies"].([]interface{})
	strategies = append(strategies, map[string]interface{}{
		"id":               "deribit-calls-btc",
		"type":             "options",
		"platform":         "deribit",
		"script":           "shared_scripts/check_options.py",
		"args":             append([]string{"covered_calls", "BTC", "--platform=deribit"}, modeTokens...),
		"capital":          1000,
		"max_drawdown_pct": 40,
	})
	root["strategies"] = strategies
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	return string(out) + "\n"
}

func optionsPromotionTempFiles(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "go-trader-config-*.json"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	return matches
}

func TestOptionsLiveModeRefusedByAdministrativePromotion(t *testing.T) {
	const id = "deribit-calls-btc"
	for _, mode := range [][]string{{"--mode=paper"}, {"--mode", "paper"}} {
		t.Run(strings.Join(mode, "_"), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.json")
			if err := os.WriteFile(path, []byte(optionsPromotionConfigJSON(t, dir, mode)), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}
			original := mustReadFile(t, path)
			cfg, err := LoadConfigForProbe(path)
			if err != nil {
				t.Fatalf("paper options config must load before promotion: %v", err)
			}
			ss := NewStatusServer(NewAppState(), &sync.RWMutex{}, "", cfg.Strategies, nil)
			ss.SetConfigContext(path, &Config{IntervalSeconds: 300})
			ss.restartFn = func() error {
				t.Errorf("restart requested by a refused promotion")
				return nil
			}
			wantReason := fmt.Sprintf("strategy %q: %s; paper-to-live is refused", id, optionsPaperOnlyReason("deribit"))

			body := fmt.Sprintf(`{"action":"paper-to-live","strategy_id":%q,"params":{}}`, id)
			w := mutationPost(ss, ss.handleAPIConfirm, "/api/confirm", body, nil)
			if w.Code != http.StatusBadRequest {
				t.Errorf("confirm status = %d, want 400; body %s", w.Code, w.Body.String())
			}
			var confirmErr struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &confirmErr); err != nil {
				t.Fatalf("parse confirm body %s: %v", w.Body.String(), err)
			}
			if confirmErr.Error != wantReason {
				t.Errorf("confirm error = %q, want %q", confirmErr.Error, wantReason)
			}

			msg, err := ss.executePaperToLive(id)
			if err == nil {
				t.Fatalf("executePaperToLive succeeded: %s", msg)
			}
			if err.Error() != wantReason {
				t.Errorf("executePaperToLive error = %q, want %q", err.Error(), wantReason)
			}
			if after := mustReadFile(t, path); !bytes.Equal(after, original) {
				t.Fatalf("config changed after a refused paper-to-live")
			}

			var flipped []string
			err = ss.mutateConfigRoot(func(root map[string]json.RawMessage) error {
				_, a, e := flipStrategyToLive(root, id)
				flipped = a
				return e
			})
			if !isLiveArgs(flipped) {
				t.Fatalf("flipStrategyToLive produced %v, want a live mode", flipped)
			}
			if err == nil {
				t.Fatalf("backstop accepted a flipped live options config")
			}
			if !strings.Contains(err.Error(), "patched config invalid") || !strings.Contains(err.Error(), optionsLiveModeConfigError("deribit")) {
				t.Errorf("backstop error = %v, want the patched-config gate refusal", err)
			}
			if after := mustReadFile(t, path); !bytes.Equal(after, original) {
				t.Fatalf("config changed after the backstop refused the flip")
			}
			if left := optionsPromotionTempFiles(t, dir); len(left) != 0 {
				t.Errorf("temp config files left behind: %v", left)
			}

			if err := ss.mutateConfigRoot(func(map[string]json.RawMessage) error { return nil }); err != nil {
				t.Fatalf("control: unchanged config write failed: %v", err)
			}
			if _, err := LoadConfigForProbe(path); err != nil {
				t.Fatalf("control: config no longer loads after unchanged write: %v", err)
			}
			if left := optionsPromotionTempFiles(t, dir); len(left) != 0 {
				t.Errorf("temp config files left behind after control write: %v", left)
			}
		})
	}
}

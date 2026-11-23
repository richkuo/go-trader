package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"os"
	"strings"
	"testing"

	"trading-scheduler/internal/egresspolicy"
)

const egressTestUser = "fixtureuser"

func egressSentinel(t *testing.T) string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "sntl" + base64.RawURLEncoding.EncodeToString(b)
}

func TestEgressConfigAndEntryRefusals(t *testing.T) {
	t.Run("ConfigBlockIsStrictAndFrozen", func(t *testing.T) {
		dir := t.TempDir()
		write := func(name, body string) string {
			p := dir + "/" + name
			if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			return p
		}
		base := `{"config_version": 20, "interval_seconds": 60, "db_file": "` + dir + `/s.db", "strategies": [], %s}`
		unknown := write("unknown.json", strings.Replace(base, "%s", `"egress": {"proxy_required": true, "proxy_url": "http://x:1"}`, 1))
		if _, err := LoadConfigReadOnly(unknown); err == nil || !strings.Contains(err.Error(), "proxy_url") {
			t.Fatalf("unknown egress key accepted: %v", err)
		}
		wrongType := write("type.json", strings.Replace(base, "%s", `"egress": {"proxy_required": "yes"}`, 1))
		if _, err := LoadConfigReadOnly(wrongType); err == nil {
			t.Fatal("non-boolean egress value accepted")
		}
		if _, err := LoadConfigForProbe(wrongType); err == nil {
			t.Fatal("probe config path accepted a non-boolean egress value")
		}
		on := write("on.json", strings.Replace(base, "%s", `"egress": {"proxy_required": true}`, 1))
		off := write("off.json", strings.Replace(base, "%s", `"log_dir": "logs"`, 1))
		cur, err := LoadConfigReadOnly(off)
		if err != nil {
			t.Fatal(err)
		}
		next, err := LoadConfigReadOnly(on)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateHotReloadCompatible(cur, next); err == nil || !strings.Contains(err.Error(), "egress changed") {
			t.Fatalf("scheduler reload accepted an egress change: %v", err)
		}
		sockDir, err := os.MkdirTemp("/tmp", "eg")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(sockDir) })
		feedBase := `{"role": "feed", "status_port": 9301, "feed": {"source": "websocket", "socket_path": "` + sockDir + `/f.sock", "consumer_configs": ["` + off + `"]}%s}`
		feedUnknown := write("feed-unknown.json", strings.Replace(feedBase, "%s", `, "egress": {"notify": true}`, 1))
		if _, err := loadFeedRoleConfig(feedUnknown); err == nil || !strings.Contains(err.Error(), "egress") {
			t.Fatalf("feed accepted an unknown egress key: %v", err)
		}
		feedOn := write("feed-on.json", strings.Replace(feedBase, "%s", `, "egress": {"proxy_required": true}`, 1))
		feedOff := write("feed-off.json", strings.Replace(feedBase, "%s", ``, 1))
		fOn, errOn := loadFeedRoleConfig(feedOn)
		fOff, errOff := loadFeedRoleConfig(feedOff)
		if errOn != nil || errOff != nil {
			t.Fatalf("feed configs: %v %v", errOn, errOff)
		}
		if errs := egressReloadErrors(fOff, fOn); len(errs) == 0 {
			t.Fatal("feed reload accepted an egress change")
		}
		peeked, err := peekEgressConfig(feedOn)
		if err != nil || peeked == nil || !peeked.ProxyRequired {
			t.Fatalf("peek: %+v %v", peeked, err)
		}
	})
	t.Run("EntryRefusesBeforeExternalAccess", func(t *testing.T) {
		sentinel := egressSentinel(t)
		cases := []struct {
			name string
			cfg  *EgressConfig
			env  string
			want string
		}{
			{"required and unset", &EgressConfig{ProxyRequired: true}, "", "proxy_required is true"},
			{"direct notifications without proxy mode", &EgressConfig{NotificationsDirect: true}, "", "notifications_direct"},
			{"malformed with proxy optional", nil, "http://" + egressTestUser + ":" + sentinel + "@proxy.example/path", "must not have a path"},
			{"unsupported scheme", nil, "ftp://" + egressTestUser + ":" + sentinel + "@proxy.example:21", "unsupported or missing scheme"},
			{"missing port", nil, "http://" + egressTestUser + ":" + sentinel + "@proxy.example", "explicit port"},
			{"scheme-less credentials", nil, egressTestUser + ":" + sentinel + "@proxy.example:1080", "scheme"},
			{"valid URL without enforcement", nil, "http://" + egressTestUser + ":" + sentinel + "@proxy.example:3128", "enforcement is unavailable"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				prev := processEgress
				processEgress = &egressRuntime{}
				t.Cleanup(func() { processEgress = prev })
				t.Setenv(egresspolicy.EnvProxy, tc.env)
				t.Setenv(egresspolicy.EnvDeployment, "nosuchdeployment")
				err := establishProcessEgress(context.Background(), tc.cfg, true)
				if err == nil {
					t.Fatal("entry accepted the policy")
				}
				if !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("error %q does not name %q", err, tc.want)
				}
				if strings.Contains(err.Error(), sentinel) || strings.Contains(err.Error(), egressTestUser) {
					t.Fatalf("error leaks credentials: %v", err)
				}
				if processEgress.isActive() {
					t.Fatal("a refused policy left proxy mode active")
				}
			})
		}
	})
}

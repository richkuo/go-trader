//go:build pyintegration

package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func egressPythonRepoRoot(t *testing.T) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(wd)
	if _, err := os.Stat(filepath.Join(root, ".venv", "bin", "python3")); err != nil {
		t.Fatalf("the repository virtualenv is required (uv sync): %v", err)
	}
	t.Chdir(root)
}

func TestEgressPythonChildrenRunUnderTheFrozenPolicy(t *testing.T) {
	ca := newEgressTestCA(t)
	venue := startEgressFakeVenue(t, ca)
	sentinel := egressSentinel(t)
	proxy := startEgressTestProxy(t, "socks5", egressTestUser, sentinel, venue.addr, nil)
	_, port, _ := strings.Cut(proxy.ln.Addr().String(), ":")
	egressPythonRepoRoot(t)
	installTestEgress(t, "socks5://"+egressTestUser+":"+sentinel+"@localhost:"+port, ca.pool, nil)

	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, egressPEM(ca), 0o644); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "egress_child.py")
	if err := os.WriteFile(script, []byte(`import json, os, sys
import requests
r = requests.post("https://api.hyperliquid.xyz/info", json={"type": "allMids"}, timeout=10, verify=os.environ["FIXTURE_CA"])
print(json.dumps({
    "argv0": sys.argv[0],
    "args": sys.argv[1:],
    "mids": r.json(),
    "http_proxy": os.environ.get("HTTP_PROXY", "").split("@")[-1],
    "http_proxy_scheme": os.environ.get("HTTP_PROXY", "").split("://")[0],
    "no_proxy": os.environ.get("NO_PROXY", ""),
    "mode": os.environ.get("GO_TRADER_EGRESS_MODE", ""),
}))
print("child-stderr-line", file=sys.stderr)
`), 0o644); err != nil {
		t.Fatal(err)
	}
	overrides := map[string]string{
		"FIXTURE_CA":  caFile,
		"HTTP_PROXY":  "http://127.0.0.1:9",
		"HTTPS_PROXY": "http://127.0.0.1:9",
		"NO_PROXY":    "*",
	}
	stdout, stderr, err := spawnPythonProcessWithEnv(context.Background(), script, []string{"BTC", "--mode=paper"}, nil, time.Minute, overrides)
	if err != nil {
		t.Fatalf("child failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}
	var out struct {
		Argv0     string            `json:"argv0"`
		Args      []string          `json:"args"`
		Mids      map[string]string `json:"mids"`
		HTTPProxy string            `json:"http_proxy"`
		Scheme    string            `json:"http_proxy_scheme"`
		NoProxy   string            `json:"no_proxy"`
		Mode      string            `json:"mode"`
	}
	if err := json.Unmarshal(stdout, &out); err != nil {
		t.Fatalf("child stdout %q: %v", stdout, err)
	}
	if out.Argv0 != script || strings.Join(out.Args, " ") != "BTC --mode=paper" {
		t.Fatalf("the bootstrap changed the script arguments: %+v", out)
	}
	if out.Mids["BTC"] != "65000.5" {
		t.Fatalf("child request did not reach the venue through the proxy: %+v", out)
	}
	if out.Scheme != "socks5h" || out.HTTPProxy != "localhost:"+port || out.NoProxy != egressNoProxy || out.Mode != "proxy" {
		t.Fatalf("per-call overrides survived: %+v", out)
	}
	recs := proxy.records()
	if len(recs) == 0 || recs[len(recs)-1].Target != "api.hyperliquid.xyz:443" || !recs[len(recs)-1].Domain {
		t.Fatalf("socks proxy records %+v", recs)
	}
	if strings.Contains(string(stderr), egressOutcomePrefix) {
		t.Fatalf("the child outcome line reached stderr consumers: %s", stderr)
	}
	if !strings.Contains(string(stderr), "child-stderr-line") {
		t.Fatalf("ordinary stderr was lost: %q", stderr)
	}
	st := processEgress.status()
	if st.ChildRequests < 1 || st.ChildLastSuccessAt == nil {
		t.Fatalf("status did not record the child outcome: %+v", st)
	}
	for _, b := range [][]byte{stdout, stderr} {
		if strings.Contains(string(b), sentinel) {
			t.Fatalf("child output holds the credential: %s", b)
		}
	}

	probeScript := filepath.Join(t.TempDir(), "probe_child.py")
	if err := os.WriteFile(probeScript, []byte(`import os, sys
sys.exit(0 if os.environ.get("GO_TRADER_EGRESS_MODE") == "proxy" and os.environ.get("ALL_PROXY", "").startswith("socks5h://") and sys.argv[1:] == ["--probe-only"] else 3)
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := probeOneCheckScript(probeScript, []string{"--probe-only"}); err != nil {
		t.Fatalf("the probe launcher does not apply the policy: %v", err)
	}

	if err := egressPythonPolicyCheck(context.Background(), []string{"ccxt", "hyperliquid", "requests", "yfinance"}); err != nil {
		t.Fatalf("python client policy check: %v", err)
	}
}

func TestEgressPythonChildFailsClosedWithoutPolicy(t *testing.T) {
	egressPythonRepoRoot(t)
	marker := filepath.Join(t.TempDir(), "ran")
	script := filepath.Join(t.TempDir(), "never.py")
	if err := os.WriteFile(script, []byte("open("+strconvQuote(marker)+", 'w').write('x')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := spawnPythonProcessRaw(context.Background(), ".venv/bin/python3", []string{egressBootstrapScript, script}, nil, time.Minute, append(os.Environ(), "GO_TRADER_EGRESS_MODE=proxy", "GO_TRADER_EGRESS_PROXY="))
	if err == nil {
		t.Fatal("the bootstrap ran a child with no proxy URL")
	}
	if !strings.Contains(string(stdout), `"error"`) {
		t.Fatalf("missing error response: %s", stdout)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("the target script ran before the policy check")
	}
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

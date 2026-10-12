//go:build pyintegration

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func entryOperatorRepoRoot(t *testing.T) string {
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

func TestEntryOperatorRealOKXQueryErrorRefusesTheRelease(t *testing.T) {
	root := entryOperatorRepoRoot(t)
	f := newEntryOperatorFixture(t, "okx-swap")
	f.backdate()
	t.Setenv("OKX_API_SECRET", "")
	t.Setenv("OKX_PASSPHRASE", "")
	t.Chdir(root)
	cashBefore := f.cash()

	res, _, err := RunOKXRecoverEntry("shared_scripts/check_okx.py", "BTC", "swap", "gtabc", 1, 2)
	if err == nil || res == nil {
		t.Fatalf("the real script must return its parsed result and an error: res=%+v err=%v", res, err)
	}
	if res.Account != okxEntryAccountKey("swap") {
		t.Fatalf("script account %q, scheduler account %q", res.Account, okxEntryAccountKey("swap"))
	}

	msg := f.command()
	if !strings.Contains(msg, "venue query failed") || !strings.Contains(msg, "requires live mode") {
		t.Fatalf("reply %q", msg)
	}
	f.assertUnchanged(msg, cashBefore)
}

func TestEntryAccountKeysMatchTheRecoveryScripts(t *testing.T) {
	root := entryOperatorRepoRoot(t)
	useEntryTestHLAccount(t)
	useEntryTestOKXAccount(t)
	for _, network := range []string{"", "1"} {
		t.Setenv("HYPERLIQUID_TESTNET", network)
		t.Setenv("OKX_SANDBOX", network)
		driver := `import sys
sys.path.insert(0, sys.argv[1] + "/shared_scripts")
import check_hyperliquid, check_okx
print(check_hyperliquid.hyperliquid_account_key(sys.argv[2]))
print(check_okx.okx_account_key("spot"))
print(check_okx.okx_account_key("swap"))
`
		cmd := exec.Command(filepath.Join(root, ".venv", "bin", "python3"), "-c", driver, root, "0x"+strings.ToUpper(entryTestHLAddress[2:]))
		cmd.Dir = t.TempDir()
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("driver: %v", err)
		}
		got := strings.Split(strings.TrimSpace(string(out)), "\n")
		want := []string{hyperliquidEntryAccountKey(), okxEntryAccountKey("spot"), okxEntryAccountKey("swap")}
		if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
			t.Fatalf("network %q: script keys %q, scheduler keys %q", network, got, want)
		}
	}
}

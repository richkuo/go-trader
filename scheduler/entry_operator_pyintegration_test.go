//go:build pyintegration

package main

import (
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

const entryHLRecoveryDriver = `import json, runpy, sys, types
script, spec = sys.argv[1], json.loads(sys.argv[2])
argv = sys.argv[3:]
cloids = {a.split("=", 1)[0]: a.split("=", 1)[1].lower() for a in argv if a.startswith("--cloid=") or a.startswith("--stop-cloid=")}
mod = types.ModuleType("adapter")
class HyperliquidExchangeAdapter:
    account_address = spec["account"]
    def query_order_by_cloid(self, cloid):
        if cloid.lower() == cloids.get("--cloid") and spec["entry"] is not None:
            return spec["entry"]
        return {"status": "unknownOid"}
    def user_fills_since(self, since_ms):
        return [dict(f, time=since_ms + 1 + i, tid=i + 1) for i, f in enumerate(spec["fills"])]
    def query_order_by_oid(self, oid):
        raise ConnectionError("connection reset by peer")
mod.HyperliquidExchangeAdapter = HyperliquidExchangeAdapter
sys.modules["adapter"] = mod
sys.argv = [script] + argv
runpy.run_path(script, run_name="__main__")
`

func installEntryHLRecoveryDriver(t *testing.T, root string, entry map[string]any) {
	t.Helper()
	spec, err := json.Marshal(map[string]any{
		"account": os.Getenv("HYPERLIQUID_ACCOUNT_ADDRESS"),
		"entry":   entry,
		"fills": []map[string]any{
			{"oid": 901, "sz": "0.25", "px": "100", "fee": "0.01", "coin": "BTC"},
			{"oid": 901, "sz": "0.15", "px": "101", "fee": "0.008", "coin": "BTC"},
			{"oid": 555, "sz": "1", "px": "100", "fee": "0.02", "coin": "BTC"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	prev := runHyperliquidRecoverEntryFn
	t.Cleanup(func() { runHyperliquidRecoverEntryFn = prev })
	runHyperliquidRecoverEntryFn = func(script, symbol, cloid, stopCloid string, sinceMs, untilMs int64) (*entryRecoveryResult, string, error) {
		args := append([]string{"-c", entryHLRecoveryDriver, filepath.Join(root, script), string(spec)}, buildHyperliquidRecoverEntryArgs(symbol, cloid, stopCloid, sinceMs, untilMs)...)
		cmd := exec.Command(filepath.Join(root, ".venv", "bin", "python3"), args...)
		cmd.Dir = root
		var stderr strings.Builder
		cmd.Stderr = &stderr
		stdout, err := cmd.Output()
		return parseEntryRecoveryOutput(stdout, stderr.String(), err)
	}
}

func TestEntryRecoveryRealHLOrderLookupFailureOnlyBlocksTheRelease(t *testing.T) {
	root := entryOperatorRepoRoot(t)
	t.Run("a found filled entry books once when an unattributed fill lookup fails", func(t *testing.T) {
		f := newEntryOperatorFixture(t, "hyperliquid")
		f.backdate()
		installEntryHLRecoveryDriver(t, root, map[string]any{"status": "order", "order": map[string]any{"status": "filled",
			"order": map[string]any{"oid": 901, "origSz": "0.4", "sz": "0", "cloid": f.intent().ClientOrderID}}})
		recoverEntryIntents(f.env.cfg, f.env.state, f.env.store, &f.env.mu, nil, nil, time.Now())
		recoverEntryIntents(f.env.cfg, f.env.state, f.env.store, &f.env.mu, nil, nil, time.Now().Add(2*entryRecoveryQueryInterval))
		if qty, _ := f.env.position(f.sc.ID, "BTC"); math.Abs(qty-0.4) > 1e-9 {
			t.Fatalf("position %v, want the proven 0.4 booked", qty)
		}
		if n := f.env.openTradeRows(f.sc.ID); n != 1 {
			t.Fatalf("open trade rows %d, want 1", n)
		}
		if got := f.env.intentStatuses(f.sc.ID); len(got) != 1 || got[0] != entryIntentCommitted {
			t.Fatalf("intents %v", got)
		}
	})
	t.Run("a missing entry stays held when an unattributed fill lookup fails", func(t *testing.T) {
		f := newEntryOperatorFixture(t, "hyperliquid")
		f.backdate()
		installEntryHLRecoveryDriver(t, root, nil)
		cashBefore := f.cash()
		recoverEntryIntents(f.env.cfg, f.env.state, f.env.store, &f.env.mu, nil, nil, time.Now())
		if r := f.row(); r.Status != entryIntentSubmitting || !strings.Contains(r.Hold, "lookup failed") {
			t.Fatalf("row %+v", r)
		}
		msg := f.command()
		if !strings.Contains(msg, "order 901 lookup failed") {
			t.Fatalf("reply %q", msg)
		}
		f.assertUnchanged(msg, cashBefore)
	})
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

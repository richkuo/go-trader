//go:build pyintegration

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

type quoteReplayPythonVerdict struct {
	OK          bool     `json:"ok"`
	Problems    []string `json:"problems"`
	AppliedFill int      `json:"applied_fills"`
	RoundTrips  []any    `json:"round_trips"`
	Funding     []any    `json:"funding_payments"`
}

func quoteReplayPythonVerify(t *testing.T, spec *quoteReplaySpec, out quoteReplayOutput) quoteReplayPythonVerdict {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	planPath, outPath := filepath.Join(dir, "plan.json"), filepath.Join(dir, "out.json")
	writeQuoteReplaySpec(t, spec, planPath)
	blob, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outPath, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(root, ".venv", "bin", "python3"), filepath.Join(root, "backtest", "quote_replay.py"), "verify", "--plan", planPath, "--output", outPath)
	cmd.Dir = root
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	_ = cmd.Run()
	var v quoteReplayPythonVerdict
	if err := json.Unmarshal(stdout.Bytes(), &v); err != nil {
		t.Fatalf("the Python verifier printed no verdict: %v\n%s\n%s", err, stdout.String(), stderr.String())
	}
	return v
}

func TestQuoteReplayPythonIndependentLedgerMatchesEverySettlement(t *testing.T) {
	spec := quoteReplayTestSpec(t, quoteReplayTempDir(t), defaultQuoteReplayMarket(), nil)
	out, code := runQuoteReplayForTest(t, spec)
	quoteReplayMustComplete(t, out, code)
	v := quoteReplayPythonVerify(t, spec, out)
	if !v.OK || v.AppliedFill != len(out.Fills) || len(v.RoundTrips) == 0 || len(v.Funding) != 1 {
		t.Fatalf("independent ledger: ok %v problems %v fills %d/%d round trips %d funding %d", v.OK, v.Problems, v.AppliedFill, len(out.Fills), len(v.RoundTrips), len(v.Funding))
	}
	tampered := out
	book := *out.Book
	book.Cash = book.Cash + "1"
	tampered.Book = &book
	if v := quoteReplayPythonVerify(t, spec, tampered); v.OK {
		t.Fatal("the independent ledger accepted a changed settled cash")
	}
	bad := out
	bad.VenueEvents = append([]quoteReplayVenueEventRow(nil), out.VenueEvents...)
	for i, e := range bad.VenueEvents {
		if e.Kind != "fill" {
			continue
		}
		var p map[string]any
		if err := json.Unmarshal(e.Event, &p); err != nil {
			t.Fatal(err)
		}
		p["fill"].(map[string]any)["px"] = "1"
		blob, _ := json.Marshal(p)
		bad.VenueEvents[i].Event = blob
		break
	}
	if v := quoteReplayPythonVerify(t, spec, bad); v.OK {
		t.Fatal("the independent fill checks accepted a maker fill away from its order price")
	}
}

func TestQuoteReplayPythonLedgerAppliesAFillAtTheFinalReadTime(t *testing.T) {
	m := defaultQuoteReplayMarket()
	m.sweepEvery = 0
	last := quoteReplayTestStart + 60_000
	spec := quoteReplayTestSpec(t, quoteReplayTempDir(t), m, func(s *quoteReplaySpec) {
		s.Window.LastDeadlineMs = last
		quoteReplayAddTrades(s, quoteReplayTradeSpec{RecvMs: last, TimeMs: last - 10, Tid: 9_300_001, Side: "A", Px: "99000", Sz: "1", Hash: "0xread"})
	})
	out, code := runQuoteReplayForTest(t, spec)
	quoteReplayMustComplete(t, out, code)
	atRead := 0
	for _, f := range out.Fills {
		if f.TimeMs == last && f.SettlementID != 0 && out.AppliedBoundary == last-1 {
			atRead++
		}
	}
	if atRead == 0 {
		t.Fatalf("no fill settled at the final read time %d (boundary %d)", last, out.AppliedBoundary)
	}
	if v := quoteReplayPythonVerify(t, spec, out); !v.OK || v.AppliedFill != len(out.Fills) {
		t.Fatalf("the independent ledger must apply fills through boundary+1: %v (%d of %d)", v.Problems, v.AppliedFill, len(out.Fills))
	}
}

func TestQuoteReplayPythonRecoveredLedgerEqualsItsDurableEvents(t *testing.T) {
	for _, fault := range []string{"after-call:16", "venue-before-persist:53", "issue-before-reserve:18"} {
		t.Run(fault, func(t *testing.T) {
			spec := quoteReplayShortSpec(t, quoteReplayTempDir(t))
			_, code, _ := runQuoteReplaySubprocess(t, spec, "--fault", fault)
			if code != basketReplayExitFault {
				t.Fatalf("fault %s exited %d", fault, code)
			}
			spec.Mode = basketReplayModeResume
			out, code := runQuoteReplayForTest(t, spec)
			quoteReplayMustComplete(t, out, code)
			if v := quoteReplayPythonVerify(t, spec, out); !v.OK {
				t.Fatalf("the recovered ledger differs from the independent ledger of its durable events: %v", v.Problems)
			}
		})
	}
}

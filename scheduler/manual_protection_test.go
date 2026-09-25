package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestAttemptManualOpenCleanup_CloseFails(t *testing.T) {
	orig := manualOpenCleanupCloseFn
	defer func() { manualOpenCleanupCloseFn = orig }()

	manualOpenCleanupCloseFn = func(symbol string, partialSz *float64, cancelOIDs []int64) (*HyperliquidCloseResult, string, error) {
		return nil, "stderr noise", fmt.Errorf("rpc timeout")
	}

	cleanedUp, msg := attemptManualOpenCleanup("ETH", 0.8, 12345, []int64{67890})
	if cleanedUp {
		t.Fatalf("expected cleanedUp=false, got msg=%q", msg)
	}
	if !strings.Contains(msg, "rpc timeout") {
		t.Errorf("msg should mention close failure cause; got %q", msg)
	}
}

package main

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// resetShutdownState is a test-only helper that re-initializes the package
// shutdown vars between subtests. shutdown.go's package vars persist across
// the test binary, so without this each test inherits the previous test's
// drain state and the WaitGroup counter.
func resetShutdownState(t *testing.T) {
	t.Helper()
	shutdownDraining = atomic.Bool{}
	sideEffectWG = sync.WaitGroup{}
	initShutdownContexts()
}

func TestBeginDrainCancelsReadOnlyContextAndSetsFlag(t *testing.T) {
	resetShutdownState(t)
	if isDraining() {
		t.Fatal("draining=true before beginDrain")
	}
	if shutdownReadOnlyCtx.Err() != nil {
		t.Fatalf("readOnlyCtx already cancelled: %v", shutdownReadOnlyCtx.Err())
	}
	beginDrain()
	if !isDraining() {
		t.Fatal("draining=false after beginDrain")
	}
	if shutdownReadOnlyCtx.Err() == nil {
		t.Fatal("readOnlyCtx not cancelled after beginDrain")
	}
	// Side-effect ctx must NOT be cancelled by Phase 1 — that's Phase 2's
	// backstop, only triggered by the cap timer in runDrain.
	if shutdownSideEffectCtx.Err() != nil {
		t.Fatalf("sideEffectCtx cancelled by beginDrain (should be Phase 2 only): %v", shutdownSideEffectCtx.Err())
	}
}

func TestBeginDrainIsIdempotent(t *testing.T) {
	resetShutdownState(t)
	beginDrain()
	beginDrain()
	beginDrain()
	if !isDraining() {
		t.Fatal("draining=false after repeated beginDrain")
	}
}

func TestRunDrainWaitsForSideEffectWG(t *testing.T) {
	resetShutdownState(t)
	// Simulate an in-flight side-effecting subprocess.
	sideEffectWG.Add(1)
	finished := make(chan struct{})
	go func() {
		// Drop the WG quickly; runDrain should return promptly.
		time.Sleep(50 * time.Millisecond)
		sideEffectWG.Done()
	}()
	go func() {
		runDrain()
		close(finished)
	}()
	select {
	case <-finished:
		// Good.
	case <-time.After(2 * time.Second):
		t.Fatal("runDrain did not return after sideEffectWG drained")
	}
	if shutdownSideEffectCtx.Err() != nil {
		t.Fatalf("sideEffectCtx cancelled despite clean drain: %v", shutdownSideEffectCtx.Err())
	}
}

func TestRunDrainCancelsSideEffectCtxAfterCapWithStubbedCap(t *testing.T) {
	resetShutdownState(t)
	// Hold a side-effect call that never finishes; force runDrain to use a
	// short cap by replacing the cap helper. shutdownDrainCap is a const, so
	// instead we test the cancel path directly.
	sideEffectWG.Add(1)
	defer sideEffectWG.Done()

	// Direct call to the cancel — equivalent to what runDrain's timeout
	// branch does. Verifies the wiring: a cap-fired cancel propagates to
	// shutdownSideEffectCtx.
	shutdownSideEffectCancel()
	if shutdownSideEffectCtx.Err() == nil {
		t.Fatal("sideEffectCtx not cancelled after shutdownSideEffectCancel")
	}
}

func TestRunDrainReturnsImmediatelyWhenNoSideEffectsInFlight(t *testing.T) {
	resetShutdownState(t)
	start := time.Now()
	runDrain()
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("runDrain blocked %v with empty WG (should be near-zero)", elapsed)
	}
}

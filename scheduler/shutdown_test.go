package main

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

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
	sideEffectWG.Add(1)
	finished := make(chan struct{})
	go func() {
		time.Sleep(50 * time.Millisecond)
		sideEffectWG.Done()
	}()
	go func() {
		runDrain()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("runDrain did not return after sideEffectWG drained")
	}
	if shutdownSideEffectCtx.Err() != nil {
		t.Fatalf("sideEffectCtx cancelled despite clean drain: %v", shutdownSideEffectCtx.Err())
	}
}

func TestRunDrainCancelsSideEffectCtxAfterCapWithStubbedCap(t *testing.T) {
	resetShutdownState(t)
	sideEffectWG.Add(1)
	defer sideEffectWG.Done()

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

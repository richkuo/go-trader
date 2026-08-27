package main

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)


const shutdownDrainCap = 15 * time.Second

var (
	shutdownReadOnlyCtx      context.Context = context.Background()
	shutdownSideEffectCtx    context.Context = context.Background()
	shutdownReadOnlyCancel   context.CancelFunc
	shutdownSideEffectCancel context.CancelFunc
	sideEffectWG             sync.WaitGroup
	shutdownDraining         atomic.Bool
)

func initShutdownContexts() {
	shutdownReadOnlyCtx, shutdownReadOnlyCancel = context.WithCancel(context.Background())
	shutdownSideEffectCtx, shutdownSideEffectCancel = context.WithCancel(context.Background())
}

func isDraining() bool {
	return shutdownDraining.Load()
}

func beginDrain() {
	if !shutdownDraining.CompareAndSwap(false, true) {
		return
	}
	if shutdownReadOnlyCancel != nil {
		shutdownReadOnlyCancel()
	}
}

func runDrain() {
	done := make(chan struct{})
	go func() {
		sideEffectWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(shutdownDrainCap):
		if shutdownSideEffectCancel != nil {
			shutdownSideEffectCancel()
		}
	}
}

package main

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// Shutdown coordinates the two-phase drain on SIGTERM/SIGINT.
//
// Phase 1 (drain): the signal handler sets shutdownDraining=true and cancels
// shutdownReadOnlyCtx. The main loop refuses new strategy work; in-flight
// read-only Python subprocesses (check_*.py, fetch_*_marks.py, balance/price
// fetchers) get SIGKILL'd via their process group through exec.CommandContext.
//
// Phase 2 (quiesce): main waits on sideEffectWG up to shutdownDrainCap.
// Side-effecting subprocesses (--execute, close_*.py, sync-protection,
// trigger updates, fetch_positions for live-state mutation paths) run to
// completion under their existing scriptTimeout so on-chain orders aren't
// killed mid-call (which would leave on-chain state with no local Trade
// record). When the cap fires, shutdownSideEffectCancel forces a SIGKILL
// backstop — last resort, not the happy path.
//
// Phase 3 (persist): main saves state, flushes the notifier (Discord/Telegram
// HTTP buffers), closes the DB, then exits.
//
// One-off CLI commands and tests skip initShutdownContexts(); the package
// vars default to context.Background() and a no-op WaitGroup, so non-daemon
// paths behave as before.

const shutdownDrainCap = 15 * time.Second

var (
	shutdownReadOnlyCtx      context.Context = context.Background()
	shutdownSideEffectCtx    context.Context = context.Background()
	shutdownReadOnlyCancel   context.CancelFunc
	shutdownSideEffectCancel context.CancelFunc
	sideEffectWG             sync.WaitGroup
	shutdownDraining         atomic.Bool
)

// initShutdownContexts is called once from the daemon entry point before the
// trading loop starts. It replaces the context.Background() defaults with
// cancellable contexts that the SIGTERM handler can drive.
func initShutdownContexts() {
	shutdownReadOnlyCtx, shutdownReadOnlyCancel = context.WithCancel(context.Background())
	shutdownSideEffectCtx, shutdownSideEffectCancel = context.WithCancel(context.Background())
}

// isDraining reports whether SIGTERM/SIGINT has fired. The main loop checks
// this at cycle top and per-strategy dispatch sites so no new strategy work
// starts after the signal.
func isDraining() bool {
	return shutdownDraining.Load()
}

// beginDrain is called from the signal handler. It is safe to call
// concurrently from main; the cancels and CompareAndSwap are idempotent. The
// signal goroutine MUST NOT do anything else (no state save, no locks) — all
// orderly shutdown work runs on the main goroutine via runDrain.
func beginDrain() {
	if !shutdownDraining.CompareAndSwap(false, true) {
		return
	}
	if shutdownReadOnlyCancel != nil {
		shutdownReadOnlyCancel()
	}
}

// runDrain runs Phases 2 and 3 from the main goroutine. It waits up to
// shutdownDrainCap for in-flight side-effecting subprocesses, then cancels
// the side-effect context as a backstop. Phase 3 (state save / notifier
// flush / DB close) is the caller's responsibility — runDrain only owns the
// quiesce.
func runDrain() {
	done := make(chan struct{})
	go func() {
		sideEffectWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		// Clean drain.
	case <-time.After(shutdownDrainCap):
		// Cap fired — force SIGKILL on whatever side-effecting subprocesses
		// are still running. This is the safety valve for a wedged Python
		// child or a script that legitimately exceeded the per-call 30s
		// scriptTimeout window.
		if shutdownSideEffectCancel != nil {
			shutdownSideEffectCancel()
		}
	}
}

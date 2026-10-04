//go:build !linux

package main

import (
	"fmt"
	"io"
	"runtime"
)

func ledgerCaptureConfinementAvailable() error {
	return fmt.Errorf("capture needs a Linux private mount namespace to give SQLite a read-only view of the state directories; %s has no such mechanism, so capture is refused (run it on the Linux host that owns the state files)", runtime.GOOS)
}

func runCaptureWorker(plan captureWorkerPlan) (captureWorkerResult, bool, error) {
	return captureWorkerResult{}, false, ledgerCaptureConfinementAvailable()
}

func ledgerCaptureWorkerMain(r io.Reader) (captureWorkerResult, error) {
	return captureWorkerResult{}, ledgerCaptureConfinementAvailable()
}

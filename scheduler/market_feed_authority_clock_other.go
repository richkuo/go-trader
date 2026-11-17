//go:build !linux && !darwin

package main

import (
	"fmt"
	"runtime"
)

const feedBootClockSource = "unsupported"

func readFeedBootIdentity() (string, error) {
	return "", fmt.Errorf("the shared feed weight authority has no boot clock on %s; only linux (and darwin for development) are supported", runtime.GOOS)
}

func readFeedBootElapsed() (int64, error) {
	return 0, fmt.Errorf("the shared feed weight authority has no boot clock on %s; only linux (and darwin for development) are supported", runtime.GOOS)
}

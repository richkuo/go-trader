//go:build darwin

package main

import (
	"fmt"
	"strings"

	"golang.org/x/sys/unix"
)

const feedBootClockSource = "darwin CLOCK_MONOTONIC and kern.bootsessionuuid (development only)"

func readFeedBootIdentity() (string, error) {
	id, err := unix.Sysctl("kern.bootsessionuuid")
	if err != nil {
		return "", fmt.Errorf("read kern.bootsessionuuid: %w", err)
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return "", fmt.Errorf("kern.bootsessionuuid is empty")
	}
	return id, nil
}

func readFeedBootElapsed() (int64, error) {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
		return 0, fmt.Errorf("read CLOCK_MONOTONIC: %w", err)
	}
	return ts.Nano(), nil
}

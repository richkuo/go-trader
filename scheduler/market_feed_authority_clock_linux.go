//go:build linux

package main

import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

const feedBootClockSource = "linux CLOCK_BOOTTIME and /proc/sys/kernel/random/boot_id"

func readFeedBootIdentity() (string, error) {
	raw, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", fmt.Errorf("read boot identity: %w", err)
	}
	id := strings.TrimSpace(string(raw))
	if id == "" {
		return "", fmt.Errorf("boot identity /proc/sys/kernel/random/boot_id is empty")
	}
	return id, nil
}

func readFeedBootElapsed() (int64, error) {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &ts); err != nil {
		return 0, fmt.Errorf("read CLOCK_BOOTTIME: %w", err)
	}
	return ts.Nano(), nil
}

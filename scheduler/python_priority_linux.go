//go:build linux

package main

import "golang.org/x/sys/unix"

func schedulerBaseNice() (int, error) {
	prio, err := unix.Getpriority(unix.PRIO_PROCESS, 0)
	if err != nil {
		return 0, err
	}
	return 20 - prio, nil
}

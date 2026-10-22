//go:build !linux

package main

import (
	"fmt"
	"os"
	"runtime"
)

func main() {
	fmt.Fprintf(os.Stderr, "go-trader-egress-controller runs only on Linux (this host runs %s)\n", runtime.GOOS)
	os.Exit(1)
}

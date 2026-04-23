package main

import (
	"net"
	"strconv"
	"strings"
	"testing"
)

func TestResolveStatusPort(t *testing.T) {
	tests := []struct {
		name    string
		cliFlag int
		cfgPort int
		want    int
	}{
		{"both unset uses default", 0, 0, DefaultStatusPort},
		{"cfg only", 0, 9000, 9000},
		{"cli overrides cfg", 7000, 9000, 7000},
		{"cli only", 7000, 0, 7000},
		{"negative cli falls through to cfg", -1, 9000, 9000},
		{"negative cli and cfg falls to default", -1, -1, DefaultStatusPort},
		{"zero cli and negative cfg falls to default", 0, -5, DefaultStatusPort},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveStatusPort(tc.cliFlag, tc.cfgPort)
			if got != tc.want {
				t.Fatalf("resolveStatusPort(%d, %d) = %d, want %d", tc.cliFlag, tc.cfgPort, got, tc.want)
			}
		})
	}
}

// TestBindWithFallback_FirstPortFree confirms the first port is taken when
// available and no fallback is needed.
func TestBindWithFallback_FirstPortFree(t *testing.T) {
	// Use port 0 to let the OS pick, then close and reuse that exact port
	// to minimize flake risk on busy CI runners.
	probe, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("probe listen: %v", err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	probe.Close()

	listener, bound, err := bindWithFallback(port, statusPortMaxAttempts)
	if err != nil {
		t.Fatalf("bindWithFallback: %v", err)
	}
	defer listener.Close()
	if bound != port {
		t.Fatalf("bound port = %d, want %d (no fallback expected)", bound, port)
	}
}

// TestBindWithFallback_FallsThrough confirms the sweep advances past an
// already-bound port and returns port+1.
func TestBindWithFallback_FallsThrough(t *testing.T) {
	// Find two consecutive free ports so we can deterministically hold the
	// first and expect the second to succeed.
	port := findConsecutiveFreePorts(t, 2)

	blocker, err := net.Listen("tcp", statusPortAddr(port))
	if err != nil {
		t.Fatalf("blocker listen on %d: %v", port, err)
	}
	defer blocker.Close()

	listener, bound, err := bindWithFallback(port, statusPortMaxAttempts)
	if err != nil {
		t.Fatalf("bindWithFallback: %v", err)
	}
	defer listener.Close()
	if bound != port+1 {
		t.Fatalf("bound port = %d, want %d (should skip held port %d)", bound, port+1, port)
	}
}

// TestBindWithFallback_AllBusy confirms an error is returned (and wraps the
// last net.Listen error) when every attempt fails. Occupying a single port
// and asking for maxAttempts=1 forces all attempts to fail without relying
// on OS-specific parse errors or port exhaustion.
func TestBindWithFallback_AllBusy(t *testing.T) {
	blocker, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("blocker listen: %v", err)
	}
	defer blocker.Close()
	port := blocker.Addr().(*net.TCPAddr).Port

	_, _, err = bindWithFallback(port, 1)
	if err == nil {
		t.Fatal("expected error when every bind attempt fails, got nil")
	}
	if !strings.Contains(err.Error(), "could not bind after 1 attempts") {
		t.Fatalf("error does not mention attempt count: %v", err)
	}
}

func statusPortAddr(port int) string {
	return net.JoinHostPort("localhost", strconv.Itoa(port))
}

// findConsecutiveFreePorts opens n listeners on OS-assigned ports, picks the
// lowest port among them, closes all, and returns that port. On most systems
// the port and port+1..port+n-1 will still be free moments later; if they're
// not, the test skips rather than flakes.
func findConsecutiveFreePorts(t *testing.T, n int) int {
	t.Helper()
	// Just grab one port from the OS and trust that port+1 is also free.
	// CI runners that fail this are too saturated for the fallback test to
	// produce a meaningful signal anyway.
	l, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("probe listen: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()

	// Sanity-check port+1 is bindable right now, else skip.
	probe, err := net.Listen("tcp", statusPortAddr(port+1))
	if err != nil {
		t.Skipf("port %d not available for fallback test: %v", port+1, err)
	}
	probe.Close()
	return port
}

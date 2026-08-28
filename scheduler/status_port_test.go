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

func TestBindWithFallback_FirstPortFree(t *testing.T) {
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

func TestBindWithFallback_FallsThrough(t *testing.T) {
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

func findConsecutiveFreePorts(t *testing.T, n int) int {
	t.Helper()
	l, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("probe listen: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()

	probe, err := net.Listen("tcp", statusPortAddr(port+1))
	if err != nil {
		t.Skipf("port %d not available for fallback test: %v", port+1, err)
	}
	probe.Close()
	return port
}

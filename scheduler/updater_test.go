package main

import (
	"strings"
	"testing"
)

func TestTailForDM(t *testing.T) {
	if got := tailForDM("hello", 100); got != "hello" {
		t.Errorf("short input should pass through: %q", got)
	}
	if got := tailForDM("  hello  \n", 100); got != "hello" {
		t.Errorf("should trim space: %q", got)
	}
	long := strings.Repeat("x", 2000)
	got := tailForDM(long, 1500)
	if !strings.HasPrefix(got, "...truncated...\n") {
		t.Errorf("oversized input should be marked truncated: %q", got[:30])
	}
	if len(got) > 1500+len("...truncated...\n") {
		t.Errorf("trimmed output too long: %d", len(got))
	}
}

func TestUpdateSystemdUnitName(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want string
	}{
		{name: "default", want: defaultGoTraderSystemdUnit},
		{name: "override", env: " go-trader-live.service ", want: "go-trader-live.service"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GO_TRADER_SERVICE", tc.env)
			if got := updateSystemdUnitName(); got != tc.want {
				t.Fatalf("expected unit %q, got %q", tc.want, got)
			}
		})
	}
}

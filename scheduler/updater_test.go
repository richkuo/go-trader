package main

import (
	"strings"
	"testing"
)

func TestFormatUpdateMessage(t *testing.T) {
	cases := []struct {
		name      string
		commitLog string
		tag       string
		want      []string
		unwant    []string
	}{
		{
			name:      "commit log",
			commitLog: "abc1234 fix: something\ndef5678 feat: new thing",
			want: []string{
				"abcdef12",
				"12345678",
				"fix: something",
				"scripts/update.sh --restart",
				"GO_TRADER_SERVICE=go-trader-live.service",
				"Update Available",
			},
		},
		{
			name: "tag",
			tag:  "v1.5.0",
			want: []string{"v1.5.0", "New Release"},
		},
		{
			name:   "empty commit log",
			unwant: []string{"```\n\n```"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := formatUpdateMessage("abcdef1234567890", "1234567890abcdef", tc.commitLog, tc.tag)
			for _, want := range tc.want {
				if !strings.Contains(msg, want) {
					t.Errorf("message missing %q", want)
				}
			}
			for _, unwant := range tc.unwant {
				if strings.Contains(msg, unwant) {
					t.Errorf("message contains %q", unwant)
				}
			}
		})
	}
}

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

func TestFormatUpdateMessageTruncatesLongLog(t *testing.T) {
	lines := make([]string, 15)
	for i := range lines {
		lines[i] = "hash" + string(rune('a'+i)) + " commit message"
	}

	msg := formatUpdateMessage("abcdef1234567890", "1234567890abcdef", strings.Join(lines, "\n"), "")
	if !strings.Contains(msg, "... and 5 more") {
		t.Error("should truncate to 10 lines with '... and 5 more'")
	}
}

package main

import (
	"strings"
	"testing"
)

func TestFormatUpdateMessage(t *testing.T) {
	msg := formatUpdateMessage(
		"abcdef1234567890",
		"1234567890abcdef",
		"abc1234 fix: something\ndef5678 feat: new thing",
		"",
	)

	if !strings.Contains(msg, "abcdef12") {
		t.Error("should contain truncated local hash")
	}
	if !strings.Contains(msg, "12345678") {
		t.Error("should contain truncated remote hash")
	}
	if !strings.Contains(msg, "fix: something") {
		t.Error("should contain commit log")
	}
	if !strings.Contains(msg, "scripts/update.sh --restart") {
		t.Error("should point operators at scripts/update.sh --restart")
	}
	if !strings.Contains(msg, "Update Available") {
		t.Error("should contain update header")
	}
}

func TestFormatUpdateMessageWithTag(t *testing.T) {
	msg := formatUpdateMessage(
		"abcdef1234567890",
		"1234567890abcdef",
		"",
		"v1.5.0",
	)

	if !strings.Contains(msg, "v1.5.0") {
		t.Error("should contain release tag")
	}
	if !strings.Contains(msg, "New Release") {
		t.Error("should contain 'New Release' header for tagged releases")
	}
}

func TestFormatUpdateMessageNoCommitLog(t *testing.T) {
	msg := formatUpdateMessage(
		"abcdef1234567890",
		"1234567890abcdef",
		"",
		"",
	)

	if strings.Contains(msg, "```\n\n```") {
		t.Error("should not contain empty code block")
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

func TestFormatUpdateMessageTruncatesLongLog(t *testing.T) {
	lines := make([]string, 15)
	for i := range lines {
		lines[i] = "hash" + string(rune('a'+i)) + " commit message"
	}
	commitLog := strings.Join(lines, "\n")

	msg := formatUpdateMessage(
		"abcdef1234567890",
		"1234567890abcdef",
		commitLog,
		"",
	)

	if !strings.Contains(msg, "... and 5 more") {
		t.Error("should truncate to 10 lines with '... and 5 more'")
	}
}

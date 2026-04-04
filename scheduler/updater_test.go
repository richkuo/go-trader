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
		true,
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
	if !strings.Contains(msg, "go build") {
		t.Error("should contain go build instruction when goChanged=true")
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
		false,
	)

	if !strings.Contains(msg, "v1.5.0") {
		t.Error("should contain release tag")
	}
	if !strings.Contains(msg, "New Release") {
		t.Error("should contain 'New Release' header for tagged releases")
	}
	if strings.Contains(msg, "go build") {
		t.Error("should not contain go build when goChanged=false")
	}
}

func TestFormatUpdateMessageNoCommitLog(t *testing.T) {
	msg := formatUpdateMessage(
		"abcdef1234567890",
		"1234567890abcdef",
		"",
		"",
		false,
	)

	if strings.Contains(msg, "```\n\n```") {
		t.Error("should not contain empty code block")
	}
}

func TestFormatUpdateMessageTruncatesLongLog(t *testing.T) {
	// Create a log with more than 10 lines
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
		true,
	)

	if !strings.Contains(msg, "... and 5 more") {
		t.Error("should truncate to 10 lines with '... and 5 more'")
	}
}

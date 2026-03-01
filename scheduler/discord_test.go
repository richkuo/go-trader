package main

import (
	"encoding/json"
	"testing"
)

func TestResolveChannel(t *testing.T) {
	channels := map[string]string{
		"spot":        "ch-spot",
		"hyperliquid": "ch-hl",
		"options":     "ch-opts",
	}

	// platform match takes priority
	if got := resolveChannel(channels, "hyperliquid", "perps"); got != "ch-hl" {
		t.Errorf("expected ch-hl, got %s", got)
	}
	// fall through to stratType
	if got := resolveChannel(channels, "binanceus", "spot"); got != "ch-spot" {
		t.Errorf("expected ch-spot, got %s", got)
	}
	// options type
	if got := resolveChannel(channels, "deribit", "options"); got != "ch-opts" {
		t.Errorf("expected ch-opts for deribit options, got %s", got)
	}
	// unknown → empty
	if got := resolveChannel(channels, "unknown", "unknown"); got != "" {
		t.Errorf("expected empty, got %s", got)
	}
}

func TestChannelKeyFromID(t *testing.T) {
	channels := map[string]string{
		"spot":        "111",
		"hyperliquid": "222",
	}
	if got := channelKeyFromID(channels, "111"); got != "spot" {
		t.Errorf("expected spot, got %s", got)
	}
	if got := channelKeyFromID(channels, "222"); got != "hyperliquid" {
		t.Errorf("expected hyperliquid, got %s", got)
	}
	// unknown channel ID falls back to itself
	if got := channelKeyFromID(channels, "999"); got != "999" {
		t.Errorf("expected 999, got %s", got)
	}
}

func TestIsOptionsType(t *testing.T) {
	spot := []StrategyConfig{{Type: "spot"}, {Type: "perps"}}
	opts := []StrategyConfig{{Type: "spot"}, {Type: "options"}}
	if isOptionsType(spot) {
		t.Error("expected false for spot/perps only")
	}
	if !isOptionsType(opts) {
		t.Error("expected true when options present")
	}
}

func TestDiscordChannels_BackwardsCompatJSON(t *testing.T) {
	// Old config format {"spot":"x","options":"y"} should still parse into map[string]string.
	raw := `{"enabled":true,"token":"","channels":{"spot":"ch1","options":"ch2"}}`
	var dc DiscordConfig
	if err := json.Unmarshal([]byte(raw), &dc); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if dc.Channels["spot"] != "ch1" {
		t.Errorf("expected ch1, got %s", dc.Channels["spot"])
	}
	if dc.Channels["options"] != "ch2" {
		t.Errorf("expected ch2, got %s", dc.Channels["options"])
	}
	// New key works too
	raw2 := `{"enabled":true,"token":"","channels":{"spot":"ch1","options":"ch2","hyperliquid":"ch3"}}`
	var dc2 DiscordConfig
	if err := json.Unmarshal([]byte(raw2), &dc2); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if dc2.Channels["hyperliquid"] != "ch3" {
		t.Errorf("expected ch3, got %s", dc2.Channels["hyperliquid"])
	}
}

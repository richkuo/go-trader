package main

import (
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

type fakeCommandLister struct {
	names []string
}

func (f fakeCommandLister) ApplicationCommands(appID string, guildID string, options ...discordgo.RequestOption) ([]*discordgo.ApplicationCommand, error) {
	out := make([]*discordgo.ApplicationCommand, 0, len(f.names))
	for _, n := range f.names {
		out = append(out, &discordgo.ApplicationCommand{Name: n})
	}
	return out, nil
}

func TestCheckCommandNamespaceOwnership(t *testing.T) {
	foreign := fakeCommandLister{names: []string{"help", "model"}}
	err := checkCommandNamespaceOwnership(foreign, "app", "")
	if err == nil {
		t.Fatal("expected refusal when another program owns commands on the same application")
	}
	if !strings.Contains(err.Error(), "another program") {
		t.Fatalf("unhelpful error text: %v", err)
	}

	ours := fakeCommandLister{names: []string{commandPrefix + "status", commandPrefix + "health"}}
	if err := checkCommandNamespaceOwnership(ours, "app", ""); err != nil {
		t.Fatalf("expected no refusal when only our own commands exist: %v", err)
	}
}

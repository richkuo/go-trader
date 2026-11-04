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

// The guild scope must be guarded too: a guild can host another program's
// commands, and a bulk overwrite would delete them.
func TestResolveSlashCommandScopeRefusesForeignGuildScope(t *testing.T) {
	foreign := fakeCommandLister{names: []string{"help"}}
	_, err := resolveSlashCommandScope("app", "1471669827545337969", foreign)
	if err == nil {
		t.Fatal("expected refusal when another program owns commands in the guild scope")
	}
	if !strings.Contains(err.Error(), "guild 1471669827545337969") {
		t.Fatalf("error should name the guild scope: %v", err)
	}
}

// With a guild configured, go-trader registers there and leaves the global
// list untouched, so OpenClaw's own commands survive.
func TestResolveSlashCommandScopeUsesConfiguredGuild(t *testing.T) {
	lister := fakeCommandLister{names: []string{commandPrefix + "status"}}
	scope, err := resolveSlashCommandScope("app", "1471669827545337969", lister)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if scope != "1471669827545337969" {
		t.Fatalf("expected the configured guild scope, got %q", scope)
	}
}

// Without a guild and without a way to check ownership, refuse rather than
// bulk overwrite a list that may belong to another program.
func TestResolveSlashCommandScopeRefusesUnverifiableGlobal(t *testing.T) {
	if _, err := resolveSlashCommandScope("app", "", nil); err == nil {
		t.Fatal("expected refusal when the global command list cannot be verified")
	}
}

// An empty guild scope is ours to take over.
func TestResolveSlashCommandScopeTakesEmptyGuild(t *testing.T) {
	scope, err := resolveSlashCommandScope("app", "1471669827545337969", fakeCommandLister{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if scope != "1471669827545337969" {
		t.Fatalf("expected the guild scope, got %q", scope)
	}
}

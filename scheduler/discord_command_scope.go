package main

import (
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"
)

// discordCommandLister is implemented by transports that can list the commands
// already registered for an application in a scope.
type discordCommandLister interface {
	ApplicationCommands(appID string, guildID string, options ...discordgo.RequestOption) ([]*discordgo.ApplicationCommand, error)
}

// checkCommandNamespaceOwnership refuses a scope that already holds commands owned
// by another program, because a bulk overwrite would delete those commands.
func checkCommandNamespaceOwnership(lister discordCommandLister, appID string, scope string) error {
	existing, err := lister.ApplicationCommands(appID, scope)
	if err != nil {
		return fmt.Errorf("list application commands: %w", err)
	}
	for _, c := range existing {
		if !strings.HasPrefix(c.Name, commandPrefix) {
			return fmt.Errorf("this Discord application already hosts %d command(s) owned by another program (for example %q); give go-trader its own Discord application and bot token, or set discord.guild_id so go-trader registers into a guild scope and leaves the global list untouched", len(existing), c.Name)
		}
	}
	return nil
}

// slashCommandScope returns the scope used to register slash commands.
//
// Discord keeps one global command list per application, and a global bulk
// overwrite replaces every command on the app, including commands another
// program registered with the same bot token. Registering into a guild keeps the
// global list intact, so two programs sharing one bot token cannot delete each
// other commands.
//
// A guild scope comes from discord.guild_id. Without one, the global scope is
// used, but only when no foreign program owns commands on the application.
func (d *DiscordNotifier) slashCommandScope(appID string) (string, error) {
	if d.cfg != nil {
		if guildID := strings.TrimSpace(d.cfg.Discord.GuildID); guildID != "" {
			return guildID, nil
		}
	}
	lister, ok := d.transport.(discordCommandLister)
	if !ok {
		return "", nil
	}
	if err := checkCommandNamespaceOwnership(lister, appID, ""); err != nil {
		return "", err
	}
	return "", nil
}

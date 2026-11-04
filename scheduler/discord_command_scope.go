package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"
)

// discordCommandLister is implemented by transports that can list the commands
// already registered for an application in a scope.
var errDiscordCommandScopeNotOwned = errors.New("discord command scope is not owned by go-trader")

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
			return fmt.Errorf("%w: the %s scope of this Discord application already hosts command(s) owned by another program (for example %q); give go-trader its own Discord application and bot token, or point discord.guild_id at a scope go-trader owns", errDiscordCommandScopeNotOwned, discordScopeLabel(scope), c.Name)
		}
	}
	return nil
}

// scopeLabel names a command scope for error messages.
func discordScopeLabel(scope string) string {
	if strings.TrimSpace(scope) == "" {
		return "global"
	}
	return "guild " + strings.TrimSpace(scope)
}

// resolveSlashCommandScope picks the scope go-trader registers into and proves
// that scope is go-trader's to overwrite.
//
// Discord keeps one global command list per application, and a global bulk
// overwrite replaces every command on the app, including commands another
// program registered with the same bot token. Registering into a guild keeps the
// global list intact, so two programs sharing one bot token cannot delete each
// other commands.
//
// A guild scope comes from discord.guild_id. Without one, the global scope is
// used, but only when the scope holds no command owned by another program.
func resolveSlashCommandScope(appID string, scope string, lister discordCommandLister) (string, error) {
	scope = strings.TrimSpace(scope)
	if lister == nil {
		if scope == "" {
			return "", fmt.Errorf("cannot verify which programs own the global Discord command list; set discord.guild_id so go-trader registers into a guild scope, or give go-trader its own Discord application and bot token")
		}
		return scope, nil
	}
	if err := checkCommandNamespaceOwnership(lister, appID, scope); err != nil {
		return "", err
	}
	return scope, nil
}

// slashCommandScope returns the scope used to register slash commands.
func (d *DiscordNotifier) slashCommandScope(appID string) (string, error) {
	scope := ""
	if d.cfg != nil {
		scope = d.cfg.Discord.GuildID
	}
	lister, _ := d.transport.(discordCommandLister)
	return resolveSlashCommandScope(appID, scope, lister)
}

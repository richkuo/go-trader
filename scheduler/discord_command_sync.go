package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/bwmarrin/discordgo"
)

type slashCommandSyncResult struct {
	Created   int
	Updated   int
	Deleted   int
	Unchanged int
}

func ownedSlashCommand(c *discordgo.ApplicationCommand) bool {
	if c == nil || !strings.HasPrefix(c.Name, commandPrefix) {
		return false
	}
	return c.Type == 0 || c.Type == discordgo.ChatApplicationCommand
}

func syncOwnedSlashCommands(t discordTransport, appID string, desired []*discordgo.ApplicationCommand) (slashCommandSyncResult, error) {
	var res slashCommandSyncResult
	want := make(map[string]bool, len(desired))
	for _, c := range desired {
		if !ownedSlashCommand(c) {
			return res, fmt.Errorf("slash command %q is not a %s chat command", c.Name, commandPrefix)
		}
		if want[c.Name] {
			return res, fmt.Errorf("duplicate slash command %q", c.Name)
		}
		want[c.Name] = true
	}
	existing, err := t.ApplicationCommands(appID, "")
	if err != nil {
		return res, fmt.Errorf("list global commands: %w", err)
	}
	have := make(map[string]*discordgo.ApplicationCommand)
	for _, c := range existing {
		if ownedSlashCommand(c) {
			have[c.Name] = c
		}
	}
	var errs []error
	for _, c := range desired {
		cur, ok := have[c.Name]
		switch {
		case !ok:
			if _, err := t.ApplicationCommandCreate(appID, "", c); err != nil {
				errs = append(errs, fmt.Errorf("create %s: %w", c.Name, err))
				continue
			}
			res.Created++
		case slashCommandMatches(cur, c):
			res.Unchanged++
		default:
			if _, err := t.ApplicationCommandEdit(appID, "", cur.ID, c); err != nil {
				errs = append(errs, fmt.Errorf("update %s: %w", c.Name, err))
				continue
			}
			res.Updated++
		}
	}
	stale := make([]string, 0)
	for name := range have {
		if !want[name] {
			stale = append(stale, name)
		}
	}
	sort.Strings(stale)
	for _, name := range stale {
		if err := t.ApplicationCommandDelete(appID, "", have[name].ID); err != nil {
			errs = append(errs, fmt.Errorf("delete %s: %w", name, err))
			continue
		}
		res.Deleted++
	}
	return res, errors.Join(errs...)
}

func deleteOwnedGuildSlashCommands(t discordTransport, appID, guildID string) (int, error) {
	existing, err := t.ApplicationCommands(appID, guildID)
	if err != nil {
		return 0, fmt.Errorf("list guild %s commands: %w", guildID, err)
	}
	var owned []*discordgo.ApplicationCommand
	for _, c := range existing {
		if ownedSlashCommand(c) {
			owned = append(owned, c)
		}
	}
	sort.Slice(owned, func(i, j int) bool { return owned[i].Name < owned[j].Name })
	deleted := 0
	var errs []error
	for _, c := range owned {
		if err := t.ApplicationCommandDelete(appID, guildID, c.ID); err != nil {
			errs = append(errs, fmt.Errorf("delete guild %s command %s: %w", guildID, c.Name, err))
			continue
		}
		deleted++
	}
	return deleted, errors.Join(errs...)
}

func slashCommandMatches(cur, want *discordgo.ApplicationCommand) bool {
	return reflect.DeepEqual(slashCommandShape(cur, want), slashCommandShape(want, want))
}

func slashCommandShape(c, ref *discordgo.ApplicationCommand) map[string]any {
	shape := map[string]any{
		"name":        c.Name,
		"description": c.Description,
		"options":     normalizedJSON(c.Options),
	}
	if ref.Contexts != nil {
		shape["contexts"] = normalizedJSON(c.Contexts)
	}
	if ref.IntegrationTypes != nil {
		shape["integration_types"] = normalizedJSON(c.IntegrationTypes)
	}
	if ref.DefaultMemberPermissions != nil {
		shape["default_member_permissions"] = normalizedJSON(c.DefaultMemberPermissions)
	}
	if ref.NSFW != nil {
		shape["nsfw"] = normalizedJSON(c.NSFW)
	}
	return shape
}

func normalizedJSON(v any) any {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("unencodable:%v", err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Sprintf("undecodable:%v", err)
	}
	return pruneEmptyJSON(out)
}

func pruneEmptyJSON(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			if p := pruneEmptyJSON(val); p != nil {
				out[k] = p
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	case []any:
		if len(x) == 0 {
			return nil
		}
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = pruneEmptyJSON(val)
		}
		return out
	case bool:
		if !x {
			return nil
		}
		return x
	case string:
		if x == "" {
			return nil
		}
		return x
	default:
		return v
	}
}

func (d *DiscordNotifier) syncSlashCommands(appID string) error {
	res, err := syncOwnedSlashCommands(d.transport, appID, slashCommands())
	fmt.Printf("Discord slash commands synced: %d created, %d updated, %d deleted, %d unchanged\n", res.Created, res.Updated, res.Deleted, res.Unchanged)
	if err != nil {
		return fmt.Errorf("sync slash commands: %w", err)
	}
	if d.cfg == nil {
		return nil
	}
	guildID := strings.TrimSpace(d.cfg.Discord.GuildID)
	if guildID == "" {
		return nil
	}
	n, gerr := deleteOwnedGuildSlashCommands(d.transport, appID, guildID)
	if gerr != nil {
		fmt.Printf("[WARN] Discord legacy guild command cleanup: %v\n", gerr)
		return nil
	}
	fmt.Printf("Discord legacy guild %s: removed %d go-trader command(s); discord.guild_id is used only for this cleanup and can be removed\n", guildID, n)
	return nil
}

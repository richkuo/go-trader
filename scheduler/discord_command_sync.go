package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

type slashCommandSyncResult struct {
	Created   int
	Updated   int
	Recreated int
	Deleted   int
	Unchanged int
}

func (r slashCommandSyncResult) writes() int {
	return r.Created + r.Updated + r.Recreated + r.Deleted
}

var (
	slashResyncInterval   = time.Minute
	slashResyncFlapWindow = 30 * time.Minute
)

const slashResyncFlapRepairs = 3

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
		if c.Contexts == nil || c.IntegrationTypes == nil || c.NSFW == nil {
			return res, fmt.Errorf("slash command %q must set contexts, integration types and nsfw so the sync can compare them", c.Name)
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
		case slashCommandNeedsRecreate(cur, c):
			if err := t.ApplicationCommandDelete(appID, "", cur.ID); err != nil {
				errs = append(errs, fmt.Errorf("recreate %s: delete: %w", c.Name, err))
				continue
			}
			if _, err := t.ApplicationCommandCreate(appID, "", c); err != nil {
				errs = append(errs, fmt.Errorf("recreate %s: create: %w", c.Name, err))
				continue
			}
			res.Recreated++
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
	return reflect.DeepEqual(slashCommandShape(cur), slashCommandShape(want))
}

func slashCommandShape(c *discordgo.ApplicationCommand) map[string]any {
	return map[string]any{
		"name":                       c.Name,
		"description":                c.Description,
		"options":                    normalizedJSON(c.Options),
		"contexts":                   normalizedJSON(c.Contexts),
		"integration_types":          normalizedJSON(c.IntegrationTypes),
		"nsfw":                       normalizedJSON(c.NSFW),
		"default_member_permissions": normalizedJSON(c.DefaultMemberPermissions),
		"name_localizations":         normalizedJSON(c.NameLocalizations),
		"description_localizations":  normalizedJSON(c.DescriptionLocalizations),
	}
}

func slashCommandNeedsRecreate(cur, want *discordgo.ApplicationCommand) bool {
	unclearable := []struct{ cur, want any }{
		{cur.DefaultMemberPermissions, want.DefaultMemberPermissions},
		{cur.NameLocalizations, want.NameLocalizations},
		{cur.DescriptionLocalizations, want.DescriptionLocalizations},
	}
	for _, f := range unclearable {
		if normalizedJSON(f.want) == nil && normalizedJSON(f.cur) != nil {
			return true
		}
	}
	return false
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

func (d *DiscordNotifier) syncOwnedSlashCommandsLocked(appID string) (slashCommandSyncResult, error) {
	d.syncMu.Lock()
	defer d.syncMu.Unlock()
	res, err := syncOwnedSlashCommands(d.transport, appID, slashCommands())
	if err != nil {
		err = fmt.Errorf("sync slash commands: %w%s", err, brokerProtocolHint(err))
	}
	return res, err
}

func brokerProtocolHint(err error) string {
	if err == nil || !strings.Contains(err.Error(), "unknown operation") {
		return ""
	}
	unit := "go-trader-notify@<deployment>.service"
	if dep := strings.TrimSpace(os.Getenv("GO_TRADER_EGRESS_DEPLOYMENT")); dep != "" {
		unit = "go-trader-notify@" + dep + ".service"
	}
	return fmt.Sprintf(" (the notification broker runs an older build than this scheduler: run `sudo systemctl restart %s`; go-trader retries every %s)", unit, slashResyncInterval)
}

func (d *DiscordNotifier) syncSlashCommands(appID string) error {
	res, err := d.syncOwnedSlashCommandsLocked(appID)
	fmt.Printf("Discord slash commands synced: %d created, %d updated, %d recreated, %d deleted, %d unchanged\n", res.Created, res.Updated, res.Recreated, res.Deleted, res.Unchanged)
	if err != nil {
		return err
	}
	if d.cfg == nil {
		return nil
	}
	guildID := strings.TrimSpace(d.cfg.Discord.GuildID)
	if guildID == "" {
		return nil
	}
	d.syncMu.Lock()
	n, gerr := deleteOwnedGuildSlashCommands(d.transport, appID, guildID)
	d.syncMu.Unlock()
	if gerr != nil {
		fmt.Printf("[WARN] Discord legacy guild command cleanup: %v\n", gerr)
		return nil
	}
	fmt.Printf("Discord legacy guild %s: removed %d go-trader command(s); discord.guild_id is used only for this cleanup and can be removed\n", guildID, n)
	return nil
}

func (d *DiscordNotifier) startSlashCommandResync(appID string, initialErr error) {
	d.resyncOnce.Do(func() {
		ctx, cancel := context.WithCancel(shutdownReadOnlyCtx)
		d.mu.Lock()
		d.resyncCancel = cancel
		d.mu.Unlock()
		lastErr := ""
		if initialErr != nil {
			lastErr = initialErr.Error()
		}
		go d.resyncSlashCommands(ctx, appID, slashResyncInterval, slashResyncFlapWindow, lastErr)
	})
}

func (d *DiscordNotifier) resyncSlashCommands(ctx context.Context, appID string, interval, flapWindow time.Duration, lastErr string) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var repairs []time.Time
	warned := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		res, err := d.syncOwnedSlashCommandsLocked(appID)
		if err != nil {
			if msg := err.Error(); msg != lastErr {
				lastErr = msg
				fmt.Printf("[WARN] Discord slash command re-sync failed: %v (retrying every %s)\n", err, interval)
			}
			continue
		}
		if lastErr != "" {
			lastErr = ""
			fmt.Printf("Discord slash command re-sync recovered: %d created, %d updated, %d recreated, %d deleted\n", res.Created, res.Updated, res.Recreated, res.Deleted)
			continue
		}
		now := time.Now()
		kept := repairs[:0]
		for _, at := range repairs {
			if now.Sub(at) < flapWindow {
				kept = append(kept, at)
			}
		}
		repairs = kept
		if len(repairs) == 0 {
			warned = false
		}
		if res.writes() == 0 {
			continue
		}
		repairs = append(repairs, now)
		fmt.Printf("[WARN] Discord slash commands changed outside go-trader and were repaired: %d created, %d updated, %d recreated, %d deleted\n", res.Created, res.Updated, res.Recreated, res.Deleted)
		if len(repairs) >= slashResyncFlapRepairs && !warned {
			warned = true
			msg := fmt.Sprintf("[slash] go-trader repaired its Discord slash commands %d times within %s. Another program or another go-trader instance that uses this bot token keeps changing them; give each program its own bot token, or run one go-trader version per token.", len(repairs), flapWindow)
			fmt.Println("[WARN] " + msg)
			if d.ownerID != "" {
				if err := d.SendDM(d.ownerID, msg); err != nil {
					fmt.Printf("[WARN] Discord slash command flap DM failed: %v\n", err)
				}
			}
		}
	}
}

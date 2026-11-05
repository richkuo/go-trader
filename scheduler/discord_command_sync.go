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
	slashResyncInterval     = time.Minute
	slashResyncMaxInterval  = 30 * time.Minute
	slashResyncFlapWindow   = 30 * time.Minute
	slashCreateBudget       = 100
	slashCreateBudgetWindow = 24 * time.Hour
)

var errSlashCreateBudget = errors.New("slash command create budget exhausted")

const slashResyncFlapRepairs = 3

var slashHeldStateCommandNames = map[string]bool{
	"clear-cash-reconcile":      true,
	operatorCloseCommandName:    true,
	"resolve-entry":             true,
	"resolve-rh-order":          true,
	robinhoodAccountCommandName: true,
}

func slashCreatePriority(c *discordgo.ApplicationCommand) int {
	name := strings.TrimPrefix(c.Name, commandPrefix)
	switch {
	case slashHeldStateCommandNames[name]:
		return 0
	case opsCommandNames[name]:
		return 1
	default:
		return 2
	}
}

func ownedSlashCommand(c *discordgo.ApplicationCommand) bool {
	if c == nil || !strings.HasPrefix(c.Name, commandPrefix) {
		return false
	}
	return c.Type == 0 || c.Type == discordgo.ChatApplicationCommand
}

func syncOwnedSlashCommands(t discordTransport, appID string, desired []*discordgo.ApplicationCommand, allowCreate func() bool, recordCreate func()) (slashCommandSyncResult, error) {
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
	ordered := append([]*discordgo.ApplicationCommand(nil), desired...)
	sort.SliceStable(ordered, func(i, j int) bool { return slashCreatePriority(ordered[i]) < slashCreatePriority(ordered[j]) })
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
	budgetHit := false
	mayCreate := func() bool {
		if allowCreate == nil || allowCreate() {
			return true
		}
		if !budgetHit {
			budgetHit = true
			errs = append(errs, errSlashCreateBudget)
		}
		return false
	}
	created := func() {
		if recordCreate != nil {
			recordCreate()
		}
	}
	for _, c := range ordered {
		cur, ok := have[c.Name]
		switch {
		case !ok:
			if !mayCreate() {
				continue
			}
			if _, err := t.ApplicationCommandCreate(appID, "", c); err != nil {
				errs = append(errs, fmt.Errorf("create %s: %w", c.Name, err))
				continue
			}
			created()
			res.Created++
		case slashCommandMatches(cur, c):
			res.Unchanged++
		case slashCommandNeedsRecreate(cur, c):
			if !mayCreate() {
				continue
			}
			if err := t.ApplicationCommandDelete(appID, "", cur.ID); err != nil {
				errs = append(errs, fmt.Errorf("recreate %s: delete: %w", c.Name, err))
				continue
			}
			if _, err := t.ApplicationCommandCreate(appID, "", c); err != nil {
				errs = append(errs, fmt.Errorf("recreate %s: create: %w", c.Name, err))
				continue
			}
			created()
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

func pruneTimes(times []time.Time, now time.Time, window time.Duration) []time.Time {
	kept := times[:0]
	for _, at := range times {
		if now.Sub(at) < window {
			kept = append(kept, at)
		}
	}
	return kept
}

func (d *DiscordNotifier) syncOwnedSlashCommandsLocked(appID string, enforceBudget bool) (slashCommandSyncResult, error) {
	d.syncMu.Lock()
	defer d.syncMu.Unlock()
	allowCreate := func() bool {
		now := time.Now()
		d.createTimes = pruneTimes(d.createTimes, now, slashCreateBudgetWindow)
		return !enforceBudget || len(d.createTimes) < slashCreateBudget
	}
	recordCreate := func() {
		d.createTimes = append(d.createTimes, time.Now())
	}
	res, err := syncOwnedSlashCommands(d.transport, appID, slashCommands(), allowCreate, recordCreate)
	if err != nil {
		err = fmt.Errorf("sync slash commands: %w%s", err, brokerProtocolHint(err))
	}
	return res, err
}

func slashRateLimited(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "http 429") || strings.Contains(msg, "rate limit")
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
	res, err := d.syncOwnedSlashCommandsLocked(appID, false)
	fmt.Printf("Discord slash commands synced: %d created, %d updated, %d recreated, %d deleted, %d unchanged\n", res.Created, res.Updated, res.Recreated, res.Deleted, res.Unchanged)
	if d.cfg != nil {
		if guildID := strings.TrimSpace(d.cfg.Discord.GuildID); guildID != "" {
			d.syncMu.Lock()
			d.legacyGuildID = guildID
			d.syncMu.Unlock()
		}
	}
	if err != nil {
		return err
	}
	d.cleanupLegacyGuildSlashCommands(appID)
	return nil
}

func (d *DiscordNotifier) cleanupLegacyGuildSlashCommands(appID string) {
	d.syncMu.Lock()
	defer d.syncMu.Unlock()
	guildID := d.legacyGuildID
	if guildID == "" || d.legacyGuildDone {
		return
	}
	n, err := deleteOwnedGuildSlashCommands(d.transport, appID, guildID)
	if err != nil {
		if msg := err.Error(); msg != d.legacyGuildErr {
			d.legacyGuildErr = msg
			fmt.Printf("[WARN] Discord legacy guild command cleanup: %v (the slash command re-sync retries it)\n", err)
		}
		return
	}
	d.legacyGuildDone = true
	fmt.Printf("Discord legacy guild %s: removed %d go-trader command(s); discord.guild_id is used only for this cleanup and can be removed\n", guildID, n)
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
		go d.resyncSlashCommands(ctx, appID, slashResyncInterval, slashResyncMaxInterval, slashResyncFlapWindow, lastErr)
	})
}

func (d *DiscordNotifier) slashResyncOwnerDM(msg string) {
	fmt.Println("[WARN] " + msg)
	if d.ownerID == "" {
		return
	}
	if err := d.SendDM(d.ownerID, msg); err != nil {
		fmt.Printf("[WARN] Discord slash command owner DM failed: %v\n", err)
	}
}

func (d *DiscordNotifier) resyncSlashCommands(ctx context.Context, appID string, base, maxWait, flapWindow time.Duration, lastErr string) {
	wait := base
	timer := time.NewTimer(wait)
	defer timer.Stop()
	var repairs []time.Time
	flapping, flapWarned, budgetWarned := false, false, false
	backOff := func() {
		wait = min(wait*2, maxWait)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		res, err := d.syncOwnedSlashCommandsLocked(appID, true)
		if err == nil {
			d.cleanupLegacyGuildSlashCommands(appID)
		}
		now := time.Now()
		repairs = pruneTimes(repairs, now, flapWindow)
		if len(repairs) == 0 && !flapping {
			flapWarned = false
		}
		budgetHit := errors.Is(err, errSlashCreateBudget)
		throttled := budgetHit || slashRateLimited(err)
		if err != nil {
			if msg := err.Error(); msg != lastErr {
				lastErr = msg
				fmt.Printf("[WARN] Discord slash command re-sync failed: %v\n", err)
			}
		} else if lastErr != "" {
			lastErr = ""
			fmt.Println("Discord slash command re-sync recovered")
		}
		if res.writes() > 0 {
			repairs = append(repairs, now)
			fmt.Printf("[WARN] Discord slash commands changed outside go-trader and were repaired: %d created, %d updated, %d recreated, %d deleted\n", res.Created, res.Updated, res.Recreated, res.Deleted)
		}
		if budgetHit && !budgetWarned {
			budgetWarned = true
			d.slashResyncOwnerDM(fmt.Sprintf("[slash] go-trader stopped re-creating its Discord slash commands: it reached its limit of %d creates within %s, because another program keeps deleting them. Missing go-trader commands stay missing until that window passes or go-trader restarts; give each program its own bot token.", slashCreateBudget, slashCreateBudgetWindow))
		}
		if len(repairs) >= slashResyncFlapRepairs {
			flapping = true
		}
		if len(repairs) >= slashResyncFlapRepairs && !flapWarned {
			flapWarned = true
			d.slashResyncOwnerDM(fmt.Sprintf("[slash] go-trader repaired its Discord slash commands %d times within %s. Another program or another go-trader instance that uses this bot token keeps changing them; go-trader now waits longer between checks (up to %s). Give each program its own bot token, or run one go-trader version per token.", len(repairs), flapWindow, maxWait))
		}
		switch {
		case throttled:
			backOff()
		case err == nil && res.writes() == 0:
			wait, flapping, budgetWarned = base, false, false
		case !flapping:
			wait = base
		case res.writes() > 0:
			backOff()
		}
		timer.Reset(wait)
	}
}

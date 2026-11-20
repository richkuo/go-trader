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

	"trading-scheduler/internal/discordcmd"
)

type slashCommandSyncResult struct {
	Created   int
	Updated   int
	Recreated int
	Deleted   int
	Unchanged int

	Listed             bool
	OtherChatCommands  int
	GlobalChatCommands int
	CapacityReached    bool
	CreatesSkipped     int
	BudgetReached      bool
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
	slashPassDeadline       = 2 * time.Minute
	slashCleanupDeadline    = 5 * time.Minute
	slashOpTimeout          = 20 * time.Second
)

const (
	slashGlobalChatCommandLimit = 100
	slashResyncFlapRepairs      = 3
)

var (
	errSlashCreateBudget    = errors.New("slash command create budget exhausted")
	errSlashCommandCapacity = errors.New("discord reached its limit of global chat-input commands for this application")
)

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

func chatSlashCommand(c *discordgo.ApplicationCommand) bool {
	return c != nil && (c.Type == 0 || c.Type == discordgo.ChatApplicationCommand)
}

func ownedSlashCommand(c *discordgo.ApplicationCommand) bool {
	return chatSlashCommand(c) && strings.HasPrefix(c.Name, commandPrefix)
}

func slashOpContext(ctx context.Context) (context.Context, context.CancelFunc) {
	limit := slashOpTimeout
	if dl, ok := ctx.Deadline(); ok {
		if rem := time.Until(dl); rem < limit {
			limit = rem
		}
	}
	return context.WithTimeout(ctx, limit)
}

func slashPassStop(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	return ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || discordcmd.IsRateLimited(err)
}

func validateDesiredSlashCommands(desired []*discordgo.ApplicationCommand) (map[string]bool, error) {
	want := make(map[string]bool, len(desired))
	for _, c := range desired {
		if !ownedSlashCommand(c) {
			return nil, fmt.Errorf("slash command %q is not a %s chat command", c.Name, commandPrefix)
		}
		if want[c.Name] {
			return nil, fmt.Errorf("duplicate slash command %q", c.Name)
		}
		if c.Contexts == nil || c.IntegrationTypes == nil || c.NSFW == nil {
			return nil, fmt.Errorf("slash command %q must set contexts, integration types and nsfw so the sync can compare them", c.Name)
		}
		want[c.Name] = true
	}
	return want, nil
}

func syncOwnedSlashCommands(ctx context.Context, api slashCommandAPI, appID string, desired []*discordgo.ApplicationCommand, allowCreate func() bool, recordCreate func()) (slashCommandSyncResult, error) {
	var res slashCommandSyncResult
	want, err := validateDesiredSlashCommands(desired)
	if err != nil {
		return res, err
	}
	ordered := append([]*discordgo.ApplicationCommand(nil), desired...)
	sort.SliceStable(ordered, func(i, j int) bool { return slashCreatePriority(ordered[i]) < slashCreatePriority(ordered[j]) })
	opCtx, cancel := slashOpContext(ctx)
	existing, err := api.list(opCtx, appID, "")
	cancel()
	if err != nil {
		return res, fmt.Errorf("list global commands: %w", err)
	}
	res.Listed = true
	have := make(map[string]*discordgo.ApplicationCommand)
	for _, c := range existing {
		if !chatSlashCommand(c) {
			continue
		}
		res.GlobalChatCommands++
		if ownedSlashCommand(c) {
			have[c.Name] = c
		} else {
			res.OtherChatCommands++
		}
	}
	var errs []error
	stop := func(err error) bool {
		errs = append(errs, err)
		return slashPassStop(ctx, err)
	}
	finish := func() (slashCommandSyncResult, error) {
		return res, errors.Join(errs...)
	}

	stale := make([]string, 0)
	for name := range have {
		if !want[name] {
			stale = append(stale, name)
		}
	}
	sort.Strings(stale)
	for _, name := range stale {
		opCtx, cancel := slashOpContext(ctx)
		err := api.remove(opCtx, appID, "", have[name].ID)
		cancel()
		if err != nil {
			if stop(fmt.Errorf("delete %s: %w", name, err)) {
				return finish()
			}
			continue
		}
		res.Deleted++
		res.GlobalChatCommands--
	}

	var creates []*discordgo.ApplicationCommand
	for _, c := range ordered {
		cur, ok := have[c.Name]
		switch {
		case !ok:
			creates = append(creates, c)
		case slashCommandMatches(cur, c):
			res.Unchanged++
		case slashCommandNeedsRecreate(cur, c):
			creates = append(creates, c)
		default:
			opCtx, cancel := slashOpContext(ctx)
			_, err := api.edit(opCtx, appID, cur.ID, c)
			cancel()
			if err != nil {
				if stop(fmt.Errorf("update %s: %w", c.Name, err)) {
					return finish()
				}
				continue
			}
			res.Updated++
		}
	}

	mayCreate := func() bool {
		if allowCreate == nil || allowCreate() {
			return true
		}
		if !res.BudgetReached {
			res.BudgetReached = true
			errs = append(errs, errSlashCreateBudget)
		}
		return false
	}
	capacity := func() {
		if !res.CapacityReached {
			res.CapacityReached = true
			errs = append(errs, errSlashCommandCapacity)
		}
	}
	for i, c := range creates {
		if res.CapacityReached {
			res.CreatesSkipped += len(creates) - i
			break
		}
		cur, recreate := have[c.Name]
		if !recreate && res.GlobalChatCommands >= slashGlobalChatCommandLimit {
			capacity()
			res.CreatesSkipped += len(creates) - i
			break
		}
		if !mayCreate() {
			res.CreatesSkipped++
			continue
		}
		if recreate {
			opCtx, cancel := slashOpContext(ctx)
			err := api.remove(opCtx, appID, "", cur.ID)
			cancel()
			if err != nil {
				if stop(fmt.Errorf("recreate %s: delete: %w", c.Name, err)) {
					return finish()
				}
				continue
			}
			res.GlobalChatCommands--
		}
		opCtx, cancel := slashOpContext(ctx)
		_, err := api.create(opCtx, appID, c)
		cancel()
		if err != nil {
			label := "create " + c.Name
			if recreate {
				label = "recreate " + c.Name + ": create"
			}
			if discordcmd.IsCapacity(err) {
				capacity()
				errs = append(errs, fmt.Errorf("%s: %w", label, err))
				res.CreatesSkipped += len(creates) - i - 1
				break
			}
			if stop(fmt.Errorf("%s: %w", label, err)) {
				return finish()
			}
			continue
		}
		if recordCreate != nil {
			recordCreate()
		}
		res.GlobalChatCommands++
		if recreate {
			res.Recreated++
		} else {
			res.Created++
		}
	}
	return finish()
}

func cleanupOwnedSlashCommandScope(ctx context.Context, api slashCommandAPI, appID, guildID string) (int, error) {
	scope := "global"
	if guildID != "" {
		scope = "guild " + guildID
	}
	opCtx, cancel := slashOpContext(ctx)
	existing, err := api.list(opCtx, appID, guildID)
	cancel()
	if err != nil {
		return 0, fmt.Errorf("list %s commands: %w", scope, err)
	}
	var owned []*discordgo.ApplicationCommand
	for _, c := range existing {
		if ownedSlashCommand(c) {
			owned = append(owned, c)
		}
	}
	sort.Slice(owned, func(i, j int) bool { return owned[i].Name < owned[j].Name })
	removed := 0
	var errs []error
	for _, c := range owned {
		opCtx, cancel := slashOpContext(ctx)
		err := api.remove(opCtx, appID, guildID, c.ID)
		cancel()
		if err != nil {
			errs = append(errs, fmt.Errorf("delete %s command %s: %w", scope, c.Name, err))
			if slashPassStop(ctx, err) {
				break
			}
			continue
		}
		removed++
	}
	return removed, errors.Join(errs...)
}

func cleanupOwnedSlashCommands(ctx context.Context, api slashCommandAPI, appID, guildID string) (int, error) {
	removed, err := cleanupOwnedSlashCommandScope(ctx, api, appID, "")
	if err != nil || guildID == "" {
		return removed, err
	}
	n, err := cleanupOwnedSlashCommandScope(ctx, api, appID, guildID)
	return removed + n, err
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

func brokerUpgradeHint() string {
	unit := "go-trader-notify@<deployment>.service"
	if dep := strings.TrimSpace(os.Getenv("GO_TRADER_EGRESS_DEPLOYMENT")); dep != "" {
		unit = "go-trader-notify@" + dep + ".service"
	}
	return fmt.Sprintf(" (update go-trader with scripts/update.sh, or run `sudo systemctl restart %s` after the update installed the new broker)", unit)
}

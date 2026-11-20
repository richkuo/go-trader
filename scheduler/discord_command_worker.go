package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"trading-scheduler/internal/discordcmd"
)

type slashCommandPolicy struct {
	Enabled       bool
	LegacyGuildID string
}

func slashCommandPolicyFor(cfg *Config) slashCommandPolicy {
	if cfg == nil {
		return slashCommandPolicy{}
	}
	return slashCommandPolicy{Enabled: cfg.Discord.SlashCommands, LegacyGuildID: strings.TrimSpace(cfg.Discord.GuildID)}
}

var slashCommandsOffHint atomic.Bool

func slashCommandOffNote() string {
	if !slashCommandsOffHint.Load() {
		return ""
	}
	return " Discord slash commands are off in this daemon: set discord.slash_commands to true and restart go-trader to use the command."
}

var (
	slashIdentityRetry   = 5 * time.Second
	slashNoticeQueueSize = 8
)

type slashCommandWorker struct {
	d       *DiscordNotifier
	api     slashCommandAPI
	policy  slashCommandPolicy
	ownerID string
	notices chan string
	done    chan struct{}

	notBefore      time.Time
	createTimes    []time.Time
	lastOtherChat  int
	listedOnce     bool
	capacityWarned bool
	failureWarned  bool
	lastLog        string
}

func (d *DiscordNotifier) startSlashCommandWorker(parent context.Context, policy slashCommandPolicy) <-chan struct{} {
	d.mu.Lock()
	if d.slashWorkerDone != nil {
		done := d.slashWorkerDone
		d.mu.Unlock()
		return done
	}
	ctx, cancel := context.WithCancel(parent)
	w := &slashCommandWorker{
		d:       d,
		api:     d.slashCommandAPI(),
		policy:  policy,
		ownerID: d.ownerID,
		notices: make(chan string, slashNoticeQueueSize),
		done:    make(chan struct{}),
	}
	d.slashWorkerCancel = cancel
	d.slashWorkerDone = w.done
	d.mu.Unlock()
	go w.sendNotices(ctx)
	go w.run(ctx)
	return w.done
}

func (w *slashCommandWorker) sendNotices(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-w.notices:
			if err := w.d.SendDM(w.ownerID, msg); err != nil {
				fmt.Printf("[WARN] Discord slash command owner DM failed: %v\n", err)
			}
		}
	}
}

func (w *slashCommandWorker) notify(msg string) {
	fmt.Println("[WARN] " + msg)
	if w.ownerID == "" {
		return
	}
	select {
	case w.notices <- msg:
	default:
		fmt.Println("[WARN] Discord slash command owner DM dropped: the notice queue is full")
	}
}

func (w *slashCommandWorker) logOnce(msg string) {
	if msg == w.lastLog {
		return
	}
	w.lastLog = msg
	fmt.Println(msg)
}

func sleepUntil(ctx context.Context, at time.Time) bool {
	wait := time.Until(at)
	if wait <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (w *slashCommandWorker) noteRateLimit(err error) bool {
	e, ok := discordcmd.AsError(err)
	if !ok || !e.RateLimited {
		return false
	}
	at := time.Now().Add(e.RetryAfter)
	if at.After(w.notBefore) {
		w.notBefore = at
	}
	return true
}

func slashDeferred(err error) bool {
	var deferred *slashCommandDeferred
	return errors.As(err, &deferred) || errors.Is(err, errSlashIdentityPending)
}

func (w *slashCommandWorker) prepare(ctx context.Context) (string, error) {
	if w.api == nil {
		return "", &slashCommandDeferred{reason: "no Discord command transport"}
	}
	if err := w.api.ready(); err != nil {
		return "", err
	}
	idCtx, cancel := slashOpContext(ctx)
	defer cancel()
	return w.api.identity(idCtx)
}

func (w *slashCommandWorker) run(ctx context.Context) {
	defer close(w.done)
	if w.policy.Enabled {
		w.runEnabled(ctx)
		return
	}
	w.runCleanup(ctx)
}

func (w *slashCommandWorker) cleanupScope() string {
	if w.policy.LegacyGuildID == "" {
		return "the global command list"
	}
	return "the global command list and legacy guild " + w.policy.LegacyGuildID
}

func (w *slashCommandWorker) runCleanup(ctx context.Context) {
	fmt.Printf("Discord slash commands are off (discord.slash_commands is false): no interaction handler and no command creates or edits; a background job removes go-trader chat commands from %s once\n", w.cleanupScope())
	deadline := time.Now().Add(slashCleanupDeadline)
	jobCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	removed := 0
	var lastErr error
	for {
		if !sleepUntil(jobCtx, w.notBefore) {
			break
		}
		appID, err := w.prepare(jobCtx)
		if err == nil {
			var n int
			n, err = cleanupOwnedSlashCommands(jobCtx, w.api, appID, w.policy.LegacyGuildID)
			removed += n
			if n > 0 {
				fmt.Printf("Discord slash command cleanup: removed %d go-trader command(s)\n", n)
			}
		}
		if err == nil {
			fmt.Printf("Discord slash command cleanup finished: removed %d go-trader command(s) from %s\n", removed, w.cleanupScope())
			if removed > 0 {
				w.notify(fmt.Sprintf("[slash] Discord slash commands are off (discord.slash_commands is false), so go-trader removed its %d slash command(s) from Discord. Held-state recovery commands such as /%sresolve-entry and /%s%s are not available. To use them, set discord.slash_commands to true and restart go-trader.", removed, commandPrefix, commandPrefix, operatorCloseCommandName))
			}
			return
		}
		lastErr = err
		if ctx.Err() != nil || jobCtx.Err() != nil {
			break
		}
		next := time.Now().Add(slashResyncInterval)
		switch {
		case w.noteRateLimit(err):
			next = w.notBefore
			w.logOnce(fmt.Sprintf("[WARN] Discord slash command cleanup is rate limited: %v", err))
		case slashDeferred(err):
			next = time.Now().Add(slashIdentityRetry)
			w.logOnce(fmt.Sprintf("[WARN] Discord slash command cleanup waits: %v", err))
		default:
			w.logOnce(fmt.Sprintf("[WARN] Discord slash command cleanup failed: %v", err))
		}
		if next.After(deadline) {
			if next.Equal(w.notBefore) {
				lastErr = fmt.Errorf("Discord's retry delay ends at %s, after the cleanup deadline: %w", w.notBefore.UTC().Format(time.RFC3339), lastErr)
			}
			break
		}
		if !sleepUntil(jobCtx, next) {
			break
		}
	}
	reason := "the cleanup deadline passed"
	if ctx.Err() != nil {
		reason = "the process is shutting down"
	}
	if lastErr != nil {
		reason += " (last error: " + lastErr.Error() + ")"
	}
	fmt.Printf("[WARN] Discord slash command cleanup unfinished: removed %d go-trader command(s) before %s; a later daemon start finishes it\n", removed, reason)
}

func (w *slashCommandWorker) allowCreate() bool {
	w.createTimes = pruneTimes(w.createTimes, time.Now(), slashCreateBudgetWindow)
	return len(w.createTimes) < slashCreateBudget
}

func (w *slashCommandWorker) recordCreate() {
	w.createTimes = append(w.createTimes, time.Now())
}

func (w *slashCommandWorker) syncPass(ctx context.Context, guildDone *bool) (slashCommandSyncResult, error) {
	passCtx, cancel := context.WithTimeout(ctx, slashPassDeadline)
	defer cancel()
	appID, err := w.prepare(passCtx)
	if err != nil {
		return slashCommandSyncResult{}, err
	}
	res, err := syncOwnedSlashCommands(passCtx, w.api, appID, slashCommands(), w.allowCreate, w.recordCreate)
	if res.Listed {
		w.listedOnce = true
		w.lastOtherChat = res.OtherChatCommands
	}
	if err == nil && !*guildDone {
		n, gerr := cleanupOwnedSlashCommandScope(passCtx, w.api, appID, w.policy.LegacyGuildID)
		if gerr != nil {
			return res, fmt.Errorf("legacy guild %s cleanup: %w", w.policy.LegacyGuildID, gerr)
		}
		*guildDone = true
		fmt.Printf("Discord legacy guild %s: removed %d go-trader command(s); discord.guild_id is used only for this cleanup and can be removed\n", w.policy.LegacyGuildID, n)
	}
	return res, err
}

func (w *slashCommandWorker) capacityNotice() string {
	other := "unknown (no command list has succeeded yet)"
	if w.listedOnce {
		other = fmt.Sprintf("%d", w.lastOtherChat)
	}
	return fmt.Sprintf("[slash] Discord refused to add a go-trader slash command: this bot application reached Discord's limit of %d global chat-input commands. Other programs that use this bot token hold %s of them in the last command list. go-trader adds no more commands until the list has room. Give go-trader its own bot application and token.", slashGlobalChatCommandLimit, other)
}

func (w *slashCommandWorker) runEnabled(ctx context.Context) {
	fmt.Println("Discord slash commands are on (discord.slash_commands is true): a background worker registers and maintains them; trading does not wait for it")
	base, maxWait, flapWindow := slashResyncInterval, slashResyncMaxInterval, slashResyncFlapWindow
	wait := time.Duration(0)
	var repairs []time.Time
	converged, flapping, flapWarned, budgetWarned := false, false, false, false
	guildDone := w.policy.LegacyGuildID == ""
	lastErr := ""
	backOff := func() {
		wait = min(max(wait*2, base), maxWait)
	}
	for {
		next := time.Now().Add(wait)
		if w.notBefore.After(next) {
			next = w.notBefore
		}
		if !sleepUntil(ctx, next) {
			return
		}
		convergedBefore := converged
		res, err := w.syncPass(ctx, &guildDone)
		if ctx.Err() != nil {
			return
		}
		now := time.Now()
		repairs = pruneTimes(repairs, now, flapWindow)
		if len(repairs) == 0 && !flapping {
			flapWarned = false
		}
		if slashDeferred(err) {
			if msg := err.Error(); msg != lastErr {
				lastErr = msg
				fmt.Printf("[WARN] Discord slash commands deferred: %v\n", err)
			}
			if errors.Is(err, errSlashIdentityPending) {
				wait = slashIdentityRetry
			} else {
				wait = base
			}
			continue
		}
		rateLimited := w.noteRateLimit(err)
		if err != nil {
			if msg := err.Error(); msg != lastErr {
				lastErr = msg
				fmt.Printf("[WARN] Discord slash command sync failed: %v\n", err)
			}
			if !convergedBefore && !rateLimited && !res.BudgetReached && !res.CapacityReached && !w.failureWarned {
				w.failureWarned = true
				w.notify("[slash] Discord slash command registration failed: " + err.Error() + ". go-trader keeps trading and retries in the background.")
			}
		} else if lastErr != "" {
			lastErr = ""
			fmt.Println("Discord slash command sync recovered")
		}
		if !convergedBefore && res.Listed {
			fmt.Printf("Discord slash commands synced: %d created, %d updated, %d recreated, %d deleted, %d unchanged\n", res.Created, res.Updated, res.Recreated, res.Deleted, res.Unchanged)
		}
		if err == nil {
			converged = true
		}
		if convergedBefore && res.writes() > 0 {
			repairs = append(repairs, now)
			fmt.Printf("[WARN] Discord slash commands changed outside go-trader and were repaired: %d created, %d updated, %d recreated, %d deleted\n", res.Created, res.Updated, res.Recreated, res.Deleted)
		}
		if res.CapacityReached {
			fmt.Printf("[WARN] Discord slash commands: the application holds %d global chat-input commands, Discord's limit is %d; %d go-trader command create(s) skipped\n", res.GlobalChatCommands, slashGlobalChatCommandLimit, res.CreatesSkipped)
			if !w.capacityWarned {
				w.capacityWarned = true
				w.notify(w.capacityNotice())
			}
		}
		if res.BudgetReached && !budgetWarned {
			budgetWarned = true
			w.notify(fmt.Sprintf("[slash] go-trader stopped creating its Discord slash commands: it reached its limit of %d creates within %s, because another program may keep deleting them. Missing go-trader commands stay missing until that window passes or go-trader restarts; give each program its own bot token.", slashCreateBudget, slashCreateBudgetWindow))
		}
		if len(repairs) >= slashResyncFlapRepairs {
			flapping = true
		}
		if len(repairs) >= slashResyncFlapRepairs && !flapWarned {
			flapWarned = true
			w.notify(fmt.Sprintf("[slash] go-trader repaired its Discord slash commands %d times within %s. Another program or another go-trader instance may use this bot token and keep changing them; go-trader now waits longer between checks (up to %s). Give each program its own bot token, or run one go-trader version per token.", len(repairs), flapWindow, maxWait))
		}
		switch {
		case rateLimited || res.BudgetReached || res.CapacityReached:
			backOff()
		case err != nil:
			wait = base
		case res.writes() == 0:
			wait, flapping, budgetWarned = base, false, false
		case !flapping:
			wait = base
		default:
			backOff()
		}
	}
}

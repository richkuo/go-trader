package main

import (
	"errors"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

type resyncDiscord struct {
	mu      sync.Mutex
	cmds    map[string]*discordgo.ApplicationCommand
	nextID  int
	creates int
	writes  int
	edits   int
	dms     []string
	lists   []time.Time
}

func (f *resyncDiscord) ChannelMessageSend(channelID string, content string, _ ...discordgo.RequestOption) (*discordgo.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dms = append(f.dms, content)
	return &discordgo.Message{ID: "1", ChannelID: channelID, Content: content}, nil
}

func (f *resyncDiscord) UserChannelCreate(string, ...discordgo.RequestOption) (*discordgo.Channel, error) {
	return &discordgo.Channel{ID: "5001"}, nil
}

func (f *resyncDiscord) InteractionRespond(*discordgo.Interaction, *discordgo.InteractionResponse, ...discordgo.RequestOption) error {
	return nil
}

func (f *resyncDiscord) FollowupMessageCreate(*discordgo.Interaction, bool, *discordgo.WebhookParams, ...discordgo.RequestOption) (*discordgo.Message, error) {
	return &discordgo.Message{}, nil
}

func (f *resyncDiscord) ApplicationCommands(string, string, ...discordgo.RequestOption) ([]*discordgo.ApplicationCommand, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists = append(f.lists, time.Now())
	out := make([]*discordgo.ApplicationCommand, 0, len(f.cmds))
	for _, c := range f.cmds {
		cp := *c
		out = append(out, &cp)
	}
	return out, nil
}

func (f *resyncDiscord) ApplicationCommandCreate(_ string, _ string, cmd *discordgo.ApplicationCommand, _ ...discordgo.RequestOption) (*discordgo.ApplicationCommand, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	cp := *cmd
	cp.ID = strconv.Itoa(700000 + f.nextID)
	cp.Type = discordgo.ChatApplicationCommand
	f.cmds[cp.ID] = &cp
	f.creates++
	f.writes++
	return &cp, nil
}

func (f *resyncDiscord) ApplicationCommandEdit(_ string, _ string, id string, cmd *discordgo.ApplicationCommand, _ ...discordgo.RequestOption) (*discordgo.ApplicationCommand, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *cmd
	cp.ID = id
	cp.Type = discordgo.ChatApplicationCommand
	f.cmds[id] = &cp
	f.writes++
	f.edits++
	return &cp, nil
}

func (f *resyncDiscord) ApplicationCommandDelete(_ string, _ string, id string, _ ...discordgo.RequestOption) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.cmds, id)
	f.writes++
	return nil
}

func (f *resyncDiscord) wipeOwned() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, c := range f.cmds {
		if strings.HasPrefix(c.Name, commandPrefix) {
			delete(f.cmds, id)
		}
	}
}

func (f *resyncDiscord) driftEdit(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.cmds {
		if c.Name == name {
			c.Description = "changed by another build"
		}
	}
}

func (f *resyncDiscord) listTimes() []time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Time(nil), f.lists...)
}

func (f *resyncDiscord) ownedNames() map[string]bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]bool{}
	for _, c := range f.cmds {
		if strings.HasPrefix(c.Name, commandPrefix) {
			out[strings.TrimPrefix(c.Name, commandPrefix)] = true
		}
	}
	return out
}

func (f *resyncDiscord) counts() (owned, creates, writes int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.cmds {
		if strings.HasPrefix(c.Name, commandPrefix) {
			owned++
		}
	}
	return owned, f.creates, f.writes
}

func (f *resyncDiscord) dmContaining(s string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, m := range f.dms {
		if strings.Contains(m, s) {
			n++
		}
	}
	return n
}

func TestSlashCommandResyncBoundsCreatesUnderPersistentDrift(t *testing.T) {
	prevBase, prevMax, prevFlap, prevBudget, prevWindow := slashResyncInterval, slashResyncMaxInterval, slashResyncFlapWindow, slashCreateBudget, slashCreateBudgetWindow
	slashResyncInterval, slashResyncMaxInterval, slashResyncFlapWindow = 20*time.Millisecond, 2*time.Second, 10*time.Second
	slashCreateBudget, slashCreateBudgetWindow = 60, 2*time.Second
	t.Cleanup(func() {
		slashResyncInterval, slashResyncMaxInterval, slashResyncFlapWindow = prevBase, prevMax, prevFlap
		slashCreateBudget, slashCreateBudgetWindow = prevBudget, prevWindow
	})

	f := &resyncDiscord{cmds: map[string]*discordgo.ApplicationCommand{
		"1": {ID: "1", Name: "help", Description: "foreign", Type: discordgo.ChatApplicationCommand},
	}}
	d := &DiscordNotifier{transport: f, ownerID: "424242"}
	t.Cleanup(d.Close)
	if err := d.syncSlashCommands("999"); err != nil {
		t.Fatalf("startup sync: %v", err)
	}
	total := len(slashCommands())
	_, startupCreates, _ := f.counts()
	if startupCreates != total {
		t.Fatalf("startup creates = %d, want %d", startupCreates, total)
	}
	d.startSlashCommandResync("999", nil)

	stormEnd := time.Now().Add(time.Second)
	for time.Now().Before(stormEnd) {
		f.wipeOwned()
		time.Sleep(5 * time.Millisecond)
	}
	_, afterStorm, _ := f.counts()
	loopCreates := afterStorm - startupCreates
	if limit := slashCreateBudget - startupCreates; loopCreates > limit {
		t.Fatalf("re-sync created %d commands during a 1s wipe storm, above the remaining budget %d", loopCreates, limit)
	}
	if f.dmContaining("stopped re-creating") != 1 {
		t.Fatalf("budget DMs = %d, want exactly 1", f.dmContaining("stopped re-creating"))
	}
	if f.dmContaining("repaired its Discord slash commands") > 1 {
		t.Fatal("more than one flap DM")
	}
	t.Logf("case (a): a peer wipes every go-trader command every 5ms for 1s: the re-sync made %d creates (budget %d per %s, %d used at startup); one budget DM", loopCreates, slashCreateBudget, slashCreateBudgetWindow, startupCreates)

	deadline := time.Now().Add(10 * time.Second)
	for {
		if owned, _, _ := f.counts(); owned == total {
			break
		}
		if time.Now().After(deadline) {
			owned, _, _ := f.counts()
			t.Fatalf("after the peer stopped, %d of %d go-trader commands are back", owned, total)
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(slashResyncMaxInterval + 200*time.Millisecond)
	_, _, settled := f.counts()
	time.Sleep(300 * time.Millisecond)
	if _, _, later := f.counts(); later != settled {
		t.Fatalf("drift-free re-sync ticks wrote %d command change(s)", later-settled)
	}
	t.Log("case (c): after the peer stops, the commands come back once the budget window passes, and the drift-free ticks after that write nothing")

	time.Sleep(slashCreateBudgetWindow)
	f.wipeOwned()
	wiped := time.Now()
	for {
		if owned, _, _ := f.counts(); owned == total {
			break
		}
		if time.Since(wiped) > slashResyncMaxInterval/2 {
			t.Fatalf("one overwrite after a quiet period was not repaired within %s; the interval did not return to %s", slashResyncMaxInterval/2, slashResyncInterval)
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Logf("case (b): after a quiet period, one overwrite was repaired in %s (base interval %s, backoff cap %s)", time.Since(wiped).Round(time.Millisecond), slashResyncInterval, slashResyncMaxInterval)
}

func TestSlashCommandResyncHoldsBackoffUnderEditDriftWhenWindowEqualsCap(t *testing.T) {
	if slashResyncFlapWindow != slashResyncMaxInterval || slashResyncMaxInterval != 30*slashResyncInterval {
		t.Fatalf("production values changed: base %s, cap %s, flap window %s; rescale this test", slashResyncInterval, slashResyncMaxInterval, slashResyncFlapWindow)
	}
	prevBase, prevMax, prevFlap := slashResyncInterval, slashResyncMaxInterval, slashResyncFlapWindow
	slashResyncInterval, slashResyncMaxInterval, slashResyncFlapWindow = 20*time.Millisecond, 600*time.Millisecond, 600*time.Millisecond
	t.Cleanup(func() {
		slashResyncInterval, slashResyncMaxInterval, slashResyncFlapWindow = prevBase, prevMax, prevFlap
	})

	f := &resyncDiscord{cmds: map[string]*discordgo.ApplicationCommand{}}
	d := &DiscordNotifier{transport: f, ownerID: "424242"}
	t.Cleanup(d.Close)
	if err := d.syncSlashCommands("999"); err != nil {
		t.Fatalf("startup sync: %v", err)
	}
	_, startupCreates, _ := f.counts()
	start := time.Now()
	d.startSlashCommandResync("999", nil)

	stop := make(chan struct{})
	peerDone := make(chan struct{})
	go func() {
		defer close(peerDone)
		for {
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
				f.driftEdit(commandPrefix + "status")
			}
		}
	}()
	time.Sleep(180 * slashResyncInterval)
	close(stop)
	<-peerDone
	stormEnd := time.Now()

	var ticks []time.Time
	for _, at := range f.listTimes() {
		if at.After(start) && at.Before(stormEnd) {
			ticks = append(ticks, at)
		}
	}
	if len(ticks) < 8 {
		t.Fatalf("only %d re-sync ticks in the drift run", len(ticks))
	}
	if gap := ticks[3].Sub(ticks[2]); gap < 3*slashResyncInterval/2 {
		t.Fatalf("after the third repair the next wait was %s, want the back-off to start above %s", gap, slashResyncInterval)
	}
	reachedCap := -1
	for i := 1; i < len(ticks); i++ {
		if ticks[i].Sub(ticks[i-1]) >= slashResyncMaxInterval*9/10 {
			reachedCap = i
			break
		}
	}
	if reachedCap < 0 {
		t.Fatalf("the wait never reached the %s cap; ticks %v", slashResyncMaxInterval, ticks)
	}
	capTicks := 0
	for i := reachedCap + 1; i < len(ticks); i++ {
		if gap := ticks[i].Sub(ticks[i-1]); gap < slashResyncMaxInterval*9/10 {
			t.Fatalf("under continuing drift the wait dropped from the %s cap to %s at tick %d", slashResyncMaxInterval, gap, i)
		}
		capTicks++
	}
	if capTicks < 2 {
		t.Fatalf("only %d tick(s) after the cap; the run did not cover a reset point", capTicks)
	}
	if n := f.dmContaining("repaired its Discord slash commands"); n != 1 {
		t.Fatalf("flap DMs during the drift run = %d, want exactly 1", n)
	}
	if _, creates, _ := f.counts(); creates != startupCreates {
		t.Fatalf("edit-only drift created %d command(s)", creates-startupCreates)
	}
	t.Logf("case (a): edit-only drift on every tick for 180 base intervals (3 h scaled; base %s, cap %s, flap window %s, equal as in production): %d ticks, wait held at the cap for the last %d, one flap DM", slashResyncInterval, slashResyncMaxInterval, slashResyncFlapWindow, len(ticks), capTicks)
	t.Logf("case (c): the first three repairs sent the flap DM and the next wait was %s, above the %s base", ticks[3].Sub(ticks[2]).Round(time.Millisecond), slashResyncInterval)

	deadline := time.Now().Add(4 * slashResyncMaxInterval)
	for {
		lists := f.listTimes()
		n := len(lists)
		if n >= 2 && lists[n-2].After(stormEnd) && lists[n-1].Sub(lists[n-2]) < slashResyncMaxInterval/4 {
			t.Logf("case (b): after the drift stopped, a drift-free tick returned the wait to %s (next gap %s)", slashResyncInterval, lists[n-1].Sub(lists[n-2]).Round(time.Millisecond))
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the wait did not return to the base after the drift stopped; ticks since %v: %v", stormEnd, lists)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if n := f.dmContaining("repaired its Discord slash commands"); n != 1 {
		t.Fatalf("flap DMs after recovery = %d, want exactly 1", n)
	}
}

func TestSlashCommandCreateBudgetCreatesHeldStateCommandsFirst(t *testing.T) {
	desired := map[string]bool{}
	for _, c := range slashCommands() {
		desired[strings.TrimPrefix(c.Name, commandPrefix)] = true
	}
	for name := range slashHeldStateCommandNames {
		if !desired[name] || !opsCommandNames[name] {
			t.Fatalf("held-state command %q is not a registered owner ops command", name)
		}
	}
	total := len(slashCommands())

	f := &resyncDiscord{cmds: map[string]*discordgo.ApplicationCommand{}}
	d := &DiscordNotifier{transport: f, ownerID: "424242"}
	t.Cleanup(d.Close)
	now := time.Now()
	for i := 0; i < slashCreateBudget-10; i++ {
		d.createTimes = append(d.createTimes, now)
	}
	_, err := d.syncOwnedSlashCommandsLocked("999", true)
	if !errors.Is(err, errSlashCreateBudget) {
		t.Fatalf("budget-limited sync error = %v, want the budget error", err)
	}
	got := f.ownedNames()
	if len(got) != 10 {
		t.Fatalf("budget-limited sync created %d commands, want 10", len(got))
	}
	for name := range slashHeldStateCommandNames {
		if !got[name] {
			t.Fatalf("held-state command %q is missing after a budget-limited sync: %v", name, got)
		}
	}
	for name := range got {
		if !opsCommandNames[name] {
			t.Fatalf("read-only command %q was created before an owner ops command: %v", name, got)
		}
	}
	names := make([]string, 0, len(got))
	for name := range got {
		names = append(names, name)
	}
	sort.Strings(names)
	t.Logf("case (a): budget room for 10 creates and %d commands missing: created %v; every held-state command exists, and no read-only command took a slot", total, names)

	f2 := &resyncDiscord{cmds: map[string]*discordgo.ApplicationCommand{}}
	d2 := &DiscordNotifier{transport: f2, ownerID: "424242"}
	t.Cleanup(d2.Close)
	if err := d2.syncSlashCommands("999"); err != nil {
		t.Fatalf("startup sync: %v", err)
	}
	prevBase := slashResyncInterval
	slashResyncInterval = 20 * time.Millisecond
	t.Cleanup(func() { slashResyncInterval = prevBase })
	d2.syncMu.Lock()
	d2.createTimes = d2.createTimes[:0]
	for i := 0; i < slashCreateBudget; i++ {
		d2.createTimes = append(d2.createTimes, time.Now())
	}
	d2.syncMu.Unlock()
	f2.wipeOwned()
	d2.startSlashCommandResync("999", nil)
	time.Sleep(10 * slashResyncInterval)
	if owned, creates, _ := f2.counts(); owned != 0 || creates != total {
		t.Fatalf("budget at zero: %d owned commands and %d creates after startup %d, want none created", owned, creates-total, total)
	}
	if n := f2.dmContaining("stopped re-creating"); n != 1 {
		t.Fatalf("budget DMs with the budget at zero = %d, want exactly 1", n)
	}
	t.Log("case (b): budget at zero and every command missing: the re-sync created nothing over 10 ticks and the owner got one DM")

	f3 := &resyncDiscord{cmds: map[string]*discordgo.ApplicationCommand{}}
	d3 := &DiscordNotifier{transport: f3, ownerID: "424242"}
	t.Cleanup(d3.Close)
	for i := 0; i < slashCreateBudget; i++ {
		d3.createTimes = append(d3.createTimes, time.Now())
	}
	if err := d3.syncSlashCommands("999"); err != nil {
		t.Fatalf("startup sync with the budget used up: %v", err)
	}
	if owned, _, _ := f3.counts(); owned != total {
		t.Fatalf("startup sync with the budget used up created %d of %d commands", owned, total)
	}
	t.Logf("case (c): startup sync with no budget limit created all %d commands, even with the re-sync budget used up", total)
}

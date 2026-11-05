package main

import (
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
	dms     []string
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

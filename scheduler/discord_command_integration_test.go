//go:build linux

package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"trading-scheduler/internal/egresspolicy"
	"trading-scheduler/internal/notifybroker"
)

const (
	slashTestOwner   = "424242"
	slashTestGuild   = "1471669827545337969"
	slashTestDMPath  = "discord.message:/api/v9/channels/5001/messages"
	slashTestChannel = "discord.message:/api/v9/channels/6001/messages"
)

type slashTestEnv struct {
	transport  string
	ca         *egressTestCA
	addr       string
	services   *fakeNotifyServices
	d          *DiscordNotifier
	notifier   *MultiNotifier
	brokerSess *notifyBrokerSession
	sock       string
}

func tuneSlashTimers(t *testing.T, resync time.Duration) {
	t.Helper()
	prev := []time.Duration{slashResyncInterval, slashResyncMaxInterval, slashResyncFlapWindow, slashPassDeadline, slashCleanupDeadline, slashOpTimeout, slashIdentityRetry}
	prevBudget := slashCreateBudget
	slashResyncInterval, slashResyncMaxInterval, slashResyncFlapWindow = resync, 4*resync, time.Minute
	slashPassDeadline, slashCleanupDeadline, slashOpTimeout, slashIdentityRetry = 30*time.Second, 30*time.Second, 20*time.Second, 100*time.Millisecond
	slashCreateBudget = 1000
	t.Cleanup(func() {
		slashResyncInterval, slashResyncMaxInterval, slashResyncFlapWindow = prev[0], prev[1], prev[2]
		slashPassDeadline, slashCleanupDeadline, slashOpTimeout, slashIdentityRetry = prev[3], prev[4], prev[5], prev[6]
		slashCreateBudget = prevBudget
	})
}

func newSlashDirectEnv(t *testing.T) *slashTestEnv {
	t.Helper()
	ca := newEgressTestCA(t)
	services, addr := startFakeNotifyServices(t, ca)
	sentinel := egressSentinel(t)
	proxy := startEgressTestProxy(t, "http", egressTestUser, sentinel, addr, nil)
	installTestEgress(t, proxy.url("localhost"), ca.pool, nil)
	session, err := discordgo.New("Bot fixture-direct-" + sentinel)
	if err != nil {
		t.Fatal(err)
	}
	pinDiscordSession(session)
	session.Client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: ca.pool, MinVersion: tls.VersionTLS12}
	session.State.Lock()
	session.State.User = &discordgo.User{ID: "999", Username: "fixture-bot", Bot: true}
	session.State.Unlock()
	d := &DiscordNotifier{session: session, transport: session, ownerID: slashTestOwner}
	notifier := NewMultiNotifier(notifierBackend{notifier: d, ownerID: slashTestOwner, channels: map[string]string{"spot": "6001"}})
	return &slashTestEnv{transport: "direct", ca: ca, addr: addr, services: services, d: d, notifier: notifier}
}

func startSlashTestBroker(t *testing.T, ca *egressTestCA, ep egresspolicy.Endpoint, sock string) func() {
	t.Helper()
	broker := notifybroker.NewBrokerFromEnv(os.Getuid(), sock, egresspolicy.NewDialer(ep), []string{"discord"})
	broker.TLSConfig = &tls.Config{RootCAs: ca.pool, MinVersion: tls.VersionTLS12}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- broker.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the broker socket never appeared")
		}
		time.Sleep(20 * time.Millisecond)
	}
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
	t.Cleanup(stop)
	return stop
}

func newSlashBrokerEnv(t *testing.T, startBroker bool) *slashTestEnv {
	t.Helper()
	ca := newEgressTestCA(t)
	services, addr := startFakeNotifyServices(t, ca)
	notifyProxy := startEgressTestProxy(t, "http", "", "", addr, nil)
	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	_, deadPort, _ := net.SplitHostPort(closed.Addr().String())
	closed.Close()
	sentinel := egressSentinel(t)
	installTestEgress(t, "http://"+egressTestUser+":"+sentinel+"@localhost:"+deadPort, ca.pool, nil)
	sockDir, err := os.MkdirTemp("/tmp", "egs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "broker.sock")
	t.Setenv("DISCORD_BOT_TOKEN", "fixture-discord-"+sentinel)
	t.Setenv("TELEGRAM_BOT_TOKEN", "")
	t.Setenv("GO_TRADER_GITHUB_TOKEN", "")
	env := &slashTestEnv{transport: "broker", ca: ca, addr: addr, services: services, sock: sock}
	if startBroker {
		ep, err := egresspolicy.ParseProxyURL(notifyProxy.url("localhost"))
		if err != nil {
			t.Fatal(err)
		}
		startSlashTestBroker(t, ca, ep, sock)
		select {
		case <-services.gwReady:
		case <-time.After(10 * time.Second):
			t.Fatal("the broker never opened the Discord gateway")
		}
	}
	processEgress.mu.Lock()
	processEgress.notificationsDirect = true
	processEgress.attestation = &egresspolicy.Attestation{Notify: &egresspolicy.NotifyAttestation{Socket: sock, BrokerUID: os.Getuid()}}
	processEgress.mu.Unlock()
	t.Cleanup(func() { setProcessNotifyBroker(nil) })
	if startBroker {
		env.connect(t)
	}
	return env
}

func (e *slashTestEnv) proxyEndpoint(t *testing.T) egresspolicy.Endpoint {
	t.Helper()
	p := startEgressTestProxy(t, "http", "", "", e.addr, nil)
	ep, err := egresspolicy.ParseProxyURL(p.url("localhost"))
	if err != nil {
		t.Fatal(err)
	}
	return ep
}

func (e *slashTestEnv) connect(t *testing.T) {
	t.Helper()
	cfg := &Config{Discord: DiscordConfig{Enabled: true, OwnerID: slashTestOwner, Channels: map[string]string{"spot": "6001"}}}
	notifier, cleanup := buildNotifierFromConfig(cfg)
	t.Cleanup(cleanup)
	d := notifier.DiscordBackend()
	if d == nil {
		t.Fatal("no Discord backend through the broker")
	}
	e.notifier, e.d, e.brokerSess = notifier, d, processNotifyBroker()
}

func slashEnvs(t *testing.T, fn func(t *testing.T, newEnv func(t *testing.T) *slashTestEnv)) {
	t.Run("direct", func(t *testing.T) {
		fn(t, newSlashDirectEnv)
	})
	t.Run("broker", func(t *testing.T) {
		fn(t, func(t *testing.T) *slashTestEnv { return newSlashBrokerEnv(t, true) })
	})
}

func (e *slashTestEnv) start(t *testing.T, policy slashCommandPolicy) (<-chan struct{}, context.CancelFunc, time.Duration) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if policy.Enabled {
		e.d.installSlashCommandHandler()
	}
	began := time.Now()
	done := e.d.startSlashCommandWorker(ctx, policy)
	took := time.Since(began)
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Errorf("the command worker did not stop within 10s of its cancel")
		}
	})
	return done, cancel, took
}

func slashWaitFor(t *testing.T, services *fakeNotifyServices, desc string, timeout time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: timed out; requests %v", desc, services.requestKeys())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func slashWaitDone(t *testing.T, done <-chan struct{}, within time.Duration, desc string) time.Duration {
	t.Helper()
	began := time.Now()
	select {
	case <-done:
	case <-time.After(within):
		t.Fatalf("%s: the command worker did not stop within %s", desc, within)
	}
	return time.Since(began)
}

func slashDesiredNames() []string {
	var names []string
	for _, c := range slashCommands() {
		names = append(names, c.Name)
	}
	sort.Strings(names)
	return names
}

func slashOwnedNames(services *fakeNotifyServices, scope string) []string {
	var out []string
	for _, n := range services.commandNames(scope) {
		if strings.HasPrefix(n, commandPrefix) {
			out = append(out, n)
		}
	}
	return out
}

func slashDMsContaining(services *fakeNotifyServices, needle string) int {
	n := 0
	for _, b := range services.bodiesFor(slashTestDMPath) {
		if strings.Contains(b, needle) {
			n++
		}
	}
	return n
}

func slashNextCommandRequest(keys []string, times []time.Time, after int) (string, time.Time, bool) {
	for i := after + 1; i < len(keys); i++ {
		k := keys[i]
		if strings.HasPrefix(k, "discord.command") || strings.HasPrefix(k, "discord.scripted:") || strings.HasPrefix(k, "discord.held:") {
			return k, times[i], true
		}
	}
	return "", time.Time{}, false
}

func TestSlashCommandWorkerOffRemovesOnlyOwnedChatCommands(t *testing.T) {
	slashEnvs(t, func(t *testing.T, newEnv func(t *testing.T) *slashTestEnv) {
		tuneSlashTimers(t, 100*time.Millisecond)
		env := newEnv(t)
		s := env.services
		s.seedCommand("", commandPrefix+"status", "Bot status")
		s.seedCommand("", commandPrefix+"retired-name", "an obsolete go-trader command")
		s.seedCommand("", "help", "OpenClaw help")
		s.seedCommandFields("", map[string]any{"name": commandPrefix + "user-menu", "type": 2})
		s.seedCommandFields("", map[string]any{"name": commandPrefix + "message-menu", "type": 3})
		s.seedCommand(slashTestGuild, commandPrefix+"health", "registered by the guild-scope build")
		s.seedCommand(slashTestGuild, "openclaw-guild", "OpenClaw guild command")

		release := s.holdCommandOp("list")
		defer release()
		done, _, took := env.start(t, slashCommandPolicy{Enabled: false, LegacyGuildID: slashTestGuild})
		if took > 100*time.Millisecond {
			t.Fatalf("starting the worker took %s", took)
		}
		slashWaitFor(t, s, "the cleanup list request is held", 10*time.Second, func() bool { return s.heldCount("list") == 1 })
		env.notifier.SendOwnerDM("owner DM while the command list is held")
		env.notifier.SendToChannel("binanceus", "spot", "trade alert while the command list is held")
		if slashDMsContaining(s, "owner DM while the command list is held") != 1 || len(s.bodiesFor(slashTestChannel)) != 1 {
			t.Fatalf("ordinary sends while the command list is held: requests %v", s.requestKeys())
		}
		t.Logf("case: %s, commands off: the worker started in %s; an owner DM and a channel trade alert were delivered while the cleanup list request was held", env.transport, took)
		release()
		slashWaitDone(t, done, 15*time.Second, "cleanup")

		wantGlobal := []string{commandPrefix + "message-menu", commandPrefix + "user-menu", "help"}
		if got := s.commandNames(""); !reflect.DeepEqual(got, wantGlobal) {
			t.Fatalf("global commands after cleanup = %v, want %v", got, wantGlobal)
		}
		if got := s.commandNames(slashTestGuild); !reflect.DeepEqual(got, []string{"openclaw-guild"}) {
			t.Fatalf("legacy guild commands after cleanup = %v, want only the foreign command", got)
		}
		if n := len(s.bodiesFor("discord.command_create")) + len(s.bodiesFor("discord.command_edit")) + len(s.bodiesFor("discord.commands_bulk")); n != 0 {
			t.Fatalf("commands off: %d create, edit or bulk request(s); want none", n)
		}
		if n := len(s.bodiesFor("discord.command_delete")); n != 3 {
			t.Fatalf("cleanup sent %d delete(s); want 3 (two global, one guild)", n)
		}
		slashWaitFor(t, s, "the cleanup owner DM", 5*time.Second, func() bool { return slashDMsContaining(s, "removed its 3 slash command(s)") == 1 })
		t.Logf("case: %s, commands off: removed go-trader-status, go-trader-retired-name and the guild go-trader-health; kept help, the user and message commands named go-trader-* and the foreign guild command; no create or edit; one owner DM", env.transport)

		lists := len(s.bodiesFor("discord.commands_list"))
		time.Sleep(5 * slashResyncInterval)
		if got := len(s.bodiesFor("discord.commands_list")); got != lists {
			t.Fatalf("commands off: %d more list request(s) after the cleanup job ended; disabled mode runs no periodic repair", got-lists)
		}
		t.Logf("case: %s, commands off: no list request in %s after the cleanup job; no periodic repair", env.transport, 5*slashResyncInterval)

		if env.transport != "broker" {
			return
		}
		env.brokerSess.discord.mu.Lock()
		installed := env.brokerSess.discord.onInteraction != nil
		env.brokerSess.discord.mu.Unlock()
		if installed {
			t.Fatal("commands off: an interaction handler is installed")
		}
		s.dispatch(t, "INTERACTION_CREATE", map[string]any{
			"id": "811", "application_id": "999", "type": 2, "token": "tok-811",
			"user": map[string]any{"id": slashTestOwner, "username": "owner"},
			"data": map[string]any{"id": "c811", "name": commandPrefix + "health", "type": 1},
		})
		answer := make(chan string, 1)
		go func() {
			resp, err := env.notifier.AskOwnerDM("confirm while commands are off?", 5*time.Second)
			if err != nil {
				answer <- "error: " + err.Error()
				return
			}
			answer <- resp
		}()
		time.Sleep(300 * time.Millisecond)
		s.dispatch(t, "MESSAGE_CREATE", map[string]any{"id": "9101", "channel_id": "5001", "content": "yes", "author": map[string]any{"id": slashTestOwner, "username": "owner"}})
		select {
		case got := <-answer:
			if got != "yes" {
				t.Fatalf("DM-reply prompt with commands off: %q", got)
			}
		case <-time.After(6 * time.Second):
			t.Fatal("DM-reply prompt with commands off: no answer")
		}
		if n := len(s.bodiesFor("discord.callback:/api/v9/interactions/811/tok-811/callback")); n != 0 {
			t.Fatalf("commands off: the interaction was answered %d time(s)", n)
		}
		t.Log("case: broker, commands off: no interaction handler, an interaction got no answer, and a DM-reply prompt still received the owner's answer")
	})
}

func TestSlashCommandWorkerOffCleanupRetriesInsideItsDeadline(t *testing.T) {
	slashEnvs(t, func(t *testing.T, newEnv func(t *testing.T) *slashTestEnv) {
		t.Run("PartialThenRetry", func(t *testing.T) {
			tuneSlashTimers(t, 50*time.Millisecond)
			env := newEnv(t)
			s := env.services
			for _, n := range []string{"alpha", "bravo", "charlie"} {
				s.seedCommand("", commandPrefix+n, "owned")
			}
			s.scriptCommandOp("delete", fakeCommandResponse{}, fakeRateLimit(0.4))
			done, _, _ := env.start(t, slashCommandPolicy{})
			slashWaitDone(t, done, 15*time.Second, "cleanup")
			if got := slashOwnedNames(s, ""); len(got) != 0 {
				t.Fatalf("owned commands after the retried cleanup: %v", got)
			}
			keys, times := s.requestLog()
			at := -1
			for i, k := range keys {
				if k == "discord.scripted:delete" {
					at = i
				}
			}
			if at < 0 || len(s.bodiesFor("discord.scripted:delete")) != 1 {
				t.Fatalf("scripted 429 not served once: %v", keys)
			}
			next, nextAt, ok := slashNextCommandRequest(keys, times, at)
			if !ok || next != "discord.commands_list" {
				t.Fatalf("after the 429 the next command request = %q; want a fresh list", next)
			}
			if gap := nextAt.Sub(times[at]); gap < 350*time.Millisecond {
				t.Fatalf("the cleanup retried %s after a 429 that asked for 400ms", gap)
			}
			if n := len(s.bodiesFor("discord.commands_list")); n != 2 {
				t.Fatalf("cleanup listed %d time(s); want 2 (first job pass, then the fresh list after the 429)", n)
			}
			t.Logf("case: %s: one delete succeeded, the next got a 429 asking for 400ms; the job waited %s, listed again and removed the rest", env.transport, nextAt.Sub(times[at]).Round(time.Millisecond))
		})
		t.Run("RetryDelayAfterTheDeadline", func(t *testing.T) {
			tuneSlashTimers(t, 50*time.Millisecond)
			slashCleanupDeadline = time.Second
			env := newEnv(t)
			s := env.services
			s.seedCommand("", commandPrefix+"alpha", "owned")
			s.scriptCommandOp("list", fakeRateLimit(30))
			done, _, _ := env.start(t, slashCommandPolicy{})
			took := slashWaitDone(t, done, 5*time.Second, "cleanup with a long retry delay")
			if n := s.commandRequests(); n != 1 {
				t.Fatalf("cleanup sent %d command request(s); want only the rate-limited list", n)
			}
			if got := slashOwnedNames(s, ""); len(got) != 1 {
				t.Fatalf("owned commands = %v; the unfinished cleanup must leave them for a later start", got)
			}
			t.Logf("case: %s: a 429 asking for 30s with a 1s cleanup deadline: the job stopped after %s with one request and left the command for a later daemon start", env.transport, took.Round(time.Millisecond))
		})
	})
}

func TestSlashCommandWorkerOnDoesNotHoldTrading(t *testing.T) {
	slashEnvs(t, func(t *testing.T, newEnv func(t *testing.T) *slashTestEnv) {
		t.Run("HeldCreateAndShutdown", func(t *testing.T) {
			tuneSlashTimers(t, 100*time.Millisecond)
			env := newEnv(t)
			s := env.services
			release := s.holdCommandOp("create")
			defer release()
			done, cancel, took := env.start(t, slashCommandPolicy{Enabled: true})
			slashWaitFor(t, s, "a held create", 10*time.Second, func() bool { return s.heldCount("create") == 1 })
			env.notifier.SendOwnerDM("owner DM while a command create is held")
			if slashDMsContaining(s, "owner DM while a command create is held") != 1 {
				t.Fatalf("owner DM while a create is held: %v", s.requestKeys())
			}
			cancel()
			stopped := slashWaitDone(t, done, 3*time.Second, "shutdown during a held create")
			slashWaitFor(t, s, "the held create is abandoned", 5*time.Second, func() bool { return len(s.bodiesFor("discord.abandoned:create")) == 1 })
			t.Logf("case: %s, commands on: the worker started in %s, an owner DM went out while a create was held, and shutdown stopped the worker in %s and dropped the held request", env.transport, took, stopped.Round(time.Millisecond))
		})
		t.Run("SharedCreateBudget", func(t *testing.T) {
			tuneSlashTimers(t, 50*time.Millisecond)
			slashCreateBudget = 5
			env := newEnv(t)
			s := env.services
			_, _, _ = env.start(t, slashCommandPolicy{Enabled: true})
			slashWaitFor(t, s, "the budget DM", 10*time.Second, func() bool { return slashDMsContaining(s, "limit of 5 creates") == 1 })
			slashWaitFor(t, s, "later passes", 10*time.Second, func() bool { return len(s.bodiesFor("discord.commands_list")) >= 4 })
			if n := len(s.bodiesFor("discord.command_create")); n != 5 {
				t.Fatalf("first sync and re-sync created %d command(s); want the budget of 5", n)
			}
			if slashDMsContaining(s, "limit of 5 creates") != 1 {
				t.Fatal("the budget DM was sent more than once")
			}
			t.Logf("case: %s: create budget 5: the first sync stopped at 5 creates, %d later passes created nothing more, one owner DM", env.transport, len(s.bodiesFor("discord.commands_list"))-1)
		})
	})
	t.Run("DirectLateIdentity", func(t *testing.T) {
		tuneSlashTimers(t, 100*time.Millisecond)
		env := newSlashDirectEnv(t)
		s := env.services
		st := env.d.session.State
		st.Lock()
		st.User = nil
		st.Unlock()
		_, _, _ = env.start(t, slashCommandPolicy{Enabled: true})
		time.Sleep(400 * time.Millisecond)
		if n := s.commandRequests(); n != 0 {
			t.Fatalf("%d command request(s) before the identity arrived", n)
		}
		st.Lock()
		st.User = &discordgo.User{ID: "999"}
		st.Unlock()
		slashWaitFor(t, s, "registration after a late identity", 10*time.Second, func() bool {
			return reflect.DeepEqual(slashOwnedNames(s, ""), slashDesiredNames())
		})
		t.Log("case: direct, commands on: no identity for 400ms: no command request; the worker registered every command once the identity arrived")
	})
}

func TestSlashCommandWorkerRateLimitEndsThePass(t *testing.T) {
	ops := []string{"list", "delete", "edit", "create", "get"}
	slashEnvs(t, func(t *testing.T, newEnv func(t *testing.T) *slashTestEnv) {
		for _, op := range ops {
			t.Run(op, func(t *testing.T) {
				env := newEnv(t)
				if op == "get" && env.transport != "broker" {
					t.Log("case: direct: the ownership read exists only in the broker")
					return
				}
				tuneSlashTimers(t, 50*time.Millisecond)
				s := env.services
				s.seedCommand("", commandPrefix+"stale-name", "an obsolete go-trader command")
				s.seedCommandFields("", map[string]any{"name": commandPrefix + "status", "description": "outdated", "contexts": []int{0, 1}, "integration_types": []int{0}})
				s.scriptCommandOp(op, fakeRateLimit(0.4))
				_, _, _ = env.start(t, slashCommandPolicy{Enabled: true})
				slashWaitFor(t, s, "convergence after the 429", 15*time.Second, func() bool {
					return reflect.DeepEqual(slashOwnedNames(s, ""), slashDesiredNames()) && s.commandDescription("", commandPrefix+"status") != "outdated"
				})
				keys, times := s.requestLog()
				at := -1
				for i, k := range keys {
					if k == "discord.scripted:"+op {
						at = i
					}
				}
				if at < 0 || len(s.bodiesFor("discord.scripted:"+op)) != 1 {
					t.Fatalf("scripted 429 on %s not served once: %v", op, keys)
				}
				next, nextAt, ok := slashNextCommandRequest(keys, times, at)
				if !ok || next != "discord.commands_list" {
					t.Fatalf("after the 429 on %s the next command request = %q; the pass must end and the next pass must list again", op, next)
				}
				gap := nextAt.Sub(times[at])
				if gap < 350*time.Millisecond {
					t.Fatalf("a 429 on %s asking for 400ms was followed by a request after %s", op, gap)
				}
				t.Logf("case: %s, 429 on %s: the pass ended, the next request was a fresh list after %s (retry delay 400ms, base interval 50ms), and the worker converged", env.transport, op, gap.Round(time.Millisecond))
			})
		}
		t.Run("AdmissionWaitHonoursShutdown", func(t *testing.T) {
			tuneSlashTimers(t, 50*time.Millisecond)
			env := newEnv(t)
			s := env.services
			s.scriptCommandOp("list", fakeCommandResponse{header: map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset-After": "10", "X-RateLimit-Bucket": "fixture"}})
			done, cancel, _ := env.start(t, slashCommandPolicy{Enabled: true})
			slashWaitFor(t, s, "the first sync", 15*time.Second, func() bool {
				return reflect.DeepEqual(slashOwnedNames(s, ""), slashDesiredNames())
			})
			time.Sleep(500 * time.Millisecond)
			if n := len(s.bodiesFor("discord.commands_list")); n != 1 {
				t.Fatalf("%d list request(s) while the list bucket is empty for 10s; want 1", n)
			}
			cancel()
			stopped := slashWaitDone(t, done, 3*time.Second, "shutdown during an admission wait")
			time.Sleep(200 * time.Millisecond)
			if n := len(s.bodiesFor("discord.commands_list")); n != 1 {
				t.Fatalf("a list request went out after shutdown: %d", n)
			}
			t.Logf("case: %s: the list bucket reported 0 remaining for 10s; the next pass waited without sending, and shutdown stopped it in %s", env.transport, stopped.Round(time.Millisecond))
		})
	})
}

func slashLastRequest(keys []string, key string) int {
	at := -1
	for i, k := range keys {
		if k == key {
			at = i
		}
	}
	return at
}

func TestSlashCommandWorkerRateLimitAfterAnotherError(t *testing.T) {
	slashEnvs(t, func(t *testing.T, newEnv func(t *testing.T) *slashTestEnv) {
		t.Run("EditErrorThenCreate429", func(t *testing.T) {
			tuneSlashTimers(t, 50*time.Millisecond)
			env := newEnv(t)
			s := env.services
			s.seedCommandFields("", map[string]any{"name": commandPrefix + "status", "description": "outdated", "contexts": []int{0, 1}, "integration_types": []int{0}})
			s.scriptCommandOp("edit", fakeCommandResponse{status: http.StatusBadRequest, body: `{"message":"Invalid Form Body","code":50035}`})
			s.scriptCommandOp("create", fakeRateLimit(2))
			_, _, _ = env.start(t, slashCommandPolicy{Enabled: true})
			slashWaitFor(t, s, "convergence after the 400 and the 429", 20*time.Second, func() bool {
				return reflect.DeepEqual(slashOwnedNames(s, ""), slashDesiredNames()) && s.commandDescription("", commandPrefix+"status") != "outdated"
			})
			keys, times := s.requestLog()
			if n := len(s.bodiesFor("discord.scripted:edit")) + len(s.bodiesFor("discord.scripted:create")); n != 2 {
				t.Fatalf("scripted 400 and 429 served %d time(s); want 2: %v", n, keys)
			}
			edit, at := slashLastRequest(keys, "discord.scripted:edit"), slashLastRequest(keys, "discord.scripted:create")
			if edit < 0 || at < edit {
				t.Fatalf("the 400 edit must come before the 429 create in one pass: %v", keys)
			}
			next, nextAt, ok := slashNextCommandRequest(keys, times, at)
			if !ok || next != "discord.commands_list" {
				t.Fatalf("after the 429 the next command request = %q; want a fresh list", next)
			}
			gap := nextAt.Sub(times[at])
			if gap < 1900*time.Millisecond {
				t.Fatalf("a 429 asking for 2s that followed a 400 in the same pass was followed by a request after %s", gap)
			}
			if n := slashDMsContaining(s, "registration failed"); n != 0 {
				t.Fatalf("%d registration-failed owner DM(s) for a rate-limited pass; want 0", n)
			}
			t.Logf("case: %s: an edit got a 400, then a create got a 429 asking for 2s (base interval 50ms); the next request was a fresh list after %s, no registration-failed DM went out, and the worker converged", env.transport, gap.Round(time.Millisecond))
		})

		t.Run("CleanupDelete403Then429", func(t *testing.T) {
			tuneSlashTimers(t, 50*time.Millisecond)
			env := newEnv(t)
			s := env.services
			for _, n := range []string{"alpha", "bravo", "charlie"} {
				s.seedCommand("", commandPrefix+n, "owned")
			}
			s.scriptCommandOp("delete", fakeCommandResponse{status: http.StatusForbidden, body: `{"message":"Missing Access","code":50001}`}, fakeRateLimit(1.5))
			done, _, _ := env.start(t, slashCommandPolicy{})
			slashWaitDone(t, done, 20*time.Second, "cleanup after a 403 and a 429")
			if got := slashOwnedNames(s, ""); len(got) != 0 {
				t.Fatalf("owned commands after the retried cleanup: %v", got)
			}
			keys, times := s.requestLog()
			at := slashLastRequest(keys, "discord.scripted:delete")
			if at < 0 || len(s.bodiesFor("discord.scripted:delete")) != 2 {
				t.Fatalf("scripted 403 and 429 not served once each: %v", keys)
			}
			next, nextAt, ok := slashNextCommandRequest(keys, times, at)
			if !ok || next != "discord.commands_list" {
				t.Fatalf("after the 429 the next command request = %q; want a fresh list", next)
			}
			gap := nextAt.Sub(times[at])
			if gap < 1400*time.Millisecond {
				t.Fatalf("the cleanup retried %s after a 429 asking for 1.5s that followed a 403", gap)
			}
			t.Logf("case: %s: one delete got a 403, the next got a 429 asking for 1.5s (base interval 50ms); the job waited %s, listed again and removed all three", env.transport, gap.Round(time.Millisecond))
		})

		t.Run("CleanupDelete403Then429AfterTheDeadline", func(t *testing.T) {
			tuneSlashTimers(t, 50*time.Millisecond)
			slashCleanupDeadline = time.Second
			env := newEnv(t)
			s := env.services
			for _, n := range []string{"alpha", "bravo", "charlie"} {
				s.seedCommand("", commandPrefix+n, "owned")
			}
			s.scriptCommandOp("delete", fakeCommandResponse{status: http.StatusForbidden, body: `{"message":"Missing Access","code":50001}`}, fakeRateLimit(30))
			done, _, _ := env.start(t, slashCommandPolicy{})
			took := slashWaitDone(t, done, 5*time.Second, "cleanup with a 403 then a long retry delay")
			keys, times := s.requestLog()
			at := slashLastRequest(keys, "discord.scripted:delete")
			if at < 0 || len(s.bodiesFor("discord.scripted:delete")) != 2 || len(s.bodiesFor("discord.commands_list")) != 1 {
				t.Fatalf("want one list, then a 403 delete and a 429 delete: %v", keys)
			}
			if next, _, ok := slashNextCommandRequest(keys, times, at); ok {
				t.Fatalf("cleanup sent %s after a 429 whose delay ends after the deadline: %v", next, keys)
			}
			if got := slashOwnedNames(s, ""); len(got) != 3 {
				t.Fatalf("owned commands = %v; the unfinished cleanup must leave them for a later start", got)
			}
			t.Logf("case: %s: a 403 then a 429 asking for 30s with a 1s cleanup deadline: the job stopped after %s with no request after the 429 and left the commands for a later daemon start", env.transport, took.Round(time.Millisecond))
		})
	})
}

func TestSlashCommandWorkerCapacityLimit(t *testing.T) {
	slashEnvs(t, func(t *testing.T, newEnv func(t *testing.T) *slashTestEnv) {
		t.Run("Error30032", func(t *testing.T) {
			tuneSlashTimers(t, 50*time.Millisecond)
			env := newEnv(t)
			s := env.services
			for _, n := range []string{"help", "model", "imagine"} {
				s.seedCommand("", n, "another program")
			}
			s.seedCommand("", commandPrefix+"old-name", "an obsolete go-trader command")
			s.scriptCommandOp("create", fakeCapacityError(), fakeCapacityError())
			_, _, _ = env.start(t, slashCommandPolicy{Enabled: true})
			slashWaitFor(t, s, "convergence after two capacity errors", 15*time.Second, func() bool {
				return reflect.DeepEqual(slashOwnedNames(s, ""), slashDesiredNames())
			})
			keys, times := s.requestLog()
			served := 0
			for i, k := range keys {
				if k != "discord.scripted:create" {
					continue
				}
				served++
				next, _, ok := slashNextCommandRequest(keys, times, i)
				if !ok || next != "discord.commands_list" {
					t.Fatalf("after error 30032 the next command request = %q; want no create or recreate until a fresh list", next)
				}
			}
			if served != 2 {
				t.Fatalf("scripted 30032 served %d time(s); want 2", served)
			}
			slashWaitFor(t, s, "the capacity DM", 5*time.Second, func() bool { return slashDMsContaining(s, "limit of 100 global chat-input commands") == 1 })
			if got := slashDMsContaining(s, "hold 3 of them"); got != 1 {
				t.Fatalf("capacity DM does not name the 3 other chat commands: %v", s.bodiesFor(slashTestDMPath))
			}
			if got := slashDMsContaining(s, "own bot application and token"); got != 1 {
				t.Fatal("capacity DM does not give the separate-token remedy")
			}
			if got := slashDMsContaining(s, "repaired its Discord slash commands"); got != 0 {
				t.Fatal("a removed old command name raised the shared-token warning")
			}
			t.Logf("case: %s: two passes got error 30032; each ended its creates at once and the next request was a fresh list; one capacity DM named the 100 limit, 3 other commands and the remedy; the deleted old command name raised no shared-token warning", env.transport)
		})
	})
	t.Run("DirectFullList", func(t *testing.T) {
		tuneSlashTimers(t, 50*time.Millisecond)
		env := newSlashDirectEnv(t)
		s := env.services
		for i := 0; i < slashGlobalChatCommandLimit; i++ {
			s.seedCommand("", "other-"+strings.Repeat("x", i%5)+string(rune('a'+i%26))+string(rune('a'+i/26)), "another program")
		}
		_, _, _ = env.start(t, slashCommandPolicy{Enabled: true})
		slashWaitFor(t, s, "two passes", 10*time.Second, func() bool { return len(s.bodiesFor("discord.commands_list")) >= 2 })
		if n := len(s.bodiesFor("discord.command_create")); n != 0 {
			t.Fatalf("%d create(s) against a full command list", n)
		}
		slashWaitFor(t, s, "the capacity DM", 5*time.Second, func() bool { return slashDMsContaining(s, "hold 100 of them") == 1 })
		t.Log("case: direct: the list already holds 100 chat commands of another program: no create request, one capacity DM")
	})
}

type oldNotifyBroker struct {
	mu     sync.Mutex
	ln     net.Listener
	conns  []net.Conn
	frames []string
	sends  []string
}

func startOldNotifyBroker(t *testing.T, sock string) *oldNotifyBroker {
	t.Helper()
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	b := &oldNotifyBroker{ln: ln}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			b.mu.Lock()
			b.conns = append(b.conns, conn)
			b.mu.Unlock()
			go b.serve(conn)
		}
	}()
	t.Cleanup(b.stop)
	return b
}

func (b *oldNotifyBroker) stop() {
	b.ln.Close()
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, c := range b.conns {
		c.Close()
	}
}

func (b *oldNotifyBroker) seen() ([]string, []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.frames...), append([]string(nil), b.sends...)
}

func (b *oldNotifyBroker) serve(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReaderSize(conn, 1<<20)
	reply := func(f *notifybroker.Frame) {
		data, _ := json.Marshal(f)
		_, _ = conn.Write(append(data, '\n'))
	}
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return
		}
		var f notifybroker.Frame
		if json.Unmarshal(line, &f) != nil {
			return
		}
		b.mu.Lock()
		b.frames = append(b.frames, f.Type+":"+f.Op)
		b.mu.Unlock()
		if f.Type != notifybroker.FrameRequest || f.ID == 0 {
			return
		}
		resp := &notifybroker.Frame{ID: f.ID, Type: notifybroker.FrameResponse, Op: f.Op}
		switch f.Op {
		case notifybroker.OpHello:
			resp.Payload = json.RawMessage(`{"discord_user_id":"999","discord":true,"telegram":false,"github":false}`)
		case notifybroker.OpDiscordUserChannel:
			resp.Payload = json.RawMessage(`{"id":"5001","type":1}`)
		case notifybroker.OpDiscordSend:
			var req notifybroker.DiscordSendRequest
			_ = json.Unmarshal(f.Payload, &req)
			b.mu.Lock()
			b.sends = append(b.sends, req.Content)
			b.mu.Unlock()
			resp.Payload = json.RawMessage(`{"id":"7001","channel_id":"5001","content":"ok"}`)
		default:
			resp.Error = "unknown operation \"" + f.Op + "\""
		}
		reply(resp)
	}
}

func TestSlashCommandWorkerWaitsForTheBrokerCommandPolicy(t *testing.T) {
	tuneSlashTimers(t, 200*time.Millisecond)
	env := newSlashBrokerEnv(t, false)
	old := startOldNotifyBroker(t, env.sock)
	env.connect(t)
	_, _, took := env.start(t, slashCommandPolicy{Enabled: true})
	time.Sleep(1500 * time.Millisecond)
	env.notifier.SendOwnerDM("owner DM through the old broker")
	frames, sends := old.seen()
	for _, f := range frames {
		if f != notifybroker.FrameRequest+":"+notifybroker.OpHello && f != notifybroker.FrameRequest+":"+notifybroker.OpDiscordUserChannel && f != notifybroker.FrameRequest+":"+notifybroker.OpDiscordSend {
			t.Fatalf("the old broker received %s; command maintenance must stay deferred and send no command or cancel frame (frames %v)", f, frames)
		}
	}
	if len(sends) != 1 || sends[0] != "owner DM through the old broker" {
		t.Fatalf("owner DM through the old broker: %v", sends)
	}
	t.Logf("case: an old broker without the command policy: the worker started in %s, sent no command or cancel frame in 1.5s, and the owner DM was delivered (frames %v)", took, frames)

	old.stop()
	os.Remove(env.sock)
	startSlashTestBroker(t, env.ca, env.proxyEndpoint(t), env.sock)
	slashWaitFor(t, env.services, "registration after the broker update", 45*time.Second, func() bool {
		return reflect.DeepEqual(slashOwnedNames(env.services, ""), slashDesiredNames())
	})
	t.Log("case: the old broker is replaced by the current broker on the same socket: the scheduler reconnected and the worker registered every command")
}

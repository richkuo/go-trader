//go:build linux

package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/gorilla/websocket"

	"trading-scheduler/internal/egresspolicy"
	"trading-scheduler/internal/notifybroker"
)

type fakeNotifyServices struct {
	mu       sync.Mutex
	requests []string
	bodies   map[string][]string
	gateway  *websocket.Conn
	gwReady  chan struct{}
	gwOnce   sync.Once
	gwOpens  int
	seq      int

	gatewayDown bool
	selfDown    bool
	messageDown bool

	commands  map[string]map[string]map[string]any
	nextCmdID int
}

func (f *fakeNotifyServices) seedCommand(scope, name, description string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.storeCommandLocked(scope, map[string]any{"name": name, "description": description})
}

func (f *fakeNotifyServices) seedCommandFields(scope string, cmd map[string]any) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.storeCommandLocked(scope, cmd)
}

func (f *fakeNotifyServices) commandField(scope, name, field string) any {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.commands[scope] {
		if c["name"] == name {
			raw, _ := json.Marshal(c[field])
			var out any
			_ = json.Unmarshal(raw, &out)
			return out
		}
	}
	return nil
}

func (f *fakeNotifyServices) wipeCommandsWithPrefix(scope, prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for id, c := range f.commands[scope] {
		if name, _ := c["name"].(string); strings.HasPrefix(name, prefix) {
			delete(f.commands[scope], id)
			n++
		}
	}
	return n
}

func (f *fakeNotifyServices) storeCommandLocked(scope string, cmd map[string]any) string {
	if f.commands == nil {
		f.commands = map[string]map[string]map[string]any{}
	}
	if f.commands[scope] == nil {
		f.commands[scope] = map[string]map[string]any{}
	}
	f.nextCmdID++
	id := strconv.Itoa(880000 + f.nextCmdID)
	cmd["id"] = id
	cmd["application_id"] = "999"
	cmd["version"] = id
	if _, ok := cmd["type"]; !ok {
		cmd["type"] = 1
	}
	if scope != "" {
		cmd["guild_id"] = scope
	}
	f.commands[scope][id] = cmd
	return id
}

func (f *fakeNotifyServices) commandNames(scope string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var names []string
	for _, c := range f.commands[scope] {
		names = append(names, c["name"].(string))
	}
	sort.Strings(names)
	return names
}

func (f *fakeNotifyServices) commandDescription(scope, name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.commands[scope] {
		if c["name"] == name {
			d, _ := c["description"].(string)
			return d
		}
	}
	return ""
}

func (f *fakeNotifyServices) commandWrites() int {
	return len(f.bodiesFor("discord.command_create")) + len(f.bodiesFor("discord.command_edit")) + len(f.bodiesFor("discord.command_delete")) + len(f.bodiesFor("discord.commands_bulk"))
}

func (f *fakeNotifyServices) serveCommands(w http.ResponseWriter, r *http.Request, path string, body []byte) {
	rest := path[strings.Index(path, "/applications/")+len("/applications/"):]
	parts := strings.Split(rest, "/")
	scope, at := "", 1
	if len(parts) > 3 && parts[1] == "guilds" {
		scope, at = parts[2], 3
	}
	if len(parts) <= at || parts[at] != "commands" || len(parts) > at+2 {
		f.record("unexpected:"+r.Host+path, string(body))
		w.WriteHeader(http.StatusNotFound)
		return
	}
	id := ""
	if len(parts) == at+2 {
		id = parts[at+1]
	}
	key, status, out := f.commandOp(r.Method, scope, id, body, r.URL.Query().Get("with_localizations") == "true")
	f.record(key, string(body))
	if out == nil {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(out)
}

func (f *fakeNotifyServices) commandOp(method, scope, id string, body []byte, withLocalizations bool) (string, int, []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	encode := func(v any) []byte {
		raw, _ := json.Marshal(v)
		return raw
	}
	switch {
	case method == http.MethodGet && id == "":
		list := make([]map[string]any, 0)
		for _, c := range f.commands[scope] {
			if withLocalizations {
				list = append(list, c)
				continue
			}
			shown := make(map[string]any, len(c))
			for k, v := range c {
				if k != "name_localizations" && k != "description_localizations" {
					shown[k] = v
				}
			}
			list = append(list, shown)
		}
		return "discord.commands_list", http.StatusOK, encode(list)
	case method == http.MethodPut && id == "":
		return "discord.commands_bulk", http.StatusOK, []byte("[]")
	case method == http.MethodPost && id == "":
		var cmd map[string]any
		if err := json.Unmarshal(body, &cmd); err != nil {
			return "discord.command_create_bad", http.StatusBadRequest, nil
		}
		newID := f.storeCommandLocked(scope, cmd)
		return "discord.command_create", http.StatusCreated, encode(f.commands[scope][newID])
	}
	cur, ok := f.commands[scope][id]
	if !ok {
		return "discord.command_missing:" + method, http.StatusNotFound, []byte(`{"message":"Unknown application command","code":10063}`)
	}
	switch method {
	case http.MethodGet:
		return "discord.command_get", http.StatusOK, encode(cur)
	case http.MethodPatch:
		var patch map[string]any
		if err := json.Unmarshal(body, &patch); err != nil {
			return "discord.command_edit_bad", http.StatusBadRequest, nil
		}
		for k, v := range patch {
			if k == "id" || k == "application_id" || k == "version" || k == "guild_id" {
				continue
			}
			cur[k] = v
		}
		return "discord.command_edit", http.StatusOK, encode(cur)
	case http.MethodDelete:
		delete(f.commands[scope], id)
		return "discord.command_delete", http.StatusNoContent, nil
	}
	return "discord.command_bad_method:" + method, http.StatusMethodNotAllowed, nil
}

func (f *fakeNotifyServices) setMessageDown(down bool) {
	f.mu.Lock()
	f.messageDown = down
	f.mu.Unlock()
}

func (f *fakeNotifyServices) setDown(gateway, self bool) {
	f.mu.Lock()
	f.gatewayDown, f.selfDown = gateway, self
	f.mu.Unlock()
}

func (f *fakeNotifyServices) down() (bool, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gatewayDown, f.selfDown
}

func (f *fakeNotifyServices) opens() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gwOpens
}

func (f *fakeNotifyServices) record(key, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, key)
	if f.bodies == nil {
		f.bodies = map[string][]string{}
	}
	f.bodies[key] = append(f.bodies[key], body)
}

func (f *fakeNotifyServices) bodiesFor(key string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.bodies[key]...)
}

func (f *fakeNotifyServices) dispatch(t *testing.T, eventType string, payload any) {
	t.Helper()
	f.mu.Lock()
	f.seq++
	seq := f.seq
	conn := f.gateway
	f.mu.Unlock()
	data, _ := json.Marshal(map[string]any{"op": 0, "t": eventType, "s": seq, "d": payload})
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatalf("gateway dispatch: %v", err)
	}
}

func startFakeNotifyServices(t *testing.T, ca *egressTestCA) (*fakeNotifyServices, string) {
	t.Helper()
	f := &fakeNotifyServices{gwReady: make(chan struct{})}
	upgrader := websocket.Upgrader{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		host := r.Host
		path := r.URL.Path
		switch {
		case host == "gateway.discord.gg":
			if gwDown, _ := f.down(); gwDown {
				f.record("discord.gateway_refused", "")
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			c, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			_ = c.WriteJSON(map[string]any{"op": 10, "d": map[string]any{"heartbeat_interval": 45000}})
			_, _, _ = c.ReadMessage()
			_ = c.WriteJSON(map[string]any{"op": 0, "t": "READY", "s": 1, "d": map[string]any{
				"v": 10, "session_id": "fixture-session", "user": map[string]any{"id": "999", "username": "fixture-bot", "bot": true}, "guilds": []any{},
			}})
			f.mu.Lock()
			f.gateway = c
			f.seq = 1
			f.gwOpens++
			f.mu.Unlock()
			f.gwOnce.Do(func() { close(f.gwReady) })
			go func() {
				for {
					if _, _, err := c.ReadMessage(); err != nil {
						return
					}
				}
			}()
			return
		case host == "discord.com" && strings.HasSuffix(path, "/gateway"):
			_, _ = io.WriteString(w, `{"url":"wss://gateway.discord.gg"}`)
		case host == "discord.com" && strings.HasSuffix(path, "/users/@me"):
			if _, selfDown := f.down(); selfDown {
				f.record("discord.self_refused", "")
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			f.record("discord.self", "")
			_, _ = io.WriteString(w, `{"id":"999","username":"fixture-bot","bot":true}`)
		case host == "discord.com" && strings.HasSuffix(path, "/users/@me/channels"):
			f.record("discord.dm_channel", string(body))
			_, _ = io.WriteString(w, `{"id":"5001","type":1}`)
		case host == "discord.com" && strings.Contains(path, "/channels/") && strings.HasSuffix(path, "/messages"):
			f.mu.Lock()
			messageDown := f.messageDown
			f.mu.Unlock()
			if messageDown {
				f.record("discord.message_refused:"+path, string(body))
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			f.record("discord.message:"+path, string(body))
			_, _ = io.WriteString(w, `{"id":"7001","channel_id":"5001","content":"ok"}`)
		case host == "discord.com" && strings.Contains(path, "/applications/"):
			f.serveCommands(w, r, path, body)
		case host == "discord.com" && strings.Contains(path, "/interactions/"):
			f.record("discord.callback:"+path, string(body))
			w.WriteHeader(http.StatusNoContent)
		case host == "api.telegram.org":
			method := path[strings.LastIndex(path, "/")+1:]
			f.record("telegram."+method, string(body))
			_, _ = io.WriteString(w, `{"ok":true,"result":{"id":1}}`)
		case host == "api.github.com" && strings.HasSuffix(path, "/issues"):
			f.record("github.issue:"+path, string(body))
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"html_url":"https://github.com/richkuo/go-trader/issues/9","number":9}`)
		default:
			f.record("unexpected:"+host+path, string(body))
			w.WriteHeader(http.StatusNotFound)
		}
	})
	srv := httptest.NewUnstartedServer(handler)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{ca.leaf(t, "discord.com", "gateway.discord.gg", "api.telegram.org", "api.github.com")}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return f, srv.Listener.Addr().String()
}

func TestEgressDirectNotificationsUseTheIsolatedBroker(t *testing.T) {
	ca := newEgressTestCA(t)
	services, addr := startFakeNotifyServices(t, ca)
	notifyProxy := startEgressTestProxy(t, "http", "", "", addr, nil)

	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	_, deadPort, _ := net.SplitHostPort(closed.Addr().String())
	closed.Close()
	sentinel := egressSentinel(t)
	installTestEgress(t, "http://"+egressTestUser+":"+sentinel+"@localhost:"+deadPort, ca.pool, nil)

	sockDir, err := os.MkdirTemp("/tmp", "egb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "broker.sock")

	t.Setenv("DISCORD_BOT_TOKEN", "fixture-discord-"+sentinel)
	t.Setenv("TELEGRAM_BOT_TOKEN", "fixture-telegram-"+sentinel)
	t.Setenv("GO_TRADER_GITHUB_TOKEN", "fixture-github-"+sentinel)
	t.Setenv(notifybroker.EnvGitHubRepos, "richkuo/go-trader")
	ep, err := egresspolicy.ParseProxyURL(notifyProxy.url("localhost"))
	if err != nil {
		t.Fatal(err)
	}
	broker := notifybroker.NewBrokerFromEnv(os.Getuid(), sock, egresspolicy.NewDialer(ep), []string{"discord", "github", "telegram"})
	broker.TLSConfig = &tls.Config{RootCAs: ca.pool, MinVersion: tls.VersionTLS12}
	ctx, cancel := context.WithCancel(context.Background())
	brokerDone := make(chan error, 1)
	go func() { brokerDone <- broker.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-brokerDone
	})
	select {
	case <-services.gwReady:
	case <-time.After(10 * time.Second):
		t.Fatal("the broker never opened the Discord gateway")
	}
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

	processEgress.mu.Lock()
	processEgress.notificationsDirect = true
	processEgress.attestation = &egresspolicy.Attestation{Notify: &egresspolicy.NotifyAttestation{Socket: sock, BrokerUID: os.Getuid()}}
	processEgress.mu.Unlock()
	t.Cleanup(func() { setProcessNotifyBroker(nil) })

	cfg := &Config{
		Discord:  DiscordConfig{Enabled: true, OwnerID: "424242", Channels: map[string]string{"spot": "6001"}},
		Telegram: TelegramConfig{Enabled: true, OwnerChatID: "777"},
	}
	notifier, cleanup := buildNotifierFromConfig(cfg)
	defer cleanup()
	if notifier.BackendCount() != 2 {
		t.Fatalf("broker backends = %d, want Discord and Telegram", notifier.BackendCount())
	}
	if len(services.bodiesFor("telegram.getMe")) != 1 {
		t.Fatal("telegram getMe did not go through the broker")
	}

	notifier.SendOwnerDM("egress outage drill")
	if got := services.bodiesFor("discord.message:/api/v9/channels/5001/messages"); len(got) != 1 || !strings.Contains(got[0], "egress outage drill") {
		t.Fatalf("owner DM through the broker: %v (requests %v)", got, services.requests)
	}
	if got := services.bodiesFor("telegram.sendMessage"); len(got) != 1 || !strings.Contains(got[0], `"777"`) {
		t.Fatalf("telegram owner message through the broker: %v", got)
	}
	notifier.SendToChannel("binanceus", "spot", "channel alert")
	if got := services.bodiesFor("discord.message:/api/v9/channels/6001/messages"); len(got) != 1 {
		t.Fatalf("channel message through the broker: %v", services.requests)
	}

	d := notifier.DiscordBackend()
	if d == nil {
		t.Fatal("no Discord backend")
	}
	const legacyGuild = "1471669827545337969"
	foreignGlobal := services.seedCommand("", "help", "OpenClaw help")
	services.seedCommand("", "model", "OpenClaw model picker")
	services.seedCommand("", commandPrefix+"resolve-robinhood-order", "old name")
	services.seedCommandFields("", map[string]any{"name": commandPrefix + "status", "description": "outdated description", "contexts": []int{1}})
	services.seedCommandFields("", map[string]any{"name": commandPrefix + "logs", "description": "Recent journalctl lines (owner DM only)", "contexts": []int{1}, "integration_types": []int{0}, "default_member_permissions": "8"})
	services.seedCommandFields("", map[string]any{"name": commandPrefix + "pnl", "description": "Portfolio P&L (total, per-platform, per-strategy)", "contexts": []int{0, 1}, "integration_types": []int{0}, "name_localizations": map[string]string{"de": "go-trader-gewinn"}, "description_localizations": map[string]string{"de": "Gewinn und Verlust"}})
	services.seedCommand(legacyGuild, commandPrefix+"health", "registered by the guild-scope build")
	services.seedCommand(legacyGuild, "openclaw-guild", "OpenClaw guild command")
	cfg.Discord.GuildID = legacyGuild
	prevResync, prevFlap, prevBudget := slashResyncInterval, slashResyncFlapWindow, slashCreateBudget
	slashResyncInterval, slashResyncFlapWindow, slashCreateBudget = 200*time.Millisecond, time.Minute, 1000
	t.Cleanup(func() {
		slashResyncInterval, slashResyncFlapWindow, slashCreateBudget = prevResync, prevFlap, prevBudget
	})
	if err := d.RegisterSlashCommands(nil, cfg); err != nil {
		t.Fatalf("register commands through the broker: %v", err)
	}
	if n := len(services.bodiesFor("discord.commands_bulk")); n != 0 {
		t.Fatalf("registration sent %d bulk overwrite(s); it must change commands one at a time", n)
	}
	var wantGlobal []string
	for _, c := range slashCommands() {
		wantGlobal = append(wantGlobal, c.Name)
	}
	wantGlobal = append(wantGlobal, "help", "model")
	sort.Strings(wantGlobal)
	if got := services.commandNames(""); !reflect.DeepEqual(got, wantGlobal) {
		t.Fatalf("global commands after sync = %v, want %v", got, wantGlobal)
	}
	if got := services.commandDescription("", commandPrefix+"status"); got == "outdated description" {
		t.Fatal("the outdated go-trader-status command was not updated")
	}
	if got := services.commandField("", commandPrefix+"status", "contexts"); !reflect.DeepEqual(got, []any{float64(0), float64(1)}) {
		t.Fatalf("go-trader-status contexts after sync = %v, want the read-only guild and bot-DM contexts", got)
	}
	if got := services.commandField("", commandPrefix+"logs", "default_member_permissions"); got != nil {
		t.Fatalf("go-trader-logs default_member_permissions after sync = %v, want it cleared by a re-create", got)
	}
	if got := services.commandField("", commandPrefix+"logs", "contexts"); !reflect.DeepEqual(got, []any{float64(1)}) {
		t.Fatalf("go-trader-logs contexts after sync = %v, want bot DM only", got)
	}
	if got := services.commandField("", commandPrefix+"pnl", "name_localizations"); got != nil {
		t.Fatalf("go-trader-pnl name_localizations after sync = %v, want them cleared by a re-create", got)
	}
	if got := services.commandField("", commandPrefix+"pnl", "description_localizations"); got != nil {
		t.Fatalf("go-trader-pnl description_localizations after sync = %v, want them cleared by a re-create", got)
	}
	if got := services.commandNames(legacyGuild); !reflect.DeepEqual(got, []string{"openclaw-guild"}) {
		t.Fatalf("legacy guild commands after cleanup = %v, want only the foreign command", got)
	}
	created, edited, deleted := len(services.bodiesFor("discord.command_create")), len(services.bodiesFor("discord.command_edit")), len(services.bodiesFor("discord.command_delete"))
	if created != len(slashCommands())-1 || edited != 1 || deleted != 4 {
		t.Fatalf("sync writes: created %d, edited %d, deleted %d; want %d, 1, 4", created, edited, deleted, len(slashCommands())-1)
	}
	t.Logf("case: shared bot token: foreign global and guild commands kept, stale go-trader commands removed (%d created, %d edited, %d deleted)", created, edited, deleted)
	t.Log("case: stored go-trader-status with contexts [1] and an outdated description: edited to contexts [0 1]")
	t.Log("case: stored go-trader-logs with default_member_permissions that a PATCH cannot clear: deleted and created again without it")
	t.Log("case: stored go-trader-pnl with name and description localizations, served by the list only on with_localizations=true: deleted and created again without them")

	writes := services.commandWrites()
	if err := d.RegisterSlashCommands(nil, cfg); err != nil {
		t.Fatalf("second registration through the broker: %v", err)
	}
	if got := services.commandWrites(); got != writes {
		t.Fatalf("a second registration with no change wrote %d command change(s)", got-writes)
	}
	t.Log("case: an unchanged command set: the second registration wrote nothing")

	brokerSess := processNotifyBroker()
	if brokerSess == nil || brokerSess.discord == nil {
		t.Fatal("no broker Discord transport")
	}
	if err := brokerSess.discord.ApplicationCommandDelete("999", "", foreignGlobal); err == nil || !strings.Contains(err.Error(), "outside the") {
		t.Fatalf("broker delete of a foreign command = %v, want a namespace refusal", err)
	}
	if _, err := brokerSess.discord.ApplicationCommandCreate("999", "", &discordgo.ApplicationCommand{Name: "help", Description: "x"}); err == nil || !strings.Contains(err.Error(), "outside the") {
		t.Fatalf("broker create of a foreign name = %v, want a namespace refusal", err)
	}
	if _, err := brokerSess.discord.ApplicationCommandEdit("999", "", foreignGlobal, &discordgo.ApplicationCommand{Name: commandPrefix + "help", Description: "x"}); err == nil || !strings.Contains(err.Error(), "outside the") {
		t.Fatalf("broker edit of a foreign command = %v, want a namespace refusal", err)
	}
	if got := services.commandNames(""); !reflect.DeepEqual(got, wantGlobal) {
		t.Fatalf("global commands after refused broker writes = %v, want %v", got, wantGlobal)
	}
	t.Log("case: the broker refuses to create, edit or delete a command outside the go-trader namespace")

	waitCommands := func(desc string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !reflect.DeepEqual(services.commandNames(""), wantGlobal) {
			if time.Now().After(deadline) {
				t.Fatalf("%s: global commands = %v, want %v", desc, services.commandNames(""), wantGlobal)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	for i := 1; i <= slashResyncFlapRepairs; i++ {
		if n := services.wipeCommandsWithPrefix("", commandPrefix); n != len(slashCommands()) {
			t.Fatalf("wipe %d removed %d go-trader commands, want %d", i, n, len(slashCommands()))
		}
		waitCommands(fmt.Sprintf("re-sync after the other program's overwrite %d", i))
	}
	t.Logf("case: another program overwrites the global list %d times: the periodic re-sync restores every go-trader command and keeps the foreign ones", slashResyncFlapRepairs)
	flapDM := func() bool {
		for _, b := range services.bodiesFor("discord.message:/api/v9/channels/5001/messages") {
			if strings.Contains(b, "repaired its Discord slash commands") {
				return true
			}
		}
		return false
	}
	flapDeadline := time.Now().Add(10 * time.Second)
	for !flapDM() {
		if time.Now().After(flapDeadline) {
			t.Fatalf("no owner DM after %d repairs within the flap window; requests %v", slashResyncFlapRepairs, services.requests)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Log("case: repeated repairs within the flap window: the owner gets one DM that names the shared bot token")

	interaction := func(id, user, name string) map[string]any {
		return map[string]any{
			"id": id, "application_id": "999", "type": 2, "token": "tok-" + id,
			"user": map[string]any{"id": user, "username": "u" + user},
			"data": map[string]any{"id": "c" + id, "name": name, "type": 1},
		}
	}
	waitCallback := func(id string) string {
		key := "discord.callback:/api/v9/interactions/" + id + "/tok-" + id + "/callback"
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if got := services.bodiesFor(key); len(got) > 0 {
				return got[0]
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("no interaction callback for %s; requests %v", id, services.requests)
		return ""
	}
	services.dispatch(t, "INTERACTION_CREATE", interaction("801", "424242", "go-trader-health"))
	if body := waitCallback("801"); !strings.Contains(body, "status server not wired") {
		t.Fatalf("owner read-only command response: %s", body)
	}
	services.dispatch(t, "INTERACTION_CREATE", interaction("802", "13", "go-trader-config"))
	if body := waitCallback("802"); !strings.Contains(body, "owner-only") {
		t.Fatalf("unauthorized ops command was not refused: %s", body)
	}

	answer := make(chan string, 1)
	go func() {
		resp, err := notifier.AskOwnerDM("confirm?", 5*time.Second)
		if err != nil {
			answer <- "error: " + err.Error()
			return
		}
		answer <- resp
	}()
	time.Sleep(300 * time.Millisecond)
	services.dispatch(t, "MESSAGE_CREATE", map[string]any{"id": "9001", "channel_id": "5001", "content": "yes", "author": map[string]any{"id": "424242", "username": "owner"}})
	select {
	case got := <-answer:
		if got != "yes" {
			t.Fatalf("prompt answer through the broker: %q", got)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("the prompt answer never arrived")
	}

	url, number, err := brokerCreateGitHubIssue(context.Background(), "richkuo/go-trader", githubIssueRequest{Title: "t", Body: "b"})
	if err != nil || number != 9 || !strings.Contains(url, "/issues/9") {
		t.Fatalf("issue report through the broker: %v %d %v", url, number, err)
	}
	if _, _, err := brokerCreateGitHubIssue(context.Background(), "someone/else", githubIssueRequest{Title: "t", Body: "b"}); err == nil || !strings.Contains(err.Error(), "not in") {
		t.Fatalf("issue report to an unlisted repository was not refused: %v", err)
	}

	sess := processNotifyBroker()
	refusals := []struct {
		op      string
		payload any
		want    string
	}{
		{"http.get", map[string]string{"url": "https://api.hyperliquid.xyz/info"}, "unknown operation"},
		{notifybroker.OpTelegramCall, map[string]any{"method": "setWebhook", "payload": map[string]string{"url": "https://evil.example"}}, "not allowed"},
		{notifybroker.OpTelegramCall, map[string]any{"method": "sendMessage", "payload": map[string]string{"chat_id": "777", "text": "x", "url": "y"}}, "not allowed"},
		{notifybroker.OpDiscordSend, map[string]string{"channel_id": "../../users", "content": "x"}, "channel id"},
		{notifybroker.OpDiscordSend, map[string]string{"channel_id": "6001", "content": "x", "url": "https://api.hyperliquid.xyz"}, "unknown field"},
		{"discord.register_commands", map[string]any{"commands": []any{}}, "unknown operation"},
		{notifybroker.OpDiscordCommandDelete, map[string]string{"id": "../../users"}, "command id"},
		{notifybroker.OpDiscordCommandDelete, map[string]string{"guild_id": "../x", "id": "1"}, "guild id"},
	}
	for _, r := range refusals {
		err := sess.client.Call(context.Background(), r.op, r.payload, nil)
		if err == nil || !strings.Contains(err.Error(), r.want) {
			t.Fatalf("broker op %s %v was not refused with %q: %v", r.op, r.payload, r.want, err)
		}
	}

	impostor := notifybroker.NewClient(sock, os.Getuid()+1, nil, nil)
	if err := impostor.Start(context.Background()); err == nil {
		t.Fatal("a client accepted a broker socket served by the wrong account")
	}

	for _, rec := range notifyProxy.records() {
		switch rec.Target {
		case "discord.com:443", "gateway.discord.gg:443", "api.telegram.org:443", "api.github.com:443":
		default:
			t.Fatalf("the broker reached a non-notification destination: %+v", rec)
		}
	}
	for _, key := range services.requests {
		if strings.HasPrefix(key, "unexpected:") {
			t.Fatalf("unexpected request %s", key)
		}
	}
	st := processEgress.status()
	if st.Tunnels != 0 {
		t.Fatalf("notification traffic used the venue proxy: %+v", st)
	}
}

func TestEgressBrokerSlashCommandsSurviveALateDiscordGateway(t *testing.T) {
	ca := newEgressTestCA(t)
	services, addr := startFakeNotifyServices(t, ca)
	services.setDown(true, true)
	notifyProxy := startEgressTestProxy(t, "http", "", "", addr, nil)

	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	_, deadPort, _ := net.SplitHostPort(closed.Addr().String())
	closed.Close()
	sentinel := egressSentinel(t)
	installTestEgress(t, "http://"+egressTestUser+":"+sentinel+"@localhost:"+deadPort, ca.pool, nil)

	sockDir, err := os.MkdirTemp("/tmp", "egl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "broker.sock")

	t.Setenv("DISCORD_BOT_TOKEN", "fixture-discord-"+sentinel)
	t.Setenv("TELEGRAM_BOT_TOKEN", "fixture-telegram-"+sentinel)
	t.Setenv("GO_TRADER_GITHUB_TOKEN", "")
	ep, err := egresspolicy.ParseProxyURL(notifyProxy.url("localhost"))
	if err != nil {
		t.Fatal(err)
	}
	startBroker := func() func() {
		broker := notifybroker.NewBrokerFromEnv(os.Getuid(), sock, egresspolicy.NewDialer(ep), []string{"discord", "telegram"})
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
		return func() {
			once.Do(func() {
				cancel()
				<-done
			})
		}
	}
	stopBroker := startBroker()
	t.Cleanup(func() { stopBroker() })

	processEgress.mu.Lock()
	processEgress.notificationsDirect = true
	processEgress.attestation = &egresspolicy.Attestation{Notify: &egresspolicy.NotifyAttestation{Socket: sock, BrokerUID: os.Getuid()}}
	processEgress.mu.Unlock()
	t.Cleanup(func() { setProcessNotifyBroker(nil) })
	prevWait, prevRetry := slashIdentityWait, slashRegisterFirstRetry
	slashIdentityWait, slashRegisterFirstRetry = 2*time.Second, 500*time.Millisecond
	t.Cleanup(func() { slashIdentityWait, slashRegisterFirstRetry = prevWait, prevRetry })

	cfg := &Config{
		Discord:  DiscordConfig{Enabled: true, OwnerID: "424242"},
		Telegram: TelegramConfig{Enabled: true, OwnerChatID: "777"},
	}
	notifier, cleanup := buildNotifierFromConfig(cfg)
	defer cleanup()
	d := notifier.DiscordBackend()
	if d == nil || notifier.BackendCount() != 2 {
		t.Fatalf("broker backends = %d, want Discord and Telegram", notifier.BackendCount())
	}

	err = d.RegisterSlashCommands(nil, cfg)
	if !errors.Is(err, errSlashCommandsDeferred) {
		t.Fatalf("registration with no Discord identity = %v, want the deferred registration", err)
	}
	t.Logf("case: the broker never connects to Discord: RegisterSlashCommands returned the one deferred warning: %v", err)
	if services.commandWrites() != 0 {
		t.Fatal("commands were registered without an application identity")
	}
	notifier.SendOwnerDM("gateway down drill")
	if got := services.bodiesFor("discord.message:/api/v9/channels/5001/messages"); len(got) != 1 || !strings.Contains(got[0], "gateway down drill") {
		t.Fatalf("owner DM while the gateway is down: %v (requests %v)", got, services.requests)
	}
	if got := services.bodiesFor("telegram.sendMessage"); len(got) != 1 || !strings.Contains(got[0], "gateway down drill") {
		t.Fatalf("telegram owner message while the gateway is down: %v", got)
	}
	if len(services.bodiesFor("discord.gateway_refused")) == 0 {
		t.Fatal("the broker never tried the gateway")
	}
	t.Log("case: the broker never connects to Discord: owner DM and Telegram message delivered while the gateway was refused")

	waitFor := func(desc string, timeout time.Duration, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(timeout)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatalf("%s: timed out; requests %v", desc, services.requests)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	interaction := func(id string) map[string]any {
		return map[string]any{
			"id": id, "application_id": "999", "type": 2, "token": "tok-" + id,
			"user": map[string]any{"id": "424242", "username": "owner"},
			"data": map[string]any{"id": "c" + id, "name": "go-trader-health", "type": 1},
		}
	}
	answered := func(id string) func() bool {
		key := "discord.callback:/api/v9/interactions/" + id + "/tok-" + id + "/callback"
		return func() bool {
			got := services.bodiesFor(key)
			return len(got) > 0 && strings.Contains(got[0], "status server not wired")
		}
	}

	services.setDown(false, false)
	select {
	case <-services.gwReady:
	case <-time.After(30 * time.Second):
		t.Fatalf("the broker never opened the Discord gateway after it came back; requests %v", services.requests)
	}
	waitFor("background slash command registration", 30*time.Second, func() bool { return len(services.commandNames("")) == len(slashCommands()) })
	services.dispatch(t, "INTERACTION_CREATE", interaction("901"))
	waitFor("the owner command after a late gateway", 10*time.Second, answered("901"))
	t.Log("case: the broker opens Discord after the registration wait ran out: commands registered in the background and /health answered")

	stopBroker()
	stopBroker = startBroker()
	waitFor("the restarted broker opens the gateway", 30*time.Second, func() bool { return services.opens() >= 2 })
	waitFor("the scheduler reconnects to the restarted broker", 30*time.Second, func() bool {
		sess := processNotifyBroker()
		return sess != nil && sess.client.Connected()
	})
	time.Sleep(500 * time.Millisecond)
	services.dispatch(t, "INTERACTION_CREATE", interaction("902"))
	waitFor("the owner command after a broker restart", 10*time.Second, answered("902"))
	t.Log("case: the broker restarts after a successful registration: /health answered again after it reconnected")

	for _, key := range services.requests {
		if strings.HasPrefix(key, "unexpected:") {
			t.Fatalf("unexpected request %s", key)
		}
	}
}

func TestEgressRefusalDMIsRecordedOnlyWhenDelivered(t *testing.T) {
	ca := newEgressTestCA(t)
	services, addr := startFakeNotifyServices(t, ca)
	notifyProxy := startEgressTestProxy(t, "http", "", "", addr, nil)

	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	_, deadPort, _ := net.SplitHostPort(closed.Addr().String())
	closed.Close()
	sentinel := egressSentinel(t)
	installTestEgress(t, "http://"+egressTestUser+":"+sentinel+"@localhost:"+deadPort, ca.pool, nil)

	sockDir, err := os.MkdirTemp("/tmp", "egr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "broker.sock")

	t.Setenv("DISCORD_BOT_TOKEN", "fixture-discord-"+sentinel)
	t.Setenv("TELEGRAM_BOT_TOKEN", "")
	t.Setenv("GO_TRADER_GITHUB_TOKEN", "")
	ep, err := egresspolicy.ParseProxyURL(notifyProxy.url("localhost"))
	if err != nil {
		t.Fatal(err)
	}

	processEgress.mu.Lock()
	processEgress.notificationsDirect = true
	processEgress.attestation = &egresspolicy.Attestation{Notify: &egresspolicy.NotifyAttestation{Socket: sock, BrokerUID: os.Getuid()}}
	processEgress.mu.Unlock()
	t.Cleanup(func() { setProcessNotifyBroker(nil) })
	prevWait := brokerStartupWait
	brokerStartupWait = time.Second
	t.Cleanup(func() { brokerStartupWait = prevWait })

	cfg := &Config{Discord: DiscordConfig{Enabled: true, OwnerID: "424242"}}
	dmPath := filepath.Join(t.TempDir(), egressRefusalDMFile)
	const reason = "egress proxy unreachable"
	interval := DefaultAlertThrottleInterval
	dmKey := "discord.message:/api/v9/channels/5001/messages"
	refusedKey := "discord.message_refused:/api/v9/channels/5001/messages"
	recorded := func() bool {
		_, err := os.Stat(dmPath)
		return err == nil
	}

	if notifyEgressRefusal(cfg, dmPath, reason, "drill one", interval, time.Now().UTC()) {
		t.Fatal("refusal DM reported delivered while the broker socket does not answer")
	}
	if recorded() {
		t.Fatal("refusal DM recorded while the broker socket does not answer")
	}
	if len(services.bodiesFor(dmKey)) != 0 {
		t.Fatalf("a DM reached Discord without a broker: %v", services.requests)
	}
	t.Log("case: broker socket does not answer: no DM delivered, no record written")

	broker := notifybroker.NewBrokerFromEnv(os.Getuid(), sock, egresspolicy.NewDialer(ep), []string{"discord"})
	broker.TLSConfig = &tls.Config{RootCAs: ca.pool, MinVersion: tls.VersionTLS12}
	ctx, cancel := context.WithCancel(context.Background())
	brokerDone := make(chan error, 1)
	go func() { brokerDone <- broker.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-brokerDone
	})
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

	services.setMessageDown(true)
	if notifyEgressRefusal(cfg, dmPath, reason, "drill two", interval, time.Now().UTC()) {
		t.Fatal("refusal DM reported delivered while Discord returns 503")
	}
	if recorded() {
		t.Fatal("refusal DM recorded while Discord returns 503 and Telegram is off")
	}
	if len(services.bodiesFor(refusedKey)) == 0 {
		t.Fatalf("the broker never tried the DM: %v", services.requests)
	}
	t.Log("case: Discord REST returns 503 and Telegram is off: no record written")

	services.setMessageDown(false)
	if !notifyEgressRefusal(cfg, dmPath, reason, "drill three", interval, time.Now().UTC()) {
		t.Fatalf("refusal DM not delivered after the broker and Discord came up: %v", services.requests)
	}
	if got := services.bodiesFor(dmKey); len(got) != 1 || !strings.Contains(got[0], "drill three") {
		t.Fatalf("delivered DM: %v", got)
	}
	if !recorded() {
		t.Fatal("delivered refusal DM was not recorded")
	}
	t.Log("case: the next restart after the broker comes up sends the DM and writes the record")

	if notifyEgressRefusal(cfg, dmPath, reason, "drill four", interval, time.Now().UTC()) {
		t.Fatal("a second refusal with the same reason was sent inside the throttle interval")
	}
	if got := services.bodiesFor(dmKey); len(got) != 1 {
		t.Fatalf("throttled refusal reached Discord: %v", got)
	}
	t.Log("case: delivered DM recorded; a later restart with the same reason is throttled")
}

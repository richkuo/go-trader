//go:build pyintegration && linux

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type slashDaemonOutput struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (o *slashDaemonOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.Write(p)
}

func (o *slashDaemonOutput) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.String()
}

type slashDaemonFixture struct {
	bin      string
	dir      string
	env      []string
	services *fakeNotifyServices
}

func newSlashDaemonFixture(t *testing.T) *slashDaemonFixture {
	t.Helper()
	bin := pyintegrationBinary(t)
	ca := newEgressTestCA(t)
	services, addr := startFakeNotifyServices(t, ca)
	proxy := startEgressTestProxy(t, "http", "", "", addr, nil)
	dir := t.TempDir()
	caPath := filepath.Join(dir, "fixture-ca.pem")
	if err := os.WriteFile(caPath, egressPEM(ca), 0o600); err != nil {
		t.Fatal(err)
	}
	var env []string
	for _, kv := range os.Environ() {
		key := strings.ToUpper(strings.SplitN(kv, "=", 2)[0])
		if strings.HasSuffix(key, "_PROXY") || strings.HasPrefix(key, "GO_TRADER_EGRESS") || key == "SSL_CERT_FILE" || key == "SSL_CERT_DIR" {
			continue
		}
		env = append(env, kv)
	}
	env = append(env,
		"DISCORD_BOT_TOKEN=fixture-daemon-token", "TELEGRAM_BOT_TOKEN=",
		"HTTPS_PROXY="+proxy.url("127.0.0.1"), "SSL_CERT_FILE="+caPath, "SSL_CERT_DIR="+dir)
	return &slashDaemonFixture{bin: bin, dir: dir, env: env, services: services}
}

func (f *slashDaemonFixture) writeConfig(t *testing.T, name string, discord map[string]any) string {
	t.Helper()
	block := map[string]any{"enabled": true, "owner_id": slashTestOwner, "channels": map[string]string{"spot": "6001"}, "guild_id": slashTestGuild}
	for k, v := range discord {
		block[k] = v
	}
	blob, err := json.MarshalIndent(map[string]any{
		"config_version": CurrentConfigVersion, "interval_seconds": 60, "auto_update": "off",
		"db_file": filepath.Join(f.dir, name+".db"), "log_dir": filepath.Join(f.dir, name+"-logs"),
		"status_token": "fixture-status-token", "strategies": []any{}, "discord": block,
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.dir, name+".json")
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

type slashDaemon struct {
	cmd    *exec.Cmd
	out    *slashDaemonOutput
	port   int
	exited chan struct{}
}

func (f *slashDaemonFixture) startDaemon(t *testing.T, cfgPath string, extra ...string) *slashDaemon {
	t.Helper()
	port := htfITFreePort(t)
	out := &slashDaemonOutput{}
	cmd := exec.Command(f.bin, append([]string{"--config", cfgPath, "--status-port", strconv.Itoa(port)}, extra...)...)
	cmd.Dir = f.dir
	cmd.Env = f.env
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	d := &slashDaemon{cmd: cmd, out: out, port: port, exited: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(d.exited)
	}()
	t.Cleanup(func() {
		select {
		case <-d.exited:
		default:
			_ = cmd.Process.Kill()
			<-d.exited
		}
		if t.Failed() {
			t.Logf("daemon output:\n%s", out.String())
		}
	})
	return d
}

func (d *slashDaemon) get(path string) (int, map[string]any) {
	req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d%s", d.port, path), nil)
	req.Header.Set("Authorization", "Bearer fixture-status-token")
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		return 0, nil
	}
	defer resp.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body
}

func (d *slashDaemon) waitCycle(t *testing.T, services *fakeNotifyServices, heldKind string) int {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		code, status := d.get("/status")
		cycles, _ := status["cycle_count"].(float64)
		hcode, health := d.get("/health")
		if code == http.StatusOK && cycles >= 1 && hcode == http.StatusOK && health["status"] == "ok" {
			if heldKind != "" && services.heldCount(heldKind) != 1 {
				t.Fatalf("the held %s request ended before the first cycle was observed", heldKind)
			}
			return int(cycles)
		}
		if time.Now().After(deadline) {
			t.Fatalf("no trading cycle within 60s (status %d %v, health %d %v); output:\n%s", code, status["cycle_count"], hcode, health, d.out.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (d *slashDaemon) stop(t *testing.T, within time.Duration) time.Duration {
	t.Helper()
	began := time.Now()
	if err := d.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-d.exited:
	case <-time.After(within):
		t.Fatalf("the daemon did not exit within %s of SIGTERM; output:\n%s", within, d.out.String())
	}
	return time.Since(began)
}

func (d *slashDaemon) waitOutput(t *testing.T, needle string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !strings.Contains(d.out.String(), needle) {
		if time.Now().After(deadline) {
			t.Fatalf("daemon output never contained %q:\n%s", needle, d.out.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (f *slashDaemonFixture) runOneShot(t *testing.T, args ...string) (string, error) {
	t.Helper()
	if args[0] != "probe" {
		args = append(args, "--status-port", strconv.Itoa(htfITFreePort(t)))
	}
	cmd := exec.Command(f.bin, args...)
	cmd.Dir = f.dir
	cmd.Env = f.env
	done := make(chan struct{})
	var out []byte
	var err error
	go func() {
		out, err = cmd.CombinedOutput()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(120 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		t.Fatalf("%v did not exit:\n%s", args, out)
	}
	return string(out), err
}

func TestSlashCommandDaemonDoesNotWaitForCommandMaintenance(t *testing.T) {
	t.Run("OffWithAHeldList", func(t *testing.T) {
		f := newSlashDaemonFixture(t)
		s := f.services
		s.seedCommand("", commandPrefix+"status", "Bot status")
		s.seedCommand("", "help", "OpenClaw help")
		cfgPath := f.writeConfig(t, "off", map[string]any{"slash_commands": false})
		release := s.holdCommandOp("list")
		defer release()
		d := f.startDaemon(t, cfgPath)
		slashWaitFor(t, s, "the held cleanup list", 60*time.Second, func() bool { return s.heldCount("list") == 1 })
		cycles := d.waitCycle(t, s, "list")
		if n := len(s.bodiesFor("discord.command_create")) + len(s.bodiesFor("discord.command_edit")); n != 0 {
			t.Fatalf("commands off: %d create or edit request(s)", n)
		}
		took := d.stop(t, 20*time.Second)
		slashWaitFor(t, s, "the held list is abandoned at shutdown", 10*time.Second, func() bool { return len(s.bodiesFor("discord.abandoned:list")) == 1 })
		if !strings.Contains(d.out.String(), "Discord slash commands are off (discord.slash_commands is false)") {
			t.Fatalf("no commands-off line in the output:\n%s", d.out.String())
		}
		t.Logf("case: commands off, cleanup list held: cycle_count=%d and /health ok while the list was pending; no create or edit; SIGTERM exited in %s and dropped the held list", cycles, took.Round(time.Millisecond))

		release()
		d2 := f.startDaemon(t, cfgPath)
		d2.waitOutput(t, "Discord slash command cleanup finished: removed 1 go-trader command(s)", 60*time.Second)
		if got := slashOwnedNames(s, ""); len(got) != 0 {
			t.Fatalf("owned commands after the second start: %v", got)
		}
		d2.stop(t, 20*time.Second)
		t.Log("case: commands off: the next daemon start finished the cleanup the first one left; help stays")
	})

	t.Run("OnWithAHeldCreate", func(t *testing.T) {
		f := newSlashDaemonFixture(t)
		s := f.services
		cfgPath := f.writeConfig(t, "on", map[string]any{"slash_commands": true})
		release := s.holdCommandOp("create")
		defer release()
		d := f.startDaemon(t, cfgPath)
		slashWaitFor(t, s, "the held create", 60*time.Second, func() bool { return s.heldCount("create") == 1 })
		cycles := d.waitCycle(t, s, "create")
		took := d.stop(t, 20*time.Second)
		slashWaitFor(t, s, "the held create is abandoned at shutdown", 10*time.Second, func() bool { return len(s.bodiesFor("discord.abandoned:create")) == 1 })
		t.Logf("case: commands on, first create held: cycle_count=%d and /health ok while the create was pending; SIGTERM exited in %s and dropped the held create", cycles, took.Round(time.Millisecond))
	})

	t.Run("OneShotModesWriteNoCommands", func(t *testing.T) {
		f := newSlashDaemonFixture(t)
		s := f.services
		s.seedCommand("", commandPrefix+"status", "Bot status")
		cfgPath := f.writeConfig(t, "oneshot", map[string]any{"slash_commands": true})
		once := f.startDaemon(t, cfgPath, "--once")
		cycles := once.waitCycle(t, s, "")
		time.Sleep(2 * time.Second)
		once.stop(t, 20*time.Second)
		summaryOut, summaryErr := f.runOneShot(t, "--config", cfgPath, "--summary", "spot")
		if n := s.commandRequests(); n != 0 {
			t.Fatalf("--once and --summary sent %d command request(s); want none: %v", n, s.requestKeys())
		}
		if strings.Contains(once.out.String(), "Discord slash commands are") || strings.Contains(summaryOut, "Discord slash commands are") {
			t.Fatal("a one-shot run started the command worker")
		}
		t.Logf("case: commands on: --once reached cycle %d and ran 2s more, --summary spot exited (err %v); neither started the command worker or sent a command request; summary output tail: %q", cycles, summaryErr, lastLine(summaryOut))
	})

	t.Run("ConfigGuards", func(t *testing.T) {
		f := newSlashDaemonFixture(t)
		cases := []struct {
			name    string
			discord map[string]any
			want    string
		}{
			{"misspelled", map[string]any{"slash_command": true}, `discord: unknown field "slash_command"`},
			{"null", map[string]any{"slash_commands": nil}, "discord.slash_commands must be true or false"},
			{"string", map[string]any{"slash_commands": "yes"}, "slash_commands"},
		}
		for _, c := range cases {
			out, err := f.runOneShot(t, "--config", f.writeConfig(t, "guard-"+c.name, c.discord), "--once")
			if err == nil || !strings.Contains(out, c.want) {
				t.Fatalf("%s: err %v, output lacks %q:\n%s", c.name, err, c.want, out)
			}
			t.Logf("case: discord block %v refused at load: %q", c.discord, c.want)
		}
		out, err := f.runOneShot(t, "probe", "--config", f.writeConfig(t, "legacy", map[string]any{"channel_paper_trades": true, "spot_summary_freq": "hourly"}))
		if err != nil || !strings.Contains(out, "probe: OK") {
			t.Fatalf("a config with retired Discord keys failed to load: %v\n%s", err, out)
		}
		t.Log("case: retired Discord keys channel_paper_trades and spot_summary_freq still load (probe: OK)")
	})

	t.Run("SIGHUPNeedsARestart", func(t *testing.T) {
		f := newSlashDaemonFixture(t)
		cfgPath := f.writeConfig(t, "reload", map[string]any{"slash_commands": false})
		d := f.startDaemon(t, cfgPath)
		d.waitCycle(t, f.services, "")
		f.writeConfig(t, "reload", map[string]any{"slash_commands": true})
		if err := d.cmd.Process.Signal(syscall.SIGHUP); err != nil {
			t.Fatal(err)
		}
		d.waitOutput(t, "discord.slash_commands changed (restart required)", 30*time.Second)
		if n := len(f.services.bodiesFor("discord.command_create")); n != 0 {
			t.Fatalf("a refused reload created %d command(s)", n)
		}
		d.stop(t, 20*time.Second)
		t.Log("case: discord.slash_commands changed on SIGHUP: the reload was refused with restart required and no command was created")
	})
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

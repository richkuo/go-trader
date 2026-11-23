//go:build pyintegration && linux

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const pmdTelegramOwner = "777"

type pmdFixture struct {
	e        *mrpEnv
	services *fakeNotifyServices
	env      []string
}

func newPMDFixture(t *testing.T) *pmdFixture {
	t.Helper()
	e := newMRPEnv(t, 40)
	ca := newEgressTestCA(t)
	services, addr := startFakeNotifyServices(t, ca)
	proxy := startEgressTestProxy(t, "http", "", "", addr, nil)
	certDir := t.TempDir()
	caPath := filepath.Join(certDir, "fixture-ca.pem")
	if err := os.WriteFile(caPath, egressPEM(ca), 0o600); err != nil {
		t.Fatal(err)
	}
	env := []string{
		"DISCORD_BOT_TOKEN=fixture-paper-warning-token", "TELEGRAM_BOT_TOKEN=fixture-paper-warning-telegram",
		"HTTPS_PROXY=" + proxy.url("127.0.0.1"), "SSL_CERT_FILE=" + caPath, "SSL_CERT_DIR=" + certDir,
	}
	return &pmdFixture{e: e, services: services, env: env}
}

func (f *pmdFixture) config(t *testing.T, name string, layout pmpLayout, owners bool, interval int, ids ...string) (string, string) {
	t.Helper()
	f.e.t = t
	dir := t.TempDir()
	discordOwner, telegramOwner := slashTestOwner, pmdTelegramOwner
	if !owners {
		discordOwner, telegramOwner = "", ""
	}
	path := f.e.writeConfig(name, func(doc map[string]any) {
		doc["db_file"] = filepath.Join(dir, "state.db")
		doc["interval_seconds"] = interval
		doc["discord"] = map[string]any{"enabled": true, "owner_id": discordOwner, "channels": map[string]string{"hyperliquid-paper": "6001"}}
		doc["telegram"] = map[string]any{"enabled": true, "owner_chat_id": telegramOwner, "channels": map[string]string{"hyperliquid-paper": "-1006001"}}
		strategies := make([]any, 0, len(ids))
		for _, id := range ids {
			strategies = append(strategies, mrpPaperManualStrategy(id, "short"))
		}
		layout.mutate(doc, dir, strategies)
	})
	return path, filepath.Join(dir, layout.file)
}

func (f *pmdFixture) run(t *testing.T, cfgPath string, args ...string) mrpRun {
	t.Helper()
	f.e.t = t
	if len(args) == 0 {
		args = []string{"--once", "--status-port", strconv.Itoa(htfITFreePort(t))}
	}
	return f.e.command(cfgPath, f.env, args...)
}

func (f *pmdFixture) once(t *testing.T, cfgPath string) mrpRun {
	t.Helper()
	r := f.run(t, cfgPath)
	if r.code != 0 {
		t.Fatalf("--once failed:\n%s", r)
	}
	return r
}

func (f *pmdFixture) dms(id string) (discord, telegram int) {
	needle := "paper manual position " + id + "/BTC"
	seen := map[string]bool{}
	for _, key := range f.services.requestKeys() {
		if seen[key] {
			continue
		}
		seen[key] = true
		if strings.HasPrefix(key, "discord.message:") && strings.Contains(key, "/channels/5001/") {
			for _, body := range f.services.bodiesFor(key) {
				if strings.Contains(body, needle) {
					discord++
				}
			}
		}
	}
	for _, body := range f.services.bodiesFor("telegram.sendMessage") {
		if strings.Contains(body, needle) && strings.Contains(body, `"chat_id":"`+pmdTelegramOwner+`"`) {
			telegram++
		}
	}
	return discord, telegram
}

func (f *pmdFixture) expectDMs(t *testing.T, id string, discord, telegram int, why string) {
	t.Helper()
	d, tg := f.dms(id)
	if d != discord || tg != telegram {
		t.Fatalf("%s: %s direct messages discord=%d telegram=%d, want %d and %d (requests %v)", why, id, d, tg, discord, telegram, f.services.requestKeys())
	}
}

func pmdReceipts(t *testing.T, dbPath, storageID string) (string, []paperWarningReceipt) {
	t.Helper()
	row := pmpPosition(t, dbPath, storageID)
	if !row.found {
		t.Fatalf("no position row for %s in %s", storageID, dbPath)
	}
	return row.posID, parsePaperWarningReceipts(row.receipts)
}

func pmdHasReceipt(receipts []paperWarningReceipt, posID, service, dest string) bool {
	return paperWarningReceiptHeld(receipts, posID, service, dest)
}

func pmdLegacyOpen(t *testing.T, cfgPath, id string) {
	t.Helper()
	pmpQueue(t, cfgPath, PendingManualAction{StrategyID: id, Action: "open", Symbol: "BTC", Side: "short", Quantity: 0.01, FillPrice: 100})
}

func TestPaperManualVenueGuardPositionWarningDelivery(t *testing.T) {
	f := newPMDFixture(t)
	layouts := pmpLayouts()

	for _, layout := range layouts {
		t.Run("lifecycle "+layout.name, func(t *testing.T) {
			id := "pm-life-" + strings.ReplaceAll(layout.name, " ", "-")
			cfg, dbPath := f.config(t, id+".json", layout, true, 60, id)
			pmdLegacyOpen(t, cfg, id)
			first := f.once(t, cfg)
			if n := pmpWarnings(first, id); n != 1 {
				t.Fatalf("the drained legacy open must print once, got %d:\n%s", n, first.stdout)
			}
			f.expectDMs(t, id, 1, 1, "first run")
			posID, receipts := pmdReceipts(t, dbPath, layout.storage(id))
			if !pmdHasReceipt(receipts, posID, "discord", slashTestOwner) || !pmdHasReceipt(receipts, posID, "telegram", pmdTelegramOwner) || len(receipts) != 2 {
				t.Fatalf("each confirmed destination must have a durable receipt in the owning file: %+v", receipts)
			}
			second := f.once(t, cfg)
			if n := pmpWarnings(second, id); n != 1 {
				t.Fatalf("the startup line must print on each run, got %d:\n%s", n, second.stdout)
			}
			f.expectDMs(t, id, 1, 1, "second run with durable receipts")

			pmpQueue(t, cfg, PendingManualAction{StrategyID: id, Action: "close", Symbol: "BTC", Side: "buy", Quantity: 0.01, FillPrice: 100, IsFullClose: true})
			pmdLegacyOpen(t, cfg, id)
			third := f.once(t, cfg)
			if n := pmpWarnings(third, id); n != 2 {
				t.Fatalf("the old position prints at startup and the reopened one prints after the drain, got %d:\n%s", n, third.stdout)
			}
			f.expectDMs(t, id, 2, 2, "a reopened position on the same coin")
			newID, newReceipts := pmdReceipts(t, dbPath, layout.storage(id))
			if newID == posID || len(newReceipts) != 2 || !pmdHasReceipt(newReceipts, newID, "discord", slashTestOwner) || pmdHasReceipt(newReceipts, posID, "discord", slashTestOwner) {
				t.Fatalf("the reopened position must start with its own receipts: old=%s new=%s %+v", posID, newID, newReceipts)
			}
		})
	}

	t.Run("unconfirmed sends stay eligible", func(t *testing.T) {
		cfg, dbPath := f.config(t, "pm-ambiguous.json", layouts[0], true, 60, "pm-ambiguous")
		pmdLegacyOpen(t, cfg, "pm-ambiguous")
		f.services.setMessageResults(true, false, true)
		first := f.once(t, cfg)
		f.services.setMessageResults(false, false, false)
		f.expectDMs(t, "pm-ambiguous", 1, 1, "replies with no message id")
		if strings.Count(first.stdout, "returned no message confirmation") != 2 {
			t.Fatalf("both unconfirmed sends must be logged:\n%s", first.stdout)
		}
		if row := pmpPosition(t, dbPath, "pm-ambiguous"); row.receipts != "" {
			t.Fatalf("an unconfirmed send must record no receipt: %+v", row)
		}
		f.services.setMessageResults(false, true, false)
		f.once(t, cfg)
		f.services.setMessageResults(false, false, false)
		f.expectDMs(t, "pm-ambiguous", 2, 1, "telegram refused on the retry")
		posID, receipts := pmdReceipts(t, dbPath, "pm-ambiguous")
		if len(receipts) != 1 || !pmdHasReceipt(receipts, posID, "discord", slashTestOwner) {
			t.Fatalf("only the confirmed destination gets a receipt: %+v", receipts)
		}
		f.once(t, cfg)
		f.expectDMs(t, "pm-ambiguous", 2, 2, "telegram retried")
		if _, receipts := pmdReceipts(t, dbPath, "pm-ambiguous"); len(receipts) != 2 {
			t.Fatalf("the retried destination must now have its receipt: %+v", receipts)
		}
	})

	t.Run("no owner destination keeps the message eligible", func(t *testing.T) {
		noOwner, dbPath := f.config(t, "pm-noowner.json", layouts[0], false, 60, "pm-noowner")
		pmdLegacyOpen(t, noOwner, "pm-noowner")
		if r := f.once(t, noOwner); pmpWarnings(r, "pm-noowner") != 1 {
			t.Fatalf("the warning must print with no owner destination:\n%s", r.stdout)
		}
		f.expectDMs(t, "pm-noowner", 0, 0, "no owner destination")
		if row := pmpPosition(t, dbPath, "pm-noowner"); row.receipts != "" {
			t.Fatalf("no destination records no receipt: %+v", row)
		}
		pmdAddOwners(t, noOwner)
		f.once(t, noOwner)
		f.expectDMs(t, "pm-noowner", 1, 1, "owner destinations added")
	})

	t.Run("read-only reports print and never send", func(t *testing.T) {
		cfg, dbPath := f.config(t, "pm-report.json", layouts[0], false, 60, "pm-report")
		pmdLegacyOpen(t, cfg, "pm-report")
		f.once(t, cfg)
		pmdAddOwners(t, cfg)
		for _, args := range [][]string{{"--summary=hyperliquid-paper"}, {"--leaderboard"}} {
			r := f.run(t, cfg, args...)
			if n := pmpWarnings(r, "pm-report"); n != 1 {
				t.Fatalf("%v must print the position warning once, got %d:\n%s", args, n, r)
			}
		}
		f.expectDMs(t, "pm-report", 0, 0, "read-only reports")
		if row := pmpPosition(t, dbPath, "pm-report"); row.receipts != "" {
			t.Fatalf("a read-only report must record no receipt: %+v", row)
		}
		f.once(t, cfg)
		f.expectDMs(t, "pm-report", 1, 1, "the scheduler run after the reports")
	})

	rejectReceipts := `CREATE TRIGGER fixture_reject_warning_receipts BEFORE INSERT ON positions WHEN NEW.paper_warning_receipts_json <> '' BEGIN SELECT RAISE(ABORT, 'fixture rejects warning receipts'); END`

	t.Run("an unsaved receipt does not suppress the next run", func(t *testing.T) {
		cfg, dbPath := f.config(t, "pm-savefail.json", layouts[0], true, 60, "pm-savefail")
		pmdLegacyOpen(t, cfg, "pm-savefail")
		f.once(t, cfg)
		f.expectDMs(t, "pm-savefail", 1, 1, "first run")
		pmpExec(t, dbPath, `UPDATE positions SET paper_warning_receipts_json = '' WHERE strategy_id = 'pm-savefail'`)
		pmpExec(t, dbPath, rejectReceipts)
		rejected := f.once(t, cfg)
		if !strings.Contains(rejected.stdout, "warning receipt for pm-savefail could not be saved") {
			t.Fatalf("the failed receipt save must be logged:\n%s", rejected.stdout)
		}
		f.expectDMs(t, "pm-savefail", 2, 2, "receipts removed from the file")
		if row := pmpPosition(t, dbPath, "pm-savefail"); row.receipts != "" {
			t.Fatalf("the rejected receipt must not be durable: %+v", row)
		}
		pmpExec(t, dbPath, `DROP TRIGGER fixture_reject_warning_receipts`)
		f.once(t, cfg)
		f.expectDMs(t, "pm-savefail", 3, 3, "restart with no durable receipt sends again")
		if _, receipts := pmdReceipts(t, dbPath, "pm-savefail"); len(receipts) != 2 {
			t.Fatalf("the send after the trigger is gone must record durable receipts: %+v", receipts)
		}
	})

	t.Run("a receipt kept in memory is saved by the next book save", func(t *testing.T) {
		cfg, dbPath := f.config(t, "pm-recover.json", layouts[0], true, 2, "pm-recover")
		pmdLegacyOpen(t, cfg, "pm-recover")
		f.once(t, cfg)
		pmpExec(t, dbPath, `UPDATE positions SET paper_warning_receipts_json = '' WHERE strategy_id = 'pm-recover'`)
		before, beforeTG := f.dms("pm-recover")
		pmpExec(t, dbPath, rejectReceipts)
		f.e.t = t
		d := f.e.startDaemon(cfg, htfITFreePort(t), f.env...)
		d.waitLog("the rejected receipt save", 90*time.Second, func(out string) bool {
			return strings.Contains(out, "warning receipt for pm-recover could not be saved")
		})
		pmpExec(t, dbPath, `DROP TRIGGER fixture_reject_warning_receipts`)
		deadline := time.Now().Add(90 * time.Second)
		for {
			if row := pmpPosition(t, dbPath, "pm-recover"); row.receipts != "" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the in-memory receipts were never saved:\n%s", d.output())
			}
			time.Sleep(500 * time.Millisecond)
		}
		d.stop()
		f.expectDMs(t, "pm-recover", before+1, beforeTG+1, "one send per destination across the daemon's cycles")
		f.once(t, cfg)
		f.expectDMs(t, "pm-recover", before+1, beforeTG+1, "the receipts saved by the later book save")
	})
}

func pmdDebugLog(t *testing.T, path string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	doc["log_level"] = "debug"
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
}

func pmdCycleStarts(out string) int {
	return strings.Count(out, "=== Cycle ")
}

func (f *pmdFixture) refusedDiscordDMs(id string) int {
	needle := "paper manual position " + id + "/BTC"
	n := 0
	seen := map[string]bool{}
	for _, key := range f.services.requestKeys() {
		if seen[key] || !strings.HasPrefix(key, "discord.message_refused:") || !strings.Contains(key, "/channels/5001/") {
			continue
		}
		seen[key] = true
		for _, body := range f.services.bodiesFor(key) {
			if strings.Contains(body, needle) {
				n++
			}
		}
	}
	return n
}

func TestPaperManualVenueGuardPositionWarningBackoff(t *testing.T) {
	f := newPMDFixture(t)
	layouts := pmpLayouts()

	t.Run("a hanging destination costs one timeout per cycle and recovers", func(t *testing.T) {
		ids := []string{"pm-hang-a", "pm-hang-b", "pm-hang-c"}
		cfg, dbPath := f.config(t, "pm-hang.json", layouts[0], true, 2, ids...)
		for _, id := range ids {
			pmdLegacyOpen(t, cfg, id)
		}
		f.services.setTelegramHang(pmdTelegramOwner)
		f.e.t = t
		d := f.e.startDaemon(cfg, htfITFreePort(t), f.env...)
		d.waitLog("the hung telegram send to fail", 120*time.Second, func(out string) bool {
			return strings.Contains(out, "to the telegram owner failed")
		})
		f.services.setTelegramHang("")
		if n := len(f.services.bodiesFor("telegram.sendMessage_hung")); n != 1 {
			t.Fatalf("the cycle must stop sending to the hung destination after its first timeout, got %d hung sends:\n%s", n, d.output())
		}
		if n := strings.Count(d.output(), "to the telegram owner failed"); n != 1 {
			t.Fatalf("one telegram failure per cycle, got %d:\n%s", n, d.output())
		}
		deadline := time.Now().Add(90 * time.Second)
		for {
			done := 0
			for _, id := range ids {
				posID, receipts := pmdReceipts(t, dbPath, id)
				if pmdHasReceipt(receipts, posID, "telegram", pmdTelegramOwner) {
					done++
				}
			}
			if done == len(ids) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("telegram receipts were never recorded after recovery:\n%s", d.output())
			}
			time.Sleep(500 * time.Millisecond)
		}
		d.stop()
		if n := len(f.services.bodiesFor("telegram.sendMessage_hung")); n != 1 {
			t.Fatalf("no send may reach the destination after it recovered and before its backoff ended, got %d hung sends", n)
		}
		for _, id := range ids {
			f.expectDMs(t, id, 1, 1, "one telegram send per position after recovery")
		}
	})

	t.Run("a refusing destination backs off and others still record", func(t *testing.T) {
		cfg, dbPath := f.config(t, "pm-refuse.json", layouts[0], true, 2, "pm-refuse")
		pmdDebugLog(t, cfg)
		pmdLegacyOpen(t, cfg, "pm-refuse")
		f.services.setMessageDown(true)
		defer f.services.setMessageDown(false)
		f.e.t = t
		d := f.e.startDaemon(cfg, htfITFreePort(t), f.env...)
		d.waitLog("three discord failures", 120*time.Second, func(out string) bool {
			return strings.Count(out, "to the discord owner failed") >= 3
		})
		d.stop()
		out := d.output()
		attempts := f.refusedDiscordDMs("pm-refuse")
		cycles := pmdCycleStarts(out)
		if attempts < 3 || attempts >= cycles {
			t.Fatalf("refused discord attempts %d must be fewer than the %d cycles:\n%s", attempts, cycles, out)
		}
		last := 0
		for _, want := range []string{"waits 2 cycles", "waits 4 cycles", "waits 8 cycles"} {
			i := strings.Index(out[last:], want)
			if i < 0 {
				t.Fatalf("the discord backoff must double (%s missing after offset %d):\n%s", want, last, out)
			}
			last += i + len(want)
		}
		posID, receipts := pmdReceipts(t, dbPath, "pm-refuse")
		if len(receipts) != 1 || !pmdHasReceipt(receipts, posID, "telegram", pmdTelegramOwner) {
			t.Fatalf("telegram must record its receipt while discord refuses: %+v", receipts)
		}
		f.expectDMs(t, "pm-refuse", 0, 1, "telegram sent once while discord refuses")
	})
}

func pmdAddOwners(t *testing.T, path string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	owned := strings.Replace(string(raw), `"owner_id": ""`, `"owner_id": "`+slashTestOwner+`"`, 1)
	owned = strings.Replace(owned, `"owner_chat_id": ""`, `"owner_chat_id": "`+pmdTelegramOwner+`"`, 1)
	if owned == string(raw) {
		t.Fatalf("the config at %s has no empty owner fields", path)
	}
	if err := os.WriteFile(path, []byte(owned), 0o600); err != nil {
		t.Fatal(err)
	}
}

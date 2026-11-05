package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

const pcOwner = "owner-1"

type pcTransport struct {
	env       *entryTestEnv
	mu        sync.Mutex
	prompts   []string
	responds  []string
	followups []string
}

func (tr *pcTransport) ChannelMessageSend(channelID, content string, _ ...discordgo.RequestOption) (*discordgo.Message, error) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.prompts = append(tr.prompts, content)
	return &discordgo.Message{ID: "m", ChannelID: channelID, Content: content}, nil
}

func (tr *pcTransport) UserChannelCreate(recipientID string, _ ...discordgo.RequestOption) (*discordgo.Channel, error) {
	return &discordgo.Channel{ID: "dm-" + recipientID}, nil
}

func (tr *pcTransport) InteractionRespond(_ *discordgo.Interaction, resp *discordgo.InteractionResponse, _ ...discordgo.RequestOption) error {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	content := ""
	if resp != nil && resp.Data != nil {
		content = resp.Data.Content
	}
	tr.responds = append(tr.responds, content)
	return nil
}

func (tr *pcTransport) FollowupMessageCreate(_ *discordgo.Interaction, _ bool, data *discordgo.WebhookParams, _ ...discordgo.RequestOption) (*discordgo.Message, error) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.followups = append(tr.followups, data.Content)
	return &discordgo.Message{}, nil
}

func (tr *pcTransport) ApplicationCommands(string, string, ...discordgo.RequestOption) ([]*discordgo.ApplicationCommand, error) {
	return nil, nil
}

func (tr *pcTransport) ApplicationCommandCreate(string, string, *discordgo.ApplicationCommand, ...discordgo.RequestOption) (*discordgo.ApplicationCommand, error) {
	return nil, nil
}

func (tr *pcTransport) ApplicationCommandEdit(string, string, string, *discordgo.ApplicationCommand, ...discordgo.RequestOption) (*discordgo.ApplicationCommand, error) {
	return nil, nil
}

func (tr *pcTransport) ApplicationCommandDelete(string, string, string, ...discordgo.RequestOption) error {
	return nil
}

type pcFixture struct {
	t         *testing.T
	env       *entryTestEnv
	ss        *StatusServer
	d         *DiscordNotifier
	tr        *pcTransport
	marksMu   sync.Mutex
	marks     map[string]float64
	markCalls int
	markHook  func(call int)
	restarts  *atomic.Int32
}

func newPCFixture(t *testing.T, cfg *Config) *pcFixture {
	t.Helper()
	env := openEntryTestEnv(t, cfg)
	useEntryTradeRecorder(t, env.store)
	f := &pcFixture{t: t, env: env, marks: map[string]float64{}, restarts: &atomic.Int32{}}
	ss := &StatusServer{state: env.state, mu: &env.mu, stateDB: env.store, strategies: cfg.Strategies, intervalSeconds: cfg.IntervalSeconds, uiCfg: cfg}
	ss.paperCloseMarks = func(sc StrategyConfig, plan *paperClosePlan) (map[string]paperCloseMark, error) {
		f.marksMu.Lock()
		f.markCalls++
		call := f.markCalls
		hook := f.markHook
		out := map[string]paperCloseMark{}
		for _, leg := range plan.Legs {
			if px, ok := f.marks[leg.Key]; ok && leg.Kind == paperCloseLegPosition {
				out[leg.Key] = paperCloseMark{Price: px, Source: "test mark"}
			}
		}
		f.marksMu.Unlock()
		if hook != nil {
			hook(call)
		}
		return out, nil
	}
	ss.restartFn = func() error {
		f.restarts.Add(1)
		return nil
	}
	st := discordgo.NewState()
	st.User = &discordgo.User{ID: "bot-1"}
	tr := &pcTransport{env: env}
	f.tr = tr
	f.ss = ss
	f.d = &DiscordNotifier{session: &discordgo.Session{State: st}, transport: tr, ownerID: pcOwner, ss: ss}
	return f
}

func (f *pcFixture) setMark(sym string, px float64) {
	f.marksMu.Lock()
	f.marks[sym] = px
	f.marksMu.Unlock()
}

func (f *pcFixture) setMarkHook(h func(call int)) {
	f.marksMu.Lock()
	f.markHook = h
	f.markCalls = 0
	f.marksMu.Unlock()
}

func pcOpts(kv ...string) []*discordgo.ApplicationCommandInteractionDataOption {
	var opts []*discordgo.ApplicationCommandInteractionDataOption
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] == "" {
			continue
		}
		opts = append(opts, &discordgo.ApplicationCommandInteractionDataOption{Name: kv[i], Type: discordgo.ApplicationCommandOptionString, Value: kv[i+1]})
	}
	return opts
}

func (f *pcFixture) run(command, userID, guildID string, opts []*discordgo.ApplicationCommandInteractionDataOption, reply string, hook func()) string {
	f.t.Helper()
	ic := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		Type:    discordgo.InteractionApplicationCommand,
		GuildID: guildID,
		Data:    discordgo.ApplicationCommandInteractionData{Name: commandPrefix + command, Options: opts},
	}}
	if guildID != "" {
		ic.Member = &discordgo.Member{User: &discordgo.User{ID: userID}}
	} else {
		ic.User = &discordgo.User{ID: userID}
	}
	f.tr.mu.Lock()
	r0, f0 := len(f.tr.responds), len(f.tr.followups)
	f.tr.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		if reply == "" {
			return
		}
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			f.d.mu.Lock()
			n := len(f.d.dmHandlers)
			f.d.mu.Unlock()
			if n > 0 {
				break
			}
			time.Sleep(time.Millisecond)
		}
		if hook != nil {
			hook()
		}
		f.d.handleDirectMessage(&discordgo.MessageCreate{Message: &discordgo.Message{Author: &discordgo.User{ID: userID}, Content: reply}})
	}()
	f.d.interactionCreate(f.tr, ic)
	<-done
	f.tr.mu.Lock()
	defer f.tr.mu.Unlock()
	if len(f.tr.followups) > f0 {
		return f.tr.followups[len(f.tr.followups)-1]
	}
	if len(f.tr.responds) > r0 {
		return f.tr.responds[len(f.tr.responds)-1]
	}
	return ""
}

func (f *pcFixture) promptCount() int {
	f.tr.mu.Lock()
	defer f.tr.mu.Unlock()
	return len(f.tr.prompts)
}

func pcDumpFile(t *testing.T, db *StateDB) string {
	t.Helper()
	var b strings.Builder
	for _, table := range []string{"strategies", "positions", "option_positions", "trades", "closed_positions", "closed_option_positions", "pending_manual_actions"} {
		rows, err := db.db.Query("SELECT * FROM " + table + " ORDER BY rowid")
		if err != nil {
			t.Fatalf("dump %s: %v", table, err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				rows.Close()
				t.Fatalf("scan %s: %v", table, err)
			}
			fmt.Fprintf(&b, "%s:", table)
			for _, v := range vals {
				if raw, ok := v.([]byte); ok {
					v = string(raw)
				}
				fmt.Fprintf(&b, " %v", v)
			}
			b.WriteString("\n")
		}
		rows.Close()
	}
	return b.String()
}

func (f *pcFixture) dumpAll() map[storageRole]string {
	f.t.Helper()
	out := map[storageRole]string{}
	for _, role := range f.env.store.order {
		out[role] = pcDumpFile(f.t, f.env.store.file(role))
	}
	return out
}

func (f *pcFixture) memory(id string) string {
	f.t.Helper()
	f.env.mu.RLock()
	defer f.env.mu.RUnlock()
	b, err := json.Marshal(f.env.state.Strategies[id])
	if err != nil {
		f.t.Fatal(err)
	}
	return string(b)
}

func (f *pcFixture) requireUnchanged(what string, id string, run func() string, want ...string) {
	f.t.Helper()
	files := f.dumpAll()
	mem := f.memory(id)
	msg := run()
	for _, w := range want {
		if !strings.Contains(msg, w) {
			f.t.Fatalf("%s: reply %q must contain %q", what, msg, w)
		}
	}
	after := f.dumpAll()
	for role, before := range files {
		if after[role] != before {
			f.t.Fatalf("%s changed state file %s:\nbefore %s\nafter  %s", what, role, before, after[role])
		}
	}
	if got := f.memory(id); got != mem {
		f.t.Fatalf("%s changed the in-memory book:\nbefore %s\nafter  %s", what, mem, got)
	}
}

func pcCount(t *testing.T, env *entryTestEnv, id, query string) int {
	t.Helper()
	db := env.fileFor(id)
	sid, err := db.toStorageID(id)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.db.QueryRow(query, sid).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func pcSplitConfig(t *testing.T) *Config {
	t.Helper()
	dir := t.TempDir()
	hl := StrategyConfig{ID: "pc-hl", Type: "perps", Platform: "hyperliquid", Args: []string{"sma", "ETH", "1h", "--mode=paper"}, Capital: 10000, Leverage: 1}
	src := StrategyConfig{ID: "pc-src", Type: "perps", Platform: "hyperliquid", Args: []string{"sma", "SOL", "1h", "--mode=paper"}, Capital: 10000, Leverage: 1,
		PaperSource: "src", StorageStrategyID: "hl-src-alias"}
	return &Config{
		DBFile:          filepath.Join(dir, "live.db"),
		PaperDBFile:     filepath.Join(dir, "paper.db"),
		PaperSources:    []PaperSourceConfig{{ID: "src", DBFile: filepath.Join(dir, "src.db")}},
		IntervalSeconds: 600,
		Strategies: []StrategyConfig{
			{ID: "pc-spot", Type: "spot", Platform: "binanceus", Args: []string{"sma_crossover", "BTC/USDT", "1h"}, Capital: 10000},
			{ID: "pc-fut", Type: "futures", Platform: "topstep", Args: []string{"sma", "ES", "1h", "--mode=paper"}, Capital: 50000},
			hl,
			src,
			ocSC("pc-opt", "deribit", "BTC"),
			hlLivePerps("pc-live", "BTC", 1000),
			{ID: "pc-quote", Type: "spot", Platform: "binanceus", Args: []string{"sma_crossover", "ETH/USDT", "1h"}, Capital: 10000},
		},
	}
}

func pcDeribitExpiry(at time.Time) (string, time.Time) {
	day := time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, time.UTC)
	return day.Format("2006-01-02"), day.Add(8 * time.Hour)
}

func pcSeed(t *testing.T, f *pcFixture, clock *paperFundingTestClock) {
	t.Helper()
	env := f.env
	now := time.Now().UTC()
	env.mu.Lock()
	spot := env.strat("pc-spot")
	spot.Cash = 5000
	spot.Positions["BTC/USDT"] = &Position{Symbol: "BTC/USDT", TradePositionID: "pc-spot-btc", Quantity: 0.1, AvgCost: 50000, Side: "long", OpenedAt: now}
	fut := env.strat("pc-fut")
	fut.Cash = 50000
	fut.Positions["ES"] = &Position{Symbol: "ES", TradePositionID: "pc-fut-es", Quantity: 1, AvgCost: 5000, Side: "long", Multiplier: 50, OpenedAt: now}
	opt := env.strat("pc-opt")
	opt.Cash = 100000
	liveExpiry, liveCutoff := pcDeribitExpiry(now.AddDate(0, 0, 30))
	pastExpiry, pastCutoff := pcDeribitExpiry(now.AddDate(0, 0, -1))
	opt.OptionPositions["BTC-call-buy-90000-"+liveExpiry] = &OptionPosition{ID: "BTC-call-buy-90000-" + liveExpiry, TradePositionID: "pc-opt-live",
		Underlying: "BTC", OptionType: "call", Strike: 90000, Expiry: liveExpiry, Action: "buy", Quantity: 1,
		EntryPremiumUSD: 1000, CurrentValueUSD: 1200, OpenedAt: now.AddDate(0, 0, -2), MarkUpdatedAt: now, MarkSource: "deribit mark",
		ExpiryCutoffAt: liveCutoff, markVerifiedAt: now}
	opt.OptionPositions["BTC-put-sell-80000-"+pastExpiry] = &OptionPosition{ID: "BTC-put-sell-80000-" + pastExpiry, TradePositionID: "pc-opt-past",
		Underlying: "BTC", OptionType: "put", Strike: 80000, Expiry: pastExpiry, Action: "sell", Quantity: 1,
		EntryPremiumUSD: 500, CurrentValueUSD: -100, OpenedAt: now.AddDate(0, 0, -9), ExpiryCutoffAt: pastCutoff, markVerifiedAt: now}
	quote := env.strat("pc-quote")
	quote.Positions["ETH/USDT"] = &Position{Symbol: "ETH/USDT", TradePositionID: "pc-quote-eth", Quantity: 1, AvgCost: 2000, Side: "long", OpenedAt: now}
	live := env.strat("pc-live")
	live.Positions["BTC"] = &Position{Symbol: "BTC", TradePositionID: "pc-live-btc", Quantity: 0.01, AvgCost: 60000, Side: "long", Multiplier: 1, OpenedAt: now}
	syncPaperFundingEligibility(env.state, env.cfg.Strategies, clock.now())
	env.mu.Unlock()

	clock.set(clock.ms + 60_000)
	logger := silentStrategyLogger("pc-hl")
	env.mu.Lock()
	if n, err := ExecutePerpsSignalWithLeverage(env.strat("pc-hl"), 1, "ETH", 2000, PerpsSizing{SizingLeverage: 1, ExchangeLeverage: 1}, 0.5, "", 0, DirectionBoth, 0, logger); err != nil || n != 1 {
		env.mu.Unlock()
		t.Fatalf("open ETH: n=%d err=%v", n, err)
	}
	if n, err := ExecutePerpsSignalWithLeverage(env.strat("pc-src"), 1, "SOL", 100, PerpsSizing{SizingLeverage: 1, ExchangeLeverage: 1}, 10, "", 0, DirectionBoth, 0, logger); err != nil || n != 1 {
		env.mu.Unlock()
		t.Fatalf("open SOL: n=%d err=%v", n, err)
	}
	env.mu.Unlock()
	env.saveAll()
}

func TestPaperCloseIntegration(t *testing.T) {
	clock := withPaperFundingClock(t, 1_800_000_000_000)
	cfg := pcSplitConfig(t)
	f := newPCFixture(t, cfg)
	pcSeed(t, f, clock)
	clock.set(clock.ms + 60_000)
	f.setMark("BTC/USDT", 55000)
	f.setMark("ES", 5010)
	f.setMark("ETH", 2100)
	f.setMark("SOL", 90)

	t.Run("slash command is registered as an owner DM ops command", func(t *testing.T) {
		var found *discordgo.ApplicationCommand
		for _, c := range slashCommands() {
			if c.Name == commandPrefix+paperCloseCommandName {
				found = c
			}
		}
		if found == nil || found.Contexts == nil || len(*found.Contexts) != 1 || (*found.Contexts)[0] != discordgo.InteractionContextBotDM || len(found.Description) > 100 {
			t.Fatalf("paper-close must be a bot-DM slash command with a short description: %+v", found)
		}
		if len(found.Options) != 2 || found.Options[0].Name != "strategy" || !found.Options[0].Required || found.Options[1].Name != "symbol" || found.Options[1].Required {
			t.Fatalf("paper-close must take a required strategy and an optional symbol: %+v", found.Options)
		}
		if !opsCommandNames[paperCloseCommandName] {
			t.Fatal("paper-close must be an owner-only ops command")
		}
	})

	t.Run("refusals leave every state file and the book unchanged", func(t *testing.T) {
		f.env.mu.Lock()
		f.env.strat("pc-quote").Quote = &quoteOwnerState{}
		f.env.mu.Unlock()
		defer func() {
			f.env.mu.Lock()
			f.env.strat("pc-quote").Quote = nil
			f.env.mu.Unlock()
		}()
		f.requireUnchanged("non-owner", "pc-spot", func() string {
			return f.run(paperCloseCommandName, "intruder", "", pcOpts("strategy", "pc-spot"), "", nil)
		}, "owner-only")
		f.requireUnchanged("guild call", "pc-spot", func() string {
			return f.run(paperCloseCommandName, pcOwner, "guild-1", pcOpts("strategy", "pc-spot"), "", nil)
		}, "only available in a DM")
		f.requireUnchanged("live strategy", "pc-live", func() string {
			return f.run(paperCloseCommandName, pcOwner, "", pcOpts("strategy", "pc-live"), "", nil)
		}, "is live", "force-close")
		f.requireUnchanged("quote-owned book", "pc-quote", func() string {
			return f.run(paperCloseCommandName, pcOwner, "", pcOpts("strategy", "pc-quote"), "", nil)
		}, "quote-owned", inventoryQuoteExitCloseName)

		for _, bad := range []float64{0, math.NaN()} {
			f.setMark("ETH", bad)
			f.requireUnchanged(fmt.Sprintf("mark %v", bad), "pc-hl", func() string {
				return f.run(paperCloseCommandName, pcOwner, "", pcOpts("strategy", "pc-hl"), "", nil)
			}, "no usable mark", "ETH")
		}
		f.marksMu.Lock()
		delete(f.marks, "ETH")
		f.marksMu.Unlock()
		f.requireUnchanged("missing mark", "pc-hl", func() string {
			return f.run(paperCloseCommandName, pcOwner, "", pcOpts("strategy", "pc-hl"), "", nil)
		}, "no usable mark", "ETH: no mark")
		f.setMark("ETH", 2100)

		key := ""
		f.env.mu.RLock()
		for k, leg := range f.env.strat("pc-opt").OptionPositions {
			if leg.Action == "buy" {
				key = k
			}
		}
		f.env.mu.RUnlock()
		for _, tc := range []struct {
			name   string
			mutate func(*OptionPosition)
			want   string
		}{
			{"failed refresh", func(p *OptionPosition) { p.MarkError = "venue down" }, "last mark refresh failed"},
			{"no refresh since restart", func(p *OptionPosition) { p.markVerifiedAt = time.Time{} }, "no successful mark refresh"},
			{"stale mark", func(p *OptionPosition) { p.markVerifiedAt = time.Now().Add(-24 * time.Hour) }, "older than"},
		} {
			f.env.mu.Lock()
			leg := f.env.strat("pc-opt").OptionPositions[key]
			saved := *leg
			tc.mutate(leg)
			f.env.mu.Unlock()
			f.requireUnchanged("option "+tc.name, "pc-opt", func() string {
				return f.run(paperCloseCommandName, pcOwner, "", pcOpts("strategy", "pc-opt"), "", nil)
			}, "fresh-mark gate", tc.want)
			f.env.mu.Lock()
			*leg = saved
			f.env.mu.Unlock()
		}

		if err := f.env.store.InsertPendingManualAction(PendingManualAction{StrategyID: "pc-hl", Action: "close", Symbol: "ETH", Side: "sell", Quantity: 0.5}); err != nil {
			t.Fatalf("queue manual action: %v", err)
		}
		f.requireUnchanged("queued manual action", "pc-hl", func() string {
			return f.run(paperCloseCommandName, pcOwner, "", pcOpts("strategy", "pc-hl"), "confirm", nil)
		}, "already submitted")
		db := f.env.fileFor("pc-hl")
		if _, err := db.db.Exec("DELETE FROM pending_manual_actions"); err != nil {
			t.Fatal(err)
		}

		files := f.dumpAll()
		cash := f.env.strat("pc-hl").Cash
		trades := len(f.env.strat("pc-hl").TradeHistory)
		msg := f.run(paperCloseCommandName, pcOwner, "", pcOpts("strategy", "pc-hl"), "confirm", func() {
			f.env.mu.Lock()
			f.env.strat("pc-hl").Positions["ETH"].TradePositionID += "-moved"
			f.env.mu.Unlock()
		})
		if !strings.Contains(msg, "changed while you were confirming") {
			t.Fatalf("a book change during the confirm must refuse: %q", msg)
		}
		f.env.mu.Lock()
		p := f.env.strat("pc-hl").Positions["ETH"]
		p.TradePositionID = strings.TrimSuffix(p.TradePositionID, "-moved")
		gotCash, gotTrades, qty := f.env.strat("pc-hl").Cash, len(f.env.strat("pc-hl").TradeHistory), p.Quantity
		f.env.mu.Unlock()
		if gotCash != cash || gotTrades != trades || qty != 0.5 {
			t.Fatalf("the refused close must book nothing: cash %v->%v trades %d->%d qty %v", cash, gotCash, trades, gotTrades, qty)
		}
		for role, b := range files {
			if f.dumpAll()[role] != b {
				t.Fatalf("the refused close changed state file %s", role)
			}
		}

		f.requireUnchanged("no confirmation", "pc-hl", func() string {
			return f.run(paperCloseCommandName, pcOwner, "", pcOpts("strategy", "pc-hl"), "no", nil)
		}, "Cancelled")
	})

	t.Run("an aliased paper source writes only its own file under the alias", func(t *testing.T) {
		before := f.dumpAll()
		cashBefore := f.env.strat("pc-src").Cash
		f.setMarkHook(func(call int) {
			if call < 2 {
				return
			}
			if !f.env.mu.TryLock() {
				t.Errorf("marks must be fetched with the state lock free")
			} else {
				f.env.mu.Unlock()
			}
			if rel, err := acquireManualActionFileLockWithWait(f.env.fileFor("pc-src").path, 0); err == nil {
				rel()
				t.Errorf("marks must be fetched while the manual-action lock is held")
			}
		})
		defer f.setMarkHook(nil)
		msg := f.run(paperCloseCommandName, pcOwner, "", pcOpts("strategy", "pc-src"), "confirm", nil)
		if !strings.Contains(msg, "Paper-closed 1 leg(s) of `pc-src` (partition paper:src)") || !strings.Contains(msg, "realized PnL $-100.00") || !strings.Contains(msg, "ConsecutiveLosses 0 -> 1") {
			t.Fatalf("source close reply = %q", msg)
		}
		after := f.dumpAll()
		for role, b := range before {
			changed := after[role] != b
			if wantChanged := role == f.env.store.ident.mustRole(t, "pc-src"); changed != wantChanged {
				t.Fatalf("file %s changed=%v, want %v", role, changed, wantChanged)
			}
		}
		srcDB := f.env.fileFor("pc-src")
		var aliasCloses, processCloses int
		_ = srcDB.db.QueryRow("SELECT COUNT(*) FROM trades WHERE strategy_id = 'hl-src-alias' AND is_close = 1 AND details LIKE '%operator_paper_close%'").Scan(&aliasCloses)
		_ = srcDB.db.QueryRow("SELECT COUNT(*) FROM trades WHERE strategy_id = 'pc-src'").Scan(&processCloses)
		if aliasCloses != 1 || processCloses != 0 {
			t.Fatalf("source rows: alias closes=%d process-id rows=%d, want 1 and 0", aliasCloses, processCloses)
		}
		if n := pcCount(t, f.env, "pc-src", "SELECT COUNT(*) FROM positions WHERE strategy_id = ?"); n != 0 {
			t.Fatalf("source positions after close = %d, want 0", n)
		}
		s := f.env.strat("pc-src")
		if got := s.Cash - cashBefore; math.Abs(got-(-100)) > 1e-9 || s.RiskState.ConsecutiveLosses != 1 || math.Abs(s.RiskState.DailyPnL-(-100)) > 1e-9 {
			t.Fatalf("source cash delta=%v losses=%d daily=%v", got, s.RiskState.ConsecutiveLosses, s.RiskState.DailyPnL)
		}
	})

	t.Run("spot, futures, perps and options close at the mark with the operator reason", func(t *testing.T) {
		type want struct {
			id      string
			pnl     float64
			cash    float64
			trades  int
			kept    int
			closedQ string
		}
		cases := []want{
			{"pc-spot", 500, 5500, 1, 0, "SELECT COUNT(*) FROM closed_positions WHERE strategy_id = ? AND close_reason = 'operator_paper_close'"},
			{"pc-fut", 500, 500, 1, 0, "SELECT COUNT(*) FROM closed_positions WHERE strategy_id = ? AND close_reason = 'operator_paper_close'"},
			{"pc-hl", 50, 50, 1, 0, "SELECT COUNT(*) FROM closed_positions WHERE strategy_id = ? AND close_reason = 'operator_paper_close'"},
			{"pc-opt", 200, 1200, 1, 1, "SELECT COUNT(*) FROM closed_option_positions WHERE strategy_id = ? AND close_reason = 'operator_paper_close'"},
		}
		for _, c := range cases {
			s := f.env.strat(c.id)
			cashBefore := s.Cash
			msg := f.run(paperCloseCommandName, pcOwner, "", pcOpts("strategy", c.id), "confirm", nil)
			if !strings.HasPrefix(msg, "Paper-closed") || !strings.Contains(msg, fmt.Sprintf("realized PnL $%.2f", c.pnl)) {
				t.Fatalf("%s reply = %q", c.id, msg)
			}
			if c.kept > 0 && !strings.Contains(msg, "kept BTC-put-sell-80000-") {
				t.Fatalf("%s reply must list the leg kept for settlement: %q", c.id, msg)
			}
			if got := s.Cash - cashBefore; math.Abs(got-c.cash) > 1e-6 {
				t.Fatalf("%s cash delta = %v, want %v", c.id, got, c.cash)
			}
			if math.Abs(s.RiskState.DailyPnL-c.pnl) > 1e-6 || s.RiskState.ConsecutiveLosses != 0 {
				t.Fatalf("%s risk: daily=%v losses=%d", c.id, s.RiskState.DailyPnL, s.RiskState.ConsecutiveLosses)
			}
			if n := pcCount(t, f.env, c.id, "SELECT COUNT(*) FROM trades WHERE strategy_id = ? AND is_close = 1 AND details LIKE '%operator_paper_close%'"); n != c.trades {
				t.Fatalf("%s close trades = %d, want %d", c.id, n, c.trades)
			}
			if n := pcCount(t, f.env, c.id, c.closedQ); n != c.trades {
				t.Fatalf("%s closed rows with the operator reason = %d, want %d", c.id, n, c.trades)
			}
			if n := pcCount(t, f.env, c.id, "SELECT COUNT(*) FROM option_positions WHERE strategy_id = ?"); n != c.kept {
				t.Fatalf("%s option rows left = %d, want %d", c.id, n, c.kept)
			}
			if n := pcCount(t, f.env, c.id, "SELECT COUNT(*) FROM positions WHERE strategy_id = ?"); n != 0 {
				t.Fatalf("%s position rows left = %d", c.id, n)
			}
		}
		hl := f.env.strat("pc-hl")
		if eth := hl.PaperFunding.Coins["ETH"]; eth == nil || eth.Anchor.SignedQty != 0 {
			t.Fatalf("the funding anchor must be flat after the close: %+v", eth)
		}
		if alerts := hl.PaperFunding.drainAlerts(); len(alerts) != 0 {
			t.Fatalf("the close must go through the funding helper: %+v", alerts)
		}
		if n := pcCount(t, f.env, "pc-live", "SELECT COUNT(*) FROM positions WHERE strategy_id = ?"); n != 1 {
			t.Fatalf("the live book must stay untouched: %d rows", n)
		}
	})
}

func TestPaperCloseDashboardAndBreakerReasonUnchanged(t *testing.T) {
	cfg := pcSplitConfig(t)
	f := newPCFixture(t, cfg)
	clock := withPaperFundingClock(t, 1_800_000_000_000)
	pcSeed(t, f, clock)
	f.setMark("ES", 4990)

	tradesTotal := func() int {
		req := httptest.NewRequest(http.MethodGet, "/api/strategies/pc-fut/trades", nil)
		w := httptest.NewRecorder()
		f.ss.handleAPIStrategyTrades(w, req, "pc-fut")
		var body struct {
			Total int `json:"total"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		return body.Total
	}
	confirm := func(params string) (int, uiConfirmResponse) {
		w := mutationPost(f.ss, f.ss.handleAPIConfirm, "/api/confirm", fmt.Sprintf(`{"action":"paper-close","strategy_id":"pc-fut","params":%s}`, params), nil)
		var resp uiConfirmResponse
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		return w.Code, resp
	}
	apply := func(nonce, params string) *httptest.ResponseRecorder {
		h := func(w http.ResponseWriter, r *http.Request) { f.ss.handleAPIStrategyPaperClose(w, r, "pc-fut") }
		return mutationPost(f.ss, h, "/api/strategies/pc-fut/paper-close", fmt.Sprintf(`{"nonce":%q,"params":%s}`, nonce, params), nil)
	}

	code, c := confirm(`{"symbol":"ES"}`)
	if code != http.StatusOK || c.ConfirmPhrase != "pc-fut" || !strings.Contains(c.Description, "ES long 1") {
		t.Fatalf("confirm = %d %+v", code, c)
	}
	f.env.mu.Lock()
	f.env.strat("pc-fut").Positions["ES"].Quantity = 2
	f.env.mu.Unlock()
	f.requireUnchanged("book change between confirm and apply", "pc-fut", func() string {
		w := apply(c.Nonce, `{"symbol":"ES"}`)
		return fmt.Sprintf("%d %s", w.Code, w.Body.String())
	}, "409", "changed while you were confirming")
	f.env.mu.Lock()
	f.env.strat("pc-fut").Positions["ES"].Quantity = 1
	f.env.mu.Unlock()

	before := tradesTotal()
	_, c = confirm(`{"symbol":"ES"}`)
	if w := apply(c.Nonce, `{}`); w.Code != http.StatusForbidden {
		t.Fatalf("a nonce bound to other params must be refused: %d %s", w.Code, w.Body.String())
	}
	_, c = confirm(`{"symbol":"ES"}`)
	w := apply(c.Nonce, `{"symbol":"ES"}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "realized PnL $-500.00") {
		t.Fatalf("dashboard paper-close = %d %s", w.Code, w.Body.String())
	}
	if got := tradesTotal(); got != before+1 {
		t.Fatalf("the dashboard trade history must show the close at once: total %d -> %d", before, got)
	}

	f.env.mu.Lock()
	spot := f.env.strat("pc-spot")
	forceCloseAllPositions(spot, &cfg.Strategies[0], map[string]float64{"BTC/USDT": 51000}, nil)
	f.env.mu.Unlock()
	last := spot.TradeHistory[len(spot.TradeHistory)-1]
	if !strings.HasPrefix(last.Details, "Circuit breaker close long, PnL: $100.00 (model-only reconciliation adjustment; no exchange fill)") {
		t.Fatalf("the breaker close details must stay unchanged: %q", last.Details)
	}
	if spot.ClosedPositions[len(spot.ClosedPositions)-1].CloseReason != "circuit_breaker" {
		t.Fatalf("the breaker close reason must stay circuit_breaker: %+v", spot.ClosedPositions)
	}
}

func pcRemoveFixture(t *testing.T) (*pcFixture, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(minimalConfigJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		DBFile:          filepath.Join(dir, "live.db"),
		PaperDBFile:     filepath.Join(dir, "paper.db"),
		IntervalSeconds: 600,
		Strategies: []StrategyConfig{
			{ID: "sma-btc", Type: "spot", Platform: "binanceus", Args: []string{"sma_crossover", "BTC/USDT", "1h"}, Capital: 1000, Paused: true},
			{ID: "hl-momentum-eth", Type: "perps", Platform: "hyperliquid", Args: []string{"momentum", "ETH", "1h", "--mode=paper"}, Capital: 100, Leverage: 1, Paused: true},
		},
	}
	f := newPCFixture(t, cfg)
	f.ss.configPath = path
	f.env.mu.Lock()
	s := f.env.strat("sma-btc")
	s.Cash = 500
	s.Positions["BTC/USDT"] = &Position{Symbol: "BTC/USDT", TradePositionID: "rm-btc", Quantity: 0.01, AvgCost: 50000, Side: "long", OpenedAt: time.Now().UTC()}
	f.env.mu.Unlock()
	f.env.saveAll()
	f.setMark("BTC/USDT", 51000)
	return f, path
}

func (f *pcFixture) setRoster(id string, edit func(*StrategyConfig)) {
	f.ss.strategiesMu.Lock()
	defer f.ss.strategiesMu.Unlock()
	for i := range f.ss.strategies {
		if f.ss.strategies[i].ID == id {
			edit(&f.ss.strategies[i])
		}
	}
}

func TestRemoveStrategyRequiresPausedFlatAndUnqueued(t *testing.T) {
	t.Run("an unpaused flat paper HL perps strategy is refused on Discord before any prompt", func(t *testing.T) {
		f, path := pcRemoveFixture(t)
		f.setRoster("hl-momentum-eth", func(sc *StrategyConfig) { sc.Paused = false })
		raw, _ := os.ReadFile(path)
		prompts := f.promptCount()
		msg := f.run("remove-strategy", pcOwner, "", pcOpts("id", "hl-momentum-eth"), "", nil)
		if !strings.Contains(msg, "Refused") || !strings.Contains(msg, "not paused") || !strings.Contains(msg, "strategies.hl-momentum-eth.paused true") {
			t.Fatalf("an unpaused remove must refuse and name the pause step: %q", msg)
		}
		if f.promptCount() != prompts || f.restarts.Load() != 0 {
			t.Fatal("a refused remove must send no prompt and start no restart")
		}
		if now, _ := os.ReadFile(path); string(now) != string(raw) {
			t.Fatal("a refused remove must leave the config bytes unchanged")
		}
	})

	t.Run("dashboard refuses an unpaused flat paper spot strategy at preview and at apply", func(t *testing.T) {
		f, path := pcRemoveFixture(t)
		f.env.mu.Lock()
		delete(f.env.strat("sma-btc").Positions, "BTC/USDT")
		f.env.mu.Unlock()
		f.setRoster("sma-btc", func(sc *StrategyConfig) { sc.Paused = false })
		w := mutationPost(f.ss, f.ss.handleAPIConfirm, "/api/confirm", `{"action":"remove-strategy","strategy_id":"sma-btc","params":{"restart":false}}`, nil)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "not paused") {
			t.Fatalf("preview of an unpaused remove must refuse: %d %s", w.Code, w.Body.String())
		}
		f.setRoster("sma-btc", func(sc *StrategyConfig) { sc.Paused = true })
		c := structuralConfirm(t, f.ss, "remove-strategy", "sma-btc", `{"restart":false}`)
		f.setRoster("sma-btc", func(sc *StrategyConfig) { sc.Paused = false })
		raw, _ := os.ReadFile(path)
		h := func(w http.ResponseWriter, r *http.Request) {
			f.ss.handleAPIStrategyStructural(w, r, "sma-btc", "remove-strategy")
		}
		w = mutationPost(f.ss, h, "/api/strategies/sma-btc/remove-strategy", fmt.Sprintf(`{"nonce":%q,"params":{"restart":false}}`, c.Nonce), nil)
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "not paused") {
			t.Fatalf("apply after a resume must refuse: %d %s", w.Code, w.Body.String())
		}
		if now, _ := os.ReadFile(path); string(now) != string(raw) {
			t.Fatal("the refused apply must leave the config bytes unchanged")
		}
	})

	t.Run("a paused replay mirror is refused because pause does not hold replayed opens", func(t *testing.T) {
		f, path := pcRemoveFixture(t)
		f.setRoster("hl-momentum-eth", func(sc *StrategyConfig) {
			sc.ReplaySharing = ReplaySharingLiveMirror
			sc.ReplaySourceID = "hl-live-src"
		})
		raw, _ := os.ReadFile(path)
		msg := f.run("remove-strategy", pcOwner, "", pcOpts("id", "hl-momentum-eth"), "", nil)
		if !strings.Contains(msg, "replay mirror of hl-live-src") {
			t.Fatalf("a replay mirror remove must refuse: %q", msg)
		}
		if now, _ := os.ReadFile(path); string(now) != string(raw) {
			t.Fatal("a refused remove must leave the config bytes unchanged")
		}
	})

	t.Run("a queued manual action is refused because it can still open a position", func(t *testing.T) {
		f, path := pcRemoveFixture(t)
		if err := f.env.store.InsertPendingManualAction(PendingManualAction{StrategyID: "hl-momentum-eth", Action: "open", Symbol: "ETH", Side: "buy", Quantity: 0.1}); err != nil {
			t.Fatalf("queue manual action: %v", err)
		}
		raw, _ := os.ReadFile(path)
		msg := f.run("remove-strategy", pcOwner, "", pcOpts("id", "hl-momentum-eth"), "", nil)
		if !strings.Contains(msg, "queued rows") || !strings.Contains(msg, "manual open ETH") {
			t.Fatalf("a remove with a queued open must refuse: %q", msg)
		}
		if now, _ := os.ReadFile(path); string(now) != string(raw) {
			t.Fatal("a refused remove must leave the config bytes unchanged")
		}
	})
}

func TestRemoveStrategyGuardIntegration(t *testing.T) {
	t.Run("refuses on both surfaces while rows exist, then removes after a flatten", func(t *testing.T) {
		f, path := pcRemoveFixture(t)
		ids := configStrategyIDs(t, path)
		prompts := f.promptCount()
		msg := f.run("remove-strategy", pcOwner, "", pcOpts("id", "sma-btc"), "", nil)
		if !strings.Contains(msg, "Refused") || !strings.Contains(msg, "BTC/USDT") || !strings.Contains(msg, "/go-trader-paper-close sma-btc") {
			t.Fatalf("Discord remove must refuse naming the symbol and the close command: %q", msg)
		}
		if f.promptCount() != prompts {
			t.Fatal("a refused remove must not send a confirm prompt")
		}
		w := mutationPost(f.ss, f.ss.handleAPIConfirm, "/api/confirm", `{"action":"remove-strategy","strategy_id":"sma-btc","params":{}}`, nil)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "BTC/USDT") {
			t.Fatalf("dashboard remove preview must refuse: %d %s", w.Code, w.Body.String())
		}

		f.env.mu.Lock()
		f.env.strat("sma-btc").Positions["BTC/USDT"].Quantity = 0
		f.env.mu.Unlock()
		msg = f.run("remove-strategy", pcOwner, "", pcOpts("id", "sma-btc"), "", nil)
		if !strings.Contains(msg, "Refused") {
			t.Fatalf("a zero-quantity row must still refuse removal: %q", msg)
		}
		f.env.mu.Lock()
		f.env.strat("sma-btc").Positions["BTC/USDT"].Quantity = 0.01
		f.env.mu.Unlock()

		if got := configStrategyIDs(t, path); strings.Join(got, ",") != strings.Join(ids, ",") {
			t.Fatalf("refusals must not change the config: %v", got)
		}

		msg = f.run(paperCloseCommandName, pcOwner, "", pcOpts("strategy", "sma-btc"), "confirm", nil)
		if !strings.Contains(msg, "realized PnL $10.00") {
			t.Fatalf("paper-close = %q", msg)
		}
		msg = f.run("remove-strategy", pcOwner, "", pcOpts("id", "sma-btc"), "confirm", nil)
		if !strings.Contains(msg, "Removed strategy `sma-btc`") {
			t.Fatalf("remove after flatten = %q", msg)
		}
		deadline := time.Now().Add(5 * time.Second)
		for f.restarts.Load() != 1 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if f.restarts.Load() != 1 {
			t.Fatalf("remove-strategy must start one restart, got %d", f.restarts.Load())
		}
		if got := configStrategyIDs(t, path); len(got) != 1 || got[0] != "hl-momentum-eth" {
			t.Fatalf("config after remove = %v", got)
		}

		trades := pcCount(t, f.env, "sma-btc", "SELECT COUNT(*) FROM trades WHERE strategy_id = ?")
		f.env.mu.Lock()
		delete(f.env.state.Strategies, "sma-btc")
		f.env.mu.Unlock()
		f.env.saveAll()
		db := f.env.fileFor("hl-momentum-eth")
		var positions, kept int
		_ = db.db.QueryRow("SELECT COUNT(*) FROM positions WHERE strategy_id = 'sma-btc'").Scan(&positions)
		_ = db.db.QueryRow("SELECT COUNT(*) FROM trades WHERE strategy_id = 'sma-btc'").Scan(&kept)
		if positions != 0 || trades == 0 || kept != trades {
			t.Fatalf("after the prune save: positions=%d trades %d -> %d", positions, trades, kept)
		}
	})

	t.Run("a position opened during the Discord confirm makes the write refuse", func(t *testing.T) {
		f, path := pcRemoveFixture(t)
		f.env.mu.Lock()
		delete(f.env.strat("sma-btc").Positions, "BTC/USDT")
		f.env.mu.Unlock()
		raw, _ := os.ReadFile(path)
		msg := f.run("remove-strategy", pcOwner, "", pcOpts("id", "sma-btc"), "confirm", func() {
			f.env.mu.Lock()
			f.env.strat("sma-btc").Positions["BTC/USDT"] = &Position{Symbol: "BTC/USDT", Quantity: 0.01, AvgCost: 50000, Side: "long"}
			f.env.mu.Unlock()
		})
		if !strings.Contains(msg, "remove-strategy failed") || !strings.Contains(msg, "BTC/USDT") {
			t.Fatalf("the post-confirm write must refuse: %q", msg)
		}
		if now, _ := os.ReadFile(path); string(now) != string(raw) {
			t.Fatal("the refused write must leave the config bytes unchanged")
		}
		if f.restarts.Load() != 0 {
			t.Fatal("a refused remove must not restart")
		}
	})

	t.Run("dashboard apply re-checks after the confirm", func(t *testing.T) {
		f, path := pcRemoveFixture(t)
		f.env.mu.Lock()
		delete(f.env.strat("sma-btc").Positions, "BTC/USDT")
		f.env.mu.Unlock()
		c := structuralConfirm(t, f.ss, "remove-strategy", "sma-btc", `{}`)
		if !strings.Contains(c.Description, removeStrategyHistoryNote) {
			t.Fatalf("preview text = %q", c.Description)
		}
		f.env.mu.Lock()
		f.env.strat("sma-btc").OptionPositions["leg-1"] = &OptionPosition{ID: "leg-1", Quantity: 0}
		f.env.mu.Unlock()
		raw, _ := os.ReadFile(path)
		h := func(w http.ResponseWriter, r *http.Request) {
			f.ss.handleAPIStrategyStructural(w, r, "sma-btc", "remove-strategy")
		}
		w := mutationPost(f.ss, h, "/api/strategies/sma-btc/remove-strategy", fmt.Sprintf(`{"nonce":%q,"params":{}}`, c.Nonce), nil)
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "leg-1") {
			t.Fatalf("apply must refuse: %d %s", w.Code, w.Body.String())
		}
		if now, _ := os.ReadFile(path); string(now) != string(raw) {
			t.Fatal("the refused apply must leave the config bytes unchanged")
		}
	})

	t.Run("advice names the close path for each kind of strategy", func(t *testing.T) {
		cases := []struct {
			sc   StrategyConfig
			want string
		}{
			{StrategyConfig{ID: "p", Type: "spot", Platform: "binanceus", Args: []string{"sma", "BTC/USDT", "1h"}}, "/go-trader-paper-close p"},
			{hlLiveManual("m", 100), "go-trader manual-close m"},
			{hlLivePerps("h", "ETH", 100), "go-trader force-close h"},
			{StrategyConfig{ID: "o", Type: "spot", Platform: "okx", Args: []string{"sma", "BTC", "1h", "--mode=live"}}, "No close command exists for platform=okx type=spot"},
		}
		for _, c := range cases {
			if got := removeStrategyCloseAdvice(c.sc, nil); !strings.Contains(got, c.want) {
				t.Fatalf("%s advice = %q, want %q", c.sc.ID, got, c.want)
			}
		}
		if got := removeStrategyCloseAdvice(cases[0].sc, &StrategyState{Quote: &quoteOwnerState{}}); !strings.Contains(got, inventoryQuoteExitCloseName) {
			t.Fatalf("quote-owned advice = %q", got)
		}
	})
}

func TestPaperCloseRealMarkFetchersAndRestingLimitRefusal(t *testing.T) {
	var mu sync.Mutex
	hlMids := map[string]string{"ETH": "2100", "SOL": "150"}
	okxLast := map[string]string{"BTC-USDT": "60000"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/info":
			_ = json.NewEncoder(w).Encode(hlMids)
		case r.URL.Path == "/api/v5/market/tickers" && r.URL.Query().Get("instType") == "SPOT":
			var data []map[string]string
			for inst, last := range okxLast {
				data = append(data, map[string]string{"instId": inst, "last": last})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": "0", "msg": "", "data": data})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	origHL, origOKX := hlMainnetURL, okxMainnetURL
	hlMainnetURL, okxMainnetURL = srv.URL, srv.URL
	defer func() { hlMainnetURL, okxMainnetURL = origHL, origOKX }()

	cfg := &Config{
		DBFile:          filepath.Join(t.TempDir(), "state.db"),
		IntervalSeconds: 600,
		Strategies: []StrategyConfig{
			{ID: "okx-sp", Type: "spot", Platform: "okx", Args: []string{"sma", "BTC", "1h", "--mode=paper", "--inst-type=spot"}, Capital: 10000},
			{ID: "hl-p", Type: "perps", Platform: "hyperliquid", Args: []string{"sma", "ETH", "1h", "--mode=paper"}, Capital: 10000, Leverage: 1},
			{ID: "man-p", Type: "manual", Platform: "hyperliquid", Symbol: "SOL", Timeframe: "1h", Args: []string{"hold", "SOL", "1h", "--mode=paper"}, Capital: 10000, Leverage: 1},
		},
	}
	f := newPCFixture(t, cfg)
	f.ss.paperCloseMarks = nil
	now := time.Now().UTC()
	f.env.mu.Lock()
	f.env.strat("okx-sp").Positions["BTC"] = &Position{Symbol: "BTC", TradePositionID: "okx-btc", Quantity: 0.1, AvgCost: 50000, Side: "long", OpenedAt: now}
	f.env.strat("hl-p").Positions["ETH"] = &Position{Symbol: "ETH", TradePositionID: "hl-eth", Quantity: 1, AvgCost: 2000, Side: "short", Multiplier: 1, OpenedAt: now}
	f.env.strat("man-p").Positions["SOL"] = &Position{Symbol: "SOL", TradePositionID: "man-sol", Quantity: 2, AvgCost: 140, Side: "long", Multiplier: 1, OpenedAt: now}
	f.env.mu.Unlock()
	f.env.saveAll()

	for _, bad := range []string{"0", "NaN"} {
		mu.Lock()
		hlMids["ETH"] = bad
		mu.Unlock()
		f.requireUnchanged("served HL mid "+bad, "hl-p", func() string {
			return f.run(paperCloseCommandName, pcOwner, "", pcOpts("strategy", "hl-p"), "", nil)
		}, "no usable mark", "ETH")
	}
	mu.Lock()
	hlMids["ETH"] = "2100"
	delete(okxLast, "BTC-USDT")
	mu.Unlock()
	f.requireUnchanged("missing OKX coin", "okx-sp", func() string {
		return f.run(paperCloseCommandName, pcOwner, "", pcOpts("strategy", "okx-sp"), "", nil)
	}, "BTC: no mark")
	mu.Lock()
	okxLast["BTC-USD"] = "59000"
	okxLast["BTC-USDT"] = "60000"
	mu.Unlock()

	msg := f.run(paperCloseCommandName, pcOwner, "", pcOpts("strategy", "okx-sp"), "confirm", nil)
	if !strings.Contains(msg, "@ 60000 (okx spot ticker BTC-USDT), realized PnL $1000.00") {
		t.Fatalf("OKX spot close = %q", msg)
	}
	msg = f.run(paperCloseCommandName, pcOwner, "", pcOpts("strategy", "hl-p", "symbol", "ETH"), "confirm", nil)
	if !strings.Contains(msg, "@ 2100 (hyperliquid allMids), realized PnL $-100.00") {
		t.Fatalf("HL perps close = %q", msg)
	}

	if _, err := f.env.store.InsertPendingLimitOrder(PendingLimitOrder{StrategyID: "man-p", Symbol: "SOL", Side: "buy", OrderOID: 7, LimitPrice: 130, OrderSize: 1, TIF: "Gtc"}); err != nil {
		t.Fatalf("insert resting limit row: %v", err)
	}
	f.requireUnchanged("resting limit row", "man-p", func() string {
		return f.run(paperCloseCommandName, pcOwner, "", pcOpts("strategy", "man-p"), "confirm", nil)
	}, "resting limit order", "SOL")
}

func (m storageIdentityMap) mustRole(t *testing.T, id string) storageRole {
	t.Helper()
	ident, ok := m.storageFor(id)
	if !ok {
		t.Fatalf("no storage owner for %s", id)
	}
	return ident.Role
}

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

const paperResearchMacroSHA = "1111111111111111111111111111111111111111111111111111111111111111"

func paperResearchNames() []string {
	names := make([]string, 0, len(addStrategyResearchCandidates))
	for name := range addStrategyResearchCandidates {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func paperResearchParams(name string) map[string]interface{} {
	if len(addStrategyResearchCandidates[name].RequiredParams) == 0 {
		return nil
	}
	return map[string]interface{}{"calendar_path": "/var/lib/go-trader/macro/calendar.json", "calendar_sha256": paperResearchMacroSHA,
		"quote_path": "/var/lib/go-trader/macro/quotes.json", "quote_sha256": strings.Repeat("2", 64)}
}

func paperResearchServer(t *testing.T, root map[string]interface{}, strategies ...map[string]interface{}) (*StatusServer, string) {
	t.Helper()
	dir := t.TempDir()
	base := map[string]interface{}{
		"config_version":   CurrentConfigVersion,
		"interval_seconds": 60,
		"log_dir":          filepath.Join(dir, "logs"),
		"db_file":          filepath.Join(dir, "state.db"),
		"strategies":       strategies,
	}
	for k, v := range root {
		base[k] = v
	}
	data, err := json.MarshalIndent(base, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfigForProbe(path)
	if err != nil {
		t.Fatalf("fixture config must load: %v", err)
	}
	ss := NewStatusServer(NewAppState(), &sync.RWMutex{}, "", cfg.Strategies, nil)
	ss.SetConfigContext(path, cfg)
	ss.restartFn = func() error { return nil }
	return ss, path
}

func paperResearchMomentum() map[string]interface{} {
	return map[string]interface{}{"id": "hl-momentum-eth", "type": "perps", "platform": "hyperliquid", "script": hyperliquidCheckScript,
		"args": []string{"momentum", "ETH", "1h", "--mode=paper"}, "capital": 100, "max_drawdown_pct": 10, "leverage": 1, "direction": "long", "margin_mode": "isolated"}
}

func fileSHA(t *testing.T, path string) string {
	t.Helper()
	sum := sha256.Sum256(mustReadFile(t, path))
	return hex.EncodeToString(sum[:])
}

type paperResearchProjection struct {
	Args               []string
	Type               string
	Platform           string
	Script             string
	Open               StrategyRef
	Close              *StrategyRef
	StopLossATRMult    float64
	Leverage           float64
	Direction          string
	IntervalSeconds    int
	MarginMode         string
	ClosedBarDecisions bool
	Capital            float64
	Quote              *QuoteConfig
	StopLossPct        float64
}

func projectStrategy(sc StrategyConfig) paperResearchProjection {
	p := paperResearchProjection{Args: sc.Args, Type: sc.Type, Platform: sc.Platform, Script: sc.Script, Open: sc.OpenStrategy,
		Close: sc.CloseStrategy, Leverage: sc.Leverage, Direction: sc.Direction, IntervalSeconds: sc.IntervalSeconds,
		MarginMode: sc.MarginMode, ClosedBarDecisions: sc.ClosedBarDecisions, Capital: sc.Capital, Quote: sc.Quote}
	if sc.StopLossATRMult != nil {
		p.StopLossATRMult = *sc.StopLossATRMult
	}
	if sc.StopLossPct != nil {
		p.StopLossPct = *sc.StopLossPct
	}
	return p
}

func projectionText(p paperResearchProjection) string {
	blob, _ := json.Marshal(p)
	return string(blob)
}

func loadedStrategy(t *testing.T, path, id string) StrategyConfig {
	t.Helper()
	cfg, err := LoadConfigForProbe(path)
	if err != nil {
		t.Fatalf("written config must validate: %v", err)
	}
	for _, sc := range cfg.Strategies {
		if sc.ID == id {
			return sc
		}
	}
	t.Fatalf("strategy %s missing from %s", id, path)
	return StrategyConfig{}
}

type paperResearchDiscord struct {
	mu        sync.Mutex
	followups []string
	dms       []string
	d         *DiscordNotifier
	reply     string
}

func (f *paperResearchDiscord) ChannelMessageSend(channelID string, content string, _ ...discordgo.RequestOption) (*discordgo.Message, error) {
	f.mu.Lock()
	f.dms = append(f.dms, content)
	reply := f.reply
	f.mu.Unlock()
	go func() {
		for k := 0; k < 200; k++ {
			f.d.mu.Lock()
			if len(f.d.dmHandlers) > 0 {
				h := f.d.dmHandlers[0]
				f.d.dmHandlers = f.d.dmHandlers[1:]
				f.d.mu.Unlock()
				h.ch <- reply
				return
			}
			f.d.mu.Unlock()
			time.Sleep(5 * time.Millisecond)
		}
	}()
	return &discordgo.Message{}, nil
}

func (f *paperResearchDiscord) UserChannelCreate(string, ...discordgo.RequestOption) (*discordgo.Channel, error) {
	return &discordgo.Channel{ID: "dm"}, nil
}

func (f *paperResearchDiscord) InteractionRespond(*discordgo.Interaction, *discordgo.InteractionResponse, ...discordgo.RequestOption) error {
	return nil
}

func (f *paperResearchDiscord) FollowupMessageCreate(_ *discordgo.Interaction, _ bool, data *discordgo.WebhookParams, _ ...discordgo.RequestOption) (*discordgo.Message, error) {
	f.mu.Lock()
	f.followups = append(f.followups, data.Content)
	f.mu.Unlock()
	return &discordgo.Message{}, nil
}

func (f *paperResearchDiscord) ApplicationCommandBulkOverwrite(string, string, []*discordgo.ApplicationCommand, ...discordgo.RequestOption) ([]*discordgo.ApplicationCommand, error) {
	return nil, nil
}

func (f *paperResearchDiscord) last() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.followups) == 0 {
		return ""
	}
	return f.followups[len(f.followups)-1]
}

func newPaperResearchDiscord(ss *StatusServer, reply string) (*DiscordNotifier, *paperResearchDiscord) {
	fake := &paperResearchDiscord{reply: reply}
	d := &DiscordNotifier{transport: fake, ownerID: "owner", ss: ss}
	fake.d = d
	return d, fake
}

func discordInteraction(name string, opts ...*discordgo.ApplicationCommandInteractionDataOption) (*discordgo.InteractionCreate, discordgo.ApplicationCommandInteractionData) {
	data := discordgo.ApplicationCommandInteractionData{Name: name, Options: opts}
	return &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{Type: discordgo.InteractionApplicationCommand, Data: data, User: &discordgo.User{ID: "owner"}}}, data
}

func stringOpt(name, v string) *discordgo.ApplicationCommandInteractionDataOption {
	return &discordgo.ApplicationCommandInteractionDataOption{Name: name, Type: discordgo.ApplicationCommandOptionString, Value: v}
}

func TestPaperResearchSetupBuildersAgree(t *testing.T) {
	websocket := map[string]interface{}{"market_feed": "websocket"}
	for _, name := range paperResearchNames() {
		t.Run(name, func(t *testing.T) {
			params := paperResearchParams(name)
			asset := researchFixtureAsset(name)
			wantID, _, err := buildAddStrategyEntryWithParams(name, "hyperliquid", asset, params)
			if err != nil {
				t.Fatal(err)
			}
			webParams := map[string]interface{}{"name": name, "platform": "hyperliquid", "asset": asset}
			if params != nil {
				webParams["open_params"] = params
			}
			pb, _ := json.Marshal(webParams)
			ss, webPath := paperResearchServer(t, websocket, paperResearchMomentum())
			confirm := structuralConfirm(t, ss, "add-strategy", "", string(pb))
			if !strings.Contains(confirm.Description, "research-only") {
				t.Fatalf("the confirmation must label the research strategy: %s", confirm.Description)
			}
			w := mutationPost(ss, ss.handleAPIAddStrategy, "/api/config/add-strategy", fmt.Sprintf(`{"nonce":%q,"params":%s}`, confirm.Nonce, pb), nil)
			if w.Code != http.StatusOK {
				t.Fatalf("web add status %d: %s", w.Code, w.Body.String())
			}
			web := loadedStrategy(t, webPath, wantID)

			dss, discordPath := paperResearchServer(t, websocket, paperResearchMomentum())
			d, fake := newPaperResearchDiscord(dss, "confirm")
			opts := []*discordgo.ApplicationCommandInteractionDataOption{stringOpt("name", name), stringOpt("platform", "hyperliquid"), stringOpt("asset", asset)}
			if params != nil {
				raw, _ := json.Marshal(params)
				opts = append(opts, stringOpt("open_params", string(raw)))
			}
			i, _ := discordInteraction("add-strategy", opts...)
			d.handleAddStrategy(fake, i, opts)
			if !strings.Contains(fake.last(), "Added strategy") {
				t.Fatalf("discord add: %q", fake.last())
			}
			discord := loadedStrategy(t, discordPath, wantID)

			initOpts := InitOptions{Assets: []string{asset}, EnableSpot: true, SpotStrategies: []string{"sma_crossover"}, SpotCapital: 1000, SpotDrawdown: 5,
				ResearchStrategies: []string{name}}
			if params != nil {
				initOpts.ResearchOpenParams = map[string]map[string]interface{}{name: params}
			}
			cfg := generateConfig(initOpts)
			if err := validateInitResearchConfig(cfg, initOpts); err != nil {
				t.Fatalf("wizard output must validate: %v", err)
			}
			if cfg.MarketFeed != marketFeedWebsocket {
				t.Fatalf("wizard research output needs a sealed feed, got %q", cfg.MarketFeed)
			}
			wizardPath := filepath.Join(t.TempDir(), "config.json")
			blob, _ := json.MarshalIndent(cfg, "", "  ")
			if err := os.WriteFile(wizardPath, blob, 0o600); err != nil {
				t.Fatal(err)
			}
			wizard := loadedStrategy(t, wizardPath, wantID)

			want := projectionText(projectStrategy(web))
			for label, got := range map[string]StrategyConfig{"discord": discord, "wizard": wizard} {
				if g := projectionText(projectStrategy(got)); g != want {
					t.Fatalf("%s entry differs from the web entry:\n%s\n%s", label, g, want)
				}
			}
			if rawEdgeGateMode(web.Args).Kind != edgeGateModePaper || isLiveArgs(web.Args) {
				t.Fatalf("research entries are explicit paper: %v", web.Args)
			}
			livePerps := InitOptions{Assets: []string{asset}, EnablePerps: true, PerpsMode: "live", PerpsStrategies: []string{"momentum"}, PerpsLeverage: 1,
				ResearchStrategies: []string{name}, ResearchOpenParams: initOpts.ResearchOpenParams}
			for _, sc := range generateConfig(livePerps).Strategies {
				if sc.ID == wantID && rawEdgeGateMode(sc.Args).Kind != edgeGateModePaper {
					t.Fatalf("a live perps wizard run must still write explicit paper for research: %v", sc.Args)
				}
			}
		})
	}
}

func TestPaperResearchSetupFailuresPreserveConfig(t *testing.T) {
	websocket := map[string]interface{}{"market_feed": "websocket"}
	check := func(t *testing.T, path, before, label string) {
		t.Helper()
		if got := fileSHA(t, path); got != before {
			t.Fatalf("%s changed the deployed config", label)
		}
	}
	t.Run("rest_feed_is_refused_and_names_the_setting", func(t *testing.T) {
		ss, path := paperResearchServer(t, nil, paperResearchMomentum())
		before := fileSHA(t, path)
		body := `{"action":"add-strategy","strategy_id":"","params":{"name":"bitcoin_lead_lag","platform":"hyperliquid","asset":"ETH"}}`
		w := mutationPost(ss, ss.handleAPIConfirm, "/api/confirm", body, nil)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `market_feed`) {
			t.Fatalf("confirm on a REST feed: %d %s", w.Code, w.Body.String())
		}
		d, fake := newPaperResearchDiscord(ss, "confirm")
		opts := []*discordgo.ApplicationCommandInteractionDataOption{stringOpt("name", "bitcoin_lead_lag"), stringOpt("platform", "hyperliquid"), stringOpt("asset", "ETH")}
		i, _ := discordInteraction("add-strategy", opts...)
		d.handleAddStrategy(fake, i, opts)
		if !strings.Contains(fake.last(), "market_feed") {
			t.Fatalf("discord on a REST feed: %q", fake.last())
		}
		check(t, path, before, "a REST-feed add")
	})
	t.Run("wrong_platform_missing_params_collision_and_stale_confirmation", func(t *testing.T) {
		ss, path := paperResearchServer(t, websocket, paperResearchMomentum())
		before := fileSHA(t, path)
		for _, params := range []string{
			`{"name":"order_book_imbalance","platform":"binanceus","asset":"BTC"}`,
			`{"name":"macro_event_breakout","platform":"hyperliquid","asset":"BTC"}`,
			`{"name":"inventory_market_making","platform":"hyperliquid","asset":"BTC"}`,
			`{"name":"momentum","platform":"hyperliquid","asset":"BTC","open_params":{"x":1}}`,
		} {
			body := fmt.Sprintf(`{"action":"add-strategy","strategy_id":"","params":%s}`, params)
			if w := mutationPost(ss, ss.handleAPIConfirm, "/api/confirm", body, nil); w.Code != http.StatusBadRequest {
				t.Fatalf("%s: confirm status %d %s", params, w.Code, w.Body.String())
			}
		}
		check(t, path, before, "refused confirmations")
		params := `{"name":"order_book_imbalance","platform":"hyperliquid","asset":"BTC"}`
		first := structuralConfirm(t, ss, "add-strategy", "", params)
		second := structuralConfirm(t, ss, "add-strategy", "", params)
		if w := mutationPost(ss, ss.handleAPIAddStrategy, "/api/config/add-strategy", fmt.Sprintf(`{"nonce":%q,"params":%s}`, first.Nonce, params), nil); w.Code != http.StatusOK {
			t.Fatalf("first add: %d %s", w.Code, w.Body.String())
		}
		afterFirst := fileSHA(t, path)
		if w := mutationPost(ss, ss.handleAPIAddStrategy, "/api/config/add-strategy", fmt.Sprintf(`{"nonce":%q,"params":%s}`, second.Nonce, params), nil); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "already exists") {
			t.Fatalf("stale confirmation after the disk changed: %d %s", w.Code, w.Body.String())
		}
		check(t, path, afterFirst, "a stale confirmation")
	})
}

func paperResearchQuoteFragment(mutate func(sc map[string]interface{})) []byte {
	sc := researchQuoteOwnerFixture("--mode=paper")
	if mutate != nil {
		mutate(sc)
	}
	blob, _ := json.Marshal(map[string]interface{}{"role": quoteOwnerSetupRole, "strategies": []interface{}{sc}})
	return blob
}

type paperResearchCDN struct {
	body   []byte
	status int
	hits   atomic.Int32
}

func (c *paperResearchCDN) RoundTrip(r *http.Request) (*http.Response, error) {
	c.hits.Add(1)
	if r.URL.Host != "cdn.discordapp.com" {
		return nil, fmt.Errorf("unexpected host %s", r.URL.Host)
	}
	status := c.status
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(string(c.body))), Header: http.Header{}, Request: r}, nil
}

func TestPaperResearchQuoteOwnerSetup(t *testing.T) {
	websocket := map[string]interface{}{"market_feed": "websocket"}
	t.Run("web_upload_appends_one_held_owner_and_binds_the_digest", func(t *testing.T) {
		ss, path := paperResearchServer(t, websocket, paperResearchMomentum())
		before := fileSHA(t, path)
		frag := paperResearchQuoteFragment(nil)
		params := fmt.Sprintf(`{"fragment":%s}`, frag)
		confirm := structuralConfirm(t, ss, quoteOwnerSetupAction, "", params)
		if !strings.Contains(confirm.Description, quoteEngineHoldReason) || !strings.Contains(confirm.Description, "hl-invmm-btc") || !strings.Contains(confirm.Description, "sha256") {
			t.Fatalf("the confirmation must show the change, digest and hold: %s", confirm.Description)
		}
		changed := strings.Replace(params, `"max_order_qty":0.01`, `"max_order_qty":0.02`, 1)
		if w := mutationPost(ss, ss.handleAPIAddQuoteOwner, "/api/config/add-quote-owner", fmt.Sprintf(`{"nonce":%q,"params":%s}`, confirm.Nonce, changed), nil); w.Code != http.StatusForbidden {
			t.Fatalf("a changed fragment must not consume the confirmation: %d %s", w.Code, w.Body.String())
		}
		if fileSHA(t, path) != before {
			t.Fatal("a refused fragment changed the config")
		}
		confirm = structuralConfirm(t, ss, quoteOwnerSetupAction, "", params)
		ss.confirmMu.Lock()
		entry := ss.confirmNonces[confirm.Nonce]
		entry.expires = time.Now().Add(-time.Second)
		ss.confirmNonces[confirm.Nonce] = entry
		ss.confirmMu.Unlock()
		if w := mutationPost(ss, ss.handleAPIAddQuoteOwner, "/api/config/add-quote-owner", fmt.Sprintf(`{"nonce":%q,"params":%s}`, confirm.Nonce, params), nil); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "expired") {
			t.Fatalf("an expired confirmation: %d %s", w.Code, w.Body.String())
		}
		if fileSHA(t, path) != before {
			t.Fatal("an expired confirmation changed the config")
		}
		confirm = structuralConfirm(t, ss, quoteOwnerSetupAction, "", params)
		if w := mutationPost(ss, ss.handleAPIAddQuoteOwner, "/api/config/add-quote-owner", fmt.Sprintf(`{"nonce":%q,"params":%s}`, confirm.Nonce, params), nil); w.Code != http.StatusOK {
			t.Fatalf("add-quote-owner: %d %s", w.Code, w.Body.String())
		}
		cfg, err := LoadConfigForProbe(path)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.quoteLoad != nil || len(cfg.Strategies) != 2 {
			t.Fatalf("the setup must append one owner and no admission: %d strategies, admission %v", len(cfg.Strategies), cfg.quoteLoad != nil)
		}
		owner := loadedStrategy(t, path, "hl-invmm-btc")
		if h, held := engineHoldFor(owner); !held || h.TrackingURL != quoteEngineHoldTrackingURL {
			t.Fatalf("the owner must be engine-held: %+v", h)
		}
		momentum := loadedStrategy(t, path, "hl-momentum-eth")
		if projectionText(projectStrategy(momentum)) != projectionText(projectStrategy(ss.strategies[0])) {
			t.Fatal("the setup changed an existing entry")
		}
		after := fileSHA(t, path)
		ss.strategiesMu.Lock()
		ss.strategies = cfg.Strategies
		ss.strategiesMu.Unlock()
		if w := mutationPost(ss, ss.handleAPIConfirm, "/api/confirm", fmt.Sprintf(`{"action":%q,"strategy_id":"","params":%s}`, quoteOwnerSetupAction, params), nil); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "already exists") {
			t.Fatalf("an identity collision: %d %s", w.Code, w.Body.String())
		}
		if fileSHA(t, path) != after {
			t.Fatal("a collision changed the config")
		}
	})
	t.Run("strict_parsing_refuses_every_malformed_fragment", func(t *testing.T) {
		big := make([]byte, quoteOwnerSetupMaxBytes+1)
		for k := range big {
			big[k] = ' '
		}
		cases := map[string][]byte{
			"over the ceiling":     big,
			"duplicate key":        []byte(strings.Replace(string(paperResearchQuoteFragment(nil)), `"role":"quote_owner"`, `"role":"quote_owner","role":"quote_owner"`, 1)),
			"root setting":         []byte(strings.Replace(string(paperResearchQuoteFragment(nil)), `"role":"quote_owner"`, `"role":"quote_owner","market_feed":"shared"`, 1)),
			"role mismatch":        []byte(strings.Replace(string(paperResearchQuoteFragment(nil)), `"role":"quote_owner"`, `"role":"scalar"`, 1)),
			"retired group":        []byte(strings.Replace(string(paperResearchQuoteFragment(nil)), `"role":"quote_owner"`, `"role":"quote_owner","baskets":[]`, 1)),
			"two owners":           []byte(strings.Replace(string(paperResearchQuoteFragment(nil)), `"strategies":[`, `"strategies":[{"id":"x"},`, 1)),
			"unknown strategy key": paperResearchQuoteFragment(func(sc map[string]interface{}) { sc["engine_enabled"] = true }),
			"unknown quote key": paperResearchQuoteFragment(func(sc map[string]interface{}) {
				sc["quote"].(map[string]interface{})["engine"] = true
			}),
			"missing quote": paperResearchQuoteFragment(func(sc map[string]interface{}) { delete(sc, "quote") }),
			"live mode": paperResearchQuoteFragment(func(sc map[string]interface{}) {
				sc["args"] = []string{"inventory_market_making", "BTC", "1m", "--mode=live"}
			}),
			"scalar open":    paperResearchQuoteFragment(func(sc map[string]interface{}) { sc["open_strategy"] = map[string]interface{}{"name": "momentum"} }),
			"trailing value": append(paperResearchQuoteFragment(nil), []byte(" {}")...),
		}
		labels := make([]string, 0, len(cases))
		for k := range cases {
			labels = append(labels, k)
		}
		sort.Strings(labels)
		for _, label := range labels {
			if _, err := parseQuoteOwnerSetup(cases[label]); err == nil {
				t.Fatalf("%s: the fragment must be refused", label)
			}
		}
		ss, path := paperResearchServer(t, websocket, paperResearchMomentum())
		before := fileSHA(t, path)
		for _, mutate := range []func(map[string]interface{}){
			func(sc map[string]interface{}) { delete(sc, "stop_loss_pct") },
			func(sc map[string]interface{}) {
				sc["close_strategy"] = map[string]interface{}{"name": "tiered_tp_pct"}
			},
			func(sc map[string]interface{}) {
				sc["args"] = []string{"inventory_market_making", "ETH", "1m", "--mode=paper"}
			},
			func(sc map[string]interface{}) { sc["platform"] = "okx" },
		} {
			setup, err := parseQuoteOwnerSetup(paperResearchQuoteFragment(mutate))
			if err != nil {
				continue
			}
			if err := ss.mutateConfigRoot(func(root map[string]json.RawMessage) error { return addQuoteOwnerToRoot(root, setup) }); err == nil {
				t.Fatalf("a structurally invalid owner was written: %s", setup.Entry)
			}
		}
		exclusive := paperResearchQuoteFragment(nil)
		ss2, path2 := paperResearchServer(t, websocket, func() map[string]interface{} {
			sc := paperResearchMomentum()
			sc["id"], sc["args"] = "hl-momentum-btc", []string{"momentum", "BTC", "1h", "--mode=paper"}
			return sc
		}())
		before2 := fileSHA(t, path2)
		setup, err := parseQuoteOwnerSetup(exclusive)
		if err != nil {
			t.Fatal(err)
		}
		if err := ss2.mutateConfigRoot(func(root map[string]json.RawMessage) error { return addQuoteOwnerToRoot(root, setup) }); err == nil || !strings.Contains(err.Error(), "exclusive ownership") {
			t.Fatalf("the exclusive-ownership rule must refuse a shared coin: %v", err)
		}
		if fileSHA(t, path) != before || fileSHA(t, path2) != before2 {
			t.Fatal("a refused quote setup changed the deployed config")
		}
	})
	t.Run("discord_attachment_downloads_once_from_the_cdn_and_confirms_the_digest", func(t *testing.T) {
		frag := paperResearchQuoteFragment(nil)
		run := func(t *testing.T, att *discordgo.MessageAttachment, cdn *paperResearchCDN, reply string) (string, string, string) {
			ss, path := paperResearchServer(t, websocket, paperResearchMomentum())
			before := fileSHA(t, path)
			prev := quoteSetupHTTPClient
			quoteSetupHTTPClient = func() *http.Client { return &http.Client{Transport: cdn} }
			defer func() { quoteSetupHTTPClient = prev }()
			d, fake := newPaperResearchDiscord(ss, reply)
			opt := &discordgo.ApplicationCommandInteractionDataOption{Name: "fragment", Type: discordgo.ApplicationCommandOptionAttachment, Value: "att1"}
			i, data := discordInteraction(quoteOwnerSetupAction, opt)
			data.Resolved = &discordgo.ApplicationCommandInteractionDataResolved{Attachments: map[string]*discordgo.MessageAttachment{"att1": att}}
			d.handleAddQuoteOwner(fake, i, data)
			fake.mu.Lock()
			dm := strings.Join(fake.dms, "\n")
			fake.mu.Unlock()
			if fileSHA(t, path) == before {
				return fake.last(), dm, ""
			}
			return fake.last(), dm, path
		}
		good := &discordgo.MessageAttachment{ID: "att1", URL: "https://cdn.discordapp.com/attachments/1/2/quote.json", Size: len(frag), ContentType: "application/json; charset=utf-8"}
		cdn := &paperResearchCDN{body: frag}
		msg, dm, path := run(t, good, cdn, "confirm")
		if path == "" || !strings.Contains(msg, "Added held quote owner") || cdn.hits.Load() != 1 {
			t.Fatalf("a confirmed attachment must append the owner after one download: %q hits %d", msg, cdn.hits.Load())
		}
		setup, _ := parseQuoteOwnerSetup(frag)
		if !strings.Contains(dm, setup.Digest) || !strings.Contains(dm, quoteEngineHoldReason) {
			t.Fatalf("the DM confirmation must show the digest and hold: %s", dm)
		}
		if h, held := engineHoldFor(loadedStrategy(t, path, "hl-invmm-btc")); !held || h.Reason != quoteEngineHoldReason {
			t.Fatal("the Discord owner must be engine-held")
		}
		refusals := map[string]struct {
			att   *discordgo.MessageAttachment
			cdn   *paperResearchCDN
			reply string
		}{
			"declined":      {good, &paperResearchCDN{body: frag}, "no"},
			"foreign host":  {&discordgo.MessageAttachment{ID: "att1", URL: "https://example.com/quote.json", Size: len(frag), ContentType: "application/json"}, &paperResearchCDN{body: frag}, "confirm"},
			"plain http":    {&discordgo.MessageAttachment{ID: "att1", URL: "http://cdn.discordapp.com/a/quote.json", Size: len(frag), ContentType: "application/json"}, &paperResearchCDN{body: frag}, "confirm"},
			"size mismatch": {&discordgo.MessageAttachment{ID: "att1", URL: good.URL, Size: len(frag) + 1, ContentType: "application/json"}, &paperResearchCDN{body: frag}, "confirm"},
			"oversized":     {&discordgo.MessageAttachment{ID: "att1", URL: good.URL, Size: quoteOwnerSetupMaxBytes + 1, ContentType: "application/json"}, &paperResearchCDN{body: frag}, "confirm"},
			"content type":  {&discordgo.MessageAttachment{ID: "att1", URL: good.URL, Size: len(frag), ContentType: "image/png"}, &paperResearchCDN{body: frag}, "confirm"},
			"id mismatch":   {&discordgo.MessageAttachment{ID: "att2", URL: good.URL, Size: len(frag), ContentType: "application/json"}, &paperResearchCDN{body: frag}, "confirm"},
			"http error":    {good, &paperResearchCDN{body: frag, status: http.StatusNotFound}, "confirm"},
		}
		labels := make([]string, 0, len(refusals))
		for k := range refusals {
			labels = append(labels, k)
		}
		sort.Strings(labels)
		for _, label := range labels {
			c := refusals[label]
			msg, _, path := run(t, c.att, c.cdn, c.reply)
			if path != "" {
				t.Fatalf("%s: the config changed (%q)", label, msg)
			}
			if label != "declined" && label != "http error" && label != "size mismatch" && c.cdn.hits.Load() != 0 {
				t.Fatalf("%s: the attachment was fetched before validation", label)
			}
		}
	})
	t.Run("wizard_file_matches_the_typed_fragment", func(t *testing.T) {
		dir := t.TempDir()
		file := filepath.Join(dir, "quote.json")
		if err := os.WriteFile(file, paperResearchQuoteFragment(nil), 0o600); err != nil {
			t.Fatal(err)
		}
		opts := InitOptions{Assets: []string{"ETH"}, EnableSpot: true, SpotStrategies: []string{"sma_crossover"}, SpotCapital: 1000, SpotDrawdown: 5, QuoteOwnerFile: file}
		cfg := generateConfig(opts)
		if err := validateInitResearchConfig(cfg, opts); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "config.json")
		blob, _ := json.MarshalIndent(cfg, "", "  ")
		if err := os.WriteFile(path, blob, 0o600); err != nil {
			t.Fatal(err)
		}
		ss, webPath := paperResearchServer(t, websocket, paperResearchMomentum())
		frag := paperResearchQuoteFragment(nil)
		params := fmt.Sprintf(`{"fragment":%s}`, frag)
		confirm := structuralConfirm(t, ss, quoteOwnerSetupAction, "", params)
		if w := mutationPost(ss, ss.handleAPIAddQuoteOwner, "/api/config/add-quote-owner", fmt.Sprintf(`{"nonce":%q,"params":%s}`, confirm.Nonce, params), nil); w.Code != http.StatusOK {
			t.Fatal(w.Body.String())
		}
		if a, b := projectionText(projectStrategy(loadedStrategy(t, path, "hl-invmm-btc"))), projectionText(projectStrategy(loadedStrategy(t, webPath, "hl-invmm-btc"))); a != b {
			t.Fatalf("wizard and web quote owners differ:\n%s\n%s", a, b)
		}
		bad := filepath.Join(dir, "bad.json")
		if err := os.WriteFile(bad, []byte(`{"role":"quote_owner","strategies":[]}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := initResearchEntries(InitOptions{QuoteOwnerFile: bad}); err == nil {
			t.Fatal("the wizard must refuse a malformed quote_owner file")
		}
	})
}

func TestPaperResearchPromotionRefusedAtEveryBoundary(t *testing.T) {
	websocket := map[string]interface{}{"market_feed": "websocket"}
	research := func() map[string]interface{} {
		sc := researchScalarFixture(t, bitcoinLeadLagStrategyName)
		sc["id"] = "hl-btclag-eth"
		return sc
	}
	t.Run("confirmation_refuses_a_research_reference", func(t *testing.T) {
		for _, sc := range []map[string]interface{}{research(), researchQuoteOwnerFixture("--mode=paper")} {
			ss, path := paperResearchServer(t, websocket, paperResearchMomentum(), sc)
			before := fileSHA(t, path)
			id := sc["id"].(string)
			body := fmt.Sprintf(`{"action":"paper-to-live","strategy_id":%q,"params":{}}`, id)
			if w := mutationPost(ss, ss.handleAPIConfirm, "/api/confirm", body, nil); w.Code != http.StatusBadRequest {
				t.Fatalf("%s: confirm status %d %s", id, w.Code, w.Body.String())
			}
			d, fake := newPaperResearchDiscord(ss, "confirm")
			opts := []*discordgo.ApplicationCommandInteractionDataOption{stringOpt("strategy", id)}
			i, _ := discordInteraction("paper-to-live", opts...)
			d.handlePaperToLive(fake, i, opts)
			if !strings.HasPrefix(fake.last(), "Refused") {
				t.Fatalf("%s: discord promotion: %q", id, fake.last())
			}
			if _, err := ss.executePaperToLive(id); err == nil {
				t.Fatalf("%s: execution must refuse", id)
			}
			if fileSHA(t, path) != before {
				t.Fatalf("%s: a refused promotion changed the config", id)
			}
		}
	})
	t.Run("a_research_reference_written_after_confirmation_is_refused", func(t *testing.T) {
		ss, path := paperResearchServer(t, websocket, paperResearchMomentum())
		confirm := structuralConfirm(t, ss, "paper-to-live", "hl-momentum-eth", `{}`)
		root := readConfigRoot(t, path)
		list, _ := configStrategies(root)
		var item map[string]interface{}
		_ = json.Unmarshal(list[0], &item)
		item["args"] = []string{"bitcoin_lead_lag", "ETH", "1h", "--mode=paper"}
		item["stop_loss_atr_mult"] = 1.0
		item["close_strategy"] = map[string]interface{}{"name": "tiered_tp_pct", "params": researchTPPct()}
		item["open_strategy"] = map[string]interface{}{"name": bitcoinLeadLagStrategyName}
		raw, _ := json.Marshal(item)
		list[0] = raw
		_ = setConfigStrategies(root, list)
		if err := writeValidatedConfigRoot(path, root); err != nil {
			t.Fatalf("the paper research edit must validate: %v", err)
		}
		before := fileSHA(t, path)
		h := func(w http.ResponseWriter, r *http.Request) {
			ss.handleAPIStrategyStructural(w, r, "hl-momentum-eth", "paper-to-live")
		}
		w := mutationPost(ss, h, "/api/strategies/hl-momentum-eth/paper-to-live", fmt.Sprintf(`{"nonce":%q,"params":{}}`, confirm.Nonce), nil)
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "research-only") {
			t.Fatalf("execution after a disk edit: %d %s", w.Code, w.Body.String())
		}
		if fileSHA(t, path) != before {
			t.Fatal("a refused promotion changed the config")
		}
	})
	t.Run("a_direct_live_candidate_never_writes", func(t *testing.T) {
		ss, path := paperResearchServer(t, websocket, paperResearchMomentum(), research())
		before := fileSHA(t, path)
		for _, flip := range []func(map[string]json.RawMessage) error{
			func(root map[string]json.RawMessage) error {
				_, _, err := flipStrategyToLive(root, "hl-btclag-eth")
				return err
			},
			func(root map[string]json.RawMessage) error {
				list, _ := configStrategies(root)
				var item map[string]interface{}
				_ = json.Unmarshal(list[1], &item)
				item["args"] = []string{"bitcoin_lead_lag", "ETH", "1h"}
				item["allow_no_edge"] = true
				raw, _ := json.Marshal(item)
				list[1] = raw
				return setConfigStrategies(root, list)
			},
		} {
			if err := ss.mutateConfigRoot(flip); err == nil {
				t.Fatal("a live or missing-mode research candidate was written")
			}
		}
		if fileSHA(t, path) != before {
			t.Fatal("a refused candidate changed the config")
		}
	})
}

func paperResearchLoadError(t *testing.T, root map[string]interface{}, strategies ...map[string]interface{}) error {
	t.Helper()
	dir := t.TempDir()
	base := map[string]interface{}{
		"config_version":   CurrentConfigVersion,
		"interval_seconds": 60,
		"log_dir":          filepath.Join(dir, "logs"),
		"db_file":          filepath.Join(dir, "state.db"),
		"auto_update":      "off",
		"strategies":       strategies,
	}
	for k, v := range root {
		base[k] = v
	}
	data, err := json.MarshalIndent(base, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = LoadConfig(path)
	return err
}

func TestPaperResearchOptionsQuoteReferencesAreRefused(t *testing.T) {
	websocket := map[string]interface{}{"market_feed": "websocket"}
	options := func() map[string]interface{} {
		return map[string]interface{}{"id": "deribit-cc-btc", "type": "options", "platform": "deribit", "script": "shared_scripts/check_options.py",
			"args": []string{"covered_calls", "BTC", "--platform=deribit", "--mode=paper"}, "capital": 1000, "max_drawdown_pct": 40}
	}
	if err := paperResearchLoadError(t, websocket, options()); err != nil {
		t.Fatalf("control: a plain paper options entry must load: %v", err)
	}
	quoteBlock := options()
	quoteBlock["quote"] = map[string]interface{}{"coin": "BTC", "max_inventory_usd": 1000.0, "max_order_qty": 0.01, "request_expiry_ms": 2000}
	capabilityOpen := options()
	capabilityOpen["open_strategy"] = map[string]interface{}{"name": inventoryMarketMakingStrategyName}
	for label, sc := range map[string]map[string]interface{}{"quote block": quoteBlock, "quote capability open": capabilityOpen} {
		err := paperResearchLoadError(t, websocket, sc)
		if err == nil {
			t.Fatalf("%s: a paper options entry with a quote reference must be refused at load", label)
		}
		if !strings.Contains(err.Error(), "quote") {
			t.Fatalf("%s: the refusal must name the quote reference: %v", label, err)
		}
		t.Logf("%s refused: %v", label, err)
	}
	_, path := paperResearchServer(t, websocket, paperResearchMomentum(), researchQuoteOwnerFixture("--mode=paper"))
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("a complete Hyperliquid perps paper quote owner must still load: %v", err)
	}
	if cfg.quoteLoad != nil {
		t.Fatal("the held quote owner must load with no quote admission")
	}
	if _, held := engineHoldFor(loadedStrategy(t, path, "hl-invmm-btc")); !held {
		t.Fatal("the complete quote owner must stay engine-held")
	}
}

func TestPaperResearchBitcoinLeadLagRefusesTheLeaderAsFollower(t *testing.T) {
	websocket := map[string]interface{}{"market_feed": "websocket"}
	t.Run("web_and_discord_refuse_btc_and_keep_the_config", func(t *testing.T) {
		ss, path := paperResearchServer(t, websocket, paperResearchMomentum())
		before := fileSHA(t, path)
		body := `{"action":"add-strategy","strategy_id":"","params":{"name":"bitcoin_lead_lag","platform":"hyperliquid","asset":"BTC"}}`
		w := mutationPost(ss, ss.handleAPIConfirm, "/api/confirm", body, nil)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "follows the BTC leader") {
			t.Fatalf("web confirm of a BTC follower: %d %s", w.Code, w.Body.String())
		}
		d, fake := newPaperResearchDiscord(ss, "confirm")
		opts := []*discordgo.ApplicationCommandInteractionDataOption{stringOpt("name", "bitcoin_lead_lag"), stringOpt("platform", "hyperliquid"), stringOpt("asset", "BTC")}
		i, _ := discordInteraction("add-strategy", opts...)
		d.handleAddStrategy(fake, i, opts)
		if !strings.Contains(fake.last(), "follows the BTC leader") {
			t.Fatalf("discord add of a BTC follower: %q", fake.last())
		}
		if fileSHA(t, path) != before {
			t.Fatal("a refused BTC follower add changed the config")
		}
	})
	t.Run("wizard_skips_btc_and_keeps_the_other_followers", func(t *testing.T) {
		opts := InitOptions{Assets: []string{"BTC", "ETH"}, EnableSpot: true, SpotStrategies: []string{"sma_crossover"}, SpotCapital: 1000, SpotDrawdown: 5,
			ResearchStrategies: []string{bitcoinLeadLagStrategyName}}
		entries, err := initResearchEntries(opts)
		if err != nil {
			t.Fatal(err)
		}
		ids := make([]string, 0, len(entries))
		for _, sc := range entries {
			ids = append(ids, sc.ID)
		}
		sort.Strings(ids)
		if strings.Join(ids, ",") != "hl-btclag-eth" {
			t.Fatalf("the wizard must write only the ETH follower, got %v", ids)
		}
		cfg := generateConfig(opts)
		if err := validateInitResearchConfig(cfg, opts); err != nil {
			t.Fatalf("wizard output must validate: %v", err)
		}
		btcOnly := opts
		btcOnly.Assets = []string{"BTC"}
		if _, err := initResearchEntries(btcOnly); err == nil || !strings.Contains(err.Error(), "needs an asset other than BTC") {
			t.Fatalf("a BTC-only wizard run must refuse the lead-lag research strategy: %v", err)
		}
	})
	t.Run("eth_follower_is_still_added", func(t *testing.T) {
		ss, path := paperResearchServer(t, websocket, paperResearchMomentum())
		params := `{"name":"bitcoin_lead_lag","platform":"hyperliquid","asset":"ETH"}`
		confirm := structuralConfirm(t, ss, "add-strategy", "", params)
		if w := mutationPost(ss, ss.handleAPIAddStrategy, "/api/config/add-strategy", fmt.Sprintf(`{"nonce":%q,"params":%s}`, confirm.Nonce, params), nil); w.Code != http.StatusOK {
			t.Fatalf("an ETH follower add: %d %s", w.Code, w.Body.String())
		}
		loadedStrategy(t, path, "hl-btclag-eth")
	})
	t.Run("a_hand_written_btc_follower_is_refused_at_load", func(t *testing.T) {
		sc := researchScalarFixture(t, bitcoinLeadLagStrategyName)
		sc["id"] = "hl-btclag-btc"
		sc["args"] = []string{bitcoinLeadLagStrategyName, "BTC", "1h", "--mode=paper"}
		err := paperResearchLoadError(t, websocket, sc)
		if err == nil || !strings.Contains(err.Error(), "follower coin cannot be BTC") {
			t.Fatalf("a BTC follower must be refused at load: %v", err)
		}
	})
}

func TestPaperResearchWebQuoteOwnerUploadParsesTheFileBytes(t *testing.T) {
	websocket := map[string]interface{}{"market_feed": "websocket"}
	textParams := func(text string) string {
		blob, _ := json.Marshal(map[string]interface{}{"fragment_text": text})
		return string(blob)
	}
	t.Run("duplicate_key_inside_quote_is_refused_like_discord", func(t *testing.T) {
		ss, path := paperResearchServer(t, websocket, paperResearchMomentum())
		before := fileSHA(t, path)
		dup := strings.Replace(string(paperResearchQuoteFragment(nil)), `"coin":"BTC"`, `"coin":"ETH","coin":"BTC"`, 1)
		if dup == string(paperResearchQuoteFragment(nil)) {
			t.Fatal("fixture: the duplicate key was not inserted")
		}
		_, want := parseQuoteOwnerSetup([]byte(dup))
		if want == nil {
			t.Fatal("the shared parser must refuse a duplicate key")
		}
		body := fmt.Sprintf(`{"action":%q,"strategy_id":"","params":%s}`, quoteOwnerSetupAction, textParams(dup))
		w := mutationPost(ss, ss.handleAPIConfirm, "/api/confirm", body, nil)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "duplicate key") {
			t.Fatalf("web confirm of a duplicate-key file: %d %s", w.Code, w.Body.String())
		}
		var got map[string]string
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		if got["error"] != want.Error() {
			t.Fatalf("the web refusal must equal the shared parser refusal:\n%s\n%s", got["error"], want.Error())
		}
		if fileSHA(t, path) != before {
			t.Fatal("a refused duplicate-key file changed the config")
		}
	})
	t.Run("valid_file_text_binds_the_same_digest_at_confirm_and_apply", func(t *testing.T) {
		ss, path := paperResearchServer(t, websocket, paperResearchMomentum())
		text := string(paperResearchQuoteFragment(nil))
		params := textParams(text)
		confirm := structuralConfirm(t, ss, quoteOwnerSetupAction, "", params)
		setup, err := parseQuoteOwnerSetup([]byte(text))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(confirm.Description, setup.Digest) {
			t.Fatalf("the confirmation must carry the file digest: %s", confirm.Description)
		}
		if w := mutationPost(ss, ss.handleAPIAddQuoteOwner, "/api/config/add-quote-owner", fmt.Sprintf(`{"nonce":%q,"params":%s}`, confirm.Nonce, params), nil); w.Code != http.StatusOK {
			t.Fatalf("add-quote-owner from file text: %d %s", w.Code, w.Body.String())
		}
		if _, held := engineHoldFor(loadedStrategy(t, path, "hl-invmm-btc")); !held {
			t.Fatal("the web owner must be engine-held")
		}
	})
	t.Run("both_forms_or_an_oversized_text_are_refused", func(t *testing.T) {
		ss, path := paperResearchServer(t, websocket, paperResearchMomentum())
		before := fileSHA(t, path)
		frag := string(paperResearchQuoteFragment(nil))
		both, _ := json.Marshal(map[string]interface{}{"fragment_text": frag, "fragment": json.RawMessage(frag)})
		big := strings.Repeat(" ", quoteOwnerSetupMaxBytes+1)
		for label, params := range map[string]string{"both": string(both), "oversized": textParams(big)} {
			body := fmt.Sprintf(`{"action":%q,"strategy_id":"","params":%s}`, quoteOwnerSetupAction, params)
			if w := mutationPost(ss, ss.handleAPIConfirm, "/api/confirm", body, nil); w.Code != http.StatusBadRequest {
				t.Fatalf("%s: confirm status %d %s", label, w.Code, w.Body.String())
			}
		}
		if fileSHA(t, path) != before {
			t.Fatal("a refused upload changed the config")
		}
	})
}

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
)

const configSecretReplacement = "***redacted***"

func redactConfigForDisplay(raw []byte) (string, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return "", fmt.Errorf("parse config: %w", err)
	}
	redactSectionKeys(root, "discord", "token", "report_github_token")
	redactSectionKeys(root, "telegram", "bot_token")
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func redactSectionKeys(root map[string]json.RawMessage, section string, keys ...string) {
	raw, ok := root[section]
	if !ok {
		return
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return
	}
	changed := false
	for _, k := range keys {
		v, ok := obj[k]
		if !ok {
			continue
		}
		var s string
		if json.Unmarshal(v, &s) == nil && s != "" {
			if nb, err := json.Marshal(configSecretReplacement); err == nil {
				obj[k] = nb
				changed = true
			}
		}
	}
	if changed {
		if nb, err := json.Marshal(obj); err == nil {
			root[section] = nb
		}
	}
}

func configStrategies(root map[string]json.RawMessage) ([]json.RawMessage, error) {
	raw, ok := root["strategies"]
	if !ok {
		return nil, fmt.Errorf("config has no strategies array")
	}
	var list []json.RawMessage
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("parse strategies: %w", err)
	}
	return list, nil
}

func setConfigStrategies(root map[string]json.RawMessage, list []json.RawMessage) error {
	nb, err := json.Marshal(list)
	if err != nil {
		return err
	}
	root["strategies"] = nb
	return nil
}

func strategyRawID(raw json.RawMessage) string {
	var item struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(raw, &item)
	return item.ID
}

func buildAddStrategyEntry(name, platform, asset string) (string, json.RawMessage, error) {
	name = strings.TrimSpace(name)
	asset = strings.ToUpper(strings.TrimSpace(asset))
	platform = strings.ToLower(strings.TrimSpace(platform))
	if name == "" || asset == "" {
		return "", nil, fmt.Errorf("name and asset are required")
	}
	if _, ok := knownShortNames[name]; !ok {
		return "", nil, fmt.Errorf("unknown strategy %q — only strategies in knownShortNames are addable via slash command; use the init wizard for others", name)
	}
	if !isSimpleAssetToken(asset) {
		return "", nil, fmt.Errorf("asset %q must be a plain ticker like BTC/ETH/SOL", asset)
	}
	short := deriveShortName(name)
	var id string
	var obj map[string]interface{}
	switch platform {
	case "hyperliquid":
		id = fmt.Sprintf("hl-%s-%s", short, strings.ToLower(asset))
		direction := DirectionLong
		if isBidirectionalPerpsStrategy(name) {
			direction = DirectionBoth
		}
		obj = map[string]interface{}{
			"id":               id,
			"type":             "perps",
			"platform":         "hyperliquid",
			"script":           "shared_scripts/check_hyperliquid.py",
			"args":             []string{name, asset, "1h", "--mode=paper"},
			"capital":          100.0,
			"max_drawdown_pct": 10.0,
			"interval_seconds": 3600,
			"leverage":         1.0,
			"direction":        direction,
			"margin_mode":      "isolated",
		}
	case "binanceus":
		id = fmt.Sprintf("%s-%s", short, strings.ToLower(asset))
		obj = map[string]interface{}{
			"id":               id,
			"type":             "spot",
			"platform":         "binanceus",
			"script":           "shared_scripts/check_strategy.py",
			"args":             []string{name, asset + "/USDT", "1h"},
			"capital":          1000.0,
			"max_drawdown_pct": 5.0,
			"interval_seconds": 3600,
		}
	default:
		return "", nil, fmt.Errorf("platform %q not supported via /add-strategy (supported: hyperliquid, binanceus) — use the init wizard for others", platform)
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return "", nil, err
	}
	return id, b, nil
}

func isSimpleAssetToken(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}

func addStrategyToRoot(root map[string]json.RawMessage, name, platform, asset string) (string, error) {
	id, entry, err := buildAddStrategyEntry(name, platform, asset)
	if err != nil {
		return "", err
	}
	list, err := configStrategies(root)
	if err != nil {
		return "", err
	}
	for _, raw := range list {
		if strategyRawID(raw) == id {
			return "", fmt.Errorf("strategy %q already exists", id)
		}
	}
	list = append(list, entry)
	if err := setConfigStrategies(root, list); err != nil {
		return "", err
	}
	return id, nil
}

func removeStrategyFromRoot(root map[string]json.RawMessage, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("strategy id is required")
	}
	list, err := configStrategies(root)
	if err != nil {
		return err
	}
	idx := -1
	for i, raw := range list {
		if strategyRawID(raw) == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("strategy %q not found", id)
	}
	if len(list) == 1 {
		return fmt.Errorf("refusing to remove the only strategy %q — a config must keep at least one strategy", id)
	}
	list = append(list[:idx], list[idx+1:]...)
	return setConfigStrategies(root, list)
}

func flipStrategyToLive(root map[string]json.RawMessage, id string) (before, after []string, err error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, nil, fmt.Errorf("strategy id is required")
	}
	list, lerr := configStrategies(root)
	if lerr != nil {
		return nil, nil, lerr
	}
	idx := -1
	for i, raw := range list {
		if strategyRawID(raw) == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil, nil, fmt.Errorf("strategy %q not found", id)
	}
	var item map[string]json.RawMessage
	if err := json.Unmarshal(list[idx], &item); err != nil {
		return nil, nil, fmt.Errorf("parse strategy %q: %w", id, err)
	}
	var args []string
	if raw, ok := item["args"]; ok {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, nil, fmt.Errorf("parse args for %q: %w", id, err)
		}
	}
	before = append([]string(nil), args...)
	flipped := false
	hasLive := false
	for i, a := range args {
		switch strings.TrimSpace(a) {
		case "--mode=paper":
			args[i] = "--mode=live"
			flipped = true
		case "--mode=live":
			hasLive = true
		}
	}
	if !flipped {
		if hasLive {
			return nil, nil, fmt.Errorf("strategy %q is already live", id)
		}
		return nil, nil, fmt.Errorf("strategy %q has no --mode=paper arg to flip (only perps/futures-style strategies support paper→live)", id)
	}
	nb, err := json.Marshal(args)
	if err != nil {
		return nil, nil, err
	}
	item["args"] = nb
	rawItem, err := json.Marshal(item)
	if err != nil {
		return nil, nil, err
	}
	list[idx] = rawItem
	if err := setConfigStrategies(root, list); err != nil {
		return nil, nil, err
	}
	return before, args, nil
}

func classifyConfigSetKey(key string) (kind, id, field string) {
	key = strings.TrimSpace(key)
	if rest, ok := strings.CutPrefix(key, "strategies."); ok {
		if dot := strings.Index(rest, "."); dot > 0 && dot < len(rest)-1 {
			return "strategy", rest[:dot], rest[dot+1:]
		}
		return "strategy", "", ""
	}
	return "top", "", ""
}

func buildTunerOverride(field, value string) (map[string]json.RawMessage, error) {
	v := strings.TrimSpace(value)
	mk := func(raw string) map[string]json.RawMessage {
		return map[string]json.RawMessage{field: json.RawMessage(raw)}
	}
	switch field {
	case "interval_seconds":
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("interval_seconds must be a positive integer")
		}
		return mk(strconv.Itoa(n)), nil
	case "leverage":
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f <= 0 {
			return nil, fmt.Errorf("leverage must be a positive number")
		}
		return mk(strconv.FormatFloat(f, 'g', -1, 64)), nil
	case "direction":
		dv := strings.ToLower(v)
		if dv != DirectionLong && dv != DirectionShort && dv != DirectionBoth {
			return nil, fmt.Errorf("direction must be long, short, or both")
		}
		b, _ := json.Marshal(dv)
		return mk(string(b)), nil
	case "invert_signal":
		bv, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("invert_signal must be true or false")
		}
		return mk(strconv.FormatBool(bv)), nil
	case "stop_loss_pct", "stop_loss_atr_mult":
		if v == "" || strings.EqualFold(v, "null") || strings.EqualFold(v, "none") {
			return mk("null"), nil
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 0 {
			return nil, fmt.Errorf("%s must be a non-negative number or 'null' to clear", field)
		}
		return mk(strconv.FormatFloat(f, 'g', -1, 64)), nil
	}
	return nil, fmt.Errorf("unsupported strategy field %q (supported: interval_seconds, direction, invert_signal, leverage, stop_loss_pct, stop_loss_atr_mult)", field)
}

func applyTopLevelConfigSet(root map[string]json.RawMessage, key, value string) (restartRequired bool, summary string, err error) {
	key = strings.TrimSpace(key)
	value = strings.TrimSpace(value)
	setRaw := func(k string, v interface{}) error {
		b, mErr := json.Marshal(v)
		if mErr != nil {
			return mErr
		}
		root[k] = b
		return nil
	}
	switch key {
	case "interval_seconds":
		n, convErr := strconv.Atoi(value)
		if convErr != nil || n <= 0 {
			return false, "", fmt.Errorf("interval_seconds must be a positive integer")
		}
		if err := setRaw("interval_seconds", n); err != nil {
			return false, "", err
		}
		return false, fmt.Sprintf("interval_seconds = %d", n), nil
	case "default_stop_loss_atr_mult":
		f, convErr := strconv.ParseFloat(value, 64)
		if convErr != nil || f < 0 {
			return false, "", fmt.Errorf("default_stop_loss_atr_mult must be a non-negative number")
		}
		if err := setRaw("default_stop_loss_atr_mult", f); err != nil {
			return false, "", err
		}
		return false, fmt.Sprintf("default_stop_loss_atr_mult = %g", f), nil
	case "auto_update":
		v := strings.ToLower(value)
		if v != "off" && v != "daily" && v != "heartbeat" {
			return false, "", fmt.Errorf("auto_update must be off, daily, or heartbeat")
		}
		if err := setRaw("auto_update", v); err != nil {
			return false, "", err
		}
		return true, fmt.Sprintf("auto_update = %s", v), nil
	case "leaderboard_post_time":
		if value != "" {
			if _, _, ok := ParseLeaderboardPostTime(value); !ok {
				return false, "", fmt.Errorf("leaderboard_post_time must be HH:MM (24h UTC) or empty to disable")
			}
		}
		if err := setRaw("leaderboard_post_time", value); err != nil {
			return false, "", err
		}
		return true, fmt.Sprintf("leaderboard_post_time = %q", value), nil
	case "discord.leaderboard_top_n":
		n, convErr := strconv.Atoi(value)
		if convErr != nil || n < 1 {
			return false, "", fmt.Errorf("discord.leaderboard_top_n must be a positive integer")
		}
		if err := setNestedRaw(root, "discord", "leaderboard_top_n", n); err != nil {
			return false, "", err
		}
		return false, fmt.Sprintf("discord.leaderboard_top_n = %d", n), nil
	}
	return false, "", fmt.Errorf("unsupported config key %q (top-level: interval_seconds, default_stop_loss_atr_mult, auto_update, leaderboard_post_time, discord.leaderboard_top_n; per-strategy: strategies.<id>.<field>)", key)
}

func setNestedRaw(root map[string]json.RawMessage, section, key string, v interface{}) error {
	obj := map[string]json.RawMessage{}
	if raw, ok := root[section]; ok {
		if err := json.Unmarshal(raw, &obj); err != nil {
			return fmt.Errorf("parse %s: %w", section, err)
		}
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	obj[key] = b
	nb, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	root[section] = nb
	return nil
}

var addPlatformKnown = map[string]string{
	"hyperliquid": "Hyperliquid perps",
	"binanceus":   "BinanceUS spot",
	"deribit":     "Deribit options",
	"ibkr":        "Interactive Brokers options/futures",
	"topstep":     "TopStep futures",
	"robinhood":   "Robinhood options",
	"okx":         "OKX spot/swap",
	"luno":        "Luno spot",
}

func platformSetupGuide(name string) (string, error) {
	n := strings.ToLower(strings.TrimSpace(name))
	label, ok := addPlatformKnown[n]
	if !ok {
		known := make([]string, 0, len(addPlatformKnown))
		for k := range addPlatformKnown {
			known = append(known, k)
		}
		sort.Strings(known)
		return "", fmt.Errorf("unknown platform %q (known: %s)", name, strings.Join(known, ", "))
	}
	addable := n == "hyperliquid" || n == "binanceus"
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("**Platform setup — %s** (%s)\n", n, label))
	sb.WriteString("Credentials are loaded from the environment, never the config file, so this command writes no secrets to disk. To finish setup:\n")
	sb.WriteString(fmt.Sprintf("1. Add the platform's API credentials to `/opt/go-trader/.env` (the exact env var names are in `platforms/%s/adapter.py` and `shared_scripts/check_%s.py`).\n", n, n))
	if addable {
		sb.WriteString(fmt.Sprintf("2. Add a strategy: `/go-trader-add-strategy <name> %s <asset>` (created in paper mode; promote later with `/go-trader-paper-to-live`).\n", n))
	} else {
		sb.WriteString("2. Add a strategy via the init wizard (`go-trader init`) — `/go-trader-add-strategy` only generates hyperliquid + binanceus entries.\n")
	}
	sb.WriteString("3. Restart go-trader (`/go-trader-restart`) so the new credentials and strategy load.\n")
	return sb.String(), nil
}

func confirmYes(reply string) bool {
	switch strings.ToLower(strings.TrimSpace(reply)) {
	case "y", "yes", "confirm", "ok":
		return true
	}
	return false
}

func writeValidatedConfigRoot(configPath string, root map[string]json.RawMessage) error {
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	if needsV13SchemaMigration(out) {
		return fmt.Errorf("config requires migration to version >= 13; restart go-trader once to migrate before using config commands")
	}
	tmp, err := os.CreateTemp(filepath.Dir(configPath), "go-trader-config-*.json")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	defer os.Remove(tmpPath)
	if err := os.WriteFile(tmpPath, out, 0o600); err != nil {
		return err
	}
	if _, err := LoadConfigForProbe(tmpPath); err != nil {
		return fmt.Errorf("patched config invalid: %w", err)
	}
	return os.Rename(tmpPath, configPath)
}

func requestSIGHUPReload() error {
	return syscall.Kill(os.Getpid(), syscall.SIGHUP)
}

func (d *DiscordNotifier) configOpsReady() (string, error) {
	if d.ss == nil {
		return "", fmt.Errorf("status server not wired; config commands unavailable")
	}
	p := strings.TrimSpace(d.ss.configPath)
	if p == "" {
		return "", fmt.Errorf("config path not configured; config commands unavailable")
	}
	return p, nil
}

func (d *DiscordNotifier) mutateConfig(_ string, fn func(root map[string]json.RawMessage) error) error {
	return d.ss.mutateConfigRoot(fn)
}

func followupText(s *discordgo.Session, i *discordgo.InteractionCreate, content string) {
	if content == "" {
		content = "(no output)"
	}
	_, _ = s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
		Content: truncateForDiscord(content),
	})
}

func (d *DiscordNotifier) applyConfigChange(s *discordgo.Session, i *discordgo.InteractionCreate, restartRequired bool, doneMsg string) {
	if restartRequired {
		followupText(s, i, doneMsg+"\nApplying via **service restart** — this instance briefly goes offline; the new one resumes the cycle.")
		go func() {
			_ = restartSelf()
		}()
		return
	}
	if err := requestSIGHUPReload(); err != nil {
		followupText(s, i, doneMsg+"\n⚠️ Wrote config but failed to signal reload: "+err.Error()+" — send SIGHUP or restart manually.")
		return
	}
	followupText(s, i, doneMsg+"\nApplied via **SIGHUP hot-reload**; effective next cycle. (If the reload is rejected as incompatible — e.g. an open-position guard — the running config is kept and the file change applies on the next restart; check logs.)")
}

func (d *DiscordNotifier) confirmDestructive(userID, prompt string) bool {
	resp, err := d.AskDM(userID, prompt+"\n\nReply `confirm` within 60s to proceed (anything else cancels).", 60*time.Second)
	if err != nil {
		return false
	}
	return confirmYes(resp)
}

func (d *DiscordNotifier) handleConfigShow(s *discordgo.Session, i *discordgo.InteractionCreate) {
	deferAck(s, i)
	path, err := d.configOpsReady()
	if err != nil {
		followupText(s, i, err.Error())
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		followupText(s, i, "read config failed: "+err.Error())
		return
	}
	pretty, err := redactConfigForDisplay(raw)
	if err != nil {
		followupText(s, i, "render config failed: "+err.Error())
		return
	}
	_, _ = s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
		Content: "Current config (Discord/Telegram tokens redacted; platform keys + status token live in the environment, not this file):",
		Files: []*discordgo.File{{
			Name:        "config.redacted.json",
			ContentType: "application/json",
			Reader:      strings.NewReader(pretty),
		}},
	})
}

func (d *DiscordNotifier) handleConfigSet(s *discordgo.Session, i *discordgo.InteractionCreate, opts []*discordgo.ApplicationCommandInteractionDataOption) {
	deferAck(s, i)
	path, err := d.configOpsReady()
	if err != nil {
		followupText(s, i, err.Error())
		return
	}
	key := optionString(opts, "key", "")
	value := optionString(opts, "value", "")
	if key == "" {
		followupText(s, i, "usage: /go-trader-config set <key> <value>")
		return
	}
	kind, id, field := classifyConfigSetKey(key)
	if kind == "strategy" {
		if id == "" || field == "" {
			followupText(s, i, "strategy key must be strategies.<id>.<field>")
			return
		}
		sc, ok := d.ss.strategyConfig(id)
		if !ok {
			followupText(s, i, "strategy not found: "+id)
			return
		}
		override, oErr := buildTunerOverride(field, value)
		if oErr != nil {
			followupText(s, i, oErr.Error())
			return
		}
		merged, mErr := mergeStrategyTunerOverrides(sc, override)
		if mErr != nil {
			followupText(s, i, mErr.Error())
			return
		}
		hasOpen := d.ss.strategyHasOpenPosition(id)
		d.ss.configWriteMu.Lock()
		restartRequired, pErr := applyStrategyConfigPatch(path, id, merged, override, hasOpen)
		d.ss.configWriteMu.Unlock()
		if pErr != nil {
			followupText(s, i, "config set failed: "+pErr.Error())
			return
		}
		d.applyConfigChange(s, i, restartRequired, fmt.Sprintf("Set `%s` = `%s` on strategy `%s`.", field, value, id))
		return
	}

	var restartRequired bool
	var summary string
	wErr := d.mutateConfig(path, func(root map[string]json.RawMessage) error {
		r, sm, e := applyTopLevelConfigSet(root, key, value)
		restartRequired = r
		summary = sm
		return e
	})
	if wErr != nil {
		followupText(s, i, "config set failed: "+wErr.Error())
		return
	}
	d.applyConfigChange(s, i, restartRequired, "Set "+summary+".")
}

func (d *DiscordNotifier) handleAddStrategy(s *discordgo.Session, i *discordgo.InteractionCreate, opts []*discordgo.ApplicationCommandInteractionDataOption) {
	deferAck(s, i)
	path, err := d.configOpsReady()
	if err != nil {
		followupText(s, i, err.Error())
		return
	}
	name := optionString(opts, "name", "")
	platform := optionString(opts, "platform", "")
	asset := optionString(opts, "asset", "")
	var newID string
	wErr := d.mutateConfig(path, func(root map[string]json.RawMessage) error {
		id, e := addStrategyToRoot(root, name, platform, asset)
		newID = id
		return e
	})
	if wErr != nil {
		followupText(s, i, "add-strategy failed: "+wErr.Error())
		return
	}
	d.applyConfigChange(s, i, true, fmt.Sprintf("Added strategy `%s` (paper mode).", newID))
}

func (d *DiscordNotifier) handleRemoveStrategy(s *discordgo.Session, i *discordgo.InteractionCreate, opts []*discordgo.ApplicationCommandInteractionDataOption) {
	deferAck(s, i)
	path, err := d.configOpsReady()
	if err != nil {
		followupText(s, i, err.Error())
		return
	}
	id := optionString(opts, "id", "")
	if id == "" {
		followupText(s, i, "usage: /go-trader-remove-strategy <id>")
		return
	}
	if !d.confirmDestructive(interactionUserID(i), fmt.Sprintf("Remove strategy `%s` from the config? It stops trading after restart. Its open positions and trade history in the state DB are NOT touched.", id)) {
		followupText(s, i, "Cancelled — no confirmation received.")
		return
	}
	wErr := d.mutateConfig(path, func(root map[string]json.RawMessage) error {
		return removeStrategyFromRoot(root, id)
	})
	if wErr != nil {
		followupText(s, i, "remove-strategy failed: "+wErr.Error())
		return
	}
	d.applyConfigChange(s, i, true, fmt.Sprintf("Removed strategy `%s`.", id))
}

func (d *DiscordNotifier) handleAddPlatform(s *discordgo.Session, i *discordgo.InteractionCreate, opts []*discordgo.ApplicationCommandInteractionDataOption) {
	name := optionString(opts, "name", "")
	guide, err := platformSetupGuide(name)
	if err != nil {
		respondEphemeral(s, i, err.Error())
		return
	}
	respondText(s, i, guide)
}

func (d *DiscordNotifier) handlePaperToLive(s *discordgo.Session, i *discordgo.InteractionCreate, opts []*discordgo.ApplicationCommandInteractionDataOption) {
	deferAck(s, i)
	if _, err := d.configOpsReady(); err != nil {
		followupText(s, i, err.Error())
		return
	}
	id := optionString(opts, "strategy", "")
	if id == "" {
		followupText(s, i, "usage: /go-trader-paper-to-live <strategy>")
		return
	}
	if reason := d.ss.paperToLiveBlockedReason(id, false); reason != "" {
		followupText(s, i, "Refused: "+reason)
		return
	}
	if !d.confirmDestructive(interactionUserID(i), fmt.Sprintf("⚠️ Switch strategy `%s` from PAPER to LIVE? After restart it places **real orders with real funds**.", id)) {
		followupText(s, i, "Cancelled — no confirmation received.")
		return
	}
	msg, err := d.ss.executePaperToLive(id)
	if err != nil {
		followupText(s, i, "paper-to-live failed: "+err.Error())
		return
	}
	d.applyConfigChange(s, i, true, msg)
}

func (d *DiscordNotifier) handleClearCashReconcile(s *discordgo.Session, i *discordgo.InteractionCreate, opts []*discordgo.ApplicationCommandInteractionDataOption) {
	deferAck(s, i)
	id := optionString(opts, "strategy", "")
	if id == "" {
		followupText(s, i, "usage: /go-trader-clear-cash-reconcile <strategy>")
		return
	}
	if d.ss == nil || d.ss.state == nil || d.ss.mu == nil {
		followupText(s, i, "status server not ready")
		return
	}
	prompt := fmt.Sprintf(
		"Clear CashReconcileRequired for `%s`? This resumes live spot buys for that strategy. Only confirm after books match the venue — this command does not change virtual cash.",
		id,
	)
	if !d.confirmDestructive(interactionUserID(i), prompt) {
		followupText(s, i, "Cancelled — no confirmation received.")
		return
	}

	d.ss.mu.Lock()
	cash, cleared, err := clearCashReconcileRequiredForStrategy(d.ss.state, id)
	var saveErr error
	if err == nil && cleared && d.ss.stateDB != nil {
		saveErr = SaveStateWithStore(d.ss.state, d.ss.stateDB)
	}
	d.ss.mu.Unlock()

	if err != nil {
		followupText(s, i, err.Error())
		return
	}
	if !cleared {
		followupText(s, i, fmt.Sprintf("Strategy `%s` is not latched (cash=$%.4f) — nothing to clear.", id, cash))
		return
	}
	msg := fmt.Sprintf("Cleared CashReconcileRequired for `%s` (cash=$%.4f). Live buys may resume; virtual cash was not changed.", id, cash)
	if saveErr != nil {
		msg += " WARNING: in-memory clear succeeded but SaveState failed (" + saveErr.Error() + ") — latch may return on restart until the next successful save."
	}
	followupText(s, i, msg)
}

func subcommandOptions(data discordgo.ApplicationCommandInteractionData) (string, []*discordgo.ApplicationCommandInteractionDataOption) {
	for _, o := range data.Options {
		if o.Type == discordgo.ApplicationCommandOptionSubCommand {
			return o.Name, o.Options
		}
	}
	return "", data.Options
}

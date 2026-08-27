package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)


type regimeGatePreset struct {
	Name           string
	Label          string
	WindowKey      string
	WindowSpec     RegimeWindowSpec
	AllowedRegimes []string
	EligibleTypes  []string
}

var regimeGatePresets = map[string]regimeGatePreset{
	"comp_up_clean_p21": {
		Name:      "comp_up_clean_p21",
		Label:     "composite `trending_up_clean` @ period 21 (#1197 breakout entry gate)",
		WindowKey: "comp_p21",
		WindowSpec: RegimeWindowSpec{
			Classifier: regimeClassifierComposite,
			Period:     21,
		},
		AllowedRegimes: []string{"trending_up_clean"},
		EligibleTypes:  []string{"futures", "perps"},
	},
}

const defaultRegimeGatePresetName = "comp_up_clean_p21"

func regimeGatePresetByName(name string) (regimeGatePreset, bool) {
	p, ok := regimeGatePresets[strings.ToLower(strings.TrimSpace(name))]
	return p, ok
}

func regimeGatePresetNames() []string {
	names := make([]string, 0, len(regimeGatePresets))
	for n := range regimeGatePresets {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func presetAllowsType(preset regimeGatePreset, typ string) bool {
	typ = strings.TrimSpace(strings.ToLower(typ))
	for _, t := range preset.EligibleTypes {
		if strings.ToLower(t) == typ {
			return true
		}
	}
	return false
}

func strategyEligibleForRegimeGate(sc StrategyConfig, preset regimeGatePreset) bool {
	return presetAllowsType(preset, sc.Type)
}

type gateCandidate struct {
	sc      StrategyConfig
	live    bool
	hasOpen bool
}

func (ss *StatusServer) strategiesForRegimeGate(preset regimeGatePreset) []gateCandidate {
	ss.strategiesMu.RLock()
	scs := make([]StrategyConfig, len(ss.strategies))
	copy(scs, ss.strategies)
	ss.strategiesMu.RUnlock()

	out := make([]gateCandidate, 0, len(scs))
	for _, sc := range scs {
		if !strategyEligibleForRegimeGate(sc, preset) {
			continue
		}
		out = append(out, gateCandidate{
			sc:      sc,
			live:    isLiveArgs(sc.Args),
			hasOpen: ss.strategyHasOpenPosition(sc.ID),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].sc.ID < out[j].sc.ID })
	return out
}

func buildRegimeGatePickerMessage(candidates []gateCandidate, preset regimeGatePreset) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Apply regime gate **%s** — %s — to which strategy?\n", preset.Name, preset.Label))
	sb.WriteString("Reply with a number within 60s (anything else cancels):\n")
	for idx, c := range candidates {
		mode := "paper"
		if c.live {
			mode = "live"
		}
		platform := c.sc.Platform
		if platform == "" {
			platform = "-"
		}
		line := fmt.Sprintf("%d. `%s` — %s/%s (%s)", idx+1, c.sc.ID, c.sc.Type, platform, mode)
		if c.hasOpen {
			line += " — ⚠️ has an open position (cannot gate while open)"
		} else {
			line += " — flat"
		}
		sb.WriteString(line + "\n")
	}
	return sb.String()
}

func parseRegimeGateSelection(reply string, n int) (int, bool) {
	num, err := strconv.Atoi(strings.TrimSpace(reply))
	if err != nil || num < 1 || num > n {
		return 0, false
	}
	return num - 1, true
}

func regimeDetectionEnabled(root map[string]json.RawMessage) bool {
	raw, ok := root["regime"]
	if !ok {
		return false
	}
	var regime struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.Unmarshal(raw, &regime); err != nil {
		return false
	}
	return regime.Enabled
}

func regimeGateSideEffectStrategies(root map[string]json.RawMessage, targetID string) ([]string, error) {
	if regimeDetectionEnabled(root) {
		return nil, nil
	}
	list, err := configStrategies(root)
	if err != nil {
		return nil, err
	}
	targetID = strings.TrimSpace(targetID)
	var out []string
	for _, raw := range list {
		id := strategyRawID(raw)
		if id == "" || id == targetID {
			continue
		}
		var item struct {
			AllowedRegimes []string `json:"allowed_regimes"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			continue
		}
		if len(item.AllowedRegimes) > 0 {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out, nil
}

func regimeGateBlastRadiusGrew(fresh, shown []string) []string {
	shownSet := make(map[string]bool, len(shown))
	for _, id := range shown {
		shownSet[id] = true
	}
	var grew []string
	for _, id := range fresh {
		if !shownSet[id] {
			grew = append(grew, id)
		}
	}
	return grew
}

func formatStrategyIDList(ids []string) string {
	quoted := make([]string, len(ids))
	for i, id := range ids {
		quoted[i] = "`" + id + "`"
	}
	return strings.Join(quoted, ", ")
}

func buildRegimeGateConfirmMessage(preset regimeGatePreset, targetID string, alsoActivated []string) string {
	msg := fmt.Sprintf("Apply regime gate **%s** (%s) to strategy `%s`?\nThis sets `regime_gate_window`=`%s` and `allowed_regimes`=%v, enables regime detection, and adds the `%s` window if missing. It only blocks ENTRIES outside the allowed regime — closes/management always run. Applied via a service restart.",
		preset.Name, preset.Label, targetID, preset.WindowKey, preset.AllowedRegimes, preset.WindowKey)
	if len(alsoActivated) > 0 {
		msg += fmt.Sprintf("\n\n⚠️ Enabling regime detection also activates previously-dormant `allowed_regimes` entry-gates on %d other strategy(ies) you did NOT select: %s. Regime detection is off today so those gates are no-ops, but they will gate new entries after the restart. Open positions are unaffected — management and closes always run; only fresh entries are gated. To leave one ungated, clear its `allowed_regimes` first.",
			len(alsoActivated), formatStrategyIDList(alsoActivated))
	}
	return msg
}

func ensureRegimeGateWindow(root map[string]json.RawMessage, preset regimeGatePreset) error {
	regime := map[string]json.RawMessage{}
	if raw, ok := root["regime"]; ok {
		if err := json.Unmarshal(raw, &regime); err != nil {
			return fmt.Errorf("parse regime: %w", err)
		}
	}
	if tb, err := json.Marshal(true); err == nil {
		regime["enabled"] = tb
	}

	windows := map[string]json.RawMessage{}
	if raw, ok := regime["windows"]; ok {
		if err := json.Unmarshal(raw, &windows); err != nil {
			return fmt.Errorf("parse regime.windows: %w", err)
		}
	}
	if existing, ok := windows[preset.WindowKey]; ok {
		spec, err := parseRegimeWindowSpecRaw(existing)
		if err != nil {
			return fmt.Errorf("parse regime.windows[%q]: %w", preset.WindowKey, err)
		}
		if spec.effectiveClassifier() != preset.WindowSpec.effectiveClassifier() || spec.Period != preset.WindowSpec.Period {
			return fmt.Errorf("regime.windows[%q] already exists as classifier=%q period=%d but gate %q needs classifier=%q period=%d — refusing to overwrite; rename or fix the existing window first",
				preset.WindowKey, spec.effectiveClassifier(), spec.Period, preset.Name,
				preset.WindowSpec.effectiveClassifier(), preset.WindowSpec.Period)
		}
	} else {
		specB, err := json.Marshal(preset.WindowSpec)
		if err != nil {
			return err
		}
		windows[preset.WindowKey] = specB
	}
	wb, err := json.Marshal(windows)
	if err != nil {
		return err
	}
	regime["windows"] = wb

	rb, err := json.Marshal(regime)
	if err != nil {
		return err
	}
	root["regime"] = rb
	return nil
}

func parseRegimeWindowSpecRaw(raw json.RawMessage) (RegimeWindowSpec, error) {
	var asInt int
	if err := json.Unmarshal(raw, &asInt); err == nil {
		return RegimeWindowSpec{Classifier: regimeClassifierADX, Period: asInt}, nil
	}
	var spec RegimeWindowSpec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return RegimeWindowSpec{}, err
	}
	return spec, nil
}

func applyRegimeGateToRoot(root map[string]json.RawMessage, strategyID string, preset regimeGatePreset) error {
	strategyID = strings.TrimSpace(strategyID)
	if strategyID == "" {
		return fmt.Errorf("strategy id is required")
	}

	list, err := configStrategies(root)
	if err != nil {
		return err
	}
	idx := -1
	for i, raw := range list {
		if strategyRawID(raw) == strategyID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("strategy %q not found", strategyID)
	}

	var item map[string]json.RawMessage
	if err := json.Unmarshal(list[idx], &item); err != nil {
		return fmt.Errorf("parse strategy %q: %w", strategyID, err)
	}
	var typ string
	if raw, ok := item["type"]; ok {
		_ = json.Unmarshal(raw, &typ)
	}
	if !presetAllowsType(preset, typ) {
		return fmt.Errorf("strategy %q is type %q, not eligible for gate %q (eligible: %s)",
			strategyID, typ, preset.Name, strings.Join(preset.EligibleTypes, ", "))
	}

	if err := ensureRegimeGateWindow(root, preset); err != nil {
		return err
	}

	gwB, err := json.Marshal(preset.WindowKey)
	if err != nil {
		return err
	}
	item["regime_gate_window"] = gwB
	arB, err := json.Marshal(preset.AllowedRegimes)
	if err != nil {
		return err
	}
	item["allowed_regimes"] = arB

	rawItem, err := json.Marshal(item)
	if err != nil {
		return err
	}
	list[idx] = rawItem
	return setConfigStrategies(root, list)
}

func (d *DiscordNotifier) handleApplyRegimeGate(s *discordgo.Session, i *discordgo.InteractionCreate, opts []*discordgo.ApplicationCommandInteractionDataOption) {
	deferAck(s, i)
	path, err := d.configOpsReady()
	if err != nil {
		followupText(s, i, err.Error())
		return
	}

	presetName := optionString(opts, "gate", defaultRegimeGatePresetName)
	preset, ok := regimeGatePresetByName(presetName)
	if !ok {
		followupText(s, i, fmt.Sprintf("unknown gate %q (available: %s)", presetName, strings.Join(regimeGatePresetNames(), ", ")))
		return
	}

	candidates := d.ss.strategiesForRegimeGate(preset)
	if len(candidates) == 0 {
		followupText(s, i, fmt.Sprintf("No eligible strategies for gate `%s` (needs type: %s). Nothing to do.", preset.Name, strings.Join(preset.EligibleTypes, "/")))
		return
	}

	userID := interactionUserID(i)
	reply, aerr := d.AskDM(userID, buildRegimeGatePickerMessage(candidates, preset), 60*time.Second)
	if aerr != nil {
		followupText(s, i, "No selection received (timed out or DM failed) — nothing changed.")
		return
	}
	sel, ok := parseRegimeGateSelection(reply, len(candidates))
	if !ok {
		followupText(s, i, "Cancelled — reply was not a listed number.")
		return
	}
	target := candidates[sel]

	if target.hasOpen {
		followupText(s, i, fmt.Sprintf("Refused: strategy `%s` has an open position. A regime entry-gate can only be wired while the strategy is flat (it would otherwise rebind the open position's stamped regime semantics). Flatten it first, then re-run.", target.sc.ID))
		return
	}

	var alsoActivated []string
	if raw, rerr := os.ReadFile(path); rerr == nil {
		var previewRoot map[string]json.RawMessage
		if json.Unmarshal(raw, &previewRoot) == nil {
			alsoActivated, _ = regimeGateSideEffectStrategies(previewRoot, target.sc.ID)
		}
	}

	confirmMsg := buildRegimeGateConfirmMessage(preset, target.sc.ID, alsoActivated)
	if !d.confirmDestructive(userID, confirmMsg) {
		followupText(s, i, "Cancelled — no confirmation received.")
		return
	}

	if d.ss.strategyHasOpenPosition(target.sc.ID) {
		followupText(s, i, fmt.Sprintf("Refused: strategy `%s` opened a position while confirming — cannot gate while open. Nothing changed.", target.sc.ID))
		return
	}

	var freshAlsoActivated []string
	if raw, rerr := os.ReadFile(path); rerr == nil {
		var freshRoot map[string]json.RawMessage
		if json.Unmarshal(raw, &freshRoot) == nil {
			freshAlsoActivated, _ = regimeGateSideEffectStrategies(freshRoot, target.sc.ID)
		}
	}
	if grew := regimeGateBlastRadiusGrew(freshAlsoActivated, alsoActivated); len(grew) > 0 {
		followupText(s, i, fmt.Sprintf("Refused: the regime-gate blast radius changed while you were confirming — strategy(ies) %s would now also be newly activated, which you were not shown. Nothing changed. Re-run `/apply-regime-gate` to review and confirm the current set.", formatStrategyIDList(grew)))
		return
	}

	wErr := d.mutateConfig(path, func(root map[string]json.RawMessage) error {
		return applyRegimeGateToRoot(root, target.sc.ID, preset)
	})
	if wErr != nil {
		followupText(s, i, "apply-regime-gate failed: "+wErr.Error())
		return
	}
	d.applyConfigChange(s, i, true, fmt.Sprintf("Wired regime gate `%s` onto strategy `%s` (regime_gate_window=`%s`, allowed_regimes=%v).",
		preset.Name, target.sc.ID, preset.WindowKey, preset.AllowedRegimes))
}

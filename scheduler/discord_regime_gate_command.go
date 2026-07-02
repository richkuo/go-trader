package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

// This file implements the interactive /apply-regime-gate mutating Discord
// command (#1205): it wires a validated regime entry-gate onto an
// operator-chosen live strategy without the operator hand-editing the
// out-of-tree config JSON.
//
// Safety model (same as the other mutating commands in
// discord_mutating_commands.go):
//   - Auth: owner-only AND DM-only via opsCommandNames + authorizeCommand.
//   - The strategy is picked interactively (numbered-list reply over AskDM,
//     the only interactive primitive the codebase has), listing only
//     type-eligible strategies so the operator can't gate an ineligible one.
//   - A regime entry-gate rebinds the stamped regime semantics of any open
//     position, so the target MUST be flat — refused (before AND after the
//     confirm, since a position can open mid-prompt) rather than queued.
//   - An explicit DM confirm precedes the write.
//   - The write goes through mutateConfig → writeValidatedConfigRoot (atomic
//     temp → LoadConfigForProbe validation → rename, serialized on
//     configWriteMu). Apply is via restart: adding a regime.windows entry is
//     rejected by the SIGHUP hot-reload path (validateHotReloadCompatible),
//     so a clean reload requires a restart.
//
// The pure helpers (preset registry, eligibility, applyRegimeGateToRoot,
// selection/prompt formatting) operate on decoded JSON / plain structs so they
// are unit-testable without a Discord gateway — see
// discord_regime_gate_command_test.go.

// regimeGatePreset is a named, validated regime entry-gate wiring an operator
// can apply to a strategy. It bundles the top-level regime.windows entry the
// gate reads with the per-strategy fields (regime_gate_window + allowed_regimes)
// that point at it. Shipping presets rather than free-form input keeps the
// operator on wirings that have been validated end to end (see the #1197
// evidence pinned by regime_comp_up_clean_gate_test.go).
type regimeGatePreset struct {
	Name           string           // command-facing id, e.g. "comp_up_clean_p21"
	Label          string           // one-line human description
	WindowKey      string           // regime.windows key the gate reads
	WindowSpec     RegimeWindowSpec // spec written under WindowKey
	AllowedRegimes []string         // labels that admit an entry
	EligibleTypes  []string         // strategy types the gate was validated against
}

// regimeGatePresets is the registry of applyable gates. Ships with the #1197
// comp_up_clean_p21 breakout gate (composite trending_up_clean at classifier
// period 21) validated for futures/perps; add new presets here.
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

// defaultRegimeGatePresetName is applied when the command's optional `gate`
// option is omitted.
const defaultRegimeGatePresetName = "comp_up_clean_p21"

// regimeGatePresetByName resolves a preset by name (case/space-insensitive).
func regimeGatePresetByName(name string) (regimeGatePreset, bool) {
	p, ok := regimeGatePresets[strings.ToLower(strings.TrimSpace(name))]
	return p, ok
}

// regimeGatePresetNames returns the registered preset names, sorted.
func regimeGatePresetNames() []string {
	names := make([]string, 0, len(regimeGatePresets))
	for n := range regimeGatePresets {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// presetAllowsType reports whether a strategy of the given type is eligible for
// the preset.
func presetAllowsType(preset regimeGatePreset, typ string) bool {
	typ = strings.TrimSpace(strings.ToLower(typ))
	for _, t := range preset.EligibleTypes {
		if strings.ToLower(t) == typ {
			return true
		}
	}
	return false
}

// strategyEligibleForRegimeGate reports whether a strategy may receive the gate.
// Eligibility is by type only (the composite gate was validated against
// futures/perps): platform within those types and live-vs-paper are surfaced to
// the operator but don't exclude a strategy.
func strategyEligibleForRegimeGate(sc StrategyConfig, preset regimeGatePreset) bool {
	return presetAllowsType(preset, sc.Type)
}

// gateCandidate is one eligible strategy plus the deployment context shown in
// the picker.
type gateCandidate struct {
	sc      StrategyConfig
	live    bool
	hasOpen bool
}

// strategiesForRegimeGate returns the eligible strategies for the preset,
// sorted by ID, each annotated with live/paper and open-position status.
//
// Lock discipline: it snapshots the strategy list under strategiesMu, releases
// it, then queries open-position status via strategyHasOpenPosition (which takes
// mu). The two locks are never held simultaneously, preserving the mu →
// strategiesMu order.
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

// buildRegimeGatePickerMessage renders the numbered-list DM the operator replies
// to. Each line names the strategy, its type/platform, live/paper mode, and
// whether it currently holds an open position (which will block the apply).
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

// parseRegimeGateSelection parses a picker reply into a 0-based index. Accepts a
// bare 1..n list number; anything else (non-numeric, out of range, "cancel")
// returns ok=false so the caller cancels.
func parseRegimeGateSelection(reply string, n int) (int, bool) {
	num, err := strconv.Atoi(strings.TrimSpace(reply))
	if err != nil || num < 1 || num > n {
		return 0, false
	}
	return num - 1, true
}

// ensureRegimeGateWindow enables regime detection and adds the preset's window
// to root["regime"].windows if absent. If the window key already exists with a
// matching classifier+period it is left as-is; if it exists with a different
// spec the operation is refused rather than clobbering an operator-defined
// window that other strategies may reference.
func ensureRegimeGateWindow(root map[string]json.RawMessage, preset regimeGatePreset) error {
	regime := map[string]json.RawMessage{}
	if raw, ok := root["regime"]; ok {
		if err := json.Unmarshal(raw, &regime); err != nil {
			return fmt.Errorf("parse regime: %w", err)
		}
	}
	// The gate only runs when regime detection is enabled.
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
		// Matching spec already present — nothing to change.
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

// parseRegimeWindowSpecRaw decodes a regime.windows value, accepting the bare-int
// ADX shorthand (mirrors RegimeWindowsMap.UnmarshalJSON).
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

// applyRegimeGateToRoot wires the preset's gate onto the strategy with the given
// ID: it ensures the regime window exists (ensureRegimeGateWindow) and sets the
// strategy's regime_gate_window + allowed_regimes. It re-checks the persisted
// strategy type as defense-in-depth so an ineligible strategy can never be
// gated even if reached directly. The whole change is validated by
// writeValidatedConfigRoot before it is persisted.
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

// handleApplyRegimeGate is the /apply-regime-gate handler: pick a preset, list
// eligible strategies, ask the operator to choose one, confirm, then write the
// gate wiring through the safe path and restart to load it.
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

	confirmMsg := fmt.Sprintf("Apply regime gate **%s** (%s) to strategy `%s`?\nThis sets `regime_gate_window`=`%s` and `allowed_regimes`=%v, enables regime detection, and adds the `%s` window if missing. It only blocks ENTRIES outside the allowed regime — closes/management always run. Applied via a service restart.",
		preset.Name, preset.Label, target.sc.ID, preset.WindowKey, preset.AllowedRegimes, preset.WindowKey)
	if !d.confirmDestructive(userID, confirmMsg) {
		followupText(s, i, "Cancelled — no confirmation received.")
		return
	}

	// Re-check flat: a position may have opened during the picker/confirm prompts.
	if d.ss.strategyHasOpenPosition(target.sc.ID) {
		followupText(s, i, fmt.Sprintf("Refused: strategy `%s` opened a position while confirming — cannot gate while open. Nothing changed.", target.sc.ID))
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

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
//   - An explicit DM confirm precedes the write. The confirm message's
//     blast-radius list (other strategies a regime.enabled flip would newly
//     activate) is recomputed immediately before the write and the write is
//     refused if it grew past what was confirmed (regimeGateBlastRadiusGrew)
//     — a concurrent config edit during the confirm wait must not silently
//     widen what the operator agreed to.
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

// regimeDetectionEnabled reports whether root["regime"].enabled is currently
// true. An absent or unparseable regime block reads as disabled, matching the
// load-time gate semantics (config.go treats regime==nil || !enabled as off).
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

// regimeGateSideEffectStrategies returns the sorted IDs of strategies OTHER than
// targetID whose entry-gating would be newly activated as a side effect of this
// apply flipping regime.enabled from false→true.
//
// A strategy's regime entry-gate (any non-empty allowed_regimes) is a no-op
// while regime detection is off — config.go WARNs on exactly this state. Wiring
// the target's gate enables regime detection for the whole config, so every
// other strategy that already carries allowed_regimes (whether via a
// regime_gate_window or the legacy single-lookback gate) also becomes live on
// the restart, changing an entry behavior the operator never selected. These are
// surfaced in the confirm prompt so the operator sees the full blast radius.
//
// When regime detection is already enabled there is no flip and hence no side
// effect, so this returns nil — the prompt must not then falsely warn.
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

// regimeGateBlastRadiusGrew returns the entries in fresh that are absent from
// shown — the strategies that would be newly activated by the write beyond
// what the operator was shown at confirm time. A concurrent config edit
// during the picker/confirm prompts (another Discord session, or the /config
// web UI) can change the blast radius between when it was computed for the
// confirm message and when the write actually happens; recomputing it right
// before the write and refusing only on growth means:
//   - an edit that ADDS allowed_regimes to a strategy the operator was never
//     shown is caught (fresh gains an id absent from shown);
//   - an edit that REMOVES allowed_regimes from a previously-listed strategy
//     does not block the write (shown may be a superset of fresh — a strictly
//     safer outcome than what was confirmed, not a reason to refuse);
//   - a concurrent toggle of regime.enabled itself only ever shrinks or
//     empties fresh relative to shown (turning the flip into a no-op), which
//     is also not growth.
//
// Both inputs are expected pre-sorted, as returned by
// regimeGateSideEffectStrategies.
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

// formatStrategyIDList renders IDs as a comma-separated, backticked list for a
// DM message.
func formatStrategyIDList(ids []string) string {
	quoted := make([]string, len(ids))
	for i, id := range ids {
		quoted[i] = "`" + id + "`"
	}
	return strings.Join(quoted, ", ")
}

// buildRegimeGateConfirmMessage renders the confirm DM shown before the write.
// alsoActivated is the set of OTHER strategies whose already-configured but
// currently-dormant allowed_regimes gates would become live because this apply
// flips regime.enabled on (see regimeGateSideEffectStrategies); listing them lets
// the operator confirm the full blast radius rather than only the selected
// strategy. It is empty when regime detection is already enabled (no flip, no
// side effect), in which case no warning is appended.
func buildRegimeGateConfirmMessage(preset regimeGatePreset, targetID string, alsoActivated []string) string {
	msg := fmt.Sprintf("Apply regime gate **%s** (%s) to strategy `%s`?\nThis sets `regime_gate_window`=`%s` and `allowed_regimes`=%v, enables regime detection, and adds the `%s` window if missing. It only blocks ENTRIES outside the allowed regime — closes/management always run. Applied via a service restart.",
		preset.Name, preset.Label, targetID, preset.WindowKey, preset.AllowedRegimes, preset.WindowKey)
	if len(alsoActivated) > 0 {
		msg += fmt.Sprintf("\n\n⚠️ Enabling regime detection also activates previously-dormant `allowed_regimes` entry-gates on %d other strategy(ies) you did NOT select: %s. Regime detection is off today so those gates are no-ops, but they will gate new entries after the restart. Open positions are unaffected — management and closes always run; only fresh entries are gated. To leave one ungated, clear its `allowed_regimes` first.",
			len(alsoActivated), formatStrategyIDList(alsoActivated))
	}
	return msg
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

	// Enabling regime detection to wire the target's gate also activates any
	// OTHER strategy that already carries allowed_regimes but is dormant because
	// detection is currently off (config.go WARNs on exactly this state). Read
	// the on-disk config — the same source mutateConfig will operate on — and
	// surface that blast radius so the operator isn't silently changing an
	// unselected strategy's entry behavior. A read/parse failure here only drops
	// the advisory; the authoritative re-read + validation happens in the write.
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

	// Re-check flat: a position may have opened during the picker/confirm prompts.
	if d.ss.strategyHasOpenPosition(target.sc.ID) {
		followupText(s, i, fmt.Sprintf("Refused: strategy `%s` opened a position while confirming — cannot gate while open. Nothing changed.", target.sc.ID))
		return
	}

	// The blast-radius list the operator just confirmed was computed before the
	// 60s confirm wait; a concurrent config edit in that window (another Discord
	// session, or the /config web UI) can grow it past what was shown. Recompute
	// it fresh and refuse rather than silently writing a wider blast radius than
	// the operator agreed to — a shrunk or unchanged set is not a refusal reason
	// (see regimeGateBlastRadiusGrew).
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

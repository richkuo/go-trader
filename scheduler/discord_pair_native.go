package main

import (
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"
)

const pairNativeCommandName = "resolve-pair-native"

func pairNativeSlashCommand() *discordgo.ApplicationCommand {
	kinds := []*discordgo.ApplicationCommandOptionChoice{}
	for _, k := range []string{pairObligationNativeUnknown, pairObligationNativeUnconfirmed, pairObligationNativeConflict, pairObligationNativeAmbiguous, pairObligationNativeLegacy} {
		kinds = append(kinds, &discordgo.ApplicationCommandOptionChoice{Name: k, Value: k})
	}
	decisions := []*discordgo.ApplicationCommandOptionChoice{}
	for _, d := range []string{pairNativeDecisionShow, pairNativeDecisionRead, pairNativeDecisionWriteOff, pairNativeDecisionAck} {
		decisions = append(decisions, &discordgo.ApplicationCommandOptionChoice{Name: d, Value: d})
	}
	return &discordgo.ApplicationCommand{Name: commandPrefix + pairNativeCommandName, Description: "Review or resolve one unknown native pair close fill (owner DM only)", Contexts: dmContext(), Options: []*discordgo.ApplicationCommandOption{
		{Type: discordgo.ApplicationCommandOptionString, Name: "partition", Description: "Pair partition: live, paper or paper:<source id>", Required: true},
		{Type: discordgo.ApplicationCommandOptionString, Name: "pair", Description: "Pair id", Required: true},
		{Type: discordgo.ApplicationCommandOptionString, Name: "member", Description: "Pair member strategy id", Required: true},
		{Type: discordgo.ApplicationCommandOptionString, Name: "decision", Description: "show lists obligations and revisions; other choices change state (DM confirm)", Required: true, Choices: decisions},
		{Type: discordgo.ApplicationCommandOptionString, Name: "kind", Description: "Obligation kind", Choices: kinds},
		{Type: discordgo.ApplicationCommandOptionString, Name: "id", Description: "Obligation action, attempt or receipt id"},
		{Type: discordgo.ApplicationCommandOptionString, Name: "revision", Description: "Current obligation revision from decision show"},
		{Type: discordgo.ApplicationCommandOptionString, Name: "reason", Description: "Why you make this decision (required to change state)"},
		{Type: discordgo.ApplicationCommandOptionString, Name: "orders", Description: "venue-read only: comma-separated venue order ids of this close"},
	}}
}

func (d *DiscordNotifier) handleResolvePairNative(s discordTransport, i *discordgo.InteractionCreate, opts []*discordgo.ApplicationCommandInteractionDataOption) {
	deferAck(s, i)
	followupText(s, i, d.resolvePairNativeCommand(interactionUserID(i), i.GuildID, opts))
}

func pairNativeParseOrders(raw string) ([]string, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, ""
	}
	seen := map[string]bool{}
	var out []string
	for _, part := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' }) {
		if !pairPrintableASCII(part, pairEvidenceMaxIDLen) {
			return nil, fmt.Sprintf("order id %q is not 1..%d printable ASCII characters", part, pairEvidenceMaxIDLen)
		}
		if seen[part] {
			return nil, fmt.Sprintf("order id %s appears twice", part)
		}
		seen[part] = true
		out = append(out, part)
	}
	if len(out) > pairNativeHistoryMaxOrders {
		return nil, fmt.Sprintf("%d order ids exceed the %d a venue read checks", len(out), pairNativeHistoryMaxOrders)
	}
	return out, ""
}

func (d *DiscordNotifier) resolvePairNativeCommand(invokerID, guildID string, opts []*discordgo.ApplicationCommandInteractionDataOption) string {
	if ok, reason := authorizeCommand(pairNativeCommandName, invokerID, guildID, d.ownerID); !ok {
		return reason
	}
	if d.ss == nil || d.ss.state == nil || d.ss.mu == nil {
		return "status server not ready"
	}
	d.ss.mu.RLock()
	views := d.ss.state.pairViews
	d.ss.mu.RUnlock()
	router, ok := views.(pairNativeOperatorRouter)
	if !ok || router == nil {
		return "refused: no pair coordinator is running in this instance"
	}
	orders, why := pairNativeParseOrders(optionString(opts, "orders", ""))
	if why != "" {
		return "refused: " + why
	}
	cmd := pairNativeOperatorCommand{Invoker: invokerID, Partition: strings.TrimSpace(optionString(opts, "partition", "")), PairID: strings.TrimSpace(optionString(opts, "pair", "")),
		MemberID: strings.TrimSpace(optionString(opts, "member", "")), Kind: strings.TrimSpace(optionString(opts, "kind", "")), ObligationID: strings.TrimSpace(optionString(opts, "id", "")),
		Decision: strings.TrimSpace(optionString(opts, "decision", pairNativeDecisionShow)), Revision: strings.TrimSpace(optionString(opts, "revision", "")),
		Reason: strings.TrimSpace(optionString(opts, "reason", "")), Orders: orders}
	switch cmd.Decision {
	case pairNativeDecisionShow:
		msg, _ := router.pairNativeOperator(cmd, false)
		return msg
	case pairNativeDecisionRead, pairNativeDecisionWriteOff, pairNativeDecisionAck:
	default:
		return fmt.Sprintf("refused: decision %q is not show, %s, %s or %s", cmd.Decision, pairNativeDecisionRead, pairNativeDecisionWriteOff, pairNativeDecisionAck)
	}
	if cmd.Kind == "" || cmd.ObligationID == "" || cmd.Revision == "" {
		return "refused: a decision needs kind, id and the current revision (run it with decision show first)"
	}
	summary, ok := router.pairNativeOperator(cmd, false)
	if !ok {
		return summary
	}
	if !d.confirmDestructive(invokerID, "Confirm this native pair decision: "+summary) {
		return "Cancelled: no confirmation received. Nothing is staged and the obligation stays open."
	}
	msg, _ := router.pairNativeOperator(cmd, true)
	return msg
}

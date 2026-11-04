package main

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

const optionsStructureContractV1 = "options_structures_v1"

type optionsActionFields OptionsAction

func (a *OptionsAction) UnmarshalJSON(b []byte) error {
	var wire struct {
		optionsActionFields
		StructureID   json.RawMessage `json:"structure_id"`
		StructureLegs json.RawMessage `json:"structure_legs"`
	}
	if err := json.Unmarshal(b, &wire); err != nil {
		return err
	}
	*a = OptionsAction(wire.optionsActionFields)
	a.StructureID, a.StructureLegs, a.structureMetaErr = "", 0, ""
	var problems []string
	if id, err := decodeOptionsStructureID(wire.StructureID); err != nil {
		problems = append(problems, err.Error())
	} else {
		a.StructureID = id
	}
	if legs, err := decodeOptionsStructureLegs(wire.StructureLegs); err != nil {
		problems = append(problems, err.Error())
	} else {
		a.StructureLegs = legs
	}
	a.structureMetaErr = strings.Join(problems, "; ")
	return nil
}

func decodeOptionsStructureID(raw json.RawMessage) (string, error) {
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return "", fmt.Errorf("structure_id is missing")
	}
	if text[0] != '"' {
		return "", fmt.Errorf("structure_id is not a JSON string")
	}
	var id string
	if err := json.Unmarshal([]byte(text), &id); err != nil {
		return "", fmt.Errorf("structure_id is not a JSON string")
	}
	if strings.TrimSpace(id) == "" {
		return "", fmt.Errorf("structure_id is empty")
	}
	return id, nil
}

func decodeOptionsStructureLegs(raw json.RawMessage) (int, error) {
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return 0, fmt.Errorf("structure_legs is missing")
	}
	if c := text[0]; c != '-' && (c < '0' || c > '9') {
		return 0, fmt.Errorf("structure_legs is not a JSON number")
	}
	v, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v != math.Trunc(v) || v < 1 || v > math.MaxInt32 {
		return 0, fmt.Errorf("structure_legs %s is not a whole positive number", text)
	}
	return int(v), nil
}

type optionStructure struct {
	ID   string
	Legs []OptionsAction
}

type optionsStructureCheck struct {
	Actions    []OptionsAction
	Closes     []OptionsAction
	Structures []optionStructure
	Refusals   []string
}

func isOptionEntryAction(action string) bool {
	return action == "buy" || action == "sell"
}

func optionsActionLabel(i int, a OptionsAction) string {
	return fmt.Sprintf("action %d (%q)", i+1, a.Action)
}

func checkOptionsStructures(result *OptionsResult) optionsStructureCheck {
	var out optionsStructureCheck
	if result == nil {
		return out
	}
	type group struct {
		legs     []OptionsAction
		declared []int
		problems []string
	}
	groups := map[string]*group{}
	var order []string
	groupFor := func(id string) *group {
		g := groups[id]
		if g == nil {
			g = &group{}
			groups[id] = g
			order = append(order, id)
		}
		return g
	}
	var refuseAll []string
	entries := 0
	for i, a := range result.Actions {
		if a.Action == "close" {
			if a.StructureID != "" {
				g := groupFor(a.StructureID)
				g.problems = append(g.problems, optionsActionLabel(i, a)+" is a close inside an entry structure")
			}
			out.Closes = append(out.Closes, a)
			continue
		}
		entries++
		if a.StructureID == "" {
			reason := "has no structure_id"
			if a.structureMetaErr != "" {
				reason = a.structureMetaErr
			}
			refuseAll = append(refuseAll, optionsActionLabel(i, a)+" "+reason+", so no group can be identified")
			continue
		}
		g := groupFor(a.StructureID)
		switch {
		case !isOptionEntryAction(a.Action):
			g.problems = append(g.problems, optionsActionLabel(i, a)+" is not buy, sell or close")
			continue
		case a.OptionType != "call" && a.OptionType != "put":
			g.problems = append(g.problems, fmt.Sprintf("%s option_type %q is not call or put", optionsActionLabel(i, a), a.OptionType))
		case a.structureMetaErr != "":
			g.problems = append(g.problems, optionsActionLabel(i, a)+" "+a.structureMetaErr)
		case a.StructureLegs <= 0:
			g.problems = append(g.problems, fmt.Sprintf("%s structure_legs %d is not a whole positive number", optionsActionLabel(i, a), a.StructureLegs))
		}
		g.legs = append(g.legs, a)
		g.declared = append(g.declared, a.StructureLegs)
	}
	if entries > 0 && result.StructureContract != optionsStructureContractV1 {
		refuseAll = append([]string{fmt.Sprintf("structure_contract %q, want %q", result.StructureContract, optionsStructureContractV1)}, refuseAll...)
	}
	if len(refuseAll) > 0 {
		out.Refusals = append(out.Refusals, fmt.Sprintf("every entry refused (%d action(s)): %s", entries, strings.Join(refuseAll, "; ")))
		out.Actions = append(out.Actions, out.Closes...)
		return out
	}
	valid := map[string]bool{}
	for _, id := range order {
		g := groups[id]
		if len(g.problems) == 0 {
			for _, d := range g.declared {
				if d != len(g.legs) {
					g.problems = append(g.problems, fmt.Sprintf("structure_legs %d does not match its %d member(s)", d, len(g.legs)))
					break
				}
			}
		}
		if len(g.problems) > 0 {
			out.Refusals = append(out.Refusals, fmt.Sprintf("structure %q refused (%d leg(s)): %s", id, len(g.legs), strings.Join(g.problems, "; ")))
			continue
		}
		valid[id] = true
		out.Structures = append(out.Structures, optionStructure{ID: id, Legs: g.legs})
	}
	for _, a := range result.Actions {
		if a.Action == "close" || valid[a.StructureID] {
			out.Actions = append(out.Actions, a)
		}
	}
	return out
}

package main

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

func ocSC(id, platform, underlying string) StrategyConfig {
	return StrategyConfig{ID: id, Type: "options", Platform: platform, Script: "shared_scripts/check_options.py",
		Args: []string{"wheel", underlying, "--platform=" + platform, "--mode=paper"}, Capital: 100000, MaxDrawdownPct: 50}
}

func ocLogger(id string) (*StrategyLogger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return &StrategyLogger{stratID: id, writer: buf}, buf
}

const ocExpiry = "2026-12-18"

func ocStructure(id string, legs ...OptionsAction) []OptionsAction {
	out := append([]OptionsAction(nil), legs...)
	for i := range out {
		out[i].StructureID, out[i].StructureLegs = id, len(legs)
	}
	return out
}

func ocRaw(platform, underlying string, spot float64, actions ...OptionsAction) *OptionsResult {
	return &OptionsResult{Underlying: underlying, Platform: platform, PremiumUnit: optionPremiumUnitUnderlying,
		StructureContract: optionsStructureContractV1, Signal: 1, SpotPrice: spot, Actions: actions}
}

func TestOptionStructureEntryExposureCapAndHoldsDropWholeStructures(t *testing.T) {
	sc := ocSC("ib-opt-filter", "ibkr", "BTC")
	butterfly := ocStructure("fly",
		OptionsAction{Action: "buy", OptionType: "call", Strike: 95, Expiry: ocExpiry, PremiumUSD: 6, Greeks: OptGreeks{Delta: 0.7}},
		OptionsAction{Action: "sell", OptionType: "call", Strike: 100, Expiry: ocExpiry, PremiumUSD: 3, Quantity: 2, Greeks: OptGreeks{Delta: 0.5}},
		OptionsAction{Action: "buy", OptionType: "call", Strike: 105, Expiry: ocExpiry, PremiumUSD: 1, Greeks: OptGreeks{Delta: 0.3}})
	shortOnly := ocStructure("short", OptionsAction{Action: "sell", OptionType: "call", Strike: 140, Expiry: ocExpiry, PremiumUSD: 1, Greeks: OptGreeks{Delta: 0.2}})
	closeAction := OptionsAction{Action: "close", OptionType: "put", Strike: 80, PremiumUSD: 1}
	build := func() *OptionsResult {
		actions := append(append(append([]OptionsAction(nil), butterfly...), shortOnly...), closeAction)
		return ocRaw("ibkr", "BTC", 100, actions...)
	}
	names := func(actions []OptionsAction) []string {
		var out []string
		for _, a := range actions {
			out = append(out, a.Action+":"+a.StructureID)
		}
		return out
	}
	logger, buf := ocLogger(sc.ID)
	r := build()
	filterOptionsEntryActions(optionsDispatch{SC: sc, Logger: logger, Risk: &scopeCycleRisk{Partition: partitionFor(sc),
		ExposureCapStatus: ExposureCapStatus{Configured: true, CapUSD: 1000, LongUSD: 2000, LongBlocked: true}}}, r)
	if got := names(r.Actions); !reflect.DeepEqual(got, []string{"sell:short", "close:"}) {
		t.Fatalf("long block kept %v; log:\n%s", got, buf.String())
	}
	if !strings.Contains(buf.String(), "Exposure cap: 3 option open action(s) in 1 structure(s) dropped whole") {
		t.Fatalf("whole-structure exposure drop not logged:\n%s", buf.String())
	}
	for _, hold := range []struct {
		name string
		d    optionsDispatch
	}{
		{"paused", optionsDispatch{SC: func() StrategyConfig { c := sc; c.Paused = true; return c }(), Risk: &scopeCycleRisk{}}},
		{"daily_loss", optionsDispatch{SC: sc, Risk: &scopeCycleRisk{DailyLossEntriesHeld: true}}},
		{"notional", optionsDispatch{SC: sc, Risk: &scopeCycleRisk{NotionalBlocked: true}}},
		{"persistence", optionsDispatch{SC: sc, Risk: &scopeCycleRisk{}, PersistenceHold: true}},
	} {
		hold.d.Logger = logger
		r := build()
		filterOptionsEntryActions(hold.d, r)
		if got := names(r.Actions); !reflect.DeepEqual(got, []string{"close:"}) {
			t.Fatalf("%s hold kept %v", hold.name, got)
		}
	}
}

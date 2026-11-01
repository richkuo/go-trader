package main

import (
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func pairEnterHoldRunner(e *pairTestEnv, hlQty float64) {
	e.runner.respond = func(req pairPolicyRequest) (pairPolicyResult, error) {
		if req.Lifecycle == pairLifecycleMatched {
			return pairHoldResult(req, "keep the spread"), nil
		}
		return pairEnterResult(req, hlQty), nil
	}
}

func pairCostLogger(id string) *StrategyLogger {
	return &StrategyLogger{stratID: id, writer: io.Discard}
}

func pairCostNear(a, b float64) bool {
	return math.Abs(a-b) <= 1e-9*math.Max(1, math.Abs(b))
}

func TestPairOrdinaryRoutesRefuseActivation(t *testing.T) {
	registerPairTestPolicy(t)
	variants := map[string]func(root map[string]interface{}){
		"root pairs": nil,
		"member pair_id only": func(r map[string]interface{}) {
			delete(r, "pairs")
		},
		"pair_member token only": func(r map[string]interface{}) {
			delete(r, "pairs")
			for _, id := range []string{pairTestHL, pairTestOKX} {
				delete(pairTestStrategy(r, id), "pair_id")
			}
		},
		"pair flag only": func(r map[string]interface{}) {
			delete(r, "pairs")
			for _, id := range []string{pairTestHL, pairTestOKX} {
				sc := pairTestStrategy(r, id)
				delete(sc, "pair_id")
				sc["args"] = []interface{}{"sma_crossover", "BTC", "1h", "--mode=paper", pairMemberFlag + "={}"}
			}
		},
		"paper with acknowledgement and an old config version": func(r map[string]interface{}) {
			r["config_version"] = 12
			for _, id := range []string{pairTestHL, pairTestOKX} {
				pairTestStrategy(r, id)["allow_no_edge"] = true
			}
		},
	}
	for name, mutate := range variants {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			doc := pairTestDoc(t, dir, mutate)
			path := filepath.Join(dir, "config.json")
			if err := os.WriteFile(path, doc, 0o600); err != nil {
				t.Fatal(err)
			}
			before := dirDigest(t, dir)
			loaders := map[string]func() error{
				"LoadConfig":                func() error { _, err := LoadConfig(path); return err },
				"LoadConfigReadOnly":        func() error { _, err := LoadConfigReadOnly(path); return err },
				"LoadConfigForProbe":        func() error { _, err := LoadConfigForProbe(path); return err },
				"LoadConfigForLedgerExport": func() error { _, err := LoadConfigForLedgerExport(doc); return err },
				"forged admission": func() error {
					_, err := loadConfigDataWith("", doc, true, true, &pairResearchAdmission{nonce: "forged"})
					return err
				},
			}
			for lname, load := range loaders {
				if err := load(); err == nil {
					t.Fatalf("%s activated a pair reference", lname)
				}
			}
			if code := runProbe([]string{"--config", path}); code == 0 {
				t.Fatal("the probe command admitted a pair reference")
			}
			if after := dirDigest(t, dir); after != before {
				t.Fatal("an ordinary route changed the config or state files while refusing a pair")
			}
		})
	}
	if _, _, err := buildAddStrategyEntry(pairMemberStrategyToken, "hyperliquid", "BTC"); err == nil {
		t.Fatal("the operator add-strategy route accepted the pair member token")
	}
}

func TestPairOrdinaryLoadRefusesResearchRecords(t *testing.T) {
	e := newPairTestEnv(t, nil)
	pairEnterHoldRunner(e, 0.06)
	e.slotStep(0)
	e.store.Close()
	dbPath := e.cfg.DBFile
	before := pairFileDigest(t, dbPath)
	ordinary := loadBasketTestConfig(t, func(root map[string]interface{}) {
		root["db_file"] = dbPath
		delete(root, "baskets")
		var kept []interface{}
		for _, raw := range root["strategies"].([]interface{}) {
			sc := raw.(map[string]interface{})
			delete(sc, "basket_id")
			sc["args"] = []interface{}{"sma_crossover", sc["args"].([]interface{})[1], "1h", "--mode=paper"}
			delete(sc, "closed_bar_decisions")
			kept = append(kept, sc)
		}
		root["strategies"] = kept
	})
	store := openSourceStore(t, ordinary)
	state := NewAppState()
	for _, sc := range ordinary.Strategies {
		state.Strategies[sc.ID] = NewStrategyState(sc)
	}
	if _, err := store.LoadBaskets(state, ordinary); err == nil {
		t.Fatal("an ordinary load adopted research pair records")
	}
	var n int
	if err := store.primary().db.QueryRow(`SELECT COUNT(*) FROM basket_member_actions WHERE group_mode = 'pair'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("the refused load changed the pair records: %d (%v)", n, err)
	}
	store.Close()
	_ = before
}

func TestPairCostOrdinaryCallersKeepTheirCosts(t *testing.T) {
	logger := pairCostLogger("plain")
	for _, c := range []struct {
		platform string
		fee      float64
	}{{"okx", OKXPerpsTakerFeePct}, {"hyperliquid", HyperliquidTakerFeePct}} {
		s := NewStrategyState(StrategyConfig{ID: "plain-" + c.platform, Platform: c.platform, Type: "perps", Capital: 1000})
		if _, err := ExecutePerpsSignalWithLeverage(s, 1, "BTC", 60000, PerpsSizing{SizingLeverage: 1}, 0, "", 0, DirectionLong, 0, logger); err != nil {
			t.Fatal(err)
		}
		pos := s.Positions["BTC"]
		px := ApplyAdverseSlippage(60000, true)
		if pos == nil || pos.AvgCost != px {
			t.Fatalf("%s: an ordinary open must keep the process slippage: %+v", c.platform, pos)
		}
		if _, err := ExecutePerpsSignalWithLeverage(s, -1, "BTC", 61000, PerpsSizing{SizingLeverage: 1}, 0, "", 0, DirectionLong, 0, logger); err != nil {
			t.Fatal(err)
		}
		for _, tr := range s.TradeHistory {
			if tr.CostModelVersion != FillCostModelVersion || tr.CostPolicyID != "" {
				t.Fatalf("%s: an ordinary row %+v must keep the process stamp", c.platform, tr)
			}
			if !pairCostNear(tr.ExchangeFee, tr.Quantity*tr.Price*c.fee) {
				t.Fatalf("%s: an ordinary row %+v must keep the venue fee rate %v", c.platform, tr, c.fee)
			}
		}
		if len(s.pairCostFaults) != 0 {
			t.Fatalf("an ordinary strategy raised a pair cost fault: %v", s.pairCostFaults)
		}
	}
}

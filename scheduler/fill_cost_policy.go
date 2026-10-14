package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sync"
)

const (
	FillCostModelReplayCustomVersion = 1001

	fillCostRoleTakerAll       = "taker_all"
	fillCostSpreadInSlippage   = "inside_slippage"
	fillCostPolicyMaxRatePct   = 0.05
	fillCostPolicyEvidenceVers = 1
)

type fillCostPolicy struct {
	ModelVersion int     `json:"model_version"`
	PolicyID     string  `json:"policy_id,omitempty"`
	TakerFeePct  float64 `json:"taker_fee_pct"`
	SlippagePct  float64 `json:"slippage_pct"`
	FillRole     string  `json:"fill_role"`
	Spread       string  `json:"spread"`
}

type fillCostPolicyEvidence struct {
	Version        int            `json:"v"`
	Policy         fillCostPolicy `json:"policy"`
	DefaultPolicy  fillCostPolicy `json:"default_policy"`
	RunFingerprint string         `json:"run_fingerprint"`
}

func defaultFillCostPolicy() fillCostPolicy {
	return fillCostPolicy{
		ModelVersion: FillCostModelVersion,
		TakerFeePct:  HyperliquidTakerFeePct,
		SlippagePct:  SlippagePct,
		FillRole:     fillCostRoleTakerAll,
		Spread:       fillCostSpreadInSlippage,
	}
}

var (
	fillCostMu     sync.RWMutex
	fillCostBound  *fillCostPolicy
	fillCostBinder string
)

func activeFillCostPolicy() fillCostPolicy {
	fillCostMu.RLock()
	defer fillCostMu.RUnlock()
	if fillCostBound != nil {
		return *fillCostBound
	}
	return defaultFillCostPolicy()
}

func (p fillCostPolicy) rates() fillCostPolicy {
	return fillCostPolicy{TakerFeePct: p.TakerFeePct, SlippagePct: p.SlippagePct, FillRole: p.FillRole, Spread: p.Spread}
}

func (p fillCostPolicy) isDefault() bool {
	return p.rates() == defaultFillCostPolicy().rates()
}

func validateFillCostRates(p fillCostPolicy) error {
	for _, v := range []struct {
		name string
		val  float64
	}{{"taker_fee_pct", p.TakerFeePct}, {"slippage_pct", p.SlippagePct}} {
		if math.IsNaN(v.val) || math.IsInf(v.val, 0) || v.val < 0 || v.val > fillCostPolicyMaxRatePct {
			return fmt.Errorf("cost policy %s %v must be a finite rate in [0, %v]", v.name, v.val, fillCostPolicyMaxRatePct)
		}
	}
	if p.FillRole != fillCostRoleTakerAll {
		return fmt.Errorf("cost policy fill_role %q is not supported; paper settlement books every fill as a taker fill (%q)", p.FillRole, fillCostRoleTakerAll)
	}
	if p.Spread != fillCostSpreadInSlippage {
		return fmt.Errorf("cost policy spread %q is not supported; spread is modeled inside slippage (%q)", p.Spread, fillCostSpreadInSlippage)
	}
	return nil
}

func resolveFillCostPolicy(p fillCostPolicy, runFingerprint string) (fillCostPolicy, error) {
	if err := validateFillCostRates(p); err != nil {
		return fillCostPolicy{}, err
	}
	if p.isDefault() {
		return defaultFillCostPolicy(), nil
	}
	out := p.rates()
	out.ModelVersion = FillCostModelReplayCustomVersion
	blob, err := json.Marshal(struct {
		Rates          fillCostPolicy `json:"rates"`
		ModelVersion   int            `json:"model_version"`
		RunFingerprint string         `json:"run_fingerprint"`
	}{out.rates(), out.ModelVersion, runFingerprint})
	if err != nil {
		return fillCostPolicy{}, err
	}
	sum := sha256.Sum256(blob)
	out.PolicyID = "replay-cost-" + hex.EncodeToString(sum[:16])
	return out, nil
}

func bindFillCostPolicy(owner string, p fillCostPolicy) error {
	if owner == "" {
		return fmt.Errorf("a cost policy binding needs an owner")
	}
	if !p.isDefault() {
		if p.ModelVersion != FillCostModelReplayCustomVersion || p.PolicyID == "" {
			return fmt.Errorf("a non-default cost policy needs model version %d and a policy id", FillCostModelReplayCustomVersion)
		}
	} else if p.ModelVersion != FillCostModelVersion || p.PolicyID != "" {
		return fmt.Errorf("the default cost policy keeps model version %d and no policy id", FillCostModelVersion)
	}
	if err := validateFillCostRates(p); err != nil {
		return err
	}
	fillCostMu.Lock()
	defer fillCostMu.Unlock()
	if fillCostBound != nil {
		return fmt.Errorf("the fill cost policy is already bound to %s", fillCostBinder)
	}
	cp := p
	fillCostBound, fillCostBinder = &cp, owner
	return nil
}

func unbindFillCostPolicy(owner string) {
	fillCostMu.Lock()
	defer fillCostMu.Unlock()
	if fillCostBinder == owner {
		fillCostBound, fillCostBinder = nil, ""
	}
}

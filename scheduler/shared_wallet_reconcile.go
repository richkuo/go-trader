package main

import (
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
)


type SharedWalletPosition struct {
	Coin          string
	UnrealizedPnL float64
}

type sharedWalletReconcileResult struct {
	Values map[string]float64
	Drift float64
	OrphanCoins []string
}

func reconcileSharedWalletMemberValues(
	members []string,
	capitalByID map[string]float64,
	positions []SharedWalletPosition,
	virtualQty map[string]map[string]float64,
	accountBalance float64,
) sharedWalletReconcileResult {
	values := make(map[string]float64, len(members))
	if len(members) == 0 {
		return sharedWalletReconcileResult{Values: values, Drift: accountBalance}
	}

	weights := make(map[string]float64, len(members))
	capitalSum := 0.0
	for _, id := range members {
		c := capitalByID[id]
		if c > 0 {
			capitalSum += c
		}
	}
	if capitalSum > 0 {
		for _, id := range members {
			c := capitalByID[id]
			if c > 0 {
				weights[id] = c / capitalSum
			} else {
				weights[id] = 0
			}
		}
	} else {
		eq := 1.0 / float64(len(members))
		for _, id := range members {
			weights[id] = eq
		}
	}

	totalUPnL := 0.0
	uPnLByCoin := make(map[string]float64)
	for _, p := range positions {
		totalUPnL += p.UnrealizedPnL
		uPnLByCoin[p.Coin] += p.UnrealizedPnL
	}

	base := accountBalance - totalUPnL

	memberSet := make(map[string]bool, len(members))
	for _, id := range members {
		memberSet[id] = true
	}
	ownedUPnL, _, orphanCoins := attributeSharedWalletUPnL(memberSet, uPnLByCoin, virtualQty)

	raw := make(map[string]float64, len(members))
	rawSum := 0.0
	for _, id := range members {
		v := weights[id]*base + ownedUPnL[id]
		raw[id] = v
		rawSum += v
	}
	drift := accountBalance - rawSum

	ordered := append([]string(nil), members...)
	sort.Strings(ordered)
	roundedSum := 0.0
	for _, id := range ordered {
		rv := roundCents(raw[id])
		values[id] = rv
		roundedSum += rv
	}
	if len(ordered) > 0 {
		residual := roundCents(rawSum) - roundCents(roundedSum)
		if residual != 0 {
			last := ordered[len(ordered)-1]
			values[last] = roundCents(values[last] + residual)
		}
	}

	return sharedWalletReconcileResult{Values: values, Drift: drift, OrphanCoins: orphanCoins}
}

func attributeSharedWalletUPnL(
	memberSet map[string]bool,
	uPnLByCoin map[string]float64,
	virtualQty map[string]map[string]float64,
) (ownedUPnL map[string]float64, attributedUPnL float64, orphanCoins []string) {
	ownedUPnL = make(map[string]float64, len(memberSet))
	coins := make([]string, 0, len(uPnLByCoin))
	for coin := range uPnLByCoin {
		coins = append(coins, coin)
	}
	sort.Strings(coins)
	for _, coin := range coins {
		pnl := uPnLByCoin[coin]
		owners := virtualQty[coin]
		if len(owners) == 0 {
			orphanCoins = append(orphanCoins, coin)
			continue
		}
		sumQty := 0.0
		for id, qty := range owners {
			if memberSet[id] && qty > 0 {
				sumQty += qty
			}
		}
		if sumQty <= 0 {
			orphanCoins = append(orphanCoins, coin)
			continue
		}
		for id, qty := range owners {
			if !memberSet[id] || qty <= 0 {
				continue
			}
			share := (qty / sumQty) * pnl
			ownedUPnL[id] += share
			attributedUPnL += share
		}
	}
	return ownedUPnL, attributedUPnL, orphanCoins
}

type ledgerWalletInputs struct {
	Members     []string
	InitialByID map[string]float64
	LedgerByID  map[string]float64
	Positions   []SharedWalletPosition
	VirtualQty  map[string]map[string]float64
	AccountBalance float64
	NonTradeFlows float64
	BaselineOffset float64
	BaselineSet    bool
}

func ledgerSharedWalletMemberValues(in ledgerWalletInputs) (sharedWalletReconcileResult, float64) {
	values := make(map[string]float64, len(in.Members))
	memberSet := make(map[string]bool, len(in.Members))
	for _, id := range in.Members {
		memberSet[id] = true
	}

	uPnLByCoin := make(map[string]float64)
	for _, p := range in.Positions {
		uPnLByCoin[p.Coin] += p.UnrealizedPnL
	}
	ownedUPnL, _, orphanCoins := attributeSharedWalletUPnL(memberSet, uPnLByCoin, in.VirtualQty)

	rawSum := 0.0
	ordered := append([]string(nil), in.Members...)
	sort.Strings(ordered)
	for _, id := range ordered {
		v := in.InitialByID[id] + in.LedgerByID[id] + ownedUPnL[id]
		rawSum += v
		values[id] = roundCents(v)
	}

	rawDrift := in.AccountBalance - rawSum - in.NonTradeFlows
	drift := rawDrift
	if in.BaselineSet {
		drift = rawDrift - in.BaselineOffset
	} else {
		drift = 0
	}
	return sharedWalletReconcileResult{Values: values, Drift: drift, OrphanCoins: orphanCoins}, rawDrift
}

func roundCents(v float64) float64 {
	return math.Round(v*100) / 100
}

type sharedWalletDriftResult struct {
	Key         SharedWalletKey
	Drift       float64
	Balance     float64
	MemberSum   float64
	OrphanCoins []string
	Basis string
	ExpectedEquity float64
	JournalPending bool
}

func reconcileSharedWalletDisplayValues(
	strategies []StrategyConfig,
	state *AppState,
	sdb *StateDB,
	sharedWallets map[SharedWalletKey][]string,
	walletBalances map[SharedWalletKey]float64,
	hlPositions []HLPosition,
	okxPositions []OKXPosition,
	okxPositionsFetched bool,
) []sharedWalletDriftResult {
	for _, ss := range state.Strategies {
		if ss != nil {
			ss.SharedWalletValueSet = false
			ss.SharedWalletPerformanceOnly = false
		}
	}
	for _, sc := range strategies {
		if ss := state.Strategies[sc.ID]; ss != nil {
			ss.SharedWalletPerformanceOnly = effectiveSharedWalletPoolBook(sc, ss)
		}
	}
	state.LatestSharedWalletBalances = make(map[SharedWalletKey]float64)
	state.LatestSharedWalletMembers = make(map[SharedWalletKey][]string)
	if len(sharedWallets) == 0 {
		return nil
	}

	byID := make(map[string]StrategyConfig, len(strategies))
	for _, sc := range strategies {
		byID[sc.ID] = sc
	}

	var results []sharedWalletDriftResult
	for key, memberIDs := range sharedWallets {
		bal, ok := walletBalances[key]
		if !ok {
			continue
		}

		members := sharedWalletMembersWithManual(key, memberIDs, strategies)
		state.LatestSharedWalletBalances[key] = bal
		state.LatestSharedWalletMembers[key] = append([]string(nil), members...)

		var positions []SharedWalletPosition
		switch key.Platform {
		case "hyperliquid":
			for _, p := range hlPositions {
				if p.Size == 0 {
					continue
				}
				positions = append(positions, SharedWalletPosition{
					Coin:          strings.ToUpper(strings.TrimSpace(p.Coin)),
					UnrealizedPnL: p.UnrealizedPnL,
				})
			}
		case "okx":
			if !okxPositionsFetched {
				continue
			}
			for _, p := range okxPositions {
				if p.Size == 0 {
					continue
				}
				positions = append(positions, SharedWalletPosition{
					Coin:          strings.ToUpper(strings.TrimSpace(p.Coin)),
					UnrealizedPnL: p.UnrealizedPnL,
				})
			}
		default:
			continue
		}

		capitalByID, virtualQty := buildSharedWalletBooks(key, members, byID, state)

		poolMode := false
		for _, id := range memberIDs {
			if effectiveSharedWalletPoolBook(byID[id], state.Strategies[id]) {
				poolMode = true
				break
			}
		}

		var res sharedWalletReconcileResult
		if poolMode {
			var reconciled bool
			res, reconciled = reconcilePooledWalletPerformance(sdb, key, members, positions, virtualQty)
			if !reconciled {
				continue
			}
		} else if key.Platform == "hyperliquid" {
			res = reconcileHLWalletViaLedger(sdb, key, members, capitalByID, positions, virtualQty, bal)
		} else {
			res = reconcileSharedWalletMemberValues(members, capitalByID, positions, virtualQty, bal)
		}
		memberSum := 0.0
		for _, id := range members {
			ss := state.Strategies[id]
			if ss == nil {
				continue
			}
			ss.SharedWalletValue = res.Values[id]
			ss.SharedWalletValueSet = true
			ss.SharedWalletPerformanceOnly = poolMode
			memberSum += res.Values[id]
		}
		results = append(results, sharedWalletDriftResult{
			Key:         key,
			Drift:       res.Drift,
			Balance:     bal,
			MemberSum:   roundCents(memberSum),
			OrphanCoins: res.OrphanCoins,
		})
	}
	return results
}

func reconcilePooledWalletPerformance(
	sdb *StateDB,
	key SharedWalletKey,
	members []string,
	positions []SharedWalletPosition,
	virtualQty map[string]map[string]float64,
) (sharedWalletReconcileResult, bool) {
	if sdb == nil {
		fmt.Printf("[WARN] shared-wallet %s: pooled performance unavailable (state db nil) — using modeled strategy books this cycle\n",
			sharedWalletKeyLabel(key))
		return sharedWalletReconcileResult{}, false
	}
	ledgerByID, err := sdb.LedgerNetByStrategy(members)
	if err != nil {
		fmt.Printf("[WARN] shared-wallet %s: pooled performance ledger unavailable: %v — using modeled strategy books this cycle\n",
			sharedWalletKeyLabel(key), err)
		return sharedWalletReconcileResult{}, false
	}

	memberSet := make(map[string]bool, len(members))
	for _, id := range members {
		memberSet[id] = true
	}
	uPnLByCoin := make(map[string]float64)
	for _, pos := range positions {
		uPnLByCoin[pos.Coin] += pos.UnrealizedPnL
	}
	ownedUPnL, _, orphanCoins := attributeSharedWalletUPnL(memberSet, uPnLByCoin, virtualQty)

	values := make(map[string]float64, len(members))
	for _, id := range members {
		values[id] = roundCents(ledgerByID[id] + ownedUPnL[id])
	}
	orphanDrift := 0.0
	for _, coin := range orphanCoins {
		orphanDrift += uPnLByCoin[coin]
	}
	return sharedWalletReconcileResult{
		Values:      values,
		Drift:       orphanDrift,
		OrphanCoins: orphanCoins,
	}, true
}

func sharedWalletMembersWithManual(key SharedWalletKey, memberIDs []string, strategies []StrategyConfig) []string {
	manualIDs := sameAccountLiveManualMembers(key, strategies)
	if len(manualIDs) == 0 {
		return memberIDs
	}
	seen := make(map[string]bool, len(memberIDs))
	for _, id := range memberIDs {
		seen[id] = true
	}
	members := append([]string(nil), memberIDs...)
	for _, id := range manualIDs {
		if !seen[id] {
			members = append(members, id)
		}
	}
	return members
}

func buildSharedWalletBooks(
	key SharedWalletKey,
	members []string,
	byID map[string]StrategyConfig,
	state *AppState,
) (map[string]float64, map[string]map[string]float64) {
	capitalByID := make(map[string]float64, len(members))
	virtualQty := make(map[string]map[string]float64)
	for _, id := range members {
		sc, ok := byID[id]
		if !ok {
			continue
		}
		ss := state.Strategies[id]
		capitalByID[id] = EffectiveInitialCapital(sc, ss)
		if ss == nil {
			continue
		}
		var posKey string
		switch key.Platform {
		case "hyperliquid":
			if sc.Type == "manual" {
				posKey = sc.Symbol
			} else {
				posKey = hyperliquidSymbol(sc.Args)
			}
		case "okx":
			posKey = okxSymbol(sc.Args)
		}
		if posKey == "" {
			continue
		}
		coin := strings.ToUpper(strings.TrimSpace(posKey))
		if pos, pok := ss.Positions[posKey]; pok && pos != nil && pos.Quantity > 0 {
			if virtualQty[coin] == nil {
				virtualQty[coin] = make(map[string]float64)
			}
			virtualQty[coin][id] = pos.Quantity
		}
		if key.Platform == "hyperliquid" {
			if hCoin := hedgeCoin(sc); hCoin != "" {
				if hPos, hok := ss.Positions[hCoin]; hok && hPos != nil && hPos.isHedgeLeg() && hPos.Quantity > 0 {
					if virtualQty[hCoin] == nil {
						virtualQty[hCoin] = make(map[string]float64)
					}
					virtualQty[hCoin][id] = hPos.Quantity
				}
			}
		}
	}
	return capitalByID, virtualQty
}

func reconcileHLWalletViaLedger(
	sdb *StateDB,
	key SharedWalletKey,
	members []string,
	capitalByID map[string]float64,
	positions []SharedWalletPosition,
	virtualQty map[string]map[string]float64,
	bal float64,
) sharedWalletReconcileResult {
	fallback := func(why string, err error) sharedWalletReconcileResult {
		fmt.Printf("[WARN] shared-wallet %s: ledger display path unavailable (%s: %v) — capital-weight split fallback this cycle\n",
			sharedWalletKeyLabel(key), why, err)
		return reconcileSharedWalletMemberValues(members, capitalByID, positions, virtualQty, bal)
	}
	if sdb == nil {
		return fallback("no state db", fmt.Errorf("sdb nil"))
	}
	ledgerByID, err := sdb.LedgerNetByStrategy(members)
	if err != nil {
		return fallback("ledger sums", err)
	}
	flows, err := sdb.SumWalletTransfers(key.Platform, key.Account)
	if err != nil {
		return fallback("transfer sum", err)
	}
	st, found, err := sdb.GetWalletLedgerState(key.Platform, key.Account)
	if err != nil {
		return fallback("ledger state", err)
	}
	if !found {
		return fallback("ledger state", fmt.Errorf("watermark row not initialized"))
	}
	res, rawDrift := ledgerSharedWalletMemberValues(ledgerWalletInputs{
		Members:        members,
		InitialByID:    capitalByID,
		LedgerByID:     ledgerByID,
		Positions:      positions,
		VirtualQty:     virtualQty,
		AccountBalance: bal,
		NonTradeFlows:  flows,
		BaselineOffset: st.BaselineOffset,
		BaselineSet:    st.BaselineSet,
	})
	if !st.BaselineSet {
		st.BaselineOffset = rawDrift
		st.BaselineSet = true
		if err := sdb.UpsertWalletLedgerState(key.Platform, key.Account, st); err != nil {
			fmt.Printf("[WARN] shared-wallet %s: baseline store failed: %v — will recompute next cycle\n",
				sharedWalletKeyLabel(key), err)
		} else {
			fmt.Printf("[shared-wallet] %s: ledger drift baseline set to $%+.2f (balance $%.2f vs ledger-derived $%.2f + flows $%.2f)\n",
				sharedWalletKeyLabel(key), rawDrift, bal, bal-rawDrift-flows, flows)
		}
	}
	return res
}

func displayStrategyValue(s *StrategyState, prices map[string]float64) float64 {
	if s != nil && s.SharedWalletValueSet {
		return s.SharedWalletValue
	}
	return PortfolioValue(s, prices)
}

func latestDisplayTotal(state *AppState, prices map[string]float64) float64 {
	if state == nil {
		return 0
	}
	deduped := make(map[string]bool)
	total := 0.0
	for key, balance := range state.LatestSharedWalletBalances {
		total += balance
		for _, id := range state.LatestSharedWalletMembers[key] {
			deduped[id] = true
		}
	}
	for id, ss := range state.Strategies {
		if !deduped[id] {
			total += displayStrategyValue(ss, prices)
		}
	}
	return total
}

func computeSubsetDisplayValue(
	subset []StrategyConfig,
	state *AppState,
	prices map[string]float64,
	walletBalances map[SharedWalletKey]float64,
	accountShared map[SharedWalletKey][]string,
) (float64, bool) {
	gated := 0.0
	var rest []StrategyConfig
	for _, sc := range subset {
		if s, ok := state.Strategies[sc.ID]; ok && s != nil && s.SharedWalletValueSet && !s.SharedWalletPerformanceOnly {
			gated += s.SharedWalletValue
			continue
		}
		rest = append(rest, sc)
	}
	if len(rest) == 0 {
		return gated, false
	}
	restVal, usedFallback := computeSubsetPortfolioValue(rest, state, prices, walletBalances, accountShared)
	return gated + restVal, usedFallback
}

func dedupedSameAccountLiveManualIDs(strategies []StrategyConfig) map[string]bool {
	out := make(map[string]bool)
	for key := range detectSharedWallets(strategies) {
		for _, id := range sameAccountLiveManualMembers(key, strategies) {
			out[id] = true
		}
	}
	return out
}

func riskPathWalletMemberIDs(key SharedWalletKey, perpsMemberIDs []string, subset []StrategyConfig) []string {
	members := append([]string(nil), perpsMemberIDs...)
	seen := make(map[string]bool, len(members))
	for _, id := range members {
		seen[id] = true
	}
	for _, id := range sameAccountLiveManualMembers(key, subset) {
		if !seen[id] {
			members = append(members, id)
			seen[id] = true
		}
	}
	return members
}

func sameAccountLiveManualMembers(key SharedWalletKey, strategies []StrategyConfig) []string {
	if key.Platform != "hyperliquid" {
		return nil
	}
	if key.Account == "" || os.Getenv("HYPERLIQUID_ACCOUNT_ADDRESS") != key.Account {
		return nil
	}
	var out []string
	for _, sc := range strategies {
		if sc.Platform == "hyperliquid" && sc.Type == "manual" && hyperliquidIsLive(sc.Args) {
			out = append(out, sc.ID)
		}
	}
	return out
}

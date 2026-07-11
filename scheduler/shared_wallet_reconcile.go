package main

import (
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
)

// Exchange-authoritative per-strategy reconciliation for shared wallets (#918).
//
// Multiple live strategies on one on-exchange account (Hyperliquid, OKX) draw
// from a single pool of cash and a single set of on-chain positions. Each
// strategy keeps its own *modeled* virtual book (StrategyState.Cash + modeled
// position P&L), but that book is a forecast: modeled fees, assumed fill
// prices, ignored funding, and a stale mark all make it drift from the real
// account. Summed across members, the modeled books do not equal the real
// balance.
//
// Instead of modeling more carefully, we READ the real values each cycle and
// split them so the per-strategy display values sum EXACTLY to the real
// account balance:
//
//	value_i = w_i * (accountBalance - U) + ownedUPnL_i
//
// where
//   - U          = Σ exchange-reported unrealized P&L across the wallet's
//     on-chain positions,
//   - w_i        = member i's configured-capital weight (Σ w_i = 1) — the
//     operator-set starting allocation, used to divide the shared collateral
//     base that is genuinely pooled (no per-strategy owner on-exchange),
//   - ownedUPnL_i = the real unrealized P&L of the positions member i owns;
//     a position shared by several co-owning peers on the same coin is split
//     by virtual-quantity share (mirrors hyperliquidKillSwitchFillShare).
//
// Σ value_i = (accountBalance - U) + Σ ownedUPnL_i. When every on-chain
// position is owned by some member, Σ ownedUPnL_i == U and the sum is exactly
// accountBalance. Any on-chain position that no member owns ("orphan") leaves
// its P&L out of Σ ownedUPnL_i, so the sum misses the balance by that orphan
// amount — returned as `drift`. This is the genuine accounting-bug signal the
// caller's throttled alarm watches; it is NOT masked into a member row.

// SharedWalletPosition is one on-chain position's reconciliation input,
// platform-agnostic so Hyperliquid (HLPosition) and OKX (OKXPosition) feed the
// same reconciler. Coin is the bare ticker ("BTC"); UnrealizedPnL is the
// exchange-reported value.
type SharedWalletPosition struct {
	Coin          string
	UnrealizedPnL float64
}

// sharedWalletReconcileResult is the output of reconcileSharedWalletMemberValues.
type sharedWalletReconcileResult struct {
	// Values maps each member strategy ID to its exchange-derived display
	// value, rounded to cents. Σ Values == round(accountBalance - Drift).
	Values map[string]float64
	// Drift is accountBalance - Σ(raw, un-rounded member values): the real
	// unrealized P&L of on-chain positions that no member owns (orphans), plus
	// any value lost when capital weights summed to zero (handled by the
	// equal-weight fallback, so normally just orphan P&L). ~0 in normal
	// operation; a materially non-zero value is an attribution/accounting bug.
	Drift float64
	// OrphanCoins lists (sorted) the on-chain coins whose unrealized P&L could
	// not be attributed to any member. It identifies WHICH position is
	// unowned in the operator alert and keys the drift tracker's streak so two
	// unrelated one-cycle transients on consecutive cycles don't read as one
	// persistent orphan.
	OrphanCoins []string
}

// reconcileSharedWalletMemberValues splits one shared wallet's real account
// balance into per-member display values. See the file-level comment for the
// model. Pure: no state mutation, no I/O.
//
//   - members:        the strategy IDs that share this wallet (≥1).
//   - capitalByID:    operator-set starting capital per member (the weight
//     basis). Members missing or with ≤0 capital fall back to an equal share
//     so the base is always fully distributed.
//   - positions:      the wallet's on-chain positions (coin + real uPnL).
//   - virtualQty:     coin → memberID → virtual quantity (>0), used to (a)
//     determine which members own a coin and (b) split a shared-coin
//     position's uPnL across co-owners. Built by the caller from current
//     state (per-platform coin extraction).
//   - accountBalance: the real wallet balance — the SAME number shown in the
//     TOTAL row (walletBalances[key]), so the per-strategy rows reconcile to
//     it.
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

	// Capital weights. Σ w_i = 1. Members with non-positive configured capital
	// fall back to an equal share of the total so the collateral base is never
	// silently dropped (which would itself create artificial drift).
	weights := make(map[string]float64, len(members))
	capitalSum := 0.0
	for _, id := range members {
		c := capitalByID[id]
		if c > 0 {
			capitalSum += c
		}
	}
	if capitalSum > 0 {
		// Distribute the base by configured capital; members with ≤0 capital
		// get 0 weight (their configured allocation is genuinely zero).
		for _, id := range members {
			c := capitalByID[id]
			if c > 0 {
				weights[id] = c / capitalSum
			} else {
				weights[id] = 0
			}
		}
	} else {
		// No positive capital anywhere → equal split so the base is fully
		// distributed and no value leaks into drift.
		eq := 1.0 / float64(len(members))
		for _, id := range members {
			weights[id] = eq
		}
	}

	// Total unrealized P&L across all on-chain positions in this wallet.
	totalUPnL := 0.0
	// Aggregate uPnL per coin (HL/OKX report one netted position per coin, but
	// sum defensively in case the snapshot ever lists a coin twice).
	uPnLByCoin := make(map[string]float64)
	for _, p := range positions {
		totalUPnL += p.UnrealizedPnL
		uPnLByCoin[p.Coin] += p.UnrealizedPnL
	}

	// base is the shared collateral pool to split by capital weight. This
	// subtraction assumes accountBalance is account EQUITY inclusive of
	// unrealized P&L (HL marginSummary.accountValue and OKX ccxt total/`eq`
	// both are — see get_account_balance). The member SUM reconciles to
	// accountBalance regardless of this assumption, but the per-member split is
	// only meaningful when it holds.
	base := accountBalance - totalUPnL

	// Attribute each coin's uPnL to its owning member(s), split by virtual-qty
	// share for shared-coin peers. Coins with on-chain uPnL but no virtual
	// owner among members are left unattributed → they surface as drift.
	memberSet := make(map[string]bool, len(members))
	for _, id := range members {
		memberSet[id] = true
	}
	ownedUPnL, _, orphanCoins := attributeSharedWalletUPnL(memberSet, uPnLByCoin, virtualQty)

	// Raw (un-rounded) per-member values. Σ raw = base + attributedUPnL
	// = accountBalance - (totalUPnL - attributedUPnL) = accountBalance - drift.
	raw := make(map[string]float64, len(members))
	rawSum := 0.0
	for _, id := range members {
		v := weights[id]*base + ownedUPnL[id]
		raw[id] = v
		rawSum += v
	}
	drift := accountBalance - rawSum

	// Round each value to cents; absorb only the sub-cent rounding residual
	// (NOT the drift) into the last member by sorted ID so the rounded values
	// sum to round(rawSum) exactly. A material drift deliberately remains a
	// visible shortfall + alarm rather than being hidden in a member row.
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

// attributeSharedWalletUPnL splits each coin's exchange-reported unrealized
// PnL across the members that virtually own it (by virtual-quantity share —
// mirrors hyperliquidKillSwitchFillShare). Coins with on-chain uPnL but no
// positive-qty member owner are returned as sorted orphanCoins; their PnL is
// excluded from attributedUPnL and surfaces in the caller's drift.
func attributeSharedWalletUPnL(
	memberSet map[string]bool,
	uPnLByCoin map[string]float64,
	virtualQty map[string]map[string]float64,
) (ownedUPnL map[string]float64, attributedUPnL float64, orphanCoins []string) {
	ownedUPnL = make(map[string]float64, len(memberSet))
	// Deterministic coin order for stable rounding behavior.
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
			continue // orphan: no member holds this coin virtually
		}
		sumQty := 0.0
		for id, qty := range owners {
			if memberSet[id] && qty > 0 {
				sumQty += qty
			}
		}
		if sumQty <= 0 {
			orphanCoins = append(orphanCoins, coin)
			continue // owners present but all non-member / non-positive → orphan
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

// ledgerWalletInputs feeds ledgerSharedWalletMemberValues — the #954
// trade-ledger replacement for the capital-weight split on Hyperliquid
// shared wallets. All fields are point-in-time snapshots; the function is
// pure (no state mutation, no I/O).
type ledgerWalletInputs struct {
	Members     []string
	InitialByID map[string]float64 // operator-set starting capital (EffectiveInitialCapital)
	LedgerByID  map[string]float64 // Σ tradeLedgerDelta per member (close net PnL − open fees + funding)
	Positions   []SharedWalletPosition
	VirtualQty  map[string]map[string]float64
	// AccountBalance is the real wallet equity (inclusive of unrealized PnL).
	AccountBalance float64
	// NonTradeFlows is Σ wallet_transfers.amount_usd — deposits, withdrawals,
	// transfers, and orphaned funding since the ledger watermarks were set.
	NonTradeFlows float64
	// BaselineOffset/BaselineSet anchor the drift at adoption time so history
	// predating the ledger (or repaired by `backfill trade-ledger`) reads as
	// zero drift. When !BaselineSet the caller stores RawDrift as the new
	// baseline and reports zero.
	BaselineOffset float64
	BaselineSet    bool
}

// ledgerSharedWalletMemberValues derives each member's display value from the
// local trades ledger instead of splitting the wallet balance (#954):
//
//	value_i = initial_capital_i + ledger_i + ownedUPnL_i
//
// where ledger_i is the strategy's cumulative trades-ledger delta (close-leg
// net PnL − open-leg fees + funding payments) and ownedUPnL_i the real
// unrealized PnL of the positions it owns (same attribution as #918/#920).
// Capital weight no longer enters the display number; an idle member's value
// no longer inherits drift from an active peer's trading.
//
// The wallet balance becomes a pure drift alarm:
//
//	rawDrift = balance − Σ value_i − NonTradeFlows
//	Drift    = rawDrift − BaselineOffset
//
// In steady state rawDrift−baseline ≈ 0; persistent non-zero drift means the
// ledger is missing or mis-pricing fills (lost rows, modeled-vs-real fee gap,
// mark-based close px) or an on-chain position is unowned (OrphanCoins names
// those). Unlike the #918 split, member values do NOT sum to the balance by
// construction — that independence is what makes the alarm able to see
// ledger errors at all.
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
		drift = 0 // first reconciled cycle: caller stores rawDrift as baseline
	}
	return sharedWalletReconcileResult{Values: values, Drift: drift, OrphanCoins: orphanCoins}, rawDrift
}

// roundCents rounds a dollar amount to the nearest cent.
func roundCents(v float64) float64 {
	return math.Round(v*100) / 100
}

// sharedWalletDriftResult reports one wallet's reconciliation outcome for the
// throttled drift alarm.
type sharedWalletDriftResult struct {
	Key         SharedWalletKey
	Drift       float64  // alarm drift: trade-ledger post-baseline drift, OR the journal drift when Basis==journal (#1100)
	Balance     float64  // the real account balance (accountValue) reconciled against
	MemberSum   float64  // Σ rounded member display values stored this cycle (attribution; always trade-ledger)
	OrphanCoins []string // sorted unattributed coins — streak signature + alert detail (PRESERVED under the journal basis as the noise-free orphan-exposure signal, #1107)
	// Basis is "" (trade-ledger) or driftBasisJournal when applyCashflowJournalDriftBasis
	// switched Drift onto the exchange-sourced cash-flow journal (#1100).
	Basis string
	// ExpectedEquity is the journal's reconstructed accountValue; only meaningful
	// when Basis==driftBasisJournal (Drift == Balance − ExpectedEquity).
	ExpectedEquity float64
	// JournalPending is set when the journal is the governing basis (operator-
	// enabled) but produced no trustworthy total this cycle — a transient stream-
	// fetch miss or a just-anchored baseline. reportSharedWalletDrift treats it as
	// "no info" and PRESERVES the journal streak instead of resetting the 2-cycle
	// confirmation off the within-tolerance trade-ledger fallback (#1107).
	JournalPending bool
}

// reconcileSharedWalletDisplayValues recomputes the exchange-authoritative
// per-strategy display value for every shared wallet that has a fresh balance
// this cycle and stores it on each member's StrategyState. Returns per-wallet
// drift for reportSharedWalletDrift.
//
// MUST be called under the state WRITE lock — it mutates
// StrategyState.SharedWalletValue / SharedWalletValueSet. No I/O: the balance
// and positions are the cycle's already-fetched clearinghouseState / OKX
// snapshot (#918 adds no network round-trips beyond what risk/sync already do).
//
// Gating contract: every strategy's SharedWalletValueSet is reset to false up
// front, then set true only for members of a wallet reconciled this cycle. So a
// wallet whose balance fetch failed (absent from walletBalances) leaves its
// members on the modeled PortfolioValue fallback, and no strategy ever serves a
// stale exchange-derived value.
//
// Membership: detectSharedWallets recognizes perps only, but a live HL `manual`
// strategy on the same account holds real on-chain positions returned by
// fetchHyperliquidState. Those are folded in as members here
// (sameAccountLiveManualMembers) so their positions are attributed (not treated
// as orphans → false drift alarm) and they receive an exchange-derived value
// (#920 review).
//
// OKX gating: HL fetches balance+positions atomically (fetchHyperliquidState),
// but OKX uses independent balance/position subprocesses. okxPositionsFetched
// reports whether the OKX position fetch succeeded this cycle; when it did not,
// OKX wallets are skipped (members fall back to PortfolioValue) rather than
// reconciled with U=0, which would skew each member's split for one cycle.
//
// #954 path split: Hyperliquid wallets derive member values from the local
// trades ledger (ledgerSharedWalletMemberValues; the wallet balance is a pure
// drift alarm, anchored by the wallet's stored baseline offset). OKX wallets
// keep the #918 capital-weight split until the ledger path is extended there.
// sdb supplies the per-member ledger sums + non-trade flows; when it is nil
// or a ledger read fails, HL wallets fall back to the split path for the
// cycle (display stays populated, WARN logged) rather than dropping rows.
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
		}
	}
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
			continue // fetch failed this cycle → members fall back (Set stays false)
		}

		// On-chain positions for this wallet's platform, coin-normalized to
		// upper-case so they match the virtualQty keys below.
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
				continue // positions fetch failed → don't reconcile with U=0
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
			continue // no position source wired for this platform yet
		}

		members := sharedWalletMembersWithManual(key, memberIDs, strategies)
		capitalByID, virtualQty := buildSharedWalletBooks(key, members, byID, state)

		var res sharedWalletReconcileResult
		if key.Platform == "hyperliquid" {
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

// sharedWalletMembersWithManual folds same-account live HL manual strategies
// into a wallet's perps member list (detectSharedWallets is perps-only, but
// their on-chain positions are in the same clearinghouseState snapshot).
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

// buildSharedWalletBooks snapshots each member's starting capital and virtual
// position quantity (coin → memberID → qty>0). posKey is the config symbol
// the strategy keys its virtual position under; the coin is its upper-case
// form, matching the SharedWalletPosition coins. HL manual keys by sc.Symbol;
// perps/OKX by the args symbol. Used by both the display reconcile and the
// wallet-ledger funding attribution (#954).
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
		// #1159: a hedge-enabled HL perps strategy also holds a leg on its hedge
		// coin. Emit it so the hedge coin is not classified as an orphan coin by
		// attributeSharedWalletUPnL (which would fire phantom drift alerts), and
		// so hedge uPnL + wallet-ledger funding events on the hedge coin
		// attribute to the owning strategy (requirement 6). Collision validation
		// guarantees the hedge coin is sole-owned, so a single member maps to it.
		if key.Platform == "hyperliquid" {
			if hc := hedgeCoin(sc); hc != "" {
				if hp, hok := ss.Positions[hc]; hok && hp != nil && hp.Quantity > 0 && hp.HedgeFor != "" {
					if virtualQty[hc] == nil {
						virtualQty[hc] = make(map[string]float64)
					}
					virtualQty[hc][id] = hp.Quantity
				}
			}
		}
	}
	return capitalByID, virtualQty
}

// reconcileHLWalletViaLedger derives a Hyperliquid wallet's member values from
// the trades ledger (#954) and anchors the drift baseline on first contact.
// Any ledger read failure falls back to the #918 capital-weight split for the
// cycle so operator rows stay populated — the fallback Drift then measures
// orphan uPnL (split semantics), which stays within tolerance in normal
// operation, so a transient DB error does not fire a false ledger-drift alarm.
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
	// The watermark row is owned by fetchWalletLedgerEvents, which anchors
	// FundingSinceMs/TransfersSinceMs at `now` on first contact. If the row is
	// absent here its init failed this cycle — upserting the zero-value state
	// below would persist watermarks of 0 and make the next fetch replay the
	// wallet's entire funding history past a baseline that never accounted for
	// it. Never originate the row from the display path; fall back for the
	// cycle and let the next fetch re-init before the baseline is anchored.
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

// displayStrategyValue returns the value to SHOW operators for a strategy: the
// exchange-authoritative shared-wallet value when one was reconciled this cycle
// (#918), otherwise the modeled PortfolioValue. Risk math must continue to call
// PortfolioValue directly — this helper is for operator-facing surfaces only
// (Discord/Telegram summaries, leaderboard, /status, dashboard, cycle log).
func displayStrategyValue(s *StrategyState, prices map[string]float64) float64 {
	if s != nil && s.SharedWalletValueSet {
		return s.SharedWalletValue
	}
	return PortfolioValue(s, prices)
}

// computeSubsetDisplayValue returns the TOTAL value for a set of operator-facing
// strategy rows so the TOTAL reconciles with the per-row displayStrategyValue.
//
// Strategies carrying an exchange-derived value this cycle (SharedWalletValueSet)
// are summed directly: reconciled member values sum to the real account balance
// by construction, so no wallet dedup is needed and even a partial slice of a
// shared wallet (per-asset summaries, leaderboard top-N) matches its visible
// rows exactly — computeSubsetPortfolioValue cannot do that for a straddling
// wallet, where it virtual-sums the modeled PortfolioValue while the rows show
// the exchange-derived split (#920 review). This also removes the display-side
// double count for a gated same-account live manual strategy, whose slice is
// already inside the wallet balance.
//
// Ungated strategies (non-shared platforms, or a wallet whose reconcile was
// skipped this cycle on a fetch failure) fall back to
// computeSubsetPortfolioValue with the original #915 dedup/virtual-sum
// semantics — consistent with displayStrategyValue falling back to the modeled
// PortfolioValue for the same rows. A wallet's members are always gated or
// ungated together (reconcileSharedWalletDisplayValues sets the whole wallet
// atomically), so the fallback never sees a partially-gated wallet.
//
// Display-only: risk math keeps computeTotalPortfolioValue / PortfolioValue.
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
		if s, ok := state.Strategies[sc.ID]; ok && s != nil && s.SharedWalletValueSet {
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

// dedupedSameAccountLiveManualIDs returns live HL manual strategy IDs whose
// collateral is already inside a deduped shared-wallet balance (#921).
func dedupedSameAccountLiveManualIDs(strategies []StrategyConfig) map[string]bool {
	out := make(map[string]bool)
	for key := range detectSharedWallets(strategies) {
		for _, id := range sameAccountLiveManualMembers(key, strategies) {
			out[id] = true
		}
	}
	return out
}

// riskPathWalletMemberIDs returns perps shared-wallet members plus same-account
// live HL manual strategies in subset. detectSharedWallets is perps-only, but
// a live manual on the same HYPERLIQUID_ACCOUNT_ADDRESS is already inside the
// wallet real balance — exclude its modeled PortfolioValue from the risk-path
// per-strategy sum when the wallet balance is contributed (#921).
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

// sameAccountLiveManualMembers returns live HL `manual` strategy IDs that trade
// from the same on-exchange account as key. detectSharedWallets recognizes only
// perps (walletKeyRegistry), but a live manual strategy on the same
// HYPERLIQUID_ACCOUNT_ADDRESS holds real on-chain positions returned by
// fetchHyperliquidState; folding it in as a reconciliation member keeps those
// positions from being misread as orphans (false drift alarm) and gives the
// manual strategy a consistent exchange-derived display value (#920 review).
// OKX has no manual instrument, so this is HL-only.
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

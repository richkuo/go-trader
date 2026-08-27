package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

const maxTradeHistory = 1000

var tradeRecorder func(strategyID string, trade Trade) error

func suspendEagerTradePersist() func() {
	prev := tradeRecorder
	tradeRecorder = nil
	return func() { tradeRecorder = prev }
}

var tradePersistWarn func(msg string)

func RecordTrade(s *StrategyState, trade Trade) {
	if trade.StrategyID == "" {
		trade.StrategyID = s.ID
	}
	if trade.PositionID == "" {
		if pos := s.Positions[trade.Symbol]; pos != nil {
			trade.PositionID = ensurePositionTradeID(s.ID, trade.Symbol, pos)
		} else if opt := s.OptionPositions[trade.Symbol]; opt != nil {
			trade.PositionID = ensureOptionTradeID(s.ID, opt)
		}
	}
	s.TradeHistory = append(s.TradeHistory, trade)
	if tradeRecorder == nil {
		return
	}
	if err := tradeRecorder(s.ID, trade); err != nil {
		msg := fmt.Sprintf("immediate trade persist failed for %s: %v", s.ID, err)
		fmt.Fprintf(os.Stderr, "[state] WARN: %s\n", msg)
		if tradePersistWarn != nil {
			tradePersistWarn(msg)
		}
		return
	}
	s.TradeHistory[len(s.TradeHistory)-1].persisted = true
}

type ReconciliationGap struct {
	Coin       string    `json:"coin"`
	OnChainQty float64   `json:"on_chain_qty"`
	VirtualQty float64   `json:"virtual_qty"`
	DeltaQty   float64   `json:"delta_qty"`
	Strategies []string  `json:"strategies"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type AppState struct {
	CycleCount          int                       `json:"cycle_count"`
	LastCycle           time.Time                 `json:"last_cycle"`
	Strategies          map[string]*StrategyState `json:"strategies"`
	PortfolioRisk       PortfolioRiskState        `json:"portfolio_risk"`
	CorrelationSnapshot *CorrelationSnapshot      `json:"correlation_snapshot,omitempty"`
	LatestSharedWalletBalances map[SharedWalletKey]float64  `json:"-"`
	LatestSharedWalletMembers  map[SharedWalletKey][]string `json:"-"`
	ReconciliationGaps      map[string]*ReconciliationGap `json:"reconciliation_gaps,omitempty"`
	LastLeaderboardPostDate string                        `json:"last_leaderboard_post_date,omitempty"`
	LastLeaderboardSummaries map[string]time.Time `json:"last_leaderboard_summaries,omitempty"`
	LastSummaryPost map[string]time.Time `json:"last_summary_post,omitempty"`
}

type StrategyState struct {
	ID               string                     `json:"id"`
	Type             string                     `json:"type"`
	Platform         string                     `json:"platform,omitempty"`
	Cash             float64                    `json:"cash"`
	InitialCapital   float64                    `json:"initial_capital"`
	Positions        map[string]*Position       `json:"positions"`
	OptionPositions  map[string]*OptionPosition `json:"option_positions"`
	TradeHistory     []Trade                    `json:"trade_history"`
	RiskState        RiskState                  `json:"risk_state"`
	Regime           string                     `json:"regime,omitempty"`
	RegimeWindows    map[string]string          `json:"regime_windows,omitempty"`
	RegimeDivergence *RegimeDivergenceState     `json:"-"`
	RegimeProfile    *RegimeProfileState        `json:"regime_profile,omitempty"`
	HurstGate HurstGateState `json:"hurst_gate_state,omitempty"`
	ClosedPositions []ClosedPosition `json:"-"`
	ClosedOptionPositions []ClosedOptionPosition `json:"-"`
	pendingTradeDiagnostics []TradeDiagnosticsRow `json:"-"`

	SharedWalletValue float64 `json:"-"`
	SharedWalletValueSet bool `json:"-"`
	SharedWalletPerformanceOnly bool `json:"-"`
	SharedWalletPoolBudget bool `json:"shared_wallet_pool_budget,omitempty"`

	CashReconcileRequired bool `json:"cash_reconcile_required,omitempty"`

	ReplayMirrorWatermark int64 `json:"replay_mirror_watermark,omitempty"`
}

func NewStrategyState(cfg StrategyConfig) *StrategyState {
	initCap := cfg.Capital
	if cfg.InitialCapital > 0 {
		initCap = cfg.InitialCapital
	}
	return &StrategyState{
		ID:              cfg.ID,
		Type:            cfg.Type,
		Platform:        cfg.Platform,
		Cash:            cfg.Capital,
		InitialCapital:  initCap,
		Positions:       make(map[string]*Position),
		OptionPositions: make(map[string]*OptionPosition),
		TradeHistory:    []Trade{},
		RiskState: RiskState{
			PeakValue:      cfg.Capital,
			MaxDrawdownPct: cfg.MaxDrawdownPct,
		},
	}
}

func NewAppState() *AppState {
	return &AppState{
		CycleCount: 0,
		Strategies: make(map[string]*StrategyState),
	}
}

type sharedWalletPoolStateTransition string

const (
	sharedWalletPoolStateUnchanged sharedWalletPoolStateTransition = ""
	sharedWalletPoolStateEntered   sharedWalletPoolStateTransition = "entered"
	sharedWalletPoolStateLeft      sharedWalletPoolStateTransition = "left"
)

func applySharedWalletPoolStateMode(sc StrategyConfig, s *StrategyState) (sharedWalletPoolStateTransition, error) {
	if s == nil {
		return sharedWalletPoolStateUnchanged, nil
	}
	currentPool := usesSharedWalletPoolBudget(sc)
	if currentPool == s.SharedWalletPoolBudget {
		s.SharedWalletPerformanceOnly = currentPool
		return sharedWalletPoolStateUnchanged, nil
	}
	if strategyHasOpenPositions(s) {
		s.SharedWalletPerformanceOnly = s.SharedWalletPoolBudget
		return sharedWalletPoolStateUnchanged, fmt.Errorf(
			"strategy[%s]: cannot transition shared-wallet pool budgeting while positions are open",
			sc.ID)
	}

	if currentPool {
		s.SharedWalletPerformanceOnly = true
		s.SharedWalletPoolBudget = true
		s.Cash = 0
		s.InitialCapital = 0
		s.RiskState.PeakValue = 0
		s.RiskState.CurrentDrawdownPct = 0
		return sharedWalletPoolStateEntered, nil
	}

	baseline := sc.InitialCapital
	if baseline <= 0 {
		baseline = sc.Capital
	}
	if baseline <= 0 {
		s.SharedWalletPerformanceOnly = true
		return sharedWalletPoolStateUnchanged, fmt.Errorf(
			"strategy[%s]: cannot leave shared-wallet pool mode without a positive resolved capital or initial_capital",
			sc.ID)
	}

	s.SharedWalletPoolBudget = false
	s.SharedWalletPerformanceOnly = false
	s.Cash += baseline
	s.InitialCapital = baseline
	s.RiskState.PeakValue = PortfolioValue(s, nil)
	s.RiskState.CurrentDrawdownPct = 0
	return sharedWalletPoolStateLeft, nil
}

func deferSharedWalletPoolTransition(sc *StrategyConfig, transitionErr error) string {
	sc.sharedWalletModeDeferred = true
	sc.Paused = true
	return fmt.Sprintf(
		"strategy %s shared-wallet pool transition deferred: %v; running manage-only until a later flat restart can complete it",
		sc.ID, transitionErr)
}

func effectiveSharedWalletPoolBook(sc StrategyConfig, s *StrategyState) bool {
	if sc.sharedWalletModeDeferred && s != nil {
		return s.SharedWalletPoolBudget
	}
	return usesSharedWalletPoolBudget(sc)
}

func sharedWalletPoolTransitionBlockers(strategies []StrategyConfig, state *AppState) map[string]error {
	blocked := make(map[string]error)
	if state == nil {
		return blocked
	}
	groups := make(map[configuredWalletKey][]StrategyConfig)
	for _, sc := range strategies {
		if key, ok := configuredWalletKeyFor(sc); ok {
			groups[key] = append(groups[key], sc)
		}
	}
	for key, members := range groups {
		if len(members) < 2 {
			continue
		}
		transitioning := false
		var openIDs []string
		for _, sc := range members {
			ss := state.Strategies[sc.ID]
			if ss == nil {
				continue
			}
			if usesSharedWalletPoolBudget(sc) != ss.SharedWalletPoolBudget {
				transitioning = true
			}
			if strategyHasOpenPositions(ss) {
				openIDs = append(openIDs, sc.ID)
			}
		}
		if !transitioning || len(openIDs) == 0 {
			continue
		}
		sort.Strings(openIDs)
		err := fmt.Errorf(
			"shared-wallet %s/%s cannot transition pool budgeting while members have open positions: %s",
			key.Platform, key.Instrument, strings.Join(openIDs, ", "))
		for _, sc := range members {
			blocked[sc.ID] = err
		}
	}
	return blocked
}

func ValidateState(state *AppState, strategies []StrategyConfig) {
	configuredPoolIDs := make(map[string]bool)
	for _, sc := range strategies {
		if usesSharedWalletPoolBudget(sc) {
			configuredPoolIDs[sc.ID] = true
		}
	}
	for id, s := range state.Strategies {
		if s.InitialCapital < 0 {
			fmt.Printf("[WARN] state: strategy %s has invalid initial_capital=%g, resetting to 0\n", id, s.InitialCapital)
			s.InitialCapital = 0
		}
		if s.Cash < 0 && !s.SharedWalletPoolBudget && !configuredPoolIDs[id] {
			fmt.Printf("[WARN] state: strategy %s has negative cash=%g, clamping to 0\n", id, s.Cash)
			s.Cash = 0
		}
		maybeClearCashReconcileRequired(s)
		for sym, pos := range s.Positions {
			if pos.Quantity <= 0 {
				fmt.Printf("[WARN] state: strategy %s position %s has invalid quantity=%g, removing\n", id, sym, pos.Quantity)
				delete(s.Positions, sym)
				continue
			}
			if pos.OwnerStrategyID == "" {
				pos.OwnerStrategyID = id
			}
		}
		for key, op := range s.OptionPositions {
			valid := true
			if op.Action != "buy" && op.Action != "sell" {
				fmt.Printf("[WARN] state: strategy %s option %s has invalid action=%q, removing\n", id, key, op.Action)
				valid = false
			}
			if op.OptionType != "call" && op.OptionType != "put" {
				fmt.Printf("[WARN] state: strategy %s option %s has invalid option_type=%q, removing\n", id, key, op.OptionType)
				valid = false
			}
			if op.Quantity <= 0 {
				fmt.Printf("[WARN] state: strategy %s option %s has invalid quantity=%g, removing\n", id, key, op.Quantity)
				valid = false
			}
			if !valid {
				delete(s.OptionPositions, key)
			}
		}
	}
}

func migrateLegacyPerpsPositionMultipliers(state *AppState, cfg *Config) int {
	if state == nil {
		return 0
	}
	perpsIDs := make(map[string]bool)
	if cfg != nil {
		for _, sc := range cfg.Strategies {
			if sc.Type == "perps" {
				perpsIDs[sc.ID] = true
			}
		}
	}
	migrated := 0
	for id, s := range state.Strategies {
		if s == nil || (s.Type != "perps" && !perpsIDs[id]) {
			continue
		}
		for _, pos := range s.Positions {
			if pos == nil || pos.Multiplier > 0 {
				continue
			}
			pos.Multiplier = 1
			migrated++
		}
	}
	return migrated
}

func ValidatePerpsDirectionConfig(state *AppState, cfg *Config) []string {
	var warnings []string
	for i := range cfg.Strategies {
		sc := &cfg.Strategies[i]
		if sc.Type != "perps" {
			continue
		}
		s, ok := state.Strategies[sc.ID]
		if !ok {
			continue
		}
		baseDirection := EffectiveDirection(*sc)
		policyConfigured := sc.RegimeDirectionalPolicy != nil && sc.RegimeDirectionalPolicy.IsConfigured()
		syms := make([]string, 0, len(s.Positions))
		for sym := range s.Positions {
			syms = append(syms, sym)
		}
		sort.Strings(syms)
		for _, sym := range syms {
			pos := s.Positions[sym]
			if pos == nil || pos.Quantity <= 0 {
				continue
			}
			if pos.isHedgeLeg() {
				continue
			}
			posRegime := positionDirectionalRegimeLabel(pos, *sc)
			effectiveDir := EffectiveDirectionForPositionGated(*sc, "", posRegime, pos.Quantity, pos.DirectionCertifiedStatesAtOpen)
			if !perpsPositionConflictsDirection(pos.Side, effectiveDir) {
				continue
			}
			if policyConfigured && pos.DirectionCertifiedAtOpen && posRegime == "" && policyAllowsPositionSideGated(*sc, pos.Side, pos.DirectionCertifiedStatesAtOpen) {
				continue
			}
			conflictSide := pos.Side
			var regimeNote string
			switch {
			case !pos.DirectionCertifiedAtOpen && policyConfigured:
				regimeNote = fmt.Sprintf("effective_direction=%q (regime_directional_policy DEFAULT-OFF / uncertified #1085 → base_direction=%q; close from flat to migrate)", effectiveDir, baseDirection)
			case posRegime != "":
				regimeNote = fmt.Sprintf("effective_direction=%q from stamped regime=%q; base_direction=%q", effectiveDir, posRegime, baseDirection)
			case policyConfigured:
				regimeNote = fmt.Sprintf("effective_direction=%q (base_direction=%q; position regime unknown — validated against base only)", effectiveDir, baseDirection)
			default:
				regimeNote = fmt.Sprintf("direction=%q", baseDirection)
			}
			msg := fmt.Sprintf("perps state-vs-config gap: strategy %s has %s %s qty=%g (%s). Position was likely seeded by migration, paper→live handoff, or a prior conflicting direction. Close manually before the next signal — the executor's fresh-open sizing will otherwise desync virtual state from the exchange.", sc.ID, conflictSide, sym, pos.Quantity, regimeNote)
			fmt.Printf("[WARN] %s\n", msg)
			warnings = append(warnings, msg)
		}
	}
	return warnings
}

func ReconcileConfigInitialCapital(cfg *Config, state *AppState, sdb *StateDB) (infos []string, errors []string) {
	if state == nil || sdb == nil {
		return nil, nil
	}
	for _, sc := range cfg.Strategies {
		if sc.InitialCapital <= 0 {
			continue
		}
		s, ok := state.Strategies[sc.ID]
		if !ok || s.InitialCapital <= 0 || s.InitialCapital == sc.InitialCapital {
			continue
		}
		prev := s.InitialCapital
		if err := sdb.SetInitialCapital(sc.ID, sc.InitialCapital); err != nil {
			msg := fmt.Sprintf("config-driven initial_capital change for %s ($%.2f → $%.2f) failed to persist: %v — DB still holds $%.2f",
				sc.ID, prev, sc.InitialCapital, err, prev)
			fmt.Fprintf(os.Stderr, "[state] WARN: %s\n", msg)
			errors = append(errors, msg)
			continue
		}
		s.InitialCapital = sc.InitialCapital
		msg := fmt.Sprintf("config-driven initial_capital change applied for %s: $%.2f → $%.2f (#343)",
			sc.ID, prev, sc.InitialCapital)
		fmt.Fprintf(os.Stderr, "[state] %s\n", msg)
		infos = append(infos, msg)
	}
	return infos, errors
}

func LoadStateWithDB(cfg *Config, sdb *StateDB) (*AppState, error) {
	state, err := sdb.LoadState()
	if err != nil {
		return nil, fmt.Errorf("sqlite load: %w", err)
	}
	if state != nil {
		if migrated := migrateLegacyPerpsPositionMultipliers(state, cfg); migrated > 0 {
			fmt.Printf("[state] Migrated %d legacy perps position multiplier(s) to 1\n", migrated)
		}
		fmt.Println("[state] Loaded from SQLite")
		return state, nil
	}
	return NewAppState(), nil
}

func SaveStateWithDB(state *AppState, cfg *Config, sdb *StateDB) error {
	return sdb.SaveState(state)
}

func SaveStrategyBookWithDB(s *StrategyState, sdb *StateDB) error {
	return sdb.SaveStrategyBook(s)
}

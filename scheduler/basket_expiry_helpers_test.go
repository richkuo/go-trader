package main

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

const basketLegacyContractJSON = `{"v":1,"basket_id":"xs-test","partition":"paper","ranker":"basket_scripted_fixture","params":{"weights":{"AVAX":-0.25,"BTC":0.25,"ETH":0.25,"SOL":-0.25}},"rebalance_interval_seconds":86400,"evaluation_window_seconds":1800,"ranker_timeout_seconds":60,"lookback_bars":48,"gross_budget_usd":4000,"max_transition_net_usd":400,"max_coin_notional_usd":1500,"min_eligible_members":3,"eligibility_source":"operator listing ledger","eligibility_version":"2026-10-01","min_history_bars":24,"listings":{"AVAX":{"listed_at_ms":1577836800000,"known_at_ms":1577836800000},"BTC":{"listed_at_ms":1577836800000,"known_at_ms":1577836800000},"ETH":{"listed_at_ms":1577836800000,"known_at_ms":1577836800000},"SOL":{"listed_at_ms":1577836800000,"known_at_ms":1577836800000}},"members":[{"strategy_id":"hl-bsk-avax","coin":"AVAX","timeframe":"1h","interval_ms":3600000},{"strategy_id":"hl-bsk-btc","coin":"BTC","timeframe":"1h","interval_ms":3600000},{"strategy_id":"hl-bsk-eth","coin":"ETH","timeframe":"1h","interval_ms":3600000},{"strategy_id":"hl-bsk-sol","coin":"SOL","timeframe":"1h","interval_ms":3600000}]}`

const basketLegacyContractFP = "4606d0985ce49f635a41038f449737fa51bf460efbfe955331944824640382c9"

func basketHorizonConfig(t *testing.T, horizon int64, mutate func(root map[string]interface{})) *Config {
	t.Helper()
	return loadBasketTestConfig(t, func(root map[string]interface{}) {
		b := basketTestBasket(root)
		b["max_transition_net_usd"] = 1100
		if horizon != 0 {
			b["holding_horizon_seconds"] = horizon
		}
		if mutate != nil {
			mutate(root)
		}
	})
}

func basketHistoricalDeadline() time.Time {
	return time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)
}

func (e *basketTestEnv) stepAt(now time.Time, snap *marketSnapshot, once bool, marksRetry time.Time) {
	e.t.Helper()
	e.now = now
	e.bc.beginCycle()
	e.bc.step(basketCycleInputs{
		Now:          now,
		Snapshot:     snap,
		SnapshotFor:  func(*basketRuntime, time.Time) *marketSnapshot { return snap },
		Once:         once,
		MarksRetryAt: marksRetry,
	})
}

func (e *basketTestEnv) lifecycleRow(id string) (basketLifecycle, float64, bool) {
	e.t.Helper()
	db := e.store.primary()
	sid, err := db.toStorageID(id)
	if err != nil {
		e.t.Fatal(err)
	}
	row := db.db.QueryRow(`SELECT `+basketMemberStateBaseColumns+`, `+basketMemberStateLifecycleSelect+` FROM basket_member_state WHERE basket_id = 'xs-test' AND strategy_id = ?`, sid)
	_, _, q, l, err := scanBasketMemberState(row.Scan)
	if err == sql.ErrNoRows {
		return basketLifecycle{}, 0, false
	}
	if err != nil {
		e.t.Fatalf("read lifecycle row %s: %v", id, err)
	}
	return l, q, true
}

func (e *basketTestEnv) openMembers(deadline time.Time) time.Time {
	e.t.Helper()
	opened := deadline.Add(time.Minute)
	e.cycle(opened, deadline, basketTestSnapshot(deadline, 60, basketTestMarks(), 1), true)
	rb := e.rt().Rebalances[deadline.UnixMilli()]
	if rb == nil || rb.Status != basketRebalanceCompleted {
		e.t.Fatalf("the opening rebalance did not complete: %+v", rb)
	}
	for _, m := range basketTestMembers {
		if e.signedQty(m.ID, m.Coin) == 0 {
			e.t.Fatalf("%s did not open", m.ID)
		}
	}
	return opened
}

func (e *basketTestEnv) attachNotifier() *mockNotifier {
	mock := &mockNotifier{}
	e.bc.notifier = NewMultiNotifier(notifierBackend{notifier: mock, ownerID: "owner"})
	return mock
}

func countDMs(m *mockNotifier, marker string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, dm := range m.dms {
		if strings.Contains(dm.content, marker) {
			n++
		}
	}
	return n
}

func (e *basketTestEnv) closeTrades(id, coin string) int {
	n := 0
	for _, tr := range e.state.Strategies[id].TradeHistory {
		if tr.IsClose && tr.Symbol == coin {
			n++
		}
	}
	return n
}

func (e *basketTestEnv) expirySubmits(v *scriptedBasketVenue) []basketAction {
	v.mu.Lock()
	defer v.mu.Unlock()
	var out []basketAction
	for _, a := range v.submits {
		if a.expiry() {
			out = append(out, a)
		}
	}
	return out
}

func basketTestLogger(id string) *StrategyLogger {
	return &StrategyLogger{stratID: id, writer: os.Stdout}
}

func basketBlockingRanker(release <-chan struct{}, weights map[string]float64) basketRankRunner {
	inner := basketTestRanker(weights, "")
	return func(ctx context.Context, stdin []byte, timeout time.Duration) ([]byte, []byte, error) {
		select {
		case <-release:
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
		return inner(ctx, stdin, timeout)
	}
}

func basketCountingRanker(n *int, mu *sync.Mutex, stdin *[]string, inner basketRankRunner) basketRankRunner {
	return func(ctx context.Context, in []byte, timeout time.Duration) ([]byte, []byte, error) {
		mu.Lock()
		*n++
		*stdin = append(*stdin, string(in))
		mu.Unlock()
		return inner(ctx, in, timeout)
	}
}

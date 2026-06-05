package main

// #879: per-cycle global regime store. Regime is computed once per distinct
// regime signature per cycle in the Go scheduler (via a dedicated read-only
// subprocess), stored here, and injected into every check script so checks no
// longer compute regime inline. Flat manual, options, and the dashboard read
// this store directly.
//
// Regime windows live in GLOBAL cfg.Regime.Windows (per-strategy fields are
// only selectors), so the dedup unit is (symbol, interval) + a fingerprint of
// the resolved global windows-spec JSON. The subprocess computes each window
// with its own classifier/period via the existing compute_multi_regime, so
// labels are byte-identical to the pre-migration inline path for the same
// candles.

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
)

// RegimeSignature dedups regime computation across peer strategies.
type RegimeSignature struct {
	Symbol   string
	Interval string
	SpecHash string
}

func regimeSignatureForStrategy(sc StrategyConfig, rc *RegimeConfig) RegimeSignature {
	return RegimeSignature{
		Symbol:   regimeSignatureSymbol(sc),
		Interval: regimeSignatureInterval(sc),
		SpecHash: regimeSpecHash(rc),
	}
}

// regimeSignatureSymbol returns the asset symbol the platform's OHLCV fetch
// uses — args[1] for every signal-check script (HL/OKX/RH/TopStep/spot all take
// "<strategy> <symbol> <timeframe>"). Manual strategies fall back to sc.Symbol.
func regimeSignatureSymbol(sc StrategyConfig) string {
	if s := positionalArg(sc.Args, 1); s != "" {
		return s
	}
	return strings.TrimSpace(sc.Symbol)
}

// regimeSignatureInterval returns the candle timeframe — args[2] when it is a
// positional (not a flag). Empty when the strategy has no timeframe argument.
func regimeSignatureInterval(sc StrategyConfig) string {
	return positionalArg(sc.Args, 2)
}

// positionalArg returns args[idx] when it exists and is not a "--flag".
func positionalArg(args []string, idx int) string {
	if idx < 0 || idx >= len(args) {
		return ""
	}
	v := strings.TrimSpace(args[idx])
	if v == "" || strings.HasPrefix(v, "--") {
		return ""
	}
	return v
}

func regimeSpecHash(rc *RegimeConfig) string {
	blob := regimeWindowsSpecJSON(rc)
	sum := sha256.Sum256([]byte(blob))
	return hex.EncodeToString(sum[:8])
}

type regimeStoreEntry struct {
	payload RegimePayload
	failed  bool
}

// RegimeStore is the per-cycle two-layer store. Rebuilt every cycle (empty at
// loop start), never persisted. A failed signature is present-but-empty so
// reads fail-open rather than treating "missing" as "compute inline".
type RegimeStore struct {
	mu      sync.RWMutex
	entries map[RegimeSignature]regimeStoreEntry
}

func newRegimeStore() *RegimeStore {
	return &RegimeStore{entries: make(map[RegimeSignature]regimeStoreEntry)}
}

func (s *RegimeStore) put(sig RegimeSignature, pl RegimePayload, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.entries[sig] = regimeStoreEntry{payload: RegimePayload{}, failed: true}
		return
	}
	s.entries[sig] = regimeStoreEntry{payload: pl}
}

func (s *RegimeStore) get(sig RegimeSignature) (RegimePayload, bool) {
	if s == nil {
		return RegimePayload{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.entries[sig]
	return e.payload, ok
}

func (s *RegimeStore) failed(sig RegimeSignature) bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.entries[sig].failed
}

// payloadForStrategy is the consumer entry point: the store payload for a
// strategy's signature (empty payload when missing/failed → fail-open).
func (s *RegimeStore) payloadForStrategy(sc StrategyConfig, rc *RegimeConfig) RegimePayload {
	if s == nil || rc == nil || !rc.Enabled {
		return RegimePayload{}
	}
	pl, _ := s.get(regimeSignatureForStrategy(sc, rc))
	return pl
}

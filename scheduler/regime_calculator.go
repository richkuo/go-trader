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
	"sort"
	"strings"
	"sync"
)

// Regime signature kinds keep two store entries that share
// (platform, symbol, interval, spec) from clobbering each other:
//   - bundle:  the per-cycle multi-window payload from the fetch_regime.py
//     subprocess, READ by every participating strategy's gate/ATR/directional.
//   - options: the advisory 3-state ADX label populated FROM the options check
//     result (write-only, dashboard display). Without a distinct kind an
//     options strategy on underlying BTC at 4h would overwrite a real BTC/4h
//     bundle that a later-dispatched peer then reads (#901 review #1).
const (
	regimeSignatureKindBundle  = "bundle"
	regimeSignatureKindOptions = "options"
)

// RegimeSignature dedups regime computation across peer strategies. Platform is
// part of the key because two exchanges' candles for the same symbol string
// differ (#901 review #2); Kind separates the bundle from the advisory options
// entry (#901 review #1).
type RegimeSignature struct {
	Platform string
	Symbol   string
	Interval string
	SpecHash string
	Kind     string
}

func regimeSignatureForStrategy(sc StrategyConfig, rc *RegimeConfig) RegimeSignature {
	return RegimeSignature{
		Platform: regimePlatformForStrategy(sc),
		Symbol:   regimeSignatureSymbol(sc),
		Interval: regimeSignatureInterval(sc),
		SpecHash: regimeSpecHash(rc),
		Kind:     regimeSignatureKindBundle,
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

// snapshot returns a deterministic (sorted) copy of the store for the dashboard
// portfolio view. Safe to hand to another goroutine — no shared map. rc is
// threaded so the top-level label honors the configured primary window
// (medium) instead of the lexicographically-first one (#901 review #3).
func (s *RegimeStore) snapshot(rc *RegimeConfig) []RegimePortfolioEntry {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]RegimePortfolioEntry, 0, len(s.entries))
	for sig, e := range s.entries {
		entry := RegimePortfolioEntry{
			Symbol:   sig.Symbol,
			Interval: sig.Interval,
			Regime:   e.payload.PrimaryLabel(rc),
			Failed:   e.failed,
		}
		if labels := e.payload.WindowLabels(); len(labels) > 0 {
			entry.Windows = cloneStringMap(labels)
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Symbol != out[j].Symbol {
			return out[i].Symbol < out[j].Symbol
		}
		return out[i].Interval < out[j].Interval
	})
	return out
}

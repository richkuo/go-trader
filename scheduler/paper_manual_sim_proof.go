package main

import (
	"encoding/json"
	"io"
	"strings"
)

const (
	paperSimProofVersion       = 1
	paperWarningReceiptVersion = 1

	paperSimKindRecordOnly = "record_only"
	paperSimKindCycleClose = "cycle_close"
)

type paperSimQueueProof struct {
	Version    int    `json:"v"`
	Kind       string `json:"kind"`
	Action     string `json:"action"`
	Symbol     string `json:"symbol"`
	PositionID string `json:"position_id,omitempty"`
}

type paperSimPositionProof struct {
	Version    int    `json:"v"`
	PositionID string `json:"position_id"`
}

type paperWarningReceipt struct {
	Version     int    `json:"v"`
	PositionID  string `json:"position_id"`
	Service     string `json:"service"`
	Destination string `json:"destination"`
}

func decodeStrictJSON(raw string, v any) bool {
	if strings.TrimSpace(raw) == "" {
		return false
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return false
	}
	var trailing json.RawMessage
	return dec.Decode(&trailing) == io.EOF
}

func paperManualHLStrategy(sc StrategyConfig) bool {
	return sc.Type == "manual" && sc.Platform == "hyperliquid" && portfolioScopeFor(sc) != ScopeLive
}

func paperSimQueueProofJSON(kind, action, symbol, positionID string) string {
	blob, err := json.Marshal(paperSimQueueProof{Version: paperSimProofVersion, Kind: kind, Action: action, Symbol: symbol, PositionID: positionID})
	if err != nil {
		return ""
	}
	return string(blob)
}

func paperSimPositionProofJSON(positionID string) string {
	if positionID == "" {
		return ""
	}
	blob, err := json.Marshal(paperSimPositionProof{Version: paperSimProofVersion, PositionID: positionID})
	if err != nil {
		return ""
	}
	return string(blob)
}

func loadedQueuePaperSimProof(proof, tpOIDsJSON, prevTPOIDsJSON, tpArmedTiersJSON string) string {
	if tpOIDsJSON != "" || prevTPOIDsJSON != "" || tpArmedTiersJSON != "" {
		return ""
	}
	return proof
}

func loadedPositionPaperSimProof(proof, tpOIDsJSON string, tp1OID, tp2OID int64) string {
	if tp1OID != 0 || tp2OID != 0 {
		return ""
	}
	if strings.TrimSpace(tpOIDsJSON) != "" {
		var oids []int64
		if err := json.Unmarshal([]byte(tpOIDsJSON), &oids); err != nil {
			return ""
		}
		for _, oid := range oids {
			if oid != 0 {
				return ""
			}
		}
	}
	return proof
}

func paperSimQueueProofHolds(sc StrategyConfig, a PendingManualAction, positionID string) bool {
	if !paperManualHLStrategy(sc) || a.StrategyID != sc.ID || a.Symbol == "" || a.Symbol != sc.Symbol {
		return false
	}
	var p paperSimQueueProof
	if !decodeStrictJSON(a.PaperSimProof, &p) {
		return false
	}
	if p.Version != paperSimProofVersion || p.Action != a.Action || p.Symbol != a.Symbol {
		return false
	}
	switch {
	case p.Kind == paperSimKindRecordOnly && a.Action == "open":
		if p.PositionID != "" {
			return false
		}
	case p.Kind == paperSimKindRecordOnly && a.Action == "add", p.Kind == paperSimKindCycleClose && a.Action == "close":
		if p.PositionID == "" || p.PositionID != positionID {
			return false
		}
	default:
		return false
	}
	if a.ExchangeOrderID != "" || a.StopLossOID != 0 || a.StopLossTriggerPx != 0 || a.PositionID != "" || a.RatchetFallbackNormalizePending {
		return false
	}
	return len(a.TPOIDs) == 0 && len(a.PrevTPOIDs) == 0 && len(a.TPArmedTiers) == 0
}

func paperSimPositionProofHolds(sc StrategyConfig, pos *Position) bool {
	if !paperManualHLStrategy(sc) || pos == nil || pos.TradePositionID == "" || !manualPositionOwnedByStrategy(pos, sc.ID) {
		return false
	}
	if pos.StopLossOID != 0 {
		return false
	}
	for _, oid := range pos.TPOIDs {
		if oid != 0 {
			return false
		}
	}
	var p paperSimPositionProof
	if !decodeStrictJSON(pos.PaperSimProof, &p) {
		return false
	}
	return p.Version == paperSimProofVersion && p.PositionID == pos.TradePositionID
}

func marshalPaperWarningReceipts(receipts []paperWarningReceipt) string {
	if len(receipts) == 0 {
		return ""
	}
	blob, err := json.Marshal(receipts)
	if err != nil {
		return ""
	}
	return string(blob)
}

func parsePaperWarningReceipts(raw string) []paperWarningReceipt {
	var receipts []paperWarningReceipt
	if !decodeStrictJSON(raw, &receipts) {
		return nil
	}
	return receipts
}

func paperWarningReceiptHeld(receipts []paperWarningReceipt, positionID, service, destination string) bool {
	if positionID == "" || service == "" || destination == "" {
		return false
	}
	for _, r := range receipts {
		if r.Version == paperWarningReceiptVersion && r.PositionID == positionID && r.Service == service && r.Destination == destination {
			return true
		}
	}
	return false
}

func paperWarningColumnsForSave(pos *Position, scope PortfolioScope) (string, string) {
	if pos == nil {
		return "", ""
	}
	if scope == ScopeLive {
		pos.PaperSimProof = ""
		pos.PaperWarningReceipts = nil
		return "", ""
	}
	return pos.PaperSimProof, marshalPaperWarningReceipts(pos.PaperWarningReceipts)
}

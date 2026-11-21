//go:build pyintegration

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	manualRegimeTestHLInfoEnv   = "GO_TRADER_TEST_HL_INFO_URL"
	manualRegimeTestHLWSEnv     = "GO_TRADER_TEST_HL_WS_URL"
	manualRegimeTestVenueLogEnv = "GO_TRADER_TEST_MANUAL_VENUE_LOG"
	manualRegimeTestFillPxEnv   = "GO_TRADER_TEST_MANUAL_FILL_PX"
	manualRegimeTestGateEnv     = "GO_TRADER_TEST_REGIME_GATE_DIR"
	manualEntryTestGateEnv      = "GO_TRADER_TEST_ENTRY_GATE_DIR"
	manualEntryTestSaveFaultEnv = "GO_TRADER_TEST_SAVE_FAULT_DIR"
	manualEntryTestCancelEnv    = "GO_TRADER_TEST_MANUAL_CANCEL_FILLED"
	manualEntryTestWaitEnv      = "GO_TRADER_TEST_MANUAL_ENTRY_WAIT_MS"

	manualEntryTestLimitOrigSize    = 0.01
	manualEntryTestLimitLotDecimals = 5
)

func manualEntryTestStageName(stage string) string {
	return strings.ReplaceAll(stage, " ", "-")
}

func manualEntryTestPartitionName(p RiskPartition) string {
	return strings.ReplaceAll(p.String(), ":", "-")
}

func manualEntryTestInstallGate(dir string) {
	manualEntryAdmissionBarrier = func(stage string) {
		name := manualEntryTestStageName(stage)
		if _, err := os.Stat(filepath.Join(dir, "arm-"+name)); err != nil {
			return
		}
		if err := os.WriteFile(filepath.Join(dir, "blocked-"+name), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
			fmt.Fprintf(os.Stderr, "entry gate %s: %v\n", dir, err)
			os.Exit(1)
		}
		deadline := time.Now().Add(5 * time.Minute)
		for {
			if _, err := os.Stat(filepath.Join(dir, "release-"+name)); err == nil {
				return
			}
			if time.Now().After(deadline) {
				fmt.Fprintf(os.Stderr, "entry gate %s: no release for %s\n", dir, stage)
				os.Exit(1)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func manualEntryTestInstallSaveFault(dir string) {
	stateSaveOutcomeFault = func(p RiskPartition) error {
		if _, err := os.Stat(filepath.Join(dir, manualEntryTestPartitionName(p))); err == nil {
			return fmt.Errorf("controlled %s save failure", partitionLabel(p))
		}
		return nil
	}
}

func manualRegimeTestInstallGate(dir string) {
	real := runRegimeBundleCheckFn
	var seq atomic.Int64
	runRegimeBundleCheckFn = func(ctx context.Context, req regimeBundleRequest, marketStdin []byte) (*RegimeBundle, error) {
		release := filepath.Join(dir, "release")
		if _, err := os.Stat(release); err != nil {
			n := seq.Add(1)
			if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("blocked-%d", n)), []byte(req.Key.String()), 0o600); err != nil {
				return nil, err
			}
			for {
				if _, err := os.Stat(release); err == nil {
					break
				}
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(10 * time.Millisecond):
				}
			}
			if _, err := os.Stat(filepath.Join(dir, "fail")); err == nil {
				return nil, fmt.Errorf("regime bundle %s: controlled classifier failure", req.Key)
			}
		}
		return real(ctx, req, marketStdin)
	}
}

var manualRegimeTestVenueMu sync.Mutex

func manualRegimeTestVenueRecord(path string, entry map[string]any) {
	manualRegimeTestVenueMu.Lock()
	defer manualRegimeTestVenueMu.Unlock()
	blob, err := json.Marshal(entry)
	if err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "manual venue substitute log %s: %v\n", path, err)
		os.Exit(1)
	}
	defer f.Close()
	f.Write(append(blob, '\n'))
}

func init() {
	if u := os.Getenv(manualRegimeTestHLInfoEnv); u != "" {
		hlMainnetURL = u
	}
	if u := os.Getenv(manualRegimeTestHLWSEnv); u != "" {
		hlFeedWebsocketURLOverride = u
	}
	if dir := os.Getenv(manualRegimeTestGateEnv); dir != "" {
		manualRegimeTestInstallGate(dir)
	}
	if dir := os.Getenv(manualEntryTestGateEnv); dir != "" {
		manualEntryTestInstallGate(dir)
	}
	if dir := os.Getenv(manualEntryTestSaveFaultEnv); dir != "" {
		manualEntryTestInstallSaveFault(dir)
	}
	if raw := os.Getenv(manualEntryTestWaitEnv); raw != "" {
		ms, err := strconv.Atoi(raw)
		if err != nil || ms <= 0 {
			fmt.Fprintf(os.Stderr, "%s needs a positive integer\n", manualEntryTestWaitEnv)
			os.Exit(1)
		}
		schedulerManualEntryWait = time.Duration(ms) * time.Millisecond
	}
	logPath := os.Getenv(manualRegimeTestVenueLogEnv)
	if logPath == "" {
		return
	}
	fillPx, err := strconv.ParseFloat(os.Getenv(manualRegimeTestFillPxEnv), 64)
	if err != nil || fillPx <= 0 {
		fmt.Fprintf(os.Stderr, "%s needs a positive %s\n", manualRegimeTestVenueLogEnv, manualRegimeTestFillPxEnv)
		os.Exit(1)
	}
	runHyperliquidExecuteFn = func(auth hlWriteAuth, script, symbol, side string, size, stopLossPct float64, cancelStopLossOID int64, prevPosQty float64, marginMode string, leverage float64, closeMode hlCloseMode, snapshot hlExecuteSnapshot, extraCancelOIDs ...int64) (*HyperliquidExecuteResult, string, error) {
		if err := auth.check(hlWriteExecute, symbol, 0); err != nil {
			manualRegimeTestVenueRecord(logPath, map[string]any{"kind": "refused_execute", "symbol": symbol})
			return nil, "", err
		}
		manualRegimeTestVenueRecord(logPath, map[string]any{"kind": "execute", "symbol": symbol, "side": side, "size": size, "cancel_oid": cancelStopLossOID, "extra_cancels": len(extraCancelOIDs)})
		return &HyperliquidExecuteResult{Platform: "hyperliquid", Execution: &HyperliquidExecution{Action: side, Symbol: symbol, Size: size, Fill: &HyperliquidFill{AvgPx: fillPx, TotalSz: size, OID: 7001}}}, "", nil
	}
	runHyperliquidLimitOpenFn = func(auth hlWriteAuth, script, symbol, side string, size, limitPx float64, tif, marginMode string, leverage float64, snapshot hlExecuteSnapshot) (*HyperliquidLimitOpenResult, string, error) {
		if err := auth.check(hlWriteLimitOpen, symbol, 0); err != nil {
			manualRegimeTestVenueRecord(logPath, map[string]any{"kind": "refused_limit_open", "symbol": symbol})
			return nil, "", err
		}
		manualRegimeTestVenueRecord(logPath, map[string]any{"kind": "limit_open", "symbol": symbol, "side": side, "size": size, "limit_px": limitPx})
		return &HyperliquidLimitOpenResult{Platform: "hyperliquid", Status: "resting", OrderOID: 8001, LimitPrice: limitPx, TIF: tif}, "", nil
	}
	cancelFilled, cancelOK := os.LookupEnv(manualEntryTestCancelEnv)
	filledSize, _ := strconv.ParseFloat(cancelFilled, 64)
	runHyperliquidCancelOrderFn = func(auth hlWriteAuth, script, symbol string, oid int64) (*HyperliquidCancelOrderResult, string, error) {
		if err := auth.check(hlWriteCancel, symbol, oid); err != nil {
			manualRegimeTestVenueRecord(logPath, map[string]any{"kind": "refused_cancel", "symbol": symbol, "oid": oid})
			return nil, "", err
		}
		manualRegimeTestVenueRecord(logPath, map[string]any{"kind": "cancel", "symbol": symbol, "oid": oid})
		if cancelOK {
			return &HyperliquidCancelOrderResult{Platform: "hyperliquid", OID: oid, Cancelled: true}, "", nil
		}
		return nil, "", fmt.Errorf("manual venue substitute refuses cancellations")
	}
	runHyperliquidLimitStatusFn = func(script, symbol string, oids []int64, sinceMs int64) (*HyperliquidLimitStatusResult, string, error) {
		manualRegimeTestVenueRecord(logPath, map[string]any{"kind": "limit_status", "symbol": symbol, "oids": len(oids)})
		if !cancelOK {
			return nil, "", fmt.Errorf("manual venue substitute refuses limit status reads")
		}
		resting := false
		proven := true
		lots := manualEntryTestLimitLotDecimals
		out := &HyperliquidLimitStatusResult{Platform: "hyperliquid"}
		for _, oid := range oids {
			orig, executed := manualEntryTestLimitOrigSize, filledSize
			out.Orders = append(out.Orders, HyperliquidLimitOrderStatus{
				OID: oid, Coin: symbol, Resting: &resting, OrderStatus: "canceled", OrderState: "cancelled",
				OrigSize: &orig, ExecutedSize: &executed, SzDecimals: &lots,
				FilledSize: filledSize, AvgPx: fillPx, FillsProven: &proven,
			})
		}
		return out, "", nil
	}
	runHyperliquidFetchATRFn = func(script, symbol, timeframe string, period int, atrMethod string) (*HyperliquidFetchATRResult, string, error) {
		manualRegimeTestVenueRecord(logPath, map[string]any{"kind": "fetch_atr", "symbol": symbol, "timeframe": timeframe})
		return nil, "", fmt.Errorf("manual venue substitute refuses private candle fetches")
	}
}

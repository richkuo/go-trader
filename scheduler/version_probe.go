package main

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

const pairMemberProbeRequestJSON = `{"pair_id":"probe","member_id":"probe","provider":"hyperliquid","instrument":"BTC","mode":"management_only"}`

var probeArgv = []string{
	"probe", "BTC", "1h",
	"--strategy-refs", `{"open":{"name":"probe","params":{}},"closes":[{"name":"probe_close","params":{}}]}`,
	"--mark-price=0",
	"--ohlcv-limit", "200",
	"--regime-windows-spec-json", `{"default":{"classifier":"adx","period":14,"adx_threshold":20}}`,
	"--regime-atr-window", "",
	"--regime-payload-json", `{"default":{"regime":"trending_up","score":0.5,"classifier":"adx","metrics":{"adx":25.0,"plus_di":20.0,"minus_di":10.0,"atr_pct":1.0}}}`,
	"--atr-method=simple",
	"--position-side", "long",
	"--position-avg-cost=1",
	"--position-qty=1",
	"--position-initial-qty=1",
	"--position-entry-atr=1",
	"--position-regime", "trending_up",
	"--position-risk-anchor-price=1",
	allowNoEdgeFlag,
	closedBarDecisionsFlag,
	decisionRegimeTimeframeFlag + "=1h",
	basketMemberFlag,
	pairMemberFlag + "=" + pairMemberProbeRequestJSON,
	"--probe-only",
}

var probeCompositeArgv = []string{
	"probe", "BTC", "1h",
	"--strategy-refs", `{"open":{"name":"probe","params":{}},"closes":[{"name":"probe_close","params":{}}]}`,
	"--mark-price=0",
	"--ohlcv-limit", "200",
	"--regime-windows-spec-json", `{"macro":{"classifier":"composite","period":14,"thresholds":{"return_eff":0.05,"range_eff":0.03,"adx":25}}}`,
	"--regime-atr-window", "",
	"--regime-payload-json", `{"macro":{"regime":"trending_up_clean","score":0.5,"classifier":"composite","metrics":{"adx":30.0}}}`,
	"--atr-method=simple",
	"--position-side", "long",
	"--position-avg-cost=1",
	"--position-qty=1",
	"--position-initial-qty=1",
	"--position-entry-atr=1",
	"--position-regime", "trending_up",
	"--position-risk-anchor-price=1",
	allowNoEdgeFlag,
	closedBarDecisionsFlag,
	decisionRegimeTimeframeFlag + "=1h",
	basketMemberFlag,
	pairMemberFlag + "=" + pairMemberProbeRequestJSON,
	"--probe-only",
}

const okxPairMemberProbeRequestJSON = `{"pair_id":"probe","member_id":"probe","provider":"okx","instrument":"BTC-USDT-SWAP","mode":"management_only"}`

var okxPairManagementProbeArgv = []string{
	"pair_member", "BTC", "1h", "--mode=paper", "--inst-type=swap",
	"--position-side", "long",
	"--position-avg-cost=1",
	"--position-qty=1",
	"--position-initial-qty=1",
	"--position-entry-atr=1",
	"--position-risk-anchor-price=1",
	"--strategy-refs", `{"open":{"name":"pair_member","params":{}},"closes":[{"name":"atr_stop","params":{"atr_mult":3}}]}`,
	pairMemberFlag + "=" + okxPairMemberProbeRequestJSON,
	pairManagementStdinFlag,
	"--probe-only",
}

var hyperliquidMarketCheckProbeArgv = append(append([]string{}, probeArgv[:len(probeArgv)-1]...),
	"--market-stdin", restingTPRuleFlag+"="+restingTPProbeRuleJSONSample, "--probe-only")

var hyperliquidMarketCheckCompositeProbeArgv = append(append([]string{}, probeCompositeArgv[:len(probeCompositeArgv)-1]...),
	"--market-stdin", restingTPRuleFlag+"="+restingTPProbeRuleJSONSample, "--probe-only")

var fetchATRProbeArgv = []string{
	"--fetch-atr", "--symbol=BTC", "--timeframe=1h", "--period=14", "--atr-method=simple", "--probe-only",
}

var llmReviewProbeArgv = []string{"--probe-only"}

var executeProbeArgv = []string{
	"--execute",
	"--symbol=BTC", "--side=buy", "--size=0",
	"--mode=paper",
	"--margin-mode=cross", "--leverage=1",
	"--account-leverage=1", "--account-margin-mode=cross",
	"--close-mode=reduce_only",
	"--cloid=0x00000000000000000000000000000001",
	"--stop-cloid=0x00000000000000000000000000000002",
	"--expires-after-ms=1",
	"--stop-expires-after-ms=1",
	"--probe-only",
}

var recoverEntryProbeArgv = []string{
	"--recover-entry",
	"--symbol=BTC",
	"--cloid=0x00000000000000000000000000000001",
	"--stop-cloid=0x00000000000000000000000000000002",
	"--since-ms=1",
	"--until-ms=2",
	"--mode=live",
	"--probe-only",
}

var okxExecuteProbeArgv = []string{
	"--execute",
	"--symbol=BTC", "--side=sell", "--size=0",
	"--mode=paper",
	"--inst-type=swap",
	"--client-order-id=gtprobe",
	"--exp-time-ms=1",
	"--reduce-only",
	"--probe-only",
}

var okxRecoverEntryProbeArgv = []string{
	"--recover-entry",
	"--symbol=BTC",
	"--inst-type=swap",
	"--client-order-id=gtprobe",
	"--since-ms=1",
	"--until-ms=2",
	"--mode=live",
	"--probe-only",
}

var sizedCloseProbeArgv = []string{
	"--symbol=BTC", "--mode=live", "--sz=0.01",
	"--side=sell", "--close-mode=reduce_only",
	"--cancel-stop-loss-oid=1", "--cancel-protection-after-close", "--cancel-min-fill=0.01",
	"--cloid=0x00000000000000000000000000000003",
	"--probe-only",
}

var basketRankerProbeArgv = []string{"--probe-only"}

var limitOpenProbeArgv = []string{
	"--limit-open",
	"--symbol=BTC", "--side=buy", "--size=0.01", "--limit-price=1",
	"--tif=Alo", "--margin-mode=cross", "--leverage=1",
	"--account-leverage=1", "--account-margin-mode=cross",
	"--probe-only",
}

var limitStatusProbeArgv = []string{
	"--limit-status", "--symbol=BTC", "--oids-json=[1]", "--probe-only",
}

var syncProtectionProbeArgv = []string{
	"--sync-protection",
	"--symbol=BTC", "--side=long", "--size=0.01",
	"--avg-cost=1", "--entry-atr=1", "--stop-loss-atr-mult=1",
	"--mode=live",
	"--stop-loss-trigger-px=1", "--preserve-moved-stop",
	`--tp-tiers-json=[{"atr_multiple":1,"close_fraction":0.5},{"atr_multiple":2,"close_fraction":1}]`,
	"--stop-loss-oid=1",
	"--tp-oids-json=[1,2]",
	"--tp-armed-tiers-json=[true,false]",
	"--force-sl-replace",
	"--force-tp-replace-json=[true,false]",
	"--cancel-tp-oids-json=[3]",
	"--reconcile-fill-hints-json=[]",
	"--probe-only",
}

var quotePlaceProbeArgv = []string{
	"--quote-place", "--symbol=BTC", "--side=buy", "--size=0.01", "--limit-price=1", "--tif=Alo",
	"--cloid=0x00000000000000000000000000000004", "--role=bid", "--expires-at-ms=1", "--reduce-only",
	"--mode=live", "--probe-only",
}

var quoteStopPlaceProbeArgv = []string{
	"--quote-place", "--symbol=BTC", "--side=sell", "--size=0.01", "--trigger-px=1",
	"--cloid=0x00000000000000000000000000000005", "--role=stop", "--expires-at-ms=1", "--reduce-only",
	"--mode=live", "--probe-only",
}

var quoteCancelProbeArgv = []string{
	"--quote-cancel", "--symbol=BTC", "--cloid=0x00000000000000000000000000000004", "--oid=1", "--mode=live", "--probe-only",
}

var quoteStatusProbeArgv = []string{
	"--quote-status", "--symbol=BTC", "--cloid=0x00000000000000000000000000000004", "--mode=live", "--probe-only",
}

var quoteRecoverProbeArgv = []string{
	"--quote-recover", "--symbol=BTC", "--since-ms=1", "--until-ms=2", "--mode=live", "--probe-only",
}

var quotePositionProbeArgv = []string{
	"--quote-position", "--symbol=BTC", "--mode=live", "--probe-only",
}

var quoteProbeArgvs = [][]string{quotePlaceProbeArgv, quoteStopPlaceProbeArgv, quoteCancelProbeArgv, quoteStatusProbeArgv, quoteRecoverProbeArgv, quotePositionProbeArgv}

var cancelOrderProbeArgv = []string{
	"--cancel-order", "--symbol=BTC", "--oid=1", "--probe-only",
}

var listOpenOrdersProbeArgv = []string{
	"--list-open-order-oids", "--probe-only",
}

var hyperliquidBatchProbeArgv = []string{
	"--batch-check", "--symbol=BTC", "--timeframe=1h",
	"--ohlcv-limit", "200", "--atr-method=simple", "--mark-price=0",
	"--regime-windows-spec-json", `{"default":{"classifier":"adx","period":14,"adx_threshold":20}}`,
	"--regime-payload-json", `{"default":{"regime":"trending_up","score":0.5,"classifier":"adx","metrics":{"adx":25.0}}}`,
	decisionRegimeTimeframeFlag + "=1h",
	"--market-stdin",
	"--probe-only",
}

var fetchCandlesProbeArgv = []string{
	"--platform=binanceus", "--type=spot", "--symbol=BTC/USDT", "--timeframe=1h", "--limit=1", "--probe-only",
}

var checkRegimeProbeArgv = []string{
	"--platform=binanceus", "--symbol=BTC/USDT", "--timeframe=1h",
	"--regime-windows-spec-json", `{"default":{"classifier":"adx","period":14,"adx_threshold":20}}`,
	"--ohlcv-limit", "200", "--min-bars", "30",
	"--probe-only",
}

var checkRegimeMarketProbeArgv = []string{
	"--platform=hyperliquid", "--symbol=BTC", "--timeframe=1h",
	"--regime-windows-spec-json", `{"default":{"classifier":"adx","period":14,"adx_threshold":20}}`,
	"--ohlcv-limit", "200", "--min-bars", "30",
	"--market-stdin",
	"--probe-only",
}

var strategyTunerSchemaProbeArgv = []string{
	"--type=spot", "--strategy=sma", "--probe-only",
}

var simulateStrategyProbeArgv = []string{"--probe-only"}

var robinhoodExecuteProbeArgv = []string{
	"--execute", "--symbol=BTC", "--side=buy", "--mode=live",
	"--ref-id=00000000-0000-4000-8000-000000000000", "--budget-ms=50000",
	"--amount_usd=1", "--probe-only",
}

var robinhoodExecuteSellProbeArgv = []string{
	"--execute", "--symbol=BTC", "--side=sell", "--mode=live",
	"--ref-id=00000000-0000-4000-8000-000000000000", "--budget-ms=50000",
	"--quantity=1", "--close-kind=whole", "--sole-owner", "--probe-only",
}

var robinhoodResolveProbeArgv = []string{
	"--resolve-order", "--symbol=BTC", "--side=sell",
	"--ref-id=00000000-0000-4000-8000-000000000000", "--budget-ms=50000",
	"--submitted-after-ms=1", "--submitted-before-ms=2", "--mode=live",
	"--order-id=probe", "--requested-quantity=1", "--expected-account-id=probe",
	"--cancel-open", "--verify-order-fields", "--probe-only",
}

var robinhoodCloseProbeArgv = []string{
	"--symbol=BTC", "--mode=live", "--quantity=1", "--budget-ms=50000",
	"--ref-id=00000000-0000-4000-8000-000000000000", "--probe-only",
}

const (
	pairOrderHLProbeRequestJSON     = `{"v":2,"pair_id":"probe","partition":"live","member_id":"probe","provider":"hyperliquid","account_key":"hyperliquid:mainnet:0x0000000000000000000000000000000000000001","instrument":"BTC","client_order_id":"0x00000000000000000000000000000005","purpose":"pair_exit","order_side":"sell","position_side":"long","base_qty":0.001,"native_qty":0.001,"contract_size":1,"price_bound":1,"accept_deadline_ms":1,"origin_slot_ms":1,"reduce_only":true,"contract_fingerprint":"0000000000000000000000000000000000000000000000000000000000000000","reference_max_age_ms":1,"reference_receipt_ms":1,"reference_px":"1"}`
	pairOrderOKXProbeRequestJSON    = `{"v":2,"pair_id":"probe","partition":"live","member_id":"probe","provider":"okx","account_key":"okx:swap:live:key-0000000000000000","instrument":"BTC-USDT-SWAP","client_order_id":"gtprobe","purpose":"pair_entry","order_side":"buy","position_side":"long","base_qty":0.01,"native_qty":1,"contract_size":0.01,"price_bound":1,"accept_deadline_ms":1,"quote_event_id":"pq-probe","origin_slot_ms":1,"reduce_only":false,"contract_fingerprint":"0000000000000000000000000000000000000000000000000000000000000000","capability_hash":"0000000000000000000000000000000000000000000000000000000000000000","evidence_hash":"0000000000000000000000000000000000000000000000000000000000000000"}`
	pairPositionHLProbeRequestJSON  = `{"v":1,"provider":"hyperliquid","account_key":"hyperliquid:mainnet:0x0000000000000000000000000000000000000001","instrument":"BTC","member_id":"probe","partition":"live"}`
	pairPositionOKXProbeRequestJSON = `{"v":1,"provider":"okx","account_key":"okx:swap:live:key-0000000000000000","instrument":"BTC-USDT-SWAP","member_id":"probe","partition":"live"}`
)

var pairOrderHLProbeArgvs = [][]string{
	{pairOrderFlag + "=" + pairOrderHLProbeRequestJSON, "--mode=live", "--probe-only"},
	{pairQueryFlag + "=" + pairOrderHLProbeRequestJSON, "--since-ms=1", "--mode=live", "--probe-only"},
	{pairPositionFlag + "=" + pairPositionHLProbeRequestJSON, "--mode=live", "--probe-only"},
}

var pairOrderOKXProbeArgvs = [][]string{
	{pairOrderFlag + "=" + pairOrderOKXProbeRequestJSON, "--mode=live", "--probe-only"},
	{pairQueryFlag + "=" + pairOrderOKXProbeRequestJSON, "--since-ms=1", "--mode=live", "--probe-only"},
}

var pairPositionOKXProbeArgv = []string{pairPositionFlag + "=" + pairPositionOKXProbeRequestJSON, "--mode=live", "--probe-only"}

var pairEvidenceHyperliquidProbeArgv = []string{"--provider=hyperliquid", "--instrument=BTC", "--probe-only"}

var pairEvidenceOKXProbeArgv = []string{"--provider=okx", "--instrument=BTC-USDT-SWAP", "--probe-only"}

const probeTimeout = 15 * time.Second

var probeOneCheckScriptFn = probeOneCheckScript

func probeCheckScripts(cfg *Config) error {
	scripts := uniqueCheckScripts(cfg)
	for _, script := range scripts {
		if err := probeOneCheckScriptFn(script, probeArgv); err != nil {
			return err
		}
		if err := probeOneCheckScriptFn(script, probeCompositeArgv); err != nil {
			return err
		}
		if filepath.Base(script) == "check_hyperliquid.py" {
			if err := probeOneCheckScriptFn(script, fetchATRProbeArgv); err != nil {
				return err
			}
			if err := probeOneCheckScriptFn(script, executeProbeArgv); err != nil {
				return err
			}
			if err := probeOneCheckScriptFn(script, limitOpenProbeArgv); err != nil {
				return err
			}
			if err := probeOneCheckScriptFn(script, limitStatusProbeArgv); err != nil {
				return err
			}
			if err := probeOneCheckScriptFn(script, syncProtectionProbeArgv); err != nil {
				return err
			}
			if err := probeOneCheckScriptFn(script, cancelOrderProbeArgv); err != nil {
				return err
			}
			if err := probeOneCheckScriptFn(script, listOpenOrdersProbeArgv); err != nil {
				return err
			}
			if err := probeOneCheckScriptFn(script, hyperliquidBatchProbeArgv); err != nil {
				return err
			}
			if err := probeOneCheckScriptFn(script, hyperliquidMarketCheckProbeArgv); err != nil {
				return err
			}
			if err := probeOneCheckScriptFn(script, hyperliquidMarketCheckCompositeProbeArgv); err != nil {
				return err
			}
			if err := probeOneCheckScriptFn(script, recoverEntryProbeArgv); err != nil {
				return err
			}
			for _, argv := range quoteProbeArgvs {
				if err := probeOneCheckScriptFn(script, argv); err != nil {
					return err
				}
			}
			for _, argv := range pairOrderHLProbeArgvs {
				if err := probeOneCheckScriptFn(script, argv); err != nil {
					return err
				}
			}
		}
		if filepath.Base(script) == "check_robinhood.py" {
			if err := probeOneCheckScriptFn(script, robinhoodExecuteProbeArgv); err != nil {
				return err
			}
			if err := probeOneCheckScriptFn(script, robinhoodExecuteSellProbeArgv); err != nil {
				return err
			}
			if err := probeOneCheckScriptFn(script, robinhoodResolveProbeArgv); err != nil {
				return err
			}
		}
		if filepath.Base(script) == "check_okx.py" {
			if err := probeOneCheckScriptFn(script, okxExecuteProbeArgv); err != nil {
				return err
			}
			if err := probeOneCheckScriptFn(script, okxRecoverEntryProbeArgv); err != nil {
				return err
			}
			for _, argv := range pairOrderOKXProbeArgvs {
				if err := probeOneCheckScriptFn(script, argv); err != nil {
					return err
				}
			}
			if err := probeOneCheckScriptFn(script, okxPairManagementProbeArgv); err != nil {
				return err
			}
		}
	}
	if anyRobinhoodLiveSpot(cfg) {
		if err := probeOneCheckScriptFn(robinhoodResolveScript, robinhoodExecuteSellProbeArgv); err != nil {
			return err
		}
		if err := probeOneCheckScriptFn(robinhoodResolveScript, robinhoodResolveProbeArgv); err != nil {
			return err
		}
		if err := probeOneCheckScriptFn(robinhoodLiveCloseScript, robinhoodCloseProbeArgv); err != nil {
			return err
		}
	}
	if anyHLLiveReconcilable(cfg) {
		if err := probeOneCheckScriptFn(hyperliquidLiveCloseScript, sizedCloseProbeArgv); err != nil {
			return err
		}
	}
	hlPerps, okxSwap := pairEvidenceProbeProviders(cfg)
	if hlPerps {
		if err := probeOneCheckScriptFn(pairEvidenceReaderScript, pairEvidenceHyperliquidProbeArgv); err != nil {
			return err
		}
		if err := probeOneCheckScriptFn(pairQuoteReaderScript, pairQuoteHLPublicProbeArgv); err != nil {
			return err
		}
		if err := probeOneCheckScriptFn(pairQuoteReaderScript, pairQuoteHLArchiveProbeArgv); err != nil {
			return err
		}
		for _, argv := range [][]string{pairHistoryHLCandlesProbeArgv, pairHistoryHLFundingProbeArgv} {
			if err := probeOneCheckScriptFn(pairHistoryReaderScript, argv); err != nil {
				return err
			}
		}
	}
	if okxSwap {
		if err := probeOneCheckScriptFn(pairEvidenceReaderScript, pairEvidenceOKXProbeArgv); err != nil {
			return err
		}
		if err := probeOneCheckScriptFn(pairQuoteReaderScript, pairQuoteOKXPublicProbeArgv); err != nil {
			return err
		}
		if err := probeOneCheckScriptFn(okxFetchPositionsScript, pairPositionOKXProbeArgv); err != nil {
			return err
		}
		for _, argv := range [][]string{pairHistoryOKXCandlesProbeArgv, pairHistoryOKXFundingProbeArgv} {
			if err := probeOneCheckScriptFn(pairHistoryReaderScript, argv); err != nil {
				return err
			}
		}
	}
	if len(cfg.Baskets) > 0 {
		if err := probeOneCheckScriptFn(basketRankerScript, basketRankerProbeArgv); err != nil {
			return err
		}
	}
	if anyStrategyUsesLLMEntryAnalysis(cfg) {
		if err := probeOneCheckScriptFn(llmEntryAnalysisScript, llmReviewProbeArgv); err != nil {
			return err
		}
	}
	if len(scripts) > 0 {
		if err := probeOneCheckScriptFn("shared_scripts/fetch_candles.py", fetchCandlesProbeArgv); err != nil {
			return err
		}
		if err := probeOneCheckScriptFn("shared_scripts/strategy_tuner_schema.py", strategyTunerSchemaProbeArgv); err != nil {
			return err
		}
		if err := probeOneCheckScriptFn(regimeCheckScript, checkRegimeProbeArgv); err != nil {
			return err
		}
		if err := probeOneCheckScriptFn(regimeCheckScript, checkRegimeMarketProbeArgv); err != nil {
			return err
		}
		if err := probeOneCheckScriptFn("shared_scripts/simulate_strategy.py", simulateStrategyProbeArgv); err != nil {
			return err
		}
		if err := probeOneCheckScriptFn(pairPolicyScript, pairPolicyProbeArgv); err != nil {
			return err
		}
	}
	return nil
}

func pairEvidenceProbeProviders(cfg *Config) (hlPerps, okxSwap bool) {
	for _, sc := range cfg.Strategies {
		switch {
		case sc.Platform == "hyperliquid" && sc.Type == "perps":
			hlPerps = true
		case sc.Platform == "okx" && sc.Type == "perps" && okxInstType(sc.Args) == "swap":
			okxSwap = true
		}
	}
	return hlPerps, okxSwap
}

func anyHLLiveReconcilable(cfg *Config) bool {
	for _, sc := range cfg.Strategies {
		if isHLLiveReconcilable(sc) {
			return true
		}
	}
	return false
}

func uniqueCheckScripts(cfg *Config) []string {
	seen := map[string]bool{}
	for _, sc := range cfg.Strategies {
		if sc.Script == "" || seen[sc.Script] {
			continue
		}
		seen[sc.Script] = true
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func probeOneCheckScript(script string, argv []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, ".venv/bin/python3", egressPythonArgv(script, argv)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if env := egressChildEnvironment(nil); env != nil {
		cmd.Env = env
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		if cmd.Process != nil {
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return fmt.Errorf("%s: probe timed out after %s", script, probeTimeout)
	}
	if err != nil {
		return formatProbeFailure(script, err, stderr.String(), stdout.String())
	}
	return nil
}

func formatProbeFailure(script string, runErr error, stderr, stdout string) error {
	stderr = strings.TrimSpace(stderr)
	stdout = strings.TrimSpace(stdout)
	detail := stderr
	if detail == "" {
		detail = stdout
	}
	if detail == "" {
		detail = runErr.Error()
	}
	if probeFailureScriptMissing(detail) {
		return fmt.Errorf("%s missing from deploy tree (sync Python with binary, e.g. scripts/update.sh): %s", script, detail)
	}
	return fmt.Errorf("%s rejected --probe-only argv (binary/Python version mismatch?): %s", script, detail)
}

func probeFailureScriptMissing(detail string) bool {
	return strings.Contains(detail, "can't open file")
}

func anyRobinhoodLiveSpot(cfg *Config) bool {
	for _, sc := range cfg.Strategies {
		if isRobinhoodLiveSpot(sc) {
			return true
		}
	}
	return false
}

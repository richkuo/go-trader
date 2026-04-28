package main

import (
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

var tradingViewCSVHeader = []string{"Symbol", "Side", "Qty", "Status", "Fill Price", "Commission", "Closing Time"}

var tradingViewCryptoQuotes = []string{"USDT", "USDC", "USD", "BTC", "ETH"}

type repeatedStringFlag []string

func (f *repeatedStringFlag) String() string {
	return strings.Join(*f, ",")
}

func (f *repeatedStringFlag) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("strategy id cannot be empty")
	}
	*f = append(*f, value)
	return nil
}

type tradingViewExportOptions struct {
	All         bool
	StrategyIDs []string
	OutputPath  string
}

func runExport(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: go-trader export tradingview [--config scheduler/config.json] (--all | --strategy <id>...) --output <file>")
		return 2
	}
	switch args[0] {
	case "tradingview":
		return runTradingViewExport(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "Unknown export target %q\n", args[0])
		return 2
	}
}

func runTradingViewExport(args []string) int {
	fs := flag.NewFlagSet("export tradingview", flag.ContinueOnError)
	configPath := fs.String("config", "scheduler/config.json", "Path to config file")
	outputPath := fs.String("output", "", "Output CSV path")
	all := fs.Bool("all", false, "Export all configured strategies with trade data")
	var strategyIDs repeatedStringFlag
	fs.Var(&strategyIDs, "strategy", "Strategy ID to export; may be specified multiple times")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "Unexpected arguments: %s\n", strings.Join(fs.Args(), " "))
		return 2
	}

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		return 1
	}
	stateDB, err := OpenStateDB(cfg.DBFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to open state DB: %v\n", err)
		return 1
	}
	defer stateDB.Close()

	n, err := exportTradingViewCSVFile(stateDB, cfg, tradingViewExportOptions{
		All:         *all,
		StrategyIDs: []string(strategyIDs),
		OutputPath:  *outputPath,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "TradingView export failed: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "Exported %d TradingView transaction rows to %s\n", n, *outputPath)
	return 0
}

func exportTradingViewCSVFile(stateDB *StateDB, cfg *Config, opts tradingViewExportOptions) (int, error) {
	if strings.TrimSpace(opts.OutputPath) == "" {
		return 0, fmt.Errorf("--output is required")
	}
	strategies, err := selectTradingViewExportStrategies(cfg, opts)
	if err != nil {
		return 0, err
	}
	ids := make([]string, 0, len(strategies))
	for _, sc := range strategies {
		ids = append(ids, sc.ID)
	}
	trades, err := stateDB.QueryTradingViewExportTrades(ids)
	if err != nil {
		return 0, err
	}
	rows, err := buildTradingViewCSVRows(strategies, trades, cfg.TradingViewExport.SymbolOverrides, !opts.All)
	if err != nil {
		return 0, err
	}
	if _, err := os.Stat(opts.OutputPath); err == nil {
		fmt.Fprintf(os.Stderr, "[WARN] overwriting existing TradingView CSV: %s\n", opts.OutputPath)
	} else if !os.IsNotExist(err) {
		return 0, fmt.Errorf("check output CSV: %w", err)
	}
	f, err := os.Create(opts.OutputPath)
	if err != nil {
		return 0, fmt.Errorf("create output CSV: %w", err)
	}
	defer f.Close()
	if err := writeTradingViewCSV(f, rows); err != nil {
		return 0, err
	}
	return len(rows), nil
}

func selectTradingViewExportStrategies(cfg *Config, opts tradingViewExportOptions) ([]StrategyConfig, error) {
	if opts.All && len(opts.StrategyIDs) > 0 {
		return nil, fmt.Errorf("use either --all or --strategy, not both")
	}
	if !opts.All && len(opts.StrategyIDs) == 0 {
		return nil, fmt.Errorf("choose --all or at least one --strategy")
	}

	byID := make(map[string]StrategyConfig, len(cfg.Strategies))
	for _, sc := range cfg.Strategies {
		if _, dup := byID[sc.ID]; dup {
			return nil, fmt.Errorf("config has duplicate strategy id %q", sc.ID)
		}
		byID[sc.ID] = sc
	}
	if opts.All {
		ids := make([]string, 0, len(byID))
		for id := range byID {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		out := make([]StrategyConfig, 0, len(ids))
		for _, id := range ids {
			out = append(out, byID[id])
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("no configured strategies to export")
		}
		return out, nil
	}

	seen := make(map[string]bool, len(opts.StrategyIDs))
	out := make([]StrategyConfig, 0, len(opts.StrategyIDs))
	for _, id := range opts.StrategyIDs {
		id = strings.TrimSpace(id)
		if seen[id] {
			continue
		}
		sc, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("strategy %q is not in config", id)
		}
		seen[id] = true
		out = append(out, sc)
	}
	return out, nil
}

func buildTradingViewCSVRows(strategies []StrategyConfig, trades []Trade, overrides map[string]string, requireEachStrategy bool) ([][]string, error) {
	byID := make(map[string]StrategyConfig, len(strategies))
	required := make(map[string]bool, len(strategies))
	for _, sc := range strategies {
		byID[sc.ID] = sc
		required[sc.ID] = true
	}

	var rows [][]string
	seenTrades := make(map[string]int, len(strategies))
	for _, trade := range trades {
		sc, ok := byID[trade.StrategyID]
		if !ok {
			continue
		}
		row, err := tradingViewCSVRow(sc, trade, overrides)
		if err != nil {
			return nil, err
		}
		rows = append(rows, row)
		seenTrades[trade.StrategyID]++
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("no trade data found for selected strategies")
	}
	if requireEachStrategy {
		var missing []string
		for id := range required {
			if seenTrades[id] == 0 {
				missing = append(missing, id)
			}
		}
		sort.Strings(missing)
		if len(missing) > 0 {
			return nil, fmt.Errorf("no trade data found for strategy %s", strings.Join(missing, ", "))
		}
	}
	return rows, nil
}

func tradingViewCSVRow(sc StrategyConfig, trade Trade, overrides map[string]string) ([]string, error) {
	side, err := tradingViewSide(sc, trade)
	if err != nil {
		return nil, err
	}
	if trade.Quantity <= 0 {
		return nil, fmt.Errorf("strategy %s trade %s at %s has non-positive quantity %g", trade.StrategyID, tradingViewTradeRef(trade), formatTime(trade.Timestamp), trade.Quantity)
	}
	if trade.Price <= 0 {
		return nil, fmt.Errorf("strategy %s trade %s at %s has non-positive price %g", trade.StrategyID, tradingViewTradeRef(trade), formatTime(trade.Timestamp), trade.Price)
	}
	tvSymbol, err := tradingViewSymbol(sc, trade, overrides)
	if err != nil {
		return nil, err
	}
	commission := trade.ExchangeFee
	return []string{
		tvSymbol,
		side,
		formatTradingViewFloat(trade.Quantity),
		"Filled",
		formatTradingViewFloat(trade.Price),
		formatTradingViewFloat(commission),
		formatTradingViewTime(trade.Timestamp),
	}, nil
}

func tradingViewSide(sc StrategyConfig, trade Trade) (string, error) {
	side := strings.ToLower(strings.TrimSpace(trade.Side))
	switch side {
	case "buy", "sell":
		return side, nil
	case "close":
		if strings.EqualFold(trade.TradeType, "options") || strings.EqualFold(sc.Type, "options") {
			return tradingViewOptionCloseSide(trade)
		}
		// Close trades from circuit-breaker / portfolio paths encode the
		// position direction in Details ("Close long" / "Close short"); see
		// risk.go and portfolio.go. Closing a long → sell, closing a short → buy.
		details := strings.ToLower(trade.Details)
		switch {
		case strings.Contains(details, "close long"):
			return "sell", nil
		case strings.Contains(details, "close short"):
			return "buy", nil
		}
		return "", fmt.Errorf("strategy %s trade %s at %s has close side without persisted position direction; cannot map to TradingView buy/sell", trade.StrategyID, tradingViewTradeRef(trade), formatTime(trade.Timestamp))
	default:
		return "", fmt.Errorf("strategy %s trade %s at %s has unsupported side %q", trade.StrategyID, tradingViewTradeRef(trade), formatTime(trade.Timestamp), trade.Side)
	}
}

// tradingViewTradeRef returns a short reference (exchange order id when set,
// otherwise the symbol) to make export-error logs traceable.
func tradingViewTradeRef(trade Trade) string {
	if id := strings.TrimSpace(trade.ExchangeOrderID); id != "" {
		return "order=" + id
	}
	return "symbol=" + strings.TrimSpace(trade.Symbol)
}

func tradingViewOptionCloseSide(trade Trade) (string, error) {
	parts := strings.FieldsFunc(strings.ToLower(strings.TrimSpace(trade.Symbol)), func(r rune) bool {
		return r == '-' || r == '_' || r == ' '
	})
	for _, part := range parts {
		switch part {
		case "sell", "short", "sold":
			return "buy", nil
		case "buy", "long", "bought":
			return "sell", nil
		}
	}
	// Legacy long-option position IDs do not include action; closing a bought
	// option is a sell transaction in TradingView.
	return "sell", nil
}

func writeTradingViewCSV(w io.Writer, rows [][]string) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(tradingViewCSVHeader); err != nil {
		return fmt.Errorf("write CSV header: %w", err)
	}
	for _, row := range rows {
		if err := cw.Write(row); err != nil {
			return fmt.Errorf("write CSV row: %w", err)
		}
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return fmt.Errorf("flush CSV: %w", err)
	}
	return nil
}

func tradingViewSymbol(sc StrategyConfig, trade Trade, overrides map[string]string) (string, error) {
	if symbol := findTradingViewSymbolOverride(sc, trade, overrides); symbol != "" {
		return symbol, nil
	}
	raw := strings.TrimSpace(trade.Symbol)
	if raw == "" {
		return "", fmt.Errorf("strategy %s trade has empty symbol", trade.StrategyID)
	}
	if strings.Contains(raw, ":") {
		return strings.ToUpper(raw), nil
	}
	ticker := normalizeTradingViewTicker(raw)
	if ticker == "" {
		return "", fmt.Errorf("strategy %s trade has invalid symbol %q", trade.StrategyID, trade.Symbol)
	}

	platform := strings.ToLower(strings.TrimSpace(sc.Platform))
	tradeType := strings.ToLower(strings.TrimSpace(trade.TradeType))
	if tradeType == "" {
		tradeType = strings.ToLower(strings.TrimSpace(sc.Type))
	}
	switch platform {
	case "binanceus":
		if isLikelyCryptoPair(ticker) {
			return "BINANCEUS:" + ticker, nil
		}
	case "okx":
		if strings.Contains(strings.ToUpper(raw), "SWAP") || tradeType == "perps" {
			if base, quote, ok := splitCryptoPair(raw); ok {
				return "OKX:" + base + quote + ".P", nil
			}
		}
		if isLikelyCryptoPair(ticker) {
			return "OKX:" + ticker, nil
		}
	}
	return "", fmt.Errorf("no TradingView symbol mapping for strategy %s platform=%q symbol=%q; add tradingview_export.symbol_overrides", sc.ID, sc.Platform, trade.Symbol)
}

func findTradingViewSymbolOverride(sc StrategyConfig, trade Trade, overrides map[string]string) string {
	if len(overrides) == 0 {
		return ""
	}
	raw := strings.TrimSpace(trade.Symbol)
	normalized := normalizeTradingViewTicker(raw)
	platforms := platformOverrideAliases(sc.Platform)
	candidates := []string{
		sc.ID + ":" + raw,
		sc.ID + ":" + normalized,
	}
	for _, platform := range platforms {
		candidates = append(candidates,
			platform+":"+raw,
			platform+":"+normalized,
		)
	}
	candidates = append(candidates, raw, normalized)
	for _, key := range candidates {
		if value := strings.TrimSpace(overrides[key]); value != "" {
			return strings.ToUpper(value)
		}
	}
	return ""
}

func platformOverrideAliases(platform string) []string {
	p := strings.ToLower(strings.TrimSpace(platform))
	switch p {
	case "hyperliquid":
		return []string{"hyperliquid", "hl"}
	case "binanceus":
		return []string{"binanceus", "bus"}
	case "robinhood":
		return []string{"robinhood", "rh"}
	case "topstep":
		return []string{"topstep", "ts"}
	case "deribit":
		return []string{"deribit"}
	case "ibkr":
		return []string{"ibkr"}
	case "okx":
		return []string{"okx"}
	case "luno":
		return []string{"luno"}
	default:
		if p == "" {
			return nil
		}
		return []string{p}
	}
}

func normalizeTradingViewTicker(symbol string) string {
	s := strings.ToUpper(strings.TrimSpace(symbol))
	replacer := strings.NewReplacer("/", "", "-", "", "_", "", " ", "")
	return replacer.Replace(s)
}

func isLikelyCryptoPair(ticker string) bool {
	for _, quote := range tradingViewCryptoQuotes {
		if strings.HasSuffix(ticker, quote) && len(ticker) > len(quote) {
			return true
		}
	}
	return false
}

func splitCryptoPair(symbol string) (string, string, bool) {
	parts := strings.FieldsFunc(strings.ToUpper(strings.TrimSpace(symbol)), func(r rune) bool {
		return r == '/' || r == '-' || r == '_' || r == ' '
	})
	// Filter instrument suffixes so BTC-USDT-SWAP becomes BTC/USDT.
	filtered := parts[:0]
	for _, part := range parts {
		if part != "" && part != "SWAP" && part != "PERP" && part != "PERPS" {
			filtered = append(filtered, part)
		}
	}
	if len(filtered) >= 2 {
		return filtered[0], filtered[1], true
	}
	ticker := normalizeTradingViewTicker(symbol)
	for _, quote := range tradingViewCryptoQuotes {
		if strings.HasSuffix(ticker, quote) && len(ticker) > len(quote) {
			return strings.TrimSuffix(ticker, quote), quote, true
		}
	}
	return "", "", false
}

func formatTradingViewFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func formatTradingViewTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02 15:04:05")
}

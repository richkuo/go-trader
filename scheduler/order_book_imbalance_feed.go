package main

import (
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strconv"
)

const (
	orderBookImbalanceStrategyName = "order_book_imbalance"
	orderBookImbalanceWindowMs     = int64(60_000)
	orderBookImbalanceCapacity     = 256
)

var orderBookImbalanceParamNames = map[string]bool{
	"depth_levels": true, "entry_threshold": true, "confirmation_observations": true,
	"max_spread_bps": true, "min_combined_depth_usd": true, "max_snapshot_age_ms": true,
}

func init() {
	feedBookStrategyNeeds[orderBookImbalanceStrategyName] = orderBookImbalanceBookNeed
}

func obiParamNumber(params map[string]interface{}, name string, def float64) (float64, bool, error) {
	raw, ok := params[name]
	if !ok {
		return def, false, nil
	}
	var v float64
	switch t := raw.(type) {
	case float64:
		v = t
	case int:
		v = float64(t)
	case int64:
		v = float64(t)
	case json.Number:
		f, err := t.Float64()
		if err != nil {
			return 0, true, fmt.Errorf("%s: %s %q is not a number", orderBookImbalanceStrategyName, name, t.String())
		}
		v = f
	default:
		return 0, true, fmt.Errorf("%s: %s must be a number, got %v", orderBookImbalanceStrategyName, name, raw)
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, true, fmt.Errorf("%s: %s must be finite, got %v", orderBookImbalanceStrategyName, name, raw)
	}
	return v, true, nil
}

func obiParamInt(params map[string]interface{}, name string, def, lo, hi int64) (int64, error) {
	v, _, err := obiParamNumber(params, name, float64(def))
	if err != nil {
		return 0, err
	}
	if v != math.Trunc(v) || v < float64(lo) || v > float64(hi) {
		return 0, fmt.Errorf("%s: %s must be an integer in [%d, %d], got %v", orderBookImbalanceStrategyName, name, lo, hi, v)
	}
	return int64(v), nil
}

func obiDecimal(v float64) *big.Rat {
	r, _ := new(big.Rat).SetString(strconv.FormatFloat(v, 'g', -1, 64))
	return r
}

func obiParamDecimal(params map[string]interface{}, name string, def, loExclusive, hi float64, hiInclusive bool) error {
	v, _, err := obiParamNumber(params, name, def)
	if err != nil {
		return err
	}
	r := obiDecimal(v)
	lo, top := obiDecimal(loExclusive), obiDecimal(hi)
	bad := r.Cmp(lo) <= 0 || r.Cmp(top) > 0 || (!hiInclusive && r.Cmp(top) == 0)
	if bad {
		closing := "]"
		if !hiInclusive {
			closing = ")"
		}
		return fmt.Errorf("%s: %s must be in (%v, %v%s, got %v", orderBookImbalanceStrategyName, name, loExclusive, hi, closing, v)
	}
	return nil
}

func validateOrderBookImbalanceParams(params map[string]interface{}) (int, error) {
	var unknown []string
	for k := range params {
		if !orderBookImbalanceParamNames[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return 0, fmt.Errorf("%s: unknown parameters %v", orderBookImbalanceStrategyName, unknown)
	}
	for name, raw := range params {
		if _, isBool := raw.(bool); isBool {
			return 0, fmt.Errorf("%s: %s must be a number, got a boolean", orderBookImbalanceStrategyName, name)
		}
	}
	depth, err := obiParamInt(params, "depth_levels", 5, 1, int64(feedBookMaxLevels))
	if err != nil {
		return 0, err
	}
	if _, err := obiParamInt(params, "confirmation_observations", 3, 2, 5); err != nil {
		return 0, err
	}
	if _, err := obiParamInt(params, "max_snapshot_age_ms", 2000, 250, 5000); err != nil {
		return 0, err
	}
	if err := obiParamDecimal(params, "entry_threshold", 0.30, 0, 1, false); err != nil {
		return 0, err
	}
	if err := obiParamDecimal(params, "max_spread_bps", 5, 0, 100, true); err != nil {
		return 0, err
	}
	if err := obiParamDecimal(params, "min_combined_depth_usd", 100000, 0, 1_000_000_000, true); err != nil {
		return 0, err
	}
	return int(depth), nil
}

func orderBookImbalanceBookNeed(sc StrategyConfig, intervalMs int64) (feedBookNeed, error) {
	params := sc.OpenStrategy.Params
	if params == nil {
		params = map[string]interface{}{}
	}
	depth, err := validateOrderBookImbalanceParams(params)
	if err != nil {
		return feedBookNeed{}, err
	}
	return contractBookNeed(orderBookImbalanceWindowMs, orderBookImbalanceCapacity, depth), nil
}

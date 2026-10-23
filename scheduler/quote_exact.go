package main

import (
	"fmt"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
)

var quoteDecimalPattern = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][-+]?[0-9]{1,3})?$`)

func quoteRatZero() *big.Rat { return new(big.Rat) }

func parseQuoteDecimal(raw string) (*big.Rat, error) {
	text := strings.TrimSpace(raw)
	if !quoteDecimalPattern.MatchString(text) {
		return nil, fmt.Errorf("%q is not a decimal number", raw)
	}
	r, ok := new(big.Rat).SetString(text)
	if !ok {
		return nil, fmt.Errorf("%q is not a decimal number", raw)
	}
	return r, nil
}

func parseQuoteRat(raw string) (*big.Rat, error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return quoteRatZero(), nil
	}
	if strings.Contains(text, "/") {
		r, ok := new(big.Rat).SetString(text)
		if !ok {
			return nil, fmt.Errorf("%q is not an exact rational", raw)
		}
		return r, nil
	}
	return parseQuoteDecimal(text)
}

func mustQuoteRat(raw string) *big.Rat {
	r, err := parseQuoteRat(raw)
	if err != nil {
		panic(err)
	}
	return r
}

func quoteRatString(r *big.Rat) string {
	if r == nil {
		return "0"
	}
	return r.RatString()
}

func quoteRatFloat(r *big.Rat) float64 {
	if r == nil {
		return 0
	}
	f, _ := r.Float64()
	return f
}

func quoteRatFromFloat(f float64) (*big.Rat, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil, fmt.Errorf("value %v is not finite", f)
	}
	return new(big.Rat).SetFloat64(f), nil
}

func quoteRatFromFloatDecimal(f float64) (*big.Rat, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil, fmt.Errorf("value %v is not finite", f)
	}
	return parseQuoteDecimal(strconv.FormatFloat(f, 'g', -1, 64))
}

func quoteRatDecimalString(r *big.Rat, decimals int) string {
	if r == nil {
		return "0"
	}
	return r.FloatString(decimals)
}

func quoteRatAbs(r *big.Rat) *big.Rat { return new(big.Rat).Abs(r) }

func quoteRatAdd(a, b *big.Rat) *big.Rat { return new(big.Rat).Add(a, b) }

func quoteRatSub(a, b *big.Rat) *big.Rat { return new(big.Rat).Sub(a, b) }

func quoteRatMul(a, b *big.Rat) *big.Rat { return new(big.Rat).Mul(a, b) }

func quoteRatQuo(a, b *big.Rat) *big.Rat {
	if b.Sign() == 0 {
		return quoteRatZero()
	}
	return new(big.Rat).Quo(a, b)
}

func quoteRatMin(a, b *big.Rat) *big.Rat {
	if a.Cmp(b) <= 0 {
		return new(big.Rat).Set(a)
	}
	return new(big.Rat).Set(b)
}

func quoteRatNeg(a *big.Rat) *big.Rat { return new(big.Rat).Neg(a) }

func quoteProjectionOK(exact *big.Rat, projected float64) bool {
	if math.IsNaN(projected) || math.IsInf(projected, 0) {
		return false
	}
	want := quoteRatFloat(exact)
	if want == projected {
		return true
	}
	scale := math.Max(1, math.Max(math.Abs(want), math.Abs(projected)))
	return math.Abs(want-projected) <= 1e-12*scale
}

func pow10Rat(n int) *big.Rat {
	if n >= 0 {
		return new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil))
	}
	return new(big.Rat).SetFrac(big.NewInt(1), new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(-n)), nil))
}

func quoteRatFloorTo(r *big.Rat, decimals int) *big.Rat {
	scaled := new(big.Rat).Mul(r, pow10Rat(decimals))
	q := new(big.Int).Div(scaled.Num(), scaled.Denom())
	return new(big.Rat).Mul(new(big.Rat).SetInt(q), pow10Rat(-decimals))
}

func quoteRatCeilTo(r *big.Rat, decimals int) *big.Rat {
	scaled := new(big.Rat).Mul(r, pow10Rat(decimals))
	q, m := new(big.Int).DivMod(scaled.Num(), scaled.Denom(), new(big.Int))
	if m.Sign() != 0 {
		q.Add(q, big.NewInt(1))
	}
	return new(big.Rat).Mul(new(big.Rat).SetInt(q), pow10Rat(-decimals))
}

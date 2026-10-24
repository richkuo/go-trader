package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

const (
	pairEvidenceVersion = 2

	pairPriceRuleHyperliquid = "hyperliquid_perps_v1"
	pairPriceRuleFixedTick   = "fixed_tick_v1"

	pairDerivationHyperliquid = "hyperliquid_info_meta_v1"
	pairDerivationOKX         = "okx_public_instrument_v1"

	pairHyperliquidCurrency    = "USDC"
	pairHyperliquidMinNotional = "10"
	pairOKXCurrency            = "USDT"
	pairOKXMinNotional         = "0"
	pairUSD                    = "USD"

	pairSourceSealedFeed     = "sealed_feed"
	pairSourceArchive        = "archive"
	pairSourcePublicSnapshot = "public_snapshot"

	pairEvidenceMaxDocumentBytes = 16 << 20
	pairEvidenceMaxDepth         = 64
	pairEvidenceMaxIDLen         = 128
	pairEvidenceMaxProvenanceLen = 256
)

var pairDerivationIDs = map[string]string{
	entryProviderHyperliquid: pairDerivationHyperliquid,
	entryProviderOKX:         pairDerivationOKX,
}

var pairPriceRules = map[string]string{
	entryProviderHyperliquid: pairPriceRuleHyperliquid,
	entryProviderOKX:         pairPriceRuleFixedTick,
}

var pairMemberSources = map[string][]string{
	entryProviderHyperliquid: {pairSourceArchive, pairSourcePublicSnapshot, pairSourceSealedFeed},
	entryProviderOKX:         {pairSourcePublicSnapshot},
}

var (
	pairDecimalCanonicalPattern = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]*[1-9])?$`)
	pairDecimalInputPattern     = regexp.MustCompile(`^-?[0-9]+(\.[0-9]+)?([eE][-+]?[0-9]{1,4})?$`)
	pairPlainDecimalPattern     = regexp.MustCompile(`^(0|[1-9][0-9]*)(\.[0-9]+)?$`)
	pairIntegerPattern          = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)
	pairCurrencyPattern         = regexp.MustCompile(`^[A-Z0-9]{2,16}$`)
	pairHexDigestPattern        = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type pairJSONNumber string

type pairDecimal string

func pairRatDecimal(r *big.Rat) (string, error) {
	if r == nil {
		return "", fmt.Errorf("decimal is missing")
	}
	if r.Sign() == 0 {
		return "0", nil
	}
	d := new(big.Int).Set(r.Denom())
	two, five, zero := big.NewInt(2), big.NewInt(5), big.NewInt(0)
	twos, fives := 0, 0
	m := new(big.Int)
	for {
		q, rem := new(big.Int).DivMod(d, two, m)
		if rem.Cmp(zero) != 0 {
			break
		}
		d, twos = q, twos+1
	}
	for {
		q, rem := new(big.Int).DivMod(d, five, m)
		if rem.Cmp(zero) != 0 {
			break
		}
		d, fives = q, fives+1
	}
	if d.Cmp(big.NewInt(1)) != 0 {
		return "", fmt.Errorf("%s has no finite decimal form", r.RatString())
	}
	digits := twos
	if fives > digits {
		digits = fives
	}
	s := r.FloatString(digits)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(s, "0")
		s = strings.TrimSuffix(s, ".")
	}
	return s, nil
}

func pairParseDecimalInput(raw string) (*big.Rat, error) {
	if !pairDecimalInputPattern.MatchString(raw) {
		return nil, fmt.Errorf("%q is not a decimal number", raw)
	}
	r, ok := new(big.Rat).SetString(raw)
	if !ok {
		return nil, fmt.Errorf("%q is not a decimal number", raw)
	}
	return r, nil
}

func pairNormalizeDecimal(raw string) (string, error) {
	r, err := pairParseDecimalInput(raw)
	if err != nil {
		return "", err
	}
	return pairRatDecimal(r)
}

func pairDecimalCanonical(s string) bool {
	return pairDecimalCanonicalPattern.MatchString(s) && s != "-0"
}

func (d pairDecimal) rat() (*big.Rat, error) {
	if !pairDecimalCanonical(string(d)) {
		return nil, fmt.Errorf("decimal %q is not in canonical form", string(d))
	}
	r, ok := new(big.Rat).SetString(string(d))
	if !ok {
		return nil, fmt.Errorf("decimal %q is unreadable", string(d))
	}
	return r, nil
}

func (d pairDecimal) MarshalJSON() ([]byte, error) {
	s, err := pairNormalizeDecimal(string(d))
	if err != nil {
		return nil, err
	}
	return json.Marshal(s)
}

func (d *pairDecimal) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("decimal must be a JSON string: %w", err)
	}
	if !pairDecimalCanonical(s) {
		return fmt.Errorf("decimal %q is not in canonical form (no exponent, sign, redundant zero or negative zero)", s)
	}
	*d = pairDecimal(s)
	return nil
}

func pairDecimalFromFloat(f float64) (pairDecimal, error) {
	r, err := quoteRatFromFloatDecimal(f)
	if err != nil {
		return "", err
	}
	s, err := pairRatDecimal(r)
	return pairDecimal(s), err
}

type pairStrictParser struct {
	b     []byte
	i     int
	depth int
}

func pairStrictParse(b []byte) (interface{}, error) {
	if !utf8.Valid(b) {
		return nil, fmt.Errorf("JSON is not valid UTF-8")
	}
	p := &pairStrictParser{b: b}
	p.ws()
	v, err := p.value()
	if err != nil {
		return nil, err
	}
	p.ws()
	if p.i != len(p.b) {
		return nil, fmt.Errorf("JSON has a trailing value at byte %d", p.i)
	}
	return v, nil
}

func (p *pairStrictParser) ws() {
	for p.i < len(p.b) {
		switch p.b[p.i] {
		case ' ', '\t', '\n', '\r':
			p.i++
		default:
			return
		}
	}
}

func (p *pairStrictParser) fail(format string, a ...interface{}) error {
	return fmt.Errorf("JSON byte %d: %s", p.i, fmt.Sprintf(format, a...))
}

func (p *pairStrictParser) value() (interface{}, error) {
	if p.i >= len(p.b) {
		return nil, p.fail("unexpected end of input")
	}
	switch c := p.b[p.i]; {
	case c == '{':
		return p.object()
	case c == '[':
		return p.array()
	case c == '"':
		return p.str()
	case c == 't':
		return true, p.literal("true")
	case c == 'f':
		return false, p.literal("false")
	case c == 'n':
		return nil, p.literal("null")
	case c == '-' || (c >= '0' && c <= '9'):
		return p.number()
	}
	return nil, p.fail("unexpected character %q", p.b[p.i])
}

func (p *pairStrictParser) literal(word string) error {
	if !bytes.HasPrefix(p.b[p.i:], []byte(word)) {
		return p.fail("invalid literal")
	}
	p.i += len(word)
	return nil
}

func (p *pairStrictParser) enter() error {
	p.depth++
	if p.depth > pairEvidenceMaxDepth {
		return p.fail("nesting is deeper than %d", pairEvidenceMaxDepth)
	}
	return nil
}

func (p *pairStrictParser) object() (interface{}, error) {
	if err := p.enter(); err != nil {
		return nil, err
	}
	defer func() { p.depth-- }()
	p.i++
	out := map[string]interface{}{}
	p.ws()
	if p.i < len(p.b) && p.b[p.i] == '}' {
		p.i++
		return out, nil
	}
	for {
		p.ws()
		if p.i >= len(p.b) || p.b[p.i] != '"' {
			return nil, p.fail("object key must be a string")
		}
		k, err := p.str()
		if err != nil {
			return nil, err
		}
		key := k.(string)
		if _, dup := out[key]; dup {
			return nil, p.fail("duplicate object key %q", key)
		}
		p.ws()
		if p.i >= len(p.b) || p.b[p.i] != ':' {
			return nil, p.fail("missing ':' after object key")
		}
		p.i++
		p.ws()
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		out[key] = v
		p.ws()
		if p.i >= len(p.b) {
			return nil, p.fail("unterminated object")
		}
		if p.b[p.i] == ',' {
			p.i++
			continue
		}
		if p.b[p.i] == '}' {
			p.i++
			return out, nil
		}
		return nil, p.fail("expected ',' or '}' in object")
	}
}

func (p *pairStrictParser) array() (interface{}, error) {
	if err := p.enter(); err != nil {
		return nil, err
	}
	defer func() { p.depth-- }()
	p.i++
	out := []interface{}{}
	p.ws()
	if p.i < len(p.b) && p.b[p.i] == ']' {
		p.i++
		return out, nil
	}
	for {
		p.ws()
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		out = append(out, v)
		p.ws()
		if p.i >= len(p.b) {
			return nil, p.fail("unterminated array")
		}
		if p.b[p.i] == ',' {
			p.i++
			continue
		}
		if p.b[p.i] == ']' {
			p.i++
			return out, nil
		}
		return nil, p.fail("expected ',' or ']' in array")
	}
}

func (p *pairStrictParser) hex4() (rune, error) {
	if p.i+4 > len(p.b) {
		return 0, p.fail("short unicode escape")
	}
	v, err := strconv.ParseUint(string(p.b[p.i:p.i+4]), 16, 32)
	if err != nil {
		return 0, p.fail("invalid unicode escape")
	}
	p.i += 4
	return rune(v), nil
}

func (p *pairStrictParser) str() (interface{}, error) {
	p.i++
	var sb strings.Builder
	for {
		if p.i >= len(p.b) {
			return nil, p.fail("unterminated string")
		}
		c := p.b[p.i]
		switch {
		case c == '"':
			p.i++
			return sb.String(), nil
		case c < 0x20:
			return nil, p.fail("raw control character in string")
		case c == '\\':
			p.i++
			if p.i >= len(p.b) {
				return nil, p.fail("unterminated escape")
			}
			e := p.b[p.i]
			p.i++
			switch e {
			case '"', '\\', '/':
				sb.WriteByte(e)
			case 'b':
				sb.WriteByte('\b')
			case 'f':
				sb.WriteByte('\f')
			case 'n':
				sb.WriteByte('\n')
			case 'r':
				sb.WriteByte('\r')
			case 't':
				sb.WriteByte('\t')
			case 'u':
				r, err := p.hex4()
				if err != nil {
					return nil, err
				}
				switch {
				case utf16.IsSurrogate(r) && r < 0xDC00:
					if p.i+2 > len(p.b) || p.b[p.i] != '\\' || p.b[p.i+1] != 'u' {
						return nil, p.fail("lone high surrogate escape")
					}
					p.i += 2
					lo, err := p.hex4()
					if err != nil {
						return nil, err
					}
					if lo < 0xDC00 || lo > 0xDFFF {
						return nil, p.fail("lone high surrogate escape")
					}
					sb.WriteRune(utf16.DecodeRune(r, lo))
				case utf16.IsSurrogate(r):
					return nil, p.fail("lone low surrogate escape")
				default:
					sb.WriteRune(r)
				}
			default:
				return nil, p.fail("invalid escape \\%c", e)
			}
		default:
			r, size := utf8.DecodeRune(p.b[p.i:])
			if r == utf8.RuneError && size <= 1 {
				return nil, p.fail("invalid UTF-8")
			}
			sb.WriteRune(r)
			p.i += size
		}
	}
}

func (p *pairStrictParser) number() (interface{}, error) {
	start := p.i
	if p.b[p.i] == '-' {
		p.i++
	}
	digits := func() int {
		n := 0
		for p.i < len(p.b) && p.b[p.i] >= '0' && p.b[p.i] <= '9' {
			p.i++
			n++
		}
		return n
	}
	if p.i < len(p.b) && p.b[p.i] == '0' {
		p.i++
	} else if digits() == 0 {
		return nil, p.fail("invalid number")
	}
	if p.i < len(p.b) && p.b[p.i] == '.' {
		p.i++
		if digits() == 0 {
			return nil, p.fail("invalid number fraction")
		}
	}
	if p.i < len(p.b) && (p.b[p.i] == 'e' || p.b[p.i] == 'E') {
		p.i++
		if p.i < len(p.b) && (p.b[p.i] == '+' || p.b[p.i] == '-') {
			p.i++
		}
		if digits() == 0 {
			return nil, p.fail("invalid number exponent")
		}
	}
	return pairJSONNumber(p.b[start:p.i]), nil
}

func pairCheckHashable(v reflect.Value, path string) error {
	if !v.IsValid() {
		return nil
	}
	switch v.Kind() {
	case reflect.String:
		if !utf8.ValidString(v.String()) {
			return fmt.Errorf("%s holds invalid UTF-8", path)
		}
	case reflect.Float32, reflect.Float64:
		return fmt.Errorf("%s is a binary float; schema decimals are canonical decimal strings", path)
	case reflect.Ptr, reflect.Interface:
		if !v.IsNil() {
			return pairCheckHashable(v.Elem(), path)
		}
	case reflect.Struct:
		t := v.Type()
		for i := 0; i < v.NumField(); i++ {
			if t.Field(i).PkgPath != "" && !t.Field(i).Anonymous {
				continue
			}
			if err := pairCheckHashable(v.Field(i), path+"."+t.Field(i).Name); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			if err := pairCheckHashable(v.Index(i), fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			if err := pairCheckHashable(k, path+" key"); err != nil {
				return err
			}
			if err := pairCheckHashable(v.MapIndex(k), path+"["+fmt.Sprint(k.Interface())+"]"); err != nil {
				return err
			}
		}
	}
	return nil
}

func pairCanonicalJSON(v interface{}) ([]byte, error) {
	if err := pairCheckHashable(reflect.ValueOf(v), "value"); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	tree, err := pairStrictParse(raw)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := pairWriteCanonical(&buf, tree); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func pairWriteCanonical(buf *bytes.Buffer, node interface{}) error {
	switch v := node.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if v {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case string:
		pairWriteString(buf, v)
	case pairJSONNumber:
		s := string(v)
		if !pairIntegerPattern.MatchString(s) {
			return fmt.Errorf("number %s is not an integer; schema decimals are canonical decimal strings", s)
		}
		if s == "-0" {
			s = "0"
		}
		buf.WriteString(s)
	case []interface{}:
		buf.WriteByte('[')
		for i, e := range v {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := pairWriteCanonical(buf, e); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]interface{}:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			pairWriteString(buf, k)
			buf.WriteByte(':')
			if err := pairWriteCanonical(buf, v[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		return fmt.Errorf("unsupported JSON node %T", node)
	}
	return nil
}

func pairWriteString(buf *bytes.Buffer, s string) {
	const hexDigits = "0123456789abcdef"
	buf.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			buf.WriteString(`\"`)
		case r == '\\':
			buf.WriteString(`\\`)
		case r == '\n':
			buf.WriteString(`\n`)
		case r == '\r':
			buf.WriteString(`\r`)
		case r == '\t':
			buf.WriteString(`\t`)
		case r < 0x20 || r == '<' || r == '>' || r == '&':
			buf.WriteString(`\u00`)
			buf.WriteByte(hexDigits[r>>4])
			buf.WriteByte(hexDigits[r&0xF])
		case r == 0x2028:
			buf.WriteString(`\u2028`)
		case r == 0x2029:
			buf.WriteString(`\u2029`)
		default:
			buf.WriteRune(r)
		}
	}
	buf.WriteByte('"')
}

func pairStructFields(t reflect.Type) []reflect.StructField {
	var out []reflect.StructField
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.Anonymous && f.Type.Kind() == reflect.Struct {
			out = append(out, pairStructFields(f.Type)...)
			continue
		}
		if f.PkgPath != "" {
			continue
		}
		name := strings.SplitN(f.Tag.Get("json"), ",", 2)[0]
		if name == "" || name == "-" {
			continue
		}
		f.Name = name
		out = append(out, f)
	}
	return out
}

func pairCheckShape(node interface{}, t reflect.Type, path string) error {
	switch t.Kind() {
	case reflect.Struct:
		obj, ok := node.(map[string]interface{})
		if !ok {
			return fmt.Errorf("%s must be a JSON object, got %s", path, pairNodeKind(node))
		}
		fields := pairStructFields(t)
		want := make(map[string]bool, len(fields))
		for _, f := range fields {
			want[f.Name] = true
			v, present := obj[f.Name]
			if !present {
				return fmt.Errorf("%s.%s is missing", path, f.Name)
			}
			if err := pairCheckShape(v, f.Type, path+"."+f.Name); err != nil {
				return err
			}
		}
		var extra []string
		for k := range obj {
			if !want[k] {
				extra = append(extra, k)
			}
		}
		sort.Strings(extra)
		if len(extra) > 0 {
			return fmt.Errorf("%s has unknown field %q", path, extra[0])
		}
	case reflect.String:
		if _, ok := node.(string); !ok {
			return fmt.Errorf("%s must be a JSON string, got %s", path, pairNodeKind(node))
		}
	case reflect.Bool:
		if _, ok := node.(bool); !ok {
			return fmt.Errorf("%s must be a JSON boolean, got %s", path, pairNodeKind(node))
		}
	case reflect.Int, reflect.Int64, reflect.Int32:
		n, ok := node.(pairJSONNumber)
		if !ok || !pairIntegerPattern.MatchString(string(n)) {
			return fmt.Errorf("%s must be a JSON integer, got %s", path, pairNodeKind(node))
		}
		if _, err := strconv.ParseInt(string(n), 10, t.Bits()); err != nil {
			return fmt.Errorf("%s integer %s is out of range", path, n)
		}
	case reflect.Uint64, reflect.Uint, reflect.Uint32:
		n, ok := node.(pairJSONNumber)
		if !ok || !pairIntegerPattern.MatchString(string(n)) || strings.HasPrefix(string(n), "-") {
			return fmt.Errorf("%s must be a JSON integer at least zero, got %s", path, pairNodeKind(node))
		}
		if _, err := strconv.ParseUint(string(n), 10, t.Bits()); err != nil {
			return fmt.Errorf("%s integer %s is out of range", path, n)
		}
	case reflect.Ptr:
		switch t.Elem().Kind() {
		case reflect.Int, reflect.Int64, reflect.Int32:
			if node == nil {
				return nil
			}
			return pairCheckShape(node, t.Elem(), path)
		}
		return fmt.Errorf("%s has unsupported nullable schema kind %s", path, t.Elem().Kind())
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 {
			return pairCheckBytesNode(node, path)
		}
		arr, ok := node.([]interface{})
		if !ok {
			return fmt.Errorf("%s must be a JSON array, got %s", path, pairNodeKind(node))
		}
		for i, e := range arr {
			if err := pairCheckShape(e, t.Elem(), fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("%s has unsupported schema kind %s", path, t.Kind())
	}
	return nil
}

func pairCheckBytesNode(node interface{}, path string) error {
	s, ok := node.(string)
	if !ok {
		return fmt.Errorf("%s must be a JSON string of standard padded base64, got %s", path, pairNodeKind(node))
	}
	if len(s) > base64.StdEncoding.EncodedLen(pairMaxEnvelopeBytes) {
		return fmt.Errorf("%s base64 text is %d bytes, above the %d byte bound", path, len(s), base64.StdEncoding.EncodedLen(pairMaxEnvelopeBytes))
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(s)
	if err != nil {
		return fmt.Errorf("%s is not standard padded base64: %v", path, err)
	}
	if base64.StdEncoding.EncodeToString(raw) != s {
		return fmt.Errorf("%s is not canonical standard padded base64", path)
	}
	return nil
}

func pairNodeKind(node interface{}) string {
	switch v := node.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case pairJSONNumber:
		if pairIntegerPattern.MatchString(string(v)) {
			return "integer " + string(v)
		}
		return "number " + string(v)
	case []interface{}:
		return "array"
	case map[string]interface{}:
		return "object"
	}
	return fmt.Sprintf("%T", node)
}

func pairStrictDecodeTree(tree interface{}, raw []byte, out interface{}, what string) error {
	if err := pairCheckShape(tree, reflect.TypeOf(out).Elem(), what); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return nil
}

func pairSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

type pairEvidenceKey struct {
	Partition  string `json:"partition"`
	Provider   string `json:"provider"`
	AccountKey string `json:"account_key"`
	Instrument string `json:"instrument"`
	MemberID   string `json:"member_id"`
}

func (k pairEvidenceKey) String() string {
	return strings.Join([]string{k.Partition, k.Provider, k.AccountKey, k.Instrument, k.MemberID}, "|")
}

func (k pairEvidenceKey) less(o pairEvidenceKey) int {
	for _, c := range [][2]string{{k.Partition, o.Partition}, {k.Provider, o.Provider}, {k.AccountKey, o.AccountKey}, {k.Instrument, o.Instrument}, {k.MemberID, o.MemberID}} {
		if c[0] != c[1] {
			if c[0] < c[1] {
				return -1
			}
			return 1
		}
	}
	return 0
}

func (k pairMemberKey) evidenceKey() pairEvidenceKey {
	return pairEvidenceKey{Partition: k.Partition.String(), Provider: k.Provider, AccountKey: k.AccountKey, Instrument: k.Instrument, MemberID: k.MemberID}
}

func (k pairEvidenceKey) memberKey() (pairMemberKey, error) {
	part, err := parseRiskPartition(k.Partition)
	if err != nil {
		return pairMemberKey{}, err
	}
	if part == unassignedPartition || part.String() != k.Partition {
		return pairMemberKey{}, fmt.Errorf("evidence key partition %q is not a canonical risk partition", k.Partition)
	}
	if pairDerivationIDs[k.Provider] == "" {
		return pairMemberKey{}, fmt.Errorf("evidence key provider %q has no pair venue", k.Provider)
	}
	for name, v := range map[string]string{"account_key": k.AccountKey, "instrument": k.Instrument, "member_id": k.MemberID} {
		if strings.TrimSpace(v) == "" || strings.TrimSpace(v) != v {
			return pairMemberKey{}, fmt.Errorf("evidence key %s %q must be a nonempty trimmed string", name, v)
		}
	}
	return pairMemberKey{Partition: part, Provider: k.Provider, AccountKey: k.AccountKey, Instrument: k.Instrument, MemberID: k.MemberID}, nil
}

type pairWireMeta struct {
	NativeUnit         string      `json:"native_unit"`
	BaseAsset          string      `json:"base_asset"`
	ContractSize       pairDecimal `json:"contract_size"`
	QuoteCurrency      string      `json:"quote_currency"`
	SettlementCurrency string      `json:"settlement_currency"`
	PriceCurrency      string      `json:"price_currency"`
	TickStep           pairDecimal `json:"tick_step"`
	LotStep            pairDecimal `json:"lot_step"`
	MinQty             pairDecimal `json:"min_qty"`
	MinNotional        pairDecimal `json:"min_notional"`
	TradingStatus      string      `json:"trading_status"`
	Linear             bool        `json:"linear"`
	MaxLeverage        pairDecimal `json:"max_leverage"`
	PriceRule          string      `json:"price_rule"`
	SzDecimals         int         `json:"sz_decimals"`
	TickStepText       string      `json:"tick_step_text"`
	LotStepText        string      `json:"lot_step_text"`
	FeeCurrencies      []string    `json:"fee_currencies"`
}

func (m pairWireMeta) clone() pairWireMeta {
	m.FeeCurrencies = append([]string{}, m.FeeCurrencies...)
	return m
}

type pairEvidenceStamp struct {
	V              int             `json:"v"`
	Key            pairEvidenceKey `json:"key"`
	EvidenceID     string          `json:"evidence_id"`
	EvidenceHash   string          `json:"evidence_hash"`
	ObservedMs     int64           `json:"observed_ms"`
	FirstReceiptMs int64           `json:"first_receipt_ms"`
	ValidFromMs    int64           `json:"valid_from_ms"`
	ValidThroughMs int64           `json:"valid_through_ms"`
	Provenance     string          `json:"provenance"`
}

type pairCapabilityEvidence struct {
	pairEvidenceStamp
	Meta               pairWireMeta `json:"meta"`
	MetadataGeneration uint64       `json:"metadata_generation"`
}

type pairConversionEvidence struct {
	pairEvidenceStamp
	Currency string      `json:"currency"`
	RateUSD  pairDecimal `json:"rate_usd"`
}

type pairAccountEvidence struct {
	pairEvidenceStamp
	PositionMode       string      `json:"position_mode"`
	MarginMode         string      `json:"margin_mode"`
	Leverage           pairDecimal `json:"leverage"`
	CollateralCurrency string      `json:"collateral_currency"`
	Equity             pairDecimal `json:"equity"`
	FreeCollateral     pairDecimal `json:"free_collateral"`
	MarginUsed         pairDecimal `json:"margin_used"`
}

type pairEvidenceDocument struct {
	V            int                      `json:"v"`
	Capabilities []pairCapabilityEvidence `json:"capabilities"`
	Accounts     []pairAccountEvidence    `json:"accounts"`
	Conversions  []pairConversionEvidence `json:"conversions"`
}

func pairRecordHashOf(v interface{}) (string, error) {
	b, err := pairCanonicalJSON(v)
	if err != nil {
		return "", err
	}
	return pairSHA256(b), nil
}

func (c pairCapabilityEvidence) computeHash() (string, error) {
	c.EvidenceHash = ""
	if c.Meta.FeeCurrencies == nil {
		c.Meta.FeeCurrencies = []string{}
	}
	return pairRecordHashOf(c)
}

func (c pairConversionEvidence) computeHash() (string, error) {
	c.EvidenceHash = ""
	return pairRecordHashOf(c)
}

func (a pairAccountEvidence) computeHash() (string, error) {
	a.EvidenceHash = ""
	return pairRecordHashOf(a)
}

func (c pairCapabilityEvidence) sealed() (pairCapabilityEvidence, error) {
	c.Meta = c.Meta.clone()
	sort.Strings(c.Meta.FeeCurrencies)
	h, err := c.computeHash()
	c.EvidenceHash = h
	return c, err
}

func (c pairConversionEvidence) sealed() (pairConversionEvidence, error) {
	h, err := c.computeHash()
	c.EvidenceHash = h
	return c, err
}

func (a pairAccountEvidence) sealed() (pairAccountEvidence, error) {
	h, err := a.computeHash()
	a.EvidenceHash = h
	return a, err
}

func pairPrintableASCII(s string, max int) bool {
	if s == "" || len(s) > max {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7E {
			return false
		}
	}
	return true
}

func pairValidateStamp(s pairEvidenceStamp, hash string, what string) error {
	if s.V != pairEvidenceVersion {
		return fmt.Errorf("%s version %d is not supported (want %d)", what, s.V, pairEvidenceVersion)
	}
	if _, err := s.Key.memberKey(); err != nil {
		return fmt.Errorf("%s %s: %w", what, s.EvidenceID, err)
	}
	if !pairPrintableASCII(s.EvidenceID, pairEvidenceMaxIDLen) {
		return fmt.Errorf("%s evidence_id %q must be 1..%d printable ASCII characters", what, s.EvidenceID, pairEvidenceMaxIDLen)
	}
	if !pairPrintableASCII(s.Provenance, pairEvidenceMaxProvenanceLen) {
		return fmt.Errorf("%s %s provenance must be 1..%d printable ASCII characters", what, s.EvidenceID, pairEvidenceMaxProvenanceLen)
	}
	if s.ObservedMs < 0 || s.FirstReceiptMs < 0 || s.ValidFromMs < 0 || s.ValidThroughMs < 0 {
		return fmt.Errorf("%s %s has a negative time", what, s.EvidenceID)
	}
	if s.ObservedMs > s.FirstReceiptMs {
		return fmt.Errorf("%s %s was observed at %d after its first receipt at %d", what, s.EvidenceID, s.ObservedMs, s.FirstReceiptMs)
	}
	if s.ValidFromMs > s.ValidThroughMs {
		return fmt.Errorf("%s %s has an invalid validity interval [%d, %d]", what, s.EvidenceID, s.ValidFromMs, s.ValidThroughMs)
	}
	if !pairHexDigestPattern.MatchString(s.EvidenceHash) || s.EvidenceHash != hash {
		return fmt.Errorf("%s %s evidence_hash does not match its canonical bytes", what, s.EvidenceID)
	}
	return nil
}

func pairEligibleAt(s pairEvidenceStamp, refMs int64) bool {
	return s.ObservedMs <= refMs && s.FirstReceiptMs <= refMs && s.ValidFromMs <= refMs && refMs <= s.ValidThroughMs
}

func pairDecimalRat(name string, d pairDecimal) (*big.Rat, error) {
	r, err := d.rat()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return r, nil
}

func pairDecimalPositive(name string, d pairDecimal) (*big.Rat, error) {
	r, err := pairDecimalRat(name, d)
	if err != nil {
		return nil, err
	}
	if r.Sign() <= 0 {
		return nil, fmt.Errorf("%s %s must be above zero", name, d)
	}
	return r, nil
}

func pairPow10Decimal(n int) pairDecimal {
	s, _ := pairRatDecimal(pow10Rat(n))
	return pairDecimal(s)
}

func pairHyperliquidTickStep(szDecimals int) pairDecimal {
	d := 6 - szDecimals
	if d < 0 {
		d = 0
	}
	return pairPow10Decimal(-d)
}

func pairValidateWireMeta(provider string, m pairWireMeta) error {
	bad := func(format string, a ...interface{}) error {
		return fmt.Errorf("%s capability meta: %s", provider, fmt.Sprintf(format, a...))
	}
	for name, d := range map[string]pairDecimal{"contract_size": m.ContractSize, "tick_step": m.TickStep, "lot_step": m.LotStep, "max_leverage": m.MaxLeverage} {
		if _, err := pairDecimalPositive(name, d); err != nil {
			return bad("%v", err)
		}
	}
	for name, d := range map[string]pairDecimal{"min_qty": m.MinQty, "min_notional": m.MinNotional} {
		r, err := pairDecimalRat(name, d)
		if err != nil {
			return bad("%v", err)
		}
		if r.Sign() < 0 {
			return bad("%s %s must not be negative", name, d)
		}
	}
	if strings.TrimSpace(m.BaseAsset) == "" || strings.TrimSpace(m.TradingStatus) == "" {
		return bad("base_asset and trading_status must be set")
	}
	if !m.Linear {
		return bad("linear must be true")
	}
	if m.FeeCurrencies == nil || !sort.StringsAreSorted(m.FeeCurrencies) {
		return bad("fee_currencies must be a sorted list")
	}
	switch provider {
	case entryProviderHyperliquid:
		if m.SzDecimals < 0 || m.SzDecimals > hlLotMetadataMaxSzDecimals {
			return bad("sz_decimals %d is outside [0, %d]", m.SzDecimals, hlLotMetadataMaxSzDecimals)
		}
		lot := pairPow10Decimal(-m.SzDecimals)
		want := map[string][2]string{
			"native_unit":         {m.NativeUnit, "base"},
			"contract_size":       {string(m.ContractSize), "1"},
			"quote_currency":      {m.QuoteCurrency, pairHyperliquidCurrency},
			"settlement_currency": {m.SettlementCurrency, pairHyperliquidCurrency},
			"price_currency":      {m.PriceCurrency, pairHyperliquidCurrency},
			"price_rule":          {m.PriceRule, pairPriceRuleHyperliquid},
			"lot_step":            {string(m.LotStep), string(lot)},
			"min_qty":             {string(m.MinQty), string(lot)},
			"tick_step":           {string(m.TickStep), string(pairHyperliquidTickStep(m.SzDecimals))},
			"min_notional":        {string(m.MinNotional), pairHyperliquidMinNotional},
			"tick_step_text":      {m.TickStepText, ""},
			"lot_step_text":       {m.LotStepText, ""},
		}
		if err := pairWantFields(want); err != nil {
			return bad("%v", err)
		}
		if len(m.FeeCurrencies) != 1 || m.FeeCurrencies[0] != pairHyperliquidCurrency {
			return bad("fee_currencies %v must be [%s]", m.FeeCurrencies, pairHyperliquidCurrency)
		}
		r, _ := m.MaxLeverage.rat()
		if r == nil || !r.IsInt() {
			return bad("max_leverage %s must be a whole number", m.MaxLeverage)
		}
	case entryProviderOKX:
		want := map[string][2]string{
			"native_unit":         {m.NativeUnit, "contract"},
			"quote_currency":      {m.QuoteCurrency, pairOKXCurrency},
			"settlement_currency": {m.SettlementCurrency, pairOKXCurrency},
			"price_currency":      {m.PriceCurrency, pairOKXCurrency},
			"price_rule":          {m.PriceRule, pairPriceRuleFixedTick},
			"tick_step_text":      {m.TickStepText, string(m.TickStep)},
			"lot_step_text":       {m.LotStepText, string(m.LotStep)},
			"min_notional":        {string(m.MinNotional), pairOKXMinNotional},
		}
		if err := pairWantFields(want); err != nil {
			return bad("%v", err)
		}
		if m.SzDecimals != 0 {
			return bad("sz_decimals %d must be 0 (the fixed tick rule uses tick_step_text)", m.SzDecimals)
		}
		if len(m.FeeCurrencies) != 1 || m.FeeCurrencies[0] != m.SettlementCurrency {
			return bad("fee_currencies %v must be [settlement_currency %s]", m.FeeCurrencies, m.SettlementCurrency)
		}
	default:
		return fmt.Errorf("capability meta provider %q has no pair venue", provider)
	}
	return nil
}

func pairWantFields(want map[string][2]string) error {
	names := make([]string, 0, len(want))
	for k := range want {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, n := range names {
		if want[n][0] != want[n][1] {
			return fmt.Errorf("%s is %q, the derivation requires %q", n, want[n][0], want[n][1])
		}
	}
	return nil
}

func pairExpectedWireMeta(m pairContractMember) (pairWireMeta, error) {
	out := pairWireMeta{
		NativeUnit: m.Meta.NativeUnit, BaseAsset: m.Meta.BaseAsset,
		QuoteCurrency: m.Meta.QuoteCurrency, SettlementCurrency: m.Meta.SettlementCurrency, PriceCurrency: m.Meta.PriceCurrency,
		TradingStatus: m.Meta.TradingStatus, Linear: m.Meta.Linear, PriceRule: m.Meta.PriceRule, SzDecimals: m.Meta.SzDecimals,
		FeeCurrencies: append([]string{}, m.Meta.FeeCurrencies...),
	}
	sort.Strings(out.FeeCurrencies)
	for _, f := range []struct {
		dst *pairDecimal
		v   float64
	}{{&out.ContractSize, m.Meta.ContractSize}, {&out.TickStep, m.Meta.TickStep}, {&out.LotStep, m.Meta.LotStep}, {&out.MinQty, m.Meta.MinQty}, {&out.MinNotional, m.Meta.MinNotional}, {&out.MaxLeverage, m.Meta.MaxLeverage}} {
		d, err := pairDecimalFromFloat(f.v)
		if err != nil {
			return pairWireMeta{}, err
		}
		*f.dst = d
	}
	for _, f := range []struct {
		dst *string
		v   string
	}{{&out.TickStepText, m.Meta.TickStepText}, {&out.LotStepText, m.Meta.LotStepText}} {
		if f.v == "" {
			continue
		}
		s, err := pairNormalizeDecimal(f.v)
		if err != nil {
			return pairWireMeta{}, err
		}
		*f.dst = s
	}
	return out, nil
}

func pairCheckKeyForMember(k pairEvidenceKey, m pairContractMember) error {
	if k.Provider != m.Provider || k.AccountKey != m.AccountKey || k.Instrument != m.Instrument || k.MemberID != m.MemberID {
		return fmt.Errorf("evidence key %s does not name member %s (%s|%s|%s)", k, m.MemberID, m.Provider, m.AccountKey, m.Instrument)
	}
	return nil
}

func pairCapabilityMatches(m pairContractMember, c pairCapabilityEvidence) error {
	h, err := c.computeHash()
	if err != nil {
		return err
	}
	if err := pairValidateStamp(c.pairEvidenceStamp, h, "capability evidence"); err != nil {
		return err
	}
	if err := pairCheckKeyForMember(c.Key, m); err != nil {
		return err
	}
	if want := pairDerivationIDs[m.Provider]; c.Provenance != want {
		return fmt.Errorf("capability evidence %s provenance %q is not the %s derivation %q", c.EvidenceID, c.Provenance, m.Provider, want)
	}
	if c.MetadataGeneration < 1 {
		return fmt.Errorf("capability evidence %s metadata_generation must be at least 1", c.EvidenceID)
	}
	if err := pairValidateWireMeta(m.Provider, c.Meta); err != nil {
		return fmt.Errorf("capability evidence %s: %w", c.EvidenceID, err)
	}
	want, err := pairExpectedWireMeta(m)
	if err != nil {
		return fmt.Errorf("member %s contract meta: %w", m.MemberID, err)
	}
	if field, got, exp, same := pairWireMetaDiff(c.Meta, want); !same {
		return fmt.Errorf("capability evidence %s meta.%s %s differs from the frozen contract %s", c.EvidenceID, field, got, exp)
	}
	return nil
}

func pairWireMetaDiff(a, b pairWireMeta) (string, string, string, bool) {
	va, vb := reflect.ValueOf(a), reflect.ValueOf(b)
	t := va.Type()
	for i := 0; i < t.NumField(); i++ {
		x, _ := json.Marshal(va.Field(i).Interface())
		y, _ := json.Marshal(vb.Field(i).Interface())
		if !bytes.Equal(x, y) {
			return strings.SplitN(t.Field(i).Tag.Get("json"), ",", 2)[0], string(x), string(y), false
		}
	}
	return "", "", "", true
}

func pairAccountMatches(m pairContractMember, a pairAccountEvidence) error {
	h, err := a.computeHash()
	if err != nil {
		return err
	}
	if err := pairValidateStamp(a.pairEvidenceStamp, h, "account evidence"); err != nil {
		return err
	}
	if err := pairCheckKeyForMember(a.Key, m); err != nil {
		return err
	}
	if a.PositionMode != m.Meta.PositionMode {
		return fmt.Errorf("account evidence %s position_mode %q differs from the required %q", a.EvidenceID, a.PositionMode, m.Meta.PositionMode)
	}
	if a.MarginMode != m.Meta.MarginMode {
		return fmt.Errorf("account evidence %s margin_mode %q differs from the required %q", a.EvidenceID, a.MarginMode, m.Meta.MarginMode)
	}
	lev, err := pairDecimalPositive("leverage", a.Leverage)
	if err != nil {
		return fmt.Errorf("account evidence %s: %w", a.EvidenceID, err)
	}
	want, err := quoteRatFromFloatDecimal(m.Leverage)
	if err != nil || lev.Cmp(want) != 0 {
		return fmt.Errorf("account evidence %s leverage %s differs from the frozen selected leverage %v", a.EvidenceID, a.Leverage, m.Leverage)
	}
	if a.CollateralCurrency != m.Meta.SettlementCurrency {
		return fmt.Errorf("account evidence %s collateral_currency %q differs from the settlement currency %q", a.EvidenceID, a.CollateralCurrency, m.Meta.SettlementCurrency)
	}
	if _, err := pairDecimalRat("equity", a.Equity); err != nil {
		return fmt.Errorf("account evidence %s: %w", a.EvidenceID, err)
	}
	for name, d := range map[string]pairDecimal{"free_collateral": a.FreeCollateral, "margin_used": a.MarginUsed} {
		r, err := pairDecimalRat(name, d)
		if err != nil {
			return fmt.Errorf("account evidence %s: %w", a.EvidenceID, err)
		}
		if r.Sign() < 0 {
			return fmt.Errorf("account evidence %s %s %s must not be negative", a.EvidenceID, name, d)
		}
	}
	return nil
}

func pairRequiredConversionCurrencies(m pairContractMember) []string {
	set := map[string]bool{m.Meta.SettlementCurrency: true, m.Meta.PriceCurrency: true}
	for _, c := range m.Meta.FeeCurrencies {
		set[c] = true
	}
	delete(set, pairUSD)
	out := make([]string, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

func pairConversionMatches(m pairContractMember, c pairConversionEvidence) error {
	h, err := c.computeHash()
	if err != nil {
		return err
	}
	if err := pairValidateStamp(c.pairEvidenceStamp, h, "conversion evidence"); err != nil {
		return err
	}
	if err := pairCheckKeyForMember(c.Key, m); err != nil {
		return err
	}
	if !pairCurrencyPattern.MatchString(c.Currency) || c.Currency == pairUSD {
		return fmt.Errorf("conversion evidence %s currency %q must be an upper-case currency other than USD (USD is the unit and needs no record)", c.EvidenceID, c.Currency)
	}
	required := false
	for _, want := range pairRequiredConversionCurrencies(m) {
		if want == c.Currency {
			required = true
		}
	}
	if !required {
		return fmt.Errorf("conversion evidence %s currency %s is not a settlement, price or fee currency of member %s", c.EvidenceID, c.Currency, m.MemberID)
	}
	if _, err := pairDecimalPositive("rate_usd", c.RateUSD); err != nil {
		return fmt.Errorf("conversion evidence %s: %w", c.EvidenceID, err)
	}
	return nil
}

func pairSortCapabilities(rs []pairCapabilityEvidence) {
	sort.SliceStable(rs, func(i, j int) bool {
		a, b := rs[i], rs[j]
		if c := a.Key.less(b.Key); c != 0 {
			return c < 0
		}
		return pairStampLess(a.pairEvidenceStamp, b.pairEvidenceStamp)
	})
}

func pairSortAccounts(rs []pairAccountEvidence) {
	sort.SliceStable(rs, func(i, j int) bool {
		a, b := rs[i], rs[j]
		if c := a.Key.less(b.Key); c != 0 {
			return c < 0
		}
		return pairStampLess(a.pairEvidenceStamp, b.pairEvidenceStamp)
	})
}

func pairSortConversions(rs []pairConversionEvidence) {
	sort.SliceStable(rs, func(i, j int) bool {
		a, b := rs[i], rs[j]
		if c := a.Key.less(b.Key); c != 0 {
			return c < 0
		}
		if a.Currency != b.Currency {
			return a.Currency < b.Currency
		}
		return pairStampLess(a.pairEvidenceStamp, b.pairEvidenceStamp)
	})
}

func pairStampLess(a, b pairEvidenceStamp) bool {
	if a.FirstReceiptMs != b.FirstReceiptMs {
		return a.FirstReceiptMs < b.FirstReceiptMs
	}
	if a.ObservedMs != b.ObservedMs {
		return a.ObservedMs < b.ObservedMs
	}
	return a.EvidenceID < b.EvidenceID
}

func pairEvidenceDocumentBytes(d pairEvidenceDocument) ([]byte, error) {
	out := pairEvidenceDocument{V: d.V,
		Capabilities: append([]pairCapabilityEvidence{}, d.Capabilities...),
		Accounts:     append([]pairAccountEvidence{}, d.Accounts...),
		Conversions:  append([]pairConversionEvidence{}, d.Conversions...)}
	for i := range out.Capabilities {
		out.Capabilities[i].Meta = out.Capabilities[i].Meta.clone()
		sort.Strings(out.Capabilities[i].Meta.FeeCurrencies)
	}
	pairSortCapabilities(out.Capabilities)
	pairSortAccounts(out.Accounts)
	pairSortConversions(out.Conversions)
	return pairCanonicalJSON(out)
}

func pairDecodeEvidenceDocument(b []byte) (pairEvidenceDocument, error) {
	var d pairEvidenceDocument
	if len(b) > pairEvidenceMaxDocumentBytes {
		return d, fmt.Errorf("evidence document is %d bytes, above the %d byte limit", len(b), pairEvidenceMaxDocumentBytes)
	}
	tree, err := pairStrictParse(b)
	if err != nil {
		return d, fmt.Errorf("evidence document: %w", err)
	}
	if obj, ok := tree.(map[string]interface{}); ok {
		if v, ok := obj["v"].(pairJSONNumber); ok && string(v) != strconv.Itoa(pairEvidenceVersion) {
			return d, fmt.Errorf("evidence document version %s is not supported (want %d)", v, pairEvidenceVersion)
		}
	}
	if err := pairStrictDecodeTree(tree, b, &d, "evidence document"); err != nil {
		return d, err
	}
	if d.V != pairEvidenceVersion {
		return d, fmt.Errorf("evidence document version %d is not supported (want %d)", d.V, pairEvidenceVersion)
	}
	canon, err := pairEvidenceDocumentBytes(d)
	if err != nil {
		return d, fmt.Errorf("evidence document: %w", err)
	}
	if !bytes.Equal(canon, b) {
		return d, fmt.Errorf("evidence document bytes are not canonical (compact sorted keys, canonical decimals, schedules sorted by key, receipt, observation and evidence id)")
	}
	seen := map[string]bool{}
	once := func(kind string, s pairEvidenceStamp) error {
		id := kind + "|" + s.Key.String() + "|" + s.EvidenceID
		if seen[id] {
			return fmt.Errorf("%s evidence %s appears twice for %s", kind, s.EvidenceID, s.Key)
		}
		seen[id] = true
		return nil
	}
	for _, c := range d.Capabilities {
		h, err := c.computeHash()
		if err != nil {
			return d, err
		}
		if err := pairValidateStamp(c.pairEvidenceStamp, h, "capability evidence"); err != nil {
			return d, err
		}
		if err := once("capability", c.pairEvidenceStamp); err != nil {
			return d, err
		}
	}
	for _, a := range d.Accounts {
		h, err := a.computeHash()
		if err != nil {
			return d, err
		}
		if err := pairValidateStamp(a.pairEvidenceStamp, h, "account evidence"); err != nil {
			return d, err
		}
		if err := once("account", a.pairEvidenceStamp); err != nil {
			return d, err
		}
	}
	for _, c := range d.Conversions {
		h, err := c.computeHash()
		if err != nil {
			return d, err
		}
		if err := pairValidateStamp(c.pairEvidenceStamp, h, "conversion evidence"); err != nil {
			return d, err
		}
		if err := once("conversion", c.pairEvidenceStamp); err != nil {
			return d, err
		}
	}
	gen := map[string]pairCapabilityEvidence{}
	for _, c := range d.Capabilities {
		k := c.Key.String()
		prev, ok := gen[k]
		if ok {
			_, _, _, same := pairWireMetaDiff(prev.Meta, c.Meta)
			switch {
			case c.MetadataGeneration < prev.MetadataGeneration:
				return d, fmt.Errorf("capability evidence %s metadata_generation %d is below the earlier %d for %s", c.EvidenceID, c.MetadataGeneration, prev.MetadataGeneration, k)
			case c.MetadataGeneration == prev.MetadataGeneration && !same:
				return d, fmt.Errorf("capability evidence %s changes meta without a new metadata_generation for %s", c.EvidenceID, k)
			}
		}
		gen[k] = c
	}
	return d, nil
}

type pairEvidenceSource interface {
	Capability(ctx context.Context, m pairContractMember, refMs int64) (pairCapabilityEvidence, error)
	Conversions(ctx context.Context, m pairContractMember, refMs int64) ([]pairConversionEvidence, error)
	Account(ctx context.Context, m pairContractMember, refMs int64) (pairAccountEvidence, error)
}

type pairFrozenEvidenceSource struct {
	digest    string
	doc       pairEvidenceDocument
	bound     bool
	boundFP   string
	partition string
	members   map[string]pairContractMember
}

func newPairFrozenEvidenceSource(doc []byte, wantDigest string) (*pairFrozenEvidenceSource, error) {
	if !pairHexDigestPattern.MatchString(wantDigest) {
		return nil, fmt.Errorf("pair evidence needs an expected lowercase SHA-256 document digest, got %q", wantDigest)
	}
	if got := pairSHA256(doc); got != wantDigest {
		return nil, fmt.Errorf("pair evidence document digest %s is not the expected %s", shortFingerprint(got), shortFingerprint(wantDigest))
	}
	d, err := pairDecodeEvidenceDocument(doc)
	if err != nil {
		return nil, err
	}
	return &pairFrozenEvidenceSource{digest: wantDigest, doc: d}, nil
}

func (s *pairFrozenEvidenceSource) Digest() string { return s.digest }

func (s *pairFrozenEvidenceSource) bind(c basketContract) error {
	if s.bound {
		if s.boundFP == c.Fingerprint() {
			return nil
		}
		return fmt.Errorf("pair evidence source is already bound to contract %s", shortFingerprint(s.boundFP))
	}
	if !c.isPair() {
		return fmt.Errorf("pair evidence binds only a pair contract")
	}
	members := map[string]pairContractMember{}
	for _, m := range c.Pair.Members {
		members[m.MemberID] = m
	}
	member := func(k pairEvidenceKey, kind, id string) (pairContractMember, error) {
		if k.Partition != c.Partition {
			return pairContractMember{}, fmt.Errorf("%s evidence %s key %s is outside the pair partition %s", kind, id, k, c.Partition)
		}
		m, ok := members[k.MemberID]
		if !ok {
			return pairContractMember{}, fmt.Errorf("%s evidence %s key %s names no pair member", kind, id, k)
		}
		return m, nil
	}
	caps, accts := map[string]int{}, map[string]int{}
	convs := map[string]map[string]int{}
	for _, r := range s.doc.Capabilities {
		m, err := member(r.Key, "capability", r.EvidenceID)
		if err != nil {
			return err
		}
		if err := pairCapabilityMatches(m, r); err != nil {
			return err
		}
		caps[m.MemberID]++
	}
	for _, r := range s.doc.Accounts {
		m, err := member(r.Key, "account", r.EvidenceID)
		if err != nil {
			return err
		}
		if err := pairAccountMatches(m, r); err != nil {
			return err
		}
		accts[m.MemberID]++
	}
	for _, r := range s.doc.Conversions {
		m, err := member(r.Key, "conversion", r.EvidenceID)
		if err != nil {
			return err
		}
		if err := pairConversionMatches(m, r); err != nil {
			return err
		}
		if convs[m.MemberID] == nil {
			convs[m.MemberID] = map[string]int{}
		}
		convs[m.MemberID][r.Currency]++
	}
	ids := make([]string, 0, len(members))
	for id := range members {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if caps[id] == 0 {
			return fmt.Errorf("pair evidence has no capability schedule for member %s", id)
		}
		if accts[id] == 0 {
			return fmt.Errorf("pair evidence has no account schedule for member %s", id)
		}
		for _, cur := range pairRequiredConversionCurrencies(members[id]) {
			if convs[id][cur] == 0 {
				return fmt.Errorf("pair evidence has no %s-to-USD conversion schedule for member %s", cur, id)
			}
		}
	}
	s.bound, s.boundFP, s.partition, s.members = true, c.Fingerprint(), c.Partition, members
	return nil
}

func (s *pairFrozenEvidenceSource) key(ctx context.Context, m pairContractMember) (pairEvidenceKey, error) {
	if err := ctx.Err(); err != nil {
		return pairEvidenceKey{}, err
	}
	if s == nil || !s.bound {
		return pairEvidenceKey{}, fmt.Errorf("pair evidence source is not bound to a contract")
	}
	b, ok := s.members[m.MemberID]
	if !ok || b.Provider != m.Provider || b.AccountKey != m.AccountKey || b.Instrument != m.Instrument {
		return pairEvidenceKey{}, fmt.Errorf("pair evidence has no member %s|%s|%s|%s in partition %s", m.Provider, m.AccountKey, m.Instrument, m.MemberID, s.partition)
	}
	return pairEvidenceKey{Partition: s.partition, Provider: m.Provider, AccountKey: m.AccountKey, Instrument: m.Instrument, MemberID: m.MemberID}, nil
}

func pairNewer(a, b pairEvidenceStamp) int {
	switch {
	case a.FirstReceiptMs != b.FirstReceiptMs:
		if a.FirstReceiptMs > b.FirstReceiptMs {
			return 1
		}
		return -1
	case a.ObservedMs != b.ObservedMs:
		if a.ObservedMs > b.ObservedMs {
			return 1
		}
		return -1
	}
	return 0
}

func (s *pairFrozenEvidenceSource) Capability(ctx context.Context, m pairContractMember, refMs int64) (pairCapabilityEvidence, error) {
	k, err := s.key(ctx, m)
	if err != nil {
		return pairCapabilityEvidence{}, err
	}
	var best *pairCapabilityEvidence
	for i := range s.doc.Capabilities {
		r := &s.doc.Capabilities[i]
		if r.Key != k || !pairEligibleAt(r.pairEvidenceStamp, refMs) {
			continue
		}
		if best == nil {
			best = r
			continue
		}
		switch pairNewer(r.pairEvidenceStamp, best.pairEvidenceStamp) {
		case 1:
			best = r
		case 0:
			_, _, _, same := pairWireMetaDiff(r.Meta, best.Meta)
			if !same || r.MetadataGeneration != best.MetadataGeneration {
				return pairCapabilityEvidence{}, fmt.Errorf("capability evidence %s and %s for %s tie at receipt %d with different content", best.EvidenceID, r.EvidenceID, k, r.FirstReceiptMs)
			}
		}
	}
	if best == nil {
		return pairCapabilityEvidence{}, fmt.Errorf("no capability evidence for %s is observed, received and valid at %d", k, refMs)
	}
	out := *best
	out.Meta = best.Meta.clone()
	return out, nil
}

func (s *pairFrozenEvidenceSource) Account(ctx context.Context, m pairContractMember, refMs int64) (pairAccountEvidence, error) {
	k, err := s.key(ctx, m)
	if err != nil {
		return pairAccountEvidence{}, err
	}
	var best *pairAccountEvidence
	for i := range s.doc.Accounts {
		r := &s.doc.Accounts[i]
		if r.Key != k || !pairEligibleAt(r.pairEvidenceStamp, refMs) {
			continue
		}
		if best == nil {
			best = r
			continue
		}
		switch pairNewer(r.pairEvidenceStamp, best.pairEvidenceStamp) {
		case 1:
			best = r
		case 0:
			x, y := *r, *best
			x.pairEvidenceStamp, y.pairEvidenceStamp = pairEvidenceStamp{}, pairEvidenceStamp{}
			if x != y {
				return pairAccountEvidence{}, fmt.Errorf("account evidence %s and %s for %s tie at receipt %d with different content", best.EvidenceID, r.EvidenceID, k, r.FirstReceiptMs)
			}
		}
	}
	if best == nil {
		return pairAccountEvidence{}, fmt.Errorf("no account evidence for %s is observed, received and valid at %d", k, refMs)
	}
	return *best, nil
}

func (s *pairFrozenEvidenceSource) Conversions(ctx context.Context, m pairContractMember, refMs int64) ([]pairConversionEvidence, error) {
	k, err := s.key(ctx, m)
	if err != nil {
		return nil, err
	}
	var out []pairConversionEvidence
	for _, cur := range pairRequiredConversionCurrencies(s.members[m.MemberID]) {
		var best *pairConversionEvidence
		for i := range s.doc.Conversions {
			r := &s.doc.Conversions[i]
			if r.Key != k || r.Currency != cur || !pairEligibleAt(r.pairEvidenceStamp, refMs) {
				continue
			}
			if best == nil {
				best = r
				continue
			}
			switch pairNewer(r.pairEvidenceStamp, best.pairEvidenceStamp) {
			case 1:
				best = r
			case 0:
				if r.RateUSD != best.RateUSD {
					return nil, fmt.Errorf("conversion evidence %s and %s for %s %s tie at receipt %d with different rates", best.EvidenceID, r.EvidenceID, k, cur, r.FirstReceiptMs)
				}
			}
		}
		if best == nil {
			return nil, fmt.Errorf("no %s-to-USD conversion evidence for %s is observed, received and valid at %d", cur, k, refMs)
		}
		out = append(out, *best)
	}
	return out, nil
}

func pairRoundBound(m pairContractMember, capEv pairCapabilityEvidence, side string, bound float64) (float64, error) {
	if err := pairCapabilityMatches(m, capEv); err != nil {
		return 0, err
	}
	if side != "buy" && side != "sell" {
		return 0, fmt.Errorf("bound side %q is neither buy nor sell", side)
	}
	if math.IsNaN(bound) || math.IsInf(bound, 0) || bound <= 0 {
		return 0, fmt.Errorf("bound %v must be finite and above zero", bound)
	}
	x, err := quoteRatFromFloatDecimal(bound)
	if err != nil {
		return 0, err
	}
	round := func(px *big.Rat) (*big.Rat, error) {
		switch capEv.Meta.PriceRule {
		case pairPriceRuleHyperliquid:
			rule := newQuotePriceRule(hlLotLookup{Known: true, SzDecimals: capEv.Meta.SzDecimals, Coin: m.Instrument})
			return rule.roundQuotePx(side, px)
		case pairPriceRuleFixedTick:
			tick, err := pairParseDecimalInput(capEv.Meta.TickStepText)
			if err != nil || tick.Sign() <= 0 {
				return nil, fmt.Errorf("tick_step_text %q is not a positive decimal", capEv.Meta.TickStepText)
			}
			q := new(big.Rat).Quo(px, tick)
			n := new(big.Int).Quo(q.Num(), q.Denom())
			if side == "sell" && new(big.Rat).SetInt(n).Cmp(q) != 0 {
				n.Add(n, big.NewInt(1))
			}
			return new(big.Rat).Mul(new(big.Rat).SetInt(n), tick), nil
		}
		return nil, fmt.Errorf("price rule %q is not supported", capEv.Meta.PriceRule)
	}
	r, err := round(x)
	if err != nil {
		return 0, err
	}
	if r.Sign() <= 0 {
		return 0, fmt.Errorf("bound %v rounds to zero", bound)
	}
	f := quoteRatFloat(r)
	if math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 {
		return 0, fmt.Errorf("rounded bound %s has no finite positive float", r.RatString())
	}
	back, err := quoteRatFromFloatDecimal(f)
	if err != nil || back.Cmp(r) != 0 {
		return 0, fmt.Errorf("rounded bound %s does not survive float projection exactly", r.RatString())
	}
	again, err := round(back)
	if err != nil || again.Cmp(back) != 0 {
		return 0, fmt.Errorf("projected bound %v is not a legal venue price", f)
	}
	if (side == "buy" && back.Cmp(x) > 0) || (side == "sell" && back.Cmp(x) < 0) {
		return 0, fmt.Errorf("projected bound %v would widen the %s bound %v", f, side, bound)
	}
	return f, nil
}

var pairEvidenceReaderScript = "shared_scripts/fetch_pair_evidence.py"

var pairEvidenceReaderClearedEnv = []string{
	"HYPERLIQUID_SECRET_KEY", "HYPERLIQUID_ACCOUNT_ADDRESS",
	"OKX_API_KEY", "OKX_API_SECRET", "OKX_PASSPHRASE",
}

type pairPublicReadResult struct {
	OK         bool         `json:"ok"`
	Provider   string       `json:"provider"`
	Instrument string       `json:"instrument"`
	Provenance string       `json:"provenance"`
	ObservedMs int64        `json:"observed_ms"`
	Meta       pairWireMeta `json:"meta"`
}

func pairReadPublicCapability(ctx context.Context, key pairEvidenceKey, prev *pairCapabilityEvidence, validForMs int64) (pairCapabilityEvidence, error) {
	if _, err := key.memberKey(); err != nil {
		return pairCapabilityEvidence{}, err
	}
	if _, ok := ctx.Deadline(); !ok {
		return pairCapabilityEvidence{}, fmt.Errorf("the public capability read needs a caller deadline")
	}
	if validForMs < 1 || validForMs > pairMaxIntervalMs {
		return pairCapabilityEvidence{}, fmt.Errorf("capability validity %d ms is outside [1, %d]", validForMs, pairMaxIntervalMs)
	}
	if prev != nil && prev.Key != key {
		return pairCapabilityEvidence{}, fmt.Errorf("the earlier capability evidence names %s, not %s", prev.Key, key)
	}
	env := map[string]string{"PYTHONDONTWRITEBYTECODE": "1"}
	for _, k := range pairEvidenceReaderClearedEnv {
		env[k] = ""
	}
	args := []string{"--provider=" + key.Provider, "--instrument=" + key.Instrument}
	stdout, stderr, runErr := spawnPythonProcessWithEnv(ctx, pairEvidenceReaderScript, args, nil, 0, env)
	receivedMs := time.Now().UnixMilli()
	tree, perr := pairStrictParse(bytes.TrimSpace(stdout))
	if perr != nil {
		if runErr != nil {
			return pairCapabilityEvidence{}, fmt.Errorf("public capability read for %s failed: %v (%s)", key, runErr, strings.TrimSpace(string(stderr)))
		}
		return pairCapabilityEvidence{}, fmt.Errorf("public capability read for %s returned unreadable output: %w", key, perr)
	}
	if obj, ok := tree.(map[string]interface{}); ok {
		if ok, _ := obj["ok"].(bool); !ok {
			msg, _ := obj["error"].(string)
			return pairCapabilityEvidence{}, fmt.Errorf("public capability read for %s refused: %s", key, msg)
		}
	}
	if runErr != nil {
		return pairCapabilityEvidence{}, fmt.Errorf("public capability read for %s failed: %v", key, runErr)
	}
	var res pairPublicReadResult
	if err := pairStrictDecodeTree(tree, bytes.TrimSpace(stdout), &res, "public capability result"); err != nil {
		return pairCapabilityEvidence{}, err
	}
	if res.Provider != key.Provider || res.Instrument != key.Instrument {
		return pairCapabilityEvidence{}, fmt.Errorf("public capability result names %s %s, not %s %s", res.Provider, res.Instrument, key.Provider, key.Instrument)
	}
	if want := pairDerivationIDs[key.Provider]; res.Provenance != want {
		return pairCapabilityEvidence{}, fmt.Errorf("public capability result derivation %q is not %q", res.Provenance, want)
	}
	if err := pairValidateWireMeta(key.Provider, res.Meta); err != nil {
		return pairCapabilityEvidence{}, err
	}
	if res.ObservedMs < 0 || res.ObservedMs > receivedMs {
		return pairCapabilityEvidence{}, fmt.Errorf("public capability observation time %d is after its receipt at %d", res.ObservedMs, receivedMs)
	}
	through, ok := pairAddMs(receivedMs, validForMs)
	if !ok {
		return pairCapabilityEvidence{}, fmt.Errorf("capability validity overflows")
	}
	metaBytes, err := pairCanonicalJSON(res.Meta)
	if err != nil {
		return pairCapabilityEvidence{}, err
	}
	gen := uint64(1)
	if prev != nil {
		gen = prev.MetadataGeneration
		if _, _, _, same := pairWireMetaDiff(prev.Meta, res.Meta); !same {
			gen++
		}
	}
	out := pairCapabilityEvidence{
		pairEvidenceStamp: pairEvidenceStamp{V: pairEvidenceVersion, Key: key,
			EvidenceID: fmt.Sprintf("%s:%d:%s", res.Provenance, receivedMs, pairSHA256(metaBytes)[:16]),
			ObservedMs: res.ObservedMs, FirstReceiptMs: receivedMs, ValidFromMs: receivedMs, ValidThroughMs: through, Provenance: res.Provenance},
		Meta: res.Meta.clone(), MetadataGeneration: gen,
	}
	return out.sealed()
}

// Package diagnosticanalyzer reads controlled, offline diagnostic exports. It
// does not contact services or import producer business implementations.
package diagnosticanalyzer

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const ContractDigest = "ddb202238cfdaabef1af11575dbfcac788fdbc5a457aea5e72b913afc5b478d4"
const SchemaVersion = "ai-proxy-diagnostics/1"

// This is a byte-for-byte copy of the frozen schema, checked by tests. Keeping
// it here lets the executable validate offline without a working directory.
//
//go:embed record.schema.json
var recordSchema []byte

type object = map[string]any

func obj(v any) object { m, _ := v.(map[string]any); return m }
func str(v any) string { s, _ := v.(string); return s }
func num(v any) *uint64 {
	n, ok := v.(json.Number)
	if !ok {
		return nil
	}
	r, ok := new(big.Rat).SetString(string(n))
	if !ok || !r.IsInt() || r.Sign() < 0 || !r.Num().IsUint64() {
		return nil
	}
	u := r.Num().Uint64()
	return &u
}
func positive(v any) bool { n := num(v); return n != nil && *n > 0 }

// decodeUnique rejects duplicate keys before any data can enter the report.
// Error messages are fixed codes; decoder errors may contain input bytes.
func decodeUnique(b []byte) (any, string) {
	if !utf8.Valid(b) {
		return nil, "invalid_utf8"
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var value func(int) (any, string)
	value = func(depth int) (any, string) {
		if depth > 64 {
			return nil, "depth_limit"
		}
		t, err := d.Token()
		if err != nil {
			return nil, "invalid_json"
		}
		if delim, ok := t.(json.Delim); ok {
			switch delim {
			case '{':
				m := object{}
				for d.More() {
					k, err := d.Token()
					if err != nil {
						return nil, "invalid_json"
					}
					key, ok := k.(string)
					if !ok {
						return nil, "invalid_json"
					}
					if _, exists := m[key]; exists {
						return nil, "duplicate_key"
					}
					v, reason := value(depth + 1)
					if reason != "" {
						return nil, reason
					}
					m[key] = v
				}
				if end, err := d.Token(); err != nil || end != json.Delim('}') {
					return nil, "invalid_json"
				}
				return m, ""
			case '[':
				a := []any{}
				for d.More() {
					v, reason := value(depth + 1)
					if reason != "" {
						return nil, reason
					}
					a = append(a, v)
				}
				if end, err := d.Token(); err != nil || end != json.Delim(']') {
					return nil, "invalid_json"
				}
				return a, ""
			default:
				return nil, "invalid_json"
			}
		}
		if n, ok := t.(json.Number); ok {
			// Bound rational normalization even for adversarial exponent spellings.
			s := string(n)
			if len(s) > 64 {
				return nil, "numeric_limit"
			}
			if i := strings.IndexAny(s, "eE"); i >= 0 {
				e, err := strconv.Atoi(s[i+1:])
				if err != nil || e < -308 || e > 308 {
					return nil, "numeric_limit"
				}
			}
		}
		return t, ""
	}
	v, reason := value(0)
	if reason != "" {
		return nil, reason
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, "invalid_json"
	}
	return v, ""
}

// canonical is typed and length-delimited, with mathematical JSON-number
// equality. It cannot hide differing payloads through floating point rounding.
func canonical(v any) string {
	switch x := v.(type) {
	case nil:
		return "z"
	case bool:
		if x {
			return "t"
		}
		return "f"
	case json.Number:
		r, _ := new(big.Rat).SetString(string(x))
		return "n" + r.RatString() + ";"
	case string:
		return "s" + strconv.Itoa(len(x)) + ":" + x
	case []any:
		var b strings.Builder
		b.WriteString("[")
		for _, y := range x {
			b.WriteString(canonical(y))
		}
		b.WriteString("]")
		return b.String()
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b strings.Builder
		b.WriteString("{")
		for _, k := range keys {
			b.WriteString(canonical(k))
			b.WriteString(canonical(x[k]))
		}
		b.WriteString("}")
		return b.String()
	default:
		return "invalid"
	}
}

type validator func(any) bool

// compileSchema implements only the keywords present in the frozen artifact.
// Unknown keywords fail compilation, not validation-open. It is intentionally
// not an extensible JSON Schema implementation and never resolves network refs.
func compileSchema(raw any, root object) (validator, error) {
	if b, ok := raw.(bool); ok {
		return func(any) bool { return b }, nil
	}
	s := obj(raw)
	if s == nil {
		return nil, errors.New("schema_invalid")
	}
	checks := []validator{}
	for k, v := range s {
		switch k {
		case "$schema", "$id", "title", "$defs", "then", "else":
		case "$ref":
			ref := str(v)
			if !strings.HasPrefix(ref, "#/$defs/") {
				return nil, errors.New("schema_ref")
			}
			f, err := compileSchema(obj(root["$defs"])[strings.TrimPrefix(ref, "#/$defs/")], root)
			if err != nil {
				return nil, err
			}
			checks = append(checks, f)
		case "type":
			typ := str(v)
			checks = append(checks, func(x any) bool {
				switch typ {
				case "null":
					return x == nil
				case "object":
					return obj(x) != nil
				case "array":
					_, ok := x.([]any)
					return ok
				case "string":
					_, ok := x.(string)
					return ok
				case "boolean":
					_, ok := x.(bool)
					return ok
				case "number", "integer":
					n, ok := x.(json.Number)
					if !ok {
						return false
					}
					r, ok := new(big.Rat).SetString(string(n))
					return ok && (typ == "number" || r.IsInt())
				default:
					return false
				}
			})
		case "const":
			want := canonical(v)
			checks = append(checks, func(x any) bool { return canonical(x) == want })
		case "enum":
			allowed := map[string]bool{}
			for _, x := range v.([]any) {
				allowed[canonical(x)] = true
			}
			checks = append(checks, func(x any) bool { return allowed[canonical(x)] })
		case "required":
			keys := v.([]any)
			checks = append(checks, func(x any) bool {
				m := obj(x)
				if m == nil {
					return true
				}
				for _, key := range keys {
					if _, ok := m[str(key)]; !ok {
						return false
					}
				}
				return true
			})
		case "properties":
			props := map[string]validator{}
			for key, sub := range obj(v) {
				f, err := compileSchema(sub, root)
				if err != nil {
					return nil, err
				}
				props[key] = f
			}
			checks = append(checks, func(x any) bool {
				m := obj(x)
				for key, f := range props {
					if y, ok := m[key]; ok && !f(y) {
						return false
					}
				}
				return true
			})
		case "additionalProperties":
			if v != false {
				return nil, errors.New("schema_keyword")
			}
			props := obj(s["properties"])
			checks = append(checks, func(x any) bool {
				for key := range obj(x) {
					if _, ok := props[key]; !ok {
						return false
					}
				}
				return true
			})
		case "minLength", "maxLength", "minItems", "maxItems":
			bound := *num(v)
			keyword := k
			checks = append(checks, func(x any) bool {
				var size int
				switch y := x.(type) {
				case string:
					if !strings.HasSuffix(keyword, "Length") {
						return true
					}
					size = utf8.RuneCountInString(y)
				case []any:
					if !strings.HasSuffix(keyword, "Items") {
						return true
					}
					size = len(y)
				default:
					return true
				}
				if strings.HasPrefix(keyword, "min") {
					return uint64(size) >= bound
				}
				return uint64(size) <= bound
			})
		case "minimum", "maximum":
			bound, _ := new(big.Rat).SetString(string(v.(json.Number)))
			minimum := k == "minimum"
			checks = append(checks, func(x any) bool {
				n, ok := x.(json.Number)
				if !ok {
					return true
				}
				r, ok := new(big.Rat).SetString(string(n))
				if !ok {
					return false
				}
				c := r.Cmp(bound)
				return minimum && c >= 0 || !minimum && c <= 0
			})
		case "pattern":
			pattern := str(v)
			nonzero := 0
			if strings.HasPrefix(pattern, "^(?!0{32}$)") {
				pattern = "^[0-9a-f]{32}$"
				nonzero = 32
			}
			if strings.HasPrefix(pattern, "^(?!0{16}$)") {
				pattern = "^[0-9a-f]{16}$"
				nonzero = 16
			}
			re, err := regexp.Compile(pattern)
			if err != nil {
				return nil, errors.New("schema_pattern")
			}
			checks = append(checks, func(x any) bool {
				t, ok := x.(string)
				return !ok || (re.MatchString(t) && (nonzero == 0 || t != strings.Repeat("0", nonzero)))
			})
		case "format":
			if v != "date-time" {
				return nil, errors.New("schema_format")
			}
			checks = append(checks, func(x any) bool {
				t, ok := x.(string)
				if !ok {
					return true
				}
				_, err := time.Parse("2006-01-02T15:04:05.000Z", t)
				return err == nil
			})
		case "items":
			f, err := compileSchema(v, root)
			if err != nil {
				return nil, err
			}
			checks = append(checks, func(x any) bool {
				a, _ := x.([]any)
				for _, y := range a {
					if !f(y) {
						return false
					}
				}
				return true
			})
		case "not":
			f, err := compileSchema(v, root)
			if err != nil {
				return nil, err
			}
			checks = append(checks, func(x any) bool { return !f(x) })
		case "allOf", "anyOf", "oneOf":
			fs := []validator{}
			for _, sub := range v.([]any) {
				f, err := compileSchema(sub, root)
				if err != nil {
					return nil, err
				}
				fs = append(fs, f)
			}
			mode := k
			checks = append(checks, func(x any) bool {
				n := 0
				for _, f := range fs {
					if f(x) {
						n++
					}
				}
				return mode == "allOf" && n == len(fs) || mode == "anyOf" && n > 0 || mode == "oneOf" && n == 1
			})
		case "if":
			f, err := compileSchema(v, root)
			if err != nil {
				return nil, err
			}
			yes, no := validator(func(any) bool { return true }), validator(func(any) bool { return true })
			if sub, ok := s["then"]; ok {
				yes, err = compileSchema(sub, root)
				if err != nil {
					return nil, err
				}
			}
			if sub, ok := s["else"]; ok {
				no, err = compileSchema(sub, root)
				if err != nil {
					return nil, err
				}
			}
			checks = append(checks, func(x any) bool {
				if f(x) {
					return yes(x)
				}
				return no(x)
			})
		default:
			return nil, errors.New("schema_keyword")
		}
	}
	return func(x any) bool {
		for _, f := range checks {
			if !f(x) {
				return false
			}
		}
		return true
	}, nil
}

func semanticIssues(r object) []string {
	issues := []string{}
	kind := str(r["spanKind"])
	d := obj(r["data"])
	if kind == "server" && r["serverSpanId"] != r["spanId"] {
		issues = append(issues, "server_owner")
	}
	if kind == "call" && r["parentSpanId"] != r["serverSpanId"] {
		issues = append(issues, "call_owner")
	}
	if r["spanId"] != nil && r["spanId"] == r["parentSpanId"] {
		issues = append(issues, "self_parent")
	}
	if r["event"] == "diag.server" || r["event"] == "diag.call" {
		if canonical(obj(d["coverage"])["expectedLastLogSeq"]) != canonical(r["logSeq"]) {
			issues = append(issues, "terminal_sequence")
		}
	}
	if kind == "server" && (r["contextSource"] == "accepted") != (r["parentSpanId"] != nil) {
		issues = append(issues, "context_parent")
	}
	if r["event"] == "upstream.attempt_finished" && d["resultClass"] == "success" {
		o := obj(d["output"])
		if d["terminalSeen"] != true || d["eofSeen"] != true || d["parserFinishOk"] != true || !(positive(o["ordinaryTextNonWhitespaceChars"]) || positive(o["validToolCalls"]) || positive(o["mediaParts"])) {
			issues = append(issues, "success_evidence")
		}
	}
	sort.Strings(issues)
	return issues
}

// Package jsonresult reproduces, byte for byte, the JSON the Python server
// hands to the model.
//
// Every payload the Python emits goes through actions.json_result():
//
//	json.dumps(payload, ensure_ascii=False, indent=2)
//
// Three things make that different from json.MarshalIndent, and all three are
// visible to the model:
//
//  1. Go escapes <, > and & to <, > and & by default. Python does
//     not. UI-tree XML dumps and WeChat message text are full of them.
//     Text uses an Encoder with SetEscapeHTML(false).
//  2. Go sorts map[string]any keys alphabetically; Python preserves insertion
//     order. Use Obj (or a struct with ordered fields) — never map[string]any.
//  3. Go renders float64(0) as "0"; Python renders 0.0 as "0.0". Use Float for
//     any value that is a Python float on the other side.
//
// Two more traps that are the caller's responsibility:
//
//   - A nil Go slice marshals to null; Python's [] marshals to []. Initialise
//     empty slices as []T{}.
//   - len(s) in Python is a rune count, not a byte count. Use RuneLen.
package jsonresult

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Obj is an insertion-ordered JSON object. The zero value is not usable; call
// New.
type Obj struct {
	keys []string
	vals map[string]any
}

// New returns an empty ordered object.
func New() *Obj {
	return &Obj{vals: make(map[string]any)}
}

// Of builds an object from alternating key/value pairs, in order.
// It panics if len(kv) is odd or a key is not a string — both are programmer
// errors that would otherwise surface as a malformed payload at runtime.
func Of(kv ...any) *Obj {
	if len(kv)%2 != 0 {
		panic("jsonresult.Of: odd number of arguments")
	}
	o := New()
	for i := 0; i < len(kv); i += 2 {
		key, ok := kv[i].(string)
		if !ok {
			panic(fmt.Sprintf("jsonresult.Of: key %d is %T, want string", i, kv[i]))
		}
		o.Set(key, kv[i+1])
	}
	return o
}

// Set stores value under key. An existing key keeps its original position,
// matching Python dict assignment semantics.
func (o *Obj) Set(key string, value any) *Obj {
	if _, exists := o.vals[key]; !exists {
		o.keys = append(o.keys, key)
	}
	o.vals[key] = value
	return o
}

// SetDefault stores value only when key is absent, appending it last. This is
// Python's payload.setdefault(key, value), which is how server.py's _ok() makes
// "ok" the LAST key of phone_backend, phone_list_devices, phone_device_info,
// phone_screenshot and phone_current_app while it stays first everywhere else.
func (o *Obj) SetDefault(key string, value any) *Obj {
	if _, exists := o.vals[key]; !exists {
		o.keys = append(o.keys, key)
		o.vals[key] = value
	}
	return o
}

// Get returns the value stored under key.
func (o *Obj) Get(key string) (any, bool) {
	v, ok := o.vals[key]
	return v, ok
}

// Has reports whether key is present.
func (o *Obj) Has(key string) bool {
	_, ok := o.vals[key]
	return ok
}

// Delete removes key, preserving the order of the rest.
func (o *Obj) Delete(key string) {
	if _, ok := o.vals[key]; !ok {
		return
	}
	delete(o.vals, key)
	for i, k := range o.keys {
		if k == key {
			o.keys = append(o.keys[:i:i], o.keys[i+1:]...)
			break
		}
	}
}

// Keys returns the keys in insertion order.
func (o *Obj) Keys() []string {
	out := make([]string, len(o.keys))
	copy(out, o.keys)
	return out
}

// Len returns the number of keys.
func (o *Obj) Len() int { return len(o.keys) }

// Map returns a plain map copy. Useful for handing a payload to the MCP SDK as
// CallToolResult.StructuredContent, where key order is not observable because
// the SDK marshals it itself.
func (o *Obj) Map() map[string]any {
	out := make(map[string]any, len(o.vals))
	for k, v := range o.vals {
		out[k] = v
	}
	return out
}

// MarshalJSON emits the object in insertion order with HTML escaping disabled.
//
// The enclosing encoder re-indents and re-compacts this output; because Text
// sets SetEscapeHTML(false) on that encoder too, the unescaped bytes survive.
func (o *Obj) MarshalJSON() ([]byte, error) {
	if o == nil {
		return []byte("null"), nil
	}
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		kb, err := marshalRaw(k)
		if err != nil {
			return nil, err
		}
		buf.Write(kb)
		buf.WriteByte(':')
		vb, err := marshalRaw(o.vals[k])
		if err != nil {
			return nil, fmt.Errorf("key %q: %w", k, err)
		}
		buf.Write(vb)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// marshalRaw marshals v compactly without HTML escaping.
func marshalRaw(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// Text renders v exactly like json.dumps(v, ensure_ascii=False, indent=2):
// two-space indent, ": " between key and value, no HTML escaping, raw UTF-8,
// no trailing newline.
//
// It never fails: a value that cannot be marshalled (a channel, a cyclic
// structure) yields a well-formed {"ok": false, "error": ...} payload instead,
// because a tool that returns nothing at all is worse than one that reports why.
func Text(v any) string {
	out, err := Encode(v)
	if err != nil {
		fallback, ferr := Encode(Of("ok", false, "error", "internal: "+err.Error()))
		if ferr != nil {
			return `{"ok": false, "error": "internal: json encoding failed"}`
		}
		return string(fallback)
	}
	return string(out)
}

// Encode is Text with the marshalling error surfaced.
func Encode(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// Float is a float64 that marshals the way Python's json.dumps writes a float:
// repr() semantics, so an integral value keeps its ".0" and NaN/±Inf render as
// the bare Python tokens.
//
// Use it for every payload value that is a Python float on the other side:
// change_score (round(x, 4)), fps (round(x, 1)) and the echoed relative
// coordinates. Without it, 0.0 silently becomes 0 and 1.0 becomes 1.
type Float float64

// MarshalJSON implements json.Marshaler.
func (f Float) MarshalJSON() ([]byte, error) {
	return []byte(PyRepr(float64(f))), nil
}

// PyRepr formats x the way Python's repr() does.
//
// Python switches to exponent notation when the decimal point position is <= -4
// or > 16, always keeps a ".0" on integral values in fixed notation, and never
// adds one in exponent notation. Go's shortest-round-trip digits are identical
// to Python's, so only the presentation rule has to be reimplemented.
func PyRepr(x float64) string {
	switch {
	case math.IsNaN(x):
		return "NaN" // json.dumps emits bare NaN/Infinity too
	case math.IsInf(x, 1):
		return "Infinity"
	case math.IsInf(x, -1):
		return "-Infinity"
	}

	// Shortest round-trip digits, in a form whose exponent is easy to read off.
	sci := strconv.FormatFloat(x, 'e', -1, 64)
	exp := 0
	if i := strings.IndexByte(sci, 'e'); i >= 0 {
		if v, err := strconv.Atoi(sci[i+1:]); err == nil {
			exp = v
		}
	}
	decpt := exp + 1 // position of the decimal point among the significant digits

	if decpt <= -4 || decpt > 16 {
		return sci // Go already writes "1e+16" / "1e-05", exactly like Python
	}
	fixed := strconv.FormatFloat(x, 'f', -1, 64)
	if !strings.ContainsRune(fixed, '.') {
		fixed += ".0"
	}
	return fixed
}

// PyRound is Python's round(x, ndigits): correct decimal rounding of the exact
// binary value with ties going to even. Go's strconv formats with the same
// round-half-to-even rule on the exact value, so a format/parse round trip is
// both simplest and exact.
//
// Getting this wrong is not cosmetic: math.Round is ties-away-from-zero, so
// round(0.5*1077) is 538 in Python and 539 with math.Round.
func PyRound(x float64, ndigits int) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return x
	}
	if ndigits < 0 {
		scale := math.Pow(10, float64(-ndigits))
		return PyRoundEven(x/scale) * scale
	}
	v, err := strconv.ParseFloat(strconv.FormatFloat(x, 'f', ndigits, 64), 64)
	if err != nil {
		return x
	}
	return v
}

// PyRoundEven is Python's single-argument round(): ties to even, returning a
// float. Callers that need Python's int result should use PyRoundInt.
func PyRoundEven(x float64) float64 { return math.RoundToEven(x) }

// PyRoundInt is Python's round(x) — banker's rounding to an int. Every
// round(v * (n - 1)) coordinate mapping in actions.py needs this, not
// math.Round.
func PyRoundInt(x float64) int {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return 0
	}
	return int(math.RoundToEven(x))
}

// RuneLen is Python's len(str): a count of code points, not bytes. The
// phone_type / phone_paste "length" field is wrong for any non-ASCII text
// without it.
func RuneLen(s string) int { return utf8.RuneCountInString(s) }

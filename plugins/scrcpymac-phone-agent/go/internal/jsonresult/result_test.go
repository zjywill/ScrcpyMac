package jsonresult

import (
	"math"
	"testing"
)

// The expected strings in this file were produced by running the real Python:
//
//	json.dumps(payload, ensure_ascii=False, indent=2)
//
// If one of them starts failing, the payload the model sees has changed shape.

func TestTextMatchesPythonJSONDumps(t *testing.T) {
	payload := Of(
		"serial", "2f019965",
		"width", 1080,
		"height", 2280,
		"note", "a<b>&c",
		"text", "搜索 😀",
		"score", Float(0.0),
		"ratio", Float(1.0),
		"items", []string{},
		"nested", Of("k", "v"),
	)
	payload.SetDefault("ok", true)

	want := "{\n" +
		"  \"serial\": \"2f019965\",\n" +
		"  \"width\": 1080,\n" +
		"  \"height\": 2280,\n" +
		"  \"note\": \"a<b>&c\",\n" +
		"  \"text\": \"搜索 😀\",\n" +
		"  \"score\": 0.0,\n" +
		"  \"ratio\": 1.0,\n" +
		"  \"items\": [],\n" +
		"  \"nested\": {\n" +
		"    \"k\": \"v\"\n" +
		"  },\n" +
		"  \"ok\": true\n" +
		"}"

	if got := Text(payload); got != want {
		t.Errorf("Text mismatch\n got: %q\nwant: %q", got, want)
	}
}

func TestSetDefaultAppendsLast(t *testing.T) {
	o := Of("ok", false, "error", "boom")
	o.SetDefault("ok", true) // already present: no move, no overwrite
	if got := Text(o); got != "{\n  \"ok\": false,\n  \"error\": \"boom\"\n}" {
		t.Errorf("SetDefault must not reorder or overwrite: %q", got)
	}
}

func TestEmptyContainers(t *testing.T) {
	if got := Text(New()); got != "{}" {
		t.Errorf("empty object: %q", got)
	}
	if got := Text([]string{}); got != "[]" {
		t.Errorf("empty slice: %q", got)
	}
	// A nil slice is Python's None, not []. This is the trap callers must avoid.
	var nilSlice []string
	if got := Text(nilSlice); got != "null" {
		t.Errorf("nil slice: %q", got)
	}
}

func TestPyReprMatchesPython(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0.0, "0.0"},
		{1.0, "1.0"},
		{math.Copysign(0, -1), "-0.0"}, // Go folds the literal -0.0 to +0.0
		{0.0347, "0.0347"},
		{3.14, "3.14"},
		{1e15, "1000000000000000.0"},
		{1e16, "1e+16"},
		{1e-5, "1e-05"},
		{0.0001, "0.0001"},
		{1234567.0, "1234567.0"},
		{123456789012345678.0, "1.2345678901234568e+17"},
	}
	for _, tc := range cases {
		if got := PyRepr(tc.in); got != tc.want {
			t.Errorf("PyRepr(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestPyRoundIsBankers(t *testing.T) {
	// The concrete divergence from math.Round, which returns 539 here.
	if got := PyRoundInt(0.5 * 1077); got != 538 {
		t.Errorf("PyRoundInt(0.5*1077) = %d, want 538", got)
	}
	for _, tc := range []struct {
		in   float64
		want int
	}{{2.5, 2}, {0.5, 0}, {1.5, 2}, {-0.5, 0}, {-1.5, -2}} {
		if got := PyRoundInt(tc.in); got != tc.want {
			t.Errorf("PyRoundInt(%v) = %d, want %d", tc.in, got, tc.want)
		}
	}
	if got := PyRound(0.03474999, 4); got != 0.0347 {
		t.Errorf("PyRound(0.03474999, 4) = %v, want 0.0347", got)
	}
	// Python's round(2.675, 2) is 2.67 because 2.675 is really 2.67499...
	if got := PyRound(2.675, 2); got != 2.67 {
		t.Errorf("PyRound(2.675, 2) = %v, want 2.67", got)
	}
}

func TestRuneLenCountsCodePoints(t *testing.T) {
	if got := RuneLen("搜索"); got != 2 {
		t.Errorf("RuneLen(搜索) = %d, want 2 (len() in Python)", got)
	}
}

func TestDeleteKeepsOrder(t *testing.T) {
	o := Of("a", 1, "b", 2, "c", 3)
	o.Delete("b")
	if got := Text(o); got != "{\n  \"a\": 1,\n  \"c\": 3\n}" {
		t.Errorf("Delete reordered: %q", got)
	}
}

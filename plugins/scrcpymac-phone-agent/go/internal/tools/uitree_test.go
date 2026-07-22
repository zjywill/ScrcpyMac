package tools

// Golden tests for the accessibility-tree group.
//
// Every expected value in testdata/*.json was produced by running the REAL
// Python implementation (server/phone_agent/actions.py) over the XML fixtures in
// testdata/ — see testdata/gen_goldens.py, which is the only supported way to
// regenerate them. Nothing here re-derives the expectations from the Go code, so
// a divergence between the two implementations fails the build rather than
// showing up on a device.
//
// The fixtures themselves came off the attached OnePlus 6 (serial 2f019965,
// Chinese locale):
//
//	settings_home.xml      Settings homepage — an ordinary app, not degraded
//	settings_display.xml   Display settings — the only real switch (checkable+checked)
//	chrome_example.xml     Chrome on example.com — the page's WebView node is
//	                       dropped by the inclusion filter, so NOT degraded
//	chrome_webview.xml     Chrome on info.cern.ch — the WebView scrolls, so it
//	                       survives the filter and the tree IS degraded
//	sparse_dialog.xml      synthetic Compose-style screen: 2 interactive nodes,
//	                       degraded through the interactive<3 rule
//	flags.xml              synthetic: every optional flag, odd bounds, CJK,
//	                       HTML characters, missing attributes
//	malformed.xml          settings_home.xml truncated mid-element
//	empty.xml              an empty dump

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zjywill/scrcpyMac/phone-agent/internal/adb"
	"github.com/zjywill/scrcpyMac/phone-agent/internal/jsonresult"
	"github.com/zjywill/scrcpyMac/phone-agent/internal/mcpserver"
)

const uitreeTestSerial = "2f019965"

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func uitreeFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(data)
}

// uitreeGolden returns a golden file with the trailing newline the generator
// appends removed.
func uitreeGolden(t *testing.T, name string) string {
	t.Helper()
	return strings.TrimSuffix(uitreeFixture(t, name), "\n")
}

func uitreeGoldenJSON(t *testing.T, name string, out any) {
	t.Helper()
	if err := json.Unmarshal([]byte(uitreeFixture(t, name)), out); err != nil {
		t.Fatalf("decode golden %s: %v", name, err)
	}
}

// uitreeFakeClock is the Go counterpart of gen_goldens.py's FakeClock: time only
// advances by the sleeps the code under test asks for.
type uitreeFakeClock struct {
	now    float64
	sleeps []float64
}

func newUITreeFakeClock() *uitreeFakeClock { return &uitreeFakeClock{now: 1000} }

func (c *uitreeFakeClock) Now() float64 { return c.now }

func (c *uitreeFakeClock) Sleep(_ context.Context, seconds float64) {
	c.sleeps = append(c.sleeps, jsonresult.PyRound(seconds, 6))
	c.now += seconds
}

// uitreeFakeInput records what find_and_tap and scroll_to_find asked for.
type uitreeFakeInput struct {
	width, height int
	sizeKnown     bool
	taps          [][3]int // x, y, verify(0/1)
	swipes        [][5]int
	tapErr        error
	swipeErr      error
}

func newUITreeFakeInput() *uitreeFakeInput {
	return &uitreeFakeInput{width: 1080, height: 2280, sizeKnown: true}
}

func (f *uitreeFakeInput) Tap(_ context.Context, x, y int, verify bool) (*jsonresult.Obj, error) {
	v := 0
	if verify {
		v = 1
	}
	f.taps = append(f.taps, [3]int{x, y, v})
	if f.tapErr != nil {
		return nil, f.tapErr
	}
	return jsonresult.Of("ok", true, "action", "tap", "x", x, "y", y, "serial", uitreeTestSerial), nil
}

func (f *uitreeFakeInput) Swipe(_ context.Context, x1, y1, x2, y2, durationMS int) (*jsonresult.Obj, error) {
	f.swipes = append(f.swipes, [5]int{x1, y1, x2, y2, durationMS})
	if f.swipeErr != nil {
		return nil, f.swipeErr
	}
	return jsonresult.Of("ok", true, "action", "swipe"), nil
}

func (f *uitreeFakeInput) ScreenSize(context.Context) (int, int, bool) {
	return f.width, f.height, f.sizeKnown
}

// uitreeTestGroup wires a group to a fake clock, a fake input backend and a
// scripted dump, so nothing touches adb.
type uitreeTestGroup struct {
	*uitreeGroup
	clock   *uitreeFakeClock
	input   *uitreeFakeInput
	dumps   int
	forced  []bool
	docs    []string
	dumpErr error
}

// newUITreeTestGroup returns a group whose dump replays docs, repeating the last
// entry once exhausted.
func newUITreeTestGroup(docs ...string) *uitreeTestGroup {
	clock := newUITreeFakeClock()
	input := newUITreeFakeInput()
	harness := &uitreeTestGroup{clock: clock, input: input, docs: docs}
	group := &uitreeGroup{
		cache: &uitreeCache{},
		input: input,
		now:   clock.Now,
		sleep: clock.Sleep,
	}
	group.dump = func(context.Context) (string, string, error) {
		if harness.dumpErr != nil {
			return "", "", harness.dumpErr
		}
		index := harness.dumps
		harness.dumps++
		if index >= len(harness.docs) {
			index = len(harness.docs) - 1
		}
		return harness.docs[index], uitreeTestSerial, nil
	}
	group.tree = func(ctx context.Context, compact, forceRefresh bool) (*uitreeTree, error) {
		harness.forced = append(harness.forced, forceRefresh)
		return group.uiTree(ctx, compact, forceRefresh)
	}
	harness.uitreeGroup = group
	return harness
}

// ---------------------------------------------------------------------------
// Compaction
// ---------------------------------------------------------------------------

func TestUITreeCompactionMatchesPython(t *testing.T) {
	fixtures := []string{
		"settings_home.xml",
		"settings_display.xml",
		"chrome_example.xml",
		"chrome_webview.xml",
		"sparse_dialog.xml",
		"flags.xml",
		"malformed.xml",
		"empty.xml",
	}
	for _, fixture := range fixtures {
		t.Run(fixture, func(t *testing.T) {
			group := newUITreeTestGroup(uitreeFixture(t, fixture))
			tree, err := group.uiTree(context.Background(), true, false)
			if err != nil {
				t.Fatalf("uiTree: %v", err)
			}
			got := jsonresult.Text(OK(tree.payload()))
			want := uitreeGolden(t, strings.Replace(fixture, ".xml", ".compact.json", 1))
			if got != want {
				t.Errorf("compact tree differs from the Python golden:\n%s", uitreeDiff(want, got))
			}
		})
	}
}

func TestUITreeRawMatchesPython(t *testing.T) {
	for _, fixture := range []string{"sparse_dialog.xml", "empty.xml"} {
		t.Run(fixture, func(t *testing.T) {
			group := newUITreeTestGroup(uitreeFixture(t, fixture))
			tree, err := group.uiTree(context.Background(), false, false)
			if err != nil {
				t.Fatalf("uiTree: %v", err)
			}
			got := jsonresult.Text(OK(tree.payload()))
			want := uitreeGolden(t, strings.Replace(fixture, ".xml", ".raw.json", 1))
			if got != want {
				t.Errorf("raw tree differs from the Python golden:\n%s", uitreeDiff(want, got))
			}
		})
	}
}

// TestUITreeDegradedTriggers states the two rules in isolation, because the
// goldens prove equality without saying what is being tested.
func TestUITreeDegradedTriggers(t *testing.T) {
	cases := []struct {
		fixture      string
		wantDegraded bool
		why          string
	}{
		{"settings_home.xml", false, "28 nodes, 26 interactive, no WebView"},
		{"settings_display.xml", false, "ordinary settings list"},
		{"chrome_example.xml", false, "the page WebView is not scrollable, so the filter drops it"},
		{"chrome_webview.xml", true, "a scrollable WebView survives the filter"},
		{"sparse_dialog.xml", true, "only 2 interactive nodes"},
		{"flags.xml", false, "plenty of interactive nodes, no WebView"},
		{"malformed.xml", true, "parse error"},
		{"empty.xml", true, "parse error"},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			tree := uitreeBuildTree(uitreeFixture(t, tc.fixture), uitreeTestSerial)
			if tree.Degraded != tc.wantDegraded {
				t.Errorf("degraded = %v, want %v (%s)", tree.Degraded, tc.wantDegraded, tc.why)
			}
			switch {
			case tree.ParseError && tree.Hint != uitreeParseErrorHint:
				t.Errorf("parse-error hint = %q", tree.Hint)
			case tree.Degraded && !tree.ParseError && tree.Hint != uitreeDegradedHint:
				t.Errorf("degraded hint = %q", tree.Hint)
			case !tree.Degraded && tree.Hint != "":
				t.Errorf("hint set on a healthy tree: %q", tree.Hint)
			}
		})
	}
}

// TestUITreeNodeFlagPresence pins the rules the golden encodes but does not
// explain: enabled appears only when false, checked only alongside checkable,
// every other flag only when true.
func TestUITreeNodeFlagPresence(t *testing.T) {
	tree := uitreeBuildTree(uitreeFixture(t, "flags.xml"), uitreeTestSerial)
	byText := map[string]*uitreeNode{}
	for _, node := range tree.Nodes {
		key := node.Text
		if key == "" {
			key = node.ResourceID
		}
		byText[key] = node
	}

	everything := byText["Everything"]
	if everything == nil {
		t.Fatal("missing the all-flags node")
	}
	for name, ptr := range map[string]*bool{
		"scrollable": everything.Scrollable,
		"password":   everything.Password,
		"focused":    everything.Focused,
		"selected":   everything.Selected,
		"checkable":  everything.Checkable,
		"checked":    everything.Checked,
	} {
		if ptr == nil || !*ptr {
			t.Errorf("%s = %v, want true", name, ptr)
		}
	}
	if everything.Enabled == nil || *everything.Enabled {
		t.Errorf("enabled = %v, want an explicit false", everything.Enabled)
	}

	wifi := byText["Wi-Fi"]
	if wifi == nil || wifi.Checkable == nil || wifi.Checked == nil || *wifi.Checked {
		t.Fatalf("checkable node must carry checked:false, got %+v", wifi)
	}
	if wifi.Enabled != nil || wifi.Password != nil || wifi.Focused != nil || wifi.Selected != nil || wifi.Scrollable != nil {
		t.Errorf("default-state flags must be omitted entirely: %+v", wifi)
	}

	plain := byText["com.example.flags:id/plain_edit"]
	if plain == nil {
		t.Fatal("an empty EditText must be kept even with no text, desc or click")
	}
	if plain.Checked != nil || plain.Checkable != nil {
		t.Errorf("checked must never appear without checkable: %+v", plain)
	}
}

func TestUITreeBoundsCenter(t *testing.T) {
	cases := []struct {
		bounds string
		want   []int
	}{
		{"[0,0][1080,2280]", []int{540, 1140}},
		{"[3,7][10,20]", []int{6, 13}},                // floor division, not rounding
		{"[0,1000][1081,1101]tail", []int{540, 1050}}, // re.match, not fullmatch
		{"[-32,900][1080,1000]", nil},                 // \d+ rejects the minus sign
		{"", nil},
		{"[0,0]", nil},
		{"garbage", nil},
	}
	for _, tc := range cases {
		got := uitreeBoundsCenter(tc.bounds)
		if len(got) != len(tc.want) || (len(got) == 2 && (got[0] != tc.want[0] || got[1] != tc.want[1])) {
			t.Errorf("uitreeBoundsCenter(%q) = %v, want %v", tc.bounds, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Selectors
// ---------------------------------------------------------------------------

type uitreeSelectorCase struct {
	Name         string   `json:"name"`
	Fixture      string   `json:"fixture"`
	Text         []string `json:"text"`
	ContentDesc  []string `json:"content_desc"`
	ResourceID   []string `json:"resource_id"`
	ClassName    []string `json:"class_name"`
	RequireAll   bool     `json:"require_all"`
	Exact        bool     `json:"exact"`
	Index        int      `json:"index"`
	Describe     string   `json:"describe"`
	MatchCount   int      `json:"match_count"`
	MatchIndexes []int    `json:"match_indexes"`
	Matched      *string  `json:"matched"`
}

func (c uitreeSelectorCase) criteria(t *testing.T) *uitreeCriteria {
	t.Helper()
	criteria, err := newUITreeCriteria(c.Text, c.ContentDesc, c.ResourceID, c.ClassName, c.RequireAll, c.Exact)
	if err != nil {
		t.Fatalf("newUITreeCriteria: %v", err)
	}
	return criteria
}

func TestUITreeSelectorsMatchPython(t *testing.T) {
	var cases []uitreeSelectorCase
	uitreeGoldenJSON(t, "selectors.json", &cases)
	if len(cases) == 0 {
		t.Fatal("selectors.json is empty")
	}

	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			tree := uitreeBuildTree(uitreeFixture(t, tc.Fixture), uitreeTestSerial)
			criteria := tc.criteria(t)

			if got := criteria.describe(); got != tc.Describe {
				t.Errorf("describe() = %q, want %q", got, tc.Describe)
			}

			matched := []int{}
			for _, node := range tree.Nodes {
				if criteria.matches(node) {
					matched = append(matched, node.Index)
				}
			}
			if len(matched) != tc.MatchCount {
				t.Errorf("match count = %d, want %d", len(matched), tc.MatchCount)
			}
			for i := range matched {
				if i < len(tc.MatchIndexes) && matched[i] != tc.MatchIndexes[i] {
					t.Errorf("match[%d] = node %d, want node %d", i, matched[i], tc.MatchIndexes[i])
				}
			}

			node := uitreeFindNode(tree.Nodes, criteria, tc.Index)
			switch {
			case tc.Matched == nil && node != nil:
				t.Errorf("index %d matched node %d, want no match", tc.Index, node.Index)
			case tc.Matched != nil && node == nil:
				t.Errorf("index %d found nothing, want:\n%s", tc.Index, *tc.Matched)
			case tc.Matched != nil:
				if got := jsonresult.Text(node); got != *tc.Matched {
					t.Errorf("matched node differs:\n%s", uitreeDiff(*tc.Matched, got))
				}
			}
		})
	}
}

type uitreeDescribeCase struct {
	Text        []string `json:"text"`
	ContentDesc []string `json:"content_desc"`
	ResourceID  []string `json:"resource_id"`
	ClassName   []string `json:"class_name"`
	RequireAll  bool     `json:"require_all"`
	Exact       bool     `json:"exact"`
	Describe    string   `json:"describe"`
}

func TestUITreeDescribeMatchesPython(t *testing.T) {
	var cases []uitreeDescribeCase
	uitreeGoldenJSON(t, "describe.json", &cases)
	if len(cases) == 0 {
		t.Fatal("describe.json is empty")
	}
	for _, tc := range cases {
		criteria, err := newUITreeCriteria(tc.Text, tc.ContentDesc, tc.ResourceID, tc.ClassName, tc.RequireAll, tc.Exact)
		if err != nil {
			t.Fatalf("newUITreeCriteria(%v): %v", tc, err)
		}
		if got := criteria.describe(); got != tc.Describe {
			t.Errorf("describe() = %q, want %q", got, tc.Describe)
		}
	}
}

func TestUITreeCriteriaRequiresAnAttribute(t *testing.T) {
	_, err := newUITreeCriteria(nil, nil, nil, nil, false, false)
	if err == nil || err.Error() != uitreeNoAttributeError {
		t.Fatalf("err = %v, want %q", err, uitreeNoAttributeError)
	}
	// An empty string is dropped by _as_list, so it is not an attribute either.
	if _, err := newUITreeCriteria([]string{""}, nil, nil, nil, false, false); err == nil {
		t.Fatal("an empty selector string must not count as a specified attribute")
	}
	if !adb.IsError(err) {
		t.Errorf("err type = %T, want an adb.Error so the tool reports it as {ok:false,error}", err)
	}
}

// ---------------------------------------------------------------------------
// Poll cadence
// ---------------------------------------------------------------------------

type uitreePollCase struct {
	Name          string   `json:"name"`
	Fixture       string   `json:"fixture"`
	Text          []string `json:"text"`
	ContentDesc   []string `json:"content_desc"`
	ResourceID    []string `json:"resource_id"`
	ClassName     []string `json:"class_name"`
	RequireAll    bool     `json:"require_all"`
	Exact         bool     `json:"exact"`
	Index         int      `json:"index"`
	TimeoutS      float64  `json:"timeout_s"`
	PollIntervalS float64  `json:"poll_interval_s"`
	ScrollToFind  int      `json:"scroll_to_find"`
	ScreenKnown   bool     `json:"screen_known"`
	Dumps         []bool   `json:"dumps"`
	Sleeps        []float64
	Scrolls       int     `json:"scrolls"`
	Found         *string `json:"found"`
	Error         *string `json:"error"`
}

func TestUITreePollMatchesPython(t *testing.T) {
	var cases []uitreePollCase
	uitreeGoldenJSON(t, "poll.json", &cases)
	if len(cases) == 0 {
		t.Fatal("poll.json is empty")
	}

	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			group := newUITreeTestGroup(uitreeFixture(t, tc.Fixture))
			// An unknown screen size makes _scroll_once a no-op that still
			// burns an attempt — and skips its own 0.4s settle.
			group.input.sizeKnown = tc.ScreenKnown
			criteria, err := newUITreeCriteria(tc.Text, tc.ContentDesc, tc.ResourceID, tc.ClassName, tc.RequireAll, tc.Exact)
			if err != nil {
				t.Fatalf("newUITreeCriteria: %v", err)
			}

			node, _, err := group.pollForNode(context.Background(), criteria,
				tc.TimeoutS, tc.PollIntervalS, tc.Index, tc.ScrollToFind)

			if tc.Error != nil {
				if err == nil {
					t.Fatalf("want error %q, got a match", *tc.Error)
				}
				if err.Error() != *tc.Error {
					t.Errorf("error = %q, want %q", err.Error(), *tc.Error)
				}
				if !adb.IsError(err) {
					t.Errorf("error type = %T, want adb.Error", err)
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if got := jsonresult.Text(node); got != *tc.Found {
					t.Errorf("found node differs:\n%s", uitreeDiff(*tc.Found, got))
				}
			}

			// The force_refresh sequence: the first dump reads the cache, every
			// later one refreshes.
			if len(group.forced) != len(tc.Dumps) {
				t.Errorf("ui_tree calls = %d, want %d", len(group.forced), len(tc.Dumps))
			}
			for i := range group.forced {
				if i < len(tc.Dumps) && group.forced[i] != tc.Dumps[i] {
					t.Errorf("ui_tree call %d force_refresh = %v, want %v", i, group.forced[i], tc.Dumps[i])
				}
			}
			if !uitreeFloatsEqual(group.clock.sleeps, tc.Sleeps) {
				t.Errorf("sleeps = %v, want %v", group.clock.sleeps, tc.Sleeps)
			}
			if len(group.input.swipes) != tc.Scrolls {
				t.Errorf("scrolls = %d, want %d", len(group.input.swipes), tc.Scrolls)
			}
			for _, swipe := range group.input.swipes {
				// _scroll_once: mid-screen, 70% -> 30% of the height, 350 ms.
				want := [5]int{540, 1596, 540, 684, 350}
				if swipe != want {
					t.Errorf("scroll swipe = %v, want %v", swipe, want)
				}
			}
		})
	}
}

// UnmarshalJSON keeps the sleep list decodable while normalising it to the same
// rounding the Python FakeClock applied.
func (c *uitreePollCase) UnmarshalJSON(data []byte) error {
	type raw uitreePollCase
	var aux struct {
		raw
		Sleeps []float64 `json:"sleeps"`
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*c = uitreePollCase(aux.raw)
	c.Sleeps = aux.Sleeps
	return nil
}

func uitreeFloatsEqual(got, want []float64) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if jsonresult.PyRound(got[i], 6) != jsonresult.PyRound(want[i], 6) {
			return false
		}
	}
	return true
}

func TestUITreePollPropagatesDumpErrors(t *testing.T) {
	group := newUITreeTestGroup(uitreeFixture(t, "settings_home.xml"))
	group.dumpErr = &adb.Error{Msg: "adb devices -l failed: no devices/emulators found"}
	criteria, err := newUITreeCriteria([]string{"nope"}, nil, nil, nil, false, false)
	if err != nil {
		t.Fatalf("newUITreeCriteria: %v", err)
	}
	_, _, err = group.pollForNode(context.Background(), criteria, 5.0, 0.4, 0, 0)
	if err == nil || err.Error() != group.dumpErr.Error() {
		t.Fatalf("err = %v, want the adb failure to abort the poll", err)
	}
}

// ---------------------------------------------------------------------------
// Cache
// ---------------------------------------------------------------------------

func TestUITreeCacheSemantics(t *testing.T) {
	doc := uitreeFixture(t, "settings_home.xml")
	group := newUITreeTestGroup(doc)
	ctx := context.Background()

	if _, err := group.uiTree(ctx, true, false); err != nil {
		t.Fatalf("first dump: %v", err)
	}
	if _, err := group.uiTree(ctx, true, false); err != nil {
		t.Fatalf("cached call: %v", err)
	}
	if group.dumps != 1 {
		t.Errorf("dumps = %d, want the second compact call served from cache", group.dumps)
	}

	// A mode mismatch re-dumps: the cached compact result has no xml.
	if _, err := group.uiTree(ctx, false, false); err != nil {
		t.Fatalf("raw call: %v", err)
	}
	if group.dumps != 2 {
		t.Errorf("dumps = %d, want a re-dump for the raw shape", group.dumps)
	}

	// force_refresh always re-dumps.
	if _, err := group.uiTree(ctx, false, true); err != nil {
		t.Fatalf("forced call: %v", err)
	}
	if group.dumps != 3 {
		t.Errorf("dumps = %d, want force_refresh to bypass the cache", group.dumps)
	}

	group.cache.invalidate()
	if _, err := group.uiTree(ctx, false, false); err != nil {
		t.Fatalf("post-invalidate call: %v", err)
	}
	if group.dumps != 4 {
		t.Errorf("dumps = %d, want a re-dump after invalidation", group.dumps)
	}
}

func TestUITreeEmptyDumpRetriesOnce(t *testing.T) {
	group := newUITreeTestGroup("", uitreeFixture(t, "settings_home.xml"))
	tree, err := group.uiTree(context.Background(), true, false)
	if err != nil {
		t.Fatalf("uiTree: %v", err)
	}
	if group.dumps != 2 {
		t.Errorf("dumps = %d, want one retry after an empty dump", group.dumps)
	}
	if !uitreeFloatsEqual(group.clock.sleeps, []float64{0.3}) {
		t.Errorf("sleeps = %v, want a single 0.3s pause", group.clock.sleeps)
	}
	if tree.ParseError || len(tree.Nodes) != 28 {
		t.Errorf("retry did not recover the tree: parseError=%v nodes=%d", tree.ParseError, len(tree.Nodes))
	}
}

func TestUITreeParseErrorIsNeverCached(t *testing.T) {
	group := newUITreeTestGroup("")
	ctx := context.Background()
	tree, err := group.uiTree(ctx, true, false)
	if err != nil {
		t.Fatalf("uiTree: %v", err)
	}
	if !tree.ParseError || !tree.Degraded {
		t.Fatalf("empty dump must produce the degraded parse-error shape: %+v", tree)
	}
	if group.cache.get(true) != nil {
		t.Error("a broken dump must not be cached")
	}
	before := group.dumps
	if _, err := group.uiTree(ctx, true, false); err != nil {
		t.Fatalf("second uiTree: %v", err)
	}
	if group.dumps == before {
		t.Error("the next call must re-dump")
	}
	if tree.nodeCount() != 0 {
		t.Errorf("nodeCount = %d, want 0 — the parse-error shape has no count key", tree.nodeCount())
	}
}

func TestInvalidateUITreeCacheIsWiredToTheSharedCache(t *testing.T) {
	uitreeSharedCache.set(&uitreeTree{Compact: true, Serial: uitreeTestSerial})
	InvalidateUITreeCache()
	if uitreeSharedCache.get(true) != nil {
		t.Error("InvalidateUITreeCache must clear the shared cache the mutation hooks call it for")
	}
}

// TestUITreeCacheIsInvalidatedByEveryMutationHook is the regression test for the
// bug this port is most likely to grow: PhoneActions kept the cache and the
// input methods on one object, so every mutation dropped the cache for free.
// Here they live in four files and the wiring is explicit — if any of the three
// hook lists is left unregistered, a tap can be followed by a stale tree.
func TestUITreeCacheIsInvalidatedByEveryMutationHook(t *testing.T) {
	if _, err := registerUITreeForTest(t); err != nil {
		t.Fatalf("register: %v", err)
	}
	for name, notify := range map[string]func(){
		"input tools (tap/swipe/key/type/paste)": inputScreenMutated,
		"device tools (launch_app)":              deviceStateChanged,
		"widget tools (scrcpymac_ui_*)":          widgetNotifyDeviceChange,
	} {
		uitreeSharedCache.set(&uitreeTree{Compact: true, Serial: uitreeTestSerial})
		notify()
		if uitreeSharedCache.get(true) != nil {
			t.Errorf("%s did not invalidate the ui_tree cache", name)
		}
	}
}

// registerUITreeForTest builds a server carrying only this group's tools, which
// is also what installs the mutation hooks.
func registerUITreeForTest(t *testing.T) (*mcpserver.Server, error) {
	t.Helper()
	registry := mcpserver.NewRegistry()
	registry.Add(mcpserver.Registration{Name: "phone-uitree", Order: mcpserver.OrderPhoneTools, Apply: registerUITree})
	return mcpserver.New(context.Background(), mcpserver.Options{Registry: registry})
}

// ---------------------------------------------------------------------------
// find_and_tap / wait_for_text
// ---------------------------------------------------------------------------

func TestUITreeFindAndTapPayload(t *testing.T) {
	group := newUITreeTestGroup(uitreeFixture(t, "settings_home.xml"))
	payload, err := group.handleFindAndTap(context.Background(), uitreeFindAndTapIn{Text: "帐号"})
	if err != nil {
		t.Fatalf("handleFindAndTap: %v", err)
	}
	if keys := payload.Keys(); len(keys) != 3 || keys[0] != "ok" || keys[1] != "matched" || keys[2] != "tap" {
		t.Fatalf("key order = %v, want [ok matched tap]", keys)
	}
	if len(group.input.taps) != 1 {
		t.Fatalf("taps = %d, want 1", len(group.input.taps))
	}
	// The first 帐号 node is at index 8, bounds [72,1085][1008,1148] -> centre.
	if got := group.input.taps[0]; got[2] != 1 {
		t.Errorf("verify flag = %d, want 1 (the tool default)", got[2])
	}
	text := jsonresult.Text(payload)
	if !strings.Contains(text, `"matched"`) || !strings.Contains(text, `"tap"`) {
		t.Errorf("payload lost a key:\n%s", text)
	}
	if !strings.Contains(text, "帐号") {
		t.Errorf("non-ASCII must survive as literal UTF-8:\n%s", text)
	}
}

func TestUITreeFindAndTapVerifyPassthrough(t *testing.T) {
	group := newUITreeTestGroup(uitreeFixture(t, "settings_home.xml"))
	verify := false
	if _, err := group.handleFindAndTap(context.Background(), uitreeFindAndTapIn{Text: "帐号", Verify: &verify}); err != nil {
		t.Fatalf("handleFindAndTap: %v", err)
	}
	if group.input.taps[0][2] != 0 {
		t.Error("verify=false must reach the tap")
	}
}

func TestUITreeFindAndTapRequiresASelector(t *testing.T) {
	group := newUITreeTestGroup(uitreeFixture(t, "settings_home.xml"))
	_, err := group.handleFindAndTap(context.Background(), uitreeFindAndTapIn{})
	if err == nil || err.Error() != uitreeNoSelectorError {
		t.Fatalf("err = %v, want %q", err, uitreeNoSelectorError)
	}
	if group.dumps != 0 {
		t.Error("the selector guard must run before any device access")
	}
}

func TestUITreeFindAndTapRejectsNodeWithoutBounds(t *testing.T) {
	group := newUITreeTestGroup(uitreeFixture(t, "flags.xml"))
	_, err := group.handleFindAndTap(context.Background(), uitreeFindAndTapIn{Text: "No bounds"})
	want := "Matched node has no tappable bounds (text=['No bounds'])"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
	if len(group.input.taps) != 0 {
		t.Error("an untappable node must not be tapped")
	}
}

func TestUITreeFindAndTapPropagatesTapErrors(t *testing.T) {
	group := newUITreeTestGroup(uitreeFixture(t, "settings_home.xml"))
	group.input.tapErr = &adb.Error{Msg: "adb shell input tap 540 1116 failed: closed"}
	_, err := group.handleFindAndTap(context.Background(), uitreeFindAndTapIn{Text: "帐号"})
	if err == nil || !strings.Contains(err.Error(), "input tap") {
		t.Fatalf("err = %v, want the tap failure to surface", err)
	}
}

type uitreeWaitCase struct {
	Name     string  `json:"name"`
	Fixture  string  `json:"fixture"`
	Text     string  `json:"text"`
	TimeoutS float64 `json:"timeout_s"`
	Result   *string `json:"result"`
	Error    *string `json:"error"`
}

func TestUITreeWaitForTextMatchesPython(t *testing.T) {
	var cases []uitreeWaitCase
	uitreeGoldenJSON(t, "wait_for_text.json", &cases)
	if len(cases) == 0 {
		t.Fatal("wait_for_text.json is empty")
	}
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			group := newUITreeTestGroup(uitreeFixture(t, tc.Fixture))
			payload, err := group.waitForText(context.Background(), uitreeAsList(tc.Text), tc.TimeoutS, uitreePollIntervalDefault)
			if tc.Error != nil {
				if err == nil {
					t.Fatalf("want error %q", *tc.Error)
				}
				if err.Error() != *tc.Error {
					t.Errorf("error = %q, want %q", err.Error(), *tc.Error)
				}
				return
			}
			if err != nil {
				t.Fatalf("waitForText: %v", err)
			}
			if got := jsonresult.Text(OK(payload)); got != *tc.Result {
				t.Errorf("result differs:\n%s", uitreeDiff(*tc.Result, got))
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The input seam
// ---------------------------------------------------------------------------

func TestSetUITreeInputOverridesTheDefault(t *testing.T) {
	replacement := newUITreeFakeInput()
	SetUITreeInput(replacement)
	t.Cleanup(func() { SetUITreeInput(nil) })

	group := newUITreeTestGroup(uitreeFixture(t, "settings_home.xml"))
	if _, err := group.handleFindAndTap(context.Background(), uitreeFindAndTapIn{Text: "帐号"}); err != nil {
		t.Fatalf("handleFindAndTap: %v", err)
	}
	if len(replacement.taps) != 1 {
		t.Errorf("installed backend taps = %d, want 1", len(replacement.taps))
	}
	if len(group.input.taps) != 0 {
		t.Error("the installed backend must win over the group's default")
	}
}

// TestUITreeInputAdapterIsTheDefault documents that the group taps through the
// phone-input group's implementation rather than a second copy of it.
func TestUITreeInputAdapterIsTheDefault(t *testing.T) {
	group := newUITreeGroup(&mcpserver.Env{})
	if _, ok := group.input.(*uitreeInputAdapter); !ok {
		t.Fatalf("default input = %T, want *uitreeInputAdapter", group.input)
	}
	if group.cache != uitreeSharedCache {
		t.Error("the group must use the process-wide cache the mutation hooks invalidate")
	}
}

// ---------------------------------------------------------------------------
// Tool surface
// ---------------------------------------------------------------------------

type uitreeContractTool struct {
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	InputSchema  json.RawMessage `json:"inputSchema"`
	OutputSchema json.RawMessage `json:"outputSchema"`
	Annotations  json.RawMessage `json:"annotations"`
	Meta         json.RawMessage `json:"meta"`
}

func uitreeContract(t *testing.T) map[string]uitreeContractTool {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "contract.json"))
	if err != nil {
		t.Fatalf("read contract.json: %v", err)
	}
	var contract struct {
		Tools []uitreeContractTool `json:"tools"`
	}
	if err := json.Unmarshal(data, &contract); err != nil {
		t.Fatalf("decode contract.json: %v", err)
	}
	out := make(map[string]uitreeContractTool, len(contract.Tools))
	for _, tool := range contract.Tools {
		out[tool.Name] = tool
	}
	return out
}

func TestUITreeToolsMatchTheFrozenContract(t *testing.T) {
	server, err := registerUITreeForTest(t)
	if err != nil {
		t.Fatalf("mcpserver.New: %v", err)
	}

	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	ctx := context.Background()
	serverSession, err := server.MCP.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer func() { _ = serverSession.Close() }()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer func() { _ = session.Close() }()

	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	got := map[string]*mcp.Tool{}
	for _, tool := range listed.Tools {
		got[tool.Name] = tool
	}
	if len(got) != 3 {
		t.Fatalf("registered %d tools, want exactly phone_ui_tree, phone_find_and_tap, phone_wait_for_text", len(got))
	}

	contract := uitreeContract(t)
	for _, name := range []string{"phone_ui_tree", "phone_find_and_tap", "phone_wait_for_text"} {
		t.Run(name, func(t *testing.T) {
			tool, ok := got[name]
			if !ok {
				t.Fatalf("%s was not registered", name)
			}
			want, ok := contract[name]
			if !ok {
				t.Fatalf("%s is missing from contract.json", name)
			}
			if tool.Description != want.Description {
				t.Errorf("description differs:\n got %q\nwant %q", tool.Description, want.Description)
			}
			if tool.Annotations != nil {
				t.Errorf("annotations = %+v, want none", tool.Annotations)
			}
			if tool.Meta != nil {
				t.Errorf("_meta = %+v, want none — these tools are model-visible", tool.Meta)
			}
			uitreeAssertJSONEqual(t, "inputSchema", tool.InputSchema, want.InputSchema)
			uitreeAssertJSONEqual(t, "outputSchema", tool.OutputSchema, want.OutputSchema)
		})
	}
}

func uitreeAssertJSONEqual(t *testing.T, label string, got any, want json.RawMessage) {
	t.Helper()
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal %s: %v", label, err)
	}
	var gotValue, wantValue any
	if err := json.Unmarshal(gotJSON, &gotValue); err != nil {
		t.Fatalf("decode %s: %v", label, err)
	}
	if err := json.Unmarshal(want, &wantValue); err != nil {
		t.Fatalf("decode contract %s: %v", label, err)
	}
	gotNormal, _ := json.Marshal(uitreeNormalizeJSON(gotValue))
	wantNormal, _ := json.Marshal(uitreeNormalizeJSON(wantValue))
	if string(gotNormal) != string(wantNormal) {
		t.Errorf("%s differs:\n got %s\nwant %s", label, gotNormal, wantNormal)
	}
}

// uitreeNormalizeJSON drops keys the contract capture does not carry, so the
// comparison is about the schema the client sees, not about SDK bookkeeping.
func uitreeNormalizeJSON(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for k, v := range typed {
			if k == "$schema" || k == "additionalProperties" {
				continue
			}
			out[k] = uitreeNormalizeJSON(v)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, v := range typed {
			out[i] = uitreeNormalizeJSON(v)
		}
		return out
	default:
		return value
	}
}

// ---------------------------------------------------------------------------
// Parser edge cases the fixtures cannot express
// ---------------------------------------------------------------------------

func TestUITreeParseErrors(t *testing.T) {
	cases := []struct {
		name string
		doc  string
	}{
		{"empty", ""},
		{"whitespace", "   \n  "},
		{"not-xml", "error: device offline"},
		{"unclosed", `<?xml version='1.0' encoding='UTF-8' standalone='yes' ?><hierarchy><node text="a"`},
		{"truncated-element", `<hierarchy><node text="a"></hierarchy>`},
		{"bad-entity", `<hierarchy><node text="&nope;" clickable="true"/></hierarchy>`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := uitreeParseXML(tc.doc); err == nil {
				t.Error("want a parse error")
			}
			tree := uitreeBuildTree(tc.doc, uitreeTestSerial)
			if !tree.ParseError || !tree.Degraded {
				t.Errorf("tree = %+v, want the degraded parse-error shape", tree)
			}
			if tree.XML != tc.doc {
				t.Error("the parse-error shape echoes the raw dump back")
			}
		})
	}
}

func TestUITreeParseAcceptsAWellFormedDumpWithNoNodes(t *testing.T) {
	nodes, err := uitreeParseXML(`<?xml version='1.0' encoding='UTF-8' standalone='yes' ?><hierarchy rotation="0" />`)
	if err != nil {
		t.Fatalf("uitreeParseXML: %v", err)
	}
	if len(nodes) != 0 {
		t.Fatalf("nodes = %d, want 0", len(nodes))
	}
	tree := uitreeBuildTree(`<?xml version='1.0' encoding='UTF-8' standalone='yes' ?><hierarchy rotation="0" />`, uitreeTestSerial)
	if tree.ParseError {
		t.Error("an empty but well-formed hierarchy is not a parse error")
	}
	if !tree.Degraded {
		t.Error("zero interactive nodes must trip the interactive<3 rule")
	}
	if got := jsonresult.Text(tree.payload()); !strings.Contains(got, `"nodes": []`) {
		t.Errorf("an empty node list must serialise as [], not null:\n%s", got)
	}
}

func TestUITreeParseIsDocumentOrdered(t *testing.T) {
	tree := uitreeBuildTree(uitreeFixture(t, "flags.xml"), uitreeTestSerial)
	for i, node := range tree.Nodes {
		if node.Index != i {
			t.Fatalf("node %d carries index %d — index is the position in the OUTPUT list", i, node.Index)
		}
	}
	// The nested child is emitted after its parent's siblings, i.e. depth-first
	// pre-order, which is what root.iter("node") produces.
	if last := tree.Nodes[len(tree.Nodes)-1]; last.Text != "Nested child" {
		t.Errorf("last node = %q, want the nested child (document order)", last.Text)
	}
}

func TestUITreeErrorsAreAdbErrors(t *testing.T) {
	// server.py catches (AdbError, OSError) and renders {"ok": false, "error"}.
	// Anything this group raises has to be in that family.
	group := newUITreeTestGroup(uitreeFixture(t, "settings_home.xml"))
	_, err := group.handleFindAndTap(context.Background(), uitreeFindAndTapIn{})
	var adbErr *adb.Error
	if !errors.As(err, &adbErr) {
		t.Fatalf("err = %T, want *adb.Error", err)
	}
}

// uitreeDiff renders the first differing line of two payloads.
func uitreeDiff(want, got string) string {
	wantLines := strings.Split(want, "\n")
	gotLines := strings.Split(got, "\n")
	for i := 0; i < len(wantLines) || i < len(gotLines); i++ {
		var w, g string
		if i < len(wantLines) {
			w = wantLines[i]
		}
		if i < len(gotLines) {
			g = gotLines[i]
		}
		if w != g {
			return "line " + itoa(i+1) + ":\n want " + w + "\n  got " + g +
				"\n(want " + itoa(len(wantLines)) + " lines, got " + itoa(len(gotLines)) + ")"
		}
	}
	return "(identical)"
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	digits := ""
	for v > 0 {
		digits = string(rune('0'+v%10)) + digits
		v /= 10
	}
	return digits
}

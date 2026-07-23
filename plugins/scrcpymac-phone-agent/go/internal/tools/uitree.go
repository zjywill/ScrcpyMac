package tools

// The accessibility-tree group: phone_ui_tree, phone_find_and_tap
// and phone_wait_for_text, ported from server/phone_agent/actions.py
// (ui_tree, NodeCriteria, _poll_for_node, find_and_tap, wait_for_text) per
// docs/spec-actions.md §6–§10.
//
// Three things here are contract, not implementation detail, and every one of
// them is pinned by a golden generated from the real Python
// (testdata/gen_goldens.py):
//
//   - the compact node shape: eight fixed keys, then the "noteworthy" flags in
//     the order scrollable, enabled, password, focused, selected, checkable,
//     checked — enabled appears ONLY when false, checked ONLY alongside
//     checkable, everything else only when true;
//   - the two degraded triggers: any raw XML node whose class contains
//     "WebView", or fewer than three KEPT nodes that are clickable or carry
//     text. Inspecting raw classes matters because a non-scrolling WebView root
//     is otherwise removed by the compact inclusion filter;
//   - the poll cadence: deadline checked only at the top of the loop, first
//     iteration reads the cache, 1.5^n backoff capped at 2 s, and a scroll that
//     consumes an attempt but skips the sleep.

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zjywill/scrcpyMac/phone-agent/internal/adb"
	"github.com/zjywill/scrcpyMac/phone-agent/internal/jsonresult"
	"github.com/zjywill/scrcpyMac/phone-agent/internal/mcpserver"
)

func init() {
	mcpserver.Register(mcpserver.Registration{
		Name:  "phone-uitree",
		Order: mcpserver.OrderPhoneTools + 40,
		Apply: registerUITree,
	})
}

// ---------------------------------------------------------------------------
// Constants lifted verbatim from the Python. Every string below is
// model-visible and must not drift.
// ---------------------------------------------------------------------------

const (
	// uitreeDumpCommand is AdbClient.ui_tree_xml()'s single round trip: dump,
	// cat, remove. It is ONE argv element — the device's /system/bin/sh parses
	// the &&, the ; and the redirections. Splitting it host-side breaks it.
	uitreeDumpCommand = "uiautomator dump /sdcard/window_dump.xml >/dev/null 2>&1 && cat /sdcard/window_dump.xml; rm -f /sdcard/window_dump.xml"

	// uitreeDumpTimeout is the 30 s the Python passes to shell() for the dump.
	uitreeDumpTimeout = 30 * time.Second

	// uitreeEmptyRetryDelay is the one 0.3 s pause before re-dumping an empty
	// tree. uiautomator can return nothing right after a navigation.
	uitreeEmptyRetryDelay = 0.3

	uitreeParseErrorHint = "UI dump was empty or unparseable. Retry phone_ui_tree or fall back to phone_screenshot for vision."
	uitreeDegradedHint   = "UI tree looks incomplete (WebView/Compose/custom-drawn). Fall back to phone_screenshot for vision."

	// uitreeNoSelectorError is raised by the tool layer (server.py:290) before
	// NodeCriteria ever sees the arguments.
	uitreeNoSelectorError = "Provide text, content_desc, resource_id, or class_name"
	// uitreeNoAttributeError is NodeCriteria's own guard, reachable only from
	// an internal caller — phone_wait_for_text("") hits it.
	uitreeNoAttributeError = "NodeCriteria requires at least one attribute"
)

// Defaults shared by find_and_tap and wait_for_text. poll_interval_s is not
// exposed by either tool; scroll/settle timings come from _scroll_once.
const (
	uitreePollIntervalDefault = 0.4
	uitreePollBackoffBase     = 1.5
	uitreePollBackoffCap      = 2.0
	uitreeScrollDurationMS    = 350
	uitreeScrollSettle        = 0.4
)

// ---------------------------------------------------------------------------
// The input seam
// ---------------------------------------------------------------------------

// UITreeInput is how phone_find_and_tap taps a matched node and how
// scroll_to_find scrolls.
//
// The default implementation is uitreeInputAdapter, which delegates to the
// phone-input group's inputActions so there is exactly one tap in the binary —
// find_and_tap's nested "tap" payload has to be byte-identical to phone_tap's,
// including the plugin control-socket shape used while streaming. The seam is
// here so a future group can take the whole input path over without this file
// growing a second copy of it.
type UITreeInput interface {
	// Tap performs actions.tap(x, y, verify=verify) — clamping, and with
	// verify the 5-point cross with screenshot comparison — and returns the
	// payload find_and_tap nests under "tap".
	Tap(ctx context.Context, x, y int, verify bool) (*jsonresult.Obj, error)
	// Swipe performs actions.swipe(...) and returns its payload.
	Swipe(ctx context.Context, x1, y1, x2, y2, durationMS int) (*jsonresult.Obj, error)
	// ScreenSize reports the device size, or ok=false when it cannot be
	// determined — which makes _clamp_device_point a no-op and _scroll_once
	// silently do nothing, exactly as in the Python.
	ScreenSize(ctx context.Context) (width, height int, ok bool)
}

var (
	uitreeInputMu       sync.Mutex
	uitreeInputOverride UITreeInput
)

// SetUITreeInput installs the input backend phone_find_and_tap uses. Passing nil
// restores the default adapter over the phone-input group. Intended to be called
// from another tool group's Apply.
func SetUITreeInput(input UITreeInput) {
	uitreeInputMu.Lock()
	defer uitreeInputMu.Unlock()
	uitreeInputOverride = input
}

func uitreeCurrentInput(fallback UITreeInput) UITreeInput {
	uitreeInputMu.Lock()
	defer uitreeInputMu.Unlock()
	if uitreeInputOverride != nil {
		return uitreeInputOverride
	}
	return fallback
}

// ---------------------------------------------------------------------------
// The tree cache
// ---------------------------------------------------------------------------

// uitreeCache is PhoneActions._ui_tree_cache: the last successful dump, with no
// TTL, shared by every tool in the process.
//
// The staleness is load-bearing, not an oversight: _poll_for_node's first
// iteration deliberately reads it so a find_and_tap right after a launch_app is
// fast. Every mutating tool must call InvalidateUITreeCache — the Python
// invalidates on select_device, tap, swipe, key, type_text, paste and
// launch_app, and (deliberately) NOT on shell, screenshot or current_app.
type uitreeCache struct {
	mu   sync.Mutex
	tree *uitreeTree
}

var uitreeSharedCache = &uitreeCache{}

// InvalidateUITreeCache drops the cached accessibility tree. Call it from any
// tool that changes what is on screen.
func InvalidateUITreeCache() { uitreeSharedCache.invalidate() }

func (c *uitreeCache) invalidate() {
	c.mu.Lock()
	c.tree = nil
	c.mu.Unlock()
}

// get returns the cached tree when it was captured in the requested mode. The
// Python tests `"nodes" in cached` / `"xml" in cached`, which is the same thing:
// a compact result has nodes and no xml, a raw one the reverse.
func (c *uitreeCache) get(compact bool) *uitreeTree {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tree == nil || c.tree.Compact != compact {
		return nil
	}
	return c.tree
}

func (c *uitreeCache) set(tree *uitreeTree) {
	c.mu.Lock()
	c.tree = tree
	c.mu.Unlock()
}

// ---------------------------------------------------------------------------
// The compact node
// ---------------------------------------------------------------------------

// uitreeNode is one entry of the compact tree.
//
// Field order IS key order, and the pointer fields reproduce the Python's
// presence rules exactly: omitempty drops a nil pointer, so a non-nil pointer to
// false (Enabled, Checked) still serialises.
type uitreeNode struct {
	// Index is the position in the OUTPUT list, not the XML index attribute,
	// and not what find_and_tap's index parameter indexes (that indexes the
	// match list). Three different meanings, one name.
	Index       int    `json:"index"`
	Text        string `json:"text"`
	ContentDesc string `json:"content_desc"`
	ResourceID  string `json:"resource_id"`
	Class       string `json:"class"`
	Clickable   bool   `json:"clickable"`
	Bounds      string `json:"bounds"`
	// Center is nil (JSON null) when bounds do not parse — which includes any
	// negative coordinate, since the Python regex only accepts \d+.
	Center []int `json:"center"`

	Scrollable *bool `json:"scrollable,omitempty"`
	Enabled    *bool `json:"enabled,omitempty"`
	Password   *bool `json:"password,omitempty"`
	Focused    *bool `json:"focused,omitempty"`
	Selected   *bool `json:"selected,omitempty"`
	Checkable  *bool `json:"checkable,omitempty"`
	Checked    *bool `json:"checked,omitempty"`
}

// disabled reports whether the node carries enabled=false, which is the only
// state NodeCriteria's enabled_only filter looks at.
func (n *uitreeNode) disabled() bool { return n.Enabled != nil && !*n.Enabled }

// uitreeTree is one ui_tree() result, kept as a struct so the cache can hand out
// a fresh payload per call (the Python returns dict(cached), a shallow copy).
type uitreeTree struct {
	Compact    bool
	Serial     string
	XML        string
	Nodes      []*uitreeNode
	Degraded   bool
	ParseError bool
	Hint       string
}

// nodeCount is last_tree.get("count", 0): the parse-error and raw shapes carry
// no count key at all, so a timeout after one of those reports 0 nodes.
func (t *uitreeTree) nodeCount() int {
	if t == nil || t.ParseError || !t.Compact {
		return 0
	}
	return len(t.Nodes)
}

// payload renders the tree in the Python's key order.
func (t *uitreeTree) payload() *jsonresult.Obj {
	switch {
	case t.ParseError:
		// ok is TRUE even though the dump failed — the failure is reported by
		// parse_error/degraded, not by ok.
		return jsonresult.Of(
			"ok", true,
			"xml", t.XML,
			"serial", t.Serial,
			"parse_error", true,
			"degraded", true,
			"hint", uitreeParseErrorHint,
		)
	case !t.Compact:
		return jsonresult.Of("ok", true, "xml", t.XML, "serial", t.Serial)
	}
	payload := jsonresult.Of("ok", true, "nodes", t.Nodes, "count", len(t.Nodes), "serial", t.Serial)
	if t.Degraded {
		payload.Set("degraded", true)
		payload.Set("hint", t.Hint)
	}
	return payload
}

// ---------------------------------------------------------------------------
// XML parsing and compaction
// ---------------------------------------------------------------------------

// uitreeBoundsRe is Python's re.match(r"\[(\d+),(\d+)\]\[(\d+),(\d+)\]", bounds):
// anchored at the start only, so trailing junk is allowed, and \d+ means no
// negative coordinate ever parses.
var uitreeBoundsRe = regexp.MustCompile(`^\[(\d+),(\d+)\]\[(\d+),(\d+)\]`)

// uitreeAttrs is the attribute subset the compaction reads. Anything absent is
// the empty string, exactly like Python's attrib.get(key, "").
type uitreeAttrs struct {
	text        string
	contentDesc string
	class       string
	resourceID  string
	bounds      string
	clickable   string
	scrollable  string
	checkable   string
	checked     string
	enabled     string
	password    string
	focused     string
	selected    string
}

type uitreeParseMeta struct {
	hasWebView bool
}

// uitreeParseXML walks every <node> element in document order and compacts it.
//
// A non-nil error is the ET.ParseError branch: an empty dump, a truncated dump,
// or anything that is not XML at all. Callers must NOT cache such a result.
func uitreeParseXML(doc string) ([]*uitreeNode, error) {
	nodes, _, err := uitreeParseXMLWithMeta(doc)
	return nodes, err
}

func uitreeParseXMLWithMeta(doc string) ([]*uitreeNode, uitreeParseMeta, error) {
	decoder := xml.NewDecoder(strings.NewReader(doc))
	decoder.Strict = true

	nodes := []*uitreeNode{}
	meta := uitreeParseMeta{}
	sawElement := false
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, uitreeParseMeta{}, err
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		sawElement = true
		if start.Name.Local != "node" {
			continue
		}
		for _, attr := range start.Attr {
			if attr.Name.Local == "class" && strings.Contains(attr.Value, "WebView") {
				meta.hasWebView = true
				break
			}
		}
		if node := uitreeCompactNode(start, len(nodes)); node != nil {
			nodes = append(nodes, node)
		}
	}
	if !sawElement {
		// Go's decoder happily reports EOF for "" or for plain text, where
		// ElementTree raises ParseError("no element found"). Map it back.
		return nil, uitreeParseMeta{}, errors.New("no element found")
	}
	return nodes, meta, nil
}

// uitreeCompactNode applies the inclusion filter and builds the node, or returns
// nil when the node is dropped.
func uitreeCompactNode(start xml.StartElement, index int) *uitreeNode {
	var a uitreeAttrs
	for _, attr := range start.Attr {
		switch attr.Name.Local {
		case "text":
			a.text = attr.Value
		case "content-desc":
			a.contentDesc = attr.Value
		case "class":
			a.class = attr.Value
		case "resource-id":
			a.resourceID = attr.Value
		case "bounds":
			a.bounds = attr.Value
		case "clickable":
			a.clickable = attr.Value
		case "scrollable":
			a.scrollable = attr.Value
		case "checkable":
			a.checkable = attr.Value
		case "checked":
			a.checked = attr.Value
		case "enabled":
			a.enabled = attr.Value
		case "password":
			a.password = attr.Value
		case "focused":
			a.focused = attr.Value
		case "selected":
			a.selected = attr.Value
		}
	}

	text := strings.TrimSpace(a.text)
	desc := strings.TrimSpace(a.contentDesc)
	class := a.class // deliberately NOT trimmed
	clickable := a.clickable == "true"
	scrollable := a.scrollable == "true"
	checkable := a.checkable == "true"
	// A Switch is often clickable=false because the parent row takes the click,
	// and an empty EditText has neither text nor description — so both are kept
	// on their own merits. `editable` never appears in the output.
	editable := strings.Contains(class, "EditText")

	if text == "" && desc == "" && !clickable && !scrollable && !editable && !checkable {
		return nil
	}

	node := &uitreeNode{
		Index:       index,
		Text:        text,
		ContentDesc: desc,
		ResourceID:  a.resourceID,
		Class:       class,
		Clickable:   clickable,
		Bounds:      a.bounds,
		Center:      uitreeBoundsCenter(a.bounds),
	}
	if scrollable {
		node.Scrollable = BoolPtr(true)
	}
	if a.enabled == "false" {
		node.Enabled = BoolPtr(false)
	}
	if a.password == "true" {
		node.Password = BoolPtr(true)
	}
	if a.focused == "true" {
		node.Focused = BoolPtr(true)
	}
	if a.selected == "true" {
		node.Selected = BoolPtr(true)
	}
	if checkable {
		node.Checkable = BoolPtr(true)
		node.Checked = BoolPtr(a.checked == "true")
	}
	return node
}

// uitreeBoundsCenter is _bounds_center: the integer midpoint of "[x1,y1][x2,y2]",
// or nil when the string does not match.
func uitreeBoundsCenter(bounds string) []int {
	match := uitreeBoundsRe.FindStringSubmatch(bounds)
	if match == nil {
		return nil
	}
	values := make([]int, 4)
	for i := 0; i < 4; i++ {
		v, err := strconv.Atoi(match[i+1])
		if err != nil {
			// Python has arbitrary-precision ints and would not fail here, but
			// a coordinate that overflows an int is not tappable either way.
			return nil
		}
		values[i] = v
	}
	// Python's // on non-negative ints is Go's integer division; the regex
	// guarantees non-negative.
	return []int{(values[0] + values[2]) / 2, (values[1] + values[3]) / 2}
}

// uitreeBuildTree turns a dump into the compact result, including the two
// degraded triggers.
func uitreeBuildTree(doc, serial string) *uitreeTree {
	nodes, meta, err := uitreeParseXMLWithMeta(doc)
	if err != nil {
		return &uitreeTree{Compact: true, Serial: serial, XML: doc, ParseError: true, Degraded: true, Hint: uitreeParseErrorHint}
	}

	interactive := 0
	for _, node := range nodes {
		// Python truthiness: clickable true, or a non-empty text.
		if node.Clickable || node.Text != "" {
			interactive++
		}
	}

	tree := &uitreeTree{Compact: true, Serial: serial, Nodes: nodes}
	if meta.hasWebView || interactive < 3 {
		tree.Degraded = true
		tree.Hint = uitreeDegradedHint
	}
	return tree
}

// ---------------------------------------------------------------------------
// NodeCriteria
// ---------------------------------------------------------------------------

// uitreeCriteria is NodeCriteria: up to four attributes, each a list of
// alternatives OR'd together, the attributes themselves AND'd under require_all
// and OR'd otherwise.
type uitreeCriteria struct {
	Text        []string
	ContentDesc []string
	ResourceID  []string
	ClassName   []string
	RequireAll  bool
	Exact       bool
	// ClickableOnly and EnabledOnly are never overridden by any caller in the
	// Python; they exist because the type does.
	ClickableOnly bool
	EnabledOnly   bool
}

// uitreeAsList is _as_list: nil and empty strings drop out, so text=["", "x"]
// is the same selector as text="x".
func uitreeAsList(values ...string) []string {
	out := []string{}
	for _, v := range values {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

// newUITreeCriteria mirrors NodeCriteria.__init__, including its guard.
func newUITreeCriteria(text, contentDesc, resourceID, className []string, requireAll, exact bool) (*uitreeCriteria, error) {
	criteria := &uitreeCriteria{
		Text:        uitreeAsList(text...),
		ContentDesc: uitreeAsList(contentDesc...),
		ResourceID:  uitreeAsList(resourceID...),
		ClassName:   uitreeAsList(className...),
		RequireAll:  requireAll,
		Exact:       exact,
		EnabledOnly: true,
	}
	if len(criteria.Text) == 0 && len(criteria.ContentDesc) == 0 &&
		len(criteria.ResourceID) == 0 && len(criteria.ClassName) == 0 {
		return nil, &adb.Error{Msg: uitreeNoAttributeError}
	}
	return criteria, nil
}

// matches is NodeCriteria.matches. Matching is case-sensitive, substring by
// default, and applied to the node's "class" key (not "class_name").
func (c *uitreeCriteria) matches(node *uitreeNode) bool {
	if c.EnabledOnly && node.disabled() {
		return false
	}
	if c.ClickableOnly && !node.Clickable {
		return false
	}

	specified := make([]bool, 0, 4)
	if len(c.Text) > 0 {
		specified = append(specified, uitreeAnyMatch(c.Text, node.Text, c.Exact))
	}
	if len(c.ContentDesc) > 0 {
		specified = append(specified, uitreeAnyMatch(c.ContentDesc, node.ContentDesc, c.Exact))
	}
	if len(c.ResourceID) > 0 {
		// Substring, deliberately: resource_id="menu_search" has to match
		// com.tencent.mm:id/menu_search, which recipes/wechat.py relies on.
		specified = append(specified, uitreeAnyMatch(c.ResourceID, node.ResourceID, c.Exact))
	}
	if len(c.ClassName) > 0 {
		specified = append(specified, uitreeAnyMatch(c.ClassName, node.Class, c.Exact))
	}
	if len(specified) == 0 {
		return false
	}
	if c.RequireAll {
		for _, hit := range specified {
			if !hit {
				return false
			}
		}
		return true
	}
	for _, hit := range specified {
		if hit {
			return true
		}
	}
	return false
}

// uitreeAnyMatch is _any_match.
func uitreeAnyMatch(needles []string, haystack string, exact bool) bool {
	for _, needle := range needles {
		if exact {
			if needle == haystack {
				return true
			}
			continue
		}
		if needle != "" && strings.Contains(haystack, needle) {
			return true
		}
	}
	return false
}

// describe is NodeCriteria.describe(): the model-visible rendering inside the
// "Element not found" and "no tappable bounds" errors. The label for class_name
// is "class", and the values are Python list reprs.
func (c *uitreeCriteria) describe() string {
	parts := make([]string, 0, 6)
	for _, group := range []struct {
		name   string
		values []string
	}{
		{"text", c.Text},
		{"content_desc", c.ContentDesc},
		{"resource_id", c.ResourceID},
		{"class", c.ClassName},
	} {
		if len(group.values) > 0 {
			parts = append(parts, group.name+"="+uitreePyReprList(group.values))
		}
	}
	if c.RequireAll {
		parts = append(parts, "require_all=True")
	}
	if c.Exact {
		parts = append(parts, "exact=True")
	}
	return strings.Join(parts, ", ")
}

// uitreePyReprList renders a []string the way Python's repr() renders a list of
// str: ['a', 'b'], with printable non-ASCII kept literal.
func uitreePyReprList(values []string) string {
	quoted := make([]string, 0, len(values))
	for _, v := range values {
		quoted = append(quoted, uitreePyReprString(v))
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// uitreePyReprString renders one string the way Python's repr() does: single
// quotes, switching to double quotes when the value contains a ' and no ".
func uitreePyReprString(s string) string {
	quote := byte('\'')
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		quote = '"'
	}
	var b strings.Builder
	b.WriteByte(quote)
	for _, r := range s {
		switch {
		case r == rune(quote):
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r > 0x7f && !unicode.IsPrint(r):
			switch {
			case r <= 0xff:
				fmt.Fprintf(&b, `\x%02x`, r)
			case r <= 0xffff:
				fmt.Fprintf(&b, `\u%04x`, r)
			default:
				fmt.Fprintf(&b, `\U%08x`, r)
			}
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte(quote)
	return b.String()
}

// uitreeFindNode is _find_node: the index-th node matching criteria, in tree
// order, or nil. A negative index never matches — there is no Python-style
// negative indexing here.
func uitreeFindNode(nodes []*uitreeNode, criteria *uitreeCriteria, index int) *uitreeNode {
	if index < 0 {
		return nil
	}
	seen := 0
	for _, node := range nodes {
		if !criteria.matches(node) {
			continue
		}
		if seen == index {
			return node
		}
		seen++
	}
	return nil
}

// ---------------------------------------------------------------------------
// The group
// ---------------------------------------------------------------------------

// uitreeGroup owns the three tools. now/sleep/dump are fields so tests can drive
// the poll cadence on a fake clock without sleeping or touching a device.
type uitreeGroup struct {
	env   *mcpserver.Env
	cache *uitreeCache
	input UITreeInput

	// now returns epoch seconds as a float64 — the same representation
	// Python's time.time() uses, so the deadline arithmetic is bit-identical.
	now   func() float64
	sleep func(ctx context.Context, seconds float64)
	dump  func(ctx context.Context) (doc string, serial string, err error)
	// tree indirects ui_tree so the poll loop can be driven, and its exact
	// force_refresh sequence observed, without a device.
	tree func(ctx context.Context, compact, forceRefresh bool) (*uitreeTree, error)
}

func newUITreeGroup(env *mcpserver.Env) *uitreeGroup {
	group := &uitreeGroup{
		env:   env,
		cache: uitreeSharedCache,
		now:   uitreeNow,
		sleep: uitreeSleep,
	}
	group.input = &uitreeInputAdapter{actions: newInputActions(env)}
	group.dump = group.adbDump
	group.tree = group.uiTree
	return group
}

func uitreeNow() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// uitreeSleep is a context-aware time.sleep: a shutdown must not have to wait
// out a 2 s backoff.
func uitreeSleep(ctx context.Context, seconds float64) {
	if seconds <= 0 {
		return
	}
	timer := time.NewTimer(time.Duration(seconds * float64(time.Second)))
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
	}
}

// adbDump is _dump_ui_xml: ensure a device, then one shell round trip.
func (g *uitreeGroup) adbDump(ctx context.Context) (string, string, error) {
	client, err := g.env.ADB()
	if err != nil {
		return "", "", err
	}
	if _, err := client.EnsureDevice(ctx); err != nil {
		return "", "", err
	}
	doc, err := client.ShellTimeout(ctx, uitreeDumpTimeout, uitreeDumpCommand)
	if err != nil {
		return "", "", err
	}
	return doc, client.Serial(), nil
}

// uiTree is PhoneActions.ui_tree.
func (g *uitreeGroup) uiTree(ctx context.Context, compact, forceRefresh bool) (*uitreeTree, error) {
	if !forceRefresh {
		if cached := g.cache.get(compact); cached != nil {
			return cached, nil
		}
	}

	doc, serial, err := g.dump(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(doc) == "" {
		// One retry, and one only. If the second dump is empty too, the parse
		// below fails and the degraded shape is returned.
		g.sleep(ctx, uitreeEmptyRetryDelay)
		doc, serial, err = g.dump(ctx)
		if err != nil {
			return nil, err
		}
	}

	if !compact {
		tree := &uitreeTree{Compact: false, Serial: serial, XML: doc}
		g.cache.set(tree)
		return tree, nil
	}

	tree := uitreeBuildTree(doc, serial)
	if tree.ParseError {
		// Never cache a broken dump; the next call must re-dump.
		g.cache.invalidate()
		return tree, nil
	}
	g.cache.set(tree)
	return tree, nil
}

// pollForNode is _poll_for_node, the single wait/backoff engine behind
// find_and_tap and wait_for_text.
func (g *uitreeGroup) pollForNode(
	ctx context.Context,
	criteria *uitreeCriteria,
	timeoutS, pollIntervalS float64,
	index, scrollToFind int,
) (*uitreeNode, *uitreeTree, error) {
	deadline := g.now() + timeoutS
	var lastTree *uitreeTree
	scrollsUsed := 0
	attempt := 0

	// The deadline is checked only here, at the top: timeout_s <= 0 performs
	// zero dumps and fails immediately reporting 0 nodes.
	for g.now() < deadline {
		tree, err := g.tree(ctx, true, attempt > 0)
		if err != nil {
			// An adb failure aborts the poll, as in the Python where the
			// AdbError propagates straight out.
			return nil, nil, err
		}
		lastTree = tree
		if node := uitreeFindNode(tree.Nodes, criteria, index); node != nil {
			return node, tree, nil
		}
		if scrollsUsed < scrollToFind {
			if err := g.scrollOnce(ctx); err != nil {
				return nil, nil, err
			}
			scrollsUsed++
			attempt++
			// Re-dump immediately: a scroll consumes an attempt but skips the
			// backoff, so the post-scroll sleeps resume already-advanced.
			continue
		}
		attempt++
		exponent := attempt - 1
		if exponent < 0 {
			exponent = 0
		}
		g.sleep(ctx, math.Min(pollIntervalS*math.Pow(uitreePollBackoffBase, float64(exponent)), uitreePollBackoffCap))
	}

	return nil, lastTree, &adb.Error{Msg: uitreeNotFoundMessage(timeoutS, criteria, lastTree, scrollsUsed, scrollToFind)}
}

// uitreeNotFoundMessage builds _poll_for_node's timeout error by the same string
// concatenation the Python uses. timeout_s is rendered with Python float repr,
// so 10 prints as "10.0".
func uitreeNotFoundMessage(timeoutS float64, criteria *uitreeCriteria, tree *uitreeTree, scrollsUsed, scrollToFind int) string {
	msg := fmt.Sprintf("Element not found within %ss (%s). Last tree had %d nodes",
		jsonresult.PyRepr(timeoutS), criteria.describe(), tree.nodeCount())
	if scrollToFind != 0 {
		msg += fmt.Sprintf(", after %d scroll(s)", scrollsUsed)
	}
	msg += "."
	if tree != nil && tree.Degraded && tree.Hint != "" {
		msg += " " + tree.Hint
	}
	return msg
}

// scrollOnce is _scroll_once(direction="up"): one mid-screen swipe from 70 % to
// 30 % of the height, which moves content up and reveals what is below.
//
// int(h*0.7) is a float multiply then truncation — not h*7/10, which would
// differ — and an unknown screen size makes it a silent no-op that still burns
// an attempt.
func (g *uitreeGroup) scrollOnce(ctx context.Context) error {
	input := uitreeCurrentInput(g.input)
	width, height, ok := input.ScreenSize(ctx)
	if !ok {
		return nil
	}
	x := width / 2
	y1 := int(float64(height) * 0.7)
	y2 := int(float64(height) * 0.3)
	if _, err := input.Swipe(ctx, x, y1, x, y2, uitreeScrollDurationMS); err != nil {
		return err
	}
	g.sleep(ctx, uitreeScrollSettle)
	return nil
}

// findAndTap is PhoneActions.find_and_tap.
func (g *uitreeGroup) findAndTap(
	ctx context.Context,
	criteria *uitreeCriteria,
	index int,
	timeoutS, pollIntervalS float64,
	scrollToFind int,
	verify bool,
) (*jsonresult.Obj, error) {
	node, _, err := g.pollForNode(ctx, criteria, timeoutS, pollIntervalS, index, scrollToFind)
	if err != nil {
		return nil, err
	}
	if node.Center == nil {
		return nil, &adb.Error{Msg: fmt.Sprintf("Matched node has no tappable bounds (%s)", criteria.describe())}
	}
	// retries/retry_radius_px/settle_s stay at the actions-level defaults; the
	// tool's own retries parameter is not plumbed through find_and_tap.
	tap, err := uitreeCurrentInput(g.input).Tap(ctx, node.Center[0], node.Center[1], verify)
	if err != nil {
		return nil, err
	}
	return jsonresult.Of("ok", true, "matched", node, "tap", tap), nil
}

// waitForText is PhoneActions.wait_for_text: text-attribute-only, substring,
// no scrolling.
func (g *uitreeGroup) waitForText(ctx context.Context, text []string, timeoutS, pollIntervalS float64) (*jsonresult.Obj, error) {
	criteria, err := newUITreeCriteria(text, nil, nil, nil, false, false)
	if err != nil {
		return nil, err
	}
	node, tree, err := g.pollForNode(ctx, criteria, timeoutS, pollIntervalS, 0, 0)
	if err != nil {
		return nil, err
	}
	serial := tree.Serial
	if serial == "" && g.env != nil {
		// The Python reads _client.serial directly and never instantiates a
		// client here. Reaching this line means a dump already succeeded, so
		// the client exists.
		if client, cerr := g.env.ADB(); cerr == nil {
			serial = client.Serial()
		}
	}
	return jsonresult.Of("ok", true, "found", node, "serial", serial), nil
}

// ---------------------------------------------------------------------------
// The input adapter
// ---------------------------------------------------------------------------

// uitreeInputAdapter is the default UITreeInput: a thin adapter over the
// phone-input group's inputActions, which owns the whole tap family (clamping,
// the 5-point verification cross, the screenshot change score) and the plugin
// control-socket path used while the H.264 stream is up.
//
// Delegating rather than reimplementing is deliberate: find_and_tap's nested
// "tap" payload has to be byte-identical to phone_tap's, and a second tap
// implementation would drift from it the first time either side is touched.
type uitreeInputAdapter struct {
	actions *inputActions
}

// Tap is actions.tap(x, y, verify=verify). Everything else stays at the
// actions-level defaults — retries=2, retry_radius_px=32, settle_s=0.45 — which
// is exactly what find_and_tap passes; the tool's own retries parameter is not
// plumbed through it.
func (a *uitreeInputAdapter) Tap(ctx context.Context, x, y int, verify bool) (*jsonresult.Obj, error) {
	opts := inputDefaultTapOptions()
	opts.Verify = verify
	return a.actions.tap(ctx, x, y, opts)
}

func (a *uitreeInputAdapter) Swipe(ctx context.Context, x1, y1, x2, y2, durationMS int) (*jsonresult.Obj, error) {
	return a.actions.swipe(ctx, x1, y1, x2, y2, durationMS)
}

func (a *uitreeInputAdapter) ScreenSize(ctx context.Context) (int, int, bool) {
	return a.actions.screenSize(ctx)
}

// ---------------------------------------------------------------------------
// Tool registration
// ---------------------------------------------------------------------------

const uitreeToolDescription = `Dump the UI accessibility tree as JSON nodes or raw XML.
Nodes carry state flags (scrollable, enabled=false, focused, checked...)
only when noteworthy. If the result has "degraded": true, the tree is
incomplete (WebView/Compose/game) — call phone_screenshot and use vision
instead.`

const uitreeFindAndTapDescription = `Find a UI element by visible text, content-desc, resource-id, or class,
then tap it. Provide at least one selector. require_all=True demands every
given selector to hit (use text + resource_id to disambiguate); exact=True
matches whole strings instead of substrings; index=N picks the Nth match;
scroll_to_find=N scrolls down up to N times when the element is off-screen.`

const uitreeWaitForTextDescription = "Wait until the given text appears in the UI tree."

type uitreeTreeIn struct {
	// Compact is a pointer so an omitted argument is distinguishable from an
	// explicit false even if the SDK ever stops applying schema defaults.
	Compact *bool `json:"compact,omitempty"`
}

type uitreeFindAndTapIn struct {
	Text         string   `json:"text,omitempty"`
	ContentDesc  string   `json:"content_desc,omitempty"`
	ResourceID   string   `json:"resource_id,omitempty"`
	ClassName    string   `json:"class_name,omitempty"`
	RequireAll   bool     `json:"require_all,omitempty"`
	Exact        bool     `json:"exact,omitempty"`
	Index        int      `json:"index,omitempty"`
	ScrollToFind int      `json:"scroll_to_find,omitempty"`
	TimeoutS     *float64 `json:"timeout_s,omitempty"`
	Verify       *bool    `json:"verify,omitempty"`
}

type uitreeWaitForTextIn struct {
	Text     string   `json:"text"`
	TimeoutS *float64 `json:"timeout_s,omitempty"`
}

func registerUITree(server *mcp.Server, env *mcpserver.Env) error {
	group := newUITreeGroup(env)

	// The cache lives here, but PhoneActions invalidated it from every mutating
	// action (spec-actions §6.1) and those actions now live in three other
	// files. All three hook lists have to be wired or a tap can be followed by
	// a phone_ui_tree served from a tree captured before it:
	// input.go for tap/swipe/key/type/paste, device.go for launch_app, and
	// widget.go for the app-only scrcpymac_ui_* input tools.
	InputOnScreenMutation(InvalidateUITreeCache)
	RegisterDeviceStateChangeHook(InvalidateUITreeCache)
	WidgetOnDeviceChange(InvalidateUITreeCache)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "phone_ui_tree",
		Description: uitreeToolDescription,
		InputSchema: ObjectSchema("phone_ui_treeArguments",
			Prop{Name: "compact", Type: "boolean", Title: "Compact", Default: true},
		),
		OutputSchema: StringOutputSchema("phone_ui_tree"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in uitreeTreeIn) (*mcp.CallToolResult, StringOut, error) {
		payload, err := group.handleUITree(ctx, in)
		return JSON(payload, err)
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "phone_find_and_tap",
		Description: uitreeFindAndTapDescription,
		InputSchema: ObjectSchema("phone_find_and_tapArguments",
			Prop{Name: "text", Type: "string", Title: "Text", Default: ""},
			Prop{Name: "content_desc", Type: "string", Title: "Content Desc", Default: ""},
			Prop{Name: "resource_id", Type: "string", Title: "Resource Id", Default: ""},
			Prop{Name: "class_name", Type: "string", Title: "Class Name", Default: ""},
			Prop{Name: "require_all", Type: "boolean", Title: "Require All", Default: false},
			Prop{Name: "exact", Type: "boolean", Title: "Exact", Default: false},
			Prop{Name: "index", Type: "integer", Title: "Index", Default: 0},
			Prop{Name: "scroll_to_find", Type: "integer", Title: "Scroll To Find", Default: 0},
			Prop{Name: "timeout_s", Type: "number", Title: "Timeout S", Default: 10},
			Prop{Name: "verify", Type: "boolean", Title: "Verify", Default: true},
		),
		OutputSchema: StringOutputSchema("phone_find_and_tap"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in uitreeFindAndTapIn) (*mcp.CallToolResult, StringOut, error) {
		payload, err := group.handleFindAndTap(ctx, in)
		return JSON(payload, err)
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "phone_wait_for_text",
		Description: uitreeWaitForTextDescription,
		InputSchema: ObjectSchema("phone_wait_for_textArguments",
			Prop{Name: "text", Type: "string", Title: "Text", Required: true},
			Prop{Name: "timeout_s", Type: "number", Title: "Timeout S", Default: 10},
		),
		OutputSchema: StringOutputSchema("phone_wait_for_text"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in uitreeWaitForTextIn) (*mcp.CallToolResult, StringOut, error) {
		timeout := 10.0
		if in.TimeoutS != nil {
			timeout = *in.TimeoutS
		}
		payload, err := group.waitForText(ctx, uitreeAsList(in.Text), timeout, uitreePollIntervalDefault)
		return JSON(payload, err)
	})

	return nil
}

// handleUITree is the tool-level half of phone_ui_tree. force_refresh is not a
// tool parameter — the model always gets the cached tree when one is current.
func (g *uitreeGroup) handleUITree(ctx context.Context, in uitreeTreeIn) (*jsonresult.Obj, error) {
	compact := true
	if in.Compact != nil {
		compact = *in.Compact
	}
	tree, err := g.tree(ctx, compact, false)
	if err != nil {
		return nil, err
	}
	// _ok()'s setdefault is a no-op here: every ui_tree shape already starts
	// with ok.
	return OK(tree.payload()), nil
}

// handleFindAndTap is the tool-level half of phone_find_and_tap: the
// "at least one selector" guard, then the actions call.
func (g *uitreeGroup) handleFindAndTap(ctx context.Context, in uitreeFindAndTapIn) (*jsonresult.Obj, error) {
	if in.Text == "" && in.ContentDesc == "" && in.ResourceID == "" && in.ClassName == "" {
		return nil, &adb.Error{Msg: uitreeNoSelectorError}
	}
	criteria, err := newUITreeCriteria(
		uitreeAsList(in.Text),
		uitreeAsList(in.ContentDesc),
		uitreeAsList(in.ResourceID),
		uitreeAsList(in.ClassName),
		in.RequireAll,
		in.Exact,
	)
	if err != nil {
		return nil, err
	}
	timeout := 10.0
	if in.TimeoutS != nil {
		timeout = *in.TimeoutS
	}
	verify := true
	if in.Verify != nil {
		verify = *in.Verify
	}
	return g.findAndTap(ctx, criteria, in.Index, timeout, uitreePollIntervalDefault, in.ScrollToFind, verify)
}

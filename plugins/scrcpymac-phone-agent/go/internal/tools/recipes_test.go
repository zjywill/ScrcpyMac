package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zjywill/scrcpyMac/phone-agent/internal/adb"
	"github.com/zjywill/scrcpyMac/phone-agent/internal/jsonresult"
	"github.com/zjywill/scrcpyMac/phone-agent/internal/mcpserver"
)

// ---------------------------------------------------------------------------
// A fake action layer. No adb, no device, and — deliberately — no way for this
// test to send a real WeChat message.
// ---------------------------------------------------------------------------

type wechatCall struct {
	Method   string
	Package  string
	Activity string
	Text     []string
	Desc     []string
	RID      []string
	TimeoutS float64
	Arg      string
}

type wechatFake struct {
	calls  []wechatCall
	sleeps []time.Duration

	launchErr error
	waitErrs  map[int]error // keyed by WaitForText call index
	findErrs  map[int]error // keyed by FindAndTap call index
	pasteErrs map[int]error // keyed by Paste call index
	keyErr    error
	shot      WeChatScreenshot
	shotErr   error

	waits  int
	finds  int
	pastes int
	shots  int
}

func newWeChatFake() *wechatFake {
	return &wechatFake{shot: WeChatScreenshot{Width: 1080, Height: 2280, SizeBytes: 214563}}
}

func (f *wechatFake) LaunchApp(_ context.Context, pkg, activity string) (*jsonresult.Obj, error) {
	f.calls = append(f.calls, wechatCall{Method: "LaunchApp", Package: pkg, Activity: activity})
	if f.launchErr != nil {
		return nil, f.launchErr
	}
	return jsonresult.Of("ok", true, "action", "launch", "package", pkg), nil
}

func (f *wechatFake) WaitForText(_ context.Context, alternatives []string, timeoutS float64) (*jsonresult.Obj, error) {
	i := f.waits
	f.waits++
	f.calls = append(f.calls, wechatCall{Method: "WaitForText", Text: alternatives, TimeoutS: timeoutS})
	if err := f.waitErrs[i]; err != nil {
		return nil, err
	}
	return jsonresult.Of("ok", true, "found", fmt.Sprintf("marker%d", i)), nil
}

func (f *wechatFake) FindAndTap(_ context.Context, sel WeChatSelector) (*jsonresult.Obj, error) {
	i := f.finds
	f.finds++
	f.calls = append(f.calls, wechatCall{
		Method:   "FindAndTap",
		Text:     sel.Text,
		Desc:     sel.ContentDesc,
		RID:      sel.ResourceID,
		TimeoutS: sel.TimeoutS,
	})
	if err := f.findErrs[i]; err != nil {
		return nil, err
	}
	return jsonresult.Of("ok", true, "matched", fmt.Sprintf("node%d", i)), nil
}

func (f *wechatFake) Paste(_ context.Context, text string) (*jsonresult.Obj, error) {
	i := f.pastes
	f.pastes++
	f.calls = append(f.calls, wechatCall{Method: "Paste", Arg: text})
	if err := f.pasteErrs[i]; err != nil {
		return nil, err
	}
	return jsonresult.Of("ok", true, "action", "paste"), nil
}

func (f *wechatFake) Key(_ context.Context, name string) (*jsonresult.Obj, error) {
	f.calls = append(f.calls, wechatCall{Method: "Key", Arg: name})
	if f.keyErr != nil {
		return nil, f.keyErr
	}
	return jsonresult.Of("ok", true, "action", "key", "key", name), nil
}

func (f *wechatFake) Screenshot(context.Context) (WeChatScreenshot, error) {
	f.shots++
	f.calls = append(f.calls, wechatCall{Method: "Screenshot"})
	if f.shotErr != nil {
		return WeChatScreenshot{}, f.shotErr
	}
	return f.shot, nil
}

func (f *wechatFake) methods() []string {
	out := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		out = append(out, c.Method)
	}
	return out
}

// wechatTestRecipe wires the fake to a recipe whose sleeps are recorded rather
// than slept, so the ~2.3s of fixed waits cost nothing.
func wechatTestRecipe(f *wechatFake) *wechatRecipe {
	return &wechatRecipe{
		driver:   f,
		timeoutS: wechatDefaultTimeoutS,
		sleep: func(_ context.Context, d time.Duration) error {
			f.sleeps = append(f.sleeps, d)
			return nil
		},
	}
}

// ---------------------------------------------------------------------------
// The happy path
// ---------------------------------------------------------------------------

func TestWeChatHappyPathResult(t *testing.T) {
	fake := newWeChatFake()
	payload, err := wechatTestRecipe(fake).send(context.Background(), "Alice", "hi")
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	want := `{
  "ok": true,
  "recipe": "wechat_send_message",
  "contact": "Alice",
  "message": "hi",
  "steps": [
    {
      "step": "launch_wechat",
      "result": {
        "ok": true,
        "action": "launch",
        "package": "com.tencent.mm"
      }
    },
    {
      "step": "wait_wechat_ready",
      "result": {
        "ok": true,
        "found": "marker0"
      }
    },
    {
      "step": "open_search",
      "result": {
        "ok": true,
        "matched": "node0"
      }
    },
    {
      "step": "wait_search_input",
      "result": {
        "ok": true,
        "found": "marker1"
      }
    },
    {
      "step": "type_contact",
      "contact": "Alice"
    },
    {
      "step": "open_chat",
      "result": {
        "ok": true,
        "matched": "node1"
      }
    },
    {
      "step": "type_message",
      "length": 2
    },
    {
      "step": "tap_send",
      "result": {
        "ok": true,
        "matched": "node2"
      }
    }
  ],
  "verification": {
    "width": 1080,
    "height": 2280,
    "size_bytes": 214563
  }
}`
	if got := jsonresult.Text(payload); got != want {
		t.Errorf("payload =\n%s\n\nwant\n%s", got, want)
	}
}

func TestWeChatHappyPathCallSequence(t *testing.T) {
	fake := newWeChatFake()
	if _, err := wechatTestRecipe(fake).send(context.Background(), "Alice", "hi"); err != nil {
		t.Fatalf("send: %v", err)
	}

	want := []string{
		"LaunchApp",   // com.tencent.mm
		"WaitForText", // home markers
		"FindAndTap",  // search entry point
		"WaitForText", // search input
		"Paste",       // the contact — paste, never type_text: contacts are Chinese
		"FindAndTap",  // the contact row
		"Paste",       // the message
		"FindAndTap",  // send
		"Screenshot",  // verification
	}
	if got := strings.Join(fake.methods(), ","); got != strings.Join(want, ",") {
		t.Errorf("call sequence =\n%v\nwant\n%v", fake.methods(), want)
	}
	// Only the fixed 0.5s settle before the verification screenshot.
	if len(fake.sleeps) != 1 || fake.sleeps[0] != 500*time.Millisecond {
		t.Errorf("sleeps = %v, want [500ms]", fake.sleeps)
	}
}

func TestWeChatSelectorsAndTimeouts(t *testing.T) {
	fake := newWeChatFake()
	if _, err := wechatTestRecipe(fake).send(context.Background(), "小明", "你好"); err != nil {
		t.Fatalf("send: %v", err)
	}

	var waits, finds []wechatCall
	for _, c := range fake.calls {
		switch c.Method {
		case "WaitForText":
			waits = append(waits, c)
		case "FindAndTap":
			finds = append(finds, c)
		}
	}

	// timeout_s is 15, so min(15,8)=8 for the home markers and min(15,6)=6 for
	// the search entry point; the rest are literals.
	if len(waits) != 2 {
		t.Fatalf("want 2 waits, got %d", len(waits))
	}
	if strings.Join(waits[0].Text, "|") != "微信|通讯录|发现|我|WeChat|Chats|Contacts" {
		t.Errorf("home markers = %v", waits[0].Text)
	}
	if waits[0].TimeoutS != 8 {
		t.Errorf("wait_wechat_ready timeout = %v, want 8", waits[0].TimeoutS)
	}
	if strings.Join(waits[1].Text, "|") != "搜索|Search" || waits[1].TimeoutS != 4 {
		t.Errorf("wait_search_input = %v / %v", waits[1].Text, waits[1].TimeoutS)
	}

	if len(finds) != 3 {
		t.Fatalf("want 3 find_and_tap calls, got %d", len(finds))
	}
	// The search entry point: bilingual text AND content_desc lists at once, plus
	// the substring resource-id, OR-ed together (require_all stays false).
	if strings.Join(finds[0].Text, "|") != "搜索|Search" {
		t.Errorf("open_search text = %v", finds[0].Text)
	}
	if strings.Join(finds[0].Desc, "|") != "搜索|Search" {
		t.Errorf("open_search content_desc = %v", finds[0].Desc)
	}
	if strings.Join(finds[0].RID, "|") != "menu_search" {
		t.Errorf("open_search resource_id = %v, want the substring menu_search", finds[0].RID)
	}
	if finds[0].TimeoutS != 6 {
		t.Errorf("open_search timeout = %v, want 6", finds[0].TimeoutS)
	}
	// The contact row: text only, full timeout.
	if strings.Join(finds[1].Text, "|") != "小明" || finds[1].Desc != nil || finds[1].RID != nil {
		t.Errorf("open_chat selector = %#v", finds[1])
	}
	if finds[1].TimeoutS != 15 {
		t.Errorf("open_chat timeout = %v, want 15", finds[1].TimeoutS)
	}
	// Send: bilingual text + content_desc, 3s.
	if strings.Join(finds[2].Text, "|") != "发送|Send" || strings.Join(finds[2].Desc, "|") != "发送|Send" {
		t.Errorf("tap_send selector = %#v", finds[2])
	}
	if finds[2].TimeoutS != 3 {
		t.Errorf("tap_send timeout = %v, want 3", finds[2].TimeoutS)
	}
}

func TestWeChatLaunchesTheRightPackageWithNoActivity(t *testing.T) {
	fake := newWeChatFake()
	if _, err := wechatTestRecipe(fake).send(context.Background(), "Alice", "hi"); err != nil {
		t.Fatalf("send: %v", err)
	}
	if fake.calls[0].Package != "com.tencent.mm" || fake.calls[0].Activity != "" {
		t.Errorf("launch = %q/%q, want com.tencent.mm with the launcher intent",
			fake.calls[0].Package, fake.calls[0].Activity)
	}
}

func TestWeChatPastesContactThenMessage(t *testing.T) {
	fake := newWeChatFake()
	if _, err := wechatTestRecipe(fake).send(context.Background(), "小明", "你好世界"); err != nil {
		t.Fatalf("send: %v", err)
	}
	var pasted []string
	for _, c := range fake.calls {
		if c.Method == "Paste" {
			pasted = append(pasted, c.Arg)
		}
	}
	if strings.Join(pasted, ",") != "小明,你好世界" {
		t.Errorf("pasted = %v, want [小明 你好世界]", pasted)
	}
}

func TestWeChatMessageLengthIsARuneCount(t *testing.T) {
	fake := newWeChatFake()
	payload, err := wechatTestRecipe(fake).send(context.Background(), "小明", "你好世界")
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	// Python's len() counts code points; len(string) in Go would report 12 bytes.
	if !strings.Contains(jsonresult.Text(payload), "\"length\": 4") {
		t.Errorf("type_message length is not a rune count:\n%s", jsonresult.Text(payload))
	}
}

// ---------------------------------------------------------------------------
// The skipped waits and the two key fallbacks
// ---------------------------------------------------------------------------

func TestWeChatSkipsTheHomeMarkerWaitOnTimeout(t *testing.T) {
	fake := newWeChatFake()
	fake.waitErrs = map[int]error{0: &adb.Error{Msg: "Element not found within 8s (text=['微信'])."}}

	payload, err := wechatTestRecipe(fake).send(context.Background(), "Alice", "hi")
	if err != nil {
		t.Fatalf("a missed home-marker wait must not fail the recipe: %v", err)
	}
	text := jsonresult.Text(payload)
	if !strings.Contains(text, "{\n      \"step\": \"wait_wechat_ready\",\n      \"skipped\": true\n    }") {
		t.Errorf("want a skipped wait_wechat_ready step:\n%s", text)
	}
	// No settle sleep for this one — only wait_search_input pays 0.5s.
	if len(fake.sleeps) != 1 {
		t.Errorf("sleeps = %v, want only the final settle", fake.sleeps)
	}
}

func TestWeChatSkippedSearchWaitSleepsFirst(t *testing.T) {
	fake := newWeChatFake()
	fake.waitErrs = map[int]error{1: &adb.Error{Msg: "Element not found within 4s (text=['搜索', 'Search'])."}}

	payload, err := wechatTestRecipe(fake).send(context.Background(), "Alice", "hi")
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if !strings.Contains(jsonresult.Text(payload), "\"step\": \"wait_search_input\",\n      \"skipped\": true") {
		t.Errorf("want a skipped wait_search_input step:\n%s", jsonresult.Text(payload))
	}
	if len(fake.sleeps) != 2 || fake.sleeps[0] != 500*time.Millisecond {
		t.Errorf("sleeps = %v, want a 500ms settle then the final 500ms", fake.sleeps)
	}
}

func TestWeChatOpenChatFallsBackToEnter(t *testing.T) {
	fake := newWeChatFake()
	fake.findErrs = map[int]error{1: &adb.Error{Msg: "Element not found within 15s (text=['Alice']). Last tree had 42 nodes."}}

	payload, err := wechatTestRecipe(fake).send(context.Background(), "Alice", "hi")
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	text := jsonresult.Text(payload)
	if !strings.Contains(text, "\"step\": \"open_chat_fallback\",\n      \"key\": \"enter\"") {
		t.Errorf("want the open_chat_fallback step:\n%s", text)
	}
	if strings.Contains(text, "\"step\": \"open_chat\"") {
		t.Error("open_chat and open_chat_fallback are mutually exclusive")
	}
	// key("enter"), then 0.8s for the chat to open, then the final 0.5s.
	if len(fake.sleeps) != 2 || fake.sleeps[0] != 800*time.Millisecond || fake.sleeps[1] != 500*time.Millisecond {
		t.Errorf("sleeps = %v, want [800ms 500ms]", fake.sleeps)
	}
	var keys []string
	for _, c := range fake.calls {
		if c.Method == "Key" {
			keys = append(keys, c.Arg)
		}
	}
	if strings.Join(keys, ",") != "enter" {
		t.Errorf("keys = %v, want exactly one enter", keys)
	}
}

func TestWeChatSendFallsBackToEnterWithoutSleeping(t *testing.T) {
	fake := newWeChatFake()
	fake.findErrs = map[int]error{2: &adb.Error{Msg: "Element not found within 3s (text=['发送', 'Send'])."}}

	payload, err := wechatTestRecipe(fake).send(context.Background(), "Alice", "hi")
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	text := jsonresult.Text(payload)
	if !strings.Contains(text, "\"step\": \"send_fallback\",\n      \"key\": \"enter\"") {
		t.Errorf("want the send_fallback step:\n%s", text)
	}
	if strings.Contains(text, "\"step\": \"tap_send\"") {
		t.Error("tap_send and send_fallback are mutually exclusive")
	}
	// Unlike open_chat_fallback there is no extra wait here.
	if len(fake.sleeps) != 1 || fake.sleeps[0] != 500*time.Millisecond {
		t.Errorf("sleeps = %v, want only the final settle", fake.sleeps)
	}
}

func TestWeChatFallbackKeyFailureAbortsTheRecipe(t *testing.T) {
	fake := newWeChatFake()
	fake.findErrs = map[int]error{1: &adb.Error{Msg: "Element not found within 15s (text=['Alice'])."}}
	fake.keyErr = &adb.Error{Msg: "plugin scrcpy stream is not running"}

	_, err := wechatTestRecipe(fake).send(context.Background(), "Alice", "hi")
	if err == nil {
		t.Fatal("want the key failure to abort")
	}
	// The key error is what gets rewritten, not the find error, and the five
	// completed steps are launch/wait_ready/open_search/wait_search/type_contact.
	if !strings.HasPrefix(err.Error(), "plugin scrcpy stream is not running. Steps completed: 5.") {
		t.Errorf("error = %q", err.Error())
	}
}

// ---------------------------------------------------------------------------
// Argument validation and the failure rewrite
// ---------------------------------------------------------------------------

func TestWeChatRejectsEmptyArguments(t *testing.T) {
	for _, tc := range []struct {
		name    string
		contact string
		message string
		want    string
	}{
		{"empty contact", "", "hi", "contact must not be empty"},
		{"blank contact", "   \t ", "hi", "contact must not be empty"},
		{"empty message", "Alice", "", "message must not be empty"},
		{"blank message", "Alice", " \n ", "message must not be empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newWeChatFake()
			_, err := wechatTestRecipe(fake).send(context.Background(), tc.contact, tc.message)
			if err == nil {
				t.Fatal("want a validation error")
			}
			// Validation happens before the try block, so the message is NOT
			// rewritten with "Steps completed".
			if err.Error() != tc.want {
				t.Errorf("error = %q, want %q", err.Error(), tc.want)
			}
			if len(fake.calls) != 0 {
				t.Errorf("validation must not touch the device: %v", fake.methods())
			}
		})
	}
}

func TestWeChatFailureRewritesTheMessage(t *testing.T) {
	fake := newWeChatFake()
	fake.findErrs = map[int]error{0: &adb.Error{Msg: "Element not found within 6s (text=['搜索', 'Search'], content_desc=['搜索', 'Search'], resource_id=['menu_search']). Last tree had 12 nodes."}}

	_, err := wechatTestRecipe(fake).send(context.Background(), "Alice", "hi")
	if err == nil {
		t.Fatal("want the open_search failure to surface")
	}
	// launch_wechat + wait_wechat_ready completed before open_search failed.
	// Note the doubled period: the Python interpolates f"{exc}. Steps completed:"
	// and the selector error already ends in one. Byte-for-byte on purpose.
	want := "Element not found within 6s (text=['搜索', 'Search'], content_desc=['搜索', 'Search'], resource_id=['menu_search']). Last tree had 12 nodes.. Steps completed: 2. Last screenshot: 1080x2280"
	if err.Error() != want {
		t.Errorf("error =\n%q\nwant\n%q", err.Error(), want)
	}
	if !adb.IsError(err) {
		t.Errorf("the rewritten error must stay an adb.Error, got %T", err)
	}
	// One screenshot, taken only for the failure report.
	if fake.shots != 1 {
		t.Errorf("screenshots = %d, want 1", fake.shots)
	}
}

func TestWeChatFailureReportsZerosWhenTheScreenshotAlsoFails(t *testing.T) {
	fake := newWeChatFake()
	fake.launchErr = &adb.Error{Msg: "adb shell monkey -p com.tencent.mm ... failed: device offline"}
	fake.shotErr = &adb.Error{Msg: "device offline"}

	_, err := wechatTestRecipe(fake).send(context.Background(), "Alice", "hi")
	if err == nil {
		t.Fatal("want the launch failure to surface")
	}
	want := "adb shell monkey -p com.tencent.mm ... failed: device offline. Steps completed: 0. Last screenshot: 0x0"
	if err.Error() != want {
		t.Errorf("error =\n%q\nwant\n%q", err.Error(), want)
	}
}

func TestWeChatNonAdbErrorsAreNotRewritten(t *testing.T) {
	// The Python catches only AdbError; an OSError propagates untouched and is
	// caught by server.py instead. Here that is anything which is not adb.Error,
	// context cancellation above all.
	fake := newWeChatFake()
	fake.launchErr = context.Canceled

	_, err := wechatTestRecipe(fake).send(context.Background(), "Alice", "hi")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want the raw context.Canceled", err)
	}
	if fake.shots != 0 {
		t.Error("no failure screenshot should be attempted for a non-adb error")
	}
}

func TestWeChatNonAdbErrorInAWaitAbortsInsteadOfSkipping(t *testing.T) {
	fake := newWeChatFake()
	fake.waitErrs = map[int]error{0: errors.New("out of memory")}

	_, err := wechatTestRecipe(fake).send(context.Background(), "Alice", "hi")
	if err == nil || err.Error() != "out of memory" {
		t.Fatalf("error = %v, want the raw non-adb error", err)
	}
}

func TestWeChatPasteFailureIsNotSwallowed(t *testing.T) {
	fake := newWeChatFake()
	fake.pasteErrs = map[int]error{0: &adb.Error{Msg: "adb shell cmd clipboard set-text ... failed: closed"}}

	_, err := wechatTestRecipe(fake).send(context.Background(), "Alice", "hi")
	if err == nil {
		t.Fatal("a failed contact paste must abort the recipe")
	}
	if !strings.Contains(err.Error(), "Steps completed: 4.") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestWeChatCancellationDuringASleepAborts(t *testing.T) {
	fake := newWeChatFake()
	ctx, cancel := context.WithCancel(context.Background())
	recipe := &wechatRecipe{driver: fake, timeoutS: wechatDefaultTimeoutS, sleep: wechatSleep}
	cancel()

	_, err := recipe.send(ctx, "Alice", "hi")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

// ---------------------------------------------------------------------------
// The tool: registration, the driver hook, and the unwired failure
// ---------------------------------------------------------------------------

func TestWeChatToolSurface(t *testing.T) {
	wechatWithDriver(t, nil)
	tools := wrListTools(t, "phone-recipes", registerPhoneRecipes)

	if len(tools) != 1 {
		t.Fatalf("registered %d tools, want 1", len(tools))
	}
	tool, ok := tools["phone_send_wechat"]
	if !ok {
		t.Fatal("phone_send_wechat was not registered")
	}
	if tool.Description != "High-level recipe: open WeChat, find a contact, and send a message." {
		t.Errorf("description = %q", tool.Description)
	}
	if tool.Annotations != nil {
		t.Errorf("phone_send_wechat must not declare annotations")
	}
	if len(tool.Meta) != 0 {
		t.Errorf("phone_send_wechat must not declare _meta, got %v", tool.Meta)
	}
	wrAssertSchema(t, "phone_send_wechat inputSchema", tool.InputSchema, `{
		"type": "object",
		"title": "phone_send_wechatArguments",
		"properties": {
			"contact": {"type": "string", "title": "Contact"},
			"message": {"type": "string", "title": "Message"}
		},
		"required": ["contact", "message"]
	}`)
	wrAssertSchema(t, "phone_send_wechat outputSchema", tool.OutputSchema, `{
		"type": "object",
		"title": "phone_send_wechatOutput",
		"properties": {"result": {"type": "string", "title": "Result"}},
		"required": ["result"]
	}`)
}

func TestWeChatDefaultDriverIsInstalledAtInit(t *testing.T) {
	// recipes_wiring.go binds the recipe to the device/uitree/input groups from
	// init(). Without it phone_send_wechat would register and then report itself
	// unavailable on every call — a silent regression, since nothing else fails.
	factory := wechatCurrentDriverFactory()
	if factory == nil {
		t.Fatal("no default WeChatDriver: recipes_wiring.go must SetWeChatDriver from init()")
	}
	// Building the driver touches no device: it only captures Env.
	driver, err := factory(&mcpserver.Env{Log: mcpserver.NewLogger(nil)})
	if err != nil {
		t.Fatalf("default driver factory: %v", err)
	}
	if _, ok := driver.(*wechatActionDriver); !ok {
		t.Errorf("default driver = %T, want *wechatActionDriver", driver)
	}
}

// wechatWithDriver installs a driver factory for the duration of one test and
// restores whatever was there before. The hook is package-global, so tests that
// touch it must not run in parallel.
func wechatWithDriver(t *testing.T, factory WeChatDriverFactory) {
	t.Helper()
	previous := wechatCurrentDriverFactory()
	SetWeChatDriver(factory)
	t.Cleanup(func() { SetWeChatDriver(previous) })
}

func TestWeChatToolReportsAMissingDriver(t *testing.T) {
	wechatWithDriver(t, nil)

	// No driver, so the handler fails before it can reach adb — this test never
	// touches the device.
	res, out := wechatCallTool(t, map[string]any{"contact": "Alice", "message": "hi"})
	if res.IsError {
		t.Error("phone_* tools report isError:false; failure lives in the payload")
	}
	want := "{\n  \"ok\": false,\n  \"error\": \"" + wechatUnavailableMessage + "\"\n}"
	if out != want {
		t.Errorf("payload =\n%s\nwant\n%s", out, want)
	}
}

func TestWeChatToolUsesTheRegisteredDriver(t *testing.T) {
	fake := newWeChatFake()
	wechatWithDriver(t, func(*mcpserver.Env) (WeChatDriver, error) { return fake, nil })

	res, out := wechatCallTool(t, map[string]any{"contact": "Alice", "message": "hi"})
	if res.IsError {
		t.Errorf("unexpected isError, payload: %s", out)
	}
	if !strings.Contains(out, `"recipe": "wechat_send_message"`) {
		t.Errorf("payload =\n%s", out)
	}
	// Shape A: the text block is the bare payload and structuredContent wraps the
	// same text under "result".
	structured, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structuredContent = %#v, want the {\"result\": ...} wrapper", res.StructuredContent)
	}
	if structured["result"] != out {
		t.Errorf("structuredContent.result does not match the text block")
	}
	if fake.shots != 1 {
		t.Errorf("screenshots = %d, want the single verification capture", fake.shots)
	}
}

func TestWeChatToolSurfacesAFactoryError(t *testing.T) {
	wechatWithDriver(t, func(*mcpserver.Env) (WeChatDriver, error) {
		return nil, &adb.Error{Msg: "adb not found. Install Android platform-tools, run scripts/install.sh, or set ADB_PATH."}
	})

	_, out := wechatCallTool(t, map[string]any{"contact": "Alice", "message": "hi"})
	if !strings.Contains(out, "adb not found") {
		t.Errorf("payload =\n%s", out)
	}
}

// wechatCallTool drives phone_send_wechat over a real client session and returns
// the result plus its text block.
func wechatCallTool(t *testing.T, args map[string]any) (*mcp.CallToolResult, string) {
	t.Helper()

	registry := mcpserver.NewRegistry()
	registry.Add(mcpserver.Registration{Name: "phone-recipes", Order: 1, Apply: registerPhoneRecipes})
	server, err := mcpserver.New(t.Context(), mcpserver.Options{Registry: registry})
	if err != nil {
		t.Fatalf("build server: %v", err)
	}

	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.MCP.Connect(t.Context(), serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "tools-test", Version: "0"}, nil)
	session, err := client.Connect(t.Context(), clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "phone_send_wechat", Arguments: args})
	if err != nil {
		t.Fatalf("tools/call: %v", err)
	}
	if len(res.Content) != 1 {
		t.Fatalf("want exactly one content block, got %d", len(res.Content))
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content[0] = %T, want text", res.Content[0])
	}
	return res, text.Text
}

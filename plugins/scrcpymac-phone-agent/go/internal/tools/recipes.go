package tools

// High-level recipes: phone_send_wechat.
//
// Port of server/phone_agent/recipes/wechat.py. The recipe is pure orchestration
// — it owns the step sequence, the fallbacks and the result shape, and nothing
// else. Every device interaction goes through WeChatDriver, which is the phone
// action layer (actions.PhoneActions in the Python). Reimplementing ui_tree
// parsing, selector matching or the input primitives here would fork behaviour
// that phone_find_and_tap / phone_wait_for_text / phone_paste already own.
//
// # Wiring the driver
//
// A driver is installed with SetWeChatDriver from an init(), so no shared file is
// edited and no group has to know about any other:
//
//	func init() {
//	    SetWeChatDriver(func(env *mcpserver.Env) (WeChatDriver, error) { ... })
//	}
//
// recipes_wiring.go does exactly that, binding the six primitives to the device,
// uitree and input groups; it is the only file that couples the recipe to their
// internals. All init() functions in this package run before mcpserver.New
// applies any registration, and the driver is resolved lazily on the first tool
// call, so the order two files register in does not matter. With no driver at
// all — which only happens if something calls SetWeChatDriver(nil), as the tests
// do — phone_send_wechat reports that it is unavailable and logs a warning at
// startup instead of failing to register.
//
// # What the recipe pins down about the driver
//
//   - WaitForText must accept a LIST of alternatives (7 bilingual home markers),
//     matching the `text` attribute only, never content_desc.
//   - FindAndTap must accept lists for text and content_desc simultaneously with
//     OR semantics across attributes (require_all=false), and a substring
//     resource_id ("menu_search" has to match "com.tencent.mm:id/menu_search").
//   - Key("enter") must work on both the adb and the H.264-stream backends.
//   - Screenshot must report width, height and size_bytes.
//   - Paste, not TypeText: contacts and messages are routinely Chinese.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zjywill/scrcpyMac/phone-agent/internal/adb"
	"github.com/zjywill/scrcpyMac/phone-agent/internal/jsonresult"
	"github.com/zjywill/scrcpyMac/phone-agent/internal/mcpserver"
)

func init() {
	mcpserver.Register(mcpserver.Registration{
		Name:  "phone-recipes",
		Order: mcpserver.OrderPhoneTools + 70,
		Apply: registerPhoneRecipes,
	})
}

// WeChat constants, verbatim from recipes/wechat.py.
const wechatPackage = "com.tencent.mm"

var (
	wechatSearchLabels = []string{"搜索", "Search"}
	wechatSendLabels   = []string{"发送", "Send"}
	wechatHomeMarkers  = []string{"微信", "通讯录", "发现", "我", "WeChat", "Chats", "Contacts"}
)

// wechatDefaultTimeoutS is send_message's `timeout_s: float = 15`. The MCP tool
// exposes no timeout parameter, so this is always the value in production; the
// tests shorten it.
const wechatDefaultTimeoutS = 15.0

// wechatUnavailableMessage is what phone_send_wechat reports when the action
// layer has not been wired in. Model-visible.
const wechatUnavailableMessage = "phone_send_wechat is unavailable: no phone action driver is registered in this build."

// WeChatScreenshot is the part of a screenshot the recipe reads. Those three
// fields are load-bearing: they become the "verification" object of the result.
type WeChatScreenshot struct {
	Width     int
	Height    int
	SizeBytes int
}

// WeChatSelector is one UI-node selector.
//
// The zero value matches the Python defaults (require_all=False, exact=False,
// index=0, scroll_to_find=0, verify=False); poll_interval_s is deliberately not
// exposed, the driver supplies its own 0.4s default.
type WeChatSelector struct {
	// Text, ContentDesc, ResourceID and ClassName are alternatives: a node
	// matching ANY entry of a populated list hits that attribute. Matching is by
	// substring unless Exact is set.
	Text        []string
	ContentDesc []string
	ResourceID  []string
	ClassName   []string
	// RequireAll demands every populated attribute hit (AND) instead of any (OR).
	RequireAll bool
	// Exact switches substring matching to equality.
	Exact bool
	// Index selects among the matches, not among the tree's nodes.
	Index int
	// TimeoutS bounds the polling.
	TimeoutS float64
	// ScrollToFind scrolls up to N times while the node is missing.
	ScrollToFind int
	// Verify asks the tap to confirm a screen change.
	Verify bool
}

// WeChatDriver is the phone action surface the recipe composes. Every method
// returns the same ordered payload the corresponding phone_* tool serialises, so
// the recipe can embed it in a step verbatim.
//
// Failures that the recipe treats as "this step did not work, fall back" must be
// adb errors (adb.Error, or anything wrapping one) — that is exactly the
// AdbError the Python catches. Any other error aborts the recipe, matching the
// Python, where only AdbError is caught and an OSError propagates.
type WeChatDriver interface {
	// LaunchApp starts a package; an empty activity uses the launcher intent.
	LaunchApp(ctx context.Context, pkg, activity string) (*jsonresult.Obj, error)
	// WaitForText waits until any alternative appears in the UI tree's text.
	WaitForText(ctx context.Context, alternatives []string, timeoutS float64) (*jsonresult.Obj, error)
	// FindAndTap polls for a matching node and taps its centre.
	FindAndTap(ctx context.Context, sel WeChatSelector) (*jsonresult.Obj, error)
	// Paste puts text on the device clipboard and pastes it.
	Paste(ctx context.Context, text string) (*jsonresult.Obj, error)
	// Key sends a named key: the recipe only uses "enter".
	Key(ctx context.Context, name string) (*jsonresult.Obj, error)
	// Screenshot captures the screen and reports its dimensions and size.
	Screenshot(ctx context.Context) (WeChatScreenshot, error)
}

// WeChatDriverFactory builds a driver from the server environment.
type WeChatDriverFactory func(env *mcpserver.Env) (WeChatDriver, error)

var (
	wechatDriverMu      sync.Mutex
	wechatDriverFactory WeChatDriverFactory
)

// SetWeChatDriver installs the phone action layer the WeChat recipe runs on.
// Call it from an init() in the file that owns the action layer. Passing nil
// clears it, which is only useful in tests.
func SetWeChatDriver(factory WeChatDriverFactory) {
	wechatDriverMu.Lock()
	defer wechatDriverMu.Unlock()
	wechatDriverFactory = factory
}

func wechatCurrentDriverFactory() WeChatDriverFactory {
	wechatDriverMu.Lock()
	defer wechatDriverMu.Unlock()
	return wechatDriverFactory
}

type wechatSendIn struct {
	Contact string `json:"contact"`
	Message string `json:"message"`
}

// wechatTool holds the lazily-resolved driver for one server.
type wechatTool struct {
	env    *mcpserver.Env
	mu     sync.Mutex
	driver WeChatDriver
}

// driverFor resolves the driver once per server. A failure is not cached: a
// factory that could not reach adb on the first call gets another chance.
func (t *wechatTool) driverFor() (WeChatDriver, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.driver != nil {
		return t.driver, nil
	}
	factory := wechatCurrentDriverFactory()
	if factory == nil {
		return nil, errors.New(wechatUnavailableMessage)
	}
	driver, err := factory(t.env)
	if err != nil {
		return nil, err
	}
	if driver == nil {
		return nil, errors.New(wechatUnavailableMessage)
	}
	t.driver = driver
	return driver, nil
}

func registerPhoneRecipes(s *mcp.Server, env *mcpserver.Env) error {
	tool := &wechatTool{env: env}
	if wechatCurrentDriverFactory() == nil {
		env.Log.Warn("phone_send_wechat has no phone action driver; the tool will report it is unavailable",
			"hook", "tools.SetWeChatDriver")
	}

	mcp.AddTool(s, &mcp.Tool{
		Name:        "phone_send_wechat",
		Description: "High-level recipe: open WeChat, find a contact, and send a message.",
		InputSchema: ObjectSchema("phone_send_wechatArguments",
			Prop{Name: "contact", Type: "string", Title: "Contact", Required: true},
			Prop{Name: "message", Type: "string", Title: "Message", Required: true},
		),
		OutputSchema: StringOutputSchema("phone_send_wechat"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in wechatSendIn) (*mcp.CallToolResult, StringOut, error) {
		driver, err := tool.driverFor()
		if err != nil {
			return JSONFail(err)
		}
		recipe := newWeChatRecipe(driver)
		return JSON(recipe.send(ctx, in.Contact, in.Message))
	})

	return nil
}

// wechatRecipe is one configured recipe. sleep is injectable so the step
// sequencing can be tested without burning the ~2.3s of fixed waits.
type wechatRecipe struct {
	driver   WeChatDriver
	timeoutS float64
	sleep    func(ctx context.Context, d time.Duration) error
}

func newWeChatRecipe(driver WeChatDriver) *wechatRecipe {
	return &wechatRecipe{
		driver:   driver,
		timeoutS: wechatDefaultTimeoutS,
		sleep:    wechatSleep,
	}
}

// wechatSleep is an interruptible time.Sleep. The Python's sleeps are
// unconditional; here a shutdown cancels them so a recipe cannot hold the
// process open.
func wechatSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// send is recipes.wechat.send_message.
//
// The argument checks are outside the try block in the Python, so their errors
// are NOT rewritten with the "Steps completed" suffix. Everything after them is.
func (r *wechatRecipe) send(ctx context.Context, contact, message string) (*jsonresult.Obj, error) {
	if strings.TrimSpace(contact) == "" {
		return nil, &adb.Error{Msg: "contact must not be empty"}
	}
	if strings.TrimSpace(message) == "" {
		return nil, &adb.Error{Msg: "message must not be empty"}
	}

	run := &wechatRun{recipe: r, steps: []*jsonresult.Obj{}}
	payload, err := run.exec(ctx, contact, message)
	if err == nil {
		return payload, nil
	}
	// Only AdbError is rewritten; anything else propagates untouched, exactly as
	// the Python's `except AdbError` does.
	if !adb.IsError(err) {
		return nil, err
	}
	shot := run.safeScreenshot(ctx)
	return nil, &adb.Error{Msg: fmt.Sprintf("%s. Steps completed: %d. Last screenshot: %dx%d",
		err.Error(), len(run.steps), shot.Width, shot.Height)}
}

// wechatRun is the mutable state of one recipe execution. steps has to survive
// the failure path: the rewritten error quotes how many completed.
type wechatRun struct {
	recipe *wechatRecipe
	steps  []*jsonresult.Obj
}

func (run *wechatRun) step(kv ...any) {
	run.steps = append(run.steps, jsonresult.Of(kv...))
}

func (run *wechatRun) exec(ctx context.Context, contact, message string) (*jsonresult.Obj, error) {
	r := run.recipe

	launch, err := r.driver.LaunchApp(ctx, wechatPackage, "")
	if err != nil {
		return nil, err
	}
	run.step("step", "launch_wechat", "result", launch)

	// One wait against all markers at once. A per-marker loop would give each
	// marker timeout/len(markers) seconds and a slow cold start would fail every
	// one of them, then proceed into the splash screen.
	if err := run.waitStep(ctx, "wait_wechat_ready", wechatHomeMarkers, math.Min(r.timeoutS, 8), 0); err != nil {
		return nil, err
	}

	// One selector call with bilingual alternatives plus WeChat's search
	// resource-id, instead of four sequential taps and a blind coordinate
	// fallback that could mis-tap on notched/tablet layouts.
	openSearch, err := r.driver.FindAndTap(ctx, WeChatSelector{
		Text:        wechatSearchLabels,
		ContentDesc: wechatSearchLabels,
		ResourceID:  []string{"menu_search"},
		TimeoutS:    math.Min(r.timeoutS, 6),
	})
	if err != nil {
		return nil, err
	}
	run.step("step", "open_search", "result", openSearch)

	// A missed search-input wait costs a fixed 0.5s instead, then continues.
	if err := run.waitStep(ctx, "wait_search_input", wechatSearchLabels, 4, 500*time.Millisecond); err != nil {
		return nil, err
	}

	if _, err := r.driver.Paste(ctx, contact); err != nil {
		return nil, err
	}
	run.step("step", "type_contact", "contact", contact)

	openChat, err := r.driver.FindAndTap(ctx, WeChatSelector{
		Text:     []string{contact},
		TimeoutS: r.timeoutS,
	})
	switch {
	case err == nil:
		run.step("step", "open_chat", "result", openChat)
	case !adb.IsError(err):
		return nil, err
	default:
		if _, kerr := r.driver.Key(ctx, "enter"); kerr != nil {
			return nil, kerr
		}
		run.step("step", "open_chat_fallback", "key", "enter")
		if serr := r.sleep(ctx, 800*time.Millisecond); serr != nil {
			return nil, serr
		}
	}

	if _, err := r.driver.Paste(ctx, message); err != nil {
		return nil, err
	}
	// length is Python's len(str): a rune count, not a byte count.
	run.step("step", "type_message", "length", jsonresult.RuneLen(message))

	tapSend, err := r.driver.FindAndTap(ctx, WeChatSelector{
		Text:        wechatSendLabels,
		ContentDesc: wechatSendLabels,
		TimeoutS:    3,
	})
	switch {
	case err == nil:
		run.step("step", "tap_send", "result", tapSend)
	case !adb.IsError(err):
		return nil, err
	default:
		if _, kerr := r.driver.Key(ctx, "enter"); kerr != nil {
			return nil, kerr
		}
		run.step("step", "send_fallback", "key", "enter")
	}

	if err := r.sleep(ctx, 500*time.Millisecond); err != nil {
		return nil, err
	}
	shot, err := r.driver.Screenshot(ctx)
	if err != nil {
		return nil, err
	}

	return jsonresult.Of(
		"ok", true,
		"recipe", "wechat_send_message",
		"contact", contact,
		"message", message,
		"steps", run.steps,
		"verification", jsonresult.Of(
			"width", shot.Width,
			"height", shot.Height,
			"size_bytes", shot.SizeBytes,
		),
	), nil
}

// waitStep runs one best-effort wait_for_text. An adb failure is swallowed and
// recorded as {"step": name, "skipped": true}, optionally after a settle sleep;
// any other error aborts the recipe.
func (run *wechatRun) waitStep(ctx context.Context, name string, alternatives []string, timeoutS float64, settle time.Duration) error {
	found, err := run.recipe.driver.WaitForText(ctx, alternatives, timeoutS)
	if err == nil {
		run.step("step", name, "result", found)
		return nil
	}
	if !adb.IsError(err) {
		return err
	}
	if settle > 0 {
		if serr := run.recipe.sleep(ctx, settle); serr != nil {
			return serr
		}
	}
	run.step("step", name, "skipped", true)
	return nil
}

// safeScreenshot is the failure-path screenshot: best effort, zeros when it does
// not work. The Python swallows only AdbError here; this swallows everything,
// because the caller is already reporting a failure and a second error would
// replace the useful message with a less useful one.
func (run *wechatRun) safeScreenshot(ctx context.Context) WeChatScreenshot {
	shot, err := run.recipe.driver.Screenshot(ctx)
	if err != nil {
		return WeChatScreenshot{}
	}
	return shot
}

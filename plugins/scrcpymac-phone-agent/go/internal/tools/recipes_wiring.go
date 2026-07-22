package tools

// The default WeChatDriver.
//
// recipes.go deliberately knows nothing about how a phone is driven; this file
// is the one place that binds the recipe's six primitives to the groups that
// already own them:
//
//	LaunchApp   -> device.go      (phone_launch_app)
//	WaitForText -> uitree.go      (phone_wait_for_text)
//	FindAndTap  -> uitree.go      (phone_find_and_tap)
//	Paste, Key  -> input.go       (phone_paste, phone_key)
//	Screenshot  -> input.go       (actions.screenshot / phone_screenshot)
//
// It is installed from init(), so it is in place before mcpserver.New applies
// any registration, and SetWeChatDriver still overrides it — tests do exactly
// that, and a future group that owns a better action layer can too.
//
// This file is the only coupling between the recipe and the other groups'
// internals. If one of them renames a method, the compiler points here and the
// fix is local: nothing in recipes.go or its tests depends on any of it.

import (
	"context"

	"github.com/zjywill/scrcpyMac/phone-agent/internal/jsonresult"
	"github.com/zjywill/scrcpyMac/phone-agent/internal/mcpserver"
)

func init() {
	SetWeChatDriver(newWeChatActionDriver)
}

// wechatPollIntervalS is find_and_tap/wait_for_text's poll_interval_s default.
// The recipe never overrides it, and neither does the Python.
const wechatPollIntervalS = 0.4

type wechatActionDriver struct {
	env   *mcpserver.Env
	input *inputActions
	tree  *uitreeGroup
}

// newWeChatActionDriver builds the driver for one server.
//
// The uitreeGroup it creates is a second instance, not the one phone_ui_tree
// registered — but the UI-tree cache is process-shared and the adb client comes
// from Env, so the recipe sees the same device, the same selected serial and the
// same cached tree the model's own tool calls do.
func newWeChatActionDriver(env *mcpserver.Env) (WeChatDriver, error) {
	return &wechatActionDriver{
		env:   env,
		input: newInputActions(env),
		tree:  newUITreeGroup(env),
	}, nil
}

func (d *wechatActionDriver) LaunchApp(ctx context.Context, pkg, activity string) (*jsonresult.Obj, error) {
	client, err := d.env.ADB()
	if err != nil {
		return nil, err
	}
	return deviceLaunchAppPayload(ctx, client, pkg, activity)
}

func (d *wechatActionDriver) WaitForText(ctx context.Context, alternatives []string, timeoutS float64) (*jsonresult.Obj, error) {
	return d.tree.waitForText(ctx, alternatives, timeoutS, wechatPollIntervalS)
}

func (d *wechatActionDriver) FindAndTap(ctx context.Context, sel WeChatSelector) (*jsonresult.Obj, error) {
	criteria, err := newUITreeCriteria(sel.Text, sel.ContentDesc, sel.ResourceID, sel.ClassName, sel.RequireAll, sel.Exact)
	if err != nil {
		return nil, err
	}
	return d.tree.findAndTap(ctx, criteria, sel.Index, sel.TimeoutS, wechatPollIntervalS, sel.ScrollToFind, sel.Verify)
}

func (d *wechatActionDriver) Paste(ctx context.Context, text string) (*jsonresult.Obj, error) {
	return d.input.paste(ctx, text)
}

func (d *wechatActionDriver) Key(ctx context.Context, name string) (*jsonresult.Obj, error) {
	return d.input.key(ctx, name)
}

func (d *wechatActionDriver) Screenshot(ctx context.Context) (WeChatScreenshot, error) {
	shot, err := d.input.screenshot(ctx)
	if err != nil {
		return WeChatScreenshot{}, err
	}
	return WeChatScreenshot{Width: shot.Width, Height: shot.Height, SizeBytes: shot.SizeBytes}, nil
}

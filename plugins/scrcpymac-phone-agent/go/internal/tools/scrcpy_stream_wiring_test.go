package tools

import (
	"context"
	"testing"

	"github.com/zjywill/scrcpyMac/phone-agent/internal/mcpserver"
	"github.com/zjywill/scrcpyMac/phone-agent/internal/scrcpy"
	"github.com/zjywill/scrcpyMac/phone-agent/internal/widget"
)

// TestStreamRegistrationInstallsEverySeam is the guard against the failure mode
// this file's registrar exists to prevent.
//
// Four tool groups were written in parallel, each declaring the slice of the
// scrcpy runtime it needs (StreamRuntime, InputRuntime, DeviceStreamStatusFunc)
// and defaulting to adb until something installs it. registerStreamTools is the
// only place that installs them. Dropping one line there is invisible: the build
// passes, every unit test passes, and the affected group silently keeps taking
// the adb path forever — phone_backend answering "adb" mid-stream, phone_tap
// going through `input tap` instead of the control socket.
func TestStreamRegistrationInstallsEverySeam(t *testing.T) {
	// The seams are process-global. Snapshot and restore them so the rest of the
	// package sees exactly what it saw before.
	t.Cleanup(func() {
		SetStreamRuntime(nil)
		SetInputRuntime(nil)
		SetDeviceStreamStatus(nil)
	})
	SetStreamRuntime(nil)
	SetInputRuntime(nil)
	SetDeviceStreamStatus(nil)

	if inputRuntime() != nil {
		t.Fatal("precondition: the input runtime should start unset")
	}
	if _, streaming := deviceStreamStatus(); streaming {
		t.Fatal("precondition: nothing should be streaming")
	}

	registry := mcpserver.NewRegistry()
	registry.Add(mcpserver.Registration{
		Name:  "scrcpy-stream",
		Order: mcpserver.OrderAppTools + 10,
		Apply: registerStreamTools,
	})
	// Deliberately no Env.Shutdown: the registrar registers Runtime.Close on the
	// PROCESS-WIDE runtime, and a closed runtime refuses to bind a loopback
	// listener ever again. Nothing was started, so nothing needs releasing.
	if _, err := mcpserver.New(context.Background(), mcpserver.Options{Registry: registry}); err != nil {
		t.Fatalf("New: %v", err)
	}

	if inputRuntime() == nil {
		t.Error("SetInputRuntime was not called: phone_tap/swipe/key/type/paste " +
			"will use adb even while the H.264 stream is running")
	}
	if ActiveStreamRuntime() == nil {
		t.Error("SetStreamRuntime was not called: the scrcpymac_ui_* tools lose the stream")
	}
	if deviceStreamFn == nil {
		t.Error("SetDeviceStreamStatus was not called: phone_backend will always " +
			"report \"adb\" and phone_device_info will never emit the video sub-object")
	}

	// With no stream up, every seam must report the adb path — the Python's
	// behaviour when nothing is streaming, and what the rest of the test suite
	// assumes.
	if rt := inputRuntime(); rt != nil {
		if st := rt.Status(); st.Streaming {
			t.Errorf("an idle runtime reports Streaming=true: %+v", st)
		}
		if rt.IsActive() {
			t.Error("an idle runtime reports IsActive=true")
		}
	}
	if _, streaming := deviceStreamStatus(); streaming {
		t.Error("an idle runtime reports a live device stream")
	}
	if got := deviceBackendName(); got != "adb" {
		t.Errorf("phone_backend backend = %q with no stream, want \"adb\"", got)
	}

	// The fourth seam: a listener bound after the widget resource was published
	// still reaches its CSP. Without the observer the connectDomains keep only
	// the wildcards, so the widget's stream URL is permitted by accident rather
	// than by declaration.
	// No observer is installed by hand here: this asserts the one the registrar
	// itself installed. A fresh runtime binding its listener must move the port
	// the widget CSP publishes.
	t.Cleanup(func() { widget.SetLoopbackPort(0) })
	widget.SetLoopbackPort(0)

	rt := scrcpy.New(scrcpy.Options{})
	t.Cleanup(func() { _ = rt.Close() })
	port, err := rt.EnsureLoopback()
	if err != nil {
		t.Fatalf("EnsureLoopback: %v", err)
	}
	if widget.LoopbackPort() != port {
		t.Errorf("widget CSP port = %d after the listener bound on %d; registerStreamTools "+
			"did not install the loopback observer, so a late bind never reaches the "+
			"widget's connectDomains", widget.LoopbackPort(), port)
	}
}

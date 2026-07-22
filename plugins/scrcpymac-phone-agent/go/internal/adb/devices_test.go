package adb

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

func TestParseDevices(t *testing.T) {
	tests := []struct {
		name   string
		stdout string
		want   []Device
	}{
		{
			name: "real output from the attached OnePlus 6",
			// Captured verbatim from `adb devices -l`, padding included.
			stdout: "List of devices attached\n" +
				"2f019965               device usb:1-1 product:OnePlus6 model:ONEPLUS_A6000 device:OnePlus6 transport_id:22\n" +
				"\n",
			want: []Device{{
				Serial: "2f019965", State: "device",
				Model: "ONEPLUS_A6000", Product: "OnePlus6",
				Codename: "OnePlus6", TransportID: "22",
			}},
		},
		{
			name:   "no devices attached",
			stdout: "List of devices attached\n\n",
			want:   []Device{},
		},
		{
			name:   "completely empty output",
			stdout: "",
			want:   []Device{},
		},
		{
			name: "offline and unauthorized carry no model or product",
			// adb prints the product/model/device triplet only once the device
			// is authorised, which is why those fields default to "".
			stdout: "List of devices attached\n" +
				"emulator-5554          offline\n" +
				"3f8a2b1c               unauthorized usb:1-2\n",
			want: []Device{
				{Serial: "emulator-5554", State: "offline"},
				{Serial: "3f8a2b1c", State: "unauthorized"},
			},
		},
		{
			name: "wifi serials keep their port",
			stdout: "List of devices attached\n" +
				"192.168.1.5:5555       device product:OnePlus6 model:ONEPLUS_A6000 device:OnePlus6 transport_id:4\n" +
				"192.168.1.7:39755      device\n",
			want: []Device{
				{
					Serial: "192.168.1.5:5555", State: "device",
					Model: "ONEPLUS_A6000", Product: "OnePlus6",
					Codename: "OnePlus6", TransportID: "4",
				},
				{Serial: "192.168.1.7:39755", State: "device"},
			},
		},
		{
			name: "daemon chatter before the header is skipped",
			// On adb 1.0.41 these go to stderr, but that has not always been
			// true; parsing them yields Device{serial:"*", state:"daemon"}.
			stdout: "* daemon not running; starting now at tcp:5037 *\n" +
				"* daemon started successfully *\n" +
				"List of devices attached\n" +
				"2f019965               device\n",
			want: []Device{{Serial: "2f019965", State: "device"}},
		},
		{
			name: "no permissions is one state, not the token 'no'",
			stdout: "List of devices attached\n" +
				"2f019965               no permissions; see [http://developer.android.com/tools/device.html] usb:1-1\n",
			want: []Device{{Serial: "2f019965", State: "no permissions"}},
		},
		{
			name: "no permissions with the plugdev hint",
			stdout: "List of devices attached\n" +
				"2f019965               no permissions (user in plugdev group; are your udev rules wrong?); see [http://x]\n",
			want: []Device{{Serial: "2f019965", State: "no permissions"}},
		},
		{
			name: "missing header still drops line 0, exactly like the Python",
			stdout: "2f019965               device\n" +
				"emulator-5554          device\n",
			want: []Device{{Serial: "emulator-5554", State: "device"}},
		},
		{
			name: "short lines, blank lines and CRLF",
			stdout: normalizeNewlines("List of devices attached\r\n" +
				"\r\n" +
				"lonely-token\r\n" +
				"   \r\n" +
				"2f019965\tdevice\ttransport_id:1\r\n"),
			want: []Device{{Serial: "2f019965", State: "device", TransportID: "1"}},
		},
		{
			name: "unknown tokens are discarded and order is preserved",
			stdout: "List of devices attached\n" +
				"AAA device usb:1-1 device:codename transport_id:9 model: product:P extra novalue\n" +
				"BBB bootloader\n",
			want: []Device{
				{Serial: "AAA", State: "device", Model: "", Product: "P", Codename: "codename", TransportID: "9"},
				{Serial: "BBB", State: "bootloader"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseDevices(tt.stdout)
			if got == nil {
				t.Fatal("ParseDevices must return an empty slice, not nil: nil marshals to null, Python emits []")
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ParseDevices()\n got %#v\nwant %#v", got, tt.want)
			}
		})
	}
}

func TestDeviceJSONKeysAreExactlyTheContract(t *testing.T) {
	// Device.to_dict() emitted serial, state, model, product — in that order,
	// nothing else, "" never null. The parsed device:/transport_id: tokens must
	// stay invisible on the wire.
	device := Device{
		Serial: "2f019965", State: "device", Model: "ONEPLUS_A6000", Product: "OnePlus6",
		Codename: "OnePlus6", TransportID: "22",
	}
	blob, err := json.Marshal(device)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"serial":"2f019965","state":"device","model":"ONEPLUS_A6000","product":"OnePlus6"}`
	if string(blob) != want {
		t.Errorf("Device JSON =\n %s\nwant %s", blob, want)
	}

	empty, err := json.Marshal(Device{Serial: "X", State: "offline"})
	if err != nil {
		t.Fatal(err)
	}
	if string(empty) != `{"serial":"X","state":"offline","model":"","product":""}` {
		t.Errorf("missing fields must be \"\", never null or absent: %s", empty)
	}
}

func TestListDevicesArgvIncludesSerial(t *testing.T) {
	// `adb -s X devices -l` is harmless (adb ignores -s for devices) but the
	// Python emits it whenever PHONE_AGENT_SERIAL is set, so the argv matches.
	fake := newRecorder(reply("List of devices attached\n2f019965 device\n"))
	client := NewWithRunner("2f019965", "/fake/adb", fake)

	devices, err := client.ListDevices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 1 || devices[0].Serial != "2f019965" {
		t.Fatalf("ListDevices = %#v", devices)
	}
	fake.wantArgv(t, 0, "/fake/adb", "-s", "2f019965", "devices", "-l")
}

func TestEnsureDeviceSelectsTheOnlyReadyDevice(t *testing.T) {
	fake := newRecorder(reply("List of devices attached\n" +
		"OFFLINE-1 offline\n" +
		"READY-1 device\n" +
		"UNAUTH-1 unauthorized\n"))
	client := NewWithRunner("", "/fake/adb", fake)

	serial, err := client.EnsureDevice(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if serial != "READY-1" {
		t.Errorf("EnsureDevice = %q, want READY-1", serial)
	}
	if client.Serial() != "READY-1" {
		t.Errorf("EnsureDevice must pin the client: Serial() = %q", client.Serial())
	}
}

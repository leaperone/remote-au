package audio

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestEncodeDeviceListsJSON(t *testing.T) {
	lists := DeviceLists{
		Playback: []DeviceInfo{
			{Index: 0, Name: "Built-in Output", ID: "play-0"},
			{Index: 1, Name: "HDMI", ID: "play-1", Note: "usable as WASAPI loopback source"},
		},
		Capture: []DeviceInfo{
			{Index: 0, Name: "Built-in Mic", ID: "cap-0"},
		},
		LoopbackNote: "loopback note",
	}

	var out bytes.Buffer
	if err := EncodeDeviceListsJSON(&out, lists); err != nil {
		t.Fatalf("EncodeDeviceListsJSON: %v", err)
	}

	if !strings.HasPrefix(out.String(), "{\n  \"playback\"") {
		t.Fatalf("json is not indented as expected: %q", out.String())
	}
	if strings.Contains(out.String(), "\"Note\"") || strings.Contains(out.String(), "\"Index\"") {
		t.Fatalf("json used Go field names: %s", out.String())
	}
	if strings.Contains(out.String(), "\"note\": \"\"") {
		t.Fatalf("empty note was not omitted: %s", out.String())
	}

	var decoded DeviceLists
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("encoded JSON did not parse: %v", err)
	}
	if decoded.Playback[1].Note != "usable as WASAPI loopback source" || decoded.LoopbackNote != "loopback note" {
		t.Fatalf("decoded=%+v, want fixture values", decoded)
	}
}

func TestSelectDeviceBySelector(t *testing.T) {
	devices := []DeviceInfo{
		{Index: 0, Name: "Built-in Output", ID: "play-0"},
		{Index: 1, Name: "USB Headset", ID: "play-1"},
		{Index: 2, Name: "USB Headset Chat", ID: "play-2"},
		{Index: 3, Name: "1 Monitor", ID: "play-3"},
	}

	tests := []struct {
		name     string
		selector string
		want     int
	}{
		{name: "index first", selector: "1", want: 1},
		{name: "case-insensitive exact", selector: "usb headset", want: 1},
		{name: "substring", selector: "built", want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := SelectDeviceBySelector(tt.selector, devices, "playback")
			if err != nil {
				t.Fatalf("SelectDeviceBySelector(%q): %v", tt.selector, err)
			}
			if got.Index != tt.want {
				t.Fatalf("Index=%d, want %d", got.Index, tt.want)
			}
		})
	}
}

func TestSelectDeviceBySelectorErrorsIncludeCandidates(t *testing.T) {
	devices := []DeviceInfo{
		{Index: 0, Name: "USB Headset", ID: "play-0"},
		{Index: 1, Name: "USB Headset Chat", ID: "play-1"},
	}

	if _, err := SelectDeviceBySelector("usb", devices, "playback"); err == nil || !strings.Contains(err.Error(), "ambiguous") || !strings.Contains(err.Error(), "[0] USB Headset") || !strings.Contains(err.Error(), "[1] USB Headset Chat") {
		t.Fatalf("ambiguous err=%v, want candidate list", err)
	}
	if _, err := SelectDeviceBySelector("missing", devices, "playback"); err == nil || !strings.Contains(err.Error(), "no playback device matching") || !strings.Contains(err.Error(), "[0] USB Headset") {
		t.Fatalf("missing err=%v, want candidate list", err)
	}
	if _, err := SelectDeviceBySelector("9", devices, "playback"); err == nil || !strings.Contains(err.Error(), "out of range") || !strings.Contains(err.Error(), "[1] USB Headset Chat") {
		t.Fatalf("out of range err=%v, want candidate list", err)
	}
	if _, err := SelectDeviceBySelector("-1", devices, "playback"); err == nil || !strings.Contains(err.Error(), "non-negative") {
		t.Fatalf("negative index err=%v, want non-negative error", err)
	}
}

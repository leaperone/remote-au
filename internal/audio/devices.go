package audio

import (
	"encoding/json"
	"fmt"
	"io"
	"runtime"
	"strconv"
	"strings"

	"github.com/gen2brain/malgo"

	"remote-au/internal/logging"
)

type DeviceInfo struct {
	Index int    `json:"index"`
	Name  string `json:"name"`
	ID    string `json:"id"`
	Note  string `json:"note,omitempty"`
}

type DeviceLists struct {
	Playback     []DeviceInfo `json:"playback"`
	Capture      []DeviceInfo `json:"capture"`
	LoopbackNote string       `json:"loopbackNote"`
}

func EnumerateDevices(verbose bool, logger logging.Logger) (DeviceLists, error) {
	ctx, err := initContext(verbose, logger)
	if err != nil {
		return DeviceLists{}, fmt.Errorf("init audio context: %w", err)
	}
	defer func() {
		_ = closeContext(ctx)
	}()

	playback, err := ctx.Devices(malgo.Playback)
	if err != nil {
		return DeviceLists{}, fmt.Errorf("enumerate playback devices: %w", err)
	}
	capture, err := ctx.Devices(malgo.Capture)
	if err != nil {
		return DeviceLists{}, fmt.Errorf("enumerate capture devices: %w", err)
	}

	return DeviceLists{
		Playback:     mapDeviceInfos(playback, playbackLoopbackNote()),
		Capture:      mapDeviceInfos(capture, ""),
		LoopbackNote: platformLoopbackNote(),
	}, nil
}

func PlaybackDeviceBySelector(selector string, verbose bool, logger logging.Logger) (*malgo.DeviceID, DeviceInfo, error) {
	return deviceBySelector(malgo.Playback, selector, verbose, logger)
}

func CaptureDeviceBySelector(selector string, source CaptureSource, verbose bool, logger logging.Logger) (*malgo.DeviceID, DeviceInfo, error) {
	if source == SourceLoopback {
		if runtime.GOOS != "windows" {
			return nil, DeviceInfo{}, fmt.Errorf("loopback device selection is only supported on Windows")
		}
		return deviceBySelector(malgo.Playback, selector, verbose, logger)
	}
	return deviceBySelector(malgo.Capture, selector, verbose, logger)
}

func PrintDevices(w io.Writer, verbose bool, logger logging.Logger) error {
	devices, err := EnumerateDevices(verbose, logger)
	if err != nil {
		return err
	}

	fmt.Fprintln(w, "Playback devices:")
	printDeviceList(w, devices.Playback)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Capture devices:")
	printDeviceList(w, devices.Capture)
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Loopback: %s\n", devices.LoopbackNote)

	return nil
}

func EncodeDeviceListsJSON(w io.Writer, lists DeviceLists) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(lists)
}

func EncodeDevicesJSON(w io.Writer, verbose bool, logger logging.Logger) error {
	devices, err := EnumerateDevices(verbose, logger)
	if err != nil {
		return err
	}
	return EncodeDeviceListsJSON(w, devices)
}

func deviceBySelector(kind malgo.DeviceType, selector string, verbose bool, logger logging.Logger) (*malgo.DeviceID, DeviceInfo, error) {
	devices, err := enumerateDeviceInfos(kind, verbose, logger)
	if err != nil {
		return nil, DeviceInfo{}, err
	}
	infos := mapDeviceInfos(devices, "")
	selected, err := SelectDeviceBySelector(selector, infos, deviceKindName(kind))
	if err != nil {
		return nil, DeviceInfo{}, err
	}

	id := devices[selected.Index].ID
	return &id, selected, nil
}

func enumerateDeviceInfos(kind malgo.DeviceType, verbose bool, logger logging.Logger) ([]malgo.DeviceInfo, error) {
	ctx, err := initContext(verbose, logger)
	if err != nil {
		return nil, fmt.Errorf("init audio context: %w", err)
	}
	defer func() {
		_ = closeContext(ctx)
	}()

	devices, err := ctx.Devices(kind)
	if err != nil {
		return nil, fmt.Errorf("enumerate %s devices: %w", deviceKindName(kind), err)
	}
	return devices, nil
}

func SelectDeviceBySelector(selector string, devices []DeviceInfo, kind string) (DeviceInfo, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return DeviceInfo{}, fmt.Errorf("%s device selector is empty", kind)
	}

	if index, err := strconv.Atoi(selector); err == nil {
		if index < 0 {
			return DeviceInfo{}, fmt.Errorf("%s device index must be non-negative: %d", kind, index)
		}
		for _, device := range devices {
			if device.Index == index {
				return device, nil
			}
		}
		return DeviceInfo{}, fmt.Errorf("%s device index %d out of range; available %s devices: %s", kind, index, kind, formatDeviceCandidates(devices))
	}

	if match, ok, err := selectNamedDevice(selector, devices, true); ok || err != nil {
		return match, err
	}
	if match, ok, err := selectNamedDevice(selector, devices, false); ok || err != nil {
		return match, err
	}

	return DeviceInfo{}, fmt.Errorf("no %s device matching %q; available %s devices: %s", kind, selector, kind, formatDeviceCandidates(devices))
}

func selectNamedDevice(selector string, devices []DeviceInfo, exact bool) (DeviceInfo, bool, error) {
	lowerSelector := strings.ToLower(selector)
	matches := make([]DeviceInfo, 0, 1)
	for _, device := range devices {
		name := strings.ToLower(device.Name)
		if (exact && name == lowerSelector) || (!exact && strings.Contains(name, lowerSelector)) {
			matches = append(matches, device)
		}
	}
	if len(matches) == 0 {
		return DeviceInfo{}, false, nil
	}
	if len(matches) == 1 {
		return matches[0], true, nil
	}
	return DeviceInfo{}, true, fmt.Errorf("device selector %q is ambiguous; matching devices: %s", selector, formatDeviceCandidates(matches))
}

func formatDeviceCandidates(devices []DeviceInfo) string {
	if len(devices) == 0 {
		return "(none)"
	}
	parts := make([]string, 0, len(devices))
	for _, device := range devices {
		parts = append(parts, fmt.Sprintf("[%d] %s", device.Index, device.Name))
	}
	return strings.Join(parts, "; ")
}

func mapDeviceInfos(infos []malgo.DeviceInfo, note string) []DeviceInfo {
	out := make([]DeviceInfo, 0, len(infos))
	for i := range infos {
		name := infos[i].Name()
		if name == "" {
			name = "(unnamed device)"
		}
		out = append(out, DeviceInfo{
			Index: i,
			Name:  name,
			ID:    infos[i].ID.String(),
			Note:  note,
		})
	}
	return out
}

func deviceKindName(kind malgo.DeviceType) string {
	switch kind {
	case malgo.Playback:
		return "playback"
	case malgo.Capture:
		return "capture"
	default:
		return "audio"
	}
}

func printDeviceList(w io.Writer, devices []DeviceInfo) {
	if len(devices) == 0 {
		fmt.Fprintln(w, "  (none)")
		return
	}
	for _, device := range devices {
		if device.Note == "" {
			fmt.Fprintf(w, "  [%d] %s\n", device.Index, device.Name)
			continue
		}
		fmt.Fprintf(w, "  [%d] %s - %s\n", device.Index, device.Name, device.Note)
	}
}

func playbackLoopbackNote() string {
	if runtime.GOOS == "windows" {
		return "usable as WASAPI loopback source"
	}
	return ""
}

func platformLoopbackNote() string {
	switch runtime.GOOS {
	case "windows":
		return "supported through WASAPI; choose a playback endpoint as the loopback source"
	case "darwin":
		return "not available through malgo/CoreAudio; use a virtual capture device for system audio"
	case "linux":
		return "use a PulseAudio/PipeWire monitor source as a capture device; malgo loopback is WASAPI-only"
	default:
		return "platform support depends on the native backend; WASAPI loopback is Windows-only"
	}
}

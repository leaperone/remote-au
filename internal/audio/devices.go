package audio

import (
	"fmt"
	"io"
	"runtime"

	"github.com/gen2brain/malgo"
)

type DeviceInfo struct {
	Index int
	Name  string
	ID    string
	Note  string
}

type DeviceLists struct {
	Playback     []DeviceInfo
	Capture      []DeviceInfo
	LoopbackNote string
}

func EnumerateDevices(verbose bool) (DeviceLists, error) {
	ctx, err := initContext(verbose)
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

func PrintDevices(w io.Writer, verbose bool) error {
	devices, err := EnumerateDevices(verbose)
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

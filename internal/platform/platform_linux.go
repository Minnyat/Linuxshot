package platform

import "linuxshot/internal/screenshot"

// New returns the Linux platform. Integrations are stubs that report
// ErrUnsupported until real implementations land; native region selection
// is unavailable (RegionSelector is nil).
func New(trayTooltip string) *Platform {
	return &Platform{
		Hotkeys:   stubHotkeys{},
		Tray:      stubTray{},
		Screen:    stubScreen{},
		Clipboard: stubClipboard{},
		Windows:   stubWindows{},
		Autostart: stubAutostart{},
	}
}

type stubHotkeys struct{}

func (stubHotkeys) SetCallback(cb func(id HotkeyID))               {}
func (stubHotkeys) Register(id HotkeyID, accelerator string) error { return ErrUnsupported }
func (stubHotkeys) UnregisterAll()                                 {}
func (stubHotkeys) Start()                                         {}
func (stubHotkeys) Stop()                                          {}

type stubTray struct{}

func (stubTray) SetCallback(cb func(id TrayMenuID)) {}
func (stubTray) SetOnShow(cb func())                {}
func (stubTray) Start() error                       { return ErrUnsupported }
func (stubTray) Stop() error                        { return nil }

type stubScreen struct{}

// MonitorAtCursor falls back to the primary display
func (stubScreen) MonitorAtCursor() int { return 0 }

func (stubScreen) CaptureWindow(handle uint64) (*screenshot.CaptureResult, error) {
	return nil, ErrUnsupported
}

type stubClipboard struct{}

func (stubClipboard) Image() (*screenshot.CaptureResult, error) { return nil, ErrUnsupported }

type stubWindows struct{}

func (stubWindows) List() ([]WindowInfo, error) { return nil, ErrUnsupported }

func (stubWindows) ListWithThumbnails(thumbWidth, thumbHeight int) ([]WindowInfoWithThumbnail, error) {
	return nil, ErrUnsupported
}

func (stubWindows) Info(handle uint64) (*WindowInfo, error) { return nil, ErrUnsupported }

type stubAutostart struct{}

func (stubAutostart) SetEnabled(enabled bool) error { return ErrUnsupported }
func (stubAutostart) SyncPath() error               { return ErrUnsupported }

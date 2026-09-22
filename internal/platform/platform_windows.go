package platform

import (
	"fmt"
	"image"

	"linuxshot/internal/config"
	"linuxshot/internal/hotkeys"
	"linuxshot/internal/overlay"
	"linuxshot/internal/screenshot"
	"linuxshot/internal/tray"
	winEnum "linuxshot/internal/windows"
)

// New returns the Win32-backed platform
func New(trayTooltip string) *Platform {
	return &Platform{
		Hotkeys:        &winHotkeys{m: hotkeys.NewHotkeyManager()},
		Tray:           &winTray{t: tray.NewTrayIcon(trayTooltip)},
		Screen:         winScreen{},
		Clipboard:      winClipboard{},
		Windows:        winWindows{},
		Autostart:      winAutostart{},
		RegionSelector: &winRegionSelector{m: overlay.NewManager()},
	}
}

// ==================== Hotkeys ====================

var hotkeyIDs = map[HotkeyID]int{
	HotkeyFullscreen: hotkeys.HotkeyFullscreen,
	HotkeyRegion:     hotkeys.HotkeyRegion,
	HotkeyWindow:     hotkeys.HotkeyWindow,
}

type winHotkeys struct{ m *hotkeys.HotkeyManager }

func (h *winHotkeys) SetCallback(cb func(id HotkeyID)) {
	h.m.SetCallback(func(winID int) {
		for id, w := range hotkeyIDs {
			if w == winID {
				cb(id)
				return
			}
		}
	})
}

func (h *winHotkeys) Register(id HotkeyID, accelerator string) error {
	mods, key, ok := hotkeys.ParseHotkeyString(accelerator)
	if !ok {
		return fmt.Errorf("invalid hotkey %q", accelerator)
	}
	return h.m.Register(hotkeyIDs[id], mods, key)
}

func (h *winHotkeys) UnregisterAll() { h.m.UnregisterAll() }
func (h *winHotkeys) Start()         { h.m.Start() }
func (h *winHotkeys) Stop()          { h.m.Stop() }

// ==================== Tray ====================

var trayMenuIDs = map[int]TrayMenuID{
	tray.MenuFullscreen: TrayFullscreen,
	tray.MenuRegion:     TrayRegion,
	tray.MenuWindow:     TrayWindow,
	tray.MenuLibrary:    TrayLibrary,
	tray.MenuQuit:       TrayQuit,
}

type winTray struct{ t *tray.TrayIcon }

func (w *winTray) SetCallback(cb func(id TrayMenuID)) {
	w.t.SetCallback(func(menuID int) {
		if id, ok := trayMenuIDs[menuID]; ok {
			cb(id)
		}
	})
}

func (w *winTray) SetOnShow(cb func()) { w.t.SetOnShow(cb) }
func (w *winTray) Start() error        { return w.t.Start() }
func (w *winTray) Stop() error         { return w.t.Stop() }

// ==================== Screen / Clipboard ====================

type winScreen struct{}

func (winScreen) MonitorAtCursor() int { return screenshot.GetMonitorAtCursor() }

func (winScreen) CaptureWindow(handle uint64) (*screenshot.CaptureResult, error) {
	return screenshot.CaptureWindowByCoords(uintptr(handle))
}

type winClipboard struct{}

func (winClipboard) Image() (*screenshot.CaptureResult, error) {
	return screenshot.GetClipboardImage()
}

// ==================== Window enumeration ====================

type winWindows struct{}

func (winWindows) List() ([]WindowInfo, error) {
	list, err := winEnum.EnumWindows()
	if err != nil {
		return nil, err
	}
	out := make([]WindowInfo, len(list))
	for i, w := range list {
		out[i] = toWindowInfo(w)
	}
	return out, nil
}

func (winWindows) ListWithThumbnails(thumbWidth, thumbHeight int) ([]WindowInfoWithThumbnail, error) {
	list, err := winEnum.EnumWindowsWithThumbnails(thumbWidth, thumbHeight)
	if err != nil {
		return nil, err
	}
	out := make([]WindowInfoWithThumbnail, len(list))
	for i, w := range list {
		out[i] = WindowInfoWithThumbnail{
			Handle:    uint64(w.Handle),
			Title:     w.Title,
			ClassName: w.ClassName,
			X:         w.X,
			Y:         w.Y,
			Width:     w.Width,
			Height:    w.Height,
			Thumbnail: w.Thumbnail,
		}
	}
	return out, nil
}

func (winWindows) Info(handle uint64) (*WindowInfo, error) {
	w, err := winEnum.GetWindowInfo(uintptr(handle))
	if err != nil || w == nil {
		return nil, err
	}
	info := toWindowInfo(*w)
	return &info, nil
}

func toWindowInfo(w winEnum.WindowInfo) WindowInfo {
	return WindowInfo{
		Handle:    uint64(w.Handle),
		Title:     w.Title,
		ClassName: w.ClassName,
		X:         w.X,
		Y:         w.Y,
		Width:     w.Width,
		Height:    w.Height,
	}
}

// ==================== Autostart ====================

type winAutostart struct{}

func (winAutostart) SetEnabled(enabled bool) error { return config.SetStartupEnabled(enabled) }
func (winAutostart) SyncPath() error               { return config.SyncStartupPath() }

// ==================== Native region selection ====================

type winRegionSelector struct{ m *overlay.Manager }

func (r *winRegionSelector) Start() error { return r.m.Start() }
func (r *winRegionSelector) Stop()        { r.m.Stop() }

func (r *winRegionSelector) Show(img *image.RGBA, bounds image.Rectangle, scaleRatio float64) <-chan RegionResult {
	src := r.m.Show(img, bounds, scaleRatio)
	out := make(chan RegionResult, 1)
	go func() {
		res := <-src
		out <- RegionResult{X: res.X, Y: res.Y, Width: res.Width, Height: res.Height, Cancelled: res.Cancelled}
	}()
	return out
}

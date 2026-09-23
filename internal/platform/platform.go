// Package platform abstracts OS-specific integrations (hotkeys, tray, window
// capture, clipboard, window enumeration, autostart) behind interfaces.
// New is implemented per OS in platform_<goos>.go.
package platform

import (
	"image"

	"linuxshot/internal/screenshot"
)

// Hotkeys registers global hotkeys from accelerator strings (e.g. "Ctrl+PrintScreen")
type Hotkeys interface {
	SetCallback(cb func(id HotkeyID))
	Register(id HotkeyID, accelerator string) error
	UnregisterAll()
	Start()
	Stop()
}

// Tray is the system tray icon and its menu
type Tray interface {
	SetCallback(cb func(id TrayMenuID))
	SetOnShow(cb func())
	Start() error
	Stop() error
}

// Screen provides capture helpers that need OS window-system access
type Screen interface {
	// MonitorAtCursor returns the display index under the cursor (0 if unknown)
	MonitorAtCursor() int
	CaptureWindow(handle uint64) (*screenshot.CaptureResult, error)
}

// Clipboard reads images from the system clipboard
type Clipboard interface {
	Image() (*screenshot.CaptureResult, error)
}

// WindowEnumerator lists top-level windows
type WindowEnumerator interface {
	List() ([]WindowInfo, error)
	ListWithThumbnails(thumbWidth, thumbHeight int) ([]WindowInfoWithThumbnail, error)
	Info(handle uint64) (*WindowInfo, error)
}

// Autostart manages launching the app on login
type Autostart interface {
	SetEnabled(enabled bool) error
	// SyncPath re-points an enabled autostart entry at the current executable
	SyncPath() error
}

// RegionSelector is an optional native region-selection overlay
type RegionSelector interface {
	Start() error
	Show(img *image.RGBA, bounds image.Rectangle, scaleRatio float64) <-chan RegionResult
	Stop()
}

// Platform bundles the OS integrations used by the app
type Platform struct {
	Hotkeys   Hotkeys
	Tray      Tray
	Screen    Screen
	Clipboard Clipboard
	Windows   WindowEnumerator
	Autostart Autostart
	// RegionSelector is nil when native region selection is unavailable
	RegionSelector RegionSelector
}

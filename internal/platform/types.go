package platform

import "errors"

// ErrUnsupported is returned by operations not yet implemented on this OS
var ErrUnsupported = errors.New("not supported on this platform")

// HotkeyID identifies a global hotkey action
type HotkeyID int

const (
	HotkeyFullscreen HotkeyID = iota + 1
	HotkeyRegion
	HotkeyWindow
)

// TrayMenuID identifies a tray menu action handled by the app
type TrayMenuID int

const (
	TrayFullscreen TrayMenuID = iota + 1
	TrayRegion
	TrayWindow
	TrayLibrary
	TrayQuit
)

// WindowInfo represents information about a top-level window.
// Handle is an opaque OS window ID (HWND on Windows, XID on X11).
type WindowInfo struct {
	Handle    uint64 `json:"handle"`
	Title     string `json:"title"`
	ClassName string `json:"className"`
	X         int    `json:"x"`
	Y         int    `json:"y"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
}

// WindowInfoWithThumbnail includes window info plus a thumbnail image
type WindowInfoWithThumbnail struct {
	Handle    uint64 `json:"handle"`
	Title     string `json:"title"`
	ClassName string `json:"className"`
	X         int    `json:"x"`
	Y         int    `json:"y"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	Thumbnail string `json:"thumbnail"` // Base64 encoded PNG thumbnail
}

// RegionResult is the outcome of a native region selection
type RegionResult struct {
	X, Y          int
	Width, Height int
	Cancelled     bool
}

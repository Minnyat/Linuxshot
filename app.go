package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"
	"golang.org/x/image/bmp"
	"golang.org/x/image/webp"
	"linuxshot/internal/config"
	"linuxshot/internal/library"
	"linuxshot/internal/platform"
	"linuxshot/internal/screenshot"
	"linuxshot/internal/updater"
	"linuxshot/internal/upload"
)

// Version is set at build time via ldflags
var Version = "dev"

// App struct
type App struct {
	ctx              context.Context
	platform         *platform.Platform
	config           *config.Config
	lastWidth        int
	lastHeight       int
	preCaptureWidth  int // Window size before capture (protected from resize events)
	preCaptureHeight int
	preCaptureX      int  // Window X position before capture
	preCaptureY      int  // Window Y position before capture
	isCapturing      bool // Flag to prevent resize events during capture
	isWindowHidden   bool // Track window visibility state
	trayRunning      bool // Tray icon started; required for close-to-tray

	// Cloud upload
	credManager    *upload.CredentialManager
	r2Uploader     *upload.R2Uploader
	gdriveUploader *upload.GDriveUploader
}

// NewApp creates a new App application struct
func NewApp() *App {
	return &App{platform: platform.New(fmt.Sprintf("LinuxShot v%s", Version))}
}

// startup is called when the app starts
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx

	// Load configuration
	cfg, err := config.Load()
	if err != nil {
		cfg = config.Default()
	}
	a.config = cfg

	// Initialize global hotkeys
	a.platform.Hotkeys.SetCallback(a.onHotkey)

	// Register hotkeys from config
	a.registerHotkeysFromConfig()
	a.platform.Hotkeys.Start()

	// Initialize native region selection overlay (optional capability)
	if a.platform.RegionSelector != nil {
		if err := a.platform.RegionSelector.Start(); err != nil {
			// Disable it so PrepareRegionCapture reports unsupported instead of waiting on a dead overlay
			println("Warning: failed to start overlay manager:", err.Error())
			a.platform.RegionSelector = nil
		}
	}

	// Initialize system tray (tooltip set in NewApp)
	a.platform.Tray.SetCallback(a.onTrayMenu)
	a.platform.Tray.SetOnShow(a.ShowWindow)
	if err := a.platform.Tray.Start(); err != nil {
		println("Warning: failed to start tray icon:", err.Error())
	} else {
		a.trayRunning = true
	}

	// Initialize window size tracking with config values
	a.lastWidth = cfg.Window.Width
	a.lastHeight = cfg.Window.Height

	// Track hidden state if app started minimized to tray (set via Wails StartHidden option)
	if cfg.Startup.MinimizeToTray {
		a.isWindowHidden = true
	}

	// Initialize cloud upload
	a.credManager = upload.NewCredentialManager()
	a.r2Uploader = upload.NewR2Uploader(a.credManager, &upload.R2Config{
		AccountID: a.config.Cloud.R2.AccountID,
		Bucket:    a.config.Cloud.R2.Bucket,
		PublicURL: a.config.Cloud.R2.PublicURL,
	})
	a.gdriveUploader = upload.NewGDriveUploader(a.credManager, &upload.GDriveConfig{
		FolderID: a.config.Cloud.GDrive.FolderID,
	})
}

// shutdown is called when the app is closing
func (a *App) shutdown(ctx context.Context) {
	// Save window size before closing using tracked values
	if a.config != nil && a.lastWidth >= 800 && a.lastHeight >= 600 {
		a.config.Window.Width = a.lastWidth
		a.config.Window.Height = a.lastHeight
		a.config.Save()
	}

	// Cleanup resources
	a.platform.Hotkeys.Stop()
	a.platform.Hotkeys.UnregisterAll()
	if a.platform.RegionSelector != nil {
		a.platform.RegionSelector.Stop()
	}
	a.platform.Tray.Stop()
}

// onHotkey handles global hotkey events
func (a *App) onHotkey(id platform.HotkeyID) {
	switch id {
	case platform.HotkeyFullscreen:
		runtime.EventsEmit(a.ctx, "hotkey:fullscreen")
	case platform.HotkeyRegion:
		runtime.EventsEmit(a.ctx, "hotkey:region")
	case platform.HotkeyWindow:
		runtime.EventsEmit(a.ctx, "hotkey:window")
	}
}

// onTrayMenu handles tray menu selections
func (a *App) onTrayMenu(menuID platform.TrayMenuID) {
	switch menuID {
	case platform.TrayFullscreen:
		runtime.EventsEmit(a.ctx, "hotkey:fullscreen")
	case platform.TrayRegion:
		runtime.EventsEmit(a.ctx, "hotkey:region")
	case platform.TrayWindow:
		runtime.EventsEmit(a.ctx, "hotkey:window")
	case platform.TrayLibrary:
		// Show main window first so library modal has context
		runtime.WindowShow(a.ctx)
		a.isWindowHidden = false
		// Emit event to open library window
		runtime.EventsEmit(a.ctx, "tray:library")
	case platform.TrayQuit:
		// Quit the application - use goroutine to avoid blocking tray menu
		go func() {
			// First try graceful shutdown via runtime.Quit
			// This may not work reliably on Windows from a goroutine
			runtime.Quit(a.ctx)

			// Fallback: force exit after a short delay if Quit didn't work
			time.Sleep(500 * time.Millisecond)
			os.Exit(0)
		}()
	}
}

// UpdateWindowSize tracks the current window size for persistence
func (a *App) UpdateWindowSize(width, height int) {
	// Skip updates during capture to preserve pre-capture size
	if a.isCapturing {
		return
	}
	if width >= 800 && height >= 600 {
		a.lastWidth = width
		a.lastHeight = height
	}
}

// MinimizeToTray hides the window (minimize to tray)
func (a *App) MinimizeToTray() {
	runtime.WindowHide(a.ctx)
	a.isWindowHidden = true
}

// OnBeforeClose is called when the window close button is clicked
// Returns true to prevent the default close behavior (if close-to-tray is enabled)
func (a *App) OnBeforeClose(ctx context.Context) bool {
	// Without a tray icon, hiding would leave no way to restore or quit the app
	if a.config != nil && a.config.Startup.CloseToTray && a.trayRunning {
		// Hide window instead of closing
		runtime.WindowHide(ctx)
		a.isWindowHidden = true
		return true // Prevent default close
	}
	return false // Allow normal close
}

// VirtualScreenBounds represents the combined bounds of all monitors
type VirtualScreenBounds struct {
	X      int `json:"x"` // Can be negative (monitor left of primary)
	Y      int `json:"y"` // Can be negative (monitor above primary)
	Width  int `json:"width"`
	Height int `json:"height"`
}

// RegionCaptureData holds the fullscreen screenshot and display info for region selection
type RegionCaptureData struct {
	Screenshot   *screenshot.CaptureResult `json:"screenshot"`
	ScreenX      int                       `json:"screenX"`
	ScreenY      int                       `json:"screenY"`
	Width        int                       `json:"width"`
	Height       int                       `json:"height"`
	ScaleRatio   float64                   `json:"scaleRatio"`   // DPI scale ratio (physical/logical)
	PhysicalW    int                       `json:"physicalW"`    // Actual screenshot width
	PhysicalH    int                       `json:"physicalH"`    // Actual screenshot height
	DisplayIndex int                       `json:"displayIndex"` // Index of the captured display
}

// PrepareRegionCapture prepares for region selection using the native overlay.
// Returns platform.ErrUnsupported when no native overlay is available (e.g. Linux).
func (a *App) PrepareRegionCapture() (*RegionCaptureData, error) {
	if a.platform.RegionSelector == nil {
		return nil, fmt.Errorf("region capture: %w", platform.ErrUnsupported)
	}

	// Set capturing flag to prevent resize events from overwriting saved size
	a.isCapturing = true

	// Save current window position before hiding
	a.preCaptureX, a.preCaptureY = runtime.WindowGetPosition(a.ctx)

	// Save current window size before hiding (protected from resize events)
	width, height := runtime.WindowGetSize(a.ctx)
	if width >= 800 && height >= 600 {
		a.preCaptureWidth = width
		a.preCaptureHeight = height
	} else if a.lastWidth >= 800 && a.lastHeight >= 600 {
		// Fallback to last tracked size
		a.preCaptureWidth = a.lastWidth
		a.preCaptureHeight = a.lastHeight
	} else {
		// Default fallback
		a.preCaptureWidth = 1200
		a.preCaptureHeight = 800
	}

	// Only hide and wait if window is currently visible
	if !a.isWindowHidden {
		runtime.WindowHide(a.ctx)
		a.isWindowHidden = true
		// Wait for window to fully hide (250ms for DWM compositor)
		time.Sleep(250 * time.Millisecond)
	}

	// Get the virtual screen bounds first
	screenX, screenY, virtualWidth, virtualHeight := screenshot.GetVirtualScreenBounds()

	// Capture raw RGBA (faster - no PNG encode)
	rgbaImg, err := screenshot.CaptureVirtualScreenRaw()
	if err != nil {
		runtime.WindowShow(a.ctx)
		a.isCapturing = false
		return nil, err
	}

	// Calculate scale ratio between physical screenshot and logical window size
	scaleRatio := float64(rgbaImg.Bounds().Dx()) / float64(virtualWidth)
	if scaleRatio < 1.0 {
		scaleRatio = 1.0
	}

	// Show native overlay and get result channel
	bounds := image.Rect(screenX, screenY, screenX+virtualWidth, screenY+virtualHeight)
	resultCh := a.platform.RegionSelector.Show(rgbaImg, bounds, scaleRatio)

	// Wait for selection result in goroutine
	go func() {
		selResult := <-resultCh
		if selResult.Cancelled {
			// User cancelled - just show window
			runtime.WindowShow(a.ctx)
			a.isWindowHidden = false
			a.isCapturing = false
			return
		}

		// Scale coordinates to physical pixels
		scaledX := int(float64(selResult.X) * scaleRatio)
		scaledY := int(float64(selResult.Y) * scaleRatio)
		scaledW := int(float64(selResult.Width) * scaleRatio)
		scaledH := int(float64(selResult.Height) * scaleRatio)

		// Crop to selected region before encoding (much faster - smaller image)
		croppedImg := rgbaImg.SubImage(image.Rect(scaledX, scaledY, scaledX+scaledW, scaledY+scaledH))

		var buf bytes.Buffer
		if err := png.Encode(&buf, croppedImg); err != nil {
			runtime.WindowShow(a.ctx)
			a.isWindowHidden = false
			a.isCapturing = false
			return
		}

		// Emit cropped image directly - no need for frontend to crop again
		runtime.EventsEmit(a.ctx, "region:selected", map[string]interface{}{
			"width":      scaledW,
			"height":     scaledH,
			"screenshot": base64.StdEncoding.EncodeToString(buf.Bytes()),
		})
	}()

	// Return minimal data (actual selection comes via event)
	return &RegionCaptureData{
		Screenshot:   nil, // Not needed - selection via event
		ScreenX:      screenX,
		ScreenY:      screenY,
		Width:        virtualWidth,
		Height:       virtualHeight,
		ScaleRatio:   scaleRatio,
		PhysicalW:    rgbaImg.Bounds().Dx(),
		PhysicalH:    rgbaImg.Bounds().Dy(),
		DisplayIndex: 0,
	}, nil
}

// imageToRGBA converts an image.Image to *image.RGBA
func imageToRGBA(img image.Image) *image.RGBA {
	if rgba, ok := img.(*image.RGBA); ok {
		return rgba
	}
	bounds := img.Bounds()
	rgba := image.NewRGBA(bounds)
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			rgba.Set(x, y, img.At(x, y))
		}
	}
	return rgba
}

// FinishRegionCapture restores the window to normal state after region selection
func (a *App) FinishRegionCapture() {
	// Simply show window and clear capturing flag
	// Native overlay doesn't modify main window, so just show it
	runtime.WindowShow(a.ctx)
	a.isWindowHidden = false
	a.isCapturing = false
}

// ShowWindow shows the main window
func (a *App) ShowWindow() {
	runtime.WindowShow(a.ctx)
	a.isWindowHidden = false
	runtime.WindowSetAlwaysOnTop(a.ctx, true)
	runtime.WindowSetAlwaysOnTop(a.ctx, false)
}

// CaptureFullscreen captures the display where the cursor is currently located
func (a *App) CaptureFullscreen() (*screenshot.CaptureResult, error) {
	return screenshot.CaptureDisplay(a.platform.Screen.MonitorAtCursor())
}

// CaptureRegion captures a specific region of the screen
func (a *App) CaptureRegion(x, y, width, height int) (*screenshot.CaptureResult, error) {
	return screenshot.CaptureRegion(x, y, width, height)
}

// CaptureDisplay captures a specific display by index
func (a *App) CaptureDisplay(displayIndex int) (*screenshot.CaptureResult, error) {
	return screenshot.CaptureDisplay(displayIndex)
}

// CaptureWindow captures a specific window by handle
func (a *App) CaptureWindow(hwnd int) (*screenshot.CaptureResult, error) {
	result, err := a.platform.Screen.CaptureWindow(uint64(hwnd))

	// Bring LinuxShot back to front after capture
	runtime.WindowShow(a.ctx)
	runtime.WindowSetAlwaysOnTop(a.ctx, true)
	time.Sleep(50 * time.Millisecond)
	runtime.WindowSetAlwaysOnTop(a.ctx, false)

	return result, err
}

// GetDisplayCount returns the number of active displays
func (a *App) GetDisplayCount() int {
	return screenshot.GetDisplayCount()
}

// GetActiveDisplayIndex returns the index of the display where the cursor is located
func (a *App) GetActiveDisplayIndex() int {
	return a.platform.Screen.MonitorAtCursor()
}

// GetVirtualScreenBounds returns the combined bounds of all monitors (virtual desktop)
func (a *App) GetVirtualScreenBounds() VirtualScreenBounds {
	x, y, w, h := screenshot.GetVirtualScreenBounds()
	return VirtualScreenBounds{X: x, Y: y, Width: w, Height: h}
}

// DisplayBounds represents the bounds of a display
type DisplayBounds struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

// GetDisplayBounds returns the bounds of a display
func (a *App) GetDisplayBounds(displayIndex int) DisplayBounds {
	bounds := screenshot.GetDisplayBounds(displayIndex)
	return DisplayBounds{
		X:      bounds.Min.X,
		Y:      bounds.Min.Y,
		Width:  bounds.Dx(),
		Height: bounds.Dy(),
	}
}

// GetWindowList returns a list of all visible windows
func (a *App) GetWindowList() ([]platform.WindowInfo, error) {
	return a.platform.Windows.List()
}

// GetWindowListWithThumbnails returns a list of all visible windows with thumbnails
func (a *App) GetWindowListWithThumbnails() ([]platform.WindowInfoWithThumbnail, error) {
	// Use 160x120 for thumbnails (4:3 aspect, good balance of quality and speed)
	return a.platform.Windows.ListWithThumbnails(160, 120)
}

// GetWindowInfo returns information about a specific window
func (a *App) GetWindowInfo(hwnd int) (*platform.WindowInfo, error) {
	return a.platform.Windows.Info(uint64(hwnd))
}

// SaveImageResult represents the result of saving an image
type SaveImageResult struct {
	Success  bool   `json:"success"`
	FilePath string `json:"filePath"`
	Error    string `json:"error,omitempty"`
}

// SaveImage saves a base64 encoded image to a file using a save dialog
func (a *App) SaveImage(imageData string, format string) SaveImageResult {
	// Determine file filter based on format
	var filters []runtime.FileFilter
	var defaultExt string

	switch strings.ToLower(format) {
	case "jpeg", "jpg":
		filters = []runtime.FileFilter{{DisplayName: "JPEG Image", Pattern: "*.jpg;*.jpeg"}}
		defaultExt = ".jpg"
	default:
		filters = []runtime.FileFilter{{DisplayName: "PNG Image", Pattern: "*.png"}}
		defaultExt = ".png"
	}

	// Show save dialog
	filePath, err := runtime.SaveFileDialog(a.ctx, runtime.SaveDialogOptions{
		Title:           "Save Screenshot",
		DefaultFilename: "screenshot" + defaultExt,
		Filters:         filters,
	})

	if err != nil {
		return SaveImageResult{Success: false, Error: err.Error()}
	}

	if filePath == "" {
		return SaveImageResult{Success: false, Error: "No file selected"}
	}

	// Ensure correct extension
	ext := filepath.Ext(filePath)
	if ext == "" {
		filePath += defaultExt
	}

	// Decode base64 data
	data, err := base64.StdEncoding.DecodeString(imageData)
	if err != nil {
		return SaveImageResult{Success: false, Error: "Failed to decode image data: " + err.Error()}
	}

	// Write to file
	err = os.WriteFile(filePath, data, 0644)
	if err != nil {
		return SaveImageResult{Success: false, Error: "Failed to save file: " + err.Error()}
	}

	return SaveImageResult{Success: true, FilePath: filePath}
}

// QuickSave saves a base64 encoded image to the configured directory
func (a *App) QuickSave(imageData string, format string) SaveImageResult {
	// Get save directory from config (fallback to default)
	saveDir := a.config.QuickSave.Folder
	if saveDir == "" {
		saveDir = config.DefaultSaveFolder()
	}

	// Create save directory if it doesn't exist
	err := os.MkdirAll(saveDir, 0755)
	if err != nil {
		return SaveImageResult{Success: false, Error: "Failed to create save directory: " + err.Error()}
	}

	// Determine file extension
	var ext string
	switch strings.ToLower(format) {
	case "jpeg", "jpg":
		ext = ".jpg"
	default:
		ext = ".png"
	}

	// Generate filename based on configured pattern
	var filename string
	now := time.Now()
	pattern := a.config.QuickSave.Pattern
	if pattern == "" {
		pattern = "timestamp"
	}

	switch pattern {
	case "date":
		// Date only: linuxshot_2024-01-15.png
		filename = "linuxshot_" + now.Format("2006-01-02") + ext
		// Avoid overwriting: append counter if file exists
		filePath := filepath.Join(saveDir, filename)
		if _, err := os.Stat(filePath); err == nil {
			counter := 1
			for {
				filename = "linuxshot_" + now.Format("2006-01-02") + "_" + fmt.Sprintf("%d", counter) + ext
				filePath = filepath.Join(saveDir, filename)
				if _, err := os.Stat(filePath); os.IsNotExist(err) {
					break
				}
				counter++
			}
		}
	case "increment":
		// Incremental: linuxshot_001.png, linuxshot_002.png
		counter := 1
		for {
			filename = fmt.Sprintf("linuxshot_%03d%s", counter, ext)
			filePath := filepath.Join(saveDir, filename)
			if _, err := os.Stat(filePath); os.IsNotExist(err) {
				break
			}
			counter++
		}
	default: // "timestamp"
		// Full timestamp: linuxshot_2024-01-15_14-30-45.png
		filename = "linuxshot_" + now.Format("2006-01-02_15-04-05") + ext
	}

	filePath := filepath.Join(saveDir, filename)

	// Decode and save
	data, err := base64.StdEncoding.DecodeString(imageData)
	if err != nil {
		return SaveImageResult{Success: false, Error: "Failed to decode image data: " + err.Error()}
	}

	err = os.WriteFile(filePath, data, 0644)
	if err != nil {
		return SaveImageResult{Success: false, Error: "Failed to save file: " + err.Error()}
	}

	return SaveImageResult{Success: true, FilePath: filePath}
}

// HotkeyConfig represents a hotkey configuration
type HotkeyConfig struct {
	Fullscreen string `json:"fullscreen"`
	Region     string `json:"region"`
	Window     string `json:"window"`
}

// GetHotkeyConfig returns the current hotkey configuration
func (a *App) GetHotkeyConfig() HotkeyConfig {
	return HotkeyConfig{
		Fullscreen: a.config.Hotkeys.Fullscreen,
		Region:     a.config.Hotkeys.Region,
		Window:     a.config.Hotkeys.Window,
	}
}

// GetConfig returns the current application configuration
func (a *App) GetConfig() *config.Config {
	return a.config
}

// SaveConfig saves the application configuration
func (a *App) SaveConfig(cfg *config.Config) error {
	// Update startup setting if changed. On failure keep the previous value so
	// the saved flag matches the OS state, but still save the other settings.
	var autostartErr error
	if cfg.Startup.LaunchOnStartup != a.config.Startup.LaunchOnStartup {
		if err := a.platform.Autostart.SetEnabled(cfg.Startup.LaunchOnStartup); err != nil {
			autostartErr = fmt.Errorf("settings saved, but launch on startup could not be changed: %w", err)
			cfg.Startup.LaunchOnStartup = a.config.Startup.LaunchOnStartup
		}
	}

	// Update hotkeys if changed
	hotkeysChanged := cfg.Hotkeys.Fullscreen != a.config.Hotkeys.Fullscreen ||
		cfg.Hotkeys.Region != a.config.Hotkeys.Region ||
		cfg.Hotkeys.Window != a.config.Hotkeys.Window

	// Store new config
	a.config = cfg

	// Save to disk
	if err := cfg.Save(); err != nil {
		return err
	}

	// Re-register hotkeys if they changed
	if hotkeysChanged {
		a.platform.Hotkeys.UnregisterAll()
		a.registerHotkeysFromConfig()
	}

	return autostartErr
}

// SelectFolder opens a folder selection dialog
func (a *App) SelectFolder() (string, error) {
	return runtime.OpenDirectoryDialog(a.ctx, runtime.OpenDialogOptions{
		Title: "Select Save Folder",
	})
}

// registerHotkeysFromConfig registers hotkeys based on current config
// Invalid or unsupported hotkeys are skipped, matching previous behavior.
func (a *App) registerHotkeysFromConfig() {
	a.platform.Hotkeys.Register(platform.HotkeyFullscreen, a.config.Hotkeys.Fullscreen)
	a.platform.Hotkeys.Register(platform.HotkeyRegion, a.config.Hotkeys.Region)
	a.platform.Hotkeys.Register(platform.HotkeyWindow, a.config.Hotkeys.Window)
}

// GetBackgroundImages returns the list of saved background images (base64 data URLs)
func (a *App) GetBackgroundImages() []string {
	if a.config == nil || a.config.BackgroundImages == nil {
		return []string{}
	}
	return a.config.BackgroundImages
}

// SaveBackgroundImages saves the list of background images to persistent config
func (a *App) SaveBackgroundImages(images []string) error {
	if a.config == nil {
		return nil
	}
	a.config.BackgroundImages = images
	return a.config.Save()
}

// OpenImage opens a file dialog to select an image and returns it as a CaptureResult
func (a *App) OpenImage() (*screenshot.CaptureResult, error) {
	// Show open file dialog
	filePath, err := runtime.OpenFileDialog(a.ctx, runtime.OpenDialogOptions{
		Title: "Open Image",
		Filters: []runtime.FileFilter{
			{DisplayName: "Image Files", Pattern: "*.png;*.jpg;*.jpeg;*.gif;*.bmp;*.webp"},
			{DisplayName: "PNG Images", Pattern: "*.png"},
			{DisplayName: "JPEG Images", Pattern: "*.jpg;*.jpeg"},
			{DisplayName: "GIF Images", Pattern: "*.gif"},
			{DisplayName: "BMP Images", Pattern: "*.bmp"},
			{DisplayName: "WebP Images", Pattern: "*.webp"},
		},
	})

	if err != nil {
		return nil, err
	}

	if filePath == "" {
		return nil, nil // User cancelled
	}

	// Read file
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}

	// Decode image to get dimensions
	var img image.Image
	ext := strings.ToLower(filepath.Ext(filePath))

	switch ext {
	case ".png":
		img, err = png.Decode(bytes.NewReader(data))
	case ".jpg", ".jpeg":
		img, err = jpeg.Decode(bytes.NewReader(data))
	case ".gif":
		img, err = gif.Decode(bytes.NewReader(data))
	case ".bmp":
		img, err = bmp.Decode(bytes.NewReader(data))
	case ".webp":
		img, err = webp.Decode(bytes.NewReader(data))
	default:
		// Try to decode as any supported format
		img, _, err = image.Decode(bytes.NewReader(data))
	}

	if err != nil {
		return nil, err
	}

	bounds := img.Bounds()

	// Re-encode as PNG for consistent handling in frontend
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}

	// Return as base64 encoded PNG
	return &screenshot.CaptureResult{
		Width:  bounds.Dx(),
		Height: bounds.Dy(),
		Data:   base64.StdEncoding.EncodeToString(buf.Bytes()),
	}, nil
}

// GetClipboardImage reads an image from the system clipboard
func (a *App) GetClipboardImage() (*screenshot.CaptureResult, error) {
	return a.platform.Clipboard.Image()
}

// CheckForUpdate checks GitHub for a newer version
func (a *App) CheckForUpdate(currentVersion string) (*updater.UpdateInfo, error) {
	return updater.CheckForUpdate(currentVersion)
}

// OpenURL opens a URL in the default browser
func (a *App) OpenURL(url string) {
	runtime.BrowserOpenURL(a.ctx, url)
}

// SetSkippedVersion sets a version to skip for update notifications
func (a *App) SetSkippedVersion(version string) error {
	a.config.Update.SkippedVersion = version
	return a.config.Save()
}

// GetSkippedVersion returns the version that user chose to skip
func (a *App) GetSkippedVersion() string {
	return a.config.Update.SkippedVersion
}

// GetEditorConfig returns the current editor panel settings
func (a *App) GetEditorConfig() *config.EditorConfig {
	return &a.config.Editor
}

// SaveEditorConfig saves editor panel settings to persistent config
func (a *App) SaveEditorConfig(editor *config.EditorConfig) error {
	if editor == nil {
		return nil
	}
	a.config.Editor = *editor
	return a.config.Save()
}

// ==================== Cloud Upload: R2 ====================

// SaveR2Config saves R2 configuration (non-sensitive data)
func (a *App) SaveR2Config(accountID, bucket, publicURL, directory string) error {
	a.config.Cloud.R2.AccountID = accountID
	a.config.Cloud.R2.Bucket = bucket
	a.config.Cloud.R2.PublicURL = publicURL
	a.config.Cloud.R2.Directory = directory

	// Update uploader config
	a.r2Uploader = upload.NewR2Uploader(a.credManager, &upload.R2Config{
		AccountID: accountID,
		Bucket:    bucket,
		PublicURL: publicURL,
		Directory: directory,
	})

	return a.config.Save()
}

// SaveR2Credentials saves R2 secrets to Windows Credential Manager
func (a *App) SaveR2Credentials(accessKeyID, secretAccessKey string) error {
	if err := a.credManager.Set(upload.CredR2AccessKeyID, accessKeyID); err != nil {
		return err
	}
	return a.credManager.Set(upload.CredR2SecretAccessKey, secretAccessKey)
}

// GetR2Config returns R2 configuration
func (a *App) GetR2Config() config.R2Config {
	return a.config.Cloud.R2
}

// IsR2Configured checks if R2 is fully configured
func (a *App) IsR2Configured() bool {
	return a.r2Uploader.IsConfigured()
}

// TestR2Connection tests R2 connectivity
func (a *App) TestR2Connection() error {
	return a.r2Uploader.TestConnection()
}

// UploadToR2 uploads image to Cloudflare R2
func (a *App) UploadToR2(imageData, filename string) (*upload.UploadResult, error) {
	data, err := base64.StdEncoding.DecodeString(imageData)
	if err != nil {
		return &upload.UploadResult{Success: false, Error: "invalid image data"}, err
	}
	return a.r2Uploader.Upload(context.Background(), data, filename)
}

// ClearR2Credentials removes R2 credentials from Windows Credential Manager
func (a *App) ClearR2Credentials() error {
	a.credManager.Delete(upload.CredR2AccessKeyID)
	a.credManager.Delete(upload.CredR2SecretAccessKey)
	return nil
}

// ==================== Cloud Upload: Google Drive ====================

// SaveGDriveCredentials saves Google OAuth client credentials to Credential Manager
func (a *App) SaveGDriveCredentials(clientID, clientSecret string) error {
	if err := a.credManager.Set(upload.CredGDriveClientID, clientID); err != nil {
		return err
	}
	return a.credManager.Set(upload.CredGDriveClientSecret, clientSecret)
}

// SaveGDriveConfig saves Google Drive configuration (non-sensitive)
func (a *App) SaveGDriveConfig(folderID string) error {
	a.config.Cloud.GDrive.FolderID = folderID

	// Update uploader config
	a.gdriveUploader = upload.NewGDriveUploader(a.credManager, &upload.GDriveConfig{
		FolderID: folderID,
	})

	return a.config.Save()
}

// GetGDriveConfig returns Google Drive configuration
func (a *App) GetGDriveConfig() config.GDriveConfig {
	return a.config.Cloud.GDrive
}

// HasGDriveCredentials checks if OAuth client credentials are configured
func (a *App) HasGDriveCredentials() bool {
	return a.credManager.Exists(upload.CredGDriveClientID) &&
		a.credManager.Exists(upload.CredGDriveClientSecret)
}

// StartGDriveAuth initiates OAuth flow and returns auth URL
func (a *App) StartGDriveAuth() (string, error) {
	url, err := a.gdriveUploader.StartAuth()
	if err != nil {
		return "", err
	}

	// Open browser
	runtime.BrowserOpenURL(a.ctx, url)

	// Wait for callback in background
	go func() {
		if err := a.gdriveUploader.WaitForAuth(); err != nil {
			runtime.EventsEmit(a.ctx, "gdrive:auth:error", err.Error())
		} else {
			runtime.EventsEmit(a.ctx, "gdrive:auth:success")
		}
	}()

	return url, nil
}

// GDriveStatus represents the Google Drive connection status
type GDriveStatus struct {
	Connected bool   `json:"connected"`
	Email     string `json:"email,omitempty"`
}

// IsGDriveConnected checks if Google Drive is connected and returns user email
func (a *App) IsGDriveConnected() (bool, string, error) {
	return a.gdriveUploader.IsConnected()
}

// GetGDriveStatus returns Google Drive connection status with email
func (a *App) GetGDriveStatus() (*GDriveStatus, error) {
	connected, email, err := a.gdriveUploader.IsConnected()
	if err != nil {
		return &GDriveStatus{Connected: false}, err
	}
	return &GDriveStatus{Connected: connected, Email: email}, nil
}

// DisconnectGDrive removes Google Drive authorization
func (a *App) DisconnectGDrive() error {
	return a.gdriveUploader.Disconnect()
}

// UploadToGDrive uploads image to Google Drive
func (a *App) UploadToGDrive(imageData, filename string) (*upload.UploadResult, error) {
	data, err := base64.StdEncoding.DecodeString(imageData)
	if err != nil {
		return &upload.UploadResult{Success: false, Error: "invalid image data"}, err
	}
	return a.gdriveUploader.Upload(context.Background(), data, filename)
}

// ClearGDriveCredentials removes all GDrive credentials from Windows Credential Manager
func (a *App) ClearGDriveCredentials() error {
	a.credManager.Delete(upload.CredGDriveClientID)
	a.credManager.Delete(upload.CredGDriveClientSecret)
	a.credManager.Delete(upload.CredGDriveToken)
	return nil
}

// ==================== Screenshot Library ====================

// GetLibraryImages returns all screenshots from QuickSave folder
func (a *App) GetLibraryImages() ([]library.LibraryImage, error) {
	folder := a.config.QuickSave.Folder
	if folder == "" {
		// Fallback to default location
		folder = config.DefaultSaveFolder()
	}

	opts := library.DefaultScanOptions()
	return library.ScanFolder(folder, opts)
}

// OpenInEditor loads an image file into the editor
// Security: validates path is within QuickSave folder
func (a *App) OpenInEditor(imagePath string) (*screenshot.CaptureResult, error) {
	// Validate path is within QuickSave folder (prevent directory traversal)
	folder := a.config.QuickSave.Folder
	if folder == "" {
		folder = config.DefaultSaveFolder()
	}

	absPath, err := filepath.Abs(imagePath)
	if err != nil {
		return nil, fmt.Errorf("invalid path: %w", err)
	}

	absFolder, err := filepath.Abs(folder)
	if err != nil {
		return nil, fmt.Errorf("invalid folder path: %w", err)
	}

	// Security check: ensure file is within QuickSave folder
	if !strings.HasPrefix(absPath, absFolder+string(filepath.Separator)) {
		return nil, fmt.Errorf("access denied: file outside QuickSave folder")
	}

	// Read file
	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	// Decode image
	var img image.Image
	ext := strings.ToLower(filepath.Ext(absPath))

	switch ext {
	case ".png":
		img, err = png.Decode(bytes.NewReader(data))
	case ".jpg", ".jpeg":
		img, err = jpeg.Decode(bytes.NewReader(data))
	default:
		img, _, err = image.Decode(bytes.NewReader(data))
	}
	if err != nil {
		return nil, fmt.Errorf("failed to decode image: %w", err)
	}

	bounds := img.Bounds()

	// Re-encode as PNG for consistent handling in frontend
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("failed to encode image: %w", err)
	}

	return &screenshot.CaptureResult{
		Width:  bounds.Dx(),
		Height: bounds.Dy(),
		Data:   base64.StdEncoding.EncodeToString(buf.Bytes()),
	}, nil
}

// DeleteScreenshot removes a screenshot file from disk
// Security: validates path is within QuickSave folder
func (a *App) DeleteScreenshot(imagePath string) error {
	// Validate path is within QuickSave folder (prevent directory traversal)
	folder := a.config.QuickSave.Folder
	if folder == "" {
		folder = config.DefaultSaveFolder()
	}

	absPath, err := filepath.Abs(imagePath)
	if err != nil {
		return fmt.Errorf("invalid path: %w", err)
	}

	absFolder, err := filepath.Abs(folder)
	if err != nil {
		return fmt.Errorf("invalid folder path: %w", err)
	}

	// Security check: ensure file is within QuickSave folder
	if !strings.HasPrefix(absPath, absFolder+string(filepath.Separator)) {
		return fmt.Errorf("access denied: file outside QuickSave folder")
	}

	return os.Remove(absPath)
}

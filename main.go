package main

import (
	"embed"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	wailsLinux "github.com/wailsapp/wails/v2/pkg/options/linux"
	wailsWindows "github.com/wailsapp/wails/v2/pkg/options/windows"
	"linuxshot/internal/config"
)

//go:embed all:frontend/dist
var assets embed.FS

// singleInstanceID identifies the app for Wails' single instance lock
const singleInstanceID = "io.github.minnyat.linuxshot"

func main() {
	// Load config to get saved window size and startup settings
	cfg, _ := config.Load()

	app := NewApp()

	// Sync autostart path if startup is enabled (fixes duplicate app issue #61)
	if cfg != nil && cfg.Startup.LaunchOnStartup {
		_ = app.platform.Autostart.SyncPath()
	}
	width := cfg.Window.Width
	height := cfg.Window.Height
	if width < 800 {
		width = 800
	}
	if height < 600 {
		height = 600
	}

	// Check if app should start hidden (minimize to tray)
	startHidden := cfg != nil && cfg.Startup.MinimizeToTray

	err := wails.Run(&options.App{
		Title:       "LinuxShot",
		Width:       width,
		Height:      height,
		MinWidth:    800,
		MinHeight:   600,
		Frameless:   true,
		StartHidden: startHidden,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		BackgroundColour: &options.RGBA{R: 27, G: 38, B: 54, A: 255},
		OnStartup:        app.startup,
		OnShutdown:       app.shutdown,
		OnBeforeClose:    app.OnBeforeClose,
		Bind: []interface{}{
			app,
		},
		// A second launch exits and brings the running instance to front
		SingleInstanceLock: &options.SingleInstanceLock{
			UniqueId: singleInstanceID,
			OnSecondInstanceLaunch: func(options.SecondInstanceData) {
				app.ShowWindow()
			},
		},
		Windows: &wailsWindows.Options{
			WebviewIsTransparent: false,
			WindowIsTranslucent:  false,
			Theme:                wailsWindows.Dark,
		},
		Linux: &wailsLinux.Options{
			ProgramName: "linuxshot",
		},
	})

	if err != nil {
		println("Error:", err.Error())
	}
}

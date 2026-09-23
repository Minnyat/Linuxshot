package main

import (
	"errors"
	"testing"

	"linuxshot/internal/config"
	"linuxshot/internal/platform"
)

type fakeAutostart struct{ err error }

func (f fakeAutostart) SetEnabled(enabled bool) error { return f.err }
func (f fakeAutostart) SyncPath() error               { return f.err }

// TestSaveConfig_AutostartUnsupported verifies other settings still save when
// autostart cannot be changed, and the flag keeps its previous value
func TestSaveConfig_AutostartUnsupported(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir) // Linux
	t.Setenv("AppData", dir)         // Windows

	app := NewApp()
	app.platform = &platform.Platform{Autostart: fakeAutostart{err: platform.ErrUnsupported}}
	app.config = config.Default()

	cfg := config.Default()
	cfg.Startup.LaunchOnStartup = true
	cfg.QuickSave.Pattern = "increment"

	err := app.SaveConfig(cfg)
	if !errors.Is(err, platform.ErrUnsupported) {
		t.Fatalf("SaveConfig() error = %v, want ErrUnsupported", err)
	}

	saved, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load() error = %v", err)
	}
	if saved.QuickSave.Pattern != "increment" {
		t.Errorf("QuickSave.Pattern = %q, want other settings saved", saved.QuickSave.Pattern)
	}
	if saved.Startup.LaunchOnStartup {
		t.Error("LaunchOnStartup saved as true, want previous value false")
	}
	if app.config.Startup.LaunchOnStartup {
		t.Error("in-memory LaunchOnStartup = true, want previous value false")
	}
}

// TestSaveConfig_AutostartEnabled verifies a successful autostart change is saved
func TestSaveConfig_AutostartEnabled(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("AppData", dir)

	app := NewApp()
	app.platform = &platform.Platform{Autostart: fakeAutostart{}}
	app.config = config.Default()

	cfg := config.Default()
	cfg.Startup.LaunchOnStartup = true
	if err := app.SaveConfig(cfg); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}

	saved, _ := config.Load()
	if !saved.Startup.LaunchOnStartup {
		t.Error("LaunchOnStartup not saved")
	}
}

# Linux Port (WinShot → LinuxShot)

## Phase 1 status

The Go backend builds, vets and tests on Linux. OS integrations sit behind `internal/platform`:

| Interface | Windows (`platform_windows.go`) | Linux (`platform_linux.go`) |
|-----------|--------------------------------|-----------------------------|
| `Hotkeys` | `internal/hotkeys` (RegisterHotKey) | stub, `ErrUnsupported` |
| `Tray` | `internal/tray` (Shell_NotifyIcon) | stub, `ErrUnsupported` |
| `Screen` (monitor at cursor, window capture) | `internal/screenshot/window.go` | primary display / `ErrUnsupported` |
| `Clipboard` | `internal/screenshot/clipboard.go` | stub, `ErrUnsupported` |
| `WindowEnumerator` | `internal/windows` | stub, `ErrUnsupported` |
| `Autostart` | `internal/config/startup.go` (registry) | stub, `ErrUnsupported` |
| `RegionSelector` (optional) | `internal/overlay` | `nil` → region capture unsupported |

- Win32 packages are unchanged apart from `//go:build windows`; `app.go` uses only the interfaces.
- Screen/region/display capture (`internal/screenshot/capture.go`) stays cross-platform (kbinani/screenshot).
- Single instance: Wails `SingleInstanceLock` (`io.github.minnyat.linuxshot`) on both OSes.
- Config: `os.UserConfigDir()/linuxshot/config.json`. Default QuickSave folder: `<XDG Pictures>/LinuxShot`.
- Credentials still use `wincred` (unsupported on Linux); the `WinShot_` key prefix is unchanged for now.

## Checks (same as CI, `.github/workflows/build-linux.yml`)

```bash
sudo apt-get install -y libgtk-3-dev libwebkit2gtk-4.1-dev
(cd frontend && npm install && npm run build)   # frontend/dist is embedded by package main
go vet ./...
GOOS=windows go vet -unsafeptr=false ./...       # keep Windows tags compiling
go test ./...
go install github.com/wailsapp/wails/v2/cmd/wails@v2.11.0
wails build -clean -platform linux/amd64 -tags webkit2_41 -ldflags "-X main.Version=<version>"
tar -czf build/bin/linuxshot-linux-amd64.tar.gz -C build/bin linuxshot
```

`-unsafeptr=false`: the existing Win32 clipboard code does pointer arithmetic on `GlobalLock` memory, which vet flags.

## Local build notes

- With Go ≥ 1.26 installed locally, prefix wails commands with `GOTOOLCHAIN=go1.24.11`
  (e.g. `GOTOOLCHAIN=go1.24.11 wails build -platform linux/amd64 -tags webkit2_41`).
  The wails v2.11.0 CLI bundles golang.org/x/tools v0.30, which cannot read newer Go export data
  (`internal error: package "encoding/json" without types ...`). CI uses Go 1.24 from `go.mod`, so it is unaffected.
- `wails doctor` reports `libwebkit: Not Found`: it probes webkit2gtk-4.0, but we build against 4.1 (`-tags webkit2_41`). Ignore it when `pkg-config --modversion webkit2gtk-4.1` succeeds.

## Known issues (Phase 2)

- A second launch exits (Wails single-instance lock over D-Bus), but GNOME focus-stealing prevention stops it from focusing the running window.
- Wails runs `OnStartup` alongside the lock check, so a second instance briefly runs startup before exiting. This is harmless with stubs, but matters once real hotkeys/tray exist.
- With no tray, `StartHidden`/minimize-to-tray still hides the window, and nothing reliably brings it back. Close-to-tray is already skipped when the tray is not running.

## Next

- Phase 2–3: real Linux hotkeys, tray, window capture/enumeration, clipboard, keyring, React region overlay.
- Phase 4: AppImage/.deb packaging (the updater already prefers these assets), frontend refresh.
- Publishing is disabled until Phase 4: pushes to `main`/`dev` only run the build check (tarball as a workflow artifact); no semantic-release, tag or GitHub release.

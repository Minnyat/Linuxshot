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

## Checks

```bash
(cd frontend && npm install && npm run build)   # frontend/dist is embedded by package main
go vet ./... && go test ./...
GOOS=windows go vet -unsafeptr=false ./...       # keep Windows tags compiling
wails build -platform linux/amd64 -tags webkit2_41   # needs libgtk-3-dev libwebkit2gtk-4.1-dev
```

`-unsafeptr=false`: the existing Win32 clipboard code does pointer arithmetic on `GlobalLock` memory, which vet flags.

## Next

- Phase 2–3: real Linux hotkeys, tray, window capture/enumeration, clipboard, keyring, React region overlay.
- Phase 4: AppImage/.deb packaging (the updater already prefers these assets), frontend refresh.

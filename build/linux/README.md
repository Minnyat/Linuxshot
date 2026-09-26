# Linux build files

## `io.github.minnyat.linuxshot.desktop`

The desktop entry every Linux channel must install. It is not decoration: on a
GNOME session it is what gives the app an identity that gnome-shell and
xdg-desktop-portal agree on, which is what the screen-capture permission is
recorded against.

Install it as, exactly:

```
$XDG_DATA_DIR/applications/io.github.minnyat.linuxshot.desktop
```

and install the icon under the same id, so `Icon=io.github.minnyat.linuxshot`
resolves:

```
$XDG_DATA_DIR/icons/hicolor/<size>x<size>/apps/io.github.minnyat.linuxshot.png
```

`Exec=linuxshot` assumes the binary is on `PATH` (`/usr/bin/linuxshot` for a
distro package). A channel that installs elsewhere - an AppImage, or a tarball
under `/opt` - must rewrite `Exec=` to the absolute path it actually uses, and
change nothing else.

### The trap: never rename the file

Every channel must ship this one file id. gnome-shell's `shell-window-tracker`
prefers a `StartupWMClass` match, but it also falls back to matching a desktop
file named `<wmclass>.desktop`. So a channel that ships the same content as
`linuxshot.desktop` looks fine - the window still maps to a desktop file - while
the portal records the permission against app id `linuxshot` rather than
`io.github.minnyat.linuxshot`. Consequences:

- a user who installs a second channel is prompted for screen-capture
  permission a second time,
- GNOME Settings' app permission list shows two entries for one app,
- revoking one of them leaves the other granted.

### The other line that must not drift: `StartupWMClass=linuxshot`

Exact, lowercase. Measured on this project's GNOME 46.2 host: the Wails window
carries `WM_CLASS = "linuxshot", "Linuxshot"` and no `_GTK_APPLICATION_ID`, and
the shell matches `StartupWMClass` against the WM_CLASS *instance* name first
and case-sensitively. The instance name comes from `g_set_prgname("linuxshot")`,
which Wails sets from the `Linux.ProgramName` option in `main.go` - so if that
option ever changes, this line has to change with it.

Without the match the shell cannot tell the window belongs to this desktop
file, the portal's focused-app check does not see us as focused, a first
non-interactive `Screenshot` request is refused, and a grant that is recorded
belongs to whichever app the shell does consider focused.

The file id also has to stay equal to `singleInstanceID` in `main.go`.

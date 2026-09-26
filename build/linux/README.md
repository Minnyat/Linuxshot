# Linux build files

## `io.github.minnyat.linuxshot.desktop`

The desktop entry every Linux channel must install. It is not decoration: on a
GNOME session it is half of what gives the app an identity that gnome-shell and
xdg-desktop-portal agree on, and that identity is what the screen-capture
permission is recorded against.

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

### How the identity actually works

Verified on this host (gnome-shell 46.0, xdg-desktop-portal 1.18.4,
xdg-desktop-portal-gnome 46.2):

- **xdg-desktop-portal** derives *our* app id from the systemd user unit the
  process runs in (`sd_pid_get_user_unit`; the unit must start with `app-`, and
  `app-<launcher>-<AppID>-<rand>.scope` is parsed for the id). Launching through
  this entry puts us in `app-gnome-io.github.minnyat.linuxshot-<pid>.scope`, so
  **the app id follows the name of the file we were launched from.**
- **gnome-shell** derives only the *focused* app, from the focused window, by
  looking the window's `WM_CLASS` up among installed entries' `StartupWMClass`
  values, then compares the two ids (`accessDialog.js`).
- Both must agree, so the entry helps only when the process is **launched
  through it** - by the shell, `gio launch`, `gtk-launch`, or an autostart entry
  with the same basename. Started from a terminal the gate fails even with the
  entry installed and our own window focused, because the portal then reports
  the terminal's identity (or none) while the focused app is us.

### Trap 1: never rename the file

Every channel must ship this one file id. A renamed `linuxshot.desktop` with the
same content still maps the window (its `StartupWMClass` is unchanged), but the
launch unit becomes `app-gnome-linuxshot-<pid>.scope`, so the portal app id
becomes `linuxshot`. Consequences:

- a user who installs a second channel is prompted for screen-capture
  permission a second time,
- the permission store ends up with two entries for one app, and revoking one
  leaves the other granted.

### Trap 2: never ship a second entry with `StartupWMClass=linuxshot`

This one is worse, and it is why the rename matters even if only one channel
misbehaves. When several entries declare the same `StartupWMClass`, gnome-shell
prefers the one whose own id equals it (`startup_wm_class_is_exact_match`,
`src/shell-app-system.c`), regardless of scan order. So with both
`io.github.minnyat.linuxshot.desktop` and `linuxshot.desktop` installed, the
shell maps our window to `linuxshot.desktop` - and an instance launched from the
*correct* entry then has portal app id `io.github.minnyat.linuxshot` while the
shell calls the focused window `linuxshot`. The ids disagree and the request is
refused outright.

### `StartupWMClass=linuxshot` must stay exactly that

Lowercase, no quotes. Our window carries `WM_CLASS = "linuxshot", "Linuxshot"`
and no `_GTK_APPLICATION_ID`; the instance name comes from
`g_set_prgname("linuxshot")`, which Wails sets from the `Linux.ProgramName`
option in `main.go`. The shell's lookup table is keyed by plain string equality,
so the match is case-sensitive and it is the lowercase instance part that
matches. If `Linux.ProgramName` ever changes, this line changes with it.

### Autostart uses the same basename

When "launch on startup" is implemented for Linux, the autostart entry must be
`~/.config/autostart/io.github.minnyat.linuxshot.desktop`. gnome-session launches
autostart entries as `app-<AppID>-autostart.service`, so a differently named file
puts us in a differently named unit and the portal app id disagrees again -
exactly the trap above, with a different launcher.

### There is no Settings UI to revoke this permission

For an app like ours (not Flatpak, not Snap) gnome-control-center derives no
portal app id - it only looks at `X-Flatpak` and `X-SnapInstanceName` - and hides
the Screenshots row, even though the portal's own dialog tells the user the
permission can be changed in the privacy settings. A Deny is permanent until the
permission store is edited directly: the `PermissionStore` D-Bus interface
(`org.freedesktop.impl.portal.PermissionStore` at
`/org/freedesktop/impl/portal/PermissionStore`, `Delete` or `SetPermission` on
table `screenshot`), or `~/.local/share/flatpak/db/screenshot`, which exists even
on hosts where flatpak is not installed and `flatpak permission-reset` therefore
does not exist. Worth knowing before the first live test, and worth telling users
in the app's own error path.

The file id also has to stay equal to `singleInstanceID` in `main.go`.

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

Evidence is labelled: **(host)** was read off this machine - binary strings, JS
extracted from `libshell-14.so`, cgroups in `/proc`; **(upstream)** is the
gnome-shell 46.0 / xdg-desktop-portal 1.18.4 source, consistent with this host
but not checkable from its stripped binaries. Versions here: gnome-shell 46.0,
xdg-desktop-portal 1.18.4, xdg-desktop-portal-gnome 46.2, gnome-control-center
46.7, systemd 255.

- **xdg-desktop-portal** derives *our* app id from the systemd user unit the
  process runs in (host: `sd_pid_get_user_unit`; the unit must start with
  `app-`, and `app-<launcher>-<AppID>-<rand>.scope` is parsed for the id). A
  launch by the shell or by gnome-session autostart puts us in
  `app-gnome-io.github.minnyat.linuxshot-<pid>.scope`, so **the app id follows
  the name of the file we were launched from.**
- **gnome-shell** derives only the *focused* app, from the focused window, by
  looking the window's `WM_CLASS` up among installed entries' `StartupWMClass`
  values (upstream), then compares the two ids (host: `accessDialog.js`).
- Both must agree, so the entry helps only when the process is launched in a way
  that **creates that scope**. On this host that means **gnome-shell** (dash, app
  grid, search) or **gnome-session autostart** - the scope name format and
  `StartTransientUnit` live in libgnome-desktop (`gnome_start_systemd_scope`),
  which `libshell-14.so` and `gnome-session-binary` link and libgio does not
  (host).

### `gio launch` and `gtk-launch` are not enough

They honour the entry but do **not** create an app scope here (host: neither
binary links libgnome-desktop, and libgio 2.80 contains neither the scope format
nor `StartTransientUnit`). Testing with them reproduces the exact failure this
entry exists to prevent. To test from a terminal, create the scope explicitly -
**untested here**, since running the app was outside the scope of the task that
wrote this file:

```
systemd-run --user --scope \
  --unit=app-gnome-io.github.minnyat.linuxshot-$RANDOM.scope linuxshot
```

Otherwise, started from a terminal the gate fails even with the entry installed
and our own window focused, because the portal then reports the terminal's
identity (or none) while the focused app is us.

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
prefers the one whose own id equals it (upstream:
`startup_wm_class_is_exact_match`, `src/shell-app-system.c`), regardless of scan
order. So with both
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
autostart entries into the same `app-gnome-<id>-<pid>.scope` form as a dash
launch (host: `update-notifier`, `evolution-alarm-notify` and
`org.gnome.SettingsDaemon.DiskUtilityNotify` all have PPid `gnome-session-binary`
and that cgroup shape; no `-autostart.service` unit exists on this systemd 255
host - that name is the pre-248 generator's, which the portal's regex still
accepts). So a differently named file means a differently named scope and a
portal app id that disagrees again - exactly the trap above, with a different
launcher.

### There is no Settings UI to revoke this permission

For an app like ours (not Flatpak, not Snap) gnome-control-center derives no
portal app id - those two keys are the only id sources in the binary (host) - and
hides the Screenshots row (upstream), even though the portal's own dialog tells
the user the permission can be changed in the privacy settings. A Deny is
permanent until the permission store is edited directly:

```
# per app - this is the one you want
PermissionStore.DeletePermission("screenshot", "screenshot", "io.github.minnyat.linuxshot")
```

on `org.freedesktop.impl.portal.PermissionStore` at
`/org/freedesktop/impl/portal/PermissionStore` (host: `DeletePermission`,
`SetPermission`, `Delete`, `Lookup` are all exported by
`/usr/libexec/xdg-permission-store`). Prefer it over the plain `Delete`, which
drops the whole `screenshot` table entry for every app at once. The store's file
is `~/.local/share/flatpak/db/screenshot`, which exists even on hosts where
flatpak is not installed and `flatpak permission-reset` therefore does not exist.
Worth knowing before the first live test, and worth telling users in the app's
own error path.

### Two rules for the first live test

- **The first non-interactive capture must be made with our own window mapped
  and focused** - that is the only moment the focus gate is judged. The gate is
  consulted on every non-interactive request until a decision is stored, and only
  an *answered* dialog stores one, so a refused request leaves the permission
  unset and changes nothing. Hiding the window first (the natural screenshot
  flow), or a first capture from the tray or a hotkey while another app is
  focused, both return code 2 with no dialog. Once the grant exists, captures
  work with the window hidden.
- **A second launch does not re-identify a running instance.** Wails'
  `SingleInstanceLock` owns a session bus name; a later launch forwards its
  arguments to the owner over D-Bus and exits, so the surviving process keeps the
  scope it was started in. Clicking the icon cannot repair an instance started
  from a terminal - quit it first. (If that forward fails, Wails lets the second
  instance run instead of exiting, leaving two processes with two identities.)

The file id also has to stay equal to `singleInstanceID` in `main.go`.

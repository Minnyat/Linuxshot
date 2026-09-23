//go:build linux

package portal

import (
	"context"
	"errors"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Environment for the manual exercise. It is a test rather than a cmd/ helper
// so the only way to hit the real portal stays in the package it belongs to,
// needs no extra module target, and is skipped by a plain `go test ./...`.
const (
	manualEnv        = "LINUXSHOT_PORTAL_MANUAL"  // set to 1 to run
	manualParentEnv  = "LINUXSHOT_PORTAL_PARENT"  // e.g. x11:0x2a00003; empty is the default
	manualTimeoutEnv = "LINUXSHOT_PORTAL_TIMEOUT" // Go duration, default 20s
	manualOutEnv     = "LINUXSHOT_PORTAL_OUT"     // output PNG, must be under /tmp
	manualReapEnv    = "LINUXSHOT_PORTAL_REAP"    // Go duration, overrides the client reap timeout
)

// manualGrace keeps a failed manual run alive long enough for the background
// reaper to delete a screenshot the portal produced after Capture gave up.
func manualGrace(c *Client) time.Duration { return c.reapWait() + 3*time.Second }

// TestManual_Capture drives the live xdg-desktop-portal Screenshot interface.
// It is skipped unless LINUXSHOT_PORTAL_MANUAL=1, so `go test ./...` never
// touches the session bus.
//
//	LINUXSHOT_PORTAL_MANUAL=1 go test -count=1 -v -run TestManual_Capture ./internal/platform/portal/
//	LINUXSHOT_PORTAL_MANUAL=1 LINUXSHOT_PORTAL_PARENT=x11:0x$(xprop -root _NET_ACTIVE_WINDOW | awk '{print $NF}' | sed 's/^0x//') \
//	  go test -count=1 -v -run TestManual_Capture ./internal/platform/portal/
//
// With an empty parent window and no stored grant, expect ErrDenied within a
// second and no dialog. With the XID of the caller's focused window, expect a
// permission dialog on the first run and a capture afterwards.
func TestManual_Capture(t *testing.T) {
	if os.Getenv(manualEnv) == "" {
		t.Skipf("set %s=1 to exercise the real portal", manualEnv)
	}

	timeout := 20 * time.Second
	if v := os.Getenv(manualTimeoutEnv); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			t.Fatalf("%s=%q: %v", manualTimeoutEnv, v, err)
		}
		timeout = d
	}

	reap := time.Duration(0)
	if v := os.Getenv(manualReapEnv); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			t.Fatalf("%s=%q: %v", manualReapEnv, v, err)
		}
		reap = d
	}

	out := os.Getenv(manualOutEnv)
	if out == "" {
		out = filepath.Join("/tmp", "linuxshot-portal-manual.png")
	}
	if !filepath.IsAbs(out) || filepath.Dir(out) != "/tmp" {
		// Screenshots from a manual run stay in /tmp, never in the user's
		// Pictures directory.
		t.Fatalf("%s=%q must be a path directly under /tmp", manualOutEnv, out)
	}

	c, err := New()
	if err != nil {
		t.Fatalf("New() error = %v (is a session bus reachable?)", err)
	}
	c.reapTimeout = reap
	defer func() { _ = c.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	version, err := c.Version(ctx)
	if err != nil {
		t.Fatalf("Version() error = %v [%s]", err, classify(err))
	}
	t.Logf("Screenshot interface version = %d", version)

	parent := os.Getenv(manualParentEnv)
	t.Logf("parent_window = %q, timeout = %s", parent, timeout)

	start := time.Now()
	img, err := c.Capture(ctx, Options{ParentWindow: parent})
	elapsed := time.Since(start)
	if err != nil {
		if errors.Is(err, ErrTimeout) || errors.Is(err, context.Canceled) {
			// Only an abandoned request needs reaping, and the reaper outlives
			// Capture, so the process has to stay alive for it - otherwise the
			// portal's file is left in the user's Pictures directory, which is
			// the thing being verified here.
			t.Logf("waiting %s for a late response to be reaped", manualGrace(c))
			time.Sleep(manualGrace(c))
		}
		t.Fatalf("Capture() error = %v [%s] after %s", err, classify(err), elapsed)
	}
	t.Logf("captured %v in %s", img.Bounds(), elapsed)

	f, err := os.Create(out)
	if err != nil {
		t.Fatalf("create %s: %v", out, err)
	}
	defer func() { _ = f.Close() }()
	if err := png.Encode(f, img); err != nil {
		t.Fatalf("encode %s: %v", out, err)
	}
	t.Logf("wrote %s", out)
}

// classify names the sentinel an error matches, so a manual run shows which
// outcome the caller would see.
func classify(err error) string {
	switch {
	case err == nil:
		return "success"
	case errors.Is(err, ErrCancelled):
		return "ErrCancelled (user dismissed the dialog)"
	case errors.Is(err, ErrDenied):
		return "ErrDenied (portal refused or failed)"
	case errors.Is(err, ErrTimeout):
		return "ErrTimeout (no response before the deadline)"
	case errors.Is(err, context.Canceled):
		return "context.Canceled (caller gave up)"
	case errors.Is(err, ErrUnavailable):
		return "ErrUnavailable (portal unreachable)"
	case errors.Is(err, ErrInvalidResponse):
		return "ErrInvalidResponse (unusable payload)"
	default:
		return "unclassified"
	}
}

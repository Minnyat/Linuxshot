//go:build linux

// Package portal is a self-contained client for the xdg-desktop-portal
// Screenshot interface (org.freedesktop.portal.Screenshot) over D-Bus.
//
// It exists because kbinani/screenshot's portal path cannot be used: it only
// engages when XDG_SESSION_TYPE is "wayland", always passes an empty
// parent_window, and its response loop has neither a timeout nor a response-code
// check, so a denied or cancelled request blocks forever. Capture always applies
// a deadline and maps every outcome to a sentinel error.
//
// Measured behaviour (GNOME 46, xdg-desktop-portal 1.18.4, Screenshot interface
// version 2, X11 session):
//
//   - parent_window "" from a process that is not the focused window and has no
//     stored grant is refused with Response code 2 immediately and no dialog is
//     shown; the journal says "Only the focused app is allowed to show a system
//     access dialog". So an empty parent_window cannot obtain a first grant -
//     pass the XID of the focused window instead. Once a grant exists an empty
//     parent_window does succeed (measured 2.1s), so callers must not treat it
//     as always-fatal.
//   - parent_window "x11:0x<xid of the focused window>" is accepted, even when
//     that window belongs to another application (the journal then logs "Failed
//     to associate portal window with parent window" and the request still
//     succeeds). The first such request may show a permission dialog and took
//     about 5s here; it leaves a stored grant behind, after which requests
//     answer in 1.2-2.6s with no dialog.
//   - The grant is keyed to the caller's app identity, which the portal derives
//     from the caller's systemd scope, not from parent_window. Changing
//     parent_window does not create a second grant.
//   - The response carries a file: URI. On this host it was a plain path in
//     ~/Pictures (Screenshot.png, Screenshot-1.png, ...), not a document-portal
//     path. The caller owns that file and nothing else removes it, so Capture
//     reads it and deletes it - including on its error paths.
//   - Abandoning a request (deadline or cancellation) does not stop a capture
//     already in flight: a request cancelled after 50ms still produced
//     ~/Pictures/Screenshot.png. Request.Close dismisses the dialog but also
//     suppresses the Response that carries the uri, so Capture hands the
//     subscription to a goroutine that waits reapTimeout for a late Response,
//     deletes the file it names, and only then closes the request.
//   - The image is the whole virtual screen across all monitors (5760x1287 on
//     this host), so callers wanting one display or a region must crop.
package portal

import (
	"context"
	"errors"
	"fmt"
	"image"
	"os"
	"time"

	"github.com/godbus/dbus/v5"
)

// Sentinel errors. Callers must distinguish outcomes with errors.Is, never by
// matching error text.
var (
	// ErrCancelled is returned when the user dismissed the portal dialog
	// (Response code 1).
	ErrCancelled = errors.New("portal: screenshot cancelled by user")

	// ErrDenied is returned when the portal refused or failed the request
	// (Response code 2, or any other non-zero code).
	ErrDenied = errors.New("portal: screenshot denied by portal")

	// ErrTimeout is returned when no Response arrived before the deadline. It
	// also matches context.DeadlineExceeded.
	ErrTimeout = errors.New("portal: timed out waiting for screenshot response")

	// ErrUnavailable is returned when the portal cannot be reached at all: no
	// session bus, no org.freedesktop.portal.Desktop, or the connection dropped.
	ErrUnavailable = errors.New("portal: screenshot portal unavailable")

	// ErrInvalidResponse is returned when the portal answered success but the
	// payload was not usable (missing or malformed uri, unreadable or
	// undecodable file).
	ErrInvalidResponse = errors.New("portal: invalid screenshot response")
)

// DefaultTimeout bounds a request that arrives with no deadline of its own. It
// has to cover a human answering a permission dialog on the first request.
const DefaultTimeout = 60 * time.Second

// closeTimeout bounds the best-effort Request.Close sent when we stop waiting.
const closeTimeout = 2 * time.Second

// reapTimeout bounds how long a goroutine keeps listening after Capture gave up,
// and so also how long an unanswered dialog is left alone before it is
// dismissed. Measured on this host: a normal capture answers in 1.2-2.6s, but
// a request abandoned after 50ms was answered as late as 8s afterwards, and the
// file it wrote had to be reclaimed. 30s covers that with margin; a dialog
// waiting for the user ends this wait as soon as the user answers it.
// A variable so tests can shorten it.
var reapTimeout = 30 * time.Second

const (
	busName         = "org.freedesktop.portal.Desktop"
	desktopPath     = dbus.ObjectPath("/org/freedesktop/portal/desktop")
	screenshotIface = "org.freedesktop.portal.Screenshot"
	requestIface    = "org.freedesktop.portal.Request"
	propertiesIface = "org.freedesktop.DBus.Properties"
)

// Options are the parameters of one screenshot request.
type Options struct {
	// ParentWindow is the portal's parent_window identifier: "x11:0x<xid>",
	// "wayland:<exported handle>", or "" for none. An empty value is refused on
	// GNOME unless a grant already exists - see the package documentation.
	ParentWindow string

	// Interactive asks the portal to show its own capture UI before returning
	// (Screenshot interface version 2 and later). False captures immediately.
	Interactive bool

	// Modal overrides the portal's default (a modal dialog) when non-nil.
	Modal *bool
}

// Client is a connection to the session bus used to make screenshot requests.
// It is safe for concurrent use: each Capture uses its own handle token,
// request path and signal channel.
type Client struct {
	bus bus

	// Timeout is the deadline applied to a Capture whose context has none.
	// Zero means DefaultTimeout.
	Timeout time.Duration
}

// New opens a private session bus connection. Close it when done.
func New() (*Client, error) {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, fmt.Errorf("%w: connect session bus: %w", ErrUnavailable, err)
	}
	return &Client{bus: &sessionBus{conn: conn}, Timeout: DefaultTimeout}, nil
}

// Close releases the bus connection.
func (c *Client) Close() error { return c.bus.Close() }

// Version reports the Screenshot interface version the portal implements
// (2 or later supports the "interactive" option). It returns ErrUnavailable
// when the portal is not on the bus.
func (c *Client) Version(ctx context.Context) (uint32, error) {
	ctx, cancel := c.withDeadline(ctx)
	defer cancel()

	var v dbus.Variant
	call := c.bus.Call(ctx, desktopPath, propertiesIface+".Get", screenshotIface, "version")
	if err := call.Store(&v); err != nil {
		return 0, callError(ctx, err)
	}
	version, ok := v.Value().(uint32)
	if !ok {
		return 0, fmt.Errorf("%w: version property is %s, want u", ErrInvalidResponse, v.Signature())
	}
	return version, nil
}

// Capture requests a screenshot and returns the decoded image, never a path:
// the file the portal produced is deleted before Capture returns.
//
// The image covers the whole virtual screen; crop it if you need less.
//
// Outcomes are distinguished with errors.Is: nil on success, ErrCancelled when
// the user dismissed the dialog, ErrDenied when the portal refused, ErrTimeout
// when nothing answered in time, context.Canceled when the caller's context was
// cancelled, ErrUnavailable when the portal is unreachable, ErrInvalidResponse
// when the payload was unusable.
func (c *Client) Capture(ctx context.Context, opts Options) (*image.RGBA, error) {
	ctx, cancel := c.withDeadline(ctx)
	defer cancel()

	token, err := newHandleToken()
	if err != nil {
		return nil, err
	}
	expected, err := requestPath(c.bus.UniqueName(), token)
	if err != nil {
		return nil, err
	}

	// Subscribe before the method call, so a fast Response cannot be missed.
	// Eavesdropping is not used: the match rules are ordinary subscriptions for
	// signals addressed to this connection.
	signals := make(chan *dbus.Signal, 4)
	c.bus.AddSignalChan(signals)

	var rules [][]dbus.MatchOption
	// unsubscribe is called once, either here or by the reaper goroutine that
	// takes the subscription over when we abandon the request.
	unsubscribe := func() {
		for _, r := range rules {
			_ = c.bus.RemoveMatch(r...)
		}
		c.bus.RemoveSignalChan(signals)
	}
	handedOff := false
	defer func() {
		if !handedOff {
			unsubscribe()
		}
	}()

	match := responseMatch(expected)
	if err := c.bus.AddMatch(ctx, match...); err != nil {
		return nil, fmt.Errorf("%w: subscribe to %s: %w", ErrUnavailable, expected, err)
	}
	rules = append(rules, match)

	var handle dbus.ObjectPath
	call := c.bus.Call(ctx, desktopPath, screenshotIface+".Screenshot", opts.ParentWindow, buildOptions(token, opts))
	if err := call.Store(&handle); err != nil {
		// A reply that never arrived does not mean the portal dropped the
		// request, so the same cleanup applies; the request is at the path we
		// derived, which is why deriving it matters.
		if ctx.Err() != nil {
			handedOff = true
			go c.reapAbandoned(expected, signals, unsubscribe)
		}
		return nil, callError(ctx, err)
	}

	// xdg-desktop-portal 0.9 and later returns the path we derived. Older
	// implementations picked their own, so subscribe to that one too. A Response
	// sent before this second subscription lands would be missed; nothing can be
	// done about that without the spec's predictable path, and no supported
	// portal version takes this branch.
	if handle != expected {
		late := responseMatch(handle)
		if err := c.bus.AddMatch(ctx, late...); err != nil {
			return nil, fmt.Errorf("%w: subscribe to %s: %w", ErrUnavailable, handle, err)
		}
		rules = append(rules, late)
	}

	for {
		select {
		case <-ctx.Done():
			// Keep listening in the background: the portal finishes the capture
			// anyway and the file it writes is ours to delete.
			handedOff = true
			go c.reapAbandoned(handle, signals, unsubscribe)
			return nil, waitError(ctx.Err())

		case sig, ok := <-signals:
			if !ok {
				return nil, fmt.Errorf("%w: bus connection closed while waiting", ErrUnavailable)
			}
			if !isResponseFor(sig, handle) {
				continue
			}
			uri, err := screenshotURI(sig)
			if err != nil {
				return nil, err
			}
			return readAndRemoveImage(uri)
		}
	}
}

// withDeadline applies the client's default deadline when the caller supplied
// none, so a hung portal can never block forever.
func (c *Client) withDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return context.WithCancel(ctx)
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return context.WithTimeout(ctx, timeout)
}

// closeRequest abandons a pending request. Best effort with its own short
// deadline: the context that got us here is already done.
func (c *Client) closeRequest(handle dbus.ObjectPath) {
	if !handle.IsValid() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	defer cancel()
	c.bus.Call(ctx, handle, requestIface+".Close")
}

// reapAbandoned cleans up after a request Capture no longer waits for.
//
// Request.Close ends the user interaction but not a capture already under way:
// the portal still writes the file, and that file is ours to delete. Worse,
// closing suppresses the Response, which is the only place the file's uri
// appears. So the order here is listen first, close last - wait up to
// reapTimeout for a Response and delete whatever file it names, and only when
// nothing answers assume a dialog is sitting there unanswered and dismiss it.
//
// If the process exits before a late Response arrives the file is left behind,
// which is the portal's own default behaviour.
func (c *Client) reapAbandoned(handle dbus.ObjectPath, signals <-chan *dbus.Signal, unsubscribe func()) {
	defer unsubscribe()

	timer := time.NewTimer(reapTimeout)
	defer timer.Stop()

	for {
		select {
		case <-timer.C:
			c.closeRequest(handle)
			return
		case sig, ok := <-signals:
			if !ok {
				return
			}
			if !isResponseFor(sig, handle) {
				continue
			}
			uri, err := screenshotURI(sig)
			if err != nil {
				return // cancelled or denied: there is no file to remove
			}
			if path, err := fileURIToPath(uri); err == nil {
				_ = os.Remove(path)
			}
			return
		}
	}
}

// waitError maps the reason we stopped waiting. A cancelled context is the
// caller's own doing and stays context.Canceled, which is not ErrCancelled:
// that one means the user dismissed the portal dialog.
func waitError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w: %w", ErrTimeout, err)
	}
	return fmt.Errorf("portal: screenshot request abandoned: %w", err)
}

// callError maps a failed method call. The portal is either missing or the
// deadline expired mid-call; a D-Bus error from a portal that is present means
// it refused the request.
func callError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return waitError(ctxErr)
	}
	var busErr dbus.Error
	if errors.As(err, &busErr) && isUnavailableName(busErr.Name) {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return fmt.Errorf("%w: %w", ErrDenied, err)
}

func isUnavailableName(name string) bool {
	switch name {
	case "org.freedesktop.DBus.Error.ServiceUnknown",
		"org.freedesktop.DBus.Error.NameHasNoOwner",
		"org.freedesktop.DBus.Error.UnknownInterface",
		"org.freedesktop.DBus.Error.UnknownMethod",
		"org.freedesktop.DBus.Error.UnknownObject",
		"org.freedesktop.DBus.Error.NoReply",
		"org.freedesktop.DBus.Error.Disconnected":
		return true
	}
	return false
}

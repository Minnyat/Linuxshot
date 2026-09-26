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
// Measured behaviour (GNOME 46.2, xdg-desktop-portal 1.18.4, Screenshot
// interface version 2, X11 session):
//
//   - What gates a first, non-interactive request is the identity of the focused
//     app, not parent_window. gnome-shell's accessDialog.js compares the app id
//     the shell derives for the calling process against the app the shell
//     considers focused, and refuses to show the dialog when they differ. It
//     does not consult the parent_window we send at all - its own comment says
//     it may use parentWindow "in the future". The refusal is Response code 2,
//     immediately and with no dialog; the journal says "Only the focused app is
//     allowed to show a system access dialog".
//   - Counting as the focused app therefore depends on the shell being able to
//     map our window to a desktop entry. That is what
//     build/linux/io.github.minnyat.linuxshot.desktop and its
//     StartupWMClass=linuxshot exist for: the entry's file id is the app id the
//     portal records the permission under, and StartupWMClass is how the shell
//     ties our window (WM_CLASS instance "linuxshot", from
//     g_set_prgname("linuxshot")) to that entry. With no such entry installed
//     the shell has no app id for us, our own window being focused does not
//     make us the focused app, and a grant obtained in that state is recorded
//     against whichever app the shell did consider focused instead.
//   - parent_window only parents the dialog. An empty one, or the XID of a
//     window belonging to another application (the journal then logs "Failed to
//     associate portal window with parent window"), changes nothing about
//     whether the request is allowed and does not create a second grant. Sending
//     the focused window's XID is still worth doing for dialog placement, but it
//     buys no permission.
//   - Interactive: true bypasses all of that - the stored permission, the dialog
//     and the focus check alike - because xdg-desktop-portal routes an
//     interactive request straight to the compositor's own screenshot UI
//     (screenshot.c:219). It needs no grant and leaves none behind.
//   - The decision is stored, and a Deny is permanent. One Deny writes
//     PERMISSION_NO to the portal's permission store, after which every
//     non-interactive request returns Response code 2 with no dialog, whoever is
//     focused; the user's only way back is GNOME Settings' app permission UI,
//     which edits that same store. A grant writes PERMISSION_YES and is checked
//     before the focus gate ever runs, so later requests succeed with no dialog
//     (measured 1.2-2.6s) even from an unfocused process and with an empty
//     parent_window.
//   - The response carries a file: URI. On this host it was a plain path in
//     ~/Pictures (Screenshot.png, Screenshot-1.png, ...), not a document-portal
//     path. The caller owns that file and nothing else removes it, so Capture
//     reads it and deletes it - including on its error paths.
//   - Abandoning a request (deadline or cancellation) does not stop a capture
//     already in flight: a request cancelled after 50ms still produced
//     ~/Pictures/Screenshot.png. Request.Close dismisses the dialog but also
//     suppresses the Response that carries the uri, so Capture hands the
//     subscription to a goroutine that waits out a reap timeout for a late
//     Response,
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
	"sync"
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

// defaultReapTimeout bounds how long a goroutine keeps listening after Capture
// gave up, and so also how long an unanswered dialog is left alone before it is
// dismissed. Measured on this host: a normal capture answers in 1.2-2.6s, but a
// request abandoned after 50ms was answered as late as 8s afterwards, and the
// file it wrote had to be reclaimed. 30s covers that with margin; a dialog
// waiting for the user ends this wait as soon as the user answers it.
//
// Dismissing an unanswered dialog sooner would reintroduce that leak, so the
// trade-off stays as it is until the capture is wired to a real user gesture
// and the dialog can be judged on screen.
const defaultReapTimeout = 30 * time.Second

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
	// "wayland:<exported handle>", or "" for none. It only parents the dialog:
	// on GNOME it is not what decides whether a request is allowed, so an empty
	// value is neither necessary nor sufficient for a refusal - see the package
	// documentation.
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

	// reapTimeout overrides defaultReapTimeout when non-zero. Per-client rather
	// than a package variable so tests can shorten it without racing against
	// reapers left over from other tests.
	reapTimeout time.Duration

	mu      sync.Mutex
	closing bool
	reapers sync.WaitGroup
}

// New opens a private session bus connection. Close it when done.
func New() (*Client, error) {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, fmt.Errorf("%w: connect session bus: %w", ErrUnavailable, err)
	}
	return &Client{bus: &sessionBus{conn: conn}, Timeout: DefaultTimeout}, nil
}

// Close releases the bus connection, but first waits for any reaper still
// trying to delete the file of an abandoned request: closing the connection
// closes the signal channels, which would lose that file.
//
// So Close can block for up to a reap timeout when a capture was abandoned
// moments earlier, and returns immediately in every other case. A process that
// exits or is killed before Close returns still leaves the file behind; nothing
// in this package can cover that.
func (c *Client) Close() error {
	c.mu.Lock()
	c.closing = true
	c.mu.Unlock()

	// Reapers bound themselves by reapTimeout; the margin covers their own
	// Request.Close and unsubscribe round trips, each bounded by closeTimeout.
	// The deadline is belt and braces so Close cannot hang on a stuck reaper.
	done := make(chan struct{})
	go func() {
		c.reapers.Wait()
		close(done)
	}()
	deadline := time.NewTimer(c.reapWait() + 2*closeTimeout)
	defer deadline.Stop()
	select {
	case <-done:
	case <-deadline.C:
	}

	return c.bus.Close()
}

// Version reports the Screenshot interface version the portal implements
// (2 or later supports the "interactive" option). It returns ErrUnavailable
// when the portal is not on the bus.
func (c *Client) Version(ctx context.Context) (uint32, error) {
	ctx, cancel := c.withDeadline(ctx)
	defer cancel()

	call := c.bus.Call(ctx, desktopPath, propertiesIface+".Get", screenshotIface, "version")
	if call.Err != nil {
		return 0, callError(ctx, call.Err, ErrDenied)
	}
	// Store's own errors are decode failures on a reply that did arrive, so they
	// are a protocol problem, not an absent portal.
	var v dbus.Variant
	if err := call.Store(&v); err != nil {
		return 0, fmt.Errorf("%w: version reply: %w", ErrInvalidResponse, err)
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
	// takes the subscription over when we abandon the request. It gets its own
	// short deadline rather than the caller's context: removing a match rule is
	// a round trip to the bus daemon, and this also runs after ctx is done.
	unsubscribe := func() {
		ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
		defer cancel()
		for _, r := range rules {
			_ = c.bus.RemoveMatch(ctx, r...)
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
		return nil, callError(ctx, fmt.Errorf("subscribe to %s: %w", expected, err), ErrUnavailable)
	}
	rules = append(rules, match)

	call := c.bus.Call(ctx, desktopPath, screenshotIface+".Screenshot", opts.ParentWindow, buildOptions(token, opts))
	if call.Err != nil {
		// A reply that never arrived does not mean the portal dropped the
		// request, so the same cleanup applies; the request is at the path we
		// derived, which is why deriving it matters.
		if ctx.Err() != nil && c.startReaper(expected, signals, unsubscribe) {
			handedOff = true
		}
		return nil, callError(ctx, call.Err, ErrDenied)
	}

	// A reply arrived; anything Store rejects is a decode failure, which makes
	// this a portal speaking a protocol we do not understand rather than a
	// missing one. No reaper is started here, unlike the branch above: there the
	// spec still guarantees the request lives at the derived path, whereas a
	// portal that answers Screenshot with the wrong signature has already broken
	// that guarantee, so listening on a path it may not be using would only hold
	// a subscription open for nothing.
	var handle dbus.ObjectPath
	if err := call.Store(&handle); err != nil {
		return nil, fmt.Errorf("%w: Screenshot reply: %w", ErrInvalidResponse, err)
	}
	// Store alone is not enough: an object path is a string type, and godbus
	// converts any convertible value into it, so a reply carrying a number comes
	// back as a nonsense path instead of an error. Left unchecked we would wait
	// out the whole deadline for a Response that can never arrive, and report a
	// timeout for what is really a malformed reply.
	if !handle.IsValid() {
		return nil, fmt.Errorf("%w: Screenshot returned %q, which is not an object path", ErrInvalidResponse, handle)
	}

	// xdg-desktop-portal 0.9 and later returns the path we derived. Older
	// implementations picked their own, so subscribe to that one too. A Response
	// sent before this second subscription lands would be missed; nothing can be
	// done about that without the spec's predictable path, and no supported
	// portal version takes this branch.
	if handle != expected {
		late := responseMatch(handle)
		if err := c.bus.AddMatch(ctx, late...); err != nil {
			return nil, callError(ctx, fmt.Errorf("subscribe to %s: %w", handle, err), ErrUnavailable)
		}
		rules = append(rules, late)
	}

	for {
		select {
		case <-ctx.Done():
			// Keep listening in the background: the portal finishes the capture
			// anyway and the file it writes is ours to delete.
			if c.startReaper(handle, signals, unsubscribe) {
				handedOff = true
			}
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

// reapWait is the reap timeout in force for this client.
func (c *Client) reapWait() time.Duration {
	if c.reapTimeout <= 0 {
		return defaultReapTimeout
	}
	return c.reapTimeout
}

// startReaper hands the subscription to a background reaper and reports whether
// it took it. It declines once Close has begun, because the bus connection is
// about to go away.
//
// A declined caller keeps only the unsubscribe: it does not reap a late file and
// does not send Request.Close. Both are deliberate on a connection that is
// closing - Request.Close is a round trip that would not land, and reaping needs
// a Response that the imminent bus.Close makes unreachable. Do not "fix" the
// missing closeRequest here; the cost is a leftover file on a client closed
// moments after an abandoned capture, which Close's own wait already covers for
// every reaper that did start.
func (c *Client) startReaper(handle dbus.ObjectPath, signals <-chan *dbus.Signal, unsubscribe func()) bool {
	c.mu.Lock()
	if c.closing {
		c.mu.Unlock()
		return false
	}
	c.reapers.Add(1)
	c.mu.Unlock()

	go func() {
		defer c.reapers.Done()
		c.reapAbandoned(handle, signals, unsubscribe)
	}()
	return true
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
// appears. So the order here is listen first, close last - wait out the reap
// timeout for a Response and delete whatever file it names, and only when
// nothing answers assume a dialog is sitting there unanswered and dismiss it.
//
// If the process exits before a late Response arrives the file is left behind,
// which is the portal's own default behaviour.
func (c *Client) reapAbandoned(handle dbus.ObjectPath, signals <-chan *dbus.Signal, unsubscribe func()) {
	defer unsubscribe()

	timer := time.NewTimer(c.reapWait())
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

// callError maps a failed D-Bus call, in three steps.
//
// The caller's context comes first: a deadline or cancellation is why the call
// failed, and reporting it as anything else would make the error match two
// unrelated sentinels at once.
//
// Then, an error that is not a dbus.Error is a transport failure: godbus reports
// those as dbus.ErrClosed or a raw io/net error, meaning the request never
// reached the portal. That is ErrUnavailable - the same sentinel the signal-wait
// branch returns for a closed connection, so one event cannot yield two answers
// depending on where Capture happened to be. Call sites must pass only Call.Err
// here: a decode error from Call.Store is also a plain error, but it comes from
// a reply that did arrive and belongs to ErrInvalidResponse instead.
//
// Only a genuine dbus.Error is left. A name meaning "nothing is there" is
// ErrUnavailable; anything else is a peer rejecting us, and the sentinel for
// that is the call site's to choose - ErrDenied for the portal refusing a
// request, ErrUnavailable for the bus daemon refusing a subscription.
func callError(ctx context.Context, err error, fallback error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return waitError(ctxErr)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		// Defensive, and unreachable with godbus v5.1.0: it only finalizes a call
		// with a context error taken from the context passed to CallWithContext,
		// which is the one checked above. Kept so a future version that carries
		// someone else's context error cannot turn it into ErrUnavailable.
		return waitError(err)
	}

	var busErr dbus.Error
	if !errors.As(err, &busErr) {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	if isUnavailableName(busErr.Name) {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return fmt.Errorf("%w: %w", fallback, err)
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

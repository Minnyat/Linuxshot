//go:build linux

package portal

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"image"
	"image/draw"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/godbus/dbus/v5"

	// Decoders for whatever the portal produces. GNOME writes PNG; JPEG is
	// registered so an alternative backend does not fail to decode.
	_ "image/jpeg"
	_ "image/png"
)

// Portal Response codes (org.freedesktop.portal.Request.Response).
const (
	responseSuccess   uint32 = 0
	responseCancelled uint32 = 1
	responseFailed    uint32 = 2
)

// bus is the slice of the session bus connection this package needs. The
// indirection keeps a fake injectable in tests, so none of them need a live bus.
type bus interface {
	// UniqueName is the connection's own bus name, e.g. ":1.42".
	UniqueName() string
	AddMatch(ctx context.Context, opts ...dbus.MatchOption) error
	RemoveMatch(ctx context.Context, opts ...dbus.MatchOption) error
	AddSignalChan(ch chan<- *dbus.Signal)
	RemoveSignalChan(ch chan<- *dbus.Signal)
	Call(ctx context.Context, path dbus.ObjectPath, method string, args ...interface{}) *dbus.Call
	Close() error
}

// sessionBus adapts *dbus.Conn to bus.
type sessionBus struct{ conn *dbus.Conn }

func (s *sessionBus) UniqueName() string {
	names := s.conn.Names()
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

func (s *sessionBus) AddMatch(ctx context.Context, opts ...dbus.MatchOption) error {
	return s.conn.AddMatchSignalContext(ctx, opts...)
}

func (s *sessionBus) RemoveMatch(ctx context.Context, opts ...dbus.MatchOption) error {
	return s.conn.RemoveMatchSignalContext(ctx, opts...)
}

func (s *sessionBus) AddSignalChan(ch chan<- *dbus.Signal)    { s.conn.Signal(ch) }
func (s *sessionBus) RemoveSignalChan(ch chan<- *dbus.Signal) { s.conn.RemoveSignal(ch) }

func (s *sessionBus) Call(ctx context.Context, path dbus.ObjectPath, method string, args ...interface{}) *dbus.Call {
	return s.conn.Object(busName, path).CallWithContext(ctx, method, 0, args...)
}

func (s *sessionBus) Close() error { return s.conn.Close() }

// newHandleToken returns a fresh handle_token. The token becomes one element of
// the request object path, so it must only contain [A-Za-z0-9_].
func newHandleToken() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("portal: generate handle token: %w", err)
	}
	return "linuxshot_" + hex.EncodeToString(b[:]), nil
}

// requestPath derives the object path of the Request the portal will create,
// per the spec: /org/freedesktop/portal/desktop/request/SENDER/TOKEN, where
// SENDER is the caller's unique name without the leading ':' and with every '.'
// replaced by '_'. Deriving it lets us subscribe before making the call.
func requestPath(uniqueName, token string) (dbus.ObjectPath, error) {
	if !strings.HasPrefix(uniqueName, ":") {
		return "", fmt.Errorf("%w: unique bus name %q is not of the form \":1.42\"", ErrUnavailable, uniqueName)
	}
	sender := strings.ReplaceAll(strings.TrimPrefix(uniqueName, ":"), ".", "_")
	if !isPathElement(sender) {
		return "", fmt.Errorf("%w: unique bus name %q does not yield a usable path element", ErrUnavailable, uniqueName)
	}
	if !isPathElement(token) {
		return "", fmt.Errorf("%w: handle token %q is not usable as a path element", ErrUnavailable, token)
	}
	return dbus.ObjectPath("/org/freedesktop/portal/desktop/request/" + sender + "/" + token), nil
}

// isRequestHandle reports whether p can be the object path of a portal Request.
// dbus.ObjectPath.IsValid is not enough on its own: an object path is a string
// type, so godbus converts whatever a reply carries into one, and a numeric body
// becomes a one-rune path - rune 47 is "/", the root path, which IsValid
// accepts. No Request lives at the root, and a handle must have at least one
// path element, so "/" is as malformed as "\a" is.
func isRequestHandle(p dbus.ObjectPath) bool {
	return p.IsValid() && p != "/"
}

// isPathElement reports whether s can be one element of a D-Bus object path:
// non-empty, and only [A-Za-z0-9_].
func isPathElement(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
		default:
			return false
		}
	}
	return true
}

// buildOptions marshals the a{sv} options of a Screenshot call. handle_token is
// a plain string, not a variant of some other type.
func buildOptions(token string, o Options) map[string]dbus.Variant {
	opts := map[string]dbus.Variant{
		"handle_token": dbus.MakeVariant(token),
	}
	if o.Interactive {
		opts["interactive"] = dbus.MakeVariant(true)
	}
	if o.Modal != nil {
		opts["modal"] = dbus.MakeVariant(*o.Modal)
	}
	return opts
}

// responseMatch is the match rule for the Response signal of one request.
func responseMatch(handle dbus.ObjectPath) []dbus.MatchOption {
	return []dbus.MatchOption{
		dbus.WithMatchSender(busName),
		dbus.WithMatchInterface(requestIface),
		dbus.WithMatchMember("Response"),
		dbus.WithMatchObjectPath(handle),
	}
}

// isResponseFor reports whether sig is the Response of the given request. The
// sender is not re-checked: the match rule names the portal's well-known bus
// name, which only the bus can resolve, and the 64 random bits in the handle
// token make the request path unguessable for anything trying to unicast a
// forged Response at us.
func isResponseFor(sig *dbus.Signal, handle dbus.ObjectPath) bool {
	return sig != nil && sig.Path == handle && sig.Name == requestIface+".Response"
}

// screenshotURI maps a Response signal to the uri it carries, or to the
// sentinel error the response code stands for.
func screenshotURI(sig *dbus.Signal) (string, error) {
	var (
		code    uint32
		results map[string]dbus.Variant
	)
	if err := dbus.Store(sig.Body, &code, &results); err != nil {
		return "", fmt.Errorf("%w: Response body is not (ua{sv}): %w", ErrInvalidResponse, err)
	}

	switch code {
	case responseSuccess:
	case responseCancelled:
		return "", ErrCancelled
	case responseFailed:
		return "", ErrDenied
	default:
		return "", fmt.Errorf("%w: response code %d", ErrDenied, code)
	}

	v, ok := results["uri"]
	if !ok {
		return "", fmt.Errorf("%w: success response has no uri", ErrInvalidResponse)
	}
	uri, ok := v.Value().(string)
	if !ok {
		return "", fmt.Errorf("%w: uri is %s, want s", ErrInvalidResponse, v.Signature())
	}
	return uri, nil
}

// fileURIToPath converts a file: URI from the portal into a local path.
// url.Parse already percent-decodes, so "%20" comes back as a space.
func fileURIToPath(uri string) (string, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return "", fmt.Errorf("%w: parse uri %q: %w", ErrInvalidResponse, uri, err)
	}
	if u.Scheme != "file" {
		return "", fmt.Errorf("%w: uri scheme %q, want file", ErrInvalidResponse, u.Scheme)
	}
	if u.Host != "" && u.Host != "localhost" {
		return "", fmt.Errorf("%w: uri host %q is not local", ErrInvalidResponse, u.Host)
	}
	if !filepath.IsAbs(u.Path) {
		return "", fmt.Errorf("%w: uri path %q is not absolute", ErrInvalidResponse, u.Path)
	}
	return filepath.Clean(u.Path), nil
}

// readAndRemoveImage decodes the file the portal wrote and deletes it, so the
// caller only ever sees pixels. The file belongs to us: on this host the portal
// writes it straight into ~/Pictures and nothing else cleans it up.
//
// Removal failures are deliberately ignored - a file we cannot unlink (a
// read-only directory, or a document-portal path under /run/user/N/doc where
// the fuse mount refuses unlink) must not turn a good screenshot into an error.
// The cost of that choice is a leftover file, which is exactly what the portal
// would have left anyway.
func readAndRemoveImage(uri string) (*image.RGBA, error) {
	path, err := fileURIToPath(uri)
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.Remove(path) }()

	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("%w: open %s: %w", ErrInvalidResponse, path, err)
	}
	defer func() { _ = f.Close() }()

	img, _, err := image.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("%w: decode %s: %w", ErrInvalidResponse, path, err)
	}
	return toRGBA(img), nil
}

// toRGBA returns img as *image.RGBA with its origin at (0,0).
func toRGBA(img image.Image) *image.RGBA {
	if rgba, ok := img.(*image.RGBA); ok && rgba.Rect.Min == (image.Point{}) {
		return rgba
	}
	b := img.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(dst, dst.Bounds(), img, b.Min, draw.Src)
	return dst
}

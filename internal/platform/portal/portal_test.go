//go:build linux

package portal

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

// ==================== fake bus ====================

type fakeCall struct {
	Path   dbus.ObjectPath
	Method string
	Args   []interface{}
}

// fakeBus implements bus without any session bus. onCall decides what a method
// call returns and may emit signals; events records the order of operations so
// tests can assert the Response subscription happens before the call.
type fakeBus struct {
	mu          sync.Mutex
	name        string
	matches     [][]dbus.MatchOption
	unmatched   [][]dbus.MatchOption
	calls       []fakeCall
	chans       []chan<- *dbus.Signal
	chansGone   int
	events      []string
	addMatchErr error
	closed      bool

	onCall func(fb *fakeBus, c fakeCall) *dbus.Call
}

func newFakeBus() *fakeBus {
	return &fakeBus{name: ":1.42"}
}

func (fb *fakeBus) UniqueName() string { return fb.name }

func (fb *fakeBus) AddMatch(ctx context.Context, opts ...dbus.MatchOption) error {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	if fb.addMatchErr != nil {
		return fb.addMatchErr
	}
	fb.matches = append(fb.matches, opts)
	fb.events = append(fb.events, "match")
	return nil
}

func (fb *fakeBus) RemoveMatch(ctx context.Context, opts ...dbus.MatchOption) error {
	if err := ctx.Err(); err != nil {
		// Cleanup must not ride the caller's expired context.
		return err
	}
	fb.mu.Lock()
	defer fb.mu.Unlock()
	fb.unmatched = append(fb.unmatched, opts)
	return nil
}

func (fb *fakeBus) AddSignalChan(ch chan<- *dbus.Signal) {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	fb.chans = append(fb.chans, ch)
	fb.events = append(fb.events, "signal-chan")
}

// RemoveSignalChan counts releases but deliberately leaves the channel
// connected, so a test can emit a late signal and prove that nothing is still
// reading it.
func (fb *fakeBus) RemoveSignalChan(ch chan<- *dbus.Signal) {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	fb.chansGone++
}

func (fb *fakeBus) Call(ctx context.Context, path dbus.ObjectPath, method string, args ...interface{}) *dbus.Call {
	c := fakeCall{Path: path, Method: method, Args: args}
	fb.mu.Lock()
	fb.calls = append(fb.calls, c)
	fb.events = append(fb.events, "call:"+method)
	onCall := fb.onCall
	fb.mu.Unlock()

	if onCall == nil {
		return &dbus.Call{}
	}
	return onCall(fb, c)
}

func (fb *fakeBus) Close() error {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	fb.closed = true
	return nil
}

// emit delivers a signal to every registered channel. Every channel gets every
// signal, because one Client is one bus connection and godbus fans a signal out
// to all channels registered on it; the client is expected to filter by request
// path. Like the real signal handler, emit never blocks on a full buffer - it
// defers that delivery instead (see signalChannelData.deliver in godbus).
func (fb *fakeBus) emit(sig *dbus.Signal) {
	fb.mu.Lock()
	chans := append([]chan<- *dbus.Signal(nil), fb.chans...)
	fb.mu.Unlock()
	for _, ch := range chans {
		select {
		case ch <- sig:
		default:
			go func(ch chan<- *dbus.Signal) {
				timer := time.NewTimer(5 * time.Second)
				defer timer.Stop()
				select {
				case ch <- sig:
				case <-timer.C: // nobody is reading any more
				}
			}(ch)
		}
	}
}

// closeChans closes the registered signal channels, as dbus does when the
// connection drops.
func (fb *fakeBus) closeChans() {
	fb.mu.Lock()
	chans := append([]chan<- *dbus.Signal(nil), fb.chans...)
	fb.mu.Unlock()
	for _, ch := range chans {
		close(ch)
	}
}

func (fb *fakeBus) snapshot() ([]fakeCall, []string, int) {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	return append([]fakeCall(nil), fb.calls...), append([]string(nil), fb.events...), fb.chansGone
}

// matchCounts returns how many match rules were added and removed. Locked
// because the reaper goroutine may still be unsubscribing.
func (fb *fakeBus) matchCounts() (added, removed int) {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	return len(fb.matches), len(fb.unmatched)
}

// respondWith answers a Screenshot call with the request handle the client
// derived, then emits a Response signal carrying code and results.
func respondWith(t *testing.T, code uint32, results map[string]dbus.Variant) func(*fakeBus, fakeCall) *dbus.Call {
	t.Helper()
	return func(fb *fakeBus, c fakeCall) *dbus.Call {
		if c.Method != screenshotIface+".Screenshot" {
			return &dbus.Call{}
		}
		handle := handleFromCall(t, fb, c)
		go fb.emit(&dbus.Signal{
			Sender: busName,
			Path:   handle,
			Name:   requestIface + ".Response",
			Body:   []interface{}{code, results},
		})
		return &dbus.Call{Body: []interface{}{handle}}
	}
}

// handleFromCall rebuilds the request path from the handle_token the client sent.
func handleFromCall(t *testing.T, fb *fakeBus, c fakeCall) dbus.ObjectPath {
	t.Helper()
	if len(c.Args) != 2 {
		t.Fatalf("Screenshot called with %d args, want 2", len(c.Args))
	}
	opts, ok := c.Args[1].(map[string]dbus.Variant)
	if !ok {
		t.Fatalf("Screenshot options are %T, want map[string]dbus.Variant", c.Args[1])
	}
	token, ok := opts["handle_token"].Value().(string)
	if !ok {
		t.Fatalf("handle_token is %T, want string", opts["handle_token"].Value())
	}
	path, err := requestPath(fb.UniqueName(), token)
	if err != nil {
		t.Fatalf("requestPath() error = %v", err)
	}
	return path
}

// writePNG writes a decodable PNG of the given size and returns its file: URI.
func writePNG(t *testing.T, name string, w, h int) (string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()
	if err := png.Encode(f, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return path, "file://" + path
}

// testClient bounds both timeouts per client: a reaper can then never outlive
// the test that started it by more than reapTimeout, and nothing is shared
// between tests.
func testClient(fb *fakeBus) *Client {
	return &Client{bus: fb, Timeout: 2 * time.Second, reapTimeout: 2 * time.Second}
}

// ==================== handle path / token ====================

func TestRequestPath(t *testing.T) {
	tests := []struct {
		name       string
		uniqueName string
		token      string
		want       dbus.ObjectPath
		wantErr    bool
	}{
		{
			name:       "sender colon stripped and dots underscored",
			uniqueName: ":1.42",
			token:      "linuxshot_0",
			want:       "/org/freedesktop/portal/desktop/request/1_42/linuxshot_0",
		},
		{
			name:       "multiple dots",
			uniqueName: ":1.2.3",
			token:      "tok",
			want:       "/org/freedesktop/portal/desktop/request/1_2_3/tok",
		},
		{name: "well-known name rejected", uniqueName: "org.example.App", token: "tok", wantErr: true},
		{name: "empty name rejected", uniqueName: "", token: "tok", wantErr: true},
		{name: "empty sender rejected", uniqueName: ":", token: "tok", wantErr: true},
		{name: "empty token rejected", uniqueName: ":1.42", token: "", wantErr: true},
		{name: "token with path separator rejected", uniqueName: ":1.42", token: "a/b", wantErr: true},
		{name: "token with dash rejected", uniqueName: ":1.42", token: "a-b", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := requestPath(tt.uniqueName, tt.token)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("requestPath(%q, %q) = %q, want error", tt.uniqueName, tt.token, got)
				}
				if !errors.Is(err, ErrUnavailable) {
					t.Errorf("error = %v, want ErrUnavailable", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("requestPath() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("requestPath() = %q, want %q", got, tt.want)
			}
			if !got.IsValid() {
				t.Errorf("requestPath() = %q, which is not a valid object path", got)
			}
		})
	}
}

func TestNewHandleToken(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 32; i++ {
		token, err := newHandleToken()
		if err != nil {
			t.Fatalf("newHandleToken() error = %v", err)
		}
		if seen[token] {
			t.Fatalf("newHandleToken() returned %q twice", token)
		}
		seen[token] = true
		for _, r := range token {
			if !(r == '_' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')) {
				t.Fatalf("token %q contains %q, not valid in an object path element", token, r)
			}
		}
		if _, err := requestPath(":1.42", token); err != nil {
			t.Fatalf("token %q does not yield a valid request path: %v", token, err)
		}
	}
}

// ==================== options ====================

func TestBuildOptions(t *testing.T) {
	modal := false
	tests := []struct {
		name string
		opts Options
		want map[string]string // key -> signature
	}{
		{
			name: "defaults send only handle_token",
			opts: Options{},
			want: map[string]string{"handle_token": "s"},
		},
		{
			name: "interactive added when set",
			opts: Options{Interactive: true},
			want: map[string]string{"handle_token": "s", "interactive": "b"},
		},
		{
			name: "modal added only when non-nil",
			opts: Options{Modal: &modal},
			want: map[string]string{"handle_token": "s", "modal": "b"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildOptions("tok", tt.opts)
			if len(got) != len(tt.want) {
				t.Fatalf("buildOptions() keys = %v, want %v", keys(got), tt.want)
			}
			for k, sig := range tt.want {
				v, ok := got[k]
				if !ok {
					t.Fatalf("buildOptions() missing key %q", k)
				}
				if v.Signature().String() != sig {
					t.Errorf("option %q signature = %q, want %q", k, v.Signature(), sig)
				}
			}
			if token, ok := got["handle_token"].Value().(string); !ok || token != "tok" {
				t.Errorf("handle_token = %#v, want string \"tok\"", got["handle_token"].Value())
			}
		})
	}
}

func keys(m map[string]dbus.Variant) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestResponseMatchRule(t *testing.T) {
	handle := dbus.ObjectPath("/org/freedesktop/portal/desktop/request/1_42/tok")
	got := responseMatch(handle)
	want := []dbus.MatchOption{
		dbus.WithMatchSender(busName),
		dbus.WithMatchInterface(requestIface),
		dbus.WithMatchMember("Response"),
		dbus.WithMatchObjectPath(handle),
	}
	if len(got) != len(want) {
		t.Fatalf("responseMatch() = %d options, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("option %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	// Eavesdropping must never be used: it is deprecated and needs a
	// monitoring connection.
	for _, o := range got {
		if o == dbus.WithMatchEavesdrop(true) {
			t.Error("responseMatch() sets eavesdrop")
		}
	}
}

func TestIsResponseFor(t *testing.T) {
	handle := dbus.ObjectPath("/org/freedesktop/portal/desktop/request/1_42/tok")
	other := dbus.ObjectPath("/org/freedesktop/portal/desktop/request/1_42/zzz")
	tests := []struct {
		name string
		sig  *dbus.Signal
		want bool
	}{
		{name: "nil", sig: nil, want: false},
		{name: "match", sig: &dbus.Signal{Path: handle, Name: requestIface + ".Response"}, want: true},
		{name: "other request", sig: &dbus.Signal{Path: other, Name: requestIface + ".Response"}, want: false},
		{name: "other member", sig: &dbus.Signal{Path: handle, Name: requestIface + ".Closed"}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isResponseFor(tt.sig, handle); got != tt.want {
				t.Errorf("isResponseFor() = %v, want %v", got, tt.want)
			}
		})
	}
}

// ==================== response mapping ====================

func TestScreenshotURI(t *testing.T) {
	uriResults := map[string]dbus.Variant{"uri": dbus.MakeVariant("file:///tmp/shot.png")}
	tests := []struct {
		name    string
		body    []interface{}
		want    string
		wantErr error
	}{
		{name: "success", body: []interface{}{responseSuccess, uriResults}, want: "file:///tmp/shot.png"},
		{name: "user cancelled", body: []interface{}{responseCancelled, map[string]dbus.Variant{}}, wantErr: ErrCancelled},
		{name: "denied", body: []interface{}{responseFailed, map[string]dbus.Variant{}}, wantErr: ErrDenied},
		{name: "unknown code treated as denied", body: []interface{}{uint32(7), map[string]dbus.Variant{}}, wantErr: ErrDenied},
		{name: "wrong body shape", body: []interface{}{"nope"}, wantErr: ErrInvalidResponse},
		{name: "no uri", body: []interface{}{responseSuccess, map[string]dbus.Variant{}}, wantErr: ErrInvalidResponse},
		{
			name:    "uri not a string",
			body:    []interface{}{responseSuccess, map[string]dbus.Variant{"uri": dbus.MakeVariant(uint32(1))}},
			wantErr: ErrInvalidResponse,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := screenshotURI(&dbus.Signal{Name: requestIface + ".Response", Body: tt.body})
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("screenshotURI() error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("screenshotURI() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("screenshotURI() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestScreenshotURI_CancelledIsNotDenied guards the distinction the caller
// relies on: dismissing the dialog is not a denial.
func TestScreenshotURI_CancelledIsNotDenied(t *testing.T) {
	_, err := screenshotURI(&dbus.Signal{Body: []interface{}{responseCancelled, map[string]dbus.Variant{}}})
	if errors.Is(err, ErrDenied) {
		t.Errorf("cancelled response also matches ErrDenied: %v", err)
	}
	_, err = screenshotURI(&dbus.Signal{Body: []interface{}{responseFailed, map[string]dbus.Variant{}}})
	if errors.Is(err, ErrCancelled) {
		t.Errorf("denied response also matches ErrCancelled: %v", err)
	}
}

func TestFileURIToPath(t *testing.T) {
	tests := []struct {
		name    string
		uri     string
		want    string
		wantErr bool
	}{
		{name: "plain path", uri: "file:///home/u/Pictures/Screenshot.png", want: "/home/u/Pictures/Screenshot.png"},
		{name: "percent decoded", uri: "file:///home/u/My%20Pictures/a%20b.png", want: "/home/u/My Pictures/a b.png"},
		{name: "localhost host allowed", uri: "file://localhost/tmp/a.png", want: "/tmp/a.png"},
		{name: "document portal path", uri: "file:///run/user/1000/doc/abcd1234/Screenshot.png", want: "/run/user/1000/doc/abcd1234/Screenshot.png"},
		{name: "cleaned", uri: "file:///tmp/./sub/../a.png", want: "/tmp/a.png"},
		{name: "empty", uri: "", wantErr: true},
		{name: "wrong scheme", uri: "http://example.com/a.png", wantErr: true},
		{name: "remote host", uri: "file://nas/tmp/a.png", wantErr: true},
		{name: "relative path", uri: "file:a.png", wantErr: true},
		{name: "bare path without scheme", uri: "/tmp/a.png", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := fileURIToPath(tt.uri)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("fileURIToPath(%q) = %q, want error", tt.uri, got)
				}
				if !errors.Is(err, ErrInvalidResponse) {
					t.Errorf("error = %v, want ErrInvalidResponse", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("fileURIToPath(%q) error = %v", tt.uri, err)
			}
			if got != tt.want {
				t.Errorf("fileURIToPath(%q) = %q, want %q", tt.uri, got, tt.want)
			}
		})
	}
}

// ==================== file handling ====================

func TestReadAndRemoveImage(t *testing.T) {
	path, uri := writePNG(t, "shot.png", 4, 3)

	img, err := readAndRemoveImage(uri)
	if err != nil {
		t.Fatalf("readAndRemoveImage() error = %v", err)
	}
	if got, want := img.Bounds(), image.Rect(0, 0, 4, 3); got != want {
		t.Errorf("bounds = %v, want %v", got, want)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("file %s still exists after read (stat err = %v)", path, err)
	}
}

func TestReadAndRemoveImage_RemovesOnDecodeError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notanimage.png")
	if err := os.WriteFile(path, []byte("not a png"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	_, err := readAndRemoveImage("file://" + path)
	if !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("error = %v, want ErrInvalidResponse", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("file %s still exists after a failed decode (stat err = %v)", path, err)
	}
}

func TestReadAndRemoveImage_MissingFile(t *testing.T) {
	_, err := readAndRemoveImage("file://" + filepath.Join(t.TempDir(), "gone.png"))
	if !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("error = %v, want ErrInvalidResponse", err)
	}
}

func TestToRGBA(t *testing.T) {
	// A non-RGBA image with a non-zero origin must come back as RGBA at (0,0).
	src := image.NewNRGBA(image.Rect(10, 20, 14, 23))
	got := toRGBA(src)
	if want := image.Rect(0, 0, 4, 3); got.Bounds() != want {
		t.Errorf("bounds = %v, want %v", got.Bounds(), want)
	}

	rgba := image.NewRGBA(image.Rect(0, 0, 2, 2))
	if toRGBA(rgba) != rgba {
		t.Error("toRGBA() copied an *image.RGBA that was already at the origin")
	}
}

// ==================== Capture ====================

func TestCapture_Success(t *testing.T) {
	_, uri := writePNG(t, "shot.png", 6, 5)
	fb := newFakeBus()
	fb.onCall = respondWith(t, responseSuccess, map[string]dbus.Variant{"uri": dbus.MakeVariant(uri)})

	img, err := testClient(fb).Capture(context.Background(), Options{ParentWindow: "x11:0x2a00003"})
	if err != nil {
		t.Fatalf("Capture() error = %v", err)
	}
	if got, want := img.Bounds(), image.Rect(0, 0, 6, 5); got != want {
		t.Errorf("bounds = %v, want %v", got, want)
	}
	if path, _ := fileURIToPath(uri); path != "" {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("portal file %s not deleted (stat err = %v)", path, err)
		}
	}

	calls, events, chansGone := fb.snapshot()
	if len(calls) != 1 {
		t.Fatalf("calls = %+v, want one Screenshot call", calls)
	}
	if calls[0].Path != desktopPath || calls[0].Method != screenshotIface+".Screenshot" {
		t.Errorf("call = %s %s, want %s %s.Screenshot", calls[0].Path, calls[0].Method, desktopPath, screenshotIface)
	}
	if parent, ok := calls[0].Args[0].(string); !ok || parent != "x11:0x2a00003" {
		t.Errorf("parent_window = %#v, want \"x11:0x2a00003\"", calls[0].Args[0])
	}
	// The Response subscription must be in place before the method call, or a
	// fast reply is lost.
	wantPrefix := []string{"signal-chan", "match", "call:" + screenshotIface + ".Screenshot"}
	if len(events) < len(wantPrefix) {
		t.Fatalf("events = %v, want at least %v", events, wantPrefix)
	}
	for i, e := range wantPrefix {
		if events[i] != e {
			t.Fatalf("events = %v, want prefix %v", events, wantPrefix)
		}
	}
	if _, removed := fb.matchCounts(); removed != 1 {
		t.Errorf("removed %d match rules, want 1", removed)
	}
	if chansGone != 1 {
		t.Errorf("removed %d signal channels, want 1", chansGone)
	}
}

func TestCapture_UserCancelled(t *testing.T) {
	fb := newFakeBus()
	fb.onCall = respondWith(t, responseCancelled, map[string]dbus.Variant{})

	_, err := testClient(fb).Capture(context.Background(), Options{})
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("Capture() error = %v, want ErrCancelled", err)
	}
	if errors.Is(err, ErrDenied) {
		t.Error("cancelled Capture also matches ErrDenied")
	}
}

func TestCapture_Denied(t *testing.T) {
	fb := newFakeBus()
	fb.onCall = respondWith(t, responseFailed, map[string]dbus.Variant{})

	_, err := testClient(fb).Capture(context.Background(), Options{})
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("Capture() error = %v, want ErrDenied", err)
	}
	if errors.Is(err, ErrCancelled) {
		t.Error("denied Capture also matches ErrCancelled")
	}
}

// TestCapture_TimesOut is the bug this package exists to avoid: a portal that
// never answers must not block the caller.
func TestCapture_TimesOut(t *testing.T) {
	fb := newFakeBus()
	fb.onCall = func(fb *fakeBus, c fakeCall) *dbus.Call {
		if c.Method != screenshotIface+".Screenshot" {
			return &dbus.Call{}
		}
		return &dbus.Call{Body: []interface{}{handleFromCall(t, fb, c)}}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	c := testClient(fb)
	c.reapTimeout = 100 * time.Millisecond // so the trailing Close is quick to observe

	start := time.Now()
	_, err := c.Capture(ctx, Options{})
	elapsed := time.Since(start)

	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("Capture() error = %v, want ErrTimeout", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Capture() error = %v, want it to also match context.DeadlineExceeded", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("Capture() took %s, want it to return at the deadline", elapsed)
	}
	// Nothing is closed yet: closing suppresses the Response that names the
	// file the portal writes anyway.
	calls, _, _ := fb.snapshot()
	if len(calls) != 1 {
		t.Errorf("calls = %+v, want only the Screenshot call at this point", calls)
	}
	// Once nothing has answered for the reap timeout, the request is closed so its
	// dialog does not linger.
	if !eventually(t, 2*time.Second, func() bool {
		calls, _, _ := fb.snapshot()
		return len(calls) == 2 && calls[1].Method == requestIface+".Close" && calls[1].Path == handleFromCall(t, fb, calls[0])
	}) {
		calls, _, _ := fb.snapshot()
		t.Errorf("calls = %+v, want a trailing %s.Close on the request path", calls, requestIface)
	}
}

// TestCapture_DefaultDeadline covers a context with no deadline of its own.
func TestCapture_DefaultDeadline(t *testing.T) {
	fb := newFakeBus()
	fb.onCall = func(fb *fakeBus, c fakeCall) *dbus.Call {
		if c.Method != screenshotIface+".Screenshot" {
			return &dbus.Call{}
		}
		return &dbus.Call{Body: []interface{}{handleFromCall(t, fb, c)}}
	}

	c := &Client{bus: fb, Timeout: 80 * time.Millisecond}
	start := time.Now()
	_, err := c.Capture(context.Background(), Options{})
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("Capture() error = %v, want ErrTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Capture() took %s, want it to honour Client.Timeout", elapsed)
	}
}

func TestCapture_ContextCancelled(t *testing.T) {
	fb := newFakeBus()
	fb.onCall = func(fb *fakeBus, c fakeCall) *dbus.Call {
		if c.Method != screenshotIface+".Screenshot" {
			return &dbus.Call{}
		}
		return &dbus.Call{Body: []interface{}{handleFromCall(t, fb, c)}}
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	_, err := testClient(fb).Capture(ctx, Options{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Capture() error = %v, want context.Canceled", err)
	}
	// Caller cancellation is not the user dismissing a dialog.
	if errors.Is(err, ErrCancelled) || errors.Is(err, ErrTimeout) {
		t.Errorf("Capture() error = %v, want it distinct from ErrCancelled/ErrTimeout", err)
	}
}

// TestCapture_IgnoresOtherSignals checks the client filters by request path, so
// another request's Response cannot be mistaken for ours.
func TestCapture_IgnoresOtherSignals(t *testing.T) {
	_, uri := writePNG(t, "shot.png", 2, 2)
	fb := newFakeBus()
	fb.onCall = func(fb *fakeBus, c fakeCall) *dbus.Call {
		if c.Method != screenshotIface+".Screenshot" {
			return &dbus.Call{}
		}
		handle := handleFromCall(t, fb, c)
		go func() {
			// Foreign request, then a different member on our own path.
			fb.emit(&dbus.Signal{
				Sender: busName,
				Path:   "/org/freedesktop/portal/desktop/request/1_42/someone_else",
				Name:   requestIface + ".Response",
				Body:   []interface{}{responseFailed, map[string]dbus.Variant{}},
			})
			fb.emit(&dbus.Signal{Sender: busName, Path: handle, Name: requestIface + ".Closed"})
			fb.emit(&dbus.Signal{
				Sender: busName,
				Path:   handle,
				Name:   requestIface + ".Response",
				Body:   []interface{}{responseSuccess, map[string]dbus.Variant{"uri": dbus.MakeVariant(uri)}},
			})
		}()
		return &dbus.Call{Body: []interface{}{handle}}
	}

	img, err := testClient(fb).Capture(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Capture() error = %v", err)
	}
	if got, want := img.Bounds(), image.Rect(0, 0, 2, 2); got != want {
		t.Errorf("bounds = %v, want %v", got, want)
	}
}

// TestCapture_HandleMismatch covers a portal that returns a handle other than
// the derived one: the client subscribes to it as well.
func TestCapture_HandleMismatch(t *testing.T) {
	_, uri := writePNG(t, "shot.png", 3, 3)
	actual := dbus.ObjectPath("/org/freedesktop/portal/desktop/request/legacy_handle")
	fb := newFakeBus()
	fb.onCall = func(fb *fakeBus, c fakeCall) *dbus.Call {
		if c.Method != screenshotIface+".Screenshot" {
			return &dbus.Call{}
		}
		go func() {
			time.Sleep(10 * time.Millisecond)
			fb.emit(&dbus.Signal{
				Sender: busName,
				Path:   actual,
				Name:   requestIface + ".Response",
				Body:   []interface{}{responseSuccess, map[string]dbus.Variant{"uri": dbus.MakeVariant(uri)}},
			})
		}()
		return &dbus.Call{Body: []interface{}{actual}}
	}

	if _, err := testClient(fb).Capture(context.Background(), Options{}); err != nil {
		t.Fatalf("Capture() error = %v", err)
	}
	added, removed := fb.matchCounts()
	if added != 2 {
		t.Fatalf("added %d match rules, want 2 (derived path and returned handle)", added)
	}
	if removed != 2 {
		t.Errorf("removed %d match rules, want 2", removed)
	}
}

func TestCapture_PortalMissing(t *testing.T) {
	fb := newFakeBus()
	fb.onCall = func(fb *fakeBus, c fakeCall) *dbus.Call {
		return &dbus.Call{Err: dbus.Error{Name: "org.freedesktop.DBus.Error.ServiceUnknown"}}
	}

	_, err := testClient(fb).Capture(context.Background(), Options{})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Capture() error = %v, want ErrUnavailable", err)
	}
	assertReleasedSynchronously(t, fb)
}

func TestCapture_MethodError(t *testing.T) {
	fb := newFakeBus()
	fb.onCall = func(fb *fakeBus, c fakeCall) *dbus.Call {
		return &dbus.Call{Err: dbus.Error{Name: "org.freedesktop.portal.Error.NotAllowed"}}
	}

	_, err := testClient(fb).Capture(context.Background(), Options{})
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("Capture() error = %v, want ErrDenied", err)
	}
	assertReleasedSynchronously(t, fb)

	// No reaper is listening, so a late response must not delete anything: a
	// request the portal rejected outright never produced a file, and deleting
	// on someone else's behalf would be worse than leaking.
	path, uri := writePNG(t, "notours.png", 1, 1)
	calls, _, _ := fb.snapshot()
	fb.emit(&dbus.Signal{
		Sender: busName,
		Path:   handleFromCall(t, fb, calls[0]),
		Name:   requestIface + ".Response",
		Body:   []interface{}{responseSuccess, map[string]dbus.Variant{"uri": dbus.MakeVariant(uri)}},
	})
	time.Sleep(50 * time.Millisecond)
	if _, err := os.Stat(path); err != nil {
		t.Errorf("a late response was acted on after Capture failed outright: %v", err)
	}
}

func TestCapture_SubscribeError(t *testing.T) {
	fb := newFakeBus()
	fb.addMatchErr = errors.New("bus refused match rule")

	_, err := testClient(fb).Capture(context.Background(), Options{})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Capture() error = %v, want ErrUnavailable", err)
	}
	if calls, _, _ := fb.snapshot(); len(calls) != 0 {
		t.Errorf("calls = %+v, want none when the subscription failed", calls)
	}
}

func TestCapture_ConnectionDropped(t *testing.T) {
	fb := newFakeBus()
	fb.onCall = func(fb *fakeBus, c fakeCall) *dbus.Call {
		if c.Method != screenshotIface+".Screenshot" {
			return &dbus.Call{}
		}
		handle := handleFromCall(t, fb, c)
		go func() {
			time.Sleep(10 * time.Millisecond)
			fb.closeChans()
		}()
		return &dbus.Call{Body: []interface{}{handle}}
	}

	_, err := testClient(fb).Capture(context.Background(), Options{})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Capture() error = %v, want ErrUnavailable", err)
	}
}

func TestCapture_BadUniqueName(t *testing.T) {
	fb := newFakeBus()
	fb.name = "org.example.NotUnique"

	_, err := testClient(fb).Capture(context.Background(), Options{})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Capture() error = %v, want ErrUnavailable", err)
	}
}

// ==================== Version ====================

func TestVersion(t *testing.T) {
	fb := newFakeBus()
	fb.onCall = func(fb *fakeBus, c fakeCall) *dbus.Call {
		if c.Method != propertiesIface+".Get" {
			t.Errorf("method = %q, want %q", c.Method, propertiesIface+".Get")
		}
		if len(c.Args) != 2 || c.Args[0] != screenshotIface || c.Args[1] != "version" {
			t.Errorf("args = %#v, want [%q version]", c.Args, screenshotIface)
		}
		return &dbus.Call{Body: []interface{}{dbus.MakeVariant(uint32(2))}}
	}

	got, err := testClient(fb).Version(context.Background())
	if err != nil {
		t.Fatalf("Version() error = %v", err)
	}
	if got != 2 {
		t.Errorf("Version() = %d, want 2", got)
	}
}

func TestVersion_PortalMissing(t *testing.T) {
	fb := newFakeBus()
	fb.onCall = func(fb *fakeBus, c fakeCall) *dbus.Call {
		return &dbus.Call{Err: dbus.Error{Name: "org.freedesktop.DBus.Error.ServiceUnknown"}}
	}

	if _, err := testClient(fb).Version(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Version() error = %v, want ErrUnavailable", err)
	}
}

func TestVersion_WrongType(t *testing.T) {
	fb := newFakeBus()
	fb.onCall = func(fb *fakeBus, c fakeCall) *dbus.Call {
		return &dbus.Call{Body: []interface{}{dbus.MakeVariant("two")}}
	}

	if _, err := testClient(fb).Version(context.Background()); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("Version() error = %v, want ErrInvalidResponse", err)
	}
}

// ==================== sentinels ====================

// TestSentinelsAreDistinct guards requirement that outcomes are told apart by
// sentinel, not by error text.
func TestSentinelsAreDistinct(t *testing.T) {
	all := []error{ErrCancelled, ErrDenied, ErrTimeout, ErrUnavailable, ErrInvalidResponse}
	for i, a := range all {
		for j, b := range all {
			if i != j && errors.Is(a, b) {
				t.Errorf("sentinel %v matches %v", a, b)
			}
		}
		if !strings.HasPrefix(a.Error(), "portal: ") {
			t.Errorf("sentinel message %q should be namespaced", a.Error())
		}
	}
}

func TestClose(t *testing.T) {
	fb := newFakeBus()
	if err := testClient(fb).Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if !fb.closed {
		t.Error("Close() did not close the bus connection")
	}
}

// TestCapture_ReapsFileAfterTimeout covers the measured behaviour that
// Request.Close does not stop a capture already in flight: the portal writes the
// file anyway, so the abandoned request must still be drained and the file
// deleted.
func TestCapture_ReapsFileAfterTimeout(t *testing.T) {
	path, uri := writePNG(t, "late.png", 2, 2)
	fb := newFakeBus()
	var handle dbus.ObjectPath
	var handleOnce sync.Once
	done := make(chan struct{})
	fb.onCall = func(fb *fakeBus, c fakeCall) *dbus.Call {
		if c.Method != screenshotIface+".Screenshot" {
			return &dbus.Call{}
		}
		h := handleFromCall(t, fb, c)
		handleOnce.Do(func() { handle = h; close(done) })
		return &dbus.Call{Body: []interface{}{h}}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	if _, err := testClient(fb).Capture(ctx, Options{}); !errors.Is(err, ErrTimeout) {
		t.Fatalf("Capture() error = %v, want ErrTimeout", err)
	}

	<-done
	// The portal answers after Capture has given up.
	fb.emit(&dbus.Signal{
		Sender: busName,
		Path:   handle,
		Name:   requestIface + ".Response",
		Body:   []interface{}{responseSuccess, map[string]dbus.Variant{"uri": dbus.MakeVariant(uri)}},
	})

	if !eventually(t, 2*time.Second, func() bool {
		_, err := os.Stat(path)
		return os.IsNotExist(err)
	}) {
		t.Errorf("late portal file %s was not deleted", path)
	}
	// Taking the subscription over must not leak it.
	if !eventually(t, 2*time.Second, func() bool {
		_, _, gone := fb.snapshot()
		return gone == 1
	}) {
		_, _, gone := fb.snapshot()
		t.Errorf("removed %d signal channels after reaping, want 1", gone)
	}
}

// TestCapture_ReaperUnsubscribesAfterDenial checks the reaper stops on a late
// non-success response, where there is no file to remove.
func TestCapture_ReaperUnsubscribesAfterDenial(t *testing.T) {
	fb := newFakeBus()
	var handle dbus.ObjectPath
	var handleOnce sync.Once
	done := make(chan struct{})
	fb.onCall = func(fb *fakeBus, c fakeCall) *dbus.Call {
		if c.Method != screenshotIface+".Screenshot" {
			return &dbus.Call{}
		}
		h := handleFromCall(t, fb, c)
		handleOnce.Do(func() { handle = h; close(done) })
		return &dbus.Call{Body: []interface{}{h}}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	if _, err := testClient(fb).Capture(ctx, Options{}); !errors.Is(err, ErrTimeout) {
		t.Fatalf("Capture() error = %v, want ErrTimeout", err)
	}

	<-done
	fb.emit(&dbus.Signal{
		Sender: busName,
		Path:   handle,
		Name:   requestIface + ".Response",
		Body:   []interface{}{responseFailed, map[string]dbus.Variant{}},
	})

	if !eventually(t, 2*time.Second, func() bool {
		_, _, gone := fb.snapshot()
		return gone == 1
	}) {
		t.Error("reaper did not release the subscription after a late denial")
	}
}

func eventually(t *testing.T, within time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

// TestCapture_ReapsFileWhenMethodReplyTimesOut covers the deadline expiring
// before the Screenshot reply arrives: the handle is unknown, but the derived
// path is the one the portal uses, so the file can still be reclaimed.
func TestCapture_ReapsFileWhenMethodReplyTimesOut(t *testing.T) {
	path, uri := writePNG(t, "late.png", 2, 2)
	fb := newFakeBus()
	fb.onCall = func(fb *fakeBus, c fakeCall) *dbus.Call {
		if c.Method != screenshotIface+".Screenshot" {
			return &dbus.Call{}
		}
		time.Sleep(150 * time.Millisecond) // reply arrives after the deadline
		return &dbus.Call{Err: context.DeadlineExceeded}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	if _, err := testClient(fb).Capture(ctx, Options{}); !errors.Is(err, ErrTimeout) {
		t.Fatalf("Capture() error = %v, want ErrTimeout", err)
	}

	calls, _, _ := fb.snapshot()
	if len(calls) == 0 {
		t.Fatal("no Screenshot call recorded")
	}
	derived := handleFromCall(t, fb, calls[0])
	fb.emit(&dbus.Signal{
		Sender: busName,
		Path:   derived,
		Name:   requestIface + ".Response",
		Body:   []interface{}{responseSuccess, map[string]dbus.Variant{"uri": dbus.MakeVariant(uri)}},
	})

	if !eventually(t, 2*time.Second, func() bool {
		_, err := os.Stat(path)
		return os.IsNotExist(err)
	}) {
		t.Errorf("late portal file %s was not deleted", path)
	}
}

// assertReleasedSynchronously checks the subscription was released before
// Capture returned, which also means no reaper goroutine was left behind.
func assertReleasedSynchronously(t *testing.T, fb *fakeBus) {
	t.Helper()
	if _, _, gone := fb.snapshot(); gone != 1 {
		t.Errorf("released %d signal channels before Capture returned, want 1", gone)
	}
	calls, _, _ := fb.snapshot()
	for _, c := range calls {
		if c.Method == requestIface+".Close" {
			t.Errorf("calls = %+v, want no %s.Close: nothing was pending", calls, requestIface)
		}
	}
}

// TestCapture_Concurrent exercises the concurrency the Client doc comment
// promises: one client, several captures at once, each getting its own image.
func TestCapture_Concurrent(t *testing.T) {
	const n = 8
	dir := t.TempDir()
	fb := newFakeBus()
	fb.onCall = func(fb *fakeBus, c fakeCall) *dbus.Call {
		if c.Method != screenshotIface+".Screenshot" {
			return &dbus.Call{}
		}
		handle := handleFromCall(t, fb, c)
		// One file per request, named after its own handle, so a client that
		// mixed two requests up would read the wrong size or a deleted file.
		width := 1 + len(handle)%16
		path := filepath.Join(dir, filepath.Base(string(handle))+".png")
		f, err := os.Create(path)
		if err != nil {
			t.Errorf("create %s: %v", path, err)
			return &dbus.Call{Err: errors.New("cannot create file")}
		}
		if err := png.Encode(f, image.NewRGBA(image.Rect(0, 0, width, 2))); err != nil {
			t.Errorf("encode %s: %v", path, err)
		}
		_ = f.Close()

		go fb.emit(&dbus.Signal{
			Sender: busName,
			Path:   handle,
			Name:   requestIface + ".Response",
			Body:   []interface{}{responseSuccess, map[string]dbus.Variant{"uri": dbus.MakeVariant("file://" + path)}},
		})
		return &dbus.Call{Body: []interface{}{handle}}
	}

	c := testClient(fb)
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			img, err := c.Capture(context.Background(), Options{})
			if err != nil {
				errs <- err
				return
			}
			if img.Bounds().Dy() != 2 || img.Bounds().Dx() < 1 {
				errs <- fmt.Errorf("unexpected bounds %v", img.Bounds())
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent Capture() error = %v", err)
	}

	calls, _, gone := fb.snapshot()
	if len(calls) != n {
		t.Errorf("recorded %d calls, want %d", len(calls), n)
	}
	if gone != n {
		t.Errorf("released %d signal channels, want %d", gone, n)
	}
	// Every request must have used a distinct handle token, and every file must
	// be gone.
	tokens := make(map[dbus.ObjectPath]bool, n)
	for _, call := range calls {
		h := handleFromCall(t, fb, call)
		if tokens[h] {
			t.Errorf("handle %s reused across concurrent captures", h)
		}
		tokens[h] = true
	}
	left, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(left) != 0 {
		t.Errorf("%d portal files left behind, want 0", len(left))
	}
}

// TestCapture_ReapsFileAfterHandleMismatch covers the legacy branch: the reaper
// has to listen on the handle the portal returned, not the derived one.
func TestCapture_ReapsFileAfterHandleMismatch(t *testing.T) {
	path, uri := writePNG(t, "legacy.png", 2, 2)
	actual := dbus.ObjectPath("/org/freedesktop/portal/desktop/request/legacy_handle")
	fb := newFakeBus()
	fb.onCall = func(fb *fakeBus, c fakeCall) *dbus.Call {
		if c.Method != screenshotIface+".Screenshot" {
			return &dbus.Call{}
		}
		return &dbus.Call{Body: []interface{}{actual}}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	if _, err := testClient(fb).Capture(ctx, Options{}); !errors.Is(err, ErrTimeout) {
		t.Fatalf("Capture() error = %v, want ErrTimeout", err)
	}

	fb.emit(&dbus.Signal{
		Sender: busName,
		Path:   actual,
		Name:   requestIface + ".Response",
		Body:   []interface{}{responseSuccess, map[string]dbus.Variant{"uri": dbus.MakeVariant(uri)}},
	})

	if !eventually(t, 2*time.Second, func() bool {
		_, err := os.Stat(path)
		return os.IsNotExist(err)
	}) {
		t.Errorf("late portal file %s was not deleted after a handle mismatch", path)
	}
	// Both match rules must come back off the bus.
	if !eventually(t, 2*time.Second, func() bool {
		_, removed := fb.matchCounts()
		return removed == 2
	}) {
		_, removed := fb.matchCounts()
		t.Errorf("removed %d match rules, want 2", removed)
	}
}

// TestClose_WaitsForReaper is the reason Close is not a plain bus.Close:
// closing the connection closes the signal channels, so a Close racing a reaper
// would lose the file the portal wrote.
func TestClose_WaitsForReaper(t *testing.T) {
	path, uri := writePNG(t, "late.png", 2, 2)
	fb := newFakeBus()
	responded := make(chan struct{})
	fb.onCall = func(fb *fakeBus, c fakeCall) *dbus.Call {
		if c.Method != screenshotIface+".Screenshot" {
			return &dbus.Call{}
		}
		handle := handleFromCall(t, fb, c)
		go func() {
			// Answers well after Capture gave up, while Close is waiting.
			time.Sleep(150 * time.Millisecond)
			fb.emit(&dbus.Signal{
				Sender: busName,
				Path:   handle,
				Name:   requestIface + ".Response",
				Body:   []interface{}{responseSuccess, map[string]dbus.Variant{"uri": dbus.MakeVariant(uri)}},
			})
			close(responded)
		}()
		return &dbus.Call{Body: []interface{}{handle}}
	}

	c := testClient(fb)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := c.Capture(ctx, Options{}); !errors.Is(err, ErrTimeout) {
		t.Fatalf("Capture() error = %v, want ErrTimeout", err)
	}

	if err := c.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	<-responded
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("Close() returned before the reaper deleted %s (stat err = %v)", path, err)
	}
	if !fb.closed {
		t.Error("Close() did not close the bus connection")
	}
}

// TestCapture_DeclinesReaperWhileClosing checks a Capture abandoned during Close
// cleans up itself instead of handing work to a connection that is going away.
func TestCapture_DeclinesReaperWhileClosing(t *testing.T) {
	fb := newFakeBus()
	fb.onCall = func(fb *fakeBus, c fakeCall) *dbus.Call {
		if c.Method != screenshotIface+".Screenshot" {
			return &dbus.Call{}
		}
		return &dbus.Call{Body: []interface{}{handleFromCall(t, fb, c)}}
	}

	c := testClient(fb)
	if err := c.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := c.Capture(ctx, Options{}); !errors.Is(err, ErrTimeout) {
		t.Fatalf("Capture() error = %v, want ErrTimeout", err)
	}
	assertReleasedSynchronously(t, fb)
}

// TestCapture_SubscribeErrorFromCancelledContext guards against reporting the
// caller's own cancellation as an unavailable portal.
func TestCapture_SubscribeErrorFromCancelledContext(t *testing.T) {
	fb := newFakeBus()
	fb.addMatchErr = context.Canceled

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := testClient(fb).Capture(ctx, Options{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Capture() error = %v, want context.Canceled", err)
	}
	if errors.Is(err, ErrUnavailable) {
		t.Errorf("Capture() error = %v also matches ErrUnavailable", err)
	}
}

package platform

import (
	"errors"
	"image"
	"image/color"
	"testing"
)

// screenPixel encodes a screen coordinate as a colour, so a crop can be checked
// pixel by pixel against the coordinates it should have come from. The +128
// keeps small negative coordinates distinct.
func screenPixel(x, y int) color.RGBA {
	return color.RGBA{R: uint8(x + 128), G: uint8(y + 128), B: 0x40, A: 0xFF}
}

// newVirtualScreen paints an image of size bounds where every pixel encodes the
// screen coordinate it stands for. virtual is the screen-coordinate rectangle
// the image covers; bounds is the image's own rectangle, which is usually at the
// origin even when virtual is not.
func newVirtualScreen(virtual, bounds image.Rectangle) *image.RGBA {
	img := image.NewRGBA(bounds)
	offset := bounds.Min.Sub(virtual.Min)
	for sy := virtual.Min.Y; sy < virtual.Max.Y; sy++ {
		for sx := virtual.Min.X; sx < virtual.Max.X; sx++ {
			img.SetRGBA(sx+offset.X, sy+offset.Y, screenPixel(sx, sy))
		}
	}
	return img
}

func TestVirtualScreenBounds(t *testing.T) {
	tests := []struct {
		name     string
		displays []image.Rectangle
		want     image.Rectangle
	}{
		{
			name: "no displays",
			want: image.Rectangle{},
		},
		{
			name:     "single display",
			displays: []image.Rectangle{image.Rect(0, 0, 4, 3)},
			want:     image.Rect(0, 0, 4, 3),
		},
		{
			name:     "secondary left of and above primary",
			displays: []image.Rectangle{image.Rect(0, 0, 4, 3), image.Rect(-5, -2, 0, 3)},
			want:     image.Rect(-5, -2, 4, 3),
		},
		{
			name:     "empty display contributes nothing",
			displays: []image.Rectangle{image.Rect(0, 0, 4, 3), {}},
			want:     image.Rect(0, 0, 4, 3),
		},
		{
			name:     "mirrored displays",
			displays: []image.Rectangle{image.Rect(0, 0, 4, 3), image.Rect(0, 0, 4, 3)},
			want:     image.Rect(0, 0, 4, 3),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := VirtualScreenBounds(tt.displays); got != tt.want {
				t.Errorf("VirtualScreenBounds(%v) = %s, want %s", tt.displays, got, tt.want)
			}
		})
	}
}

func TestDisplayAtCursor(t *testing.T) {
	// Primary, one to its right, one above-left with negative coordinates.
	displays := []image.Rectangle{
		image.Rect(0, 0, 4, 3),
		image.Rect(4, 0, 8, 3),
		image.Rect(-4, -3, 0, 0),
	}
	tests := []struct {
		name   string
		cursor image.Point
		want   int
	}{
		{name: "inside primary", cursor: image.Pt(1, 1), want: 0},
		{name: "primary top-left corner is inside", cursor: image.Pt(0, 0), want: 0},
		{name: "boundary pixel belongs to the display on the right", cursor: image.Pt(4, 1), want: 1},
		{name: "last pixel before the boundary is still the primary", cursor: image.Pt(3, 1), want: 0},
		{name: "negative coordinates", cursor: image.Pt(-2, -2), want: 2},
		{name: "bottom-right corner belongs to no display", cursor: image.Pt(8, 3), want: -1},
		{name: "gap between displays", cursor: image.Pt(-1, 1), want: -1},
		{name: "far outside", cursor: image.Pt(1000, 1000), want: -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DisplayAtCursor(displays, tt.cursor); got != tt.want {
				t.Errorf("DisplayAtCursor(%v) = %d, want %d", tt.cursor, got, tt.want)
			}
		})
	}

	t.Run("no displays", func(t *testing.T) {
		if got := DisplayAtCursor(nil, image.Pt(0, 0)); got != -1 {
			t.Errorf("DisplayAtCursor(nil) = %d, want -1", got)
		}
	})
	t.Run("mirrored displays pick the lowest index", func(t *testing.T) {
		mirrored := []image.Rectangle{image.Rect(0, 0, 4, 3), image.Rect(0, 0, 4, 3)}
		if got := DisplayAtCursor(mirrored, image.Pt(2, 2)); got != 0 {
			t.Errorf("DisplayAtCursor() = %d, want 0", got)
		}
	})
}

func TestCropDisplayAtCursor(t *testing.T) {
	tests := []struct {
		name     string
		displays []image.Rectangle
		cursor   image.Point
		// imgMin is the image's own origin; imgSize overrides its size when
		// non-zero, to feed in an image that disagrees with the geometry.
		imgMin   image.Point
		imgSize  image.Point
		nilImage bool
		wantIdx  int
		wantErr  error
	}{
		{
			name:     "single display returns the whole image",
			displays: []image.Rectangle{image.Rect(0, 0, 4, 3)},
			cursor:   image.Pt(2, 1),
			wantIdx:  0,
		},
		{
			name:     "cursor on the secondary display",
			displays: []image.Rectangle{image.Rect(0, 0, 4, 3), image.Rect(4, 0, 9, 3)},
			cursor:   image.Pt(6, 2),
			wantIdx:  1,
		},
		{
			name:     "secondary left of the primary has a negative origin",
			displays: []image.Rectangle{image.Rect(0, 0, 4, 3), image.Rect(-5, 0, 0, 3)},
			cursor:   image.Pt(-3, 1),
			wantIdx:  1,
		},
		{
			name:     "secondary above the primary has a negative origin",
			displays: []image.Rectangle{image.Rect(0, 0, 4, 3), image.Rect(0, -2, 4, 0)},
			cursor:   image.Pt(1, -1),
			wantIdx:  1,
		},
		{
			name:     "primary is not at the virtual screen origin",
			displays: []image.Rectangle{image.Rect(0, 0, 4, 3), image.Rect(-5, -2, 0, 3)},
			cursor:   image.Pt(2, 2),
			wantIdx:  0,
		},
		{
			name:     "cursor exactly on the boundary crops the display on the right",
			displays: []image.Rectangle{image.Rect(0, 0, 4, 3), image.Rect(4, 0, 8, 3)},
			cursor:   image.Pt(4, 0),
			wantIdx:  1,
		},
		{
			name:     "cursor one pixel before the boundary crops the primary",
			displays: []image.Rectangle{image.Rect(0, 0, 4, 3), image.Rect(4, 0, 8, 3)},
			cursor:   image.Pt(3, 0),
			wantIdx:  0,
		},
		{
			name:     "cursor outside every display falls back to display 0",
			displays: []image.Rectangle{image.Rect(0, 0, 4, 3), image.Rect(5, 0, 9, 3)},
			cursor:   image.Pt(4, 1), // the gap between the two displays
			wantIdx:  0,
		},
		{
			name:     "cursor on the exclusive bottom-right corner falls back to display 0",
			displays: []image.Rectangle{image.Rect(0, 0, 4, 3), image.Rect(4, 0, 8, 3)},
			cursor:   image.Pt(8, 3),
			wantIdx:  0,
		},
		{
			name:     "mirrored displays crop the lowest index",
			displays: []image.Rectangle{image.Rect(0, 0, 4, 3), image.Rect(0, 0, 4, 3)},
			cursor:   image.Pt(1, 1),
			wantIdx:  0,
		},
		{
			name:     "image whose bounds do not start at the origin",
			displays: []image.Rectangle{image.Rect(0, 0, 4, 3), image.Rect(4, 0, 8, 3)},
			cursor:   image.Pt(5, 1),
			imgMin:   image.Pt(7, 11),
			wantIdx:  1,
		},
		{
			name:     "no displays",
			displays: nil,
			cursor:   image.Pt(0, 0),
			wantErr:  ErrNoDisplays,
		},
		{
			name:     "nil image",
			displays: []image.Rectangle{image.Rect(0, 0, 4, 3)},
			cursor:   image.Pt(1, 1),
			nilImage: true,
			wantErr:  ErrGeometryMismatch,
		},
		{
			name:     "image smaller than the claimed geometry",
			displays: []image.Rectangle{image.Rect(0, 0, 4, 3), image.Rect(4, 0, 8, 3)},
			cursor:   image.Pt(1, 1),
			imgSize:  image.Pt(4, 3), // only the primary, not the virtual screen
			wantErr:  ErrGeometryMismatch,
		},
		{
			name:     "image larger than the claimed geometry",
			displays: []image.Rectangle{image.Rect(0, 0, 4, 3)},
			cursor:   image.Pt(1, 1),
			imgSize:  image.Pt(8, 3),
			wantErr:  ErrGeometryMismatch,
		},
		{
			name:     "image is the right width but the wrong height",
			displays: []image.Rectangle{image.Rect(0, 0, 4, 3)},
			cursor:   image.Pt(1, 1),
			imgSize:  image.Pt(4, 4),
			wantErr:  ErrGeometryMismatch,
		},
		{
			name:     "the only display is empty",
			displays: []image.Rectangle{{}},
			cursor:   image.Pt(0, 0),
			wantErr:  ErrGeometryMismatch,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			virtual := VirtualScreenBounds(tt.displays)
			size := image.Pt(virtual.Dx(), virtual.Dy())
			if tt.imgSize != (image.Point{}) {
				size = tt.imgSize
			}
			bounds := image.Rectangle{Min: tt.imgMin, Max: tt.imgMin.Add(size)}

			var img *image.RGBA
			if !tt.nilImage {
				img = newVirtualScreen(virtual, bounds)
			}

			got, idx, err := CropDisplayAtCursor(img, tt.displays, tt.cursor)

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("CropDisplayAtCursor() error = %v, want %v", err, tt.wantErr)
				}
				if got != nil {
					t.Errorf("CropDisplayAtCursor() image = %v, want nil on error", got.Bounds())
				}
				if idx != -1 {
					t.Errorf("CropDisplayAtCursor() index = %d, want -1 on error", idx)
				}
				return
			}
			if err != nil {
				t.Fatalf("CropDisplayAtCursor() error = %v", err)
			}
			if idx != tt.wantIdx {
				t.Fatalf("CropDisplayAtCursor() index = %d, want %d", idx, tt.wantIdx)
			}

			display := tt.displays[tt.wantIdx]
			wantBounds := image.Rect(0, 0, display.Dx(), display.Dy())
			if got.Bounds() != wantBounds {
				t.Fatalf("CropDisplayAtCursor() bounds = %s, want %s", got.Bounds(), wantBounds)
			}
			// Every pixel must be the one at the matching screen coordinate of
			// the cropped display.
			for y := 0; y < display.Dy(); y++ {
				for x := 0; x < display.Dx(); x++ {
					want := screenPixel(display.Min.X+x, display.Min.Y+y)
					if got := got.RGBAAt(x, y); got != want {
						t.Fatalf("crop at (%d,%d) = %v, want %v (screen %d,%d)",
							x, y, got, want, display.Min.X+x, display.Min.Y+y)
					}
				}
			}
		})
	}
}

// The crop must not alias the capture it came from: callers annotate these.
func TestCropDisplayAtCursor_CopiesPixels(t *testing.T) {
	displays := []image.Rectangle{image.Rect(0, 0, 4, 3), image.Rect(4, 0, 8, 3)}
	virtual := VirtualScreenBounds(displays)
	img := newVirtualScreen(virtual, image.Rect(0, 0, virtual.Dx(), virtual.Dy()))

	got, _, err := CropDisplayAtCursor(img, displays, image.Pt(5, 1))
	if err != nil {
		t.Fatalf("CropDisplayAtCursor() error = %v", err)
	}

	got.SetRGBA(0, 0, color.RGBA{A: 0xFF})
	if src := img.RGBAAt(4, 0); src != screenPixel(4, 0) {
		t.Errorf("writing to the crop changed the source at (4,0): %v, want %v", src, screenPixel(4, 0))
	}
}

// Errors must stay distinguishable from each other and from the package's other
// sentinel.
func TestCropSentinelsAreDistinct(t *testing.T) {
	if errors.Is(ErrNoDisplays, ErrGeometryMismatch) || errors.Is(ErrGeometryMismatch, ErrNoDisplays) {
		t.Error("ErrNoDisplays and ErrGeometryMismatch match each other")
	}
	for _, err := range []error{ErrNoDisplays, ErrGeometryMismatch} {
		if errors.Is(err, ErrUnsupported) {
			t.Errorf("%v matches ErrUnsupported", err)
		}
	}
}

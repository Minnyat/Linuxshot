package platform

import (
	"errors"
	"fmt"
	"image"
	"image/draw"
)

// Errors returned by CropDisplayAtCursor. Callers must distinguish them with
// errors.Is, never by matching error text.
var (
	// ErrNoDisplays is returned when the display geometry is empty, so there is
	// no display to crop to.
	ErrNoDisplays = errors.New("platform: no displays")

	// ErrGeometryMismatch is returned when the image is not the size the display
	// geometry describes, which means the two do not come from the same moment
	// and any crop derived from them would be off by an unknown amount.
	ErrGeometryMismatch = errors.New("platform: image bounds do not match display geometry")
)

// VirtualScreenBounds is the union of displays in screen coordinates: the
// rectangle a full-screen capture covers. It can start at a negative origin,
// because a monitor placed left of or above the primary one has negative
// coordinates. Empty rectangles in displays contribute nothing. The zero
// rectangle is returned for no displays.
func VirtualScreenBounds(displays []image.Rectangle) image.Rectangle {
	var union image.Rectangle
	for _, d := range displays {
		union = union.Union(d.Canon())
	}
	return union
}

// DisplayAtCursor returns the index in displays of the display the cursor is
// on, or -1 when the cursor is on none of them.
//
// Containment is half-open, like image.Rectangle: the top-left corner belongs
// to a display and the bottom-right corner does not, so adjacent monitors
// cannot both claim the same pixel. When displays overlap - mirrored outputs
// describe the same rectangle twice - the lowest index wins.
func DisplayAtCursor(displays []image.Rectangle, cursor image.Point) int {
	for i, d := range displays {
		if cursor.In(d.Canon()) {
			return i
		}
	}
	return -1
}

// CropDisplayAtCursor cuts the display the cursor is on out of a full
// virtual-screen capture. It is the crop every capture backend needs, because
// the screenshot portal always hands back the whole virtual screen.
//
// displays is the geometry of each display in screen coordinates, indexed the
// way the rest of the app indexes displays (0 is primary). img must cover
// exactly VirtualScreenBounds(displays); its own Min is taken to be the
// top-left of that rectangle, so an image whose bounds start at (0,0) - what
// every decoder produces - is mapped correctly even when the virtual screen
// starts at a negative origin.
//
// The returned image is a fresh copy with its origin at (0,0), not a view into
// img: callers annotate and encode these, and a view would both alias the
// caller's pixels and keep the whole virtual screen alive.
//
// The second return value is the index of the display that was cropped, which
// is not always DisplayAtCursor's answer - see below.
//
// When the cursor is on no display at all, the crop falls back to display 0 and
// returns index 0. A cursor outside every display is reachable in practice: a
// non-rectangular monitor arrangement leaves gaps inside the virtual screen,
// and a backend that cannot read the pointer reports some default position.
// Failing the capture for that would lose a screenshot the user asked for,
// where cropping to the primary display gives them a usable one, so the
// fallback is deliberate. It is reported rather than hidden: callers that need
// to know can compare the returned index with DisplayAtCursor, or call
// DisplayAtCursor themselves and check for -1.
//
// Errors: ErrNoDisplays when displays is empty, ErrGeometryMismatch when img is
// nil, when the display at the chosen index is empty, or when img is not the
// size the geometry claims.
func CropDisplayAtCursor(img *image.RGBA, displays []image.Rectangle, cursor image.Point) (*image.RGBA, int, error) {
	if len(displays) == 0 {
		return nil, -1, ErrNoDisplays
	}
	if img == nil {
		return nil, -1, fmt.Errorf("%w: image is nil", ErrGeometryMismatch)
	}

	virtual := VirtualScreenBounds(displays)
	bounds := img.Bounds()
	if bounds.Dx() != virtual.Dx() || bounds.Dy() != virtual.Dy() {
		return nil, -1, fmt.Errorf("%w: image is %dx%d, virtual screen %s is %dx%d",
			ErrGeometryMismatch, bounds.Dx(), bounds.Dy(), virtual, virtual.Dx(), virtual.Dy())
	}

	index := DisplayAtCursor(displays, cursor)
	if index < 0 {
		index = 0
	}
	display := displays[index].Canon()
	if display.Empty() {
		return nil, -1, fmt.Errorf("%w: display %d is empty (%s)", ErrGeometryMismatch, index, display)
	}

	// Screen coordinates to image coordinates: the virtual screen's top-left
	// corner is at the image's Min, wherever that is.
	offset := bounds.Min.Sub(virtual.Min)
	src := display.Add(offset)

	out := image.NewRGBA(image.Rect(0, 0, display.Dx(), display.Dy()))
	draw.Draw(out, out.Bounds(), img, src.Min, draw.Src)
	return out, index, nil
}

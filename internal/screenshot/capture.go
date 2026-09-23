package screenshot

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/png"
	"math"

	"github.com/kbinani/screenshot"
)

// CaptureResult holds the screenshot data
type CaptureResult struct {
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Data   string `json:"data"` // Base64 encoded PNG
}

// CaptureRegion captures a specific region of the screen
func CaptureRegion(x, y, width, height int) (*CaptureResult, error) {
	img, err := screenshot.Capture(x, y, width, height)
	if err != nil {
		return nil, err
	}
	return encodeImage(img)
}

// CaptureDisplay captures a specific display by index
func CaptureDisplay(displayIndex int) (*CaptureResult, error) {
	bounds := screenshot.GetDisplayBounds(displayIndex)
	img, err := screenshot.CaptureRect(bounds)
	if err != nil {
		return nil, err
	}
	return encodeImage(img)
}

// GetDisplayCount returns the number of active displays
func GetDisplayCount() int {
	return screenshot.NumActiveDisplays()
}

// GetDisplayBounds returns the bounds of a display
func GetDisplayBounds(displayIndex int) image.Rectangle {
	return screenshot.GetDisplayBounds(displayIndex)
}

// GetVirtualScreenBounds returns the combined bounds of all monitors (virtual desktop)
// This includes negative coordinates for monitors positioned left/above the primary monitor
func GetVirtualScreenBounds() (x, y, width, height int) {
	numDisplays := screenshot.NumActiveDisplays()
	if numDisplays == 0 {
		return 0, 0, 1920, 1080 // fallback
	}

	minX, minY := math.MaxInt32, math.MaxInt32
	maxX, maxY := math.MinInt32, math.MinInt32

	for i := 0; i < numDisplays; i++ {
		bounds := screenshot.GetDisplayBounds(i)
		if bounds.Min.X < minX {
			minX = bounds.Min.X
		}
		if bounds.Min.Y < minY {
			minY = bounds.Min.Y
		}
		if bounds.Max.X > maxX {
			maxX = bounds.Max.X
		}
		if bounds.Max.Y > maxY {
			maxY = bounds.Max.Y
		}
	}

	return minX, minY, maxX - minX, maxY - minY
}

// CaptureVirtualScreen captures the entire virtual desktop (all monitors combined)
func CaptureVirtualScreen() (*CaptureResult, error) {
	x, y, w, h := GetVirtualScreenBounds()
	return CaptureRegion(x, y, w, h)
}

// CaptureVirtualScreenRaw captures the entire virtual desktop and returns raw RGBA image
// This is faster than CaptureVirtualScreen as it skips PNG encoding
func CaptureVirtualScreenRaw() (*image.RGBA, error) {
	x, y, w, h := GetVirtualScreenBounds()
	return screenshot.Capture(x, y, w, h)
}

// encodeImage converts an image to base64 PNG
func encodeImage(img *image.RGBA) (*CaptureResult, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}

	return &CaptureResult{
		Width:  img.Bounds().Dx(),
		Height: img.Bounds().Dy(),
		Data:   base64.StdEncoding.EncodeToString(buf.Bytes()),
	}, nil
}

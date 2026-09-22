//go:build windows

package overlay

import (
	"errors"
	"fmt"
	"image"
	"syscall"
	"unsafe"
)

var (
	gdi32                  = syscall.NewLazyDLL("gdi32.dll")
	procCreateDIBSection   = gdi32.NewProc("CreateDIBSection")
	procDeleteObject       = gdi32.NewProc("DeleteObject")
	procSelectObject       = gdi32.NewProc("SelectObject")
	procCreateCompatibleDC = gdi32.NewProc("CreateCompatibleDC")
	procDeleteDC           = gdi32.NewProc("DeleteDC")
	procCreateSolidBrush   = gdi32.NewProc("CreateSolidBrush")
	procCreatePen          = gdi32.NewProc("CreatePen")
	procRectangle          = gdi32.NewProc("Rectangle")
	procSetBkMode          = gdi32.NewProc("SetBkMode")
	procGetStockObject     = gdi32.NewProc("GetStockObject")
)

// DrawContext manages GDI resources for overlay drawing
type DrawContext struct {
	HMemDC     uintptr
	hBitmap    uintptr
	hOldBitmap uintptr
	pixels     unsafe.Pointer
	width      int
	height     int
}

// NewDrawContext creates a 32-bit DIB for overlay drawing
func NewDrawContext(screenDC uintptr, width, height int) (*DrawContext, error) {
	hMemDC, _, _ := procCreateCompatibleDC.Call(screenDC)
	if hMemDC == 0 {
		return nil, errors.New("failed to create compatible DC")
	}

	// Create 32-bit DIB section (top-down with negative height)
	bi := BITMAPINFO{
		BmiHeader: BITMAPINFOHEADER{
			BiSize:        uint32(unsafe.Sizeof(BITMAPINFOHEADER{})),
			BiWidth:       int32(width),
			BiHeight:      int32(-height), // Top-down DIB
			BiPlanes:      1,
			BiBitCount:    32,
			BiCompression: BI_RGB,
		},
	}

	var pixels unsafe.Pointer
	hBitmap, _, _ := procCreateDIBSection.Call(
		hMemDC,
		uintptr(unsafe.Pointer(&bi)),
		DIB_RGB_COLORS,
		uintptr(unsafe.Pointer(&pixels)),
		0, 0,
	)
	if hBitmap == 0 {
		procDeleteDC.Call(hMemDC)
		return nil, errors.New("failed to create DIB section")
	}

	hOldBitmap, _, _ := procSelectObject.Call(hMemDC, hBitmap)

	return &DrawContext{
		HMemDC:     hMemDC,
		hBitmap:    hBitmap,
		hOldBitmap: hOldBitmap,
		pixels:     pixels,
		width:      width,
		height:     height,
	}, nil
}

// DrawOverlay renders the selection overlay
func (dc *DrawContext) DrawOverlay(screenshot *image.RGBA, sel *Selection, scaleRatio float64) {
	// 1. Draw screenshot as background
	dc.drawScreenshot(screenshot)

	// 2. Draw semi-transparent dark overlay
	dc.fillOverlay(128) // 50% opacity

	if sel.IsDragging {
		// 3. Calculate normalized selection bounds
		x1, y1 := minInt(sel.StartX, sel.EndX), minInt(sel.StartY, sel.EndY)
		x2, y2 := maxInt(sel.StartX, sel.EndX), maxInt(sel.StartY, sel.EndY)
		w, h := x2-x1, y2-y1

		// 4. Clear the selection area (show screenshot)
		dc.clearRegion(x1, y1, w, h, screenshot)

		// 5. Draw blue border (2px)
		dc.drawSelectionBorder(x1, y1, w, h)

		// 6. Draw corner handles
		dc.drawCornerHandles(x1, y1, w, h)

		// 7. Draw size indicator
		scaledW := int(float64(w) * scaleRatio)
		scaledH := int(float64(h) * scaleRatio)
		dc.drawSizeIndicator(x1, y2+8, scaledW, scaledH)
	}

	// 8. Draw instructions
	dc.drawInstructions(sel)
}

// drawScreenshot copies screenshot to pixel buffer
func (dc *DrawContext) drawScreenshot(screenshot *image.RGBA) {
	if screenshot == nil {
		return
	}

	bounds := screenshot.Bounds()
	srcWidth := bounds.Dx()
	srcHeight := bounds.Dy()

	// Get pixel buffer as slice with correct bounds
	pixelCount := dc.width * dc.height
	pixels := unsafe.Slice((*uint32)(dc.pixels), pixelCount)

	// Copy pixels (convert RGBA to BGRA with premultiplied alpha)
	for y := 0; y < minInt(srcHeight, dc.height); y++ {
		for x := 0; x < minInt(srcWidth, dc.width); x++ {
			srcIdx := y*screenshot.Stride + x*4
			dstIdx := y*dc.width + x

			r := screenshot.Pix[srcIdx]
			g := screenshot.Pix[srcIdx+1]
			b := screenshot.Pix[srcIdx+2]
			a := uint32(255) // Full opacity

			// BGRA format with premultiplied alpha
			pixels[dstIdx] = (a << 24) | (uint32(r) << 16) | (uint32(g) << 8) | uint32(b)
		}
	}
}

// fillOverlay adds semi-transparent overlay
func (dc *DrawContext) fillOverlay(alpha uint8) {
	pixelCount := dc.width * dc.height
	pixels := unsafe.Slice((*uint32)(dc.pixels), pixelCount)

	for i := 0; i < pixelCount; i++ {
		// Blend with black at specified alpha
		existing := pixels[i]
		r := (existing >> 16) & 0xFF
		g := (existing >> 8) & 0xFF
		b := existing & 0xFF

		// Darken by blending with black
		factor := float64(255-alpha) / 255.0
		r = uint32(float64(r) * factor)
		g = uint32(float64(g) * factor)
		b = uint32(float64(b) * factor)

		pixels[i] = (255 << 24) | (r << 16) | (g << 8) | b
	}
}

// clearRegion restores original screenshot in the selection area
func (dc *DrawContext) clearRegion(x, y, w, h int, screenshot *image.RGBA) {
	if screenshot == nil {
		return
	}

	bounds := screenshot.Bounds()
	srcWidth := bounds.Dx()
	srcHeight := bounds.Dy()

	pixelCount := dc.width * dc.height
	pixels := unsafe.Slice((*uint32)(dc.pixels), pixelCount)

	for dy := 0; dy < h; dy++ {
		py := y + dy
		if py < 0 || py >= dc.height || py >= srcHeight {
			continue
		}
		for dx := 0; dx < w; dx++ {
			px := x + dx
			if px < 0 || px >= dc.width || px >= srcWidth {
				continue
			}

			srcIdx := py*screenshot.Stride + px*4
			dstIdx := py*dc.width + px

			r := screenshot.Pix[srcIdx]
			g := screenshot.Pix[srcIdx+1]
			b := screenshot.Pix[srcIdx+2]

			pixels[dstIdx] = (255 << 24) | (uint32(r) << 16) | (uint32(g) << 8) | uint32(b)
		}
	}
}

// drawSelectionBorder draws a 2px blue border
func (dc *DrawContext) drawSelectionBorder(x, y, w, h int) {
	pixelCount := dc.width * dc.height
	pixels := unsafe.Slice((*uint32)(dc.pixels), pixelCount)

	// Windows blue: 0x0078D7
	blue := uint32((255 << 24) | (0x00 << 16) | (0x78 << 8) | 0xD7)

	// Draw 2px border
	for t := 0; t < 2; t++ {
		// Top edge
		for px := x; px < x+w; px++ {
			py := y + t
			if px >= 0 && px < dc.width && py >= 0 && py < dc.height {
				pixels[py*dc.width+px] = blue
			}
		}
		// Bottom edge
		for px := x; px < x+w; px++ {
			py := y + h - 1 - t
			if px >= 0 && px < dc.width && py >= 0 && py < dc.height {
				pixels[py*dc.width+px] = blue
			}
		}
		// Left edge
		for py := y; py < y+h; py++ {
			px := x + t
			if px >= 0 && px < dc.width && py >= 0 && py < dc.height {
				pixels[py*dc.width+px] = blue
			}
		}
		// Right edge
		for py := y; py < y+h; py++ {
			px := x + w - 1 - t
			if px >= 0 && px < dc.width && py >= 0 && py < dc.height {
				pixels[py*dc.width+px] = blue
			}
		}
	}
}

// drawCornerHandles draws 6x6 blue squares at corners
func (dc *DrawContext) drawCornerHandles(x, y, w, h int) {
	pixelCount := dc.width * dc.height
	pixels := unsafe.Slice((*uint32)(dc.pixels), pixelCount)

	blue := uint32((255 << 24) | (0x00 << 16) | (0x78 << 8) | 0xD7)
	handleSize := 6

	corners := []struct{ cx, cy int }{
		{x - handleSize/2, y - handleSize/2},                 // Top-left
		{x + w - handleSize/2, y - handleSize/2},             // Top-right
		{x - handleSize/2, y + h - handleSize/2},             // Bottom-left
		{x + w - handleSize/2, y + h - handleSize/2},         // Bottom-right
	}

	for _, corner := range corners {
		for dy := 0; dy < handleSize; dy++ {
			for dx := 0; dx < handleSize; dx++ {
				px := corner.cx + dx
				py := corner.cy + dy
				if px >= 0 && px < dc.width && py >= 0 && py < dc.height {
					pixels[py*dc.width+px] = blue
				}
			}
		}
	}
}

// drawSizeIndicator draws the "WxH" size text
func (dc *DrawContext) drawSizeIndicator(x, y, w, h int) {
	pixelCount := dc.width * dc.height
	pixels := unsafe.Slice((*uint32)(dc.pixels), pixelCount)

	// Draw a blue background pill
	text := fmt.Sprintf("%d x %d", w, h)
	textWidth := len(text) * 7 // Approximate character width
	pillWidth := textWidth + 16
	pillHeight := 20
	pillX := x
	pillY := y

	// Ensure pill is within bounds
	if pillY+pillHeight >= dc.height {
		pillY = y - pillHeight - 8
	}
	if pillX+pillWidth >= dc.width {
		pillX = dc.width - pillWidth - 4
	}
	if pillX < 0 {
		pillX = 4
	}

	blue := uint32((255 << 24) | (0x00 << 16) | (0x78 << 8) | 0xD7)

	// Draw pill background
	for dy := 0; dy < pillHeight; dy++ {
		for dx := 0; dx < pillWidth; dx++ {
			px := pillX + dx
			py := pillY + dy
			if px >= 0 && px < dc.width && py >= 0 && py < dc.height {
				pixels[py*dc.width+px] = blue
			}
		}
	}

	// Draw simple white text using bitmap font
	dc.drawText(pillX+8, pillY+4, text, pixels)
}

// drawText draws simple bitmap text (basic 5x7 font)
func (dc *DrawContext) drawText(x, y int, text string, pixels []uint32) {
	white := uint32((255 << 24) | (255 << 16) | (255 << 8) | 255)

	// Simple 5x7 bitmap font for digits and 'x'
	font := map[rune][]uint8{
		'0': {0x3E, 0x45, 0x49, 0x51, 0x3E},
		'1': {0x00, 0x21, 0x7F, 0x01, 0x00},
		'2': {0x27, 0x49, 0x49, 0x49, 0x31},
		'3': {0x22, 0x49, 0x49, 0x49, 0x36},
		'4': {0x0C, 0x14, 0x24, 0x7F, 0x04},
		'5': {0x72, 0x51, 0x51, 0x51, 0x4E},
		'6': {0x3E, 0x49, 0x49, 0x49, 0x26},
		'7': {0x40, 0x40, 0x47, 0x48, 0x70},
		'8': {0x36, 0x49, 0x49, 0x49, 0x36},
		'9': {0x32, 0x49, 0x49, 0x49, 0x3E},
		'x': {0x00, 0x14, 0x08, 0x14, 0x00},
		' ': {0x00, 0x00, 0x00, 0x00, 0x00},
	}

	curX := x
	for _, ch := range text {
		glyph, ok := font[ch]
		if !ok {
			curX += 6
			continue
		}

		for col, bits := range glyph {
			for row := 0; row < 7; row++ {
				if bits&(1<<(6-row)) != 0 {
					px := curX + col
					py := y + row
					if px >= 0 && px < dc.width && py >= 0 && py < dc.height {
						pixels[py*dc.width+px] = white
					}
				}
			}
		}
		curX += 6
	}
}

// drawInstructions draws instruction text at top center
func (dc *DrawContext) drawInstructions(sel *Selection) {
	pixelCount := dc.width * dc.height
	pixels := unsafe.Slice((*uint32)(dc.pixels), pixelCount)

	var text string
	if sel.IsDragging && sel.SpaceHeld {
		text = "Hold Space + Drag to reposition"
	} else {
		text = "Drag to select. Space to move. ESC cancel"
	}

	textWidth := len(text) * 7
	pillWidth := textWidth + 20
	pillHeight := 24
	pillX := (dc.width - pillWidth) / 2
	pillY := 16

	// Black background with slight transparency
	bgColor := uint32((220 << 24) | (0 << 16) | (0 << 8) | 0)

	// Draw pill background
	for dy := 0; dy < pillHeight; dy++ {
		for dx := 0; dx < pillWidth; dx++ {
			px := pillX + dx
			py := pillY + dy
			if px >= 0 && px < dc.width && py >= 0 && py < dc.height {
				pixels[py*dc.width+px] = bgColor
			}
		}
	}

	// Draw text
	dc.drawInstructionText(pillX+10, pillY+6, text, pixels)
}

// drawInstructionText draws instruction text with extended character support
func (dc *DrawContext) drawInstructionText(x, y int, text string, pixels []uint32) {
	white := uint32((255 << 24) | (255 << 16) | (255 << 8) | 255)

	// Extended 5x7 bitmap font
	font := map[rune][]uint8{
		'0': {0x3E, 0x45, 0x49, 0x51, 0x3E},
		'1': {0x00, 0x21, 0x7F, 0x01, 0x00},
		'2': {0x27, 0x49, 0x49, 0x49, 0x31},
		'3': {0x22, 0x49, 0x49, 0x49, 0x36},
		'4': {0x0C, 0x14, 0x24, 0x7F, 0x04},
		'5': {0x72, 0x51, 0x51, 0x51, 0x4E},
		'6': {0x3E, 0x49, 0x49, 0x49, 0x26},
		'7': {0x40, 0x40, 0x47, 0x48, 0x70},
		'8': {0x36, 0x49, 0x49, 0x49, 0x36},
		'9': {0x32, 0x49, 0x49, 0x49, 0x3E},
		'A': {0x3F, 0x48, 0x48, 0x48, 0x3F},
		'B': {0x7F, 0x49, 0x49, 0x49, 0x36},
		'C': {0x3E, 0x41, 0x41, 0x41, 0x22},
		'D': {0x7F, 0x41, 0x41, 0x41, 0x3E},
		'E': {0x7F, 0x49, 0x49, 0x49, 0x41},
		'S': {0x32, 0x49, 0x49, 0x49, 0x26},
		'a': {0x02, 0x15, 0x15, 0x15, 0x0F},
		'c': {0x0E, 0x11, 0x11, 0x11, 0x0A},
		'd': {0x0E, 0x11, 0x11, 0x11, 0x7F},
		'e': {0x0E, 0x15, 0x15, 0x15, 0x0C},
		'g': {0x08, 0x15, 0x15, 0x15, 0x1E},
		'h': {0x7F, 0x08, 0x08, 0x08, 0x07},
		'i': {0x00, 0x00, 0x2F, 0x00, 0x00},
		'l': {0x00, 0x00, 0x7F, 0x00, 0x00},
		'm': {0x1F, 0x10, 0x0E, 0x10, 0x0F},
		'n': {0x1F, 0x08, 0x10, 0x10, 0x0F},
		'o': {0x0E, 0x11, 0x11, 0x11, 0x0E},
		'p': {0x1F, 0x14, 0x14, 0x14, 0x08},
		'r': {0x1F, 0x08, 0x10, 0x10, 0x08},
		's': {0x09, 0x15, 0x15, 0x15, 0x12},
		't': {0x10, 0x7E, 0x11, 0x01, 0x02},
		'u': {0x1E, 0x01, 0x01, 0x01, 0x1E},
		'v': {0x18, 0x06, 0x01, 0x06, 0x18},
		'x': {0x11, 0x0A, 0x04, 0x0A, 0x11},
		' ': {0x00, 0x00, 0x00, 0x00, 0x00},
		'.': {0x00, 0x01, 0x00, 0x00, 0x00},
		'+': {0x04, 0x04, 0x1F, 0x04, 0x04},
		'H': {0x7F, 0x08, 0x08, 0x08, 0x7F},
		'P': {0x7F, 0x48, 0x48, 0x48, 0x30},
	}

	curX := x
	for _, ch := range text {
		glyph, ok := font[ch]
		if !ok {
			curX += 6
			continue
		}

		for col, bits := range glyph {
			for row := 0; row < 7; row++ {
				if bits&(1<<(6-row)) != 0 {
					px := curX + col
					py := y + row
					if px >= 0 && px < dc.width && py >= 0 && py < dc.height {
						pixels[py*dc.width+px] = white
					}
				}
			}
		}
		curX += 6
	}
}

// Cleanup releases GDI resources
func (dc *DrawContext) Cleanup() {
	if dc.hOldBitmap != 0 {
		procSelectObject.Call(dc.HMemDC, dc.hOldBitmap)
	}
	if dc.hBitmap != 0 {
		procDeleteObject.Call(dc.hBitmap)
	}
	if dc.HMemDC != 0 {
		procDeleteDC.Call(dc.HMemDC)
	}
}

// Helper functions
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

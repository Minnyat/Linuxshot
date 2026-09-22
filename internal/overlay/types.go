//go:build windows

package overlay

// Window style constants
const (
	WS_POPUP         = 0x80000000
	WS_VISIBLE       = 0x10000000
	WS_EX_LAYERED    = 0x00080000
	WS_EX_TOPMOST    = 0x00000008
	WS_EX_NOACTIVATE = 0x08000000
	WS_EX_TOOLWINDOW = 0x00000080
)

// Message constants
const (
	WM_CREATE      = 0x0001
	WM_DESTROY     = 0x0002
	WM_PAINT       = 0x000F
	WM_KEYDOWN     = 0x0100
	WM_KEYUP       = 0x0101
	WM_LBUTTONDOWN = 0x0201
	WM_LBUTTONUP   = 0x0202
	WM_MOUSEMOVE   = 0x0200
	WM_NCHITTEST   = 0x0084
	WM_SETCURSOR   = 0x0020
	VK_ESCAPE      = 0x1B
	VK_SPACE       = 0x20
	HTCLIENT       = 1
	PM_REMOVE      = 0x0001
)

// GDI constants
const (
	DIB_RGB_COLORS = 0
	SRCCOPY        = 0x00CC0020
	BI_RGB         = 0
	TRANSPARENT    = 1
	PS_SOLID       = 0
	NULL_BRUSH     = 5
)

// UpdateLayeredWindow flags
const (
	ULW_ALPHA = 0x00000002
)

// BLENDFUNCTION for UpdateLayeredWindow
type BLENDFUNCTION struct {
	BlendOp             byte
	BlendFlags          byte
	SourceConstantAlpha byte
	AlphaFormat         byte
}

const (
	AC_SRC_OVER  = 0x00
	AC_SRC_ALPHA = 0x01
)

// BITMAPINFOHEADER for DIB creation
type BITMAPINFOHEADER struct {
	BiSize          uint32
	BiWidth         int32
	BiHeight        int32
	BiPlanes        uint16
	BiBitCount      uint16
	BiCompression   uint32
	BiSizeImage     uint32
	BiXPelsPerMeter int32
	BiYPelsPerMeter int32
	BiClrUsed       uint32
	BiClrImportant  uint32
}

// BITMAPINFO for CreateDIBSection
type BITMAPINFO struct {
	BmiHeader BITMAPINFOHEADER
	BmiColors [1]uint32
}

// Selection represents the user's region selection
type Selection struct {
	StartX, StartY int
	EndX, EndY     int
	IsDragging     bool
	SpaceHeld      bool // For repositioning selection
}

// Result represents the final selection result
type Result struct {
	X, Y          int
	Width, Height int
	Cancelled     bool
}

// WNDCLASSEXW for RegisterClassExW
type WNDCLASSEXW struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     uintptr
	HIcon         uintptr
	HCursor       uintptr
	HbrBackground uintptr
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       uintptr
}

// MSG structure for Windows messages
type MSG struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      POINT
}

// POINT structure
type POINT struct {
	X, Y int32
}

// SIZE structure
type SIZE struct {
	Cx, Cy int32
}

// RECT structure
type RECT struct {
	Left, Top, Right, Bottom int32
}

// Window constants
const (
	SW_SHOW       = 5
	SW_HIDE       = 0
	HWND_TOPMOST  = ^uintptr(0) // -1
	IDC_CROSS     = 32515
	IDC_SIZEALL   = 32646
	SWP_NOSIZE    = 0x0001
	SWP_NOMOVE    = 0x0002
	SWP_SHOWWINDOW = 0x0040
)

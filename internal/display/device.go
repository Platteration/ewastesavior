package display

import (
	"image"
	"strings"

	"github.com/platteration/ewastesavior/internal/proto"
)

// Device is a screen that shows frames. Implementations need not be safe
// for concurrent use; the Controller serializes calls.
type Device interface {
	// Size returns the physical framebuffer size.
	Size() (w, h int)
	// Show displays img, which has the logical (rotated) size. dirty lists
	// the changed rectangles in img coordinates; nil means everything.
	Show(img *image.RGBA, dirty []image.Rectangle) error
	// Blank turns the picture off (on=true) or back on and reports which
	// method worked: "fbioblank", "backlight" or "black".
	Blank(on bool) (method string, err error)
	// Info describes the device: format, driver, physical and logical size.
	Info() proto.DisplayState
	Close() error
}

// Rotator is implemented by devices that can change rotation in place.
type Rotator interface {
	SetRotate(deg int) error
}

// Invalidator is implemented by devices with a shadow buffer: after
// Invalidate the next Show rewrites the whole screen (someone else drew on
// it, e.g. the text console after a VT switch).
type Invalidator interface {
	Invalidate()
}

// Checker is implemented by devices that can detect being replaced or
// removed (a KMS driver taking over from efifb/simpledrm). Check returns an
// error wrapping ErrDeviceGone when the device should be reopened.
type Checker interface {
	Check() error
}

// vtUser marks devices that need the controller to own a virtual
// terminal (real framebuffers, where fbcon would otherwise draw).
type vtUser interface {
	usesVT() bool
}

// OpenDevice opens the display_device config value: "auto" (or "")
// selects a framebuffer with AutoDevicePath, anything else is a
// /dev/fbN path.
func OpenDevice(device string, rotate int) (Device, error) {
	device = strings.TrimSpace(device)
	if device == "" || device == "auto" {
		return openAuto("/", rotate)
	}
	return OpenFramebuffer(device, rotate)
}

// fillInfo fills the format and size fields of a DisplayState.
func (c *fbCore) fillInfo(st *proto.DisplayState) {
	st.Format = c.pf.name
	st.FBWidth, st.FBHeight = c.g.w, c.g.h
	st.Width, st.Height = c.logicalSize()
	st.Rotate = c.rotate
}

package display

import (
	"fmt"
	"image"
	"sync"

	"github.com/platteration/ewastesavior/internal/proto"
)

// MemDevice is an in-memory framebuffer for tests and headless use. It
// runs the same conversion code as the Linux driver and exposes the raw
// device-format bytes. It is safe for concurrent use.
type MemDevice struct {
	mu      sync.Mutex
	core    *fbCore
	mem     []byte
	warning string
	blanked bool
	shows   int
	closed  bool
	failErr error
	method  string
}

// NewMemDevice returns a w×h memory framebuffer. format is a layout name
// (XRGB8888, XBGR8888, ARGB8888, RGB888, BGR888, RGB565, BGR565,
// XRGB1555, ARGB1555, XRGB2101010, ...) or "" for the default of bpp.
// lineLength <= w*bpp/8 means unpadded rows. Invalid arguments fall back to
// sane defaults and are reported in Info().Warning.
func NewMemDevice(w, h, bpp int, format string, lineLength int, rotate int) *MemDevice {
	return newMemDevice(w, h, bpp, format, lineLength, 0, 0, rotate)
}

func newMemDevice(w, h, bpp int, format string, lineLength, xoff, yoff, rotate int) *MemDevice {
	d := &MemDevice{method: "black"}
	var warn []string
	if w <= 0 || h <= 0 || w > 16384 || h > 16384 {
		warn = append(warn, fmt.Sprintf("bad size %dx%d, using 640x480", w, h))
		w, h = 640, 480
	}
	if format == "" {
		format = defaultFormat(bpp)
	}
	pf, err := formatByName(format)
	if err == nil && bpp != 0 && pf.bpp != bpp {
		err = fmt.Errorf("format %s is %d bpp, not %d", format, pf.bpp, bpp)
	}
	if err != nil {
		warn = append(warn, err.Error())
		pf, _ = formatByName(defaultFormat(bpp))
	}
	if need := (w + xoff) * pf.bytesPP(); lineLength < need {
		lineLength = need
	}
	if _, err := normRotate(rotate); err != nil {
		warn = append(warn, err.Error())
		rotate = 0
	}
	d.mem = make([]byte, (h+yoff)*lineLength)
	g := fbGeom{w: w, h: h, xoff: xoff, yoff: yoff, lineLen: lineLength}
	d.core, _ = newFBCore(g, pf, rotate, fbTarget{mem: d.mem})
	if len(warn) > 0 {
		d.warning = fmt.Sprint(warn)
	}
	return d
}

// Size implements Device (physical size).
func (d *MemDevice) Size() (int, int) { return d.core.g.w, d.core.g.h }

// Show implements Device.
func (d *MemDevice) Show(img *image.RGBA, dirty []image.Rectangle) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return fmt.Errorf("%w: closed", ErrDeviceGone)
	}
	if d.failErr != nil {
		err := d.failErr
		d.failErr = nil
		return err
	}
	d.shows++
	return d.core.show(img, dirty)
}

// Blank implements Device. The memory device blanks with a black frame
// unless SetBlankMethod chose another name.
func (d *MemDevice) Blank(on bool) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.blanked = on
	if on && d.method == "black" {
		d.core.fillShadow(d.core.pf.pack(0, 0, 0))
		return d.method, d.core.flush()
	}
	return d.method, nil
}

// Info implements Device.
func (d *MemDevice) Info() proto.DisplayState {
	d.mu.Lock()
	defer d.mu.Unlock()
	st := proto.DisplayState{Active: !d.closed, Device: "mem", Driver: "memory", Warning: d.warning}
	d.core.fillInfo(&st)
	return st
}

// Close implements Device.
func (d *MemDevice) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed = true
	return nil
}

// SetRotate implements Rotator.
func (d *MemDevice) SetRotate(deg int) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.core.setRotate(deg); err != nil {
		return err
	}
	d.core.invalidate()
	return nil
}

// Invalidate implements Invalidator.
func (d *MemDevice) Invalidate() {
	d.mu.Lock()
	d.core.invalidate()
	d.mu.Unlock()
}

// Bytes returns a copy of the device memory (device format, rows of
// LineLength bytes).
func (d *MemDevice) Bytes() []byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]byte(nil), d.mem...)
}

// LineLength returns the bytes per row.
func (d *MemDevice) LineLength() int { return d.core.g.lineLen }

// Format returns the pixel format name.
func (d *MemDevice) Format() string { return d.core.pf.name }

// Decode converts the device memory back to an RGBA image in physical
// orientation.
func (d *MemDevice) Decode() *image.RGBA {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.core.decode(d.mem)
}

// DecodeLogical is Decode rotated back to the logical orientation, i.e.
// what was passed to Show (quantized to the device format).
func (d *MemDevice) DecodeLogical() *image.RGBA {
	d.mu.Lock()
	defer d.mu.Unlock()
	return unrotate(d.core.decode(d.mem), d.core.rotate)
}

// Shows returns how many times Show was called.
func (d *MemDevice) Shows() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.shows
}

// RowsWritten returns how many rows were copied into device memory.
func (d *MemDevice) RowsWritten() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.core.rows
}

// Blanked reports the last Blank state.
func (d *MemDevice) Blanked() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.blanked
}

// SetBlankMethod changes the method name Blank reports; any method other
// than "black" leaves the memory untouched (like FBIOBLANK).
func (d *MemDevice) SetBlankMethod(m string) {
	d.mu.Lock()
	d.method = m
	d.mu.Unlock()
}

// FailNextShow makes the next Show return err (fault injection for tests,
// e.g. an error wrapping ErrDeviceGone).
func (d *MemDevice) FailNextShow(err error) {
	d.mu.Lock()
	d.failErr = err
	d.mu.Unlock()
}

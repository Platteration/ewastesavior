package display

import (
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"sync"

	"github.com/platteration/ewastesavior/internal/proto"
)

// pngDevice writes every shown frame to a PNG file (atomically: temp file,
// fsync, rename). The PNG has the physical size with rotation applied, so
// it shows exactly what a panel of that size would.
type pngDevice struct {
	mu      sync.Mutex
	path    string
	core    *fbCore
	mem     []byte
	err     string
	blanked bool
}

// NewPNGDevice returns a Device that writes frames to path. w and h are
// the physical size; invalid values fall back to 640x480 (reported in
// Info().Warning) because the Device contract has no error return here.
func NewPNGDevice(path string, w, h, rotate int) Device {
	d := &pngDevice{path: path}
	if w <= 0 || h <= 0 || w > 16384 || h > 16384 {
		d.err = fmt.Sprintf("bad size %dx%d, using 640x480", w, h)
		w, h = 640, 480
	}
	if _, err := normRotate(rotate); err != nil {
		d.err = err.Error()
		rotate = 0
	}
	pf, _ := formatByName("ABGR8888") // memory order R, G, B, A=0xff
	d.mem = make([]byte, w*h*4)
	d.core, _ = newFBCore(fbGeom{w: w, h: h, lineLen: w * 4}, pf, rotate, fbTarget{mem: d.mem})
	return d
}

func (d *pngDevice) Size() (int, int) { return d.core.g.w, d.core.g.h }

func (d *pngDevice) Show(img *image.RGBA, dirty []image.Rectangle) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.core.show(img, dirty); err != nil {
		return err
	}
	return d.write()
}

func (d *pngDevice) write() error {
	out := &image.RGBA{Pix: d.mem, Stride: d.core.g.lineLen, Rect: image.Rect(0, 0, d.core.g.w, d.core.g.h)}
	tmp, err := os.CreateTemp(filepath.Dir(d.path), ".savior-png-*")
	if err != nil {
		return fmt.Errorf("write %s: %w", d.path, err)
	}
	defer os.Remove(tmp.Name())
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(tmp, out); err != nil {
		tmp.Close()
		return fmt.Errorf("encode %s: %w", d.path, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", d.path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write %s: %w", d.path, err)
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", d.path, err)
	}
	if err := os.Rename(tmp.Name(), d.path); err != nil {
		return fmt.Errorf("write %s: %w", d.path, err)
	}
	return nil
}

func (d *pngDevice) Blank(on bool) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.blanked = on
	if !on {
		return "black", nil
	}
	d.core.fillShadow(d.core.pf.pack(0, 0, 0))
	if err := d.core.flush(); err != nil {
		return "black", err
	}
	return "black", d.write()
}

func (d *pngDevice) Info() proto.DisplayState {
	d.mu.Lock()
	defer d.mu.Unlock()
	st := proto.DisplayState{Active: true, Device: d.path, Driver: "png", Warning: d.err}
	d.core.fillInfo(&st)
	st.Format = "PNG"
	return st
}

func (d *pngDevice) Close() error { return nil }

func (d *pngDevice) SetRotate(deg int) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.core.setRotate(deg); err != nil {
		return err
	}
	d.core.invalidate()
	return nil
}

func (d *pngDevice) Invalidate() {
	d.mu.Lock()
	d.core.invalidate()
	d.mu.Unlock()
}

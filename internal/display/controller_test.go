package display

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/platteration/ewastesavior/internal/proto"
)

type testLogWriter struct{ t *testing.T }

func (w testLogWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimSpace(string(p)))
	return len(p), nil
}

// runController starts c.Run with fast polling and returns a stop func.
func runController(t *testing.T, c *Controller) func() {
	t.Helper()
	c.pollEvery = 20 * time.Millisecond
	c.flashEvery = 40 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			if err := <-done; err != nil {
				t.Errorf("Run: %v", err)
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

func newTestController(t *testing.T, dev Device, env Env) *Controller {
	log := slog.New(slog.NewTextHandler(testLogWriter{t}, &slog.HandlerOptions{Level: slog.LevelWarn}))
	return NewController(func() (Device, error) { return dev, nil }, env, log)
}

func centerOf(d *MemDevice) color.RGBA {
	img := d.DecodeLogical()
	return img.RGBAAt(img.Rect.Dx()/2, img.Rect.Dy()/2)
}

func colorSpec(hex string, rev int64) proto.DisplaySpec {
	return proto.DisplaySpec{Mode: proto.DisplayColor, BG: hex, Rev: rev}
}

func TestControllerAppliesSpecs(t *testing.T) {
	dev := NewMemDevice(64, 48, 32, "XRGB8888", 0, 0)
	c := newTestController(t, dev, fixedEnv())
	c.Apply(colorSpec("#ff0000", 1))
	runController(t, c)
	waitFor(t, "red", func() bool { return centerOf(dev) == rgb(255, 0, 0) })
	c.Apply(colorSpec("#0000ff", 2))
	waitFor(t, "blue", func() bool { return centerOf(dev) == rgb(0, 0, 255) })
	waitFor(t, "state", func() bool { return c.State().Rev == 2 })
	st := c.State()
	if !st.Active || !st.Foreground || st.Mode != "color" || st.Width != 64 || st.Format != "XRGB8888" || st.Error != "" || !st.Ready {
		t.Fatalf("state %+v", st)
	}
	// Applying the same spec again does nothing.
	shows := dev.Shows()
	c.Apply(colorSpec("#0000ff", 2))
	time.Sleep(80 * time.Millisecond)
	if dev.Shows() != shows {
		t.Fatal("identical spec caused a repaint")
	}
}

func TestControllerSteadyStateIdle(t *testing.T) {
	dev := NewMemDevice(320, 240, 16, "RGB565", 0, 0)
	st := StatusInfo{Name: "node-1", Link: proto.LinkConnected, Metrics: proto.Metrics{CPUPercent: 12}}
	var statusCalls atomic.Int32
	env := Env{Status: func() StatusInfo { statusCalls.Add(1); return st }} // real clock
	c := newTestController(t, dev, env)
	runController(t, c)
	waitFor(t, "first frame", func() bool { return dev.Shows() >= 1 })
	shows, rows := dev.Shows(), dev.RowsWritten()
	time.Sleep(1300 * time.Millisecond) // crosses at least one second boundary
	if statusCalls.Load() < 2 {
		t.Fatalf("status polled %d times", statusCalls.Load())
	}
	if dev.Shows() != shows || dev.RowsWritten() != rows {
		t.Fatalf("unchanged status repainted: shows %d->%d rows %d->%d", shows, dev.Shows(), rows, dev.RowsWritten())
	}
}

func TestControllerIdentify(t *testing.T) {
	dev := NewMemDevice(64, 48, 32, "", 0, 0)
	env := fixedEnv()
	env.Status = func() StatusInfo { return StatusInfo{Name: "pc1"} }
	c := newTestController(t, dev, env)
	c.Apply(colorSpec("#00ff00", 1))
	runController(t, c)
	green := rgb(0, 255, 0)
	waitFor(t, "green", func() bool { return centerOf(dev) == green })
	c.Identify(400*time.Millisecond, "7KQ", "R1C1")
	seen := map[color.RGBA]bool{}
	waitFor(t, "both overlay phases", func() bool {
		img := dev.DecodeLogical()
		seen[img.RGBAAt(5, 5)] = true // inside the panel, outside the text
		if img.RGBAAt(1, 1) != green {
			t.Fatal("overlay covered the scene edge")
		}
		return seen[identYellow] && seen[identDark]
	})
	waitFor(t, "scene restored", func() bool { return centerOf(dev) == green })

	// Identify unblanks a lid-blanked screen, and blanking resumes after.
	c.SetBlank("lid", true)
	waitFor(t, "blanked", func() bool { return dev.Blanked() && c.State().Blanked })
	c.Identify(200*time.Millisecond, "7KQ", "")
	waitFor(t, "unblanked for identify", func() bool { return !dev.Blanked() })
	waitFor(t, "blanked again", func() bool { return dev.Blanked() && c.State().BlankReason == "lid" })
}

func TestControllerBlanking(t *testing.T) {
	dev := NewMemDevice(32, 24, 32, "", 0, 0)
	c := newTestController(t, dev, fixedEnv())
	c.Apply(colorSpec("#ffffff", 1))
	runController(t, c)
	waitFor(t, "white", func() bool { return centerOf(dev) == colWhite })

	c.SetBlank("lid", true)
	waitFor(t, "lid blank", func() bool {
		st := c.State()
		return st.Blanked && st.BlankReason == "lid" && st.BlankMethod == "black"
	})
	if centerOf(dev) != colBlack {
		t.Fatal("black-frame blank not black")
	}
	c.SetBlank("lid", false)
	waitFor(t, "unblank", func() bool { return !c.State().Blanked && centerOf(dev) == colWhite })

	// Idle blanking applies only to the local default screen (Rev 0).
	c.SetBlank("idle", true)
	time.Sleep(80 * time.Millisecond)
	if c.State().Blanked {
		t.Fatal("idle blanked a hive-assigned spec")
	}
	c.Apply(proto.DisplaySpec{Mode: proto.DisplayClock}) // Rev 0: local default
	waitFor(t, "idle blank", func() bool { return c.State().BlankReason == "idle" })
	c.inputActivity() // a key press wakes it
	waitFor(t, "idle wake", func() bool { return !c.State().Blanked })

	c.Apply(proto.DisplaySpec{Mode: proto.DisplayOff, Rev: 9})
	waitFor(t, "mode off", func() bool { st := c.State(); return st.Blanked && st.BlankReason == "off" })
	c.SetBlank("", true) // ignored
}

func TestControllerIdleTimer(t *testing.T) {
	dev := NewMemDevice(32, 24, 32, "", 0, 0)
	c := newTestController(t, dev, fixedEnv())
	c.inputDir = t.TempDir() // no input devices
	c.SetIdleOff(100 * time.Millisecond)
	runController(t, c)
	waitFor(t, "idle blank", func() bool { return c.State().BlankReason == "idle" })
	c.inputActivity()
	waitFor(t, "woken", func() bool { return !c.State().Blanked })
}

func TestControllerReopensGoneDevice(t *testing.T) {
	var mu sync.Mutex
	var devs []*MemDevice
	open := func() (Device, error) {
		mu.Lock()
		defer mu.Unlock()
		d := NewMemDevice(32, 24, 16, "", 0, 0)
		devs = append(devs, d)
		return d, nil
	}
	latest := func() (*MemDevice, int) {
		mu.Lock()
		defer mu.Unlock()
		if len(devs) == 0 {
			return NewMemDevice(32, 24, 16, "", 0, 0), 0
		}
		return devs[len(devs)-1], len(devs)
	}
	c := NewController(open, fixedEnv(), nil)
	c.Apply(colorSpec("#ff0000", 1))
	runController(t, c)
	waitFor(t, "first device", func() bool { d, _ := latest(); return centerOf(d) == rgb(255, 0, 0) })
	first, _ := latest()
	first.FailNextShow(fmt.Errorf("%w: unplugged", ErrDeviceGone))
	c.Apply(colorSpec("#0000ff", 2))
	waitFor(t, "reopened", func() bool {
		d, n := latest()
		return n >= 2 && centerOf(d) == rgb(0, 0, 255)
	})
	if err := first.Show(image.NewRGBA(image.Rect(0, 0, 32, 24)), nil); !errors.Is(err, ErrDeviceGone) {
		t.Fatal("old device was not closed")
	}
	waitFor(t, "healthy state", func() bool { st := c.State(); return st.Active && st.Error == "" })
}

func TestControllerOpenFailureRetries(t *testing.T) {
	var attempts atomic.Int32
	dev := NewMemDevice(16, 16, 32, "", 0, 0)
	open := func() (Device, error) {
		if attempts.Add(1) <= 3 {
			return nil, errors.New("no framebuffer yet")
		}
		return dev, nil
	}
	c := NewController(open, fixedEnv(), nil)
	c.Apply(colorSpec("#123456", 1))
	runController(t, c)
	waitFor(t, "error reported", func() bool { return strings.Contains(c.State().Error, "no framebuffer yet") })
	if c.State().Active {
		t.Fatal("active without a device")
	}
	waitFor(t, "recovered", func() bool {
		st := c.State()
		return st.Active && st.Error == "" && centerOf(dev) == rgb(0x12, 0x34, 0x56)
	})
	// A panicking opener is survived too.
	c2 := NewController(func() (Device, error) { panic("driver bug") }, fixedEnv(), nil)
	runController(t, c2)
	waitFor(t, "panic reported", func() bool { return strings.Contains(c2.State().Error, "driver bug") })
}

func TestControllerRotation(t *testing.T) {
	dev := NewMemDevice(40, 30, 32, "", 0, 0)
	c := newTestController(t, dev, fixedEnv())
	spec := proto.DisplaySpec{Mode: "text", Text: "Up", Rev: 1}
	c.Apply(spec)
	c.SetRotate(90)
	runController(t, c)
	waitFor(t, "rotated state", func() bool { st := c.State(); return st.Width == 30 && st.Height == 40 && st.Rotate == 90 })
	want := image.NewRGBA(image.Rect(0, 0, 30, 40))
	Render(context.Background(), spec, want, fixedEnv())
	waitFor(t, "rotated frame", func() bool { return string(dev.DecodeLogical().Pix) == string(want.Pix) })
	c.SetRotate(45) // invalid: ignored with a warning
	waitFor(t, "warning", func() bool { return strings.Contains(c.State().Warning, "multiple of 90") })
	if c.State().Rotate != 90 {
		t.Fatal("invalid rotation applied")
	}
	c.SetRotate(0)
	waitFor(t, "unrotated", func() bool { st := c.State(); return st.Width == 40 && st.Warning == "" })
}

func TestControllerMediaErrorFromPNGBomb(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(pngBomb(15000, 15000))
	}))
	defer srv.Close()
	dev := NewMemDevice(64, 48, 32, "", 0, 0)
	env := fixedEnv()
	env.Fetch = newTestFetcher().Fetch
	c := newTestController(t, dev, env)
	c.Apply(proto.DisplaySpec{Mode: "image", Rev: 1, Image: &proto.Media{URL: srv.URL + "/bomb.png"}})
	runController(t, c)
	waitFor(t, "media error", func() bool { return len(c.State().MediaErrors) == 1 })
	st := c.State()
	if !strings.Contains(st.MediaErrors[0], "too large") || st.Error != "" || st.Ready || st.Total != 1 || st.Loaded != 0 {
		t.Fatalf("state %+v", st)
	}
	// Still alive and responsive.
	c.Apply(colorSpec("#00ffff", 2))
	waitFor(t, "cyan", func() bool { return centerOf(dev) == rgb(0, 255, 255) })
	waitFor(t, "errors cleared", func() bool { return len(c.State().MediaErrors) == 0 && c.State().Total == 0 })
}

func TestControllerMediaLoads(t *testing.T) {
	data := pngBytes(t, 20, 20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(data) }))
	defer srv.Close()
	dev := NewMemDevice(40, 20, 32, "", 0, 0)
	env := fixedEnv()
	env.Fetch = newTestFetcher().Fetch
	c := newTestController(t, dev, env)
	c.Apply(proto.DisplaySpec{Mode: "image", Rev: 1, Fit: "stretch", Image: &proto.Media{URL: srv.URL + "/a.png"}})
	runController(t, c)
	waitFor(t, "image shown", func() bool {
		img := dev.DecodeLogical()
		return img.RGBAAt(5, 10) == rgb(255, 0, 0) && img.RGBAAt(35, 10) == rgb(0, 0, 255)
	})
	waitFor(t, "ready", func() bool { st := c.State(); return st.Ready && st.Loaded == 1 && st.Total == 1 })
}

func TestControllerInvalidSpecAndPanics(t *testing.T) {
	dev := NewMemDevice(64, 48, 32, "", 0, 0)
	var boom atomic.Bool
	env := fixedEnv()
	env.Status = func() StatusInfo {
		if boom.Load() {
			panic("status exploded")
		}
		return StatusInfo{Name: "ok"}
	}
	c := newTestController(t, dev, env)
	c.Apply(proto.DisplaySpec{Mode: "hologram", Rev: 1})
	runController(t, c)
	errBG := rgb(0x5a, 0x10, 0x10)
	waitFor(t, "invalid spec", func() bool {
		return strings.Contains(c.State().Error, "invalid display spec") && dev.DecodeLogical().RGBAAt(1, 1) == errBG
	})
	boom.Store(true)
	c.Apply(proto.DisplaySpec{Mode: "status", Rev: 2})
	waitFor(t, "render panic", func() bool {
		return strings.Contains(c.State().Error, "status exploded") && dev.DecodeLogical().RGBAAt(1, 1) == errBG
	})
	boom.Store(false)
	c.Apply(colorSpec("#445566", 3))
	waitFor(t, "recovered", func() bool { return c.State().Error == "" && centerOf(dev) == rgb(0x44, 0x55, 0x66) })
}

func TestControllerDashboard(t *testing.T) {
	dev := NewMemDevice(320, 240, 32, "", 0, 0)
	env := fixedEnv()
	env.Stats = func(context.Context) (*proto.SwarmStats, error) {
		return &proto.SwarmStats{NodesOnline: 5}, nil
	}
	c := newTestController(t, dev, env)
	c.Apply(proto.DisplaySpec{Mode: "dashboard", Rev: 1})
	runController(t, c)
	waitFor(t, "tiles", func() bool { return countColor(dev.DecodeLogical(), image.Rect(0, 0, 320, 240), colPanel) > 5000 })
}

// TestControllerSlideshowPrerender drives the run loop step by step with
// a fake clock: the next slide is rendered from half an interval before
// the boundary, and at the boundary the prepared frame is only swapped in.
func TestControllerSlideshowPrerender(t *testing.T) {
	colors := []color.RGBA{rgb(255, 0, 0), rgb(0, 255, 0), rgb(0, 0, 255)}
	var fetches atomic.Int32
	fetch := func(_ context.Context, m proto.Media, req FetchRequest) (image.Image, error) {
		fetches.Add(1)
		return solid(req.PixelW, req.PixelH, colors[m.URL[len(m.URL)-1]-'0']), nil
	}
	var mu sync.Mutex
	now := time.Unix(999_999_990, 0) // interval 10: index 99999999 % 3 = 0
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	setNow := func(t time.Time) { mu.Lock(); now = t; mu.Unlock() }
	dev := NewMemDevice(16, 16, 32, "", 0, 0)
	c := NewController(func() (Device, error) { return dev, nil }, Env{Now: clock, Fetch: fetch}, nil)
	c.cache = NewFrameCache(1 << 20)
	c.Apply(proto.DisplaySpec{Mode: "slideshow", Rev: 1, IntervalS: 10, Images: []proto.Media{
		{URL: "http://x/0"}, {URL: "http://x/1"}, {URL: "http://x/2"}}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := newRunState(ctx, c)
	defer r.shutdown()
	go r.loader.run(ctx)
	stepUntil := func(what string, cond func() bool) {
		t.Helper()
		waitFor(t, what, func() bool { r.step(); return cond() })
	}
	stepUntil("first slide", func() bool { return centerOf(dev) == colors[0] })
	if !r.nextAt.Equal(time.Unix(1_000_000_000, 0)) {
		t.Fatalf("next boundary %v", r.nextAt.Unix())
	}
	// Before boundary - interval/2 nothing is prepared.
	setNow(time.Unix(999_999_994, 0))
	r.step()
	if r.prepared {
		t.Fatal("prepared too early")
	}
	setNow(time.Unix(999_999_995, 0))
	stepUntil("prepared next slide", func() bool { return r.prepared && !r.back.pending })
	if centerOf(dev) != colors[0] {
		t.Fatal("prepared frame shown before the boundary")
	}
	if !r.preparedFor.Equal(time.Unix(1_000_000_000, 0)) {
		t.Fatalf("prepared for %v", r.preparedFor.Unix())
	}
	// At the boundary the prepared frame is swapped in without rendering.
	renders := fetches.Load()
	backImg := r.back.img
	setNow(time.Unix(1_000_000_000, 0))
	r.step()
	if centerOf(dev) != colors[1] {
		t.Fatalf("at the boundary the screen shows %v", centerOf(dev))
	}
	if r.front.img != backImg || r.prepared {
		t.Fatal("boundary did not swap in the prepared frame")
	}
	if fetches.Load() != renders {
		t.Fatal("media fetched at the boundary")
	}
	if !r.nextAt.Equal(time.Unix(1_000_000_010, 0)) {
		t.Fatalf("next boundary %v", r.nextAt.Unix())
	}
	// sleepFor aims at the pre-render time, then the boundary.
	if d := r.sleepFor(); d > c.pollEvery {
		t.Fatalf("sleep %v exceeds the poll interval", d)
	}
}

// TestControllerVTSwitch simulates VT release/acquire signals.
func TestControllerVTSwitch(t *testing.T) {
	dev := NewMemDevice(32, 24, 32, "", 0, 0)
	c := NewController(func() (Device, error) { return dev, nil }, fixedEnv(), nil)
	c.Apply(colorSpec("#ff00ff", 1))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := newRunState(ctx, c)
	defer r.shutdown()
	r.step()
	if centerOf(dev) != rgb(255, 0, 255) {
		t.Fatal("not drawn")
	}
	r.onRelease()
	c.Apply(colorSpec("#00ff00", 2))
	r.step()
	if centerOf(dev) == rgb(0, 255, 0) {
		t.Fatal("drew while another VT was in front")
	}
	if st := c.State(); st.Foreground || st.BlankReason != "vt_away" {
		t.Fatalf("state while away %+v", st)
	}
	rows := dev.RowsWritten()
	r.onAcquire()
	r.step()
	if centerOf(dev) != rgb(0, 255, 0) {
		t.Fatal("not repainted after acquire")
	}
	if dev.RowsWritten()-rows != 24 {
		t.Fatalf("acquire repaint wrote %d rows, want the whole screen", dev.RowsWritten()-rows)
	}
	if !c.State().Foreground {
		t.Fatal("foreground not restored")
	}
}

func TestControllerRunTwice(t *testing.T) {
	c := NewController(func() (Device, error) { return NewMemDevice(8, 8, 32, "", 0, 0), nil }, fixedEnv(), nil)
	runController(t, c)
	waitFor(t, "running", func() bool { return c.State().Active })
	if err := c.Run(context.Background()); err == nil {
		t.Fatal("second Run accepted")
	}
}

func TestBlankReasonPrecedence(t *testing.T) {
	tests := []struct {
		spec   proto.DisplaySpec
		blanks []string
		want   string
	}{
		{proto.DisplaySpec{Mode: "status"}, nil, ""},
		{proto.DisplaySpec{Mode: "off", Rev: 3}, nil, "off"},
		{proto.DisplaySpec{Mode: "off", Rev: 3}, []string{"lid"}, "lid"},
		{proto.DisplaySpec{Mode: "status"}, []string{"idle"}, "idle"},
		{proto.DisplaySpec{Mode: "status", Rev: 1}, []string{"idle"}, ""},
		{proto.DisplaySpec{Mode: "text"}, []string{"idle"}, ""},
		{proto.DisplaySpec{Mode: "clock"}, []string{"idle", "power"}, "idle"},
		{proto.DisplaySpec{Mode: "text", Rev: 2}, []string{"idle", "power"}, "power"},
	}
	for _, tt := range tests {
		s := snapshot{spec: tt.spec, blanks: tt.blanks}
		if got := s.blankReason(); got != tt.want {
			t.Errorf("%+v %v: %q, want %q", tt.spec, tt.blanks, got, tt.want)
		}
	}
}

func TestCloneSpec(t *testing.T) {
	img := &proto.Media{URL: "http://a"}
	inner := &proto.DisplaySpec{Mode: "slideshow", Images: []proto.Media{{URL: "http://b"}}}
	s := proto.DisplaySpec{Mode: "wall", Image: img, Images: []proto.Media{{URL: "http://c"}},
		Wall: &proto.WallTile{Content: inner}}
	c := cloneSpec(s)
	img.URL = "changed"
	inner.Images[0].URL = "changed"
	s.Images[0].URL = "changed"
	if c.Image.URL != "http://a" || c.Wall.Content.Images[0].URL != "http://b" || c.Images[0].URL != "http://c" {
		t.Fatalf("clone shares memory: %+v", c)
	}
}

// TestControllerClockStepsBack: when the hive-adjusted clock jumps
// backwards the clock face is redrawn at once instead of waiting for the
// old next-minute deadline.
func TestControllerClockStepsBack(t *testing.T) {
	var mu sync.Mutex
	now := time.Date(2026, 5, 1, 12, 30, 10, 0, time.UTC)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	dev := NewMemDevice(64, 32, 32, "", 0, 0)
	c := NewController(func() (Device, error) { return dev, nil }, Env{Now: clock}, nil)
	c.Apply(proto.DisplaySpec{Mode: "clock", Rev: 1})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := newRunState(ctx, c)
	defer r.shutdown()
	r.step()
	if !r.nextAt.Equal(time.Date(2026, 5, 1, 12, 31, 0, 0, time.UTC)) {
		t.Fatalf("next %v", r.nextAt)
	}
	before := dev.Shows()
	mu.Lock()
	now = time.Date(2026, 5, 1, 9, 15, 0, 0, time.UTC) // stepped back 3 hours
	mu.Unlock()
	r.step()
	if dev.Shows() == before || !r.nextAt.Equal(time.Date(2026, 5, 1, 9, 16, 0, 0, time.UTC)) {
		t.Fatalf("clock not redrawn after stepping back: shows %d next %v", dev.Shows(), r.nextAt)
	}
}

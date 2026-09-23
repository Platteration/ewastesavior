package display

import (
	"context"
	"errors"
	"fmt"
	"image"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/platteration/ewastesavior/internal/proto"
)

// frame is a preallocated render target plus what it currently shows.
type frame struct {
	img     *image.RGBA
	key     string    // content key of the last render (for skipping)
	pending bool      // rendered with media still loading
	next    time.Time // the scene's next change after this frame
}

// runState is owned by the Run goroutine, except the fields under fbMu,
// which the VT goroutine also touches.
type runState struct {
	c   *Controller
	ctx context.Context
	env Env // scene env: async media and stats, device info

	fbMu        sync.Mutex
	dev         Device
	fg          bool // our VT is shown (always true without a VT)
	blanked     bool
	blankMethod string
	blankReason string
	needShow    bool   // the device must be repainted from cur
	cur         *frame // the frame meant to be on screen

	devInfo  proto.DisplayState
	devErr   string
	openErr  string
	openAt   time.Time
	pollAt   time.Time
	vt       vtHandle
	vtErr    string
	rotate   int
	rotWarn  string
	front    *frame
	back     *frame
	shownAny bool
	lastSnap snapshot

	renderedGen uint64
	forceRender bool
	renderedAt  time.Time // scene time of the last render of front
	nextAt      time.Time // scene time of the next change (zero: none)
	prepared    bool      // back holds the frame for preparedFor
	preparedFor time.Time
	renderErr   string
	renderMS    int // last full render
	showMS      int // last Show

	identSeq    uint64
	identActive bool
	identPhase  bool
	identNext   time.Time
	identStop   chan struct{}

	loader     *mediaLoader
	stats      *asyncStats
	mediaGen   uint64
	mediaSize  image.Point
	mediaItems []proto.Media
	mediaDirty atomic.Bool
	statsDirty atomic.Bool

	logged map[string]string // last logged message per category
}

func newRunState(ctx context.Context, c *Controller) *runState {
	c.mu.Lock()
	if c.cache == nil {
		c.cache = NewFrameCache(DefaultCacheBytes())
	}
	c.mu.Unlock()
	r := &runState{c: c, ctx: ctx, fg: true, logged: map[string]string{}, mediaGen: ^uint64(0)}
	r.loader = newMediaLoader(c.env.Fetch, c.cache, func() { r.mediaDirty.Store(true); c.kick() })
	r.env = c.env
	r.env.Fetch = r.loader.get
	if c.env.Stats != nil {
		r.stats = &asyncStats{fetch: c.env.Stats, ctx: ctx, notify: func() { r.statsDirty.Store(true); c.kick() }}
		r.env.Stats = r.stats.get
	}
	r.env.device = func() proto.DisplayState { return r.devInfo }
	return r
}

// locked runs f with fbMu held, releasing it even if f panics.
func (r *runState) locked(f func()) {
	r.fbMu.Lock()
	defer r.fbMu.Unlock()
	f()
}

// logOnce logs msg at most once until it changes for its category.
func (r *runState) logOnce(cat, msg string, warn bool) {
	if r.logged[cat] == msg {
		return
	}
	r.logged[cat] = msg
	if msg == "" {
		return
	}
	if warn {
		r.c.log.Warn("display: "+msg, "what", cat)
	} else {
		r.c.log.Info("display: "+msg, "what", cat)
	}
}

func (r *runState) loop() error {
	go r.loader.run(r.ctx)
	r.c.mu.Lock()
	idle, inputDir := r.c.idleOff, r.c.inputDir
	r.c.mu.Unlock()
	if idle > 0 {
		go watchInput(r.ctx, inputDir, r.c.inputActivity)
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		wait := r.safeStep()
		timer.Reset(wait)
		select {
		case <-r.ctx.Done():
			return nil
		case <-r.c.wake:
		case <-timer.C:
		}
	}
}

// safeStep runs one step and returns how long to sleep. A panic (a bug,
// or a hostile input the scenes did not expect) is logged and reported
// instead of taking the node agent down; the loop retries after a poll
// interval.
func (r *runState) safeStep() (wait time.Duration) {
	defer func() {
		if p := recover(); p != nil {
			msg := fmt.Sprintf("display controller panic: %v", p)
			r.c.log.Error(msg, "stack", firstLines(string(debug.Stack()), 16))
			r.c.mu.Lock()
			r.c.state.Error = msg
			r.c.mu.Unlock()
			r.forceRender = true
			wait = r.c.pollEvery
		}
	}()
	r.step()
	return r.sleepFor()
}

// step brings the screen in line with the desired state.
func (r *runState) step() {
	now := time.Now()
	if !now.Before(r.pollAt) {
		r.pollAt = now.Add(r.c.pollEvery)
		r.poll(now)
	}
	if r.dev == nil && !now.Before(r.openAt) {
		r.openDevice(now)
	}
	snap := r.c.snapshot()
	r.lastSnap = snap
	r.rotWarn = snap.rotateErr
	if r.dev != nil && snap.rotate != r.rotate {
		r.applyRotate(snap.rotate)
	}
	r.updateMedia(snap)
	if r.dev == nil {
		r.publish(snap)
		return
	}
	identActive := snap.ident.seq != 0 && now.Before(snap.ident.until)
	if !identActive && r.identActive {
		r.endIdentify()
	}
	if identActive {
		r.setBlank(false, "")
		r.identify(snap, now)
	} else if reason := snap.blankReason(); reason != "" {
		r.setBlank(true, reason)
	} else {
		r.setBlank(false, "")
		r.scene(snap)
	}
	r.flushPending()
	r.publish(snap)
}

// poll runs every 2 s: device replacement, VT state, idle timer.
func (r *runState) poll(now time.Time) {
	if r.dev != nil {
		if ch, ok := r.dev.(Checker); ok {
			var err error
			r.locked(func() {
				if err = ch.Check(); err != nil {
					r.closeDeviceLocked(err)
				}
			})
			if err != nil {
				r.logOnce("device", "reopening: "+err.Error(), true)
				r.openAt = now
			}
		}
	}
	if r.vt != nil {
		fg := r.vt.Foreground()
		r.locked(func() {
			if fg && !r.fg {
				r.acquireLocked()
			} else if !fg && r.fg {
				r.fg = false
			}
		})
	}
	r.c.checkIdle(now)
}

func safeOpen(open func() (Device, error)) (d Device, err error) {
	defer func() {
		if p := recover(); p != nil {
			d, err = nil, fmt.Errorf("open panic: %v", p)
		}
	}()
	if open == nil {
		return nil, errors.New("no display device configured")
	}
	d, err = open()
	if err == nil && d == nil {
		err = errors.New("no display device")
	}
	return d, err
}

func (r *runState) openDevice(now time.Time) {
	dev, err := safeOpen(r.c.open)
	if err != nil {
		r.openErr = err.Error()
		r.openAt = now.Add(r.c.pollEvery)
		r.logOnce("open", "cannot open display: "+err.Error(), true)
		return
	}
	r.openErr = ""
	r.logOnce("open", "", false)
	r.rotate = 0
	snap := r.c.snapshot()
	if rot, ok := dev.(Rotator); ok {
		if err := rot.SetRotate(snap.rotate); err == nil {
			r.rotate = snap.rotate
		}
	}
	r.fbMu.Lock()
	r.dev = dev
	r.devErr = ""
	r.blanked, r.blankMethod, r.blankReason = false, "", ""
	r.needShow = true
	r.fbMu.Unlock()
	r.refreshInfo()
	r.c.mu.Lock()
	vtPath := r.c.vtPath
	r.c.mu.Unlock()
	if u, ok := dev.(vtUser); ok && u.usesVT() && r.vt == nil && vtPath != "" {
		vt, err := openVT(vtPath, r.onRelease, r.onAcquire)
		if err != nil {
			r.vtErr = "no display VT: " + err.Error()
			r.logOnce("vt", r.vtErr, true)
		} else {
			r.vt, r.vtErr = vt, ""
		}
	}
	if r.vt != nil {
		fg := r.vt.Foreground()
		r.fbMu.Lock()
		r.fg = fg
		r.fbMu.Unlock()
	}
	r.c.log.Info("display opened", "device", r.devInfo.Device, "driver", r.devInfo.Driver,
		"format", r.devInfo.Format, "size", fmt.Sprintf("%dx%d", r.devInfo.FBWidth, r.devInfo.FBHeight), "rotate", r.rotate)
}

// refreshInfo re-reads the device description and sizes the frames to
// the logical screen.
func (r *runState) refreshInfo() {
	var info proto.DisplayState
	var pw, ph int
	r.locked(func() {
		info = r.dev.Info()
		pw, ph = r.dev.Size()
	})
	w, h := info.Width, info.Height
	if w <= 0 || h <= 0 {
		w, h = pw, ph
		if r.rotate == 90 || r.rotate == 270 {
			w, h = ph, pw
		}
	}
	info.Width, info.Height, info.Rotate = w, h, r.rotate
	r.devInfo = info
	// Two screen-sized frames must fit (current and next slide), even on
	// machines where 10% of RAM is less.
	r.c.cache.ensureBudget(2*int64(w)*int64(h)*4 + 1<<20)
	if r.front == nil || r.front.img.Rect.Dx() != w || r.front.img.Rect.Dy() != h {
		r.front = &frame{img: image.NewRGBA(image.Rect(0, 0, w, h))}
		r.back = &frame{img: image.NewRGBA(image.Rect(0, 0, w, h))}
		r.fbMu.Lock()
		r.cur = r.front
		r.fbMu.Unlock()
	} else {
		r.front.key, r.back.key = "", ""
	}
	r.forceRender = true
	r.prepared = false
	r.identSeq = 0 // rebuild the identify overlay at the new size
}

func (r *runState) applyRotate(deg int) {
	rot, ok := r.dev.(Rotator)
	if !ok {
		r.rotWarn = "this display cannot be rotated"
		return
	}
	var err error
	r.locked(func() {
		r.needShow = true
		err = rot.SetRotate(deg)
	})
	if err != nil {
		r.rotWarn = err.Error()
		return
	}
	r.rotate = deg
	r.refreshInfo()
}

// closeDeviceLocked drops the device after an error (fbMu held).
func (r *runState) closeDeviceLocked(err error) {
	if r.dev == nil {
		return
	}
	_ = r.dev.Close()
	r.dev = nil
	r.blanked, r.blankMethod, r.blankReason = false, "", ""
	r.devErr = err.Error()
}

// onRelease runs on the VT goroutine before another VT is shown.
func (r *runState) onRelease() {
	defer r.c.kick()
	r.locked(func() {
		r.fg = false
		if r.blanked && r.dev != nil { // let the text console be seen
			r.blanked, r.blankMethod = false, ""
			_, _ = r.dev.Blank(false)
		}
	})
}

// onAcquire runs on the VT goroutine when our VT is shown again.
func (r *runState) onAcquire() {
	defer r.c.kick()
	r.locked(r.acquireLocked)
}

func (r *runState) acquireLocked() {
	r.fg = true
	if inv, ok := r.dev.(Invalidator); ok && r.dev != nil {
		inv.Invalidate()
	}
	r.needShow = true
}

// setBlank blanks or unblanks the device (only while we own the screen).
func (r *runState) setBlank(on bool, reason string) {
	r.fbMu.Lock()
	defer r.fbMu.Unlock()
	if r.dev == nil || !r.fg {
		return
	}
	if on {
		r.blankReason = reason
		if r.blanked {
			return
		}
		m, err := r.dev.Blank(true)
		if err != nil {
			r.logOnce("blank", "blank failed: "+err.Error(), true)
			if errors.Is(err, ErrDeviceGone) {
				r.closeDeviceLocked(err)
				r.openAt = time.Now()
			}
			return
		}
		r.blanked, r.blankMethod = true, m
		r.c.log.Info("display blanked", "reason", reason, "method", m)
		return
	}
	if !r.blanked {
		return
	}
	if _, err := r.dev.Blank(false); err != nil {
		r.logOnce("blank", "unblank failed: "+err.Error(), true)
	}
	r.blanked, r.blankMethod, r.blankReason = false, "", ""
	if inv, ok := r.dev.(Invalidator); ok {
		inv.Invalidate()
	}
	r.needShow = true
	r.forceRender = true // the scene may be stale (clock)
}

// sleepFor returns how long to wait before the next step.
func (r *runState) sleepFor() time.Duration {
	now := time.Now()
	d := r.pollAt.Sub(now)
	if r.dev == nil {
		d = min(d, r.openAt.Sub(now))
	}
	if r.identActive {
		d = min(d, r.identNext.Sub(now), r.lastSnap.ident.until.Sub(now))
	}
	if !r.nextAt.IsZero() {
		sceneNow := r.env.now()
		d = min(d, r.nextAt.Sub(sceneNow))
		if timed, ok := timedMedia(r.lastSnap.spec); ok && !r.prepared {
			d = min(d, r.nextAt.Add(-intervalOf(timed)/2).Sub(sceneNow))
		}
	}
	return max(min(d, r.c.pollEvery), 5*time.Millisecond)
}

// publish stores the DisplayState reported to the hive.
func (r *runState) publish(snap snapshot) {
	r.fbMu.Lock()
	active := r.dev != nil
	fg, blanked, method, reason := r.fg, r.blanked, r.blankMethod, r.blankReason
	devErr := r.devErr
	r.fbMu.Unlock()
	st := proto.DisplayState{}
	if active {
		st = r.devInfo
	}
	st.Active = active
	st.Rotate = r.rotate
	st.Mode, st.Rev = snap.spec.Mode, snap.spec.Rev
	st.Loaded, st.Total, st.MediaErrors = r.loader.progress(r.mediaItems)
	st.Ready = st.Loaded == st.Total
	st.Foreground = fg
	st.Blanked, st.BlankMethod = blanked, method
	if blanked {
		st.BlankReason = reason
	}
	if active && !fg {
		st.Blanked, st.BlankReason, st.BlankMethod = true, "vt_away", ""
	}
	st.RenderMS = r.renderMS + r.showMS
	var warns []string
	if active && r.devInfo.Warning != "" {
		warns = append(warns, r.devInfo.Warning)
	}
	for _, w := range []string{r.vtErr, r.rotWarn} {
		if w != "" {
			warns = append(warns, w)
		}
	}
	st.Warning = strings.Join(warns, "; ")
	switch {
	case snap.specErr != "":
		st.Error = snap.specErr
	case r.renderErr != "":
		st.Error = r.renderErr
	case r.openErr != "" && !active:
		st.Error = r.openErr
	case devErr != "":
		st.Error = devErr
	}
	r.c.mu.Lock()
	r.c.state = st
	r.c.mu.Unlock()
}

// shutdown releases the device and the VT and marks the display inactive.
func (r *runState) shutdown() {
	if r.identStop != nil {
		close(r.identStop)
		r.identStop = nil
	}
	func() {
		defer func() { _ = recover() }() // a broken device must not block shutdown
		r.locked(func() {
			if r.dev != nil {
				d := r.dev
				r.dev = nil
				if r.blanked {
					_, _ = d.Blank(false)
				}
				_ = d.Close()
			}
		})
	}()
	if r.vt != nil {
		_ = r.vt.Close()
		r.vt = nil
	}
	r.c.mu.Lock()
	r.c.state.Active = false
	r.c.state.Foreground = false
	r.c.mu.Unlock()
}

package display

import (
	"errors"
	"fmt"
	"image"
	"time"

	"github.com/platteration/ewastesavior/internal/proto"
)

// This file holds the frame side of the run loop: scene rendering and
// slideshow pre-rendering, showing frames, the identify overlay and media
// bookkeeping.

// timedMedia reports specs whose image changes over time (pre-render).
func timedMedia(spec proto.DisplaySpec) (proto.DisplaySpec, bool) {
	if spec.Mode == proto.DisplaySlideshow {
		return spec, true
	}
	if spec.Mode == proto.DisplayWall && spec.Wall != nil && spec.Wall.Content != nil && spec.Wall.Content.Mode == proto.DisplaySlideshow {
		return *spec.Wall.Content, true
	}
	return spec, false
}

// scene renders the current spec when it changed, when its next change
// time arrives, or when loading media/stats arrive, and pre-renders the
// next slideshow frame from half an interval before the boundary so the
// swap at the boundary is only a copy.
func (r *runState) scene(snap snapshot) {
	sceneNow := r.env.now()
	// A clock stepped backwards (hive time applied, RTC fixed) would leave
	// nextAt far in the future and the scene stale: start over.
	force := snap.gen != r.renderedGen || r.forceRender || sceneNow.Before(r.renderedAt.Add(-time.Second))
	if force {
		r.prepared = false
	}
	mediaDirty := r.mediaDirty.Swap(false)
	statsDirty := r.statsDirty.Swap(false)
	due := force || (!r.nextAt.IsZero() && !sceneNow.Before(r.nextAt))
	due = due || (mediaDirty && r.front.pending) || (statsDirty && snap.spec.Mode == proto.DisplayDashboard)
	timed, isTimed := timedMedia(snap.spec)
	if due {
		boundary := !force && !r.nextAt.IsZero() && !sceneNow.Before(r.nextAt)
		if boundary && r.prepared && r.preparedFor.Equal(r.nextAt) && !r.back.pending {
			r.front, r.back = r.back, r.front
			r.prepared = false
			r.nextAt = r.front.next
			r.renderedAt = sceneNow
			r.markShow(r.front)
		} else {
			res := r.render(r.front, snap, sceneNow)
			r.nextAt = res.next
			r.renderedAt = sceneNow
			// At a slideshow boundary whose media is still loading, keep
			// the previous picture up until it arrives.
			keepOld := boundary && isTimed && res.pending && r.shownAny
			if res.drawn && !keepOld {
				r.markShow(r.front)
			}
		}
		r.renderedGen = snap.gen
		r.forceRender = false
	}
	if isTimed && !r.nextAt.IsZero() && (!r.prepared || (mediaDirty && r.back.pending)) {
		if !sceneNow.Before(r.nextAt.Add(-intervalOf(timed) / 2)) {
			r.render(r.back, snap, r.nextAt)
			r.prepared, r.preparedFor = true, r.nextAt
		}
	}
}

// markShow schedules f to be shown by flushPending.
func (r *runState) markShow(f *frame) {
	r.fbMu.Lock()
	r.cur, r.needShow = f, true
	r.fbMu.Unlock()
}

// render draws the scene at scene time `at` into f.
func (r *runState) render(f *frame, snap snapshot, at time.Time) frameResult {
	start := time.Now()
	if snap.specErr != "" {
		key := "specerr|" + snap.specErr
		if f.key != key {
			drawErrorScreen(f.img, "Invalid display spec", snap.specErr)
			f.key = key
		}
		f.pending, f.next = false, time.Time{}
		return frameResult{drawn: true, key: key}
	}
	env := r.env
	env.Now = func() time.Time { return at }
	res := renderFrame(r.ctx, snap.spec, f.img, env, f.key)
	if res.err != nil {
		msg := res.err.Error()
		r.renderErr = firstLines(msg, 1)
		r.logOnce("render", msg, true)
		func() {
			defer func() {
				if recover() != nil {
					fill(f.img, f.img.Rect, rgb(0x5a, 0x10, 0x10))
				}
			}()
			drawErrorScreen(f.img, "Display error", r.renderErr)
		}()
		res.drawn, res.key, res.pending = true, "", false
		res.next = at.Add(30 * time.Second) // try again later
	} else {
		r.renderErr = ""
		r.logOnce("render", "", false)
	}
	f.key, f.pending, f.next = res.key, res.pending, res.next
	if res.drawn {
		r.renderMS = int(time.Since(start) / time.Millisecond)
	}
	return res
}

// flushPending shows cur if the device needs repainting.
func (r *runState) flushPending() {
	r.fbMu.Lock()
	defer r.fbMu.Unlock()
	f := r.cur
	if f == nil {
		f = r.front
	}
	if !r.needShow || r.dev == nil || !r.fg || r.blanked || f == nil {
		return
	}
	start := time.Now()
	err := r.dev.Show(f.img, nil)
	if err != nil {
		r.logOnce("show", "show failed: "+err.Error(), true)
		if errors.Is(err, ErrDeviceGone) {
			r.closeDeviceLocked(err)
			r.openAt = time.Now()
		} else {
			r.devErr = err.Error()
		}
		return
	}
	r.devErr = ""
	r.logOnce("show", "", false)
	r.needShow = false
	r.shownAny = true
	r.showMS = int(time.Since(start) / time.Millisecond)
}

// identify runs the flashing identify overlay over the scene.
func (r *runState) identify(snap snapshot, now time.Time) {
	if !r.identActive || snap.ident.seq != r.identSeq {
		r.identActive, r.identSeq = true, snap.ident.seq
		if r.forceRender || snap.gen != r.renderedGen || r.front.key == "" {
			r.render(r.front, snap, r.env.now())
			r.renderedGen = snap.gen
		}
		copy(r.back.img.Pix, r.front.img.Pix)
		st := r.safeStatus()
		name, code := st.Name, snap.ident.code
		if code == "" {
			code = st.ShortCode
		}
		if code == "" && st.NodeID != "" {
			code = proto.ShortCode(st.NodeID)
		}
		label := snap.ident.label
		if label == "" && snap.spec.Wall != nil {
			label = snap.spec.Wall.Label
			if label == "" {
				label = fmt.Sprintf("wall row %d, column %d", snap.spec.Wall.Row+1, snap.spec.Wall.Col+1)
			}
		}
		drawIdentify(r.front.img, code, name, label, false)
		drawIdentify(r.back.img, code, name, label, true)
		r.front.key, r.back.key = "", ""
		r.prepared = false
		r.identPhase, r.identNext = false, now
		r.startBeep()
		r.c.log.Info("display identify", "code", code, "until", snap.ident.until.Format(time.TimeOnly))
	}
	if now.Before(r.identNext) {
		return
	}
	f := r.front
	if r.identPhase {
		f = r.back
	}
	r.markShow(f)
	r.flushPending()
	if r.vt != nil {
		r.vt.SetLEDs(!r.identPhase, false)
	}
	r.identPhase = !r.identPhase
	r.identNext = now.Add(r.c.flashEvery)
}

// safeStatus calls env.Status outside a render, surviving a panic.
func (r *runState) safeStatus() (st StatusInfo) {
	defer func() {
		if recover() != nil {
			st = StatusInfo{}
		}
	}()
	if r.env.Status != nil {
		st = r.env.Status()
	}
	return st
}

func (r *runState) startBeep() {
	if r.identStop != nil {
		close(r.identStop)
	}
	stop := make(chan struct{})
	r.identStop = stop
	vt := r.vt
	if vt == nil {
		return
	}
	go func() {
		defer vt.Tone(0)
		for i := 0; i < 3; i++ {
			vt.Tone(880)
			select {
			case <-stop:
				return
			case <-time.After(150 * time.Millisecond):
			}
			vt.Tone(0)
			select {
			case <-stop:
				return
			case <-time.After(250 * time.Millisecond):
			}
		}
	}()
}

func (r *runState) endIdentify() {
	r.identActive = false
	r.identSeq = 0
	if r.identStop != nil {
		close(r.identStop)
		r.identStop = nil
	}
	if r.vt != nil {
		r.vt.SetLEDs(false, true)
	}
	r.front.key, r.back.key = "", ""
	r.forceRender = true
}

// updateMedia tells the loader which media the current spec needs, so
// they load (and fail) early and Loaded/Total are meaningful.
func (r *runState) updateMedia(snap snapshot) {
	size := image.Point{}
	if r.front != nil {
		size = r.front.img.Rect.Size()
	}
	if snap.gen == r.mediaGen && size == r.mediaSize {
		return
	}
	r.mediaGen, r.mediaSize = snap.gen, size
	spec := snap.spec
	var items []proto.Media
	var req FetchRequest
	content := spec
	if spec.Mode == proto.DisplayWall && spec.Wall != nil && spec.Wall.Content != nil {
		t := spec.Wall
		content = *t.Content
		req = FetchRequest{CanvasW: t.CanvasW, CanvasH: t.CanvasH, Rect: image.Rect(t.X, t.Y, t.X+t.W, t.Y+t.H),
			PixelW: size.X, PixelH: size.Y, Fit: fitOf(content.Fit)}
	} else {
		req = FetchRequest{CanvasW: size.X, CanvasH: size.Y, Rect: image.Rectangle{Max: size},
			PixelW: size.X, PixelH: size.Y, Fit: fitOf(spec.Fit)}
	}
	switch content.Mode {
	case proto.DisplayImage:
		if content.Image != nil {
			items = []proto.Media{*content.Image}
		}
	case proto.DisplaySlideshow:
		if n := len(content.Images); n > 0 {
			// Load in showing order, starting with the current slide.
			idx, _ := slideIndex(r.env.now(), content.IntervalS, n)
			items = append(append(items, content.Images[idx:]...), content.Images[:idx]...)
		}
	}
	if snap.specErr != "" {
		items = nil
	}
	r.mediaItems = items
	r.loader.setSpec(items, req)
}

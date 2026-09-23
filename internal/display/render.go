package display

import (
	"context"
	"errors"
	"fmt"
	"image"
	"runtime/debug"
	"strings"
	"time"

	"github.com/platteration/ewastesavior/internal/proto"
)

// Env supplies scenes with time, node status, swarm statistics and media.
// Every field is optional.
type Env struct {
	// Now is the hive-adjusted clock (slideshow and wall sync); nil means
	// time.Now.
	Now func() time.Time
	// Status describes this node for the status scene.
	Status func() StatusInfo
	// Stats fetches swarm statistics for the dashboard scene.
	Stats func(ctx context.Context) (*proto.SwarmStats, error)
	// Fetch loads a media item, cropped and scaled as req describes. The
	// returned image must have exactly PixelW×PixelH pixels; areas not
	// covered by the image should be transparent. It is treated as
	// read-only and may be shared with a cache.
	Fetch func(ctx context.Context, m proto.Media, req FetchRequest) (image.Image, error)
	// Location is the configured timezone for clocks (nil = UTC).
	Location *time.Location

	// device describes the display (resolution, format) for the test
	// pattern; set by the controller and the CLI.
	device func() proto.DisplayState
}

func (e *Env) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// FetchRequest describes the pixels wanted from a media item: the image is
// fitted (Fit: contain, cover or stretch) into a CanvasW×CanvasH canvas,
// the canvas rectangle Rect is cut out and scaled to PixelW×PixelH. For a
// plain screen the canvas is the screen and Rect covers all of it; for a
// wall tile the canvas is the wall (in millimeters). It maps 1:1 onto the
// hive's /api/v1/blobs/{sha}/render?cw&ch&x&y&w&h&pw&ph&fit endpoint.
type FetchRequest struct {
	CanvasW, CanvasH int
	Rect             image.Rectangle
	PixelW, PixelH   int
	Fit              string
}

// key is a stable cache key for the request.
func (r FetchRequest) key() string {
	return fmt.Sprintf("%dx%d|%d,%d,%d,%d|%dx%d|%s", r.CanvasW, r.CanvasH,
		r.Rect.Min.X, r.Rect.Min.Y, r.Rect.Max.X, r.Rect.Max.Y, r.PixelW, r.PixelH, r.Fit)
}

func (r FetchRequest) valid() error {
	if r.CanvasW <= 0 || r.CanvasH <= 0 || r.PixelW <= 0 || r.PixelH <= 0 || r.Rect.Empty() ||
		r.PixelW > 16384 || r.PixelH > 16384 {
		return fmt.Errorf("invalid fetch request %s", r.key())
	}
	return nil
}

// StatusInfo is what the status scene shows about this node.
type StatusInfo struct {
	Name, ShortCode, NodeID, Version string
	Link                             proto.HiveLink
	HiveAddr, HiveError, Message     string
	Addrs                            []string
	Roles                            []proto.Role
	Metrics                          proto.Metrics
	Inventory                        proto.Inventory
	RunningTasks                     int
	PowerReason                      string
	Hive                             *HivePanel // set on a node running the hive
}

// HivePanel is the extra status panel of a SaviorOS hive (DESIGN 9).
type HivePanel struct {
	URLs        []string
	Fingerprint string
	PairCode    string
	NodesOnline int
	Persistent  bool
}

// errPending means media or data is still loading asynchronously; the
// scene draws a placeholder and the controller renders again when it
// arrives.
var errPending = errors.New("still loading")

// MediaError is a media item that could not be loaded or decoded.
type MediaError struct {
	Media string // short description: "blob 1a2b3c4d5e6f" or the URL
	Err   error
}

// Error implements error.
func (e *MediaError) Error() string { return e.Media + ": " + e.Err.Error() }

// Unwrap returns the underlying error (e.g. imaging.ErrTooLarge).
func (e *MediaError) Unwrap() error { return e.Err }

// mediaName describes m for errors and placeholders.
func mediaName(m proto.Media) string {
	if m.Blob != "" {
		return "blob " + m.Blob[:min(12, len(m.Blob))]
	}
	u := proto.Sanitize(m.URL, 80, false) // cut on a rune boundary
	if len(u) < len(m.URL) {
		u += "…"
	}
	return u
}

// frameResult is the outcome of rendering one frame.
type frameResult struct {
	next      time.Time // when the scene changes next (zero: never by itself)
	key       string    // content identity; equal keys mean identical frames
	drawn     bool      // dst was modified
	pending   bool      // some media or data is still loading
	mediaErrs []error
	err       error // render failure (for example a recovered panic)
}

// Render draws one frame of spec into dst and returns when the scene next
// changes (zero if it only changes when the spec does). Media failures are
// drawn as placeholders and returned as a joined *MediaError error; a
// panic while drawing is recovered and returned as an error.
func Render(ctx context.Context, spec proto.DisplaySpec, dst *image.RGBA, env Env) (next time.Time, err error) {
	res := renderFrame(ctx, spec, dst, env, "")
	if res.err != nil {
		return res.next, res.err
	}
	return res.next, errors.Join(res.mediaErrs...)
}

// renderFrame renders spec unless its content key equals prevKey.
func renderFrame(ctx context.Context, spec proto.DisplaySpec, dst *image.RGBA, env Env, prevKey string) (res frameResult) {
	defer func() {
		if r := recover(); r != nil {
			res = frameResult{err: fmt.Errorf("render panic: %v\n%s", r, firstLines(string(debug.Stack()), 12)), drawn: true}
		}
	}()
	if dst == nil || dst.Rect.Empty() {
		return frameResult{err: errors.New("render: empty destination")}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// Scenes draw at origin 0,0; Pix already starts at dst.Rect.Min.
	view := &image.RGBA{Pix: dst.Pix, Stride: dst.Stride, Rect: image.Rect(0, 0, dst.Rect.Dx(), dst.Rect.Dy())}
	s := &scene{ctx: ctx, dst: view, env: env, now: env.now(), w: view.Rect.Dx(), h: view.Rect.Dy(), prevKey: prevKey}
	s.draw(spec)
	return s.res
}

func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

// scene carries one render.
type scene struct {
	ctx     context.Context
	dst     *image.RGBA
	env     Env
	now     time.Time
	w, h    int
	prevKey string
	res     frameResult
}

// unchanged records key and reports whether the previous frame had the
// same content (so drawing can be skipped).
func (s *scene) unchanged(key string) bool {
	s.res.key = key
	if key != "" && key == s.prevKey {
		return true
	}
	s.res.drawn = true
	return false
}

func (s *scene) draw(spec proto.DisplaySpec) {
	switch spec.Mode {
	case proto.DisplayStatus, "":
		s.status()
	case proto.DisplayOff:
		if !s.unchanged(fmt.Sprintf("off|%dx%d", s.w, s.h)) {
			fill(s.dst, s.dst.Rect, colBlack)
		}
	case proto.DisplayColor:
		bg := specColor(spec.BG, colBlack)
		if !s.unchanged(fmt.Sprintf("color|%dx%d|%v", s.w, s.h, bg)) {
			fill(s.dst, s.dst.Rect, bg)
		}
	case proto.DisplayText:
		s.text(spec)
	case proto.DisplayClock:
		s.clock(spec)
	case proto.DisplayImage, proto.DisplaySlideshow:
		s.media(spec)
	case proto.DisplayDashboard:
		s.dashboard()
	case proto.DisplayWall:
		s.wall(spec)
	case proto.DisplayTest:
		s.testPattern()
	default:
		s.res.err = fmt.Errorf("unknown display mode %q", proto.Sanitize(spec.Mode, 32, false))
	}
}

// fetch loads a media item through env.Fetch, recording pending state and
// media errors for the caller.
func (s *scene) fetch(m proto.Media, req FetchRequest) (image.Image, error) {
	if s.env.Fetch == nil {
		err := &MediaError{Media: mediaName(m), Err: errors.New("no media source configured")}
		s.res.mediaErrs = append(s.res.mediaErrs, err)
		return nil, err
	}
	img, err := s.env.Fetch(s.ctx, m, req)
	if errors.Is(err, errPending) {
		s.res.pending = true
		return nil, err
	}
	if err == nil && img == nil {
		err = errors.New("fetch returned no image")
	}
	if err != nil {
		var me *MediaError
		if !errors.As(err, &me) {
			me = &MediaError{Media: mediaName(m), Err: err}
		}
		s.res.mediaErrs = append(s.res.mediaErrs, me)
		return nil, me
	}
	return img, nil
}

// slideIndex returns the slideshow position at t: floor(unix/interval)
// mod n, and the time of the next change.
func slideIndex(t time.Time, intervalS, n int) (int, time.Time) {
	if intervalS <= 0 {
		intervalS = proto.DefaultInterval
	}
	if n <= 0 {
		return 0, time.Time{}
	}
	iv := int64(intervalS)
	k := floorDiv(t.Unix(), iv)
	idx := k % int64(n)
	if idx < 0 {
		idx += int64(n)
	}
	return int(idx), time.Unix((k+1)*iv, 0)
}

// intervalOf returns a slideshow's effective interval.
func intervalOf(spec proto.DisplaySpec) time.Duration {
	iv := spec.IntervalS
	if iv <= 0 {
		iv = proto.DefaultInterval
	}
	return time.Duration(iv) * time.Second
}

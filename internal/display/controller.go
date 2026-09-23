package display

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/platteration/ewastesavior/internal/proto"
)

const (
	pollInterval    = 2 * time.Second // device replacement / VT / idle checks
	flashInterval   = 500 * time.Millisecond
	defaultIdentify = 30 * time.Second
	maxIdentify     = 600 * time.Second
)

// Controller drives a display (DESIGN 11): it renders the current spec
// into preallocated frames, shows them on the device, reopens the device
// when it is replaced, owns the display VT, blanks for lid/idle/off, runs
// the identify overlay and loads media in the background.
//
// All methods are safe for concurrent use; Run does the work.
type Controller struct {
	open func() (Device, error)
	env  Env
	log  *slog.Logger

	mu        sync.Mutex
	spec      proto.DisplaySpec
	specErr   string
	gen       uint64 // bumped when the spec changes
	rotate    int
	rotateErr string
	ident     identifyReq
	blanks    map[string]bool
	state     proto.DisplayState
	vtPath    string
	idleOff   time.Duration
	lastInput time.Time
	running   bool
	cache     *FrameCache

	wake       chan struct{}
	pollEvery  time.Duration
	flashEvery time.Duration
	inputDir   string
}

type identifyReq struct {
	seq         uint64
	until       time.Time // monotonic deadline
	code, label string
}

// NewController returns a controller that obtains its device from open,
// which is called again whenever the device must be reopened (it should
// honor display_device, e.g. OpenDevice(cfg.DisplayDevice, 0)). Rotation is
// applied by the controller (SetRotate). The initial spec is the status
// screen; log may be nil.
func NewController(open func() (Device, error), env Env, log *slog.Logger) *Controller {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Controller{
		open:       open,
		env:        env,
		log:        log,
		spec:       proto.DisplaySpec{Mode: proto.DisplayStatus},
		blanks:     map[string]bool{},
		vtPath:     DefaultVT,
		wake:       make(chan struct{}, 1),
		pollEvery:  pollInterval,
		flashEvery: flashInterval,
		inputDir:   "/dev/input",
		state:      proto.DisplayState{Mode: proto.DisplayStatus},
	}
}

func (c *Controller) kick() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// Apply sets the desired display spec. A spec with Rev 0 is treated as the
// node's local default (only then does the idle timer blank the screen). An
// invalid spec is shown as an error screen and reported in State().Error.
func (c *Controller) Apply(spec proto.DisplaySpec) {
	spec = cloneSpec(spec)
	errText := ""
	if err := proto.ValidateDisplaySpec(&spec); err != nil {
		errText = "invalid display spec: " + err.Error()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if errText == c.specErr && reflect.DeepEqual(spec, c.spec) {
		return
	}
	c.spec, c.specErr = spec, errText
	c.gen++
	c.kick()
}

// cloneSpec deep-copies a spec so callers may reuse theirs.
func cloneSpec(s proto.DisplaySpec) proto.DisplaySpec {
	if s.Image != nil {
		m := *s.Image
		s.Image = &m
	}
	s.Images = append([]proto.Media(nil), s.Images...)
	if s.Wall != nil {
		w := *s.Wall
		if w.Content != nil {
			c := cloneSpec(*w.Content)
			w.Content = &c
		}
		s.Wall = &w
	}
	return s
}

// SetRotate sets the rotation (0, 90, 180 or 270 degrees clockwise).
// Other values are ignored with a warning in State().
func (c *Controller) SetRotate(deg int) {
	d, err := normRotate(deg)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		c.rotateErr = err.Error()
		return
	}
	c.rotateErr = ""
	if d != c.rotate {
		c.rotate = d
		c.kick()
	}
}

// Identify shows the identify overlay for d (default 30 s, at most 10
// min): code (the short code), the node name and label (wall position; the
// wall tile's label when empty), flashing, and unblanks the screen,
// blinks the keyboard LEDs and beeps when the display VT is available.
func (c *Controller) Identify(d time.Duration, code, label string) {
	if d <= 0 {
		d = defaultIdentify
	}
	d = min(d, maxIdentify)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ident = identifyReq{seq: c.ident.seq + 1, until: time.Now().Add(d), code: code, label: label}
	c.kick()
}

// SetBlank sets or clears a blank reason ("lid", "idle", ...). The screen
// is blanked while any reason holds, except that "idle" only applies to
// the local default status/clock screen (spec Rev 0).
func (c *Controller) SetBlank(reason string, on bool) {
	reason = strings.ToLower(strings.TrimSpace(reason))
	if reason == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.blanks[reason] == on {
		return
	}
	if on {
		c.blanks[reason] = true
	} else {
		delete(c.blanks, reason)
	}
	c.kick()
}

// SetVT sets the display VT (default /dev/tty7); "" disables VT handling.
// It takes effect when the device is next opened.
func (c *Controller) SetVT(path string) {
	c.mu.Lock()
	c.vtPath = path
	c.mu.Unlock()
}

// SetIdleOff blanks the local default screen after d without input on
// /dev/input/event* (display_idle_off_min); 0 disables. Call before Run.
func (c *Controller) SetIdleOff(d time.Duration) {
	c.mu.Lock()
	c.idleOff = max(d, 0)
	c.mu.Unlock()
}

// SetCache replaces the frame cache (to share one with a URLFetcher).
// Call before Run.
func (c *Controller) SetCache(fc *FrameCache) {
	c.mu.Lock()
	c.cache = fc
	c.mu.Unlock()
}

// State returns the current display state for heartbeats.
func (c *Controller) State() proto.DisplayState {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.state
	st.MediaErrors = append([]string(nil), st.MediaErrors...)
	return st
}

// inputActivity is called on keyboard/mouse input: it ends idle blanking.
func (c *Controller) inputActivity() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastInput = time.Now()
	if c.blanks["idle"] {
		delete(c.blanks, "idle")
		c.kick()
	}
}

// checkIdle sets the idle blank reason after idleOff without input.
func (c *Controller) checkIdle(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.idleOff > 0 && !c.blanks["idle"] && now.Sub(c.lastInput) >= c.idleOff {
		c.blanks["idle"] = true
	}
}

// snapshot is a consistent copy of the desired state for one step.
type snapshot struct {
	spec      proto.DisplaySpec
	specErr   string
	gen       uint64
	rotate    int
	rotateErr string
	ident     identifyReq
	blanks    []string
}

func (c *Controller) snapshot() snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := snapshot{spec: c.spec, specErr: c.specErr, gen: c.gen, rotate: c.rotate, rotateErr: c.rotateErr, ident: c.ident}
	for r := range c.blanks {
		s.blanks = append(s.blanks, r)
	}
	sort.Strings(s.blanks)
	return s
}

// localDefault reports whether spec is the node's own default screen.
func localDefault(spec proto.DisplaySpec) bool {
	return spec.Rev == 0 && (spec.Mode == proto.DisplayStatus || spec.Mode == proto.DisplayClock || spec.Mode == "")
}

// blankReason decides whether the screen should be dark and why:
// lid first, then mode off, then idle (local default only), then others.
func (s snapshot) blankReason() string {
	has := func(r string) bool {
		for _, b := range s.blanks {
			if b == r {
				return true
			}
		}
		return false
	}
	switch {
	case has("lid"):
		return "lid"
	case s.spec.Mode == proto.DisplayOff && s.specErr == "":
		return "off"
	case has("idle") && localDefault(s.spec):
		return "idle"
	}
	for _, b := range s.blanks {
		if b != "idle" && b != "lid" {
			return b
		}
	}
	return ""
}

// Run drives the display until ctx is canceled. It never gives up: when
// no display can be opened it keeps retrying every 2 s and reports why in
// State().Error.
func (c *Controller) Run(ctx context.Context) error {
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return errors.New("display controller already running")
	}
	c.running = true
	c.lastInput = time.Now()
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.running = false
		c.mu.Unlock()
	}()
	r := newRunState(ctx, c)
	defer r.shutdown()
	return r.loop()
}

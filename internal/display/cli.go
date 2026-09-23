package display

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"image"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/platteration/ewastesavior/internal/proto"
	"github.com/platteration/ewastesavior/internal/version"
)

// Main implements `savior display <command>`.
func Main(args []string) int {
	return runCLI(args, os.Stdin, os.Stdout, os.Stderr)
}

const cliUsage = `usage: savior display <command> [flags]

commands:
  test      cycle test patterns and solid colors on the screen (dead pixels, format)
  render    render a display spec to a PNG file (no screen needed)
  show      show a display spec on the screen
  vt-reset  return the display VT to text mode (after a crash)

Run 'savior display <command> -h' for flags.`

func runCLI(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, cliUsage)
		return 2
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "test":
		return cmdTest(rest, stderr)
	case "render":
		return cmdRender(rest, stdin, stdout, stderr)
	case "show":
		return cmdShow(rest, stdin, stderr)
	case "vt-reset":
		return cmdVTReset(rest, stderr)
	case "help", "-h", "--help":
		fmt.Fprintln(stdout, cliUsage)
		return 0
	}
	fmt.Fprintf(stderr, "savior display: unknown command %q\n\n%s\n", cmd, cliUsage)
	return 2
}

// parseSize parses "WxH".
func parseSize(s string) (int, int, error) {
	a, b, ok := strings.Cut(strings.ToLower(strings.TrimSpace(s)), "x")
	w, err1 := strconv.Atoi(a)
	h, err2 := strconv.Atoi(b)
	if !ok || err1 != nil || err2 != nil || w < 1 || h < 1 || w > 16384 || h > 16384 {
		return 0, 0, fmt.Errorf("invalid size %q (want WxH, e.g. 1024x768)", s)
	}
	return w, h, nil
}

// readSpec loads and validates a DisplaySpec from a JSON file ("-" = stdin).
func readSpec(path string, stdin io.Reader) (proto.DisplaySpec, error) {
	var spec proto.DisplaySpec
	var data []byte
	var err error
	if path == "-" {
		data, err = io.ReadAll(io.LimitReader(stdin, 1<<20))
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return spec, fmt.Errorf("read spec: %w", err)
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&spec); err != nil {
		return spec, fmt.Errorf("parse spec %s: %w", path, err)
	}
	if err := proto.ValidateDisplaySpec(&spec); err != nil {
		return spec, fmt.Errorf("invalid spec: %w", err)
	}
	return spec, nil
}

// localStatus describes this machine for CLI renders (no agent running).
func localStatus() StatusInfo {
	host, _ := os.Hostname()
	st := StatusInfo{Name: host, Version: version.Version, Link: proto.LinkSearching,
		Roles: []proto.Role{proto.RoleDisplay}, Metrics: proto.Metrics{BatteryPercent: -1},
		Message: "savior display (no node agent)"}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && ipn.IP.To4() != nil {
				st.Addrs = append(st.Addrs, ipn.String())
			}
		}
	}
	return st
}

func noFetch(_ context.Context, m proto.Media, _ FetchRequest) (image.Image, error) {
	return nil, errors.New("media loading disabled (use --allow-url for URL media; blob media need a hive)")
}

func cmdRender(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("savior display render", flag.ContinueOnError)
	fl.SetOutput(stderr)
	specPath := fl.String("spec", "", "display spec JSON file (- = stdin)")
	size := fl.String("size", "1024x768", "physical screen size WxH")
	rotate := fl.Int("rotate", 0, "rotation: 0, 90, 180 or 270")
	out := fl.String("out", "", "output PNG file")
	allowURL := fl.Bool("allow-url", false, "fetch URL media over the network")
	at := fl.String("time", "", "render at this RFC 3339 time (default now)")
	tz := fl.String("timezone", "", "timezone for clocks (default local)")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	if *specPath == "" || *out == "" || fl.NArg() > 0 {
		fmt.Fprintln(stderr, "usage: savior display render --spec spec.json --size WxH [--rotate D] --out file.png [--allow-url] [--time T] [--timezone Z]")
		return 2
	}
	w, h, err := parseSize(*size)
	if err != nil {
		fmt.Fprintln(stderr, "savior display render:", err)
		return 2
	}
	rot, err := normRotate(*rotate)
	if err != nil {
		fmt.Fprintln(stderr, "savior display render:", err)
		return 2
	}
	now := time.Now()
	if *at != "" {
		if now, err = time.Parse(time.RFC3339, *at); err != nil {
			fmt.Fprintln(stderr, "savior display render: --time:", err)
			return 2
		}
	}
	loc := time.Local
	if *tz != "" {
		if loc, err = time.LoadLocation(*tz); err != nil {
			fmt.Fprintln(stderr, "savior display render: --timezone:", err)
			return 2
		}
	}
	spec, err := readSpec(*specPath, stdin)
	if err != nil {
		fmt.Fprintln(stderr, "savior display render:", err)
		return 1
	}
	dev := NewPNGDevice(*out, w, h, rot)
	info := dev.Info()
	env := Env{
		Now:      func() time.Time { return now },
		Status:   localStatus,
		Fetch:    noFetch,
		Location: loc,
		device:   func() proto.DisplayState { return info },
	}
	if *allowURL {
		env.Fetch = NewURLFetcher().Fetch
	}
	lw, lh := info.Width, info.Height
	frame := image.NewRGBA(image.Rect(0, 0, lw, lh))
	next, rerr := Render(context.Background(), spec, frame, env)
	var me *MediaError
	if rerr != nil && !errors.As(rerr, &me) {
		fmt.Fprintln(stderr, "savior display render:", rerr)
		return 1
	}
	if rerr != nil {
		fmt.Fprintln(stderr, "warning:", rerr)
	}
	if err := dev.Show(frame, nil); err != nil {
		fmt.Fprintln(stderr, "savior display render:", err)
		return 1
	}
	fmt.Fprintf(stdout, "wrote %s (%dx%d, logical %dx%d)", *out, w, h, lw, lh)
	if !next.IsZero() {
		fmt.Fprintf(stdout, ", next change %s", next.Format(time.RFC3339))
	}
	fmt.Fprintln(stdout)
	return 0
}

// signalContext is canceled by SIGINT/SIGTERM or after d (0 = never).
func signalContext(d time.Duration) (context.Context, context.CancelFunc) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	if d <= 0 {
		return ctx, stop
	}
	tctx, cancel := context.WithTimeout(ctx, d)
	return tctx, func() { cancel(); stop() }
}

func cmdShow(args []string, stdin io.Reader, stderr io.Writer) int {
	fl := flag.NewFlagSet("savior display show", flag.ContinueOnError)
	fl.SetOutput(stderr)
	specPath := fl.String("spec", "", "display spec JSON file (- = stdin)")
	device := fl.String("device", "auto", "framebuffer: auto or /dev/fbN")
	seconds := fl.Int("seconds", 0, "stop after N seconds (0 = until interrupted)")
	rotate := fl.Int("rotate", 0, "rotation: 0, 90, 180 or 270")
	vt := fl.String("vt", DefaultVT, "display VT (\"\" = none)")
	allowURL := fl.Bool("allow-url", true, "fetch URL media over the network")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	if *specPath == "" || fl.NArg() > 0 {
		fmt.Fprintln(stderr, "usage: savior display show --spec spec.json [--device auto|/dev/fbN] [--seconds N] [--rotate D] [--vt PATH]")
		return 2
	}
	spec, err := readSpec(*specPath, stdin)
	if err != nil {
		fmt.Fprintln(stderr, "savior display show:", err)
		return 1
	}
	if _, err := normRotate(*rotate); err != nil {
		fmt.Fprintln(stderr, "savior display show:", err)
		return 2
	}
	log := slog.New(slog.NewTextHandler(stderr, nil))
	env := Env{Status: localStatus, Fetch: noFetch, Location: time.Local}
	if *allowURL {
		env.Fetch = NewURLFetcher().Fetch
	}
	dev := *device
	c := NewController(func() (Device, error) { return OpenDevice(dev, 0) }, env, log)
	c.SetVT(*vt)
	c.SetRotate(*rotate)
	c.Apply(spec)
	ctx, cancel := signalContext(time.Duration(*seconds) * time.Second)
	defer cancel()
	// Report problems once the controller had a chance to open the screen.
	go func() {
		select {
		case <-ctx.Done():
		case <-time.After(3 * time.Second):
			st := c.State()
			if st.Error != "" {
				log.Warn("display problem", "error", st.Error)
			}
			if len(st.MediaErrors) > 0 {
				log.Warn("media problems", "errors", strings.Join(st.MediaErrors, "; "))
			}
		}
	}()
	if err := c.Run(ctx); err != nil {
		fmt.Fprintln(stderr, "savior display show:", err)
		return 1
	}
	if st := c.State(); st.Error != "" && !st.Active {
		fmt.Fprintln(stderr, "savior display show:", st.Error)
		return 1
	}
	return 0
}

func cmdTest(args []string, stderr io.Writer) int {
	fl := flag.NewFlagSet("savior display test", flag.ContinueOnError)
	fl.SetOutput(stderr)
	device := fl.String("device", "auto", "framebuffer: auto or /dev/fbN")
	seconds := fl.Int("seconds", 0, "stop after N seconds (0 = until interrupted)")
	step := fl.Duration("step", 3*time.Second, "time per pattern")
	rotate := fl.Int("rotate", 0, "rotation: 0, 90, 180 or 270")
	vtPath := fl.String("vt", DefaultVT, "display VT (\"\" = none)")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	if fl.NArg() > 0 || *step <= 0 {
		fmt.Fprintln(stderr, "usage: savior display test [--device auto|/dev/fbN] [--seconds N] [--step D] [--rotate D] [--vt PATH]")
		return 2
	}
	dev, err := OpenDevice(*device, *rotate)
	if err != nil {
		fmt.Fprintln(stderr, "savior display test:", err)
		return 1
	}
	defer dev.Close()
	ctx, cancel := signalContext(time.Duration(*seconds) * time.Second)
	defer cancel()
	return runTestCycle(ctx, dev, *vtPath, *step, stderr)
}

// runTestCycle shows the test pattern and solid colors in turn until ctx
// ends, pausing while another VT is in front.
func runTestCycle(ctx context.Context, dev Device, vtPath string, step time.Duration, stderr io.Writer) int {
	var mu sync.Mutex
	fg := true
	repaint := make(chan struct{}, 1)
	if u, ok := dev.(vtUser); ok && u.usesVT() && vtPath != "" {
		vt, err := openVT(vtPath,
			func() { mu.Lock(); fg = false; mu.Unlock() },
			func() {
				mu.Lock()
				fg = true
				if inv, ok := dev.(Invalidator); ok {
					inv.Invalidate()
				}
				mu.Unlock()
				select {
				case repaint <- struct{}{}:
				default:
				}
			})
		if err != nil {
			fmt.Fprintln(stderr, "warning: no display VT:", err)
		} else {
			defer vt.Close()
		}
	}
	info := dev.Info()
	fmt.Fprintf(stderr, "display %s: %s %dx%d %s, logical %dx%d; press Ctrl-C to stop\n",
		info.Device, info.Driver, info.FBWidth, info.FBHeight, info.Format, info.Width, info.Height)
	frame := image.NewRGBA(image.Rect(0, 0, info.Width, info.Height))
	desc := describeDevice(info)
	for i := 0; ; i++ {
		n := i % (len(solidTestFrames) + 1)
		if n == 0 {
			drawTestPattern(frame, desc)
		} else {
			fill(frame, frame.Rect, solidTestFrames[n-1])
			strokeRect(frame, frame.Rect, 1, colWhite)
		}
		for {
			mu.Lock()
			var err error
			if fg {
				err = dev.Show(frame, nil)
			}
			mu.Unlock()
			if err != nil {
				fmt.Fprintln(stderr, "savior display test:", err)
				return 1
			}
			select {
			case <-ctx.Done():
				return 0
			case <-repaint:
				continue
			case <-time.After(step):
			}
			break
		}
	}
}

func cmdVTReset(args []string, stderr io.Writer) int {
	fl := flag.NewFlagSet("savior display vt-reset", flag.ContinueOnError)
	fl.SetOutput(stderr)
	vt := fl.String("vt", DefaultVT, "VT to reset (/dev/tty0 = the current one)")
	device := fl.String("device", "auto", "framebuffer to unblank: auto, /dev/fbN or none")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	code := 0
	if err := resetVT(*vt); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintln(stderr, "savior display vt-reset: no", *vt)
		} else {
			fmt.Fprintln(stderr, "savior display vt-reset:", err)
			code = 1
		}
	}
	if d := *device; d != "none" && d != "" {
		if d == "auto" {
			d, _ = AutoDevicePath("/")
		}
		if d != "" {
			_ = unblankFB(d)
		}
	}
	if err := restoreSavedBacklight(); err != nil {
		fmt.Fprintln(stderr, "savior display vt-reset: backlight:", err)
	}
	return code
}

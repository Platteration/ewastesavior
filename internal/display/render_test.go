package display

import (
	"context"
	"errors"
	"image"
	"image/color"
	"strings"
	"testing"
	"time"

	"github.com/platteration/ewastesavior/internal/imaging"
	"github.com/platteration/ewastesavior/internal/proto"
)

var testNow = time.Date(2026, 9, 23, 14, 7, 31, 500e6, time.UTC)

func fixedEnv() Env {
	return Env{Now: func() time.Time { return testNow }}
}

func render(t *testing.T, spec proto.DisplaySpec, w, h int, env Env) (*image.RGBA, time.Time, error) {
	t.Helper()
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	next, err := Render(context.Background(), spec, dst, env)
	return dst, next, err
}

// countColor counts pixels of exactly c in r.
func countColor(img *image.RGBA, r image.Rectangle, c color.RGBA) int {
	n := 0
	r = r.Intersect(img.Rect)
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			if img.RGBAAt(x, y) == c {
				n++
			}
		}
	}
	return n
}

func TestRenderColorAndOff(t *testing.T) {
	img, next, err := render(t, proto.DisplaySpec{Mode: "color", BG: "#12ab34"}, 64, 48, fixedEnv())
	if err != nil || !next.IsZero() {
		t.Fatalf("err %v next %v", err, next)
	}
	if n := countColor(img, img.Rect, rgb(0x12, 0xab, 0x34)); n != 64*48 {
		t.Fatalf("%d pixels have the background color, want all", n)
	}
	img, _, _ = render(t, proto.DisplaySpec{Mode: "off"}, 20, 20, fixedEnv())
	if n := countColor(img, img.Rect, colBlack); n != 400 {
		t.Fatalf("off: %d black pixels", n)
	}
}

func TestRenderText(t *testing.T) {
	spec := proto.DisplaySpec{Mode: "text", Title: "Lab", Text: "Hello old computer", FG: "#ffff00", BG: "#000080"}
	img, _, err := render(t, spec, 640, 480, fixedEnv())
	if err != nil {
		t.Fatal(err)
	}
	fg, bg := rgb(0xff, 0xff, 0), rgb(0, 0, 0x80)
	if img.RGBAAt(0, 0) != bg || img.RGBAAt(639, 479) != bg {
		t.Fatal("corners are not the background")
	}
	center := image.Rect(160, 180, 480, 330)
	if n := countColor(img, center, fg); n < 500 {
		t.Fatalf("only %d foreground pixels near the center", n)
	}
	if n := countColor(img, image.Rect(0, 0, 640, 480*16/100), fg); n < 100 {
		t.Fatalf("title band has %d foreground pixels", n)
	}
	// Text fills a good part of the screen: ink spans > 50% of the width.
	ink := inkBounds(img, bg)
	if ink.Dx() < 640/2 || ink.Dx() > 640*95/100 {
		t.Fatalf("text ink %v does not fill ~80%% of the width", ink)
	}
	// Empty text renders just the background.
	img, _, _ = render(t, proto.DisplaySpec{Mode: "text"}, 50, 50, fixedEnv())
	if countColor(img, img.Rect, colBlack) != 2500 {
		t.Fatal("empty text is not plain black")
	}
}

func TestRenderClock(t *testing.T) {
	env := fixedEnv()
	img, next, err := render(t, proto.DisplaySpec{Mode: "clock"}, 320, 240, env)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 9, 23, 14, 8, 0, 0, time.UTC); !next.Equal(want) {
		t.Fatalf("next = %v, want the next minute %v", next, want)
	}
	if n := countColor(img, image.Rect(0, 60, 320, 180), colWhite); n < 300 {
		t.Fatalf("clock drew only %d white pixels", n)
	}
	_, next, _ = render(t, proto.DisplaySpec{Mode: "clock", ClockFormat: "15:04:05"}, 320, 240, env)
	if want := time.Date(2026, 9, 23, 14, 7, 32, 0, time.UTC); !next.Equal(want) {
		t.Fatalf("seconds format: next = %v, want %v", next, want)
	}
	// The time text depends on the zone; the key includes it.
	a := renderFrame(context.Background(), proto.DisplaySpec{Mode: "clock", Timezone: "Asia/Kathmandu"}, image.NewRGBA(image.Rect(0, 0, 100, 50)), env, "")
	b := renderFrame(context.Background(), proto.DisplaySpec{Mode: "clock"}, image.NewRGBA(image.Rect(0, 0, 100, 50)), env, "")
	if a.key == b.key || !strings.Contains(a.key, "19:52") {
		t.Fatalf("zone not applied: %q vs %q", a.key, b.key)
	}
	env.Location = time.FixedZone("X", 3600)
	c := renderFrame(context.Background(), proto.DisplaySpec{Mode: "clock"}, image.NewRGBA(image.Rect(0, 0, 100, 50)), env, "")
	if !strings.Contains(c.key, "15:07") {
		t.Fatalf("env.Location not used: %q", c.key)
	}
}

func TestShowsSeconds(t *testing.T) {
	for layout, want := range map[string]bool{"15:04": false, "3:04 PM": false, "15:04:05": true,
		"05": true, "Mon 15h": false, "15:04:05.000": true, ".000": true, "2006-01-02": false} {
		if got := showsSeconds(layout); got != want {
			t.Errorf("showsSeconds(%q) = %v", layout, got)
		}
	}
}

func TestSteadyStateSkipsRedraw(t *testing.T) {
	dst := image.NewRGBA(image.Rect(0, 0, 320, 240))
	st := StatusInfo{Name: "n1", Link: proto.LinkConnected, Metrics: proto.Metrics{CPUPercent: 10.2}}
	env := Env{Now: func() time.Time { return testNow }, Status: func() StatusInfo { return st }}
	for _, spec := range []proto.DisplaySpec{{Mode: "status"}, {Mode: "clock"}, {Mode: "text", Text: "x"}, {Mode: "test"}} {
		first := renderFrame(context.Background(), spec, dst, env, "")
		if !first.drawn || first.key == "" {
			t.Fatalf("%s: first render not drawn", spec.Mode)
		}
		again := renderFrame(context.Background(), spec, dst, env, first.key)
		if again.drawn {
			t.Errorf("%s: unchanged content was redrawn", spec.Mode)
		}
	}
	first := renderFrame(context.Background(), proto.DisplaySpec{Mode: "status"}, dst, env, "")
	st.Metrics.CPUPercent = 10.4 // rounds to the same displayed value
	if renderFrame(context.Background(), proto.DisplaySpec{Mode: "status"}, dst, env, first.key).drawn {
		t.Error("status redrawn for an invisible change")
	}
	st.Metrics.CPUPercent = 55
	if !renderFrame(context.Background(), proto.DisplaySpec{Mode: "status"}, dst, env, first.key).drawn {
		t.Error("status not redrawn after CPU changed")
	}
}

func TestStatusView(t *testing.T) {
	in := StatusInfo{
		Name: "lab\x1b[31m-pc", NodeID: "n0123456789ab", Version: "v1",
		Link: proto.LinkUnreachable, HiveAddr: "10.1.1.1:7700", HiveError: "connection refused",
		Addrs: []string{"10.1.1.5/24"}, Roles: []proto.Role{"compute", "display"},
		Metrics:   proto.Metrics{CPUPercent: 150, MemAvailableMB: 256, CPUTempC: 80, CPUTempLimitC: 85, BatteryPercent: 15, OnBattery: true},
		Inventory: proto.Inventory{MemTotalMB: 1024, HasBattery: true},
		Hive:      &HivePanel{URLs: []string{"https://10.1.1.1:7700"}, Fingerprint: "sha256:" + strings.Repeat("ab", 32), PairCode: "ABCDEFGH", NodesOnline: 4},
	}
	v := buildStatusView(in)
	if v.name != "lab[31m-pc" {
		t.Errorf("name not sanitized: %q", v.name)
	}
	if v.code != proto.ShortCode("n0123456789ab") {
		t.Errorf("code %q", v.code)
	}
	if !strings.Contains(v.hint, "port 7700 is blocked") || v.errLine != "connection refused" {
		t.Errorf("hint %q err %q", v.hint, v.errLine)
	}
	bars := map[string]barView{}
	for _, b := range v.bars {
		bars[b.label] = b
	}
	if bars["CPU"].value != "100%" || bars["RAM"].value != "768 / 1024 MB" || bars["Temp"].col != colBad || bars["Battery"].col != colBad {
		t.Errorf("bars %+v", v.bars)
	}
	if v.hive == nil || v.hive.warning == "" || v.hive.pair != "ABCDEFGH" {
		t.Errorf("hive panel %+v", v.hive)
	}
	lines := fingerprintLines(in.Hive.Fingerprint, 12, 1000)
	if len(lines) != 3 || lines[1] != strings.Repeat("abab ", 7)+"abab" {
		t.Errorf("fingerprint lines %q", lines)
	}
	narrow := fingerprintLines(in.Hive.Fingerprint, 12, 150)
	if len(narrow) != 5 {
		t.Errorf("narrow fingerprint %q", narrow)
	}
	tiny := fingerprintLines(in.Hive.Fingerprint, 12, 60)
	if len(tiny) != 1 || !strings.Contains(tiny[0], "…") && !strings.HasSuffix(tiny[0], "…") {
		t.Errorf("tiny fingerprint %q", tiny)
	}
	// Every size renders without panicking, hive panel included.
	for _, sz := range [][2]int{{640, 480}, {1920, 1080}, {480, 800}, {160, 120}, {1, 1}} {
		env := Env{Status: func() StatusInfo { return in }}
		if _, _, err := render(t, proto.DisplaySpec{Mode: "status"}, sz[0], sz[1], env); err != nil {
			t.Errorf("%v: %v", sz, err)
		}
	}
}

// solid returns a w×h image of one color.
func solid(w, h int, c color.RGBA) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	fill(img, img.Rect, c)
	return img
}

func TestRenderImage(t *testing.T) {
	red := rgb(255, 0, 0)
	var got FetchRequest
	env := fixedEnv()
	env.Fetch = func(_ context.Context, m proto.Media, req FetchRequest) (image.Image, error) {
		got = req
		// A 2:1 red image fitted (contain) onto the screen: letterbox is
		// transparent so the spec background shows.
		return imaging.RenderRegion(solid(200, 100, red), req.CanvasW, req.CanvasH, req.Fit, req.Rect, req.PixelW, req.PixelH, color.RGBA{}), nil
	}
	spec := proto.DisplaySpec{Mode: "image", Image: &proto.Media{URL: "http://x/a.png"}, BG: "#0000ff"}
	img, next, err := render(t, spec, 100, 100, env)
	if err != nil || !next.IsZero() {
		t.Fatalf("err %v next %v", err, next)
	}
	if got.PixelW != 100 || got.CanvasW != 100 || got.Rect != image.Rect(0, 0, 100, 100) || got.Fit != "contain" {
		t.Fatalf("request %+v", got)
	}
	if img.RGBAAt(50, 50) != red || img.RGBAAt(50, 5) != rgb(0, 0, 255) {
		t.Fatalf("center %v top %v", img.RGBAAt(50, 50), img.RGBAAt(50, 5))
	}
	// Errors become a placeholder plus a MediaError.
	env.Fetch = func(context.Context, proto.Media, FetchRequest) (image.Image, error) {
		return nil, errors.New("HTTP 404 Not Found")
	}
	img, _, err = render(t, spec, 200, 150, env)
	var me *MediaError
	if !errors.As(err, &me) || !strings.Contains(err.Error(), "404") {
		t.Fatalf("err = %v", err)
	}
	if inkBounds(img, rgb(0, 0, 255)).Empty() {
		t.Fatal("no placeholder text drawn")
	}
	// Pending loads are not errors.
	env.Fetch = func(context.Context, proto.Media, FetchRequest) (image.Image, error) { return nil, errPending }
	res := renderFrame(context.Background(), spec, image.NewRGBA(image.Rect(0, 0, 50, 50)), env, "")
	if !res.pending || len(res.mediaErrs) != 0 || res.err != nil {
		t.Fatalf("pending result %+v", res)
	}
	// No fetcher at all.
	env.Fetch = nil
	if _, _, err := render(t, spec, 50, 50, env); err == nil {
		t.Fatal("expected an error without a fetcher")
	}
	// A fetcher returning the wrong size is corrected, not trusted.
	env.Fetch = func(context.Context, proto.Media, FetchRequest) (image.Image, error) { return solid(7, 3, red), nil }
	img, _, err = render(t, spec, 60, 40, env)
	if err != nil || img.RGBAAt(59, 39) != red {
		t.Fatalf("wrong-size image: %v %v", err, img.RGBAAt(59, 39))
	}
}

func TestSlideIndex(t *testing.T) {
	tests := []struct {
		t        time.Time
		interval int
		n        int
		idx      int
		next     int64
	}{
		{time.Unix(0, 0), 10, 3, 0, 10},
		{time.Unix(9, 999e6), 10, 3, 0, 10},
		{time.Unix(10, 0), 10, 3, 1, 20},
		{time.Unix(35, 0), 10, 3, 0, 40},    // floor(3.5)=3, 3 mod 3 = 0
		{time.Unix(1000, 0), 0, 4, 0, 1010}, // default interval 10: 100 mod 4
		{time.Unix(-5, 0), 10, 3, 2, 0},     // floor(-0.5) = -1 -> 2
		{time.Unix(7, 0), 3, 5, 2, 9},
	}
	for _, tt := range tests {
		idx, next := slideIndex(tt.t, tt.interval, tt.n)
		if idx != tt.idx || next.Unix() != tt.next {
			t.Errorf("slideIndex(%v, %d, %d) = %d, %d; want %d, %d", tt.t.Unix(), tt.interval, tt.n, idx, next.Unix(), tt.idx, tt.next)
		}
	}
	if idx, next := slideIndex(time.Now(), 10, 0); idx != 0 || !next.IsZero() {
		t.Error("empty slideshow")
	}
}

func TestRenderSlideshowFollowsClock(t *testing.T) {
	colors := []color.RGBA{rgb(255, 0, 0), rgb(0, 255, 0), rgb(0, 0, 255)}
	spec := proto.DisplaySpec{Mode: "slideshow", IntervalS: 5, Images: []proto.Media{
		{URL: "http://x/0"}, {URL: "http://x/1"}, {URL: "http://x/2"},
	}}
	fetch := func(_ context.Context, m proto.Media, req FetchRequest) (image.Image, error) {
		i := int(m.URL[len(m.URL)-1] - '0')
		return solid(req.PixelW, req.PixelH, colors[i]), nil
	}
	now := time.Unix(1_000_000_003, 0) // floor(/5) = 200000000 -> index 2
	env := Env{Now: func() time.Time { return now }, Fetch: fetch}
	img, next, err := render(t, spec, 20, 20, env)
	if err != nil {
		t.Fatal(err)
	}
	if img.RGBAAt(10, 10) != colors[2] || next.Unix() != 1_000_000_005 {
		t.Fatalf("color %v next %v", img.RGBAAt(10, 10), next.Unix())
	}
	now = next
	img, _, _ = render(t, spec, 20, 20, env)
	if img.RGBAAt(10, 10) != colors[0] {
		t.Fatalf("after the boundary: %v", img.RGBAAt(10, 10))
	}
}

func TestRenderDashboard(t *testing.T) {
	stats := &proto.SwarmStats{NodesOnline: 7, CoresAllocatable: 10, CoresInUse: 5, TasksByState: map[string]int{"running": 3}}
	env := fixedEnv()
	env.Stats = func(context.Context) (*proto.SwarmStats, error) { return stats, nil }
	img, next, err := render(t, proto.DisplaySpec{Mode: "dashboard"}, 640, 480, env)
	if err != nil || !next.Equal(testNow.Add(5*time.Second)) {
		t.Fatalf("err %v next %v", err, next)
	}
	if countColor(img, img.Rect, colPanel) < 10000 {
		t.Fatal("no tiles drawn")
	}
	env.Stats = func(context.Context) (*proto.SwarmStats, error) { return nil, errors.New("hive down") }
	img, _, err = render(t, proto.DisplaySpec{Mode: "dashboard"}, 640, 480, env)
	if err != nil {
		t.Fatal(err)
	}
	if countColor(img, img.Rect, colWarn) == 0 {
		t.Fatal("error line not drawn")
	}
	env.Stats = nil
	if _, _, err := render(t, proto.DisplaySpec{Mode: "dashboard"}, 64, 48, env); err != nil {
		t.Fatal(err)
	}
}

// wallFetch renders a 200x100 image (left half red, right half blue) the
// way the hive render endpoint does.
func wallFetch(_ context.Context, m proto.Media, req FetchRequest) (image.Image, error) {
	src := image.NewRGBA(image.Rect(0, 0, 200, 100))
	fill(src, image.Rect(0, 0, 100, 100), rgb(255, 0, 0))
	fill(src, image.Rect(100, 0, 200, 100), rgb(0, 0, 255))
	return imaging.RenderRegion(src, req.CanvasW, req.CanvasH, req.Fit, req.Rect, req.PixelW, req.PixelH, color.RGBA{}), nil
}

func wallSpec(col int, content proto.DisplaySpec) proto.DisplaySpec {
	// Two 400x300 mm screens side by side with a 20 mm bezel gap.
	x := 0
	if col == 1 {
		x = 420
	}
	return proto.DisplaySpec{Mode: "wall", Wall: &proto.WallTile{WallID: "w1", CanvasW: 820, CanvasH: 300,
		X: x, Y: 0, W: 400, H: 300, Row: 0, Col: col, Label: "R1C" + string(rune('1'+col)), Content: &content}}
}

func TestRenderWallImageTiles(t *testing.T) {
	env := fixedEnv()
	env.Fetch = wallFetch
	content := proto.DisplaySpec{Mode: "image", Image: &proto.Media{Blob: strings.Repeat("a", 64)}, Fit: "stretch"}
	left, _, err := render(t, wallSpec(0, content), 320, 240, env)
	if err != nil {
		t.Fatal(err)
	}
	right, _, err := render(t, wallSpec(1, content), 320, 240, env)
	if err != nil {
		t.Fatal(err)
	}
	red, blue := rgb(255, 0, 0), rgb(0, 0, 255)
	if n := countColor(left, left.Rect, red); n < 320*240*95/100 {
		t.Errorf("left tile: %d red pixels", n)
	}
	if n := countColor(right, right.Rect, blue); n < 320*240*95/100 {
		t.Errorf("right tile: %d blue pixels", n)
	}
	var req FetchRequest
	env.Fetch = func(ctx context.Context, m proto.Media, r FetchRequest) (image.Image, error) {
		req = r
		return wallFetch(ctx, m, r)
	}
	if _, _, err := render(t, wallSpec(1, content), 320, 240, env); err != nil {
		t.Fatal(err)
	}
	if req.CanvasW != 820 || req.Rect != image.Rect(420, 0, 820, 300) || req.PixelW != 320 || req.PixelH != 240 {
		t.Fatalf("wall fetch request %+v", req)
	}
}

func TestRenderWallTestPatternAndText(t *testing.T) {
	img, _, err := render(t, wallSpec(1, proto.DisplaySpec{Mode: "test"}), 640, 480, fixedEnv())
	if err != nil {
		t.Fatal(err)
	}
	// The label is drawn big in the middle of the tile.
	if n := countColor(img, image.Rect(160, 170, 480, 290), colWhite); n < 500 {
		t.Fatalf("label: %d white pixels in the center", n)
	}
	// The canvas diagonal from (0,0) to (820,300) passes through this
	// tile: at canvas x=620 it is at y≈227 mm → pixel (320, 363).
	diag := rgb(0xff, 0xd4, 0x00)
	if countColor(img, image.Rect(310, 350, 330, 376), diag) == 0 {
		t.Fatal("canvas diagonal missing")
	}
	// Text content spans the canvas: both tiles get ink.
	text := proto.DisplaySpec{Mode: "text", Text: "WIDEWIDEWIDE"}
	for col := 0; col < 2; col++ {
		img, _, err := render(t, wallSpec(col, text), 320, 240, fixedEnv())
		if err != nil {
			t.Fatal(err)
		}
		if countColor(img, img.Rect, colWhite) < 1000 {
			t.Errorf("tile %d has no text", col)
		}
	}
	color := proto.DisplaySpec{Mode: "color", BG: "#00ff00"}
	img, _, _ = render(t, wallSpec(0, color), 10, 10, fixedEnv())
	if countColor(img, img.Rect, rgb(0, 255, 0)) != 100 {
		t.Fatal("wall color")
	}
	if _, _, err := render(t, proto.DisplaySpec{Mode: "wall"}, 10, 10, fixedEnv()); err == nil {
		t.Fatal("wall without tile should fail")
	}
}

func TestRenderTestPattern(t *testing.T) {
	env := fixedEnv()
	env.device = func() proto.DisplayState { return proto.DisplayState{Format: "RGB565", Driver: "vesafb"} }
	img, _, err := render(t, proto.DisplaySpec{Mode: "test"}, 800, 600, env)
	if err != nil {
		t.Fatal(err)
	}
	if img.RGBAAt(0, 0) != colWhite || img.RGBAAt(799, 300) != colWhite || img.RGBAAt(400, 599) != colWhite {
		t.Fatal("1-pixel border missing")
	}
	if img.RGBAAt(150, 100) != rgb(255, 255, 0) || img.RGBAAt(750, 100) != colBlack && img.RGBAAt(750, 100) != rgb(0, 0, 0) {
		t.Fatalf("bars: %v %v", img.RGBAAt(150, 100), img.RGBAAt(750, 100))
	}
	// Gray ramp: darker on the left than on the right.
	y := 600*55/100 + 5
	if l, r := img.RGBAAt(20, y), img.RGBAAt(780, y); l.R >= r.R {
		t.Fatalf("gray ramp %v .. %v", l, r)
	}
}

func TestRenderRecoversAndRejects(t *testing.T) {
	env := Env{Status: func() StatusInfo { panic("status exploded") }}
	_, _, err := render(t, proto.DisplaySpec{Mode: "status"}, 100, 100, env)
	if err == nil || !strings.Contains(err.Error(), "status exploded") {
		t.Fatalf("panic not recovered as error: %v", err)
	}
	if _, _, err := render(t, proto.DisplaySpec{Mode: "hologram"}, 10, 10, fixedEnv()); err == nil {
		t.Fatal("unknown mode accepted")
	}
	if _, err := Render(context.Background(), proto.DisplaySpec{Mode: "color"}, nil, fixedEnv()); err == nil {
		t.Fatal("nil destination accepted")
	}
	// A sub-image destination is drawn at its own origin.
	big := image.NewRGBA(image.Rect(0, 0, 40, 40))
	sub := big.SubImage(image.Rect(10, 10, 30, 30)).(*image.RGBA)
	if _, err := Render(context.Background(), proto.DisplaySpec{Mode: "color", BG: "#ff0000"}, sub, fixedEnv()); err != nil {
		t.Fatal(err)
	}
	if big.RGBAAt(10, 10) != rgb(255, 0, 0) || big.RGBAAt(29, 29) != rgb(255, 0, 0) || big.RGBAAt(9, 9) == rgb(255, 0, 0) || big.RGBAAt(30, 30) == rgb(255, 0, 0) {
		t.Fatal("sub-image rendering touched the wrong pixels")
	}
}

func TestIdentifyOverlay(t *testing.T) {
	img := solid(400, 300, rgb(0, 128, 0))
	drawIdentify(img, "7KQ", "lab-pc", "R1C2", false)
	if img.RGBAAt(2, 2) != rgb(0, 128, 0) {
		t.Fatal("overlay covered the scene edge")
	}
	if countColor(img, img.Rect, identYellow) < 400*300/3 || countColor(img, image.Rect(100, 50, 300, 150), identDark) < 500 {
		t.Fatal("overlay panel or code missing")
	}
	img2 := solid(400, 300, rgb(0, 128, 0))
	drawIdentify(img2, "7KQ", "lab-pc", "R1C2", true)
	if countColor(img2, img2.Rect, identDark) < 400*300/3 {
		t.Fatal("inverted phase not inverted")
	}
	drawErrorScreen(img, "Display error", strings.Repeat("very long error ", 50))
}

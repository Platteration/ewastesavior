package proto

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidRelPath(t *testing.T) {
	good := []string{"a", "out.txt", "dir/file.bin", "frames/0001.png", "ümlaut.txt", "a b.txt"}
	bad := []string{"", "/abs", "..", "../x", "a/../b", "a//b", ".", "a/.", "./a", "a\\b", "C:x", "a:b",
		"-rf", "dir/-x", "out.", "out ", "a\nb", "\x1b]52;x", ".savior-script", ".savior/x",
		strings.Repeat("a", 256), "bad\xff"}
	for _, p := range good {
		if !ValidRelPath(p) {
			t.Errorf("ValidRelPath(%q) = false, want true", p)
		}
	}
	for _, p := range bad {
		if ValidRelPath(p) {
			t.Errorf("ValidRelPath(%q) = true, want false", p)
		}
	}
}

func TestValidOutputPattern(t *testing.T) {
	for _, p := range []string{"*.png", "out/*.txt", "frame-[0-9]*.png", "result"} {
		if !ValidOutputPattern(p) {
			t.Errorf("%q rejected", p)
		}
	}
	for _, p := range []string{"../../etc/*", "/etc/*", "[", "a/[", "*/../*"} {
		if ValidOutputPattern(p) {
			t.Errorf("%q accepted", p)
		}
	}
}

func TestNodeNamesAndIDs(t *testing.T) {
	for _, n := range []string{"savior-a1b2c3", "lab-3", "x", "rack1-shelf2"} {
		if !ValidNodeName(n) {
			t.Errorf("name %q rejected", n)
		}
	}
	for _, n := range []string{"", "Lab-3", "-x", "x-", "a b", "a/b", "n0123456789ab", strings.Repeat("a", 33)} {
		if ValidNodeName(n) {
			t.Errorf("name %q accepted", n)
		}
	}
	if !ValidNodeID("n0123456789ab") || ValidNodeID("N01") || ValidNodeID("a/b") || ValidNodeID("") {
		t.Error("ValidNodeID")
	}
}

func TestSanitize(t *testing.T) {
	if got := Sanitize("a\x1b[31mb\nc\td\u0085e", 100, false); got != "a[31mbcde" {
		t.Errorf("Sanitize = %q", got)
	}
	if got := Sanitize("a\nb\tc\x00", 100, true); got != "a\nb\tc" {
		t.Errorf("Sanitize keep = %q", got)
	}
	if got := Sanitize("ééé", 4, false); got != "éé" {
		t.Errorf("Sanitize cap mid-rune = %q", got)
	}
	if got := Sanitize("ok\xffx", 10, false); got != "ok�x" {
		t.Errorf("Sanitize invalid utf8 = %q", got)
	}
}

func TestShortCode(t *testing.T) {
	a, b := ShortCode("n0123456789ab"), ShortCode("n0123456789ac")
	if len(a) != 3 || a == b || a != ShortCode("n0123456789ab") {
		t.Fatalf("short codes %q %q", a, b)
	}
	if strings.ContainsAny(a, "ILOU") {
		t.Fatalf("ambiguous chars in %q", a)
	}
}

func TestResourcesFits(t *testing.T) {
	free := Resources{Cores: 2, MemMB: 512, DiskMB: 1000}
	cases := []struct {
		need Resources
		ram  bool
		want bool
	}{
		{Resources{Cores: 2, MemMB: 512, DiskMB: 1000}, false, true},
		{Resources{Cores: 2.0000000001, MemMB: 1}, false, true}, // epsilon
		{Resources{Cores: 2.1, MemMB: 1}, false, false},
		{Resources{Cores: 1, MemMB: 513}, false, false},
		{Resources{Cores: 1, MemMB: 1, DiskMB: 1001}, false, false},
		{Resources{Cores: 1, MemMB: -5000}, false, false},
		{Resources{Cores: -1, MemMB: 1}, false, false},
		{Resources{Cores: 1, MemMB: 256, DiskMB: 256}, true, true},
		{Resources{Cores: 1, MemMB: 256, DiskMB: 257}, true, false},
	}
	for i, c := range cases {
		if got := free.Fits(c.need, c.ram); got != c.want {
			t.Errorf("case %d: Fits(%+v, %v) = %v", i, c.need, c.ram, got)
		}
	}
	if (Resources{Cores: 1, MemMB: 10}).Fits(Resources{Cores: 1, MemMB: 5, DiskMB: 50}, false) != true {
		t.Error("disk ignored when node reports none")
	}
}

func TestParseColor(t *testing.T) {
	if r, g, b, ok := ParseColor("#ff8000"); !ok || r != 255 || g != 128 || b != 0 {
		t.Error("#ff8000")
	}
	if r, g, b, ok := ParseColor("#FfF"); !ok || r != 255 || g != 255 || b != 255 {
		t.Error("#FfF")
	}
	for _, s := range []string{"", "fff", "#ff", "#gggggg", "#12345"} {
		if _, _, _, ok := ParseColor(s); ok {
			t.Errorf("%q accepted", s)
		}
	}
}

func blob(c byte) string { return strings.Repeat(string(c), 64) }

func TestValidateDisplaySpec(t *testing.T) {
	good := []DisplaySpec{
		{Mode: DisplayStatus},
		{Mode: DisplayText, Text: "hi", FG: "#fff", BG: "#000000"},
		{Mode: DisplayImage, Image: &Media{Blob: blob('a')}, Fit: "cover"},
		{Mode: DisplaySlideshow, Images: []Media{{URL: "https://x/y.png", SHA256: blob('b')}}, IntervalS: 5},
		{Mode: DisplayClock, Timezone: "Europe/Berlin"},
		{Mode: DisplayWall, Wall: &WallTile{CanvasW: 800, CanvasH: 300, X: 400, Y: 0, W: 400, H: 300,
			Content: &DisplaySpec{Mode: DisplayTest}}},
	}
	for i, s := range good {
		if err := ValidateDisplaySpec(&s); err != nil {
			t.Errorf("good %d: %v", i, err)
		}
	}
	bad := []DisplaySpec{
		{Mode: "nope"},
		{Mode: DisplayText, FG: "red"},
		{Mode: DisplayImage},
		{Mode: DisplayImage, Image: &Media{Blob: blob('a'), URL: "http://x"}},
		{Mode: DisplayImage, Image: &Media{URL: "file:///etc/passwd"}},
		{Mode: DisplaySlideshow},
		{Mode: DisplaySlideshow, Images: []Media{{Blob: blob('a')}}, IntervalS: 1},
		{Mode: DisplayText, Text: strings.Repeat("x", MaxTextLen+1)},
		{Mode: DisplayClock, Timezone: "Mars/Olympus"},
		{Mode: DisplayImage, Image: &Media{Blob: blob('a')}, Fit: "zoom"},
		{Mode: DisplayWall},
		{Mode: DisplayWall, Wall: &WallTile{CanvasW: 100, CanvasH: 100, X: 50, W: 60, H: 10, Content: &DisplaySpec{Mode: DisplayColor}}},
		{Mode: DisplayWall, Wall: &WallTile{CanvasW: 100, CanvasH: 100, W: 10, H: 10, Content: &DisplaySpec{Mode: DisplayClock}}},
		{Mode: DisplayStatus, Wall: &WallTile{}},
	}
	for i, s := range bad {
		if err := ValidateDisplaySpec(&s); err == nil {
			t.Errorf("bad %d accepted: %+v", i, s)
		}
	}
	many := DisplaySpec{Mode: DisplaySlideshow}
	for i := 0; i <= MaxImages; i++ {
		many.Images = append(many.Images, Media{Blob: blob('c')})
	}
	if ValidateDisplaySpec(&many) == nil {
		t.Error("too many images accepted")
	}
}

func TestValidateWallSpec(t *testing.T) {
	ok := WallSpec{Rows: 1, Cols: 2, Cells: []WallCell{{Node: "a", Row: 0, Col: 0}, {Node: "b", Row: 0, Col: 1}},
		Content: DisplaySpec{Mode: DisplayColor, BG: "#123456"}}
	if err := ValidateWallSpec(&ok); err != nil {
		t.Fatal(err)
	}
	mut := func(f func(w *WallSpec)) WallSpec {
		w := ok
		w.Cells = append([]WallCell(nil), ok.Cells...)
		f(&w)
		return w
	}
	bads := []WallSpec{
		mut(func(w *WallSpec) { w.Rows = 0 }),
		mut(func(w *WallSpec) { w.Cols = 17 }),
		mut(func(w *WallSpec) { w.Cells[1].Col = 0 }),                              // same position
		mut(func(w *WallSpec) { w.Cells[1].Node = "a" }),                           // same node
		mut(func(w *WallSpec) { w.Cells[1].Col = 2 }),                              // out of grid
		mut(func(w *WallSpec) { w.GapXMM = -1 }),                                   // negative gap
		mut(func(w *WallSpec) { w.Cells = nil }),                                   // empty
		mut(func(w *WallSpec) { w.Content = DisplaySpec{Mode: DisplayDashboard} }), // content mode
		mut(func(w *WallSpec) { w.Cells[0].Rect = &Rect{W: 0, H: 10} }),
	}
	for i, w := range bads {
		if err := ValidateWallSpec(&w); err == nil {
			t.Errorf("bad wall %d accepted", i)
		}
	}
}

func TestJobDefaultsAndValidation(t *testing.T) {
	s := JobSpec{Command: []string{"echo", "{{index}}/{{count}}"}}
	ApplyJobDefaults(&s)
	if s.Kind != KindExec || s.Resources.Cores != 1 || s.Resources.MemMB != DefaultMemMB ||
		s.Resources.DiskMB != DefaultDiskMB || s.TimeoutS != DefaultTimeoutS || *s.Retries != DefaultRetries ||
		s.Count != 1 || s.Requirements.Isolation != IsolationFull {
		t.Fatalf("defaults: %+v", s)
	}
	if err := ValidateJobSpec(&s); err != nil {
		t.Fatal(err)
	}
	// JSON round trip keeps Retries explicit zero.
	zero := 0
	s.Retries = &zero
	b, _ := json.Marshal(s)
	var back JobSpec
	json.Unmarshal(b, &back)
	if back.Retries == nil || *back.Retries != 0 {
		t.Fatal("retries=0 lost in JSON")
	}

	base := func() JobSpec {
		j := JobSpec{Script: "echo hi"}
		ApplyJobDefaults(&j)
		return j
	}
	bad := map[string]func(*JobSpec){
		"both":         func(j *JobSpec) { j.Command = []string{"x"} },
		"empty script": func(j *JobSpec) { j.Script = "  " },
		"neg mem":      func(j *JobSpec) { j.Resources.MemMB = -1 },
		"zero cores":   func(j *JobSpec) { j.Resources.Cores = 0 },
		"huge count":   func(j *JobSpec) { j.Count = MaxTaskCount + 1 },
		"input path":   func(j *JobSpec) { j.Inputs = []Input{{Name: "../x", Blob: blob('a')}} },
		"input dup":    func(j *JobSpec) { j.Inputs = []Input{{Name: "x", Blob: blob('a')}, {Name: "x", Blob: blob('b')}} },
		"url no sha":   func(j *JobSpec) { j.Inputs = []Input{{Name: "x", URL: "https://a/b", Size: 10}} },
		"url no size":  func(j *JobSpec) { j.Inputs = []Input{{Name: "x", URL: "https://a/b", SHA256: blob('a')}} },
		"url too big": func(j *JobSpec) {
			j.Inputs = []Input{{Name: "x", URL: "https://a/b", SHA256: blob('a'), Size: 1 << 40}}
		},
		"ftp":          func(j *JobSpec) { j.Inputs = []Input{{Name: "x", URL: "ftp://a/b", SHA256: blob('a'), Size: 1}} },
		"output":       func(j *JobSpec) { j.Outputs = []string{"../../etc/*"} },
		"env reserved": func(j *JobSpec) { j.Env = map[string]string{"SAVIOR_TASK_ID": "x"} },
		"env key":      func(j *JobSpec) { j.Env = map[string]string{"1BAD": "x"} },
		"isolation":    func(j *JobSpec) { j.Requirements.Isolation = "some" },
		"retries":      func(j *JobSpec) { r := MaxRetries + 1; j.Retries = &r },
		"priority":     func(j *JobSpec) { j.Priority = 5000 },
		"arch":         func(j *JobSpec) { j.Requirements.Arch = []string{"x86 64"} },
	}
	for name, f := range bad {
		j := base()
		f(&j)
		if err := ValidateJobSpec(&j); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestExpandTemplate(t *testing.T) {
	if got := ExpandTemplate("f{{index}}of{{count}}", 3, 10); got != "f3of10" {
		t.Fatal(got)
	}
	if got := ExpandTemplate("plain", 1, 2); got != "plain" {
		t.Fatal(got)
	}
}

func TestCountsAsAttempt(t *testing.T) {
	for k, want := range map[string]bool{ErrExit: true, ErrTimeout: true, ErrInput: false, ErrSandbox: false, ErrOutput: false, ErrInternal: false, "": false} {
		if CountsAsAttempt(k) != want {
			t.Errorf("CountsAsAttempt(%q)", k)
		}
	}
}

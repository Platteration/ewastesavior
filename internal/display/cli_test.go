package display

import (
	"bytes"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runCLITest(stdin string, args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := runCLI(args, strings.NewReader(stdin), &out, &errb)
	return code, out.String(), errb.String()
}

func TestCLIRender(t *testing.T) {
	dir := t.TempDir()
	spec := filepath.Join(dir, "spec.json")
	os.WriteFile(spec, []byte(`{"mode":"color","bg":"#ff8000"}`), 0o644)
	out := filepath.Join(dir, "out.png")
	code, stdout, stderr := runCLITest("", "render", "--spec", spec, "--size", "80x60", "--rotate", "90", "--out", out)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "logical 60x80") {
		t.Fatalf("stdout %q", stdout)
	}
	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != 80 || b.Dy() != 60 {
		t.Fatalf("png %v, want the physical size", b)
	}
	if r, g, b, _ := img.At(40, 30).RGBA(); r>>8 != 0xff || g>>8 != 0x80 || b != 0 {
		t.Fatalf("color %v", img.At(40, 30))
	}

	// Spec from stdin, a clock at a fixed time.
	code, stdout, stderr = runCLITest(`{"mode":"clock","clock_format":"15:04:05"}`, "render", "--spec", "-",
		"--out", out, "--time", "2026-01-02T03:04:05Z", "--timezone", "UTC")
	if code != 0 || !strings.Contains(stdout, "next change 2026-01-02T03:04:06Z") {
		t.Fatalf("clock: exit %d %q %q", code, stdout, stderr)
	}
	// URL media without --allow-url: a placeholder and a warning, exit 0.
	code, _, stderr = runCLITest(`{"mode":"image","image":{"url":"http://example.invalid/a.png"}}`, "render", "--spec", "-", "--out", out)
	if code != 0 || !strings.Contains(stderr, "warning") {
		t.Fatalf("media: exit %d %q", code, stderr)
	}
}

func TestCLIErrors(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.json")
	os.WriteFile(bad, []byte(`{"mode":"text","fg":"red"}`), 0o644)
	unknown := filepath.Join(dir, "unknown.json")
	os.WriteFile(unknown, []byte(`{"mode":"text","colour":"#fff"}`), 0o644)
	out := filepath.Join(dir, "o.png")
	tests := []struct {
		args []string
		code int
		msg  string
	}{
		{nil, 2, "usage"},
		{[]string{"frobnicate"}, 2, "unknown command"},
		{[]string{"render"}, 2, "usage"},
		{[]string{"render", "--spec", bad, "--out", out}, 1, "invalid color"},
		{[]string{"render", "--spec", unknown, "--out", out}, 1, "unknown field"},
		{[]string{"render", "--spec", filepath.Join(dir, "missing.json"), "--out", out}, 1, "read spec"},
		{[]string{"render", "--spec", bad, "--out", out, "--size", "0x10"}, 2, "invalid size"},
		{[]string{"render", "--spec", bad, "--out", out, "--rotate", "45"}, 2, "multiple of 90"},
		{[]string{"render", "--spec", bad, "--out", out, "--time", "yesterday"}, 2, "--time"},
		{[]string{"show"}, 2, "usage"},
		{[]string{"test", "--device", filepath.Join(dir, "nofb")}, 1, "nofb"},
		{[]string{"test", "--step", "0s"}, 2, "usage"},
	}
	for _, tt := range tests {
		code, _, stderr := runCLITest("", tt.args...)
		if code != tt.code || !strings.Contains(stderr, tt.msg) {
			t.Errorf("%v: exit %d stderr %q, want %d %q", tt.args, code, stderr, tt.code, tt.msg)
		}
	}
	if code, stdout, _ := runCLITest("", "help"); code != 0 || !strings.Contains(stdout, "vt-reset") {
		t.Errorf("help: %d %q", code, stdout)
	}
}

func TestCLIShowWithoutDisplay(t *testing.T) {
	dir := t.TempDir()
	spec := filepath.Join(dir, "spec.json")
	os.WriteFile(spec, []byte(`{"mode":"status"}`), 0o644)
	code, _, stderr := runCLITest("", "show", "--spec", spec, "--device", filepath.Join(dir, "fb9"), "--seconds", "1", "--vt", "")
	if code != 1 || !strings.Contains(stderr, "fb9") {
		t.Fatalf("exit %d stderr %q", code, stderr)
	}
}

func TestCLIVTResetMissingVT(t *testing.T) {
	// Never touch a real VT from tests: a missing path is reported, not fatal.
	code, _, stderr := runCLITest("", "vt-reset", "--vt", filepath.Join(t.TempDir(), "tty7"), "--device", "none")
	if code != 0 || stderr == "" {
		t.Fatalf("exit %d stderr %q", code, stderr)
	}
}

func TestVTResetDeviceDefaultsToConfig(t *testing.T) {
	// A screen driven through display_device = /dev/fbN must be unblanked
	// by vt-reset after a crash, not whatever "auto" would pick.
	old := configDisplayDevice
	t.Cleanup(func() { configDisplayDevice = old })
	configDisplayDevice = func() string { return "/dev/fb3" }
	for _, tc := range []struct {
		flag  string
		given bool
		want  string
	}{
		{"", false, "/dev/fb3"},
		{"/dev/fb1", true, "/dev/fb1"},
		{"none", true, ""},
		{"", true, ""},
	} {
		if got := vtResetDevice(tc.flag, tc.given); got != tc.want {
			t.Errorf("vtResetDevice(%q, %v) = %q, want %q", tc.flag, tc.given, got, tc.want)
		}
	}
	configDisplayDevice = func() string { return "none" }
	if got := vtResetDevice("", false); got != "" {
		t.Errorf("display_device = none: %q", got)
	}
	// The whole command still works with the configured device (a missing
	// VT and an unusable framebuffer are not fatal).
	fb := filepath.Join(t.TempDir(), "fb3")
	os.WriteFile(fb, nil, 0o644)
	configDisplayDevice = func() string { return fb }
	if code, _, stderr := runCLITest("", "vt-reset", "--vt", filepath.Join(t.TempDir(), "tty7")); code != 0 {
		t.Fatalf("vt-reset: %d %q", code, stderr)
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string][2]int{"1024x768": {1024, 768}, " 640X480 ": {640, 480}, "1x1": {1, 1}} {
		w, h, err := parseSize(in)
		if err != nil || w != want[0] || h != want[1] {
			t.Errorf("parseSize(%q) = %d, %d, %v", in, w, h, err)
		}
	}
	for _, bad := range []string{"", "1024", "x768", "0x5", "-1x5", "99999x5", "axb"} {
		if _, _, err := parseSize(bad); err == nil {
			t.Errorf("parseSize(%q) accepted", bad)
		}
	}
}

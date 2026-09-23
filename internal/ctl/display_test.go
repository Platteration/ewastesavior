package ctl

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/platteration/ewastesavior/internal/proto"
)

func writePNG(t *testing.T, name string, w, h int, c color.Color) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	writeFile(t, name, buf.Bytes(), 0o644)
	return buf.Bytes()
}

func lastPatch(t *testing.T, h *fakeHive) proto.NodePatch {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.patches) == 0 {
		t.Fatal("no PATCH reached the hive")
	}
	return h.patches[len(h.patches)-1]
}

func TestDisplayUploadsImagesAndSendsValidSpec(t *testing.T) {
	h := newFakeHive(t)
	e := loggedIn(t, h)
	dir := t.TempDir()
	a := writePNG(t, filepath.Join(dir, "a.png"), 4, 3, color.RGBA{255, 0, 0, 255})
	b := writePNG(t, filepath.Join(dir, "b.png"), 3, 4, color.RGBA{0, 0, 255, 255})

	code, stdout, stderr := e.run("display", "lobby-1", "slideshow",
		"--images", filepath.Join(dir, "a.png")+", https://example.com/c.jpg,"+filepath.Join(dir, "b.png"),
		"--interval", "5", "--fit", "cover", "--bg", "102030")
	if code != 0 {
		t.Fatalf("code %d, stderr %s", code, stderr)
	}
	p := lastPatch(t, h)
	if p.Display == nil {
		t.Fatal("no display spec in the patch")
	}
	want := proto.DisplaySpec{Mode: proto.DisplaySlideshow, BG: "#102030", IntervalS: 5, Fit: "cover",
		Images: []proto.Media{{Blob: sha256Hex(a)}, {URL: "https://example.com/c.jpg"}, {Blob: sha256Hex(b)}}}
	if !reflect.DeepEqual(*p.Display, want) {
		t.Fatalf("spec = %+v, want %+v", *p.Display, want)
	}
	if err := proto.ValidateDisplaySpec(p.Display); err != nil {
		t.Fatalf("sent an invalid spec: %v", err)
	}
	for _, img := range [][]byte{a, b} {
		if got := h.blobs[sha256Hex(img)]; !bytes.Equal(got, img) {
			t.Error("image blob missing or different on the hive")
		}
	}
	if !strings.Contains(stdout, "slideshow") {
		t.Errorf("stdout: %s", stdout)
	}

	// --images-dir takes every image in name order.
	code, _, stderr = e.run("display", "lobby-1", "slideshow", "--images-dir", dir)
	if code != 0 {
		t.Fatalf("images-dir: %s", stderr)
	}
	if got := lastPatch(t, h).Display.Images; len(got) != 2 || got[0].Blob != sha256Hex(a) || got[1].Blob != sha256Hex(b) {
		t.Errorf("images-dir images = %+v", got)
	}

	code, _, stderr = e.run("display", "lobby-1", "image", "--image", filepath.Join(dir, "a.png"), "--fit", "contain")
	if code != 0 {
		t.Fatalf("image: %s", stderr)
	}
	if got := lastPatch(t, h).Display; got.Mode != proto.DisplayImage || got.Image == nil || got.Image.Blob != sha256Hex(a) {
		t.Errorf("image spec = %+v", got)
	}
	code, _, stderr = e.run("display", "lobby-1", "text", "--text", "Welcome\nfriends", "--title", "Lobby", "--fg", "#fff")
	if code != 0 {
		t.Fatalf("text: %s", stderr)
	}
	if got := lastPatch(t, h).Display; got.Mode != proto.DisplayText || got.Text != "Welcome\nfriends" || got.FG != "#fff" {
		t.Errorf("text spec = %+v", got)
	}
}

func TestDisplayRejectsBadInputLocally(t *testing.T) {
	h := newFakeHive(t)
	e := loggedIn(t, h)
	dir := t.TempDir()
	notImage := writeFile(t, filepath.Join(dir, "notes.txt"), []byte("not an image"), 0o644)
	img := filepath.Join(dir, "a.png")
	writePNG(t, img, 2, 2, color.White)
	before := h.requestCount()
	cases := [][]string{
		{"display", "lobby-1", "image", "--image", notImage},
		{"display", "lobby-1", "image"},
		{"display", "lobby-1", "text", "--image", img},
		{"display", "lobby-1", "clock", "--timezone", "Mars/Olympus"},
		{"display", "lobby-1", "color", "--bg", "#12345"},
		{"display", "lobby-1", "slideshow", "--images", img, "--interval", "1"},
		{"display", "lobby-1", "wall"},
		{"display", "lobby-1", "disco"},
		{"display", "lobby-1", "image", "--image", filepath.Join(dir, "missing.png")},
		{"display", "lobby-1", "image", "--image", img, "--fit", "zoom"},
		{"display", "a/../b", "off"},
	}
	for _, args := range cases {
		if code, _, stderr := e.run(args...); code != 2 {
			t.Errorf("%v: code %d, want 2 (%s)", args, code, stderr)
		}
	}
	if n := h.requestCount(); n != before {
		t.Fatalf("%d requests sent for invalid display specs", n-before)
	}
}

func TestWallCreateBuildsCellsRowMajor(t *testing.T) {
	h := newFakeHive(t)
	e := loggedIn(t, h)
	code, stdout, stderr := e.run("wall", "create", "--name", "lobby", "--rows", "2", "--cols", "3",
		"--nodes", "a, b,-,d,e,f", "--gap-x", "12", "--gap-y", "8", "--test")
	if code != 0 {
		t.Fatalf("code %d, stderr %s", code, stderr)
	}
	h.mu.Lock()
	w := h.walls["w1"]
	h.mu.Unlock()
	want := []proto.WallCell{
		{Node: "a", Row: 0, Col: 0}, {Node: "b", Row: 0, Col: 1},
		{Node: "d", Row: 1, Col: 0}, {Node: "e", Row: 1, Col: 1}, {Node: "f", Row: 1, Col: 2},
	}
	if !reflect.DeepEqual(w.Cells, want) {
		t.Fatalf("cells = %+v, want %+v", w.Cells, want)
	}
	if w.Name != "lobby" || w.Rows != 2 || w.Cols != 3 || w.GapXMM != 12 || w.GapYMM != 8 || w.Content.Mode != proto.DisplayTest {
		t.Errorf("wall = %+v", w)
	}
	if !strings.Contains(stdout, "w1") {
		t.Errorf("stdout: %s", stdout)
	}

	// Content switches keep the layout.
	if code, _, stderr := e.run("wall", "content", "w1", "--color", "00ff00"); code != 0 {
		t.Fatalf("wall content: %s", stderr)
	}
	h.mu.Lock()
	w = h.walls["w1"]
	h.mu.Unlock()
	if w.Content.Mode != proto.DisplayColor || w.Content.BG != "#00ff00" || len(w.Cells) != 5 {
		t.Errorf("after content: %+v", w)
	}
	if code, _, stderr := e.run("wall", "test", "w1"); code != 0 {
		t.Fatalf("wall test: %s", stderr)
	}
	h.mu.Lock()
	w = h.walls["w1"]
	h.mu.Unlock()
	if w.Content.Mode != proto.DisplayTest {
		t.Errorf("after test: %+v", w.Content)
	}
	code, stdout, _ = e.run("wall", "show", "w1")
	if code != 0 || !strings.Contains(stdout, "row 2") || !strings.Contains(stdout, "col 3") {
		t.Errorf("wall show: %s", stdout)
	}
}

func TestWallCreateWithImageUploads(t *testing.T) {
	h := newFakeHive(t)
	e := loggedIn(t, h)
	img := filepath.Join(t.TempDir(), "big.png")
	data := writePNG(t, img, 16, 9, color.Black)
	code, _, stderr := e.run("wall", "create", "--name", "w", "--rows", "1", "--cols", "2", "--nodes", "a,b", "--image", img, "--fit", "cover")
	if code != 0 {
		t.Fatalf("code %d: %s", code, stderr)
	}
	h.mu.Lock()
	w := h.walls["w1"]
	_, uploaded := h.blobs[sha256Hex(data)]
	h.mu.Unlock()
	if w.Content.Mode != proto.DisplayImage || w.Content.Image == nil || w.Content.Image.Blob != sha256Hex(data) || !uploaded {
		t.Fatalf("wall content %+v, uploaded %v", w.Content, uploaded)
	}
}

func TestWallCreateRejectsBadLayouts(t *testing.T) {
	h := newFakeHive(t)
	e := loggedIn(t, h)
	before := h.requestCount()
	for _, args := range [][]string{
		{"wall", "create", "--name", "x", "--rows", "2", "--cols", "2", "--nodes", "a,b,c"},
		{"wall", "create", "--name", "x", "--rows", "1", "--cols", "2", "--nodes", "a,a"},
		{"wall", "create", "--name", "x", "--rows", "1", "--cols", "2", "--nodes", "-,-"},
		{"wall", "create", "--name", "x", "--rows", "1", "--cols", "2", "--nodes", "a,"},
		{"wall", "create", "--name", "x", "--rows", "17", "--cols", "1", "--nodes", "a"},
		{"wall", "create", "--rows", "1", "--cols", "1", "--nodes", "a"},
		{"wall", "create", "--name", "x", "--rows", "1", "--cols", "1", "--nodes", "a", "--test", "--color", "#000"},
		{"wall", "create", "--name", "x", "--rows", "1", "--cols", "1", "--nodes", "a", "--gap-x", "-5"},
		{"wall", "bogus"},
	} {
		if code, _, stderr := e.run(args...); code != 2 {
			t.Errorf("%v: code %d (%s)", args, code, stderr)
		}
	}
	if h.requestCount() != before {
		t.Fatal("invalid walls reached the hive")
	}
}

func TestImageFilesIn(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"b.PNG", "a.jpg", "notes.txt", "c.webp"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "d.png"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := imageFilesIn(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, g := range got {
		names = append(names, filepath.Base(g))
	}
	if strings.Join(names, ",") != "a.jpg,b.PNG,c.webp" {
		t.Errorf("images = %v", names)
	}
}

package hive

import (
	"archive/zip"
	"bytes"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/platteration/ewastesavior/internal/proto"
)

func testPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 255 / w), G: uint8(y * 255 / h), B: 80, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestBlobPutGetAndHeaders(t *testing.T) {
	t.Parallel()
	h := newHive(t, nil)
	data := []byte("hello blob world")
	sum := sha(data)
	if st, _ := do(t, h.hc, "PUT", h.url+"/api/v1/blobs/"+sha([]byte("other")), testAdmin, data, nil); st != http.StatusBadRequest {
		t.Fatalf("hash mismatch: %d", st)
	}
	if st, _ := do(t, h.hc, "PUT", h.url+"/api/v1/blobs/NOTAHASH", testAdmin, data, nil); st != http.StatusBadRequest {
		t.Fatalf("bad hash: %d", st)
	}
	var info proto.BlobInfo
	if st, raw := do(t, h.hc, "PUT", h.url+"/api/v1/blobs/"+sum, testAdmin, data, &info); st != 200 || info.Size != int64(len(data)) || info.SHA256 != sum {
		t.Fatalf("put: %d %s", st, raw)
	}
	if fi, err := os.Stat(h.s.blobs.path(sum)); err != nil || fi.Size() != int64(len(data)) || !strings.Contains(h.s.blobs.path(sum), "/blobs/"+sum[:2]+"/") {
		t.Fatalf("layout: %v", err)
	}
	req, _ := http.NewRequest("GET", h.url+"/api/v1/blobs/"+sum, nil)
	req.Header.Set("Authorization", "Bearer "+testAdmin)
	req.Header.Set("Range", "bytes=6-9")
	resp, err := h.hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent || string(body) != "blob" {
		t.Fatalf("range: %d %q", resp.StatusCode, body)
	}
	for k, want := range map[string]string{
		"Content-Type":            "application/octet-stream",
		"Content-Security-Policy": "sandbox",
		"X-Content-Type-Options":  "nosniff",
		"Referrer-Policy":         "no-referrer",
	} {
		if got := resp.Header.Get(k); got != want {
			t.Fatalf("%s = %q, want %q", k, got, want)
		}
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") {
		t.Fatalf("content disposition: %q", cd)
	}
	// Browser upload: the hive hashes while streaming.
	var posted proto.BlobInfo
	if st, _ := do(t, h.hc, "POST", h.url+"/api/v1/admin/blobs", testAdmin, []byte("abc"), &posted); st != 200 || posted.SHA256 != sha([]byte("abc")) {
		t.Fatalf("admin post: %d %+v", st, posted)
	}
	var list []proto.BlobInfo
	h.mustAdmin("GET", "blobs", nil, &list)
	if len(list) != 2 {
		t.Fatalf("list: %+v", list)
	}
	// Unauthenticated access is refused.
	if st, _ := do(t, h.hc, "GET", h.url+"/api/v1/blobs/"+sum, "", nil, nil); st != http.StatusUnauthorized {
		t.Fatalf("anonymous get: %d", st)
	}
}

func TestBlobSizeAndDiskLimits(t *testing.T) {
	t.Parallel()
	h := newHive(t, func(c *Config) { c.tune.maxBlob = 100 })
	data := bytes.Repeat([]byte("z"), 101)
	if st, _ := do(t, h.hc, "PUT", h.url+"/api/v1/blobs/"+sha(data), testAdmin, data, nil); st != http.StatusRequestEntityTooLarge {
		t.Fatalf("too large: %d", st)
	}
	full := newHive(t, func(c *Config) {
		c.tune.diskFree = func(string) (int64, int64, bool) { return 600 << 20, 20 << 30, true }
	})
	small := []byte("fits")
	if st, _ := do(t, full.hc, "PUT", full.url+"/api/v1/blobs/"+sha(small), testAdmin, small, nil); st != http.StatusInsufficientStorage {
		t.Fatalf("disk would drop below max(5%%, 512 MiB): %d", st)
	}
}

func TestNodeBlobScope(t *testing.T) {
	t.Parallel()
	h := newHive(t, nil)
	n := h.newNode(nil)
	n.register()
	input := h.putBlob([]byte("input data"))
	secret := h.putBlob([]byte("someone else's data"))
	img := h.putBlob(testPNG(t, 16, 9))

	out := []byte("output from a task")
	if code, _ := n.api("PUT", "blobs/"+sha(out), out, nil); code != http.StatusForbidden {
		t.Fatalf("upload without a running task: %d", code)
	}
	for _, b := range []string{input, secret, img} {
		if code, _ := n.api("GET", "blobs/"+b, nil, nil); code != http.StatusForbidden {
			t.Fatalf("unreferenced blob readable: %d", code)
		}
	}
	h.submit(scriptJob(1, func(s *proto.JobSpec) { s.Inputs = []proto.Input{{Name: "in.txt", Blob: input}} }))
	tk := n.claim(1)[0]
	if code, raw := n.api("GET", "blobs/"+input, nil, nil); code != 200 || string(raw) != "input data" {
		t.Fatalf("task input: %d %q", code, raw)
	}
	if code, _ := n.api("GET", "blobs/"+secret, nil, nil); code != http.StatusForbidden {
		t.Fatalf("other blob: %d", code)
	}
	if code, _ := n.api("PUT", "blobs/"+sha(out), out, nil); code != 200 {
		t.Fatalf("upload with a running task: %d", code)
	}
	// Display media become readable once assigned.
	h.mustAdmin("PATCH", "nodes/"+n.req.NodeID, proto.NodePatch{Display: &proto.DisplaySpec{Mode: proto.DisplayImage, Image: &proto.Media{Blob: img}}}, nil)
	if code, _ := n.api("GET", "blobs/"+img, nil, nil); code != 200 {
		t.Fatalf("display media: %d", code)
	}
	// After the task ends the input is no longer readable.
	n.succeed(tk)
	if code, _ := n.api("GET", "blobs/"+input, nil, nil); code != http.StatusForbidden {
		t.Fatalf("input after the task: %d", code)
	}
	// Uploads need a running task.
	if code, _ := n.api("PUT", "blobs/"+sha([]byte("late")), []byte("late"), nil); code != http.StatusForbidden {
		t.Fatalf("late upload: %d", code)
	}
}

func TestLeaseUploadBudget(t *testing.T) {
	t.Parallel()
	h := newHive(t, nil)
	n := h.newNode(nil)
	n.register()
	h.submit(scriptJob(1, nil))
	tk := n.claim(1)[0]
	h.s.mu.Lock()
	h.s.tasks[tk.ID].uploaded = leaseUploadCap - 10
	h.s.mu.Unlock()
	data := bytes.Repeat([]byte("q"), 11)
	if code, _ := n.api("PUT", "blobs/"+sha(data), data, nil); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over budget: %d", code)
	}
	if code, _ := n.api("PUT", "blobs/"+sha(data[:10]), data[:10], nil); code != 200 {
		t.Fatalf("within budget: %d", code)
	}
}

func TestBlobReferencesAndGC(t *testing.T) {
	t.Parallel()
	h := newHive(t, func(c *Config) { c.tune.blobTouch = 100 * time.Millisecond; c.tune.uploadGC = 100 * time.Millisecond })
	n := h.newNode(nil)
	n.register()
	in := h.putBlob([]byte("job input"))
	loose := h.putBlob([]byte("unreferenced"))
	d := h.submit(scriptJob(1, func(s *proto.JobSpec) { s.Inputs = []proto.Input{{Name: "a", Blob: in}} }))
	st, raw := do(t, h.hc, "DELETE", h.url+"/api/v1/admin/blobs/"+in, testAdmin, nil, nil)
	if st != http.StatusConflict || !strings.Contains(string(raw), d.ID) {
		t.Fatalf("referenced delete: %d %s", st, raw)
	}
	var list []proto.BlobInfo
	h.mustAdmin("GET", "blobs", nil, &list)
	for _, b := range list {
		if (b.SHA256 == in) != b.Referenced {
			t.Fatalf("referenced flag: %+v", b)
		}
	}
	// Recently touched blobs survive GC.
	var gc struct {
		Deleted    int64 `json:"deleted"`
		FreedBytes int64 `json:"freed_bytes"`
	}
	h.mustAdmin("POST", "blobs/gc", nil, &gc)
	if gc.Deleted != 0 {
		t.Fatalf("gc deleted touched blobs: %+v", gc)
	}
	time.Sleep(150 * time.Millisecond)
	h.mustAdmin("POST", "blobs/gc", nil, &gc)
	if gc.Deleted != 1 || gc.FreedBytes != int64(len("unreferenced")) {
		t.Fatalf("gc: %+v", gc)
	}
	if _, err := os.Stat(h.s.blobs.path(loose)); !os.IsNotExist(err) {
		t.Fatal("gc left the file")
	}
	// Outputs of tasks are roots; once the job is deleted they are garbage
	// for the automatic node-upload GC.
	tk := n.claim(1)[0]
	outData := []byte("result")
	n.api("PUT", "blobs/"+sha(outData), outData, nil)
	n.report(tk, proto.TaskReport{State: proto.TaskSucceeded, Outputs: []proto.Output{{Name: "r.txt", Blob: sha(outData), Size: 6}}})
	time.Sleep(150 * time.Millisecond)
	h.s.mu.Lock()
	deleted, _ := h.s.gcLocked(true, time.Now())
	h.s.mu.Unlock()
	if deleted != 0 {
		t.Fatal("referenced output collected")
	}
	h.mustAdmin("DELETE", "jobs/"+d.ID, nil, nil)
	h.s.mu.Lock()
	deleted, _ = h.s.gcLocked(true, time.Now())
	h.s.mu.Unlock()
	if deleted != 1 { // the output; the admin-uploaded input stays for manual GC
		t.Fatalf("auto gc after job delete: %d", deleted)
	}
	if st := h.admin("DELETE", "blobs/"+in, nil, nil); st != 200 {
		t.Fatalf("delete unreferenced: %d", st)
	}
}

func TestOutputsManifestAndZip(t *testing.T) {
	t.Parallel()
	h := newHive(t, nil)
	n := h.newNode(nil)
	n.register()
	d := h.submit(scriptJob(2, func(s *proto.JobSpec) { s.Outputs = []string{"*.txt", "sub/*"} }))
	tasks := n.claim(2)
	for i, tk := range tasks {
		a := []byte("alpha " + tk.ID)
		b := []byte("beta")
		for _, blob := range [][]byte{a, b} {
			if code, _ := n.api("PUT", "blobs/"+sha(blob), blob, nil); code != 200 {
				t.Fatalf("upload: %d", code)
			}
		}
		outs := []proto.Output{{Name: "a.txt", Blob: sha(a), Size: int64(len(a))}, {Name: "sub/b.bin", Blob: sha(b), Size: 4}}
		if code := n.report(tk, proto.TaskReport{State: proto.TaskSucceeded, Outputs: outs, CPUSeconds: float64(i)}); code != 200 {
			t.Fatalf("report: %d", code)
		}
	}
	var entries []proto.OutputEntry
	h.mustAdmin("GET", "jobs/"+d.ID+"/outputs", nil, &entries)
	if len(entries) != 4 || entries[0].Index != 0 || entries[0].Name != "a.txt" || entries[3].Name != "sub/b.bin" {
		t.Fatalf("manifest: %+v", entries)
	}
	resp, err := adminGet(h, "jobs/"+d.ID+"/outputs.zip")
	if err != nil || resp.code != 200 || resp.hdr.Get("Content-Type") != "application/zip" {
		t.Fatalf("zip: %v %d", err, resp.code)
	}
	zr, err := zip.NewReader(bytes.NewReader(resp.body), int64(len(resp.body)))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]string{}
	for _, f := range zr.File {
		if f.Method != zip.Store {
			t.Fatalf("%s: method %d", f.Name, f.Method)
		}
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		names[f.Name] = string(b)
	}
	if names["task-0/a.txt"] != "alpha "+tasks[0].ID || names["task-1/sub/b.bin"] != "beta" || len(names) != 4 {
		t.Fatalf("zip entries: %v", names)
	}
	one, err := adminGet(h, "tasks/"+tasks[1].ID+"/outputs/sub/b.bin")
	if err != nil || one.code != 200 || string(one.body) != "beta" || !strings.Contains(one.hdr.Get("Content-Disposition"), `filename=b.bin`) ||
		one.hdr.Get("Content-Security-Policy") != "sandbox" {
		t.Fatalf("single output: %d %q %v", one.code, one.body, one.hdr)
	}
	if st := h.admin("GET", "tasks/"+tasks[1].ID+"/outputs/nope", nil, nil); st != http.StatusNotFound {
		t.Fatalf("missing output: %d", st)
	}
}

func TestRenderEndpoint(t *testing.T) {
	t.Parallel()
	h := newHive(t, nil)
	src := testPNG(t, 64, 32)
	sum := h.putBlob(src)
	q := "cw=800&ch=600&x=400&y=0&w=400&h=300&pw=200&ph=150&fit=cover&bg=102030"
	resp, err := adminGet(h, "/api/v1/blobs/"+sum+"/render?"+q)
	if err != nil || resp.code != 200 || resp.hdr.Get("Content-Type") != "image/png" {
		t.Fatalf("render: %v %d %s", err, resp.code, resp.body)
	}
	img, err := png.Decode(bytes.NewReader(resp.body))
	if err != nil || img.Bounds().Dx() != 200 || img.Bounds().Dy() != 150 {
		t.Fatalf("rendered image: %v %v", err, img.Bounds())
	}
	h.s.renders.mu.Lock()
	cached := h.s.renders.lru.Len()
	h.s.renders.mu.Unlock()
	if cached != 1 {
		t.Fatalf("cache entries: %d", cached)
	}
	again, _ := adminGet(h, "/api/v1/blobs/"+sum+"/render?"+q)
	if !bytes.Equal(again.body, resp.body) {
		t.Fatal("cached render differs")
	}
	for _, bad := range []string{"cw=0&ch=1&w=1&h=1&pw=1&ph=1", "cw=1&ch=1&w=1&h=1&pw=5000&ph=1", "cw=1&ch=1&w=1&h=1&pw=1&ph=1&fit=zoom"} {
		if r, _ := adminGet(h, "/api/v1/blobs/"+sum+"/render?"+bad); r.code != http.StatusBadRequest {
			t.Fatalf("%s: %d", bad, r.code)
		}
	}
	notImage := h.putBlob([]byte("definitely not an image"))
	if r, _ := adminGet(h, "/api/v1/blobs/"+notImage+"/render?cw=1&ch=1&w=1&h=1&pw=1&ph=1"); r.code != http.StatusUnprocessableEntity {
		t.Fatalf("non-image: %d", r.code)
	}
	// Nodes may render only their own display media.
	n := h.newNode(nil)
	n.register()
	if code, _ := n.api("GET", "blobs/"+sum+"/render?"+q, nil, nil); code != http.StatusForbidden {
		t.Fatalf("node without the media: %d", code)
	}
	var v proto.NodeView
	h.mustAdmin("PATCH", "nodes/"+n.req.NodeID, proto.NodePatch{Display: &proto.DisplaySpec{Mode: proto.DisplaySlideshow,
		Images: []proto.Media{{Blob: sum}, {URL: "https://example.com/x.png"}}}}, &v)
	if v.Display.Images[0].Width != 64 || v.Display.Images[0].Height != 32 || v.Display.Rev == 0 {
		t.Fatalf("media dims not filled: %+v", v.Display.Images)
	}
	if code, _ := n.api("GET", "blobs/"+sum+"/render?"+q, nil, nil); code != 200 {
		t.Fatalf("node with the media: %d", code)
	}
	// Unknown blobs in specs are refused.
	if st := h.admin("PATCH", "nodes/"+n.req.NodeID, proto.NodePatch{Display: &proto.DisplaySpec{Mode: proto.DisplayImage,
		Image: &proto.Media{Blob: sha([]byte("nope"))}}}, nil); st != http.StatusBadRequest {
		t.Fatalf("unknown media blob: %d", st)
	}
}

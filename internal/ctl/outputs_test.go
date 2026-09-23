package ctl

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/platteration/ewastesavior/internal/proto"
)

func TestOutputPathRefusesUnsafeNames(t *testing.T) {
	bad := []string{"../x", "a/../../b", "/etc/passwd", "C:x", `C:\x`, `a\..\b`, "..", ".", "", "a//b",
		"./a", "a/./b", "-rf", ".savior-script", "a\x00b", "a\nb", "trailing.", "trailing ", strings.Repeat("a", 300)}
	for _, name := range bad {
		if p, err := OutputPath(proto.OutputEntry{Index: 0, Name: name}); err == nil {
			t.Errorf("OutputPath(%q) = %q, want refusal", name, p)
		}
	}
	if runtime.GOOS == "windows" {
		for _, name := range []string{"CON", "nul.txt", "a/aux"} {
			if _, err := OutputPath(proto.OutputEntry{Name: name}); err == nil {
				t.Errorf("OutputPath(%q) accepted a reserved Windows name", name)
			}
		}
	}
	if _, err := OutputPath(proto.OutputEntry{Index: -1, Name: "ok"}); err == nil {
		t.Error("negative index accepted")
	}
	for name, want := range map[string]string{"ok.txt": "task-3/ok.txt", "out/deep/r.bin": "task-3/out/deep/r.bin", "..hidden": "task-3/..hidden"} {
		if got, err := OutputPath(proto.OutputEntry{Index: 3, Name: name}); err != nil || got != want {
			t.Errorf("OutputPath(%q) = %q, %v; want %q", name, got, err, want)
		}
	}
}

func TestOutputsDownloadIsSafe(t *testing.T) {
	h := newFakeHive(t)
	e := loggedIn(t, h)
	base := t.TempDir()
	dir := filepath.Join(base, "results")
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	blob := func(s string) (string, int64) {
		sha := sha256Hex([]byte(s))
		h.blobs[sha] = []byte(s)
		return sha, int64(len(s))
	}
	okSHA, okSize := blob("good output\n")
	evilSHA, evilSize := blob("evil\n")
	oldSHA, oldSize := blob("new content\n")
	badSHA, badSize := blob("will be tampered\n")
	h.corrupt[badSHA] = true
	entries := []proto.OutputEntry{
		{TaskID: "t0", Index: 0, Name: "ok.txt", Blob: okSHA, Size: okSize},
		{TaskID: "t0", Index: 0, Name: "sub/dir/ok2.txt", Blob: okSHA, Size: okSize},
		{TaskID: "t0", Index: 0, Name: "../x", Blob: evilSHA, Size: evilSize},
		{TaskID: "t0", Index: 0, Name: "a/../../b", Blob: evilSHA, Size: evilSize},
		{TaskID: "t0", Index: 0, Name: "/abs", Blob: evilSHA, Size: evilSize},
		{TaskID: "t0", Index: 0, Name: "C:x", Blob: evilSHA, Size: evilSize},
		{TaskID: "t0", Index: 0, Name: `..\x`, Blob: evilSHA, Size: evilSize},
		{TaskID: "t2", Index: 2, Name: "exists.txt", Blob: oldSHA, Size: oldSize},
		{TaskID: "t3", Index: 3, Name: "tampered.txt", Blob: badSHA, Size: badSize},
		{TaskID: "t4", Index: 4, Name: "short.txt", Blob: okSHA, Size: okSize + 5},
		{TaskID: "t5", Index: 5, Name: "badhash.txt", Blob: "not-a-hash", Size: 1},
	}
	h.outputs["jout"] = entries
	// A file that already exists is never overwritten.
	if err := os.MkdirAll(filepath.Join(dir, "task-2"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "task-2", "exists.txt"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A symlink planted inside the target must not lead outside it.
	symlinked := false
	if runtime.GOOS != "windows" {
		if err := os.Symlink(outside, filepath.Join(dir, "task-1")); err == nil {
			symlinked = true
			h.outputs["jout"] = append(h.outputs["jout"], proto.OutputEntry{TaskID: "t1", Index: 1, Name: "escaped.txt", Blob: evilSHA, Size: evilSize})
		}
	}

	code, stdout, stderr := e.run("outputs", "jout", "-o", dir)
	if code != 1 {
		t.Fatalf("code %d, want 1 (some refused); stderr %s", code, stderr)
	}
	for _, rel := range []string{"task-0/ok.txt", "task-0/sub/dir/ok2.txt"} {
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil || string(b) != "good output\n" {
			t.Errorf("%s = %q, %v", rel, b, err)
		}
		if !strings.Contains(stdout, filepath.FromSlash(rel)) {
			t.Errorf("stdout does not list %s:\n%s", rel, stdout)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "task-2", "exists.txt")); string(b) != "old" {
		t.Errorf("existing file overwritten: %q", b)
	}
	for _, gone := range []string{"task-3/tampered.txt", "task-4/short.txt"} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(gone))); err == nil {
			t.Errorf("%s kept although it failed verification", gone)
		}
	}
	for _, want := range []string{`unsafe output name "../x"`, `"a/../../b"`, `"/abs"`, `"C:x"`, "already exists", "sha256", "invalid blob hash"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
	// Nothing appeared outside the target directory.
	ents, _ := os.ReadDir(base)
	var names []string
	for _, en := range ents {
		names = append(names, en.Name())
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "outside,results" {
		t.Errorf("base dir contains %v", names)
	}
	if ents, _ := os.ReadDir(outside); len(ents) != 0 {
		t.Errorf("files written through the symlink: %v", ents)
	}
	if symlinked && !strings.Contains(stderr, "escaped.txt") {
		t.Errorf("symlinked entry not reported:\n%s", stderr)
	}
}

func TestDownloadOutputsLibrary(t *testing.T) {
	h := newFakeHive(t)
	c, err := NewClient(h.url, "", WithAdminToken(h.token), WithTOFU(true))
	if err != nil {
		t.Fatal(err)
	}
	sha := sha256Hex([]byte("x"))
	h.blobs[sha] = []byte("x")
	dir := filepath.Join(t.TempDir(), "o")
	res, err := c.DownloadOutputs(context.Background(), []proto.OutputEntry{
		{Index: 0, Name: "a.txt", Blob: sha, Size: 1},
		{Index: 0, Name: "a.txt", Blob: sha, Size: 1}, // duplicate: second must not overwrite
		{Index: 1, Name: "../../etc/x", Blob: sha, Size: 1},
	}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Written) != 1 || len(res.Refused) != 2 {
		t.Fatalf("written %v refused %v", res.Written, res.Refused)
	}
}

func TestOutputsZipAndList(t *testing.T) {
	h := newFakeHive(t)
	e := loggedIn(t, h)
	h.outputs["jz"] = []proto.OutputEntry{{TaskID: "t0", Index: 0, Name: "a.txt", Blob: strings.Repeat("a", 64), Size: 2048}}
	code, stdout, stderr := e.run("outputs", "jz")
	if code != 0 || !strings.Contains(stdout, "a.txt") || !strings.Contains(stdout, "2.0 KiB") {
		t.Fatalf("list: %d %s %s", code, stdout, stderr)
	}
	zipPath := filepath.Join(t.TempDir(), "out.zip")
	if code, _, stderr := e.run("outputs", "jz", "--zip", "-o", zipPath); code != 0 {
		t.Fatalf("zip: %s", stderr)
	}
	if b, _ := os.ReadFile(zipPath); string(b) != "PK-fake-zip-jz" {
		t.Errorf("zip content %q", b)
	}
	if code, _, stderr := e.run("outputs", "jz", "--zip", "-o", zipPath); code != 1 || !strings.Contains(stderr, "already exists") {
		t.Errorf("zip overwrite: %d %s", code, stderr)
	}
}

func TestFetchAndUpload(t *testing.T) {
	h := newFakeHive(t)
	e := loggedIn(t, h)
	dir := t.TempDir()
	src := writeFile(t, filepath.Join(dir, "payload.bin"), []byte("payload bytes"), 0o644)
	code, stdout, stderr := e.run("upload", src)
	sha := sha256Hex([]byte("payload bytes"))
	if code != 0 || !strings.HasPrefix(stdout, sha) {
		t.Fatalf("upload: %d %q %s", code, stdout, stderr)
	}
	dst := filepath.Join(dir, "copy.bin")
	if code, _, stderr := e.run("fetch", sha, "-o", dst); code != 0 {
		t.Fatalf("fetch: %s", stderr)
	}
	if b, _ := os.ReadFile(dst); string(b) != "payload bytes" {
		t.Errorf("fetched %q", b)
	}
	h.corrupt[sha] = true
	bad := filepath.Join(dir, "bad.bin")
	if code, _, stderr := e.run("fetch", sha, "-o", bad); code != 1 || !strings.Contains(stderr, "does not match") {
		t.Errorf("tampered fetch: %d %s", code, stderr)
	}
	if _, err := os.Stat(bad); err == nil {
		t.Error("tampered download was kept")
	}
	if code, _, _ := e.run("fetch", "nothex", "-o", bad); code != 2 {
		t.Error("invalid hash accepted")
	}
}

// countingSeeker counts the bytes read from it.
type countingSeeker struct {
	*strings.Reader
	n int
}

func (c *countingSeeker) Read(p []byte) (int, error) {
	n, err := c.Reader.Read(p)
	c.n += n
	return n, err
}

func TestPutBlobSkipsBodyWhenHiveHasIt(t *testing.T) {
	h := newFakeHive(t)
	c, err := NewClient(h.url, "", WithAdminToken(h.token), WithTOFU(true))
	if err != nil {
		t.Fatal(err)
	}
	content := strings.Repeat("x", 1<<20)
	sha := sha256Hex([]byte(content))
	h.mu.Lock()
	h.blobs[sha] = []byte(content)
	h.mu.Unlock()
	r := &countingSeeker{Reader: strings.NewReader(content)}
	info, err := c.PutBlob(context.Background(), sha, int64(len(content)), r)
	if err != nil || info.SHA256 != sha {
		t.Fatalf("PutBlob: %+v %v", info, err)
	}
	if r.n != 0 {
		t.Errorf("%d body bytes read although the hive already had the blob", r.n)
	}
	// A new blob is sent in full, and a wrong hash is refused by the hive.
	other := "fresh content"
	if _, err := c.PutBlob(context.Background(), sha256Hex([]byte(other)), int64(len(other)), strings.NewReader(other)); err != nil {
		t.Fatalf("new blob: %v", err)
	}
	wrong := strings.Repeat("0", 64)
	if _, err := c.PutBlob(context.Background(), wrong, 3, strings.NewReader("abc")); err == nil || !strings.Contains(err.Error(), "hash mismatch") {
		t.Fatalf("mismatched hash: %v", err)
	}
}

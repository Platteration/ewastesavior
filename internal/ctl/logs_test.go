package ctl

import (
	"bytes"
	"strings"
	"testing"

	"github.com/platteration/ewastesavior/internal/proto"
)

func TestLogsFollowUsesOffsets(t *testing.T) {
	h := newFakeHive(t)
	e := loggedIn(t, h)
	full := "alpha\nbeta\ngamma\ndelta\n"
	h.mu.Lock()
	h.logs["t1"] = []byte(full)
	h.logChunk = 6
	h.taskState["t1"] = proto.TaskRunning
	h.mu.Unlock()

	code, stdout, stderr := e.run("logs", "-f", "t1")
	if code != 0 {
		t.Fatalf("code %d, stderr %s", code, stderr)
	}
	if stdout != full {
		t.Fatalf("stdout = %q, want %q", stdout, full)
	}
	h.mu.Lock()
	reqs := append([]logReq(nil), h.logReqs...)
	h.mu.Unlock()
	var offs []int64
	for _, r := range reqs {
		offs = append(offs, r.offset)
	}
	// 6-byte chunks: 0, 6, 12, 18, then the drained offset 23 (empty, task
	// terminal) and one final read.
	want := []int64{0, 6, 12, 18, 23, 23}
	if len(offs) != len(want) {
		t.Fatalf("offsets %v, want %v", offs, want)
	}
	for i := range want {
		if offs[i] != want[i] {
			t.Fatalf("offsets %v, want %v", offs, want)
		}
	}
	if reqs[0].waitS != "25" {
		t.Errorf("follow request wait_s = %q, want 25", reqs[0].waitS)
	}
}

func TestLogsWithoutFollowReadsAll(t *testing.T) {
	h := newFakeHive(t)
	e := loggedIn(t, h)
	h.mu.Lock()
	h.logs["t2"] = []byte("0123456789abcdef")
	h.logChunk = 5
	h.taskState["t2"] = proto.TaskSucceeded
	h.mu.Unlock()
	code, stdout, _ := e.run("logs", "t2")
	if code != 0 || stdout != "0123456789abcdef" {
		t.Fatalf("code %d stdout %q", code, stdout)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.logReqs {
		if r.waitS != "" {
			t.Errorf("non-follow request long-polled (wait_s=%s)", r.waitS)
		}
	}
}

func TestLogsSanitizedOnTerminal(t *testing.T) {
	h := newFakeHive(t)
	e := loggedIn(t, h)
	raw := "ok\t1\n\x1b]0;pwned\x07\x1b[31mred\r\n\u009b2J\u202eevil\n"
	h.mu.Lock()
	h.logs["t3"] = []byte(raw)
	h.taskState["t3"] = proto.TaskSucceeded
	h.mu.Unlock()

	e.stdoutTTY = true
	code, stdout, _ := e.run("logs", "t3")
	if code != 0 {
		t.Fatal(code)
	}
	if want := "ok\t1\n]0;pwned[31mred\n2Jevil\n"; stdout != want {
		t.Errorf("tty output = %q, want %q", stdout, want)
	}
	e.stdoutTTY = false
	if _, stdout, _ := e.run("logs", "t3"); stdout != raw {
		t.Errorf("piped output altered: %q", stdout)
	}
}

func TestSanitizeWriterSplitRunes(t *testing.T) {
	var buf bytes.Buffer
	w := &sanitizeWriter{w: &buf}
	in := []byte("h\u00e9llo \u00e9\u4e16\U0001F600 \x1b[0m end")
	for i := range in {
		if _, err := w.Write(in[i : i+1]); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if want := "h\u00e9llo \u00e9\u4e16\U0001F600 [0m end"; buf.String() != want {
		t.Errorf("got %q, want %q", buf.String(), want)
	}
	buf.Reset()
	w = &sanitizeWriter{w: &buf}
	_, _ = w.Write([]byte{0xe4, 0xb8}) // truncated rune at the end of the stream
	_ = w.Flush()
	if !strings.Contains(buf.String(), "\ufffd") {
		t.Errorf("truncated rune: %q", buf.String())
	}
}

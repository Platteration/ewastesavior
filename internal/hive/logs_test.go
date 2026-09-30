package hive

import (
	"bytes"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/platteration/ewastesavior/internal/proto"
)

func TestLogRing(t *testing.T) {
	t.Parallel()
	l := newLogRing()
	l.append(0, []byte("hello "))
	l.append(3, []byte("lo world")) // overlapping retry
	l.append(0, []byte("hel"))      // fully known
	if d, next := l.read(0, 100); string(d) != "hello world" || next != 11 {
		t.Fatalf("read: %q %d", d, next)
	}
	if d, next := l.read(6, 3); string(d) != "wor" || next != 9 {
		t.Fatalf("partial: %q %d", d, next)
	}
	l.append(20, []byte("after gap"))
	if d, next := l.read(0, 100); string(d) != "after gap" || next != 29 {
		t.Fatalf("gap: %q %d", d, next)
	}
	// The ring keeps about the last 1 MiB.
	big := bytes.Repeat([]byte("x"), 700<<10)
	l.append(29, big)
	l.append(29+int64(len(big)), big)
	if len(l.data) > logRingSize+logRingSize/4 || l.total != 29+2*int64(len(big)) {
		t.Fatalf("ring size %d total %d", len(l.data), l.total)
	}
	d, next := l.read(0, logRingSize)
	if next != l.total || int64(len(d)) != l.total-l.base {
		t.Fatalf("read from start after trimming: %d bytes, next %d", len(d), next)
	}
	tail, base := l.tailBytes(logTailSize)
	if len(tail) != logTailSize || base != l.total-logTailSize {
		t.Fatal("tail")
	}
}

func TestTaskLogs(t *testing.T) {
	t.Parallel()
	h := newHive(t, nil)
	n := h.newNode(nil)
	n.register()
	h.submit(scriptJob(1, nil))
	tk := n.claim(1)[0]
	post := func(off int64, data string, lease string) (int, int64) {
		var resp struct{ Next int64 }
		code, _ := n.api("POST", "tasks/"+tk.ID+"/log?lease="+lease+"&offset="+strconv.FormatInt(off, 10), data, &resp)
		return code, resp.Next
	}
	if code, next := post(0, "line 1\n", tk.Lease); code != 200 || next != 7 {
		t.Fatalf("first chunk: %d %d", code, next)
	}
	if code, next := post(0, "line 1\nline 2\n", tk.Lease); code != 200 || next != 14 {
		t.Fatalf("retried chunk: %d %d", code, next)
	}
	if code, _ := post(14, "x", "wrong-lease"); code != http.StatusConflict {
		t.Fatalf("stale lease: %d", code)
	}
	for _, bad := range []string{"-1", "9223372036854775000", "abc"} {
		if code, _ := n.api("POST", "tasks/"+tk.ID+"/log?lease="+tk.Lease+"&offset="+bad, "x", nil); code != http.StatusBadRequest {
			t.Fatalf("offset %s: %d", bad, code)
		}
	}
	big := strings.Repeat("y", maxLogChunk+1)
	if code, _ := post(14, big, tk.Lease); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized chunk: %d", code)
	}

	read := func(off int64, wait int) (string, int64, http.Header) {
		resp, err := adminGet(h, "tasks/"+tk.ID+"/log?offset="+strconv.FormatInt(off, 10)+"&wait_s="+strconv.Itoa(wait))
		if err != nil {
			t.Fatal(err)
		}
		next, _ := strconv.ParseInt(resp.hdr.Get(logOffsetHd), 10, 64)
		return string(resp.body), next, resp.hdr
	}
	body, next, hdr := read(0, 0)
	if body != "line 1\nline 2\n" || next != 14 || !strings.HasPrefix(hdr.Get("Content-Type"), "text/plain") {
		t.Fatalf("read: %q %d %v", body, next, hdr.Get("Content-Type"))
	}
	// Long-poll: returns as soon as data arrives.
	go func() {
		time.Sleep(100 * time.Millisecond)
		post(14, "line 3\n", tk.Lease)
	}()
	start := time.Now()
	body, next, _ = read(14, 10)
	if body != "line 3\n" || next != 21 || time.Since(start) > 5*time.Second {
		t.Fatalf("long poll: %q %d after %v", body, next, time.Since(start))
	}
	// Finishing moves the tail to disk; reads keep working with offsets.
	n.succeed(tk)
	h.s.io.flush()
	onDisk, err := os.ReadFile(h.s.logPath(tk.ID))
	if err != nil || string(onDisk) != "line 1\nline 2\nline 3\n" {
		t.Fatalf("tail file: %q %v", onDisk, err)
	}
	if body, next, _ = read(7, 5); body != "line 2\nline 3\n" || next != 21 {
		t.Fatalf("read finished: %q %d", body, next)
	}
	if body, next, _ = read(21, 5); body != "" || next != 21 {
		t.Fatalf("read at end of a finished log must not wait: %q %d", body, next)
	}
	// Logs are never in state.json.
	h.s.persist(true)
	state, _ := os.ReadFile(h.dir + "/state.json")
	if bytes.Contains(state, []byte("line 2")) {
		t.Fatal("log content in state.json")
	}
}

func TestLogTailKeepsLast64KiB(t *testing.T) {
	t.Parallel()
	h := newHive(t, nil)
	n := h.newNode(nil)
	n.register()
	h.submit(scriptJob(1, nil))
	tk := n.claim(1)[0]
	chunk := strings.Repeat("0123456789abcdef", 16<<10/16) // 16 KiB
	var off int64
	for i := 0; i < 6; i++ {
		code, _ := n.api("POST", "tasks/"+tk.ID+"/log?lease="+tk.Lease+"&offset="+strconv.FormatInt(off, 10), chunk, nil)
		if code != 200 {
			t.Fatalf("chunk %d: %d", i, code)
		}
		off += int64(len(chunk))
	}
	n.succeed(tk)
	h.s.io.flush()
	fi, err := os.Stat(h.s.logPath(tk.ID))
	if err != nil || fi.Size() != logTailSize || fi.Mode().Perm() != 0o600 {
		t.Fatalf("tail file: %v %v", fi, err)
	}
	resp, err := adminGet(h, "tasks/"+tk.ID+"/log?offset=0")
	if err != nil || int64(len(resp.body)) != logTailSize || resp.hdr.Get(logOffsetHd) != strconv.FormatInt(off, 10) {
		t.Fatalf("read of the tail: %d bytes, %v", len(resp.body), resp.hdr)
	}
}

type rawResp struct {
	code int
	hdr  http.Header
	body []byte
}

// adminGet GETs an admin path (or an absolute path starting with "/") with
// the admin token and returns the raw response.
func adminGet(h *testHive, path string) (rawResp, error) {
	u := h.url + "/api/v1/admin/" + path
	if strings.HasPrefix(path, "/") {
		u = h.url + path
	}
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("Authorization", "Bearer "+testAdmin)
	resp, err := h.hc.Do(req)
	if err != nil {
		return rawResp{}, err
	}
	defer resp.Body.Close()
	var b bytes.Buffer
	b.ReadFrom(resp.Body)
	return rawResp{code: resp.StatusCode, hdr: resp.Header, body: b.Bytes()}, nil
}

// Deleting a job removes every tail file of its tasks, also one written by
// an earlier attempt when the last attempt produced no output.
func TestLogTailRemovedWithJob(t *testing.T) {
	t.Parallel()
	h := newHive(t, nil)
	n := h.newNode(nil)
	n.register()
	d := h.submit(scriptJob(1, nil))
	tk := n.claim(1)[0]
	if code, _ := n.api("POST", "tasks/"+tk.ID+"/log?lease="+tk.Lease+"&offset=0", "attempt 1\n", nil); code != 200 {
		t.Fatalf("log: %d", code)
	}
	if code := n.report(tk, proto.TaskReport{State: proto.TaskPreempted}); code != 200 {
		t.Fatalf("preempt: %d", code)
	}
	h.s.io.flush()
	path := h.s.logPath(tk.ID)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("tail of attempt 1: %v", err)
	}
	again := n.claim(1)
	if len(again) != 1 || again[0].ID != tk.ID {
		t.Fatalf("attempt 2: %+v", again)
	}
	n.succeed(again[0]) // no output this time
	h.mustAdmin("DELETE", "jobs/"+d.ID, nil, nil)
	h.s.io.flush()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("tail file left behind by the deleted job: %v", err)
	}
}

// A follower still at the end of attempt 1 must learn at once that attempt
// 2 started a new stream (SPEC-RUNTIME-04, WEB-1): the hive used to hold
// the request until attempt 2 passed the stale offset and then return only
// the bytes after it, so the start of attempt 2 was silently skipped.
func TestTaskLogAcrossAttempts(t *testing.T) {
	t.Parallel()
	h := newHive(t, nil)
	n := h.newNode(nil)
	n.register()
	h.submit(scriptJob(1, nil))
	tk := n.claim(1)[0]
	post := func(tk proto.Task, off int64, data string) {
		t.Helper()
		if code, raw := n.api("POST", "tasks/"+tk.ID+"/log?lease="+tk.Lease+"&offset="+strconv.FormatInt(off, 10), data, nil); code != 200 {
			t.Fatalf("log append: %d %s", code, raw)
		}
	}
	read := func(off int64, wait int) (string, int64, string, time.Duration) {
		t.Helper()
		start := time.Now()
		resp, err := adminGet(h, "tasks/"+tk.ID+"/log?offset="+strconv.FormatInt(off, 10)+"&wait_s="+strconv.Itoa(wait))
		if err != nil || resp.code != 200 {
			t.Fatalf("read log: %v %d", err, resp.code)
		}
		next, _ := strconv.ParseInt(resp.hdr.Get(logOffsetHd), 10, 64)
		return string(resp.body), next, resp.hdr.Get(logAttemptHd), time.Since(start)
	}

	first := "attempt 1 wrote this long line\n"
	post(tk, 0, first)
	if body, next, att, _ := read(0, 0); body != first || next != int64(len(first)) || att != "1" {
		t.Fatalf("attempt 1: %q next %d attempt %q", body, next, att)
	}
	// Preempted: requeued without using up a retry, then dispatched again.
	if code := n.report(tk, proto.TaskReport{State: proto.TaskPreempted}); code != 200 {
		t.Fatalf("preempt: %d", code)
	}
	h.s.io.flush()
	// Between attempts the finished attempt's log is served, still labelled 1.
	if body, next, att, _ := read(0, 0); body != first || next != int64(len(first)) || att != "1" {
		t.Fatalf("between attempts: %q next %d attempt %q", body, next, att)
	}
	again := n.claim(1)
	if len(again) != 1 || again[0].ID != tk.ID {
		t.Fatalf("attempt 2 not dispatched: %+v", again)
	}
	tk2 := again[0]

	// A follower at attempt 1's end offset is answered at once, not after
	// wait_s, with the new stream's (smaller) offset and the new attempt.
	body, next, att, took := read(int64(len(first)), 10)
	if body != "" || next != 0 || att != "2" || took > 5*time.Second {
		t.Fatalf("stale offset on attempt 2: %q next %d attempt %q after %v", body, next, att, took)
	}
	second := "attempt 2 output, which is longer than the first one\n"
	post(tk2, 0, second)
	// Reading attempt 2 at the old offset returns its tail labelled with
	// attempt 2, so a client can tell that it must start over.
	if body, next, att, _ := read(int64(len(first)), 0); body != second[len(first):] || next != int64(len(second)) || att != "2" {
		t.Fatalf("attempt 2 at old offset: %q next %d attempt %q", body, next, att)
	}
	if body, next, att, _ := read(0, 0); body != second || next != int64(len(second)) || att != "2" {
		t.Fatalf("attempt 2 from 0: %q next %d attempt %q", body, next, att)
	}
	// A current offset of a running attempt still long-polls.
	go func() {
		time.Sleep(100 * time.Millisecond)
		post(tk2, int64(len(second)), "more\n")
	}()
	if body, _, att, took := read(int64(len(second)), 10); body != "more\n" || att != "2" || took > 5*time.Second {
		t.Fatalf("long poll on attempt 2: %q attempt %q after %v", body, att, took)
	}
	n.succeed(tk2)
	h.s.io.flush()
	if body, next, att, _ := read(0, 5); body != second+"more\n" || next != int64(len(second)+5) || att != "2" {
		t.Fatalf("finished: %q next %d attempt %q", body, next, att)
	}
}

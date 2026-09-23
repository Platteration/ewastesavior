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

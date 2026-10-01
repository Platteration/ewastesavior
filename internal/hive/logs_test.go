package hive

import (
	"bytes"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/platteration/ewastesavior/internal/proto"
)

func TestLogRing(t *testing.T) {
	t.Parallel()
	l := newLogRing()
	l.append(0, []byte("hello "), logRingSize)
	l.append(3, []byte("lo world"), logRingSize) // overlapping retry
	l.append(0, []byte("hel"), logRingSize)      // fully known
	if d, next := l.read(0, 100); string(d) != "hello world" || next != 11 {
		t.Fatalf("read: %q %d", d, next)
	}
	if d, next := l.read(6, 3); string(d) != "wor" || next != 9 {
		t.Fatalf("partial: %q %d", d, next)
	}
	l.append(20, []byte("after gap"), logRingSize)
	if d, next := l.read(0, 100); string(d) != "after gap" || next != 29 {
		t.Fatalf("gap: %q %d", d, next)
	}
	// The ring keeps about the last 1 MiB.
	big := bytes.Repeat([]byte("x"), 700<<10)
	l.append(29, big, logRingSize)
	l.append(29+int64(len(big)), big, logRingSize)
	if cap(l.data) > logRingSize+logRingSize/4 || len(l.data) < logRingSize || l.total != 29+2*int64(len(big)) {
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
	if err != nil || fi.Size() != logTailSize || posixModes() && fi.Mode().Perm() != 0o600 {
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

// The log rings of all running tasks share one budget: a new ring lowers
// every ring's share, and once the rings hold more than the budget the
// ones above it are trimmed, keeping their latest bytes.
func TestLogRingsShareBudget(t *testing.T) {
	t.Parallel()
	const budget = 1 << 20
	h := newHive(t, func(c *Config) { c.tune.logBudget = budget })
	s := h.s
	s.mu.Lock()
	defer s.mu.Unlock()
	j := &job{jobRecord: jobRecord{ID: "jlogs", Spec: proto.JobSpec{Count: 64}}}
	chunk := bytes.Repeat([]byte("0123456789abcdef"), 16<<10) // 256 KiB
	var ts []*task
	for i := 0; i < 64; i++ {
		tk := &task{taskRecord: taskRecord{ID: "t" + strconv.Itoa(i), Index: i, State: proto.TaskRunning}, job: j}
		ts = append(ts, tk)
		for k := 0; k < 4; k++ {
			s.appendLogLocked(tk, int64(k*len(chunk)), chunk)
		}
		var sum int64
		for _, x := range ts {
			sum += int64(cap(x.log.data))
			if got := x.log.total; got != 4*int64(len(chunk)) {
				t.Fatalf("ring %s at offset %d", x.ID, got)
			}
			if d := x.log.data; len(d) < minLogRing || !bytes.HasSuffix(chunk, d[len(d)-minLogRing:]) {
				t.Fatalf("ring %s lost its latest bytes", x.ID)
			}
		}
		if sum != s.logMem {
			t.Fatalf("accounted %d bytes, rings hold %d", s.logMem, sum)
		}
		if limit := int64(len(ts)) * (minLogRing + minLogRing/4); sum > max(budget, limit) {
			t.Fatalf("%d rings hold %d bytes; budget %d", len(ts), sum, budget)
		}
	}
	for _, x := range ts {
		s.finishLogLocked(x)
	}
	if s.logMem != 0 || len(s.logRings) != 0 {
		t.Fatalf("after the tasks ended: %d bytes in %d rings", s.logMem, len(s.logRings))
	}
}

// A finished task's log tail is written by the io queue without the state
// mutex: under a lock convoy (load harness: seconds per acquisition) the
// queue waited for it once per tail, and 2000 tails piled up in memory and
// delayed the hive's shutdown by 32 s.
func TestLogTailWriteNeedsNoStateMutex(t *testing.T) {
	t.Parallel()
	h := newHive(t, nil)
	n := h.newNode(nil)
	n.register()
	h.submit(scriptJob(12, nil))
	gate := make(chan struct{})
	h.s.io.push(func() { <-gate }) // the tails pile up behind this
	opened := false
	defer func() {
		if !opened {
			close(gate)
		}
	}()
	var done []proto.Task
	for len(done) < 12 {
		tk := n.claim(1)[0]
		line := "output of " + tk.ID + "\n"
		if code, _ := n.api("POST", "tasks/"+tk.ID+"/log?lease="+tk.Lease+"&offset=0", line, nil); code != http.StatusOK {
			t.Fatalf("log: %d", code)
		}
		n.succeed(tk)
		done = append(done, tk)
	}
	h.s.mu.Lock() // busy for as long as the queue needs
	close(gate)
	opened = true
	flushed := make(chan struct{})
	go func() { h.s.io.flush(); close(flushed) }()
	select {
	case <-flushed:
		h.s.mu.Unlock()
	case <-time.After(5 * time.Second):
		h.s.mu.Unlock()
		t.Fatal("writing log tails waited for the state mutex")
	}
	for _, tk := range done {
		want := "output of " + tk.ID + "\n"
		if b, err := os.ReadFile(h.s.logPath(tk.ID)); err != nil || string(b) != want {
			t.Fatalf("tail file of %s: %q %v", tk.ID, b, err)
		}
		if h.s.pendingTail(tk.ID) != nil {
			t.Fatalf("tail of %s still pending after its write", tk.ID)
		}
		st, raw := do(t, h.hc, "GET", h.url+"/api/v1/admin/tasks/"+tk.ID+"/log", testAdmin, nil, nil)
		if st != http.StatusOK || string(raw) != want {
			t.Fatalf("log of %s: %d %q", tk.ID, st, raw)
		}
	}
}

// Close saves the state before it waits for the io queue: a backlog of
// log tails (load harness: 32 s of them) must not delay the final save
// past a service manager's patience.
func TestCloseSavesBeforeDrainingIO(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	h := startHive(t, dir, func(c *Config) { c.tune.minPersist = time.Hour })
	if err := h.s.persist(true); err != nil {
		t.Fatal(err)
	}
	n := h.newNode(nil)
	n.register() // a structural change, not saved yet
	gate := make(chan struct{})
	h.s.io.push(func() { <-gate })
	opened := false
	defer func() {
		if !opened {
			close(gate)
		}
	}()
	closed := make(chan struct{})
	go func() { h.stop(); close(closed) }()
	eventually(t, "the final save", func() bool {
		b, _ := os.ReadFile(filepath.Join(dir, stateFile))
		return bytes.Contains(b, []byte(n.req.NodeID))
	})
	select {
	case <-closed:
		t.Fatal("Close returned before the io queue drained")
	default:
	}
	close(gate)
	opened = true
	<-closed
}

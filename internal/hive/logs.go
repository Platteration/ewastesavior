package hive

import (
	"io"
	"net/http"
	"os"
	"strconv"
	"time"
)

// maxLogOffset bounds node-supplied offsets (1 PiB) so counters can't
// overflow.
const maxLogOffset = 1 << 50

// logRing keeps the most recent output of one assignment in memory. Offsets
// count every byte the node ever sent for the lease (DESIGN 8.5).
type logRing struct {
	data  []byte // stream bytes [base, total)
	base  int64
	total int64
	wake  chan struct{} // closed and replaced on every append
}

func newLogRing() *logRing { return &logRing{wake: make(chan struct{})} }

// append adds p, which starts at stream offset off. Bytes the ring already
// has are dropped, so retried uploads are idempotent. A gap (off beyond the
// end, e.g. after a hive restart) is skipped.
func (l *logRing) append(off int64, p []byte) {
	if off > l.total {
		l.data = l.data[:0]
		l.base, l.total = off, off
	}
	skip := l.total - off
	if skip >= int64(len(p)) {
		return
	}
	p = p[skip:]
	l.data = append(l.data, p...)
	l.total += int64(len(p))
	if len(l.data) > logRingSize+logRingSize/4 {
		keep := make([]byte, logRingSize, logRingSize+logRingSize/4)
		copy(keep, l.data[len(l.data)-logRingSize:])
		l.base += int64(len(l.data) - logRingSize)
		l.data = keep
	}
	close(l.wake)
	l.wake = make(chan struct{})
}

// read returns up to max bytes from off (clamped to what is retained) and
// the offset after them.
func (l *logRing) read(off int64, max int) ([]byte, int64) {
	if off < l.base {
		off = l.base
	}
	if off >= l.total {
		return nil, l.total
	}
	start := int(off - l.base)
	end := len(l.data)
	if end-start > max {
		end = start + max
	}
	return append([]byte(nil), l.data[start:end]...), l.base + int64(end)
}

// tailBytes returns a copy of the last n bytes and their stream offset.
func (l *logRing) tailBytes(n int) ([]byte, int64) {
	if len(l.data) < n {
		n = len(l.data)
	}
	return append([]byte(nil), l.data[len(l.data)-n:]...), l.total - int64(n)
}

// finishLogLocked moves an ended assignment's log into its tail file
// (last 64 KiB, <data>/logs/<task>.log), written outside the lock.
func (s *Server) finishLogLocked(t *task) {
	l := t.log
	if l == nil {
		return
	}
	t.log = nil
	close(l.wake) // wake long-polling readers; they fall through to the tail
	tail, base := l.tailBytes(logTailSize)
	t.LogBase, t.LogEnd = base, l.total
	if len(tail) == 0 {
		t.tail = nil
		return
	}
	t.tail = tail
	path, id := s.logPath(t.ID), t.ID
	s.io.push(func() {
		err := writeFileAtomic(path, tail, 0o600)
		if err != nil {
			s.log.Warn("could not write task log tail", "task", id, "err", err)
			return
		}
		s.mu.Lock()
		if cur := s.tasks[id]; cur != nil && len(cur.tail) > 0 && &cur.tail[0] == &tail[0] {
			cur.tail = nil
		}
		s.mu.Unlock()
	})
}

func (s *Server) handleLogAppend(w http.ResponseWriter, r *http.Request, tok string) {
	id := r.PathValue("id")
	q := r.URL.Query()
	lease := q.Get("lease")
	off, err := strconv.ParseInt(q.Get("offset"), 10, 64)
	if lease == "" || err != nil || off < 0 || off > maxLogOffset {
		writeErr(w, http.StatusBadRequest, "need lease and an offset in 0..%d", int64(maxLogOffset))
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxLogChunk))
	if err != nil {
		writeErr(w, http.StatusRequestEntityTooLarge, "log chunk larger than %d bytes", maxLogChunk)
		return
	}
	status, next := func() (int, int64) {
		s.mu.Lock()
		defer s.mu.Unlock()
		n := s.nodeByTokenLocked(tok)
		if n == nil {
			return http.StatusUnauthorized, 0
		}
		t := s.tasks[id]
		// Logs are accepted while the assignment is held, including after
		// a cancel request, until the node lets go of it.
		if t == nil || !t.active() || t.Node != n.ID || t.Lease != lease {
			return http.StatusConflict, 0
		}
		if t.log == nil {
			t.log = newLogRing()
		}
		t.log.append(off, body)
		return http.StatusOK, t.log.total
	}()
	switch status {
	case http.StatusUnauthorized:
		writeErr(w, status, "unknown node token; register again")
	case http.StatusConflict:
		writeErr(w, status, "stale lease")
	default:
		writeJSON(w, http.StatusOK, map[string]int64{"next": next})
	}
}

// handleTaskLog serves GET tasks/{id}/log?offset=N&wait_s=W.
func (s *Server) handleTaskLog(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	id := r.PathValue("id")
	q := r.URL.Query()
	var off int64
	if v := q.Get("offset"); v != "" {
		var err error
		if off, err = strconv.ParseInt(v, 10, 64); err != nil || off < 0 {
			writeErr(w, http.StatusBadRequest, "invalid offset")
			return
		}
	}
	wait := time.Duration(0)
	if v := q.Get("wait_s"); v != "" {
		sec, err := strconv.Atoi(v)
		if err != nil || sec < 0 {
			writeErr(w, http.StatusBadRequest, "invalid wait_s")
			return
		}
		wait = time.Duration(sec) * time.Second
		if wait > maxLogWait {
			wait = maxLogWait
		}
	}
	setDeadlines(w, wait+15*time.Second)
	deadline := time.Now().Add(wait)
	ctx := r.Context()
	for {
		s.mu.Lock()
		t := s.tasks[id]
		if t == nil {
			s.mu.Unlock()
			writeErr(w, http.StatusNotFound, "no such task")
			return
		}
		if t.active() {
			if t.log == nil {
				t.log = newLogRing()
			}
			data, next := t.log.read(off, logRingSize)
			remaining := time.Until(deadline)
			if len(data) > 0 || remaining <= 0 {
				s.mu.Unlock()
				writeLog(w, data, next)
				return
			}
			wake := t.log.wake
			s.mu.Unlock()
			timer := time.NewTimer(remaining)
			select {
			case <-wake:
			case <-timer.C:
			case <-s.shutdown:
				// The hive is stopping: answer with what there is now so
				// the HTTP server's shutdown doesn't wait for the poll.
				deadline = time.Now()
			case <-ctx.Done():
				timer.Stop()
				return
			}
			timer.Stop()
			continue
		}
		base, end, tail := t.LogBase, t.LogEnd, t.tail
		path := s.logPath(t.ID)
		s.mu.Unlock()
		if off < base {
			off = base
		}
		if off >= end {
			writeLog(w, nil, end)
			return
		}
		if tail != nil {
			writeLog(w, tail[off-base:], end)
			return
		}
		f, err := os.Open(path)
		if err != nil {
			writeLog(w, nil, end)
			return
		}
		defer f.Close()
		buf := make([]byte, end-off)
		k, _ := f.ReadAt(buf, off-base)
		writeLog(w, buf[:k], off+int64(k))
		return
	}
}

func writeLog(w http.ResponseWriter, data []byte, next int64) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set(logOffsetHd, strconv.FormatInt(next, 10))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

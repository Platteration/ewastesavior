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

// logAttemptHd names the attempt whose log a GET tasks/{id}/log response
// holds. Every dispatch starts a new stream at offset 0 (DESIGN 8.5), so a
// follower that sees the attempt change must start over from offset 0.
const logAttemptHd = "X-Savior-Log-Attempt"

// logRing keeps the most recent output of one assignment in memory: at
// least the last limit bytes the hive gives it (logLimitLocked), in a
// buffer of at most limit + limit/4. Offsets count every byte the node ever
// sent for the lease (DESIGN 8.5).
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
func (l *logRing) append(off int64, p []byte, limit int) {
	if off > l.total {
		l.data = l.data[:0]
		l.base, l.total = off, off
	}
	skip := l.total - off
	if skip >= int64(len(p)) {
		return
	}
	p = p[skip:]
	l.total += int64(len(p))
	if len(p) > limit {
		p = p[len(p)-limit:]
		l.data = l.data[:0]
	}
	l.fit(limit, len(p))
	l.data = append(l.data, p...)
	l.base = l.total - int64(len(l.data))
	close(l.wake)
	l.wake = make(chan struct{})
}

// fit makes room for n more bytes (n <= limit) in a buffer of at most
// limit + limit/4 bytes, dropping the oldest bytes (keeping limit - n)
// when they would not fit. It allocates only to grow or shrink the buffer,
// never on every trim.
func (l *logRing) fit(limit, n int) {
	most := limit + limit/4
	if len(l.data)+n > most {
		keep := limit - n
		copy(l.data, l.data[len(l.data)-keep:])
		l.data = l.data[:keep]
	}
	need := len(l.data) + n
	if need <= cap(l.data) && cap(l.data) <= most {
		return
	}
	nd := make([]byte, len(l.data), min(max(2*cap(l.data), need, 4<<10), most))
	copy(nd, l.data)
	l.data = nd
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

// The log rings of all running tasks share one memory budget
// (tune.logBudget, DESIGN 8.5): each keeps an equal share, between
// minLogRing and logRingSize.

// logLimitLocked is how many bytes each ring keeps now.
func (s *Server) logLimitLocked() int {
	share := s.cfg.tune.logBudget / int64(max(len(s.logRings), 1)) * 4 / 5 // buffers hold up to 1.25x
	return int(min(max(share, minLogRing), logRingSize))
}

// ringLocked returns t's log ring, creating it.
func (s *Server) ringLocked(t *task) *logRing {
	if t.log == nil {
		t.log = newLogRing()
		s.logRings[t] = struct{}{}
	}
	return t.log
}

// appendLogLocked adds a chunk to t's ring within the budget. A new ring
// lowers every ring's share; once the rings hold more than they may, the
// ones above the share are trimmed to it, keeping their latest bytes.
func (s *Server) appendLogLocked(t *task, off int64, p []byte) *logRing {
	l := s.ringLocked(t)
	limit := s.logLimitLocked()
	most := limit + limit/4
	before := cap(l.data)
	l.append(off, p, limit)
	s.logMem += int64(cap(l.data) - before)
	if s.logMem > max(s.cfg.tune.logBudget, int64(len(s.logRings))*int64(most)) {
		for x := range s.logRings {
			if r := x.log; cap(r.data) > most {
				before := cap(r.data)
				r.fit(limit, 0)
				r.base = r.total - int64(len(r.data))
				s.logMem += int64(cap(r.data) - before)
			}
		}
	}
	return l
}

// dropLogLocked detaches t's ring (its assignment ended) and returns it.
func (s *Server) dropLogLocked(t *task) *logRing {
	l := t.log
	if l != nil {
		t.log = nil
		delete(s.logRings, t)
		s.logMem -= int64(cap(l.data))
	}
	return l
}

// finishLogLocked moves an ended assignment's log into its tail file
// (last 64 KiB, <data>/logs/<task>.log). The io queue writes it; until
// then readers get it from s.tails. Neither the write nor clearing the
// pending tail takes the state mutex, and the file is not fsynced (logs
// are best effort): a tail costs the io queue one small write, so it keeps
// up with hundreds of task completions a second.
func (s *Server) finishLogLocked(t *task) {
	l := s.dropLogLocked(t)
	if l == nil {
		return
	}
	close(l.wake) // wake long-polling readers; they fall through to the tail
	tail, base := l.tailBytes(logTailSize)
	t.LogBase, t.LogEnd = base, l.total
	id := t.ID
	s.tailMu.Lock()
	if len(tail) == 0 {
		delete(s.tails, id)
	} else {
		s.tails[id] = tail
	}
	s.tailMu.Unlock()
	if len(tail) == 0 {
		return
	}
	path := s.logPath(id)
	s.io.push(func() {
		if err := writeFile(path, tail, 0o600, false); err != nil {
			s.log.Warn("could not write task log tail", "task", id, "err", err)
			return
		}
		s.tailMu.Lock()
		if cur := s.tails[id]; len(cur) > 0 && &cur[0] == &tail[0] {
			delete(s.tails, id)
		}
		s.tailMu.Unlock()
	})
}

// pendingTail returns the log tail of a task that is not in its file yet.
func (s *Server) pendingTail(id string) []byte {
	s.tailMu.Lock()
	defer s.tailMu.Unlock()
	return s.tails[id]
}

// dropTail forgets a pending tail (the task is dispatched again or
// deleted); a queued write of it still happens.
func (s *Server) dropTail(id string) bool {
	s.tailMu.Lock()
	defer s.tailMu.Unlock()
	_, ok := s.tails[id]
	delete(s.tails, id)
	return ok
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
	if r.ContentLength > maxLogChunk {
		writeErr(w, http.StatusRequestEntityTooLarge, "log chunk larger than %d bytes", maxLogChunk)
		return
	}
	var body []byte
	if r.ContentLength >= 0 {
		// One allocation of the chunk's size (io.ReadAll grows from 512
		// bytes by doubling).
		body = make([]byte, r.ContentLength)
		_, err = io.ReadFull(r.Body, body)
	} else {
		body, err = io.ReadAll(http.MaxBytesReader(w, r.Body, maxLogChunk))
	}
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
		return http.StatusOK, s.appendLogLocked(t, off, body).total
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

// handleTaskLog serves GET tasks/{id}/log?offset=N&wait_s=W. Responses
// carry the next offset and the attempt the bytes belong to. An offset past
// the end of the current attempt's stream (a follower still on an earlier
// attempt) is answered at once, with the smaller next offset, instead of
// waiting until the new stream grows past it.
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
		attempt := t.Attempt
		if t.active() {
			l := s.ringLocked(t)
			data, next := l.read(off, logRingSize)
			remaining := time.Until(deadline)
			if len(data) > 0 || remaining <= 0 || off > l.total {
				s.mu.Unlock()
				writeLog(w, data, next, attempt)
				return
			}
			wake := l.wake
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
		base, end, tail := t.LogBase, t.LogEnd, s.pendingTail(t.ID)
		path := s.logPath(t.ID)
		s.mu.Unlock()
		if off < base {
			off = base
		}
		if off >= end {
			writeLog(w, nil, end, attempt)
			return
		}
		if tail != nil {
			writeLog(w, tail[off-base:], end, attempt)
			return
		}
		f, err := os.Open(path)
		if err != nil {
			writeLog(w, nil, end, attempt)
			return
		}
		defer f.Close()
		buf := make([]byte, end-off)
		k, _ := f.ReadAt(buf, off-base)
		writeLog(w, buf[:k], off+int64(k), attempt)
		return
	}
}

func writeLog(w http.ResponseWriter, data []byte, next int64, attempt int) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set(logOffsetHd, strconv.FormatInt(next, 10))
	w.Header().Set(logAttemptHd, strconv.Itoa(attempt))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

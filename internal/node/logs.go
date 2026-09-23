package node

import (
	"context"
	"sync"
	"time"
)

const (
	logChunkMax  = 256 << 10 // hive limit per POST
	logBufferMax = 1 << 20   // unsent bytes kept while the hive is unreachable
	logInterval  = 2 * time.Second
)

// logStream buffers a task's combined output and ships it to the hive in
// offset-addressed chunks, so retries never duplicate text.
type logStream struct {
	a      *Agent
	taskID string
	lease  string

	mu       sync.Mutex
	buf      []byte // unsent bytes
	bufStart int64  // absolute offset of buf[0]
	closed   bool
	stale    bool // hive rejected the lease: stop shipping
	sendMu   sync.Mutex
}

func newLogStream(a *Agent, taskID, lease string) *logStream {
	return &logStream{a: a, taskID: taskID, lease: lease}
}

// Write implements io.Writer for the runner.
func (l *logStream) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stale {
		return len(p), nil
	}
	l.buf = append(l.buf, p...)
	if over := len(l.buf) - logBufferMax; over > 0 {
		// Keep the newest output; the hive sees the gap from the offsets.
		l.buf = append(l.buf[:0:0], l.buf[over:]...)
		l.bufStart += int64(over)
	}
	return len(p), nil
}

func (l *logStream) close() {
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()
}

// run ships output every logInterval until closed and drained.
func (l *logStream) run(ctx context.Context) {
	t := time.NewTicker(logInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		l.flush(ctx)
		l.mu.Lock()
		done := l.closed && (len(l.buf) == 0 || l.stale)
		l.mu.Unlock()
		if done {
			return
		}
	}
}

// flush sends everything buffered (best effort).
func (l *logStream) flush(ctx context.Context) {
	l.sendMu.Lock()
	defer l.sendMu.Unlock()
	for i := 0; i < 16; i++ {
		l.mu.Lock()
		if l.stale || len(l.buf) == 0 {
			l.mu.Unlock()
			return
		}
		n := len(l.buf)
		if n > logChunkMax {
			n = logChunkMax
		}
		chunk := append([]byte(nil), l.buf[:n]...)
		start := l.bufStart
		l.mu.Unlock()

		l.a.mu.Lock()
		hc := l.a.hc
		l.a.mu.Unlock()
		if hc == nil {
			return
		}
		next, err := hc.postLog(ctx, l.taskID, l.lease, start, chunk)
		if err != nil {
			if statusOf(err) == 409 || statusOf(err) == 404 {
				l.mu.Lock()
				l.stale = true
				l.buf = nil
				l.mu.Unlock()
			}
			return
		}
		l.mu.Lock()
		if next <= l.bufStart {
			next = start + int64(n)
		}
		drop := next - l.bufStart
		if drop >= int64(len(l.buf)) {
			l.buf = l.buf[:0]
			l.bufStart = next
		} else if drop > 0 {
			l.buf = append(l.buf[:0:0], l.buf[drop:]...)
			l.bufStart = next
		}
		l.mu.Unlock()
	}
}

// Package logging sets up slog for long-running savior commands: text to
// stderr, optionally mirrored to a size-rotated file (1 MiB + one backup),
// so a node's logs survive agent restarts without filling RAM-backed /var.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime/debug"
	"strings"
	"sync"
)

// MaxFileSize is the size at which the log file is rotated to <file>.1.
const MaxFileSize = 1 << 20

// ParseLevel maps debug/info/warn/error to a slog level (default info).
func ParseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	return slog.LevelInfo
}

// Setup returns a logger writing to stderr and, when file != "", to a
// rotating file. Close the returned closer on exit.
func Setup(level, file string) (*slog.Logger, io.Closer, error) {
	var w io.Writer = os.Stderr
	var closer io.Closer = nopCloser{}
	if file != "" {
		rf, err := OpenRotating(file, MaxFileSize)
		if err != nil {
			return nil, nil, err
		}
		w = io.MultiWriter(os.Stderr, rf)
		closer = rf
		// Keep Go panics and fatal errors: stderr may go to /dev/null.
		if cf, err := os.OpenFile(file+".crash", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
			debug.SetCrashOutput(cf, debug.CrashOptions{})
			cf.Close()
		}
	}
	h := slog.NewTextHandler(w, &slog.HandlerOptions{Level: ParseLevel(level)})
	return slog.New(h), closer, nil
}

type nopCloser struct{}

func (nopCloser) Close() error { return nil }

// RotatingFile is an io.Writer that renames path to path.1 when it grows
// beyond max bytes. Files are created with mode 0600.
type RotatingFile struct {
	mu   sync.Mutex
	path string
	max  int64
	f    *os.File
	size int64
}

// OpenRotating opens (appending) or creates path.
func OpenRotating(path string, max int64) (*RotatingFile, error) {
	r := &RotatingFile{path: path, max: max}
	if err := r.open(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *RotatingFile) open() error {
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	r.f, r.size = f, st.Size()
	return nil
}

// Write implements io.Writer.
func (r *RotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return 0, os.ErrClosed
	}
	if r.size+int64(len(p)) > r.max && r.size > 0 {
		r.f.Close()
		os.Rename(r.path, r.path+".1")
		if err := r.open(); err != nil {
			r.f = nil
			return 0, err
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

// Close closes the file.
func (r *RotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	return err
}

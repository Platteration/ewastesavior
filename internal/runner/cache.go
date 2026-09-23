package runner

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/platteration/ewastesavior/internal/proto"
)

// errTooLarge reports a download that exceeded its declared or allowed size.
var errTooLarge = errors.New("download exceeds the allowed size")

// limitWriter fails once more than n bytes have been written.
type limitWriter struct {
	w io.Writer
	n int64
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > l.n {
		return 0, errTooLarge
	}
	n, err := l.w.Write(p)
	l.n -= int64(n)
	return n, err
}

// countWriter reports every write to fn (progress accounting).
type countWriter struct {
	w  io.Writer
	fn func(int64)
}

func (c countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	if n > 0 && c.fn != nil {
		c.fn(int64(n))
	}
	return n, err
}

// fetchFunc streams a blob into w.
type fetchFunc func(ctx context.Context, w io.Writer) (int64, error)

// blobCache keeps verified blobs in a root-only directory, named by hash,
// bounded in total size (least recently used first out).
type blobCache struct {
	dir string
	max int64
	log *slog.Logger

	mu       sync.Mutex
	inflight map[string]chan struct{}
}

func newBlobCache(dir string, maxBytes int64, log *slog.Logger) *blobCache {
	return &blobCache{dir: dir, max: maxBytes, log: log, inflight: map[string]chan struct{}{}}
}

// cacheable reports whether a blob of up to limit bytes should be cached.
func (c *blobCache) cacheable(limit int64) bool { return limit > 0 && limit <= c.max/2 }

func (c *blobCache) path(sha string) string { return filepath.Join(c.dir, sha) }

// ensure makes sure sha is in the cache, fetching it at most once even when
// several tasks ask concurrently. The download is limited to limit bytes
// and verified before it becomes visible.
func (c *blobCache) ensure(ctx context.Context, sha string, limit int64, fetch fetchFunc) error {
	if !proto.ValidSHA256(sha) {
		return fmt.Errorf("invalid hash %q", sha)
	}
	for {
		c.mu.Lock()
		if _, err := os.Lstat(c.path(sha)); err == nil {
			c.mu.Unlock()
			now := time.Now()
			os.Chtimes(c.path(sha), now, now)
			return nil
		}
		wait, busy := c.inflight[sha]
		if !busy {
			wait = make(chan struct{})
			c.inflight[sha] = wait
			c.mu.Unlock()
			err := c.download(ctx, sha, limit, fetch)
			c.mu.Lock()
			delete(c.inflight, sha)
			close(wait)
			c.mu.Unlock()
			if err == nil {
				c.evict(sha)
			}
			return err
		}
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-wait:
		}
		// Retry: the other fetch may have failed (then we try ourselves).
	}
}

func (c *blobCache) download(ctx context.Context, sha string, limit int64, fetch fetchFunc) error {
	tmp := filepath.Join(c.dir, "tmp-"+randomHex(8))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("cache: %w", err)
	}
	ok := false
	defer func() {
		f.Close()
		if !ok {
			os.Remove(tmp)
		}
	}()
	h := sha256.New()
	if _, err := fetch(ctx, &limitWriter{w: io.MultiWriter(f, h), n: limit}); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != sha {
		return fmt.Errorf("content hash mismatch: got %s, want %s", got, sha)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("cache: %w", err)
	}
	if err := os.Rename(tmp, c.path(sha)); err != nil {
		return fmt.Errorf("cache: %w", err)
	}
	ok = true
	return nil
}

// open opens a cached blob for reading.
func (c *blobCache) open(sha string) (*os.File, error) {
	return os.OpenFile(c.path(sha), os.O_RDONLY|oNoFollow, 0)
}

// drop removes a cache entry (e.g. after a failed verification).
func (c *blobCache) drop(sha string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	os.Remove(c.path(sha))
}

// evict removes least recently used blobs until the cache fits its bound.
// keep is never evicted (it was just added for a running task).
func (c *blobCache) evict(keep string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ents, err := os.ReadDir(c.dir)
	if err != nil {
		return
	}
	type item struct {
		name string
		size int64
		mod  time.Time
	}
	var items []item
	var total int64
	for _, e := range ents {
		if !e.Type().IsRegular() || !proto.ValidSHA256(e.Name()) {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		items = append(items, item{e.Name(), fi.Size(), fi.ModTime()})
		total += fi.Size()
	}
	sort.Slice(items, func(i, j int) bool { return items[i].mod.Before(items[j].mod) })
	for _, it := range items {
		if total <= c.max {
			break
		}
		if it.name == keep {
			continue
		}
		if err := os.Remove(filepath.Join(c.dir, it.name)); err == nil {
			total -= it.size
			c.log.Debug("evicted cached blob", "sha256", it.name, "size", it.size)
		}
	}
}

// clean removes leftover temporary downloads.
func (c *blobCache) clean() {
	ents, err := os.ReadDir(c.dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		if !proto.ValidSHA256(e.Name()) {
			os.RemoveAll(filepath.Join(c.dir, e.Name()))
		}
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

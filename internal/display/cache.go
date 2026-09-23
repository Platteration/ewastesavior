package display

import (
	"bufio"
	"bytes"
	"container/list"
	"image"
	"os"
	"strconv"
	"sync"
)

// FrameCache is an LRU cache of decoded, scaled media frames within a
// byte budget. Cached images are shared and must be treated as read-only.
// It is safe for concurrent use.
type FrameCache struct {
	mu     sync.Mutex
	budget int64
	used   int64
	items  map[string]*list.Element
	order  list.List // front = most recently used
}

type frameEntry struct {
	key  string
	img  image.Image
	size int64
}

// NewFrameCache returns a cache holding at most budget bytes of pixels.
func NewFrameCache(budget int64) *FrameCache {
	return &FrameCache{budget: max(budget, 0), items: map[string]*list.Element{}}
}

// DefaultCacheBytes is DESIGN 11.5's frame cache size: min(64 MiB, 10% of
// MemTotal), or 64 MiB when MemTotal is unknown.
func DefaultCacheBytes() int64 {
	const limit = 64 << 20
	if t := memTotalBytes(); t > 0 {
		return min(limit, t/10)
	}
	return limit
}

// imageBytes estimates the memory held by an image's pixels.
func imageBytes(img image.Image) int64 {
	switch m := img.(type) {
	case *image.RGBA:
		return int64(len(m.Pix))
	case *image.NRGBA:
		return int64(len(m.Pix))
	case *image.YCbCr:
		return int64(len(m.Y) + len(m.Cb) + len(m.Cr))
	case *image.Gray:
		return int64(len(m.Pix))
	case *image.Paletted:
		return int64(len(m.Pix) + 4*len(m.Palette))
	}
	// Unknown types: assume 4 bytes per pixel, with the dimensions clamped
	// so unbounded images (image.Uniform) can't overflow.
	b := img.Bounds()
	return min(int64(b.Dx()), 1<<20) * min(int64(b.Dy()), 1<<20) * 4
}

// Get returns the cached image for key, or nil.
func (c *FrameCache) Get(key string) image.Image {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[key]; ok {
		c.order.MoveToFront(e)
		return e.Value.(*frameEntry).img
	}
	return nil
}

// Add stores img, evicting least recently used frames as needed. Images
// larger than the whole budget are not cached.
func (c *FrameCache) Add(key string, img image.Image) {
	c.add(key, img, true)
}

// AddIfRoom stores img only if that needs no eviction (used for
// background prefetching, which must not push out frames needed soon).
func (c *FrameCache) AddIfRoom(key string, img image.Image) bool {
	return c.add(key, img, false)
}

func (c *FrameCache) add(key string, img image.Image, evict bool) bool {
	if c == nil || img == nil {
		return false
	}
	n := imageBytes(img)
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[key]; ok {
		c.removeLocked(e)
	}
	if n > c.budget || (!evict && c.used+n > c.budget) {
		return false
	}
	for c.used+n > c.budget && c.order.Len() > 0 {
		c.removeLocked(c.order.Back())
	}
	c.items[key] = c.order.PushFront(&frameEntry{key: key, img: img, size: n})
	c.used += n
	return true
}

func (c *FrameCache) removeLocked(e *list.Element) {
	fe := e.Value.(*frameEntry)
	c.order.Remove(e)
	delete(c.items, fe.key)
	c.used -= fe.size
}

// Bytes returns the bytes currently cached.
func (c *FrameCache) Bytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.used
}

// Len returns the number of cached frames.
func (c *FrameCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}

// ensureBudget raises the budget to at least n bytes.
func (c *FrameCache) ensureBudget(n int64) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.budget = max(c.budget, n)
	c.mu.Unlock()
}

// Budget returns the byte budget.
func (c *FrameCache) Budget() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.budget
}

// meminfoPath is read for MemTotal and MemAvailable (a variable for tests).
var meminfoPath = "/proc/meminfo"

// readMeminfo returns MemTotal and MemAvailable in bytes (0 = unknown).
func readMeminfo() (total, avail int64) {
	b, err := os.ReadFile(meminfoPath)
	if err != nil {
		return 0, 0
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := bytes.Fields(sc.Bytes())
		if len(f) < 2 {
			continue
		}
		v, err := strconv.ParseInt(string(f[1]), 10, 64)
		if err != nil {
			continue
		}
		switch string(f[0]) {
		case "MemTotal:":
			total = v << 10
		case "MemAvailable:":
			avail = v << 10
		}
	}
	return total, avail
}

func memTotalBytes() int64 {
	t, _ := readMeminfo()
	return t
}

func memAvailableBytes() int64 {
	_, a := readMeminfo()
	return a
}

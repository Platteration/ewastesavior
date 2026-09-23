package display

import (
	"context"
	"errors"
	"fmt"
	"image"
	"sort"
	"sync"
	"time"

	"github.com/platteration/ewastesavior/internal/proto"
)

const (
	fetchTimeout    = 90 * time.Second
	retryFirst      = 30 * time.Second
	retryMax        = 5 * time.Minute
	maxQueuedLoads  = 256
	statsFetchLimit = 10 * time.Second
)

// mediaStatus is the load state of one media item (by Media.Key).
type mediaStatus struct {
	ok      bool
	err     error
	fails   int
	retryAt time.Time
}

type loadJob struct {
	m   proto.Media
	req FetchRequest
	key string // cache key
	bg  bool   // prefetch: validate, cache only if there is room
	gen uint64
}

// mediaLoader fetches media in the background, one item at a time (a
// 256 MB machine can't decode two big images at once), so rendering never
// blocks on the network. Scenes call get, which answers from the cache or
// returns errPending and queues the load; when a load finishes, notify
// wakes the controller to render again.
type mediaLoader struct {
	fetch  func(ctx context.Context, m proto.Media, req FetchRequest) (image.Image, error)
	cache  *FrameCache
	notify func()

	mu       sync.Mutex
	status   map[string]*mediaStatus
	urgent   []loadJob
	bg       []loadJob
	inflight map[string]bool
	gen      uint64
	kick     chan struct{}
	// last holds the most recent on-demand result even if the cache could
	// not take it, so a frame larger than the cache can't cause a reload
	// loop.
	lastKey string
	lastImg image.Image
}

func newMediaLoader(fetch func(context.Context, proto.Media, FetchRequest) (image.Image, error), cache *FrameCache, notify func()) *mediaLoader {
	return &mediaLoader{
		fetch: fetch, cache: cache, notify: notify,
		status: map[string]*mediaStatus{}, inflight: map[string]bool{},
		kick: make(chan struct{}, 1),
	}
}

func cacheKey(m proto.Media, req FetchRequest) string { return m.Key() + "|" + req.key() }

// get is the Env.Fetch the controller gives to scenes.
func (l *mediaLoader) get(_ context.Context, m proto.Media, req FetchRequest) (image.Image, error) {
	if err := req.valid(); err != nil {
		return nil, err
	}
	key := cacheKey(m, req)
	if img := l.cache.Get(key); img != nil {
		return img, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if key == l.lastKey && l.lastImg != nil {
		return l.lastImg, nil
	}
	st := l.status[m.Key()]
	if st != nil && st.err != nil && time.Now().Before(st.retryAt) {
		return nil, st.err
	}
	l.enqueueLocked(loadJob{m: m, req: req, key: key, gen: l.gen}, true)
	if st != nil && st.err != nil {
		return nil, st.err // keep showing the error while retrying
	}
	return nil, errPending
}

func (l *mediaLoader) enqueueLocked(j loadJob, urgent bool) {
	if l.inflight[j.key] {
		return
	}
	for _, q := range [][]loadJob{l.urgent, l.bg} {
		for _, x := range q {
			if x.key == j.key {
				return
			}
		}
	}
	if urgent {
		if len(l.urgent) >= maxQueuedLoads {
			l.urgent = l.urgent[1:]
		}
		l.urgent = append(l.urgent, j)
	} else if len(l.bg) < maxQueuedLoads {
		l.bg = append(l.bg, j)
	}
	select {
	case l.kick <- struct{}{}:
	default:
	}
}

// setSpec starts a new generation for spec's media: status for media not
// in the spec is forgotten, and every item is queued for a background
// load (in display order) so errors and Loaded/Total show up early.
func (l *mediaLoader) setSpec(items []proto.Media, req FetchRequest) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.gen++
	keep := map[string]bool{}
	for _, m := range items {
		keep[m.Key()] = true
	}
	for k := range l.status {
		if !keep[k] {
			delete(l.status, k)
		}
	}
	l.urgent, l.bg = nil, nil
	if req.valid() != nil {
		return
	}
	for _, m := range items {
		key := cacheKey(m, req)
		if st := l.status[m.Key()]; st != nil && st.ok && l.cache.Get(key) != nil {
			continue
		}
		l.enqueueLocked(loadJob{m: m, req: req, key: key, bg: true, gen: l.gen}, false)
	}
}

// progress reports how many of items loaded and the current errors.
func (l *mediaLoader) progress(items []proto.Media) (loaded, total int, errs []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	seen := map[string]bool{}
	for _, m := range items {
		k := m.Key()
		if seen[k] {
			continue
		}
		seen[k] = true
		total++
		st := l.status[k]
		switch {
		case st == nil:
		case st.ok:
			loaded++
		case st.err != nil:
			errs = append(errs, proto.Sanitize(st.err.Error(), 300, false))
		}
	}
	sort.Strings(errs)
	return loaded, total, errs
}

func (l *mediaLoader) next() (loadJob, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for len(l.urgent) > 0 {
		j := l.urgent[0]
		l.urgent = l.urgent[1:]
		if !l.inflight[j.key] {
			l.inflight[j.key] = true
			return j, true
		}
	}
	for len(l.bg) > 0 {
		j := l.bg[0]
		l.bg = l.bg[1:]
		if j.gen != l.gen || l.inflight[j.key] {
			continue
		}
		if st := l.status[j.m.Key()]; st != nil && (st.ok || time.Now().Before(st.retryAt)) {
			continue // already validated, or failed recently
		}
		l.inflight[j.key] = true
		return j, true
	}
	return loadJob{}, false
}

// run processes the queues until ctx ends.
func (l *mediaLoader) run(ctx context.Context) {
	for {
		j, ok := l.next()
		if !ok {
			select {
			case <-ctx.Done():
				return
			case <-l.kick:
			}
			continue
		}
		l.safeLoad(ctx, j)
	}
}

// safeLoad runs one job; a panic is recorded as that item's error.
func (l *mediaLoader) safeLoad(ctx context.Context, j loadJob) {
	defer func() {
		if p := recover(); p != nil {
			l.mu.Lock()
			delete(l.inflight, j.key)
			l.status[j.m.Key()] = &mediaStatus{err: &MediaError{Media: mediaName(j.m), Err: fmt.Errorf("internal error: %v", p)},
				fails: 1, retryAt: time.Now().Add(retryMax)}
			l.mu.Unlock()
		}
	}()
	l.load(ctx, j)
}

func (l *mediaLoader) load(ctx context.Context, j loadJob) {
	var img image.Image
	var err error
	if l.fetch == nil {
		err = errors.New("no media source configured")
	} else {
		fctx, cancel := context.WithTimeout(ctx, fetchTimeout)
		img, err = safeFetch(fctx, l.fetch, j.m, j.req)
		cancel()
	}
	if err == nil && img == nil {
		err = errors.New("fetch returned no image")
	}
	if ctx.Err() != nil {
		return
	}
	if err == nil {
		b := img.Bounds()
		if b.Dx() != j.req.PixelW || b.Dy() != j.req.PixelH {
			err = fmt.Errorf("fetch returned %dx%d, want %dx%d", b.Dx(), b.Dy(), j.req.PixelW, j.req.PixelH)
		}
	}
	l.mu.Lock()
	delete(l.inflight, j.key)
	st := l.status[j.m.Key()]
	if st == nil {
		st = &mediaStatus{}
		l.status[j.m.Key()] = st
	}
	if err != nil {
		var me *MediaError
		if !errors.As(err, &me) {
			err = &MediaError{Media: mediaName(j.m), Err: err}
		}
		st.ok, st.err = false, err
		st.fails++
		backoff := retryFirst << min(st.fails-1, 4)
		st.retryAt = time.Now().Add(min(backoff, retryMax))
	} else {
		st.ok, st.err, st.fails = true, nil, 0
		if !j.bg {
			l.lastKey, l.lastImg = j.key, img
		}
	}
	l.mu.Unlock()
	if err == nil {
		if j.bg {
			l.cache.AddIfRoom(j.key, img)
		} else {
			l.cache.Add(j.key, img)
		}
	}
	if l.notify != nil {
		l.notify()
	}
}

// safeFetch turns a panicking fetcher (a decoder bug on hostile input)
// into an error.
func safeFetch(ctx context.Context, fetch func(context.Context, proto.Media, FetchRequest) (image.Image, error), m proto.Media, req FetchRequest) (img image.Image, err error) {
	defer func() {
		if r := recover(); r != nil {
			img, err = nil, fmt.Errorf("fetch panic: %v", r)
		}
	}()
	return fetch(ctx, m, req)
}

// asyncStats serves the dashboard from the last fetched SwarmStats and
// refreshes them in the background every dashboardRefresh.
type asyncStats struct {
	fetch  func(ctx context.Context) (*proto.SwarmStats, error)
	notify func()
	ctx    context.Context

	mu    sync.Mutex
	last  *proto.SwarmStats
	err   error
	at    time.Time
	busy  bool
	tried bool
}

func (a *asyncStats) get(_ context.Context) (*proto.SwarmStats, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.busy && (!a.tried || time.Since(a.at) >= dashboardRefresh-100*time.Millisecond) {
		a.busy, a.tried = true, true
		go a.refresh()
	}
	if a.last == nil && a.err == nil {
		return nil, errPending
	}
	return a.last, a.err
}

func (a *asyncStats) refresh() {
	ctx, cancel := context.WithTimeout(a.ctx, statsFetchLimit)
	defer cancel()
	var st *proto.SwarmStats
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("stats panic: %v", r)
			}
		}()
		st, err = a.fetch(ctx)
	}()
	a.mu.Lock()
	a.busy = false
	a.at = time.Now()
	if err == nil && st != nil {
		a.last, a.err = st, nil
	} else {
		if err == nil {
			err = errors.New("no statistics")
		}
		a.err = err
	}
	a.mu.Unlock()
	if a.ctx.Err() == nil && a.notify != nil {
		a.notify()
	}
}

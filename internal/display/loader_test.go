package display

import (
	"context"
	"errors"
	"image"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/platteration/ewastesavior/internal/proto"
)

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestMediaLoader(t *testing.T) {
	var calls atomic.Int32
	var notified atomic.Int32
	fetch := func(_ context.Context, m proto.Media, req FetchRequest) (image.Image, error) {
		calls.Add(1)
		switch {
		case strings.HasSuffix(m.URL, "bad"):
			return nil, errors.New("HTTP 404")
		case strings.HasSuffix(m.URL, "panic"):
			panic("decoder bug")
		case strings.HasSuffix(m.URL, "wrongsize"):
			return solid(3, 3, colWhite), nil
		}
		return solid(req.PixelW, req.PixelH, colWhite), nil
	}
	cache := NewFrameCache(1 << 20)
	l := newMediaLoader(fetch, cache, func() { notified.Add(1) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go l.run(ctx)
	req := screenReq(8, 8)
	good := proto.Media{URL: "http://x/good"}
	if _, err := l.get(ctx, good, req); !errors.Is(err, errPending) {
		t.Fatalf("first get = %v, want pending", err)
	}
	waitFor(t, "load", func() bool { img, _ := l.get(ctx, good, req); return img != nil })
	if calls.Load() != 1 || notified.Load() == 0 {
		t.Fatalf("calls %d notified %d", calls.Load(), notified.Load())
	}
	for _, name := range []string{"bad", "panic", "wrongsize"} {
		m := proto.Media{URL: "http://x/" + name}
		_, _ = l.get(ctx, m, req)
		waitFor(t, name+" error", func() bool {
			_, err := l.get(ctx, m, req)
			return err != nil && !errors.Is(err, errPending)
		})
		_, err := l.get(ctx, m, req)
		var me *MediaError
		if !errors.As(err, &me) || !strings.Contains(me.Media, name) {
			t.Fatalf("%s: err %v", name, err)
		}
	}
	// Failed items are not refetched before their retry time.
	before := calls.Load()
	for i := 0; i < 5; i++ {
		_, _ = l.get(ctx, proto.Media{URL: "http://x/bad"}, req)
	}
	time.Sleep(20 * time.Millisecond)
	if calls.Load() != before {
		t.Fatalf("failed media refetched %d times during backoff", calls.Load()-before)
	}
	items := []proto.Media{good, {URL: "http://x/bad"}, {URL: "http://x/new"}, good}
	l.setSpec(items, req)
	waitFor(t, "prefetch", func() bool { n, _, _ := l.progress(items); return n == 2 })
	loaded, total, errs := l.progress(items)
	if loaded != 2 || total != 3 || len(errs) != 1 || !strings.Contains(errs[0], "404") {
		t.Fatalf("progress %d/%d %q", loaded, total, errs)
	}
	// setSpec forgets media that left the spec.
	l.setSpec([]proto.Media{good}, req)
	if _, _, errs := l.progress([]proto.Media{{URL: "http://x/bad"}}); len(errs) != 0 {
		t.Fatalf("stale errors kept: %q", errs)
	}
	if _, err := l.get(ctx, good, FetchRequest{}); err == nil {
		t.Fatal("invalid request accepted")
	}
}

func TestMediaLoaderBackgroundDoesNotEvict(t *testing.T) {
	fetch := func(_ context.Context, m proto.Media, req FetchRequest) (image.Image, error) {
		return solid(req.PixelW, req.PixelH, colWhite), nil
	}
	req := screenReq(16, 16)            // 1 KiB per frame
	cache := NewFrameCache(2*1024 + 10) // room for two frames
	l := newMediaLoader(fetch, cache, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go l.run(ctx)
	var items []proto.Media
	for i := 0; i < 5; i++ {
		items = append(items, proto.Media{URL: "http://x/" + string(rune('a'+i))})
	}
	l.setSpec(items, req)
	waitFor(t, "prefetch", func() bool { n, _, _ := l.progress(items); return n == 5 })
	// The first two (shown first) stay cached; later ones were validated
	// but not allowed to evict them.
	if cache.Get(cacheKey(items[0], req)) == nil || cache.Get(cacheKey(items[1], req)) == nil {
		t.Fatal("prefetch evicted the first slides")
	}
	if cache.Len() != 2 {
		t.Fatalf("cache holds %d frames", cache.Len())
	}
}

func TestAsyncStats(t *testing.T) {
	var calls atomic.Int32
	var fail atomic.Bool
	notified := make(chan struct{}, 10)
	a := &asyncStats{ctx: context.Background(), notify: func() { notified <- struct{}{} },
		fetch: func(context.Context) (*proto.SwarmStats, error) {
			calls.Add(1)
			if fail.Load() {
				return nil, errors.New("hive down")
			}
			return &proto.SwarmStats{NodesOnline: 3}, nil
		}}
	if _, err := a.get(context.Background()); !errors.Is(err, errPending) {
		t.Fatalf("first get = %v", err)
	}
	<-notified
	st, err := a.get(context.Background())
	if err != nil || st.NodesOnline != 3 {
		t.Fatalf("stats %v %v", st, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("refreshed %d times within 5 s", calls.Load())
	}
	// After the refresh interval a failure keeps the last stats.
	fail.Store(true)
	a.mu.Lock()
	a.at = time.Now().Add(-time.Minute)
	a.mu.Unlock()
	_, _ = a.get(context.Background())
	<-notified
	st, err = a.get(context.Background())
	if st == nil || err == nil || st.NodesOnline != 3 {
		t.Fatalf("stale stats %v err %v", st, err)
	}
}

// TestMediaLoaderOversizedFrame: a frame larger than the whole cache is
// still served (no endless reloading).
func TestMediaLoaderOversizedFrame(t *testing.T) {
	var calls atomic.Int32
	fetch := func(_ context.Context, m proto.Media, req FetchRequest) (image.Image, error) {
		calls.Add(1)
		return solid(req.PixelW, req.PixelH, colWhite), nil
	}
	l := newMediaLoader(fetch, NewFrameCache(100), nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go l.run(ctx)
	m, req := proto.Media{URL: "http://x/big"}, screenReq(64, 64)
	waitFor(t, "load", func() bool { img, _ := l.get(ctx, m, req); return img != nil })
	for i := 0; i < 10; i++ {
		if img, err := l.get(ctx, m, req); img == nil || err != nil {
			t.Fatalf("get %d: %v", i, err)
		}
	}
	time.Sleep(20 * time.Millisecond)
	if calls.Load() != 1 {
		t.Fatalf("fetched %d times", calls.Load())
	}
}

package hive

import (
	"container/list"
	"net"
	"net/netip"
	"sync"
	"time"
)

// Rate limits (DESIGN 6.3).
const (
	limiterMaxEntries = 10000
	failLimit         = 5
	failWindow        = time.Minute
	failBlock         = 60 * time.Second
	helloRate         = 20.0 // requests per second per source
)

// Failure buckets are counted separately.
type bucket int

const (
	bucketRegister bucket = iota
	bucketAdmin
	bucketPair
	numBuckets
)

// lru is a size-capped map that evicts the least recently used entry.
type lru[V any] struct {
	max int
	m   map[string]*list.Element
	l   *list.List
}

type lruItem[V any] struct {
	key string
	val V
}

func newLRU[V any](max int) *lru[V] {
	return &lru[V]{max: max, m: map[string]*list.Element{}, l: list.New()}
}

func (c *lru[V]) get(key string) (V, bool) {
	if e, ok := c.m[key]; ok {
		c.l.MoveToFront(e)
		return e.Value.(*lruItem[V]).val, true
	}
	var zero V
	return zero, false
}

func (c *lru[V]) put(key string, v V) {
	if e, ok := c.m[key]; ok {
		e.Value.(*lruItem[V]).val = v
		c.l.MoveToFront(e)
		return
	}
	c.m[key] = c.l.PushFront(&lruItem[V]{key: key, val: v})
	for c.l.Len() > c.max {
		old := c.l.Back()
		c.l.Remove(old)
		delete(c.m, old.Value.(*lruItem[V]).key)
	}
}

func (c *lru[V]) len() int { return c.l.Len() }

type failState struct {
	fails        []time.Time
	blockedUntil time.Time
}

type tokenState struct {
	tokens float64
	last   time.Time
}

// limiter tracks failed authentication per source and the /hello rate.
type limiter struct {
	mu    sync.Mutex
	fails [numBuckets]*lru[*failState]
	hello *lru[*tokenState]
	now   func() time.Time
}

func newLimiter() *limiter {
	l := &limiter{hello: newLRU[*tokenState](limiterMaxEntries), now: time.Now}
	for i := range l.fails {
		l.fails[i] = newLRU[*failState](limiterMaxEntries)
	}
	return l
}

// sourceKey groups clients by IPv4 address or IPv6 /64.
func sourceKey(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	a = a.Unmap().WithZone("")
	if a.Is4() {
		return a.String()
	}
	p, err := a.Prefix(64)
	if err != nil {
		return a.String()
	}
	return p.String()
}

// blocked reports whether src is locked out of bucket b.
func (l *limiter) blocked(b bucket, src string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	st, ok := l.fails[b].get(src)
	return ok && l.now().Before(st.blockedUntil)
}

// fail records a failed attempt; the fifth within a minute blocks the
// source for 60 s.
func (l *limiter) fail(b bucket, src string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	st, ok := l.fails[b].get(src)
	if !ok {
		st = &failState{}
	}
	kept := st.fails[:0]
	for _, t := range st.fails {
		if now.Sub(t) < failWindow {
			kept = append(kept, t)
		}
	}
	st.fails = append(kept, now)
	if len(st.fails) >= failLimit {
		st.blockedUntil = now.Add(failBlock)
		st.fails = nil
	}
	l.fails[b].put(src, st)
}

// allowHello is a token bucket of helloRate per second per source.
func (l *limiter) allowHello(src string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	st, ok := l.hello.get(src)
	if !ok {
		st = &tokenState{tokens: helloRate, last: now}
		l.hello.put(src, st)
	}
	st.tokens += now.Sub(st.last).Seconds() * helloRate
	if st.tokens > helloRate {
		st.tokens = helloRate
	}
	st.last = now
	if st.tokens < 1 {
		return false
	}
	st.tokens--
	return true
}

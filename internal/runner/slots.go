package runner

import (
	"context"
	"sync"
)

// slotPool hands out concurrency slots; slot n runs as uid UIDBase+n. A
// slot whose processes could not be killed is retired so its uid is never
// shared with a later task.
type slotPool struct {
	mu      sync.Mutex
	used    []bool
	retired []bool
	freed   chan struct{} // closed and replaced whenever a slot frees up
}

func newSlotPool(n int) *slotPool {
	return &slotPool{used: make([]bool, n), retired: make([]bool, n), freed: make(chan struct{})}
}

// acquire returns a free slot, waiting until one frees up or ctx ends.
func (p *slotPool) acquire(ctx context.Context) (int, error) {
	for {
		p.mu.Lock()
		for i := range p.used {
			if !p.used[i] && !p.retired[i] {
				p.used[i] = true
				p.mu.Unlock()
				return i, nil
			}
		}
		ch := p.freed
		p.mu.Unlock()
		select {
		case <-ctx.Done():
			return -1, ctx.Err()
		case <-ch:
		}
	}
}

// release frees slot i; with retire it is never handed out again.
func (p *slotPool) release(i int, retire bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if i < 0 || i >= len(p.used) {
		return
	}
	p.used[i] = false
	if retire {
		p.retired[i] = true
		return
	}
	close(p.freed)
	p.freed = make(chan struct{})
}

func (p *slotPool) free() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for i := range p.used {
		if !p.used[i] && !p.retired[i] {
			n++
		}
	}
	return n
}

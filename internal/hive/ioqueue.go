package hive

import "sync"

// ioQueue runs file operations (log tails, deletions) sequentially outside
// the state mutex. It is unbounded so producers holding the mutex never
// block.
type ioQueue struct {
	mu      sync.Mutex
	cond    *sync.Cond
	jobs    []func()
	running bool
	closed  bool
	done    chan struct{}
}

func newIOQueue() *ioQueue {
	q := &ioQueue{done: make(chan struct{})}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func (q *ioQueue) push(f func()) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		go f() // late work after Close still completes
		return
	}
	q.jobs = append(q.jobs, f)
	q.cond.Broadcast()
}

func (q *ioQueue) run() {
	defer close(q.done)
	q.mu.Lock()
	for {
		for len(q.jobs) == 0 && !q.closed {
			q.cond.Wait()
		}
		if len(q.jobs) == 0 && q.closed {
			q.mu.Unlock()
			return
		}
		f := q.jobs[0]
		q.jobs[0] = nil
		q.jobs = q.jobs[1:]
		q.running = true
		q.mu.Unlock()
		f()
		q.mu.Lock()
		q.running = false
		q.cond.Broadcast()
	}
}

// flush waits until every queued job has run.
func (q *ioQueue) flush() {
	q.mu.Lock()
	defer q.mu.Unlock()
	for (len(q.jobs) > 0 || q.running) && !q.closed {
		q.cond.Wait()
	}
}

// close runs the remaining jobs and stops the worker.
func (q *ioQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.cond.Broadcast()
	q.mu.Unlock()
	<-q.done
}

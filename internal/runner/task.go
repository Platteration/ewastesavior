package runner

import (
	"sync"
	"time"

	"github.com/platteration/ewastesavior/internal/proto"
)

// runClock accumulates unfrozen process time on the monotonic clock.
type runClock struct {
	started bool
	paused  bool
	since   time.Time     // start of the current unpaused stretch
	total   time.Duration // closed stretches
}

func (c *runClock) start(now time.Time, paused bool) {
	c.started, c.paused, c.since = true, paused, now
}

func (c *runClock) pause(now time.Time) {
	if c.started && !c.paused {
		c.total += now.Sub(c.since)
		c.paused = true
	}
}

func (c *runClock) resume(now time.Time) {
	if c.started && c.paused {
		c.paused, c.since = false, now
	}
}

func (c *runClock) elapsed(now time.Time) time.Duration {
	if !c.started {
		return 0
	}
	if c.paused {
		return c.total
	}
	return c.total + now.Sub(c.since)
}

// procControl acts on a started task's processes.
type procControl interface {
	freeze(frozen bool) error
	kill()
}

// taskState is the live state of one Run call.
type taskState struct {
	id, lease string
	progress  func(proto.RunningTask)

	mu        sync.Mutex
	phase     string
	xfer      int64
	clk       runClock
	frozen    bool // requested by Freeze
	preempted bool
	proc      procControl   // non-nil while processes may exist
	wake      chan struct{} // poked on freeze/preempt changes
}

func newTaskState(id, lease string, progress func(proto.RunningTask)) *taskState {
	if progress == nil {
		progress = func(proto.RunningTask) {}
	}
	return &taskState{id: id, lease: lease, progress: progress, phase: proto.PhaseFetching, wake: make(chan struct{}, 1)}
}

func (t *taskState) poke() {
	select {
	case t.wake <- struct{}{}:
	default:
	}
}

func (t *taskState) snapshotLocked() proto.RunningTask {
	return proto.RunningTask{
		ID:        t.id,
		Lease:     t.lease,
		Phase:     t.phase,
		RunS:      t.clk.elapsed(time.Now()).Seconds(),
		XferBytes: t.xfer,
	}
}

func (t *taskState) snapshot() proto.RunningTask {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.snapshotLocked()
}

// report sends the current state to the progress callback (outside the lock).
func (t *taskState) report() { t.progress(t.snapshot()) }

func (t *taskState) setPhase(p string) {
	t.mu.Lock()
	t.phase = p
	t.mu.Unlock()
	t.report()
}

func (t *taskState) addXfer(n int64) {
	t.mu.Lock()
	t.xfer += n
	t.mu.Unlock()
}

func (t *taskState) runTime() time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.clk.elapsed(time.Now())
}

// freezeState reports whether the task is frozen and its unfrozen elapsed time.
func (t *taskState) freezeState() (bool, time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.frozen, t.clk.elapsed(time.Now())
}

func (t *taskState) isPreempted() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.preempted
}

// started records that the task process runs under pc. A freeze requested
// earlier is applied right away and the clock starts paused.
func (t *taskState) started(pc procControl) error {
	t.mu.Lock()
	t.proc = pc
	var err error
	paused := false
	if t.frozen {
		if err = pc.freeze(true); err == nil {
			paused = true
		}
	}
	t.clk.start(time.Now(), paused)
	if paused {
		t.phase = proto.PhaseFrozen
	} else {
		t.phase = proto.PhaseRunning
	}
	t.mu.Unlock()
	t.report()
	return err
}

// stopped stops the clock and forgets the process controls.
func (t *taskState) stopped() {
	t.mu.Lock()
	t.clk.pause(time.Now())
	t.proc = nil
	t.mu.Unlock()
}

func (t *taskState) setFrozen(frozen bool) error {
	t.mu.Lock()
	t.frozen = frozen
	var err error
	if t.proc != nil {
		// The clock only stops when the processes really stopped, so a
		// failed freeze can never extend the task past its timeout.
		if err = t.proc.freeze(frozen); err == nil {
			now := time.Now()
			if frozen {
				t.clk.pause(now)
				t.phase = proto.PhaseFrozen
			} else {
				t.clk.resume(now)
				t.phase = proto.PhaseRunning
			}
		}
	}
	t.mu.Unlock()
	t.poke()
	t.report()
	return err
}

func (t *taskState) preempt() {
	t.mu.Lock()
	t.preempted = true
	pc := t.proc
	t.mu.Unlock()
	if pc != nil {
		pc.kill()
	}
	t.poke()
}

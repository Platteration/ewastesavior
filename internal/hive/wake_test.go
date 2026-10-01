package hive

import (
	"testing"
	"time"

	"github.com/platteration/ewastesavior/internal/auth"
	"github.com/platteration/ewastesavior/internal/proto"
)

// A node whose power pause ends claims at once, while the hive still has
// its paused status; the heartbeat that reports it idle again must wake that
// claim instead of leaving it to sit out its long poll (DESIGN 9).
func TestPauseEndWakesClaim(t *testing.T) {
	t.Parallel()
	h := newHive(t, nil)
	n := h.newNode(nil)
	n.register()
	n.heartbeatStatus(proto.NodeStatus{State: proto.NodePaused, Reason: "on battery", Total: n.req.Total, Free: n.req.Total})
	job := h.submit(scriptJob(1, nil))

	type result struct {
		tasks []proto.Task
		took  time.Duration
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		tasks := n.claimWith(proto.ClaimRequest{ClaimID: auth.NewID(4), Free: n.req.Total, Max: 1, WaitS: 10})
		done <- result{tasks, time.Since(start)}
	}()
	eventually(t, "claim waiting", func() bool {
		h.s.mu.Lock()
		defer h.s.mu.Unlock()
		return h.s.nodes[n.req.NodeID].claims.Load() == 1
	})
	select {
	case r := <-done:
		t.Fatalf("a paused node's claim returned %d tasks", len(r.tasks))
	case <-time.After(200 * time.Millisecond):
	}
	n.heartbeat() // idle again
	select {
	case r := <-done:
		if len(r.tasks) != 1 || r.tasks[0].JobID != job.ID {
			t.Fatalf("claim after the pause: %+v", r.tasks)
		}
		if r.took > 3*time.Second {
			t.Fatalf("claim took %v: the pause ending didn't wake it", r.took)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("claim not woken when the node's pause ended")
	}
}

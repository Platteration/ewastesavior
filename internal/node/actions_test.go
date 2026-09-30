package node

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/platteration/ewastesavior/internal/proto"
)

// actionHive queues one-shot actions like the real hive: every heartbeat
// response repeats the actions the node has not acked yet.
type actionHive struct {
	mu     sync.Mutex
	queue  []proto.ActionDirective
	events []string // "heartbeat acked=[...]" and the test's own entries
}

func newActionHive(t *testing.T, queue ...proto.ActionDirective) (*actionHive, string) {
	h := &actionHive{queue: queue}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		var req proto.HeartbeatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		h.mu.Lock()
		h.queue = slices.DeleteFunc(h.queue, func(a proto.ActionDirective) bool { return slices.Contains(req.Status.AckedActions, a.ID) })
		h.events = append(h.events, fmt.Sprintf("heartbeat acked=%v", req.Status.AckedActions))
		resp := proto.HeartbeatResponse{Directives: proto.Directives{Actions: slices.Clone(h.queue)}}
		h.mu.Unlock()
		json.NewEncoder(w).Encode(resp)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return h, srv.URL
}

func (h *actionHive) log(e string) {
	h.mu.Lock()
	h.events = append(h.events, e)
	h.mu.Unlock()
}

func (h *actionHive) snapshot() (queue []proto.ActionDirective, events []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.queue), slices.Clone(h.events)
}

// A reboot requested by the hive is acked in a heartbeat before the
// machine goes down, and each action runs at most once per agent process
// however often it is delivered (SPEC-RUNTIME-09).
func TestRebootIsAckedFirstAndRunsOnce(t *testing.T) {
	identify := proto.ActionDirective{ID: "a1", Action: proto.ActionIdentify, Seconds: 5}
	reboot := proto.ActionDirective{ID: "a2", Action: proto.ActionReboot}
	h, url := newActionHive(t, identify, reboot)
	a := fakeAgent(nil, 0, nil)
	a.lastMetrics = proto.Metrics{Time: time.Now()} // no sampler in a fake agent
	a.hc = newHiveClient(url, "")
	ran := make(chan string, 4)
	a.opt.SystemAction = func(action string) error {
		queue, _ := h.snapshot()
		h.log(fmt.Sprintf("%s (hive still queues %d actions)", action, len(queue)))
		ran <- action
		return nil
	}

	a.applyDirectives(proto.Directives{Actions: []proto.ActionDirective{identify, reboot}})
	select {
	case got := <-ran:
		if got != proto.ActionReboot {
			t.Fatalf("system action %q", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("reboot never ran")
	}
	queue, events := h.snapshot()
	want := []string{"heartbeat acked=[a1 a2]", "reboot (hive still queues 0 actions)"}
	if !slices.Equal(events, want) || len(queue) != 0 {
		t.Fatalf("events %q (queue %v), want %q", events, queue, want)
	}
	a.mu.Lock()
	pending := slices.Clone(a.ackPending)
	a.mu.Unlock()
	if len(pending) != 0 {
		t.Errorf("acks still pending after the hive saw them: %v", pending)
	}

	// The same actions delivered again (a heartbeat response that crossed
	// the ack) are not run or acked again.
	a.applyDirectives(proto.Directives{Actions: []proto.ActionDirective{identify, reboot}})
	a.applyDirectives(proto.Directives{Actions: []proto.ActionDirective{reboot}})
	select {
	case got := <-ran:
		t.Fatalf("%s ran twice", got)
	case <-time.After(500 * time.Millisecond):
	}
	a.mu.Lock()
	pending = slices.Clone(a.ackPending)
	a.mu.Unlock()
	if len(pending) != 0 {
		t.Errorf("re-delivered actions acked again: %v", pending)
	}

	// Without ManageSystem or a SystemAction hook a reboot is acked but
	// nothing else happens.
	b := fakeAgent(nil, 0, nil)
	b.applyDirectives(proto.Directives{Actions: []proto.ActionDirective{{ID: "a3", Action: proto.ActionPoweroff}}})
	b.mu.Lock()
	defer b.mu.Unlock()
	if !slices.Equal(b.ackPending, []string{"a3"}) || !b.actionsDone["a3"] {
		t.Errorf("unmanaged poweroff: pending %v done %v", b.ackPending, b.actionsDone)
	}
}

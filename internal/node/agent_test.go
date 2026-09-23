package node

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/platteration/ewastesavior/internal/auth"
	"github.com/platteration/ewastesavior/internal/hwinfo"
	"github.com/platteration/ewastesavior/internal/power"
	"github.com/platteration/ewastesavior/internal/proto"
)

// These tests drive the agent's task bookkeeping with a fake runner and a
// fake hive; they need no Linux sandbox.

// fakeRunner stands in for *runner.Runner.
type fakeRunner struct {
	slots int
	// takeSlotAfter delays when a task takes its slot inside Run, like a
	// runner goroutine that has not reached the slot pool yet.
	takeSlotAfter time.Duration
	// release, when set, makes Run ignore cancellation until it is closed.
	release chan struct{}

	mu       sync.Mutex
	inSlot   int
	freezes  []string // "lease=true|false"
	preempts []string
}

func (f *fakeRunner) FreeSlots() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.slots - f.inSlot
}

func (f *fakeRunner) Run(ctx context.Context, t proto.Task, _ io.Writer, _ func(proto.RunningTask)) proto.TaskReport {
	canceled := proto.TaskReport{Lease: t.Lease, State: proto.TaskCanceled, Error: "canceled"}
	if f.release != nil {
		<-f.release
		return canceled
	}
	select {
	case <-time.After(f.takeSlotAfter):
	case <-ctx.Done():
		return canceled
	}
	f.mu.Lock()
	f.inSlot++
	f.mu.Unlock()
	<-ctx.Done()
	f.mu.Lock()
	f.inSlot--
	f.mu.Unlock()
	return canceled
}

func (f *fakeRunner) Freeze(lease string, frozen bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.freezes = append(f.freezes, fmt.Sprintf("%s=%v", lease, frozen))
	return nil
}

func (f *fakeRunner) Preempt(lease string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.preempts = append(f.preempts, lease)
	return nil
}

func (f *fakeRunner) calls() (freezes, preempts []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.freezes...), append([]string(nil), f.preempts...)
}

// lockedBuffer collects log output from several goroutines.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// fakeAgent returns an agent without hardware, display or hive session.
// r may be nil (no compute role).
func fakeAgent(r taskRunner, slots int, logs io.Writer) *Agent {
	if logs == nil {
		logs = io.Discard
	}
	return &Agent{
		log:         slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		link:        proto.LinkSearching,
		blacklist:   map[string]time.Time{},
		tasks:       map[string]*task{},
		actionsDone: map[string]bool{},
		wake:        make(chan struct{}, 1),
		hbInterval:  time.Second,
		runner:      r,
		runnerSlots: slots,
		total:       proto.Resources{Cores: 8, MemMB: 8192},
	}
}

// holdTask registers a task as held without running it.
func holdTask(a *Agent, lease string) *task {
	tk := &task{t: proto.Task{ID: "t-" + lease, Lease: lease}, cancel: func() {}, phase: proto.PhaseFetching, finished: make(chan struct{})}
	a.mu.Lock()
	a.tasks[lease] = tk
	a.mu.Unlock()
	return tk
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// claimHive hands out up to pool tasks through /claim and accepts reports.
type claimHive struct {
	url string

	mu        sync.Mutex
	pool      int
	handedOut int
	maxAsked  []int
}

func newClaimHive(t *testing.T, pool int) *claimHive {
	h := &claimHive{pool: pool}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/claim", func(w http.ResponseWriter, r *http.Request) {
		var req proto.ClaimRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		h.mu.Lock()
		h.maxAsked = append(h.maxAsked, req.Max)
		var resp proto.ClaimResponse
		for len(resp.Tasks) < req.Max && h.handedOut < h.pool {
			i := h.handedOut
			h.handedOut++
			resp.Tasks = append(resp.Tasks, proto.Task{ID: fmt.Sprintf("t%d", i), Lease: fmt.Sprintf("l%d", i), JobID: "j", Index: i,
				Resources: proto.Resources{Cores: 0.1, MemMB: 1}})
		}
		h.mu.Unlock()
		if len(resp.Tasks) == 0 {
			time.Sleep(20 * time.Millisecond) // a short long-poll
		}
		json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("POST /api/v1/tasks/{id}/report", func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	h.url = srv.URL
	return h
}

func (h *claimHive) claimed() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.handedOut
}

func TestClaimNeverExceedsRunnerSlots(t *testing.T) {
	// A claimed task takes its runner slot only when its goroutine gets
	// there; until then FreeSlots still counts the slot as free. The
	// agent must not claim more tasks than it can start.
	fr := &fakeRunner{slots: 2, takeSlotAfter: 300 * time.Millisecond}
	a := fakeAgent(fr, 2, nil)
	h := newClaimHive(t, 10)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.claimLoop(ctx, newHiveClient(h.url, ""))
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		a.shutdownTasks()
	})

	eventually(t, "two tasks claimed", func() bool { return h.claimed() >= 2 })
	time.Sleep(time.Second) // the tasks reach their slots; the loop polls on
	if n := h.claimed(); n != 2 {
		t.Fatalf("claimed %d tasks with 2 runner slots", n)
	}

	// A task that finished (its report still waiting to be sent) no longer
	// occupies a slot: exactly one more task is claimed.
	a.mu.Lock()
	var one *task
	for _, tk := range a.tasks {
		one = tk
		break
	}
	a.mu.Unlock()
	one.cancel()
	eventually(t, "a replacement task claimed", func() bool { return h.claimed() >= 3 })
	time.Sleep(500 * time.Millisecond)
	if n := h.claimed(); n != 3 {
		t.Fatalf("claimed %d tasks after one of 2 finished, want 3", n)
	}
}

func TestPowerPolicyIsLevelTriggered(t *testing.T) {
	// Tasks that arrive while the node is paused (hot) or preempting
	// (battery low) must be frozen or handed back too.
	fr := &fakeRunner{slots: 4}
	var logs lockedBuffer
	a := fakeAgent(fr, 4, &logs)
	idle := power.Decision{Accept: true}
	hot := power.Decision{Pause: true, Reason: "CPU too hot"}

	holdTask(a, "early")
	a.enforcePower(idle, hot)
	holdTask(a, "late") // claimed while the pause began
	a.enforcePower(hot, hot)
	freezes, _ := fr.calls()
	if !contains(freezes, "early=true") || !contains(freezes, "late=true") {
		t.Fatalf("freezes during a pause: %v", freezes)
	}
	a.enforcePower(hot, idle)
	freezes, _ = fr.calls()
	if !contains(freezes, "early=false") || !contains(freezes, "late=false") {
		t.Fatalf("thaw after a pause: %v", freezes)
	}

	low := power.Decision{Preempt: true, Reason: "battery below 40%"}
	a.enforcePower(idle, low)
	holdTask(a, "later")
	a.enforcePower(low, low)
	a.enforcePower(low, low)
	_, preempts := fr.calls()
	if !contains(preempts, "early") || !contains(preempts, "later") {
		t.Fatalf("preempts while on low battery: %v", preempts)
	}
	if n := strings.Count(logs.String(), "preempting task"); n != 3 {
		t.Errorf("logged %d preemptions for 3 tasks:\n%s", n, logs.String())
	}

	// Tasks with a final report are not touched.
	done := holdTask(a, "done")
	done.report = &proto.TaskReport{Lease: "done", State: proto.TaskSucceeded}
	a.enforcePower(low, low)
	a.enforcePower(idle, hot)
	freezes, preempts = fr.calls()
	if contains(freezes, "done=true") || contains(preempts, "done") {
		t.Errorf("finished task frozen or preempted: %v %v", freezes, preempts)
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// hangingHive accepts connections but never answers.
func hangingHive(t *testing.T) string {
	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-stop:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(stop) }) // runs before srv.Close
	return srv.URL
}

func TestShutdownDoesNotWaitForReports(t *testing.T) {
	// Canceled tasks must not first block on sending their report to an
	// unresponsive hive (up to a minute each) while the agent stops.
	fr := &fakeRunner{slots: 4}
	a := fakeAgent(fr, 4, nil)
	hc := newHiveClient(hangingHive(t), "")
	a.hc = hc
	for i := range 3 {
		a.startTask(context.Background(), hc, proto.Task{ID: fmt.Sprintf("t%d", i), Lease: fmt.Sprintf("l%d", i)})
	}
	start := time.Now()
	a.shutdownTasks()
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("shutdownTasks took %v", d)
	}
}

func TestShutdownGraceIsShared(t *testing.T) {
	// Tasks that don't stop get one shared grace period, not one each.
	old := shutdownGrace
	shutdownGrace = 400 * time.Millisecond
	t.Cleanup(func() { shutdownGrace = old })
	fr := &fakeRunner{slots: 5, release: make(chan struct{})}
	a := fakeAgent(fr, 5, nil)
	var tasks []*task
	for i := range 5 {
		a.startTask(context.Background(), nil, proto.Task{ID: fmt.Sprintf("t%d", i), Lease: fmt.Sprintf("l%d", i)})
		a.mu.Lock()
		tasks = append(tasks, a.tasks[fmt.Sprintf("l%d", i)])
		a.mu.Unlock()
	}
	start := time.Now()
	a.shutdownTasks()
	d := time.Since(start)
	close(fr.release)
	for _, tk := range tasks {
		<-tk.finished
	}
	if d < shutdownGrace || d > 3*shutdownGrace {
		t.Fatalf("shutdownTasks took %v with a %v grace for 5 stuck tasks", d, shutdownGrace)
	}
}

func TestRegisterRefusedIsNotUnreachable(t *testing.T) {
	// A hive that answers /register with 400 (for example an invalid
	// node_id) is reachable: say what it refused instead of "unreachable".
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/hello", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(proto.Hello{HiveID: "h", APIVersion: proto.APIVersion, Nonce: "nonce"})
	})
	mux.HandleFunc("POST /api/v1/register", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(proto.ErrorResponse{Error: "invalid node_id"})
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)

	a := fakeAgent(nil, 0, nil)
	a.opt.SysRoot = "../hwinfo/testdata/qemu"
	a.id = hwinfo.Identity{NodeID: "Lab-PC"}
	a.secret = auth.NewSwarmSecret("register-test-swarm-key-0123456789")
	hc, err := a.handshake(context.Background(), srv.URL)
	if err == nil || hc != nil {
		t.Fatalf("handshake succeeded: %v", err)
	}
	a.mu.Lock()
	link, detail := a.link, a.linkErr
	a.mu.Unlock()
	if link != proto.LinkRejected || !strings.Contains(detail, "invalid node_id") {
		t.Fatalf("link %q (%q), want %q with the hive's reason", link, detail, proto.LinkRejected)
	}
	if _, banned := a.blacklisted(srv.URL); !banned {
		t.Error("a hive that refuses our registration is retried at once")
	}
}

func TestUnreachableHiveIsRetriedSoon(t *testing.T) {
	// A hive that isn't listening yet (the hive machine's own node agent
	// starts first) must be retried within seconds, not minutes.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	base := "https://" + l.Addr().String()
	l.Close()

	a := fakeAgent(nil, 0, nil)
	a.opt.SysRoot = "../hwinfo/testdata/qemu"
	a.id = hwinfo.Identity{NodeID: "lab-pc"}
	a.secret = auth.NewSwarmSecret("unreachable-test-swarm-key-0123456789")
	var waits []time.Duration
	for range 7 {
		if _, err := a.handshake(context.Background(), base); err == nil {
			t.Fatal("handshake with a closed port succeeded")
		}
		until, banned := a.blacklisted(base)
		if !banned {
			t.Fatal("an unreachable hive is retried in a tight loop")
		}
		waits = append(waits, time.Until(until).Round(time.Second))
		a.mu.Lock()
		delete(a.blacklist, base) // as if the wait had passed
		a.mu.Unlock()
	}
	if a.link != proto.LinkUnreachable {
		t.Errorf("link %q, want %q", a.link, proto.LinkUnreachable)
	}
	want := []time.Duration{2, 4, 8, 16, 32, 60, 60}
	for i := range want {
		want[i] *= time.Second
	}
	if !slices.Equal(waits, want) {
		t.Errorf("waits %v, want %v", waits, want)
	}
	// A hive that answers wrongly is still skipped for the long time.
	a.ban(base)
	if until, _ := a.blacklisted(base); time.Until(until) < blacklistFor-time.Second {
		t.Errorf("ban lasts %v, want %v", time.Until(until), blacklistFor)
	}
}

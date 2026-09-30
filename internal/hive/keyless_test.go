package hive

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/platteration/ewastesavior/internal/auth"
	"github.com/platteration/ewastesavior/internal/config"
	"github.com/platteration/ewastesavior/internal/proto"
)

// A hive that doesn't netboot (and wasn't told keyless_join = yes) refuses
// every registration without a proof, before creating any record.
func TestKeylessJoinDisabledByDefault(t *testing.T) {
	t.Parallel()
	h := newHive(t, nil)
	legit := h.newNode(nil)
	legit.register()
	h.submit(scriptJob(1, func(s *proto.JobSpec) { s.Requirements.Isolation = proto.IsolationAny }))

	for _, n := range []*testNode{
		h.newNode(nil), // unknown hardware
		h.newNode(func(r *proto.RegisterRequest) { r.HWIDs = legit.req.HWIDs; r.BootID = "b-spoof" }),
	} {
		st, raw, _ := n.tryRegister(auth.Secret{}, "", true)
		if st != http.StatusForbidden || !strings.Contains(string(raw), "keyless joins are disabled") {
			t.Fatalf("keyless register on a hive without keyless joins: %d %s", st, raw)
		}
		if n.token != "" {
			t.Fatal("a refused keyless join got a token")
		}
		if code := h.admin("GET", "nodes/"+n.req.NodeID, nil, nil); code != http.StatusNotFound {
			t.Fatalf("refused keyless join left a record: %d", code)
		}
	}
	// The key-joined node is untouched.
	if v := h.nodeView(legit.req.NodeID); !v.Approved || v.Liveness != proto.NodeOnline {
		t.Fatalf("legit node: %+v", v)
	}
	if tasks := legit.claim(4); len(tasks) != 1 {
		t.Fatalf("legit node claim: %v", tasks)
	}
}

// A keyless client never modifies, approves itself through, or takes over
// a record that joined with the swarm key, online or offline.
func TestKeylessNeverUsesKeyJoinedRecord(t *testing.T) {
	t.Parallel()
	h := newHive(t, func(c *Config) { c.KeylessJoin = true; c.OfflineAfter = time.Second })
	legit := h.newNode(func(r *proto.RegisterRequest) { r.Name = "lab-1" })
	legit.register()
	h.mustAdmin("PATCH", "nodes/"+legit.req.NodeID, proto.NodePatch{Labels: &map[string]string{"room": "a"},
		Display: &proto.DisplaySpec{Mode: proto.DisplayClock}}, nil)
	h.submit(scriptJob(1, func(s *proto.JobSpec) {
		s.Requirements.Isolation = proto.IsolationAny
		s.Env = map[string]string{"SECRET": "hunter2"}
	}))

	check := func(when string) {
		t.Helper()
		// A new ID with the legit node's MAC: pending, no tasks, no stats.
		spoof := h.newNode(func(r *proto.RegisterRequest) { r.HWIDs = legit.req.HWIDs; r.BootID = "b-spoof-" + when })
		resp := spoof.registerKeyless()
		if !resp.Pending || resp.Name == "lab-1" || resp.Directives.Display != nil || resp.Directives.Labels["room"] != "" {
			t.Fatalf("%s: keyless join with a key-joined node's HWIDs: %+v", when, resp)
		}
		if tasks := spoof.claim(4); len(tasks) != 0 {
			t.Fatalf("%s: spoofing node got tasks: %+v", when, tasks)
		}
		if code, _ := spoof.api("GET", "stats", nil, nil); code != http.StatusForbidden {
			t.Fatalf("%s: spoofing node read stats: %d", when, code)
		}
		// The legit node's own ID: refused outright.
		same := h.newNode(func(r *proto.RegisterRequest) { *r = legit.req })
		st, raw, _ := same.tryRegister(auth.Secret{}, "", true)
		if st != http.StatusForbidden || !strings.Contains(string(raw), "joined with the swarm key") {
			t.Fatalf("%s: keyless join as a key-joined ID: %d %s", when, st, raw)
		}
		v := h.nodeView(legit.req.NodeID)
		if !v.Approved || v.Name != "lab-1" || v.AdminLabels["room"] != "a" || v.Display == nil {
			t.Fatalf("%s: key-joined record changed: %+v", when, v)
		}
	}
	check("online")
	if hb := legit.heartbeat(); hb.Directives.Pending {
		t.Fatal("legit node pending")
	}

	time.Sleep(1200 * time.Millisecond) // everything goes offline
	check("offline")
	// The legit node comes back with its key and finds its record intact.
	legit.req.BootID = "b-legit-2"
	resp := legit.register()
	if resp.Pending || resp.Name != "lab-1" || resp.Directives.Display == nil {
		t.Fatalf("legit node after keyless attempts: %+v", resp)
	}
	tasks := legit.claim(4)
	if len(tasks) != 1 || tasks[0].Env["SECRET"] != "hunter2" {
		t.Fatalf("legit claim: %+v", tasks)
	}
}

// A keyless machine whose ID changed (new NIC) takes over its old keyless
// record's settings by hardware ID, but waits for approval: only the same
// record is re-approved automatically.
func TestKeylessTakeoverStaysPending(t *testing.T) {
	t.Parallel()
	h := newHive(t, func(c *Config) { c.KeylessJoin = true; c.OfflineAfter = 300 * time.Millisecond })
	old := h.newNode(nil)
	old.registerKeyless()
	h.mustAdmin("PATCH", "nodes/"+old.req.NodeID, proto.NodePatch{Approved: ptr(true), Name: ptr("kiosk")}, nil)
	time.Sleep(400 * time.Millisecond)
	nw := h.newNode(func(r *proto.RegisterRequest) { r.HWIDs = append([]string{"uuid:5678"}, old.req.HWIDs...) })
	resp := nw.registerKeyless()
	if !resp.Pending || resp.Name != "kiosk" {
		t.Fatalf("keyless takeover: %+v", resp)
	}
	if code := h.admin("GET", "nodes/"+old.req.NodeID, nil, nil); code != http.StatusNotFound {
		t.Fatalf("old keyless record still present: %d", code)
	}
}

func TestConfigFromFileKeyless(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		netboot, keyless, want bool
	}{
		{false, false, false},
		{true, false, true}, // netbooted machines never get the key
		{false, true, true},
		{true, true, true},
	} {
		c := config.Default()
		c.Netboot, c.KeylessJoin = tc.netboot, tc.keyless
		if got := ConfigFromFile(c).KeylessJoin; got != tc.want {
			t.Errorf("netboot=%v keyless_join=%v: KeylessJoin=%v, want %v", tc.netboot, tc.keyless, got, tc.want)
		}
	}
	if c := config.Default(); c.KeylessJoin {
		t.Fatal("keyless_join must default to no")
	}
}

func TestHiveMemoryLimit(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		memMB int
		env   string
		want  int64
		set   bool
	}{
		{256, "", 64 << 20, true},
		{2048, "", 512 << 20, true},
		{128, "", 64 << 20, true}, // the floor
		{0, "", 0, false},         // unknown MemTotal
		{2048, "300MiB", 0, false},
	} {
		var got int64 = -1
		limit, ok := setMemoryLimit(tc.memMB, tc.env, func(v int64) int64 { got = v; return 0 })
		if ok != tc.set || limit != tc.want || (tc.set && got != tc.want) || (!tc.set && got != -1) {
			t.Errorf("%d MB, GOMEMLIMIT=%q: limit %d set %v (called with %d), want %d %v", tc.memMB, tc.env, limit, ok, got, tc.want, tc.set)
		}
	}
}

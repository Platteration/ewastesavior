package hive

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/platteration/ewastesavior/internal/config"
	"github.com/platteration/ewastesavior/internal/proto"
)

func TestSecurityHeaders(t *testing.T) {
	t.Parallel()
	h := newHive(t, nil)
	check := func(path, bearer string, wantCSP string) {
		t.Helper()
		req, _ := http.NewRequest("GET", h.url+path, nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := h.hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.Header.Get("X-Content-Type-Options") != "nosniff" || resp.Header.Get("Referrer-Policy") != "no-referrer" {
			t.Fatalf("%s: missing headers %v", path, resp.Header)
		}
		if wantCSP != "" && resp.Header.Get("Content-Security-Policy") != wantCSP {
			t.Fatalf("%s: CSP %q", path, resp.Header.Get("Content-Security-Policy"))
		}
		if resp.Header.Get("Access-Control-Allow-Origin") != "" {
			t.Fatalf("%s: CORS header present", path)
		}
	}
	check("/api/v1/hello", "", apiCSP)
	check("/api/v1/nope", "", apiCSP)
	check("/api/v1/admin/info", "", apiCSP) // 401
	check("/api/v1/admin/info", testAdmin, apiCSP)
	check("/", "", "")
	req, _ := http.NewRequest("GET", h.url+"/", nil)
	resp, err := h.hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Fatalf("dashboard CSP: %q", csp)
	}
	st, raw := do(t, h.hc, "GET", h.url+"/api/v1/nope", "", nil, nil)
	if st != http.StatusNotFound || !strings.Contains(string(raw), `"error"`) {
		t.Fatalf("unknown API path: %d %s", st, raw)
	}
}

func TestStats(t *testing.T) {
	t.Parallel()
	h := newHive(t, nil)
	n := h.newNode(func(r *proto.RegisterRequest) { r.Inventory.BenchScore = 50 })
	n.register()
	h.submit(scriptJob(3, nil))
	tk := n.claim(1)[0]
	n.succeed(tk)
	var st proto.SwarmStats
	if code, _ := n.api("GET", "stats", nil, &st); code != 200 {
		t.Fatalf("node stats: %d", code)
	}
	if st.NodesOnline != 1 || st.Cores != 4 || st.CoresAllocatable != 4 || st.BenchTotal != 50 || st.Displays != 1 ||
		st.JobsByState["running"] != 1 || st.TasksByState["succeeded"] != 1 || st.TasksByState["pending"] != 2 || st.TasksCompleted != 1 {
		t.Fatalf("stats: %+v", st)
	}
	if code, _ := do(t, h.hc, "GET", h.url+"/api/v1/stats", testAdmin, nil, &st); code != 200 || st.NodesOnline != 1 {
		t.Fatal("admin stats")
	}
}

func TestNodeConfig(t *testing.T) {
	t.Parallel()
	h := newHive(t, nil)
	get := func(q string) (int, string, http.Header) {
		r, err := adminGet(h, "node-config"+q)
		if err != nil {
			t.Fatal(err)
		}
		return r.code, string(r.body), r.hdr
	}
	code, body, hdr := get("?hive=192.168.1.20")
	if code != 200 || !strings.HasPrefix(hdr.Get("Content-Type"), "text/plain") || !strings.HasPrefix(body, "#") {
		t.Fatalf("node-config: %d %q", code, body)
	}
	parsed := config.Default()
	if w := config.ParseFile(strings.NewReader(body), "node-config", &parsed); len(w) != 0 {
		t.Fatalf("generated file has warnings: %v", w)
	}
	if parsed.SwarmKey != testKey || parsed.HiveFingerprint != h.s.Fingerprint() || parsed.Hive != "192.168.1.20:7700" {
		t.Fatalf("parsed: key %q fp %q hive %q", parsed.SwarmKey, parsed.HiveFingerprint, parsed.Hive)
	}
	// Without ?hive= the request's Host is used.
	_, body, _ = get("")
	host := strings.TrimPrefix(h.url, "https://")
	if !strings.Contains(body, "hive = "+host+"\n") {
		t.Fatalf("host fallback: %q", body)
	}
	for _, bad := range []string{"?hive=a%20b", "?hive=x%0Aswarm_key%3Devil", "?hive=https://h/path"} {
		if code, _, _ := get(bad); code != http.StatusBadRequest {
			t.Fatalf("%s: %d", bad, code)
		}
	}
	if st, _ := do(t, h.hc, "GET", h.url+"/api/v1/admin/node-config", "", nil, nil); st != http.StatusUnauthorized {
		t.Fatalf("anonymous node-config: %d", st)
	}
}

func TestHiveInfoAndClientTime(t *testing.T) {
	t.Parallel()
	var set atomic.Value
	h := newHive(t, func(c *Config) {
		c.tune.canSetClock = func() bool { return true }
		c.tune.setClock = func(t time.Time) error { set.Store(t); return nil }
	})
	h.s.mu.Lock()
	h.s.timeSynced, h.s.timeSource = false, proto.TimeRTC
	h.s.mu.Unlock()
	var info proto.HiveInfo
	h.mustAdmin("GET", "info", nil, &info)
	if info.HiveID != h.s.HiveID() || info.Fingerprint != h.s.Fingerprint() || info.SwarmHint != swarmSecret().SwarmHint() ||
		info.APIVersion != proto.APIVersion || info.Persistent != h.s.data.persistent || info.JoinPolicy != "open" || info.DataDir == "" || len(info.URLs) == 0 {
		t.Fatalf("info: %+v", info)
	}
	// A close client clock does not step ours.
	do(t, h.hc, "GET", h.url+"/api/v1/admin/info", testAdmin, nil, nil, ClientTimeHeader, time.Now().Add(10*time.Second).Format(time.RFC3339))
	if set.Load() != nil {
		t.Fatal("clock stepped for a small difference")
	}
	// Unauthenticated requests are ignored.
	future := time.Now().Add(48 * time.Hour).Truncate(time.Second)
	do(t, h.hc, "GET", h.url+"/api/v1/admin/info", "", nil, nil, ClientTimeHeader, future.Format(time.RFC3339))
	if set.Load() != nil {
		t.Fatal("anonymous request set the clock")
	}
	do(t, h.hc, "GET", h.url+"/api/v1/admin/info", testAdmin, nil, &info, ClientTimeHeader, future.Format(time.RFC3339))
	if got, _ := set.Load().(time.Time); !got.Equal(future) {
		t.Fatalf("clock not set: %v", got)
	}
	h.mustAdmin("GET", "info", nil, &info)
	if !info.TimeSynced || info.TimeSource != proto.TimeAdmin {
		t.Fatalf("time source: %+v", info)
	}
}

func TestHeartbeatDirectivesAndActions(t *testing.T) {
	t.Parallel()
	h := newHive(t, func(c *Config) { c.tune.actionExpiry = 300 * time.Millisecond })
	n := h.newNode(func(r *proto.RegisterRequest) { r.DisplayRotate = 270; r.Name = "sign" })
	resp := n.register()
	if resp.Directives.DisplayRotate != 270 || resp.Directives.Display != nil {
		t.Fatalf("initial directives: %+v", resp.Directives)
	}
	var v proto.NodeView
	h.mustAdmin("PATCH", "nodes/sign", proto.NodePatch{DisplayRotate: ptr(90), Drain: ptr(true),
		Display: &proto.DisplaySpec{Mode: proto.DisplayText, Text: "Welcome", Rev: 999999999999999}}, &v)
	hb := n.heartbeat()
	d := hb.Directives
	if d.DisplayRotate != 90 || !d.Drain || d.Display == nil || d.Display.Text != "Welcome" || d.Display.Rev == 999999999999999 || d.Display.Rev == 0 {
		t.Fatalf("directives: %+v", d)
	}
	prevRev := d.Display.Rev
	h.mustAdmin("PATCH", "nodes/sign", proto.NodePatch{Display: &proto.DisplaySpec{Mode: proto.DisplayClock}}, nil)
	if d := n.heartbeat().Directives; d.Display.Rev <= prevRev || d.Display.Mode != proto.DisplayClock {
		t.Fatalf("rev not bumped: %+v", d.Display)
	}
	if st := h.admin("PATCH", "nodes/sign", proto.NodePatch{Display: &proto.DisplaySpec{Mode: "disco"}}, nil); st != http.StatusBadRequest {
		t.Fatalf("invalid display: %d", st)
	}
	if st := h.admin("PATCH", "nodes/sign", proto.NodePatch{DisplayRotate: ptr(45)}, nil); st != http.StatusBadRequest {
		t.Fatalf("invalid rotation: %d", st)
	}
	// Actions repeat until acknowledged.
	h.mustAdmin("POST", "nodes/sign/action", proto.NodeAction{Action: proto.ActionIdentify, Seconds: 12}, nil)
	h.mustAdmin("POST", "nodes/sign/action", proto.NodeAction{Action: proto.ActionReboot}, nil)
	var ids []string
	for i := 0; i < 2; i++ {
		acts := n.heartbeat().Directives.Actions
		if len(acts) != 2 || acts[0].Action != proto.ActionIdentify || acts[0].Seconds != 12 || acts[1].Action != proto.ActionReboot {
			t.Fatalf("actions: %+v", acts)
		}
		ids = []string{acts[0].ID, acts[1].ID}
	}
	st := proto.NodeStatus{State: proto.NodeIdle, AckedActions: []string{ids[0]}}
	if acts := n.heartbeatStatus(st).Directives.Actions; len(acts) != 1 || acts[0].ID != ids[1] {
		t.Fatalf("after ack: %+v", acts)
	}
	if st := h.admin("POST", "nodes/sign/action", proto.NodeAction{Action: "selfdestruct"}, nil); st != http.StatusBadRequest {
		t.Fatalf("bad action: %d", st)
	}
	// Unacknowledged actions expire.
	time.Sleep(350 * time.Millisecond)
	if acts := n.heartbeat().Directives.Actions; len(acts) != 0 {
		t.Fatalf("expired actions still sent: %+v", acts)
	}
	// Identify all online nodes, with defaults and caps.
	h.mustAdmin("POST", "identify", proto.IdentifyRequest{}, nil)
	if acts := n.heartbeat().Directives.Actions; len(acts) != 1 || acts[0].Seconds != identifyDefault {
		t.Fatalf("identify all: %+v", acts)
	}
	if st := h.admin("POST", "identify", proto.IdentifyRequest{Nodes: []string{"ghost"}}, nil); st != http.StatusNotFound {
		t.Fatalf("identify unknown: %d", st)
	}
}

func TestNodeDelete(t *testing.T) {
	t.Parallel()
	h := newHive(t, func(c *Config) { c.OfflineAfter = 150 * time.Millisecond })
	n := h.newNode(nil)
	n.register()
	if st := h.admin("DELETE", "nodes/"+n.req.NodeID, nil, nil); st != http.StatusConflict {
		t.Fatalf("delete online: %d", st)
	}
	time.Sleep(200 * time.Millisecond)
	h.mustAdmin("DELETE", "nodes/"+n.req.NodeID, nil, nil)
	if st := h.admin("GET", "nodes/"+n.req.NodeID, nil, nil); st != http.StatusNotFound {
		t.Fatalf("deleted node: %d", st)
	}
	var list []proto.NodeView
	h.mustAdmin("GET", "nodes", nil, &list)
	if len(list) != 0 {
		t.Fatalf("list: %+v", list)
	}
}
